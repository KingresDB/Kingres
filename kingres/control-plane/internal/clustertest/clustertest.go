// SPDX-License-Identifier: AGPL-3.0-only

// Package clustertest starts a cluster of several kingresd processes on one
// machine for integration tests: every server has its own data directory, REST
// and internal API ports, PostgreSQL port and server-host (a distinct loopback
// address, 127.0.0.2, 127.0.0.3, ...), so certificate names, peer addresses and
// the sharding extension's host/port pairs are distinct and real. Later
// increments (rebalance, failover) use it too.
//
// It needs KINGRESD_BIN (a kingresd built for the machine) and KINGRES_PG_BIN
// (the KingresDB installation); without them Available is false and tests skip.
// Everything is driven through the HTTP API, as an operator would.
package clustertest

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/pgtest"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/pki"
)

// Test credentials of the first administrator.
const (
	AdminUser = "root_admin"
	AdminPass = "Correct-Horse-Battery-9!"
)

// Available reports whether the environment can run a cluster of processes.
func Available() bool { return pgtest.Available() && os.Getenv("KINGRESD_BIN") != "" }

// Node is one kingresd process with its data directory.
type Node struct {
	t       testing.TB
	Index   int
	Host    string // server id: 127.0.0.<Index+2>
	APIPort int
	// IntPort is the internal API port (APIPort+1).
	IntPort int
	PGPort  int
	Dir     string
	DataDir string
	LogFile string
	// Extra are further command-line flags of kingresd (a test that needs a
	// setting, for example a short certificate lifetime); they apply from the next Start.
	Extra []string
	// ForceEnforce makes the node enforce lease self-fencing for every group (set
	// before SetFaults; see fault.Spec).
	ForceEnforce bool

	mu     sync.Mutex
	cmd    *exec.Cmd
	exited chan struct{}
	// Token is the bearer token used by Do.
	Token  string
	client *http.Client
}

// Cluster is a set of nodes.
type Cluster struct {
	T      testing.TB
	Root   string
	Nodes  []*Node
	retain bool
}

// Retain keeps the cluster running after the test that made it ends (the
// caller then calls Destroy, for example from TestMain).
func (c *Cluster) Retain() { c.retain = true }

// Use makes t the test that the nodes report failures to.
func (c *Cluster) Use(t testing.TB) {
	c.T = t
	for _, n := range c.Nodes {
		n.t = t
	}
}

// Destroy stops everything and removes the directories.
func (c *Cluster) Destroy() { c.retain = false; c.Close() }

// New prepares n nodes (nothing is started).
func New(t testing.TB, n int) *Cluster {
	t.Helper()
	if !Available() {
		t.Skip("KINGRESD_BIN and KINGRES_PG_BIN are not set; skipping the multi-server test")
	}
	root, err := os.MkdirTemp(os.Getenv("KINGRES_TEST_ROOT"), "kgcl-")
	if err != nil {
		t.Fatal(err)
	}
	c := &Cluster{T: t, Root: root}
	for i := 0; i < n; i++ {
		dir := filepath.Join(root, "n"+strconv.Itoa(i+1))
		nd := &Node{t: t, Index: i, Host: fmt.Sprintf("127.0.0.%d", i+2), APIPort: pgtest.FreePortPair(t), PGPort: pgtest.FreePort(t),
			Dir: dir, DataDir: filepath.Join(dir, "data"), LogFile: filepath.Join(dir, "kingresd.log"),
			client: &http.Client{Timeout: 3 * time.Minute, Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}} //nolint:gosec // the node under test, on loopback
		nd.IntPort = nd.APIPort + 1
		if err := os.MkdirAll(dir, 0o700); err != nil { //nolint:gosec // a directory of the test root
			t.Fatal(err)
		}
		c.Nodes = append(c.Nodes, nd)
	}
	t.Cleanup(func() {
		if !c.retain {
			c.Close()
			// The processes are stopped: the ports can be used by the next cluster.
			for _, nd := range c.Nodes {
				pgtest.ReleasePorts(nd.APIPort, nd.APIPort+1, nd.PGPort)
			}
		}
	})
	return c
}

