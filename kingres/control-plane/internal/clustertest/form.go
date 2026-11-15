// SPDX-License-Identifier: AGPL-3.0-only

package clustertest

import "testing"

// FormEach is FormWithExtra where the extra kingresd flags are chosen per node
// (a test that gives every server its own address for a setting). extra is
// called once per node before it starts.
func FormEach(t testing.TB, n int, extra func(*Node) []string, before func(leader *Node)) *Cluster {
	t.Helper()
	c := New(t, n)
	for _, nd := range c.Nodes {
		nd.Extra = extra(nd)
	}
	for _, nd := range c.Nodes {
		nd.Start()
	}
	c.Leader().InitCluster("multi")
	if before != nil {
		before(c.Leader())
	}
	c.joinRest(t, 1)
	return c
}
