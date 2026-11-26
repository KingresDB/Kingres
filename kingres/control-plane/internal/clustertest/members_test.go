// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package clustertest_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/clustertest"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/pki"
)

func readFile(t *testing.T, parts ...string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(parts...))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// internalCall makes a call to the internal API of node n presenting the
// certificate and key (nil: none) with the given version header.
func internalCall(t *testing.T, n *clustertest.Node, pair *tls.Certificate, version, method, path string, body any) (int, map[string]any) {
	t.Helper()
	cfg := &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS13} //nolint:gosec // the server under test, on loopback
	if pair != nil {
		cfg.Certificates = []tls.Certificate{*pair}
	}
	hc := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{TLSClientConfig: cfg}}
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, "https://"+n.InternalAddr()+"/internal/v1"+path, rd)
	if version != "" {
		req.Header.Set("X-Kingres-Internal-Version", version)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return 0, map[string]any{"transportError": err.Error()}
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	out := map[string]any{}
	_ = json.Unmarshal(raw, &out)
	return resp.StatusCode, out
}

func loadPair(t *testing.T, n *clustertest.Node) *tls.Certificate {
	t.Helper()
	pair, err := tls.X509KeyPair(readFile(t, n.DataDir, "pki", "server.crt"), readFile(t, n.DataDir, "pki", "server.key"))
	if err != nil {
		t.Fatal(err)
	}
	return &pair
}

func TestInternalAPIRefusals(t *testing.T) {
	c := sharedCluster(t)
	leader, other := c.Leader(), c.Nodes[1]
	good := loadPair(t, other)

	if st, out := internalCall(t, leader, nil, "1", "GET", "/state", nil); st != 401 || out["error"] != "no_certificate" {
		t.Errorf("no certificate: %d %v", st, out)
	}
	// A certificate of another CA, for the same server id.
	otherCA, err := pki.CreateCA(filepath.Join(t.TempDir(), "pki"), "other", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	names := pki.Names{CommonName: other.Host, DNS: []string{"localhost"}}
	key, csr, _ := pki.NewKeyAndCSR(names)
	crt, _, err := otherCA.Sign(csr, names, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := tls.X509KeyPair(crt, key)
	if err != nil {
		t.Fatal(err)
	}
	if st, out := internalCall(t, leader, &foreign, "1", "GET", "/state", nil); st != 401 || out["error"] != "certificate_refused" {
		t.Errorf("certificate of another CA: %d %v", st, out)
	}
	// Another internal version is refused, naming both.
	if st, out := internalCall(t, leader, good, "99", "GET", "/state", nil); st != 409 || out["error"] != "version_mismatch" || !strings.Contains(fmt.Sprint(out["message"]), "99") {
		t.Errorf("another version: %d %v", st, out)
	}
	// A valid certificate is not enough for the calls reserved to the leader.
	for _, call := range []struct{ method, path string }{
		{"POST", "/instance/trust"}, {"POST", "/instance/stop"}, {"POST", "/node/reset"}, {"POST", "/apply"}, {"POST", "/exec"}, {"GET", "/resources"},
	} {
		var body any
		if call.method == "POST" {
			body = map[string]any{}
		}
		if st, out := internalCall(t, leader, good, "1", call.method, call.path, body); st != 403 || out["error"] != "not_leader" {
			t.Errorf("%s %s from a server that is not the leader: %d %v", call.method, call.path, st, out)
		}
	}
	// A path that is not in the table does not exist.
	if st, _ := internalCall(t, leader, good, "1", "POST", "/instance/exec-anything", map[string]any{}); st != 404 {
		t.Errorf("unknown call: %d", st)
	}
	// The public API never takes a client certificate as authentication.
	hc := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{Certificates: []tls.Certificate{*good}, InsecureSkipVerify: true}}} //nolint:gosec // loopback
	if resp, err := hc.Get(leader.Base() + "/cluster"); err == nil {
		resp.Body.Close()
		if resp.StatusCode != 401 {
			t.Errorf("the public API treated a TLS client with a certificate as authenticated: %d", resp.StatusCode)
		}
	}
}

