// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package ops

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/pgconninfo"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/pgtest"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
	"strconv"
)

func TestCancelFindsOnlyTheCallersOwnRunningStatement(t *testing.T) {
	o := &Ops{}
	var stopped sync.WaitGroup
	stopped.Add(1)
	st := &runningStatement{role: "alice", database: "d", stop: stopped.Done}
	if !o.stmts.add("s1", st) {
		t.Fatal("first add refused")
	}
	if o.stmts.add("s1", &runningStatement{role: "alice"}) {
		t.Fatal("a running id of the same role must conflict")
	}
	if !o.stmts.add("s1", &runningStatement{role: "bob"}) {
		t.Fatal("the same id of another role is another statement")
	}
	for _, c := range []struct{ role, id string }{{"mallory", "s1"}, {"alice", "other"}, {"alice", "bad id!"}, {"alice", ""}} {
		wantCode(t, o.CancelStatement(context.Background(), c.role, c.id), problem.NotFound)
	}
	if st.cancelled.Load() {
		t.Fatal("a refused cancel must leave the statement alone")
	}
	if err := o.CancelStatement(context.Background(), "alice", "s1"); err != nil {
		t.Fatal(err)
	}
	if !st.cancelled.Load() {
		t.Fatal("the statement was not marked")
	}
	stopped.Wait() // the safety net ends a statement the server did not end
	o.stmts.finish("s1", st)
	wantCode(t, o.CancelStatement(context.Background(), "alice", "s1"), problem.NotFound)
}

// TestCancelAfterTheStatementEndedNeverCancels is the pid-reuse rule: once finish ran, the entry
// is done and a cancel in flight cannot reach the backend.
func TestCancelAfterTheStatementEndedNeverCancels(t *testing.T) {
	o := &Ops{}
	st := &runningStatement{role: "alice", database: "d", stop: func() {}}
	o.stmts.add("s", st)
	found := o.stmts.find("alice", "s")
	o.stmts.finish("s", st) // the statement ends between the lookup and the cancel
	found.mu.Lock()
	done := found.done
	found.mu.Unlock()
	if !done {
		t.Fatal("a finished statement must be marked done under its lock")
	}
}

// roleConn connects as a plain role with a password to the test instance.
type roleConn struct{ in *pgtest.Instance }

func (r roleConn) Connect(ctx context.Context, db, role string) (*pgx.Conn, error) {
	dsn, err := pgconninfo.Format(pgconninfo.Pair{Key: "host", Value: "127.0.0.1"}, pgconninfo.Pair{Key: "port", Value: strconv.Itoa(r.in.Port)},
		pgconninfo.Pair{Key: "dbname", Value: db}, pgconninfo.Pair{Key: "user", Value: role}, pgconninfo.Pair{Key: "password", Value: "pw-" + role}, pgconninfo.Pair{Key: "sslmode", Value: "disable"})
	if err != nil {
		return nil, err
	}
	return pgx.Connect(ctx, dsn)
}
func (roleConn) Revoke(context.Context, string) {}

// TestIntegrationCancelEndsARunningStatement runs a long statement as a plain role, cancels it by
// its id and expects SQLSTATE 57014; another role cannot cancel it; the id is free afterwards.
func TestIntegrationCancelEndsARunningStatement(t *testing.T) {
	if !pgtest.Available() {
		t.Skip("KINGRES_PG_BIN is not set; skipping the PostgreSQL integration test")
	}
	in := pgtest.Start(t, nil)
	ctx := context.Background()
	admin := in.Connect(t, "postgres")
	for _, r := range []string{"wb_alice", "wb_bob"} {
		if _, err := admin.Exec(ctx, `CREATE ROLE `+r+` LOGIN PASSWORD 'pw-`+r+`'`); err != nil {
			t.Fatal(err)
		}
	}
	o := &Ops{Service: testService{in, t}, Roles: roleConn{in}}
	run := func(id string) <-chan error {
		ch := make(chan error, 1)
		go func() {
			_, err := o.ExecuteQuery(ctx, "wb_alice", api.QueryRequest{Database: "postgres", SQL: "SELECT pg_sleep(60)", StatementID: id})
			ch <- err
		}()
		return ch
	}
	done := run("long-1")
	waitRunning := func(id string) {
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			if o.stmts.find("wb_alice", id) != nil {
				var n int
				_ = admin.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE usename = 'wb_alice' AND query LIKE 'SELECT pg_sleep%'`).Scan(&n)
				if n == 1 {
					return
				}
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Fatal("the statement did not start")
	}
	waitRunning("long-1")
	wantCode(t, o.CancelStatement(ctx, "wb_bob", "long-1"), problem.NotFound)
	if _, err := o.ExecuteQuery(ctx, "wb_alice", api.QueryRequest{Database: "postgres", SQL: "SELECT 1", StatementID: "long-1"}); err == nil {
		t.Fatal("a running id must conflict")
	}
	start := time.Now()
	if err := o.CancelStatement(ctx, "wb_alice", "long-1"); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		p := wantCode(t, err, problem.SQLError)
		if p.SQLState != "57014" {
			t.Fatalf("sqlstate %q, want 57014", p.SQLState)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the statement was not cancelled")
	}
	if d := time.Since(start); d > 20*time.Second {
		t.Fatalf("cancel took %v", d)
	}
	// The id is free again and the slot was returned.
	if _, err := o.ExecuteQuery(ctx, "wb_alice", api.QueryRequest{Database: "postgres", SQL: "SELECT 1", StatementID: "long-1"}); err != nil {
		t.Fatalf("after the cancel: %v", err)
	}
	wantCode(t, o.CancelStatement(ctx, "wb_alice", "long-1"), problem.NotFound)
}
