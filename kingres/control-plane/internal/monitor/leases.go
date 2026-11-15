// SPDX-License-Identifier: PostgreSQL

package monitor

import "fmt"

// LeaseReading is the lease of one shard group whose primary this server runs.
type LeaseReading struct {
	Group int32
	// Lost is true when the lease could not be renewed in time.
	Lost bool
	// Enforced says the primary is stopped when the lease is lost (a group with a
	// standby); otherwise it keeps running and the alert is the only effect.
	Enforced bool
}

// leaseConditions raises leaseLost for every group whose lease this server lost.
// The alert of an enforced group is raised only if the instance runs to store it
// (an enforced loss stops the instance); serverDown tells the rest.
func (r Readings) leaseConditions() (conds []Condition) {
	for _, l := range r.Leases {
		if !l.Lost {
			continue
		}
		msg := fmt.Sprintf("Server %s lost the lease of shard group %d: no majority of the servers renewed it in time. ", r.Host, l.Group)
		if l.Enforced {
			msg += "The primary was stopped so that no second primary can exist."
		} else {
			msg += "The group has no standby to promote, so the primary keeps running; writes it accepts are not protected by a lease."
		}
		conds = append(conds, Condition{"leaseLost", "critical", serverSubject(r.Host, fmt.Sprintf("group:%d", l.Group)), msg})
	}
	return conds
}
