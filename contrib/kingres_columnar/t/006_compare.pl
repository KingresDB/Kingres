# contrib/kingres_columnar/t/006_compare.pl
#
# Differential test: the same random workload is applied to a heap table and
# to a columnar table, and the answers of the two are compared again and
# again.  Heap is the reference: every difference is a bug of the columnar
# access method (or of the reference, which is much less likely).
#
# Every step is one psql run that applies the same statements to both tables
# in the same transaction, so the semantics are identical, and then queries
# both.  A step is chosen at random from: a single-row insert, a multi-row
# VALUES insert, INSERT .. SELECT from generate_series, INSERT .. SELECT from
# the table itself, COPY FROM STDIN, a transaction that inserts and commits or
# rolls back, nested savepoints (SAVEPOINT, ROLLBACK TO, RELEASE) around
# inserts, TRUNCATE, VACUUM, VACUUM FULL, ANALYZE, columnar_compact(),
# ALTER TABLE ADD COLUMN (with and without a default) and DROP COLUMN, a
# change of the options of the columnar table (codec and level, small stripes
# and chunks so that many stripes exist), CHECKPOINT, and now and then a
# crash-style restart of the server (immediate stop) or a clean one.
#
# After every step the two tables are compared: the row count, and a number
# of random predicates (ranges and equality on int and timestamp columns,
# IS NULL, text equality and prefix, = ANY, OR/AND combinations, boolean,
# numeric, float with NaN and infinities, array and jsonb conditions), each
# giving count, sums, minima and maxima and an md5 of all selected rows in id
# order.  Each predicate runs three ways on the columnar table: with the
# Columnar Scan (the default), with kingres_columnar.enable_custom_scan off, and
# with kingres_columnar.enable_chunk_skipping off.  Inside an open
# transaction the comparison runs before COMMIT or ROLLBACK, so the
# transaction's own rows are compared too.
#
# Values are generated in SQL from the row id and a salt chosen by the
# test, so they are random-looking but the same for both tables.  TRUNCATE
# is applied to both tables; when the columnar table refuses it ("cannot
# truncate columnar table ... in a subtransaction after rows were inserted
# in an outer one", by design) the subtransaction of that step is rolled
# back on both and the run continues.
#
# Environment:
#   KC_COMPARE_SEED    the seed (default: taken from the time and printed; a
#                      failure prints it again so the run can be repeated)
#   KC_COMPARE_STEPS   the number of steps (default 3000)
#   KC_COMPARE_VERBOSE print every step
#
# Portions Copyright (c) 2026, RooyeKhat Media Co
# SPDX-License-Identifier: AGPL-3.0-only

use strict;
use warnings FATAL => 'all';

use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;

my $seed = defined $ENV{KC_COMPARE_SEED} ? $ENV{KC_COMPARE_SEED} : (time() ^ ($$ << 8)) & 0x7FFFFFFF;
my $steps = $ENV{KC_COMPARE_STEPS} // 3000;
srand($seed);
diag("KC_COMPARE_SEED=$seed KC_COMPARE_STEPS=$steps");

my $node = PostgreSQL::Test::Cluster->new('compare');
$node->init;
$node->append_conf('postgresql.conf', <<'EOC');
shared_preload_libraries = 'kingres_columnar'
fsync = off
autovacuum = off
max_parallel_workers_per_gather = 0
checkpoint_timeout = 30min
log_statement = none
log_min_messages = warning
EOC
$node->start;
$node->safe_psql('postgres', 'CREATE EXTENSION kingres_columnar');

# ---------------------------------------------------------------------------
# The schema and the value generators
# ---------------------------------------------------------------------------

# name => [ sql type, nullable-default-clause ]
my @cols = qw(id a ts s sc n b arr j f);
my %type = (
	id => 'bigint', a => 'int', ts => 'timestamp', s => 'text',
	sc => 'text COLLATE "C"', n => 'numeric', b => 'boolean',
	arr => 'int[]', j => 'jsonb', f => 'float8',
);
my $next_col = 1;                        # for ADD COLUMN names
my $next_id  = 1;                        # the next row id (perl side counter)
my $cur_rows = 0;                        # row count seen at the last comparison

$node->safe_psql('postgres', 'CREATE TABLE h (' . join(', ', map { "$_ $type{$_}" } @cols) . ') USING heap');
$node->safe_psql('postgres', 'CREATE TABLE c (' . join(', ', map { "$_ $type{$_}" } @cols) . ') USING columnar');
$node->safe_psql('postgres',
	q{SELECT kingres.columnar_set_options('c', stripe_row_count => 1000, chunk_row_count => 100)});

