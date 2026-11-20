# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/186_alter.pl: kingres.alter_distributed_table() (design/shard-phase9.md, increment 9.4, "Feature 3"), the blocking variant.
#
#   A. the cases that change the catalog only: leave the colocation group, join a group with the same placement
#   B. a rebuild: a bucket count that is a multiple of the old (the buckets stay on their nodes), one that is not, another
#      distribution column, a group with another placement; every row, the definition, the names of the partitions and of what is
#      in them, and every row in the partition of its bucket
#   C. the tables of a group together (cascade_to_colocated), with a foreign key between them
#   D. the refusals before anything changes, and a failure that leaves everything as it was

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(make_cluster run_sql prepared_count start_psql finish_psql wait_waiter wait_for_fault_file);

my ($n1, $n2, $n3) = make_cluster('al', 3);
my @nodes = ($n1, $n2, $n3);
sub q1 { my ($node, $sql) = @_; return $node->safe_psql('postgres', $sql); }

$_->safe_psql('postgres', q{CREATE ROLE al_owner; CREATE ROLE al_reader}) for @nodes;

my $helpers = q{
CREATE FUNCTION public.fingerprint(t regclass) RETURNS SETOF text LANGUAGE sql AS $$
  SELECT 'column ' || a.attnum || ' ' || a.attname || ' ' || format_type(a.atttypid, a.atttypmod)
         || ' notnull=' || a.attnotnull || ' identity=' || a.attidentity::text
         || ' default=' || coalesce(pg_get_expr(d.adbin, d.adrelid), '-')
         || ' acl=' || coalesce(a.attacl::text, '-') || ' comment=' || coalesce(col_description(t, a.attnum), '-')
    FROM pg_attribute a LEFT JOIN pg_attrdef d ON d.adrelid = a.attrelid AND d.adnum = a.attnum
   WHERE a.attrelid = t AND a.attnum > 0 AND NOT a.attisdropped
  UNION ALL
  SELECT 'constraint ' || c.conname || ' ' || pg_get_constraintdef(c.oid) FROM pg_constraint c WHERE c.conrelid = t AND c.conparentid = 0
  UNION ALL
  SELECT 'index ' || ic.relname || ' ' || replace(pg_get_indexdef(i.indexrelid), ' ONLY ', ' ') || ' comment=' || coalesce(obj_description(i.indexrelid, 'pg_class'), '-')
    FROM pg_index i JOIN pg_class ic ON ic.oid = i.indexrelid WHERE i.indrelid = t
  UNION ALL
  SELECT 'table owner=' || c.relowner::regrole || ' acl=' || coalesce(c.relacl::text, '-') || ' comment=' || coalesce(obj_description(t, 'pg_class'), '-')
    FROM pg_class c WHERE c.oid = t
$$;
-- the partitions of this node and the rows that are in the wrong one (by the bucket of the key), and the leftovers of other names
CREATE FUNCTION public.local_rows(t regclass, key name) RETURNS TABLE (part text, bucket int, n bigint, wrong bigint)
LANGUAGE plpgsql AS $$
DECLARE r record;
BEGIN
  FOR r IN SELECT c.oid::regclass AS p, (regexp_match(c.relname, '_b([0-9]{4})$'))[1]::int AS b
             FROM pg_inherits i JOIN pg_class c ON c.oid = i.inhrelid WHERE i.inhparent = t ORDER BY c.relname
  LOOP
    part := r.p::text; bucket := r.b;
    EXECUTE format('SELECT count(*), count(*) FILTER (WHERE kingres.bucket_for(%L::regclass, %I) <> %s) FROM ONLY %s', t, key, r.b, r.p) INTO n, wrong;
    RETURN NEXT;
  END LOOP;
END $$;
};
$_->safe_psql('postgres', $helpers) for @nodes;
sub fp { my ($node, $t) = @_; return q1($node, "SELECT line FROM public.fingerprint('$t') line ORDER BY 1"); }

