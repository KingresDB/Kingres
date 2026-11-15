# SPDX-License-Identifier: PostgreSQL
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/004_security.pl
#   Review 2026-11-14: S-B (the stale-map reload runs as the bootstrap
#   superuser inside a user's session and must not resolve names through the
#   session's search_path or temporary schema), finding 1 (a forwarded session
#   never turns a wrong answer into a silent one, and the flag is not for
#   ordinary sessions) and the routing switch.

use strict;
use warnings FATAL => 'all';
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;

sub make_node
{
	my ($name) = @_;
	my $node = PostgreSQL::Test::Cluster->new($name);
	$node->init;
	$node->append_conf('postgresql.conf',
		"shared_preload_libraries = 'kingres_shard'
max_prepared_transactions = 20
kingres_shard.recovery_interval = 0");
	$node->start;
	return $node;
}

my @nodes = map { make_node("s$_") } (1 .. 3);
my ($n1, $n2, $n3) = @nodes;

$n1->safe_psql('postgres', 'CREATE EXTENSION kingres_shard');
$n1->safe_psql('postgres',
	sprintf("SELECT kingres.init_cluster('%s', %d)", $n1->host, $n1->port));
for my $n ($n2, $n3)
{
	$n1->safe_psql('postgres',
		sprintf("SELECT kingres.add_node('%s', %d)", $n->host, $n->port));
}

# Unprivileged roles on every node (nodes connect as the current user).
for my $n (@nodes)
{
	$n->safe_psql('postgres', q{
		CREATE ROLE ks_user LOGIN;
		CREATE ROLE ks_victim LOGIN;
	});
}
$n1->safe_psql('postgres', q{
	CREATE TABLE public.kv (id int PRIMARY KEY, val text);
	SELECT kingres.create_distributed_table('public.kv', 'id', bucket_count => 12);
});
# Privileges are per node.
# GRANT on a distributed table is propagated from the map group primary.
$nodes[0]->safe_psql('postgres', 'GRANT ALL ON public.kv TO ks_user');

my %keys;
for my $g (0 .. 2)
{
	my $list = $n1->safe_psql('postgres', qq{
		SELECT string_agg(k::text, ',' ORDER BY k) FROM (
		  SELECT k FROM generate_series(1, 1000) k
		   WHERE (SELECT m.group_id FROM kingres.bucket_map m
		           WHERE m.colocation_id = 1
		             AND m.bucket = kingres.bucket_for(1, k)) = $g
		   LIMIT 3) s});
	$keys{$g} = [ split /,/, $list ];
}
for my $g (0 .. 2)
{
	$n1->safe_psql('postgres',
		"INSERT INTO public.kv VALUES ($keys{$g}[0], 'row-of-group-$g')");
}

sub run_sql
{
	my ($node, $sql, %extra) = @_;
	my ($stdout, $stderr);
	my $rc = $node->psql('postgres', $sql, stdout => \$stdout,
		stderr => \$stderr, %extra);
	return ($rc, $stdout, $stderr);
}

sub is_super
{
	my ($node, $role) = @_;
	return $node->safe_psql('postgres',
		"SELECT rolsuper FROM pg_roles WHERE rolname = '$role'");
}

# ---------------------------------------------------------------------------
# S-B: the stale-map reload.  Node 3 believes that group 1 owns a bucket that
# group 0 owns; an ordinary role routes a statement for it, node 2 answers
# KS001, and node 3 reloads its catalog from the map group.  The reload
# applies the snapshot as the bootstrap superuser inside the user's session:
# types and functions that the user created in the temporary schema must not
# be reachable.
# ---------------------------------------------------------------------------
{
	my $k = $keys{0}[0];
	my $b = $n1->safe_psql('postgres',
		"SELECT kingres.bucket_for('public.kv'::regclass, $k)");
	$n3->safe_psql('postgres',
		"UPDATE kingres.bucket_map SET group_id = 1 WHERE colocation_id = 1 AND bucket = $b");

	my ($rc, $out, $err) = run_sql($n3, qq{
		CREATE FUNCTION pg_temp.evil(x anyelement) RETURNS bool LANGUAGE plpgsql AS \$\$
		BEGIN ALTER ROLE ks_victim SUPERUSER; RETURN true; END \$\$;
		CREATE DOMAIN pg_temp.text AS pg_catalog.text CHECK (pg_temp.evil(VALUE));
		CREATE DOMAIN pg_temp.oid AS pg_catalog.oid CHECK (pg_temp.evil(VALUE));
		CREATE DOMAIN pg_temp.int4 AS pg_catalog.int4 CHECK (pg_temp.evil(VALUE));
		CREATE DOMAIN pg_temp.jsonb AS pg_catalog.jsonb CHECK (pg_temp.evil(VALUE));
		CREATE DOMAIN pg_temp.name AS pg_catalog.name CHECK (pg_temp.evil(VALUE));
		CREATE DOMAIN pg_temp.bool AS pg_catalog.bool CHECK (pg_temp.evil(VALUE));
		CREATE DOMAIN pg_temp.regclass AS pg_catalog.regclass CHECK (pg_temp.evil(VALUE));
		SELECT val FROM public.kv WHERE id = $k;
		}, extra_params => [ '-U', 'ks_user' ]);
	is($err, '', 'S-B: the routed statement with hostile temporary objects works');
	is($out, 'row-of-group-0', 'S-B: and returns the right row');
	is($n3->safe_psql('postgres',
			"SELECT group_id FROM kingres.bucket_map WHERE colocation_id = 1 AND bucket = $b"),
		'0', 'S-B: the stale map was reloaded');
	for my $n (@nodes)
	{
		is(is_super($n, 'ks_victim'), 'f',
			'S-B: the catalog reload did not run the hostile function as superuser on ' . $n->name);
	}
}

# ---------------------------------------------------------------------------
# Finding 1: forwarded sessions.
# ---------------------------------------------------------------------------
{
	# An ordinary session cannot switch the flag mid-session.
	my ($rc, $out, $err) = run_sql($n2,
		"SET kingres_shard.forwarded = on;", extra_params => [ '-U', 'ks_user' ]);
	like($err, qr/parameter "kingres_shard.forwarded" cannot be set after connection start/,
		'finding 1: an ordinary session cannot set kingres_shard.forwarded');
	($rc, $out, $err) = run_sql($n2,
		"SELECT set_config('kingres_shard.forwarded', 'on', false);",
		extra_params => [ '-U', 'ks_user' ]);
	like($err, qr/cannot be set after connection start/,
		'finding 1: nor through set_config()');

	# A session that was opened as forwarded never runs what it cannot place.
	my $kl = $keys{1}[0];    # local on node 2
	my $kr = $keys{0}[0];    # not on node 2
	my $tok = $n2->safe_psql('postgres', 'SELECT kingres.internal_forward_token(current_user, 2)');
	local $ENV{PGOPTIONS} = "-c kingres_shard.forwarded=on -c kingres_shard.forward_token=$tok";
	for my $case (
		[ 'a statement without a key', "SELECT * FROM public.kv" ],
		[ 'an aggregate without a key', "SELECT count(*) FROM public.kv" ],
		[ 'two keys of different groups', "SELECT * FROM public.kv WHERE id IN ($kl, $kr)" ],
		[ 'an UPDATE without a key', "UPDATE public.kv SET val = 'x'" ],
		[ 'a DELETE without a key', "DELETE FROM public.kv" ],
		[ 'a multi-row INSERT', "INSERT INTO public.kv VALUES ($kl, 'a'), ($kr, 'b')" ],
		[ 'a key of another group', "SELECT * FROM public.kv WHERE id = $kr" ],
	  )
	{
		($rc, $out, $err) = run_sql($n2, "\\set VERBOSITY verbose\n$case->[1]");
		like($err, qr/ERROR:\s+KS001:/,
			"finding 1: a forwarded session refuses $case->[0] with KS001");
		is($out, '', "finding 1: and returns no row for $case->[0]");
	}
	($rc, $out, $err) = run_sql($n2, "SELECT val FROM public.kv WHERE id = $kl");
	is($err, '', 'finding 1: a forwarded session runs a statement for a local bucket');
	is($out, 'row-of-group-1', 'finding 1: and returns the row');
}
{
	# The refused DELETE/UPDATE changed nothing.
	for my $g (0 .. 2)
	{
		is($nodes[$g]->safe_psql('postgres',
				"SELECT val FROM public.kv WHERE id = $keys{$g}[0]"),
			"row-of-group-$g", "finding 1: the row of group $g is intact");
	}
}

# ---------------------------------------------------------------------------
# The routing switch is for administrators.
# ---------------------------------------------------------------------------
{
	my ($rc, $out, $err) = run_sql($n2,
		"SET kingres_shard.enable_routing = off;", extra_params => [ '-U', 'ks_user' ]);
	like($err, qr/permission denied to set parameter "kingres_shard.enable_routing"/,
		'an ordinary role cannot switch routing off');
	($rc, $out, $err) = run_sql($n2,
		"SET kingres_shard.enable_routing = off; SELECT count(*) FROM public.kv;");
	is($err, '', 'a superuser can');
}

foreach my $n (@nodes)
{
	$n->stop;
}

done_testing();
