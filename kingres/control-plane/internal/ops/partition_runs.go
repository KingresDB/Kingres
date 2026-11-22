// SPDX-License-Identifier: AGPL-3.0-only

package ops

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/sysdb"
)

// The runs of a time-partitioning policy (contract 0.17.0): the records of kingres_timepart (kingres.timepart_run and kingres.timepart_run_action) in the
// database of the table, with the task and the role that made each, which kingres_cp.partition_run_task knows.

const runColumns = `r.run_id, r.as_of, r.trigger, r.state, r.started_at, r.finished_at, r.started_by::text, r.error`

func scanRun(row pgx.Row) (api.PartitionRun, error) {
	var r api.PartitionRun
	var fin *time.Time
	if err := row.Scan(&r.ID, &r.AsOf, &r.Trigger, &r.State, &r.StartedAt, &fin, &r.StartedBy, &r.Error); err != nil {
		return r, err
	}
	r.AsOf, r.StartedAt, r.FinishedAt = r.AsOf.UTC(), r.StartedAt.UTC(), utc(fin)
	if r.Trigger == "sql" {
		r.Trigger = "manual" // a run made from SQL is a manual run to the console
	}
	r.Actions = []api.PartitionAction{}
	return r, nil
}

// ListPartitionPolicyRuns lists the runs of the policy of a table, newest first, without their actions. state filters; the cursor is the id of the last run.
func (o *Ops) ListPartitionPolicyRuns(ctx context.Context, db, schema, table, state string, limit int, cursor string) (api.PartitionRunPage, error) {
	page := api.PartitionRunPage{Items: []api.PartitionRun{}}
	if err := checkNames(db, schema, table); err != nil {
		return page, err
	}
	if limit < 1 {
		limit = 50
	}
	after := int64(1<<62 - 1)
	if cursor != "" {
		n, err := strconv.ParseInt(cursor, 10, 64)
		if err != nil || n < 1 {
			return page, problem.Validation("The cursor is invalid.", field("cursor", "was not issued by this server", "invalid"))
		}
		after = n
	}
	if ok, err := o.timepartInstalled(ctx, db); err != nil {
		return page, err
	} else if !ok {
		return page, nil // no extension, no runs
	}
	err := o.policyTx(ctx, db, func(tx pgx.Tx) error {
		if err := requirePartitioned(ctx, tx, schema, table); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT `+runColumns+` FROM kingres.timepart_run r WHERE r.schema_name = $1 AND r.table_name = $2 AND r.run_id < $3
			AND ($4 = '' OR r.state = $4) ORDER BY r.run_id DESC LIMIT $5`, schema, table, after, state, limit+1)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			r, err := scanRun(rows)
			if err != nil {
				return err
			}
			page.Items = append(page.Items, r)
		}
		return rows.Err()
	})
	if err != nil {
		return page, wrap(err)
	}
	if len(page.Items) > limit {
		page.Items = page.Items[:limit]
		c := strconv.FormatInt(page.Items[limit-1].ID, 10)
		page.NextCursor = &c
	}
	o.addRunTasks(ctx, db, page.Items)
	return page, nil
}

// GetPartitionPolicyRun reads one run with its actions, in the order they were taken.
func (o *Ops) GetPartitionPolicyRun(ctx context.Context, db, schema, table string, id int64) (api.PartitionRun, error) {
	var run api.PartitionRun
	if err := checkNames(db, schema, table); err != nil {
		return run, err
	}
	if ok, err := o.timepartInstalled(ctx, db); err != nil {
		return run, err
	} else if !ok {
		return run, problem.New(problem.NotFound, fmt.Sprintf("Table %s.%s has no run %d (kingres_timepart is not installed in this database).", schema, table, id))
	}
	err := o.policyTx(ctx, db, func(tx pgx.Tx) error {
		if err := requirePartitioned(ctx, tx, schema, table); err != nil {
			return err
		}
		r, err := scanRun(tx.QueryRow(ctx, `SELECT `+runColumns+` FROM kingres.timepart_run r WHERE r.schema_name = $1 AND r.table_name = $2 AND r.run_id = $3`, schema, table, id))
		if errors.Is(err, pgx.ErrNoRows) {
			return problem.New(problem.NotFound, fmt.Sprintf("Table %s.%s has no run %d.", schema, table, id))
		}
		if err != nil {
			return err
		}
		run = r
		rows, err := tx.Query(ctx, `SELECT a.seq, a.kind, a.partition_name::text, a.range_from, a.range_to, a.state, a.detail, a.bytes_before, a.bytes_after, a.started_at, a.finished_at
			FROM kingres.timepart_run_action a WHERE a.run_id = $1 AND a.kind <> 'misaligned' ORDER BY a.seq`, id)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var x api.PartitionAction
			var from, to, st, fin *time.Time
			if err := rows.Scan(&x.Seq, &x.Kind, &x.Partition, &from, &to, &x.State, &x.Detail, &x.BytesBefore, &x.BytesAfter, &st, &fin); err != nil {
				return err
			}
			x.RangeFrom, x.RangeTo, x.StartedAt, x.FinishedAt = utc(from), utc(to), utc(st), utc(fin)
			run.Actions = append(run.Actions, x)
		}
		return rows.Err()
	})
	if err != nil {
		return run, wrap(err)
	}
	items := []api.PartitionRun{run}
	o.addRunTasks(ctx, db, items)
	return items[0], nil
}

// addRunTasks sets TaskID and StartedBy of the runs that a task made: the role that asked, or system for the scheduler. A run that no task made (a run
// made from SQL) keeps the role of its session.
func (o *Ops) addRunTasks(ctx context.Context, db string, runs []api.PartitionRun) {
	if len(runs) == 0 || o.Service == nil {
		return
	}
	ids := make([]int64, len(runs))
	for i, r := range runs {
		ids[i] = r.ID
	}
	_ = o.asService(ctx, sysdb.Database, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT run_id, task_id, started_by FROM kingres_cp.partition_run_task WHERE database_name = $1 AND run_id = ANY($2)`, db, ids)
		if err != nil {
			return err
		}
		defer rows.Close()
		by := map[int64][2]any{}
		for rows.Next() {
			var run, tk int64
			var who string
			if err := rows.Scan(&run, &tk, &who); err != nil {
				return err
			}
			by[run] = [2]any{tk, who}
		}
		for i := range runs {
			if v, ok := by[runs[i].ID]; ok {
				tk := v[0].(int64)
				runs[i].TaskID, runs[i].StartedBy = &tk, v[1].(string)
			}
		}
		return rows.Err()
	})
}
