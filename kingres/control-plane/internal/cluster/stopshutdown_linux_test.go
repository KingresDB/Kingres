// SPDX-License-Identifier: AGPL-3.0-only

//go:build linux

package cluster

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/instance"
)

// stopFixture is a service whose instances 1, 101 and 102 have a live fake postmaster (a copy of sleep named postgres) and a fake pg_ctl that
// records every "stop" in the file stop-asked of the data directory, removes the pid file (unless the directory has a file named slow) and says "running" for as long as it exists.
func stopFixture(t *testing.T) (svc *Service, data map[int]string) {
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
	root, bin := t.TempDir(), t.TempDir()
	script := `#!/bin/sh
cmd=$1; shift
D=""
while [ $# -gt 0 ]; do if [ "$1" = "-D" ]; then D=$2; fi; shift; done
pid="$D/postmaster.pid"
case "$cmd" in
status) [ -f "$pid" ] && exit 0; exit 3 ;;
stop) echo asked >> "$D/stop-asked"; [ -f "$D/slow" ] || rm -f "$pid"; exit 0 ;;
esac
exit 0
`
	for _, p := range []string{"pg_ctl", "initdb", "postgres", "pg_basebackup", "pg_rewind"} {
		if err := os.WriteFile(filepath.Join(bin, p), []byte(script), 0o755); err != nil { //nolint:gosec // a test script
			t.Fatal(err)
		}
	}
	svc, err = New(Config{DataDir: root, BinDir: bin}, nil)
	if err != nil {
		t.Fatal(err)
	}
	m := svc.Manager()
	data = map[int]string{}
	for _, id := range []int{1, 101, 102} {
		dir, _ := m.Dir(id)
		d, _ := m.DataDir(id)
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
		mk, _ := json.Marshal(instance.Marker{NodeID: id, Port: 5432 + id, CreatedAt: time.Now(), Magic: "kingresd-instance-v1"})
		if err := os.WriteFile(filepath.Join(dir, instance.MarkerName), mk, 0o600); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(exe, "120") //nolint:gosec // a test program
		if err := cmd.Start(); err != nil {
			t.Skipf("cannot run the copy of sleep: %v", err)
		}
		t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
		pid := fmt.Sprintf("%d\n/d\n%d\n5432\n/s\n*\n1\nready\n", cmd.Process.Pid, 1000+id)
		if err := os.WriteFile(filepath.Join(d, "postmaster.pid"), []byte(pid), 0o600); err != nil {
			t.Fatal(err)
		}
		data[id] = d
	}
	return svc, data
}

func running(data string) bool {
	_, err := os.Stat(filepath.Join(data, "postmaster.pid"))
	return err == nil
}

// The orderly stop of the service splits the instances of the server into the primaries and the standbys by their role files, tells the stand-down of the lease
// watchdog for the primaries only, and stops all of them.
func TestTheOrderlyStopStandsDownThePrimariesOnly(t *testing.T) {
	svc, data := stopFixture(t)
	m := svc.Manager()
	if err := m.WriteRole(1, instance.Role{Group: 1, Role: instance.RolePrimary, Origin: instance.OriginOwn}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []int{101, 102} {
		if err := m.WriteRole(id, instance.Role{Group: int32(id - instance.StandbyBaseID), Role: instance.RoleStandby, Origin: instance.OriginCopy}); err != nil {
			t.Fatal(err)
		}
	}
	var told []int
	var postmasters []string
	if err := svc.StopInstanceForShutdown(context.Background(), func(id int, pm string) { told = append(told, id); postmasters = append(postmasters, pm) }); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(told, []int{1}) || postmasters[0] == "" {
		t.Errorf("stand-down for %v (%q), want the primary 1 only", told, postmasters)
	}
	for id, d := range data {
		if running(d) {
			t.Errorf("instance %d still runs", id)
		}
	}
}

// A server whose home instance is a standby and that runs no primary: the home instance stands in for the primary (WritableIDs), is told once and is not asked
// a second time as a standby.
func TestTheOrderlyStopOfAServerWithoutAPrimaryStandsDownTheHomeInstanceOnce(t *testing.T) {
	svc, data := stopFixture(t)
	m := svc.Manager()
	for _, id := range []int{1, 101, 102} {
		if err := m.WriteRole(id, instance.Role{Group: int32(id%100 + 3), Role: instance.RoleStandby, Origin: instance.OriginCopy}); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(data[1], "slow"), nil, 0o600); err != nil { // stays up for a while: a second request would run pg_ctl again
		t.Fatal(err)
	}
	go func() {
		time.Sleep(600 * time.Millisecond)
		_ = os.Remove(filepath.Join(data[1], "postmaster.pid"))
	}()
	var told []int
	if err := svc.StopInstanceForShutdown(context.Background(), func(id int, _ string) { told = append(told, id) }); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(told, []int{NodeID}) {
		t.Errorf("stand-down for %v, want the home instance only, once", told)
	}
	b, _ := os.ReadFile(filepath.Join(data[1], "stop-asked"))
	if n := strings.Count(string(b), "\n"); n != 1 {
		t.Errorf("the home instance was asked %d times, want once", n)
	}
	for id, d := range data {
		if running(d) {
			t.Errorf("instance %d still runs", id)
		}
	}
}
