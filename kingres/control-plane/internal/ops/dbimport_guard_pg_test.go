// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package ops

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/pgtest"
)

// Tests of what keeps the importing role, and every role that can act as it, away from the objects that the service role works on (review of 14c): the withdrawal,
// the prepared transactions, the lock before the service role acts, the importer, the sequences, the undoing of a rename and the walk of the catalog. They run
// against a real instance (PostgreSQL 18 or 19) with the operations of this package on a service connection to it.

// pgSvc makes the connections of the service role to a test instance.
type pgSvc struct{ in *pgtest.Instance }

func (s pgSvc) ServiceConn(ctx context.Context, db string) (*pgx.Conn, error) {
	cfg, err := pgx.ParseConfig(s.in.DSN(db))
	if err != nil {
		return nil, err
	}
	return pgx.ConnectConfig(ctx, cfg)
}

// asRole connects to db as a role that has the password "pw".
func asRole(in *pgtest.Instance, db, role string) (*pgx.Conn, error) {
	cfg, err := pgx.ParseConfig(in.DSN(db))
	if err != nil {
		return nil, err
	}
	cfg.User, cfg.Password = role, "pw"
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	return pgx.ConnectConfig(ctx, cfg)
}

func newGuardInstance(t *testing.T) (*pgtest.Instance, *pgx.Conn, *Ops) {
	t.Helper()
	if !pgtest.Available() {
		t.Skip("KINGRES_PG_BIN is not set; skipping the PostgreSQL integration test")
	}
	in := pgtest.Start(t, nil)
	c := in.Connect(t, "postgres")
	return in, c, &Ops{Service: pgSvc{in}}
}

const privDB = "kingres_import_7_aabbccddeeff"

// makePrivate makes a private database the way the import does (owned by the service role, marked, PUBLIC locked out) and gives imp what the import gives it.
func makePrivate(t *testing.T, o *Ops, c *pgx.Conn) {
	t.Helper()
	pgExecAll(t, c, "create role imp login password 'pw'", "create role sub login password 'pw' in role imp", "create role bystander login password 'pw'", "create role outsider login password 'pw'")
	if err := o.createPrivateLocal(context.Background(), importCreateOp{Name: privDB, Marker: importMarker(privDB), Encoding: "UTF8"}); err != nil {
		t.Fatal(err)
	}
	pgExecAll(t, c, "grant connect, create, temporary on database "+privDB+" to imp", "grant connect on database "+privDB+" to bystander")
	d := pgConnect(t, o, privDB)
	pgExecAll(t, d, "grant usage, create on schema public to imp")
}

func pgConnect(t *testing.T, o *Ops, db string) *pgx.Conn {
	t.Helper()
	d, err := o.Service.ServiceConn(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close(context.Background()) })
	return d
}

// C1 (layer 3): a login role that is a member of the importer keeps its session when only the importer's own right to connect is withdrawn. The withdrawal takes
// the right from PUBLIC and from every role that holds it, ends every session of the database and looks again.
func TestWithdrawalEndsTheSessionsOfMembersAndOfEveryRole(t *testing.T) {
	in, c, o := newGuardInstance(t)
	makePrivate(t, o, c)
	// Something granted the right to everybody (a role that cannot be named in advance).
	pgExecAll(t, c, "grant connect on database "+privDB+" to public")
	ctx := context.Background()
	var sessions []*pgx.Conn
	for _, r := range []string{"imp", "sub", "bystander", "outsider"} {
		s, err := asRole(in, privDB, r)
		if err != nil {
			t.Fatalf("%s: %v", r, err)
		}
		defer func() { _ = s.Close(ctx) }()
		sessions = append(sessions, s)
	}
	if err := o.importAccessLocal(ctx, importAccessOp{Name: privDB, Role: "imp", Allow: false, Full: true}); err != nil {
		t.Fatal(err)
	}
	for i, s := range sessions {
		pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := s.Ping(pctx)
		cancel()
		if err == nil {
			t.Errorf("the session %d of the database survives the withdrawal", i)
		}
	}
	for _, r := range []string{"imp", "sub", "bystander", "outsider"} {
		if s, err := asRole(in, privDB, r); err == nil {
			_ = s.Close(ctx)
			t.Errorf("%s can connect after the withdrawal", r)
		}
	}
	var n int
	if err := c.QueryRow(ctx, `select count(*) from pg_stat_activity a join pg_roles r on r.rolname = a.usename where a.datname = $1 and not r.rolsuper`, privDB).Scan(&n); err != nil || n != 0 {
		t.Errorf("sessions of roles that are not superusers: %d (%v)", n, err)
	}
	// The access can be given again (the next role phase).
	if err := o.importAccessLocal(ctx, importAccessOp{Name: privDB, Role: "imp", Allow: true, Full: true}); err != nil {
		t.Fatal(err)
	}
	s, err := asRole(in, privDB, "imp")
	if err != nil {
		t.Fatalf("imp after the access is given again: %v", err)
	}
	_ = s.Close(ctx)
}

