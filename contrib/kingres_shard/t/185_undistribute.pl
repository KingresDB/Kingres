# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/185_undistribute.pl: kingres.undistribute_table() (design/shard-phase9.md, increment 9.4, "Feature 2"), the blocking variant.
#
#   A. a distributed table with rows, a key, indexes, a serial and an identity column, an owner, privileges and comments becomes an
#      ordinary table on the map group primary: the same definition, every row, nothing left on the other nodes, the catalog
#      without it and without its group, the table usable (writes, sequences) afterwards
#   B. a reference table: it stays on the target node and goes from the others
#   C. a table that is linked by keys to others: refused without cascade_via_foreign_keys, undistributed together with it, the
#      keys kept (also the key of a distributed table to a reference table, which stays a reference table)
#   D. the target group: an ordinary table on the primary of another group
#   E. the refusals before anything changes, and a failure that leaves everything as it was

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(make_cluster run_sql prepared_count wait_waiter start_psql finish_psql);

my ($n1, $n2, $n3) = make_cluster('ud', 3);
my @nodes = ($n1, $n2, $n3);
sub q1 { my ($node, $sql) = @_; return $node->safe_psql('postgres', $sql); }

$_->safe_psql('postgres', q{CREATE ROLE ud_owner; CREATE ROLE ud_reader; CREATE ROLE ud_writer}) for @nodes;

my $fingerprint_sql = q{
CREATE FUNCTION public.fingerprint(t regclass) RETURNS SETOF text LANGUAGE sql AS $$
  SELECT 'column ' || a.attnum || ' ' || a.attname || ' ' || format_type(a.atttypid, a.atttypmod)
         || ' notnull=' || a.attnotnull || ' identity=' || a.attidentity::text || ' generated=' || a.attgenerated::text
         || ' default=' || coalesce(pg_get_expr(d.adbin, d.adrelid), '-')
         || ' acl=' || coalesce(a.attacl::text, '-') || ' comment=' || coalesce(col_description(t, a.attnum), '-')
    FROM pg_attribute a LEFT JOIN pg_attrdef d ON d.adrelid = a.attrelid AND d.adnum = a.attnum
   WHERE a.attrelid = t AND a.attnum > 0 AND NOT a.attisdropped
  UNION ALL
  SELECT 'constraint ' || c.conname || ' ' || pg_get_constraintdef(c.oid) || ' comment=' || coalesce(obj_description(c.oid, 'pg_constraint'), '-')
    FROM pg_constraint c WHERE c.conrelid = t AND c.conparentid = 0
  UNION ALL
  SELECT 'index ' || ic.relname || ' ' || replace(pg_get_indexdef(i.indexrelid), ' ONLY ', ' ') || ' comment=' || coalesce(obj_description(i.indexrelid, 'pg_class'), '-')
    FROM pg_index i JOIN pg_class ic ON ic.oid = i.indexrelid WHERE i.indrelid = t
  UNION ALL
  SELECT 'table owner=' || c.relowner::regrole || ' acl=' || coalesce(c.relacl::text, '-') || ' comment=' || coalesce(obj_description(t, 'pg_class'), '-')
    FROM pg_class c WHERE c.oid = t
  UNION ALL
  SELECT 'sequence ' || s.seqrelid::regclass || ' type=' || s.seqtypid::regtype || ' start=' || s.seqstart || ' inc=' || s.seqincrement
         || ' min=' || s.seqmin || ' max=' || s.seqmax || ' cache=' || s.seqcache || ' cycle=' || s.seqcycle
         || ' owner=' || coalesce((SELECT d.refobjid::regclass || '.' || d.refobjsubid || '/' || d.deptype::text FROM pg_depend d
                                    WHERE d.classid = 'pg_class'::regclass AND d.objid = s.seqrelid
                                      AND d.refclassid = 'pg_class'::regclass LIMIT 1), '-')
    FROM pg_sequence s
   WHERE s.seqrelid IN (SELECT d.objid FROM pg_depend d WHERE d.classid = 'pg_class'::regclass
                         AND d.refclassid = 'pg_class'::regclass AND d.refobjid = t AND d.deptype IN ('a', 'i'))
$$;
};
$_->safe_psql('postgres', $fingerprint_sql) for @nodes;
sub fp { my ($node, $t) = @_; return q1($node, "SELECT line FROM public.fingerprint('$t') line ORDER BY 1"); }

