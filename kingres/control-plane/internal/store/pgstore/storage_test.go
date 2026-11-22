// SPDX-License-Identifier: AGPL-3.0-only

package pgstore

import (
	"context"
	"testing"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/pgtest"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/store"
)

// listTables and getTable say how a table is stored (contract 0.17.0): the access method of a table, the formats of the partitions of a partitioned
// table, the totals of the columnar format, and for a table that is range-partitioned on a time column the column, the count, the default partition
// and whether it has a policy of kingres_timepart. The storage filter matches a partitioned table when any partition does.
func TestIntegrationTableStorage(t *testing.T) {
	s := newTestStore(t)
	admin := pgtest.FixtureConnect(t, "postgres")
	bg := context.Background()
	if _, err := admin.Exec(bg, "CREATE DATABASE storage_cp"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// The pool of the store may hold a connection to the database; end it, then drop.
		_, _ = admin.Exec(bg, "ALTER ROLE cp_service NOSUPERUSER")
		_, _ = admin.Exec(bg, "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = 'storage_cp'")
		_, _ = admin.Exec(bg, "DROP DATABASE IF EXISTS storage_cp")
	})
	// The service role of kingresd is a superuser in a real cluster (the policy view and the columnar functions need it); the fixture role is not.
	if _, err := admin.Exec(bg, "ALTER ROLE cp_service SUPERUSER"); err != nil {
		t.Fatal(err)
	}
	c := pgtest.FixtureConnect(t, "storage_cp")
	for _, q := range []string{
		"CREATE EXTENSION kingres_columnar",
		"CREATE EXTENSION kingres_timepart",
		"CREATE TABLE heap_t (id int, v text)",
		"CREATE TABLE col_t (id int, v text) USING columnar",
		"INSERT INTO col_t SELECT g, repeat('x', 100) FROM generate_series(1, 5000) g",
		"SELECT kingres.columnar_set_options('col_t', compression => 'zstd', compression_level => 3)",
		"CREATE TABLE ev (ts timestamptz NOT NULL, n int) PARTITION BY RANGE (ts)",
		"CREATE TABLE ev_1 PARTITION OF ev FOR VALUES FROM ('2026-01-01 00:00:00+00') TO ('2026-01-02 00:00:00+00')",
		"CREATE TABLE ev_2 PARTITION OF ev FOR VALUES FROM ('2026-01-02 00:00:00+00') TO ('2026-01-03 00:00:00+00') ",
		"ALTER TABLE ev_2 SET ACCESS METHOD columnar",
		"CREATE TABLE evd (d date NOT NULL) PARTITION BY RANGE (d)",
		"CREATE TABLE evd_x PARTITION OF evd DEFAULT",
		"CREATE TABLE lst (k text) PARTITION BY LIST (k)",
		"CREATE TABLE nopol (d date NOT NULL) PARTITION BY RANGE (d)",
		"CREATE TABLE nopol_1 PARTITION OF nopol FOR VALUES FROM ('2026-01-01') TO ('2026-02-01')",
		"SELECT kingres.timepart_set_policy('ev', '1 day')",
	} {
		if _, err := c.Exec(bg, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	byName := func(items []api.Table) map[string]api.Table {
		m := map[string]api.Table{}
		for _, it := range items {
			m[it.Name] = it
		}
		return m
	}
	page, err := s.Tables(ctx(t), "storage_cp", store.TableFilter{}, store.Page{Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	all := byName(page.Items)
	str := func(p *string) string {
		if p == nil {
			return "<nil>"
		}
		return *p
	}
	if st := all["heap_t"].Storage; st == nil || str(st.AccessMethod) != "heap" || st.PartitionFormats != nil || st.Columnar != nil {
		t.Errorf("heap_t: %+v", st)
	}
	col := all["col_t"].Storage
	if col == nil || str(col.AccessMethod) != "columnar" || col.Columnar == nil || col.Columnar.Rows != 5000 || col.Columnar.Stripes < 1 || col.Columnar.BytesOnDisk <= 0 ||
		col.Columnar.BytesUncompressed < col.Columnar.BytesOnDisk || col.Columnar.CompressionRatio == nil || *col.Columnar.CompressionRatio < 1 ||
		str(col.Columnar.Compression) != "zstd" || col.Columnar.CompressionLevel == nil || *col.Columnar.CompressionLevel != 3 {
		t.Errorf("col_t: %+v %+v", col, col.Columnar)
	}
	ev := all["ev"]
	if st := ev.Storage; st == nil || st.AccessMethod != nil || st.PartitionFormats == nil || *st.PartitionFormats != (api.PartitionFormats{Heap: 1, Columnar: 1}) {
		t.Errorf("ev storage: %+v", ev.Storage)
	}
	if ev.Storage != nil && (ev.Storage.Columnar == nil || ev.Storage.Columnar.Stripes != 0) {
		t.Errorf("ev columnar totals (one empty columnar partition): %+v", ev.Storage.Columnar)
	}
	if p := ev.Partitioning; p == nil || p.Method != "range" || p.Key != "ts" || p.ColumnType != "timestamptz" || p.PartitionCount != 2 ||
		p.HasPolicy == nil || !*p.HasPolicy || p.DefaultPartition == nil || *p.DefaultPartition {
		t.Errorf("ev partitioning: %+v", ev.Partitioning)
	}
	if p := all["nopol"].Partitioning; p == nil || p.ColumnType != "date" || p.PartitionCount != 1 || p.HasPolicy == nil || *p.HasPolicy || p.DefaultPartition == nil || *p.DefaultPartition {
		t.Errorf("nopol partitioning: %+v", p)
	}
	if p := all["evd"].Partitioning; p == nil || p.DefaultPartition == nil || !*p.DefaultPartition || p.PartitionCount != 1 || p.HasPolicy == nil || *p.HasPolicy {
		t.Errorf("evd partitioning: %+v", p)
	}
	if p := all["lst"].Partitioning; p != nil {
		t.Errorf("a list-partitioned table has no time partitioning: %+v", p)
	}
	if p := all["heap_t"].Partitioning; p != nil {
		t.Errorf("heap_t partitioning: %+v", p)
	}
	// the filter
	names := func(format string) map[string]bool {
		pg, err := s.Tables(ctx(t), "storage_cp", store.TableFilter{Storage: format}, store.Page{Limit: 50})
		if err != nil {
			t.Fatal(err)
		}
		m := map[string]bool{}
		for _, it := range pg.Items {
			m[it.Name] = true
		}
		return m
	}
	if got := names("columnar"); len(got) != 2 || !got["col_t"] || !got["ev"] {
		t.Errorf("storage=columnar: %v", got)
	}
	if got := names("heap"); !got["heap_t"] || !got["ev"] || !got["nopol"] || !got["evd"] || got["col_t"] {
		t.Errorf("storage=heap: %v", got)
	}
	// getTable has the same properties
	d, err := s.Table(ctx(t), "storage_cp", "public", "ev")
	if err != nil || d.Storage == nil || d.Partitioning == nil || d.Partitioning.ColumnType != "timestamptz" {
		t.Errorf("getTable ev: %+v %v", d.Table, err)
	}
	// the paging of a filtered list stays correct
	pg1, _ := s.Tables(ctx(t), "storage_cp", store.TableFilter{Storage: "columnar"}, store.Page{Limit: 1})
	if len(pg1.Items) != 1 || pg1.NextCursor == nil {
		t.Fatalf("first page: %+v", pg1)
	}
	pg2, _ := s.Tables(ctx(t), "storage_cp", store.TableFilter{Storage: "columnar"}, store.Page{Limit: 1, Cursor: *pg1.NextCursor})
	if len(pg2.Items) != 1 || pg2.Items[0].Name == pg1.Items[0].Name || pg2.NextCursor != nil {
		t.Errorf("second page: %+v", pg2)
	}
}
