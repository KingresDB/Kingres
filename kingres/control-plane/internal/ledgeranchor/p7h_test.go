// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026 RooyeKhat Media Co

package ledgeranchor

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/anchor"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
)

// The tests of the fixes of the third review (docs/reviews/2026-11-22-ledger-phase7-p7g-fixes.md): P7H-01 to P7H-06.

func creds2() Creds {
	return Creds{"accessKeyId": "AKIAFAKEKEYID000001", "secretAccessKey": "a-secret-for-the-tests-of-p7h-0001"}
}

// ---- P7H-01: a plain file with a chosen generation cannot win on a server that seals, and no jump is believed ----

// Probe H1 of the review: a file in clear with the highest generation that the parser accepts, put in a holder that seals its files.
func TestAPlainFileWithAChosenGenerationIsRefusedByAServerThatSealsAndRotationStillWorks(t *testing.T) {
	e := newEnv(t)
	sealed := newSealedStore(t, e.dir)
	for _, gen := range []uint64{1, 7, MaxCredGeneration} {
		plain, err := e.creds.Encode(creds2(), gen, newStamp(t))
		if err != nil {
			t.Fatal(err)
		}
		if err := sealed.Install(9, "worm", plain); !errors.Is(err, ErrPlainFile) {
			t.Fatalf("a plain file of generation %d: %v", gen, err)
		}
		if sealed.Has(9) {
			t.Fatalf("a refused file was written (generation %d)", gen)
		}
	}
	// the generation 0 of an earlier version is still taken (it is older than every rotation)
	if err := sealed.Install(9, "worm", legacyPlain(creds2())); err != nil {
		t.Fatalf("a plain file of an earlier version: %v", err)
	}
	if d, _ := sealed.Digest(9); d.Generation != 0 {
		t.Fatalf("%+v", d)
	}
	// a plain file put into the directory of a server that seals does not take part in the ordering, whatever number it carries
	plain, _ := e.creds.Encode(creds2(), MaxCredGeneration, newStamp(t))
	if err := os.WriteFile(sealed.path(10), plain, 0o600); err != nil {
		t.Fatal(err)
	}
	d, err := sealed.Digest(10)
	if err != nil || d.Generation != 0 || d.Digest != "" {
		t.Fatalf("a planted plain file counts as %+v (%v)", d, err)
	}
	// and the operator can still change the credentials: nothing is stuck
	if _, err := sealed.Rotate(10, creds2(), 0); err != nil {
		t.Fatalf("a rotation over a planted file: %v", err)
	}
	if d, _ := sealed.Digest(10); d.Generation != 1 || d.Digest == "" {
		t.Fatalf("%+v", d)
	}
	// a server that does not seal still takes a plain file, but a first file is bounded against what the sender saw (P7I-01)
	if err := e.creds.Install(11, "worm", plain); !errors.Is(err, ErrGenerationJump) {
		t.Fatalf("a first file at the top of the range: %v", err)
	}
	near, _ := e.creds.Encode(creds2(), 5, newStamp(t))
	if err := e.creds.Install(11, "worm", near); err != nil {
		t.Fatalf("a plain server: %v", err)
	}
}

func TestNoJumpOfTheGenerationIsTakenByAnInstall(t *testing.T) {
	for name, st := range map[string]*CredStore{"plain": newEnv(t).creds, "sealed": newSealedStore(t, t.TempDir())} {
		if _, err := st.Rotate(5, creds2(), 2); err != nil { // generation 3
			t.Fatal(err)
		}
		cur, _ := st.Digest(5)
		mk := func(gen uint64) []byte {
			b, err := st.Encode(creds2(), gen, newStamp(t))
			if err != nil {
				t.Fatal(err)
			}
			return b
		}
		if err := st.Install(5, "worm", mk(cur.Generation+MaxGenerationJump+1)); !errors.Is(err, ErrGenerationJump) {
			t.Errorf("%s: a jump of %d: %v", name, MaxGenerationJump+1, err)
		}
		if now, _ := st.Digest(5); now != cur {
			t.Errorf("%s: a refused file was written", name)
		}
		if err := st.Install(5, "worm", mk(cur.Generation+MaxGenerationJump)); err != nil {
			t.Errorf("%s: a jump of %d: %v", name, MaxGenerationJump, err)
		}
	}
}

