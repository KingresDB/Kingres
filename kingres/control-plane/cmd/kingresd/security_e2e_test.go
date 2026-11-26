// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package main

// End-to-end tests of the findings CP-01 to CP-05 of the security review
// (docs/reviews/2026-11-14-kingresd-control-plane.md). Each is the review's
// own reproduction, run against a real kingresd and instance.

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// CP-01: a database name with libpq keywords in it must not redirect the
// service role's connection: a listener the name points at gets no connection.
func TestCP01DatabaseNameCannotRedirectTheServiceConnection(t *testing.T) {
	e := shared(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	var hits atomic.Int32
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			hits.Add(1)
			_ = c.Close()
		}
	}()
	port := ln.Addr().(*net.TCPAddr).Port
	reader := e.accountWithRole(t, "cp01_reader", "kingres_readonly")
	evil := "x host=127.0.0.1 port=" + fmt.Sprint(port)
	for _, p := range []string{"/ledger/digests", ""} {
		path := "/databases/" + url.PathEscape(evil) + p
		st, out, _ := e.as(reader).call("GET", path, nil, nil)
		if st == 200 {
			t.Errorf("%s: answered 200 for a database that does not exist: %v", path, out)
		}
	}
	time.Sleep(300 * time.Millisecond)
	if n := hits.Load(); n != 0 {
		t.Fatalf("the service role connected to the host named in the database name %d time(s)", n)
	}
}

// CP-02: the review's scenario, then a role disabled and a role dropped in SQL
// directly (caught by the periodic check), then a password change.
func TestCP02SessionsFollowTheCurrentRole(t *testing.T) {
	e := shared(t)
	pw := "Passw0rd-carol-xyz"
	e.expect(t, 201, "POST", "/roles", map[string]any{"name": "carol", "password": pw, "clusterRoles": []string{"kingres_admin"}})
	ct, ok := e.loginAs("carol", pw)
	if !ok {
		t.Fatal("carol cannot sign in")
	}
	if st, _, _ := e.as(ct).call("GET", "/roles", nil, nil); st != 200 {
		t.Fatalf("carol as admin: %d", st)
	}
	e.expect(t, 200, "PUT", "/roles/carol/cluster-roles", map[string]any{"clusterRoles": []string{}})
	st, out, _ := e.as(ct).call("PUT", "/roles/carol/cluster-roles", map[string]any{"clusterRoles": []string{"kingres_admin"}}, nil)
	if st != 401 {
		t.Fatalf("a demoted administrator's token still works: %d %v", st, out)
	}
	if st, _, _ := e.as(ct).call("GET", "/roles", nil, nil); st != 401 {
		t.Fatalf("a demoted administrator can still read roles: %d", st)
	}
	e.expect(t, 200, "GET", "/roles/carol", nil)

	// A role disabled directly in SQL, outside the API: seen within the cache time.
	dave := e.accountWithRole(t, "dave", "kingres_admin")
	if st, _, _ := e.as(dave).call("GET", "/roles", nil, nil); st != 200 {
		t.Fatalf("dave: %d", st)
	}
	svc := e.serviceConn(t, "postgres")
	if _, err := svc.Exec(context.Background(), "ALTER ROLE dave NOLOGIN"); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, e.as(dave), "GET", "/roles", 401, "a role disabled in SQL")

	// A role dropped through the API: its token stops at once.
	erin := e.accountWithRole(t, "erin", "kingres_ops")
	e.expect(t, 204, "DELETE", "/roles/erin", nil)
	if st, _, _ := e.as(erin).call("GET", "/roles", nil, nil); st != 401 {
		t.Fatalf("a dropped role's token still works: %d", st)
	}

	// A password change ends the sessions of the role.
	frank := e.accountWithRole(t, "frank", "kingres_ops")
	e.expect(t, 200, "PATCH", "/roles/frank", map[string]any{"password": "A-New-Passw0rd-1"})
	if st, _, _ := e.as(frank).call("GET", "/databases", nil, nil); st != 401 {
		t.Fatalf("the session survived a password change: %d", st)
	}
	if _, ok := e.loginAs("frank", "A-New-Passw0rd-1"); !ok {
		t.Fatal("frank cannot sign in with the new password")
	}
}

