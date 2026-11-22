# SPDX-License-Identifier: AGPL-3.0-only
#
# contrib/kingres_ledger/t/040_columnar_refused.pl
#
# Rule L1 of design/analytics-phase12-storage.md, 9.6: a ledger table is a heap table.  ledger_enable refuses a table whose access
# method is not heap (the capture code and the row hashes read heap tuples, and nothing of them is tested against another storage), and
# the other way round, a ledger table cannot be turned into a columnar table: ALTER TABLE ... SET ACCESS METHOD rewrites the table, and
# the rewrite of a ledger table is rejected by its event trigger.  Needs kingres_columnar built and installed into the same installation.
#
# Portions Copyright (c) 2026, RooyeKhat Media Co

use strict;
use warnings FATAL => 'all';

use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;

my $node = PostgreSQL::Test::Cluster->new('col_refused');
$node->init;
$node->append_conf('postgresql.conf',
	"shared_preload_libraries = 'kingres_ledger,kingres_columnar'\n"
	  . "kingres_ledger.block_interval = 0\n");
$node->start;

my ($rc, $out, $err) = $node->psql('postgres', 'CREATE EXTENSION kingres_columnar');
plan skip_all => 'kingres_columnar is not installed in this installation' if $rc != 0 && $err =~ /could not open extension control file|is not available/;
is($rc, 0, 'kingres_columnar is installed') or diag($err);
$node->safe_psql('postgres', 'CREATE EXTENSION kingres_ledger');

$node->safe_psql('postgres', q{
	CREATE TABLE public.col_t (id int PRIMARY KEY, v text) USING columnar;
	INSERT INTO public.col_t SELECT g, 'c' || g FROM generate_series(1, 20) g;
	CREATE TABLE public.heap_t (id int PRIMARY KEY, v text);
	INSERT INTO public.heap_t SELECT g, 'h' || g FROM generate_series(1, 20) g;
});

# a columnar table is refused, in both modes, and is left as it was
for my $mode (qw(append_only updatable))
{
	($rc, $out, $err) = $node->psql('postgres', "SELECT kingres.ledger_enable('public.col_t', '$mode')");
	isnt($rc, 0, "ledger_enable refuses a columnar table ($mode)");
	like($err, qr/table "col_t" uses the access method "columnar" and cannot be a ledger table/, "... with its own message ($mode)");
	like($err, qr/ERROR:.*\n.*DETAIL:.*heap tables/s, "... and a detail ($mode)");
}
is($node->safe_psql('postgres', "SELECT count(*) FROM kingres.ledger_table WHERE relid = 'public.col_t'::regclass"), '0', 'no registration was made');
is($node->safe_psql('postgres', "SELECT count(*) FROM pg_trigger WHERE tgrelid = 'public.col_t'::regclass AND NOT tgisinternal"), '0', 'no trigger was made on the table');
is($node->safe_psql('postgres', 'INSERT INTO public.col_t VALUES (100, \'later\'); SELECT count(*) FROM public.col_t'), '21', 'the columnar table still takes rows');

# a heap table is a ledger table as before
$node->safe_psql('postgres', "SELECT kingres.ledger_enable('public.heap_t', 'append_only')");
is($node->safe_psql('postgres', "SELECT count(*) FROM kingres.ledger_table WHERE relid = 'public.heap_t'::regclass"), '1', 'a heap table is put under the ledger');

# and a ledger table cannot become columnar
($rc, $out, $err) = $node->psql('postgres', 'ALTER TABLE public.heap_t SET ACCESS METHOD columnar');
isnt($rc, 0, 'SET ACCESS METHOD on a ledger table is refused');
like($err, qr/ledger/i, '... by the ledger');
is($node->safe_psql('postgres', "SELECT a.amname FROM pg_class c JOIN pg_am a ON a.oid = c.relam WHERE c.oid = 'public.heap_t'::regclass"), 'heap', 'the ledger table is still heap');
is($node->safe_psql('postgres', 'SELECT count(*) FROM public.heap_t'), '20', 'with its rows');
is($node->safe_psql('postgres', 'SELECT count(*) FROM kingres.ledger_verify()'), '0', 'and the ledger still verifies');

done_testing();