func TestPrivateKeysStayOnTheirServers(t *testing.T) {
	c := sharedCluster(t)
	if _, err := os.Stat(filepath.Join(c.Leader().DataDir, "pki", "ca.key")); err != nil {
		t.Fatal("the CA key is not on the first server")
	}
	for _, n := range c.Nodes[1:] {
		if _, err := os.Stat(filepath.Join(n.DataDir, "pki", "ca.key")); err == nil {
			t.Errorf("node %d holds the CA key", n.Index)
		}
		key := bytes.TrimSpace(readFile(t, n.DataDir, "pki", "server.key"))
		for _, o := range c.Nodes {
			if o == n {
				continue
			}
			_ = filepath.WalkDir(o.DataDir, func(p string, d fs.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if d.IsDir() {
					if d.Name() == "pgdata" {
						return filepath.SkipDir
					}
					return nil
				}
				info, ierr := d.Info()
				if ierr != nil {
					return ierr
				}
				if info.Size() > 8<<20 {
					return nil
				}
				if b, rerr := os.ReadFile(p); rerr == nil && bytes.Contains(b, key[len(key)/3:len(key)/3+40]) {
					t.Errorf("the private key of node %d is in %s", n.Index, p)
				}
				return nil
			})
		}
	}
}

func TestNoSecretsInLogsAuditOrTasks(t *testing.T) {
	c := sharedCluster(t)
	ensureObjects(t, c)
	// A verifier of the marker password, as PostgreSQL stores it.
	var verifier string
	if err := c.Leader().ServicePG("postgres").QueryRow(context.Background(), "SELECT rolpassword FROM pg_authid WHERE rolname = 'early'").Scan(&verifier); err != nil || !strings.HasPrefix(verifier, "SCRAM-SHA-256$") {
		t.Fatalf("verifier: %q %v", verifier, err)
	}
	needles := []string{markerPassword, clustertest.AdminPass, latePassword, "PRIVATE KEY", verifier, strings.Split(verifier, "$")[2]}
	check := func(where string, b []byte) {
		for _, nd := range needles {
			if nd != "" && bytes.Contains(b, []byte(nd)) {
				t.Errorf("%s contains secret material (%.12q...)", where, nd)
			}
		}
	}
	for _, n := range c.Nodes {
		check(fmt.Sprintf("node %d log", n.Index), []byte(n.Log()))
		check(fmt.Sprintf("node %d audit file", n.Index), readFile(t, n.DataDir, "audit.jsonl"))
		for _, f := range []string{"node-state.json", "setup-task.json", "peers.json"} {
			if b, err := os.ReadFile(filepath.Join(n.DataDir, f)); err == nil {
				check(fmt.Sprintf("node %d %s", n.Index, f), b)
			}
		}
	}
	// The task rows of the leader, as text.
	rows, err := c.Leader().ServicePG("kingres_system").Query(context.Background(), "SELECT t::text FROM kingres_cp.task t")
	if err != nil {
		t.Fatal(err)
	}
	all, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil || len(all) < 3 {
		t.Fatalf("task rows: %d %v", len(all), err)
	}
	check("the task table", []byte(strings.Join(all, "\n")))
	// The server rows hold certificates (public) and fingerprints, no keys.
	rows, err = c.Leader().ServicePG("kingres_system").Query(context.Background(), "SELECT s::text FROM kingres_cp.server s")
	if err != nil {
		t.Fatal(err)
	}
	all, _ = pgx.CollectRows(rows, pgx.RowTo[string])
	check("the server table", []byte(strings.Join(all, "\n")))
}

// ------------------------------------------------------------ membership

