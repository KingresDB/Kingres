# contrib/kingres_ledger/t/011_upgrade_0_3.pl
#
# ALTER EXTENSION kingres_ledger UPDATE from 0.2 (and from 0.1) to 0.3, part A
# (kingres.ledger_block_preimage, ledger_head, ledger_blocks_entries):
#
#   * a database created with 0.2 that holds append-only and updatable
#     ledger tables, blocks, digests, proofs and recorded verifications still
#     verifies after the upgrade; every block hash, digest and proof made
#     before it is the same and still verifies, and the upgrade writes
#     nothing to the ledger;
#   * installing 0.3 directly, upgrading 0.2 and upgrading 0.1 give the same
#     objects, the same privileges and the same protection, compared through
#     a fingerprint of the catalogs that includes the full definition of
#     every function;
#   * the new functions work on the upgraded database and are protected like
#     the others (not droppable, not executable by PUBLIC where they must not
#     be);
#   * pg_dump and restore of a database at 0.3 work as documented.
#
# Portions Copyright (c) 2026, RooyeKhat Media Co
# SPDX-License-Identifier: AGPL-3.0-only

use strict;
use warnings FATAL => 'all';

use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;

my $node = PostgreSQL::Test::Cluster->new('upgrade03');
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
         || ' def=' || CASE WHEN p.prokind = 'a' THEN '-' ELSE md5(pg_get_functiondef(p.oid)) END
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

sub sqlstate_of
{
	my ($db, $sql) = @_;
	my ($ret, $out, $err) = $node->psql($db, $sql,
		extra_params => [ '-v', 'VERBOSITY=sqlstate' ]);
	return ($ret, $err);
}

sub blocks_line
{
	my ($db) = @_;
	return $node->safe_psql($db,
		q{SELECT string_agg(height || ':' || encode(block_hash, 'hex'), ',' ORDER BY height)
		    FROM kingres.ledger_blocks});
}

# Rows of every ledger table that the upgrade must not touch.
my $ledger_state = q{
SELECT (SELECT count(*) FROM kingres.ledger_entry) || ' '
    || (SELECT count(*) FROM kingres.ledger_pending) || ' '
    || (SELECT count(*) FROM kingres.ledger_blocks) || ' '
    || (SELECT count(*) FROM kingres.ledger_tx) || ' '
    || (SELECT count(*) FROM kingres.ledger_digest) || ' '
    || (SELECT count(*) FROM kingres.ledger_verification) || ' '
    || (SELECT count(*) FROM kingres.ledger_table)
};

#--------------------------------------------------------------------------
# A database created with 0.2, with data of both modes.
#--------------------------------------------------------------------------
$node->safe_psql('postgres', 'CREATE DATABASE old02');
$node->safe_psql('old02', q{
	SET kingres_ledger.allow_manual_write = on;
	CREATE EXTENSION kingres_ledger VERSION '0.2';
	CREATE SCHEMA app;
	CREATE TABLE app.accounts (id int PRIMARY KEY, owner_name text NOT NULL, balance numeric(12,2));
	CREATE TABLE app.notes (id int PRIMARY KEY, body text);
	INSERT INTO app.accounts VALUES (1, 'alice', 10), (2, 'bob', 20);
	SELECT kingres.ledger_enable('app.accounts');
	SELECT kingres.ledger_enable('app.notes', 'updatable');
	INSERT INTO app.accounts VALUES (3, 'carol', 30);
	INSERT INTO app.notes VALUES (1, 'first');
	SELECT kingres.ledger_build_block();
	UPDATE app.notes SET body = 'second' WHERE id = 1;
	INSERT INTO app.accounts VALUES (4, 'dave', 40);
	SELECT kingres.ledger_build_block();
	DELETE FROM app.notes WHERE id = 1;
	SELECT kingres.ledger_build_block();
	INSERT INTO app.accounts VALUES (5, 'erin', 50);
});
is($node->safe_psql('old02',
		q{SELECT extversion FROM pg_extension WHERE extname = 'kingres_ledger'}),
	'0.2', 'the old database has version 0.2');
