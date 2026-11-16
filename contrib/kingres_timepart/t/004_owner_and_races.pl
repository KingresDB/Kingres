# contrib/kingres_timepart/t/004_owner_and_races.pl
#
# Three things that need a server of their own:
#
#   * the default: a server that nobody told to trust the owners of tables
#     does not run their code in the session of a superuser (K12T-02); what
#     the owner's code can do from inside, with the privileges of the owner
#     (COPY ... PROGRAM, a connection by dblink), is what PostgreSQL gives that
#     role and no more;
#   * the event triggers of the extension against a run: a rename, a drop and
#     a change of the owner of the table between two actions and while one is
#     waiting for its lock.  The waiting is made by a lock that the test holds
#     and releases; nothing here depends on how long anything takes;
#   * a policy that a hostile dump restores: a row shaped by hand in a dump,
#     restored by pg_restore, does not drop the partitions of a table that
#     never had a policy (K12T-03), and does after a superuser's explicit
#     adoption, which says so.
#
# Portions Copyright (c) 2026, RooyeKhat Media Co
# SPDX-License-Identifier: AGPL-3.0-only

use strict;
use warnings FATAL => 'all';

use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;

my $node = PostgreSQL::Test::Cluster->new('races');
$node->init;
$node->append_conf('postgresql.conf', q{
kingres_timepart.create_lock_timeout_ms = 60000
kingres_timepart.retire_lock_timeout_ms = 60000
});
$node->start;

sub q1
{
	my ($sql, %opt) = @_;
	return $node->safe_psql('postgres', "SET timezone = 'UTC'; $sql", %opt);
}

$node->safe_psql('postgres', q{
CREATE EXTENSION kingres_timepart;
ALTER DATABASE postgres SET search_path = public;
CREATE ROLE rc_owner LOGIN;
CREATE ROLE rc_other LOGIN;
GRANT CREATE ON SCHEMA public TO rc_owner, rc_other;
GRANT USAGE ON SCHEMA kingres TO rc_owner, rc_other;
GRANT EXECUTE ON FUNCTION
    kingres.create_time_partitions(regclass, interval, timestamptz, timestamptz, name, text),
    kingres.timepart_set_policy(regclass, interval, interval, interval, text, interval, text[], jsonb, text, interval, boolean)
  TO rc_owner, rc_other;
});
my $su = $node->safe_psql('postgres', 'SELECT current_user');

# ---- 1. the default: the code of an owner is not run in the session of a superuser ----
q1(q{
SET ROLE rc_owner;
CREATE TABLE own1 (ts timestamptz NOT NULL) PARTITION BY RANGE (ts);
SELECT kingres.timepart_set_policy('own1', '1 day', create_ahead => '1 day');
RESET ROLE;
});
my ($ret, $out, $err);
($ret, $out, $err) = $node->psql('postgres', q{SET timezone = 'UTC'; CALL kingres.timepart_run('own1', '2026-03-03 UTC');},
	stdout => \$out, stderr => \$err);
is(q1(q{SELECT state || ' ' || left(error, 29) FROM kingres.timepart_run WHERE table_name = 'own1'}),
	'failed 42501: owner_code_not_trusted', 'a server where nobody set the setting refuses the code of the owner');
is(q1(q{SELECT count(*) FROM pg_inherits WHERE inhparent = 'own1'::regclass}), '0', 'and nothing was made');
# the owner drives its own table from its own session
$node->safe_psql('postgres', q{SELECT kingres.timepart_grant_driver('rc_owner')});
($ret, $out, $err) = $node->psql('postgres', q{SET timezone = 'UTC'; CALL kingres.timepart_run('own1', '2026-03-03 UTC');},
	extra_params => [ '-U', 'rc_owner' ], stdout => \$out, stderr => \$err);
is($ret, 0, 'the owner runs its own table from its own session');
is(q1(q{SELECT state FROM kingres.timepart_run WHERE table_name = 'own1' ORDER BY run_id DESC LIMIT 1}), 'done', 'and it is done');
is(q1(q{SELECT string_agg(DISTINCT pg_get_userbyid(c.relowner), ',') FROM pg_inherits i JOIN pg_class c ON c.oid = i.inhrelid WHERE i.inhparent = 'own1'::regclass}),
	'rc_owner', 'its partitions are its own');