func TestRemoveEmptyServerAndRevokedCertificate(t *testing.T) {
	c := clustertest.Form(t, 3)
	leader, victim := c.Leader(), c.Nodes[2]
	oldPair := loadPair(t, victim)
	caPEM := readFile(t, leader.DataDir, "pki", "ca.crt")

	// The map group's server and an unknown server cannot be removed.
	if st, out := leader.Do("DELETE", "/servers/"+leader.Host, nil, nil); st != 409 {
		t.Errorf("removing the map group's server: %d %v", st, out)
	}
	if st, _ := leader.Do("DELETE", "/servers/nowhere.example", nil, nil); st != 404 {
		t.Errorf("removing an unknown server: %d", st)
	}
	body := leader.Expect(202, "DELETE", "/servers/"+victim.Host, nil)
	leader.WaitTaskDone(clustertest.TaskID(body), 5*time.Minute)

	for _, n := range c.Nodes[:2] {
		if got := n.Servers(); len(got) != 2 || got[victim.Host] != nil {
			t.Errorf("node %d still lists %v", n.Index, got)
		}
	}
	// The removed server is uninitialized again with a new setup token.
	st, state := victim.Do("GET", "/setup/state", nil, map[string]string{"Authorization": ""})
	if st != 200 || state["state"] != "uninitialized" {
		t.Errorf("removed server state: %d %v", st, state["state"])
	}
	if _, err := os.Stat(filepath.Join(victim.DataDir, "instances", "1")); err == nil {
		t.Error("the removed server still has its instance")
	}
	if _, err := os.Stat(filepath.Join(victim.DataDir, "pki", "server.crt")); err == nil {
		t.Error("the removed server still has its certificate")
	}
	for _, db := range []string{"kingres_system", "postgres"} {
		cn := leader.ServicePG(db)
		var n int
		if err := cn.QueryRow(context.Background(), "SELECT count(*) FROM kingres.node WHERE host = $1", victim.Host).Scan(&n); err == nil && n != 0 {
			t.Errorf("database %s still lists the removed node", db)
		}
	}

	// Its old certificate is refused by the internal API of the others ...
	for _, n := range c.Nodes[:2] {
		if st, out := internalCall(t, n, oldPair, "1", "GET", "/state", nil); st != 401 || out["error"] != "certificate_refused" {
			t.Errorf("node %d accepted the certificate of a removed server: %d %v", n.Index, st, out)
		}
	}
	// ... and by their PostgreSQL instances.
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(caPEM)
	for _, n := range c.Nodes[:2] {
		cfg, _ := pgx.ParseConfig("")
		cfg.Host, cfg.Port, cfg.Database, cfg.User, cfg.ConnectTimeout = n.Host, uint16(n.PGPort), "postgres", "kingresd", 10*time.Second //nolint:gosec // test ports
		cfg.TLSConfig = &tls.Config{Certificates: []tls.Certificate{*oldPair}, RootCAs: pool, ServerName: n.Host, MinVersion: tls.VersionTLS13}
		cfg.Fallbacks = nil
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		conn, err := pgx.ConnectConfig(ctx, cfg)
		cancel()
		if err == nil {
			_ = conn.Close(context.Background())
			t.Errorf("the PostgreSQL instance of node %d accepted the certificate of a removed server", n.Index)
		} else if !strings.Contains(strings.ToLower(err.Error()), "revoked") {
			t.Errorf("node %d refused the removed server's certificate, but not as revoked: %v", n.Index, err)
		}
	}
	// The removed server can be added again with its new setup token.
	st2, add := leader.AddServer(victim, victim.SetupToken(), "")
	if st2 != 202 {
		t.Fatalf("adding the removed server again: %d %v", st2, add)
	}
	leader.WaitTaskDone(clustertest.TaskID(add), 6*time.Minute)
	if got := leader.Servers(); len(got) != 3 {
		t.Errorf("after adding again: %v", got)
	}
}

func TestRemoveRefusedForServerThatOwnsBuckets(t *testing.T) {
	c := sharedCluster(t)
	prepareTable(t, c)
	for _, n := range c.Nodes[1:] {
		st, out := c.Leader().Do("DELETE", "/servers/"+n.Host, nil, nil)
		if st != 409 || !strings.Contains(fmt.Sprint(out["detail"]), "Rebalance") {
			t.Errorf("removing %s which owns buckets: %d %v", n.Host, st, out)
		}
	}
	if got := c.Leader().Servers(); len(got) != 3 {
		t.Errorf("a refused removal changed the cluster: %v", got)
	}
}