# The expressions of the generated columns.  $G is the SQL for the row id,
# $S the salt, $mode 'rnd' or 'ord' (ordered timestamps and ids).  H and H2 are
# two 28-bit hashes of the id and the salt, never negative.
sub gen_expr
{
	my ($col, $G, $S, $mode, $R) = @_;
	my $H  = "((('x' || substr(md5(($G)::text || '#$S'), 1, 7))::bit(28))::int)";
	my $H2 = "((('x' || substr(md5(($G)::text || '~$S'), 1, 7))::bit(28))::int)";
	return $G if $col eq 'id';
	return "CASE WHEN $H % 13 = 0 THEN NULL ELSE $H2 % $R END" if $col eq 'a';
	if ($col eq 'ts')
	{
		return $mode eq 'ord'
		  ? "CASE WHEN $H % 29 = 0 THEN NULL ELSE timestamp '2020-01-01' + ($G) * interval '7 second' END"
		  : "CASE WHEN $H % 17 = 0 THEN NULL ELSE timestamp '2020-01-01' + ($H2 % 1000000) * interval '1 second' END";
	}
	return "CASE $H % 9 WHEN 0 THEN NULL WHEN 1 THEN '' WHEN 2 THEN 'k' || ($H2 % 50) "
	  . "WHEN 3 THEN repeat(md5(($G)::text), 1 + $H2 % 4) WHEN 4 THEN repeat('ab', $H2 % 200) "
	  . "ELSE 'k' || ($H2 % 50) || '-' || substr(md5(($H2)::text), 1, $H2 % 12) END" if $col eq 's';
	return "CASE WHEN $H % 11 = 0 THEN NULL ELSE 'k' || lpad(($H2 % 50)::text, 2, '0') END" if $col eq 'sc';
	return "CASE WHEN $H % 7 = 0 THEN NULL WHEN $H % 211 = 0 THEN 'NaN'::numeric ELSE ($H2 % 100000)::numeric / 100 END" if $col eq 'n';
	return "CASE WHEN $H % 5 = 0 THEN NULL ELSE $H2 % 2 = 0 END" if $col eq 'b';
	return "CASE $H % 6 WHEN 0 THEN NULL WHEN 1 THEN '{}'::int[] ELSE ARRAY[$H2 % 7, $H2 % 11, $H2 % 13] END" if $col eq 'arr';
	return "CASE $H % 8 WHEN 0 THEN NULL WHEN 1 THEN '{}'::jsonb ELSE jsonb_build_object('k', $H2 % 10, 's', substr(md5(($H2)::text), 1, 5), 'l', jsonb_build_array($H2 % 3, $H2 % 5)) END" if $col eq 'j';
	return "CASE WHEN $H % 19 = 0 THEN NULL WHEN $H % 101 = 0 THEN 'NaN'::float8 WHEN $H % 103 = 0 THEN '-Infinity'::float8 WHEN $H % 107 = 0 THEN 'Infinity'::float8 ELSE ($H2 % 100000) / 7.0 END" if $col eq 'f';
	# added columns: xi int, xt text, xn numeric, xd timestamp, xb boolean
	my $t = $type{$col} // '';
	return "CASE WHEN $H % 5 = 0 THEN NULL ELSE $H2 % 100 END" if $t =~ /^int/;
	return "CASE WHEN $H % 6 = 0 THEN NULL ELSE 'e' || ($H2 % 20) END" if $t =~ /^text/;
	return "CASE WHEN $H % 6 = 0 THEN NULL ELSE ($H2 % 1000)::numeric / 10 END" if $t =~ /^numeric/;
	return "CASE WHEN $H % 6 = 0 THEN NULL ELSE timestamp '2021-06-01' + ($H2 % 100000) * interval '1 minute' END" if $t =~ /^timestamp/;
	return "CASE WHEN $H % 6 = 0 THEN NULL ELSE $H2 % 2 = 1 END";
}

# an INSERT statement template (%T is the table) for rows ids $from..$to
sub insert_gs
{
	my ($from, $to, $mode) = @_;
	my $S = int(rand(1e6));
	my $R = (10, 1000, 100000)[ int(rand(3)) ];
	my $list = join(', ', @cols);
	my $exprs = join(', ', map { gen_expr($_, 'g', $S, $mode, $R) } @cols);
	return "INSERT INTO %T ($list) SELECT $exprs FROM generate_series($from, $to) g";
}