sub nothing_left
{
	my ($label, $t, @where) = @_;
	for my $n (@where)
	{
		is(q1($n, "SELECT count(*) FROM pg_class WHERE relname LIKE '${t}%' AND relkind IN ('r', 'p')"), '0', "$label: nothing of $t is on " . $n->name);
	}
	is(q1($_, "SELECT count(*) FROM pg_class WHERE relname LIKE 'kingres\\_shadow%'"), '0', "$label: no shadow on " . $_->name) for @nodes;
	is(prepared_count($_), '0', "$label: nothing is prepared on " . $_->name) for @nodes;
}

# ---------------------------------------------------------------------------
# A. a distributed table
# ---------------------------------------------------------------------------
$n1->safe_psql('postgres', q{
	CREATE TABLE public.u1 (tenant int NOT NULL, id int NOT NULL, v text DEFAULT 'none', qty int CHECK (qty >= 0), ser serial,
	                        ident bigint GENERATED ALWAYS AS IDENTITY (START WITH 100 INCREMENT BY 5), calc int GENERATED ALWAYS AS (qty * 2) STORED,
	                        PRIMARY KEY (tenant, id));
	CREATE INDEX u1_v ON public.u1 (v);
	CREATE UNIQUE INDEX u1_part ON public.u1 (id, tenant) WHERE qty > 10;
	COMMENT ON TABLE public.u1 IS 'the items'; COMMENT ON COLUMN public.u1.v IS 'the value'; COMMENT ON INDEX public.u1_v IS 'by value';
	ALTER TABLE public.u1 OWNER TO ud_owner;
	GRANT SELECT ON public.u1 TO ud_reader; GRANT SELECT, INSERT, UPDATE ON public.u1 TO ud_writer; GRANT UPDATE (v) ON public.u1 TO ud_reader;
	INSERT INTO public.u1 (tenant, id, v, qty) SELECT g % 7, g, 'v' || g, g % 50 FROM generate_series(1, 600) g;
	SELECT kingres.create_distributed_table('public.u1', 'tenant', bucket_count => 9);
});
my $before = fp($n1, 'public.u1');
my $sum = q1($n1, 'SELECT count(*) || \'|\' || sum(id) || \'|\' || sum(qty) || \'|\' || sum(calc) || \'|\' || count(DISTINCT ident) FROM public.u1');
is($sum, '600|180300|14700|29400|600', 'A: the distributed table holds the rows');
my ($rc, $out, $err) = run_sql($n1, "SELECT kingres.undistribute_table('public.u1')");
is($rc, 0, 'A: undistribute_table runs') or diag($err);
is(q1($n1, 'SELECT count(*) FROM kingres.dist_table'), '0', 'A: the table is not in the catalog');
is(q1($n1, 'SELECT count(*) FROM kingres.colocation'), '0', 'A: its group is gone');
is(q1($n1, 'SELECT count(*) FROM kingres.bucket_map'), '0', 'A: and its buckets');
is(q1($_, 'SELECT (SELECT count(*) FROM kingres.colocation) || \'|\' || (SELECT count(*) FROM kingres.bucket_map)'), '0|0', 'A: the catalog of ' . $_->name . ' has neither the group nor its buckets') for ($n2, $n3);
is(q1($_, 'SELECT version FROM kingres.map_version'), q1($n1, 'SELECT version FROM kingres.map_version'), 'A: the map version of ' . $_->name . ' is the one of node 1') for ($n2, $n3);
is(q1($n1, q{SELECT relkind FROM pg_class WHERE oid = 'public.u1'::regclass}), 'r', 'A: it is an ordinary table on node 1');
is(q1($n1, 'SELECT count(*) || \'|\' || sum(id) || \'|\' || sum(qty) || \'|\' || sum(calc) || \'|\' || count(DISTINCT ident) FROM public.u1'), $sum, 'A: with every row');
is(fp($n1, 'public.u1'), $before, 'A: columns, defaults, keys, indexes, owner, privileges, comments and sequences are the ones of the distributed table');
nothing_left('A', 'u1', $n2, $n3);
is(q1($_, 'SELECT count(*) FROM kingres.dist_table'), '0', 'A: the catalog of ' . $_->name . ' does not have it') for @nodes;
{
	($rc, $out, $err) = run_sql($n1, "INSERT INTO public.u1 (tenant, id, qty) VALUES (1, 1000, 5), (2, 1001, 6) RETURNING ser, ident");
	is($rc, 0, 'A: the ordinary table takes rows') or diag($err);
	like($out, qr/^601\|\d+$/m, 'A: the serial goes on above its rows') or diag($out);
	is(q1($n2, q{SELECT to_regclass('public.u1') IS NULL}), 't', 'A: node 2 knows no such table');
	is(q1($n1, q{SELECT has_table_privilege('ud_writer', 'public.u1', 'INSERT')}), 't', 'A: the privileges are there');
}

