// SPDX-License-Identifier: AGPL-3.0-only

package rebalance

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/task"
)

const (
	kindFailover = "server.failover"
	kindRestore  = "replication.restorePreferredPrimaries"
)

func (e *guardEnv) setState(t *testing.T, id int64, state string) {
	t.Helper()
	if _, err := e.sys.Exec(context.Background(), `UPDATE kingres_cp.task SET state = $2, lease_holder = NULL, lease_expires_at = NULL WHERE id = $1`, id, state); err != nil {
		t.Fatal(err)
	}
}

// Migration 0011: at most one task of the failover class (a failover, a restore of the
// preferred primaries) is unfinished at any time, cluster-wide (KX003); a task that
// ended frees the class, a paused one does not.
func TestOnlyOneFailoverClassTaskIsUnfinished(t *testing.T) {
	e := newEnvNoTask(t)
	st := task.NewPGStore(e.sys)
	first, err := e.create(t, st, kindFailover, "s1")
	if err != nil {
		t.Fatal(err)
	}
	var x *task.ExcludedError
	for _, other := range []struct{ kind, subject string }{{kindFailover, "s2"}, {kindRestore, "cluster"}} {
		if _, err := e.create(t, st, other.kind, other.subject); !errors.As(err, &x) || x.Other != kindFailover || x.Problem().Code != "failover_in_progress" {
			t.Fatalf("a %s while a failover is unfinished: %v", other.kind, err)
		}
	}
	e.setState(t, first.ID, "paused")
	if _, err := e.create(t, st, kindFailover, "s2"); !errors.As(err, &x) {
		t.Fatalf("a paused failover still counts as unfinished, got %v", err)
	}
	e.setState(t, first.ID, "failed")
	second, err := e.create(t, st, kindRestore, "cluster")
	if err != nil {
		t.Fatalf("after the first ended the class is free: %v", err)
	}
	if _, err := e.create(t, st, kindFailover, "s3"); !errors.As(err, &x) || x.Other != kindRestore || x.Problem().Code != "failover_in_progress" {
		t.Fatalf("a failover while a restore is unfinished: %v", err)
	}
	e.setState(t, second.ID, "done")
}