func TestAHolderThatReportsAGenerationFarAboveIsNotBelievedAndTheAlertSaysHowToGetOut(t *testing.T) {
	e := newEnv(t)
	tg := e.addWorm("vault")
	own, _ := e.creds.Digest(tg.ID)
	e.svc.cfg.Holders = func(context.Context, []int64) (HolderReport, error) {
		rep := NewHolderReport()
		rep.Copies[tg.ID] = []HolderCopy{{Host: "h3", Generation: own.Generation + MaxGenerationJump + 1, Digest: "x"}, {Host: "h4", Generation: own.Generation + MaxGenerationJump, Digest: "y"}}
		return rep, nil
	}
	n, s, k, j := judgeCopies(own, e.holderCopies(tg.ID))
	if n == nil || n.Host != "h4" || len(s) != 0 || len(k) != 0 || strings.Join(j, ",") != "h3" {
		t.Fatalf("newer %+v stale %v conflict %v jump %v: h4 is at the bound and newer, h3 is above it", n, s, k, j)
	}
	// the job neither takes nor replaces the file of the holder that is above the bound
	asked := []string{}
	e.svc.cfg.FetchFrom = func(_ context.Context, host string, _ int64) ([]byte, error) {
		asked = append(asked, host)
		return nil, errors.New("no")
	}
	gave := false
	e.svc.cfg.Distribute = func(context.Context, int64, string, []byte) error { gave = true; return nil }
	e.svc.cfg.Holders = func(context.Context, []int64) (HolderReport, error) {
		rep := NewHolderReport()
		rep.Copies[tg.ID] = []HolderCopy{{Host: "h3", Generation: own.Generation + MaxGenerationJump + 1, Digest: "x"}}
		return rep, nil
	}
	if err := e.svc.jobCredentials(e.ctx(), e.term); err != nil {
		t.Fatal(err)
	}
	if len(asked) != 0 || gave {
		t.Fatalf("asked %v gave %v: a holder above the bound is neither taken nor replaced", asked, gave)
	}
	p := e.svc.credProblemList()[tg.ID]
	if !strings.Contains(p.Text, "Remove the file of the target on that holder") || !strings.Contains(p.Text, "h3") || p.Critical {
		t.Fatalf("%+v", p)
	}
	// the gate does not wait for it either, and a rotation here is not carried to its number
	e.putAgg("shop", 3)
	_ = e.runEnqueue()
	_ = e.runSubmit()
	if rows := e.rows(); len(rows) != 1 || rows[0].State != anchor.StateAnchored {
		t.Fatalf("%+v", rows)
	}
	if _, err := e.svc.UpdateTarget(e.ctx(), tg.ID, api.LedgerAnchorTargetUpdate{Credentials: wormCreds()}); err != nil {
		t.Fatal(err)
	}
	if d, _ := e.creds.Digest(tg.ID); d.Generation != own.Generation+1 {
		t.Fatalf("%+v: the number of a holder above the bound must not carry the rotation", d)
	}
}

func (e *env) holderCopies(id int64) []HolderCopy {
	rep, _ := e.svc.cfg.Holders(e.ctx(), []int64{id})
	return rep.Copies[id]
}

