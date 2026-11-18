// SPDX-License-Identifier: AGPL-3.0-only

package ots

import (
	"bytes"
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/anchor"
)

// Tests of the second review (A2-02, A2-06, A2-08, A2-12).

// twoCalendars is the scene of A2-02: a hostile calendar listed first that
// answers at once with a plausible claim that is false, and an honest one that
// answers later. rootH and rootA are the merkle roots the claims sit on.
type twoCalendars struct {
	h, a         *scriptCal
	rc           anchor.Receipt
	commitA      []byte
	rootH, rootA []byte
	heightH      uint64
	heightA      uint64
}

func newTwoCalendars(t *testing.T) *twoCalendars {
	t.Helper()
	digest := sha([]byte("digest under test"))
	s := &twoCalendars{h: newScriptCal(t), a: newScriptCal(t), heightH: goodHeight, heightA: goodHeight + 5}
	fh, commitH := submittedFragment(digest, s.h.url(), 1)
	fa, commitA := submittedFragment(digest, s.a.url(), 2)
	s.commitA = commitA
	s.h.answer(commitH, goodAnswer(1, s.heightH))
	s.rootH, s.rootA = claimRootOf(commitH, 1), claimRootOf(commitA, 2)
	s.rc = receiptOf(t, evidenceFor(t, digest, map[string][]byte{s.h.url(): fh, s.a.url(): fa}, []string{s.h.url(), s.a.url()}))
	return s
}

func (s *twoCalendars) honestAnswers() { s.a.answer(s.commitA, goodAnswer(2, s.heightA)) }

// headers is a source that holds the real chain: another root at the height of
// the hostile claim, the honest root at the height of the honest one.
func (s *twoCalendars) headers() *fakeHeaders {
	return &fakeHeaders{m: map[uint64][]byte{
		s.heightH: syntheticHeader(sha([]byte("the real block")), testNow.Add(30*time.Minute)),
		s.heightA: syntheticHeader(s.rootA, testNow.Add(50*time.Minute)),
	}}
}

func calendarsOf(t *testing.T, rc anchor.Receipt) []string {
	t.Helper()
	ev, err := anchor.ParseOTSEvidence(rc.Evidence)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, c := range ev.Calendars {
		out = append(out, c.URI)
	}
	return out
}

// A2-02, without a header source: the first plausible claim does not end the
// collection of proofs. The claimed record asks the calendar whose fragment is
// still pending, and the honest proof is in the evidence beside the false
// claim; neither is called anchored.
func TestClaimedRecordKeepsAskingTheOtherCalendars(t *testing.T) {
	s := newTwoCalendars(t)
	tg := upgradeTarget(t, Options{}, s.h.url(), s.a.url())
	r1, err := tg.Upgrade(context.Background(), s.rc)
	if err != nil || r1.State != anchor.ReceiptClaimed || r1.NextCheck == nil {
		t.Fatalf("round 1: %+v %v", r1, err)
	}
	// the honest calendar has no proof yet: asked again, nothing changes
	r2, err := tg.Upgrade(context.Background(), r1)
	if err != nil || r2.State != anchor.ReceiptClaimed || !bytes.Equal(r2.Evidence, r1.Evidence) || r2.NextCheck == nil {
		t.Fatalf("round 2: %+v %v", r2, err)
	}
	if n := len(s.a.asked()); n != 2 {
		t.Fatalf("the honest calendar was asked %d times in 2 rounds, want 2", n)
	}
	if n := len(s.h.asked()); n != 1 {
		t.Fatalf("the hostile calendar was asked %d times: its pending attestation was replaced by its claim", n)
	}
	// the honest proof arrives
	s.honestAnswers()
	r3, err := tg.Upgrade(context.Background(), r2)
	if err != nil || r3.State != anchor.ReceiptClaimed || bytes.Equal(r3.Evidence, r2.Evidence) {
		t.Fatalf("round 3: %+v %v", r3, err)
	}
	if r3.NextCheck != nil {
		t.Error("nothing is pending any more, and no header source is configured: no new check is asked for")
	}
	ev, _ := anchor.ParseOTSEvidence(r3.Evidence)
	chk, err := CheckEvidence(ev, testNow.Add(time.Hour), nil)
	if err != nil || len(chk.Claims) != 2 || chk.Status != anchor.BitcoinClaimed {
		t.Fatalf("%+v %v", chk, err)
	}
	// with nothing pending and no source the receipt is returned as it is, and
	// no request is made
	asked := len(s.a.asked()) + len(s.h.asked())
	r4, err := tg.Upgrade(context.Background(), r3)
	if err != nil || !bytes.Equal(r4.Evidence, r3.Evidence) || len(s.a.asked())+len(s.h.asked()) != asked {
		t.Fatalf("%+v %v", r4, err)
	}
}

