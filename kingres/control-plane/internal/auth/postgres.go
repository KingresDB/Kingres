// SPDX-License-Identifier: AGPL-3.0-only

package auth

import (
	"context"
	"errors"
	"fmt"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/cluster"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Errors returned by an Authenticator.
var (
	// ErrInvalidCredentials means PostgreSQL rejected the user name or the
	// password (or the role cannot log in). The two cases are not told apart.
	ErrInvalidCredentials = errors.New("invalid credentials")
	// ErrUnavailable means PostgreSQL could not be reached or failed.
	ErrUnavailable = errors.New("authentication backend unavailable")
)

// Authenticator verifies a PostgreSQL role and returns its cluster roles.
type Authenticator interface {
	// Authenticate returns the built-in cluster roles of the role, or
	// ErrInvalidCredentials or ErrUnavailable. The password is used for this
	// call only.
	Authenticate(ctx context.Context, username, password string) ([]string, error)
	// CurrentRoles tells, without a password, whether the role may still sign
	// in (ErrInvalidCredentials when it was dropped, can no longer log in or
	// has expired) and what its cluster roles are now.
	CurrentRoles(ctx context.Context, username string) ([]string, error)
}

// memberSQL lists the built-in cluster roles the current role is a member of,
// directly or through other roles. It walks pg_auth_members explicitly
// instead of using pg_has_role, because pg_has_role answers true for every
// role when the caller is a superuser, which would silently grant
// kingres_admin to superusers.
const memberSQL = `
WITH RECURSIVE m(roleid) AS (
    SELECT a.roleid FROM pg_auth_members a
    WHERE a.member = (SELECT oid FROM pg_roles WHERE rolname = current_user) AND a.inherit_option
  UNION
    SELECT a.roleid FROM pg_auth_members a JOIN m ON a.member = m.roleid WHERE a.inherit_option
)
SELECT r.rolname FROM pg_roles r
WHERE r.oid IN (SELECT roleid FROM m)
  AND r.rolname IN ('kingres_admin', 'kingres_ops', 'kingres_readonly', 'kingres_stats')`

// Postgres authenticates by opening a PostgreSQL connection as the role.
// Password verification is therefore done by PostgreSQL itself (SCRAM-SHA-256
// by the driver), with the server's pg_hba.conf and role attributes
// applying. kingresd never sees or stores a password hash.
type Postgres struct {
	mu      sync.RWMutex
	base    *pgx.ConnConfig
	timeout time.Duration
	// verifierSource reads stored password verifiers (see SetVerifierSource).
	verifierSource func(ctx context.Context, username string) (string, error)
}

// NewPostgres builds an authenticator from the DSN of the local instance.
// Only host, port, database, TLS and similar settings of the DSN are used;
// its user and password are replaced by the credentials of each login.
func NewPostgres(dsn string, timeout time.Duration) (*Postgres, error) {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		// The error of ParseConfig redacts the password.
		return nil, fmt.Errorf("parsing PostgreSQL DSN: %w", err)
	}
	return &Postgres{base: cfg, timeout: timeout}, nil
}

// SetDSN replaces the connection settings (host, port, database), for
// example after the control plane created the managed instance.
func (p *Postgres) SetDSN(dsn string) error {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return fmt.Errorf("parsing PostgreSQL DSN: %w", err)
	}
	p.mu.Lock()
	p.base = cfg
	p.mu.Unlock()
	return nil
}

func (p *Postgres) config() *pgx.ConnConfig {
	p.mu.RLock()
	defer p.mu.RUnlock()
	// The search path is fixed in the startup parameters (cluster.SafeSession): a database setting cannot change what the statements below resolve to.
	cfg := p.base.Copy()
	cluster.SafeSession(cfg)
	return cfg
}

// Authenticate implements Authenticator.
func (p *Postgres) Authenticate(ctx context.Context, username, password string) ([]string, error) {
	cfg := p.config()
	cfg.User = username
	cfg.Password = password
	if cfg.RuntimeParams == nil {
		cfg.RuntimeParams = map[string]string{}
	}
	cfg.RuntimeParams["application_name"] = "kingresd-login"

	cctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	conn, err := pgx.ConnectConfig(cctx, cfg)
	if err != nil {
		return nil, classifyConnectError(err)
	}
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()
	rows, err := conn.Query(cctx, memberSQL, pgx.QueryExecModeSimpleProtocol)
	if err != nil {
		return nil, fmt.Errorf("%w: reading cluster roles: %w", ErrUnavailable, err)
	}
	roles, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("%w: reading cluster roles: %w", ErrUnavailable, err)
	}
	return Normalize(roles), nil
}

// classifyConnectError maps a connection failure to ErrInvalidCredentials
// when PostgreSQL rejected the login (SQLSTATE 28xxx: invalid authorization
// specification or invalid password) and to ErrUnavailable otherwise.
func classifyConnectError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		if len(pgErr.Code) >= 2 && pgErr.Code[:2] == "28" {
			return ErrInvalidCredentials
		}
		// Too many connections for the role, database missing, and so on.
		return fmt.Errorf("%w: SQLSTATE %s", ErrUnavailable, pgErr.Code)
	}
	return fmt.Errorf("%w: %w", ErrUnavailable, err)
}

// currentSQL tells whether a role still exists, can log in and has not passed
// VALID UNTIL, and lists the cluster roles it is a member of now. It is run
// with the service connection: the answer reflects the catalog at this
// moment, not the moment of sign-in.
const currentSQL = `
WITH RECURSIVE me AS (
    SELECT oid FROM pg_catalog.pg_roles
    WHERE rolname = $1 AND rolcanlogin AND (rolvaliduntil IS NULL OR rolvaliduntil > now())),
m(roleid) AS (
    SELECT a.roleid FROM pg_catalog.pg_auth_members a WHERE a.member = (SELECT oid FROM me) AND a.inherit_option
  UNION
    SELECT a.roleid FROM pg_catalog.pg_auth_members a JOIN m ON a.member = m.roleid WHERE a.inherit_option)
SELECT (SELECT count(*) FROM me)::int,
       coalesce((SELECT array_agg(r.rolname::text) FROM pg_catalog.pg_roles r
                 WHERE r.oid IN (SELECT roleid FROM m)
                   AND r.rolname IN ('kingres_admin', 'kingres_ops', 'kingres_readonly', 'kingres_stats')), '{}')`

// CurrentRoles implements Authenticator: the role must exist, be able to log
// in and not be expired (otherwise ErrInvalidCredentials), and its cluster
// roles are derived again from pg_auth_members.
func (p *Postgres) CurrentRoles(ctx context.Context, username string) ([]string, error) {
	cfg := p.config()
	if cfg.RuntimeParams == nil {
		cfg.RuntimeParams = map[string]string{}
	}
	cfg.RuntimeParams["application_name"] = "kingresd-session"
	cctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	conn, err := pgx.ConnectConfig(cctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()
	var n int
	var roles []string
	if err := conn.QueryRow(cctx, currentSQL, username).Scan(&n, &roles); err != nil {
		return nil, fmt.Errorf("%w: reading the role: %w", ErrUnavailable, err)
	}
	if n == 0 {
		return nil, ErrInvalidCredentials
	}
	return Normalize(roles), nil
}
