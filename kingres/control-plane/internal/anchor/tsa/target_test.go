// SPDX-License-Identifier: AGPL-3.0-only

package tsa

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/anchor"
)

const (
	testUser = "tsa-user-name"
	testPass = "Zx9-very-secret-password"
)

type env struct {
	t      *testing.T
	a      *authority
	srv    *httptest.Server
	target *Target
	calls  atomic.Int32
	mu     sync.Mutex
	issue  issueOpts
	// handle, when set, replaces the default behaviour.
	handle func(w http.ResponseWriter, r *http.Request, req *tsReq)
	gotReq []*http.Request
	clock  time.Time
}

func newEnv(t *testing.T, mod func(*Options)) *env {
	t.Helper()
	e := &env{t: t, a: newAuthority(t, authOpts{})}
	e.srv = httptest.NewServer(http.HandlerFunc(e.serve))
	t.Cleanup(e.srv.Close)
	o := Options{Config: Config{URL: e.srv.URL + "/tsa", CABundlePath: "unused", TimeoutSeconds: 5, AllowInsecure: true},
		Roots: e.a.rootPool, Intermediates: e.a.interPool, Username: testUser, Password: testPass,
		Client: e.srv.Client()}
	if mod != nil {
		mod(&o)
	}
	var err error
	if e.target, err = New(o); err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *env) serve(w http.ResponseWriter, r *http.Request) {
	e.calls.Add(1)
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<16))
	e.mu.Lock()
	e.gotReq = append(e.gotReq, r.Clone(context.Background()))
	h, iss := e.handle, e.issue
	e.mu.Unlock()
	req := parseTSReq(e.t, body)
	if h != nil {
		h(w, r, req)
		return
	}
	w.Header().Set("Content-Type", "application/timestamp-reply")
	_, _ = w.Write(e.a.response(0, e.a.token(req, iss)))
}

func (e *env) set(f func(w http.ResponseWriter, r *http.Request, req *tsReq)) {
	e.mu.Lock()
	e.handle = f
	e.mu.Unlock()
}

