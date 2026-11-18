// SPDX-License-Identifier: AGPL-3.0-only

package anchor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"
)

// goldenTable is the state machine as the design means it, written out by
// hand: the state, then for every event the next state or "-" (refused). The
// test compares it with the table that the code uses AND runs every pair
// through the methods of Record, so a change of the table, of a method or of
// this text each makes it fail.
const goldenTable = `
pending    receipt_submitted=submitted receipt_claimed=- receipt_anchored=anchored error_wait=pending error_fail=failed error_conflict=failed supersede=superseded expire=- operator_retry=-
submitted  receipt_submitted=submitted receipt_claimed=claimed receipt_anchored=anchored error_wait=submitted error_fail=failed error_conflict=failed supersede=- expire=expired operator_retry=-
claimed    receipt_submitted=submitted receipt_claimed=claimed receipt_anchored=anchored error_wait=claimed error_fail=failed error_conflict=failed supersede=- expire=expired operator_retry=-
anchored   receipt_submitted=- receipt_claimed=- receipt_anchored=- error_wait=- error_fail=- error_conflict=- supersede=- expire=- operator_retry=-
failed     receipt_submitted=- receipt_claimed=- receipt_anchored=- error_wait=- error_fail=- error_conflict=- supersede=- expire=- operator_retry=pending
superseded receipt_submitted=- receipt_claimed=- receipt_anchored=- error_wait=- error_fail=- error_conflict=- supersede=- expire=- operator_retry=-
expired    receipt_submitted=- receipt_claimed=- receipt_anchored=- error_wait=- error_fail=- error_conflict=- supersede=- expire=- operator_retry=pending
`

func tableText() string {
	var sb strings.Builder
	sb.WriteString("\n")
	for _, s := range []State{StatePending, StateSubmitted, StateClaimed, StateAnchored, StateFailed, StateSuperseded, StateExpired} {
		sb.WriteString(fmt.Sprintf("%-10s", s))
		for _, ev := range Events {
			to, ok := Next(s, ev)
			v := "-"
			if ok {
				v = string(to)
			}
			sb.WriteString(fmt.Sprintf(" %s=%s", ev, v))
		}
		sb.WriteString("\n")
	}
	return sb.String()
}

func TestStateTableGolden(t *testing.T) {
	if got := tableText(); got != goldenTable {
		t.Fatalf("the transition table changed:\n%s\nwant:\n%s", got, goldenTable)
	}
	if len(States) != 7 || len(Events) != 9 {
		t.Fatal("states or events changed")
	}
	// every state of the table is a state, every target too, nothing is left over
	for s, row := range transitions {
		if !s.Valid() {
			t.Errorf("table row for unknown state %q", s)
		}
		for ev, to := range row {
			if !to.Valid() {
				t.Errorf("%s/%s leads to unknown state %q", s, ev, to)
			}
			found := false
			for _, e := range Events {
				found = found || e == ev
			}
			if !found {
				t.Errorf("table has the unknown event %q", ev)
			}
		}
	}
}

// drive applies the event to the record through the method of Record that
// belongs to it, as the wiring would.
func drive(r Record, ev Event) (Record, error) {
	now := t0.Add(48 * time.Hour)
	switch ev {
	case EventReceiptSubmitted:
		return r.ApplyReceipt(Receipt{State: ReceiptSubmitted, Evidence: json.RawMessage(`{}`)}, now)
	case EventReceiptClaimed:
		return r.ApplyReceipt(Receipt{State: ReceiptClaimed, Evidence: json.RawMessage(`{}`)}, now)
	case EventReceiptAnchored:
		return r.ApplyReceipt(Receipt{State: ReceiptAnchored, Evidence: json.RawMessage(`{}`)}, now)
	case EventWait:
		out, alert, err := r.AfterError(Errorf(ClassRetryable, "x", "y", "z", "down"), now, 0.5, Limits{})
		if err == nil && alert != AlertNone {
			return out, errors.New("alert on a wait")
		}
		return out, err
	case EventFail:
		out, alert, err := r.AfterError(Errorf(ClassPermanent, "x", "y", "z", "bad"), now, 0.5, Limits{})
		if err == nil && alert != AlertFailed {
			return out, errors.New("no alert on a failure")
		}
		return out, err
	case EventConflict:
		out, alert, err := r.AfterError(Errorf(ClassConflict, "x", "y", "z", "other"), now, 0.5, Limits{})
		if err == nil && alert != AlertConflict {
			return out, errors.New("no conflict alert")
		}
		return out, err
	case EventSupersede:
		return r.Supersede()
	case EventExpire:
		out, did := r.ExpireIfDue(now, time.Hour, time.Hour)
		if !did {
			return r, ErrNotAllowed
		}
		return out, nil
	case EventRetry:
		return r.Retry(now)
	}
	return r, fmt.Errorf("event %q has no driver", ev)
}

