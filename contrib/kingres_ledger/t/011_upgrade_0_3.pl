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

use FindBin;
use lib $FindBin::RealBin;

use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KlSign qw(have_openssl new_key sign_message message_bytes hex_of unhex_of random_nonce);

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
         || ' owner=' || pg_get_userbyid(p.proowner)::text || ' leak=' || p.proleakproof
         || ' cost=' || p.procost || ' rows=' || p.prorows
         || ' acl=' || COALESCE(p.proacl::text, '-')
         || ' src=' || md5(p.prosrc || '|' || COALESCE(p.probin, ''))
         || ' ret=' || pg_get_function_result(p.oid)
         || ' def=' || CASE WHEN p.prokind = 'a' THEN '-' ELSE md5(pg_get_functiondef(p.oid)) END
         || ' comment=' || COALESCE(obj_description(p.oid, 'pg_proc'), '-') AS line
    FROM members m JOIN pg_proc p ON m.classid = 'pg_proc'::regclass AND p.oid = m.objid
    JOIN pg_language l ON l.oid = p.prolang),
rels AS (
  SELECT c.oid, c.relname, c.relkind, c.relacl, c.relpersistence, c.relowner, c.reloptions,
         c.relrowsecurity, c.relforcerowsecurity, c.relreplident
    FROM members m JOIN pg_class c ON m.classid = 'pg_class'::regclass AND c.oid = m.objid),
tbls AS (
  SELECT 'relation ' || r.relname || ' kind=' || r.relkind::text || ' acl=' || COALESCE(r.relacl::text, '-')
         || ' owner=' || pg_get_userbyid(r.relowner)::text || ' opts=' || COALESCE(r.reloptions::text, '-')
         || ' rls=' || r.relrowsecurity || '/' || r.relforcerowsecurity || ' replident=' || r.relreplident::text
         || ' comment=' || COALESCE(obj_description(r.oid, 'pg_class'), '-') AS line FROM rels r),
cols AS (
  SELECT 'column ' || r.relname || '.' || a.attnum || ' ' || a.attname || ' '
         || format_type(a.atttypid, a.atttypmod) || ' notnull=' || a.attnotnull
         || ' identity=' || a.attidentity::text
         || ' colacl=' || COALESCE(a.attacl::text, '-')
         || ' opts=' || COALESCE((SELECT string_agg(o, ',' ORDER BY o) FROM unnest(a.attoptions) o
                                   WHERE o NOT LIKE 'pg\_attver=%'), '-') || ' storage=' || a.attstorage::text
         || ' coll=' || a.attcollation::regcollation::text || ' default='
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
         || ' func=' || e.evtfoid::regproc::text || ' owner=' || pg_get_userbyid(e.evtowner)::text
         || ' tags=' || COALESCE(e.evttags::text, '-') AS line
    FROM members m JOIN pg_event_trigger e ON m.classid = 'pg_event_trigger'::regclass AND e.oid = m.objid),
cnt AS (
  SELECT 'members ' || m.classid::regclass::text || ' ' || count(*) AS line
    FROM members m GROUP BY m.classid),
chain AS (
  SELECT 'schema acl ' || COALESCE(n.nspacl::text, '-') || ' owner=' || pg_get_userbyid(n.nspowner)::text AS line
    FROM pg_namespace n WHERE n.nspname = 'kingres'),
-- the privileges that the extension script recorded at installation, and the
-- default privileges of the database (none of the three databases has any)
initp AS (
  SELECT 'initprivs ' || CASE p.classoid
                           WHEN 'pg_class'::regclass THEN p.objoid::regclass::text
                           WHEN 'pg_proc'::regclass THEN p.objoid::regprocedure::text
                           ELSE p.classoid::regclass::text || ':' || p.objoid END
         || '.' || p.objsubid || ' ' || p.privtype::text || ' ' || p.initprivs::text AS line
    FROM pg_init_privs p
   WHERE p.objoid IN (SELECT objid FROM members)),
defacl AS (
  SELECT 'default acl ' || pg_get_userbyid(d.defaclrole)::text || ' ' || d.defaclobjtype::text || ' '
         || COALESCE(d.defaclnamespace::regnamespace::text, '-') || ' ' || d.defaclacl::text AS line
    FROM pg_default_acl d),
ext2 AS (
  SELECT 'extension ' || e.extname || ' owner=' || pg_get_userbyid(e.extowner)::text
         || ' relocatable=' || e.extrelocatable AS line FROM pg_extension e WHERE e.extname = 'kingres_ledger')
