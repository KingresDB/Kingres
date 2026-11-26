// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package standby

import (
	"fmt"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/instance"
)

// Entry is one standby of the cluster as every server knows it. The leader keeps
// the authoritative records in kingres_cp.standby and pushes the whole list to
// every server (POST /standby/plan): a server needs the list for its own primary
// (slots, synchronous_standby_names) and for the replication view, and must not
// need the leader for either.
type Entry struct {
	// ID is the cluster-wide standby id.
	ID int64 `json:"id"`
	// Group is the shard group; Server the server that runs the standby;
	// PrimaryServer the server that runs the group's primary (its host in the
	// catalog) and PrimaryPort that instance's port.
	Group         int32  `json:"group"`
	Server        string `json:"server"`
	PrimaryServer string `json:"primaryServer"`
	PrimaryPort   int    `json:"primaryPort"`
	// Port is the port of the standby instance on Server (0 until created).
	Port int `json:"port,omitempty"`
	// Ready is true once the standby exists and has been started: from then on its
	// name belongs in synchronous_standby_names once it streams.
	Ready bool `json:"ready"`
	// Generation counts the rebuilds of this standby: a plan with a higher
	// generation than the one whose slot was lost clears the "needs a rebuild" mark.
	Generation int64 `json:"generation"`
	// CatalogNode is the node id of the standby in the sharding catalog (kingres.node),
	// 0 until it is registered. Its primary names it in synchronous_standby_names only
	// from then on (kingres_shard 0.5, sequence 1: the name is checked against the
	// registered standby of the group).
	CatalogNode int32 `json:"catalogNode,omitempty"`
	// InstanceID is the directory id of the standby instance on its server (0: the
	// directory of the standby of the group, instance.StandbyDirID). A directory keeps its
	// id for life; the role file says what it serves.
	InstanceID int `json:"instanceId,omitempty"`
}

// SlotName is the physical slot of a standby on its primary; AppName the
// application name its walreceiver reports, which is what
// synchronous_standby_names lists and what the replication view matches. They are
// the same string.
func SlotName(id int64) string { return fmt.Sprintf("kingres_standby_%d", id) }

// AppName is the application name of a standby's walreceiver.
func AppName(id int64) string { return SlotName(id) }

// PlanRequest is the body of POST /standby/plan: the complete list of the
// cluster's standbys, with a version that grows with every change (an older plan
// never replaces a newer one) and the replica count it was made for.
type PlanRequest struct {
	Version  int64   `json:"version"`
	Replicas int     `json:"replicas"`
	Standbys []Entry `json:"standbys"`
}

// CreateRequest is the body of POST /standby/create: create (or finish creating)
// this standby on the receiving server.
type CreateRequest struct {
	Entry Entry `json:"entry"`
}

// CreateResponse is the answer: the port the instance listens on.
type CreateResponse struct {
	// InstanceID is the directory the standby is in, when the server found it (the former primary of a group: the
	// leader did not know its directory); OldPrimary what the rebuild of an old primary found and did.
	InstanceID int             `json:"instanceId,omitempty"`
	OldPrimary *OldPrimaryInfo `json:"oldPrimary,omitempty"`
	Port       int             `json:"port"`
}

// RemoveRequest is the body of POST /standby/remove: stop and delete the
// instance of the standby and drop its slot on the primary.
type RemoveRequest struct {
	Entry Entry `json:"entry"`
}

// RebuildRequest is the body of POST /standby/rebuild: make the standby again from
// the primary. Diverged says that the standby's history may have left the
// primary's (a former primary), where pg_rewind is tried before a new base backup.
type RebuildRequest struct {
	Entry    Entry `json:"entry"`
	Diverged bool  `json:"diverged,omitempty"`
	// Term is the catalog term the leader asks under: the old primary of a group is rebuilt only when it is newer than
	// the one the role file of its directory was written under.
	Term int64 `json:"term,omitempty"`
}

// LocalStatus is what a server knows of one standby instance it runs.
type LocalStatus struct {
	ID         int64 `json:"id"`
	Group      int32 `json:"group"`
	Port       int   `json:"port"`
	Running    bool  `json:"running"`
	Generation int64 `json:"generation"`
	// ReplayLagBytes is the WAL the standby received but has not replayed; nil when unknown.
	ReplayLagBytes *int64 `json:"replayLagBytes,omitempty"`
	// Receiving is the state of the walreceiver ("streaming", "stopped", ...).
	Receiving string `json:"receiving,omitempty"`
}

// SlotStatus is a standby's slot on the primary of this server.
type SlotStatus struct {
	ID            int64 `json:"id"`
	RetainedBytes int64 `json:"retainedBytes"`
	Active        bool  `json:"active"`
	Lost          bool  `json:"lost,omitempty"`
}

// PrimaryStatus is what the server that runs a group's primary knows of the
// group's standbys.
type PrimaryStatus struct {
	Group int32 `json:"group"`
	// SynchronousStandbyNames is the setting now; Without says that the group runs
	// without a synchronous standby (the grace period has passed).
	SynchronousStandbyNames string `json:"synchronousStandbyNames"`
	// FlushLsn is the WAL position the primary had flushed when the agent looked (X/Y form): the last position of
	// the old primary that a failover knows.
	FlushLsn string `json:"flushLsn,omitempty"`
	Without  bool   `json:"without"`
	// Streaming are the ids of the standbys that stream.
	Streaming []int64      `json:"streaming"`
	Slots     []SlotStatus `json:"slots"`
	// NeedsRebuild are the standbys whose slot was lost or dropped: they must be
	// made again from a base backup.
	NeedsRebuild []int64 `json:"needsRebuild"`
	// Planned is the number of standbys of the group in the plan this server holds: a primary whose server holds none is
	// not stopped by a lapsed lease (F3-02), and the leader records what it was told.
	Planned int `json:"planned"`
}

// Status is the answer of GET /standby/status.
type Status struct {
	Server      string         `json:"server"`
	PlanVersion int64          `json:"planVersion"`
	Standbys    []LocalStatus  `json:"standbys"`
	Primary     *PrimaryStatus `json:"primary,omitempty"`
	// Primaries are the primaries this server runs, one per group (after a failover a
	// server can run more than one); Primary is the first of them, for a leader of an
	// earlier version.
	Primaries []PrimaryStatus `json:"primaries,omitempty"`
	// Rounds counts the passes of the agent over this server's primaries since it started; a test
	// or a step that waits for "the agent has looked again" compares two readings.
	Rounds int64 `json:"rounds"`
}

// dir is the instance directory id of the standby on its server.
func (e Entry) dir() int {
	if e.InstanceID > 0 {
		return e.InstanceID
	}
	return instance.StandbyDirID(e.Group)
}

// primaryOf is the status of the primary of group that this server runs, or nil. A
// status from a server of an earlier version has only Primary.
func (s Status) primaryOf(group int32) *PrimaryStatus {
	for i := range s.Primaries {
		if s.Primaries[i].Group == group {
			return &s.Primaries[i]
		}
	}
	if len(s.Primaries) == 0 && s.Primary != nil && s.Primary.Group == group {
		return s.Primary
	}
	return nil
}
