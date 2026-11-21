// SPDX-License-Identifier: AGPL-3.0-only

package ops

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/sqlx"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/sysdb"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/task"
)

// Task kinds of the distributed schemas (contract 0.15.0). The subject is type schema, id "database/schema".
const (
	KindSchemaDistribute   = "schema.distribute"
	KindSchemaMove         = "schema.move"
	KindSchemaUndistribute = "schema.undistribute"
)

// The names of the steps (the contract's text and the tests name them).
const (
	StepSchemaCheck     = "Check the schema"
	StepSchemaIdentity  = "Set REPLICA IDENTITY FULL"
	StepSchemaDistrib   = "Make the schema distributed"
	StepSchemaVerify    = "Verify on every server"
	StepSchemaMoveCheck = "Check the move"
	StepSchemaCopy      = "Copy the data"
	StepSchemaSwitch    = "Switch"
	StepSchemaCleanup   = "Clean up the old copy"
	StepSchemaMoveFirst = "Move first"
	StepSchemaLocal     = "Make the schema local"
)

// schemaParams is the input of the three kinds.
type schemaParams struct {
	Database string `json:"database"`
	Schema   string `json:"schema"`
	// ShardGroup is the requested group of a distribution, the target of a move, or the group an undistribution moves the
	// schema to first.
	ShardGroup *int32 `json:"shardGroupId,omitempty"`
	// SetIdentity runs ALTER TABLE ... REPLICA IDENTITY FULL on the tables that have no key.
	SetIdentity bool `json:"setReplicaIdentityFull,omitempty"`
}

func schemaSubject(db, schema string) string { return db + "/" + schema }

// schemaExt is what a database has of the sharding extension.
type schemaExt struct {
	catalog bool   // the shard catalog exists (sharding is enabled)
	schemas bool   // kingres_shard 0.8: distribute_schema and the rest exist
	mode    string // row, schema or both ("" without 0.8)
}

func readSchemaExt(ctx context.Context, x execer) (schemaExt, error) {
	var e schemaExt
	if err := x.QueryRow(ctx, `SELECT to_regclass('kingres.node') IS NOT NULL, to_regprocedure('kingres.distribute_schema(regnamespace,integer)') IS NOT NULL`).Scan(&e.catalog, &e.schemas); err != nil {
		return e, err
	}
	if e.schemas {
		if err := x.QueryRow(ctx, `SELECT mode FROM kingres.sharding_settings`).Scan(&e.mode); err != nil {
			return e, err
		}
	}
	return e, nil
}

// need refuses what the database cannot do: no sharding is a conflict with the state, an extension before 0.8 is not supported.
func (e schemaExt) need(db string) error {
	if !e.catalog {
		return problem.New(problem.Conflict, fmt.Sprintf("Sharding is not enabled on database %q.", db))
	}
	if !e.schemas {
		return problem.New(problem.UnsupportedOp, "Distributed schemas need kingres_shard 0.8 in database "+db+"; run ALTER EXTENSION kingres_shard UPDATE.")
	}
	return nil
}

// schemaFact is what the catalog says about one schema.
type schemaFact struct {
	exists      bool
	distributed bool
	colo        int32
	group       int32
	node        int32 // the primary node of the owner group; 0 when it has none
	state       string
}

func readSchemaFact(ctx context.Context, x execer, schema string) (schemaFact, error) {
	var f schemaFact
	if err := x.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_namespace WHERE nspname = $1)`, schema).Scan(&f.exists); err != nil || !f.exists {
		return f, err
	}
	var colo, group, node *int32
	var state *string
	err := x.QueryRow(ctx, `SELECT s.colocation_id, s.group_id, s.node_id, s.state FROM kingres.schemas s WHERE s.schema_name = $1`, schema).Scan(&colo, &group, &node, &state)
	if errors.Is(err, pgx.ErrNoRows) {
		return f, nil
	}
	if err != nil {
		return f, err
	}
	f.distributed = colo != nil
	if colo != nil {
		f.colo = *colo
	}
	if group != nil {
		f.group = *group
	}
	if node != nil {
		f.node = *node
	}
	if state != nil {
		f.state = *state
	}
	return f, nil
}

// nodeScalar runs a statement on a node (kingres.internal_exec_scalar, as the extension does) and returns its first value.
func nodeScalar(ctx context.Context, x execer, node int32, sql string) (*string, error) {
	var v *string
	err := x.QueryRow(ctx, `SELECT kingres.internal_exec_scalar($1, $2, 20000)`, node, sql).Scan(&v)
	return v, err
}

// schemaPinned asks the owner node why the schema cannot move or become local (a ledger table); "" when it can.
func schemaPinned(ctx context.Context, x execer, f schemaFact) (string, error) {
	if !f.distributed || f.node == 0 {
		return "", nil
	}
	v, err := nodeScalar(ctx, x, f.node, "SELECT kingres.internal_schema_pinned("+strconv.Itoa(int(f.colo))+")")
	if err != nil || v == nil {
		return "", err
	}
	return *v, nil
}

// unfinishedMove tells whether the extension has a move of the schema's bucket that has not ended.
func unfinishedMove(ctx context.Context, x execer, colo int32) (bool, error) {
	var b bool
	err := x.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM kingres.bucket_move WHERE colocation_id = $1 AND bucket = 0
		AND phase IN ('preparing', 'copying', 'catching_up', 'switching'))`, colo).Scan(&b)
	return b, err
}

