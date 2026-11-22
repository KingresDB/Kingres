// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026 RooyeKhat Media Co

package clustertest_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/clustertest"
)

// The tests that the first independent review of phase 13 asked for (docs/reviews/2026-11-22-config-phase13.md).
// Like the others they share one cluster and leave it as they found it.

// otherZone returns a time zone that is not the zone of the host, so that a test of a zone change fails when the
// value is not applied or not removed (P13R-07).
func otherZone(t *testing.T, hostZone string) string {
	t.Helper()
	for _, z := range []string{"Pacific/Auckland", "Asia/Tehran"} {
		if z != hostZone {
			return z
		}
	}
	t.Fatal("no zone left")
	return ""
}

func problemCode(t *testing.T, n *clustertest.Node, status int, c map[string]any) string {
	t.Helper()
	st, b := n.Do("POST", "/config/changes", map[string]any{"changes": []any{c}}, nil)
	if st != status {
		t.Fatalf("%v: status %d, want %d: %v", c, st, status, b)
	}
	code, _ := b["code"].(string)
	return code
}

// P13R-01: a valid, unwarned change of default_transaction_read_only put the whole control plane into read-only
// mode, with no way back. The parameters that change how the own sessions of kingresd behave are refused at every
// level, and the cluster goes on working.
func TestConfigControlPlaneSessionSettingsAreRefused(t *testing.T) {
	c := sharedCluster(t)
	leader := c.Nodes[0]
	for _, tc := range []struct{ name, value string }{
		{"default_transaction_read_only", "on"}, {"transaction_read_only", "on"}, {"default_transaction_isolation", "serializable"},
		{"session_replication_role", "replica"}, {"statement_timeout", "1"}, {"lock_timeout", "1"},
		{"idle_in_transaction_session_timeout", "1"}, {"idle_session_timeout", "1000"}, {"search_path", "pg_catalog"},
	} {
		for _, ch := range []map[string]any{cfgChange(tc.name, "cluster", "", tc.value), cfgChange(tc.name, "database", "postgres", tc.value)} {
			if code := problemCode(t, leader, 422, ch); code != "config_parameter_refused" {
				t.Errorf("%s at %v: code %q", tc.name, ch["level"], code)
			}
			pv := leader.Expect(200, "POST", "/config/changes/preview", map[string]any{"changes": []any{ch}})
			if pv["valid"] != false {
				t.Errorf("%s: the preview says valid: %v", tc.name, pv)
			}
		}
		if k := cfgParam(t, leader, tc.name)["kind"]; k != "refused" {
			t.Errorf("%s is %v", tc.name, k)
		}
	}
	// Nothing was stored, every server still writes, and the next change goes through.
	for _, n := range c.Nodes {
		if got := show(t, n, "postgres", "default_transaction_read_only"); got != "off" {
			t.Errorf("node %d: default_transaction_read_only is %s", n.Index, got)
		}
		if _, err := n.ServicePG("kingres_system").Exec(context.Background(), "CREATE TEMP TABLE kg_p13r01(a int)"); err != nil {
			t.Errorf("node %d: the service connection cannot write: %v", n.Index, err)
		}
	}
	if p := cfgParam(t, leader, "default_transaction_read_only"); len(anyList(p["overrides"])) != 0 {
		t.Errorf("a refused value was stored: %v", p["overrides"])
	}
	cfgApplyDone(t, leader, cfgChange("geqo_threshold", "cluster", "", "14"))
	cfgApplyDone(t, leader, cfgChange("geqo_threshold", "cluster", "", nil))
}

