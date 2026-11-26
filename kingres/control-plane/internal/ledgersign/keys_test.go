// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package ledgersign_test

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ledgerkeys"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ledgerlog"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ledgersign"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/scheduler"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/task"
)

// memFactory makes the in-memory store; the database store has its own factory (pgstore_test.go).
func memFactory(t *testing.T) (ledgersign.Store, func(context.Context) (scheduler.Term, error), *ledgersign.MemStore) {
	m := ledgersign.NewMemStore()
	return m, m.BumpTerm, m
}

func load(t *testing.T, e *env) ledgersign.Snapshot {
	t.Helper()
	s, err := e.store.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func stateOf(s ledgersign.Snapshot, fp string) ledgerkeys.State {
	for _, k := range s.Keys {
		if k.Fingerprint() == fp {
			return k.State
		}
	}
	return 0
}

func keyIn(t *testing.T, s ledgersign.Snapshot, state ledgerkeys.State) ledgerkeys.Key {
	t.Helper()
	for _, k := range s.Keys {
		if k.State == state {
			return k.Key
		}
	}
	t.Fatalf("no key in state %s: %d keys", state, len(s.Keys))
	return ledgerkeys.Key{}
}

func verifyEvent(t *testing.T, prefix string, note []byte, wantTypes string, pubs ...ed25519.PublicKey) *ledgerlog.Note {
	t.Helper()
	n, err := ledgerlog.ParseNote(note)
	if err != nil {
		t.Fatalf("the event is not a signed note: %v", err)
	}
	var keys []ledgerlog.NoteKey
	for _, p := range pubs {
		keys = append(keys, ledgerlog.NoteKey{Name: prefix, Public: p})
	}
	ok, err := ledgerlog.VerifyNote(n, keys)
	if err != nil {
		t.Fatalf("the event does not verify: %v", err)
	}
	if len(ok) != len(pubs) {
		t.Errorf("the event is signed by %d of the %d keys that must sign it", len(ok), len(pubs))
	}
	if !strings.Contains(string(n.Text), "type: "+wantTypes+"\n") {
		t.Errorf("the event is not a %s:\n%s", wantTypes, n.Text)
	}
	return n
}

func TestTheFirstKeyIsMadeDistributedAnnouncedAndActivated(t *testing.T) {
	e := newEnv(t, memFactory, withPeers("s2"))
	tk := e.create(0)
	if len(tk.Steps) != 4 {
		t.Errorf("the creation has %d steps", len(tk.Steps))
	}
	s := load(t, e)
	k := keyIn(t, s, ledgerkeys.StateActive)
	fp := k.Fingerprint()
	if len(s.Keys) != 1 || k.ActivatedAt.IsZero() || !k.ActivatedAt.Equal(t0) {
		t.Fatalf("the set: %+v", s.Keys)
	}
	if len(s.Cluster.ClusterID) != 32 || s.Cluster.OriginPrefix != "s1/kingres-ledger" || !s.Cluster.BackupAdvised {
		t.Errorf("cluster row: id %d bytes, prefix %q, backup advised %v", len(s.Cluster.ClusterID), s.Cluster.OriginPrefix, s.Cluster.BackupAdvised)
	}
	// The trust file is the one the library writes for the set, and it is stored.
	want, err := ledgerkeys.TrustFile(s.Cluster.OriginPrefix, s.Set(), ledgerkeys.TrustOptions{})
	if err != nil || string(want) != string(s.Cluster.TrustFile) {
		t.Fatalf("the stored trust file is not the one built from the set: %v", err)
	}
	got, err := e.svc.TrustFile(context.Background())
	if err != nil || string(got) != string(want) {
		t.Fatalf("TrustFile: %v", err)
	}
	// One event, signed by the key itself under the cluster origin, in the format of 3.2.4.
	if len(s.Events) != 1 || s.Events[0].Seq != 0 || s.Events[0].Type != "key-added" || s.Events[0].Subject != fp {
		t.Fatalf("events: %+v", s.Events)
	}
	n := verifyEvent(t, s.Cluster.OriginPrefix, s.Events[0].Note, "key-added", k.Public)
	text := string(n.Text)
	for _, line := range []string{"kingres-ledger-event/1\n", "seq: 0\n", "time: " + t0.Format(time.RFC3339) + "\n", "key: " + base64.StdEncoding.EncodeToString(k.Public) + "\n", "fingerprint: " + fp + "\n", "not-before: " + t0.Format(time.RFC3339) + "\n"} {
		if !strings.Contains(text, line) {
			t.Errorf("the event lacks %q:\n%s", line, text)
		}
	}
	// The key file is on the leader and on the holder; both are named holders.
	if !e.kr.Has(fp) || !e.peers["s2"].kr.Has(fp) {
		t.Error("the key file is not on the leader and the holder")
	}
	if joined(s.Cluster.Holders) != "s1,s2" {
		t.Errorf("holders %v", s.Cluster.Holders)
	}
	// The row of the key has the task that made it and no secret.
	if s.Keys[0].TaskID != tk.ID || s.Keys[0].Mode != "file" {
		t.Errorf("key row %+v", s.Keys[0])
	}
	sig := e.signing()
	if sig.ClusterID == nil || len(*sig.ClusterID) != 64 || sig.Mode != "file" || len(sig.Keys) != 1 || sig.Keys[0].State != "active" || len(sig.Events) != 1 || joined(sig.Holders) != "s1,s2" || sig.SignerAlarm != nil {
		t.Errorf("Signing: %+v", sig)
	}
	signers, _, err := e.svc.Signers(context.Background())
	if err != nil || len(signers) != 1 {
		t.Fatalf("Signers: %v %d", err, len(signers))
	}
}

func TestARotationOverlapsTheKeysAndTheOverlapEndsByAJobThatRetiresTheOlderOne(t *testing.T) {
	e := newEnv(t, memFactory, withPeers("s2"))
	e.create(0)
	k1 := keyIn(t, load(t, e), ledgerkeys.StateActive)
	e.clk.Add(time.Hour)
	e.create(10)
	s := load(t, e)
	active := 0
	var k2 ledgerkeys.Key
	for _, k := range s.Keys {
		if k.State == ledgerkeys.StateActive {
			active++
			if k.Fingerprint() != k1.Fingerprint() {
				k2 = k.Key
			}
		}
	}
	if active != 2 {
		t.Fatalf("%d keys active after the rotation", active)
	}
	got1, _ := s.Set().Get(k1.Fingerprint())
	if want := k2.ActivatedAt.Add(10 * 24 * time.Hour); !got1.OverlapEnds.Equal(want) {
		t.Errorf("the overlap of the older key ends %v, want %v", got1.OverlapEnds, want)
	}
	// The key-added event of the second key is signed by both keys: the first authorises, the second proves possession.
	var ev ledgersign.EventRow
	for _, x := range s.Events {
		if x.Subject == k2.Fingerprint() {
			ev = x
		}
	}
	verifyEvent(t, s.Cluster.OriginPrefix, ev.Note, "key-added", k1.Public, k2.Public)
	if ev.Seq != 1 {
		t.Errorf("the second event has the number %d", ev.Seq)
	}
	// Both keys sign while the overlap lasts, and the trust file lists both.
	signers, _, err := e.svc.Signers(context.Background())
	if err != nil || len(signers) != 2 {
		t.Fatalf("Signers during the overlap: %v %d", err, len(signers))
	}
	e.autorun = true
	nStarted := len(e.started)
	if err := e.svc.Jobs()[0].Run(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	if len(e.started) != nStarted {
		t.Fatalf("the due job started %d tasks before the overlap ended", len(e.started)-nStarted)
	}
	e.clk.Add(10*24*time.Hour + time.Minute)
	if err := e.svc.Jobs()[0].Run(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	if len(e.started) != nStarted+1 || e.started[nStarted].Kind != "ledger.key.retire" {
		t.Fatalf("the due job started %+v", e.started[nStarted:])
	}
	if len(e.audits) != 1 || e.audits[0].Actor != "system" || e.audits[0].Action != "retireLedgerSigningKey" {
		t.Errorf("audit of the automatic retirement: %+v", e.audits)
	}
	s = load(t, e)
	if stateOf(s, k1.Fingerprint()) != ledgerkeys.StateRetired || stateOf(s, k2.Fingerprint()) != ledgerkeys.StateActive {
		t.Fatalf("states after the retirement: %s %s", stateOf(s, k1.Fingerprint()), stateOf(s, k2.Fingerprint()))
	}
	r, _ := s.Set().Get(k1.Fingerprint())
	if r.MaxTreeSize != 1 {
		t.Errorf("the retired key that signed nothing is bound to %d", r.MaxTreeSize)
	}
	// The retired key's file is gone from every server, the second key's file stays.
	if e.kr.Has(k1.Fingerprint()) || e.peers["s2"].kr.Has(k1.Fingerprint()) || !e.kr.Has(k2.Fingerprint()) || !e.peers["s2"].kr.Has(k2.Fingerprint()) {
		t.Error("the key files after the retirement are not where they belong")
	}
	last := s.Events[len(s.Events)-1]
	n := verifyEvent(t, s.Cluster.OriginPrefix, last.Note, "key-retired", k2.Public)
	if !strings.Contains(string(n.Text), "max-tree-size: 1\n") {
		t.Errorf("the key-retired event: %s", n.Text)
	}
	if e.resigned != 2 {
		t.Errorf("the checkpoints were signed again %d times (rotation and retirement, once each)", e.resigned)
	}
	want, _ := ledgerkeys.TrustFile(s.Cluster.OriginPrefix, s.Set(), ledgerkeys.TrustOptions{})
	if string(want) != string(s.Cluster.TrustFile) || !strings.Contains(string(want), `"max_tree_size": 1`) {
		t.Errorf("the trust file after the retirement:\n%s", s.Cluster.TrustFile)
	}
}

func TestARetiredKeyIsBoundByTheLargestCheckpointItCouldHaveSigned(t *testing.T) {
	e := newEnv(t, memFactory)
	e.create(0)
	e.create(1)
	e.mem.MaxSize = 4711
	e.clk.Add(25 * time.Hour)
	k1 := load(t, e).Keys[0]
	e.mustDone(e.run("ledger.key.retire", map[string]any{"fingerprint": k1.Fingerprint()}))
	r, _ := load(t, e).Set().Get(k1.Fingerprint())
	if r.MaxTreeSize != 4711 {
		t.Errorf("bound %d, want 4711", r.MaxTreeSize)
	}
}

func TestRevocationIsSignedByAnotherKeyAndRemovesTheFileEverywhere(t *testing.T) {
	e := newEnv(t, memFactory, withPeers("s2"))
	e.create(0)
	k1 := keyIn(t, load(t, e), ledgerkeys.StateActive)
	e.clk.Add(time.Hour)
	e.create(0)
	s := load(t, e)
	var k2 ledgerkeys.Key
	for _, k := range s.Keys {
		if k.Fingerprint() != k1.Fingerprint() {
			k2 = k.Key
		}
	}
	since := e.clk.Now().Add(-48 * time.Hour) // before the key existed: the record clamps it to the creation of the key
	tk, err := e.svc.RevokeKey(context.Background(), "tester", k1.Fingerprint(), api.RevokeLedgerSigningKeyRequest{CompromisedSince: since, Reason: "laptop lost", Confirm: k1.Fingerprint()})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.eng.RunNow(context.Background(), tk.ID, nil); err != nil {
		t.Fatal(err)
	}
	got, _ := e.eng.Get(context.Background(), tk.ID)
	e.mustDone(got)
	s = load(t, e)
	r, _ := s.Set().Get(k1.Fingerprint())
	if r.State != ledgerkeys.StateRevoked || !r.CompromisedSince.Equal(k1.CreatedAt) || r.Reason != "laptop lost" || !s.Cluster.BackupAdvised {
		t.Fatalf("revoked key: %+v", r)
	}
	last := s.Events[len(s.Events)-1]
	n := verifyEvent(t, s.Cluster.OriginPrefix, last.Note, "key-revoked", k2.Public)
	for _, line := range []string{"key: " + base64.StdEncoding.EncodeToString(k1.Public) + "\n", "compromised-since: " + t0.Format(time.RFC3339) + "\n", "reason: laptop lost\n"} {
		if !strings.Contains(string(n.Text), line) {
			t.Errorf("the revoke event lacks %q:\n%s", line, n.Text)
		}
	}
	if e.kr.Has(k1.Fingerprint()) || e.peers["s2"].kr.Has(k1.Fingerprint()) {
		t.Error("the file of the revoked key remains")
	}
	if !strings.Contains(string(s.Cluster.TrustFile), `"revoked": true`) {
		t.Errorf("the trust file does not mark the key revoked:\n%s", s.Cluster.TrustFile)
	}
	signers, _, err := e.svc.Signers(context.Background())
	if err != nil || len(signers) != 1 {
		t.Fatalf("Signers after the revocation: %v %d", err, len(signers))
	}
}

func problemCode(err error) string {
	var p *problem.Problem
	if errors.As(err, &p) {
		return string(p.Code)
	}
	return ""
}

func TestOperationsRefuseWhatTheContractSays(t *testing.T) {
	e := newEnv(t, memFactory)
	ctx := context.Background()
	if _, err := e.svc.TrustFile(ctx); problemCode(err) != "signing_key_missing" {
		t.Errorf("the trust file before any key: %v", err)
	}
	if _, err := e.svc.RetireKey(ctx, "a", strings.Repeat("0", 64)); problemCode(err) != "not_found" {
		t.Errorf("retiring an unknown key: %v", err)
	}
	if _, err := e.svc.RetireKey(ctx, "a", "not-a-fingerprint"); problemCode(err) != "not_found" {
		t.Errorf("retiring a malformed fingerprint: %v", err)
	}
	pub := "AAAA"
	if _, err := e.svc.CreateKey(ctx, "a", api.CreateLedgerSigningKeyRequest{PublicKey: &pub}); problemCode(err) != "unsupported_operation" {
		t.Errorf("a public key in mode file: %v", err)
	}
	for _, d := range []int{0 - 5, 366} {
		d := d
		if _, err := e.svc.CreateKey(ctx, "a", api.CreateLedgerSigningKeyRequest{OverlapDays: &d}); problemCode(err) != "validation_failed" {
			t.Errorf("overlapDays %d: %v", d, err)
		}
	}
	e.create(0)
	k1 := keyIn(t, load(t, e), ledgerkeys.StateActive)
	fp := k1.Fingerprint()
	// The last key can be neither retired nor revoked.
	if _, err := e.svc.RetireKey(ctx, "a", fp); problemCode(err) != "last_signing_key" {
		t.Errorf("retiring the last key: %v", err)
	}
	rev := api.RevokeLedgerSigningKeyRequest{CompromisedSince: e.clk.Now(), Reason: "x", Confirm: fp}
	if _, err := e.svc.RevokeKey(ctx, "a", fp, rev); problemCode(err) != "last_signing_key" {
		t.Errorf("revoking the last key: %v", err)
	}
	// The confirmation, the reason and the time are validated before anything else.
	bad := rev
	bad.Confirm = "nope"
	if _, err := e.svc.RevokeKey(ctx, "a", fp, bad); problemCode(err) != "validation_failed" {
		t.Errorf("a wrong confirmation: %v", err)
	}
	bad = rev
	bad.Reason = strings.Repeat("x", 201)
	if _, err := e.svc.RevokeKey(ctx, "a", fp, bad); problemCode(err) != "validation_failed" {
		t.Errorf("a long reason: %v", err)
	}
	bad = rev
	bad.Reason = "caf\u00e9"
	if _, err := e.svc.RevokeKey(ctx, "a", fp, bad); problemCode(err) != "validation_failed" {
		t.Errorf("a reason that is not ASCII: %v", err)
	}
	// A task of the same subject at a time.
	e.autorun = false
	tk, err := e.svc.CreateKey(ctx, "a", api.CreateLedgerSigningKeyRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.CreateKey(ctx, "a", api.CreateLedgerSigningKeyRequest{}); problemCode(err) != "operation_in_progress" {
		t.Errorf("a second key task: %v", err)
	}
	if err := e.eng.RunNow(ctx, tk.ID, nil); err != nil {
		t.Fatal(err)
	}
	// Two keys are active now: no third.
	if _, err := e.svc.CreateKey(ctx, "a", api.CreateLedgerSigningKeyRequest{}); problemCode(err) != "conflict" {
		t.Errorf("a rotation during an overlap: %v", err)
	}
	// An already retired or revoked key is a conflict, an unknown one is not found.
	if _, err := e.svc.RetireKey(ctx, "a", fp); err != nil {
		t.Errorf("retiring one of two active keys: %v", err)
	}
}

func TestAKeyWithNoOtherActiveKeyCannotAuthoriseARotation(t *testing.T) {
	e := newEnv(t, memFactory)
	e.create(0)
	e.create(1)
	e.clk.Add(25 * time.Hour)
	s := load(t, e)
	e.mustDone(e.run("ledger.key.retire", map[string]any{"fingerprint": s.Keys[0].Fingerprint()}))
	// Revoke the only active key through the store, as damage: the set that has been in service and
	// holds no active key is the alarm of a damaged set, so a creation is refused.
	e.mem.Overwrite(func(sn *ledgersign.Snapshot) {
		for i := range sn.Keys {
			if sn.Keys[i].State == ledgerkeys.StateActive {
				sn.Keys[i].State = ledgerkeys.StateRetired
				sn.Keys[i].RetiredAt = e.clk.Now()
			}
		}
	})
	if _, err := e.svc.CreateKey(context.Background(), "a", api.CreateLedgerSigningKeyRequest{}); problemCode(err) != "conflict" {
		t.Errorf("a creation on a damaged set: %v", err)
	}
}

func TestEveryStepIsIdempotent(t *testing.T) {
	e := newEnv(t, memFactory, withPeers("s2"))
	tk := e.create(0)
	e.clk.Add(time.Hour)
	tk2 := e.create(1)
	e.clk.Add(25 * time.Hour)
	k1 := load(t, e).Keys[0]
	rt := e.run("ledger.key.retire", map[string]any{"fingerprint": k1.Fingerprint()})
	e.mustDone(rt)
	before := e.mem.Commits
	snap := load(t, e)
	// Every task of the engine is run again from its first step with its own row: nothing may change.
	for _, kind := range []string{"ledger.key.create", "ledger.key.rotate"} {
		id := tk.ID
		if kind == "ledger.key.rotate" {
			id = tk2.ID
		}
		def := findDef(t, e.svc, kind)
		for i, st := range def.Steps {
			env := stepEnv(t, e, id)
			if err := st.Run(context.Background(), env); err != nil && !isSkip(err) {
				t.Errorf("%s step %d (%s) run again: %v", kind, i, st.Name, err)
			}
		}
	}
	def := findDef(t, e.svc, "ledger.key.retire")
	for i, st := range def.Steps {
		if err := st.Run(context.Background(), stepEnv(t, e, rt.ID)); err != nil && !isSkip(err) {
			t.Errorf("retire step %d (%s) run again: %v", i, st.Name, err)
		}
	}
	if e.mem.Commits != before {
		t.Errorf("%d changes were written by steps that were done already", e.mem.Commits-before)
	}
	after := load(t, e)
	if len(after.Events) != len(snap.Events) || len(after.Keys) != len(snap.Keys) || string(after.Cluster.TrustFile) != string(snap.Cluster.TrustFile) {
		t.Error("the state changed")
	}
}

func isSkip(err error) bool { var s *task.SkipError; return errors.As(err, &s) }

func findDef(t *testing.T, s *ledgersign.Service, kind string) task.Definition {
	t.Helper()
	for _, d := range s.Definitions() {
		if d.Kind == kind {
			return d
		}
	}
	t.Fatalf("no definition %s", kind)
	return task.Definition{}
}

// stepEnv is an Env for calling a step directly with the row of a task that ran.
func stepEnv(t *testing.T, e *env, id int64) *task.Env {
	t.Helper()
	tk, err := e.eng.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return task.EnvFor(tk)
}

func TestTheSigningViewAndTheTrustFileNeverCarryASecret(t *testing.T) {
	for _, pass := range []string{"", "correct horse battery staple"} {
		name := "plain"
		if pass != "" {
			name = "sealed"
		}
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, memFactory, withPeers("s2"), withPassphrase(pass))
			e.create(0)
			e.clk.Add(time.Hour)
			e.create(0)
			s := load(t, e)
			e.remember()
			k1 := s.Keys[0]
			e.clk.Add(time.Hour)
			if _, err := e.svc.RevokeKey(context.Background(), "a", k1.Fingerprint(), api.RevokeLedgerSigningKeyRequest{CompromisedSince: e.clk.Now(), Reason: "leak test", Confirm: k1.Fingerprint()}); err != nil {
				t.Fatal(err)
			}
			if pass != "" {
				if !e.signing().PassphraseProtected {
					t.Error("sealed key files are not reported as protected")
				}
			} else if e.signing().PassphraseProtected {
				t.Error("plain key files are reported as protected")
			}
			// Everything that leaves the process or the key directory.
			j, _ := json.Marshal(e.signing())
			e.noSecret("the signing view", j)
			tf, _ := e.svc.TrustFile(context.Background())
			e.noSecret("the trust file", tf)
			snap, _ := json.Marshal(load(t, e))
			e.noSecret("the stored state", snap)
			for _, tk := range e.tasks.Export() {
				b, _ := json.Marshal(tk)
				e.noSecret("a task row", b)
			}
			e.noSecret("the log", []byte(e.logs.String()))
			for _, a := range e.alerts {
				e.noSecret("an alert", []byte(a))
			}
			for _, a := range e.audits {
				b, _ := json.Marshal(a)
				e.noSecret("an audit entry", b)
			}
			for _, kr := range e.peers {
				_ = kr
			}
		})
	}
}

func TestAStoredSetThatDoesNotMatchThePublishedTrustFileIsAnAlarmAndNothingSigns(t *testing.T) {
	cases := map[string]func(sn *ledgersign.Snapshot){
		"a bound set to zero":         func(sn *ledgersign.Snapshot) { sn.Keys[0].MaxTreeSize = 0 },
		"the activation time shifted": func(sn *ledgersign.Snapshot) { sn.Keys[1].ActivatedAt = sn.Keys[1].ActivatedAt.Add(-time.Hour) },
		"a label changed":             func(sn *ledgersign.Snapshot) { sn.Keys[1].Label = "evil" },
		"no trust file":               func(sn *ledgersign.Snapshot) { sn.Cluster.TrustFile = nil },
		"a changed trust file": func(sn *ledgersign.Snapshot) {
			sn.Cluster.TrustFile = append([]byte(nil), sn.Cluster.TrustFile[:len(sn.Cluster.TrustFile)-2]...)
		},
		"the prefix changed":              func(sn *ledgersign.Snapshot) { sn.Cluster.OriginPrefix = "other/prefix" },
		"a marker that belongs to no key": func(sn *ledgersign.Snapshot) { sn.Keys[1].OverlapEnds = sn.Keys[1].ActivatedAt.Add(24 * time.Hour) },
		"a reason on a key not revoked":   func(sn *ledgersign.Snapshot) { sn.Keys[1].Reason = "left over" },
		"a time in the wrong order":       func(sn *ledgersign.Snapshot) { sn.Keys[1].ActivatedAt = sn.Keys[1].CreatedAt.Add(-time.Hour) },
	}
	for name, damage := range cases {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, memFactory)
			e.create(0)
			e.clk.Add(time.Hour)
			e.create(1)
			e.clk.Add(25 * time.Hour)
			e.mustDone(e.run("ledger.key.retire", map[string]any{"fingerprint": load(t, e).Keys[0].Fingerprint()}))
			e.mem.Overwrite(damage)
			ctx := context.Background()
			if _, _, err := e.svc.Signers(ctx); !errors.Is(err, ledgersign.ErrAlarm) {
				t.Fatalf("Signers on a damaged store: %v", err)
			}
			if a, _ := e.svc.Alarm(ctx); a == "" {
				t.Error("no alarm text")
			}
			sig := e.signing()
			if sig.SignerAlarm == nil || *sig.SignerAlarm == "" {
				t.Error("getLedgerSigning does not say that the signer is in alarm")
			}
			if _, err := e.svc.TrustFile(ctx); err == nil {
				t.Error("a trust file is served for a key set that does not match it")
			}
			e.alerts = nil
			if err := e.svc.Jobs()[1].Run(ctx, 1); err != nil || len(e.alerts) != 1 || !strings.HasPrefix(e.alerts[0], "ledgerSignerUnavailable/raise/") {
				t.Errorf("the check job: %v %v", err, e.alerts)
			}
			// A key task cannot start from it and a step cannot go on.
			if _, err := e.svc.CreateKey(ctx, "a", api.CreateLedgerSigningKeyRequest{}); problemCode(err) != "conflict" {
				t.Errorf("a creation: %v", err)
			}
		})
	}
}

