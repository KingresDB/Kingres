// SPDX-License-Identifier: AGPL-3.0-only

package standby

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/clock"
)

func freezeAgent() (*Agent, *clock.Fake) {
	clk := clock.NewFake(time.Unix(0, 0))
	return &Agent{Clock: clk, Log: slog.New(slog.DiscardHandler), frozen: map[int32]time.Duration{}, gone: map[string]time.Duration{},
		kick: make(chan struct{}, 1)}, clk
}

// A freeze holds until it lapses, and a lapsed freeze is harmless; the hook of the
// agent counts as well.
func TestFreezeLapsesByItselfAndTheHookCounts(t *testing.T) {
	a, clk := freezeAgent()
	if a.isFrozen(1) {
		t.Fatal("nothing is frozen at the start")
	}
	a.Freeze(1, 30*time.Second)
	clk.Advance(29 * time.Second)
	if !a.isFrozen(1) || a.isFrozen(2) {
		t.Fatal("the freeze must hold for its group, for its time")
	}
	clk.Advance(2 * time.Second)
	if a.isFrozen(1) {
		t.Fatal("a freeze nobody renews must lapse")
	}
	a.Freeze(1, 30*time.Second)
	a.Unfreeze(1)
	if a.isFrozen(1) {
		t.Fatal("Unfreeze must lift it")
	}
	a.Frozen = func(g int32) bool { return g == 4 }
	if !a.isFrozen(4) {
		t.Fatal("the Frozen hook must count")
	}
	// An absurd ttl is cut to the maximum.
	a.Freeze(5, 100*time.Hour)
	clk.Advance(MaxFreeze + time.Second)
	if a.isFrozen(5) {
		t.Fatal("a freeze longer than MaxFreeze must be cut")
	}
}

// The synchronous list of a frozen group is never emptied, however long no standby
// streams; after the freeze lapses the rule is the grace period's again.
func TestSyncRuleWhileFrozenAndAfterTheFreezeLapses(t *testing.T) {
	a, clk := freezeAgent()
	var ss SyncState
	in := func(frozen bool) SyncDecision {
		return ss.Decide(SyncInput{Now: clk.Mono(), Planned: []string{"s1"}, Streaming: map[string]bool{}, Applied: "ANY 1 (s1)", Frozen: frozen, Grace: 30 * time.Second})
	}
	a.Freeze(1, 10*time.Minute)
	_ = in(a.isFrozen(1))
	clk.Advance(5 * time.Minute)
	if d := in(a.isFrozen(1)); d.Set != nil {
		t.Fatalf("a frozen group's list was changed: %+v", d)
	}
	a.Unfreeze(1)
	if d := in(a.isFrozen(1)); d.Set == nil || *d.Set != "" || !d.WithoutSynchronousStandby {
		t.Fatalf("after the freeze the grace has long passed: the list must be emptied: %+v", d)
	}
}

// standbys-gone: the standbys of the server are not planned any more for the list of
// the other groups, so the list is emptied at once and not after the grace; the
// group that is being failed over keeps its list.
func TestStandbysGoneEmptiesTheListAtOnceButNotOfAFrozenGroup(t *testing.T) {
	a, clk := freezeAgent()
	entries := []Entry{{ID: 1, Server: "x"}, {ID: 2, Server: "y"}}
	a.StandbysGone("x", time.Minute, false)
	gone := a.goneServers()
	if !gone["x"] || gone["y"] {
		t.Fatalf("gone = %v", gone)
	}
	var planned []string
	for _, e := range entries {
		if !dropGoneEntry(e, gone, false) {
			planned = append(planned, AppName(e.ID))
		}
	}
	var ss SyncState
	d := ss.Decide(SyncInput{Now: clk.Mono(), Planned: []string{AppName(2)}, Streaming: map[string]bool{AppName(2): true}, Applied: Names([]string{AppName(1), AppName(2)}), Grace: 30 * time.Second})
	if len(planned) != 1 || d.Set == nil || *d.Set != Names([]string{AppName(2)}) {
		t.Fatalf("the name of the gone standby must leave the list at once: planned %v, %+v", planned, d)
	}
	if !dropGoneEntry(entries[0], gone, false) || dropGoneEntry(entries[0], gone, true) {
		t.Fatal("a frozen group must keep a gone standby in its list")
	}
	clk.Advance(MaxGone + time.Second)
	if len(a.goneServers()) != 0 {
		t.Fatal("a gone declaration that nobody renews must lapse")
	}
	a.StandbysGone("x", time.Minute, false)
	a.StandbysGone("x", 0, true)
	if len(a.goneServers()) != 0 {
		t.Fatal("clear must take the server back")
	}
}

func TestFreezeCallsValidateAndApply(t *testing.T) {
	a, _ := freezeAgent()
	ctx := context.Background()
	for _, bad := range []string{`{}`, `{"group":1}`, `{"group":-1,"ttlMs":5}`, `not json`} {
		if _, err := a.handleFreezeCall(ctx, CallFreeze, []byte(bad)); err == nil {
			t.Errorf("%s was accepted", bad)
		}
	}
	if _, err := a.handleFreezeCall(ctx, CallFreeze, []byte(`{"group":3,"ttlMs":60000,"task":9,"epoch":2}`)); err != nil || !a.isFrozen(3) {
		t.Fatalf("freeze: %v frozen=%v", err, a.isFrozen(3))
	}
	if g := a.FrozenGroups(); len(g) != 1 || g[0] != 3 {
		t.Fatalf("FrozenGroups = %v", g)
	}
	if _, err := a.handleFreezeCall(ctx, CallUnfreeze, []byte(`{"group":3}`)); err != nil || a.isFrozen(3) {
		t.Fatalf("unfreeze: %v", err)
	}
	if _, err := a.handleFreezeCall(ctx, CallStandbysGone, []byte(`{"server":""}`)); err == nil {
		t.Error("an empty server was accepted")
	}
	if _, err := a.handleFreezeCall(ctx, CallStandbysGone, []byte(`{"server":"x","ttlMs":1000}`)); err != nil || !a.goneServers()["x"] {
		t.Fatalf("standbys-gone: %v", err)
	}
}
