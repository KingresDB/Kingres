// SPDX-License-Identifier: PostgreSQL

package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/auth"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/instance"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/localsettings"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ops"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/store"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/task"
)

// fakeInstance stands in for the instance manager. Stopping it makes the role
// backend of the fake authenticator unavailable, as a stopped system database
// does; starting it brings the backend back.
type fakeInstance struct {
	mu      sync.Mutex
	running bool
	stopErr error
	fa      *fakeAuth
	starts  int
	stops   int
}

func (f *fakeInstance) Running(context.Context, int) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.running, nil
}

func (f *fakeInstance) Start(context.Context, int) error {
	f.mu.Lock()
	f.running = true
	f.starts++
	f.mu.Unlock()
	f.fa.mu.Lock()
	f.fa.err = nil
	f.fa.mu.Unlock()
	return nil
}

func (f *fakeInstance) Stop(context.Context, int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stops++
	if f.stopErr != nil {
		return f.stopErr
	}
	f.running = false
	f.fa.mu.Lock()
	f.fa.err = auth.ErrUnavailable
	f.fa.mu.Unlock()
	return nil
}

func (f *fakeInstance) Restart(ctx context.Context, id int) error {
	if err := f.Stop(ctx, id); err != nil {
		return err
	}
	return f.Start(ctx, id)
}

func (f *fakeInstance) WriteHBA(int, []instance.HBARule, bool) error { return nil }
func (f *fakeInstance) Reload(context.Context, int) error            { return nil }

func (f *fakeInstance) isRunning() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.running
}

// instanceEnv is a server whose Ops manage a fakeInstance.
type instanceEnv struct {
	*env
	fi  *fakeInstance
	ops *ops.Ops
	eng *task.Engine
	mem *task.MemStore
}

