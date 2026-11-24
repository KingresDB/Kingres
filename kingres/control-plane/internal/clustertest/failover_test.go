// SPDX-License-Identifier: AGPL-3.0-only

package clustertest_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/clustertest"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/failover"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/fault"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/pgtest"
)

// The failover of an ordinary group (design ha-phase5-part2.md, increment 3), on real
// servers: kingresd processes and PostgreSQL instances, writers on the client entry points.

const (
	foUser = "fo_writer"
	foPass = "Failover-Writer-Pass-1"
)

// foConn connects to database db through the client entry point of n.
func foConn(ctx context.Context, n *clustertest.Node, port int, pool *x509.CertPool, db string) (*pgx.Conn, error) {
	cfg, err := pgx.ParseConfig("")
	if err != nil {
		return nil, err
	}
	cfg.Host, cfg.Port, cfg.Database, cfg.User, cfg.Password = n.Host, uint16(port), db, foUser, foPass //nolint:gosec // test ports
	cfg.Fallbacks, cfg.ConnectTimeout = nil, 5*time.Second
	cfg.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: pool, ServerName: n.Host}
	return pgx.ConnectConfig(ctx, cfg)
}

// foWriters keep inserting rows with unique ids through the entry points; an id is
// acknowledged only when its INSERT returned.
type foWriters struct {
	mu    sync.Mutex
	acked map[int64]time.Time
	errs  int
	last  string
	stop  chan struct{}
	wg    sync.WaitGroup
}

func startFoWriters(nodes []*clustertest.Node, port int, pool *x509.CertPool) *foWriters {
	w := &foWriters{acked: map[int64]time.Time{}, stop: make(chan struct{})}
	for _, n := range nodes {
		w.wg.Add(1)
		go func(n *clustertest.Node) {
			defer w.wg.Done()
			base := int64(n.Index+1) * 1_000_000
			var conn *pgx.Conn
			defer func() {
				if conn != nil {
					_ = conn.Close(context.Background())
				}
			}()
			for i := int64(0); ; {
				select {
				case <-w.stop:
					return
				default:
				}
				ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
				if conn == nil {
					c, err := foConn(ctx, n, port, pool, "fo")
					if err != nil {
						w.note(n, "connect", err)
						cancel()
						time.Sleep(200 * time.Millisecond)
						continue
					}
					conn = c
				}
				_, err := conn.Exec(ctx, `INSERT INTO items VALUES ($1, 'w')`, base+i)
				cancel()
				if err != nil {
					w.note(n, "insert", err)
					w.mu.Lock()
					w.errs++
					w.mu.Unlock()
					_ = conn.Close(context.Background())
					conn = nil
					time.Sleep(100 * time.Millisecond)
					i++ // the next id: an INSERT that was not acknowledged may or may not exist, and is not counted
					continue
				}
				w.mu.Lock()
				w.acked[base+i] = time.Now()
				w.mu.Unlock()
				i++
				time.Sleep(15 * time.Millisecond)
			}
		}(n)
	}
	return w
}

func (w *foWriters) note(n *clustertest.Node, what string, err error) {
	w.mu.Lock()
	w.last = fmt.Sprintf("node %d %s: %v", n.Index, what, err)
	w.mu.Unlock()
}

// lastError is the last error a writer met, for the message of a test that waits for rows.
func (w *foWriters) lastError() string { w.mu.Lock(); defer w.mu.Unlock(); return w.last }

func (w *foWriters) count() int { w.mu.Lock(); defer w.mu.Unlock(); return len(w.acked) }

func (w *foWriters) Stop() map[int64]time.Time {
	select {
	case <-w.stop:
	default:
		close(w.stop)
	}
	w.wg.Wait()
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make(map[int64]time.Time, len(w.acked))
	for k, v := range w.acked {
		out[k] = v
	}
	return out
}

// foCluster forms three servers with a client entry point each and one standby per group,
// and a sharded database fo with a distributed table items owned by a role that writes.
func foCluster(t *testing.T) (*clustertest.Cluster, int, *x509.CertPool) {
	t.Helper()
	return foClusterWith(t, "-lease-duration", "6s", "-lease-renew", "1s", "-lease-margin", "2s")
}

