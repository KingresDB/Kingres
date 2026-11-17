# contrib/kingres_ledger/t/015_recipe.pl
#
# The way out of a row that stops ledger_head (review findings L8-08, L9-02,
# L9-03): the statements of the README, section "A row above the head", run
# literally.  The test reads the statements out of the README (the code block
# that follows the marker "recipe: remove-stray"); it does not retype them.
#
# Four kinds of damage, each in a database of its own:
#   1. a transaction row above the head (a row written by hand);
#   2. a digest row above the head;
#   3. a digest row that names a block that is there, with another hash (L9-02);
#   4. the head block really removed (the block row alone), which leaves its
#      transaction rows and the digest of it behind (L9-03).
# After the recipe, for each: ledger_head answers; ledger_verify reports exactly
# the findings the README lists (operator_removal, one per removed row, and
# nothing else: no orphan entry); the rows are in kingres.ledger_removed and in
# the file that the recipe wrote; no trigger was switched off at any time (the
# recipe does not mention one, and every trigger is ENABLE ALWAYS afterwards); a
# member of pg_write_all_data still cannot write; and the chain goes on.
#
# Portions Copyright (c) 2026, RooyeKhat Media Co
# SPDX-License-Identifier: AGPL-3.0-only

use strict;
use warnings FATAL => 'all';

use Cwd qw(abs_path);
use IPC::Run;
use File::Basename qw(dirname);
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;

# The recipe, out of the README.
my $readme = dirname(abs_path($0)) . '/../README.md';
open(my $fh, '<', $readme) or die "cannot read $readme: $!";
my $text = do { local $/; <$fh> };
close $fh;
$text =~ /<!-- recipe: remove-stray -->\s*```sql\n(.*?)```/s
  or BAIL_OUT('the recipe is not in the README');
my $recipe = $1;
ok(length($recipe) > 200, 'the recipe was found in the README');
unlike($recipe, qr/pg_trigger|DISABLE\s+TRIGGER|tgenabled/i,
	'the recipe switches no trigger off');
like($recipe, qr/allow_manual_write/, 'it turns the setting on for its own session');

my $node = PostgreSQL::Test::Cluster->new('recipe');
$node->init;
$node->append_conf('postgresql.conf', qq(
shared_preload_libraries = 'kingres_ledger'
kingres_ledger.block_interval = 0
));
$node->start;
$node->safe_psql('postgres', q(CREATE ROLE rc_member IN ROLE pg_write_all_data LOGIN));

my $workdir = $PostgreSQL::Test::Utils::tmp_check . '/recipe';
mkdir $workdir;
my $counter = 0;

# A database with a ledger of three blocks and a digest of the head.
sub make_db
{
	my ($db) = @_;
	$node->safe_psql('postgres', "CREATE DATABASE $db");
	$node->safe_psql($db, q(
		CREATE EXTENSION kingres_ledger;
		GRANT USAGE ON SCHEMA kingres TO rc_member;
		CREATE TABLE public.docs (id int PRIMARY KEY, body text);
		SELECT kingres.ledger_enable('public.docs');
		INSERT INTO public.docs VALUES (1, 'a'), (2, 'b');
		SELECT kingres.ledger_build_block();
		INSERT INTO public.docs VALUES (3, 'c');
		SELECT kingres.ledger_build_block();
		INSERT INTO public.docs VALUES (4, 'd');
		SELECT kingres.ledger_generate_digest();
	));
	is($node->safe_psql($db, 'SELECT height FROM kingres.ledger_head()'), '2', "[$db] three blocks");
	is($node->safe_psql($db, 'SELECT count(*) FROM kingres.ledger_verify()'), '0', "[$db] clean");
}

# Runs the README statements in one psql session, in a directory of its own so
# that the file the recipe writes is found there.
sub run_recipe
{
	my ($db) = @_;
	my $dir = "$workdir/$db";
	mkdir $dir;
	append_to_file("$dir/recipe.sql", $recipe);
	my ($stdout, $stderr);
	my $ok = IPC::Run::run(
		[ 'psql', '-X', '-q', '-v', 'ON_ERROR_STOP=1', '-d', $node->connstr($db), '-f', "$dir/recipe.sql" ],
		'>', \$stdout, '2>', \$stderr, init => sub { chdir $dir or die });
	ok($ok, "[$db] the recipe runs without an error") or diag($stderr);
	return ($stdout, $dir);
}

