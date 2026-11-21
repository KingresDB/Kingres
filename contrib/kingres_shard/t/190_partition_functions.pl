# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/190_partition_functions.pl: create_partition(), drop_partition(), detach_partition() and attach_partition() (design/shard-phase9.md,
# increment 9.5, feature 4).
#
#   A. create_partition: the leaves on every node, the catalog on every node, writes from another node, the refusals, a busy group
#   B. drop_partition and detach_partition and attach_partition: no row moves, the catalog follows
#   C. an error at each fault point leaves nothing; a writer on another node races a held operation

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(make_cluster run_sql prepared_count wait_for_fault_file start_psql finish_psql);

my ($n1, $n2, $n3) = make_cluster('pf', 3);
my @nodes = ($n1, $n2, $n3);
sub q1 { my ($node, $sql) = @_; return $node->safe_psql('postgres', $sql); }

$_->safe_psql('postgres', q{CREATE FUNCTION public.leaf_rows(p text) RETURNS bigint LANGUAGE plpgsql AS $$ DECLARE r record; n bigint := 0; c bigint; BEGIN
	FOR r IN SELECT c.oid::regclass AS l FROM pg_class c WHERE c.relname ~ ('^' || p || '_b[0-9]{4}$') AND c.relkind = 'r' LOOP EXECUTE format('SELECT count(*) FROM %s', r.l) INTO c; n := n + c; END LOOP; RETURN n; END $$;
	CREATE FUNCTION public.leaf_count(p text) RETURNS bigint LANGUAGE sql AS $$ SELECT count(*) FROM pg_class c WHERE c.relname ~ ('^' || p || '_b[0-9]{4}$') AND c.relkind = 'r' $$}) for @nodes;
sub leaves { my ($p) = @_; my $s = 0; $s += q1($_, "SELECT public.leaf_count('$p')") for @nodes; return $s; }
sub rows_in { my ($p) = @_; my $s = 0; $s += q1($_, "SELECT public.leaf_rows('$p')") for @nodes; return $s; }

sub new_table
{
	my ($t, $default) = @_;
	$n1->safe_psql('postgres', qq{
		CREATE TABLE public.$t (customer int NOT NULL, created date NOT NULL, id int NOT NULL, qty int, PRIMARY KEY (customer, created, id)) PARTITION BY RANGE (created);
		CREATE TABLE public.${t}_p1 PARTITION OF public.$t FOR VALUES FROM ('2026-01-01') TO ('2026-02-01');
		CREATE TABLE public.${t}_p2 PARTITION OF public.$t FOR VALUES FROM ('2026-02-01') TO ('2026-03-01');
	} . ($default ? qq{CREATE TABLE public.${t}_def PARTITION OF public.$t DEFAULT;} : '') . qq{
		INSERT INTO public.$t SELECT g % 11, date '2026-01-01' + (g % 55), g, g % 40 FROM generate_series(1, 400) g;
		SELECT kingres.create_distributed_table('public.$t', 'customer', bucket_count => 6);
	});
}
sub twin { my ($t) = @_; return q1($n1, "SELECT count(*) || '|' || coalesce(sum(qty), 0) FROM public.$t"); }

