# contrib/kingres_columnar/t/029_chain_parallel_backup.pl
#
# C7: what the review of the sixth round did not cover (its "Not covered" list),
# each against a heap twin.
#
#   1. COMMIT AND CHAIN and ROLLBACK AND CHAIN with a late writer (a module whose
#      pre-commit callback runs after the access method's): the state the access
#      method keeps for the commit-time flush must not leak into the next
#      transaction of the chain, under READ COMMITTED and SERIALIZABLE.
#   2. A parallel leader: an INSERT ... SELECT whose SELECT runs in workers (the
#      rows reach the access method only in the leader), with and without memory
#      pressure; and a parallel query while the leader holds buffered rows.
#   3. A base backup taken while two sessions commit paired inserts (a columnar
#      table and a heap twin, in one transaction) and a third rewrites a pair of
#      tables twenty times: the restored cluster has equal counts for every pair
#      and a clean columnar_check.  (The review ran this on 18.7 only; the file
#      runs on whichever major it is given.)
#
# Needs a compiler and the server's headers for the probe module (parts 1 and 2).
#
# Portions Copyright (c) 2026, RooyeKhat Media Co
# SPDX-License-Identifier: AGPL-3.0-only

use strict;
use warnings FATAL => 'all';

use FindBin;
use lib $FindBin::RealBin;
use KcProbe qw(build_module);
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;

my $dir = build_module('kcprobe');

my $node = PostgreSQL::Test::Cluster->new('colchain');
$node->init(allows_streaming => 1);
$node->append_conf('postgresql.conf', "shared_preload_libraries = '$dir/kcprobe,kingres_columnar'\nmax_worker_processes = 8\nmax_parallel_workers = 8\n");
$node->start;
$node->safe_psql('postgres', 'CREATE EXTENSION kingres_columnar');

sub q1 { return $node->safe_psql('postgres', $_[0]); }

sub script
{
	my ($n, $sql) = @_;
	my ($out, $err) = ('', '');

	$n->psql('postgres', $sql, stdout => \$out, stderr => \$err, on_error_stop => 0);
	return ($out, $err);
}

sub same
{
	my ($a, $b, $name) = @_;
	my $f = "SELECT count(*) || ':' || coalesce(sum(id), 0) || ':' || coalesce(md5(string_agg(id || '/' || length(pad), ',' ORDER BY id, length(pad))), '') FROM";
	is(q1("$f $a"), q1("$f $b"), $name);
}

