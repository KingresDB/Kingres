// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package ots

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/anchor"
)

// Tests of the fifth review (A5-03, A5-04, A5-05, A5-08; correction 106).

// multiFragment is the answer of a calendar to the POST of a digest that holds n
// pending attestations on n distinct commitments (n branches).
func multiFragment(digest []byte, uri string, n int) (frag []byte, commits [][]byte) {
	var parts [][]byte
	for i := 0; i < n; i++ {
		nonce := bytes.Repeat([]byte{byte(0x10 + i)}, 8)
		commits = append(commits, sha(cat(digest, nonce)))
		if i < n-1 {
			parts = append(parts, []byte{0xff})
		}
		parts = append(parts, cat(opAppend(nonce), opSHA256B, attPendingB(uri)))
	}
	return cat(parts...), commits
}

// claimsAnswer is an answer of k plausible Bitcoin claims at one height, each on
// another merkle root (false claims: at most one of them can be the block's).
func claimsAnswer(salt byte, k int, height uint64) []byte {
	var parts [][]byte
	for i := 0; i < k; i++ {
		if i < k-1 {
			parts = append(parts, []byte{0xff})
		}
		parts = append(parts, cat(opAppend([]byte{salt, byte(i), 7, 7}), opSHA256B, opSHA256B, attBitcoinB(height)))
	}
	return cat(parts...)
}

// A5-03, the probe of the review: one hostile calendar fills its own share of the
// claims kept aside (and loses its surplus), one honest claim that the source
// contradicts ONCE is kept aside, not dropped, and the source being right
// afterwards anchors the record. Before the fix the list was one list for all
// calendars: the hostile calendar filled it and the honest claim, contradicted
// for the first time, was dropped at once with its pending attestation.
func TestHostileCalendarCannotDefeatTheConfirmationOfAnother(t *testing.T) {
	digest := sha([]byte("digest under test"))
	hostile, honest := newScriptCal(t), newScriptCal(t)
	fh, commits := multiFragment(digest, hostile.url(), PerFragmentLookups)
	for i, c := range commits {
		hostile.answer(c, claimsAnswer(byte(i+1), MaxAnswerClaims, goodHeight))
	}
	fa, commitA := submittedFragment(digest, honest.url(), 0x77)
	heightA := uint64(goodHeight + 5)
	honest.answer(commitA, goodAnswer(2, heightA))
	rootA := claimRootOf(commitA, 2)
	rc := receiptOf(t, evidenceFor(t, digest, map[string][]byte{hostile.url(): fh, honest.url(): fa}, []string{hostile.url(), honest.url()}))

	src := &fakeHeaders{m: map[uint64][]byte{goodHeight: wrongHeader(), heightA: wrongHeader()}}
	clk := newTestClock()
	tg := upgradeTargetClock(t, clk, Options{Headers: src, headerWork: noWorkFn}, nil, hostile.url(), honest.url())
	r1, err := tg.Upgrade(context.Background(), rc)
	if err != nil || r1.State != anchor.ReceiptSubmitted {
		t.Fatalf("round 1: %+v %v", r1, err)
	}
	surplus := PerFragmentLookups*MaxAnswerClaims - anchor.MaxOTSContradictionsPerCalendar
	if r1.ContradictedClaims != PerFragmentLookups*MaxAnswerClaims+1 || r1.ClaimsDropped != surplus ||
		r1.ClaimsAside != anchor.MaxOTSContradictionsPerCalendar+1 {
		t.Fatalf("counts: %+v", r1)
	}
	ev1, _ := anchor.ParseOTSEvidence(r1.Evidence)
	kept := false
	for _, c := range ev1.Contradicted {
		if c.CalendarURI == honest.url() && c.Height == heightA && c.MerkleRoot == hex.EncodeToString(rootA) {
			kept = true
		}
	}
	if !kept {
		t.Fatalf("the honest claim, contradicted once, is not kept aside: %+v", ev1.Contradicted)
	}
	if _, b := leafKinds(t, ev1, honest.url()); b != 1 {
		t.Fatal("the honest fragment lost its claim")
	}
	// the source is right afterwards: the record anchors
	src.mu.Lock()
	src.m[heightA] = syntheticHeader(rootA, testNow.Add(50*time.Minute))
	src.mu.Unlock()
	clk.advance(10 * time.Minute)
	r2, err := tg.Upgrade(context.Background(), r1)
	if err != nil || r2.State != anchor.ReceiptAnchored {
		t.Fatalf("round 2: %+v %v", r2, err)
	}
	ev2, _ := anchor.ParseOTSEvidence(r2.Evidence)
	if ev2.Bitcoin == nil || ev2.Bitcoin.CalendarURI != honest.url() || ev2.Bitcoin.Status != anchor.BitcoinHeaderChecked {
		t.Fatalf("%+v", ev2.Bitcoin)
	}
}

