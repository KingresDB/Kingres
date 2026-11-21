# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/198_review_p9c_partitions.pl: user-partitioned tables, the recreated views and the statement mapping against the findings of the
# review of increments 9.4, 9.5, 9.8 and 9.9 (reviews/2026-11-21-kingres-shard-phase9-increments-4-9.md).
#
#   A. P9C-03  attach_partition refuses a table with a check, an index, a default, storage parameters, an owner or privileges of its own;
#              a table like the partitioned table is attached and a bucket move gives its leaf the definition of its siblings
#   B. the schema of the partition (mutant S18), the length of the name (S19), a bucket that is not active (S22), the leaf limit (C02)
#   C. P9C-01  the partition functions refuse a transaction snapshot
#   D. P9C-09  the statement mapping: another schema, IF NOT EXISTS, UNLOGGED, a cast with a modifier, a negative bound (C01)
#   E. P9C-10  detach_partition: the privileges and the comment of the table
#   F. P9C-11, P9C-12  a partition name with quotes and dots; a bound in lower case
#   G. P9C-14  a view on a partition stops create_distributed_table with the reason
#   H. P9C-05  a recreated view gets no privileges from the default privileges of the schema (mutant S10)

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(make_cluster run_sql prepared_count wait_for_fault_file move_state);

my ($n1, $n2, $n3) = make_cluster('pp', 3, logical => 1);
my @nodes = ($n1, $n2, $n3);
# a query that fails is a wrong answer, not the end of the test
sub q1 { my ($node, $sql) = @_; my ($rc, $out, $err) = run_sql($node, $sql); chomp $out; return $rc == 0 ? $out : "ERROR: $err"; }

$_->safe_psql('postgres', q{CREATE ROLE pc_r; CREATE ROLE pc_r2; CREATE ROLE pc_own; CREATE ROLE pc_vown; CREATE SCHEMA pc_s2}) for @nodes;

sub new_table
{
	my ($t) = @_;
	$n1->safe_psql('postgres', qq{
		CREATE TABLE public.$t (customer int NOT NULL, created date NOT NULL, id int NOT NULL, qty int, PRIMARY KEY (customer, created, id)) PARTITION BY RANGE (created);
		CREATE TABLE public.${t}_p1 PARTITION OF public.$t FOR VALUES FROM ('2026-01-01') TO ('2026-02-01');
		CREATE TABLE public.${t}_p2 PARTITION OF public.$t FOR VALUES FROM ('2026-02-01') TO ('2026-03-01');
		INSERT INTO public.$t SELECT g % 11, date '2026-01-01' + (g % 55), g, g % 40 FROM generate_series(1, 300) g;
		SELECT kingres.create_distributed_table('public.$t', 'customer', bucket_count => 6);
	});
}

