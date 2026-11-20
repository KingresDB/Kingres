# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/128_router_review_round.pl
#
# The fixes for the independent review of steps 3 and 4 of phase 2b (reviews/2026-11-19-kingres-shard-phase2b-steps-3-4.md):
#
#   A. P2C-01: a key whose type has a modifier that changes the stored value (timestamp(0), timestamptz(0), numeric(p,s),
#      char(n), varchar(n), time(0)) is hashed by the router on the value the table stores, so a multi-row INSERT, an
#      INSERT ... SELECT and an ON CONFLICT insert place every row where the single node's key lands; a value that does
#      not fit fails with the single node's message.  Every group is read directly.
#   B. P2C-04: a NULL distribution key (fixed-width and varlena) in INSERT ... VALUES, INSERT ... SELECT and COPY.
#   C. P2C-02: the source cursor of the router is fetched in bytes: rows of a few hundred kilobytes keep the peak of
#      the origin far below insert_batch_rows times the row width.
#   D. P2C-07: the refusals of the router, each with its message.
# P2C-05 and P2C-06 (a replay of the text of the local part; a node that does not answer ROLLBACK TO SAVEPOINT) are not tested here.

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use IPC::Run ();
use KingresShardTest qw(run_sql);
use KingresShardCompare;

my $c = KingresShardCompare->new('rv');
my ($n1, $n2, $n3) = $c->nodes;
my $single = $c->single;
my @nodes = ($n1, $n2, $n3);

