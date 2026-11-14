// SPDX-License-Identifier: PostgreSQL

// Package pgtest starts throwaway PostgreSQL instances for integration tests,
// created and managed by the instance manager itself. A test calls Start; it
// is skipped unless KINGRES_PG_BIN names the directory with the KingresDB
// binaries (the tests run inside WSL against the Linux build).
//
// Environment: KINGRES_PG_BIN (required), KINGRES_TEST_ROOT (parent of the
// throwaway directories, default the system temp directory) and
// KINGRES_TEST_PORT_BASE/KINGRES_TEST_PORT_END (default 56000 and 56199).
package pgtest

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/instance"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/pgconninfo"
)

// Password is the password of the service role of every test instance.
const Password = "pgtest-service-secret"

// Instance is a running test instance.
type Instance struct {
	// Manager manages it; ID is its node id.
	Manager *instance.Manager
	ID      int
	// Port is the TCP port on 127.0.0.1.
	Port int
	// Root is the directory of the manager.
	Root string
}

// DSN returns a connection string for the service role and database db.
func (i *Instance) DSN(db string) string {
	dsn, err := pgconninfo.Format(pgconninfo.Pair{Key: "host", Value: "127.0.0.1"}, pgconninfo.Pair{Key: "port", Value: strconv.Itoa(i.Port)}, pgconninfo.Pair{Key: "dbname", Value: db},
		pgconninfo.Pair{Key: "user", Value: instance.ServiceRole}, pgconninfo.Pair{Key: "password", Value: Password}, pgconninfo.Pair{Key: "sslmode", Value: "disable"})
	if err != nil {
		panic(err) // test helper: db is a literal
	}
	return dsn
}

// Connect opens a connection as the service role.
func (i *Instance) Connect(t testing.TB, db string) *pgx.Conn {
	t.Helper()
	cfg, err := pgx.ParseConfig(i.DSN(db))
	if err != nil {
		t.Fatal(err)
	}
	c, err := pgx.ConnectConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("connecting to the test instance: %v", err)
	}
	t.Cleanup(func() { _ = c.Close(context.Background()) })
	return c
}

var (
	portMu    sync.Mutex
	usedPorts = map[int]bool{}
)

// BinDir returns KINGRES_PG_BIN or "".
func BinDir() string { return os.Getenv("KINGRES_PG_BIN") }

// Available reports whether instances can be started.
func Available() bool { return BinDir() != "" }

// FreePort finds a free TCP port in the test range.
func FreePort(t testing.TB) int {
	t.Helper()
	portMu.Lock()
	defer portMu.Unlock()
	lo, hi := envInt("KINGRES_TEST_PORT_BASE", 56000), envInt("KINGRES_TEST_PORT_END", 56199)
	for p := lo; p <= hi; p++ {
		if usedPorts[p] {
			continue
		}
		l, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(p))
		if err != nil {
			continue
		}
		_ = l.Close()
		usedPorts[p] = true // never hand out the same port twice in one test binary
		return p
	}
	t.Fatalf("no free port in %d..%d", lo, hi)
	return 0
}

func envInt(k string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(k)); err == nil {
		return v
	}
	return def
}

// Start creates, starts and registers cleanup of an instance with the given
// managed rules (nil gets a rule that lets any local role in with a password).
func Start(t testing.TB, hba []instance.HBARule) *Instance {
	t.Helper()
	if !Available() {
		t.Skip("KINGRES_PG_BIN is not set; skipping the test that needs a real PostgreSQL instance")
	}
	root, err := os.MkdirTemp(os.Getenv("KINGRES_TEST_ROOT"), "kgt-")
	if err != nil {
		t.Fatal(err)
	}
	mgr, err := instance.New(instance.Options{Root: root, BinDir: BinDir()})
	if err != nil {
		t.Fatal(err)
	}
	if hba == nil {
		hba = []instance.HBARule{{Type: "host", Databases: []string{"all"}, Users: []string{"all"},
			Address: "127.0.0.0/8", Method: "scram-sha-256"}}
	}
	in := &Instance{Manager: mgr, ID: 1, Port: FreePort(t), Root: root}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	err = mgr.Init(ctx, instance.Spec{ID: 1, SuperuserPassword: Password, HBA: hba,
		Settings: instance.ManagedSettings{Port: in.Port, ListenAddresses: "127.0.0.1", MaxConnections: 50}})
	if err != nil {
		_ = os.RemoveAll(root)
		t.Fatalf("init: %v", err)
	}
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_ = mgr.Stop(c, 1)
		_ = os.RemoveAll(root)
	})
	if err := mgr.Start(ctx, 1); err != nil {
		t.Fatalf("start: %v", err)
	}
	return in
}

