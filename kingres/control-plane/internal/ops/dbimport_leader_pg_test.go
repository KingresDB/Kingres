// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package ops

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/peerclient"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/sysdb"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/task"
)

// Tests of the roll forward after the commit point when another server runs it (a change of leader), when it is cancelled, and when the outcome of the commit is
// not known. They use the rig of dbimport_commit_pg_test.go.

// routeFan is the fanout of a server that routes every call to the Ops of the server it names, as the real fanout does; a call to itself is an error.
type routeFan struct {
	mu     sync.Mutex
	self   string
	ops    map[string]*Ops
	others []OtherServer
	log    []string
	before func(server string, op importRenameOp) error
}

func (f *routeFan) Others(context.Context) ([]OtherServer, error) { return f.others, nil }

func (f *routeFan) Register(context.Context, string, OtherServer) error { return nil }

func (f *routeFan) Apply(ctx context.Context, server, kind string, params any) error {
	op, ok := params.(importRenameOp)
	if !ok {
		return nil
	}
	name := "rename"
	switch {
	case op.Back:
		name = "back"
	case op.Open:
		name = "open"
	case op.Finish:
		name = "finish"
	}
	f.mu.Lock()
	f.log = append(f.log, server+" "+name)
	before := f.before
	f.mu.Unlock()
	if server == f.self {
		return errors.New("a call to this very server")
	}
	if before != nil {
		if err := before(server, op); err != nil {
			return err
		}
	}
	peer := f.ops[server]
	if peer == nil {
		return peerclient.ErrUnreachable
	}
	return peer.importRenameLocal(ctx, op)
}

func (f *routeFan) calls() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Join(f.log, ",")
}

// killAfterMark stops the run (the process is gone) right after the commit mark was written: both servers have the final name and are closed.
type killAfterMark struct {
	*task.MemStore
	kill context.CancelFunc
}

func (s *killAfterMark) SetMark(ctx context.Context, id int64, l task.Lease, mark string) error {
	err := s.MemStore.SetMark(ctx, id, l, mark)
	s.kill()
	return err
}