// A5-03: the share is the calendar's own: a calendar at its share does not lose a
// claim because ANOTHER calendar is at its share, and the evidence the library
// writes never exceeds the bounds of the evidence format.
func TestEveryCalendarHasItsOwnShareOfTheClaimsKeptAside(t *testing.T) {
	digest := sha([]byte("digest under test"))
	var cals []*scriptCal
	frags := map[string][]byte{}
	var order []string
	for i := 0; i < 3; i++ {
		c := newScriptCal(t)
		cals = append(cals, c)
		f, commits := multiFragment(digest, c.url(), 1)
		c.answer(commits[0], claimsAnswer(byte(i+1), MaxAnswerClaims, goodHeight+uint64(i)))
		frags[c.url()] = f
		order = append(order, c.url())
	}
	rc := receiptOf(t, evidenceFor(t, digest, frags, order))
	src := &fakeHeaders{m: map[uint64][]byte{goodHeight: wrongHeader(), goodHeight + 1: wrongHeader(), goodHeight + 2: wrongHeader()}}
	tg := upgradeTarget(t, Options{Headers: src, headerWork: noWorkFn}, order...)
	out, err := tg.Upgrade(context.Background(), rc)
	if err != nil || out.ClaimsAside != 3*MaxAnswerClaims || out.ClaimsDropped != 0 {
		t.Fatalf("%+v %v", out, err)
	}
	if _, err := anchor.ParseOTSEvidence(out.Evidence); err != nil {
		t.Fatal(err)
	}
}

// A5-03: an answer that carries more Bitcoin attestations than MaxAnswerClaims is
// refused as a whole, as an implausible answer of that calendar; the other
// calendars' answers are kept; an answer of exactly the limit is merged.
func TestAnswerWithTooManyClaimsIsRefusedAsAWhole(t *testing.T) {
	digest := sha([]byte("digest under test"))
	for _, k := range []int{MaxAnswerClaims, MaxAnswerClaims + 1} {
		s := newTwoCalendars(t)
		_, commitH := submittedFragment(digest, s.h.url(), 1)
		s.h.answer(commitH, claimsAnswer(9, k, s.heightH))
		s.honestAnswers()
		tg := upgradeTarget(t, Options{}, s.h.url(), s.a.url())
		out, err := tg.Upgrade(context.Background(), s.rc)
		if err != nil {
			t.Fatalf("%d claims: %v", k, err)
		}
		ev, _ := anchor.ParseOTSEvidence(out.Evidence)
		_, bh := leafKinds(t, ev, s.h.url())
		_, ba := leafKinds(t, ev, s.a.url())
		if ba != 1 {
			t.Fatalf("%d claims: the honest calendar's answer was lost (%d)", k, ba)
		}
		if k == MaxAnswerClaims && bh != k {
			t.Fatalf("an answer of exactly %d claims was not merged (%d)", k, bh)
		}
		if k > MaxAnswerClaims && bh != 0 {
			t.Fatalf("an answer of %d claims was merged in part (%d)", k, bh)
		}
		// the refusal is a failure of that calendar, with a code of its own
		ev0, _ := anchor.ParseOTSEvidence(s.rc.Evidence)
		_, roots, uris, submitted, perr := parseEvidenceTrees(ev0)
		if perr != nil {
			t.Fatal(perr)
		}
		_, _, failures, _ := tg.lookups(context.Background(), roots, uris, submitted, testNow, nil)
		got := ""
		for _, f := range failures {
			got += anchor.CodeOf(f) + " "
		}
		if refused := strings.Contains(got, "implausible_answer"); refused != (k > MaxAnswerClaims) {
			t.Fatalf("%d claims: failures %q", k, got)
		}
	}
}

