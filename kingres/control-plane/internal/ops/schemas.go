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
	var dist bool
	if err := c.QueryRow(ctx, `SELECT to_regclass('kingres.schemas') IS NOT NULL`).Scan(&dist); err != nil {
		return nil, PGProblem(err)
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
	return f(c)
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
		var has bool
		if err := c.QueryRow(ctx, `SELECT to_regprocedure('kingres.schema_inventory(regnamespace)') IS NOT NULL`).Scan(&has); err != nil {
			return PGProblem(err)
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

// keylessTables are the tables that have no primary key and no replica identity index or FULL: a move cannot replicate them.
func keylessTables(ctx context.Context, c *pgx.Conn, schema string) ([]string, error) {
	rows, err := c.Query(ctx, `SELECT c.relname FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1 AND c.relkind IN ('r', 'p') AND NOT c.relispartition AND c.relreplident IN ('d', 'n')
		AND NOT EXISTS (SELECT 1 FROM pg_index i WHERE i.indrelid = c.oid AND i.indisprimary) ORDER BY c.relname COLLATE "C"`, schema)
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
	var has bool
	if err := c.QueryRow(ctx, `SELECT to_regclass('kingres.sharding_settings') IS NOT NULL`).Scan(&has); err != nil {
		return "", PGProblem(err)
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
	err := o.withDB(ctx, db, func(c *pgx.Conn) error {
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
	err := o.withDB(ctx, db, func(c *pgx.Conn) error {
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
		keyless, err := keylessTables(ctx, c, schema)
		if err != nil {
			return PGProblem(err)
		}
		if len(keyless) > 0 && !req.SetReplicaIdentityFull {
			p := problem.New(problem.Unprocessable, "Tables without a primary key or replica identity cannot be moved; set REPLICA IDENTITY FULL on them (setReplicaIdentityFull).")
			for _, t := range keyless {
				p.Errors = append(p.Errors, problem.FieldError{Field: t, Message: "no primary key and no replica identity", Code: "replica_identity_missing"})
			}
			return p
		}
		return o.refusePin(ctx, c, s.Distribution.ColocationID)
	})
	return in, err
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
	err := o.withDB(ctx, db, func(c *pgx.Conn) error {
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
			{Name: stepSchemaMake, NonIdempotent: false, Run: o.sDistMake},
			{Name: stepSchemaVerify, Run: o.sVerifyDistributed},
		}},
		{Kind: KindSchemaMove, Steps: []task.Step{
			{Name: stepMoveCheck, Run: o.sMoveCheck},
			{Name: stepMoveCopy, Interruptible: true, Run: o.sMoveRun},
			{Name: stepMoveSwitch, Run: o.sMoveSwitch},
			{Name: stepMoveClean, Run: o.sMoveClean},
		}, Cancellable: moveCancellable},
		{Kind: KindSchemaUndistribute, Steps: []task.Step{
			{Name: stepUndistCheck, Run: o.sUndistCheck},
			{Name: stepUndistMoveFirst, Run: o.sUndistMoveFirst},
			{Name: stepUndistMake, Run: o.sUndistMake},
			{Name: stepSchemaVerify, Run: o.sVerifyLocal},
		}},
	}
}

// moveCancellable: a cancel is accepted until the switch has started (design 13.1).
func moveCancellable(t task.Task) bool {
	switch stepState(t, stepMoveSwitch) {
	case task.StepDone, task.StepRunning:
		return false
	}
	return stepState(t, stepMoveClean) != task.StepDone
}

func schemaIn(env *task.Env) (schemaInput, error) { return inputOf[schemaInput](env.Task) }

func (o *Ops) sDistCheck(ctx context.Context, env *task.Env) error {
	in, err := schemaIn(env)
	if err != nil {
		return err
	}
	return o.withDB(ctx, in.Database, func(c *pgx.Conn) error {
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
	return o.withDB(ctx, in.Database, func(c *pgx.Conn) error {
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
	return o.withDB(ctx, in.Database, func(c *pgx.Conn) error {
		if rows, err := o.schemaRows(ctx, c, in.Schema); err == nil && len(rows) == 1 && rows[0].Distribution != nil {
			return nil
		}
		var g any
		if in.GroupID != nil {
			g = *in.GroupID
		}
		_, err := c.Exec(ctx, `SELECT kingres.distribute_schema($1::regnamespace, $2::int)`, quoteIdent(in.Schema), g)
		return schemaError(err)
	})
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
	return o.withDB(ctx, in.Database, func(c *pgx.Conn) error {
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
	return o.withDB(ctx, in.Database, func(c *pgx.Conn) error {
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
	return o.withDB(ctx, in.Database, func(c *pgx.Conn) error {
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

func (o *Ops) runSchemaMove(ctx context.Context, env *task.Env, in schemaInput) error {
	var colo int32
	var done bool
	if err := o.withDB(ctx, in.Database, func(c *pgx.Conn) error {
		s, err := o.distributedRow(ctx, c, in.Database, in.Schema)
		if err != nil {
			return err
		}
		colo = s.Distribution.ColocationID
		done = s.Distribution.ShardGroupID == in.Target && s.Distribution.State == "active"
		if !done && in.ReplicaIDFull {
			return o.setIdentityFull(ctx, c, in.Schema)
		}
		return nil
	}); err != nil {
		return err
	}
	if done {
		return nil
	}
	finished := make(chan error, 1)
	go func() {
		finished <- o.withDB(context.WithoutCancel(ctx), in.Database, func(c *pgx.Conn) error {
			_, err := c.Exec(context.WithoutCancel(ctx), `SELECT kingres.move_schema($1::regnamespace, $2::int)`, quoteIdent(in.Schema), in.Target)
			return schemaError(err)
		})
	}()
	for {
		select {
		case err := <-finished:
			return err
		case <-ctx.Done():
			// cancel a move that has not reached its switch; one that has runs to its end
			_ = o.withDB(context.WithoutCancel(ctx), in.Database, func(c *pgx.Conn) error {
				_, err := c.Exec(context.WithoutCancel(ctx), `SELECT kingres.cancel_bucket_move(move_id) FROM kingres.bucket_move
					WHERE colocation_id = $1 AND phase IN ('preparing', 'copying', 'catching_up')`, colo)
				return err
			})
			return ctx.Err()
		case <-time.After(time.Second):
			_ = o.withDB(ctx, in.Database, func(c *pgx.Conn) error {
				var phase, detail *string
				if err := c.QueryRow(ctx, `SELECT phase, detail FROM kingres.bucket_move WHERE colocation_id = $1 ORDER BY move_id DESC LIMIT 1`, colo).Scan(&phase, &detail); err == nil && phase != nil {
					d := *phase
					if detail != nil && *detail != "" {
						d += " (" + *detail + ")"
					}
					if *phase == "switching" {
						d = "Switching: writers of this schema wait. " + d
					}
					_ = env.SetDetail(ctx, d)
				}
				return nil
			})
		}
	}
}

// sMoveSwitch checks that the schema is on the target (the switch happens inside the extension's call).
func (o *Ops) sMoveSwitch(ctx context.Context, env *task.Env) error {
	in, err := schemaIn(env)
	if err != nil {
		return err
	}
	return o.withDB(ctx, in.Database, func(c *pgx.Conn) error {
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
	_ = env.SetDetail(ctx, "The old copy is emptied by the sharding extension after kingres_shard.move_cleanup_delay.")
	return nil
}

func (o *Ops) sUndistCheck(ctx context.Context, env *task.Env) error {
	in, err := schemaIn(env)
	if err != nil {
		return err
	}
	return o.withDB(ctx, in.Database, func(c *pgx.Conn) error {
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
	return o.runSchemaMove(ctx, env, in)
}

func (o *Ops) sUndistMake(ctx context.Context, env *task.Env) error {
	in, err := schemaIn(env)
	if err != nil {
		return err
	}
	return o.withDB(ctx, in.Database, func(c *pgx.Conn) error {
		if rows, err := o.schemaRows(ctx, c, in.Schema); err == nil && len(rows) == 1 && rows[0].Distribution == nil {
			return nil
		}
		_, err := c.Exec(ctx, `SELECT kingres.undistribute_schema($1::regnamespace)`, quoteIdent(in.Schema))
		return schemaError(err)
	})
}

// SchemaSubjectID is the subject id of a schema task.
func SchemaSubjectID(db, schema string) string { return db + "/" + schema }
