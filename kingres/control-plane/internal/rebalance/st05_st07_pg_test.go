// SPDX-License-Identifier: AGPL-3.0-only

package rebalance

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/fence"
)

// extStub makes the part of the extension that Recover reads in the database "postgres" of the test instance:
// the record of the moves, resume_bucket_moves() that does nothing, and internal_move_alive() on an advisory lock
// as the extension makes it (a session of the move holds the lock; anyone who finds it held knows it is alive).
func extStub(t *testing.T, e *guardEnv) *pgx.Conn {
	t.Helper()
	ctx := context.Background()
	stub := e.in.Connect(t, "postgres")
	for _, q := range []string{
		`CREATE SCHEMA IF NOT EXISTS kingres`,
		`CREATE TABLE kingres.bucket_move (move_id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY, colocation_id int, bucket int, phase text, detail text, started_at timestamptz DEFAULT clock_timestamp())`,
		`CREATE FUNCTION kingres.resume_bucket_moves() RETURNS int LANGUAGE sql AS 'SELECT 0'`,
		`CREATE FUNCTION kingres.internal_move_alive(id bigint) RETURNS bool LANGUAGE plpgsql AS $$
			BEGIN
				IF pg_try_advisory_lock(7000, id::int) THEN PERFORM pg_advisory_unlock(7000, id::int); RETURN false; END IF;
				RETURN true;
			END $$`,
	} {
		if _, err := stub.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	trustStub(t, stub)
	return stub
}

// ST-05 and P4D-03: the mark is written in the statement that makes the row, so a row never exists without it (a holder
// that takes over reads it back, and no row is matched by time except the rows of before migration 0012); a new attempt of
// the move has the mark of its own call, not the one of the attempt before.
func TestTheMarkOfAMoveIsWrittenWithItsRow(t *testing.T) {
	e, lease := newGuardEnv(t)
	ctx := fence.With(context.Background(), fence.Token{Task: e.id, Epoch: lease.Epoch})
	rows := &PGRows{Conns: e, TaskID: e.id}
	if _, err := rows.CreateRun(ctx, Options{}, nil); err != nil {
		t.Fatal(err)
	}
	m := Move{Database: "shop", Bucket: 3, From: 1, To: 2}
	got := func() MoveRow {
		all, err := rows.Moves(ctx)
		if err != nil || len(all) != 1 {
			t.Fatalf("%v %d", err, len(all))
		}
		return all[0]
	}
	seq, err := rows.StartMove(ctx, m, "s1", "s2", 1, 0) // 0 is a mark: the extension had no move yet
	if err != nil {
		t.Fatal(err)
	}
	if r := got(); !r.Marked || r.After != 0 {
		t.Fatalf("mark 0 not read back at once: %+v", r)
	}
	if err := rows.FinishMove(ctx, seq, MoveFailed, "x", "", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := rows.StartMove(ctx, m, "s1", "s2", 2, 41); err != nil {
		t.Fatal(err)
	}
	if r := got(); !r.Marked || r.After != 41 {
		t.Fatalf("the next attempt does not carry its own mark 41: %+v", r)
	}
}

// ST-05: a row left by an earlier holder is matched to the record of its call by the mark, whatever the clocks of
// the two servers say; a record of an earlier attempt (id at or below the mark) is never taken for it. A row
// made before the mark was kept is matched by time with a tolerance.
func TestRecoverMatchesTheRecordByTheMarkNotByTwoClocks(t *testing.T) {
	e, _ := newGuardEnv(t)
	ctx := context.Background()
	stub := extStub(t, e)
	b := &PG{Conns: e, RecoverWait: 3 * time.Second}
	row := MoveRow{Move: Move{Database: "shop", Colocation: 0, Bucket: 1}, State: MoveRunning, StartedAt: time.Now()}

	// the extension's clock is behind the loop's by an hour: the time path cannot find it, the mark does
	if _, err := stub.Exec(ctx, `INSERT INTO kingres.bucket_move (colocation_id, bucket, phase, started_at) VALUES (0, 1, 'done', now() - interval '1 hour')`); err != nil {
		t.Fatal(err)
	}
	marked := row
	marked.After, marked.Marked = 0, true
	if state, _, err := b.Recover(ctx, marked); err != nil || state != MoveDone {
		t.Fatalf("marked row: %q %v, want done", state, err)
	}
	// the record of an earlier attempt (id 1) is not the call of this attempt (mark 1)
	marked.After = 1
	if state, text, err := b.Recover(ctx, marked); err != nil || state != MoveFailed || !strings.Contains(text, "had not been recorded") {
		t.Fatalf("an earlier attempt's record was taken: %q %q %v", state, text, err)
	}
	// without a mark: a record a few seconds before the row (clock skew) is found, one an hour before is not
	if _, err := stub.Exec(ctx, `INSERT INTO kingres.bucket_move (colocation_id, bucket, phase, started_at) VALUES (0, 2, 'done', now() - interval '3 seconds')`); err != nil {
		t.Fatal(err)
	}
	unmarked := row
	unmarked.Move.Bucket = 2
	if state, _, err := b.Recover(ctx, unmarked); err != nil || state != MoveDone {
		t.Fatalf("unmarked row, skew 3 s: %q %v, want done", state, err)
	}
	unmarked.Move.Bucket = 1
	if state, _, err := b.Recover(ctx, unmarked); err != nil || state != MoveFailed {
		t.Fatalf("unmarked row, record an hour before: %q %v, want failed", state, err)
	}
}

// ST-07: a move whose session is alive is waited for as long as it lives, past RecoverWait; one that is not alive
// and not settled is given up after it.
func TestRecoverWaitsForALiveMoveBeyondTheBound(t *testing.T) {
	e, _ := newGuardEnv(t)
	ctx := context.Background()
	stub := extStub(t, e)
	if _, err := stub.Exec(ctx, `INSERT INTO kingres.bucket_move (colocation_id, bucket, phase) VALUES (0, 1, 'copying')`); err != nil {
		t.Fatal(err)
	}
	b := &PG{Conns: e, RecoverWait: time.Second}
	row := MoveRow{Move: Move{Database: "shop", Colocation: 0, Bucket: 1}, State: MoveRunning, ExtMoveID: 1}

	// not alive: given up after the bound
	start := time.Now()
	if _, _, err := b.Recover(ctx, row); err == nil || !strings.Contains(err.Error(), "still not settled") {
		t.Fatalf("a dead unsettled move: %v", err)
	}
	if d := time.Since(start); d > 6*time.Second {
		t.Fatalf("gave up after %s", d)
	}

	// alive for 3.5 s (3.5 times the bound), then done: the answer is done
	owner := e.in.Connect(t, "postgres")
	if _, err := owner.Exec(ctx, `SELECT pg_advisory_lock(7000, 1)`); err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(3500 * time.Millisecond)
		_, _ = stub.Exec(ctx, `UPDATE kingres.bucket_move SET phase = 'done' WHERE move_id = 1`)
		_, _ = owner.Exec(ctx, `SELECT pg_advisory_unlock(7000, 1)`)
	}()
	state, _, err := b.Recover(ctx, row)
	if err != nil || state != MoveDone {
		t.Fatalf("a live long move: %q %v, want done after the wait", state, err)
	}
}