sub insert_values
{
	my ($from, $to, $mode) = @_;
	my $S = int(rand(1e6));
	my $R = (10, 1000, 100000)[ int(rand(3)) ];
	my $list = join(', ', @cols);
	my @rows;
	for my $id ($from .. $to)
	{
		push @rows, '(' . join(', ', map { gen_expr($_, "$id", $S, $mode, $R) } @cols) . ')';
	}
	return "INSERT INTO %T ($list) VALUES " . join(', ', @rows);
}

# rows copied from the table itself, with fresh ids; ties are impossible
# because the ids are unique, so the choice of rows is the same on both tables
sub insert_self
{
	my ($from, $k) = @_;
	my $list = join(', ', @cols);
	my $sel = join(', ', map { $_ eq 'id' ? "$from + row_number() OVER (ORDER BY x.id) - 1" : "x.$_" } @cols);
	return "INSERT INTO %T ($list) SELECT $sel FROM (SELECT * FROM %T ORDER BY id LIMIT $k) x";
}

# COPY data for rows from..to, produced by the server in COPY text format
sub copy_data
{
	my ($from, $to) = @_;
	my $S = int(rand(1e6));
	my $list = join(', ', @cols);
	my $exprs = join(', ', map { gen_expr($_, 'g', $S, 'rnd', 1000) } @cols);
	return $node->safe_psql('postgres',
		"COPY (SELECT $exprs FROM generate_series($from, $to) g) TO STDOUT");
}

# ---------------------------------------------------------------------------
# Predicates and the comparison queries
# ---------------------------------------------------------------------------
sub has { my $c = shift; return scalar grep { $_ eq $c } @cols; }

sub rnd_ts
{
	return sprintf("timestamp '2020-01-01' + interval '%d seconds'", int(rand(1000000)));
}

sub one_cond
{
	my @opts;
	push @opts, sub { my ($x, $y) = sort { $a <=> $b } (1 + int(rand($next_id + 5)), 1 + int(rand($next_id + 5))); "id BETWEEN $x AND $y" };
	push @opts, sub { 'id < ' . (1 + int(rand($next_id + 5))) };
	push @opts, sub { 'id = ' . (1 + int(rand($next_id + 5))) };
	if (has('a'))
	{
		push @opts, sub { 'a = ' . int(rand(1000)) };
		push @opts, sub { 'a < ' . int(rand(1000)) };
		push @opts, sub { my ($x, $y) = sort { $a <=> $b } (int(rand(1000)), int(rand(1000))); "a BETWEEN $x AND $y" };
		push @opts, sub { 'a = ANY (ARRAY[' . join(',', map { int(rand(1000)) } 1 .. 1 + int(rand(4))) . '])' };
		push @opts, sub { 'a IS NULL' };
		push @opts, sub { 'a IS NOT NULL AND a > ' . int(rand(100000)) };
	}
	if (has('ts'))
	{
		push @opts, sub { 'ts >= ' . rnd_ts() };
		push @opts, sub { 'ts < ' . rnd_ts() };
		push @opts, sub { 'ts = ' . rnd_ts() };
		push @opts, sub { 'ts BETWEEN ' . rnd_ts() . ' AND ' . rnd_ts() };
		push @opts, sub { 'ts IS NULL' };
	}
	if (has('s'))
	{
		push @opts, sub { "s = 'k" . int(rand(50)) . "'" };
		push @opts, sub { "s LIKE 'k" . int(rand(5)) . "%'" };
		push @opts, sub { "s = ''" };
		push @opts, sub { 's IS NULL' };
		push @opts, sub { "s > 'k3'" };
	}
	if (has('sc'))
	{
		push @opts, sub { "sc = 'k" . sprintf('%02d', int(rand(50))) . "'" };
		push @opts, sub { "sc >= 'k" . sprintf('%02d', int(rand(50))) . "'" };
		push @opts, sub { "sc LIKE 'k" . int(rand(5)) . "%'" };
		push @opts, sub { "sc = ANY (ARRAY['k01', 'k17', 'k" . sprintf('%02d', int(rand(50))) . "'])" };
		push @opts, sub { "sc IS NOT NULL AND sc < 'k" . sprintf('%02d', int(rand(50))) . "'" };
	}
	if (has('b'))
	{
		push @opts, sub { ('b', 'NOT b', 'b IS NULL', 'b IS NOT TRUE')[int(rand(4))] };
	}
	if (has('n'))
	{
		push @opts, sub { 'n > ' . int(rand(1000)) };
		push @opts, sub { 'n = ' . int(rand(1000)) . '.5' };
		push @opts, sub { "n = 'NaN'" };
	}
	if (has('f'))
	{
		push @opts, sub { 'f < ' . int(rand(15000)) };
		push @opts, sub { 'f >= ' . int(rand(15000)) };
		push @opts, sub { "f = 'NaN'" };
		push @opts, sub { "f > 'Infinity'" };
		push @opts, sub { "f = '-Infinity'" };
	}
	if (has('arr'))
	{
		push @opts, sub { 'arr @> ARRAY[' . int(rand(7)) . ']' };
		push @opts, sub { "arr = '{}'" };
		push @opts, sub { 'arr IS NULL' };
	}
	if (has('j'))
	{
		push @opts, sub { "j->>'k' = '" . int(rand(10)) . "'" };
		push @opts, sub { 'j @> \'{"k": ' . int(rand(10)) . '}\'' };
	}
	for my $x (grep { /^x/ } @cols)
	{
		my $t = $type{$x};
		push @opts, sub { "$x IS NULL" };
		push @opts, sub { "$x = " . int(rand(100)) } if $t =~ /^int/;
		push @opts, sub { "$x = 'e" . int(rand(20)) . "'" } if $t =~ /^text/;
		push @opts, sub { "$x > " . int(rand(100)) } if $t =~ /^numeric/;
		push @opts, sub { "$x < timestamp '2021-06-0" . (1 + int(rand(9))) . "'" } if $t =~ /^timestamp/;
	}
	return $opts[ int(rand(@opts)) ]->();
}