// Close stops every process and instance and removes the directories.
func (c *Cluster) Close() {
	for _, n := range c.Nodes {
		n.Stop()
		n.StopStandbys()
		n.StopInstance()
	}
	if os.Getenv("KINGRES_TEST_KEEP") == "" {
		_ = os.RemoveAll(c.Root)
	}
}

// Leader returns the first node (the cluster is created on it).
func (c *Cluster) Leader() *Node { return c.Nodes[0] }

func (n *Node) args() []string {
	args := []string{"-listen", n.Host + ":" + strconv.Itoa(n.APIPort), "-internal-listen", n.Host + ":" + strconv.Itoa(n.IntPort),
		"-data-dir", n.DataDir, "-socket-dir", filepath.Join(n.Dir, "s"), "-log-format", "json", "-login-rate-limit", "1000",
		"-pg-bin-dir", pgtest.BinDir(), "-pg-port", strconv.Itoa(n.PGPort), "-pg-listen", "127.0.0.1," + n.Host, "-server-host", n.Host, "-fault-dir", n.FaultDir()}
	return append(args, n.Extra...)
}

// Start starts the process; it returns when the REST API answers.
func (n *Node) Start() {
	n.t.Helper()
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.cmd != nil {
		n.t.Fatalf("node %d is running", n.Index)
	}
	lf, err := os.OpenFile(n.LogFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		n.t.Fatal(err)
	}
	cmd := exec.CommandContext(context.Background(), os.Getenv("KINGRESD_BIN"), n.args()...) //nolint:gosec // the binary under test
	cmd.Stdout, cmd.Stderr = lf, lf
	setProc(cmd)
	if err := cmd.Start(); err != nil {
		n.t.Fatalf("starting kingresd: %v", err)
	}
	n.cmd, n.exited = cmd, make(chan struct{})
	go func() { _ = cmd.Wait(); _ = lf.Close(); close(n.exited) }()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if st, _ := n.Do("GET", "/healthz", nil, nil); st == 200 {
			return
		}
		select {
		case <-n.exited:
			n.t.Fatalf("kingresd of node %d exited early:\n%s", n.Index, n.LogTail(3000))
		case <-time.After(200 * time.Millisecond):
		}
	}
	n.t.Fatalf("kingresd of node %d did not start:\n%s", n.Index, n.LogTail(3000))
}

// Stop stops the process gracefully (the instance keeps running).
func (n *Node) Stop() { n.signal(false) }

// Kill stops the process abruptly (SIGKILL), as a crash would.
func (n *Node) Kill() { n.signal(true) }

func (n *Node) signal(hard bool) {
	n.mu.Lock()
	cmd, exited := n.cmd, n.exited
	n.mu.Unlock()
	if cmd == nil {
		return
	}
	terminate(cmd, hard)
	select {
	case <-exited:
	case <-time.After(60 * time.Second):
		terminate(cmd, true)
		<-exited
	}
	n.mu.Lock()
	n.cmd = nil
	n.mu.Unlock()
}

// Running reports whether the process runs.
func (n *Node) Running() bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.cmd != nil
}

// StopInstance stops the node's PostgreSQL instance (fast shutdown); it is for
// tests that make a server unreachable, and for clean-up.
func (n *Node) StopInstance() {
	data := filepath.Join(n.DataDir, "instances", "1", "pgdata")
	if _, err := os.Stat(filepath.Join(data, "postmaster.pid")); err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	_ = exec.CommandContext(ctx, filepath.Join(pgtest.BinDir(), "pg_ctl"), "stop", "-D", data, "-m", "fast", "-w", "-t", "50").Run() //nolint:gosec // fixed program
}

