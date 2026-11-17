// SPDX-License-Identifier: AGPL-3.0-only

package clustertest_test

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/clustertest"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/contracttest"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/fault"
)

// Phase 13 on a cluster of three servers (design config-phase13.md): every test leaves the cluster as
// it found it (no override, no server group, no pending restart), because the cluster is shared.

func cfgChange(name, level, scope string, value any) map[string]any {
	c := map[string]any{"name": name, "level": level, "value": value}
	if scope != "" {
		c["scopeId"] = scope
	}
	return c
}

// cfgApply sends a change, waits for its task and returns the finished task.
func cfgApply(t *testing.T, n *clustertest.Node, changes ...map[string]any) map[string]any {
	t.Helper()
	// One configuration task runs at a time. A change of a server's group makes the leader bring that server up to
	// date in a task of its own, which may still be running when the test goes on: the product answers 409
	// operation_in_progress for that time, and asking again is what a client does.
	var body map[string]any
	for end := time.Now().Add(2 * time.Minute); ; time.Sleep(500 * time.Millisecond) {
		st, b := n.Do("POST", "/config/changes", map[string]any{"changes": changes}, nil)
		if st == 202 {
			body = b
			break
		}
		if st != 409 || b["code"] != "operation_in_progress" || time.Now().After(end) {
			t.Fatalf("node %d: POST /config/changes: status %d, want 202: %v", n.Index, st, b)
		}
	}
	return n.WaitTask(clustertest.TaskID(body), 5*time.Minute)
}

func cfgApplyDone(t *testing.T, n *clustertest.Node, changes ...map[string]any) {
	t.Helper()
	if b := cfgApply(t, n, changes...); b["state"] != "done" {
		t.Fatalf("the change ended %v: %v", b["state"], b["error"])
	}
}

func cfgParam(t *testing.T, n *clustertest.Node, name string) map[string]any {
	t.Helper()
	return n.Expect(200, "GET", "/config/parameters/"+url.PathEscape(name), nil)
}

// cfgServer returns the entry of one server of a parameter.
func cfgServer(t *testing.T, p map[string]any, server string) map[string]any {
	t.Helper()
	for _, s := range anyList(p["servers"]) {
		if m := s.(map[string]any); m["serverId"] == server {
			return m
		}
	}
	t.Fatalf("no entry for server %s in %v", server, p["servers"])
	return nil
}

func show(t *testing.T, n *clustertest.Node, db, name string) string {
	t.Helper()
	var v string
	if err := n.ServicePG(db).QueryRow(context.Background(), "SELECT current_setting($1)", name).Scan(&v); err != nil {
		t.Fatalf("node %d: SHOW %s: %v", n.Index, name, err)
	}
	return v
}

func cfgRevision(t *testing.T, n *clustertest.Node) float64 {
	t.Helper()
	return n.Expect(200, "GET", "/config/servers", nil)["revision"].(float64)
}

