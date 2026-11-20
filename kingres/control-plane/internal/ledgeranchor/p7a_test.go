// SPDX-License-Identifier: AGPL-3.0-only

package ledgeranchor

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/anchor"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
)

// The tests of the fixes of the review of the wiring of anchoring (docs/reviews/2026-11-20-kingresd-anchoring-wiring-and-publication-fixes.md,
// P7A-01 and the following).

// ---- P7A-01: the due list is selected by state and time before it is limited ----

// starve makes BatchSize+3 records of one target claimed with a next check in the future (the oldest ones), and leaves the two newest pending.
func starve(t *testing.T, e *env, claimed func(upTo int64, next time.Time)) (pending []int64) {
	t.Helper()
	e.addWorm("vault")
	n := BatchSize + 5
	for i := 0; i < n; i++ {
		e.putAgg(fmt.Sprintf("db%03d", i), 1)
	}
	if err := e.runEnqueue(); err != nil {
		t.Fatal(err)
	}
	rs := e.rows()
	if len(rs) != n {
		t.Fatalf("%d records, want %d", len(rs), n)
	}
	claimed(rs[n-3].ID, e.clock.Now().Add(6*time.Hour))
	return []int64{rs[n-2].ID, rs[n-1].ID}
}

func checkNotStarved(t *testing.T, e *env, pending []int64) {
	t.Helper()
	got, err := e.store.Due(e.ctx(), e.clock.Now(), DueSubmit, BatchSize)
	if err != nil {
		t.Fatal(err)
	}
	// the claimed rows with a check in the future are not in the answer, whatever the limit
	var ids []int64
	for _, r := range got {
		ids = append(ids, r.ID)
	}
	if len(ids) < len(pending) {
		t.Fatalf("Due(submit) returned %v, want at least the pending %v", ids, pending)
	}
	for _, id := range pending {
		found := false
		for _, g := range ids {
			found = found || g == id
		}
		if !found {
			t.Fatalf("pending record %d is not in %v", id, ids)
		}
	}
	for _, r := range got {
		if r.State != anchor.StatePending {
			t.Fatalf("a record in state %s is in the submit set", r.State)
		}
	}
	up, err := e.store.Due(e.ctx(), e.clock.Now(), DueUpgrade, BatchSize)
	if err != nil || len(up) != 0 {
		t.Fatalf("Due(upgrade) = %d rows (%v) while every claimed record waits for a later time", len(up), err)
	}
	// a pass anchors the two (the pending records that are the newest)
	if err := e.runSubmit(); err != nil {
		t.Fatal(err)
	}
	for _, id := range pending {
		if st := e.row(id).State; st != anchor.StateAnchored {
			t.Errorf("record %d is %s after a pass", id, st)
		}
	}
}

func TestClaimedRecordsWithAFutureCheckDoNotStarvePendingOnes(t *testing.T) {
	e := newEnv(t)
	pending := starve(t, e, func(upTo int64, next time.Time) {
		e.st.mu.Lock()
		defer e.st.mu.Unlock()
		for id, r := range e.st.rows {
			if id <= upTo {
				r.State, r.NextAttemptAt = anchor.StateClaimed, &next
			}
		}
	})
	checkNotStarved(t, e, pending)
}

func TestClaimedRecordsWithAFutureCheckDoNotStarvePendingOnesInPostgreSQL(t *testing.T) {
	e, admin, _ := pgEnv(t)
	pending := starve(t, e, func(upTo int64, next time.Time) {
		if _, err := admin.Exec(bg(), `UPDATE kingres_cp.ledger_anchor SET state = 'claimed', next_attempt_at = $2 WHERE id <= $1`, upTo, next); err != nil {
			t.Fatal(err)
		}
	})
	checkNotStarved(t, e, pending)
}

