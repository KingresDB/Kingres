// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026 RooyeKhat Media Co

package clustertest_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/archive"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/archive/archivetest"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/clustertest"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/fault"
)

// Helpers and tests of the database import over three servers with a change of the leader (phase 14c) and of the round trip of a distributed schema (phase 11).

const dimImporter, dimImporterPass = "dbi_admin", "Importer-Passw0rd-1"

// dimEnv is a cluster and the two identities that a server may act as: a node keeps one token, so the test switches it before a call.
type dimEnv struct {
	c        *clustertest.Cluster
	adm, imp map[*clustertest.Node]string
}

// login signs the node in as both identities (again after a restart) and leaves it the administrator.
func (e *dimEnv) login(t *testing.T, n *clustertest.Node) {
	t.Helper()
	until(t, 3*time.Minute, fmt.Sprintf("the importer to sign in on node %d", n.Index), func() bool { return n.Login(dimImporter, dimImporterPass) })
	e.imp[n] = n.Token
	until(t, 3*time.Minute, fmt.Sprintf("the administrator to sign in on node %d", n.Index), func() bool { return n.Login(clustertest.AdminUser, clustertest.AdminPass) })
	e.adm[n] = n.Token
}

// asImporter makes the next calls of the node those of the importer; asAdmin those of the administrator.
func (e *dimEnv) asImporter(n *clustertest.Node) *clustertest.Node { n.Token = e.imp[n]; return n }
func (e *dimEnv) asAdmin(n *clustertest.Node) *clustertest.Node    { n.Token = e.adm[n]; return n }

// dimCluster forms three servers with the data transfer on, a sharded database src with a local table, a hash table, a reference table and the distributed
// schemas tn (two tables) and tm (one table), all with rows, and the importer role (a role of kingres_admin as the API makes it) signed in on every server.
func dimCluster(t *testing.T) *dimEnv {
	t.Helper()
	c := clustertest.FormTLS(t, 3, nil, func(leader *clustertest.Node) {
		body := leader.Expect(202, "POST", "/databases", map[string]any{"name": "src", "sharding": map[string]any{"enabled": true, "bucketCount": 12, "mode": "both"}})
		leader.WaitTaskDone(clustertest.TaskID(body), 4*time.Minute)
	})
	leader := c.Leader()
	for _, n := range c.Nodes {
		n.Login(clustertest.AdminUser, clustertest.AdminPass)
	}
	leader.Expect(200, "PATCH", "/settings/cluster", map[string]any{"dataTransfer": map[string]any{"enabled": true, "maxConcurrent": 4}})
	q := func(sql string) { leader.Expect(200, "POST", "/query", map[string]any{"database": "src", "sql": sql}) }
	task := func(method, path string, body any) {
		t.Helper()
		out := leader.Expect(202, method, path, body)
		if res := leader.WaitTaskDone(clustertest.TaskID(out), 4*time.Minute); res["state"] != "done" {
			t.Fatalf("%s %s: %v", method, path, res["error"])
		}
	}
	for _, s := range []string{
		"create table public.t_plain (id serial primary key, name text)",
		"create table public.refdata (id int primary key, v text)",
		"create table public.d_hash (id bigint primary key, v text)",
		"create schema tn", "create table tn.docs (id bigint primary key, v text)", "create table tn.notes (id bigint primary key, doc_id bigint, body text)",
		"create schema tm", "create table tm.facts (id bigint primary key, v text)",
	} {
		q(s)
	}
	task("PUT", "/databases/src/schemas/tn/distribution", nil)
	task("PUT", "/databases/src/schemas/tm/distribution", nil)
	task("PUT", "/databases/src/schemas/public/tables/refdata/distribution", map[string]any{"method": "reference"})
	task("PUT", "/databases/src/schemas/public/tables/d_hash/distribution", map[string]any{"method": "hash", "distributionColumn": "id"})
	for _, s := range []string{
		"insert into public.t_plain (name) values ('Ada'), ('Bob')",
		"insert into public.refdata values (1, 'a'), (2, 'b')",
		"insert into public.d_hash select g, 'v' || g from generate_series(1, 120) g",
		"insert into tn.docs select g, 'doc' || g from generate_series(1, 60) g",
		"insert into tn.notes select g, g % 60 + 1, 'note' || g from generate_series(1, 90) g",
		"insert into tm.facts select g, 'fact' || g from generate_series(1, 40) g",
	} {
		q(s)
	}
	leader.Expect(201, "POST", "/roles", map[string]any{"name": dimImporter, "password": dimImporterPass, "clusterRoles": []string{"kingres_admin"}})
	e := &dimEnv{c: c, adm: map[*clustertest.Node]string{}, imp: map[*clustertest.Node]string{}}
	for _, n := range c.Nodes {
		e.login(t, n)
	}
	return e
}

