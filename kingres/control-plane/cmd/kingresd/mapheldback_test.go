// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/failover"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/failover/journal"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
)

// F6-01 (c). The control data tell a standby that does not run from a primary that does not.
func TestTheControlDataTellAStandbyFromAPrimary(t *testing.T) {
	for state, want := range map[string]bool{"in archive recovery": true, "shut down in recovery": true, "in production": false, "shut down": false} {
		if got, err := recoveryFromControlData(state); err != nil || got != want {
			t.Errorf("%q: %v %v, want %v", state, got, err, want)
		}
	}
	for _, state := range []string{"", "in crash recovery", "shutting down", "starting up"} {
		if _, err := recoveryFromControlData(state); err == nil {
			t.Errorf("%q is not an answer", state)
		}
	}
}

// F6-01 (c). A standby whose instance cannot be reached is not read as one that may have promoted: its control data answer. The connection is asked first, and
// both failing is an error (the bootstrap is then not given up).
func TestAnUnreachableStandbyIsAskedByItsControlData(t *testing.T) {
	ctx := context.Background()
	m, _ := abandonRig(t)
	down := errors.New("the instance does not answer")
	cases := []struct {
		name     string
		query    func(context.Context, int) (bool, error)
		data     map[string]string
		dataErr  error
		want     bool
		wantErr  bool
		dataUsed bool
	}{
		{"the connection answers", func(context.Context, int) (bool, error) { return true, nil }, nil, errors.New("not asked"), true, false, false},
		{"stopped standby", func(context.Context, int) (bool, error) { return false, down }, map[string]string{"Database cluster state": "in archive recovery"}, nil, true, false, true},
		{"stopped promoted instance", func(context.Context, int) (bool, error) { return false, down }, map[string]string{"Database cluster state": "shut down"}, nil, false, false, true},
		{"nothing answers", func(context.Context, int) (bool, error) { return false, down }, nil, errors.New("pg_controldata failed"), false, true, true},
		{"a crash is no answer", func(context.Context, int) (bool, error) { return false, down }, map[string]string{"Database cluster state": "in crash recovery"}, nil, false, true, true},
	}
	for _, c := range cases {
		used := false
		m.queryInRec = c.query
		m.controlDataOf = func(context.Context, int) (map[string]string, error) { used = true; return c.data, c.dataErr }
		got, err := m.instanceInRecovery(ctx, 100)
		if (err != nil) != c.wantErr || (err == nil && got != c.want) || used != c.dataUsed {
			t.Errorf("%s: %v %v (control data used %v), want %v err %v used %v", c.name, got, err, used, c.want, c.wantErr, c.dataUsed)
		}
	}
}

// F6-01 (a). A planned bootstrap that is given up asks the old leader's server to end the hold of its primary, and says which server it resumed; the abandon holds
// although the old leader cannot be asked.
func TestAGivenUpPlannedBootstrapAsksTheOldLeaderToEndTheHold(t *testing.T) {
	ctx := context.Background()
	m, js := abandonRig(t)
	abandonWatch(m)
	var asked []string
	m.resumeOldLeader = func(_ context.Context, old string) (bool, error) { asked = append(asked, old); return true, nil }
	_, _ = js.Begin("b1", "s1", 3, "manual", "alice", false)
	_, _ = js.SetPlanned(journal.Planned{TaskID: 42, StandbyID: 3})
	ans, err := m.abandonLocalFull(ctx, "operator")
	if err != nil || !ans.Abandoned || ans.ResumedServer != "s1" || len(asked) != 1 || asked[0] != "s1" {
		t.Fatalf("%+v %v asked %v", ans, err, asked)
	}
	// the old leader cannot be reached: the bootstrap is given up all the same, and no server is named
	m2, js2 := abandonRig(t)
	abandonWatch(m2)
	m2.resumeOldLeader = func(context.Context, string) (bool, error) { return false, errors.New("unreachable") }
	_, _ = js2.Begin("b1", "s1", 3, "manual", "alice", false)
	_, _ = js2.SetPlanned(journal.Planned{TaskID: 42, StandbyID: 3})
	if ans, err := m2.abandonLocalFull(ctx, "operator"); err != nil || !ans.Abandoned || ans.ResumedServer != "" {
		t.Fatalf("%+v %v", ans, err)
	}
	// an ordinary bootstrap has no old leader that was stopped for it
	m3, js3 := abandonRig(t)
	abandonWatch(m3)
	m3.resumeOldLeader = func(context.Context, string) (bool, error) {
		t.Error("the old leader of an ordinary bootstrap was asked")
		return false, nil
	}
	_, _ = js3.Begin("b1", "s1", 3, "manual", "alice", false)
	if ok, err := m3.abandonLocal(ctx, "operator"); err != nil || !ok {
		t.Fatalf("%v %v", ok, err)
	}
}