func TestASetThatHasBeenInServiceAndHoldsNoActiveKeyIsTheAlarmOfADamagedSet(t *testing.T) {
	e := newEnv(t, memFactory)
	e.create(0)
	e.mem.Overwrite(func(sn *ledgersign.Snapshot) {
		sn.Keys[0].State = ledgerkeys.StateRetired
		sn.Keys[0].RetiredAt = sn.Keys[0].ActivatedAt
	})
	_, _, err := e.svc.Signers(context.Background())
	if !errors.Is(err, ledgersign.ErrAlarm) || !errors.Is(err, ledgerkeys.ErrNoActiveKey) {
		t.Errorf("a set with no active key: %v", err)
	}
}

func TestAKeyThatIsNotInForceYetMeansDoNotSignAndIsNotTheAlarmOfADamagedSet(t *testing.T) {
	e := newEnv(t, memFactory)
	e.create(0)
	// The clock steps back by an hour after the activation: the key is active in the record and not in force.
	e.clk.Add(-time.Hour)
	e.mem.Now = e.clk.Now
	_, _, err := e.svc.Signers(context.Background())
	if !errors.Is(err, ledgersign.ErrDoNotSign) || errors.Is(err, ledgersign.ErrAlarm) || errors.Is(err, ledgerkeys.ErrNoActiveKey) {
		t.Errorf("Signers with the clock behind the activation: %v", err)
	}
	if a, _ := e.svc.Alarm(context.Background()); a != "" {
		t.Errorf("a clock behind an activation raised the alarm %q", a)
	}
	e.clk.Add(time.Hour)
	if _, _, err := e.svc.Signers(context.Background()); err != nil {
		t.Errorf("Signers once the time has come: %v", err)
	}
}

