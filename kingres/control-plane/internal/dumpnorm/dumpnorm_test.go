// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package dumpnorm

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/sqlshape"
)

// The golden files are what pg_dump of the build of this tree writes for the database of the export fixture (spike S3 of the design): a plain table, a
// hash-distributed table with its bucket partitions, a user-partitioned distributed table with leaves under each bucket partition, a reference table, a
// columnar table, an updatable and an append-only ledger table with their triggers and history, a local partitioned table, an inheritance pair, a time
// partitioned table, an identity column and views, per major.

// goldenFacts is what the catalog says of the golden database.
func goldenFacts(buckets int) Facts {
	f := Facts{Remove: map[string]bool{}, RemoveIndex: map[string]bool{}, Hash: map[string]HashTable{}, Ledger: map[string]bool{}}
	f.Hash["public.d_hash"] = HashTable{Column: "id"}
	f.Hash["public.d_part"] = HashTable{Column: "id", User: &UserPartitioning{Method: "RANGE", Key: "ts", Partitions: []Leaf{
		{Name: "d_part_2026", Bound: "FROM ('2026-01-01 00:00:00+03:30') TO ('2027-01-01 00:00:00+03:30')"}, {Name: "d_part_rest", Bound: "DEFAULT"}}}}
	for b := 0; b < buckets; b++ {
		for _, r := range []string{"d_hash", "d_part", "d_part_2026", "d_part_rest"} {
			rel := fmt.Sprintf("public.%s_b%04d", r, b)
			f.Remove[rel] = true
			f.RemoveIndex[rel+"_pkey"] = true
			f.RemoveIndex[rel+"_tenant_idx"] = true
		}
	}
	f.Remove["public.acc_ledger_history"] = true
	for _, t := range []string{"public.acc", "public.acc_ledger_history", "public.audit_rows"} {
		f.Ledger[t] = true
	}
	return f
}

