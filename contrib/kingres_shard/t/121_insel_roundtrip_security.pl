# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/121_insel_roundtrip_security.pl
#
# Step 5 of phase 2b (design/shard-phase2b.md, 4.2, 5, 11.7, 15 and 16.4): the colocated INSERT ... SELECT
# shipped whole (P6) against forged fragments, privileges, the walker and hostile values.  These are the P6
# part of the design's 067 (forged fragments, privileges), of its 066 (hostile identifiers and values) and the
# test of the round trip of the fragment text.
#
#   A. the round trip: the text that the origin ships is accepted by a group, and the text of a statement that
#      the origin did not ship is refused by the group when it is forged into a fragment (the group decides on
#      the text that was deparsed and parsed again, with the function the origin used on the same text);
#   B. a forged fragment: no token, a stale placement, no marker key, a non-INSERT, a read of another group;
#   C. privileges: the caller's on every node, column privileges, a revoke on one group, a view of another
#      owner, a security barrier view, row-level security, a trigger on the target;
#   D. what is never shipped and why (volatile and stable functions, sequences, collations, types, whole-row
#      references, session settings);
#   E. values and names cross intact: hostile identifiers and constants, under every session profile.

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

my $c = KingresShardCompare->new('ir');
my ($n1, $n2, $n3) = $c->nodes;
my $single = $c->single;
my @nodes = ($n1, $n2, $n3);

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

sub not_shipped_reason
{
	my ($node, $sql) = @_;
	my ($rc, $out, $err) = run_sql($node, "SET client_min_messages = debug1; EXPLAIN (COSTS OFF) $sql");
	return $1 if $err =~ /INSERT \.\.\. SELECT is not shipped whole: (.*)/;
	return undef;
}

my $PUSHED = 'Kingres Fan-out Write';

# The text that is shipped to the groups, from EXPLAIN (the marker comment is part of it).
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

sub sorted { return join("\n", sort split /\n/, $_[0]); }

# ---------------------------------------------------------------------------
# the tables
# ---------------------------------------------------------------------------
table(q{CREATE TABLE public.src (id int PRIMARY KEY, v text, n int, g int)},
	q{; SELECT kingres.create_distributed_table('public.src', 'id', bucket_count => 12)});
table(q{CREATE TABLE public.dst (id int PRIMARY KEY, v text, n int DEFAULT 7)},
	q{; SELECT kingres.create_distributed_table('public.dst', 'id', colocate_with => 'public.src')});
table(q{CREATE TABLE public.other (id int PRIMARY KEY, w text)},
	q{; SELECT kingres.create_distributed_table('public.other', 'id', colocate_with => 'public.src')});
table(q{CREATE TABLE public.away (id int PRIMARY KEY, w text)},
	q{; SELECT kingres.create_distributed_table('public.away', 'id', bucket_count => 7)});
table(q{CREATE TABLE public.lookup (k int PRIMARY KEY, label text)},
	q{; SELECT kingres.create_reference_table('public.lookup')});
$c->on_both(q{INSERT INTO public.lookup VALUES (0, 'L0'), (1, 'L1'), (2, 'L2'), (3, 'L3'), (4, 'L4'), (5, 'L5'), (6, 'L6')});
$c->on_both(q{INSERT INTO public.src SELECT g, 'v' || g, g % 50, g % 7 FROM generate_series(1, 300) g});
$c->on_both(q{INSERT INTO public.other SELECT g, 'w' || g FROM generate_series(1, 300, 2) g});
$c->on_both(q{INSERT INTO public.away SELECT g, 'a' || g FROM generate_series(1, 50) g});

