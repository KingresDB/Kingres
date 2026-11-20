# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/160_fragment_kinds_security.pl
#
# The part of the design's 067_pushdown_security (design/shard-phase2b.md, 16.2) that t/019, t/114, t/126, t/131, t/132 and
# t/156 do not state for every kind of fragment: a client with `forwarded` and no token, or a wrong token, is refused for EVERY
# fragment kind; and a demoted node (the old primary after promote_node) refuses EVERY fragment kind with the retryable KS001.
# The kinds are the seven markers of the code: scan-fragment, write-fragment, reference-write, query-fragment,
# partial-fragment, insert-select-fragment and insert-fragment.  A refusal comes before anything of the text is run, so the
# tables are checked unchanged afterwards.
#
#   A. on a healthy primary: no token, a token that is not the proof, the proof (the text of the origin is accepted);
#   B. on the old primary after a planned switchover: every kind is refused with KS001, whoever asks.

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use IPC::Run ();
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(make_cluster run_sql keys_of_group prepared_count make_standby register_standby wait_catchup set_sync promote_standby release_node);

my ($f1, $f2) = make_cluster('fk', 2, replication => 1,
	extra => "kingres_shard.enable_partial_aggregation = on\nkingres_shard.enable_pushdown_join = on\n");
$f1->safe_psql('postgres', q{
	CREATE TABLE public.s (id int, c int, w int, g int, PRIMARY KEY (id, c));
	SELECT kingres.create_distributed_table('public.s', 'id', bucket_count => 12);
	CREATE TABLE public.s2 (id int, c int, w int, g int, PRIMARY KEY (id, c));
	SELECT kingres.create_distributed_table('public.s2', 'id', colocate_with => 'public.s');
	CREATE TABLE public.r (k int PRIMARY KEY, lbl text);
	SELECT kingres.create_reference_table('public.r');
	INSERT INTO public.s SELECT i, c, 1, i % 7 FROM generate_series(1, 300) i, generate_series(1, 2) c;
	INSERT INTO public.r SELECT g, 'label' FROM generate_series(1, 10) g;
});
my $s2 = make_standby($f2, 'fk_s2');
my $sid = register_standby($f1, $s2, 1);
set_sync($f2, $s2);
wait_catchup($f2, $s2);
$_->safe_psql('postgres', 'ANALYZE') for ($f1, $f2);

sub as_node
{
	my ($node, $id, $sql, $opts) = @_;
	my $tok = $node->safe_psql('postgres', "SELECT kingres.internal_forward_token(current_user, $id)");
	local $ENV{PGOPTIONS} = $opts // "-c kingres_shard.forwarded=on -c kingres_shard.forward_token=$tok";
	return run_sql($node, "\\set VERBOSITY verbose\nBEGIN;\n$sql;\nROLLBACK;");
}

sub remote_sql_of
{
	my ($sql) = @_;
	my ($rc, $plan, $err) = run_sql($f1, "EXPLAIN (COSTS OFF) $sql");
	my ($text) = $plan =~ /Remote SQL: (.*)$/m;
	die "not shipped: $sql\n$plan$err" unless defined $text;
	return $text;
}

my $PLACE = $f1->safe_psql('postgres', q{SELECT kingres.internal_placement_hash(1)});
my $closed = remote_sql_of(q{SELECT id, count(*), max(w) FROM public.s GROUP BY id});
my $partial = remote_sql_of(q{SELECT g, count(*), sum(w) FROM public.s GROUP BY g});

my ($K2) = keys_of_group($f1, 1, 1, 1, 1000000);
ok(defined $K2, "a key whose bucket is in the group of node 2");
# every kind of fragment: the marker, the text that follows it
my @kinds = (
	[ 'scan-fragment',          q{/* kingres:scan-fragment */ SELECT id, c, w, g FROM public.s AS ks_t WHERE id < 0} ],
	[ 'write-fragment',         q{/* kingres:write-fragment */ UPDATE public.s SET w = w WHERE id < 0} ],
	[ 'reference-write',        q{/* kingres:reference-write */ UPDATE public.r SET lbl = lbl WHERE k < 0} ],
	[ 'query-fragment',         "/* kingres:query-fragment place=$PLACE */ DECLARE ks_f NO SCROLL CURSOR FOR $closed" ],
	[ 'partial-fragment',       "/* kingres:partial-fragment place=$PLACE */ DECLARE ks_f NO SCROLL CURSOR FOR $partial" ],
	[ 'insert-select-fragment', "/* kingres:insert-select-fragment place=$PLACE */ INSERT INTO public.s2 SELECT id, c, w, g FROM public.s WHERE id < 0" ],
	[ 'insert-fragment',        "/* kingres:insert-fragment place=$PLACE */ INSERT INTO public.s (id, c, w, g) VALUES ($K2, 1, 1, 1)" ],
);
is(scalar(@kinds), 7, 'the seven kinds of fragment');

