# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/278_dist_timepart.pl: time partitioning policies on a user-partitioned distributed table (design/analytics-phase12-storage.md, section 11;
# increments St-D1a and St-D1b).  kingres_timepart 0.3 runs a policy of a distributed table with the partition functions of kingres_shard on
# the map group primary and converts the leaves of old partitions to columnar storage on the nodes that hold them.
#
#   A. a run creates the partitions ahead on every node, a later run converts the old ones (every leaf, with REPLICA IDENTITY FULL) and a
#      later one retires them; the rows are the same all along
#   B. the leaf budget: a policy that needs more leaves than kingres_shard.max_partition_leaves is refused, and so is the partition that
#      would pass it
#   C. a group that a bucket move or a conversion claims is retried: the run is partial and comes back soon; the claim keeps moves and
#      partition changes away, and a move keeps the claim away
#   D. a conversion that a leaf refuses leaves the partition mixed, which reads right and is converted by the next run
#   E. a converted table moves its buckets (columnar leaves included) and the policy goes on afterwards
#   F. retention by detach makes a distributed table of the old partition; the owner who is not a superuser cannot run it
#   H. the budget of an attach, the lease and the renewal of a claim, a name of multibyte letters, the way back to heap, readers during a conversion, who may convert a leaf,
#      DDL on a node that is not the map group primary, a driver that is killed
#   G. check_cluster reports a claim that is stale (not renewed for the lease, or held by a session that is gone); the report of a new partition carries the leaf budget
#
# Needs kingres_columnar and kingres_timepart built and installed into the same installation as kingres_shard.

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use IPC::Run ();
use KingresShardTest qw(make_cluster run_sql prepared_count wait_for_fault_file start_psql finish_psql);

my @nodes = make_cluster('dtp', 3, logical => 1, extra => "shared_preload_libraries = 'kingres_shard,kingres_columnar'\n");
my ($n1, $n2, $n3) = @nodes;
sub q1 { my ($node, $sql) = @_; return $node->safe_psql('postgres', $sql); }
$_->safe_psql('postgres', 'CREATE EXTENSION kingres_columnar; CREATE EXTENSION kingres_timepart') for @nodes;
# short waits between the attempts of an action, and two attempts
$n1->append_conf('postgresql.conf', "kingres_timepart.lock_retries = 2\nkingres_timepart.retry_sleep_min_ms = 10\nkingres_timepart.retry_sleep_max_ms = 20\n");
$n1->reload;

