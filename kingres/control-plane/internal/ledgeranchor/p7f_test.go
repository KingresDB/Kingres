// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026 RooyeKhat Media Co

package ledgeranchor

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/anchor"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/scheduler"
)

// The tests of the fixes of the review of the fixes P7R (docs/reviews/2026-11-22-ledger-phase7-p7r-fixes.md): P7F-02, P7F-05 and the
// parts of P7F-01 that do not depend on how copies are ordered. The ordering of copies (the rotation generation) is tested in p7g_test.go.

// staleCreds are credentials of the right shape that the fake store refuses (the old ones of a rotation).
func staleCreds() Creds {
	return Creds{"accessKeyId": anchor.Secret(testKeyID), "secretAccessKey": anchor.Secret("an-old-secret-that-was-rotated-out-0001")}
}

func newStamp(t *testing.T) string {
	t.Helper()
	s, err := NewStamp()
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// infoOfFile is what a server that holds the file raw says about it.
func (e *env) infoOfFile(raw []byte, id int64) CopyInfo {
	e.t.Helper()
	other, err := NewCredStore(e.t.TempDir(), "")
	if err != nil {
		e.t.Fatal(err)
	}
	if err := other.Install(id, "worm", append([]byte(nil), raw...)); err != nil {
		e.t.Fatal(err)
	}
	d, err := other.Digest(id)
	if err != nil {
		e.t.Fatal(err)
	}
	return d
}

// staleLeader is a leader whose file is the old credentials of generation 2, while holder h3 has the current ones of generation 5.
func staleLeader(t *testing.T) (e *env, id int64, current []byte) {
	t.Helper()
	e = newEnv(t)
	tg := e.addWorm("vault")
	id = tg.ID
	valid, err := e.creds.Get(id, "worm")
	if err != nil {
		t.Fatal(err)
	}
	current, err = e.creds.Encode(valid, 5, newStamp(t))
	if err != nil {
		t.Fatal(err)
	}
	cur := e.infoOfFile(current, id)
	if _, err := e.creds.write(id, staleCreds(), 2, newStamp(t)); err != nil {
		t.Fatal(err)
	}
	e.svc.cfg.Holders = func(context.Context, []int64) (HolderReport, error) {
		rep := NewHolderReport()
		rep.Copies[id] = []HolderCopy{{Host: "h3", Generation: cur.Generation, Digest: cur.Digest}}
		return rep, nil
	}
	e.putAgg("shop", 3)
	if err := e.runEnqueue(); err != nil {
		t.Fatal(err)
	}
	return e, id, current
}

// ---- P7F-02: a file that was given and could not be written is not "every holder has none" ----

// blockInstall makes writing a file fail: the directory of the credentials is gone (a disk that cannot be written fails the same way).
func blockInstall(t *testing.T, e *env, _ int64) {
	t.Helper()
	if err := os.RemoveAll(e.creds.dir); err != nil {
		t.Fatal(err)
	}
}

func TestAFetchedFileThatCannotBeWrittenIsNotEveryHolderHavingNone(t *testing.T) {
	e := newEnv(t)
	tg := e.addWorm("vault")
	raw, _ := e.creds.Raw(tg.ID)
	blockInstall(t, e, tg.ID)
	e.svc.cfg.Fetch = func(context.Context, int64) ([]byte, error) { return append([]byte(nil), raw...), nil }
	trow, _ := e.st.Target(e.ctx(), tg.ID)
	if got := e.svc.fetchMissing(e.ctx(), trow); got != fetchNotInstalled {
		t.Fatalf("fetchMissing says %v, want the install failure", got)
	}
	// the job says what happened
	if err := e.svc.jobCredentials(e.ctx(), e.term); err != nil {
		t.Fatal(err)
	}
	_ = e.runMonitor()
	all := e.alerts.all()
	if strings.Contains(all, "no other holder could give it") || !strings.Contains(all, "could not be stored here") || e.alerts.severity(AlertFailed, "target:1|credentials") != "critical" {
		t.Fatalf("the alert: %s", all)
	}
	// and the records wait, they do not fail for good
	e.putAgg("shop", 3)
	if err := e.runEnqueue(); err != nil {
		t.Fatal(err)
	}
	if err := e.runSubmit(); err != nil {
		t.Fatal(err)
	}
	r := e.rows()[0]
	if r.ErrorCode == "target_unbuildable" || r.State == anchor.StateFailed {
		t.Fatalf("the record failed for good because this server could not write a file that a holder gave: %+v", r)
	}
	if r.State != anchor.StatePending || r.ErrorCode != "credentials_unreached" {
		t.Fatalf("the record: state %s code %q", r.State, r.ErrorCode)
	}
	// a file that does not open here is still final: only the operator can help
	e.svc.cfg.Fetch = func(context.Context, int64) ([]byte, error) { return []byte("not a credentials file"), nil }
	if got := e.svc.fetchMissing(e.ctx(), trow); got != fetchUnopenable {
		t.Fatalf("a file that does not decode: %v, want unopenable (final, with its own text)", got)
	}
}

func TestTheAgentSaysADiskThatCannotBeWrittenIsNotAPassphraseMismatch(t *testing.T) {
	e := newEnv(t)
	tg := e.addWorm("vault")
	raw, _ := e.creds.Raw(tg.ID)
	blockInstall(t, e, tg.ID)
	a := &Agent{Creds: e.creds}
	body, _ := json.Marshal(InstallRequest{TargetID: tg.ID, Kind: "worm", File: base64.StdEncoding.EncodeToString(raw)})
	_, err := a.Handle(e.ctx(), CallCredentialsInstall, body)
	if err == nil || !strings.Contains(err.Error(), "could not be written") && !strings.Contains(errText(err), "credentials_not_written") {
		t.Fatalf("%v", err)
	}
	if strings.Contains(errText(err), "passphrase") {
		t.Fatalf("a disk error is reported as a passphrase mismatch: %v", err)
	}
}

// ---- P7F-05: the error of an older term never hides the error of a newer one ----

func TestAnErrorOfALaterTermIsNotHiddenByAnEarlierOne(t *testing.T) {
	e := newEnv(t)
	e.svc.setFatal(1, errors.New("stale news of term 1"))
	e.svc.setFatal(2, scheduler.ErrDeposed)
	if err := e.svc.takeFatal(2); !errors.Is(err, scheduler.ErrDeposed) {
		t.Fatalf("the pass of term 2 got %v, want ErrDeposed", err)
	}
	// and the reverse order: the late error of term 1 does not replace the error of term 2
	e.svc.setFatal(2, scheduler.ErrDeposed)
	e.svc.setFatal(1, errors.New("stale news of term 1"))
	if err := e.svc.takeFatal(2); !errors.Is(err, scheduler.ErrDeposed) {
		t.Fatalf("the late error of term 1 hid the error of term 2: %v", err)
	}
	// the first error of one term is kept
	first := errors.New("first")
	e.svc.setFatal(3, first)
	e.svc.setFatal(3, errors.New("second"))
	if err := e.svc.takeFatal(3); err != first {
		t.Fatalf("%v", err)
	}
}

// The wire form of the list answer: what a holder sends is what the leader reads (generation and digest), and the answer of a server that sends
// no copies is read as a list of files without a digest.
func TestTheListAnswerSurvivesTheWire(t *testing.T) {
	e := newEnv(t)
	tg := e.addWorm("vault")
	out, err := (&Agent{Creds: e.creds}).Handle(e.ctx(), CallCredentialsList, []byte("{}"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	var back ListResponse
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	d, _ := e.creds.Digest(tg.ID)
	if len(back.Copies) != 1 || back.Copies[0] != d || d.Generation != 1 {
		t.Fatalf("%s -> %+v", b, back)
	}
	var old ListResponse
	if err := json.Unmarshal([]byte(`{"ids":[1,2]}`), &old); err != nil || len(old.IDs) != 2 || len(old.Copies) != 0 {
		t.Fatalf("%+v %v", old, err)
	}
	rep := NewHolderReport()
	rep.Add("h9", old, []int64{1, 2})
	if len(rep.Copies) != 0 || len(rep.Lacking) != 0 {
		t.Fatalf("a server that sends no copies has the files and says nothing of them: %+v", rep)
	}
}

// ---- P7F-03 (mutant O2): when the targets cannot be read again after the holders were asked, no file is taken from any holder ----

// secondReadFails is a store whose list of the targets fails from the second call on.
type secondReadFails struct {
	Store
	mu sync.Mutex
	n  int
}

func (s *secondReadFails) Targets(ctx context.Context) ([]TargetRow, error) {
	s.mu.Lock()
	s.n++
	n := s.n
	s.mu.Unlock()
	if n >= 2 {
		return nil, errors.New("the database does not answer")
	}
	return s.Store.Targets(ctx)
}

func TestNoFileIsTakenFromAHolderWhenTheTargetsCannotBeReadAgain(t *testing.T) {
	e := newEnv(t)
	e.addWorm("first")
	var removed []int64
	e.svc.cfg.Undistribute = func(_ context.Context, id int64) error { removed = append(removed, id); return nil }
	e.svc.cfg.Holders = func(context.Context, []int64) (HolderReport, error) {
		// the file of a target that this pass did not see: it may belong to a row that was committed after the first read
		return HolderReport{Orphans: map[int64][]string{77: {"h2"}}}, nil
	}
	e.svc.cfg.Store = &secondReadFails{Store: e.st}
	if err := e.svc.jobCredentials(e.ctx(), e.term); err != nil {
		t.Fatal(err)
	}
	if len(removed) != 0 {
		t.Fatalf("the file of target %v was taken from the holders although the list of the targets could not be read again", removed)
	}
	// with a list that is read again, the same report does take it
	e.svc.cfg.Store = e.st
	if err := e.svc.jobCredentials(e.ctx(), e.term); err != nil {
		t.Fatal(err)
	}
	if len(removed) != 1 || removed[0] != 77 {
		t.Fatalf("removed %v, want 77", removed)
	}
}

// The holders are asked before a target is built at most once in CredGateTTL for each target and term, and again after it (a worker must not
// ask every holder on every pass).
func TestTheHoldersAreAskedBeforeATargetIsBuiltOnlyOnceInTheGateTime(t *testing.T) {
	e, _, raw := staleLeader(t)
	calls := 0
	inner := e.svc.cfg.Holders
	e.svc.cfg.Holders = func(ctx context.Context, ids []int64) (HolderReport, error) { calls++; return inner(ctx, ids) }
	e.svc.cfg.FetchFrom = func(context.Context, string, int64) ([]byte, error) { return append([]byte(nil), raw...), nil }
	if err := e.runSubmit(); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("%d questions to the holders in the first pass, want 1", calls)
	}
	for i := 0; i < 3; i++ {
		e.putAgg("shop", int64(5+i))
		_ = e.runEnqueue()
		_ = e.runSubmit()
	}
	if calls != 1 {
		t.Fatalf("%d questions within the time the answer is believed, want 1", calls)
	}
	e.clock.Advance(CredGateTTL + time.Second)
	e.putAgg("shop", 20)
	_ = e.runEnqueue()
	_ = e.runSubmit()
	if calls != 2 {
		t.Fatalf("%d questions after the time passed, want 2", calls)
	}
	// another term asks again at once
	e.term = 2
	e.st.SetTerm(2)
	e.putAgg("shop", 30)
	_ = e.runEnqueue()
	_ = e.runSubmit()
	if calls != 3 {
		t.Fatalf("%d questions in a new term, want 3", calls)
	}
	// and the entry of the term that is over is dropped (P7G-06)
	e.svc.mu.Lock()
	n := len(e.svc.credGate)
	e.svc.mu.Unlock()
	if n != 1 {
		t.Fatalf("%d entries of the gate are kept, want the one of the term that runs", n)
	}
}
