// SPDX-License-Identifier: PostgreSQL

package cluster

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

var requiredPrograms = []string{"postgres", "initdb", "pg_ctl"}

// ResolveBinDir finds the directory of the server programs: the configured
// one, else the directory of the kingresd binary, else the directory on PATH
// that holds all of postgres, initdb and pg_ctl. It does not return an error
// when nothing is configured and nothing is found: managing instances is then
// disabled (found == false). A configured directory that lacks a program is
// an error naming the setting.
func ResolveBinDir(configured string) (dir string, found bool, err error) {
	has := func(d string) bool {
		for _, p := range requiredPrograms {
			if st, e := os.Stat(filepath.Join(d, p)); e != nil || st.IsDir() { //nolint:gosec // operator-configured directory
				return false
			}
		}
		return true
	}
	if configured != "" {
		if !has(configured) {
			return "", false, fmt.Errorf("pg-bin-dir %q does not contain postgres, initdb and pg_ctl", configured)
		}
		return configured, true, nil
	}
	if exe, e := os.Executable(); e == nil {
		if d := filepath.Dir(exe); has(d) {
			return d, true, nil
		}
	}
	for _, d := range filepath.SplitList(os.Getenv("PATH")) {
		if d != "" && has(d) {
			return d, true, nil
		}
	}
	return "", false, nil
}

// VerifyBinaries checks that the programs are a KingresDB build: the version
// output of every program must name PostgreSQL 18 (this build's programs
// print the PostgreSQL version) or KingresDB, and the shard and ledger
// libraries must be installed in the package library directory reported by
// pg_config. A plain PostgreSQL installation is refused.
func VerifyBinaries(ctx context.Context, dir string) error {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	for _, p := range requiredPrograms {
		out, err := exec.CommandContext(ctx, filepath.Join(dir, p), "--version").Output() //nolint:gosec // fixed program names in the configured directory
		if err != nil {
			return fmt.Errorf("pg-bin-dir %q: running %s --version: %w", dir, p, err)
		}
		v := string(out)
		if !strings.Contains(v, "KingresDB") && !strings.Contains(v, "(PostgreSQL) 18") {
			return fmt.Errorf("pg-bin-dir %q: %s reports %q, which is not a KingresDB 18 build", dir, p, strings.TrimSpace(v))
		}
	}
	cfg := filepath.Join(dir, "pg_config")
	if _, err := os.Stat(cfg); err != nil {
		return fmt.Errorf("pg-bin-dir %q: pg_config is missing, so the KingresDB libraries cannot be checked", dir)
	}
	out, err := exec.CommandContext(ctx, cfg, "--pkglibdir").Output() //nolint:gosec // fixed program in the configured directory
	if err != nil {
		return fmt.Errorf("pg-bin-dir %q: pg_config --pkglibdir: %w", dir, err)
	}
	lib := strings.TrimSpace(string(out))
	for _, l := range []string{"kingres_shard.so", "kingres_ledger.so"} {
		if _, err := os.Stat(filepath.Join(lib, l)); err != nil {
			return fmt.Errorf("pg-bin-dir %q is not a KingresDB build: %s is missing from %s", dir, l, lib)
		}
	}
	return nil
}
