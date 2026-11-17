# SPDX-License-Identifier: AGPL-3.0-only
#
# contrib/kingres_ledger/t/020_not_covered.pl
#
# What the third review of the guard listed as not covered (section 14), run:
#
#   * a loopback through dblink and through postgres_fdw: a role that is not a
#     superuser needs a password for it, and what it sends comes back as a second
#     session of the same role, in which every hook runs;
#   * CREATE SUBSCRIPTION with the initial copy into an application ledger table,
#     and the apply of later changes;
#   * ATTACH PARTITION of a table that has a foreign key to an application ledger
#     table (a foreign key to a table of the catalog cannot be made, which another
#     test shows);
#   * DROP EXTENSION plpgsql CASCADE racing the first ledger_enable of a database.
#
# Portions Copyright (c) 2026, RooyeKhat Media Co

use strict;
use warnings FATAL => 'all';

use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;

$ENV{PGOPTIONS} = '-c search_path=public';

my $node = PostgreSQL::Test::Cluster->new('notcov');
$node->init;
my $super = do { my $u = getpwuid($<); $u };
{
	# Password authentication for every role but the one that runs the test: a
	# role that is not a superuser cannot use dblink or postgres_fdw without one.
	my $hba = $node->data_dir . '/pg_hba.conf';
	open my $fh, '>', $hba or die "cannot write $hba: $!";
	print $fh "local all $super trust\nlocal all all scram-sha-256\n";
	close $fh;
}
$node->append_conf('postgresql.conf',
	"shared_preload_libraries = 'kingres_ledger'\n"
	  . "kingres_ledger.block_interval = 0\n"
	  . "wal_level = logical\n");
$node->start;
$super = $node->safe_psql('postgres', 'SELECT current_user');

$node->safe_psql('postgres', qq{
	CREATE ROLE inh LOGIN PASSWORD 'pw';
	GRANT "$super" TO inh WITH INHERIT TRUE, SET FALSE;
	CREATE ROLE maint LOGIN PASSWORD 'pw' IN ROLE pg_maintain;
	CREATE ROLE wad LOGIN PASSWORD 'pw' IN ROLE pg_write_all_data;
	CREATE ROLE app LOGIN PASSWORD 'pw';
	CREATE DATABASE d;
});
$node->safe_psql('d', q{
	CREATE EXTENSION kingres_ledger;
	CREATE EXTENSION dblink;
	CREATE EXTENSION postgres_fdw;
	GRANT USAGE ON SCHEMA kingres TO PUBLIC;
	GRANT CREATE ON SCHEMA public TO inh, maint, wad, app;
	GRANT EXECUTE ON FUNCTION dblink_exec(text, text), dblink_connect(text, text), dblink_disconnect(text) TO inh, maint, wad;
	CREATE TABLE docs (id int PRIMARY KEY, body text);
	ALTER TABLE docs OWNER TO app;
	SELECT kingres.ledger_enable('docs');
	INSERT INTO docs VALUES (1, 'a');
	SELECT kingres.ledger_build_block();
});

sub as_role
{
	my ($role, $sql) = @_;
	local $ENV{PGPASSWORD} = 'pw';
	return $node->psql('d', $sql, extra_params => [ '-U', $role ]);
}

# A lock of the ledger tables that a backend holds, and whether the writer and
# the builder go on.
sub nothing_stuck
{
	my $held = $node->safe_psql('d', q{
		SELECT count(*) FROM pg_locks l JOIN pg_class c ON c.oid = l.relation
		 WHERE c.relnamespace = 'kingres'::regnamespace AND l.granted
		   AND l.mode IN ('RowShareLock', 'RowExclusiveLock', 'ShareLock', 'ShareRowExclusiveLock',
		                  'ExclusiveLock', 'AccessExclusiveLock')
		   AND l.pid <> pg_backend_pid()});
	my ($ret, $out, $err) = $node->psql('d', q{SET lock_timeout = '30s';
		INSERT INTO docs VALUES ((SELECT max(id) + 1 FROM docs), 'x');
		SELECT kingres.ledger_build_block() IS NOT NULL});
	return ($held eq '0' && $out =~ /^t$/m && $err eq '') ? 'ok' : "held=$held err=$err";
}

