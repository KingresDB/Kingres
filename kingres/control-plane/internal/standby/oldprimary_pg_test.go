// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package standby

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/pgtest"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/sysdb"
)

// systemDB is a system database with every migration, and a Leader whose service connection and system access go to it.
func systemDB(t *testing.T) (*pgx.Conn, *Leader) {
	t.Helper()
	in := pgtest.Start(t, nil)
	ctx := context.Background()
	if _, err := in.Connect(t, "postgres").Exec(ctx, "CREATE DATABASE kingres_system"); err != nil {
		t.Fatal(err)
	}
	c := in.Connect(t, "kingres_system")
	if _, err := sysdb.Migrate(ctx, c); err != nil {
		t.Fatal(err)
	}
	sys := func(ctx context.Context, f func(c *pgx.Conn) error) error {
		cc, err := pgx.Connect(ctx, in.DSN("kingres_system"))
		if err != nil {
			return err
		}
		defer func() { _ = cc.Close(context.WithoutCancel(ctx)) }()
		return f(cc)
	}
	l := NewLeader(&Leader{Sys: sys, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		ServiceConn: func(ctx context.Context, _ string) (*pgx.Conn, error) {
			return pgx.Connect(ctx, in.DSN("kingres_system"))
		}})
	return c, l
}

func insertOld(t *testing.T, c *pgx.Conn, id int, group int, server, state string, diverged bool, age time.Duration) {
	t.Helper()
	_, err := c.Exec(context.Background(), `INSERT INTO kingres_cp.standby (standby_id, group_id, server_id, primary_server, instance_id, slot_name, state, needs_rebuild, diverged, generation, created_at)
		OVERRIDING SYSTEM VALUE VALUES ($1, $2, $3, 'p', 1, $4, $5, $6, $6, 1, now() - $7::interval)`,
		id, group, server, fmt.Sprintf("kingres_standby_%d", id), state, diverged, fmt.Sprintf("%d seconds", int(age.Seconds())))
	if err != nil {
		t.Fatal(err)
	}
}

