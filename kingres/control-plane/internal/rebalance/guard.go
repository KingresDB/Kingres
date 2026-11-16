// SPDX-License-Identifier: AGPL-3.0-only

package rebalance

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/fence"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/sysdb"
)

// The start of a move is guarded (RB-03). Proving the lease in a transaction that ends before
// move_bucket is called proves nothing about the call, and the extension does not take the fencing
// epoch. So the proof is kept alive across the call: a transaction on the system database that has
// locked the task row FOR KEY SHARE after checking holder, epoch, state and expiry. While it is open
//   - no other server can claim the task (the claim takes the row FOR UPDATE and skips a locked row),
//     so the new holder cannot have settled the move as "not recorded" while the old call is on its way;
//   - the holder's own renewals and records still work (they update non-key columns, which does
//     not conflict with KEY SHARE).
//
// The guard is held only until the extension has recorded the move (the row in kingres.bucket_move),
// the call has returned, or guardHold has passed: not for the minutes the copy takes, because a
// frozen holder would keep the lock until the server drops its session and the task could not
// fail over meanwhile. The session therefore also carries idle_in_transaction_session_timeout.
//
// What remains, and why it is harmless: see "Calls to nodes" in the README of the control plane.

// guardHold bounds how long the guard is kept waiting for the move's record.
const guardHold = 30 * time.Second

// guardIdleTimeout ends the guard's session on the server when the holder is frozen while it holds it.
const guardIdleTimeout = 45 * time.Second

type startGuard struct {
	conn     *pgx.Conn
	tx       pgx.Tx
	released bool
}

// acquireGuard proves the lease of the run in ctx and keeps the proof (see above). ErrLeaseLost when
// the run no longer holds the task.
func acquireGuard(ctx context.Context, conns Conns, taskID int64) (*startGuard, error) {
	tok, ok := fence.From(ctx)
	if !ok || tok.Task != taskID {
		return nil, errors.New("rebalance: a call that starts a move without the fence of the task run")
	}
	c, err := conns.ServiceConn(ctx, sysdb.Database)
	if err != nil {
		return nil, err
	}
	g := &startGuard{conn: c}
	tx, err := c.Begin(ctx)
	if err != nil {
		g.close()
		return nil, err
	}
	g.tx = tx
	// The server ends the session of a holder that froze with the guard (value in ms).
	if _, err := tx.Exec(ctx, `SELECT set_config('idle_in_transaction_session_timeout', $1, true)`, "45000"); err != nil {
		g.release()
		return nil, err
	}
	var one int
	err = tx.QueryRow(ctx, `SELECT 1 FROM kingres_cp.task WHERE id = $1 AND lease_epoch = $2 AND state = 'running'
		AND lease_expires_at > clock_timestamp() FOR KEY SHARE`, taskID, tok.Epoch).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		g.release()
		return nil, ErrLeaseLost
	}
	if err != nil {
		g.release()
		return nil, err
	}
	return g, nil
}

// alive checks that the guard's session still holds the lock: the server may have ended it
// (idle timeout) while this process was frozen, and then the proof is void. A void guard means the
// call must not be made.
func (g *startGuard) alive(ctx context.Context) error {
	var one int
	if err := g.tx.QueryRow(ctx, `SELECT 1`).Scan(&one); err != nil {
		return ErrLeaseLost
	}
	return nil
}

func (g *startGuard) close() { _ = g.conn.Close(context.Background()) }

// release ends the transaction and closes the session.
func (g *startGuard) release() {
	if g == nil || g.released {
		return
	}
	g.released = true
	_ = g.tx.Rollback(context.Background())
	g.close()
}
