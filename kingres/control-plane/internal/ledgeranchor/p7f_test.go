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
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/scheduler"
)

// The tests of the fixes of the review of the fixes P7R (docs/reviews/2026-11-22-ledger-phase7-p7r-fixes.md): P7F-01, P7F-02, P7F-05.

// staleCreds are credentials of the right shape that the fake store refuses (the old ones of a rotation).
func staleCreds() Creds {
	return Creds{"accessKeyId": anchor.Secret(testKeyID), "secretAccessKey": anchor.Secret("an-old-secret-that-was-rotated-out-0001")}
}

// ageFile sets the time at which the credentials file of a target was written.
func ageFile(t *testing.T, e *env, id int64, at time.Time) {
	t.Helper()
	if err := os.Chtimes(e.creds.path(id), at, at); err != nil {
		t.Fatal(err)
	}
}

// ---- P7F-01: the digest of a credentials file ----

func TestTheDigestOfACredentialsFileIsTheSameWhenSealedAndChangesWithAnyValue(t *testing.T) {
	e := newEnv(t)
	tg := e.addWorm("vault")
	plain, err := e.creds.Digest(tg.ID)
	if err != nil || len(plain.Digest) != 64 || plain.Installed.IsZero() {
		t.Fatalf("%+v %v", plain, err)
	}
	// the same credentials in a file that another server sealed under its own passphrase (another salt and nonce) have the same digest
	pf := e.dir + "/pass"
	if err := os.WriteFile(pf, []byte("a passphrase of this test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sealed, err := NewCredStore(t.TempDir(), pf)
	if err != nil {
		t.Fatal(err)
	}
	sealed.Iterations, sealed.MinIterations = 2000, 1000
	c, err := e.creds.Get(tg.ID, "worm")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sealed.Put(tg.ID, c); err != nil {
		t.Fatal(err)
	}
	if raw, _ := sealed.Raw(tg.ID); strings.Contains(string(raw), testSecret) {
		t.Fatal("the sealed file is not sealed")
	}
	sd, err := sealed.Digest(tg.ID)
	if err != nil || sd.Digest != plain.Digest {
		t.Fatalf("sealed %+v plain %+v %v", sd, plain, err)
	}
	// another value, another member, another target: another digest
	other := Creds{"accessKeyId": c["accessKeyId"], "secretAccessKey": c["secretAccessKey"] + "x"}
	if _, err := e.creds.Put(tg.ID, other); err != nil {
		t.Fatal(err)
	}
	if d, _ := e.creds.Digest(tg.ID); d.Digest == plain.Digest {
		t.Error("another secret gives the same digest")
	}
	if _, err := e.creds.Put(tg.ID, Creds{"accessKeyId": c["accessKeyId"], "secretAccessKey": c["secretAccessKey"], "sessionToken": "a-token"}); err != nil {
		t.Fatal(err)
	}
	if d, _ := e.creds.Digest(tg.ID); d.Digest == plain.Digest {
		t.Error("another member gives the same digest")
	}
	if _, err := e.creds.Put(tg.ID+1, c); err != nil {
		t.Fatal(err)
	}
	if d, _ := e.creds.Digest(tg.ID + 1); d.Digest == plain.Digest {
		t.Error("another target gives the same digest")
	}
	if _, err := e.creds.Digest(99); !errors.Is(err, ErrNoCredentials) {
		t.Errorf("a missing file: %v", err)
	}
	if strings.Contains(plain.Digest, testSecret) {
		t.Error("the digest holds the secret")
	}
}

func TestTheListAnswerCarriesTheDigestsAndAHolderReportKeepsThem(t *testing.T) {
	e := newEnv(t)
	tg := e.addWorm("vault")
	a := &Agent{Creds: e.creds}
	out, err := a.Handle(e.ctx(), CallCredentialsList, []byte("{}"))
	if err != nil {
		t.Fatal(err)
	}
	lr := out.(ListResponse)
	d, _ := e.creds.Digest(tg.ID)
	if len(lr.IDs) != 1 || len(lr.Copies) != 1 || lr.Copies[0] != d {
		t.Fatalf("%+v want digest %+v", lr, d)
	}
	b, _ := json.Marshal(lr)
	if strings.Contains(string(b), testSecret) {
		t.Fatal("the answer holds a secret")
	}
	rep := NewHolderReport()
	rep.Add("h2", lr, []int64{tg.ID, 5})
	rep.Add("h3", ListResponse{IDs: []int64{tg.ID, 77}}, []int64{tg.ID, 5}) // an older server: the file, no copies
	if got := rep.Copies[tg.ID]; len(got) != 1 || got[0].Host != "h2" || got[0].Digest != d.Digest || !got[0].Installed.Equal(d.Installed) {
		t.Fatalf("copies %+v", got)
	}
	if len(rep.Lacking[5]) != 2 || len(rep.Orphans[77]) != 1 || rep.Orphans[77][0] != "h3" {
		t.Fatalf("lacking %v orphans %v", rep.Lacking, rep.Orphans)
	}
}

func TestJudgingTheCopiesOfTheHolders(t *testing.T) {
	now := time.Now()
	own := CopyInfo{Digest: "a", Installed: now}
	h := func(host, d string, at time.Time) HolderCopy { return HolderCopy{Host: host, Digest: d, Installed: at} }
	newer, stale := judgeCopies(own, []HolderCopy{h("h2", "a", now.Add(time.Hour)), h("h3", "b", now.Add(-time.Hour)), h("h4", "c", time.Time{}), h("h5", "d", now.Add(time.Minute)), h("h6", "e", now.Add(time.Hour))})
	if newer == nil || newer.Host != "h6" {
		t.Fatalf("newer %+v, want the newest of the files that differ and are newer (h6)", newer)
	}
	if strings.Join(stale, ",") != "h3,h4" {
		t.Fatalf("stale %v, want h3 (older) and h4 (age not known)", stale)
	}
	if n, s := judgeCopies(own, []HolderCopy{h("h2", "a", now.Add(-time.Hour))}); n != nil || len(s) != 0 {
		t.Fatalf("a holder with the same digest is current whatever its age: %v %v", n, s)
	}
	if n, s := judgeCopies(CopyInfo{Digest: "a"}, []HolderCopy{h("h2", "b", now)}); n != nil || len(s) != 1 {
		t.Fatalf("a leader whose age is not known keeps its file: %v %v", n, s)
	}
	if n, s := judgeCopies(own, []HolderCopy{{Host: "h2"}}); n != nil || len(s) != 0 {
		t.Fatalf("a holder that says nothing is not stale: %v %v", n, s)
	}
}

// ---- P7F-01: the rotation that a holder missed is given again, and the alert stays until every holder is current ----

// rotated is a leader that has changed the credentials of its target while h2 was down: the leader's file is new, h2 still has the old one.
type rotated struct {
	e        *env
	id       int64
	mu       sync.Mutex
	given    int
	failGive bool
	hold     func() HolderReport
}

func newRotated(t *testing.T) *rotated {
	t.Helper()
	r := &rotated{e: newEnv(t)}
	e := r.e
	e.svc.cfg.Distribute = func(context.Context, int64, string, []byte) error {
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.failGive {
			return errors.New("not reached: h2")
		}
		r.given++
		return nil
	}
	tg := e.addWorm("vault")
	r.id = tg.ID
	// the rotation: h2 does not get it
	r.failGive = true
	if _, err := e.svc.UpdateTarget(e.ctx(), tg.ID, api.LedgerAnchorTargetUpdate{Credentials: wormCreds()}); err != nil {
		t.Fatal(err)
	}
	r.failGive = false
	r.given = 0
	_ = e.runMonitor()
	if e.alerts.severity(AlertFailed, "target:1|credentials") != "warning" {
		t.Fatalf("no alert after the failed distribution: %s", e.alerts.all())
	}
	now := time.Now()
	ageFile(t, e, tg.ID, now)
	e.svc.cfg.Holders = func(context.Context, []int64) (HolderReport, error) { return r.hold(), nil }
	// h2 has an older file, h3 the current one
	cur, _ := e.creds.Digest(tg.ID)
	r.hold = func() HolderReport {
		rep := NewHolderReport()
		rep.Copies[tg.ID] = []HolderCopy{{Host: "h2", Digest: "0ld" + cur.Digest[3:], Installed: now.Add(-48 * time.Hour)}, {Host: "h3", Digest: cur.Digest, Installed: now}}
		return rep
	}
	return r
}

func TestARotationThatAHolderMissedIsGivenAgainAndTheAlertGoesOnlyThen(t *testing.T) {
	r := newRotated(t)
	e := r.e
	// the holder is still down: the file cannot be given, and the alert stays, with a text that is true
	r.failGive = true
	if err := e.svc.jobCredentials(e.ctx(), e.term); err != nil {
		t.Fatal(err)
	}
	_ = e.runMonitor()
	if e.alerts.severity(AlertFailed, "target:1|credentials") != "warning" || !strings.Contains(e.alerts.all(), "h2") {
		t.Fatalf("the alert went although h2 holds the old file: %s", e.alerts.all())
	}
	// the holder is up: the job gives the file again (the old code gave a file only to a holder that had none)
	r.failGive = false
	if err := e.svc.jobCredentials(e.ctx(), e.term); err != nil {
		t.Fatal(err)
	}
	if r.given != 1 {
		t.Fatalf("the file was given %d times to the holders, want 1", r.given)
	}
	_ = e.runMonitor()
	if sev := e.alerts.severity(AlertFailed, "target:1|credentials"); sev != "" {
		t.Fatalf("the alert stays after every holder was given the file: %s", e.alerts.all())
	}
	// every holder is current: nothing is given again
	r.hold = func() HolderReport {
		cur, _ := e.creds.Digest(r.id)
		rep := NewHolderReport()
		rep.Copies[r.id] = []HolderCopy{{Host: "h2", Digest: cur.Digest, Installed: time.Now()}, {Host: "h3", Digest: cur.Digest, Installed: time.Now()}}
		return rep
	}
	if err := e.svc.jobCredentials(e.ctx(), e.term); err != nil {
		t.Fatal(err)
	}
	if r.given != 1 {
		t.Fatalf("a current holder was given the file again (%d)", r.given)
	}
}

func TestTheAlertOfAMissedRotationStaysWhileAHolderCannotBeAsked(t *testing.T) {
	r := newRotated(t)
	e := r.e
	// h2 does not answer the question, the others are current: the pass does not know that h2 is, and the alert that says the file is given again
	// every minute stays
	r.hold = func() HolderReport {
		cur, _ := e.creds.Digest(r.id)
		rep := NewHolderReport()
		rep.Copies[r.id] = []HolderCopy{{Host: "h3", Digest: cur.Digest, Installed: time.Now()}}
		rep.Unreached = []string{"h2"}
		return rep
	}
	for i := 0; i < 2; i++ {
		if err := e.svc.jobCredentials(e.ctx(), e.term); err != nil {
			t.Fatal(err)
		}
		_ = e.runMonitor()
		if e.alerts.severity(AlertFailed, "target:1|credentials") != "warning" {
			t.Fatalf("pass %d wiped the alert of a holder that was not asked: %s", i, e.alerts.all())
		}
	}
	if !strings.Contains(e.alerts.all(), "given again every minute") {
		t.Fatalf("the text: %s", e.alerts.all())
	}
	// h2 answers and is current: the alert goes
	r.hold = func() HolderReport {
		cur, _ := e.creds.Digest(r.id)
		rep := NewHolderReport()
		rep.Copies[r.id] = []HolderCopy{{Host: "h2", Digest: cur.Digest, Installed: time.Now()}, {Host: "h3", Digest: cur.Digest, Installed: time.Now()}}
		return rep
	}
	if err := e.svc.jobCredentials(e.ctx(), e.term); err != nil {
		t.Fatal(err)
	}
	_ = e.runMonitor()
	if sev := e.alerts.severity(AlertFailed, "target:1|credentials"); sev != "" {
		t.Fatalf("the alert stays although every holder is current: %s", e.alerts.all())
	}
}

func TestAFailedRedeliveryOfAStaleCopyKeepsTheAlertAndNamesTheHolder(t *testing.T) {
	r := newRotated(t)
	e := r.e
	r.failGive = true
	for i := 0; i < 2; i++ {
		if err := e.svc.jobCredentials(e.ctx(), e.term); err != nil {
			t.Fatal(err)
		}
	}
	_ = e.runMonitor()
	all := e.alerts.all()
	if e.alerts.severity(AlertFailed, "target:1|credentials") != "warning" || !strings.Contains(all, "h2") || strings.Contains(all, "h3") || !strings.Contains(all, "older") {
		t.Fatalf("the alert: %s", all)
	}
}

// ---- P7F-01: a leader that missed the rotation does not anchor with the old credentials ----

// staleLeader is a leader whose file is the old credentials, written two days ago, while holder h3 has the current ones written a moment ago.
func staleLeader(t *testing.T) (e *env, id int64, current []byte) {
	t.Helper()
	e = newEnv(t)
	tg := e.addWorm("vault")
	id = tg.ID
	raw, err := e.creds.Raw(id)
	if err != nil {
		t.Fatal(err)
	}
	cur, _ := e.creds.Digest(id)
	if _, err := e.creds.Put(id, staleCreds()); err != nil {
		t.Fatal(err)
	}
	ageFile(t, e, id, time.Now().Add(-48*time.Hour))
	e.svc.cfg.Holders = func(context.Context, []int64) (HolderReport, error) {
		rep := NewHolderReport()
		rep.Copies[id] = []HolderCopy{{Host: "h3", Digest: cur.Digest, Installed: time.Now()}}
		return rep, nil
	}
	e.putAgg("shop", 3)
	if err := e.runEnqueue(); err != nil {
		t.Fatal(err)
	}
	return e, id, raw
}

func TestALeaderThatMissedARotationTakesTheNewFileBeforeItAnchors(t *testing.T) {
	e, id, raw := staleLeader(t)
	var asked []string
	e.svc.cfg.FetchFrom = func(_ context.Context, host string, tid int64) ([]byte, error) {
		asked = append(asked, host)
		return append([]byte(nil), raw...), nil
	}
	if err := e.runSubmit(); err != nil {
		t.Fatal(err)
	}
	if len(asked) != 1 || asked[0] != "h3" {
		t.Fatalf("asked %v, want h3 once", asked)
	}
	rows := e.rows()
	if len(rows) != 1 || rows[0].State != anchor.StateAnchored {
		t.Fatalf("the record was not anchored with the current credentials: %+v", rows)
	}
	for _, r := range e.s3.requests() {
		if strings.Contains(r.header.Get("Authorization"), "an-old-secret") {
			t.Fatal("a request was signed with the old secret")
		}
	}
	c, err := e.creds.Get(id, "worm")
	if err != nil || c["secretAccessKey"].Reveal() != testSecret {
		t.Fatalf("the file of the leader was not replaced: %v", err)
	}
	// the answer is believed for a while: the next pass does not ask again
	e.putAgg("shop", 5)
	_ = e.runEnqueue()
	_ = e.runSubmit()
	if len(asked) != 1 {
		t.Fatalf("asked again within the time the answer is believed: %v", asked)
	}
}

func TestALeaderThatMissedARotationAndCannotTakeTheNewFileDoesNotAnchor(t *testing.T) {
	e, id, _ := staleLeader(t)
	e.svc.cfg.FetchFrom = func(context.Context, string, int64) ([]byte, error) { return nil, errors.New("h3 went away") }
	if err := e.runSubmit(); err != nil {
		t.Fatal(err)
	}
	if n := len(e.s3.requests()); n != 0 {
		t.Fatalf("%d requests were made to the store with credentials that a holder has newer", n)
	}
	r := e.rows()[0]
	if r.State == anchor.StateAnchored || r.State == anchor.StateFailed || r.ErrorCode != "credentials_stale" {
		t.Fatalf("the record: state %s code %q: it must wait for the newer file, not fail", r.State, r.ErrorCode)
	}
	_ = id
}

func TestALeaderWhoseHoldersCannotBeAskedAnchorsWithItsOwnFile(t *testing.T) {
	e, _, _ := staleLeader(t)
	e.svc.cfg.Holders = func(context.Context, []int64) (HolderReport, error) {
		return NewHolderReport(), errors.New("no holder answers")
	}
	called := false
	e.svc.cfg.FetchFrom = func(context.Context, string, int64) ([]byte, error) { called = true; return nil, errors.New("no") }
	if err := e.runSubmit(); err != nil {
		t.Fatal(err)
	}
	if called {
		t.Error("a holder was asked for a file although none could be asked about it")
	}
	if len(e.s3.requests()) == 0 {
		t.Fatal("a leader whose holders do not answer (the old leader is down) must go on with the file it has")
	}
}

func TestTheCredentialsJobOfALeaderThatMissedARotationTakesTheNewFileAndGivesNothingAway(t *testing.T) {
	e, id, raw := staleLeader(t)
	given := 0
	e.svc.cfg.Distribute = func(context.Context, int64, string, []byte) error { given++; return nil }
	e.svc.cfg.FetchFrom = func(context.Context, string, int64) ([]byte, error) { return append([]byte(nil), raw...), nil }
	if err := e.svc.jobCredentials(e.ctx(), e.term); err != nil {
		t.Fatal(err)
	}
	c, err := e.creds.Get(id, "worm")
	if err != nil || c["secretAccessKey"].Reveal() != testSecret {
		t.Fatalf("the file of the leader was not replaced: %v", err)
	}
	if given != 0 {
		t.Fatalf("the leader gave away its file %d times although the holder that is current has the same one", given)
	}
	// when the newer file cannot be taken, the old one is not given to the holders either, and the alert is critical
	if _, err := e.creds.Put(id, staleCreds()); err != nil {
		t.Fatal(err)
	}
	ageFile(t, e, id, time.Now().Add(-48*time.Hour))
	e.svc.cfg.FetchFrom = func(context.Context, string, int64) ([]byte, error) { return nil, errors.New("h3 went away") }
	e.svc.cfg.Holders = func(context.Context, []int64) (HolderReport, error) {
		cur := e.mustDigestOf(raw, id)
		rep := NewHolderReport()
		rep.Copies[id] = []HolderCopy{{Host: "h3", Digest: cur, Installed: time.Now()}, {Host: "h2", Digest: "x", Installed: time.Now().Add(-72 * time.Hour)}}
		return rep, nil
	}
	if err := e.svc.jobCredentials(e.ctx(), e.term); err != nil {
		t.Fatal(err)
	}
	if given != 0 {
		t.Fatalf("a leader gave away a file that a holder has newer (%d)", given)
	}
	_ = e.runMonitor()
	if e.alerts.severity(AlertFailed, "target:1|credentials") != "critical" || !strings.Contains(e.alerts.all(), "older credentials file") {
		t.Fatalf("the alert: %s", e.alerts.all())
	}
}

// mustDigestOf is the digest that a file of the given bytes has under this store.
func (e *env) mustDigestOf(raw []byte, id int64) string {
	e.t.Helper()
	other, err := NewCredStore(e.t.TempDir(), "")
	if err != nil {
		e.t.Fatal(err)
	}
	c, err := e.creds.Decode(raw, "worm")
	if err != nil {
		e.t.Fatal(err)
	}
	if _, err := other.Put(id, c); err != nil {
		e.t.Fatal(err)
	}
	d, err := other.Digest(id)
	if err != nil {
		e.t.Fatal(err)
	}
	return d.Digest
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
	if strings.Contains(all, "no other holder could give it") || !strings.Contains(all, "could not be written here") || e.alerts.severity(AlertFailed, "target:1|credentials") != "critical" {
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
	if got := e.svc.fetchMissing(e.ctx(), trow); got != fetchNone {
		t.Fatalf("a file that does not decode: %v, want none", got)
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
}

// The wire form of the list answer: what a holder sends is what the leader reads, with the time to the nanosecond, and the answer of a server
// that sends no copies is read as a list of files without a digest.
func TestTheListAnswerSurvivesTheWire(t *testing.T) {
	e := newEnv(t)
	tg := e.addWorm("vault")
	at := time.Date(2026, 11, 20, 8, 30, 15, 123456789, time.UTC)
	ageFile(t, e, tg.ID, at)
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
	if len(back.Copies) != 1 || back.Copies[0].Digest != d.Digest || !back.Copies[0].Installed.Equal(at.Truncate(time.Nanosecond)) && back.Copies[0].Installed.Sub(at).Abs() > time.Second {
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
