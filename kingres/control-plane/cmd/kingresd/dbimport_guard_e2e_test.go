// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package main

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ops"
)

// The review of phase 14c: what the importing role may be, what happens to a session of a member of it, and a transaction that is prepared in the private database.

// A role with a member that can log in is refused as an importer, at the request. A role with CREATEROLE is not (a role of kingres_admin made through the API has it).
func TestDatabaseImportRefusesAnImporterThatCanReachOtherRoles(t *testing.T) {
	e, off := transferOn(t)
	defer off()
	base := smallArchive(t, e, "dbimpref")
	imp := importerOf(t, e)
	sc := e.serviceConn(t, "postgres")
	defer func() { _ = sc.Close(context.Background()) }()
	if _, err := sc.Exec(context.Background(), "create role dbx_member login in role "+importerName); err != nil {
		t.Fatal(err)
	}
	up := imp.uploadArchive(t, base)
	if st, out := imp.importDB(t, map[string]any{"uploadId": up, "database": "dbimpref_x"}); st != 403 || problemCode(out) != "forbidden" {
		t.Fatalf("a role with a member that can log in: %d %v", st, out)
	}
	if _, err := sc.Exec(context.Background(), "drop role dbx_member"); err != nil {
		t.Fatal(err)
	}
	imp.importOK(t, map[string]any{"uploadId": up, "database": "dbimpref_x"})
	if e.databaseExists(t, "dbimpref_y") || len(e.waitNoPrivate(t)) != 0 {
		t.Error("something is left")
	}
}

// A login role that is a member of the importer is a path to the private database as the importer. It cannot be in at the moment the access is withdrawn:
// the import is refused when such a member appears, the session of the member is ended, and nothing is left.
func TestDatabaseImportEndsTheSessionOfAMember(t *testing.T) {
	e, off := transferOn(t)
	defer off()
	imp := importerOf(t, e)
	base := smallArchive(t, e, "dbmember")
	up := imp.uploadArchive(t, base)
	sc := e.serviceConn(t, "postgres")
	defer func() { _ = sc.Close(context.Background()) }()
	if _, err := sc.Exec(context.Background(), "drop role if exists dbx_sub; create role dbx_sub login password 'sub-Passw0rd-1'"); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var held *pgx.Conn
	var heldErr error
	ops.ImportHook = func(ctx context.Context, step, private string) error {
		mu.Lock()
		defer mu.Unlock()
		switch step {
		case "Load the data":
			if held != nil {
				return nil
			}
			// The member joins the importer and opens a session while the importer has access.
			if _, err := sc.Exec(ctx, "grant "+importerName+" to dbx_sub"); err != nil {
				return err
			}
			cfg, _ := pgx.ParseConfig("")
			cfg.Host, cfg.Port, cfg.Database, cfg.User, cfg.Password = "127.0.0.1", uint16(e.pgPort), private, "dbx_sub", "sub-Passw0rd-1" //nolint:gosec // test port
			cfg.TLSConfig, cfg.Fallbacks = nil, nil
			held, heldErr = pgx.ConnectConfig(ctx, cfg)
		}
		return nil
	}
	defer func() { ops.ImportHook = nil }()
	st, out := imp.importDB(t, map[string]any{"uploadId": up, "database": "dbmember_x"})
	if st != 202 {
		t.Fatalf("%d %v", st, out)
	}
	done := e.waitTask(t, out)
	// The compensation of a failed task runs after the task is recorded as failed.
	names := e.waitNoPrivate(t)
	mu.Lock()
	defer mu.Unlock()
	if held != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if _, err := held.Exec(ctx, "select 1"); err == nil {
			t.Error("the session of the member is still alive")
		}
		_ = held.Close(context.Background())
	} else {
		t.Logf("the member could not connect (%v): it was refused before it could", heldErr)
	}
	if done["state"] != "failed" || taskCode(done) != "forbidden" {
		t.Errorf("the task: %v %q (%s)", done["state"], taskCode(done), taskMessage(done))
	}
	if len(names) != 0 || e.databaseExists(t, "dbmember_x") {
		t.Errorf("left: %v", names)
	}
	_, _ = sc.Exec(context.Background(), "drop role if exists dbx_sub")
}

