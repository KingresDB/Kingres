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
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/clustertest"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/pgtest"
)

// standbyArgs are the settings of the tests of the standbys: a short grace period
// before a group stops waiting for its standby, and frequent monitor rounds. The
// standby loop itself runs every three seconds.
var standbyArgs = []string{"-standby-sync-grace", "12s", "-monitor-interval", "3s"}

// formStandbys forms a cluster of n servers and waits until every group has want
// streaming standbys.
func formStandbys(t *testing.T, n, want int) *clustertest.Cluster {
	t.Helper()
	c := clustertest.FormWithExtra(t, n, standbyArgs, nil)
	waitStandbys(t, c, want)
	return c
}

type standbyView struct {
	group     int
	server    string
	port      int
	state     string
	syncState string
}

// topology reads the standbys of every group as the node reports them.
func topology(t *testing.T, n *clustertest.Node) map[int][]standbyView {
	t.Helper()
	out := map[int][]standbyView{}
	body := n.Expect(200, "GET", "/replication/topology", nil)
	groups, _ := body["groups"].([]any)
	for _, g := range groups {
		gm := g.(map[string]any)
		id := int(gm["id"].(float64))
		out[id] = nil
		sbs, _ := gm["standbys"].([]any)
		for _, s := range sbs {
			sm := s.(map[string]any)
			v := standbyView{group: id, server: sm["serverId"].(string), port: int(sm["port"].(float64)), state: sm["state"].(string)}
			if ss, ok := sm["syncState"].(string); ok {
				v.syncState = ss
			}
			out[id] = append(out[id], v)
		}
	}
	return out
}

// streamingCount counts the standbys that the primary reports as connected (they
// have a synchronous state).
func streamingCount(v []standbyView) int {
	n := 0
	for _, s := range v {
		if s.syncState != "" && s.state == "running" {
			n++
		}
	}
	return n
}

func until(t *testing.T, d time.Duration, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s", d, what)
		}
		time.Sleep(time.Second)
	}
}

// waitStandbys waits until the leader's topology shows want streaming standbys for
// every group, and the groups are as many as the nodes.
func waitStandbys(t *testing.T, c *clustertest.Cluster, want int) {
	t.Helper()
	defer func() {
		if t.Failed() {
			t.Log(standbyLog(c))
		}
	}()
	until(t, 3*time.Minute, fmt.Sprintf("%d streaming standbys in every group", want), func() bool {
		top := topology(t, c.Leader())
		if len(top) < len(c.Nodes) {
			return false
		}
		for _, v := range top {
			if len(v) != want || streamingCount(v) != want {
				return false
			}
		}
		return true
	})
}

// standbyConn connects to the standby of group g that node n runs, through its
// private socket as the service role (peer authentication, no password).
func standbyConn(t *testing.T, n *clustertest.Node, g, port int, db string) *pgx.Conn {
	t.Helper()
	cfg, err := pgx.ParseConfig("")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Host, cfg.Port, cfg.Database, cfg.User = filepath.Join(n.Dir, "s", strconv.Itoa(100+g)), uint16(port), db, "kingresd" //nolint:gosec // test ports
	cfg.TLSConfig, cfg.Fallbacks, cfg.ConnectTimeout = nil, nil, 10*time.Second
	c, err := pgx.ConnectConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("node %d: connecting to the standby of group %d: %v", n.Index, g, err)
	}
	t.Cleanup(func() { _ = c.Close(context.Background()) })
	return c
}

func nodeOf(c *clustertest.Cluster, host string) *clustertest.Node {
	for _, n := range c.Nodes {
		if n.Host == host {
			return n
		}
	}
	return nil
}