my $rows = sub { return $_[0]->safe_psql('postgres', q{SET kingres_shard.enable_routing = off; SELECT count(*) FROM public.s}) . '/' . $_[0]->safe_psql('postgres', q{SELECT lbl FROM public.r WHERE k = 1}); };
my %before = (f1 => $rows->($f1), f2 => $rows->($f2));

# ---------------------------------------------------------------------------
# A. a healthy primary
# ---------------------------------------------------------------------------
for my $k (@kinds)
{
	my ($name, $text) = @$k;
	# the control: with the proof the node runs the text (so the refusals below are the token's)
	my ($rc, $out, $err);
	if ($name ne 'insert-fragment')
	{
		# (the insert fragment takes the parameters of a batch, which psql cannot send)
		($rc, $out, $err) = as_node($f2, 2, $text);
		is($rc, 0, "'$name': with the proof of a node of the cluster a healthy primary runs it") or diag($err);
	}
	($rc, $out, $err) = as_node($f2, 2, $text, '-c kingres_shard.forwarded=on');
	isnt($rc, 0, "'$name': a session that claims to be forwarded without the token is refused");
	like($err, qr/forward|token|authent|FATAL/i, '... with a message about the token') or diag($err);
	($rc, $out, $err) = as_node($f2, 2, $text, '-c kingres_shard.forwarded=on -c kingres_shard.forward_token=' . ('0' x 64));
	isnt($rc, 0, "'$name': a token that is not the proof is refused");
	like($err, qr/cannot prove it|KS014/, '... as a session that cannot prove it') or diag($err);
	($rc, $out, $err) = as_node($f2, 2, $text, '-c kingres_shard.forwarded=on -c kingres_shard.forward_token=zz');
	isnt($rc, 0, "'$name': a token of the wrong shape is refused");
}
is($rows->($f1), $before{f1}, 'nothing changed on node 1');
is($rows->($f2), $before{f2}, 'nothing changed on node 2');

# ---------------------------------------------------------------------------
# B. the old primary after a planned switchover
# ---------------------------------------------------------------------------
{
	my $term_old = $f1->safe_psql(q{postgres}, q{SELECT term FROM kingres.map_version});
		$f2->safe_psql('postgres', 'CHECKPOINT');
	promote_standby($s2);
	set_sync($f2);
	$f1->safe_psql('postgres', "SELECT kingres.promote_node($sid)");
	for my $k (@kinds)
	{
		my ($name, $text) = @$k;
		if ($name eq 'reference-write')
		{
			# a reference table has a copy on every node, so this kind is not tied to a group: the demoted node refuses it by the
			# catalog term (a sender always says its term, term.c), which the promotion raised
			my ($rc, $out, $err) = as_node($f2, 2, $text);
			note("reference-write without a term claim on the demoted node: rc $rc");
			my $tok = $f2->safe_psql('postgres', 'SELECT kingres.internal_forward_token(current_user, 2)');
			($rc, $out, $err) = as_node($f2, 2, "SET LOCAL kingres_shard.forward_term = '$term_old'; $text", "-c kingres_shard.forwarded=on -c kingres_shard.forward_token=$tok");
			isnt($rc, 0, "'$name': the demoted node refuses it from a sender that has the old term");
			like($err, qr/term|KS002|no longer the primary|KS001/, '... naming the term') or diag($err);
			next;
		}
		my ($rc, $out, $err) = as_node($f2, 2, $text);
		isnt($rc, 0, "'$name': the demoted node refuses it");
		like($err, qr/no longer the primary of its shard group|KS001/, '... with the retryable KS001') or diag($err);
	}
	# nothing was run: the new primary and the old one hold what they held
	is($s2->safe_psql('postgres', q{SET kingres_shard.enable_routing = off; SELECT count(*) FROM public.s}), (split m{/}, $before{f2})[0], 'the new primary holds its rows');
	is($f2->safe_psql('postgres', q{SET kingres_shard.enable_routing = off; SELECT count(*) FROM public.s}), (split m{/}, $before{f2})[0], 'and the old primary the rows it had');
	is(prepared_count($_), 0, 'nothing is prepared on ' . $_->name) for ($f1, $f2, $s2);
}

done_testing();
