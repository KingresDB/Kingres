// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026 RooyeKhat Media Co

package ops

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/extguard"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/store"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/task"
)

// Distributed schemas (design shard-phase11.md, section 13). The extension does the work
// (kingres.distribute_schema, move_schema, undistribute_schema, schema_inventory); this file lists the schemas,
// refuses what can be refused before a task exists, and runs the three tasks.

// Task kinds of the schema operations; the subject is "schema" with the id "database/schema".
const (
	KindSchemaDistribute   = "schema.distribute"
	KindSchemaMove         = "schema.move"
	KindSchemaUndistribute = "schema.undistribute"
	SubjectSchema          = "schema"
)

// SchemaFilter is the filter of listSchemas.
type SchemaFilter struct {
	Distributed  *bool
	ShardGroupID *int32
	State        string
}

type schemaInput struct {
	Database string `json:"database"`
	Schema   string `json:"schema"`
	// GroupID is the group distributeSchema was asked to use (nil: in place).
	GroupID *int32 `json:"groupId,omitempty"`
	// Target is the group of a move, or the group an undistribute moves the schema to first (0 with HasTarget false: none).
	Target        int32 `json:"target,omitempty"`
	HasTarget     bool  `json:"hasTarget,omitempty"`
	ReplicaIDFull bool  `json:"replicaIdentityFull,omitempty"`
	// Colocation is the colocation group of the schema, known after it is distributed.
	Colocation int32 `json:"colocation,omitempty"`
}

var systemSchemas = `n.nspname !~ '^pg_' AND n.nspname NOT IN ('information_schema', 'kingres', 'kingres_ledger')`

// schemaRows reads the schemas of a database from the map group primary (this server).
func (o *Ops) schemaRows(ctx context.Context, c *pgx.Conn, only string) ([]api.SchemaSummary, error) {
	dist, err := extUsable(ctx, c, extguard.Shard)
	if err != nil {
		return nil, err
	}
	if dist {
		if err := c.QueryRow(ctx, `SELECT to_regclass('kingres.schemas') IS NOT NULL`).Scan(&dist); err != nil {
			return nil, PGProblem(err)
		}
	}
	join, cols := "", "NULL::int, NULL::int, NULL::text, NULL::text, NULL::bigint, NULL::int"
	if dist {
		join = ` LEFT JOIN kingres.schemas d ON d.schema_name = n.nspname`
		cols = `d.colocation_id, d.group_id, d.state, (SELECT host FROM kingres.node WHERE node_id = d.node_id), d.size_bytes, d.moving_to_group_id`
	}
	q := `SELECT n.nspname, pg_get_userbyid(n.nspowner), ` + cols + `,
		(SELECT count(*) FROM pg_class c WHERE c.relnamespace = n.oid AND c.relkind IN ('r', 'p') AND NOT c.relispartition)::int,
		(SELECT sum(greatest(c.reltuples, 0))::bigint FROM pg_class c WHERE c.relnamespace = n.oid AND c.relkind IN ('r', 'p') AND NOT c.relispartition)
		FROM pg_namespace n` + join + ` WHERE ` + systemSchemas
	var args []any
	if only != "" {
		q += ` AND n.nspname = $1`
		args = append(args, only)
	}
	rows, err := c.Query(ctx, q+` ORDER BY n.nspname COLLATE "C"`, args...)
	if err != nil {
		o.logSchemaErr("querying the schemas", "", err)
		return nil, PGProblem(err)
	}
	defer rows.Close()
	out := []api.SchemaSummary{}
	for rows.Next() {
		var s api.SchemaSummary
		var colo, group, moving *int32
		var state, server *string
		var size *int64
		var tables int
		if err := rows.Scan(&s.Name, &s.Owner, &colo, &group, &state, &server, &size, &moving, &tables, &s.EstimatedRows); err != nil {
			o.logSchemaErr("reading a schema row", "", err)
			return nil, PGProblem(err)
		}
		s.TableCount, s.SizeBytes = tables, size
		if colo != nil && group != nil && state != nil {
			s.Distribution = &api.SchemaDistribution{State: *state, ColocationID: *colo, ShardGroupID: *group, ServerID: server, MovingToShardGroupID: moving}
			if *state == "splitting" {
				s.Distribution.State = "active"
			}
		}
		out = append(out, s)
	}
	return out, PGProblem(rows.Err())
}

// logSchemaErr keeps the cause of a problem that the answer hides (PGProblem says only that the instance is not available).
func (o *Ops) logSchemaErr(what, db string, err error) {
	if o.Log != nil {
		o.Log.Warn(what+" failed", "database", db, "error", err)
	}
}

// withDB runs f on a service connection to db. A database whose sharding schema cannot be trusted is refused: f reads it as a superuser. A database without
// kingres_shard is not: f has to ask extUsable before it reads kingres.* (schemaRows does).
func (o *Ops) withDB(ctx context.Context, db string, f func(c *pgx.Conn) error) error {
	c, err := o.Service.ServiceConn(ctx, db)
	if err != nil {
		var pe *pgconn.PgError
		if errors.As(err, &pe) && pe.Code == "3D000" {
			return problem.New(problem.NotFound, fmt.Sprintf("Database %q does not exist.", db))
		}
		return connectProblem(err)
	}
	defer func() { _ = c.Close(context.WithoutCancel(ctx)) }()
	if err := refuseUntrusted(ctx, c, extguard.Shard); err != nil {
		return err
	}
	return f(c)
}

// withShardDB is withDB for what cannot do without the sharding extension.
func (o *Ops) withShardDB(ctx context.Context, db string, f func(c *pgx.Conn) error) error {
	return o.withDB(ctx, db, func(c *pgx.Conn) error {
		if ok, err := extUsable(ctx, c, extguard.Shard); err != nil {
			return err
		} else if !ok {
			return problem.New(problem.UnsupportedOp, "Database "+db+" has no sharding catalog of version 0.8; distributed schemas need it.")
		}
		return f(c)
	})
}