# the leaves of a partition on every node: how many, how many are columnar, how many have REPLICA IDENTITY FULL
sub leaves
{
	my ($p) = @_;
	my ($n, $c, $f) = (0, 0, 0);
	for my $node (@nodes)
	{
		my ($a, $b, $d) = split /\|/, q1($node, qq{SELECT count(*) || '|' || count(*) FILTER (WHERE a.amname = 'columnar') || '|' || count(*) FILTER (WHERE c.relreplident = 'f')
			FROM pg_class c JOIN pg_am a ON a.oid = c.relam WHERE c.relkind = 'r' AND c.relispartition AND c.relname ~ '^${p}_b[0-9]{4}\$'});
		$n += $a; $c += $b; $f += $d;
	}
	return "$n|$c|$f";
}
sub rows_in
{
	my ($p) = @_;
	my $s = 0;
	for my $node (@nodes)
	{
		for my $l (split /,/, q1($node, qq{SELECT coalesce(string_agg(c.oid::regclass::text, ','), '') FROM pg_class c WHERE c.relkind = 'r' AND c.relname ~ '^${p}_b[0-9]{4}\$'}))
		{
			$s += q1($node, "SELECT count(*) FROM $l");
		}
	}
	return $s;
}
sub parts { my ($t) = @_; return q1($n1, qq{SELECT coalesce(string_agg(partition_name, ',' ORDER BY partition_name), '') FROM kingres.dist_partition WHERE parent = 'public.$t'::regclass}); }
sub last_run
{
	my ($t) = @_;
	return q1($n1, qq{SELECT state FROM kingres.timepart_run WHERE table_name = '$t' ORDER BY run_id DESC LIMIT 1});
}
sub actions
{
	my ($t) = @_;
	return q1($n1, qq{SELECT coalesce(string_agg(a.kind || ':' || a.partition_name || ':' || a.state, ' ' ORDER BY a.seq), '') FROM kingres.timepart_run_action a
		WHERE a.run_id = (SELECT max(run_id) FROM kingres.timepart_run WHERE table_name = '$t')});
}
sub totals { my ($t) = @_; return q1($_[1] // $n1, "SELECT count(*) || ',' || coalesce(sum(id), 0) || ',' || coalesce(sum(v), 0) FROM public.$t"); }
sub run_policy
{
	my ($t, $as_of) = @_;
	return run_sql($n1, qq{CALL kingres.timepart_run('public.$t', '$as_of UTC')});
}

$n1->safe_psql('postgres', q{
	CREATE TABLE public.ev (cust int NOT NULL, ts timestamptz NOT NULL, id int NOT NULL, v int, PRIMARY KEY (cust, ts, id)) PARTITION BY RANGE (ts);
	CREATE TABLE public.ev_p20260301 PARTITION OF public.ev FOR VALUES FROM ('2026-03-01 00:00:00+00') TO ('2026-03-02 00:00:00+00');
	INSERT INTO public.ev SELECT g % 11, timestamptz '2026-03-01 00:00:00+00' + (g % 8) * interval '3 hours', g, g % 50 FROM generate_series(1, 300) g;
	SELECT kingres.create_distributed_table('public.ev', 'cust', bucket_count => 6);
});
my $coloc = q1($n1, q{SELECT colocation_id FROM kingres.dist_table WHERE relid = 'public.ev'::regclass});

# ---------------------------------------------------------------------------
# A. create ahead, convert, retire
# ---------------------------------------------------------------------------
my ($rc, $out, $err) = run_sql($n1, q{SELECT kingres.timepart_set_policy('public.ev', '1 day', create_ahead => '3 days', retention => '5 days', columnar_after => '2 days')});
is($rc, 0, 'A: a policy on a distributed table') or diag($err);
is(q1($n1, q{SELECT count(*) FROM kingres.timepart_plan('public.ev', '2026-03-03 12:00 UTC') WHERE kind = 'create'}), '5', 'A: the plan creates five partitions');
is(q1($n1, q{SELECT string_agg(partition_name, ',' ORDER BY seq) FROM kingres.timepart_plan('public.ev', '2026-03-03 12:00 UTC') WHERE kind = 'create'}),
	'ev_p20260302,ev_p20260303,ev_p20260304,ev_p20260305,ev_p20260306', 'A: ... with the generated names');
my $want = totals('ev');
($rc, $out, $err) = run_policy('ev', '2026-03-03 12:00');
is($rc, 0, 'A: the first run') or diag($err);
is(last_run('ev'), 'done', 'A: it is done');
is(actions('ev'), join(' ', map { "create:ev_p202603$_:done" } qw(02 03 04 05 06)), 'A: it created five partitions');
is(parts('ev'), 'ev_p20260301,ev_p20260302,ev_p20260303,ev_p20260304,ev_p20260305,ev_p20260306', 'A: the catalog has them');
is(leaves('ev_p20260304'), '6|0|0', 'A: a new partition has a leaf for each of the six buckets, over the nodes, all heap');
for my $n (@nodes)
{
	is(q1($n, q{SELECT count(*) FROM kingres.dist_partition WHERE parent = 'public.ev'::regclass}), '6', 'A: the catalog of ' . $n->name . ' has the six partitions');
}
($rc, $out, $err) = run_sql($n3, q{INSERT INTO public.ev SELECT g % 11, timestamptz '2026-03-04 00:00:00+00' + (g % 6) * interval '3 hours', 1000 + g, g FROM generate_series(1, 60) g});
is($rc, 0, 'A: rows for the new partitions from another node') or diag($err);
$want = totals('ev');
is(totals('ev', $_), $want, 'A: the table is the same from ' . $_->name) for @nodes;
is(q1($n1, q{SELECT count(*) FROM kingres.timepart_check('1 hour', '2026-03-03 12:00 UTC')}), '0', 'A: timepart_check has nothing to say');
($rc, $out, $err) = run_sql($n1, q{SELECT * FROM kingres.timepart_partitions('public.ev')});
isnt($rc, 0, 'A: timepart_partitions does not answer for a distributed table'); like($err, qr/timepart_dist_partitions/, 'A: and names the function that does');

# the run of two days later converts what is older than two days
($rc, $out, $err) = run_policy('ev', '2026-03-05 12:00');
is($rc, 0, 'A: the second run') or diag($err);
is(last_run('ev'), 'done', 'A: it is done');
like(actions('ev'), qr/^create:ev_p20260307:done create:ev_p20260308:done convert:ev_p20260301:done convert:ev_p20260302:done$/, 'A: it created two partitions and converted two');
is(leaves('ev_p20260301'), '6|6|6', 'A: every leaf of the first partition is columnar, with REPLICA IDENTITY FULL');
is(leaves('ev_p20260302'), '6|6|6', 'A: and of the second');
is(leaves('ev_p20260303'), '6|0|0', 'A: the third is heap');
is(totals('ev', $_), $want, 'A: no row is lost or doubled, from ' . $_->name) for @nodes;
is(q1($n1, q{SELECT access_method || '|' || leaves || '|' || columnar_leaves FROM kingres.timepart_dist_partitions('public.ev') WHERE partition_name = 'ev_p20260301'}), 'columnar|6|6', 'A: timepart_dist_partitions says columnar');
is(q1($n1, q{SELECT access_method FROM kingres.timepart_dist_partitions('public.ev') WHERE partition_name = 'ev_p20260304'}), 'heap', 'A: and heap');
like(q1($n1, q{SELECT detail FROM kingres.timepart_run_action WHERE kind = 'convert' AND partition_name = 'ev_p20260301'}), qr/converted to columnar: 6 leaves converted to columnar on 3 nodes, 0 already were/, 'A: the history says what was done');
is(q1($n1, q{SELECT count(*) FROM kingres.table_operation_lock}), '0', 'A: no claim is left');
is(q1($n1, q{SELECT count(*) FROM kingres.check_cluster()}), '0', 'A: check_cluster() finds nothing');
($rc, $out, $err) = run_policy('ev', '2026-03-05 12:00');
is(last_run('ev'), 'done', 'A: a run again at the same time is done');
is(actions('ev'), '', 'A: and has nothing to do (the converted partitions are not converted again)');

# retention drops the old partitions, columnar leaves included
($rc, $out, $err) = run_policy('ev', '2026-03-09 12:00');
is($rc, 0, 'A: the third run') or diag($err);
is(last_run('ev'), 'done', 'A: it is done');
like(actions('ev'), qr/drop:ev_p20260301:done drop:ev_p20260302:done drop:ev_p20260303:done/, 'A: it drops the partitions older than five days');
is(leaves('ev_p20260301'), '0|0|0', 'A: their leaves are gone on every node');
is(rows_in('ev_p20260301') + rows_in('ev_p20260302'), 0, 'A: and their rows');
is(q1($n1, q{SELECT count(*) FROM public.ev WHERE ts < '2026-03-04 00:00+00'}), '0', 'A: nothing older than the retention is read');
is(q1($n1, q{SELECT count(*) FROM kingres.check_cluster()}), '0', 'A: check_cluster() finds nothing after the drops');


# ---------------------------------------------------------------------------
# B. the leaf budget
# ---------------------------------------------------------------------------
$n1->safe_psql('postgres', q{
	CREATE TABLE public.bd (cust int NOT NULL, ts date NOT NULL, id int NOT NULL, v int, PRIMARY KEY (cust, ts, id)) PARTITION BY RANGE (ts);
	CREATE TABLE public.bd_p20260301 PARTITION OF public.bd FOR VALUES FROM ('2026-03-01') TO ('2026-03-02');
	SELECT kingres.create_distributed_table('public.bd', 'cust', bucket_count => 6);
});
is(q1($n1, q{SELECT buckets || ',' || partitions || ',' || leaves || ',' || max_leaves FROM kingres.partition_leaf_budget('public.bd')}), '6,1,6,8192', 'B: the budget of a table: six buckets, one partition, the limit');
$n1->append_conf('postgresql.conf', "kingres_shard.max_partition_leaves = 18
");
$n1->reload;
($rc, $out, $err) = run_sql($n1, q{SELECT kingres.timepart_set_policy('public.bd', '1 day', create_ahead => '3 days', retention => '5 days')});
isnt($rc, 0, 'B: a policy that needs more leaves than the limit is refused');
like($err, qr/needs about 54 leaves in a steady state \(6 buckets, 9 partitions\); kingres_shard.max_partition_leaves is 18/, 'B: with the numbers');
is(q1($n1, q{SELECT count(*) FROM kingres.timepart_policy WHERE table_name = 'bd'}), '0', 'B: and no policy is made');
($rc, $out, $err) = run_sql($n1, q{SELECT kingres.timepart_set_policy('public.bd', '1 day', create_ahead => '1 day', retention => '1 day')});
is($rc, 0, 'B: a policy that fits is accepted') or diag($err);
($rc, $out, $err) = run_sql($n1, q{SELECT kingres.create_partition('public.bd', 'bd_p20260302', 'FROM (''2026-03-02'') TO (''2026-03-03'')')});
is($rc, 0, 'B: the second partition is made (12 leaves)') or diag($err);
($rc, $out, $err) = run_sql($n1, q{SELECT kingres.create_partition('public.bd', 'bd_p20260303', 'FROM (''2026-03-03'') TO (''2026-03-04'')')});
is($rc, 0, 'B: the third is made (18 leaves, the limit)') or diag($err);
($rc, $out, $err) = run_sql($n1, q{SELECT kingres.create_partition('public.bd', 'bd_p20260304', 'FROM (''2026-03-04'') TO (''2026-03-05'')')});
isnt($rc, 0, 'B: the fourth would pass the limit and is refused');
like($err, qr/table "public.bd" would have 24 leaves \(6 buckets, 4 partitions\); kingres_shard.max_partition_leaves is 18/, 'B: with the numbers');
is(leaves('bd_p20260304'), '0|0|0', 'B: no leaf of it was made');
($rc, $out, $err) = run_sql($n1, q{SELECT kingres.create_time_partitions('public.bd', '1 day', '2026-03-05 UTC', '2026-03-01 UTC')});
isnt($rc, 0, 'B: create_time_partitions refuses at the limit too');
like($err, qr/would have 24 leaves/, 'B: with the same message');
# a policy run that meets the limit fails its action and says why; the table stays as it was
($rc, $out, $err) = run_sql($n1, q{CALL kingres.timepart_run('public.bd', '2026-03-03 12:00 UTC')});
is($rc, 0, 'B: a run at the limit') or diag($err);
is(last_run('bd'), 'partial', 'B: is partial');
like(q1($n1, q{SELECT string_agg(a.kind || ':' || a.state || ':' || a.detail, '|') FROM kingres.timepart_run_action a WHERE a.run_id = (SELECT max(run_id) FROM kingres.timepart_run WHERE table_name = 'bd') AND a.kind = 'create'}),
	qr/create:failed:.*would have 24 leaves/, 'B: the action that would pass the limit failed, with the numbers');
($rc, $out, $err) = run_sql($n1, q{SELECT * FROM kingres.create_time_partitions('public.bd', '1 day', '2026-03-03 UTC', '2026-03-01 UTC')});
is($rc, 0, 'B: create_time_partitions on a distributed table makes nothing when the partitions exist') or diag($err);
is(q1($n1, q{SELECT kingres.drop_old_time_partitions('public.bd', '2026-03-03 UTC')}), '2', 'B: drop_old_time_partitions drops the two old partitions on every node');
is(leaves('bd_p20260301') . leaves('bd_p20260302'), '0|0|00|0|0', 'B: ... leaves and all');
is(parts('bd'), 'bd_p20260303', 'B: one partition is left');
$n1->append_conf('postgresql.conf', "kingres_shard.max_partition_leaves = 8192
");
$n1->reload;


# the name of a generated partition of a distributed table fits its leaves (57 bytes, not the 63 of an ordinary table)
{
	my $long = 'bdl_' . ('x' x 46);
	$n1->safe_psql('postgres', qq{
		CREATE TABLE public.$long (cust int NOT NULL, ts date NOT NULL, id int NOT NULL, PRIMARY KEY (cust, ts, id)) PARTITION BY RANGE (ts);
		CREATE TABLE public.bdl_first PARTITION OF public.$long FOR VALUES FROM ('2026-03-01') TO ('2026-03-02');
		SELECT kingres.create_distributed_table('public.$long', 'cust', bucket_count => 6);
		SELECT kingres.timepart_set_policy('public.$long', '1 day', create_ahead => '1 day');
	});
	($rc, $out, $err) = run_policy($long, '2026-03-02 12:00');
	is($rc, 0, 'B: a run on a table with a long name') or diag($err);
	is(last_run($long), 'done', 'B: is done');
	is(q1($n1, qq{SELECT string_agg(length(partition_name)::text, ',' ORDER BY partition_name) FROM kingres.dist_partition WHERE parent = 'public.$long'::regclass AND partition_name <> 'bdl_first'}),
		'57,57', 'B: the generated names are cut to 57 bytes, so that the leaves fit');
}

# ---------------------------------------------------------------------------
# C. a group that is claimed: retry later; the claim keeps moves and partition changes away, and a move keeps the claim away
# ---------------------------------------------------------------------------
my $op = q1($n1, q{SELECT kingres.begin_leaf_conversion('public.ev', 'ev_p20260312')});
like($op, qr/^\d+$/, 'C: the group of the table is claimed for the conversion of a partition');
is(q1($n1, q{SELECT kind || '|' || phase FROM kingres.table_operation WHERE op_id = } . $op), 'convert_leaf|done', 'C: the claim is a record of its own kind');
($rc, $out, $err) = run_sql($n1, q{SELECT kingres.begin_leaf_conversion('public.ev', 'ev_p20260312')});
isnt($rc, 0, 'C: a second claim is refused'); like($err, qr/is busy/, 'C: the group is busy');
my $b0 = q1($n1, "SELECT kingres.bucket_for($coloc, 3)");
my $g0 = q1($n1, "SELECT group_id FROM kingres.bucket_map WHERE colocation_id = $coloc AND bucket = $b0");
($rc, $out, $err) = run_sql($n1, "SELECT kingres.move_bucket($coloc, $b0, " . (($g0 + 1) % 3) . ")");
isnt($rc, 0, 'C: a bucket move is refused'); like($err, qr/used by table operation $op/, 'C: it names the claim');
($rc, $out, $err) = run_sql($n1, q{SELECT kingres.create_partition('public.ev', 'ev_p20260399', 'FROM (''2026-03-30'') TO (''2026-03-31'')')});
isnt($rc, 0, 'C: create_partition is refused'); like($err, qr/colocation group \d+ is busy/, 'C: the group is busy');
($rc, $out, $err) = run_sql($n1, q{SELECT kingres.drop_partition('public.ev', 'ev_p20260312')});
isnt($rc, 0, 'C: drop_partition is refused'); like($err, qr/colocation group \d+ is busy/, 'C: the group is busy');
($rc, $out, $err) = run_sql($n1, q{ALTER TABLE public.ev ADD COLUMN extra int});
isnt($rc, 0, 'C: and so is a change of the table');
my $parts_before = parts('ev');
($rc, $out, $err) = run_policy('ev', '2026-03-11 12:00');
is($rc, 0, 'C: a run while the group is claimed ends') or diag($err);
is(last_run('ev'), 'partial', 'C: it is partial');
is(q1($n1, q{SELECT count(*) FROM kingres.timepart_run_action a WHERE a.run_id = (SELECT max(run_id) FROM kingres.timepart_run WHERE table_name = 'ev') AND a.state = 'failed' AND a.detail LIKE 'lock_timeout: retry_later: %'}),
	'6', 'C: its six actions (two creates, two conversions, two drops) failed with retry_later');
like(q1($n1, q{SELECT a.detail FROM kingres.timepart_run_action a WHERE a.run_id = (SELECT max(run_id) FROM kingres.timepart_run WHERE table_name = 'ev') AND a.kind = 'create' ORDER BY a.seq LIMIT 1}),
	qr/attempt 2 of 2/, 'C: a create was tried twice');
like(q1($n1, q{SELECT a.detail FROM kingres.timepart_run_action a WHERE a.run_id = (SELECT max(run_id) FROM kingres.timepart_run WHERE table_name = 'ev') AND a.kind = 'convert' ORDER BY a.seq LIMIT 1}),
	qr/attempt 1 of 1/, 'C: a conversion is tried once in a run');
is(parts('ev'), $parts_before, 'C: nothing changed');
is(q1($n1, q{SELECT (next_run_at = timestamptz '2026-03-11 12:05:00+00')::text FROM kingres.timepart_policy WHERE table_name = 'ev'}), 'true', 'C: the policy comes back in five minutes, not after run_every');
is(q1($n1, q{SELECT count(*) FROM kingres.timepart_check('1 hour', '2026-03-11 12:00 UTC') WHERE table_name = 'ev' AND problem = 'last_run_failed'}), '1', 'C: timepart_check reports the run');
q1($n1, "SELECT kingres.end_leaf_conversion($op)");
is(q1($n1, q{SELECT count(*) FROM kingres.table_operation_lock}), '0', 'C: the claim is released');
is(q1($n1, q{SELECT finished_at IS NOT NULL FROM kingres.table_operation WHERE op_id = } . $op), 't', 'C: and its record is closed');
($rc, $out, $err) = run_policy('ev', '2026-03-11 12:00');
is($rc, 0, 'C: the run again') or diag($err);
is(last_run('ev'), 'done', 'C: is done');
is(actions('ev'), 'create:ev_p20260313:done create:ev_p20260314:done convert:ev_p20260307:done convert:ev_p20260308:done drop:ev_p20260304:done drop:ev_p20260305:done',
	'C: it made, converted and dropped what the held run could not');
is(leaves('ev_p20260307'), '6|6|6', 'C: the leaves are columnar');

# a bucket move holds the group: a conversion waits for it (retry later), a claim is refused
{
	my $gate = "$PostgreSQL::Test::Utils::tmp_check/gate_c_$$";
	PostgreSQL::Test::Utils::append_to_file($gate, 'x');
	my $b = q1($n1, "SELECT kingres.bucket_for($coloc, 5)");
	my $g = q1($n1, "SELECT group_id FROM kingres.bucket_map WHERE colocation_id = $coloc AND bucket = $b");
	my $t = ($g + 1) % 3;
	my ($o, $e) = ('', '');
	my $hh = IPC::Run::start([ 'psql', '-X', '-At', '-h', $n1->host, '-p', $n1->port, '-d', 'postgres',
		'-c', "SET kingres_shard.fault_injection = 'move_after_slot:wait:$gate'", '-c', "SELECT kingres.move_bucket($coloc, $b, $t)" ], '>', \$o, '2>', \$e);
	ok(wait_for_fault_file("$gate.reached", 60), 'C: a move is held');
	($rc, $out, $err) = run_sql($n1, q{SELECT kingres.begin_leaf_conversion('public.ev', 'ev_p20260312')});
	isnt($rc, 0, 'C: no claim while a bucket moves'); like($err, qr/is busy/, 'C: the group is busy');
	($rc, $out, $err) = run_policy('ev', '2026-03-13 12:00');
	is($rc, 0, 'C: a run while a bucket moves') or diag($err);
	is(last_run('ev'), 'partial', 'C: is partial');
	like(q1($n1, q{SELECT string_agg(a.kind || ':' || a.state, ' ' ORDER BY a.seq) FROM kingres.timepart_run_action a WHERE a.run_id = (SELECT max(run_id) FROM kingres.timepart_run WHERE table_name = 'ev') AND a.kind = 'convert'}),
		qr/^convert:failed( convert:failed)*$/, 'C: its conversions are retried later');
	like(q1($n1, q{SELECT a.detail FROM kingres.timepart_run_action a WHERE a.run_id = (SELECT max(run_id) FROM kingres.timepart_run WHERE table_name = 'ev') AND a.kind = 'convert' ORDER BY a.seq LIMIT 1}),
		qr/retry_later: colocation group \d+ is busy/, 'C: with the reason');
	is(q1($n1, q{SELECT count(*) FROM kingres.table_operation_lock}), '0', 'C: a refused conversion leaves no claim');
	unlink $gate;
	IPC::Run::finish($hh);
	is($e, '', 'C: the held move ends') or diag($e);
	q1($n1, q{SELECT kingres.cleanup_moved_buckets(true)});
	is(q1($n1, q{SELECT count(*) FROM kingres.check_cluster()}), '0', 'C: check_cluster() finds nothing');
}
($rc, $out, $err) = run_policy('ev', '2026-03-13 12:00');
is(last_run('ev'), 'done', 'C: the run once the move is over is done');

# ---------------------------------------------------------------------------
# D. a leaf that refuses: the partition is mixed, reads right, and is converted by the next run
# ---------------------------------------------------------------------------
{
	my $leaf3 = q1($n1, q{SELECT leaf FROM kingres.partition_leaves('public.ev', 'ev_p20260311') WHERE node_id = } . q1($n3, q{SELECT node_id FROM kingres.local_node}) . ' ORDER BY bucket LIMIT 1');
	ok($leaf3 ne '', "D: node 3 holds a leaf of ev_p20260311 ($leaf3)");
	$n3->safe_psql('postgres', qq{SET kingres_shard.enable_routing = off;
		CREATE FUNCTION public.noop() RETURNS trigger LANGUAGE plpgsql AS \$\$ BEGIN RETURN NEW; END \$\$;
		CREATE TRIGGER mine BEFORE INSERT ON $leaf3 FOR EACH ROW EXECUTE FUNCTION public.noop()});
	my $want = totals('ev');
	($rc, $out, $err) = run_policy('ev', '2026-03-15 12:00');
	is($rc, 0, 'D: a run with a leaf that cannot be converted') or diag($err);
	is(last_run('ev'), 'done', 'D: a refusal is not a failure');
	like(actions('ev'), qr/convert:ev_p20260311:skipped convert:ev_p20260312:done/, 'D: the partition with the leaf is skipped, the other is converted');
	like(q1($n1, q{SELECT a.detail FROM kingres.timepart_run_action a WHERE a.partition_name = 'ev_p20260311' AND a.kind = 'convert' ORDER BY a.run_id DESC LIMIT 1}),
		qr/conversion_refused: .*the trigger "mine" of its own/, 'D: the history says why');
	my ($n, $c) = split /\|/, q1($n1, q{SELECT leaves || '|' || columnar_leaves FROM kingres.timepart_dist_partitions('public.ev') WHERE partition_name = 'ev_p20260311'});
	is($n, 6, 'D: six leaves'); ok($c > 0 && $c < 6, "D: some of them are converted ($c)");
	is(q1($n1, q{SELECT access_method FROM kingres.timepart_dist_partitions('public.ev') WHERE partition_name = 'ev_p20260311'}), 'mixed', 'D: the partition is mixed');
	is(totals('ev', $_), $want, 'D: the table reads right from ' . $_->name) for @nodes;
	is(q1($n1, q{SELECT count(*) FROM kingres.table_operation_lock}), '0', 'D: the claim is released');
	like(q1($n1, q{SELECT detail FROM kingres.timepart_check('1 hour', '2026-03-15 12:00 UTC') WHERE problem = 'convert_blocked'}), qr/ev_p20260311/, 'D: timepart_check names the partition');
	$n3->safe_psql('postgres', qq{SET kingres_shard.enable_routing = off; DROP TRIGGER mine ON $leaf3});
	($rc, $out, $err) = run_policy('ev', '2026-03-15 12:00');
	is(last_run('ev'), 'done', 'D: the next run is done');
	is(actions('ev'), 'convert:ev_p20260311:done', 'D: it converts the partition that is left, and nothing else');
	like(q1($n1, q{SELECT detail FROM kingres.timepart_run_action WHERE partition_name = 'ev_p20260311' AND state = 'done' AND kind = 'convert' ORDER BY run_id DESC LIMIT 1}),
		qr/^converted to columnar: [1-5] leaves converted to columnar on 3 nodes, [1-5] already were/, 'D: only the leaves that were not converted');
	is(leaves('ev_p20260311'), '6|6|6', 'D: the partition is columnar now');
	is(totals('ev', $_), $want, 'D: the table is the same from ' . $_->name) for @nodes;
}

# ---------------------------------------------------------------------------
# E. a table with columnar leaves moves its buckets, and the policy goes on
# ---------------------------------------------------------------------------
{
	my $want = totals('ev');
	my $b = q1($n1, "SELECT kingres.bucket_for($coloc, 7)");
	my $g = q1($n1, "SELECT group_id FROM kingres.bucket_map WHERE colocation_id = $coloc AND bucket = $b");
	my $t = ($g + 1) % 3;
	($rc, $out, $err) = run_sql($n1, "SELECT kingres.move_bucket($coloc, $b, $t)");
	is($rc, 0, 'E: a bucket with columnar leaves moves') or diag($err);
	q1($n1, q{SELECT kingres.cleanup_moved_buckets(true)});
	is(totals('ev', $_), $want, 'E: the table is the same from ' . $_->name) for @nodes;
	is(q1($nodes[$t], qq{SELECT count(*) FROM pg_class c JOIN pg_am a ON a.oid = c.relam WHERE c.relname = ('ev_p20260311_b' || lpad('$b', 4, '0')) AND a.amname = 'columnar' AND c.relreplident = 'f'}), '1', 'E: the leaf arrived columnar, with REPLICA IDENTITY FULL');
	is(leaves('ev_p20260311'), '6|6|6', 'E: the partition is still columnar everywhere');
	($rc, $out, $err) = run_policy('ev', '2026-03-17 12:00');
	is(last_run('ev'), 'done', 'E: the policy runs after the move');
	like(actions('ev'), qr/create:ev_p20260319:done/, 'E: and creates the partitions ahead');
	is(q1($n1, qq{SELECT count(*) FROM kingres.partition_leaf_storage('public.ev') WHERE node_id = } . q1($nodes[$t], q{SELECT node_id FROM kingres.local_node}) . qq{ AND bucket = $b AND partition_name = 'ev_p20260319' AND access_method = 'heap'}), '1', 'E: a new partition has its leaf on the node the bucket moved to');
	is(q1($n1, q{SELECT count(*) FROM kingres.check_cluster()}), '0', 'E: check_cluster() finds nothing');
}

# ---------------------------------------------------------------------------
# F. retention by detach; the owner who is not a superuser
# ---------------------------------------------------------------------------
$n1->safe_psql('postgres', q{
	CREATE TABLE public.dt (cust int NOT NULL, ts timestamptz NOT NULL, id int NOT NULL, v int, PRIMARY KEY (cust, ts, id)) PARTITION BY RANGE (ts);
	CREATE TABLE public.dt_p20260301 PARTITION OF public.dt FOR VALUES FROM ('2026-03-01 00:00:00+00') TO ('2026-03-02 00:00:00+00');
	INSERT INTO public.dt SELECT g % 11, timestamptz '2026-03-01 00:00:00+00' + (g % 8) * interval '3 hours', g, g FROM generate_series(1, 120) g;
	SELECT kingres.create_distributed_table('public.dt', 'cust', colocate_with => 'public.ev');
	SELECT kingres.timepart_set_policy('public.dt', '1 day', create_ahead => '2 days', retention => '2 days', retention_action => 'detach');
});
my $dt_want = totals('dt');
($rc, $out, $err) = run_policy('dt', '2026-03-02 12:00');
is(last_run('dt'), 'done', 'F: the run that creates the partitions ahead');
($rc, $out, $err) = run_policy('dt', '2026-03-04 12:00');
is(last_run('dt'), 'done', 'F: the run that retires');
like(actions('dt'), qr/detach:dt_p20260301:done/, 'F: the old partition is detached');
is(parts('dt'), 'dt_p20260302,dt_p20260303,dt_p20260304,dt_p20260305,dt_p20260306', 'F: it is not a partition any more');
is(q1($n1, q{SELECT method || '|' || (colocation_id = (SELECT colocation_id FROM kingres.dist_table WHERE relid = 'public.dt'::regclass))::text FROM kingres.dist_table WHERE relid = 'public.dt_p20260301'::regclass}), 'hash|true', 'F: it is a distributed table of the same group');
is(q1($n1, q{SELECT count(*) FROM public.dt_p20260301}) + q1($n1, q{SELECT count(*) FROM public.dt WHERE ts < '2026-03-04 00:00+00'}), '120', 'F: the rows are in the table of its own and nowhere else');

$_->safe_psql('postgres', q{CREATE ROLE dtp_owner LOGIN}) for @nodes;
$n1->safe_psql('postgres', q{ALTER TABLE public.dt OWNER TO dtp_owner; SELECT kingres.timepart_set_policy('public.dt', '1 day', create_ahead => '2 days', retention => '2 days', retention_action => 'detach', columnar_after => '1 day'); GRANT EXECUTE ON FUNCTION kingres.timepart_plan(regclass, timestamptz) TO dtp_owner; SELECT kingres.timepart_grant_driver('dtp_owner')});
my ($so, $se) = ('', '');
$rc = $n1->psql('postgres', q{SELECT count(*) FROM kingres.timepart_plan('public.dt', '2026-03-10 12:00 UTC')}, stdout => \$so, stderr => \$se, extra_params => ['-U', 'dtp_owner']);
is($rc, 0, 'F: the owner plans the policy of its distributed table (the storage of the leaves is read for it)') or diag($se);
cmp_ok($so, '>', 0, 'F: and the plan has actions');
$rc = $n1->psql('postgres', q{CALL kingres.timepart_run('public.dt', '2026-03-10 12:00 UTC')}, stdout => \$so, stderr => \$se, extra_params => ['-U', 'dtp_owner']);
is($rc, 0, 'F: the owner runs its policy') or diag($se);
is(last_run('dt'), 'partial', 'F: the run is partial');
like(q1($n1, q{SELECT string_agg(DISTINCT a.detail, '|') FROM kingres.timepart_run_action a WHERE a.run_id = (SELECT max(run_id) FROM kingres.timepart_run WHERE table_name = 'dt') AND a.state = 'failed'}),
	qr/permission denied: the partitions of a distributed table are changed by a superuser/, 'F: a distributed table is changed by a superuser only');
is(parts('dt'), 'dt_p20260302,dt_p20260303,dt_p20260304,dt_p20260305,dt_p20260306', 'F: nothing changed');
is(q1($n1, q{SELECT count(*) FROM kingres.check_cluster()}), '0', 'F: check_cluster() finds nothing');
is(prepared_count($_), '0', 'F: nothing is prepared on ' . $_->name) for @nodes;


# ---------------------------------------------------------------------------
# G. check_cluster reports a claim that is stale; the report of a change carries the leaf budget
# ---------------------------------------------------------------------------
{
	my $pn = q1($n1, q{SELECT partition_name FROM kingres.dist_partition WHERE parent = 'public.ev'::regclass ORDER BY partition_name LIMIT 1});
	my $stale = sub { return q1($n1, q{SELECT count(*) FROM kingres.check_cluster() WHERE check_name = 'leaf_conversion_stale'}); };
	# a claim whose holder is a live session is not stale
	my $sleeper = start_psql($n1, q{SELECT pg_sleep(120)});
	my $pid = '';
	for (1 .. 100)
	{
		$pid = q1($n1, q{SELECT coalesce(max(pid), 0) FROM pg_stat_activity WHERE query LIKE 'SELECT pg_sleep(120)%' AND pid <> pg_backend_pid()});
		last if $pid > 0;
		select(undef, undef, undef, 0.1);
	}
	cmp_ok($pid, '>', 0, 'G: a session that holds a claim');
	my $o1 = q1($n1, "SELECT kingres.begin_leaf_conversion('public.ev', '$pn', 'columnar', $pid)");
	is($stale->(), '0', 'G: a claim held by a live session is not stale');
	is(q1($n1, "SELECT count(*) FROM kingres.check_cluster() WHERE check_name <> 'leaf_conversion_stale'"), '0', 'G: and check_cluster has nothing else to say');
	# the holder goes: the claim is stale at once, whatever its age
	q1($n1, "SELECT pg_terminate_backend($pid)");
	for (1 .. 100) { last if q1($n1, "SELECT count(*) FROM pg_stat_activity WHERE pid = $pid") eq '0'; select(undef, undef, undef, 0.1); }
	eval { finish_psql($sleeper) };
	is($stale->(), '1', 'G: a claim whose session is gone is reported');
	like(q1($n1, q{SELECT object || ' | ' || detail FROM kingres.check_cluster() WHERE check_name = 'leaf_conversion_stale'}),
		qr/^leaf conversion $o1 of public\.ev \| .*session that holds it is gone.*end_leaf_conversion\($o1\)/, 'G: the report names the claim, the reason and the way out');
	q1($n1, "SELECT kingres.end_leaf_conversion($o1)");
	is($stale->(), '0', 'G: a released claim is not reported');
	# a claim that was not renewed for the lease is stale by its time
	my $o2 = q1($n1, "SELECT kingres.begin_leaf_conversion('public.ev', '$pn')");
	is($stale->(), '0', 'G: a fresh claim without a holder is not stale');
	q1($n1, "UPDATE kingres.table_operation SET updated_at = clock_timestamp() - interval '7 hours' WHERE op_id = $o2");
	is($stale->(), '1', 'G: a claim not renewed for the lease is reported');
	unlike(q1($n1, q{SELECT detail FROM kingres.check_cluster() WHERE check_name = 'leaf_conversion_stale'}), qr/is gone/, 'G: and says nothing of a session');
	q1($n1, "SELECT kingres.end_leaf_conversion($o2)");
	is($stale->(), '0', 'G: released');

	# the report of a change: the leaf budget
	my $codes = sub { my ($kind, $rel) = @_; return q1($n1, "SELECT coalesce(string_agg(code, ','), '') FROM kingres.table_change_report('$kind', '$rel'::regclass)"); };
	is($codes->('create_partition', 'public.bd'), '', 'G: the report of a new partition has no blocker within the budget');
	$n1->append_conf('postgresql.conf', "kingres_shard.max_partition_leaves = 6\n");
	$n1->reload;
	is($codes->('create_partition', 'public.bd'), 'leaf_budget', 'G: it has the leaf budget as a blocker when the partition would pass it');
	like(q1($n1, q{SELECT detail FROM kingres.table_change_report('create_partition', 'public.bd'::regclass) WHERE code = 'leaf_budget'}),
		qr/it would have 12 leaves \(6 buckets, 2 partitions\); kingres_shard.max_partition_leaves is 6/, 'G: with the numbers of create_partition itself');
	is($codes->('attach_partition', 'public.bd'), 'leaf_budget', 'G: and so has the report of an attach');
	is($codes->('drop_partition', 'public.bd'), '', 'G: not the report of a drop');
	$n1->append_conf('postgresql.conf', "kingres_shard.max_partition_leaves = 8192\n");
	$n1->reload;
	is($codes->('create_partition', 'public.bd'), '', 'G: nothing once the limit is back');
}

# ---------------------------------------------------------------------------
# H. the budget of an attach, the lease and the renewal of a claim, a name of multibyte letters, the way back to heap, who may convert a leaf, readers during a
#    conversion, DDL on a node that is not the map group primary, and a driver that is killed
# ---------------------------------------------------------------------------
{
	my $node1_id = q1($n1, q{SELECT node_id FROM kingres.local_node});
	my $stale = sub { return q1($n1, q{SELECT count(*) FROM kingres.check_cluster() WHERE check_name = 'leaf_conversion_stale'}); };
	my $first = sub { my ($cond) = @_; return q1($n1, qq{SELECT partition_name FROM kingres.partition_leaf_storage('public.ev') GROUP BY partition_name HAVING $cond ORDER BY partition_name LIMIT 1}); };

	# the budget of an attach_partition
	$n1->safe_psql('postgres', q{
		CREATE TABLE public.bdx (cust int NOT NULL, ts date NOT NULL, id int NOT NULL, v int, PRIMARY KEY (cust, ts, id));
		SELECT kingres.create_distributed_table('public.bdx', 'cust', colocate_with => 'public.bd');
	});
	$n1->append_conf('postgresql.conf', "kingres_shard.max_partition_leaves = 6\n");
	$n1->reload;
	($rc, $out, $err) = run_sql($n1, q{SELECT kingres.attach_partition('public.bd', 'public.bdx', 'FROM (''2027-01-01'') TO (''2027-01-02'')')});
	isnt($rc, 0, 'H: attach_partition is refused when the table would pass the leaf budget');
	like($err, qr/table "public.bd" would have 12 leaves \(6 buckets, 2 partitions\); kingres_shard.max_partition_leaves is 6/, 'H: with the numbers');
	is(q1($n1, q{SELECT count(*) FROM kingres.dist_table WHERE relid = 'public.bdx'::regclass}), '1', 'H: the table is still a distributed table of its own');
	$n1->append_conf('postgresql.conf', "kingres_shard.max_partition_leaves = 8192\n");
	$n1->reload;
	($rc, $out, $err) = run_sql($n1, q{SELECT kingres.attach_partition('public.bd', 'public.bdx', 'FROM (''2027-01-01'') TO (''2027-01-02'')')});
	is($rc, 0, 'H: and it is attached within the budget') or diag($err);

	# the lease of a claim and its renewal
	my $pn = q1($n1, q{SELECT partition_name FROM kingres.dist_partition WHERE parent = 'public.ev'::regclass ORDER BY partition_name LIMIT 1});
	my $a1 = q1($n1, "SELECT kingres.begin_leaf_conversion('public.ev', '$pn')");
	q1($n1, "UPDATE kingres.table_operation SET updated_at = clock_timestamp() - interval '7 hours' WHERE op_id = $a1");
	my $a2 = q1($n1, "SELECT kingres.begin_leaf_conversion('public.ev', '$pn')");
	isnt($a2, $a1, 'H: a claim that was not renewed for six hours is taken over by the next claim');
	like(q1($n1, "SELECT (finished_at IS NOT NULL)::text || '|' || detail FROM kingres.table_operation WHERE op_id = $a1"), qr/^true\|the claim was not renewed/, 'H: and its record says so');
	is(q1($n1, q{SELECT count(*) FROM kingres.table_operation_lock}), '1', 'H: one claim holds the group');
	q1($n1, "UPDATE kingres.table_operation SET updated_at = clock_timestamp() - interval '5 hours' WHERE op_id = $a2");
	q1($n1, "SELECT kingres.renew_leaf_conversion($a2)");
	is(q1($n1, "SELECT (updated_at > clock_timestamp() - interval '1 minute')::text FROM kingres.table_operation WHERE op_id = $a2"), 'true', 'H: renew_leaf_conversion renews it');
	($rc, $out, $err) = run_sql($n1, "SELECT kingres.begin_leaf_conversion('public.ev', '$pn')");
	isnt($rc, 0, 'H: a renewed claim is not taken over');
	like($err, qr/HINT:.*end_leaf_conversion/s, 'H: the refusal names the action that releases a claim');
	# a bucket move is refused with a hint that fits a claim of a conversion
	my $mb = q1($n1, "SELECT kingres.bucket_for($coloc, 2)");
	my $mg = q1($n1, "SELECT group_id FROM kingres.bucket_map WHERE colocation_id = $coloc AND bucket = $mb");
	($rc, $out, $err) = run_sql($n1, "SELECT kingres.move_bucket($coloc, $mb, " . (($mg + 1) % 3) . ")");
	isnt($rc, 0, 'H: a move is refused while the claim is held');
	like($err, qr/HINT:.*being converted.*end_leaf_conversion/s, 'H: with a hint about the conversion and not "cancel the operation"');
	# DDL on a node that is not the map group primary is refused (the claim is a record of the map group primary: DDL reaches the other nodes through it)
	($rc, $out, $err) = run_sql($n2, q{ALTER TABLE public.ev ADD COLUMN zz int});
	isnt($rc, 0, 'H: DDL on a server that is not the map group primary is refused while the claim is held');
	is(q1($n2, q{SELECT count(*) FROM kingres.table_operation_lock}), '0', 'H: (the claim is not on that server: it is not needed there)');
	($rc, $out, $err) = run_sql($n2, q{SELECT kingres.create_partition('public.ev', 'ev_p_zz', 'FROM (''2031-01-01'') TO (''2031-01-02'')')});
	isnt($rc, 0, 'H: nor can a partition be made there');
	is(q1($n1, q{SELECT count(*) FROM pg_attribute WHERE attrelid = 'public.ev'::regclass AND attname = 'zz'}), '0', 'H: the table is as it was');
	q1($n1, "SELECT kingres.end_leaf_conversion($a2)");
	is(q1($n1, q{SELECT count(*) FROM kingres.table_operation_lock}), '0', 'H: released');

	# a name of letters of two bytes is cut between letters, at 57 bytes
	$n1->safe_psql('postgres', q{
		SELECT format('CREATE TABLE public.%I (cust int NOT NULL, ts date NOT NULL, id int NOT NULL, PRIMARY KEY (cust, ts, id)) PARTITION BY RANGE (ts)', 'tm' || repeat(chr(233), 24)) \gexec
		SELECT format('CREATE TABLE public.%I PARTITION OF public.%I FOR VALUES FROM (''2026-03-01'') TO (''2026-03-02'')', 'first', 'tm' || repeat(chr(233), 24)) \gexec
		SELECT format('SELECT kingres.create_distributed_table(%L, ''cust'', bucket_count => 6)', format('public.%I', 'tm' || repeat(chr(233), 24))) \gexec
		SELECT format('SELECT kingres.timepart_set_policy(%L, ''1 day'', create_ahead => ''1 day'')', format('public.%I', 'tm' || repeat(chr(233), 24))) \gexec
	});
	($rc, $out, $err) = run_sql($n1, q{SELECT format('CALL kingres.timepart_run(%L, %L)', format('public.%I', 'tm' || repeat(chr(233), 24)), '2026-03-02 12:00 UTC') \gexec});
	is($rc, 0, 'H: a run on a table with a name of multibyte letters') or diag($err);
	is(q1($n1, q{SELECT string_agg(octet_length(partition_name)::text || ':' || (partition_name ~ '^tm(.){22}_p2026030[23]$')::text, ',' ORDER BY partition_name)
		FROM kingres.dist_partition WHERE parent = format('public.%I', 'tm' || repeat(chr(233), 24))::regclass AND partition_name <> 'first'}),
		'56:true,56:true', 'H: the names are 56 bytes, cut between letters');

	# an error between the claim and the first leaf (a node without the procedure) releases the claim
	{
		my $hp = $first->(q{bool_and(access_method = 'heap')});
		$n3->safe_psql('postgres', q{ALTER PROCEDURE kingres.convert_leaf(regclass, regclass, name, text[], jsonb, boolean) RENAME TO convert_leaf_away});
		($rc, $out, $err) = run_sql($n1, "SELECT kingres.convert_distributed_partition('public.ev', '$hp', 'columnar')");
		isnt($rc, 0, 'H: a conversion is refused when a node lacks the procedure');
		like($err, qr/conversion_refused: node \d+ does not have kingres_timepart 0.3/, 'H: and says which node');
		is(q1($n1, q{SELECT count(*) FROM kingres.table_operation_lock}), '0', 'H: the claim that was taken before the check is released');
		is(q1($n1, q{SELECT count(*) FROM kingres.table_operation WHERE kind = 'convert_leaf' AND finished_at IS NULL}), '0', 'H: and its record is closed');
		$n3->safe_psql('postgres', q{ALTER PROCEDURE kingres.convert_leaf_away(regclass, regclass, name, text[], jsonb, boolean) RENAME TO convert_leaf});
	}

	# the way back to heap, with readers that must always see every row
	my $col = $first->(q{bool_and(access_method = 'columnar')});
	ok($col ne '', "H: a columnar partition ($col)");
	my $want_rows = (split /,/, totals('ev'))[0];
	# a reader that runs one statement at a time (a statement of its own each, so that no lock of the scan outlives it: a long transaction that reads the table makes the
	# swap give way, which timepart_convert tests) on a server that is not the one that drives
	my $rfile = "$PostgreSQL::Test::Utils::tmp_check/reader_h_$$.sql";
	PostgreSQL::Test::Utils::append_to_file($rfile, "SELECT 'n=' || count(*) FROM public.ev;\nSELECT pg_sleep(0.01);\n" x 200);
	my ($rout, $rerr) = ('', '');
	my $reader = IPC::Run::start([ 'psql', '-X', '-At', '-h', $n2->host, '-p', $n2->port, '-d', 'postgres', '-f', $rfile ], '>', \$rout, '2>', \$rerr);
	for my $try (1 .. 3)
	{
		($rc, $out, $err) = run_sql($n1, "SELECT kingres.convert_distributed_partition('public.ev', '$col', 'heap')");
		last if $rc == 0;
	}
	is($rc, 0, 'H: the leaves of a columnar partition are converted to heap') or diag($err);
	like($out, qr/^\d leaves converted to heap on 3 nodes, \d already were/, 'H: all of them');
	IPC::Run::finish($reader);
	is($rerr, '', 'H: the reader ran without an error') or diag($rerr);
	my @seen = grep { /^n=/ } split /\n/, $rout;
	cmp_ok(scalar(@seen), '>', 50, 'H: and it read the table many times meanwhile');
	is(scalar(grep { $_ ne "n=$want_rows" } @seen), 0, 'H: and always saw every row');
	unlink $rfile;
	is(leaves($col), '6|0|6', 'H: the leaves are heap (and keep REPLICA IDENTITY FULL)');
	is((split /,/, totals('ev'))[0], $want_rows, 'H: no row is lost');
	($rc, $out, $err) = run_sql($n1, "SELECT kingres.convert_distributed_partition('public.ev', '$col', 'columnar')");
	is($rc, 0, 'H: and back to columnar') or diag($err);
	is(leaves($col), '6|6|6', 'H: columnar again');

	# a role that is not a superuser cannot convert a leaf, also with EXECUTE granted
	my $leaf1 = q1($n1, "SELECT leaf FROM kingres.partition_leaves('public.ev', '$pn') WHERE node_id = $node1_id ORDER BY bucket LIMIT 1");
	my ($bkt) = $leaf1 =~ /_b(\d{4})"?$/;
	$n1->safe_psql('postgres', q{GRANT EXECUTE ON PROCEDURE kingres.convert_leaf(regclass, regclass, name, text[], jsonb, boolean) TO dtp_owner});
	my ($so2, $se2) = ('', '');
	my $rc2 = $n1->psql('postgres', "CALL kingres.convert_leaf('public.ev_b$bkt'::regclass, '$leaf1'::regclass)", stdout => \$so2, stderr => \$se2, extra_params => ['-U', 'dtp_owner']);
	isnt($rc2, 0, 'H: convert_leaf by a role that is not a superuser is refused');
	like($se2, qr/permission denied: only a superuser may convert the leaves of a distributed table/, 'H: with the reason');

	# a driver that is killed in the middle of a conversion: its claim is reported, cancel_table_operation releases it, and the leaf that was running ends
	my $heap = $first->(q{bool_and(access_method = 'heap')});
	ok($heap ne '', "H: a partition in heap to convert ($heap)");
	my $hleaf = q1($n1, "SELECT leaf FROM kingres.partition_leaves('public.ev', '$heap') WHERE node_id = $node1_id ORDER BY bucket LIMIT 1");
	$n1->append_conf('postgresql.conf', "kingres_timepart.create_lock_timeout_ms = 60000\n");
	$n1->reload;
	# a session of node 1 holds a lock that the conversion of that leaf waits for
	my $blocker = start_psql($n1, qq{LOCK TABLE $hleaf IN ROW EXCLUSIVE MODE; SELECT pg_sleep(120)});
	ok($n1->poll_query_until('postgres', q{SELECT count(*) = 1 FROM pg_stat_activity WHERE query LIKE 'LOCK TABLE %' AND pid <> pg_backend_pid()}), 'H: a lock is held on a leaf');
	my $driver = start_psql($n1, "SELECT kingres.convert_distributed_partition('public.ev', '$heap')");
	ok($n1->poll_query_until('postgres', q{SELECT count(*) = 1 FROM kingres.table_operation_lock}), 'H: the conversion holds the group');
	my $kop = q1($n1, q{SELECT l.op_id FROM kingres.table_operation_lock l});
	my $kpid = q1($n1, "SELECT spec->>'holder_pid' FROM kingres.table_operation WHERE op_id = $kop");
	cmp_ok($kpid, '>', 0, 'H: the claim names the session that drives it');
	is($stale->(), '0', 'H: a claim of a driver that lives is not stale');
	($rc, $out, $err) = run_sql($n1, "SELECT kingres.cancel_table_operation($kop)");
	isnt($rc, 0, 'H: cancel_table_operation refuses a claim whose session lives');
	q1($n1, "SELECT pg_terminate_backend($kpid)");
	ok($n1->poll_query_until('postgres', "SELECT count(*) = 0 FROM pg_stat_activity WHERE pid = $kpid"), 'H: the driver is killed');
	eval { finish_psql($driver) };
	is($stale->(), '1', 'H: the claim of the killed driver is reported');
	($rc, $out, $err) = run_sql($n1, "SELECT kingres.cancel_table_operation($kop)");
	is($rc, 0, 'H: cancel_table_operation releases it') or diag($err);
	is(q1($n1, q{SELECT count(*) FROM kingres.table_operation_lock}), '0', 'H: the group is free');
	is($stale->(), '0', 'H: nothing is reported any more');
	# the leaf that was running on node 1 ends when its lock comes: nothing is left half done
	q1($n1, q{SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE query LIKE 'LOCK TABLE %' OR query LIKE 'SELECT pg_sleep(120)%'});
	eval { finish_psql($blocker) };
	ok($n1->poll_query_until('postgres', q{SELECT count(*) = 0 FROM pg_stat_activity WHERE query LIKE 'CALL kingres.convert_leaf(%' AND pid <> pg_backend_pid()}), 'H: the leaf that was running ends');
	is(q1($n1, q{SELECT count(*) FROM kingres.check_cluster()}), '0', 'H: check_cluster finds nothing');
	is((split /,/, totals('ev'))[0], $want_rows, 'H: no row is lost');
	$n1->append_conf('postgresql.conf', "kingres_timepart.create_lock_timeout_ms = 5000\n");
	$n1->reload;
}

done_testing();
