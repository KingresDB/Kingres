# contrib/kingres_ledger/t/014_guard.pl
#
# The write guard (review finding L9-01): a row of a ledger table exists only
# because the code of the extension wrote it.
#
#   * a member of pg_write_all_data attacks every ledger table with INSERT
#     while writers, block builders and digest generators run: not one
#     statement succeeds, the honest work is not disturbed, and the ledger
#     verifies afterwards, with no block time that the attacker chose;
#   * COPY ... FROM STDIN into each of the nine tables: refused for the member
#     and for a superuser who has not turned allow_manual_write on, allowed (and
#     rolled back here) for one who has;
#   * a hot standby replays the honest work (replay does not go through the
#     executor) and verifies it;
#   * logical replication: a subscription whose publisher publishes a ledger
#     table cannot write it on the subscriber (the guard trigger sees what the
#     executor hook does not), and the rows are not there.
#
# The test does not depend on timing: the load has a fixed number of
# transactions, and the attackers a fixed number of statements.
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
max_connections = 80
wal_level = logical
max_wal_senders = 10
max_replication_slots = 10
));
$primary->start;

sub q1
{
	my ($node, $sql, $db) = @_;
	return $node->safe_psql($db // 'gd', $sql);
}

$primary->safe_psql('postgres', q(
	CREATE DATABASE gd;
	CREATE ROLE gd_member IN ROLE pg_write_all_data LOGIN;
	CREATE ROLE gd_owner LOGIN;
));
q1($primary, q(
	CREATE EXTENSION kingres_ledger;
	GRANT USAGE ON SCHEMA kingres TO gd_member;
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
# A member of pg_write_all_data against every table, under load.
#--------------------------------------------------------------------------
my $dir = $PostgreSQL::Test::Utils::tmp_check;
my %attack = (
	ledger_table => q{INSERT INTO kingres.ledger_table (relid, mode) VALUES ('public.w', 'append_only')},
	ledger_chain => q{INSERT INTO kingres.ledger_chain (chain_id) VALUES (sha256('x'))},
	ledger_entry => q{INSERT INTO kingres.ledger_entry VALUES (900001, 0, 1, 'I', sha256('x'))},
	ledger_pending => q{INSERT INTO kingres.ledger_pending (ledger_txid, user_name) VALUES (900002, 'x')},
	ledger_pending_delete => q{DELETE FROM kingres.ledger_pending},
	ledger_tx => q{INSERT INTO kingres.ledger_tx VALUES (900003, 5000, 0, 1, sha256('x'), 'x')},
	ledger_blocks => q{INSERT INTO kingres.ledger_blocks VALUES (5000, 1, sha256('a'), sha256('b'), 1, '2100-01-01 00:00:00+00', sha256('c'))},
	ledger_digest => q{INSERT INTO kingres.ledger_digest (generated_at, generated_by, height, block_hash, digest) VALUES (now(), 'x', 0, sha256('c'), '{}')},
	ledger_verification => q{INSERT INTO kingres.ledger_verification (started_at, started_by, from_height, state) VALUES (now(), 'x', 0, 'clean')},
);
foreach my $name (keys %attack)
{
	append_to_file("$dir/a14_$name.sql", join('', ($attack{$name} . ";\n") x 100));
}
append_to_file("$dir/w14.sql", "INSERT INTO w SELECT :client_id, 'x' FROM generate_series(1, 3);\n");
append_to_file("$dir/b14.sql", "SELECT kingres.ledger_build_block();\n");
append_to_file("$dir/g14.sql", "SELECT kingres.ledger_generate_digest() IS NOT NULL;\n");
my @pgbench = ('pgbench', '-n', '-h', $primary->host, '-p', $primary->port);
my ($wout, $werr, $bout, $berr, $gout, $gerr);
my $writers = IPC::Run::start(
	[ @pgbench, '-c', '3', '-j', '2', '-t', '600', '-f', "$dir/w14.sql", 'gd' ], '>', \$wout, '2>', \$werr);
my $builders = IPC::Run::start(
	[ @pgbench, '-c', '2', '-j', '2', '-t', '600', '-f', "$dir/b14.sql", 'gd' ], '>', \$bout, '2>', \$berr);
my $digesters = IPC::Run::start(
	[ @pgbench, '-c', '2', '-j', '2', '-t', '300', '-f', "$dir/g14.sql", 'gd' ], '>', \$gout, '2>', \$gerr);
my (%aout, %aerr, %attackers);
foreach my $name (sort keys %attack)
{
	$attackers{$name} = IPC::Run::start(
		[ 'psql', '-X', '-q', '-h', $primary->host, '-p', $primary->port, '-U', 'gd_member', '-d', 'gd',
		  '-v', 'ON_ERROR_STOP=0', '-f', "$dir/a14_$name.sql" ], '>', \$aout{$name}, '2>', \$aerr{$name});
}
my $busy;
do
{
	$busy = 0;
	foreach my $h ($writers, $builders, $digesters, values %attackers)
	{
		if ($h->pumpable) { $h->pump_nb; $busy = 1; }
	}
} while ($busy);
ok($writers->finish, 'writers finished') or diag($werr);
ok($builders->finish, 'builders finished') or diag($berr);
ok($digesters->finish, 'digest generators finished') or diag($gerr);
foreach my $name (sort keys %attack)
{
	$attackers{$name}->finish;
	my $refused = () = $aerr{$name} =~ /cannot write to table/g;
	is($refused, 100, "member: all 100 statements on $name were refused by the guard")
	  or diag($aerr{$name});
}
while (q1($primary, 'SELECT kingres.ledger_build_block()') ne '') { }
is(q1($primary, 'SELECT count(*) FROM kingres.ledger_verify()'), '0', 'the ledger verifies after the run');
is(q1($primary, q(SELECT count(*) FROM kingres.ledger_blocks WHERE closed_at > now() + interval '1 day')),
	'0', 'and no block carries a time that an attacker chose');
is(q1($primary, 'SELECT count(*) FROM kingres.ledger_tx WHERE block_height >= 5000 OR ledger_txid >= 900000'),
	'0', 'no row of the attackers is in the ledger');
cmp_ok(q1($primary, 'SELECT count(*) FROM kingres.ledger_digest'), '>', 0, 'digests were recorded under the load');

#--------------------------------------------------------------------------
# COPY FROM into each table.
#--------------------------------------------------------------------------
my %copy = (
	ledger_table => [ 'relid, mode', "public.w\tappend_only" ],
	ledger_chain => [ 'chain_id', '\\\\x' . ('00' x 32) ],
	ledger_entry => [ 'ledger_txid, seq, table_id, op, row_hash', "900001\t0\t1\tI\t\\\\x" . ('00' x 32) ],
	ledger_pending => [ 'ledger_txid, user_name', "900002\tx" ],
	ledger_tx => [ 'ledger_txid, block_height, tx_index, entry_count, merkle_root, user_name',
		"900003\t5000\t0\t1\t\\\\x" . ('00' x 32) . "\tx" ],
	ledger_blocks => [ 'height, hash_version, prev_hash, tx_merkle_root, tx_count, closed_at, block_hash',
		"5000\t1\t\\\\x" . ('00' x 32) . "\t\\\\x" . ('00' x 32) . "\t1\t2100-01-01\t\\\\x" . ('00' x 32) ],
	ledger_digest => [ 'generated_at, generated_by, height, block_hash, digest',
		"2026-01-01\tx\t0\t\\\\x" . ('00' x 32) . "\t{}" ],
	ledger_verification => [ 'started_at, started_by, from_height, state', "2026-01-01\tx\t0\tclean" ],
	ledger_removed => [ 'removed_at, removed_by, reason, source_table, row_data', "2026-01-01\tx\tr\tledger_tx\t{}" ],
);
foreach my $name (sort keys %copy)
{
	my ($cols, $row) = @{ $copy{$name} };
	my $sql = "COPY kingres.$name ($cols) FROM STDIN;\n$row\n\\.\n";
	my ($ret, $out, $err) = $primary->psql('gd', $sql, extra_params => [ '-U', 'gd_member' ]);
	like($err, qr/cannot write to table "\Q$name\E"/, "COPY into $name as the member is refused");
	($ret, $out, $err) = $primary->psql('gd', $sql);
	like($err, qr/cannot write to table "\Q$name\E"/, "COPY into $name as a superuser is refused");
	($ret, $out, $err) = $primary->psql('gd',
		"BEGIN;\nSET LOCAL kingres_ledger.allow_manual_write = on;\n$sql ROLLBACK;\n");
	unlike($err, qr/cannot write to table/, "COPY into $name with allow_manual_write on is not refused by the guard");
}
is(q1($primary, 'SELECT count(*) FROM kingres.ledger_verify()'), '0', 'nothing was left by the COPY statements');

#--------------------------------------------------------------------------
# A hot standby replays the honest work and verifies it.
#--------------------------------------------------------------------------
$primary->wait_for_catchup($standby);
is(q1($standby, 'SELECT count(*) FROM kingres.ledger_verify()'), '0', 'the standby verifies what it replayed');
is(q1($standby, 'SELECT height FROM kingres.ledger_head()'), q1($primary, 'SELECT height FROM kingres.ledger_head()'),
	'with the head of the primary');

#--------------------------------------------------------------------------
# Logical replication into a ledger table: the guard trigger refuses it.
#--------------------------------------------------------------------------
my $pub = PostgreSQL::Test::Cluster->new('publisher');
$pub->init(allows_streaming => 'logical');
$pub->append_conf('postgresql.conf', qq(
shared_preload_libraries = 'kingres_ledger'
kingres_ledger.block_interval = 0
));
$pub->start;
$pub->safe_psql('postgres', 'CREATE DATABASE pd');
$pub->safe_psql('pd', q(
	CREATE EXTENSION kingres_ledger;
	CREATE TABLE public.t (id int);
	SELECT kingres.ledger_enable('public.t');
	INSERT INTO public.t VALUES (1);
	SELECT kingres.ledger_build_block();
	CREATE PUBLICATION pub_ledger FOR TABLE kingres.ledger_blocks;
));
is($pub->safe_psql('pd', 'SELECT count(*) FROM kingres.ledger_blocks'), '1', 'the publisher has a block');
my $before = q1($primary, 'SELECT count(*) FROM kingres.ledger_blocks');
my $log_offset = -s $primary->logfile;
(my $pubconn = $pub->connstr('pd')) =~ s/'/''/g;
q1($primary, "CREATE SUBSCRIPTION sub_ledger CONNECTION '$pubconn' PUBLICATION pub_ledger WITH (copy_data = true)");
ok($primary->wait_for_log(qr/cannot write to table "ledger_blocks"/, $log_offset),
	'the synchronisation of the subscription is refused by the guard');
is(q1($primary, 'SELECT count(*) FROM kingres.ledger_blocks'), $before,
	'and the table has no row from the publisher');
q1($primary, q(ALTER SUBSCRIPTION sub_ledger DISABLE));
q1($primary, q(ALTER SUBSCRIPTION sub_ledger SET (slot_name = NONE)));
q1($primary, q(DROP SUBSCRIPTION sub_ledger));
$pub->safe_psql('pd', q(SELECT pg_drop_replication_slot(slot_name) FROM pg_replication_slots));

# The apply worker: a subscription that copies nothing, and a change that
# reaches it afterwards (the block that the publisher builds for a new row).
$log_offset = -s $primary->logfile;
q1($primary, "CREATE SUBSCRIPTION sub_apply CONNECTION '$pubconn' PUBLICATION pub_ledger WITH (copy_data = false)");
$pub->safe_psql('pd', q(INSERT INTO public.t VALUES (2); SELECT kingres.ledger_build_block();));
ok($primary->wait_for_log(qr/cannot write to table "ledger_blocks"/, $log_offset),
	'the apply worker is refused by the guard too');
is(q1($primary, 'SELECT count(*) FROM kingres.ledger_blocks'), $before,
	'and the table has no row from the publisher');
q1($primary, q(ALTER SUBSCRIPTION sub_apply DISABLE));
q1($primary, q(ALTER SUBSCRIPTION sub_apply SET (slot_name = NONE)));
q1($primary, q(DROP SUBSCRIPTION sub_apply));
$pub->safe_psql('pd', q(SELECT pg_drop_replication_slot(slot_name) FROM pg_replication_slots));
is(q1($primary, 'SELECT count(*) FROM kingres.ledger_verify()'), '0', 'the ledger still verifies');

$pub->stop;
$standby->stop;
$primary->stop;
done_testing();
