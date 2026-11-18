// SPDX-License-Identifier: AGPL-3.0-only

package ots

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/anchor"
)

// scriptCal is a calendar whose answers the test writes. The answer to a
// GET of a commitment comes from the map; a commitment that is not in it is
// a 404.
type scriptCal struct {
	srv     *httptest.Server
	mu      sync.Mutex
	answers map[string][]byte // hex commitment -> answer
	status  int               // when not 0, every GET answers with this status
	gets    []string          // hex commitments asked, in order
}

func newScriptCal(t *testing.T) *scriptCal {
	c := &scriptCal{answers: map[string][]byte{}}
	c.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		c.mu.Lock()
		defer c.mu.Unlock()
		if !strings.HasPrefix(r.URL.Path, "/timestamp/") {
			w.WriteHeader(404)
			return
		}
		h := strings.TrimPrefix(r.URL.Path, "/timestamp/")
		c.gets = append(c.gets, h)
		if c.status != 0 {
			w.WriteHeader(c.status)
			return
		}
		a, ok := c.answers[h]
		if !ok {
			w.WriteHeader(404)
			return
		}
		_, _ = w.Write(a)
	}))
	t.Cleanup(c.srv.Close)
	return c
}

func (c *scriptCal) url() string { return c.srv.URL }

func (c *scriptCal) answer(commit []byte, b []byte) {
	c.mu.Lock()
	c.answers[hex.EncodeToString(commit)] = b
	c.mu.Unlock()
}

func (c *scriptCal) asked() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.gets...)
}

// submittedFragment is what a calendar answers to the POST of a digest: the
// digest, an append of a nonce, a SHA-256, and a pending attestation for uri
// (extra pending attestations may name other commitments through extra
// branches). It returns the bytes and the commitment (the message the
// pending attestation sits on).
func submittedFragment(digest []byte, uri string, nonce byte) (frag, commit []byte) {
	n := bytes.Repeat([]byte{nonce}, 8)
	commit = sha(cat(digest, n))
	return cat(opAppend(n), opSHA256B, attPendingB(uri)), commit
}

func evidenceFor(t *testing.T, digest []byte, frags map[string][]byte, order []string) anchor.OTSEvidence {
	t.Helper()
	ev := anchor.OTSEvidence{Digest: hex.EncodeToString(digest), SubmittedAt: anchor.FormatTime(testNow), Calendars: []anchor.OTSCalendar{}}
	for _, u := range order {
		ev.Calendars = append(ev.Calendars, anchor.OTSCalendar{URI: u, FragmentB64: base64.StdEncoding.EncodeToString(frags[u])})
	}
	if err := ev.Validate(); err != nil {
		t.Fatal(err)
	}
	return ev
}

func receiptOf(t *testing.T, ev anchor.OTSEvidence) anchor.Receipt {
	t.Helper()
	raw, err := anchor.EncodeEvidence(ev)
	if err != nil {
		t.Fatal(err)
	}
	return anchor.Receipt{State: anchor.ReceiptSubmitted, Evidence: raw}
}

// goodAnswer is what an honest calendar answers once Bitcoin has the
// commitment: an append, two SHA-256 operations (a merkle node) and the
// attestation.
func goodAnswer(x byte, height uint64) []byte {
	return cat(opAppend(bytes.Repeat([]byte{x}, 4)), opSHA256B, opSHA256B, attBitcoinB(height))
}

const goodHeight = 950000

func upgradeTarget(t *testing.T, o Options, urls ...string) *Target {
	t.Helper()
	o.Clock = func() time.Time { return testNow }
	tg, err := New(Config{Calendars: urls, MinCalendars: 1, AllowInsecure: true}, o)
	if err != nil {
		t.Fatal(err)
	}
	return tg
}

