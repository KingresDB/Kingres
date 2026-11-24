# contrib/kingres_ledger/t/042_catalog_trust.pl
#
# XCAT-03: the relations of the schema "kingres" that the ledger finds by name are trusted only
# when they are the ones of the extensions that own them.
#
# The library is preloaded, so its hooks run in every database, and the owner of a database can
# make the schema "kingres" and put relations in it that have the names of the ledger registry or
# of the catalog of kingres_shard.
#
#   A. a database without the extension: a registry and an index that look like the ledger's (the
#      index is the index of another table), and a function that writes a marker: ordinary DDL and
#      DML work, nothing crashes, no function of the tenant runs
#   B. the ledger installed, its schema given to the owner of the database, with tables
#      that describe a distributed schema that another shard group holds: kingres_shard is not
#      installed, so the description is not the catalog of a cluster and ledger_enable is not
#      refused because of it
#   C. the server did not restart
#
# Portions Copyright (c) 2026, RooyeKhat Media Co
# SPDX-License-Identifier: AGPL-3.0-only

use strict;
use warnings FATAL => 'all';

use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;

my $node = PostgreSQL::Test::Cluster->new('xcl');
$node->init;
$node->append_conf('postgresql.conf', "shared_preload_libraries = 'kingres_ledger'\nmax_connections = 40\n");
$node->start;

sub psql_as
{
	my ($user, $db, $sql) = @_;
	my ($stdout, $stderr);
	my $rc = $node->psql($db, $sql, stdout => \$stdout, stderr => \$stderr,
		(defined $user ? (extra_params => [ '-U', $user ]) : ()), timeout => 60);
	return ($rc, $stdout, $stderr);
}
sub log_text { return PostgreSQL::Test::Utils::slurp_file($node->logfile); }

my $started = $node->safe_psql('postgres', q{SELECT pg_postmaster_start_time()::text});
$node->safe_psql('postgres', q{CREATE ROLE tenant LOGIN CREATEDB; CREATE DATABASE spoofdb OWNER tenant; CREATE DATABASE shdb OWNER tenant});

# A
my ($rc, $out, $err) = psql_as('tenant', 'spoofdb', q{
	CREATE SCHEMA kingres;
	CREATE TABLE public.marker (what text);
	CREATE FUNCTION public.mark() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN INSERT INTO public.marker VALUES ('ran'); RETURN NULL; END $$;
	CREATE TABLE public.other (id int PRIMARY KEY);
	CREATE TABLE kingres.ledger_table (table_id int, relid oid, history_relid oid);
	CREATE UNIQUE INDEX ledger_table_relid_key ON public.other (id);
	CREATE UNIQUE INDEX ledger_table_history_relid_key ON public.other (id DESC);
	CREATE TABLE kingres.ledger_client_key (id int);
	CREATE VIEW kingres.ledger_attestation AS SELECT 1 AS id;
	INSERT INTO kingres.ledger_table VALUES (1, 'public.other'::regclass, 0);
});
is($rc, 0, 'A: the owner of a database without the extension makes look-alikes of the registry') or diag($err);
($rc, $out, $err) = psql_as('tenant', 'spoofdb', q{
	CREATE TABLE public.t (id int PRIMARY KEY, v text);
	INSERT INTO public.t SELECT g, 'v' || g FROM generate_series(1, 10) g;
	ALTER TABLE public.t ADD COLUMN w int;
	CREATE INDEX ON public.t (v);
	UPDATE public.t SET w = id;
	DELETE FROM public.t WHERE id = 1;
	ALTER TABLE public.other ADD COLUMN x int;
	DROP TABLE public.other;
	SELECT count(*) FROM public.t;
});
is($rc, 0, 'A: DDL and DML, also on the table that the registry names') or diag($err);
like($out, qr/^9$/m, 'A: ... with the answers of PostgreSQL');
($rc, $out, $err) = psql_as(undef, 'spoofdb', q{CREATE EXTENSION kingres_ledger});
unlike($err, qr/server closed the connection|terminated/i, 'A: the extension does not install over the look-alikes, and no crash');
is($node->safe_psql('spoofdb', q{SELECT count(*) FROM public.marker}), '0', 'A: no function of the tenant ran');

