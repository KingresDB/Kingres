// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package pki

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC)

func newCA(t *testing.T) *CA {
	t.Helper()
	ca, err := CreateCA(filepath.Join(t.TempDir(), "pki"), "prod", t0)
	if err != nil {
		t.Fatal(err)
	}
	return ca
}

func parse(t *testing.T, certPEM []byte) *x509.Certificate {
	t.Helper()
	b, _ := pem.Decode(certPEM)
	c, err := x509.ParseCertificate(b.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestIssueAndVerify(t *testing.T) {
	ca := newCA(t)
	names := Names{CommonName: "host1.example", DNS: []string{"alias.example"}, IPs: []net.IP{net.ParseIP("10.0.0.5")}}
	_, csr, err := NewKeyAndCSR(names)
	if err != nil {
		t.Fatal(err)
	}
	certPEM, serial, err := ca.Sign(csr, names, t0)
	if err != nil {
		t.Fatal(err)
	}
	cert := parse(t, certPEM)
	if cert.IsCA || cert.Subject.CommonName != "host1.example" || SerialHex(cert.SerialNumber) != serial {
		t.Errorf("certificate %+v", cert.Subject)
	}
	if cert.NotAfter.Sub(cert.NotBefore) < 364*24*time.Hour || cert.NotAfter.Sub(cert.NotBefore) > 367*24*time.Hour {
		t.Errorf("lifetime %v", cert.NotAfter.Sub(cert.NotBefore))
	}
	if !contains(cert.DNSNames, "host1.example") || len(cert.IPAddresses) != 1 {
		t.Errorf("names %v %v", cert.DNSNames, cert.IPAddresses)
	}
	if cn, err := ca.VerifyPeer(cert, t0); err != nil || cn != "host1.example" {
		t.Errorf("verify: %q %v", cn, err)
	}
	if _, err := ca.VerifyPeer(cert, t0.Add(2*ServerLifetime)); err == nil {
		t.Error("an expired certificate was accepted")
	}
	// The CA signs only certificates; its key file is private.
	if runtime.GOOS != "windows" {
		st, err := os.Stat(filepath.Join(ca.dir, CAKeyFile))
		if err != nil || st.Mode().Perm() != 0o600 {
			t.Errorf("CA key mode %v %v", st.Mode(), err)
		}
	}
	// Reload from disk.
	again, err := LoadCA(ca.dir)
	if err != nil || again.Cert().SerialNumber.Cmp(ca.Cert().SerialNumber) != 0 {
		t.Errorf("reload: %v", err)
	}
	if _, err := CreateCA(ca.dir, "x", t0); err == nil {
		t.Error("a second CA was created over the first")
	}
}

func TestSignRefusals(t *testing.T) {
	ca := newCA(t)
	good := Names{CommonName: "host1"}
	mk := func(tpl *x509.CertificateRequest, key any) []byte {
		der, err := x509.CreateCertificateRequest(rand.Reader, tpl, key)
		if err != nil {
			t.Fatal(err)
		}
		return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der})
	}
	ec, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	ec384, _ := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	rsaKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	cases := map[string][]byte{
		"RSA key":           mk(&x509.CertificateRequest{Subject: pkix.Name{CommonName: "host1"}, DNSNames: []string{"host1"}}, rsaKey),
		"P-384 key":         mk(&x509.CertificateRequest{Subject: pkix.Name{CommonName: "host1"}, DNSNames: []string{"host1"}}, ec384),
		"other common name": mk(&x509.CertificateRequest{Subject: pkix.Name{CommonName: "host2"}, DNSNames: []string{"host2"}}, ec),
		"extra DNS name":    mk(&x509.CertificateRequest{Subject: pkix.Name{CommonName: "host1"}, DNSNames: []string{"host1", "other"}}, ec),
		"extra address":     mk(&x509.CertificateRequest{Subject: pkix.Name{CommonName: "host1"}, IPAddresses: []net.IP{net.ParseIP("10.9.9.9")}}, ec),
		"email name":        mk(&x509.CertificateRequest{Subject: pkix.Name{CommonName: "host1"}, EmailAddresses: []string{"a@b.c"}}, ec),
		"not PEM":           []byte("garbage"),
		"wrong PEM type":    pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte{1, 2, 3}}),
	}
	for name, csr := range cases {
		if _, _, err := ca.Sign(csr, good, t0); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// A request with a broken signature.
	csr := mk(&x509.CertificateRequest{Subject: pkix.Name{CommonName: "host1"}, DNSNames: []string{"host1"}}, ec)
	blk, _ := pem.Decode(csr)
	blk.Bytes[len(blk.Bytes)-3] ^= 0xff
	if _, _, err := ca.Sign(pem.EncodeToMemory(blk), good, t0); err == nil {
		t.Error("a request with a damaged signature was signed")
	}
	// Allowed names that are not host names never reach the signer.
	for _, bad := range []Names{{CommonName: ""}, {CommonName: "a b"}, {CommonName: "x;y"}, {CommonName: "ok", DNS: []string{"bad name"}}} {
		if _, _, err := NewKeyAndCSR(bad); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("names %+v: %v", bad, err)
		}
	}
}

