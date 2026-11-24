// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package ops

import (
	"context"
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
	if err := c.QueryRow(ctx, `select pg_get_userbyid(datdba) from pg_database where datname = 'shop_final'`).Scan(&owner); err != nil || owner != "imp" {
		t.Errorf("owner after the rename: %q (%v)", owner, err)
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

// L1: a failure after the rename puts the private name, the owner, the marker and the closed door back.
func TestRenameUndoesItselfOnAFailure(t *testing.T) {
	in, c, o := newGuardInstance(t)
	makePrivate(t, o, c)
	ctx := context.Background()
	err := o.importRenameLocal(ctx, importRenameOp{From: privDB, To: "shop_final", Owner: "no_such_role"})
	if err == nil {
		t.Fatal("the rename succeeded for a role that does not exist")
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
		"create index on public.t_idx ((v operator(public.===) v))", "reset role")
	for table, ok := range map[string]bool{"t_ok": true, "t_part": true, "t_op": false, "t_dom": false, "t_dom2": false, "t_stat": false, "t_idx": false} {
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
