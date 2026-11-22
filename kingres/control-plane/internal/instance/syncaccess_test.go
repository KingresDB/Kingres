// SPDX-License-Identifier: AGPL-3.0-only

package instance

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func accessRig(t *testing.T, ids ...int) *Manager {
	t.Helper()
	root, bin := t.TempDir(), t.TempDir()
	for _, p := range []string{"initdb", "pg_ctl", "postgres", "pg_basebackup", "pg_rewind"} {
		if err := os.WriteFile(filepath.Join(bin, p), nil, 0o755); err != nil { //nolint:gosec // a stand-in
			t.Fatal(err)
		}
	}
	m, err := New(Options{Root: root, BinDir: bin})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		dir, _ := m.Dir(id)
		data, _ := m.DataDir(id)
		if err := os.MkdirAll(data, 0o700); err != nil {
			t.Fatal(err)
		}
		mk, _ := json.Marshal(Marker{NodeID: id, Port: 6000 + id, CreatedAt: time.Now(), Magic: markerMagic})
		if err := os.WriteFile(filepath.Join(dir, MarkerName), mk, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(data, "pg_hba.conf"), []byte("host all all 10.0.0.0/8 scram-sha-256\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(data, "pg_ident.conf"), []byte("# map\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return m
}

// A server whose only instance is a standby (the old primary rebuilt, nothing else on it) copies the rules from itself:
// nothing changes, and the rule is not added at every round.
func TestAccessRulesAreNotCopiedOntoThemselves(t *testing.T) {
	m := accessRig(t, 1)
	data, _ := m.DataDir(1)
	before, _ := os.ReadFile(filepath.Join(data, "pg_hba.conf"))
	for i := 0; i < 3; i++ {
		changed, err := m.SyncAccess(context.Background(), 1, 1, "kingres")
		if err != nil || changed {
			t.Fatalf("round %d: %v %v", i, changed, err)
		}
	}
	after, _ := os.ReadFile(filepath.Join(data, "pg_hba.conf"))
	if string(before) != string(after) {
		t.Fatalf("the file changed:\n%s", after)
	}
}

// The rule that lets kingresd in on the socket is added once, also when the source is a standby that has it already.
func TestTheStandbyRuleIsNotAddedTwice(t *testing.T) {
	m := accessRig(t, 1, 101, 102)
	ctx := context.Background()
	if changed, err := m.SyncAccess(ctx, 1, 101, "kingres"); err != nil || !changed {
		t.Fatalf("first copy: %v %v", changed, err)
	}
	if changed, err := m.SyncAccess(ctx, 1, 101, "kingres"); err != nil || changed {
		t.Fatalf("a second copy changed something: %v %v", changed, err)
	}
	// 102 copies from 101, which has the rule: it is not prefixed again
	if _, err := m.SyncAccess(ctx, 101, 102, "kingres"); err != nil {
		t.Fatal(err)
	}
	d, _ := m.DataDir(102)
	hba, _ := os.ReadFile(filepath.Join(d, "pg_hba.conf"))
	if n := strings.Count(string(hba), "peer map="+StandbyIdentMap); n != 1 {
		t.Fatalf("the rule is there %d times:\n%s", n, hba)
	}
}

// An old primary that was rebuilt as a standby of the new one and is the only instance of its server has to be reachable by kingresd: a
// clean switchover leaves nothing for pg_rewind to copy, so the rule of the unix socket is added to its own files, once.
func TestTheOnlyInstanceThatIsACopyGetsTheRuleOfTheSocket(t *testing.T) {
	m := accessRig(t, 1)
	if err := m.WriteRole(1, Role{Group: 1, Role: RoleStandby, Origin: OriginCopy}); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	changed, err := m.SyncAccess(ctx, 1, 1, "kingres")
	if err != nil || !changed {
		t.Fatalf("first round: %v %v", changed, err)
	}
	if changed, err := m.SyncAccess(ctx, 1, 1, "kingres"); err != nil || changed {
		t.Fatalf("a second round changed something: %v %v", changed, err)
	}
	data, _ := m.DataDir(1)
	hba, _ := os.ReadFile(filepath.Join(data, "pg_hba.conf"))
	ident, _ := os.ReadFile(filepath.Join(data, "pg_ident.conf"))
	if n := strings.Count(string(hba), "peer map="+StandbyIdentMap); n != 1 {
		t.Fatalf("the rule is in pg_hba.conf %d times:\n%s", n, hba)
	}
	if !strings.Contains(string(ident), StandbyIdentMap+` "kingres" `+ServiceRole) {
		t.Fatalf("pg_ident.conf lacks the mapping:\n%s", ident)
	}
}
