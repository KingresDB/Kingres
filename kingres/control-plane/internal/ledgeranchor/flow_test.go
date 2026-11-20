// SPDX-License-Identifier: AGPL-3.0-only

package ledgeranchor

import (
	"bytes"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/anchor"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/scheduler"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/store"
)

func wantCode(t *testing.T, err error, code problem.Code) {
	t.Helper()
	var p *problem.Problem
	if !errors.As(err, &p) || p.Code != code {
		t.Fatalf("want problem %s, got %v", code, err)
	}
}

// The whole way of a checkpoint: enqueue, submit, anchored; what is stored at the store is the signed note; the
// evidence is the library's, canonical, and the record says what the store vouched for.
func TestACheckpointIsAnchoredAtAWormStore(t *testing.T) {
	e := newEnv(t)
	tg := e.addWorm("vault")
	e.putAgg("shop", 3)
	if err := e.runEnqueue(); err != nil {
		t.Fatal(err)
	}
	rs := e.rows()
	if len(rs) != 1 || rs[0].State != anchor.StatePending || rs[0].Size != 3 || rs[0].TargetID != tg.ID {
		t.Fatalf("after enqueue: %+v", rs)
	}
	if !rs[0].SubmitStart.IsZero() {
		t.Error("the start of the first submission is set before any submission")
	}
	if err := e.runSubmit(); err != nil {
		t.Fatal(err)
	}
	r := e.rows()[0]
	if r.State != anchor.StateAnchored || r.AnchoredAt == nil || r.EvidenceTime == nil || r.Attempts != 0 || r.SubmitStart.IsZero() {
		t.Fatalf("after submit: %+v", r)
	}
	ev, err := anchor.ParseWORMEvidence(r.Evidence)
	if err != nil {
		t.Fatalf("the evidence does not read back: %v", err)
	}
	vs := e.s3.stored(ev.Key)
	if len(vs) != 1 || !bytes.Equal(vs[0].body, makeNote(r.Origin, 3)) {
		t.Fatalf("the store holds %d versions, first %q", len(vs), vs[0].body)
	}
	if ev.LockMode != "COMPLIANCE" || ev.RecordedBy != "cluster" || ev.Found {
		t.Errorf("evidence: %+v", ev)
	}
	if want := hex.EncodeToString(r.PayloadSHA256[:]); want == "" {
		t.Fatal("no digest")
	}
	// the list and the evidence file
	pg, err := e.svc.List(e.ctx(), "shop", 0, "", store.Page{Limit: 10})
	if err != nil || len(pg.Items) != 1 || pg.Items[0].State != "anchored" || pg.Items[0].TargetName != "vault" {
		t.Fatalf("list: %+v %v", pg, err)
	}
	f, err := e.svc.Evidence(e.ctx(), "shop", r.ID, "json")
	if err != nil {
		t.Fatal(err)
	}
	pf, err := anchor.ParseEvidenceFile(f.Data)
	if err != nil || pf.Kind != "worm" || pf.Size != 3 || pf.Origin != r.Origin {
		t.Fatalf("the evidence file: %+v %v", pf, err)
	}
	// a second pass changes nothing and calls nothing
	n := len(e.s3.requests())
	if err := e.runSubmit(); err != nil || len(e.s3.requests()) != n {
		t.Errorf("a second pass made %d requests more (%v)", len(e.s3.requests())-n, err)
	}
	// state of the target
	got, err := e.svc.Target(e.ctx(), tg.ID)
	if err != nil || got.State != "ok" || got.NewestAnchoredAt == nil || !got.CredentialsSet {
		t.Errorf("target: %+v %v", got, err)
	}
}

func TestOnlyTheNewestCheckpointIsPendingAndTheOlderOneIsSuperseded(t *testing.T) {
	e := newEnv(t)
	e.addWorm("vault")
	e.putAgg("shop", 1)
	if err := e.runEnqueue(); err != nil {
		t.Fatal(err)
	}
	e.putAgg("shop", 2)
	e.putAgg("shop", 5)
	if err := e.runEnqueue(); err != nil {
		t.Fatal(err)
	}
	rs := e.rows()
	if len(rs) != 2 || rs[0].State != anchor.StateSuperseded || rs[1].State != anchor.StatePending || rs[1].Size != 5 {
		t.Fatalf("rows: %+v", rs)
	}
	// the same checkpoint again creates nothing
	if err := e.runEnqueue(); err != nil || len(e.rows()) != 2 {
		t.Fatalf("a repeated enqueue: %d rows (%v)", len(e.rows()), err)
	}
	if err := e.runSubmit(); err != nil {
		t.Fatal(err)
	}
	if e.rows()[0].State != anchor.StateSuperseded || e.rows()[1].State != anchor.StateAnchored {
		t.Fatalf("after submit: %+v", e.rows())
	}
	if len(e.s3.objects) != 1 {
		t.Errorf("the superseded checkpoint was submitted too: %d keys", len(e.s3.objects))
	}
	// a target that was down for days: still one pending record, not a backlog
	e.s3.putStatus, e.s3.putCode = 503, "SlowDown"
	for i := int64(6); i < 12; i++ {
		e.putAgg("shop", i)
		e.clock.Advance(24 * time.Hour)
		_ = e.runEnqueue()
		_ = e.runSubmit()
	}
	pending := 0
	for _, r := range e.rows() {
		if r.State == anchor.StatePending {
			pending++
		}
	}
	if pending != 1 {
		t.Errorf("%d pending records after days of downtime, want 1", pending)
	}
}