// export exports database db through a follower, as the administrator, and returns the archive.
func (e *dimEnv) export(t *testing.T, db string) []byte {
	t.Helper()
	c := e.c
	st, out := e.asAdmin(c.Nodes[1]).Do("POST", "/databases/"+db+"/export", map[string]any{}, nil)
	if st != 202 {
		t.Fatalf("export: %d %v\n%s", st, out, c.Leader().LogTail(6000))
	}
	res := e.asAdmin(c.Nodes[2]).WaitTask(clustertest.TaskID(out), 6*time.Minute)
	if res["state"] != "done" {
		t.Fatalf("export task: %v", res["error"])
	}
	id := strings.TrimPrefix(res["detail"].(string), "transfer ")
	_, _, body, err := e.asAdmin(c.Leader()).DoBody("GET", "/data-exports/"+id+"/content", nil, nil)
	if err != nil || len(body) == 0 {
		t.Fatalf("download: %v", err)
	}
	return body
}

// upload stages the archive on a server as the importer and returns the id of the upload.
func (e *dimEnv) upload(t *testing.T, n *clustertest.Node, data []byte) string {
	t.Helper()
	e.asImporter(n)
	up := n.Expect(201, "POST", "/data-uploads", map[string]any{"purpose": "databaseArchive", "sizeBytes": len(data)})
	uid := up["uploadId"].(string)
	const mb = 1 << 20
	for off := 0; off < len(data); off += mb {
		end := min(off+mb, len(data))
		if st, _ := xfChunk(t, n, uid, off, data[off:end]); st != 204 {
			t.Fatalf("chunk at %d: %d", off, st)
		}
	}
	return uid
}

// startImport asks a server, as the importer, to import an upload.
func (e *dimEnv) startImport(n *clustertest.Node, upload, database string) (int, map[string]any) {
	return e.asImporter(n).Do("POST", "/database-imports", map[string]any{"uploadId": upload, "database": database}, nil)
}

