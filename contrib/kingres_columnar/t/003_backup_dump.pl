# contrib/kingres_columnar/t/003_backup_dump.pl
#
# Columnar tables across the ways a database is copied or restored.
#
#   a. pg_basebackup, plain and tar format, WAL streamed; the restored node
#      has the same rows; a node without the library cannot replay the WAL;
#   b. archive recovery to a named restore point and to an LSN: rows of the
#      transactions before the point are there, the later ones are not;
#   c. incremental backup (WAL summaries) and pg_combinebackup, a chain of
#      two increments, with a rolled-back load and a VACUUM that truncates
#      the file between the backups;
#   d. pg_dump in plain (COPY, INSERT), custom and directory format restored
#      into another node: the definition says USING columnar, the rows are
#      equal, the options (kept in the metapage, not in the dump) are those
#      of the restoring session, the stripe catalog is not dumped; a
#      partitioned table with heap and columnar partitions; an empty table;
#      a schema-only dump and a dump of the extension alone;
#   e. COPY TO and COPY FROM in text, CSV and binary format;
#   g. CREATE DATABASE ... TEMPLATE with both strategies, read on the
#      primary, on a standby and after crash recovery.
#
# pg_upgrade is not covered: a columnar file is not claimed to be readable
# by another major version.  Every node preloads the library, because the
# resource manager that replays the stripe pages is registered at preload.
#
# Portions Copyright (c) 2026, RooyeKhat Media Co
# SPDX-License-Identifier: PostgreSQL

use strict;
use warnings FATAL => 'all';

use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;

my $preload = "shared_preload_libraries = 'kingres_columnar'\n";

# Rows with NULLs in every kind of column.
sub gen
{
	my ($table, $lo, $hi) = @_;
	return qq{INSERT INTO $table
	  SELECT i,
	         CASE WHEN i % 7 = 0 THEN NULL ELSE (i % 1000)::int END,
	         CASE WHEN i % 5 = 0 THEN NULL ELSE 'row-' || i || repeat('x', (i % 40)::int) END,
	         CASE WHEN i % 11 = 0 THEN NULL ELSE (i * 1.37)::numeric(14,3) END,
	         date '2020-01-01' + (i % 365)::int,
	         i % 3 = 0
	    FROM generate_series($lo, $hi) i};
}
my $cols = '(id bigint, a int, t text, n numeric(14,3), d date, f boolean)';

# A table of many types, to see them survive text and binary transport.
my $tycols = '(id int, b bytea, j jsonb, arr int[], ts timestamptz, u uuid, r float8, ip inet, tx text[])';
sub gen_ty
{
	my ($lo, $hi) = @_;
	return qq{INSERT INTO ty
	  SELECT i,
	         CASE WHEN i % 9 = 0 THEN NULL ELSE decode(md5(i::text), 'hex') END,
	         jsonb_build_object('i', i, 's', md5(i::text)),
	         CASE WHEN i % 4 = 0 THEN NULL ELSE ARRAY[i, i + 1, NULL] END,
	         timestamptz '2025-01-01 00:00:00+00' + i * interval '1 minute',
	         CASE WHEN i % 6 = 0 THEN NULL ELSE md5(i::text)::uuid END,
	         i / 7.0,
	         '10.0.0.0'::inet + i,
	         ARRAY['a' || i, NULL, 'c']
	    FROM generate_series($lo, $hi) i};
}

# One query, one snapshot: counts, sums and a hash of the whole row text.
sub fp
{
	my ($table) = @_;
	return qq{SELECT count(*), coalesce(sum(id), 0), count(a), sum(a), count(t),
	                 sum(length(t)), count(n), sum(n), count(*) FILTER (WHERE f),
	                 sum(hashtext(c::text))
	            FROM $table c};
}
sub fp_any
{
	my ($table) = @_;
	return "SELECT count(*), sum(hashtext(c::text)) FROM $table c";
}

# What columnar_check reports, as one string; empty means clean.
sub check_rows
{
	my ($node, $db) = @_;
	return $node->safe_psql($db // 'postgres',
		"SELECT string_agg(check_name || ' ' || coalesce(relation::text, '-') || ' ' || detail, '; ') FROM kingres.columnar_check()"
	);
}

