// SPDX-License-Identifier: AGPL-3.0-only

package ops

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/task"
)

// P12C-06: new options of a table that is columnar already are set (for the stripes to come), not silently dropped.
func TestIntegrationSetStorageChangesTheOptionsOfAColumnarTable(t *testing.T) {
	o, c := storageOps(t)
	ctx := context.Background()
	execAll(t, c, `CREATE TABLE public.facts (id int, v text)`, `INSERT INTO public.facts SELECT g, 'v' FROM generate_series(1, 2000) g`)
	lvl, lvl2 := 5, 9
	in, err := o.PrepareSetStorage(ctx, "admin", "postgres", "public", "facts", "", api.TableStorageRequest{AccessMethod: "columnar", Compression: "zstd", CompressionLevel: &lvl})
	if err != nil {
		t.Fatal(err)
	}
	if got := runStorage(t, o, in, "postgres/public.facts"); got.State != task.Done {
		t.Fatalf("%s %+v", got.State, got.Error)
	}
	in2, err := o.PrepareSetStorage(ctx, "admin", "postgres", "public", "facts", "", api.TableStorageRequest{AccessMethod: "columnar", Compression: "lz4", CompressionLevel: &lvl2})
	if err != nil {
		t.Fatal(err)
	}
	got := runStorage(t, o, in2, "postgres/public.facts")
	if got.State != task.Done {
		t.Fatalf("%s %+v", got.State, got.Error)
	}
	if opt := scalar(t, c, `SELECT compression || ' ' || compression_level FROM kingres.columnar_options('public.facts')`); opt != "lz4 9" {
		t.Errorf("options after the second request: %q (was zstd 5)", opt)
	}
	if n := scalar(t, c, `SELECT count(*) FROM public.facts`); n != "2000" {
		t.Errorf("rows %s", n)
	}
	// the preview says so
	prev, err := o.PreviewSetStorage(ctx, "admin", "postgres", "public", "facts", "", api.TableStorageRequest{AccessMethod: "columnar", Compression: "none"})
	if err != nil || !prev.WillConvert || len(prev.Blockers) != 0 || !strings.Contains(strings.Join(prev.Notes, " "), "stripes") {
		t.Errorf("preview %+v %v", prev, err)
	}
	same, _ := o.PreviewSetStorage(ctx, "admin", "postgres", "public", "facts", "", api.TableStorageRequest{AccessMethod: "columnar"})
	if same.WillConvert || same.Current == nil || *same.Current != "columnar" {
		t.Errorf("columnar to columnar without options does nothing: %+v", same)
	}
}

