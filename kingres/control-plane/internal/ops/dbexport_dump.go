// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package ops

// The schema dump of a database export (design "The task database.export", step 2): pg_dump of the same PostgreSQL build as the instance, run by the service
// role, schema only, in the snapshot of the transaction that read the catalog. No client value is in any argument, the environment is clean, the password is in a
// file of mode 0600 in the staging directory that is removed afterwards, the output is read through a pipe with a size cap, and what pg_dump writes to its error
// output reaches the log of the server only, as a bounded tail.

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/transfer"
)

// MaxDumpBytes is the largest output of one run of pg_dump (design: 64 MiB).
const MaxDumpBytes = 64 << 20

// MinDumpMajor is the oldest pg_dump that the export accepts; it must also be the major of the server.
const MinDumpMajor = 18

// DumpTimeout is the longest one run of pg_dump may take.
var DumpTimeout = 10 * time.Minute

// dumpArgs are the fixed arguments of pg_dump. None comes from a client. The connection is given by the environment, and the sections are two runs.
func dumpArgs(section, snapshot string) []string {
	return []string{"--schema-only", "--no-owner", "--no-tablespaces", "--no-publications", "--no-subscriptions", "--no-security-labels", "--no-privileges",
		"--section=" + section, "--snapshot=" + snapshot,
		"--exclude-schema=kingres", "--exclude-schema=kingres_transfer", "--lock-wait-timeout=60s"}
}

// dumpEnv is the environment of pg_dump: the connection of the service role and nothing else. The password is in a file (a process of the host can read the
// environment of another process of its user), and the search path of the session is the catalog's.
func dumpEnv(cfg *pgx.ConnConfig, passfile string) []string {
	return []string{"PGHOST=" + cfg.Host, "PGPORT=" + strconv.Itoa(int(cfg.Port)), "PGUSER=" + cfg.User, "PGPASSFILE=" + passfile, "PGDATABASE=" + cfg.Database,
		"PGSSLMODE=disable", "PGCONNECT_TIMEOUT=15", "PGAPPNAME=kingresd-export", "PGOPTIONS=-c search_path=pg_catalog", "PATH=" + os.Getenv("PATH")}
}

// writePassfile writes the line of a password file for the connection into dir, with mode 0600, and returns its name; the caller removes it.
func writePassfile(dir string, cfg *pgx.ConnConfig) (string, error) {
	esc := strings.NewReplacer(`\`, `\\`, `:`, `\:`)
	f, err := os.CreateTemp(dir, "pgpass-")
	if err != nil {
		return "", err
	}
	name := f.Name()
	line := strings.Join([]string{esc.Replace(cfg.Host), strconv.Itoa(int(cfg.Port)), esc.Replace(cfg.Database), esc.Replace(cfg.User), esc.Replace(cfg.Password)}, ":") + "\n"
	if err := os.Chmod(name, 0o600); err != nil {
		_ = f.Close()
		_ = os.Remove(name)
		return "", err
	}
	if _, err := f.WriteString(line); err != nil {
		_ = f.Close()
		_ = os.Remove(name)
		return "", err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(name)
		return "", err
	}
	return name, nil
}

var dumpVersionRE = regexp.MustCompile(`^pg_dump \(PostgreSQL\) ([0-9]+)`)

// dumpMajor asks the program of the instance for its version.
func dumpMajor(ctx context.Context, prog string) (int, error) {
	out, err := exec.CommandContext(ctx, prog, "--version").Output() //nolint:gosec // a fixed program and argument
	if err != nil {
		return 0, err
	}
	m := dumpVersionRE.FindStringSubmatch(strings.TrimSpace(string(out)))
	if m == nil {
		return 0, errors.New("the version of pg_dump is not understood")
	}
	return strconv.Atoi(m[1])
}

// runDump runs pg_dump of the build of the instance (from its binary directory, never from PATH) for one section and returns what it wrote. wantMajor is the
// major of the server (0: not checked); pg_dump must be of that major and not older than MinDumpMajor.
func (o *Ops) runDump(ctx context.Context, cfg *pgx.ConnConfig, section, snapshot string, wantMajor int) (string, error) {
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
	if major, err := dumpMajor(ctx, prog); err != nil || major < MinDumpMajor || (wantMajor > 0 && major != wantMajor) {
		return "", transfer.Errorf(transfer.CodeDumpFailed, false, "pg_dump is not of the version of the server (at least %d, and the major of the server)", MinDumpMajor)
	}
	pass := ""
	if o.Transfer.Dir != "" {
		if err := os.MkdirAll(o.Transfer.Dir, 0o700); err == nil {
			pass, _ = writePassfile(o.Transfer.Dir, cfg)
		}
	}
	if pass == "" {
		return "", transfer.Errorf(transfer.CodeDumpFailed, true, "the password file of pg_dump could not be written in the staging directory")
	}
	defer func() { _ = os.Remove(pass) }()
	cmd := exec.CommandContext(ctx, prog, dumpArgs(section, snapshot)...) //nolint:gosec // a fixed program and fixed arguments
	cmd.Env = dumpEnv(cfg, pass)
	cmd.Dir = dir
	out := &capBuffer{max: MaxDumpBytes}
	cmd.Stdout = out
	// What pg_dump says on its error output can name objects: a bounded tail of it goes to the log of the server for the operator, and nowhere else.
	tail := &tailBuffer{max: 2048}
	cmd.Stderr = tail
	err := cmd.Run()
	if err != nil && o.Log != nil {
		o.Log.Warn("pg_dump of a database export failed", "section", section, "stderr_tail", tail.String())
	}
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

// tailBuffer keeps the last bytes written to it.
type tailBuffer struct {
	b   []byte
	max int
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.b = append(t.b, p...)
	if len(t.b) > t.max {
		t.b = t.b[len(t.b)-t.max:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string { return string(t.b) }

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
