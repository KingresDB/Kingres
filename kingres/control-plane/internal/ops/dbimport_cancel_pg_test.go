// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package ops

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/peerclient"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/task"
)

// Tests of the exits of an import that has NOT passed its commit point: a cancel in the last step (also while a server is waited for), the commit that a cancel
// wins, the bound of the wait for a server, and the link of the upload to its task.

// gone makes every call to the second server fail as a server that is gone for good.
func (r *importRig) gone() {
	r.fan.before = func(importRenameOp) error { return fmt.Errorf("%w: connection refused", peerclient.ErrUnreachable) }
}

// undone checks that nothing of the import is left on the first server, that its settings row is gone and that the exclusion of migration 0045 is released: a
// server.add and the server.remove of the dead server are accepted.
func (r *importRig) undone(priv string) {
	r.t.Helper()
	if s := r.state(0, "shop"); s != "missing" {
		r.t.Errorf("server 1: the final name was not put back: %q", s)
	}
	if s := r.state(0, priv); s != "missing" {
		r.t.Errorf("server 1: the private database was not dropped: %q", s)
	}
	if r.setting() != 0 {
		r.t.Error("the settings row of the import is left")
	}
	if err := r.serverTaskInsert(); err != nil {
		r.t.Fatalf("the exclusion of the import is not released (server.add is refused): %v", err)
	}
	if _, err := r.sys.Exec(context.Background(), `INSERT INTO kingres_cp.task (kind, state, subject_type, subject_id, created_by) VALUES ('server.remove', 'pending', 'server', 's2', 'admin')`); err != nil {
		r.t.Fatalf("the removal of the server is refused: %v", err)
	}
}

