# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/067_gid_tag.pl
#
# Review S7-09: any role can PREPARE TRANSACTION under any name, so a name of the
# extension's form is the extension's own only when its keyed tag verifies.  Recovery
# touches only those, a name of the right form with a wrong tag is left alone and
# reported by check_cluster (once, as a foreign prepared transaction), and no failover
# incident is written for it.  The names that 0.4 and the first 0.5 made carry no tag:
# they are still resolved when they were prepared before the node came to this
# version (the moment is kept in kingres.dtx_epoch) and are left alone after it.

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(make_cluster keys_of_group run_sql);

sub prepared { return $_[0]->safe_psql('postgres', 'SELECT count(*) FROM pg_prepared_xacts'); }

sub prepare_row
{
	my ($node, $gid, $id, $role) = @_;
	# An ordinary role cannot switch routing off; its keys are the node's own.
	my $set = defined $role ? "SET ROLE $role; " : 'SET kingres_shard.enable_routing = off; ';
	$node->safe_psql('postgres',
		"${set}BEGIN; INSERT INTO public.kv VALUES ($id, 'x'); PREPARE TRANSACTION '$gid'");
}

my ($n1, $n2, $n3) = make_cluster('gt', 3);
$n1->safe_psql('postgres', q{
	CREATE TABLE public.kv (id int PRIMARY KEY, val text);
	SELECT kingres.create_distributed_table('public.kv', 'id', bucket_count => 6);
});
$_->safe_psql('postgres', 'CREATE ROLE mallory LOGIN') for ($n1, $n2, $n3);
$n1->safe_psql('postgres', 'GRANT ALL ON public.kv TO mallory');
my @k = keys_of_group($n1, 1, 1, 10);
my $x = '4000000000';
sub tagged
{
	my ($body) = @_;
	return "${body}_" . $n1->safe_psql('postgres', "SELECT kingres.internal_gid_tag('$body')");
}

# --- the names ----------------------------------------------------------------------
# The extension's form with a valid tag: undecided -> rolled back, decided -> committed.
my $g1 = tagged("kingres_h0_1_999_${x}1_2");
prepare_row($n2, $g1, $k[0]);
my $g2 = tagged("kingres_h0_1_999_${x}2_2");
prepare_row($n2, $g2, $k[1]);
$n1->safe_psql('postgres', "INSERT INTO kingres.dtx_log VALUES ('$g2', 2, now())");
# The same form with a tag that does not verify (even though a decision row exists for one of them),
# prepared by an ordinary role, as in the review: an earlier timeline would have been rolled back
# and a failover incident written.
my $g3 = "kingres_h0_1_999_${x}3_2_0123456789abcdef";
prepare_row($n2, $g3, $k[2], 'mallory');
$n1->safe_psql('postgres', "INSERT INTO kingres.dtx_log VALUES ('$g3', 2, now())");
my $g4 = "kingres_h0_0_999_${x}4_2_fedcba9876543210";
prepare_row($n2, $g4, $k[3], 'mallory');
# A tag that is right for another body.
my $g4b = "kingres_h0_0_999_${x}7_2_" . $n1->safe_psql('postgres', "SELECT kingres.internal_gid_tag('kingres_h0_0_999_${x}8_2')");
prepare_row($n2, $g4b, $k[6], 'mallory');
# The older forms, prepared by a role after the node came to this version.
my $g5 = "kingres_1_999_${x}5_2";
prepare_row($n2, $g5, $k[4], 'mallory');
my $g6 = "kingres_g0_0_999_${x}6_2";
prepare_row($n2, $g6, $k[5], 'mallory');
is(prepared($n2), '7', 'seven transactions are prepared on node 2');

# --- check_cluster ---------------------------------------------------------------------
is($n1->safe_psql('postgres',
		"SELECT string_agg(object, ',' ORDER BY object) FROM kingres.check_cluster() WHERE check_name = 'foreign_prepared_transaction'"),
	join(',', sort ($g3, $g4, $g4b)),
	'check_cluster reports the names whose tag does not verify, once each, and not the valid ones');

