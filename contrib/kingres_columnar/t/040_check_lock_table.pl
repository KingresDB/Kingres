# contrib/kingres_columnar/t/040_check_lock_table.pl
#
# C11-03.  columnar_check(), columnar_check(true) and columnar_check_clean() kept a lock on every columnar
# table until the end of the caller's transaction.  The shared lock table has a fixed size
# (max_locks_per_transaction times the number of backends and prepared transactions), so with more
# columnar tables than it has room for, both functions ended with "out of shared memory" (an error that
# is rightly raised to the caller, not turned into a row about a table), and well below that number they
# took entries that other sessions needed while they ran.  A database with a table per partition of a time
# series cannot be checked that way.
#
# The check now lets go of each table's lock when it has looked at it (a lock the caller held before is the
# caller's and stays: t/036), so it needs one entry at a time.  This file runs it on a server whose lock
# table is small, with more columnar tables than it holds: first it shows that one transaction cannot lock
# them all (the premise), then that the three functions work, inside a transaction block as well, and
# leave no lock behind.
#
# Portions Copyright (c) 2026, RooyeKhat Media Co
# SPDX-License-Identifier: AGPL-3.0-only

use strict;
use warnings FATAL => 'all';

use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;

my $NTABLES = 1000;

my $node = PostgreSQL::Test::Cluster->new('collocktable');
$node->init;
$node->append_conf('postgresql.conf', "shared_preload_libraries = 'kingres_columnar'\nautovacuum = off\nmax_connections = 10\nmax_locks_per_transaction = 10\nmax_worker_processes = 4\nmax_wal_senders = 0\n");
$node->start;
$node->safe_psql('postgres', 'CREATE EXTENSION kingres_columnar');

sub q1
{
	my ($sql) = @_;
	my $r = eval { $node->safe_psql('postgres', $sql) };
	die "t/040: q1 failed running [$sql]: $@" if $@;
	return $r;
}

# the tables, one transaction each (one transaction could not even create them: that is the point)
my $file = PostgreSQL::Test::Utils::tempdir() . '/mk.sql';
open(my $fh, '>', $file) or die "$file: $!";
foreach my $i (1 .. $NTABLES)
{
	print $fh "CREATE TABLE lt$i (a int) USING columnar;\n";
}
close($fh);
$node->command_ok([ 'psql', '-XAtq', '-v', 'ON_ERROR_STOP=1', '-d', $node->connstr('postgres'), '-f', $file ], "$NTABLES columnar tables made, one transaction each");
is(q1("SELECT count(*) FROM pg_class WHERE relname ~ '^lt[0-9]+\$' AND relam = (SELECT oid FROM pg_am WHERE amname = 'columnar')"), $NTABLES, 'the tables are there');

# the premise: one transaction cannot hold a lock on all of them
{
	my ($ret, $out, $err) = $node->psql('postgres',
		"DO \$\$ DECLARE r record; BEGIN FOR r IN SELECT oid::regclass AS t FROM pg_class WHERE relname ~ '^lt[0-9]+\$' LOOP EXECUTE format('LOCK TABLE %s IN ACCESS SHARE MODE', r.t); END LOOP; END \$\$");
	like($err, qr/out of shared memory/, 'the premise: a transaction cannot lock all the tables at once (the lock table is too small for it)');
}

# the check works
is(q1('SELECT kingres.columnar_check_clean()'), 't', 'columnar_check_clean() works with more tables than the lock table holds');
is(q1('SELECT count(*) FROM kingres.columnar_check()'), '0', 'columnar_check() works');
is(q1('SELECT count(*) FROM kingres.columnar_check(true)'), '0', 'columnar_check(true) works');

# inside a transaction block, where the locks used to be kept to the end, and with nothing left behind
{
	my $s = $node->background_psql('postgres', on_error_stop => 1);
	my $r = $s->query_safe('BEGIN; SELECT kingres.columnar_check_clean();');
	$r =~ s/\s+//g;
	is($r, 't', 'inside a transaction block: true');
	$r = $s->query_safe("SELECT count(*) FROM pg_locks l JOIN pg_class c ON c.oid = l.relation WHERE l.pid = pg_backend_pid() AND c.relname ~ '^lt[0-9]+\$'");
	$r =~ s/\s+//g;
	is($r, '0', 'and the transaction holds no lock of the tables the check looked at');
	$s->query_safe('COMMIT');
	$s->quit;
}

# a damaged table is still found among them
{
	q1("INSERT INTO lt500 SELECT generate_series(1, 20000)");
	q1('CHECKPOINT');
	my $path = $node->data_dir . '/' . q1("SELECT pg_relation_filepath('lt500')");
	truncate($path, 8192 * 1) or die "truncate $path: $!";
	is(q1('SELECT kingres.columnar_check_clean()'), 'f', 'a damaged table among the many: not clean');
	is(q1("SELECT string_agg(relation::text, ',') FROM kingres.columnar_check() WHERE check_name = 'stripe_beyond_file'"), 'lt500', '... and it is named');
}

$node->stop;
done_testing();
