// SPDX-License-Identifier: AGPL-3.0-only

package ops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/internalapi"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/peerclient"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/task"
)

// fakeExt is the PostgreSQL side of one server for the extension steps.
type fakeExt struct {
	mu        sync.Mutex
	available map[string]string // extension -> default version in the package
	installed map[string]string // extension -> installed version
	selfTest  string
	preloaded map[string]bool
	// notLoadedOn, when set, is what PreloadedOnAll names for a library that is not loaded (the instances of the server, standbys included).
	notLoadedOn []string
	settings    map[string]string
	unknown     map[string]bool // settings this server's kingres_shard does not know
	installs    int
	failOnce    string // "install" or "settings": the next such call fails once
}

func newFakeExt() *fakeExt {
	return &fakeExt{available: map[string]string{ExtSketch: "1.0", ExtColumnar: "0.2", ExtTimepart: "0.2"}, installed: map[string]string{},
		selfTest: "hash-A", preloaded: map[string]bool{ExtColumnar: true}, settings: map[string]string{}, unknown: map[string]bool{}}
}

func (f *fakeExt) Available(_ context.Context, _, ext string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.available[ext], nil
}

func (f *fakeExt) Installed(_ context.Context, _, ext string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.installed[ext], nil
}

func (f *fakeExt) Install(_ context.Context, _, ext string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failOnce == "install" {
		f.failOnce = ""
		return errors.New("connection lost")
	}
	if f.installed[ext] != f.available[ext] {
		f.installs++
	}
	f.installed[ext] = f.available[ext]
	return nil
}

func (f *fakeExt) SelfTest(context.Context, string) (string, error) { return f.selfTest, nil }

func (f *fakeExt) Preloaded(_ context.Context, lib string) (bool, error) {
	return f.preloaded[lib], nil
}

func (f *fakeExt) PreloadedOnAll(_ context.Context, lib string) ([]string, error) {
	if f.preloaded[lib] {
		return nil, nil
	}
	if f.notLoadedOn != nil {
		return f.notLoadedOn, nil
	}
	return []string{"instance 1 (primary)"}, nil
}

func (f *fakeExt) SetDatabaseSettings(_ context.Context, _ string, s map[string]string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failOnce == "settings" {
		f.failOnce = ""
		return nil, errors.New("connection lost")
	}
	var unknown []string
	for n, v := range s {
		if f.unknown[n] {
			unknown = append(unknown, n)
			continue
		}
		if v == "" {
			delete(f.settings, n)
		} else {
			f.settings[n] = v
		}
	}
	return unknown, nil
}

// fakeCluster is a set of servers: each has an Ops with its own fakeExt, and a Fanout that routes Apply to the other server's ApplyLocal (as the
// internal API does, with the error of a CallError becoming a peerclient.APIError).
type fakeCluster struct {
	ops  map[string]*Ops
	ext  map[string]*fakeExt
	down map[string]bool
	ids  []string
	mu   sync.Mutex
	// applied counts the Apply calls by kind.
	applied map[string]int
	rows    map[string]string // server -> extversion answered to Query
}

type clusterFanout struct {
	c    *fakeCluster
	self string
}

func (f clusterFanout) Others(context.Context) ([]OtherServer, error) {
	var out []OtherServer
	for _, id := range f.c.ids {
		if id != f.self {
			out = append(out, OtherServer{ID: id})
		}
	}
	return out, nil
}

func (f clusterFanout) Apply(ctx context.Context, server, kind string, params any) error {
	if f.c.down[server] {
		return fmt.Errorf("server %s: %w", server, peerclient.ErrUnreachable)
	}
	f.c.mu.Lock()
	f.c.applied[kind]++
	f.c.mu.Unlock()
	raw, _ := json.Marshal(params)
	err := f.c.ops[server].ApplyLocal(ctx, kind, raw)
	var ce *internalapi.CallError
	switch {
	case err == nil:
		return nil
	case errors.As(err, &ce):
		return &peerclient.APIError{Status: ce.Status, Code: ce.Code, Message: ce.Message}
	}
	return &peerclient.APIError{Status: 422, Code: "apply_failed", Message: err.Error()}
}

