// SPDX-License-Identifier: AGPL-3.0-only

package tsa

import (
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"strings"
	"testing"
	"time"
)

func fuzzSeeds(f *testing.F) (*authority, []byte) {
	a := newAuthority(f, authOpts{keyAlg: "p256fixed"})
	r := testReq()
	tok := a.token(r, issueOpts{accuracy: true})
	f.Add(tok)
	f.Add(a.token(r, issueOpts{unsigned: true}))
	f.Add(a.token(r, issueOpts{ess: "v1", useSKI: false}))
	f.Add(a.response(0, tok))
	f.Add(a.response(2, nil))
	f.Add(a.response(3, nil))
	f.Add(reindefinite(tok))
	f.Add([]byte{})
	f.Add([]byte{0x30, 0x80, 0, 0})
	return a, tok
}

// FuzzParseResponse: the TimeStampResp parser never panics, and what it
// accepts is strict DER with a token that lies inside the input.
func FuzzParseResponse(f *testing.F) {
	fuzzSeeds(f)
	f.Fuzz(func(t *testing.T, b []byte) {
		r, err := ParseResponse(b)
		if err != nil {
			return
		}
		if r.HasToken && len(r.Token) > len(b) {
			t.Fatal("token outside the input")
		}
		if checkDER(b, 0) != nil {
			t.Fatal("accepted input that is not strict DER")
		}
	})
}

// verifyFixture is one signer of the embedded fixtures (a token made once by
// another implementation, with its root): the seeds of FuzzVerifyToken come from
// them, so that a worker builds no authority (A5-10: the old seeds made keys,
// certificates and signed tokens in the setup of every worker process).
type verifyFixture struct {
	token  []byte
	digest [32]byte
	roots  *x509.CertPool
	inter  *x509.CertPool
}

func loadVerifyFixtures(tb testing.TB) []verifyFixture {
	tb.Helper()
	d, err := hex.DecodeString(strings.TrimSpace(fixtureDigestHex))
	if err != nil || len(d) != 32 {
		tb.Fatal("fixture digest")
	}
	var digest [32]byte
	copy(digest[:], d)
	var out []verifyFixture
	for _, c := range []struct {
		token string
		root  []byte
	}{{fixtureECToken, fixtureECRoot}, {fixtureRSAToken, fixtureRSARoot}} {
		tok, err := base64.StdEncoding.DecodeString(strings.TrimSpace(c.token))
		if err != nil {
			tb.Fatal(err)
		}
		roots, inter, err := ParseBundle(c.root)
		if err != nil {
			tb.Fatal(err)
		}
		out = append(out, verifyFixture{token: tok, digest: digest, roots: roots, inter: inter})
	}
	return out
}

// checkVerifyInput is the body of FuzzVerifyToken: the token parser and verifier
// never panic, and a token that verifies is strict DER with the digest it was
// asked about.
func checkVerifyInput(t *testing.T, fx []verifyFixture, b []byte) {
	for _, c := range fx {
		info, err := VerifyStoredToken(b, c.digest, nil, c.roots, c.inter)
		if err != nil {
			continue
		}
		if checkDER(b, 0) != nil {
			t.Fatal("accepted a token that is not strict DER")
		}
		if info.GenTime.After(time.Now().Add(time.Hour)) || info.Policy == "" {
			t.Fatalf("accepted a token with %+v", info)
		}
	}
}

// FuzzVerifyToken: see checkVerifyInput. The seeds are the embedded fixtures and
// cuts and re-encodings of them, built once and without a key generation. For a
// short run pass -fuzzminimizetime=0: the default minimization of a new
// interesting input (up to 60 s, every step a full verification) is what makes a
// 25 s run look dead (measured: about 12,000 executions per second without it, a
// few thousand in all with it).
func FuzzVerifyToken(f *testing.F) {
	fx := loadVerifyFixtures(f)
	for _, c := range fx {
		f.Add(c.token)
		f.Add(c.token[:len(c.token)/2])
		f.Add(reindefinite(c.token))
		f.Add(nonMinimalLength(c.token))
	}
	f.Add([]byte{})
	f.Add([]byte{0x30, 0x80, 0, 0})
	f.Fuzz(func(t *testing.T, b []byte) { checkVerifyInput(t, fx, b) })
}

// The seeds of FuzzVerifyToken verify (the fuzz target reaches the signature,
// chain and binding checks, not only the parser), and a cut one does not; and
// loading them does not generate a key: it takes a fraction of a second (A5-10).
func TestFuzzVerifyTokenSeedsReachTheVerifier(t *testing.T) {
	start := time.Now()
	fx := loadVerifyFixtures(t)
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("loading the seeds took %v: a key is generated in the setup again?", d)
	}
	for i, c := range fx {
		if _, err := VerifyStoredToken(c.token, c.digest, nil, c.roots, c.inter); err != nil {
			t.Fatalf("seed %d does not verify: %v", i, err)
		}
		checkVerifyInput(t, fx, c.token)
		checkVerifyInput(t, fx, c.token[:len(c.token)/2])
		checkVerifyInput(t, fx, reindefinite(c.token))
	}
}

// FuzzDER: the strict-DER walker and the element readers never panic.
func FuzzDER(f *testing.F) {
	fuzzSeeds(f)
	f.Fuzz(func(t *testing.T, b []byte) {
		_ = checkDER(b, 0)
		if x, rest, err := readTLV(b); err == nil {
			_ = rest
			_, _ = children(x.content)
			_, _ = parseOID(x)
			_, _ = parseInt(x)
			_, _ = parseGenTime(x)
		}
	})
}

// FuzzSameName: the name comparison never panics, is symmetric, and a name
// that parses is the same name as itself.
func FuzzSameName(f *testing.F) {
	a := newAuthority(f, authOpts{keyAlg: "p256fixed"})
	f.Add(a.tsa.RawSubject, a.root.RawSubject)
	f.Add(a.tsa.RawSubject, a.tsa.RawSubject)
	f.Add([]byte{0x30, 0x00}, []byte{0x30, 0x00})
	f.Add([]byte{}, []byte{1})
	f.Fuzz(func(t *testing.T, x, y []byte) {
		if sameName(x, y) != sameName(y, x) {
			t.Fatal("not symmetric")
		}
		if _, ok := parseName(x); ok && !sameName(x, x) {
			t.Fatal("a name that parses is not the same as itself")
		}
	})
}
