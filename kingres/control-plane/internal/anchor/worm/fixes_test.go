// SPDX-License-Identifier: AGPL-3.0-only

package worm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/anchor"
)

func keyOf(size string) string {
	return "kingres-anchors/" + aggID + "/" + strings.Repeat("0", 20-len(size)) + size + ".note"
}

// A1-08: an object that the store accepted without a lock is completed by a
// later Submit: the retention is applied to that version, the key keeps one
// version, and the evidence names it.
func TestUnlockedObjectIsCompletedByLaterSubmit(t *testing.T) {
	f := newFakeS3()
	f.noLock = true
	defer f.close()
	tg := newTarget(t, f)
	p := testPayload(t, "4", 1)
	_, err := tg.Submit(context.Background(), p)
	expectErr(t, err, anchor.ClassPermanent, "not_locked")
	if vs := f.stored(keyOf("4")); len(vs) != 1 || vs[0].mode != "" {
		t.Fatal("setup: the object should exist, unlocked")
	}
	f.mu.Lock()
	f.noLock = false // the operator fixed the bucket
	f.mu.Unlock()
	rc, ev := mustSubmit(t, tg, p)
	noSigErrors(t, f)
	vs := f.stored(keyOf("4"))
	if len(vs) != 1 || vs[0].mode != "COMPLIANCE" || ev.VersionID != vs[0].id || ev.LockMode != "COMPLIANCE" || rc.State != anchor.ReceiptAnchored {
		t.Fatalf("%+v %+v", ev, vs[0])
	}
	if f.retentionCalls != 1 {
		t.Fatalf("%d retention calls", f.retentionCalls)
	}
	until, _ := time.Parse(anchor.TimeLayout, ev.RetainUntil)
	if d := time.Until(until); d < 29*24*time.Hour || d > 31*24*time.Hour {
		t.Fatalf("retention %s", ev.RetainUntil)
	}
}

// A1-08: when the store refuses the retention the error says exactly what the
// operator must do (key, version, mode, date), and once the operator has done
// it the next Submit completes: no record is left that can never progress.
func TestUnlockedObjectStoreRefusesRetention(t *testing.T) {
	f := newFakeS3()
	f.noLock = true
	f.retentionStatus = 403
	defer f.close()
	tg := newTarget(t, f)
	p := testPayload(t, "4", 1)
	_, _ = tg.Submit(context.Background(), p)
	_, err := tg.Submit(context.Background(), p)
	ae := expectErr(t, err, anchor.ClassPermanent, "not_locked")
	vs := f.stored(keyOf("4"))
	for _, want := range []string{keyOf("4"), vs[0].id, "COMPLIANCE", "Retry", "tools"} {
		if !strings.Contains(ae.Error(), want) {
			t.Errorf("the text does not say %q: %v", want, ae)
		}
	}
	// the operator locks that version with the store's own tools
	f.mu.Lock()
	vs[0].mode, vs[0].retain = "COMPLIANCE", time.Now().Add(30*24*time.Hour).UTC().Format("2006-01-02T15:04:05.000Z")
	f.mu.Unlock()
	if _, ev := mustSubmit(t, tg, p); ev.VersionID != vs[0].id {
		t.Fatal("not the same version")
	}
}

// A1-08: a version whose retention is shorter than asked is extended in the
// configured mode; a version locked in another mode is not touched (a mode
// cannot be changed): the error says so.
func TestExistingShortRetentionAndOtherMode(t *testing.T) {
	f := newFakeS3()
	defer f.close()
	tg := newTarget(t, f)
	p := testPayload(t, "5", 1)
	v := f.addVersion(keyOf("5"), p.SignedNote, "COMPLIANCE", time.Now().Add(10*24*time.Hour))
	_, ev := mustSubmit(t, tg, p)
	until, _ := time.Parse(anchor.TimeLayout, ev.RetainUntil)
	if ev.VersionID != v.id || time.Until(until) < 29*24*time.Hour || f.retentionCalls != 1 {
		t.Fatalf("%+v retention calls %d", ev, f.retentionCalls)
	}
	g := newFakeS3()
	defer g.close()
	tg = newTarget(t, g)
	p = testPayload(t, "6", 1)
	g.addVersion(keyOf("6"), p.SignedNote, "GOVERNANCE", time.Now().Add(40*24*time.Hour))
	_, err := tg.Submit(context.Background(), p)
	ae := expectErr(t, err, anchor.ClassPermanent, "not_locked")
	if g.retentionCalls != 0 || !strings.Contains(ae.Error(), "GOVERNANCE") {
		t.Fatalf("retention calls %d: %v", g.retentionCalls, ae)
	}
}

