// SPDX-License-Identifier: PostgreSQL

//go:build !windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// The test binary doubles as the watchdog: a child started with this variable runs main.
func TestMain(m *testing.M) {
	if os.Getenv("KINGRES_WATCHDOG_AS_MAIN") == "1" {
		main()
		return
	}
	os.Exit(m.Run())
}

type wdRig struct {
	dir, data, lease, marker, standdown, calls string
}

// newWDRig makes a data directory whose postmaster.pid names a live process (the
// test itself), a lease file that has run out, and a pg_ctl that only records how
// it was called.
func newWDRig(t *testing.T) *wdRig {
	t.Helper()
	dir := t.TempDir()
	r := &wdRig{dir: dir, data: filepath.Join(dir, "pgdata"), lease: filepath.Join(dir, "lease.json"),
		marker: filepath.Join(dir, "fenced"), standdown: filepath.Join(dir, "standdown"), calls: filepath.Join(dir, "pg_ctl.calls")}
	if err := os.MkdirAll(r.data, 0o700); err != nil {
		t.Fatal(err)
	}
	pid := fmt.Sprintf("%d\n%s\n1700000000\n5432\n", os.Getpid(), r.data)
	if err := os.WriteFile(filepath.Join(r.data, "postmaster.pid"), []byte(pid), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(r.lease, []byte(`{"seq": 1, "validMs": 300}`), 0o600); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\necho \"$@\" >> " + r.calls + "\n"
	if err := os.WriteFile(filepath.Join(dir, "pg_ctl"), []byte(script), 0o700); err != nil { //nolint:gosec // a test script
		t.Fatal(err)
	}
	return r
}

func (r *wdRig) start(t *testing.T) *exec.Cmd {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-lease-file", r.lease, "-data", r.data, "-pg-ctl", filepath.Join(r.dir, "pg_ctl"), "-marker", r.marker, //nolint:gosec // the test binary itself
		"-lock", filepath.Join(r.dir, "lock"), "-standdown", r.standdown, "-poll", "50ms")
	cmd.Env = append(os.Environ(), "KINGRES_WATCHDOG_AS_MAIN=1")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	return cmd
}

func waitFor(d time.Duration, f func() bool) bool {
	for end := time.Now().Add(d); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		if f() {
			return true
		}
	}
	return f()
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

// The control: an expired lease and nothing else makes the watchdog fence.
func TestWatchdogFencesAnExpiredLease(t *testing.T) {
	r := newWDRig(t)
	r.start(t)
	if !waitFor(5*time.Second, func() bool { return exists(r.marker) && exists(r.calls) }) {
		t.Fatal("an expired lease must be fenced: the marker and the immediate stop are expected")
	}
}

// An orderly stop (a fast shutdown that takes longer than the lease) must not be
// turned into an immediate shutdown: the stand-down file names this postmaster.
func TestWatchdogStandsDownForAnOrderlyStop(t *testing.T) {
	r := newWDRig(t)
	if err := os.WriteFile(r.standdown, []byte(fmt.Sprintf("%d 1700000000\n", os.Getpid())), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := r.start(t)
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the watchdog must exit when the stand-down names the running postmaster")
	}
	time.Sleep(500 * time.Millisecond)
	if exists(r.marker) || exists(r.calls) {
		t.Fatal("a watchdog that stood down must neither write the fence marker nor stop the instance")
	}
}

// A stand-down file left by an earlier run names another postmaster and is ignored,
// so a primary that really lost its lease is still fenced.
func TestWatchdogIgnoresAStaleStandDown(t *testing.T) {
	r := newWDRig(t)
	if err := os.WriteFile(r.standdown, []byte(fmt.Sprintf("%d 1600000000\n", os.Getpid())), 0o600); err != nil {
		t.Fatal(err)
	}
	r.start(t)
	if !waitFor(5*time.Second, func() bool { return exists(r.marker) && exists(r.calls) }) {
		t.Fatal("a stale stand-down file must not keep the watchdog from fencing")
	}
}
