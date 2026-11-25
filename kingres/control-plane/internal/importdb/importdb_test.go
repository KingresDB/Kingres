// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package importdb

import (
	"context"
	"reflect"
	"sort"
	"testing"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/pgtest"
)

const priv = Prefix + "15_a8ad1b646370"

// The walks over the databases leave out the private database of an import, and nothing else: a database that a client made under the prefix, that carries the
// comment of a task under another name, or that is not owned by the service role is a database like any other.
func TestIsPrivate(t *testing.T) {
	for _, tc := range []struct {
		name, comment string
		owned         bool
		want          bool
	}{
		{priv, Marker(priv), true, true},
		// the forged marker: the name and the comment can be written by a role with CREATEDB, the owner cannot
		{priv, Marker(priv), false, false},
		{priv, "", true, false},
		{priv, "", false, false},
		{priv, Marker(Prefix + "16_a8ad1b646370"), true, false},
		{priv, "kingres-import:", true, false},
		{priv, Marker(priv) + " ", true, false},
		{priv, "KINGRES-IMPORT:15_a8ad1b646370", true, false},
		{"src", Marker(priv), true, false},
		{"dil_base", "", true, false},
		{"kingres_system", "", true, false},
		// the final name after the rename keeps the marker until the last server has it: it is a database of the client by then
		{"dif_x", Marker(priv), true, false},
	} {
		if got := IsPrivate(tc.name, tc.comment, tc.owned); got != tc.want {
			t.Errorf("IsPrivate(%q, %q, %v) = %v, want %v", tc.name, tc.comment, tc.owned, got, tc.want)
		}
	}
}

// List runs the query of every walk on a real instance: the private database that the service role made and marked is left out, and so is nothing else (the one
// that a role with CREATEDB made under the same name and marker, the one that has the prefix and no marker, the one that has the marker of another name).
func TestListOnARealInstance(t *testing.T) {
	if !pgtest.Available() {
		t.Skip("KINGRES_PG_BIN is not set; skipping the PostgreSQL integration test")
	}
	in := pgtest.Start(t, nil)
	c := in.Connect(t, "postgres")
	ctx := context.Background()
	forged := Prefix + "16_0123456789ab"
	unmarked := Prefix + "17_0123456789ab"
	wrongMarker := Prefix + "18_0123456789ab"
	for _, q := range []string{
		"create role tenant login createdb",
		"create database " + priv,
		"comment on database " + priv + " is '" + Marker(priv) + "'",
		"create database " + forged + " owner tenant",
		"comment on database " + forged + " is '" + Marker(forged) + "'",
		"create database " + unmarked,
		"create database " + wrongMarker,
		"comment on database " + wrongMarker + " is '" + Marker(priv) + "'",
		"create database shop",
	} {
		if _, err := c.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	got, err := List(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	// the order is the collation of the instance, so the names are compared as a set
	want := []string{forged, unmarked, wrongMarker, "postgres", "shop"}
	sort.Strings(got)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("List = %v, want %v", got, want)
	}
}
