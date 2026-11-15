# contrib/kingres_columnar/t/007_snapshots.pl
#
# Older snapshots against commands that replace a table's storage, and
# SERIALIZABLE transactions.
#
# A REPEATABLE READ transaction whose snapshot is older than a TRUNCATE or a
# rewriting ALTER TABLE of a columnar table must not be handed an empty table:
# its read fails with a serialization failure that names the rewrite.
# VACUUM FULL, which keeps the content, keeps it for them too when every
# stripe is older than the snapshot (t/010_visibility.pl has the other case).
# SET TABLESPACE copies the file unchanged and so changes nothing for anyone;
# columnar_compact keeps the old stripes visible.
#
# Under SERIALIZABLE, transactions that use different columnar tables must
# not abort each other, while a real write skew on one table is detected.
#
# Portions Copyright (c) 2026, RooyeKhat Media Co
# SPDX-License-Identifier: AGPL-3.0-only

use strict;
use warnings FATAL => 'all';

use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;

my $node = PostgreSQL::Test::Cluster->new('colsnap');
$node->init;
$node->append_conf('postgresql.conf', q{
shared_preload_libraries = 'kingres_columnar'
max_prepared_transactions = 5
});
$node->start;

my $tsdir = PostgreSQL::Test::Utils::tempdir;
$node->safe_psql('postgres', "
CREATE EXTENSION kingres_columnar;
CREATE TABLE other (x int);
CREATE TABLESPACE snap_ts LOCATION '$tsdir';
");

# Run one statement in a background session; return its output and its
# error text (empty when it succeeded).
sub run_in
{
	my ($s, $sql) = @_;
	$s->{stderr} = '';
	my $out = $s->query($sql);
	my $err = $s->{stderr};
	$s->{stderr} = '';
	return ($out, $err);
}

# A snapshot is taken by a first query on an unrelated table; the command
# under test then runs in another session; the old snapshot reads the table.
sub old_snapshot_case
{
	my ($name, $setup, $command, $expect_error) = @_;

	$node->safe_psql('postgres', "DROP TABLE IF EXISTS c; $setup");
	my $a = $node->background_psql('postgres', on_error_stop => 0);
	$a->query_safe("BEGIN ISOLATION LEVEL REPEATABLE READ");
	$a->query_safe("SELECT count(*) FROM other");
	$node->safe_psql('postgres', $command);
	my ($out, $err) = run_in($a, "SELECT count(*) FROM c");
	if ($expect_error)
	{
		like($err, qr/could not read columnar table "c": it was rewritten or truncated/,
			"$name: the older snapshot gets an error, not an empty table");
		unlike($out, qr/^\s*0\s*$/m, "$name: no empty result");
	}
	else
	{
		is($err, '', "$name: no error");
		like($out, qr/^\s*1000\s*$/m, "$name: the older snapshot still sees every row");
	}
	$a->query("COMMIT");
	$a->quit;
	# a new snapshot sees the data in every case
	is($node->safe_psql('postgres', "SELECT count(*) FROM c"), '1000',
		"$name: the table has its rows afterwards");
}

my $load = "CREATE TABLE c (a int, b text) USING columnar;
            INSERT INTO c SELECT g, 'v' || g FROM generate_series(1, 1000) g;";

# VACUUM FULL keeps what the table contains and every stripe predates the snapshot
old_snapshot_case('VACUUM FULL', $load, 'VACUUM FULL c', 0);
old_snapshot_case('SET TABLESPACE', $load, 'ALTER TABLE c SET TABLESPACE snap_ts', 0);
old_snapshot_case('columnar_compact',
	"CREATE TABLE c (a int, b text) USING columnar;
	 INSERT INTO c SELECT g, 'x' FROM generate_series(1, 500) g;
	 INSERT INTO c SELECT g, 'y' FROM generate_series(501, 1000) g;",
	"SELECT kingres.columnar_compact('c', 100000)", 0);
old_snapshot_case('rewriting ALTER TABLE', $load,
	'ALTER TABLE c ALTER COLUMN a TYPE bigint', 1);
old_snapshot_case('SET ACCESS METHOD heap to columnar',
	"CREATE TABLE c (a int, b text);
	 INSERT INTO c SELECT g, 'v' || g FROM generate_series(1, 1000) g;",
	'ALTER TABLE c SET ACCESS METHOD columnar', 1);

# TRUNCATE: the old snapshot errors; the table is then empty for new ones
{
	$node->safe_psql('postgres', "DROP TABLE IF EXISTS c; $load");
	my $a = $node->background_psql('postgres', on_error_stop => 0);
	$a->query_safe("BEGIN ISOLATION LEVEL REPEATABLE READ");
	$a->query_safe("SELECT count(*) FROM other");
	$node->safe_psql('postgres', 'TRUNCATE c');
	my ($out, $err) = run_in($a, "SELECT count(*) FROM c");
	like($err, qr/could not read columnar table "c"/, 'TRUNCATE: the older snapshot gets an error');
	$a->query("COMMIT");
	$a->quit;
	is($node->safe_psql('postgres', "SELECT count(*) FROM c"), '0',
		'TRUNCATE: the table is empty for a new snapshot');
}

# A transaction that began after the command is not affected
{
	$node->safe_psql('postgres', "DROP TABLE IF EXISTS c; $load");
	$node->safe_psql('postgres', 'VACUUM FULL c');
	my $a = $node->background_psql('postgres', on_error_stop => 0);
	$a->query_safe("BEGIN ISOLATION LEVEL REPEATABLE READ");
	my ($out, $err) = run_in($a, "SELECT count(*) FROM c");
	is($err, '', 'a snapshot taken after VACUUM FULL reads normally');
	like($out, qr/^\s*1000\s*$/m, 'and sees all rows');
	$a->query("COMMIT");
	$a->quit;
}

# SERIALIZABLE: unrelated tables do not conflict
$node->safe_psql('postgres', "
DROP TABLE IF EXISTS a1, b1, a2, b2;
CREATE TABLE a1 (a int) USING columnar; CREATE TABLE b1 (a int) USING columnar;
CREATE TABLE a2 (a int) USING columnar; CREATE TABLE b2 (a int) USING columnar;
INSERT INTO a1 VALUES (1); INSERT INTO a2 VALUES (1);");
{
	my $s1 = $node->background_psql('postgres', on_error_stop => 0);
	my $s2 = $node->background_psql('postgres', on_error_stop => 0);
	$s1->query_safe("BEGIN ISOLATION LEVEL SERIALIZABLE");
	$s1->query_safe("SELECT count(*) FROM a1");
	$s2->query_safe("BEGIN ISOLATION LEVEL SERIALIZABLE");
	$s2->query_safe("SELECT count(*) FROM a2");
	$s1->query_safe("INSERT INTO b1 VALUES (1)");
	$s2->query_safe("INSERT INTO b2 VALUES (2)");
	my (undef, $e2) = run_in($s2, "COMMIT");
	my (undef, $e1) = run_in($s1, "COMMIT");
	is($e2, '', 'serializable, unrelated columnar tables: first commit succeeds');
	is($e1, '', 'serializable, unrelated columnar tables: second commit succeeds');
	$s1->quit;
	$s2->quit;
}

# SERIALIZABLE: a real write skew on one columnar table is still detected
$node->safe_psql('postgres', "
DROP TABLE IF EXISTS w;
CREATE TABLE w (a int) USING columnar;
INSERT INTO w VALUES (0);");
{
	my $s1 = $node->background_psql('postgres', on_error_stop => 0);
	my $s2 = $node->background_psql('postgres', on_error_stop => 0);
	$s1->query_safe("BEGIN ISOLATION LEVEL SERIALIZABLE");
	$s1->query_safe("SELECT count(*) FROM w");
	$s2->query_safe("BEGIN ISOLATION LEVEL SERIALIZABLE");
	$s2->query_safe("SELECT count(*) FROM w");
	my ($o1, $e1a) = run_in($s1, "INSERT INTO w VALUES (1)");
	my ($o2, $e2a) = run_in($s2, "INSERT INTO w VALUES (2)");
	my (undef, $e1b) = run_in($s1, "COMMIT");
	my (undef, $e2b) = run_in($s2, "COMMIT");
	my $failed = join('', $e1a, $e2a, $e1b, $e2b) =~ /could not serialize access/;
	ok($failed, 'a write skew between two transactions on one columnar table is detected');
	$s1->quit;
	$s2->quit;
	my $n = $node->safe_psql('postgres', "SELECT count(*) FROM w");
	cmp_ok($n, '<', 3, 'at most one of the two inserts committed');
}

$node->stop;
done_testing();