// A1-10: a foreign version that someone else put over the key later is not a
// conflict of the anchor: our version is still there, locked.
func TestLaterForeignVersionIsNotAConflict(t *testing.T) {
	for _, page := range []int{0, 1} {
		f := newFakeS3()
		f.listPage = page
		tg := newTarget(t, f)
		p := testPayload(t, "7", 1)
		_, first := mustSubmit(t, tg, p)
		f.addVersion(keyOf("7"), []byte("someone else wrote this"), "", time.Time{})
		f.addVersion(keyOf("7"), []byte("and this"), "", time.Time{})
		rc, ev := mustSubmit(t, tg, p)
		if ev.VersionID != first.VersionID || rc.State != anchor.ReceiptAnchored || ev.ObjectSHA256 != first.ObjectSHA256 {
			t.Fatalf("page %d: %+v vs %+v", page, ev, first)
		}
		if f.listCalls == 0 {
			t.Error("the versions of the key were not listed")
		}
		// the read-back fetched OUR version, not the latest
		var getOurs bool
		for _, r := range f.requests() {
			if r.method == "GET" && strings.Contains(r.uri, "versionId="+first.VersionID) {
				getOurs = true
			}
		}
		if !getOurs {
			t.Error("no GET of our version id")
		}
		// without foreign versions of our bytes, a foreign-only key is still a conflict
		q := testPayload(t, "8", 1)
		f.addVersion(keyOf("8"), []byte("other"), "COMPLIANCE", time.Now().Add(24*time.Hour))
		_, err := tg.Submit(context.Background(), q)
		expectErr(t, err, anchor.ClassConflict, "worm_conflict")
		f.close()
	}
	// many foreign versions after ours: still found (up to the bound)
	f := newFakeS3()
	defer f.close()
	tg := newTarget(t, f)
	p := testPayload(t, "9", 1)
	_, first := mustSubmit(t, tg, p)
	for i := 0; i < 12; i++ {
		f.addVersion(keyOf("9"), []byte{byte(i), 1, 2}, "", time.Time{})
	}
	if _, ev := mustSubmit(t, tg, p); ev.VersionID != first.VersionID {
		t.Fatal("our version not found behind 12 foreign ones")
	}
}

// A1-09: the evidence carries the SHA-256 and the length of the object that was
// read back, and the version id it was read from.
func TestEvidenceNamesTheObject(t *testing.T) {
	f := newFakeS3()
	defer f.close()
	tg := newTarget(t, f)
	p := testPayload(t, "10", 1)
	_, ev := mustSubmit(t, tg, p)
	sum := sha256.Sum256(p.SignedNote)
	vs := f.stored(keyOf("10"))
	if ev.ObjectSHA256 != hex.EncodeToString(sum[:]) || ev.ContentLength != uint64(len(p.SignedNote)) || ev.VersionID != vs[0].id {
		t.Fatalf("%+v", ev)
	}
	// the read-back GET asked for that version
	var versioned bool
	for _, r := range f.requests() {
		if r.method == "GET" && strings.Contains(r.uri, "versionId="+ev.VersionID) {
			versioned = true
		}
	}
	if !versioned {
		t.Fatal("the read-back did not fetch the version")
	}
	// the object is read back from the store, not assumed: wrong bytes fail
	g := newFakeS3()
	g.badGetBody = true
	defer g.close()
	if _, err := newTarget(t, g).Submit(context.Background(), p); err == nil {
		t.Fatal("wrong bytes accepted")
	}
}

