// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package ops

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/staging"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/archive"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/transfer"
)

// Unit tests of what the database export makes from the facts of the catalog: the catalog document, the facts of the normaliser, the labels, the data entries,
// the fingerprint and the manifest. They need no database.

// fixtureFacts is a database with one table of each kind.
func fixtureFacts() *dbFacts {
	f := &dbFacts{byOID: map[int64]*dbTable{}, Colocs: map[int]*dbColoc{}, UserPart: map[int64][]dbPartition{}, PartKey: map[int64]string{}, Buckets: map[int64]bool{},
		columnar: map[int64]map[string]string{}, Encoding: "UTF8", Collate: "en_US.UTF-8", Ctype: "en_US.UTF-8", Provider: "libc", PGMajor: 18,
		Extensions: []archive.ManifestExtension{{Name: "kingres_shard", Version: "0.9", Schema: "pg_catalog"}, {Name: "citext", Version: "1.6", Schema: "public"}}}
	add := func(t dbTable) {
		f.Tables = append(f.Tables, t)
	}
	add(dbTable{OID: 1, Schema: "app", Name: "items", Relkind: "r", AM: "heap", Cols: []string{"id", "label"}, All: []string{"id bigint", "label text"}, Rows: 10, Bytes: 8192})
	add(dbTable{OID: 2, Schema: "public", Name: "acc", Relkind: "r", AM: "heap", Cols: []string{"id", "bal"}, All: []string{"id bigint", "bal integer"},
		Ledger: &dbLedger{Mode: "updatable", Policy: "optional", HeadHeight: 3, HeadHash: "ab", HistoryOID: 3, HistoryName: "public.acc_ledger_history", HistoryCols: []string{"id", "bal", "ledger_txid"}}})
	add(dbTable{OID: 3, Schema: "public", Name: "acc_ledger_history", Relkind: "r", AM: "heap", Cols: []string{"id", "bal", "ledger_txid"}, All: []string{"id bigint", "bal integer", "ledger_txid bigint"}})
	add(dbTable{OID: 4, Schema: "public", Name: "c_events", Relkind: "r", AM: "columnar", Cols: []string{"id", "v"}, All: []string{"id bigint", "v text"}})
	add(dbTable{OID: 5, Schema: "public", Name: "d_hash", Relkind: "p", AM: "", Cols: []string{"id", "v"}, All: []string{"id bigint", "v text"}, Method: "hash", DistColumn: "id", Coloc: 7})
	add(dbTable{OID: 6, Schema: "public", Name: "d_hash_b0000", Relkind: "r", IsPartition: true, Parent: 5, Cols: []string{"id", "v"}, All: []string{"id bigint", "v text"}})
	add(dbTable{OID: 7, Schema: "public", Name: "d_part", Relkind: "p", Cols: []string{"id", "ts"}, All: []string{"id bigint", "ts timestamptz"}, Method: "hash", DistColumn: "id", Coloc: 7})
	add(dbTable{OID: 8, Schema: "public", Name: "inh_child", Relkind: "r", AM: "heap", Cols: []string{"id", "extra"}, All: []string{"id integer", "extra integer"}, Inherits: []int64{9}})
	add(dbTable{OID: 9, Schema: "public", Name: "inh_parent", Relkind: "r", AM: "heap", Cols: []string{"id"}, All: []string{"id integer"}, Children: 1})
	add(dbTable{OID: 10, Schema: "public", Name: "lp", Relkind: "p", Cols: []string{"id", "d"}, All: []string{"id integer", "d date"}})
	add(dbTable{OID: 11, Schema: "public", Name: "lp_2026", Relkind: "r", IsPartition: true, Parent: 10, AM: "heap", Cols: []string{"id", "d"}, All: []string{"id integer", "d date"}})
	add(dbTable{OID: 12, Schema: "public", Name: "refdata", Relkind: "r", AM: "heap", Cols: []string{"id"}, All: []string{"id integer"}, Method: "reference"})
	for i := range f.Tables {
		f.byOID[f.Tables[i].OID] = &f.Tables[i]
	}
	f.byOID[2].Ledger = f.Tables[1].Ledger
	f.Colocs[7] = &dbColoc{ID: 7, Buckets: 8, Type: "bigint", Groups: 3}
	f.Buckets[6] = true
	f.BucketNames = []string{"public.d_hash_b0000"}
	f.UserPart[7] = []dbPartition{{Name: "d_part_2026", Bound: "FROM ('2026-01-01 00:00:00+00') TO ('2027-01-01 00:00:00+00')"}, {Name: "d_part_rest", Bound: "DEFAULT"}}
	f.PartKey[7] = "RANGE (ts)"
	f.columnar[4] = map[string]string{"compression": "zstd"}
	f.Seqs = []dbSeq{{Schema: "app", Name: "items_id_seq", Owned: "app.items.id", Last: 5, Called: true}, {Schema: "public", Name: "orphan_seq", Owned: "public.gone.id", Last: 1}}
	f.Policies = []dbPolicy{{Schema: "public", Table: "ev", Every: "1 day"}, {Schema: "public", Table: "d_hash", Every: "1 day", Enabled: true}}
	return f
}