# every row is in the partition of its bucket, on every node; the rows add up
sub layout_ok
{
	my ($label, $t, $key, $rows) = @_;
	my ($wrong, $total, $parts) = (0, 0, 0);
	for my $n (@nodes)
	{
		for my $line (split /\n/, q1($n, "SELECT part || '|' || n || '|' || wrong FROM public.local_rows('$t', '$key')"))
		{
			my ($p, $c, $w) = split /\|/, $line;
			$wrong += $w; $total += $c; $parts++;
		}
	}
	is("$total/$wrong", "$rows/0", "$label: every row is in the partition of its bucket ($parts partitions)");
	return $parts;
}

sub no_leftovers
{
	my ($label) = @_;
	for my $n (@nodes)
	{
		is(q1($n, "SELECT count(*) FROM pg_class WHERE relname LIKE 'kingres\\_shadow%' OR relname LIKE 'kingres\\_old%'"), '0', "$label: no shadow or old table on " . $n->name);
		is(prepared_count($n), '0', "$label: nothing is prepared on " . $n->name);
	}
	is(q1($n1, 'SELECT count(*) FROM kingres.check_cluster()'), '0', "$label: check_cluster() finds nothing");
}

sub dt { my ($t) = @_; return q1($n1, "SELECT colocation_id || '|' || dist_column FROM kingres.dist_table WHERE relid = '$t'::regclass"); }

# ---------------------------------------------------------------------------
# A. the catalog only
# ---------------------------------------------------------------------------
$n1->safe_psql('postgres', q{
	CREATE TABLE public.a1 (id int PRIMARY KEY, v text); CREATE TABLE public.a2 (id int PRIMARY KEY, v text);
	INSERT INTO public.a1 SELECT g, 'a1' FROM generate_series(1, 120) g; INSERT INTO public.a2 SELECT g, 'a2' FROM generate_series(1, 120) g;
	SELECT kingres.create_distributed_table('public.a1', 'id', bucket_count => 6);
	SELECT kingres.create_distributed_table('public.a2', 'id', colocate_with => 'public.a1');
});
{
	my $g = (split /\|/, dt('public.a1'))[0];
	is((split /\|/, dt('public.a2'))[0], $g, 'A: the two tables share a group');
	my $oid = q1($n1, q{SELECT 'public.a2'::regclass::oid});
	my ($rc, $out, $err) = run_sql($n1, "SELECT kingres.alter_distributed_table('public.a2', colocate_with => 'none')");
	is($rc, 0, 'A: colocate_with none') or diag($err);
	isnt((split /\|/, dt('public.a2'))[0], $g, 'A: the table is in a group of its own');
	is((split /\|/, dt('public.a1'))[0], $g, 'A: the other table is where it was');
	is(q1($n1, q{SELECT 'public.a2'::regclass::oid}), $oid, 'A: no table was touched (the same table)');
	is(q1($n1, 'SELECT count(*) FROM public.a2'), '120', 'A: the rows are there');
	is(q1($n2, q{SELECT count(*) FROM kingres.dist_table WHERE colocation_id = (SELECT colocation_id FROM kingres.dist_table WHERE relid = 'public.a2'::regclass)}), '1', 'A: node 2 has the new catalog');
	is(q1($n1, q{SELECT count(*) FROM kingres.bucket_map WHERE colocation_id = (SELECT colocation_id FROM kingres.dist_table WHERE relid = 'public.a2'::regclass)}), '6', 'A: with its six buckets');
	# and back to the group of the first table, whose placement is the same
	($rc, $out, $err) = run_sql($n1, "SELECT kingres.alter_distributed_table('public.a2', colocate_with => 'public.a1')");
	is($rc, 0, 'A: colocate_with a table of the same placement') or diag($err);
	is((split /\|/, dt('public.a2'))[0], $g, 'A: the table is back in the group of the first');
	is(q1($n1, 'SELECT count(*) FROM kingres.colocation'), '1', 'A: the group it left is gone');
	is(q1($n1, q{SELECT 'public.a2'::regclass::oid}), $oid, 'A: and still no table was touched');
	layout_ok('A', 'public.a2', 'id', 120);
	no_leftovers('A');
}

