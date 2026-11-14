// SPDX-License-Identifier: PostgreSQL

package pki

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestIPServerIDIsANameOfTheCertificate(t *testing.T) {
	ca := newCA(t)
	names := Names{CommonName: "127.0.0.2", DNS: []string{"localhost"}, IPs: []net.IP{net.ParseIP("127.0.0.1")}}
	key, csr, err := NewKeyAndCSR(names)
	if err != nil {
		t.Fatal(err)
	}
	crt, _, err := ca.Sign(csr, names, t0)
	if err != nil {
		t.Fatal(err)
	}
	c := parse(t, crt)
	if err := c.VerifyHostname("127.0.0.2"); err != nil {
		t.Errorf("peers dial the server id, which must be a name of the certificate: %v", err)
	}
	if !CertMatchesKey(c, key) {
		t.Error("the certificate does not match the key it was requested for")
	}
	// The same key again gives a request for the same public key.
	csr2, err := CSRFromKey(key, names)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := CSRPublicKeyDER(csr)
	b, _ := CSRPublicKeyDER(csr2)
	if string(a) != string(b) {
		t.Error("CSRFromKey changed the public key")
	}
}

func TestCheckServerCertificate(t *testing.T) {
	ca := newCA(t)
	names := Names{CommonName: "db2.example", DNS: []string{"localhost"}}
	key, csr, _ := NewKeyAndCSR(names)
	crt, _, err := ca.Sign(csr, names, t0)
	if err != nil {
		t.Fatal(err)
	}
	now := t0.Add(time.Hour)
	if err := CheckServerCertificate(ca.CertPEM(), crt, key, "db2.example", now); err != nil {
		t.Fatalf("a good certificate: %v", err)
	}
	other, _, _ := NewKeyAndCSR(names)
	for name, f := range map[string]func() error{
		"another key":    func() error { return CheckServerCertificate(ca.CertPEM(), crt, other, "db2.example", now) },
		"another server": func() error { return CheckServerCertificate(ca.CertPEM(), crt, key, "db3.example", now) },
		"expired": func() error {
			return CheckServerCertificate(ca.CertPEM(), crt, key, "db2.example", t0.Add(400*24*time.Hour))
		},
		"another CA": func() error {
			o := newCA(t)
			return CheckServerCertificate(o.CertPEM(), crt, key, "db2.example", now)
		},
	} {
		if f() == nil {
			t.Errorf("%s: the certificate was accepted", name)
		}
	}
}

func TestBootstrapCertAndFingerprints(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "pki")
	n := Names{CommonName: "127.0.0.3", DNS: []string{"localhost"}}
	pair, fp, err := EnsureBootstrapCert(dir, n, t0)
	if err != nil {
		t.Fatal(err)
	}
	_, fp2, _ := EnsureBootstrapCert(dir, n, t0.Add(time.Hour))
	if fp != fp2 || fp != Fingerprint(pair.Certificate[0]) {
		t.Error("the bootstrap certificate must be stable so that its fingerprint can be pinned")
	}
	if st, err := os.Stat(filepath.Join(dir, BootstrapKeyFile)); err != nil || runtimeMode(st) {
		t.Errorf("bootstrap key: %v %v", st, err)
	}
	for in, want := range map[string]string{
		strings.ToUpper(FormatFingerprint(fp)): fp, " " + fp + " ": fp, "zz": "", fp[:62]: "", strings.Repeat("g", 64): "",
	} {
		if got := NormalizeFingerprint(in); got != want {
			t.Errorf("NormalizeFingerprint(%q) = %q, want %q", in, got, want)
		}
	}
}

func runtimeMode(st os.FileInfo) bool {
	// On Windows the mode bits say nothing; elsewhere the key is owner-only.
	return os.PathSeparator == '/' && st.Mode().Perm()&0o077 != 0
}

func TestRevokedListRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if l, err := LoadRevokedList(dir); err != nil || l != nil {
		t.Fatalf("missing list: %v %v", l, err)
	}
	if err := SaveRevokedList(dir, []string{"0a", "ff"}); err != nil {
		t.Fatal(err)
	}
	l, err := LoadRevokedList(dir)
	if err != nil || len(l) != 2 || l[1] != "ff" {
		t.Fatalf("%v %v", l, err)
	}
}