# What the code of the owner can do from inside, as the owner: nothing that the
# role could not do in a session of its own.  (Run here the way the maintenance
# would run it for a superuser that trusts its owners.)
my $dblink = $node->safe_psql('postgres', q{SELECT count(*) FROM pg_available_extensions WHERE name = 'dblink'});
SKIP:
{
	skip 'dblink is not installed', 3 unless $dblink eq '1';
	$node->safe_psql('postgres', q{CREATE EXTENSION dblink SCHEMA public});
	my $host = $node->host;
	my $port = $node->port;
	($ret, $out, $err) = $node->psql('postgres', qq{SET kingres_timepart.trust_table_owners = on;
		SELECT kingres.timepart__exec_as('rc_owner'::regrole::oid, ARRAY[\$\$SELECT public.dblink_connect('host=$host port=$port dbname=postgres user=$su')\$\$]);},
		stdout => \$out, stderr => \$err);
	like($err, qr/password or GSSAPI delegated credentials required|password is required/,
		'dblink from inside: a role that is not a superuser cannot connect to a server that asks for no password');
	($ret, $out, $err) = $node->psql('postgres', qq{SET kingres_timepart.trust_table_owners = on;
		SELECT kingres.timepart__exec_as('rc_owner'::regrole::oid, ARRAY[\$\$SELECT public.dblink_connect_u('host=$host port=$port dbname=postgres user=$su')\$\$]);},
		stdout => \$out, stderr => \$err);
	like($err, qr/permission denied for function dblink_connect_u/, 'dblink_connect_u is not for a role that is not a superuser');
	($ret, $out, $err) = $node->psql('postgres', qq{SET kingres_timepart.trust_table_owners = on;
		SELECT kingres.timepart__exec_as('rc_owner'::regrole::oid, ARRAY[\$\$COPY (SELECT 1) TO PROGRAM 'true'\$\$]);},
		stdout => \$out, stderr => \$err);
	like($err, qr/permission denied to COPY to or from an external program/, 'COPY ... PROGRAM from inside needs pg_execute_server_program');
}

# ---- 2. races of the event triggers against a run ----
# The table is the superuser's: the code of its owner is the session's own.
q1(q{
CREATE TABLE ev (ts timestamptz NOT NULL) PARTITION BY RANGE (ts);
SELECT kingres.create_time_partitions('ev', '1 day', '2026-03-03 UTC', '2026-03-01 UTC');
SELECT kingres.timepart_set_policy('ev', '1 day', create_ahead => '4 days', retention => '30 days');
});
sub parts
{
	my ($t) = @_;
	return q1(qq{SELECT count(*) FROM pg_inherits WHERE inhparent = '$t'::regclass});
}

# A session that holds the table; a session that is waiting for it, inside one
# action; a session that waits behind that one with a DDL on the table.
sub blocked_with
{
	my ($pattern) = @_;
	$node->poll_query_until('postgres', qq{SELECT count(*) > 0 FROM pg_stat_activity WHERE wait_event_type = 'Lock' AND query LIKE '$pattern'})
		or die "no session is waiting with $pattern";
}

sub race
{
	my ($table, $ddl, $as_of) = @_;
	my $run = q1(qq{SELECT kingres.timepart_begin_run('$table', '$as_of UTC')});
	my $holder = $node->background_psql('postgres', on_error_stop => 0);
	$holder->query_safe(qq{BEGIN; LOCK TABLE $table IN ACCESS EXCLUSIVE MODE;});
	my $action = $node->background_psql('postgres', on_error_stop => 0);
	$action->query_safe(q{SET timezone = 'UTC';});
	$action->query_until(qr/^waiting$/m, "\\echo waiting\nSELECT 'step', kingres.timepart_run_action($run, 'create');\n");
	blocked_with('%timepart_run_action%');
	my $racer = $node->background_psql('postgres', on_error_stop => 0);
	$racer->query_until(qr/^racing$/m, "\\echo racing\n$ddl;\n");
	blocked_with("%" . substr($ddl, 0, 20) . "%");
	$holder->query_safe('COMMIT;');
	$holder->quit;
	$action->query_safe('SELECT 1');
	$racer->query_safe('SELECT 1');
	$action->quit;
	$racer->quit;
	return $run;
}

# 2a. a rename while an action waits: the action is done under the old name,
# the policy and the history move, the next action goes on by the new name.
my $run = race('ev', 'ALTER TABLE ev RENAME TO ev2', '2026-03-03 12:00');
is(q1(qq{SELECT a.kind || ' ' || a.state FROM kingres.timepart_run_action_data a WHERE a.run_id = $run ORDER BY seq LIMIT 1}),
	'create done', 'a rename while an action waits: the action ran, the table was not in the way');
is(q1(q{SELECT table_name FROM kingres.timepart_policy WHERE table_name LIKE 'ev%'}), 'ev2', 'the policy followed the rename');
is(q1(qq{SELECT table_name FROM kingres.timepart_run_data WHERE run_id = $run}), 'ev2', 'and the run');
is(q1(q{SELECT kingres.timepart__is_sealed('public', 'ev2')}), 't', 'and the seal');
is(q1(qq{SELECT kingres.timepart_run_action($run, 'create')}), 't', 'the next action of the same run finds the table by its new name');
q1(qq{CALL kingres.timepart_run_phase($run, 'create')});
q1(qq{SELECT kingres.timepart_finish_run($run)});
is(q1(qq{SELECT state FROM kingres.timepart_run_data WHERE run_id = $run}), 'done', 'and the run ends done');
is(parts('ev2'), '7', 'partitions: the three and the four days ahead');

