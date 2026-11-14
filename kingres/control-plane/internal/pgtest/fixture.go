// SPDX-License-Identifier: PostgreSQL

package pgtest

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/pgconninfo"
)

// The integration tests of several packages read a PostgreSQL instance loaded with
// scripts/fixture_*.sql and a real streaming standby. The instance is created
// by the instance manager (package pgtest) once per test binary and removed in
// TestMain; the tests are skipped without KINGRES_PG_BIN.
var (
	fixtureOnce sync.Once
	fixtureDSN  string
	fixtureErr  error
	fixtureStop func()
)

func StopFixture() {
	if fixtureStop != nil {
		fixtureStop()
	}
}

func fixtureDir() string {
	if d := os.Getenv("KINGRES_FIXTURES"); d != "" {
		return d
	}
	return "../../../scripts"
}

// Fixture returns the DSN (role cp_service, database kingres_cp) of the
// shared fixture instance, creating it on first use. Call StopFixture in
// TestMain. It fails the test when the fixture cannot be built.
func Fixture(t testing.TB) string {
	t.Helper()
	dsn, err := setupFixture(t)
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	return dsn
}

func setupFixture(t testing.TB) (string, error) {
	fixtureOnce.Do(func() {
		in := StartNoCleanup(t, nil)
		fixtureStop = in.Stop
		bin := BinDir()
		psql := func(db string, extra ...string) error {
			args := append([]string{"-X", "-q", "-v", "ON_ERROR_STOP=1", "-h", "127.0.0.1", "-p", strconv.Itoa(in.Port), "-U", "kingresd", "-d", db}, extra...)
			cmd := exec.Command(filepath.Join(bin, "psql"), args...)
			cmd.Env = append(os.Environ(), "PGPASSWORD="+Password)
			if out, err := cmd.CombinedOutput(); err != nil {
				return fmt.Errorf("psql %v: %w\n%s", extra, err, out)
			}
			return nil
		}
		dir := fixtureDir()
		if fixtureErr = psql("postgres", "-f", filepath.Join(dir, "fixture_roles.sql")); fixtureErr != nil {
			return
		}
		standby := in.Port + 1
		if fixtureErr = psql("plain_cp", "-c", "create table public.plain_t (id int primary key, label text)"); fixtureErr != nil {
			return
		}
		if fixtureErr = psql("kingres_cp", "-v", "p1="+strconv.Itoa(in.Port), "-v", "p2="+strconv.Itoa(standby), "-f", filepath.Join(dir, "fixture_catalog.sql")); fixtureErr != nil {
			return
		}
		// A real streaming standby whose application_name is the one the
		// topology reader matches (kingres_node_2).
		sdir := filepath.Join(in.Root, "standby")
		env := append(os.Environ(), "PGPASSWORD="+Password)
		run := func(name string, args ...string) error {
			cmd := exec.Command(filepath.Join(bin, name), args...)
			cmd.Env = env
			if out, err := cmd.CombinedOutput(); err != nil {
				return fmt.Errorf("%s: %w\n%s", name, err, out)
			}
			return nil
		}
		if fixtureErr = run("pg_basebackup", "-h", "127.0.0.1", "-p", strconv.Itoa(in.Port), "-U", "kingresd", "-D", sdir, "-X", "stream", "-R", "-c", "fast"); fixtureErr != nil {
			return
		}
		primary, perr := pgconninfo.Format(pgconninfo.Pair{Key: "host", Value: "127.0.0.1"}, pgconninfo.Pair{Key: "port", Value: strconv.Itoa(in.Port)},
			pgconninfo.Pair{Key: "user", Value: "kingresd"}, pgconninfo.Pair{Key: "password", Value: Password}, pgconninfo.Pair{Key: "application_name", Value: "kingres_node_2"})
		if perr != nil {
			fixtureErr = perr
			return
		}
		conf := "\nport = " + strconv.Itoa(standby) + "\nunix_socket_directories = '" + in.Root + "'\nprimary_conninfo = '" + strings.ReplaceAll(primary, "'", "''") + "'\n"
		f, err := os.OpenFile(filepath.Join(sdir, "postgresql.auto.conf"), os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			fixtureErr = err
			return
		}
		_, _ = f.WriteString(conf)
		_ = f.Close()
		if fixtureErr = run("pg_ctl", "-D", sdir, "-l", filepath.Join(in.Root, "standby.log"), "-w", "start"); fixtureErr != nil {
			return
		}
		prev := fixtureStop
		fixtureStop = func() {
			_ = run("pg_ctl", "-D", sdir, "-m", "immediate", "stop")
			prev()
		}
		for i := 0; i < 50; i++ {
			var n int
			if c, err := in.ConnectRaw(); err == nil {
				_ = c.QueryRow(context.Background(), "select count(*) from pg_stat_replication where state = 'streaming'").Scan(&n)
				_ = c.Close(context.Background())
			}
			if n == 1 {
				break
			}
			time.Sleep(200 * time.Millisecond)
		}
		if fixtureErr = psql("postgres", "-c", "select pg_create_physical_replication_slot('kingres_slot_2')"); fixtureErr != nil {
			return
		}
		fixtureDSN, fixtureErr = pgconninfo.Format(pgconninfo.Pair{Key: "host", Value: "127.0.0.1"}, pgconninfo.Pair{Key: "port", Value: strconv.Itoa(in.Port)},
			pgconninfo.Pair{Key: "dbname", Value: "kingres_cp"}, pgconninfo.Pair{Key: "user", Value: "cp_service"},
			pgconninfo.Pair{Key: "password", Value: "cp_service_pw"}, pgconninfo.Pair{Key: "sslmode", Value: "disable"})
	})
	return fixtureDSN, fixtureErr
}
