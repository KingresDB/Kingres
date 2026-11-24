// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package cluster

import (
	"context"
	"strings"
	"testing"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/wire"
)

// M2 of the review of g2: the owner of a database can make itself the owner of the schema public; pg_stat_statements in public is then installed all the same, the
// schema is not compared, and the add-server does not fail.
func TestApplyExtensionsIgnoresTheOwnerOfPublic(t *testing.T) {
	log, _ := quietLog()
	ft := &fakeTarget{avail: map[string]bool{"pg_stat_statements": true}, schemas: map[string]string{"public": "tenant"}}
	d := wire.DatabaseSpec{Name: "db", Extensions: []string{"pg_stat_statements"}, ExtensionPlaces: map[string]wire.ExtensionPlace{"pg_stat_statements": {Schema: "public", Owner: "pg_database_owner"}}}
	called := false
	if err := applyExtensionsSkipping(context.Background(), log, ft, d, func(string, string) { called = true }); err != nil {
		t.Fatalf("a tenant-owned public schema fails the operation: %v", err)
	}
	if called || len(ft.ran) != 1 || len(ft.ran[0]) != 1 || !strings.Contains(ft.ran[0][0], `SCHEMA "public"`) {
		t.Errorf("skipped %v, ran %q", called, ft.ran)
	}
}

// The schemas kingres and kingres_* are KingresDB's: an extension that is not one of its own is refused there, whatever the leader says.
func TestApplyExtensionsRefusesAReservedSchema(t *testing.T) {
	log, _ := quietLog()
	for _, schema := range []string{"kingres", "kingres_x", "Kingres_Transfer"} {
		ft := &fakeTarget{avail: map[string]bool{"pgcrypto": true}}
		d := wire.DatabaseSpec{Name: "db", Extensions: []string{"pgcrypto"}, ExtensionPlaces: map[string]wire.ExtensionPlace{"pgcrypto": {Schema: schema, Owner: "o"}}}
		if err := applyExtensions(context.Background(), log, ft, d); err == nil || len(ft.ran) != 0 {
			t.Errorf("%s: %v, ran %q", schema, err, ft.ran)
		}
	}
	// its own extension, fixed in its schema, is made as before
	ft := &fakeTarget{avail: map[string]bool{"kingres_columnar": true}, fixed: map[string]bool{"kingres_columnar": true}}
	d := wire.DatabaseSpec{Name: "db", Extensions: []string{"kingres_columnar"}, ExtensionPlaces: map[string]wire.ExtensionPlace{"kingres_columnar": {Schema: "kingres", Owner: "o"}}}
	if err := applyExtensions(context.Background(), log, ft, d); err != nil || len(ft.ran) != 1 {
		t.Errorf("kingres_columnar: %v %q", err, ft.ran)
	}
}