// P13R-02 (classification): the settings whose bad value stops the postmaster or locks kingresd out are not settable.
func TestConfigSettingsThatStopTheServerAreNotSettable(t *testing.T) {
	c := sharedCluster(t)
	leader := c.Nodes[0]
	for _, tc := range []struct{ name, value, code string }{
		{"unix_socket_permissions", "0", "config_parameter_managed"}, {"unix_socket_group", "nosuchgroup", "config_parameter_managed"},
		{"log_destination", "syslog", "config_parameter_managed"}, {"huge_pages", "on", "config_parameter_refused"},
		{"ssl_crl_dir", "/nonexistent", "config_parameter_refused"}, {"event_source", "x", "config_parameter_refused"},
		{"archive_mode", "on", "config_parameter_refused"}, {"client_encoding", "LATIN1", "config_parameter_refused"}, {"restart_after_crash", "off", "config_parameter_refused"},
		{"log_transaction_sample_rate", "1", "config_parameter_managed"}, {"log_file_mode", "0000", "config_parameter_managed"},
	} {
		if code := problemCode(t, leader, 422, cfgChange(tc.name, "cluster", "", tc.value)); code != tc.code {
			t.Errorf("%s: code %q, want %s", tc.name, code, tc.code)
		}
		if code := problemCode(t, leader, 422, cfgChange(tc.name, "server", leader.Host, tc.value)); code != tc.code {
			t.Errorf("%s at server: code %q, want %s", tc.name, code, tc.code)
		}
	}
}

// P13R-02 (the restart): a value that every check accepts but the host cannot honour (a shared memory that does not
// exist) stops the instance from starting. The restart task fails, the previous file is put back, the instance runs
// again with the old value, and the change can be reverted.
func TestConfigRestartThatCannotStartRollsBack(t *testing.T) {
	c := sharedCluster(t)
	leader, n2 := c.Nodes[0], c.Nodes[2]
	before := show(t, n2, "postgres", "shared_buffers")
	cfgApplyDone(t, leader, cfgChange("shared_buffers", "server", n2.Host, "4000GB"))
	// A second change before the restart: the file that it replaces (the previous one) holds the bad value (P13S-01).
	cfgApplyDone(t, leader, cfgChange("geqo_threshold", "server", n2.Host, "16"))
	t.Cleanup(func() { cfgApplyDone(t, leader, cfgChange("geqo_threshold", "server", n2.Host, nil)) })
	removed := false
	t.Cleanup(func() {
		if !removed {
			cfgApplyDone(t, leader, cfgChange("shared_buffers", "server", n2.Host, nil))
		}
	})
	body := leader.Expect(202, "POST", "/config/restarts", map[string]any{"acknowledgeDowntime": true, "serverIds": []string{n2.Host}})
	done := leader.WaitTask(clustertest.TaskID(body), 8*time.Minute)
	if done["state"] != "failed" || !strings.Contains(fmt.Sprint(done["error"]), "previous file was put back") {
		if done["state"] == "done" {
			t.Fatalf("the host accepted 4000GB of shared memory; the test cannot cause the failure: %v", done)
		}
		t.Fatalf("restart task: %v %v", done["state"], done["error"])
	}
	// The instance runs again, with the old value, and the file is the previous one.
	until(t, 2*time.Minute, "the instance of the server to answer with the old value", func() bool {
		conn, err := tryServicePG(n2, "postgres")
		if err != nil {
			return false
		}
		defer func() { _ = conn.Close(context.Background()) }()
		var v string
		return conn.QueryRow(context.Background(), "SELECT current_setting('shared_buffers')").Scan(&v) == nil && v == before
	})
	if strings.Contains(overrideFile(t, n2), "4000GB") {
		t.Error("the override file still holds the value that stopped the instance")
	}
	// Revert: the stored value is removed, and the server is healthy.
	cfgApplyDone(t, leader, cfgChange("shared_buffers", "server", n2.Host, nil))
	removed = true
	until(t, 2*time.Minute, "the server to be applied", func() bool {
		for _, s := range items(t, leader.Expect(200, "GET", "/config/servers", nil)) {
			if s["serverId"] == n2.Host {
				return s["state"] == "applied"
			}
		}
		return false
	})
}

