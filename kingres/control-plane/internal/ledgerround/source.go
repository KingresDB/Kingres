// SPDX-License-Identifier: AGPL-3.0-only

package ledgerround

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/extguard"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/internalapi"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/peerclient"
)

// Group is a shard group and the server that runs its primary.
type Group struct {
	ID     int32
	Server string
}

// ReadError is the failure of one read of a group, with the code the callee gave. A group whose read failed is unread in the round: its
// members are stale, the digest is incomplete and its group is named in uncoveredShardGroupIds.
type ReadError struct {
	Code    string
	Message string
	// Group is the group the read was about; Transport is true when no answer came (a timeout, a refused connection, a server that is down).
	Group     int32
	Transport bool
}

func (e *ReadError) Error() string {
	if e.Transport {
		return fmt.Sprintf("shard group %d could not be reached: %s", e.Group, e.Message)
	}
	return fmt.Sprintf("shard group %d: %s: %s", e.Group, e.Code, e.Message)
}

// Source reads the groups. The collector uses nothing else to see a database.
type Source interface {
	// Groups lists the shard groups that hold database db with the server that runs the primary of each, ascending by group id. A database
	// that is not sharded lives in one group (0), on the server that asks.
	Groups(ctx context.Context, db string) ([]Group, error)
	// Read runs one call on the server of the group. The error is a *ReadError.
	Read(ctx context.Context, g Group, req ReadRequest) (ReadResponse, error)
}

// ClusterSource is the Source of the daemon: the catalog of the system database for the groups, this server's Reader for the groups it
// runs and the internal call for the others.
type ClusterSource struct {
	Self string
	// Conn opens a service connection to database db on this server (the catalog of the shard groups is in each sharded database).
	Conn func(ctx context.Context, db string) (*pgx.Conn, error)
	// SysConn opens a service connection to the system database, whose catalog of the shard groups is the one that is left when the database
	// itself is gone (a database dropped from SQL): the groups are then asked, and each answers that it has no such database.
	SysConn func(ctx context.Context) (*pgx.Conn, error)
	// Local answers the calls about the groups this server runs.
	Local *Reader
	// Call is the internal call to another server (method, path without /internal/v1, body, answer).
	Call func(ctx context.Context, server, method, path string, in, out any) error
	// Timeout bounds one read (default 10 seconds, design 5.4).
	Timeout time.Duration
}

// Groups implements Source.
func (s *ClusterSource) Groups(ctx context.Context, db string) ([]Group, error) {
	c, err := s.Conn(ctx, db)
	var pe *pgconn.PgError
	if errors.As(err, &pe) && pe.Code == "3D000" && s.SysConn != nil {
		c, err = s.SysConn(ctx)
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = c.Close(context.WithoutCancel(ctx)) }()
	// Before anything of the schema kingres is read: the sharding extension must be there and its schema must be the extension's alone (extguard); a schema
	// that a database owner made would run its functions as the service role.
	info, err := extguard.Verify(ctx, c, extguard.Shard)
	if err != nil {
		if _, bad := extguard.IsUntrusted(err); bad {
			return nil, fmt.Errorf("the groups of database %s are not read: %w", db, err)
		}
		return nil, err
	}
	var sharded bool
	if info.Present {
		if err := c.QueryRow(ctx, `SELECT pg_catalog.to_regclass('kingres.node') IS NOT NULL`).Scan(&sharded); err != nil {
			return nil, err
		}
	}
	if !sharded {
		return []Group{{ID: 0, Server: s.Self}}, nil
	}
	rows, err := c.Query(ctx, `SELECT group_id, host FROM kingres.node WHERE role = 'primary' AND is_active ORDER BY group_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Group
	for rows.Next() {
		var g Group
		if err := rows.Scan(&g.ID, &g.Server); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// Read implements Source.
func (s *ClusterSource) Read(ctx context.Context, g Group, req ReadRequest) (ReadResponse, error) {
	to := s.Timeout
	if to <= 0 {
		to = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, to)
	defer cancel()
	req.Group = g.ID
	var out ReadResponse
	var err error
	if g.Server == s.Self && s.Local != nil {
		var body []byte
		if body, err = json.Marshal(req); err != nil {
			return out, err
		}
		var res any
		if res, err = s.Local.Handle(ctx, body); err == nil {
			out, _ = res.(ReadResponse)
		}
	} else {
		if s.Call == nil {
			return out, &ReadError{Group: g.ID, Transport: true, Message: "this server cannot call other servers"}
		}
		err = s.Call(ctx, g.Server, "POST", strings.TrimPrefix(CallRead, "POST "), req, &out)
	}
	if err == nil {
		return out, nil
	}
	var ce *internalapi.CallError
	var ae *peerclient.APIError
	switch {
	case errors.As(err, &ce):
		return out, &ReadError{Group: g.ID, Code: ce.Code, Message: ce.Message}
	case errors.As(err, &ae):
		return out, &ReadError{Group: g.ID, Code: ae.Code, Message: ae.Message}
	}
	return out, &ReadError{Group: g.ID, Transport: true, Message: err.Error()}
}
