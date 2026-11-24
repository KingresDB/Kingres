// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Phase 5 review L1: a server that runs the lease protocol does not start without the watchdog program, unless allow-no-watchdog is set.
func TestAMissingWatchdogProgramIsAStartupError(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	empty := t.TempDir()
	err := checkWatchdog(empty, false, log)
	if err == nil || !strings.Contains(err.Error(), watchdogBinary) || !strings.Contains(err.Error(), "allow-no-watchdog") {
		t.Fatalf("a missing watchdog program must be an error that names the program and the setting: %v", err)
	}
	if err := checkWatchdog(empty, true, log); err != nil {
		t.Fatalf("allow-no-watchdog must let the server start: %v", err)
	}
	if err := os.WriteFile(filepath.Join(empty, watchdogBinary), []byte("x"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := checkWatchdog(empty, false, log); err != nil {
		t.Fatalf("a watchdog program in pg-bin-dir is found: %v", err)
	}
}