# ---------------------------------------------------------------------------
# B. a rebuild
# ---------------------------------------------------------------------------
$n1->safe_psql('postgres', q{
	CREATE TABLE public.b1 (tenant int NOT NULL, id int NOT NULL, v text DEFAULT 'none', ser serial, ident bigint GENERATED BY DEFAULT AS IDENTITY (START WITH 100 INCREMENT BY 5),
	                        PRIMARY KEY (tenant, id));
	CREATE INDEX b1_v ON public.b1 (v);
	COMMENT ON TABLE public.b1 IS 'the b1'; COMMENT ON INDEX public.b1_v IS 'by value';
	ALTER TABLE public.b1 OWNER TO al_owner; GRANT SELECT ON public.b1 TO al_reader;
	INSERT INTO public.b1 (tenant, id, v) SELECT g % 11, g, 'v' || g FROM generate_series(1, 700) g;
	SELECT kingres.create_distributed_table('public.b1', 'tenant', bucket_count => 6);
});
my $b_sum = q1($n1, q{SELECT count(*) || '|' || sum(id) || '|' || count(DISTINCT ser) FROM public.b1});
my $b_old_map = q1($n1, q{SELECT string_agg(m.bucket || ':' || m.group_id, ',' ORDER BY m.bucket) FROM kingres.bucket_map m JOIN kingres.dist_table d USING (colocation_id) WHERE d.relid = 'public.b1'::regclass});
my $b_fp = fp($n1, 'public.b1');
{
	my $oid = q1($n1, q{SELECT 'public.b1'::regclass::oid});
	my ($rc, $out, $err) = run_sql($n1, "SELECT kingres.alter_distributed_table('public.b1', bucket_count => 12)");
	is($rc, 0, 'B: a bucket count that is a multiple of the old') or diag($err);
	is(q1($n1, q{SELECT count(*) FROM kingres.bucket_map m JOIN kingres.dist_table d USING (colocation_id) WHERE d.relid = 'public.b1'::regclass}), '12', 'B: twelve buckets');
	isnt(q1($n1, q{SELECT 'public.b1'::regclass::oid}), $oid, 'B: it is another table');
	is(q1($n1, 'SELECT count(*) FROM kingres.colocation'), '2', 'B: the group it left is gone (it had no other table)');
	# the buckets stay on their nodes: new bucket b is on the group of old bucket b mod 6
	my %old = map { split /:/ } split /,/, $b_old_map;
	my $new_map = q1($n1, q{SELECT string_agg(m.bucket || ':' || m.group_id, ',' ORDER BY m.bucket) FROM kingres.bucket_map m JOIN kingres.dist_table d USING (colocation_id) WHERE d.relid = 'public.b1'::regclass});
	my %new = map { split /:/ } split /,/, $new_map;
	is(join(',', map { $new{$_} == $old{$_ % 6} ? 'ok' : "bad$_" } sort { $a <=> $b } keys %new), join(',', ('ok') x 12), 'B: every new bucket is where its rows were');
	# the partitions of the new table have the names that a new table would give them (the old ones were renamed out of the way)
	is(q1($n1, q{SELECT count(*) FROM pg_index i JOIN pg_class ic ON ic.oid = i.indexrelid JOIN pg_class c ON c.oid = i.indrelid JOIN pg_inherits h ON h.inhrelid = c.oid
	              WHERE h.inhparent = 'public.b1'::regclass AND ic.relname NOT LIKE c.relname || '\_%'}), '0', 'B: the indexes of the partitions are named after their partitions');
	is(q1($n1, q{SELECT count(*) || '|' || sum(id) || '|' || count(DISTINCT ser) FROM public.b1}), $b_sum, 'B: with every row');
	is(fp($n1, 'public.b1'), $b_fp, 'B: the definition is the same, with the names of the keys and indexes');
	layout_ok('B', 'public.b1', 'tenant', 700);
	no_leftovers('B');
	($rc, $out, $err) = run_sql($n2, "INSERT INTO public.b1 (tenant, id, v) VALUES (1, 5000, 'new') RETURNING ser");
	is($rc, 0, 'B: a node takes a row') or diag($err);
	($rc, $out, $err) = run_sql($n1, "CREATE INDEX b1_extra ON public.b1 (id); ALTER TABLE public.b1 ADD COLUMN extra int");
	is($rc, 0, 'B: DDL on the new table goes to every node') or diag($err);
	is(q1($n3, q{SELECT count(*) FROM pg_attribute WHERE attrelid = 'public.b1'::regclass AND attname = 'extra'}), '1', 'B: node 3 has the column');
	# the buckets stayed on their nodes: node placement is the one of the old buckets
	is(q1($n1, q{SELECT count(*) FROM kingres.bucket_map m JOIN kingres.dist_table d USING (colocation_id) WHERE d.relid = 'public.b1'::regclass AND m.group_id = 0}) > 0 ? 1 : 0, '1', 'B: node 1 has buckets');
	$n1->safe_psql('postgres', 'DROP INDEX public.b1_extra; ALTER TABLE public.b1 DROP COLUMN extra; DELETE FROM public.b1 WHERE id = 5000');
}
{
	$b_sum = q1($n1, q{SELECT count(*) || '|' || sum(id) FROM public.b1});
	my ($rc, $out, $err) = run_sql($n1, "SELECT kingres.alter_distributed_table('public.b1', bucket_count => 7)");
	is($rc, 0, 'B: a bucket count that is not a multiple') or diag($err);
	is(q1($n1, q{SELECT count(*) FROM kingres.bucket_map m JOIN kingres.dist_table d USING (colocation_id) WHERE d.relid = 'public.b1'::regclass}), '7', 'B: seven buckets');
	is(q1($n1, q{SELECT count(*) || '|' || sum(id) FROM public.b1}), $b_sum, 'B: with every row');
	layout_ok('B', 'public.b1', 'tenant', 700);
	no_leftovers('B');
}
{
	# another distribution column
	my ($rc, $out, $err) = run_sql($n1, "SELECT kingres.alter_distributed_table('public.b1', distribution_column => 'id')");
	is($rc, 0, 'B: another distribution column') or diag($err);
	is(dt('public.b1') =~ /\|id$/ ? 1 : 0, '1', 'B: the catalog has it');
	is(q1($n1, q{SELECT count(*) || '|' || sum(id) FROM public.b1}), $b_sum, 'B: with every row');
	layout_ok('B', 'public.b1', 'id', 700);
	is(fp($n1, 'public.b1'), $b_fp, 'B: the definition is the same');
	no_leftovers('B');
	is(q1($n1, q{SELECT count(*) FROM kingres.dist_table WHERE colocation_id = (SELECT colocation_id FROM kingres.dist_table WHERE relid = 'public.b1'::regclass)}), '1', 'B: it is not colocated with anything');
}
{
	# the layout of another group
	$n1->safe_psql('postgres', q{
		CREATE TABLE public.b2 (id int PRIMARY KEY, v text); INSERT INTO public.b2 SELECT g, 'b2' FROM generate_series(1, 60) g;
		SELECT kingres.create_distributed_table('public.b2', 'id', bucket_count => 5);
	});
	my ($rc, $out, $err) = run_sql($n1, "SELECT kingres.alter_distributed_table('public.b1', colocate_with => 'public.b2')");
	is($rc, 0, 'B: colocate with a group of another placement') or diag($err);
	is((split /\|/, dt('public.b1'))[0], (split /\|/, dt('public.b2'))[0], 'B: it is in that group');
	is(q1($n1, q{SELECT count(*) || '|' || sum(id) FROM public.b1}), $b_sum, 'B: with every row');
	layout_ok('B', 'public.b1', 'id', 700);
	no_leftovers('B');
	is(q1($n1, q{SELECT count(*) FROM public.b1 x JOIN public.b2 y ON x.id = y.id}), '60', 'B: a join with the other table works');
}

