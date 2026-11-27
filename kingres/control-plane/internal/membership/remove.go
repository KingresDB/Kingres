// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package membership

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/audit"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/extgate"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/extguard"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/peerclient"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/task"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/wire"
)

type removeInput struct {
	ServerID string `json:"serverId"`
}

// ownsBuckets reports whether a server holds buckets (or a copy of them) in
// any sharded database, naming the first database that does.
func (m *Manager) ownsBuckets(ctx context.Context, server string) (bool, string, error) {
	h, err := m.holds(ctx, server)
	if err != nil || len(h) == 0 {
		return false, "", err
	}
	return true, h[0].DB, nil
}

// RemoveServer is DELETE /servers/{id} on the leader (design D5): only a server
// that owns no buckets in any database, and does not host the map group. The
// refusals and their order are decideRemoval's, the contract's.
func (m *Manager) RemoveServer(ctx context.Context, actor, id string) (task.Task, error) {
	if _, err := m.ca(); err != nil {
		return task.Task{}, err
	}
	f, err := m.leaderFacts(ctx, id)
	if err != nil {
		m.log().Warn("reading the catalog for a removal failed", "server", id, "error", err)
		return task.Task{}, problem.New(problem.ServiceUnavailable, "The catalog cannot be read now.").WithRetryAfter(5)
	}
	if l, lerr := m.Leaders.Get(ctx); id == m.Self || (lerr == nil && l == id) {
		f.HostsMapGroup = true
	}
	if p := decideRemoval(id, false, f); p != nil {
		return task.Task{}, p
	}
	t, err := m.Ops.StartTask(ctx, KindRemove, "server", id, removeInput{ServerID: id}, actor)
	if errors.Is(err, task.ErrSubjectBusy) {
		return t, problem.New(problem.OperationInProgress, "This server is already being added or removed.")
	}
	return t, err
}

func (m *Manager) removeDefinition() task.Definition {
	return task.Definition{Kind: KindRemove, Steps: []task.Step{
		{Name: "Check that the server owns no buckets", Run: m.stepRemoveCheck},
		// A chain on a group that goes with the server cannot vanish silently: its publication ends with an event (design 5.5).
		{Name: "End the publication of the ledger chains of the server", Run: m.stepRemoveLedger},
		{Name: "Remove the standbys of the server", Run: m.stepRemoveStandbys},
		{Name: "Unregister the server in every database", Run: m.stepRemoveUnregister},
		{Name: "Remove the server from the managed rules of the others", Run: m.stepRemoveTrust},
		// The certificate is revoked before the step that cannot be undone (CP2-14): a task
		// that fails at the revocation (the CA key went away) then leaves a server that is
		// still there but cut off, never a server that is gone with a valid certificate.
		// The reset call does not need the server's certificate to be valid.
		{Name: "Revoke the certificate and distribute the revocation list", Run: m.stepRemoveRevoke},
		{Name: "Stop and delete the instance, reset the server", Irreversible: true, Run: m.stepRemoveReset},
		{Name: "Forget the server", Run: m.stepRemoveForget},
	}}
}

func (m *Manager) stepRemoveCheck(ctx context.Context, env *task.Env) error {
	in, err := inputOf[removeInput](env.Task)
	if err != nil {
		return err
	}
	if _, err := m.getServer(ctx, in.ServerID); errors.Is(err, errNoServer) {
		return nil // an earlier run already finished
	}
	owns, db, err := m.ownsBuckets(ctx, in.ServerID)
	if err != nil {
		return err
	}
	if owns {
		return fmt.Errorf("the server owns buckets in database %q; rebalance first", db)
	}
	return m.sys(ctx, func(c *pgx.Conn) error {
		_, err := c.Exec(ctx, `UPDATE kingres_cp.server SET status = 'removing' WHERE server_id = $1`, in.ServerID)
		return err
	})
}

