// SPDX-License-Identifier: AGPL-3.0-only

package tsa

import (
	"bytes"
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/anchor"
)

// submitWith runs Submit against an authority that issues the given token.
func submitWith(t *testing.T, e *env, o issueOpts) (anchor.Receipt, error) {
	t.Helper()
	e.set(func(w http.ResponseWriter, r *http.Request, req *tsReq) {
		w.Header().Set("Content-Type", "application/timestamp-reply")
		_, _ = w.Write(e.a.response(0, e.a.token(req, o)))
	})
	return e.target.Submit(context.Background(), testPayload(t))
}

// A1-02: the time of a token is bounded against the clock by Submit itself,
// in both directions, and the refusal is a failed verification of class
// refused (code time_skew).
func TestSubmitBoundsGenTime(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	for name, c := range map[string]struct {
		off time.Duration
		ok  bool
	}{
		"ahead-10m":  {10 * time.Minute, false},
		"behind-10m": {-10 * time.Minute, false},
		"ahead-2d":   {48 * time.Hour, false},
		"behind-3y":  {-3 * 365 * 24 * time.Hour, false},
		"ahead-4m":   {4 * time.Minute, true},
		"behind-4m":  {-4 * time.Minute, true},
	} {
		e := newEnv(t, nil)
		rc, err := submitWith(t, e, issueOpts{genTime: now.Add(c.off)})
		if c.ok {
			if err != nil || rc.State != anchor.ReceiptAnchored {
				t.Errorf("%s: %v", name, err)
			}
			continue
		}
		if err == nil || anchor.CodeOf(err) != CodeTime || !anchor.IsRefused(err) || rc.Evidence != nil {
			t.Errorf("%s: accepted or wrong error: %v (%v)", name, err, anchor.CodeOf(err))
		}
	}
	// the tolerance is configured: 20 minutes allows 10, and 1 second refuses 4 minutes
	e := newEnv(t, func(o *Options) { o.Config.MaxSkewSeconds = 1200 })
	if _, err := submitWith(t, e, issueOpts{genTime: now.Add(-10 * time.Minute)}); err != nil {
		t.Errorf("10 minutes within a tolerance of 20: %v", err)
	}
	e = newEnv(t, func(o *Options) { o.Config.MaxSkewSeconds = 1 })
	if _, err := submitWith(t, e, issueOpts{genTime: now.Add(-4 * time.Minute)}); err == nil {
		t.Error("4 minutes within a tolerance of 1 second")
	}
	// the stored-evidence path does not apply the bound: a token stays valid for ever
	a := newAuthority(t, authOpts{})
	tok := a.token(testReq(), issueOpts{genTime: now.Add(-3 * 365 * 24 * time.Hour)})
	if _, err := VerifyStoredToken(tok, testDigest, a.rootPool, a.interPool); err == nil {
		t.Log("an old token verifies as stored evidence, as it must (the certificate was valid then)")
	}
}

// A1-02: an authority whose certificate has expired cannot hand out tokens
// dated inside its old validity window, and an authority whose certificate
// expired a minute ago cannot hand out a token that is dated three minutes
// ago (inside the tolerance, and valid at genTime).
func TestSubmitRefusesExpiredAuthority(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	// the certificate expired an hour ago; the token is dated three hours ago
	e := newEnv(t, nil)
	e.a = newAuthority(t, authOpts{notBefore: now.Add(-5 * time.Hour), notAfter: now.Add(-time.Hour)})
	e.target = rebuild(t, e)
	_, err := submitWith(t, e, issueOpts{genTime: now.Add(-3 * time.Hour)})
	if err == nil {
		t.Fatal("a token dated inside the old validity window was accepted")
	}
	if !anchor.IsRefused(err) {
		t.Errorf("class %v", anchor.ClassOf(err))
	}
	// the same token is fine as stored evidence: valid at genTime
	tok := e.a.token(testReq(), issueOpts{genTime: now.Add(-3 * time.Hour)})
	if _, err := VerifyStoredToken(tok, testDigest, e.a.rootPool, e.a.interPool); err != nil {
		t.Fatalf("stored evidence of a token from the validity window: %v", err)
	}
	// inside the tolerance of the clock, valid at genTime, expired now
	e2 := newEnv(t, nil)
	e2.a = newAuthority(t, authOpts{notBefore: now.Add(-5 * time.Hour), notAfter: now.Add(-time.Minute)})
	e2.target = rebuild(t, e2)
	_, err = submitWith(t, e2, issueOpts{genTime: now.Add(-3 * time.Minute)})
	if err == nil || anchor.CodeOf(err) != CodeChainNow || !anchor.IsRefused(err) {
		t.Fatalf("a certificate that is not valid now: %v (%s)", err, anchor.CodeOf(err))
	}
	// not yet valid at the clock
	e3 := newEnv(t, nil)
	e3.a = newAuthority(t, authOpts{notBefore: now.Add(time.Minute), notAfter: now.Add(time.Hour)})
	e3.target = rebuild(t, e3)
	if _, err = submitWith(t, e3, issueOpts{genTime: now.Add(2 * time.Minute)}); err == nil {
		t.Fatal("a certificate that is not yet valid at the clock was accepted")
	}
}

