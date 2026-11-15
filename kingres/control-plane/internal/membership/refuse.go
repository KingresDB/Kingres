// SPDX-License-Identifier: PostgreSQL

package membership

import (
	"fmt"
	"strings"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
)

// bucketHold is the number of bucket copies (primary or replica) a server
// holds in one database.
type bucketHold struct {
	DB      string
	Buckets int
}

// removalFacts is what the refusal decision of removeServer and leaveCluster
// works from. It is plain data so that the decision can be tested with every
// combination.
type removalFacts struct {
	// Known: the server is a member of the cluster.
	Known bool
	// Dead: no instance of the server is active.
	Dead bool
	// Servers is the number of servers of the cluster, this one included.
	Servers int
	// HostsMapGroup: the server hosts an instance (primary or standby) of the
	// map group, shard group 0, the manager leader's catalog.
	HostsMapGroup bool
	// Holds lists the databases in which the server holds buckets.
	Holds []bucketHold
	// RebalanceRunning: a rebalance task is unfinished.
	RebalanceRunning bool
	// OperationRunning: another task of this server (add, remove, leave) is
	// unfinished.
	OperationRunning bool
}

// decideRemoval returns the problem that refuses removing server id, or nil.
// self tells that the server asks to leave itself (the wording differs). The
// order is the contract's, for removeServer and leaveCluster alike: unknown
// (404), dead (422, fail it over first), last_server, server_hosts_map_group,
// server_owns_buckets, rebalance_in_progress, operation_in_progress (409).
func decideRemoval(id string, self bool, f removalFacts) *problem.Problem {
	who := fmt.Sprintf("Server %q", id)
	if self {
		who = "This server"
	}
	switch {
	case !f.Known:
		return problem.New(problem.NotFound, fmt.Sprintf("Server %q does not exist.", id))
	case f.Dead:
		return problem.New(problem.NodeStateInvalid, who+" is dead; fail it over first (POST /servers/{serverId}/failover).")
	case f.Servers <= 1:
		return problem.New(problem.LastServer, who+" is the only server of the cluster.")
	case f.HostsMapGroup:
		return problem.New(problem.ServerHostsMapGroup, who+" hosts an instance of the map group (shard group 0, the manager leader's catalog). Move the map group first.")
	case len(f.Holds) > 0:
		parts := make([]string, len(f.Holds))
		total := 0
		for i, h := range f.Holds {
			parts[i] = fmt.Sprintf("%q (%d)", h.DB, h.Buckets)
			total += h.Buckets
		}
		return problem.New(problem.ServerOwnsBuckets, fmt.Sprintf(
			"%s still holds %d bucket copies in the databases %s. Rebalance them away from it first: PATCH /servers/{serverId} with shouldHaveShards false, then POST /rebalance.",
			who, total, strings.Join(parts, ", ")))
	case f.RebalanceRunning:
		return problem.New(problem.RebalanceInProgress, "A rebalance is running; wait until it has finished.")
	case f.OperationRunning:
		return problem.New(problem.OperationInProgress, who+" is already being added, removed or detached by another task.")
	}
	return nil
}
