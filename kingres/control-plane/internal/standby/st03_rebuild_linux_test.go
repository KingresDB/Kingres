// SPDX-License-Identifier: AGPL-3.0-only

//go:build linux

package standby

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

// ST-03: a rebuild that is asked again (its answer was lost) must not rebuild again. An agent with an instance
// manager over a fake pg_ctl: the standby that is of the generation asked for is only started (the call is a
// no-op); one of an older generation is rebuilt (here: its instance is removed, and the call fails because the
// primary of the fake setup cannot be reached, which is after the removal).
func TestARepeatedRebuildOfTheSameGenerationDoesNothing(t *testing.T) {
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
	a, err := NewAgent(&Agent{Svc: svc, Self: "s1", DataDir: root})
	if err != nil {
		t.Fatal(err)
	}
	m := svc.Manager()
	const id = 101
	dir, _ := m.Dir(id)
	data, _ := m.DataDir(id)
	if err := os.MkdirAll(data, 0o700); err != nil {
		t.Fatal(err)
	}
	mk, _ := json.Marshal(instance.Marker{NodeID: id, Port: 5433, CreatedAt: time.Now(), Magic: "kingresd-instance-v1"})
	if err := os.WriteFile(filepath.Join(dir, instance.MarkerName), mk, 0o600); err != nil {
		t.Fatal(err)
	}
	have := Entry{ID: 7, Group: 1, Server: "s1", PrimaryServer: "s2", PrimaryPort: 5432, Port: 5433, Ready: true, Generation: 3, InstanceID: id}
	if err := a.writeMeta(id, have); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// The generation asked for is the one the standby is of: nothing is rebuilt, the instance is only made to run.
	if _, err := a.Rebuild(ctx, have, false); err != nil {
		t.Fatalf("a repeated rebuild of the same generation: %v", err)
	}
	if _, err := a.Rebuild(ctx, have, true); err != nil { // diverged does not make it a rebuild again either
		t.Fatalf("a repeated rebuild (diverged) of the same generation: %v", err)
	}
	if !m.Exists(id) {
		t.Fatal("the instance was removed by a repeated rebuild")
	}
	b, _ := os.ReadFile(log)
	for _, bad := range []string{"stop", "pg_rewind", "pg_basebackup"} { // only starts and status checks may have happened
		if strings.Contains(string(b), bad) {
			t.Fatalf("a repeated rebuild did %q: %q", bad, b)
		}
	}

	// An older generation is rebuilt: the instance is removed (the standby is made again from the primary).
	next := have
	next.Generation = 4
	_, _ = a.Rebuild(ctx, next, false)
	if m.Exists(id) {
		t.Fatal("a standby of an older generation was not rebuilt")
	}
}
