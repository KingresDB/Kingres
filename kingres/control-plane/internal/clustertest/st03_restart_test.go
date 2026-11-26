// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package clustertest_test

import (
	"context"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/clustertest"
)

func startTime(t *testing.T, n *clustertest.Node) time.Time {
	t.Helper()
	var at time.Time
	if err := n.ServicePG("postgres").QueryRow(context.Background(), "SELECT pg_postmaster_start_time()").Scan(&at); err != nil {
		t.Fatalf("node %d: %v", n.Index, err)
	}
	return at
}

// ST-03: a restart that a task step repeats (the answer was lost) is a no-op when nothing waits for a restart:
// with ifPending the instance is restarted only when a setting is pending, and not twice.
func TestARepeatedRestartCallRestartsOnlyWhatIsPending(t *testing.T) {
	c := sharedCluster(t)
	leader, n2 := c.Nodes[0], c.Nodes[2]
	before := show(t, n2, "postgres", "shared_buffers")
	restart := func() {
		t.Helper()
		callOK(t, leader, n2, "POST", "/config/restart", map[string]any{"instance": 1, "ifPending": true}, 200, nil)
	}

	// nothing pending: no restart
	t0 := startTime(t, n2)
	restart()
	if got := startTime(t, n2); !got.Equal(t0) {
		t.Fatalf("an instance with nothing pending was restarted (%s, then %s)", t0, got)
	}

	// a setting pending: restarted once; the same call again does nothing
	cfgApplyDone(t, leader, cfgChange("shared_buffers", "server", n2.Host, "320MB"))
	until(t, time.Minute, "the setting to be pending", func() bool {
		return cfgServer(t, cfgParam(t, leader, "shared_buffers"), n2.Host)["state"] == "pendingRestart"
	})
	restart()
	t1 := startTime(t, n2)
	if !t1.After(t0) || show(t, n2, "postgres", "shared_buffers") != "320MB" {
		t.Fatalf("a pending setting did not restart the instance (%s, then %s)", t0, t1)
	}
	restart()
	if got := startTime(t, n2); !got.Equal(t1) {
		t.Fatalf("the repeated call restarted the instance again (%s, then %s)", t1, got)
	}

	// put the cluster back as it was found
	cfgApplyDone(t, leader, cfgChange("shared_buffers", "server", n2.Host, nil))
	body := leader.Expect(202, "POST", "/config/restarts", map[string]any{"acknowledgeDowntime": true})
	if done := leader.WaitTask(clustertest.TaskID(body), 8*time.Minute); done["state"] != "done" {
		t.Fatalf("restart task: %v %v", done["state"], done["error"])
	}
	until(t, 2*time.Minute, "the old shared_buffers", func() bool { return show(t, n2, "postgres", "shared_buffers") == before })
}
