// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/pgtest"
)

// Spike S3 of the data export design: what pg_dump writes, as the service role, schema only, for every kind of table of the project. The test builds the
// database of the export fixture and runs the dump the way the exporter does (the same arguments); with KINGRES_S3_OUT set it keeps the output for the
// golden files of the normaliser (one directory per major).
func TestSpikeS3DumpOfEveryTableKind(t *testing.T) {
	e := shared(t)
	buildExportFixture(t, e, "s3db")
	cfg := e.serviceConn(t, "s3db").Config()
	out := os.Getenv("KINGRES_S3_OUT")
	if out == "" {
		out = t.TempDir()
	}
	if err := os.MkdirAll(out, 0o750); err != nil {
		t.Fatal(err)
	}
	for _, section := range []string{"pre-data", "post-data"} {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		cmd := exec.CommandContext(ctx, filepath.Join(pgtest.BinDir(), "pg_dump"), "--schema-only", "--no-owner", "--no-tablespaces", "--no-publications", "--no-subscriptions",
			"--no-security-labels", "--no-privileges", "--section="+section, "--exclude-schema=kingres", "--exclude-schema=kingres_transfer", "--exclude-schema=kingres_ledger", "-h", cfg.Host, "-p", strconv.Itoa(int(cfg.Port)), "-U", cfg.User, "-d", cfg.Database)
		cmd.Env = []string{"PGPASSWORD=" + cfg.Password, "PGCONNECT_TIMEOUT=10", "PATH=/usr/bin:/bin"}
		b, err := cmd.CombinedOutput()
		cancel()
		if err != nil {
			t.Fatalf("pg_dump %s: %v\n%s", section, err, b)
		}
		if werr := os.WriteFile(filepath.Join(out, section+".sql"), b, 0o600); werr != nil {
			t.Fatal(werr)
		}
		t.Logf("%s: %d bytes", section, len(b))
	}
	// The catalog tables the exporter reads.
	c := e.serviceConn(t, "s3db")
	for _, q := range []string{"select relid::regclass::text, method, colocation_id from kingres.dist_table order by 1", "select * from kingres.colocation", "select count(*) from kingres.bucket_map"} {
		rows, err := c.Query(context.Background(), q)
		if err != nil {
			t.Logf("%s: %v", q, err)
			continue
		}
		n := 0
		for rows.Next() {
			vals, _ := rows.Values()
			t.Logf("%s -> %v", q, vals)
			n++
		}
		rows.Close()
	}
}
