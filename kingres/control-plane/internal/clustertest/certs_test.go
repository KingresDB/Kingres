// SPDX-License-Identifier: AGPL-3.0-only

package clustertest_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/clustertest"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/pki"
)

// The certificate tests run real kingresd processes with certificates that live
// seconds (-certificate-lifetime), so that renewal, revocation and the failure of
// the renewal job happen in real time.

const (
	shortLifetime = "24s"
	renewalsPath  = "/servers/%s/certificates/renewals"
	revokesPath   = "/servers/%s/certificates/revocations"
)

// shortCertificates restarts every node with the settings of the renewal job
// that tests want (a quick look and a quick retry); the leader also gets the
// short lifetime, so every certificate it signs from then on lives that long.
func shortCertificates(t *testing.T, c *clustertest.Cluster, lifetime string) {
	t.Helper()
	for _, n := range c.Nodes {
		n.Stop()
		n.Extra = []string{"-certificate-check-interval", "400ms", "-certificate-retry-interval", "300ms", "-monitor-interval", "500ms"}
		if n == c.Leader() {
			n.Extra = append(n.Extra, "-certificate-lifetime", lifetime)
		}
		n.Start()
	}
	for _, n := range c.Nodes {
		waitLogin(t, n)
	}
}

func waitLogin(t *testing.T, n *clustertest.Node) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	for !n.Login(clustertest.AdminUser, clustertest.AdminPass) {
		if time.Now().After(deadline) {
			t.Fatalf("no sign-in on node %d\n%s", n.Index, n.LogTail(3000))
		}
		time.Sleep(time.Second)
	}
}

func certOf(t *testing.T, n *clustertest.Node) *x509.Certificate {
	t.Helper()
	crt, err := pki.ParseCertPEM(readFile(t, n.DataDir, "pki", "server.crt"))
	if err != nil {
		t.Fatal(err)
	}
	return crt
}

// renewNow renews the certificates of a server through the API and waits.
func renewNow(t *testing.T, via, target *clustertest.Node) {
	t.Helper()
	body := via.Expect(202, "POST", fmt.Sprintf(renewalsPath, target.Host), nil)
	if body["kind"] != "certificate.renew" {
		t.Fatalf("task %v", body)
	}
	via.WaitTaskDone(clustertest.TaskID(body), 2*time.Minute)
}

// tlsSerialOf is the serial number of the certificate the PostgreSQL instance of
// n presents now (the handshake is enough, the login does not matter).
func tlsSerialOf(n *clustertest.Node) string {
	cfg, _ := pgx.ParseConfig("")
	cfg.Host, cfg.Port, cfg.Database, cfg.User, cfg.Password = n.Host, uint16(n.PGPort), "postgres", "nobody", "x" //nolint:gosec // test ports
	cfg.Fallbacks, cfg.ConnectTimeout = nil, 5*time.Second
	var serial string
	cfg.TLSConfig = &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12, //nolint:gosec // the instance under test, on loopback
		VerifyConnection: func(cs tls.ConnectionState) error {
			serial = pki.SerialHex(cs.PeerCertificates[0].SerialNumber)
			return nil
		}}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if c, err := pgx.ConnectConfig(ctx, cfg); err == nil {
		_ = c.Close(ctx)
	}
	return serial
}

// internalTLSSerialOf is the serial number the internal API of n presents now.
func internalTLSSerialOf(n *clustertest.Node) string {
	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}, "tcp", n.InternalAddr(), &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS13}) //nolint:gosec // the server under test, on loopback
	if err != nil {
		return ""
	}
	defer func() { _ = conn.Close() }()
	return pki.SerialHex(conn.ConnectionState().PeerCertificates[0].SerialNumber)
}

// pgAs connects to the instance of target presenting the certificate of as: the
// way the sharding extension of server "as" connects to it.
func pgAs(target, as *clustertest.Node, t *testing.T) error {
	t.Helper()
	cfg, _ := pgx.ParseConfig("")
	cfg.Host, cfg.Port, cfg.Database, cfg.User = target.Host, uint16(target.PGPort), "postgres", clustertest.AdminUser //nolint:gosec // test ports
	cfg.Fallbacks, cfg.ConnectTimeout = nil, 10*time.Second
	pair, err := tls.X509KeyPair(readFile(t, as.DataDir, "pki", "server.crt"), readFile(t, as.DataDir, "pki", "server.key"))
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(readFile(t, target.DataDir, "pki", "ca.crt"))
	cfg.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: pool, ServerName: target.Host, Certificates: []tls.Certificate{pair}}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	c, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return err
	}
	var one int
	err = c.QueryRow(ctx, "SELECT 1").Scan(&one)
	_ = c.Close(ctx)
	return err
}