// foClusterWith is foCluster with the lease settings of the caller.
func foClusterWith(t *testing.T, lease ...string) (*clustertest.Cluster, int, *x509.CertPool) {
	t.Helper()
	return foClusterOf(t, 3, lease...)
}

// foClusterOf is foClusterWith for n servers.
func foClusterOf(t *testing.T, n int, lease ...string) (*clustertest.Cluster, int, *x509.CertPool) {
	t.Helper()
	port := pgtest.FreePort(t)
	t.Cleanup(func() { pgtest.ReleasePorts(port) })
	c := clustertest.FormEach(t, n, func(n *clustertest.Node) []string {
		return append(append([]string{"-client-listen", fmt.Sprintf("%s:%d", n.Host, port)}, lease...), standbyArgs...)
	}, nil)
	waitStandbys(t, c, 1)
	leader := c.Leader()
	mkRole(t, leader, foUser, foPass, "kingres_admin")
	body := leader.Expect(202, "POST", "/databases", map[string]any{"name": "fo", "sharding": map[string]any{"enabled": true, "bucketCount": 16},
		"replication": map[string]any{"durability": "sync"}})
	leader.WaitTaskDone(clustertest.TaskID(body), 4*time.Minute)
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(readFile(t, leader.DataDir, "pki", "ca.crt"))
	ctx := context.Background()
	sc := leader.ServicePG("fo")
	defer func() { _ = sc.Close(ctx) }()
	for _, q := range []string{`CREATE TABLE items (id bigint PRIMARY KEY, val text)`, `GRANT ALL ON SCHEMA public TO ` + foUser} {
		if _, err := sc.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	body = leader.Expect(202, "PUT", "/databases/fo/schemas/public/tables/items/distribution", map[string]any{"method": "hash", "distributionColumn": "id"})
	leader.WaitTaskDone(clustertest.TaskID(body), 3*time.Minute)
	// The grant comes after the distribution: the table is then in every group, and the grant reaches its parts.
	if _, err := sc.Exec(ctx, `GRANT SELECT, INSERT, UPDATE, DELETE ON items TO `+foUser); err != nil {
		t.Fatal(err)
	}
	// Every sharded database has a catalog of its own: the standbys are registered in it by the leader's next checks.
	until(t, 3*time.Minute, "the standbys registered in the catalog of database fo", func() bool {
		fc := leader.ServicePG("fo")
		defer func() { _ = fc.Close(ctx) }()
		var n int
		return fc.QueryRow(ctx, `SELECT count(*) FROM kingres.node WHERE role = 'standby'`).Scan(&n) == nil && n >= len(c.Nodes)
	})
	return c, port, pool
}

// foVictim finds a group other than the map group whose primary runs on a server that is not
// the leader's, and the server of its standby.
func foVictim(t *testing.T, c *clustertest.Cluster) (group int, old, nw *clustertest.Node) {
	t.Helper()
	leader := c.Leader()
	primaries, _ := primaryOf(t, leader)
	top := topology(t, leader)
	for g := 1; g < len(c.Nodes); g++ {
		o, n := nodeOf(c, primaries[g]), nodeOf(c, top[g][0].server)
		if o != nil && n != nil && o != leader {
			return g, o, n
		}
	}
	t.Fatalf("no group has its primary away from the leader: %v %v", primaries, top)
	return 0, nil, nil
}

// waitFacts waits until the leader has recorded that group g streamed a synchronous standby.
func waitFacts(t *testing.T, leader *clustertest.Node, g int) {
	t.Helper()
	until(t, 2*time.Minute, "the leader's record of the group's synchronous standby", func() bool {
		c := leader.ServicePG("kingres_system")
		defer func() { _ = c.Close(context.Background()) }()
		var ok bool
		err := c.QueryRow(context.Background(), `SELECT sync_streaming AND NOT without_sync AND seen_at > now() - interval '25 seconds'
			FROM kingres_cp.group_replication_state WHERE group_id = $1`, g).Scan(&ok)
		return err == nil && ok
	})
}

// The primary of a group dies with its server while writers run through the entry points of every
// server: the failover promotes the standby, every acknowledged row is there, the writers go on after
// it, the report says what was done, and the old primary does not accept a write when its server
// comes back. A role and a database made through the API afterwards reach the promoted primary.
func TestFailoverOfAGroupWhoseServerIsDead(t *testing.T) {
	c, port, pool := foCluster(t)
	leader := c.Leader()
	g, old, nw := foVictim(t, c)
	waitFacts(t, leader, g)
	var newNode catalogNode
	for _, n := range catalogStandbys(t, leader) {
		if int(n.group) == g {
			newNode = n
		}
	}
	survivors := []*clustertest.Node{}
	for _, n := range c.Nodes {
		if n != old {
			survivors = append(survivors, n)
		}
	}
	sampler := startFoSampler(c, len(c.Nodes))
	defer func() { sampler.Stop(t) }()
	defer func() {
		if t.Failed() {
			t.Logf("catalog of the leader: %s\nleader log tail:\n%s", foCatalogDump(leader), leader.LogTail(5000))
		}
	}()
	w := startFoWriters(c.Nodes, port, pool)
	defer func() {
		if t.Failed() {
			t.Logf("last writer error: %s", w.lastError())
		}
	}()
	until(t, time.Minute, "rows acknowledged before the kill", func() bool { return w.count() > 40 })

	old.Kill()
	old.StopInstance()
	old.StopStandbys()
	killedAt := time.Now()

	st, resp := leader.DoThroughRestart("POST", "/servers/"+old.Host+"/failover", map[string]any{"mode": "hard", "comment": "test"}, nil)
	if st != 202 {
		t.Fatalf("failover request: %d %v\n%s", st, resp, leader.LogTail(4000))
	}
	// One failover at a time, cluster-wide: a second request while this one runs is refused.
	if st2, p2 := leader.Do("POST", "/servers/"+leader.Host+"/failover", map[string]any{"mode": "hard"}, nil); st2 != 409 || p2["code"] != "failover_in_progress" {
		t.Errorf("a second failover while one runs: %d %v", st2, p2)
	}
	task := leader.WaitTaskDone(clustertest.TaskID(resp), 6*time.Minute)
	t.Logf("the failover took %v after the kill; steps: %v", time.Since(killedAt).Round(time.Second), task["steps"])

	// The catalog follows, the standby is the primary of the group, and the term rose.
	until(t, time.Minute, "the promoted primary in the replication view", func() bool {
		gr := replicationGroup(t, leader, int32(g))
		p, _ := gr["primary"].(map[string]any)
		return p != nil && p["serverId"] == nw.Host && int32(p["instanceId"].(float64)) == newNode.id
	})
	// The writers go on, through the entry points of the survivors.
	before := w.count()
	until(t, time.Minute, "writes acknowledged after the failover", func() bool { return w.count() > before+30 })
	acked := w.Stop()

	// Every acknowledged row is there, read through the entry points of both survivors.
	ctx := context.Background()
	for _, n := range survivors {
		have := foReadIDs(t, n, port, pool)
		lost := 0
		for id := range acked {
			if !have[id] {
				lost++
			}
		}
		if lost > 0 {
			t.Errorf("%d of the %d acknowledged rows are missing when read through %s", lost, len(acked), n.Host)
		}
	}

	// The report names the standby, the terms and the verdict.
	rep := leader.Expect(200, "GET", fmt.Sprintf("/failover/reports/%v", clustertest.TaskID(resp)), nil)
	groups, _ := rep["groups"].([]any)
	// A dead server leaves an incident (its standby cannot be asked): the outcome says so (F3-15).
	if (rep["outcome"] != "completed" && rep["outcome"] != "completedWithWarnings") || len(groups) != 1 {
		t.Fatalf("report: %v", rep)
	}
	gr := groups[0].(map[string]any)
	if gr["outcome"] != "done" || gr["toServerId"] != nw.Host || gr["termAfter"] == nil || gr["termBefore"] == nil || gr["termAfter"].(float64) <= gr["termBefore"].(float64) {
		t.Errorf("group report: %v", gr)
	}
	if dl, _ := gr["dataLoss"].(map[string]any); dl["verdict"] != "none" {
		t.Errorf("verdict: %v", dl)
	}
	if status := leader.Expect(200, "GET", "/failover", nil); status["state"] != "idle" {
		t.Errorf("status after the failover: %v", status)
	}

	// The roles and databases made after the failover reach the promoted primary (the replication to a
	// server whose set of primaries has just changed is retried until it is applied).
	mkRole(t, leader, "fo_after", "After-Failover-Pass-1", "kingres_readonly")
	until(t, 2*time.Minute, "the role on the promoted primary", func() bool {
		cc, err := tryStandbyConn(nw, g, int(newNode.port), "postgres")
		if err != nil {
			return false
		}
		defer func() { _ = cc.Close(ctx) }()
		var n int
		return cc.QueryRow(ctx, `SELECT count(*) FROM pg_roles WHERE rolname = 'fo_after'`).Scan(&n) == nil && n == 1
	})

	var curTerm int64
	foSys := leader.ServicePG("kingres_system")
	if err := foSys.QueryRow(ctx, `SELECT term FROM kingres.catalog_status`).Scan(&curTerm); err != nil {
		t.Fatal(err)
	}
	_ = foSys.Close(ctx)
	oldTerm := curTerm - 1
	// A call of a run that knows an older catalog term is refused by the callee (stale_term): the run was superseded.
	for _, call := range []struct {
		key string
		in  any
	}{
		{"/failover/lease", failover.LeaseRequest{Group: int32(g), Role: failover.LeaseFence, ExpectedTerm: oldTerm}},
		{"/failover/promote", failover.PromoteRequest{Group: int32(g), ExpectedTerm: oldTerm}},
		{"/failover/demote", failover.DemoteRequest{Group: int32(g), Mode: "immediate", ExpectedTerm: oldTerm}},
	} {
		if code, err := leader.InternalCall(nw, "POST", call.key, call.in, nil); err != nil || code != 409 {
			t.Errorf("%s with an old term: %d %v, want 409 stale_term (the leader's term %d, asked with %d)\nlog of the callee:\n%s", call.key, code, err, curTerm, oldTerm, nw.LogTail(3000))
		}
	}
	// The old server comes back: its instance of the group is not started as a primary, and accepts no write.
	old.Start()
	for i := 0; i < 25; i++ {
		time.Sleep(time.Second)
		var st failover.State
		if code, err := leader.InternalCall(old, "GET", fmt.Sprintf("/failover/state?group=%d", g), nil, &st); err == nil && code == 200 {
			if w := st.OutOfRecovery(); len(w) > 0 {
				diag := ""
				for _, n := range c.Nodes {
					if ls, err := leaseState(leader, n); err == nil {
						diag += fmt.Sprintf("\n%s grants %+v leases %+v", n.Host, ls.Grants, ls.Leases)
					}
				}
				diag += foLeaseLines(nw)
				t.Fatalf("the old primary runs and is out of recovery after the failover: %+v (holds %v, lease %s, relinquished %v)%s\nlog of the old server:\n%s", st.Instances, st.Holds, st.LeaseState, st.Relinquished, diag, old.LogTail(5000))
			}
		}
	}
	_ = os.Remove
	_ = filepath.Join
}

// A request that cannot be proved safe is refused with a reason, and nothing happens: the
// refusals of the design (section 3.3 F0), by the API.
func TestFailoverRefusals(t *testing.T) {
	c, _, _ := foCluster(t)
	leader := c.Leader()
	_, old, _ := foVictim(t, c)

	// The planned switchover of the map group is built (TestSwitchoverOfTheMapGroup): its plan names the map group, which moves the manager. A hard failover of a
	// server whose map group primary answers is not, and is refused.
	st, p := leader.Do("POST", "/servers/"+leader.Host+"/failover", map[string]any{"mode": "graceful", "dryRun": true}, nil)
	if gs, _ := p["groups"].([]any); st != 200 || len(gs) != 1 || gs[0].(map[string]any)["isMapGroup"] != true || gs[0].(map[string]any)["movesTheManager"] != true {
		t.Errorf("the plan of a planned switchover of the map group: %d %v", st, p)
	}
	st, p = leader.Do("POST", "/servers/"+leader.Host+"/failover", map[string]any{"mode": "hard"}, nil)
	if st != 422 {
		t.Errorf("a server that runs the map group primary: %d %v", st, p)
	}
	st, p = leader.Do("POST", "/servers/nosuchserver/failover", map[string]any{"mode": "hard"}, nil)
	if st != 404 {
		t.Errorf("an unknown server: %d %v", st, p)
	}
	st, p = leader.Do("POST", "/servers/"+old.Host+"/failover", map[string]any{"mode": "hard", "shardGroupIds": []int{99}}, nil)
	if st != 422 {
		t.Errorf("a group the server is not primary of: %d %v", st, p)
	}
	// The plan of a dry run changes nothing and says what would be done.
	st, plan := leader.Do("POST", "/servers/"+old.Host+"/failover", map[string]any{"mode": "hard", "dryRun": true}, nil)
	if st != 200 || plan["kind"] != "failover" {
		t.Fatalf("dry run: %d %v", st, plan)
	}
	if gs, _ := plan["groups"].([]any); len(gs) == 0 {
		t.Errorf("the plan has no group: %v", plan)
	}
	if status := leader.Expect(200, "GET", "/failover", nil); status["state"] != "idle" {
		t.Errorf("a dry run left a task: %v", status)
	}
	// The primary is alive: the group is not failed over by a stale decision either (the lease is still held).
	_, repl := primaryOf(t, leader)
	_ = repl
}

// foSampler reads, every 250 ms, what every server reports of every group (GET /failover/state, asked as the
// leader) and records a violation when two instances of one group are out of recovery and running at the same
// time on two consecutive readings (two primaries), or when an instance is out of recovery while its role file
// still says standby (the role file must be written before pg_promote). It is used by every failover test.
type foSampler struct {
	stop chan struct{}
	done chan struct{}
	mu   sync.Mutex
	bad  []string
}

func startFoSampler(c *clustertest.Cluster, groups int) *foSampler {
	s := &foSampler{stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(s.done)
		prev := map[int]string{}
		for {
			select {
			case <-s.stop:
				return
			case <-time.After(250 * time.Millisecond):
			}
			if !c.Leader().Running() {
				continue
			}
			for g := 0; g < groups; g++ {
				var writable []string
				for _, n := range c.Nodes {
					var st failover.State
					code, err := c.Leader().InternalCall(n, "GET", fmt.Sprintf("/failover/state?group=%d", g), nil, &st)
					if err != nil || code != 200 {
						continue
					}
					for _, in := range st.OutOfRecovery() {
						writable = append(writable, fmt.Sprintf("%s/%d", n.Host, in.ID))
						if in.Role == "standby" {
							s.add(fmt.Sprintf("group %d: instance %d on %s is out of recovery and its role file says standby", g, in.ID, n.Host))
						}
					}
				}
				now := fmt.Sprint(writable)
				if len(writable) > 1 && prev[g] == now {
					s.add(fmt.Sprintf("group %d: two writable instances at the same time: %v", g, writable))
				}
				prev[g] = now
			}
		}
	}()
	return s
}

func (s *foSampler) add(m string) { s.mu.Lock(); s.bad = append(s.bad, m); s.mu.Unlock() }

// Stop ends the sampling and fails the test on any violation.
func (s *foSampler) Stop(t *testing.T) {
	t.Helper()
	close(s.stop)
	<-s.done
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range s.bad {
		t.Errorf("sampler: %s", m)
	}
}

// A standby that does not hold the lease of its group is not promoted, however the call came: the callee checks
// the lease as the last thing before pg_promote, and puts the role file back.
func TestFailoverPromoteRefusesWithoutTheLease(t *testing.T) {
	c := formStandbys(t, 3, 1)
	leader := c.Leader()
	g, _, nw := foVictim(t, c)
	until(t, 2*time.Minute, "standbys registered", func() bool { return allRegistered(recordNodes(t, leader), 3) })
	sc := leader.ServicePG("kingres_system")
	var id int64
	var inst int
	if err := sc.QueryRow(context.Background(), `SELECT standby_id, instance_id FROM kingres_cp.standby WHERE group_id = $1`, g).Scan(&id, &inst); err != nil {
		t.Fatal(err)
	}
	_ = sc.Close(context.Background())
	code, err := leader.InternalCall(nw, "POST", "/failover/promote", failover.PromoteRequest{Group: int32(g), StandbyID: id, InstanceID: inst, ExpectedTerm: -1}, nil)
	if err != nil || code != 409 {
		t.Fatalf("promote without the lease: %d %v", code, err)
	}
	role, ok := readRole(t, nw, inst)
	if !ok || role.Role != "standby" {
		t.Errorf("the role file after a refused promotion: %+v %v", role, ok)
	}
	var st failover.State
	if code, err := leader.InternalCall(nw, "GET", fmt.Sprintf("/failover/state?group=%d", g), nil, &st); err != nil || code != 200 || len(st.OutOfRecovery()) != 0 {
		t.Errorf("the standby was promoted without the lease: %d %v %+v", code, err, st)
	}
}

// Without a majority nothing is handed to another server.
func TestFailoverRefusedWithoutAMajority(t *testing.T) {
	c := formStandbys(t, 3, 1)
	leader := c.Leader()
	_, old, _ := foVictim(t, c)
	for _, n := range c.Nodes[1:] {
		n.Kill()
	}
	// The leader cannot keep the map group primary without a majority either (its lease is lost and the instance
	// is fenced), so what the request meets is a refusal of one of two kinds: quorum_unavailable when the catalog
	// can still be read, a plain 503 when it cannot. Never a task.
	until(t, 2*time.Minute, "the request is refused with a 503", func() bool {
		st, _ := leader.Do("POST", "/servers/"+old.Host+"/failover", map[string]any{"mode": "hard"}, nil)
		if st == 202 {
			t.Fatal("a failover task was made without a majority")
		}
		return st == 503
	})
}

// A standby that the catalog does not know is refused with cluster_inconsistent.
func TestFailoverRefusedForAStandbyNotInTheCatalog(t *testing.T) {
	c := formStandbys(t, 3, 1)
	leader := c.Leader()
	g, old, _ := foVictim(t, c)
	until(t, 2*time.Minute, "standbys registered", func() bool { return allRegistered(recordNodes(t, leader), 3) })
	sc := leader.ServicePG("kingres_system")
	defer func() { _ = sc.Close(context.Background()) }()
	if _, err := sc.Exec(context.Background(), `UPDATE kingres_cp.standby SET catalog_node_id = 9999 WHERE group_id = $1`, g); err != nil {
		t.Fatal(err)
	}
	st, p := leader.Do("POST", "/servers/"+old.Host+"/failover", map[string]any{"mode": "hard"}, nil)
	if st != 409 || p["code"] != "cluster_inconsistent" {
		t.Errorf("a standby that the catalog does not know: %d %v", st, p)
	}
}

// The leader's kingresd ends at a boundary of a step and is started again; the task is claimed again and ends the
// failover once, with no acknowledged row lost and never two writable instances of the group. The points are the
// engine's crash points at the steps that matter: waiting for the lease, the promotion (after the effect and before
// its record), the catalog and the standby records. Step indexes: 0 check, 1 freeze, 2 stop, 3 lease, 4 choose,
// 5 hand over, 6 promote, 7 catalog, 8 check catalog, 9 repoint, 10 records.
func TestFailoverSurvivesTheLeaderEndingAtAStepBoundary(t *testing.T) {
	for _, pt := range []struct {
		name  string
		point string
		step  int
	}{
		{"after the lease wait", "step_recorded", 3},
		{"promoted but not recorded", "step_ran", 6},
		{"in the catalog step", "step_started", 7},
		{"after the standby records", "step_recorded", 10},
	} {
		t.Run(pt.name, func(t *testing.T) {
			c, port, pool := foClusterWith(t, "-lease-duration", "20s", "-lease-renew", "2s", "-lease-margin", "3s")
			leader := c.Leader()
			g, old, nw := foVictim(t, c)
			waitFacts(t, leader, g)
			sampler := startFoSampler(c, len(c.Nodes))
			survivors := []*clustertest.Node{}
			for _, n := range c.Nodes {
				if n != old {
					survivors = append(survivors, n)
				}
			}
			w := startFoWriters(survivors, port, pool)
			until(t, time.Minute, "rows acknowledged", func() bool { return w.count() > 20 })
			old.Kill()
			old.StopInstance()
			old.StopStandbys()
			leader.SetFaults(fault.Spec{TaskExitPoint: pt.point, TaskExitStep: pt.step})
			// allowDataLoss: after the restart of the leader the check of the group is older than the verdict accepts (the
			// task pauses, which is the safe answer); what this test shows is the takeover of the steps, not the verdict.
			st, resp := leader.DoThroughRestart("POST", "/servers/"+old.Host+"/failover", map[string]any{"mode": "hard", "allowDataLoss": true}, nil)
			if st != 202 {
				t.Fatalf("failover request: %d %v", st, resp)
			}
			until(t, 5*time.Minute, "the leader's kingresd to end at the fault point", func() bool { st, _ := leader.Do("GET", "/healthz", nil, nil); return st != 200 })
			leader.Kill() // reaps the process
			leader.Heal()
			leader.Start()
			until(t, 3*time.Minute, "the restarted leader accepts a sign-in", func() bool { return leader.Login(clustertest.AdminUser, clustertest.AdminPass) })
			task := leader.WaitTaskDone(clustertest.TaskID(resp), 8*time.Minute)
			if task["state"] != "done" {
				t.Fatalf("task after the takeover: %v", task)
			}
			until(t, time.Minute, "writes after the failover", func() bool { n := w.count(); time.Sleep(time.Second); return w.count() > n+5 })
			acked := w.Stop()
			sampler.Stop(t)
			have := foReadIDs(t, leader, port, pool)
			for id := range acked {
				if !have[id] {
					t.Errorf("acknowledged row %d is missing", id)
					break
				}
			}
			rep := leader.Expect(200, "GET", fmt.Sprintf("/failover/reports/%v", clustertest.TaskID(resp)), nil)
			gs, _ := rep["groups"].([]any)
			if len(gs) != 1 || gs[0].(map[string]any)["toServerId"] != nw.Host {
				t.Errorf("report: %v", rep)
			}
		})
	}
}

// foReadIDs reads every id of items through the entry point of n, repeating the whole read until it ends without an
// error (a read that fails half way must not look like a table with fewer rows).
func foReadIDs(t *testing.T, n *clustertest.Node, port int, pool *x509.CertPool) map[int64]bool {
	t.Helper()
	ctx := context.Background()
	var have map[int64]bool
	var last error
	until(t, 2*time.Minute, "a complete read through the entry point of "+n.Host, func() bool {
		conn, err := foConn(ctx, n, port, pool, "fo")
		if err != nil {
			last = err
			return false
		}
		defer func() { _ = conn.Close(ctx) }()
		rows, err := conn.Query(ctx, "SELECT id FROM items")
		if err != nil {
			last = err
			return false
		}
		got := map[int64]bool{}
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				last = err
				rows.Close()
				return false
			}
			got[id] = true
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			last = err
			return false
		}
		have = got
		return true
	})
	if have == nil {
		t.Fatalf("the read through %s never completed: %v", n.Host, last)
	}
	return have
}