# ---------------------------------------------------------------------------
# A. create_partition
# ---------------------------------------------------------------------------
new_table('o1', 0);
$_->safe_psql('postgres', q{CREATE ROLE pf_owner}) for @nodes;
$n1->safe_psql('postgres', q{ALTER TABLE public.o1 OWNER TO pf_owner});
my ($rc, $out, $err) = run_sql($n1, q{SELECT kingres.create_partition('public.o1', 'o1_p3', 'FROM (''2026-03-01'') TO (''2026-04-01'')')});
is($rc, 0, 'A: create_partition') or diag($err);
is(leaves('o1_p3'), '6', 'A: six leaves over the nodes');
for my $n (@nodes)
{
	is(q1($n, q{SELECT count(*) FROM kingres.dist_partition WHERE parent = 'public.o1'::regclass AND partition_name = 'o1_p3' AND bound = 'FROM (''2026-03-01'') TO (''2026-04-01'')'}), '1', 'A: the catalog of ' . $n->name . ' has it');
	is(q1($n, q{SELECT count(*) FROM pg_class c JOIN pg_inherits i ON i.inhrelid = c.oid JOIN pg_class p ON p.oid = i.inhparent WHERE c.relname ~ '^o1_p3_b[0-9]{4}$' AND p.relname ~ '^o1_b[0-9]{4}$'}),
	   q1($n, q{SELECT count(*) FROM pg_class WHERE relname ~ '^o1_p3_b[0-9]{4}$'}), 'A: every leaf of ' . $n->name . ' is inside a bucket partition');
}
($rc, $out, $err) = run_sql($n2, q{INSERT INTO public.o1 SELECT g % 11, date '2026-03-01' + (g % 28), 9000 + g, g FROM generate_series(1, 90) g});
is($rc, 0, 'A: rows for the new partition from another node') or diag($err);
is(q1($_, q{SELECT count(*) FROM pg_class c WHERE c.relname ~ '^o1_p3_b[0-9]{4}$' AND c.relowner = 'pf_owner'::regrole}), q1($_, q{SELECT count(*) FROM pg_class WHERE relname ~ '^o1_p3_b[0-9]{4}$'}), 'A: the leaves of ' . $_->name . ' belong to the owner of the table') for @nodes;
is(rows_in('o1_p3'), '90', 'A: all of them are in its leaves');
is(q1($n3, q{SELECT count(*) FROM public.o1 WHERE created >= '2026-03-01'}), '90', 'A: and are read from a third node');
($rc, $out, $err) = run_sql($n3, q{INSERT INTO public.o1 VALUES (1, '2027-05-05', 1, 1)});
isnt($rc, 0, 'A: a row of an interval that has no partition is refused'); like($err, qr/no partition of relation/, 'A: by PostgreSQL');
# idempotence and refusals
$n1->safe_psql('postgres', q{CREATE TABLE public.k_plain (id int PRIMARY KEY); SELECT kingres.create_distributed_table('public.k_plain', 'id')});
($rc, $out, $err) = run_sql($n1, q{SELECT kingres.create_partition('public.o1', 'o1_p3', 'FROM (''2026-03-01'') TO (''2026-04-01'')', true)});
is($rc, 0, 'A: if_not_exists makes a repeat harmless');
for my $case (
	['a name that exists', q{SELECT kingres.create_partition('public.o1', 'o1_p3', 'FROM (''2026-05-01'') TO (''2026-06-01'')')}, 'already exists'],
	['a bound that overlaps', q{SELECT kingres.create_partition('public.o1', 'o1_p4', 'FROM (''2026-03-15'') TO (''2026-05-01'')')}, 'overlap'],
	['a bound with a second statement', q{SELECT kingres.create_partition('public.o1', 'o1_p5', 'FROM (''2026-06-01'') TO (''2026-07-01''); DROP TABLE public.o1')}, 'invalid partition bound'],
	['a bound that is not one', q{SELECT kingres.create_partition('public.o1', 'o1_p6', 'whatever')}, 'invalid partition bound'],
	['a name that is too long', q{SELECT kingres.create_partition('public.o1', 'o1_p7_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa', 'FROM (''2026-08-01'') TO (''2026-09-01'')')}, 'too long'],
	['a table that is not partitioned', q{SELECT kingres.create_partition('public.k_plain', 'x', 'FROM (1) TO (2)')}, 'not a user-partitioned'],
	)
{
	($rc, $out, $err) = run_sql($n1, $case->[1]);
	isnt($rc, 0, "A: $case->[0] is refused"); like($err, qr/$case->[2]/, "A: $case->[0]: the message") or diag($err);
}
($rc, $out, $err) = run_sql($n2, q{SELECT kingres.create_partition('public.o1', 'o1_p8', 'FROM (''2026-09-01'') TO (''2026-10-01'')')});
isnt($rc, 0, 'A: a node that is not the map group primary refuses');
is(leaves('o1_p4') + leaves('o1_p5') + leaves('o1_p6') + leaves('o1_p8'), '0', 'A: the refusals left no leaf');
is(prepared_count($_), '0', 'A: nothing is prepared on ' . $_->name) for @nodes;

# ---------------------------------------------------------------------------
# B. drop, detach and attach
# ---------------------------------------------------------------------------
my $cnt = q1($n1, q{SELECT count(*) FROM public.o1});
my $p3 = q1($n1, q{SELECT count(*) FROM public.o1 WHERE created >= '2026-03-01'});
($rc, $out, $err) = run_sql($n1, q{SELECT kingres.drop_partition('public.o1', 'o1_p3')});
is($rc, 0, 'B: drop_partition') or diag($err);
is(leaves('o1_p3'), '0', 'B: its leaves are gone everywhere');
is(q1($_, q{SELECT count(*) FROM kingres.dist_partition WHERE parent = 'public.o1'::regclass AND partition_name = 'o1_p3'}), '0', 'B: and its row on ' . $_->name) for @nodes;
is(q1($n1, q{SELECT count(*) FROM public.o1}), $cnt - $p3, 'B: only the rows of the dropped partition are gone');
($rc, $out, $err) = run_sql($n1, q{SELECT kingres.drop_partition('public.o1', 'o1_p3')});
isnt($rc, 0, 'B: dropping it again is refused');
($rc, $out, $err) = run_sql($n1, q{SELECT kingres.drop_partition('public.o1', 'o1_p3', true)});
is($rc, 0, 'B: missing_ok makes it harmless');