func testPayload(t *testing.T) anchor.Payload {
	t.Helper()
	root := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{9}, 32))
	note := "kingres.example/agg\n17\n" + root + "\n\n" + string(rune(0x2014)) + " kingres.example/agg " + base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 68)) + "\n"
	p, err := anchor.NewPayload("ab12", []byte(note), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestSubmitSuccess(t *testing.T) {
	e := newEnv(t, nil)
	p := testPayload(t)
	rc, err := e.target.Submit(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if rc.State != anchor.ReceiptAnchored || rc.EvidenceTime == nil || rc.NextCheck != nil {
		t.Fatalf("receipt %+v", rc)
	}
	ev, err := anchor.ParseTSAEvidence(rc.Evidence)
	if err != nil {
		t.Fatal(err)
	}
	if ev.TSAURL != e.srv.URL+"/tsa" || ev.Serial != "4242" || ev.Policy != "1.2.3.4.5" || ev.RecordedBy != "cluster" {
		t.Fatalf("%+v", ev)
	}
	if _, err := VerifyEvidence(ev, p.Digest, e.a.rootPool, e.a.interPool); err != nil {
		t.Fatal(err)
	}
	if anchor.FormatTime(*rc.EvidenceTime) != ev.GenTime {
		t.Fatal("evidence time is not genTime")
	}
	// the whole evidence file round trips
	file, err := anchor.EncodeEvidenceFile(anchor.EvidenceFile{Kind: anchor.KindTSA, Origin: p.Origin, Size: p.Size, PayloadSHA256: p.DigestHex(), CreatedAt: time.Now(), Evidence: rc.Evidence})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := anchor.ParseEvidenceFile(file); err != nil {
		t.Fatal(err)
	}
	// headers and body of the request
	r := e.gotReq[0]
	if r.Method != "POST" || r.Header.Get("Content-Type") != "application/timestamp-query" || r.Header.Get("Accept") != "application/timestamp-reply" {
		t.Fatalf("headers %v", r.Header)
	}
	if u, pw, ok := r.BasicAuth(); !ok || u != testUser || pw != testPass {
		t.Fatal("basic auth not sent")
	}
	// Upgrade is the identity
	rc2, err := e.target.Upgrade(context.Background(), rc)
	if err != nil || !bytes.Equal(rc2.Evidence, rc.Evidence) {
		t.Fatal("upgrade changed the receipt")
	}
}

func TestRequestEncoding(t *testing.T) {
	d := testDigest
	der, err := BuildRequest(Request{Digest: d, Nonce: big.NewInt(0x0102030405060708), PolicyOID: "1.2.3"})
	if err != nil {
		t.Fatal(err)
	}
	// written out by hand from RFC 3161: version 1, SHA-256 with NULL, imprint,
	// reqPolicy 1.2.3, nonce, certReq TRUE
	mi := hexDecode("300d06096086480165030402010500" + "0420" + fmt2hex(d[:]))
	full := append(hexDecode("020101"), append(tl(0x30, mi), hexDecode("06022a03"+"02080102030405060708"+"0101ff")...)...)
	want := fmt2hex(tl(0x30, full))
	if fmt2hex(der) != want {
		t.Fatalf("request\n got %x\nwant %s", der, want)
	}
	var r tsReq
	if rest, err := asn1Unmarshal(der, &r); err != nil || len(rest) != 0 || !r.CertReq || r.Nonce.Int64() != 0x0102030405060708 {
		t.Fatalf("%v %v", err, r)
	}
	// high-bit nonce is still a positive INTEGER
	der, _ = BuildRequest(Request{Digest: d, Nonce: new(big.Int).SetUint64(0xf102030405060708)})
	if !bytes.Contains(der, hexDecode("020900f102030405060708")) {
		t.Fatalf("nonce encoding %x", der)
	}
	if _, err := BuildRequest(Request{Digest: d}); err == nil {
		t.Fatal("no nonce accepted")
	}
	if _, err := BuildRequest(Request{Digest: d, Nonce: big.NewInt(1), PolicyOID: "x"}); err == nil {
		t.Fatal("bad policy accepted")
	}
}

func TestNonceRandom(t *testing.T) {
	n1, _ := randNonce(strings.NewReader("\x00\x00\x00\x00\x00\x00\x00\x00"))
	if n1.Int64() != 1 {
		t.Fatal("zero nonce must become 1")
	}
	if _, err := randNonce(strings.NewReader("abc")); err == nil {
		t.Fatal("short read accepted")
	}
}

func TestHTTPErrors(t *testing.T) {
	cases := []struct {
		name   string
		h      func(w http.ResponseWriter, r *http.Request, req *tsReq)
		class  anchor.Class
		after  time.Duration
		status int
	}{
		{"500", func(w http.ResponseWriter, r *http.Request, _ *tsReq) { w.WriteHeader(500) }, anchor.ClassRetryable, 0, 500},
		{"503 with Retry-After", func(w http.ResponseWriter, r *http.Request, _ *tsReq) {
			w.Header().Set("Retry-After", "120")
			w.WriteHeader(503)
		}, anchor.ClassRetryable, 120 * time.Second, 503},
		{"429", func(w http.ResponseWriter, r *http.Request, _ *tsReq) { w.WriteHeader(429) }, anchor.ClassRetryable, 0, 429},
		{"401", func(w http.ResponseWriter, r *http.Request, _ *tsReq) { w.WriteHeader(401) }, anchor.ClassRefused, 0, 401},
		{"403", func(w http.ResponseWriter, r *http.Request, _ *tsReq) { w.WriteHeader(403) }, anchor.ClassRefused, 0, 403},
		{"400", func(w http.ResponseWriter, r *http.Request, _ *tsReq) { w.WriteHeader(400) }, anchor.ClassPermanent, 0, 400},
		{"cut connection", func(w http.ResponseWriter, r *http.Request, _ *tsReq) {
			c, _, _ := w.(http.Hijacker).Hijack()
			_ = c.Close()
		}, anchor.ClassRetryable, 0, 0},
		{"truncated body", func(w http.ResponseWriter, r *http.Request, _ *tsReq) {
			w.Header().Set("Content-Type", "application/timestamp-reply")
			w.Header().Set("Content-Length", "100")
			_, _ = w.Write([]byte("abc"))
		}, anchor.ClassRetryable, 0, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t, nil)
			e.set(c.h)
			_, err := e.target.Submit(context.Background(), testPayload(t))
			if err == nil {
				t.Fatal("accepted")
			}
			if anchor.ClassOf(err) != c.class {
				t.Fatalf("class %v: %v", anchor.ClassOf(err), err)
			}
			if anchor.RetryAfterOf(err) != c.after {
				t.Fatalf("retry after %v", anchor.RetryAfterOf(err))
			}
			var ae *anchor.Error
			if !errors.As(err, &ae) || ae.Status != c.status {
				t.Fatalf("status %+v", ae)
			}
		})
	}
}

