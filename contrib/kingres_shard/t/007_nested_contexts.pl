# SPDX-License-Identifier: PostgreSQL
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/007_nested_contexts.pl
#   Review 2026-11-14, S-D ("Any PL..."): statements that reach a distributed
#   table from inside functions and other nested contexts are routed or
#   refused, never answered from the local partitions only; plus the two
#   execution modes the review did not look at: parallel workers and a hot
#   standby.  Everything runs on a three-node cluster from node 1.

use strict;
use warnings FATAL => 'all';
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;

sub make_node
{
	my ($name) = @_;
	my $node = PostgreSQL::Test::Cluster->new($name);
	$node->init(allows_streaming => 1);
	$node->append_conf('postgresql.conf',
		"shared_preload_libraries = 'kingres_shard'");
	$node->start;
	return $node;
}

my @nodes = map { make_node("f$_") } (1 .. 3);
my ($n1, $n2, $n3) = @nodes;

$n1->safe_psql('postgres', 'CREATE EXTENSION kingres_shard');
$n1->safe_psql('postgres',
	sprintf("SELECT kingres.init_cluster('%s', %d)", $n1->host, $n1->port));
for my $n ($n2, $n3)
{
	$n1->safe_psql('postgres',
		sprintf("SELECT kingres.add_node('%s', %d)", $n->host, $n->port));
}
$n1->safe_psql('postgres', q{
	CREATE TABLE public.kvn (id int PRIMARY KEY, val text);
	CREATE TABLE public.ptab (x int);
	SELECT kingres.create_distributed_table('public.kvn', 'id', bucket_count => 12);
});

my %key;
for my $g (0 .. 2)
{
	my $list = $n1->safe_psql('postgres', qq{
		SELECT string_agg(k::text, ',' ORDER BY k) FROM (
		  SELECT k FROM generate_series(1, 1000) k
		   WHERE (SELECT m.group_id FROM kingres.bucket_map m
		           WHERE m.colocation_id = 1
		             AND m.bucket = kingres.bucket_for(1, k)) = $g
		   LIMIT 3) s});
	$key{$g} = [ split /,/, $list ];
	$n1->safe_psql('postgres',
		"INSERT INTO public.kvn VALUES ($key{$g}[0], 'row-of-group-$g')");
}
my ($k0, $k1, $k2) = map { $key{$_}[0] } 0 .. 2;

sub run_sql
{
	my ($node, $sql, %extra) = @_;
	my ($stdout, $stderr);
	my $rc = $node->psql('postgres', $sql, stdout => \$stdout,
		stderr => \$stderr, %extra);
	return ($rc, $stdout, $stderr);
}

# Each case: the SQL, and either the exact output or a refusal.
sub refused
{
	my ($sql, $label) = @_;
	my ($rc, $out, $err) = run_sql($n1, $sql);
	like($err, qr/not supported yet|cannot copy from partitioned table/,
		"$label: refused");
	is($out, '', "$label: and no partial answer");
}

$n1->safe_psql('postgres', qq{
	-- inlined by the planner after the routing decision
	CREATE FUNCTION public.f_count() RETURNS bigint LANGUAGE sql STABLE
	  AS \$\$ SELECT (SELECT count(*) FROM public.kvn) \$\$;
	CREATE FUNCTION public.f_rows() RETURNS SETOF public.kvn LANGUAGE sql STABLE
	  AS \$\$ SELECT * FROM public.kvn \$\$;
	CREATE FUNCTION public.f_by_id(k int) RETURNS SETOF public.kvn LANGUAGE sql STABLE
	  AS \$\$ SELECT * FROM public.kvn WHERE id = k \$\$;
	-- not inlined
	CREATE FUNCTION public.p_count() RETURNS bigint LANGUAGE plpgsql
	  AS \$\$ BEGIN RETURN (SELECT count(*) FROM public.kvn); END \$\$;
	CREATE FUNCTION public.p_by_id(k int) RETURNS text LANGUAGE plpgsql
	  AS \$\$ BEGIN RETURN (SELECT val FROM public.kvn WHERE id = k); END \$\$;
	CREATE FUNCTION public.p_par(k int) RETURNS text LANGUAGE plpgsql STABLE PARALLEL SAFE
	  AS \$\$ BEGIN RETURN (SELECT val FROM public.kvn WHERE id = k); END \$\$;
	CREATE FUNCTION public.ptab_trg() RETURNS trigger LANGUAGE plpgsql
	  AS \$\$ BEGIN NEW.x := (SELECT length(val) FROM public.kvn WHERE id = NEW.x); RETURN NEW; END \$\$;
	CREATE TRIGGER ptab_i BEFORE INSERT ON public.ptab FOR EACH ROW EXECUTE FUNCTION public.ptab_trg();
});

