// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package cluster

import (
	"testing"

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
	if len(got) != 2 || got[1] != `CREATE EXTENSION IF NOT EXISTS "pg_stat_statements" SCHEMA "public"` {
		t.Errorf("public without owner: %q", got)
	}
}
