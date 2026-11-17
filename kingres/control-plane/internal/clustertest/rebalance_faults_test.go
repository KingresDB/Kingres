// SPDX-License-Identifier: AGPL-3.0-only

package clustertest_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/clustertest"
)

// Rebalance under faults (review of the rebalancing work, RB-01, RB-03, RB-04 and "not covered"): the session
// of a move killed during the copy, a server that goes down during the copy, the catch-up and the switch, and
// a manager whose task another holder takes over while its move is in flight. Each case asserts the
// end state of the data, of the extension's own move rows (kingres.bucket_move) and of the run's records.
// The harness has one manager leader (the map group primary, whose instance also hosts the system database), so a
// second manager cannot be run; the second holder is the test itself, which claims the task row exactly as another
// server's engine would. A SIGSTOP of the leader is not usable: its watchdog stops the instance of the group.

// rbFaultCluster is the cluster of TestRebalance: three servers, 16 buckets, every bucket on the first.
func rbFaultCluster(t *testing.T) (*clustertest.Cluster, int, int64) {
	t.Helper()
	c := clustertest.FormWithExtra(t, 3, []string{"-rebalance-retry-backoff", "1s"}, func(leader *clustertest.Node) {
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
	return c, rbRows, int64(rbRows) * int64(rbRows+1) / 2
}

// gateAt arms a fault point of the extension that holds a move at it until the returned release is called.
func gateAt(t *testing.T, c *clustertest.Cluster, point string) (reached func(time.Duration), release func()) {
	t.Helper()
	gate := filepath.Join(t.TempDir(), "gate")
	if err := os.WriteFile(gate, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	setFault(t, c, point+":wait:"+gate)
	reached = func(d time.Duration) {
		t.Helper()
		end := time.Now().Add(d)
		for {
			if _, err := os.Stat(gate + ".reached"); err == nil {
				return
			}
			if time.Now().After(end) {
				t.Fatalf("the move never reached %s: %v", point, rbStatus(c.Leader()))
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
	release = func() { setFault(t, c, ""); _ = os.Remove(gate) }
	return reached, release
}

// extensionMoves returns the phases of the extension's own move rows.
func extensionMoves(t *testing.T, conn *pgx.Conn) map[string]int {
	t.Helper()
	rows, err := conn.Query(context.Background(), `SELECT phase, count(*) FROM kingres.bucket_move GROUP BY phase`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var p string
		var n int
		if err := rows.Scan(&p, &n); err != nil {
			t.Fatal(err)
		}
		out[p] = n
	}
	return out
}

// assertSettled checks the end state: no move of the extension is unfinished, check_cluster has nothing that
// forbids a move, the data is whole and no move row of the run is left running.
func assertSettled(t *testing.T, c *clustertest.Cluster, cnt int, sum int64, runID any) {
	t.Helper()
	rb := c.Leader().ServicePG("rb") // one connection: the polling below must not open one per round
	// A move whose undo could not reach a node stays in its phase until a recovery settles it (the control plane
	// asks for that when the move fails, and the extension's recovery worker does it every interval): wait for it.
	unsettled := func() map[string]int {
		out := map[string]int{}
		for p, n := range extensionMoves(t, rb) {
			switch p {
			case "done", "cleaning", "failed", "cancelled":
			default:
				out[p] = n
			}
		}
		return out
	}
	for end := time.Now().Add(3 * time.Minute); len(unsettled()) > 0 && time.Now().Before(end); time.Sleep(2 * time.Second) {
	}
	for p, n := range unsettled() {
		t.Errorf("%d moves of the extension are still in phase %s", n, p)
	}
	var bad int
	if err := rb.QueryRow(context.Background(), `SELECT count(*) FROM kingres.check_cluster()
		WHERE check_name IN ('stalled_move', 'bucket_moving_without_move', 'move_without_moving_bucket', 'partition_misplaced', 'partition_missing', 'stray_bucket_table', 'bucket_map_incomplete')`).Scan(&bad); err != nil {
		t.Fatal(err)
	}
	if bad != 0 {
		t.Errorf("check_cluster reports %d findings that forbid moving", bad)
	}
	allRowsEqual(t, c, cnt, sum)
	if runID != nil {
		rep := leader(c).Expect(200, "GET", fmt.Sprintf("/rebalance/reports/%v", runID), nil)
		for _, m := range anyList(rep["moves"]) {
			if m.(map[string]any)["outcome"] == nil {
				t.Errorf("a move of the report has no outcome: %v", m)
			}
		}
	}
}

func leader(c *clustertest.Cluster) *clustertest.Node { return c.Leader() }

// finishRun starts again (or starts) until nothing is left to move, and returns the last report.
func finishRun(t *testing.T, c *clustertest.Cluster) map[string]any {
	t.Helper()
	l := c.Leader()
	var rep map[string]any
	for round := 0; round < 6; round++ {
		st, body := l.Do("POST", "/rebalance", nil, nil)
		if st == 409 && body["code"] == "nothing_to_rebalance" {
			return rep
		}
		if st != 202 {
			t.Fatalf("start: %d %v", st, body)
		}
		id := clustertest.TaskID(body)
		for end := time.Now().Add(10 * time.Minute); time.Now().Before(end); time.Sleep(time.Second) {
			s := rbStatus(l)
			if s["state"] == "idle" || s["state"] == "stopped" || s["state"] == "failed" {
				break
			}
		}
		_, rep = l.Do("GET", fmt.Sprintf("/rebalance/reports/%v", id), nil, nil)
		if rep["outcome"] == "completed" {
			return rep
		}
	}
	t.Fatalf("the cluster did not settle: %v", rbStatus(l))
	return nil
}

func nodeByHost(c *clustertest.Cluster, host string) *clustertest.Node {
	for _, n := range c.Nodes {
		if n.Host == host {
			return n
		}
	}
	return nil
}

// bringBack restarts the kingresd of a node whose instance was stopped, and waits until it answers.
func bringBack(t *testing.T, n *clustertest.Node) {
	t.Helper()
	n.Stop()
	n.Start()
	for end := time.Now().Add(120 * time.Second); time.Now().Before(end); time.Sleep(time.Second) {
		if n.Login(clustertest.AdminUser, clustertest.AdminPass) {
			return
		}
	}
	t.Fatalf("no sign-in on the restarted node %d\n%s", n.Index, n.LogTail(2000))
}

func TestRebalanceUnderFaults(t *testing.T) {
	c, cnt, sum := rbFaultCluster(t)
	l := c.Leader()
	allRowsEqual(t, c, cnt, sum)
	ctx := context.Background()

	// RB-04 on real instances: the backend that runs move_bucket is killed during the copy. The client sees a
	// FATAL error, which says nothing about the move: the loop asks the extension (resume_bucket_moves undoes the
	// move whose session is gone) before it records anything, and the run goes on to its end.
	t.Run("the session of a move is killed during the copy", func(t *testing.T) {
		reached, release := gateAt(t, c, "move_after_subscription")
		defer release()
		body := l.Expect(202, "POST", "/rebalance", nil)
		id := clustertest.TaskID(body)
		reached(2 * time.Minute)
		var killed int
		pg := l.ServicePG("postgres")
		rows, err := pg.Query(ctx, `SELECT pg_terminate_backend(pid) FROM pg_stat_activity
			WHERE datname = 'rb' AND query LIKE '%move_bucket(%' AND pid <> pg_backend_pid()`)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			killed++
		}
		rows.Close()
		if killed != 1 {
			t.Fatalf("%d backends of move_bucket were terminated, want 1", killed)
		}
		release()
		rep := waitReport(t, l, id, 10*time.Minute)
		if rep["outcome"] != "completed" {
			t.Fatalf("report: %v", rep)
		}
		retried := 0
		for _, m := range anyList(rep["moves"]) {
			mv := m.(map[string]any)
			if mv["outcome"] != "done" {
				t.Errorf("move %v: %v", mv["bucket"], mv)
			}
			if mv["attempts"].(float64) >= 2 {
				retried++
				t.Logf("move %v needed %v attempts: %v", mv["bucket"], mv["attempts"], mv)
			}
		}
		// The product promises that a move that fails is tried again (up to three attempts) and that the run
		// ends completed; it does not promise that nothing else fails meanwhile: on a loaded machine another
		// move can hit a timeout of its own and be retried too (each is logged above). The killed move must
		// be one of the retried ones, which the extension's record below shows.
		if retried < 1 {
			t.Errorf("no move needed a second attempt, the killed one must have")
		}
		if ph := extensionMoves(t, l.ServicePG("rb")); ph["failed"]+ph["cancelled"] < 1 {
			t.Errorf("the extension has no record of the killed move: %v", ph)
		}
		assertSettled(t, c, cnt, sum, id)
		for id, n := range primaryBuckets(t, l) {
			if n < 5 || n > 6 {
				t.Errorf("server %s holds %d primary buckets", id, n)
			}
		}
	})

	// RB-01 and RB-03 on real instances: another holder claims the task while the manager's move is in flight.
	// The old holder stands down: it neither records the
	// failure of the task, nor raises an alert, nor starts a move; the new holder's view is untouched.
	t.Run("a manager whose task another holder took over does nothing more in its name", func(t *testing.T) {
		l.Expect(200, "PATCH", "/servers/"+c.Nodes[2].Host, map[string]any{"shouldHaveShards": false})
		sys := l.ServicePG("kingres_system")
		reached, release := gateAt(t, c, "move_after_catchup")
		defer release()
		body := l.Expect(202, "POST", "/rebalance", nil)
		id := clustertest.TaskID(body)
		reached(2 * time.Minute)
		var alertsBefore int
		_ = sys.QueryRow(ctx, `SELECT count(*) FROM kingres_cp.alert WHERE kind = 'taskFailed'`).Scan(&alertsBefore)
		// The second holder claims the task row, as another server's engine would after the lease ran out. A
		// frozen kingresd cannot be used for this here: the system database lives on the instance of the manager
		// leader, whose watchdog stops it when its kingresd stops renewing the group lease. The store sees the same
		// thing either way: the old run holds an epoch that is no longer the current one, and its next write is refused.
		var epoch int64
		if err := sys.QueryRow(ctx, `UPDATE kingres_cp.task SET lease_holder = 'second-manager', lease_epoch = lease_epoch + 1,
			lease_expires_at = clock_timestamp() + interval '10 minutes' WHERE id = $1 AND state = 'running' RETURNING lease_epoch`, id).Scan(&epoch); err != nil {
			t.Fatal(err)
		}
		var inFlight int
		_ = sys.QueryRow(ctx, `SELECT count(*) FROM kingres_cp.rebalance_move WHERE task_id = $1`, id).Scan(&inFlight)
		release()
		// Give the old holder every chance to do something: its poll runs every 500 ms and its renew every 10 s.
		time.Sleep(25 * time.Second)
		var state, holder string
		var gotEpoch int64
		var outcome *string
		if err := sys.QueryRow(ctx, `SELECT t.state, coalesce(t.lease_holder, ''), t.lease_epoch, r.outcome FROM kingres_cp.task t
			JOIN kingres_cp.rebalance_run r ON r.task_id = t.id WHERE t.id = $1`, id).Scan(&state, &holder, &gotEpoch, &outcome); err != nil {
			t.Fatal(err)
		}
		if state != "running" || holder != "second-manager" || gotEpoch != epoch || outcome != nil {
			t.Errorf("the old holder changed the new holder's task: state %s holder %q epoch %d outcome %v", state, holder, gotEpoch, outcome)
		}
		var alertsAfter, moveRows int
		_ = sys.QueryRow(ctx, `SELECT count(*) FROM kingres_cp.alert WHERE kind = 'taskFailed'`).Scan(&alertsAfter)
		_ = sys.QueryRow(ctx, `SELECT count(*) FROM kingres_cp.rebalance_move WHERE task_id = $1`, id).Scan(&moveRows)
		if alertsAfter != alertsBefore {
			t.Errorf("the old holder raised %d taskFailed alerts", alertsAfter-alertsBefore)
		}
		if moveRows != inFlight {
			t.Errorf("the old holder recorded %d new moves after it lost the lease", moveRows-inFlight)
		}
		var failedRows int
		_ = sys.QueryRow(ctx, `SELECT count(*) FROM kingres_cp.rebalance_move WHERE task_id = $1 AND state = 'failed'
			AND error LIKE 'interrupted%'`, id).Scan(&failedRows)
		if failedRows != 0 {
			t.Errorf("the old holder marked %d moves of the new holder as interrupted", failedRows)
		}
		// The second holder lets go (its lease ends); the manager claims the task again, settles the move through
		// the extension and the run goes on to its end.
		if _, err := sys.Exec(ctx, `UPDATE kingres_cp.task SET lease_expires_at = clock_timestamp() - interval '1 second' WHERE id = $1`, id); err != nil {
			t.Fatal(err)
		}
		rep := waitReport(t, l, id, 10*time.Minute)
		if rep["outcome"] != "completed" {
			t.Fatalf("report: %v", rep)
		}
		assertSettled(t, c, cnt, sum, id)
		if n := primaryBuckets(t, l)[c.Nodes[2].Host]; n != 0 {
			t.Errorf("the drained server owns %d buckets", n)
		}
		l.Expect(200, "PATCH", "/servers/"+c.Nodes[2].Host, map[string]any{"shouldHaveShards": true})
		finishRun(t, c)
	})

	// A server that goes down while a move is copying, catching up and switching. The target's instance is
	// stopped at the fault point; the run either stops (a safety rule: the node is unreachable) or gives the
	// bucket up. When the server is back a start again finishes the job; the data, the extension's records and the
	// run's records are consistent at the end.
	for _, point := range []string{"move_after_subscription", "move_after_catchup", "move_switch_after_marker"} {
		t.Run("the target server goes down at "+point, func(t *testing.T) {
			shouldHold(l, c, false)
			reached, release := gateAt(t, c, point)
			defer release()
			body := l.Expect(202, "POST", "/rebalance", nil)
			id := clustertest.TaskID(body)
			reached(3 * time.Minute)
			mv := anyList(rbStatus(l)["inFlightMoves"])
			if len(mv) != 1 {
				t.Fatalf("no move in flight: %v", rbStatus(l))
			}
			target := nodeByHost(c, mv[0].(map[string]any)["toServerId"].(string))
			if target == nil || target == l {
				t.Fatalf("the target of the move is %v", mv[0])
			}
			target.StopInstance()
			release()
			// The run ends in one of its documented ways, and nothing is left running in the records.
			for end := time.Now().Add(5 * time.Minute); time.Now().Before(end); time.Sleep(time.Second) {
				if s := rbStatus(l)["state"]; s == "stopped" || s == "failed" || s == "idle" {
					break
				}
			}
			s := rbStatus(l)
			if s["state"] == "running" || s["state"] == "stopping" {
				t.Fatalf("the run is still %v with its target down: %v", s["state"], s)
			}
			sys := l.ServicePG("kingres_system")
			var running int
			_ = sys.QueryRow(ctx, `SELECT count(*) FROM kingres_cp.rebalance_move WHERE task_id = $1 AND state = 'running'`, id).Scan(&running)
			if running != 0 {
				t.Errorf("%d moves of the run are still recorded as running", running)
			}
			bringBack(t, target)
			shouldHold(l, c, false)
			rep := finishRun(t, c)
			if rep != nil && rep["outcome"] != "completed" {
				t.Errorf("the run after the repair: %v", rep["outcome"])
			}
			assertSettled(t, c, cnt, sum, nil)
			shouldHold(l, c, true)
			finishRun(t, c)
			assertSettled(t, c, cnt, sum, nil)
		})
	}
}

// shouldHold sets shouldHaveShards of the manager's server and of the second server together, so that the third
// server alone receives buckets and every move in flight has a target that is not the manager.
func shouldHold(l *clustertest.Node, c *clustertest.Cluster, v bool) {
	for _, n := range c.Nodes[:2] {
		l.Expect(200, "PATCH", "/servers/"+n.Host, map[string]any{"shouldHaveShards": v})
	}
}