sub table
{
	my ($create, $dist) = @_;
	$c->on_cluster($create . ($dist // ''));
	$c->on_single($create);
}

# every row of the table is on the group that owns the bucket of its key, once, and the groups hold what the twin holds
sub groups_hold
{
	my ($t, $col, $label) = @_;
	my ($rows, $wrong) = (0, 0);
	my $i = 0;
	for my $nd (@nodes)
	{
		my $total = $nd->safe_psql('postgres', "SET kingres_shard.enable_routing = off; SELECT count(*) FROM $t");
		my $mine = $nd->safe_psql('postgres', qq{SET kingres_shard.enable_routing = off;
			SELECT count(*) FROM $t k JOIN kingres.bucket_map m ON m.colocation_id = (SELECT colocation_id FROM kingres.dist_table WHERE relid = '$t'::regclass)
			 AND m.bucket = kingres.bucket_for('$t'::regclass, k.$col) WHERE m.group_id = $i});
		$rows += $total;
		$wrong += $total - $mine;
		$i++;
	}
	is($wrong, 0, "$label: every row is in the group that owns its bucket");
	is($rows, $single->safe_psql('postgres', "SELECT count(*) FROM $t"), "$label: the groups hold the twin's number of rows");
}

# ---------------------------------------------------------------------------
# A. keys with a type modifier
# ---------------------------------------------------------------------------
my @types = (
	[ 'ts0',  'timestamp(0)',  sub { "'2024-03-10 11:30:0$_[0].7'" }, sub { q{'2024-03-10 12:30:00'::timestamp + (g || ' sec')::interval + interval '0.7 sec'} } ],
	[ 'tz0',  'timestamptz(0)', sub { "'2024-03-10 11:30:0$_[0].7+00'" }, sub { q{'2024-03-10 12:30:00+00'::timestamptz + (g || ' sec')::interval + interval '0.7 sec'} } ],
	[ 'n82',  'numeric(8,2)',  sub { "$_[0].005" }, sub { q{g + 0.005} } ],
	[ 'ch5',  'char(5)',       sub { "'a$_[0]'" }, sub { q{'b' || g} } ],
	[ 'vc8',  'varchar(8)',    sub { "'k$_[0]'" }, sub { q{'m' || g} } ],
	[ 'tm0',  'time(0)',       sub { "'10:00:0$_[0].6'" }, sub { q{'11:00:00'::time + (g || ' sec')::interval + interval '0.6 sec'} } ],
);
for my $ty (@types)
{
	my ($name, $type, $lit, $gen) = @$ty;
	my $t = "public.mk_$name";
	table("CREATE TABLE $t (k $type, v text)", "; SELECT kingres.create_distributed_table('$t', 'k', bucket_count => 12)");
	$c->compare_dml("$name: multi-row VALUES", "INSERT INTO $t VALUES " . join(', ', map { '(' . $lit->($_) . ", 'v$_')" } (1 .. 8)));
	$c->compare_dml("$name: INSERT SELECT", "INSERT INTO $t SELECT " . $gen->() . ", 's' FROM generate_series(1, 20) g");
	$c->compare_tables($t, order_by => '1, 2', name => "$name: the rows");
	groups_hold($t, 'k', $name);
}
{
	# ON CONFLICT with a key that the modifier rounds
	table(q{CREATE TABLE public.mk_oc (k numeric(8,2) PRIMARY KEY, v text)}, q{; SELECT kingres.create_distributed_table('public.mk_oc', 'k', bucket_count => 12)});
	$c->compare_dml('numeric(8,2): ON CONFLICT DO NOTHING', q{INSERT INTO public.mk_oc VALUES (1.234, 'a'), (1.231, 'dup'), (2.345, 'b'), (3.456, 'c'), (4.567, 'd'), (4.569, 'dup') ON CONFLICT DO NOTHING});
	$c->compare_tables('public.mk_oc', order_by => '1', name => 'numeric(8,2): ON CONFLICT: the rows');
	# a value that does not fit fails with the message of a single node, and nothing is stored
	table(q{CREATE TABLE public.mk_len (k varchar(5), v text)}, q{; SELECT kingres.create_distributed_table('public.mk_len', 'k', bucket_count => 12)});
	$c->compare_dml('varchar(5): too long', q{INSERT INTO public.mk_len VALUES ('abc', 'a'), ('abcdefgh', 'b'), ('x', 'c')},
		expect_error => qr/value too long for type character varying\(5\)/, single_fails => 1);
	table(q{CREATE TABLE public.mk_prec (k numeric(4,1), v text)}, q{; SELECT kingres.create_distributed_table('public.mk_prec', 'k', bucket_count => 12)});
	$c->compare_dml('numeric(4,1): over precision', q{INSERT INTO public.mk_prec VALUES (1.5, 'a'), (12345.5, 'b'), (2.5, 'c')},
		expect_error => qr/numeric field overflow/, single_fails => 1);
	is($n1->safe_psql('postgres', 'SELECT count(*) FROM public.mk_len') + $n1->safe_psql('postgres', 'SELECT count(*) FROM public.mk_prec'), 0, 'nothing of the failed statements is stored');
}

# ---------------------------------------------------------------------------
# B. a NULL distribution key
# ---------------------------------------------------------------------------
for my $ty (['int', 'int'], ['text', 'text'])
{
	my ($name, $type) = @$ty;
	my $t = "public.nk_$name";
	table("CREATE TABLE $t (k $type, v int)", "; SELECT kingres.create_distributed_table('$t', 'k', bucket_count => 12)");
	my $v = $type eq 'int' ? sub { $_[0] } : sub { "'t$_[0]'" };
	$c->compare_dml("$name: NULL keys in VALUES", "INSERT INTO $t VALUES (NULL, 1), (" . $v->(1) . ", 2), (NULL, 3), (" . $v->(2) . ", 4), (" . $v->(3) . ", 5)");
	$c->compare_dml("$name: NULL keys in INSERT SELECT", "INSERT INTO $t SELECT CASE WHEN g % 3 = 0 THEN NULL ELSE " . ($type eq 'int' ? 'g' : "'q' || g") . " END, g FROM generate_series(10, 40) g");
	my ($cc, $cs) = $c->copy_in_both($t, "\\N\t100\n" . ($type eq 'int' ? "7" : "tcopy") . "\t101\n\\N\t102\n" . ($type eq 'int' ? "8" : "tcopy2") . "\t103\n");
	is($cc->[0], 0, "$name: COPY with NULL keys") or diag($cc->[2]);
	$c->compare_tables($t, order_by => '2', name => "$name: the rows with NULL keys");
	is($n1->safe_psql('postgres', "SELECT count(*) FROM $t WHERE k IS NULL"), $single->safe_psql('postgres', "SELECT count(*) FROM $t WHERE k IS NULL"), "$name: as many NULL keys as the twin");
	my $holders = 0;
	for my $nd (@nodes) { $holders++ if $nd->safe_psql('postgres', "SET kingres_shard.enable_routing = off; SELECT count(*) FROM $t WHERE k IS NULL") > 0; }
	is($holders, 1, "$name: every row with a NULL key is in one group (the group of bucket 0)");
}

# ---------------------------------------------------------------------------
# C. rows of a few hundred kilobytes
# ---------------------------------------------------------------------------
SKIP:
{
	skip 'needs /proc', 3 unless -r '/proc/self/status';
	table(q{CREATE TABLE public.wide (id int PRIMARY KEY, v text)}, q{; SELECT kingres.create_distributed_table('public.wide', 'id', bucket_count => 12)});
	my $b = $n1->background_psql('postgres', on_error_stop => 1);
	my $pid = $b->query_safe('SELECT pg_backend_pid()');
	chomp $pid;
	my $hwm = sub { open(my $fh, '<', "/proc/$pid/status") or return -1; while (<$fh>) { return $1 if /^VmHWM:\s+(\d+) kB/ } return -1; };
	$b->query_safe(q{SET kingres_shard.insert_batch_bytes = '1MB'});
	$b->query_safe(q{SELECT count(*) FROM public.wide});
	my $before = $hwm->();
	$b->query_safe(q{INSERT INTO public.wide SELECT g, repeat('x', 300000) FROM generate_series(1, 600) g});
	my $after = $hwm->();
	is($n1->safe_psql('postgres', 'SELECT count(*) FROM public.wide'), 600, 'the wide rows are stored');
	cmp_ok($after - $before, '<', 90 * 1024, "the peak of the origin does not follow insert_batch_rows times the row width (grew by " . ($after - $before) . " kB; 600 rows are 180 MB)");
	$b->quit;
	$b = $n1->background_psql('postgres', on_error_stop => 1);
	$b->query_safe(q{INSERT INTO public.wide SELECT 1000 + g, 'n' || g FROM generate_series(1, 5000) g});
	is($n1->safe_psql('postgres', 'SELECT count(*) FROM public.wide'), 5600, 'narrow rows after the wide ones are fetched in larger steps and are stored');
	$b->quit;
}

# ---------------------------------------------------------------------------
# D. the refusals of the router
# ---------------------------------------------------------------------------
{
	table(q{CREATE TABLE public.rf (id int PRIMARY KEY, v text)}, q{; SELECT kingres.create_distributed_table('public.rf', 'id', bucket_count => 12)});
	$c->on_both(q{INSERT INTO public.rf SELECT g, 'v' || g FROM generate_series(1, 30) g});
	table(q{CREATE TABLE public.rf_dst (id int PRIMARY KEY, v text)}, q{; SELECT kingres.create_distributed_table('public.rf_dst', 'id', colocate_with => 'public.rf')});
	$_->safe_psql('postgres', q{CREATE ROLE rf_w LOGIN; CREATE ROLE rf_o LOGIN; GRANT USAGE ON SCHEMA public TO rf_w, rf_o}) for @nodes;
	$n1->safe_psql('postgres', q{
		CREATE VIEW public.rf_own AS SELECT id + 1000 AS id, v FROM public.rf;
		CREATE VIEW public.rf_bar WITH (security_barrier = true) AS SELECT id + 2000 AS id, v FROM public.rf;
		ALTER VIEW public.rf_own OWNER TO rf_o; ALTER VIEW public.rf_bar OWNER TO rf_o;
		GRANT SELECT ON public.rf_own, public.rf_bar TO rf_w; GRANT SELECT, INSERT ON public.rf_dst TO rf_w});
	my $as = sub {
		my ($sql) = @_;
		my ($out, $err) = ('', '');
		my $h = IPC::Run::start([ 'psql', '-XAt', '-v', 'ON_ERROR_STOP=1', '-h', $n1->host, '-p', $n1->port, '-d', 'postgres', '-U', 'rf_w', '-c', $sql ], '<', \my $in, '>', \$out, '2>', \$err);
		$h->finish;
		return ($h->result(0), $out, $err);
	};
	my ($rc, $out, $err) = $as->(q{INSERT INTO public.rf_dst SELECT id, v FROM public.rf_own});
	isnt($rc, 0, 'a source that is a view of another owner is refused');
	like($err, qr/through a view owned by role/, '... with its message');
	($rc, $out, $err) = $as->(q{INSERT INTO public.rf_dst SELECT id, v FROM public.rf_bar});
	isnt($rc, 0, 'a source that is a security barrier view is refused');
	like($err, qr/security barrier view/, '... with its message');
	($rc, $out, $err) = run_sql($n1, q{BEGIN READ ONLY; INSERT INTO public.rf_dst VALUES (101, 'a'), (102, 'b'), (103, 'c'), (104, 'd'); COMMIT;});
	isnt($rc, 0, 'a multi-row INSERT in a read-only transaction is refused');
	like($err, qr/read-only transaction/, '... with the message of PostgreSQL');
	my ($crc, $cout, $cerr) = KingresShardCompare::copy_in($n1, 'public.rf_dst', "201\tx\n", options => 'FORMAT text');
	is($crc, 0, 'COPY works (the control of the refusals above)');
	is($n1->safe_psql('postgres', q{SELECT count(*) FROM public.rf_dst WHERE id IN (101, 102, 103, 104)}), 0, 'the refused statements stored nothing');
}

done_testing();
