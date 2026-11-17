# contrib/kingres_columnar/t/025_memory_bound.pl
#
# C6-03.  The memory held by the write buffers of one backend is bounded by a
# number that follows from the settings alone, whatever the number of levels of
# subtransaction, of relations, of partitions and of open cursors.
#
# The bound (README, "Memory"), with A = kingres_columnar.write_buffer_memory and
# R the bytes of the row being written:
#
#   the buffers hold at most 2A + R at the end of every write (the ceiling: a
#   write that would go on from above it is refused with an error);
#   the resident memory of the backend grows by at most
#       2 * (2A + R)         the buffers, with the slack of the allocator
#     + 2A + R               the rows of a committing subtransaction, which
#                            exist twice until phase two of its hand-over
#     + S                    the stripe being built (not counted; it follows
#                            from the stripe size and the width of the rows)
#     + 12 MB                what a backend needs to run the statements
#
# The memory is measured as the operating system sees it (VmPeak of the backend,
# the peak size of its address space, read from /proc/self/status by the backend
# itself: it counts what is allocated, whether or not it was touched, and not the
# shared buffers the backend touches), not with a counter of the extension.  Every scenario runs in a new session and reports the growth of the
# high-water mark over the first statement of that session.  Where a scenario
# succeeds its rows are compared with a heap twin; where it is refused the error
# is the one that names the limit, and the transaction that was refused leaves
# nothing behind.
#
# Portions Copyright (c) 2026, RooyeKhat Media Co
# SPDX-License-Identifier: AGPL-3.0-only

use strict;
use warnings FATAL => 'all';

use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;

plan skip_all => 'needs /proc to read the memory of a backend' unless -r '/proc/self/status';

my $node = PostgreSQL::Test::Cluster->new('colmembound');
$node->init;
$node->append_conf('postgresql.conf', q{
shared_preload_libraries = 'kingres_columnar'
max_locks_per_transaction = 4000
});
$node->start;
$node->safe_psql('postgres', q{
CREATE EXTENSION kingres_columnar;
CREATE FUNCTION hwm() RETURNS bigint LANGUAGE sql AS
$$ SELECT (regexp_match(pg_read_file('/proc/self/status'), 'VmPeak:\s+(\d+) kB'))[1]::bigint * 1024 $$;
CREATE FUNCTION kcmem() RETURNS bigint LANGUAGE sql AS
$$ SELECT coalesce(sum(total_bytes), 0)::bigint FROM pg_backend_memory_contexts
    WHERE name IN ('kingres_columnar rows', 'kingres_columnar write buffers') $$;});

my $MB = 1024 * 1024;

# run a script in a new session; returns (stdout, stderr, growth of the peak
# size in bytes)
sub measured
{
	my ($sql) = @_;
	my ($out, $err) = ('', '');

	$node->psql('postgres', "SELECT 'BASE=' || hwm();\n$sql\nROLLBACK;\nSELECT 'PEAK=' || hwm();",
		stdout => \$out, stderr => \$err, on_error_stop => 0);
	my ($base) = $out =~ /BASE=(\d+)/;
	my ($peak) = $out =~ /PEAK=(\d+)/;
	return ($out, $err, $peak - $base);
}

# the stated bound, in bytes, for allowance A, a row of R bytes and a stripe build of S bytes
sub bound
{
	my ($A, $R, $S) = @_;
	return 2 * (2 * $A + $R) + (2 * $A + $R) + $S + 12 * $MB;
}

my $limit_error = qr/the write buffers of columnar tables (hold|would hold) \d+ bytes, more than the limit of \d+ bytes/;

