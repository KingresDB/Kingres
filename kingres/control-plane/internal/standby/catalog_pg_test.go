// SPDX-License-Identifier: AGPL-3.0-only

package standby

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/pgtest"
)

// stubCatalog makes a node table and add_node/remove_node of the extension's shape in the database "postgres" of in.
func stubCatalog(t *testing.T, in *pgtest.Instance, withStandby bool) *pgx.Conn {
	t.Helper()
	c := in.Connect(t, "postgres")
	for _, q := range []string{
		`CREATE SCHEMA kingres`,
		`CREATE TABLE kingres.node (node_id serial PRIMARY KEY, host text, port int, role text, group_id int)`,
		`INSERT INTO kingres.node (host, port, role, group_id) VALUES ('p1', 7001, 'primary', 0)`,
		`CREATE FUNCTION kingres.add_node(h text, p int, g int, r text) RETURNS int LANGUAGE sql AS
			'INSERT INTO kingres.node (host, port, role, group_id) VALUES (h, p, r, g) RETURNING node_id'`,
		`CREATE FUNCTION kingres.remove_node(n int) RETURNS void LANGUAGE sql AS 'DELETE FROM kingres.node WHERE node_id = n'`,
	} {
		if _, err := c.Exec(context.Background(), q); err != nil {
			t.Fatal(err)
		}
	}
	if withStandby {
		if _, err := c.Exec(context.Background(), `INSERT INTO kingres.node (host, port, role, group_id) VALUES ('s1', 7101, 'standby', 1)`); err != nil {
			t.Fatal(err)
		}
	}
	return c
}

// FO-08: the read-back of a registration or a removal reads every primary of the catalog, not the coordinator only:
// a node that went silent at the very end (the extension still answers success) is found.
func TestTheReadBackOfARegistrationReadsEveryPrimary(t *testing.T) {
	ctx := context.Background()
	coord := pgtest.Start(t, nil)
	other := pgtest.Start(t, nil)
	stubCatalog(t, coord, false)
	oc := stubCatalog(t, other, false) // the other primary's copy of the catalog: it did not get the row
	r := &PGRegistrar{
		Conn: func(ctx context.Context) (*pgx.Conn, error) { return pgx.Connect(ctx, coord.DSN("postgres")) },
		Remote: func(ctx context.Context, _ string, _ int) (*pgx.Conn, error) {
			return pgx.Connect(ctx, other.DSN("postgres"))
		},
	}
	e := Entry{ID: 7, Group: 1, Server: "s1", Port: 7101}
	if _, err := r.Register(ctx, e); err == nil || !strings.Contains(err.Error(), "not in the catalog of") {
		t.Fatalf("a registration that a primary did not receive was accepted: %v", err)
	}
	// the node got its part by now (what the repeat of the step finds): the registration is done
	if _, err := oc.Exec(ctx, `INSERT INTO kingres.node (node_id, host, port, role, group_id) VALUES (2, 's1', 7101, 'standby', 1)`); err != nil {
		t.Fatal(err)
	}
	if id, err := r.Register(ctx, e); err != nil || id != 2 {
		t.Fatalf("the repeat: %d %v", id, err)
	}
}

func TestTheReadBackOfARemovalReadsEveryPrimary(t *testing.T) {
	ctx := context.Background()
	coord := pgtest.Start(t, nil)
	other := pgtest.Start(t, nil)
	stubCatalog(t, coord, true)
	oc := stubCatalog(t, other, true) // the other primary still has the row
	r := &PGRegistrar{
		Conn: func(ctx context.Context) (*pgx.Conn, error) { return pgx.Connect(ctx, coord.DSN("postgres")) },
		Remote: func(ctx context.Context, _ string, _ int) (*pgx.Conn, error) {
			return pgx.Connect(ctx, other.DSN("postgres"))
		},
	}
	e := Entry{ID: 7, Group: 1, Server: "s1", Port: 7101, CatalogNode: 2}
	if err := r.Unregister(ctx, e); err == nil || !strings.Contains(err.Error(), "still has") {
		t.Fatalf("a removal that a primary did not receive was accepted: %v", err)
	}
	if _, err := oc.Exec(ctx, `DELETE FROM kingres.node WHERE host = 's1'`); err != nil {
		t.Fatal(err)
	}
	if err := r.Unregister(ctx, e); err != nil {
		t.Fatalf("the repeat: %v", err)
	}
	// a row of the same host on another port is not this standby's to remove
	if _, err := coord.Connect(t, "postgres").Exec(ctx, `INSERT INTO kingres.node (host, port, role, group_id) VALUES ('s1', 7202, 'standby', 1)`); err != nil {
		t.Fatal(err)
	}
	if err := r.Unregister(ctx, Entry{ID: 8, Group: 1, Server: "s1", Port: 7101, CatalogNode: 3}); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := coord.Connect(t, "postgres").QueryRow(ctx, `SELECT count(*) FROM kingres.node WHERE port = 7202`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("the row of another port was removed: %d %v", n, err)
	}
}
