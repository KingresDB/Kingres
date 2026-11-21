// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026 RooyeKhat Media Co

package clustertest_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/clustertest"
)

// Phase 10 on a real cluster (extension 0.8): the split of a bucket, the drain of a server and the automatic rebalance.

// p10Fault sets the extension's fault point for new sessions of database db on the leader's instance.
func p10Fault(t *testing.T, c *clustertest.Cluster, db, spec string) {
	t.Helper()
	q := `ALTER DATABASE ` + db + ` RESET kingres_shard.fault_injection`
	if spec != "" {
		q = `ALTER DATABASE ` + db + ` SET kingres_shard.fault_injection = '` + spec + `'`
	}
	if _, err := c.Leader().ServicePG("postgres").Exec(context.Background(), q); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

// p10Gate holds the next session that reaches the fault point until release.
func p10Gate(t *testing.T, c *clustertest.Cluster, db, point string) (reached func(time.Duration), release func()) {
	t.Helper()
	gate := filepath.Join(t.TempDir(), "gate")
	if err := os.WriteFile(gate, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	p10Fault(t, c, db, point+":wait:"+gate)
	reached = func(d time.Duration) {
		t.Helper()
		for end := time.Now().Add(d); ; time.Sleep(200 * time.Millisecond) {
			if _, err := os.Stat(gate + ".reached"); err == nil {
				return
			}
			if time.Now().After(end) {
				t.Fatalf("the fault point %s was never reached", point)
			}
		}
	}
	release = func() { p10Fault(t, c, db, ""); _ = os.Remove(gate) }
	return reached, release
}

// p10Cleanup shortens the delay before the old copy of a moved bucket is dropped, on every instance.
func p10Cleanup(t *testing.T, c *clustertest.Cluster, delay string) {
	t.Helper()
	for _, n := range c.Nodes {
		pg := n.ServicePG("postgres")
		for _, q := range []string{`ALTER SYSTEM SET kingres_shard.move_cleanup_delay = '` + delay + `'`, `SELECT pg_reload_conf()`} {
			if _, err := pg.Exec(context.Background(), q); err != nil {
				t.Fatalf("node %d: %s: %v", n.Index, q, err)
			}
		}
	}
}

func p10Count(t *testing.T, n *clustertest.Node, db string) (int, int64) {
	t.Helper()
	var cnt int
	var sum int64
	var err error
	for i := 0; i < 30; i++ { // KS001 can be answered while a switch commits; the statement is repeated
		err = n.ServicePG(db).QueryRow(context.Background(), `SELECT count(*), coalesce(sum(id), 0)::bigint FROM items`).Scan(&cnt, &sum)
		if err == nil {
			return cnt, sum
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatalf("node %d: counting the rows of %s: %v", n.Index, db, err)
	return 0, 0
}

// p10Writer inserts rows with ids from base on node n until stop is closed, and records every id that was acknowledged.
type p10Writer struct {
	once  sync.Once
	wg    sync.WaitGroup
	stop  chan struct{}
	mu    sync.Mutex
	ids   []int64
	fails atomic.Int64
}

func p10Write(t *testing.T, n *clustertest.Node, db string, base int64) *p10Writer {
	w := &p10Writer{stop: make(chan struct{})}
	// A test that fails before it stops the writer must not leave it running: it shares the connection of the node with the next steps.
	t.Cleanup(func() { w.halt() })
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		conn := n.ServicePG(db)
		for i := int64(1); ; i++ {
			select {
			case <-w.stop:
				return
			default:
			}
			id := base + i
			ok := false
			for try := 0; try < 60 && !ok; try++ {
				_, err := conn.Exec(context.Background(), `INSERT INTO items VALUES ($1, 'live')`, id)
				ok = err == nil || strings.Contains(err.Error(), "duplicate key")
				if !ok {
					time.Sleep(100 * time.Millisecond)
				}
			}
			if !ok {
				w.fails.Add(1)
				return
			}
			w.mu.Lock()
			w.ids = append(w.ids, id)
			w.mu.Unlock()
			time.Sleep(15 * time.Millisecond)
		}
	}()
	return w
}

// finish stops the writer and returns how many rows it wrote and the sum of their ids.
func (w *p10Writer) halt() {
	w.once.Do(func() { close(w.stop) })
	w.wg.Wait()
}

func (w *p10Writer) finish(t *testing.T) (int, int64) {
	t.Helper()
	w.halt()
	if w.fails.Load() != 0 {
		t.Errorf("a write never succeeded")
	}
	var sum int64
	for _, id := range w.ids {
		sum += id
	}
	return len(w.ids), sum
}

// p10Cluster forms three servers with database db (buckets buckets) whose table items holds rows 1..rows on the first server.
func p10Cluster(t *testing.T, db string, buckets, rows int, extra []string) *clustertest.Cluster {
	t.Helper()
	return clustertest.FormWithExtra(t, 3, extra, func(leader *clustertest.Node) {
		body := leader.Expect(202, "POST", "/databases", map[string]any{"name": db, "sharding": map[string]any{"enabled": true, "bucketCount": buckets}})
		leader.WaitTaskDone(clustertest.TaskID(body), 4*time.Minute)
		sc := leader.ServicePG(db)
		if _, err := sc.Exec(context.Background(), `CREATE TABLE items (id bigint PRIMARY KEY, val text)`); err != nil {
			t.Fatal(err)
		}
		body = leader.Expect(202, "PUT", "/databases/"+db+"/schemas/public/tables/items/distribution", map[string]any{"method": "hash", "distributionColumn": "id"})
		leader.WaitTaskDone(clustertest.TaskID(body), 2*time.Minute)
		if _, err := sc.Exec(context.Background(), `INSERT INTO items SELECT g, 'row' FROM generate_series(1, $1::int) g`, rows); err != nil {
			t.Fatal(err)
		}
	})
}

func p10Buckets(t *testing.T, l *clustertest.Node, db string) map[int]map[string]any {
	t.Helper()
	body := l.Expect(200, "GET", "/databases/"+db+"/colocation-groups/1/buckets?limit=500", nil)
	out := map[int]map[string]any{}
	for _, b := range anyList(body["items"]) {
		m := b.(map[string]any)
		out[int(m["bucket"].(float64))] = m
	}
	return out
}

func TestSplitBucketOnACluster(t *testing.T) {
	const rows = 400
	c := p10Cluster(t, "sp", 8, rows, nil)
	l := c.Leader()
	wantSum := int64(rows) * int64(rows+1) / 2
	split := func(bucket int, body map[string]any) map[string]any {
		return l.Expect(202, "POST", fmt.Sprintf("/databases/sp/colocation-groups/1/buckets/%d/split", bucket), body)
	}

	t.Run("the preview names the new buckets and the unknown bucket is 404", func(t *testing.T) {
		pv := l.Expect(200, "POST", "/databases/sp/colocation-groups/1/buckets/3/split-preview", map[string]any{})
		kids := anyList(pv["children"])
		if pv["canSplit"] != true || len(kids) != 2 || kids[0].(map[string]any)["bucket"] != float64(3) || kids[1].(map[string]any)["bucket"] != float64(11) ||
			kids[1].(map[string]any)["modulus"] != float64(16) {
			t.Fatalf("preview: %v", pv)
		}
		if st, p := l.Do("POST", "/databases/sp/colocation-groups/1/buckets/99/split-preview", map[string]any{}, nil); st != 404 {
			t.Errorf("unknown bucket: %d %v", st, p)
		}
		if st, _ := l.Do("POST", "/databases/sp/colocation-groups/1/buckets/3/split-preview", map[string]any{"parts": 3}, nil); st != 400 && st != 422 {
			t.Errorf("parts 3: %d", st)
		}
	})

	var extra int
	var extraSum int64
	t.Run("a split with writers running loses no row and shows the children", func(t *testing.T) {
		w := p10Write(t, c.Nodes[1], "sp", 1_000_000)
		task := l.WaitTaskDone(clustertest.TaskID(split(3, map[string]any{"parts": 2})), 5*time.Minute)
		if task["kind"] != "bucket.split" {
			t.Errorf("task: %v", task)
		}
		extra, extraSum = w.finish(t)
		for _, n := range c.Nodes {
			if cnt, sum := p10Count(t, n, "sp"); cnt != rows+extra || sum != wantSum+extraSum {
				t.Errorf("node %d sees %d rows (sum %d), want %d (sum %d)", n.Index, cnt, sum, rows+extra, wantSum+extraSum)
			}
		}
		bs := p10Buckets(t, l, "sp")
		if len(bs) != 9 || bs[11] == nil || bs[11]["modulus"] != float64(16) || bs[3]["modulus"] != float64(16) || bs[11]["remainder"] != float64(11) {
			t.Fatalf("buckets after the split: %v", bs)
		}
		var g map[string]any
		for _, x := range anyList(l.Expect(200, "GET", "/databases/sp/colocation-groups", nil)["items"]) {
			if x.(map[string]any)["colocationId"] == float64(1) {
				g = x.(map[string]any)
			}
		}
		if g["bucketCount"] != float64(9) || g["baseBucketCount"] != float64(8) || g["maxModulus"] != float64(16) {
			t.Errorf("colocation group: %v", g)
		}
		// the same bucket cannot be split while another split of the group... and a child can be split again
		task = l.WaitTaskDone(clustertest.TaskID(split(11, nil)), 5*time.Minute)
		if len(p10Buckets(t, l, "sp")) != 10 {
			t.Errorf("a child was not split: %v", task)
		}
	})

	t.Run("a cancel before the switch undoes the split, and the bucket can be split again", func(t *testing.T) {
		// the extension looks at the request to cancel in its backfill (not after it), so the gate is before the backfill
		reached, release := p10Gate(t, c, "sp", "split_after_triggers")
		defer release()
		id := clustertest.TaskID(split(5, nil))
		reached(2 * time.Minute)
		if st, p := l.Do("POST", fmt.Sprintf("/tasks/%v/cancel", id), nil, nil); st != 202 && st != 200 {
			t.Fatalf("cancel: %d %v", st, p)
		}
		// the engine looks for the request every second and interrupts the step while the extension still holds the split at its gate
		task := l.WaitTask(id, 3*time.Minute)
		release()
		if task["state"] == "done" {
			t.Fatalf("a cancelled split finished: %v", task)
		}
		// the extension undoes the split in its own session once the gate is open: the task ends first
		for end := time.Now().Add(2 * time.Minute); ; time.Sleep(time.Second) {
			bs := p10Buckets(t, l, "sp")
			if len(bs) == 10 && bs[5]["state"] == "active" {
				break
			}
			if time.Now().After(end) {
				var rec string
				rows, _ := l.ServicePG("sp").Query(context.Background(),
					"SELECT split_id || ' ' || bucket || ' ' || phase || ' ' || coalesce(detail, '') || ' cancel=' || cancel_requested FROM kingres.bucket_split ORDER BY split_id")
				for rows != nil && rows.Next() {
					var x string
					_ = rows.Scan(&x)
					rec += x + "; "
				}
				t.Fatalf("the cancelled split was not undone: %d buckets, bucket 5 %v; splits: %s task: %v "+"%s", len(bs), bs[5], rec, task, l.LogTail(2500))
			}
		}
		for _, n := range c.Nodes {
			if cnt, sum := p10Count(t, n, "sp"); cnt != rows+extra || sum != wantSum+extraSum {
				t.Errorf("node %d after the cancel: %d rows (sum %d)", n.Index, cnt, sum)
			}
		}
		l.WaitTaskDone(clustertest.TaskID(split(5, nil)), 5*time.Minute)
		if len(p10Buckets(t, l, "sp")) != 11 {
			t.Errorf("the split after the cancel did not happen")
		}
	})

	t.Run("the leader's kingresd restarts in the middle of a split and the task finishes", func(t *testing.T) {
		reached, release := p10Gate(t, c, "sp", "split_after_backfill")
		defer release()
		id := clustertest.TaskID(split(6, nil))
		reached(2 * time.Minute)
		l.Kill()
		release()
		l.Start()
		if !l.Login(clustertest.AdminUser, clustertest.AdminPass) {
			t.Fatal("no sign-in after the restart")
		}
		l.WaitTaskDone(id, 8*time.Minute)
		if len(p10Buckets(t, l, "sp")) != 12 {
			t.Fatalf("buckets after the restart: %v", p10Buckets(t, l, "sp"))
		}
		for _, n := range c.Nodes {
			if cnt, sum := p10Count(t, n, "sp"); cnt != rows+extra || sum != wantSum+extraSum {
				t.Errorf("node %d after the restart: %d rows (sum %d)", n.Index, cnt, sum)
			}
		}
	})
}

func TestDrainOnACluster(t *testing.T) {
	const rows = 600
	c := clustertest.FormWithExtra(t, 3, append([]string{"-rebalance-retry-backoff", "1s"}, standbyArgs...), func(leader *clustertest.Node) {
		body := leader.Expect(202, "POST", "/databases", map[string]any{"name": "rb", "sharding": map[string]any{"enabled": true, "bucketCount": 12}})
		leader.WaitTaskDone(clustertest.TaskID(body), 4*time.Minute)
		sc := leader.ServicePG("rb")
		if _, err := sc.Exec(context.Background(), `CREATE TABLE items (id bigint PRIMARY KEY, val text)`); err != nil {
			t.Fatal(err)
		}
		body = leader.Expect(202, "PUT", "/databases/rb/schemas/public/tables/items/distribution", map[string]any{"method": "hash", "distributionColumn": "id"})
		leader.WaitTaskDone(clustertest.TaskID(body), 2*time.Minute)
		if _, err := sc.Exec(context.Background(), `INSERT INTO items SELECT g, 'row' FROM generate_series(1, $1::int) g`, rows); err != nil {
			t.Fatal(err)
		}
	})
	waitStandbys(t, c, 1)
	l := c.Leader()
	victim := c.Nodes[2]
	// the standbys are in the topology before every one of them is registered in the sharding catalog
	until(t, 2*time.Minute, "every standby to be registered in the catalog", func() bool {
		var n int
		err := l.ServicePG("rb").QueryRow(context.Background(), `SELECT count(*) FROM kingres.node WHERE role = 'standby' AND is_active`).Scan(&n)
		return err == nil && n >= len(c.Nodes)
	})
	wantSum := int64(rows) * int64(rows+1) / 2
	p10Cleanup(t, c, "3s")

	// spread the buckets first
	rep := waitReport(t, l, clustertest.TaskID(l.Expect(202, "POST", "/rebalance", nil)), 10*time.Minute)
	if rep["outcome"] != "completed" {
		t.Fatalf("the first rebalance: %v", rep)
	}
	if primaryBuckets(t, l)[victim.Host] == 0 {
		t.Fatalf("the victim owns no bucket: %v", primaryBuckets(t, l))
	}

	t.Run("the preview lists the moves, the standbys and no blocker; the map group server is refused", func(t *testing.T) {
		pv := l.Expect(200, "GET", "/servers/"+victim.Host+"/drain-preview", nil)
		if pv["canDrain"] != true || pv["moveCount"].(float64) == 0 || len(anyList(pv["standbyShardGroupIds"])) == 0 || pv["canKeepReplicas"] != true {
			t.Fatalf("preview: %v", pv)
		}
		for _, m := range anyList(pv["moves"]) {
			if m.(map[string]any)["reason"] != "drain" {
				t.Errorf("move: %v", m)
			}
		}
		lp := l.Expect(200, "GET", "/servers/"+l.Host+"/drain-preview", nil)
		if lp["canDrain"] != false {
			t.Errorf("the map group server can be drained: %v", lp)
		}
		if st, p := l.Do("POST", "/servers/"+l.Host+"/drain", map[string]any{}, nil); st != 409 || p["code"] != "server_hosts_map_group" {
			t.Errorf("drain of the map group server: %d %v", st, p)
		}
		if st, _ := l.Do("GET", "/servers/no-such-server/drain-preview", nil, nil); st != 404 {
			t.Errorf("unknown server: %d", st)
		}
	})

	t.Run("a drain with writers moves the buckets and the standbys away and loses nothing", func(t *testing.T) {
		w := p10Write(t, c.Nodes[1], "rb", 2_000_000)
		body := l.Expect(202, "POST", "/servers/"+victim.Host+"/drain", map[string]any{})
		id := clustertest.TaskID(body)
		if st, p := l.Do("POST", "/rebalance", nil, nil); st != 409 || p["code"] != "rebalance_in_progress" {
			t.Errorf("a rebalance during a drain: %d %v", st, p)
		}
		l.WaitTaskDone(id, 20*time.Minute)
		extra, extraSum := w.finish(t)
		for _, n := range c.Nodes {
			if cnt, sum := rowsOf(t, n); cnt != rows+extra || sum != wantSum+extraSum {
				t.Errorf("node %d sees %d rows (sum %d), want %d (sum %d)", n.Index, cnt, sum, rows+extra, wantSum+extraSum)
			}
		}
		if got := primaryBuckets(t, l)[victim.Host]; got != 0 {
			t.Errorf("the drained server owns %d buckets", got)
		}
		var standbys, groupsWithout int
		pg := l.ServicePG("rb")
		if err := pg.QueryRow(context.Background(), `SELECT count(*) FROM kingres.node WHERE host = $1 AND role = 'standby'`, victim.Host).Scan(&standbys); err != nil {
			t.Fatal(err)
		}
		if err := pg.QueryRow(context.Background(), `SELECT count(*) FROM (SELECT group_id FROM kingres.node GROUP BY group_id HAVING count(*) FILTER (WHERE role = 'standby') = 0) x`).Scan(&groupsWithout); err != nil {
			t.Fatal(err)
		}
		if standbys != 0 || groupsWithout != 0 {
			t.Errorf("standbys on the drained server: %d, groups left without a standby: %d", standbys, groupsWithout)
		}
		r := l.Expect(200, "GET", fmt.Sprintf("/rebalance/reports/%v", id), nil)
		if r["trigger"] != "drain" || r["outcome"] != "completed" {
			t.Errorf("report: %v", r)
		}
		if s := l.Servers()[victim.Host]; s["shouldHaveShards"] != false {
			t.Errorf("the drained server still holds shards: %v", s["shouldHaveShards"])
		}
	})

	t.Run("a drained server returns to service and takes buckets at the next rebalance", func(t *testing.T) {
		l.Expect(200, "PATCH", "/servers/"+victim.Host, map[string]any{"shouldHaveShards": true})
		if s := l.Servers()[victim.Host]; s["shouldHaveShards"] != true {
			t.Fatalf("server: %v", s)
		}
		rep := waitReport(t, l, clustertest.TaskID(l.Expect(202, "POST", "/rebalance", nil)), 10*time.Minute)
		if rep["outcome"] != "completed" || primaryBuckets(t, l)[victim.Host] == 0 {
			t.Errorf("after the return: %v %v", rep["outcome"], primaryBuckets(t, l))
		}
	})
}

func TestAutomaticRebalanceOnACluster(t *testing.T) {
	const rows = 300
	c := p10Cluster(t, "rb", 12, rows, []string{"-rebalance-retry-backoff", "1s", "-monitor-interval", "3s"})
	l := c.Leader()
	wantSum := int64(rows) * int64(rows+1) / 2
	if st := rbStatus(l); st["automatic"].(map[string]any)["enabled"] != false {
		t.Fatalf("the automatic mode is on by default: %v", st["automatic"])
	}
	// nothing happens while it is off, however uneven the buckets are
	time.Sleep(8 * time.Second)
	if st := rbStatus(l); st["state"] != "idle" {
		t.Fatalf("a run started with the mode off: %v", st)
	}

	// a blocker first: a server that is not live stops the automatic mode from starting
	victim := c.Nodes[2]
	victim.Kill()
	until(t, 90*time.Second, "the server to be suspect or dead", func() bool {
		s := l.Servers()[victim.Host]["status"]
		return s == "suspect" || s == "dead"
	})
	l.Expect(200, "PATCH", "/settings/cluster", map[string]any{"rebalance": map[string]any{"automatic": map[string]any{
		"enabled": true, "settleSeconds": 0, "settleAfterTopologySeconds": 0, "minIntervalSeconds": 0, "checkIntervalSeconds": 5, "threshold": 0.15}}})
	until(t, 2*time.Minute, "the automatic mode to say it is blocked by the servers rule", func() bool {
		a, _ := rbStatus(l)["automatic"].(map[string]any)
		ev, _ := a["lastEvaluation"].(map[string]any)
		if ev == nil || ev["outcome"] != "blocked" {
			return false
		}
		for _, r := range anyList(ev["blockedBy"]) {
			if r == "servers" {
				return true
			}
		}
		return false
	})
	if st := rbStatus(l); st["state"] != "idle" {
		t.Fatalf("a run started although a server is not live: %v", st)
	}

	// the server returns: the imbalance has held, so a run starts by itself and completes
	victim.Start()
	if !victim.Login(clustertest.AdminUser, clustertest.AdminPass) {
		t.Fatal("no sign-in on the restarted server")
	}
	until(t, 8*time.Minute, "an automatic run", func() bool {
		st := rbStatus(l)
		return st["trigger"] == "automatic" || st["lastReport"] != nil
	})
	until(t, 10*time.Minute, "the automatic run to end", func() bool {
		st := rbStatus(l)
		lr, _ := st["lastReport"].(map[string]any)
		return st["state"] == "idle" && lr != nil && lr["trigger"] == "automatic" && lr["outcome"] == "completed"
	})
	counts := primaryBuckets(t, l)
	for id, n := range counts {
		if n < 3 || n > 5 {
			t.Errorf("server %s holds %d buckets after the automatic run: %v", id, n, counts)
		}
	}
	for _, n := range c.Nodes {
		if cnt, sum := p10Count(t, n, "rb"); cnt != rows || sum != wantSum {
			t.Errorf("node %d sees %d rows (sum %d)", n.Index, cnt, sum)
		}
	}
	// balanced: the mode has nothing to do
	until(t, time.Minute, "nothingToDo", func() bool {
		ev, _ := rbStatus(l)["automatic"].(map[string]any)["lastEvaluation"].(map[string]any)
		return ev != nil && ev["outcome"] == "nothingToDo"
	})
}