func TestCatalogOfTheFacts(t *testing.T) {
	f := fixtureFacts()
	c := f.catalog()
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	got, err := archive.ParseCatalog(b)
	if err != nil {
		t.Fatalf("the catalog is not accepted by the strict decoder: %v", err)
	}
	byName := map[string]archive.CatalogTable{}
	for _, tb := range got.Tables {
		byName[tb.Qualified()] = tb
	}
	if _, ok := byName["public.d_hash_b0000"]; ok {
		t.Error("a bucket partition is in the catalog")
	}
	if _, ok := byName["public.acc_ledger_history"]; ok {
		t.Error("a history table is in the catalog")
	}
	for name, want := range map[string]struct{ kind, role, am string }{
		"app.items": {"local", "standalone", "heap"}, "public.acc": {"local", "standalone", "heap"}, "public.c_events": {"local", "standalone", "columnar"},
		"public.d_hash": {"hash", "standalone", "heap"}, "public.d_part": {"hash", "root", "heap"}, "public.inh_child": {"local", "inheritanceChild", "heap"},
		"public.inh_parent": {"local", "inheritanceParent", "heap"}, "public.lp": {"local", "root", "heap"}, "public.lp_2026": {"local", "leaf", "heap"}, "public.refdata": {"reference", "standalone", "heap"},
	} {
		tb := byName[name]
		if tb.Kind != want.kind || tb.TreeRole != want.role || tb.AccessMethod != want.am {
			t.Errorf("%s: kind %q role %q method %q, want %v", name, tb.Kind, tb.TreeRole, tb.AccessMethod, want)
		}
	}
	if byName["public.inh_child"].Parent != "public.inh_parent" || byName["public.lp_2026"].Parent != "public.lp" {
		t.Errorf("parents: %q %q", byName["public.inh_child"].Parent, byName["public.lp_2026"].Parent)
	}
	up := byName["public.d_part"].UserPartitioning
	if up == nil || up.Method != "range" || up.Key != "ts" || len(up.Partitions) != 2 || up.Partitions[1].Bound != "DEFAULT" {
		t.Errorf("user partitioning: %+v", up)
	}
	if l := byName["public.acc"].Ledger; l == nil || l.Mode != "updatable" || l.HeadHeight != 3 || len(l.HistoryColumns) != 3 {
		t.Errorf("ledger: %+v", l)
	}
	if byName["public.c_events"].Columnar["compression"] != "zstd" {
		t.Errorf("columnar options: %v", byName["public.c_events"].Columnar)
	}
	if len(got.ColocationGroups) != 1 || got.ColocationGroups[0].Ref != "c1" || got.ColocationGroups[0].BucketCount != 8 || byName["public.d_hash"].ColocationRef != "c1" {
		t.Errorf("colocation: %+v", got.ColocationGroups)
	}
	// A policy of a table that is not in the catalog and a sequence owned by one that is not are left out.
	if len(got.TimepartPolicies) != 1 || got.TimepartPolicies[0].Table != "d_hash" || len(got.Sequences) != 1 || got.Sequences[0].Name != "items_id_seq" {
		t.Errorf("policies %+v sequences %+v", got.TimepartPolicies, got.Sequences)
	}
}

