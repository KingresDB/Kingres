# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/126_pushdown_forged_security.pl
#
# Step 6 of phase 2b (design/shard-phase2b.md, 4.2, 5, 15, 16.2 and 16.4): the closed query that every group runs
# whole (kind "query", marker /* kingres:query-fragment place=<n> */) against forged fragments, privileges and
# hostile values.  It is the step 6 part of the design's 067 (forged fragments, privileges, S2-08) and the
# round trip of the fragment text; the numbers 055 to 069 of the design are taken by tests of 0.5.
#
#   A. the round trip: the text that the origin ships is accepted by a group, and the text of a statement that
#      the origin did not ship is refused by the group when it is forged into a fragment (the group decides on the
#      text that was deparsed and parsed again, with the function the origin used on the same text);
#   B. a forged fragment: a stale placement, a marker that is not one, a statement of another kind, a LIMIT,
#      a client that writes the marker, a session that claims to be forwarded;
#   C. privileges: the caller's on every node, column privileges, a revoke on one group, a view of another owner,
#      row-level security, a function that is PARALLEL SAFE;
#   D. values and names cross intact: hostile identifiers and constants, under every session profile.

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use IPC::Run ();
use KingresShardTest qw(run_sql prepared_count);
use KingresShardCompare;

my $c = KingresShardCompare->new('pg', extra => "kingres_shard.enable_partial_aggregation = off" . "
");
my ($n1, $n2, $n3) = $c->nodes;
my $single = $c->single;
my @nodes = ($n1, $n2, $n3);
my $FS = 'Kingres Fragment Scan';

sub table
{
	my ($create, $dist) = @_;
	$c->on_cluster($create . ($dist // ''));
	$c->on_single($create);
}

sub plan_of
{
	my ($node, $sql, $pre) = @_;
	my ($rc, $out, $err) = run_sql($node, ($pre // '') . "EXPLAIN (COSTS OFF) $sql");
	return $rc == 0 ? $out : "ERROR: $err";
}

# The text that is shipped to the groups, from EXPLAIN (without the marker, which is added when it is sent).
sub shipped_text
{
	my ($sql) = @_;
	my $p = plan_of($n1, $sql);
	return $p =~ /^\s*Remote SQL: (.*)$/m ? $1 : undef;
}

# A statement as a group that another node opened a session to receives it.
sub as_node
{
	my ($node, $id, $sql) = @_;
	my $tok = $node->safe_psql('postgres', "SELECT kingres.internal_forward_token(current_user, $id)");
	local $ENV{PGOPTIONS} = "-c kingres_shard.forwarded=on -c kingres_shard.forward_token=$tok";
	return run_sql($node, "\\set VERBOSITY verbose\nBEGIN;\n$sql;\nROLLBACK;");
}

sub as_role
{
	my ($node, $role, $sql) = @_;
	my ($out, $err) = ('', '');
	my $h = IPC::Run::start(
		[ 'psql', '-XAt', '-v', 'ON_ERROR_STOP=1', '-h', $node->host, '-p', $node->port, '-d', 'postgres', '-U', $role, '-c', $sql ],
		'<', \my $in, '>', \$out, '2>', \$err);
	$h->finish;
	return ($h->result(0), $out, $err);
}

sub fragment
{
	my ($place, $sql) = @_;
	return "/* kingres:query-fragment place=$place */ DECLARE ks_forged NO SCROLL CURSOR FOR $sql";
}

table(q{CREATE TABLE public.s (id int, g int, v text, n int)},
	q{; SELECT kingres.create_distributed_table('public.s', 'id', bucket_count => 12)});
table(q{CREATE TABLE public.o (id int PRIMARY KEY, w text)},
	q{; SELECT kingres.create_distributed_table('public.o', 'id', colocate_with => 'public.s')});
table(q{CREATE TABLE public.away (id int PRIMARY KEY, w text)},
	q{; SELECT kingres.create_distributed_table('public.away', 'id', bucket_count => 7)});
table(q{CREATE TABLE public.lookup (k int PRIMARY KEY, label text)},
	q{; SELECT kingres.create_reference_table('public.lookup')});
$c->on_every_node(q{CREATE TABLE public.loc (x int, y int)});
$c->on_single(q{CREATE TABLE public.loc (x int, y int)});
$c->on_both(q{INSERT INTO public.lookup VALUES (0, 'L0'), (1, 'L1'), (2, 'L2'), (3, 'L3'), (4, 'L4'), (5, 'L5'), (6, 'L6')});
$c->on_both(q{INSERT INTO public.s SELECT g, g % 7, 'v' || g, g % 50 FROM generate_series(1, 3000) g, generate_series(1, 2) dup});
$c->on_both(q{INSERT INTO public.o SELECT g, 'w' || g FROM generate_series(1, 3000, 2) g});
$c->on_both(q{INSERT INTO public.away SELECT g, 'a' || g FROM generate_series(1, 50) g});
$c->on_every_node(q{INSERT INTO public.loc SELECT g, g % 5 FROM generate_series(1, 40) g});
$c->on_single(q{INSERT INTO public.loc SELECT g, g % 5 FROM generate_series(1, 40) g});
$c->on_every_node(q{ANALYZE});
$c->on_single(q{ANALYZE});
my $PLACE = $n1->safe_psql('postgres', q{SELECT kingres.internal_placement_hash(1)});

# ---------------------------------------------------------------------------
# A. the round trip
# ---------------------------------------------------------------------------
my @shipped = (
	q{SELECT id, count(*), sum(n) FROM public.s GROUP BY id},
	q{SELECT s.id, o.w FROM public.s JOIN public.o ON o.id = s.id WHERE s.n > 3},
	q{SELECT DISTINCT id, g FROM public.s},
	q{SELECT s.id, l.label, count(*) FROM public.s JOIN public.lookup l ON l.k = s.g GROUP BY s.id, l.label},
	q{SELECT id, max(v), min(n) FROM public.s WHERE id % 3 = 0 AND v <> 'x*/;--' GROUP BY id HAVING count(*) >= 1},
	q{SELECT s.id, count(o.w) FROM public.s LEFT JOIN public.o ON o.id = s.id AND o.id > 10 GROUP BY s.id},
	q{SELECT id, count(DISTINCT n) FROM public.s WHERE id IN (1, 2, 3, 50, 500) GROUP BY id},
	q{SELECT s.id, t.v FROM public.s JOIN public.s t ON t.id = s.id WHERE s.n < t.g},
	q{SELECT id, 'a''b\c', 5, count(*) FROM public.s GROUP BY id ORDER BY id DESC},
	q{SELECT id, sum(n) FROM public.s GROUP BY id ORDER BY sum(n), id},
);
my @refused = (
	q{SELECT g, count(*) FROM public.s GROUP BY g},
	q{SELECT count(*) FROM public.s},
	q{SELECT DISTINCT g FROM public.s},
	q{SELECT id, row_number() OVER (PARTITION BY id ORDER BY n) FROM public.s},
	q{SELECT id, sum(n::float8) FROM public.s GROUP BY id},
	q{SELECT id, string_agg(v, ',') FROM public.s GROUP BY id},
	q{SELECT id, count(*) FROM public.s WHERE n < extract(epoch FROM now()) GROUP BY id},
	q{SELECT id, max(n::text) FROM public.s GROUP BY id},
	q{SELECT id, count(*) FROM public.s WHERE id IN (SELECT id FROM public.o) GROUP BY id},
	q{SELECT s.id, o.w FROM public.s JOIN public.o ON o.id = s.n},
	q{SELECT s.id, a.w FROM public.s JOIN public.away a ON a.id = s.id},
	q{SELECT s.id, count(*) FROM public.s JOIN public.loc ON loc.x = s.g GROUP BY s.id},
	q{SELECT o.id, count(*) FROM public.s LEFT JOIN public.o ON o.id = s.id GROUP BY o.id},
	q{SELECT id % 10, count(*) FROM public.s GROUP BY id % 10},
	q{SELECT s FROM public.s GROUP BY id, s},
	q{SELECT id, count(*) FROM public.s GROUP BY GROUPING SETS ((id), ())},
);
for my $sql (@shipped)
{
	my $text = shipped_text($sql);
	ok(defined $text, "shipped: $sql") or next;
	unlike($text, qr/\b(?:ORDER BY|LIMIT)\b.*$/, '... the text has no ORDER BY and no LIMIT') if $sql !~ /'a''b/;
	for my $nd ([ $n2, 2 ], [ $n3, 3 ])
	{
		my ($rc, $out, $err) = as_node($nd->[0], $nd->[1], fragment($PLACE, $text));
		is($rc, 0, '... node ' . $nd->[1] . ' accepts the text') or diag($err);
	}
}
for my $sql (@refused)
{
	my $plan = plan_of($n1, $sql);
	unlike($plan, qr/\Q$FS\E/, "not shipped: $sql");
	(my $text = $sql) =~ s/\s+(?:FOR UPDATE)$//;
	my ($rc, $out, $err) = as_node($n2, 2, fragment($PLACE, $sql));
	isnt($rc, 0, '... a group refuses it when it is forged into a fragment');
	like($err, qr/0A000/, '... with 0A000 (a repeat gives the same answer)');
	like($err, qr/does not accept the fragment that another node sent it/, '... and says so');
}

# ---------------------------------------------------------------------------
# B. a forged fragment
# ---------------------------------------------------------------------------
{
	my $base = q{SELECT id, count(*), sum(n) FROM public.s GROUP BY id};
	my $text = shipped_text($base);
	my ($rc, $out, $err) = as_node($n2, 2, fragment($PLACE, $text));
	is($rc, 0, 'the real fragment is accepted') or diag($err);

	($rc, $out, $err) = as_node($n2, 2, fragment(12345, $text));
	isnt($rc, 0, 'a fragment with a placement that is not the group\'s is refused');
	like($err, qr/KS001/, '... with the retryable error');
	like($err, qr/not the one the statement was shipped with/, '... which says what is wrong');

	($rc, $out, $err) = as_node($n2, 2, fragment($PLACE, $base . ' LIMIT 5'));
	isnt($rc, 0, 'a fragment with a LIMIT is refused (the node that gathers the rows applies it)');
	like($err, qr/0A000/, '... with 0A000');

	for my $bad ('/* kingres:query-fragment */ ', '/* kingres:query-fragment place=abc */ ', '/* kingres:query-fragment place=1 foo=2 */ ',
				 '/* kingres:query-fragment v=2 kind=query place=1 */ ')
	{
		($rc, $out, $err) = as_node($n2, 2, $bad . "DECLARE ks_forged NO SCROLL CURSOR FOR $text");
		isnt($rc, 0, "a marker that is not one: $bad");
		like($err, qr/0A000|does not accept|syntax/, '... is refused');
	}

	for my $stmt (q{DELETE FROM public.s}, q{UPDATE public.s SET n = 1}, q{INSERT INTO public.s VALUES (1, 1, 'a', 1)},
				  q{INSERT INTO public.o SELECT id, v FROM public.s})
	{
		($rc, $out, $err) = as_node($n2, 2, "/* kingres:query-fragment place=$PLACE */ $stmt");
		isnt($rc, 0, "a $stmt with the marker is refused");
		like($err, qr/0A000|permission denied|does not accept/, '... with a refusal');
	}
	is($n1->safe_psql('postgres', q{SELECT count(*) FROM public.s}), 6000, 'no row was changed by the forged statements');

	($rc, $out, $err) = run_sql($n1, "/* kingres:query-fragment place=$PLACE */ SELECT id, count(*) FROM public.s WHERE id <= 40 GROUP BY id");
	is($rc, 0, 'a client that writes the marker gets an ordinary statement') or diag($err);
	is(scalar(split /\n/, $out), 40, '... the statement of the client, run for all its rows');

	{
		local $ENV{PGOPTIONS} = '-c kingres_shard.forwarded=on';
		($rc, $out, $err) = run_sql($n2, "/* kingres:query-fragment place=$PLACE */ SELECT id FROM public.s");
		isnt($rc, 0, 'a session that claims to be forwarded without the token is refused');
	}
	# a node that is not the primary of its group (a stale origin asks it): not tested here, see t/125 (E)
	my ($rcl, $ol, $el) = as_node($n2, 2, "/* kingres:query-fragment place=$PLACE */ DECLARE ks_forged NO SCROLL CURSOR FOR SELECT id, count(*) FROM public.s GROUP BY id; FETCH 3 FROM ks_forged");
	is($rcl, 0, 'a real fragment gives the rows of the buckets of the group') or diag($el);
	my $mine = $n2->safe_psql('postgres', q{SET kingres_shard.enable_routing = off; SELECT count(*) FROM public.s});
	my ($full) = run_sql($n2, "BEGIN; SET kingres_shard.enable_pushdown = off; SELECT 1; ROLLBACK;");
	cmp_ok($mine, '<', 6000, '(the group holds a part of the table)');
}

# ---------------------------------------------------------------------------
# C. privileges
# ---------------------------------------------------------------------------
{
	$_->safe_psql('postgres', q{CREATE ROLE sec_c LOGIN; CREATE ROLE sec_r LOGIN; CREATE ROLE sec_n LOGIN; CREATE ROLE sec_o LOGIN;
		GRANT USAGE ON SCHEMA public TO sec_c, sec_r, sec_n, sec_o}) for @nodes;
	$n1->safe_psql('postgres', q{
		GRANT SELECT ON public.s, public.o, public.lookup TO sec_r, sec_o;
		GRANT SELECT (id, g) ON public.s TO sec_c});
	my $q = q{SELECT id, count(*), sum(n) FROM public.s GROUP BY id};
	my ($rc, $out, $err) = as_role($n2, 'sec_r', $q);
	is($rc, 0, 'a role with SELECT runs it (from another node)') or diag($err);
	is(scalar(split /\n/, $out), 3000, '... and gets every row');
	like(plan_of($n1, $q), qr/\Q$FS\E/, 'the statement of the privilege matrix is shipped');
	($rc, $out, $err) = as_role($n1, 'sec_n', $q);
	isnt($rc, 0, 'a role with no privilege is refused');
	like($err, qr/permission denied for table s/, '... with the error of PostgreSQL');
	($rc, $out, $err) = as_role($n1, 'sec_c', q{SELECT id, count(*) FROM public.s GROUP BY id});
	is($rc, 0, 'a role with SELECT on the columns the query reads runs it') or diag($err);
	($rc, $out, $err) = as_role($n1, 'sec_c', $q);
	isnt($rc, 0, 'a column the role may not read is refused');
	like($err, qr/permission denied for table s/, '... with the error of PostgreSQL');
	($rc, $out, $err) = as_role($n1, 'sec_r', q{SELECT s.id, o.w FROM public.s JOIN public.o ON o.id = s.id});
	is($rc, 0, 'a join of two tables the role may read') or diag($err);
	$n1->safe_psql('postgres', q{REVOKE SELECT ON public.o FROM sec_r});
	($rc, $out, $err) = as_role($n1, 'sec_r', q{SELECT s.id, o.w FROM public.s JOIN public.o ON o.id = s.id});
	isnt($rc, 0, 'a join with a table the role may not read is refused');
	like($err, qr/permission denied for table o/, '... with the error of PostgreSQL');
	$n1->safe_psql('postgres', q{GRANT SELECT ON public.o TO sec_r});
	# every group checks the privilege again, as the same role
	for my $nd ($n2, $n3)
	{
		$nd->safe_psql('postgres', q{SET kingres_shard.enable_routing = off; REVOKE SELECT ON public.s FROM sec_r});
		($rc, $out, $err) = as_role($n1, 'sec_r', $q);
		isnt($rc, 0, 'a group that revoked the privilege fails the statement (' . $nd->name . ')');
		like($err, qr/permission denied for table s/, '... with its error');
		$nd->safe_psql('postgres', q{SET kingres_shard.enable_routing = off; GRANT SELECT ON public.s TO sec_r});
	}
	is(prepared_count($_), 0, 'nothing is prepared on ' . $_->name) for @nodes;

	# a view of another owner: the view is a sub-select after rewriting, which is not shipped
	$n1->safe_psql('postgres', q{CREATE VIEW public.v_own AS SELECT id, count(*) AS c FROM public.s GROUP BY id;
		ALTER VIEW public.v_own OWNER TO sec_o; GRANT SELECT ON public.v_own TO sec_r});
	unlike(plan_of($n1, q{SELECT * FROM public.v_own}), qr/\Q$FS\E/, 'a view as the source is not shipped');
	($rc, $out, $err) = as_role($n1, 'sec_r', q{SELECT * FROM public.v_own});
	isnt($rc, 0, 'a view of another owner that needs another group is refused by the scan, as before (never shipped)');
	like($err, qr/read through a view owned by role/, '... with the message of the scan');

	# row-level security, made by hand with routing off (the utility hook refuses it): the statement is not shipped
	$_->safe_psql('postgres', q{SET kingres_shard.enable_routing = off; ALTER TABLE public.o ENABLE ROW LEVEL SECURITY}) for @nodes;
	my $join = q{SELECT s.id, o.w FROM public.s JOIN public.o ON o.id = s.id};
	unlike(plan_of($n1, $join), qr/\Q$FS\E/, 'a table with row-level security: not shipped');
	like(do { my ($r, $o, $e) = run_sql($n1, "EXPLAIN (VERBOSE, COSTS OFF) $join"); $o }, qr/Pushdown: not used: .*row-level security/, '... and the reason is shown');
	$_->safe_psql('postgres', q{SET kingres_shard.enable_routing = off; ALTER TABLE public.o DISABLE ROW LEVEL SECURITY}) for @nodes;
	like(plan_of($n1, $join), qr/\Q$FS\E/, 'without row-level security it is shipped again');

	# a function marked PARALLEL SAFE that runs a closed query (S2-08): never an answer from a worker's snapshot
	my $fn = q{CREATE FUNCTION public.pcnt(i int) RETURNS bigint LANGUAGE plpgsql PARALLEL SAFE AS $$ DECLARE r bigint; BEGIN
		SELECT count(*) INTO r FROM (SELECT id FROM public.s GROUP BY id) q; RETURN r + i - i; END $$};
	$c->on_every_node($fn);
	$c->on_every_node(q{INSERT INTO public.loc SELECT g, 1 FROM generate_series(1, 20000) g});
	$c->on_every_node(q{ANALYZE public.loc});
	my $par = q{SET parallel_setup_cost = 0; SET parallel_tuple_cost = 0; SET min_parallel_table_scan_size = 0; SET max_parallel_workers_per_gather = 4;
		SELECT sum(public.pcnt(x)) FROM public.loc WHERE x <= 3};
	($rc, $out, $err) = run_sql($n1, $par);
	ok($rc == 0 ? $out =~ /^\s*\d+\s*$/m && $out !~ /\b0\b/ || $out =~ /\d{4}/ : $err =~ /parallel worker/,
		'a PARALLEL SAFE function that runs a closed query answers right or refuses, never answers from a worker') or diag("$out\n$err");
}

# ---------------------------------------------------------------------------
# D. hostile values and names
# ---------------------------------------------------------------------------
{
	table(q{CREATE TABLE public."Od""d na.me" (id int, "a b" text, "c;--" int, "d*/" text)},
		q{; SELECT kingres.create_distributed_table('public."Od""d na.me"', 'id', bucket_count => 12)});
	$c->on_both(q{INSERT INTO public."Od""d na.me" SELECT g, 'it''s ' || g, g % 4, E'back\\slash ' || g FROM generate_series(1, 400) g, generate_series(1, 2) dup});
	$c->on_every_node(q{ANALYZE});
	$c->on_single(q{ANALYZE});
	my @hostile = (
		q{SELECT id, count(*), max("a b"), min("d*/") FROM public."Od""d na.me" GROUP BY id ORDER BY id},
		q{SELECT id, "c;--", count(*) FROM public."Od""d na.me" WHERE "a b" <> 'x*/;--' AND "d*/" NOT LIKE '%\\\\x%' GROUP BY id, "c;--" ORDER BY id},
		q{SELECT DISTINCT id, 'a''b\c', E'tab\there', 'uni' || U&'\00e9' FROM public."Od""d na.me" WHERE "a b" = 'it''s 7' OR id IN (1, 2, 3)},
		q{SELECT t.id, count(*) FROM public."Od""d na.me" t JOIN public."Od""d na.me" u ON u.id = t.id AND u."c;--" = t."c;--" GROUP BY t.id ORDER BY 1},
	);
	for my $q (@hostile)
	{
		$c->compare("hostile: $q", $q, ordered => ($q =~ /ORDER BY/ ? 1 : 0), ($q =~ /DISTINCT/ ? () : (path => $FS)));
	}
}

done_testing();
