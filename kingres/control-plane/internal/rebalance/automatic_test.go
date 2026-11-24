// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026 RooyeKhat Media Co

package rebalance

import (
	"reflect"
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)

func tp(d time.Duration) *time.Time { t := t0.Add(d); return &t }

func on() AutoSettings { s := DefaultAutoSettings(); s.Enabled = true; return s }

func TestEvaluate(t *testing.T) {
	cases := []struct {
		name string
		set  func(*AutoSettings)
		f    AutoFacts
		want Evaluation
	}{
		{"off", func(s *AutoSettings) { s.Enabled = false }, AutoFacts{Now: t0, PlanNeeded: true, HeldSince: tp(-time.Hour)}, Evaluation{Outcome: EvalNothingToDo}},
		{"balanced", nil, AutoFacts{Now: t0}, Evaluation{Outcome: EvalNothingToDo}},
		{"young imbalance", nil, AutoFacts{Now: t0, PlanNeeded: true, HeldSince: tp(-time.Minute)}, Evaluation{Outcome: EvalSettling}},
		{"imbalance never recorded", nil, AutoFacts{Now: t0, PlanNeeded: true}, Evaluation{Outcome: EvalSettling}},
		{"old imbalance starts", nil, AutoFacts{Now: t0, PlanNeeded: true, HeldSince: tp(-6 * time.Minute)}, Evaluation{Outcome: EvalStarted}},
		{"exactly settled", nil, AutoFacts{Now: t0, PlanNeeded: true, HeldSince: tp(-300 * time.Second)}, Evaluation{Outcome: EvalStarted}},
		{"topology change", nil, AutoFacts{Now: t0, PlanNeeded: true, HeldSince: tp(-time.Hour), LastTopologyChange: tp(-9 * time.Minute)}, Evaluation{Outcome: EvalSettling}},
		{"topology long ago", nil, AutoFacts{Now: t0, PlanNeeded: true, HeldSince: tp(-time.Hour), LastTopologyChange: tp(-11 * time.Minute)}, Evaluation{Outcome: EvalStarted}},
		{"too soon after the last run", nil, AutoFacts{Now: t0, PlanNeeded: true, HeldSince: tp(-time.Hour), LastAutomaticEnd: tp(-30 * time.Minute)}, Evaluation{Outcome: EvalSettling}},
		{"waived by a new server", nil, AutoFacts{Now: t0, PlanNeeded: true, HeldSince: tp(-time.Hour), LastAutomaticEnd: tp(-30 * time.Minute), WaiveMinInterval: true}, Evaluation{Outcome: EvalStarted}},
		{"a rule blocks", nil, AutoFacts{Now: t0, PlanNeeded: true, HeldSince: tp(-time.Hour), Blocked: []string{RuleServers, RuleFailover}}, Evaluation{Outcome: EvalBlocked, BlockedBy: []string{RuleFailover, RuleServers}}},
		{"a rule does not matter when nothing is to do", nil, AutoFacts{Now: t0, Blocked: []string{RuleServers}}, Evaluation{Outcome: EvalNothingToDo}},
		{"outside the window", func(s *AutoSettings) { s.Window = &Window{From: "01:00", To: "05:00", TimeZone: "UTC"} },
			AutoFacts{Now: t0, PlanNeeded: true, HeldSince: tp(-time.Hour)}, Evaluation{Outcome: EvalBlocked, BlockedBy: []string{RuleWindow}}},
		{"inside the window", func(s *AutoSettings) { s.Window = &Window{From: "11:00", To: "13:00", TimeZone: "UTC"} },
			AutoFacts{Now: t0, PlanNeeded: true, HeldSince: tp(-time.Hour)}, Evaluation{Outcome: EvalStarted}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := on()
			if c.set != nil {
				c.set(&s)
			}
			if got := Evaluate(s, c.f); !reflect.DeepEqual(got, c.want) {
				t.Fatalf("got %+v, want %+v", got, c.want)
			}
		})
	}
}