my $old_digest = $node->safe_psql('old02', 'SET kingres_ledger.allow_manual_write = on; SELECT kingres.ledger_generate_digest()');
$node->safe_psql('old02', q{INSERT INTO app.accounts VALUES (6, 'frank', 60)});
$node->safe_psql('old02', 'SET kingres_ledger.allow_manual_write = on; SELECT kingres.ledger_verify_recorded()');
my $row_i = $node->safe_psql('old02',
	q{SELECT encode(kingres.ledger_row_hash(a.*), 'hex') FROM app.accounts a WHERE id = 1});
my $proof_i = $node->safe_psql('old02',
	qq{SELECT kingres.ledger_inclusion_proof('app.accounts', decode('$row_i', 'hex'))});
my $row_d = $node->safe_psql('old02',
	q{SELECT encode(e.row_hash, 'hex') FROM kingres.ledger_entry e WHERE e.op = 'D' ORDER BY e.ledger_txid, e.seq LIMIT 1});
my $proof_d = $node->safe_psql('old02',
	qq{SELECT kingres.ledger_inclusion_proof('app.notes', decode('$row_d', 'hex'), 'D')});
is($node->safe_psql('old02', 'SELECT count(*) FROM kingres.ledger_verify()'),
	'0', 'the 0.2 ledger verifies before the upgrade');
my $old_blocks = blocks_line('old02');
my $old_state = $node->safe_psql('old02', $ledger_state);
my $old_height = $node->safe_psql('old02', 'SELECT max(height) FROM kingres.ledger_blocks');

# The new functions do not exist yet.
my ($ret, $err) = sqlstate_of('old02', 'SELECT * FROM kingres.ledger_head()');
like($err, qr/42883/, 'ledger_head does not exist in 0.2');

# The 0.3 library does not write the ledger through the SQL of 0.2: the old
# block builder is refused, with the statement to run, and nothing is written
# (the capture trigger is part of the library and keeps working).
my ($r0, $o0, $e0) = $node->psql('old02', 'SELECT kingres.ledger_build_block()');
isnt($r0, 0, 'the block builder of the 0.2 SQL is refused by the 0.3 library');
like($e0, qr/version 0\.2.*ALTER EXTENSION kingres_ledger UPDATE/s,
	'and the error says which version the SQL is and what to run');
is($node->safe_psql('old02', $ledger_state), $old_state, 'and nothing was written');

#--------------------------------------------------------------------------
# Upgrade.
#--------------------------------------------------------------------------
$node->safe_psql('old02', 'ALTER EXTENSION kingres_ledger UPDATE');
is($node->safe_psql('old02',
		q{SELECT extversion FROM pg_extension WHERE extname = 'kingres_ledger'}),
	'0.3', 'the extension is at 0.3 after ALTER EXTENSION UPDATE');
is($node->safe_psql('old02', $ledger_state), $old_state,
	'the upgrade wrote nothing to the ledger tables');
is($node->safe_psql('old02', 'SELECT count(*) FROM kingres.ledger_verify()'),
	'0', 'the ledger verifies after the upgrade');
is(blocks_line('old02'), $old_blocks, 'the blocks are unchanged by the upgrade');
is($node->safe_psql('old02', qq{SELECT kingres.ledger_verify_digest('$old_digest'::jsonb)}),
	't', 'a digest generated before the upgrade verifies after it');
is($node->safe_psql('old02',
		qq{SELECT kingres.ledger_verify_proof('$proof_i'::jsonb, '$old_digest'::jsonb)}),
	't', 'an I proof built before the upgrade verifies against the digest taken before');
is($node->safe_psql('old02',
		qq{SELECT kingres.ledger_verify_proof('$proof_d'::jsonb, '$old_digest'::jsonb, }
		  . q{(SELECT table_id FROM kingres.ledger_table WHERE relid = 'app.notes'::regclass), }
		  . qq{decode('$row_d', 'hex'), 'D')}),
	't', 'and a D proof of the updatable table');
