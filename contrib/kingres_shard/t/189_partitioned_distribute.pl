# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/189_partitioned_distribute.pl: create_distributed_table() of a table the user partitioned (design/shard-phase9.md, increment 9.5, feature 4):
# hash on top, the user's partitions below, as leaves inside every bucket partition.
#
#   A. a RANGE table with a default partition, rows in every partition: catalog, leaves on the owner nodes, rows, routing of new rows
#   B. a LIST table
#   C. the refusals: sub-partitions, a foreign partition, a hash-partitioned table, a partition with an object of its own, a partition on
#      its own, a name that is too long, too many leaves, the online variant
#   D. an error at each point of the creation leaves the table as it was

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(make_cluster run_sql prepared_count);

my ($n1, $n2, $n3) = make_cluster('pd', 3);
my @nodes = ($n1, $n2, $n3);
sub q1 { my ($node, $sql) = @_; return $node->safe_psql('postgres', $sql); }

# ---------------------------------------------------------------------------
# A. a RANGE table
# ---------------------------------------------------------------------------
$n1->safe_psql('postgres', q{
	CREATE TABLE public.ord (customer int NOT NULL, created date NOT NULL, id int NOT NULL, qty int, note text DEFAULT 'n',
	    PRIMARY KEY (customer, created, id)) PARTITION BY RANGE (created);
	CREATE TABLE public.ord_2026_11 PARTITION OF public.ord FOR VALUES FROM ('2026-11-01') TO ('2026-12-01');
	CREATE TABLE public.ord_2026_12 PARTITION OF public.ord FOR VALUES FROM ('2026-12-01') TO ('2027-01-01');
	CREATE TABLE public.ord_other PARTITION OF public.ord DEFAULT;
	CREATE INDEX ord_qty ON public.ord (qty);
	INSERT INTO public.ord SELECT g % 13, date '2026-11-01' + (g % 70), g, g % 50 FROM generate_series(1, 600) g;
	CREATE TABLE public.ord_tw (customer int, created date, id int, qty int, note text);
	INSERT INTO public.ord_tw SELECT * FROM public.ord;
});
my $before = q1($n1, q{SELECT count(*) || '|' || md5(string_agg(customer || ':' || created || ':' || id || ':' || coalesce(qty::text, ''), ',' ORDER BY customer, created, id)) FROM public.ord});
my ($rc, $out, $err) = run_sql($n1, q{SELECT kingres.create_distributed_table('public.ord', 'customer', bucket_count => 6)});
is($rc, 0, 'A: create_distributed_table of a RANGE-partitioned table') or diag($err);
is(q1($n1, q{SELECT partition_method || '|' || partition_key FROM kingres.dist_table WHERE relid = 'public.ord'::regclass}), 'range|(created)', 'A: the catalog has the method and the key');
is(q1($_, q{SELECT string_agg(partition_name || '=' || bound, ';' ORDER BY partition_name) FROM kingres.dist_partition WHERE parent = 'public.ord'::regclass}),
   q{ord_2026_11=FROM ('2026-11-01') TO ('2026-12-01');ord_2026_12=FROM ('2026-12-01') TO ('2027-01-01');ord_other=DEFAULT}, 'A: the partitions are in the catalog of ' . $_->name) for @nodes;
is(q1($n1, q{SELECT count(*) FROM kingres.partitions WHERE table_name = 'public.ord'::regclass AND leaf_count = 6}), '3', 'A: kingres.partitions shows three partitions of six leaves');
# the leaves: every node has, in each bucket partition it owns, one leaf per user partition
for my $n (@nodes)
{
	is(q1($n, q{SELECT count(*) FROM pg_class c JOIN pg_inherits i ON i.inhrelid = c.oid JOIN pg_class p ON p.oid = i.inhparent
	             WHERE p.relname ~ '^ord_b[0-9]{4}$' AND c.relname ~ '^ord_(2026_11|2026_12|other)_b[0-9]{4}$' AND c.relkind = 'r'}),
	   q1($n, q{SELECT 3 * count(*) FROM pg_class WHERE relname ~ '^ord_b[0-9]{4}$' AND relkind = 'p'}), 'A: three leaves in every bucket partition on ' . $n->name);
}
is(q1($n1, q{SELECT count(*) FROM pg_class WHERE relname ~ '^ord_(2026_11|2026_12|other)$'}), '0', 'A: the user partitions are not tables any more');
is(q1($n1, q{SELECT count(*) || '|' || md5(string_agg(customer || ':' || created || ':' || id || ':' || coalesce(qty::text, ''), ',' ORDER BY customer, created, id)) FROM public.ord}), $before, 'A: every row is there');
# every row is in the leaf of its interval: the leaves of one user partition, summed over the nodes, hold exactly that interval's rows
$_->safe_psql('postgres', q{CREATE FUNCTION public.leaf_rows(p text) RETURNS bigint LANGUAGE plpgsql AS $$ DECLARE r record; n bigint := 0; c bigint; BEGIN
	FOR r IN SELECT c.oid::regclass AS l FROM pg_class c WHERE c.relname ~ ('^' || p || '_b[0-9]{4}$') AND c.relkind = 'r' LOOP EXECUTE format('SELECT count(*) FROM %s', r.l) INTO c; n := n + c; END LOOP; RETURN n; END $$}) for @nodes;