{
	# an error at any step of a rebuild leaves the table as it was; a writer waits for the locks of the call
	$n1->safe_psql('postgres', q{
		CREATE TABLE public.f1 (id int PRIMARY KEY, v text); INSERT INTO public.f1 SELECT g, 'x' FROM generate_series(1, 80) g;
		SELECT kingres.create_distributed_table('public.f1', 'id', bucket_count => 4);
	});
	my $st = sub { q1($n1, q{SELECT (SELECT count(*) FROM public.f1) || '|' || (SELECT oid FROM pg_class WHERE oid = 'public.f1'::regclass) || '|' || (SELECT colocation_id FROM kingres.dist_table WHERE relid = 'public.f1'::regclass)}) };
	my $f_before = $st->();
	for my $point (qw(alter_after_lock alter_after_copy alter_after_drop alter_after_swap))
	{
		my ($rc, $out, $err) = run_sql($n1, "SET kingres_shard.fault_injection = '$point:error'; SELECT kingres.alter_distributed_table('public.f1', bucket_count => 8)");
		isnt($rc, 0, "B: an error at $point fails the call");
		is($st->(), $f_before, "B: and leaves the table as it was ($point)");
		no_leftovers("B $point");
		layout_ok("B $point", 'public.f1', 'id', 80);
	}
	my $gate = "$PostgreSQL::Test::Utils::tmp_check/gate_f1_$$";
	append_to_file($gate, 'x');
	my $caller = start_psql($n1, "SET kingres_shard.fault_injection = 'alter_after_copy:wait:$gate'; SELECT kingres.alter_distributed_table('public.f1', bucket_count => 8);");
	ok(wait_for_fault_file("$gate.reached", 60), 'B: the call is held after the copy');
	my $writer = start_psql($n1, "INSERT INTO public.f1 VALUES (5000, 'late')");
	ok(wait_waiter($n1, 1), 'B: a writer waits for the locks of the call');
	unlink $gate;
	my ($cout, $cerr) = finish_psql($caller);
	is($cerr, '', 'B: the call goes on') or diag($cerr);
	my ($wout, $werr) = finish_psql($writer);
	is($werr, '', 'B: and so does the writer') or diag($werr);
	is(q1($n1, q{SELECT count(*) || '|' || count(*) FILTER (WHERE id = 5000) FROM public.f1}), '81|1', 'B: the row of the writer is in the new table, none is lost');
	layout_ok('B', 'public.f1', 'id', 81);
}

