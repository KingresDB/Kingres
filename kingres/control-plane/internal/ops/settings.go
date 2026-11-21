// SPDX-License-Identifier: AGPL-3.0-only

package ops

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

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
		if a := r.Automatic; a != nil {
			errs = append(errs, automaticSettings(a, set)...)
		}
		if r.MaxConcurrentMoves == nil && r.Automatic == nil && r.Strategy == nil && r.Threshold == nil && !r.ThresholdSet && r.MaxMovesPerServer == nil &&
			r.MaxMoveAttempts == nil && r.MaxDiskUsedFraction == nil && r.MaxStandbyLagBytes == nil {
			errs = append(errs, field("/rebalance", "at least one property is required", "required"))
		}
		if r.MaxConcurrentMoves != nil {
			if *r.MaxConcurrentMoves < 1 || *r.MaxConcurrentMoves > 64 {
				errs = append(errs, field("/rebalance/maxConcurrentMoves", "must be 1 to 64", "out_of_range"))
			}
			set["rebalance.maxConcurrentMoves"] = *r.MaxConcurrentMoves
		}
		if r.Strategy != nil {
			if *r.Strategy != "byBucketCount" && *r.Strategy != "byDiskSize" {
				errs = append(errs, field("/rebalance/strategy", "must be byBucketCount or byDiskSize", "enum"))
			}
			set["rebalance.strategy"] = *r.Strategy
		}
		if r.Threshold != nil {
			if !(*r.Threshold >= 0 && *r.Threshold <= 1) {
				errs = append(errs, field("/rebalance/threshold", "must be 0 to 1", "out_of_range"))
			}
			set["rebalance.threshold"] = *r.Threshold
		} else if r.ThresholdSet {
			set["rebalance.threshold"] = nil
		}
		if r.MaxMovesPerServer != nil {
			if *r.MaxMovesPerServer < 1 || *r.MaxMovesPerServer > 16 {
				errs = append(errs, field("/rebalance/maxMovesPerServer", "must be 1 to 16", "out_of_range"))
			}
			set["rebalance.maxMovesPerServer"] = *r.MaxMovesPerServer
		}
		if r.MaxMoveAttempts != nil {
			if *r.MaxMoveAttempts < 1 || *r.MaxMoveAttempts > 20 {
				errs = append(errs, field("/rebalance/maxMoveAttempts", "must be 1 to 20", "out_of_range"))
			}
			set["rebalance.maxMoveAttempts"] = *r.MaxMoveAttempts
		}
		if r.MaxDiskUsedFraction != nil {
			if !(*r.MaxDiskUsedFraction >= 0.5 && *r.MaxDiskUsedFraction <= 0.99) {
				errs = append(errs, field("/rebalance/maxDiskUsedFraction", "must be 0.5 to 0.99", "out_of_range"))
			}
			set["rebalance.maxDiskUsedFraction"] = *r.MaxDiskUsedFraction
		}
		if r.MaxStandbyLagBytes != nil {
			if *r.MaxStandbyLagBytes < 0 {
				errs = append(errs, field("/rebalance/maxStandbyLagBytes", "must not be negative", "out_of_range"))
			}
			set["rebalance.maxStandbyLagBytes"] = *r.MaxStandbyLagBytes
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

// automaticSettings validates the "automatic" member and adds the values to set.
func automaticSettings(a *api.AutomaticSettingsUpdate, set map[string]any) []problem.FieldError {
	var errs []problem.FieldError
	have := a.Enabled != nil || a.Strategy != nil || a.Threshold != nil || a.CheckIntervalSeconds != nil || a.SettleSeconds != nil ||
		a.SettleAfterTopologySeconds != nil || a.MinIntervalSeconds != nil || a.FailoverSettleSeconds != nil || a.WindowSet
	if !have {
		errs = append(errs, field("/rebalance/automatic", "at least one property is required", "required"))
	}
	put := func(key string, v any) { set["rebalance.automatic."+key] = v }
	if a.Enabled != nil {
		put("enabled", *a.Enabled)
	}
	if a.Strategy != nil {
		if *a.Strategy != "byBucketCount" && *a.Strategy != "byDiskSize" {
			errs = append(errs, field("/rebalance/automatic/strategy", "must be byBucketCount or byDiskSize", "enum"))
		}
		put("strategy", *a.Strategy)
	}
	if a.Threshold != nil {
		if !(*a.Threshold > 0 && *a.Threshold <= 1) {
			errs = append(errs, field("/rebalance/automatic/threshold", "must be above 0 and at most 1", "out_of_range"))
		}
		put("threshold", *a.Threshold)
	}
	ranges := []struct {
		name     string
		v        *int
		min, max int
	}{
		{"checkIntervalSeconds", a.CheckIntervalSeconds, 5, 86400},
		{"settleSeconds", a.SettleSeconds, 0, 86400},
		{"settleAfterTopologySeconds", a.SettleAfterTopologySeconds, 0, 86400},
		{"minIntervalSeconds", a.MinIntervalSeconds, 0, 604800},
		{"failoverSettleSeconds", a.FailoverSettleSeconds, 0, 86400},
	}
	for _, r := range ranges {
		if r.v == nil {
			continue
		}
		if *r.v < r.min || *r.v > r.max {
			errs = append(errs, field("/rebalance/automatic/"+r.name, fmt.Sprintf("must be %d to %d", r.min, r.max), "out_of_range"))
		}
		put(r.name, *r.v)
	}
	if a.WindowSet {
		if a.Window == nil {
			put("window", nil)
		} else {
			w := a.Window
			if !validClock(w.From) || !validClock(w.To) {
				errs = append(errs, field("/rebalance/automatic/window", "from and to must be times of day as HH:MM", "pattern"))
			} else if _, err := time.LoadLocation(w.TimeZone); err != nil || w.TimeZone == "" {
				errs = append(errs, field("/rebalance/automatic/window/timeZone", "must be an IANA time zone name", "unknown_time_zone"))
			}
			put("window", w)
		}
	}
	return errs
}

func validClock(s string) bool {
	var h, m int
	n, err := fmt.Sscanf(s, "%d:%d", &h, &m)
	return err == nil && n == 2 && len(s) == 5 && h >= 0 && h <= 23 && m >= 0 && m <= 59
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
