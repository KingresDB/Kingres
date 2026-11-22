// SPDX-License-Identifier: AGPL-3.0-only

package ops

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/pgtest"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/task"
)

// The task table.setStorage against a real instance (kingres_columnar and kingres_timepart installed on first use, the library preloaded by the
// instance manager): a plain table and one partition are converted to columnar and back, the options arrive, the rows stay, and what cannot be
// converted is refused with its reasons.

func storageOps(t *testing.T) (*Ops, *pgx.Conn) {
	t.Helper()
	if !pgtest.Available() {
		t.Skip("KINGRES_PG_BIN is not set; skipping the PostgreSQL integration test")
	}
	in := pgtest.Start(t, nil)
	c := in.Connect(t, "postgres")
	o := &Ops{Service: testService{in, t}, ServerHost: "s1", Log: slog.New(slog.DiscardHandler)}
	e, err := task.NewEngine(task.NewMemStore(), o.Definitions(), task.Options{Holder: "A", Lease: time.Minute, StoreTries: 1})
	if err != nil {
		t.Fatal(err)
	}
	o.SetEngine(e)
	return o, c
}

func execAll(t *testing.T, c *pgx.Conn, qs ...string) {
	t.Helper()
	for _, q := range qs {
		if _, err := c.Exec(context.Background(), q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
}

func scalar(t *testing.T, c *pgx.Conn, q string) string {
	t.Helper()
	var v *string
	if err := c.QueryRow(context.Background(), "SELECT ("+q+")::text").Scan(&v); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	if v == nil {
		return ""
	}
	return *v
}

func runStorage(t *testing.T, o *Ops, in any, subject string) task.Task {
	t.Helper()
	tk, err := o.StartTask(context.Background(), KindSetStorage, "table", subject, in, "admin")
	if err != nil {
		t.Fatal(err)
	}
	o.Wait()
	got, err := o.Engine().Get(context.Background(), tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestIntegrationSetStorageOfAPlainTable(t *testing.T) {
	o, c := storageOps(t)
	ctx := context.Background()
	execAll(t, c, `CREATE TABLE public.facts (id int, v text)`, `INSERT INTO public.facts SELECT g, 'v' || g FROM generate_series(1, 3000) g`)
	lvl, stripe := 5, 2000
	in, err := o.PrepareSetStorage(ctx, "admin", "postgres", "public", "facts", "", api.TableStorageRequest{AccessMethod: "columnar", Compression: "zstd", CompressionLevel: &lvl, StripeRowCount: &stripe})
	if err != nil {
		t.Fatal(err)
	}
	got := runStorage(t, o, in, "postgres/public.facts")
	if got.State != task.Done {
		t.Fatalf("%s %+v", got.State, got.Error)
	}
	if names := stepNames(got); strings.Join(names, "|") != "Check the table|Convert the storage|Analyze the table" {
		t.Errorf("steps %v", names)
	}
	if am := scalar(t, c, `SELECT amname FROM pg_class c JOIN pg_am a ON a.oid = c.relam WHERE c.oid = 'public.facts'::regclass`); am != "columnar" {
		t.Fatalf("access method %q", am)
	}
	if n := scalar(t, c, `SELECT count(*) || ' ' || sum(id) FROM public.facts`); n != "3000 4501500" {
		t.Errorf("rows after the conversion: %s", n)
	}
	if opt := scalar(t, c, `SELECT compression || ' ' || compression_level || ' ' || stripe_row_count FROM kingres.columnar_options('public.facts')`); opt != "zstd 5 2000" {
		t.Errorf("options %q", opt)
	}
	if rt := scalar(t, c, `SELECT reltuples::int FROM pg_class WHERE oid = 'public.facts'::regclass`); rt != "3000" {
		t.Errorf("the table was not analyzed: reltuples %s", rt)
	}
	// The same request again does nothing (the step says so).
	in2, err := o.PrepareSetStorage(ctx, "admin", "postgres", "public", "facts", "", api.TableStorageRequest{AccessMethod: "columnar"})
	if err != nil {
		t.Fatal(err)
	}
	again := runStorage(t, o, in2, "postgres/public.facts")
	if again.State != task.Done {
		t.Fatalf("the repeat: %s %+v", again.State, again.Error)
	}
	// and back to heap
	in3, err := o.PrepareSetStorage(ctx, "admin", "postgres", "public", "facts", "", api.TableStorageRequest{AccessMethod: "heap"})
	if err != nil {
		t.Fatal(err)
	}
	if back := runStorage(t, o, in3, "postgres/public.facts"); back.State != task.Done {
		t.Fatalf("back: %s %+v", back.State, back.Error)
	}
	if am := scalar(t, c, `SELECT amname FROM pg_class c JOIN pg_am a ON a.oid = c.relam WHERE c.oid = 'public.facts'::regclass`); am != "heap" {
		t.Errorf("access method %q", am)
	}
	if n := scalar(t, c, `SELECT count(*) FROM public.facts`); n != "3000" {
		t.Errorf("rows after the way back: %s", n)
	}
}

func TestIntegrationSetStorageOfAPartition(t *testing.T) {
	o, c := storageOps(t)
	ctx := context.Background()
	execAll(t, c,
		`CREATE TABLE public.ev (ts timestamptz NOT NULL, n int) PARTITION BY RANGE (ts)`,
		`CREATE TABLE public.ev_1 PARTITION OF public.ev FOR VALUES FROM ('2026-01-01 00:00:00+00') TO ('2026-02-01 00:00:00+00')`,
		`CREATE TABLE public.ev_2 PARTITION OF public.ev FOR VALUES FROM ('2026-02-01 00:00:00+00') TO ('2026-03-01 00:00:00+00')`,
		`INSERT INTO public.ev SELECT '2026-01-05 00:00:00+00'::timestamptz + g * interval '1 minute', g FROM generate_series(1, 1500) g`,
		`INSERT INTO public.ev SELECT '2026-02-05 00:00:00+00'::timestamptz + g * interval '1 minute', g FROM generate_series(1, 500) g`)
	am := func(rel string) string {
		return scalar(t, c, `SELECT amname FROM pg_class c JOIN pg_am a ON a.oid = c.relam WHERE c.oid = '`+rel+`'::regclass`)
	}
	in, err := o.PrepareSetStorage(ctx, "admin", "postgres", "public", "ev", "ev_1", api.TableStorageRequest{AccessMethod: "columnar", SortBy: []string{"n"}})
	if err != nil {
		t.Fatal(err)
	}
	got := runStorage(t, o, in, "postgres/public.ev_1")
	if got.State != task.Done {
		t.Fatalf("%s %+v", got.State, got.Error)
	}
	if names := stepNames(got); strings.Join(names, "|") != "Check the partition|Convert the partition|Analyze the partition" {
		t.Errorf("steps %v", names)
	}
	if am("public.ev_1") != "columnar" || am("public.ev_2") != "heap" {
		t.Errorf("formats: ev_1 %s, ev_2 %s", am("public.ev_1"), am("public.ev_2"))
	}
	if n := scalar(t, c, `SELECT count(*) || ' ' || sum(n) FROM public.ev`); n != "2000 1251000" {
		t.Errorf("rows after the conversion: %s", n)
	}
	// the rows of the partition were written in the order asked
	if first := scalar(t, c, `SELECT n FROM public.ev_1 LIMIT 1`); first != "1" {
		t.Errorf("first row %s", first)
	}
	in2, _ := o.PrepareSetStorage(ctx, "admin", "postgres", "public", "ev", "ev_1", api.TableStorageRequest{AccessMethod: "heap"})
	if back := runStorage(t, o, in2, "postgres/public.ev_1"); back.State != task.Done || am("public.ev_1") != "heap" {
		t.Fatalf("back: %s %+v, %s", back.State, back.Error, am("public.ev_1"))
	}
}

func TestIntegrationSetStorageRefusals(t *testing.T) {
	o, c := storageOps(t)
	ctx := context.Background()
	execAll(t, c,
		`CREATE TABLE public.parent_t (id int PRIMARY KEY)`,
		`CREATE TABLE public.child_t (id int, p int REFERENCES public.parent_t (id))`,
		`CREATE TABLE public.ev (ts timestamptz NOT NULL) PARTITION BY RANGE (ts)`,
		`CREATE TABLE public.ev_1 PARTITION OF public.ev FOR VALUES FROM ('2026-01-01 00:00:00+00') TO ('2026-02-01 00:00:00+00')`,
		`CREATE TABLE public.other (x int)`)
	cases := []struct {
		name                 string
		table, partition, am string
		code                 problem.Code
		mustSay              string
	}{
		{"a table with foreign keys", "child_t", "", "columnar", problem.ConversionRefused, "foreign keys"},
		{"the table that is referenced", "parent_t", "", "columnar", problem.ConversionRefused, "foreign keys"},
		{"a partitioned table", "ev", "", "columnar", problem.ConversionRefused, "partitions"},
		{"a partition through the operation for a table", "ev_1", "", "columnar", problem.ConversionRefused, "partition"},
		{"a table that does not exist", "nope", "", "columnar", problem.NotFound, ""},
		{"a partition that does not exist", "ev", "nope", "columnar", problem.NotFound, ""},
		{"a table that is not a partition of the table", "ev", "other", "columnar", problem.NotFound, ""},
	}
	for _, tc := range cases {
		_, err := o.PrepareSetStorage(ctx, "admin", "postgres", "public", tc.table, tc.partition, api.TableStorageRequest{AccessMethod: tc.am})
		p := wantCode(t, err, tc.code)
		if tc.mustSay != "" && !strings.Contains(p.Detail, tc.mustSay) {
			t.Errorf("%s: %q does not say %q", tc.name, p.Detail, tc.mustSay)
		}
	}
	// A request that is not valid is a 400 with the field, before the database is asked.
	bad := 99
	for f, req := range map[string]api.TableStorageRequest{
		"/accessMethod":     {AccessMethod: "rocksdb"},
		"/compressionLevel": {AccessMethod: "columnar", CompressionLevel: &bad},
		"/compression":      {AccessMethod: "columnar", Compression: "gzip"},
		"/sortBy/0":         {AccessMethod: "columnar", SortBy: []string{"a; drop table x"}},
	} {
		_, err := o.PrepareSetStorage(ctx, "admin", "postgres", "public", "ev", "ev_1", req)
		p := wantCode(t, err, problem.ValidationFailed)
		found := false
		for _, e := range p.Errors {
			found = found || e.Field == f
		}
		if !found {
			t.Errorf("%s: fields %+v", f, p.Errors)
		}
	}
	// a sort order belongs to a partition
	_, err := o.PrepareSetStorage(ctx, "admin", "postgres", "public", "other", "", api.TableStorageRequest{AccessMethod: "columnar", SortBy: []string{"x"}})
	wantCode(t, err, problem.ValidationFailed)
	// the options of the columnar format are refused for heap
	lvl := 3
	_, err = o.PrepareSetStorage(ctx, "admin", "postgres", "public", "other", "", api.TableStorageRequest{AccessMethod: "heap", CompressionLevel: &lvl})
	wantCode(t, err, problem.ValidationFailed)
}

// A table that took a foreign key between the request and the run is refused by the first step, with the code and the reason.
func TestIntegrationSetStorageChecksAgainWhenTheTaskRuns(t *testing.T) {
	o, c := storageOps(t)
	ctx := context.Background()
	execAll(t, c, `CREATE TABLE public.a (id int PRIMARY KEY)`, `CREATE TABLE public.b (id int, a int)`)
	in, err := o.PrepareSetStorage(ctx, "admin", "postgres", "public", "b", "", api.TableStorageRequest{AccessMethod: "columnar"})
	if err != nil {
		t.Fatal(err)
	}
	execAll(t, c, `ALTER TABLE public.b ADD FOREIGN KEY (a) REFERENCES public.a (id)`)
	got := runStorage(t, o, in, "postgres/public.b")
	if got.State != task.Failed || got.Error == nil || got.Error.Code != string(problem.ConversionRefused) || got.Error.Step != stepStorageCheckTable || !strings.Contains(got.Error.Message, "foreign keys") {
		t.Fatalf("%s %+v", got.State, got.Error)
	}
	if am := scalar(t, c, `SELECT amname FROM pg_class c JOIN pg_am a ON a.oid = c.relam WHERE c.oid = 'public.b'::regclass`); am != "heap" {
		t.Errorf("the table was converted: %s", am)
	}
}

// Without the preloaded library nothing is converted, and the problem names the library and the server.
func TestSetStorageNeedsThePreloadedLibrary(t *testing.T) {
	fe := newFakeExt()
	fe.preloaded = map[string]bool{}
	o := &Ops{ServerHost: "s1", extBackendOverride: fe, Log: slog.New(slog.DiscardHandler)}
	err := o.ensureExtensions(context.Background(), "shop", []string{ExtColumnar}, true)
	p := wantCode(t, err, problem.ExtensionMissing)
	if !strings.Contains(p.Detail, "kingres_columnar") || !strings.Contains(p.Detail, "shared_preload_libraries") || !strings.Contains(p.Detail, "s1") {
		t.Errorf("detail %q", p.Detail)
	}
	// a package without the extension is the same problem
	fe2 := newFakeExt()
	delete(fe2.available, ExtTimepart)
	o.extBackendOverride = fe2
	wantCode(t, o.ensureExtensions(context.Background(), "shop", []string{ExtTimepart}, false), problem.ExtensionMissing)
	// with both present they are installed on first use
	o.extBackendOverride = fe2
	if err := o.ensureExtensions(context.Background(), "shop", []string{ExtColumnar}, true); err != nil || fe2.installed[ExtColumnar] != "0.2" {
		t.Errorf("%v, installed %v", err, fe2.installed)
	}
}

func TestValidateStorageAcceptsTheOptionsOfTheFormat(t *testing.T) {
	lvl, st, ch := 19, 1000, 100
	p, err := validateStorage(api.TableStorageRequest{AccessMethod: "columnar", Compression: "lz4", CompressionLevel: &lvl, StripeRowCount: &st, ChunkRowCount: &ch, SortBy: []string{"ts", "id"}}, true)
	if err != nil || p.CompressionLevel != 19 || p.StripeRowCount != 1000 || p.ChunkRowCount != 100 || len(p.SortBy) != 2 {
		t.Fatalf("%+v %v", p, err)
	}
	if got := p.optionsJSON(); got != `{"chunk_row_count":100,"compression":"lz4","compression_level":19,"stripe_row_count":1000}` {
		t.Errorf("options %v", got)
	}
	if (storageParams{}).optionsJSON() != nil {
		t.Error("no options must give null, so that the table keeps the settings of the session")
	}
	for _, n := range []int{0, 20} {
		n := n
		if _, err := validateStorage(api.TableStorageRequest{AccessMethod: "columnar", CompressionLevel: &n}, false); n == 20 && err == nil {
			t.Error("level 20 accepted")
		}
	}
}

func TestLibraryListed(t *testing.T) {
	for list, want := range map[string]bool{
		"kingres_shard,kingres_ledger,kingres_columnar": true,
		"kingres_shard, kingres_columnar":               true,
		`"kingres_columnar"`:                            true,
		"kingres_shard,kingres_ledger":                  false,
		"kingres_columnar2":                             false,
		"":                                              false,
	} {
		if got := libraryListed(list, "kingres_columnar"); got != want {
			t.Errorf("%q: %v", list, got)
		}
	}
}
