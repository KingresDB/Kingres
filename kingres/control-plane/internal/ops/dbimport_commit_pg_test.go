// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package ops

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/peerclient"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/pgtest"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/sysdb"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/task"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/transfer"
)

// Tests of the last step of a database import around its ONE COMMIT POINT (stepIMRename without the upload and the archive, importRenameFinal): two real
// PostgreSQL instances stand for two servers, the second one is reached through a fake fanout that calls the operations of its Ops, and a real task engine (the
// in-memory store) runs the step, so that the mark of the task, the restart of kingresd and the failure hook are the real ones.

const importTestNonce = "aabbccddeeff"

type peerFan struct {
	mu   sync.Mutex
	peer *Ops
	log  []string
	// before is called before an operation is applied to the peer; an error is the answer of the peer.
	before func(op importRenameOp) error
	// after is called after an operation was applied to the peer.
	after func(op importRenameOp)
}

func (f *peerFan) Others(context.Context) ([]OtherServer, error) {
	return []OtherServer{{ID: "s2"}}, nil
}

func (f *peerFan) Apply(ctx context.Context, server, kind string, params any) error {
	switch op := params.(type) {
	case importRenameOp:
		f.mu.Lock()
		name := "rename"
		switch {
		case op.Back:
			name = "back"
		case op.Open:
			name = "open"
		case op.Finish:
			name = "finish"
		}
		f.log = append(f.log, server+" "+name)
		before, after := f.before, f.after
		f.mu.Unlock()
		if before != nil {
			if err := before(op); err != nil {
				return err
			}
		}
		if err := f.peer.importRenameLocal(ctx, op); err != nil {
			return err
		}
		if after != nil {
			after(op)
		}
		return nil
	case importDropOp:
		f.mu.Lock()
		f.log = append(f.log, server+" drop")
		f.mu.Unlock()
		return f.peer.importDropLocal(ctx, op.Name)
	}
	return nil
}

func (f *peerFan) Register(context.Context, string, OtherServer) error { return nil }

func (f *peerFan) calls() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Join(f.log, ",")
}

// importRig is two servers, the engines that run the step and the settings of the system database of the first.
type importRig struct {
	t        *testing.T
	in       [2]*pgtest.Instance
	c        [2]*pgx.Conn
	o        [2]*Ops
	fan      *peerFan
	mem      *task.MemStore
	clk      *testClock
	recheck  func(ctx context.Context, revert func() string) error
	stepDone chan struct{}
}

func newImportRig(t *testing.T) *importRig {
	t.Helper()
	if !pgtest.Available() {
		t.Skip("KINGRES_PG_BIN is not set; skipping the PostgreSQL integration test")
	}
	r := &importRig{t: t, clk: &testClock{t: time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC)}}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	for i := range r.in {
		r.in[i] = pgtest.Start(t, nil)
		r.c[i] = r.in[i].Connect(t, "postgres")
		pgExecAll(t, r.c[i], "create role imp login password 'pw'", "create role outsider login password 'pw'")
		r.o[i] = &Ops{Service: pgSvc{r.in[i]}, Log: log, ServerHost: []string{"s1", "s2"}[i]}
	}
	pgExecAll(t, r.c[0], "create database "+sysdb.Database)
	sc := r.in[0].Connect(t, sysdb.Database)
	if _, err := sysdb.Migrate(context.Background(), sc); err != nil {
		t.Fatal(err)
	}
	r.fan = &peerFan{peer: r.o[1]}
	r.o[0].Fanout = r.fan
	r.mem = task.NewMemStoreClock(r.clk.Now)
	oldF, oldP, oldC := importForwardPause, otherErrorPause, importCommitPause
	importForwardPause, otherErrorPause, importCommitPause = time.Millisecond, time.Millisecond, time.Millisecond
	t.Cleanup(func() { importForwardPause, otherErrorPause, importCommitPause = oldF, oldP, oldC })
	return r
}

func (r *importRig) params() dbImportParams {
	return dbImportParams{Database: "shop", Owner: transfer.Identity{Name: "imp"}, Nonce: importTestNonce, Servers: []string{"s2"}}
}