func TestConfigClusterLevelAuditAndHistory(t *testing.T) {
	c := sharedCluster(t)
	leader, f1 := c.Nodes[0], c.Nodes[1]
	chk := contracttest.NewChecker(t)

	before := show(t, leader, "postgres", "work_mem")
	rev := cfgRevision(t, leader)

	// The preview changes nothing and says what would happen.
	pv := leader.Expect(200, "POST", "/config/changes/preview", map[string]any{"changes": []any{cfgChange("work_mem", "cluster", "", "8MB")}})
	if pv["valid"] != true || len(anyList(pv["effects"])) != 3 || len(anyList(pv["restartServerIds"])) != 0 {
		t.Errorf("preview: %v", pv)
	}
	for _, e := range anyList(pv["effects"]) {
		if e.(map[string]any)["takesEffect"] != "reload" {
			t.Errorf("effect: %v", e)
		}
	}
	if cfgRevision(t, leader) != rev || show(t, leader, "postgres", "work_mem") != before {
		t.Fatal("the preview changed something")
	}

	// Through a follower (forwarded to the leader).
	body := f1.Expect(202, "POST", "/config/changes", map[string]any{"changes": []any{cfgChange("work_mem", "cluster", "", "8MB")}})
	if err := checkResp(chk, "applyConfigChange", "202", body); err != nil {
		t.Errorf("response does not match the contract: %v", err)
	}
	if done := f1.WaitTask(clustertest.TaskID(body), 5*time.Minute); done["state"] != "done" {
		t.Fatalf("task: %v", done)
	}
	for _, n := range c.Nodes {
		if got := show(t, n, "postgres", "work_mem"); got != "8MB" {
			t.Errorf("node %d: work_mem %s", n.Index, got)
		}
	}
	p := cfgParam(t, f1, "work_mem")
	if err := checkResp(chk, "getConfigParameter", "200", p); err != nil {
		t.Errorf("response does not match the contract: %v", err)
	}
	if p["kind"] != "settable" || p["differs"] != false || len(anyList(p["overrides"])) != 1 {
		t.Errorf("parameter: %v", p)
	}
	for _, n := range c.Nodes {
		s := cfgServer(t, p, n.Host)
		if s["value"] != "8MB" || s["level"] != "cluster" || s["state"] != "applied" || s["source"] != "cluster" {
			t.Errorf("server %s: %v", n.Host, s)
		}
	}
	if cfgRevision(t, leader) != rev+1 {
		t.Errorf("revision %v after %v", cfgRevision(t, leader), rev)
	}

	// The history has the old (none) and the new value and the level; the audit log the same.
	h := leader.Expect(200, "GET", "/config/changes?name=work_mem", nil)
	if err := checkResp(chk, "listConfigChanges", "200", h); err != nil {
		t.Errorf("response does not match the contract: %v", err)
	}
	first := items(t, h)[0]
	if first["newValue"] != "8MB" || first["oldValue"] != nil || first["level"] != "cluster" || first["changedBy"] != clustertest.AdminUser || first["revertible"] != true {
		t.Errorf("history: %v", first)
	}
	au := leader.Expect(200, "GET", "/audit?action=applyConfigChange", nil)
	evs := items(t, au)
	if len(evs) == 0 {
		t.Fatalf("no audit event: %v", au)
	}
	det, _ := evs[0]["details"].(map[string]any)
	chs := anyList(det["changes"])
	if len(chs) != 1 {
		t.Fatalf("audit details: %v", det)
	}
	ch := chs[0].(map[string]any)
	if ch["name"] != "work_mem" || ch["newValue"] != "8MB" || ch["oldValue"] != nil || ch["level"] != "cluster" {
		t.Errorf("audit change: %v", ch)
	}
	if evs[0]["outcome"] != "success" {
		t.Errorf("audit outcome: %v", evs[0])
	}

	// The same change again is a retry: it records no new history row and brings every server up to date.
	cfgApplyDone(t, leader, cfgChange("work_mem", "cluster", "", "8MB"))
	if n := len(items(t, leader.Expect(200, "GET", "/config/changes?name=work_mem", nil))); n != 1 {
		t.Errorf("%d history rows after a repeated change", n)
	}
	// Filters of the list.
	l := leader.Expect(200, "GET", "/config/parameters?changed=true", nil)
	if err := checkResp(chk, "listConfigParameters", "200", l); err != nil {
		t.Errorf("response does not match the contract: %v", err)
	}
	if got := items(t, l); len(got) != 1 || got[0]["name"] != "work_mem" {
		t.Errorf("changed=true: %v", got)
	}
	if got := items(t, leader.Expect(200, "GET", "/config/parameters?q=statement_timeout", nil)); len(got) != 1 {
		t.Errorf("q: %d", len(got))
	}
	if got := items(t, leader.Expect(200, "GET", "/config/parameters?kind=refused&limit=500", nil)); len(got) < 5 {
		t.Errorf("refused parameters: %d", len(got))
	}
	pg1 := leader.Expect(200, "GET", "/config/parameters?limit=5", nil)
	if len(items(t, pg1)) != 5 || pg1["nextCursor"] == nil {
		t.Errorf("first page: %v", pg1["nextCursor"])
	}
	pg2 := leader.Expect(200, "GET", "/config/parameters?limit=5&cursor="+url.QueryEscape(pg1["nextCursor"].(string)), nil)
	if items(t, pg2)[0]["name"] == items(t, pg1)[0]["name"] {
		t.Error("the second page starts like the first")
	}

	// Removing the override returns the default.
	cfgApplyDone(t, leader, cfgChange("work_mem", "cluster", "", nil))
	for _, n := range c.Nodes {
		if got := show(t, n, "postgres", "work_mem"); got != before {
			t.Errorf("node %d: work_mem %s after the removal, want %s", n.Index, got, before)
		}
	}
	if len(anyList(cfgParam(t, leader, "work_mem")["overrides"])) != 0 {
		t.Error("an override is left")
	}
}

func TestConfigServerGroupAndServerLevels(t *testing.T) {
	c := sharedCluster(t)
	leader, n1, n2 := c.Nodes[0], c.Nodes[1], c.Nodes[2]
	leader.Expect(201, "POST", "/server-groups", map[string]any{"name": "cfg-rack"})
	t.Cleanup(func() {
		for _, n := range []*clustertest.Node{n1, n2} {
			leader.Do("PATCH", "/servers/"+n.Host, map[string]any{"serverGroup": nil}, nil)
		}
		leader.Do("DELETE", "/server-groups/cfg-rack", nil, nil)
	})
	leader.Expect(200, "PATCH", "/servers/"+n1.Host, map[string]any{"serverGroup": "cfg-rack"})
	leader.Expect(200, "PATCH", "/servers/"+n2.Host, map[string]any{"serverGroup": "cfg-rack"})

	cfgApplyDone(t, leader, cfgChange("random_page_cost", "cluster", "", "1.5"), cfgChange("random_page_cost", "serverGroup", "cfg-rack", "2.5"))
	want := func(label string, w ...string) {
		t.Helper()
		for i, n := range c.Nodes {
			if got := show(t, n, "postgres", "random_page_cost"); got != w[i] {
				t.Errorf("%s: node %d random_page_cost %s, want %s", label, i, got, w[i])
			}
		}
	}
	want("cluster and group", "1.5", "2.5", "2.5")
	p := cfgParam(t, leader, "random_page_cost")
	if cfgServer(t, p, n1.Host)["level"] != "serverGroup" || cfgServer(t, p, leader.Host)["level"] != "cluster" || p["differs"] != true || p["differsInGroup"] != false {
		t.Errorf("levels: %v", p["servers"])
	}

	// One server beats its group and the cluster; the group differs inside by design.
	cfgApplyDone(t, leader, cfgChange("random_page_cost", "server", n1.Host, "3.5"))
	want("server", "1.5", "3.5", "2.5")
	p = cfgParam(t, leader, "random_page_cost")
	if cfgServer(t, p, n1.Host)["level"] != "server" || cfgServer(t, p, n1.Host)["source"] != "server" || p["differsInGroup"] != false {
		t.Errorf("server level: %v", cfgServer(t, p, n1.Host))
	}

	// Removing the server override returns the group's value; removing the group's, the cluster's.
	cfgApplyDone(t, leader, cfgChange("random_page_cost", "server", n1.Host, nil))
	want("server removed", "1.5", "2.5", "2.5")
	cfgApplyDone(t, leader, cfgChange("random_page_cost", "serverGroup", "cfg-rack", nil))
	want("group removed", "1.5", "1.5", "1.5")

	// A server that moves into a group gets the group's value (the leader brings it up to date).
	cfgApplyDone(t, leader, cfgChange("random_page_cost", "serverGroup", "cfg-rack", "2.5"))
	leader.Expect(200, "PATCH", "/servers/"+n2.Host, map[string]any{"serverGroup": nil})
	deadline := time.Now().Add(3 * time.Minute)
	for show(t, n2, "postgres", "random_page_cost") != "1.5" {
		if time.Now().After(deadline) {
			t.Fatalf("node 2 kept the group's value after it left the group: %s", show(t, n2, "postgres", "random_page_cost"))
		}
		time.Sleep(2 * time.Second)
	}
	// A level that does not exist is refused, a cluster-only parameter cannot be set per server.
	if st, o := leader.Do("POST", "/config/changes", map[string]any{"changes": []any{cfgChange("random_page_cost", "serverGroup", "no-such-rack", "1.5")}}, nil); st != 404 || o["code"] != "not_found" {
		t.Errorf("unknown group: %d %v", st, o)
	}
	cfgApplyDone(t, leader, cfgChange("random_page_cost", "serverGroup", "cfg-rack", nil), cfgChange("random_page_cost", "cluster", "", nil))
	want("clean", "4", "4", "4")
}

