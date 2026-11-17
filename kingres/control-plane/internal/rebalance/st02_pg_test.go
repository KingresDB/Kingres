// SPDX-License-Identifier: AGPL-3.0-only

package rebalance

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/fence"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/sysdb"
)

// recordingConns remembers the sessions opened for the move (the first is the call's, the second the observation's).
type recordingConns struct {
	*guardEnv
	mu    sync.Mutex
	conns []*pgx.Conn
}

func (r *recordingConns) ServiceConn(ctx context.Context, db string) (*pgx.Conn, error) {
	c, err := r.guardEnv.ServiceConn(ctx, db)
	if err == nil && db != sysdb.Database {
		r.mu.Lock()
		r.conns = append(r.conns, c)
		r.mu.Unlock()
	}
	return c, err
}

// ST-02: the observation of the call runs on a session of its own. When that session dies the loop knows nothing
// about the call, and "nothing known" is never "not running": the call is not cancelled, the move that the extension
// recorded in the meantime runs to its end, and the observation goes on a new session.
func TestABrokenObservationSessionDoesNotCancelARecordedMove(t *testing.T) {
	e, lease := newGuardEnv(t)
	ctx := context.Background()
	stub := e.in.Connect(t, "postgres")
	for _, q := range []string{
		`CREATE SCHEMA kingres`,
		`CREATE TABLE kingres.bucket_move (move_id bigserial PRIMARY KEY, colocation_id int, bucket int, phase text, detail text, started_at timestamptz DEFAULT now())`,
		`CREATE TABLE kingres.call_ended (n int)`,
		`CREATE FUNCTION kingres.move_bucket(int, int, int) RETURNS bigint LANGUAGE plpgsql AS $$
			BEGIN PERFORM pg_sleep(6); INSERT INTO kingres.call_ended VALUES (1); RETURN 1; END $$`,
	} {
		if _, err := stub.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	conns := &recordingConns{guardEnv: e}
	b := &PG{Conns: conns, GuardHold: 1500 * time.Millisecond}
	b.afterGuard = func(context.Context) {
		go func() {
			time.Sleep(500 * time.Millisecond) // after the call was issued
			conns.mu.Lock()
			pid := conns.conns[1].PgConn().PID() // the observation session
			conns.mu.Unlock()
			if _, err := e.sys.Exec(ctx, `SELECT pg_terminate_backend($1)`, int32(pid)); err != nil {
				t.Error(err)
			}
			time.Sleep(500 * time.Millisecond)
			// the extension records the move now (phase switching): from here on it is the extension's
			if _, err := stub.Exec(ctx, `INSERT INTO kingres.bucket_move (colocation_id, bucket, phase) VALUES (0, 1, 'switching')`); err != nil {
				t.Error(err)
			}
		}()
	}
	err := b.RunMove(fence.With(ctx, fence.Token{Task: e.id, Epoch: lease.Epoch}), Move{Database: "shop", Colocation: 0, Bucket: 1, To: 2}, 0)
	var ns *NotStartedError
	if errors.As(err, &ns) {
		t.Fatalf("the call was withdrawn because the observation session failed: %v", err)
	}
	if err != nil {
		t.Fatalf("RunMove: %v", err)
	}
	var n int
	if err := stub.QueryRow(ctx, `SELECT count(*) FROM kingres.call_ended`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("the call did not run to its end (%d, %v): it was cancelled", n, err)
	}
}