func golden(t *testing.T, major, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", major, name))
	if os.IsNotExist(err) {
		t.Skipf("the golden files are not beside the test binary (%s)", filepath.Join("testdata", major, name))
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func normaliseGolden(t *testing.T, major string) Output {
	t.Helper()
	out, err := Normalise(golden(t, major, "pre-data.sql"), golden(t, major, "post-data.sql"), goldenFacts(8))
	if err != nil {
		t.Fatalf("%s: %v", major, err)
	}
	return out
}

func TestGoldenNormalisation(t *testing.T) {
	for _, major := range []string{"pg18", "pg19"} {
		t.Run(major, func(t *testing.T) {
			out := normaliseGolden(t, major)
			all := "\n" + out.Pre + out.Views + out.Post
			for _, bad := range []string{"_b0", "PARTITION BY HASH", "CREATE EXTENSION", "COMMENT ON EXTENSION", "\nSET ", "set_config", `\restrict`, `\unrestrict`, "kingres_ledger", "acc_ledger_history", "ON ONLY public.d_hash", "ONLY public.d_hash", "ONLY public.d_part", "modulus"} {
				if strings.Contains(all, bad) {
					t.Errorf("the output still holds %q", bad)
				}
			}
			for _, want := range []string{
				"CREATE TABLE public.c_events (\n    id bigint,\n    v text\n) USING columnar;",
				"PARTITION BY RANGE (ts);",
				"CREATE TABLE public.d_part_2026 PARTITION OF public.d_part FOR VALUES FROM ('2026-01-01 00:00:00+03:30') TO ('2027-01-01 00:00:00+03:30');",
				"CREATE TABLE public.d_part_rest PARTITION OF public.d_part DEFAULT;",
				"ALTER TABLE public.d_hash\n    ADD CONSTRAINT d_hash_pkey PRIMARY KEY (id);",
				"CREATE INDEX d_hash_tenant ON public.d_hash USING btree (tenant);",
				"ALTER TABLE ONLY public.lp ATTACH PARTITION public.lp_2026 FOR VALUES FROM ('2026-01-01') TO ('2027-01-01');",
				"INHERITS (public.inh_parent);",
				"CREATE TABLE public.d_hash (\n    id bigint NOT NULL,\n    tenant integer NOT NULL,\n    v text\n);",
			} {
				if !strings.Contains(all, want) {
					t.Errorf("the output lacks %q", want)
				}
			}
			if !strings.Contains(out.Views, "CREATE VIEW public.v_plain") || !strings.Contains(out.Views, "CREATE MATERIALIZED VIEW public.mv_plain") || strings.Contains(out.Pre, "VIEW") {
				t.Errorf("views are not in the views file:\npre has VIEW: %v\nviews:\n%s", strings.Contains(out.Pre, "VIEW"), out.Views)
			}
			// The ledger tables keep their data tables and lose the history table and the triggers; the foreign key and the identity are kept.
			for _, want := range []string{"CREATE TABLE public.acc (", "CREATE TABLE public.audit_rows (", "t_child_plain_id_fkey", "GENERATED ALWAYS AS IDENTITY"} {
				if !strings.Contains(all, want) {
					t.Errorf("the output lacks %q", want)
				}
			}
			// The exporter runs its output through the shape grammar: nothing may be refused.
			for name, text := range map[string]string{"pre-data.sql": out.Pre, "views.sql": out.Views, "post-data.sql": out.Post} {
				plan, err := sqlshape.ParseFile(strings.NewReader(text), "schema/"+name, sqlshape.Hints{InheritanceChildren: map[string]bool{"public.inh_child": true}})
				if err != nil {
					t.Fatalf("%s: %v", name, err)
				}
				for _, r := range plan.Refused {
					t.Errorf("%s: the grammar refuses %s at %s: %s", name, r.Kind, r.Position, r.Reason)
				}
			}
		})
	}
}

// The two majors write the same logical schema.
func TestMajorsAgree(t *testing.T) {
	a, b := normaliseGolden(t, "pg18"), normaliseGolden(t, "pg19")
	if a != b {
		t.Errorf("the normalised schema of PostgreSQL 18 and 19 differ")
	}
}

var bucketName = regexp.MustCompile(`_b[0-9]{4}`)

func TestRefusals(t *testing.T) {
	f := goldenFacts(2)
	cases := []struct {
		name, pre, kind string
	}{
		{"another access method", "SET default_table_access_method = zheap;\nCREATE TABLE public.t (a int);\n", "access method"},
		{"an unknown SET", "SET work_mem = '1GB';\n", "SET"},
		{"a hash table that is not partitioned", "CREATE TABLE public.d_hash (id bigint);\n", "distributed table"},
		{"a hash table partitioned on another column", "CREATE TABLE public.d_hash (id bigint, tenant int)\nPARTITION BY HASH (tenant);\n", "distributed table"},
		{"a hash table partitioned on two columns", "CREATE TABLE public.d_hash (id bigint, tenant int)\nPARTITION BY HASH (id, tenant);\n", "distributed table"},
	}
	for _, tc := range cases {
		_, err := Normalise(tc.pre, "", f)
		r, ok := IsRefusal(err)
		if !ok || r.Kind != tc.kind {
			t.Errorf("%s: %v", tc.name, err)
		}
	}
}

// Statements that are not in the closed set of removals and rewrites are kept as they are, so that the shape grammar can refuse them by name.
func TestOtherStatementsAreKept(t *testing.T) {
	pre := "CREATE FUNCTION public.f() RETURNS int LANGUAGE sql AS $$ select 1; $$;\nCREATE POLICY p ON public.t USING (true);\nGRANT ALL ON public.t TO x;\n"
	out, err := Normalise(pre, "CREATE TRIGGER mine BEFORE INSERT ON public.t FOR EACH ROW EXECUTE FUNCTION public.f();\nCREATE TRIGGER kingres_ledger_capture AFTER INSERT ON public.not_a_ledger_table FOR EACH ROW EXECUTE FUNCTION kingres.ledger_capture('4');\n", goldenFacts(1))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"CREATE FUNCTION public.f()", "select 1; $$", "CREATE POLICY", "GRANT ALL", "CREATE TRIGGER mine", "CREATE TRIGGER kingres_ledger_capture AFTER INSERT ON public.not_a_ledger_table"} {
		if !strings.Contains(out.Pre+out.Post, want) {
			t.Errorf("a statement was dropped: %q\n%s", want, out.Pre+out.Post)
		}
	}
}

func TestSplitter(t *testing.T) {
	src := "-- c\n\\restrict abc\nSET a = 'x;y';\n/* ; /* nested */ ; */\nSELECT $t$ ; $t$, E'a\\'b;', \"q;\"\"x\";\n\\unrestrict abc\n"
	got, err := split(src)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 || got[0].meta == "" || got[3].meta == "" || !strings.HasPrefix(got[1].text, "SET a") || !strings.Contains(got[2].text, `"q;""x"`) {
		t.Errorf("%d statements: %+v", len(got), got)
	}
	if _, err := split("SELECT 'unterminated;"); err == nil {
		t.Error("an unterminated string is an error")
	}
	if _, err := split("SELECT 1"); err == nil {
		t.Error("a missing terminator is an error")
	}
	_ = bucketName
}

// The splitter finds the statements that PostgreSQL finds in the corpus of the lexer (carriage returns, tags of high bytes, nested comments).
func TestSplitterAgreesWithTheCorpus(t *testing.T) {
	for i, c := range sqlshape.LexCorpus() {
		n, err := CountStatements(c.SQL)
		if err != nil || n != c.N {
			t.Errorf("case %d %q: %d statements (%v), PostgreSQL sees %d", i, c.SQL, n, err, c.N)
		}
	}
}
