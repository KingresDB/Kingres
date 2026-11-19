// SPDX-License-Identifier: AGPL-3.0-only

package ledgerpub_test

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ledgerlog"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ledgerpub"
)

// publication builds a store with a chain log of 300 entries (a full tile, a partial tile and bundles) and its checkpoint, an event and a
// key ring, and a Handler over it.
func publication(t *testing.T) (*ledgerpub.Handler, ledgerpub.LogRow, *ledgerpub.MemStore) {
	t.Helper()
	f := memFixture(t)
	m := f.store.(*ledgerpub.MemStore)
	var id [32]byte
	id[0], id[31] = 0xab, 0xcd
	origin, _ := ledgerlog.ChainOrigin("ledger.test/kingres", id, 3)
	g := int32(0)
	l, err := m.CreateLog(bg, f.term, ledgerpub.LogRow{Kind: ledgerpub.KindChain, Database: "shop", ShardGroupID: &g, ChainID: id[:], Epoch: 3, Origin: origin})
	if err != nil {
		t.Fatal(err)
	}
	size, root := grow(t, f, l, 0, 300)
	if err := m.PutCheckpoint(bg, f.term, ledgerpub.Checkpoint{LogID: l.ID, Size: size, Root: root, Note: []byte("origin\n300\nroot\n\n- sig\n"), SignedAt: time.Now()}, f.rev(), false, ""); err != nil {
		t.Fatal(err)
	}
	f.putEvent(0, []byte("kingres-ledger-event/1\nseq: 0\n"))
	f.putKeyring([]byte(`{"format":1}`))
	h := &ledgerpub.Handler{Objects: func(ctx context.Context, p ledgerpub.LogPath) (ledgerpub.Object, error) {
		if p.Log == "" {
			return m.Object(ctx, "", p.Rest)
		}
		return m.Object(ctx, "ledger.test/kingres/"+p.Log, p.Rest)
	}}
	return h, l, m
}

const logPrefix = "/c/ab000000000000000000000000000000000000000000000000000000000000cd/e3"

func get(t *testing.T, h http.Handler, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, nil)
	r.RemoteAddr = "192.0.2.1:5000"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestTheListenerServesTheFilesOfAPublicationWithTheRightHeaders(t *testing.T) {
	h, _, _ := publication(t)
	cases := []struct {
		path  string
		cache string
		ct    string
		size  int
	}{
		{logPrefix + "/checkpoint", ledgerpub.CacheMutable, "text/plain; charset=utf-8", len("origin\n300\nroot\n\n- sig\n")},
		{logPrefix + "/tile/0/000", ledgerpub.CacheImmutable, "application/octet-stream", 8192},
		{logPrefix + "/tile/0/001.p/44", ledgerpub.CacheImmutable, "application/octet-stream", 44 * 32},
		{logPrefix + "/tile/1/000.p/1", ledgerpub.CacheImmutable, "application/octet-stream", 32},
		{logPrefix + "/tile/entries/000", ledgerpub.CacheImmutable, "application/octet-stream", 256 * 89},
		{logPrefix + "/tile/entries/001.p/44", ledgerpub.CacheImmutable, "application/octet-stream", 44 * 89},
		{"/keyring.json", ledgerpub.CacheMutable, "application/json", len(`{"format":1}`)},
		{"/events/000000", ledgerpub.CacheImmutable, "text/plain; charset=utf-8", len("kingres-ledger-event/1\nseq: 0\n")},
	}
	for _, c := range cases {
		w := get(t, h, "GET", c.path)
		if w.Code != 200 {
			t.Errorf("%s: %d %s", c.path, w.Code, w.Body.String())
			continue
		}
		if got := w.Header().Get("Cache-Control"); got != c.cache {
			t.Errorf("%s: Cache-Control %q, want %q", c.path, got, c.cache)
		}
		if got := w.Header().Get("Content-Type"); got != c.ct {
			t.Errorf("%s: Content-Type %q, want %q", c.path, got, c.ct)
		}
		if w.Body.Len() != c.size || w.Header().Get("Content-Length") == "" {
			t.Errorf("%s: %d bytes, Content-Length %q, want %d", c.path, w.Body.Len(), w.Header().Get("Content-Length"), c.size)
		}
		if w.Header().Get("Set-Cookie") != "" || w.Header().Get("Location") != "" || w.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s: headers %v", c.path, w.Header())
		}
		// HEAD: the same headers and no body
		hw := get(t, h, "HEAD", c.path)
		if hw.Code != 200 || hw.Body.Len() != 0 || hw.Header().Get("Cache-Control") != c.cache || hw.Header().Get("Content-Length") != w.Header().Get("Content-Length") {
			t.Errorf("HEAD %s: %d, %d body bytes, %v", c.path, hw.Code, hw.Body.Len(), hw.Header())
		}
	}
	// a tile that was asked for as a partial and exists only as a full one is not invented: a miss
	if w := get(t, h, "GET", logPrefix+"/tile/0/000.p/200"); w.Code != 404 {
		t.Errorf("a partial tile that was never made: %d", w.Code)
	}
}

