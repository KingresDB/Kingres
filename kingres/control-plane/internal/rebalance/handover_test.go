// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026 RooyeKhat Media Co

package rebalance

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/task"
)

// memHandover is a marker store in memory.
type memHandover struct {
	mu sync.Mutex
	m  *MapHandover
}

func (h *memHandover) Get(context.Context) (*MapHandover, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.m == nil {
		return nil, nil
	}
	c := *h.m
	return &c, nil
}
func (h *memHandover) Put(_ context.Context, m MapHandover) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.m = &m
	return nil
}
func (h *memHandover) Clear(context.Context) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.m = nil
	return nil
}
func (h *memHandover) state() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.m == nil {
		return ""
	}
	return h.m.State
}

func TestDecideHandover(t *testing.T) {
	mk := func(state string) *MapHandover { return &MapHandover{TaskID: 7, State: state} }
	cases := []struct {
		name  string
		m     *MapHandover
		hosts bool
		want  handoverAction
		code  string
	}{
		{"not hosted, no marker", nil, false, handoverDone, ""},
		{"not hosted, marker started", mk(HandoverStarted), false, handoverDone, ""},
		{"hosted, no marker", nil, true, handoverRequest, ""},
		{"hosted, marker of another task", &MapHandover{TaskID: 3, State: HandoverStarted}, true, handoverRequest, ""},
		{"hosted, requested (leader ended before the pause)", mk(HandoverRequested), true, handoverRequest, ""},
		{"hosted, started and back (rolled back)", mk(HandoverStarted), true, handoverFail, "server_hosts_map_group"},
		{"hosted, refused with a code", &MapHandover{TaskID: 7, State: HandoverRefused, Code: "no_replica_available", Message: "none"}, true, handoverFail, "no_replica_available"},
		{"hosted, refused without a code", mk(HandoverRefused), true, handoverFail, "server_hosts_map_group"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, code, msg := decideHandover(c.m, 7, c.hosts)
			if got != c.want || code != c.code {
				t.Fatalf("got action %d code %q, want %d %q", got, code, c.want, c.code)
			}
			if got == handoverFail && msg == "" {
				t.Fatal("a failure has a text")
			}
		})
	}
}

func TestHandoverReady(t *testing.T) {
	for _, c := range []struct {
		name string
		m    MapHandover
		want bool
	}{
		{"started", MapHandover{State: HandoverStarted}, true},
		{"refused", MapHandover{State: HandoverRefused}, true},
		{"requested just now", MapHandover{State: HandoverRequested, Age: time.Second}, false},
		{"requested exactly at the grace", MapHandover{State: HandoverRequested, Age: handoverGrace}, false},
		{"requested long ago", MapHandover{State: HandoverRequested, Age: handoverGrace + time.Second}, true},
		{"unknown", MapHandover{State: "x", Age: time.Hour}, false},
	} {
		if got := handoverReady(&c.m); got != c.want {
			t.Errorf("%s: %v", c.name, got)
		}
	}
}

func TestDrainRefusalOfTheMapGroup(t *testing.T) {
	three := []string{"a", "b", "c"}
	ok := DrainFacts{Servers: three, Receivers: []string{"b", "c"}, HostsMapGroup: true, MapStandbys: []string{"b"}}
	hook := func(context.Context, string, string) error { return nil }
	for _, c := range []struct {
		name string
		s    *Service
		f    DrainFacts
		want problem.Code
	}{
		{"hand-over available", &Service{HandOverMapGroup: hook}, ok, ""},
		{"no switchover in the build", &Service{}, ok, problem.ServerHostsMapGroup},
		{"no standby of the map group elsewhere", &Service{HandOverMapGroup: hook},
			DrainFacts{Servers: three, Receivers: []string{"b", "c"}, HostsMapGroup: true}, problem.ServerHostsMapGroup},
		{"a server that does not host it", &Service{HandOverMapGroup: hook}, DrainFacts{Servers: three, Receivers: []string{"b"}}, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			c.s.ServerStatus = func(context.Context, string) string { return "live" }
			err := c.s.drainRefusal(context.Background(), "a", DrainRequest{}, c.f)
			if c.want == "" {
				if err != nil {
					t.Fatalf("got %v", err)
				}
				return
			}
			if err == nil || codeOf(t, err) != c.want {
				t.Fatalf("got %v, want %s", err, c.want)
			}
		})
	}
}

