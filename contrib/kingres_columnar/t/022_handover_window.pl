# contrib/kingres_columnar/t/022_handover_window.pl
#
# C5-03.  A reader or a writer that runs between the two phases of the hand-over
# of a subtransaction's buffers to its parent (a callback of another extension
# at the end of the subtransaction, which PostgreSQL itself never has) must see
# every row once, and a write there must not be lost or doubled: it is refused.
# The defect: phase one writes the stripes the parent's buffer calls for and
# leaves the rows in the buffers until phase two, so a reader in the window
# found the rows twice (2002 for 1002), and a write took the branch of phase two
# that adopts the child's buffer and left the parent's rows in a stripe and in
# the buffer, to be written twice by the commit.
#
# The probe is a small module of this test (t/kcprobe/kcprobe.c): its callback
# runs a statement from the setting kcprobe.sql after the callback of the access
# method.  It needs a compiler and the server's headers; where it cannot be built
# the test is skipped with the reason.
#
# Portions Copyright (c) 2026, RooyeKhat Media Co
# SPDX-License-Identifier: AGPL-3.0-only

use strict;
use warnings FATAL => 'all';

use File::Path qw(make_path);
use FindBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;

my $src = "$FindBin::RealBin/kcprobe/kcprobe.c";
my $dir = PostgreSQL::Test::Utils::tempdir();
my $pg_config = $ENV{PG_CONFIG} || 'pg_config';
my $inc = `$pg_config --includedir-server 2>/dev/null`;
chomp $inc;
my $cc = `$pg_config --cc 2>/dev/null`;
chomp $cc;
$cc = 'cc' if $cc eq '';

my $built = 0;
if ($inc ne '' && -f $src && -d $inc)
{
	my $out = `$cc -shared -fPIC -I"$inc" -o "$dir/kcprobe.so" "$src" 2>&1`;
	$built = ($? == 0 && -f "$dir/kcprobe.so");
	diag("building the probe module failed: $out") unless $built;
}
plan skip_all => 'the probe module (t/kcprobe/kcprobe.c) could not be built here: it needs a C compiler and the server headers'
  unless $built;

my $node = PostgreSQL::Test::Cluster->new('colwindow');
$node->init;
$node->append_conf('postgresql.conf', "shared_preload_libraries = '$dir/kcprobe,kingres_columnar'\n");
$node->start;
$node->safe_psql('postgres', 'CREATE EXTENSION kingres_columnar');

sub script
{
	my ($sql) = @_;
	my ($out, $err) = ('', '');

	$node->psql('postgres', $sql, stdout => \$out, stderr => \$err, on_error_stop => 0);
	return ($out, $err);
}