func TestStateMachineIsTotal(t *testing.T) {
	for _, s := range States {
		for _, ev := range Events {
			r := NewRecord(1, "agg", 5, [32]byte{1}, t0)
			r.State = s
			r.LastError = "before"
			before := r
			out, err := drive(r, ev)
			want, ok := Next(s, ev)
			switch {
			case ok && err != nil:
				t.Errorf("%s + %s: table says %s, method refused: %v", s, ev, want, err)
			case ok && out.State != want:
				t.Errorf("%s + %s: got %s, table says %s", s, ev, out.State, want)
			case !ok && err == nil:
				t.Errorf("%s + %s: table refuses, method moved to %s", s, ev, out.State)
			case !ok && !sameRecord(out, before):
				t.Errorf("%s + %s: refused but the record changed: %+v", s, ev, out)
			}
			if !sameRecord(r, before) {
				t.Errorf("%s + %s: the argument was changed (the methods are pure)", s, ev)
			}
		}
	}
}

func sameRecord(a, b Record) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

func TestContractStatusAndTerminal(t *testing.T) {
	m := map[State]string{StatePending: "pending", StateSubmitted: "pending", StateClaimed: "pending", StateAnchored: "anchored", StateFailed: "failed", StateExpired: "failed"}
	for s, want := range m {
		if got, ok := s.ContractStatus(); !ok || got != want {
			t.Errorf("%s -> %s", s, got)
		}
	}
	if _, ok := StateSuperseded.ContractStatus(); ok {
		t.Fatal("superseded is shown")
	}
	for _, s := range States {
		if !s.Valid() {
			t.Fatal(s)
		}
		wantTerm := s == StateAnchored || s == StateFailed || s == StateSuperseded || s == StateExpired
		if s.Terminal() != wantTerm {
			t.Errorf("terminal %s", s)
		}
	}
	if State("x").Valid() {
		t.Fatal("unknown state valid")
	}
	if _, ok := Next("x", EventWait); ok {
		t.Fatal("unknown state has a transition")
	}
}

func TestDueByState(t *testing.T) {
	r := NewRecord(1, "agg", 5, [32]byte{}, t0)
	if !r.Due(t0) {
		t.Fatal("pending")
	}
	later := t0.Add(time.Minute)
	for _, s := range States {
		x := r
		x.State, x.NextAttemptAt = s, &later
		want := s == StatePending || s == StateSubmitted || s == StateClaimed
		if x.Due(t0) || x.Due(later) != want {
			t.Errorf("%s: due before=%v at=%v", s, x.Due(t0), x.Due(later))
		}
		x.NextAttemptAt = nil
		// a pending or submitted record with no date is due; a claimed one is not
		if x.Due(t0) != (s == StatePending || s == StateSubmitted) {
			t.Errorf("%s without a date: %v", s, x.Due(t0))
		}
	}
}