// A1-07: the time that the store states is bounded against the clock, on a
// fresh write and on the object an earlier attempt wrote.
func TestStoreTimeIsBounded(t *testing.T) {
	for name, skew := range map[string]time.Duration{"ahead-1h": time.Hour, "behind-1h": -time.Hour, "ahead-20y": 20 * 365 * 24 * time.Hour, "behind-20y": -20 * 365 * 24 * time.Hour} {
		f := newFakeS3()
		f.skew = skew
		_, err := newTarget(t, f).Submit(context.Background(), testPayload(t, "11", 1))
		ae := expectErr(t, err, anchor.ClassRefused, "store_time_skew")
		if !strings.Contains(ae.Error(), "clock") {
			t.Errorf("%s: %v", name, ae)
		}
		f.close()
	}
	f := newFakeS3()
	f.skew = 2 * time.Minute
	if _, ev := mustSubmit(t, newTarget(t, f), testPayload(t, "11", 1)); ev.LastModified == "" {
		t.Fatal("2 minutes of skew refused")
	}
	f.close()
	// tolerance from the configuration
	f = newFakeS3()
	f.skew = time.Hour
	defer f.close()
	tg := newTarget(t, f, func(c *Config, o *Options) { c.MaxSkewSeconds = 2 * 3600 })
	mustSubmit(t, tg, testPayload(t, "11", 1))
	// the object of an earlier attempt: Last-Modified 20 years ago or 5 years ahead
	for _, lm := range map[string]time.Duration{"old": -20 * 365 * 24 * time.Hour, "future": 5 * 365 * 24 * time.Hour} {
		g := newFakeS3()
		tg := newTarget(t, g)
		p := testPayload(t, "12", 1)
		v := g.addVersion(keyOf("12"), p.SignedNote, "COMPLIANCE", time.Now().Add(30*24*time.Hour))
		g.mu.Lock()
		v.lastMod = time.Now().Add(lm).UTC().Truncate(time.Second)
		g.mu.Unlock()
		_, err := tg.Submit(context.Background(), p)
		expectErr(t, err, anchor.ClassRefused, "store_time_skew")
		g.close()
	}
	// a recent earlier object is fine
	g := newFakeS3()
	defer g.close()
	p := testPayload(t, "12", 1)
	g.addVersion(keyOf("12"), p.SignedNote, "COMPLIANCE", time.Now().Add(30*24*time.Hour))
	if _, ev := mustSubmit(t, newTarget(t, g), p); ev.ObjectSHA256 == "" {
		t.Fatal()
	}
}

// A1-12: a read-back that fails with a permanent-class answer is not retried
// for ever: it counts as refused (a few tries, then failed); a 5xx is still
// retried.
func TestReadBackFailureEndsInRefused(t *testing.T) {
	f := newFakeS3()
	f.headStatus = 404
	defer f.close()
	tg := newTarget(t, f)
	p := testPayload(t, "13", 1)
	_, err := tg.Submit(context.Background(), p)
	expectErr(t, err, anchor.ClassRefused, "verify_read_failed")
	// the retry finds the key (412) and fails the same way, so it ends
	_, err = tg.Submit(context.Background(), p)
	expectErr(t, err, anchor.ClassRefused, "verify_read_failed")
	g := newFakeS3()
	g.headStatus = 503
	defer g.close()
	_, err = newTarget(t, g).Submit(context.Background(), p)
	expectErr(t, err, anchor.ClassRetryable, "http_status")
	// driven through the record: refused four times in a row fails it
	r := anchor.NewRecord(1, "agg", 13, p.Digest, time.Now())
	var alert string
	for i := 0; i < 4; i++ {
		_, err = tg.Submit(context.Background(), p)
		r, alert, _ = r.AfterError(err, time.Now(), 0.5, anchor.Limits{})
	}
	if r.State != anchor.StateFailed || alert != anchor.AlertFailed {
		t.Fatalf("%+v %s", r, alert)
	}
}

// A1-17: a clock that the store finds wrong is not a refusal of the credentials.
func TestRequestTimeTooSkewedIsRetryable(t *testing.T) {
	f := newFakeS3()
	f.putStatus, f.putCode, f.putMsg = 403, "RequestTimeTooSkewed", "The difference between the request time and the current time is too large."
	defer f.close()
	_, err := newTarget(t, f).Submit(context.Background(), testPayload(t, "14", 1))
	ae := expectErr(t, err, anchor.ClassRetryable, "clock_skew")
	if !strings.Contains(ae.Error(), "clock") {
		t.Fatal(ae)
	}
	// any other 403 is still a refusal
	g := newFakeS3()
	g.putStatus, g.putCode, g.putMsg = 403, "AccessDenied", "no"
	defer g.close()
	_, err = newTarget(t, g).Submit(context.Background(), testPayload(t, "14", 1))
	expectErr(t, err, anchor.ClassRefused, "")
}