// A1-01: a Bitcoin attestation that cannot be the merkle root of a block, or of
// a block that could attest this digest, never makes a receipt anything but
// "submitted": the checks that need no header are made on every claim.
func TestImplausibleClaimsAreNotRecorded(t *testing.T) {
	digest := sha([]byte("digest under test"))
	lo, hi := HeightBounds(testNow, testNow)
	cases := map[string][]byte{
		"40-byte-message":   cat(opSHA256B, opSHA256B, opAppend(bytes.Repeat([]byte{1}, 8)), attBitcoinB(goodHeight)),
		"31-byte-message":   cat(opSHA256B, opSHA256B, opAppend(bytes.Repeat([]byte{1}, 8)), opSHA256B, attBitcoinB(goodHeight))[:0],
		"height-1":          goodAnswer(1, 1),
		"height-0":          goodAnswer(2, 0),
		"below-lower-bound": goodAnswer(3, lo-1),
		"above-upper-bound": goodAnswer(4, hi+1),
		"height-max":        goodAnswer(5, anchor.MaxBitcoinHeight),
		"no-operations":     cat(attBitcoinB(goodHeight)),
		"one-sha256":        cat(opAppend([]byte{9}), opSHA256B, attBitcoinB(goodHeight)),
		"ends-with-append":  cat(opSHA256B, opSHA256B, opAppend(bytes.Repeat([]byte{1}, 32)), attBitcoinB(goodHeight)),
		"ends-with-prepend": cat(opSHA256B, opSHA256B, opPrepend(bytes.Repeat([]byte{1}, 32)), attBitcoinB(goodHeight)),
		"sha1-then-sha256":  cat([]byte{0x02}, opAppend(bytes.Repeat([]byte{1}, 12)), opSHA256B, attBitcoinB(goodHeight)),
	}
	delete(cases, "31-byte-message") // the placeholder above is not a case
	for name, ans := range cases {
		cal := newScriptCal(t)
		frag, commit := submittedFragment(digest, cal.url(), 1)
		cal.answer(commit, ans)
		ev := evidenceFor(t, digest, map[string][]byte{cal.url(): frag}, []string{cal.url()})
		rc := receiptOf(t, ev)
		tg := upgradeTarget(t, Options{}, cal.url())
		out, err := tg.Upgrade(context.Background(), rc)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if out.State != anchor.ReceiptSubmitted || !bytes.Equal(out.Evidence, rc.Evidence) {
			t.Errorf("%s: the receipt is %s: %s", name, out.State, out.Evidence)
		}
	}
	// the bounds themselves are accepted
	for name, h := range map[string]uint64{"lower": lo, "upper": hi, "usual": goodHeight} {
		cal := newScriptCal(t)
		frag, commit := submittedFragment(digest, cal.url(), 1)
		cal.answer(commit, goodAnswer(1, h))
		rc := receiptOf(t, evidenceFor(t, digest, map[string][]byte{cal.url(): frag}, []string{cal.url()}))
		out, err := upgradeTarget(t, Options{}, cal.url()).Upgrade(context.Background(), rc)
		if err != nil || out.State != anchor.ReceiptClaimed {
			t.Errorf("%s height %d: %s %v", name, h, out.State, err)
		}
	}
}

// A1-01: the plausible claim is recorded as a claim, with the height, the
// calendar and the merkle root that the verifier must compare, and the
// evidence says "claimed", never anything stronger.
func TestPlausibleClaimIsClaimedNotAnchored(t *testing.T) {
	digest := sha([]byte("digest under test"))
	cal := newScriptCal(t)
	frag, commit := submittedFragment(digest, cal.url(), 1)
	cal.answer(commit, goodAnswer(1, goodHeight))
	rc := receiptOf(t, evidenceFor(t, digest, map[string][]byte{cal.url(): frag}, []string{cal.url()}))
	out, err := upgradeTarget(t, Options{}, cal.url()).Upgrade(context.Background(), rc)
	if err != nil || out.State != anchor.ReceiptClaimed || out.EvidenceTime != nil || out.NextCheck != nil {
		t.Fatalf("%+v %v", out, err)
	}
	ev, err := anchor.ParseOTSEvidence(out.Evidence)
	if err != nil || ev.Bitcoin == nil {
		t.Fatal(err)
	}
	root := sha(sha(cat(commit, bytes.Repeat([]byte{1}, 4))))
	// the commitment is the append of 4 bytes after the commitment, then two SHA-256
	root = sha(sha(cat(commit, bytes.Repeat([]byte{1}, 4))))
	if ev.Bitcoin.Status != anchor.BitcoinClaimed || ev.Bitcoin.Header != "" || ev.Bitcoin.Height != goodHeight ||
		ev.Bitcoin.CalendarURI != cal.url() || ev.Bitcoin.MerkleRoot != hex.EncodeToString(root) {
		t.Fatalf("%+v", ev.Bitcoin)
	}
	// the state machine does not count it as anchored
	r := anchor.NewRecord(1, "agg", 1, [32]byte{}, testNow)
	r.State = anchor.StateSubmitted
	r2, err := r.ApplyReceipt(out, testNow)
	if err != nil || r2.State != anchor.StateClaimed {
		t.Fatalf("%+v %v", r2, err)
	}
	if s, _ := r2.State.ContractStatus(); s != "pending" {
		t.Fatalf("a claim is shown as %s", s)
	}
}

// A1-01 (bounds from the clock): the same claim judged at another time. A height
// that is fine now is above what the chain could have reached a year before.
func TestClaimBoundsFollowTheClock(t *testing.T) {
	digest := sha([]byte("digest under test"))
	cal := newScriptCal(t)
	frag, commit := submittedFragment(digest, cal.url(), 1)
	_, hiNow := HeightBounds(testNow, testNow)
	cal.answer(commit, goodAnswer(1, hiNow+1000))
	ev := evidenceFor(t, digest, map[string][]byte{cal.url(): frag}, []string{cal.url()})
	rc := receiptOf(t, ev)
	late := func() time.Time { return testNow.Add(2 * 365 * 24 * time.Hour) }
	tg, _ := New(Config{Calendars: []string{cal.url()}, MinCalendars: 1, AllowInsecure: true}, Options{Clock: late})
	if out, err := tg.Upgrade(context.Background(), rc); err != nil || out.State != anchor.ReceiptClaimed {
		t.Fatalf("two years later: %s %v", out.State, err)
	}
	if out, err := upgradeTarget(t, Options{}, cal.url()).Upgrade(context.Background(), rc); err != nil || out.State != anchor.ReceiptSubmitted {
		t.Fatalf("now: %s %v", out.State, err)
	}
}