func TestWindow(t *testing.T) {
	at := func(h, m int) time.Time { return time.Date(2026, 11, 21, h, m, 0, 0, time.UTC) }
	w := &Window{From: "22:00", To: "06:00", TimeZone: "UTC"}
	for _, c := range []struct {
		t    time.Time
		want bool
	}{{at(22, 0), true}, {at(23, 59), true}, {at(0, 0), true}, {at(5, 59), true}, {at(6, 0), false}, {at(12, 0), false}, {at(21, 59), false}} {
		if got := w.Contains(c.t); got != c.want {
			t.Errorf("%v in a window over midnight: %v", c.t.Format("15:04"), got)
		}
	}
	// the zone decides: 23:00 UTC is 09:00 the next day in Sydney (UTC+10 in November, +11 with daylight saving)
	syd := &Window{From: "08:00", To: "10:00", TimeZone: "Australia/Sydney"}
	if !syd.Contains(at(22, 30)) || syd.Contains(at(12, 0)) {
		t.Error("time zone of the window")
	}
	if (&Window{From: "bad", To: "10:00", TimeZone: "UTC"}).Contains(at(9, 0)) {
		t.Error("an unreadable window never opens")
	}
	if (&Window{From: "08:00", To: "10:00", TimeZone: "Nowhere/Land"}).Contains(at(9, 0)) {
		t.Error("an unknown zone never opens")
	}
	var none *Window
	if !none.Contains(at(3, 0)) {
		t.Error("no window means any time")
	}
}

func TestSafetyBlockers(t *testing.T) {
	s := on()
	ok := ClusterFacts{Now: t0, ReplicaCount: 1}
	if got := SafetyBlockers(s, ok); len(got) != 0 {
		t.Fatalf("a healthy cluster: %v", got)
	}
	cases := map[string]ClusterFacts{
		RuleFailover:    {Now: t0, FailoverUnfinished: true},
		RuleNoPrimary:   {Now: t0, GroupsNoPrimary: []int32{2}},
		RuleDegraded:    {Now: t0, ReplicaCount: 1, GroupsFewStandbys: []int32{1}},
		RuleServers:     {Now: t0, ServersNotLive: []string{"db2"}},
		RuleFailedMove:  {Now: t0, FailedMoveUnacknowledged: true},
		RuleConsistency: {Now: t0, CheckError: "catalogs differ"},
	}
	for rule, f := range cases {
		if got := SafetyBlockers(s, f); !reflect.DeepEqual(got, []string{rule}) {
			t.Errorf("%s: %v", rule, got)
		}
	}
	// a change of a primary blocks for failoverSettleSeconds
	recent := ClusterFacts{Now: t0, LastPrimaryChange: tp(-5 * time.Minute)}
	if got := SafetyBlockers(s, recent); !reflect.DeepEqual(got, []string{RuleFailover}) {
		t.Errorf("a recent change of a primary: %v", got)
	}
	old := ClusterFacts{Now: t0, LastPrimaryChange: tp(-11 * time.Minute)}
	if got := SafetyBlockers(s, old); len(got) != 0 {
		t.Errorf("an old change of a primary: %v", got)
	}
	// without standbys wanted, a group without a standby is not degraded
	if got := SafetyBlockers(s, ClusterFacts{Now: t0, ReplicaCount: 0, GroupsFewStandbys: []int32{1}}); len(got) != 0 {
		t.Errorf("replicaCount 0: %v", got)
	}
}

func TestAutoSettingsApply(t *testing.T) {
	a := DefaultAutoSettings()
	a.apply("enabled", []byte(`true`))
	a.apply("strategy", []byte(`"byDiskSize"`))
	a.apply("threshold", []byte(`0.3`))
	a.apply("settleSeconds", []byte(`10`))
	a.apply("window", []byte(`{"from":"01:00","to":"05:00","timeZone":"UTC"}`))
	if !a.Enabled || a.Strategy != ByDiskSize || a.Threshold != 0.3 || a.SettleSeconds != 10 || a.Window == nil || a.Window.To != "05:00" {
		t.Fatalf("%+v", a)
	}
	a.apply("window", []byte(`null`))
	if a.Window != nil {
		t.Fatal("null removes the window")
	}
	a.apply("checkIntervalSeconds", []byte(`0`))
	if a.CheckIntervalSeconds != 60 {
		t.Fatal("a value out of range is ignored")
	}
}
