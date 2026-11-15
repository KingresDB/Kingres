// SPDX-License-Identifier: PostgreSQL

package ops

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/sysdb"
)

// UpdateClusterSettings validates the whole request, then stores the values
// in one transaction (kingres_cp.cluster_setting). Feature flags can only be
// turned on: the extension is installed in the system database, nothing is
// ever removed.
func (o *Ops) UpdateClusterSettings(ctx context.Context, actor string, req api.ClusterSettingsUpdateRequest) error {
	var errs []problem.FieldError
	set := map[string]any{}
	if v := req.DefaultReplicaCount; v != nil {
		if *v < 0 || *v > 3 {
			errs = append(errs, field("/defaultReplicaCount", "must be 0 to 3", "out_of_range"))
		}
		set["replica.default"] = *v
	}
	if v := req.DefaultDurability; v != nil {
		if _, ok := durabilityGUC[*v]; !ok {
			errs = append(errs, field("/defaultDurability", "must be async, sync or syncApply", "enum"))
		}
		set["durability.default"] = *v
	}
	if a := req.AutoFailover; a != nil {
		if a.Enabled == nil && a.TimeoutSeconds == nil && a.MaxCount == nil {
			errs = append(errs, field("/autoFailover", "at least one property is required", "required"))
		}
		if a.Enabled != nil {
			set["autoFailover.enabled"] = *a.Enabled
		}
		if a.TimeoutSeconds != nil {
			if *a.TimeoutSeconds < 1 || *a.TimeoutSeconds > 3600 {
				errs = append(errs, field("/autoFailover/timeoutSeconds", "must be 1 to 3600", "out_of_range"))
			}
			set["autoFailover.timeoutSeconds"] = *a.TimeoutSeconds
		}
		if a.MaxCount != nil {
			if *a.MaxCount < 1 || *a.MaxCount > 100 {
				errs = append(errs, field("/autoFailover/maxCount", "must be 1 to 100", "out_of_range"))
			}
			set["autoFailover.maxCount"] = *a.MaxCount
		}
	}
	if r := req.Rebalance; r != nil {
		if r.MaxConcurrentMoves == nil {
			errs = append(errs, field("/rebalance", "maxConcurrentMoves is required", "required"))
		} else {
			if *r.MaxConcurrentMoves < 1 || *r.MaxConcurrentMoves > 64 {
				errs = append(errs, field("/rebalance/maxConcurrentMoves", "must be 1 to 64", "out_of_range"))
			}
			set["rebalance.maxConcurrentMoves"] = *r.MaxConcurrentMoves
		}
	}
	var wantShard, wantLedger bool
	if f := req.Features; f != nil {
		if f.Sharding == nil && f.Ledger == nil {
			errs = append(errs, field("/features", "at least one property is required", "required"))
		}
		if f.Sharding != nil && !*f.Sharding || f.Ledger != nil && !*f.Ledger {
			return problem.New(problem.ImmutableField, "A feature can be turned on, never off.")
		}
		wantShard, wantLedger = f.Sharding != nil, f.Ledger != nil
	}
	if len(errs) > 0 {
		return problem.Validation("The settings are invalid.", errs...)
	}
	if len(set) == 0 && !wantShard && !wantLedger {
		return problem.Validation("The settings update is empty.", field("/", "at least one property is required", "required"))
	}
	if wantShard || wantLedger {
		if err := o.installExtensions(ctx, sysdb.Database, wantShard, wantLedger); err != nil {
			return wrap(err)
		}
	}
	err := wrap(o.asService(ctx, sysdb.Database, func(tx pgx.Tx) error {
		for k, v := range set {
			j, _ := json.Marshal(v)
			if _, err := tx.Exec(ctx, `INSERT INTO kingres_cp.cluster_setting (key, value) VALUES ($1, $2)
				ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = now()`, k, j); err != nil {
				return err
			}
		}
		return nil
	}))
	if _, changed := set["replica.default"]; changed && err == nil && o.OnReplicaCount != nil {
		o.OnReplicaCount()
	}
	return err
}
