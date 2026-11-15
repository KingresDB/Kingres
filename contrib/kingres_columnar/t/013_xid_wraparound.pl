# contrib/kingres_columnar/t/013_xid_wraparound.pl
#
# A columnar table through two laps of the transaction id space.
#
# The metapage of a columnar file holds the id of the transaction that created
# it.  Left alone it would, after exactly one lap (4 294 967 296 transactions),
# name whichever transaction then runs with the same 32-bit number, and every
# read of the table would fail with a serialization failure while that
# transaction lives.  VACUUM freezes the id and tells PostgreSQL the oldest
# one that is left (relfrozenxid), so that autovacuum's protection against
# wraparound reaches the table.  This test consumes ids with the
# xid_wraparound test module (src/test/modules/xid_wraparound of the
# PostgreSQL tree; skipped where it is not installed), with autovacuum doing
# the freezing, and at the same point of each lap holds a transaction whose
# id has the number the table was created with.
#
# It takes several minutes; KC_SKIP_WRAPAROUND=1 skips it.
#
# Portions Copyright (c) 2026, RooyeKhat Media Co
# SPDX-License-Identifier: AGPL-3.0-only

use strict;
use warnings FATAL => 'all';

use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;

plan skip_all => 'KC_SKIP_WRAPAROUND is set' if $ENV{KC_SKIP_WRAPAROUND};

my $node = PostgreSQL::Test::Cluster->new('colwrap');
$node->init;
$node->append_conf('postgresql.conf', q{
shared_preload_libraries = 'kingres_columnar'
autovacuum = on
autovacuum_naptime = 1s
autovacuum_freeze_max_age = 150000000
vacuum_freeze_min_age = 1000000
log_min_messages = warning
});
$node->start;

my $avail = $node->safe_psql('postgres',
	q{SELECT count(*) FROM pg_available_extensions WHERE name = 'xid_wraparound'});
if ($avail ne '1')
{
	$node->stop;
	plan skip_all => 'the xid_wraparound test module is not installed';
}

$node->safe_psql('postgres', q{
CREATE EXTENSION kingres_columnar;
CREATE EXTENSION xid_wraparound;
CREATE TABLE c (a int, t text) USING columnar;
INSERT INTO c SELECT g, 'x' || g FROM generate_series(1, 2000) g;
CREATE TABLE h (a int);
});
my $x = $node->safe_psql('postgres',
	q{SELECT xmin::text::bigint FROM pg_class WHERE relname = 'c'});
my $has_fn = $node->safe_psql('postgres',
	q{SELECT count(*) FROM pg_proc WHERE proname = 'columnar_creating_xid'});
cmp_ok($x, '>', 3, "the table was created by transaction $x");

sub current_xid
{
	return $node->safe_psql('postgres', q{SELECT pg_current_xact_id()::text::bigint});
}

# consume ids in steps, letting autovacuum freeze the table between them
sub consume_to
{
	my ($target) = @_;
	while ($target - current_xid() > 100_000_000)
	{
		$node->safe_psql('postgres', 'SELECT consume_xids(100000000)');
		$node->poll_query_until('postgres',
			q{SELECT age(relfrozenxid) < 140000000 FROM pg_class WHERE relname = 'c'})
		  or die 'autovacuum did not advance relfrozenxid of the columnar table';
		$node->poll_query_until('postgres',
			q{SELECT age(datfrozenxid) < 400000000 FROM pg_database WHERE datname = 'postgres'})
		  or die 'datfrozenxid did not advance';
	}
	my $left = $target - current_xid();
	$node->safe_psql('postgres', "SELECT consume_xids($left)") if $left > 0;
}

foreach my $lap (1, 2)
{
	my $cur = current_xid();
	my $target = (int($cur / 4294967296) + 1) * 4294967296 + $x - 40;
	consume_to($target);

	# a transaction with the id the table was created with
	my $hold = $node->background_psql('postgres', on_error_stop => 0);
	my $got = -1;
	for my $try (1 .. 100)
	{
		$hold->query_safe('BEGIN');
		my $n = $hold->query_safe(q{SELECT pg_current_xact_id()::text::bigint % 4294967296});
		$n =~ s/\s+//g;
		if ($n == $x) { $got = $n; last; }
		$hold->query_safe('ROLLBACK');
	}
	is($got, $x, "lap $lap: a transaction with the creation id $x is open");

	foreach my $level ('READ COMMITTED', 'REPEATABLE READ')
	{
		my ($ret, $out, $err) = $node->psql('postgres',
			"BEGIN ISOLATION LEVEL $level; SELECT count(*) FROM c; COMMIT");
		is($ret, 0, "lap $lap: a $level read of the table works") or diag($err);
		like($out, qr/200[0-9]/, "lap $lap: and sees every row");
	}
	$node->safe_psql('postgres', "INSERT INTO c VALUES (-$lap, 'lap')");
	$hold->query_safe('ROLLBACK');
	$hold->quit;

	is($node->safe_psql('postgres', q{SELECT relfrozenxid::text::bigint > 2 FROM pg_class WHERE relname = 'c'}),
		't', "lap $lap: relfrozenxid of the table is a real id");
	if ($has_fn eq '1')
	{
		is($node->safe_psql('postgres', q{SELECT kingres.columnar_creating_xid('c')::text::bigint}),
			'2', "lap $lap: the id in the metapage is frozen");
	}
	$node->safe_psql('postgres', 'VACUUM c');
	is($node->safe_psql('postgres', 'SELECT count(*) FROM c'), 2000 + $lap,
		"lap $lap: the table has its rows");
}

$node->stop;
done_testing();