// M1: a prepared transaction keeps its locks and keeps the database in use: a rename and a drop fail for ever while one is there.
func TestPreparedTransactionsAreRolledBack(t *testing.T) {
	in, c, o := newGuardInstance(t)
	makePrivate(t, o, c)
	ctx := context.Background()
	prepare := func(gid string) {
		s, err := asRole(in, privDB, "imp")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = s.Close(ctx) }()
		pgExecAll(t, s, "begin", "create table public.pt_"+gid+" (id int)", "lock table public.pt_"+gid+" in access exclusive mode", "prepare transaction '"+gid+"'")
	}
	count := func() int {
		var n int
		if err := c.QueryRow(ctx, `select count(*) from pg_prepared_xacts where database = $1`, privDB).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	// The withdrawal rolls it back.
	prepare("a")
	if count() != 1 {
		t.Fatal("no prepared transaction")
	}
	if err := o.importAccessLocal(ctx, importAccessOp{Name: privDB, Role: "imp", Allow: false, Full: true}); err != nil {
		t.Fatal(err)
	}
	if count() != 0 {
		t.Errorf("the withdrawal leaves %d prepared transactions", count())
	}
	// The rename rolls it back (a transaction that was prepared before the rename).
	pgExecAll(t, c, "grant connect on database "+privDB+" to imp")
	prepare("b")
	if err := o.importRenameLocal(ctx, importRenameOp{From: privDB, To: "shop_final", Owner: "imp"}); err != nil {
		t.Fatalf("the rename with a prepared transaction: %v", err)
	}
	if count() != 0 {
		t.Error("a prepared transaction of the renamed database")
	}
	var owner string
	if err := c.QueryRow(ctx, `select pg_get_userbyid(datdba) from pg_database where datname = 'shop_final'`).Scan(&owner); err != nil || owner != "kingresd" {
		t.Errorf("owner after the rename: %q (%v), the service role keeps the database until it is opened", owner, err)
	}
	if err := o.importRenameLocal(ctx, importRenameOp{From: "shop_final", To: privDB, Owner: "imp", Open: true}); err != nil {
		t.Fatal(err)
	}
	if err := c.QueryRow(ctx, `select pg_get_userbyid(datdba) from pg_database where datname = 'shop_final'`).Scan(&owner); err != nil || owner != "imp" {
		t.Errorf("owner after the opening: %q (%v)", owner, err)
	}
}

func TestDropRollsBackPreparedTransactions(t *testing.T) {
	in, c, o := newGuardInstance(t)
	makePrivate(t, o, c)
	ctx := context.Background()
	s, err := asRole(in, privDB, "imp")
	if err != nil {
		t.Fatal(err)
	}
	pgExecAll(t, s, "begin", "create table public.pt (id int)", "lock table public.pt in access exclusive mode", "prepare transaction 'x'")
	_ = s.Close(ctx)
	if err := o.importDropLocal(ctx, privDB); err != nil {
		t.Fatalf("the drop with a prepared transaction: %v", err)
	}
	var n int
	if err := c.QueryRow(ctx, `select count(*) from pg_database where datname = $1`, privDB).Scan(&n); err != nil || n != 0 {
		t.Errorf("the database is still there: %d (%v)", n, err)
	}
	// A database that carries another marker, or that a role owns, is not dropped.
	pgExecAll(t, c, "create database kingres_import_8_001122334455", "create database kingres_import_9_001122334455 owner imp")
	for _, name := range []string{"kingres_import_8_001122334455", "kingres_import_9_001122334455"} {
		if err := o.importDropLocal(ctx, name); err == nil {
			t.Errorf("%s was dropped although this import did not make it", name)
		}
	}
}

