# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/087_previous_secret.pl
#
# Review S10-04: after a rotation the previous secret stayed on every participant until the next
# rotation, and the tag check of a prepared transaction's name also accepts a tag made with it, so
# a leaked old secret kept one of its powers.  The invariant: the previous secret is wiped on
# every node once the rotation is known to be complete everywhere (the recovery pass that reached
# every primary with the current secret knows; it tells the others), and until then nothing but an
# outgoing session to a node that is stranded in the rotation uses it.
#
#   - the second point is established first: the tag of a name made with the old secret is
#     accepted as the cluster's on a participant after the rotation (the window);
#   - a pass that cannot reach every primary wipes nothing, on any node;
#   - the pass that reaches every primary wipes the previous secret on all of them, and the old
#     tag is then foreign everywhere.

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(make_cluster run_sql);

my ($n1, $n2, $n3) = make_cluster('ps', 3);

sub has_previous { return $_[0]->safe_psql('postgres', 'SELECT previous IS NOT NULL FROM kingres.cluster_secret'); }
sub gid_class { my ($n, $gid) = @_; return $n->safe_psql('postgres', "SELECT kingres.internal_gid_class('$gid')"); }

# A name of the extension's form, tagged with the secret that is in force now.
my $body = 'kingres_h0_1_999_4000000000_3';
my $old_gid = "${body}_" . $n1->safe_psql('postgres', "SELECT kingres.internal_gid_tag('$body')");
is(gid_class($n2, $old_gid), 'tagged', 'before the rotation the tag verifies');

$n1->safe_psql('postgres', 'SELECT kingres.rotate_secret()');
is(has_previous($_), 't', 'after the rotation every node holds the previous secret') for ($n1, $n2, $n3);
# the window: the previous secret still counts for a tag
is(gid_class($n2, $old_gid), 'tagged', 'in the window a participant accepts a tag made with the previous secret');

# --- a pass that cannot reach every primary wipes nothing anywhere ---------------------------
$n3->stop('fast');
my ($rc, $out, $err) = run_sql($n1, 'SELECT kingres.recover_prepared_transactions()');
is($rc, 0, 'recovery with a node down runs');
is(has_previous($n1), 't', 'the node that ran it keeps the previous secret');
is(has_previous($n2), 't', 'the participant that was reached keeps it too');
$n3->start;

# --- the pass that reaches every primary wipes it on all of them -------------------------------
($rc, $out, $err) = run_sql($n1, 'SELECT kingres.recover_prepared_transactions()');
is($rc, 0, 'recovery with every node up runs');
is(has_previous($_), 'f', 'the previous secret is gone on every node') for ($n1, $n2, $n3);
is(gid_class($_, $old_gid), 'forged', 'the old tag is forged on every node') for ($n1, $n2, $n3);

# a name tagged with the new secret is still the cluster's
my $body2 = 'kingres_h0_1_999_4000000001_3';
my $new_gid = "${body2}_" . $n1->safe_psql('postgres', "SELECT kingres.internal_gid_tag('$body2')");
is(gid_class($n2, $new_gid), 'tagged', 'a tag made with the current secret still verifies');

done_testing();