// ListSchemas is listSchemas: every schema of the database that is not a system schema, ordered by name.
func (o *Ops) ListSchemas(ctx context.Context, db string, f SchemaFilter, p store.Page) (api.SchemaPage, error) {
	var all []api.SchemaSummary
	if err := o.withDB(ctx, db, func(c *pgx.Conn) (err error) { all, err = o.schemaRows(ctx, c, ""); return }); err != nil {
		o.logSchemaErr("listing the schemas", db, err)
		return api.SchemaPage{}, err
	}
	sel := all[:0]
	for _, s := range all {
		switch {
		case f.Distributed != nil && (s.Distribution != nil) != *f.Distributed:
		case f.ShardGroupID != nil && (s.Distribution == nil || s.Distribution.ShardGroupID != *f.ShardGroupID):
		case f.State != "" && (s.Distribution == nil || s.Distribution.State != f.State):
		default:
			sel = append(sel, s)
		}
	}
	items, next, err := store.Paginate(sel, p)
	if err != nil {
		return api.SchemaPage{}, problem.New(problem.BadRequest, "The cursor is not valid.")
	}
	return api.SchemaPage{Items: items, NextCursor: next}, nil
}

// inventory is the blockers of kingres.schema_inventory (the unsupported rows).
func inventory(ctx context.Context, c *pgx.Conn, schema string) ([]api.SchemaBlocker, error) {
	rows, err := c.Query(ctx, `SELECT object_kind, object_name, reason FROM kingres.schema_inventory($1::regnamespace) WHERE NOT supported ORDER BY object_kind, object_name`, quoteIdent(schema))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []api.SchemaBlocker{}
	for rows.Next() {
		var b api.SchemaBlocker
		var reason *string
		if err := rows.Scan(&b.ObjectKind, &b.ObjectName, &reason); err != nil {
			return nil, err
		}
		if reason != nil {
			b.Reason = *reason
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

func quoteIdent(s string) string { return pgx.Identifier{s}.Sanitize() }

// GetSchema is getSchema.
func (o *Ops) GetSchema(ctx context.Context, db, name string) (api.SchemaDetail, error) {
	var d api.SchemaDetail
	err := o.withDB(ctx, db, func(c *pgx.Conn) error {
		rows, err := o.schemaRows(ctx, c, name)
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			return problem.New(problem.NotFound, fmt.Sprintf("Schema %q does not exist in database %q.", name, db))
		}
		d.SchemaSummary = rows[0]
		d.DistributionBlockers, d.TablesWithoutKey = []api.SchemaBlocker{}, []string{}
		has, err := extUsable(ctx, c, extguard.Shard)
		if err != nil {
			return err
		}
		if has {
			if err := c.QueryRow(ctx, `SELECT to_regprocedure('kingres.schema_inventory(regnamespace)') IS NOT NULL`).Scan(&has); err != nil {
				return PGProblem(err)
			}
		}
		if !has {
			return nil
		}
		if d.Distribution == nil {
			if d.DistributionBlockers, err = inventory(ctx, c, name); err != nil {
				return PGProblem(err)
			}
		} else {
			var pin *string
			if err := c.QueryRow(ctx, `SELECT kingres.internal_schema_pinned($1)`, d.Distribution.ColocationID).Scan(&pin); err == nil && pin != nil {
				d.Pinned = &api.SchemaPin{Reason: *pin}
			}
		}
		keyless, err := keylessTables(ctx, c, name)
		if err != nil {
			return PGProblem(err)
		}
		d.TablesWithoutKey = keyless
		return nil
	})
	if err != nil {
		return d, err
	}
	d.MoveInProgress = o.unfinishedSchemaTask(ctx, db, name, KindSchemaMove)
	return d, nil
}

// keylessTables are the tables that a move cannot replicate: the replica identity is NOTHING (whatever keys the table has, an
// update or a delete carries no old row), or it is the default and the table has no primary key. A table with an identity index
// or FULL is not listed.
func keylessTables(ctx context.Context, c *pgx.Conn, schema string) ([]string, error) {
	rows, err := c.Query(ctx, `SELECT c.relname FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1 AND c.relkind IN ('r', 'p')
		AND (c.relreplident = 'n' OR (c.relreplident = 'd'
			AND NOT EXISTS (SELECT 1 FROM pg_index i WHERE i.indrelid = c.oid AND i.indisprimary))) ORDER BY c.relname COLLATE "C"`, schema)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if out == nil {
		out = []string{}
	}
	return out, err
}

// unfinishedSchemaTask is the id of an unfinished task of kind for the schema.
func (o *Ops) unfinishedSchemaTask(ctx context.Context, db, schema, kind string) *int64 {
	var id *int64
	_ = o.asService(ctx, "kingres_system", func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT id FROM kingres_cp.task WHERE kind = $1 AND subject_type = 'schema' AND subject_id = $2
			AND state IN ('pending', 'running', 'paused') ORDER BY id DESC LIMIT 1`, kind, db+"/"+schema).Scan(&id)
	})
	return id
}

// ---- Prepare: the checks before a task exists

// sharding mode of the database and whether the extension has distributed schemas.
func schemaMode(ctx context.Context, c *pgx.Conn, db string) (string, error) {
	has, err := extUsable(ctx, c, extguard.Shard)
	if err != nil {
		return "", err
	}
	if has {
		if err := c.QueryRow(ctx, `SELECT to_regclass('kingres.sharding_settings') IS NOT NULL`).Scan(&has); err != nil {
			return "", PGProblem(err)
		}
	}
	if !has {
		return "", problem.New(problem.UnsupportedOp, "Database "+db+" has no sharding catalog of version 0.8; distributed schemas need it.")
	}
	var mode string
	return mode, PGProblem(c.QueryRow(ctx, `SELECT mode FROM kingres.sharding_settings`).Scan(&mode))
}

func (o *Ops) rebalanceActive(ctx context.Context) bool {
	var n int
	_ = o.asService(ctx, "kingres_system", func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM kingres_cp.task WHERE kind = 'rebalance' AND state IN ('pending', 'running')`).Scan(&n)
	})
	return n > 0
}