func TestConfigRefusedInvalidAndHidden(t *testing.T) {
	c := sharedCluster(t)
	leader := c.Nodes[0]
	rev := cfgRevision(t, leader)
	mem := show(t, leader, "postgres", "work_mem")
	fileOf := func(n *clustertest.Node) string {
		b, err := os.ReadFile(filepath.Join(n.DataDir, "instances", "1", "pgdata", "kingres-config.conf"))
		if err != nil {
			t.Fatalf("node %d: %v", n.Index, err)
		}
		return string(b)
	}
	files := map[int]string{}
	for _, n := range c.Nodes {
		files[n.Index] = fileOf(n)
	}
	bad := func(want int, code string, changes ...map[string]any) {
		t.Helper()
		st, o := leader.Do("POST", "/config/changes", map[string]any{"changes": changes}, nil)
		if st != want || o["code"] != code {
			t.Errorf("%v: status %d code %v, want %d %s: %v", changes[0], st, o["code"], want, code, o)
		}
	}
	bad(400, "validation_failed", cfgChange("work_mem", "cluster", "", "abc"))
	bad(400, "validation_failed", cfgChange("work_mem", "cluster", "", "0x10"))
	bad(400, "validation_failed", cfgChange("work_mem", "cluster", "", "63kB"))
	bad(400, "validation_failed", cfgChange("TimeZone", "cluster", "", "Nowhere/City"))
	bad(400, "validation_failed", cfgChange("default_toast_compression", "cluster", "", "bogus"))
	bad(422, "config_parameter_managed", cfgChange("synchronous_commit", "cluster", "", "on"))
	bad(422, "config_parameter_managed", cfgChange("port", "cluster", "", "5999"))
	bad(422, "config_parameter_managed", cfgChange("shared_preload_libraries", "cluster", "", "x"))
	bad(422, "config_parameter_managed", cfgChange("synchronous_standby_names", "server", leader.Host, "x"))
	bad(422, "config_parameter_managed", cfgChange("ssl_cert_file", "database", "postgres", "x"))
	bad(422, "config_parameter_refused", cfgChange("archive_command", "cluster", "", "cp %p /tmp/x"))
	bad(422, "config_parameter_refused", cfgChange("restore_command", "cluster", "", "cp /x %p"))
	bad(422, "config_parameter_refused", cfgChange("ssl_passphrase_command", "cluster", "", "echo secret"))
	bad(422, "config_parameter_refused", cfgChange("dynamic_library_path", "cluster", "", "/tmp"))
	bad(422, "config_parameter_refused", cfgChange("local_preload_libraries", "cluster", "", "x"))
	bad(422, "config_parameter_refused", cfgChange("fsync", "cluster", "", "off"))
	bad(422, "config_level_not_allowed", cfgChange("TimeZone", "server", leader.Host, "UTC"))
	bad(422, "config_level_not_allowed", cfgChange("kingres_shard.default_bucket_count", "serverGroup", "x", "64"))
	bad(422, "config_level_not_allowed", cfgChange("shared_buffers", "database", "postgres", "256MB"))
	bad(422, "config_level_not_allowed", cfgChange("log_timezone", "database", "postgres", "UTC"))
	bad(422, "config_value_below_floor", cfgChange("max_prepared_transactions", "cluster", "", "0"))
	bad(404, "config_parameter_unknown", cfgChange("no_such_parameter", "cluster", "", "1"))
	bad(404, "not_found", cfgChange("work_mem", "server", "10.9.9.9", "1MB"))
	bad(400, "validation_failed")
	if st, o := leader.Do("POST", "/config/changes", map[string]any{"changes": []any{cfgChange("work_mem", "cluster", "", "1MB"), cfgChange("work_mem", "cluster", "", "2MB")}}, nil); st != 400 {
		t.Errorf("the same value twice: %d %v", st, o)
	}
	// A request with one good and one bad change changes nothing.
	bad(422, "config_parameter_managed", cfgChange("work_mem", "cluster", "", "1MB"), cfgChange("port", "cluster", "", "5999"))
	if cfgRevision(t, leader) != rev || show(t, leader, "postgres", "work_mem") != mem {
		t.Error("a refused request changed something")
	}
	for _, n := range c.Nodes {
		if got := fileOf(n); got != files[n.Index] {
			t.Errorf("node %d: the override file changed: %q", n.Index, got)
		}
	}
	// The preview says why, with the pointer, and still answers 200.
	pv := leader.Expect(200, "POST", "/config/changes/preview", map[string]any{"changes": []any{cfgChange("archive_command", "cluster", "", "x"), cfgChange("work_mem", "cluster", "", "abc")}})
	ps := anyList(pv["problems"])
	if pv["valid"] != false || len(ps) != 2 || ps[0].(map[string]any)["code"] != "config_parameter_refused" || ps[1].(map[string]any)["pointer"] != "/changes/1/value" {
		t.Errorf("preview: %v", pv)
	}
	// A hidden value is never returned, a refused parameter is listed read-only with its reason.
	for _, name := range []string{"archive_command", "ssl_passphrase_command", "primary_conninfo", "kingres_shard.node_conninfo"} {
		p := cfgParam(t, leader, name)
		if p["hidden"] != true || p["kind"] == "settable" || p["reason"] == nil || len(anyList(p["allowedLevels"])) != 0 {
			t.Errorf("%s: %v", name, p)
		}
		for _, s := range anyList(p["servers"]) {
			if m := s.(map[string]any); m["value"] != nil || m["pendingValue"] != nil {
				t.Errorf("%s: a hidden value was returned: %v", name, m)
			}
		}
		if p["defaultValue"] != nil {
			t.Errorf("%s: the default of a hidden parameter was returned", name)
		}
	}
	if p := cfgParam(t, leader, "TimeZone"); p["clusterOnly"] == nil || len(anyList(p["allowedLevels"])) != 2 {
		t.Errorf("TimeZone: %v", p)
	}
	if st, o := leader.Do("GET", "/config/parameters/no_such_parameter", nil, nil); st != 404 || o["code"] != "config_parameter_unknown" {
		t.Errorf("unknown parameter: %d %v", st, o)
	}
}

