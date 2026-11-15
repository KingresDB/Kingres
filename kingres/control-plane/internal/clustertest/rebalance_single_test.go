// SPDX-License-Identifier: AGPL-3.0-only

package clustertest_test

import (
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/clustertest"
)

// A one-server cluster has nothing to rebalance: the operations answer, calmly and with their codes.
func TestRebalanceOneServer(t *testing.T) {
	c := clustertest.New(t, 1)
	n := c.Leader()
	n.Start()
	n.InitCluster("one")
	body := n.Expect(202, "POST", "/databases", map[string]any{"name": "solo", "sharding": map[string]any{"enabled": true, "bucketCount": 8}})
	n.WaitTaskDone(clustertest.TaskID(body), 4*time.Minute)
	if st := n.Expect(200, "GET", "/rebalance", nil); st["state"] != "idle" {
		t.Errorf("status: %v", st)
	}
	if plan := n.Expect(200, "POST", "/rebalance/plan", nil); plan["needed"] != false {
		t.Errorf("plan: %v", plan)
	}
	st, p := n.Do("POST", "/rebalance", nil, nil)
	if st != 409 || p["code"] != "nothing_to_rebalance" {
		t.Errorf("start: %d %v", st, p)
	}
	st, p = n.Do("POST", "/rebalance/stop", nil, nil)
	if st != 409 || p["code"] != "rebalance_not_running" {
		t.Errorf("stop: %d %v", st, p)
	}
}