# ---------------------------------------------------------------------------
# A. P9C-03: attach_partition and what the table has of its own
# ---------------------------------------------------------------------------
new_table('pa');
sub attach_candidate
{
	my ($name, $qty, $before, $after, $schema, $with) = @_;
	$with //= '';
	$schema //= 'public';
	$n1->safe_psql('postgres', qq{
		CREATE TABLE $schema.$name (customer int NOT NULL, created date NOT NULL, id int NOT NULL, qty int $qty, PRIMARY KEY (customer, created, id)) $with;
		$before
		INSERT INTO $schema.$name SELECT g % 11, date '2026-03-01' + (g % 28), g, g % 40 FROM generate_series(1, 200) g;
		SELECT kingres.create_distributed_table('$schema.$name', 'customer', colocate_with => 'public.pa');
		$after
	});
}
my $bound = q{FROM (''2026-03-01'') TO (''2026-04-01'')};
my @own = (
	['check constraint',     'CHECK (qty >= 0)',          '',                                          '',                                            qr/check constraint "ta_chk_qty_check"/],
	['index',                '',                          'CREATE INDEX ta_idx_q ON public.ta_idx (qty);', '',                                        qr/the index "ta_idx_q"/],
	['default',              'DEFAULT 5',                 '',                                          '',                                            qr/a default of its own on column "qty"/],
	['owner',                '',                          '',                                          'ALTER TABLE public.ta_owner OWNER TO pc_own;', qr/the owner is "pc_own"/],
	['storage parameters',   '',                          '',                                          '',                                            qr/storage parameters of its own/, 'WITH (fillfactor = 70)'],
	['privileges',           '',                          '',                                          'GRANT SELECT ON public.ta_acl TO pc_r;',      qr/privileges of its own/],
);
my %name = ('check constraint' => 'ta_chk', 'index' => 'ta_idx', 'default' => 'ta_def', 'owner' => 'ta_owner', 'privileges' => 'ta_acl', 'storage parameters' => 'ta_sp');
for my $o (@own)
{
	my ($what, $qty, $before, $after, $re, $with) = @$o;
	my $t = $name{$what};
	attach_candidate($t, $qty, $before, $after, undef, $with);
	my ($rc, $out, $err) = run_sql($n1, "SELECT kingres.attach_partition('public.pa', 'public.$t', '$bound')");
	isnt($rc, 0, "A: a table with a $what of its own is refused");
	like($err, qr/has objects of its own/, "A $what: with the reason");
	like($err, $re, "A $what: and the object");
	is(q1($n1, "SELECT count(*) FROM kingres.dist_table WHERE relid = to_regclass('public.$t')"), '1', "A $what: the table is still a distributed table");
	is(q1($n1, "SELECT count(*) FROM kingres.dist_partition WHERE parent = 'public.pa'::regclass"), '2', "A $what: and the partitions of pa are as they were");
}
attach_candidate('ta_ok', '', '', '');
is(q1($n1, "SELECT count(*) FROM kingres.internal_attach_differences('public.pa', 'public.ta_ok')"), '0', 'A: a table like the partitioned table has no difference');
{
	my ($rc, $out, $err) = run_sql($n1, "SELECT kingres.attach_partition('public.pa', 'public.ta_ok', '$bound')");
	is($rc, 0, 'A: a table like the partitioned table is attached') or diag($err);
	is(q1($n1, q{SELECT count(*) FROM public.pa WHERE created >= '2026-03-01'}), '200', 'A: its rows are rows of pa');

	my $coloc = q1($n1, q{SELECT colocation_id FROM kingres.dist_table WHERE relid = 'public.pa'::regclass});
	my $bucket = q1($n1, "SELECT bucket FROM kingres.bucket_map WHERE colocation_id = $coloc AND group_id = 0 ORDER BY bucket LIMIT 1");
	my $b4 = sprintf('%04d', $bucket);
	my $sig = sub { my ($n, $leaf) = @_; return q1($n, "SELECT (SELECT count(*) FROM pg_index WHERE indrelid = c.oid) || '|' || (SELECT count(*) FROM pg_constraint WHERE conrelid = c.oid AND contype = 'c') || '|' || pg_get_userbyid(c.relowner) || '|' || coalesce(c.relacl::text, '-') FROM pg_class c WHERE c.oid = 'public.$leaf'::regclass"); };
	my $before = $sig->($n1, "ta_ok_b$b4") . '/' . $sig->($n1, "pa_p1_b$b4");
	my ($x, $y) = split m{/}, $before;
	is($x, $y, "A: before the move the leaf of bucket $bucket has the definition of its sibling");
	my $id = q1($n1, "SELECT kingres.move_bucket($coloc, $bucket, 1)");
	for (1 .. 120) { last if q1($n1, "SELECT phase FROM kingres.bucket_move WHERE move_id = $id") =~ /^(cleaning|done)$/; select(undef, undef, undef, 0.5); }
	like(move_state($n1, $id), qr/^(cleaning|done)\|/, 'A: the bucket moved');
	q1($n1, q{SELECT kingres.cleanup_moved_buckets(true)});
	is($sig->($n2, "ta_ok_b$b4"), $x, 'A: after the move the leaf on the new node has the definition it had');
	is($sig->($n2, "pa_p1_b$b4"), $y, 'A: as its sibling');
	is(q1($n1, q{SELECT count(*) FROM public.pa}), '500', 'A: every row of pa is there');
	is(q1($n1, q{SELECT count(*) FROM kingres.check_cluster()}), '0', 'A: check_cluster() finds nothing');
}

