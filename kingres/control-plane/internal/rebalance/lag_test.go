// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026 RooyeKhat Media Co

package rebalance

import (
	"reflect"
	"testing"
)

func TestLaggingGroups(t *testing.T) {
	const limit = int64(64 << 20)
	for _, c := range []struct {
		name string
		lags map[int32]int64
		want []int32
	}{
		{"none", nil, nil},
		{"all in step", map[int32]int64{0: 0, 1: 1024, 2: limit}, nil},
		{"one past the limit", map[int32]int64{1: limit + 1, 2: 5}, []int32{1}},
		{"several, ascending", map[int32]int64{3: limit * 2, 1: limit + 1, 2: 0}, []int32{1, 3}},
		{"an unreadable lag counts as lagging", map[int32]int64{2: -1, 1: 0}, []int32{2}},
	} {
		if got := LaggingGroups(c.lags, limit); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
	// the limit is the setting: a lag of 10 is too much when no lag is allowed
	if got := LaggingGroups(map[int32]int64{1: 10}, 0); !reflect.DeepEqual(got, []int32{1}) {
		t.Errorf("limit 0: %v", got)
	}
}

func TestALaggingStandbyBlocksTheRunLikeADegradedGroup(t *testing.T) {
	s := on()
	lag := ClusterFacts{Now: t0, ReplicaCount: 1, GroupsLagging: []int32{2}}
	if got := SafetyBlockers(s, lag); !reflect.DeepEqual(got, []string{RuleDegraded}) {
		t.Fatalf("a lagging standby: %v", got)
	}
	// no standby is wanted: there is nothing to lag behind
	if got := SafetyBlockers(s, ClusterFacts{Now: t0, ReplicaCount: 0, GroupsLagging: []int32{2}}); len(got) != 0 {
		t.Fatalf("replicaCount 0: %v", got)
	}
	// both reasons name the rule once
	both := ClusterFacts{Now: t0, ReplicaCount: 1, GroupsLagging: []int32{2}, GroupsFewStandbys: []int32{1}}
	if got := SafetyBlockers(s, both); !reflect.DeepEqual(got, []string{RuleDegraded}) {
		t.Fatalf("both: %v", got)
	}
}
