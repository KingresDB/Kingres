# SPDX-License-Identifier: AGPL-3.0-only
# contrib/kingres_columnar/t/034_decoding.pl
#
# Logical decoding of transactions that had a call refused at a moment where the
# server forbids an error (the refusals of C7-01, C8-01, C8-02 and C9-01).  The
# columnar rows themselves are not decoded (they are not heap records; the README says
# so, and a publication of a columnar table is refused), but the transaction's other
# changes are, and so are the changes of the stripe catalog (kingres.columnar_stripe,
# an ordinary heap table), which is where a refusal could show: a refused call must leave
# no catalog change behind, a transaction that failed because of a refusal must decode to
# nothing, and what is decoded for a committed transaction must agree with what the
# table holds.  The test decodes with test_decoding and compares with the table.
#
# Needs a compiler and the server's headers (KcProbe.pm) and the test_decoding module
# of the installation; without it the file is skipped, with the reason.
#
# Portions Copyright (c) 2026, RooyeKhat Media Co

use strict;
use warnings FATAL => 'all';

use FindBin;
use lib $FindBin::RealBin;
use KcProbe qw(build_module);
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;

# a case that fails because the code under test is wrong must not hang the file on a lock that is never released
$ENV{PGOPTIONS} = '-c statement_timeout=240s';

my $dir = build_module('kcevents');

my $node = PostgreSQL::Test::Cluster->new('coldecode');
$node->init;
$node->append_conf('postgresql.conf', "shared_preload_libraries = '$dir/kcevents,kingres_columnar'\nwal_level = logical\nmax_prepared_transactions = 5\n");
$node->start;
$node->safe_psql('postgres', qq{
CREATE EXTENSION kingres_columnar;
CREATE FUNCTION kcevents_open(text) RETURNS void AS '$dir/kcevents', 'kcevents_open' LANGUAGE C;
CREATE FUNCTION kcevents_crit() RETURNS void AS '$dir/kcevents', 'kcevents_crit' LANGUAGE C;
CREATE TABLE t (a int) USING columnar;
CREATE TABLE hp (a int PRIMARY KEY);});

my ($o, $e) = ('', '');
$node->psql('postgres', "SELECT pg_create_logical_replication_slot('s', 'test_decoding')", stdout => \$o, stderr => \$e, on_error_stop => 0);
if ($e =~ /could not access file|does not exist|test_decoding/)
{
	$node->stop;
	plan skip_all => 'the test_decoding module is not installed in this installation';
}
is($e, '', 'a logical slot with test_decoding is made');

sub run
{
	my ($sql) = @_;
	my ($out, $err) = ('', '');

	$node->psql('postgres', $sql, stdout => \$out, stderr => \$err, on_error_stop => 0);
	return ($out, $err);
}

# the changes since the last call, one string per transaction (BEGIN to COMMIT), as test_decoding prints them
sub decoded
{
	my $out = $node->safe_psql('postgres', "SELECT data FROM pg_logical_slot_get_changes('s', NULL, NULL, 'skip-empty-xacts', '1')");
	my @tx;
	my $cur;
	foreach my $l (split /\n/, $out)
	{
		if ($l =~ /^BEGIN/) { $cur = $l . "\n"; }
		elsif ($l =~ /^COMMIT/) { push @tx, $cur . $l . "\n"; undef $cur; }
		else { $cur .= $l . "\n" if defined $cur; }
	}
	return @tx;
}

my $stripe = qr/table kingres\.columnar_stripe: (INSERT|DELETE|UPDATE)/;

# 1. a refusal at XACT_EVENT_COMMIT (decided: the transaction commits without the module's row)
($o, $e) = run("SET kcevents.at = 'commit'; BEGIN; SELECT kcevents_open('t'); INSERT INTO t VALUES (100); INSERT INTO hp VALUES (1); COMMIT;");
like($e, qr/WARNING:  insert refused on columnar table "t": the transaction is not in progress/, 'a call refused at the commit');
my @tx = decoded();
is(scalar(@tx), 1, 'the transaction with a refused call decodes to one transaction');
like($tx[0], qr/table \w+\.hp: INSERT: a\[integer\]:1/, '... with the heap row');
my @st = $tx[0] =~ /table kingres\.columnar_stripe: INSERT:.*?row_count\[integer\]:(\d+)/g;
is(join(',', @st), '1', '... and one stripe of one row in the catalog: the user\'s row, not the refused one');
is($node->safe_psql('postgres', 'SELECT string_agg(a::text, \',\') FROM t'), '100', '... which is what the table holds');
unlike($tx[0], qr/table \w+\.t:/, '... and the columnar rows themselves are not decoded');

# 2. a refusal while the transaction is alive (the commit of a subtransaction): the transaction fails and decodes to nothing
($o, $e) = run("SET kcevents.at = 'commit_sub'; BEGIN; SELECT kcevents_open('t'); INSERT INTO hp VALUES (2); SAVEPOINT s; INSERT INTO t VALUES (200); RELEASE s; COMMIT;");
like($e, qr/was refused earlier in this transaction, so it cannot commit/, 'a call refused at the commit of a subtransaction fails the transaction');
@tx = decoded();
is(scalar(@tx), 0, '... which decodes to nothing: no heap row, no stripe');
is($node->safe_psql('postgres', 'SELECT count(*) FROM hp WHERE a = 2'), 0, '... and the heap row is not there');

