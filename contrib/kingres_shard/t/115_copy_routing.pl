# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/115_copy_routing.pl
#
# Step 4 of phase 2b (design/shard-phase2b.md, 11.6 and 17): COPY ... FROM through the row router.
# These are the design's 057 and the copy part of its 058 (the numbers 056 to 069 of the design are
# taken by tests of 0.5).  Every load is made into the cluster and into a single ordinary node, and
# the tables are compared; a COPY that was not routed would leave the rows in the partitions of the
# origin only, which the placement check of every group shows.
#
#   A. formats and options, each loaded and compared (text, csv, binary, NULL, DEFAULT, HEADER, QUOTE,
#      ESCAPE, FORCE_NOT_NULL, FORCE_NULL, DELIMITER, ENCODING), column lists, omitted columns;
#   B. the sources: STDIN, a file of the server, a program;
#   C. what is refused (FREEZE, WHERE, ON_ERROR, REJECT_LIMIT, LOG_VERBOSITY, a reference table, a bucket
#      partition, the switch) and why;
#   D. errors: a bad line, a duplicate key on a group, a check: nothing is left anywhere and the error
#      names the line;
#   E. transactions, the command tag, a read-only transaction, another origin, a larger load,
#      COPY TO unchanged.

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

my $c = KingresShardCompare->new('cr');
my ($n1, $n2, $n3) = $c->nodes;
my $single = $c->single;
my $tmp = $PostgreSQL::Test::Utils::tmp_check;

