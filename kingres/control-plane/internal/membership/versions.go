// SPDX-License-Identifier: PostgreSQL

package membership

import (
	"context"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/internalapi"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/monitor"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/peerclient"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/sysdb"
)

// Extensions whose versions the servers must agree on.
var versionedExtensions = []string{"kingres_shard", "kingres_ledger"}

// LocalVersions reports the versions of this server: kingresd, the PostgreSQL
// build and the version of every KingresDB extension that its instance offers
// (the default version of pg_available_extensions: what the binaries on this
// server provide).
func (m *Manager) LocalVersions(ctx context.Context) (internalapi.VersionInfo, error) {
	out := internalapi.VersionInfo{Extensions: map[string]string{}}
	c, err := m.Svc.ServiceConn(ctx, "postgres")
	if err != nil {
		return out, err
	}
	defer func() { _ = c.Close(context.WithoutCancel(ctx)) }()
	if err := c.QueryRow(ctx, `SELECT current_setting('server_version')`).Scan(&out.PostgreSQL); err != nil {
		return out, err
	}
	rows, err := c.Query(ctx, `SELECT name, default_version FROM pg_available_extensions WHERE name = ANY($1)`, versionedExtensions)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		var v *string
		if err := rows.Scan(&name, &v); err != nil {
			return out, err
		}
		if v != nil {
			out.Extensions[name] = *v
		}
	}
	return out, rows.Err()
}

// versionSource is the manager leader's view of the versions: its own and the
// ones the other servers report over the internal API.
type versionSource struct{ m *Manager }

func (s versionSource) Local(ctx context.Context) (monitor.ServerVersions, error) {
	v, err := s.m.LocalVersions(ctx)
	return monitor.ServerVersions{Server: s.m.Self, KingresDB: s.m.Version, PostgreSQL: v.PostgreSQL, Extensions: v.Extensions}, err
}

func (s versionSource) Others(ctx context.Context) ([]string, error) {
	others, err := s.m.Others(ctx)
	if err != nil {
		return nil, err
	}
	ids := make([]string, len(others))
	for i, o := range others {
		ids[i] = o.ID
	}
	return ids, nil
}

func (s versionSource) Ask(ctx context.Context, server string) (monitor.ServerVersions, error) {
	var st internalapi.State
	if err := s.m.do(ctx, server, "GET", "/state", nil, &st); err != nil {
		return monitor.ServerVersions{}, err
	}
	// What a peer reports ends up in an alert message: bounded and cleaned.
	v := monitor.ServerVersions{Server: server, KingresDB: peerclient.CleanText(st.Version, 64), PostgreSQL: peerclient.CleanText(st.PostgreSQL, 64),
		Extensions: map[string]string{}}
	for _, name := range versionedExtensions {
		if e, ok := st.Extensions[name]; ok {
			v.Extensions[name] = peerclient.CleanText(e, 64)
		}
	}
	return v, nil
}

// CheckVersions runs the version check once (on the manager leader).
func (m *Manager) CheckVersions(ctx context.Context) error {
	c, err := m.Svc.ServiceConn(ctx, sysdb.Database)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close(context.WithoutCancel(ctx)) }()
	return monitor.CheckVersions(ctx, c, versionSource{m}, time.Now())
}

// PeerUptime asks a server how long its kingresd has run.
func (m *Manager) PeerUptime(ctx context.Context, server string) (int64, error) {
	var st internalapi.State
	if err := m.do(ctx, server, "GET", "/state", nil, &st); err != nil {
		return 0, err
	}
	return st.UptimeSeconds, nil
}
