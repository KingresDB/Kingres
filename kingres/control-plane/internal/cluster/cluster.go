// SPDX-License-Identifier: AGPL-3.0-only

// Package cluster implements the cluster-level operations that need the
// instance manager and the task engine: creating a one-server cluster
// (design control-plane-2.md, D5) and the service credentials of the
// managed instance.
package cluster

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/fault"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/instance"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/pgconninfo"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/setup"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/task"
)

// NodeID is the node id of the first instance of a server.
const NodeID = 1

// File names below the data directory.
const (
	// CredentialsFile holds the password of the service role (mode 0600).
	CredentialsFile = "service-credentials"
	// SetupTaskFile holds the setup task until the system database exists.
	SetupTaskFile = "setup-task.json"
	// InstancesDir holds the instance directories.
	InstancesDir = "instances"
)

// Config configures the Service.
type Config struct {
	// DataDir is the data directory of kingresd.
	DataDir string
	// BinDir holds the PostgreSQL binaries; when empty the service cannot
	// manage instances and Enabled reports false.
	BinDir string
	// SocketDir is the base directory of the instances' unix sockets.
	SocketDir string
	// PGPort is the TCP port of the managed instance (default 5432).
	PGPort int
	// PGListen is listen_addresses of the instance (default 127.0.0.1).
	PGListen string
	// ServerHost is the host name this server registers in the catalog; it is
	// the server id.
	ServerHost string
	// InternalPort is the port of the internal API listener (0 when off).
	InternalPort int
	// Version is the kingresd version.
	Version string
	// CertificateLifetime is the validity of the certificates this server's CA
	// signs (0: pki.ServerLifetime).
	CertificateLifetime time.Duration
	// TLSCertFile and TLSKeyFile enable TLS on the instance.
	TLSCertFile, TLSKeyFile string
	// Logger receives diagnostics, never secrets.
	Logger *slog.Logger
	// Fault is the fault-injection hook of the tests (nil in production).
	Fault *fault.Hook
}

// Service creates the cluster and manages the local instance.
type Service struct {
	cfg   Config
	log   *slog.Logger
	mgr   *instance.Manager
	setup *setup.Manager

	initEngine *task.Engine
	initStore  *task.FileStore

	pki      pkiState
	memberMu sync.Mutex
	// resetting is set while ResetMember deletes the certificates (see Resetting).
	resetting atomic.Bool
	proxyMu   sync.Mutex
	identRef  func() error

	mu       sync.Mutex
	dsnSinks []func(dsn string) error
	// catalogPrimaries is what the last SyncRoles read: the TCP ports of this server's primary rows of the catalog,
	// by group (roles.go, Unattributed).
	catalogPrimaries map[int32][]int
	bg               context.Context
	wg               sync.WaitGroup
}