func TestTheListenerAnswersOnlyTheLayoutAndNeverRedirects(t *testing.T) {
	h, _, _ := publication(t)
	h.RatePerSecond = 100000 // this test asks many times from one address
	notFound := []string{
		"/", "", "//", "/c", "/c/", "/a", "/events", "/events/", "/events/0", "/events/0000000", "/events/00000a", "/events/000001", "/events/-00001", "/events/+00001",
		"/keyring.json/", "/keyring.json/x", "/keyring",
		logPrefix, logPrefix + "/", logPrefix + "/tile", logPrefix + "/tile/", logPrefix + "/tile/0", logPrefix + "/tile/0/", logPrefix + "/tile/0/0", logPrefix + "/tile/0/0000",
		logPrefix + "/tile/00/000", logPrefix + "/tile/64/000", logPrefix + "/tile/0/000.p/0", logPrefix + "/tile/0/000.p/256", logPrefix + "/tile/0/000.p/01", logPrefix + "/tile/0/000/",
		logPrefix + "/tile/entries", logPrefix + "/checkpoint/", logPrefix + "/checkpoint/x", logPrefix + "/../checkpoint", logPrefix + "/./checkpoint",
		"/c/ab000000000000000000000000000000000000000000000000000000000000cd/e03/checkpoint",
		"/c/ab000000000000000000000000000000000000000000000000000000000000cd/e0/checkpoint",
		"/c/AB000000000000000000000000000000000000000000000000000000000000CD/e3/checkpoint",
		"/c/ab/e3/checkpoint", "/c//e3/checkpoint", "/c/ab000000000000000000000000000000000000000000000000000000000000cd/e3x/checkpoint",
		"/c/ab000000000000000000000000000000000000000000000000000000000000cd/e4/checkpoint",
		"/a/ab000000000000000000000000000000000000000000000000000000000000cd/checkpoint",
		"/a/ab000000000000000000000000000000000000000000000000000000000000cd",
		"//c/ab000000000000000000000000000000000000000000000000000000000000cd/e3/checkpoint",
		logPrefix + "//checkpoint", logPrefix + "/tile//0/000", logPrefix + "/tile/0/000%2f", logPrefix + "/%63heckpoint", logPrefix + "/checkpoint%00", logPrefix + "\\checkpoint",
		"/..", "/../keyring.json", "/c/../keyring.json", "/etc/passwd", "/%2e%2e/keyring.json",
	}
	for _, p := range notFound {
		r := httptest.NewRequest("GET", "http://x/", nil)
		r.URL.Path = p
		r.URL.RawPath = ""
		if strings.Contains(p, "%") {
			r.URL.Path = strings.NewReplacer("%2f", "/", "%63", "c", "%00", "\x00", "%2e", ".").Replace(p)
			r.URL.RawPath = p
		}
		r.RemoteAddr = "192.0.2.1:5000"
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 404 {
			t.Errorf("%q: %d, want 404", p, w.Code)
		}
		if w.Header().Get("Location") != "" {
			t.Errorf("%q: redirect to %q", p, w.Header().Get("Location"))
		}
		if cc := w.Header().Get("Cache-Control"); w.Code == 404 && (cc != "public, max-age=5" || strings.Contains(cc, "immutable")) {
			t.Errorf("%q: a miss must not be kept: %q", p, w.Header().Get("Cache-Control"))
		}
	}
	for _, m := range []string{"POST", "PUT", "DELETE", "PATCH", "OPTIONS"} {
		w := get(t, h, m, logPrefix+"/checkpoint")
		if w.Code != 405 || w.Header().Get("Allow") != "GET, HEAD" {
			t.Errorf("%s: %d %q", m, w.Code, w.Header().Get("Allow"))
		}
	}
	// the query string and a fragment-less path with a trailing question mark do not change what is served
	if w := get(t, h, "GET", logPrefix+"/checkpoint?x=1"); w.Code != 200 {
		t.Errorf("a query string: %d", w.Code)
	}
}