func TestConfigRoles(t *testing.T) {
	c := sharedCluster(t)
	leader := c.Nodes[0]
	chk := contracttest.NewChecker(t)
	_ = chk
	roles := map[string]string{"cfgview": "kingres_readonly", "cfgops": "kingres_ops", "cfgstats": "kingres_stats"}
	tokens := map[string]string{}
	for name, role := range roles {
		mkRole(t, leader, name, markerPassword, role)
		st, o := leader.Do("POST", "/auth/login", map[string]any{"username": name, "password": markerPassword, "issueToken": true}, map[string]string{"Authorization": ""})
		if st != 200 {
			t.Fatalf("login %s: %d %v", name, st, o)
		}
		tokens[name] = o["token"].(string)
	}
	t.Cleanup(func() {
		for name := range roles {
			leader.Do("DELETE", "/roles/"+name, nil, nil)
		}
	})
	as := func(name, method, path string, body any) int {
		st, _ := leader.Do(method, path, body, map[string]string{"Authorization": "Bearer " + tokens[name]})
		return st
	}
	change := map[string]any{"changes": []any{cfgChange("work_mem", "cluster", "", "8MB")}}
	for _, name := range []string{"cfgview", "cfgops"} {
		for _, p := range []string{"/config/parameters", "/config/parameters/work_mem", "/config/changes", "/config/servers", "/config/summary", "/config/time", "/config/time-zones"} {
			if st := as(name, "GET", p, nil); st != 200 {
				t.Errorf("%s GET %s: %d", name, p, st)
			}
		}
		for _, p := range []string{"/config/changes/preview", "/config/changes", "/config/restarts", "/config/changes/1/revert"} {
			if st := as(name, "POST", p, change); st != 403 {
				t.Errorf("%s POST %s: %d, want 403", name, p, st)
			}
		}
	}
	for _, p := range []string{"/config/parameters", "/config/servers", "/config/summary", "/config/time", "/config/time-zones", "/config/changes"} {
		if st := as("cfgstats", "GET", p, nil); st != 403 {
			t.Errorf("a stats-only role reads %s: %d", p, st)
		}
	}
	if st, o := leader.Do("GET", "/config/parameters", nil, map[string]string{"Authorization": ""}); st != 401 {
		t.Errorf("without a session: %d %v", st, o)
	}
	// The admin may preview and apply; nothing of the above changed anything.
	leader.Expect(200, "POST", "/config/changes/preview", change)
}

