// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026 RooyeKhat Media Co

package clustertest_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/archive"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/clustertest"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/sqlshape"
)

// A database export (phase 14b) and the drain of a server (phase 10, which also moves distributed schemas since contract 0.31.0) at the same time, on three
// servers with real instances. The export reads through the row router while the drain moves buckets and schemas; the drain's orphan cleaning waits for the
// readers of the old partitions. Both must end, the archive must hold every row once, the drained server must hold nothing, no move may stay in the cleaning phase
// and check_cluster must report nothing. A refusal of the drain while the export runs is allowed only as the clear problem operation_in_progress; the drain is
// then started again when the export has ended.

// edKeys is the count of rows, the count of distinct keys and the sum of the keys of a table, read through the router.
func edKeys(t *testing.T, n *clustertest.Node, table string) string {
	t.Helper()
	conn := n.ServicePG("sk")
	defer func() { _ = conn.Close(context.Background()) }()
	var rows, distinct, sum int64
	if err := conn.QueryRow(context.Background(), `SELECT count(*), count(DISTINCT id), coalesce(sum(id), 0) FROM `+table).Scan(&rows, &distinct, &sum); err != nil {
		t.Fatalf("keys of %s: %v", table, err)
	}
	return fmt.Sprintf("rows=%d distinct=%d sum=%d", rows, distinct, sum)
}