func (o *Ops) groupReceives(ctx context.Context, c *pgx.Conn, g int32) (bool, error) {
	var ok bool
	err := c.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM kingres.node WHERE group_id = $1 AND role = 'primary' AND is_active AND should_have_shards)`, g).Scan(&ok)
	return ok, PGProblem(err)
}

// PrepareDistributeSchema validates distributeSchema and returns the task input.
func (o *Ops) PrepareDistributeSchema(ctx context.Context, db, schema string, req api.DistributeSchemaRequest) (any, error) {
	in := schemaInput{Database: db, Schema: schema, GroupID: req.ShardGroupID, ReplicaIDFull: req.SetReplicaIdentityFull}
	err := o.withShardDB(ctx, db, func(c *pgx.Conn) error {
		mode, err := schemaMode(ctx, c, db)
		if err != nil {
			return err
		}
		if mode == "row" {
			return problem.New(problem.Conflict, "Database "+db+" shards by rows only; change its sharding mode to schema or both first.")
		}
		rows, err := o.schemaRows(ctx, c, schema)
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			return problem.New(problem.NotFound, fmt.Sprintf("Schema %q does not exist in database %q.", schema, db))
		}
		if rows[0].Distribution != nil {
			return problem.New(problem.Conflict, fmt.Sprintf("Schema %q is already distributed.", schema))
		}
		if req.ShardGroupID != nil {
			ok, err := o.groupReceives(ctx, c, *req.ShardGroupID)
			if err != nil {
				return err
			}
			if !ok {
				return problem.New(problem.Unprocessable, fmt.Sprintf("Shard group %d does not exist or should not hold shards.", *req.ShardGroupID))
			}
		}
		bl, err := inventory(ctx, c, schema)
		if err != nil {
			return PGProblem(err)
		}
		if len(bl) > 0 {
			return notDistributable(bl)
		}
		return nil
	})
	return in, err
}

func notDistributable(bl []api.SchemaBlocker) error {
	p := problem.New(problem.SchemaNotDistributable, fmt.Sprintf("The schema cannot be distributed: %d objects are not supported in a distributed schema.", len(bl)))
	for _, b := range bl {
		p.Errors = append(p.Errors, problem.FieldError{Field: b.ObjectName, Message: b.ObjectKind + ": " + b.Reason, Code: "unsupported_object"})
	}
	return p
}

// distributedRow reads the distribution of a schema; 404 when the schema does not exist, a conflict when it is local.
func (o *Ops) distributedRow(ctx context.Context, c *pgx.Conn, db, schema string) (api.SchemaSummary, error) {
	rows, err := o.schemaRows(ctx, c, schema)
	if err != nil {
		return api.SchemaSummary{}, err
	}
	if len(rows) == 0 {
		return api.SchemaSummary{}, problem.New(problem.NotFound, fmt.Sprintf("Schema %q does not exist in database %q.", schema, db))
	}
	if rows[0].Distribution == nil {
		return rows[0], problem.New(problem.Conflict, fmt.Sprintf("Schema %q is not distributed.", schema))
	}
	return rows[0], nil
}

// PrepareMoveSchema validates moveSchema.
func (o *Ops) PrepareMoveSchema(ctx context.Context, db, schema string, req api.MoveSchemaRequest) (any, error) {
	in := schemaInput{Database: db, Schema: schema, Target: req.TargetShardGroupID, HasTarget: true, ReplicaIDFull: req.SetReplicaIdentityFull}
	err := o.withShardDB(ctx, db, func(c *pgx.Conn) error {
		s, err := o.distributedRow(ctx, c, db, schema)
		if err != nil {
			return err
		}
		in.Colocation = s.Distribution.ColocationID
		if s.Distribution.State != "active" || o.unfinishedSchemaTask(ctx, db, schema, KindSchemaMove) != nil {
			return problem.New(problem.SchemaMoveInProgress, fmt.Sprintf("Schema %q is being moved.", schema))
		}
		if s.Distribution.ShardGroupID == req.TargetShardGroupID {
			return problem.New(problem.Unprocessable, "The schema is on that shard group already.")
		}
		if err := cleaningMove(ctx, c, s.Distribution.ColocationID, schema); err != nil {
			return err
		}
		ok, err := o.groupReceives(ctx, c, req.TargetShardGroupID)
		if err != nil {
			return err
		}
		if !ok {
			return problem.New(problem.Unprocessable, fmt.Sprintf("Shard group %d does not exist or should not hold shards.", req.TargetShardGroupID))
		}
		if o.rebalanceActive(ctx) {
			return problem.New(problem.RebalanceInProgress, "A rebalance is running; a move of a schema waits for it.")
		}
		if !req.SetReplicaIdentityFull {
			if err := refuseKeyless(ctx, c, schema, "Tables without a primary key or replica identity cannot be moved; set REPLICA IDENTITY FULL on them (setReplicaIdentityFull)"); err != nil {
				return err
			}
		}
		return o.refusePin(ctx, c, s.Distribution.ColocationID)
	})
	return in, err
}

// refuseKeyless is the 422 of a move of a schema that has tables the move cannot replicate: the text says what to do and every table
// is an entry of errors (code replica_identity_missing). nil when there is none.
func refuseKeyless(ctx context.Context, c *pgx.Conn, schema, what string) error {
	keyless, err := keylessTables(ctx, c, schema)
	if err != nil {
		return PGProblem(err)
	}
	if len(keyless) == 0 {
		return nil
	}
	p := problem.New(problem.Unprocessable, fmt.Sprintf("%s: %s.", what, strings.Join(keyless, ", ")))
	for _, t := range keyless {
		p.Errors = append(p.Errors, problem.FieldError{Field: t, Message: "no primary key and no replica identity", Code: "replica_identity_missing"})
	}
	return p
}

// cleaningMove refuses an operation on a schema whose last move has not finished its cleaning: the old copy is emptied after
// kingres_shard.move_cleanup_delay (10 minutes by default), and until then the extension answers "is being moved" to a move or
// to undistribute_schema. The refusal says so before a task exists.
func cleaningMove(ctx context.Context, c *pgx.Conn, colocation int32, schema string) error {
	var cleaning bool
	if err := c.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM kingres.bucket_move WHERE colocation_id = $1 AND phase = 'cleaning')`, colocation).Scan(&cleaning); err != nil {
		return nil // a catalog without the table (an older extension) has nothing to wait for
	}
	if cleaning {
		return problem.New(problem.SchemaMoveInProgress, fmt.Sprintf("The old copy of schema %q is still being cleaned up after its last move (kingres_shard.move_cleanup_delay); try again when that is done.", schema))
	}
	return nil
}

