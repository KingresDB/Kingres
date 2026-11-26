// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package standby

import (
	"reflect"
	"testing"
	"time"
)

func ring(pairs ...string) []Server {
	var out []Server
	for i := 0; i+1 < len(pairs); i += 2 {
		out = append(out, Server{ID: pairs[i], Zone: pairs[i+1]})
	}
	return out
}

func TestCandidatesChainedRing(t *testing.T) {
	r := ring("a", "", "b", "", "c", "", "d", "")
	cases := map[string][]string{
		"a": {"b", "c", "d"},
		"b": {"c", "d", "a"},
		"d": {"a", "b", "c"},
	}
	for p, want := range cases {
		if got := Candidates(r, p); !reflect.DeepEqual(got, want) {
			t.Errorf("primary %s: got %v, want %v", p, got, want)
		}
	}
}

func TestCandidatesSkipTheServerGroupOfThePrimary(t *testing.T) {
	// a and b share a rack: the standby of the group of a must not be on b while c or d exist.
	got := Candidates(ring("a", "r1", "b", "r1", "c", "r2", "d", "r3"), "a")
	if !reflect.DeepEqual(got, []string{"c", "d", "b"}) {
		t.Fatalf("got %v", got)
	}
	// With only the same rack left it is still used: fewer choices, never none.
	got = Candidates(ring("a", "r1", "b", "r1"), "a")
	if !reflect.DeepEqual(got, []string{"b"}) {
		t.Fatalf("got %v", got)
	}
}

func TestCandidatesPreferNewServerGroupsForFurtherStandbys(t *testing.T) {
	got := Candidates(ring("a", "r1", "b", "r2", "c", "r2", "d", "r3"), "a")
	// b (r2) first, then d (r3) rather than c (r2, already used), then c.
	if !reflect.DeepEqual(got, []string{"b", "d", "c"}) {
		t.Fatalf("got %v", got)
	}
}

func TestCandidatesOfSmallRings(t *testing.T) {
	if got := Candidates(nil, "a"); len(got) != 0 {
		t.Fatalf("got %v", got)
	}
	if got := Candidates(ring("a", ""), "a"); len(got) != 0 {
		t.Fatalf("a lone server has no candidate, got %v", got)
	}
}

func TestPlan(t *testing.T) {
	cand := []string{"b", "c", "d"}
	cases := []struct {
		name        string
		existing    []string
		replicas    int
		add, remove []string
	}{
		{"none yet", nil, 1, []string{"b"}, nil},
		{"two wanted", nil, 2, []string{"b", "c"}, nil},
		{"fewer servers than replicas", nil, 5, []string{"b", "c", "d"}, nil},
		{"a working standby stays", []string{"c"}, 1, nil, nil},
		{"raise keeps the existing and adds the best missing", []string{"c"}, 2, []string{"b"}, nil},
		{"lower removes the least preferred", []string{"b", "c", "d"}, 1, nil, []string{"c", "d"}},
		{"zero removes all", []string{"b", "d"}, 0, nil, []string{"b", "d"}},
		{"a standby on a vanished server is replaced", []string{"x"}, 1, []string{"b"}, []string{"x"}},
	}
	for _, c := range cases {
		add, remove := Plan(cand, c.existing, c.replicas)
		if !reflect.DeepEqual(add, c.add) || !reflect.DeepEqual(remove, c.remove) {
			t.Errorf("%s: add %v remove %v, want add %v remove %v", c.name, add, remove, c.add, c.remove)
		}
	}
}

func TestSyncNamesRule(t *testing.T) {
	const grace = 30 * time.Second
	planned := []string{"kingres_standby_2", "kingres_standby_1"}
	var s SyncState
	at := time.Duration(0)
	eval := func(applied string, streaming map[string]bool, frozen bool) SyncDecision {
		return s.Decide(SyncInput{Now: at, Planned: planned, Streaming: streaming, Applied: applied, Frozen: frozen, Grace: grace})
	}
	str := func(d SyncDecision) string {
		if d.Set == nil {
			return "<unchanged>"
		}
		return *d.Set
	}

	// A new standby that has not streamed yet: nothing is set and nothing is reported before the grace period.
	d := eval("", nil, false)
	if d.Set != nil || d.WithoutSynchronousStandby {
		t.Fatalf("a standby that never streamed must not make the group wait: %+v", d)
	}
	// One streams: the setting names all of them.
	at += time.Second
	d = eval("", map[string]bool{"kingres_standby_1": true}, false)
	if str(d) != "ANY 1 (kingres_standby_1, kingres_standby_2)" {
		t.Fatalf("got %q", str(d))
	}
	applied := *d.Set
	// Both lost: inside the grace period the group keeps waiting.
	at += 10 * time.Second
	d = eval(applied, nil, false)
	if d.Set != nil || d.WithoutSynchronousStandby {
		t.Fatalf("inside the grace period nothing changes: %+v", d)
	}
	// Past the grace period but a failover is being decided: still nothing.
	at += 25 * time.Second
	d = eval(applied, nil, true) // 25 s of silence: still inside
	at += 10 * time.Second
	d = eval(applied, nil, true)
	if d.Set != nil {
		t.Fatalf("never cleared while frozen: %+v", d)
	}
	// Not frozen any more: cleared and reported.
	d = eval(applied, nil, false)
	if d.Set == nil || *d.Set != "" || !d.WithoutSynchronousStandby {
		t.Fatalf("past the grace period the group runs without: %+v", d)
	}
	// Still reported while the setting is empty and nothing streams.
	d = eval("", nil, false)
	if d.Set != nil || !d.WithoutSynchronousStandby {
		t.Fatalf("%+v", d)
	}
	// A standby streams again: set back, report over.
	at += time.Second
	d = eval("", map[string]bool{"kingres_standby_2": true}, false)
	if d.Set == nil || *d.Set != applied || d.WithoutSynchronousStandby {
		t.Fatalf("%+v", d)
	}
	// A blip that ends inside the grace period restarts the clock.
	at += time.Second
	_ = eval(applied, nil, false)
	at += 20 * time.Second
	_ = eval(applied, map[string]bool{"kingres_standby_2": true}, false)
	at += 20 * time.Second
	if d = eval(applied, nil, false); d.Set != nil {
		t.Fatalf("the clock must restart after a streaming moment: %+v", d)
	}
	// No standby planned: emptied at once, no report.
	planned = nil
	d = eval(applied, nil, false)
	if d.Set == nil || *d.Set != "" || d.WithoutSynchronousStandby {
		t.Fatalf("%+v", d)
	}
}

func TestSlotPolicy(t *testing.T) {
	cases := []struct {
		retained int64
		want     SlotAction
	}{{0, SlotOK}, {99, SlotOK}, {100, SlotWarn}, {1000, SlotWarn}, {1001, SlotDrop}}
	for _, c := range cases {
		if got := SlotPolicy(c.retained, 100, 1000); got != c.want {
			t.Errorf("%d: got %v want %v", c.retained, got, c.want)
		}
	}
}