// A5-04: the memory of dropped claims is per calendar. A calendar's later answers
// cannot evict what is remembered of another calendar, and one answer (at most
// MaxAnswerClaims claims) cannot evict what is remembered of its own calendar
// while the share has room for it; a full share forgets its own oldest entry.
func TestDroppedMemoryIsPerCalendar(t *testing.T) {
	mk := func(uri string, i int) ClaimCheck {
		var c ClaimCheck
		c.CalendarURI, c.Height = uri, uint64(1000+i)
		c.Root[0], c.Root[1] = byte(i), byte(len(uri))
		return c
	}
	const per = anchor.MaxOTSDroppedPerCalendar
	var list []anchor.OTSDropped
	for i := 0; i < 5; i++ {
		list = rememberDropped(list, mk("https://b.example", i), testNow)
	}
	// the calendar a has room for per-MaxAnswerClaims entries, then one answer of
	// MaxAnswerClaims claims comes: nothing of a or b is forgotten
	for i := 0; i < per-MaxAnswerClaims; i++ {
		list = rememberDropped(list, mk("https://a.example", i), testNow)
	}
	for i := per - MaxAnswerClaims; i < per; i++ {
		list = rememberDropped(list, mk("https://a.example", i), testNow)
	}
	for i := 0; i < per; i++ {
		if !isRemembered(list, "https://a.example", uint64(1000+i), rootOf(mk("https://a.example", i))) {
			t.Fatalf("entry %d of a was evicted by its own answer within the bound", i)
		}
	}
	// a goes on past its share: its OWN oldest is forgotten, b is untouched
	for i := per; i < per+20; i++ {
		list = rememberDropped(list, mk("https://a.example", i), testNow)
	}
	n := map[string]int{}
	for _, d := range list {
		n[d.CalendarURI]++
	}
	if n["https://a.example"] != per || n["https://b.example"] != 5 {
		t.Fatalf("%v", n)
	}
	if isRemembered(list, "https://a.example", 1000, rootOf(mk("https://a.example", 0))) || !isRemembered(list, "https://a.example", uint64(1000+per+19), rootOf(mk("https://a.example", per+19))) {
		t.Fatal("the oldest of a is not the one forgotten")
	}
	for i := 0; i < 5; i++ {
		if !isRemembered(list, "https://b.example", uint64(1000+i), rootOf(mk("https://b.example", i))) {
			t.Fatalf("entry %d of b was evicted by the answers of a", i)
		}
	}
	// the evidence format holds the same bounds
	ev := anchor.OTSEvidence{Digest: hex.EncodeToString(make([]byte, 32)), SubmittedAt: anchor.FormatTime(testNow), Dropped: list}
	if _, err := anchor.EncodeEvidence(ev); err != nil {
		t.Fatal(err)
	}
	ev.Dropped = append(append([]anchor.OTSDropped(nil), list...), anchor.OTSDropped{CalendarURI: "https://a.example", Height: 1, MerkleRoot: hex.EncodeToString(make([]byte, 32)), At: anchor.FormatTime(testNow)})
	if _, err := anchor.EncodeEvidence(ev); err == nil {
		t.Fatal("more than a calendar's share accepted")
	}
}

// A5-05: SubmitWithPrior carries the claims kept aside and the claims dropped of
// the prior evidence into the new one; an entry that names no attestation of a
// fragment that is kept is not carried (and does not make the evidence
// unwritable). The next Upgrade without a source then does not call the claim
// that a source contradicted a plain claim.
func TestSubmitWithPriorCarriesTheMemory(t *testing.T) {
	cal, p, rc, root := probeSetup(t)
	first := upgradeTarget(t, Options{}, cal.url())
	claimed, err := first.Upgrade(context.Background(), rc)
	if err != nil || claimed.State != anchor.ReceiptClaimed {
		t.Fatalf("%+v %v", claimed, err)
	}
	prior, _ := anchor.ParseOTSEvidence(claimed.Evidence)
	prior.Bitcoin = nil
	aside := anchor.OTSContradiction{CalendarURI: cal.url(), Height: goodHeight, MerkleRoot: hex.EncodeToString(root),
		Header: hex.EncodeToString(wrongHeader()), At: anchor.FormatTime(testNow), Reason: "the merkle root of the header is not the message of the attestation"}
	gone := aside
	gone.Height++ // names no attestation of the fragment
	prior.Contradicted = []anchor.OTSContradiction{aside, gone}
	prior.Dropped = []anchor.OTSDropped{{CalendarURI: "https://other.example", Height: 950123, MerkleRoot: hex.EncodeToString(sha([]byte("x"))), At: anchor.FormatTime(testNow)}}
	rec, err := first.SubmitWithPrior(context.Background(), p, &prior)
	if err != nil || rec.State != anchor.ReceiptSubmitted {
		t.Fatalf("%+v %v", rec, err)
	}
	ev, err := anchor.ParseOTSEvidence(rec.Evidence)
	if err != nil || len(ev.Contradicted) != 1 || ev.Contradicted[0] != aside || len(ev.Dropped) != 1 || ev.Dropped[0] != prior.Dropped[0] {
		t.Fatalf("the memory was not carried (or carried wrongly): %+v %v", ev, err)
	}
	// without a source the claim stays what the earlier source made of it: aside, so
	// the record is submitted and not claimed
	out, err := first.Upgrade(context.Background(), rec)
	if err != nil || out.State == anchor.ReceiptClaimed {
		t.Fatalf("%+v %v", out, err)
	}
	// a Retry through the interface the scheduler holds does the same
	var tgt anchor.Target = first
	rs, ok := tgt.(anchor.Resumer)
	if !ok {
		t.Fatal("ots.Target is not an anchor.Resumer")
	}
	raw, err := anchor.EncodeEvidence(anchor.OTSEvidence{Digest: prior.Digest, SubmittedAt: prior.SubmittedAt, Calendars: prior.Calendars,
		Contradicted: []anchor.OTSContradiction{aside}, Dropped: prior.Dropped})
	if err != nil {
		t.Fatal(err)
	}
	asked := len(cal.asked())
	rec2, err := rs.Resume(context.Background(), p, raw)
	if err != nil || rec2.State != anchor.ReceiptSubmitted {
		t.Fatalf("%+v %v", rec2, err)
	}
	ev2, _ := anchor.ParseOTSEvidence(rec2.Evidence)
	if len(ev2.Contradicted) != 1 || ev2.SubmittedAt != prior.SubmittedAt || len(cal.asked()) != asked {
		t.Fatalf("%+v", ev2)
	}
}

