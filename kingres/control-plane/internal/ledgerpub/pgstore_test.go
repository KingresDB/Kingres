// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package ledgerpub_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ledgerpub"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ledgersign"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/pgtest"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/scheduler"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/sysdb"
)

// pgFixture is a PostgreSQL instance with the system database and every migration (so the migration 0031 runs on a real database too).
// The tests skip without KINGRES_PG_BIN.
func pgFixture(t *testing.T) (*fixture, ledgerpub.PGStore, *pgx.Conn) {
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
	st := ledgerpub.PGStore{Sys: sys}
	bump := func() scheduler.Term {
		var n int64
		if err := admin.QueryRow(ctx, `UPDATE kingres_cp.ledger_cluster SET term = term + 1 RETURNING term`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return scheduler.Term(n)
	}
	f := &fixture{store: st}
	f.term = bump()
	f.newTerm = func() scheduler.Term { f.term = bump(); return f.term }
	f.bumpKeys = func() int64 {
		var n int64
		if err := admin.QueryRow(ctx, `UPDATE kingres_cp.ledger_cluster SET keys_revision = keys_revision + 1 RETURNING keys_revision`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	f.rev = func() int64 {
		var n int64
		if err := admin.QueryRow(ctx, `SELECT keys_revision FROM kingres_cp.ledger_cluster`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	f.putEvent = func(seq int, note []byte) {
		if _, err := admin.Exec(ctx, `INSERT INTO kingres_cp.ledger_event (seq, type, note, created_at) VALUES ($1, 'key-added', $2, now())`, seq, note); err != nil {
			t.Fatal(err)
		}
	}
	f.putKeyring = func(d []byte) {
		if _, err := admin.Exec(ctx, `UPDATE kingres_cp.ledger_cluster SET trust_file = $1`, string(d)); err != nil {
			t.Fatal(err)
		}
	}
	return f, st, admin
}

func TestTheDatabaseStoreKeepsTheRulesOfTheStore(t *testing.T) {
	// one instance for all the cases; each case starts from empty tables of the publication and from a new term
	f, _, admin := pgFixture(t)
	testStore(t, func(t *testing.T) *fixture {
		for _, tbl := range []string{"ledger_tile", "ledger_checkpoint", "ledger_log", "ledger_digest_view", "ledger_event"} {
			if _, err := admin.Exec(context.Background(), `DELETE FROM kingres_cp.`+tbl); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := admin.Exec(context.Background(), `UPDATE kingres_cp.ledger_cluster SET trust_file = NULL, first_checkpoint_at = NULL, origin_prefix = NULL`); err != nil {
			t.Fatal(err)
		}
		f.newTerm()
		return f
	})
}

// A published tile cannot be changed even by an UPDATE from SQL: the trigger refuses it (a superuser can still disable it; signatures,
// anchors and mirrors tell, design 4.5), and the primary key refuses a second row.
func TestTheDatabaseRefusesAnUpdateOfATile(t *testing.T) {
	f, st, admin := pgFixture(t)
	l := newLog(t, f, 1)
	grow(t, f, l, 0, 3)
	_, err := admin.Exec(context.Background(), `UPDATE kingres_cp.ledger_tile SET data = data`)
	var pe *pgconn.PgError
	if !errors.As(err, &pe) || pe.Code != "KC001" {
		t.Fatalf("an update of a tile: %v", err)
	}
	if _, err := admin.Exec(context.Background(), `INSERT INTO kingres_cp.ledger_tile (log_id, level, idx, width, data) SELECT log_id, level, idx, width, data FROM kingres_cp.ledger_tile`); err == nil {
		t.Fatal("a second row for a tile was accepted")
	}
	_ = st
}

// The first checkpoint locks the origin prefix. A change of the key set that read the cluster row before the first checkpoint was stored writes
// the whole row back: it must not take the lock away or change the prefix (the collector stores the first checkpoint without raising the
// revision of the key set, so the change is not refused).
func TestAKeyChangeThatReadTheRowBeforeTheFirstCheckpointDoesNotUnlockThePrefix(t *testing.T) {
	f, st, admin := pgFixture(t)
	ctx := context.Background()
	ks := ledgersign.PGStore{Sys: ledgersign.SysFunc(st.Sys)}
	stale, err := ks.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	l := newLog(t, f, 1)
	size, root := grow(t, f, l, 0, 3)
	cp := ledgerpub.Checkpoint{LogID: l.ID, Size: size, Root: root, Note: []byte("n"), SignedAt: time.Now()}
	if err := f.store.PutCheckpoint(ctx, f.term, cp, f.rev(), true, "ledger.test/kingres"); err != nil {
		t.Fatal(err)
	}
	cl := stale.Cluster
	cl.BackupAdvised = true
	cl.OriginPrefix = "other.example/prefix"
	if err := ks.Commit(ctx, f.term, stale.Revision, ledgersign.Change{Cluster: &cl}); err != nil {
		t.Fatal(err)
	}
	var at *time.Time
	var prefix string
	var advised bool
	if err := admin.QueryRow(ctx, `SELECT first_checkpoint_at, coalesce(origin_prefix, ''), backup_advised FROM kingres_cp.ledger_cluster`).Scan(&at, &prefix, &advised); err != nil {
		t.Fatal(err)
	}
	if at == nil || prefix != "ledger.test/kingres" || !advised {
		t.Errorf("first_checkpoint_at %v, prefix %q, backup_advised %v: the other columns of the change are kept, the lock and the prefix are not", at, prefix, advised)
	}
}