// A real listener over TCP: no redirect is followed, a path with dots is not cleaned into a file, and a directory is never listed.
func TestAListenerOverTCPServesAndTheClientSeesNoRedirect(t *testing.T) {
	h, _, _ := publication(t)
	l := &ledgerpub.Listener{Addr: "127.0.0.1:0", Handler: h}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := l.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer l.Stop()
	if a, tlsOn, running := l.Status(); a != "127.0.0.1:0" || tlsOn || !running {
		t.Errorf("status %q %v %v", a, tlsOn, running)
	}
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	base := "http://" + l.BoundAddr()
	for path, want := range map[string]int{
		logPrefix + "/checkpoint":       200,
		logPrefix + "/tile/0/000":       200,
		"/c/ab/../keyring.json":         404,
		"/keyring.json/../keyring.json": 404,
		"//keyring.json":                404,
		"/events/":                      404,
		"/c/":                           404,
	} {
		resp, err := c.Get(base + path)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != want || resp.Header.Get("Location") != "" {
			t.Errorf("%s: %d (Location %q), want %d", path, resp.StatusCode, resp.Header.Get("Location"), want)
		}
	}
}

func TestTheListenerLimitsConcurrencyAndRate(t *testing.T) {
	h, _, _ := publication(t)
	block := make(chan struct{})
	entered := make(chan struct{}, 8)
	inner := h.Objects
	h.Objects = func(ctx context.Context, p ledgerpub.LogPath) (ledgerpub.Object, error) {
		entered <- struct{}{}
		<-block
		return inner(ctx, p)
	}
	h.MaxConcurrent = 2
	var wg sync.WaitGroup
	codes := make(chan int, 3)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes <- get(t, h, "GET", logPrefix+"/checkpoint").Code
		}()
	}
	<-entered
	<-entered
	if w := get(t, h, "GET", logPrefix+"/checkpoint"); w.Code != 503 || w.Header().Get("Retry-After") == "" {
		t.Errorf("the third request: %d", w.Code)
	}
	close(block)
	wg.Wait()
	if a, b := <-codes, <-codes; a != 200 || b != 200 {
		t.Errorf("%d %d", a, b)
	}

	// the rate of one address
	h2, _, _ := publication(t)
	now := time.Unix(1000, 0)
	h2.Now = func() time.Time { return now }
	h2.RatePerSecond = 5
	var ok, limited int
	for i := 0; i < 12; i++ {
		switch get(t, h2, "GET", logPrefix+"/checkpoint").Code {
		case 200:
			ok++
		case 429:
			limited++
		}
	}
	if ok != 5 || limited != 7 {
		t.Errorf("ok %d, limited %d", ok, limited)
	}
	// another address has its own budget, and time gives the first one its budget back
	r := httptest.NewRequest("GET", logPrefix+"/checkpoint", nil)
	r.RemoteAddr = "192.0.2.77:1"
	w := httptest.NewRecorder()
	h2.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Errorf("another address: %d", w.Code)
	}
	now = now.Add(time.Second)
	if get(t, h2, "GET", logPrefix+"/checkpoint").Code != 200 {
		t.Error("the budget did not come back")
	}
}

func TestAStoreFailureIsA503AndNeverShowsTheError(t *testing.T) {
	h := &ledgerpub.Handler{Objects: func(context.Context, ledgerpub.LogPath) (ledgerpub.Object, error) {
		return ledgerpub.Object{}, bytes.ErrTooLarge
	}}
	w := get(t, h, "GET", "/keyring.json")
	if w.Code != 503 || strings.Contains(w.Body.String(), "large") || w.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("%d %q %q", w.Code, w.Body.String(), w.Header().Get("Cache-Control"))
	}
}

func selfSigned(t *testing.T, dir string) (certFile, keyFile string, pool *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "ledger.test"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, DNSNames: []string{"ledger.test"}}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	kb, _ := x509.MarshalECPrivateKey(key)
	certFile, keyFile = filepath.Join(dir, "c.pem"), filepath.Join(dir, "k.pem")
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}), 0o600); err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	pool = x509.NewCertPool()
	pool.AddCert(cert)
	return
}

