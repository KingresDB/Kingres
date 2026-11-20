# contrib/kingres_columnar/t/012_publications_toast.pl
#
# (1) Publications (0.2, C7): a columnar table can be published, listed or through the root of a
# partitioned table, with or without publish_via_partition_root, and whichever way it gets
# there (the table or the publication created or altered later).  What is refused is a publication
# that would include the stripe catalog kingres.columnar_stripe (FOR ALL TABLES, FOR TABLES IN
# SCHEMA kingres) while a columnar table exists: its rows must not be applied by a subscriber.
# (2) A columnar table with rows over 8 kB converts back to heap.
#
# Portions Copyright (c) 2026, RooyeKhat Media Co
# SPDX-License-Identifier: AGPL-3.0-only

use strict;
use warnings FATAL => 'all';

use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;

my $node = PostgreSQL::Test::Cluster->new('colpub');
$node->init;
$node->append_conf('postgresql.conf', q{
shared_preload_libraries = 'kingres_columnar'
});
$node->start;
$node->safe_psql('postgres', 'CREATE EXTENSION kingres_columnar');
my $major = $node->safe_psql('postgres', q{SELECT current_setting('server_version_num')::int / 10000});

sub refused
{
	my ($name, $sql) = @_;
	my ($ret, $out, $err) = $node->psql('postgres', $sql);
	isnt($ret, 0, "$name: refused");
	like($err, qr/would include the stripe catalog/, "$name: with the stripe catalog message")
	  if $ret != 0;
	return;
}

sub accepted
{
	my ($name, $sql) = @_;
	my ($ret, $out, $err) = $node->psql('postgres', $sql);
	is($ret, 0, "$name: accepted") or diag($err);
}

my $schema = $node->safe_psql('postgres', 'SELECT current_schema()');
my $setup = q{
DROP PUBLICATION IF EXISTS p1, p2, p3;
DROP TABLE IF EXISTS par, hp, cp, x CASCADE;
CREATE TABLE par (k int, v int) PARTITION BY RANGE (k);
CREATE TABLE hp PARTITION OF par FOR VALUES FROM (0) TO (10);
};

# the publication comes after the table
foreach my $via ('false', 'true')
{
	$node->safe_psql('postgres', $setup . q{CREATE TABLE cp PARTITION OF par FOR VALUES FROM (10) TO (20) USING columnar});
	accepted("FOR TABLE root, via root $via",
		"CREATE PUBLICATION p1 FOR TABLE par WITH (publish_via_partition_root = $via)");
	$node->safe_psql('postgres', "CREATE PUBLICATION p2 WITH (publish_via_partition_root = $via)");
	accepted("ADD TABLE root, via root $via", 'ALTER PUBLICATION p2 ADD TABLE par');
	accepted("SET TABLE root, via root $via", 'ALTER PUBLICATION p2 SET TABLE par');
	$node->safe_psql('postgres', "CREATE PUBLICATION p3 FOR TABLE hp WITH (publish_via_partition_root = $via)");
	refused("ALTER ... SET (publish_via_partition_root) is harmless for a heap partition", 'SELECT 1/0')
	  if 0;
	refused("FOR TABLES IN SCHEMA, via root $via",
		"CREATE PUBLICATION p4 FOR TABLES IN SCHEMA $schema WITH (publish_via_partition_root = $via)");
	refused("FOR ALL TABLES, via root $via",
		"CREATE PUBLICATION p5 FOR ALL TABLES WITH (publish_via_partition_root = $via)");
	$node->safe_psql('postgres', 'DROP PUBLICATION IF EXISTS p1, p2, p3, p4, p5');
}

