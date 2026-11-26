// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package monitor

import (
	"fmt"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ha"
)

// LivenessKinds are the alert kinds the manager leader owns for the other
// servers: a server that has sent no heartbeat is serverSuspect, then serverDown.
// Reconcile them for every server with these kinds as the evaluated set, so that
// an alert clears when the server is heard again.
var LivenessKinds = map[string]bool{"serverDown": true, "serverSuspect": true}

// PeerLivenessConditions returns the condition about another server that the
// heartbeats show (none while it is live or unknown). The subject is the one of
// every alert about that server, "server:<id>".
func PeerLivenessConditions(server string, state ha.PeerState, silence time.Duration) []Condition {
	subject := "server:" + server
	secs := int64(silence.Round(time.Second) / time.Second)
	switch state {
	case ha.PeerDead:
		return []Condition{{"serverDown", "critical", subject,
			fmt.Sprintf("Server %s has sent no heartbeat for %d seconds and is considered dead.", server, secs)}}
	case ha.PeerSuspect:
		return []Condition{{"serverSuspect", "warning", subject,
			fmt.Sprintf("Server %s has sent no heartbeat for %d seconds.", server, secs)}}
	}
	return nil
}
