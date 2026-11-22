// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026 RooyeKhat Media Co

package rebalance

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
)

// The strategies of contract 0.15.0 and the names the extension uses (design 4.2).
const (
	ByBucketCount = "byBucketCount"
	ByDiskSize    = "byDiskSize"
)

// Triggers of a run (contract 0.15.0, RebalanceStatus.trigger).
const (
	TriggerManual    = "manual"
	TriggerAutomatic = "automatic"
	TriggerDrain     = "drain"
)

// Defaults of the settings (design 8.2, 8.3, 8.5).
const (
	DefaultMaxMovesPerServer    = 2
	DefaultMaxMoveAttempts      = 3
	DefaultMaxDiskUsedFraction  = 0.85
	DefaultMaxStandbyLagBytes   = int64(64 << 20)
	DefaultThresholdByBucketCnt = 0.0
	DefaultThresholdByDiskSize  = 0.1
)

// sqlStrategy is the strategy as the extension names it.
func sqlStrategy(s string) string {
	if s == ByDiskSize {
		return "by_disk_size"
	}
	return "by_bucket_count"
}

// DefaultThreshold is the threshold of a strategy when nothing says otherwise (design 4.4).
func DefaultThreshold(strategy string) float64 {
	if strategy == ByDiskSize {
		return DefaultThresholdByDiskSize
	}
	return DefaultThresholdByBucketCnt
}

// Settings are the cluster settings under "rebalance." that a run reads (kingres_cp.cluster_setting).
type Settings struct {
	Strategy            string
	Threshold           *float64
	MaxConcurrentMoves  int
	MaxMovesPerServer   int
	MaxMoveAttempts     int
	MaxDiskUsedFraction float64
	MaxStandbyLagBytes  int64
}

// DefaultSettings are the settings of a cluster that never changed them.
func DefaultSettings() Settings {
	return Settings{Strategy: ByBucketCount, MaxConcurrentMoves: 4, MaxMovesPerServer: DefaultMaxMovesPerServer,
		MaxMoveAttempts: DefaultMaxMoveAttempts, MaxDiskUsedFraction: DefaultMaxDiskUsedFraction, MaxStandbyLagBytes: DefaultMaxStandbyLagBytes}
}

// ReadSettings reads the rebalance settings from the system database.
func ReadSettings(ctx context.Context, c *pgx.Conn) (Settings, error) {
	out := DefaultSettings()
	var have bool
	if err := c.QueryRow(ctx, `SELECT to_regclass('kingres_cp.cluster_setting') IS NOT NULL`).Scan(&have); err != nil || !have {
		return out, err
	}
	rows, err := c.Query(ctx, `SELECT key, value FROM kingres_cp.cluster_setting WHERE key LIKE 'rebalance.%'`)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var k string
		var v []byte
		if err := rows.Scan(&k, &v); err != nil {
			return out, err
		}
		out.apply(k, v)
	}
	return out, rows.Err()
}

func (s *Settings) apply(key string, v []byte) {
	var n int
	var f float64
	var str string
	switch strings.TrimPrefix(key, "rebalance.") {
	case "strategy":
		if json.Unmarshal(v, &str) == nil && (str == ByBucketCount || str == ByDiskSize) {
			s.Strategy = str
		}
	case "threshold":
		if string(v) == "null" {
			s.Threshold = nil
		} else if json.Unmarshal(v, &f) == nil {
			s.Threshold = &f
		}
	case "maxConcurrentMoves":
		if json.Unmarshal(v, &n) == nil && n > 0 {
			s.MaxConcurrentMoves = n
		}
	case "maxMovesPerServer":
		if json.Unmarshal(v, &n) == nil && n > 0 {
			s.MaxMovesPerServer = n
		}
	case "maxMoveAttempts":
		if json.Unmarshal(v, &n) == nil && n > 0 {
			s.MaxMoveAttempts = n
		}
	case "maxDiskUsedFraction":
		if json.Unmarshal(v, &f) == nil && f > 0 {
			s.MaxDiskUsedFraction = f
		}
	case "maxStandbyLagBytes":
		var b int64
		if json.Unmarshal(v, &b) == nil && b >= 0 {
			s.MaxStandbyLagBytes = b
		}
	}
}

// Resolve fills what a request left out with the cluster's settings and the strategy's defaults, so that the
// run records what it really used (the status and the report show the same values).
func Resolve(o Options, s Settings) Options {
	if o.Strategy == "" {
		o.Strategy = s.Strategy
	}
	if o.Threshold == nil {
		t := DefaultThreshold(o.Strategy)
		if s.Threshold != nil && s.Strategy == o.Strategy {
			t = *s.Threshold
		}
		o.Threshold = &t
	}
	if o.MaxMovesPerServer == 0 {
		o.MaxMovesPerServer = s.MaxMovesPerServer
	}
	if o.Trigger == "" {
		o.Trigger = TriggerManual
	}
	return o
}

// OptionsFromAPI converts the options of a request.
func OptionsFromAPI(r api.RebalanceOptions) Options {
	return Options{Databases: r.Databases, MaxConcurrentMoves: r.MaxConcurrentMoves, Strategy: r.Strategy, Threshold: r.Threshold,
		DrainOnly: r.DrainOnly, MaxMovesPerServer: r.MaxMovesPerServer, AcknowledgeFailures: r.AcknowledgeFailures,
		CleanupDelaySeconds: r.CleanupDelaySeconds}
}

// validateNew checks the members of the options that contract 0.15.0 added.
func validateNew(o Options) []problem.FieldError {
	var errs []problem.FieldError
	if o.Strategy != "" && o.Strategy != ByBucketCount && o.Strategy != ByDiskSize {
		errs = append(errs, problem.FieldError{Field: "/strategy", Message: "must be byBucketCount or byDiskSize", Code: "enum"})
	}
	if o.Threshold != nil && !(*o.Threshold >= 0 && *o.Threshold <= 1) {
		errs = append(errs, problem.FieldError{Field: "/threshold", Message: "must be 0 to 1", Code: "out_of_range"})
	}
	if o.MaxMovesPerServer != 0 && (o.MaxMovesPerServer < 1 || o.MaxMovesPerServer > 16) {
		errs = append(errs, problem.FieldError{Field: "/maxMovesPerServer", Message: "must be 1 to 16", Code: "out_of_range"})
	}
	if o.CleanupDelaySeconds < 0 || o.CleanupDelaySeconds > 86400 {
		errs = append(errs, problem.FieldError{Field: "/cleanupDelaySeconds", Message: "must be 0 to 86400", Code: "out_of_range"})
	}
	return errs
}

// needsExtension08 reports whether the options use something that only plan_rebalance of the extension
// version 0.8 has.
func needsExtension08(o Options) bool {
	return o.Strategy == ByDiskSize || o.Threshold != nil && *o.Threshold != 0 || o.DrainOnly || o.DrainServer != "" || len(o.ExcludeServers) > 0
}