func TestATimeComesFromAClockThatAgreesWithTheDatabase(t *testing.T) {
	e := newEnv(t, memFactory)
	// The database is an hour ahead of this server: nothing is recorded and the answer says why.
	e.mem.Now = func() time.Time { return e.clk.Now().Add(time.Hour) }
	tk, err := e.svc.CreateKey(context.Background(), "a", api.CreateLedgerSigningKeyRequest{})
	if err != nil {
		t.Fatal(err)
	}
	_ = e.eng.RunNow(context.Background(), tk.ID, nil)
	got, _ := e.eng.Get(context.Background(), tk.ID)
	if got.State != task.Failed || got.Error == nil || got.Error.Code != "clock" || !got.Error.Retryable {
		t.Fatalf("the creation with a skewed clock: %s %+v", got.State, got.Error)
	}
	if s := load(t, e); len(s.Keys) != 0 || len(s.Events) != 0 {
		t.Error("something was recorded with a clock that disagrees with the database")
	}
	if _, _, err := e.svc.Signers(context.Background()); err == nil {
		t.Error("Signers with no keys")
	}
	e.mem.Now = e.clk.Now
	e.create(0)
}

func TestAnIdleSetIsBroughtForwardInStepsAndNeverRecordsATimeBeyondTheLimit(t *testing.T) {
	e := newEnv(t, memFactory)
	e.create(0)
	k1 := keyIn(t, load(t, e), ledgerkeys.StateActive)
	// Two years later the clock is past the limit of the set (366 days after its latest record): the rotation records the
	// limit, not the clock.
	e.clk.Add(2 * 366 * 24 * time.Hour)
	e.create(0)
	s := load(t, e)
	lim := t0.Add(366 * 24 * time.Hour)
	for _, k := range s.Keys {
		if k.Fingerprint() == k1.Fingerprint() {
			continue
		}
		if k.CreatedAt.After(lim) || k.ActivatedAt.Before(lim) || k.ActivatedAt.After(e.clk.Now()) {
			t.Errorf("the new key was created %v and activated %v with a limit of %v and a clock of %v", k.CreatedAt, k.ActivatedAt, lim, e.clk.Now())
		}
	}
	if err := s.Set().Validate(); err != nil {
		t.Errorf("the set after the catch-up: %v", err)
	}
}