# ---------------------------------------------------------------------------
# A. the round trip
# ---------------------------------------------------------------------------
my @shipped = (
	q{INSERT INTO public.dst (id, v) SELECT id, v FROM public.src},
	q{INSERT INTO public.dst (id, v, n) SELECT s.id, o.w, s.n FROM public.src s JOIN public.other o ON o.id = s.id},
	q{INSERT INTO public.dst (id, v) SELECT s.id, l.label FROM public.src s JOIN public.lookup l ON l.k = s.g},
	q{INSERT INTO public.dst SELECT id, v, n FROM public.src WHERE n > 10 ON CONFLICT DO NOTHING},
	q{INSERT INTO public.dst (id, n) SELECT id, count(*) FROM public.src GROUP BY id},
	q{INSERT INTO public.dst (id, v) SELECT DISTINCT id, v FROM public.src},
	q{INSERT INTO public.dst (id, v) SELECT s.id, o.w FROM public.src s LEFT JOIN public.other o ON o.id = s.id},
	q{INSERT INTO public.dst (id, v) SELECT s.id, o.w FROM public.src s LEFT JOIN public.other o ON o.id = s.id AND o.id > 10 WHERE s.n > 3},
	q{INSERT INTO public.dst AS d (id, v, n) SELECT id, v, n FROM public.src ON CONFLICT (id) DO UPDATE SET n = d.n + excluded.n, v = excluded.v WHERE d.n < 100 RETURNING d.id, d.n},
	q{INSERT INTO public.dst (v, id) SELECT v, id FROM public.src WHERE id % 2 = 0},
	q{INSERT INTO public.dst (id, v) SELECT id, 'lit' FROM public.src},
	q{INSERT INTO public.dst (id, v, n) SELECT id, 'a''b\c', 5 FROM public.src WHERE v <> 'x*/;--'},
	q{INSERT INTO public.dst (id, v) SELECT s.id, s.v FROM public.src s, public.other o WHERE o.id = s.id},
	q{INSERT INTO public.dst (id, v) SELECT s.id, t.v FROM public.src s JOIN public.src t ON t.id = s.id},
	q{INSERT INTO public.dst (id, v) SELECT id, v FROM public.src WHERE id = ANY (ARRAY[1, 2, 3]) OR n IN (1, 2)},
	q{INSERT INTO public.dst (id, n) SELECT id, max(n) FROM public.src GROUP BY id HAVING max(n) > 2},
);
my @refused = (
	q{INSERT INTO public.dst (id, v) SELECT id + 1, v FROM public.src},
	q{INSERT INTO public.dst (id, v) SELECT s.id, o.w FROM public.src s JOIN public.other o ON o.id = s.n},
	q{INSERT INTO public.dst (id, v) SELECT id, v FROM public.src LIMIT 5},
	q{INSERT INTO public.dst (id, v) SELECT id, v FROM public.src WHERE id IN (SELECT id FROM public.other)},
	q{INSERT INTO public.dst (id, v) SELECT id, v FROM (SELECT id, v FROM public.src) q},
	q{INSERT INTO public.dst (id, v) SELECT a.id, a.w FROM public.away a},
	q{INSERT INTO public.dst (id, v) SELECT s.id, a.w FROM public.src s JOIN public.away a ON a.id = s.id},
	q{INSERT INTO public.dst (id, v) SELECT id, v || random()::text FROM public.src},
	q{INSERT INTO public.dst (id, v) SELECT id, current_user FROM public.src},
	q{INSERT INTO public.dst (id, v) SELECT s.id, o.w FROM public.src s FULL JOIN public.other o ON o.id = s.id},
	q{INSERT INTO public.dst (id, v) SELECT o.id, o.w FROM public.lookup l LEFT JOIN public.other o ON o.id = l.k},
	q{INSERT INTO public.dst (id, v) SELECT n, v FROM public.src GROUP BY n, v},
	q{INSERT INTO public.dst (id, v) SELECT id, v FROM public.src ON CONFLICT (id) DO UPDATE SET id = excluded.id},
);

for my $sql (@shipped)
{
	my $text = shipped_text($sql);
	ok(defined $text, "shipped: $sql") or next;
	like($text, qr{^/\* kingres:insert-select-fragment place=-?\d+ \*/ INSERT INTO }, '... the text starts with the marker and an INSERT');
	my ($rc, $out, $err) = as_node($n2, 2, $text);
	is($rc, 0, '... and a group accepts the text') or diag($err);
}
for my $sql (@refused)
{
	ok(!defined shipped_text($sql), "not shipped: $sql");
	my $place = (shipped_text(q{INSERT INTO public.dst (id, v) SELECT id, v FROM public.src}) =~ /place=(-?\d+)/)[0];
	my ($rc, $out, $err) = as_node($n2, 2, "/* kingres:insert-select-fragment place=$place */ $sql");
	isnt($rc, 0, '... a group refuses it when it is forged into a fragment');
	like($err, qr/0A000/, '... with 0A000 (a repeat gives the same answer)');
	like($err, qr/does not accept the fragment that another node sent it/, '... and says so');
}

