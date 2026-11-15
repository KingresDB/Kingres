// SPDX-License-Identifier: PostgreSQL

package membership

import (
	"context"
	"errors"
	"fmt"

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
