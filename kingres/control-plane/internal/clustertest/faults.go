// SPDX-License-Identifier: AGPL-3.0-only

package clustertest

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/fault"
)

// FaultDir is the directory kingresd of this node polls for faults.json.
func (n *Node) FaultDir() string { return filepath.Join(n.Dir, "fault") }

// ForceEnforce is kept in the fault file by every SetFaults of this node (package fault).

// SetFaults replaces the faults of this node (package fault); the empty Spec
// removes them. kingresd notices at its next use of a fault point (the hook compares the file, package fault), so a step that starts after this call returns never sees the faults that were replaced; tests that depend on an effect wait for the effect, not for a time.
func (n *Node) SetFaults(s fault.Spec) {
	n.t.Helper()
	s.ForceEnforce = s.ForceEnforce || n.ForceEnforce
	b, _ := json.Marshal(s)
	tmp := filepath.Join(n.FaultDir(), ".faults.tmp")
	if err := os.MkdirAll(n.FaultDir(), 0o700); err != nil {
		n.t.Fatal(err)
	}
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		n.t.Fatal(err)
	}
	if err := os.Rename(tmp, filepath.Join(n.FaultDir(), fault.FileName)); err != nil {
		n.t.Fatal(err)
	}
}

// Isolate cuts this node off from the given other nodes in both directions: its
// calls to them fail and it drops theirs. With no argument it is cut off from
// every other server. Faults on the other nodes are not touched (the cut is
// symmetric only when both sides are told; Partition does that).
func (n *Node) Isolate(others ...*Node) {
	n.t.Helper()
	list := []string{"*"}
	if len(others) > 0 {
		list = list[:0]
		for _, o := range others {
			list = append(list, o.Host)
		}
	}
	n.SetFaults(fault.Spec{BlockOutbound: list, BlockInbound: list})
}

// Heal removes every fault of this node.
func (n *Node) Heal() { n.t.Helper(); n.SetFaults(fault.Spec{}) }

// Partition cuts the cluster in two: every node of side is cut off from every
// node not in side, in both directions, on both sides.
func (c *Cluster) Partition(side ...*Node) {
	c.T.Helper()
	in := map[*Node]bool{}
	for _, n := range side {
		in[n] = true
	}
	for _, n := range c.Nodes {
		var other []string
		for _, o := range c.Nodes {
			if in[o] != in[n] {
				other = append(other, o.Host)
			}
		}
		n.SetFaults(fault.Spec{BlockOutbound: other, BlockInbound: other})
	}
}

// HealAll removes the faults of every node.
func (c *Cluster) HealAll() {
	for _, n := range c.Nodes {
		n.Heal()
	}
}

// Pause freezes the kingresd process (SIGSTOP) without touching its instance;
// Resume continues it. A paused process answers nothing and runs no timer.
func (n *Node) Pause() {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.cmd != nil {
		pause(n.cmd, true)
	}
}

// Resume continues a paused kingresd.
func (n *Node) Resume() {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.cmd != nil {
		pause(n.cmd, false)
	}
}