# ---------------------------------------------------------------------------
# B. the checks that no test covered
# ---------------------------------------------------------------------------
{
	# S18: the table is in another schema
	attach_candidate('sx', '', '', '', 'pc_s2');
	my ($rc, $out, $err) = run_sql($n1, "SELECT kingres.attach_partition('public.pa', 'pc_s2.sx', 'FROM (''2027-01-01'') TO (''2027-02-01'')')");
	isnt($rc, 0, 'B: a table of another schema is refused');
	like($err, qr/the partition must be in the schema of the table/, 'B: with the reason');
	is(q1($n1, q{SELECT count(*) FROM kingres.dist_table WHERE relid = 'pc_s2.sx'::regclass}), '1', 'B: and it is still a distributed table');

	# S19: the longest name that leaves names of 63 bytes, and one byte more
	my $n57 = 'p' . ('a' x 56);
	my $n58 = 'p' . ('a' x 57);
	($rc, $out, $err) = run_sql($n1, "SELECT kingres.create_partition('public.pa', '$n57', 'FROM (''2027-01-01'') TO (''2027-02-01'')')");
	is($rc, 0, 'B: a partition name of 57 bytes is accepted') or diag($err);
	my $leaves = 0;
	$leaves += q1($_, "SELECT count(*) FROM pg_class WHERE relname ~ '^${n57}_b[0-9]{4}\$'") for @nodes;
	is($leaves, 6, 'B: with its six leaves, whole names');
	($rc, $out, $err) = run_sql($n1, "SELECT kingres.create_partition('public.pa', '$n58', 'FROM (''2027-02-01'') TO (''2027-03-01'')')");
	isnt($rc, 0, 'B: a partition name of 58 bytes is refused');
	like($err, qr/too long/, 'B: with the reason');
	is(q1($n1, "SELECT count(*) FROM pg_class WHERE relname LIKE '${n58}%'"), '0', 'B: and no leaf was made');
	run_sql($n1, "SELECT kingres.drop_partition('public.pa', '$n57')");

	# S22: a bucket that is not active, with no move recorded
	my $coloc = q1($n1, q{SELECT colocation_id FROM kingres.dist_table WHERE relid = 'public.pa'::regclass});
	for my $state ('moving', 'orphan')
	{
		$n1->safe_psql('postgres', "UPDATE kingres.bucket_map SET state = '$state' WHERE colocation_id = $coloc AND bucket = 2");
		for my $call ("SELECT kingres.create_partition('public.pa', 'pa_p9', 'FROM (''2028-01-01'') TO (''2028-02-01'')')",
		              "SELECT kingres.drop_partition('public.pa', 'pa_p1')",
		              "SELECT kingres.detach_partition('public.pa', 'pa_p1')",
		              "SELECT kingres.attach_partition('public.pa', 'pc_s2.sx', 'FROM (''2028-01-01'') TO (''2028-02-01'')')")
		{
			($rc, $out, $err) = run_sql($n1, $call);
			isnt($rc, 0, "B: a bucket in state $state: refused: " . substr($call, 0, 60));
			like($err, qr/is busy/, 'B: with the reason');
		}
		$n1->safe_psql('postgres', "UPDATE kingres.bucket_map SET state = 'active' WHERE colocation_id = $coloc AND bucket = 2");
	}
	is(q1($n1, q{SELECT count(*) FROM kingres.dist_partition WHERE parent = 'public.pa'::regclass}), '3', 'B: nothing changed in the catalog');

	# C02: the limit is a limit, not one below it: 4 buckets and 2 partitions are 8 leaves
	$n1->safe_psql('postgres', q{
		CREATE TABLE public.lim (k int NOT NULL, d date NOT NULL, PRIMARY KEY (k, d)) PARTITION BY RANGE (d);
		CREATE TABLE public.lim_a PARTITION OF public.lim FOR VALUES FROM ('2026-01-01') TO ('2026-02-01');
		CREATE TABLE public.lim_b PARTITION OF public.lim FOR VALUES FROM ('2026-02-01') TO ('2026-03-01');
	});
	($rc, $out, $err) = run_sql($n1, q{SET kingres_shard.max_partition_leaves = 8; SELECT kingres.create_distributed_table('public.lim', 'k', bucket_count => 4)});
	is($rc, 0, 'B: 8 leaves with a limit of 8 is accepted') or diag($err);
	is(q1($n1, q{SELECT count(*) FROM kingres.dist_table WHERE relid = 'public.lim'::regclass}), '1', 'B: the table is distributed');
}