func TestAfterError(t *testing.T) {
	r := NewRecord(1, "agg", 5, [32]byte{}, t0)
	retryable := Errorf(ClassRetryable, "tsa", "post", "x", "down")
	r1, alert, err := r.AfterError(retryable, t0, 0.5, Limits{})
	if err != nil || r1.State != StatePending || r1.Attempts != 1 || r1.Retryables != 1 || alert != AlertNone ||
		!r1.NextAttemptAt.Equal(t0.Add(30*time.Second)) || !strings.Contains(r1.LastError, "down") || r1.FailingSince == nil || !r1.FailingSince.Equal(t0) {
		t.Fatalf("%+v %v", r1, err)
	}
	later := t0.Add(time.Hour)
	r2, _, _ := r1.AfterError(retryable, later, 0.5, Limits{})
	if !r2.NextAttemptAt.Equal(later.Add(time.Minute)) || r2.Attempts != 2 || !r2.FailingSince.Equal(t0) {
		t.Fatalf("%+v", r2)
	}
	withRA := &Error{Class: ClassRetryable, Target: "worm", Text: "slow", RetryAfter: 2 * time.Hour}
	if r3, _, _ := r.AfterError(withRA, t0, 0.5, Limits{}); !r3.NextAttemptAt.Equal(t0.Add(2 * time.Hour)) {
		t.Fatal("retry after")
	}
	// a submitted and a claimed record stay what they are on a retryable error
	for _, st := range []State{StateSubmitted, StateClaimed} {
		s := r
		s.State = st
		if s1, _, err := s.AfterError(retryable, t0, 0.5, Limits{}); err != nil || s1.State != st {
			t.Fatalf("%s: state changed to %s (%v)", st, s1.State, err)
		}
	}
	// refused: 1, 5, 15 minutes, then failed
	refused := Errorf(ClassRefused, "worm", "put", "denied", "refused")
	cur := r
	for i, d := range []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute} {
		var a string
		cur, a, err = cur.AfterError(refused, t0, 0.5, Limits{})
		if err != nil || cur.State != StatePending || a != AlertNone || !cur.NextAttemptAt.Equal(t0.Add(d)) || cur.Attempts != i+1 || cur.Refusals != i+1 {
			t.Fatalf("refusal %d: %+v %s", i+1, cur, a)
		}
	}
	cur, a, _ := cur.AfterError(refused, t0, 0.5, Limits{})
	if cur.State != StateFailed || a != AlertFailed || cur.NextAttemptAt != nil || cur.Attempts != 4 || cur.Refusals != 4 {
		t.Fatalf("%+v %s", cur, a)
	}
	// permanent and conflict fail at once
	if p, a, _ := r.AfterError(Errorf(ClassPermanent, "worm", "verify", "not_locked", "no"), t0, 0.5, Limits{}); p.State != StateFailed || a != AlertFailed {
		t.Fatal("permanent")
	}
	if p, a, _ := r.AfterError(Errorf(ClassConflict, "worm", "put", "worm_conflict", "no"), t0, 0.5, Limits{}); p.State != StateFailed || a != AlertConflict {
		t.Fatal("conflict")
	}
	// an unknown error is permanent; a context error is retryable
	if p, a, _ := r.AfterError(errors.New("boom"), t0, 0.5, Limits{}); p.State != StateFailed || a != AlertFailed {
		t.Fatal("unknown")
	}
	if p, _, _ := r.AfterError(context.DeadlineExceeded, t0, 0.5, Limits{}); p.State != StatePending {
		t.Fatal("deadline")
	}
	// the stored text is cut and ASCII
	long := Errorf(ClassRetryable, "x", "y", "z", "%s\n\xff", strings.Repeat("a", 2000))
	if p, _, _ := r.AfterError(long, t0, 0.5, Limits{}); len(p.LastError) != MaxLastError || strings.ContainsAny(p.LastError, "\n\xff") {
		t.Fatalf("%d", len(p.LastError))
	}
}

