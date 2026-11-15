// SPDX-License-Identifier: AGPL-3.0-only

package pgconfig

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/instance"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/pgtest"
)

// These tests need a real PostgreSQL instance (KINGRES_PG_BIN; the WSL run provides it).

func metaOf(t *testing.T, c *pgx.Conn, cfg string) map[string]Setting {
	t.Helper()
	set, err := readSettings(context.Background(), c, cfg)
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]Setting{}
	for _, s := range set {
		m[strings.ToLower(s.Name)] = s
	}
	return m
}

// TestValidatorMatchesPostgreSQL is the differential test of design config-phase13.md ("Validation"):
// a value that kingresd accepts must mean to PostgreSQL what kingresd computed, in the base unit of the
// parameter. A value that kingresd refuses is not required to be refused by PostgreSQL (the subset is
// narrower on purpose), except that a value PostgreSQL refuses must never be accepted.
func TestValidatorMatchesPostgreSQL(t *testing.T) {
	in := pgtest.Start(t, nil)
	c := in.Connect(t, "postgres")
	ctx := context.Background()
	meta := metaOf(t, c, "")
	corpus := map[string][]string{
		"work_mem":                     {"64kB", "1MB", "1.5MB", "4096", "2GB", "63kB", "0", "-1", "1e3", "0x10", "010", "1.5", "64 kB", "64kb", "abc", "", "1TB", "2147483647kB", "2147483648kB", "+1MB"},
		"statement_timeout":            {"0", "1", "500ms", "5s", "1min", "1h", "1d", "1.5s", "0.5s", "100us", "-1", "2147483647", "9999999999", "5 s", "5S", "5m"},
		"autovacuum_naptime":           {"1", "1s", "60s", "1min", "500ms", "0", "2147483", "2147484", "1h"},
		"log_min_duration_statement":   {"-1", "0", "100", "100ms", "1s", "-2"},
		"temp_file_limit":              {"-1", "0", "1MB", "1GB", "100", "-5"},
		"random_page_cost":             {"1.1", "0", ".5", "4", "-1", "1e2", "1.7976931348623157e308", "abc", "1,5", "100"},
		"default_statistics_target":    {"1", "100", "10000", "10001", "0", "1.5", "50.0"},
		"enable_seqscan":               {"on", "off", "ON", "true", "FALSE", "yes", "no", "1", "0", "t", "f", "tr", "of", "o", "2", ""},
		"synchronous_commit":           {"on", "off", "local", "remote_write", "remote_apply", "true", "false", "yes", "no", "1", "0", "ON", "bogus", "Remote_Apply"},
		"wal_level":                    {"minimal", "replica", "logical", "LOGICAL", "bogus", "on"},
		"bytea_output":                 {"hex", "escape", "HEX", "base64"},
		"TimeZone":                     {"UTC", "Asia/Tehran", "utc", "Europe/Paris", "Nowhere/City", "+03:30", "", "PST8PDT"},
		"kingres_shard.node_timeout":   {"1", "30", "0", "-1", "abc", "30s", "30000"},
		"kingres_shard.enable_routing": {"on", "off", "maybe"},
		"max_connections":              {"1", "100", "262143", "262144", "0", "-1", "100MB"},
	}
	for name, values := range corpus {
		s, ok := meta[strings.ToLower(name)]
		if !ok {
			t.Errorf("PostgreSQL has no parameter %s", name)
			continue
		}
		for _, v := range values {
			r, kerr := Check(s, v)
			// PostgreSQL's verdict and the base-unit value it computes, for the parameters a session may set.
			var pgSetting string
			var pgErr error
			if s.Context == "user" || s.Context == "superuser" {
				tx, err := c.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				var shown string
				pgErr = tx.QueryRow(ctx, "SELECT pg_catalog.set_config($1, $2, true)", name, v).Scan(&shown)
				if pgErr == nil {
					_ = tx.QueryRow(ctx, "SELECT setting FROM pg_catalog.pg_settings WHERE name = $1", name).Scan(&pgSetting)
				}
				_ = tx.Rollback(ctx)
			} else {
				continue
			}
			switch {
			case kerr == nil && pgErr != nil && s.VarType == "string":
				// A string has no syntax of its own: its check hook (a time zone name) is layer 3.
			case kerr == nil && pgErr != nil:
				t.Errorf("%s=%q: kingresd accepts, PostgreSQL refuses: %v", name, v, pgErr)
			case kerr == nil && pgErr == nil:
				if r.BaseOK {
					want, _ := ratOf(pgSetting)
					if want == nil || r.Base.Cmp(want) != 0 {
						t.Errorf("%s=%q: kingresd computes %v, PostgreSQL %s", name, v, r.Base.FloatString(3), pgSetting)
					}
				} else if s.VarType == "bool" || s.VarType == "enum" {
					if !strings.EqualFold(r.Text, pgSetting) {
						t.Errorf("%s=%q: kingresd %q, PostgreSQL %q", name, v, r.Text, pgSetting)
					}
				}
			}
		}
	}
}

