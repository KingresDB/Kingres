// SPDX-License-Identifier: AGPL-3.0-only

package rebalance

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/fence"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/pgtest"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/sysdb"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/task"
)

// guardEnv is a real system database with one claimed task, and a Conns that counts the connections
// opened to databases other than the system database (the ones a move would use).
type guardEnv struct {
	in    *pgtest.Instance
	sys   *pgx.Conn
	st    *task.PGStore
	id    int64
	moves atomic.Int32
}

func (e *guardEnv) ServiceConn(ctx context.Context, db string) (*pgx.Conn, error) {
	name := db
	if db != sysdb.Database {
		e.moves.Add(1)
		name = "postgres" // no extension there: a call that is made fails at once
	}
	cfg, err := pgx.ParseConfig(e.in.DSN(name))
	if err != nil {
		return nil, err
	}
	return pgx.ConnectConfig(ctx, cfg)
}

func newGuardEnv(t *testing.T) (*guardEnv, task.Lease) {
	t.Helper()
	in := pgtest.Start(t, nil)
	ctx := context.Background()
	if _, err := in.Connect(t, "postgres").Exec(ctx, "CREATE DATABASE kingres_system"); err != nil {
		t.Fatal(err)
	}
	e := &guardEnv{in: in, sys: in.Connect(t, sysdb.Database)}
	if _, err := sysdb.Migrate(ctx, e.sys); err != nil {
		t.Fatal(err)
	}
	e.st = task.NewPGStore(e.sys)
	tk, err := e.st.Create(ctx, task.Task{Kind: Kind, Input: []byte("{}"), SubjectType: SubjectType, SubjectID: SubjectID,
		Steps: []task.StepState{{Name: "x", State: task.StepPending}}, CreatedAt: time.Now(), UpdatedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	e.id = tk.ID
	c, err := e.st.Claim(ctx, "A", time.Hour)
	if err != nil || c == nil {
		t.Fatalf("claim: %v %v", c, err)
	}
	return e, task.Lease{Holder: "A", Epoch: c.Epoch}
}

func (e *guardEnv) expire(t *testing.T) {
	t.Helper()
	if _, err := e.sys.Exec(context.Background(), `UPDATE kingres_cp.task SET lease_expires_at = clock_timestamp() - interval '1 second' WHERE id = $1`, e.id); err != nil {
		t.Fatal(err)
	}
}

// RB-03: a run that lost its lease does not call move_bucket: the call is not even prepared.
func TestRunMoveOfASupersededRunCallsNothing(t *testing.T) {
	e, lease := newGuardEnv(t)
	ctx := context.Background()
	e.expire(t)
	if b, err := e.st.Claim(ctx, "B", time.Hour); err != nil || b == nil || b.Epoch != 2 {
		t.Fatalf("B: %v %v", b, err)
	}
	b := &PG{Conns: e}
	err := b.RunMove(fence.With(ctx, fence.Token{Task: e.id, Epoch: lease.Epoch}), Move{Database: "shop", Bucket: 1, To: 2}, 0)
	if !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("RunMove: %v, want ErrLeaseLost", err)
	}
	if n := e.moves.Load(); n != 0 {
		t.Errorf("%d connections were opened for the move of a run that lost its lease", n)
	}
}

// RB-03: while the guard is held (the call is being issued) nobody can take the task over, even
// when its lease ran out; once the call returned the task can be claimed.
func TestGuardKeepsTheTaskFromBeingTakenOverDuringTheCall(t *testing.T) {
	e, lease := newGuardEnv(t)
	ctx := context.Background()
	var claimedDuring *task.Task
	var claimErr error
	b := &PG{Conns: e, afterGuard: func(context.Context) {
		e.expire(t)
		claimedDuring, claimErr = e.st.Claim(ctx, "B", time.Hour)
	}}
	err := b.RunMove(fence.With(ctx, fence.Token{Task: e.id, Epoch: lease.Epoch}), Move{Database: "shop", Bucket: 1, To: 2}, 0)
	if err == nil || errors.Is(err, ErrLeaseLost) {
		t.Fatalf("RunMove: %v, want the error of the missing function", err)
	}
	if claimErr != nil || claimedDuring != nil {
		t.Errorf("B claimed the task while the guard was held: %v %v", claimedDuring, claimErr)
	}
	if n := e.moves.Load(); n != 2 {
		t.Errorf("connections for the move: %d, want 2 (the call and the watcher)", n)
	}
	if c, err := e.st.Claim(ctx, "B", time.Hour); err != nil || c == nil || c.Epoch != 2 {
		t.Errorf("after the call B must be able to claim: %v %v", c, err)
	}
}

// terminateGuard ends the session of the guard on the server, as an idle timeout or a restart of the
// system database does.
func terminateGuard(t *testing.T, e *guardEnv) {
	t.Helper()
	if _, err := e.sys.Exec(context.Background(), `SELECT pg_terminate_backend(pid) FROM pg_locks
		WHERE locktype = 'advisory' AND granted AND pid <> pg_backend_pid()`); err != nil {
		t.Error(err)
	}
	time.Sleep(200 * time.Millisecond)
}

// RB-03: a guard whose session the server ended while the lease is gone (the task was claimed by
// another holder) is void, and the call is not made.
func TestVoidGuardOfALostLeaseMakesNoCall(t *testing.T) {
	e, lease := newGuardEnv(t)
	ctx := context.Background()
	b := &PG{Conns: e, afterGuard: func(context.Context) {
		terminateGuard(t, e)
		e.expire(t)
		if c, err := e.st.Claim(ctx, "B", time.Hour); err != nil || c == nil {
			t.Errorf("B could not claim the task: %v %v", c, err)
		}
	}}
	err := b.RunMove(fence.With(ctx, fence.Token{Task: e.id, Epoch: lease.Epoch}), Move{Database: "shop", Bucket: 1, To: 2}, 0)
	if !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("RunMove: %v, want ErrLeaseLost", err)
	}
}

