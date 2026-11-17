// SPDX-License-Identifier: AGPL-3.0-only

package clustertest_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/clustertest"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/lease"
)

// coldFlags: a lease of ten seconds renewed every second with a margin of two, so
// that a restart of kingresd (a few seconds) never costs a lease by itself, and
// standbys, so that the leases of all groups are enforced for real (no fault hook).
var coldFlags = []string{"-lease-duration", "10s", "-lease-renew", "1s", "-lease-margin", "2s", "-monitor-interval", "2s", "-standby-sync-grace", "12s"}

// coldCluster forms three servers with one standby per group and waits until every
// group's lease is held and enforced on its server. It returns the shard group of
// each node; every node has a table restart_probe for the writes of the checks.
func coldCluster(t *testing.T) (*clustertest.Cluster, []int32) {
	t.Helper()
	c := clustertest.FormWithExtra(t, 3, coldFlags, nil)
	waitStandbys(t, c, 1)
	groups := make([]int32, len(c.Nodes))
	for i, n := range c.Nodes {
		pg := n.ServicePG("kingres_system")
		if err := pg.QueryRow(context.Background(), `SELECT group_id FROM kingres.node WHERE host = $1 AND role = 'primary'`, n.Host).Scan(&groups[i]); err != nil {
			t.Fatalf("group of node %d: %v", i, err)
		}
		mustExec(t, n.ServicePG("postgres"), `CREATE TABLE IF NOT EXISTS restart_probe (n bigserial PRIMARY KEY)`)
	}
	for i, n := range c.Nodes {
		waitHeldAndEnforced(t, c.Nodes[(i+1)%3], n, groups[i])
	}
	return c, groups
}

func waitHeldAndEnforced(t *testing.T, as, node *clustertest.Node, group int32) {
	t.Helper()
	until(t, 2*time.Minute, fmt.Sprintf("the enforced lease of group %d held by node %d", group, node.Index), func() bool {
		s, err := leaseState(as, node)
		if err != nil {
			return false
		}
		for _, l := range s.Leases {
			if l.Group == group && l.Holder == node.Host && l.State == "held" && l.Enforced {
				return true
			}
		}
		return false
	})
}

// writeThrough makes a write through the node's own instance and waits for its
// acknowledgement (a synchronous standby has to confirm it).
func writeThrough(t *testing.T, n *clustertest.Node) {
	t.Helper()
	until(t, 2*time.Minute, fmt.Sprintf("a write through node %d", n.Index), func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		return servicePGWrite(ctx, n)
	})
}

func servicePGWrite(ctx context.Context, n *clustertest.Node) bool {
	pw, err := os.ReadFile(filepath.Join(n.DataDir, "service-credentials"))
	if err != nil {
		return false
	}
	c, err := n.PG(ctx, "kingresd", strings.TrimSpace(string(pw)), "postgres")
	if err != nil {
		return false
	}
	defer func() { _ = c.Close(context.WithoutCancel(ctx)) }()
	_, err = c.Exec(ctx, `INSERT INTO restart_probe DEFAULT VALUES`)
	return err == nil
}

