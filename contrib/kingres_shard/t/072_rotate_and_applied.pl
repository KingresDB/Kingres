# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/072_rotate_and_applied.pl
#
# Review S8-03: kingres.rotate_secret() refuses while a distributed transaction of the
# cluster is in doubt (prepared, tag made with the secret of today): after the rotation
# recovery would ignore it for ever, it would hold its locks, and check_cluster would
# call it foreign.  A name whose tag does not verify is somebody else's and does not
# stop a rotation.
#
# Review S8-06: the rows of kingres.dtx_applied follow kingres_shard.decision_retention
# (plus a minute) and are removed with an index scan.

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(make_cluster keys_of_group run_sql);

sub prepared { return $_[0]->safe_psql('postgres', 'SELECT count(*) FROM pg_prepared_xacts'); }

my ($n1, $n2) = make_cluster('rs', 2);
$n1->safe_psql('postgres', q{
	CREATE TABLE public.kv (id int PRIMARY KEY, val text);
	SELECT kingres.create_distributed_table('public.kv', 'id', bucket_count => 6);
});
my ($ka) = keys_of_group($n1, 1, 0, 1);
my ($kb) = keys_of_group($n1, 1, 1, 1);

# --- S8-03: a transaction in doubt stops the rotation --------------------------------
my ($rc, $out, $err) = run_sql($n1,
	"SET kingres_shard.fault_injection = 'after_prepare:kill'; BEGIN; "
	  . "INSERT INTO public.kv VALUES ($ka, 'a'); INSERT INTO public.kv VALUES ($kb, 'b'); COMMIT;");
isnt($rc, 0, 'the origin dies after the PREPARE');
$n1->stop('immediate', fail_ok => 1);
$n1->start;
is(prepared($n2), '1', 'the participant holds a transaction in doubt');
my $secret = $n1->safe_psql('postgres', "SELECT kingres.internal_forward_token('postgres', 2)");
($rc, $out, $err) = run_sql($n1, "\\set VERBOSITY verbose\nSELECT kingres.rotate_secret()");
isnt($rc, 0, "rotate_secret refuses");
like($err, qr/distributed transactions are in doubt/, "and says why");
like($err, qr/KS013/, "with SQLSTATE KS013");
like($err, qr/node 2 \(1\)/, 'and where');
is($n1->safe_psql('postgres', "SELECT kingres.internal_forward_token('postgres', 2)"), $secret,
	'the secret is unchanged');
$n1->safe_psql('postgres', 'SELECT kingres.recover_prepared_transactions()');
is(prepared($n2), '0', 'recovery resolves it');
($rc, $out, $err) = run_sql($n1, 'SELECT kingres.rotate_secret()');
is($rc, 0, 'and then the secret can be rotated');
isnt($n1->safe_psql('postgres', "SELECT kingres.internal_forward_token('postgres', 2)"), $secret, 'to a new one');
is($n1->safe_psql('postgres', "SELECT count(*) FROM kingres.check_cluster() WHERE check_name = 'foreign_prepared_transaction'"),
	'0', 'and check_cluster has nothing to call foreign');

# A name of the right form with a tag that does not verify is not the cluster's own: it does not stop a rotation.
$n2->safe_psql('postgres',
	"SET kingres_shard.enable_routing = off; BEGIN; INSERT INTO public.kv VALUES ($kb, 'x'); "
	  . "PREPARE TRANSACTION 'kingres_h0_1_999_4000000000_2_0123456789abcdef'");
($rc, $out, $err) = run_sql($n1, 'SELECT kingres.rotate_secret()');
is($rc, 0, 'a forged name does not stop a rotation');
like($n1->safe_psql('postgres', "SELECT detail FROM kingres.check_cluster() WHERE check_name = 'foreign_prepared_transaction'"),
	qr/prepared in the moments around a kingres\.rotate_secret\(\)/, 'and check_cluster says what the name can be');
$n2->safe_psql('postgres', "ROLLBACK PREPARED 'kingres_h0_1_999_4000000000_2_0123456789abcdef'");

# --- S8-06: dtx_applied ----------------------------------------------------------------
is($n1->safe_psql('postgres', "SELECT count(*) FROM pg_indexes WHERE schemaname = 'kingres' AND indexname = 'dtx_applied_applied_at_idx'"),
	'1', 'dtx_applied has an index on applied_at');
$n1->safe_psql('postgres', q{
	INSERT INTO kingres.dtx_applied VALUES ('kingres_h_old', now() - interval '200 seconds'),
	                                       ('kingres_h_new', now() - interval '20 seconds')});
$n1->append_conf('postgresql.conf', "kingres_shard.decision_retention = 100\n");
$n1->reload;
$n1->safe_psql('postgres', 'SELECT kingres.recover_prepared_transactions()');
is($n1->safe_psql('postgres', 'SELECT string_agg(gid, $$,$$) FROM kingres.dtx_applied WHERE gid LIKE $$kingres_h\_%$$'), 'kingres_h_new',
	'the cleanup removes the rows older than decision_retention plus a minute (a day before) and keeps the others');
is($n1->safe_psql('postgres', q{SET enable_seqscan = off; EXPLAIN DELETE FROM kingres.dtx_applied WHERE applied_at < now() - interval '1 hour'}) =~ /dtx_applied_applied_at_idx/ ? 'index' : 'seq',
	'index', 'and the delete can use the index');

done_testing();
