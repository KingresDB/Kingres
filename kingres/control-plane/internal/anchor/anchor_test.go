// SPDX-License-Identifier: AGPL-3.0-only

package anchor

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 12, 1, 10, 0, 0, 0, time.UTC)

func note(size string, root []byte, sigs string) []byte {
	return []byte("kingres.example/agg\n" + size + "\n" + base64.StdEncoding.EncodeToString(root) + "\n\n" + sigs)
}

const sigLine = "\xe2\x80\x94 kingres.example/agg AAAAAA==\n"

func root32() []byte { return []byte(strings.Repeat("r", 32)) }

func TestNewPayload(t *testing.T) {
	n := note("17", root32(), sigLine)
	p, err := NewPayload("ab12", n, t0)
	if err != nil {
		t.Fatal(err)
	}
	text := "kingres.example/agg\n17\n" + base64.StdEncoding.EncodeToString(root32()) + "\n"
	want := sha256.Sum256([]byte(text))
	if p.ID != "ab12/17" || p.Origin != "kingres.example/agg" || p.Size != 17 || string(p.NoteText) != text || p.Digest != want || p.AggregateID() != "ab12" {
		t.Fatalf("%+v", p)
	}
	if p.DigestHex() != fmt.Sprintf("%x", want) {
		t.Fatal("hex")
	}
	// two signature lines are fine
	if _, err := NewPayload("ab", note("1", root32(), distinctSigs(2)), t0); err != nil {
		t.Fatal(err)
	}
	// the payload does not alias the input
	n[0] = 'X'
	if p.NoteText[0] == 'X' || p.SignedNote[0] == 'X' {
		t.Fatal("aliased")
	}
}

