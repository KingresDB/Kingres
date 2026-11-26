// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package transfer

import (
	"context"
	"errors"
	"testing"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/auth"
)

func TestValidateSettings(t *testing.T) {
	ptr := func(n int) *int { return &n }
	big := func(n int64) *int64 { return &n }
	yes := true
	cases := []struct {
		name string
		u    api.DataTransferSettingsUpdate
		keys int
		errs []string
	}{
		{"enable", api.DataTransferSettingsUpdate{Enabled: &yes}, 1, nil},
		{"empty", api.DataTransferSettingsUpdate{}, 0, []string{"/dataTransfer"}},
		{"retention low", api.DataTransferSettingsUpdate{RetentionHours: ptr(0)}, 1, []string{"/dataTransfer/retentionHours"}},
		{"retention high", api.DataTransferSettingsUpdate{RetentionHours: ptr(169)}, 1, []string{"/dataTransfer/retentionHours"}},
		{"retention 168", api.DataTransferSettingsUpdate{RetentionHours: ptr(168)}, 1, nil},
		{"file small", api.DataTransferSettingsUpdate{MaxFileBytes: big(1000)}, 1, []string{"/dataTransfer/maxFileBytes"}},
		{"quota small", api.DataTransferSettingsUpdate{StagingQuotaBytes: big(-5)}, 1, []string{"/dataTransfer/stagingQuotaBytes"}},
		{"concurrent 0", api.DataTransferSettingsUpdate{MaxConcurrent: ptr(0)}, 1, []string{"/dataTransfer/maxConcurrent"}},
		{"concurrent 17", api.DataTransferSettingsUpdate{MaxConcurrent: ptr(17)}, 1, []string{"/dataTransfer/maxConcurrent"}},
		{"two bad", api.DataTransferSettingsUpdate{MaxConcurrent: ptr(99), RetentionHours: ptr(-1)}, 2, []string{"/dataTransfer/retentionHours", "/dataTransfer/maxConcurrent"}},
	}
	for _, tc := range cases {
		set, errs := Validate(&tc.u)
		if len(set) != tc.keys || len(errs) != len(tc.errs) {
			t.Errorf("%s: %v %v", tc.name, set, errs)
			continue
		}
		for i, e := range errs {
			if e.Field != tc.errs[i] {
				t.Errorf("%s: field %q, want %q", tc.name, e.Field, tc.errs[i])
			}
		}
	}
}

func TestSettingsApplyAndAPI(t *testing.T) {
	s := DefaultSettings()
	if s.Enabled || s.RetentionHours != 24 || s.MaxFileBytes != 20<<30 || s.MaxConcurrent != 2 || s.StagingQuotaBytes != 0 {
		t.Fatalf("defaults: %+v", s)
	}
	for k, v := range map[string]string{KeyEnabled: `"yes"`, KeyRetention: `"3"`, KeyMaxFile: `5`, KeyQuota: `12`, KeyConcurrent: `99`, "dataTransfer.other": `1`, "autoFailover.enabled": `true`} {
		s.Apply(k, []byte(v))
	}
	if s != DefaultSettings() {
		t.Errorf("an invalid value changed the setting: %+v", s)
	}
	s.Apply(KeyEnabled, []byte(`true`))
	s.Apply(KeyQuota, []byte(`1073741824`))
	a := s.API()
	if !a.Enabled || a.StagingQuotaBytes != 1<<30 || a.RetentionHours != 24 {
		t.Errorf("API: %+v", a)
	}
	if got := DefaultSettings().API(); got.StagingQuotaBytes != 100<<30 || got.Enabled {
		t.Errorf("default API: %+v", got)
	}
}

// The role is checked again at every step: a role that signed out, was demoted, was replaced or cannot be checked ends the transfer.
func TestRoleChecker(t *testing.T) {
	id := Identity{Name: "alice", OID: 100}
	mk := func(roles []string, rerr error, oid int64, found bool, oerr error) RoleChecker {
		return RoleChecker{
			Current: func(context.Context, string) ([]string, error) { return roles, rerr },
			OID:     func(context.Context, string) (int64, bool, error) { return oid, found, oerr },
		}
	}
	admin := []string{"kingres_admin"}
	cases := []struct {
		name      string
		c         RoleChecker
		code      string
		retryable bool
	}{
		{"fine", mk(admin, nil, 100, true, nil), "", false},
		{"admin and others", mk([]string{"kingres_readonly", "kingres_admin"}, nil, 100, true, nil), "", false},
		{"signed out or dropped", mk(nil, auth.ErrInvalidCredentials, 100, true, nil), CodeForbidden, false},
		{"demoted", mk([]string{"kingres_ops"}, nil, 100, true, nil), CodeForbidden, false},
		{"no cluster role", mk(nil, nil, 100, true, nil), CodeForbidden, false},
		{"replaced", mk(admin, nil, 101, true, nil), CodeForbidden, false},
		{"gone", mk(admin, nil, 0, false, nil), CodeForbidden, false},
		{"backend down", mk(nil, auth.ErrUnavailable, 100, true, nil), CodeSessionLost, true},
		{"oid lookup down", mk(admin, nil, 0, false, errors.New("boom")), CodeSessionLost, true},
	}
	for _, tc := range cases {
		err := tc.c.Check(context.Background(), id)
		if tc.code == "" {
			if err != nil {
				t.Errorf("%s: %v", tc.name, err)
			}
			continue
		}
		var te *Error
		if !errors.As(err, &te) || te.Code != tc.code || te.Retryable != tc.retryable {
			t.Errorf("%s: %v", tc.name, err)
		}
	}
}