func TestConfigHistoryAndRevert(t *testing.T) {
	c := sharedCluster(t)
	leader, f2 := c.Nodes[0], c.Nodes[2]
	cfgApplyDone(t, leader, cfgChange("effective_io_concurrency", "cluster", "", "8"))
	cfgApplyDone(t, leader, cfgChange("effective_io_concurrency", "cluster", "", "16"))
	h := items(t, leader.Expect(200, "GET", "/config/changes?name=effective_io_concurrency&level=cluster", nil))
	if len(h) != 2 || h[0]["newValue"] != "16" || h[0]["oldValue"] != "8" || h[0]["revertible"] != true || h[1]["revertible"] != false {
		t.Fatalf("history: %v", h)
	}
	older, newest := int64(h[1]["changeId"].(float64)), int64(h[0]["changeId"].(float64))
	// An older change cannot be reverted while a later one touched the same value.
	if st, o := leader.Do("POST", fmt.Sprintf("/config/changes/%d/revert", older), nil, nil); st != 409 || o["code"] != "config_change_superseded" {
		t.Errorf("revert of a superseded change: %d %v", st, o)
	}
	if st, o := leader.Do("POST", "/config/changes/999999/revert", nil, nil); st != 404 {
		t.Errorf("revert of an unknown change: %d %v", st, o)
	}
	// The latest is reverted as a new change, through a follower.
	body := f2.Expect(202, "POST", fmt.Sprintf("/config/changes/%d/revert", newest), nil)
	if done := f2.WaitTask(clustertest.TaskID(body), 5*time.Minute); done["state"] != "done" {
		t.Fatalf("revert: %v", done)
	}
	for _, n := range c.Nodes {
		if got := show(t, n, "postgres", "effective_io_concurrency"); got != "8" {
			t.Errorf("node %d: %s after the revert", n.Index, got)
		}
	}
	h = items(t, leader.Expect(200, "GET", "/config/changes?name=effective_io_concurrency", nil))
	if len(h) != 3 || h[0]["reverts"] == nil || int64(h[0]["reverts"].(float64)) != newest || h[0]["newValue"] != "8" {
		t.Errorf("history after the revert: %v", h[0])
	}
	if st, _ := leader.Do("POST", fmt.Sprintf("/config/changes/%d/revert", newest), nil, nil); st != 409 {
		t.Errorf("a change that was reverted is superseded by the revert: %d", st)
	}
	// Revert the first change of all: the override goes away.
	last := items(t, leader.Expect(200, "GET", "/config/changes?name=effective_io_concurrency&limit=1", nil))[0]
	if last["revertible"] != true {
		t.Fatalf("the revert is revertible: %v", last)
	}
	// Reverting the revert goes back to 16MB; remove the override at the end.
	body = leader.Expect(202, "POST", fmt.Sprintf("/config/changes/%d/revert", int64(last["changeId"].(float64))), nil)
	leader.WaitTaskDone(clustertest.TaskID(body), 5*time.Minute)
	if got := show(t, leader, "postgres", "effective_io_concurrency"); got != "16" {
		t.Errorf("after reverting the revert: %s", got)
	}
	cfgApplyDone(t, leader, cfgChange("effective_io_concurrency", "cluster", "", nil))
	page := leader.Expect(200, "GET", "/config/changes?limit=2", nil)
	if len(items(t, page)) != 2 || page["nextCursor"] == nil {
		t.Errorf("paging: %v", page)
	}
}

func TestConfigClusterAndDatabaseTimeZone(t *testing.T) {
	c := sharedCluster(t)
	leader := c.Nodes[0]
	mkDB(t, leader, "cfgtz")
	t.Cleanup(func() {
		body := leader.Expect(202, "DELETE", "/databases/cfgtz", nil)
		leader.WaitTask(clustertest.TaskID(body), 3*time.Minute)
	})
	base := show(t, leader, "postgres", "TimeZone")

	// A database time zone is applied on every server.
	cfgApplyDone(t, leader, cfgChange("TimeZone", "database", "cfgtz", "Asia/Tehran"))
	for _, n := range c.Nodes {
		if got := show(t, n, "cfgtz", "TimeZone"); got != "Asia/Tehran" {
			t.Errorf("node %d: database cfgtz TimeZone %s", n.Index, got)
		}
		if got := show(t, n, "postgres", "TimeZone"); got != base {
			t.Errorf("node %d: another database changed its TimeZone to %s", n.Index, got)
		}
	}
	tm := leader.Expect(200, "GET", "/config/time", nil)
	found := false
	for _, d := range anyList(tm["databases"]) {
		if m := d.(map[string]any); m["name"] == "cfgtz" && m["timeZone"] == "Asia/Tehran" {
			found = true
		}
	}
	if !found {
		t.Errorf("the database time zone is not listed: %v", tm["databases"])
	}
	cfgApplyDone(t, leader, cfgChange("TimeZone", "database", "cfgtz", nil))
	for _, n := range c.Nodes {
		if got := show(t, n, "cfgtz", "TimeZone"); got != base {
			t.Errorf("node %d: database cfgtz TimeZone %s after the removal, want %s", n.Index, got, base)
		}
	}

	// The cluster time zone and the time zone of the log, without a restart.
	cfgApplyDone(t, leader, cfgChange("TimeZone", "cluster", "", "Asia/Tehran"), cfgChange("log_timezone", "cluster", "", "Asia/Tehran"))
	for _, n := range c.Nodes {
		if got := show(t, n, "postgres", "TimeZone"); got != "Asia/Tehran" {
			t.Errorf("node %d: TimeZone %s", n.Index, got)
		}
	}
	chk := contracttest.NewChecker(t)
	deadline := time.Now().Add(30 * time.Second)
	for {
		tm = leader.Expect(200, "GET", "/config/time", nil)
		ok := true
		for _, s := range anyList(tm["servers"]) {
			if m := s.(map[string]any); m["timeZone"] != "Asia/Tehran" || m["logTimeZone"] != "Asia/Tehran" {
				ok = false
			}
		}
		if ok || time.Now().After(deadline) {
			if !ok {
				t.Errorf("zones: %v", tm["servers"])
			}
			break
		}
		time.Sleep(6 * time.Second) // the answer is cached for a few seconds
	}
	if err := checkResp(chk, "getClusterTime", "200", tm); err != nil {
		t.Errorf("response does not match the contract: %v", err)
	}
	zones := leader.Expect(200, "GET", "/config/time-zones", nil)
	if err := checkResp(chk, "listTimeZones", "200", zones); err != nil {
		t.Errorf("response does not match the contract: %v", err)
	}
	foundZone := false
	for _, z := range items(t, zones) {
		if z["name"] == "Asia/Tehran" && z["utcOffset"] == "+03:30" {
			foundZone = true
		}
	}
	if !foundZone {
		t.Errorf("Asia/Tehran is not in the zone list (%d zones)", len(items(t, zones)))
	}
	cfgApplyDone(t, leader, cfgChange("TimeZone", "cluster", "", nil), cfgChange("log_timezone", "cluster", "", nil))
	for _, n := range c.Nodes {
		if got := show(t, n, "postgres", "TimeZone"); got != base {
			t.Errorf("node %d: TimeZone %s after the removal, want %s", n.Index, got, base)
		}
	}
}

