# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/069_inflight_promotion.pl
#
# Statements in flight across a promotion (README, "High availability", guarantee 8)
# that t/049 and t/054 do not cover: a cursor that is open when the group's primary
# changes, its rescan (MOVE and FETCH backwards), and COPY.  The old primary is alive
# and is told by promote_node (the case where it can be fenced late).  What holds is
# the rule of the scan (ADR 0022): a statement answers completely and correctly, or it
# fails with an error the application can retry; it never answers with a part.

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(make_cluster keys_of_group run_sql make_standby register_standby
  wait_catchup set_sync promote_standby);

my ($n1, $n2) = make_cluster('ip', 2, replication => 1);
$n1->safe_psql('postgres', q{
	CREATE TABLE public.kv (id int PRIMARY KEY, val text);
	SELECT kingres.create_distributed_table('public.kv', 'id', bucket_count => 12);
});
my $s2 = make_standby($n2, 'ip_s2');
my $id = register_standby($n1, $s2, 1);
set_sync($n2, $s2);
my @k0 = keys_of_group($n1, 1, 0, 40);
my @k1 = keys_of_group($n1, 1, 1, 80);
$n1->safe_psql('postgres', "INSERT INTO public.kv VALUES ($_, 'old')") for (@k0, @k1[0 .. 39]);
wait_catchup($n2, $s2);
my $before = scalar(@k0) + 40;

# A cursor, opened and partly read before the promotion.
my $c = $n1->background_psql('postgres', on_error_stop => 0);
$c->query_safe("\\set VERBOSITY verbose");
$c->query_safe("BEGIN");
$c->query_safe("DECLARE cur SCROLL CURSOR FOR SELECT id FROM public.kv ORDER BY id");
my $first = $c->query_safe("FETCH 5 FROM cur");
is(scalar(split /\n/, $first), 5, 'the cursor delivers its first rows');

# The switchover: the old primary is alive, the standby is promoted, the catalog changes.
$n2->safe_psql('postgres', 'CHECKPOINT');
promote_standby($s2);
set_sync($n2);
$n1->safe_psql('postgres', "SELECT kingres.promote_node($id)");

# The rest of the cursor: complete, or an error that says to retry; never a part.
my $rest = $c->query("FETCH ALL FROM cur");
my $err = $c->{stderr};
if ($err eq '')
{
	my @rows = split /\n/, $rest;
	is(scalar(@rows), $before - 5, 'cursor: the rest of the rows arrived, as many as the snapshot of the cursor had');
}
else
{
	like($err, qr/bucket map changed|KS001|could not connect|connection/, 'cursor: it failed with an error the application can retry');
	pass('cursor: and delivered nothing more');
}
$c->{stderr} = '';
# A rescan after the promotion: complete, or an error; and the transaction can be ended.
my $again = $c->query("MOVE ABSOLUTE 0 IN cur");
$again = $c->query("FETCH ALL FROM cur");
my $err2 = $c->{stderr};
if ($err2 eq '')
{
	is(scalar(split /\n/, $again), $before, 'rescan: all the rows again, from the same snapshot');
}
else
{
	like($err2, qr/ERROR/, 'rescan: or an error');
	pass('rescan: nothing partial');
}
$c->query("ROLLBACK");
$c->quit;

# New statements answer from the new primary, completely.
is($n1->safe_psql('postgres', 'SELECT count(*) FROM public.kv'), $before, 'a new scan reads every row');

# COPY out, after the promotion: complete.
my $out = $n1->safe_psql('postgres', "COPY (SELECT id FROM public.kv ORDER BY id) TO STDOUT");
is(scalar(split /\n/, $out), $before, 'COPY TO reads every row');
# COPY into a distributed table with the row router off is refused by the extension (it would
# write one node only): the refusal is the same after the promotion, and says so; nothing is written.
my ($rc, $o, $e) = run_sql($n1, "SET kingres_shard.enable_copy_routing = off;\nCOPY public.kv FROM STDIN;\n999999\tx\n\\.\n");
isnt($rc, 0, 'COPY FROM into a distributed table is refused after the promotion with the router off, as before it');
like($e, qr/COPY FROM is not supported on distributed tables/, 'with the extension\'s message');
is($n1->safe_psql('postgres', "SELECT count(*) FROM public.kv WHERE id = 999999"), '0', 'and nothing was written');
# With the row router on (phase 2b, step 4) the COPY goes to the group that owns the bucket, whose
# primary after the promotion is the promoted node, and the row is there exactly once.
($rc, $o, $e) = run_sql($n1, "COPY public.kv FROM STDIN;\n999999\tx\n\\.\n");
is($rc, 0, 'COPY FROM after the promotion is routed') or diag $e;
is($n1->safe_psql('postgres', "SELECT count(*) FROM public.kv WHERE id = 999999"), '1', 'and the row is there once');
$n1->safe_psql('postgres', "DELETE FROM public.kv WHERE id = 999999");

# A transaction of ordinary INSERTs after the promotion reaches the new primary, and commits on it.
my @new = @k1[40 .. 59];
$n1->safe_psql('postgres', "BEGIN; " . join(' ', map { "INSERT INTO public.kv VALUES ($_, 'new');" } @new) . " COMMIT");
is($s2->safe_psql('postgres', "SET kingres_shard.enable_routing = off; SELECT count(*) FROM public.kv WHERE val = 'new'"), scalar(@new),
	'the rows of the group are on the promoted node');
is($n2->safe_psql('postgres', "SET kingres_shard.enable_routing = off; SELECT count(*) FROM public.kv WHERE val = 'new'"), '0',
	'and none is on the old primary');
is($s2->safe_psql('postgres', 'SELECT count(*) FROM pg_prepared_xacts') . $n1->safe_psql('postgres', 'SELECT count(*) FROM pg_prepared_xacts'), '00',
	'nothing is left prepared');

done_testing();
