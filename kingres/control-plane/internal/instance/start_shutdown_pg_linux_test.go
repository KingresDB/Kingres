// SPDX-License-Identifier: AGPL-3.0-only

//go:build linux

package instance_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/pgtest"
)

// checkpointerOf finds the checkpointer process of the postmaster pm (a child whose command line says so).
func checkpointerOf(pm int) int {
	ents, _ := os.ReadDir("/proc")
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		stat, err := os.ReadFile("/proc/" + e.Name() + "/stat")
		if err != nil {
			continue
		}
		// pid (comm) state ppid ...: the command may hold spaces, the fields after the last ")" do not.
		s := string(stat)
		f := strings.Fields(s[strings.LastIndex(s, ")")+1:])
		if len(f) < 2 || f[1] != strconv.Itoa(pm) {
			continue
		}
		cmd, _ := os.ReadFile("/proc/" + e.Name() + "/cmdline")
		if strings.Contains(string(cmd), "checkpointer") {
			return pid
		}
	}
	return 0
}

// A real postmaster whose shutdown is held (the checkpointer is stopped, so the shutdown checkpoint cannot finish): a start that finds it in that state
// waits for the shutdown to end and then starts the instance, so it ends up running (it did not report success for an instance that went down).
func TestAStartDuringAHeldShutdownOfARealPostmasterEndsWithTheInstanceRunning(t *testing.T) {
	in := pgtest.Start(t, nil)
	m := in.Manager
	data, _ := m.DataDir(1)
	b, err := os.ReadFile(filepath.Join(data, "postmaster.pid"))
	if err != nil {
		t.Fatal(err)
	}
	pm, _ := strconv.Atoi(strings.TrimSpace(strings.Split(string(b), "\n")[0]))
	cp := checkpointerOf(pm)
	if cp == 0 {
		t.Skip("the checkpointer of the postmaster was not found")
	}
	if err := syscall.Kill(cp, syscall.SIGSTOP); err != nil {
		t.Skipf("cannot stop the checkpointer: %v", err)
	}
	resumed := false
	resume := func() {
		if !resumed {
			resumed = true
			_ = syscall.Kill(cp, syscall.SIGCONT)
		}
	}
	defer resume()
	stop := exec.Command(filepath.Join(pgtest.BinDir(), "pg_ctl"), "stop", "-D", data, "-m", "fast", "-W") //nolint:gosec // a test
	if out, err := stop.CombinedOutput(); err != nil {
		t.Fatalf("pg_ctl stop -W: %v: %s", err, out)
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		b, _ := os.ReadFile(filepath.Join(data, "postmaster.pid"))
		if strings.HasSuffix(strings.TrimSpace(string(b)), "stopping") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the postmaster never said stopping: %q", b)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if up, err := m.Running(context.Background(), 1); err != nil || !up {
		t.Fatalf("pg_ctl status of a postmaster that is shutting down: %v %v (the defect needs it to say running)", up, err)
	}
	done := make(chan error, 1)
	go func() { done <- m.Start(context.Background(), 1) }()
	time.Sleep(1500 * time.Millisecond)
	select {
	case err := <-done:
		t.Fatalf("the start returned (%v) while the shutdown was held", err)
	default:
	}
	resume()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("start: %v", err)
		}
	case <-time.After(2 * time.Minute):
		t.Fatal("the start did not end")
	}
	if up, err := m.Running(context.Background(), 1); err != nil || !up {
		t.Fatalf("the instance is not running after the start: %v %v", up, err)
	}
	if b, _ := os.ReadFile(filepath.Join(data, "postmaster.pid")); strings.HasSuffix(strings.TrimSpace(string(b)), "stopping") {
		t.Fatalf("the instance that runs is still stopping: %q", b)
	}
}
