# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/272_ledger_refusal.pl: a ledger table is refused explicitly by create_distributed_table and create_reference_table (phase 7 step 25,
# design/ledger-phase7.md section 10): SQLSTATE KS016 and a message of its own, in the blocking and the online calls, for the history
# table of an updatable ledger table too; and check_cluster reports a ledger table that is also a distributed or reference table (a
# ledger enabled on a reference table afterwards).  t/142 has the refusals through the report; this test has the error.
# Needs kingres_ledger built and installed into the same installation as kingres_shard.

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(make_cluster run_sql prepared_count);

my ($n1, $n2) = make_cluster('lr', 2, extra => "shared_preload_libraries = 'kingres_shard,kingres_ledger'\nkingres_ledger.block_interval = 0\n");
sub qs { my ($n, $sql) = @_; return $n->safe_psql('postgres', $sql); }

qs($n1, q{
	CREATE EXTENSION kingres_ledger;
	CREATE TABLE public.led_a (id int PRIMARY KEY, v text);
	CREATE TABLE public.led_u (id int PRIMARY KEY, v text);
	CREATE TABLE public.plain (id int PRIMARY KEY, v text);
	INSERT INTO public.led_a SELECT g, 'a' || g FROM generate_series(1, 30) g;
	INSERT INTO public.led_u SELECT g, 'u' || g FROM generate_series(1, 30) g;
	INSERT INTO public.plain SELECT g, 'p' || g FROM generate_series(1, 30) g;
	SELECT kingres.ledger_enable('public.led_a', 'append_only');
	SELECT kingres.ledger_enable('public.led_u', 'updatable');
});
my $history = qs($n1, "SELECT history_relid::regclass::text FROM kingres.ledger_table WHERE relid = 'public.led_u'::regclass");

# the SQLSTATE, the message and the detail of a call that fails
sub failure
{
	my ($sql) = @_;
	my (undef, undef, $err) = run_sql($n1, "\\set VERBOSITY verbose\nSELECT $sql");
	return ($err =~ /ERROR:\s+(\w{5}):\s+([^\n]*)\n(.*)/s) ? ($1, $2, $3) : ('none', $err, '');
}

my @calls = (
	[q{kingres.create_distributed_table('public.led_a', 'id')}, 'distribute it'],
	[q{kingres.create_distributed_table('public.led_u', 'id', bucket_count => 4)}, 'distribute it'],
	[q{kingres.create_reference_table('public.led_a')}, 'make it a reference table'],
	[q{kingres.create_reference_table('public.led_u')}, 'make it a reference table'],
	[q{kingres.create_distributed_table_concurrently('public.led_a', 'id')}, 'distribute it'],
	[q{kingres.create_reference_table_concurrently('public.led_u')}, 'make it a reference table'],
	[qq{kingres.create_distributed_table('$history', 'id')}, 'distribute it'],
);
for my $c (@calls)
{
	my ($call, $what) = @$c;
	my ($st, $msg, $rest) = failure($call);
	is($st, 'KS016', "$call: SQLSTATE KS016");
	like($msg, qr/^cannot \Q$what\E: table "[^"]+" is a ledger table \(or the history table of one\) and stays where it is$/, '... and the message says what the table is');
	like($rest, qr/ledger_table: cannot (distribute|make)/, "... the detail lists the blocker with its code");
}
is(qs($n1, 'SELECT count(*) FROM kingres.dist_table') . ',' . qs($n1, 'SELECT count(*) FROM kingres.table_operation'), '0,0', 'nothing was distributed and no operation was recorded');
is(qs($n1, 'SELECT count(*) FROM public.led_a') . ',' . qs($n1, 'SELECT count(*) FROM public.led_u'), '30,30', 'the ledger tables are as they were');

# another refusal keeps its own SQLSTATE: a table that is not under the ledger and is refused for another reason
qs($n1, 'CREATE TABLE public.nokey (id int, v text); CREATE UNIQUE INDEX ON public.nokey (v)');
my ($st, $msg) = failure(q{kingres.create_distributed_table('public.nokey', 'id')});
is($st, '42P16', 'a refusal that is not about the ledger keeps its own SQLSTATE (invalid_table_definition)') or diag($msg);

# check_cluster: a ledger table that is also a distributed or reference table.  The kingres_shard hook refuses to put a trigger on a
# reference table, so ledger_enable cannot make this state; a cluster restored from a dump, or a catalog edited by hand, can have it.
is(qs($n1, q{SELECT count(*) FROM kingres.check_cluster() WHERE check_name = 'ledger_distributed_table'}), '0', 'check_cluster reports no ledger_distributed_table at first');
qs($n1, q{
	SELECT kingres.ledger_enable('public.plain', 'append_only');
	INSERT INTO kingres.dist_table (relid, method) VALUES ('public.plain'::regclass, 'reference');
});
is(qs($n1, q{SELECT string_agg(node_id || ':' || object, ',' ORDER BY node_id) FROM kingres.check_cluster() WHERE check_name = 'ledger_distributed_table'}),
	'1:public.plain', 'check_cluster names the ledger table that is a reference table, and its node');
like(qs($n1, q{SELECT detail FROM kingres.check_cluster() WHERE check_name = 'ledger_distributed_table'}), qr/ledger covers the rows of one node only/, '... and says why it matters');
# a ledger table in a distributed schema is another case (ledger_on_non_owner), and is not reported by this check
qs($n1, q{DELETE FROM kingres.dist_table WHERE relid = 'public.plain'::regclass});
is(qs($n1, q{SELECT count(*) FROM kingres.check_cluster() WHERE check_name = 'ledger_distributed_table'}), '0', 'the check is quiet again when the table is not distributed');
# the history table of an updatable ledger table counts too
qs($n1, "INSERT INTO kingres.dist_table (relid, method) VALUES ('$history'::regclass, 'reference')");
is(qs($n1, q{SELECT string_agg(node_id || ':' || object, ',' ORDER BY node_id) FROM kingres.check_cluster() WHERE check_name = 'ledger_distributed_table'}),
	"1:$history", 'check_cluster names the history table of an updatable ledger table that is a reference table');
qs($n1, "DELETE FROM kingres.dist_table WHERE relid = '$history'::regclass");
is(prepared_count($_), '0', 'nothing is prepared on ' . $_->name) for ($n1, $n2);
done_testing();
