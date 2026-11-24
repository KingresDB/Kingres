// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026 RooyeKhat Media Co

package clustertest_test

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/clustertest"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/failover"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/fault"
)

// The hard failover of the map group while its server is alive (design ha-phase5-part2.md, "a hard failover of a live leader's server"): the old leader's task
// starts the bootstrap of the standby's server, fences its own primary (no renewal, a marker, an immediate stop, the lease handed over only after the stop) and the
// standby's server promotes without a proof, becomes the manager leader and finishes the task.

// mapHardStarted is when the request of each test was made, for the measured time of the failover.
var mapHardStarted sync.Map

// mapHardRequest asks the leader for the hard failover of its own server's map group.
func mapHardRequest(t *testing.T, leader, nw *clustertest.Node, allow bool) any {
	t.Helper()
	body := map[string]any{"mode": "hard", "shardGroupIds": []int{0}, "comment": "map group, live leader",
		"targets": []map[string]any{{"shardGroupId": 0, "serverId": nw.Host}}}
	if allow {
		body["allowDataLoss"] = true
	}
	mapHardStarted.Store(t.Name(), time.Now())
	st, resp := leader.DoThroughRestart("POST", "/servers/"+leader.Host+"/failover", body, nil)
	if st != 202 {
		t.Fatalf("hard failover request: %d %v\n%s", st, resp, leader.LogTail(3000))
	}
	return clustertest.TaskID(resp)
}

// mapHardDone waits for the task to end on the new leader (the old leader's system database is the stopped instance) and fails with the logs when it did not end done.
func mapHardDone(t *testing.T, leader, nw *clustertest.Node, id any) map[string]any {
	t.Helper()
	var task map[string]any
	defer func() {
		if t.Failed() {
			st, body := nw.Do("GET", fmt.Sprintf("/tasks/%v", id), nil, nil)
			t.Logf("task %v on the new leader (%d): %v", id, st, body)
			_, fs := nw.Do("GET", "/failover", nil, nil)
			t.Logf("failover status on the new leader: %v", fs)
			t.Logf("new leader log: %s", quietLog(nw.LogTail(120000)))
			t.Logf("old leader log: %s", quietLog(leader.LogTail(120000)))
		}
	}()
	until(t, 12*time.Minute, "the task to end on the new leader", func() bool {
		st, body := nw.Do("GET", fmt.Sprintf("/tasks/%v", id), nil, nil)
		if st != 200 {
			return false
		}
		if s, _ := body["state"].(string); s == "failed" || s == "done" {
			task = body
			return true
		}
		return false
	})
	if task["state"] != "done" {
		t.Fatalf("the task ended %v: %v", task["state"], task["error"])
	}
	if at, ok := mapHardStarted.Load(t.Name()); ok {
		t.Logf("the failover took %s from the request to the end of the task", time.Since(at.(time.Time)).Round(time.Second))
	}
	return task
}

// mapHardAfter checks what every hard failover of the map group leaves: the new server is the primary of the map group and the manager leader, the old leader's
// instance is not a primary (never two writable primaries), the report names the new server, and nothing acknowledged is lost.
func mapHardAfter(t *testing.T, c *clustertest.Cluster, port int, pool *x509.CertPool, leader, nw *clustertest.Node, w *foWriters, id any) {
	t.Helper()
	hosts, _ := primaryOf(t, nw)
	if hosts[0] != nw.Host {
		t.Fatalf("the primary of the map group is on %s, want %s", hosts[0], nw.Host)
	}
	body := nw.Expect(200, "GET", "/cluster", nil)
	if ml, _ := body["managerLeader"].(map[string]any); ml == nil || ml["serverId"] != nw.Host {
		t.Fatalf("the manager leader is %v, want %s", body["managerLeader"], nw.Host)
	}
	// The old leader's instance is not a writable primary: the new leader asks it for its state (it answers once it knows the new term).
	var st failover.State
	until(t, 3*time.Minute, "the old leader's server to answer the new leader", func() bool {
		code, err := nw.InternalCall(leader, "GET", "/failover/state?group=0", nil, &st)
		return err == nil && code == 200
	})
	if len(st.OutOfRecovery()) != 0 {
		t.Errorf("the old leader's instance of the map group is a primary: %+v", st)
	}
	rep := nw.Expect(200, "GET", fmt.Sprintf("/failover/reports/%v", id), nil)
	groups, _ := rep["groups"].([]any)
	if len(groups) != 1 {
		t.Fatalf("report: %v", rep)
	}
	if g := groups[0].(map[string]any); g["toServerId"] != nw.Host || g["fromServerId"] != leader.Host {
		t.Errorf("report of the map group: %v", g)
	}
	mapAckedExcept(t, c, port, pool, w, leader)
}

