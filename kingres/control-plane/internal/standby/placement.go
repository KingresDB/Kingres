// SPDX-License-Identifier: AGPL-3.0-only

// Package standby creates, maintains and removes the standbys of the shard
// groups (design ha-phase5.md, part A): placement by the chained ring rule,
// base backups over the certificate-authenticated replication connection,
// physical slots, synchronous_standby_names with its grace period, slot
// retention limits, rebuilds, and the task that drives all of it from the
// cluster setting replicaCount.
//
// It does not touch the sharding catalog: kingres_shard 0.2 does not know
// standby nodes. The registration of a standby there is one step behind the
// CatalogRegistrar interface (catalog.go); until the extension can do it the step
// says so in the task detail.
package standby

import "sort"

// Server is a member of the ring: its id and its server group (rack or zone;
// empty when the server has none).
type Server struct {
	ID   string
	Zone string
}

func sameZone(a, b string) bool { return a != "" && a == b }

// Candidates lists the servers that may hold a standby of the group whose primary
// runs on primary, best first. The standbys of the group whose primary is on
// server i go to the next servers in the fixed ring order after i (wrapping), by
// the chained rule of the architecture ("Writes on any node"); servers of the
// primary's own server group are skipped while other choices exist, and a
// standby prefers a server group that no earlier standby of the group uses. The
// primary's own server is never a candidate: a server never runs two instances of
// one group.
func Candidates(ring []Server, primary string) []string {
	at := -1
	for i, s := range ring {
		if s.ID == primary {
			at = i
		}
	}
	var after []Server
	for k := 1; k <= len(ring); k++ {
		i := k - 1
		if at >= 0 {
			i = (at + k) % len(ring)
		}
		if ring[i].ID == primary {
			continue
		}
		after = append(after, ring[i])
	}
	pz := ""
	if at >= 0 {
		pz = ring[at].Zone
	}
	var out []string
	used := map[string]bool{}
	taken := make([]bool, len(after))
	pick := func(ok func(Server) bool) bool {
		for i, s := range after {
			if !taken[i] && ok(s) {
				taken[i] = true
				out = append(out, s.ID)
				if s.Zone != "" {
					used[s.Zone] = true
				}
				return true
			}
		}
		return false
	}
	for len(out) < len(after) {
		if pick(func(s Server) bool { return !sameZone(s.Zone, pz) && !(s.Zone != "" && used[s.Zone]) }) {
			continue
		}
		if pick(func(s Server) bool { return !sameZone(s.Zone, pz) }) {
			continue
		}
		pick(func(Server) bool { return true })
	}
	return out
}

// Plan decides what to add and what to remove so that a group has replicas
// standbys. candidates is the preference order of Candidates, existing the
// servers that run a standby of the group now. A standby that is already placed
// on a valid candidate stays (placement is stable: a server that joins does not
// move standbys that work); standbys on servers that are no candidates any more
// are removed; if there are too many, the least preferred go. Missing ones are
// taken from the front of the preference list. With fewer candidates than
// replicas the group simply gets fewer standbys (it is under-replicated, never a
// second instance on one server).
func Plan(candidates, existing []string, replicas int) (add, remove []string) {
	rank := map[string]int{}
	for i, c := range candidates {
		rank[c] = i
	}
	var valid []string
	for _, e := range existing {
		if _, ok := rank[e]; ok {
			valid = append(valid, e)
		} else {
			remove = append(remove, e)
		}
	}
	sort.SliceStable(valid, func(i, j int) bool { return rank[valid[i]] < rank[valid[j]] })
	keep := map[string]bool{}
	for i, v := range valid {
		if i < replicas {
			keep[v] = true
		} else {
			remove = append(remove, v)
		}
	}
	for _, c := range candidates {
		if len(keep) >= replicas {
			break
		}
		if !keep[c] {
			keep[c] = true
			add = append(add, c)
		}
	}
	sort.Strings(remove)
	return add, remove
}