// groupCanHold tells whether a shard group has an active primary that should hold shards.
func groupCanHold(ctx context.Context, x execer, g int32) (bool, error) {
	var b bool
	err := x.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM kingres.node WHERE group_id = $1 AND role = 'primary' AND is_active AND should_have_shards)`, g).Scan(&b)
	return b, err
}

// schemaBlockers lists what keeps a local schema from being distributed: the objects that schema_inventory does not
// support, and what the function that distributes refuses outside the inventory (the schema public).
func schemaBlockers(ctx context.Context, x execer, schema string) ([]api.SchemaBlocker, error) {
	rows, err := x.Query(ctx, `SELECT i.object_kind, i.object_name, coalesce(i.reason, '')
		FROM kingres.schema_inventory((SELECT n.oid::regnamespace FROM pg_catalog.pg_namespace n WHERE n.nspname = $1)) i
		WHERE NOT i.supported ORDER BY i.object_kind, i.object_name`, schema)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (api.SchemaBlocker, error) {
		var b api.SchemaBlocker
		err := r.Scan(&b.ObjectKind, &b.ObjectName, &b.Reason)
		return b, err
	})
	if err != nil {
		return nil, err
	}
	if schema == "public" {
		out = append(out, api.SchemaBlocker{ObjectKind: "schema", ObjectName: "public", Reason: "the schema public cannot be distributed"})
	}
	return out, nil
}

// blockerProblem is the 422 of a schema that cannot be distributed: the blockers are the entries of errors.
func blockerProblem(schema string, bl []api.SchemaBlocker) *problem.Problem {
	errs := make([]problem.FieldError, 0, len(bl))
	for _, b := range bl {
		errs = append(errs, problem.FieldError{Field: b.ObjectName, Message: b.ObjectKind + " " + b.ObjectName + ": " + b.Reason, Code: b.Reason})
	}
	p := problem.New(problem.SchemaNotDistributable, fmt.Sprintf("Schema %q cannot be distributed: %d object(s) are not supported.", schema, len(bl)))
	p.Errors = errs
	return p
}

// noKeyTables names the tables of a distributed schema that a move cannot replicate (no primary key and no full replica
// identity, or a replica identity of nothing), read on the owner node.
func noKeyTables(ctx context.Context, x execer, f schemaFact) ([]string, error) {
	if f.node == 0 {
		return nil, nil
	}
	v, err := nodeScalar(ctx, x, f.node, `SELECT coalesce(pg_catalog.string_agg(c.oid::pg_catalog.regclass::pg_catalog.text, ', ' ORDER BY c.oid::pg_catalog.regclass::pg_catalog.text), '')
		FROM kingres.dist_table d JOIN pg_catalog.pg_class c ON c.oid = d.relid::pg_catalog.oid
		WHERE d.colocation_id = `+strconv.Itoa(int(f.colo))+` AND d.method = 'schema' AND c.relkind = 'r'
		  AND (c.relreplident = 'n' OR (c.relreplident = 'd' AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_index i WHERE i.indrelid = c.oid AND i.indisprimary)))`)
	if err != nil || v == nil || *v == "" {
		return nil, err
	}
	return strings.Split(*v, ", "), nil
}

// rebalanceRunning and schemaTaskRunning read the task table of the system database.
func (o *Ops) rebalanceRunning(ctx context.Context) (bool, error) {
	var b bool
	err := o.asService(ctx, sysdb.Database, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM kingres_cp.task WHERE kind = 'rebalance' AND state IN ('pending', 'running'))`).Scan(&b)
	})
	return b, err
}