func TestNewPayloadRejects(t *testing.T) {
	r := root32()
	bad := map[string][]byte{
		"no-blank-line":   []byte("o\n1\n" + base64.StdEncoding.EncodeToString(r) + "\n" + sigLine),
		"no-signature":    note("1", r, ""),
		"sig-no-newline":  note("1", r, strings.TrimSuffix(sigLine, "\n")),
		"sig-malformed":   note("1", r, "- x AAAA\n"),
		"sig-bad-base64":  note("1", r, "\xe2\x80\x94 n !!!!\n"),
		"leading-zero":    note("01", r, sigLine),
		"sign":            note("+1", r, sigLine),
		"huge-size":       note("18446744073709551616", r, sigLine),
		"short-root":      note("1", r[:31], sigLine),
		"two-lines":       []byte("o\n1\n\n" + sigLine),
		"empty-origin":    []byte("\n1\n" + base64.StdEncoding.EncodeToString(r) + "\n\n" + sigLine),
		"space-in-origin": []byte("o r\n1\n" + base64.StdEncoding.EncodeToString(r) + "\n\n" + sigLine),
		"control-char":    []byte("o\x01\n1\n" + base64.StdEncoding.EncodeToString(r) + "\n\n" + sigLine),
		"invalid-utf8":    []byte("o\xff\n1\n" + base64.StdEncoding.EncodeToString(r) + "\n\n" + sigLine),
		"empty":           nil,
		"long-origin":     note("1", r, sigLine)[:0],
	}
	bad["long-origin"] = []byte(strings.Repeat("o", MaxOriginLen+1) + "\n1\n" + base64.StdEncoding.EncodeToString(r) + "\n\n" + sigLine)
	for name, n := range bad {
		if _, err := NewPayload("ab", n, t0); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	for _, id := range []string{"", "AB", "xyz", strings.Repeat("a", 129)} {
		if _, err := NewPayload(id, note("1", r, sigLine), t0); err == nil {
			t.Errorf("aggregate id %q accepted", id)
		}
	}
	// boundaries: a note of exactly MaxSignedNote bytes, and one more
	b := sizedNote(MaxSignedNote)
	if len(b) != MaxSignedNote {
		t.Fatalf("test setup: %d", len(b))
	}
	if _, err := NewPayload("ab", b, t0); err != nil {
		t.Fatal(err)
	}
	if _, err := NewPayload("ab", sizedNote(MaxSignedNote+1), t0); err == nil {
		t.Fatal("note over the limit accepted")
	}
}

func TestRecordFlow(t *testing.T) {
	r := NewRecord(1, "agg", 5, [32]byte{1}, t0)
	if r.State != StatePending || !r.Due(t0) {
		t.Fatal("new record")
	}
	// submitted then anchored
	nc := t0.Add(10 * time.Minute)
	r2, err := r.ApplyReceipt(Receipt{State: ReceiptSubmitted, Evidence: json.RawMessage(`{}`), NextCheck: &nc}, t0)
	if err != nil || r2.State != StateSubmitted || !r2.NextAttemptAt.Equal(nc) || r2.Due(t0) || !r2.Due(nc) {
		t.Fatalf("%+v %v", r2, err)
	}
	et := t0.Add(time.Hour)
	r3, err := r2.ApplyReceipt(Receipt{State: ReceiptAnchored, Evidence: json.RawMessage(`{"a":1}`), EvidenceTime: &et}, et)
	if err != nil || r3.State != StateAnchored || r3.AnchoredAt == nil || r3.NextAttemptAt != nil || !r3.EvidenceTime.Equal(et) || r3.Due(et) {
		t.Fatalf("%+v %v", r3, err)
	}
	// an anchored record takes nothing more
	if _, err := r3.ApplyReceipt(Receipt{State: ReceiptSubmitted}, et); err == nil {
		t.Fatal("anchored -> submitted")
	}
	if _, err := r3.Supersede(); err == nil {
		t.Fatal("anchored superseded")
	}
	if _, err := r2.Supersede(); err == nil {
		t.Fatal("submitted superseded")
	}
	if s, err := r.Supersede(); err != nil || s.State != StateSuperseded {
		t.Fatal("pending not superseded")
	}
	if _, err := r.ApplyReceipt(Receipt{State: "weird"}, t0); err == nil {
		t.Fatal("unknown receipt state")
	}
	// the original value is untouched (pure)
	if r.State != StatePending || r.Attempts != 0 {
		t.Fatal("mutated")
	}
}

func TestExpiryAndRetry(t *testing.T) {
	r := NewRecord(1, "agg", 5, [32]byte{}, t0)
	r.State = StateSubmitted
	if _, did := r.ExpireIfDue(t0.Add(7*24*time.Hour-time.Second), 7*24*time.Hour, 28*24*time.Hour); did {
		t.Fatal("expired early")
	}
	e, did := r.ExpireIfDue(t0.Add(7*24*time.Hour), 7*24*time.Hour, 28*24*time.Hour)
	if !did || e.State != StateExpired {
		t.Fatal("not expired")
	}
	// A2-02: a claimed record has its own, longer timeout: it does not expire
	// with the submitted one, and it does at its own
	c := NewRecord(1, "agg", 5, [32]byte{}, t0)
	c.State = StateClaimed
	if _, did := c.ExpireIfDue(t0.Add(7*24*time.Hour), 7*24*time.Hour, 28*24*time.Hour); did {
		t.Fatal("claimed record expired with the submitted timeout")
	}
	if _, did := c.ExpireIfDue(t0.Add(28*24*time.Hour-time.Second), 7*24*time.Hour, 28*24*time.Hour); did {
		t.Fatal("claimed record expired early")
	}
	ce, did := c.ExpireIfDue(t0.Add(28*24*time.Hour), 7*24*time.Hour, 28*24*time.Hour)
	if !did || ce.State != StateExpired || ce.NextAttemptAt != nil {
		t.Fatalf("claimed record: %+v", ce)
	}
	// a claimed record falls back to submitted on a submitted receipt
	nc := t0.Add(time.Minute)
	back, err := c.ApplyReceipt(Receipt{State: ReceiptSubmitted, Evidence: json.RawMessage(`{}`), NextCheck: &nc}, t0)
	if err != nil || back.State != StateSubmitted || !back.Due(nc) {
		t.Fatalf("%+v %v", back, err)
	}
	p := NewRecord(1, "agg", 5, [32]byte{}, t0)
	if _, did := p.ExpireIfDue(t0.Add(1000*time.Hour), time.Hour, time.Hour); did {
		t.Fatal("pending expired")
	}
	// an expired record is retried like a failed one (correction 88), and its
	// waiting period starts again: CreatedAt is the time of the Retry
	re, err := e.Retry(t0.Add(30 * 24 * time.Hour))
	if err != nil || re.State != StatePending || !re.CreatedAt.Equal(t0.Add(30*24*time.Hour)) || !re.Due(t0.Add(30*24*time.Hour)) {
		t.Fatalf("expired retried: %+v %v", re, err)
	}
	f := p
	f.State, f.Attempts, f.LastError = StateFailed, 4, "x"
	rr, err := f.Retry(t0)
	if err != nil || rr.State != StatePending || rr.Attempts != 0 || rr.LastError != "" || !rr.Due(t0) {
		t.Fatalf("%+v %v", rr, err)
	}
	if _, err := p.Retry(t0); err == nil {
		t.Fatal("pending retried")
	}
}

func TestBackoff(t *testing.T) {
	// u = 0.5 means no jitter
	want := []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 16 * time.Minute, 32 * time.Minute, time.Hour, time.Hour}
	for i, w := range want {
		if got := Backoff(i, 0.5, 0); got != w {
			t.Errorf("attempts %d: %v want %v", i, got, w)
		}
	}
	if Backoff(1000, 0.5, 0) != time.Hour || Backoff(-3, 0.5, 0) != 30*time.Second {
		t.Fatal("extremes")
	}
	// jitter of 20 percent either way
	lo, hi := Backoff(2, 0, 0), Backoff(2, 0.999999, 0)
	if lo != 96*time.Second || hi < 143*time.Second || hi > 144*time.Second {
		t.Fatalf("%v %v", lo, hi)
	}
	if Backoff(2, -5, 0) != lo || Backoff(2, 7, 0) != hi {
		t.Fatal("u out of range")
	}
	// Retry-After wins when larger, not when smaller
	if Backoff(0, 0.5, 5*time.Minute) != 5*time.Minute || Backoff(0, 0.5, time.Second) != 30*time.Second {
		t.Fatal("retry after")
	}
}