func TestResponseStatuses(t *testing.T) {
	e := newEnv(t, nil)
	reply := func(b []byte) func(http.ResponseWriter, *http.Request, *tsReq) {
		return func(w http.ResponseWriter, r *http.Request, _ *tsReq) {
			w.Header().Set("Content-Type", "application/timestamp-reply")
			_, _ = w.Write(b)
		}
	}
	failInfo := func(status int64, bit int) []byte {
		bs := make([]byte, 5)
		bs[0] = 0
		bs[1+bit/8] = 0x80 >> (bit % 8)
		return seq(seq(intI(status), seq(tl(0x0c, []byte("because"))), tl(0x03, bs)))
	}
	tests := []struct {
		name  string
		body  []byte
		class anchor.Class
		code  string
	}{
		{"rejection", e.a.response(2, nil), anchor.ClassRefused, "tsa_rejected"},
		{"rejection badAlg", failInfo(2, 0), anchor.ClassRefused, "tsa_rejected"},
		{"rejection systemFailure", failInfo(2, 25), anchor.ClassRetryable, "tsa_unavailable"},
		{"rejection timeNotAvailable", failInfo(2, 14), anchor.ClassRetryable, "tsa_unavailable"},
		{"waiting", e.a.response(3, nil), anchor.ClassRetryable, "tsa_waiting"},
		{"revocation warning", e.a.response(4, nil), anchor.ClassPermanent, "tsa_revocation"},
		{"revocation notification", e.a.response(5, nil), anchor.ClassPermanent, "tsa_revocation"},
		{"granted without token", e.a.response(0, nil), anchor.ClassPermanent, "no_token"},
		{"grantedWithMods without token", e.a.response(1, nil), anchor.ClassPermanent, "no_token"},
		{"undefined status", e.a.response(9, nil), anchor.ClassPermanent, CodeStructure},
		{"garbage", []byte("not der at all"), anchor.ClassPermanent, CodeBadDER},
		{"indefinite response", reindefinite(e.a.response(2, nil)), anchor.ClassPermanent, CodeBadDER},
		{"empty", nil, anchor.ClassPermanent, CodeBadDER},
	}
	for _, c := range tests {
		t.Run(c.name, func(t *testing.T) {
			e.set(reply(c.body))
			_, err := e.target.Submit(context.Background(), testPayload(t))
			if err == nil || anchor.ClassOf(err) != c.class || anchor.CodeOf(err) != c.code {
				t.Fatalf("got %v (class %v code %q), want %v %q", err, anchor.ClassOf(err), anchor.CodeOf(err), c.class, c.code)
			}
		})
	}
	// grantedWithMods with a token is accepted
	e.set(func(w http.ResponseWriter, r *http.Request, req *tsReq) {
		w.Header().Set("Content-Type", "application/timestamp-response")
		_, _ = w.Write(e.a.response(1, e.a.token(req, issueOpts{})))
	})
	if _, err := e.target.Submit(context.Background(), testPayload(t)); err != nil {
		t.Fatalf("grantedWithMods and the application/timestamp-response type: %v", err)
	}
}

func TestWrongTokensThroughHTTP(t *testing.T) {
	cases := map[string]struct {
		o    issueOpts
		code string
	}{
		"imprint":      {issueOpts{imprint: bytes.Repeat([]byte{1}, 32)}, CodeImprint},
		"nonce":        {issueOpts{nonceOverride: big.NewInt(1)}, CodeNonce},
		"badSignature": {issueOpts{badSigData: true}, CodeSignature},
		"unsigned":     {issueOpts{unsigned: true}, CodeUnsignedAttrs},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, nil)
			e.issue = c.o
			_, err := e.target.Submit(context.Background(), testPayload(t))
			wantCode(t, err, c.code)
		})
	}
	t.Run("policy requested", func(t *testing.T) {
		e := newEnv(t, func(o *Options) { o.Config.PolicyOID = "1.2.3.4.6" })
		_, err := e.target.Submit(context.Background(), testPayload(t))
		wantCode(t, err, CodePolicy)
	})
	t.Run("policy requested and matching", func(t *testing.T) {
		e := newEnv(t, func(o *Options) { o.Config.PolicyOID = "1.2.3.4.5" })
		if _, err := e.target.Submit(context.Background(), testPayload(t)); err != nil {
			t.Fatal(err)
		}
	})
}

