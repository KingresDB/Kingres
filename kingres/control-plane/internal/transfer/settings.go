// SPDX-License-Identifier: AGPL-3.0-only

package transfer

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
)

// The defaults of the cluster setting dataTransfer (design "Limits and timeouts").
const (
	DefaultRetentionHours = 24
	MaxRetentionHours     = 168
	DefaultMaxFileBytes   = int64(20) << 30
	// DefaultStagingQuotaCap is the larger bound of the default staging quota: the smaller of this and a quarter of the file system.
	DefaultStagingQuotaCap = int64(100) << 30
	DefaultMaxConcurrent   = 2
	MaxMaxConcurrent       = 16
	// MinFileBytes is the smallest value of the byte limits.
	MinFileBytes = int64(1) << 20
	// AtomicDefault is the default of the flag max-atomic-bytes.
	AtomicDefault = int64(2) << 30
)

// The keys of kingres_cp.cluster_setting.
const (
	KeyEnabled    = "dataTransfer.enabled"
	KeyRetention  = "dataTransfer.retentionHours"
	KeyMaxFile    = "dataTransfer.maxFileBytes"
	KeyQuota      = "dataTransfer.stagingQuotaBytes"
	KeyConcurrent = "dataTransfer.maxConcurrent"
)

// Settings is the cluster setting dataTransfer as the transfer code uses it.
type Settings struct {
	Enabled        bool
	RetentionHours int
	MaxFileBytes   int64
	// StagingQuotaBytes is 0 while the administrator has not set it: the default is the smaller of DefaultStagingQuotaCap and a quarter of
	// the file system of the staging directory, which only the leader can tell.
	StagingQuotaBytes int64
	MaxConcurrent     int
}

// DefaultSettings returns the settings of a cluster that never changed them: the data transfer is off.
func DefaultSettings() Settings {
	return Settings{RetentionHours: DefaultRetentionHours, MaxFileBytes: DefaultMaxFileBytes, MaxConcurrent: DefaultMaxConcurrent}
}

// Apply sets one value from a row of kingres_cp.cluster_setting; a key of another setting, or a value of the wrong type, changes nothing.
func (s *Settings) Apply(key string, raw []byte) {
	var n int64
	var b bool
	switch key {
	case KeyEnabled:
		if json.Unmarshal(raw, &b) == nil {
			s.Enabled = b
		}
	case KeyRetention:
		if json.Unmarshal(raw, &n) == nil && n >= 1 && n <= MaxRetentionHours {
			s.RetentionHours = int(n)
		}
	case KeyMaxFile:
		if json.Unmarshal(raw, &n) == nil && n >= MinFileBytes {
			s.MaxFileBytes = n
		}
	case KeyQuota:
		if json.Unmarshal(raw, &n) == nil && n >= MinFileBytes {
			s.StagingQuotaBytes = n
		}
	case KeyConcurrent:
		if json.Unmarshal(raw, &n) == nil && n >= 1 && n <= MaxMaxConcurrent {
			s.MaxConcurrent = int(n)
		}
	}
}

// API is the setting as the contract shows it. An unset quota is shown as the larger bound of its default (the answer of a server that
// knows its file system may show the smaller one).
func (s Settings) API() *api.DataTransferSettings {
	q := s.StagingQuotaBytes
	if q == 0 {
		q = DefaultStagingQuotaCap
	}
	return &api.DataTransferSettings{Enabled: s.Enabled, RetentionHours: s.RetentionHours, MaxFileBytes: s.MaxFileBytes, StagingQuotaBytes: q, MaxConcurrent: s.MaxConcurrent}
}

// Querier is the part of a pgx connection or transaction the setting reader needs.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// Load reads the setting from kingres_cp.cluster_setting.
func Load(ctx context.Context, q Querier) (Settings, error) {
	out := DefaultSettings()
	r, err := q.Query(ctx, `SELECT key, value FROM kingres_cp.cluster_setting WHERE key LIKE 'dataTransfer.%'`)
	if err != nil {
		return out, err
	}
	defer r.Close()
	for r.Next() {
		var k string
		var v []byte
		if err := r.Scan(&k, &v); err != nil {
			return out, err
		}
		out.Apply(k, v)
	}
	return out, r.Err()
}

// Validate checks an update of the setting and returns the values to store, by key, or the problems of the request. An update that sets
// nothing is refused.
func Validate(u *api.DataTransferSettingsUpdate) (map[string]any, []problem.FieldError) {
	set := map[string]any{}
	var errs []problem.FieldError
	bad := func(field, msg, code string) {
		errs = append(errs, problem.FieldError{Field: field, Message: msg, Code: code})
	}
	if u.Enabled != nil {
		set[KeyEnabled] = *u.Enabled
	}
	if v := u.RetentionHours; v != nil {
		if *v < 1 || *v > MaxRetentionHours {
			bad("/dataTransfer/retentionHours", "must be 1 to 168", "out_of_range")
		}
		set[KeyRetention] = *v
	}
	if v := u.MaxFileBytes; v != nil {
		if *v < MinFileBytes {
			bad("/dataTransfer/maxFileBytes", "must be at least 1 MiB", "out_of_range")
		}
		set[KeyMaxFile] = *v
	}
	if v := u.StagingQuotaBytes; v != nil {
		if *v < MinFileBytes {
			bad("/dataTransfer/stagingQuotaBytes", "must be at least 1 MiB", "out_of_range")
		}
		set[KeyQuota] = *v
	}
	if v := u.MaxConcurrent; v != nil {
		if *v < 1 || *v > MaxMaxConcurrent {
			bad("/dataTransfer/maxConcurrent", "must be 1 to 16", "out_of_range")
		}
		set[KeyConcurrent] = *v
	}
	if len(set) == 0 && len(errs) == 0 {
		bad("/dataTransfer", "at least one property is required", "required")
	}
	return set, errs
}
