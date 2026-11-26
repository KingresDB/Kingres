// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package ops

import (
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
)

func TestIsoDuration(t *testing.T) {
	for s, want := range map[string]bool{
		"P1D": true, "PT6H": true, "P1M": true, "P1Y2M3DT4H5M6S": true, "P1W": true, "PT0.5S": true, "P7D": true,
		"": false, "P": false, "PT": false, "P1DT": false, "1 day": false, "daily": false, "P-1D": false, "p1d": false, "P1H": false, "PT1D": false,
		"P1D; DROP TABLE x": false, "P" + strings.Repeat("1", 70) + "D": false,
	} {
		if got := isoDuration(s); got != want {
			t.Errorf("%q: %v, want %v", s, got, want)
		}
	}
}

func TestValidatePolicyDefaultsAndFields(t *testing.T) {
	a, err := validatePolicy(api.PartitionPolicyRequest{PartitionEvery: "P1D"})
	if err != nil || a.every != "P1D" || a.ahead != "P7D" || a.runEvery != "PT1H" || a.action != "drop" || a.timezone != "UTC" || !a.enabled || a.retention != nil || a.options != nil {
		t.Fatalf("defaults: %+v %v", a, err)
	}
	ret, col, off, lvl, z := "P90D", "P7D", false, 5, "zstd"
	a, err = validatePolicy(api.PartitionPolicyRequest{PartitionEvery: "P1M", CreateAhead: "P2M", Retention: &ret, RetentionAction: "detach", ColumnarAfter: &col,
		ColumnarSortBy: []string{"ts"}, Compression: &z, CompressionLvl: &lvl, Timezone: "Europe/Berlin", RunEvery: "PT15M", Enabled: &off})
	if err != nil || a.ahead != "P2M" || *a.retention != "P90D" || a.action != "detach" || *a.columnar != "P7D" || a.timezone != "Europe/Berlin" || a.runEvery != "PT15M" || a.enabled ||
		a.options != `{"compression":"zstd","compression_level":5}` {
		t.Errorf("values: %+v %v", a, err)
	}
	bad := "tomorrow"
	zero, big := 0, 40
	cases := map[string]api.PartitionPolicyRequest{
		"/partitionEvery":   {},
		"/createAhead":      {PartitionEvery: "P1D", CreateAhead: bad},
		"/runEvery":         {PartitionEvery: "P1D", RunEvery: bad},
		"/retention":        {PartitionEvery: "P1D", Retention: &bad},
		"/columnarAfter":    {PartitionEvery: "P1D", ColumnarAfter: &bad},
		"/retentionAction":  {PartitionEvery: "P1D", RetentionAction: "archive"},
		"/timezone":         {PartitionEvery: "P1D", Timezone: "UTC'; drop"},
		"/columnarSortBy/0": {PartitionEvery: "P1D", ColumnarSortBy: []string{"a b"}},
		"/compression":      {PartitionEvery: "P1D", Compression: &bad},
		"/compressionLevel": {PartitionEvery: "P1D", CompressionLvl: &zero},
	}
	for f, req := range cases {
		_, err := validatePolicy(req)
		var p *problem.Problem
		if !errors.As(err, &p) || p.Code != problem.ValidationFailed {
			t.Errorf("%s: %v", f, err)
			continue
		}
		found := false
		for _, e := range p.Errors {
			found = found || e.Field == f
		}
		if !found {
			t.Errorf("%s: fields %+v", f, p.Errors)
		}
	}
	if _, err := validatePolicy(api.PartitionPolicyRequest{PartitionEvery: "P1D", CompressionLvl: &big}); err == nil {
		t.Error("level 40 accepted")
	}
}