sub predicate
{
	my $n = 1 + int(rand(3));
	my $p = one_cond();
	for (2 .. $n)
	{
		my $op = rand() < 0.5 ? 'AND' : 'OR';
		my $c = one_cond();
		$p = rand() < 0.3 ? "($p) $op $c" : "$p $op $c";
	}
	return rand() < 0.1 ? "NOT ($p)" : $p;
}

# the aggregates of one comparison query: only columns that exist
sub aggs
{
	my @a = ('count(*)');
	push @a, 'sum(a)' if has('a');
	push @a, 'min(ts)', 'max(ts)' if has('ts');
	push @a, 'min(sc)', 'max(sc)' if has('sc');
	push @a, 'sum(n) FILTER (WHERE n <> \'NaN\')' if has('n');
	push @a, 'min(f)', 'max(f)' if has('f');
	push @a, "md5(string_agg(x::text, '|' ORDER BY x.id))";
	return join(', ', @a);
}

my @VARIANTS = (
	[ 'scan',    '' ],
	[ 'nocs',    'SET kingres_columnar.enable_custom_scan = off;' ],
	[ 'noskip',  'SET kingres_columnar.enable_chunk_skipping = off;' ],
	# forced parallel plans: no path over a columnar table may run in a worker
	[ 'par',     'SET debug_parallel_query = on; SET parallel_setup_cost = 0; SET parallel_tuple_cost = 0; '
	  . 'SET min_parallel_table_scan_size = 0; SET max_parallel_workers_per_gather = 2;' ],
);
my $qid = 0;
my %QTEXT;

# comparison queries: a count, and $np predicates in three variants
sub compare_sql
{
	my ($np, $full) = @_;
	my $out = '';
	my $agg = aggs();
	my @queries = ("SELECT count(*) FROM %T");
	push @queries, "SELECT $agg FROM %T x" if $full;
	push @queries, "SELECT $agg FROM %T x WHERE " . predicate() for 1 .. $np;
	for my $q (@queries)
	{
		my $id = ++$qid;
		# the plain count in one variant is enough; predicates in all three
		for my $v ($q =~ /^SELECT count/ ? $VARIANTS[0] : @VARIANTS)
		{
			(my $qh = $q) =~ s/%T/h/g;
			(my $qc = $q) =~ s/%T/c/g;
			$out .= "\\echo \@\@Q $id h $v->[0]\n$qh;\n";
			$out .= "$v->[1]\n" if $v->[1];
			$out .= "\\echo \@\@Q $id c $v->[0]\n$qc;\n";
			$out .= "RESET kingres_columnar.enable_custom_scan;\nRESET kingres_columnar.enable_chunk_skipping;\n"
			  . "RESET debug_parallel_query;\nRESET parallel_setup_cost;\nRESET parallel_tuple_cost;\n"
			  . "RESET min_parallel_table_scan_size;\nRESET max_parallel_workers_per_gather;\n" if $v->[1];
			$out .= "\\echo \@\@E $id\n";
			$QTEXT{$id} = $q;   # for the failure report
		}
	}
	return $out;
}