// One answer that holds a good claim and a bad one: only the good one is kept
// in the stored fragment.
func TestBadClaimIsDroppedGoodOneKept(t *testing.T) {
	digest := sha([]byte("digest under test"))
	cal := newScriptCal(t)
	frag, commit := submittedFragment(digest, cal.url(), 1)
	// two branches under the commitment: [sha256, sha256, bitcoin(1)] and [append, sha256, sha256, bitcoin(good)]
	ans := cat([]byte{0xff}, opSHA256B, opSHA256B, attBitcoinB(1), opAppend([]byte{7}), opSHA256B, opSHA256B, attBitcoinB(goodHeight))
	cal.answer(commit, ans)
	rc := receiptOf(t, evidenceFor(t, digest, map[string][]byte{cal.url(): frag}, []string{cal.url()}))
	out, err := upgradeTarget(t, Options{}, cal.url()).Upgrade(context.Background(), rc)
	if err != nil || out.State != anchor.ReceiptClaimed {
		t.Fatalf("%s %v", out.State, err)
	}
	ev, _ := anchor.ParseOTSEvidence(out.Evidence)
	chk, err := CheckEvidence(ev, testNow, nil)
	if err != nil || len(chk.Claims) != 1 || chk.Claims[0].Height != goodHeight || chk.Status != anchor.BitcoinClaimed {
		t.Fatalf("%+v %v", chk, err)
	}
}

// Claims at one height with two merkle roots cannot both be true: neither is
// plausible until a header settles it.
func TestContradictingClaimsAreNotPlausible(t *testing.T) {
	digest := sha([]byte("digest under test"))
	a, b := newScriptCal(t), newScriptCal(t)
	fa, ca := submittedFragment(digest, a.url(), 1)
	fb, cb := submittedFragment(digest, b.url(), 2)
	a.answer(ca, goodAnswer(1, goodHeight))
	b.answer(cb, goodAnswer(2, goodHeight))
	rc := receiptOf(t, evidenceFor(t, digest, map[string][]byte{a.url(): fa, b.url(): fb}, []string{a.url(), b.url()}))
	tg := upgradeTarget(t, Options{}, a.url(), b.url())
	out, err := tg.Upgrade(context.Background(), rc)
	if err != nil || out.State != anchor.ReceiptSubmitted {
		t.Fatalf("%s %v", out.State, err)
	}
	ev, _ := anchor.ParseOTSEvidence(out.Evidence)
	chk, err := CheckEvidence(ev, testNow, nil)
	if err != nil || len(chk.Claims) != 2 || chk.Status != "" || chk.Claims[0].Conflict == "" || chk.Claims[1].Conflict == "" {
		t.Fatalf("%+v %v", chk, err)
	}
	// different heights do not conflict
	b2 := newScriptCal(t)
	fb2, cb2 := submittedFragment(digest, b2.url(), 2)
	b2.answer(cb2, goodAnswer(2, goodHeight+1))
	rc = receiptOf(t, evidenceFor(t, digest, map[string][]byte{a.url(): fa, b2.url(): fb2}, []string{a.url(), b2.url()}))
	out, err = upgradeTarget(t, Options{}, a.url(), b2.url()).Upgrade(context.Background(), rc)
	if err != nil || out.State != anchor.ReceiptClaimed {
		t.Fatalf("%s %v", out.State, err)
	}
}

// ---- the header source ----

type fakeHeaders struct {
	mu    sync.Mutex
	m     map[uint64][]byte
	err   error
	calls []uint64
}

func (f *fakeHeaders) Header(ctx context.Context, h uint64) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, h)
	if f.err != nil {
		return nil, f.err
	}
	if b, ok := f.m[h]; ok {
		return b, nil
	}
	return nil, errors.New("no such block")
}

// noWorkFn replaces the proof-of-work check (Options.headerWork), for the tests of the path from the
// claim to a header: a header that holds a merkle root computed by SHA-256
// operations cannot be mined in a test. The real work check is tested on the
// real headers (TestRealHeadersPassCheck and the others).
func noWorkFn(h []byte) (HeaderResult, error) {
	if len(h) != HeaderSize {
		return HeaderResult{}, ErrBadHeader
	}
	return HeaderResult{Time: time.Unix(int64(binary.LittleEndian.Uint32(h[68:72])), 0).UTC()}, nil
}

func syntheticHeader(root []byte, tm time.Time) []byte {
	h := make([]byte, 80)
	h[0] = 0x20
	copy(h[36:68], root)
	binary.LittleEndian.PutUint32(h[68:72], uint32(tm.Unix()))
	binary.LittleEndian.PutUint32(h[72:76], 0x1d00ffff)
	return h
}

func claimRootOf(commit []byte, x byte) []byte {
	return sha(sha(cat(commit, bytes.Repeat([]byte{x}, 4))))
}

func headerSetup(t *testing.T) (cal *scriptCal, rc anchor.Receipt, root []byte) {
	t.Helper()
	digest := sha([]byte("digest under test"))
	cal = newScriptCal(t)
	frag, commit := submittedFragment(digest, cal.url(), 1)
	cal.answer(commit, goodAnswer(1, goodHeight))
	rc = receiptOf(t, evidenceFor(t, digest, map[string][]byte{cal.url(): frag}, []string{cal.url()}))
	return cal, rc, claimRootOf(commit, 1)
}

