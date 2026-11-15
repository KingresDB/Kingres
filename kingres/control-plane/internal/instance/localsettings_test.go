// SPDX-License-Identifier: AGPL-3.0-only

package instance

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMemorySettings(t *testing.T) {
	got := MemorySettings(1024)
	if got["shared_buffers"] != "256MB" || got["effective_cache_size"] != "512MB" || got["maintenance_work_mem"] != "64MB" {
		t.Errorf("1024 MiB: %v", got)
	}
	if got := MemorySettings(1 << 20); got["maintenance_work_mem"] != "2048MB" {
		t.Errorf("maintenance_work_mem is capped at 2 GiB: %v", got)
	}
}

func TestRenderManagedMemoryAndLogDirectory(t *testing.T) {
	base := ManagedSettings{Port: 5433}
	text, err := RenderManaged(base)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "log_directory = 'log'\n") || strings.Contains(text, "shared_buffers") {
		t.Errorf("defaults:\n%s", text)
	}
	s := base
	s.MemoryMegabytes, s.LogDirectory = 2048, "/var/log/kingres"
	text, err = RenderManaged(s)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"shared_buffers = '512MB'", "effective_cache_size = '1024MB'", "maintenance_work_mem = '128MB'", "log_directory = '/var/log/kingres'\n"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in:\n%s", want, text)
		}
	}
	// Extra still wins over the derived values (an instance created before the quota was recorded).
	s.Extra = map[string]string{"shared_buffers": "300MB"}
	text, err = RenderManaged(s)
	if err != nil || !strings.Contains(text, "shared_buffers = '300MB'") || strings.Contains(text, "512MB") {
		t.Errorf("Extra must win: %v\n%s", err, text)
	}
	// Unsafe log directories are refused.
	for _, bad := range []string{"relative", "/var/../etc", "/var/log/\nshared_buffers = 1", "/a//b"} {
		s := base
		s.LogDirectory = bad
		if _, err := RenderManaged(s); !errors.Is(err, ErrInvalid) {
			t.Errorf("log directory %q accepted (%v)", bad, err)
		}
	}
}

func TestPostmasterToken(t *testing.T) {
	root := t.TempDir()
	bin := t.TempDir()
	for _, exe := range []string{"initdb", "pg_ctl"} {
		if err := os.WriteFile(filepath.Join(bin, exe), nil, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	m, err := New(Options{Root: root, BinDir: bin, AllowRoot: true})
	if err != nil {
		t.Fatal(err)
	}
	if tok := m.PostmasterToken(1); tok != "" {
		t.Errorf("no postmaster.pid: %q", tok)
	}
	data := filepath.Join(root, "1", "pgdata")
	if err := os.MkdirAll(data, 0o700); err != nil {
		t.Fatal(err)
	}
	pid := filepath.Join(data, "postmaster.pid")
	write := func(s string) {
		t.Helper()
		if err := os.WriteFile(pid, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("4242\n/data\n1700000000\n5433\n")
	first := m.PostmasterToken(1)
	if first != "4242:1700000000" {
		t.Errorf("token %q", first)
	}
	// The same process id with another start time is another postmaster.
	write("4242\n/data\n1700000999\n5433\n")
	if m.PostmasterToken(1) == first {
		t.Error("a reused process id gave the same token")
	}
	write("not a pid\n")
	if tok := m.PostmasterToken(1); tok != "" {
		t.Errorf("garbage: %q", tok)
	}
}
