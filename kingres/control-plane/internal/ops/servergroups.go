// SPDX-License-Identifier: PostgreSQL

package ops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/sysdb"
)

// Server groups and the update of a server (README, "Server groups and
// updateServer"). The groups and the assignment of servers to them are state of
// the control plane: kingres_cp.server_group and kingres_cp.server.server_group
// in the system database of the manager leader, which is where these functions
// run (the HTTP layer forwards the operations there). The column
// kingres.node.server_group of the shard catalog is never written: the
// extension has no function to set it.

// groupNameRE is the pattern of the contract for the name of a group.
var groupNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,62}$`)

// maxGroupDescription is the longest description, in characters.
const maxGroupDescription = 200

// serverReadOnly are the properties of Server that a client may not send to
// updateServer; they are reported as read-only rather than as unknown.
var serverReadOnly = map[string]bool{
	"id": true, "address": true, "hostname": true, "status": true, "isManagerLeader": true, "version": true,
	"uptimeSeconds": true, "joinedAt": true, "lastHeartbeatAt": true, "certificateExpiresAt": true,
	"seenFingerprint": true, "fingerprintPinned": true, "instances": true, "buckets": true, "sizeBytes": true,
	"replicationLagMaxSeconds": true, "resources": true, "bucketsByDatabase": true,
}

func bad(field, msg, code string) problem.FieldError {
	return problem.FieldError{Field: field, Message: msg, Code: code}
}

// ParseServerGroupCreate validates the body of createServerGroup, given as the
// members of the JSON object, against the contract.
func ParseServerGroupCreate(raw map[string]json.RawMessage) (api.ServerGroupCreateRequest, []problem.FieldError) {
	var req api.ServerGroupCreateRequest
	var errs []problem.FieldError
	keys := make([]string, 0, len(raw))
	for k := range raw {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		switch k {
		case "name", "description":
		default:
			errs = append(errs, bad("/"+k, "is not a property of a server group", "unknown_field"))
		}
	}
	if v, ok := raw["name"]; !ok {
		errs = append(errs, bad("/name", "is required", "required"))
	} else if err := json.Unmarshal(v, &req.Name); err != nil || string(v) == "null" {
		errs = append(errs, bad("/name", "must be a string", "type"))
	} else if !groupNameRE.MatchString(req.Name) {
		errs = append(errs, bad("/name", "must match ^[A-Za-z0-9][A-Za-z0-9_.-]{0,62}$", "pattern"))
	}
	if v, ok := raw["description"]; ok {
		if err := json.Unmarshal(v, &req.Description); err != nil || string(v) == "null" {
			errs = append(errs, bad("/description", "must be a string", "type"))
		} else if utf8.RuneCountInString(req.Description) > maxGroupDescription {
			errs = append(errs, bad("/description", "must have at most 200 characters", "max_length"))
		}
	}
	return req, errs
}

// ParseServerUpdate validates the body of updateServer: only serverGroup
// (a string or null) and shouldHaveShards (a boolean) exist, and at least one
// must be present.
func ParseServerUpdate(raw map[string]json.RawMessage) (api.ServerUpdateRequest, []problem.FieldError) {
	var req api.ServerUpdateRequest
	var errs []problem.FieldError
	keys := make([]string, 0, len(raw))
	for k := range raw {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		switch {
		case k == "serverGroup" || k == "shouldHaveShards":
		case serverReadOnly[k]:
			errs = append(errs, bad("/"+k, "is read-only; only serverGroup and shouldHaveShards can be changed", "read_only"))
		default:
			errs = append(errs, bad("/"+k, "is not a property of a server", "unknown_field"))
		}
	}
	if len(raw) == 0 {
		errs = append(errs, bad("/", "at least one of serverGroup and shouldHaveShards is required", "required"))
	}
	if v, ok := raw["serverGroup"]; ok {
		req.ServerGroupSet = true
		if string(v) != "null" {
			var s string
			if err := json.Unmarshal(v, &s); err != nil {
				errs = append(errs, bad("/serverGroup", "must be a string or null", "type"))
			} else {
				req.ServerGroup = &s
			}
		}
	}
	if v, ok := raw["shouldHaveShards"]; ok {
		var b bool
		if err := json.Unmarshal(v, &b); err != nil || string(v) == "null" {
			errs = append(errs, bad("/shouldHaveShards", "must be a boolean", "type"))
		} else {
			req.ShouldHaveShards = &b
		}
	}
	return req, errs
}

// ListServerGroups returns the groups with their member servers, ordered by
// name; servers of a group are ordered by id.
func (o *Ops) ListServerGroups(ctx context.Context) ([]api.ServerGroup, error) {
	var out []api.ServerGroup
	err := o.asService(ctx, sysdb.Database, func(tx pgx.Tx) error {
		var err error
		out, err = readGroups(ctx, tx)
		return err
	})
	return out, wrap(err)
}

func readGroups(ctx context.Context, x execer) ([]api.ServerGroup, error) {
	rows, err := x.Query(ctx, `SELECT g.name, g.description, coalesce(array_agg(s.server_id ORDER BY s.server_id)
			FILTER (WHERE s.server_id IS NOT NULL), '{}')
		FROM kingres_cp.server_group g LEFT JOIN kingres_cp.server s ON s.server_group = g.name
		GROUP BY g.name, g.description ORDER BY g.name COLLATE "C"`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []api.ServerGroup{}
	for rows.Next() {
		var g api.ServerGroup
		if err := rows.Scan(&g.Name, &g.Description, &g.ServerIDs); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// CreateServerGroup creates an empty group. A name that exists is
// already_exists (the comparison is exact).
func (o *Ops) CreateServerGroup(ctx context.Context, req api.ServerGroupCreateRequest) (api.ServerGroup, error) {
	err := o.asService(ctx, sysdb.Database, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO kingres_cp.server_group (name, description) VALUES ($1, $2)`, req.Name, req.Description)
		return err
	})
	var pe *pgconn.PgError
	if errors.As(err, &pe) && pe.Code == "23505" {
		return api.ServerGroup{}, problem.New(problem.AlreadyExists, fmt.Sprintf("A server group named %q already exists.", req.Name))
	}
	if err != nil {
		return api.ServerGroup{}, wrap(err)
	}
	return api.ServerGroup{Name: req.Name, Description: req.Description, ServerIDs: []string{}}, nil
}

