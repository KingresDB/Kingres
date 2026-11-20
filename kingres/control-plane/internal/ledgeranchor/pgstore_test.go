// SPDX-License-Identifier: AGPL-3.0-only

package ledgeranchor

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/anchor"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ledgerpub"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/pgtest"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/scheduler"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/sysdb"
)

// pgEnv is the environment of the tests with a real PostgreSQL: an instance with the system database and every migration, the store in PostgreSQL, and
// the term taken from the real row. The tests skip without KINGRES_PG_BIN.
func pgEnv(t *testing.T) (*env, *pgx.Conn, PGStore) {
	t.Helper()
	in := pgtest.Start(t, nil)
	ctx := context.Background()
	if _, err := in.Connect(t, "postgres").Exec(ctx, "CREATE DATABASE kingres_system"); err != nil {
		t.Fatal(err)
	}
	admin := in.Connect(t, "kingres_system")
	if _, err := sysdb.Migrate(ctx, admin); err != nil {
		t.Fatal(err)
	}
	sys := func(ctx context.Context, f func(c *pgx.Conn) error) error {
		cfg, err := pgx.ParseConfig(in.DSN("kingres_system"))
		if err != nil {
			return err
		}
		c, err := pgx.ConnectConfig(ctx, cfg)
		if err != nil {
			return err
		}
		defer func() { _ = c.Close(context.WithoutCancel(ctx)) }()
		return f(c)
	}
	st := PGStore{Sys: sys}
	var term scheduler.Term
	if err := admin.QueryRow(ctx, `UPDATE kingres_cp.ledger_cluster SET term = term + 1 RETURNING term`).Scan(&term); err != nil {
		t.Fatal(err)
	}
	e := newEnv(t, func(e *env) { e.store, e.pg, e.term = st, admin, term })
	return e, admin, st
}

func TestTheWholeWayOfACheckpointOnTheDatabaseStore(t *testing.T) {
	e, admin, st := pgEnv(t)
	tg := e.addWorm("vault")
	e.putAgg("shop", 3)
	if err := e.runEnqueue(); err != nil {
		t.Fatal(err)
	}
	if err := e.runSubmit(); err != nil {
		t.Fatal(err)
	}
	r := e.rows()[0]
	if r.State != anchor.StateAnchored || r.TargetID != tg.ID || r.Size != 3 || r.SubmitStart.IsZero() {
		t.Fatalf("%+v", r)
	}
	ev, err := anchor.ParseWORMEvidence(r.Evidence)
	if err != nil || len(e.s3.stored(ev.Key)) != 1 {
		t.Fatalf("evidence %v", err)
	}
	// the checkpoint is marked anchored (the minimum of targets is 1)
	var anchored bool
	if err := admin.QueryRow(bg(), `SELECT anchored FROM kingres_cp.ledger_checkpoint WHERE size = 3`).Scan(&anchored); err != nil || !anchored {
		t.Errorf("checkpoint anchored %v %v", anchored, err)
	}
	// the evidence is read back byte for byte (text, not jsonb), the unique key holds, and the file of the evidence is canonical
	f, err := e.svc.Evidence(e.ctx(), "shop", r.ID, "json")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := anchor.ParseEvidenceFile(f.Data); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(bg(), `INSERT INTO kingres_cp.ledger_anchor (target_id, target_name, kind, log_id, database_name, aggregate_id, origin, size, payload_sha256, note, created_at)
		SELECT target_id, target_name, kind, log_id, database_name, aggregate_id, origin, size, payload_sha256, note, now() FROM kingres_cp.ledger_anchor`); err == nil {
		t.Error("a second record for the same (target, log, size) was accepted")
	}
	// a list with a cursor, a state filter that shows claimed as submitted
	pg, err := e.svc.List(e.ctx(), "shop", tg.ID, "anchored", pageOf(1))
	if err != nil || len(pg.Items) != 1 || pg.Items[0].State != "anchored" {
		t.Fatalf("list: %+v %v", pg, err)
	}
	if _, err := admin.Exec(bg(), `UPDATE kingres_cp.ledger_anchor SET state = 'claimed'`); err != nil {
		t.Fatal(err)
	}
	if pg, _ = e.svc.List(e.ctx(), "shop", 0, "submitted", pageOf(5)); len(pg.Items) != 1 || pg.Items[0].State != "submitted" {
		t.Errorf("a claimed record is not shown as submitted: %+v", pg)
	}
	_ = st
}

