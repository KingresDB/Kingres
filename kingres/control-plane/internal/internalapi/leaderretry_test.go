// SPDX-License-Identifier: AGPL-3.0-only

package internalapi

import (
	"context"
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
