// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package clustertest_test

import (
	"net"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/clustertest"
)

// instanceListens tells whether the PostgreSQL instance of the node accepts a connection on its port.
func instanceListens(n *clustertest.Node) bool {
	c, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(n.PGPort)), time.Second)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

// sharedHealth: the shared cluster is used by tests that run an hour apart (other clusters are made and torn down in between). When an instance of
// the shared cluster is down at the start of a test, this reports what is on disk and in the logs of that node, once, and brings
// the node back, so that one finding is one failure and not the failure of every later test of the shared cluster. A lease manager
// that restarts a fenced primary does it within a few seconds, so the wait is generous: only an instance that stays down is a finding.
func sharedHealth(t *testing.T, c *clustertest.Cluster) {
	t.Helper()
	for _, n := range c.Nodes {
		if !n.Running() {
			continue
		}
		deadline := time.Now().Add(90 * time.Second)
		for !instanceListens(n) && time.Now().Before(deadline) {
			time.Sleep(time.Second)
		}
		if instanceListens(n) {
			continue
		}
		leaseDir, _ := filepath.Glob(filepath.Join(n.DataDir, "lease", "*"))
		inst := n.InstanceLogs()
		if len(inst) > 6000 {
			inst = inst[len(inst)-6000:]
		}
		t.Errorf("the instance of the shared cluster on node %d (port %d) is down at the start of %s and stayed down for 90 s while its kingresd runs.\n"+
			"lease directory: %s\nkingresd log:\n%s\ninstance logs:\n%s", n.Index, n.PGPort, t.Name(), strings.Join(leaseDir, " "), n.LogTail(6000), inst)
		// Bring it back the way an operator would: a restart of kingresd starts a stopped instance.
		n.Stop()
		n.Start()
		waitLogin(t, n)
		if !instanceListens(n) {
			t.Fatalf("node %d: the instance is still down after a restart of kingresd", n.Index)
		}
	}
}