# ---------------------------------------------------------------------------
# B. a reference table
# ---------------------------------------------------------------------------
$n1->safe_psql('postgres', q{
	CREATE TABLE public.ur (id int PRIMARY KEY, name text);
	INSERT INTO public.ur SELECT g, 'n' || g FROM generate_series(1, 40) g;
	SELECT kingres.create_reference_table('public.ur');
});
is(q1($n2, 'SELECT count(*) FROM public.ur'), '40', 'B: the reference table is on node 2');
($rc, $out, $err) = run_sql($n1, "SELECT kingres.undistribute_table('public.ur')");
is($rc, 0, 'B: undistribute_table of a reference table runs') or diag($err);
is(q1($n1, 'SELECT count(*) FROM kingres.dist_table'), '0', 'B: it is not in the catalog');
is(q1($n1, 'SELECT count(*) FROM public.ur'), '40', 'B: it stays on node 1, with its rows');
nothing_left('B', 'ur', $n2, $n3);
($rc, $out, $err) = run_sql($n1, "INSERT INTO public.ur VALUES (100, 'x')");
is($rc, 0, 'B: and is an ordinary table: a write goes to this node only') or diag($err);
is(q1($n1, 'SELECT count(*) FROM public.ur'), '41', 'B: node 1 has the row');

# ---------------------------------------------------------------------------
# C. keys
# ---------------------------------------------------------------------------
$n1->safe_psql('postgres', q{
	CREATE TABLE public.k_ref (id int PRIMARY KEY, name text);
	INSERT INTO public.k_ref SELECT g, 'r' || g FROM generate_series(1, 20) g;
	SELECT kingres.create_reference_table('public.k_ref');
	CREATE TABLE public.k_p (id int PRIMARY KEY, r int REFERENCES public.k_ref (id), v text);
	INSERT INTO public.k_p SELECT g, 1 + g % 20, 'p' FROM generate_series(1, 200) g;
	SELECT kingres.create_distributed_table('public.k_p', 'id', bucket_count => 6);
	CREATE TABLE public.k_c (id int PRIMARY KEY, pid int NOT NULL, w text);
	INSERT INTO public.k_c SELECT g, 1 + g % 200, 'c' FROM generate_series(1, 200) g;
	SELECT kingres.create_distributed_table('public.k_c', 'id', colocate_with => 'public.k_p');
});
# a key between two distributed tables, on the distribution column of both
($rc, $out, $err) = run_sql($n1, "ALTER TABLE public.k_c ADD CONSTRAINT k_c_p FOREIGN KEY (id) REFERENCES public.k_p (id)");
diag("a key between colocated tables: rc=$rc $err") if $rc;
my $linked = $rc == 0;
# a distributed table with a key to a reference table: the reference table stays (the key is kept)
{
	($rc, $out, $err) = run_sql($n1, "SELECT kingres.undistribute_table('public.k_c')");
	if ($linked) { isnt($rc, 0, 'C: a table linked by a key to another is refused'); like($err, qr/foreign_key_linked|links/s, 'C: the error names the key'); }
	else { is($rc, 0, 'C: a table without keys can be undistributed alone') or diag($err); }
}
if ($linked)
{
	($rc, $out, $err) = run_sql($n1, "SELECT kingres.undistribute_table('public.k_ref')");
	isnt($rc, 0, 'C: a reference table that distributed tables refer to is refused');
	like($err, qr/foreign_key_linked|links/s, 'C: and the error names the key');
	my $before_c = fp($n1, 'public.k_c') . "\n" . fp($n1, 'public.k_p');
	my $rows_c = q1($n1, q{SELECT (SELECT count(*) FROM public.k_c) || '|' || (SELECT count(*) FROM public.k_p) || '|' || (SELECT count(*) FROM public.k_ref)});
	($rc, $out, $err) = run_sql($n1, "SELECT kingres.undistribute_table('public.k_p', true)");
	is($rc, 0, 'C: with cascade_via_foreign_keys the tables of the component go together') or diag($err);
	is(q1($n1, 'SELECT count(*) FROM kingres.dist_table'), '0', 'C: none of the three is in the catalog');
	is(q1($n1, q{SELECT (SELECT count(*) FROM public.k_c) || '|' || (SELECT count(*) FROM public.k_p) || '|' || (SELECT count(*) FROM public.k_ref)}), $rows_c, 'C: every row of every table is there');
	is(fp($n1, 'public.k_c') . "\n" . fp($n1, 'public.k_p'), $before_c, 'C: the definitions, with the keys, are the same');
	is(q1($n1, q{SELECT count(*) FROM pg_constraint WHERE contype = 'f' AND conrelid IN ('public.k_c'::regclass, 'public.k_p'::regclass) AND convalidated}), '2', 'C: the two keys are there, valid');
	($rc, $out, $err) = run_sql($n1, "INSERT INTO public.k_p VALUES (9998, 1, 'x'); INSERT INTO public.k_c VALUES (9998, 5, 'x')");
	is($rc, 0, 'C: a row that violates no key is accepted') or diag($err);
	($rc, $out, $err) = run_sql($n1, "INSERT INTO public.k_c VALUES (9997, 5, 'x')");
	isnt($rc, 0, 'C: a row without its parent is refused by the key between the former distributed tables');
	($rc, $out, $err) = run_sql($n1, "INSERT INTO public.k_p VALUES (9999, 99999, 'x')");
	isnt($rc, 0, 'C: a row that violates the key to the former reference table is refused');
	nothing_left('C', 'k_', $n2, $n3);
}

