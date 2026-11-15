// SPDX-License-Identifier: AGPL-3.0-only

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

// The rebalance tests run on a cluster of their own (three servers; the first has
// every bucket when the others join), in the order of a story: preview and start,
// stop and start again, a move that fails, a restart of the leader, and a server
// that is emptied and removed. Nothing here touches the shared cluster.

const rbRows = 1500

// rowsOf counts the rows of items and sums their ids, as seen from node n.
func rowsOf(t *testing.T, n *clustertest.Node) (int, int64) {
	t.Helper()
	c := n.ServicePG("rb")
	var cnt int
	var sum int64
	var err error
	for i := 0; i < 20; i++ { // KS001 can be answered while a switch commits; the statement is repeated
		err = c.QueryRow(context.Background(), `SELECT count(*), coalesce(sum(id), 0)::bigint FROM items`).Scan(&cnt, &sum)
		if err == nil {
			return cnt, sum
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("node %d: counting the rows: %v", n.Index, err)
	return 0, 0
}

func allRowsEqual(t *testing.T, c *clustertest.Cluster, wantCount int, wantSum int64) {
	t.Helper()
	for _, n := range c.Nodes {
		cnt, sum := rowsOf(t, n)
		if cnt != wantCount || sum != wantSum {
			t.Errorf("node %d sees %d rows (sum %d), want %d (sum %d)", n.Index, cnt, sum, wantCount, wantSum)
		}
	}
}

func rbStatus(n *clustertest.Node) map[string]any { return n.Expect(200, "GET", "/rebalance", nil) }

// waitReport waits until the run of task id has a report that is not stopped, and returns it.
func waitReport(t *testing.T, n *clustertest.Node, id any, d time.Duration) map[string]any {
	t.Helper()
	end := time.Now().Add(d)
	for {
		st, rep := n.Do("GET", fmt.Sprintf("/rebalance/reports/%v", id), nil, nil)
		if st == 200 && rep["outcome"] != "stopped" {
			return rep
		}
		if time.Now().After(end) {
			t.Fatalf("no report for run %v: %d %v\nstatus: %v\n%s", id, st, rep, rbStatus(n), n.LogTail(3000))
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func waitState(t *testing.T, n *clustertest.Node, want string, d time.Duration) map[string]any {
	t.Helper()
	end := time.Now().Add(d)
	for {
		s := rbStatus(n)
		if s["state"] == want {
			return s
		}
		if time.Now().After(end) {
			t.Fatalf("rebalance state %v, want %s: %v\n%s", s["state"], want, s, n.LogTail(3000))
		}
		time.Sleep(300 * time.Millisecond)
	}
}

func movesKeys(moves []any) map[string]bool {
	out := map[string]bool{}
	for _, m := range moves {
		x := m.(map[string]any)
		out[fmt.Sprintf("%v/%v->%v", x["colocationId"], x["bucket"], x["toServerId"])] = true
	}
	return out
}

func primaryBuckets(t *testing.T, n *clustertest.Node) map[string]int {
	t.Helper()
	out := map[string]int{}
	for id, s := range n.Servers() {
		b, _ := s["buckets"].(map[string]any)
		out[id] = int(b["primary"].(float64))
	}
	return out
}

// setFault sets the extension's fault point for new sessions on database rb of the leader's instance.
func setFault(t *testing.T, c *clustertest.Cluster, spec string) {
	t.Helper()
	conn := c.Leader().ServicePG("postgres")
	q := `ALTER DATABASE rb RESET kingres_shard.fault_injection`
	if spec != "" {
		q = `ALTER DATABASE rb SET kingres_shard.fault_injection = '` + spec + `'`
	}
	if _, err := conn.Exec(context.Background(), q); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

func TestRebalance(t *testing.T) {
	c := clustertest.FormWithExtra(t, 3, []string{"-rebalance-retry-backoff", "1s"}, func(leader *clustertest.Node) {
		// A database with data exists on the first server alone: every bucket is in group 0.
		body := leader.Expect(202, "POST", "/databases", map[string]any{"name": "rb", "sharding": map[string]any{"enabled": true, "bucketCount": 16}})
		leader.WaitTaskDone(clustertest.TaskID(body), 4*time.Minute)
		sc := leader.ServicePG("rb")
		if _, err := sc.Exec(context.Background(), `CREATE TABLE items (id bigint PRIMARY KEY, val text)`); err != nil {
			t.Fatal(err)
		}
		body = leader.Expect(202, "PUT", "/databases/rb/schemas/public/tables/items/distribution", map[string]any{"method": "hash", "distributionColumn": "id"})
		leader.WaitTaskDone(clustertest.TaskID(body), 2*time.Minute)
		for i := 1; i <= rbRows; i++ {
			if _, err := sc.Exec(context.Background(), `INSERT INTO items VALUES ($1, 'row')`, i); err != nil {
				t.Fatal(err)
			}
		}
	})
	leader := c.Leader()
	wantSum := int64(rbRows) * int64(rbRows+1) / 2
	allRowsEqual(t, c, rbRows, wantSum)
	mkRole(t, leader, "rb_ro", "Readonly-Password-1234", "kingres_readonly")
	login := func(user, pw string) map[string]string {
		st, b := leader.Do("POST", "/auth/login", map[string]any{"username": user, "password": pw, "issueToken": true}, map[string]string{"Authorization": ""})
		if st != 200 {
			t.Fatalf("login %s: %d %v", user, st, b)
		}
		return map[string]string{"Authorization": "Bearer " + b["token"].(string)}
	}
	ro := login("rb_ro", "Readonly-Password-1234")

	// Both new servers wait for a rebalance and the dashboard says why.
	for id, s := range leader.Servers() {
		if id != leader.Host && s["status"] != "pendingRebalance" {
			t.Errorf("server %s is %v, want pendingRebalance", id, s["status"])
		}
	}
	if st := rbStatus(leader); st["needed"] != true || st["state"] != "idle" {
		t.Errorf("status before: %v", st)
	}

	t.Run("refusals and roles before a start", func(t *testing.T) {
		// A stop with nothing running.
		st, p := leader.Do("POST", "/rebalance/stop", nil, nil)
		if st != 409 || p["code"] != "rebalance_not_running" {
			t.Errorf("stop with nothing running: %d %v", st, p)
		}
		// Roles: a read-only role previews and reads, it cannot start or stop.
		if st, _ := leader.Do("POST", "/rebalance/plan", nil, ro); st != 200 {
			t.Errorf("readonly preview: %d", st)
		}
		if st, _ := leader.Do("GET", "/rebalance", nil, ro); st != 200 {
			t.Errorf("readonly status: %d", st)
		}
		for _, p := range []string{"/rebalance", "/rebalance/stop"} {
			if st, _ := leader.Do("POST", p, nil, ro); st != 403 {
				t.Errorf("readonly POST %s: %d, want 403", p, st)
			}
		}
		// No server should hold shards: the refusal says so with its code.
		for _, n := range c.Nodes {
			leader.Expect(200, "PATCH", "/servers/"+n.Host, map[string]any{"shouldHaveShards": false})
		}
		st, p = leader.Do("POST", "/rebalance", nil, nil)
		if st != 409 || p["code"] != "no_receiving_server" {
			t.Errorf("start with no receiving server: %d %v", st, p)
		}
		for _, n := range c.Nodes {
			leader.Expect(200, "PATCH", "/servers/"+n.Host, map[string]any{"shouldHaveShards": true})
		}
	})

	var firstPlan map[string]bool
	t.Run("preview equals what a start does, with traffic during the moves", func(t *testing.T) {
		plan := leader.Expect(200, "POST", "/rebalance/plan", map[string]any{"moveLimit": 500})
		moves := anyList(plan["moves"])
		if plan["needed"] != true || len(moves) == 0 || plan["truncated"] != false {
			t.Fatalf("plan: %v", plan)
		}
		firstPlan = movesKeys(moves)
		var after int
		for _, im := range anyList(plan["serverImpacts"]) {
			after += int(im.(map[string]any)["primaryBucketsAfter"].(float64))
		}
		if after != 16 {
			t.Errorf("the plan places %d buckets, want 16: %v", after, plan["serverImpacts"])
		}

		// Reads and writes run while the buckets move.
		stop := make(chan struct{})
		var wg sync.WaitGroup
		var written, readErrs, lowReads atomic.Int64
		ids := make(chan int64, 100000)
		for w := 0; w < 2; w++ {
			wg.Add(1)
			go func(w int) {
				defer wg.Done()
				conn := c.Nodes[w+1].ServicePG("rb")
				for i := int64(1); ; i++ {
					select {
					case <-stop:
						return
					default:
					}
					id := int64(1_000_000*(w+1)) + i
					ok := false
					for try := 0; try < 40 && !ok; try++ {
						_, err := conn.Exec(context.Background(), `INSERT INTO items VALUES ($1, 'live')`, id)
						ok = err == nil || strings.Contains(fmt.Sprint(err), "duplicate key")
						if !ok {
							time.Sleep(100 * time.Millisecond)
						}
					}
					if !ok {
						t.Errorf("a write of %d never succeeded", id)
						return
					}
					written.Add(1)
					ids <- id
					time.Sleep(20 * time.Millisecond)
				}
			}(w)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			conn := c.Nodes[2].ServicePG("rb")
			for {
				select {
				case <-stop:
					return
				default:
				}
				var n int
				if err := conn.QueryRow(context.Background(), `SELECT count(*) FROM items`).Scan(&n); err != nil {
					readErrs.Add(1) // KS001 when a switch commits mid-scan: the client repeats the statement
				} else if n < rbRows {
					lowReads.Add(1)
				}
				time.Sleep(50 * time.Millisecond)
			}
		}()

		body := leader.Expect(202, "POST", "/rebalance", nil)
		id := clustertest.TaskID(body)
		if body["kind"] != "rebalance" {
			t.Errorf("task: %v", body)
		}
		// A second start while it runs is refused.
		if st, p := leader.Do("POST", "/rebalance", nil, nil); st != 409 || p["code"] != "rebalance_in_progress" {
			t.Errorf("second start: %d %v", st, p)
		}
		rep := waitReport(t, leader, id, 10*time.Minute)
		close(stop)
		wg.Wait()
		close(ids)
		if rep["outcome"] != "completed" {
			t.Fatalf("report: %v", rep)
		}
		leader.WaitTaskDone(id, time.Minute)
		if lowReads.Load() != 0 {
			t.Errorf("%d reads saw fewer rows than were there before the moves", lowReads.Load())
		}
		t.Logf("%d rows written and %d reads repeated during the moves", written.Load(), readErrs.Load())

		// What the start did is what the preview said, and every row is there once.
		did := movesKeys(anyList(rep["moves"]))
		for k := range firstPlan {
			if !did[strings.SplitN(k, "->", 2)[0]+"->"+strings.SplitN(k, "->", 2)[1]] {
				t.Errorf("planned move %s was not made: %v", k, did)
			}
		}
		if len(did) != len(firstPlan) {
			t.Errorf("made %d moves, planned %d", len(did), len(firstPlan))
		}
		var extra, extraSum int64
		for id := range ids {
			extra++
			extraSum += id
		}
		allRowsEqual(t, c, rbRows+int(extra), wantSum+extraSum)

		// The placement is even and the report has before and after.
		counts := primaryBuckets(t, leader)
		for id, n := range counts {
			if n < 5 || n > 6 {
				t.Errorf("server %s holds %d primary buckets", id, n)
			}
		}
		if len(anyList(rep["placementBefore"])) == 0 || len(anyList(rep["placementAfter"])) < 3 {
			t.Errorf("placement in the report: %v / %v", rep["placementBefore"], rep["placementAfter"])
		}
		st := rbStatus(leader)
		if st["state"] != "idle" || st["lastReport"] == nil {
			t.Errorf("status after: %v", st)
		}
		for _, n := range c.Nodes {
			for _, s := range n.Servers() {
				if s["status"] != "live" {
					t.Errorf("node %d: server %v is %v after the rebalance", n.Index, s["id"], s["status"])
				}
			}
		}
		// Nothing is left to do.
		if st, p := leader.Do("POST", "/rebalance", nil, nil); st != 409 || p["code"] != "nothing_to_rebalance" {
			t.Errorf("start on a balanced cluster: %d %v", st, p)
		}
		// The audit log has the operations.
		a := leader.Expect(200, "GET", "/audit?action=startRebalance&limit=50", nil)
		if len(anyList(a["items"])) == 0 {
			t.Errorf("no audit event for startRebalance")
		}
	})

	// From here on the data is counted from every server again and again.
	cnt, sum := rowsOf(t, leader)

	t.Run("a stop in the middle and a start again", func(t *testing.T) {
		// Make the cluster uneven again: the last server no longer should hold shards.
		victim := c.Nodes[2]
		leader.Expect(200, "PATCH", "/servers/"+victim.Host, map[string]any{"shouldHaveShards": false})
		if s := leader.Servers()[victim.Host]["status"]; s != "draining" {
			t.Errorf("a server that should not hold shards and owns buckets: %v, want draining", s)
		}
		gate := filepath.Join(t.TempDir(), "gate")
		if err := os.WriteFile(gate, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		setFault(t, c, "move_after_catchup:wait:"+gate)
		defer setFault(t, c, "")
		body := leader.Expect(202, "POST", "/rebalance", nil)
		id := clustertest.TaskID(body)
		end := time.Now().Add(2 * time.Minute)
		for {
			if _, err := os.Stat(gate + ".reached"); err == nil {
				break
			}
			if time.Now().After(end) {
				t.Fatalf("the move never reached the fault point: %v", rbStatus(leader))
			}
			time.Sleep(200 * time.Millisecond)
		}
		st := rbStatus(leader)
		moves := anyList(st["inFlightMoves"])
		if st["state"] != "running" || len(moves) != 1 {
			t.Fatalf("status with a move in flight: %v", st)
		}
		if ph := moves[0].(map[string]any)["phase"]; ph == nil {
			t.Errorf("the move in flight has no phase: %v", moves[0])
		}
		// Stop: the answer is the status, stopping while the move is cancelled.
		stop := leader.Expect(200, "POST", "/rebalance/stop", nil)
		if stop["state"] != "stopping" {
			t.Errorf("state after the stop request: %v", stop["state"])
		}
		_ = os.Remove(gate) // the move wakes up, sees the cancellation and undoes itself
		s := waitState(t, leader, "stopped", 3*time.Minute)
		if s["stoppedBy"] != "operator" {
			t.Errorf("stoppedBy = %v", s["stoppedBy"])
		}
		if task := leader.Expect(200, "GET", fmt.Sprintf("/tasks/%v", id), nil); task["state"] != "paused" {
			t.Errorf("the task is %v, want paused", task["state"])
		}
		if len(anyList(s["inFlightMoves"])) != 0 {
			t.Errorf("a move is in flight in a stopped rebalance: %v", s["inFlightMoves"])
		}
		allRowsEqual(t, c, cnt, sum)
		// A stopped rebalance does not block changing a server's flag, and a second stop is refused.
		leader.Expect(200, "PATCH", "/servers/"+c.Nodes[1].Host, map[string]any{"shouldHaveShards": true})
		if st, p := leader.Do("POST", "/rebalance/stop", nil, nil); st != 409 || p["code"] != "rebalance_not_running" {
			t.Errorf("stop of a stopped run: %d %v", st, p)
		}
		setFault(t, c, "")
		// Start again: the same task goes on.
		again := leader.Expect(202, "POST", "/rebalance", nil)
		if clustertest.TaskID(again) != id {
			t.Errorf("a start after a stop made task %v, want the stopped task %v", clustertest.TaskID(again), id)
		}
		rep := waitReport(t, leader, id, 10*time.Minute)
		if rep["outcome"] != "completed" {
			t.Fatalf("report after the restart: %v", rep)
		}
		allRowsEqual(t, c, cnt, sum)
		if n := primaryBuckets(t, leader)[victim.Host]; n != 0 {
			t.Errorf("the drained server still owns %d buckets", n)
		}
		skipped := 0
		for _, m := range anyList(rep["moves"]) {
			if m.(map[string]any)["outcome"] == "skipped" {
				skipped++
			}
		}
		t.Logf("report: %v moves, %d skipped", len(anyList(rep["moves"])), skipped)
	})

	t.Run("a move that fails is retried and the run ends failed", func(t *testing.T) {
		// Put some buckets back on the last server, then break every move.
		victim := c.Nodes[2]
		leader.Expect(200, "PATCH", "/servers/"+victim.Host, map[string]any{"shouldHaveShards": true})
		setFault(t, c, "move_after_publication:error")
		defer setFault(t, c, "")
		body := leader.Expect(202, "POST", "/rebalance", nil)
		id := clustertest.TaskID(body)
		rep := waitReport(t, leader, id, 10*time.Minute)
		if rep["outcome"] != "failed" {
			t.Fatalf("report: %v", rep)
		}
		task := leader.WaitTask(id, time.Minute)
		if task["state"] != "failed" {
			t.Errorf("the task is %v", task["state"])
		}
		for _, m := range anyList(rep["moves"]) {
			mv := m.(map[string]any)
			if mv["outcome"] != "failed" || mv["attempts"].(float64) != 3 || mv["error"] == nil {
				t.Errorf("move %v: %v", mv["bucket"], mv)
			}
		}
		if st := rbStatus(leader); st["state"] != "failed" {
			t.Errorf("status after a failed run: %v", st["state"])
		}
		// The failed run raised an alert that links to the task.
		al := leader.Expect(200, "GET", "/alerts?limit=50", nil)
		found := false
		for _, a := range anyList(al["items"]) {
			sub, _ := a.(map[string]any)["subject"].(map[string]any)
			if a.(map[string]any)["kind"] == "taskFailed" && sub != nil && sub["taskId"] == id {
				found = true
			}
		}
		if !found {
			t.Errorf("no taskFailed alert with the task: %v", al)
		}
		allRowsEqual(t, c, cnt, sum)
		// Without the fault the next run moves them.
		setFault(t, c, "")
		body = leader.Expect(202, "POST", "/rebalance", nil)
		if rep := waitReport(t, leader, clustertest.TaskID(body), 10*time.Minute); rep["outcome"] != "completed" {
			t.Fatalf("report: %v", rep)
		}
		allRowsEqual(t, c, cnt, sum)
	})

	t.Run("the leader's kingresd restarts in the middle of a move", func(t *testing.T) {
		victim := c.Nodes[2]
		leader.Expect(200, "PATCH", "/servers/"+victim.Host, map[string]any{"shouldHaveShards": false})
		gate := filepath.Join(t.TempDir(), "gate")
		if err := os.WriteFile(gate, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		setFault(t, c, "move_after_catchup:wait:"+gate)
		defer setFault(t, c, "")
		body := leader.Expect(202, "POST", "/rebalance", nil)
		id := clustertest.TaskID(body)
		end := time.Now().Add(2 * time.Minute)
		for {
			if _, err := os.Stat(gate + ".reached"); err == nil {
				break
			}
			if time.Now().After(end) {
				t.Fatal("the move never reached the fault point")
			}
			time.Sleep(200 * time.Millisecond)
		}
		leader.Kill()
		setFault(t, c, "")
		_ = os.Remove(gate)
		leader.Start()
		if !leader.Login(clustertest.AdminUser, clustertest.AdminPass) {
			t.Fatal("no sign-in after the restart")
		}
		// The task is taken over under a new epoch once its lease expired; the run goes on to its end.
		rep := waitReport(t, leader, id, 10*time.Minute)
		if rep["outcome"] != "completed" {
			t.Fatalf("report after the restart: %v", rep)
		}
		allRowsEqual(t, c, cnt, sum)
		if n := primaryBuckets(t, leader)[victim.Host]; n != 0 {
			t.Errorf("the drained server owns %d buckets", n)
		}
		if out := leader.Expect(200, "GET", fmt.Sprintf("/tasks/%v", id), nil); out["state"] != "done" {
			t.Errorf("task: %v", out["state"])
		}
	})

	t.Run("a server that should not hold shards is emptied and removed", func(t *testing.T) {
		victim := c.Nodes[2]
		if s := leader.Servers()[victim.Host]["status"]; s != "live" {
			t.Errorf("an emptied server is %v, want live", s)
		}
		// It still owns nothing, so it can be removed; the standbys it hosts do not stop that.
		body := leader.Expect(202, "DELETE", "/servers/"+victim.Host, nil)
		leader.WaitTaskDone(clustertest.TaskID(body), 5*time.Minute)
		if _, ok := leader.Servers()[victim.Host]; ok {
			t.Errorf("the removed server is still listed")
		}
		cn, sm := rowsOf(t, leader)
		if cn != cnt || sm != sum {
			t.Errorf("after the removal: %d rows (sum %d), want %d (%d)", cn, sm, cnt, sum)
		}
	})
}