sub table
{
	my ($create, $dist) = @_;
	$c->on_cluster($create . ($dist // ''));
	$c->on_single($create);
}

# The rows each node holds that belong to its group, and in all.
sub placement_ok
{
	my ($table, $name) = @_;
	my $total = 0;
	my $i = 0;
	for my $n ($n1, $n2, $n3)
	{
		my $out = $n->safe_psql('postgres', qq{
			SET kingres_shard.enable_routing = off;
			SELECT count(*) || '|' || count(*) FILTER (WHERE m.group_id = $i)
			  FROM $table k JOIN kingres.bucket_map m
			    ON m.colocation_id = (SELECT colocation_id FROM kingres.dist_table WHERE relid = '$table'::regclass)
			   AND m.bucket = kingres.bucket_for('$table'::regclass, k.id)});
		my ($all, $mine) = split /\|/, $out;
		is($mine, $all, "$name: every row of node " . ($i + 1) . " belongs to its group");
		$total += $all;
		$i++;
	}
	return $total;
}

# COPY and the command tag: psql without -q prints "COPY n".
sub copy_tag
{
	my ($node, $sql, $data) = @_;
	my ($out, $err) = ('', '');
	my $h = IPC::Run::start(
		[ 'psql', '-XAt', '-v', 'ON_ERROR_STOP=1', '-h', $node->host, '-p', $node->port, '-d', 'postgres', '-c', $sql ],
		'<', \$data, '>', \$out, '2>', \$err);
	$h->finish;
	return ($h->result(0), $out, $err);
}

sub both_copy
{
	my ($name, $sql, $data, %opt) = @_;
	my ($rc1, $o1, $e1) = copy_tag($n1, $sql, $data);
	my ($rc2, $o2, $e2) = copy_tag($single, $sql, $data);
	is($rc1, 0, "$name: the cluster loads") or diag($e1);
	is($rc2, 0, "$name: the single node loads") or diag($e2);
	is($o1, $o2, "$name: the command tag is the same ($o2)");
	return $o1;
}

table(q{CREATE TABLE public.cp (id int PRIMARY KEY, v text, n int DEFAULT 7, c int DEFAULT (3 + 4), s serial, d date, g int GENERATED ALWAYS AS (n * 2) STORED)},
	q{; SELECT kingres.create_distributed_table('public.cp', 'id', bucket_count => 12)});

# ---------------------------------------------------------------------------
# A. formats and options
# ---------------------------------------------------------------------------
both_copy('text', q{COPY public.cp (id, v, n, d) FROM STDIN},
	join('', map { "$_\tname $_\t" . ($_ * 3) . "\t2026-01-" . sprintf('%02d', 1 + $_ % 28) . "\n" } (1 .. 40)));
$c->compare_tables('public.cp', order_by => 'id', name => 'after the text format');
is(placement_ok('public.cp', 'text'), 40, 'forty rows in the groups');

both_copy('text with NULL and escapes', q{COPY public.cp (id, v, n) FROM STDIN},
	"101\ta\\tb\t\\N\n102\tback\\\\slash\t5\n103\t\\N\t6\n104\tnew\\nline\t7\n105\tdot\\\\.dot\t8\n");
both_copy('text with NULL AS', q{COPY public.cp (id, v, n) FROM STDIN (NULL 'NA')},
	"111\tNA\t1\n112\t\\N\tNA\n");
both_copy('text with DELIMITER', q{COPY public.cp (id, v, n) FROM STDIN (DELIMITER ';')}, "121;semi;1\n122;a\\;b;2\n");
$c->compare_tables('public.cp', order_by => 'id', name => 'after NULL, escapes and DELIMITER');

my $csv = qq{id,v,n\n131,"with ""quote""",1\n132,"with,comma",2\n133,"multi\nline",3\n134,,4\n135,"",5\n};
both_copy('csv with HEADER', q{COPY public.cp (id, v, n) FROM STDIN (FORMAT csv, HEADER true)}, $csv);
both_copy('csv with HEADER MATCH', q{COPY public.cp (id, v, n) FROM STDIN (FORMAT csv, HEADER match)}, qq{id,v,n\n141,x,1\n});
both_copy('csv with QUOTE and ESCAPE', q{COPY public.cp (id, v, n) FROM STDIN (FORMAT csv, QUOTE '''', ESCAPE '\\')},
	qq{151,'it\\'s',1\n152,'plain',2\n});
both_copy('csv with FORCE_NOT_NULL', q{COPY public.cp (id, v, n) FROM STDIN (FORMAT csv, FORCE_NOT_NULL (v))}, qq{161,,1\n162,x,2\n});
both_copy('csv with FORCE_NULL', q{COPY public.cp (id, v, n) FROM STDIN (FORMAT csv, FORCE_NULL (v))}, qq{171,"",1\n172,x,2\n});
both_copy('csv with a NULL string', q{COPY public.cp (id, v, n) FROM STDIN (FORMAT csv, NULL 'nil')}, qq{181,nil,1\n182,"nil",2\n});
$c->compare_tables('public.cp', order_by => 'id', name => 'after the csv options');

# DEFAULT: the marker takes the column's default; it is computed by the origin (a serial) or by the group (a constant)
both_copy('text with DEFAULT', q{COPY public.cp (id, v, n, c) FROM STDIN (DEFAULT '\D')}, "191\tx\t\\D\t\\D\n192\ty\t4\t5\n");
$c->compare_tables('public.cp', order_by => 'id', name => 'after DEFAULT');

# encodings
{
	my $latin1 = "201\tcaf\xe9\t1\n202\tna\xefve\t2\n";
	both_copy('ENCODING', q{COPY public.cp (id, v, n) FROM STDIN (ENCODING 'LATIN1')}, $latin1);
	$c->compare_tables('public.cp', order_by => 'id', name => 'after ENCODING');
}

# column lists in another order, omitted columns: a constant default (group), an expression and a serial (origin)
both_copy('a column list in another order', q{COPY public.cp (n, v, id) FROM STDIN}, "9\tr1\t211\n10\tr2\t212\n");
both_copy('omitted columns take their defaults', q{COPY public.cp (id) FROM STDIN}, "221\n222\n223\n");
both_copy('all the columns but the generated one', q{COPY public.cp FROM STDIN}, "231\tall\t1\t2\t3\t2026-02-03\n");
$c->compare_tables('public.cp', order_by => 'id', name => 'after column lists and defaults');
placement_ok('public.cp', 'after the options');

# binary: written by the single node, read by both
{
	my $bin = "$tmp/cp_binary.dat";
	unlink $bin;
	$single->safe_psql('postgres', "COPY (SELECT g AS id, 'bin' || g AS v, g % 5 AS n, date '2026-05-01' + g AS d FROM generate_series(300, 330) g) TO '$bin' (FORMAT binary)");
	my ($rc1, $o1, $e1) = run_sql($n1, "COPY public.cp (id, v, n, d) FROM '$bin' (FORMAT binary)");
	my ($rc2, $o2, $e2) = run_sql($single, "COPY public.cp (id, v, n, d) FROM '$bin' (FORMAT binary)");
	is($rc1, 0, 'a binary file loads into the cluster') or diag($e1);
	is($rc2, 0, '... and into the single node') or diag($e2);
	$c->compare_tables('public.cp', order_by => 'id', name => 'after the binary format');
	unlink $bin;
}

# ---------------------------------------------------------------------------
# B. sources: a file of the server, a program
# ---------------------------------------------------------------------------
{
	my $file = "$tmp/cp_source.tsv";
	open(my $fh, '>', $file) or die;
	print $fh "$_\tfile $_\t$_\n" for (400 .. 499);
	close $fh;
	my ($rc1, $o1, $e1) = copy_tag($n1, "COPY public.cp (id, v, n) FROM '$file'", '');
	my ($rc2, $o2, $e2) = copy_tag($single, "COPY public.cp (id, v, n) FROM '$file'", '');
	is($rc1, 0, 'a file of the server loads') or diag($e1);
	is($o1, "COPY 100\n", '... and says how many rows') or diag($o1);
	($rc1, $o1, $e1) = copy_tag($n1, "COPY public.cp (id, v, n) FROM PROGRAM 'sed s/file/prog/ $file | sed s/^4/5/'", '');
	($rc2, $o2, $e2) = copy_tag($single, "COPY public.cp (id, v, n) FROM PROGRAM 'sed s/file/prog/ $file | sed s/^4/5/'", '');
	is($rc1, 0, 'a program loads') or diag($e1);
	$c->compare_tables('public.cp', order_by => 'id', name => 'after a file and a program');
	placement_ok('public.cp', 'after the sources');
	unlink $file;
}

# ---------------------------------------------------------------------------
# C. what is refused
# ---------------------------------------------------------------------------
{
	my $before = $n1->safe_psql('postgres', 'SELECT count(*) FROM public.cp');
	my $own_bucket = $n1->safe_psql('postgres', q{SELECT min(bucket) FROM kingres.bucket_map WHERE colocation_id = 1 AND group_id = 0});
	my $own_part = sprintf('public.cp_b%04d', $own_bucket);
	for my $r (
		[ q{COPY public.cp (id, v) FROM STDIN (FREEZE)}, qr/COPY FROM with FREEZE is not supported yet for a distributed table of several shard groups/ ],
		[ q{COPY public.cp (id, v) FROM STDIN WHERE id > 1}, qr/COPY FROM with WHERE is not supported yet/ ],
		[ q{COPY public.cp (id, v) FROM STDIN (ON_ERROR ignore)}, qr/COPY FROM with ON_ERROR is not supported yet/ ],
		[ q{COPY public.cp (id, v) FROM STDIN (ON_ERROR ignore, REJECT_LIMIT 3)}, qr/COPY FROM with (ON_ERROR|REJECT_LIMIT) is not supported yet/ ],
		[ q{COPY public.cp (id, v) FROM STDIN (ON_ERROR ignore, LOG_VERBOSITY verbose)}, qr/COPY FROM with (ON_ERROR|LOG_VERBOSITY) is not supported yet/ ],
		[ q{COPY public.cp (id, g) FROM STDIN}, qr/column "g" is a generated column/ ],
		[ "COPY $own_part FROM STDIN", qr/COPY FROM would write the rows to the partitions of this node only|is not supported on/ ],
	)
	{
		my ($rc, $out, $err) = copy_tag($n1, $r->[0], "1\tx\n");
		isnt($rc, 0, "refused: $r->[0]");
		like($err, $r->[1], '... with a clear reason');
	}
	$c->on_cluster(q{CREATE TABLE public.refc (k int PRIMARY KEY); SELECT kingres.create_reference_table('public.refc')});
	my ($rc, $out, $err) = copy_tag($n1, q{COPY public.refc FROM STDIN}, "1\n");
	isnt($rc, 0, 'COPY into a reference table is still refused');
	like($err, qr/COPY FROM is not supported on|COPY FROM would write the rows to the partitions of this node only/, '... as in 2a');
	($rc, $out, $err) = copy_tag($n1, q{SET kingres_shard.enable_copy_routing = off; COPY public.cp (id, v) FROM STDIN}, "9001\tx\n");
	isnt($rc, 0, 'with enable_copy_routing off COPY is refused');
	like($err, qr/COPY FROM would write the rows to the partitions of this node only/, '... with the message of 2a');
	is($n1->safe_psql('postgres', 'SELECT count(*) FROM public.cp'), $before, 'a refused COPY loaded nothing');
}

# ---------------------------------------------------------------------------
# D. errors leave nothing behind
# ---------------------------------------------------------------------------
{
	my $before = $n1->safe_psql('postgres', 'SELECT count(*) FROM public.cp');
	my @cases = (
		[ 'a bad value on line 3', "5001\ta\t1\n5002\tb\t2\n5003\tc\tnotanumber\n5004\td\t4\n", qr/invalid input syntax for type integer: "notanumber"/, qr/COPY cp, line 3, column n/ ],
		[ 'too few columns', "5011\ta\t1\n5012\n", qr/missing data for column "v"/, qr/COPY cp, line 2/ ],
		[ 'a duplicate key on a group', join('', map { "$_\tdup\t1\n" } (6001 .. 6040)) . "1\tdup\t1\n", qr/duplicate key value violates unique constraint/, qr/bulk insert of public\.cp, batch \d+ to group \d/ ],
	);
	for my $k (@cases)
	{
		my ($name, $data, $re, $ctx) = @$k;
		my ($rc, $out, $err) = copy_tag($n1, q{COPY public.cp (id, v, n) FROM STDIN}, $data);
		isnt($rc, 0, "$name fails");
		like($err, $re, '... with the message of PostgreSQL');
		like($err, $ctx, '... and the place');
		is($n1->safe_psql('postgres', 'SELECT count(*) FROM public.cp'), $before, '... and no row of the statement is left in any group');
		is(prepared_count($_), 0, '... nor a prepared transaction on ' . $_->name) for ($n1, $n2, $n3);
	}
	my ($rc, $out, $err) = copy_tag($n1, q{COPY public.cp (id, v, n) FROM STDIN (FORMAT csv)}, qq{7001,"unterminated,1\n});
	isnt($rc, 0, 'an unterminated quote fails');
	like($err, qr/unterminated CSV quoted field/, '... as in PostgreSQL');
}

# ---------------------------------------------------------------------------
# E. transactions, tag, read-only, other origins, a larger load, COPY TO
# ---------------------------------------------------------------------------
{
	my $before = $n1->safe_psql('postgres', 'SELECT count(*) FROM public.cp');
	my ($rc, $out, $err) = run_sql($n1, qq{BEGIN; COPY public.cp (id, v) FROM STDIN;\n8001\ta\n8002\tb\n8003\tc\n\\.\nROLLBACK;});
	is($rc, 0, 'COPY in a transaction that is rolled back') or diag($err);
	is($n1->safe_psql('postgres', 'SELECT count(*) FROM public.cp'), $before, '... leaves no row');
	($rc, $out, $err) = run_sql($n1, qq{BEGIN; COPY public.cp (id, v) FROM STDIN;\n8001\ta\n8002\tb\n8003\tc\n\\.\nSAVEPOINT s; COPY public.cp (id, v) FROM STDIN;\n8101\tx\n8102\ty\n\\.\nROLLBACK TO s; COPY public.cp (id, v) FROM STDIN;\n8201\tz\n\\.\nCOMMIT;});
	is($rc, 0, 'COPY, a savepoint and COPY again') or diag($err);
	is($n1->safe_psql('postgres', 'SELECT count(*) FROM public.cp'), $before + 4, '... the rolled back COPY left no row, the others are in');
	($rc, $out, $err) = copy_tag($n1, q{BEGIN READ ONLY; COPY public.cp (id, v) FROM STDIN; COMMIT}, "9101\tx\n");
	isnt($rc, 0, 'COPY in a read-only transaction is refused');
	like($err, qr/read-only transaction/, '... and says so');
	# from the third node
	($rc, $out, $err) = copy_tag($n3, q{COPY public.cp (id, v) FROM STDIN}, join('', map { "$_\tn3\n" } (9201 .. 9260)));
	is($rc, 0, 'COPY from the third node') or diag($err);
	is($out, "COPY 60\n", '... and its tag');
	$c->on_single(q{INSERT INTO public.cp (id, v) SELECT g, 'n3' FROM generate_series(9201, 9260) g});
	$c->on_single(q{INSERT INTO public.cp (id, v) VALUES (8001, 'a'), (8002, 'b'), (8003, 'c'), (8201, 'z')});
	# a larger load with small batches
	my $data = join('', map { "$_\tbig $_\t" . ($_ % 100) . "\n" } (20000 .. 29999));
	($rc, $out, $err) = copy_tag($n1, q{SET kingres_shard.insert_batch_rows = 100; COPY public.cp (id, v, n) FROM STDIN}, $data);
	is($rc, 0, 'ten thousand rows in batches of a hundred') or diag($err);
	is($n1->safe_psql('postgres', q{SELECT count(*) FROM public.cp WHERE id >= 20000}), 10000, '... all of them are in');
	placement_ok('public.cp', 'after the larger load');
	my ($rc3, $o3, $e3) = run_sql($n2, q{SELECT count(*) FROM public.cp WHERE id BETWEEN 20000 AND 29999});
	is($o3, 10000, '... and are seen from another node');
	# COPY TO is unchanged
	my ($rs, $os) = run_sql($single, q{COPY (SELECT id, v FROM public.cp WHERE id < 100 ORDER BY id) TO STDOUT});
	my ($rd, $od) = run_sql($n1, q{COPY (SELECT id, v FROM public.cp WHERE id < 100 ORDER BY id) TO STDOUT});
	is($od, $os, 'COPY (SELECT ...) TO gives the rows of the single node');
	($rd, $od) = run_sql($n1, q{COPY public.cp (id, v) TO STDOUT});
	is(scalar(split(/\n/, $od)), $n1->safe_psql('postgres', 'SELECT count(*) FROM public.cp'), 'COPY table TO gives every row');
}

done_testing();
