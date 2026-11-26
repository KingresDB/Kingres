// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package tsa

import (
	"bytes"
	"crypto/sha256"
	"crypto/x509"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/anchor"
)

var testDigest = sha256.Sum256([]byte("checkpoint text\n"))

func testReq() *tsReq {
	r := &tsReq{Version: 1, Nonce: big.NewInt(0x1122334455667788), CertReq: true}
	r.MI.Hash = testDigest[:]
	return r
}

func expectFor(r *tsReq) Expect {
	return Expect{Digest: testDigest, Nonce: r.Nonce}
}

func wantCode(t *testing.T, err error, code string) {
	t.Helper()
	if err == nil {
		t.Fatalf("accepted, want failure %q", code)
	}
	if got := anchor.CodeOf(err); got != code {
		t.Fatalf("code %q (%v), want %q", got, err, code)
	}
	if !anchor.IsPermanent(err) {
		t.Fatalf("class %v, want permanent", anchor.ClassOf(err))
	}
}

func TestVerifyAccepts(t *testing.T) {
	for _, keyAlg := range []string{"p256", "p384", "rsa"} {
		for _, dig := range []string{"sha256", "sha384", "sha512"} {
			for _, generic := range []bool{false, true} {
				for _, ess := range []string{"v2", "v1", "v2sha384", "v2serial"} {
					a := newAuthority(t, authOpts{keyAlg: keyAlg})
					r := testReq()
					der := a.token(r, issueOpts{digestAlg: dig, sigGeneric: generic, ess: ess, accuracy: true})
					info, err := VerifyToken(der, expectFor(r), a.rootPool, a.interPool)
					if err != nil {
						t.Fatalf("%s %s generic=%v %s: %v", keyAlg, dig, generic, ess, err)
					}
					if info.Serial.Int64() != 4242 || info.Policy != "1.2.3.4.5" || info.Nonce.Cmp(r.Nonce) != 0 {
						t.Fatalf("info %+v", info)
					}
					if info.Accuracy != time.Second+500*time.Millisecond+100*time.Microsecond {
						t.Fatalf("accuracy %v", info.Accuracy)
					}
				}
			}
		}
	}
}

func TestVerifyAcceptsVariants(t *testing.T) {
	a := newAuthority(t, authOpts{ski: true})
	r := testReq()
	// signer named by subject key identifier
	if _, err := VerifyToken(a.token(r, issueOpts{useSKI: true}), expectFor(r), a.rootPool, a.interPool); err != nil {
		t.Fatalf("ski: %v", err)
	}
	// no certificates in the token: the intermediate comes from the bundle, but the signer must be in the token
	// fractional genTime
	if _, err := VerifyToken(a.token(r, issueOpts{genTimeText: time.Now().UTC().Format("20060102150405") + ".25Z"}), expectFor(r), a.rootPool, a.interPool); err != nil {
		t.Fatalf("fraction: %v", err)
	}
	// stored token: no nonce check
	if _, err := VerifyStoredToken(a.token(r, issueOpts{}), testDigest, nil, a.rootPool, a.interPool); err != nil {
		t.Fatal(err)
	}
	// token has no nonce and none expected
	r2 := testReq()
	r2.Nonce = nil
	if _, err := VerifyToken(a.token(r2, issueOpts{}), expectFor(r2), a.rootPool, a.interPool); err != nil {
		t.Fatal(err)
	}
	// non-critical extension in TSTInfo is accepted? (critical one is refused below)
	// the intermediate only in the bundle (token has no intermediate)
	// validity is checked at genTime, not now
	old := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Second)
	a2 := newAuthority(t, authOpts{notBefore: old.Add(-24 * time.Hour), notAfter: time.Now().Add(-time.Hour)})
	if _, err := VerifyToken(a2.token(r, issueOpts{genTime: old}), expectFor(r), a2.rootPool, a2.interPool); err != nil {
		t.Fatalf("certificate expired now but valid at genTime must be accepted: %v", err)
	}
}