// A1-01: with a header source configured, a claim is anchored only when the
// header at its height has the merkle root, passes the work check and has a
// time that fits; the header is stored, EvidenceTime is its time, and the
// evidence checks again with nothing but its own bytes.
func TestHeaderSourceAnchors(t *testing.T) {
	cal, rc, root := headerSetup(t)
	bt := testNow.Add(40 * time.Minute)
	hdr := syntheticHeader(root, bt)
	src := &fakeHeaders{m: map[uint64][]byte{goodHeight: hdr}}
	tg := upgradeTarget(t, Options{Headers: src, headerWork: noWorkFn}, cal.url())
	out, err := tg.Upgrade(context.Background(), rc)
	if err != nil || out.State != anchor.ReceiptAnchored || out.EvidenceTime == nil || !out.EvidenceTime.Equal(bt) {
		t.Fatalf("%+v %v", out, err)
	}
	ev, _ := anchor.ParseOTSEvidence(out.Evidence)
	if ev.Bitcoin.Status != anchor.BitcoinHeaderChecked || ev.Bitcoin.Header != hex.EncodeToString(hdr) || ev.Bitcoin.MerkleRoot != hex.EncodeToString(root) {
		t.Fatalf("%+v", ev.Bitcoin)
	}
	// the verifier needs nothing else: the header is in the evidence
	chk, err := checkEvidence(ev, testNow.Add(time.Hour), nil, noWorkAt)
	if err != nil || chk.Status != anchor.BitcoinHeaderChecked || !chk.Claims[chk.Best].HeaderTime.Equal(bt) {
		t.Fatalf("%+v %v", chk, err)
	}
	// and when the verifier supplies its own header, the same result
	chk, err = checkEvidence(ev, testNow.Add(time.Hour), func(h uint64) ([]byte, bool) { return hdr, h == goodHeight }, noWorkAt)
	if err != nil || chk.Status != anchor.BitcoinHeaderChecked {
		t.Fatalf("%+v %v", chk, err)
	}
	// a header of the verifier that has another root makes the claim false
	other := syntheticHeader(sha([]byte("another root")), bt)
	chk, err = checkEvidence(ev, testNow.Add(time.Hour), func(h uint64) ([]byte, bool) { return other, h == goodHeight }, noWorkAt)
	if err == nil && chk.Status == anchor.BitcoinHeaderChecked && chk.Claims[0].HeaderProblem == "" {
		t.Fatal("a header with another merkle root passed")
	}
	// the header in the evidence is itself checked: another root is refused
	bad := ev
	b := *ev.Bitcoin
	b.Header = hex.EncodeToString(other)
	bad.Bitcoin = &b
	if _, err := checkEvidence(bad, testNow.Add(time.Hour), nil, noWorkAt); err == nil {
		t.Fatal("evidence with a header that does not hold the root accepted")
	}
}

// The merkle root of the header is compared as it stands in bytes 36 to 68: a
// header that holds the reversed root does not anchor the claim.
func TestHeaderSourceRootNotReversed(t *testing.T) {
	cal, rc, root := headerSetup(t)
	rev := make([]byte, 32)
	for i := range rev {
		rev[i] = root[31-i]
	}
	src := &fakeHeaders{m: map[uint64][]byte{goodHeight: syntheticHeader(rev, testNow.Add(time.Minute))}}
	out, err := upgradeTarget(t, Options{Headers: src, headerWork: noWorkFn}, cal.url()).Upgrade(context.Background(), rc)
	// the contradiction is kept aside (correction 90), not final: never anchored,
	// and not a conflict on one answer
	if err != nil || out.State != anchor.ReceiptSubmitted || out.ContradictedClaims != 1 || out.ClaimsAside != 1 || out.ClaimsDropped != 0 {
		t.Fatalf("%+v %v", out, err)
	}
}

func TestHeaderSourceFailures(t *testing.T) {
	cal, rc, root := headerSetup(t)
	// a header with another root contradicts the claim: it is kept aside, the
	// record falls back to submitted; when the source contradicts it again an
	// hour later nothing is left that could deliver, so a calendar lied: an error
	// of class conflict and the receipt as it was
	for name, hdr := range map[string][]byte{
		"false claim": syntheticHeader(sha([]byte("x")), testNow.Add(time.Minute)),
		// a block that is older than the digest contradicts the claim too
		"older block": syntheticHeader(root, testNow.Add(-BlockTimeSlack-time.Minute)),
	} {
		src := &fakeHeaders{m: map[uint64][]byte{goodHeight: hdr}}
		clk := newTestClock()
		tg := upgradeTargetClock(t, clk, Options{Headers: src, headerWork: noWorkFn}, nil, cal.url())
		aside, err := tg.Upgrade(context.Background(), rc)
		if err != nil || aside.State != anchor.ReceiptSubmitted || aside.ClaimsAside != 1 {
			t.Fatalf("%s: %+v %v", name, aside, err)
		}
		clk.advance(time.Hour)
		_, err = tg.Upgrade(context.Background(), aside)
		if err == nil || !anchor.IsConflict(err) || anchor.CodeOf(err) != "claim_contradicted" {
			t.Fatalf("%s: %v", name, err)
		}
	}
	// a block time in the future (our clock may be wrong) and a header that
	// fails the work check (the source may be wrong) contradict nothing: the
	// claim stays, as a claim, to be checked again
	future := &fakeHeaders{m: map[uint64][]byte{goodHeight: syntheticHeader(root, testNow.Add(FutureSlack+time.Hour))}}
	synthetic := &fakeHeaders{m: map[uint64][]byte{goodHeight: syntheticHeader(root, testNow.Add(time.Minute))}}
	for name, o := range map[string]Options{
		"future":    {Headers: future, headerWork: noWorkFn},
		"no-work":   {Headers: synthetic}, // the real work check: a synthetic header does not pass it
		"too-short": {Headers: &fakeHeaders{m: map[uint64][]byte{goodHeight: []byte{1, 2, 3}}}},
	} {
		out, err := upgradeTarget(t, o, cal.url()).Upgrade(context.Background(), rc)
		if err != nil || out.State != anchor.ReceiptClaimed || out.NextCheck == nil {
			t.Errorf("%s: %s %v", name, out.State, err)
			continue
		}
		ev, _ := anchor.ParseOTSEvidence(out.Evidence)
		if ev.Bitcoin == nil || ev.Bitcoin.Status != anchor.BitcoinClaimed || len(ev.Calendars) != 1 {
			t.Errorf("%s: the claim was dropped or called checked: %+v", name, ev.Bitcoin)
		}
	}
}

