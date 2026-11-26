// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

//go:build linux

package instance_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/instance"
)

// fakePostmaster starts a process whose command line says postgres (a copy of sleep with that name) and returns its pid: the pid file of the fixture
// needs the pid of a live postmaster, as the check of the start wants one.
func fakePostmaster(t *testing.T) int {
	t.Helper()
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("no sleep program")
	}
	b, err := os.ReadFile(sleep)
	if err != nil {
		t.Skip("cannot read sleep")
	}
	exe := filepath.Join(t.TempDir(), "postgres")
	if err := os.WriteFile(exe, b, 0o755); err != nil { //nolint:gosec // a test program
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "120") //nolint:gosec // a test program
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot run the copy of sleep: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	return cmd.Process.Pid
}

// pidFile is a postmaster.pid with the given status line (and the lines before it: the pid, the data directory, the start time, the port, the socket
// directory, the listen addresses and the shared memory key); lines is how many of them are written.
func pidFile(pid int, status string, lines int, eol string) string {
	all := []string{fmt.Sprint(pid), "/d", "0", "5432", "/s", "*", "1", status}
	return strings.Join(all[:lines], eol) + eol
}

// stoppingFixture is a manager whose fake pg_ctl reports "running" for as long as postmaster.pid exists and whose start writes a pid file with the
// status "ready" and leaves a witness file behind. content builds the pid file of the instance from the pid of a live postmaster.
func stoppingFixture(t *testing.T, stopTO time.Duration, content func(pid int) string) (m *instance.Manager, data string) {
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
	if err := os.WriteFile(filepath.Join(data, "postmaster.pid"), []byte(content(fakePostmaster(t))), 0o600); err != nil {
		t.Fatal(err)
	}
	return m, data
}

func stopping(pid int) string { return pidFile(pid, "stopping", 8, "\n") }

func started(data string) bool {
	_, err := os.Stat(filepath.Join(data, "start-witness"))
	return err == nil
}

// A start that finds the postmaster shutting down (pg_ctl status still says running) waits for the shutdown to end and then starts the instance: it
// does not report success for an instance that goes down.
func TestAStartWaitsForAPostmasterThatIsShuttingDownAndThenStartsIt(t *testing.T) {
	m, data := stoppingFixture(t, 10*time.Second, stopping)
	go func() {
		time.Sleep(700 * time.Millisecond)
		_ = os.Remove(filepath.Join(data, "postmaster.pid")) // the shutdown ends
	}()
	if err := m.Start(context.Background(), 1); err != nil {
		t.Fatalf("start: %v", err)
	}
	if !started(data) {
		t.Fatal("the instance was not started: the start took a postmaster that was shutting down for a running one")
	}
	b, _ := os.ReadFile(filepath.Join(data, "postmaster.pid"))
	if !strings.HasSuffix(strings.TrimSpace(string(b)), "ready") {
		t.Fatalf("the pid file after the start: %q", b)
	}
}

// The same with the line endings of a file that was edited on another system.
func TestAStartWaitsForAShutdownThatIsWrittenWithCRLF(t *testing.T) {
	m, data := stoppingFixture(t, 10*time.Second, func(pid int) string { return pidFile(pid, "stopping", 8, "\r\n") })
	go func() {
		time.Sleep(500 * time.Millisecond)
		_ = os.Remove(filepath.Join(data, "postmaster.pid"))
	}()
	if err := m.Start(context.Background(), 1); err != nil || !started(data) {
		t.Fatalf("start: %v, started %v", err, started(data))
	}
}

// A shutdown that does not end within the bound is an error, never a success, and nothing is started over it.
func TestAStartThatCannotOutwaitTheShutdownIsAnError(t *testing.T) {
	m, data := stoppingFixture(t, time.Second, stopping)
	begin := time.Now()
	if err := m.Start(context.Background(), 1); err == nil {
		t.Fatal("a start reported success for a postmaster that stays in its shutdown")
	}
	if took := time.Since(begin); took > 5*time.Second {
		t.Fatalf("the wait took %s, want the bound of the stop (1 s)", took)
	}
	if started(data) {
		t.Fatal("pg_ctl start was run over a postmaster that was shutting down")
	}
}

// What is not a shutdown is found running, as before: nothing is waited for and nothing is started. A postmaster that is up ("ready", "standby " with its
// padding), one that is still starting (a file of fewer than 8 lines, or an empty one), and a "stopping" file whose pid is another program.
func TestAStartOfAPostmasterThatIsNotShuttingDownIsANoOp(t *testing.T) {
	for name, content := range map[string]func(pid int) string{
		"ready":            func(pid int) string { return pidFile(pid, "ready   ", 8, "\n") },
		"standby with pad": func(pid int) string { return pidFile(pid, "standby ", 8, "\n") },
		"starting":         func(pid int) string { return pidFile(pid, "starting", 8, "\n") },
		// The last line says stopping but the status line (the 8th) is not there: a postmaster that is still starting.
		"five lines":         func(pid int) string { return fmt.Sprintf("%d\n/d\n0\n5432\nstopping\n", pid) },
		"seven lines":        func(pid int) string { return fmt.Sprintf("%d\n/d\n0\n5432\n/s\n*\nstopping\n", pid) },
		"empty":              func(int) string { return "" },
		"stopping, not ours": func(int) string { return pidFile(os.Getpid(), "stopping", 8, "\n") }, // the pid of this test program, which is not a postmaster
	} {
		t.Run(name, func(t *testing.T) {
			m, data := stoppingFixture(t, time.Second, content)
			begin := time.Now()
			if err := m.Start(context.Background(), 1); err != nil {
				t.Fatal(err)
			}
			if started(data) {
				t.Fatal("a running instance was started again")
			}
			if took := time.Since(begin); took > 800*time.Millisecond {
				t.Fatalf("the start waited %s for an instance that is not shutting down", took)
			}
		})
	}
}

// A start that waited for a shutdown does not bring the instance back during the stop of the service.
func TestAStartThatWaitedDoesNotStartDuringTheStopOfTheService(t *testing.T) {
	m, data := stoppingFixture(t, 10*time.Second, stopping)
	go func() {
		time.Sleep(700 * time.Millisecond)
		_ = m.StopForShutdown(context.Background(), 1, func(string) {}) // the service stops: the fake stop removes the pid file
	}()
	if err := m.Start(context.Background(), 1); err == nil {
		t.Fatal("a start brought the instance back during the stop of the service")
	}
	if started(data) {
		t.Fatal("pg_ctl start was run during the stop of the service")
	}
}
