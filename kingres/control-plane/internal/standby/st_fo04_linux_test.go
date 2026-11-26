// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

//go:build linux

package standby

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/cluster"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/instance"
)

// fakeAgent is an agent with an instance manager over a fake pg_ctl and one managed directory, id 101, with the
// meta file of the standby entry returned.
func fakeAgent(t *testing.T) (*Agent, *instance.Manager, Entry, string) {
	t.Helper()
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
	e := Entry{ID: 7, Group: 1, Server: "s1", PrimaryServer: "s2", PrimaryPort: 5432, Port: 5433, Ready: true, Generation: 3, InstanceID: id}
	if err := a.writeMeta(id, e); err != nil {
		t.Fatal(err)
	}
	return a, m, e, data
}

// FO-04 and FO-09: a directory is deleted as a standby only when it is proved to be one. A promoted standby
// whose role file is not written yet (a crash between pg_promote and the file), is broken, or says primary is a
// primary with its data and is kept; the same for a rebuild.
func TestADirectoryThatIsNotProvedToBeAStandbyIsNeverDeleted(t *testing.T) {
	ctx := context.Background()
	a, m, e, data := fakeAgent(t)
	const id = 101
	dir, _ := m.Dir(id)
	rolePath := filepath.Join(dir, instance.RoleFileName)
	signal := filepath.Join(data, "standby.signal")

	cases := []struct {
		name  string
		setup func()
	}{
		{"no role file, promoted (no standby.signal)", func() { _ = os.Remove(rolePath); _ = os.Remove(signal) }},
		{"broken role file", func() { _ = os.WriteFile(rolePath, []byte("{not json"), 0o600); _ = os.WriteFile(signal, nil, 0o600) }},
		{"role file says primary", func() {
			_ = m.WriteRole(id, instance.Role{Group: 1, Role: instance.RolePrimary, Origin: instance.OriginCopy})
			_ = os.WriteFile(signal, nil, 0o600)
		}},
		{"role file says standby but the data directory has no standby.signal", func() {
			_ = m.WriteRole(id, instance.Role{Group: 1, Role: instance.RoleStandby, Origin: instance.OriginCopy})
			_ = os.Remove(signal)
		}},
	}
	for _, c := range cases {
		c.setup()
		if err := a.Remove(ctx, e); !errors.Is(err, ErrNotProvedStandby) {
			t.Errorf("%s: Remove = %v, want ErrNotProvedStandby", c.name, err)
		}
		if _, err := a.Rebuild(ctx, Entry{ID: e.ID, Group: 1, Server: "s1", PrimaryServer: "s2", PrimaryPort: 5432, Generation: 9, InstanceID: id}, true); !errors.Is(err, ErrNotProvedStandby) {
			t.Errorf("%s: Rebuild = %v, want ErrNotProvedStandby", c.name, err)
		}
		if !m.Exists(id) {
			t.Fatalf("%s: the directory was deleted", c.name)
		}
		if _, err := os.Stat(data); err != nil {
			t.Fatalf("%s: the data directory is gone", c.name)
		}
	}
	// the supervision does not list a directory whose role file is broken or says primary as a standby
	_ = os.WriteFile(rolePath, []byte("{not json"), 0o600)
	if got := a.localStandbys(); len(got) != 0 {
		t.Errorf("a directory with a broken role file is listed as a standby: %+v", got)
	}

	// proved: role file standby and standby.signal: removed
	if err := m.WriteRole(id, instance.Role{Group: 1, Role: instance.RoleStandby, Origin: instance.OriginCopy}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(signal, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	_ = a.Remove(ctx, e) // the slot of the fake setup cannot be dropped: only the directory matters here
	if m.Exists(id) {
		t.Fatal("a proved standby was not removed")
	}
}
