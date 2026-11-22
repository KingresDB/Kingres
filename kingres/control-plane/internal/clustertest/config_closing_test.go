// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026 RooyeKhat Media Co

package clustertest_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/clustertest"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/contracttest"
)

// The integration tests that the check of phase 13 against its design found missing (design
// config-phase13.md, "The check against the design"). Like the others they share one cluster and leave it as
// they found it.

func overrideFile(t *testing.T, n *clustertest.Node) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(n.DataDir, "instances", "1", "pgdata", "kingres-config.conf"))
	if err != nil {
		t.Fatalf("node %d: %v", n.Index, err)
	}
	return string(b)
}

// After a change at the cluster level every server runs the value, reads it from the override file, and
// PostgreSQL's own view of the file (pg_file_settings) has no error and says the line was applied.
func TestConfigFileSettingsAreCleanOnEveryServer(t *testing.T) {
	c := sharedCluster(t)
	leader := c.Nodes[0]
	cfgApplyDone(t, leader, cfgChange("commit_delay", "cluster", "", "50"), cfgChange("from_collapse_limit", "cluster", "", "12"))
	t.Cleanup(func() {
		cfgApplyDone(t, leader, cfgChange("commit_delay", "cluster", "", nil), cfgChange("from_collapse_limit", "cluster", "", nil))
	})
	for _, n := range c.Nodes {
		db := n.ServicePG("postgres")
		rows, err := db.Query(context.Background(), `SELECT lower(name), setting, applied, coalesce(error, '') FROM pg_file_settings WHERE sourcefile LIKE '%kingres-config.conf'`)
		if err != nil {
			t.Fatalf("node %d: pg_file_settings: %v", n.Index, err)
		}
		seen := map[string]bool{}
		for rows.Next() {
			var name, setting, errText string
			var applied bool
			if err := rows.Scan(&name, &setting, &applied, &errText); err != nil {
				t.Fatal(err)
			}
			if errText != "" {
				t.Errorf("node %d: pg_file_settings reports %s: %s", n.Index, name, errText)
			}
			seen[name] = applied
		}
		rows.Close()
		if !seen["commit_delay"] || !seen["from_collapse_limit"] {
			t.Errorf("node %d: the lines of the change are missing or not applied: %v", n.Index, seen)
		}
		var src string
		if err := db.QueryRow(context.Background(), `SELECT source FROM pg_settings WHERE name = 'commit_delay'`).Scan(&src); err != nil || src != "configuration file" {
			t.Errorf("node %d: the source of commit_delay is %q (%v)", n.Index, src, err)
		}
		if got := show(t, n, "postgres", "commit_delay"); got != "50" {
			t.Errorf("node %d: commit_delay %s", n.Index, got)
		}
		if f := overrideFile(t, n); !strings.Contains(f, "commit_delay") {
			t.Errorf("node %d: the override file lacks the line: %q", n.Index, f)
		}
	}
}

