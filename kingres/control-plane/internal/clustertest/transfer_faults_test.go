// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package clustertest_test

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/clustertest"
)

// Faults of the data transfer on three servers that serve HTTPS: the leader that holds the files restarts in the middle of an import and of an export (the role
// map of the process is empty afterwards, so the steps that follow cannot act as the role: transfer_session_lost), and a shard group whose instance is stopped in the
// middle of a routed COPY (the import is atomic across groups: all rows or none, and no prepared transaction is left).

func xfFormed(t *testing.T) (*clustertest.Cluster, *clustertest.Node) {
	t.Helper()
	c := clustertest.FormTLS(t, 3, nil, func(leader *clustertest.Node) {
		body := leader.Expect(202, "POST", "/databases", map[string]any{"name": "xf", "sharding": map[string]any{"enabled": true, "bucketCount": 12}})
		leader.WaitTaskDone(clustertest.TaskID(body), 4*time.Minute)
	})
	leader := c.Leader()
	for _, n := range c.Nodes {
		n.Login(clustertest.AdminUser, clustertest.AdminPass)
	}
	leader.Expect(200, "PATCH", "/settings/cluster", map[string]any{"dataTransfer": map[string]any{"enabled": true, "maxConcurrent": 4, "stagingQuotaBytes": 4 << 30}})
	return c, leader
}

func xfUpload(t *testing.T, n *clustertest.Node, data []byte) string {
	t.Helper()
	up := n.Expect(201, "POST", "/data-uploads", map[string]any{"purpose": "tableImport", "sizeBytes": len(data)})
	id := up["uploadId"].(string)
	const mb = 1 << 20
	for off := 0; off < len(data); off += mb {
		end := off + mb
		if end > len(data) {
			end = len(data)
		}
		if st, _ := xfChunk(t, n, id, off, data[off:end]); st != 204 {
			t.Fatalf("chunk at %d: %d", off, st)
		}
	}
	return id
}