func TestFactsOfTheNormaliser(t *testing.T) {
	nf := fixtureFacts().normFacts()
	if !nf.Remove["public.d_hash_b0000"] || !nf.Remove["public.acc_ledger_history"] || nf.Remove["public.d_hash"] {
		t.Errorf("remove: %v", nf.Remove)
	}
	if !nf.Ledger["public.acc"] || !nf.Ledger["public.acc_ledger_history"] || nf.Ledger["app.items"] {
		t.Errorf("ledger: %v", nf.Ledger)
	}
	h := nf.Hash["public.d_part"]
	if h.Column != "id" || h.User == nil || h.User.Method != "RANGE" || h.User.Key != "ts" || len(h.User.Partitions) != 2 || nf.Hash["public.d_hash"].User != nil {
		t.Errorf("hash tables: %+v", nf.Hash)
	}
}

func TestDataEntriesAndLabels(t *testing.T) {
	f := fixtureFacts()
	names := func(es []dataEntry) (out []string) {
		for _, e := range es {
			n := e.Table.qualified()
			if e.History {
				n = e.Base + "(history)"
			}
			out = append(out, n)
		}
		return out
	}
	all := names(f.dataEntries(map[string]bool{}, "all"))
	want := "app.items public.acc public.acc(history) public.c_events public.d_hash public.d_part public.inh_child public.inh_parent public.lp public.refdata"
	if strings.Join(all, " ") != want {
		t.Errorf("entries: %v", all)
	}
	for _, e := range f.dataEntries(map[string]bool{}, "all") {
		if e.Table.Name == "inh_parent" != e.Only {
			t.Errorf("FROM ONLY for %s: %v", e.Table.Name, e.Only)
		}
	}
	if got := names(f.dataEntries(map[string]bool{"public.acc": true, "public.d_hash": true}, "all")); strings.Contains(strings.Join(got, " "), "public.acc") || strings.Contains(strings.Join(got, " "), "d_hash") {
		t.Errorf("excluded tables are in the entries: %v", got)
	}
	if len(f.dataEntries(nil, "none")) != 0 {
		t.Error("data mode none has entries")
	}
	// The label of a table over three groups is per group; the others are one snapshot; the archive is then not one snapshot.
	if l, g := f.label(f.Tables[4]); l != "per-group" || g != 3 {
		t.Errorf("label of a hash table over three groups: %s %d", l, g)
	}
	if l, g := f.label(f.Tables[0]); l != "single-snapshot" || g != 1 {
		t.Errorf("label of a local table: %s %d", l, g)
	}
	if f.singleSnapshot() {
		t.Error("an archive with a table over three groups is not one snapshot")
	}
	f.Colocs[7].Groups = 1
	if !f.singleSnapshot() {
		t.Error("an archive of tables on one group is one snapshot")
	}
	if f.groupCount() != 1 {
		t.Errorf("group count %d", f.groupCount())
	}
}

func TestFingerprintChangesWithTheStructure(t *testing.T) {
	a, b := fixtureFacts(), fixtureFacts()
	if a.fingerprint() != b.fingerprint() || len(a.fingerprint()) != 64 {
		t.Fatal("the fingerprint is not stable")
	}
	b.Tables[0].All = append(b.Tables[0].All, "z integer")
	if a.fingerprint() == b.fingerprint() {
		t.Error("a column added does not change the fingerprint")
	}
	c := fixtureFacts()
	c.Tables[4].DistColumn = "v"
	if a.fingerprint() == c.fingerprint() {
		t.Error("another distribution column does not change the fingerprint")
	}
	d := fixtureFacts()
	d.Colocs[7].Buckets = 16
	if a.fingerprint() == d.fingerprint() {
		t.Error("another bucket count does not change the fingerprint")
	}
	e := fixtureFacts()
	e.Tables[3].AM = "heap"
	if a.fingerprint() == e.fingerprint() {
		t.Error("another access method does not change the fingerprint")
	}
}

