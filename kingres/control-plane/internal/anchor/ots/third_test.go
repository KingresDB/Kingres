// SPDX-License-Identifier: AGPL-3.0-only

package ots

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"math/big"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/anchor"
)

// Tests of the third review (A3-03, A3-04, A3-06, A3-12).

// testClock is a clock that a test moves between the rounds of an Upgrade.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func newTestClock() *testClock { return &testClock{t: testNow} }

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// upgradeTargetClock is upgradeTarget with the clock of the test and a
// configuration function.
func upgradeTargetClock(t *testing.T, clk *testClock, o Options, cfg func(*Config), urls ...string) *Target {
	t.Helper()
	o.Clock = clk.now
	c := Config{Calendars: urls, MinCalendars: 1, AllowInsecure: true}
	if cfg != nil {
		cfg(&c)
	}
	tg, err := New(c, o)
	if err != nil {
		t.Fatal(err)
	}
	return tg
}

// ---- A3-03: a contradicted claim is kept aside, not dropped on one answer ----

// wrongHeader is a header of the claim's height that has another merkle root
// (a header of a stale fork, an orphaned block).
func wrongHeader() []byte {
	return syntheticHeader(sha([]byte("a block of another fork")), testNow.Add(time.Minute))
}

// A header source that is wrong once costs nothing: the claim is kept aside
// with the header that contradicted it and the time, is neither plausible nor
// anchored, the calendar is asked again in the next round, and a header that
// checks the claim puts it back and anchors the record.
func TestWrongHeaderOnceKeepsTheClaimAside(t *testing.T) {
	cal, rc, root := headerSetup(t)
	src := &fakeHeaders{m: map[uint64][]byte{goodHeight: wrongHeader()}}
	clk := newTestClock()
	tg := upgradeTargetClock(t, clk, Options{Headers: src, headerWork: noWorkFn}, nil, cal.url())
	r1, err := tg.Upgrade(context.Background(), rc)
	if err != nil || r1.State != anchor.ReceiptSubmitted || r1.NextCheck == nil {
		t.Fatalf("round 1: %+v %v", r1, err)
	}
	if r1.ContradictedClaims != 1 || r1.ClaimsAside != 1 || r1.ClaimsDropped != 0 {
		t.Fatalf("counts: %+v", r1)
	}
	ev, err := anchor.ParseOTSEvidence(r1.Evidence)
	if err != nil || ev.Bitcoin != nil || len(ev.Calendars) != 1 || len(ev.Contradicted) != 1 {
		t.Fatalf("%+v %v", ev, err)
	}
	c := ev.Contradicted[0]
	if c.CalendarURI != cal.url() || c.Height != goodHeight || c.MerkleRoot != hex.EncodeToString(root) ||
		c.Header != hex.EncodeToString(wrongHeader()) || c.At != anchor.FormatTime(testNow) || c.Reason == "" {
		t.Fatalf("%+v", c)
	}
	// the claim is in the fragment but is not plausible: a verifier's check
	// says "no claim", names it quarantined, and refuses a summary that names it
	chk, err := CheckEvidence(ev, testNow.Add(time.Minute), nil)
	if err != nil || chk.Status != "" || len(chk.Claims) != 1 || !chk.Claims[0].Quarantined || chk.Claims[0].Plausible() {
		t.Fatalf("%+v %v", chk, err)
	}
	named := ev
	named.Bitcoin = &anchor.OTSBitcoin{Height: goodHeight, CalendarURI: cal.url(), MerkleRoot: hex.EncodeToString(root), Status: anchor.BitcoinClaimed}
	if _, err := CheckEvidence(named, testNow.Add(time.Minute), nil); err == nil {
		t.Fatal("a summary that names a claim kept aside was accepted")
	}
	// the next round, with the source right again: the calendar is asked again
	// (its pending attestation was kept) and the header anchors the record
	asked := len(cal.asked())
	src.mu.Lock()
	src.m = map[uint64][]byte{goodHeight: syntheticHeader(root, testNow.Add(40*time.Minute))}
	src.mu.Unlock()
	clk.advance(10 * time.Minute)
	r2, err := tg.Upgrade(context.Background(), r1)
	if err != nil || r2.State != anchor.ReceiptAnchored || r2.ClaimsAside != 0 || r2.ClaimsDropped != 0 {
		t.Fatalf("round 2: %+v %v", r2, err)
	}
	if len(cal.asked()) <= asked {
		t.Fatal("the calendar was not asked again")
	}
	ev2, _ := anchor.ParseOTSEvidence(r2.Evidence)
	if ev2.Bitcoin == nil || ev2.Bitcoin.Status != anchor.BitcoinHeaderChecked || len(ev2.Contradicted) != 0 {
		t.Fatalf("%+v", ev2)
	}
}