func TestConfigClockDifference(t *testing.T) {
	c := sharedCluster(t)
	leader, f1 := c.Nodes[0], c.Nodes[1]
	t.Cleanup(func() { f1.Heal() })
	f1.Heal()
	time.Sleep(6 * time.Second)
	read := func() map[string]map[string]any {
		tm := leader.Expect(200, "GET", "/config/time", nil)
		out := map[string]map[string]any{}
		for _, s := range anyList(tm["servers"]) {
			m := s.(map[string]any)
			out[m["serverId"].(string)] = m
		}
		return out
	}
	base := read()
	if len(base) != 3 {
		t.Fatalf("servers: %v", base)
	}
	for id, s := range base {
		d, u := s["differenceMs"].(float64), s["uncertaintyMs"].(float64)
		if math.Abs(d) > u+200 || u <= 0 && id != leader.Host || s["reachable"] != true {
			t.Errorf("server %s: difference %v +/- %v", id, d, u)
		}
	}
	if base[leader.Host]["differenceMs"].(float64) != 0 {
		t.Errorf("the reference server differs from itself: %v", base[leader.Host])
	}

	// Step the clock of one server by five seconds (the fault hook): the difference is reported with its
	// uncertainty and the alert is raised.
	f1.SetFaults(fault.Spec{ClockOffsetMs: 5000})
	deadline := time.Now().Add(40 * time.Second)
	var got map[string]any
	for {
		time.Sleep(6 * time.Second)
		got = read()[f1.Host]
		d, u := got["differenceMs"].(float64), got["uncertaintyMs"].(float64)
		if math.Abs(d-5000) <= u+100 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the difference of the stepped clock is %v +/- %v, want 5000", d, u)
		}
	}
	sum := leader.Expect(200, "GET", "/config/summary", nil)
	lc, _ := sum["largestClockDifference"].(map[string]any)
	if lc["serverId"] != f1.Host || math.Abs(lc["differenceMs"].(float64)-5000) > 300 {
		t.Errorf("summary: %v", sum)
	}
	chk := contracttest.NewChecker(t)
	if err := checkResp(chk, "getConfigSummary", "200", sum); err != nil {
		t.Errorf("response does not match the contract: %v", err)
	}
	until(t, 2*time.Minute, "the alert clockDifference", func() bool {
		for _, a := range items(t, leader.Expect(200, "GET", "/alerts?state=open&limit=200", nil)) {
			if a["kind"] == "clockDifference" && a["serverId"] == f1.Host {
				return true
			}
		}
		return false
	})
	f1.Heal()
	until(t, 2*time.Minute, "the alert to clear", func() bool {
		for _, a := range items(t, leader.Expect(200, "GET", "/alerts?state=open&limit=200", nil)) {
			if a["kind"] == "clockDifference" {
				return false
			}
		}
		return true
	})
}

func TestConfigPendingRestartAndRestartTask(t *testing.T) {
	c := sharedCluster(t)
	leader, n2 := c.Nodes[0], c.Nodes[2]
	chk := contracttest.NewChecker(t)
	before := show(t, n2, "postgres", "shared_buffers")

	pv := leader.Expect(200, "POST", "/config/changes/preview", map[string]any{"changes": []any{cfgChange("shared_buffers", "server", n2.Host, "320MB")}})
	if pv["valid"] != true || len(anyList(pv["restartServerIds"])) != 1 || anyList(pv["restartServerIds"])[0] != n2.Host {
		t.Errorf("preview: %v", pv)
	}
	if err := checkResp(chk, "previewConfigChange", "200", pv); err != nil {
		t.Errorf("response does not match the contract: %v", err)
	}
	cfgApplyDone(t, leader, cfgChange("shared_buffers", "server", n2.Host, "320MB"))

	// Pending on that server only; the running value is the old one; nothing was restarted.
	if got := show(t, n2, "postgres", "shared_buffers"); got != before {
		t.Errorf("shared_buffers changed without a restart: %s", got)
	}
	p := cfgParam(t, leader, "shared_buffers")
	s2 := cfgServer(t, p, n2.Host)
	if s2["state"] != "pendingRestart" || s2["value"] != before || s2["pendingValue"] != "320MB" {
		t.Errorf("pending: %v", s2)
	}
	if cfgServer(t, p, leader.Host)["state"] != "applied" {
		t.Errorf("another server: %v", cfgServer(t, p, leader.Host))
	}
	if got := items(t, leader.Expect(200, "GET", "/config/parameters?pendingRestart=true", nil)); len(got) != 1 || got[0]["name"] != "shared_buffers" {
		t.Errorf("pendingRestart filter: %v", got)
	}
	// The state of a server is read from its instances as they are now: while its standby is being started again after the
	// restart of an earlier run the server is "waiting" ("Instance 101 is not running"), which is true and passes.
	var sv map[string]any
	until(t, time.Minute, "the server to show its pending restart", func() bool {
		sv = leader.Expect(200, "GET", "/config/servers", nil)
		for _, s := range items(t, sv) {
			if s["serverId"] == n2.Host {
				return s["state"] == "pendingRestart" && len(anyList(s["pendingRestart"])) == 1
			}
		}
		return false
	})
	if err := checkResp(chk, "listConfigServers", "200", sv); err != nil {
		t.Errorf("response does not match the contract: %v", err)
	}
	sum := leader.Expect(200, "GET", "/config/summary", nil)
	if l := anyList(sum["serversPendingRestart"]); len(l) != 1 || l[0] != n2.Host {
		t.Errorf("summary: %v", sum)
	}

	// The restart needs the acknowledgement of the downtime.
	if st, o := leader.Do("POST", "/config/restarts", map[string]any{"acknowledgeDowntime": false}, nil); st != 400 {
		t.Errorf("without the acknowledgement: %d %v", st, o)
	}
	body := leader.Expect(202, "POST", "/config/restarts", map[string]any{"acknowledgeDowntime": true, "serverIds": []string{n2.Host}})
	if done := leader.WaitTask(clustertest.TaskID(body), 8*time.Minute); done["state"] != "done" {
		t.Fatalf("restart task: %v %v", done["state"], done["error"])
	}
	deadline := time.Now().Add(2 * time.Minute)
	for {
		if show(t, n2, "postgres", "shared_buffers") == "320MB" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("shared_buffers is %s after the restart", show(t, n2, "postgres", "shared_buffers"))
		}
		time.Sleep(time.Second)
	}
	if s := cfgServer(t, cfgParam(t, leader, "shared_buffers"), n2.Host); s["state"] != "applied" || s["value"] != "320MB" {
		t.Errorf("after the restart: %v", s)
	}
	if st, o := leader.Do("POST", "/config/restarts", map[string]any{"acknowledgeDowntime": true}, nil); st != 409 || o["code"] != "nothing_pending" {
		t.Errorf("nothing pending: %d %v", st, o)
	}
	// Put it back (the removal waits for a restart too).
	cfgApplyDone(t, leader, cfgChange("shared_buffers", "server", n2.Host, nil))
	body = leader.Expect(202, "POST", "/config/restarts", map[string]any{"acknowledgeDowntime": true})
	if done := leader.WaitTask(clustertest.TaskID(body), 8*time.Minute); done["state"] != "done" {
		t.Fatalf("second restart task: %v %v", done["state"], done["error"])
	}
	until(t, 2*time.Minute, "the old shared_buffers", func() bool { return show(t, n2, "postgres", "shared_buffers") == before })
}

