# contrib/kingres_columnar/t/055_unique_build.pl
#
# C6c: the interplay of a unique index with other transactions.
#  1. CREATE UNIQUE INDEX while another transaction holds uncommitted rows: it waits for
#     that transaction (the build's table lock conflicts with the writer's), and then
#     sees the rows if they committed (a duplicate is reported) and not if they did not.
#     A writer that starts while the build runs waits for the build and then meets the
#     index.
#  2. Savepoints: a key of a rolled-back subtransaction can be taken again in the same
#     transaction and by another one; a duplicate inside a subtransaction is an error
#     that the savepoint undoes.
#  3. A waiter on a key whose writer writes more than a stripe before it commits (its
#     rows are flushed meanwhile) is woken by the commit and gets the duplicate error;
#     if the writer rolls back the waiter takes the key.
#  4. Two transactions that take two keys in opposite order deadlock, as with a heap
#     table: one of them gets the deadlock error and the other commits.
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

my $node = new_node('ubuild', conf => "deadlock_timeout = 200ms\n");

sub sess { return $node->background_psql('postgres', on_error_stop => 0); }

# run a statement in a session and return what it printed to stderr
sub run
{
	my ($s, $sql) = @_;
	$s->{stderr} = '';
	$s->query($sql);
	return $s->{stderr};
}

# start a statement that blocks; returns at once
sub start_blocking
{
	my ($s, $sql) = @_;
	$s->{stderr} = '';
	$s->query_until(qr/^started$/m, "\\echo started\n$sql\n");
}

# wait until $n backends wait for a lock
sub wait_lock
{
	my ($n) = @_;
	for (my $i = 0; $i < 400; $i++)
	{
		return 1 if q1($node, "SELECT count(*) FROM pg_stat_activity WHERE wait_event_type = 'Lock'") >= $n;
		select(undef, undef, undef, 0.05);
	}
	return 0;
}

# finish a blocked statement: wait for it and return its stderr
sub finish { my ($s) = @_; $s->query("SELECT 'sync';"); return $s->{stderr}; }

# ---------------------------------------------------------------------------
# 1. the build and a writer
# ---------------------------------------------------------------------------
q1($node, q{CREATE TABLE b1 (k int, v text) USING columnar; INSERT INTO b1 SELECT g, 'old' FROM generate_series(1, 100) g;});

# 1a: the writer commits a duplicate while the build waits
my ($w, $b) = (sess(), sess());
run($w, "BEGIN; INSERT INTO b1 VALUES (50, 'dup'), (500, 'new');");
start_blocking($b, "CREATE UNIQUE INDEX b1_k ON b1 (k);");
ok(wait_lock(1), '1a: the build waits for the writer');
run($w, "COMMIT;");
like(finish($b), qr/could not create unique index.*\n.*Key \(k\)=\(50\) is duplicated/s, '1a: the build sees the writer\'s rows and reports the duplicate');
is(q1($node, "SELECT count(*) FROM pg_indexes WHERE indexname = 'b1_k'"), 0, '1a: no index was left');

# 1b: the writer rolls back: the build succeeds and the later insert meets it
q1($node, q{CREATE TABLE b2 (k int, v text) USING columnar; INSERT INTO b2 SELECT g, 'old' FROM generate_series(1, 100) g;});
run($w, "BEGIN; INSERT INTO b2 VALUES (50, 'dup'), (501, 'new');");
start_blocking($b, "CREATE UNIQUE INDEX b2_k ON b2 (k);");
ok(wait_lock(1), '1b: the build waits for the writer');
run($w, "ROLLBACK;");
is(finish($b), '', '1b: the build succeeds');
like(run($w, "INSERT INTO b2 VALUES (50, 'x');"), qr/duplicate key/, '1b: a duplicate of an old row is refused');
is(run($w, "INSERT INTO b2 VALUES (501, 'x');"), '', '1b: the key of the rolled-back writer is free');
is(q1($node, "SELECT count(*) FROM kingres.columnar_check_indexes('b2')"), 0, '1b: the index is complete');

# 1c: a writer that arrives during the build waits for it and then meets the index
q1($node, "CREATE TABLE b3 (k int, v text) USING columnar; INSERT INTO b3 SELECT g, 'old' FROM generate_series(1, 100) g;");
my $lock = sess();
run($lock, "BEGIN; LOCK TABLE b3 IN ROW EXCLUSIVE MODE;");    # a stand-in for a writer: the build waits for it
start_blocking($b, "CREATE UNIQUE INDEX b3_k ON b3 (k);");
ok(wait_lock(1), '1c: the build waits');
start_blocking($w, "INSERT INTO b3 VALUES (7, 'late');");
ok(wait_lock(2), '1c: a writer that arrives waits behind the build');
run($lock, "COMMIT;");
is(finish($b), '', '1c: the build succeeds');
like(finish($w), qr/duplicate key/, '1c: the late writer meets the index');
is(q1($node, "SELECT count(*) FROM b3"), 100, '1c: its row is not there');