func TestTheGenerationAtTheBoundGivesAWayOut(t *testing.T) {
	e := newEnv(t)
	tg := e.addWorm("vault")
	if _, err := e.creds.write(tg.ID, creds2(), MaxCredGeneration, newStamp(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := e.creds.Rotate(tg.ID, creds2(), 0); !errors.Is(err, ErrGenerationExhausted) || !strings.Contains(err.Error(), "remove the file") {
		t.Fatalf("%v", err)
	}
	// the way out the message names: remove the file, enter the credentials again
	if err := e.creds.Remove(tg.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.creds.Rotate(tg.ID, creds2(), 0); err != nil {
		t.Fatal(err)
	}
}

// ---- P7H-02: the wait of an episode ends when its cause ends ----

func TestTwoStaleEpisodesAreTwoWaitsInTheGate(t *testing.T) {
	e, id, _ := staleLeader(t)
	trow, _ := e.st.Target(e.ctx(), id)
	e.svc.cfg.FetchFrom = func(context.Context, string, int64) ([]byte, error) { return nil, errors.New("h3 went away") }
	err := e.svc.ensureCurrent(e.ctx(), e.term, trow)
	var se credsStaleError
	if !errors.As(err, &se) || se.final {
		t.Fatalf("the first failure of an episode: %v", err)
	}
	// the holders agree (the holder that had the newer file is gone, or the operator entered the credentials above it)
	own, _ := e.creds.Digest(id)
	e.clock.Advance(10 * time.Minute)
	e.svc.cfg.Holders = func(context.Context, []int64) (HolderReport, error) {
		rep := NewHolderReport()
		rep.Copies[id] = []HolderCopy{{Host: "h3", Generation: own.Generation, Digest: own.Digest}}
		return rep, nil
	}
	if err := e.svc.ensureCurrent(e.ctx(), e.term, trow); err != nil {
		t.Fatal(err)
	}
	e.svc.mu.Lock()
	n := len(e.svc.credStaleSince)
	e.svc.mu.Unlock()
	if n != 0 {
		t.Fatalf("the wait of the first episode was not ended by the agreement (%d notes)", n)
	}
	// two hours later another holder has a newer file that cannot be fetched: a new wait, not the end of the old one
	e.clock.Advance(2 * time.Hour)
	e.svc.cfg.Holders = func(context.Context, []int64) (HolderReport, error) {
		rep := NewHolderReport()
		rep.Copies[id] = []HolderCopy{{Host: "h5", Generation: own.Generation + 3, Digest: "zz"}}
		return rep, nil
	}
	err = e.svc.ensureCurrent(e.ctx(), e.term, trow)
	if !errors.As(err, &se) || se.final {
		t.Fatalf("the first failure of the second episode is final: %v", err)
	}
}

func TestTheJobEndsTheWaitWhenNoHolderHasANewerFile(t *testing.T) {
	e, id, _ := staleLeader(t)
	e.svc.cfg.FetchFrom = func(context.Context, string, int64) ([]byte, error) { return nil, errors.New("h3 went away") }
	_ = e.runSubmit() // the gate notes the failure
	e.svc.mu.Lock()
	n := len(e.svc.credStaleSince)
	e.svc.mu.Unlock()
	if n != 1 {
		t.Fatalf("not noted (%d)", n)
	}
	own, _ := e.creds.Digest(id)
	agree := func(context.Context, []int64) (HolderReport, error) {
		rep := NewHolderReport()
		rep.Copies[id] = []HolderCopy{{Host: "h3", Generation: own.Generation, Digest: own.Digest}}
		return rep, nil
	}
	// a pass that could not ask the holders knows nothing and leaves the note
	e.svc.cfg.Holders = func(context.Context, []int64) (HolderReport, error) {
		return NewHolderReport(), errors.New("no answer")
	}
	if err := e.svc.jobCredentials(e.ctx(), e.term); err != nil {
		t.Fatal(err)
	}
	e.svc.mu.Lock()
	n = len(e.svc.credStaleSince)
	e.svc.mu.Unlock()
	if n != 1 {
		t.Fatalf("a pass that asked nobody ended the wait (%d)", n)
	}
	e.svc.cfg.Holders = agree
	if err := e.svc.jobCredentials(e.ctx(), e.term); err != nil {
		t.Fatal(err)
	}
	e.svc.mu.Lock()
	n = len(e.svc.credStaleSince)
	e.svc.mu.Unlock()
	if n != 0 {
		t.Fatalf("the job did not end the wait (%d)", n)
	}
}

// ---- P7H-06: the bound is exactly CredStaleLimit ----

func TestTheWaitForANewerFileEndsExactlyAtTheLimit(t *testing.T) {
	e := newEnv(t)
	if CredStaleLimit != 30*time.Minute {
		t.Fatalf("the limit is %v, the design and the alert say 30 minutes", CredStaleLimit)
	}
	if e.svc.staleFailure(1, takeFetch) {
		t.Fatal("the first failure is final")
	}
	e.clock.Advance(CredStaleLimit - time.Second)
	if e.svc.staleFailure(1, takeFetch) {
		t.Fatal("final one second before the limit")
	}
	e.clock.Advance(time.Second)
	if !e.svc.staleFailure(1, takeFetch) {
		t.Fatal("not final at the limit")
	}
	// a failure that is not about the file waits for the same time, one that is about the file does not
	if !e.svc.staleFailure(2, takeUnopenable) {
		t.Fatal("an unopenable file is not final at once")
	}
	if e.svc.staleFailure(3, takeLocal) || e.svc.staleFailure(4, takeOlder) {
		t.Fatal("a failure of this server or an older file is final at once")
	}
}

// ---- P7H-03: a file taken from a holder is an alert, and a change of credentials that has not seen every holder is refused ----

func TestTakingTheNewerFileOfAHolderRaisesAnAlertThatStays(t *testing.T) {
	e, id, raw := staleLeader(t)
	e.svc.cfg.FetchFrom = func(context.Context, string, int64) ([]byte, error) { return append([]byte(nil), raw...), nil }
	if err := e.runSubmit(); err != nil { // the gate takes the file
		t.Fatal(err)
	}
	if err := e.svc.jobCredentials(e.ctx(), e.term); err != nil {
		t.Fatal(err)
	}
	_ = e.runMonitor()
	if e.alerts.severity(AlertFailed, "target:1|credentials") != "warning" || !strings.Contains(e.alerts.all(), "replaced its credentials file") || !strings.Contains(e.alerts.all(), "h3") {
		t.Fatalf("no alert for a file that was taken: %s", e.alerts.all())
	}
	// it stays for a day, and goes after it
	e.clock.Advance(time.Hour)
	if err := e.svc.jobCredentials(e.ctx(), e.term); err != nil {
		t.Fatal(err)
	}
	if p := e.svc.credProblemList()[id]; !strings.Contains(p.Text, "replaced its credentials file") {
		t.Fatalf("the notice is gone after an hour: %+v", p)
	}
	e.clock.Advance(CredNoticeFor)
	if err := e.svc.jobCredentials(e.ctx(), e.term); err != nil {
		t.Fatal(err)
	}
	if p, ok := e.svc.credProblemList()[id]; ok {
		t.Fatalf("the notice stays after %v: %+v", CredNoticeFor, p)
	}
}

func TestTheJobRaisesTheAlertToo(t *testing.T) {
	e, id, raw := staleLeader(t)
	e.svc.cfg.FetchFrom = func(context.Context, string, int64) ([]byte, error) { return append([]byte(nil), raw...), nil }
	if err := e.svc.jobCredentials(e.ctx(), e.term); err != nil {
		t.Fatal(err)
	}
	if p := e.svc.credProblemList()[id]; !strings.Contains(p.Text, "replaced its credentials file") {
		t.Fatalf("%+v", p)
	}
}

func TestAChangeOfCredentialsThatHasNotSeenEveryHolderIsRefused(t *testing.T) {
	e := newEnv(t)
	tg := e.addWorm("vault")
	before, _ := e.creds.Digest(tg.ID)
	rowBefore, _ := e.st.Target(e.ctx(), tg.ID)
	for name, hold := range map[string]func(context.Context, []int64) (HolderReport, error){
		"a holder that did not answer": func(context.Context, []int64) (HolderReport, error) {
			rep := NewHolderReport()
			rep.Unreached = []string{"h2"}
			return rep, nil
		},
		"the holders could not be listed": func(context.Context, []int64) (HolderReport, error) {
			return NewHolderReport(), errors.New("the list of the members cannot be read")
		},
	} {
		e.svc.cfg.Holders = hold
		_, err := e.svc.UpdateTarget(e.ctx(), tg.ID, api.LedgerAnchorTargetUpdate{Credentials: wormCreds()})
		if err == nil || !strings.Contains(err.Error(), "The credentials were not changed") {
			t.Fatalf("%s: %v", name, err)
		}
		if now, _ := e.creds.Digest(tg.ID); now != before {
			t.Fatalf("%s: the file was changed", name)
		}
		if row, _ := e.st.Target(e.ctx(), tg.ID); row.Revision != rowBefore.Revision {
			t.Fatalf("%s: the row was changed", name)
		}
	}
	if !strings.Contains(rotationPartialError{hosts: []string{"h2", "h5"}}.Error(), "h2, h5") {
		t.Fatal("the answer does not name the holders")
	}
	// every holder answered: the change is made
	e.svc.cfg.Holders = func(context.Context, []int64) (HolderReport, error) { return NewHolderReport(), nil }
	if _, err := e.svc.UpdateTarget(e.ctx(), tg.ID, api.LedgerAnchorTargetUpdate{Credentials: wormCreds()}); err != nil {
		t.Fatal(err)
	}
	if now, _ := e.creds.Digest(tg.ID); now.Generation != before.Generation+1 {
		t.Fatalf("%+v", now)
	}
	// a target that is made has no earlier file anywhere: a holder that is away cannot undo anything
	e.svc.cfg.Holders = func(context.Context, []int64) (HolderReport, error) {
		rep := NewHolderReport()
		rep.Unreached = []string{"h2"}
		return rep, nil
	}
	if _, err := e.svc.CreateTarget(e.ctx(), "admin", api.LedgerAnchorTargetCreate{Kind: "worm", Name: "second", Enabled: true, Config: e.wormConfig(nil), Credentials: wormCreds()}); err != nil {
		t.Fatalf("a new target was refused: %v", err)
	}
	// and a change that brings credentials to a target that had none
	if _, err := e.svc.CreateTarget(e.ctx(), "admin", api.LedgerAnchorTargetCreate{Kind: "worm", Name: "third", Enabled: false, Config: e.wormConfig(nil)}); err != nil {
		t.Fatal(err)
	}
	third := int64(3)
	if _, err := e.svc.UpdateTarget(e.ctx(), third, api.LedgerAnchorTargetUpdate{Credentials: wormCreds()}); err != nil {
		t.Fatalf("credentials for a target that had none were refused: %v", err)
	}
}

// ---- P7H-04: changes of the credentials of one target go one at a time ----

func TestConcurrentRotationsOfOneFileGetDifferentGenerations(t *testing.T) {
	e := newEnv(t)
	tg := e.addWorm("vault")
	const n = 24
	var wg sync.WaitGroup
	gens := make(chan uint64, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			data, err := e.creds.Rotate(tg.ID, creds2(), 0)
			if err != nil {
				t.Error(err)
				return
			}
			st, err := e.creds.DecodeFile(data, "worm")
			if err != nil {
				t.Error(err)
				return
			}
			gens <- st.Generation
		}()
	}
	wg.Wait()
	close(gens)
	seen := map[uint64]bool{}
	for g := range gens {
		if seen[g] {
			t.Fatalf("generation %d was given twice", g)
		}
		seen[g] = true
	}
	if d, _ := e.creds.Digest(tg.ID); d.Generation != 1+n || len(seen) != n {
		t.Fatalf("%+v after %d rotations from generation 1 (%d distinct)", d, n, len(seen))
	}
}

func TestChangesOfTheCredentialsOfOneTargetAreHandedToTheHoldersOneAtATime(t *testing.T) {
	e := newEnv(t)
	tg := e.addWorm("vault")
	var active, most int32
	var order []uint64
	var mu sync.Mutex
	e.svc.cfg.Distribute = func(_ context.Context, id int64, _ string, file []byte) error {
		cur := atomic.AddInt32(&active, 1)
		for {
			m := atomic.LoadInt32(&most)
			if cur <= m || atomic.CompareAndSwapInt32(&most, m, cur) {
				break
			}
		}
		st, err := e.creds.DecodeFile(file, "worm")
		if err != nil {
			t.Error(err)
		}
		mu.Lock()
		order = append(order, st.Generation)
		mu.Unlock()
		time.Sleep(15 * time.Millisecond)
		atomic.AddInt32(&active, -1)
		return nil
	}
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := e.svc.putCreds(e.ctx(), tg.ID, "worm", creds2(), false); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if most != 1 {
		t.Fatalf("%d hand-overs of one target ran at once", most)
	}
	for i := 1; i < len(order); i++ {
		if order[i] != order[i-1]+1 {
			t.Fatalf("the generations were handed over in the order %v", order)
		}
	}
}