// One target with many due records does not take the places of another (the limit is for each target).
func TestOneTargetWithManyRecordsDoesNotTakeThePlacesOfAnother(t *testing.T) {
	for name, mk := range map[string]func(t *testing.T) *env{
		"memory": func(t *testing.T) *env { return newEnv(t) },
		"pg":     func(t *testing.T) *env { e, _, _ := pgEnv(t); return e },
	} {
		t.Run(name, func(t *testing.T) {
			e := mk(t)
			e.addWorm("big")
			for i := 0; i < BatchSize+4; i++ {
				e.putAgg(fmt.Sprintf("db%03d", i), 1)
			}
			if err := e.runEnqueue(); err != nil {
				t.Fatal(err)
			}
			small := e.addWorm("small")
			e.putAgg("late", 1)
			if err := e.runEnqueue(); err != nil {
				t.Fatal(err)
			}
			got, err := e.store.Due(e.ctx(), e.clock.Now(), DueSubmit, BatchSize)
			if err != nil {
				t.Fatal(err)
			}
			big, others := 0, 0
			for _, r := range got {
				if r.TargetID == small.ID {
					others++
				} else {
					big++
				}
			}
			if big != BatchSize || others == 0 {
				t.Fatalf("the big target has %d places and the small one %d (the limit is %d for each target)", big, others, BatchSize)
			}
		})
	}
}

// ---- P7A-04: a hung target holds one place, not the pass ----

func TestAHungTargetDoesNotHoldTheOtherTargetsOfAPass(t *testing.T) {
	e := newEnv(t)
	e.svc.cfg.SubmitTimeout = 400 * time.Millisecond
	e.svc.cfg.PassWait = 150 * time.Millisecond
	hung := e.addWorm("hung")
	e.s3.hang = make(chan struct{})
	defer close(e.s3.hang)
	good := newFakeS3()
	good.clock = e.clock.Now
	defer good.close()
	cfg := map[string]any{"endpoint": good.srv.URL, "region": fakeRegion, "bucket": fakeBucket, "allowInsecure": true, "retentionDays": 30, "mode": "compliance", "ifNoneMatch": "on"}
	healthy, err := e.svc.CreateTarget(e.ctx(), "admin", api.LedgerAnchorTargetCreate{Kind: "worm", Name: "healthy", Enabled: true, Config: mustRaw(cfg), Credentials: wormCreds()})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		e.putAgg(fmt.Sprintf("db%d", i), 1)
	}
	if err := e.runEnqueue(); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := e.runSubmit(); err != nil {
		t.Fatal(err)
	}
	took := time.Since(start)
	anchored := 0
	for _, r := range e.rows() {
		if r.TargetID == healthy.ID && r.State == anchor.StateAnchored {
			anchored++
		}
		if r.TargetID == hung.ID && r.State == anchor.StateAnchored {
			t.Fatalf("a record of the hung target is anchored")
		}
	}
	if anchored != 5 {
		t.Fatalf("%d of 5 records of the healthy target are anchored after a pass while the other target hangs", anchored)
	}
	// five records of the hung target at 400 ms each would be two seconds; the pass does not wait for them
	if took > time.Second {
		t.Fatalf("the pass took %s", took)
	}
	// the hung target keeps its worker: a second pass starts no second one for it
	n := len(e.s3.requests())
	if err := e.runSubmit(); err != nil {
		t.Fatal(err)
	}
	if got := len(e.s3.requests()); got > n+1 {
		t.Errorf("the hung target got %d more requests from a second pass", got-n)
	}
}

// ---- P7A-05: the evidence must belong to the record ----