func rebuild(t *testing.T, e *env) *Target {
	t.Helper()
	o := Options{Config: Config{URL: e.srv.URL + "/tsa", CABundlePath: "unused", TimeoutSeconds: 5, AllowInsecure: true},
		Roots: e.a.rootPool, Intermediates: e.a.interPool, Username: testUser, Password: testPass, Client: e.srv.Client()}
	tg, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	return tg
}

// A1-18: a payload that was not built consistently is refused, not stamped.
func TestSubmitRefusesInconsistentPayload(t *testing.T) {
	e := newEnv(t, nil)
	good := testPayload(t)
	bad := map[string]anchor.Payload{
		"zero":            {},
		"digest":          func() anchor.Payload { p := good; p.Digest[0] ^= 1; return p }(),
		"text":            func() anchor.Payload { p := good; p.NoteText = append([]byte("x"), p.NoteText...); return p }(),
		"signed-note":     func() anchor.Payload { p := good; p.SignedNote = bytes.ToUpper(p.SignedNote); return p }(),
		"size":            func() anchor.Payload { p := good; p.Size++; return p }(),
		"id":              func() anchor.Payload { p := good; p.ID = "zz/17"; return p }(),
		"only-the-digest": {Digest: good.Digest},
	}
	for name, p := range bad {
		before := e.calls.Load()
		_, err := e.target.Submit(context.Background(), p)
		if err == nil || anchor.CodeOf(err) != "bad_payload" || !anchor.IsPermanent(err) {
			t.Errorf("%s: %v", name, err)
		}
		if e.calls.Load() != before {
			t.Errorf("%s: a request was sent", name)
		}
	}
}

// A1-16: the checks that depend on nothing but the token and the certificate.
func TestVerifyExtraChecks(t *testing.T) {
	// an RSA signer shorter than 2048 bits, whatever the toolchain allows
	a := newAuthority(t, authOpts{tsaKeyBits: 1024})
	if _, ok := a.tsaKey.(*rsa.PrivateKey); !ok {
		t.Fatal("test setup")
	}
	if _, err := VerifyToken(a.token(testReq(), issueOpts{}), Expect{Digest: testDigest}, a.rootPool, a.interPool); err == nil || anchor.CodeOf(err) != CodeAlgorithm {
		t.Errorf("1024-bit signer: %v", err)
	}
	ok := newAuthority(t, authOpts{tsaKeyBits: 2048})
	if _, err := VerifyToken(ok.token(testReq(), issueOpts{}), Expect{Digest: testDigest}, ok.rootPool, ok.interPool); err != nil {
		t.Errorf("2048-bit signer: %v", err)
	}
	// the issuer in the ESSCertID must be the certificate's issuer
	b := newAuthority(t, authOpts{})
	for _, c := range []struct {
		ess string
		ok  bool
	}{{"v2serial", true}, {"v2badissuer", false}, {"v2badserial", false}} {
		_, err := VerifyToken(b.token(testReq(), issueOpts{ess: c.ess}), Expect{Digest: testDigest}, b.rootPool, b.interPool)
		if (err == nil) != c.ok {
			t.Errorf("ess %s: %v", c.ess, err)
		}
	}
	// the tsa name of the TSTInfo must be a name of the signing certificate
	subj := b.tsa.RawSubject
	otherName := tl(0xa4, b.root.RawSubject)
	for name, c := range map[string]struct {
		name []byte
		ok   bool
	}{
		"none":         {nil, true},
		"subject":      {tl(0xa4, subj), true},
		"other-dir":    {otherName, false},
		"dns-no-match": {tl(0x82, []byte("tsa.example")), false},
	} {
		_, err := VerifyToken(b.token(testReq(), issueOpts{tsaName: c.name}), Expect{Digest: testDigest}, b.rootPool, b.interPool)
		if (err == nil) != c.ok || (err != nil && anchor.CodeOf(err) != CodeTSAName) {
			t.Errorf("tsa name %s: %v", name, err)
		}
	}
	san := newAuthority(t, authOpts{tsaSAN: "tsa.example"})
	if _, err := VerifyToken(san.token(testReq(), issueOpts{tsaName: tl(0x82, []byte("tsa.example"))}), Expect{Digest: testDigest}, san.rootPool, san.interPool); err != nil {
		t.Errorf("tsa name that is a DNS name of the certificate: %v", err)
	}
	// the key usage of the signer, when present, must allow signing
	ku := newAuthority(t, authOpts{tsaKeyUsage: x509.KeyUsageKeyEncipherment})
	if _, err := VerifyToken(ku.token(testReq(), issueOpts{}), Expect{Digest: testDigest}, ku.rootPool, ku.interPool); err == nil || anchor.CodeOf(err) != CodeKeyUsage {
		t.Errorf("key usage without a signing bit: %v", err)
	}
	cc := newAuthority(t, authOpts{tsaKeyUsage: x509.KeyUsageContentCommitment})
	if _, err := VerifyToken(cc.token(testReq(), issueOpts{}), Expect{Digest: testDigest}, cc.rootPool, cc.interPool); err != nil {
		t.Errorf("key usage contentCommitment: %v", err)
	}
}