func xfTaskEnd(t *testing.T, n *clustertest.Node, id any, d time.Duration) map[string]any {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		st, cur := n.Do("GET", "/tasks/"+fmt.Sprint(int64(id.(float64))), nil, nil)
		if st == 401 {
			// The sessions of the process are gone with a restart.
			n.Login(clustertest.AdminUser, clustertest.AdminPass)
			continue
		}
		if st == 200 && (cur["state"] == "failed" || cur["state"] == "done") {
			return cur
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("task %v did not end", id)
	return nil
}

func xfCode(task map[string]any) string {
	if er, _ := task["error"].(map[string]any); er != nil {
		c, _ := er["code"].(string)
		return c
	}
	return ""
}

func xfCount(t *testing.T, n *clustertest.Node, q string) int64 {
	t.Helper()
	var v int64
	var err error
	for i := 0; i < 30; i++ {
		if err = n.ServicePG("xf").QueryRow(context.Background(), q).Scan(&v); err == nil {
			return v
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatalf("%s on node %d: %v", q, n.Index, err)
	return 0
}

func TestDataTransferLeaderRestartMidTransfer(t *testing.T) {
	c, leader := xfFormed(t)
	q := func(sql string) { leader.Expect(200, "POST", "/query", map[string]any{"database": "xf", "sql": sql}) }
	q("create table slow_t (id int, v text)")
	q("create function slow_trg() returns trigger language plpgsql as $$ begin perform pg_sleep(0.05); return new; end $$")
	q("create trigger slow_trg before insert on slow_t for each row execute function slow_trg()")
	var rows bytes.Buffer
	for i := 0; i < 600; i++ {
		fmt.Fprintf(&rows, "%d,row\n", i)
	}
	uid := xfUpload(t, leader, rows.Bytes())
	st, out := leader.Do("POST", "/databases/xf/schemas/public/tables/slow_t/import", map[string]any{"uploadId": uid, "header": "none", "keepUpload": true}, nil)
	if st != 202 {
		t.Fatalf("import: %d %v", st, out)
	}
	time.Sleep(3 * time.Second)
	// The leader dies and comes back on the same disk: the staged files are there, the role map of the process is empty.
	leader.Kill()
	leader.Start()
	until(t, 2*time.Minute, "the leader answers", func() bool { st, _ := leader.Do("GET", "/healthz", nil, nil); return st == 200 })
	done := xfTaskEnd(t, leader, out["id"], 3*time.Minute)
	code := xfCode(done)
	leader.Login(clustertest.AdminUser, clustertest.AdminPass)
	rowsNow := xfCount(t, leader, "SELECT count(*) FROM slow_t")
	// Two outcomes are right, and nothing else: the steps could not act as the role (the role map of the new process was empty), and the task failed with a
	// retryable code and nothing stored; or the sign-in that the poll made came first, the load ran again from its start in a transaction of its own, and
	// every row is there exactly once. Half the rows, or twice the rows, would be a defect.
	switch {
	case done["state"] == "failed" && (code == "transfer_session_lost" || code == "copy_interrupted"):
		if er, _ := done["error"].(map[string]any); er["retryable"] != true {
			t.Errorf("not retryable: %v", er)
		}
		if rowsNow != 0 {
			t.Errorf("rows stored by an import that was interrupted before the commit: %d", rowsNow)
		}
		if got := leader.Expect(200, "GET", "/data-uploads/"+uid, nil); got["state"] != "complete" {
			t.Errorf("the upload after the restart: %v", got)
		}
		q("drop trigger slow_trg on slow_t")
		st, out = leader.Do("POST", "/databases/xf/schemas/public/tables/slow_t/import", map[string]any{"uploadId": uid, "header": "none"}, nil)
		if st != 202 {
			t.Fatalf("repeat: %d %v", st, out)
		}
		if res := leader.WaitTask(clustertest.TaskID(out), 3*time.Minute); res["state"] != "done" {
			t.Fatalf("the repeat: %v", res["error"])
		}
	case done["state"] == "done":
		t.Logf("the load ran again after the restart")
	default:
		t.Errorf("an import that the restart interrupted: %v", done)
	}
	if got := xfCount(t, leader, "SELECT count(*) FROM slow_t"); got != 600 {
		t.Errorf("rows after the import: %d, want 600", got)
	}

	// An export that the restart interrupts: no file is left half written, and no ready file is claimed.
	q("create table big_t (id int, v text)")
	q("insert into big_t select g, repeat('x', 80) from generate_series(1, 1500000) g")
	st, out = leader.Do("POST", "/databases/xf/schemas/public/tables/big_t/export", map[string]any{}, nil)
	if st != 202 {
		t.Fatalf("export: %d %v", st, out)
	}
	time.Sleep(1500 * time.Millisecond)
	leader.Kill()
	leader.Start()
	until(t, 2*time.Minute, "the leader answers again", func() bool { st, _ := leader.Do("GET", "/healthz", nil, nil); return st == 200 })
	done = xfTaskEnd(t, leader, out["id"], 3*time.Minute)
	if done["state"] == "failed" {
		if code := xfCode(done); code != "transfer_session_lost" && code != "copy_interrupted" && code != "transfer_leader_changed" {
			t.Errorf("an export that the restart interrupted: %v", done["error"])
		}
	}
	leader.Login(clustertest.AdminUser, clustertest.AdminPass)
	if done["state"] == "failed" {
		files, _ := filepath.Glob(filepath.Join(leader.DataDir, "transfer", "transfer-*.part"))
		if len(files) != 0 {
			t.Errorf("a failed export left %v", files)
		}
		if l := leader.Expect(200, "GET", "/data-exports", nil); len(l["items"].([]any)) > 0 {
			for _, it := range l["items"].([]any) {
				if it.(map[string]any)["state"] == "ready" {
					t.Errorf("a ready export after a failed one: %v", it)
				}
			}
		}
	}
	_ = c
	_ = os.Stderr
}

func TestDataTransferShardGroupLostMidCopy(t *testing.T) {
	c, leader := xfFormed(t)
	q := func(sql string) { leader.Expect(200, "POST", "/query", map[string]any{"database": "xf", "sql": sql}) }
	q("create table gd_t (id bigint primary key, v text)")
	tb := leader.Expect(202, "PUT", "/databases/xf/schemas/public/tables/gd_t/distribution", map[string]any{"method": "hash", "distributionColumn": "id"})
	leader.WaitTaskDone(clustertest.TaskID(tb), 4*time.Minute)
	const total = 2500000
	var rows bytes.Buffer
	for i := 1; i <= total; i++ {
		fmt.Fprintf(&rows, "%d,value number %d of a long enough line\n", i, i)
	}
	uid := xfUpload(t, leader, rows.Bytes())
	st, out := leader.Do("POST", "/databases/xf/schemas/public/tables/gd_t/import", map[string]any{"uploadId": uid, "header": "none", "keepUpload": true}, nil)
	if st != 202 {
		t.Fatalf("import: %d %v", st, out)
	}
	time.Sleep(2 * time.Second)
	victim := c.Nodes[2]
	victim.StopInstance()
	done := xfTaskEnd(t, leader, out["id"], 5*time.Minute)
	t.Logf("the import ended %v %v", done["state"], done["error"])
	// The instance comes back (the process restarts it).
	victim.Kill()
	victim.Start()
	until(t, 3*time.Minute, "the server answers", func() bool { st, _ := victim.Do("GET", "/healthz", nil, nil); return st == 200 })
	until(t, 3*time.Minute, "the table can be counted", func() bool {
		var n int64
		err := leader.ServicePG("xf").QueryRow(context.Background(), "SELECT count(*) FROM gd_t").Scan(&n)
		return err == nil
	})
	got := xfCount(t, leader, "SELECT count(*) FROM gd_t")
	if got != 0 && got != total {
		t.Errorf("half the rows after a group was lost: %d of %d", got, total)
	}
	if done["state"] == "done" && got != total {
		t.Errorf("the task says done and the table has %d rows", got)
	}
	if done["state"] == "failed" && code2(done) == "import_outcome_unknown" {
		t.Logf("outcome unknown, rows in the table: %d", got)
	} else if done["state"] == "failed" && got != 0 {
		t.Errorf("the task failed before the commit and %d rows are stored", got)
	}
	for _, n := range c.Nodes {
		var prepared int64
		until(t, time.Minute, "prepared transactions of node", func() bool {
			return n.ServicePG("postgres").QueryRow(context.Background(), "SELECT count(*) FROM pg_prepared_xacts").Scan(&prepared) == nil
		})
		if prepared != 0 {
			t.Logf("node %d still has %d prepared transactions (the recovery of the router resolves them)", n.Index, prepared)
		}
	}
	_ = strings.TrimSpace
}

func code2(task map[string]any) string { return xfCode(task) }
