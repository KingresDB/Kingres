// SPDX-License-Identifier: AGPL-3.0-only

package internalapi

import (
	"context"
	"errors"
	"testing"
	"time"
)

// A leader that is unknown for a moment (a catalog that is read slowly, or just copied)
// must not make the server refuse the leader's call with not_leader.
func TestUnknownLeaderIsAskedAgainBeforeARefusal(t *testing.T) {
	asks := 0
	s := New(nil, Backend{Leader: func() string {
		asks++
		if asks < 3 {
			return ""
		}
		return "s1"
	}}, nil, nil)
	s.leaderRetry = time.Millisecond
	if got := s.leader(context.Background()); got != "s1" || asks != 3 {
		t.Fatalf("leader = %q after %d asks", got, asks)
	}
	// Truly unknown: gives up after the bounded number of asks.
	asks = -100
	if got := s.leader(context.Background()); got != "" || asks != -96 {
		t.Fatalf("leader = %q after %d asks", got, asks+100)
	}
	// A known leader is returned at once.
	s.be.Leader = func() string { asks++; return "s2" }
	asks = 0
	if s.leader(context.Background()) != "s2" || asks != 1 {
		t.Fatal("a known leader was asked more than once")
	}
}

// A catalog that cannot be read for a moment (the leader's instance restarts) must not
// make the server answer 503 "unavailable" to a member's call: it asks again; a catalog
// that stays unreadable still fails closed.
func TestMembershipIsAskedAgainWhileTheCatalogIsUnreadable(t *testing.T) {
	asks := 0
	s := New(nil, Backend{Member: func(context.Context, string) (bool, error) {
		asks++
		if asks < 4 {
			return false, errors.New("catalog down")
		}
		return true, nil
	}}, nil, nil)
	s.leaderRetry = time.Millisecond
	if ok, err := s.isMember(context.Background(), "p"); err != nil || !ok || asks != 4 {
		t.Fatalf("member = %v, %v after %d asks", ok, err, asks)
	}
	s.be.Member = func(context.Context, string) (bool, error) { return false, errors.New("down") }
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := s.isMember(ctx, "p"); err == nil {
		t.Fatal("a catalog that stays unreadable must still be an error")
	}
}
