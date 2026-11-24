// SPDX-License-Identifier: AGPL-3.0-only

package clustertest_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/clustertest"
)

// The time-partitioning policy of a user-partitioned distributed table (phase 12, section 11; increments St-D1a and St-D1b) through the REST API, on a
// cluster of three servers: a policy is set, planned and run by hand and by the scheduler; a run creates the partitions ahead on every server, converts the
// old ones to columnar storage on the servers that hold their leaves, and drops the partitions older than the retention; the listing of the partitions says how
// they are stored; a policy that needs more leaves than kingres_shard.max_partition_leaves allows is refused; a group that is busy makes the run partial.

func dpLeaves(t *testing.T, c *clustertest.Cluster, db, rel string) (total, columnar, full int) {
	t.Helper()
	for _, n := range c.Nodes {
		var got string
		got = stScalar(t, n, db, fmt.Sprintf(`SELECT count(*) || ',' || count(*) FILTER (WHERE a.amname = 'columnar') || ',' || count(*) FILTER (WHERE c.relreplident = 'f')
			FROM pg_class c JOIN pg_am a ON a.oid = c.relam WHERE c.relkind = 'r' AND c.relispartition AND c.relname ~ '%s'`, rel))
		var a, b, d int
		if _, err := fmt.Sscanf(got, "%d,%d,%d", &a, &b, &d); err != nil {
			t.Fatalf("node %d: %q: %v", n.Index, got, err)
		}
		total, columnar, full = total+a, columnar+b, full+d
	}
	return
}

