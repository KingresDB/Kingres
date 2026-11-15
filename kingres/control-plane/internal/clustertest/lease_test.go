// SPDX-License-Identifier: PostgreSQL

package clustertest_test

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/clustertest"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/fault"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ha"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/lease"
)

// leaseFlags makes the lease protocol fast enough for a test: a lease of six
// seconds, renewed every second, with a margin of two (so a holder fences at
// +4 s and a grantor frees the lease at +8 s after the last renewal).
var leaseFlags = []string{"-lease-duration", "6s", "-lease-renew", "1s", "-lease-margin", "2s", "-monitor-interval", "2s"}

// leaseCluster forms three servers; enforce makes every group count as having a
// standby (the fault hook stand-in until standbys exist). It returns the cluster
// and the shard group of each node.
func leaseCluster(t *testing.T, enforce bool, extra ...string) (*clustertest.Cluster, []int32) {
	t.Helper()
	c := clustertest.FormWithExtra(t, 3, append(append([]string(nil), leaseFlags...), extra...), nil)
	for _, n := range c.Nodes {
		n.ForceEnforce = enforce
		n.Heal() // writes the fault file with the enforcement flag
	}
	groups := make([]int32, len(c.Nodes))
	for i, n := range c.Nodes {
		pg := n.ServicePG("kingres_system")
		var g int32
		if err := pg.QueryRow(context.Background(), `SELECT group_id FROM kingres.node WHERE host = $1 AND role = 'primary'`, n.Host).Scan(&g); err != nil {
			t.Fatalf("group of node %d: %v", i, err)
		}
		groups[i] = g
		mustExec(t, n.ServicePG("postgres"), `CREATE TABLE IF NOT EXISTS lease_probe (n bigserial PRIMARY KEY, at timestamptz DEFAULT now())`)
	}
	for i, n := range c.Nodes {
		waitLease(t, c.Nodes[(i+1)%3], n, groups[i], "held", time.Minute)
	}
	return c, groups
}

func mustExec(t *testing.T, c *pgx.Conn, sql string) {
	t.Helper()
	if _, err := c.Exec(context.Background(), sql); err != nil {
		t.Fatal(err)
	}
}

// leaseState reads GET /lease/state of node as the server `as`.
func leaseState(as, node *clustertest.Node) (struct {
	Server string         `json:"server"`
	Grants []lease.Entry  `json:"grants"`
	Leases []ha.LeaseInfo `json:"leases"`
}, error) {
	var out struct {
		Server string         `json:"server"`
		Grants []lease.Entry  `json:"grants"`
		Leases []ha.LeaseInfo `json:"leases"`
	}
	st, err := as.InternalCall(node, "GET", "/lease/state", nil, &out)
	if err == nil && st != 200 {
		err = fmt.Errorf("status %d", st)
	}
	return out, err
}