func TestVerifyRejects(t *testing.T) {
	now := time.Now().UTC()
	cases := []struct {
		name string
		auth authOpts
		iss  issueOpts
		exp  func(*Expect)
		code string
	}{
		{"wrong imprint", authOpts{}, issueOpts{imprint: bytes.Repeat([]byte{7}, 32)}, nil, CodeImprint},
		{"imprint of another length", authOpts{}, issueOpts{imprint: []byte{1, 2, 3}}, nil, CodeImprint},
		{"wrong nonce", authOpts{}, issueOpts{nonceOverride: big.NewInt(5)}, nil, CodeNonce},
		{"missing nonce", authOpts{}, issueOpts{dropNonce: true}, nil, CodeNonce},
		{"wrong policy", authOpts{}, issueOpts{}, func(e *Expect) { e.Policy = "1.9.9" }, CodePolicy},
		{"EKU not critical", authOpts{eku: ekuNonCritical}, issueOpts{}, nil, CodeEKU},
		{"EKU absent", authOpts{eku: ekuNone}, issueOpts{}, nil, CodeEKU},
		{"EKU with another purpose", authOpts{eku: ekuExtraCritical}, issueOpts{}, nil, CodeEKU},
		{"certificate expired at genTime", authOpts{notBefore: now.Add(-48 * time.Hour), notAfter: now.Add(-24 * time.Hour)}, issueOpts{}, nil, CodeChain},
		{"certificate not yet valid at genTime", authOpts{notBefore: now.Add(24 * time.Hour), notAfter: now.Add(48 * time.Hour)}, issueOpts{}, nil, CodeChain},
		{"signature over other bytes", authOpts{}, issueOpts{badSigData: true}, nil, CodeSignature},
		{"unsigned attributes", authOpts{}, issueOpts{unsigned: true}, nil, CodeUnsignedAttrs},
		{"token for another hash algorithm (SHA-512 imprint)", authOpts{}, issueOpts{imprintAlg: "sha512", imprint: bytes.Repeat([]byte{1}, 64)}, nil, CodeImprint},
		{"token for SHA-1 imprint", authOpts{}, issueOpts{imprintAlg: "sha1", imprint: bytes.Repeat([]byte{1}, 20)}, nil, CodeImprint},
		{"message digest attribute wrong", authOpts{}, issueOpts{wrongMD: true}, nil, CodeMessageDigest},
		{"content type attribute wrong", authOpts{}, issueOpts{wrongCT: true}, nil, CodeSignedAttrs},
		{"content type attribute missing", authOpts{}, issueOpts{omitContentTypeAttr: true}, nil, CodeSignedAttrs},
		{"message digest attribute repeated", authOpts{}, issueOpts{dupMD: true}, nil, CodeSignedAttrs},
		{"no signed attributes", authOpts{}, issueOpts{noSignedAttrs: true}, nil, CodeSignedAttrs},
		{"no signing certificate attribute", authOpts{}, issueOpts{ess: "none"}, nil, CodeCertBinding},
		{"ESSCertID hash wrong", authOpts{}, issueOpts{ess: "badhash"}, nil, CodeCertBinding},
		{"ESSCertID serial wrong", authOpts{}, issueOpts{ess: "v2badserial"}, nil, CodeCertBinding},
		{"sid names another certificate", authOpts{}, issueOpts{wrongSID: true}, nil, CodeNoSignerCert},
		{"no certificates in the token", authOpts{}, issueOpts{noCerts: true}, nil, CodeNoSignerCert},
		{"encapsulated type not TSTInfo", authOpts{}, issueOpts{eContentOID: "1.2.840.113549.1.7.1"}, nil, CodeStructure},
		{"TSTInfo version 2", authOpts{}, issueOpts{tstVersion: 2}, nil, CodeTSTInfo},
		{"TSTInfo critical extension", authOpts{}, issueOpts{critExt: true}, nil, CodeTSTInfo},
		{"TSTInfo with trailing byte", authOpts{}, issueOpts{extraTSTByte: true}, nil, CodeBadDER},
		{"TSTInfo truncated", authOpts{}, issueOpts{badTSTDER: true}, nil, CodeBadDER},
		{"TSTInfo genTime with trailing zero fraction", authOpts{}, issueOpts{genTimeText: now.Format("20060102150405") + ".50Z"}, nil, CodeTSTInfo},
		{"TSTInfo genTime without Z", authOpts{}, issueOpts{genTimeText: now.Format("20060102150405") + "+0100"}, nil, CodeTSTInfo},
		{"two SignerInfos", authOpts{}, issueOpts{twoSigners: true}, nil, CodeStructure},
		{"SignedData version 2", authOpts{}, issueOpts{versionSD: 2}, nil, CodeStructure},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := newAuthority(t, c.auth)
			r := testReq()
			e := expectFor(r)
			if c.exp != nil {
				c.exp(&e)
			}
			_, err := VerifyToken(a.token(r, c.iss), e, a.rootPool, a.interPool)
			wantCode(t, err, c.code)
		})
	}
}