# ---------------------------------------------------------------------------
# Steps
# ---------------------------------------------------------------------------

# apply a template to both tables
sub both
{
	my ($t) = @_;
	(my $h = $t) =~ s/%T/h/g;
	(my $c = $t) =~ s/%T/c/g;
	return "$h;\n$c;\n";
}

my @codecs = qw(none pglz lz4 zstd);
my @log;                               # the last steps, for a failure report
my %stat;                              # how often each step kind ran
my $refusals = 0;
my $restarts = 0;
my $compared = 0;

# One random insert statement (template), consuming ids
sub rnd_insert
{
	my $r = rand();
	my $mode = rand() < 0.4 ? 'ord' : 'rnd';
	my $stmt;
	if ($r < 0.15 && $next_id > 1)
	{
		my $k = 1 + int(rand(200));
		$stmt = insert_self($next_id, $k);
		$next_id += $k;
	}
	elsif ($r < 0.35)
	{
		my $k = 1 + int(rand(20));
		$stmt = insert_values($next_id, $next_id + $k - 1, $mode);
		$next_id += $k;
	}
	elsif ($r < 0.45)
	{
		$stmt = insert_values($next_id, $next_id, $mode);
		$next_id += 1;
	}
	else
	{
		my $k = (1, 5, 50, 300, 1200, 2600)[ int(rand(6)) ] + int(rand(40));
		$stmt = insert_gs($next_id, $next_id + $k - 1, $mode);
		$next_id += $k;
	}
	return $stmt;
}

