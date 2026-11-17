# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# The foundation of phase 2b (design/shard-phase2b.md, 4.7 and 2.3), checked in
# the source and against a server:
#
#   1. one place computes a bucket: no file but hashing.c hashes a key or takes
#      the remainder of a hash, and bucket_count and bucket_group are read only
#      where the design allows; the check itself is tested with snippets that it
#      must refuse;
#   2. one place knows the version of PostgreSQL: pg_compat.h; it defines every
#      name that 2.3 of the design lists, and the settings it lists for a
#      partial fragment exist on the server this test runs against (and
#      enable_eager_aggregate exists exactly on 19).
#
# What the bucket of every value is, is tested by the regression test
# bucket_lookup (its expected file was recorded before the lookup moved).

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;

my $src = "$FindBin::RealBin/../src";

sub slurp
{
	my ($path) = @_;
	open(my $fh, '<', $path) or die "could not open $path: $!";
	local $/;
	my $text = <$fh>;
	close($fh);
	return $text;
}

# The code of a C file without its comments, strings and character constants.
sub code_of
{
	my ($text) = @_;
	$text =~ s{/\*.*?\*/|//[^\n]*|"(?:\\.|[^"\\\n])*"|'(?:\\.|[^'\\\n])+'}{ }gs;
	return $text;
}

# The files that may read the fields of a colocation group (bucket_count: the
# loading of the catalog, the bucket function, the creation of the partitions
# of a table and of a moved bucket; bucket_group: only the loading and the
# accessors, which are in catalog.c).
my $COUNT_OK = qr/^(?:catalog|hashing|dist_table|bucket_move)\.[ch]$/;
my $GROUP_OK = qr/^catalog\.[ch]$/;
my $HASH_OK = qr/^(?:hashing|dist_table)\.[ch]$/;

# The problems of a set of files (name => text): a list of strings.
sub bucket_lookup_problems
{
	my (%files) = @_;
	my @problems;
	for my $f (sort keys %files)
	{
		my $code = code_of($files{$f});
		push @problems, "$f reads bucket_count"
		  if $f !~ $COUNT_OK && $code =~ /\bbucket_count\b/;
		push @problems, "$f reads bucket_group"
		  if $f !~ $GROUP_OK && $code =~ /(?:->|\.)\s*bucket_group\b/;
		push @problems, "$f computes a remainder of a hash"
		  if $f !~ $HASH_OK
		  && ($code =~ /\b\w*hash\w*\s*%(?!=)/
			|| $code =~ /[^%]%\s*(?:\(\s*u?int(?:64)?\s*\)\s*)?[\w>.-]*(?:bucket_count|nbuckets)\b/);
		push @problems, "$f calls the hash function of a key itself"
		  if $f !~ $HASH_OK
		  && $code =~ /\b(?:compute_partition_hash_value|HASHEXTENDED_PROC|hash_extended_proc)\b/;
		push @problems, "$f tests the version of PostgreSQL"
		  if $f !~ /^pg_compat\.h$/ && $code =~ /\bPG_VERSION_NUM\b/;
	}
	return @problems;
}

# ---------------------------------------------------------------------------
# 1. The source: nothing but the lookup functions computes a bucket.
# ---------------------------------------------------------------------------
my %real;
opendir(my $dh, $src) or die "could not open $src: $!";
for my $f (sort grep { /\.[ch]$/ } readdir($dh))
{
	$real{$f} = slurp("$src/$f");
}
closedir($dh);
cmp_ok(scalar(keys %real), '>', 20, 'the sources of the extension are found');
is(join('; ', bucket_lookup_problems(%real)), '', 'no file but the lookup computes a bucket or tests the version');

# The check itself: each of these must be refused (a check that never fails is
# no check), and the allowed forms must not be.
my @refused = (
	[ 'planner.c', 'b = (int) (hash % (uint64) coloc->bucket_count);', qr/computes a remainder|reads bucket_count/ ],
	[ 'dist_scan.c', 'for (b = 0; b < dt->coloc->bucket_count; b++)', qr/reads bucket_count/ ],
	[ 'fanout.c', 'x = h % nbuckets;', qr/computes a remainder/ ],
	[ 'fanout.c', 'x = key_hash % 16;', qr/computes a remainder/ ],
	[ 'fanout.c', 'x = h % (uint64) n_buckets_total + (long) bucket_count;', qr/reads bucket_count/ ],
	[ 'planner.c', 'g = coloc->bucket_group[b];', qr/reads bucket_group/ ],
	[ 'shippable.c', 'g = c.bucket_group[0];', qr/reads bucket_group/ ],
	[ 'remote_scan.c', 'h = compute_partition_hash_value(1, f, &c, &v, &n);', qr/calls the hash function/ ],
	[ 'remote_scan.c', 'p = get_opfamily_proc(f, t, t, HASHEXTENDED_PROC);', qr/calls the hash function/ ],
	[ 'planner.c', "#if PG_VERSION_NUM >= 190000\nint x;\n#endif", qr/tests the version/ ],
	[ 'catalog.h', "#if PG_VERSION_NUM < 180000\n#endif", qr/tests the version/ ],
);
for my $r (@refused)
{
	my ($file, $text, $re) = @$r;
	like(join('; ', bucket_lookup_problems($file => $text)), $re, "the check refuses: $text");
}
my @allowed = (
	[ 'catalog.c', 'c->bucket_group[b] = -1; n = c->bucket_count;' ],
	[ 'hashing.c', 'return (int) (hash % (uint64) coloc->bucket_count);' ],
	[ 'dist_table.c', 'tce = lookup_type_cache(t, TYPECACHE_HASH_EXTENDED_PROC); x = hash_extended_proc;' ],
	[ 'bucket_move.c', 'int bucket_count; x = bucket_count;' ],
	[ 'planner.c', '/* hash % bucket_count in a comment */ x = 1;' ],
	[ 'planner.c', 'errmsg("bucket_count %d and hash %% 7 in a string", n);' ],
	[ 'planner.c', 'x = n % 2; y = kingres_shard_bucket_group(coloc, b);' ],
	[ 'planner.c', 'g = kingres_shard_coloc_nbuckets(coloc);' ],
	[ 'pg_compat.h', "#if PG_VERSION_NUM >= 190000\n#endif" ],
);
for my $a (@allowed)
{
	my ($file, $text) = @$a;
	is(join('; ', bucket_lookup_problems($file => $text)), '', "the check allows: $text");
}

# The declarations of the design (4.7).
my $hashing_h = slurp("$src/hashing.h");
like($hashing_h, qr/kingres_shard_hash_of_value\(Oid column_type, Oid collation,\s+Oid value_type, Datum value,\s+bool isnull, uint64 \*hash\)/,
	'hashing.h declares kingres_shard_hash_of_value as in the design');
like($hashing_h, qr/kingres_shard_bucket_for_hash\(const KsColocation \*coloc,\s+uint64 hash\)/,
	'hashing.h declares kingres_shard_bucket_for_hash as in the design');
like($hashing_h, qr/kingres_shard_try_bucket\(Oid column_type, Oid collation,\s+const KsColocation \*coloc/,
	'kingres_shard_try_bucket takes the colocation group');
like($hashing_h, qr/kingres_shard_bucket\(Oid column_type, Oid collation,\s+const KsColocation \*coloc/,
	'kingres_shard_bucket takes the colocation group');
my $catalog_h = slurp("$src/catalog.h");
like($catalog_h, qr/kingres_shard_coloc_nbuckets\(const KsColocation \*coloc\)/, 'catalog.h declares the number of buckets');
like($catalog_h, qr/kingres_shard_coloc_bucket\(const KsColocation \*coloc, int i\)/, 'catalog.h declares the i-th bucket');
like($catalog_h, qr/kingres_shard_bucket_group\(const KsColocation \*coloc, int bucket\)/, 'catalog.h declares the group of a bucket');

# ---------------------------------------------------------------------------
# 2. pg_compat.h defines what 2.3 of the design lists, and what it lists for a
#    partial fragment exists on this server.
# ---------------------------------------------------------------------------
my $compat = slurp("$src/pg_compat.h");
for my $name (qw(KS_SERVER_MAJOR KS_TABLE_BEGINSCAN KS_T_CLUSTER_STMT ks_cluster_stmt_relation
	KS_PLANNER_ES_PARAM KS_PLANNER_ES_ARG KS_CLEAR_RESULT_RELATIONS ks_result_relations
	KS_LIBPQ_RESULTS_OWNED_BY_CONTEXT ks_onconflict_action_shippable KS_ONCONFLICT_IS_SELECT
	KS_IS_GROUPED_REL KS_PARTIAL_PLAN_GUCS KS_N_PARTIAL_PLAN_GUCS ks_result_adopt
	ks_window_ignore_nulls KS_SET_PLAN_ORIGIN))
{
	like($compat, qr/\b\Q$name\E\b/, "pg_compat.h defines $name");
}

my ($list19, $list18) = $compat =~
  /#if PG_VERSION_NUM >= 190000\n#define KS_PARTIAL_PLAN_GUCS\s*\\\n(.*?)\n#else\n#define KS_PARTIAL_PLAN_GUCS\s*\\\n(.*?)\n#endif/s;
ok(defined $list19 && defined $list18, 'pg_compat.h has the settings of a partial fragment for 19 and for 18');
my @gucs19 = $list19 =~ /\{"([a-z_]+)",\s*"([^"]*)"\}/g;
my @gucs18 = $list18 =~ /\{"([a-z_]+)",\s*"([^"]*)"\}/g;
is(scalar(@gucs18), 4, 'the list for 18 has two settings');
is(scalar(@gucs19), 6, 'the list for 19 has three settings');
is(join(',', @gucs19[ 0 .. 3 ]), join(',', @gucs18), 'the list for 19 is the list for 18 and one more');
is($gucs19[4], 'enable_eager_aggregate', 'the one more is eager aggregation');

my $node = PostgreSQL::Test::Cluster->new('fnd');
$node->init;
$node->start;
my $major = int($node->safe_psql('postgres', 'SHOW server_version_num') / 10000);
my @gucs = $major >= 19 ? @gucs19 : @gucs18;
while (my ($name, $value) = splice(@gucs, 0, 2))
{
	is($node->safe_psql('postgres', "SET $name = '$value'; SHOW $name"), $value,
		"the setting $name = $value of a partial fragment exists on PostgreSQL $major");
}
is($node->safe_psql('postgres', q{SELECT count(*) FROM pg_settings WHERE name = 'enable_eager_aggregate'}),
	($major >= 19 ? '1' : '0'), "enable_eager_aggregate exists exactly on 19 (this is $major)");
$node->stop;

done_testing();
