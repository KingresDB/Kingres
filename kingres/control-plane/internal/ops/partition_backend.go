// SPDX-License-Identifier: AGPL-3.0-only

package ops

import (
	"context"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/partsched"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/sysdb"
)

// PartitionBackend gives the scheduler of the time-partitioning policies (internal/partsched) the view of this server's instance: the policies that are
// due, the findings of the check, and the start of a run. It reads every database of the instance that has kingres_timepart. A plain partitioned table
// lives in the instance of this server, where the leader runs (see the storage operations).
type PartitionBackend struct{ O *Ops }

var _ partsched.Backend = PartitionBackend{}

// databasesWithTimepart lists the databases of the instance in which kingres_timepart is installed.
func (b PartitionBackend) databasesWithTimepart(ctx context.Context) ([]string, error) {
	o := b.O
	var names []string
	err := o.asService(ctx, "postgres", func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT datname::text FROM pg_database WHERE datallowconn AND NOT datistemplate AND datname <> $1 ORDER BY datname`, sysdb.Database)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var n string
			if err := rows.Scan(&n); err != nil {
				return err
			}
			names = append(names, n)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, wrap(err)
	}
	var out []string
	for _, n := range names {
		if protectedDB[n] && n != "postgres" {
			continue
		}
		v, err := o.ext().Installed(ctx, n, ExtTimepart)
		if err != nil {
			continue // a database that cannot be opened now is looked at again at the next tick
		}
		if v != "" {
			out = append(out, n)
		}
	}
	return out, nil
}

// Due implements partsched.Backend.
func (b PartitionBackend) Due(ctx context.Context, asOf time.Time) ([]partsched.Due, error) {
	dbs, err := b.databasesWithTimepart(ctx)
	if err != nil {
		return nil, err
	}
	var out []partsched.Due
	for _, db := range dbs {
		err := b.O.asService(ctx, db, func(tx pgx.Tx) error {
			rows, err := tx.Query(ctx, `SELECT schema_name::text, table_name::text FROM kingres.timepart_due($1)`, asOf)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				d := partsched.Due{Database: db}
				if err := rows.Scan(&d.Schema, &d.Table); err != nil {
					return err
				}
				out = append(out, d)
			}
			return rows.Err()
		})
		if err != nil {
			b.O.Log.Warn("asking a database which policies are due failed", "database", db, "error", err)
		}
	}
	return out, nil
}

// Start implements partsched.Backend.
func (b PartitionBackend) Start(ctx context.Context, d partsched.Due, asOf time.Time) (bool, error) {
	return b.O.StartMaintain(ctx, d.Database, d.Schema, d.Table, asOf)
}

// Checks implements partsched.Backend. A run that was partial counts as a failure for the alert only when the run before it was partial too. It also returns the
// databases it read, so that an alert of a database that could not be read is left as it is.
func (b PartitionBackend) Checks(ctx context.Context) ([]partsched.Check, []string, error) {
	dbs, err := b.databasesWithTimepart(ctx)
	if err != nil {
		return nil, nil, err
	}
	var out []partsched.Check
	var read []string
	for _, db := range dbs {
		err := b.O.asService(ctx, db, func(tx pgx.Tx) error {
			rows, err := tx.Query(ctx, `SELECT schema_name::text, table_name::text, problem, coalesce(detail, '') FROM kingres.timepart_check()`)
			if err != nil {
				return err
			}
			var list []partsched.Check
			for rows.Next() {
				c := partsched.Check{Database: db}
				if err := rows.Scan(&c.Schema, &c.Table, &c.Problem, &c.Detail); err != nil {
					rows.Close()
					return err
				}
				list = append(list, c)
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return err
			}
			for _, c := range list {
				if c.Problem == "last_run_failed" && strings.HasPrefix(c.Detail, "partial") {
					var prev string
					err := tx.QueryRow(ctx, `SELECT state FROM kingres.timepart_run r WHERE r.schema_name = $1 AND r.table_name = $2 AND r.state <> 'running'
						ORDER BY r.run_id DESC OFFSET 1 LIMIT 1`, c.Schema, c.Table).Scan(&prev)
					if !secondPartial(prev, err) {
						continue
					}
				}
				out = append(out, c)
			}
			return nil
		})
		if err != nil {
			b.O.Log.Warn("reading the check of the policies of a database failed", "database", db, "error", err)
			continue
		}
		read = append(read, db)
	}
	return out, read, nil
}

// secondPartial says whether the run before the last one was partial too: one partial run is a lock that was held, two in a row is a policy that does not get
// its work done, and only that is an alert.
func secondPartial(prev string, err error) bool { return err == nil && prev == "partial" }