# ---------------------------------------------------------------------------
# Refused, with no silent partial answer.
# ---------------------------------------------------------------------------
refused("SELECT public.f_count()", 'S-D: an inlined SQL function that counts the table');
refused("SELECT count(*) FROM public.f_rows()", 'S-D: an inlined set-returning SQL function');
refused("SELECT * FROM public.f_by_id($k1)",
	'S-D: an inlined SQL function that reads a remote key');
refused("SELECT * FROM public.f_by_id($k0)",
	'S-D: an inlined SQL function that reads a local key');
refused("SELECT public.p_count()", 'S-D: a PL/pgSQL function that counts the table');
refused("DO \$\$ BEGIN PERFORM count(*) FROM public.kvn; END \$\$", 'S-D: a DO block without a key');
refused("DO \$\$ BEGIN EXECUTE 'DELETE FROM public.kvn'; END \$\$", 'S-D: EXECUTE of a DELETE without a key');
refused("CREATE TABLE public.ctas_bad AS SELECT * FROM public.kvn", 'S-D: CREATE TABLE AS without a key');
refused("COPY (SELECT * FROM public.kvn) TO STDOUT", 'S-D: COPY (query) TO without a key');
refused("COPY public.kvn TO STDOUT", 'S-D: COPY table TO');
refused("EXPLAIN (ANALYZE, COSTS OFF) SELECT * FROM public.kvn", 'S-D: EXPLAIN ANALYZE without a key');
refused("BEGIN; DECLARE c CURSOR FOR SELECT * FROM public.kvn; FETCH 1 FROM c; COMMIT;", 'S-D: a cursor without a key');
{
	my ($rc, $out, $err) = run_sql($n1, "SELECT public.f_count()");
	unlike($out, qr/^\d+$/, 'S-D: the inlined count does not return the count of the local partitions');
}

# ---------------------------------------------------------------------------
# Routed to the owner of the key.
# ---------------------------------------------------------------------------
is($n1->safe_psql('postgres', "SELECT public.p_by_id($k1)"), 'row-of-group-1',
	'S-D: a PL/pgSQL function reads a remote key');
is($n1->safe_psql('postgres', "SELECT public.p_by_id($k2)"), 'row-of-group-2',
	'S-D: and another remote key');