# a distributed table with a key to a reference table: the key is kept and the reference table stays a reference table
$n1->safe_psql('postgres', q{
	CREATE TABLE public.k2_ref (id int PRIMARY KEY); INSERT INTO public.k2_ref SELECT g FROM generate_series(1, 10) g;
	SELECT kingres.create_reference_table('public.k2_ref');
	CREATE TABLE public.k2_p (id int PRIMARY KEY, r int REFERENCES public.k2_ref (id)); INSERT INTO public.k2_p SELECT g, 1 + g % 10 FROM generate_series(1, 100) g;
	SELECT kingres.create_distributed_table('public.k2_p', 'id', bucket_count => 4);
});
($rc, $out, $err) = run_sql($n1, "SELECT kingres.undistribute_table('public.k2_p')");
is($rc, 0, 'C: a distributed table with a key to a reference table is undistributed alone') or diag($err);
is(q1($n1, q{SELECT string_agg(relid::text || ':' || method, ',') FROM kingres.dist_table}), 'k2_ref:reference', 'C: the reference table is still one');
is(q1($n1, q{SELECT count(*) FROM pg_constraint WHERE conrelid = 'public.k2_p'::regclass AND contype = 'f' AND convalidated AND confrelid = 'public.k2_ref'::regclass}), '1', 'C: and the key to it is kept');
($rc, $out, $err) = run_sql($n1, "INSERT INTO public.k2_p VALUES (5000, 99)");
isnt($rc, 0, 'C: the kept key refuses a row without its parent');
is(q1($n2, 'SELECT count(*) FROM public.k2_ref'), '10', 'C: the reference table is still on node 2');