// F6-02. The abandon takes the lifecycle lock that the promoting call holds across pg_promote: it waits for a promotion that is running, and finds it done (409)
// instead of giving a bootstrap up whose standby is being promoted.
func TestTheAbandonWaitsForAPromotionThatIsRunningAndThenRefuses(t *testing.T) {
	m, js := abandonRig(t)
	seen := abandonWatch(m)
	_, _ = js.Begin("b1", "s1", 3, "manual", "alice", false)
	_ = js.PromotionIntent()
	var mu sync.Mutex
	held := true
	release := make(chan struct{})
	m.lifecycleLock = func(ctx context.Context) (func(), error) {
		<-release // the promoting call holds the lock
		return func() { mu.Lock(); held = false; mu.Unlock() }, nil
	}
	m.inRecovery = func(context.Context) (bool, error) { return true, nil } // the probe alone would say: still a standby
	type res struct {
		ok  bool
		err error
	}
	done := make(chan res, 1)
	go func() { ok, err := m.abandonLocal(context.Background(), "operator"); done <- res{ok, err} }()
	select {
	case r := <-done:
		t.Fatalf("the abandon did not wait for the lock: %+v", r)
	case <-time.After(200 * time.Millisecond):
	}
	// the promotion finishes under the lock and is recorded
	_ = js.Done(journal.StepPromote)
	close(release)
	r := <-done
	var p *problem.Problem
	if r.ok || !errors.As(r.err, &p) || p.Code != problem.Conflict {
		t.Fatalf("the abandon after the promotion: %+v", r)
	}
	if !js.Active() || seen.released != 0 {
		t.Fatalf("the bootstrap that promoted was given up (active %v, released %d)", js.Active(), seen.released)
	}
	mu.Lock()
	defer mu.Unlock()
	if held {
		t.Fatal("the lock was not released by the abandon")
	}
}

// resolveRig is the old leader's side: held back, with a list of servers and an answer for each.
type resolveRig struct {
	m       *mapFailover
	answers map[string]any // mapBootstrapStatus, or an error
	ended   int
	endErr  error
}

func newResolveRig(t *testing.T, answers map[string]any) *resolveRig {
	t.Helper()
	m, _ := abandonRig(t)
	m.self = "s1"
	r := &resolveRig{m: m, answers: answers}
	m.heldBack = func() bool { return true }
	m.memberList = func(context.Context) ([]string, error) { return []string{"s1", "s2", "s3"}, nil }
	m.callPeer = func(_ context.Context, server, method, path string, _, out any) error {
		want := "/failover/map/bootstrap?" + url.Values{"old": {"s1"}}.Encode()
		if method != http.MethodGet || path != want {
			return fmt.Errorf("unexpected call %s %s", method, path)
		}
		switch a := r.answers[server].(type) {
		case mapBootstrapStatus:
			b, _ := json.Marshal(a)
			return json.Unmarshal(b, out)
		case error:
			return a
		}
		return errors.New("no answer")
	}
	m.endHold = func(context.Context) error { r.ended++; return r.endErr }
	return r
}