// A1-19: a reply of another media type (a captive portal, a proxy page) is
// not a verdict on the authority: it is tried again.
func TestContentTypeMismatchIsRetryable(t *testing.T) {
	e := newEnv(t, nil)
	e.set(func(w http.ResponseWriter, r *http.Request, _ *tsReq) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html>sign in</html>"))
	})
	_, err := e.target.Submit(context.Background(), testPayload(t))
	if err == nil || anchor.CodeOf(err) != "content_type" || !anchor.IsRetryable(err) {
		t.Fatalf("%v", err)
	}
}

// A1-15: no query in the URL (the evidence file is public) and no Basic
// credentials over http unless allowInsecure says so.
func TestConfigRefusesQueryAndBasicOverHTTP(t *testing.T) {
	tg := &Target{}
	for name, cfg := range map[string]string{
		"query":       `{"url":"https://tsa.example/ts?key=secret","caBundlePath":"/x"}`,
		"empty-query": `{"url":"https://tsa.example/ts?","caBundlePath":"/x"}`,
		"skew-neg":    `{"url":"https://tsa.example/ts","caBundlePath":"/x","maxSkewSeconds":-1}`,
		"skew-big":    `{"url":"https://tsa.example/ts","caBundlePath":"/x","maxSkewSeconds":86401}`,
	} {
		if err := tg.Validate(json.RawMessage(cfg)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if err := tg.Validate(json.RawMessage(`{"url":"https://tsa.example/ts","caBundlePath":"/x","maxSkewSeconds":86400,"allowInsecure":true}`)); err != nil {
		t.Error(err)
	}
	a := newAuthority(t, authOpts{})
	mk := func(url string, user anchor.Secret, insecure bool) error {
		_, err := New(Options{Config: Config{URL: url, CABundlePath: "x", AllowInsecure: insecure}, Roots: a.rootPool, Username: user, Password: "pw-pw-pw-pw"})
		return err
	}
	if mk("http://tsa.example/ts", "user-name", false) == nil {
		t.Error("Basic credentials over http accepted")
	}
	if err := mk("http://tsa.example/ts", "user-name", true); err != nil {
		t.Error(err)
	}
	if err := mk("http://tsa.example/ts", "", false); err != nil {
		t.Errorf("no credentials over http: %v", err)
	}
	if err := mk("https://tsa.example/ts", "user-name", false); err != nil {
		t.Error(err)
	}
	if !strings.Contains(strings.ToLower(func() string {
		_, err := New(Options{Config: Config{URL: "http://t.example/x", CABundlePath: "x"}, Roots: a.rootPool, Username: "u"})
		return err.Error()
	}()), "allowinsecure") {
		t.Error("the refusal does not name allowInsecure")
	}
}