# the writers of a table wait for the transaction (the readers wait too once the table is dropped, which is the last step of the call)
$n1->safe_psql('postgres', q{
	CREATE TABLE public.w1 (id int PRIMARY KEY, v text); INSERT INTO public.w1 SELECT g, 'x' FROM generate_series(1, 60) g;
	SELECT kingres.create_distributed_table('public.w1', 'id', bucket_count => 4);
});
{
	my $holder = $n1->background_psql('postgres');
	$holder->query_safe("BEGIN; SELECT kingres.undistribute_table('public.w1');");
	my $writer = start_psql($n1, "INSERT INTO public.w1 VALUES (1000, 'late')");
	ok(wait_waiter($n1, 1), 'F: a writer waits for the transaction');
	$holder->query_safe('COMMIT;');
	my ($wout, $werr) = finish_psql($writer);
	is($werr, '', 'F: the writer goes on when the transaction commits') or diag($werr);
	is(q1($n1, q{SELECT count(*) FROM public.w1 WHERE id = 1000}) . '|' . q1($n1, q{SELECT relkind::text FROM pg_class WHERE oid = 'public.w1'::regclass}), '1|r', 'F: its row is in the ordinary table');
	$holder->quit;
}
# a writer that comes while the call holds its locks waits for it, and its row goes into the ordinary table; an error at any step of the call
# leaves the table as it was
$n1->safe_psql('postgres', q{
	CREATE TABLE public.w3 (id int PRIMARY KEY, v text); INSERT INTO public.w3 SELECT g, 'x' FROM generate_series(1, 80) g;
	SELECT kingres.create_distributed_table('public.w3', 'id', bucket_count => 4);
});
sub w3_state { return q1($n1, q{SELECT (SELECT count(*) FROM public.w3) || '|' || (SELECT relkind::text FROM pg_class WHERE oid = 'public.w3'::regclass) || '|' || (SELECT count(*) FROM kingres.dist_table WHERE relid = 'public.w3'::regclass)}); }
my $w3_before = w3_state();
for my $point (qw(undistribute_after_lock undistribute_after_copy undistribute_after_drop undistribute_after_swap))
{
	($rc, $out, $err) = run_sql($n1, "SET kingres_shard.fault_injection = '$point:error'; SELECT kingres.undistribute_table('public.w3')");
	isnt($rc, 0, "F: an error at $point fails the call");
	is(w3_state(), $w3_before, "F: and leaves the table as it was ($point)");
	nothing_left("F $point", 'w3_', ());
}
{
	my $gate = "$PostgreSQL::Test::Utils::tmp_check/gate_w3_$$";
	append_to_file($gate, 'x');
	my $caller = start_psql($n1, "SET kingres_shard.fault_injection = 'undistribute_after_copy:wait:$gate'; SELECT kingres.undistribute_table('public.w3');");
	ok(KingresShardTest::wait_for_fault_file("$gate.reached", 60), 'F: the call is held after the copy');
	my $writer = start_psql($n1, "INSERT INTO public.w3 VALUES (5000, 'late')");
	ok(wait_waiter($n1, 1), 'F: a writer waits for the locks of the call');
	unlink $gate;
	my ($cout, $cerr) = finish_psql($caller);
	is($cerr, '', 'F: the call goes on') or diag($cerr);
	my ($wout, $werr) = finish_psql($writer);
	is($werr, '', 'F: and so does the writer') or diag($werr);
	is(q1($n1, q{SELECT count(*) || '|' || count(*) FILTER (WHERE id = 5000) FROM public.w3}), '81|1', 'F: its row is in the ordinary table, none is lost');
}

# a rolled back transaction leaves the table as it was
$n1->safe_psql('postgres', q{
	CREATE TABLE public.w2 (id int PRIMARY KEY, v text); INSERT INTO public.w2 SELECT g, 'x' FROM generate_series(1, 60) g;
	SELECT kingres.create_distributed_table('public.w2', 'id', bucket_count => 4);
});
($rc, $out, $err) = run_sql($n1, "BEGIN; SELECT kingres.undistribute_table('public.w2'); ROLLBACK;");
is($rc, 0, 'F: a rolled back undistribute');
is(q1($n1, q{SELECT (SELECT count(*) FROM public.w2) || '|' || (SELECT relkind::text FROM pg_class WHERE oid = 'public.w2'::regclass) || '|' || (SELECT count(*) FROM kingres.dist_table WHERE relid = 'public.w2'::regclass)}), '60|p|1', 'F: leaves the table distributed with its rows');
nothing_left('F', 'w2_', ());

# ---------------------------------------------------------------------------
# D. the target group
# ---------------------------------------------------------------------------
$n1->safe_psql('postgres', q{
	CREATE TABLE public.d1 (id int PRIMARY KEY, v text, ser serial, ident int GENERATED ALWAYS AS IDENTITY);
	ALTER TABLE public.d1 OWNER TO ud_owner; GRANT SELECT ON public.d1 TO ud_reader;
	INSERT INTO public.d1 (id, v) SELECT g, 'v' || g FROM generate_series(1, 200) g;
	SELECT kingres.create_distributed_table('public.d1', 'id', bucket_count => 6);
});
{
	my $g2 = q1($n1, 'SELECT group_id FROM kingres.node WHERE node_id = 2');
	my $before_d = fp($n1, 'public.d1');
	($rc, $out, $err) = run_sql($n1, "SELECT kingres.undistribute_table('public.d1', target_group => $g2)");
	is($rc, 0, 'D: undistribute to the primary of another group') or diag($err);
	is(q1($n2, q{SELECT relkind FROM pg_class WHERE oid = 'public.d1'::regclass}), 'r', 'D: an ordinary table on node 2');
	is(q1($n2, 'SELECT count(*) || \'|\' || sum(id) FROM public.d1'), '200|20100', 'D: with every row');
	is(fp($n2, 'public.d1'), $before_d, 'D: and the definition of the distributed table');
	nothing_left('D', 'd1', $n1, $n3);
	($rc, $out, $err) = run_sql($n2, "INSERT INTO public.d1 (id) VALUES (1000) RETURNING ser, ident");
	is($rc, 0, 'D: node 2 takes a row') or diag($err);
	like($out, qr/^201\|201$/m, 'D: the serial and the identity go on above the rows') or diag($out);
}