// TestRefusedSetMatchesVersion fails when the set of parameters PostgreSQL hides from an ordinary
// database user changes: a new one is refused until someone decides, a vanished one needs the table
// updated.
func TestRefusedSetMatchesVersion(t *testing.T) {
	in := pgtest.Start(t, nil)
	c := in.Connect(t, "postgres")
	ctx := context.Background()
	set, err := readSettings(ctx, c, "")
	if err != nil {
		t.Fatal(err)
	}
	h, err := hidden(ctx, c, set)
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(h)
	have := map[string]bool{}
	for _, n := range h {
		have[strings.ToLower(n)] = true
	}
	// The hand-reviewed list of PostgreSQL 18 (guc_tables.c, GUC_SUPERUSER_ONLY) and of the extensions.
	reviewed := []string{
		"config_file", "data_directory", "dynamic_library_path", "external_pid_file", "extension_control_path", "hba_file", "ident_file",
		"jit_provider", "krb_server_keyfile", "log_directory", "log_filename", "oauth_validator_libraries", "output_plugin_libraries",
		"primary_conninfo", "session_preload_libraries", "shared_preload_libraries", "ssl_ciphers", "ssl_dh_params_file", "ssl_groups",
		"ssl_max_protocol_version", "ssl_min_protocol_version", "ssl_passphrase_command", "ssl_tls13_ciphers", "unix_socket_directories",
		"kingres_shard.node_conninfo", "kingres_shard.fault_injection",
	}
	known := map[string]bool{}
	for _, n := range reviewed {
		known[n] = true
		if !have[n] {
			t.Logf("reviewed parameter %s is not hidden on this build (extension not loaded or version differs)", n)
		}
	}
	for n := range have {
		if !known[n] {
			t.Errorf("PostgreSQL hides %s from ordinary users and no one reviewed it: it is refused as superuser-only until the table and this list are updated", n)
		}
	}
	// Every hidden parameter is refused (or managed), never settable.
	for _, s := range set {
		cl := Classify(s, have[strings.ToLower(s.Name)])
		if have[strings.ToLower(s.Name)] && cl.Kind == KindSettable {
			t.Errorf("%s is hidden but settable", s.Name)
		}
	}
	// PostgreSQL 18 shows archive_command, restore_command and the like to every database user (checked),
	// so the superuser-only rule does not find them: they are refused by name, and kingresd keeps their
	// value hidden all the same (it may hold a secret).
	for _, n := range []string{"archive_command", "restore_command", "archive_cleanup_command", "recovery_end_command", "archive_library", "local_preload_libraries"} {
		s, ok := byName(set, n)
		if !ok {
			t.Errorf("PostgreSQL has no %s", n)
			continue
		}
		if cl := Classify(s, have[n]); cl.Kind != KindRefused || !cl.Hidden {
			t.Errorf("%s must be refused with a hidden value: %+v", n, cl)
		}
	}
	for _, n := range []string{"ssl_passphrase_command", "dynamic_library_path", "shared_preload_libraries", "session_preload_libraries"} {
		if !have[n] {
			t.Errorf("%s must be hidden by PostgreSQL", n)
		}
	}
}

func byName(set []Setting, n string) (Setting, bool) {
	for _, s := range set {
		if s.Name == n {
			return s, true
		}
	}
	return Setting{}, false
}

