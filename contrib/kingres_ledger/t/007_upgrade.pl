# contrib/kingres_ledger/t/007_upgrade.pl
#
# ALTER EXTENSION kingres_ledger UPDATE from 0.1 to 0.2.
#
#   * a database that was created with version 0.1 and holds append-only
#     ledger tables, blocks and digests still verifies after the upgrade, and
#     a digest generated before the upgrade still verifies after it;
#   * installing 0.2 directly and upgrading from 0.1 give the same objects,
#     the same privileges and the same protection (compared through a
#     fingerprint of the catalogs: functions with the hash of their source,
#     tables, columns, constraints, indexes, triggers, event triggers,
#     privileges and comments of every member of the extension);
#   * the upgraded database has the 0.2 features: updatable tables, the
#     records for operators, and every protection rule applies to the new
#     tables.
#
# Portions Copyright (c) 2026, RooyeKhat Media Co
# SPDX-License-Identifier: AGPL-3.0-only

use strict;
use warnings FATAL => 'all';

use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;

my $node = PostgreSQL::Test::Cluster->new('upgrade');
$node->init;
$node->append_conf('postgresql.conf',
	"shared_preload_libraries = 'kingres_ledger'\n"
	  . "kingres_ledger.block_interval = 0\n");
$node->start;

# A fingerprint of everything the extension created, with names instead of
# OIDs, so that two databases can be compared line by line.
my $fingerprint = q{
WITH ext AS (SELECT oid FROM pg_extension WHERE extname = 'kingres_ledger'),
members AS (
  SELECT d.classid, d.objid FROM pg_depend d, ext
   WHERE d.refclassid = 'pg_extension'::regclass AND d.refobjid = ext.oid
     AND d.deptype = 'e'),
funcs AS (
  SELECT 'function ' || p.oid::regprocedure::text
         || ' sec=' || p.prosecdef || ' vol=' || p.provolatile::text
         || ' par=' || p.proparallel::text || ' strict=' || p.proisstrict
         || ' kind=' || p.prokind::text || ' lang=' || l.lanname
         || ' cfg=' || COALESCE(p.proconfig::text, '-')
         || ' acl=' || COALESCE(p.proacl::text, '-')
         || ' src=' || md5(p.prosrc || '|' || COALESCE(p.probin, ''))
         || ' ret=' || pg_get_function_result(p.oid)
         || ' comment=' || COALESCE(obj_description(p.oid, 'pg_proc'), '-') AS line
    FROM members m JOIN pg_proc p ON m.classid = 'pg_proc'::regclass AND p.oid = m.objid
    JOIN pg_language l ON l.oid = p.prolang),
rels AS (
  SELECT c.oid, c.relname, c.relkind, c.relacl, c.relpersistence
    FROM members m JOIN pg_class c ON m.classid = 'pg_class'::regclass AND c.oid = m.objid),
tbls AS (
  SELECT 'relation ' || r.relname || ' kind=' || r.relkind::text || ' acl=' || COALESCE(r.relacl::text, '-')
         || ' comment=' || COALESCE(obj_description(r.oid, 'pg_class'), '-') AS line FROM rels r),
cols AS (
  SELECT 'column ' || r.relname || '.' || a.attnum || ' ' || a.attname || ' '
         || format_type(a.atttypid, a.atttypmod) || ' notnull=' || a.attnotnull
         || ' identity=' || a.attidentity::text || ' default='
         || COALESCE(pg_get_expr(d.adbin, d.adrelid), '-') AS line
    FROM rels r JOIN pg_attribute a ON a.attrelid = r.oid AND a.attnum > 0 AND NOT a.attisdropped
    LEFT JOIN pg_attrdef d ON d.adrelid = a.attrelid AND d.adnum = a.attnum
   WHERE r.relkind = 'r'),
cons AS (
  SELECT 'constraint ' || r.relname || ' ' || c.conname || ' ' || pg_get_constraintdef(c.oid) AS line
    FROM rels r JOIN pg_constraint c ON c.conrelid = r.oid),
idx AS (
  SELECT 'index ' || pg_get_indexdef(i.indexrelid) AS line
    FROM rels r JOIN pg_index i ON i.indrelid = r.oid),
trg AS (
  SELECT 'trigger ' || r.relname || ' ' || t.tgname || ' enabled=' || t.tgenabled::text || ' '
         || pg_get_triggerdef(t.oid) AS line
    FROM rels r JOIN pg_trigger t ON t.tgrelid = r.oid AND NOT t.tgisinternal),
evt AS (
  SELECT 'event trigger ' || e.evtname || ' ' || e.evtevent || ' enabled=' || e.evtenabled::text
         || ' func=' || e.evtfoid::regproc::text AS line
    FROM members m JOIN pg_event_trigger e ON m.classid = 'pg_event_trigger'::regclass AND e.oid = m.objid),
cnt AS (
  SELECT 'members ' || m.classid::regclass::text || ' ' || count(*) AS line
    FROM members m GROUP BY m.classid),
chain AS (
  SELECT 'schema acl ' || COALESCE(n.nspacl::text, '-') AS line
    FROM pg_namespace n WHERE n.nspname = 'kingres')
SELECT line FROM (
  SELECT line FROM funcs UNION ALL SELECT line FROM tbls UNION ALL SELECT line FROM cols
  UNION ALL SELECT line FROM cons UNION ALL SELECT line FROM idx UNION ALL SELECT line FROM trg
  UNION ALL SELECT line FROM evt UNION ALL SELECT line FROM cnt UNION ALL SELECT line FROM chain) u
ORDER BY line
};

