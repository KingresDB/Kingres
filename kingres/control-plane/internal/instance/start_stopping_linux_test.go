// SPDX-License-Identifier: AGPL-3.0-only

//go:build linux

package instance_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/instance"
)

// stoppingFixture is a manager whose fake pg_ctl reports "running" for as long as postmaster.pid exists and whose start writes a pid file with the
// status "ready" and leaves a witness file behind. The pid file of the instance says "stopping": the postmaster is shutting down.
func stoppingFixture(t *testing.T, stopTO time.Duration) (m *instance.Manager, data string) {
	t.Helper()
	root, bin := t.TempDir(), t.TempDir()
	script := `#!/bin/sh
cmd=$1; shift
D=""
while [ $# -gt 0 ]; do if [ "$1" = "-D" ]; then D=$2; fi; shift; done
pid="$D/postmaster.pid"
case "$cmd" in
status) [ -f "$pid" ] && exit 0; exit 3 ;;
start) printf '1\n/d\n0\n5432\n/s\n*\n1\nready\n' > "$pid"; echo started > "$D/start-witness"; exit 0 ;;
stop) rm -f "$pid"; exit 0 ;;
esac
exit 0
`
	for _, p := range []string{"pg_ctl", "initdb"} {
		if err := os.WriteFile(filepath.Join(bin, p), []byte(script), 0o755); err != nil { //nolint:gosec // a test script
			t.Fatal(err)
		}
	}
	m, err := instance.New(instance.Options{Root: root, BinDir: bin, StopTimeout: stopTO, StartTimeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	dir, _ := m.Dir(1)
	data, _ = m.DataDir(1)
	if err := os.MkdirAll(data, 0o700); err != nil {
		t.Fatal(err)
	}
	mk, _ := json.Marshal(instance.Marker{NodeID: 1, Port: 5432, CreatedAt: time.Now(), Magic: "kingresd-instance-v1"})
	if err := os.WriteFile(filepath.Join(dir, instance.MarkerName), mk, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data, "postmaster.pid"), []byte("1\n/d\n0\n5432\n/s\n*\n1\nstopping\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return m, data
}

// A start that finds the postmaster shutting down (pg_ctl status still says running) waits for the shutdown to end and then starts the instance: it
// does not report success for an instance that goes down.
func TestAStartWaitsForAPostmasterThatIsShuttingDownAndThenStartsIt(t *testing.T) {
	m, data := stoppingFixture(t, 10*time.Second)
	go func() {
		time.Sleep(700 * time.Millisecond)
		_ = os.Remove(filepath.Join(data, "postmaster.pid")) // the shutdown ends
	}()
	if err := m.Start(context.Background(), 1); err != nil {
		t.Fatalf("start: %v", err)
	}
	if _, err := os.Stat(filepath.Join(data, "start-witness")); err != nil {
		t.Fatal("the instance was not started: the start took a postmaster that was shutting down for a running one")
	}
	b, _ := os.ReadFile(filepath.Join(data, "postmaster.pid"))
	if !strings.HasSuffix(strings.TrimSpace(string(b)), "ready") {
		t.Fatalf("the pid file after the start: %q", b)
	}
}

// A shutdown that does not end within the bound is an error, never a success, and nothing is started over it.
func TestAStartThatCannotOutwaitTheShutdownIsAnError(t *testing.T) {
	m, data := stoppingFixture(t, time.Second)
	begin := time.Now()
	if err := m.Start(context.Background(), 1); err == nil {
		t.Fatal("a start reported success for a postmaster that stays in its shutdown")
	}
	if took := time.Since(begin); took > 5*time.Second {
		t.Fatalf("the wait took %s, want the bound of the stop (1 s)", took)
	}
	if _, err := os.Stat(filepath.Join(data, "start-witness")); err == nil {
		t.Fatal("pg_ctl start was run over a postmaster that was shutting down")
	}
}

// A postmaster that is up (status ready) is found running, as before: nothing is waited for and nothing is started.
func TestAStartOfARunningPostmasterIsStillANoOp(t *testing.T) {
	m, data := stoppingFixture(t, time.Second)
	if err := os.WriteFile(filepath.Join(data, "postmaster.pid"), []byte("1\n/d\n0\n5432\n/s\n*\n1\nready\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := m.Start(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(data, "start-witness")); err == nil {
		t.Fatal("a running instance was started again")
	}
}
