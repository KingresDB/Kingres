// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/config"
)

func TestVersionFlag(t *testing.T) {
	// The flag prints version, contract revision and commit and stops.
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	err := run(context.Background(), []string{"-version"}, noEnv, &bytes.Buffer{}, nil)
	_ = w.Close()
	os.Stdout = old
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(r)
	if !errors.Is(err, config.ErrHelp) {
		t.Fatalf("run: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "kingresd "+version) || !strings.Contains(out, "contract "+contractRevision) || !strings.Contains(out, "commit "+commit) {
		t.Errorf("version output %q", out)
	}
	if commit != "unknown" && commit == "" {
		t.Error("commit must be unknown when not injected")
	}
}

func TestMissingPostgresProgramsStopTheServerWithAClearMessage(t *testing.T) {
	dir := t.TempDir()
	err := run(context.Background(), []string{"-data-dir", filepath.Join(dir, "d"), "-listen", "127.0.0.1:0", "-pg-bin-dir", filepath.Join(dir, "none")},
		noEnv, &bytes.Buffer{}, nil)
	if err == nil || !strings.Contains(err.Error(), "pg-bin-dir") {
		t.Errorf("error %v does not name the setting", err)
	}
}

// TestInstancesSurviveARestartAndAreStoppedOnRequest: stopping kingresd leaves
// the instance running, a new kingresd re-adopts it (same postmaster), and
// "-stop-instances" stops it with a fast shutdown.
func TestInstancesSurviveARestartAndAreStoppedOnRequest(t *testing.T) {
	e := startCluster(t)
	pidFile := filepath.Join(e.dataDir, "instances", "1", "pgdata", "postmaster.pid")
	before, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pidOf := func(b []byte) string { l, _, _ := strings.Cut(string(b), "\n"); return l }
	e.stopManager(t)
	after, err := os.ReadFile(pidFile)
	if err != nil || pidOf(after) != pidOf(before) {
		t.Fatalf("the instance did not survive the manager: %v", err)
	}
	e.startManager(t)
	e.login(t, adminUser, adminPass)
	e.expect(t, 200, "GET", "/cluster", nil)
	now, _ := os.ReadFile(pidFile)
	if pidOf(now) != pidOf(before) {
		t.Errorf("the instance was restarted instead of re-adopted: %s -> %s", pidOf(before), pidOf(now))
	}
	// A network change still works after re-adoption (the role mapping starts empty).
	e.expect(t, 200, "GET", "/settings/network-access", nil)
	e.stopManager(t)
	args := append([]string{}, e.args...)
	args = append(args, "-stop-instances")
	if err := run(context.Background(), args, noEnv, &bytes.Buffer{}, nil); err != nil {
		t.Fatalf("-stop-instances: %v", err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		if _, err := os.Stat(pidFile); err != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the instance is still running after -stop-instances")
		}
		time.Sleep(200 * time.Millisecond)
	}
	// And the next start brings it back.
	e.startManager(t)
	e.login(t, adminUser, adminPass)
	if _, err := os.Stat(pidFile); err != nil {
		t.Errorf("the instance was not started: %v", err)
	}
}
