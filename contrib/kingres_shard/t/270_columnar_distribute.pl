# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/270_columnar_distribute.pl: a columnar table stays columnar when it is distributed (design/analytics-phase12-storage.md, 9.1,
# increment S1).  create_distributed_table carries the access method: the parent and every bucket partition on every node are
# columnar, every bucket partition has REPLICA IDENTITY FULL, a node that lacks the access method is a blocker of the report, a
# reference table cannot be columnar, the leaves of a user-partitioned table keep their own access methods, and undistribute and
# alter keep the access method.  Needs kingres_columnar and kingres_ledger built and installed into the same installation.

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(make_cluster run_sql prepared_count);

my ($n1, $n2) = make_cluster('cd', 2, extra => "shared_preload_libraries = 'kingres_shard,kingres_columnar'\n");

sub qs { my ($n, $sql) = @_; return $n->safe_psql('postgres', $sql); }
sub codes
{
	my ($sql) = @_;
	return qs($n1, "SELECT coalesce(string_agg(severity || ':' || code, ',' ORDER BY code), '') FROM kingres.table_change_report($sql)");
}
# the access methods of the parts of a table on a node: "parent=<am> parts=<n>:<am>[:<replident>]"
sub ams
{
	my ($n, $t) = @_;
	return qs($n, qq{
		SELECT coalesce((SELECT a.amname FROM pg_class c JOIN pg_am a ON a.oid = c.relam WHERE c.oid = 'public.$t'::regclass), 'none')
		    || ' ' || coalesce((SELECT string_agg(DISTINCT coalesce(a.amname, 'none') || '/' || c.relreplident::text, ',' ORDER BY coalesce(a.amname, 'none') || '/' || c.relreplident::text)
		                          FROM pg_inherits i JOIN pg_class c ON c.oid = i.inhrelid LEFT JOIN pg_am a ON a.oid = c.relam
		                         WHERE i.inhparent = 'public.$t'::regclass AND c.relkind = 'r'), '-')});
}

# a node without the access method: the report says so, nothing is done
qs($n1, 'CREATE EXTENSION kingres_columnar');
qs($n1, q{
	CREATE TABLE public.ev (id int, grp int, v text) USING columnar;
	INSERT INTO public.ev SELECT g, g % 7, 'v' || g FROM generate_series(1, 2000) g;
});
like(codes("'distribute', 'public.ev', '{\"column\":\"id\"}'"), qr/blocker:access_method_missing/, 'the report names a node that lacks the access method');
like(codes("'distribute', 'public.ev', '{\"column\":\"id\"}'"), qr/note:columnar_storage/, '... and says what the table becomes');
my (undef, undef, $err) = run_sql($n1, "SELECT kingres.create_distributed_table('public.ev', 'id', bucket_count => 8)");
like($err, qr/access method "columnar" does not exist on node/, 'create_distributed_table refuses: the other node cannot make the table');
is(qs($n1, "SELECT relkind::text || ':' || (SELECT amname FROM pg_am WHERE oid = relam) FROM pg_class WHERE oid = 'public.ev'::regclass"), 'r:columnar', 'the table is as it was');
is(qs($n1, 'SELECT count(*) FROM kingres.dist_table'), '0', 'nothing was distributed');
qs($n2, 'CREATE EXTENSION kingres_columnar');
unlike(codes("'distribute', 'public.ev', '{\"column\":\"id\"}'"), qr/blocker/, 'with the access method on both nodes there is no blocker');

# the distribution of a columnar table with rows (the blocking copy)
qs($n1, "SELECT kingres.create_distributed_table('public.ev', 'id', bucket_count => 8)");
for my $n ($n1, $n2)
{
	is(ams($n, 'ev'), 'columnar columnar/f', 'ev: the parent and every bucket partition are columnar with REPLICA IDENTITY FULL on ' . $n->name);
	is(qs($n, 'SELECT count(*) || \',\' || sum(id) FROM public.ev'), '2000,2001000', 'every row is there through the distributed scan on ' . $n->name);
}
is(qs($n1, "SELECT count(*) FROM kingres.bucket_map b JOIN kingres.dist_table d USING (colocation_id) WHERE d.relid = 'public.ev'::regclass"), '8', 'the catalog has the 8 buckets');