sub findings
{
	my ($db) = @_;
	return $node->safe_psql($db,
		q(SELECT coalesce(string_agg(check_name || ' x' || n, ',' ORDER BY check_name), '-')
		    FROM (SELECT check_name, count(*) AS n FROM kingres.ledger_verify() GROUP BY 1) f));
}

sub plant_manual
{
	my ($db, $sql) = @_;
	$node->safe_psql($db, "SET kingres_ledger.allow_manual_write = on;\n$sql");
}

sub after_checks
{
	my ($db, $dir, $removed, $planted) = @_;
	# The row that the test planted by hand is itself recorded (manual_write);
	# the recipe removes the row and keeps that record.
	my $expect = ($planted ? "manual_write x$planted," : '') . "operator_removal x$removed";
	is($node->safe_psql($db, 'SELECT height FROM kingres.ledger_head()'), '2', "[$db] ledger_head answers");
	is(findings($db), $expect,
		"[$db] ledger_verify reports exactly the $removed removals the README lists");
	is($node->safe_psql($db, q(SELECT count(*) FROM kingres.ledger_removed WHERE action = 'removed')), $removed,
		"[$db] every removed row is in kingres.ledger_removed");
	ok(-s "$dir/stray-rows.json", "[$db] the recipe wrote the rows to a file");
	is($node->safe_psql($db, q(SELECT count(*) FROM pg_trigger t JOIN pg_class c ON c.oid = t.tgrelid
		 WHERE c.relnamespace = 'kingres'::regnamespace AND NOT t.tgisinternal AND t.tgenabled <> 'A')),
		'0', "[$db] every trigger of the ledger is still enabled (always)");
	my ($ret, $out, $err) = $node->psql($db, q(UPDATE kingres.ledger_tx SET user_name = 'm'),
		extra_params => [ '-U', 'rc_member' ]);
	isnt($ret, 0, "[$db] a member of pg_write_all_data still cannot change the ledger");
	($ret, $out, $err) = $node->psql($db, q(INSERT INTO kingres.ledger_pending VALUES (1, 'm')),
		extra_params => [ '-U', 'rc_member' ]);
	like($err, qr/cannot write to table/, "[$db] nor write it");
	$node->safe_psql($db, q(INSERT INTO public.docs VALUES (100, 'after the recipe')));
	is($node->safe_psql($db, q(SELECT kingres.ledger_build_block() IS NOT NULL)), 't',
		"[$db] the chain goes on");
	is($node->safe_psql($db, q(SELECT count(*) FROM kingres.ledger_pending)), '0',
		"[$db] and the queue is empty");
	is(findings($db), $expect, "[$db] and nothing new is reported");
}

#--------------------------------------------------------------------------
# 1. A transaction row above the head.
#--------------------------------------------------------------------------
make_db('rc1');
plant_manual('rc1', q(INSERT INTO kingres.ledger_tx VALUES (900000001, 77, 0, 1, sha256('x'), 'x')));
my ($ret, $out, $err) = $node->psql('rc1', 'SELECT * FROM kingres.ledger_head()');
like($err, qr/the row of transaction 900000001 in kingres\.ledger_tx names block 77/, '[rc1] ledger_head stops');
is(findings('rc1'), 'manual_write x1,tx_block x1', '[rc1] and ledger_verify names the row (and the write by hand)');
my ($stdout, $dir) = run_recipe('rc1');
after_checks('rc1', $dir, 1, 1);

#--------------------------------------------------------------------------
# 2. A digest row above the head.
#--------------------------------------------------------------------------
make_db('rc2');
plant_manual('rc2', q(INSERT INTO kingres.ledger_digest (generated_at, generated_by, height, block_hash, digest)
	VALUES (now(), 'x', 88, sha256('y'), '{}')));
($ret, $out, $err) = $node->psql('rc2', 'SELECT * FROM kingres.ledger_head()');
like($err, qr/digest \d+ in kingres\.ledger_digest names block 88/, '[rc2] ledger_head stops');
($stdout, $dir) = run_recipe('rc2');
after_checks('rc2', $dir, 1, 1);

#--------------------------------------------------------------------------
# 3. A digest row with another hash than its block (L9-02).
#--------------------------------------------------------------------------
make_db('rc3');
plant_manual('rc3', q(INSERT INTO kingres.ledger_digest (generated_at, generated_by, height, block_hash, digest)
	VALUES (now(), 'x', 0, sha256('wrong'), '{}')));