func (o *Ops) schemaTaskRunning(ctx context.Context, db, schema string) (bool, error) {
	var b bool
	err := o.asService(ctx, sysdb.Database, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM kingres_cp.task WHERE subject_type = 'schema' AND subject_id = $1 AND state IN ('pending', 'running', 'paused'))`,
			schemaSubject(db, schema)).Scan(&b)
	})
	return b, err
}

// validSchemaNames refuses a database or schema name that cannot name anything (the 404 of a name PostgreSQL would not accept).
func validSchemaNames(db, schema string) error {
	if _, err := sqlx.NewIdent(db); err != nil {
		return problem.New(problem.NotFound, "No such database.")
	}
	if _, err := sqlx.NewIdent(schema); err != nil {
		return problem.New(problem.NotFound, "No such schema.")
	}
	return nil
}

// checkDistributable is the check of distributeSchema, made twice (when the request arrives and as the first step).
func checkDistributable(ctx context.Context, x execer, db, schema string, group *int32) error {
	ext, err := readSchemaExt(ctx, x)
	if err != nil {
		return err
	}
	if err := ext.need(db); err != nil {
		return err
	}
	f, err := readSchemaFact(ctx, x, schema)
	if err != nil {
		return err
	}
	if !f.exists {
		return problem.New(problem.NotFound, fmt.Sprintf("Schema %q does not exist in database %q.", schema, db))
	}
	if f.distributed {
		return problem.New(problem.Conflict, fmt.Sprintf("Schema %q is already distributed.", schema))
	}
	if ext.mode == "row" {
		return problem.New(problem.Conflict, fmt.Sprintf("Database %q is in the sharding mode row, which has no distributed schemas; change the mode to schema or both first.", db))
	}
	bl, err := schemaBlockers(ctx, x, schema)
	if err != nil {
		return err
	}
	if len(bl) > 0 {
		return blockerProblem(schema, bl)
	}
	if group != nil {
		ok, err := groupCanHold(ctx, x, *group)
		if err != nil {
			return err
		}
		if !ok {
			return problem.Validation("The request is invalid.", field("/shardGroupId", "is not a shard group whose primary should hold shards", "not_found"))
		}
	}
	return nil
}

// PrepareDistributeSchema checks a distributeSchema request and returns the task input.
func (o *Ops) PrepareDistributeSchema(ctx context.Context, db, schema string, req api.DistributeSchemaRequest) (any, error) {
	if err := validSchemaNames(db, schema); err != nil {
		return nil, err
	}
	if req.ShardGroupID != nil && *req.ShardGroupID < 0 {
		return nil, problem.Validation("The request is invalid.", field("/shardGroupId", "must be 0 or more", "out_of_range"))
	}
	err := o.asService(ctx, db, func(tx pgx.Tx) error { return checkDistributable(ctx, tx, db, schema, req.ShardGroupID) })
	if err != nil {
		return nil, wrap(err)
	}
	return schemaParams{Database: db, Schema: schema, ShardGroup: req.ShardGroupID, SetIdentity: req.SetReplicaIdentityFull}, nil
}

// PrepareMoveSchema checks a moveSchema request and returns the task input.
func (o *Ops) PrepareMoveSchema(ctx context.Context, db, schema string, req api.MoveSchemaRequest) (any, error) {
	if err := validSchemaNames(db, schema); err != nil {
		return nil, err
	}
	if req.TargetShardGroupID == nil {
		return nil, problem.Validation("The request is invalid.", field("/targetShardGroupId", "is required", "required"))
	}
	if *req.TargetShardGroupID < 0 {
		return nil, problem.Validation("The request is invalid.", field("/targetShardGroupId", "must be 0 or more", "out_of_range"))
	}
	p := schemaParams{Database: db, Schema: schema, ShardGroup: req.TargetShardGroupID, SetIdentity: req.SetReplicaIdentityFull}
	if err := o.checkMove(ctx, p, true); err != nil {
		return nil, err
	}
	return p, nil
}

// checkMove is the check of a move (at the request and as the first step). request is true at the request, where a task of the
// same subject that is running is the second move; as a step the task itself is that task.
func (o *Ops) checkMove(ctx context.Context, p schemaParams, request bool) error {
	if request {
		if running, err := o.rebalanceRunning(ctx); err != nil {
			return wrap(err)
		} else if running {
			return problem.New(problem.RebalanceInProgress, "A rebalance is running; wait until it has finished.")
		}
		if running, err := o.schemaTaskRunning(ctx, p.Database, p.Schema); err != nil {
			return wrap(err)
		} else if running {
			return problem.New(problem.SchemaMoveInProgress, fmt.Sprintf("Schema %q has an unfinished task; wait until it has finished.", p.Schema))
		}
	}
	return wrap(o.asService(ctx, p.Database, func(tx pgx.Tx) error {
		ext, err := readSchemaExt(ctx, tx)
		if err != nil {
			return err
		}
		if err := ext.need(p.Database); err != nil {
			return err
		}
		f, err := readSchemaFact(ctx, tx, p.Schema)
		if err != nil {
			return err
		}
		if !f.exists {
			return problem.New(problem.NotFound, fmt.Sprintf("Schema %q does not exist in database %q.", p.Schema, p.Database))
		}
		if !f.distributed {
			return problem.New(problem.Conflict, fmt.Sprintf("Schema %q is not distributed; only a distributed schema moves.", p.Schema))
		}
		moving, err := unfinishedMove(ctx, tx, f.colo)
		if err != nil {
			return err
		}
		if moving || f.state != "active" {
			return problem.New(problem.SchemaMoveInProgress, fmt.Sprintf("Schema %q is moving.", p.Schema))
		}
		if pin, err := schemaPinned(ctx, tx, f); err != nil {
			return err
		} else if pin != "" {
			return problem.New(problem.SchemaPinned, fmt.Sprintf("Schema %q cannot move: %s", p.Schema, pin))
		}
		if f.group == *p.ShardGroup {
			return problem.New(problem.Conflict, fmt.Sprintf("Schema %q is on shard group %d already.", p.Schema, f.group))
		}
		ok, err := groupCanHold(ctx, tx, *p.ShardGroup)
		if err != nil {
			return err
		}
		if !ok {
			return problem.Validation("The request is invalid.", field("/targetShardGroupId", "is not a shard group whose primary should hold shards", "not_found"))
		}
		if !p.SetIdentity {
			names, err := noKeyTables(ctx, tx, f)
			if err != nil {
				return err
			}
			if len(names) > 0 {
				return problem.New(problem.Unprocessable, fmt.Sprintf("Tables without a primary key and without REPLICA IDENTITY FULL cannot be moved: %s. Add a key, or ask for setReplicaIdentityFull.", strings.Join(names, ", ")))
			}
		}
		return nil
	}))
}

// PrepareUndistributeSchema checks an undistributeSchema request and returns the task input. group is the shard group to move
// the schema to first, nil for none.
func (o *Ops) PrepareUndistributeSchema(ctx context.Context, db, schema string, group *int32) (any, error) {
	if err := validSchemaNames(db, schema); err != nil {
		return nil, err
	}
	if group != nil && *group < 0 {
		return nil, problem.Validation("The request is invalid.", field("shardGroupId", "must be 0 or more", "out_of_range"))
	}
	if running, err := o.schemaTaskRunning(ctx, db, schema); err != nil {
		return nil, wrap(err)
	} else if running {
		return nil, problem.New(problem.SchemaMoveInProgress, fmt.Sprintf("Schema %q has an unfinished task; wait until it has finished.", schema))
	}
	err := o.asService(ctx, db, func(tx pgx.Tx) error { return checkUndistributable(ctx, tx, db, schema, group) })
	if err != nil {
		return nil, wrap(err)
	}
	return schemaParams{Database: db, Schema: schema, ShardGroup: group}, nil
}

func checkUndistributable(ctx context.Context, x execer, db, schema string, group *int32) error {
	ext, err := readSchemaExt(ctx, x)
	if err != nil {
		return err
	}
	if err := ext.need(db); err != nil {
		return err
	}
	f, err := readSchemaFact(ctx, x, schema)
	if err != nil {
		return err
	}
	if !f.exists {
		return problem.New(problem.NotFound, fmt.Sprintf("Schema %q does not exist in database %q.", schema, db))
	}
	if !f.distributed {
		return problem.New(problem.Conflict, fmt.Sprintf("Schema %q is not distributed.", schema))
	}
	moving, err := unfinishedMove(ctx, x, f.colo)
	if err != nil {
		return err
	}
	if moving || f.state != "active" {
		return problem.New(problem.SchemaMoveInProgress, fmt.Sprintf("Schema %q is moving.", schema))
	}
	if pin, err := schemaPinned(ctx, x, f); err != nil {
		return err
	} else if pin != "" {
		return problem.New(problem.SchemaPinned, fmt.Sprintf("Schema %q cannot become local: %s", schema, pin))
	}
	if group != nil && *group != f.group {
		ok, err := groupCanHold(ctx, x, *group)
		if err != nil {
			return err
		}
		if !ok {
			return problem.Validation("The request is invalid.", field("shardGroupId", "is not a shard group whose primary should hold shards", "not_found"))
		}
		names, err := noKeyTables(ctx, x, f)
		if err != nil {
			return err
		}
		if len(names) > 0 {
			return problem.New(problem.Unprocessable, fmt.Sprintf("Tables without a primary key and without REPLICA IDENTITY FULL cannot be moved first: %s.", strings.Join(names, ", ")))
		}
	}
	return nil
}

// schemaDefinitions are the task definitions of the schema kinds.
func (o *Ops) schemaDefinitions() []task.Definition {
	return []task.Definition{
		{Kind: KindSchemaDistribute, Steps: []task.Step{
			{Name: StepSchemaCheck, Run: o.stepDistributeCheck},
			{Name: StepSchemaIdentity, Run: o.stepSetIdentity},
			// distribute_schema is one call that creates the schema on every node and registers it; a run that ended in it
			// reads the catalog to know whether it took effect (it did not run twice).
			{Name: StepSchemaDistrib, NonIdempotent: true, Run: o.stepDistributeSchema, Recover: o.recoverDistributed},
			{Name: StepSchemaVerify, Run: o.stepVerifyDistributed},
		}},
		{Kind: KindSchemaUndistribute, Cancellable: schemaMoveCancellable, Steps: []task.Step{
			{Name: StepSchemaCheck, Run: o.stepUndistributeCheck},
			{Name: StepSchemaMoveFirst, Run: o.stepMoveFirst},
			{Name: StepSchemaLocal, NonIdempotent: true, Run: o.stepUndistributeSchema, Recover: o.recoverUndistributed},
			{Name: StepSchemaVerify, Run: o.stepVerifyLocal},
		}},
		{Kind: KindSchemaMove, Cancellable: schemaMoveCancellable, Steps: []task.Step{
			{Name: StepSchemaMoveCheck, Run: o.stepMoveCheck},
			{Name: StepSchemaIdentity, Run: o.stepMoveIdentity},
			{Name: StepSchemaCopy, Run: o.stepCopy},
			{Name: StepSchemaSwitch, Run: o.stepSwitch},
			{Name: StepSchemaCleanup, Run: o.stepCleanup},
		}},
	}
}

// schemaMoveCancellable is the cancel guard of the kinds that move a schema: a move can be stopped until the phase switching.
// From there the extension's switch is in its two-phase commit and the move rolls forward only; a request is refused with
// task_not_cancellable, and the mark of one that arrived before is not followed.
func schemaMoveCancellable(t task.Task) bool {
	if !t.State.Active() {
		return false
	}
	for _, s := range t.Steps {
		switch s.Name {
		case StepSchemaSwitch, StepSchemaCleanup, StepSchemaLocal:
			if s.State != task.StepPending {
				return false
			}
		}
	}
	for _, p := range []string{"switching", "cleaning", "done"} {
		if strings.HasPrefix(t.Detail, p) {
			return false
		}
	}
	return true
}

func (o *Ops) schemaConn(ctx context.Context, db string) (*pgx.Conn, error) {
	if o.Service == nil {
		return nil, problem.New(problem.UnsupportedOp, "This server has no managed instance.")
	}
	return o.Service.ServiceConn(ctx, db)
}

// pgText is what a step records of an error: the message of PostgreSQL, and its detail and hint, when it is one.
func pgText(err error) string {
	var pe *pgconn.PgError
	if !errors.As(err, &pe) {
		var p *problem.Problem
		if errors.As(err, &p) {
			s := p.Detail
			if s == "" {
				s = p.Title
			}
			for _, e := range p.Errors {
				s += "; " + e.Message
			}
			return s
		}
		return err.Error()
	}
	s := pe.Message
	if pe.Detail != "" {
		s += " (" + pe.Detail + ")"
	}
	if pe.Hint != "" {
		s += " " + pe.Hint
	}
	return s
}

// ---------------------------------------------------------------- schema.distribute

func (o *Ops) stepDistributeCheck(ctx context.Context, env *task.Env) error {
	p, err := inputOf[schemaParams](env.Task)
	if err != nil {
		return err
	}
	return o.asServiceTask(ctx, p.Database, func(tx pgx.Tx) error { return checkDistributable(ctx, tx, p.Database, p.Schema, p.ShardGroup) })
}

// asServiceTask is asService for a step: a problem becomes an error whose text says what is wrong.
func (o *Ops) asServiceTask(ctx context.Context, db string, f func(tx pgx.Tx) error) error {
	err := o.asService(ctx, db, f)
	if err == nil {
		return nil
	}
	var p *problem.Problem
	if errors.As(err, &p) {
		return errors.New(pgText(err))
	}
	return err
}

func (o *Ops) stepSetIdentity(ctx context.Context, env *task.Env) error {
	p, err := inputOf[schemaParams](env.Task)
	if err != nil {
		return err
	}
	if !p.SetIdentity {
		return task.Skip("REPLICA IDENTITY FULL was not asked for")
	}
	return o.setIdentityFull(ctx, p.Database, p.Schema)
}

// setIdentityFull runs ALTER TABLE ... REPLICA IDENTITY FULL on every table of the schema that has no primary key and no full
// identity. It is the user's table that changes, so it is done only on request, and repeating it changes nothing.
func (o *Ops) setIdentityFull(ctx context.Context, db, schema string) error {
	c, err := o.schemaConn(ctx, db)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close(context.WithoutCancel(ctx)) }()
	rows, err := c.Query(ctx, `SELECT c.relname FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1 AND c.relkind IN ('r', 'p')
		  AND (c.relreplident = 'n' OR (c.relreplident IN ('d', 'i') AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_index i WHERE i.indrelid = c.oid AND i.indisprimary)))
		ORDER BY c.relname`, schema)
	if err != nil {
		return err
	}
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	for _, n := range names {
		q, err := sqlx.QualifiedIdent(schema, n)
		if err != nil {
			return err
		}
		if _, err := c.Exec(ctx, "ALTER TABLE "+q.String()+" REPLICA IDENTITY FULL"); err != nil {
			return fmt.Errorf("setting REPLICA IDENTITY FULL on %s: %s", q.String(), pgText(err))
		}
	}
	return nil
}

func (o *Ops) stepDistributeSchema(ctx context.Context, env *task.Env) error {
	p, err := inputOf[schemaParams](env.Task)
	if err != nil {
		return err
	}
	c, err := o.schemaConn(ctx, p.Database)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close(context.WithoutCancel(ctx)) }()
	var colo int32
	err = c.QueryRow(ctx, `SELECT kingres.distribute_schema((SELECT n.oid::regnamespace FROM pg_catalog.pg_namespace n WHERE n.nspname = $1), $2::int)`,
		p.Schema, p.ShardGroup).Scan(&colo)
	if err != nil {
		return fmt.Errorf("the extension refused to distribute the schema: %s", pgText(err))
	}
	return nil
}

// recoverDistributed tells, from the catalog, whether a call of distribute_schema that was cut off took effect: the schema is
// in kingres.dist_schema when its transaction committed. When it is not, the extension is asked to settle what a cut
// call left prepared on the other nodes, so that the step can run again.
func (o *Ops) recoverDistributed(ctx context.Context, env *task.Env) (bool, error) {
	p, err := inputOf[schemaParams](env.Task)
	if err != nil {
		return false, err
	}
	return o.recoverCall(ctx, p, true)
}

func (o *Ops) recoverUndistributed(ctx context.Context, env *task.Env) (bool, error) {
	p, err := inputOf[schemaParams](env.Task)
	if err != nil {
		return false, err
	}
	return o.recoverCall(ctx, p, false)
}

// recoverCall reads the catalog: wantDistributed true is the effect of distribute_schema, false the one of undistribute_schema.
func (o *Ops) recoverCall(ctx context.Context, p schemaParams, wantDistributed bool) (bool, error) {
	c, err := o.schemaConn(ctx, p.Database)
	if err != nil {
		return false, err
	}
	defer func() { _ = c.Close(context.WithoutCancel(ctx)) }()
	var in bool
	if err := c.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM kingres.dist_schema s JOIN pg_catalog.pg_namespace n ON n.oid = s.nsp::pg_catalog.oid WHERE n.nspname = $1)`, p.Schema).Scan(&in); err != nil {
		return false, err
	}
	if in == wantDistributed {
		return true, nil
	}
	// Not there: what the cut call left prepared on the other nodes is settled before the step runs again.
	if _, err := c.Exec(ctx, `SELECT kingres.recover_prepared_transactions()`); err != nil {
		return false, err
	}
	return false, nil
}