// primaryPID is the process id of the node's primary instance, "" when it is not running.
func primaryPID(n *clustertest.Node) string {
	b, err := os.ReadFile(filepath.Join(n.DataDir, "instances", "1", "pgdata", "postmaster.pid"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(strings.SplitN(string(b), "\n", 2)[0])
}

// assertNoCrash fails when a PostgreSQL log of the node tells of an immediate
// shutdown or of a recovery after a crash: a clean stop and start has neither.
func assertNoCrash(t *testing.T, n *clustertest.Node) {
	t.Helper()
	logs := n.InstanceLogs()
	for _, bad := range []string{"immediate shutdown request", "automatic recovery in progress", "not properly shut down"} {
		if strings.Contains(logs, bad) {
			t.Errorf("node %d: a PostgreSQL log says %q\n%s", n.Index, bad, grepLines(logs, bad))
		}
	}
}

func grepLines(s, sub string) string {
	var out []string
	file := ""
	for _, l := range strings.Split(s, "\n") {
		if strings.HasPrefix(l, "==== ") {
			file = l
		}
		if strings.Contains(l, sub) {
			out = append(out, file+": "+l)
		}
	}
	return strings.Join(out, "\n")
}

// assertNoFence fails when the node's audit log has a lease event of loss or fence, or
// its kingresd log (from offset) says the lease was lost.
func assertNoFence(t *testing.T, n *clustertest.Node, logFrom int) {
	t.Helper()
	b, _ := os.ReadFile(filepath.Join(n.DataDir, "audit.jsonl"))
	for _, a := range []string{lease.AuditLeaseFenced, lease.AuditLeaseLost} {
		if strings.Contains(string(b), `"action":"`+a+`"`) {
			t.Errorf("node %d: the audit log has the event %s\n%s", n.Index, a, grepLines(string(b), a))
		}
	}
	if log := n.Log(); logFrom <= len(log) && strings.Contains(log[logFrom:], "the group lease was lost") {
		t.Errorf("node %d: kingresd logged that the group lease was lost\n%s", n.Index, grepLines(log[logFrom:], "lease was lost"))
	}
}

// settled checks the end state of a node: its primary runs, its lease is held, a
// write through it is acknowledged, nothing crashed and nothing was fenced.
func settled(t *testing.T, c *clustertest.Cluster, groups []int32, i, logFrom int) {
	t.Helper()
	n := c.Nodes[i]
	waitInstance(t, n, true, 3*time.Minute)
	waitHeldAndEnforced(t, c.Nodes[(i+1)%3], n, groups[i])
	writeThrough(t, n)
	assertNoCrash(t, n)
	assertNoFence(t, n, logFrom)
}

// The three variants share one cluster (restarting its servers is why it is not the
// shared one) and run in this order: the restart of kingresd alone, the stop and start
// of one whole server, and the cold start of the whole cluster.
func TestRestartsOfServersDoNotCrashOrFenceAnInstance(t *testing.T) {
	c, groups := coldCluster(t)
	marks := make([]int, len(c.Nodes))
	snapshot := func() {
		for i, n := range c.Nodes {
			marks[i] = len(n.Log())
		}
	}
	snapshot()

	t.Run("only kingresd restarts while its instance runs", func(t *testing.T) {
		n := c.Nodes[1]
		before := primaryPID(n)
		if before == "" {
			t.Fatal("the instance does not run")
		}
		n.Stop()
		n.Start()
		time.Sleep(5 * time.Second) // beyond the first renewal rounds
		settled(t, c, groups, 1, marks[1])
		if after := primaryPID(n); after != before {
			t.Errorf("the instance was restarted: postmaster %s before, %s after", before, after)
		}
	})

	t.Run("one whole server stops and starts while the others run", func(t *testing.T) {
		n := c.Nodes[2]
		n.StopOrderly()
		if primaryPID(n) != "" {
			t.Error("the instance still runs after the orderly stop")
		}
		time.Sleep(4 * time.Second)
		marks[2] = len(n.Log())
		n.Start()
		settled(t, c, groups, 2, marks[2])
	})

	t.Run("the whole cluster stops and starts one server at a time", func(t *testing.T) {
		var wg sync.WaitGroup
		errs := make([]string, len(c.Nodes))
		for i, n := range c.Nodes {
			wg.Add(1)
			go func() { // the servers of a compose project are stopped together
				defer wg.Done()
				n.Stop()
				if out, err := n.StopInstances(); err != nil {
					errs[i] = fmt.Sprintf("%v\n%s", err, out)
				}
			}()
		}
		wg.Wait()
		for i, e := range errs {
			if e != "" {
				t.Fatalf("stopping node %d: %s", i, e)
			}
			if primaryPID(c.Nodes[i]) != "" {
				t.Fatalf("the instance of node %d still runs after the orderly stop", i)
			}
		}
		snapshot()
		pause := 4 * time.Second // longer than one renewal round (1 s) and its answer
		c.Nodes[0].Start()
		time.Sleep(pause)
		if primaryPID(c.Nodes[0]) != "" || instanceAccepts(c.Nodes[0]) {
			t.Error("the first server started its primary without a lease (no other server is up)")
		}
		assertNoFence(t, c.Nodes[0], marks[0])
		c.Nodes[1].Start()
		time.Sleep(pause)
		c.Nodes[2].Start()
		for i := range c.Nodes {
			settled(t, c, groups, i, marks[i])
		}
		for i, n := range c.Nodes {
			if !n.Login(clustertest.AdminUser, clustertest.AdminPass) { // the sessions ended with the restart
				t.Fatalf("the administrator cannot sign in on node %d again", i)
			}
		}
		waitStandbys(t, c, 1)
		for i, n := range c.Nodes {
			assertNoCrash(t, n) // the standbys are checked once they run again
			if t.Failed() {
				t.Logf("node %d:\n%s", i, n.LogTail(3000))
			}
		}
	})
}

// An orderly stop whose fast shutdown takes longer than the lease does not end in an
// immediate shutdown by the watchdog: kingresd is gone (no more renewals) and a backend
// that does not react (stopped with SIGSTOP) keeps the shutdown waiting for longer than
// the lease; the watchdog stands down because the stop was requested. The entry point of
// the container does exactly this (kingresd exits, then "kingresd -stop-instances").
func TestOrderlyStopOutlastingTheLeaseIsNotFencedByTheWatchdog(t *testing.T) {
	c, groups := leaseCluster(t, true) // a lease of 6 s, margin 2 s: the watchdog fences 4 s after the last renewal
	victim := c.Nodes[1]
	waitWatchdog(t, victim)

	conn := victim.ServicePG("postgres")
	var pid int
	if err := conn.QueryRow(context.Background(), `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	signal := func(sig string) {
		if out, err := exec.Command("kill", "-"+sig, strconv.Itoa(pid)).CombinedOutput(); err != nil {
			t.Logf("kill -%s %d: %v %s", sig, pid, err, out)
		}
	}
	signal("STOP") // the fast shutdown cannot end its session until this is resumed
	resumed := false
	defer func() {
		if !resumed {
			signal("CONT")
		}
	}()

	victim.Stop() // kingresd exits: nothing renews the lease any more
	done := make(chan error, 1)
	go func() {
		out, err := victim.StopInstances()
		if err != nil {
			err = fmt.Errorf("%w: %s", err, out)
		}
		done <- err
	}()
	// Three times the time the watchdog needs after the last renewal.
	time.Sleep(12 * time.Second)
	select {
	case err := <-done:
		t.Fatalf("the orderly stop ended although a backend holds it: %v", err)
	default:
	}
	if lease.AnyFenced(filepath.Join(victim.DataDir, "lease")) {
		t.Error("the watchdog fenced an instance that was being stopped on purpose")
	}
	signal("CONT")
	resumed = true
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("kingresd -stop-instances: %v", err)
		}
	case <-time.After(2 * time.Minute):
		t.Fatal("the orderly stop did not end after the backend was resumed")
	}
	_ = conn.Close(context.Background())
	if strings.Contains(victim.InstanceLogs(), "immediate shutdown request") {
		t.Errorf("the watchdog turned a clean stop into a crash\n%s", grepLines(victim.InstanceLogs(), "immediate shutdown request"))
	}
	// The instance starts again, without crash recovery, once kingresd holds the lease.
	victim.Start()
	waitInstance(t, victim, true, 2*time.Minute)
	waitLease(t, c.Nodes[2], victim, groups[1], "held", time.Minute)
	assertNoCrash(t, victim)
}
