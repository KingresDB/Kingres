// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package ops

// The schema dump of a database export (design "The task database.export", step 2): pg_dump of the same PostgreSQL build as the instance, run by the service
// role, schema only, in the snapshot of the transaction that read the catalog. No client value is in any argument, the environment is clean, the output is
// read through a pipe with a size cap, and nothing that pg_dump writes to its error output is ever copied into a task, a problem or a log.

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/transfer"
)

// MaxDumpBytes is the largest output of one run of pg_dump (design: 64 MiB).
const MaxDumpBytes = 64 << 20

// DumpTimeout is the longest one run of pg_dump may take.
var DumpTimeout = 10 * time.Minute

// dumpArgs are the fixed arguments of pg_dump. None comes from a client. The connection is given by the environment (a password never goes into an
// argument), and the sections are two runs.
func dumpArgs(section, snapshot string) []string {
	return []string{"--schema-only", "--no-owner", "--no-tablespaces", "--no-publications", "--no-subscriptions", "--no-security-labels", "--no-privileges",
		"--section=" + section, "--snapshot=" + snapshot,
		"--exclude-schema=kingres", "--exclude-schema=kingres_transfer", "--lock-wait-timeout=60s"}
}

// dumpEnv is the environment of pg_dump: the connection of the service role and nothing else (no PGPASSFILE, no PGSERVICE, no PGOPTIONS of the process).
func dumpEnv(cfg *pgx.ConnConfig) []string {
	return []string{"PGHOST=" + cfg.Host, "PGPORT=" + strconv.Itoa(int(cfg.Port)), "PGUSER=" + cfg.User, "PGPASSWORD=" + cfg.Password, "PGDATABASE=" + cfg.Database,
		"PGSSLMODE=disable", "PGCONNECT_TIMEOUT=15", "PGAPPNAME=kingresd-export", "PATH=" + os.Getenv("PATH")}
}

// runDump runs pg_dump of the build of the instance (from its binary directory, never from PATH) for one section and returns what it wrote.
func (o *Ops) runDump(ctx context.Context, cfg *pgx.ConnConfig, section, snapshot string) (string, error) {
	dir := ""
	if o.Transfer != nil {
		dir = o.Transfer.PGBinDir
	}
	if dir == "" {
		return "", transfer.Errorf(transfer.CodeDumpFailed, false, "the directory of the PostgreSQL programs is not known, so the schema cannot be dumped")
	}
	prog := filepath.Join(dir, "pg_dump")
	if _, err := os.Stat(prog); err != nil {
		return "", transfer.Errorf(transfer.CodeDumpFailed, false, "pg_dump is not in the directory of the PostgreSQL programs")
	}
	ctx, cancel := context.WithTimeout(ctx, DumpTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, prog, dumpArgs(section, snapshot)...) //nolint:gosec // a fixed program and fixed arguments
	cmd.Env = dumpEnv(cfg)
	cmd.Dir = dir
	out := &capBuffer{max: MaxDumpBytes}
	cmd.Stdout = out
	// What pg_dump says on its error output can name objects and values; it is read only to be thrown away.
	cmd.Stderr = io.Discard
	err := cmd.Run()
	switch {
	case out.over:
		return "", transfer.Errorf(transfer.CodeDumpFailed, false, "the schema dump is larger than %d bytes", MaxDumpBytes)
	case ctx.Err() != nil && errors.Is(ctx.Err(), context.DeadlineExceeded):
		return "", transfer.Errorf(transfer.CodeDumpFailed, true, "the schema dump took too long")
	case ctx.Err() != nil:
		return "", ctx.Err()
	case err != nil:
		return "", transfer.Errorf(transfer.CodeDumpFailed, true, "the schema dump did not finish; repeat the operation")
	}
	return out.b.String(), nil
}

// capBuffer keeps the output up to a limit and remembers that it went over.
type capBuffer struct {
	b    bytes.Buffer
	max  int
	over bool
}

func (c *capBuffer) Write(p []byte) (int, error) {
	if c.b.Len()+len(p) > c.max {
		c.over = true
		return 0, errors.New("the output is larger than the cap")
	}
	return c.b.Write(p)
}