// pgCtl runs pg_ctl on the data directory of an instance of the node.
func pgCtl(t *testing.T, n *clustertest.Node, dirID int, args ...string) {
	t.Helper()
	data := filepath.Join(n.DataDir, "instances", strconv.Itoa(dirID), "pgdata")
	full := append([]string{}, args...)
	full = append(full, "-D", data)
	out, err := exec.Command(filepath.Join(pgtest.BinDir(), "pg_ctl"), full...).CombinedOutput() //nolint:gosec // the installation under test
	if err != nil {
		t.Fatalf("pg_ctl %v: %v: %s", args, err, out)
	}
}

func openAlerts(t *testing.T, n *clustertest.Node, kind string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, a := range items(t, n.Expect(200, "GET", "/alerts?state=open&limit=200", nil)) {
		if a["kind"] == kind {
			out = append(out, a)
		}
	}
	return out
}

// Standbys are created for every group by the default replica count, stream, hold
// a copy of the primary, and are placed by the chained ring rule.
func TestStandbysAreCreatedForEveryGroupAndStream(t *testing.T) {
	c := formStandbys(t, 3, 1)
	top := topology(t, c.Leader())
	primaries, _ := primaryOf(t, c.Leader())
	for g := 0; g < 3; g++ {
		sb := top[g][0]
		// The chained rule (the next server in the ring) is covered by the unit tests of the placement; here the
		// cluster has to put the standby on a server other than its primary's.
		if sb.server == primaries[g] {
			t.Errorf("the standby of group %d runs on the server of its primary, %s", g, sb.server)
		}
		if sb.state != "running" {
			t.Errorf("group %d: standby state %q", g, sb.state)
		}
	}
	// Every server reports the same standbys.
	for _, n := range c.Nodes[1:] {
		got := topology(t, n)
		for g := 0; g < 3; g++ {
			if len(got[g]) != 1 || got[g][0].server != top[g][0].server {
				t.Errorf("node %d sees %v for group %d, the leader %v", n.Index, got[g], g, top[g])
			}
		}
	}
	// The standby is a read-only copy: a table made on the primary appears on it.
	p := c.Nodes[0].ServicePG("postgres")
	if _, err := p.Exec(context.Background(), `CREATE TABLE standby_probe AS SELECT 42 AS v`); err != nil {
		t.Fatal(err)
	}
	host := nodeOf(c, top[0][0].server)
	sc := standbyConn(t, host, 0, top[0][0].port, "postgres")
	until(t, time.Minute, "the table on the standby", func() bool {
		var v int
		return sc.QueryRow(context.Background(), `SELECT v FROM standby_probe`).Scan(&v) == nil && v == 42
	})
	var rec bool
	if err := sc.QueryRow(context.Background(), `SELECT pg_is_in_recovery()`).Scan(&rec); err != nil || !rec {
		t.Fatalf("the standby is not in recovery: %v %v", rec, err)
	}
	if _, err := sc.Exec(context.Background(), `CREATE TABLE must_fail (x int)`); err == nil {
		t.Fatal("a write on the standby succeeded")
	}
	// A reconcile that runs meanwhile (the replica count is changed here, as any later change of the
	// cluster would) has its steps pending or running: only the tasks that ended say what happened to
	// the step. The contract promises the state "skipped" for a step that was not run, not for one
	// that has not been reached.
	if st, task, _ := c.Leader().DoH("PATCH", "/settings/cluster", map[string]any{"defaultReplicaCount": 2}, map[string]string{"Prefer": "respond-async"}); st != 202 {
		t.Fatalf("raising the replica count: %d %v", st, task)
	}
	found := false
	for _, tk := range items(t, c.Leader().Expect(200, "GET", "/tasks?limit=50", nil)) {
		if tk["kind"] == "standby.reconcile" && tk["state"] == "done" {
			steps, _ := tk["progress"].(map[string]any)["steps"].([]any)
			for _, s := range steps {
				if st := s.(map[string]any); st["name"] == "standby.register_in_catalog" {
					found = true
					// The step is skipped on purpose: the contract's state, with the reason in the detail.
					if st["state"] != "skipped" {
						t.Errorf("the step standby.register_in_catalog is %v, want skipped", st["state"])
					}
				}
			}
		}
	}
	if !found {
		t.Error("no standby.reconcile task with the step standby.register_in_catalog was found")
	}
}

