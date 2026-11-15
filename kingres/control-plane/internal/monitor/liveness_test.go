// SPDX-License-Identifier: PostgreSQL

package monitor

import (
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ha"
)

func TestPeerLivenessConditions(t *testing.T) {
	for _, tc := range []struct {
		state ha.PeerState
		kind  string
	}{{ha.PeerLive, ""}, {ha.PeerUnknown, ""}, {ha.PeerSuspect, "serverSuspect"}, {ha.PeerDead, "serverDown"}} {
		got := PeerLivenessConditions("b", tc.state, 12*time.Second)
		if tc.kind == "" {
			if len(got) != 0 {
				t.Fatalf("state %v: want no condition, got %v", tc.state, got)
			}
			continue
		}
		if len(got) != 1 || got[0].Kind != tc.kind || got[0].Subject != "server:b" {
			t.Fatalf("state %v: %v", tc.state, got)
		}
	}
}