# a heap table next to it stays heap
qs($n1, q{CREATE TABLE public.hp (id int PRIMARY KEY, v text); INSERT INTO public.hp SELECT g, 'h' || g FROM generate_series(1, 100) g;});
qs($n1, "SELECT kingres.create_distributed_table('public.hp', 'id', colocate_with => 'public.ev')");
is(ams($n2, 'hp'), 'none heap/d', 'a heap table is a heap table, with the replica identity it had');
is(qs($n1, "SELECT (SELECT count(*) FROM public.hp h JOIN public.ev e ON e.id = h.id)"), '100', 'a join of the heap and the columnar table, colocated');

# writes: the router, COPY, an UPDATE and a DELETE
qs($n1, "INSERT INTO public.ev SELECT g, g % 7, 'w' || g FROM generate_series(2001, 2100) g");
is(qs($n2, 'SELECT count(*) FROM public.ev'), '2100', "INSERT through the router");
{
	my ($o, $e);
	my $rows = join('', map { "$_\t1\tc$_\n" } 2101 .. 2150);
	is($n1->psql('postgres', "COPY public.ev FROM STDIN;\n$rows\\.\n", stdout => \$o, stderr => \$e), 0, "COPY FROM through the router") or diag($e);
	is(qs($n2, 'SELECT count(*) FROM public.ev'), '2150', "the copied rows are in");
}
($err) = (run_sql($n1, "UPDATE public.ev SET v = 'x' WHERE id = 5"))[2];
like($err, qr/./, 'an UPDATE is refused by the access method');
($err) = (run_sql($n1, "DELETE FROM public.ev WHERE id = 5"))[2];
like($err, qr/./, 'a DELETE is refused too');
is(qs($n1, 'SELECT count(*) FROM public.ev'), '2150', 'and nothing changed');

# a columnar table with a primary key, empty
qs($n1, 'CREATE TABLE public.ek (id int PRIMARY KEY, v text) USING columnar');
qs($n1, "SELECT kingres.create_distributed_table('public.ek', 'id', colocate_with => 'public.ev')");
is(ams($n2, 'ek'), 'columnar columnar/f', 'an empty columnar table with a primary key');
qs($n1, "INSERT INTO public.ek SELECT g, 'k' || g FROM generate_series(1, 50) g");
is(qs($n2, 'SELECT count(*) FROM public.ek'), '50', 'rows go in after the distribution');
($err) = (run_sql($n1, "INSERT INTO public.ek VALUES (7, 'dup')"))[2];
like($err, qr/duplicate key|unique/, 'the primary key still holds on the bucket partition');

# a reference table cannot be columnar
qs($n1, q{CREATE TABLE public.rc (id int PRIMARY KEY, v text) USING columnar});
like(codes("'distribute_reference', 'public.rc'"), qr/blocker:columnar_reference_table/, 'the report: a reference table cannot be columnar');
($err) = (run_sql($n1, "SELECT kingres.create_reference_table('public.rc')"))[2];
like($err, qr/reference table.*access method "columnar"/s, 'create_reference_table refuses a columnar table');
is(qs($n1, 'SELECT count(*) FROM kingres.dist_table WHERE relid = \'public.rc\'::regclass'), '0', 'and leaves it alone');

# a user-partitioned table: each leaf keeps its access method
qs($n1, q{
	CREATE TABLE public.up (id int, d date, v text) PARTITION BY RANGE (d);
	CREATE TABLE public.up_old PARTITION OF public.up FOR VALUES FROM ('2020-01-01') TO ('2021-01-01') USING columnar;
	SELECT kingres.columnar_set_options('public.up_old', compression => 'none', stripe_row_count => 25000, chunk_row_count => 2500);
	CREATE TABLE public.up_new PARTITION OF public.up FOR VALUES FROM ('2021-01-01') TO ('2022-01-01') USING heap;
	INSERT INTO public.up SELECT g, '2020-06-01'::date + (g % 300), 'u' || g FROM generate_series(1, 400) g;
});
qs($n1, "SELECT kingres.create_distributed_table('public.up', 'id', colocate_with => 'public.ev')");
for my $n ($n1, $n2)
{
	is(qs($n, q{SELECT string_agg(DISTINCT regexp_replace(c.relname, '_b[0-9]+$', '') || '=' || coalesce(a.amname, 'none') || '/' || c.relreplident::text, ',' ORDER BY regexp_replace(c.relname, '_b[0-9]+$', '') || '=' || coalesce(a.amname, 'none') || '/' || c.relreplident::text)
	                          FROM pg_class c LEFT JOIN pg_am a ON a.oid = c.relam
	                         WHERE c.relname ~ '^up_(old|new)_b[0-9]+$'}),
		'up_new=heap/d,up_old=columnar/f', 'the leaves of the user-partitioned table keep their access methods on ' . $n->name);
	is(qs($n, q{SELECT coalesce(string_agg(DISTINCT o.compression || '/' || o.stripe_row_count || '/' || o.chunk_row_count, ','), 'none-found') FROM pg_class c, LATERAL kingres.columnar_options(c.oid::regclass) o WHERE c.relname ~ '^up_old_b[0-9]+$'}), 'none/25000/2500', 'and the options of the columnar leaf on ' . $n->name);
}
is(qs($n1, 'SELECT count(*) FROM public.up'), '400', 'the rows of the user-partitioned table are there');