// L1: a failure after the rename (the opening of the database) is undone: the private name, the owner, the marker and the closed door are back.
func TestRenameUndoesItselfOnAFailure(t *testing.T) {
	in, c, o := newGuardInstance(t)
	makePrivate(t, o, c)
	ctx := context.Background()
	if err := o.importRenameLocal(ctx, importRenameOp{From: privDB, To: "shop_final", Owner: "no_such_role"}); err != nil {
		t.Fatalf("the rename itself does not need the role: %v", err)
	}
	if err := o.importRenameLocal(ctx, importRenameOp{From: "shop_final", To: privDB, Owner: "no_such_role", Open: true}); err == nil {
		t.Fatal("the opening succeeded for a role that does not exist")
	}
	if err := o.importRenameLocal(ctx, importRenameOp{From: "shop_final", To: privDB, Back: true, Marker: importMarker(privDB)}); err != nil {
		t.Fatal(err)
	}
	var names string
	if e := c.QueryRow(ctx, `select string_agg(datname, ',' order by datname) from pg_database where datname like 'kingres_import_%' or datname = 'shop_final'`).Scan(&names); e != nil || names != privDB {
		t.Errorf("databases after the failure: %q (%v)", names, e)
	}
	var owner, marker string
	if e := c.QueryRow(ctx, `select pg_get_userbyid(datdba), coalesce(shobj_description(oid, 'pg_database'), '') from pg_database where datname = $1`, privDB).Scan(&owner, &marker); e != nil || owner != "kingresd" || marker != importMarker(privDB) {
		t.Errorf("owner %q marker %q (%v)", owner, marker, e)
	}
	if s, e := asRole(in, privDB, "outsider"); e == nil {
		_ = s.Close(ctx)
		t.Error("PUBLIC may connect to the database that the failure left")
	}
	// The hook can drop it.
	if e := o.importDropLocal(ctx, privDB); e != nil {
		t.Errorf("the hook cannot drop the database after the failure: %v", e)
	}
}

// M2: a sequence is set only in a schema that is not the system's or an extension's, when the importing role owns it and (on the coordinator) it is linked to the
// column that the catalog of the archive names.
func TestRaiseSequenceRefusals(t *testing.T) {
	_, c, o := newGuardInstance(t)
	ctx := context.Background()
	pgExecAll(t, c, "create role imp", "create schema kingres_x", "create schema app", "grant usage, create on schema public, app to imp",
		"create table app.t (id bigserial primary key)", "alter table app.t owner to imp", "alter sequence app.t_id_seq owner to imp",
		"create sequence kingres_x.s1", "alter sequence kingres_x.s1 owner to imp",
		"create sequence public.s_super", "create sequence public.s_free", "alter sequence public.s_free owner to imp",
		"create table public.u (id bigserial)", "alter table public.u owner to imp")
	val := func(n string) int64 {
		var v *int64
		if err := c.QueryRow(ctx, "select last_value from "+n).Scan(&v); err != nil {
			t.Fatal(err)
		}
		if v == nil {
			return 0
		}
		return *v
	}
	cases := []struct {
		name string
		it   importSeqItem
		full bool
		ok   bool
	}{
		{"an owned sequence", importSeqItem{Schema: "app", Name: "t_id_seq", Value: 500, Table: "app.t", Column: "id"}, true, true},
		{"a sequence of a schema of an extension", importSeqItem{Schema: "kingres_x", Name: "s1", Value: 500, Table: "app.t", Column: "id"}, true, false},
		{"a sequence of the schema kingres", importSeqItem{Schema: "kingres", Name: "colocation_id_seq", Value: 500, Table: "app.t", Column: "id"}, true, false},
		{"a sequence of pg_catalog", importSeqItem{Schema: "pg_catalog", Name: "pg_class_oid_index", Value: 500, Table: "app.t", Column: "id"}, true, false},
		{"a sequence that a superuser owns", importSeqItem{Schema: "public", Name: "s_super", Value: 500, Table: "app.t", Column: "id"}, true, false},
		{"a sequence that no column owns", importSeqItem{Schema: "public", Name: "s_free", Value: 500, Table: "app.t", Column: "id"}, true, false},
		{"a sequence of another column", importSeqItem{Schema: "public", Name: "u_id_seq", Value: 500, Table: "app.t", Column: "id"}, true, false},
		{"the column that a sequence belongs to, named wrongly", importSeqItem{Schema: "app", Name: "t_id_seq", Value: 900, Table: "app.t", Column: "nope"}, true, false},
	}
	for _, tc := range cases {
		conn := pgConnect(t, o, "postgres")
		_, _ = conn.Exec(ctx, "SET search_path = pg_catalog")
		err := raiseSequence(ctx, conn, tc.it, "imp", tc.full)
		if (err == nil) != tc.ok {
			t.Errorf("%s: %v, want ok=%v", tc.name, err, tc.ok)
		}
	}
	if val("app.t_id_seq") != 500 || val("public.s_super") != 1 || val("public.s_free") != 1 || val("kingres_x.s1") != 1 || val("public.u_id_seq") != 1 {
		t.Errorf("values: %d %d %d %d %d", val("app.t_id_seq"), val("public.s_super"), val("public.s_free"), val("kingres_x.s1"), val("public.u_id_seq"))
	}
	// Away from the coordinator (full is false) a refused sequence is skipped, and one that the role owns is set without the link.
	conn := pgConnect(t, o, "postgres")
	if err := raiseSequence(ctx, conn, importSeqItem{Schema: "kingres_x", Name: "s1", Value: 700, Table: "app.t", Column: "id"}, "imp", false); err == nil || val("kingres_x.s1") != 1 {
		t.Errorf("a protected schema away from the coordinator: %v, value %d", err, val("kingres_x.s1"))
	}
	if err := raiseSequence(ctx, conn, importSeqItem{Schema: "public", Name: "s_super", Value: 700, Table: "app.t", Column: "id"}, "imp", false); err != nil || val("public.s_super") != 1 {
		t.Errorf("a sequence of another owner away from the coordinator: %v, value %d", err, val("public.s_super"))
	}
	if err := raiseSequence(ctx, conn, importSeqItem{Schema: "public", Name: "s_free", Value: 700, Table: "app.t", Column: "id"}, "imp", false); err != nil || val("public.s_free") != 700 {
		t.Errorf("an owned sequence away from the coordinator: %v, value %d", err, val("public.s_free"))
	}
}

