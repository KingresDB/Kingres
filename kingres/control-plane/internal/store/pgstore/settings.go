// SPDX-License-Identifier: AGPL-3.0-only

package pgstore

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// dbSetting is a row of kingres_cp.database_setting: what the control plane
// recorded when it created or changed a database.
type dbSetting struct {
	name       string
	sharding   bool
	bucketCnt  *int
	durability string
	ledgerMode string
	digest     *int
	createdAt  time.Time
	createdBy  string
}

// dbSettings reads the recorded settings by database name. Without the
// system schema (a node that is not set up) the map is empty.
func (s *Store) dbSettings(ctx context.Context) (map[string]dbSetting, error) {
	out := map[string]dbSetting{}
	p, err := s.pool(ctx, "")
	if err != nil {
		return out, err
	}
	ok, err := s.regclassExists(ctx, p, "kingres_cp.database_setting")
	if err != nil || !ok {
		return out, err
	}
	rows, err := p.Query(ctx, `SELECT database_name, sharding_enabled, bucket_count, durability, ledger_mode,
		ledger_digest_interval, created_at, created_by FROM kingres_cp.database_setting`)
	if err != nil {
		return out, s.wrap("reading kingres_cp.database_setting", err)
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (dbSetting, error) {
		var d dbSetting
		return d, r.Scan(&d.name, &d.sharding, &d.bucketCnt, &d.durability, &d.ledgerMode, &d.digest, &d.createdAt, &d.createdBy)
	})
	if err != nil {
		return out, s.wrap("reading kingres_cp.database_setting", err)
	}
	for _, d := range list {
		d.createdAt = d.createdAt.UTC()
		out[d.name] = d
	}
	return out, nil
}