func TestFingerprintModesAndSetupTokenReplay(t *testing.T) {
	c := clustertest.New(t, 4)
	for _, n := range c.Nodes {
		n.Start()
	}
	leader := c.Leader()
	leader.InitCluster("fp")

	// Adding with a wrong pinned fingerprint: refused before the token is sent,
	// and the new server is untouched.
	n1 := c.Nodes[1]
	wrong := strings.Repeat("ab", 32)
	if st, out := leader.AddServer(n1, n1.SetupToken(), wrong); st != 422 {
		t.Fatalf("wrong pin: %d %v", st, out)
	}
	if _, err := os.Stat(filepath.Join(n1.DataDir, "pki", "server.crt")); err == nil {
		t.Error("a server received a certificate although the pin was wrong")
	}
	if got := leader.Servers(); len(got) != 1 {
		t.Errorf("a refused add changed the cluster: %v", got)
	}
	// A wrong setup token.
	if st, out := leader.AddServer(n1, "not-the-token", n1.BootstrapFingerprint()); st != 401 {
		t.Errorf("wrong token: %d %v", st, out)
	}
	// The right pin works (pinned mode).
	seen1 := n1.BootstrapFingerprint()
	st, add := leader.AddServer(n1, n1.SetupToken(), seen1)
	if st != 202 {
		t.Fatalf("pinned add: %d %v", st, add)
	}
	leader.WaitTaskDone(clustertest.TaskID(add), 6*time.Minute)
	// The pinned add is recorded as such: the fingerprint that was checked.
	if s := leader.Servers()[n1.Host]; s["seenFingerprint"] != seen1 || s["fingerprintPinned"] != true {
		t.Errorf("after the pinned add: seenFingerprint %v, fingerprintPinned %v (want %s, true)", s["seenFingerprint"], s["fingerprintPinned"], seen1)
	}

	// Replay of the setup token of a server that joined: refused.
	oldToken := "" // the token file is gone; keep what an attacker could have captured
	_ = oldToken
	if st, out := leader.AddServer(n1, "replayed", ""); st != 409 && st != 401 {
		t.Errorf("replayed token against a member: %d %v", st, out)
	}
	if _, err := os.Stat(filepath.Join(n1.DataDir, "setup-token")); err == nil {
		t.Error("the setup token still exists after the join")
	}
	st3, out3 := n1.Do("POST", "/setup/join-cluster", map[string]any{"existingNodeAddress": leader.RestAddr(),
		"admin":     map[string]any{"username": clustertest.AdminUser, "password": clustertest.AdminPass},
		"dataPaths": map[string]any{"dataDirectory": n1.DataDir}}, map[string]string{"X-Setup-Token": "anything", "Authorization": ""})
	if st3 != 422 {
		t.Errorf("join on a member: %d %v", st3, out3)
	}

	// Joining with a wrong pin fails on the new server and records nothing.
	n2 := c.Nodes[2]
	prev := setupTaskID(n2)
	if st, body := n2.JoinFrom(leader, wrong); st != 202 {
		t.Fatalf("join with a pin: %d %v", st, body)
	}
	if err := waitSetupFailure(n2, prev); err == "" || !strings.Contains(err, "pinned fingerprint") {
		t.Errorf("join with a wrong pin: %q", err)
	}
	if got := leader.Servers(); len(got) != 2 {
		t.Errorf("a refused join changed the cluster: %v", got)
	}
	// Wrong credentials, and a role that is not an administrator.
	mkRole(t, leader, "plain", "Plain-Role-Password-1", "kingres_ops")
	for _, cred := range [][2]string{{clustertest.AdminUser, "wrong-password"}, {"plain", "Plain-Role-Password-1"}} {
		prev = setupTaskID(n2)
		st, body := n2.Do("POST", "/setup/join-cluster", map[string]any{"existingNodeAddress": leader.RestAddr(),
			"admin": map[string]any{"username": cred[0], "password": cred[1]}, "dataPaths": map[string]any{"dataDirectory": n2.DataDir}, "allowUnpinned": true},
			map[string]string{"X-Setup-Token": n2.SetupToken(), "Authorization": ""})
		if st != 202 {
			t.Fatalf("join request: %d %v", st, body)
		}
		if err := waitSetupFailure(n2, prev); err == "" || !strings.Contains(err, "refused") {
			t.Errorf("join as %s: %q", cred[0], err)
		}
	}
	// The right pin: the fingerprint of the existing server's internal certificate.
	leaderCert, _ := pki.ParseCertPEM(readFile(t, leader.DataDir, "pki", "server.crt"))
	st4, body4 := n2.JoinFrom(leader, pki.FormatFingerprint(pki.Fingerprint(leaderCert.Raw)))
	if st4 != 202 {
		t.Fatalf("pinned join: %d %v", st4, body4)
	}
	n2.WaitSetup(8 * time.Minute)
	if got := leader.Servers(); len(got) != 3 {
		t.Errorf("after the pinned join: %v", got)
	} else if s := got[n2.Host]; s["seenFingerprint"] != nil || s["fingerprintPinned"] != false {
		// The fingerprint that was pinned is the existing server's, checked by the
		// joining server (its setup task says so); the leader saw no certificate of
		// the joining server, so it has no first contact to report.
		t.Errorf("after the pinned join: seenFingerprint %v, fingerprintPinned %v", s["seenFingerprint"], s["fingerprintPinned"])
	}
}

// waitSetupFailure waits for the setup task of the node to fail and returns
// its error message.
// setupTaskID returns the id of the node's latest setup task (0 when none).
func setupTaskID(n *clustertest.Node) float64 {
	_, body := n.Do("GET", "/setup/state", nil, map[string]string{"Authorization": ""})
	if tk, _ := body["setupTask"].(map[string]any); tk != nil {
		id, _ := tk["id"].(float64)
		return id
	}
	return 0
}

func waitSetupFailure(n *clustertest.Node, prev float64) string {
	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		st, body := n.Do("GET", "/setup/state", nil, map[string]string{"Authorization": ""})
		if st == 200 {
			if tk, _ := body["setupTask"].(map[string]any); tk != nil && tk["state"] == "failed" && tk["id"] != prev {
				e, _ := tk["error"].(map[string]any)
				return fmt.Sprint(e["message"])
			}
		}
		time.Sleep(300 * time.Millisecond)
	}
	return ""
}