// The replica count drives the standbys: raised, every group gets a second one;
// lowered to zero, they and their slots are gone.
func TestReplicaCountRaisedAndLowered(t *testing.T) {
	c := formStandbys(t, 3, 1)
	leader := c.Leader()
	primaries, _ := primaryOf(t, leader)
	// Asked for with Prefer: respond-async, the update that starts the reconcile task answers 202 with the Task and
	// a Location (contract 0.9.0); without the header it answers 200 with the settings, as before.
	st, task, hdr := leader.DoH("PATCH", "/settings/cluster", map[string]any{"defaultReplicaCount": 2}, map[string]string{"Prefer": "respond-async"})
	if st != 202 || task["kind"] != "standby.reconcile" || hdr.Get("Preference-Applied") != "respond-async" ||
		hdr.Get("Location") != fmt.Sprintf("/api/v1/tasks/%v", task["id"]) {
		t.Fatalf("update with Prefer: %d %v %v", st, task, hdr)
	}
	leader.WaitTaskDone(clustertest.TaskID(task), 5*time.Minute)
	waitStandbys(t, c, 2)
	if st, _ := leader.Do("PATCH", "/settings/cluster", map[string]any{"defaultReplicaCount": 2}, nil); st != 200 {
		t.Errorf("the same update without the header: %d", st)
	}
	top := topology(t, leader)
	for g, v := range top {
		if v[0].server == v[1].server {
			t.Errorf("group %d has two standbys on one server", g)
		}
		for _, s := range v {
			if s.server == primaries[g] {
				t.Errorf("group %d has a standby on its own primary's server", g)
			}
		}
	}
	// A third server-set is impossible with three servers: the setting 3 is accepted, the groups stay at two,
	// and the cluster says it is under-replicated.
	leader.Expect(200, "PATCH", "/settings/cluster", map[string]any{"defaultReplicaCount": 1})
	waitStandbys(t, c, 1)
	leader.Expect(200, "PATCH", "/settings/cluster", map[string]any{"defaultReplicaCount": 0})
	until(t, 4*time.Minute, "no standby left", func() bool {
		for _, v := range topology(t, leader) {
			if len(v) != 0 {
				return false
			}
		}
		return true
	})
	for _, n := range c.Nodes {
		entries, _ := os.ReadDir(filepath.Join(n.DataDir, "instances"))
		until(t, time.Minute, "the instance directories of the standbys to be gone", func() bool {
			entries, _ = os.ReadDir(filepath.Join(n.DataDir, "instances"))
			return len(entries) == 1
		})
		var slots int
		if err := n.ServicePG("postgres").QueryRow(context.Background(), `SELECT count(*) FROM pg_replication_slots WHERE slot_name LIKE 'kingres_standby_%'`).Scan(&slots); err != nil || slots != 0 {
			t.Errorf("node %d keeps %d standby slots (%v)", n.Index, slots, err)
		}
	}
	// With no standby the synchronous setting is empty again.
	until(t, time.Minute, "an empty synchronous_standby_names", func() bool {
		var v string
		return c.Nodes[0].ServicePG("postgres").QueryRow(context.Background(), `SHOW synchronous_standby_names`).Scan(&v) == nil && v == ""
	})
}