// A2-02 and A3-03, with a header source from the start: the false claim is kept
// aside (it stays in its fragment, is not plausible), the record falls back to
// submitted while the honest calendar can still deliver, and the honest proof
// anchors it.
func TestHostileFirstClaimIsDroppedByAHeader(t *testing.T) {
	s := newTwoCalendars(t)
	src := s.headers()
	tg := upgradeTarget(t, Options{Headers: src, headerWork: noWorkFn}, s.h.url(), s.a.url())
	r1, err := tg.Upgrade(context.Background(), s.rc)
	if err != nil || r1.State != anchor.ReceiptSubmitted || r1.NextCheck == nil {
		t.Fatalf("round 1: %+v %v (the record must fall back to submitted, not fail)", r1, err)
	}
	if got := calendarsOf(t, r1); len(got) != 2 {
		t.Fatalf("calendars after the claim was put aside: %v", got)
	}
	if ev, _ := anchor.ParseOTSEvidence(r1.Evidence); ev.Bitcoin != nil || len(ev.Contradicted) != 1 || ev.Contradicted[0].CalendarURI != s.h.url() {
		t.Fatalf("the claim kept aside is named as the summary, or not listed: %+v", ev)
	}
	// the record that held the claim takes the receipt
	rec := anchor.NewRecord(1, "agg", 1, [32]byte{}, testNow)
	rec.State = anchor.StateClaimed
	if rec2, err := rec.ApplyReceipt(r1, testNow); err != nil || rec2.State != anchor.StateSubmitted {
		t.Fatalf("%+v %v", rec2, err)
	}
	s.honestAnswers()
	r2, err := tg.Upgrade(context.Background(), r1)
	if err != nil || r2.State != anchor.ReceiptAnchored || r2.EvidenceTime == nil || !r2.EvidenceTime.Equal(testNow.Add(50*time.Minute)) {
		t.Fatalf("round 2: %+v %v", r2, err)
	}
	ev, _ := anchor.ParseOTSEvidence(r2.Evidence)
	if ev.Bitcoin == nil || ev.Bitcoin.Height != s.heightA || ev.Bitcoin.CalendarURI != s.a.url() || ev.Bitcoin.Status != anchor.BitcoinHeaderChecked {
		t.Fatalf("%+v", ev.Bitcoin)
	}
}

// A2-02, a claimed record meets a header source that was configured later: the
// hostile claim is contradicted, the honest calendar is asked, and the honest
// proof anchors the record in one call. And when the honest calendar has
// nothing yet, the claimed record falls back to submitted, not to a conflict.
func TestClaimedRecordWithALateHeaderSource(t *testing.T) {
	s := newTwoCalendars(t)
	claimed, err := upgradeTarget(t, Options{}, s.h.url(), s.a.url()).Upgrade(context.Background(), s.rc)
	if err != nil || claimed.State != anchor.ReceiptClaimed {
		t.Fatalf("%+v %v", claimed, err)
	}
	late := upgradeTarget(t, Options{Headers: s.headers(), headerWork: noWorkFn}, s.h.url(), s.a.url())
	back, err := late.Upgrade(context.Background(), claimed)
	if err != nil || back.State != anchor.ReceiptSubmitted || len(calendarsOf(t, back)) != 2 || back.ClaimsAside != 1 {
		t.Fatalf("fall back: %+v %v", back, err)
	}
	s.honestAnswers()
	out, err := late.Upgrade(context.Background(), claimed)
	if err != nil || out.State != anchor.ReceiptAnchored {
		t.Fatalf("one call: %+v %v", out, err)
	}
	ev, _ := anchor.ParseOTSEvidence(out.Evidence)
	if ev.Bitcoin.CalendarURI != s.a.url() || len(ev.Calendars) != 2 || len(ev.Contradicted) != 1 {
		t.Fatalf("%+v %v", ev.Bitcoin, calendarsOf(t, out))
	}
	// and a real check of the result by a verifier that has the headers
	chk, err := checkEvidence(ev, testNow.Add(time.Hour), nil, noWorkAt)
	if err != nil || chk.Status != anchor.BitcoinHeaderChecked {
		t.Fatalf("%+v %v", chk, err)
	}
}

