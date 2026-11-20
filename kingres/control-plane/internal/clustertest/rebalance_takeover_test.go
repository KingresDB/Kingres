// SPDX-License-Identifier: AGPL-3.0-only

package clustertest_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/clustertest"
)

// takeoverAt holds a move at the fault point of the extension, lets another holder claim the task, waits until
// the manager has noticed (it stands down) and shows what the extension and the cluster look like while the move
// is still held. Then the move is released and the second holder lets go. The run must end, and nothing is left
// that forbids a move: the data, the extension's records and the catalogs of every node agree.
func takeoverAt(t *testing.T, point string, past bool) {
	t.Helper()
	c, cnt, sum := rbFaultCluster(t)
	l := c.Leader()
	ctx := context.Background()
	l.Expect(200, "PATCH", "/servers/"+c.Nodes[2].Host, map[string]any{"shouldHaveShards": false})
	sys := l.ServicePG("kingres_system")
	reached, release := gateAt(t, c, point)
	defer release()
	body := l.Expect(202, "POST", "/rebalance", nil)
	id := clustertest.TaskID(body)
	reached(2 * time.Minute)
	logBefore := strings.Count(l.Log(), "task lease lost")
	if _, err := sys.Exec(ctx, `UPDATE kingres_cp.task SET lease_holder = 'second-manager', lease_epoch = lease_epoch + 1,
		lease_expires_at = clock_timestamp() + interval '10 minutes' WHERE id = $1 AND state = 'running'`, id); err != nil {
		t.Fatal(err)
	}
	// The manager notices at its next write and stands down: it records nothing and starts nothing.
	end := time.Now().Add(30 * time.Second)
	for strings.Count(l.Log(), "task lease lost") == logBefore && time.Now().Before(end) {
		time.Sleep(200 * time.Millisecond)
	}
	t.Logf("while the move is held, after the manager stood down:\n%s", rbDump(l, id))
	release()
	if past {
		// Past the point of no return the move belongs to the extension and the call is not cancelled when the
		// holder stands down: the move finishes by itself, long before the recovery of the extension (after two
		// node timeouts) would settle a move whose session was cut. With the call cancelled by the holder that
		// stood down it stays in "switching", and the writes to its bucket wait for it.
		rb := l.ServicePG("rb")
		for end := time.Now().Add(45 * time.Second); ; time.Sleep(500 * time.Millisecond) {
			var open int
			if err := rb.QueryRow(ctx, `SELECT count(*) FROM kingres.bucket_move WHERE phase IN ('preparing', 'copying', 'catching_up', 'switching')`).Scan(&open); err == nil && open == 0 {
				break
			}
			if time.Now().After(end) {
				t.Fatalf("the move did not finish by itself 45 s after it was released:\n%s", rbDump(l, id))
			}
		}
	}
	time.Sleep(5 * time.Second)
	t.Logf("5 s after the move was released:\n%s", rbDump(l, id))
	// A connection that was opened minutes ago (it waited for the held move) may have been cut by the instance or the host under
	// load: this statement opens its own.
	sys2 := l.ServicePG("kingres_system")
	if _, err := sys2.Exec(ctx, `UPDATE kingres_cp.task SET lease_expires_at = clock_timestamp() - interval '1 second' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	_ = sys2.Close(ctx)
	rep := waitReport(t, l, id, 5*time.Minute)
	if rep["outcome"] != "completed" {
		t.Fatalf("report: %v\n%s", rep, rbDump(l, id))
	}
	assertSettled(t, c, cnt, sum, id)
}

func TestRebalanceTakeoverInTheSwitch(t *testing.T) { takeoverAt(t, "move_switch_after_marker", true) }

func TestRebalanceTakeoverAtTheCatchUp(t *testing.T) { takeoverAt(t, "move_after_catchup", false) }

// RBR-01 on a real cluster: the call of move_bucket waits for the cluster lock of the extension (another session
// holds it, as a long add_node or a distribution of a large table does) for longer than the guard hold of 30 s.
// The wait is not a failure and not an attempt: the call is waited for, as it is the call of a run that still
// holds the task, and the move is made with its first attempt once the lock is free. The control plane used to
// cancel the call at 30 s, which recorded a failed attempt; three such waits gave the bucket up.
func TestRebalanceCallWaitsForTheClusterLockBeyondTheGuardHold(t *testing.T) {
	c, cnt, sum := rbFaultCluster(t)
	l := c.Leader()
	ctx := context.Background()
	shouldHold(l, c, false)
	lockConn, err := l.PG(ctx, "kingresd", servicePassword(t, l), "rb")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lockConn.Close(ctx) }()
	tx, err := lockConn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `LOCK TABLE kingres.map_version IN EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	released := false
	defer func() {
		if !released {
			_ = tx.Rollback(ctx)
		}
	}()
	body := l.Expect(202, "POST", "/rebalance", nil)
	id := clustertest.TaskID(body)
	// The call is now waiting for the lock: wait until the database says so, then keep the lock past the hold.
	sys := l.ServicePG("rb")
	var waitingSince time.Time
	for end := time.Now().Add(2 * time.Minute); waitingSince.IsZero(); time.Sleep(300 * time.Millisecond) {
		var n int
		if err := sys.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE wait_event_type = 'Lock' AND pid <> pg_backend_pid()`).Scan(&n); err == nil && n > 0 {
			waitingSince = time.Now()
		}
		if time.Now().After(end) {
			t.Fatalf("no session of the call waited for a lock\n%s", rbDump(l, id))
		}
	}
	time.Sleep(40 * time.Second) // longer than the 30 s of the guard hold
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	released = true
	rep := waitReport(t, l, id, 8*time.Minute)
	if rep["outcome"] != "completed" {
		t.Fatalf("report: %v\n%s", rep, rbDump(l, id))
	}
	for _, m := range anyList(rep["moves"]) {
		if a := m.(map[string]any)["attempts"]; a != float64(1) {
			t.Errorf("a move needed %v attempts, the wait for the lock is not an attempt: %v", a, m)
		}
	}
	assertSettled(t, c, cnt, sum, id)
}