// A prepared transaction in the private database does not hold it: the next step, the rename and the drop of a failed import roll it back.
func TestDatabaseImportRollsBackAPreparedTransaction(t *testing.T) {
	e, off := transferOn(t)
	defer off()
	imp := importerOf(t, e)
	base := smallArchive(t, e, "dbprep")
	prepare := func(ctx context.Context, private string) error {
		cfg, _ := pgx.ParseConfig("")
		cfg.Host, cfg.Port, cfg.Database, cfg.User, cfg.Password = "127.0.0.1", uint16(e.pgPort), private, importerName, importerPass //nolint:gosec // test port
		cfg.TLSConfig, cfg.Fallbacks = nil, nil
		c, err := pgx.ConnectConfig(ctx, cfg)
		if err != nil {
			return err
		}
		defer func() { _ = c.Close(ctx) }()
		for _, q := range []string{"begin", "select 1", fmt.Sprintf("prepare transaction 'pin_%d'", time.Now().UnixNano())} {
			if _, err := c.Exec(ctx, q); err != nil {
				return err
			}
		}
		return nil
	}
	var once sync.Once
	var prepErr error
	ops.ImportHook = func(ctx context.Context, step, private string) error {
		if step == "Load the data" {
			once.Do(func() { prepErr = prepare(ctx, private) })
		}
		return nil
	}
	defer func() { ops.ImportHook = nil }()
	up := imp.uploadArchive(t, base)
	imp.importOK(t, map[string]any{"uploadId": up, "database": "dbprep_ok"})
	if prepErr != nil {
		t.Fatalf("the prepared transaction could not be made: %v", prepErr)
	}
	sc := e.serviceConn(t, "postgres")
	defer func() { _ = sc.Close(context.Background()) }()
	if got := rowsText(t, sc, "select count(*) from pg_prepared_xacts"); got != "[0]" {
		t.Errorf("prepared transactions after the import: %s", got)
	}
	// A failed import drops the database although a transaction is prepared in it (the final name is taken at the last moment).
	once = sync.Once{}
	prepErr = nil
	ops.ImportHook = func(ctx context.Context, step, private string) error {
		if step == "Load the data" {
			once.Do(func() { prepErr = prepare(ctx, private) })
		}
		if step == "Take the final name" {
			_, err := sc.Exec(ctx, "create database dbprep_x")
			return err
		}
		return nil
	}
	imp.importFails(t, map[string]any{"uploadId": up, "database": "dbprep_x"}, "forbidden")
	if prepErr != nil {
		t.Fatalf("the prepared transaction could not be made: %v", prepErr)
	}
	if names := e.waitNoPrivate(t); len(names) != 0 {
		t.Errorf("a prepared transaction kept the database: %v", names)
	}
}

// An importer that has CREATEROLE (as the API makes it) makes a login role in the middle of the import and opens sessions, as itself and as the new role, in the
// private database. None of them can touch anything during a service phase: the right to connect is withdrawn from everyone, every session is ended, and what is
// left of the import is either a finished database or nothing.
func TestDatabaseImportOfACreateRoleImporterThatMakesALoginRoleMidImport(t *testing.T) {
	e, off := transferOn(t)
	defer off()
	imp := importerOf(t, e)
	base := smallArchive(t, e, "dbcr")
	up := imp.uploadArchive(t, base)
	sc := e.serviceConn(t, "postgres")
	defer func() { _ = sc.Close(context.Background()) }()
	var hasCreateRole bool
	if err := sc.QueryRow(context.Background(), "select rolcreaterole from pg_roles where rolname = $1", importerName).Scan(&hasCreateRole); err != nil || !hasCreateRole {
		t.Fatalf("the importer must have CREATEROLE as the API makes it: %v %v", hasCreateRole, err)
	}
	connect := func(ctx context.Context, db, user, pass string) (*pgx.Conn, error) {
		cfg, _ := pgx.ParseConfig("")
		cfg.Host, cfg.Port, cfg.Database, cfg.User, cfg.Password = "127.0.0.1", uint16(e.pgPort), db, user, pass //nolint:gosec // test port
		cfg.TLSConfig, cfg.Fallbacks = nil, nil
		return pgx.ConnectConfig(ctx, cfg)
	}
	var mu sync.Mutex
	var held []*pgx.Conn
	var once sync.Once
	var hookErr error
	ops.ImportHook = func(ctx context.Context, step, private string) error {
		if step != "Load the data" {
			return nil
		}
		once.Do(func() {
			mu.Lock()
			defer mu.Unlock()
			// The importer's own session in the private database and in the cluster database.
			own, err := connect(ctx, private, importerName, importerPass)
			if err != nil {
				hookErr = err
				return
			}
			held = append(held, own)
			adm, err := connect(ctx, "postgres", importerName, importerPass)
			if err != nil {
				hookErr = err
				return
			}
			defer func() { _ = adm.Close(ctx) }()
			// With CREATEROLE the importer makes a login role, tries to make it a member of itself and to open a session of it in the private database.
			if _, err := adm.Exec(ctx, "create role dbx_cr login password 'cr-Passw0rd-1'"); err != nil {
				hookErr = err
				return
			}
			_, _ = adm.Exec(ctx, "grant "+importerName+" to dbx_cr")
			_, _ = adm.Exec(ctx, "grant connect on database "+private+" to dbx_cr")
			if c, err := connect(ctx, private, "dbx_cr", "cr-Passw0rd-1"); err == nil {
				held = append(held, c)
			}
		})
		return nil
	}
	defer func() { ops.ImportHook = nil }()
	st, out := imp.importDB(t, map[string]any{"uploadId": up, "database": "dbcr_x"})
	if st != 202 {
		t.Fatalf("%d %v", st, out)
	}
	done := e.waitTask(t, out)
	names := e.waitNoPrivate(t)
	mu.Lock()
	defer mu.Unlock()
	if hookErr != nil {
		t.Fatalf("the importer could not make its role or its sessions: %v", hookErr)
	}
	for i, c := range held {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		if _, err := c.Exec(ctx, "select 1"); err == nil {
			t.Errorf("session %d of the importer or of its role is still alive after the import", i)
		}
		cancel()
		_ = c.Close(context.Background())
	}
	if len(names) != 0 {
		t.Errorf("a private database is left: %v", names)
	}
	// Either the member that logs in was found (the import is refused) or the import finished; in both cases the final name and the private name agree.
	switch done["state"] {
	case "failed":
		if taskCode(done) != "forbidden" {
			t.Errorf("the task failed with %q (%s)", taskCode(done), taskMessage(done))
		}
		if e.databaseExists(t, "dbcr_x") {
			t.Error("a failed import left the database")
		}
	case "done":
		if !e.databaseExists(t, "dbcr_x") {
			t.Error("a finished import left no database")
		}
	default:
		t.Errorf("the task: %v", done["state"])
	}
	_, _ = sc.Exec(context.Background(), "drop role if exists dbx_cr")
}