// activePrimaries are the ids of the nodes that run an active primary.
func activePrimaries(ctx context.Context, x execer) ([]int32, error) {
	rows, err := x.Query(ctx, `SELECT node_id FROM kingres.node WHERE role = 'primary' AND is_active ORDER BY node_id`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[int32])
}

// nodeSchemaState reads, on a node, whether it has the schema, how many tables it holds and whether it knows the schema as distributed.
func nodeSchemaState(ctx context.Context, x execer, node int32, schema string) (string, error) {
	id, err := sqlx.NewIdent(schema)
	if err != nil {
		return "", err
	}
	lit, err := sqlx.QuoteLiteral(id.String())
	if err != nil {
		return "", err
	}
	v, err := nodeScalar(ctx, x, node, `SELECT CASE WHEN pg_catalog.to_regnamespace(`+lit+`) IS NULL THEN 'missing' ELSE
		(SELECT pg_catalog.count(*) FROM pg_catalog.pg_class c WHERE c.relnamespace = pg_catalog.to_regnamespace(`+lit+`) AND c.relkind IN ('r', 'p') AND NOT c.relispartition)::pg_catalog.text
		|| ':' || EXISTS (SELECT 1 FROM kingres.dist_schema s WHERE s.nsp = pg_catalog.to_regnamespace(`+lit+`))::pg_catalog.text END`)
	if err != nil || v == nil {
		return "", err
	}
	return *v, nil
}