// The leader's look at the old primaries that wait for a rebuild (design 6.5): a silent server is noted, then its
// record is removed after ReplaceAfter; the alert comes after the shorter of ten minutes and half of it; a server that
// answers is not noted, and the alert goes when the standby is ready.
func TestTheLeaderWatchesTheOldPrimariesThatWait(t *testing.T) {
	ctx := context.Background()
	c, l := systemDB(t)
	l.ReplaceAfter = 20 * time.Second
	dead := map[string]bool{"s-silent": true, "s-gone": true}
	l.Dead = func(s string) bool { return dead[s] }
	alerts := func(kind string) int {
		var n int
		if err := c.QueryRow(ctx, `SELECT count(*) FROM kingres_cp.alert WHERE kind = $1 AND cleared_at IS NULL`, kind).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	// 1: silent for the first time, 5 s old: noted, nothing else
	insertOld(t, c, 1, 1, "s-silent", stateRebuilding, true, 5*time.Second)
	// 2: silent and already noted 30 s ago: removed
	insertOld(t, c, 2, 2, "s-gone", stateRebuilding, true, 40*time.Second)
	if _, err := c.Exec(ctx, `UPDATE kingres_cp.standby SET waiting_since = now() - interval '30 seconds' WHERE standby_id = 2`); err != nil {
		t.Fatal(err)
	}
	// 3: the server answers, 15 s old (more than half of ReplaceAfter): the alert, and a mark that was set is cleared
	insertOld(t, c, 3, 3, "s-up", stateRebuilding, true, 15*time.Second)
	if _, err := c.Exec(ctx, `UPDATE kingres_cp.standby SET waiting_since = now() - interval '5 seconds' WHERE standby_id = 3`); err != nil {
		t.Fatal(err)
	}
	// 4: an ordinary standby that is not waiting: untouched
	insertOld(t, c, 4, 4, "s-ok", stateReady, false, time.Hour)
	list := func() map[int64]Record {
		recs, err := l.st.list(ctx)
		if err != nil {
			t.Fatal(err)
		}
		m := map[int64]Record{}
		for _, r := range recs {
			m[r.ID] = r
		}
		return m
	}
	l.watchOldPrimaries(ctx, mustList(t, l), map[string]Status{"s-up": {}, "s-ok": {}})
	got := list()
	if got[1].WaitingSince == nil || got[1].State == stateRemoving {
		t.Errorf("record 1: %+v", got[1])
	}
	if got[2].State != stateRemoving {
		t.Errorf("record 2 (silent for longer than ReplaceAfter) is %s, want removing", got[2].State)
	}
	if got[3].WaitingSince != nil || got[3].State == stateRemoving {
		t.Errorf("record 3 (its server answers): %+v", got[3])
	}
	if got[4].State != stateReady || got[4].WaitingSince != nil {
		t.Errorf("record 4: %+v", got[4])
	}
	// alert: 3 (15 s old, server answers) and 1? (5 s old: not yet); the shorter rule is 10 s
	if n := alerts("oldPrimaryNotRebuilt"); n != 1 {
		t.Errorf("%d alerts oldPrimaryNotRebuilt, want 1 (record 3 only)", n)
	}
	// the standby of 3 is rebuilt: the alert is cleared by the rebuild
	l.clearOldPrimaryAlerts(ctx, got[3])
	if n := alerts("oldPrimaryNotRebuilt"); n != 0 {
		t.Errorf("%d alerts oldPrimaryNotRebuilt after the rebuild", n)
	}
	// ready clears diverged and the mark
	if err := l.st.setState(ctx, 3, stateReady, 1234); err != nil {
		t.Fatal(err)
	}
	if r := list()[3]; r.Diverged || r.WaitingSince != nil || r.State != stateReady || r.Port != 1234 {
		t.Errorf("record 3 after the rebuild: %+v", r)
	}
	// and the alert of a record that is not waiting any more is not raised again
	l.watchOldPrimaries(ctx, mustList(t, l), map[string]Status{"s-up": {}, "s-ok": {}})
	if n := alerts("oldPrimaryNotRebuilt"); n != 0 {
		t.Errorf("the alert came back: %d", n)
	}
}

func mustList(t *testing.T, l *Leader) []Record {
	t.Helper()
	recs, err := l.st.list(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return recs
}

// What the rebuild found is stored in the newest failover row of the group that names the server, and the report
// reads it (the state follows the standby record).
func TestTheReportOfTheOldPrimaryIsStoredInTheNewestRunOfTheGroup(t *testing.T) {
	ctx := context.Background()
	c, l := systemDB(t)
	for _, task := range []int{10, 11} {
		if _, err := c.Exec(ctx, `INSERT INTO kingres_cp.task (id, kind, state, subject_type, subject_id, created_by) OVERRIDING SYSTEM VALUE
			VALUES ($1, 'server.failover', 'done', 'server', $2, 'x')`, task, fmt.Sprintf("s%d", task)); err != nil {
			t.Fatal(err)
		}
		if _, err := c.Exec(ctx, `INSERT INTO kingres_cp.failover_run (task_id, kind, trigger) VALUES ($1, 'failover', 'manual')`, task); err != nil {
			t.Fatal(err)
		}
		if _, err := c.Exec(ctx, `INSERT INTO kingres_cp.failover_group (task_id, group_id, seq, old_server) VALUES ($1, 1, 0, 's-old')`, task); err != nil {
			t.Fatal(err)
		}
	}
	if err := l.st.recordOldPrimary(ctx, 1, "s-old", []byte(`{"method":"pgRewind","divergenceBytes":42}`)); err != nil {
		t.Fatal(err)
	}
	var older, newer *string
	if err := c.QueryRow(ctx, `SELECT (SELECT old_primary_report::text FROM kingres_cp.failover_group WHERE task_id = 10),
		(SELECT old_primary_report::text FROM kingres_cp.failover_group WHERE task_id = 11)`).Scan(&older, &newer); err != nil {
		t.Fatal(err)
	}
	if older != nil || newer == nil {
		t.Errorf("older %v newer %v: the report must go to the newest run only", older, newer)
	}
}