// mapAckedExcept is mapAcked without the old leader's entry point: its instance of the map group is the stopped or rebuilding old primary, and a read through its
// entry point needs the map group.
func mapAckedExcept(t *testing.T, c *clustertest.Cluster, port int, pool *x509.CertPool, w *foWriters, except *clustertest.Node) {
	t.Helper()
	before := w.count()
	until(t, 5*time.Minute, "writes after the end", func() bool { return w.count() > before+30 })
	acked := w.Stop()
	for _, n := range c.Nodes {
		if n == except {
			continue
		}
		have := foReadIDs(t, n, port, pool)
		for rid := range acked {
			if !have[rid] {
				t.Errorf("acknowledged row %d is missing when read through %s", rid, n.Host)
				break
			}
		}
	}
}

// The leader's own server is failed over while it is alive, with the loss accepted (the verdict of a group is not what this test is about): the old leader fences
// its primary and hands the lease over, the standby's server promotes and finishes the task. Nothing acknowledged is lost and the writes resume.
func TestHardFailoverOfTheMapGroupWhileItsServerIsAlive(t *testing.T) {
	c, port, pool, leader, nw, w := mapSwitchoverEnv(t)
	sampler := startMapSampler(c)
	targets := mapProbeTargets(t, leader, nw)
	probe := startWriteProbe(t, targets)
	id := mapHardRequest(t, leader, nw, true)
	mapHardDone(t, leader, nw, id)
	ps := probe()
	checkProbe(t, ps, leader, nw)
	checkSwitchPoint(t, ps, leader, targets[1])
	mapHardAfter(t, c, port, pool, leader, nw, w, id)
	sampler.Stop(t)
}

// The primary does not stop (the fault skips the stop, as a postmaster that hangs does): nothing is handed over, nobody renews the lease, and the watchdog stops the
// primary when it runs out; only then does the standby's server hold the lease and promote. There is never a second writable primary, and nothing acknowledged is lost.
func TestHardFailoverOfTheMapGroupWhoseOldPrimaryDoesNotStop(t *testing.T) {
	c, port, pool, leader, nw, w := mapSwitchoverEnv(t)
	sampler := startMapSampler(c)
	targets := mapProbeTargets(t, leader, nw)
	probe := startWriteProbe(t, targets)
	leader.SetFaults(fault.Spec{FailoverPoint: "demote:nostop", FailoverMode: "skip"})
	id := mapHardRequest(t, leader, nw, true)
	mapHardDone(t, leader, nw, id)
	if !strings.Contains(leader.LogTail(400000), "the old primary is not stopped (test fault demote:nostop)") {
		t.Errorf("the fault did not take effect: the old primary was stopped by the fence")
	}
	ps := probe()
	checkProbe(t, ps, leader, nw)
	checkSwitchPoint(t, ps, leader, targets[1])
	mapHardAfter(t, c, port, pool, leader, nw, w, id)
	sampler.Stop(t)
}

// kingresd of the old leader ends right after it fenced its primary and handed the lease over (the leader changes at the stop): the standby's server does not depend on it,
// promotes, becomes the leader and finishes the task; the old primary does not start again when the old leader's kingresd is back.
func TestHardFailoverOfTheMapGroupWhoseOldLeaderEndsAfterTheFence(t *testing.T) {
	c, port, pool, leader, nw, w := mapSwitchoverEnv(t)
	sampler := startMapSampler(c)
	leader.SetFaults(fault.Spec{FailoverPoint: "demote:exit", FailoverMode: "exit"})
	id := mapHardRequest(t, leader, nw, true)
	until(t, 5*time.Minute, "the old leader's kingresd to end after the fence", func() bool {
		st, _ := leader.Do("GET", "/healthz", nil, nil)
		return st != 200
	})
	leader.Kill()
	leader.Heal()
	time.Sleep(5 * time.Second) // the old leader is away for a while
	leader.Start()
	mapHardDone(t, leader, nw, id)
	mapHardAfter(t, c, port, pool, leader, nw, w, id)
	sampler.Stop(t)
}

