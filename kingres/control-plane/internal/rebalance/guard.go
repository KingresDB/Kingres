// SPDX-License-Identifier: AGPL-3.0-only

package rebalance

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/fence"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/sysdb"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/task"
)

// The start of a move is guarded (RB-03). Proving the lease in a transaction that ends before
// move_bucket is called proves nothing about the call, and the extension does not take the fencing
// epoch. So the proof is kept alive across the call: a session on the system database that has
// taken the shared advisory lock of the task (task.GuardKey), not in a transaction, and then checked holder, epoch, state and
// expiry. While it is open
//   - no other server can claim the task (the claim tries the exclusive lock and skips the task),
//     so the new holder cannot have settled the move as "not recorded" while the old call is on its way;
//   - the holder's own renewals and records still work, and so does everything else on the row: an
//     advisory lock blocks no statement on the table (a lock on the row would have kept a schema change
//     of the table and a delete of the row waiting, RBR-04).
//
// The guard is held only until the extension has recorded the move (the row in kingres.bucket_move),
// the call has returned, or the call is withdrawn: while the call waits for a lock it is kept alive with a
// statement every few seconds (RunMove), and a frozen holder, which does not, loses it to the server:
// the session carries idle_session_timeout, so the task cannot be held for the minutes a
// holder is frozen. A guard that is lost that way is a void proof, not a lost lease: RunMove proves the
// lease again.
//
// What remains, and why it is harmless: see "Calls to nodes" in the README of the control plane.

// guardHold is how long the call may be unrecorded before the loop asks what it is doing (see RunMove).
const guardHold = 30 * time.Second

// guardIdleTimeout ends the guard's session on the server when the holder is frozen while it holds it.
const guardIdleTimeout = 45 * time.Second

type startGuard struct {
	conn     *pgx.Conn
	released bool
}

// acquireGuard proves the lease of the run in ctx and keeps the proof (see above). ErrLeaseLost when
// the run no longer holds the task. The proof is a shared advisory lock of the session, not a transaction: a
// transaction would keep a lock on the task table (the check reads it) and a schema change of the table would wait for it.
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
	// The server ends the session of a holder that froze with the guard (value in ms).
	if _, err := c.Exec(ctx, `SELECT set_config('idle_session_timeout', $1, false)`, "45000"); err != nil {
		g.release()
		return nil, err
	}
	// The lock first, then the check: a claim that comes later skips the task, a claim that came earlier is seen by the check.
	if _, err := c.Exec(ctx, `SELECT pg_advisory_lock_shared($1)`, task.GuardKey(taskID)); err != nil {
		g.release()
		return nil, err
	}
	var one int
	err = c.QueryRow(ctx, `SELECT 1 FROM kingres_cp.task WHERE id = $1 AND lease_epoch = $2 AND state = 'running'
		AND lease_expires_at > clock_timestamp()`, taskID, tok.Epoch).Scan(&one)
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
// call must not be made on its strength; RunMove proves the lease again.
func (g *startGuard) alive(ctx context.Context) error {
	var one int
	if err := g.conn.QueryRow(ctx, `SELECT 1`).Scan(&one); err != nil {
		return ErrLeaseLost
	}
	return nil
}

func (g *startGuard) close() { _ = g.conn.Close(context.Background()) }

// release closes the session, which releases the lock.
func (g *startGuard) release() {
	if g == nil || g.released {
		return
	}
	g.released = true
	g.close()
}
