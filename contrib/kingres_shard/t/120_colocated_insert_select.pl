# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/120_colocated_insert_select.pl
#
# Step 5 of phase 2b (design/shard-phase2b.md, section 17): the colocated INSERT ... SELECT
# shipped whole (P6, 11.7).  This is the P6 part of the design's 056_bulk_insert (the numbers
# 056 to 069 of the design are taken by tests of 0.5).  Every answer is compared with a single
# ordinary node that gets the same statements, and, with kingres_shard.enable_colocated_insert_select
# off, with the row router (or the refusal) that the same statement gets without the path.  A
# test that passes because nothing was shipped fails on the path assertion, which names the
# custom scan and its "Pushed" line.
#
#   A. which statements are shipped and which are not, each with its reason (DEBUG1);
#   B. the data: copies, joins, a reference table, grouping, DISTINCT, RETURNING;
#   C. ON CONFLICT: DO NOTHING, DO UPDATE with every check of 11.7;
#   D. errors: what the single node says, and nothing left behind;
#   E. prepared statements, generic plans and the switch;
#   F. every group holds exactly the rows of its buckets.

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(run_sql prepared_count);
use KingresShardCompare;

my $c = KingresShardCompare->new('is');
my ($n1, $n2, $n3) = $c->nodes;
my $single = $c->single;

