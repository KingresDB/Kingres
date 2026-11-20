# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/161_pushdown_ledger.pl
#
# The design's 068_pushdown_ledger (design/shard-phase2b.md, 11.8 and 16.2): kingres_shard and kingres_ledger loaded together.
# The ledger tables of a node are local tables of that node (a ledger table is never distributed: t/142).  They are written
# from the groups' data by INSERT ... SELECT.  The source of an INSERT ... SELECT is read by the 2a Kingres Distributed Scan, not by
# the fragment paths of phase 2b (design 6.1 and 18: it is planned as a subquery), so section A tests the ledger with the 2a scan,
# and section A2 tests the ledger with the fragment paths: the same statements run inside a function (RETURN QUERY plans a statement
# of its own at the top level, where the partial aggregation, the ordered merge and the closed query are shipped) and feed the ledger
# table (review P2F-04); the log of a group shows that the fragments were sent.  The ledger's guarantees must not change:
#
#   A. INSERT INTO <ledger table> SELECT ... FROM <distributed table> from every node, for an append-only and an updatable
#      table, with a source that is a scan, a partial aggregation, an ordered merge with a limit and a closed aggregate: every
#      row is captured exactly once (the table holds each row once; the history of the updatable table holds each insert once),
#      ledger_verify() reports no problem;
#   A2. the same through functions, so that the fragment paths are under the ledger: one entry per row, none twice;
#   B. an UPDATE and a DELETE of an append-only table are still refused, an UPDATE of the updatable one is recorded;
#   C. a COPY into a distributed table by a role that also owns a ledger table does not touch that ledger table, and the
#      ledger stays consistent.
#
# This test needs contrib/kingres_ledger built and installed into the same installation (as t/142 does); both libraries are
# preloaded.

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use IPC::Run ();
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(make_cluster run_sql prepared_count);

my @nodes = make_cluster('pl', 3, extra => "shared_preload_libraries = 'kingres_shard,kingres_ledger'\n"
	. "kingres_ledger.block_interval = 0\n"
	. "kingres_shard.enable_partial_aggregation = on\nkingres_shard.enable_pushdown_sort = on\nlog_statement = 'all'\n");
my ($n1) = @nodes;
$_->safe_psql('postgres', 'CREATE ROLE lrole LOGIN') for @nodes;
$n1->safe_psql('postgres', q{
	CREATE TABLE public.dist (id int, k int, g int, v text, PRIMARY KEY (id, k));
	SELECT kingres.create_distributed_table('public.dist', 'id', bucket_count => 12);
	INSERT INTO public.dist SELECT i, k, i % 7, 'v' || (i % 41)::text FROM generate_series(1, 300) i, generate_series(1, 10) k;
	GRANT SELECT, INSERT ON public.dist TO lrole;
});
$_->safe_psql('postgres', 'ANALYZE') for @nodes;
for my $n (@nodes)
{
	$n->safe_psql('postgres', q{
		CREATE EXTENSION kingres_ledger;
		CREATE TABLE public.led_a (kind int, id int, v text);
		CREATE TABLE public.led_u (kind int, id int, v text);
		CREATE TABLE public.led_own (kind int, id int, v text);
		CREATE TABLE public.led_f (kind int, id int, v text);
		CREATE TABLE public.led_fu (kind int, id int, v text);
		ALTER TABLE public.led_own OWNER TO lrole;
		SELECT kingres.ledger_enable('public.led_a', 'append_only');
		SELECT kingres.ledger_enable('public.led_u', 'updatable');
		SELECT kingres.ledger_enable('public.led_own', 'append_only');
		SELECT kingres.ledger_enable('public.led_f', 'append_only');
		SELECT kingres.ledger_enable('public.led_fu', 'updatable');
	});
}

# the sources: one per path of phase 2b; the plan text proves that the path was taken
my @src = (
	[ 'a scan',                    q{SELECT 1, id, v FROM public.dist WHERE k = 1 AND id <= 30}, undef, 30 ],
	[ 'a partial aggregation',     q{SELECT 2, g, count(*)::text FROM public.dist GROUP BY g}, qr/Finalize/, 7 ],
	[ 'an ordered merge with a limit', q{SELECT 3, id, count(*) FROM public.dist GROUP BY id ORDER BY count(*) DESC, id LIMIT 20}, qr/Merge keys/, 20 ],
	[ 'a closed aggregate',        q{SELECT 4, id, max(v) FROM public.dist WHERE id <= 100 GROUP BY id}, qr/Pushed: closed/, 100 ],
);
my $EXPECT = 0;
$EXPECT += $_->[3] for @src;

