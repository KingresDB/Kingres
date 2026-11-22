// SPDX-License-Identifier: AGPL-3.0-only

package transfer_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/pgtest"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/sysdb"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/transfer"
)

func sysConn(t *testing.T) *pgx.Conn {
	t.Helper()
	in := pgtest.Start(t, nil)
	ctx := context.Background()
	if _, err := in.Connect(t, "postgres").Exec(ctx, "CREATE DATABASE kingres_system"); err != nil {
		t.Fatal(err)
	}
	conn := in.Connect(t, "kingres_system")
	if _, err := sysdb.Migrate(ctx, conn); err != nil {
		t.Fatal(err)
	}
	return conn
}

func recordsOn(conn *pgx.Conn) transfer.Records {
	return transfer.Records{Run: func(ctx context.Context, f func(tx pgx.Tx) error) error {
		tx, err := conn.Begin(ctx)
		if err != nil {
			return err
		}
		if err := f(tx); err != nil {
			_ = tx.Rollback(ctx)
			return err
		}
		return tx.Commit(ctx)
	}}
}

var (
	alice = transfer.Identity{Name: "alice", OID: 16401}
	bob   = transfer.Identity{Name: "bob", OID: 16402}
)

func newRec(owner transfer.Identity, kind string) transfer.Record {
	declared := int64(100)
	return transfer.Record{Kind: kind, State: transfer.StateReceiving, ServerID: "10.0.0.1", Owner: owner, DeclaredBytes: &declared, ReservedBytes: 100,
		ExpiresAt: time.Now().Add(time.Hour)}
}

// The record: who may read it (the owner by name and OID, nobody else, and a deleted one not at all), how the list pages, how an update
// changes only what it names, and the constraints of the table.
func TestRecords(t *testing.T) {
	conn := sysConn(t)
	rs := recordsOn(conn)
	ctx := context.Background()

	id1, err := rs.Create(ctx, newRec(alice, transfer.KindUpload))
	if err != nil {
		t.Fatal(err)
	}
	r2 := newRec(alice, transfer.KindTableExport)
	r2.Database, r2.Schema, r2.Table, r2.Format, r2.State = "shop", "public", "orders", "csv", transfer.StateBuilding
	id2, err := rs.Create(ctx, r2)
	if err != nil {
		t.Fatal(err)
	}
	id3, err := rs.Create(ctx, newRec(bob, transfer.KindUpload))
	if err != nil || id1 >= id2 || id2 >= id3 {
		t.Fatalf("ids %d %d %d %v", id1, id2, id3, err)
	}

	got, err := rs.GetOwned(ctx, id2, alice)
	if err != nil || got.Table != "orders" || got.Database != "shop" || got.Format != "csv" || got.Owner != alice || got.ReservedBytes != 100 || got.ServerID != "10.0.0.1" || got.State != transfer.StateBuilding {
		t.Fatalf("GetOwned: %+v %v", got, err)
	}
	// Another role, the same name with another OID, and an unknown id are all not found; nothing tells them apart.
	for _, who := range []transfer.Identity{bob, {Name: "alice", OID: 99999}, {Name: "alice2", OID: alice.OID}} {
		if _, err := rs.GetOwned(ctx, id2, who); !errors.Is(err, transfer.ErrNotFound) {
			t.Errorf("%+v read the file of alice: %v", who, err)
		}
	}
	if _, err := rs.GetOwned(ctx, 424242, alice); !errors.Is(err, transfer.ErrNotFound) {
		t.Errorf("unknown id: %v", err)
	}
	if got.IDString() == "" {
		t.Error("IDString")
	}

	// The list holds the caller's files of the asked kinds, newest first, and pages by id.
	both := []string{transfer.KindUpload, transfer.KindTableExport}
	all, err := rs.List(ctx, alice, both, 0, 50)
	if err != nil || len(all) != 2 || all[0].ID != id2 || all[1].ID != id1 {
		t.Fatalf("List: %+v %v", all, err)
	}
	only, _ := rs.List(ctx, alice, []string{transfer.KindTableExport}, 0, 50)
	if len(only) != 1 || only[0].ID != id2 {
		t.Errorf("kinds: %+v", only)
	}
	page, _ := rs.List(ctx, alice, both, id2, 50)
	if len(page) != 1 || page[0].ID != id1 {
		t.Errorf("before: %+v", page)
	}
	if one, _ := rs.List(ctx, alice, both, 0, 1); len(one) != 1 {
		t.Errorf("limit: %+v", one)
	}
	if other, _ := rs.List(ctx, transfer.Identity{Name: "alice", OID: 1}, []string{transfer.KindUpload}, 0, 50); len(other) != 0 {
		t.Errorf("a role with another OID lists the files of alice: %+v", other)
	}

	// An update changes what it names and leaves the rest.
	size := int64(4096)
	sum := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	ready, exp := transfer.StateReady, time.Now().Add(48*time.Hour).UTC().Truncate(time.Second)
	task := int64(77)
	if err := rs.Apply(ctx, id2, transfer.Update{State: &ready, SizeBytes: &size, SHA256: &sum, Report: json.RawMessage(`{"rows":3}`), ExpiresAt: &exp, TaskID: &task, Touch: true, WrappedKey: []byte{1, 2, 3}}); err != nil {
		t.Fatal(err)
	}
	got, _ = rs.Get(ctx, id2)
	if got.State != transfer.StateReady || got.SizeBytes == nil || *got.SizeBytes != 4096 || got.SHA256 != sum || string(got.Report) != `{"rows": 3}` ||
		!got.ExpiresAt.Equal(exp) || got.TaskID == nil || *got.TaskID != 77 || got.Table != "orders" || got.ReservedBytes != 100 || len(got.WrappedKey) != 3 {
		t.Errorf("after Apply: %+v", got)
	}
	if got.DownloadedAt != nil {
		t.Error("downloaded_at set early")
	}
	if err := rs.Apply(ctx, id2, transfer.Update{Downloaded: true}); err != nil {
		t.Fatal(err)
	}
	if got, _ = rs.Get(ctx, id2); got.DownloadedAt == nil || got.State != transfer.StateReady {
		t.Errorf("downloaded: %+v", got)
	}
	// The checks of the table.
	bad := "NOT-A-HASH"
	if err := rs.Apply(ctx, id2, transfer.Update{SHA256: &bad}); err == nil {
		t.Error("a SHA-256 that is not 64 hex characters was stored")
	}
	nope := "somewhere"
	if err := rs.Apply(ctx, id2, transfer.Update{State: &nope}); err == nil {
		t.Error("an unknown state was stored")
	}
	neg := int64(-1)
	if err := rs.Apply(ctx, id2, transfer.Update{ReservedBytes: &neg}); err == nil {
		t.Error("a negative reservation was stored")
	}
	for _, r := range []transfer.Record{{}, {Kind: "upload", State: "receiving", Owner: alice}, {Kind: "other", State: "receiving", Owner: alice, ExpiresAt: time.Now()}} {
		if _, err := rs.Create(ctx, r); err == nil {
			t.Errorf("Create(%+v) must fail", r)
		}
	}

	// A deleted record is gone for everyone and is never changed back.
	del := transfer.StateDeleted
	if err := rs.Apply(ctx, id1, transfer.Update{State: &del}); err != nil {
		t.Fatal(err)
	}
	if _, err := rs.GetOwned(ctx, id1, alice); !errors.Is(err, transfer.ErrNotFound) {
		t.Errorf("deleted record readable: %v", err)
	}
	if err := rs.Apply(ctx, id1, transfer.Update{State: &ready}); !errors.Is(err, transfer.ErrNotFound) {
		t.Errorf("a deleted record changed: %v", err)
	}
	if after, _ := rs.List(ctx, alice, []string{transfer.KindUpload}, 0, 50); len(after) != 0 {
		t.Errorf("deleted record listed: %+v", after)
	}
	if err := rs.Apply(ctx, 424242, transfer.Update{Touch: true}); !errors.Is(err, transfer.ErrNotFound) {
		t.Errorf("unknown id: %v", err)
	}

	// The janitor's views: the highest id, the live files of a server, the reserved bytes.
	if max, err := rs.MaxID(ctx); err != nil || max != id3 {
		t.Errorf("MaxID %d %v, want %d", max, err, id3)
	}
	on, _ := rs.OnServer(ctx, "10.0.0.1")
	if len(on) != 2 || on[0].ID != id2 || on[1].ID != id3 {
		t.Errorf("OnServer: %+v", on)
	}
	if other, _ := rs.OnServer(ctx, "10.0.0.2"); len(other) != 0 {
		t.Errorf("another server: %+v", other)
	}
	if total, err := rs.Reserved(ctx); err != nil || total != 200 {
		t.Errorf("Reserved %d %v, want 200 (the deleted file holds nothing)", total, err)
	}
}