// A commit is acknowledged only when a standby has it; when the standby is lost
// for longer than the grace period the group keeps accepting writes and says so.
func TestSynchronousCommitNeedsTheStandbyUntilTheGracePeriod(t *testing.T) {
	c := formStandbys(t, 3, 1)
	primary := c.Nodes[0]
	top := topology(t, c.Leader())
	holder := nodeOf(c, top[0][0].server)
	ctx := context.Background()
	p := primary.ServicePG("postgres")
	var names string
	until(t, time.Minute, "synchronous_standby_names on the primary", func() bool {
		return p.QueryRow(ctx, `SHOW synchronous_standby_names`).Scan(&names) == nil && strings.HasPrefix(names, "ANY 1 (kingres_standby_")
	})
	if _, err := p.Exec(ctx, `CREATE TABLE sync_probe (n int)`); err != nil {
		t.Fatal(err)
	}
	// With the standby streaming the commit is acknowledged, and the standby has the row.
	if _, err := p.Exec(ctx, `INSERT INTO sync_probe VALUES (1)`); err != nil {
		t.Fatal(err)
	}
	sc := standbyConn(t, holder, 0, top[0][0].port, "postgres")
	var n int
	if err := sc.QueryRow(ctx, `SELECT count(*) FROM sync_probe`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("a synchronous commit was acknowledged but the standby has %d rows (%v)", n, err)
	}

	// The standby goes away and its server's kingresd (which would restart it) with it.
	_ = sc.Close(ctx)
	holder.Stop()
	pgCtl(t, holder, 100, "stop", "-m", "fast", "-w")
	until(t, time.Minute, "the primary to see the standby gone", func() bool {
		var k int
		return p.QueryRow(ctx, `SELECT count(*) FROM pg_stat_replication WHERE application_name LIKE 'kingres_standby_%'`).Scan(&k) == nil && k == 0
	})
	done := make(chan error, 1)
	start := time.Now()
	go func() {
		c2 := primary.ServicePG("postgres")
		_, err := c2.Exec(context.Background(), `INSERT INTO sync_probe VALUES (2)`)
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("a commit was acknowledged after %s although no standby had it (%v)", time.Since(start), err)
	case <-time.After(3 * time.Second):
	}
	// After the grace period the group runs without the standby: the commit goes through and the alert is raised.
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the commit failed: %v", err)
		}
	case <-time.After(2 * time.Minute):
		t.Fatal("the commit was never acknowledged after the grace period")
	}
	until(t, time.Minute, "the alert groupWithoutSynchronousStandby", func() bool {
		return len(openAlerts(t, c.Leader(), "groupWithoutSynchronousStandby")) > 0
	})
	if err := p.QueryRow(ctx, `SHOW synchronous_standby_names`).Scan(&names); err != nil || names != "" {
		t.Fatalf("synchronous_standby_names = %q (%v), want empty after the grace period", names, err)
	}

	// The server comes back: its supervision starts the standby, the group waits for it again, the alert clears.
	holder.Start()
	until(t, 3*time.Minute, "the standby streaming again and the setting restored", func() bool {
		return p.QueryRow(ctx, `SHOW synchronous_standby_names`).Scan(&names) == nil && strings.HasPrefix(names, "ANY 1 (")
	})
	until(t, 2*time.Minute, "the alert to clear", func() bool {
		return len(openAlerts(t, c.Leader(), "groupWithoutSynchronousStandby")) == 0
	})
	sc = standbyConn(t, holder, 0, top[0][0].port, "postgres")
	until(t, time.Minute, "both rows on the standby", func() bool {
		var k int
		return sc.QueryRow(ctx, `SELECT count(*) FROM sync_probe`).Scan(&k) == nil && k == 2
	})
}

