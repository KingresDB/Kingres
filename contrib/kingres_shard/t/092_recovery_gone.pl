# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/092_recovery_gone.pl
#
# Review S10 ("Not reproduced"): recovery sends COMMIT PREPARED (or ROLLBACK PREPARED) for a transaction
# that is gone by the time the command arrives (somebody committed it by hand, or the first
# COMMIT PREPARED whose cancel came late already did).  The participant answers "does not exist"
# (SQLSTATE 42704): that is a resolved transaction, not a failure.  The pass stops at the fault
# point recovery_before_resolve, the transaction is committed by hand on the participant, the pass
# goes on: no error, no warning, the row is there exactly once, nothing is left prepared, and the
# next pass has nothing to do.  The same for a transaction that recovery would roll back.

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(make_cluster keys_of_group run_sql start_psql finish_psql wait_for_fault_file);

sub prepared { return $_[0]->safe_psql('postgres', 'SELECT count(*) FROM pg_prepared_xacts'); }

my ($n1, $n2) = make_cluster('rg', 2);
$n1->safe_psql('postgres', q{
	CREATE TABLE public.kv (id int PRIMARY KEY, val text);
	SELECT kingres.create_distributed_table('public.kv', 'id', bucket_count => 6);
});
my @k = keys_of_group($n1, 1, 1, 4);
my $x = '4000000000';

sub tagged
{
	my ($body) = @_;
	return "${body}_" . $n1->safe_psql('postgres', "SELECT kingres.internal_gid_tag('$body')");
}

sub prepare_row
{
	my ($gid, $id) = @_;
	$n2->safe_psql('postgres',
		"SET kingres_shard.enable_routing = off; BEGIN; INSERT INTO public.kv VALUES ($id, 'x'); PREPARE TRANSACTION '$gid'");
}

foreach my $case ([ 'commit', 0, 'COMMIT PREPARED', 1 ], [ 'rollback', 1, 'ROLLBACK PREPARED', 0 ])
{
	my ($what, $i, $stmt, $rows) = @$case;
	my $gid = tagged("kingres_h0_1_999_${x}${i}_2");
	prepare_row($gid, $k[$i]);
	# a decision row says commit; none says roll back
	$n1->safe_psql('postgres', "INSERT INTO kingres.dtx_log VALUES ('$gid', 2, now())") if $what eq 'commit';
	my $file = $n1->basedir . "/rg_gate_$what";
	open(my $fh, '>', $file) or die;
	close($fh);
	my $h = start_psql($n1,
		"SET kingres_shard.fault_injection = 'recovery_before_resolve:wait:$file'; SELECT kingres.recover_prepared_transactions()");
	ok(wait_for_fault_file("$file.reached", 30), "[$what] recovery has listed the transaction and is about to resolve it");
	$n2->safe_psql('postgres', "$stmt '$gid'");
	is(prepared($n2), '0', "[$what] somebody resolved it by hand in the meantime");
	unlink $file;
	my ($out, $err) = finish_psql($h);
	chomp($out);
	is($err, '', "[$what] the pass ends without an error or a warning ($err)");
	like($out, qr/^0$/m, "[$what] and does not count a transaction that it did not resolve");
	is($n2->safe_psql('postgres', "SET kingres_shard.enable_routing = off; SELECT count(*) FROM public.kv WHERE id = $k[$i]"),
		$rows, "[$what] the row is there " . ($rows ? 'once' : 'not at all'));
	is($n1->safe_psql('postgres', 'SELECT kingres.recover_prepared_transactions()'), '0', "[$what] the next pass has nothing to do");
	is($n1->safe_psql('postgres', "SELECT count(*) FROM kingres.dtx_incident"), '0', "[$what] no incident was written");
}

done_testing();
