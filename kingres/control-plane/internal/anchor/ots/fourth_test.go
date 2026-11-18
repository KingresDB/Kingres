// SPDX-License-Identifier: AGPL-3.0-only

package ots

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/anchor"
)

// Tests of the fourth review (A4-04, A4-05; correction 96).

func leafKinds(t *testing.T, ev anchor.OTSEvidence, uri string) (pending, bitcoin int) {
	t.Helper()
	d, _ := hex.DecodeString(ev.Digest)
	for _, c := range ev.Calendars {
		if c.URI != uri {
			continue
		}
		raw, _ := base64.StdEncoding.DecodeString(c.FragmentB64)
		n, err := ParseFragment(raw, d)
		if err != nil {
			t.Fatal(err)
		}
		for _, l := range n.Leaves() {
			switch l.Att.Kind {
			case AttPending:
				pending++
			case AttBitcoin:
				bitcoin++
			}
		}
	}
	return
}

// probeSetup is the setup of the review's probe: one calendar whose answer is a
// claim, on a payload of the library (so that a Retry can pass the evidence to
// SubmitWithPrior).
func probeSetup(t *testing.T) (cal *scriptCal, p anchor.Payload, rc anchor.Receipt, root []byte) {
	t.Helper()
	p = testPayload(t)
	cal = newScriptCal(t)
	frag, commit := submittedFragment(p.Digest[:], cal.url(), 1)
	cal.answer(commit, goodAnswer(1, goodHeight))
	rc = receiptOf(t, evidenceFor(t, p.Digest[:], map[string][]byte{cal.url(): frag}, []string{cal.url()}))
	return cal, p, rc, claimRootOf(commit, 1)
}

// A4-04: the pending attestation of a merged claim is kept in every
// configuration, so that a header source configured later, or one that
// contradicts the claim, can have the calendar asked again. Without a source
// nothing is asked for it (nothing can contradict the claim, and the answer is
// merged).
func TestPendingOfAMergedClaimIsKeptWithoutASource(t *testing.T) {
	cal, _, rc, _ := probeSetup(t)
	tg := upgradeTarget(t, Options{}, cal.url())
	out, err := tg.Upgrade(context.Background(), rc)
	if err != nil || out.State != anchor.ReceiptClaimed {
		t.Fatalf("%+v %v", out, err)
	}
	ev, _ := anchor.ParseOTSEvidence(out.Evidence)
	if p, b := leafKinds(t, ev, cal.url()); p != 1 || b != 1 {
		t.Fatalf("%d pending %d bitcoin: the pending attestation of the merged claim was removed", p, b)
	}
	// no request is made for it in the next round, and the receipt is as it was
	asked := len(cal.asked())
	again, err := tg.Upgrade(context.Background(), out)
	if err != nil || len(cal.asked()) != asked || string(again.Evidence) != string(out.Evidence) || again.NextCheck != nil {
		t.Fatalf("%+v %v (asked %d, was %d)", again, err, len(cal.asked()), asked)
	}
	// a source configured later contradicts the claim: the calendar IS asked again
	clk := newTestClock()
	src := &fakeHeaders{m: map[uint64][]byte{goodHeight: wrongHeader()}}
	late := upgradeTargetClock(t, clk, Options{Headers: src, headerWork: noWorkFn}, nil, cal.url())
	r2, err := late.Upgrade(context.Background(), out)
	if err != nil || r2.ClaimsAside != 1 || len(cal.asked()) <= asked {
		t.Fatalf("%+v %v (asked %d, was %d)", r2, err, len(cal.asked()), asked)
	}
}

