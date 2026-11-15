# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/024_low_findings.pl
#
# Review findings S2-17 to S2-20:
#   S2-17  the batches of a scan that an error ended are freed;
#   S2-18  UPDATE ... FROM a distributed table says what is unsupported;
#   S2-19  a routed SELECT that calls a volatile function is a writer of the
#          distributed transaction (it is prepared, not committed first);
#   S2-20  add_node compares encoding and collation, a prepared transaction of a
#          user whose name merely starts with kingres_ is left alone.

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(make_cluster make_node keys_of_group run_sql prepared_count);

my ($n1, $n2, $n3) = make_cluster('lf', 3, extra => "max_connections = 50\n");
my @nodes = ($n1, $n2, $n3);
$n1->safe_psql('postgres', q{
	CREATE TABLE public.big (id int PRIMARY KEY, pad text);
	CREATE TABLE public.d (id int PRIMARY KEY, v text);
	CREATE TABLE public.lw (k int PRIMARY KEY, lab text);
	SELECT kingres.create_distributed_table('public.big', 'id', bucket_count => 12);
	SELECT kingres.create_distributed_table('public.d', 'id', bucket_count => 12);
});
$_->safe_psql('postgres', q{
	CREATE TABLE IF NOT EXISTS public.lw (k int PRIMARY KEY, lab text);
	CREATE TABLE public.wlog (i int);
	CREATE FUNCTION public.wr(x int) RETURNS int LANGUAGE plpgsql VOLATILE AS $$
	BEGIN INSERT INTO public.wlog VALUES (x); RETURN x; END $$;
}) for @nodes;
$n1->safe_psql('postgres', 'SELECT kingres.recover_prepared_transactions()');

# ---------------------------------------------------------------------------
# S2-17: an aborted scan does not leak its batches.
# ---------------------------------------------------------------------------
{
	for my $i (1 .. 90)
	{
		$n1->safe_psql('postgres', "INSERT INTO public.big VALUES ($i, repeat('x', 200000))");
	}
	my $bc = $n1->safe_psql('postgres', q{SELECT colocation_id FROM kingres.dist_table WHERE relid = 'public.big'::regclass});
	my $kerr = (keys_of_group($n1, $bc, 1, 3, 1))[2];	# a row of another group, read after others
	my $s = $n1->background_psql('postgres', on_error_stop => 0);
	my $pid = $s->query('SELECT pg_backend_pid()');
	chomp $pid;
	$s->query('SET kingres_shard.scan_fetch_size = 30');
	$s->query('SELECT count(*) FROM public.big');	# warm up
	my $rss = sub { my ($v) = slurp_file("/proc/$pid/status") =~ /^VmRSS:\s+(\d+) kB/m; return $v; };
	my $before = $rss->();
	$s->query("SELECT id, pad, 1 / (id - $kerr) FROM public.big") for (1 .. 30);
	my $after = $rss->();
	cmp_ok($after - $before, '<', 150_000, "S2-17: 30 aborted scans of ~18 MB each leave the backend at +" . ($after - $before) . " kB");
	$s->quit;
}

# ---------------------------------------------------------------------------
# S2-18: the message.
# ---------------------------------------------------------------------------
{
	$n1->safe_psql('postgres', "INSERT INTO public.lw VALUES (3, 'x'); INSERT INTO public.d VALUES (3, 'd')");
	my ($rc, $out, $err) = run_sql($n1, q{UPDATE public.lw SET lab = d.v FROM public.d d WHERE d.id = lw.k});
	isnt($rc, 0, 'S2-18: UPDATE of a local table FROM a distributed table is refused');
	like($err, qr/UPDATE, DELETE and MERGE that read distributed table "d"/, 'S2-18: the message says what is unsupported');
	unlike($err, qr/system columns/, 'S2-18: and does not talk about system columns');
}