#--------------------------------------------------------------------------
# A loopback through dblink: the second session is the same role
#--------------------------------------------------------------------------
my $conn = sprintf("host=%s port=%d dbname=d", $node->host, $node->port);
my @via_dblink = (
	[ 'wad', 'INSERT INTO kingres.ledger_blocks DEFAULT VALUES', qr/cannot write to table "ledger_blocks"/ ],
	[ 'wad', 'INSERT INTO kingres.ledger_pending (ledger_txid, user_name) VALUES (1, \'x\')', qr/cannot write to table "ledger_pending"/ ],
	[ 'wad', 'LOCK TABLE kingres.ledger_pending IN ROW EXCLUSIVE MODE', qr/KL002|cannot lock|ledger/ ],
	[ 'maint', 'LOCK TABLE kingres.ledger_blocks IN ACCESS EXCLUSIVE MODE', qr/KL002|cannot lock|ledger/ ],
	[ 'inh', 'DROP TABLE kingres.ledger_blocks', qr/cannot drop|ledger|protected/ ],
	[ 'inh', 'TRUNCATE kingres.ledger_pending', qr/cannot truncate|ledger|append-only/ ],
);
foreach my $c (@via_dblink)
{
	my ($role, $stmt, $re) = @$c;
	my ($ret, $out, $err) = as_role($role, qq{
		SELECT dblink_connect('lo', '$conn user=$role password=pw');
		SELECT dblink_exec('lo', 'BEGIN');
		SELECT dblink_exec('lo', \$q\$$stmt\$q\$);
		SELECT dblink_exec('lo', 'ROLLBACK');});
	like($err, $re, "dblink loopback as $role: '$stmt' is refused by the hooks of the second session");
	is(nothing_stuck(), 'ok', 'and nothing of the ledger is held or stopped');
}
# the honest use: a role that owns a ledger table writes it through the loopback
{
	my ($ret, $out, $err) = as_role('app', q{SELECT 1});
	$node->safe_psql('d', q{GRANT EXECUTE ON FUNCTION dblink_exec(text, text), dblink_connect(text, text), dblink_disconnect(text) TO app});
	($ret, $out, $err) = as_role('app', qq{
		SELECT dblink_connect('lo', '$conn user=app password=pw');
		SELECT dblink_exec('lo', 'INSERT INTO docs VALUES (900, \$\$by loopback\$\$)');});
	is($err, '', 'the owner of a ledger table writes it through a loopback');
	is($node->safe_psql('d', q{SELECT kingres.ledger_build_block() IS NOT NULL}), 't', 'and the row is captured and built');
}

#--------------------------------------------------------------------------
# A loopback through postgres_fdw
#--------------------------------------------------------------------------
$node->safe_psql('d', qq{
	CREATE SERVER loop FOREIGN DATA WRAPPER postgres_fdw OPTIONS (host '} . $node->host . qq{', port '} . $node->port . qq{', dbname 'd');
	CREATE USER MAPPING FOR wad SERVER loop OPTIONS (user 'wad', password 'pw');
	CREATE USER MAPPING FOR inh SERVER loop OPTIONS (user 'inh', password 'pw');
	GRANT USAGE ON FOREIGN SERVER loop TO wad, inh;
	CREATE FOREIGN TABLE f_blocks (height bigint, hash_version smallint, prev_hash bytea, tx_merkle_root bytea,
		tx_count int, closed_at timestamptz, block_hash bytea)
		SERVER loop OPTIONS (schema_name 'kingres', table_name 'ledger_blocks');
	CREATE FOREIGN TABLE f_pending (ledger_txid bigint, user_name name, queued_at timestamptz)
		SERVER loop OPTIONS (schema_name 'kingres', table_name 'ledger_pending');
	GRANT ALL ON f_blocks, f_pending TO wad, inh;
});
foreach my $c (
	[ 'wad', q{INSERT INTO f_blocks (height, hash_version, prev_hash, tx_merkle_root, tx_count, closed_at, block_hash)
		VALUES (900, 1, sha256('a'), sha256('b'), 1, now(), sha256('c'))}, qr/cannot write to table "ledger_blocks"/ ],
	[ 'wad', q{BEGIN; SELECT * FROM f_pending FOR UPDATE; COMMIT}, qr/ERROR/ ],
	[ 'wad', q{DELETE FROM f_pending}, qr/ERROR/ ],
	[ 'inh', q{UPDATE f_blocks SET tx_count = 0}, qr/ERROR/ ])
{
	my ($role, $stmt, $re) = @$c;
	my ($ret, $out, $err) = as_role($role, $stmt);
	like($err, $re, "postgres_fdw as $role: the statement on a foreign table of the ledger is refused");
	is(nothing_stuck(), 'ok', 'and nothing of the ledger is held or stopped');
}
is($node->safe_psql('d', q{SELECT count(*) FROM kingres.ledger_verify()}), '0', 'the ledger verifies after the loopbacks');