func TestEnqueueOnlyForEnabledTargetsAndAnchorNowAnswersTheRecords(t *testing.T) {
	e := newEnv(t)
	on := e.addWorm("vault")
	off, err := e.svc.CreateTarget(e.ctx(), "admin", api.LedgerAnchorTargetCreate{Kind: "worm", Name: "cold", Config: e.wormConfig(nil)})
	if err != nil || off.Enabled {
		t.Fatalf("a target is created disabled by default: %+v %v", off, err)
	}
	_, err = e.svc.AnchorNow(e.ctx(), "shop")
	wantCode(t, err, problem.LedgerNotPublished)
	e.putAgg("shop", 4)
	_, err = e.svc.AnchorNow(e.ctx(), "nope")
	wantCode(t, err, problem.NotFound)
	l, err := e.svc.AnchorNow(e.ctx(), "shop")
	if err != nil || len(l.Items) != 1 || l.Items[0].State != "pending" || *l.Items[0].TargetID != on.ID || l.Items[0].AggregateSize != 4 {
		t.Fatalf("anchor now: %+v %v", l, err)
	}
	// again: the same record, not a second one
	l2, err := e.svc.AnchorNow(e.ctx(), "shop")
	if err != nil || len(l2.Items) != 1 || l2.Items[0].ID != l.Items[0].ID || len(e.rows()) != 1 {
		t.Fatalf("anchor now twice: %+v %v", l2, err)
	}
	if _, err := e.svc.UpdateTarget(e.ctx(), on.ID, api.LedgerAnchorTargetUpdate{Enabled: ptr(false)}); err != nil {
		t.Fatal(err)
	}
	_, err = e.svc.AnchorNow(e.ctx(), "shop")
	wantCode(t, err, problem.Conflict)
	// a disabled target is not worked on
	if err := e.runSubmit(); err != nil || e.rows()[0].State != anchor.StatePending {
		t.Fatalf("a record of a disabled target was submitted: %+v %v", e.rows()[0], err)
	}
}

func ptr[T any](v T) *T { return &v }

func TestAFollowerAndADeposedLeaderWriteNothing(t *testing.T) {
	e := newEnv(t)
	e.addWorm("vault")
	e.putAgg("shop", 1)
	e.leading = false
	for name, f := range map[string]func() error{
		"anchor now": func() error { _, err := e.svc.AnchorNow(e.ctx(), "shop"); return err },
		"create": func() error {
			_, err := e.svc.CreateTarget(e.ctx(), "a", api.LedgerAnchorTargetCreate{Kind: "worm", Name: "x", Config: e.wormConfig(nil)})
			return err
		},
		"test":   func() error { _, err := e.svc.TestTarget(e.ctx(), 1); return err },
		"delete": func() error { return e.svc.DeleteTarget(e.ctx(), 1) },
	} {
		wantCode(t, f(), problem.ServiceUnavailable)
		_ = name
	}
	if len(e.rows()) != 0 {
		t.Fatal("a follower made a record")
	}
	// a deposed leader: it believes it holds term 1, the stored term is 2
	e.leading = true
	e.st.SetTerm(2)
	if err := e.runEnqueue(); !errors.Is(err, scheduler.ErrDeposed) {
		t.Fatalf("enqueue under a deposed term: %v", err)
	}
	if len(e.rows()) != 0 {
		t.Fatal("a deposed leader made a record")
	}
	e.st.SetTerm(1)
	if err := e.runEnqueue(); err != nil {
		t.Fatal(err)
	}
	e.st.SetTerm(2)
	before := len(e.s3.requests())
	if err := e.runSubmit(); !errors.Is(err, scheduler.ErrDeposed) {
		t.Fatalf("submit under a deposed term: %v", err)
	}
	if len(e.s3.requests()) != before || e.rows()[0].State != anchor.StatePending || !e.rows()[0].SubmitStart.IsZero() {
		t.Errorf("a deposed leader called the store or wrote a start: %+v", e.rows()[0])
	}
}

func TestRetryOfAFailedRecordAndNoOtherState(t *testing.T) {
	e := newEnv(t)
	e.addWorm("vault")
	e.putAgg("shop", 2)
	if err := e.runEnqueue(); err != nil {
		t.Fatal(err)
	}
	id := e.rows()[0].ID
	_, err := e.svc.Retry(e.ctx(), "shop", id)
	wantCode(t, err, problem.Conflict) // pending
	_, err = e.svc.Retry(e.ctx(), "other", id)
	wantCode(t, err, problem.NotFound)
	// the store lacks object lock: a permanent failure
	e.s3.noLock = true
	if err := e.runSubmit(); err != nil {
		t.Fatal(err)
	}
	r := e.row(id)
	if r.State != anchor.StateFailed || r.Alert != AlertFailed || r.ErrorCode == "" {
		t.Fatalf("failed record: %+v", r)
	}
	start := r.SubmitStart
	if start.IsZero() {
		t.Fatal("a failed submission has no start")
	}
	e.s3.noLock = false
	got, err := e.svc.Retry(e.ctx(), "shop", id)
	if err != nil || got.State != "pending" || got.Attempts != 0 || got.LastError != nil {
		t.Fatalf("retry: %+v %v", got, err)
	}
	r = e.row(id)
	if r.Generation != 0 || !r.SubmitStart.Equal(start) || r.Alert != "" {
		t.Errorf("a retry changed the start or the generation, or kept the alert: %+v", r)
	}
	if err := e.runSubmit(); err != nil {
		t.Fatal(err)
	}
	if e.row(id).State != anchor.StateAnchored {
		t.Fatalf("after the retry: %+v", e.row(id))
	}
	// the object of the first attempt (written without a lock) was completed, not duplicated
	_, err = e.svc.Retry(e.ctx(), "shop", id)
	wantCode(t, err, problem.Conflict) // anchored
	if n := len(e.s3.objects); n != 1 {
		t.Errorf("%d keys in the store", n)
	}
	_ = strings.TrimSpace
}
