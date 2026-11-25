// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package ops

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/peerclient"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/sysdb"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/task"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/transfer"
)

// Tests of the exits of a paused import (the cancel of a paused import, the repeat of the request, the janitor, the servers that left) on the rig of
// dbimport_commit_pg_test.go.

// pgStore is a task store on the system database (a pool: the engine uses it from several goroutines).
func (r *importRig) pgStore() *task.PGStore {
	r.t.Helper()
	if r.pg == nil {
		pool, err := pgxpool.New(context.Background(), r.in[0].DSN(sysdb.Database))
		if err != nil {
			r.t.Fatal(err)
		}
		r.t.Cleanup(pool.Close)
		r.sys, r.pg = pool, task.NewPGStore(pool)
	}
	return r.pg
}

// rigRemote asks the second server of the rig, as the internal call exec of the leader does.
type rigRemote struct{ c *pgx.Conn }

func (q rigRemote) Query(ctx context.Context, server, db, sql string) ([][]any, error) {
	rows, err := q.c.Query(ctx, sql)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out [][]any
	for rows.Next() {
		v, err := rows.Values()
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// pauseCommitted runs the task until its roll forward is stuck on a peer that refuses to open the database, and cancels it: the task is committed and paused.
func (r *importRig) pauseCommitted(e1 *task.Engine, tk task.Task, priv string) {
	r.t.Helper()
	old := importCancelPoll
	importCancelPoll = 5 * time.Millisecond
	r.t.Cleanup(func() { importCancelPoll = old })
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
			r.t.Fatal("the roll forward never retried")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := e1.Cancel(context.Background(), tk.ID); err != nil {
		r.t.Fatal(err)
	}
	select {
	case err := <-ended:
		if err != nil {
			r.t.Fatalf("the run: %v", err)
		}
	case <-time.After(30 * time.Second):
		r.t.Fatal("a cancel did not end the roll forward")
	}
	got := r.get(e1, tk.ID)
	if got.State != task.Paused || got.Mark != importCommitMark(priv) {
		r.t.Fatalf("after the cancel: %s mark %q %+v", got.State, got.Mark, got.Error)
	}
	r.fan.before = nil
}

func hasProblem(err error, code problem.Code) bool {
	var p *problem.Problem
	return errors.As(err, &p) && p.Code == code
}

func (r *importRig) serverTaskInsert() error {
	_, err := r.sys.Exec(context.Background(), `INSERT INTO kingres_cp.task (kind, state, subject_type, subject_id, created_by) VALUES ('server.add', 'pending', 'server', 'sx', 'admin')`)
	return err
}

// H1a and the exclusion: a paused committed import that cannot be resumed is ended by a cancel. Nothing is undone, the servers are not touched, the message names
// the database and where it is open and where it is not, and the exclusion of the operations of the cluster is released.
func TestImportCancelOfAPausedCommittedImportEndsItAndTouchesNothing(t *testing.T) {
	r := newImportRig(t)
	r.o[0].Remote = rigRemote{r.c[1]}
	r.kind = KindDatabaseImport
	e1, p := r.engine("A", r.pgStore(), nil)
	tk, priv := r.submit(e1, p)
	r.pauseCommitted(e1, tk, priv)
	paused := r.get(e1, tk.ID)
	if !strings.HasPrefix(paused.Detail, "Repeat the same import request") {
		t.Errorf("the pause does not start with the instruction: %q", paused.Detail)
	}
	if err := r.serverTaskInsert(); err == nil || !strings.Contains(err.Error(), "KX006") {
		t.Fatalf("a paused import must exclude server.add: %v", err)
	}
	before0, before1 := r.state(0, "shop"), r.state(1, "shop")
	calls := r.fan.calls()

	if err := e1.Cancel(context.Background(), tk.ID); err != nil {
		t.Fatalf("the cancel of the paused import: %v", err)
	}
	got := r.get(e1, tk.ID)
	if got.State != task.Failed || got.Error == nil || got.Error.Code != task.CodeCanceled || got.Mark != importCommitMark(priv) {
		t.Fatalf("after the cancel: %s %+v mark %q", got.State, got.Error, got.Mark)
	}
	msg := got.Error.Message
	for _, want := range []string{"Ended by a cancel after the commit point", "shop", "operator guide", "Open on: s1", "Not open on: s2"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the message %q lacks %q", msg, want)
		}
	}
	if !strings.HasPrefix(msg, "Ended by a cancel") {
		t.Errorf("the instruction is not first: %q", msg)
	}
	if after := r.fan.calls(); after != calls {
		t.Errorf("the cancel called the servers: %s -> %s", calls, after)
	}
	if r.state(0, "shop") != before0 || r.state(1, "shop") != before1 {
		t.Errorf("the cancel changed a database: %q %q -> %q %q", before0, before1, r.state(0, "shop"), r.state(1, "shop"))
	}
	if err := r.serverTaskInsert(); err != nil {
		t.Fatalf("the exclusion is not released after the cancel: %v", err)
	}
	if err := e1.Cancel(context.Background(), tk.ID); !errors.Is(err, task.ErrNotCancellable) {
		t.Errorf("a second cancel: %v", err)
	}
	if err := e1.Resume(context.Background(), tk.ID); err == nil {
		t.Error("an ended task can be resumed")
	}
}

// flakyGet is a store whose reads fail while the flag is set.
type flakyGet struct {
	*task.MemStore
	fail atomic.Bool
}

func (s *flakyGet) Get(ctx context.Context, id int64) (task.Task, error) {
	if s.fail.Load() {
		return task.Task{}, errors.New("the task store cannot be read")
	}
	return s.MemStore.Get(ctx, id)
}

// pauseUnreadable ends a step of the import with an error at the moment the store cannot be read: the task is paused as "may be committed" while its mark is empty.
func (r *importRig) pauseUnreadable(t *testing.T, st *flakyGet) (*task.Engine, task.Task, string) {
	t.Helper()
	r.recheck = func(ctx context.Context, revert func() string) error {
		st.fail.Store(true)
		return errors.New("the catalogs differ")
	}
	e1, p := r.engine("A", st, nil)
	tk, priv := r.submit(e1, p)
	if ran, err := e1.RunOnce(context.Background()); !ran || err != nil {
		t.Fatalf("the run: %v %v", ran, err)
	}
	st.fail.Store(false)
	r.recheck = nil
	got := r.get(e1, tk.ID)
	if got.State != task.Paused || got.Mark != "" || !strings.Contains(got.Detail, "may be committed") {
		t.Fatalf("the task is %s with the mark %q: %q", got.State, got.Mark, got.Detail)
	}
	return e1, got, priv
}

// H2: a task that was paused only because its mark could not be read has an empty mark when the store is back. A cancel ends it and the hook undoes the work as for
// any import that is not committed.
func TestImportCancelOfAPausedImportWithAnEmptyMarkUndoesIt(t *testing.T) {
	r := newImportRig(t)
	st := &flakyGet{MemStore: r.mem}
	e1, tk, priv := r.pauseUnreadable(t, st)
	if r.state(0, "shop") != "kingresd/"+importMarker(priv) {
		t.Fatalf("before the cancel the database is not at the final name, closed: %q", r.state(0, "shop"))
	}
	if err := e1.Cancel(context.Background(), tk.ID); err != nil {
		t.Fatalf("the cancel: %v", err)
	}
	got := r.get(e1, tk.ID)
	if got.State != task.Failed || got.Error == nil || got.Error.Code != task.CodeCanceled {
		t.Fatalf("after the cancel: %s %+v", got.State, got.Error)
	}
	for i := 0; i < 2; i++ {
		if s := r.state(i, "shop"); s != "missing" {
			t.Errorf("server %d: the final name was not put back: %q", i+1, s)
		}
		if s := r.state(i, priv); s != "missing" {
			t.Errorf("server %d: the private database was not dropped: %q", i+1, s)
		}
	}
}

// startRig is the rig of a test that goes through StartDatabaseImport: the task has the kind of the import, the upload record belongs to the importing role and names
// the task.
type startRig struct {
	*importRig
	rec int64
}

func newStartRig(t *testing.T) *startRig {
	t.Helper()
	r := newImportRig(t)
	r.kind = KindDatabaseImport
	dir := t.TempDir()
	r.o[0].Transfer = &TransferConfig{Dir: filepath.Join(dir, "staging"), KeyFile: filepath.Join(dir, "master.key"),
		CurrentRoles: func(context.Context, string) ([]string, error) { return []string{"kingres_admin"}, nil }}
	if _, err := r.o[0].xfer(context.Background()); err != nil {
		t.Fatal(err)
	}
	var oid int64
	if err := r.c[0].QueryRow(context.Background(), `select oid::bigint from pg_roles where rolname = 'imp'`).Scan(&oid); err != nil {
		t.Fatal(err)
	}
	id, err := r.o[0].records().Create(context.Background(), transfer.Record{Kind: transfer.KindUpload, State: transfer.StateReady, ServerID: "s1",
		Owner: transfer.Identity{Name: "imp", OID: oid}, ExpiresAt: time.Now().Add(time.Hour), Purpose: "databaseArchive"})
	if err != nil {
		t.Fatal(err)
	}
	sha := strings.Repeat("b", 64)
	if err := r.o[0].records().Apply(context.Background(), id, transfer.Update{SHA256: &sha}); err != nil {
		t.Fatal(err)
	}
	return &startRig{importRig: r, rec: id}
}

func (r *startRig) link(e *task.Engine, tk task.Task) {
	r.t.Helper()
	r.o[0].SetEngine(e)
	id := tk.ID
	if err := r.o[0].records().Apply(context.Background(), r.rec, transfer.Update{TaskID: &id}); err != nil {
		r.t.Fatal(err)
	}
}

func (r *startRig) request(db string) api.ImportDatabaseRequest {
	return api.ImportDatabaseRequest{UploadID: idText(r.rec), Database: db}
}

func (r *startRig) waitDone(e *task.Engine, id int64) task.Task {
	r.t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for {
		got := r.get(e, id)
		if got.State == task.Done || got.State == task.Failed {
			r.o[0].wg.Wait()
			return got
		}
		if time.Now().After(deadline) {
			r.t.Fatalf("the task does not end: %s %q", got.State, got.Detail)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// M1: the repeat of the request goes through StartDatabaseImport. Another role is refused, a repeat for another database is a new request (and is refused while the
// upload is used), the other fields of the repeat are not read, and the task that comes back is the one that was paused.
func TestImportRepeatOfTheRequestResumesThePausedTask(t *testing.T) {
	r := newStartRig(t)
	e1, p := r.engine("A", r.mem, nil)
	tk, priv := r.submit(e1, p)
	r.link(e1, tk)
	r.pauseCommitted(e1, tk, priv)
	ctx := context.Background()

	if _, err := r.o[0].StartDatabaseImport(ctx, "outsider", r.request("shop")); !hasProblem(err, problem.NotFound) {
		t.Fatalf("another role repeats the request: %v", err)
	}
	if got := r.get(e1, tk.ID); got.State != task.Paused {
		t.Fatalf("a repeat by another role changed the task: %s", got.State)
	}
	if _, err := r.o[0].StartDatabaseImport(ctx, "imp", r.request("other")); !hasProblem(err, problem.OperationInProgress) {
		t.Fatalf("a repeat for another database: %v", err)
	}
	if got := r.get(e1, tk.ID); got.State != task.Paused {
		t.Fatalf("a repeat for another database changed the task: %s", got.State)
	}
	// the flags of the repeat are not read: the checksum differs from the one of the upload, and keepOnFailure is set
	req := r.request("shop")
	req.ExpectedSHA256 = strings.Repeat("a", 64)
	req.KeepOnFailure = true
	got, err := r.o[0].StartDatabaseImport(ctx, "imp", req)
	if err != nil {
		t.Fatalf("the repeat of the request: %v", err)
	}
	if got.ID != tk.ID || got.Kind != KindDatabaseImport || got.SubjectType != "database" || got.SubjectID != "shop" || got.State == task.Failed || len(got.Steps) == 0 {
		t.Fatalf("the answer is not the task that was paused: %+v", got)
	}
	done := r.waitDone(e1, tk.ID)
	if done.State != task.Done {
		t.Fatalf("after the repeat: %s %+v %s", done.State, done.Error, done.Detail)
	}
	if kept, _ := inputOf[dbImportParams](done); kept.KeepOnFailure {
		t.Error("a field of the repeat changed the task")
	}
	for i := 0; i < 2; i++ {
		if s := r.state(i, "shop"); s != "imp/" {
			t.Errorf("server %d: %q", i+1, s)
		}
	}
}

// M1 and H2: while the resumed task runs, a repeat is refused with 409; an import that was paused as "may be committed" with an empty mark is resumed by the repeat and
// runs as an import that is not committed.
func TestImportRepeatWhileTheTaskRunsIsRefusedAndAnEmptyMarkResumes(t *testing.T) {
	r := newStartRig(t)
	st := &flakyGet{MemStore: r.mem}
	e1, tk, _ := r.pauseUnreadable(t, st)
	r.link(e1, tk)
	gate := make(chan struct{})
	entered := make(chan struct{}, 1)
	// the resumed task is held at the start of its step
	e2, _ := r.engine("B", st, func(s task.Step) task.Step {
		run := s.Run
		s.Run = func(ctx context.Context, env *task.Env) error {
			select {
			case entered <- struct{}{}:
			default:
			}
			select {
			case <-gate:
			case <-ctx.Done():
				return ctx.Err()
			}
			return run(ctx, env)
		}
		return s
	})
	r.o[0].SetEngine(e2)
	ctx := context.Background()
	got, err := r.o[0].StartDatabaseImport(ctx, "imp", r.request("shop"))
	if err != nil {
		t.Fatalf("the repeat for a task that was paused with an empty mark: %v", err)
	}
	if got.ID != tk.ID {
		t.Fatalf("another task: %+v", got)
	}
	select {
	case <-entered:
	case <-time.After(30 * time.Second):
		t.Fatal("the resumed task did not start")
	}
	if _, err := r.o[0].StartDatabaseImport(ctx, "imp", r.request("shop")); !hasProblem(err, problem.OperationInProgress) {
		t.Fatalf("a repeat while the task runs: %v", err)
	}
	close(gate)
	done := r.waitDone(e2, tk.ID)
	if done.State != task.Done {
		t.Fatalf("after the resume: %s %+v %s", done.State, done.Error, done.Detail)
	}
	for i := 0; i < 2; i++ {
		if s := r.state(i, "shop"); s != "imp/" {
			t.Errorf("server %d: %q", i+1, s)
		}
	}
}

// H1c: the janitor does not expire, delete or prune an upload that a task of the import still uses (running or paused). The same rows of finished tasks are handled
// as before.
func TestJanitorLeavesTheUploadOfAnActiveTask(t *testing.T) {
	r := newStartRig(t)
	ctx := context.Background()
	r.pgStore()
	var imp int64
	if err := r.c[0].QueryRow(ctx, `select oid::bigint from pg_roles where rolname = 'imp'`).Scan(&imp); err != nil {
		t.Fatal(err)
	}
	newTask := func(subject, state string) int64 {
		var id int64
		if err := r.sys.QueryRow(ctx, `INSERT INTO kingres_cp.task (kind, state, subject_type, subject_id, created_by) VALUES ('database.import', $1, 'database', $2, 'admin') RETURNING id`, state, subject).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	newRec := func(owner string, oid int64, state string, tk int64, expired bool, old bool) int64 {
		exp := time.Now().Add(time.Hour)
		if expired {
			exp = time.Now().Add(-time.Hour)
		}
		id, err := r.o[0].records().Create(ctx, transfer.Record{Kind: transfer.KindUpload, State: transfer.StateReady, ServerID: "s1", Owner: transfer.Identity{Name: owner, OID: oid},
			ExpiresAt: exp, Purpose: "databaseArchive", TaskID: &tk})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := r.sys.Exec(ctx, `UPDATE kingres_cp.data_transfer SET state = $2, updated_at = CASE WHEN $3 THEN now() - interval '8 days' ELSE updated_at END WHERE transfer_id = $1`, id, state, old); err != nil {
			t.Fatal(err)
		}
		return id
	}
	pausedExpired := newRec("imp", imp, "ready", newTask("a1", "paused"), true, false)
	doneExpired := newRec("imp", imp, "ready", newTask("a2", "done"), true, false)
	pausedGone := newRec("ghost", 999999, "ready", newTask("a3", "paused"), false, false)
	failedGone := newRec("ghost", 999999, "ready", newTask("a4", "failed"), false, false)
	pausedOld := newRec("imp", imp, "expired", newTask("a5", "paused"), true, true)
	failedOld := newRec("imp", imp, "expired", newTask("a6", "failed"), true, true)
	last := newRec("imp", imp, "ready", newTask("a7", "done"), false, false)
	_ = last
	if _, err := r.o[0].TransferSweep(ctx); err != nil {
		t.Fatalf("the sweep: %v", err)
	}
	stateOf := func(id int64) string {
		var s string
		err := r.sys.QueryRow(ctx, `SELECT state FROM kingres_cp.data_transfer WHERE transfer_id = $1`, id).Scan(&s)
		if errors.Is(err, pgx.ErrNoRows) {
			return "pruned"
		}
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	// the staging directory of the test holds no file, so a row that stays "ready" is set to "lost" by the same sweep: what matters is that it was not expired or deleted
	for _, c := range []struct {
		name string
		id   int64
		not  []string
		is   string
	}{
		{"an expired upload of a paused task", pausedExpired, []string{"expired", "deleted", "pruned"}, ""},
		{"an upload of a role that was dropped, with a paused task", pausedGone, []string{"expired", "deleted", "pruned"}, ""},
		{"an old expired row of a paused task", pausedOld, []string{"pruned", "deleted"}, "expired"},
		{"an expired upload of a finished task", doneExpired, nil, "expired"},
		{"an upload of a role that was dropped, with a finished task", failedGone, nil, "deleted"},
		{"an old expired row of a finished task", failedOld, nil, "pruned"},
	} {
		got := stateOf(c.id)
		if c.is != "" && got != c.is {
			t.Errorf("%s: %s, want %s", c.name, got, c.is)
		}
		for _, bad := range c.not {
			if got == bad {
				t.Errorf("%s: the janitor made it %s", c.name, got)
			}
		}
	}
}

// countingReader is the file of an archive that counts its closes.
type countingReader struct{ closed atomic.Int32 }

func (c *countingReader) ReadAt([]byte, int64) (int, error) { return 0, io.EOF }
func (c *countingReader) Size() int64                       { return 0 }
func (c *countingReader) Close() error                      { c.closed.Add(1); return nil }

// M2: the archive that the last step opened is closed on the path of the refusal of a dangerous importer, on the path of an error, and only once.
func TestImportRenameStepClosesTheArchiveOnTheRefusal(t *testing.T) {
	in, c, o := newGuardInstance(t)
	_ = in
	pgExecAll(t, c, "create role boss superuser")
	p := dbImportParams{Database: "shop", Owner: transfer.Identity{Name: "boss"}, Nonce: importTestNonce}
	tk := task.Task{ID: 7, Kind: KindDatabaseImport, Input: mustJSON(t, p)}
	env := &task.Env{Task: tk}
	rd := &countingReader{}
	err := o.renameWithArchive(context.Background(), env, p, false, transfer.Record{}, &archiveFile{r: rd}, nil)
	if err == nil || !strings.Contains(err.Error(), "SUPERUSER") {
		t.Fatalf("the refusal: %v", err)
	}
	if n := rd.closed.Load(); n != 1 {
		t.Errorf("the archive was closed %d times on the refusal", n)
	}
	rd2 := &countingReader{}
	if err := o.renameWithArchive(context.Background(), env, p, false, transfer.Record{}, &archiveFile{r: rd2}, errors.New("the upload is gone")); err == nil {
		t.Fatal("an error of the load was ignored")
	}
	if n := rd2.closed.Load(); n != 1 {
		t.Errorf("the archive was closed %d times on the error path", n)
	}
}

// M5: a cancel request that was recorded before the commit point (the guard refuses a cancel from the sequences on, and a request may arrive after it was read) does
// not pause the import after the commit point: the step clears it before the mark is written, so the roll forward does not find it.
func TestImportCancelRequestRecordedBeforeTheCommitDoesNotPauseTheCommittedImport(t *testing.T) {
	r := newImportRig(t)
	old := importCancelPoll
	importCancelPoll = 2 * time.Millisecond
	defer func() { importCancelPoll = old }()
	e1, p := r.engine("A", r.mem, nil)
	tk, _ := r.submit(e1, p)
	var once sync.Once
	r.recheck = func(ctx context.Context, revert func() string) error {
		var err error
		once.Do(func() { err = r.mem.RequestCancel(ctx, tk.ID, time.Now()) }) // as a request that was accepted on a read that is out of date
		return err
	}
	// the roll forward lasts long enough for the poll of the cancel request to look at least once
	r.fan.before = func(op importRenameOp) error {
		if op.Open {
			time.Sleep(100 * time.Millisecond)
		}
		return nil
	}
	if err := e1.RunNow(context.Background(), tk.ID, nil); err != nil {
		t.Fatalf("the run: %v", err)
	}
	got := r.get(e1, tk.ID)
	if got.State != task.Done || got.CancelRequested {
		t.Fatalf("the stale request paused or changed the committed import: %s cancel %v %+v %q", got.State, got.CancelRequested, got.Error, got.Detail)
	}
	for i := 0; i < 2; i++ {
		if s := r.state(i, "shop"); s != "imp/" {
			t.Errorf("server %d: %q", i+1, s)
		}
	}
}