func TestPartitionPolicyOfADistributedTable(t *testing.T) {
	c := clustertest.FormWithExtra(t, 3, []string{"-partition-scheduler-interval", "3s"}, func(leader *clustertest.Node) {
		body := leader.Expect(202, "POST", "/databases", map[string]any{"name": "dp", "sharding": map[string]any{"enabled": true, "bucketCount": 6}})
		leader.WaitTaskDone(clustertest.TaskID(body), 4*time.Minute)
	})
	leader := c.Leader()
	tbl := func(name string) string { return "/databases/dp/schemas/public/tables/" + name }
	day := func(offset int) string {
		return time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, offset).Format("2006-01-02")
	}
	taskDone := func(body map[string]any) map[string]any {
		t.Helper()
		got := leader.WaitTask(clustertest.TaskID(body), 5*time.Minute)
		if got["state"] != "done" {
			t.Fatalf("task ended %v: %v\n%s", got["state"], got["error"], leader.LogTail(3000))
		}
		return got
	}
	counts := func(db, q string) string {
		var out []string
		for _, n := range c.Nodes {
			out = append(out, stScalar(t, n, db, q))
		}
		return strings.Join(out, ",")
	}

	// Two old partitions: one that the retention takes (ten days ago), one that the conversion takes (three days ago).
	stExec(t, leader, "dp",
		`CREATE TABLE ev (cust int NOT NULL, ts timestamptz NOT NULL, id int NOT NULL, v int, PRIMARY KEY (cust, ts, id)) PARTITION BY RANGE (ts)`,
		fmt.Sprintf(`CREATE TABLE ev_old PARTITION OF ev FOR VALUES FROM ('%s 00:00:00+00') TO ('%s 00:00:00+00')`, day(-10), day(-9)),
		fmt.Sprintf(`CREATE TABLE ev_third PARTITION OF ev FOR VALUES FROM ('%s 00:00:00+00') TO ('%s 00:00:00+00')`, day(-3), day(-2)),
		fmt.Sprintf(`INSERT INTO ev SELECT g %% 13, '%s 00:00:00+00'::timestamptz + (g %% 1000) * interval '1 minute', g, g %% 50 FROM generate_series(1, 400) g`, day(-10)),
		fmt.Sprintf(`INSERT INTO ev SELECT g %% 13, '%s 00:00:00+00'::timestamptz + (g %% 1000) * interval '1 minute', 1000 + g, g %% 50 FROM generate_series(1, 600) g`, day(-3)))
	taskDone(leader.Expect(202, "PUT", tbl("ev")+"/distribution", map[string]any{"method": "hash", "distributionColumn": "cust", "mode": "blocking"}))
	if got := counts("dp", `SELECT count(*) FROM ev`); got != "1000,1000,1000" {
		t.Fatalf("rows after the distribution: %s", got)
	}

	t.Run("a policy of a distributed table is set and planned", func(t *testing.T) {
		if st, p := leader.Do("GET", tbl("ev")+"/partition-policy", nil, nil); st != 404 || p["code"] != "not_found" {
			t.Fatalf("no policy yet: %d %v", st, p)
		}
		pol := map[string]any{"partitionEvery": "P1D", "createAhead": "P2D", "retention": "P5D", "retentionAction": "drop", "columnarAfter": "P2D", "enabled": false}
		put := leader.Expect(200, "PUT", tbl("ev")+"/partition-policy", pol)
		if put["partitionColumn"] != "ts" || put["partitionColumnType"] != "timestamptz" || put["partitionEvery"] != "P1D" {
			t.Fatalf("put: %v", put)
		}
		plan := leader.Expect(200, "POST", tbl("ev")+"/partition-policy/plan", map[string]any{})
		kinds := map[string]int{}
		for _, a := range plan["actions"].([]any) {
			kinds[a.(map[string]any)["kind"].(string)]++
		}
		if kinds["create"] != 7 || kinds["convert"] != 1 || kinds["drop"] != 1 {
			// the days -5, -4 and -2 to +2 are made (-3 exists); only the partition of day -3 is there to convert yet (a run converts the two it makes for -5 and -4 too)
			t.Errorf("the plan: %v", kinds)
		}
		if got := counts("dp", `SELECT count(*) FROM kingres.dist_partition WHERE parent = 'ev'::regclass`); got != "2,2,2" {
			t.Errorf("a plan made partitions: %s", got)
		}
		// a policy that would need more leaves than the limit is refused with the numbers
		st, p := leader.Do("PUT", tbl("ev")+"/partition-policy", map[string]any{"partitionEvery": "PT1H", "createAhead": "P3650D"}, nil)
		if st != 409 || p["code"] != "partition_policy_conflict" || !strings.Contains(fmt.Sprint(p["detail"]), "max_partition_leaves is 8192") {
			t.Errorf("a policy over the leaf budget: %d %v", st, p)
		}
		if got := leader.Expect(200, "GET", tbl("ev")+"/partition-policy", nil); got["partitionEvery"] != "P1D" {
			t.Errorf("the refused policy changed the policy: %v", got)
		}
	})

	t.Run("a run creates, converts and retires on every server", func(t *testing.T) {
		before := counts("dp", `SELECT count(*) FROM ev`)
		run := leader.Expect(202, "POST", tbl("ev")+"/partition-policy/runs", map[string]any{"asOf": time.Now().UTC().Format(time.RFC3339)})
		got := taskDone(run)
		if got["kind"] != "partition.maintain" {
			t.Errorf("kind %v", got["kind"])
		}
		runs := leader.Expect(200, "GET", tbl("ev")+"/partition-policy/runs?state=done", nil)
		var mine map[string]any
		for _, r := range runs["items"].([]any) {
			if m := r.(map[string]any); m["taskId"] == got["id"] {
				mine = m
			}
		}
		if mine == nil {
			t.Fatalf("the run is not in the list: %v", runs["items"])
		}
		full := leader.Expect(200, "GET", fmt.Sprintf("%s/partition-policy/runs/%v", tbl("ev"), mine["id"]), nil)
		kinds := map[string]int{}
		for _, a := range full["actions"].([]any) {
			m := a.(map[string]any)
			if m["state"] != "done" {
				t.Errorf("an action that is not done: %v", m)
			}
			// the partitions of a distributed table are not tables of this database: no size before or after (contract 0.23.0)
			if m["kind"] != "create" && (m["bytesBefore"] != nil || m["bytesAfter"] != nil) {
				t.Errorf("an action of a distributed partition has sizes: %v", m)
			}
			kinds[m["kind"].(string)]++
		}
		if kinds["create"] != 7 || kinds["convert"] != 3 || kinds["drop"] != 1 {
			t.Errorf("the actions: %v", kinds)
		}
		// the catalog of every server has the partitions: seven made, the one that existed, and not the one that was dropped
		if got := counts("dp", `SELECT count(*) FROM kingres.dist_partition WHERE parent = 'ev'::regclass`); got != "8,8,8" {
			t.Errorf("partitions in the catalog: %s", got)
		}
		if got := counts("dp", `SELECT count(*) FROM kingres.dist_partition WHERE parent = 'ev'::regclass AND partition_name = 'ev_old'`); got != "0,0,0" {
			t.Errorf("the old partition is still there: %s", got)
		}
		total, columnar, full6 := dpLeaves(t, c, "dp", `^(ev_p[0-9]+|ev_third)_b[0-9]{4}$`)
		if total != 48 || columnar != 18 || full6 != 18 {
			t.Errorf("leaves: %d, columnar %d, with REPLICA IDENTITY FULL %d; want 48, 18, 18", total, columnar, full6)
		}
		if n, _, _ := dpLeaves(t, c, "dp", `^ev_old_b[0-9]{4}$`); n != 0 {
			t.Errorf("%d leaves of the dropped partition are left", n)
		}
		after := counts("dp", `SELECT count(*) FROM ev`)
		if before != "1000,1000,1000" || after != "600,600,600" {
			t.Errorf("rows: %s before, %s after (the 400 rows of the dropped partition are gone, the 600 of the converted one are not)", before, after)
		}
		list := leader.Expect(200, "GET", tbl("ev")+"/partitions", nil)
		am := map[string]string{}
		for _, it := range list["items"].([]any) {
			m := it.(map[string]any)
			s, _ := m["accessMethod"].(string)
			am[m["name"].(string)] = s
			if m["leafCount"] != float64(6) {
				t.Errorf("leafCount of %v: %v", m["name"], m["leafCount"])
			}
		}
		col := 0
		for _, v := range am {
			if v == "columnar" {
				col++
			}
		}
		if len(am) != 8 || col != 3 || am["ev_third"] != "columnar" {
			t.Errorf("the partitions and their storage: %v", am)
		}
		if p := am["ev_p"+strings.ReplaceAll(day(1), "-", "")]; p != "heap" {
			t.Errorf("a partition ahead is %q", p)
		}
		var third map[string]any
		for _, it := range list["items"].([]any) {
			if m := it.(map[string]any); m["name"] == "ev_third" {
				third = m
			}
		}
		if third["rangeFrom"] == nil || !strings.HasPrefix(fmt.Sprint(third["rangeFrom"]), day(-3)) {
			t.Errorf("the bounds of a partition: %v", third)
		}
		pol := leader.Expect(200, "GET", tbl("ev")+"/partition-policy", nil)
		h, _ := pol["health"].(map[string]any)
		if h["ok"] != true || pol["lastRun"].(map[string]any)["state"] != "done" {
			t.Errorf("the policy after the run: %v", pol)
		}
		// nothing is left claimed or half done
		if got := counts("dp", `SELECT count(*) FROM kingres.table_operation_lock`); got != "0,0,0" {
			t.Errorf("claims left: %s", got)
		}
		if got := stScalar(t, leader, "dp", `SELECT count(*) FROM kingres.check_cluster()`); got != "0" {
			t.Errorf("check_cluster: %s", got)
		}
	})

	t.Run("a table that is busy makes the run partial, and the scheduler finishes it", func(t *testing.T) {
		// the group is claimed (as a conversion does): the run creates nothing, and says retry later
		stExec(t, leader, "dp", `SELECT kingres.begin_leaf_conversion('ev'::regclass, 'ev_third')`)
		leader.Expect(200, "PUT", tbl("ev")+"/partition-policy", map[string]any{"partitionEvery": "P1D", "createAhead": "P4D", "retention": "P5D", "columnarAfter": "P2D", "enabled": false})
		run := leader.Expect(202, "POST", tbl("ev")+"/partition-policy/runs", map[string]any{"asOf": time.Now().UTC().Format(time.RFC3339)})
		got := leader.WaitTask(clustertest.TaskID(run), 5*time.Minute)
		if got["state"] != "done" {
			t.Fatalf("the task: %v %v", got["state"], got["error"])
		}
		runs := leader.Expect(200, "GET", tbl("ev")+"/partition-policy/runs?state=partial", nil)
		items := runs["items"].([]any)
		if len(items) == 0 {
			t.Fatalf("no partial run: %v", leader.Expect(200, "GET", tbl("ev")+"/partition-policy/runs", nil))
		}
		full := leader.Expect(200, "GET", fmt.Sprintf("%s/partition-policy/runs/%v", tbl("ev"), items[0].(map[string]any)["id"]), nil)
		failed := 0
		for _, a := range full["actions"].([]any) {
			m := a.(map[string]any)
			if m["state"] == "failed" && strings.Contains(fmt.Sprint(m["detail"]), "retry_later") {
				failed++
			}
		}
		if failed == 0 {
			t.Errorf("no action says retry_later: %v", full["actions"])
		}
		if got := counts("dp", `SELECT count(*) FROM kingres.dist_partition WHERE parent = 'ev'::regclass`); got != "8,8,8" {
			t.Errorf("a busy group changed the partitions: %s", got)
		}
		// release the claim; the next run is done and has made the partitions
		stExec(t, leader, "dp", `SELECT kingres.end_leaf_conversion(op_id) FROM kingres.table_operation WHERE kind = 'convert_leaf' AND finished_at IS NULL`)
		run = leader.Expect(202, "POST", tbl("ev")+"/partition-policy/runs", map[string]any{"asOf": time.Now().UTC().Format(time.RFC3339)})
		taskDone(run)
		if got := counts("dp", `SELECT count(*) FROM kingres.dist_partition WHERE parent = 'ev'::regclass`); got != "10,10,10" {
			t.Errorf("partitions after the run that could: %s", got)
		}
	})

	t.Run("the scheduler of the leader runs the policy of a distributed table that is due", func(t *testing.T) {
		stExec(t, leader, "dp",
			`CREATE TABLE sched_d (cust int NOT NULL, ts timestamptz NOT NULL, id int NOT NULL, PRIMARY KEY (cust, ts, id)) PARTITION BY RANGE (ts)`,
			fmt.Sprintf(`CREATE TABLE sched_d_first PARTITION OF sched_d FOR VALUES FROM ('%s 00:00:00+00') TO ('%s 00:00:00+00')`, day(-1), day(0)))
		taskDone(leader.Expect(202, "PUT", tbl("sched_d")+"/distribution", map[string]any{"method": "hash", "distributionColumn": "cust", "mode": "blocking"}))
		leader.Expect(200, "PUT", tbl("sched_d")+"/partition-policy", map[string]any{"partitionEvery": "P1D", "createAhead": "P2D", "retention": "P30D"})
		var items []any
		for end := time.Now().Add(3 * time.Minute); time.Now().Before(end); time.Sleep(time.Second) {
			runs := leader.Expect(200, "GET", tbl("sched_d")+"/partition-policy/runs?state=done", nil)
			if items = runs["items"].([]any); len(items) > 0 {
				break
			}
		}
		if len(items) == 0 {
			t.Fatalf("the scheduler started no run %s", leader.LogTail(3000))
		}
		if r := items[len(items)-1].(map[string]any); r["trigger"] != "schedule" || r["startedBy"] != "system" {
			t.Errorf("run %v", r)
		}
		if got := counts("dp", `SELECT count(*) FROM kingres.dist_partition WHERE parent = 'sched_d'::regclass`); got != "4,4,4" {
			t.Errorf("partitions on every server: %s (yesterday's and three more)", got)
		}
		if n, _, _ := dpLeaves(t, c, "dp", `^sched_d_p[0-9]+_b[0-9]{4}$`); n != 18 {
			t.Errorf("%d leaves of the three new partitions, want 18", n)
		}
	})
}