func TestEvidenceOfAnotherCheckpointIsNotAccepted(t *testing.T) {
	// worm
	e := newEnv(t)
	e.addWorm("vault")
	e.putAgg("shop", 3)
	_ = e.runEnqueue()
	_ = e.runSubmit()
	r3, err := e.store.Record(e.ctx(), e.rows()[0].ID)
	if err != nil || r3.State != anchor.StateAnchored {
		t.Fatalf("%+v %v", r3, err)
	}
	e.putAgg("shop", 4)
	_ = e.runEnqueue()
	rs := e.rows()
	r4, _ := e.store.Record(e.ctx(), rs[len(rs)-1].ID)
	k := kinds["worm"]
	if err := k.Binds(r3, r3.Evidence); err != nil {
		t.Fatalf("the evidence of the record itself: %v", err)
	}
	if err := k.Binds(r4, r3.Evidence); err == nil {
		t.Error("worm: the evidence of size 3 binds the record of size 4")
	}
	other := r3
	other.Note = append([]byte(nil), r3.Note...)
	other.Note[len(other.Note)-3] ^= 1
	if err := k.Binds(other, r3.Evidence); err == nil {
		t.Error("worm: evidence binds a record whose note is another")
	}
	// the single gate: a receipt with the evidence of another checkpoint fails the record
	row, err := e.store.Begin(e.ctx(), e.term, r4.ID, e.clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	trow, _ := e.store.Target(e.ctx(), row.TargetID)
	b, berr := e.svc.build(trow)
	if berr != nil {
		t.Fatal(berr)
	}
	at := e.clock.Now()
	rc := anchor.Receipt{State: "anchored", Evidence: r3.Evidence, EvidenceTime: &at}
	if err := e.svc.succeed(e.ctx(), e.term, row, rc, b, trow.Revision); err != nil {
		t.Fatal(err)
	}
	got := e.row(r4.ID)
	if got.State == anchor.StateAnchored || got.State != anchor.StateFailed || got.ErrorCode != "evidence_mismatch" {
		t.Fatalf("record of size 4 after a receipt with the evidence of size 3: state %s code %q", got.State, got.ErrorCode)
	}
}

func TestEvidenceOfAnotherCheckpointDoesNotBindATsaOrOtsRecord(t *testing.T) {
	te := newTSAEnv(t)
	id := te.oneRecord(te.bundle)
	r3, _ := te.store.Record(te.ctx(), id)
	te.putAgg("shop", 4)
	_ = te.runEnqueue()
	rs := te.rows()
	r4, _ := te.store.Record(te.ctx(), rs[len(rs)-1].ID)
	k := kinds["tsa"]
	if err := k.Binds(r3, r3.Evidence); err != nil {
		t.Fatalf("tsa: the evidence of the record itself: %v", err)
	}
	if err := k.Binds(r4, r3.Evidence); err == nil {
		t.Error("tsa: the token of size 3 binds the record of size 4")
	}

	oe := newEnv(t)
	a, c := newOTSCal(t), newOTSCal(t)
	oid := oe.otsRecord(nil, a, c)
	_ = oe.runSubmit()
	o3, _ := oe.store.Record(oe.ctx(), oid)
	if len(o3.Evidence) == 0 {
		t.Fatalf("no evidence after the submission: %+v", o3)
	}
	oe.putAgg("shop", 4)
	_ = oe.runEnqueue()
	ors := oe.rows()
	o4, _ := oe.store.Record(oe.ctx(), ors[len(ors)-1].ID)
	ok := kinds["ots"]
	if err := ok.Binds(o3, o3.Evidence); err != nil {
		t.Fatalf("ots: the evidence of the record itself: %v", err)
	}
	if err := ok.Binds(o4, o3.Evidence); err == nil {
		t.Error("ots: the digest of size 3 binds the record of size 4")
	}
}

// ---- P7A-07: the result of a call is not applied to a target that was changed or switched off meanwhile ----

func TestAResultOfACallIsDroppedWhenTheTargetChangedMeanwhile(t *testing.T) {
	for name, change := range map[string]func(e *env, id int64) error{
		"switched off": func(e *env, id int64) error {
			off := false
			_, err := e.svc.UpdateTarget(e.ctx(), id, api.LedgerAnchorTargetUpdate{Enabled: &off})
			return err
		},
		"renamed (a new revision)": func(e *env, id int64) error {
			n := "other name"
			_, err := e.svc.UpdateTarget(e.ctx(), id, api.LedgerAnchorTargetUpdate{Name: &n})
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			e, id := oneRecord(t)
			done := false
			e.svc.cfg.Hook = func(point string) error {
				if point == "after-call" && !done {
					done = true
					return change(e, 1)
				}
				return nil
			}
			_ = e.runSubmit()
			r := e.row(id)
			if r.State == anchor.StateAnchored {
				t.Fatalf("the record is anchored by a call made for the old configuration: %+v", r)
			}
			if tg, _ := e.store.Target(e.ctx(), 1); tg.Revision < 2 {
				t.Fatalf("the hook did not change the target: %+v", tg)
			}
			// the hook is spent; the next pass (for a target that is still enabled) anchors it with the new configuration
			e.svc.cfg.Hook = nil
			if name != "switched off" {
				_ = e.runSubmit()
				if r := e.row(id); r.State != anchor.StateAnchored {
					t.Errorf("after the next pass: %+v", r)
				}
			}
		})
	}
}

// ---- P7A-08: an old failure does not keep the alert once a newer checkpoint is anchored ----

