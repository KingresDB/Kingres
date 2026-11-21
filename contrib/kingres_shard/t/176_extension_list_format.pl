# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/176_extension_list_format.pl
#
# Review S13F-01 and S13F-02 of the review of the fixes for step 13.
#
# S13F-02.  The marker of a shipped statement carries the extensions it uses as "name:version[,name:version...]" and every place that
# reads it splits it at ',' and at ':'.  A name or a version with either character would be cut in the wrong place (a node that has
# the extension would be reported as lacking it).  Names and versions are therefore made of letters, digits, '_', '.' and '-', and a
# list that is not of that exact form is refused, with 0A000 and not as a stale map, before it is split (shippable.c, one check used
# by the probe of the sender and by the group that receives the fragment).
#
#   A. a list of the exact form is accepted by a group (the reference);
#   B. a list that is not (a colon or a comma where it does not belong, an empty name, an empty version, a name without a version) is
#      refused by the group with 0A000 and the message of the format, for both kinds of fragment.
#
# S13F-01.  An extension at the same version but in another schema on a node would give the group's own 3F000 or 42883, not KS001.  It
# cannot happen for the extensions that kingres_shard ships: the list that is compiled in has only kingres_sketch, whose control file
# is not relocatable and names its schema, so a node cannot have it anywhere else.  The README names the limit; this test pins the
# premise, so that a change of the control file, or a second extension on the list, is noticed:
#
#   C. kingres_sketch cannot be installed in another schema, and cannot be moved there.

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(make_cluster run_sql);

my @nodes = make_cluster('xl', 3, extra => "kingres_shard.enable_partial_aggregation = on\n");
my ($n1, $n2, $n3) = @nodes;
# ---- C. the premise of the limit of S13F-01 (first, in a database of its own, before anything depends on it)
{
	$n3->safe_psql('postgres', 'CREATE DATABASE xl_other');
	my ($out, $err);
	my $rc = $n3->psql('xl_other', q{CREATE EXTENSION kingres_sketch SCHEMA public}, stdout => \$out, stderr => \$err);
	isnt($rc, 0, 'C: kingres_sketch cannot be installed in another schema');
	like($err, qr/must be installed in schema "kingres_sketch"/, 'C: it says which schema') or diag($err);
	$n3->safe_psql('xl_other', q{CREATE EXTENSION kingres_sketch});
	is($n3->safe_psql('xl_other', q{SELECT n.nspname FROM pg_extension e JOIN pg_namespace n ON n.oid = e.extnamespace WHERE e.extname = 'kingres_sketch'}),
		'kingres_sketch', 'C: it lives in the schema kingres_sketch');
	$rc = $n3->psql('xl_other', q{ALTER EXTENSION kingres_sketch SET SCHEMA public}, stdout => \$out, stderr => \$err);
	isnt($rc, 0, 'C: kingres_sketch cannot be moved to another schema');
	like($err, qr/does not support SET SCHEMA/, 'C: it says it is not relocatable') or diag($err);
}

$_->safe_psql('postgres', 'CREATE EXTENSION kingres_sketch') for @nodes;
$n1->safe_psql('postgres', q{
	CREATE TABLE public.s (id int, c int, g int, PRIMARY KEY (id, c));
	SELECT kingres.create_distributed_table('public.s', 'id', bucket_count => 12);
	INSERT INTO public.s SELECT i, c, i % 5 FROM generate_series(1, 240) i, generate_series(1, 3) c;
});
$_->safe_psql('postgres', 'ANALYZE') for @nodes;

my $K = 'kingres_sketch';
my $PLACE = $n1->safe_psql('postgres', q{SELECT kingres.internal_placement_hash(1)});

sub plan_of
{
	my ($node, $sql) = @_;
	my ($rc, $out, $err) = run_sql($node, "EXPLAIN (COSTS OFF) $sql");
	return $rc == 0 ? $out : "ERROR: $err";
}

my $as_node = sub {
	my ($node, $id, $sql) = @_;
	my $tok = $node->safe_psql('postgres', "SELECT kingres.internal_forward_token(current_user, $id)");
	local $ENV{PGOPTIONS} = "-c kingres_shard.forwarded=on -c kingres_shard.forward_token=$tok";
	return run_sql($node, "\\set VERBOSITY verbose\nBEGIN;\n$sql;\nROLLBACK;");
};
my $frag = sub {
	my ($kind, $ext, $sql) = @_;
	return "/* kingres:$kind-fragment place=$PLACE ext=$ext */ DECLARE ks_forged NO SCROLL CURSOR FOR $sql";
};

my %text;
{
	my $p = plan_of($n1, qq{SELECT g, $K.approx_count_distinct(c) FROM public.s GROUP BY g});
	($text{partial}) = $p =~ /^\s*Remote SQL: (.*)$/m;
	$p = plan_of($n1, qq{SELECT id, $K.approx_count_distinct(c), count(*) FROM public.s GROUP BY id});
	($text{query}) = $p =~ /^\s*Remote SQL: (.*)$/m;
	ok(defined $text{partial} && defined $text{query}, 'the text that the origin ships is known, for both kinds of fragment');
}

# ---- A. the exact form is accepted
for my $kind (qw(partial query))
{
	my ($rc, $out, $err) = $as_node->($n2, 2, $frag->($kind, 'kingres_sketch:1.0', $text{$kind}));
	is($rc, 0, "A: a group accepts the $kind fragment with the list of the exact form") or diag($err);
}

# ---- B. a list that is not of the exact form is refused before it is split
my @bad = (
	[ 'a colon in the version',          'kingres_sketch:1.0:2' ],
	[ 'a colon in the name',             'kingres:sketch:1.0' ],
	[ 'a name without a version',        'kingres_sketch' ],
	[ 'an empty name',                   ':1.0' ],
	[ 'an empty version',                'kingres_sketch:' ],
	[ 'a trailing comma',                'kingres_sketch:1.0,' ],
	[ 'a leading comma',                 ',kingres_sketch:1.0' ],
	[ 'an item without a version',       'kingres_sketch:1.0,other' ],
	[ 'two commas',                      'kingres_sketch:1.0,,other:1' ],
);
for my $kind (qw(partial query))
{
	for my $b (@bad)
	{
		my ($what, $list) = @$b;
		my ($rc, $out, $err) = $as_node->($n2, 2, $frag->($kind, $list, $text{$kind}));
		isnt($rc, 0, "B: $kind, $what: refused");
		like($err, qr/0A000/, "B: $kind, $what: with 0A000, a repeat gives the same answer") or diag($err);
		like($err, qr/is not of the form name:version/, "B: $kind, $what: and the message says the form") or diag($err);
		unlike($err, qr/KS001/, "B: $kind, $what: it is not a stale map");
	}
}

done_testing();