// The restore task joins the kinds that exclude a rebalance, and a rebalance excludes it.
func TestARestoreOfPreferredPrimariesExcludesARebalanceBothWays(t *testing.T) {
	e := newEnvNoTask(t)
	st := task.NewPGStore(e.sys)
	restore, err := e.create(t, st, kindRestore, "cluster")
	if err != nil {
		t.Fatal(err)
	}
	var x *task.ExcludedError
	if _, err := e.create(t, st, Kind, ""); !errors.As(err, &x) || x.Other != kindRestore {
		t.Fatalf("a rebalance while a restore is unfinished: %v", err)
	}
	e.setState(t, restore.ID, "done")
	if _, err := e.create(t, st, Kind, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := e.create(t, st, kindRestore, "cluster"); !errors.As(err, &x) || x.Problem().Code != "rebalance_in_progress" {
		t.Fatalf("a restore while a rebalance runs: %v", err)
	}
}

// KX004: the record of an instance stop or restart is refused while an unfinished
// failover has a group whose old or new node is that instance; other instances, a
// start, and instances of a run that is over are not affected.
func TestInstanceStopIsRefusedWhileItsGroupIsBeingFailedOver(t *testing.T) {
	e := newEnvNoTask(t)
	ctx := context.Background()
	st := task.NewPGStore(e.sys)
	record := func(kind, instance string) error {
		_, err := e.sys.Exec(ctx, `INSERT INTO kingres_cp.task (kind, state, subject_type, subject_id, input, progress, created_at, updated_at, created_by)
			VALUES ($1, 'running', 'instance', $2, '{}', '{"steps":[]}', now(), now(), 'u')`, kind, instance)
		return err
	}
	finish := func(instance string) {
		if _, err := e.sys.Exec(ctx, `UPDATE kingres_cp.task SET state = 'done' WHERE subject_type = 'instance' AND subject_id = $1`, instance); err != nil {
			t.Fatal(err)
		}
	}
	if err := record("instance.stop", "7"); err != nil {
		t.Fatalf("with no failover a stop must be recorded: %v", err)
	}
	finish("7")
	fo, err := e.create(t, st, kindFailover, "s1")
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO kingres_cp.failover_run (task_id, kind, trigger) VALUES ($1, 'failover', 'manual')`,
		`INSERT INTO kingres_cp.failover_group (task_id, group_id, seq, state, old_node_id, new_node_id) VALUES ($1, 3, 1, 'frozen', 7, 12)`,
	} {
		if _, err := e.sys.Exec(ctx, q, fo.ID); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []struct{ kind, inst string }{{"instance.stop", "7"}, {"instance.restart", "12"}, {"config.restart", "7"}} {
		err := record(c.kind, c.inst)
		var pe *pgconn.PgError
		if !errors.As(err, &pe) || pe.Code != "KX004" {
			t.Errorf("%s of instance %s during the failover: %v", c.kind, c.inst, err)
		}
	}
	if err := record("instance.stop", "8"); err != nil {
		t.Errorf("another instance must not be affected: %v", err)
	}
	finish("8")
	if err := record("instance.start", "7"); err != nil {
		t.Errorf("a start is not refused: %v", err)
	}
	finish("7")
	// Through the engine's record the refusal is an ExcludedError that the REST layer answers as failover_in_progress.
	_, err = st.Record(ctx, task.Task{Kind: "instance.stop", State: task.Running, SubjectType: "instance", SubjectID: "7", Input: []byte("{}")})
	var x *task.ExcludedError
	if !errors.As(err, &x) || x.Problem().Code != "failover_in_progress" {
		t.Errorf("Record: %v", err)
	}
	// The group is done: the instance may be stopped again.
	if _, err := e.sys.Exec(ctx, `UPDATE kingres_cp.failover_group SET state = 'done' WHERE task_id = $1`, fo.ID); err != nil {
		t.Fatal(err)
	}
	if err := record("instance.stop", "7"); err != nil {
		t.Errorf("a group that is done frees its instances: %v", err)
	}
	finish("7")
	// A run that ended frees them too.
	if _, err := e.sys.Exec(ctx, `UPDATE kingres_cp.failover_group SET state = 'frozen' WHERE task_id = $1`, fo.ID); err != nil {
		t.Fatal(err)
	}
	e.setState(t, fo.ID, "done")
	if err := record("instance.stop", "7"); err != nil {
		t.Errorf("a run that ended frees its instances: %v", err)
	}
}

// FO-11: the refusal of a stop or restart during a failover covers every instance of a server of the group (a stop
// acts on every primary the server runs, not on the instance named in the task), and a config restart, which has
// no instance as its subject, is refused while a failover is unfinished.
func TestAStopOfAnyInstanceOfTheServerAndAConfigRestartAreRefusedDuringAFailover(t *testing.T) {
	e := newEnvNoTask(t)
	ctx := context.Background()
	st := task.NewPGStore(e.sys)
	for _, q := range []string{
		`CREATE SCHEMA IF NOT EXISTS kingres`,
		`CREATE TABLE kingres.node (node_id int PRIMARY KEY, host text, port int)`,
		`INSERT INTO kingres.node VALUES (7, 'old', 5432), (12, 'new', 5432), (8, 'other', 5432), (9, 'old', 5433)`,
	} {
		if _, err := e.sys.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	record := func(kind, subjectType, subject string) error {
		_, err := e.sys.Exec(ctx, `INSERT INTO kingres_cp.task (kind, state, subject_type, subject_id, input, progress, created_at, updated_at, created_by)
			VALUES ($1, 'running', $2, $3, '{}', '{"steps":[]}', now(), now(), 'u')`, kind, subjectType, subject)
		return err
	}
	if err := record("config.restart", "config", "restart"); err != nil {
		t.Fatalf("with no failover a config restart must be recorded: %v", err)
	}
	if _, err := e.sys.Exec(ctx, `UPDATE kingres_cp.task SET state = 'done' WHERE kind = 'config.restart'`); err != nil {
		t.Fatal(err)
	}
	fo, err := e.create(t, st, kindFailover, "s1")
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO kingres_cp.failover_run (task_id, kind, trigger) VALUES ($1, 'failover', 'manual')`,
		`INSERT INTO kingres_cp.failover_group (task_id, group_id, seq, state, old_server, old_node_id, new_server, new_node_id) VALUES ($1, 3, 1, 'frozen', 'old', 7, 'new', 12)`,
	} {
		if _, err := e.sys.Exec(ctx, q, fo.ID); err != nil {
			t.Fatal(err)
		}
	}
	refused := func(err error) bool {
		var pe *pgconn.PgError
		return errors.As(err, &pe) && pe.Code == "KX004"
	}
	// node 9 is the second instance of the server "old": not a node of the group, but its server is
	if err := record("instance.stop", "instance", "9"); !refused(err) {
		t.Errorf("a stop of another instance of the old server: %v", err)
	}
	if err := record("config.restart", "config", "restart"); !refused(err) {
		t.Errorf("a config restart during a failover: %v", err)
	}
	if err := record("instance.stop", "instance", "8"); err != nil {
		t.Errorf("an instance of another server must not be affected: %v", err)
	}
}

