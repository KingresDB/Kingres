# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/253_catalog_spoof.pl: the catalog tables are trusted only where the extension is installed (XCAT-01).
#
#   The library is in shared_preload_libraries, so its hooks run in every database.  The owner of a database that does not have the
#   extension can make the schema "kingres" and put relations in it that have the names of the catalog tables.  The loader used to take
#   them for the catalog: views made a backend crash (and with it the postmaster), and tables made it route statements to the nodes that
#   they named.  The catalog is now trusted only when kingres_shard is installed in the database, the relations are members of it and the
#   schema belongs to a superuser; and a catalog table of an unexpected shape is an ERROR, not a crash.
#
#   A. a database owner who is not a superuser makes the schema with views of the six names and functions that write a marker; ordinary
#      statements, DDL, DML and CREATE EXTENSION attempts: no crash, no marker, normal PostgreSQL behaviour
#   B. the same with real tables that describe a node on a port where a listener records every connection, a distributed table and a
#      map group primary: the listener sees nothing, the rows go to the local table, the recovery workers call nothing; one log line
#   C. the extension is installed (database postgres): a relation that is not a member of it is not the catalog (ALTER EXTENSION ... DROP
#      TABLE), a schema of a role that is not a superuser is not trusted, and a catalog table of a changed shape is an ERROR
#   D. the postmaster did not restart, and no process ended by a signal

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use IO::Socket::INET;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(make_node);

