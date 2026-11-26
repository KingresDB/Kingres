// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

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

// A stop and a start of one instance at once (a restart of a standby that the agent of the standbys starts again at the
// moment it is down) must not make the stop fail: `pg_ctl stop -w` waits until the pid file is gone, and the start
// of the other caller puts a new one there before it looks. A fake pg_ctl shows it without a PostgreSQL: its stop
// removes the pid file, waits a second and then looks for it, like the real one; the start comes in between.
func TestAStartDuringAStopDoesNotMakeTheStopFail(t *testing.T) {
	root, bin := t.TempDir(), t.TempDir()
	script := `#!/bin/sh
cmd=$1; shift
D=""
while [ $# -gt 0 ]; do if [ "$1" = "-D" ]; then D=$2; fi; shift; done
pid="$D/postmaster.pid"
case "$cmd" in
status) [ -f "$pid" ] && exit 0; exit 3 ;;
start) echo 1 > "$pid"; exit 0 ;;
stop) sleep 0.3; rm -f "$pid"; sleep 1; if [ -f "$pid" ]; then echo "server does not shut down"; exit 1; fi; exit 0 ;;
esac
exit 0
`
	for _, p := range []string{"pg_ctl", "initdb"} {
		if err := os.WriteFile(filepath.Join(bin, p), []byte(script), 0o755); err != nil { //nolint:gosec // a test script
			t.Fatal(err)
		}
	}
	m, err := instance.New(instance.Options{Root: root, BinDir: bin, StopTimeout: 10 * time.Second, StartTimeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	dir, _ := m.Dir(1)
	data, _ := m.DataDir(1)
	if err := os.MkdirAll(data, 0o700); err != nil {
		t.Fatal(err)
	}
	mk, _ := json.Marshal(instance.Marker{NodeID: 1, Port: 5432, CreatedAt: time.Now(), Magic: "kingresd-instance-v1"})
	if err := os.WriteFile(filepath.Join(dir, instance.MarkerName), mk, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data, "postmaster.pid"), []byte("1"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	var wg sync.WaitGroup
	var stopErr, startErr error
	wg.Add(2)
	go func() { defer wg.Done(); stopErr = m.Stop(ctx, 1) }()
	go func() { defer wg.Done(); time.Sleep(600 * time.Millisecond); startErr = m.Start(ctx, 1) }()
	wg.Wait()
	if stopErr != nil {
		t.Fatalf("the stop failed because another caller started the instance meanwhile: %v", stopErr)
	}
	if startErr != nil {
		t.Fatalf("start: %v", startErr)
	}
	if up, err := m.Running(ctx, 1); err != nil || !up {
		t.Fatalf("the instance is not running after the start: %v %v", up, err)
	}
}