// L6 and C1 (layer 4): a role that has CREATEROLE may import (the coordinator's decision); a role that has a member that can log in (directly or through another), may not import.
func TestImporterRefusal(t *testing.T) {
	_, c, _ := newGuardInstance(t)
	ctx := context.Background()
	pgExecAll(t, c, "create role plain login", "create role cr login createrole", "create role withlogin login", "create role m1 login in role withlogin",
		"create role withnologin login", "create role m2 nologin in role withnologin", "create role deep login", "create role mid nologin in role deep", "create role leaf login in role mid",
		"create role adminonly login", "create role creatorlike login", "grant adminonly to creatorlike with admin true, inherit false, set false",
		"create role setonly login", "create role setter login", "grant setonly to setter with admin false, inherit false, set true")
	for role, want := range map[string]string{"plain": "", "cr": "", "withlogin": "members", "withnologin": "", "deep": "members", "m1": "", "no_such_role": "missing", "adminonly": "", "setonly": "members"} {
		got, err := importerRefusal(ctx, c, role)
		if err != nil || got != want {
			t.Errorf("%s: %q (%v), want %q", role, got, err, want)
		}
	}
}

// C1 (layer 1): the service role never acts on a table of the role outside a transaction that has locked it. A member session swaps the table for a view that calls
// a function of the role; a service transaction that checks the table and then counts it would run the function as a superuser. With the lock the swap waits.
func TestLockedTableDefeatsAViewSwap(t *testing.T) {
	in, c, o := newGuardInstance(t)
	ctx := context.Background()
	pgExecAll(t, c, "create role imp login password 'pw'", "grant create on schema public to imp")
	imp, err := asRole(in, "postgres", "imp")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = imp.Close(ctx) }()
	pgExecAll(t, imp, "create table public.t (id int)", "insert into public.t values (1), (2)", "create table public.proof (who text, su boolean)",
		"create function public.evil() returns bigint language sql as $$ insert into public.proof select current_user, (select rolsuper from pg_roles where rolname = current_user); select 1::bigint $$")
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			for _, q := range []string{"begin", "alter table public.t rename to t_real", "create view public.t as select public.evil() as c", "commit", "begin", "drop view public.t",
				"alter table public.t_real rename to t", "commit"} {
				if _, err := imp.Exec(ctx, q); err != nil {
					_, _ = imp.Exec(ctx, "rollback")
					break
				}
			}
		}
	}()
	svc := pgConnect(t, o, "postgres")
	if _, err := svc.Exec(ctx, "SET search_path = pg_catalog"); err != nil {
		t.Fatal(err)
	}
	counted := 0
	var lastErr error
	for i := 0; i < 400; i++ {
		err := withTrustedTable(ctx, svc, "public", "t", "imp", map[string]string{}, func(tx pgx.Tx) error {
			var n int64
			return tx.QueryRow(ctx, "SELECT pg_catalog.count(*) FROM public.t").Scan(&n)
		})
		if err == nil {
			counted++
		} else {
			lastErr = err
		}
	}
	close(stop)
	wg.Wait()
	var supers int
	if err := c.QueryRow(ctx, "select count(*) from public.proof where su").Scan(&supers); err != nil {
		t.Fatal(err)
	}
	if supers != 0 {
		t.Errorf("the function of the role ran as a superuser %d times in %d locked reads", supers, counted)
	}
	if counted == 0 {
		t.Errorf("no read was made (the last error: %v)", lastErr)
	}
}