// A claimed receipt whose lookups all fail but whose claim stands is reported
// as an error (so that the record waits and tries again), unchanged.
func TestClaimedRecordWhoseLookupsAllFail(t *testing.T) {
	s := newTwoCalendars(t)
	tg := upgradeTarget(t, Options{}, s.h.url(), s.a.url())
	claimed, err := tg.Upgrade(context.Background(), s.rc)
	if err != nil {
		t.Fatal(err)
	}
	s.a.mu.Lock()
	s.a.status = 503
	s.a.mu.Unlock()
	out, err := tg.Upgrade(context.Background(), claimed)
	if err == nil || !anchor.IsRetryable(err) || !bytes.Equal(out.Evidence, claimed.Evidence) {
		t.Fatalf("%+v %v", out, err)
	}
	rec := anchor.NewRecord(1, "agg", 1, [32]byte{}, testNow)
	rec.State = anchor.StateClaimed
	rec2, alert, aerr := rec.AfterError(err, testNow, 0.5, anchor.Limits{})
	if aerr != nil || rec2.State != anchor.StateClaimed || alert != anchor.AlertNone || rec2.NextAttemptAt == nil {
		t.Fatalf("%+v %q %v", rec2, alert, aerr)
	}
}

// A2-08: a round in which every lookup fails permanently is retryable: the
// record waits for its expiry instead of failing on one bad round. A stored
// fragment that cannot be read is permanent.
func TestRoundOfPermanentFailuresIsRetryable(t *testing.T) {
	digest := sha([]byte("digest under test"))
	cal := newScriptCal(t)
	cal.status = 410
	f, _ := submittedFragment(digest, cal.url(), 1)
	rc := receiptOf(t, evidenceFor(t, digest, map[string][]byte{cal.url(): f}, []string{cal.url()}))
	_, err := upgradeTarget(t, Options{}, cal.url()).Upgrade(context.Background(), rc)
	if err == nil || !anchor.IsRetryable(err) || anchor.CodeOf(err) != "upgrade_failed" {
		t.Fatalf("%v", err)
	}
	// the record: it waits, with a backoff, and is still submitted
	r := anchor.NewRecord(1, "agg", 1, digest32(digest), testNow)
	r.State = anchor.StateSubmitted
	r2, alert, aerr := r.AfterError(err, testNow, 0.5, anchor.Limits{})
	if aerr != nil || r2.State != anchor.StateSubmitted || alert != anchor.AlertNone {
		t.Fatalf("%+v %q %v", r2, alert, aerr)
	}
	// ... and the expiry still ends it
	if e, did := r2.ExpireIfDue(testNow.Add(Timeout(0)), Timeout(0), ClaimedTimeout(0)); !did || e.State != anchor.StateExpired {
		t.Fatal("the record does not expire")
	}
	// a fragment of our own that cannot be read is permanent
	bad := anchor.OTSEvidence{Digest: rcDigest(t, rc), SubmittedAt: anchor.FormatTime(testNow),
		Calendars: []anchor.OTSCalendar{{URI: cal.url(), FragmentB64: base64.StdEncoding.EncodeToString([]byte("garbage"))}}}
	raw, err := anchor.EncodeEvidence(bad)
	if err != nil {
		t.Fatal(err)
	}
	_, err = upgradeTarget(t, Options{}, cal.url()).Upgrade(context.Background(), anchor.Receipt{State: anchor.ReceiptSubmitted, Evidence: raw})
	if err == nil || !anchor.IsPermanent(err) {
		t.Fatalf("%v", err)
	}
}

func digest32(b []byte) (d [32]byte) { copy(d[:], b); return }

func rcDigest(t *testing.T, rc anchor.Receipt) string {
	t.Helper()
	ev, err := anchor.ParseOTSEvidence(rc.Evidence)
	if err != nil {
		t.Fatal(err)
	}
	return ev.Digest
}

