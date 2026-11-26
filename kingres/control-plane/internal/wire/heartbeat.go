// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package wire

// HeartbeatRequest is the body of POST /heartbeat. It carries no time of any
// kind: the receiver notes when the call arrived on its own monotonic clock.
type HeartbeatRequest struct {
	// IntervalMillis is how often the sender intends to send (a duration).
	IntervalMillis int64 `json:"intervalMillis"`
	// UptimeSeconds is how long the sender's kingresd has run (a duration).
	UptimeSeconds int64 `json:"uptimeSeconds"`
}

// HeartbeatResponse is the answer: the receiver's own server id and uptime, which
// the sender takes as proof that the receiver is alive.
type HeartbeatResponse struct {
	ServerID      string `json:"serverId"`
	UptimeSeconds int64  `json:"uptimeSeconds"`
}