// M3: the walk of the catalog finds code that the text of an expression does not show: an operator, a domain of a column, an extended statistic.
func TestTableTrustedWalksTheCatalog(t *testing.T) {
	_, c, _ := newGuardInstance(t)
	ctx := context.Background()
	pgExecAll(t, c, "create role imp_owner", "grant create, usage on schema public to imp_owner", "set role imp_owner",
		"create function public.cmp(int, int) returns boolean language sql immutable as 'select true'",
		"create function public.one(int) returns int language sql immutable as 'select 1'",
		"create operator public.=== (leftarg = int, rightarg = int, function = public.cmp)",
		"create domain public.dom as int check (value operator(public.===) value)",
		"create domain public.dom2 as int check (public.one(value) > 0)",
		"create domain public.dom_ok as int check (value > 0)",
		"create table public.t_ok (id int primary key, v public.dom_ok, e text)",
		"create table public.t_op (id int, v int, check (v operator(public.===) v))",
		"create table public.t_dom (id int, v public.dom)",
		"create table public.t_dom2 (id int, v public.dom2)",
		"create table public.t_stat (id int, a int, b int)",
		"create statistics public.st on (public.one(a)), b from public.t_stat",
		"create table public.t_part (id int, d date) partition by range (d)",
		"create table public.t_part_1 partition of public.t_part for values from ('2026-01-01') to ('2027-01-01')",
		"create table public.t_idx (id int, v int)",
		"create index on public.t_idx ((v operator(public.===) v))",
		// A domain with a function of the role in its CHECK, reached only through a composite type, an array of it, a range or a multirange (the review of 14c, L-A).
		"create type public.comp as (a public.dom, b int)", "create table public.t_comp (id int, v public.comp)", "create table public.t_comp_arr (id int, v public.comp[])",
		"create type public.comp_ok as (a public.dom_ok, b int)", "create table public.t_comp_ok (id int, v public.comp_ok)",
		"create function public.dfn(int, int) returns float8 language sql immutable as 'select 1.0::float8'",
		"create type public.rng as range (subtype = int4, subtype_diff = public.dfn)",
		"create table public.t_rng (id int, v public.rng)", "create table public.t_mrng (id int, v public.rng_multirange)",
		"reset role")
	for table, ok := range map[string]bool{"t_ok": true, "t_part": true, "t_op": false, "t_dom": false, "t_dom2": false, "t_stat": false, "t_idx": false,
		"t_comp": false, "t_comp_arr": false, "t_comp_ok": true, "t_rng": false, "t_mrng": false} {
		tx, err := c.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		err = tableTrusted(ctx, tx, "public", table, "imp_owner", map[string]string{})
		_ = tx.Rollback(ctx)
		if (err == nil) != ok {
			t.Errorf("table %s: trusted=%v (%v), want %v", table, err == nil, err, ok)
		}
	}
	tx, err := c.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// An object of an extension of the closed list is not a finding.
	if _, err := tx.Exec(ctx, "create extension if not exists citext"); err == nil {
		if _, err := tx.Exec(ctx, "create table public.t_ci (id int, v public.citext) "); err == nil {
			_, _ = tx.Exec(ctx, "alter table public.t_ci owner to imp_owner")
			if err := tableTrusted(ctx, tx, "public", "t_ci", "imp_owner", map[string]string{"citext": "public"}); err != nil {
				t.Errorf("a column of citext: %v", err)
			}
		}
	}
	_ = fmt.Sprint
	_ = strings.TrimSpace
}

// C1 (layer 3), the start-up window: a session that was in its start-up when the right to connect went shows up after a round in which none was found. The loop
// looks again after a quiet round, so the late session is ended too. The hook makes the late session at round 1.
func TestWithdrawalLooksAgainAfterAQuietRound(t *testing.T) {
	in, c, o := newGuardInstance(t)
	makePrivate(t, o, c)
	ctx := context.Background()
	var late *pgx.Conn
	var rounds []int
	withdrawHook = func(hctx context.Context, round int) {
		rounds = append(rounds, round)
		if round != 1 || late != nil {
			return
		}
		pgExecAll(t, c, "grant connect on database "+privDB+" to imp")
		s, err := asRole(in, privDB, "imp")
		pgExecAll(t, c, "revoke connect on database "+privDB+" from imp")
		if err != nil {
			t.Errorf("the late session: %v", err)
			return
		}
		late = s
	}
	defer func() { withdrawHook = nil }()
	if err := o.importAccessLocal(ctx, importAccessOp{Name: privDB, Role: "imp", Allow: false, Full: true}); err != nil {
		t.Fatal(err)
	}
	if late == nil {
		t.Fatalf("the loop did not run a second round (rounds %v): a quiet round ends it", rounds)
	}
	defer func() { _ = late.Close(ctx) }()
	pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := late.Ping(pctx); err == nil {
		t.Error("the session that appeared after a quiet round survives the withdrawal")
	}
}