// makePrivates makes the private database of the task on both servers, as the import does, and gives the importing role what the import gives it.
func (r *importRig) makePrivates(priv string) {
	r.t.Helper()
	for i := range r.o {
		if err := r.o[i].createPrivateLocal(context.Background(), importCreateOp{Name: priv, Marker: importMarker(priv), Encoding: "UTF8"}); err != nil {
			r.t.Fatal(err)
		}
		pgExecAll(r.t, r.c[i], "grant connect, create, temporary on database "+priv+" to imp")
	}
}

func (r *importRig) w(p dbImportParams, priv string) renameFinal {
	return renameFinal{p: p, priv: priv, buckets: 128, ledgerMode: "off", checks: renameChecks{
		access:  func(context.Context) error { return nil },
		servers: func(context.Context) error { return nil },
		catalog: func(context.Context) error { return nil },
		recheck: func(ctx context.Context, revert func() string) error {
			if r.recheck != nil {
				return r.recheck(ctx, revert)
			}
			return nil
		},
	}}
}

// engine builds an engine of the holder on the shared store; a step that is given runs in place of the step of the task.
func (r *importRig) engine(holder string, store task.Store, wrap func(task.Step) task.Step) (*task.Engine, dbImportParams) {
	r.t.Helper()
	p := r.params()
	step := task.Step{Name: "Take the final name", Run: func(ctx context.Context, env *task.Env) error {
		priv := p.private(env.Task.ID)
		return r.o[0].importRenameFinal(ctx, env, r.w(p, priv))
	}}
	if wrap != nil {
		step = wrap(step)
	}
	def := task.Definition{Kind: "test.import.final", Steps: []task.Step{step}, OnFail: r.o[0].importOnFailHook}
	e, err := task.NewEngine(store, []task.Definition{def}, task.Options{Holder: holder, Lease: time.Minute, StoreTries: 1, Now: r.clk.Now})
	if err != nil {
		r.t.Fatal(err)
	}
	return e, p
}

// submit makes the task and the private databases of it.
func (r *importRig) submit(e *task.Engine, p dbImportParams) (task.Task, string) {
	r.t.Helper()
	tk, err := e.Submit(context.Background(), "test.import.final", task.Subject{Type: "database", ID: "shop"}, p, "admin")
	if err != nil {
		r.t.Fatal(err)
	}
	priv := p.private(tk.ID)
	r.makePrivates(priv)
	return tk, priv
}

// state of a database on server i: owner/comment, or "missing".
func (r *importRig) state(i int, name string) string { return finalState(r.c[i], name) }

func (r *importRig) setting() int {
	var n int
	sc := pgConnect(r.t, r.o[0], sysdb.Database)
	if err := sc.QueryRow(context.Background(), `select count(*) from kingres_cp.database_setting where database_name = 'shop'`).Scan(&n); err != nil {
		r.t.Fatal(err)
	}
	return n
}

func (r *importRig) get(e *task.Engine, id int64) task.Task {
	tk, err := e.Get(context.Background(), id)
	if err != nil {
		r.t.Fatal(err)
	}
	return tk
}

// crashAt kills the first run when the fanout is asked for the named operation on the peer: the operation is not applied, the run is cancelled and ends the
// way a kingresd that is shut down does.
func (r *importRig) crashAt(kill context.CancelFunc, name string, applyFirst bool) {
	var once sync.Once
	match := func(op importRenameOp) bool {
		switch name {
		case "open":
			return op.Open
		case "finish":
			return op.Finish
		case "rename":
			return !op.Open && !op.Finish && !op.Back
		}
		return false
	}
	if applyFirst {
		r.fan.after = func(op importRenameOp) {
			if match(op) {
				once.Do(kill)
			}
		}
		return
	}
	r.fan.before = func(op importRenameOp) error {
		if match(op) {
			fired := false
			once.Do(func() { fired = true; kill() })
			if fired {
				return context.Canceled
			}
		}
		return nil
	}
}

