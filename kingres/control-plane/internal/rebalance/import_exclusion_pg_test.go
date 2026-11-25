// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package rebalance

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/importdb"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/task"
)

const importKind = "database.import"

// other puts a task of the kind and the state given in the database, as an unfinished (or finished) operation that the candidate meets.
func (e *guardEnv) other(t *testing.T, kind, state string) {
	t.Helper()
	typ, subj := "server", "s9"
	switch kind {
	case Kind:
		typ, subj = SubjectType, SubjectID
	case importKind:
		typ, subj = "database", "shop"
	}
	if _, err := e.sys.Exec(context.Background(), `INSERT INTO kingres_cp.task (kind, state, subject_type, subject_id, input, progress, created_at, updated_at, created_by)
		VALUES ($1, $2, $3, $4, '{}', '{"steps":[]}', now(), now(), 'u')`, kind, state, typ, subj); err != nil {
		t.Fatalf("%s %s: %v", kind, state, err)
	}
}

func (e *guardEnv) clear(t *testing.T) {
	t.Helper()
	if _, err := e.sys.Exec(context.Background(), `DELETE FROM kingres_cp.task`); err != nil {
		t.Fatal(err)
	}
}

// candidate asks the store for a new task of the kind (a pending one) and answers what the exclusion says.
func (e *guardEnv) candidate(t *testing.T, kind string) error {
	t.Helper()
	typ, subj := "server", "s8"
	switch kind {
	case Kind:
		typ, subj = SubjectType, SubjectID
	case importKind:
		typ, subj = "database", "shop2"
	}
	tk, err := task.NewPGStore(e.sys).Create(context.Background(), task.Task{Kind: kind, SubjectType: typ, SubjectID: subj, Input: []byte("{}"),
		Steps: []task.StepState{{Name: "x", State: task.StepPending}}, CreatedAt: time.Now(), UpdatedAt: time.Now()})
	if err == nil {
		_, err = e.sys.Exec(context.Background(), `UPDATE kingres_cp.task SET state = 'done' WHERE id = $1`, tk.ID)
	}
	return err
}

func wantExcluded(t *testing.T, what string, err error, other string) {
	t.Helper()
	var x *task.ExcludedError
	if !errors.As(err, &x) || x.Other != other {
		t.Fatalf("%s: %v, want an ExcludedError naming %s", what, err, other)
	}
	if p := x.Problem(); p.Code != problem.OperationInProgress {
		t.Errorf("%s: problem %q, want operation_in_progress", what, p.Code)
	}
}