# ---------------------------------------------------------------------------
# C. P9C-01: the partition functions
# ---------------------------------------------------------------------------
for my $level ('REPEATABLE READ', 'SERIALIZABLE')
{
	my ($rc, $out, $err) = run_sql($n1, "BEGIN ISOLATION LEVEL $level; SELECT 1; SELECT kingres.create_partition('public.pa', 'pa_iso', 'FROM (''2029-01-01'') TO (''2029-02-01'')'); COMMIT;");
	isnt($rc, 0, "C: create_partition is refused in $level");
	like($err, qr/cannot run in a REPEATABLE READ or SERIALIZABLE transaction/, "C: $level: with the reason");
}
is(q1($n1, q{SELECT count(*) FROM pg_class WHERE relname LIKE 'pa\_iso%'}), '0', 'C: no leaf was made');

# ---------------------------------------------------------------------------
# D. P9C-09: the statement mapping
# ---------------------------------------------------------------------------
{
	my ($rc, $out, $err) = run_sql($n1, q{CREATE TABLE pc_s2.pp2 PARTITION OF public.pa FOR VALUES FROM ('2030-01-01') TO ('2030-02-01')});
	isnt($rc, 0, 'D: a partition in another schema is refused');
	like($err, qr/the partition must be in the schema of the table/, 'D: with the reason');
	is(q1($n1, q{SELECT count(*) FROM kingres.dist_partition WHERE partition_name = 'pp2'}), '0', 'D: no partition pp2');

	($rc, $out, $err) = run_sql($n1, q{CREATE TABLE IF NOT EXISTS public.pa_p1 PARTITION OF public.pa FOR VALUES FROM ('2026-01-01') TO ('2026-02-01')});
	is($rc, 0, 'D: CREATE TABLE IF NOT EXISTS of a partition that exists is not an error') or diag($err);
	like($err, qr/relation "pa_p1" already exists, skipping/, 'D: it is a notice');
	($rc, $out, $err) = run_sql($n1, q{CREATE TABLE public.pa_p1 PARTITION OF public.pa FOR VALUES FROM ('2026-01-01') TO ('2026-02-01')});
	isnt($rc, 0, 'D: without IF NOT EXISTS it is an error');
	($rc, $out, $err) = run_sql($n1, q{CREATE TABLE IF NOT EXISTS public.pa_p5 PARTITION OF public.pa FOR VALUES FROM ('2031-01-01') TO ('2031-02-01')});
	is($rc, 0, 'D: IF NOT EXISTS of a partition that does not exist makes it') or diag($err);
	is(q1($n1, q{SELECT count(*) FROM kingres.dist_partition WHERE partition_name = 'pa_p5'}), '1', 'D: it is a partition');

	($rc, $out, $err) = run_sql($n1, q{CREATE UNLOGGED TABLE public.pa_u PARTITION OF public.pa FOR VALUES FROM ('2032-01-01') TO ('2032-02-01')});
	isnt($rc, 0, 'D: an UNLOGGED partition is refused');
	like($err, qr/permanent table/, 'D: with the reason');
	is(q1($n1, q{SELECT count(*) FROM kingres.dist_partition WHERE partition_name = 'pa_u'}), '0', 'D: and it was not made');

	# a cast with a modifier is written with it: 5.7::numeric(3,0) is 6
	$n1->safe_psql('postgres', q{
		CREATE TABLE public.nm (k int NOT NULL, amount numeric(10,2) NOT NULL, PRIMARY KEY (k, amount)) PARTITION BY RANGE (amount);
		CREATE TABLE public.nm_0 PARTITION OF public.nm FOR VALUES FROM (0) TO (1);
		SELECT kingres.create_distributed_table('public.nm', 'k', bucket_count => 3);
	});
	($rc, $out, $err) = run_sql($n1, q{CREATE TABLE public.nm_a PARTITION OF public.nm FOR VALUES FROM (5.7::numeric(3,0)) TO (100::numeric(5,0))});
	is($rc, 0, 'D: a bound with casts that have modifiers') or diag($err);
	my %b;
	for my $n (@nodes) { $b{$_} = 1 for split /\n/, q1($n, q{SELECT pg_get_expr(relpartbound, oid) FROM pg_class WHERE relname ~ '^nm_a_b[0-9]{4}$'}); }
	like(join('|', sort keys %b), qr/^FOR VALUES FROM \(6(\.00)?\) TO \(100(\.00)?\)$/, 'D: the leaves have the bound that PostgreSQL gives the cast (5.7 rounded to 6)');
	like(q1($n1, q{SELECT bound FROM kingres.dist_partition WHERE partition_name = 'nm_a'}), qr/numeric\(3,0\)/, 'D: the catalog keeps the modifier');

	# C01: a negative number in a bound
	$n1->safe_psql('postgres', q{
		CREATE TABLE public.ng (k int NOT NULL, n int NOT NULL, PRIMARY KEY (k, n)) PARTITION BY RANGE (n);
		CREATE TABLE public.ng_0 PARTITION OF public.ng FOR VALUES FROM (0) TO (100);
		SELECT kingres.create_distributed_table('public.ng', 'k', bucket_count => 3);
	});
	($rc, $out, $err) = run_sql($n1, q{CREATE TABLE public.ng_neg PARTITION OF public.ng FOR VALUES FROM (-100) TO (0)});
	is($rc, 0, 'D: a partition with negative numbers in its bound') or diag($err);
	is(q1($n1, q{SELECT bound FROM kingres.dist_partition WHERE partition_name = 'ng_neg'}), 'FROM (-100) TO (0)', 'D: the bound is written with its sign');
	# a minus in front of a cast is an operator in the parse tree, not a negative constant
	($rc, $out, $err) = run_sql($n1, q{CREATE TABLE public.ng_neg2 PARTITION OF public.ng FOR VALUES FROM (-200::int) TO (-100)});
	is($rc, 0, 'D: a bound with a minus in front of a cast') or diag($err);
	is(q1($n1, q{SELECT bound FROM kingres.dist_partition WHERE partition_name = 'ng_neg2'}), 'FROM (-200::integer) TO (-100)', 'D: the minus is written back');
	my %nb;
	for my $n (@nodes) { $nb{$_} = 1 for split /\n/, q1($n, q{SELECT pg_get_expr(relpartbound, oid) FROM pg_class WHERE relname ~ '^ng_neg2_b[0-9]{4}$'}); }
	is(join('|', sort keys %nb), "FOR VALUES FROM ('-200') TO ('-100')", 'D: the leaves have the negative bounds');
	($rc, $out, $err) = run_sql($n1, q{INSERT INTO public.ng SELECT g, -g FROM generate_series(1, 30) g});
	is($rc, 0, 'D: rows with negative keys are accepted') or diag($err);
	is(q1($n1, q{SELECT count(*) FROM public.ng WHERE n < 0}), '30', 'D: and found');
	($rc, $out, $err) = run_sql($n1, q{INSERT INTO public.ng VALUES (1, -201)});
	isnt($rc, 0, 'D: a row below the bound is refused');
}