func TestUpgradeInterval(t *testing.T) {
	if UpgradeInterval(0) != 10*time.Minute || UpgradeInterval(3*time.Hour-1) != 10*time.Minute ||
		UpgradeInterval(3*time.Hour) != time.Hour || UpgradeInterval(100*time.Hour) != time.Hour {
		t.Fatal("interval")
	}
}

func TestErrorClasses(t *testing.T) {
	wrapped := fmt.Errorf("outer: %w", Errorf(ClassConflict, "worm", "put", "c", "x"))
	if ClassOf(wrapped) != ClassConflict || !IsConflict(wrapped) || IsRetryable(wrapped) || IsPermanent(wrapped) || IsRefused(wrapped) {
		t.Fatal("wrapped")
	}
	if CodeOf(wrapped) != "c" || RetryAfterOf(wrapped) != 0 {
		t.Fatal("code")
	}
	if IsRetryable(nil) || IsPermanent(nil) || IsConflict(nil) || IsRefused(nil) {
		t.Fatal("nil")
	}
	if ClassOf(context.Canceled) != ClassRetryable || ClassOf(errors.New("x")) != ClassPermanent {
		t.Fatal("foreign")
	}
	for c, n := range map[Class]string{ClassRetryable: "retryable", ClassPermanent: "permanent", ClassRefused: "refused", ClassConflict: "conflict", 0: "unknown"} {
		if c.String() != n {
			t.Fatal(c)
		}
	}
	for code, want := range map[int]Class{401: ClassRefused, 403: ClassRefused, 408: ClassRetryable, 425: ClassRetryable, 429: ClassRetryable,
		500: ClassRetryable, 503: ClassRetryable, 400: ClassPermanent, 404: ClassPermanent, 412: ClassPermanent} {
		if ClassOfStatus(code) != want {
			t.Errorf("status %d", code)
		}
	}
	e := &Error{Class: ClassPermanent, Target: "tsa", Op: "post", Text: "bad", Status: 502, Err: errors.New("secret detail")}
	if got := e.Error(); got != "tsa post: bad (http 502)" {
		t.Fatal(got)
	}
	if !errors.Is(e, e.Err) {
		t.Fatal("unwrap")
	}
}

