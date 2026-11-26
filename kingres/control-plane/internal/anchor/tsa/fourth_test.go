// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package tsa

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/anchor"
)

// Tests of the fourth review (A4-06; correction 97).

// A4-06: the stored-evidence verifiers can apply the pin, and the evidence
// carries the SHA-256 of the signer's public key.
func TestStoredVerifiersApplyThePinAndEvidenceCarriesTheKeyHash(t *testing.T) {
	e := newEnv(t, nil)
	p := testPayload(t)
	rc, err := e.target.Submit(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	ev, err := anchor.ParseTSAEvidence(rc.Evidence)
	if err != nil {
		t.Fatal(err)
	}
	h := SignerKeyHash(e.a.tsa)
	if ev.SignerKeySHA256 != hex.EncodeToString(h[:]) {
		t.Fatalf("the evidence carries %s, the key hash is %x", ev.SignerKeySHA256, h)
	}
	// the right pin, and no pin
	if _, err := VerifyEvidence(ev, p.Digest, h[:], e.a.rootPool, e.a.interPool); err != nil {
		t.Fatal(err)
	}
	der := mustDER(t, ev)
	if _, err := VerifyStoredToken(der, p.Digest, h[:], e.a.rootPool, e.a.interPool); err != nil {
		t.Fatal(err)
	}
	// another key, a pin of the wrong length, an empty non-nil pin: refused
	wrong := h
	wrong[0] ^= 1
	for name, pin := range map[string][]byte{"other-key": wrong[:], "short": h[:5], "empty": {}} {
		if _, err := VerifyEvidence(ev, p.Digest, pin, e.a.rootPool, e.a.interPool); anchor.CodeOf(err) != CodeSignerPin {
			t.Errorf("%s: VerifyEvidence: %v", name, err)
		}
		if _, err := VerifyStoredToken(der, p.Digest, pin, e.a.rootPool, e.a.interPool); anchor.CodeOf(err) != CodeSignerPin {
			t.Errorf("%s: VerifyStoredToken: %v", name, err)
		}
	}
	// a key hash in the evidence that is not the token's signer: refused
	bad := ev
	bad.SignerKeySHA256 = hex.EncodeToString(wrong[:])
	if _, err := VerifyEvidence(bad, p.Digest, nil, e.a.rootPool, e.a.interPool); anchor.CodeOf(err) != CodeEvidenceMismatch {
		t.Fatalf("%v", err)
	}
	// and the evidence must carry one
	none := ev
	none.SignerKeySHA256 = ""
	if _, err := anchor.EncodeEvidence(none); err == nil {
		t.Fatal("evidence without a key hash was encoded")
	}
	if _, err := anchor.EncodeEvidence(func() anchor.TSAEvidence { x := ev; x.SignerKeySHA256 = strings.ToUpper(ev.SignerKeySHA256); return x }()); err == nil {
		t.Fatal("an upper case key hash was encoded")
	}
}

func mustDER(t *testing.T, ev anchor.TSAEvidence) []byte {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString(ev.TokenDERB64)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// A4-06: findSigner tried the first certificate that matched; a token that lists
// another certificate of the same issuer and serial (or key id) before the one
// that signed failed with an ESS mismatch. Every matching certificate is tried.
func TestEveryMatchingCertificateIsTriedAsTheSigner(t *testing.T) {
	for _, ski := range []bool{false, true} {
		a := newAuthority(t, authOpts{ski: true})
		decoyKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		tmpl := *a.tsa
		der, err := x509.CreateCertificate(rand.Reader, &tmpl, a.inter, decoyKey.Public(), a.interKey)
		if err != nil {
			t.Fatal(err)
		}
		decoy, err := x509.ParseCertificate(der)
		if err != nil {
			t.Fatal(err)
		}
		r := testReq()
		tok := a.token(r, issueOpts{useSKI: ski, certsPrefix: decoy.Raw})
		pt, aerr := parseToken(tok)
		if aerr != nil {
			t.Fatal(aerr)
		}
		if got := findSigners(pt); len(got) != 2 || got[0].Equal(a.tsa) || !got[1].Equal(a.tsa) {
			t.Fatalf("ski=%v: %d candidates", ski, len(got))
		}
		info, err := VerifyToken(tok, expectFor(r), a.rootPool, a.interPool)
		if err != nil {
			t.Fatalf("ski=%v: %v", ski, err)
		}
		if !info.Signer.Equal(a.tsa) || info.SignerKeySHA256 != SignerKeyHash(a.tsa) {
			t.Fatal("the wrong certificate signed")
		}
		// a pin of the decoy's key does not make the decoy the signer: it did not sign
		h := SignerKeyHash(decoy)
		if _, err := VerifyToken(tok, Expect{Digest: testDigest, SignerKeySHA256: h[:]}, a.rootPool, a.interPool); err == nil {
			t.Fatal("a token signed by another key passed the pin of the decoy")
		}
		// when none of them can verify, the error is the first one's
		bad := a.token(r, issueOpts{useSKI: ski, certsPrefix: decoy.Raw, ess: "badhash"})
		if _, err := VerifyToken(bad, expectFor(r), a.rootPool, a.interPool); anchor.CodeOf(err) != CodeCertBinding {
			t.Fatalf("ski=%v: %v", ski, err)
		}
	}
}