// A2-06: a merge that fails (here the fragment would grow past its limit)
// makes the rest of that fragment's answers in the round skipped: their nodes
// belong to the tree that was put back, and merging into them would report a
// change that is not in the fragment.
func TestFailedMergeSkipsTheRestOfThatFragment(t *testing.T) {
	digest := sha([]byte("digest under test"))
	h := newScriptCal(t)
	var frag []byte
	var commits [][]byte
	for i := 1; i <= 2; i++ {
		arg := bytes.Repeat([]byte{byte(i)}, 8)
		commits = append(commits, sha(cat(digest, arg)))
		item := cat(opAppend(arg), opSHA256B, attPendingB(h.url()))
		if i == 1 {
			frag = cat(frag, []byte{0xff}, item)
		} else {
			frag = cat(frag, item)
		}
	}
	big := cat(opAppend(bytes.Repeat([]byte{7}, 3000)), opSHA256B, opSHA256B, attBitcoinB(goodHeight))
	h.answer(commits[0], big)
	h.answer(commits[1], goodAnswer(2, goodHeight+1)) // fits on its own
	ev := evidenceFor(t, digest, map[string][]byte{h.url(): frag}, []string{h.url()})
	tg := upgradeTarget(t, Options{fragmentLimit: 2000}, h.url())
	_, roots, uris, submitted, err := parseEvidenceTrees(ev)
	if err != nil {
		t.Fatal(err)
	}
	if node, _ := ParseFragment(frag, digest); node == nil {
		t.Fatal("test fragment")
	} else if b, _ := node.Serialize(); !bytes.Equal(b, frag) {
		t.Fatal("the test fragment is not canonical")
	}
	changed, ok, failures := tg.lookups(context.Background(), roots, uris, submitted, testNow)
	if len(failures) != 1 || ok != 1 {
		t.Fatalf("%d failures, %d answers: %v", len(failures), ok, failures)
	}
	if !strings.Contains(failures[0].Error(), "larger than") {
		t.Fatalf("%v", failures[0])
	}
	if changed {
		t.Fatal("a change was reported that is not in the fragment: the second answer was merged into the tree that was put back")
	}
	if n := len(pendingOf(roots[0])); n != 2 {
		t.Fatalf("%d pending attestations left, want 2", n)
	}
	if len(h.asked()) != 2 {
		t.Fatalf("asked %v", h.asked())
	}
}

// A2-06: no more than MaxParallel requests are in flight at once, however many
// lookups the round has. The calendars answer slowly, so that a larger
// parallelism would show.
func TestLookupsAreBoundedInParallel(t *testing.T) {
	if MaxParallel != 8 || MaxCalendars != 16 {
		t.Fatalf("the limits changed: %d %d", MaxParallel, MaxCalendars)
	}
	digest := sha([]byte("digest under test"))
	var inflight, peak atomic.Int32
	var mu sync.Mutex
	frags := map[string][]byte{}
	var order []string
	for i := 0; i < MaxCalendars; i++ {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			n := inflight.Add(1)
			for {
				p := peak.Load()
				if n <= p || peak.CompareAndSwap(p, n) {
					break
				}
			}
			time.Sleep(150 * time.Millisecond)
			inflight.Add(-1)
			w.WriteHeader(404)
		}))
		t.Cleanup(srv.Close)
		f, _ := submittedFragment(digest, srv.URL, byte(i+1))
		mu.Lock()
		frags[srv.URL] = f
		order = append(order, srv.URL)
		mu.Unlock()
	}
	rc := receiptOf(t, evidenceFor(t, digest, frags, order))
	out, err := upgradeTarget(t, Options{}, order...).Upgrade(context.Background(), rc)
	if err != nil || out.State != anchor.ReceiptSubmitted {
		t.Fatalf("%+v %v", out, err)
	}
	if p := peak.Load(); p > 8 || p < 4 {
		t.Fatalf("%d requests were in flight at once; the bound is 8 and the round has 16 lookups", p)
	}
}

// ClaimedTimeout is longer than Timeout, and defaults apply.
func TestClaimedTimeoutIsLonger(t *testing.T) {
	if Timeout(0) != 7*24*time.Hour || Timeout(3) != 3*24*time.Hour || ClaimedTimeout(0) != 28*24*time.Hour || ClaimedTimeout(3) != 12*24*time.Hour {
		t.Fatalf("%v %v %v %v", Timeout(0), Timeout(3), ClaimedTimeout(0), ClaimedTimeout(3))
	}
}
