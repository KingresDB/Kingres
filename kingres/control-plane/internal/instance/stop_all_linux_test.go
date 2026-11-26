// SPDX-License-Identifier: AGPL-3.0-only

//go:build linux

package instance_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/instance"
)

// stopAllFixture is a manager with the instances 1 (a primary) and 101 (a standby) whose fake pg_ctl "stop" leaves the pid file of an instance that has
// a file named "slow" in its data directory (it shuts down when the test removes the pid file) and removes the pid file of the others at once.
func stopAllFixture(t *testing.T, stopTO time.Duration) (m *instance.Manager, data map[int]string) {
	t.Helper()
	root, bin := t.TempDir(), t.TempDir()
	script := `#!/bin/sh
cmd=$1; shift
D=""
while [ $# -gt 0 ]; do if [ "$1" = "-D" ]; then D=$2; fi; shift; done
pid="$D/postmaster.pid"
case "$cmd" in
status) [ -f "$pid" ] && exit 0; exit 3 ;;
stop) date +%s%N > "$D/stop-asked"; [ -f "$D/slow" ] || rm -f "$pid"; exit 0 ;;
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
	for _, id := range []int{1, 101} {
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

// The orderly stop of the service asks every instance for its fast shutdown before it waits for any: a standby that is slow to shut down (here: until the
// test lets it) does not delay the request for the primary, and the stand-down of the primary's lease watchdog (the requested call) follows that request at
// once. Waiting for the standby first would leave the primary without a stand-down until its lease ran out and the watchdog stopped it with an immediate
// shutdown (a crash recovery at the next start).
func TestTheRequestForThePrimaryDoesNotWaitForASlowStandby(t *testing.T) {
	m, data := stopAllFixture(t, 30*time.Second)
	if err := os.WriteFile(filepath.Join(data[101], "slow"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var told []int
	var standbyUpWhenTold bool
	done := make(chan error, 1)
	go func() {
		done <- m.StopAllForShutdown(context.Background(), []int{101}, []int{1}, func(id int, pm string) {
			mu.Lock()
			defer mu.Unlock()
			told = append(told, id)
			_, err := os.Stat(filepath.Join(data[101], "postmaster.pid"))
			standbyUpWhenTold = err == nil
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
	if !asked(data[101]) || !asked(data[1]) {
		t.Errorf("both instances are asked before the wait: standby %v primary %v", asked(data[101]), asked(data[1]))
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

// A primary whose shutdown outlasts its time is an error, the other instances are still asked and awaited, and the stop timeout counts from the request of each
// instance.
func TestAnInstanceThatDoesNotShutDownIsAnErrorAndTheOthersAreStillStopped(t *testing.T) {
	m, data := stopAllFixture(t, time.Second)
	if err := os.WriteFile(filepath.Join(data[1], "slow"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	var told []int
	begin := time.Now()
	err := m.StopAllForShutdown(context.Background(), []int{101}, []int{1}, func(id int, _ string) { told = append(told, id) })
	if err == nil {
		t.Fatal("an instance that stayed up was reported as stopped")
	}
	if took := time.Since(begin); took > 6*time.Second {
		t.Fatalf("the stop took %s, want about the stop timeout (1 s)", took)
	}
	if len(told) != 1 || told[0] != 1 {
		t.Errorf("told %v, want [1]", told)
	}
	if _, serr := os.Stat(filepath.Join(data[101], "postmaster.pid")); serr == nil {
		t.Error("the standby was not stopped")
	}
}

// A stopped instance is not an error and is not told to anyone.
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
}
