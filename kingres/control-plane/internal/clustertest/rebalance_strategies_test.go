// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026 RooyeKhat Media Co

package clustertest_test

import (
	"context"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/clustertest"
)

// Phase 10, increment 2: the strategies, the threshold and the weights through the real extension (0.8).
// Three servers; the first holds every bucket when the others join, so every plan is a fill of two empty groups.

func planImpacts(plan map[string]any) map[string]map[string]any {
	out := map[string]map[string]any{}
	for _, im := range anyList(plan["serverImpacts"]) {
		m := im.(map[string]any)
		out[m["serverId"].(string)] = m
	}
	return out
}

func TestRebalanceStrategiesAndWeights(t *testing.T) {
	c := clustertest.FormWithExtra(t, 3, []string{"-rebalance-retry-backoff", "1s"}, func(leader *clustertest.Node) {
		body := leader.Expect(202, "POST", "/databases", map[string]any{"name": "rb", "sharding": map[string]any{"enabled": true, "bucketCount": 16}})
		leader.WaitTaskDone(clustertest.TaskID(body), 4*time.Minute)
		sc := leader.ServicePG("rb")
		if _, err := sc.Exec(context.Background(), `CREATE TABLE items (id bigint PRIMARY KEY, val text)`); err != nil {
			t.Fatal(err)
		}
		body = leader.Expect(202, "PUT", "/databases/rb/schemas/public/tables/items/distribution", map[string]any{"method": "hash", "distributionColumn": "id"})
		leader.WaitTaskDone(clustertest.TaskID(body), 2*time.Minute)
		if _, err := sc.Exec(context.Background(), `INSERT INTO items SELECT g, repeat('x', 200) FROM generate_series(1, 400) g`); err != nil {
			t.Fatal(err)
		}
	})
	leader := c.Leader()
	a, b := c.Nodes[1].Host, c.Nodes[2].Host

	t.Run("the plan says its strategy, threshold, reason and loads", func(t *testing.T) {
		plan := leader.Expect(200, "POST", "/rebalance/plan", map[string]any{"moveLimit": 500})
		if plan["strategy"] != "byBucketCount" || plan["threshold"] != float64(0) || plan["needed"] != true {
			t.Fatalf("default plan: %v", plan)
		}
		for _, m := range anyList(plan["moves"]) {
			mv := m.(map[string]any)
			if mv["reason"] != "balance" || mv["cost"] != float64(1) {
				t.Errorf("move: %v", mv)
			}
		}
		im := planImpacts(plan)
		if im[leader.Host]["loadBefore"] != float64(16) || im[leader.Host]["primaryBucketsBefore"] != float64(16) {
			t.Errorf("impact of the first server: %v", im[leader.Host])
		}
		if plan["imbalance"] == nil || plan["imbalance"].(float64) <= 0 {
			t.Errorf("imbalance: %v", plan["imbalance"])
		}
	})

	t.Run("by disk size every bucket costs at least a mebibyte and the threshold is the strategy's", func(t *testing.T) {
		plan := leader.Expect(200, "POST", "/rebalance/plan", map[string]any{"strategy": "byDiskSize", "moveLimit": 500})
		if plan["strategy"] != "byDiskSize" || plan["threshold"] != 0.1 || plan["needed"] != true {
			t.Fatalf("plan: %v", plan)
		}
		for _, m := range anyList(plan["moves"]) {
			if mv := m.(map[string]any); mv["cost"].(float64) < 1<<20 {
				t.Errorf("cost below the minimum: %v", mv)
			}
		}
		// A threshold of 1 accepts any placement: nothing to do.
		plan = leader.Expect(200, "POST", "/rebalance/plan", map[string]any{"strategy": "byDiskSize", "threshold": 1})
		if plan["needed"] != false {
			t.Errorf("a threshold of 1 plans nothing: %v", plan)
		}
	})

	t.Run("weights shape the plan and are shown on the server", func(t *testing.T) {
		if st, p := leader.Do("PATCH", "/servers/"+a, map[string]any{"weight": 0}, nil); st != 400 && st != 422 {
			t.Errorf("weight 0: %d %v", st, p)
		}
		s := leader.Expect(200, "PATCH", "/servers/"+a, map[string]any{"weight": 2})
		if s["weight"] != float64(2) {
			t.Fatalf("server after the change: %v", s)
		}
		if got := leader.Servers()[a]["weight"]; got != float64(2) {
			t.Errorf("listServers: %v", got)
		}
		plan := leader.Expect(200, "POST", "/rebalance/plan", map[string]any{"moveLimit": 500})
		im := planImpacts(plan)
		// weights 1, 2, 1 over 16 buckets: 4, 8, 4
		if im[a]["primaryBucketsAfter"] != float64(8) || im[b]["primaryBucketsAfter"] != float64(4) || im[leader.Host]["primaryBucketsAfter"] != float64(4) {
			t.Errorf("weighted placement: %v", plan["serverImpacts"])
		}
		leader.Expect(200, "PATCH", "/servers/"+a, map[string]any{"weight": 1})
	})

	t.Run("excluded servers are planned as drained", func(t *testing.T) {
		plan := leader.Expect(200, "POST", "/rebalance/plan", map[string]any{"excludeServerIds": []string{b}, "moveLimit": 500})
		im := planImpacts(plan)
		if im[b]["primaryBucketsAfter"] != float64(0) || im[a]["primaryBucketsAfter"] != float64(8) || im[leader.Host]["primaryBucketsAfter"] != float64(8) {
			t.Errorf("placement without %s: %v", b, plan["serverImpacts"])
		}
	})

	t.Run("the settings give the defaults of a run", func(t *testing.T) {
		set := leader.Expect(200, "PATCH", "/settings/cluster", map[string]any{"rebalance": map[string]any{"strategy": "byDiskSize", "threshold": 0.2, "maxMovesPerServer": 3}})
		rb := set["rebalance"].(map[string]any)
		if rb["strategy"] != "byDiskSize" || rb["threshold"] != 0.2 || rb["maxMovesPerServer"] != float64(3) || rb["maxMoveAttempts"] != float64(3) {
			t.Fatalf("settings: %v", rb)
		}
		plan := leader.Expect(200, "POST", "/rebalance/plan", map[string]any{})
		if plan["strategy"] != "byDiskSize" || plan["threshold"] != 0.2 {
			t.Errorf("the plan takes the settings: %v", plan)
		}
		if st, p := leader.Do("PATCH", "/settings/cluster", map[string]any{"rebalance": map[string]any{"strategy": "byColour"}}, nil); st != 400 && st != 422 {
			t.Errorf("unknown strategy: %d %v", st, p)
		}
		set = leader.Expect(200, "PATCH", "/settings/cluster", map[string]any{"rebalance": map[string]any{"threshold": nil, "strategy": "byBucketCount"}})
		if rb = set["rebalance"].(map[string]any); rb["threshold"] != nil {
			t.Errorf("null takes the default again: %v", rb)
		}
	})

	t.Run("a run records and reports what it used, and a drain run says drain", func(t *testing.T) {
		leader.Expect(200, "PATCH", "/servers/"+leader.Host, map[string]any{"shouldHaveShards": false})
		body := leader.Expect(202, "POST", "/rebalance", map[string]any{"strategy": "byDiskSize", "drainOnly": true})
		id := clustertest.TaskID(body)
		rep := waitReport(t, leader, id, 10*time.Minute)
		if rep["outcome"] != "completed" || rep["strategy"] != "byDiskSize" || rep["trigger"] != "manual" {
			t.Fatalf("report: %v", rep)
		}
		if opts, _ := rep["options"].(map[string]any); opts["drainOnly"] != true || opts["threshold"] != 0.1 {
			t.Errorf("options in the report: %v", rep["options"])
		}
		for _, m := range anyList(rep["moves"]) {
			if mv := m.(map[string]any); mv["reason"] != "drain" {
				t.Errorf("move: %v", mv)
			}
		}
		if got := primaryBuckets(t, leader)[leader.Host]; got != 0 {
			t.Errorf("%s still holds %d buckets", leader.Host, got)
		}
		leader.Expect(200, "PATCH", "/servers/"+leader.Host, map[string]any{"shouldHaveShards": true})
	})
}
