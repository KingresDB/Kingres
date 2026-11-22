// SPDX-License-Identifier: AGPL-3.0-only

package ops

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/sysdb"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/task"
)

// UpdateClusterSettings validates the whole request, then stores the values
// in one transaction (kingres_cp.cluster_setting). Feature flags can only be
// turned on: the extension is installed in the system database, nothing is
// ever removed.
//
// It returns the task that the update started, or nil: a new replica count starts the
// reconcile of the standbys (standby.reconcile) and the caller reports it.
func (o *Ops) UpdateClusterSettings(ctx context.Context, actor string, req api.ClusterSettingsUpdateRequest) (*task.Task, error) {
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
	if c := req.Clock; c != nil {
		if c.MaxDifferenceMs == nil {
			errs = append(errs, field("/clock", "maxDifferenceMs is required", "required"))
		} else {
			if *c.MaxDifferenceMs < 10 || *c.MaxDifferenceMs > 600000 {
				errs = append(errs, field("/clock/maxDifferenceMs", "must be 10 to 600000", "out_of_range"))
			}
			set["clock.maxDifferenceMs"] = *c.MaxDifferenceMs
		}
	}
	if a := req.Analytics; a != nil {
		if a.AllowNonSuperuserOwners == nil {
			errs = append(errs, field("/analytics", "allowNonSuperuserOwners is required", "required"))
		} else {
			set[settingNonSuperuserOwners] = *a.AllowNonSuperuserOwners
		}
	}
	var wantShard, wantLedger bool
	if f := req.Features; f != nil {
		if f.Sharding == nil && f.Ledger == nil {
			errs = append(errs, field("/features", "at least one property is required", "required"))
		}
		if f.Sharding != nil && !*f.Sharding || f.Ledger != nil && !*f.Ledger {
			return nil, problem.New(problem.ImmutableField, "A feature can be turned on, never off.")
		}
		wantShard, wantLedger = f.Sharding != nil, f.Ledger != nil
	}
	if len(errs) > 0 {
		return nil, problem.Validation("The settings are invalid.", errs...)
	}
	if len(set) == 0 && !wantShard && !wantLedger {
		return nil, problem.Validation("The settings update is empty.", field("/", "at least one property is required", "required"))
	}
	if wantShard || wantLedger {
		if err := o.installExtensions(ctx, sysdb.Database, wantShard, wantLedger); err != nil {
			return nil, wrap(err)
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
	if err != nil {
		return nil, err
	}
	if _, changed := set["replica.default"]; changed && o.OnReplicaCount != nil {
		return o.OnReplicaCount(ctx)
	}
	return nil, nil
}

// ResetAutoFailoverCounter sets the counter of automatic failovers to 0 (the operation resetAutoFailoverCounter; a rebalance that
// completes does it too).
func (o *Ops) ResetAutoFailoverCounter(ctx context.Context) error {
	return wrap(o.asService(ctx, sysdb.Database, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO kingres_cp.cluster_setting (key, value) VALUES ('autoFailover.eventsSinceReset', '0'::jsonb)
			ON CONFLICT (key) DO UPDATE SET value = '0'::jsonb, updated_at = now()`)
		return err
	}))
}