// A request that cannot be served changes nothing: the standby's server is gone, so no standby can be read; the answer is a refusal and the old leader goes on
// as the primary and the leader (the fence was never asked for).
func TestHardFailoverOfTheMapGroupWithoutAReadableStandbyChangesNothing(t *testing.T) {
	_, _, _, leader, nw, w := mapSwitchoverEnv(t)
	foKillServer(nw)
	body := map[string]any{"mode": "hard", "shardGroupIds": []int{0}, "allowDataLoss": true}
	var st int
	var resp map[string]any
	until(t, 3*time.Minute, "the request to be refused or to fail", func() bool {
		st, resp = leader.Do("POST", "/servers/"+leader.Host+"/failover", body, nil)
		return st != 503
	})
	if st == 202 {
		// the task started: it must fail at its stop step with nothing changed
		id := clustertest.TaskID(resp)
		var task map[string]any
		until(t, 5*time.Minute, "the task to fail", func() bool {
			s, b := leader.Do("GET", fmt.Sprintf("/tasks/%v", id), nil, nil)
			if s == 200 && (b["state"] == "failed" || b["state"] == "paused") {
				task = b
				return true
			}
			return false
		})
		t.Logf("the task: %v", task["state"])
	} else if st/100 != 4 {
		t.Fatalf("answer %d %v", st, resp)
	}
	hosts, _ := primaryOf(t, leader)
	if hosts[0] != leader.Host {
		t.Fatalf("the primary of the map group moved to %s", hosts[0])
	}
	before := w.count()
	until(t, 2*time.Minute, "writes on the old leader's side", func() bool { return w.count() > before+20 })
	if st, _ := leader.Do("GET", "/cluster", nil, nil); st != 200 {
		t.Fatalf("the old leader no longer answers: %d", st)
	}
	w.Stop()
}

// The loss that the candidate judges after the stop is not the one the leader accepted (the fault makes the facts say that no synchronous standby streamed): the
// bootstrap pauses with nothing promoted, and the repeated request, sent to the standby's server, carries the acceptance and finishes the failover.
func TestHardFailoverOfTheMapGroupPausesAfterTheStopAndTheRepeatedRequestFinishesIt(t *testing.T) {
	c, port, pool, leader, nw, w := mapSwitchoverEnv(t)
	sampler := startMapSampler(c)
	leader.SetFaults(fault.Spec{FailoverPoint: "maphard:facts", FailoverMode: "async"})
	// no acceptance of a loss in the request: the verdict of the leader (a synchronous standby streamed) is none
	st, resp := leader.DoThroughRestart("POST", "/servers/"+leader.Host+"/failover", map[string]any{"mode": "hard", "shardGroupIds": []int{0},
		"targets": []map[string]any{{"shardGroupId": 0, "serverId": nw.Host}}}, nil)
	if st != 202 {
		// the map group's standby is not synchronous in this cluster: the verdict is a loss before the stop and the request needs the decision
		t.Skipf("the leader judged a loss before the stop (%d %v): the case below needs a synchronous standby of the map group", st, resp)
	}
	id := clustertest.TaskID(resp)
	until(t, 5*time.Minute, "the old leader's system database to be stopped", func() bool {
		s, _ := leader.Do("GET", "/cluster", nil, nil)
		return s != 200
	})
	until(t, 3*time.Minute, "the bootstrap to pause for the decision", func() bool {
		s, b := nw.Do("GET", "/failover", nil, nil)
		if s != 200 {
			return false
		}
		gs, _ := b["groups"].([]any)
		if len(gs) == 0 {
			return false
		}
		msg, _ := gs[0].(map[string]any)["message"].(string)
		return strings.Contains(msg, "could lose data") && strings.Contains(msg, "Send the request to server "+nw.Host)
	})
	var sn failover.State
	if code, err := leader.InternalCall(nw, "GET", "/failover/state?group=0", nil, &sn); err == nil && code == 200 && len(sn.OutOfRecovery()) != 0 {
		t.Fatalf("the standby was promoted although the loss was not accepted: %+v", sn)
	}
	st, resp = nw.DoThroughRestart("POST", "/servers/"+leader.Host+"/failover", map[string]any{"mode": "hard", "allowDataLoss": true, "comment": "repeated", "targets": []map[string]any{{"shardGroupId": 0, "serverId": nw.Host}}}, nil)
	if st != 202 {
		t.Fatalf("the repeated request: %d %v\n%s", st, resp, quietLog(nw.LogTail(20000)))
	}
	mapHardDone(t, leader, nw, id)
	mapHardAfter(t, c, port, pool, leader, nw, w, id)
	sampler.Stop(t)
}

