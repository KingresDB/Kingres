// SPDX-License-Identifier: AGPL-3.0-only

package rebalance

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/fence"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/sysdb"
)

// Tests of the closing review of phase 4 (P4D-02 and P4D-08): the arms of the ST-02 fix and the last look before the cancel.

// callStub makes the extension's side of a move on the plain database: a record table, and a move_bucket that takes
// seconds and says when it ended.
func callStub(t *testing.T, e *guardEnv, seconds int) *pgx.Conn {
	t.Helper()
	ctx := context.Background()
	stub := e.in.Connect(t, "postgres")
	for _, q := range []string{
		`CREATE SCHEMA kingres`,
		`CREATE TABLE kingres.bucket_move (move_id bigserial PRIMARY KEY, colocation_id int, bucket int, phase text, detail text, started_at timestamptz DEFAULT now())`,
		`CREATE TABLE kingres.call_ended (n int)`,
		fmt.Sprintf(`CREATE FUNCTION kingres.move_bucket(int, int, int) RETURNS bigint LANGUAGE plpgsql AS $$
			BEGIN PERFORM pg_sleep(%d); INSERT INTO kingres.call_ended VALUES (1); RETURN 1; END $$`, seconds),
	} {
		if _, err := stub.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	return stub
}

func callEnded(t *testing.T, stub *pgx.Conn) int {
	t.Helper()
	var n int
	if err := stub.QueryRow(context.Background(), `SELECT count(*) FROM kingres.call_ended`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// denyConns refuses new sessions on a database other than the system database once deny is set: an observation
// session that cannot be made again.
type denyConns struct {
	*recordingConns
	deny atomic.Bool
}

func (d *denyConns) ServiceConn(ctx context.Context, db string) (*pgx.Conn, error) {
	if db != sysdb.Database && d.deny.Load() {
		return nil, errors.New("no session can be made")
	}
	return d.recordingConns.ServiceConn(ctx, db)
}

func terminateObservation(t *testing.T, e *guardEnv, conns *recordingConns) {
	t.Helper()
	conns.mu.Lock()
	pid := conns.conns[1].PgConn().PID() // the observation session
	conns.mu.Unlock()
	if _, err := e.sys.Exec(context.Background(), `SELECT pg_terminate_backend($1)`, int32(pid)); err != nil {
		t.Error(err)
	}
}

// P4D-02, second arm of ST-02: a dead observation session is made again at once, so the record that the extension wrote is seen
// at the next poll and the guard is let go while the call still runs. Without the new session the loop sees nothing until the
// hold ends (30 s here), the guard stays, and no other server could take the task for as long as the move runs.
func TestADeadObservationSessionIsReopenedAndTheRecordIsSeen(t *testing.T) {
	e, lease := newGuardEnv(t)
	ctx := context.Background()
	stub := callStub(t, e, 4)
	conns := &recordingConns{guardEnv: e}
	b := &PG{Conns: conns, GuardHold: 30 * time.Second}
	probe := make(chan int, 1)
	b.afterGuard = func(context.Context) {
		go func() {
			time.Sleep(300 * time.Millisecond)
			terminateObservation(t, e, conns)
			time.Sleep(300 * time.Millisecond)
			if _, err := stub.Exec(ctx, `INSERT INTO kingres.bucket_move (colocation_id, bucket, phase) VALUES (0, 1, 'switching')`); err != nil {
				t.Error(err)
			}
			time.Sleep(1500 * time.Millisecond)
			var held int
			if err := stub.QueryRow(ctx, `SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND granted`).Scan(&held); err != nil {
				t.Error(err)
			}
			probe <- held
		}()
	}
	err := b.RunMove(fence.With(ctx, fence.Token{Task: e.id, Epoch: lease.Epoch}), Move{Database: "shop", Colocation: 0, Bucket: 1, To: 2}, 0)
	if err != nil {
		t.Fatalf("RunMove: %v", err)
	}
	if held := <-probe; held != 0 {
		t.Fatalf("the guard was still held %d s after the extension recorded the move: the dead observation session was not made again", 2)
	}
	if n := callEnded(t, stub); n != 1 {
		t.Fatalf("the call did not run to its end (%d)", n)
	}
}

// P4D-02, first arm and the bound: while no look at the call can be had (the observation session is dead and none can be made),
// "running" is unknown and the call is not cancelled, neither when the hold ends (an error of the running-check is not "not
// running") nor when MaxCallWait is reached (the bound applies to a call that was looked at). The call runs to its end.
func TestACallThatCannotBeObservedIsNotCancelled(t *testing.T) {
	e, lease := newGuardEnv(t)
	ctx := context.Background()
	stub := callStub(t, e, 4)
	conns := &denyConns{recordingConns: &recordingConns{guardEnv: e}}
	b := &PG{Conns: conns, GuardHold: time.Second, MaxCallWait: 2 * time.Second}
	b.afterGuard = func(context.Context) {
		go func() {
			time.Sleep(300 * time.Millisecond)
			conns.deny.Store(true)
			terminateObservation(t, e, conns.recordingConns)
		}()
	}
	err := b.RunMove(fence.With(ctx, fence.Token{Task: e.id, Epoch: lease.Epoch}), Move{Database: "shop", Colocation: 0, Bucket: 1, To: 2}, 0)
	var ns *NotStartedError
	if errors.As(err, &ns) {
		t.Fatalf("a call that could not be observed was withdrawn: %v", err)
	}
	if err != nil {
		t.Fatalf("RunMove: %v", err)
	}
	if n := callEnded(t, stub); n != 1 {
		t.Fatalf("the call did not run to its end (%d): it was cancelled", n)
	}
}

// P4D-08: the run ends in the same moment in which the extension records the move. The last look before the cancel sees the
// record: the call is not cancelled and the move runs to its end.
func TestARecordWrittenAsTheRunEndsIsNotCancelled(t *testing.T) {
	e, lease := newGuardEnv(t)
	stub := callStub(t, e, 3)
	b := &PG{Conns: e, GuardHold: 30 * time.Second}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b.afterGuard = func(context.Context) {
		go func() {
			time.Sleep(700 * time.Millisecond)
			if _, err := stub.Exec(context.Background(), `INSERT INTO kingres.bucket_move (colocation_id, bucket, phase) VALUES (0, 1, 'copying')`); err != nil {
				t.Error(err)
			}
			cancel()
		}()
	}
	err := b.RunMove(fence.With(ctx, fence.Token{Task: e.id, Epoch: lease.Epoch}), Move{Database: "shop", Colocation: 0, Bucket: 1, To: 2}, 0)
	if err != nil {
		t.Fatalf("RunMove: %v (the call was cancelled although the extension had recorded the move)", err)
	}
	if n := callEnded(t, stub); n != 1 {
		t.Fatalf("the call did not run to its end (%d): it was cancelled", n)
	}
}