# ---------------------------------------------------------------------------
# C. the tables of a group together
# ---------------------------------------------------------------------------
$n1->safe_psql('postgres', q{
	CREATE TABLE public.c_p (id int PRIMARY KEY, v text); CREATE TABLE public.c_c (id int PRIMARY KEY, w text);
	INSERT INTO public.c_p SELECT g, 'p' FROM generate_series(1, 150) g; INSERT INTO public.c_c SELECT g, 'c' FROM generate_series(1, 150) g;
	SELECT kingres.create_distributed_table('public.c_p', 'id', bucket_count => 4);
	SELECT kingres.create_distributed_table('public.c_c', 'id', colocate_with => 'public.c_p');
	ALTER TABLE public.c_c ADD CONSTRAINT c_c_p FOREIGN KEY (id) REFERENCES public.c_p (id);
});
{
	my ($rc, $out, $err) = run_sql($n1, "SELECT kingres.alter_distributed_table('public.c_p', bucket_count => 8)");
	my $c_old = q1($n1, q{SELECT string_agg(m.bucket || ':' || m.group_id, ',' ORDER BY m.bucket) FROM kingres.bucket_map m JOIN kingres.dist_table d USING (colocation_id) WHERE d.relid = 'public.c_p'::regclass});
	isnt($rc, 0, 'C: a table with a key to a colocated table is refused alone');
	like($err, qr/foreign_key_colocation:|depends on the colocation/s, 'C: and the error names the key');
	($rc, $out, $err) = run_sql($n1, "SELECT kingres.alter_distributed_table('public.c_p', bucket_count => 8, cascade_to_colocated => true)");
	is($rc, 0, 'C: with cascade_to_colocated both are rebuilt') or diag($err);
	is(dt('public.c_p') =~ /^(\d+)\|/ && $1 eq (split /\|/, dt('public.c_c'))[0] ? 1 : 0, '1', 'C: they are in one group');
	is(q1($n1, q{SELECT bucket_count FROM kingres.colocation WHERE colocation_id = (SELECT colocation_id FROM kingres.dist_table WHERE relid = 'public.c_p'::regclass)}), '8', 'C: of eight buckets');
	is(q1($n1, q{SELECT (SELECT count(*) FROM public.c_p) || '|' || (SELECT count(*) FROM public.c_c)}), '150|150', 'C: every row of both');
	{
		# four buckets over three groups, eight buckets: bucket b is on the group of old bucket b mod 4 (round-robin would give b mod 3)
		my %o = map { split /:/ } split /,/, $c_old;
		my %n = map { split /:/ } split /,/, q1($n1, q{SELECT string_agg(m.bucket || ':' || m.group_id, ',' ORDER BY m.bucket) FROM kingres.bucket_map m JOIN kingres.dist_table d USING (colocation_id) WHERE d.relid = 'public.c_p'::regclass});
		is(join(',', map { $n{$_} == $o{$_ % 4} ? 'ok' : "bad$_" } sort { $a <=> $b } keys %n), join(',', ('ok') x 8), 'C: every new bucket is where its rows were');
	}
	layout_ok('C', 'public.c_p', 'id', 150);
	layout_ok('C', 'public.c_c', 'id', 150);
	is(q1($n1, q{SELECT count(*) FROM pg_constraint WHERE conname = 'c_c_p' AND conparentid = 0 AND convalidated}), '1', 'C: the key between them is there, valid');
	($rc, $out, $err) = run_sql($n1, "INSERT INTO public.c_c VALUES (9999, 'x')");
	isnt($rc, 0, 'C: and it still refuses a row without its parent');
	no_leftovers('C');
}