func (f clusterFanout) Register(context.Context, string, OtherServer) error { return nil }

func (f *fakeCluster) Query(_ context.Context, server, _, _ string) ([][]any, error) {
	if f.down[server] {
		return nil, fmt.Errorf("server %s: %w", server, peerclient.ErrUnreachable)
	}
	v := f.ext[server].installed[ExtSketch]
	if v == "" {
		return [][]any{}, nil
	}
	return [][]any{{v}}, nil
}

func newFakeCluster(ids ...string) *fakeCluster {
	c := &fakeCluster{ops: map[string]*Ops{}, ext: map[string]*fakeExt{}, down: map[string]bool{}, ids: ids, applied: map[string]int{}}
	for _, id := range ids {
		fe := newFakeExt()
		c.ext[id] = fe
		c.ops[id] = &Ops{ServerHost: id, extBackendOverride: fe, Log: slog.New(slog.DiscardHandler)}
		c.ops[id].Fanout = clusterFanout{c: c, self: id}
		c.ops[id].Remote = c
	}
	return c
}

// leader returns the Ops of the first server with a task engine on an in-memory store; the recorded settings are kept in rec.
func (c *fakeCluster) leader(t *testing.T, rec *[]approxParams) (*Ops, *task.Engine) {
	o, e, _ := c.leaderClock(t, rec)
	return o, e
}

func (c *fakeCluster) leaderClock(t *testing.T, rec *[]approxParams) (*Ops, *task.Engine, *testClock) {
	t.Helper()
	o := c.ops[c.ids[0]]
	o.approxRecordOverride = func(_ context.Context, p approxParams) error {
		*rec = append(*rec, p)
		return nil
	}
	clk := &testClock{t: time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)}
	e, err := task.NewEngine(task.NewMemStoreClock(clk.Now), o.Definitions(), task.Options{Holder: "A", Now: clk.Now, Lease: 30 * time.Second, StoreTries: 1})
	if err != nil {
		t.Fatal(err)
	}
	o.SetEngine(e)
	return o, e, clk
}

func submitApprox(t *testing.T, e *task.Engine, c *fakeCluster, enabled bool, mode string, rate float64) task.Task {
	t.Helper()
	p := approxParams{Database: "shop", Enabled: enabled, CountDistinct: mode, ErrorRate: rate, Servers: c.ids, Actor: "admin"}
	tk, err := e.Submit(context.Background(), KindApproximateAggregates, task.Subject{Type: "database", ID: "shop"}, p, "admin")
	if err != nil {
		t.Fatal(err)
	}
	return tk
}

func runTask(t *testing.T, e *task.Engine, id int64) task.Task {
	t.Helper()
	_ = e.RunNow(context.Background(), id, nil)
	got, err := e.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestApproxTaskHasTheFiveSteps(t *testing.T) {
	c := newFakeCluster("a", "b", "c")
	_, e := c.leader(t, new([]approxParams))
	tk := submitApprox(t, e, c, true, "approximate", 0.01)
	want := []string{stepApproxCheck, "Install the extension on server a", "Install the extension on server b", "Install the extension on server c",
		stepApproxVerify, stepApproxApply, stepApproxRecord}
	if got := stepNames(tk); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("steps %v, want %v", got, want)
	}
}

func TestApproxEnableInstallsEverywhereAndAppliesTheSettings(t *testing.T) {
	c := newFakeCluster("a", "b", "c")
	var rec []approxParams
	_, e := c.leader(t, &rec)
	got := runTask(t, e, submitApprox(t, e, c, true, "approximate", 0.02).ID)
	if got.State != task.Done {
		t.Fatalf("%s %+v", got.State, got.Error)
	}
	for id, fe := range c.ext {
		if fe.installed[ExtSketch] != "1.0" || fe.installs != 1 {
			t.Errorf("server %s: installed %q, %d installs", id, fe.installed[ExtSketch], fe.installs)
		}
		want := map[string]string{"kingres_shard.count_distinct_mode": "approximate", "kingres_shard.count_distinct_error_rate": "0.02", "kingres_shard.enable_sketch_pushdown": "on"}
		for n, v := range want {
			if fe.settings[n] != v {
				t.Errorf("server %s: %s = %q, want %q", id, n, fe.settings[n], v)
			}
		}
	}
	if len(rec) != 1 || !rec[0].Enabled || rec[0].CountDistinct != "approximate" || rec[0].ErrorRate != 0.02 {
		t.Errorf("recorded %+v", rec)
	}
}

