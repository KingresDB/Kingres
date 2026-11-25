// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/internalapi"
)

type canned map[string]struct {
	a   peerAnswer
	err error
}

func (c canned) ask(_ context.Context, peer string) (peerAnswer, error) {
	r := c[peer]
	return r.a, r.err
}

func ans(leader string, held bool) struct {
	a   peerAnswer
	err error
} {
	return struct {
		a   peerAnswer
		err error
	}{a: peerAnswer{Leader: leader, HeldBack: held}}
}

func TestAgreedLeader(t *testing.T) {
	down := struct {
		a   peerAnswer
		err error
	}{err: errors.New("unreachable")}
	cases := []struct {
		name  string
		total int
		peers canned
		want  string
	}{
		{"all agree, three servers", 3, canned{"b": ans("a", false), "c": ans("a", false)}, "a"},
		{"one of two peers is enough with three servers (this server counts)", 3, canned{"b": ans("a", false), "c": down}, "a"},
		{"disagreeing answers", 3, canned{"b": ans("a", false), "c": ans("b", false)}, ""},
		{"unreachable peers leave too few answers (five servers)", 5, canned{"b": ans("a", false), "c": down, "d": down, "e": down}, ""},
		{"a minority of five", 5, canned{"b": ans("a", false), "c": down, "d": down, "e": ans("", false)}, ""},
		{"a majority of five", 5, canned{"b": ans("a", false), "c": ans("a", false), "d": down, "e": down}, "a"},
		{"answers of held-back servers are ignored", 3, canned{"b": ans("a", true), "c": ans("a", true)}, ""},
		{"a held-back answer does not make up the majority (five servers)", 5, canned{"b": ans("a", false), "c": ans("a", true), "d": ans("a", true), "e": down}, ""},
		{"a held-back answer that disagrees is ignored, not a disagreement", 3, canned{"b": ans("a", false), "c": ans("old", true)}, "a"},
		{"nobody names a leader", 3, canned{"b": ans("", false), "c": ans("", false)}, ""},
	}
	for _, c := range cases {
		var peers []string
		for p := range c.peers {
			peers = append(peers, p)
		}
		if got := agreedLeader(context.Background(), peers, c.total, c.peers.ask); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

// Nothing is kept between calls: a leader that changed is seen at the next call, and so is a peer that stopped answering.
func TestAgreedLeaderIsNotCached(t *testing.T) {
	peers := canned{"b": ans("a", false), "c": ans("a", false)}
	names := []string{"b", "c"}
	if got := agreedLeader(context.Background(), names, 3, peers.ask); got != "a" {
		t.Fatalf("first: %q", got)
	}
	peers["b"], peers["c"] = ans("z", false), ans("z", false)
	if got := agreedLeader(context.Background(), names, 3, peers.ask); got != "z" {
		t.Errorf("after the leader changed: %q, want z", got)
	}
	peers["b"], peers["c"] = ans("z", true), ans("z", true)
	if got := agreedLeader(context.Background(), names, 3, peers.ask); got != "" {
		t.Errorf("after the peers were held back: %q, want nothing", got)
	}
}

func TestUnquiesceOfAServerThatCannotReadItsTerm(t *testing.T) {
	ctx := context.Background()
	noPrimary := func(int32) (int, bool) { return 0, false }
	primary := func(int32) (int, bool) { return 3, true }
	stopped := func(context.Context, int) (bool, error) { return false, nil }
	running := func(context.Context, int) (bool, error) { return true, nil }
	unknown := func(context.Context, int) (bool, error) { return false, errors.New("cannot look") }
	if err := unquiesceUnreadable(ctx, 1, noPrimary, running); err != nil {
		t.Errorf("no primary of the group here: %v", err)
	}
	if err := unquiesceUnreadable(ctx, 1, primary, stopped); err != nil {
		t.Errorf("the primary does not run: %v", err)
	}
	for name, run := range map[string]func(context.Context, int) (bool, error){"runs": running, "unknown": unknown} {
		err := unquiesceUnreadable(ctx, 1, primary, run)
		var ce *internalapi.CallError
		if !errors.As(err, &ce) || ce.Status != http.StatusServiceUnavailable {
			t.Errorf("the primary %s: %v, want 503", name, err)
		}
	}
}

// Phase 5 review L4: the members are asked side by side; six that hold the call for the whole timeout cost the call one timeout, not six.
func TestTheMembersAreAskedSideBySide(t *testing.T) {
	slow := func(ctx context.Context, peer string) (peerAnswer, error) {
		if peer == "up" {
			return peerAnswer{Leader: "a"}, nil
		}
		<-ctx.Done()
		return peerAnswer{}, ctx.Err()
	}
	start := time.Now()
	got := agreedLeader(context.Background(), []string{"up", "d1", "d2", "d3", "d4", "d5"}, 7, slow)
	if took := time.Since(start); took > 3*time.Second {
		t.Fatalf("the call took %v: the members were asked one after the other", took)
	}
	if got != "" {
		t.Fatalf("one answer of seven servers is not a majority with this one: %q", got)
	}
	if got := agreedLeader(context.Background(), []string{"up", "d1"}, 3, slow); got != "a" {
		t.Fatalf("this server and one answer are a majority of three: %q", got)
	}
}
