// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026 RooyeKhat Media Co

package ledgeranchor

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/anchor"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/scheduler"
)

// The tests of the fixes of the review of the anchoring fixes (docs/reviews/2026-11-21-ledger-phase7-anchoring-and-verifier-fixes.md,
// P7R-03, P7R-04, P7R-08 and P7R-10).

// ---- P7R-03: the error of a late worker belongs to its term ----

func TestTheErrorOfALateWorkerOfAnEarlierTermDoesNotEndALaterTerm(t *testing.T) {
	e, id := oneRecord(t)
	// a worker of term 1 meets ErrDeposed after its pass returned
	e.svc.setFatal(1, scheduler.ErrDeposed)
	// this server leads again, in term 2: its first pass works and does not return the old news
	e.st.SetTerm(2)
	e.term = 2
	if err := e.runSubmit(); err != nil {
		t.Fatalf("the first pass of a later term returned %v", err)
	}
	if r := e.row(id); r.State != anchor.StateAnchored {
		t.Fatalf("the first pass of the later term did not work: %+v", r)
	}
	// and the error was dropped, not kept for the pass after
	if err := e.runSubmit(); err != nil {
		t.Fatalf("the second pass: %v", err)
	}
}

func TestTheErrorOfALateWorkerOfThisTermStillEndsTheTerm(t *testing.T) {
	e, id := oneRecord(t)
	e.svc.setFatal(e.term, scheduler.ErrDeposed)
	if err := e.runSubmit(); !isDeposed(err) {
		t.Fatalf("the pass returned %v, want ErrDeposed", err)
	}
	if r := e.row(id); r.State != anchor.StatePending {
		t.Fatalf("a pass that was told the term is over worked: %+v", r)
	}
	// the error is returned once
	if err := e.runSubmit(); err != nil {
		t.Fatalf("the next pass: %v", err)
	}
}

// ---- P7R-04 (M03b): a target that has a worker gets no second one ----

func TestASecondPassStartsNoSecondWorkerForATargetThatHangs(t *testing.T) {
	e := newEnv(t)
	e.svc.cfg.SubmitTimeout = 3 * time.Second
	e.svc.cfg.PassWait = 100 * time.Millisecond
	e.addWorm("hung")
	e.s3.hang = make(chan struct{})
	defer close(e.s3.hang)
	for i := 0; i < 3; i++ {
		e.putAgg("db"+string(rune('a'+i)), 1)
	}
	if err := e.runEnqueue(); err != nil {
		t.Fatal(err)
	}
	if err := e.runSubmit(); err != nil {
		t.Fatal(err)
	}
	n := len(e.s3.requests())
	if n != 1 {
		t.Fatalf("the first pass made %d requests to a target that hangs, want 1 (one worker, one record at a time)", n)
	}
	for i := 0; i < 2; i++ {
		if err := e.runSubmit(); err != nil {
			t.Fatal(err)
		}
	}
	if got := len(e.s3.requests()); got != n {
		t.Fatalf("later passes made %d more requests to the target whose worker is still in its first call (a second worker was started)", got-n)
	}
	// the worker is registered until it ends
	e.svc.mu.Lock()
	working := len(e.svc.working)
	e.svc.mu.Unlock()
	if working != 1 {
		t.Errorf("%d workers are registered, want 1", working)
	}
	if e.svc.beginTarget(DueSubmit, 1) {
		t.Error("beginTarget gives the worker of a target a second time")
	}
	if !e.svc.beginTarget(DueUpgrade, 1) {
		t.Error("the worker for the other set of the same target is refused")
	}
}

// ---- P7R-04 (M05c, M05d): the revision guard of Apply on the database store ----

