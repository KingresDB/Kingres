// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/failover"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/failover/journal"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/internalapi"
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
	cand    string // the server that the hold was made for ("": no record)
}

func newResolveRig(t *testing.T, answers map[string]any) *resolveRig {
	t.Helper()
	m, _ := abandonRig(t)
	m.self = "s1"
	r := &resolveRig{m: m, answers: answers}
	m.candidateRecord = func() string { return r.cand }
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

// F6-01 (b), F7-01. The old leader ends its own hold only when nothing owns it, on the word of the server that the group was handed to: that candidate answers and shows its
// standby in recovery, no bootstrap that names the old leader is active, and no standby was promoted. A candidate that does not answer, or whose standby cannot be read,
// blocks (it may be the promoted primary that took writes, and a standby that answers may follow it). Without a record, and for the operator's call, every server must answer.
func TestTheOldLeaderEndsAHoldThatNothingOwns(t *testing.T) {
	ctx := context.Background()
	in := mapBootstrapStatus{Standby: "in_recovery"}
	none := mapBootstrapStatus{Standby: "none"}
	down := errors.New("down")
	cases := []struct {
		name    string
		cand    string
		strict  bool
		answers map[string]any
		heldBk  bool
		endErr  error
		want    bool
		ended   int
	}{
		{"the candidate shows its standby in recovery, none active", "s2", false, map[string]any{"s2": in, "s3": none}, true, nil, true, 1},
		{"the candidate answers, another server does not", "s2", false, map[string]any{"s2": in, "s3": down}, true, nil, true, 1},
		{"the candidate does not answer, another standby is in recovery", "s2", false, map[string]any{"s2": down, "s3": in}, true, nil, false, 0},
		{"the standby of the candidate cannot be read, another is in recovery", "s2", false, map[string]any{"s2": mapBootstrapStatus{Standby: "unknown"}, "s3": in}, true, nil, false, 0},
		{"the candidate hosts no standby", "s2", false, map[string]any{"s2": none, "s3": in}, true, nil, false, 0},
		{"the candidate was promoted", "s2", false, map[string]any{"s2": mapBootstrapStatus{Standby: "primary"}, "s3": in}, true, nil, false, 0},
		{"a bootstrap that names it is active on the candidate", "s2", false, map[string]any{"s2": mapBootstrapStatus{Active: true, Standby: "in_recovery"}, "s3": in}, true, nil, false, 0},
		{"a bootstrap that names it is active elsewhere", "s2", false, map[string]any{"s2": in, "s3": mapBootstrapStatus{Active: true}}, true, nil, false, 0},
		{"another standby was promoted", "s2", false, map[string]any{"s2": in, "s3": mapBootstrapStatus{Standby: "primary"}}, true, nil, false, 0},
		{"every server answers, none is promoted (no record)", "", false, map[string]any{"s2": in, "s3": none}, true, nil, true, 1},
		{"no record: a server does not answer", "", false, map[string]any{"s2": in, "s3": down}, true, nil, false, 0},
		{"no record: a standby cannot be read", "", false, map[string]any{"s2": in, "s3": mapBootstrapStatus{Standby: "unknown"}}, true, nil, false, 0},
		{"no record: no standby is shown in recovery", "", false, map[string]any{"s2": none, "s3": none}, true, nil, false, 0},
		{"the operator's call: the candidate answered, another server does not", "s2", true, map[string]any{"s2": in, "s3": down}, true, nil, false, 0},
		{"the operator's call: every server answers", "s2", true, map[string]any{"s2": in, "s3": none}, true, nil, true, 1},
		{"nobody answers", "", false, map[string]any{"s2": down, "s3": down}, true, nil, false, 0},
		{"not held back", "s2", false, map[string]any{"s2": in, "s3": in}, false, nil, false, 0},
		{"the hold cannot be ended", "s2", false, map[string]any{"s2": in, "s3": in}, true, errors.New("no"), false, 1},
	}
	for _, c := range cases {
		r := newResolveRig(t, c.answers)
		r.cand = c.cand
		r.endErr = c.endErr
		r.m.heldBack = func() bool { return c.heldBk }
		got, why := r.m.resolveHeldBack(ctx, c.strict)
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

// F7-02. The lease of a bootstrap that was given up cannot be let go of: the abandon fails (503, nothing answered as given up, the old leader is not asked to resume), the
// promoting call refuses a journal that was given up, and the repeat of the abandon finishes the release and then asks the old leader.
func TestAFailedReleaseOfTheLeaseFailsTheAbandonAndTheRepeatFinishesIt(t *testing.T) {
	ctx := context.Background()
	m, js := abandonRig(t)
	seen := abandonWatch(m)
	releaseErr := errors.New("no majority of grantors")
	m.releaseFence = func(context.Context) error { seen.released++; return releaseErr }
	resumed := 0
	m.resumeOldLeader = func(context.Context, string) (bool, error) { resumed++; return true, nil }
	_, _ = js.Begin("b1", "s1", 3, "manual", "alice", false)
	_, _ = js.SetPlanned(journal.Planned{TaskID: 42, StandbyID: 3})
	ans, err := m.abandonLocalFull(ctx, "operator")
	var p *problem.Problem
	if ans.Abandoned || !errors.As(err, &p) || p.Code != problem.ServiceUnavailable || resumed != 0 || len(seen.told) != 0 {
		t.Fatalf("a failed release: %+v %v (resumed %d, told %v)", ans, err, resumed, seen.told)
	}
	// the call that promotes finds the journal given up, whatever became of the lease
	var ce *internalapi.CallError
	if err := promoteBlockedByJournal(m, 0); !errors.As(err, &ce) || ce.Status != 409 {
		t.Fatalf("the promoting call was not refused: %v", err)
	}
	if err := promoteBlockedByJournal(m, 1); err != nil {
		t.Fatalf("an ordinary group was refused: %v", err)
	}
	if err := promoteBlockedByJournal(nil, 0); err != nil {
		t.Fatalf("no map failover: %v", err)
	}
	// the repeat, with the lease let go of this time, finishes what the first call began
	releaseErr = nil
	ans, err = m.abandonLocalFull(ctx, "operator again")
	if err != nil || !ans.Abandoned || ans.ResumedServer != "s1" || resumed != 1 || len(seen.told) != 1 {
		t.Fatalf("the repeat: %+v %v (resumed %d, told %v)", ans, err, resumed, seen.told)
	}
	if ans, err := m.abandonLocalFull(ctx, "a third time"); err != nil || ans.Abandoned {
		t.Fatalf("a third call has nothing to do: %+v %v", ans, err)
	}
}

// A bootstrap that is in progress is not refused to promote.
func TestAJournalInProgressDoesNotRefuseThePromotingCall(t *testing.T) {
	m, js := abandonRig(t)
	_, _ = js.Begin("b1", "s1", 3, "manual", "alice", false)
	if err := promoteBlockedByJournal(m, 0); err != nil {
		t.Fatalf("%v", err)
	}
}

// F7-04 (M11b). The lifecycle lock is held until the lease is let go of: the promoting call that waited for it must find either the promotion done or the lease gone.
func TestTheLifecycleLockIsHeldUntilTheLeaseIsLetGoOf(t *testing.T) {
	m, js := abandonRig(t)
	abandonWatch(m)
	var mu sync.Mutex
	locked, heldAtRelease := false, false
	m.lifecycleLock = func(context.Context) (func(), error) {
		mu.Lock()
		locked = true
		mu.Unlock()
		return func() { mu.Lock(); locked = false; mu.Unlock() }, nil
	}
	m.releaseFence = func(context.Context) error {
		mu.Lock()
		heldAtRelease = locked
		mu.Unlock()
		return nil
	}
	_, _ = js.Begin("b1", "s1", 3, "manual", "alice", false)
	if ok, err := m.abandonLocal(context.Background(), "operator"); err != nil || !ok {
		t.Fatalf("%v %v", ok, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if !heldAtRelease || locked {
		t.Fatalf("the lock was held at the release: %v, and is released at the end: %v", heldAtRelease, !locked)
	}
}

// F7-04 (M18b). The loop that resolves a hold is started by kingresd: a test of the wiring, since the loop tests call it directly.
func TestTheLoopThatResolvesAHoldIsStartedByMain(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(src), "MapFailover.WatchHeldBack(") {
		t.Fatal("main.go does not start WatchHeldBack")
	}
}

// countingHandler counts the warnings of a logger.
type countingHandler struct {
	mu    sync.Mutex
	warns int
}

func (h *countingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *countingHandler) Handle(_ context.Context, r slog.Record) error {
	if r.Level >= slog.LevelWarn {
		h.mu.Lock()
		h.warns++
		h.mu.Unlock()
	}
	return nil
}
func (h *countingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *countingHandler) WithGroup(string) slog.Handler      { return h }
func (h *countingHandler) count() int                         { h.mu.Lock(); defer h.mu.Unlock(); return h.warns }

// F7-03. A hold that cannot be resolved is named at warning level once a minute and shown in the failover status with the reason; when it is resolved the status goes.
func TestAHoldThatCannotBeResolvedIsNamedOnceAMinuteAndInTheStatus(t *testing.T) {
	ctx := context.Background()
	down := errors.New("down")
	r := newResolveRig(t, map[string]any{"s2": down, "s3": down})
	r.cand = "s2"
	h := &countingHandler{}
	r.m.log = slog.New(h)
	t0 := time.Unix(1_000_000, 0)
	r.m.watchOnce(ctx, t0)
	if h.count() != 1 {
		t.Fatalf("the first look: %d warnings", h.count())
	}
	r.m.watchOnce(ctx, t0.Add(30*time.Second))
	if h.count() != 1 {
		t.Fatalf("a second look within the minute: %d warnings", h.count())
	}
	r.m.watchOnce(ctx, t0.Add(61*time.Second))
	if h.count() != 2 {
		t.Fatalf("a look after the minute: %d warnings", h.count())
	}
	st, ok := r.m.Status()
	if !ok || st.State != "paused" || st.PausedReason == nil || !strings.Contains(*st.PausedReason, "does not answer") {
		t.Fatalf("the status of a hold that cannot be resolved: %+v %v", st, ok)
	}
	// the candidate answers: the hold is resolved and the status goes
	r.answers["s2"] = mapBootstrapStatus{Standby: "in_recovery"}
	r.m.watchOnce(ctx, t0.Add(70*time.Second))
	if r.ended != 1 {
		t.Fatalf("the hold was not ended: %d", r.ended)
	}
	if _, ok := r.m.Status(); ok {
		t.Fatal("the status of a resolved hold remains")
	}
}

// F7-01. Each rule that keeps a hold says its own reason (the log and the status name it), so that a rule that is lost is seen and not covered by another.
func TestEachRuleThatKeepsAHoldSaysItsReason(t *testing.T) {
	ctx := context.Background()
	in := mapBootstrapStatus{Standby: "in_recovery"}
	down := errors.New("down")
	cases := []struct {
		name    string
		cand    string
		answers map[string]any
		reason  string
	}{
		{"the candidate does not answer", "s2", map[string]any{"s2": down, "s3": in}, "to which the map group was handed, does not answer"},
		{"the standby of the candidate cannot be read", "s2", map[string]any{"s2": mapBootstrapStatus{Standby: "unknown"}, "s3": in}, "cannot be read"},
		{"the candidate hosts no standby", "s2", map[string]any{"s2": mapBootstrapStatus{Standby: "none"}, "s3": in}, "hosts no standby"},
		{"the candidate is not a server of the cluster", "s9", map[string]any{"s2": in, "s3": in}, "did not show its standby in recovery"},
		{"a bootstrap is active", "s2", map[string]any{"s2": in, "s3": mapBootstrapStatus{Active: true}}, "is active on s3"},
		{"a standby was promoted", "s2", map[string]any{"s2": in, "s3": mapBootstrapStatus{Standby: "primary"}}, "was promoted"},
		{"no record, a server does not answer", "", map[string]any{"s2": in, "s3": down}, "the server s3 does not answer"},
		{"no record, no standby in recovery", "", map[string]any{"s2": mapBootstrapStatus{Standby: "none"}, "s3": mapBootstrapStatus{Standby: "none"}}, "no standby of the map group could be shown"},
	}
	for _, c := range cases {
		r := newResolveRig(t, c.answers)
		r.cand = c.cand
		got, why := r.m.resolveHeldBack(ctx, false)
		if got || !strings.Contains(why, c.reason) || r.ended != 0 {
			t.Errorf("%s: resumed %v, reason %q, want a reason with %q", c.name, got, why, c.reason)
		}
	}
	// the operator's call asks every server, whatever the record says
	r := newResolveRig(t, map[string]any{"s2": in, "s3": down})
	r.cand = "s2"
	if got, why := r.m.resolveHeldBack(ctx, true); got || !strings.Contains(why, "the server s3 does not answer") {
		t.Errorf("the operator's call: %v %q", got, why)
	}
}

// F7-02. The promoting call consults the journal of the bootstrap (a test of the wiring: the handler needs a whole server to run).
func TestThePromotingCallConsultsTheJournalOfTheBootstrap(t *testing.T) {
	src, err := os.ReadFile("failover.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(src), "promoteBlockedByJournal(d.MapFailover, r.Group)") {
		t.Fatal("handleFailoverPromote does not ask the journal of the bootstrap")
	}
}