func TestAnOldFailureDoesNotKeepTheAlertAfterANewAnchor(t *testing.T) {
	e, id := oneRecord(t)
	e.s3.noLock = true
	_ = e.runSubmit()
	_ = e.runMonitor()
	if e.alerts.severity(AlertFailed, "target:1") != "critical" {
		t.Fatalf("no alert for the failure: %s", e.alerts.all())
	}
	e.s3.noLock = false
	e.putAgg("shop", 4)
	_ = e.runEnqueue()
	_ = e.runSubmit()
	rs := e.rows()
	if rs[len(rs)-1].State != anchor.StateAnchored {
		t.Fatalf("the newer record: %+v", rs)
	}
	_ = e.runMonitor()
	if sev := e.alerts.severity(AlertFailed, "target:1"); sev != "" {
		t.Errorf("the alert of the old failure stays (%s) after size 4 was anchored", sev)
	}
	if got, _ := e.svc.Target(e.ctx(), 1); got.State == "failed" {
		t.Errorf("the target is shown as failed: %s", got.State)
	}
	// and the obsolete record is not retried
	_, err := e.svc.Retry(e.ctx(), "shop", id)
	wantCode(t, err, problem.Conflict)
}

// ---- P7A-09: the evidence file is for an anchored record ----

func TestTheEvidenceFileIsGivenForAnAnchoredRecordOnly(t *testing.T) {
	e := newEnv(t)
	a, c := newOTSCal(t), newOTSCal(t)
	id := e.otsRecord(nil, a, c)
	_ = e.runSubmit()
	r := e.row(id)
	if r.State == anchor.StateAnchored || len(r.Evidence) == 0 {
		t.Fatalf("setup: %+v", r)
	}
	_, err := e.svc.Evidence(e.ctx(), "shop", id, "json")
	wantCode(t, err, problem.NotFound)
	if f, err := e.svc.Evidence(e.ctx(), "shop", id, "ots"); err != nil || len(f.Data) == 0 {
		t.Errorf("the ots file of a submitted record: %d bytes, %v", len(f.Data), err)
	}
	// an anchored record: the time of the file is the time of the anchoring, not of the last write
	e2, id2 := oneRecord(t)
	_ = e2.runSubmit()
	r2 := e2.row(id2)
	e2.clock.Advance(48 * time.Hour)
	f, err := e2.svc.Evidence(e2.ctx(), "shop", id2, "json")
	if err != nil {
		t.Fatal(err)
	}
	pf, err := anchor.ParseEvidenceFile(f.Data)
	if err != nil || !pf.CreatedAt.Equal(r2.AnchoredAt.Truncate(time.Second)) {
		t.Fatalf("created_at %v want %v (%v)", pf.CreatedAt, r2.AnchoredAt, err)
	}
}

// ---- P7A-10: the redactor knows the forms of a secret, and the credentials are long enough to be redacted ----

func TestTheRedactorRemovesTheFormsOfASecretAndShortCredentialsAreRefused(t *testing.T) {
	user, pass := "bob-the-user", "s3cr3t-pw/+="
	pair := user + ":" + pass
	red := anchor.NewRedactor(anchor.Secret(user), anchor.Secret(pass), anchor.Secret(pair))
	b64 := base64.StdEncoding.EncodeToString([]byte(pair))
	for _, text := range []string{
		"status: bad header Basic " + b64,
		"echo " + pass,
		"echo " + strings.ReplaceAll(strings.ReplaceAll(pass, "/", "%2F"), "+", "%2B"),
		"echo " + fmt.Sprintf("%x", pass),
		"echo " + base64.RawURLEncoding.EncodeToString([]byte(pair)),
	} {
		out := red.Clean(text, 300)
		for _, f := range []string{b64, pass, fmt.Sprintf("%x", pass), base64.RawURLEncoding.EncodeToString([]byte(pair)), "s3cr3t-pw%2F%2B%3D"} {
			if strings.Contains(out, f) {
				t.Errorf("%q still holds %q", out, f)
			}
		}
	}
	for _, in := range []string{`{"username":"bob","password":"longenough"}`, `{"username":"longenough","password":"abc"}`} {
		if c, err := ParseCreds("tsa", json.RawMessage(in)); err == nil {
			t.Errorf("%s accepted: %v", in, c)
		}
	}
	// the Basic pair of a TSA is a secret of its own for the redactor of the target
	c := Creds{"username": anchor.Secret(user), "password": anchor.Secret(pass)}
	if got := anchor.NewRedactor(c.Secrets()...).Clean("Authorization: Basic "+b64, 300); strings.Contains(got, b64[16:]) {
		t.Errorf("the pair of a target is not redacted: %s", got)
	}
}

// ---- P7A-12: the text of a credentialsPath that cannot be used says why ----

