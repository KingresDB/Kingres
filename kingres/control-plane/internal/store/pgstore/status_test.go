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
	if got := statusOf(sv, 0, anyHeld(empty)); got != "live" {
		t.Errorf("status with no buckets in the cluster = %q, want live", got)
	}
	held := map[string]map[int32]*groupBuckets{"app": {1: {Active: 4}}}
	if !anyHeld(held) {
		t.Fatal("buckets not seen")
	}
	// Buckets exist on another server: this one waits for a rebalance.
	if got := statusOf(sv, 0, anyHeld(held)); got != "pendingRebalance" {
		t.Errorf("status = %q, want pendingRebalance", got)
	}
	if got := statusOf(sv, 3, true); got != "live" {
		t.Errorf("status with buckets = %q", got)
	}
}