// StartNoCleanup is Start for helpers that outlive one test (a fixture shared
// by the tests of a package): the caller stops and removes the instance with
// Stop. A fixed set of HBA rules lets the service role replicate from
// 127.0.0.1 (for a standby made with pg_basebackup).
func StartNoCleanup(t testing.TB, hba []instance.HBARule) *Instance {
	t.Helper()
	if !Available() {
		t.Skip("KINGRES_PG_BIN is not set; skipping the test that needs a real PostgreSQL instance")
	}
	root, err := os.MkdirTemp(os.Getenv("KINGRES_TEST_ROOT"), "kgt-")
	if err != nil {
		t.Fatal(err)
	}
	mgr, err := instance.New(instance.Options{Root: root, BinDir: BinDir()})
	if err != nil {
		t.Fatal(err)
	}
	if hba == nil {
		hba = []instance.HBARule{{Type: "host", Databases: []string{"all"}, Users: []string{"all"}, Address: "127.0.0.0/8", Method: "scram-sha-256"}}
	}
	in := &Instance{Manager: mgr, ID: 1, Port: FreePort(t), Root: root}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if err := mgr.Init(ctx, instance.Spec{ID: 1, SuperuserPassword: Password, HBA: hba,
		Settings: instance.ManagedSettings{Port: in.Port, ListenAddresses: "127.0.0.1", MaxConnections: 50}}); err != nil {
		_ = os.RemoveAll(root)
		t.Fatalf("init: %v", err)
	}
	// Replication connections need a rule that the managed rules cannot
	// express (the keyword "replication" would be quoted); test instances only.
	hbaPath := filepath.Join(root, "1", "pgdata", "pg_hba.conf")
	b, _ := os.ReadFile(hbaPath)
	b = append(b, []byte("host replication kingresd 127.0.0.1/32 scram-sha-256\n")...)
	if err := os.WriteFile(hbaPath, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := mgr.Start(ctx, 1); err != nil {
		t.Fatalf("start: %v", err)
	}
	return in
}

// Stop stops the instance and removes its directory.
func (i *Instance) Stop() {
	c, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	_ = i.Manager.Stop(c, 1)
	_ = os.RemoveAll(i.Root)
}

// ConnectRaw opens a service connection to database postgres without test
// cleanup; the caller closes it.
func (i *Instance) ConnectRaw() (*pgx.Conn, error) {
	cfg, err := pgx.ParseConfig(i.DSN("postgres"))
	if err != nil {
		return nil, err
	}
	return pgx.ConnectConfig(context.Background(), cfg)
}

// FreePortPair returns p and p+1, both free on 127.0.0.1 (a server's REST API
// and the internal API above it).
func FreePortPair(t testing.TB) int {
	t.Helper()
	portMu.Lock()
	defer portMu.Unlock()
	lo, hi := envInt("KINGRES_TEST_PORT_BASE", 56000), envInt("KINGRES_TEST_PORT_END", 56199)
	for p := lo; p < hi; p++ {
		if usedPorts[p] || usedPorts[p+1] {
			continue
		}
		l1, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(p))
		if err != nil {
			continue
		}
		l2, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(p+1))
		_ = l1.Close()
		if err != nil {
			continue
		}
		_ = l2.Close()
		usedPorts[p], usedPorts[p+1] = true, true
		return p
	}
	t.Fatalf("no free pair of ports in %d..%d", lo, hi)
	return 0
}

// ReleasePorts makes ports handed out by FreePort or FreePortPair available
// again. Only a test that owns every process that used them may call it (the
// multi-server tests do, after stopping their nodes); the shared cluster of a
// test binary never does.
func ReleasePorts(ports ...int) {
	portMu.Lock()
	defer portMu.Unlock()
	for _, p := range ports {
		delete(usedPorts, p)
	}
}