# B
($rc, $out, $err) = psql_as('tenant', 'shdb', q{CREATE SCHEMA s; CREATE TABLE s.t (id int PRIMARY KEY, v text)});
is($rc, 0, 'B: the tenant makes a table in a schema of its own') or diag($err);
# the superuser installs the ledger (the schema is made by the install) and gives the schema to the tenant; in a build
# that refuses to install into a schema of somebody else this is the way to get one
($rc, $out, $err) = psql_as(undef, 'shdb', q{CREATE EXTENSION kingres_ledger; ALTER SCHEMA kingres OWNER TO tenant});
is($rc, 0, 'B: the superuser installs the ledger and gives its schema to the tenant') or diag($err);
($rc, $out, $err) = psql_as('tenant', 'shdb', q{
	CREATE TABLE kingres.dist_schema (nsp regnamespace NOT NULL, colocation_id int NOT NULL);
	CREATE TABLE kingres.bucket_map (colocation_id int NOT NULL, bucket int NOT NULL, group_id int NOT NULL, state text NOT NULL DEFAULT 'active');
	CREATE TABLE kingres.local_node (node_id int NOT NULL);
	CREATE TABLE kingres.node (node_id int NOT NULL, group_id int NOT NULL, role text NOT NULL, is_active bool NOT NULL DEFAULT true);
	CREATE TABLE kingres.bucket_move (colocation_id int, phase text);
	INSERT INTO kingres.node VALUES (1, 1, 'primary'), (2, 7, 'primary');
	INSERT INTO kingres.local_node VALUES (1);
	INSERT INTO kingres.dist_schema VALUES ('s'::regnamespace, 5);
	INSERT INTO kingres.bucket_map VALUES (5, 0, 7, 'active');
});
is($rc, 0, 'B: the owner of the schema makes tables that say that group 7 holds the schema s, and this node is of group 1') or diag($err);
($rc, $out, $err) = psql_as(undef, 'shdb', q{SELECT kingres.ledger_enable('s.t')});
isnt($rc, 0, 'B: while the schema is the tenant\'s, ledger_enable refuses (the registry cannot be trusted)');
like($err, qr/cannot be trusted/, 'B: ... and says why') or diag($err);
($rc, $out, $err) = psql_as(undef, 'shdb', q{ALTER SCHEMA kingres OWNER TO CURRENT_USER});
is($rc, 0, 'B: the superuser takes the schema (the tenant tables stay in it)') or diag($err);
($rc, $out, $err) = psql_as(undef, 'shdb', q{SELECT kingres.ledger_enable('s.t')});
is($rc, 0, 'B: ledger_enable does not take the look-alike for the catalog of a cluster') or diag($err);
unlike($err, qr/distributed schema that shard group/, 'B: ... no refusal because of it');

# B2. the ledger installed in a schema of a superuser, which a superuser then gives to the tenant: the registry cannot be read
# any more, and that must not read as "there is nothing to protect"
$node->safe_psql('postgres', q{CREATE DATABASE closeddb OWNER tenant});
($rc, $out, $err) = psql_as(undef, 'closeddb', q{
	CREATE EXTENSION kingres_ledger;
	CREATE TABLE public.w (a int);
	SELECT kingres.ledger_enable('public.w');
	INSERT INTO public.w VALUES (1);
	ALTER SCHEMA kingres OWNER TO tenant;
});
is($rc, 0, 'B2: a ledger table, then the schema of the ledger given to the tenant') or diag($err);
($rc, $out, $err) = psql_as('tenant', 'closeddb', q{DROP SCHEMA kingres CASCADE});
isnt($rc, 0, 'B2: the tenant cannot drop the schema of the ledger');
like($err, qr/cannot drop schema|ledger/i, 'B2: ... the ledger refuses') or diag($err);
($rc, $out, $err) = psql_as('tenant', 'closeddb', q{DROP OWNED BY tenant});
isnt($rc, 0, 'B2: nor DROP OWNED BY the tenant (the schema is the tenant\'s)');
is($node->safe_psql('closeddb', q{SELECT count(*) FROM pg_namespace WHERE nspname = 'kingres'}), '1', 'B2: the schema is still there');
($rc, $out, $err) = psql_as(undef, 'closeddb', q{CREATE TABLE public.w2 (a int); SELECT kingres.ledger_enable('public.w2')});
isnt($rc, 0, 'B2: ledger_enable refuses while the registry cannot be trusted');
like($err, qr/cannot be trusted/, 'B2: ... and says why') or diag($err);
($rc, $out, $err) = psql_as(undef, 'closeddb', q{ALTER SCHEMA kingres OWNER TO CURRENT_USER; SELECT kingres.ledger_enable('public.w2')});
is($rc, 0, 'B2: a superuser repairs it, and the ledger works again') or diag($err);

# C
is($node->safe_psql('postgres', q{SELECT pg_postmaster_start_time()::text}), $started, 'C: the postmaster did not restart');
unlike(log_text(), qr/terminated by signal|was terminated|TRAP:/, 'C: no process ended by a signal');

$node->stop;
done_testing();