for my $spec (['ord_2026_11', q{created >= '2026-11-01' AND created < '2026-12-01'}], ['ord_2026_12', q{created >= '2026-12-01' AND created < '2027-01-01'}],
              ['ord_other', q{created < '2026-11-01' OR created >= '2027-01-01'}])
{
	my $sum = 0;
	$sum += q1($_, "SELECT public.leaf_rows('$spec->[0]')") for @nodes;
	is($sum, q1($n1, "SELECT count(*) FROM public.ord_tw WHERE $spec->[1]"), "A: the leaves of $spec->[0] hold the rows of its interval, once");
}
is(q1($n1, q{SELECT count(*) FROM public.ord_tw WHERE created >= '2026-11-01' AND created < '2026-12-01'}) > 0 ? 1 : 0, '1', 'A: (the interval of November is not empty)');
is(q1($n1, q{SELECT count(*) FROM public.ord WHERE created >= '2026-11-01' AND created < '2026-12-01'}), q1($n1, q{SELECT count(*) FROM public.ord_tw WHERE created >= '2026-11-01' AND created < '2026-12-01'}), 'A: the rows of November');
is(q1($n1, q{SELECT count(*) FROM public.ord WHERE created >= '2027-01-01'}), q1($n1, q{SELECT count(*) FROM public.ord_tw WHERE created >= '2027-01-01'}), 'A: the rows of the default partition');
($rc, $out, $err) = run_sql($n2, q{INSERT INTO public.ord VALUES (3, '2026-12-15', 100001, 7), (4, '2027-03-01', 100002, 8), (5, '2026-11-02', 100003, 9); UPDATE public.ord SET qty = 99 WHERE id = 100001; DELETE FROM public.ord WHERE id = 100003});
is($rc, 0, 'A: writes from another node reach the leaves') or diag($err);
is(q1($n3, q{SELECT count(*) || '|' || sum(qty) FROM public.ord WHERE id IN (100001, 100002)}), '2|107', 'A: and are read back from a third node');
is(q1($n1, q{SELECT count(*) FROM kingres.check_cluster()}), '0', 'A: check_cluster() finds nothing');
is(prepared_count($_), '0', 'A: nothing is prepared on ' . $_->name) for @nodes;
is(q1($n1, q{SELECT count(*) FROM pg_class WHERE relname LIKE 'kingres\_shadow%'}), '0', 'A: no shadow is left');

# ---------------------------------------------------------------------------
# B. a LIST table
# ---------------------------------------------------------------------------
$n1->safe_psql('postgres', q{
	CREATE TABLE public.lst (tenant int NOT NULL, region text NOT NULL, id int NOT NULL, PRIMARY KEY (tenant, region, id)) PARTITION BY LIST (region);
	CREATE TABLE public.lst_eu PARTITION OF public.lst FOR VALUES IN ('de', 'fr');
	CREATE TABLE public.lst_us PARTITION OF public.lst FOR VALUES IN ('us');
	INSERT INTO public.lst SELECT g % 9, (ARRAY['de', 'fr', 'us'])[1 + g % 3], g FROM generate_series(1, 300) g;
});
($rc, $out, $err) = run_sql($n1, q{SELECT kingres.create_distributed_table('public.lst', 'tenant', bucket_count => 4)});
is($rc, 0, 'B: a LIST-partitioned table') or diag($err);
is(q1($n1, q{SELECT partition_method || '|' || partition_key FROM kingres.dist_table WHERE relid = 'public.lst'::regclass}), 'list|(region)', 'B: the catalog has the method and the key');
is(q1($n1, q{SELECT count(*) || '|' || count(DISTINCT region) FROM public.lst}), '300|3', 'B: every row is there');
is(q1($n1, q{SELECT count(*) FROM public.lst WHERE region IN ('de', 'fr')}), '200', 'B: the rows of the first partition');
($rc, $out, $err) = run_sql($n3, q{INSERT INTO public.lst VALUES (1, 'it', 9999)});
isnt($rc, 0, 'B: a row that fits no partition is refused'); like($err, qr/no partition of relation .* found for row/, 'B: with the error of PostgreSQL');