// rows runs a query as the service role on the server (the importer owns what it imports) and returns the rows as text.
func dimRows(t *testing.T, n *clustertest.Node, db, sql string) string {
	t.Helper()
	c := n.ServicePG(db)
	defer func() { _ = c.Close(context.Background()) }()
	rows, err := c.Query(context.Background(), sql)
	if err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	defer rows.Close()
	var out [][]any
	for rows.Next() {
		vals, err := rows.Values()
		if err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		out = append(out, vals)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return fmt.Sprint(out)
}

// dimTables are the tables whose rows an import must keep.
var dimTables = []string{"public.t_plain", "public.refdata", "public.d_hash", "tn.docs", "tn.notes", "tm.facts"}

func dimDigest(t *testing.T, n *clustertest.Node, db, tb string) string {
	t.Helper()
	return dimRows(t, n, db, "select count(*), coalesce(md5(string_agg(x::text, '|' order by x::text)), '') from "+tb+" x")
}

// dimLeader is the manager leader as one of the live servers tells it (nil when none answers).
func dimLeader(c *clustertest.Cluster, live ...*clustertest.Node) *clustertest.Node {
	for _, n := range live {
		st, b := n.Do("GET", "/cluster", nil, nil)
		if st != 200 {
			continue
		}
		if ml, _ := b["managerLeader"].(map[string]any); ml != nil {
			for _, m := range c.Nodes {
				if ml["serverId"] == m.Host {
					return m
				}
			}
		}
	}
	return nil
}

// dimRoles is what a failed import must leave as it found it, on one server: the roles with their attributes, the memberships and the per-database settings.
func (e *dimEnv) roles(t *testing.T, n *clustertest.Node) string {
	t.Helper()
	r := e.asAdmin(n).Expect(200, "POST", "/query", map[string]any{"database": "postgres", "sql": `select 'role', rolname::text, rolsuper::text || rolcreaterole::text || rolcanlogin::text || rolinherit::text || rolbypassrls::text || rolreplication::text, '' from pg_roles
		union all select 'member', roleid::regrole::text, member::regrole::text, '' from pg_auth_members
		union all select 'setting', setdatabase::text, setrole::text, setconfig::text from pg_db_role_setting order by 1, 2, 3`})
	return fmt.Sprint(r["rows"])
}

// databases lists the databases of a server that carry the name or belong to an import (the private ones).
func (e *dimEnv) databases(t *testing.T, n *clustertest.Node, name string) string {
	t.Helper()
	r := e.asAdmin(n).Expect(200, "POST", "/query", map[string]any{"database": "postgres", "sql": "select datname from pg_database where datname = '" + name + "' or datname like 'kingres_import_%' order by 1"})
	return fmt.Sprint(r["rows"])
}

// The leader's kingresd ends at the start of every step of database.import (the process ends as a kill does, its PostgreSQL instances go on), the other servers elect a
// leader, and the import is either finished by it or ends failed with nothing left: no private database on any server, no final database, the roles as they were. The
// old server then returns as a follower and the next step is tried on the new leader. A finished import has every row.
func TestDatabaseImportWithALeaderChangeAtEveryStep(t *testing.T) {
	e := dimCluster(t)
	c := e.c
	archive := e.export(t, "src")
	leader := dimLeader(c, c.Nodes...)
	if leader == nil {
		t.Fatal("no leader")
	}
	// A baseline import on the leader tells the steps and the digests of the rows.
	st, out := e.startImport(leader, e.upload(t, leader, archive), "dil_base")
	if st != 202 {
		t.Fatalf("baseline import: %d %v", st, out)
	}
	base := e.asAdmin(leader).WaitTask(clustertest.TaskID(out), 10*time.Minute)
	if base["state"] != "done" {
		t.Fatalf("baseline import: %v", base["error"])
	}
	var steps []string
	for _, s := range anyList(base["steps"]) {
		steps = append(steps, s.(map[string]any)["name"].(string))
	}
	if len(steps) < 12 {
		t.Fatalf("the import task has %d steps: %v", len(steps), steps)
	}
	want := map[string]string{}
	for _, tb := range dimTables {
		want[tb] = dimDigest(t, leader, "dil_base", tb)
	}
	t.Logf("steps of the import: %v", steps)

	for i, step := range steps {
		t.Run(fmt.Sprintf("%02d %s", i, step), func(t *testing.T) {
			old := dimLeader(c, c.Nodes...)
			if old == nil {
				t.Fatal("no leader")
			}
			var others []*clustertest.Node
			for _, n := range c.Nodes {
				if n != old {
					others = append(others, n)
				}
			}
			before := make([]string, len(c.Nodes))
			for k, n := range c.Nodes {
				before[k] = e.roles(t, n)
			}
			target := fmt.Sprintf("dil_s%02d", i)
			up := e.upload(t, old, archive)
			old.SetFaults(fault.Spec{TaskExitPoint: "step_started", TaskExitStep: i})
			st, out := e.startImport(old, up, target)
			until(t, 5*time.Minute, "the leader's kingresd to end at the start of step "+step, func() bool {
				s, _ := old.Do("GET", "/healthz", nil, nil)
				return s != 200
			})
			old.Kill()
			old.Heal()
			// Another server leads once the lease of the old one has run out.
			var nl *clustertest.Node
			until(t, 4*time.Minute, "another server to lead", func() bool {
				nl = dimLeader(c, others...)
				return nl != nil && nl != old
			})
			e.asAdmin(nl)
			var tk any
			if st == 202 {
				tk = clustertest.TaskID(out)
			} else { // the process ended before it answered: the task is the newest of its kind
				items := anyList(nl.Expect(200, "GET", "/tasks?kind=database.import&limit=1", nil)["items"])
				if len(items) == 0 || fmt.Sprint(items[0].(map[string]any)["id"]) == fmt.Sprint(base["id"]) {
					t.Fatalf("the request answered %d %v and no import task was started", st, out)
				}
				tk = clustertest.TaskID(items[0].(map[string]any))
			}
			res := nl.WaitTask(tk, 20*time.Minute)
			t.Logf("step %q: the import ended %v (new leader %s)", step, res["state"], nl.Host)
			defer func() {
				if t.Failed() {
					t.Logf("task: %v", res)
					t.Logf("new leader log: %s", quietLog(nl.LogTail(40000)))
				}
			}()
			// The old server returns as a follower.
			old.Start()
			e.login(t, old)
			for _, n := range c.Nodes {
				until(t, 4*time.Minute, fmt.Sprintf("node %d to hold what an import that ended %v leaves", n.Index, res["state"]), func() bool {
					got := e.databases(t, n, target)
					if res["state"] == "done" {
						return strings.Contains(got, target) && !strings.Contains(got, "kingres_import_")
					}
					return !strings.Contains(got, target) && !strings.Contains(got, "kingres_import_")
				})
			}
			if res["state"] == "done" {
				for _, tb := range dimTables {
					if got := dimDigest(t, nl, target, tb); got != want[tb] {
						t.Errorf("table %s of the finished import: %s, want %s", tb, got, want[tb])
					}
				}
			}
			for k, n := range c.Nodes {
				if after := e.roles(t, n); after != before[k] {
					t.Errorf("node %d: the roles changed\n before %s\n after  %s", n.Index, before[k], after)
				}
			}
		})
	}
}

// The round trip of a distributed schema (phase 11 into phase 14c): a database with two distributed schemas, a hash table, a reference table and a local table is
// exported and imported under a new name. The imported database has the same schemas distributed (each on one group), the same rows, and the schema tasks work on it:
// a move to another group keeps the rows, a make local and a distribute again too, and the source database is not touched.
func TestDatabaseImportOfADistributedSchemaRoundTrip(t *testing.T) {
	e := dimCluster(t)
	c := e.c
	leader := c.Leader()
	archive := e.export(t, "src")
	st, out := e.startImport(c.Nodes[1], e.upload(t, c.Nodes[1], archive), "dsr_copy")
	if st != 202 {
		t.Fatalf("import: %d %v", st, out)
	}
	if res := e.asAdmin(c.Nodes[2]).WaitTask(clustertest.TaskID(out), 10*time.Minute); res["state"] != "done" {
		t.Fatalf("import: %v", res["error"])
	}
	schemaGroup := func(db, schema string) any {
		d, _ := e.asAdmin(leader).Expect(200, "GET", "/databases/"+db+"/schemas/"+schema, nil)["distribution"].(map[string]any)
		if d == nil {
			return nil
		}
		return d["shardGroupId"]
	}
	for _, s := range []string{"tn", "tm"} {
		if g := schemaGroup("dsr_copy", s); g == nil {
			t.Fatalf("schema %s of the imported database is not distributed", s)
		}
	}
	if schemaGroup("dsr_copy", "public") != nil {
		t.Errorf("the schema public of the imported database is distributed")
	}
	sqlSchemas := "select n.nspname from kingres.dist_schema d join pg_catalog.pg_namespace n on n.oid = d.nsp order by 1"
	if a, b := dimRows(t, leader, "src", sqlSchemas), dimRows(t, leader, "dsr_copy", sqlSchemas); a != b || a != "[[tm] [tn]]" {
		t.Errorf("distributed schemas: source %s, imported %s", a, b)
	}
	for _, tb := range dimTables {
		if a, b := dimDigest(t, leader, "src", tb), dimDigest(t, leader, "dsr_copy", tb); a != b {
			t.Errorf("table %s: source %s, imported %s", tb, a, b)
		}
	}
	srcGroups := map[string]any{"tn": schemaGroup("src", "tn"), "tm": schemaGroup("src", "tm")}
	waitSettled := func(db string) {
		t.Helper()
		until(t, 3*time.Minute, "the old copy of a move in "+db+" to be emptied", func() bool {
			return skInt(t, leader, db, `SELECT count(*) FROM kingres.bucket_move WHERE phase = 'cleaning'`) == 0
		})
	}
	wait := func(method, path string, body any) {
		t.Helper()
		o := e.asAdmin(leader).Expect(202, method, path, body)
		if res := leader.WaitTaskDone(clustertest.TaskID(o), 8*time.Minute); res["state"] != "done" {
			t.Fatalf("%s %s: %v", method, path, res["error"])
		}
	}
	// A schema task on the imported database: the move to another group.
	cur, _ := schemaGroup("dsr_copy", "tn").(float64)
	target := 1
	if int(cur) == 1 {
		target = 2
	}
	wait("POST", "/databases/dsr_copy/schemas/tn/moves", map[string]any{"targetShardGroupId": target})
	if g := schemaGroup("dsr_copy", "tn"); g != float64(target) {
		t.Errorf("after the move the schema tn is on group %v, want %d", g, target)
	}
	waitSettled("dsr_copy")
	for _, tb := range []string{"tn.docs", "tn.notes"} {
		if a, b := dimDigest(t, leader, "src", tb), dimDigest(t, leader, "dsr_copy", tb); a != b {
			t.Errorf("after the move, table %s: source %s, imported %s", tb, a, b)
		}
	}
	// New rows go to the table where it is now.
	if err := insertDoc(leader); err != nil {
		t.Errorf("an insert into the moved schema: %v", err)
	}
	if got := dimRows(t, leader, "dsr_copy", "select count(*) from tn.docs"); got != "[[61]]" {
		t.Errorf("rows of tn.docs after the insert: %s", got)
	}
	// Make local and distribute again.
	wait("DELETE", fmt.Sprintf("/databases/dsr_copy/schemas/tm/distribution?shardGroupId=%d", target), nil)
	if g := schemaGroup("dsr_copy", "tm"); g != nil {
		t.Errorf("schema tm is still distributed after the make local: %v", g)
	}
	if got := dimRows(t, leader, "dsr_copy", "select count(*) from tm.facts"); got != "[[40]]" {
		t.Errorf("rows of tm.facts after the make local: %s", got)
	}
	wait("PUT", "/databases/dsr_copy/schemas/tm/distribution", nil)
	if g := schemaGroup("dsr_copy", "tm"); g == nil {
		t.Errorf("schema tm is not distributed after it was distributed again")
	}
	if got := dimRows(t, leader, "dsr_copy", "select count(*) from tm.facts"); got != "[[40]]" {
		t.Errorf("rows of tm.facts after the distribute: %s", got)
	}
	// The source database is as it was.
	for s, g := range srcGroups {
		if now := schemaGroup("src", s); now != g {
			t.Errorf("the schema %s of the source moved from group %v to %v", s, g, now)
		}
	}
	for _, tb := range dimTables {
		if tb == "tn.docs" {
			continue
		}
		if a, b := dimDigest(t, leader, "src", tb), dimDigest(t, leader, "dsr_copy", tb); a != b {
			t.Errorf("at the end, table %s: source %s, imported %s", tb, a, b)
		}
	}
}

// insertDoc adds a row to the table of the distributed schema of the imported database.
func insertDoc(n *clustertest.Node) error {
	c := n.ServicePG("dsr_copy")
	defer func() { _ = c.Close(context.Background()) }()
	_, err := c.Exec(context.Background(), "insert into tn.docs values (1000, 'new')")
	return err
}

// The archive of a database with distributed schemas is refused when the SQL of the archive makes a relation in a schema that the catalog distributes and does not
// name (L-C of the review of 14c: the schema is distributed as it is, and the service role would read and move what the importing role made and nothing vouched
// for): the task ends failed with table_not_eligible, nothing is left on any server (no private database, no final database, the roles as they were), and the message
// does not repeat the name of the relation. The good archive imports, so every refusal is the work of the extra relation.
func TestDatabaseImportRefusesAnExtraRelationInADistributedSchema(t *testing.T) {
	e := dimCluster(t)
	c := e.c
	leader := c.Leader()
	base := e.export(t, "src")
	st, out := e.startImport(leader, e.upload(t, leader, base), "dsx_ok")
	if st != 202 {
		t.Fatalf("import of the good archive: %d %v", st, out)
	}
	if res := e.asAdmin(leader).WaitTask(clustertest.TaskID(out), 10*time.Minute); res["state"] != "done" {
		t.Fatalf("import of the good archive: %v", res["error"])
	}
	cases := []struct {
		name, schema, sql string
	}{
		{"a table that the catalog does not name", "tn", "CREATE TABLE tn.zz_extra (a int);\n"},
		{"a table in the other distributed schema", "tm", "CREATE TABLE tm.zz_extra (a int);\n"},
		{"a partitioned table that the catalog does not name", "tn", "CREATE TABLE tn.zz_extra_p (a int, d date) PARTITION BY RANGE (d);\n"},
		{"a table whose default calls a function", "tn", "CREATE TABLE tn.zz_extra_c (a int DEFAULT length('x'));\n"},
		{"a view in the schema", "tn", "CREATE VIEW tn.zz_extra_v AS SELECT id FROM tn.docs;\n"},
	}
	before := make([]string, len(c.Nodes))
	for k, n := range c.Nodes {
		before[k] = e.roles(t, n)
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := archivetest.Read(t, base)
			v.Files[archive.PreDataName] += tc.sql
			st, out := e.startImport(leader, e.upload(t, leader, v.Bytes(t)), "dsx_bad")
			if st != 202 {
				if st != 422 { // a refusal at the request is as good: nothing was started
					t.Fatalf("import: %d %v", st, out)
				}
			} else {
				res := e.asAdmin(leader).WaitTask(clustertest.TaskID(out), 10*time.Minute)
				code := ""
				msg := ""
				if er, _ := res["error"].(map[string]any); er != nil {
					code, _ = er["code"].(string)
					msg, _ = er["message"].(string)
				}
				if res["state"] != "failed" || code != "table_not_eligible" {
					t.Fatalf("task: state %v code %q (%s), want failed with table_not_eligible", res["state"], code, msg)
				}
				if strings.Contains(msg, "zz_extra") {
					t.Errorf("the message of the task names the relation of the archive: %q", msg)
				}
			}
			for _, n := range c.Nodes {
				until(t, 4*time.Minute, fmt.Sprintf("node %d to hold no database of the refused import", n.Index), func() bool {
					return !strings.Contains(e.databases(t, n, "dsx_bad"), "dsx_bad") && !strings.Contains(e.databases(t, n, "dsx_bad"), "kingres_import_")
				})
			}
			for k, n := range c.Nodes {
				if after := e.roles(t, n); after != before[k] {
					t.Errorf("node %d: the roles changed\n before %s\n after  %s", n.Index, before[k], after)
				}
			}
		})
	}
}