func TestADeposedLeaderCannotWriteAKeyEventAndANewLeaderCan(t *testing.T) {
	e := newEnv(t, memFactory)
	e.create(0)
	before := load(t, e)
	// Another server took over: the stored term moved on, this one still believes it holds its own.
	e.mem.SetTerm(99)
	tk, err := e.svc.CreateKey(context.Background(), "a", api.CreateLedgerSigningKeyRequest{})
	if err != nil {
		t.Fatal(err)
	}
	_ = e.eng.RunNow(context.Background(), tk.ID, nil)
	got, _ := e.eng.Get(context.Background(), tk.ID)
	if got.State != task.Failed || got.Error == nil || got.Error.Code != "term_over" {
		t.Fatalf("a task of a deposed leader: %s %+v", got.State, got.Error)
	}
	after := load(t, e)
	if len(after.Keys) != len(before.Keys) || len(after.Events) != len(before.Events) || after.Revision != before.Revision {
		t.Errorf("a deposed leader changed the store: %d keys, %d events, revision %d to %d", len(after.Keys), len(after.Events), before.Revision, after.Revision)
	}
	// Settings are written under the same guard.
	on := true
	if _, err := e.svc.UpdateSettings(context.Background(), api.LedgerSettingsUpdate{Publication: &api.LedgerPublicationUpdate{Enabled: &on}}); err == nil {
		t.Error("a deposed leader changed the settings")
	}
	// A later term works again.
	e.becomeLeader()
	e.create(0)
}