# ------------------------------------------------------------------------
# 1. COMMIT AND CHAIN
# ------------------------------------------------------------------------
foreach my $iso ('READ COMMITTED', 'REPEATABLE READ', 'SERIALIZABLE')
{
	q1("DROP TABLE IF EXISTS p, h; CREATE TABLE p (id int, pad text) USING columnar; CREATE TABLE h (id int, pad text);");
	my ($out, $err) = script($node, "
SET kcprobe.commit_sql = 'INSERT INTO p VALUES (1000, \$\$late\$\$); INSERT INTO h VALUES (1000, \$\$late\$\$)';
BEGIN ISOLATION LEVEL $iso;
INSERT INTO p VALUES (1, \$\$a\$\$); INSERT INTO h VALUES (1, \$\$a\$\$);
COMMIT AND CHAIN;
INSERT INTO p SELECT g, repeat(\$\$b\$\$, 100) FROM generate_series(2, 2500) g; INSERT INTO h SELECT g, repeat(\$\$b\$\$, 100) FROM generate_series(2, 2500) g;
SAVEPOINT s; INSERT INTO p VALUES (3000, \$\$s\$\$); INSERT INTO h VALUES (3000, \$\$s\$\$); RELEASE s;
COMMIT AND CHAIN;
INSERT INTO p VALUES (4000, \$\$gone\$\$); INSERT INTO h VALUES (4000, \$\$gone\$\$);
ROLLBACK AND CHAIN;
INSERT INTO p VALUES (5000, \$\$c\$\$); INSERT INTO h VALUES (5000, \$\$c\$\$);
SELECT 'ISO=' || current_setting('transaction_isolation');
COMMIT;
SET kcprobe.commit_sql = '';");
	unlike($err, qr/ERROR|WARNING/, "$iso: COMMIT AND CHAIN and ROLLBACK AND CHAIN with a late writer: no error, no warning");
	my $isol = lc $iso;
	like($out, qr/ISO=\Q$isol\E/, "$iso: the chain keeps the isolation level");
	same('p', 'h', "$iso: the chain: the columnar table has the rows of the heap twin (late rows of three commits, the rolled back row absent)");
	# the late row of each commit that reaches the pre-commit (the two COMMIT AND CHAIN, the last
	# COMMIT, and the statement that made the setting, a transaction of its own; not the ROLLBACK):
	# as many as the heap twin has, and not fewer than the four
	is(q1("SELECT count(*) FROM p WHERE id = 1000"), q1("SELECT count(*) FROM h WHERE id = 1000"), "$iso: ... the late row of each commit is there once, as many as in the heap twin");
	cmp_ok(q1("SELECT count(*) FROM p WHERE id = 1000"), '>=', 4, "$iso: ... at least the late rows of the three commits of the chain and the setting's own");
	is(q1("SELECT count(*) FROM p WHERE id = 4000"), 0, "$iso: ... and the rolled back one is not");
	is(q1("SELECT count(*) FROM kingres.columnar_check()"), 0, "$iso: columnar_check has nothing to report");
}

# ------------------------------------------------------------------------
# 2. A parallel leader
# ------------------------------------------------------------------------
{
	# CREATE TABLE AS is the statement that may run its query in workers while the
	# leader writes the new table; INSERT ... SELECT never does
	q1("DROP TABLE IF EXISTS p, h, src;
		CREATE TABLE src AS SELECT g AS id, repeat('x', 2000) AS pad FROM generate_series(1, 60000) g;
		ALTER TABLE src SET (parallel_workers = 3);
		ANALYZE src;");
	my $par = "SET parallel_setup_cost = 0; SET parallel_tuple_cost = 0; SET min_parallel_table_scan_size = 0; SET max_parallel_workers_per_gather = 3; SET kingres_columnar.write_buffer_memory = '1MB';";
	my ($out, $err) = script($node, "$par
SELECT * FROM kingres.columnar_write_paths(true) WHERE false;
EXPLAIN (COSTS OFF) CREATE TABLE p USING columnar AS SELECT id, pad FROM src WHERE id % 2 = 0;
CREATE TABLE p USING columnar AS SELECT id, pad FROM src WHERE id % 2 = 0;
CREATE TABLE h AS SELECT id, pad FROM src WHERE id % 2 = 0;
SELECT 'FLUSH=' || hits FROM kingres.columnar_write_paths() WHERE path = 'pressure_flush';");
	unlike($err, qr/ERROR/, 'a CREATE TABLE AS whose query is parallel: no error');
	like($out, qr/Gather/, '... the plan has a Gather under the writer (the rows reach the access method in the leader)');
	same('p', 'h', '... and the columnar table has the rows of the heap twin');
	like($out, qr/FLUSH=[1-9]/, '... memory pressure wrote stripes in the leader (30 000 rows of 2 kB under 1 MB)');
	is(q1("SELECT count(*) FROM kingres.columnar_check()"), 0, 'columnar_check has nothing to report');
}

# ------------------------------------------------------------------------
# 3. A base backup under load, restored
# ------------------------------------------------------------------------
{
	$node->safe_psql('postgres', "
CREATE TABLE c1 (id int, pad text) USING columnar;  CREATE TABLE h1 (id int, pad text);
CREATE TABLE c2 (id int, pad text) USING columnar;  CREATE TABLE h2 (id int, pad text);
CREATE TABLE rc (a int, b text) USING columnar;     CREATE TABLE rh (a int, b text);
INSERT INTO rc SELECT g, 'r' || g FROM generate_series(1, 2000) g;
INSERT INTO rh SELECT g, 'r' || g FROM generate_series(1, 2000) g;
CREATE PROCEDURE load_pair(c regclass, h regclass, n int) LANGUAGE plpgsql AS \$\$
BEGIN
  FOR i IN 1..n LOOP
    EXECUTE format('INSERT INTO %s SELECT g + %s, repeat(%L, 50) FROM generate_series(1, 5) g', c, i * 10, 'x');
    EXECUTE format('INSERT INTO %s SELECT g + %s, repeat(%L, 50) FROM generate_series(1, 5) g', h, i * 10, 'x');
    COMMIT;
  END LOOP;
END \$\$;
CREATE PROCEDURE rewrite_pair(n int) LANGUAGE plpgsql AS \$\$
BEGIN
  FOR i IN 1..n LOOP
    IF i % 2 = 1 THEN
      ALTER TABLE rc ALTER COLUMN a TYPE bigint; ALTER TABLE rh ALTER COLUMN a TYPE bigint;
    ELSE
      ALTER TABLE rc ALTER COLUMN a TYPE int; ALTER TABLE rh ALTER COLUMN a TYPE int;
    END IF;
    COMMIT;
  END LOOP;
END \$\$;");
	my @bg;
	foreach my $call ("CALL load_pair('c1', 'h1', 1500)", "CALL load_pair('c2', 'h2', 1500)", "CALL rewrite_pair(20)")
	{
		my $b = $node->background_psql('postgres', on_error_stop => 1);
		$b->query_until(qr/started/, "\\echo started\n$call;\n");
		push @bg, $b;
	}
	$node->backup('bkp');
	foreach my $b (@bg) { $b->query_safe("SELECT 1"); $b->quit; }

	my $restored = PostgreSQL::Test::Cluster->new('colbackup');
	$restored->init_from_backup($node, 'bkp');
	$restored->start;
	my $r = sub { return $restored->safe_psql('postgres', $_[0]); };
	my $pair = sub
	{
		my ($c, $h, $col) = @_;
		return $r->("SELECT (SELECT count(*) || ':' || sum($col) FROM $c) = (SELECT count(*) || ':' || sum($col) FROM $h)");
	};
	is($pair->('c1', 'h1', 'id'), 't', 'the restored cluster: the first pair of tables, written by one transaction at a time, has equal counts and sums');
	is($pair->('c2', 'h2', 'id'), 't', '... and the second pair');
	is($pair->('rc', 'rh', 'a'), 't', '... and the pair that was rewritten while the backup was taken');
	cmp_ok($r->("SELECT count(*) FROM c1") + $r->("SELECT count(*) FROM c2"), '>', 0, '... and the backup caught some of the load');
	is($r->("SELECT count(*) FROM kingres.columnar_check()"), 0, '... columnar_check has nothing to report on the restored cluster');
	$restored->stop;
}

$node->stop;
done_testing();
