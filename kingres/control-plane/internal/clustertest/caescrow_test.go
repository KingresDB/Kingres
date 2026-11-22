// SPDX-License-Identifier: AGPL-3.0-only

package clustertest_test

import (
	"crypto/x509"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/clustertest"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/pki"
)

// The escrow of the key of the certificate authority (ha-phase5-part2.md, 3.4): the leader keeps a sealed copy of the CA key on the
// server that hosts the standby of the map group; when the map group is failed over, the new leader opens its copy, raises no alert and
// issues certificates without anyone restoring a file.

func hasAuthorityAlert(n *clustertest.Node) bool {
	body := n.Expect(200, "GET", "/alerts", nil)
	items, _ := body["items"].([]any)
	for _, it := range items {
		if a, _ := it.(map[string]any); a["kind"] == "certificateAuthorityUnavailable" {
			return true
		}
	}
	return false
}

func escrowStatus(t *testing.T, n *clustertest.Node) map[string]any {
	t.Helper()
	return n.Expect(200, "GET", "/certificate-authority/escrow", nil)
}

// switchEscrowOn turns the escrow on at the leader and waits until every recipient holds the newest copy.
func switchEscrowOn(t *testing.T, leader *clustertest.Node, cand *clustertest.Node) {
	t.Helper()
	mapLogin(t, leader)
	st := leader.Expect(200, "PUT", "/certificate-authority/escrow", map[string]any{"enabled": true})
	if st["enabled"] != true || st["holdsKey"] != true {
		t.Fatalf("status after switching on: %v", st)
	}
	until(t, 3*time.Minute, "the sealed copy on the standby's server", func() bool {
		s := escrowStatus(t, leader)
		return s["complete"] == true
	})
	rs, _ := escrowStatus(t, leader)["recipients"].([]any)
	if len(rs) != 1 || rs[0].(map[string]any)["serverId"] != cand.Host || rs[0].(map[string]any)["state"] != "current" {
		t.Fatalf("recipients: %v", rs)
	}
}

func escrowFile(n *clustertest.Node, name string) string {
	return filepath.Join(n.DataDir, "pki", "escrow", name)
}

// TestCAEscrowMapGroupFailover: with the escrow on, the failover of the leader's server leaves the new leader with the CA key, without an
// alert, and its certificate authority issues the certificates of the other servers by itself.
func TestCAEscrowMapGroupFailover(t *testing.T) {
	m := newMapCluster(t)
	switchEscrowOn(t, m.old, m.cand)
	// Only the standby's server holds a copy; the key is not readable in the file, and the new leader does not hold ca.key yet.
	if !fileExists(escrowFile(m.cand, pki.EscrowBlobFile)) {
		t.Fatal("the standby's server holds no sealed copy")
	}
	if fileExists(escrowFile(m.other, pki.EscrowBlobFile)) {
		t.Error("a server that cannot become leader holds a copy")
	}
	if fileExists(filepath.Join(m.cand.DataDir, "pki", pki.CAKeyFile)) {
		t.Fatal("the standby's server holds ca.key before the failover")
	}
	blob := string(readFile(t, escrowFile(m.cand, pki.EscrowBlobFile)))
	if strings.Contains(blob, "PRIVATE KEY") {
		t.Fatal("the sealed copy holds the key in the clear")
	}
	caKey := strings.TrimSpace(string(readFile(t, m.old.DataDir, "pki", pki.CAKeyFile)))
	if strings.Contains(blob, caKey) {
		t.Fatal("the sealed copy holds the CA key")
	}
	oldSerial := certOf(t, m.other).SerialNumber

	foKillServer(m.old)
	m.request(t, m.other)
	id := m.recordedTask(t)
	m.cand.WaitTaskDone(id, 12*time.Minute)

	// The new leader has the key and says nothing about it.
	if !fileExists(filepath.Join(m.cand.DataDir, "pki", pki.CAKeyFile)) {
		t.Fatalf("the new leader did not adopt its sealed copy\n%s", m.cand.LogTail(4000))
	}
	mapLogin(t, m.cand)
	time.Sleep(20 * time.Second)
	if hasAuthorityAlert(m.cand) {
		t.Fatal("the new leader raised certificateAuthorityUnavailable although the escrow was complete")
	}
	if s := escrowStatus(t, m.cand); s["enabled"] != true || s["holdsKey"] != true {
		t.Fatalf("escrow status of the new leader: %v", s)
	}

	// Without any request, the other server's renewal job asks the new leader for a certificate and gets one.
	m.other.Stop()
	m.other.Extra = append(m.other.Extra, "-certificate-renew-fraction", "0.99", "-certificate-check-interval", "400ms", "-certificate-retry-interval", "300ms")
	m.other.Start()
	until(t, 3*time.Minute, "a certificate of the new leader on the other server", func() bool {
		b, err := os.ReadFile(filepath.Join(m.other.DataDir, "pki", pki.ServerCertFile))
		if err != nil {
			return false
		}
		c, err := pki.ParseCertPEM(b)
		return err == nil && c.SerialNumber.Cmp(oldSerial) != 0
	})
	// The certificate is the CA's: the CA certificate is the one every server holds, and it verifies it.
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(readFile(t, m.cand.DataDir, "pki", pki.CACertFile))
	if _, err := certOf(t, m.other).Verify(x509.VerifyOptions{Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		t.Fatalf("the renewed certificate is not the CA's: %v", err)
	}
	// And the operator's own request is served by the new leader, too.
	mapLogin(t, m.cand)
	body := m.cand.Expect(202, "POST", "/servers/"+m.other.Host+"/certificates/renewals", nil)
	m.cand.WaitTaskDone(clustertest.TaskID(body), 3*time.Minute)
}

// TestCAEscrowTamperedCopyIsNotAdopted: a copy that was changed on the standby's disk does not open, so the new leader does not
// get a key from it and says so with the alert; nothing is written as ca.key.
func TestCAEscrowTamperedCopyIsNotAdopted(t *testing.T) {
	m := newMapCluster(t)
	switchEscrowOn(t, m.old, m.cand)
	p := escrowFile(m.cand, pki.EscrowBlobFile)
	b := readFile(t, p)
	// Flip a byte of the sealed content (inside the base64 of the ciphertext).
	i := strings.Index(string(b), `"ciphertext":"`) + len(`"ciphertext":"`) + 10
	b[i] ^= 0x01
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	foKillServer(m.old)
	m.request(t, m.other)
	id := m.recordedTask(t)
	m.cand.WaitTaskDone(id, 12*time.Minute)
	mapLogin(t, m.cand)
	until(t, 2*time.Minute, "the alert about the certificate authority", func() bool { return hasAuthorityAlert(m.cand) })
	if fileExists(filepath.Join(m.cand.DataDir, "pki", pki.CAKeyFile)) {
		t.Fatal("ca.key was written from a damaged copy")
	}
}
