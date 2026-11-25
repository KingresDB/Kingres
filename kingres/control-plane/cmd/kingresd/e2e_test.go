// SPDX-License-Identifier: AGPL-3.0-only

package main

// End-to-end tests: a real kingresd (the run function) manages a real
// PostgreSQL instance built from KINGRES_PG_BIN. They are skipped without it
// and run inside WSL (scripts/run-integration.ps1). One cluster is created
// through the HTTP API once per test binary and shared by the tests; tests
// that need an isolated cluster call startCluster themselves.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/instance"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/pgtest"
)

const (
	adminUser = "root_admin"
	adminPass = "Correct-Horse-Battery-9!"
	// markerPassword is used in negative tests to prove that passwords never
	// reach logs, the audit file or the task table.
	markerPassword = "MARKER-pw-8c1f2e7a-do-not-leak"
)

// cp is a running kingresd with its cluster.
type cp struct {
	base    string
	dataDir string
	pgPort  int
	logs    *syncBuffer
	cancel  context.CancelFunc
	done    chan error
	root    string
	token   string // bearer token of the administrator
	client  *http.Client
	args    []string
	// internal is the address of the internal API listener.
	internal string
}

var (
	sharedMu  sync.Mutex
	sharedCP  *cp
	sharedTLS *cp
	allEnvs   []*cp
	allEnvsMu sync.Mutex
)

func TestMain(m *testing.M) {
	code := m.Run()
	allEnvsMu.Lock()
	for _, e := range allEnvs {
		e.shutdown()
	}
	allEnvsMu.Unlock()
	os.Exit(code)
}

// newServer starts kingresd (uninitialized) with a fresh data directory.
func newServer(t testing.TB) *cp { return newServerWith(t, false) }

// newServerWith starts kingresd; with tls it serves HTTPS with a certificate made for the test (the data transfer refuses plain HTTP) and the
// client of the returned cp trusts it.
func newServerWith(t testing.TB, useTLS bool) *cp {
	t.Helper()
	if !pgtest.Available() {
		t.Skip("KINGRES_PG_BIN is not set; skipping the end-to-end test")
	}
	root, err := os.MkdirTemp(os.Getenv("KINGRES_TEST_ROOT"), "kgcp-")
	if err != nil {
		t.Fatal(err)
	}
	e := &cp{root: root, dataDir: filepath.Join(root, "data"), pgPort: pgtest.FreePort(t), logs: &syncBuffer{},
		done: make(chan error, 1), client: &http.Client{Timeout: 3 * time.Minute}}
	ready := make(chan string, 1)
	ctx, cancel := context.WithCancel(context.Background())
	e.cancel = cancel
	e.internal = fmt.Sprintf("127.0.0.1:%d", pgtest.FreePort(t))
	args := []string{"-listen", "127.0.0.1:0", "-data-dir", e.dataDir, "-log-format", "json",
		"-login-rate-limit", "1000", "-internal-listen", e.internal, "-pg-bin-dir", pgtest.BinDir(), "-pg-port", fmt.Sprint(e.pgPort), "-server-host", "127.0.0.1"}
	if findWatchdog(pgtest.BinDir()) == "" {
		// a development machine without the program built next to the test binary
		args = append(args, "-allow-no-watchdog")
	}
	if useTLS {
		crt, key := writeSelfSigned(t, root)
		// The cluster that serves HTTPS is the one of the data transfer tests: a cap of 24 MiB on an import keeps the limit tests quick and leaves room for a record of 15 MiB.
		args = append(args, "-tls-cert-file", crt, "-tls-key-file", key, "-max-atomic-bytes", "25165824")
		e.client = tlsClient()
	}
	e.args = args
	go func() { e.done <- run(ctx, args, noEnv, e.logs, ready) }()
	select {
	case addr := <-ready:
		e.base = "http://" + addr + "/api/v1"
		if useTLS {
			e.base = "https://" + addr + "/api/v1"
		}
	case err := <-e.done:
		t.Fatalf("kingresd exited early: %v\n%s", err, e.logs.String())
	case <-time.After(30 * time.Second):
		t.Fatal("kingresd did not start")
	}
	allEnvsMu.Lock()
	allEnvs = append(allEnvs, e)
	allEnvsMu.Unlock()
	return e
}

// shutdown stops kingresd and its instance and removes the directories.
func (e *cp) shutdown() {
	e.cancel()
	select {
	case <-e.done:
	case <-time.After(30 * time.Second):
	}
	if mgr, err := instance.New(instance.Options{Root: filepath.Join(e.dataDir, "instances"), BinDir: pgtest.BinDir()}); err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		_ = mgr.Stop(ctx, 1)
		cancel()
	}
	_ = os.RemoveAll(e.root)
}

// startCluster creates a new one-server cluster through the API only.
func startCluster(t testing.TB) *cp { return startClusterWith(t, false) }