// H1 of the sixth review: the leader that started the import dies after the commit point and before it opened anything; another server becomes the leader. The list
// of servers of the request does not name the old leader (it holds the others), so the new leader must open and finish the database on it too, or the database stays
// closed there for ever while the task ends done.
func TestImportRollForwardAfterALeaderChangeFinishesTheOldLeader(t *testing.T) {
	for _, tc := range []struct {
		name   string
		left   bool
		origin string
		others []OtherServer
	}{
		{"the server that is in the cluster now", false, "", []OtherServer{{ID: "s1"}}},
		{"the starting server named by the request and still a member", false, "s1", []OtherServer{{ID: "s1"}}},
		{"the starting server named by the request that left the cluster is skipped", true, "s1", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newImportRig(t)
			runCtx, kill := context.WithCancel(context.Background())
			defer kill()
			e1, p := r.engine("A", &killAfterMark{MemStore: r.mem, kill: kill}, nil)
			p.Origin = tc.origin
			tk, err := e1.Submit(context.Background(), "test.import.final", task.Subject{Type: "database", ID: "shop"}, p, "admin")
			if err != nil {
				t.Fatal(err)
			}
			priv := p.private(tk.ID)
			r.makePrivates(priv)
			_ = e1.RunNow(runCtx, tk.ID, nil)
			mid := r.get(e1, tk.ID)
			if mid.Mark != importCommitMark(priv) {
				t.Fatalf("after the stop: %s mark %q", mid.State, mid.Mark)
			}
			closed := "kingresd/" + importMarker(priv)
			if r.state(0, "shop") != closed || r.state(1, "shop") != closed {
				t.Fatalf("before the change of leader: %q %q", r.state(0, "shop"), r.state(1, "shop"))
			}
			// server 2 is the leader now: its list of "others" has server 1, the list of the request has server 2 itself
			fan2 := &routeFan{self: "s2", ops: map[string]*Ops{"s1": r.o[0]}, others: tc.others}
			r.o[1].Fanout = fan2
			def := task.Definition{Kind: "test.import.final", Committed: importCommittedNote, OnFail: r.o[1].importOnFailHook,
				Steps: []task.Step{{Name: "Take the final name", Run: func(ctx context.Context, env *task.Env) error {
					return r.o[1].importRenameFinal(ctx, env, r.w(p, p.private(env.Task.ID)))
				}}}}
			r.clk.Add(3 * time.Minute)
			e2, err := task.NewEngine(r.mem, []task.Definition{def}, task.Options{Holder: "B", Lease: time.Minute, StoreTries: 1, Now: r.clk.Now})
			if err != nil {
				t.Fatal(err)
			}
			// a roll forward that cannot reach a server waits for it for ever: the run is bounded, so that the test fails and does not hang
			bound, stop := context.WithTimeout(context.Background(), 90*time.Second)
			defer stop()
			if ran, err := e2.RunOnce(bound); !ran || err != nil {
				t.Fatalf("the takeover: %v %v", ran, err)
			}
			done := r.get(e2, tk.ID)
			if done.State != task.Done {
				t.Fatalf("after the change of leader: %s %+v %s", done.State, done.Error, done.Detail)
			}
			if tc.left {
				// H1d: a server that is no longer a member is skipped: the task ends, nothing is called on it, and the detail says so
				if got := r.state(1, "shop"); got != "imp/" {
					t.Errorf("the new leader after the roll forward: %q", got)
				}
				if got := r.state(0, "shop"); got != closed {
					t.Errorf("the server that left was touched: %q", got)
				}
				if c := fan2.calls(); strings.Contains(c, "s1 ") {
					t.Errorf("a call was made to a server that is not a member: %s", c)
				}
				if !strings.Contains(done.Detail, "no longer members") || !strings.Contains(done.Detail, "s1") {
					t.Errorf("the detail does not name the skipped server: %q", done.Detail)
				}
				return
			}
			for i := 0; i < 2; i++ {
				if got := r.state(i, "shop"); got != "imp/" {
					t.Errorf("server %d after the roll forward: %q (the old leader is closed or still marked)", i+1, got)
				}
			}
			c := fan2.calls()
			if !strings.Contains(c, "s1 open") || !strings.Contains(c, "s1 finish") {
				t.Errorf("the old leader was not opened and finished: %s", c)
			}
			if strings.Contains(c, "s2 ") {
				t.Errorf("the new leader called itself: %s", c)
			}
			r.clientWrites(0, "on-the-old-leader")
		})
	}
}

