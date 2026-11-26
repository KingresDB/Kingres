// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package clustertest_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/clustertest"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/failover"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/fault"
)

// The cluster tests of the fixes for the review of the manual failover (2026-11-18, F3-01 to F3-16).

// foWaitTask waits until the task is in one of the states (done, failed or paused).
func foWaitTask(t *testing.T, n *clustertest.Node, id any, d time.Duration, states ...string) map[string]any {
	t.Helper()
	var last map[string]any
	until(t, d, fmt.Sprintf("task %v to be %v", id, states), func() bool {
		st, body := n.Do("GET", fmt.Sprintf("/tasks/%v", id), nil, nil)
		last = body
		if st != 200 {
			return false
		}
		for _, s := range states {
			if body["state"] == s {
				return true
			}
		}
		return false
	})
	return last
}

// foVictimOnLeader finds a group whose primary runs on a server that is not the leader's and whose standby runs on the
// leader's server: the leader's server is then the new primary of that group.
func foVictimOnLeader(t *testing.T, c *clustertest.Cluster) (group int, old *clustertest.Node) {
	t.Helper()
	leader := c.Leader()
	primaries, _ := primaryOf(t, leader)
	top := topology(t, leader)
	for g := 1; g < len(c.Nodes); g++ {
		o := nodeOf(c, primaries[g])
		for _, s := range top[g] {
			if s.server == leader.Host && o != nil && o != leader {
				return g, o
			}
		}
	}
	t.Skipf("no group has its primary away from the leader and its standby on the leader: %v %v", primaries, top)
	return 0, nil
}

func foKillServer(old *clustertest.Node) {
	old.Kill()
	old.StopInstance()
	old.StopStandbys()
}

// F3-01. The leader's own server is the new primary, and a step after the promotion fails with a code: the release that
// the failed task sends (OnFail) must not forget the lease of the promoted primary, or the lease runs out at the
// grantors and the watchdog stops the only promoted copy.
func TestFailoverFailedTaskKeepsTheLeaseOfThePromotedPrimary(t *testing.T) {
	c, _, _ := foClusterWith(t, "-lease-duration", "12s", "-lease-renew", "2s", "-lease-margin", "3s")
	leader := c.Leader()
	g, old := foVictimOnLeader(t, c)
	waitFacts(t, leader, g)
	foKillServer(old)
	leader.SetFaults(fault.Spec{FailoverPoint: fmt.Sprintf("Group %d: %s:before", g, failover.StepCatalog), FailoverMode: "fail"})
	st, resp := leader.DoThroughRestart("POST", "/servers/"+old.Host+"/failover", map[string]any{"mode": "hard"}, nil)
	if st != 202 {
		t.Fatalf("failover request: %d %v\n%s", st, resp, leader.LogTail(3000))
	}
	task := foWaitTask(t, leader, clustertest.TaskID(resp), 6*time.Minute, "done", "failed")
	if task["state"] != "failed" {
		t.Fatalf("the injected failure did not fail the task: %v", task)
	}
	// The promoted instance keeps its lease for several lease durations (12 s) after the task failed: it holds it, it
	// runs, and it is out of recovery. Without the fix the release makes the lease run out and the instance is stopped.
	deadline := time.Now().Add(40 * time.Second) // more than three lease durations
	for time.Now().Before(deadline) {
		var s failover.State
		code, err := leader.InternalCall(leader, "GET", fmt.Sprintf("/failover/state?group=%d", g), nil, &s)
		if err != nil || code != 200 {
			t.Fatalf("state of the promoted server: %d %v", code, err)
		}
		if !s.Holds || len(s.OutOfRecovery()) != 1 || !s.Attached {
			t.Fatalf("the promoted primary lost its lease or stopped after the failed task: holds=%v attached=%v adopted=%q instances=%+v\n%s",
				s.Holds, s.Attached, s.Adopted, s.Instances, leader.LogTail(4000))
		}
		time.Sleep(time.Second)
	}
}

