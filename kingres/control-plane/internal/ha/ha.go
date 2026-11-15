// SPDX-License-Identifier: AGPL-3.0-only

// Package ha holds the small interfaces through which the features of high
// availability know each other: liveness (heartbeats), leases, standbys and the
// client entry point. They are separate packages with one job each; this one
// only lets them meet without importing each other (the store, for example,
// needs liveness and standbys to answer the server and replication views, and
// the client entry point needs the leases).
//
// A Registry is created once in main; the feature that owns a slot sets it at
// start, readers use the accessors, which never return nil (a feature that is
// not running answers "unknown" or "nothing").
package ha

import "sync"

// Registry connects the features. It is safe for concurrent use.
type Registry struct {
	mu       sync.RWMutex
	liveness Liveness
	leases   Leases
	standbys Standbys
}

// NewRegistry returns a registry whose slots answer "nothing known".
func NewRegistry() *Registry { return &Registry{} }

// SetLiveness installs the heartbeat tracker.
func (r *Registry) SetLiveness(l Liveness) { r.mu.Lock(); r.liveness = l; r.mu.Unlock() }

// SetLeases installs the lease manager.
func (r *Registry) SetLeases(l Leases) { r.mu.Lock(); r.leases = l; r.mu.Unlock() }

// SetStandbys installs the standby manager.
func (r *Registry) SetStandbys(s Standbys) { r.mu.Lock(); r.standbys = s; r.mu.Unlock() }

// Liveness returns the heartbeat tracker, or one that knows nothing.
func (r *Registry) Liveness() Liveness {
	if r == nil {
		return noLiveness{}
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.liveness == nil {
		return noLiveness{}
	}
	return r.liveness
}

// Leases returns the lease manager, or one that holds nothing.
func (r *Registry) Leases() Leases {
	if r == nil {
		return noLeases{}
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.leases == nil {
		return noLeases{}
	}
	return r.leases
}

// Standbys returns the standby manager, or one that has none.
func (r *Registry) Standbys() Standbys {
	if r == nil {
		return noStandbys{}
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.standbys == nil {
		return noStandbys{}
	}
	return r.standbys
}