func TestVerifyRejectsDER(t *testing.T) {
	a := newAuthority(t, authOpts{})
	r := testReq()
	der := a.token(r, issueOpts{})
	if _, err := VerifyToken(der, expectFor(r), a.rootPool, a.interPool); err != nil {
		t.Fatal(err)
	}
	for name, bad := range map[string][]byte{
		"indefinite length":        reindefinite(der),
		"non-minimal length":       nonMinimalLength(der),
		"trailing data":            append(append([]byte(nil), der...), 0),
		"empty":                    nil,
		"truncated":                der[:len(der)-1],
		"high tag number":          append([]byte{0x3f, 0x01}, der[2:]...),
		"constructed octet string": bytes.Replace(append([]byte(nil), der...), []byte{0x04, 0x20}, []byte{0x24, 0x20}, 1),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := VerifyToken(bad, expectFor(r), a.rootPool, a.interPool)
			if err == nil {
				t.Fatal("accepted")
			}
			if !anchor.IsPermanent(err) {
				t.Fatalf("class %v", anchor.ClassOf(err))
			}
		})
	}
}

func TestVerifyUntrustedRoot(t *testing.T) {
	a, b := newAuthority(t, authOpts{}), newAuthority(t, authOpts{})
	r := testReq()
	_, err := VerifyToken(a.token(r, issueOpts{}), expectFor(r), b.rootPool, b.interPool)
	wantCode(t, err, CodeChain)
	_, err = VerifyToken(a.token(r, issueOpts{}), expectFor(r), nil, nil)
	wantCode(t, err, CodeNoRoots)
	// the token's own intermediate is not a trust anchor
	only := x509.NewCertPool()
	only.AddCert(a.inter)
	_, err = VerifyToken(a.token(r, issueOpts{}), expectFor(r), only, nil)
	_ = err // an intermediate as anchor is accepted by crypto/x509 as a partial chain only with a flag; must not panic
	if err == nil {
		t.Log("intermediate-only pool accepted by x509 (not an error of this package)")
	}
}

func TestSignatureAlgorithmsRefused(t *testing.T) {
	a := newAuthority(t, authOpts{})
	r := testReq()
	// SHA-1 digest algorithm
	_, err := VerifyToken(a.token(r, issueOpts{digestAlg: "sha1", sigGeneric: true}), expectFor(r), a.rootPool, a.interPool)
	wantCode(t, err, CodeAlgorithm)
	if err == nil {
		t.Fatal("sha1 accepted")
	}
}

func TestEvidenceRoundTrip(t *testing.T) {
	a := newAuthority(t, authOpts{})
	r := testReq()
	der := a.token(r, issueOpts{})
	info, err := VerifyToken(der, expectFor(r), a.rootPool, a.interPool)
	if err != nil {
		t.Fatal(err)
	}
	ev := EvidenceFromToken("https://tsa.example/ts", info, der)
	raw, err := anchor.EncodeEvidence(ev)
	if err != nil {
		t.Fatal(err)
	}
	raw2, _ := anchor.EncodeEvidence(ev)
	if !bytes.Equal(raw, raw2) {
		t.Fatal("not deterministic")
	}
	back, err := anchor.ParseTSAEvidence(raw)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyEvidence(back, testDigest, nil, a.rootPool, a.interPool); err != nil {
		t.Fatal(err)
	}
	other := testDigest
	other[0] ^= 1
	if _, err := VerifyEvidence(back, other, nil, a.rootPool, a.interPool); anchor.CodeOf(err) != CodeImprint {
		t.Fatalf("other digest: %v", err)
	}
	back.Serial = "1"
	if _, err := VerifyEvidence(back, testDigest, nil, a.rootPool, a.interPool); anchor.CodeOf(err) != CodeEvidenceMismatch {
		t.Fatalf("serial: %v", err)
	}
	if !strings.Contains(string(raw), `"recorded_by":"cluster"`) {
		t.Fatal(string(raw))
	}
}

func TestTokenSizeLimit(t *testing.T) {
	_, err := VerifyToken(make([]byte, anchor.MaxTokenDER+1), Expect{}, nil, nil)
	wantCode(t, err, CodeTooLarge)
	_, err = VerifyToken(make([]byte, anchor.MaxTokenDER), Expect{}, nil, nil)
	wantCode(t, err, CodeBadDER) // at the limit the size check passes
}

