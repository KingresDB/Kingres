// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package cluster

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/sqlx"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/wire"
)

func buildAll(t *testing.T, e string, p wire.ExtensionPlace, known, fixed bool) []string {
	t.Helper()
	bs, err := extensionStatements(e, p, known, fixed)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, b := range bs {
		st, err := b.Build()
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, st.SQL)
	}
	return out
}

// An extension is installed in the schema it has on the leader, with the schema made as the leader has it; the names are quoted identifiers.
func TestExtensionStatementsFollowTheLeadersSchema(t *testing.T) {
	got := buildAll(t, "pgcrypto", wire.ExtensionPlace{Schema: "My Ext", Owner: "app owner"}, true, false)
	want := []string{`CREATE SCHEMA IF NOT EXISTS "My Ext" AUTHORIZATION "app owner"`, `CREATE EXTENSION IF NOT EXISTS "pgcrypto" SCHEMA "My Ext"`}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("got %q, want %q", got, want)
	}
	// a hostile schema name is one identifier
	got = buildAll(t, "pgcrypto", wire.ExtensionPlace{Schema: `x"; DROP SCHEMA public; --`, Owner: "o"}, true, false)
	if len(got) != 2 || got[1] != `CREATE EXTENSION IF NOT EXISTS "pgcrypto" SCHEMA "x""; DROP SCHEMA public; --"` {
		t.Errorf("got %q", got)
	}
}

func TestExtensionStatementsWithoutAClause(t *testing.T) {
	for name, c := range map[string]struct {
		p            wire.ExtensionPlace
		known, fixed bool
	}{
		"fixed schema (kingres_*)": {wire.ExtensionPlace{Schema: "kingres", Owner: "o"}, true, true},
		"pg_catalog":               {wire.ExtensionPlace{Schema: "pg_catalog", Owner: "o"}, true, false},
		"sender says nothing":      {wire.ExtensionPlace{}, false, false},
	} {
		got := buildAll(t, "pgcrypto", c.p, c.known, c.fixed)
		if len(got) != 1 || got[0] != `CREATE EXTENSION IF NOT EXISTS "pgcrypto"` {
			t.Errorf("%s: %q", name, got)
		}
	}
	got := buildAll(t, "pg_stat_statements", wire.ExtensionPlace{Schema: "public"}, true, false)
	if len(got) != 1 || got[0] != `CREATE EXTENSION IF NOT EXISTS "pg_stat_statements" SCHEMA "public"` {
		t.Errorf("public without owner: %q", got)
	}
}

// A leader of an earlier version (the cluster upgrades the leader last) sends no places: pg_stat_statements is made in public, as everywhere else, and
// another extension is made without a clause, as before.
func TestExtensionStatementsForALeaderThatSendsNoPlaces(t *testing.T) {
	got := buildAll(t, "pg_stat_statements", wire.ExtensionPlace{}, false, false)
	if len(got) != 1 || got[0] != `CREATE EXTENSION IF NOT EXISTS "pg_stat_statements" SCHEMA "public"` {
		t.Errorf("pg_stat_statements: %q", got)
	}
	if got := buildAll(t, "pgcrypto", wire.ExtensionPlace{}, false, false); len(got) != 1 || got[0] != `CREATE EXTENSION IF NOT EXISTS "pgcrypto"` {
		t.Errorf("pgcrypto: %q", got)
	}
	// a fixed schema still wins
	if got := buildAll(t, "pg_stat_statements", wire.ExtensionPlace{}, false, true); len(got) != 1 {
		t.Errorf("fixed: %q", got)
	}
}

type fakeTarget struct {
	avail   map[string]bool
	fixed   map[string]bool
	schemas map[string]string // schema -> owner on this server
	ran     [][]string
	failOn  string // an extension whose statements fail
}

func (f *fakeTarget) Available(context.Context) (map[string]bool, error) { return f.avail, nil }
func (f *fakeTarget) Fixed(_ context.Context, e string) (bool, error)    { return f.fixed[e], nil }
func (f *fakeTarget) SchemaOwner(_ context.Context, s string) (string, bool, error) {
	o, ok := f.schemas[s]
	return o, ok, nil
}
func (f *fakeTarget) Run(_ context.Context, stmts []*sqlx.Builder) error {
	var texts []string
	for _, b := range stmts {
		st, err := b.Build()
		if err != nil {
			return err
		}
		texts = append(texts, st.SQL)
	}
	if f.failOn != "" && strings.Contains(strings.Join(texts, ";"), `"`+f.failOn+`"`) {
		return errors.New("boom")
	}
	f.ran = append(f.ran, texts)
	return nil
}

func quietLog() (*slog.Logger, *strings.Builder) {
	var sb strings.Builder
	return slog.New(slog.NewTextHandler(&sb, nil)), &sb
}

