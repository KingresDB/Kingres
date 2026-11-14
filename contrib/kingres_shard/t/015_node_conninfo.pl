# SPDX-License-Identifier: PostgreSQL
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# kingres_shard.node_conninfo: connection options appended to every node-to-node
# connection string.  The keywords the extension controls are rejected when the
# setting is made, the options are applied to the connections, and the values
# that name key material never appear in the messages of the extension.

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(make_cluster run_sql);

my ($n1, $n2) = make_cluster('ci', 2, tcp => 1);
$_->safe_psql('postgres', 'CREATE ROLE ci_plain LOGIN') for ($n1, $n2);
$n1->safe_psql('postgres', q{
	CREATE TABLE public.kv (id int PRIMARY KEY, val text);
	SELECT kingres.create_distributed_table('public.kv', 'id', bucket_count => 8);
});
my ($k_remote) = KingresShardTest::keys_of_group($n1, 1, 1, 1);
$n1->safe_psql('postgres', "INSERT INTO public.kv VALUES ($k_remote, 'remote-row')");

# ---------------------------------------------------------------------------
# Validation when the setting is made.
# ---------------------------------------------------------------------------
for my $kw (qw(host hostaddr port dbname user password options service client_encoding
	application_name connect_timeout replication))
{
	my ($rc, $out, $err) = run_sql($n1, "SET kingres_shard.node_conninfo = '$kw=somevalue'");
	isnt($rc, 0, "the keyword $kw is rejected");
	like($err, qr/keyword "$kw" is controlled by the extension/, "$kw: the message names the keyword");
	unlike($err, qr/somevalue/, "$kw: the message does not repeat the value");
}
{
	my ($rc, $out, $err) = run_sql($n1, "SET kingres_shard.node_conninfo = 'sslmode=disable host=elsewhere sslkey=/secret/keys/server.key'");
	isnt($rc, 0, 'a forbidden keyword among good ones is rejected');
	unlike($err, qr{/secret/keys}, 'the path of a key in a rejected value is not shown');
	($rc, $out, $err) = run_sql($n1, "SET kingres_shard.node_conninfo = 'this is not a connection string'");
	isnt($rc, 0, 'a value that is not a connection string is rejected');
	unlike($err, qr/this is not/, 'and not repeated');
	($rc, $out, $err) = run_sql($n1, "SET kingres_shard.node_conninfo = 'sslmode=require sslrootcert=/secret/ca/root.crt'");
	is($err, '', 'valid options are accepted');
}
{
	my ($rc, $out, $err) = run_sql($n1, "SET ROLE ci_plain; SET kingres_shard.node_conninfo = 'sslmode=disable'");
	like($err, qr/permission denied to set parameter "kingres_shard.node_conninfo"/, 'a role that is not a superuser cannot set it');
	($rc, $out, $err) = run_sql($n1, "SET ROLE ci_plain; SHOW kingres_shard.node_conninfo");
	like($err, qr/permission denied/, 'nor read it');
	is($n1->safe_psql('postgres', 'SHOW kingres_shard.node_conninfo'), '', 'a superuser sees the default (empty) value');
}

# ---------------------------------------------------------------------------
# The options are applied to the connections.
# ---------------------------------------------------------------------------
{
	# The server has no SSL: with sslmode=require the connection cannot be made, which
	# shows that the option reached libpq.
	my ($rc, $out, $err) = run_sql($n1, "SET kingres_shard.node_conninfo = 'sslmode=require';\nSELECT val FROM public.kv WHERE id = $k_remote");
	isnt($rc, 0, 'with sslmode=require the forwarded statement cannot connect to a node without SSL');
	like($err, qr/could not connect to node/, 'the error is a connection error');
	like($err, qr/SSL|ssl/, 'and says why');
	($rc, $out, $err) = run_sql($n1, "SET kingres_shard.node_conninfo = 'sslmode=disable';\nSELECT val FROM public.kv WHERE id = $k_remote");
	is($err, '', 'with sslmode=disable the statement runs');
	is($out, 'remote-row', 'and returns the row');
	($rc, $out, $err) = run_sql($n1, "SET kingres_shard.node_conninfo = 'sslmode=prefer connect_timeout=3';\nSELECT val FROM public.kv WHERE id = $k_remote");
	like($err, qr/connect_timeout|controlled by the extension/, 'connect_timeout cannot be set through it');
}

# ---------------------------------------------------------------------------
# A path of key material is not shown in an error or in the log.
# ---------------------------------------------------------------------------
{
	my $setting = "sslmode=verify-full sslrootcert=/secret/ca/root.crt sslcert=/secret/c/client.crt sslkey=/secret/k/client.key sslpassword=hunter2";
	my $msg = 'connection to server failed: could not read /secret/k/client.key and could not open certificate file "/secret/c/client.crt"; root certificate file "/secret/ca/root.crt" does not exist; password hunter2';
	my $scrubbed = $n1->safe_psql('postgres',
		"SET kingres_shard.node_conninfo = '$setting'; SELECT kingres.internal_scrub_conn_error(\$q\$$msg\$q\$)");
	unlike($scrubbed, qr{/secret/}, 'the paths of key material are removed from a connection error');
	unlike($scrubbed, qr/hunter2/, 'and the password of the key');
	is(scalar(() = $scrubbed =~ /<hidden>/g), 4, 'each one is replaced by a placeholder');
	like($scrubbed, qr/connection to server failed: could not read <hidden> and could not open certificate file "<hidden>"/, 'the rest of the message is kept');
	is($n1->safe_psql('postgres', "SELECT kingres.internal_scrub_conn_error('plain message')"), 'plain message',
		'without options naming secrets the text is unchanged');

	my $log_before = -s $n1->logfile;
	my ($rc, $out, $err) = run_sql($n1, "SET kingres_shard.node_conninfo = '$setting';\nSELECT val FROM public.kv WHERE id = $k_remote");
	isnt($rc, 0, 'a connection with these options fails against a server without SSL');
	like($err, qr/could not connect to node/, 'with a connection error');
	unlike($err, qr{/secret/|hunter2}, 'whose text shows none of the secrets');
	my $log = join("\n", grep { /ERROR:|DETAIL:|STATEMENT:  SELECT/ } split /\n/, slurp_file($n1->logfile, $log_before));
	unlike($log, qr{/secret/|hunter2}, 'and neither do the error lines of the server log');
}

# ---------------------------------------------------------------------------
# In the configuration file: used by every session and by the recovery worker.
# ---------------------------------------------------------------------------
{
	$n1->append_conf('postgresql.conf', "kingres_shard.node_conninfo = 'sslmode=require'\n");
	$n1->reload;
	is($n1->safe_psql('postgres', 'SHOW kingres_shard.node_conninfo'), 'sslmode=require', 'the setting of the configuration file is in force');
	my ($rc, $out, $err) = run_sql($n1, "SELECT val FROM public.kv WHERE id = $k_remote");
	like($err, qr/could not connect to node/, 'a plain session now needs SSL for the other node');
	$n1->append_conf('postgresql.conf', "kingres_shard.node_conninfo = ''\n");
	$n1->reload;
	($rc, $out, $err) = run_sql($n1, "SELECT val FROM public.kv WHERE id = $k_remote");
	is($err, '', 'and works again after the setting is emptied');
}

done_testing();