// A4-04, the probe of the review: a claim merged before a header source existed,
// then a source that is wrong for over an hour, then right. The record fails
// (class conflict, claim_contradicted) as before, but the proof is not lost: the
// error carries the evidence as it was before the claims were dropped, and
// SubmitWithPrior with it, once the source is right, gives a record that
// anchors at once, with the first submission time.
func TestClaimMergedBeforeASourceSurvivesASourceWrongForAnHour(t *testing.T) {
	cal, p, rc, root := probeSetup(t)
	clk := newTestClock()
	// 1: no source: the claim is merged and the receipt is claimed
	first := upgradeTargetClock(t, clk, Options{}, nil, cal.url())
	claimed, err := first.Upgrade(context.Background(), rc)
	if err != nil || claimed.State != anchor.ReceiptClaimed {
		t.Fatalf("%+v %v", claimed, err)
	}
	// 2: a source that returns a header of another root, every 10 minutes
	src := &fakeHeaders{m: map[uint64][]byte{goodHeight: wrongHeader()}}
	wrong := upgradeTargetClock(t, clk, Options{Headers: src, headerWork: noWorkFn}, nil, cal.url())
	cur := claimed
	var failure error
	for i := 0; i < 8; i++ {
		out, err := wrong.Upgrade(context.Background(), cur)
		if err != nil {
			failure = err
			break
		}
		if out.State != anchor.ReceiptSubmitted || out.ClaimsAside != 1 {
			t.Fatalf("round %d: %+v", i+1, out)
		}
		cur = out
		clk.advance(10 * time.Minute)
	}
	if failure == nil {
		t.Fatal("the claim was never dropped")
	}
	if !anchor.IsConflict(failure) || anchor.CodeOf(failure) != "claim_contradicted" {
		t.Fatalf("%v", failure)
	}
	var ce *ContradictedError
	if !errors.As(failure, &ce) {
		t.Fatalf("%T carries no evidence", failure)
	}
	prior, ok := PriorEvidence(failure)
	if !ok || prior.Digest != hex.EncodeToString(p.Digest[:]) {
		t.Fatalf("%+v %v", prior, ok)
	}
	if pe, be := leafKinds(t, prior, cal.url()); be != 1 || pe != 1 {
		t.Fatalf("the evidence of the error does not hold the claim: %d pending %d bitcoin", pe, be)
	}
	// 3: the source is right; the operator retries with the evidence the error
	// carried, and the submission time of the first attempt is kept
	right := upgradeTargetClock(t, clk, Options{
		Headers:    &fakeHeaders{m: map[uint64][]byte{goodHeight: syntheticHeader(root, testNow.Add(40*time.Minute))}},
		headerWork: noWorkFn,
	}, nil, cal.url())
	rec, err := right.SubmitWithPrior(context.Background(), p, &prior)
	if err != nil || rec.State != anchor.ReceiptSubmitted {
		t.Fatalf("%+v %v", rec, err)
	}
	if ev, _ := anchor.ParseOTSEvidence(rec.Evidence); ev.SubmittedAt != anchor.FormatTime(testNow) {
		t.Fatalf("submitted_at %s", ev.SubmittedAt)
	}
	done, err := right.Upgrade(context.Background(), rec)
	if err != nil || done.State != anchor.ReceiptAnchored {
		t.Fatalf("%+v %v", done, err)
	}
	// the same input receipt of the failed round anchors too (the proof was fine)
	again, err := right.Upgrade(context.Background(), cur)
	if err != nil || again.State != anchor.ReceiptAnchored {
		t.Fatalf("%+v %v", again, err)
	}
}

// PriorEvidence reads the evidence from a PartialError as well, and from nothing
// else.
func TestPriorEvidenceOfAPartialError(t *testing.T) {
	ev := anchor.OTSEvidence{Digest: hex.EncodeToString(make([]byte, 32)), SubmittedAt: anchor.FormatTime(testNow)}
	pe := &PartialError{Evidence: ev, Err: anchor.Errorf(anchor.ClassRetryable, anchor.KindOTS, "submit", "too_few_calendars", "x")}
	if got, ok := PriorEvidence(pe); !ok || got.Digest != ev.Digest {
		t.Fatalf("%+v %v", got, ok)
	}
	if _, ok := PriorEvidence(anchor.Errorf(anchor.ClassPermanent, anchor.KindOTS, "upgrade", "x", "y")); ok {
		t.Fatal("evidence from an error that carries none")
	}
	if _, ok := PriorEvidence(nil); ok {
		t.Fatal("evidence from nil")
	}
}

