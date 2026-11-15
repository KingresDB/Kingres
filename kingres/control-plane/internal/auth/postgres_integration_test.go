// SPDX-License-Identifier: PostgreSQL

package auth

import (
	"context"
	"errors"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/pgtest"
)

// Integration tests against the fixture instance (see scripts/); they run
// only when KINGRES_PG_BIN is set (pgtest builds the fixture).

func testDSN(t *testing.T) string {
	t.Helper()
	if !pgtest.Available() {
		t.Skip("KINGRES_PG_BIN is not set; skipping the PostgreSQL integration test")
	}
	dsn := pgtest.Fixture(t)
	return dsn
}

func TestPostgresAuthenticate(t *testing.T) {
	p, err := NewPostgres(testDSN(t), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name, user, pass string
		roles            []string
		err              error
	}{
		{"admin", "cp_admin", "cp_admin_pw", []string{RoleAdmin}, nil},
		{"readonly", "cp_ro", "cp_ro_pw", []string{RoleReadonly}, nil},
		{"role through another role", "cp_member", "cp_member_pw", []string{RoleOps}, nil},
		{"valid role without cluster role", "cp_none", "cp_none_pw", []string{}, nil},
		{"superuser is not admin", "cp_super", "cp_super_pw", []string{}, nil},
		{"wrong password", "cp_admin", "wrong", nil, ErrInvalidCredentials},
		{"unknown role", "no_such_role", "x", nil, ErrInvalidCredentials},
		{"role without LOGIN", "cp_disabled", "x", nil, ErrInvalidCredentials},
		{"user name that looks like SQL", `x"; drop table y; --`, "x", nil, ErrInvalidCredentials},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			roles, err := p.Authenticate(ctx, tc.user, tc.pass)
			if !errors.Is(err, tc.err) {
				t.Fatalf("err = %v, want %v", err, tc.err)
			}
			if tc.err == nil && !reflect.DeepEqual(roles, tc.roles) {
				t.Errorf("roles = %v, want %v", roles, tc.roles)
			}
		})
	}
}

func TestPostgresAuthenticateUnavailable(t *testing.T) {
	u, err := url.Parse(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	u.Host = "127.0.0.1:1" // nothing listens here
	p, err := NewPostgres(u.String(), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Authenticate(context.Background(), "cp_admin", "cp_admin_pw"); !errors.Is(err, ErrUnavailable) {
		t.Errorf("err = %v, want ErrUnavailable", err)
	}
}

func TestNewPostgresRejectsBadDSN(t *testing.T) {
	if _, err := NewPostgres("postgres://user:secret@host:notaport/db", time.Second); err == nil {
		t.Fatal("bad DSN accepted")
	} else if msg := err.Error(); strings.Contains(msg, "secret") {
		t.Errorf("error leaks the password: %v", err)
	}
}
