// SPDX-License-Identifier: PostgreSQL

// Package membership is the leader's side of servers joining a cluster and
// leaving it (design control-plane-3.md, D4 and D5), and the receiving side of
// the internal calls that the leader makes on the other servers. It owns the
// tasks server.add and server.remove.
package membership

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/sysdb"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/wire"
)

// Server statuses recorded in kingres_cp.server.
const (
	StatusPending  = "pending"
	StatusActive   = "active"
	StatusRemoving = "removing"
)

// serverRow is a row of kingres_cp.server.
type serverRow struct {
	ID              string
	Status          string
	InternalAddress string
	PGPort          int
	Serial          string
	CertPEM         string
	PublicKey       []byte
	Seen            string
	Pinned          bool
	Registering     bool
	JoinedAt        time.Time
}

const rowCols = `server_id, status, coalesce(internal_address, ''), coalesce(pg_port, 0), coalesce(certificate_serial, ''),
	coalesce(certificate_pem, ''), coalesce(public_key_der, ''::bytea), coalesce(seen_fingerprint, ''), fingerprint_pinned, registering, joined_at`

func scanRow(r pgx.Row) (serverRow, error) {
	var s serverRow
	err := r.Scan(&s.ID, &s.Status, &s.InternalAddress, &s.PGPort, &s.Serial, &s.CertPEM, &s.PublicKey, &s.Seen, &s.Pinned, &s.Registering, &s.JoinedAt)
	return s, err
}

// sys runs f on a service connection to the system database.
func (m *Manager) sys(ctx context.Context, f func(c *pgx.Conn) error) error {
	c, err := m.Svc.ServiceConn(ctx, sysdb.Database)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close(context.WithoutCancel(ctx)) }()
	return f(c)
}

var errNoServer = errors.New("no such server")

func (m *Manager) getServer(ctx context.Context, id string) (serverRow, error) {
	var s serverRow
	err := m.sys(ctx, func(c *pgx.Conn) error {
		var err error
		s, err = scanRow(c.QueryRow(ctx, `SELECT `+rowCols+` FROM kingres_cp.server WHERE server_id = $1`, id))
		if errors.Is(err, pgx.ErrNoRows) {
			return errNoServer
		}
		return err
	})
	return s, err
}

func (m *Manager) listServers(ctx context.Context) ([]serverRow, error) {
	var out []serverRow
	err := m.sys(ctx, func(c *pgx.Conn) error {
		rows, err := c.Query(ctx, `SELECT `+rowCols+` FROM kingres_cp.server ORDER BY joined_at, server_id`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			s, err := scanRow(rows)
			if err != nil {
				return err
			}
			out = append(out, s)
		}
		return rows.Err()
	})
	return out, err
}

// peerInfos returns the servers as the members need to know them: the active
// ones, and the pending one named by withPending; except the one named by
// without. This server's own address comes from the peer directory.
func (m *Manager) peerInfos(ctx context.Context, withPending, without string) ([]wire.PeerInfo, error) {
	rows, err := m.listServers(ctx)
	if err != nil {
		return nil, err
	}
	var out []wire.PeerInfo
	for _, r := range rows {
		if r.ID == without {
			continue
		}
		if r.Status != StatusActive && r.ID != withPending {
			continue
		}
		p := wire.PeerInfo{ServerID: r.ID, PGPort: r.PGPort, InternalAddress: r.InternalAddress}
		if r.ID == m.Self {
			p.PGPort = m.Svc.PGPort()
			if me, ok := m.Peers.Get(m.Self); ok {
				p.InternalAddress = me.InternalAddr
			}
		}
		if p.InternalAddress == "" || p.PGPort == 0 {
			return nil, fmt.Errorf("the address of server %q is not recorded", r.ID)
		}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ServerID < out[j].ServerID })
	return out, nil
}

// clusterIdentity returns the cluster id and name recorded at setup.
func (m *Manager) clusterIdentity(ctx context.Context) (id, name string, err error) {
	err = m.sys(ctx, func(c *pgx.Conn) error {
		rows, err := c.Query(ctx, `SELECT key, value FROM kingres_cp.cluster_setting WHERE key IN ('cluster.id', 'cluster.name')`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var k string
			var v []byte
			if err := rows.Scan(&k, &v); err != nil {
				return err
			}
			var s string
			_ = json.Unmarshal(v, &s)
			if k == "cluster.id" {
				id = s
			} else {
				name = s
			}
		}
		return rows.Err()
	})
	return id, name, err
}

// memoryQuota returns the cluster's configured memory quota per server.
func (m *Manager) memoryQuota(ctx context.Context) int {
	mb := 1024
	_ = m.sys(ctx, func(c *pgx.Conn) error {
		var v []byte
		if err := c.QueryRow(ctx, `SELECT value FROM kingres_cp.cluster_setting WHERE key = 'memory.quotaMegabytes'`).Scan(&v); err == nil {
			var n int
			if json.Unmarshal(v, &n) == nil && n >= 512 {
				mb = n
			}
		}
		return nil
	})
	return mb
}