sub plan_of
{
	my ($node, $sql) = @_;
	my ($rc, $out, $err) = run_sql($node, "EXPLAIN (COSTS OFF) $sql");
	return $rc == 0 ? $out : "ERROR: $err";
}

my %problems_before;
for my $n (@nodes)
{
	my @ver = $n->safe_psql('postgres', 'SELECT count(*) FROM kingres.ledger_verify()');
	$problems_before{ $n->name } = $ver[0];
	is($ver[0], 0, $n->name . ': the ledger verifies before anything is written');
}

# ---------------------------------------------------------------------------
# A. INSERT ... SELECT from the distributed table, from every node
# ---------------------------------------------------------------------------
for my $n (@nodes)
{
	my $nm = $n->name;
	for my $s (@src)
	{
		my ($what, $sql, $path) = @$s;
		like(plan_of($n, $sql), $path, "$nm: the source '$what' is shipped as that path") if defined $path;
		note("the plan of INSERT ... SELECT <$what>: " . join(' / ', grep { length } map { s/^\s+//r } split(/->/, plan_of($n, "INSERT INTO public.led_a $sql")))) if $n == $nodes[0];
		for my $t (qw(led_a led_u))
		{
			my ($rc, $out, $err) = run_sql($n, "INSERT INTO public.$t $sql");
			is($rc, 0, "$nm: INSERT INTO $t SELECT <$what> runs") or diag($err);
		}
	}
	for my $t (qw(led_a led_u))
	{
		is($n->safe_psql('postgres', "SELECT count(*) FROM public.$t"), $EXPECT, "$nm: $t holds $EXPECT rows");
		is($n->safe_psql('postgres', "SELECT count(DISTINCT (kind, id)) FROM public.$t"), $EXPECT, "$nm: and each of them once");
	}
	for my $t (qw(led_a led_u))
	{
		is($n->safe_psql('postgres', "SELECT count(*) || '/' || count(DISTINCT (e.ledger_txid, e.seq)) || '/' || count(DISTINCT e.row_hash) FROM kingres.ledger_entry e JOIN kingres.ledger_table lt USING (table_id) WHERE lt.relid = 'public.$t'::regclass"),
			"$EXPECT/$EXPECT/$EXPECT", "$nm: the ledger holds one entry for each row of $t, none twice");
	}
	is($n->safe_psql('postgres', 'SELECT count(*) FROM kingres.ledger_verify()'), 0, "$nm: ledger_verify() reports no problem");
}

# ---------------------------------------------------------------------------
# A2. the same through functions: the fragment paths under the ledger (review P2F-04)
# ---------------------------------------------------------------------------
{
	# [ function, the types it returns, the statement, the marker that a group's log must show, the rows ]
	my @fn = (
		[ 'f_scan',    'int, int, text',   q{SELECT 1, id, v FROM public.dist WHERE k = 1 AND id <= 30}, undef, 30 ],
		[ 'f_partial', 'int, int, bigint', q{SELECT 2, g, count(*) FROM public.dist GROUP BY g}, 'kingres:partial-fragment', 7 ],
		[ 'f_merge',   'int, int, bigint', q{SELECT 3, id, count(*) FROM public.dist GROUP BY id ORDER BY count(*) DESC, id LIMIT 20}, 'kingres:query-fragment', 20 ],
		[ 'f_closed',  'int, int, text',   q{SELECT 4, id, max(v) FROM public.dist WHERE id <= 100 GROUP BY id}, 'kingres:query-fragment', 100 ],
	);
	my $total = 0;
	$total += $_->[4] for @fn;
	for my $n (@nodes)
	{
		for my $f (@fn)
		{
			my ($name, $types, $sql) = @$f;
			my @ty = split /, /, $types;
			my $cols = join(', ', map { "o$_ $ty[$_ - 1]" } (1 .. 3));
			$n->safe_psql('postgres', "CREATE FUNCTION public.$name() RETURNS TABLE ($cols) LANGUAGE plpgsql AS \$f\$ BEGIN RETURN QUERY $sql; END \$f\$");
		}
	}
	for my $n (@nodes)
	{
		my $nm = $n->name;
		my ($other) = grep { $_ != $n } @nodes;
		for my $f (@fn)
		{
			my ($name, $types, $sql, $marker, $rows) = @$f;
			my $off = -s $other->logfile;
			for my $t (qw(led_f led_fu))
			{
				my ($rc, $out, $err) = run_sql($n, "INSERT INTO public.$t SELECT o1, o2, o3::text FROM public.$name()");
				is($rc, 0, "$nm: INSERT INTO $t SELECT FROM $name() runs") or diag($err);
			}
			if (defined $marker)
			{
				my $log = PostgreSQL::Test::Utils::slurp_file($other->logfile, $off);
				my $seen = () = $log =~ /\Q$marker\E/g;
				cmp_ok($seen, '>=', 2, "$nm: a group received the fragment of $name() ($marker, seen $seen times): the pushdown path is under the ledger");
			}
		}
		for my $t (qw(led_f led_fu))
		{
			is($n->safe_psql('postgres', "SELECT count(*) || '/' || count(DISTINCT (kind, id)) FROM public.$t"), "$total/$total", "$nm: $t holds each row once");
			is($n->safe_psql('postgres', "SELECT count(*) || '/' || count(DISTINCT (e.ledger_txid, e.seq)) || '/' || count(DISTINCT e.row_hash) FROM kingres.ledger_entry e JOIN kingres.ledger_table lt USING (table_id) WHERE lt.relid = 'public.$t'::regclass"),
				"$total/$total/$total", "$nm: the ledger holds one entry for each row of $t, none twice");
		}
		is($n->safe_psql('postgres', 'SELECT count(*) FROM kingres.ledger_verify()'), 0, "$nm: ledger_verify() reports no problem");
	}
}

# ---------------------------------------------------------------------------
# B. the ledger still refuses what it refused
# ---------------------------------------------------------------------------
for my $n (@nodes)
{
	my $nm = $n->name;
	my ($rc, $out, $err) = run_sql($n, q{UPDATE public.led_a SET v = 'changed'});
	isnt($rc, 0, "$nm: an UPDATE of an append-only table is refused");
	($rc, $out, $err) = run_sql($n, q{DELETE FROM public.led_a});
	isnt($rc, 0, "$nm: a DELETE of an append-only table is refused");
	is($n->safe_psql('postgres', 'SELECT count(*) FROM public.led_a'), $EXPECT, "$nm: and the table is as it was");
	($rc, $out, $err) = run_sql($n, q{UPDATE public.led_u SET v = 'changed' WHERE kind = 1 AND id = 1});
	is($rc, 0, "$nm: an UPDATE of the updatable table is allowed") or diag($err);
	my $hist = $n->safe_psql('postgres', "SELECT history_relid::regclass::text FROM kingres.ledger_table WHERE relid = 'public.led_u'::regclass");
	is($n->safe_psql('postgres', "SELECT count(*) FROM $hist"), '1', "$nm: and the old version is recorded once in the history");
	is($n->safe_psql('postgres', 'SELECT count(*) FROM kingres.ledger_verify()'), 0, "$nm: ledger_verify() still reports no problem");
}

# ---------------------------------------------------------------------------
# C. COPY into a distributed table by a role that owns a ledger table
# ---------------------------------------------------------------------------
{
	my $i = 0;
	for my $n (@nodes)
	{
		my $nm = $n->name;
		my $base = 100000 + 100 * $i++;
		my $own_before = $n->safe_psql('postgres', 'SELECT count(*) FROM public.led_own');
		my $pending_before = $n->safe_psql('postgres', 'SELECT count(*) FROM kingres.ledger_pending');
		my $data = join('', map { ($base + $_) . "\t1\t" . ($_ % 7) . "\tcopied$_\n" } (1 .. 40));
		my ($out, $err) = ('', '');
		my $h = IPC::Run::start([ 'psql', '-XAtq', '-v', 'ON_ERROR_STOP=1', '-h', $n->host, '-p', $n->port, '-U', 'lrole', '-d', 'postgres', '-c', 'COPY public.dist (id, k, g, v) FROM STDIN' ],
			'<', \$data, '>', \$out, '2>', \$err);
		$h->finish;
		my $rc = $h->result(0);
		is($rc, 0, "$nm: COPY into the distributed table by the owner of a ledger table") or diag($err);
		is($n->safe_psql('postgres', "SELECT count(*) FROM public.dist WHERE id > $base AND id <= $base + 40"), 40, "$nm: its rows are in the table");
		is($n->safe_psql('postgres', 'SELECT count(*) FROM public.led_own'), $own_before, "$nm: the ledger table of the role is not touched");
		is($n->safe_psql('postgres', 'SELECT count(*) FROM kingres.ledger_pending'), $pending_before, "$nm: and nothing of the COPY was queued in the ledger");
		is($n->safe_psql('postgres', 'SELECT count(*) FROM kingres.ledger_verify()'), 0, "$nm: ledger_verify() reports no problem");
	}
	is($n1->safe_psql('postgres', 'SELECT count(*) FROM public.dist'), 3000 + 120, 'the distributed table holds the rows of the three COPYs');
}

is(prepared_count($_), 0, 'nothing is prepared on ' . $_->name) for @nodes;

done_testing();