# ------------------------------------------------------------------------
# 1. Levels of subtransaction.  60 nested savepoints, 3.6 MB of rows each, under
#    an allowance of 8 MB (before: 206 MB held, +209 MB resident).
# ------------------------------------------------------------------------
{
	my $A = 8 * $MB;
	$node->safe_psql('postgres', q{
DROP TABLE IF EXISTS tl, hl;
CREATE TABLE tl (id int, pad text) USING columnar;
SELECT kingres.columnar_set_options('tl', stripe_row_count => 100000);
CREATE TABLE hl (id int, pad text);});
	my $levels = join("\n", map { "SAVEPOINT s$_; INSERT INTO tl SELECT g, repeat('x', 1000) FROM generate_series(1, 3600) g;" } 1 .. 60);
	my ($out, $err, $grow) = measured("SET kingres_columnar.write_buffer_memory = '8MB'; BEGIN;\n$levels\nSELECT 'N=' || count(*) FROM tl;");
	like($err, $limit_error, '60 nested levels of 3.6 MB under 8 MB: the write that goes past the limit is refused');
	like($err, qr/DETAIL:  The limit is twice kingres_columnar.write_buffer_memory/, '... and the error says what the limit is');
	cmp_ok($grow, '<=', bound($A, 1100, 4 * $MB),
		sprintf('... the backend grew by %.1f MB, within the stated bound of %.1f MB (60 levels held 209 MB before)', $grow / $MB, bound($A, 1100, 4 * $MB) / $MB));
	is($node->safe_psql('postgres', 'SELECT count(*) FROM tl'), 0, '... and the refused transaction left nothing behind');

	# a depth that fits is as good as heap: every level, release, rollback to and commit
	my $fits = join("\n", map { "SAVEPOINT s$_; INSERT INTO %T% SELECT g + $_ * 100000, repeat('x', 1000) FROM generate_series(1, 3600) g;" } 1 .. 3);
	foreach my $t ('tl', 'hl')
	{
		(my $s = $fits) =~ s/%T%/$t/g;
		$node->safe_psql('postgres', "SET kingres_columnar.write_buffer_memory = '8MB'; BEGIN;\n$s\nROLLBACK TO s2;\nRELEASE s1;\nCOMMIT;");
	}
	is($node->safe_psql('postgres', "SELECT count(*) || '/' || sum(id) FROM tl"),
	   $node->safe_psql('postgres', "SELECT count(*) || '/' || sum(id) FROM hl"),
	   'three levels, a rollback to the second and a release of the first: the rows are those of the heap twin');
	cmp_ok($node->safe_psql('postgres', "SELECT count(*) FROM tl"), '==', 3600, '... 3600 of them (the first level)');
}

# ------------------------------------------------------------------------
# 2. Open cursors, each pinning the rows written since the previous one: 40
#    cursors under 8 MB (before: 114 MB held, +119 MB resident).
# ------------------------------------------------------------------------
{
	my $A = 8 * $MB;
	$node->safe_psql('postgres', q{
DROP TABLE IF EXISTS tc;
CREATE TABLE tc (id int, pad text) USING columnar;
SELECT kingres.columnar_set_options('tc', stripe_row_count => 100000);});
	my $cur = join("\n", map { "INSERT INTO tc SELECT g, repeat('x', 1000) FROM generate_series(1, 3000) g;\nDECLARE c$_ CURSOR FOR SELECT count(*) FROM tc;\nFETCH 1 FROM c$_;" } 1 .. 40);
	my ($out, $err, $grow) = measured("SET kingres_columnar.write_buffer_memory = '8MB'; BEGIN;\n$cur\n");
	like($err, $limit_error, '40 cursors, each after 3 MB of rows, under 8 MB: the write that goes past the limit is refused');
	cmp_ok($grow, '<=', bound($A, 1100, 4 * $MB),
		sprintf('... the backend grew by %.1f MB, within the stated bound of %.1f MB', $grow / $MB, bound($A, 1100, 4 * $MB) / $MB));
	is($node->safe_psql('postgres', 'SELECT count(*) FROM tc'), 0, '... and nothing was committed');
}