func TestTheDatabaseStoreAppliesAResultOnlyToTheRevisionItWasMadeFor(t *testing.T) {
	e, admin, st := pgEnv(t)
	e.addWorm("vault")
	e.putAgg("shop", 3)
	if err := e.runEnqueue(); err != nil {
		t.Fatal(err)
	}
	id := e.rows()[0].ID
	row, err := st.Begin(e.ctx(), e.term, id, e.clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	trow, err := st.Target(e.ctx(), row.TargetID)
	if err != nil {
		t.Fatal(err)
	}
	w := row
	w.LastError = "changed by the test"
	// another revision: the result is dropped
	if err := st.Apply(e.ctx(), e.term, w, row.Version, trow.Revision+1); !errors.Is(err, ErrConflict) {
		t.Fatalf("a result for another revision of the target: %v", err)
	}
	if got := e.row(id); got.LastError == "changed by the test" || got.Version != row.Version {
		t.Fatalf("the row changed: %+v", got)
	}
	// the right revision: applied
	if err := st.Apply(e.ctx(), e.term, w, row.Version, trow.Revision); err != nil {
		t.Fatalf("the right revision: %v", err)
	}
	// the revision 0 means that the caller has none to check (a retry by an operator): applied
	w.LastError = "second"
	if err := st.Apply(e.ctx(), e.term, w, row.Version+1, 0); err != nil {
		t.Fatalf("revision 0: %v", err)
	}
	// a target that is switched off without a change of its revision: the result is dropped
	if _, err := admin.Exec(e.ctx(), `UPDATE kingres_cp.ledger_anchor_target SET enabled = false WHERE id = $1`, trow.ID); err != nil {
		t.Fatal(err)
	}
	w.LastError = "third"
	if err := st.Apply(e.ctx(), e.term, w, row.Version+2, trow.Revision); !errors.Is(err, ErrConflict) {
		t.Fatalf("a result for a target that is switched off: %v", err)
	}
}

// ---- P7R-04 (M06b2): the database store does not list an old failure once a newer checkpoint of the same target and log is anchored ----

func TestTheDatabaseStoreDoesNotListAnOldFailureBehindANewerAnchor(t *testing.T) {
	e, _, st := pgEnv(t)
	e.addWorm("vault")
	e.putAgg("shop", 3)
	if err := e.runEnqueue(); err != nil {
		t.Fatal(err)
	}
	id := e.rows()[0].ID
	e.s3.noLock = true
	_ = e.runSubmit()
	if r := e.row(id); r.State != anchor.StateFailed {
		t.Fatalf("setup: %+v", r)
	}
	fs, err := st.Failures(e.ctx())
	if err != nil || len(fs) != 1 || fs[0].ID != id {
		t.Fatalf("the failure is not listed before a newer anchor: %v %v", fs, err)
	}
	e.s3.noLock = false
	e.putAgg("shop", 4)
	_ = e.runEnqueue()
	_ = e.runSubmit()
	rs := e.rows()
	if rs[len(rs)-1].State != anchor.StateAnchored {
		t.Fatalf("the newer record: %+v", rs)
	}
	fs, err = st.Failures(e.ctx())
	if err != nil || len(fs) != 0 {
		t.Fatalf("the old failure is listed although size 4 is anchored: %v %v", fs, err)
	}
	// a newer record that is not anchored does not hide it
	e.s3.noLock = true
	e.putAgg("shop", 5)
	_ = e.runEnqueue()
	_ = e.runSubmit()
	fs, err = st.Failures(e.ctx())
	if err != nil || len(fs) == 0 {
		t.Fatalf("a failure of size 5 is not listed: %v %v", fs, err)
	}
}

// ---- P7R-04 (M14, M15): the credentials job is scheduled, and it does not take the file of an existing target for an orphan ----

func TestTheCredentialsJobIsOneOfTheJobsOfTheService(t *testing.T) {
	e := newEnv(t)
	tg := e.addWorm("vault")
	var given []int64
	e.svc.cfg.Distribute = func(_ context.Context, id int64, _ string, _ []byte) error { given = append(given, id); return nil }
	e.svc.cfg.Holders = func(context.Context, []int64) (HolderReport, error) {
		return HolderReport{Lacking: map[int64][]string{tg.ID: {"h2"}}}, nil
	}
	var job *scheduler.Job
	for _, j := range e.svc.Jobs() {
		if j.Name == "ledger.anchor.credentials" {
			j := j
			job = &j
		}
	}
	if job == nil {
		t.Fatal("ledger.anchor.credentials is not one of the jobs of the service: nothing would ever run it")
	}
	if job.Every != CredentialsEvery || job.Run == nil {
		t.Fatalf("%+v", job)
	}
	if err := job.Run(e.ctx(), e.term); err != nil {
		t.Fatal(err)
	}
	if len(given) != 1 || given[0] != tg.ID {
		t.Fatalf("the job of the scheduler did not give the file to the holder that lacks it: %v", given)
	}
}

func TestTheCredentialsJobDoesNotTakeTheFileOfAnExistingTargetForAnOrphan(t *testing.T) {
	e := newEnv(t)
	tg := e.addWorm("vault")
	var mu sync.Mutex
	var removed []int64
	e.svc.cfg.Undistribute = func(_ context.Context, id int64) error {
		mu.Lock()
		defer mu.Unlock()
		removed = append(removed, id)
		return nil
	}
	e.svc.cfg.Holders = func(context.Context, []int64) (HolderReport, error) {
		// a holder has the file of the existing target, which this server has not listed (a holder the leader lacks the file of), and one of a gone target
		return HolderReport{Orphans: map[int64][]string{tg.ID: {"h2"}, 77: {"h3"}}}, nil
	}
	if err := e.svc.jobCredentials(e.ctx(), e.term); err != nil {
		t.Fatal(err)
	}
	if len(removed) != 1 || removed[0] != 77 {
		t.Fatalf("removed %v, want only the file of the target that is gone (77)", removed)
	}
}

// ---- P7R-10: a target created while the job asks the holders keeps its files ----

func TestATargetCreatedWhileTheHoldersAreAskedKeepsItsFiles(t *testing.T) {
	e := newEnv(t)
	e.addWorm("first")
	var mu sync.Mutex
	var removed []int64
	e.svc.cfg.Undistribute = func(_ context.Context, id int64) error {
		mu.Lock()
		defer mu.Unlock()
		removed = append(removed, id)
		return nil
	}
	var lateID int64
	e.svc.cfg.Holders = func(context.Context, []int64) (HolderReport, error) {
		// CreateTarget has committed the row of a second target and given the file to the holders; the job has read the list of the targets before
		// (creating the target asks the holders for the generation too: only the first question makes the target)
		if lateID == 0 {
			lateID = -1
			lateID = e.addWorm("late").ID
		}
		return HolderReport{Orphans: map[int64][]string{lateID: {"h2", "h3"}, 77: {"h2"}}}, nil
	}
	if err := e.svc.jobCredentials(e.ctx(), e.term); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(removed) != 1 || removed[0] != 77 {
		t.Fatalf("removed %v: the files of a target that was created while the holders were asked must stay, and only the file of the gone target (77) goes", removed)
	}
}

func TestTheFilesOfADeletedTargetAreStillTakenFromTheHolders(t *testing.T) {
	e := newEnv(t)
	tg := e.addWorm("doomed")
	if err := e.svc.DeleteTarget(e.ctx(), tg.ID); err != nil {
		t.Fatal(err)
	}
	var removed []int64
	e.svc.cfg.Undistribute = func(_ context.Context, id int64) error { removed = append(removed, id); return nil }
	e.svc.cfg.Holders = func(context.Context, []int64) (HolderReport, error) {
		return HolderReport{Orphans: map[int64][]string{tg.ID: {"h2"}}}, nil
	}
	if err := e.svc.jobCredentials(e.ctx(), e.term); err != nil {
		t.Fatal(err)
	}
	if len(removed) != 1 || removed[0] != tg.ID {
		t.Fatalf("removed %v, want the deleted target %d", removed, tg.ID)
	}
}

// ---- P7R-04 (M36b to M36d): every form of a secret is removed, whole ----

func TestEachFormOfASecretIsRemovedWholeFromAText(t *testing.T) {
	const secret = "p@ss w0rd/+=~~~???~" // 19 bytes: the standard and the URL base64 forms differ, and the padded and raw ones too
	b := []byte(secret)
	forms := map[string]string{
		"plain":                secret,
		"url query escaped":    url.QueryEscape(secret),
		"url path escaped":     url.PathEscape(secret),
		"hex, lower case":      hex.EncodeToString(b),
		"hex, upper case":      strings.ToUpper(hex.EncodeToString(b)),
		"base64, standard":     base64.StdEncoding.EncodeToString(b),
		"base64, raw standard": base64.RawStdEncoding.EncodeToString(b),
		"base64, URL":          base64.URLEncoding.EncodeToString(b),
		"base64, raw URL":      base64.RawURLEncoding.EncodeToString(b),
	}
	seen := map[string]string{}
	for name, f := range forms {
		if other, dup := seen[f]; dup {
			t.Fatalf("the forms %q and %q are the same text %q: the test needs a secret whose forms differ", name, other, f)
		}
		seen[f] = name
	}
	red := anchor.NewRedactor(anchor.Secret(secret))
	for name, f := range forms {
		// the text holds this one form and nothing else of the secret: the output must be the text with the form replaced, to the last character
		got := red.Clean("x "+f+" y", 400)
		if want := "x " + anchor.RedactedText + " y"; got != want {
			t.Errorf("%s: %q cleaned to %q, want %q", name, f, got, want)
		}
	}
}

// ---- P7R-04 (M04b): the object key of the evidence of a WORM record is compared ----

func TestTheObjectKeyOfTheEvidenceOfAWormRecordMustBeTheKeyOfThisCheckpoint(t *testing.T) {
	e := newEnv(t)
	e.addWorm("vault")
	e.putAgg("shop", 3)
	_ = e.runEnqueue()
	_ = e.runSubmit()
	r, err := e.store.Record(e.ctx(), e.rows()[0].ID)
	if err != nil || r.State != anchor.StateAnchored {
		t.Fatalf("%+v %v", r, err)
	}
	k := kinds["worm"]
	if err := k.Binds(r, r.Evidence); err != nil {
		t.Fatalf("the evidence of the record itself: %v", err)
	}
	ev, err := anchor.ParseWORMEvidence(r.Evidence)
	if err != nil {
		t.Fatal(err)
	}
	// the same object bytes (hash and length), another key: the object of another checkpoint's place
	ev.Key = ev.Key[:strings.LastIndex(ev.Key, "/")+1] + "999.note"
	raw, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	if err := k.Binds(r, raw); err == nil {
		t.Error("evidence with the right hash and length and the key of another size binds the record")
	}
	ev.Key = "other-prefix/" + r.AggregateID + "/" + "4.note"
	raw, _ = json.Marshal(ev)
	if err := k.Binds(r, raw); err == nil {
		t.Error("evidence with the key of another size binds the record")
	}
}

// ---- P7R-08: a holder that cannot be asked is not a holder that has no file ----

func TestFetchFirstTellsEveryHolderHasNoneFromNoneReachable(t *testing.T) {
	ask := func(answers map[string]HolderAnswer, files map[string][]byte) func(context.Context, string) ([]byte, HolderAnswer) {
		return func(_ context.Context, h string) ([]byte, HolderAnswer) { return files[h], answers[h] }
	}
	bg := context.Background()
	if _, err := FetchFirst(bg, []string{"h1", "h2"}, ask(map[string]HolderAnswer{"h1": HolderHasNone, "h2": HolderHasNone}, nil)); !errors.Is(err, ErrNoCredentials) {
		t.Errorf("every holder said none: %v", err)
	}
	if _, err := FetchFirst(bg, nil, ask(nil, nil)); !errors.Is(err, ErrNoCredentials) {
		t.Errorf("no other holder: %v", err)
	}
	if _, err := FetchFirst(bg, []string{"h1", "h2"}, ask(map[string]HolderAnswer{"h1": HolderUnreached, "h2": HolderUnreached}, nil)); !errors.Is(err, ErrHoldersUnreached) || errors.Is(err, ErrNoCredentials) {
		t.Errorf("none reachable: %v", err)
	}
	if _, err := FetchFirst(bg, []string{"h1", "h2"}, ask(map[string]HolderAnswer{"h1": HolderHasNone, "h2": HolderUnreached}, nil)); !errors.Is(err, ErrHoldersUnreached) {
		t.Errorf("one said none and one could not be asked: %v", err)
	}
	got, err := FetchFirst(bg, []string{"h1", "h2"}, ask(map[string]HolderAnswer{"h1": HolderUnreached, "h2": HolderHasFile}, map[string][]byte{"h2": []byte("file")}))
	if err != nil || string(got) != "file" {
		t.Errorf("the second holder has it: %q %v", got, err)
	}
	// "has the file" with no bytes says nothing
	if _, err := FetchFirst(bg, []string{"h1"}, ask(map[string]HolderAnswer{"h1": HolderHasFile}, nil)); !errors.Is(err, ErrHoldersUnreached) {
		t.Errorf("an empty file: %v", err)
	}
	ctx, cancel := context.WithCancel(bg)
	cancel()
	if _, err := FetchFirst(ctx, []string{"h1"}, ask(map[string]HolderAnswer{"h1": HolderHasNone}, nil)); !errors.Is(err, ErrHoldersUnreached) {
		t.Errorf("the caller's time ended: %v", err)
	}
}

func TestRecordsOfATargetWhoseFileNoReachableHolderCouldGiveWaitAndAreNotFailed(t *testing.T) {
	e, id := oneRecord(t)
	file, err := e.creds.Raw(1)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.creds.Remove(1); err != nil {
		t.Fatal(err)
	}
	// the old leader, the only holder of the file, is down
	e.svc.cfg.Fetch = func(context.Context, int64) ([]byte, error) { return nil, ErrHoldersUnreached }
	_ = e.runSubmit()
	r := e.row(id)
	if r.State != anchor.StatePending || r.NextAttemptAt == nil || r.ErrorCode == "target_unbuildable" || r.Attempts == 0 {
		t.Fatalf("a record whose holders could not be asked must wait with a retry, not fail: %+v", r)
	}
	_ = e.runMonitor()
	if e.alerts.severity(AlertFailed, "target:1") != "" {
		t.Errorf("an alert of a failed record for a record that waits: %s", e.alerts.all())
	}
	// the old leader returns, and the next try finds the file
	e.svc.cfg.Fetch = func(context.Context, int64) ([]byte, error) { return file, nil }
	e.clock.Advance(2 * time.Hour)
	_ = e.runSubmit()
	if r := e.row(id); r.State != anchor.StateAnchored {
		t.Fatalf("after the holder is back: %+v", r)
	}
}

func TestRecordsOfATargetWhoseFileEveryHolderLacksStillFailAtOnce(t *testing.T) {
	e, id := oneRecord(t)
	if err := e.creds.Remove(1); err != nil {
		t.Fatal(err)
	}
	e.svc.cfg.Fetch = func(context.Context, int64) ([]byte, error) { return nil, ErrNoCredentials }
	_ = e.runSubmit()
	if r := e.row(id); r.State != anchor.StateFailed || r.ErrorCode != "target_unbuildable" {
		t.Fatalf("%+v", r)
	}
}

func TestTheCredentialsJobSaysWhenTheHoldersCouldNotBeAskedAndKeepsAsking(t *testing.T) {
	e := newEnv(t)
	tg := e.addWorm("vault")
	if err := e.creds.Remove(tg.ID); err != nil {
		t.Fatal(err)
	}
	e.svc.cfg.Fetch = func(context.Context, int64) ([]byte, error) { return nil, ErrHoldersUnreached }
	if err := e.svc.jobCredentials(e.ctx(), e.term); err != nil {
		t.Fatal(err)
	}
	p := e.svc.credProblemList()[tg.ID]
	if !p.Critical || !strings.Contains(p.Text, "not every other holder could be asked") || strings.Contains(p.Text, "Enter the credentials of the target again") {
		t.Fatalf("%+v", p)
	}
	e.svc.cfg.Fetch = func(context.Context, int64) ([]byte, error) { return nil, ErrNoCredentials }
	if err := e.svc.jobCredentials(e.ctx(), e.term); err != nil {
		t.Fatal(err)
	}
	if p := e.svc.credProblemList()[tg.ID]; !strings.Contains(p.Text, "Enter the credentials of the target again") {
		t.Fatalf("every holder said none: %+v", p)
	}
}
