# contrib/kingres_ledger/t/012_publish_read.pl
#
# The reads for tile publication (kingres.ledger_head,
# kingres.ledger_blocks_entries, kingres.ledger_block_preimage) outside what
# one session of pg_regress can show:
#
#   * an independent recomputation: the entries the function returns are
#     parsed and hashed here with Digest::SHA (not by the extension), the
#     hashes are compared with the stored block hashes, the links of the
#     chain are checked from the genesis hash, and the RFC 6962 tree over the
#     entries is built here and compared with the root the extension's own
#     aggregate gives over the same leaves;
#   * the same entries written as tiles (tlog-tiles) with a checkpoint signed
#     here, and checked by the Go verifier when KINGRES_VERIFY names its
#     binary (skipped otherwise);
#   * concurrent calls while writers and block builders run: every call sees
#     one snapshot, the head never goes back, no call fails;
#   * a streaming standby gives the same answers as its primary;
#   * the bound of 10000 entries per call.
#
# KINGRES_VERIFY=/path/to/kingres-verify runs the verifier check (it needs
# the openssl command for Ed25519 signatures).
#
# Portions Copyright (c) 2026, RooyeKhat Media Co
# SPDX-License-Identifier: AGPL-3.0-only

use strict;
use warnings FATAL => 'all';

use Digest::SHA qw(sha256 sha256_hex);
use File::Path qw(make_path);
use IPC::Run;
use MIME::Base64 qw(encode_base64);
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
	my ($node, $db, $sql) = @_;
	return $node->safe_psql($db, $sql);
}

#--------------------------------------------------------------------------
# A chain of 40 blocks over an append-only and an updatable table.
#--------------------------------------------------------------------------
$primary->safe_psql('postgres', 'CREATE DATABASE pub');
q1($primary, 'pub', q(
	CREATE EXTENSION kingres_ledger;
	CREATE TABLE public.a (id int PRIMARY KEY, v text);
	CREATE TABLE public.u (id int PRIMARY KEY, v text);
	SELECT kingres.ledger_enable('public.a');
	SELECT kingres.ledger_enable('public.u', 'updatable');
));
for my $i (1 .. 40)
{
	q1($primary, 'pub', "INSERT INTO a VALUES ($i, 'row $i'); "
		. ($i % 3 == 0 ? "INSERT INTO u VALUES ($i, 'u'); UPDATE u SET v = 'w' WHERE id = $i; " : '')
		. 'SELECT kingres.ledger_build_block();');
}
is(q1($primary, 'pub', 'SELECT count(*) FROM kingres.ledger_blocks'), '40', 'the chain has 40 blocks');

# All entries, in pages of 7 (a size that does not divide 40).
sub read_all
{
	my ($node, $db, $page) = @_;
	my @rows;
	my $from = 0;
	while (1)
	{
		my $out = q1($node, $db,
			"SELECT height || ' ' || encode(entry, 'hex') FROM kingres.ledger_blocks_entries($from, $page) ORDER BY height");
		my @lines = split /\n/, $out;
		last if !@lines;
		cmp_ok(scalar(@lines), '<=', $page, "page from $from is at most $page") if $from == 0;
		foreach my $l (@lines)
		{
			my ($h, $hex) = split / /, $l;
			push @rows, [ $h, pack('H*', $hex) ];
		}
		$from = $rows[-1][0] + 1;
	}
	return @rows;
}

my @rows = read_all($primary, 'pub', 7);
is(scalar(@rows), 40, 'paging by 7 returns all 40 blocks');
is_deeply([ map { $_->[0] } @rows ], [ 0 .. 39 ], 'the heights are 0 to 39 in order, no gap, no repeat');
is(scalar(grep { length($_->[1]) == 87 } @rows), 40, 'every entry is 87 bytes');

# Stored blocks, read as text.
my %stored;
foreach my $l (split /\n/, q1($primary, 'pub', q(
	SELECT height || ' ' || encode(block_hash, 'hex') || ' ' || encode(prev_hash, 'hex') || ' '
	    || encode(tx_merkle_root, 'hex') || ' ' || tx_count || ' '
	    || round((extract(epoch FROM closed_at) - 946684800) * 1000000)::bigint
	  FROM kingres.ledger_blocks ORDER BY height)))
{
	my ($h, $bh, $ph, $tr, $tc, $us) = split / /, $l;
	$stored{$h} = { hash => $bh, prev => $ph, root => $tr, count => $tc, us => $us };
}
my ($chain_hex, $head_h, $head_hash, $head_us) = split /\|/,
	q1($primary, 'pub', q(SELECT encode(chain_id, 'hex') || '|' || height || '|' || encode(block_hash, 'hex') || '|' || closed_at_us FROM kingres.ledger_head()));
