// SPDX-License-Identifier: AGPL-3.0-only

package instance

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStartupLogTail(t *testing.T) {
	dir := t.TempDir()
	if got := startupLogTail(dir); got != "" {
		t.Fatalf("no log: got %q", got)
	}
	log := filepath.Join(dir, "kingres-startup.log")
	if err := os.WriteFile(log, []byte("LOG: starting\nFATAL: could not bind IPv4 address\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := startupLogTail(dir); got != "LOG: starting | FATAL: could not bind IPv4 address |" {
		t.Fatalf("short log: got %q", got)
	}
	if err := os.WriteFile(log, []byte(strings.Repeat("x", 5000)+"\nthe end\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got := startupLogTail(dir)
	if len(got) > 2100 || !strings.HasSuffix(got, "the end |") {
		t.Fatalf("long log: len %d, tail %q", len(got), got[max(0, len(got)-20):])
	}
}
