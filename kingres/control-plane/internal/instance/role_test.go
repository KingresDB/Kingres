// SPDX-License-Identifier: AGPL-3.0-only

package instance

import (
	"os"
	"path/filepath"
	"testing"
)

func newTestManager(t *testing.T) *Manager {
	t.Helper()
	root, bin := t.TempDir(), t.TempDir()
	for _, exe := range []string{"initdb", "pg_ctl"} {
		if err := os.WriteFile(filepath.Join(bin, exe), nil, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{"1", "103"} {
		if err := os.MkdirAll(filepath.Join(root, id), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	m, err := New(Options{Root: root, BinDir: bin, AllowRoot: true})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// A directory without a role file is what the code before the file made of its id; a
// file wins; a file that cannot be read is an error, never the default (the default
// "primary" for an instance that is a standby would send writes to it).
func TestRoleFileAndDefaults(t *testing.T) {
	m := newTestManager(t)
	r, exists, err := m.RoleOf(1)
	if err != nil || exists || r.Role != RolePrimary || r.Origin != OriginOwn || r.Group != GroupUnknown {
		t.Fatalf("instance 1 without a file: %+v %v %v", r, exists, err)
	}
	r, _, _ = m.RoleOf(103)
	if r.Role != RoleStandby || r.Origin != OriginCopy || r.Group != 3 {
		t.Fatalf("directory 103 without a file: %+v", r)
	}
	// A promoted standby: directory 103 is the primary of group 3 and still a copy.
	if err := m.WriteRole(103, Role{Group: 3, Role: RolePrimary, Origin: OriginCopy, Term: 4}); err != nil {
		t.Fatal(err)
	}
	r, exists, err = m.RoleOf(103)
	if err != nil || !exists || !r.Writable() || !m.IsCopy(103) || !m.IsWritable(103) || r.Term != 4 {
		t.Fatalf("after the promotion: %+v %v %v", r, exists, err)
	}
	// Garbage is an error and MustRole says unknown: neither writable, nor a standby, nor a copy (FO-09).
	dir, _ := m.Dir(103)
	if err := os.WriteFile(filepath.Join(dir, RoleFileName), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.RoleOf(103); err == nil {
		t.Fatal("a broken role file must be an error")
	}
	if u := m.MustRole(103); m.IsWritable(103) || u.IsStandby() || m.IsCopy(103) || u.Role != RoleUnknown {
		t.Fatalf("an unreadable role file must be unknown, not the default of its id: %+v", u)
	}
	if err := m.WriteRole(1, Role{Group: 0, Role: "leader", Origin: OriginOwn}); err == nil {
		t.Fatal("a role other than primary or standby must be refused")
	}
}