// The term guard on the real row: the writes of a deposed leader are refused, the compare and swap drops a stale result, supersede and the delete of a
// target on a real database, and a kill at each write (the hook of the store) leaves nothing half done.
func TestTheDatabaseStoreKeepsTheRulesOfTheStore(t *testing.T) {
	e, admin, st := pgEnv(t)
	tg := e.addWorm("vault")
	e.putAgg("shop", 1)
	if err := e.runEnqueue(); err != nil {
		t.Fatal(err)
	}
	e.putAgg("shop", 2)
	if err := e.runEnqueue(); err != nil {
		t.Fatal(err)
	}
	rs := e.rows()
	if len(rs) != 2 || rs[0].State != anchor.StateSuperseded || rs[1].State != anchor.StatePending {
		t.Fatalf("supersede: %+v", rs)
	}
	// a stale result: the row changed since the job read it
	row := e.row(rs[1].ID)
	other := row
	other.LastError = "x"
	if err := st.Apply(e.ctx(), e.term, other, row.Version); err != nil {
		t.Fatal(err)
	}
	if err := st.Apply(e.ctx(), e.term, row, row.Version); !errors.Is(err, ErrConflict) {
		t.Fatalf("a stale write: %v", err)
	}
	// a deposed leader writes nothing
	var newer scheduler.Term
	if err := admin.QueryRow(bg(), `UPDATE kingres_cp.ledger_cluster SET term = term + 1 RETURNING term`).Scan(&newer); err != nil {
		t.Fatal(err)
	}
	before := e.row(rs[1].ID)
	if _, err := st.Begin(e.ctx(), e.term, before.ID, e.clock.Now()); !errors.Is(err, scheduler.ErrDeposed) {
		t.Errorf("Begin: %v", err)
	}
	if err := st.Apply(e.ctx(), e.term, before, before.Version); !errors.Is(err, scheduler.ErrDeposed) {
		t.Errorf("Apply: %v", err)
	}
	if _, err := e.svc.CreateTarget(e.ctx(), "a", api.LedgerAnchorTargetCreate{Kind: "worm", Name: "x", Config: e.wormConfig(nil)}); err == nil {
		t.Error("a deposed leader created a target")
	} else {
		var p *problem.Problem
		if !errors.As(err, &p) || p.Code != problem.ServiceUnavailable {
			t.Errorf("create: %v", err)
		}
	}
	if err := e.runEnqueue(); !errors.Is(err, scheduler.ErrDeposed) {
		t.Errorf("enqueue under a deposed term: %v", err)
	}
	if after := e.row(before.ID); after.Version != before.Version || !after.SubmitStart.IsZero() {
		t.Errorf("the deposed leader changed the row: %+v", after)
	}
	// the new leader goes on
	e.term = newer
	// a kill at each write of the store, by the hook after the guard
	for _, kind := range []string{"begin", "apply"} {
		killed := false
		st.OnWrite = func(k string) {
			if k == kind && !killed {
				killed = true
				panic("killed at " + k)
			}
		}
		e.rebuildOn(st)
		func() {
			defer func() { _ = recover() }()
			_ = e.runSubmit()
		}()
		st.OnWrite = nil
		e.rebuildOn(st)
		if err := e.runSubmit(); err != nil {
			t.Fatal(err)
		}
		if got := e.row(before.ID); got.State == anchor.StateAnchored {
			break
		}
	}
	if got := e.row(before.ID); got.State != anchor.StateAnchored || len(e.s3.objects) != 1 {
		t.Fatalf("after the kills: %+v, %d keys", got, len(e.s3.objects))
	}
	// the delete of a target keeps its records, with the name
	if err := e.svc.DeleteTarget(e.ctx(), tg.ID); err != nil {
		t.Fatal(err)
	}
	var tid *int64
	var name string
	if err := admin.QueryRow(bg(), `SELECT target_id, target_name FROM kingres_cp.ledger_anchor ORDER BY id DESC LIMIT 1`).Scan(&tid, &name); err != nil || tid != nil || name != "vault" {
		t.Errorf("after the delete: %v %v %v", tid, name, err)
	}
	_ = time.Second
}

// rebuildOn makes the service again over the given store (a restart of the process, with the same credentials directory).
func (e *env) rebuildOn(st Store) { e.store = st; e.rebuild("") }

// The evidence file is published below the aggregate log (correction 187): the same bytes as downloadLedgerAnchor, under the canonical path
// only, nothing for a record without evidence.
func TestTheEvidenceFileIsPublishedUnderTheAggregateLog(t *testing.T) {
	e, _, st := pgEnv(t)
	tg := e.addWorm("vault")
	e.putAgg("shop", 3)
	if err := e.runEnqueue(); err != nil {
		t.Fatal(err)
	}
	pub := ledgerpub.PGStore{Sys: ledgerpub.SysFunc(st.Sys)}
	r0 := e.rows()[0]
	path := "anchors/3/" + strconv.FormatInt(tg.ID, 10) + ".json"
	if _, err := pub.Object(bg(), r0.Origin, path); !errors.Is(err, ledgerpub.ErrNotFound) {
		t.Fatalf("a record without evidence: %v", err)
	}
	if err := e.runSubmit(); err != nil {
		t.Fatal(err)
	}
	o, err := pub.Object(bg(), r0.Origin, path)
	if err != nil || !o.Mutable || o.ContentType != "application/json" {
		t.Fatalf("%v %+v", err, o)
	}
	want, err := e.svc.Evidence(e.ctx(), "shop", r0.ID, "json")
	if err != nil || string(o.Data) != string(want.Data) {
		t.Fatalf("the published file is not the downloaded one: %v", err)
	}
	for _, bad := range []string{"anchors/03/" + strconv.FormatInt(tg.ID, 10) + ".json", "anchors/3/" + strconv.FormatInt(tg.ID, 10), "anchors/3/99.json", "anchors/4/1.json", "anchors", "anchors/3", "anchors/3/1.json/x"} {
		if _, err := pub.Object(bg(), r0.Origin, bad); !errors.Is(err, ledgerpub.ErrNotFound) {
			t.Errorf("%s: %v", bad, err)
		}
	}
}
