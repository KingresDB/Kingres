// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package tsa

// Cross-checks with OpenSSL, run when an openssl binary is on the PATH and
// skipped otherwise: a token of the test authority is verified by
// "openssl ts -verify", and tokens made by "openssl ts -reply" (RSA and ECDSA
// signers) are verified by this package.

import (
	"bytes"
	"encoding/pem"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func opensslPath(t *testing.T) string {
	t.Helper()
	p, err := exec.LookPath("openssl")
	if err != nil {
		t.Skip("openssl not found")
	}
	return p
}

func runOpenSSL(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command(opensslPath(t), args...)
	cmd.Dir = dir
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("openssl %s: %v\n%s", strings.Join(args, " "), err, out.String())
	}
	return out.String()
}

func writeFile(t *testing.T, dir, name string, b []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestOpenSSLVerifiesOurToken: a token and a whole response of the test
// authority are accepted by "openssl ts -verify" for the same request.
func TestOpenSSLVerifiesOurToken(t *testing.T) {
	opensslPath(t)
	for _, keyAlg := range []string{"p256", "rsa"} {
		t.Run(keyAlg, func(t *testing.T) {
			a := newAuthority(t, authOpts{keyAlg: keyAlg})
			dir := t.TempDir()
			nonce := big.NewInt(0x0badc0ffee123456)
			q, err := BuildRequest(Request{Digest: testDigest, Nonce: nonce})
			if err != nil {
				t.Fatal(err)
			}
			r := parseTSReq(t, q)
			token := a.token(r, issueOpts{accuracy: true})
			writeFile(t, dir, "q.tsq", q)
			writeFile(t, dir, "token.der", token)
			writeFile(t, dir, "resp.tsr", a.response(0, token))
			writeFile(t, dir, "root.pem", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: a.root.Raw}))
			writeFile(t, dir, "inter.pem", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: a.inter.Raw}))
			out := runOpenSSL(t, dir, "ts", "-verify", "-in", "resp.tsr", "-queryfile", "q.tsq", "-CAfile", "root.pem", "-untrusted", "inter.pem")
			if !strings.Contains(out, "Verification: OK") {
				t.Fatal(out)
			}
			out = runOpenSSL(t, dir, "ts", "-verify", "-in", "token.der", "-token_in", "-queryfile", "q.tsq", "-CAfile", "root.pem", "-untrusted", "inter.pem")
			if !strings.Contains(out, "Verification: OK") {
				t.Fatal(out)
			}
			// openssl also parses our request (the serial file of the TSA config is incremented before use)
			txt := runOpenSSL(t, dir, "ts", "-query", "-in", "q.tsq", "-text")
			if !strings.Contains(txt, "Hash Algorithm: sha256") || !strings.Contains(txt, "Certificate required: yes") || !strings.Contains(strings.ToLower(txt), "nonce: 0x0badc0ffee123456") {
				t.Fatal(txt)
			}
			// and refuses a wrong imprint, as we do
			bad := append([]byte(nil), token...)
			rb := testReq()
			rb.MI.Hash = bytes.Repeat([]byte{3}, 32)
			bad = a.token(&tsReq{Version: 1, Nonce: nonce, MI: rb.MI}, issueOpts{})
			writeFile(t, dir, "bad.der", bad)
			cmd := exec.Command(opensslPath(t), "ts", "-verify", "-in", "bad.der", "-token_in", "-queryfile", "q.tsq", "-CAfile", "root.pem", "-untrusted", "inter.pem")
			cmd.Dir = dir
			if err := cmd.Run(); err == nil {
				t.Fatal("openssl accepted a wrong imprint")
			}
		})
	}
}

const opensslTSAConf = `
[tsa]
default_tsa = tsa_config1
[tsa_config1]
serial = ./serial
signer_cert = ./tsa.pem
certs = ./chain.pem
signer_key = ./tsa.key
signer_digest = sha256
default_policy = 1.2.3.4.1
other_policies = 1.2.3.4.2
digests = sha256, sha384, sha512
accuracy = secs:1, millisecs:500, microsecs:100
ordering = yes
tsa_name = yes
ess_cert_id_chain = no
ess_cert_id_alg = sha256
`