func TestAFollowerNeverSignsAndTheLeaderOnlyAfterItHoldsATerm(t *testing.T) {
	e := newEnv(t, memFactory)
	e.create(0)
	e.stopLeading()
	if _, _, err := e.svc.Signers(context.Background()); !errors.Is(err, ledgersign.ErrNotLeader) {
		t.Errorf("Signers without a term: %v", err)
	}
	if _, err := e.svc.CreateKey(context.Background(), "a", api.CreateLedgerSigningKeyRequest{}); problemCode(err) != "service_unavailable" {
		t.Errorf("a creation on a server without a term: %v", err)
	}
	if _, err := e.svc.UpdateSettings(context.Background(), api.LedgerSettingsUpdate{}); problemCode(err) != "service_unavailable" {
		t.Errorf("settings on a server without a term: %v", err)
	}
	e.becomeLeader()
	if _, _, err := e.svc.Signers(context.Background()); err != nil {
		t.Errorf("Signers with a term: %v", err)
	}
}

func TestSettingsAreValidatedAndThePrefixIsLockedOnceAKeyExists(t *testing.T) {
	e := newEnv(t, memFactory)
	ctx := context.Background()
	s0, err := e.svc.Settings(ctx)
	if err != nil || s0.Publication.OriginPrefix != "s1/kingres-ledger" || s0.Publication.OriginPrefixLocked || s0.Publication.Enabled || s0.Publication.IntervalSeconds != 60 || s0.Anchor.IntervalSeconds != 3600 || s0.Anchor.MinimumAnchored != 1 {
		t.Fatalf("defaults: %+v %v", s0, err)
	}
	iv, bad, on, url := 120, 5, true, "https://ledger.example.com/kingres"
	var u api.LedgerPublicationUpdate
	u.PublicURL.Set, u.PublicURL.Value = true, &url
	pref := "ledger.example.com/kingres"
	u.Enabled, u.IntervalSeconds, u.OriginPrefix = &on, &iv, &pref
	got, err := e.svc.UpdateSettings(ctx, api.LedgerSettingsUpdate{Publication: &u, Anchor: &api.LedgerAnchorUpdate{MinimumAnchored: &iv}})
	if problemCode(err) != "validation_failed" {
		t.Fatalf("anchor.minimumAnchored 120: %v", err)
	}
	m := 3
	got, err = e.svc.UpdateSettings(ctx, api.LedgerSettingsUpdate{Publication: &u, Anchor: &api.LedgerAnchorUpdate{MinimumAnchored: &m}})
	if err != nil || !got.Publication.Enabled || got.Publication.OriginPrefix != pref || got.Publication.IntervalSeconds != 120 || got.Publication.PublicURL == nil || *got.Publication.PublicURL != url || got.Anchor.MinimumAnchored != 3 {
		t.Fatalf("an update: %+v %v", got, err)
	}
	// An absent member is left alone; null clears the URL.
	var clear api.LedgerPublicationUpdate
	clear.PublicURL.Set = true
	got, err = e.svc.UpdateSettings(ctx, api.LedgerSettingsUpdate{Publication: &clear})
	if err != nil || got.Publication.PublicURL != nil || got.Publication.OriginPrefix != pref || got.Anchor.MinimumAnchored != 3 {
		t.Fatalf("null url: %+v %v", got, err)
	}
	for name, in := range map[string]api.LedgerSettingsUpdate{
		"publication interval low":  {Publication: &api.LedgerPublicationUpdate{IntervalSeconds: &bad}},
		"anchor interval low":       {Anchor: &api.LedgerAnchorUpdate{IntervalSeconds: &bad}},
		"origin prefix with scheme": {Publication: &api.LedgerPublicationUpdate{OriginPrefix: ptr("https://x.example")}},
		"origin prefix with space":  {Publication: &api.LedgerPublicationUpdate{OriginPrefix: ptr("a b")}},
	} {
		if _, err := e.svc.UpdateSettings(ctx, in); problemCode(err) != "validation_failed" {
			t.Errorf("%s: %v", name, err)
		}
	}
	// With the first key made, the prefix is part of a signed event: locked.
	e.create(0)
	got, _ = e.svc.Settings(ctx)
	if !got.Publication.OriginPrefixLocked || got.Publication.OriginPrefix != pref {
		t.Errorf("after the first key: %+v", got)
	}
	if _, err := e.svc.UpdateSettings(ctx, api.LedgerSettingsUpdate{Publication: &api.LedgerPublicationUpdate{OriginPrefix: ptr("other/prefix")}}); problemCode(err) != "origin_prefix_locked" {
		t.Errorf("changing the locked prefix: %v", err)
	}
	// Saying the same prefix again is not a change.
	if _, err := e.svc.UpdateSettings(ctx, api.LedgerSettingsUpdate{Publication: &api.LedgerPublicationUpdate{OriginPrefix: &pref}}); err != nil {
		t.Errorf("the same prefix: %v", err)
	}
	// The events and the trust file use the stored prefix.
	s := load(t, e)
	if s.Cluster.OriginPrefix != pref || !strings.Contains(string(s.Cluster.TrustFile), `"origin_prefix": "ledger.example.com/kingres"`) {
		t.Errorf("the prefix of the first key: %q", s.Cluster.OriginPrefix)
	}
}