func TestApproxDisableNeverDropsTheExtension(t *testing.T) {
	c := newFakeCluster("a", "b")
	var rec []approxParams
	_, e := c.leader(t, &rec)
	if got := runTask(t, e, submitApprox(t, e, c, true, "approximate", 0.05).ID); got.State != task.Done {
		t.Fatalf("%s %+v", got.State, got.Error)
	}
	got := runTask(t, e, submitApprox(t, e, c, false, "exact", 0).ID)
	if got.State != task.Done {
		t.Fatalf("%s %+v", got.State, got.Error)
	}
	for id, fe := range c.ext {
		if fe.installed[ExtSketch] != "1.0" {
			t.Errorf("server %s: the extension was dropped", id)
		}
		if fe.settings["kingres_shard.count_distinct_mode"] != "exact" || fe.settings["kingres_shard.enable_sketch_pushdown"] != "off" {
			t.Errorf("server %s: settings %v", id, fe.settings)
		}
		if v, ok := fe.settings["kingres_shard.count_distinct_error_rate"]; ok {
			t.Errorf("server %s: the error rate is still set (%q)", id, v)
		}
		if fe.installs != 1 {
			t.Errorf("server %s: %d installs (the second run must find the extension)", id, fe.installs)
		}
	}
	if len(rec) != 2 || rec[1].Enabled {
		t.Errorf("recorded %+v", rec)
	}
}

func TestApproxCheckRefusesADifferentVersionAndChangesNothing(t *testing.T) {
	c := newFakeCluster("a", "b", "c")
	c.ext["c"].available[ExtSketch] = "1.1"
	var rec []approxParams
	_, e := c.leader(t, &rec)
	got := runTask(t, e, submitApprox(t, e, c, true, "approximate", 0.01).ID)
	if got.State != task.Failed || got.Error == nil || got.Error.Code != string(problem.ExtensionVersionMismatch) || got.Error.Step != stepApproxCheck {
		t.Fatalf("%s %+v", got.State, got.Error)
	}
	if !strings.Contains(got.Error.Message, "server c") {
		t.Errorf("the message does not name the server: %q", got.Error.Message)
	}
	for id, fe := range c.ext {
		if fe.installs != 0 || len(fe.settings) != 0 {
			t.Errorf("server %s changed: %d installs, settings %v", id, fe.installs, fe.settings)
		}
	}
	if len(rec) != 0 {
		t.Errorf("recorded %+v", rec)
	}
}

func TestApproxCheckNamesAServerWhosePackageLacksTheExtension(t *testing.T) {
	c := newFakeCluster("a", "b")
	delete(c.ext["b"].available, ExtSketch)
	_, e := c.leader(t, new([]approxParams))
	got := runTask(t, e, submitApprox(t, e, c, true, "exact", 0).ID)
	if got.State != task.Failed || got.Error == nil || got.Error.Code != string(problem.ExtensionUnavailable) || !strings.Contains(got.Error.Message, "server b") {
		t.Fatalf("%s %+v", got.State, got.Error)
	}
}

func TestApproxVerifyNamesEveryServerThatDiffers(t *testing.T) {
	c := newFakeCluster("a", "b", "c", "d")
	// b ends at another version, c answers the known values differently; d is fine.
	c.ext["c"].selfTest = "hash-C"
	_, e := c.leader(t, new([]approxParams))
	tk := submitApprox(t, e, c, true, "approximate", 0.01)
	// b installs, then its version is changed behind the task's back (an update by hand) before the verification.
	var once sync.Once
	c.ops["a"].extBackendOverride = &hookExt{fakeExt: c.ext["a"], onSelfTest: func() { once.Do(func() { c.ext["b"].installed[ExtSketch] = "0.9" }) }}
	got := runTask(t, e, tk.ID)
	if got.State != task.Failed || got.Error == nil || got.Error.Step != stepApproxVerify {
		t.Fatalf("%s %+v", got.State, got.Error)
	}
	if got.Error.Code != string(problem.ExtensionVersionMismatch) {
		t.Errorf("code %q (the first failing server is b)", got.Error.Code)
	}
	for _, want := range []string{"server b", "server c"} {
		if !strings.Contains(got.Error.Message, want) {
			t.Errorf("the message does not name %s: %q", want, got.Error.Message)
		}
	}
	if strings.Contains(got.Error.Message, "server d") {
		t.Errorf("the message names a server that is fine: %q", got.Error.Message)
	}
	if c.ext["a"].settings["kingres_shard.count_distinct_mode"] != "" {
		t.Error("the settings were applied after a failed verification")
	}
}