// L-B (the second review of 14c): the undoing of a rename on a peer only touches a database that carries the marker of the task. A peer where the final name is a
// database of somebody else (the rename failed there, so it was never ours) is left as it is; the marker stays on the final name until the rename is finished.
func TestRenameBackLeavesAForeignDatabaseAlone(t *testing.T) {
	_, c, o := newGuardInstance(t)
	ctx := context.Background()
	pgExecAll(t, c, "create role stranger login", "create database shop_final owner stranger", "create database shop_other owner stranger", "comment on database shop_other is 'something else'")
	state := func(name string) string {
		var s string
		if err := c.QueryRow(ctx, `select pg_get_userbyid(datdba) || '/' || coalesce(shobj_description(oid, 'pg_database'), '') from pg_database where datname = $1`, name).Scan(&s); err != nil {
			return "missing"
		}
		return s
	}
	for _, name := range []string{"shop_final", "shop_other"} {
		before := state(name)
		if err := o.importRenameLocal(ctx, importRenameOp{From: name, To: privDB, Back: true, Marker: importMarker(privDB)}); err != nil {
			t.Errorf("%s: the undoing of a rename that was not ours returned %v", name, err)
		}
		if state(name) != before || state(privDB) != "missing" {
			t.Errorf("%s: a database that is not ours was changed: %q, the private name is %q", name, state(name), state(privDB))
		}
	}
	// A marker that is not the task's, or none, is refused outright.
	for _, m := range []string{"", importMarker("kingres_import_8_001122334455")} {
		if err := o.importRenameLocal(ctx, importRenameOp{From: "shop_final", To: privDB, Back: true, Marker: m}); err == nil {
			t.Errorf("a rename back with the marker %q is accepted", m)
		}
	}
	// The marker stays on the final name until the rename is finished, and the undoing works while it is there.
	pgExecAll(t, c, "drop database shop_final", "drop database shop_other")
	makePrivate(t, o, c)
	if err := o.importRenameLocal(ctx, importRenameOp{From: privDB, To: "shop_final", Owner: "imp"}); err != nil {
		t.Fatal(err)
	}
	if got := state("shop_final"); got != "kingresd/"+importMarker(privDB) {
		t.Errorf("the final name after the rename: %q", got)
	}
	if err := o.importRenameLocal(ctx, importRenameOp{From: "shop_final", To: privDB, Back: true, Marker: importMarker(privDB)}); err != nil {
		t.Fatal(err)
	}
	if state("shop_final") != "missing" || state(privDB) != "kingresd/"+importMarker(privDB) {
		t.Errorf("after the undoing: final %q, private %q", state("shop_final"), state(privDB))
	}
	if err := o.importRenameLocal(ctx, importRenameOp{From: privDB, To: "shop_final", Owner: "imp"}); err != nil {
		t.Fatal(err)
	}
	if err := o.importRenameLocal(ctx, importRenameOp{From: "shop_final", To: privDB, Owner: "imp", Open: true}); err != nil {
		t.Fatal(err)
	}
	if err := o.importRenameLocal(ctx, importRenameOp{From: "shop_final", To: privDB, Finish: true, Marker: importMarker(privDB)}); err != nil {
		t.Fatal(err)
	}
	if got := state("shop_final"); got != "imp/" {
		t.Errorf("the final name after the finish: %q", got)
	}
	if err := o.importRenameLocal(ctx, importRenameOp{From: "shop_final", To: privDB, Back: true, Marker: importMarker(privDB)}); err != nil || state("shop_final") != "imp/" {
		t.Errorf("a finished rename was undone: %v %q", err, state("shop_final"))
	}
}