// StopStandbys stops the standby instances of the node (directories 100 and up of
// its instances directory, see instance.StandbyBaseID); Close calls it.
func (n *Node) StopStandbys() {
	entries, _ := os.ReadDir(filepath.Join(n.DataDir, "instances"))
	for _, e := range entries {
		if id, err := strconv.Atoi(e.Name()); err != nil || id < 100 {
			continue
		}
		data := filepath.Join(n.DataDir, "instances", e.Name(), "pgdata")
		if _, err := os.Stat(filepath.Join(data, "postmaster.pid")); err != nil {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		_ = exec.CommandContext(ctx, filepath.Join(pgtest.BinDir(), "pg_ctl"), "stop", "-D", data, "-m", "fast", "-w", "-t", "50").Run() //nolint:gosec // fixed program
		cancel()
	}
}

// LogTail returns the last max bytes of the process log without the access
// log lines (set KINGRES_TEST_VERBOSE to keep them).
func (n *Node) LogTail(max int) string {
	b, _ := os.ReadFile(n.LogFile)
	if os.Getenv("KINGRES_TEST_VERBOSE") == "" {
		var keep [][]byte
		for _, l := range bytes.Split(b, []byte{10}) {
			if !bytes.Contains(l, []byte(`"msg":"request"`)) {
				keep = append(keep, l)
			}
		}
		b = bytes.Join(keep, []byte{10})
	}
	if len(b) > max {
		b = b[len(b)-max:]
	}
	return string(b)
}

// Log returns the whole process log.
func (n *Node) Log() string { b, _ := os.ReadFile(n.LogFile); return string(b) }

// Base is the URL prefix of the REST API.
func (n *Node) Base() string { return fmt.Sprintf("http://%s:%d/api/v1", n.Host, n.APIPort) }

// InternalAddr is host:port of the internal API.
func (n *Node) InternalAddr() string { return fmt.Sprintf("%s:%d", n.Host, n.IntPort) }

// RestAddr is host:port of the REST API.
func (n *Node) RestAddr() string { return fmt.Sprintf("%s:%d", n.Host, n.APIPort) }

// Do makes a JSON request with the node's bearer token (if any). A JSON array
// answer is wrapped as {"_array": [...]}.
func (n *Node) Do(method, path string, body any, headers map[string]string) (int, map[string]any) {
	st, out, _ := n.DoH(method, path, body, headers)
	return st, out
}

// DoH is Do that also returns the response headers.
func (n *Node) DoH(method, path string, body any, headers map[string]string) (int, map[string]any, http.Header) {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequestWithContext(context.Background(), method, n.Base()+path, rd)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if n.Token != "" {
		req.Header.Set("Authorization", "Bearer "+n.Token)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := n.client.Do(req)
	if err != nil {
		return 0, map[string]any{"transportError": err.Error()}, nil
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	out := map[string]any{}
	if len(raw) > 0 {
		if raw[0] == '[' {
			var a []any
			_ = json.Unmarshal(raw, &a)
			out["_array"] = a
		} else if err := json.Unmarshal(raw, &out); err != nil {
			out["_raw"] = string(raw)
		}
	}
	return resp.StatusCode, out, resp.Header
}

// Expect makes a request and fails the test unless it answers want.
func (n *Node) Expect(want int, method, path string, body any) map[string]any {
	n.t.Helper()
	st, out := n.Do(method, path, body, nil)
	if st != want {
		n.t.Fatalf("node %d: %s %s: status %d, want %d: %v\nlog tail:\n%s", n.Index, method, path, st, want, out, n.LogTail(2500))
	}
	return out
}

// SetupToken reads the node's setup token from its data directory.
func (n *Node) SetupToken() string {
	b, err := os.ReadFile(filepath.Join(n.DataDir, "setup-token"))
	if err != nil {
		n.t.Fatalf("node %d: setup token: %v", n.Index, err)
	}
	return strings.TrimSpace(string(b))
}

// BootstrapFingerprint is the SHA-256 fingerprint (hex) of the certificate the
// node's internal API presents while it has no cluster, read from its pki
// directory (what an operator reads from the log).
func (n *Node) BootstrapFingerprint() string {
	b, err := os.ReadFile(filepath.Join(n.DataDir, "pki", pki.BootstrapCertFile))
	if err != nil {
		n.t.Fatalf("node %d: bootstrap certificate: %v", n.Index, err)
	}
	c, err := pki.ParseCertPEM(b)
	if err != nil {
		n.t.Fatal(err)
	}
	return pki.Fingerprint(c.Raw)
}

// Login signs in and keeps the bearer token.
func (n *Node) Login(user, pass string) bool {
	// A server whose group lease is not held yet starts its instance only once it has one
	// (a lease round), so the sign-in is retried while the server says it cannot serve: 503 or no answer.
	var st int
	var body map[string]any
	for end := time.Now().Add(60 * time.Second); ; time.Sleep(500 * time.Millisecond) {
		st, body = n.Do("POST", "/auth/login", map[string]any{"username": user, "password": pass, "issueToken": true}, map[string]string{"Authorization": ""})
		if (st != 503 && st != 0) || time.Now().After(end) {
			break
		}
	}
	if st != 200 {
		return false
	}
	tok, _ := body["token"].(string)
	n.Token = tok
	return tok != ""
}

// InitCluster creates a one-server cluster on the node and waits for it.
func (n *Node) InitCluster(name string) {
	n.t.Helper()
	tok := n.SetupToken()
	st, body := n.Do("POST", "/setup/init-cluster", map[string]any{
		"clusterName": name, "admin": map[string]any{"username": AdminUser, "password": AdminPass},
		"dataPaths": map[string]any{"dataDirectory": n.DataDir}, "memoryQuota": map[string]any{"totalMegabytes": 1024},
	}, map[string]string{"X-Setup-Token": tok})
	if st != http.StatusAccepted {
		n.t.Fatalf("init-cluster: %d %v", st, body)
	}
	n.WaitSetup(4 * time.Minute)
	if !n.Login(AdminUser, AdminPass) {
		n.t.Fatalf("the administrator cannot sign in on node %d", n.Index)
	}
}

// WaitSetup waits until the setupTask of GET /setup/state is done (fails when
// it failed).
func (n *Node) WaitSetup(d time.Duration) {
	n.t.Helper()
	if err := n.waitSetup(d); err != nil {
		n.t.Fatalf("node %d: %v\n%s", n.Index, err, n.LogTail(3000))
	}
}

func (n *Node) waitSetup(d time.Duration) error {
	deadline := time.Now().Add(d)
	for {
		st, body := n.Do("GET", "/setup/state", nil, map[string]string{"Authorization": ""})
		if st == 200 {
			if tk, _ := body["setupTask"].(map[string]any); tk != nil {
				switch tk["state"] {
				case "done":
					return nil
				case "failed":
					return fmt.Errorf("setup failed: %v", tk["error"])
				}
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("setup did not finish: %v", body)
		}
		time.Sleep(300 * time.Millisecond)
	}
}

// JoinFrom asks node n to join the cluster of existing (POST /setup/join-cluster
// on n). pin is the fingerprint to pin; "" asks for trust on first use, which
// the server refuses unless the request says allowUnpinned (contract 0.6.0), so
// the helper does. It returns the status and body without waiting.
func (n *Node) JoinFrom(existing *Node, pin string) (int, map[string]any) {
	h := map[string]string{"X-Setup-Token": n.SetupToken(), "Authorization": ""}
	if pin != "" {
		h["X-Kingres-Pin-Sha256"] = pin
	}
	return n.Do("POST", "/setup/join-cluster", map[string]any{
		"existingNodeAddress": existing.RestAddr(), "admin": map[string]any{"username": AdminUser, "password": AdminPass},
		"dataPaths": map[string]any{"dataDirectory": n.DataDir}, "memoryQuota": map[string]any{"totalMegabytes": 1024},
		"allowUnpinned": pin == "",
	}, h)
}

// AddServer asks the cluster (POST /servers on n) to add node other.
func (n *Node) AddServer(other *Node, token, pin string) (int, map[string]any) {
	h := map[string]string{}
	if pin != "" {
		h["X-Kingres-Pin-Sha256"] = pin
	}
	return n.Do("POST", "/servers", map[string]any{"address": other.RestAddr(), "setupToken": token, "allowUnpinned": pin == ""}, h)
}

// WaitTask polls GET /tasks/{id} until the task is done or failed.
func (n *Node) WaitTask(id any, d time.Duration) map[string]any {
	n.t.Helper()
	deadline := time.Now().Add(d)
	for {
		st, body := n.Do("GET", fmt.Sprintf("/tasks/%v", id), nil, nil)
		if st == 200 {
			switch body["state"] {
			case "done", "failed":
				return body
			}
		}
		if time.Now().After(deadline) {
			n.t.Fatalf("task %v did not finish: %d %v\n%s", id, st, body, n.LogTail(3000))
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// WaitTaskDone is WaitTask that fails unless the task is done.
func (n *Node) WaitTaskDone(id any, d time.Duration) map[string]any {
	n.t.Helper()
	b := n.WaitTask(id, d)
	if b["state"] != "done" {
		n.t.Fatalf("task %v ended %v: %v\n%s", id, b["state"], b["error"], n.LogTail(3000))
	}
	return b
}

// TaskID extracts the id of an accepted task.
func TaskID(body map[string]any) any { return body["id"] }

// Servers returns the ids of the servers listed by GET /servers.
func (n *Node) Servers() map[string]map[string]any {
	n.t.Helper()
	out := map[string]map[string]any{}
	body := n.Expect(200, "GET", "/servers", nil)
	items, _ := body["items"].([]any)
	for _, it := range items {
		m, _ := it.(map[string]any)
		if id, _ := m["id"].(string); id != "" {
			out[id] = m
		}
	}
	return out
}

// PG opens a connection to database db of this node's instance as user, over
// TCP without TLS (the managed rules allow password logins from loopback).
func (n *Node) PG(ctx context.Context, user, pass, db string) (*pgx.Conn, error) {
	cfg, err := pgx.ParseConfig("")
	if err != nil {
		return nil, err
	}
	cfg.Host, cfg.Port, cfg.Database, cfg.User, cfg.Password = "127.0.0.1", uint16(n.PGPort), db, user, pass //nolint:gosec // test ports
	cfg.TLSConfig, cfg.Fallbacks, cfg.ConnectTimeout = nil, nil, 10*time.Second
	return pgx.ConnectConfig(ctx, cfg)
}

// MustPG is PG that fails the test on error; the connection is closed at the
// end of the test.
func (n *Node) MustPG(user, pass, db string) *pgx.Conn {
	n.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	c, err := n.PG(ctx, user, pass, db)
	if err != nil {
		n.t.Fatalf("node %d: connecting to %s as %s: %v", n.Index, db, user, err)
	}
	n.t.Cleanup(func() { _ = c.Close(context.Background()) })
	return c
}

// Form builds a cluster of n nodes: the first creates it, the second joins from
// its own API, the others are added from the cluster's API. Every node is
// started and, at the end, listed by every node's GET /servers.
func Form(t testing.TB, n int) *Cluster { return FormWith(t, n, nil) }

// Dump returns the log tails of every node, for failure messages.
func (c *Cluster) Dump() string {
	var sb strings.Builder
	for _, n := range c.Nodes {
		fmt.Fprintf(&sb, "---- node %d (%s)\n%s\n", n.Index, n.Host, n.LogTail(2500))
	}
	return sb.String()
}

// ServicePG opens a connection as the instance's service role (superuser) over
// the loopback address; its password is read from the node's data directory.
// It is for diagnostics in tests, never for the operations under test.
func (n *Node) ServicePG(db string) *pgx.Conn {
	n.t.Helper()
	pw, err := os.ReadFile(filepath.Join(n.DataDir, "service-credentials"))
	if err != nil {
		n.t.Fatalf("node %d: service credentials: %v", n.Index, err)
	}
	return n.MustPG("kingresd", strings.TrimSpace(string(pw)), db)
}

// FormWith is Form with a hook that runs on the leader after the cluster was
// created and before any other server joins (to make objects that exist before
// the servers are added).
func FormWith(t testing.TB, n int, before func(leader *Node)) *Cluster {
	t.Helper()
	return FormWithExtra(t, n, nil, before)
}

// FormWithExtra is FormWith where every node starts with the extra kingresd flags
// (a test that needs short timeouts).
func FormWithExtra(t testing.TB, n int, extra []string, before func(leader *Node)) *Cluster {
	t.Helper()
	c := New(t, n)
	for _, nd := range c.Nodes {
		nd.Extra = append([]string(nil), extra...)
	}
	for _, nd := range c.Nodes {
		nd.Start()
	}
	c.Leader().InitCluster("multi")
	if before != nil {
		before(c.Leader())
	}
	c.joinRest(t, 1)
	return c
}

// joinRest joins node 1 from its own API and adds the others from the leader's.
func (c *Cluster) joinRest(t testing.TB, from int) {
	t.Helper()
	for i := from; i < len(c.Nodes); i++ {
		nd := c.Nodes[i]
		if i == 1 {
			st, body := nd.JoinFrom(c.Leader(), "")
			if st != http.StatusAccepted {
				t.Fatalf("join-cluster: %d %v\n%s", st, body, c.Dump())
			}
			if err := nd.waitSetup(8 * time.Minute); err != nil {
				t.Fatalf("%v\n%s", err, c.Dump())
			}
		} else {
			st, body := c.Leader().AddServer(nd, nd.SetupToken(), "")
			if st != http.StatusAccepted {
				t.Fatalf("add server: %d %v\n%s", st, body, c.Dump())
			}
			if b := c.Leader().WaitTask(TaskID(body), 8*time.Minute); b["state"] != "done" {
				t.Fatalf("add server task: %v\n%s", b["error"], c.Dump())
			}
		}
		if !nd.Login(AdminUser, AdminPass) {
			t.Fatalf("the administrator cannot sign in on node %d\n%s", i, c.Dump())
		}
	}
}

// StopInstances runs "kingresd -stop-instances" with the node's settings, as the
// entry point of the container does after kingresd has exited, and returns its
// error and output. The process of the node must be stopped before (Stop).
func (n *Node) StopInstances() (string, error) {
	n.t.Helper()
	cmd := exec.CommandContext(context.Background(), os.Getenv("KINGRESD_BIN"), append(n.args(), "-stop-instances")...) //nolint:gosec // the binary under test
	out, err := cmd.CombinedOutput()
	if lf, ferr := os.OpenFile(n.LogFile, os.O_APPEND|os.O_WRONLY, 0o600); ferr == nil {
		_, _ = lf.Write(out)
		_ = lf.Close()
	}
	return string(out), err
}

// StopOrderly is the orderly stop of a server as the container's entry point does
// it: kingresd gets SIGTERM and exits, and only then "kingresd -stop-instances"
// shuts the instances down with a fast shutdown.
func (n *Node) StopOrderly() {
	n.t.Helper()
	n.Stop()
	if out, err := n.StopInstances(); err != nil {
		n.t.Fatalf("kingresd -stop-instances of node %d: %v\n%s", n.Index, err, out)
	}
}

// InstanceLogs returns the PostgreSQL log files of every instance of the node (the
// primary and the standbys it runs), concatenated, with the file names.
func (n *Node) InstanceLogs() string {
	var sb strings.Builder
	files, _ := filepath.Glob(filepath.Join(n.DataDir, "instances", "*", "pgdata", "log", "*"))
	more, _ := filepath.Glob(filepath.Join(n.DataDir, "instances", "*", "pgdata", "kingres-startup.log"))
	for _, f := range append(files, more...) {
		b, _ := os.ReadFile(f)
		fmt.Fprintf(&sb, "==== %s\n%s\n", strings.TrimPrefix(f, n.DataDir), b)
	}
	return sb.String()
}