# ---------------------------------------------------------------------------
# B. a forged fragment
# ---------------------------------------------------------------------------
{
	my $base = q{INSERT INTO public.dst (id, v) SELECT id, v FROM public.src};
	my $text = shipped_text($base);
	my ($place) = $text =~ /place=(-?\d+)/;
	my ($rc, $out, $err) = as_node($n2, 2, $text);
	is($rc, 0, 'the real fragment is accepted') or diag($err);

	# a stale placement is the retryable error, nothing is inserted
	my $stale = $text =~ s/place=-?\d+/place=12345/r;
	($rc, $out, $err) = as_node($n2, 2, $stale);
	isnt($rc, 0, 'a fragment with a placement that is not the group\'s is refused');
	like($err, qr/KS001/, '... with the retryable error');
	like($err, qr/not the one the statement was shipped with/, '... which says what is wrong');

	# no key in the marker, or a key that the group does not know
	for my $bad ('/* kingres:insert-select-fragment */ ', '/* kingres:insert-select-fragment place=abc */ ',
				 '/* kingres:insert-select-fragment place=1 foo=2 */ ')
	{
		my $t = $text =~ s{^/\* [^*]*\*/ }{$bad}r;
		($rc, $out, $err) = as_node($n2, 2, $t);
		isnt($rc, 0, "a marker that is not one: $bad");
		like($err, qr/0A000|does not accept|syntax/, '... is refused');
	}

	# a statement of another kind with the marker
	for my $stmt (q{SELECT count(*) FROM public.src}, q{DELETE FROM public.dst}, q{UPDATE public.dst SET n = 1},
				  q{INSERT INTO public.dst (id, v) VALUES (1, 'a'), (2, 'b')})
	{
		($rc, $out, $err) = as_node($n2, 2, "/* kingres:insert-select-fragment place=$place */ $stmt");
		isnt($rc, 0, "a $stmt with the marker is refused");
		like($err, qr/0A000/, '... with 0A000');
	}

	# a session that is not forwarded is an ordinary session: the marker is a comment
	($rc, $out, $err) = run_sql($n1, "/* kingres:insert-select-fragment place=$place */ INSERT INTO public.dst (id, v) SELECT id, v FROM public.src WHERE id <= 40 RETURNING id");
	is($rc, 0, 'a client that writes the marker gets an ordinary statement') or diag($err);
	is(scalar(split /\n/, $out), 40, '... the statement of the client, run for all its rows');
	$c->on_cluster(q{TRUNCATE public.dst});

	# a forwarded session without the token is refused before any statement runs
	{
		local $ENV{PGOPTIONS} = '-c kingres_shard.forwarded=on';
		($rc, $out, $err) = run_sql($n2, $text);
		isnt($rc, 0, 'a session that claims to be forwarded without the token is refused');
	}
	# a fragment that names a table of another group's layout reads something this node cannot answer for
	($rc, $out, $err) = as_node($n2, 2, "/* kingres:insert-select-fragment place=$place */ INSERT INTO public.away (id, w) SELECT id, v FROM public.src");
	isnt($rc, 0, 'a fragment whose target is of another colocation group is refused');
}

