// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026 RooyeKhat Media Co

package instance

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// P13S-05: a standby that was copied from another server must not keep that server's override file. It gets the file
// of this server (instance 1), or the empty file, and none of the files that belong to the instance that was copied.
func TestAStandbyCopyGetsTheOverrideFileOfThisServer(t *testing.T) {
	bin := t.TempDir()
	for _, p := range []string{"initdb", "pg_ctl", "initdb.exe", "pg_ctl.exe"} {
		if err := os.WriteFile(filepath.Join(bin, p), []byte("stand-in"), 0o755); err != nil { //nolint:gosec // a stand-in
			t.Fatal(err)
		}
	}
	m, err := New(Options{Root: t.TempDir(), BinDir: bin})
	if err != nil {
		t.Fatal(err)
	}
	dir1, _ := m.Dir(1)
	data1, _ := m.DataDir(1)
	if err := os.MkdirAll(data1, 0o700); err != nil {
		t.Fatal(err)
	}
	mk, _ := json.Marshal(Marker{NodeID: 1, Port: 5432, CreatedAt: time.Now(), Magic: markerMagic})
	if err := os.WriteFile(filepath.Join(dir1, MarkerName), mk, 0o600); err != nil {
		t.Fatal(err)
	}
	mine := EmptyConfigText + "work_mem = '8MB'\n"
	if err := os.WriteFile(filepath.Join(data1, ConfigFileName), []byte(mine), 0o600); err != nil {
		t.Fatal(err)
	}

	copied := filepath.Join(t.TempDir(), "pgdata")
	if err := os.MkdirAll(copied, 0o700); err != nil {
		t.Fatal(err)
	}
	for suffix, text := range map[string]string{"": "work_mem = '999MB'\n", ".good": "work_mem = '999MB'\n", ".prev": "x\n", ".rolledback": "x\n"} {
		if err := os.WriteFile(filepath.Join(copied, ConfigFileName+suffix), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.seedStandbyOverride(copied); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(copied, ConfigFileName)); string(b) != mine {
		t.Errorf("the standby holds %q, want the file of this server", b)
	}
	for _, suffix := range []string{".good", ".prev", ".rolledback"} {
		if _, err := os.Stat(filepath.Join(copied, ConfigFileName+suffix)); err == nil {
			t.Errorf("%s of the copied instance is still there", suffix)
		}
	}
	// A server without a file of its own gives the empty file.
	if err := os.Remove(filepath.Join(data1, ConfigFileName)); err != nil {
		t.Fatal(err)
	}
	if err := m.seedStandbyOverride(copied); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(copied, ConfigFileName)); string(b) != EmptyConfigText {
		t.Errorf("the standby holds %q, want the empty file", b)
	}
}
