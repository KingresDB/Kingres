// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package pgstore

import (
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ha"
)

type fakeLiveness map[string]ha.PeerState

func (f fakeLiveness) State(id string) ha.PeerState         { return f[id] }
func (f fakeLiveness) LastHeard(string) (time.Time, bool)   { return time.Time{}, false }
func (f fakeLiveness) Silence(string) (time.Duration, bool) { return 0, false }

func TestHeartbeatsOverlayTheCatalogStatus(t *testing.T) {
	reg := ha.NewRegistry()
	s := &Store{opts: Options{HA: reg}}
	if got := s.withLiveness("b", "live"); got != "live" {
		t.Fatalf("without a tracker the catalog decides, got %s", got)
	}
	reg.SetLiveness(fakeLiveness{"a": ha.PeerLive, "b": ha.PeerSuspect, "c": ha.PeerDead, "d": ha.PeerUnknown})
	for _, tc := range []struct{ id, base, want string }{
		{"a", "pendingRebalance", "pendingRebalance"},
		{"b", "live", "suspect"},
		{"c", "draining", "dead"},
		{"d", "live", "live"},
		{"b", "dead", "dead"},
	} {
		if got := s.withLiveness(tc.id, tc.base); got != tc.want {
			t.Errorf("%s with base %s: %s, want %s", tc.id, tc.base, got, tc.want)
		}
	}
	// Heard again: the catalog status shows again, nothing was stored.
	reg.SetLiveness(fakeLiveness{"c": ha.PeerLive})
	if got := s.withLiveness("c", "draining"); got != "draining" {
		t.Errorf("after the server was heard again: %s", got)
	}
}