// A4-04, the re-merge cycle: a claim dropped for good is remembered in the
// evidence (calendar, height, root); a calendar that answers the same claim
// again has it put aside at once, before it enters a fragment, and it is not
// counted as contradicted, aside or dropped again: only ClaimsRepeated says so.
func TestDroppedClaimIsRememberedAndAnsweredAgainIsPutAsideAtOnce(t *testing.T) {
	s := newTwoCalendars(t)
	clk := newTestClock()
	tg := upgradeTargetClock(t, clk, Options{Headers: s.headers(), headerWork: noWorkFn}, nil, s.h.url(), s.a.url())
	r1, err := tg.Upgrade(context.Background(), s.rc)
	if err != nil || r1.ClaimsAside != 1 {
		t.Fatalf("round 1: %+v %v", r1, err)
	}
	clk.advance(61 * time.Minute)
	r2, err := tg.Upgrade(context.Background(), r1)
	if err != nil || r2.ClaimsDropped != 1 || r2.ClaimsAside != 0 {
		t.Fatalf("round 2: %+v %v", r2, err)
	}
	ev2, _ := anchor.ParseOTSEvidence(r2.Evidence)
	if len(ev2.Dropped) != 1 || ev2.Dropped[0].CalendarURI != s.h.url() || ev2.Dropped[0].Height != s.heightH ||
		ev2.Dropped[0].MerkleRoot != hex.EncodeToString(s.rootH) || ev2.Dropped[0].At != anchor.FormatTime(clk.now()) {
		t.Fatalf("the dropped claim is not remembered: %+v", ev2.Dropped)
	}
	// the hostile calendar's fragment comes back (a spare pending attestation of
	// its own, answered with the same claim): the original receipt's fragment of
	// that calendar is put into the evidence that remembers the drop
	orig, _ := anchor.ParseOTSEvidence(s.rc.Evidence)
	var back anchor.OTSEvidence = ev2
	for _, c := range orig.Calendars {
		if c.URI == s.h.url() {
			back.Calendars = append([]anchor.OTSCalendar{c}, back.Calendars...)
		}
	}
	raw, err := anchor.EncodeEvidence(back)
	if err != nil {
		t.Fatal(err)
	}
	clk.advance(10 * time.Minute)
	askedBefore := len(s.h.asked())
	r3, err := tg.Upgrade(context.Background(), anchor.Receipt{State: anchor.ReceiptSubmitted, Evidence: raw})
	if err != nil {
		t.Fatalf("round 3: %v", err)
	}
	if len(s.h.asked()) <= askedBefore {
		t.Fatal("the hostile calendar was not asked")
	}
	if r3.ClaimsRepeated != 1 || r3.ContradictedClaims != 0 || r3.ClaimsAside != 0 || r3.ClaimsDropped != 0 {
		t.Fatalf("the claim was counted as new: %+v", r3)
	}
	ev3, _ := anchor.ParseOTSEvidence(r3.Evidence)
	if len(ev3.Contradicted) != 0 || len(ev3.Dropped) != 1 {
		t.Fatalf("%+v", ev3)
	}
	if _, b := leafKinds(t, ev3, s.h.url()); b != 0 {
		t.Fatalf("the re-merged claim entered the fragment")
	}
	// and a target that did not remember would have merged it and put it aside
	// as a new contradiction
	forgot := back
	forgot.Dropped = nil
	rawForgot, err := anchor.EncodeEvidence(forgot)
	if err != nil {
		t.Fatal(err)
	}
	f3, err := tg.Upgrade(context.Background(), anchor.Receipt{State: anchor.ReceiptSubmitted, Evidence: rawForgot})
	if err != nil || f3.ContradictedClaims != 1 || f3.ClaimsAside != 1 || f3.ClaimsRepeated != 0 {
		t.Fatalf("control: %+v %v", f3, err)
	}
}

func rootOf(c ClaimCheck) []byte { return c.Root[:] }

func TestRememberDroppedIsBoundedAndDeduplicated(t *testing.T) {
	var list []anchor.OTSDropped
	mk := func(i int) ClaimCheck {
		var c ClaimCheck
		c.CalendarURI = "https://a.example"
		c.Height = uint64(1000 + i)
		c.Root[0] = byte(i)
		return c
	}
	for i := 0; i < anchor.MaxOTSDropped+8; i++ {
		list = rememberDropped(list, mk(i), testNow)
	}
	if len(list) != anchor.MaxOTSDropped {
		t.Fatalf("%d entries", len(list))
	}
	if isRemembered(list, "https://a.example", 1000, rootOf(mk(0))) || !isRemembered(list, "https://a.example", 1008, rootOf(mk(8))) {
		t.Fatal("the oldest entries are not the ones forgotten")
	}
	n := len(list)
	if list = rememberDropped(list, mk(20), testNow); len(list) != n {
		t.Fatal("a claim already remembered was added twice")
	}
	ev := anchor.OTSEvidence{Digest: hex.EncodeToString(make([]byte, 32)), SubmittedAt: anchor.FormatTime(testNow), Dropped: list}
	if _, err := anchor.EncodeEvidence(ev); err != nil {
		t.Fatal(err)
	}
}

// A4-05: the package exposes the height and the block time of the newest entry of
// its table so that the caller can warn when it is old. The test pins the
// function to the table; it does not look at the clock.
func TestNewestCheckpoint(t *testing.T) {
	h, tm := NewestCheckpoint()
	last := checkpoints[len(checkpoints)-1]
	if h != last.height || tm.Unix() != last.time || tm.Location() != time.UTC {
		t.Fatalf("%d %v", h, tm)
	}
	for _, c := range checkpoints {
		if c.height > h || c.time > tm.Unix() {
			t.Fatalf("checkpoint %d is newer than the one reported", c.height)
		}
	}
	// the floor of every later height is taken from that entry
	if MaxTargetAt(h+1, 1).Cmp(MaxTargetAt(h, 1)) != 0 {
		t.Fatal("the floor after the newest checkpoint is not that of the newest checkpoint")
	}
}