func TestConfigRestartOrderOfStandbys(t *testing.T) {
	c := sharedCluster(t)
	leader := c.Nodes[0]
	type pair struct {
		group           int
		standby, prim   string
		standbyInstance int
	}
	var pairs []pair
	until(t, 4*time.Minute, "a ready standby for every group", func() bool {
		pairs = nil
		rows, err := leader.ServicePG("kingres_system").Query(context.Background(), `SELECT group_id, server_id, primary_server, instance_id FROM kingres_cp.standby WHERE state = 'ready'`)
		if err != nil {
			return false
		}
		defer rows.Close()
		for rows.Next() {
			var p pair
			if err := rows.Scan(&p.group, &p.standby, &p.prim, &p.standbyInstance); err != nil {
				return false
			}
			pairs = append(pairs, p)
		}
		return len(pairs) >= 3
	})
	index := func(order []any, server string, inst int) int {
		for i, o := range order {
			m := o.(map[string]any)
			if m["serverId"] == server && int(m["instanceId"].(float64)) == inst {
				return i
			}
		}
		return -1
	}
	// Going up: a standby before its primary. Going down: after.
	for _, tc := range []struct {
		value string
		up    bool
	}{{"150", true}, {"60", false}} {
		pv := leader.Expect(200, "POST", "/config/changes/preview", map[string]any{"changes": []any{cfgChange("max_connections", "cluster", "", tc.value)}})
		if pv["valid"] != true || pv["restartOrderConflict"] != nil {
			t.Fatalf("preview %s: %v", tc.value, pv)
		}
		order := anyList(pv["restartOrder"])
		if len(order) < 6 {
			t.Fatalf("restart order of %d instances: %v", len(order), order)
		}
		for _, p := range pairs {
			si, pi := index(order, p.standby, p.standbyInstance), index(order, p.prim, 1)
			if si < 0 || pi < 0 {
				t.Fatalf("pair %+v is not in the order %v", p, order)
			}
			if tc.up && si > pi || !tc.up && pi > si {
				t.Errorf("max_connections=%s: standby of group %d at %d and its primary at %d", tc.value, p.group, si, pi)
			}
		}
	}
	// One value up and another down on the same pair has no safe order.
	pv := leader.Expect(200, "POST", "/config/changes/preview", map[string]any{"changes": []any{cfgChange("max_connections", "cluster", "", "150"), cfgChange("max_locks_per_transaction", "cluster", "", "32")}})
	if pv["restartOrderConflict"] == nil || !strings.Contains(pv["restartOrderConflict"].(string), "max_locks_per_transaction") {
		t.Errorf("mixed directions: %v", pv["restartOrderConflict"])
	}
	// Nothing was applied by any of that.
	if p := cfgParam(t, leader, "max_connections"); len(anyList(p["overrides"])) != 0 {
		t.Errorf("a preview left an override: %v", p["overrides"])
	}
}