# ---------------------------------------------------------------------------
# 2. savepoints
# ---------------------------------------------------------------------------
q1($node, "CREATE TABLE sp (k int, v text) USING columnar; CREATE UNIQUE INDEX sp_k ON sp (k);");
my ($s1, $s2) = (sess(), sess());
run($s1, "BEGIN; INSERT INTO sp VALUES (1, 'a'); SAVEPOINT x; INSERT INTO sp VALUES (2, 'b');");
like(run($s1, "INSERT INTO sp VALUES (2, 'again');"), qr/duplicate key/, '2: a duplicate inside the transaction is refused');
run($s1, "ROLLBACK TO x;");
is(run($s1, "INSERT INTO sp VALUES (2, 'second');"), '', '2: after the savepoint is undone the key is free');
run($s1, "SAVEPOINT y; INSERT INTO sp VALUES (3, 'c'); ROLLBACK TO y; INSERT INTO sp VALUES (3, 'c2');");
is(run($s1, "COMMIT;"), '', '2: commit');
is(q1($node, "SELECT string_agg(k || v, ',' ORDER BY k) FROM sp"), '1a,2second,3c2', '2: the rows that survive');
# a key of a rolled-back subtransaction is free for another transaction at once
run($s1, "BEGIN; SAVEPOINT z; INSERT INTO sp VALUES (10, 'z'); ROLLBACK TO z;");
is(run($s2, "INSERT INTO sp VALUES (10, 'other');"), '', '2: another session takes a key a savepoint gave up');
run($s1, "COMMIT;");
like(run($s1, "INSERT INTO sp VALUES (10, 'last');"), qr/duplicate key/, '2: ... and it keeps it');
# a savepoint that does not roll back, and a nested one that does
run($s1, "BEGIN; SAVEPOINT a; INSERT INTO sp VALUES (20, 'a'); SAVEPOINT b; INSERT INTO sp VALUES (21, 'b'); ROLLBACK TO b; RELEASE a; COMMIT;");
is(q1($node, "SELECT count(*) FROM sp WHERE k IN (20, 21)"), 1, '2: only the key outside the rolled-back nested savepoint is there');
is(q1($node, "SELECT count(*) - count(DISTINCT k) FROM sp"), 0, '2: no key twice');

# ---------------------------------------------------------------------------
# 3. a waiter and a writer that flushes
# ---------------------------------------------------------------------------
q1($node, "CREATE TABLE wf (k int, v text) USING columnar; CREATE UNIQUE INDEX wf_k ON wf (k);
	SELECT kingres.columnar_set_options('wf', stripe_row_count => 1000, chunk_row_count => 100);");
for my $mode ('commit', 'rollback')
{
	my $base = $mode eq 'commit' ? 0 : 100000;
	run($s1, "BEGIN; INSERT INTO wf VALUES ($base + 1, 'first');");
	start_blocking($s2, "INSERT INTO wf VALUES ($base + 1, 'waiter');");
	ok(wait_lock(1), "3 ($mode): the waiter waits for the writer");
	# the writer goes over several stripes while the waiter waits
	is(run($s1, "INSERT INTO wf SELECT $base + g, 'bulk' FROM generate_series(2, 4500) g;"), '', "3 ($mode): the writer flushes stripes meanwhile");
	run($s1, $mode eq 'commit' ? "COMMIT;" : "ROLLBACK;");
	my $err = finish($s2);
	if ($mode eq 'commit') { like($err, qr/duplicate key/, '3 (commit): the waiter gets the duplicate error'); }
	else { is($err, '', '3 (rollback): the waiter takes the key'); }
	is(q1($node, "SELECT count(*) FROM wf WHERE k = $base + 1"), 1, "3 ($mode): the key is in the table once");
}
is(q1($node, "SELECT count(*) - count(DISTINCT k) FROM wf"), 0, '3: no key twice');
is(q1($node, "SELECT count(*) FROM kingres.columnar_check_indexes('wf')"), 0, '3: the index is complete');

# ---------------------------------------------------------------------------
# 4. a deadlock
# ---------------------------------------------------------------------------
q1($node, "CREATE TABLE dl (k int, v text) USING columnar; CREATE UNIQUE INDEX dl_k ON dl (k);");
run($s1, "BEGIN; INSERT INTO dl VALUES (1, 's1');");
run($s2, "BEGIN; INSERT INTO dl VALUES (2, 's2');");
start_blocking($s1, "INSERT INTO dl VALUES (2, 's1');");
ok(wait_lock(1), '4: the first waits');
my $e2 = run($s2, "INSERT INTO dl VALUES (1, 's2');");
my $e1 = finish($s1);
my $dead = ($e1 =~ /deadlock detected/ ? 1 : 0) + ($e2 =~ /deadlock detected/ ? 1 : 0);
is($dead, 1, '4: exactly one of them gets the deadlock error');
run($s1, "COMMIT;");
run($s2, "COMMIT;");
is(q1($node, "SELECT count(*) - count(DISTINCT k) FROM dl"), 0, '4: no key twice');
cmp_ok(q1($node, "SELECT count(*) FROM dl"), '>=', 1, '4: the survivor committed');

$_->quit for ($w, $b, $lock, $s1, $s2);
is(q1($node, "SELECT count(*) FROM kingres.columnar_check()"), 0, 'columnar_check has nothing to report');
$node->stop;
done_testing();