func TestApplyExtensionsFollowsTheLeader(t *testing.T) {
	log, _ := quietLog()
	ft := &fakeTarget{avail: map[string]bool{"pgcrypto": true, "kingres_columnar": true, "pg_stat_statements": true}, fixed: map[string]bool{"kingres_columnar": true}, schemas: map[string]string{"public": "pg_database_owner"}}
	d := wire.DatabaseSpec{Name: "db", Extensions: []string{"kingres_columnar", "kingres_shard", "pg_stat_statements", "pgcrypto"}, ExtensionPlaces: map[string]wire.ExtensionPlace{
		"kingres_columnar": {Schema: "kingres", Owner: "o"}, "pg_stat_statements": {Schema: "public", Owner: "pg_database_owner"}, "pgcrypto": {Schema: "x", Owner: "o"}}}
	if err := applyExtensions(context.Background(), log, ft, d); err != nil {
		t.Fatal(err)
	}
	// one Run per extension (a schema and its extension together), kingres_shard left to add_node, the fixed one without a schema
	if len(ft.ran) != 3 || len(ft.ran[0]) != 1 || len(ft.ran[1]) != 1 || len(ft.ran[2]) != 2 {
		t.Fatalf("ran %q", ft.ran)
	}
}

// A schema that exists on this server with another owner than on the leader skips that extension (logged, reported to the handler of the alert) and never fails
// the operation; the other extensions are still installed.
func TestApplyExtensionsRefusesASchemaOfAnotherOwner(t *testing.T) {
	log, _ := quietLog()
	ft := &fakeTarget{avail: map[string]bool{"pgcrypto": true, "hstore": true}, schemas: map[string]string{"x": "intruder"}}
	d := wire.DatabaseSpec{Name: "db", Extensions: []string{"hstore", "pgcrypto"}, ExtensionPlaces: map[string]wire.ExtensionPlace{
		"pgcrypto": {Schema: "x", Owner: "o"}, "hstore": {Schema: "y", Owner: "o"}}}
	var skippedExt, skippedWhy string
	err := applyExtensionsSkipping(context.Background(), log, ft, d, extHooks{skipped: func(e, why string) { skippedExt, skippedWhy = e, why }})
	if err != nil {
		t.Fatalf("a schema that a tenant made its own fails the operation: %v", err)
	}
	if skippedExt != "pgcrypto" || !strings.Contains(skippedWhy, "owned by intruder on this server and by o on the leader") {
		t.Errorf("skipped %q: %q", skippedExt, skippedWhy)
	}
	if len(ft.ran) != 1 || !strings.Contains(ft.ran[0][1], `"hstore"`) {
		t.Errorf("ran %q", ft.ran)
	}
	// the same owner is accepted
	ft = &fakeTarget{avail: map[string]bool{"pgcrypto": true}, schemas: map[string]string{"x": "o"}}
	if err := applyExtensions(context.Background(), log, ft, wire.DatabaseSpec{Name: "db", Extensions: []string{"pgcrypto"}, ExtensionPlaces: d.ExtensionPlaces}); err != nil || len(ft.ran) != 1 {
		t.Errorf("same owner: %v %q", err, ft.ran)
	}
}

// Extensions that the package lacks: pg_stat_statements is a warning, the others are one error that names them all, and the rest is installed.
func TestApplyExtensionsReportsEveryMissingExtension(t *testing.T) {
	log, logged := quietLog()
	ft := &fakeTarget{avail: map[string]bool{"pgcrypto": true}}
	d := wire.DatabaseSpec{Name: "db", Extensions: []string{"zeta", "pg_stat_statements", "pgcrypto", "alpha"}}
	err := applyExtensions(context.Background(), log, ft, d)
	if err == nil || !strings.Contains(err.Error(), "extensions alpha, zeta of database db") {
		t.Fatalf("error %v", err)
	}
	if strings.Contains(err.Error(), "pg_stat_statements") {
		t.Errorf("pg_stat_statements is an error: %v", err)
	}
	if !strings.Contains(logged.String(), "pg_stat_statements") || len(ft.ran) != 1 {
		t.Errorf("log %q, ran %q", logged.String(), ft.ran)
	}
	// only pg_stat_statements missing: no error
	ft = &fakeTarget{avail: map[string]bool{}}
	if err := applyExtensions(context.Background(), log, ft, wire.DatabaseSpec{Name: "db", Extensions: []string{"pg_stat_statements"}}); err != nil {
		t.Errorf("only pg_stat_statements missing: %v", err)
	}
	// a failing extension does not stop the next
	ft = &fakeTarget{avail: map[string]bool{"a": true, "b": true}, failOn: "a"}
	err = applyExtensions(context.Background(), log, ft, wire.DatabaseSpec{Name: "db", Extensions: []string{"a", "b"}})
	if err == nil || !strings.Contains(err.Error(), "installing extension a failed") || len(ft.ran) != 1 {
		t.Errorf("failure: %v, ran %q", err, ft.ran)
	}
}