// The setting: stored keys win over the defaults, an update is validated as a whole and an unset quota stays unset.
func TestSettingsLoadAndValidate(t *testing.T) {
	conn := sysConn(t)
	ctx := context.Background()
	if s, err := transfer.Load(ctx, conn); err != nil || s != transfer.DefaultSettings() || s.Enabled {
		t.Fatalf("defaults: %+v %v", s, err)
	}
	on, hours, file, quota, conc := true, 72, int64(5<<30), int64(9<<30), 3
	vals, errs := transfer.Validate(&api.DataTransferSettingsUpdate{Enabled: &on, RetentionHours: &hours, MaxFileBytes: &file, StagingQuotaBytes: &quota, MaxConcurrent: &conc})
	if len(errs) != 0 || len(vals) != 5 {
		t.Fatalf("Validate: %v %v", vals, errs)
	}
	for k, v := range vals {
		raw, _ := json.Marshal(v)
		if _, err := conn.Exec(ctx, `INSERT INTO kingres_cp.cluster_setting (key, value) VALUES ($1, $2)`, k, raw); err != nil {
			t.Fatal(err)
		}
	}
	s, err := transfer.Load(ctx, conn)
	want := transfer.Settings{Enabled: true, RetentionHours: 72, MaxFileBytes: 5 << 30, StagingQuotaBytes: 9 << 30, MaxConcurrent: 3}
	if err != nil || s != want {
		t.Errorf("Load: %+v %v", s, err)
	}
	// A value out of range in the table (an older or hand-edited row) falls back to the default.
	if _, err := conn.Exec(ctx, `UPDATE kingres_cp.cluster_setting SET value = '9999' WHERE key = 'dataTransfer.retentionHours'`); err != nil {
		t.Fatal(err)
	}
	if s, _ = transfer.Load(ctx, conn); s.RetentionHours != transfer.DefaultRetentionHours {
		t.Errorf("an out-of-range retention was taken: %d", s.RetentionHours)
	}
}