func TestTheTextOfACredentialsPathThatFailsSaysWhy(t *testing.T) {
	e := newEnv(t)
	p := filepath.Join(t.TempDir(), "creds.json")
	if err := writeFileMode(p, mustString(map[string]string{"accessKeyId": testKeyID, "secretAccessKey": testSecret})); err != nil {
		t.Fatal(err)
	}
	cfg := map[string]any{}
	_ = json.Unmarshal(e.wormConfig(nil), &cfg)
	cfg["credentialsPath"] = p
	tg, err := e.svc.CreateTarget(e.ctx(), "admin", api.LedgerAnchorTargetCreate{Kind: "worm", Name: "mount", Enabled: true, Config: mustRaw(cfg)})
	if err != nil {
		t.Fatal(err)
	}
	detail := func() string {
		res, err := e.svc.TestTarget(e.ctx(), tg.ID)
		if err != nil || len(res.Checks) == 0 {
			t.Fatalf("%v %+v", err, res)
		}
		return res.Checks[0].Detail
	}
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if d := detail(); !strings.Contains(d, "does not exist") || strings.Contains(d, "%!") {
		t.Errorf("missing file: %q", d)
	}
	if err := writeFileMode(p, "not json"); err != nil {
		t.Fatal(err)
	}
	if d := detail(); !strings.Contains(d, "not a JSON object") || strings.Contains(d, "%!") {
		t.Errorf("bad content: %q", d)
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(p, 0o644); err != nil {
			t.Fatal(err)
		}
		if d := detail(); !strings.Contains(d, "group or others") || strings.Contains(d, "%!") {
			t.Errorf("bad mode: %q", d)
		}
	}
}

// ---- P7A-03: the credentials follow the targets ----

func TestAServerThatLacksTheCredentialsFileFetchesItFromAnotherHolder(t *testing.T) {
	e, id := oneRecord(t)
	file, err := e.creds.Raw(1)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.creds.Remove(1); err != nil {
		t.Fatal(err)
	}
	// no holder has it: the record fails at once, with the cause in the record and in the alert
	_ = e.runSubmit()
	r := e.row(id)
	if r.State != anchor.StateFailed || r.ErrorCode != "target_unbuildable" || !strings.Contains(r.LastError, "no credentials file") {
		t.Fatalf("a record of a target that cannot be built: %+v", r)
	}
	_ = e.runMonitor()
	if e.alerts.severity(AlertFailed, "target:1") != "critical" || !strings.Contains(e.alerts.all(), "no credentials file") {
		t.Fatalf("alerts: %s", e.alerts.all())
	}
	// a holder has it: the same pass fetches it and anchors
	e.svc.cfg.Fetch = func(context.Context, int64) ([]byte, error) { return file, nil }
	if _, err := e.svc.Retry(e.ctx(), "shop", id); err != nil {
		t.Fatal(err)
	}
	_ = e.runSubmit()
	if r := e.row(id); r.State != anchor.StateAnchored {
		t.Fatalf("after the fetch: %+v", r)
	}
	if !e.creds.Has(1) {
		t.Error("the file was not installed")
	}
}