// A1-04: the fourth refusal in a row fails the record; retryable failures
// before it do not count, and a retryable failure after refusals waits the
// backoff of the first retryable failure, not of the sum.
func TestRefusalCountsRefusalsOnly(t *testing.T) {
	retryable := Errorf(ClassRetryable, "tsa", "post", "x", "down")
	refused := Errorf(ClassRefused, "worm", "put", "denied", "refused")
	r := NewRecord(1, "agg", 5, [32]byte{}, t0)
	for i := 0; i < 3; i++ {
		r, _, _ = r.AfterError(retryable, t0, 0.5, Limits{})
	}
	r, a, _ := r.AfterError(refused, t0, 0.5, Limits{})
	if r.State != StatePending || a != AlertNone || !r.NextAttemptAt.Equal(t0.Add(time.Minute)) || r.Refusals != 1 {
		t.Fatalf("first refusal after three retryable failures: %+v %s", r, a)
	}
	// three refusals, then one retryable failure: 30 seconds, not 4 minutes
	r = NewRecord(1, "agg", 5, [32]byte{}, t0)
	for i := 0; i < 3; i++ {
		r, _, _ = r.AfterError(refused, t0, 0.5, Limits{})
	}
	r, _, _ = r.AfterError(retryable, t0, 0.5, Limits{})
	if !r.NextAttemptAt.Equal(t0.Add(30*time.Second)) || r.Refusals != 0 || r.Retryables != 1 {
		t.Fatalf("retryable after refusals: %+v", r)
	}
	// the budget starts again after the interruption: three more refusals are waited out
	for i := 0; i < 3; i++ {
		r, a, _ = r.AfterError(refused, t0, 0.5, Limits{})
		if r.State != StatePending || a != AlertNone {
			t.Fatalf("refusal %d after the interruption: %+v %s", i+1, r, a)
		}
	}
	r, a, _ = r.AfterError(refused, t0, 0.5, Limits{})
	if r.State != StateFailed || a != AlertFailed {
		t.Fatalf("fourth refusal in a row: %+v %s", r, a)
	}
	// a success clears the counters
	r = NewRecord(1, "agg", 5, [32]byte{}, t0)
	r, _, _ = r.AfterError(refused, t0, 0.5, Limits{})
	r, err := r.ApplyReceipt(Receipt{State: ReceiptSubmitted, Evidence: json.RawMessage(`{}`)}, t0)
	if err != nil || r.Refusals != 0 || r.Attempts != 0 || r.Retryables != 0 || r.FailingSince != nil {
		t.Fatalf("%+v %v", r, err)
	}
}

// A1-05: a late failure of a stale job does not move a record out of a final
// state; the record is returned unchanged with an error and no alert.
func TestAfterErrorRefusesFinalStates(t *testing.T) {
	errs := []error{
		Errorf(ClassPermanent, "x", "y", "z", "bad"),
		Errorf(ClassConflict, "x", "y", "z", "bad"),
		Errorf(ClassRefused, "x", "y", "z", "bad"),
		Errorf(ClassRetryable, "x", "y", "z", "bad"),
	}
	for _, s := range []State{StateAnchored, StateSuperseded, StateExpired, StateFailed} {
		for _, e := range errs {
			r := NewRecord(1, "agg", 5, [32]byte{}, t0)
			r.State, r.LastError = s, "before"
			out, alert, err := r.AfterError(e, t0, 0.5, Limits{})
			if err == nil || !errors.Is(err, ErrNotAllowed) || alert != AlertNone || !sameRecord(out, r) {
				t.Errorf("%s + %v: %+v %q %v", s, ClassOf(e), out, alert, err)
			}
		}
	}
	if _, _, err := NewRecord(1, "agg", 5, [32]byte{}, t0).AfterError(nil, t0, 0.5, Limits{}); err == nil {
		t.Fatal("a nil error did not give an error (or panicked)")
	}
}