# ---------------------------------------------------------------------------
# E. P9C-10: detach_partition carries the privileges and the comment
# ---------------------------------------------------------------------------
new_table('pd');
$n1->safe_psql('postgres', q{GRANT SELECT ON public.pd TO pc_r; GRANT UPDATE ON public.pd TO pc_r2 WITH GRANT OPTION; COMMENT ON TABLE public.pd IS 'the table pd'});
{
	my $acl = sub { my ($n, $t) = @_; return q1($n, "SELECT coalesce((SELECT string_agg(x::text, ',' ORDER BY x::text) FROM unnest(relacl) x), '-') || '|' || coalesce(obj_description(oid, 'pg_class'), '-') FROM pg_class WHERE oid = 'public.$t'::regclass"); };
	my ($rc, $out, $err) = run_sql($n1, q{SELECT kingres.detach_partition('public.pd', 'pd_p1')});
	is($rc, 0, 'E: detach_partition') or diag($err);
	for my $n (@nodes)
	{
		is($acl->($n, 'pd_p1'), $acl->($n, 'pd'), 'E: the new table has the privileges and the comment of the table on ' . $n->name);
	}
	is(q1($n1, q{SELECT concat(has_table_privilege('pc_r', 'public.pd_p1', 'SELECT'), has_table_privilege('pc_r', 'public.pd_p1', 'UPDATE'))}), 'tf', 'E: pc_r can read it and not update it');
	new_table('pe');
	($rc, $out, $err) = run_sql($n1, q{SELECT kingres.detach_partition('public.pe', 'pe_p1')});
	is($rc, 0, 'E: detach_partition of a table without privileges') or diag($err);
	is($acl->($n1, 'pe_p1'), $acl->($n1, 'pe'), 'E: the new table has what the table had, which is the owner only');
	unlike($acl->($n1, 'pe_p1'), qr/pc_r/, 'E: and nothing of the roles of the other table');
	is(q1($n1, q{SELECT count(*) FROM kingres.check_cluster()}), '0', 'E: check_cluster() finds nothing');
}