// waitCalls waits until the fanout has been asked at least n times.
func (r *importRig) waitCalls(n int) {
	r.t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for strings.Count(r.fan.calls(), "s2") < n {
		if time.Now().After(deadline) {
			r.t.Fatalf("the fanout was not asked %d times: %s", n, r.fan.calls())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func waitEnded(t *testing.T, ended chan error) {
	t.Helper()
	select {
	case err := <-ended:
		if err != nil {
			t.Fatalf("the run: %v", err)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("the run does not end")
	}
}

// H1a: a running import that is in its last step, waiting for a server that is gone, is ended by a cancel: the step sees it, the names are put back, the settings
// row is removed, the private database is dropped, and the exclusion is released so that the dead server can be removed.
func TestImportCancelWhileTheLastStepWaitsForAServerThatIsGone(t *testing.T) {
	r := newImportRig(t)
	old := unreachablePause
	unreachablePause = 5 * time.Millisecond
	defer func() { unreachablePause = old }()
	r.kind = KindDatabaseImport
	e1, p := r.engine("A", r.pgStore(), nil)
	tk, priv := r.submit(e1, p)
	r.gone()
	ended := make(chan error, 1)
	go func() { ended <- e1.RunNow(context.Background(), tk.ID, nil) }()
	r.waitCalls(3)
	if got := r.get(e1, tk.ID); got.State != task.Running || got.Mark != "" {
		t.Fatalf("the task before the cancel: %s mark %q", got.State, got.Mark)
	}
	if err := r.serverTaskInsert(); err == nil || !strings.Contains(err.Error(), "KX006") {
		t.Fatalf("a running import must exclude server.add: %v", err)
	}
	if err := e1.Cancel(context.Background(), tk.ID); err != nil {
		t.Fatalf("the cancel of the running import: %v", err)
	}
	waitEnded(t, ended)
	got := r.get(e1, tk.ID)
	if got.State != task.Failed || got.Error == nil || got.Error.Code != task.CodeCanceled || got.Mark != "" {
		t.Fatalf("after the cancel: %s %+v mark %q", got.State, got.Error, got.Mark)
	}
	r.undone(priv)
}

// H1c: without a cancel, the wait for a server that is gone ends after ImportServerWait: the step fails (and says that the import was undone and what stays on that
// server), the import is undone and the exclusion is released.
func TestImportWaitForAServerThatIsGoneIsBounded(t *testing.T) {
	r := newImportRig(t)
	old := unreachablePause
	unreachablePause = 5 * time.Millisecond
	defer func() { unreachablePause = old }()
	r.o[0].ImportServerWait = 150 * time.Millisecond
	r.kind = KindDatabaseImport
	e1, p := r.engine("A", r.pgStore(), nil)
	tk, priv := r.submit(e1, p)
	r.gone()
	started := time.Now()
	if err := e1.RunNow(context.Background(), tk.ID, nil); err != nil {
		t.Fatalf("the run: %v", err)
	}
	if time.Since(started) > 30*time.Second {
		t.Errorf("the wait took %v", time.Since(started))
	}
	got := r.get(e1, tk.ID)
	if got.State != task.Failed || got.Error == nil || got.Mark != "" || !strings.Contains(got.Error.Message, "did not answer") || !strings.Contains(got.Error.Message, "the import was undone") {
		t.Fatalf("the task: %s %+v mark %q", got.State, got.Error, got.Mark)
	}
	r.undone(priv)
}

// H1b: a cancel request that is recorded before the mark is written wins, in the statement of the store: the mark is not written, the import is undone, and the
// task does not roll forward. (The request here is recorded at the recheck, the last act before the commit.)
func TestImportCancelRecordedBeforeTheCommitWinsAndUndoesTheImport(t *testing.T) {
	r := newImportRig(t)
	r.kind = KindDatabaseImport
	e1, p := r.engine("A", r.pgStore(), nil)
	tk, priv := r.submit(e1, p)
	var once sync.Once
	var cerr error
	r.recheck = func(ctx context.Context, revert func() string) error {
		once.Do(func() { cerr = e1.Cancel(ctx, tk.ID) })
		return nil
	}
	if err := e1.RunNow(context.Background(), tk.ID, nil); err != nil {
		t.Fatalf("the run: %v", err)
	}
	if cerr != nil {
		t.Fatalf("the cancel before the commit point: %v", cerr)
	}
	got := r.get(e1, tk.ID)
	if got.State != task.Failed || got.Mark != "" || got.Error == nil || got.Error.Code != task.CodeCanceled {
		t.Fatalf("the task: %s %+v mark %q", got.State, got.Error, got.Mark)
	}
	if c := r.fan.calls(); strings.Contains(c, "open") || strings.Contains(c, "finish") {
		t.Errorf("the import rolled forward: %s", c)
	}
	r.undone(priv)
}

// H1b: a cancel that comes after the mark is written is the cancel of a committed import: it pauses it and nothing is undone (the rest of the cases are in
// dbimport_exit_pg_test.go).
func TestImportCancelAfterTheCommitPausesAndDoesNotUndo(t *testing.T) {
	r := newImportRig(t)
	r.kind = KindDatabaseImport
	e1, p := r.engine("A", r.pgStore(), nil)
	tk, priv := r.submit(e1, p)
	r.pauseCommitted(e1, tk, priv)
	if got := r.get(e1, tk.ID); got.State != task.Paused || got.Mark != importCommitMark(priv) {
		t.Fatalf("the task: %s mark %q", got.State, got.Mark)
	}
	if r.state(0, "shop") == "missing" {
		t.Error("the committed database is gone")
	}
}

// M2: a start that cannot write the id of its task into the upload does not leave an import that nobody can reach: the write is tried again, and when it never
// works the task is cancelled and the request fails (503, to be repeated). A write that works at the second try is a start.
func TestImportStartWithoutTheLinkIsNotLeftBehind(t *testing.T) {
	r := newStartRig(t)
	oldHook, oldTries, oldPause := linkImportHook, importLinkTries, importLinkPause
	importLinkPause = time.Millisecond
	defer func() { linkImportHook, importLinkTries, importLinkPause = oldHook, oldTries, oldPause }()
	// the task of this test stops at its first step: the start is what is looked at
	e1, _ := r.engine("A", r.mem, func(s task.Step) task.Step {
		s.Run = func(ctx context.Context, env *task.Env) error { <-ctx.Done(); return ctx.Err() }
		return s
	})
	r.o[0].SetEngine(e1)
	var writes atomic.Int32
	linkImportHook = func() error { writes.Add(1); return errors.New("the system database does not answer") }
	_, err := r.o[0].StartDatabaseImport(context.Background(), "imp", r.request("shop"))
	var pr *problem.Problem
	if !errors.As(err, &pr) || pr.Code != problem.ServiceUnavailable || writes.Load() != int32(importLinkTries) {
		t.Fatalf("the start without a link: %v after %d writes", err, writes.Load())
	}
	// the task that was started is ended: no import is left that no repeat of the request can reach
	deadline := time.Now().Add(30 * time.Second)
	for {
		ts, _ := r.mem.List(context.Background(), 10)
		if len(ts) == 1 && ts[0].State == task.Failed && ts[0].Error != nil && ts[0].Error.Code == task.CodeCanceled {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the task that was started is not ended: %+v", ts)
		}
		time.Sleep(10 * time.Millisecond)
	}
	r.o[0].wg.Wait()
	// the write works at the second try: a start, and the upload names the task
	writes.Store(0)
	linkImportHook = func() error {
		if writes.Add(1) == 1 {
			return errors.New("a hiccup")
		}
		return nil
	}
	got, err := r.o[0].StartDatabaseImport(context.Background(), "imp", r.request("shop2"))
	if err != nil || got.ID == 0 {
		t.Fatalf("the start with a link at the second try: %+v %v", got, err)
	}
	if rec, rerr := r.o[0].records().Get(context.Background(), r.rec); rerr != nil || rec.TaskID == nil || *rec.TaskID != got.ID {
		t.Fatalf("the upload is not linked to the task: %+v %v", rec, rerr)
	}
	_ = e1.Cancel(context.Background(), got.ID)
	r.o[0].wg.Wait()
}

// markThenCancel writes the mark and then fails the first read (the read-back of SetMark) while it records a cancel request, as a cancel that arrives between
// the two tries of the commit.
type markThenCancel struct {
	*task.MemStore
	mu    sync.Mutex
	stage int
}

func (s *markThenCancel) SetMark(ctx context.Context, id int64, l task.Lease, mark string) error {
	err := s.MemStore.SetMark(ctx, id, l, mark)
	s.mu.Lock()
	if err == nil && s.stage == 0 {
		s.stage = 1
	}
	s.mu.Unlock()
	return err
}

func (s *markThenCancel) Get(ctx context.Context, id int64) (task.Task, error) {
	s.mu.Lock()
	armed := s.stage == 1
	if armed {
		s.stage = 2
	}
	s.mu.Unlock()
	if armed {
		_ = s.MemStore.RequestCancel(ctx, id, time.Now())
		return task.Task{}, errors.New("the task store cannot be read")
	}
	return s.MemStore.Get(ctx, id)
}

// L3: when the first write of the mark was made, its read-back failed and the second write is refused for a cancel, the import is not undone and its error does not
// say that it was: it says that it may be committed.
func TestImportCommitConfirmedWhileACancelArrivesIsNotCalledUndone(t *testing.T) {
	r := newImportRig(t)
	st := &markThenCancel{MemStore: r.mem}
	e1, p := r.engine("A", st, nil)
	tk, priv := r.submit(e1, p)
	if err := e1.RunNow(context.Background(), tk.ID, nil); err != nil {
		t.Fatalf("the run: %v", err)
	}
	got := r.get(e1, tk.ID)
	if got.State != task.Paused || got.Mark != importCommitMark(priv) {
		t.Fatalf("the task: %s mark %q %+v", got.State, got.Mark, got.Error)
	}
	_, reason, _ := strings.Cut(got.Detail, "Reason: ")
	if !strings.Contains(reason, "may be committed") || strings.Contains(reason, "so it was undone") {
		t.Errorf("the reason is not consistent: %q", got.Detail)
	}
	if r.state(0, "shop") == "missing" {
		t.Error("the committed database was put back")
	}
}

// M1 (review 9): when the link cannot be written and the cancel of the task fails as well, the request does not say that the import was canceled: it says that the
// task may still be running and names it, and the task is not ended. A task that ended meanwhile counts as canceled.
func TestImportStartWhenTheLinkAndTheCancelBothFailSaysSo(t *testing.T) {
	r := newStartRig(t)
	oldLink, oldCancel, oldTries, oldPause := linkImportHook, cancelImportHook, importLinkTries, importLinkPause
	importLinkPause = time.Millisecond
	defer func() {
		linkImportHook, cancelImportHook, importLinkTries, importLinkPause = oldLink, oldCancel, oldTries, oldPause
	}()
	e1, _ := r.engine("A", r.mem, func(s task.Step) task.Step {
		s.Run = func(ctx context.Context, env *task.Env) error { <-ctx.Done(); return ctx.Err() }
		return s
	})
	r.o[0].SetEngine(e1)
	linkImportHook = func() error { return errors.New("the system database does not answer") }
	var cancels atomic.Int32
	cancelImportHook = func() error { cancels.Add(1); return errors.New("the task store does not answer") }
	_, err := r.o[0].StartDatabaseImport(context.Background(), "imp", r.request("shop"))
	var pr *problem.Problem
	if !errors.As(err, &pr) || pr.Code != problem.ServiceUnavailable {
		t.Fatalf("the start: %v", err)
	}
	ts, _ := r.mem.List(context.Background(), 10)
	if len(ts) != 1 {
		t.Fatalf("%d tasks", len(ts))
	}
	id := fmt.Sprint(ts[0].ID)
	if !strings.Contains(pr.Detail, "task "+id+" may still be running") || strings.Contains(pr.Detail, "so it was canceled") {
		t.Errorf("the answer: %q", pr.Detail)
	}
	if cancels.Load() != int32(importLinkTries) {
		t.Errorf("%d tries to cancel", cancels.Load())
	}
	if got := r.get(e1, ts[0].ID); got.State == task.Failed || got.State == task.Done {
		t.Errorf("the task ended: %s", got.State)
	}
	// the cancel that works at last ends the task
	cancelImportHook = nil
	if err := r.o[0].cancelUnlinkedImport(ts[0].ID); err != nil {
		t.Fatal(err)
	}
	r.o[0].wg.Wait()
	// a task that has ended is as good as canceled
	if err := r.o[0].cancelUnlinkedImport(ts[0].ID); err != nil {
		t.Errorf("the cancel of an ended task: %v", err)
	}
}