// The standbys survive a restart of the primary instance, of kingresd, a renewal
// of the certificates and a restart of the server's kingresd with the standby
// instance stopped.
func TestStandbysSurviveRestartsAndCertificateRenewal(t *testing.T) {
	c := formStandbys(t, 3, 1)
	leader := c.Leader()
	ctx := context.Background()

	// Restart the instance of the primary of group 1: its standby reconnects.
	hosts, ids := primaryOf(t, leader)
	n1 := nodeOf(c, hosts[1])
	body := n1.Expect(202, "POST", fmt.Sprintf("/this-server/instances/%d/restart", ids[1]), nil)
	n1.WaitTaskDone(clustertest.TaskID(body), 3*time.Minute)
	waitStandbys(t, c, 1)

	// Renew the certificates of every server and end the walsenders, so that each
	// standby has to connect again with the new certificate files.
	for _, n := range c.Nodes {
		renewNow(t, leader, n)
	}
	for _, n := range c.Nodes {
		if _, err := n.ServicePG("postgres").Exec(ctx, `SELECT pg_terminate_backend(pid) FROM pg_stat_replication WHERE application_name LIKE 'kingres_standby_%'`); err != nil {
			t.Fatal(err)
		}
	}
	waitStandbys(t, c, 1)

	// kingresd of the server that runs the standby of group 0 stops, and so does the standby; when
	// kingresd is back it starts the standby again.
	top := topology(t, leader)
	holder := nodeOf(c, top[0][0].server)
	holder.Stop()
	pgCtl(t, holder, 100, "stop", "-m", "fast", "-w")
	holder.Start()
	waitStandbys(t, c, 1)
	// kingresd killed and restarted while the standby keeps running changes nothing.
	holder.Kill()
	holder.Start()
	waitStandbys(t, c, 1)
	p := c.Nodes[0].ServicePG("postgres")
	if _, err := p.Exec(ctx, `CREATE TABLE after_restarts AS SELECT 1 AS v`); err != nil {
		t.Fatal(err)
	}
	sc := standbyConn(t, holder, 0, top[0][0].port, "postgres")
	until(t, time.Minute, "a table made after the restarts on the standby", func() bool {
		var v int
		return sc.QueryRow(ctx, `SELECT v FROM after_restarts`).Scan(&v) == nil
	})
}

// A server that is removed takes its standbys with it, its processes and the
// slots they used; the groups that remain place their standbys again.
func TestRemovingAServerRemovesItsStandbys(t *testing.T) {
	c := formStandbys(t, 3, 1)
	leader := c.Leader()
	gone := c.Nodes[2]
	hosts, _ := primaryOf(t, leader)
	body := leader.Expect(202, "DELETE", "/servers/"+gone.Host, nil)
	if res := leader.WaitTask(clustertest.TaskID(body), 8*time.Minute); res["state"] != "done" {
		t.Fatalf("the removal ended %v: %v\nremoved server:\n%s\n%s", res["state"], res["error"], gone.LogTail(5000), standbyLog(c))
	}
	// Two servers remain: each group's standby is on the other server.
	until(t, 6*time.Minute, "one streaming standby for each of the two remaining groups", func() bool {
		top := topology(t, leader)
		n := 0
		for g, v := range top {
			if hosts[g] == gone.Host {
				continue // the group of the removed server is gone with it
			}
			if len(v) == 1 && streamingCount(v) == 1 && v[0].server != gone.Host {
				n++
			}
		}
		return n == 2
	})
	// Nothing of the removed server is left running.
	entries, _ := os.ReadDir(filepath.Join(gone.DataDir, "instances"))
	if len(entries) != 0 {
		t.Errorf("the removed server keeps instance directories: %v", entries)
	}
	// No slot of a standby that is gone remains on any primary.
	for _, n := range c.Nodes[:2] {
		var stale int
		err := n.ServicePG("postgres").QueryRow(context.Background(), `SELECT count(*) FROM pg_replication_slots WHERE slot_name LIKE 'kingres_standby_%' AND NOT active`).Scan(&stale)
		if err != nil || stale != 0 {
			t.Errorf("node %d keeps %d inactive standby slots (%v)", n.Index, stale, err)
		}
	}
}

// standbyLog collects the lines of the logs of every node that mention standbys
// or fail, for the message of a test that timed out.
func standbyLog(c *clustertest.Cluster) string {
	var sb strings.Builder
	for _, n := range c.Nodes {
		fmt.Fprintf(&sb, "---- node %d (%s)\n", n.Index, n.Host)
		var keep []string
		for _, l := range strings.Split(n.Log(), "\n") {
			if strings.Contains(l, "standby") || strings.Contains(l, `"level":"ERROR"`) || strings.Contains(l, "task ended") {
				keep = append(keep, l)
			}
		}
		if len(keep) > 25 {
			keep = keep[len(keep)-25:]
		}
		sb.WriteString(strings.Join(keep, "\n") + "\n")
	}
	return sb.String()
}