// TestOpenSSLTokenVerifiedByUs: tokens made by "openssl ts -reply" are
// accepted by VerifyToken, and by VerifyToken only for the right imprint.
func TestOpenSSLTokenVerifiedByUs(t *testing.T) {
	opensslPath(t)
	for _, keyGen := range [][]string{
		{"genpkey", "-algorithm", "RSA", "-pkeyopt", "rsa_keygen_bits:2048"},
		{"genpkey", "-algorithm", "EC", "-pkeyopt", "ec_paramgen_curve:P-256"},
	} {
		t.Run(keyGen[2], func(t *testing.T) {
			dir := t.TempDir()
			run := func(args ...string) string { return runOpenSSL(t, dir, args...) }
			run(append(keyGen, "-out", "root.key")...)
			run("req", "-new", "-x509", "-key", "root.key", "-subj", "/CN=OpenSSL Test Root", "-days", "3650", "-out", "root.pem",
				"-addext", "basicConstraints=critical,CA:TRUE", "-addext", "keyUsage=critical,keyCertSign,cRLSign")
			run(append(keyGen, "-out", "tsa.key")...)
			run("req", "-new", "-key", "tsa.key", "-subj", "/CN=OpenSSL Test TSA", "-out", "tsa.csr")
			writeFile(t, dir, "ext.cnf", []byte("basicConstraints=CA:FALSE\nkeyUsage=critical,digitalSignature\nextendedKeyUsage=critical,timeStamping\nsubjectKeyIdentifier=hash\n"))
			run("x509", "-req", "-in", "tsa.csr", "-CA", "root.pem", "-CAkey", "root.key", "-CAcreateserial", "-days", "30", "-extfile", "ext.cnf", "-out", "tsa.pem")
			chain, _ := os.ReadFile(filepath.Join(dir, "tsa.pem"))
			rootPEM, _ := os.ReadFile(filepath.Join(dir, "root.pem"))
			writeFile(t, dir, "chain.pem", append(chain, rootPEM...))
			writeFile(t, dir, "serial", []byte("0A1B2C\n"))
			writeFile(t, dir, "ts.cnf", []byte(opensslTSAConf))
			roots, inter, err := ParseBundle(rootPEM)
			if err != nil {
				t.Fatal(err)
			}
			nonce := big.NewInt(0x1234567890abcdef)
			q, err := BuildRequest(Request{Digest: testDigest, Nonce: nonce})
			if err != nil {
				t.Fatal(err)
			}
			writeFile(t, dir, "q.tsq", q)
			run("ts", "-reply", "-queryfile", "q.tsq", "-config", "ts.cnf", "-out", "resp.tsr")
			resp, err := os.ReadFile(filepath.Join(dir, "resp.tsr"))
			if err != nil {
				t.Fatal(err)
			}
			pr, err := ParseResponse(resp)
			if err != nil || pr.Status != StatusGranted || !pr.HasToken {
				t.Fatalf("%v %+v", err, pr)
			}
			info, err := VerifyToken(pr.Token, Expect{Digest: testDigest, Nonce: nonce, Policy: "1.2.3.4.1"}, roots, inter)
			if err != nil {
				t.Fatal(err)
			}
			if info.Serial.Int64() != 0x0a1b2d || !info.Ordering || info.Accuracy != time.Second+500*time.Millisecond+100*time.Microsecond {
				t.Fatalf("%+v", info)
			}
			if time.Since(info.GenTime) > time.Minute || time.Until(info.GenTime) > time.Minute {
				t.Fatalf("genTime %v", info.GenTime)
			}
			other := testDigest
			other[5] ^= 1
			if _, err := VerifyToken(pr.Token, Expect{Digest: other}, roots, inter); err == nil {
				t.Fatal("wrong imprint accepted")
			}
			if _, err := VerifyToken(pr.Token, Expect{Digest: testDigest, Nonce: big.NewInt(7)}, roots, inter); err == nil {
				t.Fatal("wrong nonce accepted")
			}
			// a tampered token is refused
			bad := append([]byte(nil), pr.Token...)
			bad[len(bad)-3] ^= 0x40
			if _, err := VerifyToken(bad, Expect{Digest: testDigest}, roots, inter); err == nil {
				t.Fatal("tampered token accepted")
			}
		})
	}
}