#--------------------------------------------------------------------------
# CREATE SUBSCRIPTION with the initial copy into an application ledger table
#--------------------------------------------------------------------------
my $pub = PostgreSQL::Test::Cluster->new('notcov_pub');
$pub->init(allows_streaming => 'logical');
$pub->start;
$pub->safe_psql('postgres', q{
	CREATE TABLE feed (id int PRIMARY KEY, body text);
	INSERT INTO feed VALUES (1, 'one'), (2, 'two'), (3, 'three');
	CREATE PUBLICATION pf FOR TABLE feed;
});
$node->safe_psql('d', q{
	CREATE TABLE feed (id int PRIMARY KEY, body text);
	SELECT kingres.ledger_enable('feed');
});
my $pubconn = $pub->connstr('postgres');
$pubconn =~ s/'/''/g;
$node->safe_psql('d', "CREATE SUBSCRIPTION sf CONNECTION '$pubconn' PUBLICATION pf WITH (copy_data = true)");
# the subscription is in database d: pg_subscription_rel is a catalog of the database, and the default of the
# helper (postgres) would find it empty there and not wait for the copy at all
$node->wait_for_subscription_sync($pub, 'sf', 'd');
is($node->safe_psql('d', q{SELECT count(*) FROM feed}), '3', 'the initial copy filled the ledger table');
is($node->safe_psql('d', q{SELECT count(*) FROM kingres.ledger_entry
	WHERE table_id = (SELECT table_id FROM kingres.ledger_table WHERE relid = 'feed'::regclass)}),
	'3', 'and every copied row was captured (the triggers are ENABLE ALWAYS)');
$pub->safe_psql('postgres', q{INSERT INTO feed VALUES (4, 'four')});
$pub->wait_for_catchup('sf');
is($node->safe_psql('d', q{SELECT count(*) FROM kingres.ledger_entry
	WHERE table_id = (SELECT table_id FROM kingres.ledger_table WHERE relid = 'feed'::regclass)}),
	'4', 'and so is a row that the apply worker inserts afterwards');
$pub->safe_psql('postgres', q{UPDATE feed SET body = 'changed' WHERE id = 1});
my $log_offset = -s $node->logfile;
ok($node->wait_for_log(qr/append-only|cannot update|"feed"/, $log_offset),
	'an update that arrives on an append-only ledger table is refused by the apply worker');
$node->safe_psql('d', q{ALTER SUBSCRIPTION sf DISABLE});
$node->safe_psql('d', q{ALTER SUBSCRIPTION sf SET (slot_name = NONE)});
$node->safe_psql('d', q{DROP SUBSCRIPTION sf});
is($node->safe_psql('d', q{SELECT kingres.ledger_build_block() IS NOT NULL}), 't', 'a block is built from the copied rows');
is($node->safe_psql('d', q{SELECT count(*) FROM kingres.ledger_verify()}), '0', 'and the ledger verifies');
$pub->stop;

#--------------------------------------------------------------------------
# ATTACH PARTITION of a table that has a foreign key to a ledger table
#--------------------------------------------------------------------------
$node->safe_psql('d', q{
	CREATE TABLE refs (id int, doc int REFERENCES docs (id)) PARTITION BY RANGE (id);
	CREATE TABLE refs_1 (id int, doc int REFERENCES docs (id));
	CREATE TABLE refs_2 (id int, doc int);
	ALTER TABLE refs OWNER TO app;
	ALTER TABLE refs_1 OWNER TO app;
	ALTER TABLE refs_2 OWNER TO app;
});
my ($aret, $aout, $aerr) = as_role('app', q{
	ALTER TABLE refs ATTACH PARTITION refs_1 FOR VALUES FROM (0) TO (100);
	ALTER TABLE refs ATTACH PARTITION refs_2 FOR VALUES FROM (100) TO (200);
	INSERT INTO refs VALUES (1, 1), (150, 1);});