// KX005 (migration 0014, F2-09): a failover cannot start while an instance stop, an instance restart or a config restart
// is running; the exclusion works both ways, and a task that ended frees the way.
func TestAFailoverIsRefusedWhileAnInstanceOperationRuns(t *testing.T) {
	e := newEnvNoTask(t)
	st := task.NewPGStore(e.sys)
	var x *task.ExcludedError
	for _, op := range []struct{ kind, subject, typ string }{{"instance.stop", "1", "instance"}, {"instance.restart", "2", "instance"}, {"config.restart", "restart", "config"}} {
		var id int64
		if err := e.sys.QueryRow(context.Background(), `INSERT INTO kingres_cp.task (kind, state, subject_type, subject_id, created_by) VALUES ($1, 'running', $2, $3, 'test') RETURNING id`,
			op.kind, op.typ, op.subject).Scan(&id); err != nil {
			t.Fatal(err)
		}
		if _, err := e.create(t, st, kindFailover, "s1"); !errors.As(err, &x) {
			t.Fatalf("a failover while a %s runs was accepted: %v", op.kind, err)
		}
		e.setState(t, id, "done")
	}
	if _, err := e.create(t, st, kindFailover, "s1"); err != nil {
		t.Fatalf("after the operations ended: %v", err)
	}
}

// KX006 and KX007 (migration 0016, F3-04): a failover cannot start while a database is created or dropped or a table is
// distributed, those operations cannot start while a failover is unfinished, and an ended task frees the way.
func TestAFailoverAndDatabaseChangesExcludeEachOther(t *testing.T) {
	e := newEnvNoTask(t)
	st := task.NewPGStore(e.sys)
	var x *task.ExcludedError
	for _, kind := range []string{"database.create", "database.drop", "table.distribute"} {
		var id int64
		if err := e.sys.QueryRow(context.Background(), `INSERT INTO kingres_cp.task (kind, state, subject_type, subject_id, created_by) VALUES ($1, 'running', 'database', 'd', 'test') RETURNING id`,
			kind).Scan(&id); err != nil {
			t.Fatal(err)
		}
		if _, err := e.create(t, st, kindFailover, "s1"); !errors.As(err, &x) {
			t.Fatalf("a failover while %s runs was accepted: %v", kind, err)
		}
		e.setState(t, id, "done")
	}
	if _, err := e.create(t, st, kindFailover, "s1"); err != nil {
		t.Fatalf("after the operations ended: %v", err)
	}
	for _, kind := range []string{"database.create", "database.drop", "table.distribute"} {
		var nid int64
		err := e.sys.QueryRow(context.Background(), `INSERT INTO kingres_cp.task (kind, state, subject_type, subject_id, created_by) VALUES ($1, 'pending', 'database', 'd', 'test') RETURNING id`,
			kind).Scan(&nid)
		var pe *pgconn.PgError
		if !errors.As(err, &pe) || pe.Code != "KX007" {
			t.Fatalf("%s while a failover is unfinished was accepted: %v", kind, err)
		}
	}
}