is($node->safe_psql('old02', q{SELECT count(*) FROM kingres.ledger_verification WHERE state = 'clean'}),
	'1', 'the verification recorded before the upgrade is still there');

# Every block of the old database: the preimage of the new function hashes to
# the stored hash, with SHA-256 from the server and not from the extension.
is($node->safe_psql('old02', q{
	SELECT count(*) FILTER (WHERE sha256(e.entry) = b.block_hash) || '/' || count(*)
	  FROM kingres.ledger_blocks_entries(0, 100) e JOIN kingres.ledger_blocks b USING (height)}),
	($old_height + 1) . '/' . ($old_height + 1),
	'the preimages of the blocks made by 0.2 hash to their stored hashes');
is($node->safe_psql('old02', q{SELECT height FROM kingres.ledger_head()}), $old_height,
	'ledger_head sees the head of the old ledger');
is($node->safe_psql('old02', $ledger_state), $old_state,
	'and the new functions wrote nothing');

# Work continues: new blocks, a digest after the upgrade, a proof against it.
$node->safe_psql('old02', q{INSERT INTO app.accounts VALUES (7, 'grace', 70)});
my $tip = $node->safe_psql('old02', 'SELECT kingres.ledger_generate_digest()');
is($node->safe_psql('old02', qq{SELECT kingres.ledger_verify_digest('$tip'::jsonb)}),
	't', 'a digest taken after the upgrade verifies');
is($node->safe_psql('old02', q{SELECT height FROM kingres.ledger_head()}), $old_height + 1,
	'ledger_head follows the new block');
is($node->safe_psql('old02', 'SELECT count(*) FROM kingres.ledger_verify()'),
	'0', 'and the ledger verifies after more writes');

# The new functions are protected like the others.
foreach my $f ('ledger_head()', 'ledger_blocks_entries(bigint, int)', 'ledger_block_preimage(smallint, bigint, bytea, bytea, int, timestamptz)')
{
	($ret, $err) = sqlstate_of('old02', "DROP FUNCTION kingres.$f");
	isnt($ret, 0, "DROP FUNCTION $f is refused");
}
is($node->safe_psql('old02', q{
	SELECT string_agg(p.proname, ',' ORDER BY p.proname) FROM pg_proc p
	 WHERE p.pronamespace = 'kingres'::regnamespace
	   AND p.proname IN ('ledger_head', 'ledger_blocks_entries', 'ledger_block_preimage')
	   AND has_function_privilege('public', p.oid, 'execute')}),
	'ledger_block_preimage', 'only the preimage function (a pure computation) is executable by PUBLIC');

#--------------------------------------------------------------------------
# Same objects, privileges and protection as a direct installation, from 0.2
# and from 0.1.
#--------------------------------------------------------------------------
$node->safe_psql('postgres', 'CREATE DATABASE new03');
$node->safe_psql('new03', q{CREATE EXTENSION kingres_ledger});
is($node->safe_psql('new03', q{SELECT extversion FROM pg_extension WHERE extname = 'kingres_ledger'}),
	'0.3', 'a plain CREATE EXTENSION installs 0.3');
$node->safe_psql('postgres', 'CREATE DATABASE old01');
$node->safe_psql('old01', q{
	SET kingres_ledger.allow_manual_write = on;
	CREATE EXTENSION kingres_ledger VERSION '0.1';
	CREATE TABLE public.t (id int PRIMARY KEY);
	SELECT kingres.ledger_enable('public.t');
	INSERT INTO public.t VALUES (1);
	SELECT kingres.ledger_build_block();
});
my $d01 = $node->safe_psql('old01', 'SET kingres_ledger.allow_manual_write = on; SELECT kingres.ledger_generate_digest()');
$node->safe_psql('old01', 'ALTER EXTENSION kingres_ledger UPDATE');
is($node->safe_psql('old01', q{SELECT extversion FROM pg_extension WHERE extname = 'kingres_ledger'}),
	'0.3', 'a 0.1 database reaches 0.3 in one command (through 0.2)');