func TestContentType(t *testing.T) {
	e := newEnv(t, nil)
	for _, ct := range []string{"", "text/html", "application/octet-stream", "application/timestamp-query"} {
		e.set(func(w http.ResponseWriter, r *http.Request, req *tsReq) {
			if ct != "" {
				w.Header().Set("Content-Type", ct)
			} else {
				w.Header()["Content-Type"] = nil
			}
			_, _ = w.Write(e.a.response(0, e.a.token(req, issueOpts{})))
		})
		_, err := e.target.Submit(context.Background(), testPayload(t))
		if anchor.CodeOf(err) != "content_type" {
			t.Fatalf("%q: %v", ct, err)
		}
	}
}

func TestResponseSizeCap(t *testing.T) {
	e := newEnv(t, nil)
	send := func(n int) func(http.ResponseWriter, *http.Request, *tsReq) {
		return func(w http.ResponseWriter, r *http.Request, _ *tsReq) {
			w.Header().Set("Content-Type", "application/timestamp-reply")
			_, _ = w.Write(bytes.Repeat([]byte{0x30}, n))
		}
	}
	e.set(send(MaxResponseBytes))
	_, err := e.target.Submit(context.Background(), testPayload(t))
	if anchor.CodeOf(err) != CodeBadDER { // read in full, then rejected as DER
		t.Fatalf("at the limit: %v", err)
	}
	e.set(send(MaxResponseBytes + 1))
	_, err = e.target.Submit(context.Background(), testPayload(t))
	if !anchor.IsPermanent(err) || !strings.Contains(err.Error(), "larger than the limit") {
		t.Fatalf("over the limit: %v", err)
	}
}

func TestParseResponseLimit(t *testing.T) {
	_, err := ParseResponse(make([]byte, MaxResponseBytes+1))
	wantCode2(t, err, CodeTooLarge)
}

func wantCode2(t *testing.T, err error, code string) {
	t.Helper()
	if anchor.CodeOf(err) != code {
		t.Fatalf("code %q (%v), want %q", anchor.CodeOf(err), err, code)
	}
}