func TestAListenerWithTLSServesOverTLSOnlyAndRefusesOldVersions(t *testing.T) {
	h, _, _ := publication(t)
	cf, kf, pool := selfSigned(t, t.TempDir())
	l := &ledgerpub.Listener{Addr: "127.0.0.1:0", TLSCert: cf, TLSKey: kf, Handler: h}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := l.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer l.Stop()
	if _, tlsOn, running := l.Status(); !tlsOn || !running {
		t.Fatalf("status tls %v running %v", tlsOn, running)
	}
	c := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, ServerName: "ledger.test"}}}
	resp, err := c.Get("https://" + l.BoundAddr() + logPrefix + "/checkpoint")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || resp.TLS == nil || !bytes.HasPrefix(body, []byte("origin\n300\n")) {
		t.Errorf("%d %v %q", resp.StatusCode, resp.TLS != nil, body)
	}
	// plain HTTP to the TLS port does not get a file
	if resp, err := http.Get("http://" + l.BoundAddr() + logPrefix + "/checkpoint"); err == nil {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode == 200 && bytes.Contains(b, []byte("origin")) {
			t.Error("a file was served in clear text on the TLS port")
		}
	}
	// TLS 1.1 is refused
	conn, err := tls.Dial("tcp", l.BoundAddr(), &tls.Config{RootCAs: pool, ServerName: "ledger.test", MinVersion: tls.VersionTLS10, MaxVersion: tls.VersionTLS11})
	if err == nil {
		conn.Close()
		t.Error("TLS 1.1 was accepted")
	}
	// a certificate that is not there stops the start, with an error and nothing running
	bad := &ledgerpub.Listener{Addr: "127.0.0.1:0", TLSCert: filepath.Join(t.TempDir(), "none.pem"), TLSKey: kf, Handler: h}
	if err := bad.Start(ctx); err == nil {
		t.Error("a listener started without its certificate")
		bad.Stop()
	}
	if _, _, running := bad.Status(); running {
		t.Error("running")
	}
	// a nil listener reports nothing
	var none *ledgerpub.Listener
	if a, tl, r := none.Status(); a != "" || tl || r {
		t.Error("a nil listener")
	}
	_ = hex.EncodeToString
}

func TestPathParsing(t *testing.T) {
	good := map[string]ledgerpub.LogPath{
		"keyring.json":  {Rest: "keyring.json"},
		"events/000012": {Rest: "events/000012"},
		"c/" + strings.Repeat("0a", 32) + "/e12/checkpoint":       {Log: "c/" + strings.Repeat("0a", 32) + "/e12", Rest: "checkpoint"},
		"a/" + strings.Repeat("0a", 32) + "/tile/0/x001/x234/067": {Log: "a/" + strings.Repeat("0a", 32), Rest: "tile/0/x001/x234/067"},
	}
	for in, want := range good {
		got, err := ledgerpub.ParsePath(in)
		if err != nil || got != want {
			t.Errorf("%q: %+v %v", in, got, err)
		}
	}
	for _, in := range []string{"", "/", "c", "events/1234567", "events/12345", "events/00001x", "x/y", "c/zz/e1/checkpoint", "a/" + strings.Repeat("0A", 32) + "/checkpoint"} {
		if _, err := ledgerpub.ParsePath(in); err == nil {
			t.Errorf("%q was accepted", in)
		}
	}
	for _, p := range []string{"checkpoint", "tile/0/000", "tile/entries/001.p/3", "tile/3/x001/x234/067"} {
		if _, err := ledgerpub.ParseRest(p); err != nil {
			t.Errorf("%q: %v", p, err)
		}
	}
	for _, p := range []string{"", "tile", "tile/0", "checkpoint/", "Checkpoint", "tile/0/000.p/256", "tile/0/001/000"} {
		if _, err := ledgerpub.ParseRest(p); err == nil {
			t.Errorf("%q was accepted", p)
		}
	}
}