func (o *Ops) refusePin(ctx context.Context, c *pgx.Conn, colocation int32) error {
	var pin *string
	if err := c.QueryRow(ctx, `SELECT kingres.internal_schema_pinned($1)`, colocation).Scan(&pin); err == nil && pin != nil {
		return problem.New(problem.SchemaPinned, *pin)
	}
	return nil
}

// PrepareUndistributeSchema validates undistributeSchema; target (nil: where it is) names a group to move it to first.
func (o *Ops) PrepareUndistributeSchema(ctx context.Context, db, schema string, target *int32) (any, error) {
	in := schemaInput{Database: db, Schema: schema}
	err := o.withShardDB(ctx, db, func(c *pgx.Conn) error {
		s, err := o.distributedRow(ctx, c, db, schema)
		if err != nil {
			return err
		}
		in.Colocation = s.Distribution.ColocationID
		if s.Distribution.State != "active" || o.unfinishedSchemaTask(ctx, db, schema, KindSchemaMove) != nil {
			return problem.New(problem.SchemaMoveInProgress, fmt.Sprintf("Schema %q is being moved.", schema))
		}
		if err := o.refusePin(ctx, c, s.Distribution.ColocationID); err != nil {
			return err
		}
		if err := cleaningMove(ctx, c, s.Distribution.ColocationID, schema); err != nil {
			return err
		}
		if target != nil && *target != s.Distribution.ShardGroupID {
			ok, err := o.groupReceives(ctx, c, *target)
			if err != nil {
				return err
			}
			if !ok {
				return problem.New(problem.Unprocessable, fmt.Sprintf("Shard group %d does not exist or should not hold shards.", *target))
			}
			// the move first is a move: its tables need a key as much as a move of their own (the request has no setReplicaIdentityFull)
			if err := refuseKeyless(ctx, c, schema, "Tables without a primary key or replica identity cannot be moved first; move the schema with setReplicaIdentityFull, or add a key"); err != nil {
				return err
			}
			if o.rebalanceActive(ctx) {
				return problem.New(problem.RebalanceInProgress, "A rebalance is running; moving a schema first waits for it.")
			}
			in.Target, in.HasTarget = *target, true
		}
		return nil
	})
	return in, err
}

// ---- the tasks

// Step names (shown to users), design 13.1.
const (
	stepSchemaCheck     = "Check the schema"
	stepSchemaIdentity  = "Set REPLICA IDENTITY FULL"
	stepSchemaMake      = "Make the schema distributed"
	stepSchemaVerify    = "Verify on every server"
	stepMoveCheck       = "Check the move"
	stepMoveCopy        = "Copy the data"
	stepMoveSwitch      = "Switch"
	stepMoveClean       = "Clean up the old copy"
	stepUndistCheck     = "Check the schema"
	stepUndistMoveFirst = "Move first"
	stepUndistMake      = "Make the schema local"
)

func (o *Ops) schemaDefinitions() []task.Definition {
	return []task.Definition{
		{Kind: KindSchemaDistribute, Steps: []task.Step{
			{Name: stepSchemaCheck, Run: o.sDistCheck},
			{Name: stepSchemaIdentity, Run: o.sDistIdentity},
			// distribute_schema is one call that creates the schema on every node and registers it, in a transaction that is
			// prepared on the other nodes first: a run that ended in it asks the catalog whether it took effect, and settles what
			// a cut call left prepared before it runs the call again (Recover).
			{Name: stepSchemaMake, NonIdempotent: true, Run: o.sDistMake, Recover: o.recoverDistribute},
			{Name: stepSchemaVerify, Run: o.sVerifyDistributed},
		}},
		{Kind: KindSchemaMove, Steps: []task.Step{
			{Name: stepMoveCheck, Run: o.sMoveCheck},
			// Not interruptible: the engine would end the task as cancelled while the extension goes on with the move. The step
			// asks the extension to stop (before its switch) and says truthfully what happened (see runSchemaMove).
			{Name: stepMoveCopy, Run: o.sMoveRun},
			{Name: stepMoveSwitch, Run: o.sMoveSwitch},
			{Name: stepMoveClean, Run: o.sMoveClean},
		}, Cancellable: moveCancellable},
		{Kind: KindSchemaUndistribute, Steps: []task.Step{
			{Name: stepUndistCheck, Run: o.sUndistCheck},
			{Name: stepUndistMoveFirst, Run: o.sUndistMoveFirst},
			{Name: stepUndistMake, NonIdempotent: true, Run: o.sUndistMake, Recover: o.recoverUndistribute},
			{Name: stepSchemaVerify, Run: o.sVerifyLocal},
		}, Cancellable: undistributeCancellable},
	}
}

// moveSwitchedDetail are the prefixes of the detail of a task that moves a schema once the move is in or past its switch (the
// phases of the extension's record). From the phase switching the move rolls forward only, so a cancel request is refused, and the
// mark of one that arrived before is not followed.
var moveSwitchedDetail = []string{"switching", "cleaning", "done", moveTooLateText}

// moveTooLateText is the detail of a move whose cancel request could not stop it (it came in or after the switch).
const moveTooLateText = "The cancel request came too late"

func switchPassed(detail string) bool {
	for _, p := range moveSwitchedDetail {
		if strings.HasPrefix(detail, p) {
			return true
		}
	}
	return false
}

// moveCancellable: a cancel is accepted until the switch of the extension has started. The switch happens inside the call of the
// copy step, so the guard looks at the phase that the step records in the detail, and at the steps: once the copy step is done the
// schema is on its target and there is nothing left to cancel (design 13.1).
func moveCancellable(t task.Task) bool {
	for _, name := range []string{stepMoveCopy, stepMoveSwitch, stepMoveClean} {
		if stepState(t, name) == task.StepDone {
			return false
		}
	}
	for _, name := range []string{stepMoveSwitch, stepMoveClean} {
		if stepState(t, name) == task.StepRunning {
			return false
		}
	}
	return !switchPassed(t.Detail)
}