// A refusal of the extension names its argument at the start of the message; the field is the argument named first, and of retention and
// retention_action at the same place the longer one.
func TestPolicySQLErrorNamesTheFirstArgument(t *testing.T) {
	cases := []struct{ code, msg, field string }{
		{"22023", "retention must be at least one partition_every", "/retention"},
		{"22023", "retention_action must be drop or detach", "/retentionAction"},
		{"22023", "create_ahead must not be negative", "/createAhead"},
		{"22023", "run_every must be positive", "/runEvery"},
		{"22023", "partition_every in hours needs a timestamp or timestamptz column, not date", "/partitionEvery"},
		{"22023", "unknown time zone \"Mars/Olympus\"", "/timezone"},
		{"22023", "something the extension says that names no argument", "/partitionEvery"}, // the first field
		{"22023", "columnar_after must be at most retention", "/columnarAfter"},
	}
	for _, tc := range cases {
		err := policySQLError(&pgconn.PgError{Code: tc.code, Message: tc.msg})
		var p *problem.Problem
		if !errors.As(err, &p) || p.Code != problem.ValidationFailed || len(p.Errors) != 1 || p.Errors[0].Field != tc.field || p.Errors[0].Message != tc.msg {
			t.Errorf("%q: %v", tc.msg, err)
		}
	}
	err := policySQLError(&pgconn.PgError{Code: "0A000", Message: "table public.ev has a default partition \"ev_d\""})
	var p *problem.Problem
	if !errors.As(err, &p) || p.Code != problem.PartitionPolicyConflict || !strings.Contains(p.Detail, "default partition") {
		t.Errorf("0A000: %v", err)
	}
	// a distributed table whose policy would need more leaves than kingres_shard.max_partition_leaves allows is a conflict with the table, with the numbers
	err = policySQLError(&pgconn.PgError{Code: "54000", Message: "the policy needs about 54 leaves in a steady state (6 buckets, 9 partitions); kingres_shard.max_partition_leaves is 18"})
	if !errors.As(err, &p) || p.Code != problem.PartitionPolicyConflict || !strings.Contains(p.Detail, "54 leaves") || !strings.Contains(p.Detail, "max_partition_leaves is 18") {
		t.Errorf("54000: %v", err)
	}
	err = policySQLError(&pgconn.PgError{Code: "42704", Message: "table public.ev has no time partitioning policy"})
	if !errors.As(err, &p) || p.Code != problem.NotFound {
		t.Errorf("42704: %v", err)
	}
}

func TestStorageSQLErrorGivesARefusalItsCode(t *testing.T) {
	err := storageSQLError(&pgconn.PgError{Code: "0A000", Message: "columnar tables cannot have foreign keys"})
	var ee *extError
	if !errors.As(err, &ee) || ee.code != problem.ConversionRefused || ee.msg != "columnar tables cannot have foreign keys" {
		t.Errorf("0A000: %v", err)
	}
	other := errors.New("connection lost")
	if storageSQLError(other) != other || storageSQLError(nil) != nil {
		t.Error("other errors must pass through")
	}
	if e := storageSQLError(&pgconn.PgError{Code: "57014", Message: "canceled"}); errors.As(e, &ee) {
		t.Error("a cancel is not a refusal")
	}
}

// The bound of a partition of a distributed table is the text that was given to create_partition; a range of two quoted values gives its limits, any other
// bound (a list, a value that is not quoted, MINVALUE) none.
func TestDistBoundRE(t *testing.T) {
	for bound, want := range map[string][2]string{
		"FROM ('2026-03-01') TO ('2026-03-02')":                                       {"2026-03-01", "2026-03-02"},
		"from ( '2026-03-01T00:00:00.000000Z' ) to ( '2026-03-02T00:00:00.000000Z' )": {"2026-03-01T00:00:00.000000Z", "2026-03-02T00:00:00.000000Z"},
		" FROM ('a') TO ('b') ":                                                       {"a", "b"},
		"IN ('a', 'b')":                                                               {},
		"FROM (MINVALUE) TO ('2026-03-02')":                                           {},
		"DEFAULT":                                                                     {},
	} {
		m := distBoundRE.FindStringSubmatch(bound)
		if want[0] == "" {
			if m != nil {
				t.Errorf("%q: matched %v", bound, m)
			}
			continue
		}
		if m == nil || m[1] != want[0] || m[2] != want[1] {
			t.Errorf("%q: %v, want %v", bound, m, want)
		}
	}
}