func TestMapGroupStepIsSkippedWhereItDoesNotApply(t *testing.T) {
	ctx := context.Background()
	env := task.EnvFor(task.Task{ID: 7})
	if err := (&Service{HandOverMapGroup: func(context.Context, string, string) error { return nil }}).stepDrainMapGroup(ctx, env, nil, Options{}); err == nil {
		t.Error("a rebalance skips the step")
	}
	if err := (&Service{}).stepDrainMapGroup(ctx, env, nil, Options{DrainServer: "a"}); err == nil {
		t.Error("a build without the switchover skips the step")
	}
}

// handoverEnv is an engine over memory with one definition whose only step is the step under test, and the service around it.
type handoverEnv struct {
	s     *Service
	eng   *task.Engine
	store *memHandover
	mu    sync.Mutex
	hosts bool
	asked []string
	err   error
}

func newHandoverEnv(t *testing.T) *handoverEnv {
	t.Helper()
	h := &handoverEnv{store: &memHandover{}, hosts: true}
	h.s = &Service{Handover: h.store, DrainPoll: time.Millisecond,
		HostsMapGroup: func(context.Context, string) (bool, error) { h.mu.Lock(); defer h.mu.Unlock(); return h.hosts, nil },
		HandOverMapGroup: func(_ context.Context, actor, server string) error {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.asked = append(h.asked, actor+"@"+server)
			return h.err
		}}
	def := task.Definition{Kind: Kind, Steps: []task.Step{{Name: StepDrainMapGroup, Run: func(ctx context.Context, env *task.Env) error {
		return h.s.stepDrainMapGroup(ctx, env, nil, Options{DrainServer: "a"})
	}}}}
	eng, err := task.NewEngine(task.NewMemStore(), []task.Definition{def}, task.Options{Holder: "A", Lease: time.Minute, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	h.eng = eng
	h.s.Engine = func() *task.Engine { return eng }
	return h
}

func (h *handoverEnv) set(hosts bool, err error) {
	h.mu.Lock()
	h.hosts, h.err = hosts, err
	h.mu.Unlock()
}

func (h *handoverEnv) askedNow() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.asked...)
}

