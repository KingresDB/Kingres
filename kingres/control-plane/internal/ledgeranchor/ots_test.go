// SPDX-License-Identifier: AGPL-3.0-only

package ledgeranchor

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/anchor"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/anchor/ots"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
)

// otsCal is a fake calendar: it answers a digest with a fragment that ends in a pending attestation, and a later request for the commitment with a
// Bitcoin attestation once told to.
type otsCal struct {
	srv     *httptest.Server
	mu      sync.Mutex
	mode    string // ok, down, huge, hang
	upgrade bool
	height  uint64
	other   bool // answer the commitment with another message
	posts   int
}

func newOTSCal(t *testing.T) *otsCal {
	c := &otsCal{mode: "ok", height: 950000}
	c.srv = httptest.NewServer(http.HandlerFunc(c.handle))
	t.Cleanup(c.srv.Close)
	return c
}

func (c *otsCal) set(f func(c *otsCal)) { c.mu.Lock(); f(c); c.mu.Unlock() }

func (c *otsCal) handle(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	mode, up, height, other := c.mode, c.upgrade, c.height, c.other
	if r.Method == http.MethodPost {
		c.posts++
	}
	c.mu.Unlock()
	switch mode {
	case "down":
		w.WriteHeader(503)
		return
	case "huge":
		_, _ = w.Write(make([]byte, ots.MaxCalendarResponse+1))
		return
	case "hang":
		select {
		case <-r.Context().Done():
		case <-time.After(20 * time.Second):
		}
		return
	}
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/digest":
		d, _ := io.ReadAll(io.LimitReader(r.Body, 1000))
		n := ots.NewNode(d)
		nonce := sha256.Sum256(append([]byte(c.srv.URL), d...))
		x, _ := n.Child(ots.Op{Tag: ots.TagAppend, Arg: nonce[:8]})
		x, _ = x.Child(ots.Op{Tag: ots.TagSHA256})
		p, _ := ots.NewPending(c.srv.URL)
		x.AddAttestation(p)
		b, _ := n.Serialize()
		_, _ = w.Write(b)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/timestamp/"):
		cm, err := hex.DecodeString(strings.TrimPrefix(r.URL.Path, "/timestamp/"))
		if err != nil || !up {
			w.WriteHeader(404)
			_, _ = io.WriteString(w, "Pending confirmation in Bitcoin blockchain")
			return
		}
		if other {
			cm = append([]byte("another commitment"), cm...)
		}
		n := ots.NewNode(cm)
		x, _ := n.Child(ots.Op{Tag: ots.TagAppend, Arg: []byte("sibling-from-bitcoin-merkle-path")})
		x, _ = x.Child(ots.Op{Tag: ots.TagSHA256})
		x, _ = x.Child(ots.Op{Tag: ots.TagSHA256})
		a, _ := ots.NewBitcoin(height)
		x.AddAttestation(a)
		b, _ := n.Serialize()
		_, _ = w.Write(b)
	default:
		w.WriteHeader(404)
	}
}

func (e *env) addOTS(name string, extra map[string]any, cals ...*otsCal) api.LedgerAnchorTarget {
	e.t.Helper()
	var urls []string
	for _, c := range cals {
		urls = append(urls, c.srv.URL)
	}
	cfg := map[string]any{"calendars": urls, "allowInsecure": true, "upgradeTimeoutDays": 7}
	for k, v := range extra {
		cfg[k] = v
	}
	tg, err := e.svc.CreateTarget(e.ctx(), "admin", api.LedgerAnchorTargetCreate{Kind: "ots", Name: name, Enabled: true, Config: mustRaw(cfg)})
	if err != nil {
		e.t.Fatalf("creating the ots target: %v", err)
	}
	return tg
}

func (e *env) otsRecord(extra map[string]any, cals ...*otsCal) int64 {
	e.t.Helper()
	e.addOTS("stamp", extra, cals...)
	e.putAgg("shop", 3)
	if err := e.runEnqueue(); err != nil {
		e.t.Fatal(err)
	}
	return e.rows()[0].ID
}