// A5-05: Resume with no prior is a Submit, an unreadable prior is refused, and
// the prior of a partial Submit is available in the encoding of the column.
func TestResumeAndPriorEvidenceRaw(t *testing.T) {
	p := testPayload(t)
	f := newFakeCal(t, false)
	good := newTarget(t, nil, 1, f.url())
	rc, err := good.Resume(context.Background(), p, nil)
	if err != nil || rc.State != anchor.ReceiptSubmitted {
		t.Fatalf("%+v %v", rc, err)
	}
	if _, err := good.Resume(context.Background(), p, []byte(`{"digest":1}`)); anchor.CodeOf(err) != "prior_evidence" {
		t.Fatalf("%v", err)
	}
	// a partial Submit: one of two calendars is down
	down := newFakeCal(t, false)
	down.set("503")
	both := newTarget(t, nil, 2, f.url(), down.url())
	_, err = both.Submit(context.Background(), p)
	var pe *PartialError
	if !errors.As(err, &pe) {
		t.Fatalf("%v", err)
	}
	raw, ok := PriorEvidenceRaw(err)
	if !ok {
		t.Fatal("no prior evidence in the encoding of the column")
	}
	if _, perr := anchor.ParseOTSEvidence(raw); perr != nil {
		t.Fatal(perr)
	}
	down.set("ok")
	rc2, err := both.Resume(context.Background(), p, raw)
	if err != nil || rc2.State != anchor.ReceiptSubmitted {
		t.Fatalf("%+v %v", rc2, err)
	}
	if _, ok := PriorEvidenceRaw(errors.New("x")); ok {
		t.Fatal("evidence from an error that carries none")
	}
	if !strings.Contains(string(rc2.Evidence), f.url()) {
		t.Fatal("the calendar that had answered is not in the evidence")
	}
}

// A5-08: the age of the newest entry of the table is given against a time the
// caller states (no clock, so that this test never starts to fail by itself).
func TestNewestCheckpointAge(t *testing.T) {
	_, bt := NewestCheckpoint()
	if d := NewestCheckpointAge(bt.Add(400 * 24 * time.Hour)); d != 400*24*time.Hour || d <= CheckpointWarnAge {
		t.Fatalf("%v", d)
	}
	if d := NewestCheckpointAge(bt.Add(30 * 24 * time.Hour)); d != 30*24*time.Hour || d > CheckpointWarnAge {
		t.Fatalf("%v", d)
	}
	if NewestCheckpointAge(bt.Add(-time.Hour)) >= 0 {
		t.Fatal("a table newer than now has a positive age")
	}
	if CheckpointWarnAge != 365*24*time.Hour {
		t.Fatal("the warning age is a year")
	}
}

// A5-04: the memory is bounded in all as well (calendars that are not in the
// evidence any more may still be remembered): the oldest of all is forgotten.
func TestDroppedMemoryIsBoundedInAll(t *testing.T) {
	var list []anchor.OTSDropped
	for c := 0; c < 25; c++ {
		for i := 0; i < anchor.MaxOTSDroppedPerCalendar-2; i++ {
			var cc ClaimCheck
			cc.CalendarURI, cc.Height = "https://c"+string(rune('a'+c))+".example", uint64(1000+i)
			cc.Root[0], cc.Root[1] = byte(c), byte(i)
			list = rememberDropped(list, cc, testNow)
		}
	}
	if len(list) != anchor.MaxOTSDropped {
		t.Fatalf("%d entries, want %d", len(list), anchor.MaxOTSDropped)
	}
	ev := anchor.OTSEvidence{Digest: hex.EncodeToString(make([]byte, 32)), SubmittedAt: anchor.FormatTime(testNow), Dropped: list}
	if _, err := anchor.EncodeEvidence(ev); err != nil {
		t.Fatal(err)
	}
	if list[0].CalendarURI == "https://ca.example" {
		t.Fatal("the oldest entries are not the ones forgotten")
	}
}