// mapSampler reads, every 200 ms, whether the instance of the map group on each server is out of recovery and running (a writable primary), asked by the first server
// that can ask all of them (the state calls are the leader's, and the leader moves during the failover of the map group), and records a violation when two servers
// are writable on two consecutive readings that a single asker made.
type mapSampler struct {
	stop chan struct{}
	done chan struct{}
	mu   sync.Mutex
	bad  []string
	n    int
}

func startMapSampler(c *clustertest.Cluster) *mapSampler {
	s := &mapSampler{stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(s.done)
		prev := ""
		for {
			select {
			case <-s.stop:
				return
			case <-time.After(200 * time.Millisecond):
			}
			for _, from := range c.Nodes {
				if !from.Running() {
					continue
				}
				var writable []string
				all := true
				for _, to := range c.Nodes {
					var st failover.State
					code, err := from.InternalCall(to, "GET", "/failover/state?group=0", nil, &st)
					if err != nil || code != 200 {
						all = false
						break
					}
					for _, in := range st.OutOfRecovery() {
						writable = append(writable, fmt.Sprintf("%s/%d", to.Host, in.ID))
					}
				}
				if !all {
					continue
				}
				s.mu.Lock()
				s.n++
				s.mu.Unlock()
				now := fmt.Sprint(writable)
				if len(writable) > 1 && prev == now {
					s.add(fmt.Sprintf("two writable instances of the map group at the same time (asked by %s): %v", from.Host, writable))
				}
				prev = now
				break
			}
		}
	}()
	return s
}

func (s *mapSampler) add(m string) { s.mu.Lock(); s.bad = append(s.bad, m); s.mu.Unlock() }

// Stop ends the sampling and fails the test on any violation. A test that never got a complete reading proves nothing and fails too.
func (s *mapSampler) Stop(t *testing.T) {
	t.Helper()
	close(s.stop)
	<-s.done
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range s.bad {
		t.Errorf("sampler: %s", m)
	}
	if s.n < 5 {
		t.Errorf("sampler: only %d complete readings were made", s.n)
	}
	t.Logf("the sampler made %d complete readings of the map group", s.n)
}

// writeProbe asks the instance of the map group of one server, every 20 ms through its own connection, to assign a transaction id (txid_current fails on a standby and on a
// stopped instance, and succeeds only on a writable primary), and keeps the first and the last instant at which it succeeded.
type writeProbe struct {
	first, last time.Time
	n           int
	// ackLsn is the highest position of a transactional WAL message that the instance acknowledged to the probe (pg_logical_emit_message in its own transaction, which
	// the instance has committed when the statement answers): every commit acknowledged to a client up to it must be in the history of the new primary.
	ackLsn uint64
}

// probeTarget is the instance of the map group on one server: the port of the primary of the map group on the leader's server, and the port of its standby (from the catalog)
// on the standby's server, which is not the port the node's own kingresd opened first (that instance may serve another group).
type probeTarget struct {
	node *clustertest.Node
	port int
}

// mapProbeTargets finds the instances of group 0 on the old leader and on the standby's server.
func mapProbeTargets(t *testing.T, leader, nw *clustertest.Node) []probeTarget {
	t.Helper()
	out := []probeTarget{{leader, leader.PGPort}, {nw, 0}}
	for _, s := range catalogStandbys(t, leader) {
		if s.group == 0 && s.host == nw.Host {
			out[1].port = s.port
		}
	}
	if out[1].port == 0 {
		t.Fatalf("no standby of the map group on %s in the catalog", nw.Host)
	}
	return out
}