($ret, $out, $err) = $node->psql('rc3', 'SELECT * FROM kingres.ledger_head()');
like($err, qr/block 0 is not the block that digest \d+ in kingres\.ledger_digest names/, '[rc3] ledger_head stops');
like($node->safe_psql('rc3', q(SELECT string_agg(check_name || ' ' || object, ',' ORDER BY check_name) FROM kingres.ledger_verify())),
	qr/^digest_hash digest \d+,digest_json digest \d+,manual_write kingres\.ledger_digest$/, '[rc3] and ledger_verify names the digest row (L9-02)');
($stdout, $dir) = run_recipe('rc3');
after_checks('rc3', $dir, 1, 1);

#--------------------------------------------------------------------------
# 4. The head block removed: its transaction rows and its digest stay.
#--------------------------------------------------------------------------
make_db('rc4');
my $txrows = $node->safe_psql('rc4', 'SELECT count(*) FROM kingres.ledger_tx WHERE block_height = 2');
my $digests = $node->safe_psql('rc4', 'SELECT count(*) FROM kingres.ledger_digest WHERE height = 2');
cmp_ok($txrows, '>=', 1, '[rc4] the head block has a transaction');
cmp_ok($digests, '>=', 1, '[rc4] and a digest');
# The damage, as somebody with catalog access would do it: the trigger of one
# table off, the block row removed, the trigger back.
$node->safe_psql('rc4', q(UPDATE pg_trigger SET tgenabled = 'D'
	WHERE tgrelid = 'kingres.ledger_blocks'::regclass AND tgname = 'kingres_ledger_block_modify'));
$node->safe_psql('rc4', q(SET kingres_ledger.allow_manual_write = on;
	DELETE FROM kingres.ledger_blocks WHERE height = 2;));
$node->safe_psql('rc4', q(UPDATE pg_trigger SET tgenabled = 'A'
	WHERE tgrelid = 'kingres.ledger_blocks'::regclass AND tgname = 'kingres_ledger_block_modify'));
is($node->safe_psql('rc4', 'SELECT max(height) FROM kingres.ledger_blocks'), '1', '[rc4] the head block is gone');
($ret, $out, $err) = $node->psql('rc4', 'SELECT * FROM kingres.ledger_head()');
like($err, qr/the head is block 1, but the row of transaction \d+ in kingres\.ledger_tx names block 2/,
	'[rc4] ledger_head stops, on the transaction row');
like(findings('rc4'), qr/digest_block x\d+,tx_block x\d+/, '[rc4] ledger_verify reports the rows');
($stdout, $dir) = run_recipe('rc4');
my $expected_removed = $txrows + $digests;
is($node->safe_psql('rc4', 'SELECT height FROM kingres.ledger_head()'), '1', '[rc4] ledger_head answers, with the block that is there');
is(findings('rc4'), "operator_removal x$expected_removed", '[rc4] exactly the removals the README lists');
is($node->safe_psql('rc4', q(SELECT count(*) FROM kingres.ledger_pending)), $txrows,
	'[rc4] the transactions of the removed block are queued again: no entry is left without a block');
is($node->safe_psql('rc4', q(SELECT kingres.ledger_build_block())), '2', '[rc4] and the next block is built from them');
is(findings('rc4'), "operator_removal x$expected_removed", '[rc4] and nothing else is reported');
ok(-s "$dir/stray-rows.json", '[rc4] the file of the recipe exists');
my $json = slurp_file("$dir/stray-rows.json");
like($json, qr/"transactions"/, '[rc4] and holds the transactions');
like($json, qr/"digests"/, 'and the digests');

#--------------------------------------------------------------------------
# What the recipe refuses.
#--------------------------------------------------------------------------
($ret, $out, $err) = $node->psql('rc4', q(SELECT kingres.ledger_remove_stray('no setting')));
like($err, qr/manual write/, 'without the setting the function refuses');
($ret, $out, $err) = $node->psql('rc4', q(SELECT kingres.ledger_remove_stray('a member')),
	extra_params => [ '-U', 'rc_member' ]);
like($err, qr/permission denied/, 'a member of pg_write_all_data cannot call it');
($ret, $out, $err) = $node->psql('rc4', q(SET ROLE rc_member; SET kingres_ledger.allow_manual_write = on;));
like($err, qr/permission denied to set parameter/, 'and cannot turn the setting on');

$node->stop;
done_testing();