// The writer, the catalog and the manifest of the exporter are accepted by the validator of the importer: the chain of the export without a database.
func TestExporterOutputIsAcceptedByTheValidator(t *testing.T) {
	f := fixtureFacts()
	f.Colocs[7].Groups = 1
	cat := f.catalog()
	cj, _ := json.Marshal(cat)
	var buf bytes.Buffer
	w := archive.NewWriter(&buf)
	for _, e := range []struct{ name, data string }{{archive.CatalogName, string(cj)}, {archive.PreDataName, "CREATE TABLE app.items (\n    id bigint NOT NULL,\n    label text\n);\n"}, {archive.ViewsName, ""}, {archive.PostDataName, ""}} {
		ew, err := w.Create(e.name)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = ew.Write([]byte(e.data))
	}
	entries := f.dataEntries(nil, "all")
	meta := map[string]archive.ManifestEntry{}
	for i, e := range entries {
		name := archive.DataName(i + 1)
		ew, err := w.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = ew.Write([]byte(strings.Join(e.Cols, ",") + "\n"))
		tn := e.Table.qualified()
		if e.History {
			tn = e.Base
		}
		meta[name] = archive.ManifestEntry{Table: tn, Columns: e.Cols, Rows: 0, Consistency: "archive-single-snapshot", Groups: 1, LedgerHistory: e.History}
	}
	m := f.manifest("shop", "0.2.0-dev", f.fingerprint(), time.Date(2026, 11, 23, 9, 0, 0, 0, time.UTC), "all", nil, f.warnings(entries, true, false, nil))
	if _, err := w.Finish(m, meta); err != nil {
		t.Fatalf("the writer refuses the manifest of the exporter: %v", err)
	}
	res, err := archive.Validate(bytes.NewReader(buf.Bytes()), int64(buf.Len()), archive.Limits{MaxImportBytes: 1 << 20})
	if err != nil {
		t.Fatalf("the validator refuses the archive of the exporter: %v", err)
	}
	if res.Manifest.Source.Database != "shop" || res.Manifest.Totals.Tables != 9 {
		t.Errorf("manifest: %+v", res.Manifest.Totals)
	}
	if reasons := archive.CheckCompatibility(res.Manifest, archive.Target{PostgresMajor: 19, Extensions: map[string]string{"kingres_shard": "0.9", "citext": "1.6"}}); len(reasons) != 0 {
		t.Errorf("an archive of 18 does not fit 19: %v", reasons)
	}
	if reasons := archive.CheckCompatibility(res.Manifest, archive.Target{PostgresMajor: 17}); len(reasons) == 0 {
		t.Error("an archive of 18 fits 17")
	}
}

func TestWarningsSayWhatTheArchiveDoesNotCarry(t *testing.T) {
	f := fixtureFacts()
	f.LargeObjects, f.OIDColumns = 2, []string{"public.t.c"}
	ws := strings.Join(f.warnings(f.dataEntries(nil, "all"), false, true, map[string]bool{"public.acc": true}), "\n")
	for _, want := range []string{"owners, privileges", "2 large objects", "table public.d_hash was read per group (3 groups)", "ledger tables are carried as data", "time-partition policies", "left out"} {
		if !strings.Contains(ws, want) {
			t.Errorf("the warnings lack %q:\n%s", want, ws)
		}
	}
	if strings.Contains(strings.Join(fixtureFacts().warnings(nil, true, false, nil), "\n"), "large objects") {
		t.Error("a warning about large objects without any")
	}
}

func TestCheckExportableRefusesWhatTheArchiveDoesNotCarry(t *testing.T) {
	o := &Ops{}
	f := fixtureFacts()
	f.Unsupported = []string{"function: public.f", "policy: t.p", "function: public.g"}
	err := o.checkExportable(context.Background(), "db", "r", f, false)
	var p *problem.Problem
	if !errors.As(err, &p) || p.Code != problem.SchemaUnrecognised || !strings.Contains(p.Detail, "function, policy") || strings.Contains(p.Detail, "public.f") {
		t.Errorf("unsupported objects: %v", err)
	}
	f.Unsupported = nil
	f.LargeObjects = 1
	err = o.checkExportable(context.Background(), "db", "r", f, false)
	if !errors.As(err, &p) || p.Code != problem.LargeObjectsPresent {
		t.Errorf("large objects: %v", err)
	}
	f.LargeObjects, f.OIDColumns = 0, []string{"public.t.c"}
	if err = o.checkExportable(context.Background(), "db", "r", f, false); !errors.As(err, &p) || p.Code != problem.LargeObjectsPresent {
		t.Errorf("an oid column: %v", err)
	}
}

