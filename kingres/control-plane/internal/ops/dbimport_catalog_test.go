// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package ops

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/sysdb"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/transfer"
)

func TestStaleCatalogPrimaries(t *testing.T) {
	n := func(h string, p int32, r string) catalogNode { return catalogNode{h, p, r} }
	sys := []catalogNode{n("a", 5432, "standby"), n("b", 5432, "primary"), n("c", 5432, "primary")}
	for _, tc := range []struct {
		name string
		priv []catalogNode
		want []string
	}{
		{"agrees", []catalogNode{n("b", 5432, "primary"), n("c", 5432, "primary")}, nil},
		{"a failover of a group: the old primary is a standby now", []catalogNode{n("a", 5432, "primary"), n("b", 5432, "primary")}, []string{"a:5432"}},
		{"a node that is gone", []catalogNode{n("z", 5432, "primary")}, []string{"z:5432"}},
		{"another port is another node", []catalogNode{n("b", 5433, "primary")}, []string{"b:5433"}},
		{"a standby of the private catalog is not compared", []catalogNode{n("a", 5432, "standby"), n("b", 5432, "primary")}, nil},
		{"empty", nil, nil},
	} {
		if got := staleCatalogPrimaries(tc.priv, sys); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
}

// checkImportCatalog on a real instance: the private catalog and the system catalog are tables of the shape of kingres.node.
func TestCheckImportCatalogOnARealInstance(t *testing.T) {
	in, c, o := newGuardInstance(t)
	pgExecAll(t, c, "create database "+sysdb.Database, "create database "+privDB)
	mk := func(db string, rows ...string) {
		d := pgConnect(t, o, db)
		pgExecAll(t, d, "create schema kingres", "create table kingres.node (node_id int, group_id int, host text, port int, role text)")
		for _, r := range rows {
			pgExecAll(t, d, "insert into kingres.node values "+r)
		}
	}
	_ = in
	mk(sysdb.Database, "(1, 0, 'h1', 5432, 'standby')", "(2, 0, 'h2', 5432, 'primary')", "(3, 1, 'h3', 5432, 'primary')")
	// the private catalog after a failover of the map group from h1 to h2 that it did not follow
	mk(privDB, "(1, 0, 'h1', 5432, 'primary')", "(2, 1, 'h3', 5432, 'primary')")
	err := o.checkImportCatalog(context.Background(), privDB)
	var te *transfer.Error
	if !errors.As(err, &te) || te.Code != transfer.CodeLeaderChanged {
		t.Fatalf("a stale catalog: %v", err)
	}
	// the catalog is as the system catalog says
	d := pgConnect(t, o, privDB)
	pgExecAll(t, d, "update kingres.node set host = 'h2' where node_id = 1")
	if err := o.checkImportCatalog(context.Background(), privDB); err != nil {
		t.Fatalf("a catalog that agrees: %v", err)
	}
	// a private database that has no catalog (the archive has no sharded tables) is not compared
	pgExecAll(t, d, "drop schema kingres cascade")
	if err := o.checkImportCatalog(context.Background(), privDB); err != nil {
		t.Fatalf("no catalog: %v", err)
	}
}
