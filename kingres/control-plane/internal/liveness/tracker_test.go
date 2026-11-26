// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package liveness

import (
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/clock"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ha"
)

var cfg = Config{Interval: time.Second, Suspect: 5 * time.Second, Failure: 10 * time.Second}

func newT() (*Tracker, *clock.Fake) {
	clk := clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	t := New(clk, cfg, "a")
	t.Expect([]string{"a", "b", "c"})
	return t, clk
}

func TestSilenceMovesAPeerThroughSuspectToDeadAndBack(t *testing.T) {
	tr, clk := newT()
	if tr.State("b") != ha.PeerUnknown {
		t.Fatal("a peer nobody heard yet is unknown, not dead")
	}
	tr.Heard("b")
	if tr.State("b") != ha.PeerLive {
		t.Fatal("heard: live")
	}
	for _, step := range []struct {
		after time.Duration
		want  ha.PeerState
	}{{2 * time.Second, ha.PeerLive}, {2 * time.Second, ha.PeerLive}, {2 * time.Second, ha.PeerSuspect}, {3 * time.Second, ha.PeerSuspect}, {2 * time.Second, ha.PeerDead}} {
		clk.Advance(step.after)
		tr.Tick()
		if got := tr.State("b"); got != step.want {
			t.Fatalf("after %s more: state %v, want %v", step.after, got, step.want)
		}
	}
	tr.Heard("b")
	if tr.State("b") != ha.PeerLive {
		t.Fatal("a dead peer that is heard again is live again")
	}
}

func TestPeerThatNeverAnswersBecomesDead(t *testing.T) {
	tr, clk := newT()
	for i := 0; i < 11; i++ {
		clk.Advance(time.Second)
		tr.Tick()
	}
	if tr.State("c") != ha.PeerDead {
		t.Fatalf("a peer that never answered must be dead after the failure timeout, got %v", tr.State("c"))
	}
}

func TestWallClockStepChangesNothing(t *testing.T) {
	tr, clk := newT()
	tr.Heard("b")
	clk.Advance(2 * time.Second)
	tr.Tick()
	clk.Advance(2 * time.Second)
	tr.Tick()
	clk.Advance(2 * time.Second)
	tr.Tick()
	clk.StepWall(-24 * time.Hour)
	if tr.State("b") != ha.PeerSuspect {
		t.Fatal("a backward step of the wall clock changed the state")
	}
	clk.StepWall(48 * time.Hour)
	tr.Tick()
	if tr.State("b") != ha.PeerSuspect {
		t.Fatal("a forward step of the wall clock changed the state")
	}
	clk.Advance(2 * time.Second)
	tr.Tick()
	clk.Advance(2 * time.Second)
	tr.Tick()
	if tr.State("b") != ha.PeerDead {
		t.Fatal("monotonic time must still decide")
	}
}

func TestStalledProcessDoesNotJudgeItsPeers(t *testing.T) {
	tr, clk := newT()
	tr.Heard("b")
	tr.Tick()
	clk.Advance(time.Minute) // this process was frozen
	if got := tr.State("b"); got != ha.PeerLive {
		t.Fatalf("before the first tick after the stall the answer is the one from before it, got %v", got)
	}
	tr.Tick()
	if got := tr.State("b"); got != ha.PeerLive {
		t.Fatalf("after a stall every peer gets a fresh window, got %v", got)
	}
	for i := 0; i < 11; i++ {
		clk.Advance(time.Second)
		tr.Tick()
	}
	if tr.State("b") != ha.PeerDead {
		t.Fatal("a peer that stays silent after the stall is still declared dead")
	}
}

func TestLastHeardIsDerivedFromTheMonotonicAge(t *testing.T) {
	tr, clk := newT()
	if _, ok := tr.LastHeard("b"); ok {
		t.Fatal("never heard")
	}
	tr.Heard("b")
	clk.Advance(3 * time.Second)
	got, ok := tr.LastHeard("b")
	if !ok || !got.Equal(clk.Wall().Add(-3*time.Second)) {
		t.Fatalf("LastHeard %v", got)
	}
}

func TestStrangersAndSelf(t *testing.T) {
	tr, _ := newT()
	tr.Heard("zz")
	if tr.State("zz") != ha.PeerUnknown || len(tr.Expected()) != 2 {
		t.Fatal("a server that is not expected must be ignored")
	}
	if tr.State("a") != ha.PeerLive {
		t.Fatal("this server is live")
	}
	tr.Expect([]string{"b"})
	if len(tr.Expected()) != 1 {
		t.Fatal("servers no longer listed are forgotten")
	}
}
