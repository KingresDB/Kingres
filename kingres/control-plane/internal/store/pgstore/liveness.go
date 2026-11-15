// SPDX-License-Identifier: AGPL-3.0-only

package pgstore

import (
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ha"
)

// withLiveness puts what the heartbeats say over the status the catalog gives:
// a server that has been silent for the suspect timeout is "suspect", one that
// has been silent for the failure timeout is "dead", and when it is heard again
// the catalog's status shows again (nothing is stored, so nothing has to be
// undone). A server the catalog already calls dead stays dead. This server is
// never silent to itself, and without a heartbeat tracker the catalog decides.
//
// Both the server list and the counts of the cluster overview use this one
// function on the same catalog snapshot, so they cannot disagree.
func (s *Store) withLiveness(id, base string) string {
	if base == "dead" {
		return base
	}
	switch s.opts.HA.Liveness().State(id) {
	case ha.PeerDead:
		return "dead"
	case ha.PeerSuspect:
		return "suspect"
	}
	return base
}

// lastHeartbeat is the time of the last heartbeat from a server, nil when none
// was heard. The time is derived from a monotonic age for display only.
func (s *Store) lastHeartbeat(id string) *time.Time {
	t, ok := s.opts.HA.Liveness().LastHeard(id)
	if !ok {
		return nil
	}
	t = t.UTC()
	return &t
}
