// SPDX-License-Identifier: AGPL-3.0-only

package rebalance

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// PauseForMapGroupFailover pauses a rebalance that the old leader left pending or running, with direct conditional updates and
// without the engine (design ha-phase5-part2.md, 3.4, B7): the new leader's database is the copy the standby replayed up to the
// loss, the old leader's run is gone, migration 0009 would refuse the failover task while the rebalance is active, and the engine
// must not resume a rebalance in the middle of a promotion. The run is recorded as stopped by the safety rule, as the stop of a
// failover does; the extension's recovery settles the move in flight when the rebalance is started again. It returns the number of
// rebalances paused, and is repeated harmlessly.
func PauseForMapGroupFailover(ctx context.Context, c *pgx.Conn) (int, error) {
	const text = "The map group was failed over."
	tx, err := c.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := tx.Exec(ctx, `UPDATE kingres_cp.rebalance_run SET outcome = 'stopped', stop_requested = true, stopped_by = 'safetyRule', error = $1, finished_at = NULL
		WHERE outcome IS NULL AND task_id IN (SELECT id FROM kingres_cp.task WHERE kind = 'rebalance' AND state IN ('pending', 'running'))`, text); err != nil {
		return 0, err
	}
	tag, err := tx.Exec(ctx, `UPDATE kingres_cp.task SET state = 'paused', lease_holder = NULL, lease_expires_at = NULL, updated_at = clock_timestamp(),
		progress = jsonb_set(progress, '{detail}', to_jsonb($1::text))
		WHERE kind = 'rebalance' AND state IN ('pending', 'running')`, text)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}