func TestRenewalThreshold(t *testing.T) {
	ca := newCA(t)
	n := Names{CommonName: "host1"}
	_, csr, _ := NewKeyAndCSR(n)
	pemb, _, _ := ca.Sign(csr, n, t0)
	c := parse(t, pemb)
	if NeedsRenewal(c, t0.Add(100*24*time.Hour)) {
		t.Error("renewal asked after 100 days")
	}
	if !NeedsRenewal(c, t0.Add(250*24*time.Hour)) {
		t.Error("renewal not asked with less than a third left")
	}
}

func TestRevocation(t *testing.T) {
	ca := newCA(t)
	n := Names{CommonName: "host1"}
	_, csr, _ := NewKeyAndCSR(n)
	pemb, serial, _ := ca.Sign(csr, n, t0)
	cert := parse(t, pemb)
	v, _ := NewVerifier(ca.CertPEM())
	if _, err := v.VerifyPeer(cert, t0); err != nil {
		t.Fatal(err)
	}
	if err := ca.Revoke(serial, t0); err != nil {
		t.Fatal(err)
	}
	if _, err := ca.VerifyPeer(cert, t0); !errors.Is(err, ErrRevoked) {
		t.Errorf("revoked certificate accepted by the CA: %v", err)
	}
	v.SetRevoked(ca.Revoked())
	if _, err := v.VerifyPeer(cert, t0); !errors.Is(err, ErrRevoked) {
		t.Errorf("revoked certificate accepted by a verifier: %v", err)
	}
	// The list survives a reload and is published as a CRL that lists the serial.
	again, err := LoadCA(ca.dir)
	if err != nil || !again.IsRevoked(serial) {
		t.Errorf("revocation lost: %v", err)
	}
	crlPEM, err := again.CRL(t0)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := pem.Decode(crlPEM)
	crl, err := x509.ParseRevocationList(b.Bytes)
	if err != nil || len(crl.RevokedCertificateEntries) != 1 || SerialHex(crl.RevokedCertificateEntries[0].SerialNumber) != serial {
		t.Fatalf("crl: %v %v", crl, err)
	}
	if err := crl.CheckSignatureFrom(ca.Cert()); err != nil {
		t.Errorf("crl signature: %v", err)
	}
	if err := ca.Revoke("not hex!", t0); err == nil {
		t.Error("an invalid serial was recorded")
	}
	// A certificate of another CA is never accepted.
	other := newCA(t)
	_, csr2, _ := NewKeyAndCSR(n)
	op, _, _ := other.Sign(csr2, n, t0)
	if _, err := v.VerifyPeer(parse(t, op), t0); err == nil {
		t.Error("a certificate of another CA was accepted")
	}
}

func TestFingerprint(t *testing.T) {
	ca := newCA(t)
	fp := Fingerprint(ca.Cert().Raw)
	if len(fp) != 64 || fp != Fingerprint(ca.Cert().Raw) {
		t.Errorf("fingerprint %q", fp)
	}
}

// A CA with a short certificate lifetime (tests, and operators who want it)
// issues certificates of exactly that lifetime, with a back-dating that is a
// small part of it, and the renewal threshold follows the configured part.
func TestShortLifetimeAndRenewalFraction(t *testing.T) {
	ca := newCA(t)
	ca.SetServerLifetime(20 * time.Second)
	n := Names{CommonName: "host1"}
	_, csr, _ := NewKeyAndCSR(n)
	pemb, _, err := ca.Sign(csr, n, t0)
	if err != nil {
		t.Fatal(err)
	}
	c := parse(t, pemb)
	if got := c.NotAfter.Sub(t0); got != 20*time.Second {
		t.Errorf("notAfter - now = %v, want 20s", got)
	}
	if back := t0.Sub(c.NotBefore); back <= 0 || back > 3*time.Second {
		t.Errorf("back-dating %v is not a small part of a 20 s lifetime", back)
	}
	if _, err := ca.VerifyPeer(c, t0.Add(time.Second)); err != nil {
		t.Errorf("a young certificate was refused: %v", err)
	}
	if _, err := ca.VerifyPeer(c, t0.Add(21*time.Second)); err == nil {
		t.Error("an expired certificate was accepted")
	}
	life := c.NotAfter.Sub(c.NotBefore)
	if NeedsRenewalAt(c, t0, 0.25) || !NeedsRenewalAt(c, c.NotAfter.Add(-life/5), 0.25) {
		t.Error("the renewal threshold does not follow the fraction")
	}
	if NeedsRenewalAt(c, c.NotAfter.Add(-life/3), 0.2) || !NeedsRenewalAt(c, c.NotAfter.Add(-life/3), 0.5) {
		t.Error("a larger fraction must ask earlier")
	}
	if NeedsRenewalAt(c, t0, 0) || !NeedsRenewalAt(c, c.NotAfter.Add(time.Second), 0.0001) {
		t.Error("an expired certificate always needs renewal; a zero fraction never asks before expiry")
	}
}

func TestRemainingFraction(t *testing.T) {
	ca := newCA(t)
	n := Names{CommonName: "host1"}
	_, csr, _ := NewKeyAndCSR(n)
	pemb, _, _ := ca.Sign(csr, n, t0)
	c := parse(t, pemb)
	if r := RemainingFraction(c, c.NotBefore); r < 0.99 || r > 1.01 {
		t.Errorf("fraction at notBefore = %v", r)
	}
	if r := RemainingFraction(c, c.NotAfter.Add(time.Hour)); r != 0 {
		t.Errorf("fraction after expiry = %v", r)
	}
}
