// SPDX-License-Identifier: PostgreSQL

package pgstore

import (
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ha"
)

// leaseView is the lease of a group as the answering server knows it: held or
// lost by this server when it runs the group's primary, granted by this server
// to another holder otherwise. Nil when the server knows nothing about the group.
func leaseView(reg *ha.Registry, group int32) *api.GroupLease {
	for _, li := range reg.Leases().Info() {
		if li.Group != group {
			continue
		}
		l := &api.GroupLease{Holder: li.Holder, State: li.State, Enforced: li.Enforced}
		if li.State == "held" || li.State == "granted" {
			ms := li.RemainingMillis
			l.RemainingMillis = &ms
		}
		return l
	}
	return nil
}