# ---------------------------------------------------------------------------
# F. P9C-11 and P9C-12: names, and the case of a bound
# ---------------------------------------------------------------------------
{
	new_table('pf');
	my ($rc, $out, $err) = run_sql($n1, q{SELECT kingres.create_partition('public.pf', 'a"b''c', 'FROM (''2027-03-01'') TO (''2027-04-01'')')});
	is($rc, 0, 'F: a partition name with a double quote and a quote') or diag($err);
	($rc, $out, $err) = run_sql($n1, q{SELECT kingres.create_partition('public.pf', 'd.o.t', 'FROM (''2027-04-01'') TO (''2027-05-01'')')});
	is($rc, 0, 'F: a partition name with dots') or diag($err);
	is(q1($n1, q{SELECT coalesce(string_agg(check_name || ':' || coalesce(object, ''), ','), 'clean') FROM kingres.check_cluster()}), 'clean', 'F: check_cluster() sees the leaves');
	run_sql($n1, q{SELECT kingres.drop_partition('public.pf', 'a"b''c'); SELECT kingres.drop_partition('public.pf', 'd.o.t')});

	($rc, $out, $err) = run_sql($n1, q{SELECT kingres.create_partition('public.pf', 'lc', 'from (''2027-05-01'') to (''2027-06-01'')')});
	is($rc, 0, 'F: a bound in lower case') or diag($err);
	is(q1($n1, q{SELECT count(*) FROM pg_class WHERE relname ~ '^lc_b[0-9]{4}$'}) + q1($n2, q{SELECT count(*) FROM pg_class WHERE relname ~ '^lc_b[0-9]{4}$'}) + q1($n3, q{SELECT count(*) FROM pg_class WHERE relname ~ '^lc_b[0-9]{4}$'}),
	   6, 'F: its six leaves');
	($rc, $out, $err) = run_sql($n1, q{SELECT kingres.create_partition('public.pf', 'lcd', 'default')});
	is($rc, 0, 'F: the default partition as "default"') or diag($err);
	is(q1($n1, q{SELECT is_default FROM kingres.partitions WHERE partition_name = 'lcd'}), 't', 'F: the view knows it is the default partition');
	($rc, $out, $err) = run_sql($n1, q{SELECT kingres.create_partition('public.pf', 'bad', 'from (''2027-07-01'') to (''2027-08-01''); drop table public.pf')});
	isnt($rc, 0, 'F: a second statement in a lower-case bound is still refused');
	like($err, qr/invalid partition bound/, 'F: with the reason');
	is(q1($n1, q{SELECT count(*) FROM kingres.check_cluster()}), '0', 'F: check_cluster() finds nothing');
}

