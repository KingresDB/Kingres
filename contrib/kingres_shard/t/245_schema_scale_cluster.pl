# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/245_schema_scale_cluster.pl
#
# Phase 11 (design/shard-phase11.md 15.2, "119_schema_scale"; 5.3, 7.11, step 12): the cost of many distributed schemas, on three nodes.  The numbers are
# reported (diag), not asserted; the test asserts that every schema and table exists, is registered and is the same on every node.  The count is
# KS_SCALE_SCHEMAS (default 200 schemas of 5 tables); the design asks for 2000.

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use Time::HiRes qw(time);
use KingresShardTest qw(make_cluster run_sql prepared_count);

my $nsch = $ENV{KS_SCALE_SCHEMAS} // 200;
my @nodes = make_cluster('sc', 3);
my ($n1, $n2, $n3) = @nodes;

sub q1 { my ($n, $sql) = @_; return $n->safe_psql('postgres', $sql); }

q1($n1, q{SELECT kingres.set_sharding_mode('both', true)});

# one session creates every schema and table: the time of each statement is what the statement costs, without a connection
my $t0 = time();
my $sql = '';
for my $i (1 .. $nsch)
{
	$sql .= "CREATE SCHEMA tenant_$i;\n";
	$sql .= "CREATE TABLE tenant_$i.t$_ (id int PRIMARY KEY, v text);\n" for 1 .. 5;
}
my $tc = time();
q1($n1, $sql);
my $total = time() - $tc;
diag(sprintf("%d schemas and %d tables created in %.1f s: %.1f ms per statement (a CREATE SCHEMA or a CREATE TABLE, two-phase on 3 nodes)",
		$nsch, $nsch * 5, $total, 1000 * $total / ($nsch * 6)));

is(q1($n1, q{SELECT count(*) FROM kingres.dist_schema}), $nsch, "every schema is distributed");
for my $n (@nodes)
{
	is(q1($n, q{SELECT count(*) FROM kingres.dist_table WHERE method = 'schema'}), $nsch * 5, $n->name . ': every table is registered');
	is(q1($n, q{SELECT count(*) FROM pg_namespace WHERE nspname LIKE 'tenant\_%'}), $nsch, $n->name . ': every schema exists');
}
is(prepared_count($_), 0, $_->name . ': no prepared transaction') for @nodes;

# a new backend loads the catalog cache on its first statement: its time and its size
for my $n (@nodes)
{
	my $s = time();
	my $mem = $n->safe_psql('postgres', q{SELECT count(*) FROM tenant_1.t1; SELECT (SELECT coalesce(sum(total_bytes), 0) FROM pg_backend_memory_contexts WHERE name ILIKE '%kingres%')});
	diag(sprintf("%s: first statement of a new backend %.0f ms; kingres memory contexts of the backend: %s bytes", $n->name, 1000 * (time() - $s), (split /\n/, $mem)[-1]));
	$s = time();
	$n->safe_psql('postgres', q{SELECT count(*) FROM tenant_1.t1});
	diag(sprintf("%s: the same with a warm start of psql %.0f ms (the connection itself is most of it)", $n->name, 1000 * (time() - $s)));
}

# a routed statement from a node that does not hold the schema, and one that does
{
	my $loops = 50;
	my $s = time();
	q1($n1, join('', map { "INSERT INTO tenant_$_.t1 VALUES (1, 'x');\n" } 1 .. $loops));
	diag(sprintf("%d single-row INSERTs into %d different schemas from one session: %.1f ms each", $loops, $loops, 1000 * (time() - $s) / $loops));
}
# add_node with this many schemas: every schema and table is made on the new node by the batches of the add
{
	my $n4 = PostgreSQL::Test::Cluster->new('sc4');
	$n4->init;
	$n4->append_conf('postgresql.conf', "shared_preload_libraries = 'kingres_shard'\nmax_prepared_transactions = 20\n");
	$n4->start;
	$n4->safe_psql('postgres', 'CREATE EXTENSION kingres_shard');
	my $s = time();
	my ($rc, $out, $err) = run_sql($n1, sprintf("SELECT kingres.add_node('%s', %d)", $n4->host, $n4->port));
	diag(sprintf("add_node with %d schemas: rc=%d in %.1f s%s", $nsch, $rc, time() - $s, $rc ? " ($err)" : ''));
	if ($rc == 0)
	{
		is(q1($n4, q{SELECT count(*) FROM pg_namespace WHERE nspname LIKE 'tenant\_%'}), $nsch, 'the new node has every schema');
		is(q1($n4, q{SELECT count(*) FROM kingres.dist_table WHERE method = 'schema'}), $nsch * 5, '... and every table registered');
	}
	$n4->stop('immediate');
}

done_testing();
