// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ops"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/partsched"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/scheduler"
)

// addPartitionJobs adds the jobs of the time-partitioning scheduler (internal/partsched) to the scheduler of the manager leader: they start the run of each
// policy that is due, and raise the alerts partitionsAheadLow and partitionPolicyFailed.
func addPartitionJobs(sched *scheduler.Scheduler, o *ops.Ops, sys func(ctx context.Context, f func(c *pgx.Conn) error) error, interval time.Duration, log *slog.Logger) error {
	if o == nil {
		return nil
	}
	ps, err := partsched.New(partsched.Config{
		Backend: ops.PartitionBackend{O: o}, Alerts: sysAlerts{sys: sys}, Interval: interval, AlertEvery: 5 * interval, Log: log.With("component", "partsched"),
	})
	if err != nil {
		return err
	}
	for _, j := range ps.Jobs() {
		if err := sched.Add(j); err != nil {
			return err
		}
	}
	return nil
}

// sysAlerts keeps the alerts of the partition jobs in the alert table of the system database, as the other jobs of the leader do.
type sysAlerts struct {
	sys func(ctx context.Context, f func(c *pgx.Conn) error) error
}

func (a sysAlerts) Open(ctx context.Context, kinds []string) ([]partsched.OpenAlert, error) {
	var out []partsched.OpenAlert
	err := a.sys(ctx, func(c *pgx.Conn) error {
		rows, err := c.Query(ctx, `SELECT kind, subject FROM kingres_cp.alert WHERE cleared_at IS NULL AND kind = ANY($1)`, kinds)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var o partsched.OpenAlert
			if err := rows.Scan(&o.Kind, &o.Subject); err != nil {
				return err
			}
			out = append(out, o)
		}
		return rows.Err()
	})
	return out, err
}

func (a sysAlerts) Raise(ctx context.Context, kind, severity, subject, message string) error {
	return a.sys(ctx, func(c *pgx.Conn) error {
		_, err := c.Exec(ctx, `INSERT INTO kingres_cp.alert (severity, kind, subject, message, raised_at)
			VALUES ($1, $2, $3, $4, now()) ON CONFLICT (kind, subject) WHERE cleared_at IS NULL DO UPDATE SET message = $4, severity = $1`, severity, kind, subject, message)
		return err
	})
}

func (a sysAlerts) Clear(ctx context.Context, kind, subject string) error {
	return a.sys(ctx, func(c *pgx.Conn) error {
		_, err := c.Exec(ctx, `UPDATE kingres_cp.alert SET cleared_at = now() WHERE kind = $1 AND subject = $2 AND cleared_at IS NULL`, kind, subject)
		return err
	})
}

// partitionMetrics writes the counters of the runs of the policies in the Prometheus text format (they count what this server did as the leader).
func partitionMetrics(o *ops.Ops) func(w io.Writer) {
	return func(w io.Writer) {
		runs, bad, failed := o.PartitionMetrics()
		fmt.Fprintf(w, "# HELP kingres_partition_runs_total Runs of time-partitioning policies finished by this server as the leader.\n# TYPE kingres_partition_runs_total counter\nkingres_partition_runs_total %d\n", runs)
		fmt.Fprintf(w, "# HELP kingres_partition_runs_failed_total Runs that ended failed or partial.\n# TYPE kingres_partition_runs_failed_total counter\nkingres_partition_runs_failed_total %d\n", bad)
		fmt.Fprintf(w, "# HELP kingres_partition_actions_failed_total Actions of runs that failed (a lock timeout, a refusal).\n# TYPE kingres_partition_actions_failed_total counter\nkingres_partition_actions_failed_total %d\n", failed)
	}
}