// A database that was made with kingres_timepart 0.2 (before the conversion of the leaves of a distributed table) is brought to the version of the package on every
// server by the leader, with no step of an operator, and the conversion then works.
func TestTimepartUpdatedOnEveryServer(t *testing.T) {
	c := clustertest.FormWithExtra(t, 3, []string{"-partition-scheduler-interval", "3s"}, func(leader *clustertest.Node) {
		body := leader.Expect(202, "POST", "/databases", map[string]any{"name": "xu", "sharding": map[string]any{"enabled": true, "bucketCount": 6}})
		leader.WaitTaskDone(clustertest.TaskID(body), 4*time.Minute)
	})
	leader := c.Leader()
	day := func(offset int) string {
		return time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, offset).Format("2006-01-02")
	}
	versions := func() string {
		var out []string
		for _, n := range c.Nodes {
			out = append(out, stScalar(t, n, "xu", `SELECT coalesce(extversion, 'none') FROM (SELECT 1) x LEFT JOIN pg_extension e ON e.extname = 'kingres_timepart'`))
		}
		return strings.Join(out, ",")
	}
	if got := versions(); got != "0.3,0.3,0.3" {
		t.Fatalf("a database that was made has %s", got)
	}
	// the state of a database from before: version 0.2 everywhere (the extension is dropped and made again at 0.2; it has no policy yet)
	for _, n := range c.Nodes {
		stExec(t, n, "xu", `DROP EXTENSION kingres_timepart`, `CREATE EXTENSION kingres_timepart VERSION '0.2'`)
	}
	// the leader updates it (the job runs every ten intervals of the scheduler, 30 s here)
	var got string
	for end := time.Now().Add(4 * time.Minute); time.Now().Before(end); time.Sleep(2 * time.Second) {
		if got = versions(); got == "0.3,0.3,0.3" {
			break
		}
	}
	if got != "0.3,0.3,0.3" {
		t.Fatalf("kingres_timepart was not updated on every server: %s\n%s", got, leader.LogTail(3000))
	}
	// the conversion of the leaves works on what was updated
	tbl := "/databases/xu/schemas/public/tables/ev"
	stExec(t, leader, "xu",
		`CREATE TABLE ev (cust int NOT NULL, ts timestamptz NOT NULL, id int NOT NULL, PRIMARY KEY (cust, ts, id)) PARTITION BY RANGE (ts)`,
		fmt.Sprintf(`CREATE TABLE ev_third PARTITION OF ev FOR VALUES FROM ('%s 00:00:00+00') TO ('%s 00:00:00+00')`, day(-3), day(-2)),
		fmt.Sprintf(`INSERT INTO ev SELECT g %% 13, '%s 00:00:00+00'::timestamptz + (g %% 1000) * interval '1 minute', g FROM generate_series(1, 300) g`, day(-3)))
	dist := leader.Expect(202, "PUT", tbl+"/distribution", map[string]any{"method": "hash", "distributionColumn": "cust", "mode": "blocking"})
	if r := leader.WaitTask(clustertest.TaskID(dist), 5*time.Minute); r["state"] != "done" {
		t.Fatalf("distribute: %v", r)
	}
	leader.Expect(200, "PUT", tbl+"/partition-policy", map[string]any{"partitionEvery": "P1D", "createAhead": "P1D", "retention": "P30D", "columnarAfter": "P2D", "enabled": false})
	run := leader.Expect(202, "POST", tbl+"/partition-policy/runs", map[string]any{"asOf": time.Now().UTC().Format(time.RFC3339)})
	if r := leader.WaitTask(clustertest.TaskID(run), 5*time.Minute); r["state"] != "done" {
		t.Fatalf("run: %v %v", r["state"], r["error"])
	}
	total, columnar, full := dpLeaves(t, c, "xu", `^ev_third_b[0-9]{4}$`)
	if total != 6 || columnar != 6 || full != 6 {
		t.Errorf("leaves of the converted partition: %d, columnar %d, with REPLICA IDENTITY FULL %d; want 6, 6, 6", total, columnar, full)
	}
	var cnt []string
	for _, n := range c.Nodes {
		cnt = append(cnt, stScalar(t, n, "xu", `SELECT count(*) FROM ev`))
	}
	if strings.Join(cnt, ",") != "300,300,300" {
		t.Errorf("rows after the conversion: %v", cnt)
	}
}