# ---------------------------------------------------------------------------
# S2-19: a routed SELECT with a volatile function is a writer.
# ---------------------------------------------------------------------------
{
	my $coloc = $n1->safe_psql('postgres', q{SELECT colocation_id FROM kingres.dist_table WHERE relid = 'public.d'::regclass});
	my ($ka) = keys_of_group($n1, $coloc, 0, 1, 1000);
	my ($kb) = keys_of_group($n1, $coloc, 1, 1, 1000);
	my ($kc) = keys_of_group($n1, $coloc, 2, 1, 1000);
	$n1->safe_psql('postgres', "INSERT INTO public.d VALUES ($kb, 'b')");
	my ($rc, $out, $err) = run_sql($n1,
		"SET kingres_shard.fault_injection = 'after_prepare:error';\n"
		  . "BEGIN; SELECT public.wr(1) FROM public.d WHERE id = $kb; INSERT INTO public.d VALUES ($kc, 'c'); INSERT INTO public.d VALUES ($ka, 'a'); COMMIT;");
	like($err, qr/injected failure at after_prepare/, 'the commit fails after the prepare');
	is($n2->safe_psql('postgres', 'SELECT count(*) FROM public.wlog'), '0',
		'S2-19: the write of the volatile function on node 2 was prepared and rolled back with the rest');
	is($n3->safe_psql('postgres', "SELECT count(*) FROM public.d WHERE id = $kc"), '0', 'and so was the insert on node 3');
	is(prepared_count($_), '0', 'nothing is prepared on ' . $_->name) for @nodes;

	# Without the failure the transaction commits and the write is there.
	($rc, $out, $err) = run_sql($n1,
		"BEGIN; SELECT public.wr(2) FROM public.d WHERE id = $kb; INSERT INTO public.d VALUES ($kc, 'c'); INSERT INTO public.d VALUES ($ka, 'a'); COMMIT;");
	is($err, '', 'the same transaction commits');
	is($n2->safe_psql('postgres', 'SELECT count(*) FROM public.wlog'), '1', 'with the write of the function');
}

# ---------------------------------------------------------------------------
# S2-20
# ---------------------------------------------------------------------------
{
	# A node with another encoding is not added.
	my $nl = PostgreSQL::Test::Cluster->new('lfl');
	$nl->init(extra => [ '--encoding=LATIN1', '--locale=C' ]);
	$nl->append_conf('postgresql.conf', "shared_preload_libraries = 'kingres_shard'\nmax_prepared_transactions = 20\n");
	$nl->start;
	my ($rc, $out, $err) = run_sql($n1, sprintf("SELECT kingres.add_node('%s', %d)", $nl->host, $nl->port));
	isnt($rc, 0, 'S2-20: a node with another encoding is not added');
	like($err, qr/differs from this node in default collation or its library version/, 'S2-20: with a message that says why');
	is($n1->safe_psql('postgres', 'SELECT count(*) FROM kingres.node'), '3', 'and is not registered');
	is(prepared_count($nl), '0', 'nothing is left prepared on it');
	$nl->stop;

	# S2-10: a default that PostgreSQL turns into the time of the ALTER on each node.
	$n1->safe_psql('postgres', q{CREATE TABLE public.r2 (id int PRIMARY KEY); INSERT INTO public.r2 VALUES (1); SELECT kingres.create_reference_table('public.r2')});
	for my $special ('now', 'Today', ' tomorrow ', 'yesterday')
	{
		($rc, $out, $err) = run_sql($n1, "ALTER TABLE public.r2 ADD COLUMN ts timestamptz DEFAULT '$special'");
		isnt($rc, 0, "S2-10: ADD COLUMN ... DEFAULT '$special' is refused");
		like($err, qr/can mean another value on each node/, "S2-10: with the reason");
	}
	($rc, $out, $err) = run_sql($n1, "ALTER TABLE public.r2 ADD COLUMN ts timestamptz DEFAULT '2024-01-01 00:00:00+00', ADD COLUMN ts2 timestamptz DEFAULT 'infinity'");
	is($err, '', 'a fixed date default is accepted');

	# A prepared transaction of a user whose name starts with "kingres_" is not ours.
	$n2->safe_psql('postgres', q{BEGIN; INSERT INTO public.wlog VALUES (99); PREPARE TRANSACTION 'kingres_1_x_y_z'});
	$n1->safe_psql('postgres', 'SELECT kingres.recover_prepared_transactions()');
	is(prepared_count($n2), '1', 'S2-20: recovery leaves a user transaction named kingres_1_x_y_z alone');
	$n2->safe_psql('postgres', q{ROLLBACK PREPARED 'kingres_1_x_y_z'});
}

done_testing();