# 2b. a change of owner between two actions: the next action refuses and does
# nothing; the run says why.  A change of owner while an action waits: the
# action that had the table runs, the next one refuses.
q1(q{SELECT kingres.timepart_set_policy('ev2', '1 day', create_ahead => '8 days', retention => '30 days')});
$run = race('ev2', 'ALTER TABLE ev2 OWNER TO rc_other', '2026-03-03 12:00');
is(q1(qq{SELECT a.state FROM kingres.timepart_run_action_data a WHERE a.run_id = $run ORDER BY seq LIMIT 1}), 'done',
	'an owner change while an action waits: the action that had the table ran');
my $n = parts('ev2');
is(q1(qq{SELECT kingres.timepart_run_action($run, 'create')}), 'f', 'the next action ends the run');
is(q1(qq{SELECT state || ' ' || left(error, 30) FROM kingres.timepart_run_data WHERE run_id = $run}),
	'failed 42501: owner_changed: the owne', 'it failed: owner_changed');
is(parts('ev2'), $n, 'and made nothing');
is(q1(q{SELECT pg_get_userbyid(relowner) FROM pg_class WHERE oid = 'ev2'::regclass}), 'rc_other', 'the new owner owns the table');
is(q1(q{SELECT table_owner FROM kingres.timepart_policy WHERE table_name = 'ev2'}), $su, 'the policy is still the one of the former owner');
is(q1(qq{SELECT kingres.timepart_begin_run('ev2', '2026-03-04 UTC') > 0}), 't', 'a new run is recorded');
is(q1(q{SELECT state FROM kingres.timepart_run_data ORDER BY run_id DESC LIMIT 1}), 'failed', 'as failed: the policy is not the new owner\'s');
# the new owner sets its policy; the former owner's runs stay with him
q1(q{SELECT kingres.timepart_set_policy('ev2', '1 day', create_ahead => '2 days')});
is(q1(q{SELECT table_owner FROM kingres.timepart_policy WHERE table_name = 'ev2'}), 'rc_other', 'the new owner\'s policy');

# 2c. a drop while an action waits: the action that had the table ran, the
# policy is gone with the table, the history stays and no longer belongs to
# a table, and the next action ends the run.
q1(q{
CREATE TABLE ev3 (ts timestamptz NOT NULL) PARTITION BY RANGE (ts);
SELECT kingres.create_time_partitions('ev3', '1 day', '2026-03-03 UTC', '2026-03-01 UTC');
SELECT kingres.timepart_set_policy('ev3', '1 day', create_ahead => '4 days');
});
$run = race('ev3', 'DROP TABLE ev3', '2026-03-03 12:00');
# Which of the two gets the table first is for the lock manager to say: the action ran
# and then the table went, or the table went and the action found it gone.  Either way
# nothing is half done.
like(q1(qq{SELECT count(*) FROM kingres.timepart_run_action_data a WHERE a.run_id = $run AND a.state = 'done'}), qr/^[01]$/,
	'a drop while an action waits: the action ran or found the table gone');
is(q1(q{SELECT count(*) FROM kingres.timepart_policy_data WHERE table_name = 'ev3'}), '0', 'the policy went with the table');
is(q1(q{SELECT count(*) FROM kingres.timepart_policy_seal WHERE table_name = 'ev3'}), '0', 'and its seal');
is(q1(qq{SELECT kingres.timepart_run_action($run, 'create')}), 'f', 'the next action ends the run');
is(q1(qq{SELECT state FROM kingres.timepart_run_data WHERE run_id = $run}), 'failed', 'the run is failed, whichever came first');
is(q1(q{SELECT count(*) FROM pg_class WHERE relname LIKE 'ev3%' AND relnamespace = 'public'::regnamespace}), '0', 'and no partition of the table is left behind');
is(q1(qq{SELECT table_oid IS NULL FROM kingres.timepart_run_data WHERE run_id = $run}), 't', 'the history no longer belongs to a table');
# a table of the same name made by another role is another table
q1(q{SET ROLE rc_other; CREATE TABLE ev3 (ts timestamptz NOT NULL) PARTITION BY RANGE (ts); RESET ROLE});
is(q1(q{SELECT count(*) FROM kingres.timepart_run_data WHERE table_name = 'ev3' AND table_oid = 'ev3'::regclass::oid}), '0',
	'a new table of the name is not the table of the old runs');
is(q1(qq{SET ROLE rc_other; SELECT count(*) FROM kingres.timepart_run WHERE run_id = $run}), '0', 'and its owner does not see them');