// hookExt calls a function when self_test is read (the verification has started).
type hookExt struct {
	*fakeExt
	onSelfTest func()
}

func (h *hookExt) SelfTest(ctx context.Context, db string) (string, error) {
	h.onSelfTest()
	return h.fakeExt.SelfTest(ctx, db)
}

func TestApproxHashMismatchIsReportedWithItsOwnCode(t *testing.T) {
	c := newFakeCluster("a", "b")
	c.ext["b"].selfTest = "other"
	_, e := c.leader(t, new([]approxParams))
	got := runTask(t, e, submitApprox(t, e, c, true, "approximate", 0.01).ID)
	if got.State != task.Failed || got.Error == nil || got.Error.Code != string(problem.SketchHashMismatch) || !strings.Contains(got.Error.Message, "server b") {
		t.Fatalf("%s %+v", got.State, got.Error)
	}
}

func TestApproxWaitsForAServerThatIsDownAndResumes(t *testing.T) {
	oldPause := otherErrorPause
	otherErrorPause = time.Millisecond
	defer func() { otherErrorPause = oldPause }()
	c := newFakeCluster("a", "b")
	c.down["b"] = true
	_, e, clk := c.leaderClock(t, new([]approxParams))
	tk := submitApprox(t, e, c, true, "approximate", 0.01)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_ = e.RunNow(ctx, tk.ID, nil)
	if c.ext["a"].installed[ExtSketch] != "" || c.ext["b"].installed[ExtSketch] != "" {
		t.Fatal("something was installed while a server was down: the check must stop first")
	}
	c.down["b"] = false
	// The task waited and was stopped with the process; the lease runs out and the next run goes on.
	clk.Add(time.Minute)
	if ran, err := e.RunOnce(context.Background()); !ran || err != nil {
		t.Fatalf("the next run: %v %v", ran, err)
	}
	got, _ := e.Get(context.Background(), tk.ID)
	if got.State != task.Done {
		t.Fatalf("%s %+v", got.State, got.Error)
	}
	if c.ext["b"].installed[ExtSketch] != "1.0" || c.ext["b"].installs != 1 {
		t.Errorf("b: %q, %d installs", c.ext["b"].installed[ExtSketch], c.ext["b"].installs)
	}
}

// A server that refuses once for a reason that heals (a lost connection) is tried again inside the step: the task ends done, with one install.
func TestApproxARemoteServerThatRefusesOnceIsRetried(t *testing.T) {
	oldPause := otherErrorPause
	otherErrorPause = time.Millisecond
	defer func() { otherErrorPause = oldPause }()
	for _, kind := range []string{"install", "settings"} {
		c := newFakeCluster("a", "b", "c")
		var rec []approxParams
		_, e := c.leader(t, &rec)
		c.ext["c"].failOnce = kind
		got := runTask(t, e, submitApprox(t, e, c, true, "approximate", 0.01).ID)
		if got.State != task.Done {
			t.Fatalf("%s: %s %+v", kind, got.State, got.Error)
		}
		for id, fe := range c.ext {
			if fe.installs != 1 || fe.settings["kingres_shard.count_distinct_mode"] != "approximate" {
				t.Errorf("%s: server %s: %d installs, settings %v", kind, id, fe.installs, fe.settings)
			}
		}
	}
}