func newInstanceEnv(t *testing.T, mods ...func(*Deps)) *instanceEnv {
	t.Helper()
	fi := &fakeInstance{running: true}
	o := &ops.Ops{Instances: fi, Log: slog.New(slog.DiscardHandler)}
	var e *env
	e = newEnv(t, append([]func(*Deps){func(d *Deps) { d.Ops = o }}, mods...)...)
	fi.fa = e.fa
	e.initialize()
	ms := task.NewMemStore()
	eng, err := task.NewEngine(ms, o.InstanceDefinitions(), task.Options{Holder: "test", Logger: slog.New(slog.DiscardHandler), Poll: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	o.SetEngine(eng)
	t.Cleanup(o.Wait)
	return &instanceEnv{env: e, fi: fi, ops: o, eng: eng, mem: ms}
}

func eventually(t *testing.T, what string, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !f() {
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (ie *instanceEnv) state() string {
	st, err := ie.ops.InstanceState(context.Background())
	if err != nil {
		return "error: " + err.Error()
	}
	return st
}

func taskID(t *testing.T, m map[string]any) int64 {
	t.Helper()
	f, ok := m["id"].(float64)
	if !ok {
		t.Fatalf("no task id in %v", m)
	}
	return int64(f)
}

// The whole cycle of the contract: stop answers 202 and the task is recorded
// done before the shutdown; while the instance is down every authenticated
// operation but the start answers 503 system_database_stopped and health keeps
// answering; the start works from the in-memory session and its task is written
// when the instance is back.
func TestStopStartCycleWhileTheSystemDatabaseIsDown(t *testing.T) {
	ie := newInstanceEnv(t)
	tok := ie.token(auth.RoleAdmin)
	if w := ie.do(req{method: "GET", path: "/api/v1/cluster", bearer: tok}); w.Code != 200 {
		t.Fatalf("before the stop: %d %s", w.Code, w.Body.String())
	}
	// A read-only role that was checked while the instance ran (a session that
	// was never checked is refused instead, CP2-19).
	ro := ie.token(auth.RoleReadonly)
	if w := ie.do(req{method: "GET", path: "/api/v1/cluster", bearer: ro}); w.Code != 200 {
		t.Fatalf("read-only role before the stop: %d %s", w.Code, w.Body.String())
	}
	w := ie.do(req{method: "POST", path: "/api/v1/this-server/instances/1/stop", bearer: tok})
	if w.Code != 202 {
		t.Fatalf("stop: %d %s", w.Code, w.Body.String())
	}
	stopTask := decode(t, w)
	if stopTask["kind"] != "instance.stop" {
		t.Errorf("stop task: %v", stopTask)
	}
	stopID := taskID(t, stopTask)
	eventually(t, "the instance to stop", func() bool { return !ie.fi.isRunning() && ie.state() == ops.StateStopped })
	got, err := ie.eng.Get(context.Background(), stopID)
	if err != nil || got.State != task.Done {
		t.Fatalf("the stop task must be recorded as done: %v %+v", err, got)
	}

	// Everything that needs the system database answers 503 system_database_stopped.
	ie.clock.Advance(10 * time.Second) // past the cache of the role check
	for _, rq := range []req{
		{method: "GET", path: "/api/v1/cluster", bearer: tok},
		{method: "GET", path: "/api/v1/this-server/settings", bearer: tok},
		{method: "GET", path: "/api/v1/this-server/instances/1", bearer: tok},
		{method: "POST", path: "/api/v1/this-server/instances/1/stop", bearer: tok},
		{method: "GET", path: "/api/v1/tasks/1", bearer: tok},
		{method: "POST", path: "/api/v1/auth/login", body: map[string]any{"username": "alice", "password": "s3cret"}},
	} {
		m := wantProblem(t, ie.do(rq), 503, "system_database_stopped")
		if !strings.Contains(m["detail"].(string), "/this-server/instances/1/start") {
			t.Errorf("%s %s: detail %q does not say how to start it", rq.method, rq.path, m["detail"])
		}
	}
	// Health, readiness and the node state keep answering.
	for _, p := range []string{"/api/v1/healthz", "/api/v1/readyz", "/api/v1/setup/state"} {
		if w := ie.do(req{method: "GET", path: p}); w.Code != 200 {
			t.Errorf("%s while stopped: %d %s", p, w.Code, w.Body.String())
		}
	}
	// A role without ops cannot start it (the check uses the remembered roles).
	wantProblem(t, ie.do(req{method: "POST", path: "/api/v1/this-server/instances/1/start", bearer: ro}), 403, "forbidden")

	// The start works with the token of before the stop.
	w = ie.do(req{method: "POST", path: "/api/v1/this-server/instances/1/start", bearer: tok})
	if w.Code != 202 {
		t.Fatalf("start: %d %s", w.Code, w.Body.String())
	}
	startTask := decode(t, w)
	startID := taskID(t, startTask)
	if startTask["kind"] != "instance.start" || w.Header().Get("Location") == "" {
		t.Errorf("start task: %v", startTask)
	}
	if startID <= stopID || startID > 1<<32 {
		t.Errorf("the start task should use the id reserved while the database was up: stop %d, start %d", stopID, startID)
	}
	eventually(t, "the instance to run again", func() bool { return ie.fi.isRunning() && ie.state() == ops.StateRunning })
	eventually(t, "the start task to be written", func() bool {
		s, err := ie.eng.Get(context.Background(), startID)
		return err == nil && s.State == task.Done && s.Kind == "instance.start"
	})
	ie.clock.Advance(10 * time.Second)
	if w := ie.do(req{method: "GET", path: "/api/v1/cluster", bearer: tok}); w.Code != 200 {
		t.Errorf("after the start: %d %s", w.Code, w.Body.String())
	}
	// Audited.
	var stopAudited, startAudited bool
	for _, ev := range ie.audit.Events() {
		stopAudited = stopAudited || ev.Action == "stopThisServerInstance"
		startAudited = startAudited || ev.Action == "startThisServerInstance"
	}
	if !stopAudited || !startAudited {
		t.Errorf("audit: stop %v start %v", stopAudited, startAudited)
	}
}

// CP-02 stays whole: only startThisServerInstance, only while the local
// instance is not running, and only with a recent successful check, may be
// authorized from memory.
func TestStartFromMemoryDoesNotWeakenRevalidation(t *testing.T) {
	down := func(ie *instanceEnv) {
		ie.fa.mu.Lock()
		ie.fa.err = auth.ErrUnavailable
		ie.fa.mu.Unlock()
		ie.clock.Advance(10 * time.Second)
	}
	t.Run("a running instance still fails closed, also for the start", func(t *testing.T) {
		ie := newInstanceEnv(t)
		tok := ie.token(auth.RoleAdmin)
		if w := ie.do(req{method: "GET", path: "/api/v1/cluster", bearer: tok}); w.Code != 200 {
			t.Fatal(w.Code)
		}
		down(ie) // the backend fails, but the instance runs: not our stop
		wantProblem(t, ie.do(req{method: "GET", path: "/api/v1/cluster", bearer: tok}), 503, "service_unavailable")
		wantProblem(t, ie.do(req{method: "POST", path: "/api/v1/this-server/instances/1/start", bearer: tok}), 503, "service_unavailable")
		if ie.fi.starts != 0 {
			t.Error("the instance was started")
		}
	})
	t.Run("an instance that is not running (not stopped by us): only the start passes", func(t *testing.T) {
		ie := newInstanceEnv(t)
		tok := ie.token(auth.RoleAdmin)
		if w := ie.do(req{method: "GET", path: "/api/v1/cluster", bearer: tok}); w.Code != 200 {
			t.Fatal(w.Code)
		}
		ie.fi.mu.Lock()
		ie.fi.running = false
		ie.fi.mu.Unlock()
		down(ie)
		for _, rq := range []req{
			{method: "GET", path: "/api/v1/cluster", bearer: tok},
			{method: "POST", path: "/api/v1/this-server/instances/1/stop", bearer: tok},
			{method: "POST", path: "/api/v1/this-server/instances/1/restart", bearer: tok},
			{method: "GET", path: "/api/v1/this-server/settings", bearer: tok},
		} {
			wantProblem(t, ie.do(rq), 503, "service_unavailable")
		}
		if w := ie.do(req{method: "POST", path: "/api/v1/this-server/instances/1/start", bearer: tok}); w.Code != 202 {
			t.Fatalf("start of a crashed instance: %d %s", w.Code, w.Body.String())
		}
		eventually(t, "the start", ie.fi.isRunning)
	})
	t.Run("a role that lost its cluster roles is not revived from memory", func(t *testing.T) {
		ie := newInstanceEnv(t)
		tok := ie.token(auth.RoleAdmin)
		if w := ie.do(req{method: "GET", path: "/api/v1/cluster", bearer: tok}); w.Code != 200 {
			t.Fatal(w.Code)
		}
		ie.fi.mu.Lock()
		ie.fi.running = false
		ie.fi.mu.Unlock()
		ie.clock.Advance(10 * time.Second)
		ie.setUser("tester", nil) // the backend answers: no cluster role any more
		wantProblem(t, ie.do(req{method: "POST", path: "/api/v1/this-server/instances/1/start", bearer: tok}), 401, "unauthenticated")
	})
	t.Run("a session that never had a role check is refused, not trusted from sign-in (CP2-19)", func(t *testing.T) {
		ie := newInstanceEnv(t)
		tok := ie.token(auth.RoleAdmin)
		ie.fi.mu.Lock()
		ie.fi.running = false
		ie.fi.mu.Unlock()
		down(ie)
		wantProblem(t, ie.do(req{method: "POST", path: "/api/v1/this-server/instances/1/start", bearer: tok}), 503, "service_unavailable")
		if ie.fi.starts != 0 {
			t.Error("the instance was started from the roles of the sign-in")
		}
	})
	t.Run("the last check must be recent", func(t *testing.T) {
		ie := newInstanceEnv(t, func(d *Deps) { d.StaleRolesMaxAge = 30 * time.Second })
		tok := ie.token(auth.RoleAdmin)
		if w := ie.do(req{method: "GET", path: "/api/v1/cluster", bearer: tok}); w.Code != 200 {
			t.Fatal(w.Code)
		}
		if w := ie.do(req{method: "POST", path: "/api/v1/this-server/instances/1/stop", bearer: tok}); w.Code != 202 {
			t.Fatal(w.Code)
		}
		eventually(t, "the stop", func() bool { return ie.state() == ops.StateStopped })
		ie.clock.Advance(2 * time.Minute) // older than the bound, younger than the session
		wantProblem(t, ie.do(req{method: "POST", path: "/api/v1/this-server/instances/1/start", bearer: tok}), 503, "system_database_stopped")
	})
	t.Run("an expired session is not accepted either", func(t *testing.T) {
		ie := newInstanceEnv(t)
		tok := ie.token(auth.RoleAdmin)
		if w := ie.do(req{method: "POST", path: "/api/v1/this-server/instances/1/stop", bearer: tok}); w.Code != 202 {
			t.Fatal(w.Code)
		}
		eventually(t, "the stop", func() bool { return ie.state() == ops.StateStopped })
		ie.clock.Advance(2 * time.Hour)
		wantProblem(t, ie.do(req{method: "POST", path: "/api/v1/this-server/instances/1/start", bearer: tok}), 401, "session_expired")
	})
}

func TestInstanceConflictsRolesAndRefusals(t *testing.T) {
	ie := newInstanceEnv(t)
	admin := ie.token(auth.RoleAdmin)
	ops1 := ie.token(auth.RoleOps)
	ro := ie.token(auth.RoleReadonly)
	// Roles: ops may start and stop, readonly may not.
	for _, a := range []string{"start", "stop"} {
		wantProblem(t, ie.do(req{method: "POST", path: "/api/v1/this-server/instances/1/" + a, bearer: ro}), 403, "forbidden")
	}
	// A running instance cannot be started.
	wantProblem(t, ie.do(req{method: "POST", path: "/api/v1/this-server/instances/1/start", bearer: ops1}), 409, "instance_already_running")
	// Unknown and malformed instances.
	wantProblem(t, ie.do(req{method: "POST", path: "/api/v1/this-server/instances/7/stop", bearer: ops1}), 404, "not_found")
	wantProblem(t, ie.do(req{method: "POST", path: "/api/v1/this-server/instances/x/stop", bearer: ops1}), 400, "validation_failed")
	// A body that is not {force: bool}.
	wantProblem(t, ie.do(req{method: "POST", path: "/api/v1/this-server/instances/1/stop", bearer: ops1, body: `{"force":"yes"}`}), 400, "bad_request")
	wantProblem(t, ie.do(req{method: "POST", path: "/api/v1/this-server/instances/1/stop", bearer: ops1, body: `{"other":1}`}), 400, "bad_request")
	if ie.fi.stops != 0 {
		t.Fatal("a refused request stopped the instance")
	}
	// Stop, then stop again, restart and a second start.
	if w := ie.do(req{method: "POST", path: "/api/v1/this-server/instances/1/stop", bearer: ops1, body: map[string]any{"force": false}}); w.Code != 202 {
		t.Fatalf("stop: %d %s", w.Code, w.Body.String())
	}
	eventually(t, "the stop", func() bool { return ie.state() == ops.StateStopped })
	ie.clock.Advance(10 * time.Second)
	// While stopped the guard answers first; the conflict is visible through the
	// operations that pass it: the start of a stopped instance works, a second
	// start answers 409.
	if w := ie.do(req{method: "POST", path: "/api/v1/this-server/instances/1/start", bearer: ops1}); w.Code != 202 {
		t.Fatalf("start: %d %s", w.Code, w.Body.String())
	}
	eventually(t, "the start", func() bool { return ie.state() == ops.StateRunning })
	wantProblem(t, ie.do(req{method: "POST", path: "/api/v1/this-server/instances/1/start", bearer: admin}), 409, "instance_already_running")
}

// A stop whose shutdown fails corrects the task to failed and the API answers
// normally again.
func TestStopThatFailsIsCorrectedToFailed(t *testing.T) {
	ie := newInstanceEnv(t)
	ie.fi.stopErr = errors.New("pg_ctl: server does not shut down")
	tok := ie.token(auth.RoleAdmin)
	w := ie.do(req{method: "POST", path: "/api/v1/this-server/instances/1/stop", bearer: tok})
	if w.Code != 202 {
		t.Fatalf("stop: %d %s", w.Code, w.Body.String())
	}
	id := taskID(t, decode(t, w))
	eventually(t, "the task to be corrected", func() bool {
		got, err := ie.eng.Get(context.Background(), id)
		return err == nil && got.State == task.Failed
	})
	got, _ := ie.eng.Get(context.Background(), id)
	if got.Error == nil || got.Error.Code != task.CodeStepFailed || !strings.Contains(got.Error.Message, "does not shut down") {
		t.Errorf("error: %+v", got.Error)
	}
	if ie.state() != ops.StateRunning {
		t.Errorf("state %s", ie.state())
	}
	if w := ie.do(req{method: "GET", path: "/api/v1/cluster", bearer: tok}); w.Code != 200 {
		t.Errorf("the API must answer again: %d %s", w.Code, w.Body.String())
	}
}

func TestStoppedInstanceCannotBeStoppedOrRestarted(t *testing.T) {
	ie := newInstanceEnv(t)
	tok := ie.token(auth.RoleAdmin)
	ie.fi.mu.Lock()
	ie.fi.running = false // stopped on the machine; the backend still answers (a replica elsewhere)
	ie.fi.mu.Unlock()
	wantProblem(t, ie.do(req{method: "POST", path: "/api/v1/this-server/instances/1/stop", bearer: tok}), 409, "instance_already_stopped")
	wantProblem(t, ie.do(req{method: "POST", path: "/api/v1/this-server/instances/1/restart", bearer: tok}), 409, "instance_already_stopped")
}

// fakeSettingsInstances is the instance manager of the settings tests.
type fakeSettingsInstances struct {
	mu  sync.Mutex
	s   instance.ManagedSettings
	tok string
}

func (f *fakeSettingsInstances) Settings(int) (instance.ManagedSettings, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.s, nil
}

func (f *fakeSettingsInstances) WriteManaged(_ int, s instance.ManagedSettings) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.s = s
	return nil
}

func (f *fakeSettingsInstances) PostmasterToken(int) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tok
}

func TestSettingsOperations(t *testing.T) {
	fs := &fakeSettingsInstances{s: instance.ManagedSettings{Port: 5433, MemoryMegabytes: 1024}, tok: "10:1"}
	var lvl slog.LevelVar
	dir := t.TempDir()
	mgr, err := localsettings.New(localsettings.Options{DataDir: dir, Instances: fs, InstanceID: 1, PGPort: 5433, Level: &lvl,
		PhysicalMemoryMB: func() (int64, bool) { return 8192, true }, CheckDirectory: func(string) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	e := newEnv(t, func(d *Deps) { d.LocalSettings = mgr })
	e.initialize()
	admin, ops1, ro := e.token(auth.RoleAdmin), e.token(auth.RoleOps), e.token(auth.RoleReadonly)
	chk := newSchemaChecker(t)

	w := e.do(req{method: "GET", path: "/api/v1/this-server/settings", bearer: ro})
	if w.Code != 200 {
		t.Fatalf("get: %d %s", w.Code, w.Body.String())
	}
	if err := chk.validateResponse("getThisServerSettings", "200", w.Body.Bytes()); err != nil {
		t.Errorf("get does not match the contract: %v\n%s", err, w.Body.String())
	}
	m := decode(t, w)
	if m["memoryQuota"].(map[string]any)["totalMegabytes"] != float64(1024) || m["logLevel"] != "info" || m["restartRequired"] != false ||
		m["clientPorts"].(map[string]any)["readWritePort"] != float64(5433) {
		t.Errorf("settings: %v", m)
	}
	// Roles: update needs admin, get needs readonly.
	wantProblem(t, e.do(req{method: "PATCH", path: "/api/v1/this-server/settings", bearer: ops1, body: map[string]any{"logLevel": "debug"}}), 403, "forbidden")
	if w := e.do(req{method: "GET", path: "/api/v1/this-server/settings", bearer: e.token(auth.RoleStats)}); w.Code != 403 {
		t.Errorf("get as stats: %d", w.Code)
	}
	// Refusals: 400 with the field.
	for _, tc := range []struct{ body, field string }{
		{`{}`, "/"},
		{`{"dataPaths":{"dataDirectory":"/x"}}`, "/dataPaths"},
		{`{"clientPorts":{"readWritePort":1}}`, "/clientPorts"},
		{`{"somethingElse":1}`, "/somethingElse"},
		{`{"memoryQuota":{"totalMegabytes":100}}`, "/memoryQuota/totalMegabytes"},
		{`{"memoryQuota":{"totalMegabytes":"big"}}`, "/memoryQuota/totalMegabytes"},
		{`{"memoryQuota":{}}`, "/memoryQuota/totalMegabytes"},
		{`{"memoryQuota":{"totalMegabytes":1024,"x":1}}`, "/memoryQuota/x"},
		{`{"logLevel":"loud"}`, "/logLevel"},
		{`{"logLevel":3}`, "/logLevel"},
		{`{"logDirectory":5}`, "/logDirectory"},
	} {
		m := wantProblem(t, e.do(req{method: "PATCH", path: "/api/v1/this-server/settings", bearer: admin, body: tc.body}), 400, "validation_failed")
		found := false
		for _, it := range m["errors"].([]any) {
			found = found || it.(map[string]any)["field"] == tc.field
		}
		if !found {
			t.Errorf("%s: errors %v do not name %s", tc.body, m["errors"], tc.field)
		}
	}
	// Refusals: 422 with the field, nothing changed.
	for _, tc := range []struct{ body, field string }{
		{`{"memoryQuota":{"totalMegabytes":9000}}`, "/memoryQuota/totalMegabytes"},
		{`{"logDirectory":"relative/dir"}`, "/logDirectory"},
		{`{"logDirectory":"/var/../etc"}`, "/logDirectory"},
	} {
		m := wantProblem(t, e.do(req{method: "PATCH", path: "/api/v1/this-server/settings", bearer: admin, body: tc.body}), 422, "unprocessable")
		if errs := m["errors"].([]any); len(errs) != 1 || errs[0].(map[string]any)["field"] != tc.field {
			t.Errorf("%s: errors %v", tc.body, m["errors"])
		}
	}
	if got := mustGet(t, e, admin); got["restartRequired"] != false || got["memoryQuota"].(map[string]any)["totalMegabytes"] != float64(1024) {
		t.Errorf("a refused update changed something: %v", got)
	}
	// The log level applies at once and needs no restart.
	w = e.do(req{method: "PATCH", path: "/api/v1/this-server/settings", bearer: admin, body: map[string]any{"logLevel": "warning"}})
	if w.Code != 200 {
		t.Fatalf("patch: %d %s", w.Code, w.Body.String())
	}
	if err := chk.validateResponse("updateThisServerSettings", "200", w.Body.Bytes()); err != nil {
		t.Errorf("patch does not match the contract: %v", err)
	}
	if got := decode(t, w); got["logLevel"] != "warning" || got["restartRequired"] != false {
		t.Errorf("after the level change: %v", got)
	}
	if lvl.Level() != slog.LevelWarn {
		t.Errorf("the level was not applied: %v", lvl.Level())
	}
	// The quota and the log directory need a restart.
	w = e.do(req{method: "PATCH", path: "/api/v1/this-server/settings", bearer: admin, body: map[string]any{"memoryQuota": map[string]any{"totalMegabytes": 2048}, "logDirectory": "/var/log/kingres"}})
	if w.Code != 200 {
		t.Fatalf("patch: %d %s", w.Code, w.Body.String())
	}
	got := decode(t, w)
	if got["restartRequired"] != true || got["memoryQuota"].(map[string]any)["totalMegabytes"] != float64(2048) ||
		got["dataPaths"].(map[string]any)["logDirectory"] != "/var/log/kingres" {
		t.Errorf("after the quota change: %v", got)
	}
	// An equal value changes nothing: still pending for the same postmaster.
	if got := mustPatch(t, e, admin, map[string]any{"memoryQuota": map[string]any{"totalMegabytes": 2048}}); got["restartRequired"] != true {
		t.Errorf("equal value: %v", got)
	}
	// The instance is restarted: no longer pending.
	fs.mu.Lock()
	fs.tok = "11:2"
	fs.mu.Unlock()
	if got := mustGet(t, e, admin); got["restartRequired"] != false {
		t.Errorf("after the restart: %v", got)
	}
	// logDirectory null selects the default.
	got = mustPatch(t, e, admin, map[string]any{"logDirectory": nil})
	if v, has := got["dataPaths"].(map[string]any)["logDirectory"]; !has || v != nil || got["restartRequired"] != true {
		t.Errorf("logDirectory null: %v", got)
	}
	// Audited with the operation id.
	found := false
	for _, ev := range e.audit.Events() {
		found = found || ev.Action == "updateThisServerSettings"
	}
	if !found {
		t.Error("updateThisServerSettings was not audited")
	}
	if w := e.do(req{method: "PATCH", path: "/api/v1/this-server/settings", bearer: admin}); w.Code != 400 {
		t.Errorf("no body: %d", w.Code)
	}
}

func mustGet(t *testing.T, e *env, tok string) map[string]any {
	t.Helper()
	w := e.do(req{method: "GET", path: "/api/v1/this-server/settings", bearer: tok})
	if w.Code != 200 {
		t.Fatalf("get: %d %s", w.Code, w.Body.String())
	}
	return decode(t, w)
}

func mustPatch(t *testing.T, e *env, tok string, body any) map[string]any {
	t.Helper()
	w := e.do(req{method: "PATCH", path: "/api/v1/this-server/settings", bearer: tok, body: body})
	if w.Code != http.StatusOK {
		t.Fatalf("patch: %d %s", w.Code, w.Body.String())
	}
	return decode(t, w)
}

// The tasks of start, stop and restart are written by their owner, not run by
// the engine: they are not cancellable and a cancel request is refused.
func TestInstanceTasksAreNotCancellable(t *testing.T) {
	ie := newInstanceEnv(t)
	ie.fi.stopErr = errors.New("does not stop") // keeps the instance running afterwards
	tok := ie.token(auth.RoleAdmin)
	w := ie.do(req{method: "POST", path: "/api/v1/this-server/instances/1/restart", bearer: tok})
	if w.Code != 202 {
		t.Fatalf("restart: %d %s", w.Code, w.Body.String())
	}
	m := decode(t, w)
	if m["cancellable"] != false || m["kind"] != "instance.restart" {
		t.Errorf("restart task: %v", m)
	}
	// The restart fails (the fake cannot stop) and is recorded as failed.
	eventually(t, "the restart to end", func() bool {
		got, err := ie.eng.Get(context.Background(), taskID(t, m))
		return err == nil && got.State == task.Failed
	})
	eventually(t, "the gate to open again", func() bool { return !ie.ops.LocalInstanceDown() })
	wantProblem(t, ie.do(req{method: "POST", path: "/api/v1/tasks/" + itoa64(taskID(t, m)) + "/cancel", bearer: tok}), 409, "task_not_cancellable")
}

// Defect 4: a request that was already running when the instance went down
// loses its connection; it is answered with the documented
// system_database_stopped, not with a store failure.
func TestStoreFailureWhileTheInstanceIsDownIsTheDocumentedCode(t *testing.T) {
	ie := newInstanceEnv(t)
	tok := ie.token(auth.RoleAdmin)
	r := httptest.NewRequest("GET", "/api/v1/cluster", nil)
	if p := ie.srv.storeProblem(r, store.ErrUnavailable, ""); p.Code != problem.ServiceUnavailable {
		t.Fatalf("instance running: %s", p.Code)
	}
	if w := ie.do(req{method: "POST", path: "/api/v1/this-server/instances/1/stop", bearer: tok}); w.Code != 202 {
		t.Fatal(w.Code)
	}
	if p := ie.srv.storeProblem(r, store.ErrUnavailable, ""); p.Code != problem.SystemDatabaseStopped {
		t.Errorf("after the stop was accepted: %s", p.Code)
	}
}

// While the system database is stopped getSession and listThisServerInstances
// answer from memory for a session this process checked before the stop (the
// panel's reload), and refuse every other session: never checked, ended by
// Changed, refused by a check, or too old.
func TestSessionAndInstancesWhileTheSystemDatabaseIsStopped(t *testing.T) {
	stop := func(ie *instanceEnv, tok string) {
		t.Helper()
		if w := ie.do(req{method: "POST", path: "/api/v1/this-server/instances/1/stop", bearer: tok}); w.Code != 202 {
			t.Fatalf("stop: %d %s", w.Code, w.Body.String())
		}
		eventually(t, "the stop", func() bool { return ie.state() == ops.StateStopped })
		ie.clock.Advance(10 * time.Second) // past the cache of the role check
	}
	t.Run("a checked session reads itself and the instances from memory", func(t *testing.T) {
		ie := newInstanceEnv(t)
		cookie, _ := ie.cookieSession(auth.RoleAdmin)
		if w := ie.do(req{method: "GET", path: "/api/v1/this-server/instances", cookie: cookie}); w.Code != 200 {
			t.Fatalf("before the stop: %d", w.Code)
		}
		// The test catalog has no instance: put one where the last read would have.
		size := int64(5)
		ie.srv.inst.last.Store(&[]api.Instance{{ID: 1, ServerID: "h", Role: "primary", State: "running", Port: 5432, SizeBytes: &size}})
		stop(ie, ie.token(auth.RoleAdmin))
		w := ie.do(req{method: "GET", path: "/api/v1/auth/session", cookie: cookie})
		if w.Code != 200 {
			t.Fatalf("getSession while stopped: %d %s", w.Code, w.Body.String())
		}
		m := decode(t, w)
		if m["degraded"] != true || m["degradedReason"] == "" || m["csrfToken"] == nil || m["clientAddress"] != "127.0.0.1" {
			t.Errorf("degraded session: %v", m)
		}
		w = ie.do(req{method: "GET", path: "/api/v1/this-server/instances", cookie: cookie})
		if w.Code != 200 {
			t.Fatalf("instances while stopped: %d %s", w.Code, w.Body.String())
		}
		items := decode(t, w)["items"].([]any)
		if len(items) != 1 || items[0].(map[string]any)["state"] != "stopped" || items[0].(map[string]any)["sizeBytes"] != nil {
			t.Errorf("instances: %v", items)
		}
	})
	t.Run("a session this process never checked is refused", func(t *testing.T) {
		ie := newInstanceEnv(t)
		stop(ie, ie.token(auth.RoleAdmin))
		fresh := ie.token(auth.RoleOps) // another role that this process never checked
		ie.clock.Advance(10 * time.Second)
		for _, p := range []string{"/api/v1/auth/session", "/api/v1/this-server/instances"} {
			wantProblem(t, ie.do(req{method: "GET", path: p, bearer: fresh}), 503, "system_database_stopped")
		}
	})
	t.Run("Changed ends the session and forgets the check", func(t *testing.T) {
		ie := newInstanceEnv(t)
		tok := ie.token(auth.RoleAdmin)
		if w := ie.do(req{method: "GET", path: "/api/v1/this-server/instances", bearer: tok}); w.Code != 200 {
			t.Fatal(w.Code)
		}
		stop(ie, tok)
		ie.srv.RoleChanged("tester")
		for _, p := range []string{"/api/v1/auth/session", "/api/v1/this-server/instances"} {
			wantProblem(t, ie.do(req{method: "GET", path: p, bearer: tok}), 401, "unauthenticated")
		}
	})
	t.Run("a change of another role forgets the check of this one", func(t *testing.T) {
		ie := newInstanceEnv(t)
		tok := ie.token(auth.RoleAdmin)
		if w := ie.do(req{method: "GET", path: "/api/v1/this-server/instances", bearer: tok}); w.Code != 200 {
			t.Fatal(w.Code)
		}
		stop(ie, tok)
		ie.srv.RoleChanged("someone_else") // a group membership may have changed this role's roles too
		wantProblem(t, ie.do(req{method: "GET", path: "/api/v1/auth/session", bearer: tok}), 503, "system_database_stopped")
	})
	t.Run("the check must be recent", func(t *testing.T) {
		ie := newInstanceEnv(t, func(d *Deps) { d.StaleRolesMaxAge = 30 * time.Second })
		tok := ie.token(auth.RoleAdmin)
		if w := ie.do(req{method: "GET", path: "/api/v1/this-server/instances", bearer: tok}); w.Code != 200 {
			t.Fatal(w.Code)
		}
		stop(ie, tok)
		ie.clock.Advance(2 * time.Minute)
		wantProblem(t, ie.do(req{method: "GET", path: "/api/v1/auth/session", bearer: tok}), 503, "system_database_stopped")
	})
	t.Run("a role without the instance role gets nothing more than before", func(t *testing.T) {
		ie := newInstanceEnv(t)
		stats := ie.token(auth.RoleStats)
		if w := ie.do(req{method: "GET", path: "/api/v1/auth/session", bearer: stats}); w.Code != 200 {
			t.Fatal(w.Code)
		}
		stop(ie, ie.token(auth.RoleAdmin))
		wantProblem(t, ie.do(req{method: "GET", path: "/api/v1/this-server/instances", bearer: stats}), 403, "forbidden")
		if w := ie.do(req{method: "GET", path: "/api/v1/auth/session", bearer: stats}); w.Code != 200 {
			t.Errorf("the own session of a stats role: %d", w.Code)
		}
	})
}