// The rows of a table are counted as the importing role: a table that is swapped for a view that calls a function of the role, just before the count, runs the
// function as the role and never as the service superuser. The function reports the user that runs it.
func TestDatabaseImportCountsTheRowsAsTheRole(t *testing.T) {
	e, off := transferOn(t)
	defer off()
	imp := importerOf(t, e)
	base := smallArchive(t, e, "dbcount")
	up := imp.uploadArchive(t, base)
	var mu sync.Mutex
	var seen []string
	var once sync.Once
	var hookErr error
	var listener *pgx.Conn
	ops.ImportHook = func(ctx context.Context, step, private string) error {
		if step != "Set the sequences" {
			return nil
		}
		once.Do(func() {
			cfg, _ := pgx.ParseConfig("")
			cfg.Host, cfg.Port, cfg.Database, cfg.User, cfg.Password = "127.0.0.1", uint16(e.pgPort), private, importerName, importerPass //nolint:gosec // test port
			cfg.TLSConfig, cfg.Fallbacks = nil, nil
			l, err := pgx.ConnectConfig(ctx, cfg)
			if err != nil {
				hookErr = err
				return
			}
			listener = l
			c, err := pgx.ConnectConfig(ctx, cfg)
			if err != nil {
				hookErr = err
				return
			}
			defer func() { _ = c.Close(ctx) }()
			for _, q := range []string{"listen spy",
				"alter table public.victim rename to victim_real",
				"create function public.spy() returns boolean language plpgsql as $f$ begin perform pg_notify('spy', current_user::text); return true; end $f$",
				"create view public.victim as select id, v from public.victim_real where public.spy()"} {
				if _, err := l.Exec(ctx, q); err != nil {
					hookErr = fmt.Errorf("%s: %w", q, err)
					return
				}
			}
			go func() {
				for {
					wctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
					n, err := l.WaitForNotification(wctx)
					cancel()
					if err != nil {
						return
					}
					mu.Lock()
					seen = append(seen, n.Payload)
					mu.Unlock()
				}
			}()
		})
		return nil
	}
	defer func() { ops.ImportHook = nil }()
	st, out := imp.importDB(t, map[string]any{"uploadId": up, "database": "dbcount_x"})
	if st != 202 {
		t.Fatalf("%d %v", st, out)
	}
	_ = e.waitTask(t, out)
	e.waitNoPrivate(t)
	time.Sleep(time.Second)
	mu.Lock()
	defer mu.Unlock()
	if hookErr != nil {
		t.Fatal(hookErr)
	}
	if listener != nil {
		_ = listener.Close(context.Background())
	}
	if len(seen) == 0 {
		t.Fatal("the function was never run: the count did not touch the swapped table")
	}
	for _, who := range seen {
		if who != importerName {
			t.Errorf("a function of the importing role ran as %q", who)
		}
	}
}