// A failure of this server fails the step; asking again (a new task, the subject is free) finishes, nothing is installed twice and the setting is
// recorded once.
func TestApproxARepeatAfterAFailureFinishes(t *testing.T) {
	for _, kind := range []string{"install", "settings"} {
		c := newFakeCluster("a", "b", "c")
		var rec []approxParams
		_, e := c.leader(t, &rec)
		c.ext["a"].failOnce = kind
		first := runTask(t, e, submitApprox(t, e, c, true, "approximate", 0.01).ID)
		if first.State != task.Failed {
			t.Fatalf("%s: the injected failure did not stop the task: %s", kind, first.State)
		}
		if len(rec) != 0 {
			t.Fatalf("%s: recorded before the end", kind)
		}
		got := runTask(t, e, submitApprox(t, e, c, true, "approximate", 0.01).ID)
		if got.State != task.Done {
			t.Fatalf("%s: the repeat: %s %+v", kind, got.State, got.Error)
		}
		for id, fe := range c.ext {
			if fe.installs != 1 || fe.installed[ExtSketch] != "1.0" || fe.settings["kingres_shard.count_distinct_mode"] != "approximate" {
				t.Errorf("%s: server %s: %d installs, settings %v", kind, id, fe.installs, fe.settings)
			}
		}
		if len(rec) != 1 {
			t.Errorf("%s: recorded %d times", kind, len(rec))
		}
	}
}

// The process that runs the task dies at any one of the mutating calls (here the run is cancelled at the n-th); the lease runs out and another run
// takes the task over at the step it was in. The result is the same as an uninterrupted run, with one install on each server and one record.
func TestApproxInterruptedAtEveryStepResumes(t *testing.T) {
	const calls = 6 // three installs and three settings
	for n := 1; n <= calls; n++ {
		t.Run(fmt.Sprint("call", n), func(t *testing.T) {
			c := newFakeCluster("a", "b", "c")
			var rec []approxParams
			_, e, clk := c.leaderClock(t, &rec)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			count := 0
			var mu sync.Mutex
			for _, id := range c.ids {
				c.ops[id].extBackendOverride = &callHook{fakeExt: c.ext[id], before: func() {
					mu.Lock()
					defer mu.Unlock()
					count++
					if count == n {
						cancel()
					}
				}}
			}
			tk := submitApprox(t, e, c, true, "approximate", 0.01)
			_ = e.RunNow(ctx, tk.ID, nil)
			if mid, _ := e.Get(context.Background(), tk.ID); mid.State == task.Done {
				t.Fatalf("the cancelled run finished the task")
			}
			clk.Add(time.Minute) // the lease of the dead run runs out
			if ran, err := e.RunOnce(context.Background()); !ran || err != nil {
				t.Fatalf("the takeover: %v %v", ran, err)
			}
			got, _ := e.Get(context.Background(), tk.ID)
			if got.State != task.Done {
				t.Fatalf("after the takeover: %s %+v", got.State, got.Error)
			}
			for id, fe := range c.ext {
				if fe.installs != 1 || fe.installed[ExtSketch] != "1.0" || fe.settings["kingres_shard.count_distinct_mode"] != "approximate" {
					t.Errorf("server %s: %d installs, settings %v", id, fe.installs, fe.settings)
				}
			}
			if len(rec) != 1 {
				t.Errorf("the setting was recorded %d times", len(rec))
			}
		})
	}
}

// callHook calls a function before a mutating call.
type callHook struct {
	*fakeExt
	before func()
}

func (h *callHook) Install(ctx context.Context, db, ext string) error {
	h.before()
	return h.fakeExt.Install(ctx, db, ext)
}

func (h *callHook) SetDatabaseSettings(ctx context.Context, db string, s map[string]string) ([]string, error) {
	h.before()
	return h.fakeExt.SetDatabaseSettings(ctx, db, s)
}

func TestApproxSkipsASettingTheServerDoesNotKnow(t *testing.T) {
	c := newFakeCluster("a")
	c.ext["a"].unknown["kingres_shard.count_distinct_mode"] = true
	_, e := c.leader(t, new([]approxParams))
	got := runTask(t, e, submitApprox(t, e, c, true, "approximate", 0.01).ID)
	if got.State != task.Done {
		t.Fatalf("%s %+v", got.State, got.Error)
	}
	detail := got.Detail
	if !strings.Contains(detail, "count_distinct_mode") {
		t.Errorf("the step does not say what it skipped: %q", detail)
	}
	if c.ext["a"].settings["kingres_shard.enable_sketch_pushdown"] != "on" {
		t.Error("the settings the server knows were not applied")
	}
}

