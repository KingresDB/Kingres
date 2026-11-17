// SPDX-License-Identifier: AGPL-3.0-only

package ha

import "context"

// Standby is one standby instance kingresd manages, as the read side needs it.
type Standby struct {
	// ID is the cluster-wide standby id (also in the slot and application names).
	ID int64
	// Group is the shard group; ServerID the server that runs the standby.
	Group    int32
	ServerID string
	// Port is the port of the standby instance on that server.
	Port int
	// CatalogNode is the node id of the standby in the sharding catalog, 0 until it is
	// registered; the replication view lists the catalog's nodes and uses it to find
	// the standby's walsender.
	CatalogNode int32
	// State is the process state of the instance.
	State string
	// Streaming says whether the standby is connected to its primary.
	Streaming bool
	// SyncState is the primary's view: async, potential, sync, quorum, or "".
	SyncState string
	// LagBytes and LagSeconds are the replay lag; nil when unknown.
	LagBytes   *int64
	LagSeconds *float64
}

// Standbys is the standby manager as the read side sees it.
type Standbys interface {
	// List returns the standbys kingresd manages, in a stable order.
	List(ctx context.Context) ([]Standby, error)
	// HasStandby tells, from what this server knows locally (never a network call),
	// whether the shard group whose primary this server runs has at least one
	// standby planned or running. The lease manager enforces self-fencing only then.
	HasStandby(group int32) bool
}

type noStandbys struct{}

func (noStandbys) List(context.Context) ([]Standby, error) { return nil, nil }
func (noStandbys) HasStandby(int32) bool                   { return false }