// F3-02 and F3-12. A server that does not answer, of which the leader cannot show that its plan knew of a standby, is not
// failed over without the operator's acceptance, and the plan of a dry run says so.
func TestFailoverRefusedWhenFencingOfTheOldPrimaryIsUnproven(t *testing.T) {
	c, _, _ := foClusterWith(t, "-lease-duration", "12s", "-lease-renew", "2s", "-lease-margin", "3s")
	leader := c.Leader()
	g, old, _ := foVictim(t, c)
	waitFacts(t, leader, g)
	foKillServer(old)
	ctx := context.Background()
	sys := leader.ServicePG("kingres_system")
	if _, err := sys.Exec(ctx, `UPDATE kingres_cp.group_replication_state SET planned_standbys = 0 WHERE group_id = $1`, g); err != nil {
		t.Fatal(err)
	}
	_ = sys.Close(ctx)
	st, p := leader.Do("POST", "/servers/"+old.Host+"/failover", map[string]any{"mode": "hard"}, nil)
	if st != 409 || p["code"] != "cluster_inconsistent" {
		t.Fatalf("a failover without proof that the old primary is fenced: %d %v", st, p)
	}
	st, plan := leader.Do("POST", "/servers/"+old.Host+"/failover", map[string]any{"mode": "hard", "dryRun": true}, nil)
	if st != 200 || plan["canProceed"] != false {
		t.Fatalf("the plan must say that the failover cannot proceed: %d %v", st, plan)
	}
	st, resp := leader.DoThroughRestart("POST", "/servers/"+old.Host+"/failover", map[string]any{"mode": "hard", "allowDataLoss": true}, nil)
	if st != 202 {
		t.Fatalf("an accepted failover: %d %v", st, resp)
	}
	leader.WaitTaskDone(clustertest.TaskID(resp), 6*time.Minute)
}

// F3-03. The leader alone does not hear the old primary's server (it is cut off from it), the rest of the cluster does: the
// silence is the leader's, not the server's death, and the verdict must not say that nothing can be lost.
func TestFailoverVerdictIsNotNoneWhenOnlyTheLeaderLostTheServer(t *testing.T) {
	c, _, _ := foClusterWith(t, "-lease-duration", "12s", "-lease-renew", "2s", "-lease-margin", "3s")
	leader := c.Leader()
	g, old, _ := foVictim(t, c)
	waitFacts(t, leader, g)
	leader.Isolate(old)
	defer leader.Heal()
	until(t, 3*time.Minute, "the leader's record of the group to be older than the age that a verdict accepts", func() bool {
		sys := leader.ServicePG("kingres_system")
		defer func() { _ = sys.Close(context.Background()) }()
		var old bool
		err := sys.QueryRow(context.Background(), `SELECT seen_at < now() - interval '40 seconds' FROM kingres_cp.group_replication_state WHERE group_id = $1`, g).Scan(&old)
		return err == nil && old
	})
	st, plan := leader.Do("POST", "/servers/"+old.Host+"/failover", map[string]any{"mode": "hard", "dryRun": true}, nil)
	if st != 200 {
		t.Fatalf("dry run: %d %v", st, plan)
	}
	groups, _ := plan["groups"].([]any)
	if len(groups) != 1 {
		t.Fatalf("plan: %v", plan)
	}
	dl, _ := groups[0].(map[string]any)["dataLoss"].(map[string]any)
	if dl["verdict"] == "none" {
		t.Fatalf("the verdict is none for a primary that ran unobserved by the leader: %v", dl)
	}
}

// F3-04. A database cannot be created while a failover is unfinished, and a failure of the catalog step after the
// promotion pauses the task: nothing fails after the point of no return.
func TestFailoverExcludesDatabaseCreationAndPausesAfterThePromotion(t *testing.T) {
	c, _, _ := foClusterWith(t, "-lease-duration", "12s", "-lease-renew", "2s", "-lease-margin", "3s")
	leader := c.Leader()
	g, old, _ := foVictim(t, c)
	waitFacts(t, leader, g)
	foKillServer(old)
	leader.SetFaults(fault.Spec{FailoverPoint: fmt.Sprintf("Group %d: %s:before", g, failover.StepCatalog), FailoverMode: "pause"})
	st, resp := leader.DoThroughRestart("POST", "/servers/"+old.Host+"/failover", map[string]any{"mode": "hard"}, nil)
	if st != 202 {
		t.Fatalf("failover request: %d %v", st, resp)
	}
	id := clustertest.TaskID(resp)
	task := foWaitTask(t, leader, id, 6*time.Minute, "done", "failed", "paused")
	if task["state"] != "paused" {
		t.Fatalf("the task did not pause at the catalog step: %v", task)
	}
	// The group is promoted; a database created now would find the catalogs half moved.
	st, p := leader.Do("POST", "/databases", map[string]any{"name": "fo2", "sharding": map[string]any{"enabled": true, "bucketCount": 16}}, nil)
	if st != 409 || p["code"] != "failover_in_progress" {
		t.Fatalf("a database created while a failover is unfinished: %d %v", st, p)
	}
	leader.Heal()
	st, resp = leader.DoThroughRestart("POST", "/servers/"+old.Host+"/failover", map[string]any{"mode": "hard", "allowDataLoss": true}, nil)
	if st != 202 && st != 200 {
		t.Fatalf("the repeated request: %d %v", st, resp)
	}
	foWaitTask(t, leader, id, 6*time.Minute, "done")
	// The exclusion is lifted: the creation is accepted (it cannot finish while the dead server is away, so it is not waited for).
	leader.Expect(202, "POST", "/databases", map[string]any{"name": "fo2", "sharding": map[string]any{"enabled": true, "bucketCount": 16}})
}

