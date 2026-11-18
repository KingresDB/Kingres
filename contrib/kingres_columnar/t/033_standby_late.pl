# contrib/kingres_columnar/t/033_standby_late.pl
#
# The write paths of the last rounds on a streaming standby, across a promotion, with
# wal_consistency_checking = all on both nodes (every full-page image that redo
# rebuilds is compared with the page the primary logged): rows written after the
# commit-time flush (stripes of one row), rows written between the two phases of a
# hand-over, a table truncated in place by a callback of another module at the
# pre-commit (relation_nontransactional_truncate), a transaction that failed
# because a call was refused while it was still alive, and files dropped after the
# clean-up.  The standby must hold what the primary holds, replay without a
# consistency failure, and after promotion the tables read, check clean and take
# writes.  A reader on the hot standby that holds the table which is dropped late (C9)
# is ended by the standby as for any other DROP (max_standby_streaming_delay), the
# drop and the clean-up of the next transaction replay, and the rest of the standby
# goes on reading.
#
# Needs a compiler and the server's headers (KcProbe.pm).
#
# Portions Copyright (c) 2026, RooyeKhat Media Co
# SPDX-License-Identifier: AGPL-3.0-only

use strict;
use warnings FATAL => 'all';

use FindBin;
use lib $FindBin::RealBin;
use KcProbe qw(build_module);
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;

my $probe = build_module('kcprobe');
my $events = build_module('kcevents');

my $primary = PostgreSQL::Test::Cluster->new('colstby_primary');
$primary->init(allows_streaming => 1);
$primary->append_conf('postgresql.conf', "shared_preload_libraries = '$probe/kcprobe,$events/kcevents,kingres_columnar'\n"
	. "wal_consistency_checking = 'all'\nmax_prepared_transactions = 5\n");
$primary->start;
$primary->safe_psql('postgres', qq{
CREATE EXTENSION kingres_columnar;
CREATE FUNCTION kcevents_open(text) RETURNS void AS '$events/kcevents', 'kcevents_open' LANGUAGE C;
CREATE FUNCTION kcevents_crit() RETURNS void AS '$events/kcevents', 'kcevents_crit' LANGUAGE C;
CREATE TABLE late (a int) USING columnar;
CREATE TABLE win (a int) USING columnar;
SELECT kingres.columnar_set_options('win', stripe_row_count => 1000, chunk_row_count => 100);
CREATE TABLE trunc (a int) USING columnar;
CREATE TABLE lost (a int) USING columnar;
CREATE TABLE gone (a int) USING columnar;
INSERT INTO gone SELECT generate_series(1, 100);
INSERT INTO trunc SELECT generate_series(1, 50);});

$primary->backup('b');
my $standby = PostgreSQL::Test::Cluster->new('colstby_standby');
$standby->init_from_backup($primary, 'b', has_streaming => 1);
$standby->start;

sub run
{
	my ($sql) = @_;
	my ($out, $err) = ('', '');
	$primary->psql('postgres', $sql, stdout => \$out, stderr => \$err, on_error_stop => 0);
	return ($out, $err);
}