func TestTheCredentialsJobGivesTheHoldersTheFilesTheyLackAndRaisesWhatItCannotDo(t *testing.T) {
	e := newEnv(t)
	tg := e.addWorm("vault")
	var mu sync.Mutex
	var given, removed []int64
	failGive := false
	e.svc.cfg.Distribute = func(_ context.Context, id int64, _ string, file []byte) error {
		mu.Lock()
		defer mu.Unlock()
		if failGive {
			return errors.New("not reached: h2")
		}
		if len(file) == 0 {
			t.Error("an empty file was given")
		}
		given = append(given, id)
		return nil
	}
	e.svc.cfg.Undistribute = func(_ context.Context, id int64) error { removed = append(removed, id); return nil }
	e.svc.cfg.Holders = func(_ context.Context, ids []int64) (HolderReport, error) {
		return HolderReport{Lacking: map[int64][]string{tg.ID: {"h2"}}, Orphans: map[int64][]string{77: {"h3"}}}, nil
	}
	if err := e.svc.jobCredentials(e.ctx(), e.term); err != nil {
		t.Fatal(err)
	}
	if len(given) != 1 || given[0] != tg.ID || len(removed) != 1 || removed[0] != 77 {
		t.Fatalf("given %v removed %v", given, removed)
	}
	_ = e.runMonitor()
	if len(e.alerts.open) != 0 {
		t.Fatalf("alerts after a good pass: %s", e.alerts.all())
	}
	// the distribution fails: an alert, not only a line of the log
	failGive = true
	if err := e.svc.jobCredentials(e.ctx(), e.term); err != nil {
		t.Fatal(err)
	}
	_ = e.runMonitor()
	if e.alerts.severity(AlertFailed, "target:1|credentials") != "warning" || !strings.Contains(e.alerts.all(), "h2") {
		t.Fatalf("alerts: %s", e.alerts.all())
	}
	failGive = false
	if err := e.svc.jobCredentials(e.ctx(), e.term); err != nil {
		t.Fatal(err)
	}
	_ = e.runMonitor()
	if sev := e.alerts.severity(AlertFailed, "target:1|credentials"); sev != "" {
		t.Errorf("the alert stays after the holders have the file: %s", sev)
	}
	// this server lacks the file and no holder has it: critical for an enabled target
	if err := e.creds.Remove(tg.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.jobCredentials(e.ctx(), e.term); err != nil {
		t.Fatal(err)
	}
	_ = e.runMonitor()
	if e.alerts.severity(AlertFailed, "target:1|credentials") != "critical" {
		t.Fatalf("alerts: %s", e.alerts.all())
	}
}

func TestAFailedDistributionAtCreateIsAnAlertAndTheAnswerStaysOK(t *testing.T) {
	e := newEnv(t)
	e.svc.cfg.Distribute = func(context.Context, int64, string, []byte) error { return errors.New("not reached: h2") }
	tg := e.addWorm("vault")
	if tg.ID == 0 {
		t.Fatal("no target")
	}
	_ = e.runMonitor()
	if e.alerts.severity(AlertFailed, "target:1|credentials") == "" {
		t.Fatalf("no alert for a distribution that failed: %s", e.alerts.all())
	}
}

// ---- P7A-13: two enqueues of one checkpoint do not fail the second ----

func TestAConcurrentEnqueueOfTheSameCheckpointIsNotAnError(t *testing.T) {
	e, admin, st := pgEnv(t)
	tg := e.addWorm("vault")
	e.putAgg("shop", 3)
	aggs, err := st.Aggregates(e.ctx())
	if err != nil || len(aggs) != 1 {
		t.Fatalf("%v %v", aggs, err)
	}
	a := aggs[0]
	// a second writer holds an insert of the same (target, log, size) open while the first one looks and inserts
	tx, err := admin.Begin(e.ctx())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(bg()) }()
	pl, err := anchor.ParsePayload(a.AggregateID, a.Note)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(e.ctx(), `INSERT INTO kingres_cp.ledger_anchor (target_id, target_name, kind, log_id, database_name, aggregate_id, origin, size, payload_sha256, note, state, next_attempt_at, created_at)
		VALUES ($1, 'vault', 'worm', $2, $3, $4, $5, $6, $7, $8, 'pending', now(), now())`, tg.ID, a.LogID, a.Database, a.AggregateID, a.Origin, a.Size, pl.Digest[:], a.Note); err != nil {
		t.Fatal(err)
	}
	type res struct {
		row     Row
		created bool
		err     error
	}
	ch := make(chan res, 1)
	go func() {
		r, c, err := st.Enqueue(e.ctx(), e.term, Enqueue{Target: Target{ID: tg.ID, Name: "vault", Kind: "worm"}, Agg: a, Now: e.clock.Now()})
		ch <- res{r, c, err}
	}()
	time.Sleep(500 * time.Millisecond)
	if err := tx.Commit(e.ctx()); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-ch:
		if r.err != nil || r.created || r.row.Size != uint64(a.Size) {
			t.Fatalf("the second enqueue: %+v", r)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the enqueue did not return")
	}
}

var _ = pgx.ErrNoRows

// ---- P7A-17: the gaps that the mutants of the review showed ----

func TestTheEvidenceOfARecordIsGivenOnlyForItsDatabaseAndOnlyWhenThereIsSome(t *testing.T) {
	e, id := oneRecord(t)
	// a record that has no evidence yet
	_, err := e.svc.Evidence(e.ctx(), "shop", id, "json")
	wantCode(t, err, problem.NotFound)
	_ = e.runSubmit()
	// the database of another record
	_, err = e.svc.Evidence(e.ctx(), "other", id, "json")
	wantCode(t, err, problem.NotFound)
	if _, err := e.svc.Evidence(e.ctx(), "shop", id, "json"); err != nil {
		t.Fatal(err)
	}
}

