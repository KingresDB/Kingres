// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

//go:build linux

package cluster

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/instance"
)

// FO-09: two directories that claim one group are not acted on: neither is the primary of the group here.
func TestTwoDirectoriesClaimingOneGroupHaveNoPrimary(t *testing.T) {
	root := t.TempDir()
	bin := t.TempDir()
	for _, p := range []string{"initdb", "pg_ctl", "postgres", "pg_basebackup", "pg_rewind"} {
		name := p
		if err := os.WriteFile(filepath.Join(bin, name), nil, 0o755); err != nil { //nolint:gosec // a stand-in
			t.Fatal(err)
		}
	}
	svc, err := New(Config{DataDir: root, BinDir: bin}, nil)
	if err != nil {
		t.Fatal(err)
	}
	m := svc.Manager()
	for _, id := range []int{1, 101} {
		dir, _ := m.Dir(id)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		mk, _ := json.Marshal(instance.Marker{NodeID: id, Port: 5432 + id, CreatedAt: time.Now(), Magic: "kingresd-instance-v1"})
		if err := os.WriteFile(filepath.Join(dir, instance.MarkerName), mk, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.WriteRole(1, instance.Role{Group: 2, Role: instance.RolePrimary, Origin: instance.OriginOwn}); err != nil {
		t.Fatal(err)
	}
	if li, ok := svc.PrimaryOf(2); !ok || li.ID != 1 {
		t.Fatalf("one claim: %+v %v", li, ok)
	}
	if err := m.WriteRole(101, instance.Role{Group: 2, Role: instance.RolePrimary, Origin: instance.OriginCopy}); err != nil {
		t.Fatal(err)
	}
	if li, ok := svc.PrimaryOf(2); ok {
		t.Fatalf("two claims for group 2 must give no primary, got %+v", li)
	}
}