# the script of one step: returns (script, description)
sub make_step
{
	my ($kind) = @_;
	my $s = '';
	my $d = $kind;
	if ($kind eq 'insert')
	{
		$s .= both(rnd_insert());
	}
	elsif ($kind eq 'copy')
	{
		my $k = 1 + int(rand(600));
		my $data = copy_data($next_id, $next_id + $k - 1);
		$next_id += $k;
		my $list = join(', ', @cols);
		for my $t (qw(h c))
		{
			$s .= "COPY $t ($list) FROM STDIN;\n$data\n\\.\n";
		}
		$d = "copy $k rows";
	}
	elsif ($kind eq 'txn')
	{
		$s .= "BEGIN;\n";
		my $inserted = 0;
		my $depth = 0;
		my $nops = 1 + int(rand(6));
		my $nsp = 0;
		my @stack;
		my @cursors;			# open cursors: { n, seq }
		my %spseq;				# savepoint number => sequence number
		my $ncur = 0;
		my $had_cursor = 0;		# a cursor was declared: no TRUNCATE or rewrite after it
		my $seq = 0;
		for my $i (1 .. $nops)
		{
			my $r = rand();
			if ($r < 0.45)
			{
				$s .= both(rnd_insert());
				$inserted = 1;
			}
			elsif ($r < 0.65)
			{
				my $n = ++$nsp;
				$s .= "SAVEPOINT sp$n;\n";
				push @stack, $n;
				$spseq{$n} = ++$seq;
			}
			elsif ($r < 0.8 && @stack)
			{
				$s .= "ROLLBACK TO sp$stack[-1];\n";
				# the rollback closes the cursors declared after the savepoint
				@cursors = grep { $_->{seq} < $spseq{ $stack[-1] } } @cursors;
			}
			elsif ($r < 0.9 && @stack)
			{
				$s .= "RELEASE sp$stack[-1];\n";
				pop @stack;
			}
			elsif ($r < 0.85)
			{
				$s .= compare_sql(1, 0);
			}
			elsif ($r < 0.89 && !$had_cursor)
			{
				# a rewrite of the table while rows are buffered
				$s .= rewrite_step();
				$stat{'rewrite in txn'}++;
			}
			elsif ($r < 0.915)
			{
				# a cursor over the table, fetched later in the transaction
				my $n = ++$ncur;
				$had_cursor = 1;
				my $pred = predicate();
				$s .= "DECLARE ch$n CURSOR FOR SELECT id FROM h WHERE $pred ORDER BY id;\n";
				$s .= "DECLARE cc$n CURSOR FOR SELECT id FROM c WHERE $pred ORDER BY id;\n";
				push @cursors, { n => $n, seq => ++$seq };
				$stat{'cursor'}++;
			}
			elsif ($r < 0.97 && @cursors)
			{
				# fetch a cursor: it must have kept every row it was entitled to
				# across RELEASE, ROLLBACK TO and flushes caused by size
				my $cu = splice(@cursors, int(rand(@cursors)), 1);
				my $id = ++$qid;
				$s .= "\\echo \@\@Q $id h scan\nFETCH ALL FROM ch$cu->{n};\n"
				  . "\\echo \@\@Q $id c scan\nFETCH ALL FROM cc$cu->{n};\n\\echo \@\@E $id\n";
				$QTEXT{$id} = "cursor $cu->{n}";
				$stat{'cursor fetch'}++;
			}
			else
			{
				# TRUNCATE inside a transaction.  If rows were inserted before
				# (in this transaction), the columnar table may refuse it
				# inside a subtransaction; guard it and roll back to the guard.
				if ($had_cursor)
				{
					$s .= compare_sql(1, 0);	# no TRUNCATE under an open cursor
				}
				elsif ($inserted || @stack)
				{
					$s .= "SAVEPOINT trg;\nTRUNCATE h;\nTRUNCATE c;\n\\if :ERROR\nROLLBACK TO trg;\n\\endif\nRELEASE trg;\n";
				}
				else
				{
					$s .= "TRUNCATE h;\nTRUNCATE c;\n";
				}
				$stat{'truncate in txn'}++;
			}
		}
		$s .= compare_sql(1, 0);
		my $end = rand() < 0.5 ? 'COMMIT' : 'ROLLBACK';
		$s .= "$end;\n";
		$d = "txn ($nops ops, $end)";
	}
	elsif ($kind eq 'rewrite')
	{
		$s .= "BEGIN;\n" . both(rnd_insert());
		$s .= "SAVEPOINT rs;\n" . rewrite_step() . compare_sql(1, 0);
		$s .= both(rnd_insert()) . "ROLLBACK TO rs;\n" . compare_sql(1, 0);
		$s .= rewrite_step() . compare_sql(1, 0);
		$s .= (rand() < 0.7 ? 'COMMIT' : 'ROLLBACK') . ";\n";
	}
	elsif ($kind eq 'truncate')
	{
		$s .= "BEGIN;\nTRUNCATE h;\nTRUNCATE c;\n";
		if (rand() < 0.5)
		{
			$s .= both(rnd_insert());
		}
		$s .= compare_sql(1, 0);
		$s .= (rand() < 0.8 ? 'COMMIT' : 'ROLLBACK') . ";\n";
	}
	elsif ($kind eq 'vacuum')
	{
		$s .= both('VACUUM %T');
	}
	elsif ($kind eq 'vacuum full')
	{
		$s .= both('VACUUM FULL %T');
	}
	elsif ($kind eq 'analyze')
	{
		$s .= both('ANALYZE %T');
	}
	elsif ($kind eq 'compact')
	{
		$s .= "SELECT kingres.columnar_compact('c');\n";
		$s .= "SELECT kingres.columnar_compact('c', " . (1 + int(rand(2000))) . ");\n" if rand() < 0.5;
	}
	elsif ($kind eq 'add column' && @cols >= 24)
	{
		# a table with hundreds of columns only slows the run down; past 24
		# columns the step drops an added column instead
		my @x = grep { /^x/ } @cols;
		return () unless @x;
		my $name = $x[ int(rand(@x)) ];
		$s .= both("ALTER TABLE %T DROP COLUMN $name");
		@cols = grep { $_ ne $name } @cols;
		$d = "drop column $name";
	}
	elsif ($kind eq 'add column')
	{
		my $name = 'x' . $next_col++;
		my $t = (qw(int text numeric timestamp boolean))[ int(rand(5)) ];
		my %defaults = (int => '42', text => "'dflt'", numeric => '1.5', timestamp => "'2024-01-01'", boolean => 'true');
		my $clause = rand() < 0.5 ? " DEFAULT $defaults{$t}" : '';
		$s .= both("ALTER TABLE %T ADD COLUMN $name $t$clause");
		push @cols, $name;
		$type{$name} = $t;
		$d = "add column $name $t$clause";
	}
	elsif ($kind eq 'drop column')
	{
		my @c = grep { $_ ne 'id' } @cols;
		return () unless @c > 3;
		my $name = $c[ int(rand(@c)) ];
		$s .= both("ALTER TABLE %T DROP COLUMN $name");
		@cols = grep { $_ ne $name } @cols;
		$d = "drop column $name";
	}
	elsif ($kind eq 'options')
	{
		my $codec = $codecs[ int(rand(@codecs)) ];
		my $level = $codec eq 'zstd' ? ', compression_level => ' . (1 + int(rand(9))) : '';
		my $stripe = (1000, 1500, 3000, 150000)[ int(rand(4)) ];
		my $chunk = (100, 250, 1000)[ int(rand(3)) ];
		$chunk = $stripe if $chunk > $stripe;
		$s .= "SELECT kingres.columnar_set_options('c', compression => '$codec'$level, "
		  . "stripe_row_count => $stripe, chunk_row_count => $chunk);\n";
		$d = "options $codec $stripe/$chunk";
	}
	elsif ($kind eq 'checkpoint')
	{
		$s .= "CHECKPOINT;\n";
	}
	return ($s, $d);
}