// A source that is unreachable leaves the claim a claim, to be checked again;
// the next call with a working source anchors it; a claimed receipt with a
// source that contradicts it, and nothing else, is an error of class conflict.
func TestHeaderSourceUnavailableThenAvailable(t *testing.T) {
	cal, rc, root := headerSetup(t)
	src := &fakeHeaders{err: errors.New("node unreachable")}
	tg := upgradeTarget(t, Options{Headers: src, headerWork: noWorkFn}, cal.url())
	out, err := tg.Upgrade(context.Background(), rc)
	if err != nil || out.State != anchor.ReceiptClaimed || out.NextCheck == nil || !out.NextCheck.Equal(testNow.Add(FirstCheck)) {
		t.Fatalf("%+v %v", out, err)
	}
	// without a source a claimed receipt is returned as it is; with one it is checked again
	src.mu.Lock()
	src.err, src.m = nil, map[uint64][]byte{goodHeight: syntheticHeader(root, testNow.Add(time.Minute))}
	src.mu.Unlock()
	// a target with a header source keeps the pending attestation of a merged
	// answer, so that a calendar can be asked again: it is asked once more
	gets := len(cal.asked())
	out2, err := tg.Upgrade(context.Background(), out)
	if err != nil || out2.State != anchor.ReceiptAnchored || len(cal.asked()) != gets+1 {
		t.Fatalf("%+v %v (calendar asked %d more times)", out2, err, len(cal.asked())-gets)
	}
	// contradicted: kept aside on the first answer, a conflict when the source
	// contradicts it again an hour later
	src.mu.Lock()
	src.m = map[uint64][]byte{goodHeight: syntheticHeader(sha([]byte("x")), testNow.Add(time.Minute))}
	src.mu.Unlock()
	clk := newTestClock()
	tg = upgradeTargetClock(t, clk, Options{Headers: src, headerWork: noWorkFn}, nil, cal.url())
	aside, err := tg.Upgrade(context.Background(), out)
	if err != nil || aside.State != anchor.ReceiptSubmitted || aside.ClaimsAside != 1 {
		t.Fatalf("%+v %v", aside, err)
	}
	clk.advance(time.Hour)
	_, err = tg.Upgrade(context.Background(), aside)
	if err == nil || !anchor.IsConflict(err) || anchor.CodeOf(err) != "claim_contradicted" {
		t.Fatalf("%v", err)
	}
	// a record that is claimed takes that error as a failure with the conflict alert
	r := anchor.NewRecord(1, "agg", 1, [32]byte{}, testNow)
	r.State = anchor.StateClaimed
	r2, alert, aerr := r.AfterError(err, testNow, 0.5, anchor.Limits{})
	if aerr != nil || r2.State != anchor.StateFailed || alert != anchor.AlertConflict {
		t.Fatalf("%+v %s %v", r2, alert, aerr)
	}
	// a header source asked for many heights is bounded
	if n := MaxHeaderChecks; n != 8 {
		t.Fatalf("MaxHeaderChecks %d", n)
	}
}

// ---- one hostile calendar against honest ones (A1-03) ----

// A hostile calendar H answers every lookup with something larger than a
// calendar may answer; the honest calendar A keeps its claim.
func TestOversizedAnswerOfOneCalendarCostsOnlyThatCalendar(t *testing.T) {
	digest := sha([]byte("digest under test"))
	h, a := newScriptCal(t), newScriptCal(t)
	fh, ch := submittedFragment(digest, h.url(), 1)
	fa, ca := submittedFragment(digest, a.url(), 2)
	h.answer(ch, bytes.Repeat([]byte{0}, MaxCalendarResponse+1))
	a.answer(ca, goodAnswer(2, goodHeight))
	rc := receiptOf(t, evidenceFor(t, digest, map[string][]byte{h.url(): fh, a.url(): fa}, []string{h.url(), a.url()}))
	out, err := upgradeTarget(t, Options{}, h.url(), a.url()).Upgrade(context.Background(), rc)
	if err != nil || out.State != anchor.ReceiptClaimed {
		t.Fatalf("%s %v", out.State, err)
	}
	ev, _ := anchor.ParseOTSEvidence(out.Evidence)
	if ev.Bitcoin == nil || ev.Bitcoin.CalendarURI != a.url() {
		t.Fatalf("%+v", ev.Bitcoin)
	}
	// the hostile calendar's own fragment is unchanged
	for _, c := range ev.Calendars {
		if c.URI == h.url() && c.FragmentB64 != base64.StdEncoding.EncodeToString(fh) {
			t.Error("the fragment of the hostile calendar changed")
		}
	}
}