# ---- 3. a hostile dump ----
# A table of the owner that never had a policy; a dump made by hand, restored
# by pg_restore.  The row carries what a restore carries: the stamp of
# the installation that made it, the name of the table and of its real owner.
$node->safe_psql('postgres', q{
SET ROLE rc_owner;
CREATE TABLE victim (ts timestamptz NOT NULL) PARTITION BY RANGE (ts);
SELECT kingres.create_time_partitions('victim', '1 day', '2026-05-10 UTC', '2026-05-01 UTC');
CREATE TABLE victim2 (ts timestamptz NOT NULL) PARTITION BY RANGE (ts);
SELECT kingres.create_time_partitions('victim2', '1 day', '2026-05-10 UTC', '2026-05-01 UTC');
RESET ROLE;
});
$node->safe_psql('postgres', 'CREATE DATABASE hostile');
$node->safe_psql('hostile', q{CREATE EXTENSION kingres_timepart});
my $hostile_sql = $node->basedir . '/hostile.sql';
# the second row has the stamp of this installation, which every role can read
my $ours = $node->safe_psql('postgres', 'SELECT kingres.timepart__stamp()');
my $cols = 'schema_name, table_name, parent_oid, partition_every, timezone, create_ahead, retention, retention_action, columnar_after, columnar_sort, columnar_options, run_every, enabled, next_run_at, table_owner, created_by, created_at, updated_at, stamp, table_owner_oid, created_by_oid';
sub row
{
	my ($t, $stamp) = @_;
	return join("\t", 'public', $t, 1, '1 day', 'UTC', '1 day', '1 day', 'drop', '\N', '\N', '\N', '00:05:00', 't',
		'2026-01-01 00:00:00+00', 'rc_owner', 'rc_owner', '2026-01-01 00:00:00+00', '2026-01-01 00:00:00+00', $stamp, 1, 1) . "\n";
}
PostgreSQL::Test::Utils::append_to_file($hostile_sql,
	"SET client_encoding = 'UTF8';\nCOPY kingres.timepart_policy_data ($cols) FROM stdin;\n"
	. row('victim', '11111111-1111-1111-1111-111111111111') . row('victim2', $ours) . "\\.\n");
# the same database, restored with the hostile dump, with the table of the owner
# made in it: restore into a copy of the cluster's database 'postgres'
$node->safe_psql('postgres', 'CREATE DATABASE hostile2 TEMPLATE postgres');
$node->command_ok([ 'psql', '-X', '-q', '-v', 'ON_ERROR_STOP=1', '-d', $node->connstr('hostile2'), '-f', $hostile_sql ], 'the hostile dump is restored');
is($node->safe_psql('hostile2', q{SELECT count(*) FROM kingres.timepart_policy_data WHERE table_name LIKE 'victim%'}), '2', 'its policy rows are there');
foreach my $v ('victim', 'victim2')
{
	$node->safe_psql('hostile2', qq{SET timezone = 'UTC'; SET kingres_timepart.trust_table_owners = on; CALL kingres.timepart_run('$v', '2026-05-20 UTC')});
	is($node->safe_psql('hostile2', qq{SELECT count(*) FROM pg_inherits WHERE inhparent = '$v'::regclass}), '10',
		"$v: the maintenance did not touch the partitions of the table");
	like($node->safe_psql('hostile2', qq{SELECT state || ' ' || error FROM kingres.timepart_run WHERE table_name = '$v'}),
		qr/^failed 42501: policy_unverified: the policy of public\.$v was/, "$v: the run failed and said why");
}
is($node->safe_psql('hostile2', q{SELECT count(*) FROM kingres.timepart_due('2030-01-01 UTC') WHERE table_name LIKE 'victim%'}), '0', 'neither is due');
# an explicit adoption by a superuser, which says what it adopts
($ret, $out, $err) = $node->psql('hostile2', q{SELECT kingres.timepart_adopt()}, stdout => \$out, stderr => \$err);
like($err, qr/NOTICE:  policy of public\.victim adopted: partition_every 1 day, retention 1 day, retention_action drop, owner "rc_owner"/,
	'timepart_adopt() adopts the row of another installation, with a notice that names the table, the retention and the owner');
like($err, qr/WARNING:  policy of public\.victim2 is not adopted: it carries the stamp of this installation but was not written by it/,
	'and not the row that has the stamp of this installation');
$node->safe_psql('hostile2', q{SET timezone = 'UTC'; SET kingres_timepart.trust_table_owners = on; CALL kingres.timepart_run('victim', '2026-05-20 UTC')});
is($node->safe_psql('hostile2', q{SELECT count(*) FROM pg_inherits WHERE inhparent = 'victim'::regclass}), '2',
	'after the adoption the policy acts, as any policy does: that is what the superuser who adopts a dump accepts');

$node->stop;
done_testing();
