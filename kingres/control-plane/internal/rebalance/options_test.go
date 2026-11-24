// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026 RooyeKhat Media Co

package rebalance

import (
	"testing"
)

func f64(v float64) *float64 { return &v }

func TestResolveFillsFromSettingsAndStrategyDefaults(t *testing.T) {
	set := DefaultSettings()
	o := Resolve(Options{}, set)
	if o.Strategy != ByBucketCount || o.Threshold == nil || *o.Threshold != 0 || o.MaxMovesPerServer != 2 || o.Trigger != TriggerManual {
		t.Fatalf("defaults: %+v", o)
	}
	o = Resolve(Options{Strategy: ByDiskSize}, set)
	if *o.Threshold != 0.1 {
		t.Fatalf("by disk size defaults to 0.1, got %v", *o.Threshold)
	}
	// A cluster threshold applies to the cluster's strategy only; another strategy takes its own default.
	set.Strategy, set.Threshold = ByDiskSize, f64(0.25)
	if got := *Resolve(Options{}, set).Threshold; got != 0.25 {
		t.Fatalf("cluster threshold: %v", got)
	}
	if got := *Resolve(Options{Strategy: ByBucketCount}, set).Threshold; got != 0 {
		t.Fatalf("other strategy must not take the cluster threshold: %v", got)
	}
	// What the request says wins, also an explicit zero.
	o = Resolve(Options{Strategy: ByDiskSize, Threshold: f64(0), MaxMovesPerServer: 5, Trigger: TriggerDrain}, set)
	if *o.Threshold != 0 || o.MaxMovesPerServer != 5 || o.Trigger != TriggerDrain {
		t.Fatalf("explicit values: %+v", o)
	}
}

func TestValidateNew(t *testing.T) {
	bad := []Options{
		{Strategy: "byColour"},
		{Threshold: f64(-0.1)},
		{Threshold: f64(1.5)},
		{MaxMovesPerServer: 17},
		{CleanupDelaySeconds: -1},
		{CleanupDelaySeconds: 86401},
	}
	for i, o := range bad {
		if len(validateNew(o)) != 1 {
			t.Errorf("case %d: want one error, got %v", i, validateNew(o))
		}
	}
	ok := []Options{{}, {Strategy: ByDiskSize, Threshold: f64(1)}, {Threshold: f64(0)}, {MaxMovesPerServer: 16}}
	for i, o := range ok {
		if e := validateNew(o); len(e) != 0 {
			t.Errorf("ok case %d: %v", i, e)
		}
	}
}

func TestSettingsApply(t *testing.T) {
	s := DefaultSettings()
	s.apply("rebalance.strategy", []byte(`"byDiskSize"`))
	s.apply("rebalance.threshold", []byte(`0.3`))
	s.apply("rebalance.maxMovesPerServer", []byte(`4`))
	s.apply("rebalance.maxMoveAttempts", []byte(`5`))
	s.apply("rebalance.maxDiskUsedFraction", []byte(`0.9`))
	s.apply("rebalance.maxStandbyLagBytes", []byte(`1024`))
	if s.Strategy != ByDiskSize || *s.Threshold != 0.3 || s.MaxMovesPerServer != 4 || s.MaxMoveAttempts != 5 || s.MaxDiskUsedFraction != 0.9 || s.MaxStandbyLagBytes != 1024 {
		t.Fatalf("%+v", s)
	}
	s.apply("rebalance.threshold", []byte(`null`))
	if s.Threshold != nil {
		t.Fatal("null takes the strategy default again")
	}
	s.apply("rebalance.strategy", []byte(`"nonsense"`))
	if s.Strategy != ByDiskSize {
		t.Fatal("an unknown strategy is ignored")
	}
	s.apply("rebalance.maxMovesPerServer", []byte(`"x"`))
	if s.MaxMovesPerServer != 4 {
		t.Fatal("a value of the wrong type is ignored")
	}
}

func TestNeedsExtension08(t *testing.T) {
	if needsExtension08(Options{Strategy: ByBucketCount, Threshold: f64(0)}) {
		t.Fatal("the old plan is enough")
	}
	for _, o := range []Options{{Strategy: ByDiskSize}, {Threshold: f64(0.1)}, {DrainOnly: true}, {ExcludeServers: []string{"a"}}} {
		if !needsExtension08(o) {
			t.Fatalf("%+v needs plan_rebalance", o)
		}
	}
}

func TestSQLStrategy(t *testing.T) {
	if sqlStrategy(ByDiskSize) != "by_disk_size" || sqlStrategy(ByBucketCount) != "by_bucket_count" || sqlStrategy("") != "by_bucket_count" {
		t.Fatal("strategy names")
	}
}
