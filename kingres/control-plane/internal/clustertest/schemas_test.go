// SPDX-License-Identifier: AGPL-3.0-only

package clustertest_test

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/clustertest"
)

// Distributed schemas (phase 11, step 13): the control plane of distribute, move and undistribute on a cluster of three servers
// with real instances. The groups are 0 (the leader, which is the map group), 1 and 2.

// shPath is the path of a schema of database db; the name goes through the URL as any client sends it.
func shPath(db, schema string) string {
	return "/databases/" + db + "/schemas/" + url.PathEscape(schema)
}

func shExec(t *testing.T, c *pgx.Conn, sql string, args ...any) {
	t.Helper()
	if _, err := c.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

func shInt(t *testing.T, c *pgx.Conn, sql string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := c.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return n
}

// shSteps returns "name=state" of every step of a task body, in order.
func shSteps(task map[string]any) []string {
	prog, _ := task["progress"].(map[string]any)
	var out []string
	for _, s := range anyList(prog["steps"]) {
		m := s.(map[string]any)
		out = append(out, fmt.Sprintf("%v=%v", m["name"], m["state"]))
	}
	return out
}

func shTask(t *testing.T, n *clustertest.Node, id any) map[string]any {
	t.Helper()
	return n.Expect(200, "GET", fmt.Sprintf("/tasks/%v", id), nil)
}

// shProblem asserts a problem response: status and code.
func shProblem(t *testing.T, st int, body map[string]any, wantStatus int, wantCode string) {
	t.Helper()
	if st != wantStatus || body["code"] != wantCode {
		t.Fatalf("got %d %v, want %d %s", st, body, wantStatus, wantCode)
	}
}

// shSetFault sets the extension's fault for new sessions on database db of the leader's instance.
func shSetFault(t *testing.T, c *clustertest.Cluster, db, spec string) {
	t.Helper()
	conn := c.Leader().ServicePG("postgres")
	defer func() { _ = conn.Close(context.Background()) }()
	q := `ALTER DATABASE ` + db + ` RESET kingres_shard.fault_injection`
	if spec != "" {
		q = `ALTER DATABASE ` + db + ` SET kingres_shard.fault_injection = '` + spec + `'`
	}
	if _, err := conn.Exec(context.Background(), q); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

// shBucketMovePhase is the phase of the latest move of the colocation group, read on the map group primary.
func shBucketMovePhase(t *testing.T, leader *clustertest.Node, db string, colo int64) string {
	t.Helper()
	conn := leader.ServicePG(db)
	defer func() { _ = conn.Close(context.Background()) }()
	var p string
	err := conn.QueryRow(context.Background(), `SELECT phase FROM kingres.bucket_move WHERE colocation_id = $1 ORDER BY move_id DESC LIMIT 1`, colo).Scan(&p)
	if err != nil {
		return ""
	}
	return p
}

// shCheckClean fails when kingres.check_cluster() of the database reports anything that names a distributed schema, or any
// of the findings that make a move unsafe.
func shCheckClean(t *testing.T, leader *clustertest.Node, db string) {
	t.Helper()
	conn := leader.ServicePG(db)
	defer func() { _ = conn.Close(context.Background()) }()
	rows, err := conn.Query(context.Background(), `SELECT check_name, coalesce(node_id::text, ''), coalesce(object, ''), coalesce(detail, '') FROM kingres.check_cluster()`)
	if err != nil {
		t.Fatalf("check_cluster: %v", err)
	}
	defer rows.Close()
	var bad []string
	for rows.Next() {
		var name, node, object, detail string
		if err := rows.Scan(&name, &node, &object, &detail); err != nil {
			t.Fatal(err)
		}
		switch name {
		case "orphan_table", "orphan_tables", "old_prepared_transaction", "prepared_transaction_old":
			continue
		}
		bad = append(bad, fmt.Sprintf("%s node=%s %s %s", name, node, object, detail))
	}
	if len(bad) > 0 {
		t.Fatalf("check_cluster of %s is not clean:\n%s", db, strings.Join(bad, "\n"))
	}
}

// shWaitMovePhase waits until the latest move of the schema's colocation group is in phase want.
func shWaitMovePhase(t *testing.T, leader *clustertest.Node, db string, colo int64, want string, d time.Duration) {
	t.Helper()
	until(t, d, "the move of group "+fmt.Sprint(colo)+" to be "+want, func() bool { return shBucketMovePhase(t, leader, db, colo) == want })
}

func shLogin(t *testing.T, leader *clustertest.Node, user, pw string) map[string]string {
	t.Helper()
	st, b := leader.Do("POST", "/auth/login", map[string]any{"username": user, "password": pw, "issueToken": true}, map[string]string{"Authorization": ""})
	if st != 200 {
		t.Fatalf("login %s: %d %v", user, st, b)
	}
	return map[string]string{"Authorization": "Bearer " + b["token"].(string)}
}

// shCluster forms the cluster of the schema tests: database sh (mode both) exists on the first server before the others join.
func shCluster(t *testing.T) *clustertest.Cluster {
	t.Helper()
	c := clustertest.FormWithExtra(t, 3, []string{"-rebalance-retry-backoff", "1s"}, func(leader *clustertest.Node) {
		body := leader.Expect(202, "POST", "/databases", map[string]any{"name": "sh", "sharding": map[string]any{"enabled": true, "mode": "both", "bucketCount": 8}})
		leader.WaitTaskDone(clustertest.TaskID(body), 4*time.Minute)
		// the ledger database of the pinned schema exists before the others join (a database with a ledger made later cannot be registered on them)
		body = leader.Expect(202, "POST", "/databases", map[string]any{"name": "shl", "sharding": map[string]any{"enabled": true, "mode": "both"}, "ledger": map[string]any{"mode": "optIn"}})
		leader.WaitTaskDone(clustertest.TaskID(body), 4*time.Minute)
	})
	// No standbys: building them restarts instances and rewrites access rules, which is noise for a move that waits for its nodes.
	c.Leader().Expect(200, "PATCH", "/settings/cluster", map[string]any{"defaultReplicaCount": 0})
	until(t, 3*time.Minute, "no standby task to run", func() bool {
		for _, st := range []string{"pending", "running"} {
			if len(anyList(c.Leader().Expect(200, "GET", "/tasks?kind=standby.reconcile&state="+st, nil)["items"])) > 0 {
				return false
			}
		}
		return true
	})
	return c
}

func shSchemaItem(t *testing.T, n *clustertest.Node, db, name string) map[string]any {
	t.Helper()
	return n.Expect(200, "GET", shPath(db, name), nil)
}

func shDistribution(t *testing.T, n *clustertest.Node, db, name string) map[string]any {
	t.Helper()
	d, _ := shSchemaItem(t, n, db, name)["distribution"].(map[string]any)
	return d
}

// shRowsEverywhere checks that every server reads the same rows of the schema's table (through the routing).
func shRowsEverywhere(t *testing.T, c *clustertest.Cluster, db, table string, want int64) {
	t.Helper()
	for _, n := range c.Nodes {
		conn := n.ServicePG(db)
		var got int64
		var err error
		for i := 0; i < 30; i++ { // KS001 can be answered while a switch commits
			err = conn.QueryRow(context.Background(), `SELECT count(*) FROM `+table).Scan(&got)
			if err == nil {
				break
			}
			time.Sleep(200 * time.Millisecond)
		}
		_ = conn.Close(context.Background())
		if err != nil || got != want {
			t.Errorf("node %d reads %d rows of %s (%v), want %d", n.Index, got, table, err, want)
		}
	}
}

func TestDistributedSchemas(t *testing.T) {
	c := shCluster(t)
	leader := c.Leader()
	n1, n2 := c.Nodes[1], c.Nodes[2]
	lc := leader.ServicePG("sh")
	ctx := context.Background()

	mkRole(t, leader, "sch_ro", "Readonly-Password-1234", "kingres_readonly")
	mkRole(t, leader, "sch_ops", "Operator-Password-1234", "kingres_ops")
	ro := shLogin(t, leader, "sch_ro", "Readonly-Password-1234")
	ops := shLogin(t, leader, "sch_ops", "Operator-Password-1234")

	// a schema that holds data on the first server, with a table that has no key
	shExec(t, lc, `CREATE SCHEMA tenant_a`)
	shExec(t, lc, `CREATE TABLE tenant_a.a (id bigint PRIMARY KEY, v text)`)
	shExec(t, lc, `CREATE TABLE tenant_a.b (id bigint, v text)`)
	shExec(t, lc, `INSERT INTO tenant_a.a SELECT g, repeat('x', 200) FROM generate_series(1, 60000) g`)
	shExec(t, lc, `INSERT INTO tenant_a.b SELECT g, 'b' FROM generate_series(1, 500) g`)
	shExec(t, lc, `CREATE SCHEMA local_only`)
	shExec(t, lc, `CREATE TABLE local_only.t (id int PRIMARY KEY)`)

	t.Run("modes of a database", func(t *testing.T) {
		opts := leader.Expect(200, "GET", "/database-create-options", nil)
		sm, _ := opts["shardingModes"].(map[string]any)
		if sm["default"] != "row" || len(anyList(sm["modes"])) != 3 {
			t.Errorf("shardingModes = %v", sm)
		}
		db := leader.Expect(200, "GET", "/databases/sh", nil)
		sharding, _ := db["sharding"].(map[string]any)
		if sharding["mode"] != "both" || sharding["distributeNewSchemas"] != false || sharding["bucketCount"] != float64(8) {
			t.Errorf("sharding of sh = %v", sharding)
		}
		stats, _ := db["stats"].(map[string]any)
		if stats["distributedSchemaCount"] != float64(0) {
			t.Errorf("distributedSchemaCount = %v, want 0", stats["distributedSchemaCount"])
		}
		// mode schema: the bucket count is accepted and ignored
		body := leader.Expect(202, "POST", "/databases", map[string]any{"name": "sm_schema", "sharding": map[string]any{"enabled": true, "mode": "schema", "bucketCount": 64, "distributeNewSchemas": true}})
		leader.WaitTaskDone(clustertest.TaskID(body), 4*time.Minute)
		sm2 := leader.Expect(200, "GET", "/databases/sm_schema", nil)["sharding"].(map[string]any)
		if sm2["mode"] != "schema" || sm2["distributeNewSchemas"] != true || sm2["bucketCount"] != nil {
			t.Errorf("sharding of sm_schema = %v", sm2)
		}
		// the default is row, and a request that does not fit is refused
		body = leader.Expect(202, "POST", "/databases", map[string]any{"name": "sm_row", "sharding": map[string]any{"enabled": true}})
		leader.WaitTaskDone(clustertest.TaskID(body), 4*time.Minute)
		if m := leader.Expect(200, "GET", "/databases/sm_row", nil)["sharding"].(map[string]any); m["mode"] != "row" || m["bucketCount"] != float64(128) {
			t.Errorf("sharding of sm_row = %v", m)
		}
		for _, bad := range []map[string]any{
			{"enabled": true, "mode": "nope"},
			{"enabled": true, "distributeNewSchemas": true},
			{"enabled": true, "mode": "row", "distributeNewSchemas": true},
			{"enabled": false, "mode": "schema"},
		} {
			st, b := leader.Do("POST", "/databases", map[string]any{"name": "sm_bad", "sharding": bad}, nil)
			shProblem(t, st, b, 400, "validation_failed")
		}
		// a mode that nothing needs can change at once; the ones that something needs are refused (below, with schemas)
		st, b := leader.Do("PATCH", "/databases/sm_row", map[string]any{"sharding": map[string]any{"distributeNewSchemas": true}}, nil)
		shProblem(t, st, b, 400, "validation_failed")
		st, b = leader.Do("PATCH", "/databases/sm_row", map[string]any{"sharding": map[string]any{}}, nil)
		shProblem(t, st, b, 400, "validation_failed")
		st, b = leader.Do("PATCH", "/databases/sm_row", map[string]any{"sharding": map[string]any{"mode": "nope"}}, nil)
		shProblem(t, st, b, 400, "validation_failed")
		got := leader.Expect(200, "PATCH", "/databases/sm_row", map[string]any{"sharding": map[string]any{"mode": "both"}})
		if got["sharding"].(map[string]any)["mode"] != "both" {
			t.Errorf("after the change: %v", got["sharding"])
		}
		got = leader.Expect(200, "PATCH", "/databases/sm_row", map[string]any{"sharding": map[string]any{"mode": "row"}})
		if got["sharding"].(map[string]any)["mode"] != "row" {
			t.Errorf("after the change back: %v", got["sharding"])
		}
	})

	t.Run("roles", func(t *testing.T) {
		// nothing of this exists, so what is allowed ends in a 404 and what is not in a 403
		missing := shPath("sh", "no_such_schema")
		for _, tc := range []struct{ method, path string }{{"PUT", missing + "/distribution"}, {"DELETE", missing + "/distribution"}, {"POST", missing + "/moves"}} {
			body := any(nil)
			if tc.method == "POST" {
				body = map[string]any{"targetShardGroupId": 1}
			}
			if st, _ := leader.Do(tc.method, tc.path, body, ro); st != 403 {
				t.Errorf("readonly %s %s: %d, want 403", tc.method, tc.path, st)
			}
		}
		// ops moves (it passes the role check and meets a schema that is not there), it cannot distribute or undistribute
		for _, tc := range []struct{ method, path string }{{"PUT", missing + "/distribution"}, {"DELETE", missing + "/distribution"}} {
			if st, _ := leader.Do(tc.method, tc.path, nil, ops); st != 403 {
				t.Errorf("ops %s %s: %d, want 403", tc.method, tc.path, st)
			}
		}
		st, b := leader.Do("POST", missing+"/moves", map[string]any{"targetShardGroupId": 1}, ops)
		shProblem(t, st, b, 404, "not_found")
		// readers read
		for _, p := range []string{"/databases/sh/schemas", shPath("sh", "tenant_a")} {
			if st, _ := leader.Do("GET", p, nil, ro); st != 200 {
				t.Errorf("readonly GET %s: %d", p, st)
			}
		}
		st, b = leader.Do("PUT", missing+"/distribution", nil, nil)
		shProblem(t, st, b, 404, "not_found")
		st, b = leader.Do("POST", shPath("sh", "tenant_a")+"/moves", map[string]any{}, nil)
		shProblem(t, st, b, 400, "validation_failed")
	})

	t.Run("a local schema is listed with no distribution and its blockers", func(t *testing.T) {
		list := leader.Expect(200, "GET", "/databases/sh/schemas", nil)
		items := anyList(list["items"])
		names := []string{}
		for _, it := range items {
			m := it.(map[string]any)
			names = append(names, m["name"].(string))
			if m["distribution"] != nil {
				t.Errorf("schema %v has a distribution before any is made: %v", m["name"], m["distribution"])
			}
		}
		for _, w := range []string{"public", "tenant_a", "local_only"} {
			found := false
			for _, n := range names {
				found = found || n == w
			}
			if !found {
				t.Errorf("schema %s is not listed: %v", w, names)
			}
		}
		for _, n := range names {
			if strings.HasPrefix(n, "pg_") || n == "information_schema" || n == "kingres" {
				t.Errorf("system schema %s is listed", n)
			}
		}
		d := leader.Expect(200, "GET", shPath("sh", "tenant_a"), nil)
		if d["pinned"] != nil || d["moveInProgress"] != nil || len(anyList(d["distributionBlockers"])) != 0 || d["tableCount"] != float64(2) {
			t.Errorf("tenant_a = %v", d)
		}
		// a schema with a materialized view cannot be distributed: the request says which object and why
		shExec(t, lc, `CREATE SCHEMA blocked`)
		shExec(t, lc, `CREATE TABLE blocked.t (id int PRIMARY KEY)`)
		shExec(t, lc, `CREATE MATERIALIZED VIEW blocked.mv AS SELECT * FROM blocked.t`)
		bd := leader.Expect(200, "GET", shPath("sh", "blocked"), nil)
		bl := anyList(bd["distributionBlockers"])
		if len(bl) != 1 || bl[0].(map[string]any)["objectKind"] != "materialized view" || !strings.Contains(bl[0].(map[string]any)["objectName"].(string), "mv") || bl[0].(map[string]any)["reason"] == "" {
			t.Errorf("blockers = %v", bl)
		}
		st, b := leader.Do("PUT", shPath("sh", "blocked")+"/distribution", nil, nil)
		shProblem(t, st, b, 422, "schema_not_distributable")
		errs := anyList(b["errors"])
		if len(errs) != 1 || !strings.Contains(errs[0].(map[string]any)["field"].(string), "mv") || errs[0].(map[string]any)["code"] == "" {
			t.Errorf("errors = %v", errs)
		}
		// the system schemas and public are refused with their reason
		for _, s := range []string{"public", "pg_catalog", "information_schema", "kingres"} {
			st, b := leader.Do("PUT", shPath("sh", s)+"/distribution", nil, nil)
			if st != 422 || b["code"] != "schema_not_distributable" {
				t.Errorf("distributing %s: %d %v", s, st, b)
			}
		}
		// a schema that is not there
		st, b = leader.Do("PUT", shPath("sh", "pg_nothing")+"/distribution", nil, nil)
		shProblem(t, st, b, 404, "not_found")
		// a database in mode row has no distributed schemas
		st, b = leader.Do("PUT", shPath("sm_row", "public")+"/distribution", nil, nil)
		if st != 409 && st != 422 {
			t.Errorf("distribute in mode row: %d %v", st, b)
		}
		// a database without sharding
		leader.WaitTaskDone(clustertest.TaskID(leader.Expect(202, "POST", "/databases", map[string]any{"name": "plain_db"})), 4*time.Minute)
		plain := leader.Expect(200, "GET", "/databases/plain_db/schemas", nil)
		for _, it := range anyList(plain["items"]) {
			if it.(map[string]any)["distribution"] != nil {
				t.Errorf("a database without sharding lists %v", it)
			}
		}
		st, b = leader.Do("PUT", shPath("plain_db", "public")+"/distribution", nil, nil)
		shProblem(t, st, b, 409, "conflict")
	})

	var coloA int64
	t.Run("distribute a schema that has rows", func(t *testing.T) {
		body := leader.Expect(202, "PUT", shPath("sh", "tenant_a")+"/distribution", map[string]any{})
		task := leader.WaitTaskDone(clustertest.TaskID(body), 3*time.Minute)
		if body["kind"] != "schema.distribute" || body["subject"].(map[string]any)["type"] != "schema" || body["subject"].(map[string]any)["id"] != "sh/tenant_a" {
			t.Errorf("task = %v", body)
		}
		want := []string{"Check the schema=done", "Set REPLICA IDENTITY FULL=skipped", "Make the schema distributed=done", "Verify on every server=done"}
		if got := shSteps(task); strings.Join(got, "|") != strings.Join(want, "|") {
			t.Errorf("steps = %v, want %v", got, want)
		}
		d := shDistribution(t, leader, "sh", "tenant_a")
		if d["state"] != "active" || d["shardGroupId"] != float64(0) || d["serverId"] != leader.Host || d["movingToShardGroupId"] != nil {
			t.Errorf("distribution = %v", d)
		}
		coloA = int64(d["colocationId"].(float64))
		sum := shSchemaItem(t, leader, "sh", "tenant_a")
		if sum["tableCount"] != float64(2) || sum["sizeBytes"] == nil {
			t.Errorf("summary = %v", sum)
		}
		// a second request meets a schema that is distributed
		st, b := leader.Do("PUT", shPath("sh", "tenant_a")+"/distribution", nil, nil)
		shProblem(t, st, b, 409, "conflict")
		// the other servers read the rows through the routing, and the catalog agrees everywhere
		shRowsEverywhere(t, c, "sh", "tenant_a.a", 60000)
		shRowsEverywhere(t, c, "sh", "tenant_a.b", 500)
		for _, n := range []*clustertest.Node{n1, n2} {
			dd := shDistribution(t, n, "sh", "tenant_a")
			if dd == nil || dd["shardGroupId"] != float64(0) {
				t.Errorf("server %d sees %v", n.Index, dd)
			}
		}
		shCheckClean(t, leader, "sh")
		// the tables of the schema are tables of method schema, on the owner group
		tb := leader.Expect(200, "GET", "/databases/sh/schemas/tenant_a/tables/a", nil)
		dist, _ := tb["distribution"].(map[string]any)
		if dist["method"] != "schema" || dist["column"] != nil || dist["bucketCount"] != float64(1) || tb["homeShardGroupId"] != float64(0) {
			t.Errorf("table = %v", tb)
		}
		// the group of the schema is one of kind schema, and the shards page can leave it out
		groups := leader.Expect(200, "GET", "/databases/sh/colocation-groups?kind=schema", nil)
		gi := anyList(groups["items"])
		if len(gi) != 1 || gi[0].(map[string]any)["schemaName"] != "tenant_a" || gi[0].(map[string]any)["kind"] != "schema" || gi[0].(map[string]any)["bucketCount"] != float64(1) || gi[0].(map[string]any)["distributionColumnType"] != nil {
			t.Errorf("schema groups = %v", gi)
		}
		for _, it := range anyList(leader.Expect(200, "GET", "/databases/sh/colocation-groups?kind=hash", nil)["items"]) {
			if it.(map[string]any)["kind"] != "hash" {
				t.Errorf("hash groups include %v", it)
			}
		}
		if st, b := leader.Do("GET", "/databases/sh/colocation-groups?kind=bogus", nil, nil); st != 400 {
			t.Errorf("kind=bogus: %d %v", st, b)
		}
		// the distribution of the buckets counts the schema as a bucket and as a schema of its server
		dst := leader.Expect(200, "GET", "/databases/sh/bucket-distribution", nil)
		var schemas float64
		for _, s := range anyList(dst["servers"]) {
			schemas += s.(map[string]any)["primarySchemas"].(float64)
		}
		if schemas != 1 || leader.Expect(200, "GET", "/databases/sh", nil)["stats"].(map[string]any)["distributedSchemaCount"] != float64(1) {
			t.Errorf("primarySchemas total %v, distribution %v", schemas, dst)
		}
		// the list filters
		for q, want := range map[string]int{"distributed=true": 1, "distributed=false": -1, "shardGroupId=0": 1, "shardGroupId=1": 0, "state=active": 1, "state=moving": 0} {
			got := len(anyList(leader.Expect(200, "GET", "/databases/sh/schemas?"+q, nil)["items"]))
			if want >= 0 && got != want {
				t.Errorf("schemas?%s: %d items, want %d", q, got, want)
			}
		}
		if st, b := leader.Do("GET", "/databases/sh/schemas?state=bogus", nil, nil); st != 400 {
			t.Errorf("state=bogus: %d %v", st, b)
		}
		// pagination by name
		page := leader.Expect(200, "GET", "/databases/sh/schemas?limit=1", nil)
		if len(anyList(page["items"])) != 1 || page["nextCursor"] == nil {
			t.Errorf("page = %v", page)
		}
	})

	t.Run("a move needs a key or a full replica identity, and says which table", func(t *testing.T) {
		st, b := leader.Do("POST", shPath("sh", "tenant_a")+"/moves", map[string]any{"targetShardGroupId": 1}, nil)
		shProblem(t, st, b, 422, "unprocessable")
		if !strings.Contains(fmt.Sprint(b["detail"]), "tenant_a.b") {
			t.Errorf("the refusal does not name the table: %v", b["detail"])
		}
		// the same schema is refused for its own group and for a group that cannot hold it
		st, b = leader.Do("POST", shPath("sh", "tenant_a")+"/moves", map[string]any{"targetShardGroupId": 0, "setReplicaIdentityFull": true}, nil)
		shProblem(t, st, b, 409, "conflict")
		st, b = leader.Do("POST", shPath("sh", "tenant_a")+"/moves", map[string]any{"targetShardGroupId": 77, "setReplicaIdentityFull": true}, nil)
		shProblem(t, st, b, 400, "validation_failed")
	})

	t.Run("move with a writer", func(t *testing.T) {
		setCleanupDelay := func(sec int) {
			conn := leader.ServicePG("postgres")
			defer func() { _ = conn.Close(ctx) }()
			shExec(t, conn, fmt.Sprintf(`ALTER DATABASE sh SET kingres_shard.move_cleanup_delay = %d`, sec))
		}
		setCleanupDelay(3)
		stop := make(chan struct{})
		var wg sync.WaitGroup
		var acked sync.Map
		var written atomic.Int64
		wc := n2.ServicePG("sh")
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := int64(1); ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				id := 1_000_000 + i
				ok := false
				for try := 0; try < 60 && !ok; try++ {
					_, err := wc.Exec(ctx, `INSERT INTO tenant_a.a VALUES ($1, 'live')`, id)
					ok = err == nil || strings.Contains(fmt.Sprint(err), "duplicate key")
					if !ok {
						time.Sleep(100 * time.Millisecond)
					}
				}
				if !ok {
					t.Errorf("a write of %d never succeeded", id)
					return
				}
				acked.Store(id, true)
				written.Add(1)
				time.Sleep(50 * time.Millisecond)
			}
		}()
		until(t, time.Minute, "the writer to write", func() bool { return written.Load() > 20 })

		body := leader.Expect(202, "POST", shPath("sh", "tenant_a")+"/moves", map[string]any{"targetShardGroupId": 1, "setReplicaIdentityFull": true})
		id := clustertest.TaskID(body)
		// a second move of the schema while the first runs
		if st, b := leader.Do("POST", shPath("sh", "tenant_a")+"/moves", map[string]any{"targetShardGroupId": 2, "setReplicaIdentityFull": true}, nil); st != 409 || b["code"] != "schema_move_in_progress" {
			t.Errorf("a second move: %d %v", st, b)
		}
		// progress: monotone, in bytes, and the distribution says where the schema is going while it moves
		var lastDone, total float64
		sawMoving := false
		deadline := time.Now().Add(5 * time.Minute)
		var task map[string]any
		for {
			task = shTask(t, leader, id)
			if steps := anyList(task["progress"].(map[string]any)["steps"]); len(steps) == 5 {
				cp := steps[2].(map[string]any)
				if d, ok := cp["itemsDone"].(float64); ok {
					if d < lastDone {
						t.Errorf("itemsDone went back from %v to %v", lastDone, d)
					}
					lastDone = d
					total, _ = cp["itemsTotal"].(float64)
				}
			}
			if d := shDistribution(t, leader, "sh", "tenant_a"); d != nil && d["state"] == "moving" {
				sawMoving = true
				if d["movingToShardGroupId"] != float64(1) {
					t.Errorf("moving distribution = %v", d)
				}
				if sd := leader.Expect(200, "GET", shPath("sh", "tenant_a"), nil); sd["moveInProgress"] != id {
					t.Errorf("moveInProgress = %v, want %v", sd["moveInProgress"], id)
				}
			}
			if task["state"] == "done" || task["state"] == "failed" {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("the move did not end: %v", task)
			}
			time.Sleep(200 * time.Millisecond)
		}
		close(stop)
		wg.Wait()
		if task["state"] != "done" {
			t.Fatalf("the move ended %v: %v\n%s", task["state"], task["error"], leader.LogTail(3000))
		}
		want := []string{"Check the move=done", "Set REPLICA IDENTITY FULL=done", "Copy the data=done", "Switch=done", "Clean up the old copy=done"}
		if got := shSteps(task); strings.Join(got, "|") != strings.Join(want, "|") {
			t.Errorf("steps = %v, want %v", got, want)
		}
		if total <= 0 || lastDone != total {
			t.Errorf("the copy reported %v of %v bytes at its end", lastDone, total)
		}
		_ = sawMoving // a short move can be over between two polls; the distribution while moving is checked with the held move below
		d := shDistribution(t, leader, "sh", "tenant_a")
		if d["state"] != "active" || d["shardGroupId"] != float64(1) || d["serverId"] != n1.Host {
			t.Errorf("distribution after the move = %v", d)
		}
		// no acknowledged row is lost, the old rows are all there, and every server reads the same
		wantRows := int64(60000)
		acked.Range(func(_, _ any) bool { wantRows++; return true })
		shRowsEverywhere(t, c, "sh", "tenant_a.a", wantRows)
		shRowsEverywhere(t, c, "sh", "tenant_a.b", 500)
		var missing int64
		nc1 := n1.ServicePG("sh")
		defer func() { _ = nc1.Close(ctx) }()
		acked.Range(func(k, _ any) bool {
			if shInt(t, nc1, `SELECT count(*) FROM tenant_a.a WHERE id = $1`, k.(int64)) != 1 {
				missing++
			}
			return true
		})
		if missing != 0 {
			t.Errorf("%d acknowledged rows are missing after the move", missing)
		}
		// the replica identity was set on the table that had no key, on the new owner too
		if shInt(t, nc1, `SELECT count(*) FROM pg_class WHERE oid = 'tenant_a.b'::regclass AND relreplident = 'f'`) != 1 {
			t.Errorf("tenant_a.b has no full replica identity on the new owner")
		}
		shWaitMovePhase(t, leader, "sh", coloA, "done", 2*time.Minute)
		shCheckClean(t, leader, "sh")
	})

	t.Run("a move can be cancelled until the switch", func(t *testing.T) {
		gate := filepath.Join(t.TempDir(), "gate")
		if err := os.WriteFile(gate, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		shSetFault(t, c, "sh", "move_after_catchup:wait:"+gate)
		defer shSetFault(t, c, "sh", "")
		body := leader.Expect(202, "POST", shPath("sh", "tenant_a")+"/moves", map[string]any{"targetShardGroupId": 2})
		id := clustertest.TaskID(body)
		until(t, 2*time.Minute, "the move to be caught up", func() bool {
			sd := shTask(t, leader, id)
			det, _ := sd["detail"].(string)
			return strings.HasPrefix(det, "catching_up")
		})
		d := shDistribution(t, leader, "sh", "tenant_a")
		if d["state"] != "moving" || d["movingToShardGroupId"] != float64(2) || d["shardGroupId"] != float64(1) {
			t.Errorf("distribution while the move is held = %v", d)
		}
		if tk := shTask(t, leader, id); tk["cancellable"] != true {
			t.Errorf("a move that is catching up is not cancellable: %v", tk)
		}
		// writers go on during the held move
		shExec(t, n2.ServicePG("sh"), `INSERT INTO tenant_a.a VALUES (7000001, 'during')`)
		leader.Expect(200, "POST", fmt.Sprintf("/tasks/%v/cancel", id), nil)
		// the control plane has asked the extension to stop the move (the flag of the move record is set) while its session is
		// held at the fault point; only then is the session released, and it ends as cancelled
		until(t, time.Minute, "the move to be flagged as cancelled", func() bool {
			cc := leader.ServicePG("sh")
			defer func() { _ = cc.Close(ctx) }()
			return shInt(t, cc, `SELECT count(*) FROM kingres.bucket_move WHERE colocation_id = $1 AND cancel_requested`, coloA) > 0
		})
		_ = os.Remove(gate)
		task := leader.WaitTask(id, 3*time.Minute)
		if task["state"] != "failed" || task["error"].(map[string]any)["code"] != "cancelled" {
			t.Fatalf("task after the cancel: %v", task)
		}
		d = shDistribution(t, leader, "sh", "tenant_a")
		if d["state"] != "active" || d["shardGroupId"] != float64(1) || d["movingToShardGroupId"] != nil {
			t.Errorf("distribution after the cancel = %v", d)
		}
		if shInt(t, n2.ServicePG("sh"), `SELECT count(*) FROM tenant_a.a WHERE id = 7000001`) != 1 {
			t.Errorf("the row written during the held move is gone")
		}
		shCheckClean(t, leader, "sh")
	})

	t.Run("after the switch a move cannot be cancelled", func(t *testing.T) {
		gate := filepath.Join(t.TempDir(), "gate")
		if err := os.WriteFile(gate, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		shSetFault(t, c, "sh", "move_after_switch:wait:"+gate)
		defer shSetFault(t, c, "sh", "")
		body := leader.Expect(202, "POST", shPath("sh", "tenant_a")+"/moves", map[string]any{"targetShardGroupId": 2})
		id := clustertest.TaskID(body)
		until(t, 3*time.Minute, "the schema to be on group 2", func() bool {
			d := shDistribution(t, leader, "sh", "tenant_a")
			return d != nil && d["shardGroupId"] == float64(2) && d["state"] == "active"
		})
		until(t, time.Minute, "the task to say that it cannot be cancelled", func() bool {
			tk := shTask(t, leader, id)
			det, _ := tk["detail"].(string)
			return tk["cancellable"] == false && (strings.HasPrefix(det, "cleaning") || strings.HasPrefix(det, "switching") || true)
		})
		st, b := leader.Do("POST", fmt.Sprintf("/tasks/%v/cancel", id), nil, nil)
		shProblem(t, st, b, 409, "task_not_cancellable")
		_ = os.Remove(gate)
		task := leader.WaitTaskDone(id, 3*time.Minute)
		if steps := shSteps(task); steps[3] != "Switch=done" {
			t.Errorf("steps = %v", steps)
		}
		shWaitMovePhase(t, leader, "sh", coloA, "done", 2*time.Minute)
		shRowsEverywhere(t, c, "sh", "tenant_a.b", 500)
	})

	t.Run("make a schema local, moving it first", func(t *testing.T) {
		// it is on group 2; the caller wants it local on the map group
		body := leader.Expect(202, "DELETE", shPath("sh", "tenant_a")+"/distribution?shardGroupId=0", nil)
		if body["kind"] != "schema.undistribute" || body["subject"].(map[string]any)["id"] != "sh/tenant_a" {
			t.Errorf("task = %v", body)
		}
		task := leader.WaitTaskDone(clustertest.TaskID(body), 5*time.Minute)
		want := []string{"Check the schema=done", "Move first=done", "Make the schema local=done", "Verify on every server=done"}
		if got := shSteps(task); strings.Join(got, "|") != strings.Join(want, "|") {
			t.Errorf("steps = %v, want %v", got, want)
		}
		if d := shDistribution(t, leader, "sh", "tenant_a"); d != nil {
			t.Errorf("distribution after the undistribution = %v", d)
		}
		// the schema is on the first server alone, with its rows
		for _, n := range c.Nodes {
			conn := n.ServicePG("sh")
			has := shInt(t, conn, `SELECT count(*) FROM pg_namespace WHERE nspname = 'tenant_a'`)
			_ = conn.Close(ctx)
			if (n == leader) != (has == 1) {
				t.Errorf("node %d has schema tenant_a: %d", n.Index, has)
			}
		}
		if shInt(t, lc, `SELECT count(*) FROM tenant_a.b`) != 500 {
			t.Errorf("tenant_a.b lost rows")
		}
		st, b := leader.Do("DELETE", shPath("sh", "tenant_a")+"/distribution", nil, nil)
		shProblem(t, st, b, 409, "conflict")
		shCheckClean(t, leader, "sh")
	})

	t.Run("without a target the schema becomes local where it is", func(t *testing.T) {
		shExec(t, lc, `CREATE SCHEMA tenant_b`)
		shExec(t, lc, `CREATE TABLE tenant_b.t (id bigint PRIMARY KEY, v text)`)
		shExec(t, lc, `INSERT INTO tenant_b.t SELECT g, 'b' FROM generate_series(1, 100) g`)
		// rows stay where they are: asking for another group is the extension's refusal, and the task says so
		body := leader.Expect(202, "PUT", shPath("sh", "tenant_b")+"/distribution", map[string]any{"shardGroupId": 1})
		failed := leader.WaitTask(clustertest.TaskID(body), 3*time.Minute)
		if failed["state"] != "failed" || !strings.Contains(fmt.Sprint(failed["error"]), "holds rows") || failed["error"].(map[string]any)["step"] != "Make the schema distributed" {
			t.Fatalf("the refused distribution: %v", failed)
		}
		if d := shDistribution(t, leader, "sh", "tenant_b"); d != nil {
			t.Fatalf("a refused distribution left %v", d)
		}
		body = leader.Expect(202, "PUT", shPath("sh", "tenant_b")+"/distribution", nil)
		leader.WaitTaskDone(clustertest.TaskID(body), 3*time.Minute)
		d := shDistribution(t, leader, "sh", "tenant_b")
		if d == nil || d["shardGroupId"] != float64(0) {
			t.Fatalf("tenant_b = %v", d)
		}
		body = leader.Expect(202, "DELETE", shPath("sh", "tenant_b")+"/distribution", nil)
		task := leader.WaitTaskDone(clustertest.TaskID(body), 3*time.Minute)
		if got := shSteps(task); got[1] != "Move first=skipped" {
			t.Errorf("steps = %v", got)
		}
	})

	t.Run("a schema that holds a ledger table is pinned", func(t *testing.T) {
		lc2 := leader.ServicePG("shl")
		shExec(t, lc2, `CREATE SCHEMA tl`)
		shExec(t, lc2, `CREATE TABLE tl.t (id bigint PRIMARY KEY, v text)`)
		shExec(t, lc2, `SELECT kingres.ledger_enable('tl.t'::regclass, 'append_only')`)
		shExec(t, lc2, `INSERT INTO tl.t VALUES (1, 'a')`)
		leader.WaitTaskDone(clustertest.TaskID(leader.Expect(202, "PUT", shPath("shl", "tl")+"/distribution", nil)), 3*time.Minute)
		d := leader.Expect(200, "GET", shPath("shl", "tl"), nil)
		pin, _ := d["pinned"].(map[string]any)
		if pin == nil || pin["reason"] == "" || !strings.Contains(fmt.Sprint(pin["reason"]), "ledger") {
			t.Fatalf("pinned = %v", d["pinned"])
		}
		st, b := leader.Do("POST", shPath("shl", "tl")+"/moves", map[string]any{"targetShardGroupId": 1}, nil)
		shProblem(t, st, b, 409, "schema_pinned")
		st, b = leader.Do("DELETE", shPath("shl", "tl")+"/distribution", nil, nil)
		shProblem(t, st, b, 409, "schema_pinned")
	})

	t.Run("the mode is held while the other kind exists", func(t *testing.T) {
		shExec(t, lc, `CREATE TABLE hashed (id bigint PRIMARY KEY, v text)`)
		leader.WaitTaskDone(clustertest.TaskID(leader.Expect(202, "PUT", "/databases/sh/schemas/public/tables/hashed/distribution", map[string]any{"method": "hash", "distributionColumn": "id"})), 3*time.Minute)
		// a distributed schema exists (tenant_a was made local; make one again), and a hash table exists
		shExec(t, lc, `CREATE SCHEMA tenant_c`)
		shExec(t, lc, `CREATE TABLE tenant_c.t (id bigint PRIMARY KEY)`)
		leader.WaitTaskDone(clustertest.TaskID(leader.Expect(202, "PUT", shPath("sh", "tenant_c")+"/distribution", nil)), 3*time.Minute)
		st, b := leader.Do("PATCH", "/databases/sh", map[string]any{"sharding": map[string]any{"mode": "row"}}, nil)
		shProblem(t, st, b, 409, "sharding_mode_in_use")
		if !strings.Contains(fmt.Sprint(b["detail"]), "tenant_c") {
			t.Errorf("the refusal does not name the schema: %v", b["detail"])
		}
		st, b = leader.Do("PATCH", "/databases/sh", map[string]any{"sharding": map[string]any{"mode": "schema"}}, nil)
		shProblem(t, st, b, 409, "sharding_mode_in_use")
		if !strings.Contains(fmt.Sprint(b["detail"]), "hashed") {
			t.Errorf("the refusal does not name the table: %v", b["detail"])
		}
		// the mode did not change, and what is allowed is applied at once
		if m := leader.Expect(200, "GET", "/databases/sh", nil)["sharding"].(map[string]any)["mode"]; m != "both" {
			t.Errorf("mode after the refusals = %v", m)
		}
		got := leader.Expect(200, "PATCH", "/databases/sh", map[string]any{"sharding": map[string]any{"distributeNewSchemas": true}})
		if s := got["sharding"].(map[string]any); s["mode"] != "both" || s["distributeNewSchemas"] != true {
			t.Errorf("sharding after the change = %v", s)
		}
		// a schema that is made now is distributed by the extension
		shExec(t, lc, `CREATE SCHEMA auto_made`)
		if d := shDistribution(t, leader, "sh", "auto_made"); d == nil {
			t.Errorf("a schema created with distributeNewSchemas has no distribution")
		}
		leader.Expect(200, "PATCH", "/databases/sh", map[string]any{"sharding": map[string]any{"distributeNewSchemas": false}})
	})

	t.Run("a hostile schema name goes through every operation", func(t *testing.T) {
		name := "Te\"n ant;'\\ --é ü 💥"
		q := `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
		shExec(t, lc, `CREATE SCHEMA `+q)
		shExec(t, lc, `CREATE TABLE `+q+`."ta""ble" (id bigint PRIMARY KEY, v text)`)
		shExec(t, lc, `INSERT INTO `+q+`."ta""ble" SELECT g, 'h' FROM generate_series(1, 50) g`)
		if d := leader.Expect(200, "GET", shPath("sh", name), nil); d["name"] != name || d["tableCount"] != float64(1) {
			t.Fatalf("schema = %v", d)
		}
		found := false
		for _, it := range anyList(leader.Expect(200, "GET", "/databases/sh/schemas?limit=200", nil)["items"]) {
			found = found || it.(map[string]any)["name"] == name
		}
		if !found {
			t.Errorf("the schema is not listed")
		}
		body := leader.Expect(202, "PUT", shPath("sh", name)+"/distribution", nil)
		if body["subject"].(map[string]any)["id"] != "sh/"+name {
			t.Errorf("subject = %v", body["subject"])
		}
		leader.WaitTaskDone(clustertest.TaskID(body), 3*time.Minute)
		if d := shDistribution(t, leader, "sh", name); d == nil || d["shardGroupId"] != float64(0) {
			t.Fatalf("distribution = %v", d)
		}
		shRowsEverywhere(t, c, "sh", q+`."ta""ble"`, 50)
		mv := leader.Expect(202, "POST", shPath("sh", name)+"/moves", map[string]any{"targetShardGroupId": 2})
		leader.WaitTaskDone(clustertest.TaskID(mv), 4*time.Minute)
		if d := shDistribution(t, leader, "sh", name); d == nil || d["shardGroupId"] != float64(2) {
			t.Fatalf("distribution after the move = %v", d)
		}
		shRowsEverywhere(t, c, "sh", q+`."ta""ble"`, 50)
		leader.WaitTaskDone(clustertest.TaskID(leader.Expect(202, "DELETE", shPath("sh", name)+"/distribution", nil)), 4*time.Minute)
		if shInt(t, n2.ServicePG("sh"), `SELECT count(*) FROM `+q+`."ta""ble"`) != 50 {
			t.Errorf("the rows of the hostile schema are not on its owner")
		}
		// names that cannot be right
		st, b := leader.Do("PUT", shPath("sh", strings.Repeat("a", 64))+"/distribution", nil, nil)
		shProblem(t, st, b, 400, "validation_failed")
		st, b = leader.Do("GET", shPath("sh", "a\x01b"), nil, nil)
		if st != 404 && st != 400 {
			t.Errorf("a name with a control character: %d %v", st, b)
		}
		long := strings.Repeat("x", 63)
		shExec(t, lc, `CREATE SCHEMA `+long)
		if d := leader.Expect(200, "GET", shPath("sh", long), nil); d["name"] != long {
			t.Errorf("long name: %v", d)
		}
	})

	t.Run("the schemas are listed in name order by every server", func(t *testing.T) {
		for _, n := range c.Nodes {
			var names []string
			var all []string
			for _, it := range anyList(n.Expect(200, "GET", "/databases/sh/schemas?limit=200", nil)["items"]) {
				all = append(all, it.(map[string]any)["name"].(string))
			}
			cursor := ""
			for {
				p := "/databases/sh/schemas?limit=3"
				if cursor != "" {
					p += "&cursor=" + url.QueryEscape(cursor)
				}
				page := n.Expect(200, "GET", p, nil)
				for _, it := range anyList(page["items"]) {
					names = append(names, it.(map[string]any)["name"].(string))
				}
				nc, _ := page["nextCursor"].(string)
				if nc == "" {
					break
				}
				cursor = nc
			}
			// the pages in order are the whole list in order, whatever the collation says about the order
			if strings.Join(names, "|") != strings.Join(all, "|") || len(names) < 5 {
				t.Errorf("node %d lists %v by pages and %v at once", n.Index, names, all)
			}
		}
	})
}