func startWriteProbe(t *testing.T, targets []probeTarget) (stop func() []writeProbe) {
	t.Helper()
	nodes := targets
	out := make([]writeProbe, len(nodes))
	quit := make(chan struct{})
	var wg sync.WaitGroup
	var mu sync.Mutex
	for i, n := range nodes {
		// the standby has the roles of the primary: the leader's service password opens both
		pw, err := os.ReadFile(filepath.Join(targets[0].node.DataDir, "service-credentials"))
		if err != nil {
			t.Fatalf("service credentials: %v", err)
		}
		wg.Add(1)
		go func(i int, n probeTarget, pass string) {
			defer wg.Done()
			var c *pgx.Conn
			defer func() {
				if c != nil {
					_ = c.Close(context.Background())
				}
			}()
			for {
				select {
				case <-quit:
					return
				case <-time.After(20 * time.Millisecond):
				}
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				if c == nil {
					var err error
					if c, err = n.node.PGAt(ctx, n.port, "kingresd", pass, "kingres_system"); err != nil {
						c = nil
						cancel()
						continue
					}
				}
				var lsn string
				if err := c.QueryRow(ctx, `SELECT pg_logical_emit_message(true, 'p5probe', 'x')::text`).Scan(&lsn); err != nil {
					if c.IsClosed() {
						c = nil
					}
					cancel()
					continue
				}
				cancel()
				now := time.Now()
				mu.Lock()
				if out[i].n == 0 {
					out[i].first = now
				}
				out[i].last, out[i].n = now, out[i].n+1
				if v, ok := failover.ParseLSN(lsn); ok && v > out[i].ackLsn {
					out[i].ackLsn = v
				}
				mu.Unlock()
			}
		}(i, n, strings.TrimSpace(string(pw)))
	}
	return func() []writeProbe {
		close(quit)
		wg.Wait()
		mu.Lock()
		defer mu.Unlock()
		return append([]writeProbe(nil), out...)
	}
}

// checkProbe asserts what the probe shows of a failover from old to new: the old instance wrote before it (so the probe was live), the new one wrote after, and the old one
// never accepted a write at or after the first write of the new one.
func checkProbe(t *testing.T, ps []writeProbe, old, nw *clustertest.Node) {
	t.Helper()
	o, n := ps[0], ps[1]
	t.Logf("write probe: old %s wrote %d times until %s, new %s wrote %d times from %s", old.Host, o.n, o.last.Format("15:04:05.000"), nw.Host, n.n, n.first.Format("15:04:05.000"))
	if o.n == 0 || n.n == 0 {
		t.Fatalf("the probe saw no write on one of the instances: old %d, new %d", o.n, n.n)
	}
	if !o.last.Before(n.first) {
		t.Errorf("two writable primaries: the old one accepted a write at %s, the new one at %s", o.last.Format("15:04:05.000"), n.first.Format("15:04:05.000"))
	}
}