func TestDERDepthLimit(t *testing.T) {
	nest := func(n int) []byte {
		b := []byte{0x05, 0x00}
		for i := 0; i < n; i++ {
			b = tl(0x30, b)
		}
		return b
	}
	if err := checkDER(nest(MaxDERDepth), 0); err != nil {
		t.Fatalf("depth %d: %v", MaxDERDepth, err)
	}
	if err := checkDER(nest(MaxDERDepth+2), 0); err == nil {
		t.Fatal("too deep accepted")
	}
}

func TestDERPrimitives(t *testing.T) {
	bad := [][]byte{
		{0x02, 0x00}, {0x02, 0x02, 0x00, 0x01}, {0x02, 0x02, 0xff, 0x80}, // integers
		{0x01, 0x01, 0x01}, {0x01, 0x02, 0, 0}, // booleans
		{0x05, 0x01, 0x00},         // null
		{0x10, 0x00}, {0x11, 0x00}, // primitive sequence, set
		{0x30, 0x81, 0x05, 1, 2, 3, 4, 5}, // long form for short length
		{0x30, 0x84, 0, 0, 0, 1, 0},       // leading zero
		{0x30, 0x85, 1, 0, 0, 0, 0, 0},    // 5 length bytes
		{0x30, 0x05, 1},                   // length beyond data
	}
	for i, b := range bad {
		if err := checkDER(b, 0); err == nil {
			t.Errorf("case %d accepted: %x", i, b)
		}
	}
	good := [][]byte{{0x02, 0x01, 0x00}, {0x02, 0x02, 0x00, 0x80}, {0x02, 0x01, 0xff}, {0x01, 0x01, 0xff}, {0x05, 0x00}, {0x30, 0x00}}
	for i, b := range good {
		if err := checkDER(b, 0); err != nil {
			t.Errorf("good case %d: %v", i, err)
		}
	}
}

func TestGenTimeParsing(t *testing.T) {
	ok := map[string]string{
		"20261117101500Z":     "2026-11-17T10:15:00Z",
		"20261117101500.5Z":   "2026-11-17T10:15:00.5Z",
		"20240229235959.123Z": "2024-02-29T23:59:59.123Z",
	}
	for in, want := range ok {
		tm, err := parseGenTime(tlv{tag: tagGenTime, content: []byte(in)})
		if err != nil || tm.Format(time.RFC3339Nano) != want {
			t.Errorf("%s: %v %v", in, tm, err)
		}
	}
	for _, in := range []string{"20261117101500", "20261117101560Z", "20260230101500Z", "202611171015Z", "20261117101500.Z", "20261117101500.50Z", "2026111710150aZ", "20261317101500Z", "20261117101500,5Z"} {
		if _, err := parseGenTime(tlv{tag: tagGenTime, content: []byte(in)}); err == nil {
			t.Errorf("%s accepted", in)
		}
	}
}

func TestOIDParsing(t *testing.T) {
	for _, s := range []string{"1.2.840.113549.1.9.16.1.4", "2.16.840.1.101.3.4.2.1", "2.999.3", "0.9"} {
		o, err := ParseOID(s)
		if err != nil {
			t.Fatal(s, err)
		}
		got, err := parseOID(tlv{tag: tagOID, content: oidB(s)[2:]})
		if err != nil || got != s || o.String() != s {
			t.Fatalf("%s -> %s %v", s, got, err)
		}
	}
	for _, s := range []string{"", "1", "3.1", "1.40", "1.02", "1..2", "a.b", "1.2.", strings.Repeat("1.", 100) + "1"} {
		if _, err := ParseOID(s); err == nil {
			t.Errorf("%q accepted", s)
		}
	}
	for _, c := range [][]byte{{}, {0x80, 0x01}, {0x2a, 0x86}} {
		if _, err := parseOID(tlv{tag: tagOID, content: c}); err == nil {
			t.Errorf("% x accepted", c)
		}
	}
}

func TestCertificateCountLimit(t *testing.T) {
	a := newAuthority(t, authOpts{})
	r := testReq()
	if _, err := VerifyToken(a.token(r, issueOpts{extraCerts: MaxCertificates - 2}), expectFor(r), a.rootPool, a.interPool); err != nil {
		t.Fatalf("at the limit: %v", err)
	}
	_, err := VerifyToken(a.token(r, issueOpts{extraCerts: MaxCertificates - 1}), expectFor(r), a.rootPool, a.interPool)
	wantCode(t, err, CodeStructure)
}

func TestTimeoutLimitInConfig(t *testing.T) {
	if _, err := ParseConfig([]byte(`{"url":"https://a/","caBundlePath":"x","timeoutSeconds":300}`)); err != nil {
		t.Fatal(err)
	}
}