// RBR-04: a guard whose session ended for another reason than a lost lease (a restart of the system
// database, the idle timeout) is not a lost lease: the lease is proved again and the call is made.
func TestVoidGuardOfAValidLeaseIsTakenAgain(t *testing.T) {
	e, lease := newGuardEnv(t)
	ctx := context.Background()
	b := &PG{Conns: e, afterGuard: func(context.Context) { terminateGuard(t, e) }}
	err := b.RunMove(fence.With(ctx, fence.Token{Task: e.id, Epoch: lease.Epoch}), Move{Database: "shop", Bucket: 1, To: 2}, 0)
	if err == nil || errors.Is(err, ErrLeaseLost) {
		t.Fatalf("RunMove: %v, want the error of the missing function (the call was made)", err)
	}
	var ns *NotStartedError
	if errors.As(err, &ns) {
		t.Fatalf("RunMove: %v, the call was reported as not started", err)
	}
}

// The holder's own renewals and records are not blocked by its guard.
func TestGuardDoesNotBlockTheHoldersOwnWrites(t *testing.T) {
	e, lease := newGuardEnv(t)
	ctx := context.Background()
	g, err := acquireGuard(fence.With(ctx, fence.Token{Task: e.id, Epoch: lease.Epoch}), e, e.id)
	if err != nil {
		t.Fatal(err)
	}
	defer g.release()
	wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := e.st.Renew(wctx, e.id, lease, time.Hour); err != nil {
		t.Errorf("renew while the guard is held: %v", err)
	}
	if err := e.st.SetDetail(wctx, e.id, lease, "x"); err != nil {
		t.Errorf("record while the guard is held: %v", err)
	}
}

// RBR-04: the guard keeps the task from being claimed and blocks nothing else on the table or on the row:
// a schema change of the task table, a delete of the row and a lock of the row all go through while it is
// held (the lock on the row that the guard used to take kept them waiting for as long as the call waited).
func TestGuardBlocksOnlyTheClaim(t *testing.T) {
	e, lease := newGuardEnv(t)
	ctx := context.Background()
	g, err := acquireGuard(fence.With(ctx, fence.Token{Task: e.id, Epoch: lease.Epoch}), e, e.id)
	if err != nil {
		t.Fatal(err)
	}
	defer g.release()
	e.expire(t)
	if c, err := e.st.Claim(ctx, "B", time.Hour); err != nil || c != nil {
		t.Fatalf("B claimed a task whose guard is held: %v %v", c, err)
	}
	other := e.in.Connect(t, sysdb.Database)
	for name, sql := range map[string]string{
		"SELECT FOR UPDATE": `SELECT 1 FROM kingres_cp.task WHERE id = $1 FOR UPDATE`,
		"ALTER TABLE":       `ALTER TABLE kingres_cp.task ADD COLUMN guard_probe int`,
		"DELETE":            `DELETE FROM kingres_cp.task WHERE id = $1 AND false`,
	} {
		if _, err := other.Exec(ctx, `SET lock_timeout = '2s'`); err != nil {
			t.Fatal(err)
		}
		args := []any{e.id}
		if name == "ALTER TABLE" {
			args = nil
		}
		if _, err := other.Exec(ctx, sql, args...); err != nil {
			t.Errorf("%s was blocked by the guard: %v", name, err)
		}
	}
	g.release()
	if c, err := e.st.Claim(ctx, "B", time.Hour); err != nil || c == nil {
		t.Errorf("after the guard B must be able to claim: %v %v", c, err)
	}
}
