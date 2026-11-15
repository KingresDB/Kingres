# contrib/kingres_columnar/t/008_array_validation.pl
#
# Damage inside an array value of a chunk.
#
# An array is read from disk as bytes and handed to the array code, which
# trusts its header.  Two protections are tested, with data checksums off so
# that only the extension's own checks are in play:
#
#   * accidental damage (a flipped byte): the chunk's own checksum catches it
#     before the chunk is decoded, SQLSTATE XX001;
#   * a file written on purpose with valid chunk and footer checksums but a
#     hostile array header (a huge data offset, too many dimensions, an
#     element type that is not the column's, a length that runs past the
#     value): the structural check of the array rejects it with XX001
#     instead of letting array_out read outside the value.
#
# What is and is not validated is stated in the README.
#
# Portions Copyright (c) 2026, RooyeKhat Media Co
# SPDX-License-Identifier: AGPL-3.0-only

use strict;
use warnings FATAL => 'all';

use Fcntl qw(O_RDONLY O_RDWR SEEK_SET);
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;

my $BLCKSZ  = 8192;
my $PGHDR   = 24;
my $PAYLOAD = $BLCKSZ - $PGHDR - 24;

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
	$crc = $CRC_TABLE[($crc ^ $_) & 0xFF] ^ ($crc >> 8) for unpack('C*', $data);
	return ($crc ^ 0xFFFFFFFF) & 0xFFFFFFFF;
}

sub file_read
{
	my ($path, $off, $len) = @_;
	sysopen(my $fh, $path, O_RDONLY) or die "open $path: $!";
	sysseek($fh, $off, SEEK_SET) or die "seek: $!";
	my $buf = '';
	sysread($fh, $buf, $len) // die "read: $!";
	close($fh);
	return $buf;
}

sub file_write
{
	my ($path, $off, $data) = @_;
	sysopen(my $fh, $path, O_RDWR) or die "open $path: $!";
	sysseek($fh, $off, SEEK_SET) or die "seek: $!";
	syswrite($fh, $data) == length($data) or die "write: $!";
	close($fh);
}

my $node = PostgreSQL::Test::Cluster->new('arrval');
$node->init(extra => ['--no-data-checksums']);
$node->append_conf('postgresql.conf', q{
shared_preload_libraries = 'kingres_columnar'
autovacuum = off
fsync = off
});
$node->start;
$node->safe_psql('postgres', 'CREATE EXTENSION kingres_columnar');

# One table per scenario; codec none keeps the array bytes readable in the
# file.  Two columns: an int[] and a text[].
my @cases = (
	[ 'accidental flip of a data offset byte (no checksum fixed)', 0, 'flip',  4, "\xf0\xff\xff\x7f" ],
	[ 'accidental flip of an element byte (no checksum fixed)',    0, 'flip', 24, "\x55" ],
	[ 'crafted: data offset 0x7ffffff0',                           1, 'ok',    4, "\xf0\xff\xff\x7f" ],
	[ 'crafted: 100 dimensions',                                   1, 'ok',    0, pack('V', 100) ],
	[ 'crafted: negative dimension',                               1, 'ok',   12, pack('V', 0xFFFFFFF0) ],
	[ 'crafted: element type of another type',                     1, 'ok',    8, pack('V', 25) ],
	[ 'crafted: dimension larger than the value',                  1, 'ok',   12, pack('V', 100000) ],
	[ 'crafted: data offset set with no bitmap room',              1, 'ok',    4, pack('V', 20) ],
);

my $n = 0;
for my $case (@cases)
{
	my ($name, $crafted, $kind, $at, $new) = @$case;
	my $t = sprintf('ta%02d', ++$n);

	$node->safe_psql('postgres', "
CREATE TABLE $t (id int, arr int[]) USING columnar;
SELECT kingres.columnar_set_options('$t', 'none');
INSERT INTO $t VALUES (1, '{1,2,3}'), (2, '{4,5,6}');
CHECKPOINT;");
	my ($path, $first_block, $blob_len, $foff, $ngroups) = split /\|/, $node->safe_psql('postgres',
		"SELECT pg_relation_filepath('$t'), s.first_block, s.blob_len, s.footer_offset, s.chunk_groups
		   FROM kingres.columnar_stripe s WHERE s.rel_uid = kingres.columnar_identity('$t')");
	$path = $node->data_dir . '/' . $path;
	$node->stop;

	# the blob of the single stripe (it fits in one or two pages)
	my $blob = '';
	for my $j (0 .. int(($blob_len + $PAYLOAD - 1) / $PAYLOAD) - 1)
	{
		$blob .= file_read($path, ($first_block + $j) * $BLCKSZ + $PGHDR, $PAYLOAD);
	}
	$blob = substr($blob, 0, $blob_len);

	# find the array of the first row: ndim 1, dataoffset 0, elemtype 23, dim 3, lbound 1
	my $pat = pack('V V V V V V V V', 1, 0, 23, 3, 1, 1, 2, 3);
	my $at_blob = index($blob, $pat);
	die "array not found in the stripe of $t" if $at_blob < 0;

	substr($blob, $at_blob + $at, length($new)) = $new;

	if ($crafted)
	{
		# fix the chunk checksum and the footer checksum: a file written on
		# purpose.  Footer: per group a row count, then per column a 40-byte
		# entry (off 0, comp_len 8, raw 12, nnulls 16, crc 20, ...) and the
		# minimum and maximum bytes; column 1 is the array column.
		my $f = substr($blob, $foff);
		my $pos = 4;
		my ($ml0, $xl0) = unpack('v v', substr($f, $pos + 32, 4));
		$pos += 40 + $ml0 + $xl0;
		my ($coff, $clen) = unpack('Q< V', substr($f, $pos, 12));
		substr($f, $pos + 20, 4) = pack('V', crc32c(substr($blob, $coff, $clen)));
		substr($blob, $foff) = $f;
		substr($blob, 36, 4) = pack('V', crc32c($f));
	}

	for my $j (0 .. int(($blob_len + $PAYLOAD - 1) / $PAYLOAD) - 1)
	{
		my $chunk = substr($blob, $j * $PAYLOAD, $PAYLOAD);
		file_write($path, ($first_block + $j) * $BLCKSZ + $PGHDR, $chunk);
	}
	$node->start;

	for my $q ("SELECT * FROM $t", "SELECT count(*), sum(array_length(arr, 1)) FROM $t",
		"COPY $t TO STDOUT", "SELECT md5(string_agg(x::text, '')) FROM $t x")
	{
		my ($ret, $out, $err) = $node->psql('postgres', $q);
		like($err, qr/ERROR:  columnar table "$t" is corrupt/, "$name: a corruption error, not a result: $q");
		is($node->safe_psql("postgres", "SELECT 1"), "1", "$name: the server is alive after: $q");
	}
	my ($ret, $out, $err) = $node->psql('postgres', "SELECT arr FROM $t LIMIT 1",
		extra_params => ['-v', 'VERBOSITY=sqlstate']);
	like($err, qr/XX001/, "$name: SQLSTATE XX001");
	$node->safe_psql('postgres', "DROP TABLE $t");
}

$node->stop;
done_testing();