func TestValidateApprox(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	cases := []struct {
		name string
		req  api.ApproximateAggregatesRequest
		mode string
		rate float64
		bad  string // the field of the 400, "" when valid
	}{
		{"off", api.ApproximateAggregatesRequest{}, "exact", 0, ""},
		{"on, exact", api.ApproximateAggregatesRequest{Enabled: true}, "exact", 0, ""},
		{"approximate takes the default rate", api.ApproximateAggregatesRequest{Enabled: true, CountDistinct: "approximate"}, "approximate", 0.01, ""},
		{"approximate with a rate", api.ApproximateAggregatesRequest{Enabled: true, CountDistinct: "approximate", ErrorRate: f(0.0005)}, "approximate", 0.0005, ""},
		{"the upper bound", api.ApproximateAggregatesRequest{Enabled: true, CountDistinct: "approximate", ErrorRate: f(0.1)}, "approximate", 0.1, ""},
		{"off with approximate", api.ApproximateAggregatesRequest{CountDistinct: "approximate"}, "", 0, "/countDistinct"},
		{"a rate below the range", api.ApproximateAggregatesRequest{Enabled: true, CountDistinct: "approximate", ErrorRate: f(0.0004)}, "", 0, "/errorRate"},
		{"a rate above the range", api.ApproximateAggregatesRequest{Enabled: true, CountDistinct: "approximate", ErrorRate: f(0.11)}, "", 0, "/errorRate"},
		{"a rate with exact", api.ApproximateAggregatesRequest{Enabled: true, CountDistinct: "exact", ErrorRate: f(0.01)}, "", 0, "/errorRate"},
		{"an unknown mode", api.ApproximateAggregatesRequest{Enabled: true, CountDistinct: "fast"}, "", 0, "/countDistinct"},
	}
	for _, tc := range cases {
		mode, rate, err := validateApprox(tc.req)
		if tc.bad == "" {
			if err != nil || mode != tc.mode || rate != tc.rate {
				t.Errorf("%s: %q %v %v", tc.name, mode, rate, err)
			}
			continue
		}
		var p *problem.Problem
		if !errors.As(err, &p) || p.Code != problem.ValidationFailed || len(p.Errors) == 0 || p.Errors[0].Field != tc.bad {
			t.Errorf("%s: %v", tc.name, err)
		}
	}
}

func TestGetApproximateAggregatesStatesOfTheServers(t *testing.T) {
	c := newFakeCluster("a", "b", "c", "d")
	c.ext["a"].installed[ExtSketch] = "1.0"
	c.ext["b"].installed[ExtSketch] = "1.0"
	c.ext["c"].installed[ExtSketch] = "0.9"
	c.down["d"] = true
	o := c.ops["a"]
	states := func() map[string]string {
		out, err := o.getServerStates(context.Background(), "shop")
		if err != nil {
			t.Fatal(err)
		}
		m := map[string]string{}
		for _, s := range out.Servers {
			m[s.ServerID] = s.State
		}
		return m
	}
	got := states()
	want := map[string]string{"a": "ok", "b": "ok", "c": "versionMismatch", "d": "unreachable"}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s: %q, want %q (%v)", k, got[k], v, got)
		}
	}
	version := func() *string {
		out, err := o.getServerStates(context.Background(), "shop")
		if err != nil {
			t.Fatal(err)
		}
		return out.Version
	}
	if version() != nil {
		t.Error("a version is reported although the servers differ")
	}
	c.ext["c"].installed[ExtSketch] = "1.0"
	if v := version(); v == nil || *v != "1.0" {
		t.Errorf("version %v", v)
	}
	delete(c.ext["b"].installed, ExtSketch)
	if got := states(); got["b"] != "notInstalled" {
		t.Errorf("b: %q", got["b"])
	}
}