# detach: the partition becomes a table of its own, with its rows, in the group
my $p2n = q1($n1, q{SELECT count(*) FROM public.o1 WHERE created >= '2026-02-01' AND created < '2026-03-01'});
my $rest = q1($n1, q{SELECT count(*) FROM public.o1}) - $p2n;
($rc, $out, $err) = run_sql($n1, q{SELECT kingres.detach_partition('public.o1', 'o1_p2')});
is($rc, 0, 'B: detach_partition') or diag($err);
is(q1($n1, q{SELECT count(*) FROM public.o1_p2}), $p2n, 'B: the detached partition is a table with its rows');
is(q1($n1, q{SELECT count(*) FROM public.o1}), $rest, 'B: and the table has the others');
is(q1($n1, q{SELECT method || '|' || dist_column || '|' || (colocation_id = (SELECT colocation_id FROM kingres.dist_table WHERE relid = 'public.o1'::regclass))::text FROM kingres.dist_table WHERE relid = 'public.o1_p2'::regclass}), 'hash|customer|true', 'B: it is a distributed table of the same group');
is(q1($_, q{SELECT count(*) FROM kingres.dist_partition WHERE partition_name = 'o1_p2'}), '0', 'B: no partition row on ' . $_->name) for @nodes;
($rc, $out, $err) = run_sql($n2, q{INSERT INTO public.o1_p2 VALUES (3, '2026-02-10', 777777, 1); UPDATE public.o1_p2 SET qty = 5 WHERE id = 777777});
is($rc, 0, 'B: the detached table takes writes from another node') or diag($err);
is(q1($n1, q{SELECT count(*) FROM kingres.check_cluster()}), '0', 'B: check_cluster() finds nothing');

# attach it again, to the same table
($rc, $out, $err) = run_sql($n1, q{SELECT kingres.attach_partition('public.o1', 'public.o1_p2', 'FROM (''2026-02-01'') TO (''2026-03-01'')')});
is($rc, 0, 'B: attach_partition') or diag($err);
is(q1($n1, q{SELECT count(*) FROM public.o1}), $rest + $p2n + 1, 'B: the rows are back in the table, and the one written meanwhile');
is(q1($n1, q{SELECT count(*) FROM kingres.dist_table WHERE relid::text LIKE '%o1_p2'}), '0', 'B: the table is not in the catalog any more');
is(q1($_, q{SELECT to_regclass('public.o1_p2') IS NULL}), 't', 'B: and not a table on ' . $_->name) for @nodes;
is(q1($_, q{SELECT count(*) FROM kingres.dist_partition WHERE parent = 'public.o1'::regclass AND partition_name = 'o1_p2'}), '1', 'B: it is a partition on ' . $_->name) for @nodes;
is(leaves('o1_p2'), '6', 'B: with six leaves');
($rc, $out, $err) = run_sql($n1, q{SELECT kingres.attach_partition('public.o1', 'public.k_plain', 'FROM (''2030-01-01'') TO (''2031-01-01'')')});
isnt($rc, 0, 'B: a table with another distribution column is refused');
# a table on the same distribution column but in another colocation group is refused
$n1->safe_psql('postgres', q{CREATE TABLE public.ax (customer int NOT NULL, created date NOT NULL, id int NOT NULL, qty int, PRIMARY KEY (customer, created, id)); SELECT kingres.create_distributed_table('public.ax', 'customer', bucket_count => 3)});
($rc, $out, $err) = run_sql($n1, q{SELECT kingres.attach_partition('public.o1', 'public.ax', 'FROM (''2030-01-01'') TO (''2031-01-01'')')});
isnt($rc, 0, 'B: a table of another colocation group is refused'); like($err, qr/colocation group/, 'B: with the reason');
is(q1($n1, q{SELECT count(*) FROM kingres.dist_table WHERE relid = 'public.ax'::regclass}), '1', 'B: and it is still a distributed table');
is(q1($n1, q{SELECT count(*) FROM kingres.check_cluster()}), '0', 'B: check_cluster() finds nothing');