// A calendar that answers with garbage, with a redirect, or with a 500 is that
// calendar's failure; the others are used.
func TestMalformedAnswerOfOneCalendarCostsOnlyThatCalendar(t *testing.T) {
	for name, bad := range map[string]func(*scriptCal, []byte){
		"garbage":  func(c *scriptCal, commit []byte) { c.answer(commit, []byte("not a timestamp")) },
		"keccak":   func(c *scriptCal, commit []byte) { c.answer(commit, cat([]byte{0x67}, attBitcoinB(goodHeight))) },
		"too-high": func(c *scriptCal, commit []byte) { c.answer(commit, goodAnswer(1, uint64(1)<<31)) },
		"500":      func(c *scriptCal, commit []byte) { c.status = 500 },
	} {
		digest := sha([]byte("digest under test"))
		h, a := newScriptCal(t), newScriptCal(t)
		fh, ch := submittedFragment(digest, h.url(), 1)
		fa, ca := submittedFragment(digest, a.url(), 2)
		bad(h, ch)
		a.answer(ca, goodAnswer(2, goodHeight))
		rc := receiptOf(t, evidenceFor(t, digest, map[string][]byte{h.url(): fh, a.url(): fa}, []string{h.url(), a.url()}))
		out, err := upgradeTarget(t, Options{}, h.url(), a.url()).Upgrade(context.Background(), rc)
		if err != nil || out.State != anchor.ReceiptClaimed {
			t.Errorf("%s: %s %v", name, out.State, err)
		}
	}
}

// A calendar whose answers would make its stored fragment larger than
// MaxFragment loses those answers; the others' fragments are kept.
func TestBloatedFragmentOfOneCalendarIsDropped(t *testing.T) {
	const fragmentLimit = 15000
	digest := sha([]byte("digest under test"))
	h, a := newScriptCal(t), newScriptCal(t)
	// H's fragment has 16 branches, each ending in a pending attestation for H with its own commitment
	n := byte(0)
	var hfrag []byte
	var commits [][]byte
	for i := 0; i < 16; i++ {
		n++
		arg := bytes.Repeat([]byte{n}, 8)
		commits = append(commits, sha(cat(digest, arg)))
		item := cat(opAppend(arg), opSHA256B, attPendingB(h.url()))
		if i < 15 {
			hfrag = cat(hfrag, []byte{0xff}, item)
		} else {
			hfrag = cat(hfrag, item)
		}
	}
	// the branches must be in the canonical order for the stored fragment to round trip: sort by argument (they are: 1..16)
	fa, ca := submittedFragment(digest, a.url(), 200)
	big := func(i int) []byte {
		// each answer: about 8 KB, three heights, plausible claims with distinct heights
		var out []byte
		for k := 0; k < 2; k++ {
			arg := bytes.Repeat([]byte{byte(i*2 + k + 1)}, 4000)
			item := cat(opAppend(arg), opSHA256B, opSHA256B, attBitcoinB(uint64(goodHeight+i*10+k)))
			if k == 0 {
				out = cat(out, []byte{0xff}, item)
			} else {
				out = cat(out, item)
			}
		}
		return out
	}
	for i, c := range commits {
		h.answer(c, big(i))
	}
	a.answer(ca, goodAnswer(9, goodHeight+777))
	frags := map[string][]byte{h.url(): hfrag, a.url(): fa}
	// the evidence parser sorts nothing: the stored fragment must be canonical
	if node, err := ParseFragment(hfrag, digest); err != nil {
		t.Fatal(err)
	} else if b, _ := node.Serialize(); !bytes.Equal(b, hfrag) {
		t.Fatal("the test fragment is not canonical: the test cannot prove the oversize path (a change of the canonical order must not turn this into a skip)")
	}
	rc := receiptOf(t, evidenceFor(t, digest, frags, []string{h.url(), a.url()}))
	tg := upgradeTarget(t, Options{fragmentLimit: fragmentLimit}, h.url(), a.url())
	rc.State = anchor.ReceiptSubmitted
	cur := rc
	out, err := tg.Upgrade(context.Background(), cur)
	if err != nil {
		t.Fatal(err)
	}
	cur = out
	ev, err := anchor.ParseOTSEvidence(cur.Evidence)
	if err != nil {
		t.Fatal(err)
	}
	var hSize int
	for _, c := range ev.Calendars {
		raw, _ := base64.StdEncoding.DecodeString(c.FragmentB64)
		if len(raw) > fragmentLimit {
			t.Errorf("%s: fragment of %d bytes", c.URI, len(raw))
		}
		if c.URI == h.url() {
			hSize = len(raw)
		}
	}
	// H's answers were taken until its fragment would pass the limit, then dropped
	if hSize <= len(hfrag) || hSize > fragmentLimit {
		t.Errorf("the fragment of the hostile calendar is %d bytes (it was %d)", hSize, len(hfrag))
	}
	if cur.State != anchor.ReceiptClaimed || ev.Bitcoin == nil {
		t.Fatalf("the honest calendar's claim was lost: %s", cur.State)
	}
}

