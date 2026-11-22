# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/275_prepare_confirm.pl: the confirmation after PREPARE TRANSACTION (internal_sync_confirm) is skipped only for the node that add_node
# is making part of the cluster, whose extension is made in the very transaction (review P12S-04).  A participant of the cluster that
# lacks the function fails the transaction instead of dropping the guarantee that its prepared record is durable on its standby.

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(make_cluster run_sql prepared_count wait_prepared_count);

my ($n1, $n2) = make_cluster('pc', 2);
sub qs { my ($n, $sql) = @_; return $n->safe_psql('postgres', $sql); }
qs($n1, q{CREATE TABLE public.t (id int PRIMARY KEY, v text); SELECT kingres.create_distributed_table('public.t', 'id', bucket_count => 8)});

# a write that spans both groups is a two-phase transaction
my ($rc, undef, $err) = run_sql($n1, "INSERT INTO public.t SELECT g, 'a' FROM generate_series(1, 200) g");
is($rc, 0, 'a write that spans both groups commits') or diag($err);
is(qs($n1, 'SELECT count(*) FROM public.t'), '200', '... with every row');

# the function is gone on a participant of the cluster
qs($n2, q{ALTER EXTENSION kingres_shard DROP FUNCTION kingres.internal_sync_confirm(integer); DROP FUNCTION kingres.internal_sync_confirm(integer)});
($rc, undef, $err) = run_sql($n1, "INSERT INTO public.t SELECT g, 'b' FROM generate_series(201, 400) g");
isnt($rc, 0, 'a participant that lacks internal_sync_confirm fails the transaction');
like($err, qr/could not confirm the prepared transaction on node/, '... with the message of the confirmation');
ok(wait_prepared_count($n2, 0) || 1, 'the prepared part is rolled back');
is(prepared_count($n2), '0', '... on the participant');
is(qs($n1, 'SELECT count(*) FROM public.t'), '200', 'and nothing of the failed transaction is visible');

done_testing();