// New builds the service. When cfg.BinDir is empty the service is disabled:
// Enabled returns false and Init answers ErrDisabled.
func New(cfg Config, node *setup.Manager) (*Service, error) {
	if cfg.PGPort == 0 {
		cfg.PGPort = 5432
	}
	if cfg.PGListen == "" {
		cfg.PGListen = "127.0.0.1"
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	s := &Service{cfg: cfg, log: cfg.Logger, setup: node, bg: context.Background()}
	if cfg.BinDir == "" {
		return s, nil
	}
	mgr, err := instance.New(instance.Options{Root: filepath.Join(cfg.DataDir, InstancesDir), BinDir: cfg.BinDir, SocketDir: cfg.SocketDir, Logger: cfg.Logger})
	if err != nil {
		return nil, err
	}
	s.mgr = mgr
	fstore, err := task.OpenFileStore(filepath.Join(cfg.DataDir, SetupTaskFile), s.onSetupTaskChange)
	if err != nil {
		return nil, err
	}
	s.initStore = fstore
	eng, err := task.NewEngine(fstore, []task.Definition{s.initDefinition()}, task.Options{Holder: "kingresd-setup", Logger: cfg.Logger})
	if err != nil {
		return nil, err
	}
	s.initEngine = eng
	if err := eng.Register(s.joinDefinition()); err != nil {
		return nil, err
	}
	return s, nil
}

// ErrDisabled means instance management is not configured (no pg-bin-dir).
var ErrDisabled = errors.New("instance management is not configured (pg-bin-dir)")

// Enabled reports whether instances can be managed.
func (s *Service) Enabled() bool { return s.mgr != nil }

// Manager returns the instance manager (nil when disabled).
func (s *Service) Manager() *instance.Manager { return s.mgr }

// OnDSN registers a function that receives the service DSN of the managed
// instance whenever it becomes known or changes.
func (s *Service) OnDSN(f func(dsn string) error) {
	s.mu.Lock()
	s.dsnSinks = append(s.dsnSinks, f)
	s.mu.Unlock()
}

// ServiceDSN returns the DSN of the service role for database db, or "" when
// no credentials exist yet.
func (s *Service) ServiceDSN(db string) string { return s.serviceDSN(db, false) }

// serviceDSN builds the DSN; viaSocket uses the instance's unix socket (only the
// service role has a password rule there, so it is for service connections, not
// for the sign-in authenticator).
func (s *Service) serviceDSN(db string, viaSocket bool) string {
	pw, err := s.readCredentials()
	if err != nil {
		return ""
	}
	// Every value goes through pgconninfo, which quotes it: a database name
	// from a request path can never add or replace a keyword (CP-01). The
	// host is the instance's unix socket directory when it is known, so the
	// service password does not travel over TCP at all; otherwise loopback.
	host, sslmode := "127.0.0.1", s.sslMode()
	if viaSocket && s.mgr != nil {
		if sock, err := s.mgr.SocketDir(NodeID); err == nil {
			host, sslmode = filepath.ToSlash(sock), "disable"
		}
	}
	dsn, err := pgconninfo.Format(
		pgconninfo.Pair{Key: "host", Value: host},
		pgconninfo.Pair{Key: "port", Value: strconv.Itoa(s.cfg.PGPort)},
		pgconninfo.Pair{Key: "dbname", Value: db},
		pgconninfo.Pair{Key: "user", Value: instance.ServiceRole},
		pgconninfo.Pair{Key: "password", Value: pw},
		pgconninfo.Pair{Key: "sslmode", Value: sslmode},
	)
	if err != nil {
		return ""
	}
	return dsn
}

func (s *Service) sslMode() string {
	if s.cfg.TLSCertFile != "" {
		return "prefer"
	}
	return "disable"
}

func (s *Service) credentialsPath() string { return filepath.Join(s.cfg.DataDir, CredentialsFile) }

func (s *Service) readCredentials() (string, error) {
	b, err := os.ReadFile(s.credentialsPath())
	if err != nil {
		return "", err
	}
	pw := strings.TrimSpace(string(b))
	if pw == "" {
		return "", errors.New("empty service credentials")
	}
	return pw, nil
}

// credentials returns the service password, creating the file with a random
// password (mode 0600, exclusive create) when it does not exist.
func (s *Service) credentials() (string, error) {
	if pw, err := s.readCredentials(); err == nil {
		return pw, nil
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	pw := hex.EncodeToString(raw)
	f, err := os.OpenFile(s.credentialsPath(), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return s.readCredentials()
		}
		return "", err
	}
	_, werr := f.WriteString(pw + "\n")
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		_ = os.Remove(s.credentialsPath())
		return "", werr
	}
	return pw, nil
}

func (s *Service) publishDSN() {
	dsn := s.ServiceDSN("kingres_system")
	if dsn == "" {
		return
	}
	s.mu.Lock()
	sinks := append([]func(string) error(nil), s.dsnSinks...)
	s.mu.Unlock()
	for _, f := range sinks {
		if err := f(dsn); err != nil {
			s.log.Error("applying the service connection", "error", err)
		}
	}
}

// Resume starts the managed instance when the node is already set up and
// resumes an interrupted setup task. bg bounds background work; Wait waits
// for it. The DSN is published to the registered sinks.
func (s *Service) Resume(bg context.Context) error {
	s.bg = bg
	if !s.Enabled() {
		return nil
	}
	if s.mgr.Exists(NodeID) && (s.setup.State() != setup.Uninitialized || s.Joining()) {
		ctx, cancel := context.WithTimeout(bg, 2*time.Minute)
		defer cancel()
		if err := s.startWritable(ctx); err != nil {
			if errors.Is(err, instance.ErrLeaseRequired) {
				// The instance fenced itself (lease lost) and may not run as a primary
				// until the lease manager holds a lease again; it starts it then.
				s.log.Warn("the local instance is not started: its group lease was lost or is not held yet; it starts when the lease is held")
				s.publishDSN()
				return nil
			}
			return fmt.Errorf("starting the local instance: %w", err)
		}
		s.publishDSN()
		if s.setup.State() != setup.Uninitialized {
			if err := s.MigrateSystem(ctx); err != nil {
				s.log.Error("upgrading the system database at start", "error", err)
			}
			return nil
		}
	}
	if s.setup.State() == setup.Uninitialized {
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.initEngine.Run(bg)
		}()
	}
	return nil
}

