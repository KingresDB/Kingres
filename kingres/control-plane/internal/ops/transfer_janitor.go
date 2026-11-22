// SPDX-License-Identifier: AGPL-3.0-only

package ops

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/staging"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/sysdb"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/transfer"
)

// SweepReport says what a pass of the janitor did.
type SweepReport struct {
	Expired, Deleted, Lost, Pruned int
	// Restored is true when the files show a higher id than the system database has issued: the database was restored from an older copy, so the
	// janitor deleted nothing and an operator must look.
	Restored bool
}

// TransferSweep is the janitor of the staged files (design "Where files live", limit 4). It runs on the manager leader every 5 minutes and once
// before the first transfer. It expires files whose time has passed, uploads that received nothing for 48 hours and files of roles that were dropped,
// deletes the files of rows that are done with, sets rows whose file is gone to lost, and hands the rest to the rules of the staging store (a name that
// matches the pattern, a regular file, older than an hour, no row or a row that says remove, and never after a restored database).
func (o *Ops) TransferSweep(ctx context.Context) (SweepReport, error) {
	var rep SweepReport
	x, err := o.xfer(ctx)
	if err != nil {
		return rep, err
	}
	var ready, receiving []int64
	rows := map[int64]staging.RowInfo{}
	var highest int64
	err = wrap(o.asService(ctx, sysdb.Database, func(tx pgx.Tx) error {
		tag, e := tx.Exec(ctx, `UPDATE kingres_cp.data_transfer d SET state = 'expired'
			WHERE d.server_id = $1 AND d.state IN ('ready', 'receiving')
			  AND (d.expires_at < now() OR (d.kind = 'upload' AND d.state = 'receiving' AND d.updated_at < now() - interval '48 hours'))`, o.ServerHost)
		if e != nil {
			return e
		}
		rep.Expired = int(tag.RowsAffected())
		// A role that was dropped or renamed does not keep its files: its OID no longer belongs to that name.
		tag, e = tx.Exec(ctx, `UPDATE kingres_cp.data_transfer d SET state = 'deleted'
			WHERE d.server_id = $1 AND d.state IN ('ready', 'receiving', 'building')
			  AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_roles r WHERE r.oid::bigint = d.created_by_oid AND r.rolname = d.created_by)`, o.ServerHost)
		if e != nil {
			return e
		}
		rep.Deleted = int(tag.RowsAffected())
		if e = tx.QueryRow(ctx, `SELECT coalesce(max(transfer_id), 0) FROM kingres_cp.data_transfer`).Scan(&highest); e != nil {
			return e
		}
		r, e := tx.Query(ctx, `SELECT transfer_id, state FROM kingres_cp.data_transfer WHERE server_id = $1`, o.ServerHost)
		if e != nil {
			return e
		}
		defer r.Close()
		for r.Next() {
			var id int64
			var st string
			if e := r.Scan(&id, &st); e != nil {
				return e
			}
			switch st {
			case transfer.StateReady:
				ready = append(ready, id)
				rows[id] = staging.RowInfo{}
			case transfer.StateReceiving:
				receiving = append(receiving, id)
				rows[id] = staging.RowInfo{}
			case transfer.StateBuilding:
				rows[id] = staging.RowInfo{}
			default:
				rows[id] = staging.RowInfo{Remove: true}
			}
		}
		return r.Err()
	}))
	if err != nil {
		return rep, err
	}
	sr, err := x.store.Sweep(staging.SweepInput{HighestID: highest, Rows: rows, ReadyIDs: ready})
	if errors.Is(err, staging.ErrRestoredDatabase) {
		rep.Restored = true
		o.Log.Error("the staging directory holds files with ids the system database never issued; it looks restored from an older copy, so no file is deleted until an operator looks at it")
		return rep, nil
	}
	if err != nil {
		return rep, err
	}
	// Uploads that are still being received need their file too; a missing one is lost.
	for _, id := range receiving {
		if st, _, serr := x.store.State(id); serr == nil && st == staging.StateNone {
			sr.Lost = append(sr.Lost, id)
		}
	}
	for _, id := range sr.Lost {
		o.setLost(ctx, id)
		x.forget(id)
		rep.Lost++
	}
	// A file of a row that is done with was deleted by the sweep; its reservation is freed.
	for id, ri := range rows {
		if ri.Remove {
			x.forget(id)
		}
	}
	rep.Deleted += len(sr.Deleted)
	// Old rows of finished transfers are not kept for ever.
	if perr := wrap(o.asService(ctx, sysdb.Database, func(tx pgx.Tx) error {
		tag, e := tx.Exec(ctx, `DELETE FROM kingres_cp.data_transfer WHERE server_id = $1 AND state IN ('deleted', 'failed', 'expired', 'lost') AND updated_at < now() - interval '7 days'
			AND transfer_id <> (SELECT max(transfer_id) FROM kingres_cp.data_transfer)`, o.ServerHost)
		rep.Pruned = int(tag.RowsAffected())
		return e
	})); perr != nil {
		o.Log.Warn("pruning the records of old transfers failed", "error", perr)
	}
	if o.Transfer != nil && o.Transfer.Audit != nil && (rep.Expired+rep.Deleted+rep.Lost > 0) {
		o.Transfer.Audit(ctx, "dataTransfer.purged", map[string]any{"expired": rep.Expired, "deleted": rep.Deleted, "lost": rep.Lost, "files": len(sr.Deleted)})
	}
	return rep, nil
}

// TransferStats is what the metrics and the alert of the data transfer read: the bytes the reservations hold, the free bytes of the file system of the staging
// directory, the quota, the running transfers and the files that are kept. ok is false on a server that never opened its staging directory (it holds no file).
func (o *Ops) TransferStats(ctx context.Context) (used, free, quota int64, running, staged int, ok bool) {
	o.xferMu.Lock()
	x := o.xferSt
	o.xferMu.Unlock()
	if x == nil {
		return 0, 0, 0, 0, 0, false
	}
	for _, h := range x.store.Holds() {
		used += h.Bytes
	}
	sp, err := x.store.Space()
	if err != nil {
		return 0, 0, 0, 0, 0, false
	}
	free = int64(sp.Free)
	quota = staging.DefaultQuota(sp.Total)
	if st, serr := o.settings(ctx); serr == nil && st.StagingQuotaBytes > 0 {
		quota = st.StagingQuotaBytes
	}
	_ = o.asService(ctx, sysdb.Database, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FILTER (WHERE state IN ('receiving', 'building'))::int, count(*) FILTER (WHERE state = 'ready')::int
			FROM kingres_cp.data_transfer WHERE server_id = $1`, o.ServerHost).Scan(&running, &staged)
	})
	return used, free, quota, running, staged, true
}