func waitFor(t *testing.T, what string, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !f() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// The step pauses its task, and only then asks for the switchover (a failover task is refused while a rebalance runs), once; the marker
// says started. After the switchover (the server no longer hosts the primary) the resumed step is done.
func TestMapGroupStepPausesAsksOnceAndIsDoneAfterTheSwitchover(t *testing.T) {
	h := newHandoverEnv(t)
	ctx := context.Background()
	tk, err := h.eng.Submit(ctx, Kind, task.Subject{Type: SubjectType, ID: SubjectID}, input{}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	h.s.BGFn = func() context.Context { return ctx }
	if err := h.eng.RunNow(ctx, tk.ID, nil); err != nil {
		t.Fatalf("run: %v", err)
	}
	got, _ := h.eng.Get(ctx, tk.ID)
	if got.State != task.Paused {
		t.Fatalf("the drain is %s, want paused", got.State)
	}
	waitFor(t, "the marker to say started", func() bool { return h.store.state() == HandoverStarted })
	if a := h.askedNow(); len(a) != 1 || a[0] != "admin@a" {
		t.Fatalf("the switchover was asked for %v, want once by the actor of the drain for server a", a)
	}
	// the new leader resumes the task: the server no longer hosts the map group
	h.set(false, nil)
	if err := h.eng.Resume(ctx, tk.ID); err != nil {
		t.Fatal(err)
	}
	if err := h.eng.RunNow(ctx, tk.ID, nil); err != nil {
		t.Fatalf("second run: %v", err)
	}
	got, _ = h.eng.Get(ctx, tk.ID)
	if got.State != task.Done {
		t.Fatalf("the drain is %s after the hand-over, want done", got.State)
	}
	if h.store.state() != "" {
		t.Errorf("the marker stays after the step: %q", h.store.state())
	}
	if a := h.askedNow(); len(a) != 1 {
		t.Errorf("the switchover was asked for %d times", len(a))
	}
}

// A refusal of the switchover is recorded with its code; the resumed step fails with that code and clears the marker.
func TestMapGroupStepFailsWithTheCodeOfARefusedSwitchover(t *testing.T) {
	h := newHandoverEnv(t)
	ctx := context.Background()
	h.set(true, problem.New(problem.NoReplicaAvailable, "No server that hosts a ready standby of the map group can be reached."))
	h.s.BGFn = func() context.Context { return ctx }
	tk, _ := h.eng.Submit(ctx, Kind, task.Subject{Type: SubjectType, ID: SubjectID}, input{}, "admin")
	if err := h.eng.RunNow(ctx, tk.ID, nil); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the marker to say refused", func() bool { return h.store.state() == HandoverRefused })
	m, _ := h.store.Get(ctx)
	if m.Code != string(problem.NoReplicaAvailable) || m.Message == "" {
		t.Fatalf("marker %+v", m)
	}
	if err := h.eng.Resume(ctx, tk.ID); err != nil {
		t.Fatal(err)
	}
	_ = h.eng.RunNow(ctx, tk.ID, nil)
	got, _ := h.eng.Get(ctx, tk.ID)
	if got.State != task.Failed || got.Error == nil || !contains(got.Error.Message, string(problem.NoReplicaAvailable)+": ") {
		t.Fatalf("the drain: %s %+v", got.State, got.Error)
	}
	if h.store.state() != "" {
		t.Errorf("the marker stays after the failure: %q", h.store.state())
	}
}

// A switchover that was rolled back leaves the server on the map group: the resumed step fails and the drain can be started again.
func TestMapGroupStepFailsWhenTheSwitchoverWasRolledBack(t *testing.T) {
	h := newHandoverEnv(t)
	ctx := context.Background()
	h.s.BGFn = func() context.Context { return ctx }
	tk, _ := h.eng.Submit(ctx, Kind, task.Subject{Type: SubjectType, ID: SubjectID}, input{}, "admin")
	_ = h.eng.RunNow(ctx, tk.ID, nil)
	waitFor(t, "started", func() bool { return h.store.state() == HandoverStarted })
	if err := h.eng.Resume(ctx, tk.ID); err != nil {
		t.Fatal(err)
	}
	_ = h.eng.RunNow(ctx, tk.ID, nil)
	got, _ := h.eng.Get(ctx, tk.ID)
	if got.State != task.Failed || got.Error == nil || !contains(got.Error.Message, string(problem.ServerHostsMapGroup)+": ") {
		t.Fatalf("the drain: %s %+v", got.State, got.Error)
	}
}

// The switchover is not asked for while the drain still runs: handOverAfter waits for the pause. A task that never pauses gets a refusal.
func TestHandOverWaitsForThePauseAndRefusesWhenThereIsNone(t *testing.T) {
	h := newHandoverEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.s.BGFn = func() context.Context { return ctx }
	// a task that is pending (never runs): no pause comes
	tk, _ := h.eng.Submit(ctx, Kind, task.Subject{Type: SubjectType, ID: SubjectID}, input{}, "admin")
	done := make(chan struct{})
	go func() { h.s.handOverAfter(tk.ID, "a", "admin"); close(done) }()
	time.Sleep(300 * time.Millisecond)
	if a := h.askedNow(); len(a) != 0 {
		t.Fatalf("asked while the drain was not paused: %v", a)
	}
	cancel()
	<-done
	if a := h.askedNow(); len(a) != 0 {
		t.Fatalf("asked after the cancel: %v", a)
	}
}

func TestAHookThatFailsWithAPlainErrorIsRecordedWithoutItsText(t *testing.T) {
	h := newHandoverEnv(t)
	ctx := context.Background()
	h.s.BGFn = func() context.Context { return ctx }
	h.set(true, errors.New("dial tcp 10.1.2.3:5432: refused"))
	tk, _ := h.eng.Submit(ctx, Kind, task.Subject{Type: SubjectType, ID: SubjectID}, input{}, "admin")
	_ = h.eng.RunNow(ctx, tk.ID, nil)
	waitFor(t, "refused", func() bool { return h.store.state() == HandoverRefused })
	m, _ := h.store.Get(ctx)
	if m.Code != string(problem.ServerHostsMapGroup) || m.Message == "" || contains(m.Message, "10.1.2.3") {
		t.Fatalf("marker %+v", m)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
