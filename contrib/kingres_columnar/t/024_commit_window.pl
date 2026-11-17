# contrib/kingres_columnar/t/024_commit_window.pl
#
# C6-01.  A row that an insert accepted without an error is committed with its
# transaction, whatever code writes it and in whatever order the libraries were
# loaded.  The defect: the access method writes its buffers in its own pre-commit
# callback, and a callback of another module that runs after it (a module that is
# listed before it in shared_preload_libraries) wrote into a buffer that nobody
# wrote any more: the row was dropped at commit without an error.
#
# The probe module (t/kcprobe/kcprobe.c) runs a statement from the setting
# kcprobe.commit_sql at the pre-commit and the pre-prepare.  The statement writes
# the same rows into a columnar table and into a heap twin; the two are compared
# after the commit, with the module loaded before the access method (its callback
# runs after the access method's flush) and after it (before).  Deletes and
# updates arriving there are refused with an error (the table is append-only) and
# nothing of the transaction is committed.  A columnar table cannot be temporary,
# so ON COMMIT DELETE ROWS and ON COMMIT DROP, the other actions that PostgreSQL
# runs after the pre-commit callbacks, cannot reach one.
#
# It needs a compiler and the server's headers; where it cannot be built the
# test is skipped with the reason.
#
# Portions Copyright (c) 2026, RooyeKhat Media Co
# SPDX-License-Identifier: AGPL-3.0-only

use strict;
use warnings FATAL => 'all';

use FindBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;

my $src = "$FindBin::RealBin/kcprobe/kcprobe.c";
my $dir = PostgreSQL::Test::Utils::tempdir();
my $pg_config = $ENV{PG_CONFIG} || 'pg_config';
my $inc = `$pg_config --includedir-server 2>/dev/null`;
chomp $inc;
my $cc = `$pg_config --cc 2>/dev/null`;
chomp $cc;
$cc = 'cc' if $cc eq '';

my $built = 0;
if ($inc ne '' && -f $src && -d $inc)
{
	my $out = `$cc -shared -fPIC -I"$inc" -o "$dir/kcprobe.so" "$src" 2>&1`;
	$built = ($? == 0 && -f "$dir/kcprobe.so");
	diag("building the probe module failed: $out") unless $built;
}
plan skip_all => 'the probe module (t/kcprobe/kcprobe.c) could not be built here: it needs a C compiler and the server headers'
  unless $built;

