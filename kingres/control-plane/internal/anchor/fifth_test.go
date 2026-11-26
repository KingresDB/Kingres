// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package anchor

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

// Tests of the fifth review that belong to this package (A5-03, A5-09; corrections
// 105 and 106): the final refusal, the start and the generation in the record, the
// payload built from the record, and the bounds of the evidence per calendar.

// A5-09 (1): a refusal whose verdict is final fails the record at once with the
// alert of a failed record; a refusal that can heal keeps its three waits.
func TestFinalRefusalFailsAtOnce(t *testing.T) {
	final := Errorf(ClassRefused, KindWORM, "verify", "store_time_skew", "x")
	final.Final = true
	if !IsFinal(final) || IsFinal(Errorf(ClassRefused, KindWORM, "verify", "store_time_skew", "x")) || IsFinal(nil) {
		t.Fatal("IsFinal")
	}
	r := NewRecord(1, "agg", 5, [32]byte{}, t0)
	out, alert, err := r.AfterError(final, t0, 0.5, Limits{})
	if err != nil || out.State != StateFailed || alert != AlertFailed || out.Refusals != 1 || out.NextAttemptAt != nil {
		t.Fatalf("%+v %q %v", out, alert, err)
	}
	// the control: the same refusal that is not final waits 1, 5 and 15 minutes
	// and fails on the fourth
	soft := Errorf(ClassRefused, KindWORM, "verify", "store_time_skew", "x")
	cur := r
	for i, wait := range []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute} {
		var a string
		cur, a, err = cur.AfterError(soft, t0, 0.5, Limits{})
		if err != nil || cur.State != StatePending || a != AlertNone || cur.NextAttemptAt == nil || !cur.NextAttemptAt.Equal(t0.Add(wait)) {
			t.Fatalf("refusal %d: %+v %q %v", i+1, cur, a, err)
		}
	}
	if cur, a, _ := cur.AfterError(soft, t0, 0.5, Limits{}); cur.State != StateFailed || a != AlertFailed {
		t.Fatalf("%+v %q", cur, a)
	}
	// a final error of another class is not changed by the marker
	perm := Errorf(ClassRetryable, KindWORM, "verify", "x", "x")
	perm.Final = true
	if out, _, _ := r.AfterError(perm, t0, 0.5, Limits{}); out.State != StatePending {
		t.Fatalf("a retryable error became final: %+v", out)
	}
}

// A5-09 (3): the start and the generation are in the record, the start is set
// once and never changes, and the payload is built from the record.
func TestRecordHoldsTheStartAndTheGeneration(t *testing.T) {
	n := note("17", root32(), sigLine)
	p0, err := ParsePayload("ab12", n)
	if err != nil {
		t.Fatal(err)
	}
	r := NewRecord(1, "ab12", p0.Size, p0.Digest, t0)
	if !r.SubmitStart.IsZero() || r.Generation != 0 {
		t.Fatalf("%+v", r)
	}
	if _, err := PayloadFor(r, "ab12", n); err == nil {
		t.Fatal("a payload was built from a record without a start")
	}
	if _, err := r.WithSubmitStart(time.Time{}); err == nil {
		t.Fatal("a zero start accepted")
	}
	s := t0.Add(1500 * time.Millisecond)
	r, err = r.WithSubmitStart(s)
	if err != nil || !r.SubmitStart.Equal(t0.Add(time.Second)) {
		t.Fatalf("%v %v", r.SubmitStart, err)
	}
	if again, err := r.WithSubmitStart(s); err != nil || !again.SubmitStart.Equal(r.SubmitStart) {
		t.Fatalf("the same start was refused: %v", err)
	}
	if _, err := r.WithSubmitStart(s.Add(time.Hour)); err == nil {
		t.Fatal("a start that changes was accepted")
	}
	p, err := PayloadFor(r, "ab12", n)
	if err != nil || !p.SubmitStart.Equal(r.SubmitStart) || p.Generation != 0 || p.Check() != nil {
		t.Fatalf("%+v %v", p, err)
	}
	// the record's values survive every transition that is not a new generation
	failed, _, _ := r.AfterError(Errorf(ClassPermanent, KindWORM, "x", "x", "x"), t0, 0.5, Limits{})
	retried, err := failed.Retry(t0.Add(time.Hour))
	if err != nil || !retried.SubmitStart.Equal(r.SubmitStart) || retried.Generation != 0 {
		t.Fatalf("Retry changed the start or the generation: %+v %v", retried, err)
	}
	// a note that is not the record's checkpoint is refused
	other := note("18", root32(), sigLine)
	if _, err := PayloadFor(r, "ab12", other); err == nil {
		t.Fatal("a note of another size was accepted")
	}
	r2 := r
	r2.PayloadSHA256[0] ^= 1
	if _, err := PayloadFor(r2, "ab12", n); err == nil {
		t.Fatal("a note with another digest was accepted")
	}
	// the generation of the record is the payload's
	g := r
	g.Generation = 3
	if p, err := PayloadFor(g, "ab12", n); err != nil || p.Generation != 3 {
		t.Fatalf("%+v %v", p, err)
	}
	g.Generation = MaxGeneration + 1
	if _, err := PayloadFor(g, "ab12", n); err == nil {
		t.Fatal("a generation over the limit")
	}
}