# --- recovery ----------------------------------------------------------------------------
is($n1->safe_psql('postgres', 'SELECT kingres.recover_prepared_transactions()'), '2',
	'recovery resolved the two names with a valid tag');
is($n2->safe_psql('postgres', "SELECT string_agg(gid, ',' ORDER BY gid) FROM pg_prepared_xacts"),
	join(',', sort ($g3, $g4, $g4b, $g5, $g6)),
	'and left alone every other name of the form of the extension');
is($n2->safe_psql('postgres', "SET kingres_shard.enable_routing = off; SELECT string_agg(id::text, ',' ORDER BY id) FROM public.kv"),
	$k[1], 'the one with the decision committed, the undecided one was rolled back');
is($n1->safe_psql('postgres', "SELECT count(*) FROM kingres.dtx_incident"), '0',
	'no failover incident was written for a forged name');
is($n1->safe_psql('postgres', "SELECT count(*) FROM kingres.check_cluster() WHERE check_name = 'prepared_rolled_back_after_failover'"), '0',
	'and check_cluster has no alarm of a failover that did not happen');
# They are what the role made them: it can finish them itself.
$n2->safe_psql('postgres', "ROLLBACK PREPARED '$_'") for ($g3, $g4, $g4b, $g5, $g6);
is(prepared($n2), '0', 'the role ends its own prepared transactions');

# --- what an ordinary role can ask ---------------------------------------------------------
my ($rc, $out, $err) = run_sql($n2, "SET ROLE mallory; SELECT kingres.internal_gid_tag('x')");
like($err, qr/permission denied/, 'an ordinary role cannot ask for a tag');
($rc, $out, $err) = run_sql($n2, "SET ROLE mallory; SELECT kingres.internal_gid_class('x')");
like($err, qr/permission denied/, 'nor for the class of a name');

# --- the length -------------------------------------------------------------------------------
# The longest identifier the extension makes: a three-digit group, a timeline, a pid and a
# target of ten digits, twenty digits of transaction number, a repeat counter, the tag.
my $longest = "kingres_h999_4294967295_4294967295_" . ('9' x 20) . "_2147483647_99_" . ('a' x 16);
cmp_ok(length($longest), '<=', 150, "the longest identifier is " . length($longest) . " bytes");
$n2->safe_psql('postgres', "SET kingres_shard.enable_routing = off; BEGIN; INSERT INTO public.kv VALUES ($k[7], 'long'); PREPARE TRANSACTION '$longest'");
is($n2->safe_psql('postgres', "SELECT kingres.internal_gid_class('$longest')"), 'forged', 'it is parsed as the extension\'s form (and its tag is wrong)');
$n2->safe_psql('postgres', "ROLLBACK PREPARED '$longest'");
cmp_ok(length($longest), '<', 200, 'and fits the limit PostgreSQL puts on identifiers of prepared transactions (200 bytes)');

# --- the names of 0.4 and the moment of the upgrade ------------------------------------------------
{
	prepare_row($n2, "kingres_1_999_${x}11_2", $k[8]);
	# The upgrade of node 1 comes after that transaction was prepared, and before the next.
	my $cut = $n2->safe_psql('postgres',
		"SELECT prepared + interval '1 microsecond' FROM pg_prepared_xacts WHERE gid = 'kingres_1_999_${x}11_2'");
	$n1->safe_psql('postgres', "UPDATE kingres.dtx_epoch SET old_format_until = '$cut'");
	prepare_row($n2, "kingres_1_999_${x}12_2", $k[9], 'mallory');
	$n1->safe_psql('postgres', 'SELECT kingres.recover_prepared_transactions()');
	is($n2->safe_psql('postgres', "SELECT string_agg(gid, ',' ORDER BY gid) FROM pg_prepared_xacts"), "kingres_1_999_${x}12_2",
		'a 0.4 name prepared before the upgrade is resolved; the same name prepared after it is left alone');
}

done_testing();