is($node->safe_psql('old01', qq{SELECT kingres.ledger_verify_digest('$d01'::jsonb)}),
	't', 'a digest of 0.1 verifies at 0.3');

my $fp_new = fingerprint('new03');
my $fp_old = fingerprint('old02');
my $fp_old1 = fingerprint('old01');
ok(length($fp_new) > 1000, 'the fingerprint covers the extension');
cmp_ok(scalar(my @fp_lines = split(/\n/, $fp_new)), '>=', 200, 'and has the lines of a full catalog dump');
like($fp_new, qr/function (kingres\.)?ledger_head\(\)/, 'and includes the new functions');
like($fp_new, qr/def=[0-9a-f]{32}/, 'and the definition of each function');
is($fp_old, $fp_new, 'upgrading 0.2 to 0.3 gives the objects of installing 0.3');
is($fp_old1, $fp_new, 'upgrading 0.1 to 0.3 gives the objects of installing 0.3');
foreach my $pair ([ $fp_old, 'old02' ], [ $fp_old1, 'old01' ])
{
	if ($pair->[0] ne $fp_new)
	{
		my %in_b = map { $_ => 1 } split /\n/, $fp_new;
		my %in_a = map { $_ => 1 } split /\n/, $pair->[0];
		diag("only in $pair->[1]: $_") for grep { !$in_b{$_} } split /\n/, $pair->[0];
		diag("only in the direct installation: $_") for grep { !$in_a{$_} } split /\n/, $fp_new;
	}
}

# An empty ledger at 0.3: the new functions give nothing, and nothing is recorded.
is($node->safe_psql('new03', 'SELECT count(*) FROM kingres.ledger_head()'), '0',
	'ledger_head of an empty ledger has no row');
is($node->safe_psql('new03', 'SELECT count(*) FROM kingres.ledger_blocks_entries(0, 10)'), '0',
	'ledger_blocks_entries of an empty ledger has no row');

# Protection: the same statements are rejected in all three.
foreach my $db ('old02', 'old01', 'new03')
{
	foreach my $stmt (
		'DROP TABLE kingres.ledger_blocks',
		'ALTER TABLE kingres.ledger_table ADD COLUMN x int',
		'TRUNCATE kingres.ledger_blocks')
	{
		my ($r, $e) = sqlstate_of($db, $stmt);
		like($e, qr/KL00[12]/, "[$db] rejected: $stmt");
	}
}

#--------------------------------------------------------------------------
# pg_dump and restore at 0.3 (the ledger tables are not dumped: README).
#--------------------------------------------------------------------------
my $dump = $node->basedir . '/old02.sql';
command_ok([ 'pg_dump', '--no-sync', '-f', $dump, '-d', $node->connstr('old02') ],
	'pg_dump of an upgraded database works');
$node->safe_psql('postgres', 'CREATE DATABASE restored03');
my ($rout, $rerr) = run_command([ 'psql', '-X', '-q', '-v', 'ON_ERROR_STOP=1',
		'-d', $node->connstr('restored03'), '-f', $dump ]);
is($rerr, '', 'the dump restores without an error');
is($node->safe_psql('restored03', q{SELECT extversion FROM pg_extension WHERE extname = 'kingres_ledger'}),
	'0.3', 'the restored database has the extension at 0.3');
is($node->safe_psql('restored03', 'SELECT count(*) FROM app.accounts'), '7',
	'the rows of the ledger table are restored');
is($node->safe_psql('restored03', 'SELECT count(*) FROM kingres.ledger_head()'), '0',
	'but the restored ledger is empty: ledger_head has no row');
is($node->safe_psql('restored03', 'SELECT count(*) FROM kingres.ledger_blocks_entries(0, 10)'), '0',
	'and ledger_blocks_entries has none');
my $fp_rest = fingerprint('restored03');
is($fp_rest, $fp_new, 'the restored database has the objects of a direct installation of 0.3');

$node->stop;
done_testing();