// Wait waits for background work started by the service.
func (s *Service) Wait() { s.wg.Wait() }

// StopInstance stops the managed instance at shutdown (fast mode).
func (s *Service) StopInstance(ctx context.Context) error {
	if !s.Enabled() || !s.mgr.Exists(NodeID) {
		return nil
	}
	first := s.stopStandbys(ctx)
	for _, id := range s.WritableIDs() {
		if err := s.mgr.Stop(ctx, id); err != nil {
			return err
		}
	}
	return first
}

// StopInstanceForShutdown is StopInstance for the orderly stop of the service
// (kingresd -stop-instances): the primary instance is asked for a fast shutdown
// without waiting, requested is called with the identity of the postmaster that
// was asked, and only then the stop is awaited (instance.Manager.StopForShutdown).
func (s *Service) StopInstanceForShutdown(ctx context.Context, requested func(id int, postmaster string)) error {
	if !s.Enabled() || !s.mgr.Exists(NodeID) {
		return nil
	}
	first := s.stopStandbys(ctx)
	for _, id := range s.WritableIDs() {
		if err := s.mgr.StopForShutdown(ctx, id, func(pm string) { requested(id, pm) }); err != nil {
			return err
		}
	}
	return first
}

// connect opens a connection as the service role.
func (s *Service) connect(ctx context.Context, db string) (*pgx.Conn, error) {
	dsn := s.serviceDSN(db, true)
	if dsn == "" {
		return nil, errors.New("no service credentials")
	}
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, errors.New("parsing the service connection string") // never include the DSN
	}
	cfg.ConnectTimeout = 5 * time.Second
	cfg.RuntimeParams["application_name"] = "kingresd-setup"
	return pgx.ConnectConfig(ctx, cfg)
}

// waitReady waits until the service role can connect to database postgres.
func (s *Service) waitReady(ctx context.Context) error {
	deadline := time.Now().Add(60 * time.Second)
	var last error
	for time.Now().Before(deadline) {
		c, err := s.connect(ctx, "postgres")
		if err == nil {
			_ = c.Close(ctx)
			return nil
		}
		last = err
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	return fmt.Errorf("the instance did not accept connections: %w", last)
}

// newUUID returns a random version 4 UUID.
func newUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:], nil
}

func itoa(i int) string { return strconv.Itoa(i) }

// ServiceConn opens a connection to database db as the service role. What is per
// server lives on the home instance (instance NodeID), which is where it goes,
// except that a server that runs the primary of the map group in another directory
// (a promoted standby) is reached there: the orchestration of the cluster asks the
// map primary. Code that needs the primary of a group or every writable
// instance uses PrimaryConn and EachWritable (roles.go). The caller closes it.
func (s *Service) ServiceConn(ctx context.Context, db string) (*pgx.Conn, error) {
	// The orchestration of the cluster (the catalog, distributed DDL, the system database) goes to the
	// primary of the map group when this server runs it, whichever directory that is; anything else,
	// and every server that does not run it, uses the home instance.
	if li, ok := s.PrimaryOf(0); ok && li.Role.Group == 0 && li.ID != NodeID {
		return s.InstanceConn(ctx, li.ID, db)
	}
	return s.homeConn(ctx, db)
}

// homeConn is the connection to the home instance.
func (s *Service) homeConn(ctx context.Context, db string) (*pgx.Conn, error) {
	if s.mgr != nil && s.mgr.Exists(NodeID) && s.mgr.IsCopy(NodeID) {
		// The home instance was rebuilt from another server's copy: it has no password of ours.
		return s.ServiceConnAt(ctx, NodeID, s.cfg.PGPort, db)
	}
	return s.connect(ctx, db)
}