// undistributeCancellable: the move first is cancellable as a move is, and the task between the move first and the making of the
// schema local too (the schema then stays distributed on its new group); the making local is not.
func undistributeCancellable(t task.Task) bool {
	switch stepState(t, stepUndistMake) {
	case task.StepRunning, task.StepDone:
		return false
	}
	if stepState(t, stepUndistMoveFirst) == task.StepRunning {
		return !switchPassed(t.Detail)
	}
	return true
}

func schemaIn(env *task.Env) (schemaInput, error) { return inputOf[schemaInput](env.Task) }

func (o *Ops) sDistCheck(ctx context.Context, env *task.Env) error {
	in, err := schemaIn(env)
	if err != nil {
		return err
	}
	return o.withShardDB(ctx, in.Database, func(c *pgx.Conn) error {
		if rows, err := o.schemaRows(ctx, c, in.Schema); err == nil && len(rows) == 1 && rows[0].Distribution != nil {
			return nil // a repeated step: it is distributed already
		}
		bl, err := inventory(ctx, c, in.Schema)
		if err != nil {
			return PGProblem(err)
		}
		if len(bl) > 0 {
			return notDistributable(bl)
		}
		return nil
	})
}

func (o *Ops) sDistIdentity(ctx context.Context, env *task.Env) error {
	in, err := schemaIn(env)
	if err != nil {
		return err
	}
	if !in.ReplicaIDFull {
		return task.Skip("not asked for")
	}
	return o.withShardDB(ctx, in.Database, func(c *pgx.Conn) error {
		return o.setIdentityFull(ctx, c, in.Schema)
	})
}

func (o *Ops) setIdentityFull(ctx context.Context, c *pgx.Conn, schema string) error {
	keyless, err := keylessTables(ctx, c, schema)
	if err != nil {
		return PGProblem(err)
	}
	for _, t := range keyless {
		if _, err := c.Exec(ctx, `ALTER TABLE `+pgx.Identifier{schema, t}.Sanitize()+` REPLICA IDENTITY FULL`); err != nil {
			return PGProblem(err)
		}
	}
	return nil
}

// sDistMake calls distribute_schema; its result is read from kingres.dist_schema first, so a repeat after an interruption does not call it twice.
func (o *Ops) sDistMake(ctx context.Context, env *task.Env) error {
	in, err := schemaIn(env)
	if err != nil {
		return err
	}
	return o.withShardDB(ctx, in.Database, func(c *pgx.Conn) error {
		if rows, err := o.schemaRows(ctx, c, in.Schema); err == nil && len(rows) == 1 && rows[0].Distribution != nil {
			return nil
		}
		var g any
		if in.GroupID != nil {
			g = *in.GroupID
		}
		_, err := c.Exec(ctx, `SELECT kingres.distribute_schema($1::regnamespace, $2::int)`, quoteIdent(in.Schema), g)
		return o.settledAfter(ctx, c, in.Schema, true, schemaError(err))
	})
}

// settledAfter turns the failure of a call of the extension into success when the schema is in the state the call was to make it: a
// backend of a dead leader may have been between its prepare and its commit when the step was recovered, and the call that ran again
// then waited for it and found the work done ("already distributed"). Any other failure stays.
func (o *Ops) settledAfter(ctx context.Context, c *pgx.Conn, schema string, wantDistributed bool, callErr error) error {
	if callErr == nil {
		return nil
	}
	rows, err := o.schemaRows(context.WithoutCancel(ctx), c, schema)
	if err == nil && len(rows) == 1 && (rows[0].Distribution != nil) == wantDistributed {
		if o.Log != nil {
			o.Log.Info("a call of the extension failed but the schema is in the state it was to make", "schema", schema, "error", callErr)
		}
		return nil
	}
	return callErr
}

// recoverDistribute and recoverUndistribute are the Recover of the two steps that call the extension once and must not call it twice:
// they read whether the call took effect (the schema is distributed, or local). When it did not, what the cut call left prepared on
// the other nodes is settled first (kingres.recover_prepared_transactions), because a prepared transaction holds locks that would
// make the call that runs again wait or fail.
func (o *Ops) recoverDistribute(ctx context.Context, env *task.Env) (bool, error) {
	return o.recoverCall(ctx, env, true)
}

func (o *Ops) recoverUndistribute(ctx context.Context, env *task.Env) (bool, error) {
	return o.recoverCall(ctx, env, false)
}

func (o *Ops) recoverCall(ctx context.Context, env *task.Env, wantDistributed bool) (bool, error) {
	in, err := schemaIn(env)
	if err != nil {
		return false, err
	}
	took := false
	err = o.withShardDB(ctx, in.Database, func(c *pgx.Conn) error {
		rows, err := o.schemaRows(ctx, c, in.Schema)
		if err != nil {
			return err
		}
		if len(rows) == 0 && !wantDistributed {
			// a schema made local on another group is not on the map group any more
			took = true
			return nil
		}
		if len(rows) != 1 {
			return errors.New("the schema is not there")
		}
		if (rows[0].Distribution != nil) == wantDistributed {
			took = true
			return nil
		}
		if o.Log != nil {
			o.Log.Info("a schema step that was cut off has not taken effect; settling what it left prepared", "database", in.Database, "schema", in.Schema, "distribute", wantDistributed)
		}
		if _, err := c.Exec(ctx, `SELECT kingres.recover_prepared_transactions()`); err != nil {
			return PGProblem(err)
		}
		return nil
	})
	return took, err
}

// schemaError maps an error of the extension to a problem.
func schemaError(err error) error {
	if err == nil {
		return nil
	}
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		msg := pe.Message
		switch {
		case strings.Contains(msg, "schema_pinned"):
			return problem.New(problem.SchemaPinned, msg)
		case strings.Contains(msg, "not supported") || strings.Contains(msg, "cannot be distributed"):
			return problem.New(problem.SchemaNotDistributable, msg)
		case strings.Contains(msg, "already"):
			return problem.New(problem.Conflict, msg)
		}
		return fmt.Errorf("%s", msg)
	}
	return err
}