func TestSplitPartKeyAndEstimates(t *testing.T) {
	for in, want := range map[string][2]string{"RANGE (ts)": {"range", "ts"}, "LIST (a, b)": {"list", "a, b"}, "": {"", ""}, "RANGE": {"", ""}} {
		if m, k := splitPartKey(in); m != want[0] || k != want[1] {
			t.Errorf("splitPartKey(%q) = %q %q", in, m, k)
		}
	}
	f := fixtureFacts()
	if e := dbEstimate(f, 1<<40); e != 64<<20 {
		t.Errorf("the smallest estimate is 64 MiB: %d", e)
	}
	f.Tables[3].Bytes = 100 << 20
	if e := dbEstimate(f, 1<<40); e < 1000<<20 || e > 1001<<20 {
		t.Errorf("a columnar table counts ten times: %d", e)
	}
	if e := dbEstimate(f, 200<<20); e != 200<<20 {
		t.Errorf("the estimate is capped by the largest file: %d", e)
	}
	if likeEscape(`a_b%c\d`) != `a\_b\%c\\d` {
		t.Errorf("likeEscape: %q", likeEscape(`a_b%c\d`))
	}
	for _, k := range []string{"table.distribute", "table.import", "table.setStorage"} {
		if !tableTaskKind(k) {
			t.Errorf("%s is a table task", k)
		}
	}
	for _, k := range []string{"table.export", "database.export", "rebalance"} {
		if tableTaskKind(k) {
			t.Errorf("%s is not a table task that waits for an export", k)
		}
	}
}

// pg_dump is run with fixed arguments, no value of a client in them, and an environment of its own.
func TestDumpArgumentsAndEnvironment(t *testing.T) {
	args := strings.Join(dumpArgs("pre-data", "00000003-1"), " ")
	for _, want := range []string{"--schema-only", "--no-owner", "--no-privileges", "--no-publications", "--section=pre-data", "--snapshot=00000003-1", "--exclude-schema=kingres", "--exclude-schema=kingres_transfer"} {
		if !strings.Contains(args, want) {
			t.Errorf("pg_dump arguments lack %s: %s", want, args)
		}
	}
	for _, bad := range []string{"--data-only", "--file", "-f ", "--table", "--format"} {
		if strings.Contains(args, bad) {
			t.Errorf("pg_dump argument %s", bad)
		}
	}
	cfg := &pgx.ConnConfig{Config: pgconn.Config{Host: "127.0.0.1", Port: 5432, User: "kingresd", Password: "secret", Database: "shop"}}
	env := strings.Join(dumpEnv(cfg, "/x/pgpass"), " ")
	if !strings.Contains(env, "PGPASSFILE=/x/pgpass") || !strings.Contains(env, "PGOPTIONS=-c search_path=pg_catalog") || strings.Contains(env, "PGPASSWORD") || strings.Contains(env, "secret") || strings.Contains(env, "PGSERVICE") || strings.Contains(args, "secret") {
		t.Errorf("environment %q", env)
	}
	o := &Ops{}
	_, err := o.runDump(context.Background(), cfg, "pre-data", "x", 0)
	if te := transfer.SafeError(err); te.Code != transfer.CodeDumpFailed {
		t.Errorf("no directory of the programs: %v", err)
	}
	o.Transfer = &TransferConfig{PGBinDir: t.TempDir()}
	if _, err = o.runDump(context.Background(), cfg, "pre-data", "x", 0); transfer.SafeError(err).Code != transfer.CodeDumpFailed {
		t.Errorf("no pg_dump in the directory: %v", err)
	}
	cb := &capBuffer{max: 4}
	if _, err := cb.Write([]byte("12345")); err == nil || !cb.over {
		t.Error("the cap of the dump is not applied")
	}
}