// A submitted record waits for Bitcoin; a calendar that never upgrades makes it expire after upgradeTimeoutDays with the alert, and a Retry
// waits again.
func TestACalendarThatNeverUpgradesEndsInExpiredAndRetryWaitsAgain(t *testing.T) {
	e := newEnv(t)
	a, b := newOTSCal(t), newOTSCal(t)
	id := e.otsRecord(nil, a, b)
	if err := e.runSubmit(); err != nil {
		t.Fatal(err)
	}
	r := e.row(id)
	if r.State != anchor.StateSubmitted || r.NextAttemptAt == nil {
		t.Fatalf("after submit: %+v", r)
	}
	for i := 0; i < 5; i++ {
		e.clock.Advance(anchor.UpgradeInterval(time.Hour) + time.Second)
		if err := e.runUpgrade(); err != nil {
			t.Fatal(err)
		}
	}
	if r = e.row(id); r.State != anchor.StateSubmitted {
		t.Fatalf("still waiting: %+v", r)
	}
	e.clock.Advance(8 * 24 * time.Hour)
	if err := e.runUpgrade(); err != nil {
		t.Fatal(err)
	}
	r = e.row(id)
	if r.State != anchor.StateExpired || r.Alert != AlertFailed {
		t.Fatalf("after the timeout: %+v", r)
	}
	_ = e.runMonitor()
	if e.alerts.severity(AlertFailed, "target:1") != "critical" {
		t.Errorf("alerts: %s", e.alerts.all())
	}
	got, err := e.svc.Retry(e.ctx(), "shop", id)
	if err != nil || got.State != "pending" {
		t.Fatalf("retry: %+v %v", got, err)
	}
	if err := e.runSubmit(); err != nil {
		t.Fatal(err)
	}
	if r = e.row(id); r.State != anchor.StateSubmitted || r.Alert != "" {
		t.Fatalf("after the retry: %+v", r)
	}
}

// A pending attestation that completes later gives a claim, and without a header source a claim is not an anchor: the state is claimed (shown as
// submitted), it counts for nothing, it keeps its time for the next look, and the ots file can be exported.
func TestAClaimOfCalendarsIsNotAnAnchorWithoutAHeaderSource(t *testing.T) {
	e := newEnv(t)
	a, b := newOTSCal(t), newOTSCal(t)
	id := e.otsRecord(nil, a, b)
	_ = e.runSubmit()
	for _, c := range []*otsCal{a} { // one calendar claims: two calendars cannot share a merkle root in a fake, and the library refuses two roots at one height
		c.set(func(c *otsCal) { c.upgrade, c.height = true, plausibleHeight(e) })
	}
	e.clock.Advance(15 * time.Minute)
	if err := e.runUpgrade(); err != nil {
		t.Fatal(err)
	}
	r := e.row(id)
	if r.State != anchor.StateClaimed || r.NextAttemptAt == nil || r.AnchoredAt != nil {
		t.Fatalf("after the claim: %+v", r)
	}
	pg, _ := e.svc.List(e.ctx(), "shop", 0, "", pageOf(5))
	if pg.Items[0].State != "submitted" {
		t.Errorf("the API shows %q", pg.Items[0].State)
	}
	if got, _ := e.svc.Target(e.ctx(), 1); got.State == "ok" || got.NewestAnchoredAt != nil {
		t.Errorf("a claim counted as an anchor: %+v", got)
	}
	// a claimed record with no time for its next look is looked at again (duty 13)
	e.rewriteNext(id, nil)
	e.clock.Advance(time.Minute)
	_ = e.runUpgrade()
	if r = e.row(id); r.NextAttemptAt == nil || r.State != anchor.StateClaimed {
		t.Errorf("a claimed record without a next look: %+v", r)
	}
	if f, err := e.svc.Evidence(e.ctx(), "shop", id, "ots"); err != nil || len(f.Data) < 40 {
		t.Errorf("ots export: %d bytes %v", len(f.Data), err)
	}
	if _, err := e.svc.Evidence(e.ctx(), "shop", id, "tsr"); err == nil {
		t.Error("a tsr file for an ots record")
	}
}

func (e *env) rewriteNext(id int64, t *time.Time) {
	m := e.st
	m.mu.Lock()
	m.rows[id].NextAttemptAt = t
	m.mu.Unlock()
}

// A header source that is unavailable, hostile or wrong never makes a claim an anchor; it is reached only at its configured host.
func TestAHeaderSourceThatCannotConfirmLeavesTheClaimAClaim(t *testing.T) {
	var hits int
	var mu sync.Mutex
	var hdr [80]byte
	h := sha256.Sum256(hdr[:])
	h = sha256.Sum256(h[:])
	for i, j := 0, 31; i < j; i, j = i+1, j-1 {
		h[i], h[j] = h[j], h[i]
	}
	src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
		switch {
		case strings.HasPrefix(r.URL.Path, "/block-height/"):
			_, _ = io.WriteString(w, hex.EncodeToString(h[:]))
		case strings.HasSuffix(r.URL.Path, "/header"):
			_, _ = io.WriteString(w, hex.EncodeToString(hdr[:])) // an 80-byte header that is nobody's: no work, not the merkle root of the claim
		}
	}))
	defer src.Close()
	e := newEnv(t)
	a, b := newOTSCal(t), newOTSCal(t)
	id := e.otsRecord(map[string]any{"headerSource": map[string]any{"url": src.URL}}, a, b)
	_ = e.runSubmit()
	for _, c := range []*otsCal{a} { // one calendar claims: two calendars cannot share a merkle root in a fake, and the library refuses two roots at one height
		c.set(func(c *otsCal) { c.upgrade, c.height = true, plausibleHeight(e) })
	}
	e.clock.Advance(15 * time.Minute)
	_ = e.runUpgrade()
	mu.Lock()
	n := hits
	mu.Unlock()
	if r := e.row(id); r.State == anchor.StateAnchored || n == 0 {
		t.Fatalf("the source was asked %d times and the record is %+v", n, r)
	}
	// the source is a host of the egress rule, and a host that is not configured is not reached
	eg, _ := NewEgress([]string{a.srv.URL, b.srv.URL, src.URL})
	if !eg.Allows(strings.TrimPrefix(src.URL, "http://")) {
		t.Error("the header source host is not allowed")
	}
	if _, err := e.svc.CreateTarget(e.ctx(), "a", api.LedgerAnchorTargetCreate{Kind: "ots", Name: "bad", Config: mustRaw(map[string]any{
		"calendars": []string{a.srv.URL}, "allowInsecure": true, "headerSource": map[string]any{"url": "ftp://x"}})}); err == nil {
		t.Error("a header source that is not http or https was accepted")
	}
}