// L-C: every relation of a schema that is distributed is listed, locked and checked; a table that the archive's catalog does not name is refused.
func TestLockSchemaTablesRefusesWhatTheCatalogDoesNotName(t *testing.T) {
	_, c, _ := newGuardInstance(t)
	ctx := context.Background()
	pgExecAll(t, c, "create role imp_owner", "create schema s1", "create table s1.a (id int primary key)", "create table s1.b (id int primary key)",
		"create table s1.c (id int, v int, check (v > 0))", "alter table s1.a owner to imp_owner", "alter table s1.b owner to imp_owner", "alter table s1.c owner to imp_owner")
	cases := []struct {
		name  string
		known map[string]bool
		extra string
		ok    bool
	}{
		{"every table named", map[string]bool{"a": true, "b": true, "c": true}, "", true},
		{"a table that the catalog does not name", map[string]bool{"a": true, "c": true}, "", false},
		{"a view in the schema", map[string]bool{"a": true, "b": true, "c": true}, "create view s1.v as select 1", false},
		{"a table that holds code of the role", map[string]bool{"a": true, "b": true, "c": true, "d": true}, "create function public.chk(int) returns boolean language sql immutable as 'select true'; alter function public.chk(int) owner to imp_owner; create table s1.d (id int, v int, check (public.chk(v))); alter table s1.d owner to imp_owner", false},
	}
	for _, tc := range cases {
		tx, err := c.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if tc.extra != "" {
			if _, err := tx.Exec(ctx, tc.extra); err != nil {
				t.Fatal(err)
			}
		}
		err = lockSchemaTables(ctx, tx, "s1", tc.known, "imp_owner", map[string]string{})
		_ = tx.Rollback(ctx)
		if (err == nil) != tc.ok {
			t.Errorf("%s: %v, want ok=%v", tc.name, err, tc.ok)
		}
	}
}

// finalState is owner/marker of a database ("missing" when it is not there).
func finalState(c *pgx.Conn, name string) string {
	var s string
	if err := c.QueryRow(context.Background(), `select pg_get_userbyid(datdba) || '/' || coalesce(shobj_description(oid, 'pg_database'), '') from pg_database where datname = $1`, name).Scan(&s); err != nil {
		return "missing"
	}
	return s
}

// M1 (the fourth review of the import): the final name is open to nobody between the rename and the opening, which happens after the catalogs were compared again.
// No role can connect to it and the importing role does not own it until the opening.
func TestNoClientConnectsBetweenTheRenameAndTheOpening(t *testing.T) {
	in, c, o := newGuardInstance(t)
	makePrivate(t, o, c)
	ctx := context.Background()
	// the access of the role is withdrawn before the rename, as the step does
	if err := o.importAccessLocal(ctx, importAccessOp{Name: privDB, Role: "imp", Allow: false, Full: true}); err != nil {
		t.Fatal(err)
	}
	if err := o.importRenameLocal(ctx, importRenameOp{From: privDB, To: "shop_final", Owner: "imp"}); err != nil {
		t.Fatal(err)
	}
	for _, role := range []string{"outsider", "imp", "bystander"} {
		if s, err := asRole(in, "shop_final", role); err == nil {
			_ = s.Close(ctx)
			t.Errorf("%s connected to the final name before the recheck passed", role)
		}
	}
	if got := finalState(c, "shop_final"); got != "kingresd/"+importMarker(privDB) {
		t.Errorf("between the rename and the opening: %q", got)
	}
	if err := o.importRenameLocal(ctx, importRenameOp{From: "shop_final", To: privDB, Owner: "imp", Open: true}); err != nil {
		t.Fatal(err)
	}
	s, err := asRole(in, "shop_final", "outsider")
	if err != nil {
		t.Fatalf("PUBLIC cannot connect after the opening: %v", err)
	}
	_ = s.Close(ctx)
	if got := finalState(c, "shop_final"); got != "imp/"+importMarker(privDB) {
		t.Errorf("after the opening: %q", got)
	}
	// a database without the marker of the task is never opened
	pgExecAll(t, c, "create database shop_other")
	if err := o.importRenameLocal(ctx, importRenameOp{From: "shop_other", To: privDB, Owner: "imp", Open: true}); err == nil {
		t.Error("a database without the marker was opened")
	}
}

// recFanout records what the failure hook sends to the other servers.
type recFanout struct {
	mu   sync.Mutex
	sent []string
}

func (f *recFanout) Others(context.Context) ([]OtherServer, error) {
	return []OtherServer{{ID: "s2"}}, nil
}
func (f *recFanout) Apply(_ context.Context, server, kind string, params any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if op, ok := params.(importRenameOp); ok {
		f.sent = append(f.sent, fmt.Sprintf("%s %s back=%t from=%s to=%s", server, kind, op.Back, op.From, op.To))
	}
	return nil
}
func (f *recFanout) Register(context.Context, string, OtherServer) error { return nil }