func TestConfigServerThatIsDownGetsTheChange(t *testing.T) {
	c := sharedCluster(t)
	leader, n2 := c.Nodes[0], c.Nodes[2]
	n2.Stop()
	restarted := false
	t.Cleanup(func() {
		if !restarted {
			n2.Start()
			n2.Login(clustertest.AdminUser, clustertest.AdminPass)
		}
	})
	body := leader.Expect(202, "POST", "/config/changes", map[string]any{"changes": []any{cfgChange("work_mem", "cluster", "", "12MB")}})
	id := clustertest.TaskID(body)
	// The task waits for the server and says which.
	until(t, 90*time.Second, "the task to wait for the server", func() bool {
		sv := leader.Expect(200, "GET", "/config/servers", nil)
		for _, s := range items(t, sv) {
			if s["serverId"] == n2.Host && s["state"] == "waiting" {
				return true
			}
		}
		return false
	})
	_, tk := leader.Do("GET", fmt.Sprintf("/tasks/%v", id), nil, nil)
	if tk["state"] != "running" {
		t.Errorf("the task is %v while a server is down: %v", tk["state"], tk)
	}
	for _, n := range []*clustertest.Node{leader, c.Nodes[1]} {
		until(t, 30*time.Second, "the other servers to have the value", func() bool { return show(t, n, "postgres", "work_mem") == "12MB" })
	}
	n2.Start()
	restarted = true
	if !n2.Login(clustertest.AdminUser, clustertest.AdminPass) {
		t.Fatal("login after the restart")
	}
	if done := leader.WaitTask(id, 5*time.Minute); done["state"] != "done" {
		t.Fatalf("the task after the server returned: %v %v", done["state"], done["error"])
	}
	if got := show(t, n2, "postgres", "work_mem"); got != "12MB" {
		t.Errorf("the server that was down has %s", got)
	}
	cfgApplyDone(t, leader, cfgChange("work_mem", "cluster", "", nil))
}

// A server that cannot write its file fails and says so; the others keep the new value; a retry after
// the cause is gone brings the server up to date.
func TestConfigFailedServerIsReported(t *testing.T) {
	c := sharedCluster(t)
	leader, n1 := c.Nodes[0], c.Nodes[1]
	dir := filepath.Join(n1.DataDir, "instances", "1", "pgdata")
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Skip("cannot change the mode of the data directory: " + err.Error())
	}
	restore := func() { _ = os.Chmod(dir, 0o700) }
	t.Cleanup(restore)
	if f, err := os.CreateTemp(dir, "probe"); err == nil { // running as a user that ignores the mode (root): the test cannot cause the failure
		_ = f.Close()
		_ = os.Remove(f.Name())
		restore()
		t.Skip("the user ignores directory modes")
	}
	done := cfgApply(t, leader, cfgChange("default_statistics_target", "cluster", "", "150"))
	if done["state"] != "failed" || !strings.Contains(fmt.Sprint(done["error"]), "config_incomplete") {
		t.Fatalf("the task: %v %v", done["state"], done["error"])
	}
	sv := items(t, leader.Expect(200, "GET", "/config/servers", nil))
	for _, s := range sv {
		switch s["serverId"] {
		case n1.Host:
			if s["state"] != "failed" || len(anyList(s["failed"])) == 0 {
				t.Errorf("the failed server: %v", s)
			}
		default:
			if s["state"] == "failed" {
				t.Errorf("another server failed: %v", s)
			}
		}
	}
	if got := show(t, leader, "postgres", "default_statistics_target"); got != "150" {
		t.Errorf("a server that could apply has %s", got)
	}
	if got := show(t, n1, "postgres", "default_statistics_target"); got == "150" {
		t.Error("the failed server has the value")
	}
	p := cfgParam(t, leader, "default_statistics_target")
	if s := cfgServer(t, p, n1.Host); s["state"] != "failed" || s["error"] == nil {
		t.Errorf("parameter on the failed server: %v", s)
	}
	restore()
	cfgApplyDone(t, leader, cfgChange("default_statistics_target", "cluster", "", "150")) // the retry
	if got := show(t, n1, "postgres", "default_statistics_target"); got != "150" {
		t.Errorf("after the retry: %s", got)
	}
	if s := items(t, leader.Expect(200, "GET", "/config/servers", nil)); s[0]["state"] == "failed" || s[1]["state"] == "failed" || s[2]["state"] == "failed" {
		t.Errorf("a server is still failed after the retry: %v", s)
	}
	cfgApplyDone(t, leader, cfgChange("default_statistics_target", "cluster", "", nil))
}

// checkResp validates a decoded answer against the response schema of the contract.
func checkResp(chk *contracttest.Checker, op, status string, body map[string]any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	if status == "202" { // the contract has the shared response AcceptedTask, whose body is a Task
		return chk.Schema("Task", b)
	}
	return chk.Response(op, status, b)
}

func TestConfigClockThresholdSetting(t *testing.T) {
	c := sharedCluster(t)
	leader := c.Nodes[0]
	s := leader.Expect(200, "GET", "/settings/cluster", nil)
	clk, _ := s["clock"].(map[string]any)
	if clk["maxDifferenceMs"] != float64(1000) {
		t.Fatalf("default threshold: %v", s["clock"])
	}
	t.Cleanup(func() {
		leader.Do("PATCH", "/settings/cluster", map[string]any{"clock": map[string]any{"maxDifferenceMs": 1000}}, nil)
	})
	// The setting starts no task, so the answer is 200 with the settings.
	out := leader.Expect(200, "PATCH", "/settings/cluster", map[string]any{"clock": map[string]any{"maxDifferenceMs": 5000}})
	if clk, _ := out["clock"].(map[string]any); clk["maxDifferenceMs"] != float64(5000) {
		t.Errorf("updated settings: %v", out["clock"])
	}
	if got := leader.Expect(200, "GET", "/config/summary", nil)["maxDifferenceMs"]; got != float64(5000) {
		t.Errorf("summary threshold: %v", got)
	}
	for _, v := range []any{9, 600001, "x"} {
		if st, o := leader.Do("PATCH", "/settings/cluster", map[string]any{"clock": map[string]any{"maxDifferenceMs": v}}, nil); st != 400 {
			t.Errorf("threshold %v: %d %v", v, st, o)
		}
	}
	if st, _ := leader.Do("PATCH", "/settings/cluster", map[string]any{"clock": map[string]any{}}, nil); st != 400 {
		t.Errorf("an empty clock object: %d", st)
	}
}
