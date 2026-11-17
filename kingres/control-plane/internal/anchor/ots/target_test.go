// SPDX-License-Identifier: AGPL-3.0-only

package ots

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/anchor"
)

// dash is U+2014, the first character of a signature line of a signed note.
var dash = string(rune(0x2014))

var testNow = time.Date(2026, 12, 1, 10, 0, 0, 0, time.UTC)

func testPayload(t testing.TB) anchor.Payload {
	t.Helper()
	root := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))
	sig := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{9}, 68))
	note := "kingres.example/aggregate\n17\n" + root + "\n\n" + dash + " kingres.example/aggregate " + sig + "\n"
	p, err := anchor.NewPayload("ab12", []byte(note), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func seqBytes(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i)
	}
	return b
}

func newTarget(t testing.TB, client *http.Client, min int, urls ...string) *Target {
	t.Helper()
	tg, err := New(Config{Calendars: urls, MinCalendars: min, AllowInsecure: true}, Options{
		Client: client, Clock: func() time.Time { return testNow },
		Rand: bytes.NewReader(seqBytes(256)),
	})
	if err != nil {
		t.Fatal(err)
	}
	return tg
}

func TestSubmitUpgradeExport(t *testing.T) {
	a, b := newFakeCal(t, false), newFakeCal(t, false)
	tg := newTarget(t, nil, 2, a.url(), b.url())
	p := testPayload(t)
	rc, err := tg.Submit(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if rc.State != anchor.ReceiptSubmitted || rc.NextCheck == nil || !rc.NextCheck.Equal(testNow.Add(FirstCheck)) || rc.EvidenceTime != nil {
		t.Fatalf("receipt %+v", rc)
	}
	if len(a.badReq)+len(b.badReq) != 0 || !bytes.Equal(a.lastBody, p.Digest[:]) {
		t.Errorf("request problems: %v %v, body %x", a.badReq, b.badReq, a.lastBody)
	}
	ev, err := anchor.ParseOTSEvidence(rc.Evidence)
	if err != nil {
		t.Fatal(err)
	}
	if ev.Digest != p.DigestHex() || len(ev.Calendars) != 2 || ev.Bitcoin != nil || ev.Calendars[0].URI != a.url() {
		t.Fatalf("evidence %s", rc.Evidence)
	}
	// the evidence is canonical: re-encoding gives the same bytes
	again, _ := anchor.EncodeEvidence(ev)
	if !bytes.Equal(again, rc.Evidence) {
		t.Error("evidence is not canonical")
	}
	// calendars not confirmed: 404, nothing changes
	rc2, err := tg.Upgrade(context.Background(), rc)
	if err != nil || rc2.State != anchor.ReceiptSubmitted || !bytes.Equal(rc2.Evidence, rc.Evidence) {
		t.Fatalf("upgrade before confirmation: %+v %v", rc2, err)
	}
	if _, g := a.counts(); g != 1 {
		t.Errorf("gets %d", g)
	}
	// only calendar B confirms
	b.mu.Lock()
	b.bitcoin, b.height = true, 960000
	b.mu.Unlock()
	rc3, err := tg.Upgrade(context.Background(), rc2)
	// no header source is configured: a calendar's attestation is a claim, not an anchor
	if err != nil || rc3.State != anchor.ReceiptClaimed || rc3.EvidenceTime != nil || rc3.NextCheck == nil || !rc3.NextCheck.Equal(testNow.Add(FirstCheck)) {
		t.Fatalf("upgrade: %+v %v", rc3, err)
	}
	ev3, _ := anchor.ParseOTSEvidence(rc3.Evidence)
	if ev3.Bitcoin == nil || ev3.Bitcoin.Height != 960000 || ev3.Bitcoin.CalendarURI != b.url() || ev3.Bitcoin.Status != anchor.BitcoinClaimed || ev3.Bitcoin.Header != "" {
		t.Fatalf("bitcoin %+v", ev3.Bitcoin)
	}
	if ev3.SubmittedAt != anchor.FormatTime(testNow) {
		t.Fatalf("submitted_at %s", ev3.SubmittedAt)
	}
	// calendar A keeps its pending attestation, B's was replaced
	for _, c := range ev3.Calendars {
		raw, _ := base64.StdEncoding.DecodeString(c.FragmentB64)
		d, _ := hex.DecodeString(ev3.Digest)
		n, err := ParseFragment(raw, d)
		if err != nil {
			t.Fatal(err)
		}
		var pend, btc int
		for _, l := range n.Leaves() {
			if l.Att.Kind == AttPending {
				pend++
			} else if l.Att.Kind == AttBitcoin {
				btc++
			}
		}
		if c.URI == a.url() && (pend != 1 || btc != 0) || c.URI == b.url() && (pend != 0 || btc != 1) {
			t.Errorf("%s: %d pending %d bitcoin", c.URI, pend, btc)
		}
	}
	// a claimed receipt whose other fragment still holds a pending attestation
	// asks that calendar again (A2-02), and not the one that has delivered
	_, g0 := b.counts()
	_, a0 := a.counts()
	rc4, err := tg.Upgrade(context.Background(), rc3)
	if err != nil || rc4.State != anchor.ReceiptClaimed || !bytes.Equal(rc4.Evidence, rc3.Evidence) || rc4.NextCheck == nil {
		t.Fatalf("claimed upgrade changed: %+v %v", rc4, err)
	}
	if _, g1 := b.counts(); g1 != g0 {
		t.Error("the calendar that had delivered was asked again")
	}
	if _, a1 := a.counts(); a1 != a0+1 {
		t.Errorf("the calendar with a pending attestation was asked %d times, want 1", a1-a0)
	}
	// the exported file: parses, re-serialises identically, holds both a pending and a bitcoin leaf
	file, err := Export(ev3)
	if err != nil {
		t.Fatal(err)
	}
	f, err := ParseFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if again, _ := f.Serialize(); !bytes.Equal(again, file) {
		t.Error("export is not canonical")
	}
	if f.HashOp != TagSHA256 || !bytes.Equal(f.Root.Msg, p.Digest[:]) {
		t.Error("export header")
	}
	kinds := map[AttKind]int{}
	for _, l := range f.Root.Leaves() {
		kinds[l.Att.Kind]++
	}
	if kinds[AttPending] != 1 || kinds[AttBitcoin] != 1 {
		t.Errorf("leaves %v", kinds)
	}
	if !bytes.HasPrefix(file, Magic()) {
		t.Error("magic")
	}
	// the evidence file of the publication round trips
	ef, err := anchor.EncodeEvidenceFile(anchor.EvidenceFile{Kind: anchor.KindOTS, Origin: p.Origin, Size: p.Size,
		PayloadSHA256: p.DigestHex(), CreatedAt: testNow, Evidence: rc3.Evidence})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := anchor.ParseEvidenceFile(ef); err != nil {
		t.Fatal(err)
	}
}

func TestSubmitPartialAndPrior(t *testing.T) {
	a, b := newFakeCal(t, false), newFakeCal(t, false)
	b.set("503")
	tg := newTarget(t, nil, 2, a.url(), b.url())
	p := testPayload(t)
	_, err := tg.Submit(context.Background(), p)
	var pe *PartialError
	if !errors.As(err, &pe) {
		t.Fatalf("err %v", err)
	}
	if !anchor.IsRetryable(err) || anchor.IsPermanent(err) || anchor.CodeOf(err) != "too_few_calendars" {
		t.Errorf("class %v code %s", anchor.ClassOf(err), anchor.CodeOf(err))
	}
	if len(pe.Evidence.Calendars) != 1 || pe.Evidence.Calendars[0].URI != a.url() {
		t.Fatalf("partial evidence %+v", pe.Evidence)
	}
	if _, perr := anchor.EncodeEvidence(pe.Evidence); perr != nil {
		t.Errorf("partial evidence does not encode: %v", perr)
	}
	b.set("ok")
	rc, err := tg.SubmitWithPrior(context.Background(), p, &pe.Evidence)
	if err != nil {
		t.Fatal(err)
	}
	if pa, _ := a.counts(); pa != 1 {
		t.Errorf("calendar A asked %d times, want 1", pa)
	}
	if pb, _ := b.counts(); pb != 2 {
		t.Errorf("calendar B asked %d times, want 2", pb)
	}
	ev, _ := anchor.ParseOTSEvidence(rc.Evidence)
	if len(ev.Calendars) != 2 || ev.Calendars[0].FragmentB64 != pe.Evidence.Calendars[0].FragmentB64 {
		t.Error("kept fragment changed")
	}
	// prior for another digest
	other := pe.Evidence
	other.Digest = strings.Repeat("00", 32)
	if _, err := tg.SubmitWithPrior(context.Background(), p, &other); !anchor.IsPermanent(err) {
		t.Errorf("prior of another digest: %v", err)
	}
	// min 1: one answer is enough
	b.set("503")
	tg1 := newTarget(t, nil, 1, a.url(), b.url())
	if _, err := tg1.Submit(context.Background(), p); err != nil {
		t.Errorf("min 1: %v", err)
	}
}

func TestSubmitFailureModes(t *testing.T) {
	p := testPayload(t)
	cases := []struct {
		mode      string
		retryable bool
		status    int
	}{
		{"500", true, 500}, {"503", true, 503}, {"429", true, 429}, {"403", false, 403},
		{"huge", false, 0}, {"cut", true, 0}, {"garbage", false, 0}, {"noattest", false, 0},
	}
	for _, c := range cases {
		t.Run(c.mode, func(t *testing.T) {
			a := newFakeCal(t, false)
			a.set(c.mode)
			tg := newTarget(t, nil, 1, a.url())
			_, err := tg.Submit(context.Background(), p)
			if err == nil {
				t.Fatal("accepted")
			}
			if anchor.IsRetryable(err) != c.retryable {
				t.Errorf("class %v, retryable want %v: %v", anchor.ClassOf(err), c.retryable, err)
			}
			if c.mode == "429" && anchor.RetryAfterOf(err) != 7*time.Second {
				t.Errorf("retry after %v", anchor.RetryAfterOf(err))
			}
			var ae *anchor.Error
			if !errors.As(err, &ae) {
				t.Fatal("not an anchor error")
			}
			var inner *anchor.Error
			if !errors.As(ae.Err, &inner) || (c.status != 0 && inner.Status != c.status) {
				t.Errorf("inner %+v", ae.Err)
			}
		})
	}
}

func TestSlowCalendarHonoursContext(t *testing.T) {
	a := newFakeCal(t, false)
	a.set("slow")
	tg := newTarget(t, nil, 1, a.url())
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := tg.Submit(ctx, testPayload(t))
	if time.Since(start) > 5*time.Second {
		t.Fatal("did not return at the context deadline")
	}
	if !anchor.IsRetryable(err) {
		t.Errorf("slow calendar: %v", err)
	}
}

func TestRedirects(t *testing.T) {
	p := testPayload(t)
	target := newFakeCal(t, false) // another host:port
	a := newFakeCal(t, false)
	a.set("redirect-other")
	a.other = target.url() + "/digest"
	tg := newTarget(t, nil, 1, a.url())
	_, err := tg.Submit(context.Background(), p)
	if err == nil || anchor.IsRetryable(err) {
		t.Fatalf("redirect to another host: %v", err)
	}
	if posts, _ := target.counts(); posts != 0 {
		t.Fatal("the redirect target was contacted")
	}
	// same host redirect is followed
	a.set("redirect-same")
	if _, err := tg.Submit(context.Background(), p); err != nil {
		t.Errorf("same-host redirect: %v", err)
	}
}

func TestTLSCalendar(t *testing.T) {
	a := newFakeCal(t, true)
	p := testPayload(t)
	// https is accepted without allowInsecure, with the server's client (trusts its certificate)
	tg, err := New(Config{Calendars: []string{a.url()}}, Options{Client: a.srv.Client(), Clock: func() time.Time { return testNow }})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tg.Submit(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	// the default client does not trust the test certificate: permanent failure
	tg2, _ := New(Config{Calendars: []string{a.url()}}, Options{})
	_, err = tg2.Submit(context.Background(), p)
	if err == nil || anchor.IsRetryable(err) {
		t.Errorf("untrusted certificate: %v", err)
	}
}

func TestUpgradeSkipsForeignCalendars(t *testing.T) {
	a, evil := newFakeCal(t, false), newFakeCal(t, false)
	evil.bitcoin = true
	tg := newTarget(t, nil, 1, a.url())
	p := testPayload(t)
	// a fragment from "a" whose pending attestation names another server
	n := NewNode(p.Digest[:])
	x, _ := n.Child(Op{Tag: TagSHA256})
	att, _ := NewPending(evil.url())
	x.AddAttestation(att)
	raw, _ := n.Serialize()
	ev := anchor.OTSEvidence{Digest: p.DigestHex(), SubmittedAt: anchor.FormatTime(testNow), Calendars: []anchor.OTSCalendar{{URI: a.url(), FragmentB64: base64.StdEncoding.EncodeToString(raw)}}}
	enc, _ := anchor.EncodeEvidence(ev)
	rc, err := tg.Upgrade(context.Background(), anchor.Receipt{State: anchor.ReceiptSubmitted, Evidence: enc})
	if err != nil || rc.State != anchor.ReceiptSubmitted {
		t.Fatalf("%+v %v", rc, err)
	}
	if _, g := evil.counts(); g != 0 {
		t.Error("a calendar outside the configuration was asked")
	}
}

func TestUpgradeAllFail(t *testing.T) {
	a := newFakeCal(t, false)
	tg := newTarget(t, nil, 1, a.url())
	rc, err := tg.Submit(context.Background(), testPayload(t))
	if err != nil {
		t.Fatal(err)
	}
	a.set("503")
	out, err := tg.Upgrade(context.Background(), rc)
	if !anchor.IsRetryable(err) || !bytes.Equal(out.Evidence, rc.Evidence) || out.State != anchor.ReceiptSubmitted {
		t.Errorf("upgrade with a failing calendar: %v %+v", err, out)
	}
	// a bad stored fragment is permanent
	bad := anchor.Receipt{State: anchor.ReceiptSubmitted, Evidence: json.RawMessage(`{"digest":"` + strings.Repeat("00", 32) + `","calendars":[{"uri":"x","fragment_b64":"AAAA"}],"bitcoin":null}`)}
	if _, err := tg.Upgrade(context.Background(), bad); !anchor.IsPermanent(err) {
		t.Errorf("bad fragment: %v", err)
	}
	if _, err := tg.Upgrade(context.Background(), anchor.Receipt{State: anchor.ReceiptSubmitted, Evidence: json.RawMessage(`{`)}); !anchor.IsPermanent(err) {
		t.Errorf("bad evidence: %v", err)
	}
}

func TestTestMethod(t *testing.T) {
	a, b := newFakeCal(t, false), newFakeCal(t, false)
	b.set("500")
	tg := newTarget(t, nil, 2, a.url(), b.url())
	res := tg.Test(context.Background())
	if res.OK() {
		t.Error("Test passed with one calendar down and two needed")
	}
	if len(res.Checks) != 3 || res.Checks[0].Status != anchor.CheckOK || res.Checks[1].Status != anchor.CheckFailed || res.Checks[2].Name != "minimum" {
		t.Errorf("%+v", res.Checks)
	}
	// the random digests are 32 bytes from the supplied reader
	if len(a.lastBody) != 32 || len(b.lastBody) != 32 || bytes.Equal(a.lastBody, b.lastBody) {
		t.Errorf("bodies %x %x", a.lastBody, b.lastBody)
	}
	b.set("ok")
	if res := tg.Test(context.Background()); !res.OK() {
		t.Errorf("%+v", res.Checks)
	}
}

func TestConfig(t *testing.T) {
	ok := func(js string) Config {
		t.Helper()
		c, err := ParseConfig(json.RawMessage(js))
		if err != nil {
			t.Fatalf("%s: %v", js, err)
		}
		return c
	}
	c := ok(`{"calendars":["https://A.example/","https://b.example"]}`)
	if c.MinCalendars != 2 || c.UpgradeTimeoutDays != 7 || c.Calendars[0] != "https://a.example" {
		t.Errorf("%+v", c)
	}
	if c := ok(`{"calendars":["https://a.example"]}`); c.MinCalendars != 1 {
		t.Errorf("single calendar default min %d", c.MinCalendars)
	}
	if c := ok(`{"calendars":["http://a.example:8080/cal/"],"allowInsecure":true}`); c.Calendars[0] != "http://a.example:8080/cal" {
		t.Errorf("%+v", c)
	}
	bad := []string{
		`{}`, `{"calendars":[]}`, `{"calendars":["http://a.example"]}`, `{"calendars":["ftp://a.example"]}`,
		`{"calendars":["https://u:p@a.example"]}`, `{"calendars":["https://a.example?x=1"]}`, `{"calendars":["https://a.example#f"]}`,
		`{"calendars":["https://a.example","https://a.example/"]}`, `{"calendars":["https://a.example"],"minCalendars":2}`,
		`{"calendars":["https://a.example"],"minCalendars":-1}`, `{"calendars":["https://a.example"],"upgradeTimeoutDays":366}`,
		`{"calendars":["https://a.example"],"upgradeTimeoutDays":-1}`, `{"calendars":["https://a.example"],"x":1}`,
		`{"calendars":["https://a.example"]} {}`, `[]`, `{"calendars":["https://a.example/a b"]}`, `{"calendars":["https://"]}`,
		`{"calendars":["https://a.example/%41"]}`, `{"calendars":["` + strings.Repeat("a", 1001) + `"]}`,
	}
	for _, js := range bad {
		if _, err := ParseConfig(json.RawMessage(js)); err == nil {
			t.Errorf("accepted %s", js)
		}
	}
	many := `{"calendars":[`
	for i := 0; i <= MaxCalendars; i++ {
		if i > 0 {
			many += ","
		}
		many += fmt.Sprintf(`"https://c%d.example"`, i)
	}
	if _, err := ParseConfig(json.RawMessage(many + `]}`)); err == nil {
		t.Error("too many calendars accepted")
	}
	tg := newTarget(t, nil, 1, "http://127.0.0.1:1")
	if tg.Kind() != anchor.KindOTS || tg.Validate(json.RawMessage(`{"calendars":["https://a.example"]}`)) != nil || tg.Validate(json.RawMessage(`{"calendars":[]}`)) == nil {
		t.Error("Kind or Validate")
	}
}

func TestExpired(t *testing.T) {
	c := testNow
	if Expired(c, c.Add(7*24*time.Hour-time.Second), 0) {
		t.Error("expired one second early")
	}
	if !Expired(c, c.Add(7*24*time.Hour), 0) {
		t.Error("not expired at the limit")
	}
	if Expired(c, c.Add(2*24*time.Hour), 3) || !Expired(c, c.Add(3*24*time.Hour), 3) {
		t.Error("custom timeout")
	}
}

func TestCalendarURLNormalize(t *testing.T) {
	if _, err := NormalizeCalendarURL("https://x.example", false); err != nil {
		t.Error(err)
	}
	if got, _ := NormalizeCalendarURL("https://X.Example:443///", false); got != "https://x.example:443" {
		t.Errorf("%q", got)
	}
}

// With OTS_EXPORT_DIR set the test writes the exported files there, for a
// check with the reference client ("ots info") by hand; without it, nothing is written.
func TestExportForReferenceClient(t *testing.T) {
	dir := os.Getenv("OTS_EXPORT_DIR")
	if dir == "" {
		t.Skip("OTS_EXPORT_DIR not set")
	}
	a, b := newFakeCal(t, false), newFakeCal(t, false)
	b.mu.Lock()
	b.bitcoin = true
	b.mu.Unlock()
	tg := newTarget(t, nil, 2, a.url(), b.url())
	rc, err := tg.Submit(context.Background(), testPayload(t))
	if err != nil {
		t.Fatal(err)
	}
	pend, _ := anchor.ParseOTSEvidence(rc.Evidence)
	rc, err = tg.Upgrade(context.Background(), rc)
	if err != nil {
		t.Fatal(err)
	}
	for name, ev := range map[string]json.RawMessage{"pending.ots": mustEncode(t, pend), "upgraded.ots": rc.Evidence} {
		parsed, _ := anchor.ParseOTSEvidence(ev)
		b, err := Export(parsed)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func mustEncode(t *testing.T, ev anchor.OTSEvidence) json.RawMessage {
	t.Helper()
	b, err := anchor.EncodeEvidence(ev)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