# 3. a refusal in a critical section after the extension's own pre-commit (C9-01): committed without the call
($o, $e) = run("SET kcevents.at = 'pre_commit'; SET kcevents.crit = on; BEGIN; SELECT kcevents_open('t'); INSERT INTO hp VALUES (3); INSERT INTO t VALUES (300); COMMIT;");
like($e, qr/WARNING:  insert refused on columnar table "t": inside a critical section/, 'a call refused in a critical section after the extension\'s pre-commit');
@tx = decoded();
is(scalar(@tx), 1, '... the transaction committed and decodes');
like($tx[0], qr/table \w+\.hp: INSERT: a\[integer\]:3/, '... with its heap row');
@st = $tx[0] =~ /table kingres\.columnar_stripe: INSERT:.*?row_count\[integer\]:(\d+)/g;
is(join(',', @st), '1', '... and the one stripe of the user\'s row');
is($node->safe_psql('postgres', 'SELECT string_agg(a::text, \',\' ORDER BY a) FROM t'), '100,300', '... the table holds the user\'s rows and not the refused one');

# 4. a table emptied in place at the pre-commit by the module (relation_nontransactional_truncate): the catalog rows are deleted.
#    Only a table given a new file in the same transaction is emptied in place (C10-05), so the transaction truncates the
#    table and writes it before the module empties it: the stripe the commit wrote is inserted and deleted again.
($o, $e) = run("SET kcevents.at = 'pre_commit'; SET kcevents.op = 'ntrunc'; BEGIN; TRUNCATE t; INSERT INTO t SELECT generate_series(1, 5); SELECT kcevents_open('t'); COMMIT;");
unlike($e, qr/ERROR/, 'a truncate in place at the pre-commit of a table given a new file by the transaction: no error');
@tx = decoded();
is(scalar(@tx), 1, '... decodes');
like($tx[0], qr/table kingres\.columnar_stripe: DELETE/, '... with the delete of the stripe rows');
my @ins = $tx[0] =~ /table kingres\.columnar_stripe: INSERT:.*?row_count\[integer\]:(\d+)/g;
is(join(',', @ins), '5', '... and the one stripe the commit wrote, which the module then emptied');
is($node->safe_psql('postgres', 'SELECT count(*) FROM t'), 0, '... and the table is empty');
# a table that existed before the transaction is not touched (C10-05)
$node->safe_psql('postgres', 'INSERT INTO t VALUES (9)');
decoded();
($o, $e) = run("SET kcevents.at = 'pre_commit'; SET kcevents.op = 'ntrunc'; BEGIN; SELECT kcevents_open('t'); COMMIT;");
like($e, qr/ERROR:  cannot truncate columnar table "t" in place/, 'a truncate in place of a table that existed before the transaction is an error');
is(scalar(decoded()), 0, '... which decodes to nothing');
is($node->safe_psql('postgres', 'SELECT count(*) FROM t'), 1, '... and the table keeps its row');
$node->safe_psql('postgres', 'TRUNCATE t');
decoded();

# 5. an ordinary columnar transaction decodes to its stripes, and the stripe's rows are the rows of the table
($o, $e) = run("SET kcevents.at = ''; SET kcevents.op = 'insert'; BEGIN; INSERT INTO t SELECT generate_series(1, 1000); INSERT INTO hp VALUES (5); COMMIT;");
@tx = decoded();
is(scalar(@tx), 1, 'an ordinary transaction decodes');
@st = $tx[0] =~ /table kingres\.columnar_stripe: INSERT:.*?row_count\[integer\]:(\d+)/g;
my $sum = 0;
$sum += $_ foreach @st;
is($sum, $node->safe_psql('postgres', 'SELECT count(*) FROM t'), 'the rows of the decoded stripes are the rows of the table');

# 6. a prepared transaction that lost a call while alive is not prepared, and decodes to nothing
($o, $e) = run("SET kcevents.at = 'commit_sub'; BEGIN; SELECT kcevents_open('t'); SAVEPOINT s; INSERT INTO t VALUES (1); RELEASE s; PREPARE TRANSACTION 'x';");
like($e, qr/was refused earlier in this transaction, so it cannot commit/, 'a transaction that lost a call is not prepared');
is($node->safe_psql('postgres', 'SELECT count(*) FROM pg_prepared_xacts'), 0, '... and no prepared transaction is left');
@tx = decoded();
is(scalar(@tx), 0, '... and nothing is decoded');

is($node->safe_psql('postgres', 'SELECT count(*) FROM kingres.columnar_check()'), 0, 'columnar_check has nothing to report');
unlike(slurp_file($node->logfile), qr/PANIC|TRAP:|terminated by signal/, 'the server logged no PANIC');
$node->stop;
done_testing();
