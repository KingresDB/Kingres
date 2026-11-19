# contrib/kingres_columnar/t/005_corruption.pl
#
# Corrupt and hostile files: every read of a damaged columnar table must end
# in a clean error (SQLSTATE XX001, or XX002 for the catalog index), or in a
# result, and never in a crash, an assertion failure, a hang or an absurd
# allocation.  Other tables must keep working and the server must stay up.
#
# How it works.  Many identical victim tables are built (two stripes of 1000
# rows, 10 chunk groups each, five columns, one table per scenario), the
# server is stopped cleanly so that every page is on disk, and each scenario
# edits the bytes of its own relation file with sysseek/syswrite.  After the
# restart the stripe catalog rows of some scenarios are made hostile with
# UPDATE.  Then a battery of statements runs against each victim, every one
# in its own session, and each outcome is classified:
#
#   ok      the statement returned a result (the data may be wrong: the
#           format has a checksum of the footer and of the pages, not of the
#           chunks, so a flipped value byte inside an uncompressed fixed-width
#           chunk is expected to read back as a different value)
#   clean   the statement failed with XX001 or XX002 (or a documented
#           SQLSTATE of the scenario, for example 0A000 for a format version
#           this server does not read)
#   BAD     any other SQLSTATE (for example an out of memory error caused by
#           a hostile length), a lost connection, or a timeout
#
# A crash is found two ways: the lost connection of the session, and a
# TRAP:/signal line in the server log.  Some scenarios also require that
# the damage is detected (must_error): a statement that touches the damaged
# structure has to fail, not return a result.
#
# The checksum of the footer is computed in Perl (CRC-32C, Castagnoli, as the
# server does with pg_crc32c) so that a scenario can craft a footer with
# hostile fields that still passes the checksum.
#
# Environment: KC_CORRUPT_SEED fixes the seed of the random byte flips (the
# default is a fixed number, so the test is reproducible; set it to explore);
# KC_CORRUPT_FUZZ is the number of footer-entry fuzz scenarios (default 40);
# KC_CORRUPT_ONLY=<regex> runs only the scenarios whose name matches, and
# KC_CORRUPT_VERBOSE=1 prints the edits and the outcome of every statement of
# every scenario (for debugging a failure).
#
# The test starts two clusters: one with data checksums off, so that the
# structural checks of the format are what finds the damage, and one with
# checksums on.  PostgreSQL 18 turns checksums on by default, so the first is
# created with --no-data-checksums.  When prlimit is available the address
# space of the servers is capped at 3 GB, so that a hostile length that makes
# the server request a multi-gigabyte buffer shows up as an out of memory
# error (SQLSTATE 53200), which counts as a failure.
#
# Portions Copyright (c) 2026, RooyeKhat Media Co
# SPDX-License-Identifier: AGPL-3.0-only

use strict;
use warnings FATAL => 'all';
no warnings 'portable';			# 64-bit constants are used on purpose

use Fcntl qw(:DEFAULT :seek);
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;

my $seed = defined $ENV{KC_CORRUPT_SEED} ? $ENV{KC_CORRUPT_SEED} : 20260601;
srand($seed);
diag("KC_CORRUPT_SEED=$seed");

# ---------------------------------------------------------------------------
# The page layout of the format (kingres_columnar.h)
# ---------------------------------------------------------------------------
my $BLCKSZ  = 8192;
my $PGHDR   = 24;                        # SizeOfPageHeaderData
my $SPECIAL = 24;                        # KcPageSpecial
my $PAYLOAD = $BLCKSZ - $PGHDR - $SPECIAL;
my $META    = $PGHDR;                    # KcMetaData starts here in block 0
my $SPECOFF = $BLCKSZ - $SPECIAL;        # offset of the special area in a page

# statement outcomes accepted as a clean failure
my %CLEAN = (XX001 => 1, XX002 => 1);

# ---------------------------------------------------------------------------
# CRC-32C (Castagnoli), reflected polynomial 0x82F63B78, as pg_crc32c
# ---------------------------------------------------------------------------
my @CRC_TABLE;
for my $i (0 .. 255)
{
	my $c = $i;
	for (1 .. 8) { $c = ($c & 1) ? (($c >> 1) ^ 0x82F63B78) : ($c >> 1); }
	$CRC_TABLE[$i] = $c;
}

sub crc32c
{
	my ($data) = @_;
	my $crc = 0xFFFFFFFF;
	$crc = $CRC_TABLE[($crc ^ $_) & 0xFF] ^ ($crc >> 8)
	  for unpack('C*', $data);
	return ($crc ^ 0xFFFFFFFF) & 0xFFFFFFFF;
}

# Known answer of CRC-32C: the check value of "123456789" is 0xE3069283.
is(crc32c('123456789'), 0xE3069283, 'the Perl CRC-32C matches the standard check value');

# ---------------------------------------------------------------------------
# Raw file access
# ---------------------------------------------------------------------------
sub file_read
{
	my ($path, $off, $len) = @_;
	sysopen(my $fh, $path, O_RDONLY) or die "open $path: $!";
	sysseek($fh, $off, SEEK_SET) or die "seek $path: $!";
	my $buf = '';
	my $n = sysread($fh, $buf, $len);
	die "read $path: $!" unless defined $n;
	close($fh);
	return $buf;
}

sub file_write
{
	my ($path, $off, $data) = @_;
	sysopen(my $fh, $path, O_RDWR) or die "open $path: $!";
	sysseek($fh, $off, SEEK_SET) or die "seek $path: $!";
	my $n = syswrite($fh, $data);
	die "write $path: $!" unless defined $n && $n == length($data);
	close($fh);
}

# ---------------------------------------------------------------------------
# The cluster
# ---------------------------------------------------------------------------
my $node = PostgreSQL::Test::Cluster->new('corrupt');
# PostgreSQL 18 turns data checksums on by default; this cluster has them off so
# that the structural checks of the format are what detects a damaged byte
# (the second cluster, below, runs with checksums)
$node->init(extra => ['--no-data-checksums']);
$node->append_conf('postgresql.conf', <<'EOC');
shared_preload_libraries = 'kingres_columnar'
autovacuum = off
fsync = off
log_min_messages = warning
EOC
$node->start;
$node->safe_psql('postgres', 'CREATE EXTENSION kingres_columnar');
is($node->safe_psql('postgres', 'SHOW data_checksums'), 'off',
	'data checksums are off in the first cluster');

# A cap on the address space of the postmaster (inherited by every backend
# started afterwards) turns a hostile length that makes the server ask for
# gigabytes into an out of memory error that the test can see; without it
# the allocation would only be virtual and go unnoticed.
my $have_prlimit = system('prlimit --version >/dev/null 2>&1') == 0;

sub limit_memory
{
	return unless $have_prlimit;
	my $pid = $node->{_pid};
	return unless defined $pid;
	system('prlimit', '--as=' . (3 * 1024 * 1024 * 1024), "--pid=$pid");
}
limit_memory();

# ---------------------------------------------------------------------------
# Victim tables
# ---------------------------------------------------------------------------
my $ROWS = 2000;
my $healthy_rows = 500;

$node->safe_psql('postgres',
	"CREATE TABLE healthy (id int, v text) USING heap;"
	  . "INSERT INTO healthy SELECT g, md5(g::text) FROM generate_series(1, $healthy_rows) g;");

my @scen;                                # the scenarios, in order

# KC_CORRUPT_ONLY=<regex> runs only the scenarios whose name matches (for debugging)
my $ONLY = $ENV{KC_CORRUPT_ONLY};

# add(name => ..., codec => ..., file => sub, sql => sub, must_error => [..], ...)
sub add
{
	my ($name, %o) = @_;
	return if defined $ONLY && $name !~ /$ONLY/ && !$o{control};
	$o{name}  = $name;
	$o{codec} //= 'zstd';
	$o{bat}   //= 'full';
	$o{table} = sprintf('v%03d', scalar(@scen) + 1);
	push @scen, \%o;
}

# the statement battery; the keys are used by must_error
my @BATTERY = (
	[ count   => 'SELECT count(*) FROM %T' ],
	[ sum     => 'SELECT sum(i), sum(f), sum(id) FROM %T' ],
	[ full    => "SELECT md5(string_agg(x::text, '')) FROM %T x" ],
	[ copy    => 'COPY %T TO STDOUT' ],
	[ skip1   => "SELECT count(*) FROM %T WHERE i BETWEEN 100 AND 200 AND s >= 'a'" ],
	[ skip2   => "SET kingres_columnar.enable_custom_scan = off;\n"
		  . "SELECT count(*) FROM %T WHERE i = ANY (ARRAY[5, 1500]) OR s = 'zz'" ],
	[ skip3   => "SET kingres_columnar.enable_chunk_skipping = off;\n"
		  . 'SELECT count(*) FROM %T WHERE i > 1900' ],
	[ isnull  => 'SELECT count(*) FROM %T WHERE s IS NULL AND i IS NOT NULL' ],
	[ options => "SELECT * FROM kingres.columnar_options('%T')" ],
	[ info    => "SELECT * FROM kingres.columnar_info('%T')" ],
	[ stripes => "SELECT count(*) FROM kingres.columnar_stripes('%T')" ],
	[ analyze => 'ANALYZE %T' ],
	[ check   => 'SELECT count(*) FROM kingres.columnar_check()' ],
	[ vacuum  => 'VACUUM %T' ],
	[ compact => "SELECT kingres.columnar_compact('%T')" ],
	[ insert  => "INSERT INTO %T VALUES (-1, 1, 'x', 1.0, true)" ],
	[ vfull   => 'VACUUM FULL %T' ],
);
my %SHORT = map { $_ => 1 } qw(count full skip1 check insert);

# ---------------------------------------------------------------------------
# Edit helpers.  Every edit is logged in $c->{edits} so a failure prints the
# exact bytes that were changed.
# ---------------------------------------------------------------------------
sub hexs { return unpack("H*", substr($_[0], 0, 32)) . (length($_[0]) > 32 ? "... (" . length($_[0]) . " bytes)" : ""); }

sub poke
{
	my ($c, $abs, $bytes) = @_;
	my $old = file_read($c->{path}, $abs, length($bytes));
	file_write($c->{path}, $abs, $bytes);
	push @{ $c->{edits} },
	  sprintf('file offset %d: %s -> %s', $abs, hexs(substr($old, 0, 24)) . (length($old) > 24 ? '...' : ''), hexs(substr($bytes, 0, 24)) . (length($bytes) > 24 ? '...' : ''));
}

# a stripe's blob is the concatenation of the payloads of its pages
sub blob_read
{
	my ($c, $s) = @_;
	my $blob = '';
	for my $p (0 .. $s->{block_count} - 1)
	{
		my $take = $s->{blob_len} - $p * $PAYLOAD;
		$take = $PAYLOAD if $take > $PAYLOAD;
		last if $take <= 0;
		$blob .= file_read($c->{path}, ($s->{first_block} + $p) * $BLCKSZ + $PGHDR, $take);
	}
	return $blob;
}

sub blob_write
{
	my ($c, $s, $blob) = @_;
	for my $p (0 .. $s->{block_count} - 1)
	{
		my $off = $p * $PAYLOAD;
		last if $off >= length($blob);
		file_write($c->{path}, ($s->{first_block} + $p) * $BLCKSZ + $PGHDR,
			substr($blob, $off, $PAYLOAD));
	}
}

