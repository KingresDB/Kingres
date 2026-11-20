// SPDX-License-Identifier: AGPL-3.0-only

//go:build linux

package pgconfig

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/cluster"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/instance"
)

// P4D-05 (ST-03): RestartInstance with ifPending restarts the instance only when something waits for a restart. The
// condition is pinned here over a fake pg_ctl and a stand-in for the question, so that a unit test fails when the
// repeat of a restart call (its answer was lost) restarts an instance again.
func TestARestartCallWithIfPendingRestartsOnlyWhatIsPending(t *testing.T) {
	root, bin := t.TempDir(), t.TempDir()
	log := filepath.Join(root, "pgctl.log")
	script := `#!/bin/sh
cmd=$1; shift
D=""
while [ $# -gt 0 ]; do if [ "$1" = "-D" ]; then D=$2; fi; shift; done
pid="$D/postmaster.pid"
echo "$cmd" >> "` + log + `"
case "$cmd" in
status) [ -f "$pid" ] && exit 0; exit 3 ;;
start) echo 1 > "$pid"; exit 0 ;;
stop) rm -f "$pid"; exit 0 ;;
esac
exit 0
`
	for _, p := range []string{"pg_ctl", "initdb", "pg_basebackup", "pg_rewind"} {
		if err := os.WriteFile(filepath.Join(bin, p), []byte(script), 0o755); err != nil { //nolint:gosec // a test script
			t.Fatal(err)
		}
	}
	svc, err := cluster.New(cluster.Config{DataDir: root, BinDir: bin}, nil)
	if err != nil {
		t.Fatal(err)
	}
	const id = 101
	m := svc.Manager()
	dir, _ := m.Dir(id)
	data, _ := m.DataDir(id)
	if err := os.MkdirAll(data, 0o700); err != nil {
		t.Fatal(err)
	}
	mk, _ := json.Marshal(instance.Marker{NodeID: id, Port: 5433, CreatedAt: time.Now(), Magic: "kingresd-instance-v1"})
	if err := os.WriteFile(filepath.Join(dir, instance.MarkerName), mk, 0o600); err != nil {
		t.Fatal(err)
	}
	pending := false
	a := &Agent{Svc: svc, Self: "s1", needed: func(context.Context, int) bool { return pending }}
	calls := func() string { b, _ := os.ReadFile(log); return string(b) }
	restart := func(ifPending bool) {
		// The restart is made; the wait for the instance to answer cannot end well on a fake, so it is cut short.
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = a.RestartInstance(ctx, id, ifPending)
	}

	restart(true) // nothing waits for a restart: nothing happens
	if got := calls(); strings.Contains(got, "stop") || strings.Contains(got, "start") {
		t.Fatalf("an instance with nothing pending was restarted: %q", got)
	}
	restart(false) // without ifPending the call restarts, whatever is pending
	if got := calls(); !strings.Contains(got, "start") {
		t.Fatalf("a restart call without ifPending did not restart: %q", got)
	}
	_ = os.Remove(log)
	pending = true
	restart(true) // something waits: restarted
	if got := calls(); !strings.Contains(got, "start") {
		t.Fatalf("an instance with a pending setting was not restarted: %q", got)
	}
}