// checkClusterRows are the rows of kingres.check_cluster() that name the schema (the checks of the schema have names that start
// with schema_ or stray_schema), as text.
func checkClusterRows(ctx context.Context, x execer, schema string) ([]string, error) {
	rows, err := x.Query(ctx, `SELECT check_name, coalesce(node_id::text, ''), coalesce(object, ''), coalesce(detail, '') FROM kingres.check_cluster()`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name, node, object, detail string
		if err := rows.Scan(&name, &node, &object, &detail); err != nil {
			return nil, err
		}
		if !strings.HasPrefix(name, "schema_") && !strings.HasPrefix(name, "stray_schema") {
			continue
		}
		if !strings.Contains(object, schema) && !strings.Contains(detail, schema) {
			continue
		}
		out = append(out, fmt.Sprintf("%s (node %s) %s %s", name, node, object, detail))
	}
	return out, rows.Err()
}

func (o *Ops) stepVerifyDistributed(ctx context.Context, env *task.Env) error {
	p, err := inputOf[schemaParams](env.Task)
	if err != nil {
		return err
	}
	return o.asServiceTask(ctx, p.Database, func(tx pgx.Tx) error {
		f, err := readSchemaFact(ctx, tx, p.Schema)
		if err != nil {
			return err
		}
		if !f.distributed {
			return errors.New("the schema is not distributed in the catalog")
		}
		nodes, err := activePrimaries(ctx, tx)
		if err != nil {
			return err
		}
		var want string
		for i, n := range nodes {
			st, err := nodeSchemaState(ctx, tx, n, p.Schema)
			if err != nil {
				return fmt.Errorf("asking node %d: %w", n, err)
			}
			if !strings.HasSuffix(st, ":true") {
				return fmt.Errorf("node %d does not have the schema as a distributed schema (%s)", n, st)
			}
			if i == 0 {
				want = st
			} else if st != want {
				return fmt.Errorf("node %d has %s tables in the schema, node %d has %s", n, st, nodes[0], want)
			}
		}
		rows, err := checkClusterRows(ctx, tx, p.Schema)
		if err != nil {
			return err
		}
		if len(rows) > 0 {
			return fmt.Errorf("kingres.check_cluster() reports: %s", strings.Join(rows, "; "))
		}
		return nil
	})
}