func startClusterWith(t testing.TB, useTLS bool) *cp {
	t.Helper()
	e := newServerWith(t, useTLS)
	tok, err := os.ReadFile(filepath.Join(e.dataDir, "setup-token"))
	if err != nil {
		t.Fatalf("setup token: %v", err)
	}
	e.token = strings.TrimSpace(string(tok))
	req := map[string]any{
		"clusterName": "test-cluster",
		"admin":       map[string]any{"username": adminUser, "password": adminPass},
		"dataPaths":   map[string]any{"dataDirectory": e.dataDir},
		"memoryQuota": map[string]any{"totalMegabytes": 1024},
	}
	st, body, _ := e.call("POST", "/setup/init-cluster", req, map[string]string{"X-Setup-Token": e.token})
	if st != http.StatusAccepted {
		t.Fatalf("init-cluster: %d %v", st, body)
	}
	deadline := time.Now().Add(4 * time.Minute)
	for {
		st, body, _ := e.call("GET", "/setup/state", nil, nil)
		if st == 200 {
			if tk, _ := body["setupTask"].(map[string]any); tk != nil {
				switch tk["state"] {
				case "done":
					goto done
				case "failed":
					t.Fatalf("setup failed: %v\n%s", tk["error"], e.logs.String())
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("setup did not finish: %v\n%s", body, e.logs.String())
		}
		time.Sleep(300 * time.Millisecond)
	}
done:
	e.token = ""
	e.login(t, adminUser, adminPass)
	return e
}

// shared returns the cluster shared by the tests of this binary.
func shared(t testing.TB) *cp {
	t.Helper()
	sharedMu.Lock()
	defer sharedMu.Unlock()
	if sharedCP == nil {
		sharedCP = startCluster(t)
	}
	return sharedCP
}

// login signs in and stores the bearer token for later calls.
func (e *cp) login(t testing.TB, user, pass string) {
	t.Helper()
	tok, ok := e.loginAs(user, pass)
	if !ok {
		t.Fatalf("login as %s failed", user)
	}
	e.token = tok
}

// loginAs returns a bearer token for the account, without changing e.
func (e *cp) loginAs(user, pass string) (string, bool) {
	st, body, _ := e.call("POST", "/auth/login", map[string]any{"username": user, "password": pass, "issueToken": true}, nil)
	if st != 200 {
		return "", false
	}
	tok, _ := body["token"].(string)
	return tok, tok != ""
}

// call performs a JSON request. headers may carry X-Setup-Token or
// Authorization; without Authorization the bearer token of e is used. The
// body is returned as a JSON object (a JSON array is wrapped as {"_array":...}).
func (e *cp) call(method, path string, body any, headers map[string]string) (int, map[string]any, http.Header) {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, e.base+path, rd)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if e.token != "" {
		req.Header.Set("Authorization", "Bearer "+e.token)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := e.client.Do(req)
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

// as returns a copy of e that sends the given token.
func (e *cp) as(token string) *cp {
	c := *e
	c.token = token
	return &c
}

func (e *cp) anon() *cp { return e.as("") }

// expect fails the test unless the call answers status.
func (e *cp) expect(t testing.TB, want int, method, path string, body any) map[string]any {
	t.Helper()
	st, out, _ := e.call(method, path, body, nil)
	if st != want {
		t.Fatalf("%s %s: status %d, want %d: %v\nserver log tail:\n%s", method, path, st, want, out, tail(e.logs.String(), 2500))
	}
	return out
}

func TestInitClusterEndToEnd(t *testing.T) {
	e := shared(t)

	// The node is clustered, the administrator can sign in, and the cluster
	// answers on every page that reads it.
	_, state, _ := e.call("GET", "/setup/state", nil, nil)
	if state["state"] != "clustered" {
		t.Errorf("state %v", state["state"])
	}
	tk, _ := state["setupTask"].(map[string]any)
	if tk == nil || tk["state"] != "done" || tk["progressPercent"] != float64(100) {
		t.Fatalf("setupTask %v", state["setupTask"])
	}
	steps, _ := tk["steps"].([]any)
	if len(steps) != 6 {
		t.Errorf("steps %v", steps)
	}
	for _, s := range steps {
		if s.(map[string]any)["state"] != "done" {
			t.Errorf("step %v", s)
		}
	}
	cl := e.expect(t, 200, "GET", "/cluster", nil)
	if cl["name"] != "test-cluster" || cl["clusterId"] == nil {
		t.Errorf("cluster %v", cl)
	}
	srv := e.expect(t, 200, "GET", "/servers", nil)
	if items, _ := srv["items"].([]any); len(items) != 1 {
		t.Errorf("servers %v", srv)
	}
	sess := e.expect(t, 200, "GET", "/auth/session", nil)
	if sess["username"] != adminUser {
		t.Errorf("session %v", sess)
	}
	// The setup task is a normal task after sign-in.
	task := e.expect(t, 200, "GET", fmt.Sprintf("/tasks/%v", tk["id"]), nil)
	if task["kind"] != "cluster.init" || task["state"] != "done" {
		t.Errorf("task %v", task)
	}

	// Setup is closed for good: the token is gone and the operation is refused.
	if _, err := os.Stat(filepath.Join(e.dataDir, "setup-token")); err == nil {
		t.Error("setup token file still exists")
	}
	st, _, _ := e.anon().call("POST", "/setup/init-cluster", map[string]any{}, map[string]string{"X-Setup-Token": "x"})
	if st != 422 {
		t.Errorf("second init-cluster: %d", st)
	}
	// A wrong password does not sign in.
	if _, ok := e.loginAs(adminUser, "wrong-password-123"); ok {
		t.Error("wrong password signed in")
	}
}

func TestInitClusterRejectsBadRequests(t *testing.T) {
	e := newServer(t)
	tok, _ := os.ReadFile(filepath.Join(e.dataDir, "setup-token"))
	h := map[string]string{"X-Setup-Token": strings.TrimSpace(string(tok))}
	good := func() map[string]any {
		return map[string]any{
			"clusterName": "c1", "admin": map[string]any{"username": "boss", "password": markerPassword},
			"dataPaths": map[string]any{"dataDirectory": e.dataDir}, "memoryQuota": map[string]any{"totalMegabytes": 1024},
		}
	}
	mut := func(f func(m map[string]any)) map[string]any { m := good(); f(m); return m }
	cases := map[string]map[string]any{
		"bad cluster name": mut(func(m map[string]any) { m["clusterName"] = "bad name; DROP" }),
		"bad admin name": mut(func(m map[string]any) {
			m["admin"] = map[string]any{"username": `x"; DROP ROLE y; --`, "password": markerPassword}
		}),
		"reserved admin": mut(func(m map[string]any) {
			m["admin"] = map[string]any{"username": "kingresd", "password": markerPassword}
		}),
		"short password":     mut(func(m map[string]any) { m["admin"] = map[string]any{"username": "boss", "password": "short"} }),
		"other data dir":     mut(func(m map[string]any) { m["dataPaths"] = map[string]any{"dataDirectory": "/etc"} }),
		"traversal data dir": mut(func(m map[string]any) { m["dataPaths"] = map[string]any{"dataDirectory": e.dataDir + "/../../etc"} }),
		"wal dir": mut(func(m map[string]any) {
			m["dataPaths"] = map[string]any{"dataDirectory": e.dataDir, "walDirectory": "/tmp/wal"}
		}),
		"log dir": mut(func(m map[string]any) {
			m["dataPaths"] = map[string]any{"dataDirectory": e.dataDir, "logDirectory": "../x"}
		}),
		"small memory": mut(func(m map[string]any) { m["memoryQuota"] = map[string]any{"totalMegabytes": 100} }),
		"bad replicas": mut(func(m map[string]any) { m["defaultReplicaCount"] = 9 }),
	}
	for name, body := range cases {
		st, out, _ := e.call("POST", "/setup/init-cluster", body, h)
		if st != 422 && st != 400 {
			t.Errorf("%s: status %d %v", name, st, out)
		}
	}
	if st, _, _ := e.call("POST", "/setup/init-cluster", good(), nil); st != 401 {
		t.Errorf("without token: %d", st)
	}
	if st, _, _ := e.call("POST", "/setup/init-cluster", good(), map[string]string{"X-Setup-Token": "wrong"}); st != 401 {
		t.Errorf("wrong token: %d", st)
	}
	// Nothing was created by the rejected requests.
	if _, err := os.Stat(filepath.Join(e.dataDir, "instances")); err == nil {
		if ents, _ := os.ReadDir(filepath.Join(e.dataDir, "instances")); len(ents) != 0 {
			t.Errorf("rejected requests created %d instance directories", len(ents))
		}
	}
	// The marker password never reached the logs.
	if strings.Contains(e.logs.String(), markerPassword) {
		t.Error("password in the log")
	}
}

func tail(s string, n int) string {
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}

// stopManager stops kingresd only; the PostgreSQL instance keeps running.
func (e *cp) stopManager(t testing.TB) {
	t.Helper()
	e.cancel()
	select {
	case err := <-e.done:
		if err != nil {
			t.Fatalf("kingresd stopped with an error: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("kingresd did not stop")
	}
}

// startManager starts kingresd again on the same data directory.
func (e *cp) startManager(t testing.TB) {
	t.Helper()
	ready := make(chan string, 1)
	ctx, cancel := context.WithCancel(context.Background())
	e.cancel = cancel
	e.done = make(chan error, 1)
	go func() { e.done <- run(ctx, e.args, noEnv, e.logs, ready) }()
	select {
	case addr := <-ready:
		scheme := "http"
		if strings.HasPrefix(e.base, "https") {
			scheme = "https"
		}
		e.base = scheme + "://" + addr + "/api/v1"
	case err := <-e.done:
		t.Fatalf("kingresd exited early: %v\n%s", err, tail(e.logs.String(), 3000))
	case <-time.After(60 * time.Second):
		t.Fatal("kingresd did not start")
	}
}

// sharedWithTLS returns a second cluster of the test binary that serves HTTPS, for the data transfer tests.
func sharedWithTLS(t testing.TB) *cp {
	t.Helper()
	sharedMu.Lock()
	defer sharedMu.Unlock()
	if sharedTLS == nil {
		sharedTLS = startClusterWith(t, true)
	}
	return sharedTLS
}
