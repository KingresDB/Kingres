# contrib/kingres_columnar/t/010_visibility.pl
#
# What a statement sees of the stripes the running transaction changed
# itself, and what older snapshots see through the commands that rewrite a
# table.
#
# The stripe catalog is read with the extension's own copy of the heap
# visibility rules.  For the transaction's own deletions they must be those of
# HeapTupleSatisfiesMVCC: a row deleted by an earlier command is gone, a row
# deleted by the reader's own command is still there.  The only command that
# deletes catalog rows of a table is columnar_compact, and a heap twin gives
# the answer to compare with.
#
# Portions Copyright (c) 2026, RooyeKhat Media Co
# SPDX-License-Identifier: AGPL-3.0-only

use strict;
use warnings FATAL => 'all';

use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;

my $node = PostgreSQL::Test::Cluster->new('colvis');
$node->init;
$node->append_conf('postgresql.conf', q{
shared_preload_libraries = 'kingres_columnar'
max_prepared_transactions = 5
});
$node->start;

$node->safe_psql('postgres', q{
CREATE EXTENSION kingres_columnar;
CREATE TABLE c (a int) USING columnar;
INSERT INTO c SELECT generate_series(1,1000);
INSERT INTO c SELECT generate_series(1,1000);
INSERT INTO c SELECT generate_series(1,1000);
CREATE TABLE h (a int);
INSERT INTO h SELECT generate_series(1,3000);
CREATE FUNCTION cnt_c() RETURNS bigint LANGUAGE sql STABLE AS 'SELECT count(*) FROM c';
CREATE FUNCTION cnt_cv() RETURNS bigint LANGUAGE sql VOLATILE AS 'SELECT count(*) FROM c';
CREATE FUNCTION cnt_h() RETURNS bigint LANGUAGE sql STABLE AS 'SELECT count(*) FROM h';
});

# C2-04: one statement that compacts and reads
my $heap = $node->safe_psql('postgres',
	'WITH d AS (DELETE FROM h RETURNING 1) SELECT count(*) || chr(124) || cnt_h() FROM d');
is($heap, '3000|3000', 'heap twin: a statement that deletes still sees the rows');
is($node->safe_psql('postgres',
	"SELECT kingres.columnar_compact('c', 100000), cnt_c()"),
	'1|3000', 'compaction and a STABLE reader in one statement: the rows are seen');
is($node->safe_psql('postgres', 'SELECT cnt_c()'), '3000',
	'the next statement sees them too');

# a second table in the same state, for the volatile reader
$node->safe_psql('postgres', q{
TRUNCATE c;
INSERT INTO c SELECT generate_series(1,1000);
INSERT INTO c SELECT generate_series(1,1000);
INSERT INTO c SELECT generate_series(1,1000);});
is($node->safe_psql('postgres',
	"SELECT kingres.columnar_compact('c', 100000), cnt_cv()"),
	'1|3000', 'compaction and a VOLATILE reader in one statement');

# two compactions in one statement do not trip over each other's deletions
$node->safe_psql('postgres', q{
TRUNCATE c;
INSERT INTO c SELECT generate_series(1,1000);
INSERT INTO c SELECT generate_series(1,1000);
INSERT INTO c SELECT generate_series(1,1000);});
is($node->safe_psql('postgres',
	"SELECT kingres.columnar_compact('c', 100000), kingres.columnar_compact('c', 100000), cnt_c()"),
	'1|0|3000', 'two compactions in one statement');

# the own deletion in a subtransaction that is rolled back, and one released
is($node->safe_psql('postgres', q{
BEGIN;
SAVEPOINT s;
SELECT kingres.columnar_compact('c', 100000) > -1;
SELECT count(*) FROM c;
ROLLBACK TO s;
SELECT count(*) FROM c;
SAVEPOINT s2;
INSERT INTO c VALUES (1);
SELECT kingres.columnar_compact('c', 100000) > -1;
RELEASE s2;
SELECT count(*) FROM c;
COMMIT;
SELECT count(*) FROM c;}),
	"t\n3000\n3000\nt\n3001\n3001", 'compaction in subtransactions: counts stay right');

# ---------------------------------------------------------------------------
# VACUUM FULL keeps what older snapshots see (C2-05)
# ---------------------------------------------------------------------------
$node->safe_psql('postgres', q{
CREATE TABLE other (x int);
CREATE TABLE v (a int, b text) USING columnar;
INSERT INTO v SELECT g, 'x' || g FROM generate_series(1, 1000) g;
CREATE TABLE res (n bigint);
CREATE FUNCTION slow_count() RETURNS bigint LANGUAGE plpgsql STABLE AS $f$
BEGIN
  PERFORM pg_advisory_lock(4242);
  PERFORM pg_advisory_unlock(4242);
  RETURN (SELECT count(*) FROM v);
END $f$;
});

sub run_in
{
	my ($s, $sql) = @_;
	$s->{stderr} = '';
	my $out = $s->query($sql);
	my $err = $s->{stderr};
	$s->{stderr} = '';
	return ($out, $err);
}

foreach my $level ('REPEATABLE READ', 'SERIALIZABLE')
{
	my $a = $node->background_psql('postgres', on_error_stop => 0);
	$a->query_safe("BEGIN ISOLATION LEVEL $level");
	$a->query_safe("SELECT count(*) FROM other");
	$node->safe_psql('postgres', 'VACUUM FULL v');
	my ($out, $err) = run_in($a, "SELECT count(*) FROM v");
	is($err, '', "$level: an older snapshot reads the table after VACUUM FULL");
	like($out, qr/^\s*1000\s*$/m, "$level: and sees every row");
	$a->query("COMMIT");
	$a->quit;
}