// ---------------------------------------------------------------- schema.undistribute

func (o *Ops) stepUndistributeCheck(ctx context.Context, env *task.Env) error {
	p, err := inputOf[schemaParams](env.Task)
	if err != nil {
		return err
	}
	return o.asServiceTask(ctx, p.Database, func(tx pgx.Tx) error { return checkUndistributable(ctx, tx, p.Database, p.Schema, p.ShardGroup) })
}

func (o *Ops) stepMoveFirst(ctx context.Context, env *task.Env) error {
	p, err := inputOf[schemaParams](env.Task)
	if err != nil {
		return err
	}
	if p.ShardGroup == nil {
		return task.Skip("no shard group was named")
	}
	var f schemaFact
	if err := o.asService(ctx, p.Database, func(tx pgx.Tx) error {
		var err error
		f, err = readSchemaFact(ctx, tx, p.Schema)
		return err
	}); err != nil {
		return err
	}
	if f.distributed && f.group == *p.ShardGroup && f.state == "active" {
		return task.Skip(fmt.Sprintf("the schema is on shard group %d already", f.group))
	}
	return o.moveAndSwitch(ctx, env, p.Database, p.Schema, *p.ShardGroup)
}

func (o *Ops) stepUndistributeSchema(ctx context.Context, env *task.Env) error {
	p, err := inputOf[schemaParams](env.Task)
	if err != nil {
		return err
	}
	c, err := o.schemaConn(ctx, p.Database)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close(context.WithoutCancel(ctx)) }()
	if err := finishCleaning(ctx, c, p.Schema); err != nil {
		return err
	}
	if _, err := c.Exec(ctx, `SELECT kingres.undistribute_schema((SELECT n.oid::regnamespace FROM pg_catalog.pg_namespace n WHERE n.nspname = $1))`, p.Schema); err != nil {
		return fmt.Errorf("the extension refused to make the schema local: %s", pgText(err))
	}
	return nil
}

func (o *Ops) stepVerifyLocal(ctx context.Context, env *task.Env) error {
	p, err := inputOf[schemaParams](env.Task)
	if err != nil {
		return err
	}
	return o.asServiceTask(ctx, p.Database, func(tx pgx.Tx) error {
		nodes, err := activePrimaries(ctx, tx)
		if err != nil {
			return err
		}
		have := 0
		for _, n := range nodes {
			st, err := nodeSchemaState(ctx, tx, n, p.Schema)
			if err != nil {
				return fmt.Errorf("asking node %d: %w", n, err)
			}
			if strings.HasSuffix(st, ":true") {
				return fmt.Errorf("node %d still knows the schema as a distributed schema", n)
			}
			if st != "missing" {
				have++
			}
		}
		if have != 1 {
			return fmt.Errorf("the schema exists on %d servers, a local schema on exactly one", have)
		}
		return nil
	})
}

// ---------------------------------------------------------------- schema.move

func (o *Ops) stepMoveCheck(ctx context.Context, env *task.Env) error {
	p, err := inputOf[schemaParams](env.Task)
	if err != nil {
		return err
	}
	// the request has been checked when it arrived; a step that starts later checks the preconditions again, and a task
	// that is the one that moves the schema is not the second move
	if err := o.checkMove(ctx, p, false); err != nil {
		// a move of this task that is already done (the run that ended after the switch) is not an error
		var done bool
		_ = o.asService(ctx, p.Database, func(tx pgx.Tx) error {
			f, ferr := readSchemaFact(ctx, tx, p.Schema)
			done = ferr == nil && f.distributed && p.ShardGroup != nil && f.group == *p.ShardGroup && f.state == "active"
			return nil
		})
		if done {
			return nil
		}
		return errors.New(pgText(err))
	}
	return nil
}

func (o *Ops) stepMoveIdentity(ctx context.Context, env *task.Env) error {
	p, err := inputOf[schemaParams](env.Task)
	if err != nil {
		return err
	}
	if !p.SetIdentity {
		return task.Skip("REPLICA IDENTITY FULL was not asked for")
	}
	return o.setIdentityFull(ctx, p.Database, p.Schema)
}

func (o *Ops) stepCopy(ctx context.Context, env *task.Env) error {
	p, err := inputOf[schemaParams](env.Task)
	if err != nil {
		return err
	}
	if p.ShardGroup == nil {
		return errors.New("the task has no target shard group")
	}
	return o.moveAndSwitch(ctx, env, p.Database, p.Schema, *p.ShardGroup)
}

func (o *Ops) stepSwitch(ctx context.Context, env *task.Env) error {
	p, err := inputOf[schemaParams](env.Task)
	if err != nil {
		return err
	}
	return o.asServiceTask(ctx, p.Database, func(tx pgx.Tx) error {
		f, err := readSchemaFact(ctx, tx, p.Schema)
		if err != nil {
			return err
		}
		if !f.distributed || p.ShardGroup == nil || f.group != *p.ShardGroup || f.state != "active" {
			return fmt.Errorf("the schema is not served by shard group %d after the move", derefInt(p.ShardGroup))
		}
		return nil
	})
}

func derefInt(p *int32) int32 {
	if p == nil {
		return -1
	}
	return *p
}

