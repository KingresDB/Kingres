// SPDX-License-Identifier: AGPL-3.0-only

package membership

import (
	"context"
	"encoding/json"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/internalapi"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ops"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/wire"
)

// Manager implements ops.Fanout: it reaches the other servers of the cluster.
var _ ops.Fanout = (*Manager)(nil)

// Others returns the other active servers.
func (m *Manager) Others(ctx context.Context) ([]ops.OtherServer, error) {
	rows, err := m.listServers(ctx)
	if err != nil {
		return nil, err
	}
	var out []ops.OtherServer
	for _, r := range rows {
		if r.ID != m.Self && r.Status == StatusActive {
			out = append(out, ops.OtherServer{ID: r.ID, PGPort: r.PGPort})
		}
	}
	return out, nil
}

// Apply runs a cluster-wide operation on a server.
func (m *Manager) Apply(ctx context.Context, server, kind string, params any) error {
	raw, err := json.Marshal(params)
	if err != nil {
		return err
	}
	return m.do(ctx, server, "POST", "/apply", wire.ApplyRequest{Kind: kind, Params: raw}, nil)
}

// Register registers a server in a sharded database.
func (m *Manager) Register(ctx context.Context, db string, s ops.OtherServer) error {
	return m.registerNode(ctx, db, s.ID, s.PGPort)
}

// Manager reads from another server with the internal call exec (leader only; the callers are leader operations).
var _ ops.RemoteQuerier = (*Manager)(nil)

// Query runs a read-only statement on a local database of another server and returns its rows.
func (m *Manager) Query(ctx context.Context, server, db, sql string) ([][]any, error) {
	var res internalapi.ExecResult
	if err := m.do(ctx, server, "POST", "/exec", internalapi.ExecRequest{Database: db, SQL: sql}, &res); err != nil {
		return nil, err
	}
	return res.Rows, nil
}
