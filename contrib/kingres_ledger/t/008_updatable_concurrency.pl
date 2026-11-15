# contrib/kingres_ledger/t/008_updatable_concurrency.pl
#
# Updatable ledger tables under concurrency and replication.
#
#   * Many sessions update, delete, insert, upsert, merge and lock the same
#     few rows of an updatable ledger table, in READ COMMITTED (where a
#     concurrent update makes the executor re-evaluate the row, EvalPlanQual,
#     and update the newest version) and in REPEATABLE READ (serialization
#     failures, retried), while blocks are built.  Whatever happens, the
#     current rows equal the entries that are in force (every I minus every
#     D) and every D entry has its history row: ledger_verify finds nothing.
#   * Logical replication apply: the subscriber's updatable ledger table
#     captures the replicated UPDATE and DELETE, although the apply worker runs
#     with session_replication_role = replica.
#
# Portions Copyright (c) 2026, RooyeKhat Media Co
# SPDX-License-Identifier: PostgreSQL

use strict;
use warnings FATAL => 'all';

use IPC::Run;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;

my $node = PostgreSQL::Test::Cluster->new('main');
$node->init;
$node->append_conf(
	'postgresql.conf', qq(
shared_preload_libraries = 'kingres_ledger'
kingres_ledger.block_interval = 0
max_connections = 60
));
$node->start;

$node->safe_psql(
	'postgres', q(
CREATE EXTENSION kingres_ledger;
CREATE TABLE public.c (id int PRIMARY KEY, v int NOT NULL, t text, big text);
INSERT INTO c SELECT g, 0, 'init', repeat('x', 3000) FROM generate_series(1, 8) g;
SELECT kingres.ledger_enable('public.c', 'updatable');
CREATE FUNCTION public.c_after() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    -- an AFTER trigger that updates the row again, now and then
    IF NEW.v % 7 = 0 AND NEW.t IS DISTINCT FROM 'seven' THEN
        UPDATE c SET t = 'seven' WHERE id = NEW.id;
    END IF;
    RETURN NULL;
END $$;
CREATE TRIGGER a_after AFTER UPDATE ON public.c FOR EACH ROW EXECUTE FUNCTION public.c_after();
));

my $rc = "$PostgreSQL::Test::Utils::tmp_check/rc.sql";
append_to_file($rc, q(
\set id random(1, 8)
\set other random(1, 8)
\set kind random(1, 10)
BEGIN;
\if :kind <= 3
UPDATE c SET v = v + 1 WHERE id = :id;
\elif :kind = 4
UPDATE c SET v = v + 1, t = 'multi' WHERE id IN (:id, :other);
\elif :kind = 5
DELETE FROM c WHERE id = :id;
INSERT INTO c VALUES (:id, 100, 'reborn', repeat('y', 3000)) ON CONFLICT (id) DO NOTHING;
\elif :kind = 6
INSERT INTO c VALUES (:id, 1, 'upsert', NULL) ON CONFLICT (id) DO UPDATE SET v = c.v + 1, big = NULL;
\elif :kind = 7
SELECT v FROM c WHERE id = :id FOR UPDATE;
UPDATE c SET v = v + 1 WHERE id = :id;
\elif :kind = 8
MERGE INTO c USING (VALUES (:id, 1)) AS s(id, n) ON c.id = s.id
  WHEN MATCHED AND c.v > 50 THEN DELETE
  WHEN MATCHED THEN UPDATE SET v = c.v + s.n;
\elif :kind = 9
UPDATE c SET v = v + 1 WHERE id = :id;
SAVEPOINT s;
UPDATE c SET v = v + 1 WHERE id = :id;
ROLLBACK TO s;
UPDATE c SET t = 'sp' WHERE id = :id;
\else
UPDATE c SET v = v + 1 WHERE id = :id;
ROLLBACK;
BEGIN;
\endif
COMMIT;
));

my $rr = "$PostgreSQL::Test::Utils::tmp_check/rr.sql";
append_to_file($rr, q(
\set id random(1, 8)
BEGIN ISOLATION LEVEL REPEATABLE READ;
UPDATE c SET v = v + 1 WHERE id = :id;
COMMIT;
));

my $builder = "$PostgreSQL::Test::Utils::tmp_check/builder2.sql";
append_to_file($builder, q(
SELECT kingres.ledger_build_block();
));

my @pgbench = ('pgbench', '-n', '-h', $node->host, '-p', $node->port);
my ($o1, $e1, $o2, $e2, $o3, $e3);
my $p1 = IPC::Run::start(
	[ @pgbench, '-c', '8', '-j', '4', '-T', '6', '--max-tries=20', '-f', $rc, 'postgres' ],
	'>', \$o1, '2>', \$e1);
my $p2 = IPC::Run::start(
	[ @pgbench, '-c', '4', '-j', '2', '-T', '6', '--max-tries=50', '-f', $rr, 'postgres' ],
	'>', \$o2, '2>', \$e2);
my $p3 = IPC::Run::start(
	[ @pgbench, '-c', '2', '-j', '2', '-T', '6', '-f', $builder, 'postgres' ],
	'>', \$o3, '2>', \$e3);
