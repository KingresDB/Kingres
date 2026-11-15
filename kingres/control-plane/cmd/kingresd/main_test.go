// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/testutil"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/config"
)

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func noEnv(string) (string, bool) { return "", false }

// freeAddr returns a loopback address that was free a moment ago.
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

// TestServerLifecycle starts the real server (without a PostgreSQL instance:
// the node is uninitialized), checks the endpoints of the smoke test, and
// shuts it down gracefully.
func TestServerLifecycle(t *testing.T) {
	dir := testutil.TempDir(t)
	logs := &syncBuffer{}
	ready := make(chan string, 1)
	ctx, cancel := context.WithCancel(context.Background())
	args := []string{
		"-listen", freeAddr(t), "-data-dir", dir, "-log-format", "json",
		"-pg-dsn", "host=127.0.0.1 port=1 dbname=postgres user=nobody password=dsn-secret-xyz connect_timeout=1",
	}
	done := make(chan error, 1)
	go func() { done <- run(ctx, args, noEnv, logs, ready) }()

	var addr string
	select {
	case addr = <-ready:
	case err := <-done:
		t.Fatalf("server exited early: %v\n%s", err, logs.String())
	case <-time.After(20 * time.Second):
		t.Fatal("server did not start")
	}
	get := func(path string) (int, map[string]any) {
		t.Helper()
		resp, err := http.Get("http://" + addr + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var m map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&m)
		return resp.StatusCode, m
	}
	if code, m := get("/api/v1/healthz"); code != 200 || m["status"] != "ok" {
		t.Errorf("healthz: %d %v", code, m)
	}
	if code, m := get("/api/v1/readyz"); code != 200 || m["nodeState"] != "uninitialized" {
		t.Errorf("readyz: %d %v", code, m)
	}
	code, m := get("/api/v1/setup/state")
	if code != 200 || m["state"] != "uninitialized" || m["setupTokenRequired"] != true || m["localhostOnly"] != true {
		t.Errorf("setup/state: %d %v", code, m)
	}
	if code, m := get("/api/v1/cluster"); code != 503 || m["code"] != "node_uninitialized" {
		t.Errorf("cluster: %d %v", code, m)
	}
	resp, err := http.Get("http://" + addr + "/databases/shop")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") {
		t.Errorf("panel fallback: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}

	// The setup token exists on disk and is never logged.
	tok, err := os.ReadFile(filepath.Join(dir, "setup-token"))
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("run returned %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("server did not shut down")
	}
	out := logs.String()
	if strings.Contains(out, strings.TrimSpace(string(tok))) {
		t.Error("the setup token was logged")
	}
	if strings.Contains(out, "dsn-secret-xyz") {
		t.Error("the DSN password was logged")
	}
	for _, want := range []string{`"msg":"starting kingresd"`, `"msg":"listening"`, `"msg":"stopped"`, `"msg":"request"`} {
		if !strings.Contains(out, want) {
			t.Errorf("log lacks %s", want)
		}
	}
}

func TestRunRejectsBadConfiguration(t *testing.T) {
	err := run(context.Background(), []string{"-log-level", "loud"}, noEnv, &bytes.Buffer{}, nil)
	if err == nil || !strings.Contains(err.Error(), "log-level") {
		t.Errorf("err = %v", err)
	}
	if err := run(context.Background(), []string{"-help"}, noEnv, &bytes.Buffer{}, nil); !errors.Is(err, config.ErrHelp) {
		t.Errorf("-help: %v", err)
	}
}

func TestRunFailsWhenAddressIsTaken(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	err = run(context.Background(), []string{"-listen", ln.Addr().String(), "-data-dir", testutil.TempDir(t)}, noEnv, &bytes.Buffer{}, nil)
	if err == nil || !strings.Contains(err.Error(), "listening on") {
		t.Errorf("err = %v", err)
	}
}