# a READ COMMITTED statement whose function opens the table after the command
{
	my $holder = $node->background_psql('postgres', on_error_stop => 0);
	$holder->query_safe("SELECT pg_advisory_lock(4242)");
	my $a = $node->background_psql('postgres', on_error_stop => 0);
	$a->query_until(qr/started/, '\echo started' . "\nINSERT INTO res SELECT slow_count();\n");
	$node->safe_psql('postgres', 'VACUUM FULL v');
	$holder->query_safe("SELECT pg_advisory_unlock(4242)");
	my ($out, $err) = run_in($a, "SELECT 1");
	is($err, '', 'READ COMMITTED statement over a STABLE function: no error after VACUUM FULL');
	is($node->safe_psql('postgres', 'SELECT n FROM res'), '1000',
		'and it read the table');
	$a->quit;
	$holder->quit;
}

# rows newer than the snapshot: heap would show the snapshot the old rows
# only; here the snapshot gets an error, never a wrong answer
{
	my $a = $node->background_psql('postgres', on_error_stop => 0);
	$a->query_safe("BEGIN ISOLATION LEVEL REPEATABLE READ");
	$a->query_safe("SELECT count(*) FROM other");
	$node->safe_psql('postgres', q{INSERT INTO v SELECT g, 'y' FROM generate_series(1, 500) g});
	$node->safe_psql('postgres', 'VACUUM FULL v');
	my ($out, $err) = run_in($a, "SELECT count(*) FROM v");
	like($err, qr/could not read columnar table "v": it was rewritten or truncated/,
		'a snapshot older than some stripes gets the serialization failure');
	unlike($out, qr/^\s*1000\s*$/m, 'and no wrong count');
	$a->query("COMMIT");
	$a->quit;
	is($node->safe_psql('postgres', 'SELECT count(*) FROM v'), '1500', 'the table has all its rows');
	is($node->safe_psql('postgres', 'SELECT kingres.columnar_reconcile() >= 0; SELECT count(*) FROM kingres.columnar_check()'), "t
0",
		'columnar_check is clean');
}

# ---------------------------------------------------------------------------
# The transaction id of the metapage is frozen by VACUUM (C2-06)
# ---------------------------------------------------------------------------
$node->safe_psql('postgres', q{CREATE TABLE f (a int) USING columnar; INSERT INTO f VALUES (1)});
my $x = $node->safe_psql('postgres', q{SELECT kingres.columnar_creating_xid('f')::text::bigint});
cmp_ok($x, '>', 2, 'a new file holds the id of its creating transaction');
my $rfx = $node->safe_psql('postgres',
	q{SELECT relfrozenxid::text::bigint FROM pg_class WHERE relname = 'f'});
cmp_ok($rfx, '>', 0, 'relfrozenxid of a columnar table is a real id');
cmp_ok($rfx, '<=', $x, 'and does not pass the id the file holds');

# an open transaction older than the id keeps it
{
	my $a = $node->background_psql('postgres', on_error_stop => 0);
	$a->query_safe("BEGIN ISOLATION LEVEL REPEATABLE READ");
	$a->query_safe("SELECT count(*) FROM other");
	$node->safe_psql('postgres', q{CREATE TABLE f2 (a int) USING columnar; INSERT INTO f2 VALUES (1)});
	my $x2 = $node->safe_psql('postgres', q{SELECT kingres.columnar_creating_xid('f2')::text::bigint});
	$node->safe_psql('postgres', 'VACUUM f2');
	is($node->safe_psql('postgres', q{SELECT kingres.columnar_creating_xid('f2')::text::bigint}),
		$x2, 'VACUUM keeps the id while an older snapshot exists');
	cmp_ok($node->safe_psql('postgres',
			q{SELECT relfrozenxid::text::bigint FROM pg_class WHERE relname = 'f2'}),
		'<=', $x2, 'and relfrozenxid does not pass it');
	$a->query("COMMIT");
	$a->quit;
	$node->safe_psql('postgres', 'VACUUM f2');
	is($node->safe_psql('postgres', q{SELECT kingres.columnar_creating_xid('f2')::text::bigint}),
		'2', 'VACUUM freezes it once every snapshot can see it');
	cmp_ok($node->safe_psql('postgres',
			q{SELECT relfrozenxid::text::bigint FROM pg_class WHERE relname = 'f2'}),
		'>', $x2, 'and relfrozenxid moves on');
}
$node->safe_psql('postgres', 'VACUUM f');
is($node->safe_psql('postgres', q{SELECT kingres.columnar_creating_xid('f')::text::bigint}),
	'2', 'the id of the first table is frozen too');
is($node->safe_psql('postgres', 'SELECT count(*) FROM f'), '1', 'and the table reads');

# TRUNCATE and the rewrites that change rows start a new file with a new id
$node->safe_psql('postgres', 'TRUNCATE f; INSERT INTO f VALUES (2)');
cmp_ok($node->safe_psql('postgres', q{SELECT kingres.columnar_creating_xid('f')::text::bigint}),
	'>', 2, 'TRUNCATE gives the new file the id of its transaction');
$node->safe_psql('postgres', 'VACUUM FULL f');
is($node->safe_psql('postgres', q{SELECT kingres.columnar_creating_xid('f')::text::bigint}),
	'2', 'a VACUUM FULL that kept what every snapshot sees leaves no id');
cmp_ok($node->safe_psql('postgres',
		q{SELECT relfrozenxid::text::bigint FROM pg_class WHERE relname = 'f'}),
	'>', 2, 'relfrozenxid of the new file is a real id');
$node->stop;
done_testing();
