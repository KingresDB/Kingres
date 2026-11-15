// SPDX-License-Identifier: AGPL-3.0-only

package setup

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/testutil"
)

var fixed = func() time.Time { return time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC) }

func TestFirstStartCreatesUninitializedStateAndToken(t *testing.T) {
	dir := testutil.TempDir(t)
	m, err := Open(dir, fixed)
	if err != nil {
		t.Fatal(err)
	}
	if m.State() != Uninitialized {
		t.Fatalf("state = %s", m.State())
	}
	tok, err := os.ReadFile(m.TokenPath())
	if err != nil {
		t.Fatal(err)
	}
	token := strings.TrimSpace(string(tok))
	if len(token) < 32 {
		t.Errorf("token too short: %d characters", len(token))
	}
	if runtime.GOOS != "windows" {
		st, _ := os.Stat(m.TokenPath())
		if st.Mode().Perm() != 0o600 {
			t.Errorf("token file mode = %v, want 0600", st.Mode().Perm())
		}
		st, _ = os.Stat(m.StatePath())
		if st.Mode().Perm() != 0o600 {
			t.Errorf("state file mode = %v, want 0600", st.Mode().Perm())
		}
	}
	ok, err := m.VerifyToken(token)
	if err != nil || !ok {
		t.Errorf("valid token: %v %v", ok, err)
	}
	for _, bad := range []string{"", "x", token + "0", token[:len(token)-1], strings.ToUpper(token)} {
		if ok, _ := m.VerifyToken(bad); ok {
			t.Errorf("token %q accepted", bad)
		}
	}
	// A restart keeps the same token.
	m2, err := Open(dir, fixed)
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := m2.VerifyToken(token); !ok {
		t.Error("the token must survive a restart")
	}
}

func TestShortTokenFileIsReplaced(t *testing.T) {
	dir := testutil.TempDir(t)
	if err := os.WriteFile(filepath.Join(dir, TokenFileName), []byte("abc\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := Open(dir, fixed)
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := m.VerifyToken("abc"); ok {
		t.Error("a short token file must not be trusted")
	}
	b, _ := os.ReadFile(m.TokenPath())
	if len(strings.TrimSpace(string(b))) < 32 {
		t.Error("token was not regenerated")
	}
}

func TestTransitionsAndPersistence(t *testing.T) {
	dir := testutil.TempDir(t)
	m, _ := Open(dir, fixed)
	tests := []struct {
		to      State
		wantErr bool
	}{
		{Uninitialized, true}, {Standalone, false}, {Standalone, true}, {Clustered, false}, {Uninitialized, true}, {Standalone, false},
	}
	for _, tc := range tests {
		err := m.Transition(tc.to, "", "", nil)
		if tc.wantErr != errors.Is(err, ErrInvalidTransition) {
			t.Errorf("-> %s: err = %v", tc.to, err)
		}
	}
	id := "db3.example.com"
	if err := m.Transition(Clustered, "cid", "prod", &id); err != nil {
		t.Fatal(err)
	}
	m2, err := Open(dir, fixed)
	if err != nil {
		t.Fatal(err)
	}
	snap := m2.Snapshot()
	if snap.State != Clustered || snap.ClusterID != "cid" || snap.ClusterName != "prod" || snap.ServerID == nil || *snap.ServerID != "db3.example.com" {
		t.Errorf("snapshot after restart = %+v", snap)
	}
}

func TestTransitionConsumesToken(t *testing.T) {
	m, _ := Open(testutil.TempDir(t), fixed)
	b, _ := os.ReadFile(m.TokenPath())
	token := strings.TrimSpace(string(b))
	if err := m.Transition(Standalone, "", "", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(m.TokenPath()); !os.IsNotExist(err) {
		t.Error("token file must be removed")
	}
	if ok, err := m.VerifyToken(token); ok || !errors.Is(err, ErrTokenConsumed) {
		t.Errorf("consumed token: %v %v", ok, err)
	}
}

func TestInitializedNodeRemovesStaleToken(t *testing.T) {
	dir := testutil.TempDir(t)
	m, _ := Open(dir, fixed)
	_ = m.Transition(Standalone, "", "", nil)
	if err := os.WriteFile(m.TokenPath(), []byte(strings.Repeat("a", 48)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir, fixed); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(m.TokenPath()); !os.IsNotExist(err) {
		t.Error("a leftover token file must be deleted on an initialized node")
	}
}

func TestCorruptStateIsRefused(t *testing.T) {
	for name, content := range map[string]string{"garbage": "{", "unknown state": `{"state":"zombie"}`} {
		dir := testutil.TempDir(t)
		_ = os.WriteFile(filepath.Join(dir, StateFileName), []byte(content), 0o600)
		if _, err := Open(dir, fixed); err == nil {
			t.Errorf("%s: Open must fail rather than guess", name)
		}
	}
}

func TestSetupTaskIsPersistedAndSurvivesInitialization(t *testing.T) {
	dir := testutil.TempDir(t)
	m, _ := Open(dir, fixed)
	if m.Snapshot().SetupTask != nil {
		t.Fatal("a fresh node has no setup task")
	}
	want := api.SetupTaskSummary{ID: 7, Kind: "cluster.init", State: "running", ProgressPercent: 40,
		Steps: []api.SetupTaskStep{{Name: "a", State: "done"}, {Name: "b", State: "running"}}}
	if err := m.SetSetupTask(want); err != nil {
		t.Fatal(err)
	}
	if err := m.Transition(Standalone, "cid", "prod", nil); err != nil {
		t.Fatal(err)
	}
	m2, err := Open(dir, fixed)
	if err != nil {
		t.Fatal(err)
	}
	got := m2.Snapshot().SetupTask
	if got == nil || got.ID != 7 || got.State != "running" || got.ProgressPercent != 40 || len(got.Steps) != 2 || got.Steps[1].State != "running" {
		t.Errorf("setup task after restart = %+v", got)
	}
}