foreach my $order ('after', 'before')
{
	# "after": the probe's callback runs after the access method's flush
	my $libs = $order eq 'after' ? "$dir/kcprobe,kingres_columnar" : "kingres_columnar,$dir/kcprobe";
	my $node = PostgreSQL::Test::Cluster->new("colcommit_$order");
	$node->init;
	$node->append_conf('postgresql.conf', "shared_preload_libraries = '$libs'\nmax_prepared_transactions = 10\n");
	$node->start;
	$node->safe_psql('postgres', 'CREATE EXTENSION kingres_columnar');

	my $run = sub
	{
		my ($sql) = @_;
		my ($out, $err) = ('', '');
		$node->psql('postgres', $sql, stdout => \$out, stderr => \$err, on_error_stop => 0);
		return ($out, $err);
	};
	my $q = sub { my ($sql) = @_; return $node->safe_psql('postgres', $sql); };
	my $fresh = sub
	{
		$q->("DROP TABLE IF EXISTS p, q, h, hq CASCADE;
			CREATE TABLE p (a int, b text) USING columnar;
			CREATE TABLE q (a int, b text) USING columnar;
			CREATE TABLE h (a int, b text);
			CREATE TABLE hq (a int, b text);");
	};
	my $same = sub
	{
		my ($name) = @_;
		my $cp = $q->("SELECT count(*) || ':' || coalesce(md5(string_agg(a || coalesce(b, '-'), ',' ORDER BY a, b)), '') FROM p");
		my $ch = $q->("SELECT count(*) || ':' || coalesce(md5(string_agg(a || coalesce(b, '-'), ',' ORDER BY a, b)), '') FROM h");
		is($cp, $ch, "$order: $name: the columnar table holds what the heap twin holds");
		return $cp;
	};

	# the statement the callback runs writes both tables
	my $both = sub { my ($vals) = @_; return "INSERT INTO p $vals; INSERT INTO h $vals"; };

	$fresh->();
	my ($out, $err) = $run->("
BEGIN; SET LOCAL kcprobe.commit_sql = '" . $both->("VALUES (777, \$\$late\$\$), (778, NULL)") . "';
INSERT INTO p VALUES (1, 'x'); INSERT INTO h VALUES (1, 'x'); COMMIT;");
	unlike($err, qr/ERROR/, "$order: a row written by a pre-commit callback: no error");
	is($q->("SELECT count(*) FROM p"), 3, "$order: ... the commit has the row of the statement and the two rows of the callback");
	$same->('a callback writes two rows');

	# the callback is the only writer of the transaction
	$fresh->();
	$run->("BEGIN; SET LOCAL kcprobe.commit_sql = '" . $both->("VALUES (5, \$\$only\$\$)") . "'; SELECT 1; COMMIT;");
	is($q->("SELECT count(*) FROM p"), 1, "$order: the callback is the only writer of the transaction");
	$same->('only the callback writes');

	# many rows in one statement, wide values, and a statement that reads what it wrote
	$fresh->();
	$run->("BEGIN; SET LOCAL kcprobe.commit_sql = '"
		. $both->("SELECT g, repeat(\$\$z\$\$, 200 + g % 50) FROM generate_series(1, 6000) g")
		. "'; INSERT INTO p SELECT g + 100000, \$\$s\$\$ FROM generate_series(1, 2500) g; "
		. "INSERT INTO h SELECT g + 100000, \$\$s\$\$ FROM generate_series(1, 2500) g; COMMIT;");
	is($q->("SELECT count(*) FROM p"), 8500, "$order: 6000 late rows and 2500 ordinary rows");
	$same->('many rows');

	# COPY from a program is the multi-insert path
	$fresh->();
	$run->("BEGIN; SET LOCAL kcprobe.commit_sql = 'COPY p (a) FROM PROGRAM \$\$seq 1 3000\$\$; COPY h (a) FROM PROGRAM \$\$seq 1 3000\$\$';
		INSERT INTO p VALUES (-1, \$\$a\$\$); INSERT INTO h VALUES (-1, \$\$a\$\$); COMMIT;");
	is($q->("SELECT count(*) FROM p"), 3001, "$order: COPY in the callback");
	$same->('copy');
	# a late COPY is one write: the number of stripes is not one per row
	cmp_ok($q->("SELECT count(*) FROM kingres.columnar_stripes('p')"), '<=', 5,
		"$order: ... a late COPY of 3000 rows makes a few stripes, not one per row");

	# a subtransaction in the transaction, released before the commit
	$fresh->();
	$run->("BEGIN; SET LOCAL kcprobe.commit_sql = '" . $both->("VALUES (900, \$\$late\$\$)") . "';
		INSERT INTO p VALUES (1, \$\$a\$\$); INSERT INTO h VALUES (1, \$\$a\$\$);
		SAVEPOINT s; INSERT INTO p VALUES (2, \$\$b\$\$); INSERT INTO h VALUES (2, \$\$b\$\$); RELEASE s;
		SAVEPOINT t; INSERT INTO p VALUES (3, \$\$c\$\$); INSERT INTO h VALUES (3, \$\$c\$\$); ROLLBACK TO t; COMMIT;");
	$same->('with savepoints');

	# two columnar tables, one of them made in the transaction
	$fresh->();
	$run->("BEGIN; CREATE TABLE r (a int) USING columnar;
		SET LOCAL kcprobe.commit_sql = 'INSERT INTO r VALUES (1), (2), (3); INSERT INTO p VALUES (50, \$\$x\$\$); INSERT INTO h VALUES (50, \$\$x\$\$)';
		INSERT INTO r VALUES (10); COMMIT;");
	is($q->("SELECT count(*) FROM r"), 4, "$order: a table made in the transaction takes late rows");
	$same->('table made in the transaction');
	$q->("DROP TABLE r");

	# the rows are there after a restart too (the stripes are logged)
	$node->restart;
	$same->('after a restart');

	# PREPARE TRANSACTION: rows written between the flush and the prepare record
	$fresh->();
	$run->("BEGIN; SET LOCAL kcprobe.commit_sql = '" . $both->("VALUES (888, \$\$prep\$\$)") . "';
		INSERT INTO p VALUES (1, \$\$a\$\$); INSERT INTO h VALUES (1, \$\$a\$\$); PREPARE TRANSACTION 'kc1';");
	is($q->("SELECT count(*) FROM p"), 0, "$order: prepared: nothing is visible yet");
	$q->("COMMIT PREPARED 'kc1'");
	is($q->("SELECT count(*) FROM p"), 2, "$order: COMMIT PREPARED has the row written between the flush and the prepare");
	$same->('prepared and committed');
	$node->restart;
	$same->('prepared and committed, after a restart');

	$fresh->();
	$run->("BEGIN; SET LOCAL kcprobe.commit_sql = '" . $both->("VALUES (889, \$\$prep\$\$)") . "';
		INSERT INTO p VALUES (1, \$\$a\$\$); INSERT INTO h VALUES (1, \$\$a\$\$); PREPARE TRANSACTION 'kc2';");
	$node->restart;
	$q->("COMMIT PREPARED 'kc2'");
	$same->('prepared, a restart, then committed');
	$fresh->();
	$run->("BEGIN; SET LOCAL kcprobe.commit_sql = '" . $both->("VALUES (890, \$\$prep\$\$)") . "';
		INSERT INTO p VALUES (1, \$\$a\$\$); INSERT INTO h VALUES (1, \$\$a\$\$); PREPARE TRANSACTION 'kc3';");
	$q->("ROLLBACK PREPARED 'kc3'");
	is($q->("SELECT count(*) FROM p"), 0, "$order: ROLLBACK PREPARED discards the late row with the rest");

	# deletes and updates arriving in that place are refused, not dropped
	$fresh->();
	$q->("INSERT INTO p VALUES (1, 'keep')");
	($out, $err) = $run->("BEGIN; SET LOCAL kcprobe.commit_sql = 'DELETE FROM p'; INSERT INTO p VALUES (2, \$\$new\$\$); COMMIT;");
	like($err, qr/append-only/, "$order: a delete in a pre-commit callback is refused");
	is($q->("SELECT count(*) FROM p"), 1, "$order: ... and nothing of that transaction is committed");
	($out, $err) = $run->("BEGIN; SET LOCAL kcprobe.commit_sql = 'UPDATE p SET a = a + 1'; INSERT INTO p VALUES (2, \$\$new\$\$); COMMIT;");
	like($err, qr/append-only|fetching a row by its TID/, "$order: an update in a pre-commit callback is refused");
	is($q->("SELECT count(*) FROM p"), 1, "$order: ... and nothing of that transaction is committed");
	($out, $err) = $run->("BEGIN; SET LOCAL kcprobe.commit_sql = 'TRUNCATE p'; INSERT INTO p VALUES (2, \$\$new\$\$); COMMIT;");
	is($q->("SELECT count(*) FROM p"), 0, "$order: a truncate in a pre-commit callback empties the table, as heap does");

	# a temporary columnar table, the target of ON COMMIT DELETE ROWS and ON COMMIT DROP, is refused
	($out, $err) = $run->("CREATE TEMP TABLE tt (a int) USING columnar ON COMMIT DELETE ROWS");
	like($err, qr/cannot be temporary or unlogged/, "$order: ON COMMIT DELETE ROWS cannot reach a columnar table: there is no temporary one");
	($out, $err) = $run->("CREATE TEMP TABLE tt (a int) USING columnar ON COMMIT DROP");
	like($err, qr/cannot be temporary or unlogged/, "$order: ON COMMIT DROP cannot reach a columnar table either");

	# an error in the callback aborts the transaction and leaves nothing
	$fresh->();
	($out, $err) = $run->("BEGIN; SET LOCAL kcprobe.commit_sql = 'INSERT INTO p VALUES (1, \$\$x\$\$); SELECT 1/0'; INSERT INTO p VALUES (2, \$\$a\$\$); COMMIT;");
	like($err, qr/division by zero/, "$order: a failing callback");
	is($q->("SELECT count(*) FROM p"), 0, "$order: ... aborts the transaction with its late rows");

	# the next transaction is not affected by the late state of the last one
	$run->("INSERT INTO p VALUES (3, \$\$z\$\$)");
	is($q->("SELECT count(*) FROM p"), 1, "$order: an ordinary insert after those still works");
	$node->stop;
}

done_testing();