// restart makes the second engine take the task over, as a kingresd that started again does, and runs it.
func (r *importRig) restart(id int64) (*task.Engine, task.Task) {
	r.t.Helper()
	r.fan.before, r.fan.after = nil, nil
	r.clk.Add(3 * time.Minute) // the lease of the first run runs out
	e2, _ := r.engine("B", r.mem, nil)
	if ran, err := e2.RunOnce(context.Background()); !ran || err != nil {
		r.t.Fatalf("the takeover: %v %v", ran, err)
	}
	return e2, r.get(e2, id)
}

func (r *importRig) clientWrites(i int, v string) {
	r.t.Helper()
	cc, err := asRole(r.in[i], "shop", "imp")
	if err != nil {
		r.t.Fatalf("the importing role cannot connect to the opened database on server %d: %v", i+1, err)
	}
	defer cc.Close(context.Background())
	pgExecAll(r.t, cc, "create table if not exists t(v text)", "insert into t values ('"+v+"')")
}

func (r *importRig) clientRows(i int) string {
	r.t.Helper()
	cc, err := asRole(r.in[i], "shop", "imp")
	if err != nil {
		r.t.Fatalf("the importing role cannot connect to the database on server %d: %v", i+1, err)
	}
	defer cc.Close(context.Background())
	var s string
	if err := cc.QueryRow(context.Background(), `select coalesce(string_agg(v, ',' order by v), '') from t`).Scan(&s); err != nil {
		r.t.Fatal(err)
	}
	return s
}

// (a) a restart between the opening on the first server and the opening on the second: the task rolls forward, the data that a client wrote on the first server
// survives, nothing is put back or dropped.
func TestImportRestartBetweenTheOpeningsRollsForward(t *testing.T) {
	r := newImportRig(t)
	runCtx, kill := context.WithCancel(context.Background())
	defer kill()
	e1, p := r.engine("A", r.mem, nil)
	tk, priv := r.submit(e1, p)
	r.crashAt(kill, "open", false)
	_ = e1.RunNow(runCtx, tk.ID, nil)
	mid := r.get(e1, tk.ID)
	if mid.State != task.Running || mid.Mark != importCommitMark(priv) {
		t.Fatalf("after the crash: state %s mark %q", mid.State, mid.Mark)
	}
	if got := r.state(0, "shop"); got != "imp/"+importMarker(priv) {
		t.Fatalf("server 1 before the restart: %q", got)
	}
	if got := r.state(1, "shop"); got != "kingresd/"+importMarker(priv) {
		t.Fatalf("server 2 before the restart: %q", got)
	}
	// a client works on server 1 while the task is down
	r.clientWrites(0, "written-between")
	e2, done := r.restart(tk.ID)
	if done.State != task.Done {
		t.Fatalf("after the restart: %s %+v", done.State, done.Error)
	}
	_ = e2
	for i := 0; i < 2; i++ {
		if got := r.state(i, "shop"); got != "imp/" {
			t.Errorf("server %d after the restart: %q", i+1, got)
		}
		if got := r.state(i, priv); got != "missing" {
			t.Errorf("server %d still has the private name: %q", i+1, got)
		}
	}
	if got := r.clientRows(0); got != "written-between" {
		t.Errorf("the data of the client on server 1: %q", got)
	}
	if r.setting() != 1 {
		t.Error("the settings row is gone")
	}
	if c := r.fan.calls(); strings.Contains(c, "back") || strings.Contains(c, "drop") {
		t.Errorf("the roll forward put back or dropped something: %s", c)
	}
}

