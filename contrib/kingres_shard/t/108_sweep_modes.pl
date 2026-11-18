# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/108_sweep_modes.pl
#
# S13-05: the sweep of t/100 with the three other ways in which a node can fail at the n-th chunk of a session: the
# connection is closed (close_after), the node's answer is lost (lose_answer), the node answers with an error (error_at).
# Judged by the same rule as the silent node: an error and no mixture, or a success done everywhere, or a success with a
# warning that names the node that was left and one recovery pass that settles it.
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

BEGIN { $ENV{KS_SWEEP_SCENARIO} = 'modes'; }
do "$FindBin::RealBin/100_sweep_calls.pl" or die($@ || $!);
