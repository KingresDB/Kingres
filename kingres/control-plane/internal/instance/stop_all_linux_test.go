// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

//go:build linux

package instance_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/instance"
)

// stopAllFixture is a manager with the instances 1 (a primary) and 101 and 102 (standbys). Its fake pg_ctl "stop" writes its arguments to the file stop-args of
// the data directory and then
//   - exits 1 when the data directory has a file named "failreq" (the request fails, the instance stays up),
//   - waits until the pid file is gone when it was given -w (a request that waits: the property of the fix is that the service never makes one),
//   - leaves the pid file of an instance that has a file named "slow" (it shuts down when the test removes the pid file),
//   - and removes the pid file of the others at once.
func stopAllFixture(t *testing.T, stopTO time.Duration) (m *instance.Manager, data map[int]string) {
	t.Helper()
	root, bin := t.TempDir(), t.TempDir()
	script := `#!/bin/sh
cmd=$1; shift; args="$*"
D=""
while [ $# -gt 0 ]; do if [ "$1" = "-D" ]; then D=$2; fi; shift; done
pid="$D/postmaster.pid"
case "$cmd" in
status) [ -f "$pid" ] && exit 0; exit 3 ;;
stop)
  echo "$args" >> "$D/stop-args"; date +%s%N > "$D/stop-asked"
  [ -f "$D/failreq" ] && exit 1
  case " $args " in *" -w "*) n=0; while [ -f "$pid" ] && [ $n -lt 30 ]; do sleep 0.1; n=$((n+1)); done; [ -f "$pid" ] && exit 1 ;; esac
  [ -f "$D/slow" ] || rm -f "$pid"
  exit 0 ;;
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
	data = map[int]string{}
	for _, id := range []int{1, 101, 102} {
		dir, _ := m.Dir(id)
		d, _ := m.DataDir(id)
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
		mk, _ := json.Marshal(instance.Marker{NodeID: id, Port: 5432, CreatedAt: time.Now(), Magic: "kingresd-instance-v1"})
		if err := os.WriteFile(filepath.Join(dir, instance.MarkerName), mk, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "postmaster.pid"), []byte(pidFile(fakePostmaster(t), "ready", 8, "\n")), 0o600); err != nil {
			t.Fatal(err)
		}
		data[id] = d
	}
	return m, data
}

func asked(data string) bool {
	_, err := os.Stat(filepath.Join(data, "stop-asked"))
	return err == nil
}

func up(data string) bool {
	_, err := os.Stat(filepath.Join(data, "postmaster.pid"))
	return err == nil
}

func touch(t *testing.T, dir, name string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), nil, 0o600); err != nil {
		t.Fatal(err)
	}
}

// The orderly stop of the service asks every instance for its fast shutdown before it waits for any: a standby that is slow to shut down (here: until the
// test lets it) does not delay the request for the primary, and the stand-down of the primary's lease watchdog (the requested call) follows that request at
// once. Waiting for the standby first would leave the primary without a stand-down until its lease ran out and the watchdog stopped it with an immediate
// shutdown (a crash recovery at the next start). The fake pg_ctl blocks on -w, so a request that waits fails this test too, and the arguments of every
// request are checked: a fast shutdown, not waiting.
func TestTheRequestForThePrimaryDoesNotWaitForASlowStandby(t *testing.T) {
	m, data := stopAllFixture(t, 30*time.Second)
	touch(t, data[101], "slow")
	var mu sync.Mutex
	var told []int
	var standbyUpWhenTold bool
	done := make(chan error, 1)
	go func() {
		done <- m.StopAllForShutdown(context.Background(), []int{101}, []int{1}, func(id int, pm string) {
			mu.Lock()
			defer mu.Unlock()
			told = append(told, id)
			standbyUpWhenTold = up(data[101])
			if pm == "" {
				t.Errorf("the postmaster identity of instance %d is empty", id)
			}
		})
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		n := len(told)
		mu.Unlock()
		if n > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the primary was not asked while the standby was still shutting down")
		}
		time.Sleep(20 * time.Millisecond)
	}
	mu.Lock()
	if len(told) != 1 || told[0] != 1 || !standbyUpWhenTold {
		t.Errorf("told %v, standby still up when told %v (want [1] and true)", told, standbyUpWhenTold)
	}
	mu.Unlock()
	// The standby is asked after the primary, without a wait for it: its request arrives although the instance is still up.
	for time.Now().Before(deadline) && !asked(data[101]) {
		time.Sleep(20 * time.Millisecond)
	}
	if !asked(data[101]) || !asked(data[1]) {
		t.Errorf("both instances are asked before the wait: standby %v primary %v", asked(data[101]), asked(data[1]))
	}
	for _, id := range []int{1, 101} {
		b, _ := os.ReadFile(filepath.Join(data[id], "stop-args"))
		args := " " + strings.TrimSpace(string(b)) + " "
		if !strings.Contains(args, " -m fast ") || !strings.Contains(args, " -W ") || strings.Contains(args, " -w ") {
			t.Errorf("instance %d: pg_ctl was run with %q, want a fast shutdown with -W and without -w", id, args)
		}
	}
	select {
	case err := <-done:
		t.Fatalf("the stop returned (%v) while the standby still ran", err)
	case <-time.After(300 * time.Millisecond):
	}
	_ = os.Remove(filepath.Join(data[101], "postmaster.pid")) // the standby is down
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("stop: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the stop did not return after the standby was down")
	}
}

// A primary whose shutdown outlasts its time is an error that names the instance, the other instances are still asked and awaited, and the stop timeout counts
// from the request of each instance.
func TestAnInstanceThatDoesNotShutDownIsAnErrorAndTheOthersAreStillStopped(t *testing.T) {
	m, data := stopAllFixture(t, time.Second)
	touch(t, data[1], "slow")
	var told []int
	begin := time.Now()
	err := m.StopAllForShutdown(context.Background(), []int{101}, []int{1}, func(id int, _ string) { told = append(told, id) })
	if err == nil || !strings.Contains(err.Error(), "instance 1:") {
		t.Fatalf("an instance that stayed up: %v", err)
	}
	if took := time.Since(begin); took > 6*time.Second {
		t.Fatalf("the stop took %s, want about the stop timeout (1 s)", took)
	}
	if len(told) != 1 || told[0] != 1 {
		t.Errorf("told %v, want [1]", told)
	}
	if up(data[101]) {
		t.Error("the standby was not stopped")
	}
}

// A request that fails (pg_ctl exits 1 and the instance stays up) is an error that names its instance and does not keep the other instances from being asked
// and awaited; the stand-down is not given for the instance that did not take the request. Every failure is returned, not the first only.
func TestAFailedRequestDoesNotKeepTheOtherInstancesFromBeingStopped(t *testing.T) {
	m, data := stopAllFixture(t, 3*time.Second)
	touch(t, data[1], "failreq")   // the primary: the request fails
	touch(t, data[102], "failreq") // a standby: the request fails
	var told []int
	err := m.StopAllForShutdown(context.Background(), []int{101, 102}, []int{1}, func(id int, _ string) { told = append(told, id) })
	if err == nil || !strings.Contains(err.Error(), "instance 1:") || !strings.Contains(err.Error(), "instance 102:") || strings.Contains(err.Error(), "instance 101:") {
		t.Fatalf("the error must name instances 1 and 102 and not 101: %v", err)
	}
	if len(told) != 0 {
		t.Errorf("a stand-down was given for an instance that did not take the request: %v", told)
	}
	if up(data[101]) {
		t.Error("the standby whose request worked was not stopped")
	}
	if !up(data[1]) || !up(data[102]) {
		t.Error("an instance whose request failed went down")
	}
}

// A stopped instance is not an error and is not told to anyone; the standbys are still stopped.
func TestAStoppedInstanceIsNeitherAnErrorNorAStandDown(t *testing.T) {
	m, data := stopAllFixture(t, 5*time.Second)
	_ = os.Remove(filepath.Join(data[1], "postmaster.pid"))
	var told []int
	if err := m.StopAllForShutdown(context.Background(), []int{101}, []int{1}, func(id int, _ string) { told = append(told, id) }); err != nil {
		t.Fatal(err)
	}
	if len(told) != 0 {
		t.Errorf("a stopped primary was told: %v", told)
	}
	if up(data[101]) {
		t.Error("the standby was not stopped")
	}
}

// An instance that is in both lists (a role file that cannot be read makes it a standby, and the home instance stands in for a missing primary) is asked once,
// as a primary.
func TestAnInstanceInBothListsIsAskedOnceAsAPrimary(t *testing.T) {
	m, data := stopAllFixture(t, 5*time.Second)
	touch(t, data[1], "slow") // the instance stays up for a while, so a second request for it would run pg_ctl again
	go func() {
		time.Sleep(600 * time.Millisecond)
		_ = os.Remove(filepath.Join(data[1], "postmaster.pid"))
	}()
	var told []int
	if err := m.StopAllForShutdown(context.Background(), []int{1, 101}, []int{1}, func(id int, _ string) { told = append(told, id) }); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(data[1], "stop-args"))
	if n := strings.Count(string(b), "\n"); n != 1 {
		t.Errorf("instance 1 was asked %d times, want once", n)
	}
	if len(told) != 1 || told[0] != 1 {
		t.Errorf("told %v, want [1]", told)
	}
}

// The primaries are asked before the standbys: the stand-down of a primary is the time-critical part, and a slow call for a standby must not hold it back.
func TestThePrimariesAreAskedBeforeTheStandbys(t *testing.T) {
	m, data := stopAllFixture(t, 5*time.Second)
	if err := m.StopAllForShutdown(context.Background(), []int{101, 102}, []int{1}, nil); err != nil {
		t.Fatal(err)
	}
	stamp := func(id int) int64 {
		b, err := os.ReadFile(filepath.Join(data[id], "stop-asked"))
		if err != nil {
			t.Fatalf("instance %d was not asked: %v", id, err)
		}
		var n int64
		for _, c := range strings.TrimSpace(string(b)) {
			n = n*10 + int64(c-'0')
		}
		return n
	}
	if stamp(1) > stamp(101) || stamp(1) > stamp(102) {
		t.Errorf("the primary was asked at %d, the standbys at %d and %d", stamp(1), stamp(101), stamp(102))
	}
}