SELECT line FROM (
  SELECT line FROM funcs UNION ALL SELECT line FROM tbls UNION ALL SELECT line FROM cols
  UNION ALL SELECT line FROM cons UNION ALL SELECT line FROM idx UNION ALL SELECT line FROM trg
  UNION ALL SELECT line FROM evt UNION ALL SELECT line FROM cnt UNION ALL SELECT line FROM chain
  UNION ALL SELECT line FROM initp UNION ALL SELECT line FROM defacl UNION ALL SELECT line FROM ext2) u
ORDER BY line
};

# The capture trigger of each of the three tables of client signatures has, as its argument,
# the next free identifier of the registry when the table was registered: 1 to 3 in a new
# database, after the tables of the user in an upgraded one.  The fingerprint hides that number
# (the test below checks it) and compares everything else.
sub fingerprint
{
	my ($db) = @_;
	my $fp = $node->safe_psql($db, $fingerprint);
	$fp =~ s/(trigger ledger_(?:client_key|client_key_revocation|attestation) kingres_ledger_capture .*ledger_capture\(')\d+/${1}N/g;
	return $fp;
}

sub internal_ids
{
	my ($db) = @_;
	return $node->safe_psql($db, q{SELECT string_agg(c.relname || '=' || lt.table_id || ':' ||
		(SELECT substring(pg_get_triggerdef(g.oid) from 'ledger_capture\(''(\d+)''\)')
		   FROM pg_trigger g WHERE g.tgrelid = lt.relid AND g.tgname = 'kingres_ledger_capture'),
		',' ORDER BY c.relname)
		FROM kingres.ledger_table lt JOIN pg_class c ON c.oid = lt.relid
		WHERE c.relnamespace = 'kingres'::regnamespace});
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
# Every function of the old SQL that writes is refused, and so is a statement
# of the caller's own, each with the code KL005 of this refusal (and not the
# 42501 of the others); the setting that the database was built with is not set
# in these sessions.
$node->safe_psql('old02', 'CREATE TABLE app.late (id int)');
foreach my $stmt (
	'SELECT kingres.ledger_generate_digest()',
	'SELECT kingres.ledger_verify_recorded()',
	q{SELECT kingres.ledger_enable('app.late')},
	q{INSERT INTO kingres.ledger_digest (generated_at, generated_by, height, block_hash, digest) SELECT now(), 'x', 0, block_hash, '{}' FROM kingres.ledger_blocks LIMIT 1})
{
	my ($rs, $es) = sqlstate_of('old02', $stmt);
	like($es, qr/KL005/, "[old02] refused with its own SQLSTATE KL005: $stmt");
	my ($rm, $om, $em) = $node->psql('old02', $stmt);
	like($em, qr/version 0\.2, and this library writes the ledger only through the functions of version 0\.3/,
		"[old02] and the message names both versions: $stmt");
	like($em, qr/Run ALTER EXTENSION kingres_ledger UPDATE/, "[old02] and the hint says what to run: $stmt");
}
is($node->safe_psql('old02', $ledger_state), $old_state, 'and none of them wrote anything');

#--------------------------------------------------------------------------
# Upgrade.
#--------------------------------------------------------------------------
$node->safe_psql('old02', 'ALTER EXTENSION kingres_ledger UPDATE');
is($node->safe_psql('old02',
		q{SELECT extversion FROM pg_extension WHERE extname = 'kingres_ledger'}),
	'0.3', 'the extension is at 0.3 after ALTER EXTENSION UPDATE');
# The upgrade registers the three tables of client signatures (three rows of the registry,
# the last number of the state) and writes nothing else: no entry, no block, no digest.
my $after_state = $old_state;
$after_state =~ s/(\d+)$/$1 + 3/e;
is($node->safe_psql('old02', $ledger_state), $after_state,
	'the upgrade wrote nothing to the ledger tables but the registration of the three tables of client signatures');
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
is($node->safe_psql('old02', $ledger_state), $after_state,
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
# something is pending, so that the old builder has a write to refuse
$node->safe_psql('old01', q{INSERT INTO public.t VALUES (2)});
my $state01_sql = q{SELECT (SELECT count(*) FROM kingres.ledger_entry) || ' '
    || (SELECT count(*) FROM kingres.ledger_pending) || ' '
    || (SELECT count(*) FROM kingres.ledger_blocks) || ' '
    || (SELECT count(*) FROM kingres.ledger_tx) || ' '
    || (SELECT count(*) FROM kingres.ledger_table)};
my $state01 = $node->safe_psql('old01', $state01_sql);
$node->safe_psql('old01', 'CREATE TABLE public.late (id int)');
foreach my $stmt (
	'SELECT kingres.ledger_build_block()',
	'SELECT kingres.ledger_generate_digest()',
	q{SELECT kingres.ledger_enable('public.late')})
{
	my ($rs, $es) = sqlstate_of('old01', $stmt);
	like($es, qr/KL005/, "[old01] refused with its own SQLSTATE KL005: $stmt");
	my ($rm, $om, $em) = $node->psql('old01', $stmt);
	like($em, qr/version 0\.1, and this library writes the ledger only through the functions of version 0\.3/,
		"[old01] and the message names both versions: $stmt");
}
is($node->safe_psql('old01', $state01_sql), $state01, 'and none of them wrote anything at 0.1');
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
like($fp_new, qr/column ledger_digest\.[0-9]+ generated_at .* colacl=\{=r\//, 'and the privileges on columns (the grant on ledger_digest)');
like($fp_new, qr/ owner=\S+ /, 'and the owners');
is($fp_old, $fp_new, 'upgrading 0.2 to 0.3 gives the objects of installing 0.3');
is($fp_old1, $fp_new, 'upgrading 0.1 to 0.3 gives the objects of installing 0.3');
# The identifiers of the three tables: 1 to 3 in the new database, and the next free ones after
# the tables of the user in the upgraded ones (two tables in old02, one in old01).
is(internal_ids('new03'), 'ledger_attestation=3:3,ledger_client_key=1:1,ledger_client_key_revocation=2:2',
	'a new database registers the three tables of client signatures as 1 to 3, and the trigger agrees');
is(internal_ids('old02'), 'ledger_attestation=5:5,ledger_client_key=3:3,ledger_client_key_revocation=4:4',
	'the upgrade of a database with two ledger tables gives them 3 to 5, and the trigger agrees');
is(internal_ids('old01'), 'ledger_attestation=4:4,ledger_client_key=2:2,ledger_client_key_revocation=3:3',
	'and one with one ledger table 2 to 4');
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


#--------------------------------------------------------------------------
# Client signatures (part B) work in a database that was upgraded, from 0.2 and from 0.1,
# as in one that was installed: register a key, attest a row, the ledger verifies, and
# the three tables are registered, protected and not readable by PUBLIC.
#--------------------------------------------------------------------------
$node->safe_psql('new03', q{CREATE TABLE public.t3 (id int PRIMARY KEY); SELECT kingres.ledger_enable('public.t3');});
foreach my $case ([ 'old02', 'app.accounts', q{ROW(100, 'zed', 1.00)}, q{(100, 'zed', 1.00)} ],
	[ 'old01', 'public.t', q{ROW(100)}, q{(100)} ],
	[ 'new03', 'public.t3', q{ROW(100)}, q{(100)} ])
{
	my ($db, $table, $row, $values) = @$case;
	is($node->safe_psql($db, q{SELECT string_agg(c.relname || ':' || lt.mode, ',' ORDER BY c.relname)
		FROM kingres.ledger_table lt JOIN pg_class c ON c.oid = lt.relid
		WHERE c.relnamespace = 'kingres'::regnamespace}),
		'ledger_attestation:append_only,ledger_client_key:append_only,ledger_client_key_revocation:append_only',
		"[$db] the three tables of client signatures are registered as append-only ledger tables");
	is($node->safe_psql($db, q{SELECT count(*) FROM pg_class c WHERE c.relnamespace = 'kingres'::regnamespace
		AND c.relname IN ('ledger_attestation', 'ledger_client_key', 'ledger_client_key_revocation')
		AND has_table_privilege('public', c.oid, 'select')}), '0',
		"[$db] and PUBLIC cannot read them");
	is($node->safe_psql($db, q{SELECT string_agg(p.proname, ',' ORDER BY p.proname) FROM pg_proc p
		WHERE p.pronamespace = 'kingres'::regnamespace AND p.proname IN ('ledger_attest', 'ledger_client_key_register',
		'ledger_client_key_revoke', 'ledger_set_signature_policy', 'ledger_register_internal', 'ledger_attestation_message')
		AND has_function_privilege('public', p.oid, 'execute')}),
		'ledger_attestation_message',
		"[$db] only the message function is executable by PUBLIC");
	is($node->safe_psql($db, q{SELECT a.attname FROM pg_attribute a WHERE a.attrelid = 'kingres.ledger_table'::regclass
		AND a.attnum IN (6, 7) ORDER BY a.attnum}) =~ s/\n/,/gr,
		'signature_policy,signature_required_from', "[$db] ledger_table has the two columns after history_relid");
	is($node->safe_psql($db, q{SELECT client_keys_active || ' ' || attestations || ' ' || required_tables FROM kingres.ledger_status}),
		'0 0 0', "[$db] ledger_status has the three columns at the end");
}
SKIP:
{
	skip 'the openssl command (3.0 or later) is needed to make Ed25519 signatures', 12 unless have_openssl();
	my $dir = PostgreSQL::Test::Utils::tempdir;
	foreach my $case ([ 'old02', 'app.accounts', q{ROW(100, 'zed', 1.00)}, q{(100, 'zed', 1.00)} ],
		[ 'old01', 'public.t', q{ROW(100)}, q{(100)} ],
		[ 'new03', 'public.t3', q{ROW(100)}, q{(100)} ])
	{
		my ($db, $table, $row, $values) = @$case;
		my $key = new_key($dir, "k_$db");
		my $keyid = $node->safe_psql($db, sprintf(q{SELECT encode(kingres.ledger_client_key_register(decode('%s', 'hex'), NULL, 'upgrade test'), 'hex')}, hex_of($key->{pub})));
		is($keyid, hex_of($key->{key_id}), "[$db] a key is registered, with the identifier SHA-256(public key)[:16]");
		my $chain = unhex_of($node->safe_psql($db, q{SELECT encode(chain_id, 'hex') FROM kingres.ledger_chain}));
		my $tid = $node->safe_psql($db, "SELECT table_id FROM kingres.ledger_table WHERE relid = '$table'::regclass");
		my $hash = $node->safe_psql($db, "SELECT encode(kingres.ledger_row_hash($row\::$table), 'hex')");
		my $nonce = random_nonce();
		my $sig = sign_message($key, message_bytes(chain_id => $chain, table_id => $tid, op => 'I',
				row_hash => unhex_of($hash), key_id => $key->{key_id}, nonce => $nonce));
		$node->safe_psql($db, sprintf(q{BEGIN; INSERT INTO %s VALUES %s; SELECT kingres.ledger_attest('%s', decode('%s', 'hex'), 'I', decode('%s', 'hex'), decode('%s', 'hex'), decode('%s', 'hex')); COMMIT;},
				$table, $values, $table, $hash, $keyid, hex_of($nonce), hex_of($sig)));
		is($node->safe_psql($db, 'SELECT count(*) FROM kingres.ledger_attestation'), '1', "[$db] an attested write");
		$node->safe_psql($db, 'SELECT kingres.ledger_build_block()');
		is($node->safe_psql($db, 'SELECT count(*) FROM kingres.ledger_verify()'), '0', "[$db] and the ledger verifies");
		$node->safe_psql($db, sprintf(q{SELECT kingres.ledger_set_signature_policy('%s', 'required')}, $table));
		my ($r, $o, $e) = $node->psql($db, "INSERT INTO $table VALUES $values" =~ s/100/101/r, extra_params => [ '-v', 'VERBOSITY=sqlstate' ]);
		like($e, qr/KL010/, "[$db] the policy required is enforced");
	}
}

# Protection: the same statements are rejected in all three.
foreach my $db ('old02', 'old01', 'new03')
{
	foreach my $stmt (
		'DROP TABLE kingres.ledger_blocks',
		'ALTER TABLE kingres.ledger_table ADD COLUMN x int',
		'TRUNCATE kingres.ledger_blocks',
		'DROP TABLE kingres.ledger_attestation',
		'TRUNCATE kingres.ledger_client_key',
		'ALTER TABLE kingres.ledger_client_key_revocation ADD COLUMN x int')
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
is($node->safe_psql('restored03', 'SELECT count(*) FROM app.accounts'), have_openssl() ? '8' : '7',
	'the rows of the ledger table are restored (the seven of the test and the one that the attestation test above wrote)');
is($node->safe_psql('restored03', 'SELECT count(*) FROM kingres.ledger_head()'), '0',
	'but the restored ledger is empty: ledger_head has no row');
is($node->safe_psql('restored03', 'SELECT count(*) FROM kingres.ledger_blocks_entries(0, 10)'), '0',
	'and ledger_blocks_entries has none');
my $fp_rest = fingerprint('restored03');
is($fp_rest, $fp_new, 'the restored database has the objects of a direct installation of 0.3');

$node->stop;
done_testing();
