# SPDX-License-Identifier: PostgreSQL
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/042_add_node_acl.pl
#
# Review finding S3-11: add_node() copies the owner and the privileges of the
# distributed and reference tables (and of the sequences of their columns, and
# the column privileges), and says which roles are missing on the new node
# before it changes anything.

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(make_node make_cluster run_sql);

my ($n1, $n2) = make_cluster('an', 2);
my $n3 = make_node('an3');

sub as_role
{
	my ($node, $role, $sql) = @_;
	return run_sql($node, $sql, extra_params => [ '-U', $role ]);
}

for my $n ($n1, $n2)
{
	$n->safe_psql('postgres', q{
		CREATE ROLE alice LOGIN;
		CREATE ROLE bob LOGIN;
		CREATE ROLE carol LOGIN;
		CREATE ROLE eve LOGIN;
		CREATE SCHEMA IF NOT EXISTS app;
		GRANT USAGE ON SCHEMA app TO PUBLIC;
		ALTER SCHEMA app OWNER TO alice;
	});
}
$n1->safe_psql('postgres', q{
	CREATE TABLE app.rr (id int PRIMARY KEY, v int, note text);
	CREATE TABLE app.dd (id int PRIMARY KEY, v int, s serial);
	INSERT INTO app.rr SELECT g, g FROM generate_series(1, 3) g;
	SELECT kingres.create_reference_table('app.rr');
	SELECT kingres.create_distributed_table('app.dd', 'id');
	ALTER TABLE app.rr OWNER TO alice;
	ALTER TABLE app.dd OWNER TO alice;
	GRANT SELECT, INSERT, UPDATE, DELETE ON app.rr TO bob;
	GRANT SELECT ON app.rr TO carol WITH GRANT OPTION;
	GRANT SELECT (id, v) ON app.dd TO carol;
	GRANT INSERT, SELECT ON app.dd TO bob;
	REVOKE ALL ON app.dd FROM PUBLIC;
	GRANT USAGE ON SEQUENCE app.dd_s_seq TO bob;
});

# The new node has none of the roles: nothing is changed and the message names them.
my ($rc, $out, $err) = run_sql($n1, sprintf("SELECT kingres.add_node('%s', %d)", $n3->host, $n3->port));
isnt($rc, 0, 'add_node is refused while the new node lacks roles');
like($err, qr/alice, bob, carol/, 'the message names the missing roles');
unlike($err, qr/eve/, 'a role that holds nothing is not required');
is($n3->safe_psql('postgres', q{SELECT count(*) FROM pg_extension WHERE extname = 'kingres_shard'}),
	'0', 'nothing was created on the new node');
is($n1->safe_psql('postgres', q{SELECT count(*) FROM kingres.node}), '2', 'the node is not registered');

$n3->safe_psql('postgres', q{
	CREATE ROLE alice LOGIN; CREATE ROLE bob LOGIN; CREATE ROLE carol LOGIN; CREATE ROLE eve LOGIN;

});
($rc, $out, $err) = run_sql($n1, sprintf("SELECT kingres.add_node('%s', %d)", $n3->host, $n3->port));
is($rc, 0, 'add_node succeeds once the roles exist') or diag $err;

my $acl_sql = q{
	SELECT string_agg(c.relname || ' ' || c.relkind::text || ' ' || c.relowner::regrole::text || ' ' || coalesce(c.relacl::text, '-'),
	                  E'\n' ORDER BY c.relname)
	  FROM pg_class c
	 WHERE c.relnamespace = 'app'::regnamespace AND c.relkind IN ('r', 'p', 'S') AND NOT c.relispartition
};
my $col_sql = q{
	SELECT coalesce(string_agg(a.attrelid::regclass::text || '.' || a.attname || ' ' || a.attacl::text, E'\n'
	                           ORDER BY a.attrelid::regclass::text, a.attname), '')
	  FROM pg_attribute a WHERE a.attrelid IN ('app.rr'::regclass, 'app.dd'::regclass) AND a.attacl IS NOT NULL
};
my $want = $n1->safe_psql('postgres', $acl_sql);
like($want, qr/dd_s_seq S/, 'the sequence of a column is part of what is compared');
is($n3->safe_psql('postgres', $acl_sql), $want, 'owners and privileges of the tables and the sequence are the same on the new node');
is($n3->safe_psql('postgres', $col_sql), $n1->safe_psql('postgres', $col_sql),
	'column privileges are the same on the new node');
isnt($n1->safe_psql('postgres', $col_sql), '', 'there are column privileges to compare');
my $ns_sql = q{SELECT nspowner::regrole::text || ' ' || coalesce(nspacl::text, '-') FROM pg_namespace WHERE nspname = 'app'};
is($n3->safe_psql('postgres', $ns_sql), $n1->safe_psql('postgres', $ns_sql), 'the schema has the same owner and privileges on the new node');
is($n3->safe_psql('postgres', q{SELECT has_table_privilege('eve', 'app.dd', 'SELECT')}), 'f',
	'a privilege removed from PUBLIC stays removed on the new node');

# The writer of a reference table works from every node, with a custom and a generic plan.
for my $node ($n1, $n2, $n3)
{
	my $k = 100 + $node->port % 100;
	($rc, $out, $err) = as_role($node, 'bob', "INSERT INTO app.rr VALUES ($k, 1)");
	is($rc, 0, 'bob writes a reference table from ' . $node->name) or diag $err;
	($rc, $out, $err) = as_role($node, 'bob', qq{
		SET plan_cache_mode = force_generic_plan;
		PREPARE w(int) AS UPDATE app.rr SET v = v + 1 WHERE id = \$1;
		EXECUTE w($k); EXECUTE w($k);});
	is($rc, 0, 'a generic plan of bob works from ' . $node->name) or diag $err;
}
($rc, $out, $err) = as_role($n3, 'eve', q{INSERT INTO app.rr VALUES (999, 1)});
isnt($rc, 0, 'eve may not write');
like($err, qr/permission denied/, 'eve is told so');
is($n1->safe_psql('postgres', q{SELECT string_agg(id || ':' || v, ',' ORDER BY id) FROM app.rr WHERE id > 3}),
	$n3->safe_psql('postgres', q{SELECT string_agg(id || ':' || v, ',' ORDER BY id) FROM app.rr WHERE id > 3}),
	'the writes reached the new node');

done_testing();
