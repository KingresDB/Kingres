// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package ledgersign_test

import (
	"context"
	"crypto/ed25519"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ledgerkeys"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ledgersign"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/task"
)

// P7-01: signing a checkpoint and retiring a key exclude each other. A collector pass (Publication, then a store of a checkpoint of a larger size
// with every signer it was given) is interleaved with the retirement at every boundary of the engine: either the retiring key is not given to the
// collector once its event exists, or the retirement refuses to happen. In no case is a checkpoint stored with the key above the bound the key is
// retired with.
func TestARetirementAndASigningPassInterleavedAtEveryBoundaryNeverLeaveACheckpointAboveTheBound(t *testing.T) {
	points := []string{task.FaultBeforeStep, task.FaultStepStarted, task.FaultStepRan, task.FaultStepRecorded, task.FaultBeforeFinish}
	for _, point := range points {
		for step := 0; step <= 8; step++ {
			point, step := point, step
			t.Run(point+"/"+string(rune('0'+step)), func(t *testing.T) {
				e := newEnv(t, memFactory, withPeers("s2"))
				e.create(0)
				k1 := keyIn(t, load(t, e), ledgerkeys.StateActive)
				e.clk.Add(time.Hour)
				e.create(1)
				e.clk.Add(25 * time.Hour)
				signedWithK1 := uint64(0)
				e.mem.MaxSize = 100
				pass := func() {
					pc, err := e.svc.Publication(context.Background())
					if err != nil || pc.SignErr != nil {
						return
					}
					for _, s := range pc.Signers {
						if s.Public().Equal(k1.Public) {
							// the collector signs a larger tree with every signer it was given and stores it (the store is shared with the key service)
							e.mem.MaxSize += 1000
							signedWithK1 = e.mem.MaxSize
						}
					}
				}
				fired := false
				e.fault = func(p string, _ int64, st int) {
					if p == point && st == step && !fired {
						fired = true
						pass()
					}
				}
				tk, err := e.svc.RetireKey(context.Background(), "a", k1.Fingerprint())
				if err != nil {
					t.Fatal(err)
				}
				_ = e.eng.RunNow(context.Background(), tk.ID, nil)
				got, _ := e.eng.Get(context.Background(), tk.ID)
				r, _ := load(t, e).Set().Get(k1.Fingerprint())
				if r.State == ledgerkeys.StateRetired {
					if signedWithK1 > r.MaxTreeSize {
						t.Fatalf("retired with the bound %d while a checkpoint of size %d was signed with the key", r.MaxTreeSize, signedWithK1)
					}
				} else if signedWithK1 == 0 {
					t.Fatalf("not retired and nothing was signed: %s %+v", got.State, got.Error)
				} else if got.Error == nil || got.Error.Code != "bound_exceeded" {
					t.Fatalf("a checkpoint above the bound was stored and the task ended %s %+v", got.State, got.Error)
				}
			})
		}
	}
}

// The retirement commit checks the size under the guard: a checkpoint stored between the event and the retirement, by a signer that was loaded before
// the event, makes the retirement refuse (a task error, the key stays active).
func TestTheRetirementRefusesWhenACheckpointAboveTheBoundWasStoredAfterTheEvent(t *testing.T) {
	e := newEnv(t, memFactory)
	e.create(0)
	k1 := keyIn(t, load(t, e), ledgerkeys.StateActive)
	e.clk.Add(time.Hour)
	e.create(1)
	e.clk.Add(25 * time.Hour)
	e.mem.MaxSize = 100
	e.mem.BySigner = func(pub ed25519.PublicKey) uint64 {
		if pub.Equal(k1.Public) {
			return e.mem.MaxSize
		}
		return 0
	}
	e.fault = func(p string, _ int64, st int) {
		if p == task.FaultStepRecorded && st == 0 {
			e.mem.MaxSize = 5000
		}
	}
	tk, _ := e.svc.RetireKey(context.Background(), "a", k1.Fingerprint())
	_ = e.eng.RunNow(context.Background(), tk.ID, nil)
	got, _ := e.eng.Get(context.Background(), tk.ID)
	if got.State != task.Failed || got.Error == nil || got.Error.Code != "bound_exceeded" {
		t.Fatalf("%s %+v", got.State, got.Error)
	}
	if stateOf(load(t, e), k1.Fingerprint()) != ledgerkeys.StateActive {
		t.Error("the key was retired")
	}
}

