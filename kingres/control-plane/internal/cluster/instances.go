// SPDX-License-Identifier: PostgreSQL

package cluster

import (
	"context"
	"errors"
	"path/filepath"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/instance"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/pgconninfo"
)

// ServiceConnAt opens a service connection to the local instance with directory
// id id that listens on port, through its unix socket (the service password then
// never travels over TCP). It is how kingresd looks at a standby instance of
// this server; ServiceConn is the same for the primary instance.
func (s *Service) ServiceConnAt(ctx context.Context, id, port int, db string) (*pgx.Conn, error) {
	if !s.Enabled() {
		return nil, ErrDisabled
	}
	// A standby is a copy of another server's data and has that server's service password: it is
	// reached by peer authentication over its private socket instead (instance.StandbyIdentMap).
	pw := ""
	if !instance.IsStandbyDirID(id) {
		var err error
		if pw, err = s.readCredentials(); err != nil {
			return nil, errors.New("no service credentials")
		}
	}
	sock, err := s.mgr.SocketDir(id)
	if err != nil {
		return nil, err
	}
	dsn, err := pgconninfo.Format(
		pgconninfo.Pair{Key: "host", Value: filepath.ToSlash(sock)},
		pgconninfo.Pair{Key: "port", Value: strconv.Itoa(port)},
		pgconninfo.Pair{Key: "dbname", Value: db},
		pgconninfo.Pair{Key: "user", Value: instance.ServiceRole},
		pgconninfo.Pair{Key: "password", Value: pw},
		pgconninfo.Pair{Key: "sslmode", Value: "disable"},
	)
	if err != nil {
		return nil, errors.New("building the service connection string")
	}
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, errors.New("parsing the service connection string") // never include the DSN
	}
	cfg.ConnectTimeout = 5 * time.Second
	cfg.RuntimeParams["application_name"] = "kingresd"
	return pgx.ConnectConfig(ctx, cfg)
}

// RemoteConn opens a connection to the instance of another server, as the
// service role, authenticated by this server's certificate (no password): the
// same way the sharding extension and the catalog reads reach other servers. The
// peer is verified against the cluster CA and the name host.
func (s *Service) RemoteConn(ctx context.Context, host string, port int, db string) (*pgx.Conn, error) {
	dsn, err := pgconninfo.Format(
		pgconninfo.Pair{Key: "host", Value: host},
		pgconninfo.Pair{Key: "port", Value: strconv.Itoa(port)},
		pgconninfo.Pair{Key: "dbname", Value: db},
		pgconninfo.Pair{Key: "user", Value: instance.ServiceRole},
		pgconninfo.Pair{Key: "sslmode", Value: "require"},
	)
	if err != nil {
		return nil, errors.New("building the node connection string")
	}
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, errors.New("parsing the node connection string")
	}
	tc, err := s.NodeTLSConfig(host)
	if err != nil {
		return nil, errors.New("no certificate for the node connection")
	}
	cfg.TLSConfig, cfg.Password, cfg.Fallbacks = tc, "", nil
	cfg.ConnectTimeout = 5 * time.Second
	cfg.RuntimeParams["application_name"] = "kingresd"
	return pgx.ConnectConfig(ctx, cfg)
}

// ReplicationConninfo is the connection string of a standby's connection to its
// primary (and of pg_basebackup and pg_rewind): host, port, the service role, the
// application name and the certificate options that name this server's files.
// No password is in it; the primary maps the certificate name to the role
// (pg_ident map kingres_nodes, rule "hostssl replication"). dbname is for
// pg_rewind, which needs a database connection; empty leaves it out.
func (s *Service) ReplicationConninfo(host string, port int, application, dbname string) (string, error) {
	pairs := []pgconninfo.Pair{
		{Key: "host", Value: host},
		{Key: "port", Value: strconv.Itoa(port)},
		{Key: "user", Value: instance.ServiceRole},
		{Key: "application_name", Value: application},
	}
	if dbname != "" {
		pairs = append(pairs, pgconninfo.Pair{Key: "dbname", Value: dbname})
	}
	base, err := pgconninfo.Format(pairs...)
	if err != nil {
		return "", err
	}
	tls, err := s.withTLS(instance.ManagedSettings{})
	if err != nil {
		return "", err
	}
	return base + " " + tls.NodeConninfo, nil
}

// PKIFile returns the path of a file of this server's pki directory (for the
// modification checks that tell a standby supervisor to reload its instances).
func (s *Service) PKIFile(name string) string { return s.pkiPath(name) }