func (o *Ops) sVerifyDistributed(ctx context.Context, env *task.Env) error {
	in, err := schemaIn(env)
	if err != nil {
		return err
	}
	return o.withShardDB(ctx, in.Database, func(c *pgx.Conn) error {
		rows, err := o.schemaRows(ctx, c, in.Schema)
		if err != nil {
			return err
		}
		if len(rows) != 1 || rows[0].Distribution == nil {
			return errors.New("the schema is not distributed after the step")
		}
		return nil
	})
}

func (o *Ops) sVerifyLocal(ctx context.Context, env *task.Env) error {
	in, err := schemaIn(env)
	if err != nil {
		return err
	}
	return o.withShardDB(ctx, in.Database, func(c *pgx.Conn) error {
		rows, err := o.schemaRows(ctx, c, in.Schema)
		if err != nil {
			return err
		}
		if len(rows) == 1 && rows[0].Distribution != nil {
			return errors.New("the schema is still distributed after the step")
		}
		return nil
	})
}

func (o *Ops) sMoveCheck(ctx context.Context, env *task.Env) error {
	in, err := schemaIn(env)
	if err != nil {
		return err
	}
	return o.withShardDB(ctx, in.Database, func(c *pgx.Conn) error {
		s, err := o.distributedRow(ctx, c, in.Database, in.Schema)
		if err != nil {
			return err
		}
		if s.Distribution.ShardGroupID == in.Target && s.Distribution.State == "active" {
			return nil // moved already (a repeated step)
		}
		return o.refusePin(ctx, c, s.Distribution.ColocationID)
	})
}

// sMoveRun makes the schema's tables movable when asked, calls move_schema in a session of its own and follows the extension's record.
func (o *Ops) sMoveRun(ctx context.Context, env *task.Env) error {
	in, err := schemaIn(env)
	if err != nil {
		return err
	}
	return o.runSchemaMove(ctx, env, in)
}

// moveRecord is the extension's record of a move of the schema's bucket (kingres.bucket_move).
type moveRecord struct {
	ID            int64
	Phase, Detail string
	Source        int32
	Target        int32
}

// terminal: the move has ended (cleaning is the end of the work of a move: the schema is served by the target and the old copy is
// emptied after kingres_shard.move_cleanup_delay).
func (m moveRecord) terminal() bool {
	switch m.Phase {
	case "cleaning", "done", "failed", "cancelled":
		return true
	}
	return false
}

// movePhases that a cancel can still stop (the extension refuses cancel_bucket_move in the others).
func (m moveRecord) stoppable() bool {
	return m.Phase == "preparing" || m.Phase == "copying" || m.Phase == "catching_up"
}

const (
	// moveWaitMax is how long a run waits for the extension to settle a move that an earlier run left (the session that ran it
	// is gone; resume_bucket_moves undoes it or finishes it).
	moveWaitMax = 10 * time.Minute
	// moveEndWait is how long a run waits for the call to return after the record has ended (the call drops the replication; what
	// it leaves, the recovery repeats).
	moveEndWait = 3 * time.Minute
	// moveTries is how many times a move that ended without effect (failed) is started again.
	moveTries = 3
)

// latestMove reads the newest record of the schema's colocation group with an id above after.
func latestMove(ctx context.Context, c *pgx.Conn, colo int32, after int64) (moveRecord, bool, error) {
	var m moveRecord
	err := c.QueryRow(ctx, `SELECT move_id, phase, coalesce(detail, ''), source_group, target_group FROM kingres.bucket_move
		WHERE colocation_id = $1 AND move_id > $2 ORDER BY move_id DESC LIMIT 1`, colo, after).Scan(&m.ID, &m.Phase, &m.Detail, &m.Source, &m.Target)
	if errors.Is(err, pgx.ErrNoRows) {
		return m, false, nil
	}
	return m, err == nil, err
}

// moveCancelled is the end of a move that a cancel stopped before its switch and the extension undid: the task fails with the code
// cancelled and says that the schema is where it was.
type moveCancelled struct{ group int32 }

func (e moveCancelled) Error() string {
	return fmt.Sprintf("The move was cancelled before its switch and undone: the schema is unchanged, on shard group %d.", e.group)
}
func (moveCancelled) TaskCode() string    { return task.CodeCanceled }
func (moveCancelled) TaskRetryable() bool { return false }