// checkSwitchPoint asserts that the new timeline began at or after the highest position that the old primary acknowledged to the probe before the fence: the switch point is
// the position in the last timeline history file of the new primary (read as the service role).
func checkSwitchPoint(t *testing.T, ps []writeProbe, leader *clustertest.Node, target probeTarget) {
	t.Helper()
	pw, err := os.ReadFile(filepath.Join(leader.DataDir, "service-credentials"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	c, err := target.node.PGAt(ctx, target.port, "kingresd", strings.TrimSpace(string(pw)), "kingres_system")
	if err != nil {
		t.Fatalf("connecting to the new primary: %v", err)
	}
	defer func() { _ = c.Close(context.Background()) }()
	var name, hist string
	if err := c.QueryRow(ctx, `SELECT name FROM pg_ls_waldir() WHERE name LIKE '%.history' ORDER BY name DESC LIMIT 1`).Scan(&name); err != nil {
		t.Fatalf("the timeline history file of the new primary: %v", err)
	}
	if err := c.QueryRow(ctx, `SELECT pg_read_file('pg_wal/' || $1)`, name).Scan(&hist); err != nil {
		t.Fatalf("reading %s: %v", name, err)
	}
	var switchLsn uint64
	for _, l := range strings.Split(strings.TrimSpace(hist), "\n") {
		if f := strings.Fields(l); len(f) >= 2 {
			if v, ok := failover.ParseLSN(f[1]); ok {
				switchLsn = v
			}
		}
	}
	t.Logf("the old primary acknowledged WAL messages up to %s; the new timeline (%s) began at %s", failover.FormatLSN(ps[0].ackLsn), name, failover.FormatLSN(switchLsn))
	if ps[0].ackLsn == 0 || switchLsn == 0 {
		t.Fatalf("nothing to compare: acknowledged %d, switch point %d (history %q)", ps[0].ackLsn, switchLsn, hist)
	}
	if switchLsn < ps[0].ackLsn {
		t.Errorf("an acknowledged commit is not in the new history: the old primary acknowledged up to %s and the new timeline began at %s", failover.FormatLSN(ps[0].ackLsn), failover.FormatLSN(switchLsn))
	}
}

// P2 (local recovery). The candidate, the only standby of the map group, takes the lease and dies before it promotes: the old leader's primary of the map group is stopped
// and held back for a server that cannot answer, and the cluster has no second copy of the system database that could show that nothing was promoted. Every REST call
// answers 503. The way out is the local command of kingresd on the server of the old leader: it is refused without its two words, for the server it runs on, for another
// server than the one the hold was made for and while kingresd runs on the data directory; with both words and kingresd stopped it ends the hold, the old primary starts
// when its server holds the lease, the cluster serves again with every row the old primary acknowledged, and the release is in the audit log.
func TestADeadOnlyCandidateIsReleasedByTheLocalCommand(t *testing.T) {
	c, port, pool, leader, nw, w := mapSwitchoverEnv(t)
	var third *clustertest.Node
	for _, n := range c.Nodes {
		if n != leader && n != nw {
			third = n
		}
	}
	// the old leader keeps the keys of the administrators on disk, so that it can check the declaration of a lost server while its system database is stopped
	until(t, 2*time.Minute, "the cache of the administrators' keys on the old leader", func() bool {
		b, err := os.ReadFile(filepath.Join(leader.DataDir, "failover", "admin-keys.json"))
		return err == nil && strings.Contains(string(b), clustertest.AdminUser)
	})
	nw.SetFaults(fault.Spec{FailoverPoint: "promote:unavailable", FailoverMode: "unavailable"})
	_ = mapHardRequest(t, leader, nw, true)
	mapStopped(t, leader)
	until(t, 3*time.Minute, "the candidate to hold the lease and wait for its promotion", func() bool {
		s, b := nw.Do("GET", "/failover", nil, nil)
		return s == 200 && b["state"] == "running"
	})
	foKillServer(nw)
	defer func() {
		if t.Failed() {
			t.Logf("leader log:\n%s", quietLog(leader.LogTail(60000)))
			mc, _ := os.ReadFile(filepath.Join(leader.DataDir, "instances", "1", "pgdata", "kingres-managed.conf"))
			pl := leader.InstanceLogs()
			t.Logf("managed settings of the old primary:\n%s\nits log (tail):\n%s", mc, pl[max(0, len(pl)-5000):])
		}
	}()
	// the old leader cannot serve the REST API: its system database is the held-back primary, and the only other copy is on the dead candidate
	var last int
	var lastBody map[string]any
	until(t, 5*time.Minute, "503 from the old leader", func() bool { last, lastBody = leader.Do("GET", "/cluster", nil, nil); return last == 503 })
	t.Logf("%s answers %d %v", leader.Host, last, lastBody)
	st, body := third.Do("GET", "/cluster", nil, nil)
	t.Logf("%s (no copy of the system database) answers %d %v", third.Host, st, body)
	args := func(extra ...string) []string { return append([]string{"-release-map-hold"}, extra...) }
	run := func(extra ...string) (string, error) { return leader.Command(args(extra...)...) }

	// refused: without the words, for the server it runs on, and while kingresd runs on the data directory
	for name, extra := range map[string][]string{
		"no words":         nil,
		"no acceptance":    {"-lost", nw.Host},
		"no declaration":   {"-accept-data-loss"},
		"the local server": {"-lost", leader.Host, "-accept-data-loss"},
		"kingresd running": {"-lost", nw.Host, "-accept-data-loss"},
	} {
		if out, err := run(extra...); err == nil {
			t.Fatalf("%s: the command ran\n%s", name, out)
		} else if name == "kingresd running" && !strings.Contains(out, "stop kingresd") {
			t.Fatalf("%s: the refusal does not say why:\n%s", name, out)
		}
	}
	leader.Stop()
	// the hold was made for the candidate; another server named lost does not release it
	if out, err := run("-lost", third.Host, "-accept-data-loss"); err == nil || !strings.Contains(out, nw.Host) {
		t.Fatalf("the hold was released for a server it was not made for: %v\n%s", err, out)
	}
	out, err := run("-lost", nw.Host, "-accept-data-loss")
	if err != nil {
		t.Fatalf("the command: %v\n%s", err, out)
	}
	t.Logf("the command said: %s", out)
	leader.Start()
	// the sessions of kingresd are in its memory: the operator signs in again once the system database is back
	until(t, 8*time.Minute, "the cluster to serve again through the old leader", func() bool {
		if !leader.Login(clustertest.AdminUser, clustertest.AdminPass) {
			return false
		}
		s, _ := leader.Do("GET", "/cluster", nil, nil)
		return s == 200
	})
	until(t, 3*time.Minute, "writes after the release", func() bool { return w.count() > 0 })
	before := w.count()
	until(t, 5*time.Minute, "new rows after the release", func() bool { return w.count() > before+20 })
	acked := w.Stop()
	foCheckAckedByKey(t, acked, []*clustertest.Node{leader, third}, port, pool)
	// the release is in the audit log, with the lost server in its details
	st, body = leader.Do("GET", "/audit?action=releaseMapGroupHold&limit=50", nil, nil)
	if st != 200 {
		t.Fatalf("the audit log: %d %v", st, body)
	}
	found, refusedRecorded := false, false
	items, _ := body["items"].([]any)
	for _, it := range items {
		ev, _ := it.(map[string]any)
		if ev["action"] == "releaseMapGroupHold" {
			d, _ := ev["details"].(map[string]any)
			if ev["outcome"] == "success" && ev["source"] == "cli" && d["lostServerId"] == nw.Host {
				found = true
			}
			if ev["outcome"] == "failure" && d["lostServerId"] == third.Host {
				refusedRecorded = true
			}
		}
	}
	if !found {
		t.Errorf("the release is not in the audit log: %v", body)
	}
	if !refusedRecorded {
		t.Errorf("the refused release (another server named lost) is not in the audit log: %v", body)
	}
}

// foCheckAckedByKey asks for every acknowledged row (at most 600, evenly spread) by its key through the entry points of the nodes. A third of the table lives in the
// groups of a server that is dead, so a read of all of it cannot complete: a group that is dead answers with an error and the row is not counted; a group that answers
// must have the row (no row is a lost row), and the groups of the nodes asked must answer for a good share of the rows.
func foCheckAckedByKey(t *testing.T, acked map[int64]time.Time, nodes []*clustertest.Node, port int, pool *x509.CertPool) {
	t.Helper()
	var ids []int64
	for rid := range acked {
		ids = append(ids, rid)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	if len(ids) > 600 {
		step := len(ids) / 600
		var picked []int64
		for i := 0; i < len(ids); i += step {
			picked = append(picked, ids[i])
		}
		ids = picked
	}
	for _, n := range nodes {
		// the entry point of a node that was just restarted needs a moment before the first key is answered (the groups that are alive answer for some keys)
		until(t, 3*time.Minute, "a key answered through the entry point of "+n.Host, func() bool {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			c, err := foConn(ctx, n, port, pool, "fo")
			if err != nil {
				return false
			}
			defer func() { _ = c.Close(context.Background()) }()
			for _, rid := range ids {
				var got int64
				if c.QueryRow(ctx, `SELECT id FROM items WHERE id = $1`, rid).Scan(&got) == nil {
					return true
				}
			}
			return false
		})
		found, unreachable := 0, 0
		var conn *pgx.Conn
		for _, rid := range ids {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			if conn == nil {
				c, err := foConn(ctx, n, port, pool, "fo")
				if err != nil {
					cancel()
					unreachable++
					continue
				}
				conn = c
			}
			var got int64
			err := conn.QueryRow(ctx, `SELECT id FROM items WHERE id = $1`, rid).Scan(&got)
			cancel()
			switch {
			case err == nil:
				found++
			case errors.Is(err, pgx.ErrNoRows):
				t.Errorf("acknowledged row %d is missing when read through %s", rid, n.Host)
			default:
				unreachable++
				_ = conn.Close(context.Background())
				conn = nil
			}
		}
		if conn != nil {
			_ = conn.Close(context.Background())
		}
		t.Logf("through %s: %d of %d acknowledged rows found, %d in the groups of the dead server", n.Host, found, len(ids), unreachable)
		if found < len(ids)/6 {
			t.Errorf("through %s only %d of %d acknowledged rows were found: the data of the old primary is not there", n.Host, found, len(ids))
		}
	}
}

// P2 (REST). With a second standby of the map group the operator declares the dead candidate lost through REST, on a third server, and the proof of his session reaches the
// held-back old leader, which checks it against the keys of the administrators that it cached while its system database ran, releases its hold, and serves again. The
// old leader's kingresd runs and its instance is stopped; the session that signs is made after the hold began, so the key of the session comes from a server that can still
// read a verifier. A server that is alive and a server that is no candidate are refused first.
func TestTheRESTDeclarationOfALostServerWorksAcrossRealServers(t *testing.T) {
	c := clustertest.FormWithExtra(t, 3, append([]string{"-lease-duration", "12s", "-lease-renew", "2s", "-lease-margin", "3s"}, standbyArgs...), nil)
	leader := c.Leader()
	if st, body := leader.Do("PATCH", "/settings/cluster", map[string]any{"defaultReplicaCount": 2}, nil); st != 200 && st != 202 {
		t.Fatalf("two standbys for every group: %d %v", st, body)
	}
	waitStandbysFor(t, c, 2, 10*time.Minute)
	views := topology(t, leader)[0]
	var nw, third *clustertest.Node
	for _, n := range c.Nodes {
		switch {
		case n == leader:
		case n.Host == views[0].server:
			nw = n
		default:
			third = n
		}
	}
	if nw == nil || third == nil {
		t.Fatalf("the two standbys of the map group are on %v", views)
	}
	defer func() {
		if t.Failed() {
			t.Logf("old leader log:\n%s\nthird log:\n%s", quietLog(leader.LogTail(40000)), quietLog(third.LogTail(20000)))
		}
	}()
	until(t, 2*time.Minute, "the cache of the administrators' keys on the old leader", func() bool {
		b, err := os.ReadFile(filepath.Join(leader.DataDir, "failover", "admin-keys.json"))
		return err == nil && strings.Contains(string(b), clustertest.AdminUser)
	})
	nw.SetFaults(fault.Spec{FailoverPoint: "promote:unavailable", FailoverMode: "unavailable"})
	_ = mapHardRequest(t, leader, nw, true)
	mapStopped(t, leader)
	until(t, 3*time.Minute, "the candidate to hold the lease and wait for its promotion", func() bool {
		s, b := nw.Do("GET", "/failover", nil, nil)
		return s == 200 && b["state"] == "running"
	})
	foKillServer(nw)
	// the session of the operator is made now, while the old leader's system database is stopped
	until(t, 3*time.Minute, "the operator to sign in on the third server", func() bool { return third.Login(clustertest.AdminUser, clustertest.AdminPass) })
	declare := func(lost string) (int, map[string]any) {
		return third.Do("POST", "/failover/map/abandon", map[string]any{"reason": "the candidate is dead", "lostServerId": lost}, nil)
	}
	// a server that is alive is no lost server
	if st, body := declare(third.Host); st != 409 {
		t.Fatalf("a live server was declared lost: %d %v", st, body)
	}
	var res map[string]any
	until(t, 8*time.Minute, "the declaration to release the old leader", func() bool {
		st, body := declare(nw.Host)
		t.Logf("declaration: %d %v", st, body)
		if st == 200 {
			res = body
			return true
		}
		return false
	})
	if res["resumedServerId"] != leader.Host {
		t.Fatalf("the declaration resumed %v, not the old leader %s", res["resumedServerId"], leader.Host)
	}
	until(t, 8*time.Minute, "the cluster to serve again through the old leader", func() bool {
		return leader.Login(clustertest.AdminUser, clustertest.AdminPass) && func() bool { s, _ := leader.Do("GET", "/cluster", nil, nil); return s == 200 }()
	})
	// a rerun after the release says that nothing is held back: an error that says so, not an empty success
	if st, body := declare(nw.Host); st == 200 {
		t.Errorf("a second declaration answered a success: %v", body)
	}
}
