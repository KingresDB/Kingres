// SPDX-License-Identifier: PostgreSQL

package membership

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/audit"
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
	dbs, err := m.shardedDatabases(ctx)
	if err != nil {
		return false, "", err
	}
	for _, db := range dbs {
		c, err := m.Svc.ServiceConn(ctx, db)
		if err != nil {
			return false, "", err
		}
		var n int
		err = c.QueryRow(ctx, `SELECT count(*) FROM kingres.bucket_map b
			WHERE b.group_id IN (SELECT n.group_id FROM kingres.node n WHERE n.host = $1)`, server).Scan(&n)
		_ = c.Close(context.WithoutCancel(ctx))
		if err != nil {
			return false, "", err
		}
		if n > 0 {
			return true, db, nil
		}
	}
	return false, "", nil
}

// RemoveServer is DELETE /servers/{id} on the leader (design D5): only a server
// that owns no buckets in any database, and not the server of the map group.
func (m *Manager) RemoveServer(ctx context.Context, actor, id string) (task.Task, error) {
	row, err := m.getServer(ctx, id)
	if errors.Is(err, errNoServer) || (err == nil && row.Status == StatusPending) {
		return task.Task{}, problem.New(problem.NotFound, fmt.Sprintf("Server %q does not exist.", id))
	}
	if err != nil {
		return task.Task{}, err
	}
	if id == m.Self {
		return task.Task{}, conflictProblem("This server holds the primary of the map group (the manager leader) and cannot be removed.")
	}
	if l, lerr := m.Leaders.Get(ctx); lerr == nil && l == id {
		return task.Task{}, conflictProblem("This server holds the primary of the map group and cannot be removed.")
	}
	owns, db, err := m.ownsBuckets(ctx, id)
	if err != nil {
		m.log().Warn("checking the buckets of a server failed", "server", id, "error", err)
		return task.Task{}, problem.New(problem.ServiceUnavailable, "The bucket map cannot be read now.").WithRetryAfter(5)
	}
	if owns {
		return task.Task{}, conflictProblem(fmt.Sprintf("Server %q still owns buckets (for example in database %q). Rebalance them away from it first.", id, db))
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
		{Name: "Unregister the server in every database", Run: m.stepRemoveUnregister},
		{Name: "Remove the server from the managed rules of the others", Run: m.stepRemoveTrust},
		{Name: "Stop and delete the instance, reset the server", Irreversible: true, Run: m.stepRemoveReset},
		{Name: "Revoke the certificate and distribute the revocation list", Run: m.stepRemoveRevoke},
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
	for _, db := range dbs {
		err := m.waitUnreachable(ctx, env, in.ServerID, "unregistering it in "+db, func() error {
			return m.unregisterNode(ctx, db, in.ServerID)
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func (m *Manager) unregisterNode(ctx context.Context, db, host string) error {
	c, err := m.Svc.ServiceConn(ctx, db)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close(context.WithoutCancel(ctx)) }()
	rows, err := c.Query(ctx, `SELECT node_id FROM kingres.node WHERE host = $1 ORDER BY node_id DESC`, host)
	if err != nil {
		return err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[int32])
	if err != nil {
		return err
	}
	for _, id := range ids {
		if _, err := c.Exec(ctx, `SELECT kingres.remove_node($1)`, id); err != nil {
			return fmt.Errorf("unregistering node %d in database %s: %w", id, db, cleanPG(err))
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
		return m.do(ctx, in.ServerID, "POST", "/node/reset", wire.Empty{}, nil)
	})
	var ae *peerclient.APIError
	if errors.As(err, &ae) && ae.Status == 401 {
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
	if row.Serial != "" {
		if err := ca.Revoke(row.Serial, time.Now()); err != nil {
			return err
		}
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
	return []task.Definition{m.addDefinition(), m.removeDefinition()}
}
