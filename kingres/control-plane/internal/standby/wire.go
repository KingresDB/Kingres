// SPDX-License-Identifier: PostgreSQL

package standby

import "fmt"

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
	Port int `json:"port"`
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
	Without                 bool   `json:"without"`
	// Streaming are the ids of the standbys that stream.
	Streaming []int64      `json:"streaming"`
	Slots     []SlotStatus `json:"slots"`
	// NeedsRebuild are the standbys whose slot was lost or dropped: they must be
	// made again from a base backup.
	NeedsRebuild []int64 `json:"needsRebuild"`
}

// Status is the answer of GET /standby/status.
type Status struct {
	Server      string         `json:"server"`
	PlanVersion int64          `json:"planVersion"`
	Standbys    []LocalStatus  `json:"standbys"`
	Primary     *PrimaryStatus `json:"primary,omitempty"`
}
