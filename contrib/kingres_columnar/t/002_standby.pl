# contrib/kingres_columnar/t/002_standby.pl
#
# A columnar table on a streaming standby with hot_standby = on, and across
# a promotion and a pg_rewind.
#
#   1. a load on the primary (many committed transactions, each of several
#      stripes, and one large transaction that makes the file much longer
#      than the standby's copy of it, so that redo has to extend the file
#      with page images beyond its end) is read on the standby while it runs:
#      the standby sees whole transactions only, and everything after
#      catch-up;
#   2. a standby only reads: statements that write are refused, nothing
#      PANICs, the maintenance functions fail cleanly;
#   3. a standby query holding the table across a VACUUM of the primary that
#      truncates the file tail is cancelled by the recovery conflict, a query
#      on another table is not, and replay goes on with identical data;
#   4. promotion: the data is there and the node accepts loads, VACUUM,
#      VACUUM FULL and columnar_compact;
#   5. wal_consistency_checking for the resource manager on both nodes;
#   6. pg_rewind of an old primary that wrote columnar data after the
#      standby was promoted.
#
# Every node preloads the library: the resource manager is registered at
# preload, and a node that replays the records cannot start without it.
#
# Portions Copyright (c) 2026, RooyeKhat Media Co
# SPDX-License-Identifier: PostgreSQL

use strict;
use warnings FATAL => 'all';

use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use File::Copy;
use Time::HiRes qw(usleep);

my $preload = "shared_preload_libraries = 'kingres_columnar'\n";

# Rows of the test tables: NULLs in every column kind, text of varying length,
# numeric with a scale.  $gen->(table, from, to) is an INSERT.
sub gen
{
	my ($table, $lo, $hi) = @_;
	return qq{INSERT INTO $table
	  SELECT i,
	         CASE WHEN i % 7 = 0 THEN NULL ELSE (i % 1000)::int END,
	         CASE WHEN i % 5 = 0 THEN NULL ELSE 'row-' || i || repeat('x', (i % 40)::int) END,
	         CASE WHEN i % 11 = 0 THEN NULL ELSE (i * 1.37)::numeric(14,3) END,
	         date '2020-01-01' + (i % 365)::int,
	         i % 3 = 0
	    FROM generate_series($lo, $hi) i};
}

my $cols = '(id bigint, a int, t text, n numeric(14,3), d date, f boolean)';

# One query, one snapshot: counts, sums and a hash of the whole row.
sub fp
{
	my ($table) = @_;
	return qq{SELECT count(*), coalesce(sum(id), 0), count(a), sum(a), count(t),
	                 sum(length(t)), count(n), sum(n), sum(d - date '2020-01-01'),
	                 count(*) FILTER (WHERE f), sum(hashtext(c::text))
	            FROM $table c};
}