// P13R-03: a server that is down in the middle of the list does not keep the servers after it from the change, and
// does not keep every later change out. The leader brings the server up to date when it answers again.
func TestConfigServerInTheMiddleThatIsDownDoesNotBlock(t *testing.T) {
	c := sharedCluster(t)
	leader, n1, n2 := c.Nodes[0], c.Nodes[1], c.Nodes[2]
	n1.Stop()
	restarted := false
	t.Cleanup(func() {
		if !restarted {
			n1.Start()
			n1.Login(clustertest.AdminUser, clustertest.AdminPass)
		}
	})
	done := cfgApply(t, leader, cfgChange("geqo_threshold", "cluster", "", "13"))
	if done["state"] != "done" {
		t.Fatalf("the change with a server down: %v %v", done["state"], done["error"])
	}
	for _, n := range []*clustertest.Node{leader, n2} {
		if got := show(t, n, "postgres", "geqo_threshold"); got != "13" {
			t.Errorf("node %d has %s", n.Index, got)
		}
	}
	var waiting bool
	for _, s := range items(t, leader.Expect(200, "GET", "/config/servers", nil)) {
		if s["serverId"] == n1.Host && s["state"] == "waiting" {
			waiting = true
		}
	}
	if !waiting {
		t.Error("the stopped server is not shown as waiting")
	}
	// A later change is accepted at once (not 409 operation_in_progress) and reaches the servers that are up.
	st, b := leader.Do("POST", "/config/changes", map[string]any{"changes": []any{cfgChange("geqo_threshold", "cluster", "", "14")}}, nil)
	if st != 202 {
		t.Fatalf("the next change: %d %v", st, b)
	}
	if d := leader.WaitTask(clustertest.TaskID(b), 5*time.Minute); d["state"] != "done" {
		t.Fatalf("the next change ended %v: %v", d["state"], d["error"])
	}
	n1.Start()
	restarted = true
	if !n1.Login(clustertest.AdminUser, clustertest.AdminPass) {
		t.Fatal("login after the restart")
	}
	until(t, 4*time.Minute, "the server that was down to get the value from the leader", func() bool {
		conn, err := tryServicePG(n1, "postgres")
		if err != nil {
			return false
		}
		defer func() { _ = conn.Close(context.Background()) }()
		var v string
		return conn.QueryRow(context.Background(), "SELECT current_setting('geqo_threshold')").Scan(&v) == nil && v == "14"
	})
	cfgApplyDone(t, leader, cfgChange("geqo_threshold", "cluster", "", nil))
}

// P13R-05: a stored value whose name became managed (an upgrade added it to the list) does not fail every apply, and
// can be removed through the API.
func TestConfigStoredValueOfAManagedNameCanBeRemoved(t *testing.T) {
	c := sharedCluster(t)
	leader := c.Nodes[0]
	sys := leader.ServicePG("kingres_system")
	if _, err := sys.Exec(context.Background(), `INSERT INTO kingres_cp.config_value (level, scope_id, name, value, revision, updated_by) VALUES ('cluster', '', 'wal_level', 'logical', 0, 'test')`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = leader.ServicePG("kingres_system").Exec(context.Background(), `DELETE FROM kingres_cp.config_value WHERE name = 'wal_level'`)
	})
	// Other changes are not blocked by it.
	cfgApplyDone(t, leader, cfgChange("geqo_threshold", "cluster", "", "7"))
	for _, n := range c.Nodes {
		if got := show(t, n, "postgres", "geqo_threshold"); got != "7" {
			t.Errorf("node %d: geqo_threshold %s", n.Index, got)
		}
		if got := show(t, n, "postgres", "wal_level"); got != "logical" && got != "replica" {
			t.Errorf("node %d: wal_level %s", n.Index, got)
		}
		if strings.Contains(overrideFile(t, n), "wal_level") {
			t.Errorf("node %d wrote the managed name to its override file", n.Index)
		}
	}
	// Setting it is refused, removing it is accepted.
	if code := problemCode(t, leader, 422, cfgChange("wal_level", "cluster", "", "logical")); code != "config_parameter_managed" {
		t.Errorf("setting it: %q", code)
	}
	pv := leader.Expect(200, "POST", "/config/changes/preview", map[string]any{"changes": []any{cfgChange("wal_level", "cluster", "", nil)}})
	if pv["valid"] != true {
		t.Fatalf("the removal is not valid: %v", pv)
	}
	cfgApplyDone(t, leader, cfgChange("wal_level", "cluster", "", nil))
	var n int
	if err := leader.ServicePG("kingres_system").QueryRow(context.Background(), `SELECT count(*) FROM kingres_cp.config_value WHERE name = 'wal_level'`).Scan(&n); err != nil || n != 0 {
		t.Errorf("the stored value is still there: %d %v", n, err)
	}
	cfgApplyDone(t, leader, cfgChange("geqo_threshold", "cluster", "", nil))
}