func TestUploadStateOfAnArchive(t *testing.T) {
	rec := transfer.Record{State: transfer.StateReady, Purpose: "databaseArchive"}
	for in, want := range map[string]string{"": "complete", "validating": "validating", "valid": "valid", "invalid": "invalid", "x": "complete"} {
		rec.ArchiveState = in
		if got := uploadState(rec); got != want {
			t.Errorf("state %q: %s, want %s", in, got, want)
		}
	}
	if uploadFormat("databaseArchive") != "archive/1" || uploadFormat("tableImport") != "csv" {
		t.Error("upload format")
	}
	rec.Report, _ = json.Marshal(uploadReport{Archive: nil})
	if cachedReport(rec) != nil {
		t.Error("a cached report from nothing")
	}
}

// The cache of a validation is by the SHA-256 of the file: a report made for other bytes is not used.
func TestCachedReportIsByTheChecksum(t *testing.T) {
	good := strings.Repeat("a", 64)
	rep := &api.ArchiveReport{Valid: true, SHA256: good}
	b, _ := json.Marshal(uploadReport{Archive: rep, Version: ValidatorVersion})
	rec := transfer.Record{SHA256: good, Report: b}
	if cachedReport(rec) == nil {
		t.Error("the report of these bytes is not used")
	}
	stale, _ := json.Marshal(uploadReport{Archive: rep, Version: "an older validator"})
	if cachedReport(transfer.Record{SHA256: good, Report: stale}) != nil {
		t.Error("the report of another version of the validator is used")
	}
	rec.SHA256 = strings.Repeat("b", 64)
	if cachedReport(rec) != nil {
		t.Error("the report of other bytes is used")
	}
	rec.SHA256 = ""
	if cachedReport(rec) != nil {
		t.Error("a report is used for a file without a checksum")
	}
}

// The password of pg_dump is in a file of mode 0600 with the escapes of a password file, and nowhere in the arguments or the environment.
func TestPasswordFileOfPgDump(t *testing.T) {
	cfg := &pgx.ConnConfig{Config: pgconn.Config{Host: "127.0.0.1", Port: 5432, User: "kingresd", Password: "se:cr\\et", Database: "shop"}}
	dir := t.TempDir()
	name, err := writePassfile(dir, cfg)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(name)
	if string(b) != `127.0.0.1:5432:shop:kingresd:se\:cr\\et`+"\n" {
		t.Errorf("the line of the password file: %q", b)
	}
	if runtime.GOOS != "windows" {
		if st, _ := os.Stat(name); st.Mode().Perm() != 0o600 {
			t.Errorf("mode %v", st.Mode())
		}
	}
}

// A read that did not finish is not a verdict about the archive: the validation fails the task and nothing is cached or made into a report (B3).
func TestAFailedReadIsNotAReport(t *testing.T) {
	o := &Ops{}
	x := &xferState{keys: staging.NewKeysForTest(1)}
	rec := transfer.Record{ID: 7, KeyID: "no such key", WrappedKey: []byte("x")}
	v, err := o.validationOf(context.Background(), x, rec)
	if err == nil || v != nil {
		t.Fatalf("a failed read gave a validation: %v %v", v, err)
	}
	if te := transfer.SafeError(err); te.Code != transfer.CodeKeyLost {
		t.Errorf("code %q", te.Code)
	}
	if o.validation(rec.ID) != nil {
		t.Error("a failed read was kept for the next step")
	}
	// A verdict about the archive is kept.
	o.setValidation(8, &validation{err: &archive.Error{Code: archive.CodeInvalid}})
	if v, err := o.validationOf(context.Background(), x, transfer.Record{ID: 8}); err != nil || v == nil {
		t.Errorf("a verdict was refused: %v", err)
	}
}

// The limits of a validation: the smaller of the limit of the import and a multiple of the staged file, and a ratio for one entry (B5).
func TestArchiveLimitsFollowTheFile(t *testing.T) {
	if l := archiveLimits(1 << 20); l.MaxImportBytes != (1<<20)*MaxExpansion || l.MaxRatio != MaxEntryRatio {
		t.Errorf("limits of a small file: %+v", l)
	}
	if l := archiveLimits(1 << 40); l.MaxImportBytes != 100<<30 {
		t.Errorf("limits of a large file: %+v", l)
	}
}
