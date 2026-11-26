// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package pki

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

// CP2-18: the content of a file is flushed before the rename makes it the file,
// and the directory after it, so a crash cannot leave an empty revocation list.
func TestWriteFileFlushesBeforeAndAfterTheRename(t *testing.T) {
	var events []string
	oldFile, oldDir := syncFile, syncDir
	t.Cleanup(func() { syncFile, syncDir = oldFile, oldDir })
	dir := t.TempDir()
	path := filepath.Join(dir, "revoked.json")
	syncFile = func(f *os.File) error {
		if _, err := os.Stat(path); err == nil {
			events = append(events, "file flushed after the rename")
		} else {
			events = append(events, "file")
		}
		return nil
	}
	syncDir = func(string) error {
		if _, err := os.Stat(path); err != nil {
			events = append(events, "directory flushed before the rename")
		} else {
			events = append(events, "directory")
		}
		return nil
	}
	if err := WriteFile(path, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(events, ","); got != "file,directory" {
		t.Fatalf("flush order: %s", got)
	}
	// A failed flush is an error and leaves nothing half written behind.
	syncFile = func(*os.File) error { return errors.New("disk full") }
	if err := WriteFile(path, []byte("{\"changed\":true}"), 0o600); err == nil {
		t.Fatal("a failed flush was ignored")
	}
	if b, _ := os.ReadFile(path); string(b) != "{}" {
		t.Errorf("the file after a failed write: %q", b)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, ".pki-*")); len(left) != 0 {
		t.Errorf("temporary files left: %v", left)
	}
}

// Handshakes that arrive while the bootstrap files do not exist (a server has just reset itself and
// every other server calls it) all ask for the certificate at once: they must all get the same,
// whole pair, and the files must stay a pair for the calls after them.
func TestBootstrapCertIsCreatedOnceUnderConcurrentCalls(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "pki")
	n := Names{CommonName: "127.0.0.3", DNS: []string{"localhost"}}
	const callers = 24
	fps := make([]string, callers)
	errs := make([]error, callers)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, fps[i], errs[i] = EnsureBootstrapCert(dir, n, t0)
		}()
	}
	close(start)
	wg.Wait()
	for i := range fps {
		if errs[i] != nil {
			t.Fatalf("caller %d: %v", i, errs[i])
		}
		if fps[i] != fps[0] {
			t.Fatalf("caller %d got another certificate than caller 0", i)
		}
	}
	if _, fp, err := EnsureBootstrapCert(dir, n, t0); err != nil || fp != fps[0] {
		t.Fatalf("the files are not a pair after the calls: %v", err)
	}
}

// Half of a pair (a crash between the two writes) is replaced, not kept for ever.
func TestBootstrapCertWithoutItsCertificateIsMadeAgain(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "pki")
	n := Names{CommonName: "127.0.0.3"}
	if _, _, err := EnsureBootstrapCert(dir, n, t0); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, BootstrapCertFile)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := EnsureBootstrapCert(dir, n, t0); err != nil {
		t.Fatalf("a key without its certificate must not stay broken: %v", err)
	}
}