# rows written one at a time after the commit-time flush, and in the window of a hand-over
run("BEGIN; SET LOCAL kcprobe.commit_sql = 'INSERT INTO late SELECT generate_series(1, 300)'; INSERT INTO late VALUES (0); COMMIT;");
run("BEGIN; INSERT INTO win SELECT generate_series(1, 999); SAVEPOINT s; INSERT INTO win VALUES (10001);
SET kcprobe.sql = 'COPY win (a) FROM PROGRAM \$\$seq 20001 20007\$\$'; RELEASE s; SET kcprobe.sql = ''; COMMIT;");

# a table emptied in place at the pre-commit by another module
# (only a table given a new file in the transaction is emptied in place, C10-05: the transaction truncates it, writes
# it, and the module empties it)
run("SET kcevents.at = 'pre_commit'; SET kcevents.op = 'ntrunc'; BEGIN; TRUNCATE trunc; INSERT INTO trunc SELECT generate_series(1, 50); SELECT kcevents_open('trunc'); COMMIT;");

# a transaction that lost a call while it was alive fails; one that rolls the loss back commits
my ($o, $e) = run("SET kcevents.at = 'commit_sub'; BEGIN; SELECT kcevents_open('lost'); SAVEPOINT s; INSERT INTO lost VALUES (1); RELEASE s; COMMIT;");
like($e, qr/was refused earlier in this transaction, so it cannot commit/, 'the transaction that lost a call while alive failed');
run("SET kcevents.at = 'commit_sub'; BEGIN; SELECT kcevents_open('lost'); SAVEPOINT s; INSERT INTO lost VALUES (2); RELEASE s; ROLLBACK;");
run("INSERT INTO lost VALUES (3)");

# a hot standby reader of the table that is dropped late: it reads the table before the drop (the
# standby serves it) and holds it open across the drop
$standby->append_conf('postgresql.conf', "max_standby_streaming_delay = '1s'\n");
$standby->reload;
is($standby->safe_psql('postgres', 'SELECT count(*) FROM gone'), 100, 'hot standby: the table that is about to be dropped late reads on the standby');
my $reader = $standby->background_psql('postgres', on_error_stop => 0);
$reader->query('BEGIN; SELECT count(*) FROM gone;');

# a file dropped after the clean-up, and the next transaction of the session
run("SET event_triggers = off; BEGIN; SET LOCAL kcprobe.commit_sql = 'DROP TABLE gone'; SELECT 1; COMMIT; SELECT 1; SELECT 1;");

$primary->wait_for_catchup($standby);

# the drop replayed over the reader (it was cancelled after the delay), and the stripe rows of the
# dropped file are gone on the standby too
$standby->poll_query_until('postgres', "SELECT to_regclass('public.gone') IS NULL");
is($standby->safe_psql('postgres', "SELECT to_regclass('public.gone') IS NULL"), 't', 'hot standby: the late drop replayed under a reader');
is($standby->safe_psql('postgres', 'SELECT pg_is_in_recovery()'), 't', 'hot standby: the standby is still in recovery and answering');
is($standby->safe_psql('postgres', 'SELECT count(*) FROM kingres.columnar_stripe WHERE rel_uid NOT IN (SELECT kingres.columnar_identity(c.oid) FROM pg_class c JOIN pg_am a ON a.oid = c.relam WHERE a.amname = \'columnar\')'), 0,
	'hot standby: no stripe row of the dropped file is left');
like(slurp_file($standby->logfile), qr/conflict with recovery/, 'hot standby: the reader was ended by the conflict with the drop, as for any table');
eval { $reader->quit; };

foreach my $t ('late', 'win', 'trunc', 'lost')
{
	my $sql = "SELECT count(*) || ':' || coalesce(sum(a), 0) FROM $t";
	is($standby->safe_psql('postgres', $sql), $primary->safe_psql('postgres', $sql), "$t: the standby holds what the primary holds");
}
is($primary->safe_psql('postgres', 'SELECT count(*) FROM late'), 301, 'late: all rows written after the flush are there');
is($primary->safe_psql('postgres', 'SELECT count(*) FROM trunc'), 0, 'trunc: emptied in place');
is($primary->safe_psql('postgres', 'SELECT count(*) FROM lost'), 1, 'lost: only the committed row');
is($primary->safe_psql('postgres', 'SELECT count(*) FROM kingres.columnar_check()'), 0, 'columnar_check on the primary: nothing to report');
is($standby->safe_psql('postgres', 'SELECT count(*) FROM kingres.columnar_check()'), 0, 'columnar_check on the standby: nothing to report');

$standby->promote;
is($standby->safe_psql('postgres', 'SELECT count(*) FROM kingres.columnar_check()'), 0, 'after promotion: nothing to report');
$standby->safe_psql('postgres', 'INSERT INTO late SELECT generate_series(1, 10); INSERT INTO trunc VALUES (1); VACUUM late; SELECT kingres.columnar_compact(\'late\', 10)');
is($standby->safe_psql('postgres', 'SELECT count(*) FROM late'), 311, 'after promotion: the table takes writes, VACUUM and compaction');
unlike(slurp_file($standby->logfile), qr/inconsistent page|PANIC/, 'replay found no inconsistent page');
unlike(slurp_file($primary->logfile), qr/PANIC|TRAP:/, 'the primary logged no PANIC');

$standby->stop;
$primary->stop;
done_testing();
