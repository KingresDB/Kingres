// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package ops

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/archive"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/task"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/transfer"
)

// A staged file that a test can read: the archive bytes, with the methods the importer uses.
type memFile struct{ *bytes.Reader }

func (m memFile) Size() int64  { return int64(m.Reader.Len()) + 0 }
func (m memFile) Close() error { return nil }

func newMem(b []byte) memFile { return memFile{bytes.NewReader(b)} }

// opaqueW hides the Seek methods of a buffer: the writer must work on a destination that is not seekable.
type opaqueW struct{ w io.Writer }

func (o opaqueW) Write(p []byte) (int, error) { return o.w.Write(p) }

// testArchive builds a small archive with the writer: one local table with two rows and one hash table with a ledger-free catalog entry.
func testArchive(t testing.TB, pre, views, post string, mod func(m *archive.Manifest, c *archive.Catalog)) []byte {
	t.Helper()
	cat := archive.Catalog{CatalogVersion: 1, ColocationGroups: []archive.ColocationGroup{{Ref: "c1", BucketCount: 8, DistColumnType: "bigint"}},
		Tables: []archive.CatalogTable{
			{Schema: "public", Name: "t", Kind: "local", TreeRole: "standalone", Columns: []string{"id", "v"}, AccessMethod: "heap"},
			{Schema: "public", Name: "d", Kind: "hash", TreeRole: "standalone", Columns: []string{"id"}, DistributionColumn: "id", ColocationRef: "c1", AccessMethod: "heap"},
		}, TimepartPolicies: []archive.TimepartPolicy{}, Sequences: []archive.CatalogSequence{}}
	man := archive.Manifest{CreatedAt: "2026-11-23T09:30:00Z", Source: archive.Source{Database: "shop", KingresVersion: "0.1.0", ContractVersion: "0.41.0", PostgresMajor: 18, GroupCount: 1,
		Encoding: "UTF8", LcCollate: "C", LcCtype: "C", LocaleProvider: "libc"},
		Extensions: []archive.ManifestExtension{{Name: "kingres_shard", Version: "0.9", Schema: "pg_catalog"}}, Fingerprint: strings.Repeat("a", 64), Options: archive.Options{DataMode: "all"}}
	if mod != nil {
		mod(&man, &cat)
	}
	cj, _ := json.Marshal(cat)
	var buf bytes.Buffer
	w := archive.NewWriter(opaqueW{&buf})
	meta := map[string]archive.ManifestEntry{}
	for _, p := range []struct {
		name string
		data []byte
		meta archive.ManifestEntry
	}{
		{archive.CatalogName, cj, archive.ManifestEntry{}},
		{archive.PreDataName, []byte(pre), archive.ManifestEntry{}},
		{archive.ViewsName, []byte(views), archive.ManifestEntry{}},
		{archive.PostDataName, []byte(post), archive.ManifestEntry{}},
		{archive.DataName(1), []byte("id,v\n\"1\",\"a\"\n\"2\",\\N\n"), archive.ManifestEntry{Table: "public.t", Columns: []string{"id", "v"}, Rows: 2, Consistency: "single-snapshot", Groups: 1}},
		{archive.DataName(2), []byte("id\n\"1\"\n"), archive.ManifestEntry{Table: "public.d", Columns: []string{"id"}, Rows: 1, Consistency: "single-snapshot", Groups: 1}},
	} {
		ew, err := w.Create(p.name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := ew.Write(p.data); err != nil {
			t.Fatal(err)
		}
		if p.meta.Table != "" {
			meta[p.name] = p.meta
		}
	}
	if _, err := w.Finish(man, meta); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

const goodPre = "CREATE TABLE public.t (id bigint NOT NULL, v text);\nCREATE TABLE public.d (id bigint NOT NULL);\n"

func readArchive(t testing.TB, b []byte, p dbImportParams) (*importState, error) {
	t.Helper()
	o := &Ops{}
	a := &archiveFile{r: newMem(b), rec: transfer.Record{}}
	return o.readImportArchive(context.Background(), a, p)
}

func taskCodeOf(err error) string {
	var c task.Coded
	if errors.As(err, &c) {
		return c.TaskCode()
	}
	return ""
}

// The revalidation of an archive from its bytes: a good one gives a state; the checksum, the grammar and the structure each refuse.
func TestReadImportArchiveRevalidates(t *testing.T) {
	good := testArchive(t, goodPre, "", "ALTER TABLE ONLY public.t ADD CONSTRAINT t_pkey PRIMARY KEY (id);\n", nil)
	st, err := readArchive(t, good, dbImportParams{})
	if err != nil {
		t.Fatal(err)
	}
	if len(st.plan.Tables) != 2 || !st.plan.Tables["public.t"] || st.plan.Statements != 3 {
		t.Errorf("plan: %+v", st.plan)
	}
	// The checksum that the operator expects binds the import to the bytes.
	if _, err := readArchive(t, good, dbImportParams{ExpectedSHA256: strings.Repeat("0", 64)}); taskCodeOf(err) != "archive_invalid" {
		t.Errorf("a checksum that is not the file's: %v", err)
	}
	sum, _ := sumOfFile(bytes.NewReader(good))
	if _, err := readArchive(t, good, dbImportParams{ExpectedSHA256: sum}); err != nil {
		t.Errorf("the checksum of the file: %v", err)
	}
	// The grammar.
	for name, post := range map[string]string{
		"set role":   "SET ROLE postgres;\n",
		"grant":      "GRANT ALL ON ALL TABLES IN SCHEMA pg_catalog TO PUBLIC;\n",
		"alter db":   "ALTER DATABASE x SET search_path = y;\n",
		"drop table": "DROP TABLE public.t;\n",
		"copy":       "COPY public.t TO PROGRAM 'id';\n",
	} {
		_, err := readArchive(t, testArchive(t, goodPre, "", post, nil), dbImportParams{})
		if taskCodeOf(err) != "archive_statement_refused" {
			t.Errorf("%s: %v", name, err)
		}
		if err != nil && (strings.Contains(err.Error(), "postgres") || strings.Contains(err.Error(), "PROGRAM")) {
			t.Errorf("%s: the error holds text of the archive: %v", name, err)
		}
	}
	// The structure: a file that is not an archive, a truncated archive, a changed byte.
	for name, b := range map[string][]byte{"garbage": []byte("this is not a zip file"), "truncated": good[:len(good)-30], "empty": nil} {
		if _, err := readArchive(t, b, dbImportParams{}); taskCodeOf(err) != "archive_invalid" {
			t.Errorf("%s: %v", name, err)
		}
	}
	flipped := append([]byte(nil), good...)
	flipped[40] ^= 0xff
	if _, err := readArchive(t, flipped, dbImportParams{}); err == nil {
		t.Error("a changed byte is not found")
	}
}

func TestPlanCollectsFunctionsTablesAndViews(t *testing.T) {
	pre := goodPre + "CREATE TABLE public.f (a int DEFAULT abs(-1), b text GENERATED ALWAYS AS (lower(public.cx(a::text))) STORED);\n"
	views := "CREATE VIEW public.v AS SELECT now() AS n, pg_sleep(1);\nCREATE MATERIALIZED VIEW public.mv AS SELECT 1 AS x WITH NO DATA;\n"
	b := testArchive(t, pre, views, "", func(m *archive.Manifest, c *archive.Catalog) {
		c.Tables = append(c.Tables, archive.CatalogTable{Schema: "public", Name: "f", Kind: "local", TreeRole: "standalone", Columns: []string{"a"}, AccessMethod: "heap"})
		m.Options.ExcludedTables = []string{"public.f"}
	})
	st, err := readArchive(t, b, dbImportParams{})
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(st.plan.Functions, ",")
	for _, want := range []string{"abs", "lower", "public.cx", "now", "pg_sleep"} {
		if !strings.Contains(","+got+",", ","+want+",") {
			t.Errorf("functions %s lack %s", got, want)
		}
	}
	if len(st.plan.MatViews) != 1 || st.plan.MatViews[0] != "public.mv" || !st.plan.Tables["public.f"] {
		t.Errorf("plan: %+v", st.plan)
	}
}

// The verdict about a function name, from the candidates that pg_proc has.
func TestFunctionVerdict(t *testing.T) {
	ext := map[string]string{"citext": "public", "kingres_ledger": "kingres", "hstore": "ext", "pgcrypto": "public"}
	builtin := []procCand{{Schema: "pg_catalog"}}
	cases := []struct {
		name   string
		schema string
		fn     string
		cands  []procCand
		ok     bool
	}{
		{"a builtin", "", "lower", builtin, true},
		{"a builtin by its schema", "pg_catalog", "now", builtin, true},
		{"nextval", "", "nextval", builtin, true},
		{"pg_sleep", "", "pg_sleep", builtin, false},
		{"pg_sleep_for", "", "pg_sleep_for", builtin, false},
		{"pg_read_file", "", "pg_read_file", builtin, false},
		{"pg_read_binary_file", "", "pg_read_binary_file", builtin, false},
		{"pg_ls_dir", "", "pg_ls_dir", builtin, false},
		{"lo_import", "", "lo_import", builtin, false},
		{"set_config", "", "set_config", builtin, false},
		{"setval", "", "setval", builtin, false},
		{"pg_terminate_backend", "", "pg_terminate_backend", builtin, false},
		{"pg_advisory_lock", "", "pg_advisory_lock", builtin, false},
		{"query_to_xml", "", "query_to_xml", builtin, false},
		{"upper case of a denied name", "", "PG_SLEEP", builtin, false},
		{"it does not exist", "", "nope", nil, false},
		{"it is in another schema than the search path of the role", "", "mine", []procCand{{Schema: "public"}}, false},
		{"a user function by its schema", "public", "mine", []procCand{{Schema: "public"}}, false},
		{"a function of an extension of pg_catalog", "", "kingres_thing", []procCand{{Schema: "pg_catalog", Ext: "kingres_shard"}}, false},
		{"a function of citext", "public", "citext_ne", []procCand{{Schema: "public", Ext: "citext"}}, true},
		{"a function of an extension in another schema", "ext", "hstore", []procCand{{Schema: "ext", Ext: "hstore"}}, true},
		{"a function of citext named in the wrong schema", "other", "citext_ne", []procCand{{Schema: "public", Ext: "citext"}}, false},
		{"a function of the ledger", "kingres", "ledger_enable", []procCand{{Schema: "kingres", Ext: "kingres_ledger"}}, false},
		{"an extension function that the manifest does not list", "public", "fuzz", []procCand{{Schema: "public", Ext: "fuzzystrmatch"}}, false},
		{"a security definer function", "", "secdef", []procCand{{Schema: "pg_catalog", SecDef: true}}, false},
		{"one of two overloads is denied", "", "lo_x", []procCand{{Schema: "pg_catalog"}, {Schema: "pg_catalog", Ext: "x"}}, false},
	}
	for _, tc := range cases {
		if why := functionVerdict(tc.schema, tc.fn, tc.cands, ext); (why == "") != tc.ok {
			t.Errorf("%s: verdict %q, want ok=%v", tc.name, why, tc.ok)
		}
	}
}

func TestSafeNameBoundsAndCleans(t *testing.T) {
	if got := safeName("pg_sleep\n\x00;DROP"); got != "pg_sleep???DROP" {
		t.Errorf("%q", got)
	}
	long := strings.Repeat("a", 100)
	if got := safeName(long); len(got) != 51 || !strings.HasSuffix(got, "...") {
		t.Errorf("%q", got)
	}
	if got := safeSentence("a b\x1b[31m é"); strings.ContainsAny(got, "\x1b") || strings.Contains(got, "é") {
		t.Errorf("%q", got)
	}
}

func TestPrivateNameIsTheTasksOwn(t *testing.T) {
	if privateName(41, "ab12") != "kingres_import_41_ab12" || !dbNameRE.MatchString(privateName(1<<40, "0123456789ab")) {
		t.Errorf("%q", privateName(41, "ab12"))
	}
	// A name that a request may give can never be a private name, so a task cannot be made to work in, or drop, another task's database.
	for _, n := range []string{"kingres_import_7", "kingres_import_", "postgres", "template0", "template1", "kingres_system", "Upper", "a b", "", "x;y", strings.Repeat("a", 64), "1abc"} {
		if usableImportName(n) {
			t.Errorf("%q is usable as a final name", n)
		}
	}
	for _, n := range []string{"shop", "_x", "a1_b2", strings.Repeat("a", 63)} {
		if !usableImportName(n) {
			t.Errorf("%q is not usable as a final name", n)
		}
	}
}

func TestImportCreateOpFromTheManifest(t *testing.T) {
	m := archive.Manifest{Source: archive.Source{Encoding: "UTF8", LcCollate: "C", LcCtype: "C"}, Extensions: []archive.ManifestExtension{
		{Name: "kingres_shard", Version: "0.9", Schema: "pg_catalog"}, {Name: "kingres_ledger", Version: "0.3", Schema: "kingres"},
		{Name: "citext", Version: "1.6", Schema: "public"}, {Name: "hstore", Version: "1.8", Schema: "ext"}, {Name: "plpgsql", Version: "1.0", Schema: "pg_catalog"}}}
	op := importCreateOpOf("kingres_import_5", "alice", m)
	if op.Name != "kingres_import_5" || op.Role != "alice" || op.Encoding != "UTF8" {
		t.Errorf("%+v", op)
	}
	var names []string
	for _, e := range op.Extensions {
		names = append(names, e.Name)
	}
	if strings.Join(names, ",") != "kingres_ledger,citext,hstore" {
		t.Errorf("extensions %v (the sharding extension is installed by its own step, plpgsql is in the template)", names)
	}
	if len(op.Schemas) != 1 || op.Schemas[0] != "ext" {
		t.Errorf("schemas to make: %v", op.Schemas)
	}
	skip := extSkipSchemas(m)
	if !skip["ext"] || !skip["kingres"] || skip["public"] || skip["pg_catalog"] {
		t.Errorf("schemas the statements may not create: %v", skip)
	}
}

// The extensions are the closed list: nothing else is ever put in a CREATE EXTENSION.
func TestSetupRefusesAnExtensionOutsideTheList(t *testing.T) {
	o := &Ops{}
	for _, e := range []string{"dblink", "file_fdw", "postgres_fdw", "plpython3u", "adminpack", "pg_read_server_files", "kingres_shard", "plpgsql", "x; drop database postgres"} {
		err := o.setupPrivateLocal(context.Background(), importCreateOp{Name: "kingres_import_1", Extensions: []importExtSpec{{Name: e, Schema: "public"}}})
		if err == nil || !strings.Contains(err.Error(), "cannot be installed") {
			t.Errorf("%q: %v", e, err)
		}
	}
	for _, name := range []string{"kingres_import_1;drop", "postgres", "kingres_import", "other_1"} {
		if err := o.setupPrivateLocal(context.Background(), importCreateOp{Name: name, Schemas: []string{"x"}}); err == nil {
			t.Errorf("%q is set up", name)
		}
		if err := o.createPrivateLocal(context.Background(), importCreateOp{Name: name, Encoding: "UTF8"}); err == nil {
			t.Errorf("%q is created", name)
		}
	}
	if err := o.createPrivateLocal(context.Background(), importCreateOp{Name: "kingres_import_1", Encoding: "UTF8; DROP", LcCollate: "C"}); err == nil {
		t.Error("an encoding with text in it is accepted")
	}
}

func TestRenameAndAccessRefuseForeignNames(t *testing.T) {
	o := &Ops{}
	ctx := context.Background()
	for _, op := range []importRenameOp{
		{From: "postgres", To: "shop", Owner: "a"}, {From: "kingres_import_1", To: "postgres", Owner: "a"}, {From: "kingres_import_1", To: "kingres_import_2", Owner: "a"},
		{From: "shop", To: "kingres_import_1", Owner: "a"}, {From: "kingres_import_1", To: "Shop", Owner: "a"},
		{From: "shop", To: "other", Owner: "a", Back: true}, {From: "postgres", To: "kingres_import_1", Back: true}, {From: "kingres_import_1", To: "kingres_import_2", Back: true},
	} {
		if err := o.importRenameLocal(ctx, op); err == nil || !strings.Contains(err.Error(), "not allowed") {
			t.Errorf("%+v: %v", op, err)
		}
	}
	for _, name := range []string{"postgres", "shop", "kingres_system"} {
		if err := o.importAccessLocal(ctx, importAccessOp{Name: name, Role: "alice", Allow: true}); err == nil || !strings.Contains(err.Error(), "cannot be changed") {
			t.Errorf("access to %q: %v", name, err)
		}
		if err := o.importSequencesLocal(ctx, importSeqOp{Name: name}); err == nil {
			t.Errorf("sequences of %q", name)
		}
	}
}

func TestDefinitionStepsAndTheCancelGuard(t *testing.T) {
	o := &Ops{}
	def := o.dbImportDefinition()
	in, _ := json.Marshal(dbImportParams{UploadID: 1, Database: "shop", Servers: []string{"s2", "s3"}})
	steps, err := def.Dynamic(in)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, s := range steps {
		names = append(names, s.Name)
	}
	want := "Check the archive|Create the private database|Create the private database on server s2|Create the private database on server s3|Install the extensions|Check the functions|" +
		"Create the tables|Distribute the tables|Create the views|Load the data|Create the indexes and constraints|Set the sequences|Enable the ledger and the policies|Verify|Take the final name"
	if strings.Join(names, "|") != want {
		t.Errorf("steps:\n%s\nwant\n%s", strings.Join(names, "|"), want)
	}
	// One server: no per-server step.
	one, _ := json.Marshal(dbImportParams{UploadID: 1, Database: "shop"})
	if s, _ := def.Dynamic(one); len(s) != 13 {
		t.Errorf("%d steps for a cluster of one server", len(s))
	}
	// A cancel is accepted until the sequences are set.
	mk := func(state string, step string) task.Task {
		var ts []task.StepState
		for _, n := range names {
			s := task.StepState{Name: n, State: task.StepPending}
			if n == step {
				s.State = state
			}
			ts = append(ts, s)
		}
		return task.Task{Steps: ts}
	}
	for _, tc := range []struct {
		step, state string
		ok          bool
	}{{stepMData, task.StepRunning, true}, {stepMPost, task.StepDone, true}, {stepMSeq, task.StepRunning, false}, {stepMSeq, task.StepDone, false}, {stepMLedger, task.StepRunning, false}, {stepMRename, task.StepRunning, false}} {
		if got := def.Cancellable(mk(tc.state, tc.step)); got != tc.ok {
			t.Errorf("cancel while %s is %s: %v", tc.step, tc.state, got)
		}
	}
}

func TestRefusalFromTheCachedReport(t *testing.T) {
	if refusalFromReport(nil) != nil || refusalFromReport(&api.ArchiveReport{Valid: true}) != nil {
		t.Error("a valid report refuses")
	}
	rep := &api.ArchiveReport{Valid: false, Statements: &api.ArchiveStatements{Refused: []api.RefusedStatement{{Kind: "SET", Position: "schema/pre-data.sql#3", Fingerprint: "9:abcd", Reason: "x"}}}}
	err := refusalFromReport(rep)
	var p *problem.Problem
	if !errors.As(err, &p) || p.Code != problem.ArchiveStatementRefused || p.Status != 422 || p.Reasons == nil {
		t.Fatalf("%v", err)
	}
	b, _ := json.Marshal(p)
	if strings.Contains(string(b), "SET ROLE") {
		t.Errorf("the problem holds text: %s", b)
	}
	inc := &api.ArchiveReport{Valid: false, Reasons: []api.TableBlocker{{Code: "postgres_major", Object: "postgres", Message: "m"}}}
	if err := refusalFromReport(inc); !errors.As(err, &p) || p.Code != problem.ArchiveIncompatible {
		t.Errorf("an incompatible archive: %v", err)
	}
	bad := &api.ArchiveReport{Valid: false, Reasons: []api.TableBlocker{{Code: "sha256", Object: "manifest.json", Message: "m"}}}
	if err := refusalFromReport(bad); !errors.As(err, &p) || p.Code != problem.ArchiveInvalid {
		t.Errorf("an invalid archive: %v", err)
	}
	many := &api.ArchiveReport{Valid: false}
	for i := 0; i < 100; i++ {
		many.Reasons = append(many.Reasons, api.TableBlocker{Code: "sha256", Object: "x"})
	}
	if err := refusalFromReport(many); !errors.As(err, &p) || len(p.Reasons.([]api.TableBlocker)) != maxImportReasons {
		t.Errorf("reasons are bounded: %v", err)
	}
}

func TestDataEntriesWithoutHistories(t *testing.T) {
	m := archive.Manifest{Entries: []archive.ManifestEntry{{Name: "catalog.json"}, {Name: "data/00001.csv", Table: "a.b"}, {Name: "data/00002.csv", Table: "a.c", LedgerHistory: true}, {Name: "data/00003.csv", Table: "a.d"}}}
	got := dataEntriesOf(m)
	if len(got) != 2 || got[0].Table != "a.b" || got[1].Table != "a.d" {
		t.Errorf("%+v", got)
	}
	if s, tb, ok := splitQualified("public.t"); !ok || s != "public" || tb != "t" {
		t.Error("split")
	}
	for _, bad := range []string{"t", ".t", "p.", "a.b.c", ""} {
		if _, _, ok := splitQualified(bad); ok {
			t.Errorf("%q splits", bad)
		}
	}
}

func TestExprFunctionNames(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"lower(name)", "lower"},
		{"(a + abs(b))", "abs"},
		{"CHECK ((length(note) < 50))", "length"},
		{"CREATE INDEX i ON public.t USING btree (lower(name))", "lower"},
		{"public.mine(1)", "public.mine"},
		{"\"public\".\"Mine\"(1)", "public.Mine"},
		{"pg_read_file('/etc/passwd'::text)", "pg_read_file"},
		{"'call(me)'::text", ""},
		{"'it''s lower(x)'::text", ""},
		{"COALESCE(a, 0)", ""},
		{"(x)::numeric(10,2)", ""},
		{"(x)::character varying(5)", ""},
		{"nextval('s'::regclass)", "nextval"},
		{"a IN (1, 2)", ""},
		{"FOREIGN KEY (plain_id) REFERENCES public.t_plain(id) ON DELETE CASCADE", ""},
		{"FOREIGN KEY (a) REFERENCES \"My Table\"(id) CHECK (lower(x) = x)", "lower"},
		{"ARRAY[1, 2]", ""},
		{"a.b.c.d(1)", "b.c.d"},
		{"pg_sleep(1), pg_sleep(2), lower(x)", "pg_sleep,lower"},
	}
	for _, tc := range cases {
		if got := strings.Join(exprFunctionNames(tc.in), ","); got != tc.want {
			t.Errorf("%q: %q, want %q", tc.in, got, tc.want)
		}
	}
}

