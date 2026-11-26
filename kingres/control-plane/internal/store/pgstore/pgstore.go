// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

// Package pgstore implements store.Store on PostgreSQL. It reads the
// KingresDB catalog (schema kingres, specified in
// kingres/docs/design/shard-mvp.md and ledger-mvp.md) and the PostgreSQL
// system catalogs through the service connection configured as the DSN.
//
// The kingres catalog is per database (its extensions are per database), so
// reads that concern one database use a connection pool for that database,
// created on first use and only for databases that exist in pg_database.
//
// Every query tolerates a missing catalog table: the extension may not be
// installed yet, and the answer is then an empty but valid result.
package pgstore

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/cluster"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/extguard"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ha"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/store"
)

// Options configures a Store.
type Options struct {
	// DSN is the connection string of the local map-group instance. The
	// role needs CONNECT on the databases and, for full monitoring data,
	// membership in pg_monitor.
	DSN string
	// Logger receives diagnostics; it must not be nil.
	Logger *slog.Logger
	// PanelPort is the REST/panel port used to form server addresses
	// (host:port) for servers other than this one.
	PanelPort string
	// Version is the KingresDB version reported for servers.
	Version string
	// Started is the start time of this process, for the local uptime.
	Started time.Time
	// Now is the clock; time.Now when nil.
	Now func() time.Time
	// ConnectTimeout bounds opening a connection; 3 s when zero.
	ConnectTimeout time.Duration
	// NodeTLS, when set, gives the TLS configuration for connections to the
	// instances of other servers (and this server's own address as registered in
	// the catalog): the cluster certificate is the client certificate and
	// replaces the password (design D3). nil: the local credentials are used.
	NodeTLS func(host string) (*tls.Config, error)
	// HA connects the store to heartbeats, leases and standbys (nil: none are
	// running; the catalog alone decides).
	HA *ha.Registry
}

// Store is the PostgreSQL implementation of store.Store.
type Store struct {
	opts Options
	base *pgxpool.Config
	now  func() time.Time

	mu     sync.Mutex
	pools  map[string]*pgxpool.Pool
	closed bool
	ext    extState
}

var _ store.Store = (*Store)(nil)

// New parses the DSN and returns a Store. It does not connect: the instance
// may not be running yet (an uninitialized node), and connections are opened
// on demand.
func New(opts Options) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(opts.DSN)
	if err != nil {
		return nil, fmt.Errorf("parsing PostgreSQL DSN: %w", err)
	}
	if opts.ConnectTimeout == 0 {
		opts.ConnectTimeout = 3 * time.Second
	}
	cfg.ConnConfig.ConnectTimeout = opts.ConnectTimeout
	cfg.MaxConns = 4
	cfg.MinConns = 0
	cfg.MaxConnIdleTime = time.Minute
	cfg.ConnConfig.RuntimeParams["application_name"] = "kingresd"
	cluster.SafeSession(cfg.ConnConfig)
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Store{opts: opts, base: cfg, now: now, pools: map[string]*pgxpool.Pool{}}, nil
}

// Target returns "host:port/database" of the configured instance, without
// credentials, for logging.
func (s *Store) Target() string {
	s.mu.Lock()
	c := s.base.ConnConfig
	s.mu.Unlock()
	return net.JoinHostPort(c.Host, strconv.Itoa(int(c.Port))) + "/" + c.Database
}

// Close closes all connection pools.
func (s *Store) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	for _, p := range s.pools {
		p.Close()
	}
	s.pools = map[string]*pgxpool.Pool{}
}

// SetDSN replaces the connection string (for example after the control plane
// created the managed instance and learned its credentials) and closes the
// pools opened with the old one.
func (s *Store) SetDSN(dsn string) error {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return fmt.Errorf("parsing PostgreSQL DSN: %w", err)
	}
	cfg.ConnConfig.ConnectTimeout = s.opts.ConnectTimeout
	cfg.MaxConns, cfg.MinConns, cfg.MaxConnIdleTime = 4, 0, time.Minute
	cfg.ConnConfig.RuntimeParams["application_name"] = "kingresd"
	cluster.SafeSession(cfg.ConnConfig)
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.pools {
		p.Close()
	}
	s.pools = map[string]*pgxpool.Pool{}
	s.base = cfg
	return nil
}