// runSchemaMove makes the schema moved to the target and returns when the switch is done (the phase is cleaning or done). It is the
// work of the step "Copy the data" and, for make local, of "Move first". It is safe to run again after a leader change: a schema that
// is on its target is done, a move that an earlier run left is followed (after the extension was asked to settle it), and a move that
// ended without effect is started again, up to three times.
//
// A cancel is the step's own business (the engine looks at the mark only between steps, and the step is not interruptible: the engine
// would end the task as cancelled while the extension goes on). Before the switch the step asks the extension to stop, and the
// answer is truthful: the move was undone (the task fails with the code cancelled), or it came too late and the task goes on to its
// end with the note in its detail.
func (o *Ops) runSchemaMove(ctx context.Context, env *task.Env, in schemaInput) error {
	var colo int32
	var onTarget bool
	if err := o.withShardDB(ctx, in.Database, func(c *pgx.Conn) error {
		s, err := o.distributedRow(ctx, c, in.Database, in.Schema)
		if err != nil {
			return err
		}
		colo = s.Distribution.ColocationID
		onTarget = s.Distribution.ShardGroupID == in.Target && s.Distribution.State == "active"
		if !onTarget && in.ReplicaIDFull {
			return o.setIdentityFull(ctx, c, in.Schema)
		}
		return nil
	}); err != nil {
		return err
	}
	if onTarget {
		return nil
	}
	watch, err := o.Service.ServiceConn(ctx, in.Database)
	if err != nil {
		return connectProblem(err)
	}
	defer func() { _ = watch.Close(context.WithoutCancel(ctx)) }()
	// the watching session reads kingres.bucket_move and calls kingres.resume_bucket_moves as a superuser
	if err := requireExt(ctx, watch, extguard.Shard); err != nil {
		return err
	}
	asked := false
	for try := 0; try < moveTries; try++ {
		s, err := o.distributedRow(ctx, watch, in.Database, in.Schema)
		if err != nil {
			return err
		}
		if s.Distribution.ShardGroupID == in.Target && s.Distribution.State == "active" {
			return nil
		}
		left, found, err := latestMove(ctx, watch, colo, 0)
		if err != nil {
			return PGProblem(err)
		}
		var end moveRecord
		if found && !left.terminal() {
			// a move that an earlier run started: its session may be gone, the extension settles those
			if left.Target != in.Target {
				return problem.New(problem.SchemaMoveInProgress, fmt.Sprintf("Schema %q is already moving to shard group %d.", in.Schema, left.Target))
			}
			_, _ = watch.Exec(ctx, `SELECT kingres.resume_bucket_moves()`)
			end, err = o.followMove(ctx, env, watch, colo, left.ID-1, nil, moveWaitMax, &asked)
		} else {
			var mark int64
			if found {
				mark = left.ID
			}
			errc := make(chan error, 1)
			// The call is made in a session of its own that stays open until the move has ended: its live mark is a session
			// lock, which is how the extension tells a running move from an abandoned one.
			go func() {
				errc <- o.withShardDB(context.WithoutCancel(ctx), in.Database, func(c *pgx.Conn) error {
					_, err := c.Exec(context.WithoutCancel(ctx), `SELECT kingres.move_schema($1::regnamespace, $2::int)`, quoteIdent(in.Schema), in.Target)
					return schemaError(err)
				})
			}()
			end, err = o.followMove(ctx, env, watch, colo, mark, errc, 0, &asked)
		}
		if err != nil {
			return err
		}
		switch end.Phase {
		case "cleaning", "done":
			if asked {
				// the request was accepted and the move completed all the same: the extension looked after its switch
				_ = env.SetDetail(ctx, moveTooLateText+": the move had passed the point where it can be undone, and it completed")
			}
			return nil
		case "cancelled":
			return moveCancelled{group: end.Source}
		}
		// failed: look again, and start again unless a person asked to stop it
		if asked || try == moveTries-1 {
			return fmt.Errorf("the move failed: %s", end.Detail)
		}
	}
	return errors.New("the move did not end in a switch")
}

// followMove polls the record of the move until it has ended and the call that runs it (errc; nil when no call of this run does) has
// returned, reports the phase in the detail of the task, and asks the extension to stop the move when a cancel was requested and the
// move has not reached its switch. bound > 0 limits how long it waits for a move that nobody of this run drives. asked is set when a
// cancel request was handed to the extension.
func (o *Ops) followMove(ctx context.Context, env *task.Env, watch *pgx.Conn, colo int32, mark int64, errc <-chan error, bound time.Duration, asked *bool) (moveRecord, error) {
	var (
		last        moveRecord
		have        bool
		callErr     error
		callDone    = errc == nil
		tooLate     bool
		lastText    string
		started     = time.Now()
		endSeen     time.Time
		callEnd     = time.Now()
		nextResume  time.Time
		cancelFails int
	)
	resumeEvery := o.resumeInterval(ctx, watch)
	for {
		if callDone && have && last.terminal() {
			return last, nil
		}
		if bound > 0 && time.Since(started) > bound {
			return last, fmt.Errorf("the move is still not settled after %s; run kingres.resume_bucket_moves() on the map group primary and check kingres.check_cluster()", bound)
		}
		select {
		case <-ctx.Done():
			// a lost lease or a shutdown, not a cancel: the extension owns the move and the next holder of the task follows it
			return last, ctx.Err()
		case callErr = <-errc:
			callDone = true
			callEnd = time.Now()
			errc = nil
		case <-time.After(time.Second):
		}
		m, found, err := latestMove(ctx, watch, colo, mark)
		if err != nil {
			return last, PGProblem(err)
		}
		if found {
			last, have = m, true
		}
		if callDone && !have {
			// the call ended without recording a move: it refused
			if callErr != nil {
				return last, callErr
			}
			return last, errors.New("the move was not recorded")
		}
		if !have {
			continue
		}
		if callDone && !last.terminal() {
			// Nobody of this run drives the move any more (its call returned, or it was left by a dead leader): the extension settles it
			// (resume_bucket_moves undoes a move whose session is gone, and skips one whose last change is younger than twice its node
			// timeout), so the control plane asks again and again instead of waiting for the recovery worker, and gives up after a bound.
			if time.Since(callEnd) > moveWaitMax {
				return last, fmt.Errorf("the move is not settled %s after its call ended (%s); run kingres.resume_bucket_moves() on the map group primary and check kingres.check_cluster()", moveWaitMax, errText(callErr))
			}
			if time.Now().After(nextResume) {
				if o.Log != nil {
					o.Log.Info("asking the extension to settle a move that no call of this run drives", "colocation", colo, "phase", last.Phase)
				}
				_, _ = watch.Exec(ctx, `SELECT kingres.resume_bucket_moves()`)
				nextResume = time.Now().Add(resumeEvery)
			}
		}
		if last.terminal() && !callDone {
			// the switch is done; what the call still does is the dropping of the replication, which the recovery repeats
			if endSeen.IsZero() {
				endSeen = time.Now()
			} else if time.Since(endSeen) > moveEndWait {
				return last, nil
			}
		}
		if !*asked && !tooLate && env.CancelRequested(ctx) {
			if last.stoppable() {
				if _, err := watch.Exec(ctx, `SELECT kingres.cancel_bucket_move($1)`, last.ID); err != nil {
					if cancelRefused(err) {
						// the extension refuses a move that is switching (or made): too late, the move goes on and the task with it
						tooLate = true
					} else if cancelFails++; cancelFails >= 5 {
						// not a refusal: the request could not be handed over; it is said, and the task does not pretend it was too late
						_ = env.SetDetail(ctx, "The cancel request could not be handed to the extension: "+err.Error())
						cancelFails = 0
					}
				} else {
					*asked = true
				}
			} else if !last.terminal() {
				tooLate = true // switching
			}
		}
		text := last.Phase + ": " + movePhaseText(last)
		if tooLate {
			text = moveTooLateText + " (" + text + ")"
		}
		if text != lastText {
			lastText = text
			_ = env.SetDetail(ctx, text)
		}
	}
}