// A claim kept aside is dropped for good only when the source contradicts it
// again in a round at least the interval (default one hour) after the first
// contradiction; before that it stays aside, and the calendar is asked again.
func TestContradictionIsConfirmedAnHourLater(t *testing.T) {
	s := newTwoCalendars(t)
	src := s.headers()
	clk := newTestClock()
	tg := upgradeTargetClock(t, clk, Options{Headers: src, headerWork: noWorkFn}, nil, s.h.url(), s.a.url())
	r1, err := tg.Upgrade(context.Background(), s.rc)
	if err != nil || r1.ClaimsAside != 1 || r1.ContradictedClaims != 1 || r1.ClaimsDropped != 0 {
		t.Fatalf("round 1: %+v %v", r1, err)
	}
	// 59 minutes later: contradicted again, but not yet for good; the entry keeps
	// the time of the first contradiction
	clk.advance(59 * time.Minute)
	askedBefore := len(s.h.asked())
	r2, err := tg.Upgrade(context.Background(), r1)
	if err != nil || r2.ClaimsAside != 1 || r2.ContradictedClaims != 1 || r2.ClaimsDropped != 0 {
		t.Fatalf("round 2: %+v %v", r2, err)
	}
	if ev, _ := anchor.ParseOTSEvidence(r2.Evidence); len(ev.Contradicted) != 1 || ev.Contradicted[0].At != anchor.FormatTime(testNow) {
		t.Fatalf("the time of the first contradiction was not kept: %+v", ev.Contradicted)
	}
	if len(s.h.asked()) <= askedBefore {
		t.Fatal("the hostile calendar was not asked again while its claim was aside")
	}
	// an hour after the first contradiction: dropped for good, with the pending
	// attestation, so the calendar is not asked for the same claim again
	clk.advance(time.Minute)
	r3, err := tg.Upgrade(context.Background(), r2)
	if err != nil || r3.ClaimsDropped != 1 || r3.ClaimsAside != 0 || r3.ContradictedClaims != 1 || r3.State != anchor.ReceiptSubmitted {
		t.Fatalf("round 3: %+v %v", r3, err)
	}
	ev, _ := anchor.ParseOTSEvidence(r3.Evidence)
	if len(ev.Contradicted) != 0 || len(ev.Calendars) != 1 || ev.Calendars[0].URI != s.a.url() {
		t.Fatalf("%+v", ev)
	}
	askedBefore = len(s.h.asked())
	clk.advance(10 * time.Minute)
	if _, err := tg.Upgrade(context.Background(), r3); err != nil || len(s.h.asked()) != askedBefore {
		t.Fatalf("the dropped calendar was asked again: %v", err)
	}
}

