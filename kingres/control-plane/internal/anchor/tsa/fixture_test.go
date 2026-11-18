// SPDX-License-Identifier: AGPL-3.0-only

package tsa

// Tokens made once by "openssl ts -reply" (OpenSSL 3, an ECDSA and an RSA
// signer, each under its own root made for the purpose) are part of the test
// binary: the cross-check with a second implementation of RFC 3161 then runs
// where no openssl binary is installed. The certificates are valid for ten
// years from the day they were made; the token is judged at its genTime, so
// the check does not depend on the day it runs.

import (
	_ "embed"
	"encoding/base64"
	"encoding/hex"
	"strings"
	"testing"
	"time"
)

//go:embed testdata/openssl_fixture_digest.hex
var fixtureDigestHex string

//go:embed testdata/openssl_fixture_ec_token.b64
var fixtureECToken string

//go:embed testdata/openssl_fixture_ec_root.pem
var fixtureECRoot []byte

//go:embed testdata/openssl_fixture_rsa_token.b64
var fixtureRSAToken string

//go:embed testdata/openssl_fixture_rsa_root.pem
var fixtureRSARoot []byte

func TestOpenSSLFixtureTokens(t *testing.T) {
	d, err := hex.DecodeString(strings.TrimSpace(fixtureDigestHex))
	if err != nil || len(d) != 32 {
		t.Fatal("digest")
	}
	var digest [32]byte
	copy(digest[:], d)
	for name, c := range map[string]struct {
		token string
		root  []byte
		sig   string
	}{"ecdsa": {fixtureECToken, fixtureECRoot, "ECDSA-SHA-256"}, "rsa": {fixtureRSAToken, fixtureRSARoot, "RSA-SHA-256"}} {
		t.Run(name, func(t *testing.T) {
			tok, err := base64.StdEncoding.DecodeString(strings.TrimSpace(c.token))
			if err != nil {
				t.Fatal(err)
			}
			roots, inter, err := ParseBundle(c.root)
			if err != nil {
				t.Fatal(err)
			}
			info, err := VerifyStoredToken(tok, digest, nil, roots, inter)
			if err != nil {
				t.Fatal(err)
			}
			if info.Serial.Int64() != 0x0a1b2d || info.Policy != "1.2.3.4.1" || !info.Ordering ||
				info.Accuracy != time.Second+500*time.Millisecond+100*time.Microsecond || info.SignatureAlgorithm != c.sig {
				t.Fatalf("%+v", info)
			}
			if info.GenTime.Year() < 2026 {
				t.Fatalf("genTime %v", info.GenTime)
			}
			// the evidence made from it verifies and is bound to the digest
			ev := EvidenceFromToken("https://tsa.example/tsr", info, tok)
			if _, err := VerifyEvidence(ev, digest, nil, roots, inter); err != nil {
				t.Fatal(err)
			}
			other := digest
			other[7] ^= 1
			if _, err := VerifyStoredToken(tok, other, nil, roots, inter); err == nil {
				t.Fatal("another digest accepted")
			}
			bad := append([]byte(nil), tok...)
			bad[len(bad)-3] ^= 0x40
			if _, err := VerifyStoredToken(bad, digest, nil, roots, inter); err == nil {
				t.Fatal("a tampered token accepted")
			}
			// the other fixture's root is not a root of this token
			otherRoot := fixtureRSARoot
			if name == "rsa" {
				otherRoot = fixtureECRoot
			}
			r2, i2, _ := ParseBundle(otherRoot)
			if _, err := VerifyStoredToken(tok, digest, nil, r2, i2); err == nil {
				t.Fatal("verified under another root")
			}
		})
	}
}
