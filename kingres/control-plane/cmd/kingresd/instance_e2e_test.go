// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func (e *cp) postmasterPID() string {
	_, err := os.Stat(filepath.Join(e.dataDir, "instances", "1", "pgdata", "postmaster.pid"))
	if err != nil {
		return ""
	}
	return "running"
}

func (e *cp) waitUntil(t testing.TB, what string, d time.Duration, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !f() {
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for %s\nserver log tail:\n%s", what, tail(e.logs.String(), 3000))
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// TestInstanceStopStartEndToEnd stops the instance that hosts the system
// database of a one-server cluster and brings it back with the token of before.
func TestInstanceStopStartEndToEnd(t *testing.T) {
	e := startCluster(t)
	opsTok := e.accountWithRole(t, "life_ops", "kingres_ops")
	roTok := e.accountWithRole(t, "life_ro", "kingres_readonly")
	adminTok := e.token

	inst := e.expect(t, 200, "GET", "/this-server/instances/1", nil)
	valid(t, "getThisServerInstance", "200", inst)
	if inst["state"] != "running" {
		t.Fatalf("instance %v", inst)
	}
	// Refusals before anything happened.
	if st, out, _ := e.call("POST", "/this-server/instances/1/start", nil, nil); st != 409 || problemCode(out) != "instance_already_running" {
		t.Errorf("start of a running instance: %d %v", st, out)
	}
	for _, a := range []string{"start", "stop", "restart"} {
		if st, _, _ := e.as(roTok).call("POST", "/this-server/instances/1/"+a, nil, nil); st != 403 {
			t.Errorf("%s as readonly: %d", a, st)
		}
	}
	if st, _, _ := e.call("POST", "/this-server/instances/2/stop", nil, nil); st != 404 {
		t.Errorf("stop of an unknown instance: %d", st)
	}
	if st, out, _ := e.call("POST", "/this-server/instances/1/stop", map[string]any{"force": "yes"}, nil); st != 400 {
		t.Errorf("stop with a bad body: %d %v", st, out)
	}

	// Stop it as the ops role.
	st, stopTask, _ := e.as(opsTok).call("POST", "/this-server/instances/1/stop", nil, nil)
	if st != 202 {
		t.Fatalf("stop: %d %v", st, stopTask)
	}
	validTask(t, stopTask)
	if stopTask["kind"] != "instance.stop" || stopTask["subject"].(map[string]any)["id"] != "1" {
		t.Errorf("stop task %v", stopTask)
	}
	e.waitUntil(t, "the instance to stop", 2*time.Minute, func() bool { return e.postmasterPID() == "" })

	// The API keeps answering from this process; everything that needs the
	// system database says why.
	for _, rq := range [][2]string{{"GET", "/cluster"}, {"GET", "/this-server/instances/1"}, {"GET", "/this-server/settings"}, {"GET", "/tasks"}, {"POST", "/this-server/instances/1/stop"}, {"POST", "/this-server/instances/1/restart"}} {
		st, out, h := e.call(rq[0], rq[1], nil, nil)
		if st != 503 || problemCode(out) != "system_database_stopped" {
			t.Errorf("%s %s while stopped: %d %v", rq[0], rq[1], st, out)
		}
		if h.Get("Content-Type") != "application/problem+json" {
			t.Errorf("content type %q", h.Get("Content-Type"))
		}
	}
	// The reload of a panel: the session and the instance list come from memory (contract 0.6.0).
	if st, out, _ := e.call("GET", "/auth/session", nil, nil); st != 200 || out["degraded"] != true || out["username"] != adminUser {
		t.Errorf("getSession while stopped: %d %v", st, out)
	} else {
		valid(t, "getSession", "200", out)
	}
	if st, out, _ := e.call("GET", "/this-server/instances", nil, nil); st != 200 {
		t.Errorf("listThisServerInstances while stopped: %d %v", st, out)
	} else if items, _ := out["items"].([]any); len(items) != 1 || items[0].(map[string]any)["state"] != "stopped" {
		t.Errorf("instances while stopped: %v", out)
	} else {
		valid(t, "listThisServerInstances", "200", out)
	}
	if st, out, _ := e.anon().call("POST", "/auth/login", map[string]any{"username": adminUser, "password": adminPass}, nil); st != 503 || problemCode(out) != "system_database_stopped" {
		t.Errorf("login while stopped: %d %v", st, out)
	}
	if st, out, _ := e.anon().call("GET", "/readyz", nil, nil); st != 200 {
		t.Errorf("readiness while stopped: %d %v", st, out)
	} else if cs, _ := out["checks"].([]any); len(cs) != 1 || cs[0].(map[string]any)["ok"] != false {
		t.Errorf("readiness checks while stopped: %v", out)
	}
	for _, p := range []string{"/healthz", "/setup/state"} {
		if st, out, _ := e.anon().call("GET", p, nil, nil); st != 200 {
			t.Errorf("%s while stopped: %d %v", p, st, out)
		}
	}
	// Another role does not get through with its own token before it has been seen
	// (here: the readonly role may not start).
	if st, _, _ := e.as(roTok).call("POST", "/this-server/instances/1/start", nil, nil); st != 403 {
		t.Errorf("start as readonly while stopped: %d", st)
	}
	// A request without credentials is not served from memory either.
	if st, _, _ := e.anon().call("POST", "/this-server/instances/1/start", nil, nil); st != 401 {
		t.Errorf("start without a token: %d", st)
	}
	time.Sleep(12 * time.Second) // long enough for the monitor to see the outage

	// Start it with the token of before the stop.
	st, startTask, h := e.as(adminTok).call("POST", "/this-server/instances/1/start", nil, nil)
	if st != 202 {
		t.Fatalf("start: %d %v", st, startTask)
	}
	validTask(t, startTask)
	if startTask["kind"] != "instance.start" || !strings.HasPrefix(h.Get("Location"), "/api/v1/tasks/") {
		t.Errorf("start task %v location %q", startTask, h.Get("Location"))
	}
	if st, out, _ := e.as(adminTok).call("POST", "/this-server/instances/1/start", nil, nil); st != 409 && st != 503 {
		t.Errorf("second start: %d %v", st, out)
	}
	id := fmt.Sprint(int64(startTask["id"].(float64)))
	// getTask answers 503 until the database is back, then the task, done.
	var done map[string]any
	e.waitUntil(t, "the start task to be written", 3*time.Minute, func() bool {
		st, cur, _ := e.as(adminTok).call("GET", "/tasks/"+id, nil, nil)
		if st == 200 && (cur["state"] == "done" || cur["state"] == "failed") {
			done = cur
			return true
		}
		return false
	})
	validTask(t, done)
	if done["state"] != "done" || done["kind"] != "instance.start" {
		t.Errorf("start task after the start: %v", done)
	}
	if e.postmasterPID() == "" {
		t.Error("the instance is not running")
	}
	// The task of the stop was recorded as done before the shutdown.
	stopID := fmt.Sprint(int64(stopTask["id"].(float64)))
	if got := e.as(adminTok).expect(t, 200, "GET", "/tasks/"+stopID, nil); got["state"] != "done" || got["kind"] != "instance.stop" {
		t.Errorf("stop task: %v", got)
	}
	// Everything works again, also the old token.
	e.as(adminTok).expect(t, 200, "GET", "/cluster", nil)
	e.as(opsTok).expect(t, 200, "GET", "/this-server/instances/1", nil)
	if inst := e.as(adminTok).expect(t, 200, "GET", "/this-server/instances/1", nil); inst["state"] != "running" {
		t.Errorf("instance after the start: %v", inst)
	}
	// Audited.
	for _, action := range []string{"stopThisServerInstance", "startThisServerInstance"} {
		out := e.as(adminTok).expect(t, 200, "GET", "/audit?action="+action, nil)
		items, _ := out["items"].([]any)
		if len(items) == 0 {
			t.Errorf("%s was not audited", action)
		}
	}
	// The monitor raises serverDown when the server is back.
	e.waitUntil(t, "the serverDown alert", 90*time.Second, func() bool {
		st, out, _ := e.as(adminTok).call("GET", "/alerts", nil, nil)
		if st != 200 {
			return false
		}
		items, _ := out["items"].([]any)
		for _, it := range items {
			if it.(map[string]any)["kind"] == "serverDown" {
				return true
			}
		}
		return false
	})
	// And a stop with the instance still serving other work later: restart works.
	e.as(adminTok).mustTask(t, "POST", "/this-server/instances/1/restart", nil)
}

// TestLocalSettingsEndToEnd reads and changes the settings of this server.
func TestLocalSettingsEndToEnd(t *testing.T) {
	e := startCluster(t)
	logDir := filepath.Join(e.root, "kinglogs")
	if err := os.MkdirAll(logDir, 0o750); err != nil {
		t.Fatal(err)
	}
	notDir := filepath.Join(e.root, "afile")
	if err := os.WriteFile(notDir, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	get := func() map[string]any {
		out := e.expect(t, 200, "GET", "/this-server/settings", nil)
		valid(t, "getThisServerSettings", "200", out)
		return out
	}
	patch := func(body any) map[string]any {
		out := e.expect(t, 200, "PATCH", "/this-server/settings", body)
		valid(t, "updateThisServerSettings", "200", out)
		return out
	}
	quota := func(m map[string]any) float64 { return m["memoryQuota"].(map[string]any)["totalMegabytes"].(float64) }

	s := get()
	dp := s["dataPaths"].(map[string]any)
	if dp["dataDirectory"] != e.dataDir || quota(s) != 1024 || s["logLevel"] != "info" || s["restartRequired"] != false ||
		s["clientPorts"].(map[string]any)["readWritePort"] != float64(e.pgPort) {
		t.Errorf("settings %v", s)
	}
	// Roles.
	ops := e.accountWithRole(t, "set_ops2", "kingres_ops")
	ro := e.accountWithRole(t, "set_ro2", "kingres_readonly")
	if st, _, _ := e.as(ro).call("GET", "/this-server/settings", nil, nil); st != 200 {
		t.Errorf("get as readonly: %d", st)
	}
	if st, _, _ := e.as(ops).call("PATCH", "/this-server/settings", map[string]any{"logLevel": "debug"}, nil); st != 403 {
		t.Errorf("patch as ops: %d", st)
	}
	// Refusals change nothing.
	for _, tc := range []struct {
		body any
		want int
		code string
	}{
		{map[string]any{}, 400, "validation_failed"},
		{map[string]any{"dataPaths": map[string]any{"dataDirectory": "/tmp"}}, 400, "validation_failed"},
		{map[string]any{"clientPorts": map[string]any{"readWritePort": 5}}, 400, "validation_failed"},
		{map[string]any{"memoryQuota": map[string]any{"totalMegabytes": 100}}, 400, "validation_failed"},
		{map[string]any{"logLevel": "loud"}, 400, "validation_failed"},
		{map[string]any{"memoryQuota": map[string]any{"totalMegabytes": 1 << 22}}, 422, "unprocessable"},
		{map[string]any{"logDirectory": "relative"}, 422, "unprocessable"},
		{map[string]any{"logDirectory": filepath.Join(e.root, "missing")}, 422, "unprocessable"},
		{map[string]any{"logDirectory": notDir}, 422, "unprocessable"},
		{map[string]any{"logLevel": "debug", "logDirectory": "relative"}, 422, "unprocessable"},
	} {
		st, out, _ := e.call("PATCH", "/this-server/settings", tc.body, nil)
		if st != tc.want || problemCode(out) != tc.code {
			t.Errorf("%v: %d %v", tc.body, st, out)
			continue
		}
		raw, _ := json.Marshal(out)
		if err := chk(t).Schema("Problem", raw); err != nil {
			t.Errorf("problem does not match the contract: %v: %s", err, raw)
		}
		if errs, _ := out["errors"].([]any); len(errs) == 0 || errs[0].(map[string]any)["field"] == "" {
			t.Errorf("%v: no field named: %v", tc.body, out)
		}
	}
	if s := get(); s["logLevel"] != "info" || quota(s) != 1024 || s["restartRequired"] != false {
		t.Errorf("a refused update changed something: %v", s)
	}
	// The log level applies at once and never needs a restart.
	if s := patch(map[string]any{"logLevel": "warning"}); s["logLevel"] != "warning" || s["restartRequired"] != false {
		t.Errorf("log level: %v", s)
	}
	patch(map[string]any{"logLevel": "info"})
	// Memory quota and log directory: saved, pending a restart.
	s = patch(map[string]any{"memoryQuota": map[string]any{"totalMegabytes": 2048}, "logDirectory": logDir})
	if quota(s) != 2048 || s["restartRequired"] != true || s["dataPaths"].(map[string]any)["logDirectory"] != logDir {
		t.Errorf("after the change: %v", s)
	}
	if s := get(); s["restartRequired"] != true || quota(s) != 2048 {
		t.Errorf("restartRequired must stay true: %v", s)
	}
	conf, err := os.ReadFile(filepath.Join(e.dataDir, "instances", "1", "pgdata", "kingres-managed.conf"))
	if err != nil || !strings.Contains(string(conf), "shared_buffers = '512MB'") || !strings.Contains(string(conf), "log_directory = '"+logDir+"'") {
		t.Errorf("managed configuration (%v):\n%s", err, conf)
	}
	// A value equal to the current one changes nothing and does not clear the mark.
	if s := patch(map[string]any{"memoryQuota": map[string]any{"totalMegabytes": 2048}}); s["restartRequired"] != true {
		t.Errorf("equal value: %v", s)
	}
	show := func() string {
		out := e.expect(t, 200, "POST", "/query", map[string]any{"database": "postgres", "sql": "show shared_buffers"})
		rows, _ := out["rows"].([]any)
		if len(rows) != 1 {
			t.Fatalf("show shared_buffers: %v", out)
		}
		return fmt.Sprint(rows[0].([]any)[0])
	}
	if got := show(); got != "256MB" {
		t.Errorf("before the restart shared_buffers = %s, want the old value", got)
	}
	restartedAt := time.Now().Add(-2 * time.Second)
	e.mustTask(t, "POST", "/this-server/instances/1/restart", nil)
	e.waitUntil(t, "the restart", time.Minute, func() bool { st, _, _ := e.call("GET", "/cluster", nil, nil); return st == 200 })
	if s := get(); s["restartRequired"] != false || quota(s) != 2048 {
		t.Errorf("after the restart: %v", s)
	}
	if got := show(); got != "512MB" {
		t.Errorf("after the restart shared_buffers = %s", got)
	}
	e.waitUntil(t, "a log file in the new directory", 30*time.Second, func() bool {
		ents, _ := os.ReadDir(logDir)
		return len(ents) > 0
	})
	// CP2-17: the log is read from the moved directory, which only holds what the instance
	// wrote since the restart; the old directory has older entries.
	entries, _ := e.expect(t, 200, "GET", "/this-server/logs?limit=500", nil)["items"].([]any)
	if len(entries) == 0 {
		t.Error("the log of the moved directory is empty")
	}
	for _, it := range entries {
		ts, err := time.Parse(time.RFC3339Nano, it.(map[string]any)["timestamp"].(string))
		if err != nil || ts.Before(restartedAt) {
			t.Errorf("an entry of the old log directory is listed: %v (%v)", it, err)
			break
		}
	}
	// null selects the system default again.
	s = patch(map[string]any{"logDirectory": nil, "memoryQuota": map[string]any{"totalMegabytes": 1024}})
	if v, has := s["dataPaths"].(map[string]any)["logDirectory"]; !has || v != nil || s["restartRequired"] != true {
		t.Errorf("default log directory: %v", s)
	}
	e.mustTask(t, "POST", "/this-server/instances/1/restart", nil)
	e.waitUntil(t, "the restart", time.Minute, func() bool { st, _, _ := e.call("GET", "/cluster", nil, nil); return st == 200 })
	if s := get(); s["restartRequired"] != false || quota(s) != 1024 {
		t.Errorf("after the second restart: %v", s)
	}
	// The pending mark survives a restart of kingresd (not of the instance).
	patch(map[string]any{"memoryQuota": map[string]any{"totalMegabytes": 1536}})
	e.stopManager(t)
	e.startManager(t)
	e.login(t, adminUser, adminPass)
	if s := get(); s["restartRequired"] != true || quota(s) != 1536 {
		t.Errorf("after a restart of kingresd: %v", s)
	}
	// The log level that was saved is applied at start.
	patch(map[string]any{"logLevel": "error"})
	e.stopManager(t)
	e.startManager(t)
	e.login(t, adminUser, adminPass)
	if s := get(); s["logLevel"] != "error" {
		t.Errorf("saved log level after a restart: %v", s)
	}
	// An audit entry exists.
	out := e.expect(t, 200, "GET", "/audit?action=updateThisServerSettings", nil)
	if items, _ := out["items"].([]any); len(items) == 0 {
		t.Error("updateThisServerSettings was not audited")
	}
}