# change bytes of a stripe's blob at a blob offset
sub blob_poke
{
	my ($c, $si, $off, $bytes) = @_;
	my $s    = $c->{stripes}[$si];
	my $blob = blob_read($c, $s);
	my $old  = substr($blob, $off, length($bytes));
	substr($blob, $off, length($bytes)) = $bytes;
	blob_write($c, $s, $blob);
	push @{ $c->{edits} },
	  sprintf('stripe %d blob offset %d (file offset %d): %s -> %s',
		$s->{id}, $off,
		($s->{first_block} + int($off / $PAYLOAD)) * $BLCKSZ + $PGHDR + $off % $PAYLOAD,
		hexs($old), hexs($bytes));
}

# the offset in the file of byte $off of a stripe's blob
sub blob_file_off
{
	my ($c, $si, $off) = @_;
	my $s = $c->{stripes}[$si];
	return ($s->{first_block} + int($off / $PAYLOAD)) * $BLCKSZ + $PGHDR + $off % $PAYLOAD;
}

# the footer of a stripe as a list of groups with their chunk entries
sub footer_layout
{
	my ($f, $ngroups, $ncols) = @_;
	my @groups;
	my $pos = 0;
	for my $g (0 .. $ngroups - 1)
	{
		my %gr = (pos => $pos, rows => unpack('V', substr($f, $pos, 4)), cols => []);
		$pos += 4;
		for my $col (0 .. $ncols - 1)
		{
			my ($off, $comp, $raw, $nn, $crc, $typ, $colln, $minl, $maxl, $codec, $flags) =
			  unpack('Q< V V V V V V v v C C', substr($f, $pos, 38));
			push @{ $gr{cols} },
			  {
				pos   => $pos, off => $off, comp_len => $comp, raw_len => $raw,
				nnulls => $nn, min_len => $minl, max_len => $maxl,
				codec => $codec, flags => $flags,
				minpos => $pos + 40, maxpos => $pos + 40 + $minl,
			  };
			$pos += 40 + $minl + $maxl;
		}
		push @groups, \%gr;
	}
	return @groups;
}