func movePhaseText(m moveRecord) string {
	switch m.Phase {
	case "preparing":
		return "checking and recording the move"
	case "copying":
		return fmt.Sprintf("copying the rows to shard group %d", m.Target)
	case "catching_up":
		return fmt.Sprintf("the copy on shard group %d catches up with the changes", m.Target)
	case "switching":
		return "the writers of this schema wait for the switch"
	case "cleaning":
		return "the schema is served by its new group; the old copy is emptied after kingres_shard.move_cleanup_delay"
	}
	if m.Detail != "" {
		return m.Detail
	}
	return "move " + m.Phase
}

// sMoveSwitch checks that the schema is on the target (the switch happens inside the extension's call).
func (o *Ops) sMoveSwitch(ctx context.Context, env *task.Env) error {
	in, err := schemaIn(env)
	if err != nil {
		return err
	}
	return o.withShardDB(ctx, in.Database, func(c *pgx.Conn) error {
		s, err := o.distributedRow(ctx, c, in.Database, in.Schema)
		if err != nil {
			return err
		}
		if s.Distribution.ShardGroupID != in.Target {
			return fmt.Errorf("the schema is on shard group %d, not on %d", s.Distribution.ShardGroupID, in.Target)
		}
		return nil
	})
}

// sMoveClean does not wait for the old copy to be emptied (after move_cleanup_delay): the schema is served by the target.
func (o *Ops) sMoveClean(ctx context.Context, env *task.Env) error {
	if strings.HasPrefix(env.Task.Detail, moveTooLateText) {
		return nil // the note of a cancel request that came too late stays on the task
	}
	_ = env.SetDetail(ctx, "The old copy is emptied by the sharding extension after kingres_shard.move_cleanup_delay.")
	return nil
}

func (o *Ops) sUndistCheck(ctx context.Context, env *task.Env) error {
	in, err := schemaIn(env)
	if err != nil {
		return err
	}
	return o.withShardDB(ctx, in.Database, func(c *pgx.Conn) error {
		rows, err := o.schemaRows(ctx, c, in.Schema)
		if err != nil {
			return err
		}
		if len(rows) == 1 && rows[0].Distribution == nil {
			return nil // local already (a repeated step)
		}
		if len(rows) == 1 {
			return o.refusePin(ctx, c, rows[0].Distribution.ColocationID)
		}
		return problem.New(problem.NotFound, "The schema does not exist.")
	})
}

func (o *Ops) sUndistMoveFirst(ctx context.Context, env *task.Env) error {
	in, err := schemaIn(env)
	if err != nil {
		return err
	}
	if !in.HasTarget {
		return task.Skip("the schema is made local where it is")
	}
	if err := o.runSchemaMove(ctx, env, in); err != nil {
		return err
	}
	// A cancel between this step and the making local leaves the schema distributed on its new group; the task says so.
	_ = env.SetDetail(ctx, fmt.Sprintf("The schema has moved to shard group %d; if the task is cancelled before it is made local, it stays distributed there.", in.Target))
	return nil
}

func (o *Ops) sUndistMake(ctx context.Context, env *task.Env) error {
	in, err := schemaIn(env)
	if err != nil {
		return err
	}
	return o.withShardDB(ctx, in.Database, func(c *pgx.Conn) error {
		if rows, err := o.schemaRows(ctx, c, in.Schema); err == nil && len(rows) == 1 && rows[0].Distribution == nil {
			return nil
		}
		if in.HasTarget {
			if err := finishCleaning(ctx, c, in.Colocation); err != nil {
				return err
			}
		}
		_, err := c.Exec(ctx, `SELECT kingres.undistribute_schema($1::regnamespace)`, quoteIdent(in.Schema))
		return o.settledAfter(ctx, c, in.Schema, false, schemaError(err))
	})
}

// SchemaSubjectID is the subject id of a schema task.
func SchemaSubjectID(db, schema string) string { return db + "/" + schema }

// resumeInterval is how often a task that waits for the extension to settle a move asks it to: twice the node timeout of the
// extension (it leaves a move in preparing or switching alone while its last change is younger than that) and a second.
func (o *Ops) resumeInterval(ctx context.Context, c *pgx.Conn) time.Duration {
	var sec int
	if err := c.QueryRow(ctx, `SELECT setting::int FROM pg_settings WHERE name = 'kingres_shard.node_timeout'`).Scan(&sec); err != nil || sec <= 0 {
		sec = 10
	}
	return time.Duration(2*sec+1) * time.Second
}

func errText(err error) string {
	if err == nil {
		return "no error"
	}
	return err.Error()
}

// cancelRefused tells the refusal of cancel_bucket_move for a move that is switching or made (SQLSTATE 55000, object_not_in_prerequisite_state)
// from a failure of the call itself (a lost connection, a timeout): only the first is "too late"; the second is tried again.
func cancelRefused(err error) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && pe.Code == "55000"
}

// finishCleaning ends at once the cleaning of the old copy that the move first of a make local left: the extension refuses to make a
// schema local while a move of it is in the phase cleaning, and its delay is ten minutes by default. It is the call that
// kingres.cleanup_moved_buckets makes, for the moves of this colocation group only, so that other schemas keep their delay. A
// cleaning that an earlier move left was refused when the request arrived (cleaningMove), so what is found here is this task's own.
func finishCleaning(ctx context.Context, c *pgx.Conn, colocation int32) error {
	rows, err := c.Query(ctx, `SELECT move_id FROM kingres.bucket_move WHERE colocation_id = $1 AND phase = 'cleaning' ORDER BY move_id`, colocation)
	if err != nil {
		return PGProblem(err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil || len(ids) == 0 {
		return PGProblem(err)
	}
	var self int32
	if err := c.QueryRow(ctx, `SELECT node_id FROM kingres.local_node`).Scan(&self); err != nil {
		return PGProblem(err)
	}
	for _, id := range ids {
		if _, err := c.Exec(ctx, `SELECT kingres.internal_exec_scalar($1, $2, 20000)`, self, fmt.Sprintf("SELECT kingres.internal_continue_cleaning(%d, false, true)", id)); err != nil {
			return fmt.Errorf("ending the cleaning of move %d: %w", id, err)
		}
	}
	return nil
}