is($n1->safe_psql('postgres',
		"DO \$\$ BEGIN UPDATE public.kvn SET val = 'by-do' WHERE id = $k1; END \$\$;
		 SELECT val FROM public.kvn WHERE id = $k1"),
	'by-do', 'S-D: a DO block updates a remote key');
is($n2->safe_psql('postgres', "SELECT val FROM public.kvn WHERE id = $k1"), 'by-do',
	'S-D: the owner has the update');
$n1->safe_psql('postgres', "UPDATE public.kvn SET val = 'row-of-group-1' WHERE id = $k1");
is($n1->safe_psql('postgres',
		"DO \$\$ DECLARE r text; BEGIN EXECUTE 'SELECT val FROM public.kvn WHERE id = $k2' INTO r; RAISE NOTICE 'got %', r; END \$\$"),
	'', 'S-D: EXECUTE of a statement with a remote key runs');
{
	my ($rc, $out, $err) = run_sql($n1,
		"DO \$\$ DECLARE r text; BEGIN EXECUTE 'SELECT val FROM public.kvn WHERE id = $k2' INTO r; RAISE NOTICE 'got %', r; END \$\$");
	like($err, qr/got row-of-group-2/, 'S-D: and returns the remote row');
}
is($n1->safe_psql('postgres', "INSERT INTO public.ptab VALUES ($k2) RETURNING x"),
	length('row-of-group-2'), 'S-D: a trigger reads a remote key');
is($n1->safe_psql('postgres',
		"CREATE TABLE public.ctas_ok AS SELECT * FROM public.kvn WHERE id = $k2;
		 SELECT val FROM public.ctas_ok"),
	'row-of-group-2', 'S-D: CREATE TABLE AS with a remote key');
{
	my ($rc, $out, $err) = run_sql($n1,
		"COPY (SELECT val FROM public.kvn WHERE id = $k2) TO STDOUT");
	is($out, 'row-of-group-2', 'S-D: COPY (query) TO with a remote key');
}
{
	my @want = ('row-of-group-1') x 8;
	my $script = "PREPARE q (int) AS SELECT val FROM public.kvn WHERE id = \$1;\n"
	  . join('', map { "EXECUTE q($k1);\n" } 1 .. 8);
	is(join(',', split /\n/, $n1->safe_psql('postgres', $script)), join(',', @want),
		'S-D: a prepared statement with a remote key under plan_cache_mode auto');
	my ($rc, $out, $err) = run_sql($n1, "SET plan_cache_mode = force_custom_plan;\n" . $script);
	is($err, '', 'S-D: and under force_custom_plan');
	($rc, $out, $err) = run_sql($n1, "SET plan_cache_mode = force_generic_plan;\n"
		  . "PREPARE g (int) AS SELECT val FROM public.kvn WHERE id = \$1;\nEXECUTE g($k1);");
	like($err, qr/generic plan cannot route/, 'S-D: and is refused under force_generic_plan');
	is($out, '', 'S-D: with no row');
}

# ---------------------------------------------------------------------------
# A parallel worker does not forward a statement.
# ---------------------------------------------------------------------------
{
	my ($rc, $out, $err) = run_sql($n1,
		"SET debug_parallel_query = on;\nSELECT public.p_par($k0);");
	is($out, 'row-of-group-0', 'a parallel worker runs a statement for a local bucket');
	($rc, $out, $err) = run_sql($n1,
		"SET debug_parallel_query = on;\nSELECT public.p_par($k1);");
	like($err, qr/parallel worker/, 'a parallel worker refuses to forward a statement');
	is($out, '', 'and returns nothing');
}

# ---------------------------------------------------------------------------
# A hot standby of the map group primary.
# ---------------------------------------------------------------------------
{
	$n1->backup('standby_bk');
	my $s = PostgreSQL::Test::Cluster->new('fs');
	$s->init_from_backup($n1, 'standby_bk', has_streaming => 1);
	$s->start;
	$n1->wait_for_catchup($s);

	is($s->safe_psql('postgres', "SELECT val FROM public.kvn WHERE id = $k0"), 'row-of-group-0',
		'a hot standby reads a key of its own group');
	is($s->safe_psql('postgres', "SELECT val FROM public.kvn WHERE id = $k1"), 'row-of-group-1',
		'a hot standby forwards a read for another group');
	my ($rc, $out, $err) = run_sql($s, "INSERT INTO public.kvn VALUES ($k2 + 1000000, 'x')");
	like($err, qr/read-only/, 'a write on a hot standby is refused');
	($rc, $out, $err) = run_sql($s, "SELECT * FROM public.kvn");
	like($err, qr/span shard groups/, 'a statement without a key is refused on a hot standby');
	$s->stop;
}

foreach my $n (@nodes)
{
	$n->stop;
}

done_testing();
