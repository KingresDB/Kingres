// SPDX-License-Identifier: PostgreSQL

package main

// End-to-end tests of the operations that change the cluster, run against a
// real kingresd and a real PostgreSQL instance (see e2e_test.go). Every
// response is validated against the schemas of the contract.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/contracttest"
)

var (
	checkerOnce sync.Once
	checker     *contracttest.Checker
)

func chk(t testing.TB) *contracttest.Checker {
	checkerOnce.Do(func() { checker = contracttest.NewChecker(t) })
	return checker
}

// valid fails the test unless body matches the 200/201 schema of op.
func valid(t testing.TB, op, status string, body map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(body)
	if err := chk(t).Response(op, status, raw); err != nil {
		t.Errorf("%s %s: response does not match the contract: %v\n%s", op, status, err, raw)
	}
}

// validTask checks a Task body (202 responses reference the AcceptedTask response).
func validTask(t testing.TB, body map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(body)
	if err := chk(t).Schema("Task", raw); err != nil {
		t.Errorf("task does not match the contract: %v\n%s", err, raw)
	}
}

// waitTask polls a task until it is done or failed.
func (e *cp) waitTask(t testing.TB, task map[string]any) map[string]any {
	t.Helper()
	validTask(t, task)
	id := fmt.Sprint(int64(task["id"].(float64)))
	deadline := time.Now().Add(3 * time.Minute)
	for {
		st, cur, _ := e.call("GET", "/tasks/"+id, nil, nil)
		if st != 503 { // 503: the instance is restarting under the task
			validTask(t, cur)
			if s := cur["state"]; s == "done" || s == "failed" {
				return cur
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("task %s did not finish: %v", id, cur)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func (e *cp) mustTask(t testing.TB, method, path string, body any) map[string]any {
	t.Helper()
	st, out, _ := e.call(method, path, body, nil)
	if st != 202 {
		t.Fatalf("%s %s: status %d: %v", method, path, st, out)
	}
	done := e.waitTask(t, out)
	if done["state"] != "done" {
		t.Fatalf("%s %s: task failed: %v", method, path, done["error"])
	}
	return done
}

// tokens creates roles with the given cluster role and returns bearer tokens.
func (e *cp) accountWithRole(t testing.TB, name, clusterRole string) string {
	t.Helper()
	pw := "Passw0rd-" + name + "-xyz"
	st, out, _ := e.call("POST", "/roles", map[string]any{"name": name, "password": pw, "clusterRoles": []string{clusterRole}}, nil)
	if st != 201 {
		t.Fatalf("creating %s: %d %v", name, st, out)
	}
	tok, ok := e.loginAs(name, pw)
	if !ok {
		t.Fatalf("login as %s", name)
	}
	return tok
}

func problemCode(out map[string]any) string { c, _ := out["code"].(string); return c }

func TestRolesLifecycle(t *testing.T) {
	e := shared(t)
	out := e.expect(t, 201, "POST", "/roles", map[string]any{
		"name": "alice", "password": markerPassword, "canCreateDatabase": true,
		"connectionLimit": 5, "validUntil": "2099-01-01T00:00:00Z", "clusterRoles": []string{"kingres_readonly"},
	})
	valid(t, "createRole", "201", out)
	if out["name"] != "alice" || out["canLogin"] != true || out["canCreateDatabase"] != true || out["connectionLimit"] != float64(5) {
		t.Errorf("role %v", out)
	}
	if cr, _ := out["clusterRoles"].([]any); len(cr) != 1 || cr[0] != "kingres_readonly" {
		t.Errorf("cluster roles %v", out["clusterRoles"])
	}
	// The new role signs in with the password (a SCRAM verifier was set).
	if _, ok := e.loginAs("alice", markerPassword); !ok {
		t.Error("alice cannot sign in with her password")
	}
	if st, o, _ := e.call("POST", "/roles", map[string]any{"name": "alice"}, nil); st != 409 || problemCode(o) != "already_exists" {
		t.Errorf("duplicate: %d %v", st, o)
	}
	// Update: disable, change the limit, clear the expiry.
	out = e.expect(t, 200, "PATCH", "/roles/alice", map[string]any{"canLogin": false, "connectionLimit": -1, "validUntil": nil})
	valid(t, "updateRole", "200", out)
	if out["canLogin"] != false || out["validUntil"] != nil {
		t.Errorf("updated %v", out)
	}
	if _, ok := e.loginAs("alice", markerPassword); ok {
		t.Error("a disabled role signed in")
	}
	e.expect(t, 200, "PATCH", "/roles/alice", map[string]any{"canLogin": true})
	// Memberships.
	e.expect(t, 201, "POST", "/roles", map[string]any{"name": "team_a", "canLogin": false})
	out = e.expect(t, 200, "PUT", "/roles/alice/memberships", map[string]any{"memberOf": []string{"team_a"}})
	valid(t, "setRoleMemberships", "200", out)
	if m, _ := out["memberOf"].([]any); len(m) != 1 || m[0] != "team_a" {
		t.Errorf("memberships %v", out["memberOf"])
	}
	out = e.expect(t, 200, "PUT", "/roles/alice/memberships", map[string]any{"memberOf": []string{}})
	if m, _ := out["memberOf"].([]any); len(m) != 0 {
		t.Errorf("memberships after clearing %v", out["memberOf"])
	}
	if st, o, _ := e.call("PUT", "/roles/alice/memberships", map[string]any{"memberOf": []string{"kingres_admin"}}, nil); st != 400 && st != 422 {
		t.Errorf("a cluster role through memberships: %d %v", st, o)
	}
	if st, _, _ := e.call("PUT", "/roles/alice/memberships", map[string]any{"memberOf": []string{"no_such_role"}}, nil); st != 400 && st != 422 {
		t.Errorf("unknown parent: %d", st)
	}
	// Cluster roles.
	out = e.expect(t, 200, "PUT", "/roles/alice/cluster-roles", map[string]any{"clusterRoles": []string{"kingres_ops", "kingres_stats"}})
	valid(t, "setRoleClusterRoles", "200", out)
	if cr, _ := out["clusterRoles"].([]any); len(cr) != 2 {
		t.Errorf("cluster roles %v", out["clusterRoles"])
	}
	// Delete.
	e.expect(t, 204, "DELETE", "/roles/alice", nil)
	e.expect(t, 204, "DELETE", "/roles/team_a", nil)
	if st, _, _ := e.call("GET", "/roles/alice", nil, nil); st != 404 {
		t.Errorf("deleted role: %d", st)
	}
	if st, _, _ := e.call("DELETE", "/roles/alice", nil, nil); st != 404 {
		t.Errorf("deleting twice: %d", st)
	}
}

func TestRoleRefusals(t *testing.T) {
	e := shared(t)
	// The last administrator cannot be removed, disabled or demoted, and a
	// role cannot disable or drop itself.
	for name, call := range map[string]func() (int, map[string]any){
		"drop self": func() (int, map[string]any) {
			st, o, _ := e.call("DELETE", "/roles/"+adminUser, nil, nil)
			return st, o
		},
		"disable self": func() (int, map[string]any) {
			st, o, _ := e.call("PATCH", "/roles/"+adminUser, map[string]any{"canLogin": false}, nil)
			return st, o
		},
		"demote": func() (int, map[string]any) {
			st, o, _ := e.call("PUT", "/roles/"+adminUser+"/cluster-roles", map[string]any{"clusterRoles": []string{"kingres_readonly"}}, nil)
			return st, o
		},
		"drop cluster role": func() (int, map[string]any) {
			st, o, _ := e.call("DELETE", "/roles/kingres_admin", nil, nil)
			return st, o
		},
	} {
		if st, o := call(); st != 409 {
			t.Errorf("%s: status %d %v", name, st, o)
		}
	}
	// With a second administrator the first can be demoted by the second but
	// not the last one.
	e.expect(t, 201, "POST", "/roles", map[string]any{"name": "admin_two", "password": "Another-Passw0rd-1", "clusterRoles": []string{"kingres_admin"}})
	tok2, ok := e.loginAs("admin_two", "Another-Passw0rd-1")
	if !ok {
		t.Fatal("second admin cannot sign in")
	}
	two := e.as(tok2)
	if st, o, _ := two.call("PUT", "/roles/"+adminUser+"/cluster-roles", map[string]any{"clusterRoles": []string{"kingres_readonly"}}, nil); st != 200 {
		t.Fatalf("demoting the first admin: %d %v", st, o)
	}
	if st, o, _ := two.call("PUT", "/roles/admin_two/cluster-roles", map[string]any{"clusterRoles": []string{}}, nil); st != 409 || problemCode(o) != "last_admin" {
		t.Errorf("demoting the last admin: %d %v", st, o)
	}
	// Restore: the second admin promotes the first again, then is dropped.
	two.expect(t, 200, "PUT", "/roles/"+adminUser+"/cluster-roles", map[string]any{"clusterRoles": []string{"kingres_admin"}})
	// Changing a role ends its sessions (CP-02): the first administrator signs in again.
	e.login(t, adminUser, adminPass)
	e.expect(t, 204, "DELETE", "/roles/admin_two", nil)
	// The built-in roles are not valid role names for creation tricks.
	if st, _, _ := e.call("POST", "/roles", map[string]any{"name": "pg_evil"}, nil); st != 400 && st != 422 {
		t.Errorf("pg_ prefix accepted: %d", st)
	}
}

// TestMutationsNeedAdmin calls every mutating operation of the security,
// database, table, network and task groups with accounts of lower roles.
func TestMutationsNeedAdmin(t *testing.T) {
	e := shared(t)
	ro := e.accountWithRole(t, "nonadm_ro", "kingres_readonly")
	ops := e.accountWithRole(t, "nonadm_ops", "kingres_ops")
	stats := e.accountWithRole(t, "nonadm_stats", "kingres_stats")
	calls := []struct {
		method, path string
		body         any
	}{
		{"POST", "/roles", map[string]any{"name": "zz1"}},
		{"PATCH", "/roles/nonadm_ro", map[string]any{"canLogin": false}},
		{"DELETE", "/roles/nonadm_ro", nil},
		{"PUT", "/roles/nonadm_ro/memberships", map[string]any{"memberOf": []string{}}},
		{"PUT", "/roles/nonadm_ro/cluster-roles", map[string]any{"clusterRoles": []string{"kingres_admin"}}},
		{"POST", "/grants", map[string]any{"grantee": "nonadm_ro", "level": "database", "database": "postgres", "privileges": []string{"connect"}}},
		{"POST", "/grants/revocations", map[string]any{"grantee": "nonadm_ro", "level": "database", "database": "postgres", "privileges": []string{"connect"}}},
		{"POST", "/grants/template-applications", map[string]any{"template": "owner", "grantee": "nonadm_ro", "database": "postgres"}},
		{"PUT", "/settings/network-access", map[string]any{"revision": 1, "tlsRequired": false, "rules": []any{}}},
		{"POST", "/databases", map[string]any{"name": "zzdb"}},
		{"PATCH", "/databases/postgres", map[string]any{"replication": map[string]any{"durability": "async"}}},
		{"DELETE", "/databases/postgres", nil},
		{"PUT", "/databases/postgres/schemas/public/tables/t/distribution", map[string]any{"method": "hash", "distributionColumn": "id"}},
		{"PUT", "/databases/postgres/schemas/public/tables/t/ledger", map[string]any{"mode": "appendOnly"}},
	}
	for _, tok := range []string{ro, ops, stats, ""} {
		who := e.as(tok)
		for _, c := range calls {
			st, out, _ := who.call(c.method, c.path, c.body, nil)
			want := 403
			if tok == "" {
				want = 401
			}
			if st != want {
				t.Errorf("%s %s with token %.4q: status %d, want %d: %v", c.method, c.path, tok, st, want, out)
			}
		}
	}
	// The query workbench needs readonly or better, not stats.
	if st, _, _ := e.as(stats).call("POST", "/query", map[string]any{"database": "postgres", "sql": "select 1"}, nil); st != 403 {
		t.Errorf("stats account on the workbench: %d", st)
	}
	for _, n := range []string{"nonadm_ro", "nonadm_ops", "nonadm_stats"} {
		e.expect(t, 204, "DELETE", "/roles/"+n, nil)
	}
}

// TestInjectionAttempts sends SQL metacharacters in every name and value the
// security and database operations accept: nothing may execute, and the cluster
// must still work afterwards.
func TestInjectionAttempts(t *testing.T) {
	e := shared(t)
	evil := []string{
		`x"; DROP DATABASE postgres; --`, `x'; DROP ROLE ` + adminUser + `; --`, "x\x00y", "a\nb", `x"y`, `'`, `\`, strings.Repeat("a", 70),
		`"; SELECT pg_sleep(30); --`, `x$$y$$`,
	}
	for _, s := range evil {
		bodies := []struct {
			method, path string
			body         any
		}{
			{"POST", "/roles", map[string]any{"name": s}},
			{"POST", "/roles", map[string]any{"name": "inj_ok", "memberOf": []string{s}}},
			{"PATCH", "/roles/" + safePath(s), map[string]any{"canLogin": false}},
			{"PUT", "/roles/inj_none/memberships", map[string]any{"memberOf": []string{s}}},
			{"POST", "/grants", map[string]any{"grantee": s, "level": "database", "database": "postgres", "privileges": []string{"connect"}}},
			{"POST", "/grants", map[string]any{"grantee": adminUser, "level": "table", "database": "postgres", "schema": s, "table": s, "privileges": []string{"select"}}},
			{"POST", "/grants", map[string]any{"grantee": adminUser, "level": "database", "database": "postgres", "privileges": []string{s}}},
			{"POST", "/grants/template-applications", map[string]any{"template": "readOnly", "grantee": s, "database": s}},
			{"POST", "/databases", map[string]any{"name": s}},
			{"POST", "/databases", map[string]any{"name": "injdb", "general": map[string]any{"encoding": s, "lcCollate": s}}},
			{"PATCH", "/databases/" + safePath(s), map[string]any{"general": map[string]any{"owner": s}}},
			{"DELETE", "/databases/" + safePath(s), nil},
			{"PUT", "/databases/postgres/schemas/" + safePath(s) + "/tables/" + safePath(s) + "/distribution", map[string]any{"method": "hash", "distributionColumn": s}},
			{"PUT", "/databases/postgres/schemas/public/tables/x/ledger", map[string]any{"mode": s}},
			{"PUT", "/settings/network-access", map[string]any{"revision": 1, "tlsRequired": false, "rules": []any{
				map[string]any{"type": "host", "databases": []string{s}, "users": []string{s}, "address": s, "method": "scram-sha-256"}}}},
		}
		for _, b := range bodies {
			st, out, _ := e.call(b.method, b.path, b.body, nil)
			if st < 400 || st >= 500 {
				t.Errorf("%s %s with %q: status %d %v", b.method, b.path, trunc(s), st, out)
			}
			if strings.Contains(fmt.Sprint(out), "SQL syntax") {
				t.Errorf("%s %s: a syntax error means the input reached the parser unquoted: %v", b.method, b.path, out)
			}
		}
	}
	// Nothing was executed: the databases and the admin role are intact.
	e.expect(t, 200, "GET", "/roles/"+adminUser, nil)
	var sleeping bool
	st, out, _ := e.call("POST", "/query", map[string]any{"database": "postgres",
		"sql": "select count(*) filter (where datname = 'postgres') as pg, count(*) filter (where datname = 'injdb') as inj from pg_database"}, nil)
	if st != 200 {
		t.Fatalf("query: %d %v", st, out)
	}
	row := out["rows"].([]any)[0].([]any)
	if row[0] != float64(1) || row[1] != float64(0) {
		t.Errorf("databases after the attempts: %v", row)
	}
	_ = sleeping
}

func safePath(s string) string {
	// Percent-encode everything, so that the path itself is well-formed.
	var sb strings.Builder
	for i := 0; i < len(s); i++ {
		fmt.Fprintf(&sb, "%%%02X", s[i])
	}
	return sb.String()
}

func trunc(s string) string {
	if len(s) > 20 {
		return s[:20] + "..."
	}
	return s
}

func TestDatabasesTablesAndLedger(t *testing.T) {
	e := shared(t)
	// Each combination of the sharding and ledger settings.
	combos := []struct {
		name    string
		sharded bool
		ledger  string
	}{
		{"db_plain", false, "off"}, {"db_shard", true, "off"}, {"db_ledger", false, "optIn"}, {"db_both", true, "optIn"},
	}
	for _, c := range combos {
		task := e.mustTask(t, "POST", "/databases", map[string]any{
			"name":        c.name,
			"sharding":    map[string]any{"enabled": c.sharded, "bucketCount": 32},
			"replication": map[string]any{"durability": "async"},
			"ledger":      map[string]any{"mode": c.ledger, "digestIntervalSeconds": 600},
		})
		if task["kind"] != "database.create" {
			t.Errorf("task kind %v", task["kind"])
		}
		db := e.expect(t, 200, "GET", "/databases/"+c.name, nil)
		valid(t, "getDatabase", "200", db)
		sh := db["sharding"].(map[string]any)
		if sh["enabled"] != c.sharded {
			t.Errorf("%s: sharding %v", c.name, sh)
		}
		if c.sharded && sh["bucketCount"] != nil && sh["bucketCount"] != float64(32) {
			t.Errorf("%s: bucket count %v", c.name, sh["bucketCount"])
		}
		if led := db["ledger"].(map[string]any); led["mode"] != c.ledger || led["digestIntervalSeconds"] != float64(600) {
			t.Errorf("%s: ledger %v", c.name, led)
		}
		if db["replication"].(map[string]any)["durability"] != "async" {
			t.Errorf("%s: durability %v", c.name, db["replication"])
		}
		if db["createdBy"] != adminUser || db["createdAt"] == nil {
			t.Errorf("%s: createdBy %v createdAt %v", c.name, db["createdBy"], db["createdAt"])
		}
	}
	if st, out, _ := e.call("POST", "/databases", map[string]any{"name": "db_plain"}, nil); st != 409 {
		t.Errorf("duplicate database: %d %v", st, out)
	}
	list := e.expect(t, 200, "GET", "/databases", nil)
	valid(t, "listDatabases", "200", list)

	// Update: durability and connection limit are synchronous.
	db := e.expect(t, 200, "PATCH", "/databases/db_plain", map[string]any{"replication": map[string]any{"durability": "syncApply"}, "general": map[string]any{"connectionLimit": 9}})
	valid(t, "updateDatabase", "200", db)
	if db["replication"].(map[string]any)["durability"] != "syncApply" || db["general"].(map[string]any)["connectionLimit"] != float64(9) {
		t.Errorf("updated %v", db)
	}
	// Sharding can be turned on, never off; the ledger cannot be turned off.
	db = e.expect(t, 200, "PATCH", "/databases/db_plain", map[string]any{"sharding": map[string]any{"enabled": true}})
	if db["sharding"].(map[string]any)["enabled"] != true {
		t.Errorf("sharding not enabled: %v", db["sharding"])
	}
	if st, _, _ := e.call("PATCH", "/databases/db_plain", map[string]any{"sharding": map[string]any{"enabled": false}}, nil); st != 400 && st != 409 && st != 422 {
		t.Errorf("sharding off: %d", st)
	}
	if st, _, _ := e.call("PATCH", "/databases/db_both", map[string]any{"ledger": map[string]any{"mode": "off"}}, nil); st != 409 {
		t.Errorf("ledger off: %d", st)
	}

	// Tables: create with psql-equivalent through the workbench, distribute, enable the ledger.
	q := func(sql string) map[string]any {
		return e.expect(t, 200, "POST", "/query", map[string]any{"database": "db_both", "sql": sql})
	}
	q("create table public.orders (id bigint primary key, customer text not null)")
	q("create table public.audit_rows (id bigint primary key, note text)")
	q("create table public.orders_keys (id bigint, k text, primary key (id), unique (k))")
	tbl := e.expect(t, 200, "GET", "/databases/db_both/schemas/public/tables/orders", nil)
	valid(t, "getTable", "200", tbl)
	cols := tbl["columns"].([]any)
	if c0 := cols[0].(map[string]any); c0["canBeDistributionColumn"] != true || len(c0["blockingKeys"].([]any)) != 0 {
		t.Errorf("column id: %v", c0)
	}
	keys := e.expect(t, 200, "GET", "/databases/db_both/schemas/public/tables/orders_keys", nil)
	for _, c := range keys["columns"].([]any) {
		cm := c.(map[string]any)
		if cm["name"] == "id" && (cm["canBeDistributionColumn"] != false || cm["distributionBlockedReason"] != "notInUniqueKey" ||
			len(cm["blockingKeys"].([]any)) != 1) {
			t.Errorf("id of orders_keys: %v", cm)
		}
	}
	// Distribute.
	if st, o, _ := e.call("PUT", "/databases/db_plain/schemas/public/tables/nope/distribution", map[string]any{"method": "hash", "distributionColumn": "id"}, nil); st != 404 {
		t.Errorf("unknown table: %d %v", st, o)
	}
	if st, _, _ := e.call("PUT", "/databases/db_both/schemas/public/tables/orders/distribution", map[string]any{"method": "hash"}, nil); st != 400 && st != 422 {
		t.Errorf("hash without column: %d", st)
	}
	if st, o, _ := e.call("PUT", "/databases/db_both/schemas/public/tables/orders/distribution", map[string]any{"method": "reference"}, nil); st != 422 || problemCode(o) != "unsupported_operation" {
		t.Errorf("reference: %d %v", st, o)
	}
	e.mustTask(t, "PUT", "/databases/db_both/schemas/public/tables/orders/distribution", map[string]any{"method": "hash", "distributionColumn": "id"})
	tbl = e.expect(t, 200, "GET", "/databases/db_both/schemas/public/tables/orders", nil)
	if cl, _ := tbl["classes"].([]any); len(cl) != 1 || cl[0] != "distributed" {
		t.Errorf("classes after distribution: %v", tbl["classes"])
	}
	// Enable the ledger on a table.
	out := e.expect(t, 200, "PUT", "/databases/db_both/schemas/public/tables/audit_rows/ledger", map[string]any{"mode": "appendOnly"})
	valid(t, "enableTableLedger", "200", out)
	if l, _ := out["ledger"].(map[string]any); l == nil || l["mode"] != "appendOnly" {
		t.Errorf("ledger after enabling: %v", out["ledger"])
	}
	if st, _, _ := e.call("PUT", "/databases/db_both/schemas/public/tables/audit_rows/ledger", map[string]any{"mode": "appendOnly"}, nil); st != 409 {
		t.Errorf("enabling twice: %d", st)
	}
	if st, o, _ := e.call("PUT", "/databases/db_shard/schemas/public/tables/nope/ledger", map[string]any{"mode": "appendOnly"}, nil); st != 404 {
		t.Errorf("unknown table: %d %v", st, o)
	}
	if st, _, _ := e.call("PUT", "/databases/db_plain/schemas/public/tables/x/ledger", map[string]any{"mode": "appendOnly"}, nil); st != 404 && st != 409 {
		t.Errorf("database without ledger: %d", st)
	}
	// Cluster summary for a statistics-only account.
	sum := e.expect(t, 200, "GET", "/cluster/summary", nil)
	valid(t, "getClusterSummary", "200", sum)

	// Drop every database; a protected one cannot be dropped.
	for _, c := range combos {
		e.mustTask(t, "DELETE", "/databases/"+c.name, nil)
		if st, _, _ := e.call("GET", "/databases/"+c.name, nil, nil); st != 404 {
			t.Errorf("%s still exists: %d", c.name, st)
		}
	}
	for _, p := range []string{"postgres", "template1", "kingres_system"} {
		if st, _, _ := e.call("DELETE", "/databases/"+p, nil, nil); st < 400 {
			t.Errorf("dropping %s: %d", p, st)
		}
	}
}

func TestGrantsAndWorkbench(t *testing.T) {
	e := shared(t)
	e.mustTask(t, "POST", "/databases", map[string]any{"name": "gdb"})
	pw := "Reader-Passw0rd-1"
	e.expect(t, 201, "POST", "/roles", map[string]any{"name": "reader", "password": pw, "clusterRoles": []string{"kingres_readonly"}})
	rtok, ok := e.loginAs("reader", pw)
	if !ok {
		t.Fatal("reader login")
	}
	reader := e.as(rtok)
	run := func(who *cp, sql string) (int, map[string]any) {
		st, out, _ := who.call("POST", "/query", map[string]any{"database": "gdb", "sql": sql}, nil)
		return st, out
	}
	if st, out := run(e, "create table public.t (id int primary key, v text); insert into public.t values (1, 'a'), (2, 'b'), (3, 'c')"); st != 422 || problemCode(out) != "sql_error" {
		t.Errorf("two statements in one request: %d %v", st, out)
	}
	if st, out := run(e, "create table public.t (id int primary key, v text)"); st != 200 {
		t.Fatalf("create table: %d %v", st, out)
	}
	if st, out := run(e, "insert into public.t values (1, 'a'), (2, 'b'), (3, 'c')"); st != 200 || out["commandTag"] != "INSERT 0 3" {
		t.Fatalf("insert: %d %v", st, out)
	}
	// The unprivileged role is stopped by PostgreSQL itself: no CONNECT, then no SELECT.
	if st, out := run(reader, "select * from public.t"); st != 403 && st != 422 {
		t.Errorf("reader before any grant: %d %v", st, out)
	}
	// Revoke the default PUBLIC connect so the CONNECT grant matters.
	e.expect(t, 200, "POST", "/query", map[string]any{"database": "postgres", "sql": "revoke connect on database gdb from public"})
	if st, out := run(reader, "select 1"); st != 403 {
		t.Errorf("reader without CONNECT: %d %v", st, out)
	}
	g := e.expect(t, 200, "POST", "/grants", map[string]any{"grantee": "reader", "level": "database", "database": "gdb", "privileges": []string{"connect"}})
	valid(t, "createGrant", "200", g)
	if st, out := run(reader, "select * from public.t"); st != 422 || problemCode(out) != "sql_error" || out["sqlState"] != "42501" {
		t.Errorf("reader without USAGE/SELECT: %d %v", st, out)
	}
	e.expect(t, 200, "POST", "/grants", map[string]any{"grantee": "reader", "level": "schema", "database": "gdb", "schema": "public", "privileges": []string{"usage"}})
	e.expect(t, 200, "POST", "/grants", map[string]any{"grantee": "reader", "level": "table", "database": "gdb", "schema": "public", "table": "t", "privileges": []string{"select"}})
	st, out := run(reader, "select id, v from public.t order by id")
	if st != 200 || out["rowCount"] != float64(3) {
		t.Fatalf("reader after grants: %d %v", st, out)
	}
	valid(t, "executeQuery", "200", out)
	if st, out := run(reader, "delete from public.t"); st != 422 || out["sqlState"] != "42501" {
		t.Errorf("reader deleting: %d %v", st, out)
	}
	// Row limit and truncation.
	_, out, _ = e.call("POST", "/query", map[string]any{"database": "gdb", "sql": "select generate_series(1, 100)", "rowLimit": 5}, nil)
	if out["truncated"] != true || out["rowCount"] != float64(5) {
		t.Errorf("row limit: %v", out)
	}
	// Timeout.
	st, out, _ = e.call("POST", "/query", map[string]any{"database": "gdb", "sql": "select pg_sleep(5)", "timeoutMs": 200}, nil)
	if st != 422 {
		t.Errorf("timeout: %d %v", st, out)
	}
	// Revoke and templates.
	e.expect(t, 204, "POST", "/grants/revocations", map[string]any{"grantee": "reader", "level": "table", "database": "gdb", "schema": "public", "table": "t", "privileges": []string{"select"}})
	if st, out := run(reader, "select * from public.t"); st != 422 || out["sqlState"] != "42501" {
		t.Errorf("reader after revoke: %d %v", st, out)
	}
	e.expect(t, 204, "POST", "/grants/template-applications", map[string]any{"template": "readOnly", "grantee": "reader", "database": "gdb"})
	if st, out := run(reader, "select count(*) from public.t"); st != 200 {
		t.Errorf("readOnly template: %d %v", st, out)
	}
	if st, out := run(reader, "insert into public.t values (9, 'z')"); st != 422 || out["sqlState"] != "42501" {
		t.Errorf("insert with readOnly: %d %v", st, out)
	}
	e.expect(t, 204, "POST", "/grants/template-applications", map[string]any{"template": "readWrite", "grantee": "reader", "database": "gdb"})
	if st, out := run(reader, "insert into public.t values (9, 'z')"); st != 200 {
		t.Errorf("insert with readWrite: %d %v", st, out)
	}
	// Invalid grants.
	for _, g := range []map[string]any{
		{"grantee": "reader", "level": "database", "database": "gdb", "privileges": []string{"select"}},
		{"grantee": "reader", "level": "table", "database": "gdb", "schema": "public", "privileges": []string{"select"}},
		{"grantee": "reader", "level": "view", "database": "gdb", "privileges": []string{"select"}},
		{"grantee": "reader", "level": "database", "database": "gdb", "privileges": []string{}},
		{"grantee": "reader", "level": "database", "database": "gdb", "privileges": []string{"CONNECT"}},
		{"grantee": "nobody_here", "level": "database", "database": "gdb", "privileges": []string{"connect"}},
		{"grantee": "reader", "level": "database", "database": "no_such_db", "privileges": []string{"connect"}},
	} {
		if st, out, _ := e.call("POST", "/grants", g, nil); st < 400 || st >= 500 {
			t.Errorf("grant %v: %d %v", g, st, out)
		}
	}
	// The workbench statement text is not in the audit file; its length and hash are.
	auditBytes, _ := os.ReadFile(filepath.Join(e.dataDir, "audit.jsonl"))
	if strings.Contains(string(auditBytes), "select id, v from public.t") {
		t.Error("workbench statement text in the audit file")
	}
	if !strings.Contains(string(auditBytes), `"executeQuery"`) || !strings.Contains(string(auditBytes), "sqlHash") {
		t.Error("workbench execution not audited with length and hash")
	}
	e.mustTask(t, "DELETE", "/databases/gdb", nil)
	e.expect(t, 204, "DELETE", "/roles/reader", nil)
}

func TestNetworkAccess(t *testing.T) {
	e := shared(t)
	na := e.expect(t, 200, "GET", "/settings/network-access", nil)
	valid(t, "getNetworkAccess", "200", na)
	rev := na["revision"].(float64)
	if len(na["rules"].([]any)) != 2 || rev != 1 {
		t.Fatalf("initial rules %v", na)
	}
	pw := "Remote-Passw0rd-1"
	e.expect(t, 201, "POST", "/roles", map[string]any{"name": "remote_user", "password": pw, "clusterRoles": []string{"kingres_readonly"}})
	rules := func(extra ...map[string]any) []any {
		out := []any{}
		for _, x := range extra {
			out = append(out, x)
		}
		out = append(out,
			map[string]any{"type": "host", "databases": []string{"all"}, "users": []string{"all"}, "address": "127.0.0.0/8", "method": "scram-sha-256"})
		return out
	}
	reject := map[string]any{"type": "host", "databases": []string{"all"}, "users": []string{"remote_user"}, "address": "127.0.0.0/8", "method": "reject"}
	if _, ok := e.loginAs("remote_user", pw); !ok {
		t.Fatal("remote_user cannot sign in before the change")
	}
	e.mustTask(t, "PUT", "/settings/network-access", map[string]any{"revision": rev, "tlsRequired": false, "rules": rules(reject)})
	time.Sleep(500 * time.Millisecond)
	if _, ok := e.loginAs("remote_user", pw); ok {
		t.Error("remote_user can still sign in after the reject rule")
	}
	na = e.expect(t, 200, "GET", "/settings/network-access", nil)
	valid(t, "getNetworkAccess", "200", na)
	if na["revision"] != rev+1 {
		t.Errorf("revision %v", na["revision"])
	}
	if as := na["applyStatus"].([]any); len(as) != 1 || as[0].(map[string]any)["appliedRevision"] != rev+1 {
		t.Errorf("applyStatus %v", na["applyStatus"])
	}
	// Stale revision, rule sets that lock the caller out, invalid rules.
	if st, o, _ := e.call("PUT", "/settings/network-access", map[string]any{"revision": rev, "tlsRequired": false, "rules": rules()}, nil); st != 409 || problemCode(o) != "optimistic_lock_failed" {
		t.Errorf("stale revision: %d %v", st, o)
	}
	lock := map[string]any{"type": "host", "databases": []string{"all"}, "users": []string{"all"}, "address": "127.0.0.0/8", "method": "reject"}
	if st, o, _ := e.call("PUT", "/settings/network-access", map[string]any{"revision": rev + 1, "tlsRequired": false, "rules": []any{lock}}, nil); st != 422 {
		t.Errorf("lock-out rule set: %d %v", st, o)
	}
	if st, o, _ := e.call("PUT", "/settings/network-access", map[string]any{"revision": rev + 1, "tlsRequired": false, "rules": []any{}}, nil); st != 422 {
		t.Errorf("empty rule set locks everybody out: %d %v", st, o)
	}
	bad := []map[string]any{
		{"type": "host", "databases": []string{"all"}, "users": []string{"all"}, "address": "evil.example.com", "method": "scram-sha-256"},
		{"type": "host", "databases": []string{"all"}, "users": []string{"all"}, "address": "10.0.0.0/8", "method": "trust"},
		{"type": "local", "databases": []string{"all"}, "users": []string{"all"}, "address": "10.0.0.0/8", "method": "scram-sha-256"},
		{"type": "host", "databases": []string{"a b"}, "users": []string{"all"}, "address": "10.0.0.0/8", "method": "scram-sha-256"},
		{"type": "host", "databases": []string{"all"}, "users": []string{"all"}, "address": "10.0.0.0/8\nhost all all all trust", "method": "scram-sha-256"},
	}
	for _, r := range bad {
		if st, o, _ := e.call("PUT", "/settings/network-access", map[string]any{"revision": rev + 1, "tlsRequired": false, "rules": rules(r)}, nil); st < 400 || st >= 500 {
			t.Errorf("bad rule %v: %d %v", r, st, o)
		}
	}
	// TLS required: plain host rules are refused.
	if st, _, _ := e.call("PUT", "/settings/network-access", map[string]any{"revision": rev + 1, "tlsRequired": true, "rules": rules()}, nil); st < 400 || st >= 500 {
		t.Errorf("plain rules under tlsRequired: %d", st)
	}
	// Dry run (contract 0.6.0): checked like a real change, nothing is changed.
	const dry = "/settings/network-access?dryRun=true"
	if st, o, _ := e.call("PUT", dry, map[string]any{"revision": rev + 1, "tlsRequired": false, "rules": []any{lock}}, nil); st != 422 {
		t.Errorf("dry run of a lock-out rule set: %d %v", st, o)
	}
	if st, o, _ := e.call("PUT", dry, map[string]any{"revision": rev, "tlsRequired": false, "rules": rules()}, nil); st != 409 {
		t.Errorf("dry run with a stale revision: %d %v", st, o)
	}
	if st, o, _ := e.call("PUT", "/settings/network-access?dryRun=maybe", map[string]any{"revision": rev + 1, "tlsRequired": false, "rules": rules()}, nil); st != 400 {
		t.Errorf("dryRun=maybe: %d %v", st, o)
	}
	st, chk, _ := e.call("PUT", dry, map[string]any{"revision": rev + 1, "tlsRequired": false, "rules": rules()}, nil)
	if st != 200 || chk["valid"] != true || chk["revision"] != rev+1 {
		t.Errorf("dry run of a valid change: %d %v", st, chk)
	}
	valid(t, "replaceNetworkAccess", "200", chk)
	if after := e.expect(t, 200, "GET", "/settings/network-access", nil); after["revision"] != rev+1 {
		t.Errorf("a dry run changed the revision: %v", after["revision"])
	}
	// Restore.
	e.mustTask(t, "PUT", "/settings/network-access", map[string]any{"revision": rev + 1, "tlsRequired": false, "rules": rules()})
	// A reload takes effect shortly after the task finished.
	signedIn := false
	for i := 0; i < 7 && !signedIn; i++ {
		_, signedIn = e.loginAs("remote_user", pw)
		if !signedIn {
			time.Sleep(200 * time.Millisecond)
		}
	}
	if !signedIn {
		t.Error("remote_user cannot sign in after the rules were restored")
	}
	// The generated file is exactly the managed rules, never anything else.
	hba, _ := os.ReadFile(filepath.Join(e.dataDir, "instances", "1", "pgdata", "pg_hba.conf"))
	if strings.Contains(string(hba), "trust") || !strings.Contains(string(hba), "peer map=kingres_proxy") {
		t.Errorf("pg_hba.conf:\n%s", hba)
	}
	e.expect(t, 204, "DELETE", "/roles/remote_user", nil)
}

// TestSecretsNeverLeak runs after the other tests of this file (alphabetical
// order of the names is not relied on: it only checks what exists) and greps
// the log, the audit file, the task table and the setup task file for the
// marker password used by TestRolesLifecycle and the setup tests.
func TestZSecretsNeverLeak(t *testing.T) {
	e := shared(t)
	_, out, _ := e.call("GET", "/tasks?limit=200", nil, nil)
	raw, _ := json.Marshal(out)
	files := map[string]string{"log": e.logs.String(), "tasks": string(raw)}
	for _, name := range []string{"audit.jsonl", "setup-task.json", "node-state.json"} {
		b, _ := os.ReadFile(filepath.Join(e.dataDir, name))
		files[name] = string(b)
	}
	// The task table itself, not only the API view of it.
	st, q, _ := e.call("POST", "/query", map[string]any{"database": "kingres_system", "sql": "select input::text, progress::text, error::text from kingres_cp.task"}, nil)
	if st == 200 {
		b, _ := json.Marshal(q)
		files["task table"] = string(b)
	}
	for _, secret := range []string{markerPassword, adminPass, "Passw0rd-", "Reader-Passw0rd", "Remote-Passw0rd", "Another-Passw0rd"} {
		for name, content := range files {
			if strings.Contains(content, secret) {
				t.Errorf("secret %q found in %s", secret, name)
			}
		}
	}
	// No SCRAM verifier either.
	for name, content := range files {
		if strings.Contains(content, "SCRAM-SHA-256$") {
			t.Errorf("a password verifier is in %s", name)
		}
	}
}