// stepCleanup ends the task at the phase cleaning (the schema is served by the target; the old copy is emptied after
// kingres_shard.move_cleanup_delay) or at done when the extension has finished the cleaning.
func (o *Ops) stepCleanup(ctx context.Context, env *task.Env) error {
	p, err := inputOf[schemaParams](env.Task)
	if err != nil {
		return err
	}
	var phase, detail string
	err = o.asServiceTask(ctx, p.Database, func(tx pgx.Tx) error {
		f, err := readSchemaFact(ctx, tx, p.Schema)
		if err != nil || !f.distributed {
			return err
		}
		err = tx.QueryRow(ctx, `SELECT phase, coalesce(detail, '') FROM kingres.bucket_move WHERE colocation_id = $1 AND bucket = 0 ORDER BY move_id DESC LIMIT 1`, f.colo).Scan(&phase, &detail)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return err
	})
	if err != nil {
		return err
	}
	if phase == "cleaning" {
		_ = env.SetDetail(ctx, "cleaning: the schema is served by its new group; the old copy is emptied after kingres_shard.move_cleanup_delay")
	}
	return nil
}

// cancelledError is how a step says that it stopped on a cancel request: the task is recorded as failed with the code cancelled.
type cancelledError struct{ msg string }

func (e *cancelledError) Error() string       { return e.msg }
func (e *cancelledError) TaskCode() string    { return task.CodeCanceled }
func (e *cancelledError) TaskRetryable() bool { return false }

// moveRow is a row of kingres.bucket_move.
type moveRow struct {
	id             int64
	phase, detail  string
	source, target int32
}

func (m moveRow) terminal() bool {
	switch m.phase {
	case "cleaning", "done", "failed", "cancelled":
		return true
	}
	return false
}

func readMove(ctx context.Context, x execer, colo int32, after int64) (moveRow, bool, error) {
	var m moveRow
	err := x.QueryRow(ctx, `SELECT move_id, phase, coalesce(detail, ''), source_group, target_group FROM kingres.bucket_move
		WHERE colocation_id = $1 AND bucket = 0 AND move_id > $2 ORDER BY move_id DESC LIMIT 1`, colo, after).Scan(&m.id, &m.phase, &m.detail, &m.source, &m.target)
	if errors.Is(err, pgx.ErrNoRows) {
		return m, false, nil
	}
	return m, err == nil, err
}

const (
	movePoll       = 500 * time.Millisecond
	moveRecoverMax = 10 * time.Minute
	moveEndMax     = 3 * time.Minute
)

// moveAndSwitch moves the schema to the group and returns when the switch is done (the phase is cleaning or done). It is the
// work of the step "Copy the data" and, for undistributeSchema, of "Move first". It is safe to run again: a schema that is on
// the group is done, a move that a run left behind is followed (the recovery of the extension decides what becomes of it),
// and a move that ended without effect is started again, up to three times.
func (o *Ops) moveAndSwitch(ctx context.Context, env *task.Env, db, schema string, target int32) error {
	call, err := o.schemaConn(ctx, db)
	if err != nil {
		return err
	}
	defer func() { _ = call.Close(context.WithoutCancel(ctx)) }()
	watch, err := o.schemaConn(ctx, db)
	if err != nil {
		return err
	}
	defer func() { _ = watch.Close(context.WithoutCancel(ctx)) }()
	for attempt := 0; attempt < 3; attempt++ {
		f, err := readSchemaFact(ctx, watch, schema)
		if err != nil {
			return err
		}
		if !f.distributed {
			return errors.New("the schema is not distributed")
		}
		if f.group == target && f.state == "active" {
			return nil
		}
		if mv, found, err := readMove(ctx, watch, f.colo, 0); err != nil {
			return err
		} else if found && !mv.terminal() {
			// a move that a run left behind: followed, after the recovery of the extension was asked to look at it
			if mv.target != target {
				return fmt.Errorf("the schema is already moving to shard group %d", mv.target)
			}
			_, _ = watch.Exec(ctx, `SELECT kingres.resume_bucket_moves()`)
			end, err := o.followMove(ctx, env, watch, f, mv.id-1, nil, moveRecoverMax)
			if err != nil {
				return err
			}
			if end.phase == "cleaning" || end.phase == "done" {
				return nil
			}
			continue
		}
		var mark int64
		if err := watch.QueryRow(ctx, `SELECT coalesce(max(move_id), 0) FROM kingres.bucket_move`).Scan(&mark); err != nil {
			return err
		}
		errc := make(chan error, 1)
		callCtx, stop := context.WithCancel(context.WithoutCancel(ctx))
		go func() {
			var id int64
			errc <- call.QueryRow(callCtx, `SELECT kingres.move_schema((SELECT n.oid::regnamespace FROM pg_catalog.pg_namespace n WHERE n.nspname = $1), $2)`, schema, target).Scan(&id)
		}()
		end, ferr := o.followMove(ctx, env, watch, f, mark, errc, 0)
		stop()
		if ferr != nil {
			return ferr
		}
		if end.phase == "cleaning" || end.phase == "done" {
			return nil
		}
		// failed or cancelled by something else: look again, and start again
		if attempt == 2 {
			return fmt.Errorf("the move ended as %s: %s", end.phase, end.detail)
		}
	}
	return errors.New("the move did not end in a switch")
}

