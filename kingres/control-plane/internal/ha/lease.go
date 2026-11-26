// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package ha

// LeaseInfo is the state of the lease of one shard group as this server sees it.
type LeaseInfo struct {
	// Group is the shard group id.
	Group int32 `json:"group"`
	// Holder is the server that holds the lease (as far as this server knows).
	Holder string `json:"holder"`
	// State is "held" (this server holds it and it is valid), "lost" (this server
	// ran the primary and lost the lease), "granted" (this server granted it to
	// Holder), or "none".
	State string `json:"state"`
	// RemainingMillis is the time left, measured on this server's monotonic clock.
	RemainingMillis int64 `json:"remainingMillis"`
	// Enforced tells whether losing the lease stops the instance (a group with a
	// standby) or only raises an alert (a group without one).
	Enforced bool `json:"enforced"`
}

// Leases is the lease manager of this server.
type Leases interface {
	// Holds tells whether this server holds a valid lease for the group right
	// now, with the safety margin applied (so a true answer is safe to act on).
	Holds(group int32) bool
	// Info lists the leases this server knows of.
	Info() []LeaseInfo
}

type noLeases struct{}

func (noLeases) Holds(int32) bool  { return false }
func (noLeases) Info() []LeaseInfo { return nil }