is($head_h, 39, 'ledger_head names the last block');
is($head_hash, $stored{39}{hash}, 'and its stored hash');
is($head_us, $stored{39}{us}, 'and its close time in microseconds since 2000-01-01');
is($chain_hex, q1($primary, 'pub', q(SELECT encode(chain_id, 'hex') FROM kingres.ledger_chain)), 'and the chain identity');

# The recomputation: hash, fields, links, all with Perl.
my $ok_hash = 0;
my $ok_fields = 0;
my $ok_link = 0;
my $prev = sha256("\x14" . pack('H*', $chain_hex));    # the genesis hash
foreach my $r (@rows)
{
	my ($h, $e) = @$r;
	$ok_hash++ if sha256_hex($e) eq $stored{$h}{hash};
	my ($tag, $ver, $hh, $p, $root, $cnt, $us) = unpack('C n Q> a32 a32 N q>', $e);
	$ok_fields++ if $tag == 0x13 && $ver == 1 && $hh == $h
	  && unpack('H*', $p) eq $stored{$h}{prev} && unpack('H*', $root) eq $stored{$h}{root}
	  && $cnt == $stored{$h}{count} && $us == $stored{$h}{us};
	$ok_link++ if $p eq $prev;
	$prev = sha256($e);
}
is($ok_hash, 40, 'SHA-256 of every entry is the stored block hash (computed here)');
is($ok_fields, 40, 'every field of every entry parses to the stored column (computed here)');
is($ok_link, 40, 'every entry links to the hash of the one before, the first to the genesis hash');

# RFC 6962 over the entries, with Digest::SHA.
sub mth
{
	my ($leaves, $lo, $hi) = @_;
	my $n = $hi - $lo;
	return sha256('') if $n == 0;
	return $leaves->[$lo] if $n == 1;
	my $k = 1;
	$k *= 2 while $k * 2 < $n;
	return sha256("\x01" . mth($leaves, $lo, $lo + $k) . mth($leaves, $lo + $k, $hi));
}
my @leaf = map { sha256("\x00" . $_->[1]) } @rows;
my $root = unpack('H*', mth(\@leaf, 0, scalar(@leaf)));
is($root,
	q1($primary, 'pub', q(SELECT encode(kingres.ledger_merkle_root(kingres.ledger_leaf_hash(e.entry) ORDER BY e.height), 'hex')
	                       FROM kingres.ledger_blocks_entries(0, 100) e)),
	'the root computed here is the root of the extension over the same leaves');
my $all_prefix_roots = 1;
foreach my $n (1, 2, 3, 7, 8, 9, 16, 17, 31, 33)
{
	my $sql_root = q1($primary, 'pub', qq(SELECT encode(kingres.ledger_merkle_root(kingres.ledger_leaf_hash(e.entry) ORDER BY e.height), 'hex')
	                       FROM kingres.ledger_blocks_entries(0, $n) e));
	$all_prefix_roots = 0 if unpack('H*', mth(\@leaf, 0, $n)) ne $sql_root;
}
ok($all_prefix_roots, 'and so is the root of every prefix tried, whatever the shape of the tree');

