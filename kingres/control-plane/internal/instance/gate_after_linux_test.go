// SPDX-License-Identifier: AGPL-3.0-only

//go:build linux

package instance_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/instance"
)

// FO-14: a fence that arrives while a start by another caller runs wrote its marker and found nothing to stop; the
// start asks the gate again when it is done and stops the instance that came up against it.
func TestAnInstanceThatCameUpAgainstTheGateIsStopped(t *testing.T) {
	root, bin := t.TempDir(), t.TempDir()
	script := `#!/bin/sh
cmd=$1; shift
D=""
while [ $# -gt 0 ]; do if [ "$1" = "-D" ]; then D=$2; fi; shift; done
pid="$D/postmaster.pid"
case "$cmd" in
status) [ -f "$pid" ] && exit 0; exit 3 ;;
start) echo 1 > "$pid"; exit 0 ;;
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
	var asked atomic.Int32
	m.SetStartGate(func(int) error {
		if asked.Add(1) >= 2 { // the fence arrived while the start ran
			return instance.ErrLeaseRequired
		}
		return nil
	})
	if err := m.Start(context.Background(), 1); !errors.Is(err, instance.ErrLeaseRequired) {
		t.Fatalf("Start = %v, want the gate's refusal", err)
	}
	if up, _ := m.Running(context.Background(), 1); up {
		t.Fatal("the instance that came up against the gate is still running")
	}
}