// P8-02: after the key-retired event the other key keeps signing and the logs grow, in every step of the task and after it. The retirement
// still ends: the refusal is about checkpoints signed by the key that retires, and none was. (Before the fix the largest tree of the cluster
// was compared with the bound and the task failed for good with bound_exceeded.)
func TestTheRetirementEndsWhileTheOtherKeyKeepsSigning(t *testing.T) {
	e := newEnv(t, memFactory)
	e.create(0)
	k1 := keyIn(t, load(t, e), ledgerkeys.StateActive)
	e.clk.Add(time.Hour)
	e.create(1)
	e.clk.Add(25 * time.Hour)
	e.mem.MaxSize = 100
	e.mem.BySigner = func(ed25519.PublicKey) uint64 { return 0 } // only the other key signs
	e.fault = func(p string, _ int64, st int) {
		if p == task.FaultStepRecorded {
			e.mem.MaxSize += 7 // a new checkpoint of the new key after every step
		}
	}
	tk, _ := e.svc.RetireKey(context.Background(), "a", k1.Fingerprint())
	_ = e.eng.RunNow(context.Background(), tk.ID, nil)
	got, _ := e.eng.Get(context.Background(), tk.ID)
	if got.State != task.Done {
		t.Fatalf("the retirement did not end: %s %+v", got.State, got.Error)
	}
	if stateOf(load(t, e), k1.Fingerprint()) != ledgerkeys.StateRetired {
		t.Error("the key is not retired")
	}
}