// A5-09 (2): NextGeneration is the one step: exactly the next generation, a start
// later than the old one, state, start and generation in one record, only for a
// failed record. WithGeneration of a payload applies the same rule.
func TestNextGenerationIsExactlyOneStepWithALaterStart(t *testing.T) {
	n := note("17", root32(), sigLine)
	p0, _ := ParsePayload("ab12", n)
	r := NewRecord(1, "ab12", p0.Size, p0.Digest, t0)
	r, _ = r.WithSubmitStart(t0)
	now := t0.Add(time.Hour)
	if _, err := r.NextGeneration(now, now); err == nil {
		t.Fatal("a pending record took a new generation")
	}
	expired := r
	expired.State = StateExpired
	if _, err := expired.NextGeneration(now, now); err == nil {
		t.Fatal("an expired record took a new generation")
	}
	failed, _, _ := r.AfterError(Errorf(ClassPermanent, KindWORM, "verify", "store_time_early", "x"), t0, 0.5, Limits{})
	if failed.State != StateFailed {
		t.Fatal(failed.State)
	}
	if _, err := failed.NextGeneration(t0, now); err == nil {
		t.Fatal("a start that is not later than the old one was accepted")
	}
	if _, err := failed.NextGeneration(t0.Add(-time.Hour), now); err == nil {
		t.Fatal("an earlier start was accepted")
	}
	if _, err := failed.NextGeneration(t0.Add(500*time.Millisecond), now); err == nil {
		t.Fatal("a start in the same second was accepted")
	}
	if _, err := failed.NextGeneration(time.Time{}, now); err == nil {
		t.Fatal("a zero start was accepted")
	}
	noStart := failed
	noStart.SubmitStart = time.Time{}
	if _, err := noStart.NextGeneration(now, now); err == nil {
		t.Fatal("a record without a start took a new generation")
	}
	next, err := failed.NextGeneration(now, now)
	if err != nil || next.Generation != 1 || !next.SubmitStart.Equal(now) || next.State != StatePending ||
		next.NextAttemptAt == nil || !next.NextAttemptAt.Equal(now) || next.Attempts != 0 || next.LastError != "" {
		t.Fatalf("%+v %v", next, err)
	}
	if failed.Generation != 0 || !failed.SubmitStart.Equal(t0) {
		t.Fatal("NextGeneration changed its receiver")
	}
	// the payload of the new generation is built from the record
	p1, err := PayloadFor(next, "ab12", n)
	if err != nil || p1.Generation != 1 || !p1.SubmitStart.Equal(now) {
		t.Fatalf("%+v %v", p1, err)
	}
	// the last generation is the last
	last := failed
	last.Generation = MaxGeneration
	if _, err := last.NextGeneration(now, now); err == nil {
		t.Fatal("a generation over the limit was reached")
	}

	// the same rule for a payload alone
	p, _ := NewPayload("ab12", n, t0)
	for name, c := range map[string]struct {
		g uint32
		s time.Time
	}{
		"skipped": {2, now}, "repeated": {0, now}, "lowered-from-1": {0, now}, "same-start": {1, t0}, "earlier-start": {1, t0.Add(-time.Hour)},
	} {
		src := p
		if name == "lowered-from-1" {
			src.Generation = 1
		}
		if _, err := src.WithGeneration(c.g, c.s); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if q, err := p.WithGeneration(1, now); err != nil || q.Generation != 1 {
		t.Fatalf("%+v %v", q, err)
	}
}

// A5-03, A5-04: the evidence format holds the bounds per calendar and in all.
func TestOTSEvidenceBoundsPerCalendar(t *testing.T) {
	root := strings.Repeat("ab", 32)
	hdr := strings.Repeat("00", 80)
	base := OTSEvidence{Digest: strings.Repeat("00", 32), SubmittedAt: FormatTime(t0)}
	uris := make([]string, MaxOTSCalendars)
	for i := range uris {
		uris[i] = "https://c" + strconv.Itoa(i) + ".example"
		base.Calendars = append(base.Calendars, OTSCalendar{URI: uris[i], FragmentB64: "AAAA"})
	}
	aside := func(uri string, h int) OTSContradiction {
		return OTSContradiction{CalendarURI: uri, Height: uint64(1000 + h), MerkleRoot: root, Header: hdr, At: FormatTime(t0), Reason: "x"}
	}
	dropped := func(uri string, h int) OTSDropped {
		return OTSDropped{CalendarURI: uri, Height: uint64(1000 + h), MerkleRoot: root, At: FormatTime(t0)}
	}
	// every calendar at its full share: the largest evidence the library writes
	full := base
	for _, u := range uris {
		for h := 0; h < MaxOTSContradictionsPerCalendar; h++ {
			full.Contradicted = append(full.Contradicted, aside(u, h))
		}
		for h := 0; h < MaxOTSDroppedPerCalendar; h++ {
			full.Dropped = append(full.Dropped, dropped(u, h))
		}
	}
	if len(full.Contradicted) != MaxOTSContradictions || len(full.Dropped) != MaxOTSDropped {
		t.Fatal("the totals are not the shares together")
	}
	if _, err := EncodeEvidence(full); err != nil {
		t.Fatalf("every calendar at its share refused: %v", err)
	}
	// one calendar over its share is refused although the totals are not reached
	over := base
	for h := 0; h <= MaxOTSContradictionsPerCalendar; h++ {
		over.Contradicted = append(over.Contradicted, aside(uris[0], h))
	}
	if _, err := EncodeEvidence(over); err == nil {
		t.Fatal("one calendar over its share of the claims kept aside accepted")
	}
	overD := base
	for h := 0; h <= MaxOTSDroppedPerCalendar; h++ {
		overD.Dropped = append(overD.Dropped, dropped("https://gone.example", h))
	}
	if _, err := EncodeEvidence(overD); err == nil {
		t.Fatal("one calendar over its share of the dropped claims accepted")
	}
	if MaxOTSContradictions != MaxOTSCalendars*MaxOTSContradictionsPerCalendar || MaxOTSDropped != MaxOTSCalendars*MaxOTSDroppedPerCalendar {
		t.Fatal("the totals are not the shares of every calendar")
	}
}

// the section of duties lists sixteen items (A5, "the duties of the caller").
func TestDutiesSectionHasSixteenItems(t *testing.T) {
	i := strings.Index(docSource, "// # Duties of the caller")
	if i < 0 {
		t.Fatal("no section")
	}
	sec := docSource[i:]
	for n := 1; n <= 16; n++ {
		if !strings.Contains(sec, "\n//  "+strconv.Itoa(n)+". ") && !strings.Contains(sec, "\n// "+strconv.Itoa(n)+". ") {
			t.Errorf("duty %d is missing", n)
		}
	}
	if strings.Contains(sec, "\n//  17. ") || strings.Contains(sec, "\n// 17. ") {
		t.Error("a seventeenth item")
	}
}