func TestRetryAfterHeader(t *testing.T) {
	h := func(v string) http.Header { return http.Header{"Retry-After": {v}} }
	now := t0
	cases := map[string]time.Duration{
		"": 0, "7": 7 * time.Second, "0": 0, "-5": 0, "99999999": 24 * time.Hour, "abc": 0,
		now.Add(90 * time.Second).Format(http.TimeFormat): 90 * time.Second,
		now.Add(-time.Hour).Format(http.TimeFormat):       0,
		now.Add(1000 * time.Hour).Format(http.TimeFormat): 24 * time.Hour,
	}
	for in, want := range cases {
		if got := RetryAfter(h(in), now); got != want {
			t.Errorf("%q: %v want %v", in, got, want)
		}
	}
}

func TestTransportError(t *testing.T) {
	// a store that hangs
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/?token=SECRETQUERY", nil)
	_, err := Client(nil, 0).Do(req)
	e := TransportError("tsa", "post", err)
	if e.Class != ClassRetryable || e.Text != "timed out" || strings.Contains(e.Error(), "SECRETQUERY") || strings.Contains(e.Error(), "127.0.0.1") {
		t.Fatalf("%v", e)
	}
	// connection refused
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := l.Addr().String()
	l.Close()
	_, err = Client(nil, 0).Get("http://" + addr + "/?token=SECRETQUERY")
	if e := TransportError("tsa", "post", err); e.Class != ClassRetryable || strings.Contains(e.Error(), "SECRETQUERY") {
		t.Fatalf("%v", e)
	}
	if TransportError("a", "b", nil) != nil {
		t.Fatal("nil")
	}
	already := Errorf(ClassConflict, "a", "b", "c", "d")
	if TransportError("a", "b", fmt.Errorf("x: %w", already)) != already {
		t.Fatal("not passed through")
	}
	if e := TransportError("a", "b", io.ErrUnexpectedEOF); e.Class != ClassRetryable || e.Text != "connection cut" {
		t.Fatal(e)
	}
	if e := TransportError("a", "b", ErrTooLarge); e.Class != ClassPermanent {
		t.Fatal(e)
	}
}

func TestClientRedirects(t *testing.T) {
	var otherHits int
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { otherHits++ }))
	defer other.Close()
	var same *httptest.Server
	same = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/other":
			http.Redirect(w, r, other.URL, 307)
		case "/loop":
			http.Redirect(w, r, "/loop", 302)
		case "/ok":
			http.Redirect(w, r, "/done", 302)
		default:
			fmt.Fprint(w, "done")
		}
	}))
	defer same.Close()
	// no redirects at all: the 3xx is returned
	resp, err := Client(nil, 0).Get(same.URL + "/ok")
	if err != nil || resp.StatusCode != 302 {
		t.Fatalf("%v %v", resp, err)
	}
	resp.Body.Close()
	// same host allowed
	resp, err = Client(nil, 3).Get(same.URL + "/ok")
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("%v %v", resp, err)
	}
	resp.Body.Close()
	// other host refused, never contacted
	_, err = Client(nil, 3).Get(same.URL + "/other")
	if !errors.Is(err, ErrRedirectRefused) || otherHits != 0 {
		t.Fatalf("%v hits %d", err, otherHits)
	}
	// a loop is cut
	if _, err = Client(nil, 3).Get(same.URL + "/loop"); !errors.Is(err, ErrRedirectRefused) {
		t.Fatal(err)
	}
	// the caller's client is not changed
	c := &http.Client{Timeout: time.Second}
	cc := Client(c, 2)
	if c.CheckRedirect != nil || cc.Timeout != time.Second {
		t.Fatal("client copy")
	}
}

