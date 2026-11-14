// SPDX-License-Identifier: PostgreSQL

package instance_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/instance"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/pgtest"
)

func TestNewRefusals(t *testing.T) {
	if _, err := instance.New(instance.Options{Root: t.TempDir(), BinDir: t.TempDir()}); err == nil {
		t.Error("a bin directory without initdb and pg_ctl was accepted")
	}
	if _, err := instance.New(instance.Options{BinDir: t.TempDir()}); err == nil {
		t.Error("empty root accepted")
	}
}

func TestDirRejectsBadIDs(t *testing.T) {
	bin := t.TempDir()
	for _, n := range []string{"initdb", "pg_ctl"} {
		if err := os.WriteFile(filepath.Join(bin, n), []byte("x"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	m, err := instance.New(instance.Options{Root: t.TempDir(), BinDir: bin})
	if err != nil {
		t.Skipf("cannot construct a manager here: %v", err) // running as root
	}
	for _, id := range []int{0, -1, 1 << 30} {
		if _, err := m.Dir(id); !errors.Is(err, instance.ErrInvalid) {
			t.Errorf("id %d accepted", id)
		}
	}
	// A directory without our marker is refused by every operation.
	dir, _ := m.Dir(7)
	if err := os.MkdirAll(filepath.Join(dir, "pgdata"), 0o700); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for name, err := range map[string]error{
		"start":   m.Start(ctx, 7),
		"stop":    m.Stop(ctx, 7),
		"reload":  m.Reload(ctx, 7),
		"hba":     m.WriteHBA(7, nil, false),
		"managed": m.WriteManaged(7, instance.ManagedSettings{Port: 56000}),
		"init":    m.Init(ctx, instance.Spec{ID: 7, SuperuserPassword: "x", Settings: instance.ManagedSettings{Port: 56000}}),
	} {
		if err == nil {
			t.Errorf("%s accepted an unmanaged directory", name)
		}
	}
	// A symlink or a marker of another node is not ours.
	other := `{"nodeId":8,"port":1,"magic":"kingresd-instance-v1"}`
	if err := os.WriteFile(filepath.Join(dir, instance.MarkerName), []byte(other), 0o600); err != nil {
		t.Fatal(err)
	}
	if m.Exists(7) {
		t.Error("marker of node 8 accepted for node 7")
	}
}

func TestLifecycle(t *testing.T) {
	in := pgtest.Start(t, nil)
	ctx := context.Background()
	m := in.Manager

	if !m.Exists(1) {
		t.Fatal("marker missing")
	}
	if up, err := m.Running(ctx, 1); err != nil || !up {
		t.Fatalf("running = %v, %v", up, err)
	}
	conn := in.Connect(t, "postgres")
	show := func(c *pgx.Conn, name string) string {
		var v string
		if err := c.QueryRow(ctx, "SELECT current_setting($1)", name).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	if got := show(conn, "shared_preload_libraries"); got != "kingres_shard,kingres_ledger" {
		t.Errorf("shared_preload_libraries = %q", got)
	}
	if got := show(conn, "password_encryption"); got != "scram-sha-256" {
		t.Errorf("password_encryption = %q", got)
	}
	if got := show(conn, "max_prepared_transactions"); got == "0" {
		t.Error("max_prepared_transactions is 0")
	}
	if got := show(conn, "ssl"); got != "off" {
		t.Errorf("ssl = %q without certificates", got)
	}
	// Both extensions can be created because both libraries are preloaded.
	for _, ext := range []string{"kingres_shard", "kingres_ledger"} {
		if _, err := conn.Exec(ctx, "CREATE EXTENSION IF NOT EXISTS "+ext); err != nil {
			t.Errorf("CREATE EXTENSION %s: %v", ext, err)
		}
	}
	// The password never stays on disk.
	if _, err := os.Stat(filepath.Join(in.Root, "1", "pwfile")); err == nil {
		t.Error("pwfile left behind")
	}

	// Starting a running instance and stopping a stopped one are no-ops.
	if err := m.Start(ctx, 1); err != nil {
		t.Errorf("second start: %v", err)
	}
	var pid1, pid2 int
	_ = conn.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&pid1)
	if err := m.Restart(ctx, 1); err != nil {
		t.Fatalf("restart: %v", err)
	}
	conn2 := in.Connect(t, "postgres")
	_ = conn2.QueryRow(ctx, "SELECT 1").Scan(&pid2)
	if err := conn.Ping(ctx); err == nil {
		t.Error("old connection survived a restart")
	}
	if err := m.Stop(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if up, _ := m.Running(ctx, 1); up {
		t.Error("still running after stop")
	}
	if err := m.Stop(ctx, 1); err != nil {
		t.Errorf("second stop: %v", err)
	}
	if err := m.Reload(ctx, 1); !errors.Is(err, instance.ErrNotRunning) {
		t.Errorf("reload of a stopped instance: %v", err)
	}
	if err := m.Start(ctx, 1); err != nil {
		t.Fatalf("start after stop: %v", err)
	}
}

func TestHBAChangeTakesEffect(t *testing.T) {
	in := pgtest.Start(t, nil)
	ctx := context.Background()
	m := in.Manager
	// Create a login role through the service connection.
	svc := in.Connect(t, "postgres")
	if _, err := svc.Exec(ctx, "CREATE ROLE bob LOGIN PASSWORD 'bobpw'"); err != nil {
		t.Fatal(err)
	}
	bob := func() error {
		cfg, _ := pgx.ParseConfig(strings.Replace(in.DSN("postgres"), "user=kingresd password="+pgtest.Password, "user=bob password=bobpw", 1))
		c, err := pgx.ConnectConfig(ctx, cfg)
		if err == nil {
			_ = c.Close(ctx)
		}
		return err
	}
	if err := bob(); err != nil {
		t.Fatalf("bob cannot connect before the change: %v", err)
	}
	reject := []instance.HBARule{
		{Type: "host", Databases: []string{"all"}, Users: []string{"bob"}, Address: "127.0.0.0/8", Method: "reject"},
		{Type: "host", Databases: []string{"all"}, Users: []string{"all"}, Address: "127.0.0.0/8", Method: "scram-sha-256"},
	}
	if err := m.WriteHBA(1, reject, false); err != nil {
		t.Fatal(err)
	}
	if err := m.Reload(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if err := bob(); err == nil {
		t.Error("bob can still connect after the reject rule was reloaded")
	}
	// The service role is never locked out by managed rules.
	if c := in.Connect(t, "postgres"); c == nil {
		t.Error("service role locked out")
	}
	// An invalid rule set leaves pg_hba.conf as it was.
	before, _ := os.ReadFile(filepath.Join(in.Root, "1", "pgdata", "pg_hba.conf"))
	if err := m.WriteHBA(1, []instance.HBARule{{Type: "host", Databases: []string{"a b"}, Users: []string{"u"}, Address: "all", Method: "reject"}}, false); err == nil {
		t.Error("invalid rule accepted")
	}
	after, _ := os.ReadFile(filepath.Join(in.Root, "1", "pgdata", "pg_hba.conf"))
	if string(before) != string(after) {
		t.Error("pg_hba.conf changed by a rejected rule set")
	}
}

func TestInitFailureLeavesNothing(t *testing.T) {
	if !pgtest.Available() {
		t.Skip("KINGRES_PG_BIN not set")
	}
	root := t.TempDir()
	m, err := instance.New(instance.Options{Root: root, BinDir: pgtest.BinDir()})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	bad := instance.Spec{ID: 1, SuperuserPassword: "x", Settings: instance.ManagedSettings{Port: 56000, Extra: map[string]string{"hba_file": "/etc/passwd"}}}
	if err := m.Init(ctx, bad); err == nil {
		t.Fatal("bad spec accepted")
	}
	if ents, _ := os.ReadDir(root); len(ents) != 0 {
		t.Errorf("bad spec left %d entries", len(ents))
	}
	good := instance.Spec{ID: 1, SuperuserPassword: "x", Settings: instance.ManagedSettings{Port: pgtest.FreePort(t)}, Locale: "no/such;locale"}
	if err := m.Init(ctx, good); err == nil {
		t.Fatal("bad locale accepted")
	}
	good.Locale = "C"
	good.Settings.Port = pgtest.FreePort(t)
	if err := m.Init(ctx, good); err != nil {
		t.Fatal(err)
	}
	if err := m.Init(ctx, good); !errors.Is(err, instance.ErrExists) {
		t.Errorf("second init: %v", err)
	}
}