# ---------------------------------------------------------------------------
# C. refusals
# ---------------------------------------------------------------------------
sub refused
{
	my ($label, $setup, $call, $code) = @_;
	$n1->safe_psql('postgres', $setup) if $setup;
	my ($rc, $out, $err) = run_sql($n1, $call);
	isnt($rc, 0, "C: $label is refused");
	like($err, qr/$code/, "C: $label: the error names $code") or diag($err);
}
refused('sub-partitions', q{
	CREATE TABLE public.c1 (k int NOT NULL, d date NOT NULL, s int NOT NULL, PRIMARY KEY (k, d, s)) PARTITION BY RANGE (d);
	CREATE TABLE public.c1_a PARTITION OF public.c1 FOR VALUES FROM ('2026-01-01') TO ('2027-01-01') PARTITION BY HASH (s);
}, q{SELECT kingres.create_distributed_table('public.c1', 'k')}, 'itself partitioned');
refused('a foreign partition', q{
	CREATE FOREIGN DATA WRAPPER pd_fdw; CREATE SERVER pd_srv FOREIGN DATA WRAPPER pd_fdw;
	CREATE TABLE public.c2 (k int NOT NULL, d date NOT NULL) PARTITION BY RANGE (d);
	CREATE FOREIGN TABLE public.c2_f PARTITION OF public.c2 FOR VALUES FROM ('2026-01-01') TO ('2027-01-01') SERVER pd_srv;
}, q{SELECT kingres.create_distributed_table('public.c2', 'k')}, 'is not an ordinary table');
refused('a partition with a default of its own', q{
	CREATE TABLE public.c8 (k int NOT NULL, d date NOT NULL, v int, PRIMARY KEY (k, d)) PARTITION BY RANGE (d);
	CREATE TABLE public.c8_a PARTITION OF public.c8 FOR VALUES FROM ('2026-01-01') TO ('2027-01-01');
	ALTER TABLE public.c8_a ALTER COLUMN v SET DEFAULT 5;
}, q{SELECT kingres.create_distributed_table('public.c8', 'k')}, 'of its own');
refused('a partition with a storage parameter', q{
	CREATE TABLE public.c9 (k int NOT NULL, d date NOT NULL, v int, PRIMARY KEY (k, d)) PARTITION BY RANGE (d);
	CREATE TABLE public.c9_a PARTITION OF public.c9 FOR VALUES FROM ('2026-01-01') TO ('2027-01-01') WITH (fillfactor = 70);
}, q{SELECT kingres.create_distributed_table('public.c9', 'k')}, 'of its own');
{
	# a partition in another tablespace (a directory of its own, on this node)
	my $dir = PostgreSQL::Test::Utils::tempdir();
	$n1->safe_psql('postgres', "CREATE TABLESPACE pd_ts LOCATION '$dir'");
	refused('a partition in another tablespace', q{
		CREATE TABLE public.c10 (k int NOT NULL, d date NOT NULL, PRIMARY KEY (k, d)) PARTITION BY RANGE (d);
		CREATE TABLE public.c10_a PARTITION OF public.c10 FOR VALUES FROM ('2026-01-01') TO ('2027-01-01') TABLESPACE pd_ts;
	}, q{SELECT kingres.create_distributed_table('public.c10', 'k')}, 'another tablespace');
}
refused('a hash-partitioned table', q{
	CREATE TABLE public.c3 (k int NOT NULL, PRIMARY KEY (k)) PARTITION BY HASH (k);
	CREATE TABLE public.c3_a PARTITION OF public.c3 FOR VALUES WITH (MODULUS 2, REMAINDER 0);
	CREATE TABLE public.c3_b PARTITION OF public.c3 FOR VALUES WITH (MODULUS 2, REMAINDER 1);
}, q{SELECT kingres.create_distributed_table('public.c3', 'k')}, 'only tables partitioned by RANGE or LIST');
refused('a partition with an index of its own', q{
	CREATE TABLE public.c4 (k int NOT NULL, d date NOT NULL, v int, PRIMARY KEY (k, d)) PARTITION BY RANGE (d);
	CREATE TABLE public.c4_a PARTITION OF public.c4 FOR VALUES FROM ('2026-01-01') TO ('2027-01-01');
	CREATE INDEX ON ONLY public.c4_a (v);
}, q{SELECT kingres.create_distributed_table('public.c4', 'k')}, 'of its own');
refused('a partition with a check of its own', q{
	CREATE TABLE public.c5 (k int NOT NULL, d date NOT NULL, v int, PRIMARY KEY (k, d)) PARTITION BY RANGE (d);
	CREATE TABLE public.c5_a PARTITION OF public.c5 FOR VALUES FROM ('2026-01-01') TO ('2027-01-01');
	ALTER TABLE public.c5_a ADD CONSTRAINT own CHECK (v > 0);
}, q{SELECT kingres.create_distributed_table('public.c5', 'k')}, 'of its own');
refused('a partition on its own', undef, q{SELECT kingres.create_distributed_table('public.c5_a', 'k')}, 'partitioned_table|partition');
refused('a partition name that is too long', q{
	CREATE TABLE public.c6 (k int NOT NULL, d date NOT NULL, PRIMARY KEY (k, d)) PARTITION BY RANGE (d);
	CREATE TABLE public.c6_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa PARTITION OF public.c6 FOR VALUES FROM ('2026-01-01') TO ('2027-01-01');
}, q{SELECT kingres.create_distributed_table('public.c6', 'k')}, 'is too long');
refused('too many leaves', q{
	CREATE TABLE public.c7 (k int NOT NULL, d date NOT NULL, PRIMARY KEY (k, d)) PARTITION BY RANGE (d);
	CREATE TABLE public.c7_a PARTITION OF public.c7 FOR VALUES FROM ('2026-01-01') TO ('2026-02-01');
	CREATE TABLE public.c7_b PARTITION OF public.c7 FOR VALUES FROM ('2026-02-01') TO ('2026-03-01');
}, q{SET kingres_shard.max_partition_leaves = 7; SELECT kingres.create_distributed_table('public.c7', 'k', bucket_count => 4)}, 'max_partition_leaves');
refused('the online variant', undef, q{SELECT kingres.create_distributed_table_concurrently('public.c7', 'k')}, 'not_available_yet|blocking create_distributed_table');
is(q1($n1, q{SELECT count(*) FROM kingres.dist_table WHERE relid::text ~ 'c[0-9]'}), '0', 'C: none of them is distributed');
is(q1($n1, q{SELECT count(*) FROM kingres.table_change_report('distribute', 'public.ord_tw', '{"column": "customer"}') WHERE severity = 'blocker'}), '0', 'C: the report of an ordinary table is unchanged');
is(q1($n1, q{SELECT string_agg(code, ',' ORDER BY code) FROM kingres.table_change_report('distribute', 'public.c4', '{"column": "k"}') WHERE severity = 'blocker'}), 'partition_local_object', 'C: the report lists the code');
$n1->safe_psql('postgres', q{CREATE TABLE public.w1 (k int NOT NULL, d date NOT NULL, PRIMARY KEY (k, d)) PARTITION BY RANGE (d); CREATE TABLE public.w1_a PARTITION OF public.w1 DEFAULT;});
is(q1($n1, q{SELECT string_agg(code, ',' ORDER BY code) FROM kingres.table_change_report('distribute', 'public.w1', '{"column": "d"}') WHERE severity = 'warning'}), 'default_partition,distribution_column_is_partition_key', 'C: the report warns of a default partition and of a distribution column that is the key');