// One calendar that is down: the record waits with what the other gave, the one that answered is not asked again, and the record is submitted
// when the other comes back (Resume from the kept evidence, duty 12).
func TestAPartialSubmissionKeepsWhatAnsweredAndResumes(t *testing.T) {
	e := newEnv(t)
	a, b := newOTSCal(t), newOTSCal(t)
	b.set(func(c *otsCal) { c.mode = "down" })
	id := e.otsRecord(map[string]any{"minCalendars": 2}, a, b)
	_ = e.runSubmit()
	r := e.row(id)
	if r.State != anchor.StatePending || r.Attempts != 1 || len(r.Evidence) == 0 {
		t.Fatalf("after a partial submission: %+v (evidence kept: %v)", r, len(r.Evidence) > 0)
	}
	b.set(func(c *otsCal) { c.mode = "ok" })
	e.clock.Advance(2 * time.Minute)
	_ = e.runSubmit()
	r = e.row(id)
	if r.State != anchor.StateSubmitted {
		t.Fatalf("after the second attempt: %+v", r)
	}
	a.mu.Lock()
	posts := a.posts
	a.mu.Unlock()
	if posts != 1 {
		t.Errorf("the calendar that had answered was asked %d times", posts)
	}
}

// Calendars that disagree, answer for another commitment, answer too much or never: no anchor, no hang.
func TestHostileCalendarsAnchorNothing(t *testing.T) {
	for name, setup := range map[string]func(a, b *otsCal){
		"a commitment that is not the one asked": func(a, b *otsCal) {
			a.set(func(c *otsCal) { c.upgrade, c.other = true, true })
			b.set(func(c *otsCal) { c.upgrade = true })
		},
		"two calendars that disagree": func(a, b *otsCal) {
			ph := plausibleHeight(clockEnv)
			a.set(func(c *otsCal) { c.upgrade, c.height = true, ph })
			b.set(func(c *otsCal) { c.upgrade, c.height = true, ph })
		},
		"an oversized answer": func(a, b *otsCal) { a.set(func(c *otsCal) { c.mode = "huge" }) },
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			a, b := newOTSCal(t), newOTSCal(t)
			id := e.otsRecord(nil, a, b)
			_ = e.runSubmit()
			clockEnv = e
			setup(a, b)
			done := make(chan struct{})
			go func() {
				e.clock.Advance(15 * time.Minute)
				_ = e.runUpgrade()
				close(done)
			}()
			select {
			case <-done:
			case <-time.After(30 * time.Second):
				t.Fatal("the job hangs")
			}
			if r := e.row(id); r.State == anchor.StateAnchored {
				t.Fatalf("anchored: %+v", r)
			}
		})
	}
}

func TestAnOtsTargetIsCheckedWhenItIsMade(t *testing.T) {
	e := newEnv(t)
	a := newOTSCal(t)
	for name, cfg := range map[string]map[string]any{
		"no calendar":     {"calendars": []string{}},
		"a repeat":        {"calendars": []string{a.srv.URL, a.srv.URL}, "allowInsecure": true},
		"http by default": {"calendars": []string{a.srv.URL}},
		"min too large":   {"calendars": []string{a.srv.URL}, "allowInsecure": true, "minCalendars": 3},
	} {
		if _, err := e.svc.CreateTarget(e.ctx(), "a", api.LedgerAnchorTargetCreate{Kind: "ots", Name: "x", Config: mustRaw(cfg)}); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := e.svc.CreateTarget(e.ctx(), "a", api.LedgerAnchorTargetCreate{Kind: "ots", Name: "y", Config: mustRaw(map[string]any{"calendars": []string{a.srv.URL}, "allowInsecure": true}), Credentials: wormCreds()}); err == nil {
		t.Error("credentials were accepted for a kind that takes none")
	}
}

// plausibleHeight is a height that the library accepts as possible for a record made now and looked at a quarter of an hour later.
func plausibleHeight(e *env) uint64 {
	lo, hi := ots.HeightBounds(e.clock.Now(), e.clock.Now().Add(15*time.Minute))
	return (lo + hi) / 2
}

var clockEnv *env