# ------------------------------------------------------------------------
# 3. Relations: 1 500 partitions of one table and 1 500 tables, in one
#    transaction, under the smallest allowance (1 MB): the buffers of that many
#    relations are charged for what they cost, so what the buffers add follows
#    the allowance and not the number of relations.  The relations themselves
#    cost what they cost heap (the relation cache), so the growth is compared
#    with a heap twin of the same scenario: columnar may grow by the bound of
#    the buffers beyond it.  The rows are those of the twin.
# ------------------------------------------------------------------------
{
	my $A = 1 * $MB;
	my $buffers = bound($A, 100, 2 * $MB) - 12 * $MB;
	$node->safe_psql('postgres', q{
DROP TABLE IF EXISTS pt, hpt;
CREATE TABLE pt (a int, b text) PARTITION BY HASH (a);
CREATE TABLE hpt (a int, b text) PARTITION BY HASH (a);
DO $$ BEGIN FOR i IN 0..1499 LOOP
  EXECUTE format('CREATE TABLE pt_%s PARTITION OF pt FOR VALUES WITH (modulus 1500, remainder %s) USING columnar', i, i);
  EXECUTE format('CREATE TABLE hpt_%s PARTITION OF hpt FOR VALUES WITH (modulus 1500, remainder %s)', i, i);
END LOOP; END $$;});
	my %grow;
	foreach my $t ('pt', 'hpt')
	{
		my ($out, $err, $g) = measured("SET kingres_columnar.write_buffer_memory = '1MB'; BEGIN;
INSERT INTO $t SELECT g, 'row' || g FROM generate_series(1, 30000) g;
COMMIT;");
		unlike($err, qr/ERROR/, "30 000 rows into 1 500 partitions of $t under 1 MB: no error");
		$grow{$t} = $g;
	}
	cmp_ok($grow{pt}, '<=', $grow{hpt} + $buffers,
		sprintf('1 500 partitions: columnar grew by %.1f MB, heap by %.1f MB, the buffers may add %.1f MB',
			$grow{pt} / $MB, $grow{hpt} / $MB, $buffers / $MB));
	is($node->safe_psql('postgres', 'SELECT count(*) || \'/\' || sum(a) FROM pt'),
	   $node->safe_psql('postgres', 'SELECT count(*) || \'/\' || sum(a) FROM hpt'), '... and the commit has the rows the heap twin has');

	$node->safe_psql('postgres', q{
DO $$ BEGIN FOR i IN 0..1499 LOOP
  EXECUTE format('DROP TABLE IF EXISTS mt_%s; CREATE TABLE mt_%s (a int, b text) USING columnar', i, i);
  EXECUTE format('DROP TABLE IF EXISTS ht_%s; CREATE TABLE ht_%s (a int, b text)', i, i);
END LOOP; END $$;});
	foreach my $p (['mt', 'columnar'], ['ht', 'heap'])
	{
		my ($pre, $name) = @$p;
		my ($out, $err, $g) = measured("SET kingres_columnar.write_buffer_memory = '1MB'; BEGIN;
DO \$\$ BEGIN FOR i IN 0..1499 LOOP
  EXECUTE format('INSERT INTO ${pre}_%s SELECT g, repeat(\$x\$y\$x\$, 100) FROM generate_series(1, 5) g', i);
END LOOP; END \$\$;
COMMIT;");
		unlike($err, qr/ERROR/, "1 500 $name tables, 5 rows each, in one transaction under 1 MB: no error");
		$grow{$pre} = $g;
	}
	cmp_ok($grow{mt}, '<=', $grow{ht} + $buffers,
		sprintf('1 500 tables: columnar grew by %.1f MB, heap by %.1f MB, the buffers may add %.1f MB',
			$grow{mt} / $MB, $grow{ht} / $MB, $buffers / $MB));
	my @tot;
	foreach my $pre ('mt', 'ht')
	{
		my ($out, $err) = ('', '');
		$node->psql('postgres', "DO \$\$ DECLARE n bigint := 0; c bigint; BEGIN FOR i IN 0..1499 LOOP EXECUTE format('SELECT count(*) FROM ${pre}_%s', i) INTO c; n := n + c; END LOOP; RAISE NOTICE 'TOTAL=%', n; END \$\$;",
			stdout => \$out, stderr => \$err);
		push @tot, $err =~ /TOTAL=(\d+)/;
	}
	is("$tot[0]/$tot[1]", '7500/7500', '... and every table has its rows, as the heap tables do');
}

# ------------------------------------------------------------------------
# 4. Wide rows: rows of 5 MB under 8 MB, in one statement and in subtransactions
#    (the row being written is the R of the bound, the stripe it makes the S)
# ------------------------------------------------------------------------
{
	my $A = 8 * $MB;
	my $R = 5 * $MB;
	$node->safe_psql('postgres', q{
DROP TABLE IF EXISTS tw, hw;
CREATE TABLE tw (id int, pad text) USING columnar;
SELECT kingres.columnar_set_options('tw', stripe_row_count => 100000);
CREATE TABLE hw (id int, pad text);});
	my ($out, $err, $grow) = measured("SET kingres_columnar.write_buffer_memory = '8MB'; BEGIN;
INSERT INTO tw SELECT g, repeat('w', 5000000) FROM generate_series(1, 24) g;
SELECT 'N=' || count(*) FROM tw;
DO \$\$ BEGIN FOR i IN 1..12 LOOP BEGIN
  INSERT INTO tw SELECT 100 + i, repeat('v', 5000000);
  IF i % 4 = 0 THEN RAISE EXCEPTION 'x'; END IF;
EXCEPTION WHEN raise_exception THEN NULL; END; END LOOP; END \$\$;
SELECT 'M=' || count(*) FROM tw;
COMMIT;");
	unlike($err, qr/ERROR/, 'rows of 5 MB under 8 MB, in one statement and in blocks that fail: no error');
	like($out, qr/N=24\b/, '... the statement\'s rows are there');
	like($out, qr/M=33\b/, '... and the blocks\' rows that were not rolled back');
	cmp_ok($grow, '<=', bound($A, $R, 40 * $MB),
		sprintf('... the backend grew by %.1f MB, within the stated bound of %.1f MB', $grow / $MB, bound($A, $R, 40 * $MB) / $MB));
}

# ------------------------------------------------------------------------
# 5. The accounting counts memory, not payload (C6-07): a tiny value costs a
#    chunk of the allocator (16 bytes for the empty text, whose payload is one
#    byte).  The memory of the buffers, measured by the server (the contexts) and
#    not by the extension, stays near the allowance of 16 MB; with the payload
#    counted instead of the chunk it is about twice that.
# ------------------------------------------------------------------------
{
	$node->safe_psql('postgres', q{
DROP TABLE IF EXISTS tt;
CREATE TABLE tt (a text, b text, c text, d text, e text, f text, g text, h text) USING columnar;
SELECT kingres.columnar_set_options('tt', stripe_row_count => 1000000);});
	my ($out, $err) = ('', '');
	$node->psql('postgres', q{
SET kingres_columnar.write_buffer_memory = '16MB';
BEGIN;
DO $$ DECLARE mx bigint := 0; m bigint; BEGIN
  FOR i IN 1..400 LOOP
    INSERT INTO tt SELECT '', '', '', '', '', '', '', '' FROM generate_series(1, 1000);
    m := kcmem();
    IF m > mx THEN mx := m; END IF;
  END LOOP;
  RAISE NOTICE 'MAXMEM=%', mx;
END $$;
ROLLBACK;}, stdout => \$out, stderr => \$err, on_error_stop => 0);
	my ($mx) = $err =~ /MAXMEM=(\d+)/;
	cmp_ok($mx, '<=', 21 * $MB, sprintf('400 000 rows of eight empty values: the buffers hold at most %.1f MB of memory under an allowance of 16 MB', $mx / $MB));
	cmp_ok($mx, '>=', 8 * $MB, '... and the measure sees the buffers (they do hold some)');
}

$node->stop;
done_testing();