// M2 of the sixth review: a roll forward that cannot heal (a peer that refuses for ever) does not hold the task for ever against a cancel. The cancel ends the run
// without undoing anything, the task is paused as committed and not finished, and a resume (a repeat of the request) goes on from the mark and finishes it.
func TestImportCancelInsideTheRollForwardPausesAndResumes(t *testing.T) {
	r := newImportRig(t)
	old := importCancelPoll
	importCancelPoll = 5 * time.Millisecond
	defer func() { importCancelPoll = old }()
	e1, p := r.engine("A", r.mem, nil)
	tk, priv := r.submit(e1, p)
	var refusals atomic.Int32
	r.fan.before = func(op importRenameOp) error {
		if op.Open {
			refusals.Add(1)
			return &peerclient.APIError{Status: 409, Code: "conflict", Message: "refused"}
		}
		return nil
	}
	ended := make(chan error, 1)
	go func() { ended <- e1.RunNow(context.Background(), tk.ID, nil) }()
	deadline := time.Now().Add(30 * time.Second)
	for refusals.Load() < 3 {
		if time.Now().After(deadline) {
			t.Fatal("the roll forward never retried")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := e1.Cancel(context.Background(), tk.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-ended:
		if err != nil {
			t.Fatalf("the run: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("a cancel did not end the roll forward")
	}
	got := r.get(e1, tk.ID)
	if got.State != task.Paused || got.Mark != importCommitMark(priv) {
		t.Fatalf("after the cancel: %s mark %q %+v", got.State, got.Mark, got.Error)
	}
	if !strings.Contains(got.Detail, "is committed and not finished") || !strings.Contains(got.Detail, "s2") {
		t.Errorf("the task does not say that it is committed and not finished: %q", got.Detail)
	}
	if c := r.fan.calls(); strings.Contains(c, "back") || strings.Contains(c, "drop") {
		t.Errorf("the cancel undid something: %s", c)
	}
	if r.state(0, "shop") == "missing" || r.state(1, "shop") == "missing" {
		t.Fatalf("the database is gone: %q %q", r.state(0, "shop"), r.state(1, "shop"))
	}
	// the operator repeats the request: the task goes on from the mark
	r.fan.before = nil
	if err := e1.Resume(context.Background(), tk.ID); err != nil {
		t.Fatal(err)
	}
	if ran, err := e1.RunOnce(context.Background()); !ran || err != nil {
		t.Fatalf("the resumed run: %v %v", ran, err)
	}
	if got := r.get(e1, tk.ID); got.State != task.Done {
		t.Fatalf("after the resume: %s %+v", got.State, got.Error)
	}
	for i := 0; i < 2; i++ {
		if s := r.state(i, "shop"); s != "imp/" {
			t.Errorf("server %d: %q", i+1, s)
		}
	}
}

// A shutdown inside the roll forward ends the run with the context error and leaves the mark: the next run goes on.
func TestImportShutdownInsideTheRollForwardLeavesTheMark(t *testing.T) {
	r := newImportRig(t)
	e1, p := r.engine("A", r.mem, nil)
	tk, priv := r.submit(e1, p)
	runCtx, kill := context.WithCancel(context.Background())
	defer kill()
	var refusals atomic.Int32
	r.fan.before = func(op importRenameOp) error {
		if op.Open {
			if refusals.Add(1) == 3 {
				kill()
			}
			return &peerclient.APIError{Status: 409, Code: "conflict", Message: "refused"}
		}
		return nil
	}
	ended := make(chan error, 1)
	go func() { ended <- e1.RunNow(runCtx, tk.ID, nil) }()
	select {
	case <-ended:
	case <-time.After(30 * time.Second):
		t.Fatal("a shutdown did not end the roll forward")
	}
	got := r.get(e1, tk.ID)
	if got.State != task.Running || got.Mark != importCommitMark(priv) {
		t.Fatalf("after the shutdown: %s mark %q", got.State, got.Mark)
	}
	_, done := r.restart(tk.ID)
	if done.State != task.Done {
		t.Fatalf("after the restart: %s %+v", done.State, done.Error)
	}
}

// unknownCommit is a store that does not answer after the first write of the mark: the step cannot tell whether the commit is there.
type unknownCommit struct {
	*task.MemStore
	broken atomic.Bool
	fixed  atomic.Bool
}

// SetMark does not answer (and breaks the reads) until the store is fixed.
func (s *unknownCommit) SetMark(ctx context.Context, id int64, l task.Lease, mark string) error {
	if !s.fixed.Load() {
		s.broken.Store(true)
		return errors.New("the store went away")
	}
	return s.MemStore.SetMark(ctx, id, l, mark)
}

func (s *unknownCommit) Get(ctx context.Context, id int64) (task.Task, error) {
	if s.broken.Load() {
		return task.Task{}, errors.New("the store went away")
	}
	return s.MemStore.Get(ctx, id)
}

// An unknown outcome of the commit: the step does not put anything back, the engine pauses the task with a detail that says it may be committed, the hook does
// not run, and when the store is back the resumed run finds the databases it renamed and finishes the import.
func TestImportUnknownCommitOutcomeIsNotUndoneAndResumes(t *testing.T) {
	r := newImportRig(t)
	store := &unknownCommit{MemStore: r.mem}
	e1, p := r.engine("A", store, nil)
	tk, priv := r.submit(e1, p)
	if err := e1.RunNow(context.Background(), tk.ID, nil); err != nil {
		t.Fatal(err)
	}
	store.fixed.Store(true)
	store.broken.Store(false)
	got := r.get(e1, tk.ID)
	if got.State != task.Paused || got.Error != nil {
		t.Fatalf("the task: %s %+v", got.State, got.Error)
	}
	if !strings.Contains(got.Detail, "may be committed") && !strings.Contains(got.Detail, "committed") {
		t.Errorf("the detail does not say that the import may be committed: %q", got.Detail)
	}
	closed := "kingresd/" + importMarker(priv)
	if r.state(0, "shop") != closed || r.state(1, "shop") != closed {
		t.Errorf("the databases were changed: %q %q", r.state(0, "shop"), r.state(1, "shop"))
	}
	if c := r.fan.calls(); strings.Contains(c, "back") || strings.Contains(c, "drop") || strings.Contains(c, "open") {
		t.Errorf("the step or the hook acted on an unknown commit: %s", c)
	}
	if r.setting() != 1 {
		t.Error("the settings row was removed")
	}
	if err := e1.Resume(context.Background(), tk.ID); err != nil {
		t.Fatal(err)
	}
	if ran, err := e1.RunOnce(context.Background()); !ran || err != nil {
		t.Fatalf("the resumed run: %v %v", ran, err)
	}
	if got := r.get(e1, tk.ID); got.State != task.Done {
		t.Fatalf("after the resume: %s %+v", got.State, got.Error)
	}
	for i := 0; i < 2; i++ {
		if s := r.state(i, "shop"); s != "imp/" {
			t.Errorf("server %d: %q", i+1, s)
		}
	}
}

// L1 of the sixth review: a run that lost its lease and reaches revert while the run that holds the task has written the commit mark does not put the names back.
func TestImportRevertNeverUndoesACommittedImport(t *testing.T) {
	r := newImportRig(t)
	e1, p := r.engine("A", r.mem, nil)
	tk, priv := r.submit(e1, p)
	r.recheck = func(ctx context.Context, revert func() string) error {
		// the run that holds the task commits while this one is stuck in its recheck
		cur, _ := r.mem.Get(ctx, tk.ID)
		if err := r.mem.SetMark(ctx, tk.ID, task.Lease{Holder: cur.Holder, Epoch: cur.Epoch}, importCommitMark(priv)); err != nil {
			t.Error(err)
		}
		return errors.New("a failover changed the primary" + revert())
	}
	_ = e1.RunNow(context.Background(), tk.ID, nil)
	closed := "kingresd/" + importMarker(priv)
	if r.state(0, "shop") != closed || r.state(1, "shop") != closed {
		t.Errorf("a committed import was put back: %q %q", r.state(0, "shop"), r.state(1, "shop"))
	}
	if strings.Contains(r.fan.calls(), "back") {
		t.Errorf("a server was put back: %s", r.fan.calls())
	}
}

// L5 of the sixth review: the hook removes only the settings row that the importing role made.
func TestImportUnrecordKeepsTheRowOfAnotherRole(t *testing.T) {
	r := newImportRig(t)
	sc := pgConnect(t, r.o[0], sysdb.Database)
	if _, err := sc.Exec(context.Background(), `insert into kingres_cp.database_setting (database_name, sharding_enabled, durability, ledger_mode, created_by) values ('shop', false, 'sync', 'off', 'somebody-else')`); err != nil {
		t.Fatal(err)
	}
	if err := r.o[0].unrecordImportedSettings(context.Background(), "shop", "imp"); err != nil {
		t.Fatal(err)
	}
	if r.setting() != 1 {
		t.Error("the row of another role was deleted")
	}
	if err := r.o[0].unrecordImportedSettings(context.Background(), "shop", "somebody-else"); err != nil {
		t.Fatal(err)
	}
	if r.setting() != 0 {
		t.Error("the row of the role was not deleted")
	}
}
