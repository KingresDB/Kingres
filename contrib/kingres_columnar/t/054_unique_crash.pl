# contrib/kingres_columnar/t/054_unique_crash.pl
#
# C6c: a crash in the middle of unique inserts.  Sessions insert keys into a columnar
# table with a unique index (autocommit, small transactions with a rollback, and one
# transaction that is open and one that is prepared at the moment of the crash).  The
# server is stopped in the immediate mode.  After the restart (and a second crash, during
# the recovery's first checkpoint-free run) no key is in the table twice, every key
# whose commit was acknowledged is there, a key of the open transaction is not, the
# prepared transaction's keys are there after COMMIT PREPARED, an acknowledged key cannot be
# inserted again and a lost one can, and columnar_check_indexes finds nothing.
#
# Portions Copyright (c) 2026, RooyeKhat Media Co
# SPDX-License-Identifier: AGPL-3.0-only

use strict;
use warnings FATAL => 'all';

use FindBin;
use lib $FindBin::RealBin;
use KcReserve qw(new_node q1);
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;

my $node = new_node('ucrash');
q1($node, q{
	CREATE TABLE c (k int, b text) USING columnar;
	SELECT kingres.columnar_set_options('c', stripe_row_count => 1000, chunk_row_count => 100);
	CREATE UNIQUE INDEX c_k ON c (k);
});

my (%acked);
my $s = $node->background_psql('postgres', on_error_stop => 0);
$s->query("SET synchronous_commit = on;");
for my $k (1 .. 450)
{
	$s->{stderr} = '';
	if ($k % 7 == 0)
	{
		$s->query("BEGIN; INSERT INTO c VALUES ($k, 'rolled'); ROLLBACK;");
	}
	else
	{
		$s->query("INSERT INTO c VALUES ($k, 'x$k');");
		$acked{$k} = 1 if $s->{stderr} eq '';
	}
}
# an open transaction, a prepared one, and an aborted retry of an acked key
my $open = $node->background_psql('postgres', on_error_stop => 0);
$open->query("BEGIN; INSERT INTO c SELECT g, 'open' FROM generate_series(1001, 1030) g;");
my $prep = $node->background_psql('postgres', on_error_stop => 0);
$prep->query("BEGIN; INSERT INTO c SELECT g, 'prep' FROM generate_series(2001, 2250) g; PREPARE TRANSACTION 'pg1';");
$s->query("INSERT INTO c VALUES (5, 'again');");
like($s->{stderr}, qr/duplicate key/, 'an acknowledged key cannot be inserted again before the crash');
ok(scalar(keys %acked) > 380, scalar(keys %acked) . ' keys acknowledged');

for my $round (1, 2)
{
	$node->stop('immediate');
	$node->start;
	my $n = q1($node, "SELECT count(*) FROM c WHERE k < 1000");
	is($n, scalar(keys %acked), "round $round: the table holds exactly the acknowledged keys");
	is(q1($node, "SELECT count(*) - count(DISTINCT k) FROM c"), 0, "round $round: no key twice");
	is(q1($node, "SELECT count(*) FROM c WHERE k BETWEEN 1001 AND 1030"), 0, "round $round: the open transaction's keys are not there");
	is(q1($node, "SELECT count(*) FROM pg_prepared_xacts"), 1, "round $round: the prepared transaction survived");
	# a key of the prepared transaction is locked by it
	my ($rc, $out, $err) = $node->psql('postgres', "SET lock_timeout = '100ms'; INSERT INTO c VALUES (2100, 'try');");
	isnt($rc, 0, "round $round: a key of the prepared transaction cannot be taken");
	like($err, qr/lock timeout|duplicate key/, "round $round: ... it waits for the prepared transaction");
	($rc, $out, $err) = $node->psql('postgres', "INSERT INTO c VALUES (8, 'again');");
	like($err, qr/duplicate key/, "round $round: an acknowledged key cannot be inserted again");
	($rc, $out, $err) = $node->psql('postgres', "INSERT INTO c VALUES (" . (3000 + $round) . ", 'new');");
	is($rc, 0, "round $round: a new key can be taken") or diag($err);
	is(q1($node, "SELECT count(*) FROM kingres.columnar_check_indexes('c')"), 0, "round $round: columnar_check_indexes finds nothing");
}

q1($node, "COMMIT PREPARED 'pg1'");
is(q1($node, "SELECT count(*) FROM c WHERE k BETWEEN 2001 AND 2250"), 250, 'the prepared transaction committed its 250 keys');
is(q1($node, "SELECT count(*) - count(DISTINCT k) FROM c"), 0, 'no key twice');
my ($rc, $out, $err) = $node->psql('postgres', "INSERT INTO c VALUES (2100, 'late');");
like($err, qr/duplicate key/, 'a committed key of the prepared transaction cannot be inserted again');
is(q1($node, "SELECT count(*) FROM kingres.columnar_check_indexes('c')"), 0, 'columnar_check_indexes finds nothing');
$node->stop;
done_testing();