is($aerr, '', 'a table with a foreign key to a ledger table can be attached as a partition of another');
is(nothing_stuck(), 'ok', 'and nothing of the catalog is held or stopped');
($aret, $aout, $aerr) = as_role('app', q{ALTER TABLE docs ATTACH PARTITION refs_2 FOR VALUES FROM (0) TO (1)});
like($aerr, qr/ERROR/, 'a ledger table takes no partition');
($aret, $aout, $aerr) = as_role('app', q{CREATE TABLE refs_3 (id int); ALTER TABLE refs_3 ATTACH PARTITION docs FOR VALUES FROM (0) TO (1)});
like($aerr, qr/ERROR/, 'and is not attached as one');
is($node->safe_psql('d', q{SELECT count(*) FROM kingres.ledger_verify()}), '0', 'the ledger verifies');

#--------------------------------------------------------------------------
# DROP EXTENSION plpgsql CASCADE racing the first ledger_enable
#--------------------------------------------------------------------------
# Whatever the order, a database never ends with a table that has the capture
# trigger of the ledger and no ledger.  The statements are run in both orders,
# the second session waiting until the first is either blocked or done.
my %outcome;
for my $i (1 .. 6)
{
	my $db = "race$i";
	$node->safe_psql('postgres', "CREATE DATABASE $db");
	$node->safe_psql($db, q{CREATE EXTENSION kingres_ledger; CREATE TABLE t (id int)});
	my ($first, $second);
	{
		local $ENV{PGPASSWORD} = 'pw';
		local $ENV{PGUSER} = 'inh';
		$first = $node->background_psql($db, on_error_stop => 0);
	}
	$second = $node->background_psql($db, on_error_stop => 0);
	my $pid1 = $first->query_safe('SELECT pg_backend_pid()');
	my $pid2 = $second->query_safe('SELECT pg_backend_pid()');
	$pid1 =~ s/\s+//g;
	$pid2 =~ s/\s+//g;
	my $order;
	if ($i % 2)
	{
		$order = 'drop first';
		$first->query('BEGIN; DROP EXTENSION plpgsql CASCADE;');
		$second->query_until(qr/started/, "\\echo started\nSET client_min_messages = error;\nSELECT kingres.ledger_enable('t');\nSELECT 'e_done';\n");
		# wait until the enable is blocked or has finished
		$node->poll_query_until($db, qq{SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE pid = $pid2
			AND (wait_event_type = 'Lock' OR (state LIKE 'idle%' AND query LIKE '%e_done%')))});
		$first->query($i % 4 == 1 ? 'COMMIT' : 'ROLLBACK');
	}
	else
	{
		$order = 'enable first';
		$second->query_safe(q{SET client_min_messages = error});
		$second->query_safe(q{SELECT kingres.ledger_enable('t')});
		$first->query_until(qr/started/, "\\echo started\nDROP EXTENSION plpgsql CASCADE;\nSELECT 'd_done';\n");
		$node->poll_query_until($db, qq{SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE pid = $pid1
			AND (wait_event_type = 'Lock' OR (state LIKE 'idle%' AND query LIKE '%d_done%')))});
	}
	$second->query('SELECT 1');
	$first->query('SELECT 1');
	$first->quit;
	$second->quit;
	my $state = $node->safe_psql($db, q{SELECT
		(SELECT count(*) FROM pg_extension WHERE extname = 'kingres_ledger') || ' '
		|| (SELECT count(*) FROM pg_trigger WHERE tgname = 'kingres_ledger_capture')});
	$outcome{"$order: $state"}++;
	ok($state eq '1 1' || $state eq '1 0' || $state eq '0 0',
		"race $i ($order): the database has the ledger ($state) or no trace of it, never a capture trigger without it");
}
diag(join('; ', map { "$_ x $outcome{$_}" } sort keys %outcome));

done_testing();
