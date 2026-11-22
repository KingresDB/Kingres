// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026 RooyeKhat Media Co

package ledgeranchor

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/internalapi"
)

// The tests of the fixes of the review of the P7H fixes (docs/reviews/2026-11-22-ledger-phase7-p7h-fixes.md): P7I-01 to P7I-03.

func installBody(t *testing.T, id int64, file []byte, known uint64) []byte {
	t.Helper()
	b, err := json.Marshal(InstallRequest{TargetID: id, Kind: "worm", File: base64.StdEncoding.EncodeToString(file), Known: known})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// ---- P7I-02: a leader that does not seal and holders that do converge ----

func TestALeaderThatDoesNotSealGivesItsFilesToHoldersThatDo(t *testing.T) {
	e := newEnv(t) // the leader keeps its files in clear
	tg := e.addWorm("vault")
	c, _ := e.creds.Get(tg.ID, "worm")
	for i := 0; i < 2; i++ { // generation 3
		if _, err := e.creds.Put(tg.ID, c); err != nil {
			t.Fatal(err)
		}
	}
	lead, _ := e.creds.Digest(tg.ID)
	holder := newSealedStore(t, t.TempDir())
	agent := &Agent{Creds: holder}
	raw, _ := e.creds.Raw(tg.ID)
	if !strings.Contains(string(raw), `"generation":3`) {
		t.Fatalf("the leader's file is not in clear at generation 3: %s", raw)
	}
	// the file reaches the sealing holder over the call of the leader, and is sealed there with its generation and stamp
	e.svc.cfg.Distribute = func(ctx context.Context, id int64, kind string, file []byte) error {
		_, err := agent.Handle(ctx, CallCredentialsInstall, installBody(t, id, file, KnownGeneration(ctx)))
		return err
	}
	e.svc.cfg.Holders = func(context.Context, []int64) (HolderReport, error) {
		rep := NewHolderReport()
		rep.Lacking[tg.ID] = []string{"h2"}
		return rep, nil
	}
	if err := e.svc.jobCredentials(e.ctx(), e.term); err != nil {
		t.Fatal(err)
	}
	got, err := holder.Digest(tg.ID)
	if err != nil || got != lead {
		t.Fatalf("the holder holds %+v (%v), the leader %+v", got, err, lead)
	}
	if hraw, _ := holder.Raw(tg.ID); strings.Contains(string(hraw), "a-secret-for-the-tests") || strings.Contains(string(hraw), `"generation"`) {
		t.Fatal("the file of the holder is not sealed")
	}
	if p, ok := e.svc.credProblemList()[tg.ID]; ok {
		t.Fatalf("a problem remains: %+v", p)
	}
	// a file in clear that is not from the leader (fetched from a holder, put there by someone) is still refused
	if err := holder.Install(tg.ID+1, "worm", append([]byte(nil), raw...)); !errors.Is(err, ErrPlainFile) {
		t.Fatalf("%v", err)
	}
}

func TestTheAlertNamesWhyAHolderRefusedTheFile(t *testing.T) {
	e := newEnv(t)
	tg := e.addWorm("vault")
	e.svc.cfg.Distribute = func(context.Context, int64, string, []byte) error {
		return errors.New("not reached or refused: h2 (generation_too_far: far above)")
	}
	e.svc.cfg.Holders = func(context.Context, []int64) (HolderReport, error) {
		rep := NewHolderReport()
		rep.Lacking[tg.ID] = []string{"h2"}
		return rep, nil
	}
	if err := e.svc.jobCredentials(e.ctx(), e.term); err != nil {
		t.Fatal(err)
	}
	if p := e.svc.credProblemList()[tg.ID]; !strings.Contains(p.Text, "Cause: not reached or refused: h2 (generation_too_far") {
		t.Fatalf("%+v", p)
	}
}

// A server that has just got a passphrase file seals its own files in clear, so that it does not take itself for behind.
func TestAServerThatJustGotAPassphraseFileSealsItsOwnFilesAndKeepsItsPlaceInTheOrdering(t *testing.T) {
	dir := t.TempDir()
	plain, err := NewCredStore(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	plain.Iterations, plain.MinIterations = 2000, 1000
	st := newStamp(t)
	for id, gen := range map[int64]uint64{1: 3, 3: MaxLeaderJump + 1} {
		if _, err := plain.write(id, creds2(), gen, st); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(plain.path(2), legacyPlain(creds2()), 0o600); err != nil {
		t.Fatal(err)
	}
	before, _ := plain.Digest(1)
	pf := dir + "/the-new-passphrase-file"
	if err := os.WriteFile(pf, []byte("a passphrase of this test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sealing, err := NewCredStore(dir, pf)
	if err != nil {
		t.Fatal(err)
	}
	sealing.Iterations, sealing.MinIterations = 2000, 1000
	if d, _ := sealing.Digest(1); d.Generation != 0 {
		t.Fatalf("a file in clear above 0 counts as %+v on a server that seals: that is what SealPlainFiles repairs", d)
	}
	n, err := sealing.SealPlainFiles()
	if err != nil || n != 1 {
		t.Fatalf("sealed %d (%v), want 1 (the file of generation 3; generation 0 and the one above the bound stay)", n, err)
	}
	if d, _ := sealing.Digest(1); d != before {
		t.Fatalf("after sealing %+v, before %+v: the generation and the digest must stay", d, before)
	}
	if raw, _ := sealing.Raw(1); strings.Contains(string(raw), "a-secret-for-the-tests") {
		t.Fatal("the file is not sealed")
	}
	if raw, _ := sealing.Raw(2); !strings.Contains(string(raw), "a-secret-for-the-tests") {
		t.Fatal("a file of generation 0 was touched")
	}
	if d, _ := sealing.Digest(3); d.Generation != 0 {
		t.Fatalf("a file above the bound counts as %+v", d)
	}
	if n, _ := plain.SealPlainFiles(); n != 0 {
		t.Fatalf("a server that does not seal sealed %d files", n)
	}
	if n, _ := sealing.SealPlainFiles(); n != 0 {
		t.Fatalf("sealed again: %d", n)
	}
}

// ---- P7I-01: the first file is bounded ----

func TestTheFirstFileOfAServerIsBoundedAgainstWhatTheSenderSaw(t *testing.T) {
	e := newEnv(t)
	mk := func(gen uint64) []byte {
		b, err := e.creds.Encode(creds2(), gen, newStamp(t))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	fresh := func() *CredStore {
		s, _ := NewCredStore(t.TempDir(), "")
		s.Iterations, s.MinIterations = 2000, 1000
		return s
	}
	if err := fresh().InstallFrom(1, "worm", mk(100), InstallSource{}); !errors.Is(err, ErrGenerationJump) {
		t.Errorf("a fetched first file of generation 100 with nothing known: %v", err)
	}
	if err := fresh().InstallFrom(1, "worm", mk(40+MaxGenerationJump), InstallSource{Known: 40}); err != nil {
		t.Errorf("at the bound: %v", err)
	}
	if err := fresh().InstallFrom(1, "worm", mk(41+MaxGenerationJump), InstallSource{Known: 40}); !errors.Is(err, ErrGenerationJump) {
		t.Errorf("above the bound: %v", err)
	}
	// from the leader the bound is wider, and still a bound
	if err := fresh().InstallFrom(1, "worm", mk(1000), InstallSource{Leader: true}); err != nil {
		t.Errorf("the leader gives a first file of generation 1000: %v", err)
	}
	if err := fresh().InstallFrom(1, "worm", mk(MaxLeaderJump+1), InstallSource{Leader: true}); !errors.Is(err, ErrGenerationJump) {
		t.Errorf("the leader gives a first file above its bound: %v", err)
	}
	if err := fresh().InstallFrom(1, "worm", mk(MaxLeaderJump+11), InstallSource{Leader: true, Known: 10}); !errors.Is(err, ErrGenerationJump) {
		t.Errorf("above Known plus the bound: %v", err)
	}
}

func TestAFileFetchedByAServerWithoutOneIsBoundedAgainstTheReportOfTheHolders(t *testing.T) {
	e := newEnv(t)
	tg := e.addWorm("vault")
	trow, _ := e.st.Target(e.ctx(), tg.ID)
	if err := e.creds.Remove(tg.ID); err != nil {
		t.Fatal(err)
	}
	e.svc.cfg.Holders = func(context.Context, []int64) (HolderReport, error) {
		rep := NewHolderReport()
		rep.Copies[tg.ID] = []HolderCopy{{Host: "h2", Generation: 50, Digest: "x"}}
		return rep, nil
	}
	good, _ := e.creds.Encode(creds2(), 50, newStamp(t))
	poisoned, _ := e.creds.Encode(creds2(), 1<<30, newStamp(t))
	e.svc.cfg.Fetch = func(context.Context, int64) ([]byte, error) { return append([]byte(nil), poisoned...), nil }
	if got := e.svc.fetchMissing(e.ctx(), trow); got != fetchRefused {
		t.Fatalf("a poisoned file: %v", got)
	}
	if e.creds.Has(tg.ID) {
		t.Fatal("a refused file was written")
	}
	if err := e.svc.jobCredentials(e.ctx(), e.term); err != nil {
		t.Fatal(err)
	}
	if p := e.svc.credProblemList()[tg.ID]; !strings.Contains(p.Text, "far above what the holders report") || !p.Critical {
		t.Fatalf("%+v", p)
	}
	e.svc.cfg.Fetch = func(context.Context, int64) ([]byte, error) { return append([]byte(nil), good...), nil }
	if got := e.svc.fetchMissing(e.ctx(), trow); got != fetchOK {
		t.Fatalf("a good file: %v", got)
	}
}

// ---- P7I-03: a holder that is far behind is brought up to date by the leader, and a leader without a file does not change blindly ----

func gen0(s *CredStore, id int64) uint64 {
	d, _ := s.Digest(id)
	return d.Generation
}

func TestTheLeaderBringsAHolderThatMissedManyChangesUpToDate(t *testing.T) {
	holder := newEnv(t).creds
	if _, err := holder.Rotate(1, creds2(), 2); err != nil { // generation 3
		t.Fatal(err)
	}
	cur := gen0(holder, 1)
	far, _ := holder.Encode(creds2(), cur+1000, newStamp(t))
	// a file that is not from the leader does not cross the bound; the leader's does
	if err := holder.InstallFrom(1, "worm", append([]byte(nil), far...), InstallSource{}); !errors.Is(err, ErrGenerationJump) {
		t.Fatalf("%v", err)
	}
	agent := &Agent{Creds: holder}
	if _, err := agent.Handle(context.Background(), CallCredentialsInstall, installBody(t, 1, far, cur)); err != nil {
		t.Fatalf("the leader's file across the bound was refused: %v", err)
	}
	if g := gen0(holder, 1); g != cur+1000 {
		t.Fatalf("generation %d", g)
	}
	// above the leader's own bound it is a damaged file, and the answer names the remedy
	farther, _ := holder.Encode(creds2(), gen0(holder, 1)+MaxLeaderJump+1, newStamp(t))
	_, err := agent.Handle(context.Background(), CallCredentialsInstall, installBody(t, 1, farther, 0))
	var ce *internalapi.CallError
	if !errors.As(err, &ce) || ce.Status != http.StatusConflict || ce.Code != "generation_too_far" || !strings.Contains(ce.Message, "remove it") {
		t.Fatalf("%v", err)
	}
}

func TestALeaderWithoutAFileDoesNotChangeTheCredentialsWhileTheHoldersHaveOne(t *testing.T) {
	e := newEnv(t)
	tg := e.addWorm("vault")
	if err := e.creds.Remove(tg.ID); err != nil {
		t.Fatal(err)
	}
	e.svc.cfg.Holders = func(context.Context, []int64) (HolderReport, error) {
		rep := NewHolderReport()
		rep.Copies[tg.ID] = []HolderCopy{{Host: "h2", Generation: 100, Digest: "x"}}
		return rep, nil
	}
	rowBefore, _ := e.st.Target(e.ctx(), tg.ID)
	_, err := e.svc.UpdateTarget(e.ctx(), tg.ID, api.LedgerAnchorTargetUpdate{Credentials: wormCreds()})
	if err == nil || !strings.Contains(err.Error(), "has no credentials file of the target and the holders have one") {
		t.Fatalf("%v", err)
	}
	if e.creds.Has(tg.ID) {
		t.Fatal("a file was written blind")
	}
	if row, _ := e.st.Target(e.ctx(), tg.ID); row.Revision != rowBefore.Revision {
		t.Fatal("the row was changed")
	}
	// the view without a file of its own does not hide the holders (the floor is what they report)
	v, err := e.svc.generationView(e.ctx(), tg.ID)
	if err != nil || v.floor != 100 || v.known != 100 || v.ownFile || !v.holdersHaveFile {
		t.Fatalf("%+v %v", v, err)
	}
	// with a file the same report is far above and does not carry the floor
	if _, err := e.creds.write(tg.ID, creds2(), 3, newStamp(t)); err != nil {
		t.Fatal(err)
	}
	v, _ = e.svc.generationView(e.ctx(), tg.ID)
	if v.floor != 0 || v.known != 100 {
		t.Fatalf("%+v", v)
	}
}