// (b) a restart between the end on the first server and the end on the second: the import is done, nothing is dropped, and the marker is gone from both.
func TestImportRestartBetweenTheEndsIsDone(t *testing.T) {
	r := newImportRig(t)
	runCtx, kill := context.WithCancel(context.Background())
	defer kill()
	e1, p := r.engine("A", r.mem, nil)
	tk, priv := r.submit(e1, p)
	r.crashAt(kill, "finish", false)
	_ = e1.RunNow(runCtx, tk.ID, nil)
	if got := r.state(0, "shop"); got != "imp/" {
		t.Fatalf("server 1 before the restart (marker removed): %q", got)
	}
	if got := r.state(1, "shop"); got != "imp/"+importMarker(priv) {
		t.Fatalf("server 2 before the restart (opened, marked): %q", got)
	}
	r.clientWrites(1, "on-two")
	_, done := r.restart(tk.ID)
	if done.State != task.Done {
		t.Fatalf("after the restart: %s %+v", done.State, done.Error)
	}
	for i := 0; i < 2; i++ {
		if got := r.state(i, "shop"); got != "imp/" {
			t.Errorf("server %d after the restart: %q", i+1, got)
		}
	}
	if got := r.clientRows(1); got != "on-two" {
		t.Errorf("the data of the client on server 2: %q", got)
	}
	if r.setting() != 1 {
		t.Error("the settings row is gone")
	}
	if c := r.fan.calls(); strings.Contains(c, "back") || strings.Contains(c, "drop") {
		t.Errorf("the second run put back or dropped something: %s", c)
	}
}

// After the commit point every error of a server is tried again, and the marker of the first server is not removed before the second one opened: a refusal of the
// peer (not a lost connection) at the opening and at the end never fails the task and never undoes anything.
func TestImportErrorsAfterTheCommitPointAreTriedAgain(t *testing.T) {
	r := newImportRig(t)
	e1, p := r.engine("A", r.mem, nil)
	tk, priv := r.submit(e1, p)
	var n int
	var mu sync.Mutex
	r.fan.before = func(op importRenameOp) error {
		mu.Lock()
		defer mu.Unlock()
		if op.Open || op.Finish {
			n++
			// refused four times: the step of the peer gives up on a refusal that cannot heal, and the step goes round again
			if n <= 4 {
				return &peerclient.APIError{Status: 409, Code: "conflict", Message: "refused"}
			}
		}
		return nil
	}
	if err := e1.RunNow(context.Background(), tk.ID, nil); err != nil {
		t.Fatal(err)
	}
	got := r.get(e1, tk.ID)
	if got.State != task.Done {
		t.Fatalf("the task: %s %+v", got.State, got.Error)
	}
	if n < 5 {
		t.Errorf("the peer was asked %d times", n)
	}
	for i := 0; i < 2; i++ {
		if s := r.state(i, "shop"); s != "imp/" {
			t.Errorf("server %d: %q", i+1, s)
		}
		_ = priv
	}
	if c := r.fan.calls(); strings.Contains(c, "back") || strings.Contains(c, "drop") {
		t.Errorf("an error after the commit point put back or dropped: %s", c)
	}
}

// (c) the hook refuses an opened database: with the committed mark it does nothing at all, and without the mark (the second guard) it still does not touch a
// database that the importing role owns. A closed database of the same import on the other server is not touched by the hook of a committed task either.
func TestImportHookRefusesAnOpenedDatabase(t *testing.T) {
	r := newImportRig(t)
	runCtx, kill := context.WithCancel(context.Background())
	defer kill()
	e1, p := r.engine("A", r.mem, nil)
	tk, priv := r.submit(e1, p)
	r.crashAt(kill, "open", false)
	_ = e1.RunNow(runCtx, tk.ID, nil)
	r.clientWrites(0, "kept")
	failed := r.get(e1, tk.ID)
	failed.State, failed.Error = task.Failed, &task.Error{Code: task.CodeStepFailed, Message: "boom"}
	ctx := context.Background()
	before := [2]string{r.state(0, "shop"), r.state(1, "shop")}
	// the committed task: nothing happens, on either server
	r.o[0].importOnFailHook(ctx, failed)
	if got := [2]string{r.state(0, "shop"), r.state(1, "shop")}; got != before {
		t.Errorf("the hook of a committed task changed %v to %v", before, got)
	}
	if r.state(0, priv) != "missing" || r.state(1, priv) != "missing" || r.setting() != 1 {
		t.Errorf("the hook of a committed task: private %q %q, settings %d", r.state(0, priv), r.state(1, priv), r.setting())
	}
	// the mark cannot be read: the same
	unread := failed
	unread.Mark = task.MarkUnreadable
	r.o[0].importOnFailHook(ctx, unread)
	if got := [2]string{r.state(0, "shop"), r.state(1, "shop")}; got != before {
		t.Errorf("the hook of a task whose mark cannot be read changed %v to %v", before, got)
	}
	// the second guard: a task that is not marked, and a database that the importing role owns (server 1 is opened): it is not put back and not dropped
	nomark := failed
	nomark.Mark = ""
	r.o[0].importOnFailHook(ctx, nomark)
	if got := r.state(0, "shop"); got != before[0] {
		t.Errorf("the hook put back or dropped an opened database: %q, was %q", got, before[0])
	}
	if got := r.clientRows(0); got != "kept" {
		t.Errorf("the data of the client: %q", got)
	}
}

