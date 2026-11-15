# contrib/kingres_ledger/t/005_environments.pl
#
# Behaviour in the environments that the review of the first version did not
# examine: logical replication apply into a ledger table, a hot standby, a
# parallel query, and an extension owner that is not a superuser any more.
#
# Portions Copyright (c) 2026, RooyeKhat Media Co
# SPDX-License-Identifier: PostgreSQL

use strict;
use warnings FATAL => 'all';

use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;

#--------------------------------------------------------------------------
# Logical replication apply into a ledger table.  The apply worker runs with
# session_replication_role = replica; the triggers of the ledger are ENABLE
# ALWAYS and fire.  Replicated inserts are captured (as the subscription
# owner); a replicated UPDATE or DELETE is rejected by the append-only
# triggers, so the apply worker stops with an error until the subscription
# is changed, and the ledger stays consistent.
#--------------------------------------------------------------------------
my $pub = PostgreSQL::Test::Cluster->new('pub');
$pub->init(allows_streaming => 'logical');
$pub->start;
my $sub = PostgreSQL::Test::Cluster->new('sub');
$sub->init;
$sub->append_conf('postgresql.conf',
	"shared_preload_libraries = 'kingres_ledger'\n"
	  . "wal_retrieve_retry_interval = '100ms'\n");
$sub->start;

$pub->safe_psql('postgres',
	q(CREATE TABLE public.t (id int PRIMARY KEY, v text);
	  INSERT INTO t VALUES (1, 'one');
	  CREATE PUBLICATION p FOR TABLE t;));
$sub->safe_psql('postgres',
	q(CREATE EXTENSION kingres_ledger;
	  CREATE TABLE public.t (id int PRIMARY KEY, v text);
	  SELECT kingres.ledger_enable('public.t');));
my $connstr = $pub->connstr . ' dbname=postgres';
$sub->safe_psql('postgres',
	"CREATE SUBSCRIPTION s CONNECTION '$connstr' PUBLICATION p");
$sub->wait_for_subscription_sync($pub, 's');

$pub->safe_psql('postgres', q(INSERT INTO t VALUES (2, 'two'), (3, 'three')));
$pub->wait_for_catchup('s');
is($sub->safe_psql('postgres', 'SELECT count(*) FROM public.t'),
	'3', 'replicated rows arrived');
is( $sub->safe_psql('postgres',
		q(SELECT count(*) FROM kingres.ledger_entry e JOIN kingres.ledger_table t USING (table_id)
		  WHERE t.relid = 'public.t'::regclass)),
	'3', 'every replicated row has a ledger entry');
is( $sub->safe_psql('postgres',
		q(SELECT DISTINCT user_name FROM kingres.ledger_pending ORDER BY 1)),
	$sub->safe_psql('postgres', q(SELECT current_user)),
	'the writer of the replicated rows is the owner of the subscription');
is($sub->safe_psql('postgres', 'SELECT count(*) FROM kingres.ledger_verify()'),
	'0', 'the ledger of the subscriber verifies');

$pub->safe_psql('postgres', q(UPDATE t SET v = 'changed' WHERE id = 2));
$sub->poll_query_until('postgres',
	q(SELECT apply_error_count > 0 FROM pg_stat_subscription_stats WHERE subname = 's'))
  or diag('no apply error was recorded');
is( $sub->safe_psql('postgres', q(SELECT v FROM public.t WHERE id = 2)),
	'two', 'the replicated UPDATE was rejected, the ledger table is unchanged');
is($sub->safe_psql('postgres', 'SELECT count(*) FROM kingres.ledger_verify()'),
	'0', 'the ledger of the subscriber still verifies');
$sub->stop;
$pub->stop;

#--------------------------------------------------------------------------
# A hot standby: the verification and the digest checks work on it, and
# nothing that writes does.
#--------------------------------------------------------------------------
my $primary = PostgreSQL::Test::Cluster->new('primary');
$primary->init(allows_streaming => 1);
$primary->append_conf('postgresql.conf',
	"shared_preload_libraries = 'kingres_ledger'\n");
$primary->start;
$primary->safe_psql('postgres',
	q(CREATE EXTENSION kingres_ledger;
	  CREATE TABLE public.items (id int PRIMARY KEY, label text);
	  CREATE TABLE public.more (id int PRIMARY KEY);
	  SELECT kingres.ledger_enable('public.items');
	  INSERT INTO items VALUES (1, 'a'), (2, 'b');));
