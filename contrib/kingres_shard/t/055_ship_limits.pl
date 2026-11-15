# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/055_ship_limits.pl
#
# What may be shipped to another node: a value or restriction of type money is
# not shipped (its text form depends on lc_monetary of the session), and an ON
# CONFLICT action that this extension has not been designed for (PostgreSQL 19's
# DO SELECT) is refused.

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(make_cluster keys_of_group run_sql);

my ($n1, $n2) = make_cluster('sl', 2);
$n1->safe_psql('postgres', q{
	CREATE TABLE public.kv (id int PRIMARY KEY, val text, price money);
	SELECT kingres.create_distributed_table('public.kv', 'id', bucket_count => 8);
	CREATE TABLE public.ref (id int PRIMARY KEY, price money);
	CREATE TABLE public.ref2 (id int PRIMARY KEY, v int);
	SELECT kingres.create_reference_table('public.ref2');
	SELECT kingres.create_reference_table('public.ref');
});
my ($ka) = keys_of_group($n1, 1, 0, 1);
my ($kb) = keys_of_group($n1, 1, 1, 1);
$n1->safe_psql('postgres', "INSERT INTO public.kv VALUES ($ka, 'a', 1)");
$n1->safe_psql('postgres', "INSERT INTO public.kv VALUES ($kb, 'b', 2)");

# A fanned-out write that restricts by money, or sets a money value, is not shipped.
my ($rc, $out, $err) = run_sql($n1, "UPDATE public.kv SET val = 'x' WHERE price > '1'::money");
isnt($rc, 0, 'an UPDATE that spans groups and restricts by money is refused');
like($err, qr/cannot be shipped|not supported|money/i, 'and says it cannot be shipped');
($rc, $out, $err) = run_sql($n1, "UPDATE public.kv SET price = '5'::money");
isnt($rc, 0, 'an UPDATE that spans groups and sets a money value is refused');
is($n1->safe_psql('postgres', "SELECT count(*) FROM public.kv WHERE val = 'x'"), '0', 'and nothing was written');
# The same statement for one key runs on its group (nothing is shipped to other groups).
$n1->safe_psql('postgres', "UPDATE public.kv SET price = '7'::money WHERE id = $kb");
is($n1->safe_psql('postgres', "SELECT price::numeric FROM public.kv WHERE id = $kb"), '7.00', 'a one-key write with money works');
# A reference table write with a money value runs on every node and is refused.
($rc, $out, $err) = run_sql($n1, "INSERT INTO public.ref VALUES (1, '3'::money)");
isnt($rc, 0, 'a reference-table write with a money value is refused');

SKIP:
{
	my $major = $n1->safe_psql('postgres', "SELECT current_setting('server_version_num')::int / 10000");
	skip 'ON CONFLICT DO SELECT exists on PostgreSQL 19 only', 2 unless $major >= 19;
	($rc, $out, $err) = run_sql($n1,
		"INSERT INTO public.ref2 VALUES (2, 1) ON CONFLICT (id) DO SELECT RETURNING id");
	isnt($rc, 0, 'ON CONFLICT DO SELECT on a reference table is refused');
	like($err, qr/ON CONFLICT action/, 'and says why');
}

done_testing();