ok($p1->finish, 'READ COMMITTED writers finished') or diag($e1);
ok($p2->finish, 'REPEATABLE READ writers finished') or diag($e2);
ok($p3->finish, 'builders finished') or diag($e3);

$node->safe_psql('postgres', 'SELECT kingres.ledger_build_block()');
is($node->safe_psql('postgres', 'SELECT count(*) FROM kingres.ledger_verify()'),
	'0', 'the ledger verifies after the concurrent updates');
my $stats = $node->safe_psql('postgres', q(
	SELECT (SELECT count(*) FROM kingres.ledger_entry WHERE op = 'D') || ' ' ||
	       (SELECT count(*) FROM public.c_ledger_history)));
my ($d, $h) = split ' ', $stats;
cmp_ok($d, '>', 100, "many versions were replaced ($d D entries)");
is($d, $h, 'one history row for every D entry');
is($node->safe_psql('postgres', q(
		SELECT count(*) FROM public.c_ledger_history h
		 WHERE kingres.ledger_history_row_hash(h.*) NOT IN
		       (SELECT row_hash FROM kingres.ledger_entry WHERE op = 'D'))),
	'0', 'every history row hashes to the row hash of a D entry');
is($node->safe_psql('postgres', q(
		SELECT count(*) FROM (
		  SELECT row_hash, sum(CASE op WHEN 'I' THEN 1 ELSE -1 END) n
		    FROM kingres.ledger_entry GROUP BY 1 HAVING sum(CASE op WHEN 'I' THEN 1 ELSE -1 END) <> 0) e
		 FULL JOIN (SELECT kingres.ledger_row_hash(c.*) AS row_hash, count(*) n FROM public.c c GROUP BY 1) r
		   USING (row_hash)
		 WHERE e.n IS DISTINCT FROM r.n)),
	'0', 'the rows in the table are what the entries leave in force');
is($node->safe_psql('postgres', 'SELECT count(*) FROM kingres.ledger_pending'),
	'0', 'nothing is left pending');

# With event triggers off an ADD COLUMN does not reach the history table: the
# next UPDATE or DELETE fails instead of recording a row without the column,
# and the verification reports it.
$node->safe_psql('postgres', 'SET event_triggers = off; ALTER TABLE public.c ADD COLUMN z int');
my ($ret, $stdout, $stderr) = $node->psql('postgres', 'UPDATE public.c SET v = v WHERE id = (SELECT min(id) FROM public.c)');
isnt($ret, 0, 'an UPDATE of a table whose history lacks a column fails');
like($stderr, qr/has no column "z"/, 'with a clear message');
is($node->safe_psql('postgres', q(SELECT count(*) > 0 FROM kingres.ledger_verify() WHERE check_name = 'history_columns')),
	't', 'and ledger_verify reports the history table');
$node->stop;

#--------------------------------------------------------------------------
# Logical replication apply into an updatable ledger table.
#--------------------------------------------------------------------------
my $pub = PostgreSQL::Test::Cluster->new('pub');
$pub->init(allows_streaming => 'logical');
$pub->start;
my $sub = PostgreSQL::Test::Cluster->new('sub');
$sub->init;
$sub->append_conf('postgresql.conf',
	"shared_preload_libraries = 'kingres_ledger'\n"
	  . "kingres_ledger.block_interval = 0\n"
	  . "wal_retrieve_retry_interval = '100ms'\n");
$sub->start;
$pub->safe_psql('postgres',
	q(CREATE TABLE public.t (id int PRIMARY KEY, v text);
	  INSERT INTO t VALUES (1, 'one'), (2, 'two');
	  CREATE PUBLICATION p FOR TABLE t;));
$sub->safe_psql('postgres',
	q(CREATE EXTENSION kingres_ledger;
	  CREATE TABLE public.t (id int PRIMARY KEY, v text);
	  SELECT kingres.ledger_enable('public.t', 'updatable');));
$sub->safe_psql('postgres',
	"CREATE SUBSCRIPTION s CONNECTION '" . $pub->connstr . " dbname=postgres' PUBLICATION p");
$sub->wait_for_subscription_sync($pub, 's');
$pub->safe_psql('postgres',
	q(UPDATE t SET v = 'uno' WHERE id = 1;
	  DELETE FROM t WHERE id = 2;
	  INSERT INTO t VALUES (3, 'three');
	  UPDATE t SET v = 'tres' WHERE id = 3;));
$pub->wait_for_catchup('s');
is($sub->safe_psql('postgres', 'SELECT string_agg(id || v, \',\' ORDER BY id) FROM public.t'),
	'1uno,3tres', 'the replicated changes arrived');
is($sub->safe_psql('postgres',
		'SELECT string_agg(id || v, \',\' ORDER BY id, v) FROM public.t_ledger_history'),
	'1one,2two,3three', 'the replaced versions are in the history table');
is($sub->safe_psql('postgres', 'SELECT count(*) FROM kingres.ledger_verify()'),
	'0', 'the subscriber ledger verifies (the apply worker fired the capture trigger)');
$sub->stop;
$pub->stop;

done_testing();