my $digest = $primary->safe_psql('postgres', 'SELECT kingres.ledger_generate_digest()');
$primary->backup('b');
my $standby = PostgreSQL::Test::Cluster->new('standby');
$standby->init_from_backup($primary, 'b', has_streaming => 1);
$standby->append_conf('postgresql.conf',
	"shared_preload_libraries = 'kingres_ledger'\n");
$standby->start;
$primary->wait_for_catchup($standby);

is($standby->safe_psql('postgres', 'SELECT count(*) FROM kingres.ledger_verify()'),
	'0', 'ledger_verify works on a hot standby');
is($standby->safe_psql('postgres', "SELECT kingres.ledger_verify_digest('$digest')"),
	't', 'ledger_verify_digest works on a hot standby');
is( $standby->safe_psql(
		'postgres',
		qq(SELECT kingres.ledger_verify_proof(
		   kingres.ledger_inclusion_proof('items', kingres.ledger_row_hash(i.*)), '$digest'::jsonb)
		   FROM items i WHERE id = 1)),
	't', 'a proof can be built and checked on a hot standby');
my ($ret, $stdout, $stderr) = $standby->psql('postgres',
	q(SELECT kingres.ledger_enable('public.more')));
isnt($ret, 0, 'ledger_enable fails on a standby');
like($stderr, qr/read-only|recovery/, 'because it is read-only');
($ret, $stdout, $stderr) = $standby->psql('postgres',
	q(ALTER TABLE items DISABLE TRIGGER ALL));
isnt($ret, 0, 'the hook runs on the standby');

#--------------------------------------------------------------------------
# A parallel query: the functions that read tables are not parallel safe, the
# pure ones are, and nothing breaks when the planner is forced to try.
#--------------------------------------------------------------------------
$primary->safe_psql('postgres',
	q(INSERT INTO items SELECT g, 'x' FROM generate_series(10, 2000) g));
$primary->safe_psql('postgres', 'SELECT kingres.ledger_build_block()');
is( $primary->safe_psql('postgres',
		q(SET debug_parallel_query = on; SET parallel_setup_cost = 0; SET parallel_tuple_cost = 0;
		  SELECT count(*) FROM items i WHERE kingres.ledger_row_hash(i.*) IS NOT NULL)),
	'1993', 'ledger_row_hash under debug_parallel_query');
is( $primary->safe_psql('postgres',
		q(SET debug_parallel_query = on; SET parallel_setup_cost = 0; SET parallel_tuple_cost = 0;
		  SELECT count(*) FROM kingres.ledger_verify())),
	'0', 'ledger_verify under debug_parallel_query');
is( $primary->safe_psql('postgres',
		q(SET debug_parallel_query = on;
		  SELECT count(*) FROM generate_series(1, 3) g
		  WHERE kingres.ledger_leaf_hash(int4send(g)) <> kingres.ledger_leaf_hash('\x00'))),
	'3', 'the pure hash primitives under debug_parallel_query');
$standby->stop;

#--------------------------------------------------------------------------
# The owner of the extension must be a superuser: ledger_enable creates
# triggers on tables owned by other roles.
#--------------------------------------------------------------------------
my $other = PostgreSQL::Test::Cluster->new('other');
$other->init;
$other->append_conf('postgresql.conf',
	"shared_preload_libraries = 'kingres_ledger'\n");
$other->start;
$other->safe_psql('postgres',
	q(CREATE ROLE extowner SUPERUSER LOGIN;
	  SET ROLE extowner;
	  CREATE EXTENSION kingres_ledger;
	  RESET ROLE;
	  CREATE TABLE public.mine (a int);
	  ALTER ROLE extowner NOSUPERUSER;));
($ret, $stdout, $stderr) = $other->psql('postgres',
	q(SELECT kingres.ledger_enable('public.mine')),
	extra_params => [ '-v', 'VERBOSITY=terse' ]);
isnt($ret, 0, 'ledger_enable refuses when the owner of the extension is not a superuser');
like($stderr, qr/must be a superuser/, 'with a clear message');
$other->stop;
$primary->stop;

done_testing();