sub new_node
{
	my ($name, %o) = @_;
	my $node = PostgreSQL::Test::Cluster->new($name);
	$node->init(
		allows_streaming => 1,
		extra => ['--data-checksums']);
	$node->append_conf('postgresql.conf', $preload . "autovacuum = off\n" . ($o{conf} // ''));
	$node->enable_archiving if $o{archive};
	$node->start;
	return $node;
}

# A node restored from $backup of $root, started (or not) with its own conf.
sub restored_node
{
	my ($name, $root, $backup, %o) = @_;
	my $node = PostgreSQL::Test::Cluster->new($name);
	$node->init_from_backup($root, $backup, %{ $o{init} // {} });
	$node->append_conf('postgresql.conf', $o{conf}) if defined $o{conf};
	return $node;
}

###############################################################################
# a. pg_basebackup, plain and tar
###############################################################################

my $src = new_node('src');
$src->safe_psql('postgres', 'CREATE EXTENSION kingres_columnar');
$src->safe_psql('postgres', "CREATE TABLE ct $cols USING columnar");
$src->safe_psql('postgres',
	"SELECT kingres.columnar_set_options('ct', stripe_row_count => 5000, chunk_row_count => 1000)");
$src->safe_psql('postgres', gen('ct', 1, 20000));
$src->safe_psql('postgres', gen('ct', 20001, 47000));
$src->safe_psql('postgres', "BEGIN; " . gen('ct', 90001, 100000) . "; ROLLBACK");
my $src_fp = $src->safe_psql('postgres', fp('ct'));
like($src_fp, qr/^47000\|/, 'source has its rows');

$src->backup('plain', backup_options => [ '-X', 'stream', '-Fp' ]);
my $r_plain = restored_node('r_plain', $src, 'plain');
$r_plain->start;
is($r_plain->safe_psql('postgres', fp('ct')), $src_fp,
	'plain base backup restores to the same rows');
is($r_plain->safe_psql('postgres', "SELECT * FROM kingres.columnar_options('ct')"),
	$src->safe_psql('postgres', "SELECT * FROM kingres.columnar_options('ct')"),
	'the options travelled with the file');
is(check_rows($r_plain), '', 'columnar_check clean after the plain backup');
# the restored node is a primary of its own: it loads and vacuums
$r_plain->safe_psql('postgres', gen('ct', 47001, 50000));
$r_plain->safe_psql('postgres', 'VACUUM ct');
is($r_plain->safe_psql('postgres', 'SELECT count(*) FROM ct'), '50000',
	'the restored node accepts a load');
$r_plain->stop;

my $tardir = $src->backup_dir . '/tar';
$src->command_ok(
	[
		'pg_basebackup', '--no-sync', '--checkpoint=fast',
		'-X', 'stream', '-Ft', '-D', $tardir,
		'-h', $src->host, '-p', $src->port,
	],
	'tar format base backup');
my $r_tar = restored_node('r_tar', $src, 'tar', init => { tar_program => 'tar' });
$r_tar->start;
is($r_tar->safe_psql('postgres', fp('ct')), $src_fp,
	'tar base backup restores to the same rows');
$r_tar->stop;

# A backup taken while a columnar transaction loads: the node restored from it
# has the whole transaction or none of it.
{
	my $bg = $src->background_psql('postgres', on_error_stop => 1);
	$bg->query_until(qr/started/,
		"\\echo started\nBEGIN;\n" . gen('ct', 200001, 330000)
		  . ";\nSELECT pg_sleep(3);\nCOMMIT;\n");
	$src->backup('during', backup_options => [ '-X', 'stream' ]);
	$bg->query_safe('SELECT 1');
	$bg->quit;
	my $with_lib = restored_node('with_lib', $src, 'during');
	$with_lib->start;
	my $c = $with_lib->safe_psql('postgres', 'SELECT count(*) FROM ct');
	ok($c == 47000 || $c == 47000 + 130000, "restored a whole transaction or none ($c rows)");
	is(check_rows($with_lib), '', 'columnar_check clean after a backup taken during a load');
	$with_lib->stop;
}

###############################################################################
# b. archive recovery to a restore point and to an LSN
###############################################################################

my $pitr = new_node('pitr', archive => 1);
$pitr->safe_psql('postgres', 'CREATE EXTENSION kingres_columnar');
$pitr->safe_psql('postgres', "CREATE TABLE ct $cols USING columnar");
$pitr->safe_psql('postgres',
	"SELECT kingres.columnar_set_options('ct', stripe_row_count => 2000, chunk_row_count => 500)");
$pitr->safe_psql('postgres', gen('ct', 1, 3000));
$pitr->backup('base');

$pitr->safe_psql('postgres', gen('ct', 3001, 9000));
$pitr->safe_psql('postgres', gen('ct', 9001, 12000));
$pitr->safe_psql('postgres', "BEGIN; " . gen('ct', 500001, 520000) . "; ROLLBACK");
my $fp_rp = $pitr->safe_psql('postgres', fp('ct'));
$pitr->safe_psql('postgres', "SELECT pg_create_restore_point('rp1')");
$pitr->safe_psql('postgres', gen('ct', 12001, 20000));
my $fp_lsn = $pitr->safe_psql('postgres', fp('ct'));
my $lsn = $pitr->safe_psql('postgres', 'SELECT pg_current_wal_lsn()');
$pitr->safe_psql('postgres', gen('ct', 20001, 26000));
$pitr->safe_psql('postgres', "SELECT kingres.columnar_compact('ct', 1000)");
$pitr->safe_psql('postgres', gen('ct', 26001, 30000));
my $fp_end = $pitr->safe_psql('postgres', fp('ct'));
like($fp_rp,  qr/^12000\|/, 'rows at the restore point');
like($fp_lsn, qr/^20000\|/, 'rows at the LSN');
like($fp_end, qr/^30000\|/, 'rows at the end');
my $last_wal = $pitr->safe_psql('postgres', 'SELECT pg_walfile_name(pg_switch_wal())');
$pitr->poll_query_until('postgres',
	"SELECT last_archived_wal >= '$last_wal' FROM pg_stat_archiver")
  or die 'WAL not archived';

for my $case (
	[ 'pitr_name', "recovery_target_name = 'rp1'", $fp_rp ],
	[ 'pitr_lsn',  "recovery_target_lsn = '$lsn'", $fp_lsn ],
	[ 'pitr_end',  "recovery_target = 'immediate'", undef ],
	[ 'pitr_all',  "", $fp_end ])
{
	my ($name, $target, $want) = @$case;
	my $n = restored_node($name, $pitr, 'base',
		init => { has_restoring => 1, standby => 0 },
		# the copied configuration archives into the source's directory
		conf => "archive_mode = off\n$target\nrecovery_target_action = 'promote'\n");
	$n->start;
	$n->poll_query_until('postgres', 'SELECT NOT pg_is_in_recovery()')
	  or die "$name: recovery did not finish";
	if (defined $want)
	{
		is($n->safe_psql('postgres', fp('ct')), $want, "$name: the state at the target");
	}
	else
	{
		like($n->safe_psql('postgres', fp('ct')), qr/^3000\|/, "$name: the state of the base backup");
	}
	is(check_rows($n), '', "$name: columnar_check clean");
	# the recovered node is writable and its counters moved past the old stripes
	$n->safe_psql('postgres', gen('ct', 700001, 704000));
	is($n->safe_psql('postgres', 'SELECT count(*) FROM ct WHERE id > 700000'), '4000',
		"$name: a load after recovery");
	$n->stop;
}

# without the library the same recovery cannot reach its target
{
	my $n = restored_node('pitr_nolib', $pitr, 'base',
		init => { has_restoring => 1, standby => 0 },
		conf => "archive_mode = off\nshared_preload_libraries = ''\nrecovery_target_name = 'rp1'\nrecovery_target_action = 'promote'\n");
	my $ok = $n->start(fail_ok => 1);
	ok(!$ok, 'archive recovery without the preloaded library does not succeed');
	like(slurp_file($n->logfile),
		qr/resource manager|recovery ended before configured recovery target|KingresColumnar/,
		'  and the log says why');
}

###############################################################################
# c. incremental backup and pg_combinebackup
###############################################################################

my $inc = new_node('inc', conf => "summarize_wal = on\n");
$inc->safe_psql('postgres', 'CREATE EXTENSION kingres_columnar');
$inc->safe_psql('postgres', "CREATE TABLE ct $cols USING columnar");
$inc->safe_psql('postgres',
	"SELECT kingres.columnar_set_options('ct', stripe_row_count => 3000, chunk_row_count => 500)");
$inc->safe_psql('postgres', "CREATE TABLE untouched $cols USING columnar");
$inc->safe_psql('postgres', gen('untouched', 1, 4000));
$inc->safe_psql('postgres', gen('ct', 1, 10000));
$inc->safe_psql('postgres', "BEGIN; " . gen('ct', 800001, 830000) . "; ROLLBACK");
$inc->safe_psql('postgres', 'VACUUM ct');
$inc->backup('full');
my $fp_full = $inc->safe_psql('postgres', fp('ct'));

# the load goes beyond the end of the file of the full backup
$inc->safe_psql('postgres', gen('ct', 10001, 40000));
$inc->safe_psql('postgres', "CREATE TABLE added $cols USING columnar");
$inc->safe_psql('postgres', gen('added', 1, 5000));
$inc->backup('incr1',
	backup_options => [ '--incremental', $inc->backup_dir . '/full/backup_manifest' ]);
my $fp_inc1 = $inc->safe_psql('postgres', fp('ct'));
my $size_inc1 = $inc->safe_psql('postgres', "SELECT pg_relation_size('ct')");

# the second increment: a load, a rolled-back load and a truncating VACUUM
$inc->safe_psql('postgres', gen('ct', 40001, 52000));
$inc->safe_psql('postgres', "BEGIN; " . gen('ct', 900001, 960000) . "; ROLLBACK");
$inc->safe_psql('postgres', 'VACUUM ct');
$inc->safe_psql('postgres', gen('ct', 52001, 53000));
$inc->backup('incr2',
	backup_options => [ '--incremental', $inc->backup_dir . '/incr1/backup_manifest' ]);
my $fp_inc2 = $inc->safe_psql('postgres', fp('ct'));
my $size_inc2 = $inc->safe_psql('postgres', "SELECT pg_relation_size('ct')");
my $fp_added = $inc->safe_psql('postgres', fp('added'));
my $fp_untouched = $inc->safe_psql('postgres', fp('untouched'));

for my $case (
	[ 'comb1', ['full', 'incr1'], 'incr1', $fp_inc1, $size_inc1 ],
	[ 'comb2', ['full', 'incr1', 'incr2'], 'incr2', $fp_inc2, $size_inc2 ])
{
	my ($name, $chain, $last, $want, $size) = @$case;
	my @prior = @$chain;
	pop @prior;
	my $n = PostgreSQL::Test::Cluster->new($name);
	$n->init_from_backup($inc, $last, combine_with_prior => \@prior);
	$n->start;
	is($n->safe_psql('postgres', fp('ct')), $want,
		"$name: combined backup has the rows of its end");
	is($n->safe_psql('postgres', "SELECT pg_relation_size('ct')"), $size,
		"$name: file length equals the source's at the backup");
	is(check_rows($n), '', "$name: columnar_check clean");
	is($n->safe_psql('postgres', fp('untouched')), $fp_untouched, "$name: an unchanged table");
	is($n->safe_psql('postgres', fp('added')), $fp_added, "$name: a table created after the full backup")
	  if $name eq 'comb2';
	$n->safe_psql('postgres', gen('ct', 1000001, 1003000));
	is($n->safe_psql('postgres', 'SELECT count(*) FROM ct WHERE id > 1000000'), '3000',
		"$name: a load after the restore");
	$n->stop;
}
# the full backup alone is still what it was
{
	my $n = restored_node('comb0', $inc, 'full');
	$n->start;
	is($n->safe_psql('postgres', fp('ct')), $fp_full, 'the full backup itself is intact');
	$n->stop;
}

###############################################################################
# d. pg_dump and restore into another node; e. COPY
###############################################################################

my $codecs = $src->safe_psql('postgres',
	"SELECT string_agg(e, ',') FROM unnest((SELECT enumvals FROM pg_settings WHERE name = 'kingres_columnar.compression')) e");
my $src_codec = $codecs =~ /zstd/ ? 'zstd' : 'pglz';
my $dst_codec = $codecs =~ /lz4/ ? 'lz4' : 'none';
note "codecs: $codecs; source table uses $src_codec, restoring node defaults to $dst_codec";

my $dsrc = new_node('dump_src');
my $ddst = new_node('dump_dst',
	conf => "kingres_columnar.compression = '$dst_codec'\nkingres_columnar.compression_level = 5\n"
	  . "kingres_columnar.stripe_row_count = 20000\nkingres_columnar.chunk_row_count = 2000\n");

$dsrc->safe_psql('postgres', 'CREATE EXTENSION kingres_columnar');
$dsrc->safe_psql('postgres', <<"EOS");
CREATE TABLE ct $cols USING columnar;
SELECT kingres.columnar_set_options('ct', compression => '$src_codec',
       compression_level => 9, stripe_row_count => 5000, chunk_row_count => 1000);
CREATE TABLE ty $tycols USING columnar;
CREATE TABLE ect (id int, v text) USING columnar;
CREATE TABLE h $cols;
CREATE TABLE pt (k int, v text) PARTITION BY RANGE (k);
CREATE TABLE pt1 PARTITION OF pt FOR VALUES FROM (0) TO (1000);
CREATE TABLE pt2 PARTITION OF pt FOR VALUES FROM (1000) TO (2000) USING columnar;
CREATE TABLE pt3 PARTITION OF pt FOR VALUES FROM (2000) TO (3000) USING columnar;
EOS
$dsrc->safe_psql('postgres', gen('ct', 1, 25000));
$dsrc->safe_psql('postgres', gen('ct', 25001, 50000));
$dsrc->safe_psql('postgres', gen_ty(1, 6000));
$dsrc->safe_psql('postgres', gen('h', 1, 5000));
$dsrc->safe_psql('postgres',
	"INSERT INTO pt SELECT i % 3000, 'v' || i FROM generate_series(1, 9000) i");

my %want_fp = (
	ct => fp('ct'), ty => fp_any('ty'), ect => fp_any('ect'), h => fp('h'),
	pt => fp_any('pt'), pt1 => fp_any('pt1'), pt2 => fp_any('pt2'), pt3 => fp_any('pt3'));
my %src_fp;
$src_fp{$_} = $dsrc->safe_psql('postgres', $want_fp{$_}) for sort keys %want_fp;
like($src_fp{ct}, qr/^50000\|/, 'dump source loaded');
my $src_opts = $dsrc->safe_psql('postgres', "SELECT * FROM kingres.columnar_options('ct')");
like($src_opts, qr/^\Q$src_codec\E\|9\|5000\|1000$/, "source options are $src_codec/9/5000/1000");

my $tmp = PostgreSQL::Test::Utils::tempdir;

sub check_restored
{
	my ($db, $label, %o) = @_;
	for my $t (sort keys %want_fp)
	{
		is($ddst->safe_psql($db, $want_fp{$t}), $src_fp{$t}, "$label: $t has the same rows");
	}
	is($ddst->safe_psql($db,
			"SELECT string_agg(relname || '=' || amname, ',' ORDER BY relname) FROM pg_class c JOIN pg_am a ON a.oid = c.relam WHERE relname IN ('ct','ty','ect','h','pt1','pt2','pt3') AND relkind = 'r'"),
		'ct=columnar,ect=columnar,h=heap,pt1=heap,pt2=columnar,pt3=columnar,ty=columnar',
		"$label: access methods are kept");
	# options are not in a dump: the restoring session's defaults apply
	is($ddst->safe_psql($db, "SELECT * FROM kingres.columnar_options('ct')"),
		"$dst_codec|5|20000|2000", "$label: ct has the options of the restoring node, not the source's");
	is($ddst->safe_psql($db, "SELECT * FROM kingres.columnar_options('ect')"),
		"$dst_codec|5|20000|2000", "$label: ect too");
	is(check_rows($ddst, $db), '', "$label: columnar_check clean");
	is($ddst->safe_psql($db, "SELECT count(*) FROM kingres.columnar_stripe"),
		$ddst->safe_psql($db, "SELECT count(*) FROM kingres.columnar_stripe s WHERE filenode IN (SELECT relfilenode FROM pg_class)"),
		"$label: every stripe row belongs to a table");
}

# plain SQL, COPY
my $plain = "$tmp/plain.sql";
$dsrc->command_ok([ 'pg_dump', '-Fp', '-f', $plain, '-d', 'postgres' ], 'pg_dump plain');
my $dump = slurp_file($plain);
like($dump, qr/default_table_access_method = columnar;(?:(?!SET default).)*?CREATE TABLE \w+\.ct /s, 'the dump selects the columnar access method for ct');
like($dump, qr/default_table_access_method = heap;(?:(?!SET default).)*?CREATE TABLE \w+\.h /s, '  and heap for h');
like($dump, qr/CREATE EXTENSION IF NOT EXISTS kingres_columnar/, 'the dump creates the extension');
unlike($dump, qr/columnar_stripe/, 'the stripe catalog is not in the dump');
unlike($dump, qr/\\\.\n.*stripe/s, 'no stripe rows in the dump');
like($dump, qr/COPY \w+\.ct \(/, 'rows are copied with COPY');
$ddst->safe_psql('postgres', 'CREATE DATABASE r_plain');
$ddst->command_ok([ 'psql', '-X', '-q', '-v', 'ON_ERROR_STOP=1', '-d', 'r_plain', '-f', $plain ], 'restore plain');
check_restored('r_plain', 'plain COPY');
# one COPY statement is one transaction: stripes of the target's row count
is($ddst->safe_psql('r_plain', "SELECT count(*) FROM kingres.columnar_stripes('ct')"), '3',
	'plain COPY: 50000 rows became 3 stripes of the target size');
# the restored table is usable
$ddst->safe_psql('r_plain', gen('ct', 50001, 50100));
is($ddst->safe_psql('r_plain', 'SELECT count(*) FROM ct'), '50100', 'the restored table takes new rows');

# INSERT statements (several rows per statement)
my $ins = "$tmp/ins.sql";
$dsrc->command_ok([ 'pg_dump', '-Fp', '--rows-per-insert=500', '-f', $ins, '-d', 'postgres' ], 'pg_dump --rows-per-insert');
like(slurp_file($ins), qr/INSERT INTO \w+\.ct (?:\(.*?\) )?VALUES/s, 'rows are INSERT statements');
$ddst->safe_psql('postgres', 'CREATE DATABASE r_ins');
$ddst->command_ok([ 'psql', '-X', '-q', '-v', 'ON_ERROR_STOP=1', '-1', '-d', 'r_ins', '-f', $ins ], 'restore INSERT dump in one transaction');
check_restored('r_ins', 'INSERT');

# plain --inserts, one row per statement, for the table of many types
my $ins1 = "$tmp/ins1.sql";
$dsrc->command_ok([ 'pg_dump', '-Fp', '--inserts', '-t', 'ty', '-t', 'ect', '-f', $ins1, '-d', 'postgres' ], 'pg_dump --inserts');
$ddst->safe_psql('postgres', 'CREATE DATABASE r_ins1');
$ddst->safe_psql('r_ins1', 'CREATE EXTENSION kingres_columnar');
$ddst->command_ok([ 'psql', '-X', '-q', '-v', 'ON_ERROR_STOP=1', '-d', 'r_ins1', '-f', $ins1 ], 'restore --inserts');
is($ddst->safe_psql('r_ins1', fp_any('ty')), $src_fp{ty}, '--inserts: ty has the same rows');

# custom format
my $custom = "$tmp/dump.custom";
$dsrc->command_ok([ 'pg_dump', '-Fc', '-f', $custom, '-d', 'postgres' ], 'pg_dump custom');
$ddst->safe_psql('postgres', 'CREATE DATABASE r_custom');
$ddst->command_ok([ 'pg_restore', '-d', 'r_custom', '--exit-on-error', $custom ], 'pg_restore custom');
check_restored('r_custom', 'custom');

# directory format, parallel dump and restore
my $dir = "$tmp/dump.dir";
$dsrc->command_ok([ 'pg_dump', '-Fd', '-j', '2', '-f', $dir, '-d', 'postgres' ], 'pg_dump directory');
$ddst->safe_psql('postgres', 'CREATE DATABASE r_dir');
$ddst->command_ok([ 'pg_restore', '-j', '2', '-d', 'r_dir', '--exit-on-error', $dir ], 'pg_restore directory, 2 jobs');
check_restored('r_dir', 'directory');

# schema only, and the extension alone
my $schema = "$tmp/schema.sql";
$dsrc->command_ok([ 'pg_dump', '-Fp', '--schema-only', '-f', $schema, '-d', 'postgres' ], 'pg_dump --schema-only');
$ddst->safe_psql('postgres', 'CREATE DATABASE r_schema');
$ddst->command_ok([ 'psql', '-X', '-q', '-v', 'ON_ERROR_STOP=1', '-d', 'r_schema', '-f', $schema ], 'restore schema only');
is($ddst->safe_psql('r_schema', "SELECT count(*) FROM ct") . '/' . $ddst->safe_psql('r_schema', "SELECT count(*) FROM pt"),
	'0/0', 'schema-only restore: empty tables');
is($ddst->safe_psql('r_schema', "SELECT amname FROM pg_class c JOIN pg_am a ON a.oid = c.relam WHERE relname = 'pt3'"),
	'columnar', 'schema-only restore keeps USING columnar');
$ddst->safe_psql('r_schema', gen('ct', 1, 3000));
is($ddst->safe_psql('r_schema', 'SELECT count(*) FROM ct'), '3000', 'a schema-only restored table takes rows');

my $extdump = "$tmp/ext.sql";
$dsrc->command_ok([ 'pg_dump', '-Fp', '--extension=kingres_columnar', '-f', $extdump, '-d', 'postgres' ], 'pg_dump --extension');
$ddst->safe_psql('postgres', 'CREATE DATABASE r_ext');
$ddst->command_ok([ 'psql', '-X', '-q', '-v', 'ON_ERROR_STOP=1', '-d', 'r_ext', '-f', $extdump ], 'restore the extension dump');
is($ddst->safe_psql('r_ext', "SELECT count(*) FROM pg_am WHERE amname = 'columnar'"), '1', 'the access method exists');
$ddst->safe_psql('r_ext', "CREATE TABLE x (i int) USING columnar; INSERT INTO x VALUES (1)");

# the empty table alone
my $emp = "$tmp/empty.sql";
$dsrc->command_ok([ 'pg_dump', '-Fp', '-t', 'ect', '-f', $emp, '-d', 'postgres' ], 'pg_dump of an empty columnar table');
$ddst->safe_psql('postgres', 'CREATE DATABASE r_empty');
$ddst->safe_psql('r_empty', 'CREATE EXTENSION kingres_columnar');
$ddst->command_ok([ 'psql', '-X', '-q', '-v', 'ON_ERROR_STOP=1', '-d', 'r_empty', '-f', $emp ], 'restore the empty table');
is($ddst->safe_psql('r_empty', 'SELECT count(*) FROM ect'), '0', 'the empty table is empty');
$ddst->safe_psql('r_empty', "INSERT INTO ect SELECT i, 'v' FROM generate_series(1, 10) i");
is($ddst->safe_psql('r_empty', 'SELECT count(*) FROM ect'), '10', 'and takes rows');

# e. COPY to a file and back, in each format
for my $fmt ('text', 'csv', 'binary')
{
	my $f = "$tmp/copy.$fmt";
	my $opt = $fmt eq 'csv' ? "FORMAT csv, HEADER" : "FORMAT $fmt";
	$dsrc->safe_psql('postgres', "COPY ct TO '$f' WITH ($opt)");
	$dsrc->safe_psql('postgres', "COPY ty TO '$f.ty' WITH ($opt)");
	$dsrc->safe_psql('postgres', "CREATE TABLE ct_$fmt $cols USING columnar; CREATE TABLE ty_$fmt $tycols USING columnar");
	$dsrc->safe_psql('postgres', "COPY ct_$fmt FROM '$f' WITH ($opt)");
	$dsrc->safe_psql('postgres', "COPY ty_$fmt FROM '$f.ty' WITH ($opt)");
	is($dsrc->safe_psql('postgres', fp("ct_$fmt")), $src_fp{ct}, "COPY round trip, $fmt: ct");
	is($dsrc->safe_psql('postgres', fp_any("ty_$fmt")), $src_fp{ty}, "COPY round trip, $fmt: ty");
	# and into a heap table, and out of a heap table into a columnar one
	$dsrc->safe_psql('postgres', "CREATE TABLE hh_$fmt $cols; COPY hh_$fmt FROM '$f' WITH ($opt)");
	is($dsrc->safe_psql('postgres', fp("hh_$fmt")), $src_fp{ct}, "COPY columnar to heap, $fmt");
}
$dsrc->safe_psql('postgres', "COPY ect TO '$tmp/ect.bin' WITH (FORMAT binary)");
$dsrc->safe_psql('postgres', "COPY ect FROM '$tmp/ect.bin' WITH (FORMAT binary)");
is($dsrc->safe_psql('postgres', 'SELECT count(*) FROM ect'), '0', 'COPY of an empty columnar table');
$dsrc->stop;
$ddst->stop;

###############################################################################
# g. CREATE DATABASE ... TEMPLATE
###############################################################################
#
# The stripe catalog is a table of the template database and is copied with
# it, the relation files are copied block by block; both have the same
# relfilenodes in the copy.  The WAL_LOG strategy copies through the buffer
# manager and logs every page as a standard page, which leaves out the space
# between pd_lower and pd_upper: a stripe page whose payload sits in that
# space is zeroed by replay.

{
	my $tp = new_node('tpl');
	$tp->safe_psql('postgres', 'CREATE DATABASE tpl');
	$tp->safe_psql('tpl', 'CREATE EXTENSION kingres_columnar');
	$tp->safe_psql('tpl', "CREATE TABLE ct $cols USING columnar");
	$tp->safe_psql('tpl',
		"SELECT kingres.columnar_set_options('ct', compression => 'pglz', stripe_row_count => 4000, chunk_row_count => 1000)");
	$tp->safe_psql('tpl', gen('ct', 1, 14000));
	$tp->safe_psql('tpl', "CREATE TABLE ty $tycols USING columnar");
	$tp->safe_psql('tpl', gen_ty(1, 3000));
	$tp->safe_psql('tpl', "CREATE TABLE h $cols");
	$tp->safe_psql('tpl', gen('h', 1, 100));
	$tp->backup('b');
	my $ts = PostgreSQL::Test::Cluster->new('tpl_standby');
	$ts->init_from_backup($tp, 'b', has_streaming => 1);
	$ts->append_conf('postgresql.conf', "hot_standby = on\n");
	$ts->start;

	my $t_fp = $tp->safe_psql('tpl', fp('ct'));
	my $t_ty = $tp->safe_psql('tpl', fp_any('ty'));
	my $t_opt = $tp->safe_psql('tpl', "SELECT * FROM kingres.columnar_options('ct')");

	$tp->safe_psql('postgres', 'CREATE DATABASE cp_file TEMPLATE tpl STRATEGY = FILE_COPY');
	$tp->command_ok([ 'createdb', '--template=tpl', 'cp_cli' ], 'createdb --template');
	$tp->safe_psql('postgres', 'CREATE DATABASE cp_wal TEMPLATE tpl STRATEGY = WAL_LOG');
	$tp->wait_for_catchup($ts);

	for my $db ('cp_file', 'cp_cli', 'cp_wal')
	{
		is($tp->safe_psql($db, fp('ct')), $t_fp, "$db: rows on the primary");
		is($tp->safe_psql($db, fp_any('ty')), $t_ty, "$db: ty on the primary");
		is($tp->safe_psql($db, "SELECT * FROM kingres.columnar_options('ct')"), $t_opt, "$db: options copied");
		is(check_rows($tp, $db), '', "$db: columnar_check clean on the primary");
		my ($ret, $out, $err) = $ts->psql($db, fp('ct'));
		is("$out$err", $t_fp, "$db: rows on the standby");
		($ret, $out, $err) = $ts->psql($db, fp_any('ty'));
		is("$out$err", $t_ty, "$db: ty on the standby");
	}

	# the copy and the original are independent afterwards
	$tp->safe_psql('cp_wal', gen('ct', 14001, 15000));
	$tp->safe_psql('cp_wal', 'VACUUM ct');
	$tp->safe_psql('tpl', gen('ct', 14001, 14500));
	is($tp->safe_psql('cp_wal', 'SELECT count(*) FROM ct'), '15000', 'the copy takes rows');
	is($tp->safe_psql('tpl', 'SELECT count(*) FROM ct'), '14500', 'the template is not changed by the copy');
	$tp->safe_psql('postgres', 'DROP DATABASE cp_file');
	is($tp->safe_psql('cp_cli', 'SELECT count(*) FROM ct'), '14000', 'dropping one copy leaves the others');
	$tp->wait_for_catchup($ts);

	# crash recovery replays the log of a WAL_LOG copy made after the last
	# checkpoint
	$tp->safe_psql('postgres', 'CREATE DATABASE cp_wal2 TEMPLATE tpl STRATEGY = WAL_LOG');
	$tp->stop('immediate');
	$tp->start;
	my ($ret, $out, $err) = $tp->psql('cp_wal2', fp('ct'));
	is("$out$err", $tp->safe_psql('tpl', fp('ct')), 'cp_wal2 after crash recovery of the primary');
	$ts->stop;
	$tp->stop;
}

done_testing();
