// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package clustertest_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/clustertest"
)

// Storage and time partitioning through the REST API (contract 0.17.0) on a cluster of three servers: a database that the control plane made has
// kingres_columnar and kingres_timepart on every server, a table and one partition are converted to columnar and back (the library is preloaded), the
// tables show how they are stored, a policy is set, planned, run by hand and by the scheduler of the leader, its runs and alerts are shown, and the
// roles see what the contract says.

func stScalar(t *testing.T, n *clustertest.Node, db, q string) string {
	t.Helper()
	var v *string
	if err := n.ServicePG(db).QueryRow(context.Background(), "SELECT ("+q+")::text").Scan(&v); err != nil {
		t.Fatalf("node %d: %s: %v", n.Index, q, err)
	}
	if v == nil {
		return ""
	}
	return *v
}

func stExec(t *testing.T, n *clustertest.Node, db string, qs ...string) {
	t.Helper()
	for _, q := range qs {
		if _, err := n.ServicePG(db).Exec(context.Background(), q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
}

func TestTableStorageAndPartitionPolicies(t *testing.T) {
	c := clustertest.FormWithExtra(t, 3, []string{"-partition-scheduler-interval", "3s"}, func(leader *clustertest.Node) {
		body := leader.Expect(202, "POST", "/databases", map[string]any{"name": "st"})
		leader.WaitTaskDone(clustertest.TaskID(body), 4*time.Minute)
	})
	leader := c.Leader()
	tbl := func(name string) string { return "/databases/st/schemas/public/tables/" + name }
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

	t.Run("a database that was made has the storage extensions and the preloaded library on every server", func(t *testing.T) {
		for _, n := range c.Nodes {
			if got := stScalar(t, n, "st", `SELECT string_agg(extname, ',' ORDER BY extname) FROM pg_extension WHERE extname IN ('kingres_columnar', 'kingres_timepart')`); got != "kingres_columnar,kingres_timepart" {
				t.Errorf("node %d: extensions %q", n.Index, got)
			}
			if got := stScalar(t, n, "postgres", `current_setting('shared_preload_libraries')`); !strings.Contains(got, "kingres_columnar") {
				t.Errorf("node %d: shared_preload_libraries %q", n.Index, got)
			}
		}
	})

	stExec(t, leader, "st",
		`CREATE TABLE facts (id int, v text)`,
		`INSERT INTO facts SELECT g, 'v' || g FROM generate_series(1, 4000) g`,
		`CREATE TABLE refs (id int PRIMARY KEY)`,
		`CREATE TABLE uses (id int, r int REFERENCES refs (id))`,
		`CREATE TABLE ev (ts timestamptz NOT NULL, n int) PARTITION BY RANGE (ts)`,
		fmt.Sprintf(`CREATE TABLE ev_a PARTITION OF ev FOR VALUES FROM ('%s 00:00:00+00') TO ('%s 00:00:00+00')`, day(-40), day(-35)),
		fmt.Sprintf(`CREATE TABLE ev_b PARTITION OF ev FOR VALUES FROM ('%s 00:00:00+00') TO ('%s 00:00:00+00')`, day(-35), day(-30)),
		fmt.Sprintf(`INSERT INTO ev SELECT '%s 00:00:00+00'::timestamptz + g * interval '1 minute', g FROM generate_series(1, 2000) g`, day(-39)))

	t.Run("a table converts to columnar and back, and says how it is stored", func(t *testing.T) {
		before := leader.Expect(200, "GET", tbl("facts"), nil)
		if st, _ := before["storage"].(map[string]any); st["accessMethod"] != "heap" {
			t.Fatalf("storage before: %v", before["storage"])
		}
		b := leader.Expect(202, "PUT", tbl("facts")+"/storage", map[string]any{"accessMethod": "columnar", "compression": "zstd", "compressionLevel": 3})
		got := taskDone(b)
		if sub, _ := got["subject"].(map[string]any); sub["type"] != "table" || sub["id"] != "st/public.facts" || got["kind"] != "table.setStorage" {
			t.Errorf("task %v %v", got["kind"], got["subject"])
		}
		after := leader.Expect(200, "GET", tbl("facts"), nil)
		st, _ := after["storage"].(map[string]any)
		col, _ := st["columnar"].(map[string]any)
		if st["accessMethod"] != "columnar" || col["rows"] != float64(4000) || col["compression"] != "zstd" || col["bytesOnDisk"].(float64) <= 0 {
			t.Fatalf("storage after: %v", after["storage"])
		}
		lst := leader.Expect(200, "GET", "/databases/st/tables?storage=columnar", nil)
		var names []string
		for _, it := range lst["items"].([]any) {
			names = append(names, it.(map[string]any)["name"].(string))
		}
		if strings.Join(names, ",") != "facts" {
			t.Errorf("storage=columnar lists %v", names)
		}
		if st, p := leader.Do("GET", "/databases/st/tables?storage=rocksdb", nil, nil); st != 400 || p["code"] != "validation_failed" {
			t.Errorf("an unknown storage filter: %d %v", st, p)
		}
		if n := stScalar(t, leader, "st", `SELECT count(*) || ' ' || sum(id) FROM facts`); n != "4000 8002000" {
			t.Errorf("rows %s", n)
		}
		taskDone(leader.Expect(202, "PUT", tbl("facts")+"/storage", map[string]any{"accessMethod": "heap"}))
		if g := leader.Expect(200, "GET", tbl("facts"), nil); g["storage"].(map[string]any)["accessMethod"] != "heap" {
			t.Errorf("storage after the way back: %v", g["storage"])
		}
	})

	t.Run("what cannot be converted is refused with its reasons, before a task exists", func(t *testing.T) {
		st, p := leader.Do("PUT", tbl("uses")+"/storage", map[string]any{"accessMethod": "columnar"}, nil)
		if st != 409 || p["code"] != "conversion_refused" || !strings.Contains(fmt.Sprint(p["detail"]), "foreign keys") || !strings.Contains(fmt.Sprint(p["blockers"]), "foreign_keys") {
			t.Errorf("a table with a foreign key: %d %v", st, p)
		}
		// the preview gives the same reasons as a typed list, and starts nothing
		if st, pv := leader.Do("PUT", tbl("uses")+"/storage?dryRun=true", map[string]any{"accessMethod": "columnar"}, nil); st != 200 || pv["willConvert"] != false || !strings.Contains(fmt.Sprint(pv["blockers"]), "foreign_keys") || pv["current"] != "heap" {
			t.Errorf("preview of a table with a foreign key: %d %v", st, pv)
		}
		if st, pv := leader.Do("PUT", tbl("facts")+"/storage?dryRun=true", map[string]any{"accessMethod": "columnar"}, nil); st != 200 || pv["willConvert"] != true {
			t.Errorf("preview of a table that can be converted: %d %v", st, pv)
		}
		if st, p := leader.Do("PUT", tbl("ev")+"/storage", map[string]any{"accessMethod": "columnar"}, nil); st != 409 || p["code"] != "conversion_refused" {
			t.Errorf("a partitioned table: %d %v", st, p)
		}
		if st, p := leader.Do("PUT", tbl("nope")+"/storage", map[string]any{"accessMethod": "columnar"}, nil); st != 404 {
			t.Errorf("an unknown table: %d %v", st, p)
		}
		if st, p := leader.Do("PUT", tbl("facts")+"/storage", map[string]any{"accessMethod": "rocksdb"}, nil); st != 400 || p["code"] != "validation_failed" {
			t.Errorf("an unknown format: %d %v", st, p)
		}
	})

	t.Run("a partition converts while its table stays one table", func(t *testing.T) {
		parts := leader.Expect(200, "GET", tbl("ev")+"/partitions", nil)
		items := parts["items"].([]any)
		if len(items) != 2 || items[0].(map[string]any)["name"] != "ev_a" || items[0].(map[string]any)["accessMethod"] != "heap" || items[0].(map[string]any)["rangeFrom"] == nil {
			t.Fatalf("partitions: %v", parts)
		}
		b := leader.Expect(202, "PUT", tbl("ev")+"/partitions/ev_a/storage", map[string]any{"accessMethod": "columnar", "sortBy": []string{"ts"}})
		got := taskDone(b)
		if sub, _ := got["subject"].(map[string]any); sub["id"] != "st/public.ev_a" {
			t.Errorf("subject %v", got["subject"])
		}
		parts = leader.Expect(200, "GET", tbl("ev")+"/partitions", nil)
		a := parts["items"].([]any)[0].(map[string]any)
		if a["accessMethod"] != "columnar" || a["compressionRatio"] == nil {
			t.Errorf("ev_a after: %v", a)
		}
		ev := leader.Expect(200, "GET", tbl("ev"), nil)
		pf := ev["storage"].(map[string]any)["partitionFormats"].(map[string]any)
		if pf["columnar"] != float64(1) || pf["heap"] != float64(1) || ev["partitioning"].(map[string]any)["columnType"] != "timestamptz" {
			t.Errorf("ev: %v %v", ev["storage"], ev["partitioning"])
		}
		if n := stScalar(t, leader, "st", `SELECT count(*) FROM ev`); n != "2000" {
			t.Errorf("rows %s", n)
		}
		if st, p := leader.Do("PUT", tbl("ev")+"/partitions/facts/storage", map[string]any{"accessMethod": "columnar"}, nil); st != 404 {
			t.Errorf("a table that is not a partition: %d %v", st, p)
		}
	})

	t.Run("a policy is set, planned, run by hand, and removed", func(t *testing.T) {
		if st, p := leader.Do("GET", tbl("ev")+"/partition-policy", nil, nil); st != 404 || p["code"] != "not_found" {
			t.Fatalf("no policy yet: %d %v", st, p)
		}
		pol := map[string]any{"partitionEvery": "P1D", "createAhead": "P2D", "retention": "P3650D", "retentionAction": "drop", "enabled": false} // disabled: only the run by hand acts
		for _, bad := range []map[string]any{{"partitionEvery": "daily"}, {"partitionEvery": "PT7H"}, {"partitionEvery": "P1D", "retentionAction": "archive"}} {
			if st, p := leader.Do("PUT", tbl("ev")+"/partition-policy", bad, nil); st != 400 || p["code"] != "validation_failed" || p["errors"] == nil {
				t.Errorf("%v: %d %v", bad, st, p)
			}
		}
		if st, p := leader.Do("PUT", tbl("facts")+"/partition-policy", pol, nil); st != 409 || p["code"] != "partition_policy_conflict" {
			t.Errorf("a table that is not partitioned: %d %v", st, p)
		}
		put := leader.Expect(200, "PUT", tbl("ev")+"/partition-policy", pol)
		if put["partitionEvery"] != "P1D" || put["partitionColumn"] != "ts" || put["partitionColumnType"] != "timestamptz" || put["nextRunAt"] == nil || put["lastRun"] != nil {
			t.Fatalf("put: %v", put)
		}
		if ev := leader.Expect(200, "GET", tbl("ev"), nil); ev["partitioning"].(map[string]any)["hasPolicy"] != true {
			t.Errorf("hasPolicy: %v", ev["partitioning"])
		}
		plan := leader.Expect(200, "POST", tbl("ev")+"/partition-policy/plan", map[string]any{})
		acts := plan["actions"].([]any)
		if len(acts) == 0 || acts[0].(map[string]any)["state"] != "planned" {
			t.Errorf("plan: %v", plan)
		}
		if n := stScalar(t, leader, "st", `SELECT count(*) FROM pg_inherits WHERE inhparent = 'ev'::regclass`); n != "2" {
			t.Errorf("a plan made %s partitions", n)
		}
		// by hand, for a time in the past (the scheduler may run it too: wait for every task of the table)
		// the time of a run is the time of the server: a run for 2100 is refused, and so is one for two days ago
		for _, at := range []string{"2100-01-01T00:00:00Z", time.Now().UTC().Add(-48 * time.Hour).Format(time.RFC3339)} {
			st, p := leader.Do("POST", tbl("ev")+"/partition-policy/runs", map[string]any{"asOf": at}, nil)
			errs, _ := p["errors"].([]any)
			if st != 400 || p["code"] != "validation_failed" || len(errs) != 1 || errs[0].(map[string]any)["field"] != "/asOf" {
				t.Errorf("asOf %s: %d %v", at, st, p)
			}
		}
		run := leader.Expect(202, "POST", tbl("ev")+"/partition-policy/runs", map[string]any{"asOf": time.Now().UTC().Format(time.RFC3339)})
		got := leader.WaitTask(clustertest.TaskID(run), 5*time.Minute)
		if got["kind"] != "partition.maintain" {
			t.Errorf("kind %v", got["kind"])
		}
		if got["state"] != "done" {
			t.Fatalf("run ended %v: %v", got["state"], got["error"])
		}
		runs := leader.Expect(200, "GET", tbl("ev")+"/partition-policy/runs?state=done", nil)
		var mine map[string]any
		for _, r := range runs["items"].([]any) {
			m := r.(map[string]any)
			if m["taskId"] == got["id"] {
				mine = m
			}
		}
		if mine == nil || mine["trigger"] != "manual" || mine["startedBy"] != clustertest.AdminUser {
			t.Fatalf("the run in the list: %v of %v", mine, runs["items"])
		}
		full := leader.Expect(200, "GET", fmt.Sprintf("%s/partition-policy/runs/%v", tbl("ev"), mine["id"]), nil)
		if len(full["actions"].([]any)) == 0 || full["actions"].([]any)[0].(map[string]any)["state"] != "done" {
			t.Errorf("actions: %v", full["actions"])
		}
		if n := stScalar(t, leader, "st", `SELECT count(*) FROM pg_inherits WHERE inhparent = 'ev'::regclass AND inhrelid::regclass::text LIKE 'ev\_p%'`); n == "0" {
			t.Error("the run made no partition")
		}
		if st, _ := leader.Do("GET", tbl("ev")+"/partition-policy/runs/999999", nil, nil); st != 404 {
			t.Errorf("an unknown run: %d", st)
		}
		if st, _ := leader.Do("DELETE", tbl("ev")+"/partition-policy", nil, nil); st != 204 {
			t.Errorf("delete: %d", st)
		}
		if st, _ := leader.Do("DELETE", tbl("ev")+"/partition-policy", nil, nil); st != 404 {
			t.Errorf("delete again: %d", st)
		}
		if st, p := leader.Do("POST", tbl("ev")+"/partition-policy/runs", nil, nil); st != 404 {
			t.Errorf("a run without a policy: %d %v", st, p)
		}
	})

	t.Run("the scheduler of the leader runs a policy that is due", func(t *testing.T) {
		// The first partition is yesterday's: the policy has to make today's and the next two, and the scheduler does it within its interval (3 s here).
		stExec(t, leader, "st",
			"CREATE TABLE sched_t (ts timestamptz NOT NULL) PARTITION BY RANGE (ts)",
			fmt.Sprintf("CREATE TABLE sched_t_first PARTITION OF sched_t FOR VALUES FROM ('%s 00:00:00+00') TO ('%s 00:00:00+00')", day(-1), day(0)))
		leader.Expect(200, "PUT", tbl("sched_t")+"/partition-policy", map[string]any{"partitionEvery": "P1D", "createAhead": "P2D", "retention": "P36500D"})
		var runs map[string]any
		for end := time.Now().Add(3 * time.Minute); time.Now().Before(end); time.Sleep(time.Second) {
			runs = leader.Expect(200, "GET", tbl("sched_t")+"/partition-policy/runs?state=done", nil)
			if len(runs["items"].([]any)) > 0 {
				break
			}
		}
		items := runs["items"].([]any)
		if len(items) == 0 {
			t.Fatalf("the scheduler started no run %s", leader.LogTail(3000))
		}
		r := items[len(items)-1].(map[string]any)
		if r["trigger"] != "schedule" || r["startedBy"] != "system" || r["taskId"] == nil {
			t.Errorf("run %v", r)
		}
		tasks := leader.Expect(200, "GET", "/tasks?kind=partition.maintain", nil)
		var system int
		for _, it := range tasks["items"].([]any) {
			if it.(map[string]any)["createdBy"] == "system" {
				system++
			}
		}
		if system == 0 {
			t.Error("no task made by the system")
		}
		if n := stScalar(t, leader, "st", "SELECT count(*) FROM pg_inherits WHERE inhparent = 'sched_t'::regclass"); n != "4" {
			t.Errorf("%s partitions, want yesterday's and three more", n)
		}
		// it is not due again before run_every (an hour): no second run within a few intervals
		time.Sleep(10 * time.Second)
		again := leader.Expect(200, "GET", tbl("sched_t")+"/partition-policy/runs", nil)
		if n := len(again["items"].([]any)); n != len(items) {
			t.Errorf("%d runs, were %d: a run that is not due was started", n, len(items))
		}
	})

	t.Run("an alert shows a policy that is behind, and goes when the policy has caught up", func(t *testing.T) {
		// The policy wants 30 days ahead and the table has yesterday's partition only; the policy is disabled, so nothing fixes it yet.
		stExec(t, leader, "st",
			"CREATE TABLE alert_t (ts timestamptz NOT NULL) PARTITION BY RANGE (ts)",
			fmt.Sprintf("CREATE TABLE alert_t_first PARTITION OF alert_t FOR VALUES FROM ('%s 00:00:00+00') TO ('%s 00:00:00+00')", day(-1), day(1)))
		leader.Expect(200, "PUT", tbl("alert_t")+"/partition-policy", map[string]any{"partitionEvery": "P1D", "createAhead": "P30D", "enabled": false})
		find := func(kind string) bool {
			al := leader.Expect(200, "GET", "/alerts?limit=200", nil)
			for _, it := range al["items"].([]any) {
				a := it.(map[string]any)
				sub, _ := a["subject"].(map[string]any)
				if a["kind"] == kind && sub["databaseName"] == "st" && sub["tableSchema"] == "public" && sub["tableName"] == "alert_t" && sub["aspect"] == nil && a["state"] != "resolved" {
					return true
				}
			}
			return false
		}
		raised := false
		for end := time.Now().Add(2 * time.Minute); time.Now().Before(end) && !raised; time.Sleep(2 * time.Second) {
			raised = find("partitionsAheadLow")
		}
		if !raised {
			t.Fatalf("no partitionsAheadLow alert %s", leader.LogTail(3000))
		}
		pol := leader.Expect(200, "GET", tbl("alert_t")+"/partition-policy", nil)
		h, _ := pol["health"].(map[string]any)
		if h["ok"] != false || !strings.Contains(fmt.Sprint(h["problems"]), "aheadLow") {
			t.Errorf("health %v", h)
		}
		// enable it: the scheduler catches up and the alert goes
		leader.Expect(200, "PUT", tbl("alert_t")+"/partition-policy", map[string]any{"partitionEvery": "P1D", "createAhead": "P30D", "enabled": true})
		gone := false
		for end := time.Now().Add(3 * time.Minute); time.Now().Before(end) && !gone; time.Sleep(2 * time.Second) {
			gone = !find("partitionsAheadLow")
		}
		if !gone {
			t.Errorf("the alert is still open after the policy caught up")
		}
	})

	t.Run("the roles see what the contract says", func(t *testing.T) {
		tokens := map[string]string{}
		for name, role := range map[string]string{"stview": "kingres_readonly", "stops": "kingres_ops", "ststats": "kingres_stats"} {
			mkRole(t, leader, name, markerPassword, role)
			st, o := leader.Do("POST", "/auth/login", map[string]any{"username": name, "password": markerPassword, "issueToken": true}, map[string]string{"Authorization": ""})
			if st != 200 {
				t.Fatalf("login %s: %d %v", name, st, o)
			}
			tokens[name] = o["token"].(string)
		}
		as := func(name, method, path string, body any) int {
			st, _ := leader.Do(method, path, body, map[string]string{"Authorization": "Bearer " + tokens[name]})
			return st
		}
		writes := []struct{ method, path string }{
			{"PUT", tbl("facts") + "/storage"}, {"PUT", tbl("ev") + "/partitions/ev_a/storage"}, {"PUT", tbl("ev") + "/partition-policy"},
			{"DELETE", tbl("ev") + "/partition-policy"}, {"POST", tbl("ev") + "/partition-policy/runs"},
		}
		for _, name := range []string{"stview", "stops"} {
			for _, w := range writes {
				if st := as(name, w.method, w.path, map[string]any{"accessMethod": "heap", "partitionEvery": "P1D"}); st != 403 {
					t.Errorf("%s %s %s: %d, want 403", name, w.method, w.path, st)
				}
			}
		}
		for _, p := range []string{tbl("ev") + "/partitions", tbl("ev") + "/partition-policy/runs", "/databases/st/tables?storage=heap"} {
			if st := as("stview", "GET", p, nil); st != 200 {
				t.Errorf("a read-only role reads %s: %d", p, st)
			}
			if st := as("ststats", "GET", p, nil); st != 403 {
				t.Errorf("a stats role reads %s: %d, want 403", p, st)
			}
		}
		// a preview is a read: a read-only role may ask (no policy of ev now: 404, not 403)
		if st := as("stview", "POST", tbl("sched_t")+"/partition-policy/plan", map[string]any{}); st != 200 {
			t.Errorf("plan as a read-only role: %d", st)
		}
		if st := as("ststats", "POST", tbl("sched_t")+"/partition-policy/plan", map[string]any{}); st != 403 {
			t.Errorf("plan as a stats role: %d", st)
		}
		audit := leader.Expect(200, "GET", "/audit?action=setTableStorage", nil)
		if items, _ := audit["items"].([]any); len(items) < 3 {
			t.Errorf("the audit log has %d setTableStorage events", len(items))
		}
	})
}
