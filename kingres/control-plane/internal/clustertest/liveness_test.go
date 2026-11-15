// SPDX-License-Identifier: AGPL-3.0-only

package clustertest_test

import (
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/clustertest"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/fault"
)

// Short bounds so that the test does not wait for the production defaults. The
// bounds the assertions use are generous multiples of them: the machine is shared
// and loaded, so every wait polls for the condition and never sleeps for a time.
var lvFlags = []string{"-heartbeat-interval", "500ms", "-suspect-timeout", "3s", "-failure-timeout", "6s", "-leader-loss-bound", "12s"}

const lvWithin = 60 * time.Second

// lvStatus is the status node n shows for server id ("" when the call fails).
func lvStatus(n *clustertest.Node, id string) string {
	st, body := n.Do("GET", "/servers", nil, nil)
	if st != 200 {
		return ""
	}
	items, _ := body["items"].([]any)
	for _, it := range items {
		m, _ := it.(map[string]any)
		if m["id"] == id {
			s, _ := m["status"].(string)
			return s
		}
	}
	return ""
}

// lvWait polls until pred holds for the status node n shows for server id, and
// returns how long it took.
func lvWait(t *testing.T, n *clustertest.Node, id string, pred func(string) bool, what string) time.Duration {
	t.Helper()
	start := time.Now()
	for time.Since(start) < lvWithin {
		if pred(lvStatus(n, id)) {
			return time.Since(start)
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("node %d never showed %s as %s (last: %q)\n%s", n.Index, id, what, lvStatus(n, id), n.LogTail(2000))
	return 0
}

func lvIs(want string) func(string) bool { return func(s string) bool { return s == want } }

func lvIn(want ...string) func(string) bool {
	return func(s string) bool {
		for _, w := range want {
			if s == w {
				return true
			}
		}
		return false
	}
}

// lvCounts reads the counts of the cluster overview and the statuses of the list
// of the same node and says whether they agree.
func lvCounts(t *testing.T, n *clustertest.Node) (counts map[string]int, list map[string]int) {
	t.Helper()
	counts, list = map[string]int{}, map[string]int{}
	st, body := n.Do("GET", "/cluster", nil, nil)
	if st != 200 {
		t.Fatalf("node %d: GET /cluster: %d %v", n.Index, st, body)
	}
	sv, _ := body["servers"].(map[string]any)
	for k, v := range sv {
		f, _ := v.(float64)
		counts[k] = int(f)
	}
	for _, s := range n.Servers() {
		list["total"]++
		status, _ := s["status"].(string)
		list[status]++
	}
	return counts, list
}

func lvOpenAlert(n *clustertest.Node, kind, server string) bool {
	st, body := n.Do("GET", "/alerts?state=open&limit=200", nil, nil)
	if st != 200 {
		return false
	}
	items, _ := body["items"].([]any)
	for _, it := range items {
		m, _ := it.(map[string]any)
		if m["kind"] == kind && m["serverId"] == server {
			return true
		}
	}
	return false
}

func lvPoll(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(lvWithin); time.Now().Before(deadline); time.Sleep(250 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

// TestHeartbeatsAndLiveness runs one three-server cluster through the liveness
// behavior: a killed server is dead on every server and returns, a one-way cut
// changes nothing, a partition shows both sides dead to each other, a paused
// process does not declare the others dead when it wakes, a step of the wall
// clock changes nothing, and a follower that lost the leader for longer than the
// bound stops authorizing sessions (and not before).
func TestHeartbeatsAndLiveness(t *testing.T) {
	c := clustertest.FormWithExtra(t, 3, lvFlags, nil)
	n0, n1, n2 := c.Nodes[0], c.Nodes[1], c.Nodes[2]
	healthy := lvIn("live", "pendingRebalance")

	t.Run("everyone sees everyone alive with a heartbeat time", func(t *testing.T) {
		for _, n := range c.Nodes {
			for _, o := range c.Nodes {
				lvWait(t, n, o.Host, healthy, "live")
			}
			// A server learns of the third one from the directory the leader sends and hears it a
			// heartbeat later: wait for the time rather than read it at once.
			lvPoll(t, "a heartbeat time for every other server", func() bool {
				for id, s := range n.Servers() {
					if id != n.Host && s["lastHeartbeatAt"] == nil {
						return false
					}
				}
				return true
			})
			counts, list := lvCounts(t, n)
			if counts["total"] != 3 || counts["dead"] != 0 || counts["suspect"] != 0 || counts["live"]+counts["pendingRebalance"] != 3 {
				t.Errorf("node %d: overview counts %v", n.Index, counts)
			}
			for _, k := range []string{"total", "live", "suspect", "dead", "pendingRebalance"} {
				if counts[k] != list[k] {
					t.Errorf("node %d: overview says %d %s, the list %d", n.Index, counts[k], k, list[k])
				}
			}
		}
	})

	t.Run("a killed server is dead on every server and returns", func(t *testing.T) {
		before := lvStatus(n0, n2.Host)
		n2.Kill()
		var took [2]time.Duration
		for i, n := range []*clustertest.Node{n0, n1} {
			took[i] = lvWait(t, n, n2.Host, lvIs("dead"), "dead")
		}
		t.Logf("dead after %s on node 0 and %s on node 1 (failure timeout 6s)", took[0], took[1])
		for _, n := range []*clustertest.Node{n0, n1} {
			counts, list := lvCounts(t, n)
			if counts["dead"] != 1 || counts["dead"] != list["dead"] || counts["total"] != 3 {
				t.Errorf("node %d: counts %v, list %v", n.Index, counts, list)
			}
			if h := counts["live"] + counts["pendingRebalance"]; h != 2 {
				t.Errorf("node %d: %d healthy servers, want 2: %v", n.Index, h, counts)
			}
		}
		lvPoll(t, "the serverDown alert", func() bool { return lvOpenAlert(n0, "serverDown", n2.Host) && lvOpenAlert(n1, "serverDown", n2.Host) })
		n2.Start()
		if !n2.Login(clustertest.AdminUser, clustertest.AdminPass) {
			t.Fatalf("cannot sign in on node 2 after the restart\n%s", c.Dump())
		}
		for _, n := range []*clustertest.Node{n0, n1} {
			lvWait(t, n, n2.Host, lvIs(before), "its previous status "+before)
		}
		lvPoll(t, "the alert to clear", func() bool { return !lvOpenAlert(n0, "serverDown", n2.Host) && !lvOpenAlert(n1, "serverDown", n2.Host) })
		lvWait(t, n2, n0.Host, healthy, "live")
	})

	t.Run("a one-way cut changes nothing", func(t *testing.T) {
		n1.SetFaults(fault.Spec{BlockOutbound: []string{n0.Host}})
		defer n1.Heal()
		// Twice the failure timeout: node 1 cannot call node 0, but node 0 calls node 1 and the
		// answers show node 1 alive to both of them.
		end := time.Now().Add(14 * time.Second)
		for time.Now().Before(end) {
			for _, p := range [][2]*clustertest.Node{{n0, n1}, {n1, n0}} {
				if s := lvStatus(p[0], p[1].Host); !healthy(s) {
					t.Fatalf("node %d shows node %d as %q during a one-way cut", p[0].Index, p[1].Index, s)
				}
			}
			time.Sleep(time.Second)
		}
	})

	t.Run("a partition shows both sides dead to each other and heals", func(t *testing.T) {
		// The isolated side cannot be asked for its view here: after the leader-loss bound it refuses
		// sessions (next subtest), so only the majority side is read.
		c.Partition(n2)
		defer c.HealAll()
		lvWait(t, n0, n2.Host, lvIs("dead"), "dead")
		lvWait(t, n1, n2.Host, lvIs("dead"), "dead")
		if s := lvStatus(n0, n1.Host); !healthy(s) {
			t.Errorf("the majority side must stay healthy to itself, node 0 shows node 1 as %q", s)
		}
		c.HealAll()
		for _, n := range c.Nodes {
			for _, o := range c.Nodes {
				lvWait(t, n, o.Host, healthy, "healthy again")
			}
		}
	})

	t.Run("a short blip does not stop a follower, a long loss does", func(t *testing.T) {
		get := func(n *clustertest.Node) (int, string) {
			st, body := n.Do("GET", "/servers", nil, nil)
			code, _ := body["code"].(string)
			return st, code
		}
		// Shorter than the bound (12 s): every read on the isolated follower still works.
		c.Partition(n2)
		defer c.HealAll()
		for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(500 * time.Millisecond) {
			if st, code := get(n2); st != 200 {
				t.Fatalf("a read on the follower failed after %s without the leader: %d %s", 5*time.Second, st, code)
			}
		}
		c.HealAll()
		lvWait(t, n2, n0.Host, healthy, "healthy again")
		// Longer than the bound: the follower no longer authorizes sessions; the others do.
		c.Partition(n2)
		start := time.Now()
		lvPoll(t, "the isolated follower to refuse sessions", func() bool { st, code := get(n2); return st == 503 && code == "leader_unavailable" })
		took := time.Since(start)
		t.Logf("the follower refused sessions after %s (bound 12s)", took)
		if took < 8*time.Second {
			t.Errorf("the follower refused after only %s, well before the bound of 12s", took)
		}
		if st, _ := get(n1); st != 200 {
			t.Errorf("the follower that can reach the leader answered %d", st)
		}
		if st, _ := get(n0); st != 200 {
			t.Errorf("the leader answered %d", st)
		}
		c.HealAll()
		lvPoll(t, "the follower to serve again", func() bool { st, _ := get(n2); return st == 200 })
	})

	t.Run("a paused process does not declare the others dead when it wakes", func(t *testing.T) {
		n1.Pause()
		lvWait(t, n0, n1.Host, lvIs("dead"), "dead")
		n1.Resume()
		// Right after the pause node 1 has not heard from anybody for longer than the failure
		// timeout by its own count; it must not conclude that the others died.
		end := time.Now().Add(6 * time.Second)
		for time.Now().Before(end) {
			for _, o := range []*clustertest.Node{n0, n2} {
				if s := lvStatus(n1, o.Host); s == "dead" {
					t.Fatalf("node 1 declared node %d dead after its own pause", o.Index)
				}
			}
			time.Sleep(200 * time.Millisecond)
		}
		lvWait(t, n0, n1.Host, healthy, "healthy again")
	})

	t.Run("a step of the wall clock changes nothing", func(t *testing.T) {
		n1.SetFaults(fault.Spec{ClockOffsetMs: int64(time.Hour / time.Millisecond)})
		defer n1.Heal()
		end := time.Now().Add(14 * time.Second)
		for time.Now().Before(end) {
			for _, o := range []*clustertest.Node{n0, n2} {
				if s := lvStatus(n1, o.Host); !healthy(s) {
					t.Fatalf("node 1 shows node %d as %q after its clock stepped", o.Index, s)
				}
			}
			if s := lvStatus(n0, n1.Host); !healthy(s) {
				t.Fatalf("node 0 shows node 1 as %q after its clock stepped", s)
			}
			time.Sleep(time.Second)
		}
		_, list := lvCounts(t, n1)
		if list["dead"]+list["suspect"] != 0 {
			t.Errorf("statuses after the step: %v", list)
		}
	})
}