sub fingerprint
{
	my ($db) = @_;
	return $node->safe_psql($db, $fingerprint);
}

#--------------------------------------------------------------------------
# A database created with 0.1, with data.
#--------------------------------------------------------------------------
$node->safe_psql('postgres', 'CREATE DATABASE old01');
$node->safe_psql('old01', q{
	SET kingres_ledger.allow_manual_write = on;
	CREATE EXTENSION kingres_ledger VERSION '0.1';
	CREATE SCHEMA app;
	CREATE TABLE app.accounts (id int PRIMARY KEY, owner_name text NOT NULL, balance numeric(12,2));
	INSERT INTO app.accounts VALUES (1, 'alice', 10), (2, 'bob', 20);
	SELECT kingres.ledger_enable('app.accounts');
	INSERT INTO app.accounts VALUES (3, 'carol', 30);
	SELECT kingres.ledger_build_block();
	INSERT INTO app.accounts VALUES (4, 'dave', 40);
});
is($node->safe_psql('old01',
		q{SELECT extversion FROM pg_extension WHERE extname = 'kingres_ledger'}),
	'0.1', 'the old database has version 0.1');
my $old_digest = $node->safe_psql('old01', 'SET kingres_ledger.allow_manual_write = on; SELECT kingres.ledger_generate_digest()');
$node->safe_psql('old01', q{INSERT INTO app.accounts VALUES (5, 'erin', 50)});
my $old_proof_row = $node->safe_psql('old01',
	q{SELECT encode(kingres.ledger_row_hash(a.*), 'hex') FROM app.accounts a WHERE id = 1});
my $old_proof = $node->safe_psql('old01',
	qq{SELECT kingres.ledger_inclusion_proof('app.accounts', decode('$old_proof_row', 'hex'))});
is($node->safe_psql('old01', 'SELECT count(*) FROM kingres.ledger_verify()'),
	'0', 'the 0.1 ledger verifies before the upgrade');
my $old_blocks = $node->safe_psql('old01',
	q{SELECT string_agg(height || ':' || encode(block_hash, 'hex'), ',' ORDER BY height)
	    FROM kingres.ledger_blocks});

#--------------------------------------------------------------------------
# Upgrade.
#--------------------------------------------------------------------------
$node->safe_psql('old01', q{ALTER EXTENSION kingres_ledger UPDATE TO '0.2'});
is($node->safe_psql('old01',
		q{SELECT extversion FROM pg_extension WHERE extname = 'kingres_ledger'}),
	'0.2', 'the extension is at 0.2 after ALTER EXTENSION UPDATE');
is($node->safe_psql('old01', 'SELECT count(*) FROM kingres.ledger_verify()'),
	'0', 'the ledger verifies after the upgrade');
is( $node->safe_psql('old01',
		q{SELECT string_agg(height || ':' || encode(block_hash, 'hex'), ',' ORDER BY height)
		    FROM kingres.ledger_blocks}),
	$old_blocks, 'the blocks are unchanged by the upgrade');
is($node->safe_psql('old01',
		qq{SELECT kingres.ledger_verify_digest('$old_digest'::jsonb)}),
	't', 'a digest generated before the upgrade verifies after it');
is($node->safe_psql('old01',
		qq{SELECT kingres.ledger_verify_proof('$old_proof'::jsonb, '$old_digest'::jsonb)}),
	't', 'a proof built before the upgrade verifies after it, against the digest taken before');
is($node->safe_psql('old01',
		qq{SELECT kingres.ledger_verify_proof('$old_proof'::jsonb, '$old_digest'::jsonb, }
		  . q{(SELECT table_id FROM kingres.ledger_table WHERE relid = 'app.accounts'::regclass), }
		  . qq{decode('$old_proof_row', 'hex'))}),
	't', 'and with the table and row it is for');
my $tip = $node->safe_psql('old01', 'SET kingres_ledger.allow_manual_write = on; SELECT kingres.ledger_generate_digest()');
is($node->safe_psql('old01', qq{SELECT kingres.ledger_verify_digest('$tip'::jsonb)}),
	't', 'a digest taken after the upgrade verifies');

# The existing table is still append-only and still captured.
my ($ret, $stdout, $stderr) = $node->psql('old01',
	q{UPDATE app.accounts SET balance = 1 WHERE id = 1},
	extra_params => [ '-v', 'VERBOSITY=sqlstate' ]);