func waitStatus(t *testing.T, e *cp, method, path string, want int, what string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	var st int
	for time.Now().Before(deadline) {
		st, _, _ = e.call(method, path, nil, nil)
		if st == want {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("%s: status %d after 15 s, want %d", what, st, want)
}

// pgLogs returns the text of the instance's log files.
func (e *cp) pgLogs(t *testing.T) string {
	t.Helper()
	files, _ := filepath.Glob(filepath.Join(e.dataDir, "instances", "1", "pgdata", "log", "*.log"))
	var sb strings.Builder
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err == nil {
			sb.Write(b)
		}
	}
	return sb.String()
}

// CP-03: the text of a failed workbench statement is not in PostgreSQL's log
// (so it cannot be served by /logs), and the log operations need more than
// the readonly role.
func TestCP03FailedStatementTextIsNotLogged(t *testing.T) {
	e := shared(t)
	reader := e.accountWithRole(t, "cp03_dev", "kingres_readonly")
	secret := "S3cr3t-In-Workbench-cp03"
	st, out, _ := e.as(reader).call("POST", "/query", map[string]any{"database": "postgres", "sql": "create user zz password '" + secret + "' nosuchoption"}, nil)
	if st != 422 {
		t.Fatalf("the statement should fail with sql_error: %d %v", st, out)
	}
	time.Sleep(500 * time.Millisecond)
	if strings.Contains(e.pgLogs(t), secret) {
		t.Fatal("the text of the failed statement is in PostgreSQL's log")
	}
	// Whoever may read the logs, the secret is not there; and readonly may not read them.
	for _, p := range []string{"/logs", "/this-server/logs"} {
		if st, _, _ := e.as(reader).call("GET", p, nil, nil); st != 403 {
			t.Errorf("GET %s as kingres_readonly: %d, want 403", p, st)
		}
	}
	if st, out, _ := e.call("GET", "/logs?limit=200", nil, nil); st != 200 || strings.Contains(fmt.Sprint(out), secret) {
		t.Errorf("GET /logs as an administrator: %d, secret present: %v", st, strings.Contains(fmt.Sprint(out), secret))
	}
}

// CP-04: the review's statement is refused without a memory spike, many
// medium rows are cut at the byte budget, and the statements run at once are
// bounded.
func TestCP04WorkbenchResultsAreBounded(t *testing.T) {
	e := shared(t)
	reader := e.accountWithRole(t, "cp04_dev", "kingres_readonly")
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	st, out, _ := e.as(reader).call("POST", "/query", map[string]any{"database": "postgres", "sql": "select repeat('x', 60000000) from generate_series(1,10)", "rowLimit": 10}, nil)
	if st != 422 {
		t.Fatalf("a 60 MB value: %d, want 422: %.200v", st, out)
	}
	runtime.ReadMemStats(&after)
	if grown := after.TotalAlloc - before.TotalAlloc; grown > 400<<20 {
		t.Errorf("one refused statement allocated %d MB", grown>>20)
	}
	st, out, _ = e.as(reader).call("POST", "/query", map[string]any{"database": "postgres", "sql": "select repeat('y', 500000) from generate_series(1,200)", "rowLimit": 200}, nil)
	if st != 200 || out["truncated"] != true {
		t.Fatalf("200 rows of 500 kB: %d truncated=%v", st, out["truncated"])
	}
	if rows, _ := out["rows"].([]any); len(rows) == 0 || len(rows) > 40 {
		t.Errorf("rows returned: %d (the byte budget is 16 MiB)", len(rows))
	}
}

// CP-05: dangerous predefined roles cannot be granted through the API, and
// the workbench refuses to run as a role that has them anyway.
func TestCP05DangerousMembershipsAreRefused(t *testing.T) {
	e := shared(t)
	pw := "Passw0rd-cp05-xyz"
	e.expect(t, 201, "POST", "/roles", map[string]any{"name": "cp05_user", "password": pw, "clusterRoles": []string{"kingres_readonly"}})
	for _, r := range []string{"pg_execute_server_program", "pg_read_server_files", "pg_write_server_files", "pg_read_server_logs", "pg_some_future_role"} {
		st, out, _ := e.call("PUT", "/roles/cp05_user/memberships", map[string]any{"memberOf": []string{r}}, nil)
		if st != 400 {
			t.Errorf("membership in %s: %d %v", r, st, out)
		}
		st, out, _ = e.call("POST", "/roles", map[string]any{"name": "cp05_new_" + strings.TrimPrefix(r, "pg_"), "memberOf": []string{r}}, nil)
		if st != 400 {
			t.Errorf("creating a role that is a member of %s: %d %v", r, st, out)
		}
	}
	// A role with the superuser or replication attribute is no membership target either.
	svc := e.serviceConn(t, "postgres")
	if _, err := svc.Exec(context.Background(), "CREATE ROLE cp05_repl NOLOGIN REPLICATION"); err != nil {
		t.Fatal(err)
	}
	if st, out, _ := e.call("PUT", "/roles/cp05_user/memberships", map[string]any{"memberOf": []string{"cp05_repl"}}, nil); st != 400 {
		t.Errorf("membership in a replication role: %d %v", st, out)
	}
	if st, out, _ := e.call("PUT", "/roles/cp05_user/memberships", map[string]any{"memberOf": []string{"kingresd"}}, nil); st != 400 {
		t.Errorf("membership in the service role: %d %v", st, out)
	}
	// Granted in SQL by a superuser (outside the panel's model): the workbench refuses.
	if _, err := svc.Exec(context.Background(), "GRANT pg_execute_server_program TO cp05_user"); err != nil {
		t.Fatal(err)
	}
	tok, ok := e.loginAs("cp05_user", pw)
	if !ok {
		t.Fatal("login")
	}
	st, out, _ := e.as(tok).call("POST", "/query", map[string]any{"database": "postgres", "sql": "select 1"}, nil)
	if st != 403 {
		t.Errorf("workbench as a role with pg_execute_server_program: %d %v", st, out)
	}
}