// Migration 0045: a database import and a rebalance (a drain is one) or the removal of a server exclude each other, in both orders. A mutant of the migration that
// leaves out one of the kinds fails the case of that kind.
func TestAnImportAndARebalanceOrARemovalExcludeEachOther(t *testing.T) {
	e := newEnvNoTask(t)
	// an import is unfinished (in any of its three states): a rebalance (a drain), a removal and a leave of a server are refused
	for _, st := range []string{"pending", "running", "paused"} {
		e.clear(t)
		e.other(t, importKind, st)
		wantExcluded(t, "a rebalance while an import is "+st, e.candidate(t, Kind), importKind)
		for _, k := range []string{"server.remove", "server.leave"} {
			wantExcluded(t, k+" while an import is "+st, e.candidate(t, k), importKind)
		}
		// the failover and the add of a server are not excluded: the check before the final name finds what a failover changed
		for _, k := range []string{"server.add", "server.failover"} {
			if err := e.candidate(t, k); err != nil {
				t.Errorf("%s while an import is %s: %v", k, st, err)
			}
		}
	}
	// the import has ended (done or failed): nothing is refused
	for _, st := range []string{"done", "failed"} {
		e.clear(t)
		e.other(t, importKind, st)
		for _, k := range []string{Kind, "server.remove", "server.leave"} {
			if err := e.candidate(t, k); err != nil {
				t.Errorf("%s after an import that is %s: %v", k, st, err)
			}
		}
	}
	// a rebalance that is pending or running, or an unfinished removal or leave, refuses the import; a paused rebalance does not
	for _, k := range []string{Kind, "server.remove", "server.leave"} {
		for _, st := range []string{"pending", "running"} {
			e.clear(t)
			e.other(t, k, st)
			wantExcluded(t, "an import while "+k+" is "+st, e.candidate(t, importKind), k)
		}
	}
	for _, k := range []string{"server.remove", "server.leave"} {
		e.clear(t)
		e.other(t, k, "paused")
		wantExcluded(t, "an import while "+k+" is paused", e.candidate(t, importKind), k)
	}
	e.clear(t)
	e.other(t, Kind, "paused")
	if err := e.candidate(t, importKind); err != nil {
		t.Errorf("an import while a rebalance is paused: %v", err)
	}
	// a resumed rebalance is refused while an import is unfinished (the same rule on the way from paused to pending)
	e.clear(t)
	e.other(t, importKind, "running")
	if _, err := e.sys.Exec(context.Background(), `INSERT INTO kingres_cp.task (kind, state, subject_type, subject_id, input, progress, created_at, updated_at, created_by)
		VALUES ('rebalance', 'paused', $1, $2, '{}', '{"steps":[]}', now(), now(), 'u')`, SubjectType, SubjectID); err != nil {
		t.Fatal(err)
	}
	var id int64
	if err := e.sys.QueryRow(context.Background(), `SELECT id FROM kingres_cp.task WHERE kind = 'rebalance'`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	wantExcluded(t, "the resume of a rebalance while an import runs", ResumeRun(context.Background(), e.sys, id, Options{}), importKind)
}

// The check that a drained server owns no bucket reads the private databases of imports too, and names them: a drain is refused while an import runs, so such a
// database is one that a failed import left (keepOnFailure), and a bucket of it on the server must keep the drain from ending.
func TestTheDrainCountsTheBucketsOfAKeptPrivateDatabase(t *testing.T) {
	e, _ := newGuardEnv(t)
	schemaStub(t, e)
	ctx := context.Background()
	priv := importdb.Prefix + "21_0123456789ab"
	c := e.in.Connect(t, "postgres")
	for _, q := range []string{"create database " + priv, "comment on database " + priv + " is '" + importdb.Marker(priv) + "'"} {
		if _, err := c.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	b := &PG{Conns: e}
	// the walks that change something leave it out; the count reads it
	dbs, err := b.Databases(ctx, Options{})
	if err != nil || slices.Contains(dbs, priv) {
		t.Fatalf("Databases lists the private database: %v %v", dbs, err)
	}
	dbs, err = b.Databases(ctx, Options{WithPrivate: true})
	if err != nil || !slices.Contains(dbs, priv) {
		t.Fatalf("Databases with WithPrivate leaves it out: %v %v", dbs, err)
	}
	s := &Service{Conns: e, Backend: b, DrainWait: time.Nanosecond}
	// the test environment maps every database to the one stub catalog, so the private database holds the same four buckets of the server
	n, private, err := s.bucketsOnWithPrivate(ctx, "10.0.0.1")
	if err != nil || !slices.Equal(private, []string{priv}) {
		t.Fatalf("bucketsOnWithPrivate: %d %v %v", n, private, err)
	}
	if only, err := s.bucketsOn(ctx, "10.0.0.1"); err != nil || only != n {
		t.Fatalf("bucketsOn: %d %v (%d with the private database)", only, err, n)
	}
	var withoutPrivate int
	for _, db := range dbs {
		if db == priv {
			continue
		}
		withoutPrivate += 4
	}
	if n <= withoutPrivate-4 { // the private database adds its four to the count of the others
		t.Fatalf("the count %d does not include the private database (others: %d)", n, withoutPrivate)
	}
	err = s.stepDrainEmpty(ctx, nil, nil, Options{DrainServer: "10.0.0.1"})
	if err == nil || codeOf(t, err) != problem.ServerOwnsBuckets || !strings.Contains(err.Error(), priv) {
		t.Fatalf("the drain ended, or does not name the private database: %v", err)
	}
	// the databases of the clients hold no bucket of the server any more: the private one alone keeps the drain from ending
	if _, err := c.Exec(ctx, `UPDATE kingres.bucket_map SET group_id = 3`); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Exec(ctx, `UPDATE kingres.node SET host = '10.0.0.9' WHERE host = '10.0.0.1'`); err != nil {
		t.Fatal(err)
	}
	if n, _, err = s.bucketsOnWithPrivate(ctx, "10.0.0.1"); err != nil || n != 0 {
		t.Fatalf("an empty server: %d %v", n, err)
	}
}

// The check that a rebalance or a drain makes before it makes a task refuses while an import is unfinished, with the code of the other refusals of a change of the servers.
func TestTheCheckBeforeARebalanceRefusesWhileAnImportRuns(t *testing.T) {
	e := newEnvNoTask(t)
	b := &PG{Conns: e}
	if err := b.Check(context.Background(), nil); err != nil {
		t.Fatalf("without an import: %v", err)
	}
	e.other(t, importKind, "running")
	err := b.Check(context.Background(), nil)
	if err == nil || codeOf(t, err) != problem.OperationInProgress || !strings.Contains(err.Error(), importKind) {
		t.Fatalf("with an import: %v", err)
	}
}
