# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/250_install_script_current.pl: the committed kingres_shard--0.8.sql is the output of mk-install-script.sh (review P12R-02).
#
# The install script of 0.8 is generated from the 0.7 install script and the 0.7--0.8 upgrade script.  A change of the 0.7 scripts (a merge
# of a fix for 0.7) leaves a stale 0.8.sql that git does not flag: a fresh install then differs from an upgrade.  The test generates the
# file again into a temporary place and compares.

use strict;
use warnings FATAL => 'all';
use FindBin;
use File::Compare;
use PostgreSQL::Test::Utils;
use Test::More;

my $dir = "$FindBin::RealBin/..";
my $tmp = PostgreSQL::Test::Utils::tempdir();
my $out = "$tmp/kingres_shard--0.8.sql";
plan skip_all => 'mk-install-script.sh is not in this copy' unless -f "$dir/mk-install-script.sh";

local $ENV{KS_INSTALL_SCRIPT_OUT} = $out;
my $rc = system('sh', "$dir/mk-install-script.sh");
is($rc, 0, 'mk-install-script.sh runs');
ok(-s $out, '... and writes the install script');
is(compare($out, "$dir/kingres_shard--0.8.sql"), 0,
	'kingres_shard--0.8.sql is what mk-install-script.sh generates (run it in contrib/kingres_shard and commit the result)')
  or diag("diff -u $dir/kingres_shard--0.8.sql $out");

# the install script of 0.9 is the 0.8 install script with the 0.8--0.9 upgrade script appended
my $out9 = "$tmp/kingres_shard--0.9.sql";
local $ENV{KS_INSTALL_SCRIPT_OUT_09} = $out9;
$rc = system('sh', "$dir/mk-install-script.sh");
is($rc, 0, 'mk-install-script.sh runs again for 0.9');
is(compare($out9, "$dir/kingres_shard--0.9.sql"), 0,
	'kingres_shard--0.9.sql is what mk-install-script.sh generates (run it in contrib/kingres_shard and commit the result)')
  or diag("diff -u $dir/kingres_shard--0.9.sql $out9");

done_testing();