// (d) a failure before the commit point: the names are put back, the settings row is removed, the hook drops the private databases and no database is left.
func TestImportFailureBeforeTheCommitPointIsUndone(t *testing.T) {
	for _, tc := range []struct {
		name string
		fail func(r *importRig)
	}{
		{"the recheck of the catalog", func(r *importRig) {
			r.recheck = func(ctx context.Context, revert func() string) error {
				return errors.New("the catalogs differ" + revert())
			}
		}},
		{"the peer refuses the final name", func(r *importRig) {
			r.fan.before = func(op importRenameOp) error {
				if !op.Open && !op.Finish && !op.Back {
					return &peerclient.APIError{Status: 409, Code: "conflict", Message: "no"}
				}
				return nil
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newImportRig(t)
			e1, p := r.engine("A", r.mem, nil)
			tk, priv := r.submit(e1, p)
			tc.fail(r)
			if err := e1.RunNow(context.Background(), tk.ID, nil); err != nil {
				t.Fatal(err)
			}
			got := r.get(e1, tk.ID)
			if got.State != task.Failed || got.Mark != "" {
				t.Fatalf("the task: %s mark %q %+v", got.State, got.Mark, got.Error)
			}
			for i := 0; i < 2; i++ {
				if s := r.state(i, "shop"); s != "missing" {
					t.Errorf("server %d has %q at the final name", i+1, s)
				}
				if s := r.state(i, priv); s != "missing" {
					t.Errorf("server %d has %q at the private name (the hook drops it)", i+1, s)
				}
			}
			if r.setting() != 0 {
				t.Error("a settings row is left")
			}
		})
	}
}

// (d) the commit itself fails: the store refuses the mark and shows that it is not there; the step undoes its work.
type refusingMarks struct {
	*task.MemStore
	refuse bool
}

func (s *refusingMarks) SetMark(ctx context.Context, id int64, l task.Lease, mark string) error {
	if s.refuse {
		return errors.New("the store is down")
	}
	return s.MemStore.SetMark(ctx, id, l, mark)
}

func TestImportFailureOfTheCommitUndoesTheWork(t *testing.T) {
	r := newImportRig(t)
	store := &refusingMarks{MemStore: r.mem, refuse: true}
	e1, p := r.engine("A", store, nil)
	tk, priv := r.submit(e1, p)
	if err := e1.RunNow(context.Background(), tk.ID, nil); err != nil {
		t.Fatal(err)
	}
	got := r.get(e1, tk.ID)
	if got.State != task.Failed || !strings.Contains(got.Error.Message, "could not be committed") {
		t.Fatalf("the task: %s %+v", got.State, got.Error)
	}
	for i := 0; i < 2; i++ {
		if s, q := r.state(i, "shop"), r.state(i, priv); s != "missing" || q != "missing" {
			t.Errorf("server %d: final %q private %q", i+1, s, q)
		}
	}
	if r.setting() != 0 {
		t.Error("a settings row is left")
	}
	if strings.Contains(r.fan.calls(), "open") {
		t.Errorf("a server was opened before the commit: %s", r.fan.calls())
	}
}

// M2 of the fifth review: a restart after the settings were recorded and before the commit point, and a recheck that fails on the run that goes on: the names are put
// back and the settings row is removed (it stays no more).
func TestImportResumedRecheckFailureLeavesNoSettingsRow(t *testing.T) {
	r := newImportRig(t)
	runCtx, kill := context.WithCancel(context.Background())
	defer kill()
	e0, p := r.engine("A", r.mem, nil)
	tk, priv := r.submit(e0, p)
	// the first run: takes the final name on both servers, the settings are recorded, and kingresd stops when the step writes the commit mark
	killing := &killOnMark{MemStore: r.mem, kill: kill}
	ek, _ := r.engine("A", killing, nil)
	_ = ek.RunNow(runCtx, tk.ID, nil)
	mid := r.get(ek, tk.ID)
	if mid.State == task.Failed || mid.Mark != "" {
		t.Fatalf("after the stop: %s mark %q %+v", mid.State, mid.Mark, mid.Error)
	}
	if r.state(0, "shop") != "kingresd/"+importMarker(priv) || r.state(1, "shop") != "kingresd/"+importMarker(priv) || r.setting() != 1 {
		t.Fatalf("before the restart: %q %q settings %d", r.state(0, "shop"), r.state(1, "shop"), r.setting())
	}
	// the second run: the recheck fails
	r.recheck = func(ctx context.Context, revert func() string) error {
		return errors.New("a failover changed the primary" + revert())
	}
	_, done := r.restart(tk.ID)
	if done.State != task.Failed {
		t.Fatalf("after the restart: %s", done.State)
	}
	if r.setting() != 0 {
		t.Error("the settings row of the failed import is left")
	}
	for i := 0; i < 2; i++ {
		if s, q := r.state(i, "shop"), r.state(i, priv); s != "missing" || q != "missing" {
			t.Errorf("server %d: final %q private %q", i+1, s, q)
		}
	}
}

// killOnMark stops the run (the process is gone) when the step asks the store for the mark.
type killOnMark struct {
	*task.MemStore
	kill context.CancelFunc
}

func (s *killOnMark) SetMark(ctx context.Context, id int64, l task.Lease, mark string) error {
	s.kill()
	return ctx.Err()
}

// L1 of the fifth review: a database at the final name that carries the marker of the task but is owned by somebody else is not a resume; the import fails with
// "exists" and the hook leaves that database alone.
func TestImportForgedMarkerIsNotAResume(t *testing.T) {
	r := newImportRig(t)
	e1, p := r.engine("A", r.mem, nil)
	tk, priv := r.submit(e1, p)
	pgExecAll(t, r.c[0], "create database shop owner imp", "comment on database shop is '"+importMarker(priv)+"'")
	if err := e1.RunNow(context.Background(), tk.ID, nil); err != nil {
		t.Fatal(err)
	}
	got := r.get(e1, tk.ID)
	if got.State != task.Failed || !strings.Contains(got.Error.Message, "exists") {
		t.Fatalf("the task: %s %+v", got.State, got.Error)
	}
	if s := r.state(0, "shop"); s != "imp/"+importMarker(priv) {
		t.Errorf("the forged database was changed: %q", s)
	}
	if s := r.state(1, "shop"); s != "missing" {
		t.Errorf("the peer renamed: %q", s)
	}
	if r.state(0, priv) != "missing" || r.state(1, priv) != "missing" {
		t.Errorf("the private databases are left: %q %q", r.state(0, priv), r.state(1, priv))
	}
}

// The commit mark of the task is in the store before the first server opens: it is read in the opening of the first server.
func TestImportCommitMarkIsWrittenBeforeTheFirstOpening(t *testing.T) {
	r := newImportRig(t)
	e1, p := r.engine("A", r.mem, nil)
	tk, priv := r.submit(e1, p)
	var seen string
	r.fan.before = func(op importRenameOp) error {
		if op.Open && seen == "" {
			cur, _ := r.mem.Get(context.Background(), tk.ID)
			seen = cur.Mark
			// the first server is opened before the peer is asked
			if r.state(0, "shop") != "imp/"+importMarker(priv) {
				t.Errorf("server 1 at the first opening of the peer: %q", r.state(0, "shop"))
			}
		}
		return nil
	}
	if err := e1.RunNow(context.Background(), tk.ID, nil); err != nil {
		t.Fatal(err)
	}
	if seen != importCommitMark(priv) {
		t.Errorf("the mark when the peer was opened: %q", seen)
	}
}