# A rewrite of the table: a type change of column a on both tables, or the
# columnar table taken through the heap access method and back.  Rows the
# transaction buffered must be carried into the new storage exactly once, at
# every savepoint level, and a rollback to a savepoint brings the old file
# and its rows back.
sub rewrite_step
{
	if (rand() < 0.25 || !has('a'))
	{
		return "ALTER TABLE c SET ACCESS METHOD heap;\nALTER TABLE c SET ACCESS METHOD columnar;\n"
		  . "SELECT kingres.columnar_set_options('c', stripe_row_count => 1000, chunk_row_count => 100);\n";
	}
	my $nt = (qw(bigint int numeric))[ int(rand(3)) ];
	return both("ALTER TABLE %T ALTER COLUMN a TYPE $nt USING a::$nt");
}

# step kinds and weights
my @KINDS = (
	[ insert => 30 ], [ copy => 4 ], [ txn => 28 ], [ truncate => 2 ], [ rewrite => 4 ],
	[ vacuum => 3 ], ['vacuum full' => 2], [ analyze => 2 ], [ compact => 3 ],
	['add column' => 2], ['drop column' => 1], [ options => 5 ], [ checkpoint => 1 ],
	[ restart => 1.2 ],
);
my $wsum = 0;
$wsum += $_->[1] for @KINDS;

sub pick_kind
{
	my $r = rand($wsum);
	for my $k (@KINDS) { return $k->[0] if ($r -= $k->[1]) < 0; }
	return 'insert';
}

# run a script; returns (comparisons hash, errors list, raw stderr)
sub run_script
{
	my ($script) = @_;
	my $full = "\\set ON_ERROR_STOP 0\nSET lc_messages = 'C';\n$script";
	my ($out, $err) = ('', '');
	my $ret = $node->psql('postgres', $full, stdout => \$out, stderr => \$err, on_error_stop => 0, timeout => 300);
	my (%res, @errs);
	my ($cur_id, $cur_which, $cur_v);
	for my $line (split /\n/, $out)
	{
		if ($line =~ /^\@\@Q (\d+) (h|c) (\w+)/)
		{
			($cur_id, $cur_which, $cur_v) = ($1, $2, $3);
			$res{$cur_id}{$cur_which}{$cur_v} = '';
		}
		elsif ($line =~ /^\@\@E (\d+)/) { undef $cur_which; }
		elsif (defined $cur_which)
		{
			$res{$cur_id}{$cur_which}{$cur_v} .= "$line\n";
		}
	}
	@errs = ($err =~ /^psql:<stdin>:\d+: (?:ERROR|FATAL|PANIC):\s+(.*)$/mg);
	return (\%res, \@errs, $err, $ret);
}

# compare the results of a script; returns an error text or undef
sub check_results
{
	my ($res, $errs, $err) = @_;
	for my $e (@$errs)
	{
		next if $e =~ /^cannot truncate columnar table/;
		return "unexpected error: $e";
	}
	for my $id (sort { $a <=> $b } keys %$res)
	{
		my $r = $res->{$id};
		my $href = $r->{h}{scan};
		for my $v (sort keys %{ $r->{h} })
		{
			my $hv = $r->{h}{$v};
			my $cv = $r->{c}{$v};
			return "query $id ($v) produced no result: $QTEXT{$id}" unless defined $hv && defined $cv;
			return "query $id ($v) differs between heap and columnar:\n  $QTEXT{$id}\n  heap:     $hv  columnar: $cv"
			  if $hv ne $cv;
			$compared++;
		}
	}
	return undef;
}