// M2 and M3 (the fourth review of the import): the rename step can be run again after a restart. A database at the final name that carries the marker of this task is
// its own earlier rename (the rename answers nothing to do, and the opening can be repeated); any other database at that name is "exists". When the task fails after
// that, the failure hook puts the database back by the marker, on this server and on the others, and then drops it as the private database; a database without the
// marker is never touched.
func TestTheRenameCanBeRunAgainAfterARestartAndTheHookPutsItBack(t *testing.T) {
	_, c, o := newGuardInstance(t)
	makePrivate(t, o, c)
	ctx := context.Background()
	rename := importRenameOp{From: privDB, To: "shop_final", Owner: "imp"}
	if err := o.importRenameLocal(ctx, rename); err != nil {
		t.Fatal(err)
	}
	want := "kingresd/" + importMarker(privDB)
	// the restart: the step runs again
	if err := o.importRenameLocal(ctx, rename); err != nil {
		t.Fatalf("the rename after a restart: %v", err)
	}
	if got := finalState(c, "shop_final"); got != want {
		t.Errorf("after the second run: %q", got)
	}
	// ... and again after the opening
	open := importRenameOp{From: "shop_final", To: privDB, Owner: "imp", Open: true}
	if err := o.importRenameLocal(ctx, open); err != nil {
		t.Fatal(err)
	}
	if err := o.importRenameLocal(ctx, rename); err != nil || finalState(c, "shop_final") != "imp/"+importMarker(privDB) {
		t.Errorf("the rename after the opening: %v %q", err, finalState(c, "shop_final"))
	}
	if err := o.importRenameLocal(ctx, open); err != nil {
		t.Errorf("the opening twice: %v", err)
	}
	// a database of somebody else at the final name, or one with the marker of another task, is "exists"
	pgExecAll(t, c, "create database shop_x", "create database shop_y", "comment on database shop_y is '"+importMarker("kingres_import_8_001122334455")+"'")
	for _, name := range []string{"shop_x", "shop_y"} {
		if err := o.importRenameLocal(ctx, importRenameOp{From: privDB, To: name, Owner: "imp"}); !errors.Is(err, errDatabaseExists) {
			t.Errorf("%s: %v, want the database exists", name, err)
		}
	}
	// the hook: the task failed after the rename; the database is at the final name with the marker (service ownership: the opening is undone first)
	if err := o.importRenameLocal(ctx, importRenameOp{From: "shop_final", To: privDB, Back: true, Marker: importMarker(privDB)}); err != nil {
		t.Fatal(err)
	}
	if err := o.importRenameLocal(ctx, rename); err != nil {
		t.Fatal(err)
	}
	fan := &recFanout{}
	o.Fanout = fan
	o.ServerHost = "s1"
	p := dbImportParams{Database: "shop_final", Nonce: "aabbccddeeff", Servers: []string{"s2"}}
	o.putBackEverywhere(ctx, p, 7)
	if got := finalState(c, "shop_final"); got != "missing" {
		t.Errorf("the final name after the hook: %q", got)
	}
	if got := finalState(c, privDB); got != want {
		t.Errorf("the private name after the hook: %q", got)
	}
	if len(fan.sent) != 1 || !strings.Contains(fan.sent[0], "s2 import.rename back=true from=shop_final to="+privDB) {
		t.Errorf("sent to the other servers: %v", fan.sent)
	}
	if err := o.importDropLocal(ctx, privDB); err != nil || finalState(c, privDB) != "missing" {
		t.Errorf("the drop of the put-back database: %v %q", err, finalState(c, privDB))
	}
	// a database at the final name without the marker of the task is left alone by the hook
	pgExecAll(t, c, "create database shop_final")
	before := finalState(c, "shop_final")
	o.putBackEverywhere(ctx, p, 7)
	if got := finalState(c, "shop_final"); got != before || finalState(c, privDB) != "missing" {
		t.Errorf("the hook touched a database that is not the task's: %q, private %q", got, finalState(c, privDB))
	}
}

// M3: the note of a failed putting back names the database as it is on that server (the final name) and says what happens and what the operator does.
func TestThePutBackNoteNamesTheFinalName(t *testing.T) {
	n := putBackNote([]string{"s2", "s3"}, "shop")
	for _, w := range []string{"s2, s3", "under the final name shop", "tries again", "DROP DATABASE"} {
		if !strings.Contains(n, w) {
			t.Errorf("the note lacks %q: %s", w, n)
		}
	}
	if strings.Contains(n, privatePrefix) {
		t.Errorf("the note names the private database, which is not there: %s", n)
	}
	if putBackNote(nil, "shop") != "" {
		t.Error("a note without a failure")
	}
}
