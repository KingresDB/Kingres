// SPDX-License-Identifier: AGPL-3.0-only

//go:build linux

package instance_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/instance"
)

// ST-04: the wait for the lifecycle lock ends with the context of the caller. A start that takes long (a fake pg_ctl
// whose start sleeps) must not hold a stop with a short context of its own for as long as it runs; and the
// fencing stop (StopImmediate) takes no lock at all.
func TestTheLifecycleLockWaitEndsWithTheContext(t *testing.T) {
	root, bin := t.TempDir(), t.TempDir()
	script := `#!/bin/sh
cmd=$1; shift
D=""
while [ $# -gt 0 ]; do if [ "$1" = "-D" ]; then D=$2; fi; shift; done
pid="$D/postmaster.pid"
case "$cmd" in
status) [ -f "$pid" ] && exit 0; exit 3 ;;
start) sleep 3; echo 1 > "$pid"; exit 0 ;;
stop) rm -f "$pid"; exit 0 ;;
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
	started := make(chan error, 1)
	go func() { started <- m.Start(context.Background(), 1) }()
	// Wait until the start holds the lock: the fake pg_ctl is sleeping inside it.
	deadline := time.Now().Add(5 * time.Second)
	for {
		sctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
		start := time.Now()
		err := m.Stop(sctx, 1)
		cancel()
		if err != nil {
			if took := time.Since(start); took > time.Second {
				t.Fatalf("a stop with a context of 150 ms waited %s for the lock", took)
			}
			break // it gave up with the error of its context, as it must
		}
		if time.Now().After(deadline) {
			t.Fatal("the stop never met the start's lock")
		}
	}
	// The fencing stop does not wait for the start (it takes no lock).
	begin := time.Now()
	if err := m.StopImmediate(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(begin); took > time.Second {
		t.Fatalf("StopImmediate waited %s behind a start", took)
	}
	<-started
}