// A1-06: retryable failures end at the attempt limit and at the age limit,
// as a failure with the alert; the default limits apply when none is given.
func TestRetryableFailuresAreBounded(t *testing.T) {
	retryable := &Error{Class: ClassRetryable, Target: "worm", Text: "unavailable", RetryAfter: 24 * time.Hour}
	// the attempt limit: the failure that exceeds it fails the record
	lim := Limits{MaxAttempts: 3, MaxAge: 1000 * time.Hour}
	r := NewRecord(1, "agg", 5, [32]byte{}, t0)
	now := t0
	for i := 1; i <= 3; i++ {
		var a string
		r, a, _ = r.AfterError(retryable, now, 0.5, lim)
		if r.State != StatePending || a != AlertNone {
			t.Fatalf("attempt %d: %+v %s", i, r, a)
		}
		now = now.Add(time.Minute)
	}
	r, a, _ := r.AfterError(retryable, now, 0.5, lim)
	if r.State != StateFailed || a != AlertFailed || r.NextAttemptAt != nil || !strings.Contains(r.LastError, "unavailable") {
		t.Fatalf("attempt limit: %+v %s", r, a)
	}
	// the age limit counts from the first failure of the streak
	lim = Limits{MaxAttempts: 1000, MaxAge: 48 * time.Hour}
	r = NewRecord(1, "agg", 5, [32]byte{}, t0)
	r, _, _ = r.AfterError(retryable, t0, 0.5, lim)
	r, a, _ = r.AfterError(retryable, t0.Add(48*time.Hour-time.Second), 0.5, lim)
	if r.State != StatePending || a != AlertNone {
		t.Fatalf("just inside the age limit: %+v %s", r, a)
	}
	r, a, _ = r.AfterError(retryable, t0.Add(48*time.Hour), 0.5, lim)
	if r.State != StateFailed || a != AlertFailed {
		t.Fatalf("age limit: %+v %s", r, a)
	}
	// a success restarts the streak: the age counts from the next first failure
	r = NewRecord(1, "agg", 5, [32]byte{}, t0)
	r, _, _ = r.AfterError(retryable, t0, 0.5, lim)
	r, _ = r.ApplyReceipt(Receipt{State: ReceiptSubmitted, Evidence: json.RawMessage(`{}`)}, t0)
	r, a, _ = r.AfterError(retryable, t0.Add(100*time.Hour), 0.5, lim)
	if r.State != StateSubmitted || a != AlertNone {
		t.Fatalf("streak not restarted: %+v %s", r, a)
	}
	// the defaults
	d := Limits{}.WithDefaults()
	if d.MaxAttempts != DefaultMaxAttempts || d.MaxAge != DefaultMaxAge {
		t.Fatal(d)
	}
	r = NewRecord(1, "agg", 5, [32]byte{}, t0)
	r, _, _ = r.AfterError(retryable, t0, 0.5, Limits{})
	r, a, _ = r.AfterError(retryable, t0.Add(DefaultMaxAge), 0.5, Limits{})
	if r.State != StateFailed || a != AlertFailed {
		t.Fatalf("default age limit: %+v %s", r, a)
	}
}

func TestRefusedRetryDelay(t *testing.T) {
	got := []time.Duration{RefusedRetryDelay(0), RefusedRetryDelay(1), RefusedRetryDelay(2), RefusedRetryDelay(3), RefusedRetryDelay(4)}
	want := []time.Duration{0, time.Minute, 5 * time.Minute, 15 * time.Minute, 0}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("delay %d: %v", i, got[i])
		}
	}
}

func TestEventsAreDistinct(t *testing.T) {
	seen := map[Event]bool{}
	var names []string
	for _, e := range Events {
		if seen[e] {
			t.Errorf("event %q listed twice", e)
		}
		seen[e] = true
		names = append(names, string(e))
	}
	sort.Strings(names)
	if len(names) != 9 {
		t.Fatal(names)
	}
}

// A stored copy of evidence may come back from the database in another spelling
// (a jsonb column reorders the members and adds white space); CanonicalEvidence
// reads it and gives the bytes that the strict parsers need.
func TestCanonicalEvidenceFromAnotherSpelling(t *testing.T) {
	canon, err := EncodeEvidence(goldenWORM)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(canon, &m); err != nil {
		t.Fatal(err)
	}
	loose, _ := json.MarshalIndent(m, "", "  ") // sorted members, white space
	if _, err := ParseWORMEvidence(loose); err == nil {
		t.Fatal("the strict parser accepted another spelling")
	}
	got, err := CanonicalEvidence(KindWORM, loose)
	if err != nil || string(got) != string(canon) {
		t.Fatalf("%s %v", got, err)
	}
	if _, err := ParseWORMEvidence(got); err != nil {
		t.Fatal(err)
	}
	for name, bad := range map[string]string{
		"unknown-member": strings.Replace(string(loose), `"bucket"`, `"extra": 1, "bucket"`, 1),
		"wrong-kind":     string(loose),
	} {
		kind := KindWORM
		if name == "wrong-kind" {
			kind = KindTSA
		}
		if _, err := CanonicalEvidence(kind, []byte(bad)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if _, err := CanonicalEvidence("nope", loose); err == nil {
		t.Error("unknown kind accepted")
	}
}