// L5: the suffix that the control plane adds to a violated key is on a line of its own, so that a comment at the end of the text of the archive does not swallow
// it, and a text that is NOT VALID already is not given a second one.
func TestNotValidText(t *testing.T) {
	cases := map[string]string{
		"ALTER TABLE a.t ADD CONSTRAINT c FOREIGN KEY (x) REFERENCES a.p (id)":                       "ALTER TABLE a.t ADD CONSTRAINT c FOREIGN KEY (x) REFERENCES a.p (id)\n NOT VALID",
		"ALTER TABLE a.t ADD CONSTRAINT c FOREIGN KEY (x) REFERENCES a.p (id) -- trailing":           "ALTER TABLE a.t ADD CONSTRAINT c FOREIGN KEY (x) REFERENCES a.p (id) -- trailing\n NOT VALID",
		"ALTER TABLE a.t ADD CONSTRAINT c FOREIGN KEY (x) REFERENCES a.p (id) NOT VALID":             "ALTER TABLE a.t ADD CONSTRAINT c FOREIGN KEY (x) REFERENCES a.p (id) NOT VALID",
		"ALTER TABLE a.t ADD CONSTRAINT c FOREIGN KEY (x) REFERENCES a.p (id) not\tvalid -- c":       "ALTER TABLE a.t ADD CONSTRAINT c FOREIGN KEY (x) REFERENCES a.p (id) not\tvalid -- c",
		"ALTER TABLE a.t ADD CONSTRAINT c FOREIGN KEY (x) REFERENCES a.p (id) /* NOT VALID */":       "ALTER TABLE a.t ADD CONSTRAINT c FOREIGN KEY (x) REFERENCES a.p (id) /* NOT VALID */\n NOT VALID",
		"ALTER TABLE a.t ADD CONSTRAINT c FOREIGN KEY (x) REFERENCES a.p (id) -- NOT VALID\n-- more": "ALTER TABLE a.t ADD CONSTRAINT c FOREIGN KEY (x) REFERENCES a.p (id) -- NOT VALID\n-- more\n NOT VALID",
	}
	for in, want := range cases {
		if got := notValidText(in); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}
