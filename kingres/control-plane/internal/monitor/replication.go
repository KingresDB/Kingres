// SPDX-License-Identifier: AGPL-3.0-only

package monitor

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// ReplicationWarnAfter and ReplicationCritAfter are how long a server may stay
// behind on changes to roles, grants and network rules before the alert
// replicationPending is a warning and then critical. A role that was disabled or
// dropped still works on a server that has not heard of it (CP2-07), so the
// alert is meant to be acted on, not read later.
const (
	ReplicationWarnAfter = time.Minute
	ReplicationCritAfter = 10 * time.Minute
)

// ReplicationReading is a server that has not applied changes the leader made:
// the number of replication tasks that still wait for it and the creation time
// of the oldest of them.
type ReplicationReading struct {
	Server string
	Tasks  int
	Since  time.Time
}

// ReadReplication lists the servers that replication tasks (kind replicate.apply,
// one step per server) still wait for. It reads the task table of the system
// database it is connected to, which only the manager leader keeps current.
func ReadReplication(ctx context.Context, c *pgx.Conn) ([]ReplicationReading, error) {
	rows, err := c.Query(ctx, `
		SELECT regexp_replace(st->>'name', '^Bring server (.*) up to date$', '\1') AS server,
		       count(DISTINCT t.id)::int, min(t.created_at)
		  FROM kingres_cp.task t, jsonb_array_elements(t.progress->'steps') AS st
		 WHERE t.kind = 'replicate.apply' AND t.state <> 'done' AND st->>'state' <> 'done'
		 GROUP BY 1 ORDER BY 1`)
	if err != nil {
		return nil, fmt.Errorf("reading the replication tasks: %w", err)
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (ReplicationReading, error) {
		var r ReplicationReading
		err := row.Scan(&r.Server, &r.Tasks, &r.Since)
		return r, err
	})
}

func (r Readings) replicationConditions() (conds []Condition) {
	for _, p := range r.Replication {
		waited := r.Now.Sub(p.Since)
		if waited < ReplicationWarnAfter {
			continue
		}
		sev := "warning"
		if waited >= ReplicationCritAfter {
			sev = "critical"
		}
		conds = append(conds, Condition{"replicationPending", sev, serverSubject(r.Host, "behind:"+p.Server),
			fmt.Sprintf("Server %s has not applied %d change(s) to roles, grants or settings for %s: a role that was changed, disabled or dropped may still work there.",
				p.Server, p.Tasks, waited.Round(time.Second))})
	}
	return conds
}