// N2 and the design: compliance is the default; governance is an explicit
// choice that the configuration and the Test report as a warning.
func TestGovernanceIsExplicitAndWarned(t *testing.T) {
	c, err := Config{Region: "r1", Bucket: "my-bucket"}.Normalize()
	if err != nil || c.Mode != ModeCompliance || len(c.Warnings()) != 0 {
		t.Fatalf("%+v %v", c, err)
	}
	g, err := Config{Region: "r1", Bucket: "my-bucket", Mode: "governance"}.Normalize()
	if err != nil || g.Mode != ModeGovernance || len(g.Warnings()) != 1 || !strings.Contains(g.Warnings()[0], "BypassGovernanceRetention") {
		t.Fatalf("%+v %v", g, err)
	}
	f := newFakeS3()
	defer f.close()
	res := newTarget(t, f, mode("governance")).Test(context.Background())
	var warned bool
	for _, ch := range res.Checks {
		if ch.Name == "mode" && ch.Status == anchor.CheckWarning && strings.Contains(ch.Text, "governance") {
			warned = true
		}
	}
	if !warned || !res.OK() {
		t.Fatalf("%+v", res)
	}
	// a Submit in governance mode says GOVERNANCE in the evidence
	if _, ev := mustSubmit(t, newTarget(t, f, mode("governance")), testPayload(t, "15", 1)); ev.LockMode != "GOVERNANCE" {
		t.Fatal(ev.LockMode)
	}
}

// A1-18: a payload that was not built consistently is refused before anything is sent.
func TestSubmitRefusesInconsistentPayload(t *testing.T) {
	f := newFakeS3()
	defer f.close()
	tg := newTarget(t, f)
	good := testPayload(t, "16", 1)
	bad := map[string]anchor.Payload{
		"zero":   {},
		"digest": func() anchor.Payload { p := good; p.Digest[3] ^= 1; return p }(),
		"size":   func() anchor.Payload { p := good; p.Size = 99; return p }(),
		"text":   func() anchor.Payload { p := good; p.NoteText = []byte("x\n"); return p }(),
	}
	for name, p := range bad {
		_, err := tg.Submit(context.Background(), p)
		if err == nil || anchor.CodeOf(err) != "bad_payload" || !anchor.IsPermanent(err) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if len(f.requests()) != 0 {
		t.Fatal("a request was sent")
	}
}

func TestVerifyNoteAndHead(t *testing.T) {
	f := newFakeS3()
	defer f.close()
	p := testPayload(t, "17", 1)
	_, ev := mustSubmit(t, newTarget(t, f), p)
	if err := VerifyNote(ev, aggID, p.SignedNote, p.DigestHex()); err != nil {
		t.Fatal(err)
	}
	other := testPayload(t, "17", 2)
	if VerifyNote(ev, aggID, other.SignedNote, other.DigestHex()) == nil {
		t.Error("another note accepted")
	}
	if VerifyNote(ev, aggID, p.SignedNote, other.DigestHex()) == nil {
		t.Error("another payload digest accepted")
	}
	if VerifyNote(ev, "ab", p.SignedNote, p.DigestHex()) == nil {
		t.Error("another aggregate accepted")
	}
	long := append(append([]byte(nil), p.SignedNote...), []byte(strings.Repeat("\xe2\x80\x94 kingres.example/aggregate AAAAAA==\n", 1))...)
	if VerifyNote(ev, aggID, long, p.DigestHex()) == nil {
		t.Error("a note with another length accepted")
	}
	vs := f.stored(keyOf("17"))
	h := http.Header{}
	h.Set("X-Amz-Version-Id", vs[0].id)
	h.Set("X-Amz-Object-Lock-Mode", vs[0].mode)
	h.Set("X-Amz-Object-Lock-Retain-Until-Date", vs[0].retain)
	h.Set("Content-Length", strconv.Itoa(len(p.SignedNote)))
	if err := VerifyHead(ev, h); err != nil {
		t.Fatal(err)
	}
	h.Set("X-Amz-Object-Lock-Mode", "GOVERNANCE")
	if VerifyHead(ev, h) == nil {
		t.Error("another mode accepted")
	}
	h.Set("X-Amz-Object-Lock-Mode", vs[0].mode)
	h.Set("X-Amz-Version-Id", "other")
	if VerifyHead(ev, h) == nil {
		t.Error("another version accepted")
	}
}

func itoa(n int) string { return strings.TrimSpace(strings.Repeat(" ", 0) + intString(n)) }

func intString(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
