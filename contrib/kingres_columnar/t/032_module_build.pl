# contrib/kingres_columnar/t/032_module_build.pl
#
# C8-04.  What a test file does when a test-only module (t/kcevents, t/kcprobe)
# cannot be built:
#
#   - a machine without a C compiler: the file is skipped, with a message that names
#     the module (the skip is the machine's, and the message says so);
#   - a compiler that runs and a build that fails (a header that is missing, a compile
#     error, a pg_config that cannot say where the headers are): the file FAILS, with
#     the compiler's output.  A test that skipped itself on a build error would let a
#     broken module hide behind a pass, which is what the earlier version of
#     t/KcProbe.pm did ("Result: NOTESTS").
#
# The test builds the module against a pg_config that is made to lie in each way, and
# runs the real helper (build_module) in a child perl for the two outcomes of the
# wrapper: a skip and a failure.  The in-tree (meson) build compiles both modules with
# the extension (meson.build), so a tree that cannot build them fails the build.
#
# Portions Copyright (c) 2026, RooyeKhat Media Co
# SPDX-License-Identifier: AGPL-3.0-only

use strict;
use warnings FATAL => 'all';

use FindBin;
use lib $FindBin::RealBin;
use KcProbe ();
use PostgreSQL::Test::Utils;
use Test::More;

plan skip_all => 'needs a POSIX shell for the pg_config wrapper' if $PostgreSQL::Test::Utils::windows_os;

my $real = $ENV{PG_CONFIG} || 'pg_config';
my $realinc = `$real --includedir-server 2>/dev/null`;
chomp $realinc;
my $realcc = `$real --cc 2>/dev/null`;
chomp $realcc;
$realcc = 'cc' if $realcc eq '';

# a pg_config that answers --cc and --includedir-server as it is told and anything else as the real one
sub wrapper
{
	my ($name, $cc, $inc) = @_;
	my $d = PostgreSQL::Test::Utils::tempdir();
	my $path = "$d/pg_config_$name";
	open(my $fh, '>', $path) or die "cannot write $path: $!";
	print $fh "#!/bin/sh\n";
	print $fh "case \"\$1\" in\n";
	print $fh "  --cc) echo '$cc' ;;\n";
	print $fh "  --includedir-server) echo '$inc' ;;\n";
	print $fh "  *) exec $real \"\$@\" ;;\n";
	print $fh "esac\n";
	close $fh;
	chmod 0755, $path;
	return $path;
}

my $empty = PostgreSQL::Test::Utils::tempdir();
my $nocc = wrapper('nocc', '/nonexistent/compiler-for-the-test', $realinc);
my $noinc = wrapper('noinc', $realcc, '');
my $badinc = wrapper('badinc', $realcc, $empty);

# --- the helper itself
{
	local $ENV{PG_CONFIG} = $real;
	my ($st, $what) = KcProbe::_build('kcevents');
	is($st, 'ok', 'the module builds against the installation under test');
	ok(-f "$what/kcevents.so", '... and the library is there');
	($st, $what) = KcProbe::_build('kcprobe');
	is($st, 'ok', 'the probe module builds too');
}
{
	local $ENV{PG_CONFIG} = $nocc;
	my ($st, $what) = KcProbe::_build('kcevents');
	is($st, 'nocompiler', 'no compiler on the machine: the outcome is "no compiler"');
	like($what, qr/test module kcevents .* no C compiler/, '... and the message names the module and says why');
}
{
	local $ENV{PG_CONFIG} = $badinc;
	my ($st, $what) = KcProbe::_build('kcevents');
	is($st, 'failed', 'a compiler that runs and headers that are not there: the outcome is "failed", not a skip');
	like($what, qr/building the test module kcevents failed/, '... with the compiler output');
}
{
	local $ENV{PG_CONFIG} = $noinc;
	my ($st, $what) = KcProbe::_build('kcprobe');
	is($st, 'failed', 'a pg_config that does not say where the headers are: "failed"');
	like($what, qr/server headers were not found/, '... and says so');
}

# --- the helper as the test files use it, in a child perl: a skip and a failure
sub child
{
	my ($pgc) = @_;
	my $out = '';
	{
		local $ENV{PG_CONFIG} = $pgc;
		# the child gets the library path of this process: a run under meson passes it with -I, not in PERL5LIB
		my $inc = join(' ', map { "-I\"$_\"" } @INC);
		$out = `cd "$FindBin::RealBin" && "$^X" $inc -I. -MKcProbe=build_module -MTest::More -e 'my \$d = build_module(q(kcevents)); plan(tests => 1); ok(1, q(built));' 2>&1`;
	}
	return ($? >> 8, $out);
}
{
	my ($rc, $out) = child($nocc);
	is($rc, 0, 'a file without a compiler ends cleanly');
	like($out, qr/^1\.\.0 # SKIP .*test module kcevents .*no C compiler/m, '... as a skip that names the module');
}
{
	my ($rc, $out) = child($badinc);
	isnt($rc, 0, 'a file whose module does not build FAILS (a non-zero exit)');
	like($out, qr/building the test module kcevents failed/, '... with the compiler output');
	unlike($out, qr/SKIP/, '... and is not a skip');
}
{
	my ($rc, $out) = child($real);
	is($rc, 0, 'a file whose module builds runs');
	like($out, qr/^ok 1 - built/m, '... its tests');
}

done_testing();
