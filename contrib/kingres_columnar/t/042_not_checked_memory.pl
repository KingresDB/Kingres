# contrib/kingres_columnar/t/042_not_checked_memory.pl
#
# C12-02.  Every not_checked row of a locked table asked the lock manager for a full copy of the lock data
# of the server and kept it until the call ended: the memory (and the time, with every lock partition held
# for each copy) of a call with N locked tables grew with N times the size of the lock table, that is with
# the square of N when a large DDL holds the tables (a transaction that alters a partitioned table holds
# every partition).  A check made while such a DDL ran could end with "out of memory" where it should
# have reported not_checked.
#
# The lock data is now read once for each fixed number of rows and freed, and at the end of the call.  This
# file locks a few hundred tables in one session and runs the check in another: every table is reported,
# each with the process that holds it, and the peak memory of the checking backend (VmHWM of the process,
# where the operating system has it) grows by little more than what the rows themselves cost.  The
# quadratic version grows by many times that bound (several tens of megabytes for these numbers, measured);
# the bound is far below anything that could harm a machine.
#
# Portions Copyright (c) 2026, RooyeKhat Media Co
# SPDX-License-Identifier: AGPL-3.0-only

use strict;
use warnings FATAL => 'all';

use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;

my $NTABLES = 800;
my $BOUND_KB = 14 * 1024;

my $node = PostgreSQL::Test::Cluster->new('colnotchecked');
$node->init;
$node->append_conf('postgresql.conf', "shared_preload_libraries = 'kingres_columnar'\nautovacuum = off\nmax_connections = 20\nmax_worker_processes = 4\nmax_wal_senders = 0\n");
$node->start;
$node->safe_psql('postgres', 'CREATE EXTENSION kingres_columnar');

sub q1
{
	my ($sql) = @_;
	my $r = eval { $node->safe_psql('postgres', $sql) };
	die "t/042: q1 failed running [$sql]: $@" if $@;
	return $r;
}

sub bgq
{
	my ($s, $sql) = @_;
	my $r = eval { $s->query_safe($sql) };
	die "t/042: a background session failed running [$sql]: $@" if $@;
	$r =~ s/\s+//g;
	return $r;
}

# the peak resident memory of a process in kB, or undef where the system does not tell
sub hwm_kb
{
	my ($pid) = @_;
	open(my $fh, '<', "/proc/$pid/status") or return undef;
	while (my $l = <$fh>)
	{
		return $1 if $l =~ /^VmHWM:\s+(\d+) kB/;
	}
	return undef;
}

my $file = PostgreSQL::Test::Utils::tempdir() . '/mk.sql';
open(my $fh, '>', $file) or die "$file: $!";
foreach my $i (1 .. $NTABLES)
{
	print $fh "CREATE TABLE nc$i (a int) USING columnar;\n";
}
close($fh);
$node->command_ok([ 'psql', '-XAtq', '-v', 'ON_ERROR_STOP=1', '-d', $node->connstr('postgres'), '-f', $file ], "$NTABLES columnar tables made");

# one session holds them all
my $holder = $node->background_psql('postgres', on_error_stop => 1);
bgq($holder, "BEGIN; DO \$\$ DECLARE r record; BEGIN FOR r IN SELECT oid::regclass AS t FROM pg_class WHERE relname ~ '^nc[0-9]+\$' LOOP EXECUTE format('LOCK TABLE %s IN ACCESS EXCLUSIVE MODE', r.t); END LOOP; END \$\$;");
my $hpid = bgq($holder, 'SELECT pg_backend_pid()');

my $call = $node->background_psql('postgres', on_error_stop => 1);
my $cpid = bgq($call, 'SELECT pg_backend_pid()');
# warm the backend up: catalogs, the library, the plan of the call
bgq($call, 'SELECT count(*) FROM pg_class');

my $before = hwm_kb($cpid);
my $n = bgq($call, "SELECT count(*) FROM kingres.columnar_check() WHERE check_name = 'not_checked'");
my $after = hwm_kb($cpid);
is($n, $NTABLES, "every one of the $NTABLES locked tables is reported as not_checked");
is(bgq($call, "SELECT count(*) FROM kingres.columnar_check() WHERE check_name = 'not_checked' AND detail ~ 'It is held by process $hpid\\.'"), $NTABLES,
	'... each one names the process that holds it');
is(bgq($call, 'SELECT kingres.columnar_check_clean()'), 'f', '... and the strict form is false');

SKIP:
{
	skip 'the peak memory of a process is not known on this system', 1 unless defined $before && defined $after;
	my $grew = $after - $before;
	cmp_ok($grew, '<', $BOUND_KB, "the peak memory of the checking backend grew by $grew kB for $NTABLES not_checked rows (bound $BOUND_KB kB)");
}

bgq($holder, 'COMMIT');
$holder->quit;
is(bgq($call, 'SELECT kingres.columnar_check_clean()'), 't', 'when the holder is gone the check is clean');
$call->quit;
$node->stop;
done_testing();