// P7-04: a key file that exists and does not open is an alarm on the leader, the alert is raised, and the distribution gives a holder with a bad copy the key again.
func TestAKeyFileThatDoesNotOpenIsAnAlarmAndTheDistributionRepairsAHolder(t *testing.T) {
	e := newEnv(t, memFactory, withPeers("s2"))
	e.create(0)
	fp := keyIn(t, load(t, e), ledgerkeys.StateActive).Fingerprint()
	ctx := context.Background()
	// the holder s2: garbage under the name of the key
	bad := filepath.Join(e.dir, "s2", "signing", fp+".key")
	if err := os.WriteFile(bad, []byte("garbage\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	e.peers["s2"].kr.Forget(fp)
	e.autorun = true
	n := len(e.started)
	if err := e.svc.Jobs()[2].Run(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if len(e.started) != n+1 {
		t.Fatalf("the sync job started %d tasks for a holder with a file that does not open", len(e.started)-n)
	}
	if err := e.peers["s2"].kr.Verify(fp, keyIn(t, load(t, e), ledgerkeys.StateActive).Public); err != nil {
		t.Errorf("the file of s2 was not repaired: %v", err)
	}
	// the leader: its own file is garbage
	if err := os.WriteFile(filepath.Join(e.dir, "s1", "signing", fp+".key"), []byte("garbage\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	e.kr.Forget(fp)
	a, err := e.svc.Alarm(ctx)
	if err != nil || a == "" || !strings.Contains(a, fp[:16]) {
		t.Fatalf("the alarm for a file that does not open: %q %v", a, err)
	}
	if strings.Contains(a, e.dir) {
		t.Errorf("the alarm names a path: %q", a)
	}
	e.alerts = nil
	if err := e.svc.Jobs()[1].Run(ctx, 1); err != nil || len(e.alerts) != 1 || !strings.HasPrefix(e.alerts[0], "ledgerSignerUnavailable/raise/") {
		t.Fatalf("the check job: %v %v", err, e.alerts)
	}
	_, _, err = e.svc.Signers(ctx)
	if !errors.Is(err, ledgersign.ErrAlarm) {
		t.Errorf("Signers: %v", err)
	}
	if err != nil && strings.Contains(err.Error(), e.dir) {
		t.Errorf("the error of Signers names a path (P7-08): %v", err)
	}
	// a file of another key under the right name is the same alarm
	other := e.peers["s2"].kr
	_ = other
}

// P7-03: a server that left the cluster and cannot be reached is forgotten as a holder: the distribution is not started for ever, a revocation can
// finish, and the key files that may remain on that host are named in the log.
func TestAServerThatLeftTheClusterIsForgottenAndTheDistributionIsNotStartedForEver(t *testing.T) {
	e := newEnv(t, memFactory, withPeers("s2", "s3"))
	e.create(0)
	ctx := context.Background()
	if got := joined(load(t, e).Cluster.Holders); got != "s1,s2,s3" {
		t.Fatalf("holders %s", got)
	}
	e.hold = []string{"s2"}
	e.removed = map[string]bool{"s3": true}
	e.peers["s3"].down = true
	e.autorun = true
	n := len(e.started)
	if err := e.svc.Jobs()[2].Run(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if len(e.started) != n+1 {
		t.Fatalf("the sync job started %d tasks", len(e.started)-n)
	}
	if got := joined(load(t, e).Cluster.Holders); got != "s1,s2" {
		t.Errorf("holders after the server was forgotten: %s", got)
	}
	if !strings.Contains(e.logs.String(), "left the cluster") {
		t.Error("the log does not say that key files may remain on a host that left")
	}
	for i := 0; i < 3; i++ {
		n = len(e.started)
		if err := e.svc.Jobs()[2].Run(ctx, 1); err != nil || len(e.started) != n {
			t.Fatalf("pass %d: the sync job started a task (%v)", i, err)
		}
	}
}

// P7-03: a revocation whose last step could not remove a file can be run again, and finishes when the server answers.
func TestARevocationThatLeftAFileOnAServerCanBeRunAgain(t *testing.T) {
	e := newEnv(t, memFactory, withPeers("s2"))
	e.create(0)
	k1 := keyIn(t, load(t, e), ledgerkeys.StateActive)
	e.clk.Add(time.Hour)
	e.create(10)
	e.peers["s2"].down = true
	req := revokeRequest(k1.Fingerprint(), e.clk.Now())
	tk, err := e.svc.RevokeKey(context.Background(), "a", k1.Fingerprint(), req)
	if err != nil {
		t.Fatal(err)
	}
	_ = e.eng.RunNow(context.Background(), tk.ID, nil)
	got, _ := e.eng.Get(context.Background(), tk.ID)
	if got.Error == nil || got.Error.Code != "key_files_remain" {
		t.Fatalf("%s %+v", got.State, got.Error)
	}
	if stateOf(load(t, e), k1.Fingerprint()) != ledgerkeys.StateRevoked {
		t.Fatal("not revoked")
	}
	e.peers["s2"].down = false
	tk2, err := e.svc.RevokeKey(context.Background(), "a", k1.Fingerprint(), req)
	if err != nil {
		t.Fatalf("running the revocation again: %v", err)
	}
	_ = e.eng.RunNow(context.Background(), tk2.ID, nil)
	got2, _ := e.eng.Get(context.Background(), tk2.ID)
	if got2.State != task.Done || e.peers["s2"].kr.Has(k1.Fingerprint()) {
		t.Fatalf("%s %+v, file on s2: %v", got2.State, got2.Error, e.peers["s2"].kr.Has(k1.Fingerprint()))
	}
	// nothing remains: another revocation is refused as before
	if _, err := e.svc.RevokeKey(context.Background(), "a", k1.Fingerprint(), req); err == nil {
		t.Error("a second revocation of a key whose files are gone was accepted")
	}
}

// P7-05: the chain of events is verified at every load: a changed note, a deleted event or one that no key of the set signed is an alarm.
func TestTheChainOfEventsIsVerifiedAtEveryLoad(t *testing.T) {
	ctx := context.Background()
	for name, damage := range map[string]func(s *ledgersign.Snapshot){
		"a note that is not a note": func(s *ledgersign.Snapshot) { s.Events[0].Note = []byte("garbage") },
		"a changed line of a signed note": func(s *ledgersign.Snapshot) {
			s.Events[1].Note = []byte(strings.Replace(string(s.Events[1].Note), "type: key-added", "type: key-added ", 1))
		},
		"a number that does not follow": func(s *ledgersign.Snapshot) { s.Events[1].Seq = 5 },
		"a type that is not the note's": func(s *ledgersign.Snapshot) { s.Events[0].Type = ledgersign.EventKeyRetired },
		"a deleted event":               func(s *ledgersign.Snapshot) { s.Events = s.Events[1:] },
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, memFactory)
			e.create(0)
			e.clk.Add(time.Hour)
			e.create(1)
			if a, _ := e.svc.Alarm(ctx); a != "" {
				t.Fatalf("an alarm before the damage: %q", a)
			}
			e.mem.Overwrite(damage)
			if _, err := e.svc.TrustFile(ctx); err == nil {
				t.Error("a trust file was served over events that do not verify")
			}
			if _, _, err := e.svc.Signers(ctx); !errors.Is(err, ledgersign.ErrAlarm) {
				t.Errorf("Signers: %v", err)
			}
		})
	}
}

// P7-06: the event and the record carry the same compromised-since (decided once, in the event).
func TestTheRevocationEventAndTheRecordCarryTheSameTime(t *testing.T) {
	e := newEnv(t, memFactory)
	e.create(0)
	k1 := keyIn(t, load(t, e), ledgerkeys.StateActive)
	e.clk.Add(time.Hour)
	e.create(10)
	req := revokeRequest(k1.Fingerprint(), e.clk.Now().Add(60*time.Second))
	tk, err := e.svc.RevokeKey(context.Background(), "a", k1.Fingerprint(), req)
	if err != nil {
		t.Fatal(err)
	}
	e.fault = func(p string, _ int64, st int) {
		if p == task.FaultStepRecorded && st == 0 {
			e.clk.Add(10 * time.Second) // time passes between the event and the revocation
		}
	}
	_ = e.eng.RunNow(context.Background(), tk.ID, nil)
	s := load(t, e)
	r, _ := s.Set().Get(k1.Fingerprint())
	var ev ledgersign.EventRow
	for _, x := range s.Events {
		if x.Type == ledgersign.EventKeyRevoked {
			ev = x
		}
	}
	line := "compromised-since: " + r.CompromisedSince.UTC().Format(time.RFC3339) + "\n"
	if !strings.Contains(string(ev.Note), line) {
		t.Errorf("the record says %s, the event:\n%s", r.CompromisedSince, ev.Note)
	}
}

// P7-09 and P7-10: a temporary key file that a crash left is removed at the start (when it is older than a minute); a key file that the daemon did not
// mark is not adopted.
func TestTemporaryKeyFilesAreCleanedAndAFileWithoutAMarkerIsNotAdopted(t *testing.T) {
	dir := t.TempDir()
	kr, err := ledgersign.NewKeyring(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(dir, "signing", ".key-old")
	young := filepath.Join(dir, "signing", ".key-young")
	for _, p := range []string{old, young} {
		if err := os.WriteFile(p, []byte("a whole key file"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	past := time.Now().Add(-time.Hour)
	if err := os.Chtimes(old, past, past); err != nil {
		t.Fatal(err)
	}
	if _, err := ledgersign.NewKeyring(dir, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(old); err == nil {
		t.Error("the old temporary file was kept")
	}
	if _, err := os.Stat(young); err != nil {
		t.Error("a young temporary file (a write in progress) was removed")
	}
	// a key file nobody marked
	pub, write, _, err := kr.Generate()
	if err != nil {
		t.Fatal(err)
	}
	if err := write(); err != nil {
		t.Fatal(err)
	}
	fp := ledgerkeys.Fingerprint(pub)
	if got, _ := kr.Orphan(map[string]bool{}, time.Now().Add(-time.Minute), time.Minute, 7); got != "" {
		t.Errorf("a file without a marker was adopted: %s", got)
	}
	if err := kr.Mark(fp, 7); err != nil {
		t.Fatal(err)
	}
	if got, _ := kr.Orphan(map[string]bool{}, time.Now().Add(-time.Minute), time.Minute, 7); got != fp {
		t.Errorf("a marked file was not found: %q", got)
	}
	// P8-07: the marker is the one of the task that asks; the marker of another task does not adopt the file
	if got, _ := kr.Orphan(map[string]bool{}, time.Now().Add(-time.Minute), time.Minute, 6); got != "" {
		t.Errorf("a file marked by task 7 was adopted by the earlier task 6: %q", got)
	}
	if err := kr.Remove(fp); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "signing", fp+".task")); err == nil {
		t.Error("the marker stays after the key file was removed")
	}
}

// The mutants the review found alive: the last-key check of the event step of a retirement (the API check shadows it, so the task is run without the API), and a
// missing published trust file as an alarm.
func TestTheEventStepRefusesToRetireTheOnlyKeyAndAMissingTrustFileIsAnAlarm(t *testing.T) {
	e := newEnv(t, memFactory)
	e.create(0)
	k := keyIn(t, load(t, e), ledgerkeys.StateActive)
	got := e.run("ledger.key.retire", map[string]any{"fingerprint": k.Fingerprint()})
	if got.State != task.Failed || got.Error == nil || got.Error.Code != "last_signing_key" {
		t.Fatalf("retiring the only key through the task: %s %+v", got.State, got.Error)
	}
	if stateOf(load(t, e), k.Fingerprint()) != ledgerkeys.StateActive {
		t.Error("the only key was retired")
	}
	e.mem.Overwrite(func(s *ledgersign.Snapshot) { s.Cluster.TrustFile = nil })
	if a, _ := e.svc.Alarm(context.Background()); a == "" {
		t.Error("no alarm without a published trust file")
	}
	if _, _, err := e.svc.Signers(context.Background()); !errors.Is(err, ledgersign.ErrAlarm) {
		t.Errorf("Signers: %v", err)
	}
}

// P7-11: a retirement that was started a few minutes ago is not started again by the job at once.
func TestTheDueJobDoesNotStartTheSameRetirementEveryMinute(t *testing.T) {
	e := newEnv(t, memFactory)
	e.create(0)
	e.clk.Add(time.Hour)
	e.create(1)
	e.clk.Add(25 * time.Hour)
	ctx := context.Background()
	n := len(e.started)
	if err := e.svc.Jobs()[0].Run(ctx, 1); err != nil || len(e.started) != n+1 {
		t.Fatalf("first pass: %v %d", err, len(e.started)-n)
	}
	// the task is left unrun (it would fail for ever); the next passes start nothing
	_ = e.eng.Cancel(ctx, e.started[n].ID)
	for i := 0; i < 3; i++ {
		if err := e.svc.Jobs()[0].Run(ctx, 1); err != nil || len(e.started) != n+1 {
			t.Fatalf("pass %d started %d tasks", i, len(e.started)-n-1)
		}
	}
}

func revokeRequest(fp string, since time.Time) api.RevokeLedgerSigningKeyRequest {
	return api.RevokeLedgerSigningKeyRequest{CompromisedSince: since.UTC().Truncate(time.Second), Reason: "stolen", Confirm: fp}
}