func ptr[T any](v T) *T { return &v }

func TestKeyFilesAreNotLostWhenTheCommitFailsAfterTheFileWasWritten(t *testing.T) {
	// The file is written before the row; a failed commit leaves a file with no row, and the next try
	// (the same task run again) adopts it instead of making a second key.
	e := newEnv(t, memFactory)
	fail := true
	e.mem.Hook = func(ch ledgersign.Change) error {
		if fail && len(ch.Keys) > 0 {
			fail = false
			return errors.New("the database went away")
		}
		return nil
	}
	tk, err := e.svc.CreateKey(context.Background(), "a", api.CreateLedgerSigningKeyRequest{})
	if err != nil {
		t.Fatal(err)
	}
	_ = e.eng.RunNow(context.Background(), tk.ID, nil)
	got, _ := e.eng.Get(context.Background(), tk.ID)
	if got.State != task.Failed {
		t.Fatalf("the first run: %s", got.State)
	}
	fps, _ := e.kr.List()
	if len(fps) != 1 {
		t.Fatalf("%d key files after the failed commit", len(fps))
	}
	// The same step, again (the engine resumes a task at its failed step): the file is adopted.
	e.mustDone(e.run("ledger.key.create", nil))
	fps2, _ := e.kr.List()
	s := load(t, e)
	if len(s.Keys) != 1 || len(fps2) != 1 || fps2[0] != fps[0] || s.Keys[0].Fingerprint() != fps[0] {
		t.Errorf("after the retry: %d keys, files %v (was %v)", len(s.Keys), fps2, fps)
	}
}