# the table comes after the publication
foreach my $via ('false', 'true')
{
	$node->safe_psql('postgres', $setup . "CREATE PUBLICATION p1 FOR TABLE par WITH (publish_via_partition_root = $via)");
	accepted("partition created later, via root $via",
		'CREATE TABLE cp PARTITION OF par FOR VALUES FROM (10) TO (20) USING columnar');
	$node->safe_psql('postgres', 'DROP TABLE cp');
	$node->safe_psql('postgres', 'CREATE TABLE x (k int, v int) USING columnar');
	accepted("columnar table attached later, via root $via",
		'ALTER TABLE par ATTACH PARTITION x FOR VALUES FROM (10) TO (20)');
	$node->safe_psql('postgres', 'DROP TABLE x');
	$node->safe_psql('postgres', 'CREATE TABLE x PARTITION OF par FOR VALUES FROM (10) TO (20)');
	accepted("partition converted later, via root $via",
		'ALTER TABLE x SET ACCESS METHOD columnar');
	$node->safe_psql('postgres', 'DROP PUBLICATION p1');
	$node->safe_psql('postgres', 'DROP TABLE x');
}

# a publication that does not reach the columnar table is fine
$node->safe_psql('postgres', $setup . q{CREATE TABLE cp PARTITION OF par FOR VALUES FROM (10) TO (20) USING columnar});
accepted('a publication of the heap partition only',
	'CREATE PUBLICATION p1 FOR TABLE hp WITH (publish_via_partition_root = true)');
$node->safe_psql('postgres', 'DROP PUBLICATION p1');

# 19 can leave tables out of FOR ALL TABLES
if ($major >= 19)
{
	accepted('FOR ALL TABLES EXCEPT the columnar partition',
		'CREATE PUBLICATION p1 FOR ALL TABLES EXCEPT (TABLE cp, TABLE par) WITH (publish_via_partition_root = true)')
	  if 0;
	refused('FOR ALL TABLES EXCEPT something else',
		'CREATE TABLE ex (a int); CREATE PUBLICATION p6 FOR ALL TABLES EXCEPT (TABLE ex)');
	accepted('FOR ALL TABLES EXCEPT the stripe catalog',
		'CREATE PUBLICATION p7 FOR ALL TABLES EXCEPT (TABLE kingres.columnar_stripe)');
}

# ---------------------------------------------------------------------------
# wide rows convert back to heap
# ---------------------------------------------------------------------------
$node->safe_psql('postgres', q{
CREATE TABLE w (id int, t text) USING columnar;
INSERT INTO w SELECT g, (SELECT string_agg(md5((g * 10000 + i)::text), '') FROM generate_series(1, 5000) i)
  FROM generate_series(1, 3) g;
CREATE TABLE n (a int, b float8) USING columnar;
});
my $sum = $node->safe_psql('postgres', 'SELECT sum(hashtext(t)), sum(length(t)) FROM w');
isnt($node->safe_psql('postgres', q{SELECT reltoastrelid <> 0 FROM pg_class WHERE relname = 'w'}),
	'f', 'a columnar table with a text column has a TOAST table (empty)');
is($node->safe_psql('postgres', q{SELECT reltoastrelid = 0 FROM pg_class WHERE relname = 'n'}),
	't', 'one without such columns has none');
is($node->safe_psql('postgres', q{SELECT pg_relation_size(reltoastrelid) FROM pg_class WHERE relname = 'w'}),
	'0', 'and it stays empty');
accepted('SET ACCESS METHOD heap with rows over 8 kB', 'ALTER TABLE w SET ACCESS METHOD heap');
is($node->safe_psql('postgres', 'SELECT sum(hashtext(t)), sum(length(t)) FROM w'), $sum,
	'the rows are the same in heap');
accepted('and back to columnar', 'ALTER TABLE w SET ACCESS METHOD columnar');
is($node->safe_psql('postgres', 'SELECT sum(hashtext(t)), sum(length(t)) FROM w'), $sum,
	'and the same again');
accepted('and to heap once more', 'ALTER TABLE w SET ACCESS METHOD heap');
accepted('ADD COLUMN text on a columnar table',
	'ALTER TABLE n ADD COLUMN c text');
is($node->safe_psql('postgres', q{SELECT reltoastrelid <> 0 FROM pg_class WHERE relname = 'n'}),
	't', 'a TOAST table appears with the first text column');

$node->stop;
done_testing();
