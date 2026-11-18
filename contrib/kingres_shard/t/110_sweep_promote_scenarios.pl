# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/110_sweep_promote_scenarios.pl
#
# S13-01 and S13-05 for promote_node(): the sweep of t/101 with the other ways to fail and with the call inside a savepoint,
# a PL/pgSQL block with an EXCEPTION clause and a transaction block.  Building a cluster for every position makes this the slowest
# file: the default run takes the savepoint variant only; KINGRES_LONG_TESTS=1 adds the PL/pgSQL block and the transaction block.
#
# The work is in 101_sweep_promote.pl and t/KsSweep.pm; this file only chooses the scenarios (KS_SWEEP_SCENARIO), so that the scenarios run in
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

BEGIN { $ENV{KS_SWEEP_SCENARIO} = $ENV{KINGRES_LONG_TESTS} ? 'modes,variants' : 'modes,savepoint'; }
do "$FindBin::RealBin/101_sweep_promote.pl" or die($@ || $!);