// With a hostile calendar listed first that names the honest one in many
// pending attestations the honest one does not know, the honest calendar is
// still asked for its own commitment in the first round, and the hostile
// fragment spends only its own budget.
func TestNoStarvationByAHostileFirstCalendar(t *testing.T) {
	digest := sha([]byte("digest under test"))
	h, a := newScriptCal(t), newScriptCal(t)
	var hfrag []byte
	for i := 0; i < 40; i++ {
		arg := bytes.Repeat([]byte{byte(i + 1)}, 8)
		item := cat(opAppend(arg), opSHA256B, attPendingB(a.url()))
		if i < 39 {
			hfrag = cat(hfrag, []byte{0xff}, item)
		} else {
			hfrag = cat(hfrag, item)
		}
	}
	fa, ca := submittedFragment(digest, a.url(), 200)
	a.answer(ca, goodAnswer(9, goodHeight))
	frags := map[string][]byte{h.url(): hfrag, a.url(): fa}
	rc := receiptOf(t, evidenceFor(t, digest, frags, []string{h.url(), a.url()}))
	out, err := upgradeTarget(t, Options{}, h.url(), a.url()).Upgrade(context.Background(), rc)
	if err != nil || out.State != anchor.ReceiptClaimed {
		t.Fatalf("%s %v", out.State, err)
	}
	asked := a.asked()
	// literals: the budget of the design is 4 requests per fragment and 64 per call
	if PerFragmentLookups != 4 || MaxUpgradeLookups != 64 {
		t.Fatalf("the budget changed: %d %d", PerFragmentLookups, MaxUpgradeLookups)
	}
	if len(asked) > 4+1 {
		t.Errorf("the honest calendar was asked %d times; a hostile fragment may cost 4", len(asked))
	}
	own := hex.EncodeToString(ca)
	found := false
	for _, g := range asked {
		found = found || g == own
	}
	if !found {
		t.Error("the honest calendar was not asked for its own commitment")
	}
	// and a hostile fragment costs a bounded number of requests in total
	if len(a.asked())+len(h.asked()) > 64 {
		t.Errorf("%d requests in one call", len(a.asked())+len(h.asked()))
	}
}

// A calendar whose answers never come (a 404 for ever) is not an error and the
// others are asked in every round.
func TestEveryCalendarIsAskedEveryRound(t *testing.T) {
	digest := sha([]byte("digest under test"))
	var cals []*scriptCal
	frags := map[string][]byte{}
	var order []string
	for i := 0; i < 5; i++ {
		c := newScriptCal(t)
		f, _ := submittedFragment(digest, c.url(), byte(i+1))
		cals = append(cals, c)
		frags[c.url()] = f
		order = append(order, c.url())
	}
	rc := receiptOf(t, evidenceFor(t, digest, frags, order))
	urls := order
	tg := upgradeTarget(t, Options{}, urls...)
	for round := 0; round < 3; round++ {
		out, err := tg.Upgrade(context.Background(), rc)
		if err != nil || out.State != anchor.ReceiptSubmitted {
			t.Fatalf("%s %v", out.State, err)
		}
		for i, c := range cals {
			if n := len(c.asked()); n != round+1 {
				t.Errorf("round %d: calendar %d was asked %d times", round, i, n)
			}
		}
	}
}

// All lookups fail: the class is the most hopeful one among them, and permanent
// answers of every calendar make the round retryable, not permanent.
func TestUpgradeErrorClass(t *testing.T) {
	digest := sha([]byte("digest under test"))
	for name, c := range map[string]struct {
		status int
		class  anchor.Class
	}{"503": {503, anchor.ClassRetryable}, "403": {403, anchor.ClassRefused},
		// A2-08: a round of permanent answers is retryable (until the expiry of the record)
		"400": {400, anchor.ClassRetryable}, "410": {410, anchor.ClassRetryable}} {
		cal := newScriptCal(t)
		cal.status = c.status
		f, _ := submittedFragment(digest, cal.url(), 1)
		rc := receiptOf(t, evidenceFor(t, digest, map[string][]byte{cal.url(): f}, []string{cal.url()}))
		_, err := upgradeTarget(t, Options{}, cal.url()).Upgrade(context.Background(), rc)
		if err == nil || anchor.ClassOf(err) != c.class {
			t.Errorf("%s: %v (%v)", name, err, anchor.ClassOf(err))
		}
	}
	// one retryable among permanent: retryable
	a, b := newScriptCal(t), newScriptCal(t)
	a.status, b.status = 400, 503
	fa, _ := submittedFragment(digest, a.url(), 1)
	fb, _ := submittedFragment(digest, b.url(), 2)
	rc := receiptOf(t, evidenceFor(t, digest, map[string][]byte{a.url(): fa, b.url(): fb}, []string{a.url(), b.url()}))
	_, err := upgradeTarget(t, Options{}, a.url(), b.url()).Upgrade(context.Background(), rc)
	if !anchor.IsRetryable(err) {
		t.Errorf("%v", err)
	}
}

// ---- Submit ----