// P13R-11: a request that carries the revision the client saw is refused (409 config_change_superseded) when a later
// revision changed a value it changes; without it, or when the value is already the wanted one, it goes through.
func TestConfigExpectedRevision(t *testing.T) {
	c := sharedCluster(t)
	leader := c.Nodes[0]
	r0 := int64(cfgRevision(t, leader))
	cfgApplyDone(t, leader, cfgChange("geqo_threshold", "cluster", "", "9"))
	r1 := int64(cfgRevision(t, leader))
	post := func(rev int64, value any) (int, map[string]any) {
		return leader.Do("POST", "/config/changes", map[string]any{"changes": []any{cfgChange("geqo_threshold", "cluster", "", value)}, "expectedRevision": rev}, nil)
	}
	st, b := post(r0, "10")
	if st != 409 || b["code"] != "config_change_superseded" {
		t.Fatalf("a request made against an older revision: %d %v", st, b)
	}
	if got := show(t, leader, "postgres", "geqo_threshold"); got != "9" {
		t.Errorf("the refused request changed the value: %s", got)
	}
	// The repeat of a change that was recorded already goes through (a retry after a failed task).
	st, b = post(r0, "9")
	if st != 202 {
		t.Fatalf("the repeat of the recorded change: %d %v", st, b)
	}
	leader.WaitTask(clustertest.TaskID(b), 5*time.Minute)
	st, b = post(r1, "10")
	if st != 202 {
		t.Fatalf("a request made against the current revision: %d %v", st, b)
	}
	if d := leader.WaitTask(clustertest.TaskID(b), 5*time.Minute); d["state"] != "done" {
		t.Fatalf("%v %v", d["state"], d["error"])
	}
	cfgApplyDone(t, leader, cfgChange("geqo_threshold", "cluster", "", nil))
}

// P13R-12: the audit of an applied change has the normalized values and the servers; a refused attempt leaves a trace
// with the names and levels, never the value.
func TestConfigAuditDetails(t *testing.T) {
	c := sharedCluster(t)
	leader := c.Nodes[0]
	cfgApplyDone(t, leader, cfgChange("temp_buffers", "cluster", "", " 9MB "))
	evs := items(t, leader.Expect(200, "GET", "/audit?action=applyConfigChange", nil))
	var det map[string]any
	for _, e := range evs {
		d, _ := e["details"].(map[string]any)
		if e["outcome"] == "success" && len(anyList(d["changes"])) == 1 && anyList(d["changes"])[0].(map[string]any)["newValue"] == "9MB" {
			det = d
			break
		}
	}
	if det == nil {
		t.Fatalf("no audit event with the normalized value: %v", evs)
	}
	if len(anyList(det["servers"])) != len(c.Nodes) || det["taskId"] == nil || det["basedOnRevision"] == nil {
		t.Errorf("audit details: %v", det)
	}
	const secret = "touch /tmp/kg-audit-secret"
	if code := problemCode(t, leader, 422, cfgChange("archive_command", "cluster", "", secret)); code != "config_parameter_refused" {
		t.Fatalf("code %q", code)
	}
	evs = items(t, leader.Expect(200, "GET", "/audit?action=applyConfigChange&outcome=failure", nil))
	found := false
	for _, e := range evs {
		b, _ := json.Marshal(e)
		if strings.Contains(string(b), "kg-audit-secret") {
			t.Errorf("the value of a refused parameter is in the audit log: %s", b)
		}
		d, _ := e["details"].(map[string]any)
		for _, a := range anyList(d["attempted"]) {
			if m := a.(map[string]any); m["name"] == "archive_command" && m["level"] == "cluster" {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("the refused attempt left no trace: %v", evs)
	}
	cfgApplyDone(t, leader, cfgChange("temp_buffers", "cluster", "", nil))
}

// tryServicePG opens a service connection to a node, or returns the error (the node may be down or starting).
func tryServicePG(n *clustertest.Node, db string) (*pgx.Conn, error) {
	pw, err := os.ReadFile(filepath.Join(n.DataDir, "service-credentials"))
	if err != nil {
		return nil, err
	}
	return n.PG(context.Background(), "kingresd", strings.TrimSpace(string(pw)), db)
}