my $node = make_node('xc', recovery_interval => 1, deadlock_interval => 1);
sub su { my ($sql, $db) = @_; return $node->safe_psql($db // 'postgres', $sql); }
sub as_tenant
{
	my ($db, $sql) = @_;
	my ($stdout, $stderr);
	my $rc = $node->psql($db, $sql, stdout => \$stdout, stderr => \$stderr, extra_params => [ '-U', 'tenant' ], timeout => 60);
	return ($rc, $stdout, $stderr);
}
sub as_super
{
	my ($db, $sql) = @_;
	my ($stdout, $stderr);
	my $rc = $node->psql($db, $sql, stdout => \$stdout, stderr => \$stderr, timeout => 60);
	return ($rc, $stdout, $stderr);
}
sub log_text { return PostgreSQL::Test::Utils::slurp_file($node->logfile); }

# the listener that stands for the node of the fake catalog: it records every connection that is made to it
my $fake_port = PostgreSQL::Test::Cluster::get_free_port();
my $listener = IO::Socket::INET->new(Listen => 16, LocalAddr => '127.0.0.1', LocalPort => $fake_port, ReuseAddr => 1, Blocking => 0)
  or die "no listener: $!";
sub connections
{
	my $n = 0;
	while (my $c = $listener->accept)
	{
		$n++;
		close $c;
	}
	return $n;
}

my $started = su(q{SELECT pg_postmaster_start_time()::text});

su(q{CREATE ROLE tenant LOGIN CREATEDB});
su(q{CREATE DATABASE viewdb OWNER tenant});
su(q{CREATE DATABASE tabledb OWNER tenant});
su(q{SELECT rolsuper FROM pg_roles WHERE rolname = 'tenant'}) eq 'f' or BAIL_OUT('the tenant must not be a superuser');

# what the functions of the tenant do when they are called by anybody: a marker, in a table that the tenant owns
my $marker_functions = q{
	CREATE TABLE public.marker (what text);
	CREATE FUNCTION kingres.mark(w text) RETURNS void LANGUAGE sql AS $$ INSERT INTO public.marker VALUES (w) $$;
	CREATE FUNCTION kingres.version() RETURNS text LANGUAGE sql AS $$ SELECT kingres.mark('version'); SELECT '0.9'::text $$;
	CREATE FUNCTION kingres.internal_try_lock(text) RETURNS bool LANGUAGE sql AS $$ SELECT kingres.mark('try_lock'); SELECT true $$;
	CREATE FUNCTION kingres.internal_sync_probe(int) RETURNS bool LANGUAGE sql AS $$ SELECT kingres.mark('sync_probe'); SELECT true $$;
	CREATE FUNCTION kingres.internal_dtx_applied(text) RETURNS void LANGUAGE sql AS $$ SELECT kingres.mark('dtx_applied') $$;
	CREATE FUNCTION kingres.internal_require_version(text) RETURNS void LANGUAGE sql AS $$ SELECT kingres.mark('require_version') $$;
	CREATE FUNCTION kingres.internal_catalog_snapshot() RETURNS jsonb LANGUAGE sql AS $$ SELECT kingres.mark('snapshot'); SELECT '{}'::jsonb $$;
	CREATE FUNCTION kingres.internal_apply_catalog(jsonb) RETURNS void LANGUAGE sql AS $$ SELECT kingres.mark('apply_catalog') $$;
	CREATE FUNCTION kingres.resume_bucket_moves() RETURNS void LANGUAGE sql AS $$ SELECT kingres.mark('resume_bucket_moves') $$;
	CREATE FUNCTION kingres.cleanup_moved_buckets() RETURNS void LANGUAGE sql AS $$ SELECT kingres.mark('cleanup_moved_buckets') $$;
	CREATE FUNCTION kingres.detect_distributed_deadlocks() RETURNS void LANGUAGE sql AS $$ SELECT kingres.mark('detect_deadlocks') $$;
	CREATE FUNCTION kingres.internal_gid_tag(text) RETURNS text LANGUAGE sql AS $$ SELECT kingres.mark('gid_tag'); SELECT 'x'::text $$;
	CREATE FUNCTION kingres.internal_forward_token(text, int) RETURNS text LANGUAGE sql AS $$ SELECT kingres.mark('forward_token'); SELECT 'x'::text $$;
};

# ---------------------------------------------------------------------------------------------------------------------------------
# A. views of the six names
# ---------------------------------------------------------------------------------------------------------------------------------
my ($rc, $out, $err) = as_tenant('viewdb', q{
	CREATE SCHEMA kingres;
	CREATE VIEW kingres.node AS SELECT 1 AS node_id, 0 AS group_id, 'h'::text AS host, 5432 AS port, 'primary'::text AS role;
	CREATE VIEW kingres.colocation AS SELECT 1 AS colocation_id;
	CREATE VIEW kingres.dist_table AS SELECT 1 AS relid;
	CREATE VIEW kingres.bucket_map AS SELECT 1 AS colocation_id;
	CREATE VIEW kingres.map_version AS SELECT 1::bigint AS version;
	CREATE VIEW kingres.local_node AS SELECT 1 AS node_id;
	CREATE VIEW kingres.cluster_secret AS SELECT 'x'::bytea AS secret;
} . $marker_functions);
is($rc, 0, 'A: the owner of a database without the extension makes the schema, the views and the functions') or diag($err);

($rc, $out, $err) = as_tenant('viewdb', q{
	CREATE TABLE public.victim (id int PRIMARY KEY, v text);
	INSERT INTO public.victim SELECT g, 'v' || g FROM generate_series(1, 10) g;
	UPDATE public.victim SET v = 'u' WHERE id = 3;
	DELETE FROM public.victim WHERE id = 4;
	ALTER TABLE public.victim ADD COLUMN w int;
	CREATE INDEX ON public.victim (v);
	SELECT count(*) FROM public.victim;
});
is($rc, 0, 'A: ordinary DDL and DML in the database') or diag($err);
like($out, qr/^9$/m, 'A: ... with the answers of PostgreSQL');
($rc, $out, $err) = as_tenant('viewdb', q{SELECT v.id, w.v FROM public.victim v JOIN public.victim w USING (id) WHERE id < 3 ORDER BY id});
is($rc, 0, 'A: a join') or diag($err);
($rc, $out, $err) = as_tenant('viewdb', q{CREATE EXTENSION kingres_shard});
isnt($rc, 0, 'A: the tenant cannot install the extension');
unlike($err, qr/server closed the connection|terminated|crash/i, 'A: ... and the backend did not crash') or diag($err);
($rc, $out, $err) = as_super('viewdb', q{CREATE EXTENSION kingres_shard});
isnt($rc, 0, 'A: the extension does not install over the look-alike tables either') or diag($out);
unlike($err, qr/server closed the connection|terminated|crash/i, 'A: ... and the backend did not crash') or diag($err);
($rc, $out, $err) = as_super('viewdb', q{DROP SCHEMA kingres CASCADE; CREATE EXTENSION kingres_shard; SELECT kingres.version()});
is($rc, 0, 'A: after the superuser removed the schema of the tenant, the extension installs') or diag($err);
is(su(q{SELECT count(*) FROM public.marker}, 'viewdb'), '0', 'A: no function of the tenant ran');

# ---------------------------------------------------------------------------------------------------------------------------------
# B. real tables that describe a cluster
# ---------------------------------------------------------------------------------------------------------------------------------
my $fake_catalog = sprintf(q{
	CREATE SCHEMA kingres;
	CREATE TABLE kingres.node (node_id int NOT NULL, group_id int NOT NULL, host text NOT NULL, port int NOT NULL, role text NOT NULL,
	                           is_active bool NOT NULL DEFAULT true, should_have_shards bool NOT NULL DEFAULT true, server_group text);
	CREATE TABLE kingres.colocation (colocation_id int NOT NULL, bucket_count int NOT NULL, dist_column_type regtype, dist_collation oid NOT NULL DEFAULT 0,
	                                 max_modulus int, placement_epoch bigint NOT NULL DEFAULT 0, kind text NOT NULL DEFAULT 'hash');
	CREATE TABLE kingres.dist_table (relid regclass NOT NULL, method text NOT NULL, dist_column name, colocation_id int);
	CREATE TABLE kingres.bucket_map (colocation_id int NOT NULL, bucket int NOT NULL, group_id int NOT NULL, state text NOT NULL DEFAULT 'active', modulus int);
	CREATE TABLE kingres.map_version (version bigint NOT NULL, term bigint NOT NULL DEFAULT 0, superseded_by_term bigint NOT NULL DEFAULT 0);
	CREATE TABLE kingres.local_node (node_id int NOT NULL, timeline int NOT NULL DEFAULT 0);
	CREATE TABLE kingres.cluster_secret (secret bytea, previous bytea, previous_at timestamptz);
	CREATE TABLE kingres.dtx_log (gid text, durable bool);
	CREATE TABLE kingres.bucket_move (move_id bigint, phase text);
	CREATE TABLE kingres.dtx_incident (id int);
	CREATE TABLE kingres.dtx_applied (gid text, node_id int);
	CREATE TABLE public.victim (id int PRIMARY KEY, v text);
	INSERT INTO kingres.cluster_secret VALUES ('\x0102030405060708090a0b0c0d0e0f10', NULL, NULL);
	INSERT INTO kingres.node VALUES (1, 0, '127.0.0.1', %d, 'primary'), (2, 1, '127.0.0.1', %d, 'primary');
	INSERT INTO kingres.local_node VALUES (1, 0);
	INSERT INTO kingres.map_version VALUES (7, 0, 0);
	INSERT INTO kingres.colocation VALUES (1, 4, 'int4'::regtype, 0, 4, 0, 'hash');
	INSERT INTO kingres.bucket_map SELECT 1, b, 1, 'active', 4 FROM generate_series(0, 3) b;
	INSERT INTO kingres.dist_table VALUES ('public.victim'::regclass, 'hash', 'id', 1);
}, $fake_port, $fake_port);
($rc, $out, $err) = as_tenant('tabledb', $fake_catalog . $marker_functions);
is($rc, 0, 'B: the owner makes tables of the catalog that name a node on the port of the listener, a distributed table and the functions') or diag($err);
is(connections(), 0, 'B: making them connects nowhere');

my $log_before = -s $node->logfile;
# one session: the log line is made once for it
($rc, $out, $err) = as_tenant('tabledb', q{
	INSERT INTO public.victim SELECT g, 'v' || g FROM generate_series(1, 10) g;
	SELECT count(*) FROM public.victim;
	UPDATE public.victim SET v = 'u' WHERE id = 3;
	DELETE FROM public.victim WHERE id = 4;
	SELECT count(*) FROM public.victim;
	SELECT * FROM public.victim WHERE id = 5;
	ALTER TABLE public.victim ADD COLUMN w int;
	CREATE INDEX ON public.victim (v);
	CREATE TABLE public.other (id int, v text);
	INSERT INTO public.other VALUES (1, 'x');
	SELECT count(*) FROM public.victim JOIN public.other USING (id);
	TRUNCATE public.other;
	ALTER TABLE public.victim DROP COLUMN id;
	DROP TABLE public.victim;
});
is($rc, 0, 'B: statements on the table that the fake catalog calls distributed run as in PostgreSQL') or diag($err);
like($out, qr/^10\n.*^9\n/ms, 'B: the rows were written to and read from the local table');
is(connections(), 0, 'B: no connection was made to the node of the fake catalog');
my $new_log = substr(log_text(), $log_before);
is(scalar(() = $new_log =~ /kingres_shard: .*treated as not installed/g), 1, 'B: one line in the log for the session') or diag($new_log);
unlike($new_log, qr/terminated by signal/, 'B: no process ended by a signal');

# the recovery launcher and its workers visit every database every second; the fake catalog says that this node is the map group primary
sleep(5);
is(connections(), 0, 'B: the background workers made no connection either');
($rc, $out, $err) = as_tenant('tabledb', q{CREATE TABLE public.victim2 (id int PRIMARY KEY); INSERT INTO public.victim2 VALUES (1); SELECT * FROM public.victim2});
is($rc, 0, 'B: and the database still works') or diag($err);
($rc, $out, $err) = as_tenant('tabledb', q{CREATE EXTENSION kingres_shard});
isnt($rc, 0, 'B: the tenant cannot install the extension');
($rc, $out, $err) = as_super('tabledb', q{CREATE EXTENSION kingres_shard});
isnt($rc, 0, 'B: the extension does not install over the tables of the tenant') or diag($out);
unlike($err, qr/server closed the connection|terminated|crash/i, 'B: ... and the backend did not crash') or diag($err);
is(su(q{SELECT count(*) FROM public.marker}, 'tabledb'), '0', 'B: no function of the tenant ran (not in the workers, not in the sessions)');
is(connections(), 0, 'B: still no connection');

# B2. the same tables, made by a superuser in a database that does not have the extension (a restored dump, a schema that the
# extension left behind): the schema is a superuser's, and it is still not the catalog of an installed extension
su(q{CREATE DATABASE leftdb});
($rc, $out, $err) = as_super('leftdb', $fake_catalog);
is($rc, 0, 'B2: a superuser makes the same tables in a database without the extension') or diag($err);
($rc, $out, $err) = as_super('leftdb', q{
	INSERT INTO public.victim SELECT g, 'v' || g FROM generate_series(1, 10) g;
	SELECT count(*) FROM public.victim;
	ALTER TABLE public.victim ADD COLUMN w int;
});
is($rc, 0, 'B2: statements on the table that these tables call distributed run as in PostgreSQL') or diag($err);
is(connections(), 0, 'B2: no connection was made to the node of the tables');

# ---------------------------------------------------------------------------------------------------------------------------------
# C. the extension is installed
# ---------------------------------------------------------------------------------------------------------------------------------
su(q{CREATE EXTENSION kingres_shard});
su(sprintf(q{SELECT kingres.init_cluster('%s', %d)}, $node->host, $node->port));
su(q{CREATE TABLE public.t (id int PRIMARY KEY, v text); SELECT kingres.create_distributed_table('public.t', 'id', bucket_count => 4)});
su(q{INSERT INTO public.t SELECT g, 'v' || g FROM generate_series(1, 10) g});
($rc, $out, $err) = as_super('postgres', q{ALTER TABLE public.t DROP COLUMN id});
isnt($rc, 0, 'C: with the catalog of the extension a distributed table keeps its distribution column');
like($err, qr/not supported on distributed tables/, 'C: ... that is the refusal') or diag($err);

# a relation of the schema that is not a member of the extension is not the catalog: the hooks act as if the extension were not installed
($rc, $out, $err) = as_super('postgres', q{
	BEGIN;
	ALTER EXTENSION kingres_shard DROP TABLE kingres.local_node;
	ALTER TABLE public.t DROP COLUMN id;
	ROLLBACK;
});
like($err, qr/part of the partition key/, 'C: a catalog table that is not a member of the extension is not the catalog: PostgreSQL answers, not the hooks') or diag($err);
unlike($err, qr/not supported on distributed tables/, 'C: ... no refusal of the extension (PostgreSQL has its own)');
($rc, $out, $err) = as_super('postgres', q{ALTER TABLE public.t DROP COLUMN id});
like($err, qr/not supported on distributed tables/, 'C: after the rollback the catalog is the catalog again');

# the schema must belong to a superuser
su(q{CREATE ROLE other SUPERUSER});
($rc, $out, $err) = as_super('postgres', q{
	BEGIN;
	ALTER SCHEMA kingres OWNER TO tenant;
	ALTER TABLE public.t DROP COLUMN id;
	ROLLBACK;
});
like($err, qr/part of the partition key/, 'C: a schema that is owned by a role that is not a superuser is not trusted: PostgreSQL answers') or diag($err);
($rc, $out, $err) = as_super('postgres', q{
	BEGIN;
	ALTER SCHEMA kingres OWNER TO other;
	ALTER TABLE public.t DROP COLUMN id;
	ROLLBACK;
});
like($err, qr/not supported on distributed tables/, 'C: a schema of another superuser is trusted');

# a catalog table of another shape is an error, not a crash
($rc, $out, $err) = as_super('postgres', q{
	BEGIN;
	ALTER TABLE kingres.map_version RENAME TO map_version_real;
	CREATE TABLE kingres.map_version (version int);
	ALTER EXTENSION kingres_shard ADD TABLE kingres.map_version;
	SELECT 1;
	ROLLBACK;
});
isnt($rc, 0, 'C: a catalog table with a column of another type is refused');
like($err, qr/does not have the type that kingres_shard expects/, 'C: ... with an error of the extension') or diag($err);
unlike($err, qr/server closed the connection|terminated|crash/i, 'C: ... and no crash');
($rc, $out, $err) = as_super('postgres', q{
	BEGIN;
	ALTER TABLE kingres.local_node RENAME TO local_node_real;
	CREATE VIEW kingres.local_node AS SELECT 1 AS node_id;
	ALTER EXTENSION kingres_shard ADD VIEW kingres.local_node;
	SELECT 1;
	ROLLBACK;
});
isnt($rc, 0, 'C: a view that is a member of the extension and has the name of a catalog table is refused');
like($err, qr/is not a table, so it cannot be a catalog table/, 'C: ... with an error of the extension') or diag($err);
($rc, $out, $err) = as_super('postgres', q{SELECT count(*) FROM public.t});
is($rc, 0, 'C: and after the rollback everything works') or diag($err);
like($out, qr/^10$/m, 'C: ... with the rows');

# ---------------------------------------------------------------------------------------------------------------------------------
# D. the server
# ---------------------------------------------------------------------------------------------------------------------------------
is(su(q{SELECT pg_postmaster_start_time()::text}), $started, 'D: the postmaster did not restart');
unlike(log_text(), qr/terminated by signal|server process .* was terminated|TRAP:/, 'D: no process of the server ended by a signal');
is(connections(), 0, 'D: nothing connected to the listener, in the whole test');

$listener->close;
$node->stop;
done_testing();