// A1-11: too few calendars is retryable when a calendar was only unreachable,
// refused when one only refused, permanent when a wrong answer makes the
// minimum out of reach.
func TestTooFewCalendarsClass(t *testing.T) {
	for name, c := range map[string]struct {
		bad   string
		class anchor.Class
	}{
		"503":     {"503", anchor.ClassRetryable},
		"403":     {"403", anchor.ClassRefused},
		"garbage": {"garbage", anchor.ClassPermanent},
		"huge":    {"huge", anchor.ClassPermanent},
	} {
		a, b := newFakeCal(t, false), newFakeCal(t, false)
		b.set(c.bad)
		tg := newTarget(t, nil, 2, a.url(), b.url())
		_, err := tg.Submit(context.Background(), testPayload(t))
		var pe *PartialError
		if !errors.As(err, &pe) || anchor.CodeOf(err) != "too_few_calendars" || anchor.ClassOf(err) != c.class {
			t.Errorf("%s: %v (class %v)", name, err, anchor.ClassOf(err))
		}
		if len(pe.Evidence.Calendars) != 1 {
			t.Errorf("%s: the answer of the good calendar was lost", name)
		}
	}
	// two refusals and a minimum of two: refused
	a, b := newFakeCal(t, false), newFakeCal(t, false)
	a.set("403")
	b.set("403")
	_, err := newTarget(t, nil, 2, a.url(), b.url()).Submit(context.Background(), testPayload(t))
	if !anchor.IsRefused(err) {
		t.Errorf("two refusals: %v", err)
	}
	// a 403 together with a wrong answer, minimum 2 of 3 calendars with one good: refused (the 403 may pass)
	c1, c2, c3 := newFakeCal(t, false), newFakeCal(t, false), newFakeCal(t, false)
	c2.set("403")
	c3.set("garbage")
	_, err = newTarget(t, nil, 2, c1.url(), c2.url(), c3.url()).Submit(context.Background(), testPayload(t))
	if !anchor.IsRefused(err) {
		t.Errorf("one good, one refused, one wrong: %v", err)
	}
	// driven through the record, a refusal gets the 1, 5 and 15 minute waits
	r := anchor.NewRecord(1, "agg", 17, [32]byte{}, testNow)
	r2, _, aerr := r.AfterError(err, testNow, 0.5, anchor.Limits{})
	if aerr != nil || !r2.NextAttemptAt.Equal(testNow.Add(time.Minute)) {
		t.Fatalf("%+v %v", r2, aerr)
	}
}

func TestSubmitRefusesInconsistentPayload(t *testing.T) {
	a := newFakeCal(t, false)
	tg := newTarget(t, nil, 1, a.url())
	good := testPayload(t)
	for name, p := range map[string]anchor.Payload{
		"zero":        {},
		"digest":      func() anchor.Payload { q := good; q.Digest[1] ^= 1; return q }(),
		"only-digest": {Digest: good.Digest},
	} {
		_, err := tg.Submit(context.Background(), p)
		if err == nil || anchor.CodeOf(err) != "bad_payload" || !anchor.IsPermanent(err) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if p, g := a.counts(); p != 0 || g != 0 {
		t.Error("a request was sent")
	}
}

func TestSubmitSetsSubmittedAtAndKeepsThePriorOne(t *testing.T) {
	a, b := newFakeCal(t, false), newFakeCal(t, false)
	b.set("503")
	now := testNow
	tg, _ := New(Config{Calendars: []string{a.url(), b.url()}, MinCalendars: 2, AllowInsecure: true}, Options{Clock: func() time.Time { return now }})
	p := testPayload(t)
	_, err := tg.Submit(context.Background(), p)
	var pe *PartialError
	if !errors.As(err, &pe) || pe.Evidence.SubmittedAt != anchor.FormatTime(testNow) {
		t.Fatalf("%v", err)
	}
	b.set("ok")
	now = testNow.Add(3 * time.Hour)
	rc, err := tg.SubmitWithPrior(context.Background(), p, &pe.Evidence)
	if err != nil {
		t.Fatal(err)
	}
	ev, _ := anchor.ParseOTSEvidence(rc.Evidence)
	if ev.SubmittedAt != anchor.FormatTime(testNow) {
		t.Fatalf("submitted_at %s: the time of the first attempt must be kept", ev.SubmittedAt)
	}
}

// A1-13: one limit for the height, in the parser and in the evidence.
func TestHeightLimitIsOne(t *testing.T) {
	if MaxHeight != anchor.MaxBitcoinHeight {
		t.Fatal("two limits")
	}
	if _, err := NewBitcoin(MaxHeight); err != nil {
		t.Fatal(err)
	}
	if _, err := NewBitcoin(MaxHeight + 1); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("%v", err)
	}
	if _, err := ParseFragment(cat(opSHA256B, attBitcoinB(uint64(MaxHeight)+1)), ownDigest); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("%v", err)
	}
}

func TestMagicIsACopy(t *testing.T) {
	m := Magic()
	m[0] = 0xee
	if Magic()[0] != 0 || !bytes.Equal(Magic(), ownMagic) {
		t.Fatal("Magic can be changed by a caller")
	}
}

// noWorkAt is noWorkFn for the work check of the internal functions, which also
// gets the height of the claim.
func noWorkAt(h []byte, _ uint64) (HeaderResult, error) { return noWorkFn(h) }