func (m *Manager) stepRemoveUnregister(ctx context.Context, env *task.Env) error {
	in, err := inputOf[removeInput](env.Task)
	if err != nil {
		return err
	}
	dbs, err := m.shardedDatabases(ctx)
	if err != nil {
		return err
	}
	// The detail is also cleared when the task is being stopped, so it does not use the context of the step.
	say := func(s string) { _ = env.SetDetail(context.WithoutCancel(ctx), s) }
	for _, db := range dbs {
		err := m.waitUnreachable(ctx, env, in.ServerID, "unregistering it in "+db, func() error {
			return m.unregisterNode(ctx, db, in.ServerID, say)
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func (m *Manager) unregisterNode(ctx context.Context, db, host string, say func(string)) error {
	c, err := m.Svc.ServiceConn(ctx, db)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close(context.WithoutCancel(ctx)) }()
	return m.unregisterNodeOn(ctx, c, db, host, say)
}

// cleaningWait bounds how long the removal of a node waits for the moves of its group that are still in the cleaning phase
// after their cleanup was forced; cleaningPoll is the pause between two tries. The forced cleanup does not finish every move
// at once: it skips a move that another cleanup holds at that moment (the one at the end of a rebalance, or the background
// worker of the extension), and a move waits while a transaction of its switch is still prepared or a reader still uses its
// old partitions. Each try forces the cleanup again. The bound is checked before each new try, and a try in progress is not cut
// short (a forced cleanup can take up to a minute per move whose old partitions are still read), so the wait can end later.
var (
	cleaningWait = 2 * time.Minute
	cleaningPoll = 2 * time.Second
)

// removeCalls are the calls of the removal of one node, apart from the database, so that the wait for the moves in the cleaning
// phase can be tested without one.
type removeCalls struct {
	remove   func() error        // kingres.remove_node
	cleaning func() (int, error) // the moves of the node's group in the cleaning phase; 0 when the group still has a standby or buckets
	force    func() (int, error) // kingres.cleanup_moved_buckets(true): the number of moves it finished
	say      func(string)        // the detail of the task; "" clears it
}

// removeWaitingForCleaning removes a node. The extension refuses it with SQLSTATE 55006 while the group of the node has a standby,
// owns buckets, or takes part in a move that is not finished. When moves of the group are in the cleaning phase (and the group has
// no standby and no bucket, which the extension checks first), their cleanup is forced and the removal is tried again, until it is
// done or wait is over; while one of them is cleaning, a move of the group in an earlier phase is waited for too, as it may reach
// the cleaning phase. Any other refusal (a standby, buckets, or only moves in earlier phases) and any other error are returned at
// once. forced is the number of moves the forced cleanups finished, and ran says whether one ran at all.
func removeWaitingForCleaning(ctx context.Context, rc removeCalls, wait, poll time.Duration) (forced int, ran bool, err error) {
	deadline := time.Now().Add(wait)
	told := false
	defer func() {
		if told {
			rc.say("")
		}
	}()
	for try := 0; ; try++ {
		err = rc.remove()
		var pe *pgconn.PgError
		if err == nil || !errors.As(err, &pe) || pe.Code != "55006" {
			return forced, ran, err
		}
		n, cerr := rc.cleaning()
		if cerr != nil || n == 0 {
			return forced, ran, err
		}
		if try > 0 {
			if time.Now().After(deadline) {
				return forced, ran, err
			}
			rc.say(fmt.Sprintf("Waiting for %d bucket moves of its group in the cleaning phase to finish before the server is unregistered.", n))
			told = true
			select {
			case <-ctx.Done():
				return forced, ran, ctx.Err()
			case <-time.After(poll):
			}
		}
		f, ferr := rc.force()
		if ferr != nil {
			return forced, ran, err
		}
		forced += f
		ran = true
	}
}

// unregisterNodeOn is unregisterNode on the session c of the service role, which is open to database db (see registerNodeOn).
// say, when not nil, is told what the removal waits for.
func (m *Manager) unregisterNodeOn(ctx context.Context, c *pgx.Conn, db, host string, say func(string)) error {
	if say == nil {
		say = func(string) {}
	}
	if err := extgate.Require(ctx, c, extguard.Shard, db); err != nil {
		return err
	}
	rows, err := c.Query(ctx, `SELECT node_id FROM kingres.node WHERE host = $1 ORDER BY node_id DESC`, host)
	if err != nil {
		return err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[int32])
	if err != nil {
		return err
	}
	for _, id := range ids {
		// The server was emptied by a rebalance a moment ago: its moves are still in the cleaning phase,
		// waiting for the delay after which the old copies of the buckets are dropped, and the extension
		// refuses to remove a group that takes part in a move. The server is going away with its data, so
		// the old copies are dropped now (cleanup_moved_buckets(true)), and the removal tries again, for as
		// long as cleaningWait, while a move of the group is still cleaning (see removeWaitingForCleaning).
		// The extension cannot limit this to the groups of the server that goes (there is no argument for it): it
		// finishes every move that is cleaning, also those of buckets that moved between other servers, whose old
		// copies are then dropped before their retention delay is over. That is said in the log and the audit log
		// with the number, so that it is not silent (RB-14 c).
		forced, ran, rerr := removeWaitingForCleaning(ctx, removeCalls{
			remove: func() error {
				_, err := c.Exec(ctx, `SELECT kingres.remove_node($1)`, id)
				return err
			},
			cleaning: func() (int, error) {
				var n int
				// The refusals of kingres.remove_node in its order: a standby of the group, buckets of the group, then a move.
				err := c.QueryRow(ctx, `SELECT count(*) FROM kingres.bucket_move m JOIN kingres.node n ON n.node_id = $1
					WHERE n.role = 'primary' AND m.phase = 'cleaning' AND (m.source_group = n.group_id OR m.target_group = n.group_id)
					AND NOT EXISTS (SELECT 1 FROM kingres.node s WHERE s.group_id = n.group_id AND s.role = 'standby')
					AND NOT EXISTS (SELECT 1 FROM kingres.bucket_map b WHERE b.group_id = n.group_id)`, id).Scan(&n)
				return n, err
			},
			force: func() (int, error) {
				var n int
				err := c.QueryRow(ctx, `SELECT kingres.cleanup_moved_buckets(true)`).Scan(&n)
				return n, err
			},
			say: say,
		}, cleaningWait, cleaningPoll)
		if ran && forced > 0 {
			m.log().Warn("removing a server finished the moves in the cleaning phase of the whole cluster before their retention delay", "server", host, "database", db, "moves", forced)
			m.audit(ctx, "system", "cleanupMovedBucketsForced", host, audit.OutcomeSuccess, map[string]any{"database": db, "moves": forced})
		}
		if rerr != nil {
			var pe *pgconn.PgError
			if errors.As(rerr, &pe) {
				m.log().Warn("the extension refused to unregister a node", "server", host, "database", db, "node", id, "sqlstate", pe.Code, "message", pe.Message)
			}
			return fmt.Errorf("unregistering node %d in database %s: %w", id, db, cleanPG(rerr))
		}
	}
	return nil
}

func (m *Manager) stepRemoveTrust(ctx context.Context, env *task.Env) error {
	in, err := inputOf[removeInput](env.Task)
	if err != nil {
		return err
	}
	return m.trustEverywhere(ctx, env, "", in.ServerID)
}

func (m *Manager) stepRemoveReset(ctx context.Context, env *task.Env) error {
	in, err := inputOf[removeInput](env.Task)
	if err != nil {
		return err
	}
	// The server may already be reset by an earlier run: then its certificate
	// is gone and the call fails with the certificate refused; that is done.
	err = m.waitUnreachable(ctx, env, in.ServerID, "deleting its instance", func() error {
		return m.doUnchecked(ctx, in.ServerID, "POST", "/node/reset", wire.Empty{}, nil)
	})
	var ae *peerclient.APIError
	if errors.As(err, &ae) && ae.Status == 401 {
		return nil
	}
	// A server that left alone (a forced leave) or was reset by an earlier run no
	// longer presents a certificate of this cluster: there is nothing left to reset.
	if errors.Is(err, peerclient.ErrNotMember) {
		return nil
	}
	return err
}

func (m *Manager) stepRemoveRevoke(ctx context.Context, env *task.Env) error {
	in, err := inputOf[removeInput](env.Task)
	if err != nil {
		return err
	}
	row, err := m.getServer(ctx, in.ServerID)
	if errors.Is(err, errNoServer) {
		return nil
	}
	if err != nil {
		return err
	}
	ca, err := m.Svc.CA()
	if err != nil {
		return err
	}
	if err := m.revokeServerCertificates(ctx, ca, in.ServerID, row.Serial); err != nil {
		return err
	}
	m.Client.Forget(in.ServerID)
	return m.distributeRevocations(ctx)
}

func (m *Manager) stepRemoveForget(ctx context.Context, env *task.Env) error {
	in, err := inputOf[removeInput](env.Task)
	if err != nil {
		return err
	}
	err = m.sys(ctx, func(c *pgx.Conn) error {
		_, err := c.Exec(ctx, `DELETE FROM kingres_cp.server WHERE server_id = $1`, in.ServerID)
		return err
	})
	if err != nil {
		return err
	}
	_ = m.Peers.Delete(in.ServerID)
	if err := m.distributePeers(ctx, env, in.ServerID); err != nil {
		return err
	}
	m.audit(ctx, env.Task.CreatedBy, "removeServerDone", in.ServerID, audit.OutcomeSuccess, map[string]any{"taskId": env.Task.ID})
	return nil
}

// Definitions returns the task definitions of this package.
func (m *Manager) Definitions() []task.Definition {
	return []task.Definition{m.addDefinition(), m.removeDefinition(), m.leaveDefinition(), m.certRenewDefinition(), m.certRevokeDefinition()}
}

// stepRemoveStandbys removes the standbys that run on the server and those of the
// groups whose primary it runs, with their slots (design ha-phase5, part A). It
// comes before the server is unregistered, while the catalog still names it.
func (m *Manager) stepRemoveStandbys(ctx context.Context, env *task.Env) error {
	if m.Standbys == nil {
		return nil
	}
	in, err := inputOf[removeInput](env.Task)
	if err != nil {
		in2, err2 := inputOf[leaveInput](env.Task)
		if err2 != nil {
			return err
		}
		in.ServerID = in2.ServerID
	}
	if _, err := m.getServer(ctx, in.ServerID); errors.Is(err, errNoServer) {
		return nil // an earlier run finished
	}
	if err := m.Standbys.PurgeServer(ctx, in.ServerID); err != nil {
		return fmt.Errorf("removing the standbys of the server: %w", err)
	}
	return nil
}

// stepRemoveLedger ends the publication of the ledger chains of the groups whose primary runs on the server (design 5.5): the hook
// of the removal of a group. It does nothing when the ledger is not published or the server runs no group with a published chain.
func (m *Manager) stepRemoveLedger(ctx context.Context, env *task.Env) error {
	if m.LedgerRetire == nil {
		return task.Skip("this server does not publish the ledger")
	}
	in, err := inputOf[removeInput](env.Task)
	if err != nil {
		return err
	}
	if _, err := m.getServer(ctx, in.ServerID); errors.Is(err, errNoServer) {
		return nil // an earlier run already finished
	}
	return m.LedgerRetire(ctx, in.ServerID)
}
