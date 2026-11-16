# contrib/kingres_ledger/t/013_head_inflight.pl
#
# ledger_head and a row above the head (review finding L8-08):
#
#   * the check that no transaction row and no recorded digest names a block
#     above the head must never fire on honest work.  Writers, block builders
#     and digest generators run while the head is read, on the primary and on
#     a hot standby that replays their work, in single statements and in
#     loops of a VOLATILE context (a DO block, where every call has its own
#     snapshot).  Not one call may fail.  The test does not depend on timing:
#     it reads until the load has finished, and the number of calls is only
#     reported;
#   * a row that is planted (as a superuser, which is what it takes) is
#     refused by the primary and by the standby that replays it, with a
#     message that names the row and the table, and ledger_verify names it
#     on both.
#
# Portions Copyright (c) 2026, RooyeKhat Media Co
# SPDX-License-Identifier: AGPL-3.0-only

use strict;
use warnings FATAL => 'all';

use IPC::Run;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;

my $primary = PostgreSQL::Test::Cluster->new('primary');
$primary->init(allows_streaming => 1);
$primary->append_conf('postgresql.conf', qq(
shared_preload_libraries = 'kingres_ledger'
kingres_ledger.block_interval = 0
max_connections = 60
));
$primary->start;

sub q1
{
	my ($node, $sql) = @_;
	return $node->safe_psql('inf', $sql);
}

$primary->safe_psql('postgres', 'CREATE DATABASE inf');
q1($primary, q(
	CREATE EXTENSION kingres_ledger;
	CREATE TABLE public.w (client int, note text);
	SELECT kingres.ledger_enable('public.w');
	INSERT INTO public.w VALUES (0, 'first');
	SELECT kingres.ledger_build_block();
));
$primary->backup('b1');
my $standby = PostgreSQL::Test::Cluster->new('standby');
$standby->init_from_backup($primary, 'b1', has_streaming => 1);
$standby->append_conf('postgresql.conf', "shared_preload_libraries = 'kingres_ledger'");
$standby->start;
$primary->wait_for_catchup($standby);

#--------------------------------------------------------------------------
# Honest work: nothing above the head may be seen, at any moment.
#--------------------------------------------------------------------------
my $dir = $PostgreSQL::Test::Utils::tmp_check;
append_to_file("$dir/w13.sql", "INSERT INTO w SELECT :client_id, 'x' FROM generate_series(1, 3);\n");
append_to_file("$dir/b13.sql", "SELECT kingres.ledger_build_block();\n");
append_to_file("$dir/g13.sql", "SELECT kingres.ledger_generate_digest() IS NOT NULL;\n");
my @pgbench = ('pgbench', '-n', '-h', $primary->host, '-p', $primary->port);
my ($wout, $werr, $bout, $berr, $gout, $gerr);
my $writers = IPC::Run::start(
	[ @pgbench, '-c', '3', '-j', '2', '-t', '3000', '-f', "$dir/w13.sql", 'inf' ], '>', \$wout, '2>', \$werr);
my $builders = IPC::Run::start(
	[ @pgbench, '-c', '2', '-j', '2', '-t', '3000', '-f', "$dir/b13.sql", 'inf' ], '>', \$bout, '2>', \$berr);
my $digesters = IPC::Run::start(
	[ @pgbench, '-c', '2', '-j', '2', '-t', '1500', '-f', "$dir/g13.sql", 'inf' ], '>', \$gout, '2>', \$gerr);

my $loop_sql = q(DO $$ BEGIN FOR i IN 1..10 LOOP PERFORM * FROM kingres.ledger_head(); END LOOP; END $$);
my ($calls, $failed, @errors) = (0, 0);
do
{
	foreach my $node ($primary, $standby)
	{
		foreach my $sql ('SELECT count(*) FROM kingres.ledger_head()', $loop_sql,
			q(BEGIN ISOLATION LEVEL REPEATABLE READ; SELECT count(*) FROM kingres.ledger_head();
			  SELECT count(*) FROM kingres.ledger_blocks_entries(0, 10000); COMMIT))
		{
			my ($ret, $out, $err) = $node->psql('inf', $sql);
			$calls++;
			if ($ret != 0)
			{
				$failed++;
				push @errors, $err;
			}
		}
	}
	$writers->pump_nb if $writers->pumpable;
	$builders->pump_nb if $builders->pumpable;
	$digesters->pump_nb if $digesters->pumpable;
} while ($writers->pumpable || $builders->pumpable || $digesters->pumpable);
ok($writers->finish, 'writers finished') or diag($werr);
ok($builders->finish, 'builders finished') or diag($berr);
ok($digesters->finish, 'digest generators finished') or diag($gerr);
cmp_ok($calls, '>=', 6, "calls were made while the load ran ($calls)");
is($failed, 0, 'no call of ledger_head failed on the primary or on the standby under load')
  or diag(join("\n", @errors[ 0 .. ($#errors > 4 ? 4 : $#errors) ]));
while (q1($primary, 'SELECT kingres.ledger_build_block()') ne '') { }
is(q1($primary, 'SELECT count(*) FROM kingres.ledger_verify()'), '0', 'the ledger verifies after the run');
cmp_ok(q1($primary, 'SELECT count(*) FROM kingres.ledger_digest'), '>', 0, 'digests were recorded under the load');

#--------------------------------------------------------------------------
# A planted row: refused where it is, and where it is replayed.
#--------------------------------------------------------------------------
$primary->wait_for_catchup($standby);
my $head = q1($primary, 'SELECT height FROM kingres.ledger_head()');
is(q1($standby, 'SELECT height FROM kingres.ledger_head()'), $head, 'the standby has the primary head');
q1($primary, q(INSERT INTO kingres.ledger_tx
	VALUES (900000007, 7777, 0, 1, sha256('x'), 'x')));
$primary->wait_for_catchup($standby);
foreach my $node ($primary, $standby)
{
	my $name = $node == $primary ? 'primary' : 'standby';
	my ($ret, $out, $err) = $node->psql('inf', 'SELECT * FROM kingres.ledger_head()');
	isnt($ret, 0, "ledger_head refuses on the $name");
	like($err, qr/the head is block \Q$head\E, but the row of transaction 900000007 in kingres\.ledger_tx names block 7777/,
		"the error on the $name names the row, the table and the block");
	is(q1($node, q(SELECT string_agg(check_name || ' ' || object, ',') FROM kingres.ledger_verify())),
		'tx_block tx 900000007', "ledger_verify on the $name reports the same row");
}

$standby->stop;
$primary->stop;
done_testing();