// A key that a failed task left in state next is taken again by the next creation, unless it has waited so long that the
// library would refuse to activate it: then it is left alone (it takes one of the 16 places until it is revoked) and a
// fresh key is made.
func TestAStaleNextKeyIsNotTakenAgainAndAYoungOneIs(t *testing.T) {
	for _, age := range []time.Duration{time.Hour, 301 * 24 * time.Hour} {
		t.Run(age.String(), func(t *testing.T) {
			e := newEnv(t, memFactory)
			killed := false
			e.mem.Hook = func(ch ledgersign.Change) error {
				if !killed && len(ch.Events) > 0 {
					killed = true
					return errors.New("killed")
				}
				return nil
			}
			tk, err := e.svc.CreateKey(context.Background(), "a", api.CreateLedgerSigningKeyRequest{})
			if err != nil {
				t.Fatal(err)
			}
			_ = e.eng.RunNow(context.Background(), tk.ID, nil)
			e.mem.Hook = nil
			first := load(t, e).Keys[0].Fingerprint()
			if stateOf(load(t, e), first) != ledgerkeys.StateNext {
				t.Fatal("the key the killed task made is not next")
			}
			e.clk.Add(age)
			e.mustDone(e.run("ledger.key.create", nil))
			s := load(t, e)
			if age < 300*24*time.Hour {
				if len(s.Keys) != 1 || stateOf(s, first) != ledgerkeys.StateActive {
					t.Errorf("the young key was not taken again: %d keys", len(s.Keys))
				}
				return
			}
			if len(s.Keys) != 2 || stateOf(s, first) != ledgerkeys.StateNext {
				t.Errorf("the stale key was taken again or lost: %d keys, the first is %s", len(s.Keys), stateOf(s, first))
			}
			if err := s.Set().Validate(); err != nil {
				t.Error(err)
			}
		})
	}
}