// F6-01 (b). The old leader ends its own hold only when nothing owns it: no bootstrap that names it is active, no standby was promoted, and a standby is shown to be
// in recovery. A server that does not answer neither blocks nor proves.
func TestTheOldLeaderEndsAHoldThatNothingOwns(t *testing.T) {
	ctx := context.Background()
	in := mapBootstrapStatus{Standby: "in_recovery"}
	cases := []struct {
		name    string
		answers map[string]any
		heldBk  bool
		endErr  error
		want    bool
		ended   int
	}{
		{"a standby in recovery, none active", map[string]any{"s2": in, "s3": mapBootstrapStatus{Standby: "none"}}, true, nil, true, 1},
		{"the other server does not answer", map[string]any{"s2": in, "s3": errors.New("down")}, true, nil, true, 1},
		{"a bootstrap that names it is active", map[string]any{"s2": mapBootstrapStatus{Active: true, Standby: "in_recovery"}, "s3": in}, true, nil, false, 0},
		{"a standby was promoted", map[string]any{"s2": mapBootstrapStatus{Standby: "primary"}, "s3": in}, true, nil, false, 0},
		{"no standby is shown in recovery", map[string]any{"s2": mapBootstrapStatus{Standby: "unknown"}, "s3": mapBootstrapStatus{Standby: "none"}}, true, nil, false, 0},
		{"nobody answers", map[string]any{"s2": errors.New("down"), "s3": errors.New("down")}, true, nil, false, 0},
		{"not held back", map[string]any{"s2": in, "s3": in}, false, nil, false, 0},
		{"the hold cannot be ended", map[string]any{"s2": in, "s3": in}, true, errors.New("no"), false, 1},
	}
	for _, c := range cases {
		r := newResolveRig(t, c.answers)
		r.endErr = c.endErr
		r.m.heldBack = func() bool { return c.heldBk }
		got, why := r.m.resolveHeldBack(ctx)
		if got != c.want || r.ended != c.ended || (!got && why == "") {
			t.Errorf("%s: resumed %v (%q), ended %d; want %v, %d", c.name, got, why, r.ended, c.want, c.ended)
		}
	}
}

// F6-01 (b). The old leader's loop resolves the hold by itself (a kingresd that restarted finds the marker and no task), and leaves a hold that has an owner.
func TestTheOldLeadersLoopEndsAHoldThatNothingOwns(t *testing.T) {
	old := heldBackEvery
	heldBackEvery = 10 * time.Millisecond
	defer func() { heldBackEvery = old }()
	r := newResolveRig(t, map[string]any{"s2": mapBootstrapStatus{Standby: "in_recovery"}, "s3": mapBootstrapStatus{Standby: "none"}})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.m.WatchHeldBack(ctx); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if r.ended > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	if r.ended == 0 {
		t.Fatal("the loop did not end the hold")
	}
}

func TestTheOldLeadersLoopLeavesAHoldThatHasAnOwner(t *testing.T) {
	old := heldBackEvery
	heldBackEvery = 10 * time.Millisecond
	defer func() { heldBackEvery = old }()
	r := newResolveRig(t, map[string]any{"s2": mapBootstrapStatus{Active: true, Standby: "in_recovery"}, "s3": mapBootstrapStatus{Standby: "none"}})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.m.WatchHeldBack(ctx); close(done) }()
	time.Sleep(300 * time.Millisecond)
	cancel()
	<-done
	if r.ended != 0 {
		t.Fatal("the loop ended a hold that an active bootstrap owns")
	}
}

