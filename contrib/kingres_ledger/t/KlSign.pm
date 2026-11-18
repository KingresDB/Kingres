# SPDX-License-Identifier: AGPL-3.0-only
#
# contrib/kingres_ledger/t/KlSign.pm
#
# Helpers of the TAP tests of client signatures (src/sign.c): an Ed25519 key
# pair and a signature made with the openssl command (a client of the ledger
# holds its own key and the server never sees it), and an independent encoding
# of the message a client signs.  The encoding is written here and not read from
# the server, so that a test that compares the two compares two implementations.
#
# Portions Copyright (c) 2026, RooyeKhat Media Co

package KlSign;

use strict;
use warnings FATAL => 'all';

use Digest::SHA qw(sha256);
use Exporter 'import';

our @EXPORT_OK = qw(have_openssl new_key sign_message message_bytes
  hex_of unhex_of key_id_of random_nonce);

# True when the openssl command can make and use Ed25519 signatures.
sub have_openssl
{
	my $out = `openssl version 2>&1`;
	return 0 if $? != 0;
	my ($major) = $out =~ /OpenSSL (\d+)\./;
	return defined $major && $major >= 3;
}

sub hex_of
{
	my ($bytes) = @_;
	return unpack('H*', $bytes);
}

sub unhex_of
{
	my ($hex) = @_;
	return pack('H*', $hex);
}

sub slurp_bin
{
	my ($file) = @_;
	open(my $fh, '<:raw', $file) or die "cannot read $file: $!";
	local $/;
	my $s = <$fh>;
	close($fh);
	return $s;
}

sub spit_bin
{
	my ($file, $bytes) = @_;
	open(my $fh, '>:raw', $file) or die "cannot write $file: $!";
	print $fh $bytes;
	close($fh);
}

# A new key pair in $dir: { pem => file, pub => 32 raw bytes, key_id => 16 raw bytes }.
sub new_key
{
	my ($dir, $name) = @_;
	my $pem = "$dir/$name.pem";
	system('openssl', 'genpkey', '-algorithm', 'ed25519', '-out', $pem) == 0
	  or die "openssl genpkey failed";
	system("openssl pkey -in '$pem' -pubout -outform DER -out '$dir/$name.pub.der'") == 0
	  or die "openssl pkey failed";
	my $der = slurp_bin("$dir/$name.pub.der");
	my $pub = substr($der, -32);
	return { pem => $pem, pub => $pub, key_id => key_id_of($pub), dir => $dir, name => $name };
}

# The identifier of a key: the first 16 bytes of the SHA-256 of the public key.
sub key_id_of
{
	my ($pub) = @_;
	return substr(sha256($pub), 0, 16);
}

# 16 random bytes.
sub random_nonce
{
	open(my $fh, '<:raw', '/dev/urandom') or die "no /dev/urandom";
	read($fh, my $n, 16);
	close($fh);
	return $n;
}

# The message a client signs: tag, a zero byte, chain id, u32 table id,
# the operation byte, row hash, key id and nonce.
sub message_bytes
{
	my (%a) = @_;
	return "kingres-ledger/client-signature/v1\0"
	  . $a{chain_id}
	  . pack('N', $a{table_id})
	  . $a{op}
	  . $a{row_hash}
	  . $a{key_id}
	  . $a{nonce};
}

# Ed25519 signature (64 raw bytes) of a message with a key made by new_key.
sub sign_message
{
	my ($key, $msg) = @_;
	my $base = "$key->{dir}/$key->{name}.msg";
	spit_bin("$base.bin", $msg);
	unlink("$base.sig");
	system('openssl', 'pkeyutl', '-sign', '-inkey', $key->{pem}, '-rawin',
		'-in', "$base.bin", '-out', "$base.sig") == 0
	  or die "openssl pkeyutl failed";
	my $sig = slurp_bin("$base.sig");
	die "signature of " . length($sig) . " bytes" unless length($sig) == 64;
	return $sig;
}

1;