like($stderr, qr/KL001/, 'the upgraded append-only table still rejects UPDATE');
$node->safe_psql('old01', q{INSERT INTO app.accounts VALUES (6, 'frank', 60)});
$node->safe_psql('old01', 'SET kingres_ledger.allow_manual_write = on; SELECT kingres.ledger_build_block()');
is($node->safe_psql('old01', 'SELECT count(*) FROM kingres.ledger_verify()'),
	'0', 'and verifies after more writes');

#--------------------------------------------------------------------------
# Same objects, privileges and protection as a direct installation.
#--------------------------------------------------------------------------
$node->safe_psql('postgres', 'CREATE DATABASE new02');
$node->safe_psql('new02', q{CREATE EXTENSION kingres_ledger VERSION '0.2'});
my $fp_new = fingerprint('new02');
my $fp_old = fingerprint('old01');
ok(length($fp_new) > 1000, 'the fingerprint covers the extension');
is($fp_old, $fp_new, 'upgrading 0.1 to 0.2 gives the objects of installing 0.2');
# What PUBLIC can read of the records is the same after an upgrade (the second
# review: ledger_verification is not readable, ledger_digest without generated_by),
# and the functions that must not be executable are not.
foreach my $db ('old01', 'new02')
{
	is($node->safe_psql($db, q{
		SELECT string_agg(c.relname || '.' || a.attname, ',' ORDER BY c.relname, a.attnum)
		  FROM pg_class c JOIN pg_attribute a ON a.attrelid = c.oid AND a.attnum > 0 AND NOT a.attisdropped
		 WHERE c.relnamespace = 'kingres'::regnamespace AND c.relkind = 'r'
		   AND c.relname IN ('ledger_digest', 'ledger_verification')
		   AND has_column_privilege('public', c.oid, a.attnum, 'select')}),
		'ledger_digest.digest_id,ledger_digest.generated_at,ledger_digest.height,ledger_digest.block_hash,ledger_digest.digest',
		"$db: PUBLIC reads the digests without generated_by and nothing of the verifications");
	is($node->safe_psql($db, q{
		SELECT count(*) FROM pg_proc p
		 WHERE p.pronamespace = 'kingres'::regnamespace
		   AND p.proname IN ('ledger_history_acl_sync', 'ledger_history_sync', 'ledger_worker_state')
		   AND has_function_privilege('public', p.oid, 'execute')}),
		'0', "$db: PUBLIC cannot execute the internal functions of the history and worker");
}
if ($fp_old ne $fp_new)
{
	my @a = split /\n/, $fp_old;
	my @b = split /\n/, $fp_new;
	my %in_b = map { $_ => 1 } @b;
	my %in_a = map { $_ => 1 } @a;
	diag("only in the upgraded database: $_") for grep { !$in_b{$_} } @a;
	diag("only in the direct installation: $_") for grep { !$in_a{$_} } @b;
}

# An empty ledger: no digest, nothing recorded, a status with no block.
is($node->safe_psql('new02', 'SELECT kingres.ledger_generate_digest() IS NULL'),
	't', 'the digest of an empty ledger is NULL');
is($node->safe_psql('new02', 'SELECT count(*) FROM kingres.ledger_digest'),
	'0', 'and nothing is recorded for it');
is($node->safe_psql('new02',
		q{SELECT append_only_tables || ' ' || updatable_tables || ' ' || coalesce(latest_block_height::text, 'none')
		       || ' ' || pending_transactions || ' ' || coalesce(oldest_pending_age::text, 'none')
		       || ' ' || coalesce(last_digest_id::text, 'none') || ' ' || coalesce(last_verification_id::text, 'none')
		    FROM kingres.ledger_status}),
	'0 0 none 0 none none none', 'the status of an empty ledger');
is($node->safe_psql('new02', 'SET kingres_ledger.allow_manual_write = on; SELECT kingres.ledger_verify_recorded()'),
	'1', 'the first recorded verification has id 1');

# Protection: the same statements are rejected in both.
foreach my $db ('old01', 'new02')
{
	foreach my $stmt (
		'DROP TABLE kingres.ledger_blocks',
		'ALTER TABLE kingres.ledger_table ADD COLUMN x int',
		'DROP TABLE kingres.ledger_digest',
		'ALTER TABLE kingres.ledger_verification DISABLE TRIGGER ALL')
	{
		($ret, $stdout, $stderr) = $node->psql($db, $stmt,
			extra_params => [ '-v', 'VERBOSITY=sqlstate' ]);
		like($stderr, qr/KL002/, "[$db] rejected: $stmt");
	}
}

# The extension cannot be dropped while a ledger table exists (old01 has one).
($ret, $stdout, $stderr) = $node->psql('old01', 'DROP EXTENSION kingres_ledger',
	extra_params => [ '-v', 'VERBOSITY=sqlstate' ]);
like($stderr, qr/KL002/, '[old01] rejected: DROP EXTENSION kingres_ledger');

done_testing();