// The interval is configurable (contradictionConfirmMinutes), with the range
// 1 to 10080 and the default 60.
func TestContradictionConfirmIntervalIsConfigurable(t *testing.T) {
	if c, err := ParseConfig([]byte(`{"calendars":["https://a.example"]}`)); err != nil || c.ContradictionConfirmMinutes != 60 {
		t.Fatalf("%+v %v", c, err)
	}
	for _, bad := range []string{"-1", "10081"} {
		if _, err := ParseConfig([]byte(`{"calendars":["https://a.example"],"contradictionConfirmMinutes":` + bad + `}`)); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
	if c, err := ParseConfig([]byte(`{"calendars":["https://a.example"],"contradictionConfirmMinutes":10080}`)); err != nil || c.ContradictionConfirmMinutes != 10080 {
		t.Fatalf("%+v %v", c, err)
	}
	cal, rc, _ := headerSetup(t)
	src := &fakeHeaders{m: map[uint64][]byte{goodHeight: wrongHeader()}}
	clk := newTestClock()
	tg := upgradeTargetClock(t, clk, Options{Headers: src, headerWork: noWorkFn}, func(c *Config) { c.ContradictionConfirmMinutes = 5 }, cal.url())
	r1, err := tg.Upgrade(context.Background(), rc)
	if err != nil || r1.ClaimsAside != 1 {
		t.Fatalf("%+v %v", r1, err)
	}
	clk.advance(4 * time.Minute)
	if r2, err := tg.Upgrade(context.Background(), r1); err != nil || r2.ClaimsAside != 1 || r2.ClaimsDropped != 0 {
		t.Fatalf("4 minutes: %+v %v", r2, err)
	}
	clk.advance(time.Minute)
	if _, err := tg.Upgrade(context.Background(), r1); err == nil || anchor.CodeOf(err) != "claim_contradicted" || !anchor.IsConflict(err) {
		t.Fatalf("5 minutes: %v", err)
	}
}

// What is kept aside is bounded per calendar (correction 106): the claims of one
// calendar over its share (anchor.MaxOTSContradictionsPerCalendar) are dropped
// for good at once, and the evidence stays within its limit.
func TestClaimsAsideAreBounded(t *testing.T) {
	digest := sha([]byte("digest under test"))
	cal := newScriptCal(t)
	const per = anchor.MaxOTSContradictionsPerCalendar
	const n = 2 * per
	frag, commits := multiFragment(digest, cal.url(), 2)
	for i, c := range commits {
		cal.answer(c, claimsAnswer(byte(i+1), per, goodHeight))
	}
	rc := receiptOf(t, evidenceFor(t, digest, map[string][]byte{cal.url(): frag}, []string{cal.url()}))
	src := &fakeHeaders{m: map[uint64][]byte{goodHeight: wrongHeader()}}
	tg := upgradeTarget(t, Options{Headers: src, headerWork: noWorkFn}, cal.url())
	out, err := tg.Upgrade(context.Background(), rc)
	if err != nil || out.ContradictedClaims != n || out.ClaimsAside != per || out.ClaimsDropped != per {
		t.Fatalf("%+v %v", out, err)
	}
	if ev, err := anchor.ParseOTSEvidence(out.Evidence); err != nil || len(ev.Contradicted) != per {
		t.Fatalf("%v", err)
	}
}

// The evidence format of the claims kept aside: the member is an array that the
// writer always writes, and an entry must name a calendar of the evidence and,
// when the evidence is read to be used, a claim of its fragments.
func TestContradictedEvidenceShape(t *testing.T) {
	cal, rc, _ := headerSetup(t)
	src := &fakeHeaders{m: map[uint64][]byte{goodHeight: wrongHeader()}}
	out, err := upgradeTarget(t, Options{Headers: src, headerWork: noWorkFn}, cal.url()).Upgrade(context.Background(), rc)
	if err != nil {
		t.Fatal(err)
	}
	ev, _ := anchor.ParseOTSEvidence(out.Evidence)
	again, err := anchor.EncodeEvidence(ev)
	if err != nil || string(again) != string(out.Evidence) {
		t.Fatalf("not a fixed point: %v", err)
	}
	bad := ev
	bad.Contradicted = append([]anchor.OTSContradiction(nil), ev.Contradicted...)
	bad.Contradicted[0].CalendarURI = "https://elsewhere.example"
	if _, err := anchor.EncodeEvidence(bad); err == nil {
		t.Error("an entry naming no calendar of the evidence accepted")
	}
	bad = ev
	bad.Contradicted = append([]anchor.OTSContradiction(nil), ev.Contradicted...)
	bad.Contradicted[0].Height++
	raw, err := anchor.EncodeEvidence(bad)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := CheckEvidence(bad, testNow, nil); err == nil {
		t.Error("CheckEvidence accepted an entry that names no claim")
	}
	if _, err := upgradeTarget(t, Options{Headers: src, headerWork: noWorkFn}, cal.url()).Upgrade(context.Background(), anchor.Receipt{State: anchor.ReceiptSubmitted, Evidence: raw}); err == nil {
		t.Error("Upgrade accepted an entry that names no claim")
	}
	if !strings.Contains(string(out.Evidence), `"contradicted":[{"calendar_uri":`) {
		t.Fatal(string(out.Evidence))
	}
}

// ---- A3-04: the work of a header ----

// bitsOf is the compact form of a target.
func bitsOf(target *big.Int) uint32 {
	size := uint((target.BitLen() + 7) / 8)
	var mant uint32
	if size <= 3 {
		mant = uint32(new(big.Int).Lsh(target, 8*(3-size)).Uint64())
	} else {
		mant = uint32(new(big.Int).Rsh(target, 8*(size-3)).Uint64())
	}
	if mant&0x00800000 != 0 {
		mant >>= 8
		size++
	}
	return uint32(size)<<24 | mant
}

// mineHeader returns a header with the merkle root and time given whose double
// SHA-256 meets target (at most 2^26 tries: the targets of these tests need
// about 2^12).
func mineHeader(t *testing.T, root []byte, tm time.Time, target *big.Int) []byte {
	t.Helper()
	h := make([]byte, 80)
	h[0] = 0x20
	copy(h[36:68], root)
	binary.LittleEndian.PutUint32(h[68:72], uint32(tm.Unix()))
	bits := bitsOf(target)
	binary.LittleEndian.PutUint32(h[72:76], bits)
	real, _ := CompactToTarget(bits)
	for nonce := uint32(0); nonce < 1<<26; nonce++ {
		binary.LittleEndian.PutUint32(h[76:80], nonce)
		a := sha(h)
		b := sha(a)
		var be [32]byte
		for i := range be {
			be[i] = b[31-i]
		}
		if new(big.Int).SetBytes(be[:]).Cmp(real) <= 0 {
			return h
		}
	}
	t.Fatal("no header found")
	return nil
}

func pow2(n uint) *big.Int { return new(big.Int).Lsh(big.NewInt(1), n) }

func mustTarget(bits uint32) *big.Int {
	t, err := CompactToTarget(bits)
	if err != nil {
		panic(err)
	}
	return t
}

// The default floor refuses a header of difficulty 1 by arithmetic on the
// target (no mining): the real genesis header, which is a valid header of
// difficulty 1, passes the plain work check and is refused for every height of
// the present era, and passes only at a height where difficulty 1 was real.
func TestDifficultyOneIsBelowTheFloor(t *testing.T) {
	hs := headers(t)
	genesis := hs[0].raw
	if _, err := CheckHeader(genesis); err != nil {
		t.Fatalf("the plain work check refuses the genesis header: %v", err)
	}
	diff1 := mustTarget(powLimitBits)
	for _, height := range []uint64{912096, 950000, 1_000_000, 2_000_000} {
		floor := MaxTargetAt(height, DefaultWorkSlack)
		if floor.Cmp(diff1) >= 0 {
			t.Fatalf("height %d: the floor %x is not below the target of difficulty 1", height, floor)
		}
		// by arithmetic: the target of difficulty 1 is more than 2^40 times the floor
		if r := new(big.Int).Div(diff1, floor); r.BitLen() <= 41 {
			t.Fatalf("height %d: difficulty 1 is only %v times easier than the floor", height, r)
		}
		if _, err := CheckHeaderAt(genesis, height); err == nil || !errors.Is(err, ErrBadHeader) {
			t.Errorf("height %d: the genesis header passes the floor: %v", height, err)
		}
	}
	// at height 0 the floor is 16 times the limit, and the genesis header passes
	if r, err := CheckHeaderAt(genesis, 0); err != nil || r.Work.BitLen() < 32 || r.Work.BitLen() > 33 {
		t.Fatalf("%+v %v", r, err)
	}
	// a real header of the present era passes at its height, and reports its work
	r, err := CheckHeaderAt(hs[912095].raw, 912095)
	if err != nil || r.Bits != 0x17022b91 || r.Work.BitLen() < 78 || r.Work.BitLen() > 80 {
		t.Fatalf("%+v %v", r, err)
	}
	// the floor of every height after the last checkpoint is 16 times its target
	want := new(big.Int).Mul(mustTarget(0x17022b91), big.NewInt(16))
	if MaxTargetAt(912095, 16).Cmp(want) != 0 || MaxTargetAt(5_000_000, 16).Cmp(want) != 0 || MaxTargetAt(912094, 16).Cmp(want) == 0 {
		t.Fatal("the floor is not 16 times the target of the nearest earlier checkpoint")
	}
	// WorkSlack is the option, and it has an upper bound (A4-05): a slack of 2^48,
	// or any value above MaxWorkSlack, is refused by New, and the largest slack
	// admitted keeps the floor of the present era below difficulty 1
	for _, s := range []uint{1 << 48, MaxWorkSlack + 1, ^uint(0)} {
		if _, err := New(Config{Calendars: []string{"https://a.example"}}, Options{WorkSlack: s}); err == nil {
			t.Fatalf("a slack of %d was accepted", s)
		}
	}
	tg, err := New(Config{Calendars: []string{"https://a.example"}}, Options{WorkSlack: MaxWorkSlack})
	if err != nil {
		t.Fatal(err)
	}
	for _, height := range []uint64{912095, 912096, 950000, 5_000_000} {
		if _, err := tg.work(genesis, height); err == nil {
			t.Fatalf("height %d: the largest slack admits difficulty 1", height)
		}
		if MaxTargetAt(height, MaxWorkSlack).Cmp(diff1) >= 0 {
			t.Fatalf("height %d: the floor of the largest slack is not below difficulty 1", height)
		}
	}
	def, _ := New(Config{Calendars: []string{"https://a.example"}}, Options{})
	if _, err := def.work(genesis, 950000); err == nil {
		t.Fatal("the default target accepts difficulty 1")
	}
}

// The table of checkpoints agrees with the real headers in testdata, bits
// included (the floor is taken from it).
func TestCheckpointBitsAreTheRealOnes(t *testing.T) {
	hs := headers(t)
	for _, c := range checkpoints {
		raw := hs[c.height].raw
		if got := binary.LittleEndian.Uint32(raw[72:76]); got != c.bits {
			t.Errorf("height %d: bits %#x in the table, %#x in the header", c.height, c.bits, got)
		}
	}
}

// A header that is mined in the test, with bounds low enough to mine in a
// moment: a header mined against the easiest target that the floor must still
// refuse is refused, one mined against the floor is accepted and reports its
// work; through Upgrade the refused header leaves the claim a claim (the source
// may be wrong), and the accepted one anchors it.
func TestMinedHeaderAgainstALoweredFloor(t *testing.T) {
	limit, floor := pow2(250), pow2(244)
	work := func(h []byte) (HeaderResult, error) { return checkHeaderWith(h, limit, floor) }
	cal, rc, root := headerSetup(t)
	tm := testNow.Add(40 * time.Minute)

	tooEasy := mineHeader(t, root, tm, new(big.Int).Mul(floor, big.NewInt(2)))
	if _, err := work(tooEasy); err == nil || !strings.Contains(err.Error(), "floor") {
		t.Fatalf("the header mined above the floor: %v", err)
	}
	good := mineHeader(t, root, tm, floor)
	res, err := work(good)
	if err != nil || res.Work.BitLen() < 12 || res.Work.BitLen() > 13 || res.Bits != bitsOf(floor) {
		t.Fatalf("%+v %v", res, err)
	}
	if _, err := checkHeaderWith(good, pow2(243), nil); err == nil {
		t.Fatal("a target above the limit passes")
	}

	src := &fakeHeaders{m: map[uint64][]byte{goodHeight: tooEasy}}
	out, err := upgradeTarget(t, Options{Headers: src, headerWork: work}, cal.url()).Upgrade(context.Background(), rc)
	if err != nil || out.State != anchor.ReceiptClaimed || out.NextCheck == nil || out.ContradictedClaims != 0 || out.ClaimsAside != 0 {
		t.Fatalf("below the floor: %+v %v", out, err)
	}
	src = &fakeHeaders{m: map[uint64][]byte{goodHeight: good}}
	out, err = upgradeTarget(t, Options{Headers: src, headerWork: work}, cal.url()).Upgrade(context.Background(), rc)
	if err != nil || out.State != anchor.ReceiptAnchored {
		t.Fatalf("at the floor: %+v %v", out, err)
	}
}

// CheckEvidence reads a header_checked evidence with the same floor as Upgrade:
// the work function it uses is CheckHeaderAt (a header of difficulty 1 does not
// pass at the height of a claim).
func TestCheckEvidenceAppliesTheFloor(t *testing.T) {
	genesis := headers(t)[0].raw
	if _, err := CheckHeaderAt(genesis, goodHeight); err == nil {
		t.Fatal("difficulty 1 passes at the height of a claim")
	}
	// a verifier that supplies a header of difficulty 1 for the height of a claim
	// gets the floor as the problem, not a check of the merkle root (the work
	// is judged first)
	cal, rc, _ := headerSetup(t)
	claimed, err := upgradeTarget(t, Options{}, cal.url()).Upgrade(context.Background(), rc)
	if err != nil || claimed.State != anchor.ReceiptClaimed {
		t.Fatalf("%+v %v", claimed, err)
	}
	ev, _ := anchor.ParseOTSEvidence(claimed.Evidence)
	chk, err := CheckEvidence(ev, testNow.Add(time.Hour), func(h uint64) ([]byte, bool) { return genesis, h == goodHeight })
	if err != nil || len(chk.Claims) != 1 || !strings.Contains(chk.Claims[0].HeaderProblem, "floor") {
		t.Fatalf("%+v %v", chk, err)
	}
}

// ---- A3-06: Upgrade has a bound of its own ----

func TestUpgradeHasADeadlineOfItsOwn(t *testing.T) {
	if DefaultUpgradeTimeout != 5*time.Minute {
		t.Fatalf("%v", DefaultUpgradeTimeout)
	}
	cal := newFakeCal(t, false)
	cal.set("slow")
	digest := sha([]byte("digest under test"))
	frag, _ := submittedFragment(digest, cal.url(), 1)
	rc := receiptOf(t, evidenceFor(t, digest, map[string][]byte{cal.url(): frag}, []string{cal.url()}))
	tg := upgradeTarget(t, Options{UpgradeTimeout: 300 * time.Millisecond}, cal.url())
	start := time.Now()
	_, err := tg.Upgrade(context.Background(), rc) // the caller's context has no deadline
	if err == nil || !anchor.IsRetryable(err) {
		t.Fatalf("%v", err)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("Upgrade took %v: the calendar request was bounded by its own 20 s only", d)
	}
}

// ---- A3-12: the tags cannot be written ----

func TestTagsAreFunctions(t *testing.T) {
	p := TagPending()
	p[0] = 0
	if TagPending()[0] != 0x83 || TagBitcoin() != [8]byte{0x05, 0x88, 0x96, 0x0d, 0x73, 0xd7, 0x19, 0x01} {
		t.Fatal("a tag changed")
	}
	n, err := NewBitcoin(5)
	if err != nil || n.Tag != TagBitcoin() {
		t.Fatalf("%+v %v", n, err)
	}
}