# edit the footer; fix_crc: write the header's footer checksum for the new
# bytes (a crafted footer), otherwise leave the old one (a damaged footer)
sub footer_edit
{
	my ($c, $si, $fix_crc, $code) = @_;
	my $s     = $c->{stripes}[$si];
	my $blob  = blob_read($c, $s);
	my $foff  = $s->{footer_offset};
	my $f     = substr($blob, $foff);
	my @lay   = footer_layout($f, $s->{chunk_groups}, $s->{natts});
	my $orig  = $f;
	$code->(\$f, \@lay, $blob);
	die 'footer length changed' if length($f) != length($orig);
	my @diff;
	for my $i (0 .. length($f) - 1)
	{
		push @diff, sprintf('%d:%02x->%02x', $foff + $i,
			ord(substr($orig, $i, 1)), ord(substr($f, $i, 1)))
		  if substr($f, $i, 1) ne substr($orig, $i, 1);
	}
	substr($blob, $foff) = $f;
	if ($fix_crc)
	{
		substr($blob, 36, 4) = pack('V', crc32c($f));
	}
	blob_write($c, $s, $blob);
	push @{ $c->{edits} },
	  sprintf('stripe %d footer (blob offset %d, crc %s): %s',
		$s->{id}, $foff, $fix_crc ? 'recomputed' : 'unchanged',
		join(' ', @diff[0 .. ($#diff > 39 ? 39 : $#diff)]) . ($#diff > 39 ? ' ...' : ''));
}

# put bytes into a footer buffer at a footer offset
sub fput
{
	my ($fref, $pos, $fmt, @v) = @_;
	my $b = pack($fmt, @v);
	substr($$fref, $pos, length($b)) = $b;
}

# a header field of the stripe blob (offset, pack format, value)
sub hdr_poke
{
	my ($c, $si, $off, $fmt, @v) = @_;
	blob_poke($c, $si, $off, pack($fmt, @v));
}

# a field of the special area of a stripe's page number $p
sub special_poke
{
	my ($c, $si, $p, $off, $fmt, @v) = @_;
	my $s = $c->{stripes}[$si];
	poke($c, ($s->{first_block} + $p) * $BLCKSZ + $SPECOFF + $off, pack($fmt, @v));
}

# a field of the page header of a stripe's page number $p
sub pghdr_poke
{
	my ($c, $si, $p, $off, $fmt, @v) = @_;
	my $s = $c->{stripes}[$si];
	poke($c, ($s->{first_block} + $p) * $BLCKSZ + $off, pack($fmt, @v));
}

# a field of the metapage (offset inside KcMetaData)
sub meta_poke
{
	my ($c, $off, $fmt, @v) = @_;
	poke($c, $META + $off, pack($fmt, @v));
}

# a random chunk of the second group of stripe 0 for the data scenarios
sub chunk_of
{
	my ($c, $si, $g, $col) = @_;
	my $s    = $c->{stripes}[$si];
	my $blob = blob_read($c, $s);
	my $f    = substr($blob, $s->{footer_offset});
	my @lay  = footer_layout($f, $s->{chunk_groups}, $s->{natts});
	return $lay[$g]{cols}[$col];
}

sub sqlset
{
	my ($c, $stripe_idx, $set) = @_;
	return sprintf('UPDATE kingres.columnar_stripe SET %s WHERE rel_uid = %d AND stripe_id = %d',
		$set, $c->{filenode}, $c->{stripes}[$stripe_idx]{id});
}

# ---------------------------------------------------------------------------
# The scenarios
# ---------------------------------------------------------------------------

# --- 0. controls: undamaged tables, one per codec -------------------------
for my $codec (qw(none pglz lz4 zstd))
{
	add("control: undamaged table, codec $codec", codec => $codec, control => 1);
}

# --- 1. the metapage -------------------------------------------------------
my @MI = (qw(insert options));         # the statements that read the metapage
add('meta: bad magic', must_error => \@MI,
	file => sub { meta_poke($_[0], 0, 'V', 0xDEADBEEF) });
add('meta: magic zero', must_error => \@MI,
	file => sub { meta_poke($_[0], 0, 'V', 0) });
add('meta: format version 4 (feature not supported is fine; 2 and 3 are read)',
	must_error => \@MI, ok_states => ['0A000'],
	file => sub { meta_poke($_[0], 4, 'V', 4) });
add('meta: format version 0xFFFFFFFF', must_error => \@MI, ok_states => ['0A000'],
	file => sub { meta_poke($_[0], 4, 'V', 0xFFFFFFFF) });
add('meta: codec 9', must_error => \@MI,
	file => sub { meta_poke($_[0], 8, 'V', 9) });
add('meta: codec 4 (the auto value is never stored)', must_error => \@MI,
	file => sub { meta_poke($_[0], 8, 'V', 4) });
add('meta: codec 0xFFFFFFFF', must_error => \@MI,
	file => sub { meta_poke($_[0], 8, 'V', 0xFFFFFFFF) });
add('meta: stripe_rows 0', must_error => \@MI,
	file => sub { meta_poke($_[0], 16, 'V', 0) });
add('meta: stripe_rows 999 (below the minimum)', must_error => \@MI,
	file => sub { meta_poke($_[0], 16, 'V', 999) });
add('meta: stripe_rows 0xFFFFFFFF', must_error => \@MI,
	file => sub { meta_poke($_[0], 16, 'V', 0xFFFFFFFF) });
add('meta: stripe_rows 10000001 (above the maximum)', must_error => \@MI,
	file => sub { meta_poke($_[0], 16, 'V', 10000001) });
add('meta: chunk_rows 0', must_error => \@MI,
	file => sub { meta_poke($_[0], 20, 'V', 0) });
add('meta: chunk_rows 99', must_error => \@MI,
	file => sub { meta_poke($_[0], 20, 'V', 99) });
add('meta: chunk_rows 100001', must_error => \@MI,
	file => sub { meta_poke($_[0], 20, 'V', 100001) });
add('meta: chunk_rows 0xFFFFFFFF', must_error => \@MI,
	file => sub { meta_poke($_[0], 20, 'V', 0xFFFFFFFF) });
add('meta: level -2147483648 (not validated by the reader; no crash)', codec => 'zstd',
	file => sub { meta_poke($_[0], 12, 'l<', -2147483648) });
add('meta: level 2147483647 with zstd (no crash)', codec => 'zstd',
	file => sub { meta_poke($_[0], 12, 'l<', 2147483647) });
add('meta: level 2147483647 with lz4 (no crash)', codec => 'lz4',
	file => sub { meta_poke($_[0], 12, 'l<', 2147483647) });
add('meta: next_stripe_id 0x7FFFFFFFFFFFFFFF (no ids left is a clean error)', ok_states => ['54000'],
	file => sub { meta_poke($_[0], 32, 'Q<', 0x7FFFFFFFFFFFFFFF) });
add('meta: next_row 0x7FFFFFFFFFFFFFFF (no row numbers left is a clean error)', ok_states => ['54000'],
	file => sub { meta_poke($_[0], 24, 'Q<', 0x7FFFFFFFFFFFFFFF) });
add('meta: next_row at the last number a TID can hold (C6a: no row numbers left is a clean error)',
	must_error => ['insert'], ok_states => ['54000'],
	file => sub { meta_poke($_[0], 24, 'Q<', (0xFFFFFFFE - 1) * 2048) });
add('meta: next_row two numbers before it (an insert of one row works)',
	file => sub { meta_poke($_[0], 24, 'Q<', (0xFFFFFFFE - 1) * 2048 - 2) });
add('meta: page header pd_special 8168 (a special area on the metapage)', must_error => \@MI,
	file => sub { poke($_[0], 16, pack('v', 8168)) });
add('meta: whole page zeroed', must_error => \@MI,
	file => sub { poke($_[0], 0, "\0" x $BLCKSZ) });
add('meta: page header pd_lower 0xFFFF (insane header)', must_error => \@MI,
	file => sub { poke($_[0], 12, pack('v', 0xFFFF)) });

# --- 2. the pages of a stripe ------------------------------------------------
for my $pg (['first', 0], ['second', 1], ['last', -1])
{
	my ($pn, $pidx) = @$pg;
	my $pageof = sub { my ($c) = @_; return $pidx < 0 ? $c->{stripes}[0]{block_count} - 1 : $pidx; };
	add("page: $pn page of stripe 0, special magic 0", must_error => ['full'],
		file => sub { special_poke($_[0], 0, $pageof->($_[0]), 0, 'v', 0) });
	add("page: $pn page of stripe 0, special magic 0xFFFF", must_error => ['full'],
		file => sub { special_poke($_[0], 0, $pageof->($_[0]), 0, 'v', 0xFFFF) });
	add("page: $pn page of stripe 0, page_no wrong", must_error => ['full'],
		file => sub { special_poke($_[0], 0, $pageof->($_[0]), 4, 'V', 77) });
	add("page: $pn page of stripe 0, page_no 0xFFFFFFFF", must_error => ['full'],
		file => sub { special_poke($_[0], 0, $pageof->($_[0]), 4, 'V', 0xFFFFFFFF) });
	add("page: $pn page of stripe 0, page_count wrong", must_error => ['full'],
		file => sub { special_poke($_[0], 0, $pageof->($_[0]), 8, 'V', $_[0]{stripes}[0]{block_count} + 1) });
	add("page: $pn page of stripe 0, page_count 0", must_error => ['full'],
		file => sub { special_poke($_[0], 0, $pageof->($_[0]), 8, 'V', 0) });
	add("page: $pn page of stripe 0, stripe_id of another stripe", must_error => ['full'],
		file => sub { special_poke($_[0], 0, $pageof->($_[0]), 16, 'Q<', $_[0]{stripes}[1]{id}) });
	add("page: $pn page of stripe 0, stripe_id 0xFFFFFFFFFFFFFFFF", must_error => ['full'],
		file => sub { special_poke($_[0], 0, $pageof->($_[0]), 16, 'Q<', 0xFFFFFFFFFFFFFFFF) });
	add("page: $pn page of stripe 0, pd_special 0", must_error => ['full'],
		file => sub { pghdr_poke($_[0], 0, $pageof->($_[0]), 16, 'v', 0) });
	add("page: $pn page of stripe 0, pd_special 8192 (no special area)", must_error => ['full'],
		file => sub { pghdr_poke($_[0], 0, $pageof->($_[0]), 16, 'v', 8192) });
	add("page: $pn page of stripe 0, pd_special 8190 (unaligned)", must_error => ['full'],
		file => sub { pghdr_poke($_[0], 0, $pageof->($_[0]), 16, 'v', 8190) });
	add("page: $pn page of stripe 0, pd_special 0xFFF8 (beyond the page)", must_error => ['full'],
		file => sub { pghdr_poke($_[0], 0, $pageof->($_[0]), 16, 'v', 0xFFF8) });
	add("page: $pn page of stripe 0, pd_lower 0xFFFF", must_error => ['full'],
		file => sub { pghdr_poke($_[0], 0, $pageof->($_[0]), 12, 'v', 0xFFFF) });
	add("page: $pn page of stripe 0, pd_upper 0", must_error => ['full'],
		file => sub { pghdr_poke($_[0], 0, $pageof->($_[0]), 14, 'v', 0) });
	add("page: $pn page of stripe 0, pd_flags 0xFFFF", must_error => ['full'],
		file => sub { pghdr_poke($_[0], 0, $pageof->($_[0]), 10, 'v', 0xFFFF) });
	add("page: $pn page of stripe 0, pd_pagesize_version 0", must_error => ['full'],
		file => sub { pghdr_poke($_[0], 0, $pageof->($_[0]), 18, 'v', 0) });
	add("page: $pn page of stripe 0, zero-filled", must_error => ['full'],
		file => sub { poke($_[0], ($_[0]{stripes}[0]{first_block} + $pageof->($_[0])) * $BLCKSZ, "\0" x $BLCKSZ) });
	add("page: $pn page of stripe 0, filled with 0xFF", must_error => ['full'],
		file => sub { poke($_[0], ($_[0]{stripes}[0]{first_block} + $pageof->($_[0])) * $BLCKSZ, "\xFF" x $BLCKSZ) });
}
add('page: first page of stripe 0 replaced by the first page of stripe 1', must_error => ['full'],
	file => sub {
		my $c = shift;
		my $other = file_read($c->{path}, $c->{stripes}[1]{first_block} * $BLCKSZ, $BLCKSZ);
		poke($c, $c->{stripes}[0]{first_block} * $BLCKSZ, $other);
	});
add('page: the metapage copied over the first page of stripe 0', must_error => ['full'],
	file => sub {
		my $c = shift;
		my $meta = file_read($c->{path}, 0, $BLCKSZ);
		poke($c, $c->{stripes}[0]{first_block} * $BLCKSZ, $meta);
	});
add('page: a stripe page copied over the metapage', must_error => \@MI,
	file => sub {
		my $c = shift;
		my $pg = file_read($c->{path}, $c->{stripes}[0]{first_block} * $BLCKSZ, $BLCKSZ);
		poke($c, 0, $pg);
	});

# --- 3. the stripe header --------------------------------------------------
my @H = (
	[ 'magic 0',                         0, 'V', 0 ],
	[ 'magic one bit off',               0, 'V', 0x4B435330 ],
	[ 'version 9',                       4, 'v', 9 ],
	[ 'ncols 0',                         6, 'v', 0 ],
	[ 'ncols 65535',                     6, 'v', 65535 ],
	[ 'ncols +1',                        6, 'v', 6 ],
	[ 'nrows 0',                         8, 'V', 0 ],
	[ 'nrows 0xFFFFFFFF',                8, 'V', 0xFFFFFFFF ],
	[ 'nrows +1',                        8, 'V', 1001 ],
	[ 'ngroups 0',                       12, 'V', 0 ],
	[ 'ngroups 0xFFFFFFFF',              12, 'V', 0xFFFFFFFF ],
	[ 'ngroups 0x7FFFFFFF',              12, 'V', 0x7FFFFFFF ],
	[ 'ngroups +1',                      12, 'V', 11 ],
	[ 'stripe_id wrong',                 16, 'Q<', 424242 ],
	[ 'footer_off 0',                    24, 'Q<', 0 ],
	[ 'footer_off 63',                   24, 'Q<', 63 ],
	[ 'footer_off 2^63',                 24, 'Q<', 0x8000000000000000 ],
	[ 'footer_off 0xFFFFFFFFFFFFFFFF',   24, 'Q<', 0xFFFFFFFFFFFFFFFF ],
	[ 'footer_len 0',                    32, 'V', 0 ],
	[ 'footer_len 0xFFFFFFFF',           32, 'V', 0xFFFFFFFF ],
	[ 'footer_len +1',                   32, 'V', 'PLUS1' ],
	[ 'footer_crc 0',                    36, 'V', 0 ],
	[ 'footer_crc 0xFFFFFFFF',           36, 'V', 0xFFFFFFFF ],
	[ 'blob_len 0',                      40, 'Q<', 0 ],
	[ 'blob_len 2^63',                   40, 'Q<', 0x8000000000000000 ],
	[ 'blob_len 0xFFFFFFFFFFFFFFFF',     40, 'Q<', 0xFFFFFFFFFFFFFFFF ],
	[ 'blob_len +1',                     40, 'Q<', 'PLUS1' ],
);
for my $h (@H)
{
	my ($what, $off, $fmt, $val) = @$h;
	add("header: $what", must_error => ['count', 'full'],
		file => sub {
			my $c = shift;
			my $v = $val;
			if ($val eq 'PLUS1')
			{
				$v = $off == 32 ? $c->{stripes}[0]{blob_len} - $c->{stripes}[0]{footer_offset} + 1
				  : $c->{stripes}[0]{blob_len} + 1;
			}
			hdr_poke($c, 0, $off, $fmt, $v);
		});
}
# the header and the catalog row agree on a hostile value: the plausibility
# checks, not the comparison with the catalog, must catch it
add('header+catalog: ngroups 0x7FFFFFFF in both', must_error => ['count', 'full'],
	file => sub { hdr_poke($_[0], 0, 12, 'V', 0x7FFFFFFF) },
	sql  => sub { sqlset($_[0], 0, 'chunk_groups = 2147483647') });
add('header+catalog: ngroups 0x7FFFFFFF, footer_len as large as the checks allow', must_error => ['count', 'full'],
	file => sub { hdr_poke($_[0], 0, 12, 'V', 0x7FFFFFFF) },
	sql  => sub { sqlset($_[0], 0, 'chunk_groups = 2147483647') });
add('header+catalog: ngroups = footer_len / 4 in both', must_error => ['count', 'full'],
	file => sub {
		my $c = shift;
		my $s = $c->{stripes}[0];
		hdr_poke($c, 0, 12, 'V', int(($s->{blob_len} - $s->{footer_offset}) / 4));
	},
	sql => sub {
		my $c = shift;
		my $s = $c->{stripes}[0];
		sqlset($c, 0, 'chunk_groups = ' . int(($s->{blob_len} - $s->{footer_offset}) / 4));
	});
add('header+catalog: nrows 1000000 in both', must_error => ['count', 'full'],
	file => sub { hdr_poke($_[0], 0, 8, 'V', 1000000) },
	sql  => sub { sqlset($_[0], 0, 'row_count = 1000000') });
add('header+catalog: ncols 3 in both (fewer columns than the table)', must_error => ['full'],
	file => sub { hdr_poke($_[0], 0, 6, 'v', 3) },
	sql  => sub { sqlset($_[0], 0, 'natts = 3') });
add('header+catalog: ncols 9 in both (more columns than the table)', must_error => ['full'],
	file => sub { hdr_poke($_[0], 0, 6, 'v', 9) },
	sql  => sub { sqlset($_[0], 0, 'natts = 9') });
add('header+catalog: footer_off = footer_off - 40 and footer_len + 40 in both',
	must_error => ['count', 'full'],
	file => sub {
		my $c = shift;
		my $s = $c->{stripes}[0];
		hdr_poke($c, 0, 24, 'Q<', $s->{footer_offset} - 40);
		hdr_poke($c, 0, 32, 'V', $s->{blob_len} - $s->{footer_offset} + 40);
	},
	sql => sub { sqlset($_[0], 0, 'footer_offset = ' . ($_[0]{stripes}[0]{footer_offset} - 40)) });

# --- 4. the footer -----------------------------------------------------------
# random byte flips without fixing the checksum: the CRC must catch them
for my $i (1 .. 6)
{
	add("footer: random byte flip $i, checksum not fixed", must_error => ['count', 'full'],
		file => sub {
			my $c = shift;
			footer_edit($c, 0, 0, sub {
				my ($f) = @_;
				my $p = int(rand(length($$f)));
				substr($$f, $p, 1) = chr(ord(substr($$f, $p, 1)) ^ (1 << int(rand(8))));
			});
		});
}
add('footer: first byte flipped, checksum not fixed', must_error => ['count', 'full'],
	file => sub { footer_edit($_[0], 0, 0, sub { substr(${ $_[0] }, 0, 1) = chr(ord(substr(${ $_[0] }, 0, 1)) ^ 1) }) });
add('footer: last byte flipped, checksum not fixed', must_error => ['count', 'full'],
	file => sub { footer_edit($_[0], 0, 0, sub { substr(${ $_[0] }, -1, 1) = chr(ord(substr(${ $_[0] }, -1, 1)) ^ 0x80) }) });
add('footer: zero-filled, checksum not fixed', must_error => ['count', 'full'],
	file => sub { footer_edit($_[0], 0, 0, sub { ${ $_[0] } = "\0" x length(${ $_[0] }) }) });

# hostile fields with a recomputed checksum.  Entries: [name, code(col entry)]
# applied to group 1, column col; $cdef gives the codec of the table.
my @FIELDS = (
	[ 'chunk off 0 (inside the header)',            sub { fput($_[0], $_[1]{pos} + 0, 'Q<', 0) } ],
	[ 'chunk off 10',                               sub { fput($_[0], $_[1]{pos} + 0, 'Q<', 10) } ],
	[ 'chunk off beyond the blob',                  sub { fput($_[0], $_[1]{pos} + 0, 'Q<', 1 << 40) } ],
	[ 'chunk off 2^63',                             sub { fput($_[0], $_[1]{pos} + 0, 'Q<', 0x8000000000000000) } ],
	[ 'chunk off 0xFFFFFFFFFFFFFFFF',               sub { fput($_[0], $_[1]{pos} + 0, 'Q<', 0xFFFFFFFFFFFFFFFF) } ],
	[ 'chunk off 8 bytes before the footer, comp_len 64', sub { fput($_[0], $_[1]{pos} + 0, 'Q<', $_[2] - 8); fput($_[0], $_[1]{pos} + 8, 'V', 64) } ],
	[ 'chunk comp_len 0xFFFFFFFF',                  sub { fput($_[0], $_[1]{pos} + 8, 'V', 0xFFFFFFFF) } ],
	[ 'chunk comp_len 0',                           sub { fput($_[0], $_[1]{pos} + 8, 'V', 0) } ],
	[ 'chunk comp_len +1',                          sub { fput($_[0], $_[1]{pos} + 8, 'V', $_[1]{comp_len} + 1) } ],
	[ 'chunk comp_len -1',                          sub { fput($_[0], $_[1]{pos} + 8, 'V', $_[1]{comp_len} - 1) } ],
	[ 'chunk raw_len 0xFFFFFFFF (4 GB)',            sub { fput($_[0], $_[1]{pos} + 12, 'V', 0xFFFFFFFF) } ],
	[ 'chunk raw_len 0x7FFFFFFF (2 GB)',            sub { fput($_[0], $_[1]{pos} + 12, 'V', 0x7FFFFFFF) } ],
	[ 'chunk raw_len 0',                            sub { fput($_[0], $_[1]{pos} + 12, 'V', 0) } ],
	[ 'chunk raw_len +1',                           sub { fput($_[0], $_[1]{pos} + 12, 'V', $_[1]{raw_len} + 1) } ],
	[ 'chunk raw_len -1',                           sub { fput($_[0], $_[1]{pos} + 12, 'V', $_[1]{raw_len} - 1) } ],
	[ 'chunk nnulls 0xFFFFFFFF',                    sub { fput($_[0], $_[1]{pos} + 16, 'V', 0xFFFFFFFF) } ],
	[ 'chunk nnulls > nrows',                       sub { fput($_[0], $_[1]{pos} + 16, 'V', 101) } ],
	[ 'chunk nnulls +1',                            sub { fput($_[0], $_[1]{pos} + 16, 'V', $_[1]{nnulls} + 1) } ],
	[ 'chunk nnulls 100 (all null, bitmap says otherwise)', sub { fput($_[0], $_[1]{pos} + 16, 'V', 100) } ],
	[ 'chunk nnulls 5 with the null flag cleared',  sub { fput($_[0], $_[1]{pos} + 16, 'V', 5); fput($_[0], $_[1]{pos} + 37, 'C', $_[1]{flags} & ~1) } ],
	[ 'chunk null flag set with nnulls 0',          sub { fput($_[0], $_[1]{pos} + 16, 'V', 0); fput($_[0], $_[1]{pos} + 37, 'C', $_[1]{flags} | 1) } ],
	[ 'chunk flags 0xFF',                           sub { fput($_[0], $_[1]{pos} + 37, 'C', 0xFF) } ],
	[ 'chunk flags 0 (no null flag, no min/max)',   sub { fput($_[0], $_[1]{pos} + 37, 'C', 0) } ],
	[ 'chunk codec 9',                              sub { fput($_[0], $_[1]{pos} + 36, 'C', 9) } ],
	[ 'chunk codec 255',                            sub { fput($_[0], $_[1]{pos} + 36, 'C', 255) } ],
	[ 'chunk codec 4 (auto)',                       sub { fput($_[0], $_[1]{pos} + 36, 'C', 4) } ],
	[ 'chunk codec 0 for a compressed chunk',       sub { fput($_[0], $_[1]{pos} + 36, 'C', 0) } ],
	[ 'chunk codec 1 (pglz) for another codec',     sub { fput($_[0], $_[1]{pos} + 36, 'C', 1) } ],
	[ 'chunk codec 2 (lz4) for another codec',      sub { fput($_[0], $_[1]{pos} + 36, 'C', 2) } ],
	[ 'chunk codec 3 (zstd) for another codec',     sub { fput($_[0], $_[1]{pos} + 36, 'C', 3) } ],
	[ 'chunk min_len 65',                           sub { fput($_[0], $_[1]{pos} + 32, 'v', 65) } ],
	[ 'chunk max_len 65',                           sub { fput($_[0], $_[1]{pos} + 34, 'v', 65) } ],
	[ 'chunk min_len 0xFFFF',                       sub { fput($_[0], $_[1]{pos} + 32, 'v', 0xFFFF) } ],
	[ 'chunk max_len 0xFFFF',                       sub { fput($_[0], $_[1]{pos} + 34, 'v', 0xFFFF) } ],
	[ 'chunk min_len 0 with the min/max flag set',  sub { fput($_[0], $_[1]{pos} + 32, 'v', 0) } ],
	[ 'chunk min_len +1, max_len -1 (lengths shift, total unchanged)', sub { fput($_[0], $_[1]{pos} + 32, 'v', $_[1]{min_len} + 1); fput($_[0], $_[1]{pos} + 34, 'v', $_[1]{max_len} ? $_[1]{max_len} - 1 : 0) } ],
	[ 'chunk min_len 3, max_len rest (wrong width for the type)', sub { fput($_[0], $_[1]{pos} + 32, 'v', 3); fput($_[0], $_[1]{pos} + 34, 'v', $_[1]{min_len} + $_[1]{max_len} - 3) } ],
	[ 'chunk padding bytes nonzero',                sub { fput($_[0], $_[1]{pos} + 26, 'a6', "\xFF" x 6) } ],
);
my %FIELD_MUST = (    # those whose damage must be reported by a full read
	'chunk off 0 (inside the header)' => 1, 'chunk off 10' => 1,
	'chunk off beyond the blob' => 1, 'chunk off 2^63' => 1,
	'chunk off 0xFFFFFFFFFFFFFFFF' => 1,
	'chunk comp_len 0xFFFFFFFF' => 1, 'chunk nnulls 0xFFFFFFFF' => 1,
	'chunk nnulls > nrows' => 1, 'chunk nnulls +1' => 1,
	'chunk nnulls 100 (all null, bitmap says otherwise)' => 1,
	'chunk nnulls 5 with the null flag cleared' => 1,
	'chunk flags 0xFF' => 1, 'chunk codec 9' => 1, 'chunk codec 255' => 1,
	'chunk codec 4 (auto)' => 1, 'chunk min_len 65' => 1, 'chunk max_len 65' => 1,
	'chunk min_len 0xFFFF' => 1, 'chunk max_len 0xFFFF' => 1,
);

# column 2 is the text column (variable width, with nulls), column 1 an int4
# with nulls, column 0 an int8 without; run the fields against text and int
# with the zstd codec, and the length fields against the other codecs too
for my $f (@FIELDS)
{
	my ($what, $code) = @$f;
	my $must = $FIELD_MUST{$what} ? ['full'] : [];
	for my $colspec (['text', 2], ['int4', 1])
	{
		my ($cn, $col) = @$colspec;
		add("footer(crc fixed): $what, $cn column, zstd", codec => 'zstd', must_error => $must,
			file => sub {
				my $c = shift;
				footer_edit($c, 0, 1, sub {
					my ($fr, $lay) = @_;
					$code->($fr, $lay->[1]{cols}[$col], $c->{stripes}[0]{footer_offset});
				});
			});
	}
}
for my $codec (qw(none pglz lz4))
{
	for my $what ('chunk raw_len 0xFFFFFFFF (4 GB)', 'chunk raw_len 0x7FFFFFFF (2 GB)',
		'chunk raw_len +1', 'chunk raw_len -1', 'chunk raw_len 0', 'chunk comp_len +1',
		'chunk comp_len -1', 'chunk codec 0 for a compressed chunk',
		'chunk codec 1 (pglz) for another codec', 'chunk codec 2 (lz4) for another codec',
		'chunk codec 3 (zstd) for another codec')
	{
		my ($f) = grep { $_->[0] eq $what } @FIELDS;
		my $code = $f->[1];
		add("footer(crc fixed): $what, text column, $codec", codec => $codec,
			file => sub {
				my $c = shift;
				footer_edit($c, 0, 1, sub {
					my ($fr, $lay) = @_;
					$code->($fr, $lay->[1]{cols}[2], $c->{stripes}[0]{footer_offset});
				});
			});
	}
}

# group row counts
my @GROUPS = (
	[ 'group 0 rows +1 (sum does not match)',      sub { fput($_[0], $_[1][0]{pos}, 'V', $_[1][0]{rows} + 1) }, 1 ],
	[ 'group 0 rows 0',                            sub { fput($_[0], $_[1][0]{pos}, 'V', 0) }, 1 ],
	[ 'group 0 rows 100001 (above the chunk limit)', sub { fput($_[0], $_[1][0]{pos}, 'V', 100001) }, 1 ],
	[ 'group 0 rows 0xFFFFFFFF',                   sub { fput($_[0], $_[1][0]{pos}, 'V', 0xFFFFFFFF) }, 1 ],
	[ 'group 0 rows +50, group 1 rows -50 (sum kept)', sub { fput($_[0], $_[1][0]{pos}, 'V', $_[1][0]{rows} + 50); fput($_[0], $_[1][1]{pos}, 'V', $_[1][1]{rows} - 50) }, 1 ],
	[ 'group 0 rows 1, group 1 rows 199 (sum kept)', sub { fput($_[0], $_[1][0]{pos}, 'V', 1); fput($_[0], $_[1][1]{pos}, 'V', 199) }, 1 ],
	[ 'group 9 rows -1',                           sub { fput($_[0], $_[1][9]{pos}, 'V', $_[1][9]{rows} - 1) }, 1 ],
);
for my $g (@GROUPS)
{
	my ($what, $code, $must) = @$g;
	add("footer(crc fixed): $what", must_error => ($must ? ['full'] : []),
		file => sub {
			my $c = shift;
			footer_edit($c, 0, 1, sub { $code->($_[0], $_[1]) });
		});
}

# random fuzz: a few random bytes of the footer changed, checksum fixed; the
# only requirement is no crash and a clean error or a result
for my $i (1 .. 20)
{
	add("footer fuzz $i: random bytes changed, checksum fixed", bat => 'short', codec => (qw(none pglz lz4 zstd))[$i % 4],
		file => sub {
			my $c = shift;
			footer_edit($c, 0, 1, sub {
				my ($f) = @_;
				my $n = 1 + int(rand(4));
				for (1 .. $n)
				{
					my $p = int(rand(length($$f)));
					substr($$f, $p, 1) = chr(int(rand(256)));
				}
			});
		});
}

# the same, aimed at the fixed fields of the chunk entries and the group row
# counts (random bytes of random 32-byte entries), where a wrong number turns
# into an offset, a length or a count
my $nfuzz = $ENV{KC_CORRUPT_FUZZ} // 40;
for my $i (1 .. $nfuzz)
{
	add("footer entry fuzz $i: random bytes of random chunk entries changed, checksum fixed",
		bat => 'short', codec => (qw(none pglz lz4 zstd))[$i % 4],
		file => sub {
			my $c = shift;
			footer_edit($c, 0, 1, sub {
				my ($f, $lay) = @_;
				for (1 .. 1 + int(rand(3)))
				{
					my $g = $lay->[ int(rand(@$lay)) ];
					if (rand() < 0.15)
					{
						substr($$f, $g->{pos} + int(rand(4)), 1) = chr(int(rand(256)));
						next;
					}
					my $e = $g->{cols}[ int(rand(@{ $g->{cols} })) ];
					substr($$f, $e->{pos} + int(rand(38)), 1) = chr(int(rand(256)));
				}
			});
		});
}

# statistics that are valid in shape but hostile in meaning: a variable-length
# minimum that is an external or an indirect varlena pointer.  The scan
# compares it with the constant of the qual, which detoasts it.
sub stat_bytes_edit
{
	my ($c, $minb) = @_;
	footer_edit($c, 0, 1, sub {
		my ($f, $lay) = @_;
		# the first group whose text column has statistics
		for my $g (@$lay)
		{
			my $e = $g->{cols}[2];
			next unless ($e->{flags} & 2) && $e->{min_len} + $e->{max_len} > length($minb) + 1;
			my $tot = $e->{min_len} + $e->{max_len};
			my $rest = $tot - length($minb);
			# max: a valid one-byte-header varlena of $rest bytes
			my $max = chr((($rest) << 1) | 1) . ('z' x ($rest - 1));
			fput($f, $e->{pos} + 32, 'v', length($minb));
			fput($f, $e->{pos} + 34, 'v', $rest);
			substr($$f, $e->{minpos}, $tot) = $minb . $max;
			return;
		}
		die 'no text chunk with statistics';
	});
}
add('stats: text minimum is an indirect varlena pointer (VARTAG_INDIRECT)',
	bat => 'short', must_error => [],
	file => sub { stat_bytes_edit($_[0], pack('CC', 0x01, 0x01) . ("\x41" x 8)) },
	extra => ["SELECT count(*) FROM %T WHERE s = 'abc'", "SELECT count(*) FROM %T WHERE s > 'm'"]);
add('stats: text minimum is an expanded varlena pointer (VARTAG_EXPANDED_RO)',
	bat => 'short',
	file => sub { stat_bytes_edit($_[0], pack('CC', 0x01, 0x02) . ("\x41" x 8)) },
	extra => ["SELECT count(*) FROM %T WHERE s = 'abc'", "SELECT count(*) FROM %T WHERE s > 'm'"]);
add('stats: text minimum is an on-disk toast pointer (VARTAG_ONDISK)',
	bat => 'short',
	file => sub { stat_bytes_edit($_[0], pack('CC', 0x01, 0x12) . ("\x41" x 16)) },
	extra => ["SELECT count(*) FROM %T WHERE s = 'abc'", "SELECT count(*) FROM %T WHERE s > 'm'"]);
add('stats: text minimum has a 4-byte compressed varlena header',
	bat => 'short',
	file => sub { stat_bytes_edit($_[0], pack('V', (12 << 2) | 2) . ("\x41" x 8)) },
	extra => ["SELECT count(*) FROM %T WHERE s = 'abc'", "SELECT count(*) FROM %T WHERE s > 'm'"]);
add('stats: text minimum is a one-byte header claiming a longer value',
	bat => 'short',
	file => sub { stat_bytes_edit($_[0], pack("C", (127 << 1) | 1) . ("\x41" x 11)) },
	extra => ["SELECT count(*) FROM %T WHERE s = 'abc'"]);

# --- 5. the bytes of a chunk -------------------------------------------------
# compressed streams of each codec: columns 0 (int8), 2 (text), 3 (float8)
for my $codec (qw(none pglz lz4 zstd))
{
	for my $colspec (['id', 0], ['text', 2], ['float', 3])
	{
		my ($cn, $col) = @$colspec;
		my $tag = "$codec/$cn";
		add("chunk data $tag: one random byte flipped", codec => $codec, bat => 'short',
			file => sub {
				my $c = shift;
				my $e = chunk_of($c, 0, 3, $col);
				my $p = int(rand($e->{comp_len}));
				my $old = ord(file_read($c->{path}, blob_file_off($c, 0, $e->{off} + $p), 1));
				blob_poke($c, 0, $e->{off} + $p, chr($old ^ (1 << int(rand(8)))));
			});
		add("chunk data $tag: 8 consecutive bytes set to 0xFF", codec => $codec, bat => 'short',
			file => sub {
				my $c = shift;
				my $e = chunk_of($c, 0, 3, $col);
				my $p = int(rand($e->{comp_len} > 8 ? $e->{comp_len} - 8 : 1));
				blob_poke($c, 0, $e->{off} + $p, "\xFF" x ($e->{comp_len} >= 8 ? 8 : $e->{comp_len}));
			});
		add("chunk data $tag: the whole stream is random bytes", codec => $codec, bat => 'short',
			file => sub {
				my $c = shift;
				my $e = chunk_of($c, 0, 3, $col);
				blob_poke($c, 0, $e->{off}, join('', map { chr(int(rand(256))) } 1 .. $e->{comp_len}));
			});
		add("chunk data $tag: the whole stream is zero", codec => $codec, bat => 'short',
			file => sub {
				my $c = shift;
				my $e = chunk_of($c, 0, 3, $col);
				blob_poke($c, 0, $e->{off}, "\0" x $e->{comp_len});
			});
		add("chunk data $tag: the first half of the stream is zero", codec => $codec, bat => 'short',
			file => sub {
				my $c = shift;
				my $e = chunk_of($c, 0, 3, $col);
				blob_poke($c, 0, $e->{off}, "\0" x int($e->{comp_len} / 2));
			});
	}
}

# an uncompressed variable-length chunk: [null bitmap][u32 lengths][bytes]
sub text_chunk_edit
{
	my ($c, $col, $code) = @_;
	my $e = chunk_of($c, 0, 3, $col);
	die 'chunk is compressed' if $e->{codec} != 0;
	my $bm = $e->{nnulls} > 0 ? int((100 + 7) / 8) : 0;
	$code->($c, $e, $e->{off}, $bm, 100 - $e->{nnulls});
}

sub read_u32_at
{
	my ($c, $boff) = @_;
	return unpack('V', file_read($c->{path}, blob_file_off($c, 0, $boff), 4));
}

add('chunk data none/text: first length 0xFFFFFFFF', codec => 'none', must_error => ['full'],
	file => sub { text_chunk_edit($_[0], 2, sub { my ($c, $e, $o, $bm) = @_; blob_poke($c, 0, $o + $bm, pack('V', 0xFFFFFFFF)) }) });
add('chunk data none/text: first length +1 (sum mismatch)', codec => 'none', must_error => ['full'],
	file => sub { text_chunk_edit($_[0], 2, sub { my ($c, $e, $o, $bm) = @_; blob_poke($c, 0, $o + $bm, pack('V', read_u32_at($c, $o + $bm) + 1)) }) });
add('chunk data none/text: first length -1 (sum mismatch)', codec => 'none', must_error => ['full'],
	file => sub { text_chunk_edit($_[0], 2, sub { my ($c, $e, $o, $bm) = @_; blob_poke($c, 0, $o + $bm, pack('V', read_u32_at($c, $o + $bm) - 1)) }) });
add('chunk data none/text: first length 0, second 0xFFFFFFFF (sum overflows 32 bits)', codec => 'none', must_error => ['full'],
	file => sub { text_chunk_edit($_[0], 2, sub { my ($c, $e, $o, $bm) = @_; blob_poke($c, 0, $o + $bm, pack('VV', 0, 0xFFFFFFFF)) }) });
add('chunk data none/text: four lengths of 0x40000000 (the sum is 2^32)', codec => 'none', must_error => ['full'],
	file => sub { text_chunk_edit($_[0], 2, sub { my ($c, $e, $o, $bm) = @_; blob_poke($c, 0, $o + $bm, pack('V4', (0x40000000) x 4)) }) });
add('chunk data none/text: all lengths zero', codec => 'none', must_error => ['full'],
	file => sub { text_chunk_edit($_[0], 2, sub { my ($c, $e, $o, $bm, $nn) = @_; blob_poke($c, 0, $o + $bm, "\0" x (4 * $nn)) }) });
add('chunk data none/text: length array is random bytes', codec => 'none', must_error => ['full'],
	file => sub { text_chunk_edit($_[0], 2, sub { my ($c, $e, $o, $bm, $nn) = @_; blob_poke($c, 0, $o + $bm, join('', map { chr(int(rand(256))) } 1 .. 4 * $nn)) }) });
add('chunk data none/text: two lengths swapped (sum kept; readable but wrong is expected)', codec => 'none',
	file => sub { text_chunk_edit($_[0], 2, sub { my ($c, $e, $o, $bm) = @_;
		my @l = (read_u32_at($c, $o + $bm), read_u32_at($c, $o + $bm + 4));
		blob_poke($c, 0, $o + $bm, pack('V2', $l[1], $l[0])) if $l[0] != $l[1] }) });
for my $col (['text', 2], ['int4', 1])
{
	my ($cn, $ci) = @$col;
	add("chunk data none/$cn: null bitmap, one bit flipped", codec => 'none', must_error => ['full'],
		file => sub { text_chunk_edit($_[0], $ci, sub { my ($c, $e, $o) = @_;
			my $b = ord(file_read($c->{path}, blob_file_off($c, 0, $o + 3), 1));
			blob_poke($c, 0, $o + 3, chr($b ^ 4)) }) });
	add("chunk data none/$cn: null bitmap all ones", codec => 'none', must_error => ['full'],
		file => sub { text_chunk_edit($_[0], $ci, sub { my ($c, $e, $o, $bm) = @_; blob_poke($c, 0, $o, "\xFF" x $bm) }) });
	add("chunk data none/$cn: null bitmap all zeros", codec => 'none', must_error => ['full'],
		file => sub { text_chunk_edit($_[0], $ci, sub { my ($c, $e, $o, $bm) = @_; blob_poke($c, 0, $o, "\0" x $bm) }) });
}
add('chunk data none/id: one value byte changed (readable but wrong is expected)', codec => 'none', bat => 'short',
	file => sub { my $c = shift; my $e = chunk_of($c, 0, 3, 0); blob_poke($c, 0, $e->{off} + 17, "\xA5") });

# --- 6. the stripe catalog rows ---------------------------------------------
sub bytea_lit { return "'\\x" . unpack('H*', $_[0]) . "'::bytea"; }

sub stats_blob
{
	my @e = @_;
	return join '', map {
		my ($nn, $minb, $maxb, $fl, $minl, $maxl) = @$_;
		$minl //= length($minb);
		$maxl //= length($maxb);
		pack('V v v C a3', $nn, $minl, $maxl, $fl, "\0\0\0") . $minb . $maxb
	} @e;
}
my @PLAIN = ([ 0, '', '', 0 ]) x 5;

my @CAT = (
	[ 'first_block beyond the file',        sub { 'first_block = ' . ($_[0]{nblocks} + 100) }, 1 ],
	[ 'first_block 0 (the metapage)',       sub { 'first_block = 0' }, 1 ],
	[ 'first_block 2^31',                   sub { 'first_block = 2147483648' }, 1 ],
	[ 'first_block 2^32 - 1 (MaxBlockNumber)', sub { 'first_block = 4294967295' }, 1 ],
	[ 'first_block 2^32 - 2',               sub { 'first_block = 4294967294' }, 1 ],
	[ 'first_block 2^63 - 1',               sub { 'first_block = 9223372036854775807' }, 1 ],
	[ 'first_block -1',                     sub { 'first_block = -1' }, 1 ],
	[ 'first_block of the other stripe',    sub { 'first_block = ' . $_[0]{stripes}[1]{first_block} }, 1 ],
	[ 'first_block + 1',                    sub { 'first_block = ' . ($_[0]{stripes}[0]{first_block} + 1) }, 1 ],
	[ 'block_count 0',                      sub { 'block_count = 0' }, 1 ],
	[ 'block_count 1 (blob_len inconsistent)', sub { 'block_count = 1' }, 1 ],
	[ 'block_count 2^31 - 1',               sub { 'block_count = 2147483647' }, 1 ],
	[ 'block_count -1',                     sub { 'block_count = -1' }, 1 ],
	[ 'block_count +1',                     sub { 'block_count = ' . ($_[0]{stripes}[0]{block_count} + 1) }, 1 ],
	[ 'block_count -1 of the true value',   sub { 'block_count = ' . ($_[0]{stripes}[0]{block_count} - 1) }, 1 ],
	[ 'blob_len 0',                         sub { 'blob_len = 0' }, 1 ],
	[ 'blob_len 1',                         sub { 'blob_len = 1' }, 1 ],
	[ 'blob_len +1',                        sub { 'blob_len = ' . ($_[0]{stripes}[0]{blob_len} + 1) }, 1 ],
	[ 'blob_len -1',                        sub { 'blob_len = ' . ($_[0]{stripes}[0]{blob_len} - 1) }, 1 ],
	[ 'blob_len 2^62',                      sub { 'blob_len = 4611686018427387904' }, 1 ],
	[ 'blob_len -2^63',                     sub { 'blob_len = -9223372036854775808' }, 1 ],
	[ 'blob_len = the page capacity of the run exactly', sub { 'blob_len = ' . ($_[0]{stripes}[0]{block_count} * $PAYLOAD) }, 1 ],
	[ 'footer_offset 0',                    sub { 'footer_offset = 0' }, 1 ],
	[ 'footer_offset 63',                   sub { 'footer_offset = 63' }, 1 ],
	[ 'footer_offset 64',                   sub { 'footer_offset = 64' }, 1 ],
	[ 'footer_offset = blob_len',           sub { 'footer_offset = ' . $_[0]{stripes}[0]{blob_len} }, 1 ],
	[ 'footer_offset = blob_len - 1',       sub { 'footer_offset = ' . ($_[0]{stripes}[0]{blob_len} - 1) }, 1 ],
	[ 'footer_offset -1',                   sub { 'footer_offset = -1' }, 1 ],
	[ 'footer_offset 2^62',                 sub { 'footer_offset = 4611686018427387904' }, 1 ],
	[ 'row_count 0',                        sub { 'row_count = 0' }, 1 ],
	[ 'row_count 1',                        sub { 'row_count = 1' }, 1 ],
	[ 'row_count 999',                      sub { 'row_count = 999' }, 1 ],
	[ 'row_count 1001',                     sub { 'row_count = 1001' }, 1 ],
	[ 'row_count 2^31 - 1',                 sub { 'row_count = 2147483647' }, 1 ],
	[ 'row_count -1',                       sub { 'row_count = -1' }, 1 ],
	[ 'row_count 40000000',                 sub { 'row_count = 40000000' }, 1 ],
	[ 'row_count 40000001',                 sub { 'row_count = 40000001' }, 1 ],
	[ 'chunk_groups 0',                     sub { 'chunk_groups = 0' }, 1 ],
	[ 'chunk_groups 2^31 - 1',              sub { 'chunk_groups = 2147483647' }, 1 ],
	[ 'chunk_groups -1',                    sub { 'chunk_groups = -1' }, 1 ],
	[ 'chunk_groups 9',                     sub { 'chunk_groups = 9' }, 1 ],
	[ 'chunk_groups 11',                    sub { 'chunk_groups = 11' }, 1 ],
	[ 'natts 0',                            sub { 'natts = 0' }, 1 ],
	[ 'natts 1',                            sub { 'natts = 1' }, 1 ],
	[ 'natts 4',                            sub { 'natts = 4' }, 1 ],
	[ 'natts 6',                            sub { 'natts = 6' }, 1 ],
	[ 'natts 5000',                         sub { 'natts = 5000' }, 1 ],
	[ 'natts 32767',                        sub { 'natts = 32767' }, 1 ],
	[ 'natts -1',                           sub { 'natts = -1' }, 1 ],
	[ 'stripe_id 0',                        sub { 'stripe_id = 0' }, 1 ],
	[ 'stripe_id -1',                       sub { 'stripe_id = -1' }, 1 ],
	[ 'stripe_id 99999 (the pages say another id)', sub { 'stripe_id = 99999' }, 1 ],
	[ 'first_row -1',                       sub { 'first_row = -1' }, 0 ],
	[ 'first_row 2^62',                     sub { 'first_row = 4611686018427387904' }, 0 ],
	[ 'raw_bytes -1',                       sub { 'raw_bytes = -1' }, 0 ],
	[ 'raw_bytes 2^62',                     sub { 'raw_bytes = 4611686018427387904' }, 0 ],
	[ 'cid_points empty',                   sub { 'cid_points = ' . bytea_lit('') }, 0 ],
	[ 'cid_points 1 byte',                  sub { 'cid_points = ' . bytea_lit("\0") }, 1 ],
	[ 'cid_points 7 bytes',                 sub { 'cid_points = ' . bytea_lit("\0" x 7) }, 1 ],
	[ 'cid_points 17 bytes',                sub { 'cid_points = ' . bytea_lit("\x01" x 17) }, 1 ],
	[ 'cid_points 8 bytes of 0xFF',         sub { 'cid_points = ' . bytea_lit("\xFF" x 8) }, 0 ],
	[ 'cid_points 8 zero bytes',            sub { 'cid_points = ' . bytea_lit("\0" x 8) }, 0 ],
	[ 'cid_points 4096 bytes of 0xFF',      sub { 'cid_points = ' . bytea_lit("\xFF" x 4096) }, 0 ],
	[ 'col_stats empty',                    sub { 'col_stats = ' . bytea_lit('') }, 0 ],
	[ 'col_stats 6 bytes',                  sub { 'col_stats = ' . bytea_lit("\0" x 6) }, 0 ],
	[ 'col_stats 59 bytes (one byte short)', sub { 'col_stats = ' . bytea_lit("\0" x 59) }, 0 ],
	[ 'col_stats 4096 bytes of 0xFF',       sub { 'col_stats = ' . bytea_lit("\xFF" x 4096) }, 0 ],
	[ 'col_stats min_len 65',               sub { 'col_stats = ' . bytea_lit(stats_blob([ 0, '', '', 2, 65, 0 ], @PLAIN[1 .. 4])) }, 0 ],
	[ 'col_stats max_len 65',               sub { 'col_stats = ' . bytea_lit(stats_blob([ 0, '', '', 2, 0, 65 ], @PLAIN[1 .. 4])) }, 0 ],
	[ 'col_stats nnulls 0xFFFFFFFF',        sub { 'col_stats = ' . bytea_lit(stats_blob([ 0xFFFFFFFF, '', '', 0 ], @PLAIN[1 .. 4])) }, 0 ],
	[ 'col_stats flags 0xFF',               sub { 'col_stats = ' . bytea_lit(stats_blob([ 0, '', '', 0xFF ], @PLAIN[1 .. 4])) }, 0 ],
	[ 'col_stats lengths longer than the rest', sub { 'col_stats = ' . bytea_lit(stats_blob([ 0, "ab", "cd", 2, 60, 60 ], @PLAIN[1 .. 4])) }, 0 ],
	[ 'col_stats id minimum of 3 bytes with the min/max flag', sub { 'col_stats = ' . bytea_lit(stats_blob([ 0, "abc", "abc", 2 ], @PLAIN[1 .. 4])) }, 0 ],
	[ 'col_stats nnulls 0 everywhere, no min/max (lies in a valid shape)', sub { 'col_stats = ' . bytea_lit(stats_blob(@PLAIN)) }, 0 ],
	[ 'col_stats extra bytes after the last column', sub { 'col_stats = ' . bytea_lit(stats_blob(@PLAIN) . "\xFF" x 100) }, 0 ],
);
for my $r (@CAT)
{
	my ($what, $setf, $must) = @$r;
	add("catalog: $what", must_error => ($must ? ['full'] : []),
		sql => sub { sqlset($_[0], 0, $setf->($_[0])) });
}
add('catalog: both stripes damaged (first_block 0 and natts 0)', must_error => ['full'],
	sql => sub { sqlset($_[0], 0, 'first_block = 0') . ";\n" . sqlset($_[0], 1, 'natts = 0') });
add('catalog: stripe row deleted (rows vanish; not corruption)',
	sql => sub { sprintf('DELETE FROM kingres.columnar_stripe WHERE rel_uid = %d AND stripe_id = %d', $_[0]{filenode}, $_[0]{stripes}[0]{id}) });
add('catalog: all stripe rows deleted',
	sql => sub { sprintf('DELETE FROM kingres.columnar_stripe WHERE rel_uid = %d', $_[0]{filenode}) });
add('catalog: stripe row duplicated under a new stripe id (same pages)', must_error => ['full'],
	sql => sub { sprintf('INSERT INTO kingres.columnar_stripe SELECT rel_uid, 99999, first_row, row_count, chunk_groups, natts, first_block, block_count, blob_len, raw_bytes, footer_offset, cid_points, col_stats FROM kingres.columnar_stripe WHERE rel_uid = %d AND stripe_id = %d', $_[0]{filenode}, $_[0]{stripes}[0]{id}) });
# text statistics of the catalog row with a hostile pointer-style minimum
for my $p (['indirect', pack('CC', 0x01, 0x01) . ("\x41" x 8)],
	['expanded', pack('CC', 0x01, 0x02) . ("\x41" x 8)],
	['on-disk toast', pack('CC', 0x01, 0x12) . ("\x41" x 16)],
	['compressed header', pack('V', (12 << 2) | 2) . ("\x41" x 8)])
{
	my ($pn, $pb) = @$p;
	add("catalog: col_stats text minimum is a $pn varlena", bat => 'short',
		extra => ["SELECT count(*) FROM %T WHERE s = 'abc'", "SELECT count(*) FROM %T WHERE s > 'm'"],
		sql => sub {
			sqlset($_[0], 0, 'col_stats = ' . bytea_lit(stats_blob(
				@PLAIN[0 .. 1], [ 0, $pb, chr((4 << 1) | 1) . 'zzz', 2 ], @PLAIN[3 .. 4])))
		});
}

# --- 7. torn files -----------------------------------------------------------
sub truncate_to
{
	my ($c, $size) = @_;
	my $old = -s $c->{path};
	truncate($c->{path}, $size) or die "truncate: $!";
	push @{ $c->{edits} }, "file truncated from $old to $size bytes";
}
add('file: cut in half', must_error => ['count', 'full'],
	file => sub { truncate_to($_[0], int((-s $_[0]{path}) / 2 / $BLCKSZ) * $BLCKSZ) });
add('file: cut in half in the middle of a page', must_error => ['count', 'full'],
	file => sub { truncate_to($_[0], int((-s $_[0]{path}) / 2) + 123) });
add('file: cut to the metapage only', must_error => ['count', 'full'],
	file => sub { truncate_to($_[0], $BLCKSZ) });
add('file: cut to a partial metapage', must_error => \@MI,
	file => sub { truncate_to($_[0], 4000) });
add('file: cut to zero bytes', must_error => \@MI,
	file => sub { truncate_to($_[0], 0) });
add('file: last page cut off', must_error => ['full'],
	file => sub { truncate_to($_[0], (-s $_[0]{path}) - $BLCKSZ) });
add('file: last 100 bytes cut off', must_error => ['full'],
	file => sub { truncate_to($_[0], (-s $_[0]{path}) - 100) });
add('file: stripe 1 removed, stripe 0 intact', must_error => ['count', 'full'],
	file => sub { truncate_to($_[0], $_[0]{stripes}[1]{first_block} * $BLCKSZ) });
add('file: a zero-filled page in the middle of the second stripe', must_error => ['full'],
	file => sub { my $c = shift; my $s = $c->{stripes}[1]; poke($c, ($s->{first_block} + int($s->{block_count} / 2)) * $BLCKSZ, "\0" x $BLCKSZ) });
add('file: a zero-filled page in the middle of the first stripe', must_error => ['full'],
	file => sub { my $c = shift; my $s = $c->{stripes}[0]; poke($c, ($s->{first_block} + int($s->{block_count} / 2)) * $BLCKSZ, "\0" x $BLCKSZ) });
add('file: garbage appended after the last page (unreferenced pages are harmless)',
	file => sub { my $c = shift; poke($c, -s $c->{path}, join('', map { chr(int(rand(256))) } 1 .. 3 * $BLCKSZ)) });
add('file: stripe 0 and stripe 1 pages swapped (equal sizes only)', must_error => ['full'],
	file => sub {
		my $c = shift;
		my ($a, $b) = @{ $c->{stripes} };
		return unless $a->{block_count} == $b->{block_count};
		my $pa = file_read($c->{path}, $a->{first_block} * $BLCKSZ, $a->{block_count} * $BLCKSZ);
		my $pb = file_read($c->{path}, $b->{first_block} * $BLCKSZ, $b->{block_count} * $BLCKSZ);
		poke($c, $a->{first_block} * $BLCKSZ, $pb);
		poke($c, $b->{first_block} * $BLCKSZ, $pa);
	});

# ---------------------------------------------------------------------------
# The driver
# ---------------------------------------------------------------------------

sub log_since
{
	my ($n, $off) = @_;
	return PostgreSQL::Test::Utils::slurp_file($n->logfile, $off);
}

# Make sure the server answers.  The test clusters run with restart_after_crash
# off, so after a crash the postmaster is gone: start it again (crash
# recovery) so that the remaining scenarios can run.
sub wait_for_server
{
	my ($n) = @_;
	for my $attempt (1 .. 3)
	{
		for (1 .. 20)
		{
			my ($ret) = $n->psql('postgres', 'SELECT 1', stdout => \my $o, stderr => \my $e);
			return 1 if defined $ret && $ret == 0;
			select(undef, undef, undef, 0.25);
		}
		eval { $n->stop('immediate', fail_ok => 1); };
		eval { $n->start(fail_ok => 1); };
		limit_memory();
	}
	return 0;
}

# Run the statements of a scenario in one psql session.  psql sets the
# variables ERROR and SQLSTATE after every statement, which is how each
# outcome is read back.  A session that ends in the middle (a crash) or does
# not end (a hang) is reported against the statement that was running, and
# the rest of the battery continues in a new session.
sub run_battery
{
	my ($n, $s, $keys) = @_;
	my %res;
	my @todo = @$keys;
	my @problems;
	my $guard = 0;

	while (@todo && $guard++ < 40)
	{
		my $script = "\\set ON_ERROR_STOP 0\n\\set VERBOSITY verbose\n";
		for my $k (@todo)
		{
			my $sql = $s->{stmts}{$k};
			$script .= "\\echo \@\@B $k\n$sql;\n\\echo \@\@R $k :ERROR :SQLSTATE\n";
			$script .= "RESET kingres_columnar.enable_custom_scan;\nRESET kingres_columnar.enable_chunk_skipping;\n"
			  if $k =~ /^skip/;
		}
		my ($out, $err) = ('', '');
		my $timedout = 0;
		my $ret;
		eval {
			$ret = $n->psql('postgres', $script, stdout => \$out, stderr => \$err,
				on_error_stop => 0, timeout => 240);
			1;
		} or $timedout = 1;
		my %done;
		for my $line (split /\n/, $out)
		{
			if ($line =~ /\@\@R (\S+) (\S+)(?: (\S+))?/)
			{
				$res{$1} = { err => ($2 eq q(true)), state => ($3 // q()), msg => q() };
				$done{$1} = 1;
			}
		}
		# the messages of the statements that failed, in order
		my @msgs = ($err =~ /^psql:<stdin>:\d+: (?:ERROR|FATAL|PANIC):\s+(\w{5}): (.*)$/mg);
		my @errkeys = grep { $done{$_} && $res{$_}{err} } @todo;
		for my $i (0 .. $#errkeys)
		{
			$res{ $errkeys[$i] }{state} = $msgs[2 * $i] // q(?);
			$res{ $errkeys[$i] }{msg} = $msgs[2 * $i + 1] // q();
		}

		my @rest = grep { !$done{$_} } @todo;
		last unless @rest;
		my $victim = $rest[0];
		my $why = $timedout ? 'timed out (hang?)'
		  : 'session lost (psql exit ' . ($ret // 'undef') . '): ' . substr($err, -300)
		  . ' | stdout tail: ' . substr($out, -200);
		push @problems, "$victim: BAD $why";
		$res{$victim} = { err => 1, state => 'LOST', msg => $why };
		if ($timedout)
		{
			$n->psql('postgres',
				"SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE pid <> pg_backend_pid() AND datname = 'postgres'");
		}
		wait_for_server($n);
		shift @rest;
		@todo = @rest;
	}
	return (\%res, \@problems);
}

# create the victim tables of the scenarios and read their layout
sub build_victims
{
	my ($n, $list) = @_;
	my $ddl = '';
	for my $s (@$list)
	{
		my $t = $s->{table};
		$ddl .= "CREATE TABLE $t (id bigint, i int, s text COLLATE \"C\", f float8, b boolean) USING columnar;\n";
		$ddl .= "SELECT kingres.columnar_set_options('$t', compression => '$s->{codec}', "
		  . "stripe_row_count => 1000, chunk_row_count => 100);\n";
		$ddl .= "INSERT INTO $t SELECT g, CASE WHEN g % 10 = 0 THEN NULL ELSE g END, "
		  . "CASE WHEN g % 7 = 0 THEN NULL ELSE substr(md5(g::text) || md5((g + 1)::text), 1, 8 + g % 24) END, "
		  . "g * 1.5, g % 2 = 0 FROM generate_series(1, $ROWS) g;\n";
	}
	$n->safe_psql('postgres', $ddl);
	$n->safe_psql('postgres', 'CHECKPOINT');

	my %by = map { $_->{table} => $_ } @$list;
	my $rows = $n->safe_psql('postgres', <<'EOS');
SELECT c.relname, pg_relation_filepath(c.oid), kingres.columnar_identity(c.oid),
       pg_relation_size(c.oid) / 8192,
       s.stripe_id, s.first_block, s.block_count, s.blob_len, s.footer_offset,
       s.row_count, s.chunk_groups, s.natts
  FROM pg_class c JOIN kingres.columnar_stripe s ON s.rel_uid = kingres.columnar_identity(c.oid)
 WHERE c.relname ~ '^v[0-9]{3}$'
 ORDER BY c.relname, s.stripe_id
EOS
	for my $line (split /\n/, $rows)
	{
		my ($rn, $fp, $fnode, $nb, $id, $fb, $bc, $bl, $fo, $rc, $cg, $na) = split /\|/, $line;
		my $s = $by{$rn} or next;
		$s->{path}     = $n->data_dir . '/' . $fp;
		$s->{filenode} = $fnode;
		$s->{nblocks}  = $nb;
		$s->{edits}    //= [];
		push @{ $s->{stripes} }, {
			id => $id, first_block => $fb, block_count => $bc, blob_len => $bl,
			footer_offset => $fo, row_count => $rc, chunk_groups => $cg, natts => $na,
		};
	}
	for my $s (@$list)
	{
		die "victim $s->{table} has " . scalar(@{ $s->{stripes} // [] }) . ' stripes, expected 2'
		  unless @{ $s->{stripes} // [] } == 2;
		$s->{stmts} = {};
		for my $b (@BATTERY)
		{
			(my $sql = $b->[1]) =~ s/%T/$s->{table}/g;
			$s->{stmts}{ $b->[0] } = $sql;
		}
		$s->{stmts}{drop} = "DROP TABLE $s->{table}";
		my $i = 0;
		for my $x (@{ $s->{extra} // [] })
		{
			(my $sql = $x) =~ s/%T/$s->{table}/g;
			$s->{stmts}{ 'extra' . $i++ } = $sql;
		}
	}
}

# build the victims, stop the server, damage the files, start it, damage the
# catalog rows, then run the battery against each victim
sub run_scenarios
{
	my ($n, $list, $label) = @_;

	build_victims($n, $list);

	# a reference columnar table, and the digests of it and of a heap table
	$n->safe_psql('postgres', <<'EOS');
DROP TABLE IF EXISTS good;
CREATE TABLE good (id bigint, i int, s text, f float8, b boolean) USING columnar;
SELECT kingres.columnar_set_options('good', compression => 'lz4', stripe_row_count => 1000, chunk_row_count => 100);
INSERT INTO good SELECT g, g, md5(g::text), g, true FROM generate_series(1, 3000) g;
EOS
	my $digest_sql = "SELECT md5(string_agg(x::text, '' ORDER BY id)) FROM %s x";
	my $good_digest = $n->safe_psql('postgres', sprintf($digest_sql, 'good'));
	my $heal_digest = $n->safe_psql('postgres', sprintf($digest_sql, 'healthy'));

	$n->stop;
	for my $s (@$list)
	{
		next unless $s->{file};
		eval { $s->{file}->($s); 1 } or $s->{setup_error} = "file edit failed: $@";
	}
	$n->start;
	limit_memory();

	for my $s (@$list)
	{
		next unless $s->{sql};
		my $sql = $s->{sql}->($s);
		my ($ret) = $n->psql('postgres', $sql, stdout => \my $so, stderr => \my $se);
		$s->{setup_error} = "catalog edit failed: $se" if $ret != 0;
		push @{ $s->{edits} }, "catalog: $sql";
	}

	for my $s (@$list)
	{
		my $logoff = -s $n->logfile;
		my @problems;
		push @problems, $s->{setup_error} if $s->{setup_error};

		my @keys = map { $_->[0] } @BATTERY;
		@keys = grep { $SHORT{$_} } @keys if $s->{bat} eq q(short);
		# columnar_check() looks at every columnar table of the database, so it
		# fails on the tables of the other scenarios that are still damaged
		@keys = grep { $_ ne q(check) } @keys if $s->{control};
		push @keys, grep { /^extra/ } sort keys %{ $s->{stmts} };
		push @keys, 'drop';

		my ($res, $pr) = run_battery($n, $s, \@keys);
		push @problems, @$pr;

		my %okstate = (%CLEAN, map { $_ => 1 } @{ $s->{ok_states} // [] });
		for my $k (@keys)
		{
			my $r = $res->{$k} or next;
			next if $r->{state} eq 'LOST';
			push @problems, "$k: BAD sqlstate $r->{state}: $r->{msg}"
			  if $r->{err} && !$okstate{ $r->{state} };
			push @problems, "$k: an undamaged table failed: $r->{state} $r->{msg}"
			  if $s->{control} && $r->{err};
		}
		for my $k (@{ $s->{must_error} // [] })
		{
			my $r = $res->{$k};
			push @problems, "$k: the damage was NOT detected (the statement returned a result)"
			  if !$r || !$r->{err};
		}

		# other tables keep working, the server is up
		my $healthy_ok = eval {
			wait_for_server($n)
			  && $n->safe_psql('postgres', sprintf($digest_sql, 'healthy')) eq $heal_digest
			  && $n->safe_psql('postgres', sprintf($digest_sql, 'good')) eq $good_digest;
		};
		push @problems, 'the healthy tables no longer read correctly after this scenario'
		  unless $healthy_ok;
		my $log = log_since($n, $logoff);
		push @problems, "CRASH in the server log: $1"
		  if $log =~ /(TRAP:[^\n]*|terminated by signal[^\n]*|server process \(PID \d+\) was terminated[^\n]*)/;

		diag(join("\n", "  [$s->{name}]", (map { "  edit: $_" } @{ $s->{edits} }),
			'  results: ' . join(', ', map { "$_=" . ($res->{$_}{err} ? $res->{$_}{state} : 'ok') } grep { $res->{$_} } @keys),
			map { "  message($_): $res->{$_}{msg}" } grep { $res->{$_} && $res->{$_}{err} } @keys))
		  if $ENV{KC_CORRUPT_VERBOSE};
		ok(!@problems, "$label: $s->{name}")
		  or diag(join("\n",
			"  table $s->{table}, relation file $s->{path}",
			(map { "  edit: $_" } @{ $s->{edits} }),
			(map { "  problem: $_" } @problems),
			'  results: ' . join(', ', map { "$_=" . ($res->{$_}{err} ? $res->{$_}{state} : 'ok') } grep { $res->{$_} } @keys)));
	}
	return;
}

run_scenarios($node, \@scen, 'plain');

# ---------------------------------------------------------------------------
# The catalog table itself: damage a page of kingres.columnar_stripe or of its
# index.  Every columnar read of the database then fails, cleanly (that is the
# catalog of all tables); heap tables keep working.  The file is restored
# afterwards.
# ---------------------------------------------------------------------------
sub poke_plain
{
	my ($path, $off, $bytes) = @_;
	file_write($path, $off, $bytes);
}

sub catalog_damage_test
{
	my ($n, $label, $what, $relname, $code, $expect_error) = @_;
	require File::Copy;
	my $path = $n->data_dir . '/' . $n->safe_psql('postgres', "SELECT pg_relation_filepath('$relname')");
	$n->safe_psql('postgres',
		'CREATE TABLE IF NOT EXISTS good2 (id int, v text) USING columnar;'
		  . 'INSERT INTO good2 SELECT g, md5(g::text) FROM generate_series(1, 100) g'
		  . ' WHERE NOT EXISTS (SELECT 1 FROM good2)');
	$n->stop;
	File::Copy::copy($path, "$path.keep") or die "copy: $!";
	my @edits = $code->($path);
	$n->start;
	limit_memory();
	my $logoff = -s $n->logfile;
	my @problems;
	my @seen;
	for my $sql ('SELECT count(*) FROM good2', "SELECT md5(string_agg(x::text, '')) FROM good2 x",
		'COPY good2 TO STDOUT', 'ANALYZE good2', 'VACUUM good2', 'VACUUM FULL good2',
		'SELECT count(*) FROM kingres.columnar_check()', "SELECT * FROM kingres.columnar_info('good2')",
		'SELECT count(*) FROM good2 WHERE id < 5', "INSERT INTO good2 VALUES (1, 'x')")
	{
		my ($ret, $o, $e) = (undef, '', '');
		my $timedout = 0;
		eval {
			$ret = $n->psql('postgres', "\\set VERBOSITY sqlstate\n$sql",
				stdout => \$o, stderr => \$e, timeout => 120);
			1;
		} or $timedout = 1;
		if ($timedout) { push @problems, "$sql: timed out"; next; }
		if ($ret != 0)
		{
			my ($state) = ($e =~ /ERROR:\s+(\w{5})/);
			push @seen, ($state // 'LOST');
			push @problems, "$sql: " . ($state ? "BAD sqlstate $state" : "connection lost: $e")
			  unless $state && $CLEAN{$state};
			wait_for_server($n) unless $state;
		}
		else
		{
			push @seen, 'ok';
		}
	}
	push @problems, 'no statement failed: the damage was not detected'
	  if $expect_error && !grep { $_ ne 'ok' } @seen;
	my $heap_ok = eval { $n->safe_psql('postgres', 'SELECT count(*) FROM healthy') == $healthy_rows };
	push @problems, 'the heap table is unreadable' unless $heap_ok;
	my $log = log_since($n, $logoff);
	push @problems, "CRASH in the server log: $1"
	  if $log =~ /(TRAP:[^\n]*|terminated by signal[^\n]*)/;
	ok(!@problems, "$label: $what")
	  or diag(join("\n", (map { "  edit: $_" } @edits), (map { "  problem: $_" } @problems), '  results: ' . join(',', @seen)));
	$n->stop;
	File::Copy::move("$path.keep", $path) or die "restore: $!";
	$n->start;
	limit_memory();
}

# ---------------------------------------------------------------------------
# With data checksums on
# ---------------------------------------------------------------------------
my $node_ck = PostgreSQL::Test::Cluster->new('corrupt_ck');
$node_ck->init(extra => ['--data-checksums']);
$node_ck->append_conf('postgresql.conf', <<'EOC');
shared_preload_libraries = 'kingres_columnar'
autovacuum = off
fsync = off
EOC
$node_ck->start;
$node_ck->safe_psql('postgres', 'CREATE EXTENSION kingres_columnar');
$node_ck->safe_psql('postgres',
	'CREATE TABLE healthy (id int, v text) USING heap;'
	  . "INSERT INTO healthy SELECT g, md5(g::text) FROM generate_series(1, $healthy_rows) g;");
is($node_ck->safe_psql('postgres', 'SHOW data_checksums'), 'on',
	'data checksums are on in the second cluster');
limit_memory_for($node_ck);

# With checksums, a flipped byte of any page that is read is a checksum
# failure, including bytes the format itself never looks at (the writer_xid of
# the special area, the prune_xid of the page header, the unused part of the
# metapage).
my @ck;
sub addck
{
	my ($name, %o) = @_;
	$o{name}  = $name;
	$o{codec} //= 'lz4';
	$o{bat}   //= 'full';
	$o{table} = sprintf('v%03d', scalar(@ck) + 1);
	push @ck, \%o;
}
for my $p (0 .. 5)
{
	addck("a random byte flipped in page $p of stripe 0", must_error => ['full'],
		file => sub {
			my $c = shift;
			my $s = $c->{stripes}[0];
			my $pg = $p >= $s->{block_count} ? $s->{block_count} - 1 : $p;
			my $off = ($s->{first_block} + $pg) * $BLCKSZ + int(rand($BLCKSZ));
			my $old = ord(file_read($c->{path}, $off, 1));
			poke($c, $off, chr($old ^ (1 << int(rand(8)))));
		});
}
addck('the writer_xid of a special area flipped (the format never looks at it)', must_error => ['full'],
	file => sub { my $c = shift; my $s = $c->{stripes}[0]; poke($c, ($s->{first_block} + 1) * $BLCKSZ + $SPECOFF + 12, "\x77") });
addck('prune_xid of a stripe page flipped', must_error => ['full'],
	file => sub { my $c = shift; my $s = $c->{stripes}[0]; poke($c, ($s->{first_block} + 1) * $BLCKSZ + 20, "\x77") });
addck('unused reserved area of the metapage flipped', must_error => \@MI,
	file => sub { poke($_[0], $META + 100, "\x77") });
addck('metapage next_row flipped', must_error => \@MI,
	file => sub { poke($_[0], $META + 24, "\x77") });
addck('the checksum field of a page zeroed', must_error => ['full'],
	file => sub { my $c = shift; my $s = $c->{stripes}[0]; poke($c, ($s->{first_block} + 1) * $BLCKSZ + 8, "\0\0") });
addck('a page replaced by the page of another block (the checksum covers the block number)', must_error => ['full'],
	file => sub { my $c = shift; my $s = $c->{stripes}[0];
		my $pg = file_read($c->{path}, ($s->{first_block} + 2) * $BLCKSZ, $BLCKSZ);
		poke($c, ($s->{first_block} + 1) * $BLCKSZ, $pg) });
addck('a crafted hostile footer with a correct footer CRC but a stale page checksum', must_error => ['full'],
	file => sub { footer_edit($_[0], 0, 1, sub { fput($_[0], $_[1][1]{cols}[2]{pos} + 12, 'V', 0xFFFFFFFF) }) });
addck('a page zero-filled in the middle of a stripe (all zeros is a valid new page)', must_error => ['full'],
	file => sub { my $c = shift; my $s = $c->{stripes}[0]; poke($c, ($s->{first_block} + 1) * $BLCKSZ, "\0" x $BLCKSZ) });
addck('control: undamaged table', control => 1);

# the postmaster of the second cluster gets the same memory cap
sub limit_memory_for
{
	my ($n) = @_;
	return unless $have_prlimit && defined $n->{_pid};
	system('prlimit', '--as=' . (3 * 1024 * 1024 * 1024), "--pid=$n->{_pid}");
}

{
	# run_scenarios caps the memory of the global $node only; point the
	# helper at the second cluster for this call
	no warnings 'redefine';
	local *limit_memory = sub { limit_memory_for($node_ck) };
	run_scenarios($node_ck, \@ck, 'checksums');

	catalog_damage_test($node_ck, 'checksums', 'a byte of the catalog heap page flipped',
		'kingres.columnar_stripe',
		sub { my $p = shift; poke_plain($p, 8192 - 100, "\x5A"); ("catalog heap file $p offset " . (8192 - 100) . ' -> 5a') }, 1);
	catalog_damage_test($node_ck, 'checksums', 'a byte of the catalog index page flipped',
		'kingres.columnar_stripe_pkey',
		sub { my $p = shift; poke_plain($p, 8192 + 5000, "\x5A"); ("catalog index file $p offset " . (8192 + 5000) . ' -> 5a') }, 1);
}
catalog_damage_test($node, 'plain', 'the page header of the catalog heap page made insane (pd_lower 0xFFFF)',
	'kingres.columnar_stripe',
	sub { my $p = shift; poke_plain($p, 12, "\xFF\xFF"); ("catalog heap file $p offset 12 -> ffff") }, 1);
catalog_damage_test($node, 'plain', 'the catalog heap page zero-filled (an empty catalog is not an error)',
	'kingres.columnar_stripe',
	sub { my $p = shift; poke_plain($p, 0, "\0" x 8192); ("catalog heap file $p page 0 zeroed") }, 0);

$node->stop;
$node_ck->stop;
done_testing();