# ---------------------------------------------------------------------------
# C. fault points, and a writer on another node racing a held operation
# ---------------------------------------------------------------------------
new_table('o2', 1);
my $state = sub { return twin('o2') . '|' . q1($n1, q{SELECT count(*) FROM kingres.dist_partition WHERE parent = 'public.o2'::regclass}) . '|' . leaves('o2_p3'); };
for my $point (qw(partition_after_lock partition_after_leaves))
{
	my $before = $state->();
	($rc, $out, $err) = run_sql($n1, qq{SET kingres_shard.fault_injection = '$point:error'; SELECT kingres.create_partition('public.o2', 'o2_p3', 'FROM (''2026-03-01'') TO (''2026-04-01'')')});
	isnt($rc, 0, "C $point: create_partition fails");
	is($state->(), $before, "C $point: the table, the catalog and the leaves are as they were");
	is(prepared_count($_), '0', "C $point: nothing prepared on " . $_->name) for @nodes;
}
# a writer races a held create_partition: rows of an existing partition go on; rows of the default partition wait for the lock the attach takes on it
my $gate = "$PostgreSQL::Test::Utils::tmp_check/gate_c_$$";
append_to_file($gate, 'x');
my ($hout, $herr) = ('', '');
my $hh = IPC::Run::start([ 'psql', '-X', '-At', '-h', $n1->host, '-p', $n1->port, '-d', 'postgres',
	'-c', "SET kingres_shard.fault_injection = 'partition_after_leaves:wait:$gate'",
	'-c', q{SELECT kingres.create_partition('public.o2', 'o2_p3', 'FROM (''2026-03-01'') TO (''2026-04-01'')')} ], '>', \$hout, '2>', \$herr);
ok(wait_for_fault_file("$gate.reached", 60), 'C: create_partition is held after the leaves');
($rc, $out, $err) = run_sql($n2, q{INSERT INTO public.o2 VALUES (4, '2026-01-20', 880001, 1), (5, '2026-02-20', 880002, 2)});
is($rc, 0, 'C: a writer on another node goes on, into the existing partitions') or diag($err);
my $w = start_psql($n2, q{INSERT INTO public.o2 VALUES (6, '2030-01-01', 880003, 3)});
my $waited = 0;
for (my $i = 0; $i < 300 && !$waited; $i++)
{
	for my $n (@nodes) { $waited = 1 if q1($n, 'SELECT count(*) FROM pg_locks WHERE NOT granted') > 0; }
	select(undef, undef, undef, 0.1);
}
ok($waited, 'C: a writer into the default partition waits for the lock of the attach');
unlink $gate;
IPC::Run::finish($hh);
is($herr, '', 'C: the held call ends') or diag($herr);
my ($wout, $werr) = finish_psql($w);
is($werr, '', 'C: and the writer that waited goes on') or diag($werr);
is(q1($n1, q{SELECT count(*) FROM public.o2 WHERE id IN (880001, 880002, 880003)}), '3', 'C: no write is lost');
is(rows_in('o2_def'), q1($n1, q{SELECT count(*) FROM public.o2 WHERE created >= '2026-04-01'}), 'C: the default partition has its rows');
is(q1($n1, q{SELECT count(*) FROM kingres.check_cluster()}), '0', 'C: check_cluster() finds nothing');
# a group that an operation uses refuses
$n1->safe_psql('postgres', q{CREATE TABLE public.o2_d2 (id int PRIMARY KEY, v int); INSERT INTO public.o2_d2 SELECT g, g FROM generate_series(1, 50) g; SELECT kingres.create_distributed_table('public.o2_d2', 'id', colocate_with => 'public.o2')});
my $g2 = "$PostgreSQL::Test::Utils::tmp_check/gate_d_$$";
append_to_file($g2, 'x');
my ($o2, $e2) = ('', '');
my $h2 = IPC::Run::start([ 'psql', '-X', '-At', '-h', $n1->host, '-p', $n1->port, '-d', 'postgres',
	'-c', "SET kingres_shard.fault_injection = 'rebuild_after_copy:wait:$g2'",
	'-c', q{SELECT kingres.alter_distributed_table_concurrently('public.o2_d2', bucket_count => 9)} ], '>', \$o2, '2>', \$e2);
ok(wait_for_fault_file("$g2.reached", 60), 'C: an online alter of a table of the group is held');
($rc, $out, $err) = run_sql($n1, q{SELECT kingres.create_partition('public.o2', 'o2_p4', 'FROM (''2026-05-01'') TO (''2026-06-01'')')});
isnt($rc, 0, 'C: create_partition is refused while an operation uses the group'); like($err, qr/is busy/, 'C: and says so');
unlink $g2;
IPC::Run::finish($h2);
is($e2, '', 'C: the alter ends') or diag($e2);
done_testing();