// Migration 0041: the tasks of the distributed schemas and a failover exclude each other (KX006, KX007), and the two that move a
// bucket (schema.move, schema.undistribute) and a rebalance exclude each other (KX001, KX002); schema.distribute does not move one
// and lives beside a rebalance. An ended task frees the way.
func TestSchemaTasksExcludeAFailoverAndARebalanceInTheDatabase(t *testing.T) {
	e := newEnvNoTask(t)
	ctx := context.Background()
	insert := func(kind, state string) (int64, error) {
		var id int64
		err := e.sys.QueryRow(ctx, `INSERT INTO kingres_cp.task (kind, state, subject_type, subject_id, created_by) VALUES ($1, $2, 'schema', $1, 'test') RETURNING id`,
			kind, state).Scan(&id)
		return id, err
	}
	code := func(err error) string {
		var pe *pgconn.PgError
		if errors.As(err, &pe) {
			return pe.Code
		}
		return "none"
	}
	schemaKinds := []string{"schema.distribute", "schema.move", "schema.undistribute"}
	// a failover first
	for _, fo := range []string{kindFailover, kindRestore} {
		foID, err := insert(fo, "running")
		if err != nil {
			t.Fatal(err)
		}
		for _, k := range schemaKinds {
			if _, err := insert(k, "pending"); code(err) != "KX007" {
				t.Fatalf("%s while %s runs: %v", k, fo, err)
			}
		}
		e.setState(t, foID, "paused")
		if _, err := insert("schema.move", "pending"); code(err) != "KX007" {
			t.Fatalf("a paused %s still excludes: %v", fo, err)
		}
		e.setState(t, foID, "done")
	}
	// a schema task first
	for _, k := range schemaKinds {
		id, err := insert(k, "running")
		if err != nil {
			t.Fatal(err)
		}
		for _, fo := range []string{kindFailover, kindRestore} {
			if _, err := insert(fo, "pending"); code(err) != "KX006" {
				t.Fatalf("%s while %s runs: %v", fo, k, err)
			}
		}
		moves := k != "schema.distribute"
		_, err = insert("rebalance", "pending")
		switch {
		case moves && code(err) != "KX001":
			t.Fatalf("a rebalance while %s runs: %v", k, err)
		case !moves && err != nil:
			t.Fatalf("a rebalance beside %s: %v", k, err)
		}
		if !moves {
			if _, err := e.sys.Exec(ctx, `UPDATE kingres_cp.task SET state = 'done' WHERE kind = 'rebalance'`); err != nil {
				t.Fatal(err)
			}
		}
		e.setState(t, id, "done")
	}
	// a rebalance first
	rb, err := insert("rebalance", "running")
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range schemaKinds {
		_, err := insert(k, "pending")
		if k == "schema.distribute" && err != nil {
			t.Fatalf("%s beside a rebalance: %v", k, err)
		}
		if k != "schema.distribute" && code(err) != "KX002" {
			t.Fatalf("%s while a rebalance runs: %v", k, err)
		}
	}
	e.setState(t, rb, "done")
	if _, err := insert(kindFailover, "pending"); code(err) != "KX006" {
		t.Fatalf("the schema.distribute task of the last step is still unfinished: %v", err)
	}
	if _, err := e.sys.Exec(ctx, `UPDATE kingres_cp.task SET state = 'done' WHERE kind LIKE 'schema.%'`); err != nil {
		t.Fatal(err)
	}
	if _, err := insert(kindFailover, "pending"); err != nil {
		t.Fatalf("after every schema task ended: %v", err)
	}
}

// Migration 0041: schema.move and schema.undistribute and a change of the servers (add, remove, leave) exclude each other (KX007 for
// the schema task, KX006 for the server task); schema.distribute lives beside them.
func TestSchemaMovesAndServerChangesExcludeEachOtherInTheDatabase(t *testing.T) {
	e := newEnvNoTask(t)
	ctx := context.Background()
	insert := func(kind string) (int64, error) {
		var id int64
		err := e.sys.QueryRow(ctx, `INSERT INTO kingres_cp.task (kind, state, subject_type, subject_id, created_by) VALUES ($1, 'running', 'x', $1, 'test') RETURNING id`, kind).Scan(&id)
		return id, err
	}
	code := func(err error) string {
		var pe *pgconn.PgError
		if errors.As(err, &pe) {
			return pe.Code
		}
		return "none"
	}
	for _, srv := range []string{"server.add", "server.remove", "server.leave"} {
		for _, k := range []string{"schema.move", "schema.undistribute"} {
			id, err := insert(srv)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := insert(k); code(err) != "KX007" {
				t.Fatalf("%s while %s runs: %v", k, srv, err)
			}
			e.setState(t, id, "done")
			id, err = insert(k)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := insert(srv); code(err) != "KX006" {
				t.Fatalf("%s while %s runs: %v", srv, k, err)
			}
			e.setState(t, id, "done")
		}
		id, err := insert("schema.distribute")
		if err != nil {
			t.Fatal(err)
		}
		sid, err := insert(srv)
		if err != nil {
			t.Fatalf("%s beside schema.distribute: %v", srv, err)
		}
		e.setState(t, id, "done")
		e.setState(t, sid, "done")
	}
}