# A cluster with the library preloaded and checksums on (pg_rewind and the
# page images both make use of them).
sub new_primary
{
	my ($name, $conf) = @_;
	my $node = PostgreSQL::Test::Cluster->new($name);
	$node->init(allows_streaming => 1, extra => ['--data-checksums']);
	$node->append_conf('postgresql.conf',
		# autovacuum off: it would truncate the dead tails the tests measure
		$preload . "wal_keep_size = '200MB'\nautovacuum = off\n" . ($conf // ''));
	$node->start;
	return $node;
}

sub new_standby
{
	my ($primary, $name, $backup, $conf) = @_;
	my $node = PostgreSQL::Test::Cluster->new($name);
	$node->init_from_backup($primary, $backup, has_streaming => 1);
	$node->append_conf('postgresql.conf',
		"hot_standby = on\nmax_standby_streaming_delay = '1s'\n"
		  . ($conf // ''));
	$node->start;
	return $node;
}

# What columnar_check reports on a node, one string; empty means clean.
sub check_rows
{
	my ($node) = @_;
	return $node->safe_psql('postgres',
		"SELECT string_agg(check_name || ' ' || coalesce(relation::text, '-') || ' ' || detail, '; ') FROM kingres.columnar_check()");
}

# The standby's log must not show a crash, an assertion or a corrupt table.
sub log_is_clean
{
	my ($node, $what) = @_;
	my $log = slurp_file($node->logfile);
	unlike($log, qr/PANIC|TRAP:|is corrupt|WARNING:|could not (?:read|extend)/,
		"$what: no PANIC, assertion, corrupt table or warning in the log");
}

###############################################################################
# 1. load on the primary, read on the standby while it runs
###############################################################################

my $primary = new_primary('primary');
$primary->safe_psql('postgres', 'CREATE EXTENSION kingres_columnar');
$primary->safe_psql('postgres', "CREATE TABLE ct $cols USING columnar");
$primary->safe_psql('postgres',
	"SELECT kingres.columnar_set_options('ct', stripe_row_count => 1000, chunk_row_count => 500)");
# 2000 rows exist when the backup is taken: the standby's file is two stripes.
$primary->safe_psql('postgres', gen('ct', 1, 2000));
$primary->backup('b');
my $standby = new_standby($primary, 'standby', 'b');
$primary->wait_for_catchup($standby);

my $size0 = $standby->safe_psql('postgres', "SELECT pg_relation_size('ct')");
cmp_ok($size0, '<', 400000, 'the standby starts with a small file');

# 30 transactions of 4000 rows (4 stripes each), then one of 300000 rows
# (300 stripes).  Counts that are not 2000 + 4000 k, or 122000 or 422000,
# would be a transaction seen in part.
my $load = <<"EOS";
\\echo load_started
DO \$\$
BEGIN
  FOR k IN 0..29 LOOP
    @{[ gen('ct', '2001 + k * 4000', '2000 + (k + 1) * 4000') ]};
    COMMIT;
  END LOOP;
END \$\$;
@{[ gen('ct', 122001, 422000) ]};
EOS

my $loader = $primary->background_psql('postgres', on_error_stop => 1);
$loader->query_until(qr/load_started/, $load);

my ($prev, $samples, $bad, %seen) = (0, 0, 0);
my $deadline = time + 240;
while (1)
{
	my $r = $standby->safe_psql('postgres',
		'SELECT count(*), coalesce(sum(id), 0), coalesce(max(id), 0) FROM ct');
	my ($c, $sum, $max) = split /\|/, $r;
	$samples++;
	$seen{$c}++;
	my $whole =
	     ($c >= 2000 && $c <= 122000 && ($c - 2000) % 4000 == 0)
	  || $c == 422000;
	$bad++ unless $whole && $sum == $c * ($c + 1) / 2 && $max == $c;
	$bad++ if $c < $prev;    # never goes back
	$prev = $c;
	last if $c == 422000 || time > $deadline;
	usleep(20_000);
}
is($bad, 0, 'standby counts during the load are monotonic and whole-transaction');
is($prev, 422000, 'the standby reached the end of the load');
note "standby samples: $samples, distinct counts: " . scalar(keys %seen);
$loader->query_safe('SELECT 1');
$loader->quit;

$primary->wait_for_catchup($standby);
my $p_fp = $primary->safe_psql('postgres', fp('ct'));
my $s_fp = $standby->safe_psql('postgres', fp('ct'));
like($p_fp, qr/^422000\|/, 'primary has all rows');
is($s_fp, $p_fp, 'count, sums and row hash of ct are equal on the standby');
is( $standby->safe_psql('postgres', "SELECT pg_relation_size('ct')"),
	$primary->safe_psql('postgres', "SELECT pg_relation_size('ct')"),
	'the file has the same length on both nodes (redo extended it)');
cmp_ok(
	$standby->safe_psql('postgres', "SELECT pg_relation_size('ct')"),
	'>', 20 * $size0,
	'the standby file is far longer than at the backup (V6)');
is( $standby->safe_psql('postgres',
		'SELECT count(*), sum(row_count) FROM kingres.columnar_stripes(\'ct\')'),
	$primary->safe_psql('postgres',
		'SELECT count(*), sum(row_count) FROM kingres.columnar_stripes(\'ct\')'),
	'same stripes');
is(check_rows($standby), '', 'columnar_check finds nothing on the standby');
# a filtered, projected read takes the skipping path on the standby too
is( $standby->safe_psql('postgres',
		"SELECT count(*), sum(a) FROM ct WHERE id BETWEEN 100000 AND 250000"),
	$primary->safe_psql('postgres',
		"SELECT count(*), sum(a) FROM ct WHERE id BETWEEN 100000 AND 250000"),
	'a range read with chunk skipping agrees');

###############################################################################
# 2. a standby only reads
###############################################################################

my ($ret, $out, $err);
for my $stmt (
	"INSERT INTO ct VALUES (1, 1, 'x', 1, now(), true)",
	"CREATE TABLE s1 (i int) USING columnar",
	"COPY ct FROM STDIN",
	"TRUNCATE ct")
{
	($ret, $out, $err) = $standby->psql('postgres', $stmt);
	isnt($ret, 0, "standby refuses: $stmt");
	like($err, qr/read-only transaction|during recovery/,
		"  with a read-only error");
}
# functions that would write: refused by locks or by the read-only check,
# never by a PANIC in a critical section
for my $stmt (
	"SELECT kingres.columnar_set_options('ct', compression => 'pglz')",
	"SELECT kingres.columnar_compact('ct')",
	"SELECT kingres.columnar_check(true)",
	"VACUUM ct")
{
	($ret, $out, $err) = $standby->psql('postgres', $stmt);
	isnt($ret, 0, "standby refuses: $stmt");
	unlike($err, qr/server closed the connection|terminating/,
		"  and survives it");
}
# a stale catalog row (as if a drop was not reconciled) on the primary: the
# reconcile function must not try to delete it on the standby
$primary->safe_psql('postgres',
	"INSERT INTO kingres.columnar_stripe VALUES (999999, 1, 0, 1, 1, 1, 1, 1, 1, 1, 1, '\\x00', '\\x00')"
);
$primary->wait_for_catchup($standby);
($ret, $out, $err) =
  $standby->psql('postgres', "SELECT kingres.columnar_reconcile()");
unlike($err, qr/server closed the connection|terminating/,
	'columnar_reconcile on the standby does not crash it');
isnt($standby->safe_psql('postgres', 'SELECT pg_is_in_recovery()'),
	'f', 'standby is still a standby');
$primary->safe_psql('postgres',
	"DELETE FROM kingres.columnar_stripe WHERE filenode = 999999");

# reads of every kind on the standby: EXPLAIN ANALYZE, COPY TO, a cursor
$standby->safe_psql('postgres',
	"EXPLAIN (ANALYZE, BUFFERS) SELECT sum(a) FROM ct WHERE a > 10");
my $copied = $standby->safe_psql('postgres',
	"COPY (SELECT * FROM ct WHERE id <= 5000) TO STDOUT");
is(scalar(my @copied = split(/\n/, $copied)), 5000,'COPY TO on the standby');
$standby->safe_psql('postgres',
	"BEGIN; DECLARE c CURSOR FOR SELECT * FROM ct; FETCH 100 FROM c; COMMIT");
is($standby->safe_psql('postgres', fp('ct')), $p_fp,
	'reads on the standby changed nothing');
log_is_clean($standby, 'standby after reads and refused writes');

###############################################################################
# 3. standby queries across a VACUUM that truncates the file tail
###############################################################################

$primary->safe_psql('postgres', "CREATE TABLE tr $cols USING columnar");
$primary->safe_psql('postgres',
	"SELECT kingres.columnar_set_options('tr', stripe_row_count => 1000, chunk_row_count => 500)");
$primary->safe_psql('postgres', gen('tr', 1, 20000));
# 200000 rows are written (stripes are flushed at 1000 rows) and rolled back:
# the file ends in a long run of pages no stripe owns.
$primary->safe_psql('postgres',
	"BEGIN; " . gen('tr', 20001, 220000) . "; ROLLBACK");
my $before = $primary->safe_psql('postgres', "SELECT pg_relation_size('tr')");
$primary->wait_for_catchup($standby);
is( $standby->safe_psql('postgres', "SELECT pg_relation_size('tr')"),
	$before, 'standby has the dead pages too before the VACUUM');
my $tr_fp = $primary->safe_psql('postgres', fp('tr'));
like($tr_fp, qr/^20000\|/, 'the rolled-back load is not visible');

# session A holds tr; session B reads ct, a table the VACUUM does not touch
my $logpos = -s $standby->logfile;
my $sa = $standby->background_psql('postgres');
$sa->query_safe(
	"BEGIN ISOLATION LEVEL REPEATABLE READ; DECLARE c CURSOR FOR SELECT id, t FROM tr; FETCH 10 FROM c;"
);
my $sb = $standby->background_psql('postgres');
$sb->query_safe(
	"BEGIN ISOLATION LEVEL REPEATABLE READ; DECLARE c CURSOR FOR SELECT id, t FROM ct; FETCH 10 FROM c;"
);

$primary->safe_psql('postgres', 'VACUUM tr');
my $after = $primary->safe_psql('postgres', "SELECT pg_relation_size('tr')");
cmp_ok($after, '<', $before / 4, 'VACUUM truncated the dead tail on the primary');
# the replay waits at most max_standby_streaming_delay, then cancels A
$primary->wait_for_catchup($standby);
is( $standby->safe_psql('postgres', "SELECT pg_relation_size('tr')"),
	$after, 'the standby truncated its file to the same length');

# A is idle in a transaction, so the conflict ends its session
$standby->wait_for_log(
	qr/conflict with recovery/, $logpos);
pass('the query holding the truncated table was cancelled by the recovery conflict');
eval { $sa->query("FETCH 10 FROM c;") };
ok($@ ne '' || $sa->{stderr} =~ /conflict|terminating|closed/,
	'the cancelled session cannot go on');
eval { $sa->quit };

($out, $ret) = $sb->query("FETCH 10 FROM c;");
is($sb->{stderr}, '', 'the query on another table was not cancelled');
$sb->{stderr} = '';
$sb->query_safe("COMMIT");
$sb->quit;

is($standby->safe_psql('postgres', fp('tr')), $tr_fp,
	'tr reads the same on the standby after the truncation');
is(check_rows($standby), '', 'columnar_check finds nothing on the standby');
# the VACUUM truncated nothing a stripe needs: new loads, then replay goes on
$primary->safe_psql('postgres', gen('tr', 20001, 30000));
$primary->safe_psql('postgres', gen('ct', 422001, 430000));
$primary->wait_for_catchup($standby);
for my $t ('tr', 'ct')
{
	is($standby->safe_psql('postgres', fp($t)),
		$primary->safe_psql('postgres', fp($t)),
		"$t is equal on both nodes after the loads that follow the truncation");
}
# a VACUUM with nothing to truncate, and a second round with no reader
$primary->safe_psql('postgres',
	"BEGIN; " . gen('tr', 30001, 90000) . "; ROLLBACK");
$primary->safe_psql('postgres', 'VACUUM tr');
$primary->wait_for_catchup($standby);
is($standby->safe_psql('postgres', "SELECT pg_relation_size('tr')"),
	$primary->safe_psql('postgres', "SELECT pg_relation_size('tr')"),
	'a truncation with no reader replays without a cancel');
is($standby->safe_psql('postgres', fp('tr')),
	$primary->safe_psql('postgres', fp('tr')), 'tr still equal');
log_is_clean($standby, 'standby after the truncations');

###############################################################################
# 4. promotion
###############################################################################

my $ct_fp = $primary->safe_psql('postgres', fp('ct'));
my $tr2_fp = $primary->safe_psql('postgres', fp('tr'));
$primary->wait_for_catchup($standby);
$standby->promote;
$standby->poll_query_until('postgres', 'SELECT NOT pg_is_in_recovery()')
  or die 'promotion did not finish';
is($standby->safe_psql('postgres', fp('ct')), $ct_fp, 'promoted node has ct');
is($standby->safe_psql('postgres', fp('tr')), $tr2_fp, 'promoted node has tr');
# a load on the promoted node: the metapage counters came from redo, a stale
# one would collide with an existing stripe id
$standby->safe_psql('postgres', gen('ct', 500001, 520000));
$standby->safe_psql('postgres', gen('tr', 500001, 503000));
$standby->safe_psql('postgres',
	"BEGIN; " . gen('tr', 600001, 640000) . "; ROLLBACK");
my $new_ct = $standby->safe_psql('postgres', fp('ct'));
like($new_ct, qr/^450000\|/, 'the promoted node accepted the load');
$standby->safe_psql('postgres', 'VACUUM ct');
$standby->safe_psql('postgres', 'VACUUM tr');
is($standby->safe_psql('postgres', fp('ct')), $new_ct, 'VACUUM keeps ct');
my $new_tr = $standby->safe_psql('postgres', fp('tr'));
my $n = $standby->safe_psql('postgres',
	"SELECT kingres.columnar_compact('ct', 5000)");
cmp_ok($n, '>=', 1, 'columnar_compact merged small stripes on the new primary');
is($standby->safe_psql('postgres', fp('ct')), $new_ct, 'compact keeps ct');
$standby->safe_psql('postgres', 'VACUUM FULL tr');
is($standby->safe_psql('postgres', fp('tr')), $new_tr, 'VACUUM FULL keeps tr');
$standby->safe_psql('postgres', 'VACUUM FULL ct');
is($standby->safe_psql('postgres', fp('ct')), $new_ct, 'VACUUM FULL keeps ct');
# A rewrite leaves the catalog rows of the old file until the next
# reconcile (design 5.5, V5); report what is there, then the next VACUUM of
# a columnar table must clear it.
my $orph = check_rows($standby);
note "columnar_check right after VACUUM FULL: [$orph]";
like($orph, qr/^(|orphan_catalog_rows - [0-9]+ stripe catalog rows belong to files no relation uses)$/,
	'after VACUUM FULL nothing is reported but orphan catalog rows');
$standby->safe_psql('postgres', 'VACUUM ct');
is(check_rows($standby), '', 'columnar_check clean on the promoted node');
log_is_clean($standby, 'promoted node');

# the promoted node restarts and recovers its own columnar WAL
$standby->restart;
is($standby->safe_psql('postgres', fp('ct')), $new_ct,
	'ct after a restart of the promoted node');
$primary->stop;
$standby->stop;

###############################################################################
# 5. wal_consistency_checking on the resource manager, on both nodes
###############################################################################

{
	my $conf = "wal_consistency_checking = 'KingresColumnar'\n";
	my $wp = new_primary('wcc_primary', $conf);
	$wp->safe_psql('postgres', 'CREATE EXTENSION kingres_columnar');
	$wp->safe_psql('postgres', "CREATE TABLE w $cols USING columnar");
	$wp->safe_psql('postgres',
		"SELECT kingres.columnar_set_options('w', stripe_row_count => 1000, chunk_row_count => 500)");
	$wp->safe_psql('postgres', gen('w', 1, 3000));
	$wp->backup('b');
	my $ws = new_standby($wp, 'wcc_standby', 'b', $conf);
	is($ws->safe_psql('postgres', 'SHOW wal_consistency_checking'),
		'KingresColumnar', 'the setting names the custom resource manager');

	$wp->safe_psql('postgres', gen('w', 3001, 53000));
	$wp->safe_psql('postgres',
		"BEGIN; " . gen('w', 60001, 80000) . "; ROLLBACK");
	$wp->safe_psql('postgres', 'VACUUM w');
	$wp->safe_psql('postgres',
		"SELECT kingres.columnar_set_options('w', compression => 'pglz')");
	$wp->safe_psql('postgres', gen('w', 53001, 60000));
	$wp->safe_psql('postgres', "SELECT kingres.columnar_compact('w', 5000)");
	$wp->safe_psql('postgres', 'TRUNCATE w');
	$wp->safe_psql('postgres', gen('w', 1, 12000));
	$wp->wait_for_catchup($ws);
	is($ws->safe_psql('postgres', fp('w')), $wp->safe_psql('postgres', fp('w')),
		'w is equal under wal_consistency_checking');
	log_is_clean($ws, 'standby under wal_consistency_checking');
	$ws->safe_psql('postgres', 'SELECT 1');
	$wp->stop;
	$ws->stop;
}

###############################################################################
# 6. pg_rewind of an old primary that wrote columnar data after the promotion
###############################################################################

{
	my $old = new_primary('old_primary');
	$old->safe_psql('postgres', 'CREATE EXTENSION kingres_columnar');
	$old->safe_psql('postgres', "CREATE TABLE r $cols USING columnar");
	$old->safe_psql('postgres',
		"SELECT kingres.columnar_set_options('r', stripe_row_count => 1000, chunk_row_count => 500)");
	$old->safe_psql('postgres', gen('r', 1, 5000));
	$old->backup('b');
	my $new = new_standby($old, 'new_primary', 'b');
	$old->wait_for_catchup($new);

	$new->promote;
	$new->poll_query_until('postgres', 'SELECT NOT pg_is_in_recovery()')
	  or die 'promotion did not finish';
	# pg_rewind needs the new primary to have reached a checkpoint on the new
	# timeline
	$new->safe_psql('postgres', 'CHECKPOINT');

	# divergence: committed columnar data on both, a table only on each, an
	# aborted load and a VACUUM on the old one, a VACUUM FULL on the new one
	$old->safe_psql('postgres', gen('r', 100001, 130000));
	$old->safe_psql('postgres', "CREATE TABLE only_old $cols USING columnar");
	$old->safe_psql('postgres', gen('only_old', 1, 8000));
	$old->safe_psql('postgres',
		"BEGIN; " . gen('r', 900001, 940000) . "; ROLLBACK");
	$old->safe_psql('postgres', 'VACUUM r');
	$old->safe_psql('postgres', 'CHECKPOINT');

	$new->safe_psql('postgres', gen('r', 200001, 217000));
	$new->safe_psql('postgres', "CREATE TABLE only_new $cols USING columnar");
	$new->safe_psql('postgres', gen('only_new', 1, 6000));
	$new->safe_psql('postgres', 'CHECKPOINT');
	my $new_fp = $new->safe_psql('postgres', fp('r'));
	like($new_fp, qr/^22000\|/, 'the new primary has its own rows');

	$old->stop;
	# pg_rewind copies the configuration of the source over the target's, so
	# keep the target's (port, socket directory) and restore it afterwards
	my $conf_keep = $old->basedir . '/postgresql.conf.keep';
	copy($old->data_dir . '/postgresql.conf', $conf_keep) or die "copy: $!";
	my $connstr = $new->connstr('postgres');
	command_ok(
		[
			'pg_rewind', '--debug',
			'--source-server' => $connstr,
			'--target-pgdata' => $old->data_dir,
		],
		'pg_rewind of an old primary with columnar records in its divergent WAL'
	);
	copy($conf_keep, $old->data_dir . '/postgresql.conf') or die "copy: $!";
	my $root = $new->connstr;
	$old->append_conf('postgresql.conf',
		"primary_conninfo = '$root application_name=old_primary'\nhot_standby = on\n");
	$old->set_standby_mode;
	$old->start;
	$new->wait_for_catchup($old);
	is($old->safe_psql('postgres', 'SELECT pg_is_in_recovery()'), 't',
		'the rewound node is a standby');
	is($old->safe_psql('postgres', fp('r')), $new_fp,
		'the rewound node shows the new primary\'s rows of r (and none of its own)');
	is($old->safe_psql('postgres', fp('only_new')),
		$new->safe_psql('postgres', fp('only_new')),
		'a table created on the new primary is there');
	is( $old->safe_psql('postgres',
			"SELECT count(*) FROM pg_class WHERE relname = 'only_old'"),
		'0', 'a table of the divergent branch is gone');
	is(check_rows($old), '', 'columnar_check clean on the rewound node');
	is( $old->safe_psql('postgres', "SELECT pg_relation_size('r')"),
		$new->safe_psql('postgres', "SELECT pg_relation_size('r')"),
		'the file of r has the length of the new primary\'s');

	# and it keeps following
	$new->safe_psql('postgres', gen('r', 300001, 310000));
	$new->wait_for_catchup($old);
	is($old->safe_psql('postgres', fp('r')), $new->safe_psql('postgres', fp('r')),
		'the rewound node replays a later columnar load');
	log_is_clean($old, 'rewound node');
	$old->stop;
	$new->stop;
}

done_testing();
