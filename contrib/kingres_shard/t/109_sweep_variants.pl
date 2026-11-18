# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/109_sweep_variants.pl
#
# S13-01: the sweep of t/100 for a call that is not in autocommit: inside SAVEPOINT / RELEASE, inside a PL/pgSQL block with an
# EXCEPTION clause, inside a transaction block that the caller commits.  The statements that end the work of a node session
# (RELEASE SAVEPOINT, ROLLBACK TO SAVEPOINT, COMMIT TRANSACTION) are sent after the call has been left, and must end within
# the bound of the README with an error or a warning, whatever the node does.
#
# The work is in 100_sweep_calls.pl and t/KsSweep.pm; this file only chooses the scenarios (KS_SWEEP_SCENARIO), so that the scenarios run in
# parallel with the default sweep.  The positions are a sample by default (the first, the last, one in each third of the
# call, and the chunks around the RELEASE, COMMIT, PREPARE and second-phase statements); every position runs with
# KINGRES_LONG_TESTS=1.

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
# loaded here, at compile time: the harness modules have INIT blocks that must not be loaded by the do below
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;

BEGIN { $ENV{KS_SWEEP_SCENARIO} = 'variants'; }
do "$FindBin::RealBin/100_sweep_calls.pl" or die($@ || $!);