// A value that the checkers accept but PostgreSQL rejects in the file (here a time zone that is stored
// behind the back of the validator) is caught by the check of the file: the previous file is restored,
// the task fails with PostgreSQL's text, and no server runs the value.
func TestConfigValueRejectedByPostgreSQLInTheFileRestoresTheFile(t *testing.T) {
	c := sharedCluster(t)
	leader := c.Nodes[0]
	files := map[int]string{}
	zones := map[int]string{}
	for _, n := range c.Nodes {
		files[n.Index] = overrideFile(t, n)
		zones[n.Index] = show(t, n, "postgres", "TimeZone")
	}
	sys := leader.ServicePG("kingres_system")
	if _, err := sys.Exec(context.Background(), `INSERT INTO kingres_cp.config_value (level, scope_id, name, value, revision, updated_by) VALUES ('cluster', '', 'timezone', 'Nowhere/City', 0, 'test')`); err != nil {
		t.Fatalf("injecting the value: %v", err)
	}
	removed := false
	remove := func() {
		if removed {
			return
		}
		removed = true
		if _, err := leader.ServicePG("kingres_system").Exec(context.Background(), `DELETE FROM kingres_cp.config_value WHERE level = 'cluster' AND name = 'timezone'`); err != nil {
			t.Errorf("removing the injected value: %v", err)
		}
	}
	t.Cleanup(func() {
		remove()
		// Bring every server up to date with the stored values again.
		cfgApplyDone(t, leader, cfgChange("default_statistics_target", "cluster", "", "101"))
		cfgApplyDone(t, leader, cfgChange("default_statistics_target", "cluster", "", nil))
	})
	done := cfgApply(t, leader, cfgChange("default_statistics_target", "cluster", "", "101"))
	if done["state"] != "failed" {
		t.Fatalf("the task ended %v: %v", done["state"], done["error"])
	}
	sv := items(t, leader.Expect(200, "GET", "/config/servers", nil))
	// pg_file_settings gives no more than "setting could not be applied" for a value that PostgreSQL refuses; the report names the parameter.
	text := fmt.Sprint(sv)
	if !strings.Contains(text, "name:timezone") || !strings.Contains(text, "could not be applied") {
		t.Errorf("no failed server names the parameter and the text of pg_file_settings: %v", sv)
	}
	for _, n := range c.Nodes {
		if got := overrideFile(t, n); got != files[n.Index] {
			t.Errorf("node %d: the override file was not restored: %q", n.Index, got)
		}
		if got := show(t, n, "postgres", "TimeZone"); got != zones[n.Index] {
			t.Errorf("node %d: TimeZone is %s, was %s", n.Index, got, zones[n.Index])
		}
	}
	remove()
}

// A time zone of a database is a value of the catalog of each primary instance and reaches the standbys by
// replay (design, "Facts checked" V10): a connection to a standby shows it too.
func TestConfigDatabaseTimeZoneReachesTheStandbys(t *testing.T) {
	c := sharedCluster(t)
	leader := c.Nodes[0]
	zone := otherZone(t, show(t, leader, "postgres", "TimeZone")) // not the zone of the host, or the test proves nothing
	mkDB(t, leader, "cfgtzs")
	t.Cleanup(func() {
		body := leader.Expect(202, "DELETE", "/databases/cfgtzs", nil)
		leader.WaitTask(clustertest.TaskID(body), 3*time.Minute)
	})
	var standbys []standbyView
	until(t, 4*time.Minute, "a streaming standby in a group", func() bool {
		standbys = nil
		for _, v := range topology(t, leader) {
			for _, s := range v {
				if s.syncState != "" && s.state == "running" {
					standbys = append(standbys, s)
				}
			}
		}
		return len(standbys) > 0
	})
	cfgApplyDone(t, leader, cfgChange("TimeZone", "database", "cfgtzs", zone))
	t.Cleanup(func() { cfgApplyDone(t, leader, cfgChange("TimeZone", "database", "cfgtzs", nil)) })
	for _, s := range standbys {
		n := nodeOf(c, s.server)
		if n == nil {
			t.Fatalf("no node for %s", s.server)
		}
		until(t, time.Minute, "the standby of group "+strconv.Itoa(s.group)+" on "+s.server+" to show the zone", func() bool {
			conn, err := tryStandbyConn(n, s.group, s.port, "cfgtzs")
			if err != nil {
				return false
			}
			defer func() { _ = conn.Close(context.Background()) }()
			var recovery bool
			var tz string
			if err := conn.QueryRow(context.Background(), `SELECT pg_is_in_recovery(), current_setting('TimeZone')`).Scan(&recovery, &tz); err != nil {
				return false
			}
			return recovery && tz == zone
		})
	}
}