sub fresh
{
	my ($stripe, $chunk) = @_;

	$node->safe_psql('postgres', "
		DROP TABLE IF EXISTS p, q;
		CREATE TABLE p (a int) USING columnar;
		CREATE TABLE q (a int) USING columnar;
		SELECT kingres.columnar_set_options('p', stripe_row_count => $stripe, chunk_row_count => $chunk);
		SELECT kingres.columnar_set_options('q', stripe_row_count => $stripe, chunk_row_count => $chunk);");
}

sub probe_values
{
	my ($err) = @_;
	return join(',', $err =~ /kcprobe: (.+)/g);
}

# ------------------------------------------------------------------------
# 1. A reader in the window sees each row once
# ------------------------------------------------------------------------
# (parent rows, child rows, stripe size): a stripe is written by phase one when
# the parent's buffer reaches the stripe size with the child's rows
foreach my $c ([999, 3, 1000], [5, 3, 1000], [999, 1, 1000], [1000, 5, 1000], [2500, 700, 1000],
	[0, 1500, 1000], [10, 3000, 1000])
{
	my ($np, $nc, $stripe) = @$c;
	fresh($stripe, 100);
	my ($out, $err) = script("
BEGIN;
INSERT INTO p SELECT generate_series(1, $np);
SAVEPOINT s;
INSERT INTO p SELECT generate_series(10001, 10000 + $nc);
SET kcprobe.sql = 'SELECT count(*) || \$\$ \$\$ || coalesce(sum(a), 0) FROM p';
RELEASE s;
SET kcprobe.sql = '';
SELECT 'AFTER=' || count(*) || ' ' || coalesce(sum(a), 0) FROM p;
COMMIT;
SELECT 'FINAL=' || count(*) || ' ' || coalesce(sum(a), 0) FROM p;");
	my $sum = $np * ($np + 1) / 2 + $nc * 10000 + $nc * ($nc + 1) / 2;
	my $want = ($np + $nc) . " $sum";
	my @seen = $err =~ /kcprobe: (\d+ \d+)/g;
	is(scalar(@seen) . ':' . ($seen[0] // ''), "1:$want",
		"parent $np rows, child $nc rows, stripe $stripe: the callback between the phases sees each row once");
	like($out, qr/AFTER=\Q$want\E/, "... and the transaction after the release");
	like($out, qr/FINAL=\Q$want\E/, "... and after the commit");
}

# two tables in the window, and three levels
{
	fresh(1000, 100);
	my ($out, $err) = script("
BEGIN;
INSERT INTO p SELECT generate_series(1, 999);
INSERT INTO q SELECT generate_series(1, 998);
SAVEPOINT s1;
INSERT INTO p SELECT generate_series(1000, 1002);
INSERT INTO q SELECT generate_series(1000, 1004);
SAVEPOINT s2;
INSERT INTO p SELECT generate_series(2000, 2001);
SET kcprobe.sql = 'SELECT (SELECT count(*) FROM p) || \$\$/\$\$ || (SELECT count(*) FROM q)';
RELEASE s2;
RELEASE s1;
SET kcprobe.sql = '';
COMMIT;
SELECT 'FINAL=' || (SELECT count(*) FROM p) || '/' || (SELECT count(*) FROM q);");
	is(probe_values($err), '1004/1003,1004/1003', 'two tables and two levels: each callback sees both tables whole, once');
	like($out, qr/FINAL=1004\/1003/, '... and the commit keeps them');
}

# memory pressure writes the merged buffers in phase one too
{
	fresh(100000, 100);
	my ($out, $err) = script("
SET kingres_columnar.write_buffer_memory = '1MB';
BEGIN;
INSERT INTO p SELECT g FROM generate_series(1, 8000) g;
SAVEPOINT s;
INSERT INTO p SELECT g FROM generate_series(9001, 12000) g;
SET kcprobe.sql = 'SELECT count(*) || \$\$ \$\$ || sum(a) FROM p';
RELEASE s;
SET kcprobe.sql = '';
COMMIT;
SELECT 'FINAL=' || count(*) || ' ' || sum(a) FROM p;");
	my $sum = 8000 * 8001 / 2 + 3000 * 9000 + 3000 * 3001 / 2;
	is(probe_values($err), "11000 $sum", 'memory pressure: the callback sees each row once');
	like($out, qr/FINAL=11000 \Q$sum\E/, '... and the commit keeps them');
	my $stripes = $node->safe_psql('postgres', "SELECT count(*) FROM kingres.columnar_stripes('p')");
	cmp_ok($stripes, '>=', 1, 'the buffers were written under the allowance');
}

# ------------------------------------------------------------------------
# 2. A write in the window to a table whose rows are being handed over is
#    refused cleanly; the subtransaction is rolled back, the parent's rows stay
# ------------------------------------------------------------------------
foreach my $c ([999, 3], [5, 3], [2500, 700])
{
	my ($np, $nc) = @$c;
	my $sumnp = $np * ($np + 1) / 2;
	fresh(1000, 100);
	my ($out, $err) = script("
BEGIN;
INSERT INTO p SELECT generate_series(1, $np);
SET kcprobe.sql = 'INSERT INTO p VALUES (-1)';
DO \$\$ BEGIN
  BEGIN
    INSERT INTO p SELECT generate_series(10001, 10000 + $nc);
  EXCEPTION WHEN OTHERS THEN RAISE NOTICE 'caught: %', sqlerrm;
  END;
END \$\$;
SET kcprobe.sql = '';
SELECT 'IN=' || count(*) || ' ' || coalesce(min(a), 0) FROM p;
COMMIT;
SELECT 'FINAL=' || count(*) || ' ' || sum(a) FROM p;");
	like($err, qr/caught: columnar table "p" cannot be written while a subtransaction is being committed/,
		"parent $np, child $nc: a write between the phases is refused");
	like($out, qr/IN=$np 1\b/, "... the parent's rows are all there, no row of the child, nothing doubled");
	like($out, qr/FINAL=$np $sumnp\b/, "... and the commit writes each of them once");
}

# a write to a table the subtransaction did not write in the window is fine
{
	fresh(1000, 100);
	my ($out, $err) = script("
BEGIN;
INSERT INTO p SELECT generate_series(1, 999);
SAVEPOINT s;
INSERT INTO p SELECT generate_series(1000, 1002);
SET kcprobe.sql = 'INSERT INTO q VALUES (7)';
RELEASE s;
SET kcprobe.sql = '';
COMMIT;
SELECT 'FINAL=' || (SELECT count(*) FROM p) || '/' || (SELECT count(*) FROM q);");
	unlike($err, qr/ERROR/, 'a write to another table in the window: no error');
	like($out, qr/FINAL=1002\/1/, '... and both tables are right');
}

# ------------------------------------------------------------------------
# 3. A cursor that began in the window cannot go on afterwards, and is
#    refused cleanly rather than reading other rows than it began with
# ------------------------------------------------------------------------
{
	fresh(1000, 100);
	my ($out, $err) = script("
BEGIN;
INSERT INTO p SELECT generate_series(1, 999);
SAVEPOINT s;
INSERT INTO p SELECT generate_series(1000, 1002);
SET kcprobe.sql = 'cursor:SELECT a FROM p';
RELEASE s;
SET kcprobe.sql = '';
FETCH ALL FROM kcc;
ROLLBACK;
SELECT 'FINAL=' || count(*) FROM p;");
	like($err, qr/the scan of columnar table "p" cannot go on: it began while a subtransaction was being committed/,
		'a cursor opened in the window cannot go on after it');
	like($out, qr/FINAL=0/, '... and nothing is left of the aborted transaction');
}

# the ordinary case keeps working: a cursor opened before, or after, is not affected
{
	fresh(1000, 100);
	my ($out, $err) = script("
BEGIN;
INSERT INTO p SELECT generate_series(1, 999);
DECLARE c1 CURSOR FOR SELECT a FROM p;
FETCH 2 FROM c1;
SAVEPOINT s;
INSERT INTO p SELECT generate_series(1000, 1002);
RELEASE s;
FETCH 2 FROM c1;
DECLARE c2 CURSOR FOR SELECT a FROM p;
FETCH 1 FROM c2;
CLOSE c1; CLOSE c2;
SELECT 'IN=' || count(*) FROM p;
COMMIT;");
	unlike($err, qr/ERROR/, 'cursors opened before and after a release: no error');
	like($out, qr/IN=1002/, '... all rows there');
}


# ------------------------------------------------------------------------
# 4. Memory at the moment of the commit of a subtransaction (C5-01): a child's
#    buffer that is a real part of the allowance is written by the child and
#    not copied into the parent's buffer, which would hold its rows twice for
#    the length of the commit.  The probe reads the memory of the buffers
#    between the phases.
# ------------------------------------------------------------------------
{
	$node->safe_psql('postgres', q{
CREATE FUNCTION kcmem() RETURNS bigint LANGUAGE sql AS
$$ SELECT coalesce(sum(total_bytes), 0)::bigint FROM pg_backend_memory_contexts
    WHERE name IN ('kingres_columnar rows', 'kingres_columnar write buffers') $$;
DROP TABLE IF EXISTS tw;
CREATE TABLE tw (id int, pad text) USING columnar;});
	my $A = 8 * 1024 * 1024;

	# the parent holds 3 MB, the child 5 MB: together within the allowance, so nothing
	# was written, and a copy of the child's rows would take the memory to 13 MB
	my ($out, $err) = script("
SET kingres_columnar.write_buffer_memory = '8MB';
BEGIN;
INSERT INTO tw SELECT g, repeat('x', 100000) FROM generate_series(1, 30) g;
SET kcprobe.sql = 'SELECT kcmem()';
DO \$\$ BEGIN BEGIN
  INSERT INTO tw SELECT g, repeat('x', 100000) FROM generate_series(1000, 1049) g;
EXCEPTION WHEN division_by_zero THEN NULL; END; END \$\$;
SET kcprobe.sql = '';
COMMIT;
SELECT 'N=' || count(*) FROM tw;");
	my ($seen) = $err =~ /kcprobe: (\d+)/;
	cmp_ok($seen, '<=', $A + 1024 * 1024,
		"a child holding 5 MB commits into a parent holding 3 MB: $seen bytes in the window, not a copy of the 5 MB");
	like($out, qr/N=80\b/, '... and the rows are all there');

	# many small children: each copied, the merged buffer written as it fills
	($out, $err) = script("
SET kingres_columnar.write_buffer_memory = '8MB';
BEGIN;
SET kcprobe.sql = 'SELECT kcmem()';
DO \$\$ BEGIN FOR i IN 1..300 LOOP BEGIN
  INSERT INTO tw VALUES (i, repeat('x', 100000));
EXCEPTION WHEN division_by_zero THEN NULL; END; END LOOP; END \$\$;
SET kcprobe.sql = '';
COMMIT;
SELECT 'N=' || count(*) FROM tw;");
	my $peak = 0;
	foreach my $v ($err =~ /kcprobe: (\d+)/g) { $peak = $v if $v > $peak; }
	cmp_ok($peak, '<=', $A + 2 * 1024 * 1024,
		"300 children of one wide row: $peak bytes at the busiest window, within the allowance and a copy of one row");
	like($out, qr/N=380\b/, '... and the rows are all there');
}

$node->stop;
done_testing();