// TestOverrideFileOnRealInstance checks the override file with PostgreSQL itself: the precedence
// (derived default, override, managed), the quoting of hostile values, the check before the reload
// (V2) and the restore of the previous file.
func TestOverrideFileOnRealInstance(t *testing.T) {
	in := pgtest.Start(t, nil)
	m := in.Manager
	c := in.Connect(t, "postgres")
	ctx := context.Background()
	cfg, _ := m.ConfigPath(in.ID)

	if _, err := m.EnsureConfig(in.ID); err != nil {
		t.Fatal(err)
	}
	text, err := instance.RenderConfig(5, map[string]string{
		"work_mem":                   "8MB",
		"timezone":                   "Asia/Tehran",
		"max_connections":            "60",
		"log_line_prefix":            "it's a \\ test # not a comment é",
		"shared_buffers":             "192MB",
		"kingres_shard.node_timeout": "7",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.WriteConfig(in.ID, text); err != nil {
		t.Fatal(err)
	}
	if bad := fileProblems(ctx, c, cfg, metaOf(t, c, cfg)); len(bad) != 0 {
		t.Fatalf("PostgreSQL complains about a valid file: %v", bad)
	}
	if err := m.Reload(ctx, in.ID); err != nil {
		t.Fatal(err)
	}
	var t0 string
	_ = t0
	meta := func() map[string]Setting { return metaOf(t, c, cfg) }
	waitFor(t, func() bool { return meta()["work_mem"].Setting == "8192" })
	got := meta()
	if got["timezone"].Setting != "Asia/Tehran" {
		t.Errorf("TimeZone %q", got["timezone"].Setting)
	}
	if got["log_line_prefix"].Setting != "it's a \\ test # not a comment é" {
		t.Errorf("a hostile value did not round-trip: %q", got["log_line_prefix"].Setting)
	}
	// A derived default is overridden by the file: max_connections is 50 in the test instance and 60 now,
	// pending a restart (postmaster context), with the pending value read from pg_file_settings.
	if mc := got["max_connections"]; !mc.PendingRestart || mc.Setting != "50" || mc.PendingValue != "60" {
		t.Errorf("max_connections: %+v", mc)
	}
	if sb := got["shared_buffers"]; !sb.PendingRestart || sb.PendingValue != "192MB" {
		t.Errorf("shared_buffers: pending=%v pendingValue=%q", sb.PendingRestart, sb.PendingValue)
	}
	if got["kingres_shard.node_timeout"].Setting != "7" {
		t.Errorf("an extension parameter: %q", got["kingres_shard.node_timeout"].Setting)
	}
	// A managed setting wins over anything: the file cannot even be rendered with one.
	if _, err := instance.RenderConfig(1, map[string]string{"port": "1"}); err == nil {
		t.Error("a managed parameter was rendered")
	}
	// One row per name in pg_file_settings for our file: no second setting came out of a value.
	rows, err := fileSettings(ctx, c, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 6 {
		t.Errorf("%d settings in the file, want 6: %v", len(rows), rows)
	}

	// The check before the reload (V2): an invalid value is found in the file and the previous file is put back.
	bad := "# kingresd configuration revision 6\nwork_mem = 'abc'\n"
	if err := m.WriteConfig(in.ID, bad); err != nil {
		t.Fatal(err)
	}
	p := fileProblems(ctx, c, cfg, meta())
	if p["work_mem"] == "" {
		t.Errorf("problems: %v", p)
	}
	if err := m.RestoreConfig(in.ID); err != nil {
		t.Fatal(err)
	}
	if p := fileProblems(ctx, c, cfg, meta()); len(p) != 0 {
		t.Errorf("after the restore: %v", p)
	}
	back, _ := m.ReadConfig(in.ID)
	if back != text || instance.ConfigRevision(back) != 5 {
		t.Errorf("the previous file was not restored: %q", back)
	}
	// A syntax error is also found before any reload.
	if err := m.WriteConfig(in.ID, "this is = = bad\n"); err != nil {
		t.Fatal(err)
	}
	if p := fileProblems(ctx, c, cfg, meta()); len(p) == 0 {
		t.Error("a syntax error was not found")
	}
	_ = m.RestoreConfig(in.ID)

	// Layer 3 of the validation.
	if err := m.CheckValue(ctx, in.ID, "timezone", "Nowhere/City"); err == nil {
		t.Error("postgres -C accepted an unknown time zone")
	}
	if err := m.CheckValue(ctx, in.ID, "timezone", "Europe/Paris"); err != nil {
		t.Errorf("postgres -C refused a time zone: %v", err)
	}
	if err := m.CheckValue(ctx, in.ID, "wal_level", "bogus"); err == nil {
		t.Error("postgres -C accepted a wal_level that does not exist")
	}
	// A value cannot be a second option.
	if err := m.CheckValue(ctx, in.ID, "work_mem", "1MB -c fsync=off"); err == nil {
		t.Error("an option in a value was accepted")
	}
}

func waitFor(t *testing.T, f func() bool) {
	t.Helper()
	for i := 0; i < 100; i++ {
		if f() {
			return
		}
		sleep100()
	}
	t.Fatal("timed out")
}

func TestHostileFileValuesRoundTrip(t *testing.T) {
	if os.Getenv("KINGRES_PG_BIN") == "" {
		t.Skip("needs KINGRES_PG_BIN")
	}
	in := pgtest.Start(t, nil)
	c := in.Connect(t, "postgres")
	cfg, _ := in.Manager.ConfigPath(in.ID)
	values := map[string]string{}
	hostile := []string{"it's", `back\slash`, `ends\`, "a # b", "x' include '/etc/passwd", "include 'x'", "é漢字", `\'`, "''", strings.Repeat("a", 900)}
	for i, v := range hostile {
		values[fmt.Sprintf("kingres_h.v%d", i)] = v
	}
	// Placeholders of a prefix that no extension owns are accepted by PostgreSQL in a file only as
	// "unrecognized" warnings at reload; the file is still parsed, so pg_file_settings shows every row.
	text, err := instance.RenderConfig(1, values)
	if err != nil {
		t.Fatal(err)
	}
	if err := in.Manager.WriteConfig(in.ID, text); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	rows, err := c.Query(ctx, "SELECT name, setting FROM pg_catalog.pg_file_settings WHERE sourcefile = $1 ORDER BY name", cfg)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for rows.Next() {
		var n, s string
		if err := rows.Scan(&n, &s); err != nil {
			t.Fatal(err)
		}
		got[n] = s
	}
	rows.Close()
	if len(got) != len(hostile) {
		t.Fatalf("%d settings in the file for %d values: %v", len(got), len(hostile), got)
	}
	for i, v := range hostile {
		if got[fmt.Sprintf("kingres_h.v%d", i)] != v {
			t.Errorf("value %d: %q, want %q", i, got[fmt.Sprintf("kingres_h.v%d", i)], v)
		}
	}
}

func sleep100() { time.Sleep(100 * time.Millisecond) }