func TestTimeoutFromContext(t *testing.T) {
	e := newEnv(t, nil)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	e.set(func(w http.ResponseWriter, r *http.Request, _ *tsReq) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := e.target.Submit(ctx, testPayload(t))
	if !anchor.IsRetryable(err) || time.Since(start) > 3*time.Second {
		t.Fatalf("%v after %v", err, time.Since(start))
	}
}

func TestRedirects(t *testing.T) {
	var hits atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer other.Close()
	e := newEnv(t, nil)
	e.set(func(w http.ResponseWriter, r *http.Request, _ *tsReq) {
		http.Redirect(w, r, other.URL+"/steal", http.StatusTemporaryRedirect)
	})
	_, err := e.target.Submit(context.Background(), testPayload(t))
	if err == nil || !anchor.IsPermanent(err) || hits.Load() != 0 {
		t.Fatalf("redirect to another host: err %v hits %d", err, hits.Load())
	}
	// the same host is followed, with the body
	e.set(func(w http.ResponseWriter, r *http.Request, req *tsReq) {
		if r.URL.Path == "/tsa" {
			http.Redirect(w, r, "/tsa2", http.StatusTemporaryRedirect)
			return
		}
		w.Header().Set("Content-Type", "application/timestamp-reply")
		_, _ = w.Write(e.a.response(0, e.a.token(req, issueOpts{})))
	})
	if _, err := e.target.Submit(context.Background(), testPayload(t)); err != nil {
		t.Fatalf("same-host redirect: %v", err)
	}
	// a loop is cut
	e.set(func(w http.ResponseWriter, r *http.Request, _ *tsReq) {
		http.Redirect(w, r, "/tsa", http.StatusTemporaryRedirect)
	})
	if _, err := e.target.Submit(context.Background(), testPayload(t)); err == nil {
		t.Fatal("redirect loop accepted")
	}
}

func TestSkewOf(t *testing.T) {
	if SkewOf(time.Unix(1000, 0), time.Unix(400, 0)) != 10*time.Minute {
		t.Fatal("SkewOf")
	}
}

func TestTestMethod(t *testing.T) {
	e := newEnv(t, nil)
	res := e.target.Test(context.Background())
	if !res.OK() || len(res.Checks) != 3 {
		t.Fatalf("%+v", res)
	}
	for _, c := range res.Checks {
		if c.Status != anchor.CheckOK {
			t.Fatalf("%+v", c)
		}
	}
	if !strings.Contains(res.Checks[1].Text, "Test TSA") {
		t.Fatalf("%+v", res.Checks[1])
	}
	// the request was for the SHA-256 of the empty string
	e.set(func(w http.ResponseWriter, r *http.Request, req *tsReq) {
		if hexs := fmt2hex(req.MI.Hash); hexs != "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855" {
			t.Errorf("digest %s", hexs)
		}
		w.Header().Set("Content-Type", "application/timestamp-reply")
		_, _ = w.Write(e.a.response(0, e.a.token(req, issueOpts{imprint: bytes.Repeat([]byte{1}, 32)})))
	})
	res = e.target.Test(context.Background())
	if res.OK() || res.Checks[0].Status != anchor.CheckFailed {
		t.Fatalf("%+v", res)
	}
	// a clock an hour off: the token is not accepted, the test says so
	e = newEnv(t, func(o *Options) { o.Clock = func() time.Time { return time.Now().Add(time.Hour) } })
	res = e.target.Test(context.Background())
	if res.OK() || res.Checks[0].Status != anchor.CheckFailed || !strings.Contains(res.Checks[0].Text, "clock") {
		t.Fatalf("%+v", res)
	}
}

func TestConfig(t *testing.T) {
	good := `{"url":"https://tsa.example/ts","hashAlgorithm":"sha256","policyOid":"1.2.3","caBundlePath":"/x/ca.pem","timeoutSeconds":20}`
	tg := &Target{}
	if err := tg.Validate(json.RawMessage(good)); err != nil {
		t.Fatal(err)
	}
	if tg.Kind() != "tsa" {
		t.Fatal(tg.Kind())
	}
	for name, cfg := range map[string]string{
		"unknown member":   `{"url":"https://a/","caBundlePath":"x","extra":1}`,
		"no bundle":        `{"url":"https://a/"}`,
		"no url":           `{"caBundlePath":"x"}`,
		"ftp":              `{"url":"ftp://a/","caBundlePath":"x"}`,
		"user info":        `{"url":"https://u:p@a/","caBundlePath":"x"}`,
		"sha1":             `{"url":"https://a/","caBundlePath":"x","hashAlgorithm":"sha1"}`,
		"bad oid":          `{"url":"https://a/","caBundlePath":"x","policyOid":"abc"}`,
		"negative timeout": `{"url":"https://a/","caBundlePath":"x","timeoutSeconds":-1}`,
		"huge timeout":     `{"url":"https://a/","caBundlePath":"x","timeoutSeconds":301}`,
		"trailing":         good + `{}`,
		"not json":         `[`,
		"non-ascii url":    "{\"url\":\"https://a/" + string(rune(0xe9)) + "\",\"caBundlePath\":\"x\"}",
		"space in url":     `{"url":"https://a/ b","caBundlePath":"x"}`,
	} {
		if err := tg.Validate(json.RawMessage(cfg)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if _, err := New(Options{Config: Config{URL: "https://a/", CABundlePath: "x"}}); err == nil {
		t.Fatal("New without roots")
	}
}

func TestParseBundle(t *testing.T) {
	a := newAuthority(t, authOpts{})
	roots, inter, err := ParseBundle(a.bundlePEM())
	if err != nil {
		t.Fatal(err)
	}
	r := testReq()
	if _, err := VerifyToken(a.token(r, issueOpts{noCerts: false}), expectFor(r), roots, inter); err != nil {
		t.Fatal(err)
	}
	for name, b := range map[string][]byte{
		"empty":             nil,
		"only intermediate": []byte(pemOf(a.inter.Raw)),
		"junk":              []byte("hello"),
		"not a cert":        []byte("-----BEGIN PRIVATE KEY-----\nAAAA\n-----END PRIVATE KEY-----\n"),
		"bad cert":          []byte("-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----\n"),
		"trailing junk":     append(a.bundlePEM(), []byte("junk")...),
	} {
		if _, _, err := ParseBundle(b); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	many := bytes.Repeat([]byte(pemOf(a.root.Raw)), MaxBundleCerts)
	if _, _, err := ParseBundle(many); err != nil {
		t.Fatalf("at the limit: %v", err)
	}
	if _, _, err := ParseBundle(append(many, []byte(pemOf(a.root.Raw))...)); err == nil {
		t.Fatal("over the limit accepted")
	}
}

// TestNoSecretInOutput formats every type and every error produced by the
// fake server and looks for the credentials.
func TestNoSecretInOutput(t *testing.T) {
	e := newEnv(t, nil)
	// a server that echoes the credentials in its status text and in headers
	e.set(func(w http.ResponseWriter, r *http.Request, req *tsReq) {
		u, p, _ := r.BasicAuth()
		w.Header().Set("Content-Type", "application/timestamp-reply")
		w.Header().Set("X-Echo", u+":"+p)
		_, _ = w.Write(seq(seq(intI(2), seq(tl(0x0c, []byte("denied for "+u+" password "+p))))))
	})
	_, errReject := e.target.Submit(context.Background(), testPayload(t))
	if errReject == nil {
		t.Fatal("expected error")
	}
	e.set(func(w http.ResponseWriter, r *http.Request, _ *tsReq) {
		u, p, _ := r.BasicAuth()
		http.Error(w, u+" "+p, 401)
	})
	_, err401 := e.target.Submit(context.Background(), testPayload(t))
	e.set(func(w http.ResponseWriter, r *http.Request, _ *tsReq) {
		c, _, _ := w.(http.Hijacker).Hijack()
		_ = c.Close()
	})
	_, errCut := e.target.Submit(context.Background(), testPayload(t))
	e.set(nil)
	rc, errOK := e.target.Submit(context.Background(), testPayload(t))
	if errOK != nil {
		t.Fatal(errOK)
	}
	res := e.target.Test(context.Background())
	info, _ := VerifyToken(mustTokenFor(e), Expect{Digest: testDigest}, e.a.rootPool, e.a.interPool)
	cfg := Config{URL: e.srv.URL, CABundlePath: "p"}
	opts := Options{Config: cfg, Username: testUser, Password: testPass}
	objs := []any{e.target, *e.target, opts, &opts, cfg, rc, res, info, errReject, err401, errCut, anchor.Secret(testPass),
		&Response{StatusText: "x"}, Request{Nonce: big.NewInt(1)}}
	for _, o := range objs {
		for _, f := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%d"} {
			s := fmt.Sprintf(f, o)
			for _, secret := range []string{testPass, testUser} {
				if strings.Contains(s, secret) {
					t.Fatalf("%s of %T contains a credential: %s", f, o, s)
				}
			}
		}
		if j, err := json.Marshal(o); err == nil {
			if strings.Contains(string(j), testPass) || strings.Contains(string(j), testUser) {
				t.Fatalf("json of %T contains a credential: %s", o, j)
			}
		}
	}
	for _, err := range []error{errReject, err401, errCut} {
		for chain := err; chain != nil; chain = errors.Unwrap(chain) {
			if strings.Contains(chain.Error(), testPass) || strings.Contains(chain.Error(), testUser) {
				t.Fatalf("error chain contains a credential: %v", chain)
			}
		}
	}
}

func mustTokenFor(e *env) []byte {
	r := testReq()
	return e.a.token(r, issueOpts{})
}

func TestStatusTextSanitised(t *testing.T) {
	e := newEnv(t, nil)
	e.set(func(w http.ResponseWriter, r *http.Request, _ *tsReq) {
		w.Header().Set("Content-Type", "application/timestamp-reply")
		_, _ = w.Write(seq(seq(intI(2), seq(tl(0x0c, []byte("bad\r\nline "+testPass+strings.Repeat("z", 500)))))))
	})
	_, err := e.target.Submit(context.Background(), testPayload(t))
	if err == nil || strings.ContainsAny(err.Error(), "\r\n") || len(err.Error()) > 250 {
		t.Fatalf("%q", err)
	}
}