// foCatalogDump lists kingres.node and the standby records as the leader has them, for a failing test.
func foCatalogDump(leader *clustertest.Node) string {
	ctx := context.Background()
	var out string
	for _, db := range []string{"kingres_system", "fo"} {
		out += "[" + db + "] " + foNodes(leader, db)
	}
	c := leader.ServicePG("kingres_system")
	defer func() { _ = c.Close(ctx) }()
	rows, err := c.Query(ctx, "SELECT standby_id, group_id, server_id, port, state FROM kingres_cp.standby ORDER BY 1")
	if err == nil {
		for rows.Next() {
			var id, g, port int
			var srv, st string
			_ = rows.Scan(&id, &g, &srv, &port, &st)
			out += fmt.Sprintf("| standby %d group %d on %s:%d %s ", id, g, srv, port, st)
		}
		rows.Close()
	}
	return out
}

func foNodes(leader *clustertest.Node, db string) string {
	ctx := context.Background()
	c := leader.ServicePG(db)
	defer func() { _ = c.Close(ctx) }()
	rows, err := c.Query(ctx, "SELECT node_id, group_id, host, port, role, is_active FROM kingres.node ORDER BY node_id")
	if err != nil {
		return err.Error()
	}
	defer rows.Close()
	var out string
	for rows.Next() {
		var id, g, port int
		var host, role string
		var act bool
		_ = rows.Scan(&id, &g, &host, &port, &role, &act)
		out += fmt.Sprintf("node %d group %d %s:%d %s active=%v; ", id, g, host, port, role, act)
	}
	return out
}