func TestASealedCredentialsFileWithFewerIterationsThanTheMinimumIsRefused(t *testing.T) {
	e := newEnv(t)
	e.rebuild(writePassphrase(t))
	tg := e.addWorm("vault")
	if _, err := e.creds.Get(tg.ID, "worm"); err != nil {
		t.Fatalf("the file just written: %v", err)
	}
	e.creds.MinIterations = e.creds.Iterations + 1
	if _, err := e.creds.Get(tg.ID, "worm"); !errors.Is(err, ErrCredentials) {
		t.Errorf("a file sealed with fewer iterations than the minimum: %v", err)
	}
}

func TestARecordIsWorkedOnByOneWorkerOfAProcessAtATime(t *testing.T) {
	e := newEnv(t)
	if !e.svc.claim(7) || e.svc.claim(7) {
		t.Fatal("a record can be claimed twice in one process")
	}
	e.svc.release(7)
	if !e.svc.claim(7) {
		t.Fatal("a released record cannot be claimed")
	}
}

func TestTheBehindWarningComesAtThreeIntervalsAndAnHourNotBefore(t *testing.T) {
	e := newEnv(t)
	e.addWorm("vault")
	e.putAgg("shop", 1)
	_ = e.runEnqueue()
	e.s3.putStatus, e.s3.putCode = 503, "SlowDown"
	e.clock.Advance(4*time.Hour - time.Minute) // the default interval is one hour: 3 x 1 h + 1 h
	_ = e.runMonitor()
	if e.alerts.severity(AlertBehind, "target:1") != "" {
		t.Fatalf("a warning before three intervals and an hour: %s", e.alerts.all())
	}
	e.clock.Advance(2 * time.Minute)
	_ = e.runMonitor()
	if e.alerts.severity(AlertBehind, "target:1") != "warning" {
		t.Fatalf("no warning after three intervals and an hour: %s", e.alerts.all())
	}
}

func TestTheLagOfATargetIsClockedFromTheMomentItWasSwitchedOn(t *testing.T) {
	e := newEnv(t)
	e.putAgg("shop", 1)
	e.clock.Advance(10 * time.Hour) // the checkpoint is old when the target is made
	e.addWorm("vault")
	_ = e.runEnqueue()
	e.s3.putStatus, e.s3.putCode = 503, "SlowDown"
	_ = e.runMonitor()
	if e.alerts.severity(AlertBehind, "target:1") != "" {
		t.Fatalf("a target that was just switched on is behind: %s", e.alerts.all())
	}
	e.clock.Advance(4*time.Hour + time.Minute)
	_ = e.runMonitor()
	if e.alerts.severity(AlertBehind, "target:1") != "warning" {
		t.Fatalf("not behind four hours after it was switched on: %s", e.alerts.all())
	}
}

func TestAReceiptWhoseEvidenceDoesNotReadBackFailsTheRecord(t *testing.T) {
	e, id := oneRecord(t)
	row, err := e.store.Begin(e.ctx(), e.term, id, e.clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	trow, _ := e.store.Target(e.ctx(), row.TargetID)
	b, _ := e.svc.build(trow)
	at := e.clock.Now()
	rc := anchor.Receipt{State: "anchored", Evidence: json.RawMessage(`{"not":"evidence"}`), EvidenceTime: &at}
	if err := e.svc.succeed(e.ctx(), e.term, row, rc, b, trow.Revision); err != nil {
		t.Fatal(err)
	}
	if r := e.row(id); r.State == anchor.StateAnchored || r.ErrorCode != "bad_evidence" {
		t.Fatalf("%+v", r)
	}
}

func TestACallThatIsCutShortByTheEndOfTheLeadershipWritesNothing(t *testing.T) {
	e, id := oneRecord(t)
	e.s3.hang = make(chan struct{})
	defer close(e.s3.hang)
	ctx, cancel := context.WithCancel(e.ctx())
	go func() { time.Sleep(150 * time.Millisecond); cancel() }()
	_ = e.svc.jobSubmit(ctx, e.term)
	r := e.row(id)
	if r.Attempts != 0 || r.State != anchor.StatePending || r.LastError != "" {
		t.Fatalf("a call that was cut short by the end of the leadership was written as a failure: %+v", r)
	}
}