func TestReadLimited(t *testing.T) {
	for _, n := range []int{0, 1, 99, 100} {
		if b, err := ReadLimited(strings.NewReader(strings.Repeat("x", n)), 100); err != nil || len(b) != n {
			t.Fatalf("%d: %v", n, err)
		}
	}
	if _, err := ReadLimited(strings.NewReader(strings.Repeat("x", 101)), 100); !errors.Is(err, ErrTooLarge) {
		t.Fatal(err)
	}
}

func TestSecretAndRedactor(t *testing.T) {
	s := Secret("hunter2-secret-value")
	type holder struct{ S Secret }
	outs := []string{fmt.Sprint(s), fmt.Sprintf("%v %+v %#v %s %q %x %d", s, s, s, s, s, s, s), fmt.Sprintf("%+v %#v", holder{s}, holder{s}), fmt.Sprintf("%v", &holder{s})}
	for _, f := range []any{s, holder{s}, []Secret{s}, map[string]Secret{"k": s}} {
		j, err := json.Marshal(f)
		if err != nil {
			t.Fatal(err)
		}
		outs = append(outs, string(j))
	}
	for _, o := range outs {
		if strings.Contains(o, "hunter2") {
			t.Fatalf("leak in %q", o)
		}
	}
	if s.Reveal() != "hunter2-secret-value" {
		t.Fatal("reveal")
	}
	r := NewRedactor(s, "ab", "") // short values and empty ones are ignored
	got := r.Clean("x hunter2-secret-value y ab\n\xff"+strings.Repeat("z", 400), 0)
	if strings.Contains(got, "hunter2") || !strings.Contains(got, RedactedText) || !strings.Contains(got, " ab?") || len(got) > 300 {
		t.Fatalf("%q", got)
	}
	// a secret cut by the length limit leaves no prefix of 4 or more bytes
	got = r.Clean("aaaaaaaa hunter2-secret-value", 8+1+9)
	if strings.Contains(got, "hunt") {
		t.Fatalf("%q", got)
	}
	// A2-06: a secret that the text ends in the middle of (a message that the
	// store itself cut) is trimmed for every cut from all but its last byte
	// down to its first 4 bytes; a cut of 3 bytes cannot be told from text and
	// stays (documented: the first three bytes may show)
	const secret = "hunter2-secret-value"
	for n := len(secret) - 1; n >= 4; n-- {
		if got = r.Clean("aaaaaaaa "+secret[:n], 0); got != "aaaaaaaa " {
			t.Errorf("a secret cut after %d bytes leaves %q", n, got)
		}
	}
	if got = r.Clean("aaaaaaaa "+secret[:3], 0); got != "aaaaaaaa hun" {
		t.Errorf("a cut of 3 bytes: %q", got)
	}
	for _, o := range []string{fmt.Sprintf("%v", r), fmt.Sprintf("%+v", r), fmt.Sprintf("%#v", r)} {
		if strings.Contains(o, "hunter2") {
			t.Fatalf("redactor leaks: %q", o)
		}
	}
	if j, _ := json.Marshal(r); strings.Contains(string(j), "hunter2") {
		t.Fatal("redactor json")
	}
}

func TestTestResult(t *testing.T) {
	var r TestResult
	r.Add("a", CheckOK, "fine %d", 1)
	r.Add("b", CheckWarning, "hm")
	if !r.OK() || r.Checks[0].Text != "fine 1" {
		t.Fatal("ok")
	}
	r.Add("c", CheckFailed, "bad")
	if r.OK() {
		t.Fatal("failed check")
	}
	j, _ := json.Marshal(r)
	if !strings.Contains(string(j), `"status":"failed"`) {
		t.Fatal(string(j))
	}
}

// distinctSigs returns n signature lines that differ in their key id (the
// verifier refuses two lines with the same name and key id).
func distinctSigs(n int) string {
	var sb strings.Builder
	for i := 0; i < n; i++ {
		sb.WriteString("\xe2\x80\x94 kingres.example/agg " + base64.StdEncoding.EncodeToString([]byte{0, 0, byte(i >> 8), byte(i)}) + "\n")
	}
	return sb.String()
}