// A set that holds only next keys (a creation that was killed before it activated its key) is checked too: it is not a
// state the daemon makes, and nothing is signed or started from it.
func TestADamagedSetOfOnlyNextKeysIsAnAlarmToo(t *testing.T) {
	e := newEnv(t, memFactory)
	killed := false
	e.mem.Hook = func(ch ledgersign.Change) error {
		if !killed && len(ch.Events) > 0 {
			killed = true
			return errors.New("killed")
		}
		return nil
	}
	tk, err := e.svc.CreateKey(context.Background(), "a", api.CreateLedgerSigningKeyRequest{})
	if err != nil {
		t.Fatal(err)
	}
	_ = e.eng.RunNow(context.Background(), tk.ID, nil)
	e.mem.Hook = nil
	if a, _ := e.svc.Alarm(context.Background()); a != "" {
		t.Fatalf("an alarm for a set of one next key: %q", a)
	}
	e.mem.Overwrite(func(sn *ledgersign.Snapshot) { sn.Keys[0].MaxTreeSize = 5 }) // a bound on a key that is not retired
	if a, _ := e.svc.Alarm(context.Background()); a == "" {
		t.Error("a damaged set of only next keys is no alarm")
	}
	if _, err := e.svc.CreateKey(context.Background(), "a", api.CreateLedgerSigningKeyRequest{}); problemCode(err) != "conflict" {
		t.Errorf("a creation on a damaged set: %v", err)
	}
}