# ---------------------------------------------------------------------------
# A few fixed checks before the random run
# ---------------------------------------------------------------------------
$node->safe_psql('postgres', 'INSERT INTO c SELECT * FROM h');
my $plan = $node->safe_psql('postgres', 'EXPLAIN (costs off) SELECT count(*) FROM c WHERE a < 5');
like($plan, qr/Columnar Scan/, 'the default plan of the columnar table uses the Columnar Scan');
$plan = $node->safe_psql('postgres', "SET kingres_columnar.enable_custom_scan = off; EXPLAIN (costs off) SELECT count(*) FROM c WHERE a < 5");
unlike($plan, qr/Columnar Scan/, 'with enable_custom_scan off the plan is a plain scan');

# ---------------------------------------------------------------------------
# The random run
# ---------------------------------------------------------------------------
my $failure;
for my $step (1 .. $steps)
{
	my $kind = pick_kind();
	my ($script, $desc);

	if ($kind eq 'restart')
	{
		my $how = rand() < 0.5 ? 'immediate' : 'fast';
		$node->stop($how);
		$node->start;
		$restarts++;
		$stat{"restart ($how)"}++;
		push @log, "step $step: restart $how";
		$script = '';
		$desc = "restart $how";
	}
	else
	{
		($script, $desc) = make_step($kind);
		next unless defined $script;
		$stat{$kind}++;
	}

	# the comparison after the step (inside the transaction for 'txn' steps
	# it already ran; this one sees the committed result)
	my $full = ($step % 25 == 0);
	my $cmp = compare_sql(1 + (rand() < 0.3 ? 1 : 0), $full);
	my $whole = $script . $cmp;

	my ($res, $errs, $err, $ret) = run_script($whole);
	$refusals++ for grep { /^cannot truncate columnar table/ } @$errs;
	push @log, "step $step: $desc\n$whole";
	shift @log while @log > 30;
	diag("step $step: $desc") if $ENV{KC_COMPARE_VERBOSE};

	my $problem = check_results($res, $errs, $err);
	if (defined $problem)
	{
		$failure = "step $step ($desc): $problem";
		last;
	}

	# the stripe catalog must agree with the files after every kind of step
	if ($step % 50 == 0)
	{
		my $chk = $node->safe_psql('postgres',
			"SELECT coalesce(string_agg(check_name || ' ' || relation || ' ' || detail, '; '), '') FROM kingres.columnar_check()");
		if ($chk ne '')
		{
			$failure = "step $step ($desc): columnar_check() reports: $chk";
			last;
		}
	}

	# keep the tables from growing without bound
	if ($step % 40 == 0)
	{
		$cur_rows = $node->safe_psql('postgres', 'SELECT count(*) FROM h');
		if ($cur_rows > 40000)
		{
			my ($res2, $errs2) = run_script("BEGIN;\nTRUNCATE h;\nTRUNCATE c;\nCOMMIT;\n");
			$cur_rows = 0;
			$stat{'size cap truncate'}++;
		}
	}
}

if (defined $failure)
{
	diag("KC_COMPARE_SEED=$seed (repeat with KC_COMPARE_SEED=$seed KC_COMPARE_STEPS=$steps)");
	diag("FAILED: $failure");
	diag("the last steps (oldest first):\n" . join("\n---\n", @log));
	diag('columns now: ' . join(', ', @cols));
}
ok(!defined $failure, "heap and columnar agree on $steps random steps (seed $seed)");

# the server must still be healthy, and the final state consistent
my $final_h = $node->safe_psql('postgres', "SELECT count(*), md5(string_agg(x::text, '|' ORDER BY x.id)) FROM h x");
my $final_c = $node->safe_psql('postgres', "SELECT count(*), md5(string_agg(x::text, '|' ORDER BY x.id)) FROM c x");
is($final_c, $final_h, 'the final contents of the two tables are identical');
$node->stop('fast');
$node->start;
is($node->safe_psql('postgres', "SELECT count(*), md5(string_agg(x::text, '|' ORDER BY x.id)) FROM c x"),
	$final_h, 'the columnar table reads the same after a clean restart');
is($node->safe_psql('postgres', "SELECT coalesce(string_agg(check_name || ' ' || relation || ' ' || detail, E'\n'), '') FROM kingres.columnar_check()"), '',	'columnar_check() reports nothing');
unlike(slurp_file($node->logfile), qr/TRAP:|terminated by signal/, 'no crash in the server log');

diag(sprintf('%d comparisons of heap against columnar, %d truncate refusals handled, %d restarts; steps: %s',
	$compared, $refusals, $restarts, join(', ', map { "$_=$stat{$_}" } sort keys %stat)));
$node->stop;
done_testing();
