// SPDX-License-Identifier: PostgreSQL

package pgstore

import "testing"

func TestStatusPendingRebalanceNeedsBucketsElsewhere(t *testing.T) {
	sv := &serverInfo{ID: "a", Active: true, Should: true}
	empty := map[string]map[int32]*groupBuckets{"app": {0: {}}}
	if anyHeld(empty) {
		t.Fatal("a bucket map without buckets counts as holding buckets")
	}
	// A sharded database with no distributed table: nothing to move, nothing pending.
	if got := statusOf(sv, 0, 0, anyHeld(empty)); got != "live" {
		t.Errorf("status with no buckets in the cluster = %q, want live", got)
	}
	held := map[string]map[int32]*groupBuckets{"app": {1: {Active: 4}}}
	if !anyHeld(held) {
		t.Fatal("buckets not seen")
	}
	// Buckets exist on another server: this one waits for a rebalance.
	if got := statusOf(sv, 0, 0, anyHeld(held)); got != "pendingRebalance" {
		t.Errorf("status = %q, want pendingRebalance", got)
	}
	if got := statusOf(sv, 3, 3, true); got != "live" {
		t.Errorf("status with buckets = %q", got)
	}
	// Defect 2: the replicas of a neighboring group's buckets, which the join creates a
	// standby for, are not what a rebalance gives a server; whether they had arrived must not
	// decide between live and pendingRebalance.
	for _, replicas := range []int{0, 1, 32} {
		if got := statusOf(sv, replicas, 0, true); got != "pendingRebalance" {
			t.Errorf("a server with %d replica buckets and no primary bucket: %q, want pendingRebalance", replicas, got)
		}
	}
	// A server that is being drained still reports draining while it holds anything.
	if got := statusOf(&serverInfo{ID: "a", Active: true}, 2, 0, true); got != "draining" {
		t.Errorf("draining server = %q", got)
	}
}
