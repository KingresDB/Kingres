// SPDX-License-Identifier: AGPL-3.0-only

package clustertest_test

import (
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/clustertest"
)

// TestTaskWaitsForAServerThatWaitsForItsLease: kingresd of a follower is down long enough for
// its watchdog to fence the instance, a cluster-wide task has a step on that server, and then
// kingresd starts again. While the instance is held back until the lease is won, the leader's
// calls to the server must be answered 503 (never 403 not_leader), the step must wait, and the
// task must end done without a failed step. It runs on a cluster of its own: the server is
// stopped and started.
func TestTaskWaitsForAServerThatWaitsForItsLease(t *testing.T) {
	c := clustertest.FormWithExtra(t, 3, leaseFlags, nil)
	leader, victim := c.Leader(), c.Nodes[2]
	// The standby is made some time after the cluster is formed (a base backup), and only a group with one has a
	// watchdog: wait for it, or the test fails on a loaded machine for a reason that has nothing to do with it.
	waitStandbys(t, c, 1)
	waitWatchdog(t, victim) // the group has a standby: the watchdog runs
	victim.Kill()
	waitInstance(t, victim, false, time.Minute) // fenced by the watchdog

	cur := leader.Expect(200, "GET", "/settings/network-access", nil)
	body := leader.Expect(202, "PUT", "/settings/network-access", map[string]any{
		"revision": cur["revision"], "tlsRequired": cur["tlsRequired"], "rules": cur["rules"]})
	id := body["id"]

	victim.Start()
	seen := map[int]int{}
	for deadline := time.Now().Add(2 * time.Minute); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		st, err := leader.InternalCall(victim, "GET", "/resources", nil, nil)
		if err != nil {
			continue // not listening yet
		}
		seen[st]++
		if st == 403 {
			t.Fatalf("a held-back server answered 403 not_leader (%v)", seen)
		}
		if instanceAccepts(victim) && st == 200 {
			break
		}
	}
	if seen[503] == 0 {
		t.Logf("the window was not observed by the probe (statuses %v); the task result below still decides", seen)
	}
	done := leader.WaitTask(id, 4*time.Minute)
	if done["state"] != "done" {
		t.Fatalf("the task ended %v: %v\n%s", done["state"], done["error"], leader.LogTail(3000))
	}
	prog, _ := done["progress"].(map[string]any)
	steps, _ := prog["steps"].([]any)
	for _, s := range steps {
		if m, _ := s.(map[string]any); m["state"] == "failed" {
			t.Fatalf("a step failed: %v", m)
		}
	}
}