// primaryOf returns, for every group, the server that runs its primary and the
// catalog id of that instance.
func primaryOf(t *testing.T, n *clustertest.Node) (host map[int]string, instance map[int]int) {
	t.Helper()
	host, instance = map[int]string{}, map[int]int{}
	body := n.Expect(200, "GET", "/replication/topology", nil)
	groups, _ := body["groups"].([]any)
	for _, g := range groups {
		gm := g.(map[string]any)
		id := int(gm["id"].(float64))
		if p, ok := gm["primary"].(map[string]any); ok {
			host[id], instance[id] = p["serverId"].(string), int(p["instanceId"].(float64))
		}
	}
	return host, instance
}

// A standby that is gone for long makes its slot retain WAL: an alert comes at the
// warning limit, the slot is dropped at the limit, and the standby that comes back
// is rebuilt from a new base backup and has everything.
func TestSlotRetentionAlertsThenDropsThenTheStandbyIsRebuilt(t *testing.T) {
	args := append([]string{"-slot-retention-warn-bytes", "2097152", "-slot-retention-limit-bytes", "12582912"}, standbyArgs...)
	c := clustertest.FormWithExtra(t, 3, args, nil)
	waitStandbys(t, c, 1)
	ctx := context.Background()
	primary := c.Nodes[0]
	top := topology(t, c.Leader())
	holder := nodeOf(c, top[0][0].server)
	p := primary.ServicePG("postgres")
	if _, err := p.Exec(ctx, `CREATE TABLE retain_probe (id int, pad text)`); err != nil {
		t.Fatal(err)
	}
	holder.Stop()
	pgCtl(t, holder, 100, "stop", "-m", "fast", "-w")
	write := func(rows int) {
		t.Helper()
		// Asynchronous: the group waits for its standby until the grace period has passed.
		if _, err := p.Exec(ctx, `SET synchronous_commit = local`); err != nil {
			t.Fatal(err)
		}
		if _, err := p.Exec(ctx, `INSERT INTO retain_probe SELECT g, repeat('x', 1000) FROM generate_series(1, $1) g`, rows); err != nil {
			t.Fatal(err)
		}
	}
	write(4000) // about 4 MB of WAL: past the warning limit, below the drop limit
	until(t, 2*time.Minute, "the alert about the WAL a slot retains", func() bool {
		return len(openAlerts(t, c.Leader(), "replicationLagHigh")) > 0
	})
	var slots int
	if err := p.QueryRow(ctx, `SELECT count(*) FROM pg_replication_slots WHERE slot_name LIKE 'kingres_standby_%'`).Scan(&slots); err != nil || slots != 1 {
		t.Fatalf("the slot must still exist below the limit: %d %v", slots, err)
	}
	write(14000) // past the drop limit
	until(t, 2*time.Minute, "the slot to be dropped", func() bool {
		return p.QueryRow(ctx, `SELECT count(*) FROM pg_replication_slots WHERE slot_name LIKE 'kingres_standby_%'`).Scan(&slots) == nil && slots == 0
	})
	holder.Start()
	waitStandbys(t, c, 1)
	until(t, 2*time.Minute, "the rebuilt standby to hold every row", func() bool {
		sc := standbyConn(t, holder, 0, topology(t, c.Leader())[0][0].port, "postgres")
		var n int
		return sc.QueryRow(ctx, `SELECT count(*) FROM retain_probe`).Scan(&n) == nil && n == 18000
	})
	until(t, time.Minute, "the alert to clear", func() bool {
		return len(openAlerts(t, c.Leader(), "replicationLagHigh")) == 0
	})
}
