# contrib/kingres_ledger/t/006_guard_without_hook.pl
#
# The point-of-action layer on its own: the event triggers of the extension
# enforce the rules even where the ProcessUtility hook is not there, for
# example after the library was removed from shared_preload_libraries.  The
# ledger is set up with the hook, the server is restarted without it, and each
# statement that would switch the ledger off is tried.  This also shows what
# only the hook stops: a column rename.
#
# Portions Copyright (c) 2026, RooyeKhat Media Co
# SPDX-License-Identifier: AGPL-3.0-only

use strict;
use warnings FATAL => 'all';

use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;

my $node = PostgreSQL::Test::Cluster->new('guard');
$node->init;
$node->append_conf('postgresql.conf',
	"shared_preload_libraries = 'kingres_ledger'\n");
$node->start;

sub q1
{
	return $node->safe_psql('postgres', shift);
}

q1(q(
CREATE EXTENSION kingres_ledger;
CREATE SCHEMA app;
CREATE TYPE app.base AS ENUM ('a', 'b');
CREATE DOMAIN app.dom AS app.base;
CREATE TYPE app.pair AS (a int, b int);
CREATE TABLE app.led (id int PRIMARY KEY, v varchar(10), n int);
CREATE TABLE app.d (id int, d app.dom);
SELECT kingres.ledger_enable('app.led');
SELECT kingres.ledger_enable('app.d');
INSERT INTO app.led VALUES (1, 'one', 10), (2, 'two', 20);
INSERT INTO app.d VALUES (1, 'a');
));
my $digest = q1('SELECT kingres.ledger_generate_digest()');

# Restart without the library in shared_preload_libraries: no hook.
$node->append_conf('postgresql.conf', "shared_preload_libraries = ''\n");
$node->restart;
is(q1('SELECT kingres.ledger_hook_active()'), 'f', 'the hook is not installed');

my @must_fail = (
	'ALTER TABLE app.led DISABLE TRIGGER ALL',
	'ALTER TABLE app.led DISABLE TRIGGER kingres_ledger_capture',
	'ALTER TABLE app.led ENABLE REPLICA TRIGGER kingres_ledger_block_modify',
	'DROP TRIGGER kingres_ledger_block_modify ON app.led',
	'DROP TRIGGER kingres_ledger_capture ON app.led',
	'DROP TABLE app.led',
	'DROP TABLE app.led CASCADE',
	'DROP SCHEMA app CASCADE',
	'ALTER TABLE app.led DROP COLUMN n',
	'ALTER TABLE app.led SET UNLOGGED',
	'ALTER TABLE app.led ALTER COLUMN n TYPE bigint',
	'CREATE TABLE app.child () INHERITS (app.led)',
	'ALTER TABLE app.led ADD COLUMN c app.pair',
	'ALTER TABLE app.led ADD COLUMN c app.pair[]',
	'ALTER TABLE app.led ADD COLUMN g int GENERATED ALWAYS AS (id + 1) STORED',
	'CREATE RULE r AS ON INSERT TO app.led DO INSTEAD NOTHING',
	'DROP TYPE app.base CASCADE',
	'DROP DOMAIN app.dom CASCADE',
	'ALTER TABLE kingres.ledger_blocks DISABLE TRIGGER ALL',
	'ALTER TABLE kingres.ledger_entry ENABLE ROW LEVEL SECURITY',
	'CREATE POLICY p ON kingres.ledger_entry USING (false)',
);
foreach my $stmt (@must_fail)
{
	my ($ret, $stdout, $stderr) = $node->psql(
		'postgres', $stmt,
		extra_params => [ '-v', 'VERBOSITY=sqlstate' ]);
	isnt($ret, 0, "rejected without the hook: $stmt");
	like($stderr, qr/KL002/, "by the event trigger: $stmt");
}

is(q1('SELECT count(*) FROM kingres.ledger_verify()'), '0',
	'the ledger still verifies after all of them');
is(q1('SELECT count(*) FROM app.led'), '2', 'and no row was lost');
is(q1(qq(SELECT kingres.ledger_verify_digest('$digest'))), 't',
	'the digest from before still matches');
is( q1(q(SELECT count(*) FROM pg_trigger WHERE tgname LIKE 'kingres_ledger%' AND tgenabled <> 'A')),
	'0', 'every trigger is still ENABLE ALWAYS');

# What the event triggers cannot see: a column rename (the point-of-action
# layer has no memory of the names of the columns).  It is detected: the
# hashes of the rows change.
q1('ALTER TABLE app.led RENAME COLUMN v TO w');
like( q1(q(SELECT string_agg(check_name, ',') FROM kingres.ledger_verify())),
	qr/table_rows/, 'a column rename without the hook is reported by ledger_verify');
q1('ALTER TABLE app.led RENAME COLUMN w TO v');
is(q1('SELECT count(*) FROM kingres.ledger_verify()'), '0', 'and undone');

# The event triggers themselves can be switched off without the hook; the
# verification says so.
q1('ALTER EVENT TRIGGER kingres_ledger_guard_end DISABLE');
like( q1(q(SELECT string_agg(check_name, ',') FROM kingres.ledger_verify())),
	qr/event_trigger/, 'a disabled event trigger is reported by ledger_verify');
q1('ALTER EVENT TRIGGER kingres_ledger_guard_end ENABLE ALWAYS');
is(q1('SELECT count(*) FROM kingres.ledger_verify()'), '0', 'and enabled again');

$node->stop;
done_testing();