sub table
{
	my ($create, $dist) = @_;
	$c->on_cluster($create . ($dist // ''));
	$c->on_single($create);
}

sub plan_of
{
	my ($node, $sql) = @_;
	my ($rc, $out, $err) = run_sql($node, "EXPLAIN (COSTS OFF) $sql");
	return $rc == 0 ? $out : "ERROR: $err";
}

# Why a statement is not shipped (the DEBUG1 message of the planner), or undef when it is.
sub not_shipped_reason
{
	my ($node, $sql) = @_;
	my ($rc, $out, $err) = run_sql($node, "SET client_min_messages = debug1; EXPLAIN (COSTS OFF) $sql");
	return $1 if $err =~ /INSERT \.\.\. SELECT is not shipped whole: (.*)/;
	return undef;
}

my $PUSHED = 'Kingres Fan-out Write';

# The statement is planned as the shipped statement; if not, the reason is shown.
sub is_shipped
{
	my ($sql, $name) = @_;
	my $ok = like(plan_of($n1, $sql), qr/\Q$PUSHED\E/, $name);
	diag('not shipped: ' . (not_shipped_reason($n1, $sql) // '(no reason)')) unless $ok;
	return $ok;
}

# The rows a node holds in its own partitions, and how many of them belong to its group.
sub local_rows
{
	my ($node, $table, $group, $key) = @_;
	$key //= 'id';
	return $node->safe_psql('postgres', qq{
		SET kingres_shard.enable_routing = off;
		SELECT count(*) || '|' || count(*) FILTER (WHERE m.group_id = $group)
		  FROM $table k JOIN kingres.bucket_map m
		    ON m.colocation_id = (SELECT colocation_id FROM kingres.dist_table WHERE relid = '$table'::regclass)
		   AND m.bucket = kingres.bucket_for('$table'::regclass, k.$key)});
}

sub placement_ok
{
	my ($table, $name, $key) = @_;
	my $total = 0;
	my $i = 0;
	for my $n ($n1, $n2, $n3)
	{
		my ($all, $mine) = split /\|/, local_rows($n, $table, $i, $key);
		is($mine, $all, "$name: every row of node " . ($i + 1) . " belongs to its group");
		$total += $all;
		$i++;
	}
	return $total;
}

sub sorted
{
	return join("\n", sort split /\n/, $_[0]);
}

# A statement shipped whole: the plan says so, the count and the table equal the single node's
# and equal what the router (the switch off) gives.
sub shipped_equals_single
{
	my ($name, $sql, $table, %opt) = @_;
	like(plan_of($n1, $sql), qr/\Q$PUSHED\E.*Pushed: colocated INSERT \.\.\. SELECT/s, "$name: planned as the shipped statement")
	  or diag('not shipped: ' . (not_shipped_reason($n1, $sql) // '(no reason)'));
	$c->compare_dml($name, $sql);
	$c->compare_tables($table, order_by => $opt{order_by} // 'id', name => "$name: the table");
}

# ---------------------------------------------------------------------------
# the tables
# ---------------------------------------------------------------------------
table(q{CREATE TABLE public.src (id int PRIMARY KEY, v text, n int, g int)},
	q{; SELECT kingres.create_distributed_table('public.src', 'id', bucket_count => 12)});
table(q{CREATE TABLE public.dst (id int PRIMARY KEY, v text, n int DEFAULT 7)},
	q{; SELECT kingres.create_distributed_table('public.dst', 'id', colocate_with => 'public.src')});
table(q{CREATE TABLE public.other (id int PRIMARY KEY, w text)},
	q{; SELECT kingres.create_distributed_table('public.other', 'id', colocate_with => 'public.src')});
table(q{CREATE TABLE public.lookup (k int PRIMARY KEY, label text)},
	q{; SELECT kingres.create_reference_table('public.lookup')});
$c->on_both(q{INSERT INTO public.lookup VALUES (0, 'L0'), (1, 'L1'), (2, 'L2'), (3, 'L3'), (4, 'L4'), (5, 'L5'), (6, 'L6')});
$c->on_both(q{INSERT INTO public.src SELECT g, 'v' || g, g % 50, g % 7 FROM generate_series(1, 400) g});
$c->on_both(q{INSERT INTO public.other SELECT g, 'w' || g FROM generate_series(1, 400, 2) g});

# ---------------------------------------------------------------------------
# A. which statements are shipped
# ---------------------------------------------------------------------------
for my $sql (
	q{INSERT INTO public.dst (id, v) SELECT id, v FROM public.src},
	q{INSERT INTO public.dst (id, v, n) SELECT s.id, o.w, s.n FROM public.src s JOIN public.other o ON o.id = s.id},
	q{INSERT INTO public.dst (id, v) SELECT s.id, l.label FROM public.src s JOIN public.lookup l ON l.k = s.g},
	q{INSERT INTO public.dst SELECT id, v, n FROM public.src WHERE n > 10 ON CONFLICT DO NOTHING},
	q{INSERT INTO public.dst (id, n) SELECT id, count(*) FROM public.src GROUP BY id},
	q{INSERT INTO public.dst (id, v) SELECT DISTINCT id, v FROM public.src},
	q{INSERT INTO public.dst (id, v) SELECT s.id, o.w FROM public.src s LEFT JOIN public.other o ON o.id = s.id},
)
{
	my $p = plan_of($n1, $sql);
	like($p, qr/\Q$PUSHED\E/, "shipped: $sql");
	unlike($p, qr/Row Router/, '... and not routed');
}

my @fallbacks = (
	[ q{INSERT INTO public.dst (id, v) SELECT id + 1, v FROM public.src}, qr/not a distribution key of the source/ ],
	[ q{INSERT INTO public.dst (id, v) SELECT n, v FROM public.src}, qr/not a distribution key of the source/ ],
	[ q{INSERT INTO public.dst (id, v) SELECT s.id, o.w FROM public.src s JOIN public.other o ON o.id = s.n}, qr/more than one key class|not all joined/ ],
	[ q{INSERT INTO public.dst (id, v) SELECT count(*), 'x' FROM public.src}, qr/aggregate without GROUP BY|not the key itself|not a distribution key/ ],
	[ q{INSERT INTO public.dst (id, v) SELECT n, 'x' FROM public.src GROUP BY n}, qr/not a distribution key|grouping does not include/ ],
	[ q{INSERT INTO public.dst (id, v) SELECT id, v FROM public.src LIMIT 5}, qr/LIMIT or OFFSET/ ],
	[ q{INSERT INTO public.dst (id, v) SELECT id, v FROM public.src WHERE id IN (SELECT id FROM public.other)}, qr/sub-select/ ],
	[ q{INSERT INTO public.dst (id, v) SELECT id, v FROM public.src WHERE n IN (SELECT id FROM public.other)}, qr/sub-select/ ],
	[ q{INSERT INTO public.dst (id, v) SELECT id, v FROM (SELECT id, v FROM public.src) q}, qr/something other than a table/ ],
	[ q{INSERT INTO public.dst (id, v) SELECT l.k, l.label FROM public.lookup l}, qr/no distributed table/ ],
	[ q{INSERT INTO public.dst (id, v) SELECT o.id, o.w FROM public.lookup l LEFT JOIN public.other o ON o.id = l.k}, qr/not a distribution key|replicated/ ],
	[ q{INSERT INTO public.dst (id, v) SELECT id, v || random()::text FROM public.src}, qr/not built in and immutable|text form/ ],
	[ q{INSERT INTO public.dst (id, v) SELECT id, now()::text FROM public.src}, qr/not built in and immutable|text form/ ],
	[ q{INSERT INTO public.dst (id, v) SELECT id, current_user FROM public.src}, qr/cannot be shipped/ ],
	[ q{INSERT INTO public.dst (id, v) SELECT s.id, o.w FROM public.src s FULL JOIN public.other o ON o.id = s.id}, qr/not a distribution key/ ],
);
for my $f (@fallbacks)
{
	my $why = not_shipped_reason($n1, $f->[0]);
	ok(defined $why, "not shipped: $f->[0]");
	like($why // '', $f->[1], '... for the reason') if defined $why;
	unlike(plan_of($n1, $f->[0]), qr/\Q$PUSHED\E/, '... the plan is not the shipped statement');
}

# ---------------------------------------------------------------------------
# B. the data
# ---------------------------------------------------------------------------
shipped_equals_single('a plain copy', q{INSERT INTO public.dst (id, v) SELECT id, v FROM public.src WHERE id <= 200}, 'public.dst');
is(placement_ok('public.dst', 'after the copy'), 200, 'every copied row is in some group, once');
$c->on_both(q{TRUNCATE public.dst});
shipped_equals_single('a join of two colocated tables', q{INSERT INTO public.dst (id, v, n) SELECT s.id, o.w, s.n FROM public.src s JOIN public.other o ON o.id = s.id}, 'public.dst');
$c->on_both(q{TRUNCATE public.dst});
shipped_equals_single('a join with a reference table', q{INSERT INTO public.dst (id, v) SELECT s.id, l.label FROM public.src s JOIN public.lookup l ON l.k = s.g}, 'public.dst');
$c->on_both(q{TRUNCATE public.dst});
shipped_equals_single('a left join keeps the preserved side', q{INSERT INTO public.dst (id, v) SELECT s.id, o.w FROM public.src s LEFT JOIN public.other o ON o.id = s.id}, 'public.dst');
$c->on_both(q{TRUNCATE public.dst});
shipped_equals_single('GROUP BY the key', q{INSERT INTO public.dst (id, n) SELECT id, count(*) + sum(n) FROM public.src GROUP BY id}, 'public.dst');
$c->on_both(q{TRUNCATE public.dst});
shipped_equals_single('DISTINCT with the key', q{INSERT INTO public.dst (id, v) SELECT DISTINCT id, v FROM public.src}, 'public.dst');
$c->on_both(q{TRUNCATE public.dst});
shipped_equals_single('columns in another order', q{INSERT INTO public.dst (v, id) SELECT v, id FROM public.src WHERE id % 3 = 0}, 'public.dst');
$c->on_both(q{TRUNCATE public.dst});
shipped_equals_single('a constant default is applied by the group', q{INSERT INTO public.dst (id, v) SELECT id, 'k' FROM public.src}, 'public.dst');
is($n1->safe_psql('postgres', q{SELECT count(*) FROM public.dst WHERE n = 7}), 400, '... for every row');
$c->on_both(q{TRUNCATE public.dst});

# RETURNING: the rows and the count
{
	my $sql = q{INSERT INTO public.dst (id, v) SELECT id, v FROM public.src WHERE id <= 30 RETURNING id, upper(v), n + 1, id * 2};
	my ($rs, $os) = run_sql($single, $sql);
	my ($rd, $od, $ed) = run_sql($n1, $sql);
	is($rd, 0, 'RETURNING of a shipped statement') or diag($ed);
	is(sorted($od), sorted($os), '... returns the rows of the single node');
	my $star = q{INSERT INTO public.dst (id, v) SELECT id + 7000, w FROM public.other WHERE id < 30 RETURNING *};
	my ($rcs, $ocs) = run_sql($single, $star);
	my ($rc, $oc, $ec) = run_sql($n2, $star);
	is($rc, 0, 'RETURNING * from another node') or diag($ec);
	is(sorted($oc), sorted($ocs), '... gives the rows of the single node');
	$c->on_both(q{DELETE FROM public.dst WHERE id >= 7000});
}
$c->compare_tables('public.dst', order_by => 'id', name => 'after RETURNING');
$c->on_both(q{TRUNCATE public.dst});

# every statement, from every node
for my $node ($n1, $n2, $n3)
{
	my $sql = q{INSERT INTO public.dst (id, v) SELECT id, v FROM public.src WHERE id <= 60 RETURNING id};
	my ($rd, $od, $ed) = run_sql($node, $sql);
	is($rd, 0, 'shipped from ' . $node->name) or diag($ed);
	is(join(',', sort { $a <=> $b } split /\n/, $od), join(',', 1 .. 60), '... returns every row once') or diag($od);
	$c->on_cluster(q{TRUNCATE public.dst});
}
placement_ok('public.src', 'the source stays where it is');

# the switch: with it off the statement takes the row router, and gives the same rows
{
	my $sql = q{INSERT INTO public.dst (id, v) SELECT id, v FROM public.src WHERE id <= 100};
	my ($rc, $out, $err) = run_sql($n1, "SET kingres_shard.enable_colocated_insert_select = off; EXPLAIN (COSTS OFF) $sql");
	like($out, qr/Row Router/, 'with enable_colocated_insert_select off the statement is the row router');
	unlike($out, qr/\Q$PUSHED\E/, '... and is not shipped');
	($rc, $out, $err) = run_sql($n1, "SET kingres_shard.enable_colocated_insert_select = off; $sql");
	is($rc, 0, '... and runs') or diag($err);
	$c->on_single($sql);
	$c->compare_tables('public.dst', order_by => 'id', name => 'the router with the switch off');
	$c->on_both(q{TRUNCATE public.dst});
}

# ---------------------------------------------------------------------------
# C. ON CONFLICT
# ---------------------------------------------------------------------------
table(q{CREATE TABLE public.upd (id int PRIMARY KEY, v text, n int DEFAULT 0)},
	q{; SELECT kingres.create_distributed_table('public.upd', 'id', colocate_with => 'public.src')});
table(q{CREATE TABLE public.dups (id int, v text)},
	q{; SELECT kingres.create_distributed_table('public.dups', 'id', colocate_with => 'public.src')});

# DO NOTHING in every form: shipped, and equal to the single node and to the router
$c->on_both(q{INSERT INTO public.upd SELECT g, 'old', 1 FROM generate_series(1, 40) g});
for my $clause ('ON CONFLICT DO NOTHING', 'ON CONFLICT (id) DO NOTHING', 'ON CONFLICT ON CONSTRAINT upd_pkey DO NOTHING',
				'ON CONFLICT (id) WHERE id > 0 DO NOTHING')
{
	my $sql = "INSERT INTO public.upd (id, v, n) SELECT id, v, n FROM public.src WHERE id <= 80 $clause";
	like(plan_of($n1, $sql), qr/\Q$PUSHED\E/, "shipped: $clause");
	$c->compare_dml("$clause: the count skips the conflicts", $sql);
	$c->compare_tables('public.upd', order_by => 'id', name => "after $clause");
	# the router gives the same
	$c->on_both(q{DELETE FROM public.upd WHERE id > 40});
	$c->on_both(q{UPDATE public.upd SET v = 'old', n = 1});
	my ($rc, $out, $err) = run_sql($n1, "SET kingres_shard.enable_colocated_insert_select = off; $sql");
	is($rc, 0, "$clause through the router") or diag($err);
	$c->on_single($sql);
	$c->compare_tables('public.upd', order_by => 'id', name => "$clause: the router and the shipped statement agree");
	$c->on_both(q{DELETE FROM public.upd WHERE id > 40});
	$c->on_both(q{UPDATE public.upd SET v = 'old', n = 1});
}

# DO UPDATE (11.7): a counter that every row conflicts with, repeated
{
	my $sql = q{INSERT INTO public.upd (id, v, n) SELECT id, 'new', 1 FROM public.src WHERE id <= 60
		ON CONFLICT (id) DO UPDATE SET n = upd.n + excluded.n, v = excluded.v};
	is_shipped($sql, 'DO UPDATE with a counter is shipped');
	$c->compare_dml('DO UPDATE counter, first run (40 conflicts, 20 inserts)', $sql);
	$c->compare_dml('DO UPDATE counter, second run (every row conflicts)', $sql);
	$c->compare_dml('DO UPDATE counter, third run', $sql);
	$c->compare_tables('public.upd', order_by => 'id', name => 'after the counters');
	my $diff = $n1->safe_psql('postgres', q{SELECT string_agg(DISTINCT n::text, ',' ORDER BY n::text) FROM public.upd WHERE id <= 40});
	is($diff, '4', 'the counter reached 4 for every row that existed');
}
{
	# DO UPDATE ... WHERE: the count is the rows that were updated
	my $sql = q{INSERT INTO public.upd (id, v, n) SELECT id, 'w', 100 FROM public.src WHERE id <= 60
		ON CONFLICT (id) DO UPDATE SET v = excluded.v, n = excluded.n WHERE upd.id % 3 = 0};
	like(plan_of($n1, $sql), qr/\Q$PUSHED\E/, 'DO UPDATE ... WHERE is shipped');
	$c->compare_dml('DO UPDATE ... WHERE: the count is the rows updated', $sql);
	$c->compare_tables('public.upd', order_by => 'id', name => 'after DO UPDATE ... WHERE');
	# by the name of the constraint, and with an inference clause and its predicate on a partial unique index
	$c->on_both(q{CREATE UNIQUE INDEX upd_pos ON public.upd (id) WHERE n > 0});
	for my $target ('ON CONFLICT ON CONSTRAINT upd_pkey', 'ON CONFLICT (id) WHERE n > 0', 'ON CONFLICT (id)')
	{
		my $s = qq{INSERT INTO public.upd (id, v, n) SELECT id, 'x', 5 FROM public.src WHERE id BETWEEN 30 AND 50
			$target DO UPDATE SET n = upd.n + excluded.n};
		my $shipped = plan_of($n1, $s) =~ /\Q$PUSHED\E/ ? 1 : 0;
		is($shipped, 1, "shipped: $target");
		# a partial index is a unique index that does not name the predicate: single node picks it only if it matches
		my ($rs, $os, $es) = run_sql($single, $s);
		my ($rd, $od, $ed) = run_sql($n1, $s);
		is($rd, $rs, "$target: the same outcome as the single node") or diag("cluster: $ed\nsingle: $es");
		$c->compare_tables('public.upd', order_by => 'id', name => "after $target");
	}
	$c->on_both(q{DROP INDEX public.upd_pos});
}
{
	# RETURNING with DO UPDATE: the rows of both kinds
	my $sql = q{INSERT INTO public.upd (id, v, n) SELECT id, 'r', 1 FROM public.src WHERE id BETWEEN 35 AND 50
		ON CONFLICT (id) DO UPDATE SET n = upd.n + excluded.n RETURNING id, v, n, id * 2};
	like(plan_of($n1, $sql), qr/\Q$PUSHED\E/, 'DO UPDATE ... RETURNING is shipped');
	my ($rs, $os) = run_sql($single, $sql);
	my ($rd, $od, $ed) = run_sql($n1, $sql);
	is($rd, 0, 'DO UPDATE ... RETURNING') or diag($ed);
	is(sorted($od), sorted($os), '... returns what the single node returns');
	$c->compare_tables('public.upd', order_by => 'id', name => 'after DO UPDATE ... RETURNING');
}

# duplicates in the source: the error of the single node, and nothing changed
$c->on_both(q{INSERT INTO public.dups SELECT g % 25, 'd' FROM generate_series(1, 50) g});
{
	my $sql = q{INSERT INTO public.upd (id, v, n) SELECT id, v, 1 FROM public.dups ON CONFLICT (id) DO UPDATE SET n = excluded.n};
	like(plan_of($n1, $sql), qr/\Q$PUSHED\E/, 'a source with duplicate keys is shipped');
	my ($rs, $os, $es) = run_sql($single, $sql);
	my ($rd, $od, $ed) = run_sql($n1, $sql);
	isnt($rs, 0, 'the single node refuses a row that it would affect twice');
	isnt($rd, 0, '... and so does the cluster');
	like($ed, qr/ON CONFLICT DO UPDATE command cannot affect row a second time/, '... with the message of PostgreSQL');
	like($es, qr/ON CONFLICT DO UPDATE command cannot affect row a second time/, '... the same');
	$c->compare_tables('public.upd', order_by => 'id', name => 'a refused DO UPDATE changes nothing');
	is(prepared_count($_), 0, '... and leaves nothing prepared on ' . $_->name) for ($n1, $n2, $n3);
	# DO NOTHING with the same duplicates: the later row of a key is skipped
	$sql = q{INSERT INTO public.upd (id, v, n) SELECT id + 1000, v, 1 FROM public.dups ON CONFLICT (id) DO NOTHING};
	my ($r1) = run_sql($single, $sql);
	my ($r2) = run_sql($n1, $sql);
	is($r2, $r1, 'DO NOTHING with duplicate keys in the source: same outcome');
	$c->compare_tables('public.upd', order_by => 'id', name => 'after DO NOTHING with duplicates', query => q{SELECT id, n FROM public.upd ORDER BY id});
}

# what is not shipped with DO UPDATE, each with its reason
{
	my @nope = (
		[ q{INSERT INTO public.upd (id, v) SELECT id, v FROM public.src ON CONFLICT (id) DO UPDATE SET id = excluded.id + 0}, qr/assigns the distribution column/ ],
		[ q{INSERT INTO public.upd (id, v) SELECT id, v FROM public.src ON CONFLICT (id) DO UPDATE SET v = excluded.v || random()::text}, qr/not built in and immutable|text form/ ],
		[ q{INSERT INTO public.upd (id, v) SELECT id, v FROM public.src ON CONFLICT (id) DO UPDATE SET v = (SELECT w FROM public.other WHERE other.id = excluded.id)}, qr/sub-select/ ],
		[ q{INSERT INTO public.upd (id, v) SELECT id, v FROM public.src ON CONFLICT (id) DO UPDATE SET v = lower(excluded.v) WHERE upd.v IS DISTINCT FROM excluded.v}, undef ],
	);
	for my $f (@nope)
	{
		my $why = not_shipped_reason($n1, $f->[0]);
		if (defined $f->[1])
		{
			ok(defined $why, "not shipped: $f->[0]");
			like($why // '', $f->[1], '... for the reason');
			my ($rc, $out, $err) = run_sql($n1, $f->[0]);
			isnt($rc, 0, '... and the router refuses DO UPDATE (nothing is inserted)');
			like($err, qr/ON CONFLICT DO UPDATE is not supported yet for an INSERT that goes to several shard groups/, '... with its message');
		}
		else
		{
			ok(!defined $why, "shipped: $f->[0]");
		}
	}
	$c->compare_tables('public.upd', order_by => 'id', name => 'the refused statements changed nothing', query => q{SELECT id, n FROM public.upd ORDER BY id});
}

# PostgreSQL itself refuses a unique index of a partitioned table that lacks the partition key (or has it under
# another collation), so the check of 11.7 (condition 1) can only meet a catalog that was changed by hand: here
# an ordinary index on a column that is not the key is made unique in pg_index on every node.  The statement
# is then not shipped, and DO UPDATE is refused by the router.
{
	my ($rc, $out, $err) = run_sql($n1, q{CREATE INDEX upd_n_only ON public.upd (n)});
	is($rc, 0, 'an index on a column that is not the key') or diag($err);
	is_shipped(q{INSERT INTO public.upd (id, v) SELECT id, v FROM public.src WHERE id > 380 ON CONFLICT (id) DO UPDATE SET v = excluded.v},
		'a non-unique index of the target does not matter');
	$_->safe_psql('postgres', q{UPDATE pg_index SET indisunique = true WHERE indexrelid = 'public.upd_n_only'::regclass}) for ($n1, $n2, $n3);
	like(not_shipped_reason($n1, q{INSERT INTO public.upd (id, v) SELECT id, v FROM public.src WHERE id > 380 ON CONFLICT DO NOTHING}) // '',
		qr/unique index of the target does not include the distribution column/, 'a unique index without the key: not shipped');
	($rc, $out, $err) = run_sql($n1, q{INSERT INTO public.upd (id, v) SELECT id, v FROM public.src WHERE id > 380 ON CONFLICT (id) DO UPDATE SET v = excluded.v});
	isnt($rc, 0, '... and DO UPDATE is then refused by the router');
	like($err, qr/ON CONFLICT DO UPDATE is not supported yet/, '... with its message');
	$_->safe_psql('postgres', q{UPDATE pg_index SET indisunique = false WHERE indexrelid = 'public.upd_n_only'::regclass}) for ($n1, $n2, $n3);
	($rc, $out, $err) = run_sql($n1, q{DROP INDEX public.upd_n_only});
	is($rc, 0, 'the index is dropped') or diag($err);
}

# ---------------------------------------------------------------------------
# D. errors: what the single node says, and nothing left behind
# ---------------------------------------------------------------------------
sub err_lines
{
	my ($e) = @_;
	# The error and its detail.  The name of a bucket partition (dst_b0003_pkey for dst_pkey) and the
	# values of the row that failed first are not the single node's: which group fails first, and on
	# which row, is not specified by anyone.
	return join("\n", map { s/^psql:<stdin>:\d+: //r =~ s/_b\d{4}//gr =~ s/\d+/N/gr }
		grep { /^(?:psql:<stdin>:\d+: )?(?:ERROR|DETAIL):/ } split /\n/, $e);
}

sub same_error
{
	my ($name, $sql, $table, $want_re) = @_;
	my $before_s = $single->safe_psql('postgres', "SELECT count(*) FROM $table");
	my ($rs, $os, $es) = run_sql($single, $sql);
	my ($rd, $od, $ed) = run_sql($n1, $sql);
	isnt($rs, 0, "$name: the single node fails");
	isnt($rd, 0, "$name: the cluster fails");
	like($ed, $want_re, "$name: the error is PostgreSQL's");
	is(err_lines($ed), err_lines($es), "$name: the error and its detail are the single node's");
	is($n1->safe_psql('postgres', "SELECT count(*) FROM $table"), $before_s, "$name: no row of the statement is left");
	is(prepared_count($_), 0, "$name: nothing is prepared on " . $_->name) for ($n1, $n2, $n3);
}

$c->on_both(q{TRUNCATE public.dst});
$c->on_both(q{INSERT INTO public.dst (id, v) SELECT id, v FROM public.src WHERE id <= 10});
same_error('a unique violation', q{INSERT INTO public.dst (id, v) SELECT id, v FROM public.src WHERE id <= 50}, 'public.dst',
	qr/duplicate key value violates unique constraint "dst(_b\d+)?_pkey"/);
same_error('a division by zero', q{INSERT INTO public.dst (id, n) SELECT id, 10 / (id - 77) FROM public.src WHERE id > 60}, 'public.dst',
	qr/division by zero/);
{
	$c->on_both(q{CREATE TABLE public.chk (id int PRIMARY KEY, k int NOT NULL, q int CHECK (q >= 0))});
	$c->on_cluster(q{SELECT kingres.create_distributed_table('public.chk', 'id', colocate_with => 'public.src')});
	$c->on_both(q{ALTER TABLE public.chk ADD FOREIGN KEY (k) REFERENCES public.lookup (k)});
	$c->compare_dml('rows that satisfy the constraints', q{INSERT INTO public.chk SELECT id, g, n FROM public.src WHERE id <= 30});
	same_error('a foreign key to a reference table', q{INSERT INTO public.chk SELECT id + 100, g + 50, n FROM public.src WHERE id <= 30}, 'public.chk',
		qr/violates foreign key constraint/);
	same_error('a check constraint', q{INSERT INTO public.chk SELECT id + 100, g, n - 10 FROM public.src WHERE id <= 30}, 'public.chk',
		qr/violates check constraint/);
	same_error('a not null violation', q{INSERT INTO public.chk (id, q) SELECT id + 100, n FROM public.src WHERE id <= 30}, 'public.chk',
		qr/null value in column "k"/);
}

# a savepoint, a rollback, and the transaction's own writes
{
	my $script = q{BEGIN;
		INSERT INTO public.other SELECT id + 10000, w FROM public.other WHERE id < 100;
		INSERT INTO public.dst (id, v) SELECT id, w FROM public.other WHERE id >= 10000;
		SAVEPOINT s1;
		INSERT INTO public.dst (id, v) SELECT id + 20000, v FROM public.src WHERE id < 50;
		ROLLBACK TO s1;
		INSERT INTO public.dst (id, v) SELECT id + 30000, v FROM public.src WHERE id < 20;
		COMMIT;};
	my ($rs, $os, $es) = run_sql($single, $script);
	my ($rd, $od, $ed) = run_sql($n1, $script);
	is($rd, 0, 'a shipped statement in a transaction with a savepoint') or diag($ed);
	$c->compare_tables('public.dst', order_by => 'id', name => 'the rows that read the transaction\'s own writes, a rolled back savepoint');
	$c->compare_tables('public.other', order_by => 'id', name => 'the table the transaction wrote');
	is(prepared_count($_), 0, '... and nothing is prepared on ' . $_->name) for ($n1, $n2, $n3);
	($rd, $od, $ed) = run_sql($n1, q{BEGIN; INSERT INTO public.dst (id, v) SELECT id + 40000, v FROM public.src WHERE id < 30; ROLLBACK;});
	is($rd, 0, 'a rolled back shipped statement');
	is($n1->safe_psql('postgres', q{SELECT count(*) FROM public.dst WHERE id >= 40000}), 0, '... leaves no row in any group');
	($rd, $od, $ed) = run_sql($n1, q{BEGIN READ ONLY; INSERT INTO public.dst (id, v) SELECT id + 50000, v FROM public.src; COMMIT});
	isnt($rd, 0, 'a read-only transaction cannot run it');
	like($ed, qr/read-only transaction/, '... and says so');
}

# ---------------------------------------------------------------------------
# E. prepared statements, generic plans, a table that changes, the switch
# ---------------------------------------------------------------------------
{
	$c->on_both(q{TRUNCATE public.dst});
	my $sql = q{PREPARE pi (int) AS INSERT INTO public.dst (id, v) SELECT id, v FROM public.src WHERE id <= $1 ON CONFLICT DO NOTHING;
		EXECUTE pi (10); EXECUTE pi (20); EXECUTE pi (30); EXECUTE pi (40); EXECUTE pi (50);
		EXECUTE pi (60); EXECUTE pi (70);
		SET plan_cache_mode = force_generic_plan;
		EXECUTE pi (80); EXECUTE pi (90);
		SET kingres_shard.enable_colocated_insert_select = off;
		EXECUTE pi (100);
		RESET kingres_shard.enable_colocated_insert_select;
		RESET plan_cache_mode;
		EXECUTE pi (110);};
	my ($rs, $os, $es) = run_sql($single, $sql);
	my ($rd, $od, $ed) = run_sql($n1, $sql);
	is($rd, 0, 'a prepared statement, five custom plans, a generic plan and the switch') or diag($ed);
	$c->compare_tables('public.dst', order_by => 'id', name => 'after the prepared statement');
	# a plan made before the source changes kind: the table is dropped and made again as a local table
	$c->on_both(q{CREATE TABLE public.mover (id int PRIMARY KEY, v text)});
	$c->on_cluster(q{SELECT kingres.create_distributed_table('public.mover', 'id', colocate_with => 'public.src')});
	$c->on_both(q{INSERT INTO public.mover SELECT id, v FROM public.src WHERE id <= 20});
	my $prep = q{PREPARE pm AS INSERT INTO public.dst (id, v) SELECT id, v FROM public.mover ON CONFLICT DO NOTHING};
	is_shipped(q{INSERT INTO public.dst (id, v) SELECT id, v FROM public.mover ON CONFLICT DO NOTHING}, 'the statement of the prepared plan is shipped');
	for my $node ($n1, $single)
	{
		my ($r, $o, $e) = run_sql($node, "$prep; SET plan_cache_mode = force_generic_plan; EXECUTE pm; EXECUTE pm; DELETE FROM public.dst WHERE id <= 5;
			DROP TABLE public.mover; CREATE TABLE public.mover (id int, v text); INSERT INTO public.mover SELECT id, v FROM public.src WHERE id <= 5;
			EXECUTE pm;");
		is($r, 0, 'a prepared statement after its source was dropped and made again as a local table (' . ($node == $single ? 'single' : 'cluster') . ')') or diag($e);
	}
	$c->compare_tables('public.dst', order_by => 'id', name => 'after the source became a local table',);
	$c->on_both(q{DROP TABLE public.mover});
}

# ---------------------------------------------------------------------------
# E2. generated and identity columns, a domain, a foreign key to a colocated table
# ---------------------------------------------------------------------------
{
	table(q{CREATE TABLE public.gen (id int PRIMARY KEY, a int, gs int GENERATED ALWAYS AS (a + 1) STORED,
			gv int GENERATED ALWAYS AS (a * 2) VIRTUAL, ia bigint GENERATED ALWAYS AS IDENTITY, ib bigint GENERATED BY DEFAULT AS IDENTITY)},
		q{; SELECT kingres.create_distributed_table('public.gen', 'id', colocate_with => 'public.src')});
	# the identity columns are defaults computed once per row at the origin: the row router takes the statement
	like(not_shipped_reason($n1, q{INSERT INTO public.gen (id, a) SELECT id, n FROM public.src}) // '', qr/volatile default/,
		'an identity column that the statement does not name: not shipped');
	my ($rc, $out, $err) = run_sql($n1, q{INSERT INTO public.gen (id, a) SELECT id, n FROM public.src});
	is($rc, 0, '... the router runs it') or diag($err);
	$c->on_single(q{INSERT INTO public.gen (id, a) SELECT id, n FROM public.src});
	$c->compare_tables('public.gen', order_by => 'id', name => 'generated columns after the router', query => q{SELECT id, a, gs, gv FROM public.gen ORDER BY id});
	$c->on_both(q{TRUNCATE public.gen});
	# naming them with OVERRIDING SYSTEM VALUE ships the statement; the stored and the virtual column are the group's own
	my $sql = q{INSERT INTO public.gen (id, a, ia, ib) OVERRIDING SYSTEM VALUE SELECT id, n, id * 10, id * 100 FROM public.src};
	is_shipped($sql, 'OVERRIDING SYSTEM VALUE with the identity columns named is shipped');
	$c->compare_dml('OVERRIDING SYSTEM VALUE', $sql);
	$c->compare_tables('public.gen', order_by => 'id', name => 'generated and identity columns after the shipped statement');
	($rc, $out, $err) = run_sql($n1, q{INSERT INTO public.gen (id, a, ia) SELECT id + 100000, n, 1 FROM public.src});
	isnt($rc, 0, 'a value for a GENERATED ALWAYS identity column without OVERRIDING is refused');
	like($err, qr/cannot insert a non-DEFAULT value into column "ia"/, '... as on a single node');
	($rc, $out, $err) = run_sql($n1, q{INSERT INTO public.gen (id, a, ia) SELECT id, n, id FROM public.src});
	isnt($rc, 0, '... also when the statement would be shipped');
	like($err, qr/cannot insert a non-DEFAULT value into column "ia"/, '... with the same message');
	$c->compare_tables('public.gen', order_by => 'id', name => 'the refused statements changed nothing');

	# a domain column of the target: the cast is not shippable, the router takes it, the check is the group's
	$c->on_every_node(q{CREATE DOMAIN public.nonneg AS int CHECK (VALUE >= 0)});
	$c->on_single(q{CREATE DOMAIN public.nonneg AS int CHECK (VALUE >= 0)});
	table(q{CREATE TABLE public.dom (id int PRIMARY KEY, d public.nonneg)},
		q{; SELECT kingres.create_distributed_table('public.dom', 'id', colocate_with => 'public.src')});
	ok(defined not_shipped_reason($n1, q{INSERT INTO public.dom (id, d) SELECT id, n FROM public.src}), 'a domain column of the target: not shipped');
	$c->compare_dml('a domain column through the router', q{INSERT INTO public.dom (id, d) SELECT id, n FROM public.src});
	$c->compare_tables('public.dom', order_by => 'id', name => 'the domain column');
	same_error('a domain check', q{INSERT INTO public.dom (id, d) SELECT id + 1000, n - 10 FROM public.src}, 'public.dom',
		qr/violates check constraint|value for domain/);

	# a foreign key between colocated tables (on the key): the check is the group's, the row it needs is on the group
	table(q{CREATE TABLE public.child (id int PRIMARY KEY, w text)},
		q{; SELECT kingres.create_distributed_table('public.child', 'id', colocate_with => 'public.src')});
	table(q{CREATE TABLE public.orph (id int PRIMARY KEY, w text)},
		q{; SELECT kingres.create_distributed_table('public.orph', 'id', colocate_with => 'public.src')});
	$c->on_both(q{ALTER TABLE public.child ADD FOREIGN KEY (id) REFERENCES public.src (id)});
	$c->on_both(q{INSERT INTO public.orph SELECT g, 'o' FROM generate_series(1001, 1040) g});
	is_shipped(q{INSERT INTO public.child (id, w) SELECT id, w FROM public.other WHERE id < 1000}, 'a child of a colocated table: shipped');
	$c->compare_dml('a foreign key to a colocated table: the parent row is on the same group', q{INSERT INTO public.child (id, w) SELECT id, w FROM public.other WHERE id < 1000});
	$c->compare_tables('public.child', order_by => 'id', name => 'the child table');
	same_error('a foreign key violation on a group', q{INSERT INTO public.child (id, w) SELECT id, w FROM public.orph}, 'public.child',
		qr/violates foreign key constraint/);
}

# ---------------------------------------------------------------------------
# F. every group holds exactly the rows of its buckets
# ---------------------------------------------------------------------------
placement_ok('public.dst', 'dst at the end');
placement_ok('public.upd', 'upd at the end');
placement_ok('public.chk', 'chk at the end');
is(prepared_count($_), 0, 'no prepared transaction is left on ' . $_->name) for ($n1, $n2, $n3);

done_testing();