// The status call is answered to the server it is about only, and says whether a bootstrap that names that server is active.
func TestTheBootstrapStatusIsAnsweredToTheOldServerOnly(t *testing.T) {
	ctx := context.Background()
	m, js := abandonRig(t)
	d := &haDeps{MapFailover: m}
	m.fd = &failoverDeps{}
	ask := func(caller, old string) (any, error) {
		return handleMapBootstrap(ctx, d, caller, []byte(url.Values{"old": {old}}.Encode()))
	}
	if _, err := ask("s9", "s1"); err == nil {
		t.Fatal("a server asked about another server's bootstrap")
	}
	out, err := ask("s1", "s1")
	if st, _ := out.(mapBootstrapStatus); err != nil || st.Active || st.Standby != "none" {
		t.Fatalf("%+v %v", out, err)
	}
	_, _ = js.Begin("b1", "s1", 3, "manual", "alice", false)
	out, err = ask("s1", "s1")
	if st, _ := out.(mapBootstrapStatus); err != nil || !st.Active {
		t.Fatalf("a bootstrap that names s1 is active: %+v %v", out, err)
	}
	out, _ = ask("s4", "s4")
	if st, _ := out.(mapBootstrapStatus); st.Active {
		t.Fatalf("a bootstrap that names s1 is not one of s4: %+v", out)
	}
	if _, err := handleMapBootstrap(ctx, d, "s1", []byte("")); err == nil {
		t.Fatal("a call without the old server")
	}
}

// F6-03. With no bootstrap in progress anywhere, the operator's call asks every server to end the hold of its primary, and the answer names the server that
// was resumed; a bootstrap that is in progress on another server is given up there and its old leader is named.
func TestTheOperatorsAbandonResumesAnOldLeaderThatNothingOwns(t *testing.T) {
	ctx := context.Background()
	m, _ := abandonRig(t)
	m.self = "s3"
	m.memberList = func(context.Context) ([]string, error) { return []string{"s1", "s2", "s3"}, nil }
	m.heldBack = func() bool { return false }
	var calls []string
	m.callPeer = func(_ context.Context, server, method, path string, _, out any) error {
		calls = append(calls, method+" "+server+" "+path)
		switch path {
		case "/failover/map/abandon":
			b, _ := json.Marshal(mapAbandonAnswer{})
			return json.Unmarshal(b, out)
		case "/failover/map/resume":
			b, _ := json.Marshal(mapResumeAnswer{Resumed: server == "s1"})
			return json.Unmarshal(b, out)
		}
		return fmt.Errorf("unexpected %s", path)
	}
	res, err := m.Abandon(ctx, "ops", "stranded")
	if err != nil || res.Abandoned || res.ResumedServer != "s1" {
		t.Fatalf("%+v %v\n%v", res, err, calls)
	}
	// a bootstrap that is in progress elsewhere is given up there
	m.callPeer = func(_ context.Context, server, _, path string, _, out any) error {
		if path == "/failover/map/abandon" && server == "s2" {
			b, _ := json.Marshal(mapAbandonAnswer{Abandoned: true, ResumedServer: "s1"})
			return json.Unmarshal(b, out)
		}
		b, _ := json.Marshal(mapAbandonAnswer{})
		return json.Unmarshal(b, out)
	}
	res, err = m.Abandon(ctx, "ops", "stuck")
	if err != nil || !res.Abandoned || res.ResumedServer != "s1" {
		t.Fatalf("%+v %v", res, err)
	}
	// nobody to resume: abandoned false and no server named, not an error
	m.callPeer = func(_ context.Context, _, _, path string, _, out any) error {
		if path == "/failover/map/resume" {
			b, _ := json.Marshal(mapResumeAnswer{Detail: "not held back"})
			return json.Unmarshal(b, out)
		}
		b, _ := json.Marshal(mapAbandonAnswer{})
		return json.Unmarshal(b, out)
	}
	if res, err = m.Abandon(ctx, "ops", "x"); err != nil || res != (failover.MapAbandonResult{}) {
		t.Fatalf("%+v %v", res, err)
	}
}

// The resume call answers what it did and why not.
func TestTheResumeCallSaysWhyItDidNothing(t *testing.T) {
	m, _ := abandonRig(t)
	m.heldBack = func() bool { return false }
	out, err := handleMapResume(context.Background(), &haDeps{MapFailover: m}, "s2", nil)
	if a, _ := out.(mapResumeAnswer); err != nil || a.Resumed || a.Detail == "" {
		t.Fatalf("%+v %v", out, err)
	}
}