// P12C-07: the conversion waits for a lock with a short lock_timeout and says so, and gives up with operation_in_progress when the lock stays; it goes through
// when the lock goes.
func TestIntegrationConversionWaitsForALockInsteadOfBlockingTheTable(t *testing.T) {
	o, c := storageOps(t)
	ctx := context.Background()
	oldT, oldP, oldL := storageLockTimeout, storageLockPause, storageLockLimit
	storageLockTimeout, storageLockPause, storageLockLimit = 150*time.Millisecond, 100*time.Millisecond, 1500*time.Millisecond
	defer func() { storageLockTimeout, storageLockPause, storageLockLimit = oldT, oldP, oldL }()
	execAll(t, c, `CREATE TABLE public.busy (id int)`, `INSERT INTO public.busy VALUES (1)`)
	holder := second(t, c)
	if _, err := holder.Exec(ctx, `BEGIN`); err != nil {
		t.Fatal(err)
	}
	if _, err := holder.Exec(ctx, `LOCK TABLE public.busy IN ACCESS SHARE MODE`); err != nil {
		t.Fatal(err)
	}
	in, err := o.PrepareSetStorage(ctx, "admin", "postgres", "public", "busy", "", api.TableStorageRequest{AccessMethod: "columnar"})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	got := runStorage(t, o, in, "postgres/public.busy")
	if got.State != task.Failed || got.Error == nil || got.Error.Code != string(problem.OperationInProgress) || !got.Error.Retryable || got.Error.Step != stepStorageConvert {
		t.Fatalf("%s %+v", got.State, got.Error)
	}
	if time.Since(start) > 30*time.Second {
		t.Errorf("it waited %v", time.Since(start))
	}
	// a reader of the table is not queued behind the conversion: the lock request is dropped at each try
	if n := scalar(t, c, `SELECT count(*) FROM public.busy`); n != "1" {
		t.Errorf("rows %s", n)
	}
	if am := scalar(t, c, `SELECT amname FROM pg_class c JOIN pg_am a ON a.oid = c.relam WHERE c.oid = 'public.busy'::regclass`); am != "heap" {
		t.Errorf("converted under a lock: %s", am)
	}
	// the lock goes while the task waits: it goes through
	storageLockLimit = 20 * time.Second
	go func() {
		time.Sleep(800 * time.Millisecond)
		_, _ = holder.Exec(ctx, `ROLLBACK`)
	}()
	again := runStorage(t, o, in, "postgres/public.busy")
	if again.State != task.Done {
		t.Fatalf("after the lock went: %s %+v", again.State, again.Error)
	}
	if am := scalar(t, c, `SELECT amname FROM pg_class c JOIN pg_am a ON a.oid = c.relam WHERE c.oid = 'public.busy'::regclass`); am != "columnar" {
		t.Errorf("not converted: %s", am)
	}
}

// P12C-04: the table of a role that is not a superuser converts, as a run of a policy of it does: both drive the extension with the owners trusted.
func TestIntegrationAPartitionOfANonSuperuserOwnerConverts(t *testing.T) {
	o, c := storageOps(t)
	ctx := context.Background()
	execAll(t, c,
		`CREATE ROLE app_owner NOLOGIN`, `GRANT CREATE ON SCHEMA public TO app_owner`, // the owner makes the new partition in its schema
		`CREATE TABLE public.ev (ts timestamptz NOT NULL, n int) PARTITION BY RANGE (ts)`,
		`CREATE TABLE public.ev_1 PARTITION OF public.ev FOR VALUES FROM ('2026-01-01 00:00:00+00') TO ('2026-02-01 00:00:00+00')`,
		`INSERT INTO public.ev SELECT '2026-01-05 00:00:00+00'::timestamptz + g * interval '1 minute', g FROM generate_series(1, 500) g`,
		`CREATE TABLE public.plain_t (id int)`, `INSERT INTO public.plain_t VALUES (1)`,
		`ALTER TABLE public.ev OWNER TO app_owner`, `ALTER TABLE public.ev_1 OWNER TO app_owner`, `ALTER TABLE public.plain_t OWNER TO app_owner`)
	allowOwners(t, c, true)
	in, err := o.PrepareSetStorage(ctx, "admin", "postgres", "public", "ev", "ev_1", api.TableStorageRequest{AccessMethod: "columnar"})
	if err != nil {
		t.Fatal(err)
	}
	got := runStorage(t, o, in, "postgres/public.ev_1")
	if got.State != task.Done {
		t.Fatalf("a partition of a table owned by a role that is not a superuser: %s %+v", got.State, got.Error)
	}
	if am := scalar(t, c, `SELECT amname FROM pg_class c JOIN pg_am a ON a.oid = c.relam WHERE c.relname = 'ev_1'`); am != "columnar" {
		t.Errorf("access method %s", am)
	}
	if n := scalar(t, c, `SELECT count(*) FROM public.ev`); n != "500" {
		t.Errorf("rows %s", n)
	}
	in2, err := o.PrepareSetStorage(ctx, "admin", "postgres", "public", "plain_t", "", api.TableStorageRequest{AccessMethod: "columnar"})
	if err != nil {
		t.Fatal(err)
	}
	if got := runStorage(t, o, in2, "postgres/public.plain_t"); got.State != task.Done {
		t.Fatalf("a plain table of that owner: %s %+v", got.State, got.Error)
	}
}

