# contrib/kingres_columnar/t/KcProbe.pm
#
# Builds one of the test-only modules under t/ (kcprobe, kcevents) with the
# compiler and the headers of the installation the test runs against, the way
# the modules under src/test/modules are built for their tests: they are never
# installed and never part of a release.
#
# What a test that needs a module does when it cannot get one:
#
#   - no C compiler on the machine: the test file is skipped, with a message
#     that names the module (the skip is the machine's, not the code's);
#   - a compiler that runs and a build that fails (a header that does not
#     exist, a compile error, a pg_config that cannot say where the headers
#     are): the test file FAILS, with the compiler's output.  A test that
#     skipped itself on a build error would hide a broken module behind a pass.
#
# The in-tree (meson) build compiles both modules with the extension, so a tree
# that cannot build them fails the build and not only the tests (meson.build).
#
# Portions Copyright (c) 2026, RooyeKhat Media Co
# SPDX-License-Identifier: AGPL-3.0-only

package KcProbe;

use strict;
use warnings FATAL => 'all';

use FindBin;
use IPC::Cmd ();
use PostgreSQL::Test::Utils;
use Test::More ();

use Exporter 'import';
our @EXPORT_OK = qw(build_module);

# _build('kcevents') -> (status, directory or message)
#   status 'ok': the directory that holds kcevents.so
#   status 'nocompiler': a message naming the module and the compiler
#   status 'failed': the compiler's output (or what is missing)
sub _build
{
	my ($name) = @_;
	my $src = "$FindBin::RealBin/$name/$name.c";
	my $pg_config = $ENV{PG_CONFIG} || 'pg_config';
	my $inc = `$pg_config --includedir-server 2>/dev/null`;
	chomp $inc;
	my $cc = `$pg_config --cc 2>/dev/null`;
	chomp $cc;
	$cc = 'cc' if $cc eq '';
	my ($prog) = split ' ', $cc;

	return ('nocompiler',
		"the test module $name (t/$name/$name.c) cannot be built here: no C compiler ($prog) was found")
	  unless IPC::Cmd::can_run($prog);
	return ('failed', "the source of the test module $name is missing: $src")
	  unless -f $src;
	return ('failed',
		"the server headers were not found (\"$pg_config --includedir-server\" gave \"$inc\"), so the test module $name cannot be built")
	  unless $inc ne '' && -d $inc;

	my $dir = PostgreSQL::Test::Utils::tempdir();
	my $out = `$cc -shared -fPIC -I"$inc" -o "$dir/$name.so" "$src" 2>&1`;
	return ('failed', "building the test module $name failed:\n$out")
	  if $? != 0 || !-f "$dir/$name.so";
	return ('ok', $dir);
}

# build_module('kcevents') -> the directory that holds kcevents.so
sub build_module
{
	my ($name) = @_;
	my ($status, $what) = _build($name);

	if ($status eq 'nocompiler')
	{
		Test::More::plan(skip_all => $what);
		exit 0;
	}
	if ($status ne 'ok')
	{
		# a failing test of its own, so that the file is reported as failed with the reason
		# (PostgreSQL::Test::Utils points STDERR at the log, a die would be seen there only)
		Test::More::plan(tests => 1);
		Test::More::fail("the test module $name builds");
		Test::More::diag($what);
		exit 1;
	}
	return $what;
}

1;