// waitLease waits until node reports the state for its own group.
func waitLease(t *testing.T, as, node *clustertest.Node, group int32, state string, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	var last string
	for time.Now().Before(deadline) {
		s, err := leaseState(as, node)
		if err == nil {
			for _, l := range s.Leases {
				if l.Group == group && l.Holder == node.Host {
					last = l.State
					if l.State == state {
						return
					}
				}
			}
		} else {
			last = err.Error()
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("node %d never reached lease state %q for group %d (last %q)", node.Index, state, group, last)
}

// freeAfter polls the grantors of the majority side (asked with the certificate of
// prober, acting as a would-be new holder) until every one of them would grant the
// group to another holder, and returns that instant. This is the earliest moment a
// new holder could obtain a majority.
func freeAfter(t *testing.T, prober *clustertest.Node, grantors []*clustertest.Node, group int32, d time.Duration) time.Time {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		all := true
		for _, g := range grantors {
			var r lease.Reply
			st, err := prober.InternalCall(g, "POST", "/lease/grant", lease.Request{Group: group, DurationMs: 1000, DryRun: true}, &r)
			if err != nil || st != 200 || !r.Granted {
				all = false
				break
			}
		}
		if all {
			return time.Now()
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("the lease of group %d never became free on the majority side", group)
	return time.Time{}
}

// writer is a client that keeps inserting into one node's instance and records
// the last acknowledged write.
type writer struct {
	n    *clustertest.Node
	stop chan struct{}
	done chan struct{}

	mu      sync.Mutex
	lastAck time.Time
	acks    int
}

func startWriter(t *testing.T, n *clustertest.Node) *writer {
	t.Helper()
	pw, err := os.ReadFile(filepath.Join(n.DataDir, "service-credentials"))
	if err != nil {
		t.Fatal(err)
	}
	w := &writer{n: n, stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(w.done)
		var conn *pgx.Conn
		for {
			select {
			case <-w.stop:
				if conn != nil {
					_ = conn.Close(context.Background())
				}
				return
			default:
			}
			if conn == nil {
				cfg, _ := pgx.ParseConfig("")
				cfg.Host, cfg.Port, cfg.Database, cfg.User, cfg.Password = "127.0.0.1", uint16(n.PGPort), "postgres", "kingresd", strings.TrimSpace(string(pw)) //nolint:gosec // test port
				cfg.TLSConfig, cfg.Fallbacks, cfg.ConnectTimeout = nil, nil, 2*time.Second
				c, err := pgx.ConnectConfig(context.Background(), cfg)
				if err != nil {
					time.Sleep(100 * time.Millisecond)
					continue
				}
				conn = c
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			_, err := conn.Exec(ctx, `INSERT INTO lease_probe DEFAULT VALUES`)
			cancel()
			if err != nil {
				_ = conn.Close(context.Background())
				conn = nil
				time.Sleep(100 * time.Millisecond)
				continue
			}
			w.mu.Lock()
			w.lastAck, w.acks = time.Now(), w.acks+1
			w.mu.Unlock()
			time.Sleep(20 * time.Millisecond)
		}
	}()
	t.Cleanup(w.Stop)
	return w
}

func (w *writer) Stop() {
	select {
	case <-w.stop:
	default:
		close(w.stop)
	}
	<-w.done
}

func (w *writer) last() (time.Time, int) { w.mu.Lock(); defer w.mu.Unlock(); return w.lastAck, w.acks }

// waitAcks waits until more than n writes were acknowledged.
func (w *writer) waitAcks(t *testing.T, n int, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if _, a := w.last(); a > n {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("the writer on node %d got no acknowledgement", w.n.Index)
}

func instanceAccepts(n *clustertest.Node) bool {
	c, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(n.PGPort), time.Second)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

func waitInstance(t *testing.T, n *clustertest.Node, up bool, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if instanceAccepts(n) == up {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("the instance of node %d did not become up=%v\n%s", n.Index, up, leaseDiag(n))
}

// A primary cut off from the majority, with a standby (enforcement on), stops
// accepting writes before a new holder could be granted the lease: the two
// timelines are measured and compared.
func TestPartitionedPrimaryWithStandbyStopsBeforeANewHolderCouldBeGranted(t *testing.T) {
	c, groups := leaseCluster(t, true)
	victim, other := c.Nodes[1], c.Nodes[2]
	w := startWriter(t, victim)
	w.waitAcks(t, 5, time.Minute)

	victim.Isolate()
	cutAt := time.Now()
	freeAt := freeAfter(t, other, []*clustertest.Node{c.Nodes[0], other}, groups[1], time.Minute)
	waitInstance(t, victim, false, 30*time.Second)
	lastAck, _ := w.last()
	t.Logf("cut at +0, last acknowledged write +%v, lease free for a new holder +%v", lastAck.Sub(cutAt).Round(time.Millisecond), freeAt.Sub(cutAt).Round(time.Millisecond))
	if !lastAck.Before(freeAt) {
		t.Fatalf("the primary acknowledged a write (%v after the cut) after the lease could be granted to another holder (%v)", lastAck.Sub(cutAt), freeAt.Sub(cutAt))
	}
	if gap := freeAt.Sub(lastAck); gap < time.Second {
		t.Fatalf("only %v between the last acknowledged write and the earliest new grant", gap)
	}
	// The minority never regains the lease while cut off: the instance stays down.
	time.Sleep(8 * time.Second)
	if instanceAccepts(victim) {
		t.Fatal("the fenced primary came back without a lease")
	}
	if _, err := os.Stat(filepath.Join(victim.DataDir, "lease", lease.FencedMarker)); err != nil {
		t.Fatalf("no fence marker: %v", err)
	}
	// Healed: the lease is obtained again and the primary starts; writes resume.
	_, before := w.last()
	victim.Heal()
	waitInstance(t, victim, true, 90*time.Second)
	w.waitAcks(t, before, time.Minute)
}

// A group without a standby keeps running when it loses the lease, and says so with
// the alert leaseLost; the lease is never held on the minority side.
func TestPartitionedPrimaryWithoutStandbyKeepsRunningAndRaisesLeaseLost(t *testing.T) {
	c, groups := leaseCluster(t, false)
	victim, other := c.Nodes[1], c.Nodes[2]
	w := startWriter(t, victim)
	w.waitAcks(t, 5, time.Minute)

	victim.Isolate()
	freeAt := freeAfter(t, other, []*clustertest.Node{c.Nodes[0], other}, groups[1], time.Minute)
	_, afterFree := w.last()
	w.waitAcks(t, afterFree+5, time.Minute)
	if last, _ := w.last(); !last.After(freeAt) {
		t.Fatal("the primary without a standby stopped accepting writes")
	}
	// The minority never holds the lease: the answering server's own view says lost.
	deadline := time.Now().Add(time.Minute)
	for {
		g := replicationGroup(t, victim, groups[1])
		if lease, _ := g["lease"].(map[string]any); lease != nil && lease["state"] == "lost" && lease["enforced"] == false {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the replication view never showed the lost lease: %v", g["lease"])
		}
		time.Sleep(500 * time.Millisecond)
	}
	waitAlert(t, victim, "leaseLost", true, time.Minute)
	victim.Heal()
	waitAlert(t, victim, "leaseLost", false, time.Minute)
}

func replicationGroup(t *testing.T, n *clustertest.Node, group int32) map[string]any {
	t.Helper()
	body := n.Expect(200, "GET", "/replication/topology", nil)
	groups, _ := body["groups"].([]any)
	for _, g := range groups {
		m, _ := g.(map[string]any)
		if int32(m["id"].(float64)) == group {
			return m
		}
	}
	t.Fatalf("group %d not in the replication view", group)
	return nil
}

// waitAlert waits until an alert of the kind is open (or none is).
func waitAlert(t *testing.T, n *clustertest.Node, kind string, open bool, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		_, body := n.Do("GET", "/alerts?limit=200", nil, nil)
		found := false
		items, _ := body["items"].([]any)
		for _, it := range items {
			m, _ := it.(map[string]any)
			if m["kind"] == kind && m["state"] != "resolved" {
				found = true
			}
		}
		if found == open {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("alert %s did not become open=%v on node %d", kind, open, n.Index)
}

// kingresd killed while its primary keeps running: the watchdog stops the instance
// when the group has a standby, and the instance does not start without a lease.
func TestWatchdogStopsThePrimaryWhenKingresdIsDead(t *testing.T) {
	c, groups := leaseCluster(t, true)
	victim, other := c.Nodes[1], c.Nodes[2]
	waitWatchdog(t, victim)
	victim.Kill()
	freeAt := freeAfter(t, other, []*clustertest.Node{c.Nodes[0], other}, groups[1], time.Minute)
	waitInstance(t, victim, false, 30*time.Second)
	stoppedBefore := !instanceAccepts(victim)
	if !stoppedBefore {
		t.Fatal("the instance runs although kingresd is dead and the lease is over")
	}
	_ = freeAt
	if _, err := os.Stat(filepath.Join(victim.DataDir, "lease", lease.FencedMarker)); err != nil {
		t.Fatalf("the watchdog left no marker: %v", err)
	}
	// kingresd comes back: it must not start the primary until it holds a lease, and it
	// starts it as soon as it does.
	victim.Start()
	waitInstance(t, victim, true, 2*time.Minute)
	waitLease(t, other, victim, groups[1], "held", time.Minute)
}

func waitFile(t *testing.T, p string) {
	t.Helper()
	deadline := time.Now().Add(time.Minute)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(p); err == nil {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("%s did not appear", p)
}

// kingresd paused (SIGSTOP) for longer than the lease and then resumed never ends
// with a primary that acknowledges writes after the lease could have been granted
// elsewhere, and does not hold a lease it lost: it gets a fresh one.
func TestPausedKingresdNeverActsOnALeaseItLost(t *testing.T) {
	c, groups := leaseCluster(t, true)
	victim, other := c.Nodes[1], c.Nodes[2]
	waitWatchdog(t, victim)
	w := startWriter(t, victim)
	w.waitAcks(t, 5, time.Minute)
	pausedAt := time.Now()
	victim.Pause()
	resumed := false
	defer func() {
		if !resumed {
			victim.Resume()
		}
	}()
	freeAt := freeAfter(t, other, []*clustertest.Node{c.Nodes[0], other}, groups[1], time.Minute)
	waitInstance(t, victim, false, 30*time.Second) // stopped by the watchdog, kingresd frozen
	lastAck, _ := w.last()
	if !lastAck.Before(freeAt) {
		t.Fatalf("a write was acknowledged %v after the pause, after the lease could be granted elsewhere (%v)", lastAck.Sub(pausedAt), freeAt.Sub(pausedAt))
	}
	time.Sleep(3 * time.Second) // a pause well beyond the lease
	victim.Resume()
	resumed = true
	// A fresh lease, then the primary starts again and takes writes.
	waitInstance(t, victim, true, 2*time.Minute)
	waitLease(t, other, victim, groups[1], "held", time.Minute)
	_, before := w.last()
	w.waitAcks(t, before, time.Minute)
}

// A grantor that restarts inside the lifetime of a lease does not grant the group to
// a second holder: it keeps the lease for the full duration plus the margin.
func TestGrantorRestartedInsideALeaseDoesNotGrantToASecondHolder(t *testing.T) {
	c, groups := leaseCluster(t, false, "-lease-duration", "20s", "-lease-renew", "2s", "-lease-margin", "3s")
	holder, grantor, prober := c.Nodes[1], c.Nodes[0], c.Nodes[2]
	// The holder stops renewing: frozen, its lease runs out only after the full duration.
	holder.Pause()
	defer holder.Resume()
	grantor.Kill()
	grantor.Start()
	var r lease.Reply
	deadline := time.Now().Add(time.Minute)
	for {
		st, err := prober.InternalCall(grantor, "POST", "/lease/grant", lease.Request{Group: groups[1], DurationMs: 1000, DryRun: true}, &r)
		if err == nil && st == 200 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the restarted grantor did not answer")
		}
		time.Sleep(100 * time.Millisecond)
	}
	if r.Granted || r.Holder != holder.Host {
		t.Fatalf("the restarted grantor would grant the lease of %s to a second holder: %+v", holder.Host, r)
	}
	if got, err := os.ReadFile(filepath.Join(grantor.DataDir, "lease", lease.GrantsFile)); err != nil || !strings.Contains(string(got), holder.Host) {
		t.Fatalf("the grant is not on disk: %v %s", err, got)
	}
}

// A step of the wall clock on one server changes nothing: the lease is held without
// a break, the primary keeps taking writes, and the grantors still show the holder.
func TestWallClockStepChangesNothingForLeases(t *testing.T) {
	c, groups := leaseCluster(t, true)
	victim, other := c.Nodes[1], c.Nodes[2]
	w := startWriter(t, victim)
	w.waitAcks(t, 5, time.Minute)
	steps := []int64{3600_000, -3600_000, 86400_000, 0}
	for _, off := range steps {
		victim.SetFaults(fault.Spec{ClockOffsetMs: off})
		_, before := w.last()
		deadline := time.Now().Add(9 * time.Second) // longer than a lease
		for time.Now().Before(deadline) {
			s, err := leaseState(other, victim)
			if err != nil {
				t.Fatalf("offset %d ms: %v", off, err)
			}
			held := false
			for _, l := range s.Leases {
				if l.Group == groups[1] && l.State == "held" && l.Holder == victim.Host {
					held = true
				}
			}
			if !held {
				t.Fatalf("offset %d ms: the lease was not held: %+v", off, s.Leases)
			}
			time.Sleep(300 * time.Millisecond)
		}
		w.waitAcks(t, before, 10*time.Second)
	}
	if !instanceAccepts(victim) {
		t.Fatal("the primary was stopped")
	}
	// The grantors saw the holder renew throughout.
	g, err := leaseState(other, c.Nodes[0])
	if err != nil {
		t.Fatal(err)
	}
	var seen bool
	for _, e := range g.Grants {
		if e.Group == groups[1] && e.Holder == victim.Host && e.RemainingMs > 0 {
			seen = true
		}
	}
	if !seen {
		t.Fatalf("the grantor lost the holder: %+v", g.Grants)
	}
}

// leaseDiag describes the lease files and the watchdog of a node, for failures.
func leaseDiag(n *clustertest.Node) string {
	var sb strings.Builder
	for _, f := range []string{"lease/watchdog.log", "lease/fenced", "lease/grants.json", "instances/1/lease.json"} {
		b, err := os.ReadFile(filepath.Join(n.DataDir, f))
		fmt.Fprintf(&sb, "-- %s (err %v): %s\n", f, err, strings.TrimSpace(string(b)))
	}
	out, _ := exec.Command("pgrep", "-af", "kingres-watchdog").CombinedOutput()
	fmt.Fprintf(&sb, "-- watchdog processes: %s\n", out)
	sb.WriteString(n.LogTail(2500))
	return sb.String()
}

// waitWatchdog waits until the lease watchdog of the node runs (kingresd starts it
// at the first renewal after the group counts as enforced; a test that freezes or
// kills kingresd must not do it earlier).
func waitWatchdog(t *testing.T, n *clustertest.Node) {
	t.Helper()
	waitFile(t, filepath.Join(n.DataDir, "instances", "1", "lease.json"))
	deadline := time.Now().Add(time.Minute)
	for time.Now().Before(deadline) {
		out, _ := exec.Command("pgrep", "-f", "kingres-watchdog.*"+filepath.Join(n.DataDir, "instances", "1", "pgdata")).Output()
		if strings.TrimSpace(string(out)) != "" {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("the watchdog of node %d does not run\n%s", n.Index, leaseDiag(n))
}