# ---------------------------------------------------------------------------
# C. privileges
# ---------------------------------------------------------------------------
{
	$_->safe_psql('postgres', q{CREATE ROLE sec_w LOGIN; CREATE ROLE sec_c LOGIN; CREATE ROLE sec_r LOGIN; CREATE ROLE sec_n LOGIN; CREATE ROLE sec_o LOGIN;
		GRANT USAGE ON SCHEMA public TO sec_w, sec_c, sec_r, sec_n, sec_o}) for @nodes;
	$n1->safe_psql('postgres', q{
		GRANT INSERT, SELECT ON public.dst TO sec_w;
		GRANT SELECT ON public.src, public.other, public.lookup TO sec_w, sec_c, sec_r, sec_o;
		GRANT INSERT (id, v), SELECT (id) ON public.dst TO sec_c;
		GRANT SELECT ON public.dst TO sec_r;
		GRANT INSERT, SELECT ON public.dst TO sec_o});
	$n1->safe_psql('postgres', 'SELECT kingres.recover_prepared_transactions()');
	my $total = sub { $n1->safe_psql('postgres', 'SELECT count(*) FROM public.dst') };
	my $stmt = q{INSERT INTO public.dst (id, v) SELECT id, v FROM public.src WHERE id <= 60};
	like(plan_of($n1, $stmt), qr/\Q$PUSHED\E/, 'the statement of the privilege matrix is shipped');

	my ($rc, $out, $err) = as_role($n1, 'sec_w', $stmt);
	is($rc, 0, 'a role with INSERT and SELECT runs it') or diag($err);
	is($total->(), 60, '... and the rows are there');
	$n1->safe_psql('postgres', 'TRUNCATE public.dst');
	($rc, $out, $err) = as_role($n2, 'sec_c', $stmt);
	is($rc, 0, 'a role with INSERT on two columns runs it (from another node)') or diag($err);
	is($n1->safe_psql('postgres', q{SELECT string_agg(DISTINCT n::text, ',') FROM public.dst}), '7', '... and the groups apply the constant default of the column it cannot write');
	$n1->safe_psql('postgres', 'TRUNCATE public.dst');

	for my $case (
		[ 'sec_r', $stmt, qr/permission denied for table dst/, 'a role that can only read the target' ],
		[ 'sec_n', $stmt, qr/permission denied for table (dst|src)/, 'a role with no privilege' ],
		[ 'sec_c', q{INSERT INTO public.dst (id, v, n) SELECT id, v, n FROM public.src}, qr/permission denied for table dst/, 'a column without INSERT' ],
		[ 'sec_c', q{INSERT INTO public.dst (id, v) SELECT id, v FROM public.src RETURNING v}, qr/permission denied for table dst/, 'RETURNING a column without SELECT' ],
		[ 'sec_w', q{INSERT INTO public.dst (id, v) SELECT id, v FROM public.src ON CONFLICT (id) DO UPDATE SET v = excluded.v}, qr/permission denied for table dst/, 'DO UPDATE without UPDATE' ],
	)
	{
		my ($role, $sql, $re, $name) = @$case;
		($rc, $out, $err) = as_role($n1, $role, $sql);
		isnt($rc, 0, "$name: refused");
		like($err, $re, '... with the error of PostgreSQL');
		is($total->(), 0, '... no row is left');
		is(prepared_count($_), 0, '... nothing is prepared on ' . $_->name) for @nodes;
	}
	# a source the role may not read, on a group
	$n1->safe_psql('postgres', q{REVOKE SELECT ON public.other FROM sec_w});
	($rc, $out, $err) = as_role($n1, 'sec_w', q{INSERT INTO public.dst (id, v) SELECT s.id, o.w FROM public.src s JOIN public.other o ON o.id = s.id});
	isnt($rc, 0, 'a join with a table the role may not read is refused');
	like($err, qr/permission denied for table other/, '... with the error of PostgreSQL');
	$n1->safe_psql('postgres', q{GRANT SELECT ON public.other TO sec_w});
	# every group checks the privilege again, as the same role
	for my $nd ($n2, $n3)
	{
		$nd->safe_psql('postgres', q{SET kingres_shard.enable_routing = off; REVOKE INSERT ON public.dst FROM sec_w});
		($rc, $out, $err) = as_role($n1, 'sec_w', $stmt);
		isnt($rc, 0, 'a group that revoked the privilege fails the statement (' . $nd->name . ')');
		like($err, qr/permission denied for table dst/, '... with its error');
		is($total->(), 0, '... and no group keeps a row');
		is(prepared_count($_), 0, '... nor a prepared transaction on ' . $_->name) for @nodes;
		$nd->safe_psql('postgres', q{SET kingres_shard.enable_routing = off; GRANT INSERT ON public.dst TO sec_w});
	}

	# a view of another owner, and a security barrier view: the source is a sub-select, which is not shipped
	$n1->safe_psql('postgres', q{
		CREATE VIEW public.v_inv WITH (security_invoker = true) AS SELECT id, v FROM public.src WHERE n < 40;
		CREATE VIEW public.v_own AS SELECT id, v FROM public.src WHERE n < 40;
		CREATE VIEW public.v_bar WITH (security_barrier = true) AS SELECT id, v FROM public.src WHERE n < 40;
		ALTER VIEW public.v_own OWNER TO sec_o; ALTER VIEW public.v_bar OWNER TO sec_o;
		GRANT SELECT ON public.v_inv, public.v_own, public.v_bar TO sec_w});
	for my $v ('v_inv', 'v_own', 'v_bar')
	{
		like(not_shipped_reason($n1, "INSERT INTO public.dst (id, v) SELECT id, v FROM public.$v") // '', qr/something other than a table/,
			"a view ($v) as the source is not shipped");
	}
	($rc, $out, $err) = as_role($n1, 'sec_w', q{INSERT INTO public.dst (id, v) SELECT id, v FROM public.v_own});
	isnt($rc, 0, 'a view of another owner is refused by the router');
	like($err, qr/through a view owned by role/, '... with the message of the router');
	($rc, $out, $err) = as_role($n1, 'sec_w', q{INSERT INTO public.dst (id, v) SELECT id, v FROM public.v_bar});
	isnt($rc, 0, 'a security barrier view is refused by the router');
	like($err, qr/security barrier view/, '... with its message');
	($rc, $out, $err) = as_role($n1, 'sec_w', q{INSERT INTO public.dst (id, v) SELECT id, v FROM public.v_inv});
	is($rc, 0, 'a security_invoker view is read by the router') or diag($err);
	$n1->safe_psql('postgres', 'TRUNCATE public.dst');

	# an updatable view (and WITH CHECK OPTION) as the target
	$n1->safe_psql('postgres', q{CREATE VIEW public.w_dst AS SELECT id, v FROM public.dst WHERE id > 0 WITH CHECK OPTION});
	($rc, $out, $err) = run_sql($n1, q{INSERT INTO public.w_dst SELECT id, v FROM public.src});
	isnt($rc, 0, 'a view WITH CHECK OPTION as the target is not shipped (the router refuses it)');
	like($err, qr/cannot check the rows against a view or a row-level security policy/, '... with its message');

	# a trigger on the target (made by hand with routing off) would run on the groups
	$_->safe_psql('postgres', q{SET kingres_shard.enable_routing = off;
		CREATE FUNCTION public.trg_noop() RETURNS trigger LANGUAGE plpgsql AS $f$ BEGIN RETURN NEW; END $f$;
		CREATE TRIGGER dst_trg BEFORE INSERT ON public.dst FOR EACH ROW EXECUTE FUNCTION public.trg_noop()}) for @nodes;
	like(not_shipped_reason($n1, $stmt) // '', qr/trigger or a rule/, 'a trigger on the target: not shipped');
	$_->safe_psql('postgres', q{SET kingres_shard.enable_routing = off; DROP TRIGGER dst_trg ON public.dst}) for @nodes;
	is_deeply([ map { not_shipped_reason($n1, $stmt) // 'shipped' } (1) ], ['shipped'], 'without the trigger it is shipped again');

	# row-level security, made by hand with routing off (the utility hook refuses it): on a table the statement reads
	# and on its target.  The statement is not shipped: the groups would apply the policies of their own role.
	$_->safe_psql('postgres', q{SET kingres_shard.enable_routing = off; ALTER TABLE public.lookup ENABLE ROW LEVEL SECURITY}) for @nodes;
	like(not_shipped_reason($n1, q{INSERT INTO public.dst (id, v) SELECT s.id, l.label FROM public.src s JOIN public.lookup l ON l.k = s.g}) // '',
		qr/row-level security/, 'a source with row-level security: not shipped');
	$_->safe_psql('postgres', q{SET kingres_shard.enable_routing = off; ALTER TABLE public.lookup DISABLE ROW LEVEL SECURITY}) for @nodes;
	$_->safe_psql('postgres', q{SET kingres_shard.enable_routing = off; ALTER TABLE public.dst ENABLE ROW LEVEL SECURITY}) for @nodes;
	like(not_shipped_reason($n1, $stmt) // '', qr/row-level security/, 'a target with row-level security: not shipped');
	$_->safe_psql('postgres', q{SET kingres_shard.enable_routing = off; ALTER TABLE public.dst DISABLE ROW LEVEL SECURITY}) for @nodes;
	like(plan_of($n1, $stmt), qr/\Q$PUSHED\E/, 'without row-level security it is shipped again');

	# a session that switches the triggers off cannot do it through this statement
	($rc, $out, $err) = run_sql($n1, qq{SET session_replication_role = replica; $stmt});
	isnt($rc, 0, 'session_replication_role replica: refused');
	like($err, qr/cannot be written while session_replication_role is/, '... with the message of the other writes');
	is($total->(), 0, '... nothing was inserted');
}

# ---------------------------------------------------------------------------
# D. what is never shipped, and why
# ---------------------------------------------------------------------------
{
	$c->on_every_node(q{CREATE TYPE public.mood AS ENUM ('sad', 'ok')});
	$c->on_single(q{CREATE TYPE public.mood AS ENUM ('sad', 'ok')});
	table(q{CREATE TABLE public.sq (id int PRIMARY KEY, a text, d int DEFAULT 5)},
		q{; SELECT kingres.create_distributed_table('public.sq', 'id', colocate_with => 'public.src')});
	table(q{CREATE TABLE public.sq_ser (id int PRIMARY KEY, a text, s serial)},
		q{; SELECT kingres.create_distributed_table('public.sq_ser', 'id', colocate_with => 'public.src')});
	table(q{CREATE TABLE public.sq_idn (id int PRIMARY KEY, a text, ia bigint GENERATED BY DEFAULT AS IDENTITY)},
		q{; SELECT kingres.create_distributed_table('public.sq_idn', 'id', colocate_with => 'public.src')});
	table(q{CREATE TABLE public.sq_now (id int PRIMARY KEY, a text, t timestamptz DEFAULT now())},
		q{; SELECT kingres.create_distributed_table('public.sq_now', 'id', colocate_with => 'public.src')});
	table(q{CREATE TABLE public.sq_typ (id int PRIMARY KEY, m money, e public.mood, f float8)},
		q{; SELECT kingres.create_distributed_table('public.sq_typ', 'id', colocate_with => 'public.src')});
	table(q{CREATE TABLE public.sqs (id int PRIMARY KEY, a text, m money, e public.mood, f float8, ts timestamptz, ia bigint)},
		q{; SELECT kingres.create_distributed_table('public.sqs', 'id', colocate_with => 'public.src')});
	$c->on_both(q{INSERT INTO public.sqs SELECT g, 'a' || g, g::money, 'ok', g / 3.0, '2026-01-01'::timestamptz + g * interval '1 hour', g FROM generate_series(1, 30) g});
	my @no = (
		[ q{INSERT INTO public.sq_ser (id, a) SELECT id, a FROM public.sqs}, qr/volatile default/, 'a serial column the statement does not name' ],
		[ q{INSERT INTO public.sq_idn (id, a) SELECT id, a FROM public.sqs}, qr/volatile default/, 'an identity column the statement does not name' ],
		[ q{INSERT INTO public.sq_now (id, a) SELECT id, a FROM public.sqs}, qr/default that is not a built-in immutable expression/, 'a stable default (now())' ],
		[ q{INSERT INTO public.sq_typ (id, m) SELECT id, m FROM public.sqs}, qr/not built in/, 'money' ],
		[ q{INSERT INTO public.sq_typ (id, e) SELECT id, e FROM public.sqs}, qr/not built in/, 'an enum' ],
		[ q{INSERT INTO public.sq_typ (id, f) SELECT id, sum(f) FROM public.sqs GROUP BY id}, qr/order of the rows/, 'a float aggregate' ],
		[ q{INSERT INTO public.sq (id, a) SELECT id, a || a FROM public.sqs WHERE ts > now()}, qr/not built in and immutable/, 'a stable function in the restriction' ],
		[ q{INSERT INTO public.sq (id, a) SELECT id, a FROM public.sqs WHERE a COLLATE "POSIX" = 'a1'}, qr/collation/, 'a collation that is not default or C' ],
		[ q{INSERT INTO public.sq (id, a) SELECT id, (sqs.*)::text FROM public.sqs}, qr/whole-row|text form|not built in/, 'a whole-row reference' ],
		[ q{INSERT INTO public.sq (id, a) SELECT id, a FROM public.sqs WHERE ctid IS NOT NULL}, qr/system column/, 'a system column' ],
		[ q{INSERT INTO public.sq (id, a) SELECT id, to_char(ts, 'HH')  FROM public.sqs}, qr/not built in and immutable/, 'to_char of a timestamptz (stable)' ],
		[ q{INSERT INTO public.sq (id, a) SELECT id, a FROM public.sqs TABLESAMPLE SYSTEM (50)}, qr/samples a table/, 'a table sample' ],
		[ q{INSERT INTO public.sq (id, a) SELECT id, a FROM public.sqs FOR UPDATE}, qr/locks rows|row locking/, 'row locking' ],
		[ q{WITH x AS (SELECT id, a FROM public.sqs) INSERT INTO public.sq (id, a) SELECT id, a FROM x}, qr/WITH clause/, 'a WITH clause' ],
		[ q{INSERT INTO public.sq (id, a) SELECT id, a FROM public.sqs UNION ALL SELECT id, a FROM public.sqs}, qr/set operation|something other than a table|source/, 'a set operation' ],
	);
	for my $f (@no)
	{
		my $why = not_shipped_reason($n1, $f->[0]);
		ok(defined $why, "not shipped: $f->[2]");
		like($why // '', $f->[1], '... for the reason');
	}
	# each of them still gives the answer of the single node (the router computes the defaults once at the origin)
	for my $sql (q{INSERT INTO public.sq_typ (id, m, e, f) SELECT id, m, e, f FROM public.sqs},
				 q{INSERT INTO public.sq (id, a) SELECT id, a FROM public.sqs WHERE ts > now() - interval '100 years'},
				 q{INSERT INTO public.sq_now (id, a) SELECT id, a FROM public.sqs})
	{
		my ($rd, $od, $ed) = run_sql($n1, $sql);
		is($rd, 0, "the router gives the answer: $sql") or diag($ed);
		$c->on_single($sql);
	}
	$c->compare_tables('public.sq_typ', order_by => 'id', name => 'the router rows of money, enum and float columns');
	$c->compare_tables('public.sq', order_by => 'id', name => 'the router rows of a stable restriction');
	$c->compare_tables('public.sq_now', order_by => 'id', name => 'the router rows with a stable default',
		query => q{SELECT id, a, t IS NOT NULL FROM public.sq_now ORDER BY id});
	is($n1->safe_psql('postgres', q{SELECT count(DISTINCT t) FROM public.sq_now}), 1, 'a stable default is computed once, at the origin (the same for every group)');
}

# ---------------------------------------------------------------------------
# E. values and names cross intact
# ---------------------------------------------------------------------------
{
	# every type that is shipped, and constants of every kind, in every session profile
	table(q{CREATE TABLE public.ty (id int PRIMARY KEY, t text, vc varchar(20), n numeric(10,3), f8 float8, f4 float4, b boolean,
			d date, ts timestamp, iv interval, ba bytea, js jsonb, u uuid, ia int[], ta text[], c char(5), i8 bigint, r int4range)},
		q{; SELECT kingres.create_distributed_table('public.ty', 'id', colocate_with => 'public.src')});
	table(q{CREATE TABLE public.ty2 (LIKE public.ty INCLUDING DEFAULTS)},
		q{; ALTER TABLE public.ty2 ADD PRIMARY KEY (id); SELECT kingres.create_distributed_table('public.ty2', 'id', colocate_with => 'public.src')});
	my $hostile = q{ARRAY['plain', 'with,comma', 'with{brace}', 'with"quote', 'with\\back', 'NULL', '', ' lead and trail ', E'new\nline', E'tab\there', 'uni' || chr(233) || chr(8364), '{}', '"', '\\', ',', 'a b']};
	my $load = qq{INSERT INTO public.ty
		SELECT k, h, left(h, 20), k * 1.125, k / 7.0, (k / 3.0)::float4, k % 2 = 0,
		       date '2026-03-04' + k, timestamp '2026-03-04 05:06:07.891' + k * interval '1 hour', k * interval '1 day 02:03:04.5',
		       decode(lpad(to_hex(k), 4, '0'), 'hex'), jsonb_build_object('k', k, 's', h), md5(k::text)::uuid,
		       ARRAY[k, NULL, k + 1], ARRAY[h, NULL, 'NULL'], 'ab', k::bigint * 1000000007, int4range(k, k + 5)
		  FROM (SELECT k, (SELECT a[1 + k % 16] FROM (SELECT $hostile AS a) s) AS h FROM generate_series(1, 48) k) q};
	$c->on_both($load);
	my $copy = q{INSERT INTO public.ty2 SELECT id, t || 'x', vc, n + 0.5, f8 * 2, f4, NOT b, d + 1, ts + interval '1 day 2 hours', iv + interval '1 hour', ba, js, u, ia, ta, c, i8 - 1, r FROM public.ty};
	like(plan_of($n1, $copy), qr/\Q$PUSHED\E/, 'a copy with expressions of every type is shipped');
	my $i = 0;
	for my $profile ('RESET ALL', @KingresShardCompare::PROFILES)
	{
		$c->on_both(q{TRUNCATE public.ty2});
		$c->compare_dml("every type under the profile $i", $copy, settings => "$profile;");
		$c->compare_tables('public.ty2', order_by => 'id', name => "every type, profile $i: the copy");
		$i++;
	}
	# a hostile constant inside the statement text
	for my $lit (q{'a,b"c\d{e}'}, q{'NULL'}, q{E'new\nline\ttab'}, q{'*/ ; DROP TABLE public.src; /*'}, q{'uni' || chr(233) || chr(128512)}, q{''''})
	{
		my $s = "INSERT INTO public.dst (id, v) SELECT id, $lit FROM public.src WHERE id <= 5";
		like(plan_of($n1, $s), qr/\Q$PUSHED\E/, "a hostile constant is shipped: $lit");
		$c->compare_dml("hostile constant $lit", $s);
		$c->compare_tables('public.dst', order_by => 'id', name => "after the constant $lit");
		$c->on_both(q{TRUNCATE public.dst});
	}
	# hostile identifiers
	my $tn = q{"x*/ ; DROP TABLE y; /*"};
	my $cn = q{"select"};
	my $cn2 = qq{"a b"};
	table(qq{CREATE TABLE public.$tn (id int PRIMARY KEY, $cn text, $cn2 int, "t\nnl" text)},
		qq{; SELECT kingres.create_distributed_table('public.$tn', 'id', colocate_with => 'public.src')});
	table(qq{CREATE TABLE public."a""b" (id int PRIMARY KEY, $cn text, $cn2 int, "t\nnl" text)},
		qq{; SELECT kingres.create_distributed_table('public."a""b"', 'id', colocate_with => 'public.src')});
	$c->on_both(qq{INSERT INTO public.$tn SELECT g, 'x' || g, g, 't' FROM generate_series(1, 40) g});
	my $h = qq{INSERT INTO public."a""b" (id, $cn, $cn2, "t\nnl") SELECT id, $cn || 'h', $cn2 + 1, "t\nnl" FROM public.$tn WHERE $cn2 > 3};
	like(plan_of($n1, $h), qr/\Q$PUSHED\E/, 'hostile table and column names: shipped');
	$c->compare_dml('hostile names', $h);
	$c->compare_tables(q{public."a""b"}, order_by => 'id', name => 'hostile names: the copy');
	# a source that shadows built-in names must not change the text that the group reads
	$c->on_both(q{CREATE FUNCTION public.sum(int) RETURNS int LANGUAGE sql AS 'SELECT 42'});
	my $sh = q{INSERT INTO public.dst (id, n) SELECT id, pg_catalog.sum(n) FROM public.src GROUP BY id};
	$c->compare_dml('a function public.sum does not change the aggregate', $sh);
	$c->compare_tables('public.dst', order_by => 'id', name => 'the aggregate is pg_catalog.sum');
	$c->on_both(q{TRUNCATE public.dst});
	# a SET of the session does not change what is shipped
	my @sets = (q{SET search_path = pg_temp, public}, q{SET search_path = ''});
	# PostgreSQL 19 no longer has non-standard string literals
	push @sets, q{SET standard_conforming_strings = off; SET escape_string_warning = off}
	  if $n1->safe_psql('postgres', q{SELECT count(*) FROM pg_settings WHERE name = 'escape_string_warning'}) == 1;
	for my $set (@sets)
	{
		my ($rd, $od, $ed) = run_sql($n1, "$set; INSERT INTO public.dst (id, v) SELECT id, 'q\\w' FROM public.src WHERE id <= 5");
		is($rd, 0, "$set") or diag($ed);
		my ($rs, $os, $es) = run_sql($single, "$set; INSERT INTO public.dst (id, v) SELECT id, 'q\\w' FROM public.src WHERE id <= 5");
		is($rs, 0, "$set (single node)") or diag($es);
		$c->compare_tables('public.dst', order_by => 'id', name => "after $set");
		$c->on_both(q{TRUNCATE public.dst});
	}
}

# ---------------------------------------------------------------------------
# EXPLAIN
# ---------------------------------------------------------------------------
{
	my $p = plan_of($n1, q{INSERT INTO public.dst (id, v) SELECT id, v FROM public.src WHERE id <= 5 RETURNING id});
	like($p, qr/Custom Scan \(Kingres Fan-out Write\)/, 'EXPLAIN names the custom scan');
	like($p, qr/Pushed: colocated INSERT \.\.\. SELECT/, '... says what was pushed');
	like($p, qr/Nodes: 3/, '... how many groups run it');
	like($p, qr/Remote SQL: \/\* kingres:insert-select-fragment place=-?\d+ \*\/ INSERT INTO public\.dst \(id, v\)\s+SELECT/, '... and the text that they run');
	my ($rc, $out, $err) = run_sql($n1, q{BEGIN; EXPLAIN (ANALYZE, COSTS OFF, TIMING OFF) INSERT INTO public.dst (id, v) SELECT id, v FROM public.src WHERE id <= 5; ROLLBACK});
	is($rc, 0, 'EXPLAIN ANALYZE runs the statement') or diag($err);
	is($n1->safe_psql('postgres', 'SELECT count(*) FROM public.dst'), 0, '... and the rollback undid it');
}


# ---------------------------------------------------------------------------
# E2. other ways a statement reaches the planner: COPY (INSERT ... RETURNING), a SQL function, a PL/pgSQL loop that
#     re-uses one cached plan, extreme constants (NaN, infinity, -0, the extremes of the types)
# ---------------------------------------------------------------------------
{
	$c->on_both(q{TRUNCATE public.dst});
	my $copy_sql = q{COPY (INSERT INTO public.dst (id, v) SELECT id, v FROM public.src WHERE id <= 30 RETURNING id, v) TO STDOUT};
	my ($rs, $os, $es) = run_sql($single, $copy_sql);
	my ($rd, $od, $ed) = run_sql($n1, $copy_sql);
	is($rd, 0, 'COPY (INSERT ... RETURNING) TO STDOUT runs a shipped statement') or diag($ed);
	is(sorted($od), sorted($os), '... and gives the rows of the single node');
	$c->compare_tables('public.dst', order_by => 'id', name => 'after COPY (INSERT ... RETURNING)');
	$c->on_both(q{TRUNCATE public.dst});

	$c->on_both(q{CREATE FUNCTION public.ins_sql(lim int) RETURNS void LANGUAGE sql AS 'INSERT INTO public.dst (id, v) SELECT id, v FROM public.src WHERE id <= lim'});
	($rd, $od, $ed) = run_sql($n1, q{SELECT public.ins_sql(25)});
	is($rd, 0, 'a SQL function that runs a shipped statement') or diag($ed);
	run_sql($single, q{SELECT public.ins_sql(25)});
	$c->compare_tables('public.dst', order_by => 'id', name => 'after the SQL function');
	$c->on_both(q{TRUNCATE public.dst});

	my $loop = q{DO $$ DECLARE i int; n bigint := 0; c bigint; BEGIN
		  FOR i IN 1 .. 12 LOOP
		    INSERT INTO public.dst (id, v) SELECT id, v FROM public.src WHERE id % 12 = i - 1 + 0 AND id <= 120 ON CONFLICT DO NOTHING;
		    GET DIAGNOSTICS c = ROW_COUNT; n := n + c;
		  END LOOP;
		  RAISE NOTICE 'LOOPROWS=%', n; END $$};
	($rs, $os, $es) = run_sql($single, $loop);
	($rd, $od, $ed) = run_sql($n1, $loop);
	is($rd, 0, 'a PL/pgSQL loop that re-uses one plan of a shipped statement') or diag($ed);
	my ($want) = $es =~ /LOOPROWS=(\d+)/;
	my ($got) = $ed =~ /LOOPROWS=(\d+)/;
	is($got, $want, '... counts the rows as the single node does');
	$c->compare_tables('public.dst', order_by => 'id', name => 'after the loop');
	$c->on_both(q{TRUNCATE public.dst});

	table(q{CREATE TABLE public.ext (id int PRIMARY KEY, f float8, f4 float4, d date, ts timestamp, n numeric, i2 smallint, i8 bigint, iv interval)},
		q{; SELECT kingres.create_distributed_table('public.ext', 'id', colocate_with => 'public.src')});
	my $ext = q{INSERT INTO public.ext SELECT id, CASE id % 6 WHEN 0 THEN 'NaN'::float8 WHEN 1 THEN 'Infinity'::float8 WHEN 2 THEN '-Infinity'::float8 WHEN 3 THEN '-0'::float8
		WHEN 4 THEN 1e308::float8 ELSE 4.9e-324::float8 END, 'NaN'::float4, CASE id % 3 WHEN 0 THEN 'infinity'::date WHEN 1 THEN '-infinity'::date ELSE date '4713-01-01 BC' END,
		CASE id % 2 WHEN 0 THEN 'infinity'::timestamp ELSE timestamp '294276-12-31 23:59:59' END, 123456789012345678901234567890.123456789::numeric, (-32768)::smallint,
		9223372036854775807::bigint, interval '-178000000 years 3 mons -5 days 07:00:00.000001' FROM public.src WHERE id <= 30};
	like(plan_of($n1, $ext), qr/\Q$PUSHED\E/, 'a statement with the extreme constants of the types is shipped');
	my $i = 0;
	for my $profile ('RESET ALL', @KingresShardCompare::PROFILES)
	{
		$c->on_both(q{TRUNCATE public.ext});
		$c->compare_dml("extreme constants under the profile $i", $ext, settings => "$profile;");
		$c->compare_tables('public.ext', order_by => 'id', name => "extreme constants, profile $i");
		$i++;
	}
}
is(prepared_count($_), 0, 'nothing is prepared on ' . $_->name) for @nodes;
done_testing();
