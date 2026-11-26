// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package failover

import (
	"context"
	"errors"
	"testing"
	"time"
)

func autoView() AutoView {
	return AutoView{
		Settings: AutoSettings{Enabled: true, TimeoutSeconds: 30, MaxCount: 1, FailureTimeout: 10 * time.Second},
		Servers: []AutoServer{
			{ID: "s1", Dead: true, Silence: 40 * time.Second, NotLiveFor: 2, Answered: 2, Members: 3, Groups: []int32{1}, WithStandby: []int32{1}},
			{ID: "s2"}, {ID: "s3"},
		},
		HearsMajority: true,
		Now:           time.Unix(1_000_000, 0),
	}
}

// Every row of the table of section 4.3 and the evidence of 4.2.
func TestEvaluateFollowsTheRulesOfTheDesign(t *testing.T) {
	for _, c := range []struct {
		name  string
		mod   func(v *AutoView)
		start string
		limit bool
	}{
		{"a dead server that a majority agrees on is failed over", func(v *AutoView) {}, "s1", false},
		{"off", func(v *AutoView) { v.Settings.Enabled = false }, "", false},
		{"the limit is reached", func(v *AutoView) { v.Settings.EventsSinceReset = 1 }, "", true},
		{"no majority is heard", func(v *AutoView) { v.HearsMajority = false }, "", false},
		{"silent for less than the timeout", func(v *AutoView) { v.Servers[0].Silence = 29 * time.Second }, "", false},
		{"the floor is three failure timeouts", func(v *AutoView) { v.Settings.TimeoutSeconds = 1; v.Servers[0].Silence = 20 * time.Second }, "", false},
		{"not dead for the tracker", func(v *AutoView) { v.Servers[0].Dead = false }, "", false},
		{"the others still hear it", func(v *AutoView) { v.Servers[0].NotLiveFor = 1 }, "", false},
		{"exactly half of the members agree is not a majority", func(v *AutoView) { v.Servers[0].NotLiveFor, v.Servers[0].Members = 2, 4 }, "", false},
		{"no member answered", func(v *AutoView) { v.Servers[0].NotLiveFor, v.Servers[0].Answered, v.Servers[0].Members = 0, 0, 3 }, "", false},
		{"silent for exactly the timeout", func(v *AutoView) { v.Servers[0].Silence = 30 * time.Second }, "s1", false},
		{"another failover is unfinished", func(v *AutoView) { v.Unfinished = true }, "", false},
		{"a group of it was failed over a minute ago", func(v *AutoView) { v.LastEnded = map[int32]time.Time{1: v.Now.Add(-time.Minute)} }, "", false},
		{"no group has a standby", func(v *AutoView) { v.Servers[0].WithStandby = nil }, "", false},
	} {
		v := autoView()
		c.mod(&v)
		d := Evaluate(v)
		if d.Start != c.start || d.LimitReached != c.limit {
			t.Errorf("%s: %+v", c.name, d)
		}
	}
	// A cool-down that has passed does not hold it back.
	v := autoView()
	v.LastEnded = map[int32]time.Time{1: v.Now.Add(-AutoCooldown - time.Second)}
	if d := Evaluate(v); d.Start != "s1" {
		t.Errorf("after the cool-down: %+v", d)
	}
	// A group without a standby is named for the alert, the others are failed over.
	v = autoView()
	v.Servers[0].Groups = []int32{1, 2}
	if d := Evaluate(v); d.Start != "s1" || len(d.Blocked) != 1 || d.Blocked[0] != 2 {
		t.Errorf("blocked: %+v", d)
	}
}

type autoFake struct {
	v        AutoView
	stopOK   bool
	startErr error
	started  []string
	stops    int
	alerts   []string
	cleared  []string
}

func (f *autoFake) View(context.Context) (AutoView, error) { return f.v, nil }
func (f *autoFake) StopRebalance(context.Context, string) (bool, error) {
	f.stops++
	return f.stopOK, nil
}
func (f *autoFake) Start(_ context.Context, s string) error {
	f.started = append(f.started, s)
	return f.startErr
}
func (f *autoFake) Alert(_ context.Context, kind, _, _, _ string, raise bool) {
	if raise {
		f.alerts = append(f.alerts, kind)
	} else {
		f.cleared = append(f.cleared, kind)
	}
}
func (f *autoFake) Log(string, ...any) {}

// The rebalance is stopped before the task starts, and a start that waits for it does not happen yet (4.5).
func TestAutoStopsTheRebalanceBeforeItStartsTheTask(t *testing.T) {
	f := &autoFake{v: autoView()}
	a := &Auto{P: f}
	if d, err := a.Step(context.Background()); err != nil || d.Start != "" || len(f.started) != 0 || f.stops != 1 {
		t.Fatalf("%+v %v started %v stops %d", d, err, f.started, f.stops)
	}
	f.stopOK = true
	if d, _ := a.Step(context.Background()); d.Start != "s1" || len(f.started) != 1 {
		t.Fatalf("%+v started %v", d, f.started)
	}
}

// A failed start backs off, and the back-off grows; the limit raises its alert once.
func TestAutoBacksOffAfterAFailedStartAndRaisesTheLimitOnce(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	f := &autoFake{v: autoView(), stopOK: true, startErr: errors.New("lease_still_held")}
	a := &Auto{P: f, Now: func() time.Time { return now }}
	_, _ = a.Step(context.Background())
	if len(f.started) != 1 {
		t.Fatal("no start")
	}
	_, _ = a.Step(context.Background())
	if len(f.started) != 1 {
		t.Fatal("the loop did not back off")
	}
	now = now.Add(61 * time.Second)
	_, _ = a.Step(context.Background())
	if len(f.started) != 2 {
		t.Fatal("the back-off did not end")
	}
	now = now.Add(61 * time.Second)
	_, _ = a.Step(context.Background())
	if len(f.started) != 2 {
		t.Fatal("the second back-off is not longer")
	}
	f2 := &autoFake{v: autoView(), stopOK: true}
	f2.v.Settings.EventsSinceReset = 1
	a2 := &Auto{P: f2}
	for i := 0; i < 3; i++ {
		_, _ = a2.Step(context.Background())
	}
	if len(f2.started) != 0 || len(f2.alerts) != 1 || f2.alerts[0] != "autoFailoverLimitReached" {
		t.Fatalf("started %v alerts %v", f2.started, f2.alerts)
	}
}

// F4-13. A leader that takes over does not know what the one before it raised: its first look clears the alert of the limit when the limit is
// not reached (after a reset, the alert of the old leader would stay for ever), and raises it when it is.
func TestANewLoopClearsTheAlertOfTheLimitWhenItIsNotReached(t *testing.T) {
	f := &autoFake{v: autoView(), stopOK: true}
	a := &Auto{P: f}
	_, _ = a.Step(context.Background())
	if len(f.cleared) != 1 || f.cleared[0] != "autoFailoverLimitReached" {
		t.Fatalf("cleared %v", f.cleared)
	}
	f.started = nil
	_, _ = a.Step(context.Background())
	if len(f.cleared) != 1 {
		t.Fatalf("the clear is repeated every look: %v", f.cleared)
	}
	f2 := &autoFake{v: autoView(), stopOK: true}
	f2.v.Settings.EventsSinceReset = 1
	a2 := &Auto{P: f2}
	_, _ = a2.Step(context.Background())
	if len(f2.alerts) != 1 {
		t.Fatalf("raised %v", f2.alerts)
	}
}