// F3-09. The entry point of a server whose primary was failed over answers: it forwards to the ring, it does not reset the
// connection because its own catalog cannot be read.
func TestFailoverEntryPointOfTheFailedOverServerForwards(t *testing.T) {
	c, port, pool := foClusterWith(t, "-lease-duration", "12s", "-lease-renew", "2s", "-lease-margin", "3s")
	leader := c.Leader()
	g, old, _ := foVictim(t, c)
	waitFacts(t, leader, g)
	st, resp := leader.DoThroughRestart("POST", "/servers/"+old.Host+"/failover", map[string]any{"mode": "hard", "allowDataLoss": true}, nil)
	if st != 202 {
		t.Fatalf("failover request: %d %v", st, resp)
	}
	leader.WaitTaskDone(clustertest.TaskID(resp), 6*time.Minute)
	// A connection that the entry point forwards ends at another server, whose certificate names that server: the name checked is the leader's.
	foEntryPointAnswers(t, old, port, pool, leader.Host)
}

func foEntryPointAnswers(t *testing.T, n *clustertest.Node, port int, pool *x509.CertPool, serverName string) {
	t.Helper()
	ctx := context.Background()
	var last error
	deadline := time.Now().Add(40 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := foConnAs(ctx, n, port, pool, "fo", serverName)
		if err == nil {
			var cnt int
			err = conn.QueryRow(ctx, `SELECT count(*) FROM items`).Scan(&cnt)
			_ = conn.Close(ctx)
			if err == nil {
				return
			}
		}
		last = err
		time.Sleep(time.Second)
	}
	t.Fatalf("the entry point of %s does not answer after the failover of its primary: %v\n%s", n.Host, last, n.LogTail(3000))
}

// F3-13. The destructive calls of the failover are refused without the fence header of a task run, and without the group.
func TestFailoverCalleeRefusesUnfencedAndUnnamedCalls(t *testing.T) {
	c, _, _ := foClusterWith(t, "-lease-duration", "12s", "-lease-renew", "2s", "-lease-margin", "3s")
	leader := c.Leader()
	_, old, _ := foVictim(t, c)
	body := failover.DemoteRequest{Group: 1, Mode: "immediate", ExpectedTerm: -1}
	if code, err := leader.InternalCallUnfenced(old, "POST", "/failover/demote", body, nil); err != nil || code != 400 {
		t.Errorf("demote without the fence header: %d %v, want 400", code, err)
	}
	if code, err := leader.InternalCallUnfenced(old, "POST", "/failover/promote", failover.PromoteRequest{Group: 1, ExpectedTerm: -1}, nil); err != nil || code != 400 {
		t.Errorf("promote without the fence header: %d %v, want 400", code, err)
	}
	if code, err := leader.InternalCall(old, "POST", "/failover/demote", map[string]any{"mode": "immediate", "expectedTerm": -1}, nil); err != nil || code != 400 {
		t.Errorf("demote without a group: %d %v, want 400", code, err)
	}
	// F4-09: the forward calls of a switchover need the fence of a run as well, and the mark and the stop need the term of the run.
	for path, in := range map[string]any{"/failover/quiesce": failover.QuiesceRequest{Group: 1}, "/failover/mark": failover.MarkRequest{Group: 1, ExpectedTerm: 0},
		"/failover/verify": failover.VerifyRequest{Group: 1, Mark: "0/1"}} {
		if code, err := leader.InternalCallUnfenced(old, "POST", path, in, nil); err != nil || code != 400 {
			t.Errorf("%s without the fence header: %d %v, want 400", path, code, err)
		}
	}
	if code, err := leader.InternalCall(old, "POST", "/failover/mark", failover.MarkRequest{Group: 1, ExpectedTerm: -1}, nil); err != nil || code != 400 {
		t.Errorf("mark without the term of the run: %d %v, want 400", code, err)
	}
	if code, err := leader.InternalCall(old, "POST", "/failover/demote", failover.DemoteRequest{Group: 1, Mode: "fast", ExpectedTerm: -1}, nil); err != nil || code != 400 {
		t.Errorf("a clean stop without the term of the run: %d %v, want 400", code, err)
	}
	// The primary still runs: nothing was stopped by those calls.
	var s failover.State
	if code, err := leader.InternalCall(old, "GET", "/failover/state?group=1", nil, &s); err != nil || code != 200 {
		t.Fatalf("state: %d %v", code, err)
	}
	if s.Quiesced || len(s.OutOfRecovery()) != 1 {
		t.Errorf("the refused calls changed the primary: %+v", s)
	}
}