// P8-05: 4096 addresses that asked one after the other fill the table; a new auditor is still served (the oldest entry makes room), and a
// flood from one IPv6 /64 is one client.
func TestAFullRateTableServesTheNewAuditorAndAnIPv6NetworkIsOneClient(t *testing.T) {
	h, _, _ := publication(t)
	now := time.Unix(5000, 0)
	h.Now = func() time.Time { return now }
	from := func(addr string) int {
		r := httptest.NewRequest("GET", logPrefix+"/checkpoint", nil)
		r.RemoteAddr = addr
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	for i := 0; i < 5000; i++ {
		now = now.Add(time.Millisecond) // all within 10 seconds: nothing is stale
		if c := from(fmt.Sprintf("10.%d.%d.%d:1", i>>16&255, i>>8&255, i&255)); c != 200 {
			t.Fatalf("address %d: %d", i, c)
		}
	}
	if c := from("203.0.113.9:1"); c != 200 {
		t.Fatalf("a new auditor with a full table: %d", c)
	}
	// one /64 is one client: its budget is shared by every address of it
	h.RatePerSecond = 3
	got429 := false
	for i := 0; i < 20; i++ {
		if from(fmt.Sprintf("[2001:db8:1:2:%x::1]:1", i+1)) == 429 {
			got429 = true
		}
	}
	if !got429 {
		t.Error("twenty addresses of one /64 were not limited")
	}
	if c := from("[2001:db8:9:9::1]:1"); c != 200 {
		t.Errorf("another network: %d", c)
	}
}

// P8-09: every answer says nosniff and a public CORS header (the mutants that the review found alive).
func TestEveryAnswerSaysNosniffAndTheCORSHeader(t *testing.T) {
	h, _, _ := publication(t)
	for _, p := range []string{logPrefix + "/checkpoint", logPrefix + "/nothing", "/keyring.json"} {
		w := get(t, h, "GET", p)
		if w.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s: no nosniff (status %d)", p, w.Code)
		}
	}
	if w := get(t, h, "GET", logPrefix+"/checkpoint"); w.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Errorf("no CORS header: %v", w.Header())
	}
}

// logs.json lists the logs of a publication in a defined format (correction 178), is served like the key ring, and has no other spelling.
func TestLogsJSONListsTheLogsInTheDefinedFormat(t *testing.T) {
	h, l, _ := publication(t)
	w := get(t, h, "GET", "/logs.json")
	if w.Code != 200 || w.Header().Get("Content-Type") != "application/json" || w.Header().Get("Cache-Control") != ledgerpub.CacheMutable {
		t.Fatalf("%d %v", w.Code, w.Header())
	}
	want := `{"format":1,"logs":[{"kind":"chain","origin":"` + l.Origin + `","state":"active","size":300}]}` + "\n"
	if w.Body.String() != want {
		t.Errorf("logs.json is %q, want %q", w.Body.String(), want)
	}
	for _, p := range []string{"/logs.json/", "/logs.json/x", "/logs", "/LOGS.json"} {
		if c := get(t, h, "GET", p).Code; c != 404 && c != 400 {
			t.Errorf("%s: %d", p, c)
		}
	}
}

// The concurrency limit by a real assertion (P8-09): with a limit of 2 and ten requests that all wait inside the store, at most two are ever inside
// at once, and the others are answered 503 with Retry-After at once, not after a timeout.
func TestTheConcurrencyLimitIsNeverExceeded(t *testing.T) {
	var inside, peak atomic.Int64
	release := make(chan struct{})
	h := &ledgerpub.Handler{MaxConcurrent: 2, RatePerSecond: 100000, Objects: func(ctx context.Context, p ledgerpub.LogPath) (ledgerpub.Object, error) {
		n := inside.Add(1)
		for {
			old := peak.Load()
			if n <= old || peak.CompareAndSwap(old, n) {
				break
			}
		}
		defer inside.Add(-1)
		select {
		case <-release:
		case <-ctx.Done():
		}
		return ledgerpub.Object{Data: []byte("{}"), Mutable: true, ContentType: "application/json"}, nil
	}}
	var wg sync.WaitGroup
	codes := make(chan int, 10)
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := get(t, h, "GET", "/keyring.json")
			if w.Code == 503 && w.Header().Get("Retry-After") == "" {
				t.Error("a 503 without Retry-After")
			}
			codes <- w.Code
		}()
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(codes) < 8 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if len(codes) != 8 {
		t.Fatalf("%d requests were answered while two were held: the others were not refused at once", len(codes))
	}
	close(release)
	wg.Wait()
	close(codes)
	ok, busy := 0, 0
	for c := range codes {
		switch c {
		case 200:
			ok++
		case 503:
			busy++
		}
	}
	if ok != 2 || busy != 8 || peak.Load() != 2 {
		t.Errorf("ok %d busy %d peak inside %d", ok, busy, peak.Load())
	}
}