func TestDatabaseExportAndDrainAtTheSameTime(t *testing.T) {
	const itemRows = 300000 // enough that the export is still writing data when the drain is started
	const schemaRows = 20000
	c := clustertest.FormTLS(t, 3, []string{"-rebalance-retry-backoff", "1s"}, func(leader *clustertest.Node) {
		body := leader.Expect(202, "POST", "/databases", map[string]any{"name": "sk", "sharding": map[string]any{"enabled": true, "mode": "both", "bucketCount": 8}})
		leader.WaitTaskDone(clustertest.TaskID(body), 4*time.Minute)
		sc := leader.ServicePG("sk")
		if _, err := sc.Exec(context.Background(), `CREATE TABLE items (id bigint PRIMARY KEY, val text)`); err != nil {
			t.Fatal(err)
		}
		body = leader.Expect(202, "PUT", "/databases/sk/schemas/public/tables/items/distribution", map[string]any{"method": "hash", "distributionColumn": "id"})
		leader.WaitTaskDone(clustertest.TaskID(body), 2*time.Minute)
		if _, err := sc.Exec(context.Background(), `INSERT INTO items SELECT g, repeat('x', 150) FROM generate_series(1, $1::int) g`, itemRows); err != nil {
			t.Fatal(err)
		}
	})
	l := c.Leader()
	for _, n := range c.Nodes {
		n.Login(clustertest.AdminUser, clustertest.AdminPass)
	}
	l.Expect(200, "PATCH", "/settings/cluster", map[string]any{"defaultReplicaCount": 0, "dataTransfer": map[string]any{"enabled": true, "maxConcurrent": 4}, "analytics": map[string]any{"allowNonSuperuserOwners": true}})
	until(t, 3*time.Minute, "no standby task to run", func() bool {
		for _, st := range []string{"pending", "running"} {
			if len(anyList(l.Expect(200, "GET", "/tasks?kind=standby.reconcile&state="+st, nil)["items"])) > 0 {
				return false
			}
		}
		return true
	})
	p10Cleanup(t, c, "3s")
	victim := c.Nodes[2]

	rep := waitReport(t, l, clustertest.TaskID(l.Expect(202, "POST", "/rebalance", nil)), 10*time.Minute)
	if rep["outcome"] != "completed" {
		t.Fatalf("the first rebalance: %v", rep)
	}
	until(t, 2*time.Minute, "the first rebalance to have ended", func() bool { return rbStatus(l)["state"] == "idle" })
	if primaryBuckets(t, l)[victim.Host] == 0 {
		t.Fatalf("the victim owns no bucket: %v", primaryBuckets(t, l))
	}
	var victimGroup int
	if err := l.ServicePG("sk").QueryRow(context.Background(), `SELECT group_id FROM kingres.node WHERE host = $1 AND role = 'primary' AND is_active`, victim.Host).Scan(&victimGroup); err != nil {
		t.Fatal(err)
	}
	schemas := []string{"dr_a", "dr_b"}
	for _, s := range schemas {
		skMake(t, c, s, schemaRows)
		l.WaitTaskDone(clustertest.TaskID(l.Expect(202, "PUT", "/databases/sk/schemas/"+s+"/distribution", map[string]any{"shardGroupId": victimGroup})), 3*time.Minute)
		if host, _ := sdOwner(t, l, s); host != victim.Host {
			t.Fatalf("schema %s is on %s, want %s", s, host, victim.Host)
		}
	}
	tables := []string{"public.items", "dr_a.t", "dr_b.t"}
	want := map[string]int64{"public.items": itemRows, "dr_a.t": schemaRows, "dr_b.t": schemaRows}
	before := map[string]string{}
	for _, tb := range tables {
		before[tb] = edKeys(t, l, tb)
		if exp := fmt.Sprintf("rows=%d distinct=%d", want[tb], want[tb]); !strings.HasPrefix(before[tb], exp) {
			t.Fatalf("%s before: %s, want %s", tb, before[tb], exp)
		}
	}

	// The export, through the leader; the drain is started when its data step runs.
	exID := clustertest.TaskID(l.Expect(202, "POST", "/databases/sk/export", map[string]any{}))
	var exBody map[string]any
	until(t, 10*time.Minute, "the export to write its data", func() bool {
		st, b := l.Do("GET", fmt.Sprintf("/tasks/%v", exID), nil, nil)
		if st != 200 {
			return false
		}
		exBody = b
		if b["state"] == "failed" {
			t.Fatalf("the export failed before the drain: %v", b["error"])
		}
		return b["state"] == "done" || taskSteps(b)["Write the data"] == "running"
	})
	if exBody["state"] == "done" {
		t.Fatalf("the export ended before the drain could start; raise itemRows")
	}

	var drainID any
	st, dr := l.Do("POST", "/servers/"+victim.Host+"/drain", map[string]any{}, nil)
	refused := false
	switch st {
	case 202:
		drainID = clustertest.TaskID(dr)
		if _, b := l.Do("GET", fmt.Sprintf("/tasks/%v", exID), nil, nil); b["state"] == "done" {
			t.Logf("the export ended before the drain was accepted; the overlap was short")
		}
	case 409:
		// Refused by design while the export runs: it must be a clear problem and nothing else.
		if dr["code"] != "operation_in_progress" || dr["detail"] == nil || fmt.Sprint(dr["detail"]) == "" {
			t.Fatalf("the drain was refused with something that is not a clear problem: %d %v", st, dr)
		}
		refused = true
		t.Logf("the drain was refused while the export runs: %v", dr["detail"])
	default:
		t.Fatalf("the drain: %d %v", st, dr)
	}

	// The export finishes and its archive verifies.
	res := l.WaitTask(exID, 20*time.Minute)
	if res["state"] != "done" || !strings.HasPrefix(fmt.Sprint(res["detail"]), "transfer ") {
		t.Fatalf("the export: state %v, error %v\n%s", res["state"], res["error"], l.LogTail(3000))
	}
	if refused {
		drainID = clustertest.TaskID(l.Expect(202, "POST", "/servers/"+victim.Host+"/drain", map[string]any{}))
	}
	xid := strings.TrimPrefix(fmt.Sprint(res["detail"]), "transfer ")
	st, _, body, err := l.DoBody("GET", "/data-exports/"+xid+"/content", nil, nil)
	if err != nil || st != 200 {
		t.Fatalf("download of the archive: %d %v", st, err)
	}
	r, err := archive.Validate(bytes.NewReader(body), int64(len(body)), archive.Limits{MaxImportBytes: 1 << 33, SQLFor: func(cat *archive.Catalog) archive.SQLChecker {
		return func(name string, rd io.Reader) (archive.SQLResult, error) {
			plan, err := sqlshape.ParseFile(rd, name, sqlshape.Hints{})
			if err != nil {
				return archive.SQLResult{}, err
			}
			o := archive.SQLResult{ByKind: plan.ByKind}
			for _, rf := range plan.Refused {
				o.Refused = append(o.Refused, archive.RefusedStatement{Kind: rf.Kind, Position: rf.Position, Fingerprint: rf.Fingerprint, Reason: rf.Reason})
			}
			return o, nil
		}
	}})
	if err != nil {
		t.Fatalf("the archive is not valid: %v", err)
	}
	for n, sr := range r.SQL {
		if len(sr.Refused) > 0 {
			t.Errorf("%s: refused %v", n, sr.Refused)
		}
	}
	got := map[string]int64{}
	for _, e := range r.Manifest.Entries {
		if e.Table != "" && !e.LedgerHistory {
			got[e.Table] = e.Rows
		}
	}
	for tb, w := range want {
		if got[tb] != w {
			t.Errorf("the archive holds %d rows of %s, want %d (entries %v)", got[tb], tb, w, got)
		}
	}

	// The drain finishes with the server holding nothing, and nothing is lost or doubled.
	l.WaitTaskDone(drainID, 30*time.Minute)
	for _, s := range schemas {
		host, g := sdOwner(t, l, s)
		if host == victim.Host || host == "" || g == float64(victimGroup) {
			t.Errorf("schema %s is still on the drained server: %s group %v", s, host, g)
		}
	}
	for _, tb := range tables {
		if after := edKeys(t, l, tb); after != before[tb] {
			t.Errorf("%s after: %s, before: %s", tb, after, before[tb])
		}
	}
	if n := skInt(t, l, "sk", `SELECT count(*) FROM kingres.bucket_map m JOIN kingres.node n ON n.group_id = m.group_id AND n.role = 'primary' AND n.is_active AND n.host = $1`, victim.Host); n != 0 {
		t.Errorf("the drained server owns %d buckets or schemas", n)
	}
	if rp := l.Expect(200, "GET", fmt.Sprintf("/rebalance/reports/%v", drainID), nil); rp["trigger"] != "drain" || rp["outcome"] != "completed" {
		t.Errorf("report of the drain: %v", rp)
	}

	// No move stays in the phase cleaning, no move is unfinished, no task is running, and check_cluster has nothing to report.
	until(t, 10*time.Minute, "no move to be unfinished or in the phase cleaning", func() bool {
		return skInt(t, l, "sk", `SELECT count(*) FROM kingres.bucket_move WHERE phase NOT IN ('done', 'failed', 'cancelled')`) == 0
	})
	var cc []string
	until(t, 5*time.Minute, "check_cluster to report nothing", func() bool {
		conn := l.ServicePG("sk")
		defer func() { _ = conn.Close(context.Background()) }()
		rows, err := conn.Query(context.Background(), `SELECT check_name || ': ' || coalesce(object, '') || ' ' || coalesce(detail, '') FROM kingres.check_cluster()`)
		if err != nil {
			return false
		}
		defer rows.Close()
		cc = nil
		for rows.Next() {
			var s string
			_ = rows.Scan(&s)
			cc = append(cc, s)
		}
		return rows.Err() == nil && len(cc) == 0
	})
	for _, k := range []string{"rebalance", "database.export"} {
		for _, s := range []string{"pending", "running", "paused"} {
			if items := anyList(l.Expect(200, "GET", "/tasks?kind="+k+"&state="+s, nil)["items"]); len(items) > 0 {
				t.Errorf("tasks of kind %s in state %s are left: %v", k, s, items)
			}
		}
	}
}
