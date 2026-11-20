# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/142_distribute_ledger.pl: a ledger table (kingres_ledger) is refused as the
# source of a distribution, as design/shard-phase9.md says (blocker
# "ledger_table"): its capture trigger and its protection hook are the integrity
# mechanism, and a copy into a new table would start a new history.  This test
# needs contrib/kingres_ledger built and installed into the same installation as
# kingres_shard (make -C contrib/kingres_ledger install against the same
# pg_config); both libraries are preloaded.  And the reverse: a distributed table
# cannot be put under the ledger (ledger_enable refuses a partitioned table), so
# a distributed ledger table does not exist (design, "Needs verification" 11).

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(make_cluster run_sql prepared_count);

my ($n1, $n2) = make_cluster('dl', 2, extra => "shared_preload_libraries = 'kingres_shard,kingres_ledger'\n"
	. "kingres_ledger.block_interval = 0\n");

$n1->safe_psql('postgres', q{
	CREATE EXTENSION kingres_ledger;
	CREATE TABLE public.led_a (id int PRIMARY KEY, v text);
	CREATE TABLE public.led_u (id int PRIMARY KEY, v text);
	CREATE TABLE public.led_ok (id int PRIMARY KEY, v text);
	INSERT INTO public.led_a SELECT g, 'a' || g FROM generate_series(1, 50) g;
	INSERT INTO public.led_u SELECT g, 'u' || g FROM generate_series(1, 50) g;
	INSERT INTO public.led_ok SELECT g, 'ok' || g FROM generate_series(1, 50) g;
	SELECT kingres.ledger_enable('public.led_a', 'append_only');
	SELECT kingres.ledger_enable('public.led_u', 'updatable');
});
my $history = $n1->safe_psql('postgres', "SELECT history_relid::regclass::text FROM kingres.ledger_table WHERE relid = 'public.led_u'::regclass");
ok($history ne '', "the updatable ledger table has a history table ($history)");

sub codes
{
	my ($sql) = @_;
	return $n1->safe_psql('postgres', "SELECT coalesce(string_agg(severity || ':' || code, ',' ORDER BY code), '') FROM kingres.table_change_report($sql)");
}

for my $t (qw(led_a led_u))
{
	like(codes("'distribute', 'public.$t', '{\"column\":\"id\"}'"), qr/blocker:ledger_table/, "the report names the ledger table $t");
	like(codes("'distribute_reference', 'public.$t'"), qr/blocker:ledger_table/, "also for a reference table ($t)");
	my (undef, undef, $err) = run_sql($n1, "SELECT kingres.create_distributed_table('public.$t', 'id')");
	like($err, qr/ledger_table:.*is a ledger table/s, "create_distributed_table refuses $t and says why");
	(undef, undef, $err) = run_sql($n1, "SELECT kingres.create_reference_table('public.$t')");
	like($err, qr/ledger_table:.*is a ledger table/s, "create_reference_table refuses $t and says why");
	is($n1->safe_psql('postgres', "SELECT (SELECT count(*) FROM public.$t) || ',' || (SELECT relkind::text FROM pg_class WHERE oid = 'public.$t'::regclass)"),
		'50,r', "$t is the plain table it was, with its rows");
}
{
	my (undef, undef, $err) = run_sql($n1, "SELECT kingres.create_distributed_table('$history', 'id')");
	like($err, qr/ledger_table:/s, 'the history table of an updatable ledger table is refused too');
}
is($n1->safe_psql('postgres', "SELECT count(*) FROM kingres.dist_table"), '0', 'nothing was distributed');
is($n1->safe_psql('postgres', "SELECT count(*) FROM pg_class WHERE relname LIKE 'kingres\\_shadow%' OR relname ~ '^led_[a-z]_b[0-9]{4}\$'"), '0', 'and nothing is left behind');
is($n2->safe_psql('postgres', "SELECT count(*) FROM pg_class WHERE relname LIKE 'kingres\\_shadow%' OR relname ~ '^led_[a-z]_b[0-9]{4}\$'"), '0', 'on the other node either');
is(prepared_count($_), '0', 'nothing is prepared on ' . $_->name) for ($n1, $n2);

# the ledger still works and verifies after the refusals
like($n1->safe_psql('postgres', "INSERT INTO public.led_a VALUES (100, 'later') RETURNING id"), qr/^100$/, 'the ledger table still takes rows');
my ($rc, $out, $err) = run_sql($n1, "SELECT count(*) FROM kingres.ledger_verify()");
is($rc, 0, 'ledger_verify() runs') or diag($err);

# a table that is not under the ledger is distributed as usual, next to ledger tables
$n1->safe_psql('postgres', "SELECT kingres.create_distributed_table('public.led_ok', 'id', bucket_count => 8)");
is($n1->safe_psql('postgres', 'SELECT count(*) FROM public.led_ok'), '50', 'a table outside the ledger is distributed with its rows');
is($n2->safe_psql('postgres', 'SELECT count(*) FROM public.led_ok'), '50', 'and is whole from the other node');

# and the reverse: a distributed table cannot be put under the ledger
($rc, $out, $err) = run_sql($n1, "SELECT kingres.ledger_enable('public.led_ok', 'append_only')");
isnt($rc, 0, 'ledger_enable refuses a distributed table');
like($err, qr/./, 'with a message');
diag("ledger_enable on a distributed table says: $err");
is($n1->safe_psql('postgres', "SELECT count(*) FROM kingres.ledger_table WHERE relid = 'public.led_ok'::regclass"), '0',
	'so a distributed ledger table does not exist');

done_testing();
