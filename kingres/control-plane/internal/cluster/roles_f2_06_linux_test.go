// SPDX-License-Identifier: AGPL-3.0-only

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

func f206Service(t *testing.T, ports map[int]int) *Service {
	t.Helper()
	root, bin := t.TempDir(), t.TempDir()
	for _, p := range []string{"initdb", "pg_ctl", "postgres", "pg_basebackup", "pg_rewind"} {
		if err := os.WriteFile(filepath.Join(bin, p), nil, 0o755); err != nil { //nolint:gosec // a stand-in
			t.Fatal(err)
		}
	}
	svc, err := New(Config{DataDir: root, BinDir: bin}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for id, port := range ports {
		dir, _ := svc.Manager().Dir(id)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		mk, _ := json.Marshal(instance.Marker{NodeID: id, Port: port, CreatedAt: time.Now(), Magic: "kingresd-instance-v1"})
		if err := os.WriteFile(filepath.Join(dir, instance.MarkerName), mk, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return svc
}

func ids(l []LocalInstance) []int {
	var out []int
	for _, li := range l {
		out = append(out, li.ID)
	}
	return out
}

// F2-06: the three states in which no instance is attributed to a group although the catalog names this server its
// primary on a port: an unreadable role file, two claims, a role file of another group.
func TestInstancesThatCannotBeAttributedToAGroupAreListed(t *testing.T) {
	svc := f206Service(t, map[int]int{1: 5401, 101: 5402})
	m := svc.Manager()
	rows := []CatalogRow{{Host: "h", Port: 5401, Group: 2, Role: "primary"}}
	svc.SyncRoles("h", rows)
	if err := m.WriteRole(1, instance.Role{Group: 2, Role: instance.RolePrimary, Origin: instance.OriginOwn}); err != nil {
		t.Fatal(err)
	}
	if err := m.WriteRole(101, instance.Role{Group: 2, Role: instance.RoleStandby, Origin: instance.OriginCopy}); err != nil {
		t.Fatal(err)
	}
	if got := svc.Unattributed(2); len(got) != 0 {
		t.Fatalf("a clean state lists %v", ids(got))
	}

	// (1) the role file of the primary cannot be read
	dir, _ := m.Dir(1)
	if err := os.WriteFile(filepath.Join(dir, instance.RoleFileName), []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := ids(svc.Unattributed(2)); len(got) != 1 || got[0] != 1 {
		t.Fatalf("an unreadable role file on the catalog's port: %v", got)
	}
	if _, ok := svc.PrimaryOf(2); ok {
		t.Fatal("PrimaryOf names an instance whose role file cannot be read")
	}
	if g, primary := svc.GateInfo(1); !primary || g != instance.GroupUnknown {
		t.Fatalf("the gate must hold an unreadable role file like a primary of any group: %d %v", g, primary)
	}

	// (2) two directories claim the group
	if err := m.WriteRole(1, instance.Role{Group: 2, Role: instance.RolePrimary, Origin: instance.OriginOwn}); err != nil {
		t.Fatal(err)
	}
	if err := m.WriteRole(101, instance.Role{Group: 2, Role: instance.RolePrimary, Origin: instance.OriginCopy}); err != nil {
		t.Fatal(err)
	}
	if got := ids(svc.Unattributed(2)); len(got) != 2 {
		t.Fatalf("two claims: %v", got)
	}

	// (3) the role file names another group than the catalog gives for the port
	if err := m.WriteRole(101, instance.Role{Group: 2, Role: instance.RoleStandby, Origin: instance.OriginCopy}); err != nil {
		t.Fatal(err)
	}
	if err := m.WriteRole(1, instance.Role{Group: 3, Role: instance.RolePrimary, Origin: instance.OriginOwn}); err != nil {
		t.Fatal(err)
	}
	if got := ids(svc.Unattributed(2)); len(got) != 1 || got[0] != 1 {
		t.Fatalf("a role file of another group on the catalog's port: %v", got)
	}
	if g, primary := svc.GateInfo(1); !primary || g != instance.GroupUnknown {
		t.Fatalf("the gate must treat a role file that contradicts the catalog as any group: %d %v", g, primary)
	}
}

// Another server's primary row names no instance of this server.
func TestAnotherServersRowListsNothing(t *testing.T) {
	svc := f206Service(t, map[int]int{1: 5401})
	svc.SyncRoles("h", []CatalogRow{{Host: "other", Port: 5401, Group: 2, Role: "primary"}})
	if err := svc.Manager().WriteRole(1, instance.Role{Group: 2, Role: instance.RolePrimary, Origin: instance.OriginOwn}); err != nil {
		t.Fatal(err)
	}
	if got := svc.Unattributed(2); len(got) != 0 {
		t.Fatalf("%v", ids(got))
	}
}