// pool returns the pool of a database, creating it on first use. db "" means
// the database of the DSN.
func (s *Store) pool(ctx context.Context, db string) (*pgxpool.Pool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if db == "" {
		db = s.base.ConnConfig.Database
	}
	if s.closed {
		return nil, fmt.Errorf("%w: store closed", store.ErrUnavailable)
	}
	if p, ok := s.pools[db]; ok {
		return p, nil
	}
	cfg := s.base.Copy()
	cfg.ConnConfig.Database = db
	p, err := pgxpool.NewWithConfig(context.WithoutCancel(ctx), cfg)
	if err != nil {
		return nil, fmt.Errorf("creating pool for database %q: %w", db, err)
	}
	s.pools[db] = p
	return p, nil
}

// connectInstance opens a one-off connection to another instance of the
// cluster (host and port from kingres.node) with the service credentials.
func (s *Store) connectInstance(ctx context.Context, host string, port int) (*pgx.Conn, error) {
	if port < 1 || port > 65535 {
		return nil, fmt.Errorf("invalid port %d for node %s", port, host)
	}
	s.mu.Lock()
	cfg := s.base.ConnConfig.Copy()
	s.mu.Unlock()
	cfg.Host = host
	cfg.Port = uint16(port)
	if s.opts.NodeTLS != nil && host != "127.0.0.1" && host != "localhost" && host != "::1" {
		tc, err := s.opts.NodeTLS(host)
		if err != nil {
			return nil, fmt.Errorf("%w: no certificate for the node connection", store.ErrUnavailable)
		}
		cfg.TLSConfig, cfg.Password, cfg.Fallbacks = tc, "", nil
	} else {
		for _, fb := range cfg.Fallbacks {
			fb.Host = host
			fb.Port = uint16(port)
		}
	}
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return nil, s.wrap("connecting to instance", err)
	}
	return conn, nil
}

// wrap classifies a driver error: connection-level failures become
// store.ErrUnavailable, everything else is wrapped with the operation.
func (s *Store) wrap(op string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrInvalidCursor) {
		return err
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code[:2] {
		case "08", "53", "57", "28":
			return fmt.Errorf("%s: %w: %w", op, store.ErrUnavailable, err)
		}
		return fmt.Errorf("%s: %w", op, err)
	}
	if errors.Is(err, context.Canceled) {
		return fmt.Errorf("%s: %w", op, err)
	}
	// Dial errors, timeouts and closed connections.
	return fmt.Errorf("%s: %w: %w", op, store.ErrUnavailable, err)
}

// Ping implements store.Store.
func (s *Store) Ping(ctx context.Context) error {
	p, err := s.pool(ctx, "")
	if err != nil {
		return err
	}
	var one int
	if err := p.QueryRow(ctx, "SELECT 1").Scan(&one); err != nil {
		return s.wrap("ping", err)
	}
	return nil
}

// PostgresVersion implements store.Store.
func (s *Store) PostgresVersion(ctx context.Context) (string, error) {
	p, err := s.pool(ctx, "")
	if err != nil {
		return "", err
	}
	var v string
	if err := p.QueryRow(ctx, "SELECT current_setting('server_version')").Scan(&v); err != nil {
		return "", s.wrap("reading server version", err)
	}
	return v, nil
}

// regclassExists reports whether a relation such as "kingres.node" exists
// in the database. A missing extension is not an error.
func (s *Store) regclassExists(ctx context.Context, p *pgxpool.Pool, name string) (bool, error) {
	// A relation of the schema kingres counts only when the extension that defines it is installed and its schema is trusted (extguard): a name says nothing about
	// who made the object, and the readers that follow run it as the service role, a superuser.
	if strings.HasPrefix(name, "kingres.") {
		if ok, err := s.extReady(ctx, p, extguard.ObjectExtension(name)); err != nil || !ok {
			return false, err
		}
	}
	var ok bool
	if err := p.QueryRow(ctx, "SELECT pg_catalog.to_regclass($1) IS NOT NULL", name).Scan(&ok); err != nil {
		return false, s.wrap("checking for "+name, err)
	}
	return ok, nil
}

// isNoRows reports whether err is pgx.ErrNoRows.
func isNoRows(err error) bool { return errors.Is(err, pgx.ErrNoRows) }

// dbHandle is a verified database and its pool.
type dbHandle struct {
	name string
	pool *pgxpool.Pool
}