// foConnAs is foConn with the server name that the certificate is checked against (a forwarded connection ends at another server).
func foConnAs(ctx context.Context, n *clustertest.Node, port int, pool *x509.CertPool, db, serverName string) (*pgx.Conn, error) {
	cfg, err := pgx.ParseConfig("")
	if err != nil {
		return nil, err
	}
	cfg.Host, cfg.Port, cfg.Database, cfg.User, cfg.Password = n.Host, uint16(port), db, foUser, foPass //nolint:gosec // test ports
	cfg.Fallbacks, cfg.ConnectTimeout = nil, 5*time.Second
	cfg.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: pool, ServerName: serverName}
	return pgx.ConnectConfig(ctx, cfg)
}

// foEvents returns the events of the report of a failover task.
func foEvents(t *testing.T, leader *clustertest.Node, id any) []map[string]any {
	t.Helper()
	rep := leader.Expect(200, "GET", fmt.Sprintf("/failover/reports/%v", id), nil)
	var out []map[string]any
	evs, _ := rep["events"].([]any)
	for _, e := range evs {
		if m, ok := e.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// foDeadFailover kills the old primary's server and runs a failover of it that the caller waits for.
func foDeadFailover(t *testing.T, c *clustertest.Cluster, old *clustertest.Node, g int) any {
	t.Helper()
	leader := c.Leader()
	waitFacts(t, leader, g)
	foKillServer(old)
	st, resp := leader.DoThroughRestart("POST", "/servers/"+old.Host+"/failover", map[string]any{"mode": "hard"}, nil)
	if st != 202 {
		t.Fatalf("failover request: %d %v\n%s", st, resp, leader.LogTail(3000))
	}
	return clustertest.TaskID(resp)
}

// F3-10. The prepared transactions are recovered, and the incidents read, in every database that has a catalog: the report
// names the database "fo" as well as the system database.
func TestFailoverRecoversPreparedTransactionsInEveryDatabase(t *testing.T) {
	c, _, _ := foClusterWith(t, "-lease-duration", "12s", "-lease-renew", "2s", "-lease-margin", "3s")
	g, old, _ := foVictim(t, c)
	id := foDeadFailover(t, c, old, g)
	c.Leader().WaitTaskDone(id, 8*time.Minute)
	seen := map[string]bool{}
	for _, e := range foEvents(t, c.Leader(), id) {
		if e["kind"] == "preparedRecovered" {
			msg, _ := e["message"].(string)
			for _, db := range []string{"kingres_system", "fo"} {
				if strings.Contains(msg, `"`+db+`"`) {
					seen[db] = true
				}
			}
		}
	}
	if !seen["kingres_system"] || !seen["fo"] {
		t.Fatalf("recover_prepared_transactions() was not recorded in every database: %v", seen)
	}
}

// F3-05. The call that promotes the standby ends after it wrote the role file and before pg_promote: the repeat of the call
// finishes the promotion. Without the fix the instance (role primary, still in recovery) is refused for ever with not_standby
// and the task fails.
func TestFailoverFinishesAPromotionThatEndedAfterTheRoleFile(t *testing.T) {
	c, _, _ := foClusterWith(t, "-lease-duration", "12s", "-lease-renew", "2s", "-lease-margin", "3s")
	g, old, nw := foVictim(t, c)
	nw.SetFaults(fault.Spec{FailoverPoint: "promote:afterRoleFile", FailoverMode: "fail"})
	id := foDeadFailover(t, c, old, g)
	task := foWaitTask(t, c.Leader(), id, 8*time.Minute, "done", "failed")
	if task["state"] != "done" {
		t.Fatalf("the failover did not finish after the interrupted promotion: %v\n%s", task, nw.LogTail(3000))
	}
	if !strings.Contains(nw.Log(), "ends between the role file and pg_promote") {
		t.Fatal("the interruption was never injected, the test shows nothing")
	}
}

// F3-08. The lease cannot be attached to the promoted instance: the call does not answer success, the step hands the lease
// over again and repeats, and the repeat attaches. The report shows the second hand-over.
func TestFailoverPromoteAnswersLeaseNotHeldWhenTheLeaseCannotBeAttached(t *testing.T) {
	c, _, _ := foClusterWith(t, "-lease-duration", "12s", "-lease-renew", "2s", "-lease-margin", "3s")
	g, old, nw := foVictim(t, c)
	nw.SetFaults(fault.Spec{FailoverPoint: "promote:attach", FailoverMode: "fail"})
	id := foDeadFailover(t, c, old, g)
	c.Leader().WaitTaskDone(id, 8*time.Minute)
	if !strings.Contains(nw.Log(), "the attach of the lease fails") {
		t.Fatal("the failure was never injected, the test shows nothing")
	}
	handovers := 0
	for _, e := range foEvents(t, c.Leader(), id) {
		if e["kind"] == "leaseHandedOver" {
			handovers++
		}
	}
	// The hand-over of the step "Hand the lease" leaves no event when the new server is the leader's own (it holds the lease already):
	// the one after the failed attach is then the only one.
	need := 2
	if nw == c.Leader() {
		need = 1
	}
	if handovers < need {
		t.Fatalf("the lease was handed over %d time(s), want %d: a failed attach was taken for success", handovers, need)
	}
}

// F3-09, the setup in which the defect shows: the failover is paused after the old primary was stopped and before its catalog
// moved, so the old server's only primary (its home instance) is stopped and held back, and its own catalog cannot be read.
// Its client entry point must forward to the ring. Without the fallback to the other servers' catalog it resets the connection.
func TestFailoverEntryPointOfAServerWithoutALocalCatalogForwards(t *testing.T) {
	c, port, pool := foClusterWith(t, "-lease-duration", "12s", "-lease-renew", "2s", "-lease-margin", "3s")
	leader := c.Leader()
	g, old, _ := foVictim(t, c)
	waitFacts(t, leader, g)
	leader.SetFaults(fault.Spec{FailoverPoint: fmt.Sprintf("Group %d: %s:before", g, failover.StepCatalog), FailoverMode: "pause"})
	st, resp := leader.DoThroughRestart("POST", "/servers/"+old.Host+"/failover", map[string]any{"mode": "hard", "allowDataLoss": true}, nil)
	if st != 202 {
		t.Fatalf("failover request: %d %v", st, resp)
	}
	id := clustertest.TaskID(resp)
	if task := foWaitTask(t, leader, id, 6*time.Minute, "done", "failed", "paused"); task["state"] != "paused" {
		t.Fatalf("the task did not pause: %v", task)
	}
	// The old primary is stopped and its server cannot read its catalog: the entry point answers anyway.
	ctx := context.Background()
	var last error
	ok := false
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline) && !ok; time.Sleep(time.Second) {
		// The connection is forwarded to another server, whose certificate names that server: any name of the cluster may be it.
		for _, n := range c.Nodes {
			conn, err := foConnAs(ctx, old, port, pool, "fo", n.Host)
			if err == nil {
				var one int
				err = conn.QueryRow(ctx, `SELECT 1`).Scan(&one)
				_ = conn.Close(ctx)
			}
			if ok, last = err == nil, err; ok {
				break
			}
		}
	}
	if !ok {
		t.Fatalf("the entry point of %s, whose local catalog is unreadable, does not answer: %v\n%s", old.Host, last, old.LogTail(3000))
	}
	leader.Heal()
	st, resp = leader.DoThroughRestart("POST", "/servers/"+old.Host+"/failover", map[string]any{"mode": "hard", "allowDataLoss": true}, nil)
	if st != 202 && st != 200 {
		t.Fatalf("the repeated request: %d %v", st, resp)
	}
	foWaitTask(t, leader, id, 6*time.Minute, "done")
}
