# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/010_install_script_current.pl: the committed kingres_timepart--0.3.sql is the output of mk-install-script.sh.
#
# The install script of 0.3 is generated from the 0.2 install script and the 0.2--0.3 upgrade script.  A change of the
# upgrade script without the generator leaves a stale install script that git does not flag: a fresh install then differs
# from an upgrade.  The test generates the file again into a temporary place and compares.

use strict;
use warnings FATAL => 'all';
use FindBin;
use File::Compare;
use PostgreSQL::Test::Utils;
use Test::More;

my $dir = "$FindBin::RealBin/..";
my $tmp = PostgreSQL::Test::Utils::tempdir();
my $out = "$tmp/kingres_timepart--0.3.sql";
plan skip_all => 'mk-install-script.sh is not in this copy' unless -f "$dir/mk-install-script.sh";

local $ENV{KT_INSTALL_SCRIPT_OUT} = $out;
my $rc = system('sh', "$dir/mk-install-script.sh");
is($rc, 0, 'mk-install-script.sh runs');
ok(-s $out, '... and writes the install script');
is(compare($out, "$dir/kingres_timepart--0.3.sql"), 0,
	'kingres_timepart--0.3.sql is what mk-install-script.sh generates (run it in contrib/kingres_timepart and commit the result)')
  or diag("diff -u $dir/kingres_timepart--0.3.sql $out");

done_testing();
