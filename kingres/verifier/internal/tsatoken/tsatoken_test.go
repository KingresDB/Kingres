// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package tsatoken

import (
	"bytes"
	"crypto/sha256"
	"crypto/x509"
	"strings"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/verifier/internal/anchortest"
)

func pool(t *testing.T, pem string) *x509.CertPool {
	p := x509.NewCertPool()
	if !p.AppendCertsFromPEM([]byte(pem)) {
		t.Fatal("pem")
	}
	return p
}

var digest = sha256.Sum256([]byte("checkpoint text"))

func TestAGoodTokenVerifiesWithECDSAAndRSA(t *testing.T) {
	for _, rsa := range []bool{false, true} {
		a := anchortest.NewTSA(t, anchortest.TSAOpts{Digest: digest, RSA: rsa, WithName: true, Nonce: true})
		res, err := Verify(a.Token, Options{Digest: digest, Roots: pool(t, a.RootPEM)})
		if err != nil {
			t.Fatalf("rsa=%v: %v", rsa, err)
		}
		if !res.GenTime.Equal(a.GenTime) || res.Serial != a.Serial || res.Policy != a.Policy || res.SignerKeySHA256 != a.KeySHA {
			t.Fatalf("%+v", res)
		}
		// the pin
		if _, err := Verify(a.Token, Options{Digest: digest, Roots: pool(t, a.RootPEM), KeySHA256: &a.KeySHA}); err != nil {
			t.Fatal(err)
		}
		other := a.KeySHA
		other[0] ^= 1
		if _, err := Verify(a.Token, Options{Digest: digest, Roots: pool(t, a.RootPEM), KeySHA256: &other}); err == nil || !strings.Contains(err.Error(), "not the one you pinned") {
			t.Fatalf("pin: %v", err)
		}
	}
}

func TestATokenIsRefusedForEveryWrongness(t *testing.T) {
	cases := []struct {
		name string
		o    anchortest.TSAOpts
		want string
	}{
		{"another imprint", anchortest.TSAOpts{Imprint: &[32]byte{1}}, "stamps something else"},
		{"bad signature", anchortest.TSAOpts{BadSignature: true}, "signature does not verify"},
		{"bad signature rsa", anchortest.TSAOpts{BadSignature: true, RSA: true}, "signature does not verify"},
		{"message digest attribute", anchortest.TSAOpts{WrongDigest: true}, "not the digest of the TSTInfo"},
		{"no ESS attribute", anchortest.TSAOpts{NoESS: true}, "lack the content type"},
		{"EKU not critical", anchortest.TSAOpts{NotCriticalEKU: true}, "not critical"},
		{"EKU with another purpose", anchortest.TSAOpts{ExtraEKU: true}, "alone"},
		{"no EKU", anchortest.TSAOpts{NoEKU: true}, "no extended key usage"},
		{"wrong tsa name", anchortest.TSAOpts{WrongName: true}, "not a subject name"},
		{"ESS names another certificate", anchortest.TSAOpts{WrongESS: true}, "does not name the certificate"},
		{"two signers", anchortest.TSAOpts{TwoSigners: true}, "expected exactly 1"},
		{"certificate expired at genTime", anchortest.TSAOpts{CertNotAfter: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), CertNotBefore: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}, "does not chain"},
		{"certificate not yet valid at genTime", anchortest.TSAOpts{CertNotBefore: time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC), CertNotAfter: time.Date(2027, 12, 1, 0, 0, 0, 0, time.UTC)}, "does not chain"},
	}
	for _, c := range cases {
		o := c.o
		o.Digest = digest
		a := anchortest.NewTSA(t, o)
		_, err := Verify(a.Token, Options{Digest: digest, Roots: pool(t, a.RootPEM)})
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v (want %q)", c.name, err, c.want)
		}
	}
}

func TestACertificateThatExpiredAfterGenTimeStaysGood(t *testing.T) {
	// valid in 2020, expired for years by the clock of the run
	a := anchortest.NewTSA(t, anchortest.TSAOpts{Digest: digest, GenTime: time.Date(2020, 6, 1, 0, 0, 0, 0, time.UTC), CertNotBefore: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC), CertNotAfter: time.Date(2021, 1, 1, 0, 0, 0, 0, time.UTC)})
	if _, err := Verify(a.Token, Options{Digest: digest, Roots: pool(t, a.RootPEM)}); err != nil {
		t.Fatal(err)
	}
}

func TestOnlyTheAuditorsRootsCount(t *testing.T) {
	a := anchortest.NewTSA(t, anchortest.TSAOpts{Digest: digest})
	b := anchortest.NewTSA(t, anchortest.TSAOpts{Digest: digest}) // another authority, another root
	if _, err := Verify(a.Token, Options{Digest: digest, Roots: pool(t, b.RootPEM)}); err == nil || !strings.Contains(err.Error(), "does not chain") {
		t.Fatalf("%v", err)
	}
	if _, err := Verify(a.Token, Options{Digest: digest}); err == nil || !strings.Contains(err.Error(), "no root") {
		t.Fatalf("%v", err)
	}
}

func TestTheEncodingIsStrict(t *testing.T) {
	a := anchortest.NewTSA(t, anchortest.TSAOpts{Digest: digest})
	if _, err := Parse(append(append([]byte{}, a.Token...), 0)); err == nil {
		t.Error("trailing byte accepted")
	}
	if _, err := Parse(nil); err == nil {
		t.Error("empty accepted")
	}
	if _, err := Parse(make([]byte, MaxToken+1)); err == nil {
		t.Error("over-long accepted")
	}
	// every truncation and every single-byte change either fails to parse or fails to verify
	roots := pool(t, a.RootPEM)
	for i := 0; i < len(a.Token); i++ {
		if _, err := Verify(a.Token[:i], Options{Digest: digest, Roots: roots}); err == nil {
			t.Fatalf("truncation at %d accepted", i)
		}
	}
	// the one place a byte may change without effect is the copy of the root the token carries: it is never trusted (the
	// auditor's pool is), and the signer chains to it directly
	tok, _ := Parse(a.Token)
	ca := bytes.Index(a.Token, tok.Certs[1].Raw)
	for i := 0; i < len(a.Token); i++ {
		m := append([]byte{}, a.Token...)
		m[i] ^= 0x01
		if _, err := Verify(m, Options{Digest: digest, Roots: roots}); err == nil && (i < ca || i >= ca+len(tok.Certs[1].Raw)) {
			t.Errorf("a flipped byte at %d was accepted (the root copy is at %d to %d)", i, ca, ca+len(tok.Certs[1].Raw))
		}
	}
}

func FuzzParse(f *testing.F) {
	for _, rsa := range []bool{false, true} {
		a := anchortest.NewTSA(f, anchortest.TSAOpts{Digest: digest, RSA: rsa, WithName: true, Nonce: true})
		f.Add(a.Token)
	}
	f.Add([]byte{0x30, 0x80, 0x00, 0x00})
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, b []byte) {
		tok, err := Parse(b)
		if err != nil {
			return
		}
		// a token that parses is never verified without a root, and never by an empty pool
		if _, err := tok.Verify(Options{Digest: digest}); err == nil {
			t.Fatal("verified without a root")
		}
		if _, err := tok.Verify(Options{Digest: digest, Roots: x509.NewCertPool()}); err == nil {
			t.Fatal("verified against an empty pool")
		}
	})
}
