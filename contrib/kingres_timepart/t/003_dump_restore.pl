# contrib/kingres_timepart/t/003_dump_restore.pl
#
# pg_dump and restore of a database with policies and a history of runs:
# policies, runs and actions come back unchanged, the numbering of runs goes
# on after the last one, the OID kept in a policy is only a hint, and a run on
# the restored database does what the same run on the original does.  Done
# with the custom format and with plain SQL.
#
# Portions Copyright (c) 2026, RooyeKhat Media Co
# SPDX-License-Identifier: AGPL-3.0-only

use strict;
use warnings FATAL => 'all';

use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;

my $node = PostgreSQL::Test::Cluster->new('dump');
$node->init;
$node->start;

sub q1
{
	my ($db, $sql) = @_;
	return $node->safe_psql($db, "SET timezone = 'UTC'; $sql");
}

q1('postgres', q{
CREATE EXTENSION kingres_timepart;
CREATE SCHEMA app;
-- a table with policy, rows, runs that create and drop
CREATE TABLE app.ev (id int, ts timestamptz NOT NULL, v text) PARTITION BY RANGE (ts);
SELECT kingres.create_time_partitions('app.ev', '1 day', '2026-03-03 UTC', '2026-03-01 UTC');
INSERT INTO app.ev SELECT g, timestamptz '2026-03-01 UTC' + g * interval '3 hours', 'row ' || g FROM generate_series(0, 20) g;
SELECT kingres.timepart_set_policy('app.ev', '1 day', create_ahead => '3 days', retention => '5 days', run_every => '2 hours');
-- a table whose run ended partial (a name that is taken)
CREATE TABLE app.clash (ts timestamptz NOT NULL) PARTITION BY RANGE (ts);
CREATE TABLE app.clash_p20260311 (x int);
SELECT kingres.timepart_set_policy('app.clash', '1 day', create_ahead => '2 days');
-- detach instead of drop, a zone with daylight saving
CREATE TABLE app.det (ts timestamptz NOT NULL) PARTITION BY RANGE (ts);
SELECT kingres.create_time_partitions('app.det', '1 day', '2026-03-02 UTC', '2026-03-01 UTC');
SELECT kingres.timepart_set_policy('app.det', '1 day', create_ahead => '1 day', retention => '2 days',
       retention_action => 'detach', timezone => 'Europe/Berlin');
-- a date key in another zone, disabled
CREATE TABLE app.dd (d date NOT NULL) PARTITION BY RANGE (d);
SELECT kingres.timepart_set_policy('app.dd', '1 month', create_ahead => '40 days', timezone => 'Asia/Tokyo', enabled => false,
       columnar_after => '60 days', columnar_sort => '{d}', columnar_options => '{"compression": "zstd", "level": 3}');
-- a run that fails as a whole (too many partitions for one run)
CREATE TABLE app.big (ts timestamptz NOT NULL) PARTITION BY RANGE (ts);
SELECT kingres.timepart_set_policy('app.big', '1 hour', create_ahead => '2 years');
-- awkward names
CREATE SCHEMA "Odd ""schema""";
CREATE TABLE "Odd ""schema""".""""";--" (ts timestamptz NOT NULL) PARTITION BY RANGE (ts);
SELECT kingres.timepart_set_policy('"Odd ""schema""".""""";--"', '1 week', create_ahead => '10 days');
});

foreach my $r (
	[ 'app.ev',    '2026-03-04 12:00 UTC' ],
	[ 'app.clash', '2026-03-10 05:00 UTC' ],
	[ 'app.big',   '2026-03-10 05:00 UTC' ],
	[ 'app.det',   '2026-03-05 06:00 UTC' ],
	[ '"Odd ""schema""".""""";--"', '2026-03-05 00:00 UTC' ],
	[ 'app.ev',    '2026-03-12 00:00 UTC' ],)
{
	$node->safe_psql('postgres',
		"SET timezone = 'UTC'; CALL kingres.timepart_run('$r->[0]', '$r->[1]');");
}
# a run left running: begin without finish, then its session ends
$node->safe_psql('postgres', q{SELECT kingres.timepart_begin_run('app.det', '2026-03-06 UTC', 'manual')});

# What must come back: every row of the three tables, but for the OID hint.
my $fingerprint = q{
SELECT 'policy', count(*), md5(string_agg((to_jsonb(p) - 'parent_oid')::text, '|' ORDER BY p.schema_name, p.table_name)) FROM kingres.timepart_policy p
UNION ALL
SELECT 'run', count(*), md5(string_agg(to_jsonb(r)::text, '|' ORDER BY r.run_id)) FROM kingres.timepart_run r
UNION ALL
SELECT 'action', count(*), md5(string_agg(to_jsonb(a)::text, '|' ORDER BY a.run_id, a.seq)) FROM kingres.timepart_run_action a
UNION ALL
SELECT 'partitions', count(*), md5(string_agg(c.relname, '|' ORDER BY c.relname))
  FROM pg_class c WHERE c.relnamespace IN ('app'::regnamespace) AND c.relkind IN ('r', 'p')
ORDER BY 1};
my $before = q1('postgres', $fingerprint);
like($before, qr/^action\|\d+\|/m, 'the original has history');
my ($nruns) = q1('postgres', q{SELECT count(*) FROM kingres.timepart_run}) =~ /(\d+)/;
cmp_ok($nruns, '>=', 6, 'with runs of several kinds');
is(q1('postgres', q{SELECT string_agg(DISTINCT state, ',' ORDER BY state) FROM kingres.timepart_run}),
	'done,failed,partial,running', 'in every state');

my $dumpfile = $node->basedir . '/db.dump';
$node->command_ok([ 'pg_dump', '-Fc', '-f', $dumpfile, $node->connstr('postgres') ], 'pg_dump, custom format');

$node->safe_psql('postgres', 'CREATE DATABASE restored');
$node->command_ok([ 'pg_restore', '-d', $node->connstr('restored'), $dumpfile ], 'pg_restore');

is(q1('restored', $fingerprint), $before, 'policies, runs, actions and partitions are the same after the restore');
is(q1('restored', q{SELECT count(*) FROM pg_event_trigger WHERE evtname LIKE 'kingres_timepart%' AND evtenabled = 'A'}),
	'2', 'the event triggers are there');
is(q1('restored', q{SELECT count(*) FROM pg_extension WHERE extname = 'kingres_timepart'}), '1', 'and the extension');

# The OID in the policy is a hint: the restored tables have other OIDs.
is(q1('restored', q{SELECT count(*) FROM kingres.timepart_policy p WHERE p.parent_oid = to_regclass(format('%I.%I', p.schema_name, p.table_name))::oid}),
	'0', 'the hints are stale after a restore');
# The old hint may even be the OID of another table; renames use the hint
# only when the named table is gone, so they cannot move a policy by mistake.
q1('restored', q{CREATE TABLE app.unrelated (ts timestamptz NOT NULL) PARTITION BY RANGE (ts);
	UPDATE kingres.timepart_policy SET parent_oid = 'app.unrelated'::regclass::oid WHERE table_name = 'dd';
	ALTER TABLE app.unrelated RENAME TO unrelated2;});
is(q1('restored', q{SELECT count(*) FROM kingres.timepart_policy WHERE table_name = 'dd'}), '1',
	'a stale hint that names another table does not move a policy');
q1('restored', q{DROP TABLE app.unrelated2});
is(q1('restored', q{SELECT count(*) FROM kingres.timepart_policy WHERE table_name = 'dd'}), '1',
	'nor does it drop one');
# A policy with a stale hint is found by name, and the hint is refreshed.
my $maxrun = q1('restored', q{SELECT max(run_id) FROM kingres.timepart_run});
$node->safe_psql('restored', q{SET timezone = 'UTC'; CALL kingres.timepart_run('app.ev', '2026-03-14 00:00 UTC');});
$node->safe_psql('postgres', q{SET timezone = 'UTC'; CALL kingres.timepart_run('app.ev', '2026-03-14 00:00 UTC');});
is(q1('restored', q{SELECT p.parent_oid = 'app.ev'::regclass::oid FROM kingres.timepart_policy p WHERE table_name = 'ev'}),
	't', 'a run found the table by name and refreshed the hint');
cmp_ok(q1('restored', q{SELECT max(run_id) FROM kingres.timepart_run}), '>', $maxrun,
	'the numbering of runs goes on after the last one restored');
is(q1('restored', q{SELECT count(*) FROM (SELECT run_id FROM kingres.timepart_run GROUP BY run_id HAVING count(*) > 1) d}),
	'0', 'no run number is used twice');

# The same run on both: the same partitions.
my $parts = q{SELECT string_agg(c.relname || ' ' || pg_get_expr(c.relpartbound, c.oid), '|' ORDER BY c.relname)
	FROM pg_inherits i JOIN pg_class c ON c.oid = i.inhrelid WHERE i.inhparent = 'app.ev'::regclass};
is(q1('restored', $parts), q1('postgres', $parts), 'a run on the restored database does what the same run on the original does');
is(q1('restored', q{SELECT state FROM kingres.timepart_run WHERE run_id = (SELECT max(run_id) FROM kingres.timepart_run)}),
	'done', 'and is done');
# The abandoned run is marked interrupted by the next begin on its table.
$node->safe_psql('restored', q{SET timezone = 'UTC'; CALL kingres.timepart_run('app.det', '2026-03-07 UTC');});
is(q1('restored', q{SELECT count(*) FROM kingres.timepart_run WHERE state = 'running'}), '0',
	'a run that was running in the dump is not live in the restored database: the next run marks it');
is(q1('restored', q{SELECT error FROM kingres.timepart_run WHERE table_name = 'det' AND error IS NOT NULL}), 'interrupted', 'as interrupted');

# A rename after the restore still moves the policy: the table is found by
# name when the hint is stale, and the rename trigger goes by the new OID.
q1('restored', q{ALTER TABLE app.det RENAME TO det2});
is(q1('restored', q{SELECT table_name FROM kingres.timepart_policy WHERE table_name LIKE 'det%'}), 'det2',
	'a rename after the restore moves the policy');

# Plain SQL.
my $plain = $node->basedir . '/db.sql';
$node->command_ok([ 'pg_dump', '-f', $plain, $node->connstr('postgres') ], 'pg_dump, plain format');
$node->safe_psql('postgres', 'CREATE DATABASE restored_plain');
$node->command_ok([ 'psql', '-X', '-q', '-v', 'ON_ERROR_STOP=1', '-d', $node->connstr('restored_plain'), '-f', $plain ], 'restore from plain SQL');
is(q1('restored_plain', q{SELECT 'policy', count(*) FROM kingres.timepart_policy UNION ALL SELECT 'run', count(*) FROM kingres.timepart_run
	UNION ALL SELECT 'action', count(*) FROM kingres.timepart_run_action ORDER BY 1}),
	q1('postgres', q{SELECT 'policy', count(*) FROM kingres.timepart_policy UNION ALL SELECT 'run', count(*) FROM kingres.timepart_run
	UNION ALL SELECT 'action', count(*) FROM kingres.timepart_run_action ORDER BY 1}),
	'the plain dump carries policies, runs and actions');
my $nextid = q1('restored_plain', q{SELECT kingres.timepart_begin_run('app.ev', '2026-03-20 UTC', 'manual')});
cmp_ok($nextid, '>', q1('postgres', q{SELECT max(run_id) FROM kingres.timepart_run}),
	'run numbers go on in the plain restore too');

# Without the history: a schema-only dump has no policies and no runs.
my $schemaonly = $node->basedir . '/schema.sql';
$node->command_ok([ 'pg_dump', '-s', '-f', $schemaonly, $node->connstr('postgres') ], 'pg_dump, schema only');
$node->safe_psql('postgres', 'CREATE DATABASE restored_schema');
$node->command_ok([ 'psql', '-X', '-q', '-v', 'ON_ERROR_STOP=1', '-d', $node->connstr('restored_schema'), '-f', $schemaonly ], 'restore of the schema only');
is(q1('restored_schema', q{SELECT count(*) FROM kingres.timepart_policy}), '0', 'no policies in a schema-only dump');

# K12R-01: a dump by a role that is not a superuser.  The policy and the
# history are plain tables that grant nothing to PUBLIC (no row-level
# security), so a role that can read every table dumps them; the restore of
# that dump gives the same policies and history.
$node->safe_psql('postgres', q{
CREATE ROLE backup LOGIN IN ROLE pg_read_all_data;
CREATE ROLE dbown LOGIN;
CREATE ROLE dbown_reader LOGIN IN ROLE pg_read_all_data;
CREATE ROLE nobody LOGIN;
});
my $now = q1('postgres', $fingerprint);
sub dump_as
{
	my ($role, $file, @fmt) = @_;
	return $node->command_ok(
		[ 'pg_dump', @fmt, '-U', $role, '-f', $file, $node->connstr('postgres') ],
		"pg_dump as $role (@fmt)");
}
sub restore_check
{
	my ($file, $db, $what) = @_;
	$node->safe_psql('postgres', "CREATE DATABASE $db");
	$node->command_ok([ 'psql', '-X', '-q', '-v', 'ON_ERROR_STOP=1', '-d', $node->connstr($db), '-f', $file ],
		"restore of the dump of $what");
	is(q1($db, $fingerprint), $now, "policies, runs, actions and partitions after the restore of the dump of $what are the same");
}

# The superuser, as a reference.
my $d_super = $node->basedir . '/d_super.sql';
my $d_backup = $node->basedir . '/d_backup.sql';
dump_as($node->safe_psql('postgres', 'SELECT current_user'), $d_super);
dump_as('backup', $d_backup);
restore_check($d_super, 'r_super', 'the superuser');
restore_check($d_backup, 'r_backup', 'a role with pg_read_all_data only');
sub norm { my $t = slurp_file($_[0]); $t =~ s{^\\(?:un)?restrict .*$}{}mg; return $t; }
is(norm($d_backup), norm($d_super),
	'the dump of the backup role is the dump of the superuser, byte for byte');
my $d_backup_c = $node->basedir . '/d_backup.dump';
$node->command_ok([ 'pg_dump', '-Fc', '-U', 'backup', '-f', $d_backup_c, $node->connstr('postgres') ],
	'pg_dump as the backup role, custom format');
$node->safe_psql('postgres', 'CREATE DATABASE r_backup_c');
$node->command_ok([ 'pg_restore', '-d', $node->connstr('r_backup_c'), $d_backup_c ], 'pg_restore of that dump');
is(q1('r_backup_c', $fingerprint), $now, 'and the same policies and history come back');
$node->command_ok([ 'pg_dump', '-U', 'backup', '-t', 'kingres.timepart_policy_data', '-f', '/dev/null', $node->connstr('postgres') ],
	'pg_dump of the policy table alone by the backup role');
$node->command_ok([ 'pg_dump', '-U', 'backup', '--data-only', '--inserts', '-f', '/dev/null', $node->connstr('postgres') ],
	'pg_dump with INSERT statements by the backup role');

# The database owner: with pg_read_all_data it dumps, without it it is a role
# like any other that does not own the tables of the extension.
$node->safe_psql('postgres', 'ALTER DATABASE postgres OWNER TO dbown');
my $d_dbown = $node->basedir . '/d_dbown.sql';
$node->safe_psql('postgres', 'GRANT pg_read_all_data TO dbown');
dump_as('dbown', $d_dbown);
restore_check($d_dbown, 'r_dbown', 'the database owner');
$node->safe_psql('postgres', 'REVOKE pg_read_all_data FROM dbown');
command_fails_like(
	[ 'pg_dump', '-U', 'dbown', '-f', '/dev/null', $node->connstr('postgres') ],
	qr/permission denied for table timepart_|permission denied for/,
	'a database owner without pg_read_all_data is refused by name, not by row-level security');
command_fails_like(
	[ 'pg_dump', '-U', 'nobody', '-f', '/dev/null', $node->connstr('postgres') ],
	qr/permission denied/,
	'and so is a role with no rights');

# The restore of the dump needs a superuser, as the extension is not trusted
# and the restore writes the policies as they were: the database owner is
# refused with the permission error and nothing is half restored.
$node->safe_psql('postgres', 'CREATE DATABASE r_owner_try OWNER dbown');
$node->safe_psql('r_owner_try', 'CREATE EXTENSION kingres_timepart');
$node->command_fails([ 'pg_restore', '-U', 'dbown', '-d', $node->connstr('r_owner_try'), $d_backup_c ],
	'a restore by the database owner is refused');
is(q1('r_owner_try', q{SELECT count(*) FROM kingres.timepart_policy}), '0', 'and no policy came in');
is(q1('r_owner_try', q{SELECT count(*) FROM kingres.timepart_run}), '0', 'nor a run');
$node->safe_psql('postgres', 'CREATE DATABASE r_owner_try2 OWNER dbown');
$node->safe_psql('r_owner_try2', 'CREATE EXTENSION kingres_timepart');
$node->command_fails_like([ 'pg_restore', '--no-owner', '-U', 'dbown', '-d', $node->connstr('r_owner_try2'), $d_backup_c ],
	qr/permission denied for schema kingres/, 'also without the owners: the database owner has no access to the schema of the extension');
is(q1('r_owner_try2', q{SELECT count(*) FROM kingres.timepart_policy_data}), '0', 'and still no policy');

# CREATE EXTENSION in a database whose schema kingres was made by something
# else (another KingresDB extension), then dump and restore.
$node->safe_psql('postgres', 'CREATE DATABASE has_schema');
$node->safe_psql('has_schema', q{CREATE SCHEMA kingres; CREATE TABLE kingres.other (x int); INSERT INTO kingres.other VALUES (1);
	CREATE EXTENSION kingres_timepart;
	CREATE TABLE public.t (ts timestamptz NOT NULL) PARTITION BY RANGE (ts);
	SELECT kingres.timepart_set_policy('public.t', '1 day');});
is(q1('has_schema', q{SELECT count(*) FROM kingres.timepart_policy}), '1', 'the extension is created in an existing schema kingres');
my $d_has = $node->basedir . '/d_has.sql';
$node->command_ok([ 'pg_dump', '-U', 'backup', '-f', $d_has, $node->connstr('has_schema') ], 'dump of it by the backup role');
$node->safe_psql('postgres', 'CREATE DATABASE has_schema2');
$node->command_ok([ 'psql', '-X', '-q', '-v', 'ON_ERROR_STOP=1', '-d', $node->connstr('has_schema2'), '-f', $d_has ], 'and its restore');
is(q1('has_schema2', q{SELECT count(*) FROM kingres.timepart_policy}) . q1('has_schema2', q{SELECT x FROM kingres.other}), '11',
	'with the policy and the table of the other owner of the schema');

$node->stop;
done_testing();
