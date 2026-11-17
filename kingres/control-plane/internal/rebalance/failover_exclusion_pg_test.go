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