# ---------------------------------------------------------------------------
# G. P9C-14: a view on a partition
# ---------------------------------------------------------------------------
{
	$n1->safe_psql('postgres', q{
		CREATE TABLE public.vp (k int NOT NULL, d date NOT NULL, PRIMARY KEY (k, d)) PARTITION BY RANGE (d);
		CREATE TABLE public.vp_a PARTITION OF public.vp FOR VALUES FROM ('2026-01-01') TO ('2027-01-01');
		INSERT INTO public.vp SELECT g, date '2026-01-01' + g FROM generate_series(1, 30) g;
		CREATE VIEW public.vp_v AS SELECT * FROM public.vp_a;
	});
	is(q1($n1, q{SELECT coalesce(string_agg(code, ','), '') FROM kingres.table_change_report('distribute', 'public.vp', '{"column":"k"}') WHERE severity = 'blocker'}), 'dependent_object', 'G: the report names the view');
	my ($rc, $out, $err) = run_sql($n1, q{SELECT kingres.create_distributed_table('public.vp', 'k')});
	isnt($rc, 0, 'G: create_distributed_table is refused');
	like($err, qr/view public\.vp_v depends on the partition public\.vp_a/, 'G: with the view and the partition');
	unlike($err, qr/cannot drop table/, 'G: not with the error of DROP TABLE');
	is(q1($n1, q{SELECT relkind FROM pg_class WHERE oid = 'public.vp'::regclass}), 'p', 'G: the table is as it was');
	$n1->safe_psql('postgres', q{DROP VIEW public.vp_v});
	($rc, $out, $err) = run_sql($n1, q{SELECT kingres.create_distributed_table('public.vp', 'k')});
	is($rc, 0, 'G: without the view it works') or diag($err);
}

# ---------------------------------------------------------------------------
# H. P9C-05: the privileges of a recreated view
# ---------------------------------------------------------------------------
{
	$n1->safe_psql('postgres', q{
		CREATE TABLE public.vt (id int PRIMARY KEY, customer int NOT NULL, qty int);
		INSERT INTO public.vt SELECT g, g % 13, g % 50 FROM generate_series(1, 200) g;
		SELECT kingres.create_distributed_table('public.vt', 'id', bucket_count => 4);
		CREATE VIEW public.vv_plain AS SELECT id, qty FROM public.vt;
		CREATE VIEW public.vv_acl AS SELECT id, customer FROM public.vt;
		ALTER VIEW public.vv_plain OWNER TO pc_vown;
		ALTER VIEW public.vv_acl OWNER TO pc_vown;
		GRANT SELECT ON public.vv_acl TO pc_r2 WITH GRANT OPTION;
		SET ROLE pc_vown;
		REVOKE INSERT, UPDATE, DELETE, TRUNCATE, REFERENCES, TRIGGER ON public.vv_acl FROM pc_vown;
		RESET ROLE;
	});
	# the default privileges of the schema arrive after the views exist, on every node: a new object would get them
	$_->safe_psql('postgres', q{ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT SELECT ON TABLES TO pc_r}) for @nodes;
	my $fp = sub { my ($n) = @_; return q1($n, q{SELECT string_agg(c.relname || '=' || (SELECT string_agg(x::text, ',' ORDER BY x::text) FROM unnest(coalesce(c.relacl, acldefault('r', c.relowner))) x) || '/' || pg_get_userbyid(c.relowner), E'\n' ORDER BY c.relname) FROM pg_class c WHERE c.relname IN ('vv_plain', 'vv_acl')}); };
	my $before = $fp->($n1);
	unlike($before, qr/pc_r=/, 'H: before: the views have nothing for pc_r');
	like($before, qr/vv_acl=pc_r2=r\*\/pc_vown/, 'H: before: vv_acl has the privilege of pc_r2 with grant option, and the owner has none of its own');
	my ($rc, $out, $err) = run_sql($n1, q{SET kingres_shard.dependent_views = recreate; SELECT kingres.alter_distributed_table('public.vt', bucket_count => 8)});
	is($rc, 0, 'H: alter with the views recreated') or diag($err);
	my $after = $fp->($n1);
	unlike($after, qr/pc_r=/, 'H: the recreated views have no privilege that the default privileges would give pc_r');
	is(q1($n1, q{SELECT concat(has_table_privilege('pc_r', 'public.vv_plain', 'SELECT'), has_table_privilege('pc_r', 'public.vv_acl', 'SELECT'))}), 'ff', 'H: pc_r cannot read them');
	is(q1($n1, q{SELECT concat(has_table_privilege('pc_vown', 'public.vv_plain', 'SELECT'), has_table_privilege('pc_vown', 'public.vv_plain', 'UPDATE'))}), 'tt', 'H: the owner of the view without an ACL has every privilege');
	is($after, $before, 'H: the ACL and the owner of both views are exactly what they were (an owner who had given up a privilege does not get it back)');
}

done_testing();