# ---------------------------------------------------------------------------
# D. refusals and a failure
# ---------------------------------------------------------------------------
$n1->safe_psql('postgres', q{
	CREATE TABLE public.d1 (id int PRIMARY KEY, other int, v text);
	INSERT INTO public.d1 SELECT g, g, 'x' FROM generate_series(1, 90) g;
	SELECT kingres.create_distributed_table('public.d1', 'id', bucket_count => 6);
	CREATE TABLE public.d_plain (id int PRIMARY KEY);
});
sub d_state { return q1($n1, q{SELECT (SELECT count(*) FROM public.d1) || '|' || (SELECT oid FROM pg_class WHERE oid = 'public.d1'::regclass) || '|' || (SELECT colocation_id FROM kingres.dist_table WHERE relid = 'public.d1'::regclass)}); }
my $d_before = d_state();
{
	my ($rc, $out, $err) = run_sql($n1, "SELECT kingres.alter_distributed_table('public.d_plain', bucket_count => 3)");
	isnt($rc, 0, 'D: an ordinary table is refused'); like($err, qr/not_distributed:|not a distributed table/s, 'D: and the error says so');
	($rc, $out, $err) = run_sql($n1, "SELECT kingres.alter_distributed_table('public.d1', distribution_column => 'nocolumn')");
	isnt($rc, 0, 'D: a column that does not exist is refused'); like($err, qr/column_missing:|does not exist/s, 'D: and the error says so');
	($rc, $out, $err) = run_sql($n1, "SELECT kingres.alter_distributed_table('public.d1', distribution_column => 'other')");
	isnt($rc, 0, 'D: a column that the key does not include is refused'); like($err, qr/unique_without_distribution_column:|does not include/s, 'D: and the error says so');
	($rc, $out, $err) = run_sql($n1, "SELECT kingres.alter_distributed_table('public.d1', bucket_count => 5000)");
	isnt($rc, 0, 'D: a bucket count that is too big is refused');
	($rc, $out, $err) = run_sql($n2, "SELECT kingres.alter_distributed_table('public.d1', bucket_count => 3)");
	isnt($rc, 0, 'D: a node that is not the map group primary refuses');
	is(d_state(), $d_before, 'D: nothing changed');
	# the same request as what is there: nothing happens
	($rc, $out, $err) = run_sql($n1, "SELECT kingres.alter_distributed_table('public.d1', bucket_count => 6, distribution_column => 'id')");
	is($rc, 0, 'D: a request for what is there is accepted'); is(d_state(), $d_before, 'D: and changes nothing');
	# a failure at the commit
	($rc, $out, $err) = run_sql($n1, "SET kingres_shard.fault_injection = 'before_prepare:error'; SELECT kingres.alter_distributed_table('public.d1', bucket_count => 12)");
	isnt($rc, 0, 'D: a failure before the prepare fails the call');
	is(d_state(), $d_before, 'D: the table is as it was');
	no_leftovers('D');
	layout_ok('D', 'public.d1', 'id', 90);
	($rc, $out, $err) = run_sql($n1, "SELECT kingres.alter_distributed_table('public.d1', bucket_count => 12)");
	is($rc, 0, 'D: the next call succeeds') or diag($err);
	is(q1($n1, 'SELECT count(*) FROM public.d1'), '90', 'D: with every row');
}

done_testing();
