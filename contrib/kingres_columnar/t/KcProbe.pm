# contrib/kingres_columnar/t/KcProbe.pm
#
# Builds one of the test-only modules under t/ (kcprobe, kcevents) with the
# compiler and the headers of the installation the test runs against, the way
# the modules under src/test/modules are built for their tests: they are never
# installed and never part of a release.  A test that needs one skips itself,
# with the reason, where it cannot be built.
#
# Portions Copyright (c) 2026, RooyeKhat Media Co
# SPDX-License-Identifier: AGPL-3.0-only

package KcProbe;

use strict;
use warnings FATAL => 'all';

use FindBin;
use PostgreSQL::Test::Utils;

use Exporter 'import';
our @EXPORT_OK = qw(build_module);

# build_module('kcevents') -> the directory that holds kcevents.so, or undef
sub build_module
{
	my ($name) = @_;
	my $src = "$FindBin::RealBin/$name/$name.c";
	my $dir = PostgreSQL::Test::Utils::tempdir();
	my $pg_config = $ENV{PG_CONFIG} || 'pg_config';
	my $inc = `$pg_config --includedir-server 2>/dev/null`;
	chomp $inc;
	my $cc = `$pg_config --cc 2>/dev/null`;
	chomp $cc;
	$cc = 'cc' if $cc eq '';

	return undef unless $inc ne '' && -f $src && -d $inc;
	my $out = `$cc -shared -fPIC -I"$inc" -o "$dir/$name.so" "$src" 2>&1`;
	if ($? != 0 || !-f "$dir/$name.so")
	{
		diag("building the test module $name failed: $out");
		return undef;
	}
	return $dir;
}

1;
