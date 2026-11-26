// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package cluster

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func fakeDir(t *testing.T, programs ...string) string {
	t.Helper()
	d := t.TempDir()
	for _, p := range programs {
		if err := os.WriteFile(filepath.Join(d, p), []byte("#!/bin/sh\necho 'postgres (PostgreSQL) 16.0'\n"), 0o755); err != nil { //nolint:gosec // test fixture
			t.Fatal(err)
		}
	}
	return d
}

func TestResolveBinDir(t *testing.T) {
	full := fakeDir(t, "postgres", "initdb", "pg_ctl")
	if d, found, err := ResolveBinDir(full); err != nil || !found || d != full {
		t.Errorf("configured: %q %v %v", d, found, err)
	}
	partial := fakeDir(t, "postgres", "initdb")
	if _, _, err := ResolveBinDir(partial); err == nil || !strings.Contains(err.Error(), "pg-bin-dir") {
		t.Errorf("a configured directory without pg_ctl: %v", err)
	}
	if _, _, err := ResolveBinDir(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("a configured directory that does not exist was accepted")
	}
	t.Setenv("PATH", full+string(os.PathListSeparator)+t.TempDir())
	if d, found, err := ResolveBinDir(""); err != nil || !found || d != full {
		t.Errorf("PATH lookup: %q %v %v", d, found, err)
	}
	t.Setenv("PATH", t.TempDir())
	if _, found, err := ResolveBinDir(""); found || err != nil {
		t.Errorf("nothing configured and nothing on PATH: %v %v", found, err)
	}
}

func TestVerifyBinariesRefusesAPlainPostgres(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake programs are shell scripts")
	}
	d := fakeDir(t, "postgres", "initdb", "pg_ctl", "pg_config")
	err := VerifyBinaries(context.Background(), d)
	if err == nil || !strings.Contains(err.Error(), "supported PostgreSQL major") {
		t.Errorf("version 16 programs: %v", err)
	}
	// Right version, but no KingresDB libraries.
	for _, p := range requiredPrograms {
		script := "#!/bin/sh\necho 'x (PostgreSQL) 18.7'\n"
		if err := os.WriteFile(filepath.Join(d, p), []byte(script), 0o755); err != nil { //nolint:gosec // test fixture
			t.Fatal(err)
		}
	}
	lib := t.TempDir()
	cfg := "#!/bin/sh\necho " + lib + "\n"
	if err := os.WriteFile(filepath.Join(d, "pg_config"), []byte(cfg), 0o755); err != nil { //nolint:gosec // test fixture
		t.Fatal(err)
	}
	err = VerifyBinaries(context.Background(), d)
	if err == nil || !strings.Contains(err.Error(), "kingres_shard.so") {
		t.Errorf("missing libraries: %v", err)
	}
	for _, l := range []string{"kingres_shard.so", "kingres_ledger.so"} {
		_ = os.WriteFile(filepath.Join(lib, l), nil, 0o600)
	}
	if err := VerifyBinaries(context.Background(), d); err != nil {
		t.Errorf("a complete installation: %v", err)
	}
	// Major 19, in its pre-release forms too; a mix of majors; an unsupported one.
	write := func(p, ver string) {
		if err := os.WriteFile(filepath.Join(d, p), []byte("#!/bin/sh\necho 'postgres (PostgreSQL) "+ver+"'\n"), 0o755); err != nil { //nolint:gosec // test fixture
			t.Fatal(err)
		}
	}
	for _, v := range []string{"19rc2", "19beta1", "19.0", "18.1"} {
		for _, p := range requiredPrograms {
			write(p, v)
		}
		if err := VerifyBinaries(context.Background(), d); err != nil {
			t.Errorf("version %s: %v", v, err)
		}
	}
	write("pg_ctl", "18.1")
	for _, p := range []string{"postgres", "initdb"} {
		write(p, "19rc2")
	}
	if err := VerifyBinaries(context.Background(), d); err == nil || !strings.Contains(err.Error(), "one major") {
		t.Errorf("mixed majors: %v", err)
	}
	for _, p := range requiredPrograms {
		write(p, "20beta1")
	}
	if err := VerifyBinaries(context.Background(), d); err == nil || !strings.Contains(err.Error(), "20beta1") || !strings.Contains(err.Error(), "18 and 19") {
		t.Errorf("an unsupported major: %v", err)
	}
}