// DeleteServerGroup deletes an empty group: not_found when it does not exist,
// server_group_not_empty (listing the servers) while any server, in any
// status, is assigned to it. Nothing changes on a refusal.
func (o *Ops) DeleteServerGroup(ctx context.Context, name string) error {
	return wrap(o.asService(ctx, sysdb.Database, func(tx pgx.Tx) error {
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM kingres_cp.server_group WHERE name = $1)`, name).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return problem.New(problem.NotFound, fmt.Sprintf("Server group %q does not exist.", name))
		}
		members, err := groupMembers(ctx, tx, name)
		if err != nil {
			return err
		}
		if len(members) > 0 {
			return notEmpty(name, members)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM kingres_cp.server_group WHERE name = $1`, name); err != nil {
			var pe *pgconn.PgError
			if errors.As(err, &pe) && pe.Code == "23503" {
				// A server was assigned between the check and the delete.
				return problem.New(problem.ServerGroupNotEmpty, fmt.Sprintf("Server group %q was just assigned to a server.", name))
			}
			return err
		}
		return nil
	}))
}

func groupMembers(ctx context.Context, x execer, name string) ([]string, error) {
	rows, err := x.Query(ctx, `SELECT server_id FROM kingres_cp.server WHERE server_group = $1 ORDER BY server_id COLLATE "C"`, name)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

func notEmpty(name string, members []string) error {
	return problem.New(problem.ServerGroupNotEmpty,
		fmt.Sprintf("Server group %q still has servers: %s. Move or unassign them first.", name, strings.Join(members, ", ")))
}

// ServerPlacement is what UpdateServer needs to know of the server from the
// shard catalog.
type ServerPlacement struct {
	// ShouldHaveShards is the value the catalog shows now.
	ShouldHaveShards bool
}

// UpdateServer applies a validated update to a server that exists in the shard
// catalog. Order of the checks: group exists (422), then, only when something
// would change, an unfinished add/remove/leave of the server (409
// operation_in_progress) and a running rebalance (409 rebalance_in_progress).
// A value equal to the current one changes nothing. The group assignment and
// the catalog change are made under one transaction of the system database:
// when the catalog refuses, the assignment is rolled back.
func (o *Ops) UpdateServer(ctx context.Context, serverID string, cur ServerPlacement, req api.ServerUpdateRequest) error {
	return wrap(o.asService(ctx, sysdb.Database, func(tx pgx.Tx) error {
		var group *string
		var status string
		err := tx.QueryRow(ctx, `SELECT server_group, status FROM kingres_cp.server WHERE server_id = $1 FOR UPDATE`, serverID).Scan(&group, &status)
		if errors.Is(err, pgx.ErrNoRows) {
			return problem.New(problem.NotFound, fmt.Sprintf("Server %s does not exist.", serverID))
		}
		if err != nil {
			return err
		}
		changeGroup := false
		if req.ServerGroupSet {
			if req.ServerGroup != nil {
				var ok bool
				if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM kingres_cp.server_group WHERE name = $1)`, *req.ServerGroup).Scan(&ok); err != nil {
					return err
				}
				if !ok {
					p := problem.New(problem.Unprocessable, fmt.Sprintf("Server group %q does not exist; create it first.", *req.ServerGroup))
					p.Errors = []problem.FieldError{bad("/serverGroup", "no such server group", "unknown_group")}
					return p
				}
			}
			changeGroup = (group == nil) != (req.ServerGroup == nil) || group != nil && *group != *req.ServerGroup
		}
		changeShards := req.ShouldHaveShards != nil && *req.ShouldHaveShards != cur.ShouldHaveShards
		if !changeGroup && !changeShards {
			return nil
		}
		if status != "active" {
			return problem.New(problem.OperationInProgress, fmt.Sprintf("Server %s is being added or removed.", serverID))
		}
		var kind string
		err = tx.QueryRow(ctx, `SELECT kind FROM kingres_cp.task
			WHERE subject_type = 'server' AND subject_id = $1 AND kind IN ('server.add', 'server.remove', 'server.leave')
			  AND state IN ('pending', 'running', 'paused') LIMIT 1`, serverID).Scan(&kind)
		switch {
		case err == nil:
			return problem.New(problem.OperationInProgress, fmt.Sprintf("Server %s has an unfinished task of kind %s.", serverID, kind))
		case !errors.Is(err, pgx.ErrNoRows):
			return err
		}
		var rebalancing bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM kingres_cp.task WHERE kind = 'rebalance' AND state IN ('pending', 'running', 'paused'))
			`).Scan(&rebalancing); err != nil {
			return err
		}
		if rebalancing {
			return problem.New(problem.RebalanceInProgress, "A rebalance is running; it would change the placement under it.")
		}
		if changeGroup {
			if _, err := tx.Exec(ctx, `UPDATE kingres_cp.server SET server_group = $2 WHERE server_id = $1`, serverID, req.ServerGroup); err != nil {
				return err
			}
		}
		if changeShards {
			return o.setShouldHaveShards(ctx, serverID, *req.ShouldHaveShards)
		}
		return nil
	}))
}

// setShouldHaveShards sets the flag of every node of the server in the shard
// catalog of every database that has one, with kingres.set_node_property (the
// extension's function, which runs on the map group's primary, that is, on the
// leader, and publishes the catalog). Rebalance moves nothing until it runs.
func (o *Ops) setShouldHaveShards(ctx context.Context, host string, want bool) error {
	c, err := o.Service.ServiceConn(ctx, "postgres")
	if err != nil {
		return connectProblem(err)
	}
	rows, err := c.Query(ctx, `SELECT datname FROM pg_catalog.pg_database WHERE NOT datistemplate AND datallowconn ORDER BY datname`)
	var dbs []string
	if err == nil {
		dbs, err = pgx.CollectRows(rows, pgx.RowTo[string])
	}
	_ = c.Close(context.WithoutCancel(ctx))
	if err != nil {
		return connectProblem(err)
	}
	for _, db := range dbs {
		if err := o.setShouldHaveShardsIn(ctx, db, host, want); err != nil {
			return err
		}
	}
	return nil
}

func (o *Ops) setShouldHaveShardsIn(ctx context.Context, db, host string, want bool) error {
	c, err := o.Service.ServiceConn(ctx, db)
	if err != nil {
		return connectProblem(err)
	}
	defer func() { _ = c.Close(context.WithoutCancel(ctx)) }()
	var have bool
	if err := c.QueryRow(ctx, `SELECT to_regclass('kingres.node') IS NOT NULL`).Scan(&have); err != nil {
		return PGProblem(err)
	}
	if !have {
		return nil
	}
	rows, err := c.Query(ctx, `SELECT node_id FROM kingres.node WHERE host = $1 AND should_have_shards <> $2 ORDER BY node_id`, host, want)
	if err != nil {
		return PGProblem(err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[int32])
	if err != nil {
		return PGProblem(err)
	}
	if len(ids) == 0 {
		return nil
	}
	return setNodeProperty(ctx, c, db, ids, want)
}

// setNodeProperty changes should_have_shards of the nodes in one transaction. It
// calls kingres.set_node_property where the extension has it (0.3 and later);
// for 0.2, which has the column and kingres.sync_catalog() but not the
// function, it runs the same statements (the lock of the map version, the
// update, the publication of the catalog), after checking, as the function
// does, that this instance holds the primary of the map group.
func setNodeProperty(ctx context.Context, c *pgx.Conn, db string, ids []int32, want bool) error {
	var hasFn bool
	if err := c.QueryRow(ctx, `SELECT to_regprocedure('kingres.set_node_property(integer,boolean)') IS NOT NULL`).Scan(&hasFn); err != nil {
		return PGProblem(err)
	}
	err := inTx(ctx, c, func(tx pgx.Tx) error {
		if hasFn {
			for _, id := range ids {
				if _, err := tx.Exec(ctx, `SELECT kingres.set_node_property($1, $2)`, id, want); err != nil {
					return err
				}
			}
			return nil
		}
		if _, err := tx.Exec(ctx, `LOCK TABLE kingres.map_version IN EXCLUSIVE MODE`); err != nil {
			return err
		}
		var isMap bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM kingres.local_node l JOIN kingres.node n ON n.node_id = l.node_id
			WHERE n.group_id = 0 AND n.role = 'primary')`).Scan(&isMap); err != nil {
			return err
		}
		if !isMap {
			return errNotMapPrimary
		}
		if _, err := tx.Exec(ctx, `UPDATE kingres.node SET should_have_shards = $2 WHERE node_id = ANY($1)`, ids, want); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `SELECT kingres.sync_catalog()`)
		return err
	})
	var pe *pgconn.PgError
	switch {
	case errors.Is(err, errNotMapPrimary) || errors.As(err, &pe) && pe.Code == "55000":
		return problem.New(problem.NodeStateInvalid, "This server does not hold the primary of the map group in database "+db+"; shouldHaveShards can only be changed through it.")
	}
	return err
}

var errNotMapPrimary = errors.New("not the map group primary")

// ValidGroupName reports whether name can be the name of a group.
func ValidGroupName(name string) bool { return groupNameRE.MatchString(name) }