// The warnings of the preview (contract 0.16.0) on a real cluster: they never make a request invalid, the
// answer matches the contract, and the unusual-value rule has its edges at ten times and a tenth of the
// default of PostgreSQL.
func TestConfigPreviewWarnings(t *testing.T) {
	c := sharedCluster(t)
	leader := c.Nodes[0]
	chk := contracttest.NewChecker(t)
	preview := func(changes ...map[string]any) map[string]any {
		t.Helper()
		pv := leader.Expect(200, "POST", "/config/changes/preview", map[string]any{"changes": toAny(changes)})
		if err := checkResp(chk, "previewConfigChange", "200", pv); err != nil {
			t.Errorf("response does not match the contract: %v", err)
		}
		return pv
	}
	codes := func(pv map[string]any) []string {
		var out []string
		for _, w := range anyList(pv["warnings"]) {
			out = append(out, w.(map[string]any)["code"].(string))
		}
		return out
	}
	// work_mem: the default is 4MB, so 40MB is exactly ten times; a tenth is 409.6 kB.
	for _, tc := range []struct {
		value string
		warns bool
	}{{"16MB", false}, {"40MB", false}, {"41MB", true}, {"410kB", false}, {"409kB", true}, {"1GB", true}} {
		pv := preview(cfgChange("work_mem", "cluster", "", tc.value))
		if pv["valid"] != true || (len(codes(pv)) == 1 && codes(pv)[0] != "config_value_unusual") || (len(codes(pv)) > 0) != tc.warns {
			t.Errorf("work_mem %s: %v", tc.value, pv)
		}
	}
	// A parameter that is not a memory size is never unusual.
	if pv := preview(cfgChange("autovacuum_naptime", "cluster", "", "24h"), cfgChange("random_page_cost", "cluster", "", "900")); pv["valid"] != true || len(codes(pv)) != 0 {
		t.Errorf("a time and a real: %v", pv)
	}
	// Removing a value warns about nothing.
	if pv := preview(cfgChange("work_mem", "cluster", "", nil)); pv["valid"] != true || len(codes(pv)) != 0 {
		t.Errorf("a removal: %v", pv)
	}
	// The reserved connections against the running max_connections: one below the limit is allowed.
	max, err := strconv.Atoi(show(t, leader, "postgres", "max_connections"))
	if err != nil {
		t.Fatal(err)
	}
	reserved, err := strconv.Atoi(show(t, leader, "postgres", "reserved_connections"))
	if err != nil {
		t.Fatal(err)
	}
	fits := strconv.Itoa(max - 1 - reserved)
	if pv := preview(cfgChange("superuser_reserved_connections", "cluster", "", fits)); pv["valid"] != true || len(codes(pv)) != 0 {
		t.Errorf("superuser_reserved_connections %s of %d: %v", fits, max, pv)
	}
	over := strconv.Itoa(max - reserved)
	pv := preview(cfgChange("superuser_reserved_connections", "cluster", "", over))
	if pv["valid"] != true || len(codes(pv)) != 1 || codes(pv)[0] != "config_reserved_connections" {
		t.Fatalf("superuser_reserved_connections %s of %d: %v", over, max, pv)
	}
	w := anyList(pv["warnings"])[0].(map[string]any)
	if w["index"].(float64) != 0 || w["pointer"] != "/changes/0/value" || !strings.Contains(w["message"].(string), "max_connections") {
		t.Errorf("the warning: %v", w)
	}
	// A warning never stops the change: the request is accepted by the same rules as its preview.
	if st, _ := leader.Do("POST", "/config/changes/preview", map[string]any{"changes": []any{cfgChange("work_mem", "cluster", "", "abc")}}, nil); st != 200 {
		t.Errorf("an invalid preview answers %d", st)
	}
	bad := leader.Expect(200, "POST", "/config/changes/preview", map[string]any{"changes": []any{cfgChange("work_mem", "cluster", "", "abc")}})
	if bad["valid"] != false || len(anyList(bad["warnings"])) != 0 {
		t.Errorf("an invalid request carries warnings: %v", bad)
	}
}

func toAny(in []map[string]any) []any {
	out := make([]any, len(in))
	for i, m := range in {
		out[i] = m
	}
	return out
}