# undistribute keeps the access method
qs($n1, "SELECT kingres.undistribute_table('public.ek')");
is(ams($n1, 'ek'), 'columnar -', 'undistribute_table gives a columnar table back');
is(qs($n1, 'SELECT count(*) FROM public.ek'), '50', 'with its rows');

# alter_distributed_table (a rebuild into the same table with another number of buckets) keeps the access method
qs($n1, "SELECT kingres.alter_distributed_table('public.ev', bucket_count => 4, cascade_to_colocated => true)");
for my $n ($n1, $n2)
{
	is(ams($n, 'ev'), 'columnar columnar/f', 'alter_distributed_table: still columnar with REPLICA IDENTITY FULL on ' . $n->name);
	is(qs($n, 'SELECT count(*) || \',\' || sum(id) FROM public.ev'), '2150,2312325', 'and whole on ' . $n->name);
}

# the options of a columnar table (columnar_set_options) are carried: create_distributed_table, alter_distributed_table, undistribute_table (P12S-03)
qs($n1, q{
	CREATE TABLE public.oc (id int, v text) USING columnar;
	INSERT INTO public.oc SELECT g, 'o' || g FROM generate_series(1, 300) g;
	SELECT kingres.columnar_set_options('public.oc', compression => 'none', stripe_row_count => 20000, chunk_row_count => 5000);
});
sub options_of
{
	my ($n, $t) = @_;
	return qs($n, qq{SELECT coalesce(string_agg(DISTINCT o.compression || '/' || o.stripe_row_count || '/' || o.chunk_row_count, ','), 'none-found')
	                  FROM pg_inherits i JOIN pg_class c ON c.oid = i.inhrelid, LATERAL kingres.columnar_options(c.oid::regclass) o
	                 WHERE i.inhparent = 'public.$t'::regclass});
}
qs($n1, "SELECT kingres.create_distributed_table('public.oc', 'id', bucket_count => 4)");
is(options_of($_, 'oc'), 'none/20000/5000', 'create_distributed_table carries the options of the columnar table to every bucket partition on ' . $_->name) for ($n1, $n2);
qs($n1, "SELECT kingres.alter_distributed_table('public.oc', bucket_count => 6)");
is(options_of($_, 'oc'), 'none/20000/5000', 'alter_distributed_table keeps them on ' . $_->name) for ($n1, $n2);
qs($n1, "SELECT kingres.undistribute_table('public.oc')");
is(qs($n1, q{SELECT o.compression || '/' || o.stripe_row_count || '/' || o.chunk_row_count FROM kingres.columnar_options('public.oc') o}), 'none/20000/5000', 'undistribute_table gives the options back');
is(qs($n1, 'SELECT count(*) FROM public.oc'), '300', 'with its rows');

# a node that joins later gets the parent with its access method; without the access method it is refused and nothing is left
my $n3 = KingresShardTest::make_node('cd3', extra => "shared_preload_libraries = 'kingres_shard,kingres_columnar'\n");
(undef, undef, $err) = run_sql($n1, sprintf("SELECT kingres.add_node('%s', %d)", $n3->host, $n3->port));
like($err, qr/columnar/, 'add_node: a node without the access method is refused, and the message names it');
is(qs($n1, "SELECT count(*) FROM kingres.node"), '2', 'so the node is not in the cluster');
qs($n3, 'CREATE EXTENSION kingres_columnar');
qs($n1, sprintf("SELECT kingres.add_node('%s', %d)", $n3->host, $n3->port));
is(ams($n3, 'ev'), 'columnar -', 'add_node: the parent on the new node is columnar');
is(ams($n3, 'hp'), 'none -', 'and a heap table is a heap table');

is(prepared_count($_), '0', 'nothing is prepared on ' . $_->name) for ($n1, $n2, $n3);
done_testing();