# ---------------------------------------------------------------------------
# D. an error at every point leaves the table as it was
# ---------------------------------------------------------------------------
for my $point (qw(distribute_after_lock distribute_after_shadow distribute_before_swap distribute_after_swap))
{
	my $t = 'e_' . $point;
	$n1->safe_psql('postgres', qq{
		CREATE TABLE public.$t (k int NOT NULL, d date NOT NULL, id int NOT NULL, PRIMARY KEY (k, d, id)) PARTITION BY RANGE (d);
		CREATE TABLE public.${t}_a PARTITION OF public.$t FOR VALUES FROM ('2026-01-01') TO ('2027-01-01');
		INSERT INTO public.$t SELECT g % 5, date '2026-01-01' + g, g FROM generate_series(1, 80) g;
	});
	my ($rc, $out, $err) = run_sql($n1, qq{SET kingres_shard.fault_injection = '$point:error'; SELECT kingres.create_distributed_table('public.$t', 'k', bucket_count => 3)});
	isnt($rc, 0, "D $point: the call fails");
	is(q1($n1, qq{SELECT relkind::text || '|' || (SELECT count(*) FROM public.$t) || '|' || (SELECT count(*) FROM kingres.dist_table WHERE relid = 'public.$t'::regclass) FROM pg_class WHERE oid = 'public.$t'::regclass}),
	   'p|80|0', "D $point: the table is as it was");
	is(q1($n1, qq{SELECT count(*) FROM pg_class WHERE relname = '${t}_a' AND relkind = 'r'}), '1', "D $point: and so is its partition");
	for my $n (@nodes)
	{
		is(q1($n, q{SELECT count(*) FROM pg_class WHERE relname LIKE 'kingres\_shadow%'}), '0', "D $point: no shadow on " . $n->name);
		is(prepared_count($n), '0', "D $point: nothing prepared on " . $n->name);
	}
	($rc, $out, $err) = run_sql($n1, qq{SELECT kingres.create_distributed_table('public.$t', 'k', bucket_count => 3)});
	is($rc, 0, "D $point: the next call succeeds") or diag($err);
	is(q1($n1, qq{SELECT count(*) FROM public.$t}), '80', "D $point: with every row");
}
done_testing();