func postmasterPID(t *testing.T, n *clustertest.Node) string {
	t.Helper()
	b := readFile(t, n.DataDir, "instances", "1", "pgdata", "postmaster.pid")
	return strings.SplitN(string(b), "\n", 2)[0]
}

func countAudit(t *testing.T, n *clustertest.Node, action, resource string) int {
	t.Helper()
	cnt := 0
	for _, l := range strings.Split(auditActions(t, n), "\n") {
		if strings.Contains(l, `"action":"`+action+`"`) && (resource == "" || strings.Contains(l, `"resourceId":"`+resource+`"`)) {
			cnt++
		}
	}
	return cnt
}

// Certificates that live seconds are renewed by each server's job before they
// expire; statements that route between the three servers, new connections and
// the internal API keep working through every renewal, without a restart of
// PostgreSQL or kingresd; the instance presents the new certificate after a
// reload.
func TestCertificateRenewalKeepsAThreeServerClusterWorking(t *testing.T) {
	c, _ := formManually(t, 3, nil)
	leader := c.Leader()
	mkDB(t, leader, "renew_db")
	ctx := context.Background()
	admin := leader.MustPG(clustertest.AdminUser, clustertest.AdminPass, "renew_db")
	if _, err := admin.Exec(ctx, "CREATE TABLE t (id int PRIMARY KEY, v text)"); err != nil {
		t.Fatal(err)
	}
	task := leader.Expect(202, "PUT", "/databases/renew_db/schemas/public/tables/t/distribution", map[string]any{"method": "hash", "distributionColumn": "id"})
	leader.WaitTaskDone(clustertest.TaskID(task), 4*time.Minute)
	for i := 1; i <= 60; i++ {
		if _, err := admin.Exec(ctx, "INSERT INTO t VALUES ($1, $2)", i, fmt.Sprintf("row-%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	shortCertificates(t, c, shortLifetime)
	pids := map[int]string{}
	for _, n := range c.Nodes {
		pids[n.Index] = postmasterPID(t, n)
		renewNow(t, leader, n)
	}
	// Sessions that exist before the renewals must survive them.
	long := map[int]*pgx.Conn{}
	for _, n := range c.Nodes {
		long[n.Index] = n.MustPG(clustertest.AdminUser, clustertest.AdminPass, "renew_db")
	}

	first := map[int]*x509.Certificate{}
	for _, n := range c.Nodes {
		first[n.Index] = certOf(t, n)
		if life := first[n.Index].NotAfter.Sub(first[n.Index].NotBefore); life > 40*time.Second {
			t.Fatalf("node %d: the certificate lives %v, not seconds", n.Index, life)
		}
	}
	serials := map[int]map[string]bool{0: {}, 1: {}, 2: {}}
	var failures []string
	fail := func(f string, a ...any) {
		if len(failures) < 12 {
			failures = append(failures, fmt.Sprintf(f, a...))
		}
	}
	deadline := time.Now().Add(60 * time.Second)
	rounds := 0
	for time.Now().Before(deadline) {
		rounds++
		for _, n := range c.Nodes {
			serials[n.Index][pki.SerialHex(certOf(t, n).SerialNumber)] = true
			// The REST API of every server answers with the cluster's state (a follower asks the leader over the internal API).
			if st, out := n.Do("GET", "/servers", nil, nil); st != 200 || len(anyList(out["items"])) != 3 {
				fail("node %d GET /servers: %d %v", n.Index, st, out)
			}
			// A new connection routes statements to the three servers over the certificates.
			cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			conn, err := n.PG(cctx, clustertest.AdminUser, clustertest.AdminPass, "renew_db")
			if err != nil {
				fail("node %d connecting: %v", n.Index, err)
				cancel()
				continue
			}
			var cnt int
			if err := conn.QueryRow(cctx, "SELECT count(*) FROM t").Scan(&cnt); err != nil || cnt != 60 {
				fail("node %d count: %d %v", n.Index, cnt, err)
			}
			if _, err := conn.Exec(cctx, "UPDATE t SET v = v WHERE id IN (1, 2, 3, 4, 5, 6, 7, 8)"); err != nil {
				fail("node %d update: %v", n.Index, err)
			}
			_ = conn.Close(cctx)
			cancel()
		}
		time.Sleep(300 * time.Millisecond)
	}
	for _, f := range failures {
		t.Error(f)
	}
	for _, n := range c.Nodes {
		if got := len(serials[n.Index]); got < 3 {
			t.Errorf("node %d held %d different certificates in %d rounds; the job should have renewed at least twice\n%s\nleader:\n%s", n.Index, got, rounds, certEvents(n, 12), certEvents(leader, 12))
		}
		// kingresd and the instance were not restarted by any renewal.
		if postmasterPID(t, n) != pids[n.Index] {
			t.Errorf("node %d: PostgreSQL was restarted", n.Index)
		}
		// The long-lived session still works.
		var cnt int
		if err := long[n.Index].QueryRow(ctx, "SELECT count(*) FROM t").Scan(&cnt); err != nil || cnt != 60 {
			t.Errorf("node %d: the session opened before the renewals: %d %v", n.Index, cnt, err)
		}
		// The instance and the internal API present what is in the file (the renewed certificate).
		want := pki.SerialHex(certOf(t, n).SerialNumber)
		for try := 0; try < 20 && tlsSerialOf(n) != want; try++ {
			time.Sleep(300 * time.Millisecond)
			want = pki.SerialHex(certOf(t, n).SerialNumber)
		}
		if got := tlsSerialOf(n); got != want {
			t.Errorf("node %d: PostgreSQL presents %s, the file holds %s", n.Index, got, want)
		}
		if got := internalTLSSerialOf(n); got != pki.SerialHex(certOf(t, n).SerialNumber) {
			t.Logf("node %d: internal API presents %s (a renewal may have just happened)", n.Index, got)
		}
		if !n.Running() {
			t.Errorf("node %d: kingresd stopped", n.Index)
		}
	}
	// Every renewal of a follower is audited on the leader, the leader's own on the leader.
	for _, n := range c.Nodes {
		if got := countAudit(t, leader, "certificateRenewed", n.Host); got < 2 {
			t.Errorf("%d certificateRenewed audit events for %s on the leader, want at least 2", got, n.Host)
		}
	}
	// No renewal failed, so no alert and no failure event.
	for _, n := range c.Nodes {
		if got := countAudit(t, n, "certificateRenewalFailed", ""); got != 0 {
			t.Errorf("node %d audited %d renewal failures", n.Index, got)
		}
	}
}

// A revoked certificate is refused by the internal API and by PostgreSQL, the
// server cannot renew, an administrator cannot renew it either, and the way back
// (removing it and joining it again) works.
func TestRevokedCertificateIsRefusedAndTheServerGetsBackIn(t *testing.T) {
	c, _ := formManually(t, 3, nil)
	leader, victim := c.Leader(), c.Nodes[2]
	ops := "ops_cert"
	mkRole(t, leader, ops, "A-Long-Password-1!", "kingres_ops")

	// Before: the certificate of the third server works everywhere.
	pair := loadPair(t, victim)
	if st, out := internalCall(t, leader, pair, "1", "GET", "/state", nil); st != 200 {
		t.Fatalf("before the revocation: %d %v", st, out)
	}
	if err := pgAs(leader, victim, t); err != nil {
		t.Fatalf("before the revocation, PostgreSQL refuses the certificate of the third server: %v", err)
	}
	oldSerial := serialOf(t, victim)

	// Refusals: a role that is not an administrator, an unknown server, the leader itself.
	opsTok := sessionToken(t, leader, ops, "A-Long-Password-1!")
	st, out := leader.Do("POST", fmt.Sprintf(revokesPath, victim.Host), nil, map[string]string{"Authorization": "Bearer " + opsTok})
	if st != 403 {
		t.Errorf("revoking as kingres_ops: %d %v", st, out)
	}
	if st, out := leader.Do("POST", fmt.Sprintf(revokesPath, "10.9.9.9"), nil, nil); st != 404 {
		t.Errorf("revoking an unknown server: %d %v", st, out)
	}
	if st, out := leader.Do("POST", fmt.Sprintf(revokesPath, leader.Host), nil, nil); st != 409 || out["code"] != "server_hosts_map_group" {
		t.Errorf("revoking the leader's certificates: %d %v", st, out)
	}
	if st, out := leader.Do("POST", fmt.Sprintf(renewalsPath, "10.9.9.9"), nil, nil); st != 404 {
		t.Errorf("renewing an unknown server: %d %v", st, out)
	}

	// A server renews only itself, and only with the key it was admitted with.
	_, strangerCSR, err := pki.NewKeyAndCSR(pki.Names{CommonName: victim.Host, DNS: []string{"localhost"}})
	if err != nil {
		t.Fatal(err)
	}
	if st, out := internalCall(t, leader, pair, "1", "POST", "/pki/renew", map[string]any{"csr": string(strangerCSR)}); st != 422 {
		t.Errorf("renewal with another key: %d %v", st, out)
	}
	_, otherCSR, _ := pki.NewKeyAndCSR(pki.Names{CommonName: c.Nodes[1].Host, DNS: []string{"localhost"}})
	if st, out := internalCall(t, leader, pair, "1", "POST", "/pki/renew", map[string]any{"csr": string(otherCSR)}); st != 422 {
		t.Errorf("a server asking for the certificate of another: %d %v", st, out)
	}

	// Revoke through a follower (forwarded).
	body := c.Nodes[1].Expect(202, "POST", fmt.Sprintf(revokesPath, victim.Host), nil)
	if body["kind"] != "certificate.revoke" {
		t.Fatalf("task %v", body)
	}
	leader.WaitTaskDone(clustertest.TaskID(body), 2*time.Minute)
	if st, out := leader.Do("POST", fmt.Sprintf(revokesPath, victim.Host), nil, nil); st != 409 || out["code"] != "certificate_revoked" {
		t.Errorf("revoking twice: %d %v", st, out)
	}

	// After: refused by the internal API of every other server, by PostgreSQL, and renewal is refused.
	for _, target := range []*clustertest.Node{leader, c.Nodes[1]} {
		if st, out := internalCall(t, target, pair, "1", "GET", "/state", nil); st != 401 || out["error"] != "certificate_refused" {
			t.Errorf("internal API of node %d after the revocation: %d %v", target.Index, st, out)
		}
		if err := pgAs(target, victim, t); err == nil {
			t.Errorf("PostgreSQL of node %d accepts the revoked certificate", target.Index)
		} else if !strings.Contains(err.Error(), "revoked") && !strings.Contains(err.Error(), "certificate") {
			t.Errorf("PostgreSQL of node %d refused it with %v", target.Index, err)
		}
	}
	if st, out := internalCall(t, leader, pair, "1", "POST", "/pki/renew", map[string]any{"csr": string(strangerCSR)}); st != 401 {
		t.Errorf("renewal by the revoked server through the internal API: %d %v", st, out)
	}
	if st, out := leader.Do("POST", fmt.Sprintf(renewalsPath, victim.Host), nil, nil); st != 409 || out["code"] != "certificate_revoked" {
		t.Errorf("renewal of a revoked server through the API: %d %v", st, out)
	}
	if !contains(revokedOn(t, leader, leader), oldSerial) {
		t.Error("the revoked serial is not on the leader's list")
	}
	// The revoked server itself got the list (the leader's certificate is valid), and its record says so.
	if got := c.Nodes[1].Servers()[victim.Host]; got == nil || got["certificateRevokedAt"] == nil {
		t.Errorf("GET /servers does not say that the certificates are revoked: %v", got)
	}
	// Its own job was refused by the leader: the server is not renewed.
	if serialOf(t, victim) != oldSerial {
		t.Error("the revoked server holds a new certificate")
	}

	// The way back: remove it (it owns no bucket) and join it again with a new setup token.
	body = leader.Expect(202, "DELETE", "/servers/"+victim.Host, nil)
	leader.WaitTaskDone(clustertest.TaskID(body), 3*time.Minute)
	waitUninitialized(t, victim, 90*time.Second)
	st, add := leader.AddServer(victim, victim.SetupToken(), "")
	if st != 202 {
		t.Fatalf("adding the server again: %d %v", st, add)
	}
	leader.WaitTaskDone(clustertest.TaskID(add), 8*time.Minute)
	if serialOf(t, victim) == oldSerial {
		t.Error("the server came back with the revoked certificate")
	}
	if st, out := internalCall(t, leader, loadPair(t, victim), "1", "GET", "/state", nil); st != 200 {
		t.Errorf("the rejoined server's new certificate is refused: %d %v", st, out)
	}
	if err := pgAs(leader, victim, t); err != nil {
		t.Errorf("PostgreSQL refuses the new certificate: %v", err)
	}
	if got := leader.Servers()[victim.Host]; got == nil || got["certificateRevokedAt"] != nil {
		t.Errorf("the rejoined server is listed as %v", got)
	}
}

// When the manager leader does not hold the CA key the operations that need it
// answer a dedicated 503, and a server's renewal job fails visibly: it retries,
// raises certificateRenewalFailed after three failures in a row, the monitor
// raises certificateExpiring when the certificate gets close to its end, and
// both resolve when the CA is back and the job has renewed.
func TestCertificateAuthorityMissingOnTheLeader(t *testing.T) {
	c, _ := formManually(t, 3, nil)
	leader, follower, third := c.Leader(), c.Nodes[1], c.Nodes[2]
	// Certificates that live a minute: the follower's job looks at 40 s, the
	// monitor warns at 50 s (less than half of the renewal window is left).
	shortCertificates(t, c, "60s")
	renewNow(t, leader, follower)
	renewNow(t, leader, leader)
	start := time.Now()

	// The CA key is lost from the leader (as after a change of the manager leader to another server).
	key := filepath.Join(leader.DataDir, "pki", "ca.key")
	saved := filepath.Join(leader.Dir, "ca.key.saved")
	leader.Stop()
	if err := os.Rename(key, saved); err != nil {
		t.Fatal(err)
	}
	leader.Start()
	waitLogin(t, leader)

	wantCA := func(what string, st int, out map[string]any) {
		t.Helper()
		if st != 503 || out["code"] != "certificate_authority_unavailable" {
			t.Errorf("%s: %d %v", what, st, out)
		}
	}
	st, out := leader.Do("POST", fmt.Sprintf(renewalsPath, follower.Host), nil, nil)
	wantCA("renewServerCertificates", st, out)
	st, out = leader.Do("POST", fmt.Sprintf(revokesPath, follower.Host), nil, nil)
	wantCA("revokeServerCertificates", st, out)
	st, out = follower.Do("POST", fmt.Sprintf(renewalsPath, third.Host), nil, nil) // forwarded: the leader's answer
	wantCA("renewServerCertificates through a follower", st, out)
	st, out = leader.Do("DELETE", "/servers/"+third.Host, nil, nil)
	wantCA("removeServer", st, out)
	// The internal call of the job answers with its own code.
	if st, out := internalCall(t, leader, loadPair(t, follower), "1", "POST", "/pki/renew", map[string]any{"csr": "x"}); st != 503 || out["error"] != "ca_unavailable" {
		t.Errorf("POST /pki/renew without the CA: %d %v", st, out)
	}

	waitAlert := func(kind string, want int, d time.Duration) (message string) {
		t.Helper()
		fsys := follower.ServicePG("kingres_system")
		deadline := time.Now().Add(d)
		for {
			var n int
			err := fsys.QueryRow(context.Background(), `SELECT count(*), coalesce(max(message), '') FROM kingres_cp.alert
				WHERE kind = $1 AND cleared_at IS NULL`, kind).Scan(&n, &message)
			if err == nil && n == want {
				return message
			}
			if time.Now().After(deadline) {
				t.Fatalf("%d open %s alerts on the follower, want %d (%v)\n%s", n, kind, want, err, certEvents(follower, 12))
			}
			time.Sleep(400 * time.Millisecond)
		}
	}
	// The follower's job fails (its certificate lives 60 s): the alert appears after three failures in a row.
	msg := waitAlert("certificateRenewalFailed", 1, 90*time.Second)
	if !strings.Contains(msg, follower.Host) || !strings.Contains(msg, "ca_unavailable") {
		t.Errorf("alert message %q", msg)
	}
	if got := countAudit(t, follower, "certificateRenewalFailed", follower.Host); got != 1 {
		t.Errorf("%d audit events of the failure streak on the follower, want 1 (at its start)", got)
	}
	// The listing of the cluster shows it from any server, with the server and its title.
	var listed map[string]any
	for _, a := range items(t, leader.Expect(200, "GET", "/alerts?state=open&limit=200", nil)) {
		// The leader that lost the CA key cannot renew its own certificate either and raises its own alert.
		if a["kind"] == "certificateRenewalFailed" && a["serverId"] == follower.Host {
			listed = a
		}
	}
	if listed == nil || listed["serverId"] != follower.Host || listed["severity"] != "warning" || listed["title"] != "Certificate renewal failed" {
		t.Errorf("the alert in GET /alerts: %v", listed)
	}
	// The certificate is nearly at its end: the monitor warns.
	waitAlert("certificateExpiring", 1, 60*time.Second)

	// The CA key is back, with time to spare: the job renews, both alerts resolve, the end of the streak is audited.
	if time.Since(start) > 52*time.Second {
		t.Fatalf("the test was too slow (%v) to restore the CA before the certificate expires", time.Since(start))
	}
	leader.Stop()
	if err := os.Rename(saved, key); err != nil {
		t.Fatal(err)
	}
	leader.Start()
	waitLogin(t, leader)
	waitAlert("certificateRenewalFailed", 0, 40*time.Second)
	waitAlert("certificateExpiring", 0, 40*time.Second)
	if got := countAudit(t, follower, "certificateRenewalFailed", follower.Host); got != 2 {
		t.Errorf("%d audit events of the failure streak on the follower, want 2 (start and recovery)", got)
	}
	// The alerts of the leader itself (it had no CA key either) are resolved too: nothing of the two kinds is open anywhere.
	for deadline := time.Now().Add(40 * time.Second); ; time.Sleep(500 * time.Millisecond) {
		open := 0
		var which []string
		for _, a := range items(t, leader.Expect(200, "GET", "/alerts?state=open&limit=200", nil)) {
			if a["kind"] == "certificateRenewalFailed" || a["kind"] == "certificateExpiring" {
				open++
				which = append(which, fmt.Sprint(a["kind"], " ", a["serverId"], " ", a["severity"], " ", a["message"]))
			}
		}
		if open == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Errorf("%d certificate alerts are still open in the cluster: %v", open, which)
			break
		}
	}
	// And the operations work again.
	renewNow(t, leader, third)
}

// certEvents returns the first lines of the log of n about certificates (the
// start of a story is what explains it; LogTail shows only the end).
func certEvents(n *clustertest.Node, max int) string {
	var out []string
	for _, l := range strings.Split(n.Log(), "\n") {
		if strings.Contains(l, "ertificate") || strings.Contains(l, "internal call refused") {
			out = append(out, l)
			if len(out) >= max {
				break
			}
		}
	}
	return strings.Join(out, "\n")
}

// A server whose certificates were revoked cannot ask the cluster to let it leave
// (the leader refuses its certificate); with force it resets itself, as for any
// cluster that cannot be reached, and without force it is told why not.
func TestRevokedServerLeavesWithForceOnly(t *testing.T) {
	c, _ := formManually(t, 3, nil)
	leader, victim := c.Leader(), c.Nodes[2]
	body := leader.Expect(202, "POST", fmt.Sprintf(revokesPath, victim.Host), nil)
	leader.WaitTaskDone(clustertest.TaskID(body), 2*time.Minute)
	victim.Token = sessionToken(t, victim, clustertest.AdminUser, clustertest.AdminPass)
	st, out := victim.Do("POST", leaveURL, nil, nil)
	if st != 503 || out["code"] != "service_unavailable" || !strings.Contains(fmt.Sprint(out["detail"]), "no longer accepts the certificate") {
		t.Errorf("leaving without force: %d %v", st, out)
	}
	st, out = victim.Do("POST", leaveURL, map[string]any{"force": true}, nil)
	if st != 202 || out["state"] != "done" || out["id"].(float64) != 0 {
		t.Fatalf("leaving with force: %d %v", st, out)
	}
	waitUninitialized(t, victim, 90*time.Second)
	// The cluster still lists it (nothing told the leader) until it is removed there.
	if got := leader.Servers()[victim.Host]; got == nil {
		t.Error("the leader forgot a server that left alone")
	}
	// Removing it from the cluster does not wait for a server that has reset itself.
	body = leader.Expect(202, "DELETE", "/servers/"+victim.Host, nil)
	leader.WaitTaskDone(clustertest.TaskID(body), 2*time.Minute)
	if got := leader.Servers()[victim.Host]; got != nil {
		t.Errorf("the removed server is still listed: %v", got)
	}
}
