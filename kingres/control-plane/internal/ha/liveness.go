// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package ha

import "time"

// PeerState is what this server believes of another server's liveness.
type PeerState int

// The states. Unknown means no heartbeat has been expected yet (a server that
// joined a moment ago, or a tracker that is not running): it is never shown as
// dead.
const (
	PeerUnknown PeerState = iota
	PeerLive
	PeerSuspect
	PeerDead
)

// Liveness is the heartbeat tracker of this server.
type Liveness interface {
	// State is the state of a server as this server sees it.
	State(server string) PeerState
	// LastHeard is the wall-clock time of the last heartbeat from a server, derived
	// for display from a monotonic age; false when none was ever heard.
	LastHeard(server string) (time.Time, bool)
	// Silence is how long (monotonic) a server has been silent; false for a server
	// that is not expected, and for this server itself.
	Silence(server string) (time.Duration, bool)
}

type noLiveness struct{}

func (noLiveness) State(string) PeerState               { return PeerUnknown }
func (noLiveness) LastHeard(string) (time.Time, bool)   { return time.Time{}, false }
func (noLiveness) Silence(string) (time.Duration, bool) { return 0, false }