// followMove polls the move of the schema until it is terminal and the call that runs it (errc; nil when nobody of this run
// does) has returned, reporting the phase and the bytes copied, and honouring a cancel request until the phase switching.
// bound > 0 limits how long it waits for a move that nobody of this run drives.
func (o *Ops) followMove(ctx context.Context, env *task.Env, watch *pgx.Conn, f schemaFact, mark int64, errc <-chan error, bound time.Duration) (moveRow, error) {
	var (
		last      moveRow
		haveRow   bool
		callErr   error
		callDone  = errc == nil
		cancelled bool
		lastText  string
		nextBytes time.Time
		total     int64
		baseline  int64 = -1
		started         = time.Now()
		endSeen   time.Time
		callEnded time.Time
	)
	for {
		if callDone && haveRow && last.terminal() {
			break
		}
		if bound > 0 && time.Since(started) > bound {
			return last, fmt.Errorf("the move is still not settled after %s; run kingres.resume_bucket_moves() on the map group primary and check kingres.check_cluster()", bound)
		}
		select {
		case <-ctx.Done():
			return last, ctx.Err()
		case callErr = <-errc:
			callDone = true
			callEnded = time.Now()
			errc = nil
		case <-time.After(movePoll):
		}
		mv, found, err := readMove(ctx, watch, f.colo, mark)
		if err != nil {
			return last, err
		}
		if found {
			last, haveRow = mv, true
		}
		if !callEnded.IsZero() && !last.terminal() && time.Since(callEnded) > moveRecoverMax {
			// the call has ended and the move is still not over: the recovery of the extension did not settle it
			return last, fmt.Errorf("the move is not settled %s after its call ended (%s); run kingres.resume_bucket_moves() and check kingres.check_cluster()", moveRecoverMax, pgText(callErr))
		}
		if callDone && !haveRow {
			// the call ended without recording a move: it refused
			if callErr != nil {
				return last, fmt.Errorf("the extension refused to move the schema: %s", pgText(callErr))
			}
			return last, errors.New("the move was not recorded")
		}
		if !haveRow {
			continue
		}
		text := last.phase + ": " + phaseText(last)
		if text != lastText {
			lastText = text
			_ = env.SetDetail(ctx, text)
		}
		if last.phase == "copying" || last.phase == "catching_up" || last.phase == "preparing" {
			if time.Now().After(nextBytes) {
				nextBytes = time.Now().Add(2 * time.Second)
				if total == 0 {
					total, _ = o.groupBytes(ctx, watch, f.colo, last.source)
				}
				if baseline < 0 {
					baseline, _ = o.groupBytes(ctx, watch, f.colo, last.target)
				}
				if now, err := o.groupBytes(ctx, watch, f.colo, last.target); err == nil && total > 0 {
					done := now - baseline
					if done < 0 {
						done = 0
					}
					if done > total {
						done = total
					}
					_ = env.SetItems(ctx, done, total)
				}
			}
			if !cancelled && env.CancelRequested(ctx) {
				cancelled = true
				// Before the switch a move can be stopped; the extension undoes it, and the schema stays active on the source.
				if _, err := watch.Exec(ctx, `SELECT kingres.cancel_bucket_move($1)`, last.id); err != nil {
					o.Log.Warn("cancelling the move of a schema failed", "schema", f.colo, "error", err)
					cancelled = false
				}
			}
		}
		if last.terminal() && !callDone {
			if endSeen.IsZero() {
				endSeen = time.Now()
			} else if time.Since(endSeen) > moveEndMax {
				// the switch is done; what the call still does is the dropping of the replication, which the recovery repeats
				return last, nil
			}
		}
	}
	if total > 0 && (last.phase == "cleaning" || last.phase == "done") {
		_ = env.SetItems(ctx, total, total)
	}
	if last.phase == "cancelled" || (cancelled && last.phase == "failed") {
		return last, &cancelledError{msg: "The move was cancelled before the switch; the schema is active on shard group " + strconv.Itoa(int(last.source)) + "."}
	}
	if last.phase == "failed" {
		return last, fmt.Errorf("the move failed: %s", last.detail)
	}
	return last, nil
}

func phaseText(m moveRow) string {
	switch m.phase {
	case "preparing":
		return "checking and recording the move"
	case "copying":
		return fmt.Sprintf("copying the rows to shard group %d", m.target)
	case "catching_up":
		return fmt.Sprintf("the copy on shard group %d catches up with the changes", m.target)
	case "switching":
		return "the writers of this schema wait for the switch"
	case "cleaning":
		return "the schema is served by its new group"
	}
	if m.detail != "" {
		return m.detail
	}
	return "move " + m.phase
}

// groupBytes reads the size of the tables of the schema on the primary of a group (the source holds the data, the target is filling).
func (o *Ops) groupBytes(ctx context.Context, x execer, colo, group int32) (int64, error) {
	var node *int32
	if err := x.QueryRow(ctx, `SELECT kingres.internal_group_node($1)`, group).Scan(&node); err != nil || node == nil {
		return 0, err
	}
	v, err := nodeScalar(ctx, x, *node, `SELECT coalesce(pg_catalog.sum(pg_catalog.pg_total_relation_size(d.relid)), 0)::pg_catalog.text FROM kingres.dist_table d WHERE d.colocation_id = `+strconv.Itoa(int(colo)))
	if err != nil || v == nil {
		return 0, err
	}
	return strconv.ParseInt(*v, 10, 64)
}

// finishCleaning ends, at once, the cleaning of the old copy that a move of the schema left (the extension refuses to make a
// schema local while a move of it is in the phase cleaning, and its delay is ten minutes by default): the same call the
// extension's cleanup_moved_buckets makes, for the moves of this schema only, so that the old copies of other schemas keep their delay.
func finishCleaning(ctx context.Context, c *pgx.Conn, schema string) error {
	rows, err := c.Query(ctx, `SELECT m.move_id FROM kingres.bucket_move m JOIN kingres.dist_schema s ON s.colocation_id = m.colocation_id
		JOIN pg_catalog.pg_namespace n ON n.oid = s.nsp::pg_catalog.oid WHERE n.nspname = $1 AND m.phase = 'cleaning' ORDER BY m.move_id`, schema)
	if err != nil {
		return err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil || len(ids) == 0 {
		return err
	}
	var self int32
	if err := c.QueryRow(ctx, `SELECT node_id FROM kingres.local_node`).Scan(&self); err != nil {
		return err
	}
	for _, id := range ids {
		if _, err := nodeScalar(ctx, c, self, "SELECT kingres.internal_continue_cleaning("+strconv.FormatInt(id, 10)+", false, true)"); err != nil {
			return fmt.Errorf("ending the cleaning of move %d: %s", id, pgText(err))
		}
	}
	return nil
}