// The preview gives typed reasons, with a table that cannot be converted and one that can, and installs nothing.
func TestIntegrationPreviewSetStorageGivesTypedReasonsAndInstallsNothing(t *testing.T) {
	o, c := storageOps(t)
	ctx := context.Background()
	execAll(t, c,
		`CREATE TABLE public.refs (id int PRIMARY KEY)`, `CREATE TABLE public.uses (id int, r int REFERENCES public.refs (id))`,
		`CREATE TABLE public.ok_t (id int)`,
		`CREATE TABLE public.ev (ts timestamptz NOT NULL) PARTITION BY RANGE (ts)`,
		`CREATE TABLE public.ev_1 PARTITION OF public.ev FOR VALUES FROM ('2026-01-01 00:00:00+00') TO ('2026-02-01 00:00:00+00')`)
	// the library is preloaded by the instance manager; the extensions are not installed yet
	if n := scalar(t, c, `SELECT count(*) FROM pg_extension WHERE extname LIKE 'kingres_%'`); n != "0" {
		t.Skipf("the instance already has %s extensions", n)
	}
	prev, err := o.PreviewSetStorage(ctx, "admin", "postgres", "public", "uses", "", api.TableStorageRequest{AccessMethod: "columnar"})
	if err != nil || prev.WillConvert || len(prev.Blockers) != 1 || prev.Blockers[0].Code != "foreign_keys" || prev.Blockers[0].Object != "public.uses" || prev.Target != "columnar" {
		t.Fatalf("%+v %v", prev, err)
	}
	if prev.Current == nil || *prev.Current != "heap" {
		t.Errorf("current %v", prev.Current)
	}
	ok, err := o.PreviewSetStorage(ctx, "admin", "postgres", "public", "ok_t", "", api.TableStorageRequest{AccessMethod: "columnar"})
	if err != nil || !ok.WillConvert || len(ok.Blockers) != 0 || !strings.Contains(strings.Join(ok.Notes, " "), "ACCESS EXCLUSIVE") {
		t.Errorf("%+v %v", ok, err)
	}
	pt, err := o.PreviewSetStorage(ctx, "admin", "postgres", "public", "ev", "", api.TableStorageRequest{AccessMethod: "columnar"})
	if err != nil || pt.WillConvert || len(pt.Blockers) != 1 || pt.Blockers[0].Code != "partitioned_table" || pt.Current != nil {
		t.Errorf("a partitioned table: %+v %v", pt, err)
	}
	part, err := o.PreviewSetStorage(ctx, "admin", "postgres", "public", "ev", "ev_1", api.TableStorageRequest{AccessMethod: "columnar"})
	if err != nil || len(part.Notes) == 0 {
		t.Errorf("a partition before kingres_timepart is installed: %+v %v", part, err)
	}
	if n := scalar(t, c, `SELECT count(*) FROM pg_extension WHERE extname LIKE 'kingres_%'`); n != "0" {
		t.Errorf("a preview installed %s extensions", n)
	}
	// the real request carries the same reasons as Problem.blockers
	_, err = o.PrepareSetStorage(ctx, "admin", "postgres", "public", "uses", "", api.TableStorageRequest{AccessMethod: "columnar"})
	p := wantCode(t, err, problem.ConversionRefused)
	bl, isList := p.Blockers.([]api.TableBlocker)
	if !isList || len(bl) != 1 || bl[0].Code != "foreign_keys" {
		t.Errorf("blockers %v", p.Blockers)
	}
	// an invalid request, and a table that is not there, are the same problems as for the real one
	_, err = o.PreviewSetStorage(ctx, "admin", "postgres", "public", "ok_t", "", api.TableStorageRequest{AccessMethod: "rocksdb"})
	wantCode(t, err, problem.ValidationFailed)
	_, err = o.PreviewSetStorage(ctx, "admin", "postgres", "public", "nowhere", "", api.TableStorageRequest{AccessMethod: "columnar"})
	wantCode(t, err, problem.NotFound)
}