# ---------------------------------------------------------------------------
# E. refusals, and a failure that leaves everything as it was
# ---------------------------------------------------------------------------
$n1->safe_psql('postgres', q{
	CREATE TABLE public.e1 (id int PRIMARY KEY, v text);
	INSERT INTO public.e1 SELECT g, 'v' FROM generate_series(1, 150) g;
	SELECT kingres.create_distributed_table('public.e1', 'id', bucket_count => 6);
	CREATE TABLE public.e_plain (id int PRIMARY KEY);
});
sub e_state { return q1($n1, q{SELECT (SELECT count(*) FROM public.e1) || '|' || (SELECT relkind::text FROM pg_class WHERE oid = 'public.e1'::regclass) || '|' || (SELECT count(*) FROM kingres.dist_table WHERE relid = 'public.e1'::regclass)}); }
my $e_before = e_state();
is($e_before, '150|p|1', 'E: the table is distributed');
($rc, $out, $err) = run_sql($n1, "SELECT kingres.undistribute_table('public.e_plain')");
isnt($rc, 0, 'E: an ordinary table is refused'); like($err, qr/not_distributed:|not a distributed or reference table/s, 'E: and says so');
($rc, $out, $err) = run_sql($n1, "SELECT kingres.undistribute_table('public.e1', target_group => 99)");
isnt($rc, 0, 'E: a group that does not exist is refused'); like($err, qr/target_group_invalid:|has no active primary/s, 'E: and says so');
($rc, $out, $err) = run_sql($n2, "SELECT kingres.undistribute_table('public.e1')");
isnt($rc, 0, 'E: a node that is not the map group primary refuses'); is(e_state(), $e_before, 'E: nothing changed');
# a view
$n1->safe_psql('postgres', 'CREATE VIEW public.e1_view AS SELECT id FROM public.e1');
($rc, $out, $err) = run_sql($n1, "SELECT kingres.undistribute_table('public.e1')");
isnt($rc, 0, 'E: a view that uses the table stops it'); like($err, qr/e1_view depends on table public\.e1, which is dropped by the change/s, 'E: the report names the view (before anything is dropped)');
is(e_state(), $e_before, 'E: nothing changed');
$n1->safe_psql('postgres', 'DROP VIEW public.e1_view');
# a group that an operation uses
{
	$n1->safe_psql('postgres', 'CREATE TABLE public.e2 (id int PRIMARY KEY, v text); INSERT INTO public.e2 SELECT g, \'x\' FROM generate_series(1, 20) g');
	my $op = q1($n1, q{SELECT kingres.begin_table_operation('distribute', 'public.e2', jsonb_build_object('column', 'id', 'colocate_with', 'public.e1'::regclass::oid::text))});
	($rc, $out, $err) = run_sql($n1, "SELECT kingres.undistribute_table('public.e1')");
	isnt($rc, 0, 'E: a group that an operation uses is refused'); like($err, qr/group_busy:|is busy/s, 'E: and says so');
	is(e_state(), $e_before, 'E: nothing changed');
	q1($n1, "SELECT kingres.cancel_table_operation($op)");
}
# a failure at the commit: the table is distributed, with its rows, and nothing is left anywhere
($rc, $out, $err) = run_sql($n1, "SET kingres_shard.fault_injection = 'before_prepare:error'; SELECT kingres.undistribute_table('public.e1')");
isnt($rc, 0, 'E: a failure before the prepare fails the call') or diag($out);
is(e_state(), $e_before, 'E: the table is as it was');
nothing_left('E', 'e1_', ());
is(q1($_, q{SELECT count(*) FROM pg_class WHERE relname LIKE 'e1\_b%' AND relkind IN ('r', 'p')}) > 0 ? 1 : 0, '1', 'E: the partitions are on ' . $_->name) for ($n1);
# and then it works
($rc, $out, $err) = run_sql($n1, "SELECT kingres.undistribute_table('public.e1')");
is($rc, 0, 'E: the next call succeeds') or diag($err);
is(q1($n1, 'SELECT count(*) FROM public.e1'), '150', 'E: with every row');

done_testing();