#--------------------------------------------------------------------------
# Tiles and a signed checkpoint, checked by the Go verifier (optional).
#--------------------------------------------------------------------------
sub tile_path
{
	my ($level, $n, $width) = @_;
	my @g;
	my $m = $n;
	do { unshift @g, sprintf('%03d', $m % 1000); $m = int($m / 1000); } while ($m > 0);
	my $p = join('/', map { ($_ == $#g) ? $g[$_] : "x$g[$_]" } 0 .. $#g);
	return "tile/$level/$p" . ($width < 256 ? ".p/$width" : '');
}

sub write_tiles
{
	my ($dir, $leaves) = @_;
	my $size = scalar(@$leaves);
	my @lvl = ($leaves);
	my $l = 0;
	while (1)
	{
		my @next;
		my $cur = $lvl[$l];
		for (my $i = 0; $i + 1 < @$cur; $i += 2)
		{
			push @next, sha256("\x01" . $cur->[$i] . $cur->[$i + 1]);
		}
		last if !@next;
		push @lvl, \@next;
		$l++;
	}
	for (my $L = 0; 8 * $L < @lvl; $L++)
	{
		my $hashes = $lvl[8 * $L];
		my $cnt = scalar(@$hashes);
		next if $cnt == 0;
		for (my $n = 0; $n * 256 < $cnt; $n++)
		{
			my $w = $cnt - $n * 256;
			$w = 256 if $w > 256;
			my $path = "$dir/" . tile_path($L, $n, $w);
			(my $d = $path) =~ s{/[^/]*$}{};
			make_path($d);
			open(my $fh, '>:raw', $path) or die "$path: $!";
			print $fh join('', @{$hashes}[ $n * 256 .. $n * 256 + $w - 1 ]);
			close $fh;
		}
	}
}

SKIP:
{
	my $verify = $ENV{KINGRES_VERIFY};
	skip 'KINGRES_VERIFY does not name the kingres-verify binary', 8
	  if !defined($verify) || $verify eq '' || !-x $verify;
	my $have_openssl = system('openssl version >/dev/null 2>&1') == 0;
	skip 'the openssl command is not available', 8 if !$have_openssl;

	my $work = $primary->basedir . '/publish';
	make_path($work);
	# An Ed25519 key for the test; the public key is the last 32 bytes of the DER.
	system("openssl genpkey -algorithm ed25519 -out $work/key.pem 2>/dev/null") == 0 or die 'genpkey';
	system("openssl pkey -in $work/key.pem -pubout -outform DER -out $work/pub.der 2>/dev/null") == 0 or die 'pubout';
	open(my $pf, '<:raw', "$work/pub.der") or die;
	local $/;
	my $der = <$pf>;
	close $pf;
	my $pub = substr($der, -32);
	my $prefix = 'ledger.example.test/kingres';
	my $origin = "$prefix/c/$chain_hex/e1";

	my $note = sub {
		my ($size) = @_;
		my $r = $size == 0 ? sha256('') : mth(\@leaf, 0, $size);
		my $text = "$origin\n$size\n" . encode_base64($r, '') . "\n";
		open(my $mf, '>:raw', "$work/msg") or die;
		print $mf $text;
		close $mf;
		system("openssl pkeyutl -sign -inkey $work/key.pem -rawin -in $work/msg -out $work/sig 2>/dev/null") == 0 or die 'sign';
		open(my $sf, '<:raw', "$work/sig") or die;
		my $sig = <$sf>;
		close $sf;
		my $keyid = substr(sha256($origin . "\n" . "\x01" . $pub), 0, 4);
		return $text . "\n\xE2\x80\x94 $origin " . encode_base64($keyid . $sig, '') . "\n";
	};
	my $src = "$work/pub";
	my $logdir = "$src/c/$chain_hex/e1";
	make_path($logdir);
	write_tiles($logdir, \@leaf);
	foreach my $pair ([ 'old', 17 ], [ 'new', 40 ])
	{
		open(my $nf, '>:raw', "$work/$pair->[0].note") or die;
		print $nf $note->($pair->[1]);
		close $nf;
	}
	open(my $cf, '>:raw', "$logdir/checkpoint") or die;
	{
		open(my $nn, '<:raw', "$work/new.note") or die;
		print $cf scalar(<$nn>);
		close $nn;
	}
	close $cf;

	my $fp = sha256_hex($pub);
	my $pubb64 = encode_base64($pub, '');
	my $trust = "$work/trust.json";
	my ($so, $se);
	ok(IPC::Run::run([ $verify, 'trust', 'init', '--origin-prefix', $prefix, '--public-key', $pubb64,
			'--fingerprint', $fp, '--out', $trust ], '>', \$so, '2>', \$se), 'trust init') or diag($se);
	my $log = "chain:$chain_hex:e1";
	ok(IPC::Run::run([ $verify, 'checkpoint', '--trust', $trust, '--log', $log, '--source', $src ], '>', \$so, '2>', \$se),
		'the verifier accepts the checkpoint signed here') or diag("$so$se");
	ok(IPC::Run::run([ $verify, 'consistency', '--trust', $trust, '--log', $log, '--old', "$work/old.note",
			'--source', $src ], '>', \$so, '2>', \$se),
		'the verifier finds the log of 40 leaves consistent with the log of 17, from tiles built from ledger_blocks_entries')
	  or diag("$so$se");
	like($so, qr/VERIFIED/, 'and says VERIFIED');
	# The size 17 root the verifier has to reach from the tiles is the one computed here.
	ok(IPC::Run::run([ $verify, 'consistency', '--trust', $trust, '--log', $log,
			'--old', '17:' . encode_base64(mth(\@leaf, 0, 17), ''), '--source', $src ], '>', \$so, '2>', \$se),
		'with the old root given as size and root') or diag("$so$se");
	# A wrong old root is not consistent.
	ok(!IPC::Run::run([ $verify, 'consistency', '--trust', $trust, '--log', $log,
			'--old', '17:' . encode_base64(sha256('wrong'), ''), '--source', $src ], '>', \$so, '2>', \$se),
		'a wrong old root is refused');
	# One leaf hash of the last block changed in the tile: refused.
	my $tile0 = "$logdir/" . tile_path(0, 0, 40);
	$tile0 = "$logdir/tile/0/000.p/40" if !-e $tile0;
	open(my $tf, '+<:raw', $tile0) or die "$tile0: $!";
	seek($tf, 39 * 32, 0);
	print $tf sha256('tampered');
	close $tf;
	ok(!IPC::Run::run([ $verify, 'consistency', '--trust', $trust, '--log', $log, '--old', "$work/old.note",
			'--source', $src ], '>', \$so, '2>', \$se),
		'a changed leaf hash in the tile is refused');
	like($so . $se, qr/FAILED|mismatch|not consistent|wrong|differ/i, 'with a failure message');
}

#--------------------------------------------------------------------------
# A standby gives what the primary gives.
#--------------------------------------------------------------------------
$primary->backup('b1');
my $standby = PostgreSQL::Test::Cluster->new('standby');
$standby->init_from_backup($primary, 'b1', has_streaming => 1);
$standby->append_conf('postgresql.conf', "shared_preload_libraries = 'kingres_ledger'");
$standby->start;
$primary->wait_for_catchup($standby);
is(q1($standby, 'pub', 'SELECT pg_is_in_recovery()'), 't', 'the standby is in recovery');
my @srows = read_all($standby, 'pub', 7);
is(scalar(@srows), 40, 'the standby returns all 40 blocks');
ok(join('', map { $_->[1] } @srows) eq join('', map { $_->[1] } @rows), 'with the same entries as the primary');
is(q1($standby, 'pub', q(SELECT height || ' ' || encode(block_hash, 'hex') FROM kingres.ledger_head())),
	"39 $head_hash", 'and the same head');
is(q1($standby, 'pub', q(BEGIN READ ONLY; SELECT count(*) FROM kingres.ledger_blocks_entries(0, 5); COMMIT)),
	'5', 'the call is a read-only transaction');

#--------------------------------------------------------------------------
# Concurrent calls while writers and block builders run.
#--------------------------------------------------------------------------
q1($primary, 'pub', 'CREATE TABLE public.w (client int, note text); SELECT kingres.ledger_enable(\'public.w\')');
my $dir = $PostgreSQL::Test::Utils::tmp_check;
append_to_file("$dir/w12.sql", "INSERT INTO w SELECT :client_id, 'x' FROM generate_series(1, 3);\n");
append_to_file("$dir/b12.sql", "SELECT kingres.ledger_build_block();\n");
my @pgbench = ('pgbench', '-n', '-h', $primary->host, '-p', $primary->port);
my ($wout, $werr, $bout, $berr);
my $writers = IPC::Run::start(
	[ @pgbench, '-c', '4', '-j', '2', '-t', '400', '-f', "$dir/w12.sql", 'pub' ], '>', \$wout, '2>', \$werr);
my $builders = IPC::Run::start(
	[ @pgbench, '-c', '2', '-j', '2', '-t', '800', '-f', "$dir/b12.sql", 'pub' ], '>', \$bout, '2>', \$berr);

# One statement: the head and the page are read by one snapshot, so the last
# entry is the head.  Errors (XX001 on a torn read) would die here.
my $snapshot_sql = q(
	SELECT h.height || ' ' || (SELECT max(e.height) FROM kingres.ledger_blocks_entries(0, 10000) e)
	    || ' ' || (SELECT count(*) FROM kingres.ledger_blocks_entries(0, 10000) e2)
	  FROM kingres.ledger_head() h);
my ($calls, $bad, $back, $last) = (0, 0, 0, -1);
my %seen;
my $standby_last = -1;
do
{
	foreach my $node ($primary, $standby)
	{
		my ($h, $max, $cnt) = split / /, q1($node, 'pub', $snapshot_sql);
		$calls++;
		$seen{$h}++ if $node == $primary;
		$bad++ if $h != $max || $cnt != $h + 1;
		if ($node == $primary)
		{
			$back++ if $h < $last;
			$last = $h;
		}
		else
		{
			$back++ if $h < $standby_last;
			$standby_last = $h;
		}
	}
	$writers->pump_nb if $writers->pumpable;
	$builders->pump_nb if $builders->pumpable;
} while ($writers->pumpable || $builders->pumpable);
ok($writers->finish, 'writers finished') or diag($werr);
ok($builders->finish, 'builders finished') or diag($berr);
cmp_ok($calls, '>=', 2, "calls were made while the load ran ($calls)");
diag("distinct heads seen on the primary during the run: " . scalar(keys %seen));
is($bad, 0, 'every call saw one snapshot: the head is the last entry and no block is missing');
is($back, 0, 'the head never went back, on the primary or on the standby');
# Close what is pending and compare the end state.
while (q1($primary, 'pub', 'SELECT kingres.ledger_build_block()') ne '') { }
is(q1($primary, 'pub', 'SELECT count(*) FROM kingres.ledger_verify()'), '0', 'the ledger verifies after the run');
$primary->wait_for_catchup($standby);
my $final_primary = q1($primary, 'pub', q(SELECT height || ' ' || encode(block_hash, 'hex') FROM kingres.ledger_head()));
is(q1($standby, 'pub', q(SELECT height || ' ' || encode(block_hash, 'hex') FROM kingres.ledger_head())),
	$final_primary, 'the standby reaches the primary head');
my @prows = read_all($primary, 'pub', 10000);
my @srows2 = read_all($standby, 'pub', 10000);
ok(join('', map { $_->[1] } @prows) eq join('', map { $_->[1] } @srows2), 'and the same entries');
cmp_ok(scalar(@prows), '>', 41, 'blocks were built under the load');
is(scalar(grep { sha256_hex($_->[1]) eq q1($primary, 'pub', "SELECT encode(block_hash, 'hex') FROM kingres.ledger_blocks WHERE height = $_->[0]") } @prows[ 0 .. 49 ]),
	50, 'the first 50 entries hash to their stored hashes after the run');
is(q1($primary, 'pub', q(SELECT (SELECT count(*) FROM kingres.ledger_digest))), '0',
	'no digest was recorded by any of the calls');
$standby->stop;

#--------------------------------------------------------------------------
# The bound of 10000 entries per call.
#--------------------------------------------------------------------------
$primary->safe_psql('postgres', 'CREATE DATABASE big');
q1($primary, 'big', q(
	CREATE EXTENSION kingres_ledger;
	CREATE TABLE public.t (id int);
	SELECT kingres.ledger_enable('public.t');
	DO $$ BEGIN FOR i IN 1..10030 LOOP INSERT INTO public.t VALUES (i); COMMIT; END LOOP; END $$;
	SET kingres_ledger.max_block_transactions = 1;
	SELECT count(kingres.ledger_build_block()) FROM generate_series(1, 10030);
));
is(q1($primary, 'big', 'SELECT count(*) FROM kingres.ledger_blocks'), '10030', 'a chain of 10030 blocks');
is(q1($primary, 'big', 'SELECT count(*) || \' \' || min(height) || \' \' || max(height) FROM kingres.ledger_blocks_entries(0, 2147483647)'),
	'10000 0 9999', 'a huge max_count returns 10000 entries');
is(q1($primary, 'big', 'SELECT count(*) || \' \' || min(height) || \' \' || max(height) FROM kingres.ledger_blocks_entries(10000, 2147483647)'),
	'30 10000 10029', 'the next call returns the rest');
is(q1($primary, 'big', 'SELECT count(*) || \' \' || min(height) || \' \' || max(height) FROM kingres.ledger_blocks_entries(9990, 50)'),
	'40 9990 10029', 'a page that crosses 10000 is not cut there');
is(q1($primary, 'big', 'SELECT count(*) FROM kingres.ledger_blocks_entries(10030, 5)'), '0', 'past the head: nothing');
is(q1($primary, 'big', 'SELECT count(*) FROM kingres.ledger_verify()'), '0', 'the long chain verifies');

$primary->stop;
done_testing();
