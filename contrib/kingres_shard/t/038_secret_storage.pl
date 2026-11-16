# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/038_secret_storage.pl
#
# Review findings S3-04, S3-06, S3-09 and S3-17: where the cluster secret lives
# and who can read it.
#
# S3-04: the secret is not readable through table privileges, predefined roles
# (pg_read_all_data), ownership of the database, or pg_dump.
# S3-06: it reaches no error message or log line of any node, even with
# log_parameter_max_length_on_error = -1 and a statement that fails.
# S3-09: a node of a cluster that has lost its secret refuses a forwarded claim
# of an ordinary role (fail closed) and the secret can be pushed back by the
# map group primary.
# S3-17: a token is bound to the database name, so that tokens made from a copy
# of the database under another name are useless.  The secret can be rotated
# (kingres.rotate_secret()) and the old tokens stop working.

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use IPC::Run;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(make_cluster keys_of_group run_sql);

my $leaky = "log_parameter_max_length = -1\nlog_parameter_max_length_on_error = -1\n"
  . "log_statement = 'all'\nlog_min_messages = debug1\n";
my ($n1, $n2, $n3) = make_cluster('ss', 3, extra => $leaky);
my @nodes = ($n1, $n2, $n3);

$_->safe_psql('postgres', q{
	CREATE ROLE reader LOGIN; GRANT pg_read_all_data TO reader;
	CREATE ROLE filer LOGIN; GRANT pg_read_server_files, pg_write_server_files, pg_monitor,
	  pg_read_all_stats, pg_read_all_settings, pg_stat_scan_tables, pg_signal_backend TO filer;
	CREATE ROLE dbo LOGIN CREATEDB CREATEROLE;
	CREATE ROLE bob LOGIN;
}) for @nodes;
$n1->safe_psql('postgres', q{
	CREATE TABLE public.r (id int PRIMARY KEY, v int);
	CREATE TABLE public.d (id int PRIMARY KEY, v int);
	SELECT kingres.create_reference_table('public.r');
	SELECT kingres.create_distributed_table('public.d', 'id', bucket_count => 12);
	GRANT ALL ON public.r, public.d TO bob;
});
$n1->safe_psql('postgres', 'SELECT kingres.recover_prepared_transactions()');
my ($k0) = keys_of_group($n1, 1, 0, 1);
my ($k1) = keys_of_group($n1, 1, 1, 1);

sub secret_of { return $_[0]->safe_psql('postgres', 'SELECT encode(secret, $$hex$$) FROM kingres.cluster_secret'); }
my $secret = secret_of($n1);
my $first_secret = $secret;
like($secret, qr/^[0-9a-f]{64}$/, 'the cluster has a secret');

# ---------------------------------------------------------------------------
# S3-04: roles that bypass table privileges see nothing.
# ---------------------------------------------------------------------------
for my $role ('reader', 'filer', 'dbo', 'bob')
{
	my ($rc, $out, $err) = run_sql($n1,
		q{SELECT coalesce(string_agg(encode(secret, 'hex'), ','), 'none') FROM kingres.cluster_secret},
		extra_params => [ '-U', $role ]);
	unlike($out, qr/\Q$secret\E/, "S3-04: $role cannot read the secret with SELECT");
	if ($role eq 'reader')
	{
		is($out, 'none', 'pg_read_all_data sees no row of kingres.cluster_secret') or diag($err);
	}
	($rc, $out, $err) = run_sql($n1,
		q{SET row_security = off; SELECT encode(secret, 'hex') FROM kingres.cluster_secret},
		extra_params => [ '-U', $role ]);
	unlike($out, qr/\Q$secret\E/, "S3-04: $role cannot read the secret with row_security = off");
	($rc, $out, $err) = run_sql($n1,
		q{COPY kingres.cluster_secret TO STDOUT},
		extra_params => [ '-U', $role ]);
	unlike($out, qr/\Q$secret\E/, "S3-04: $role cannot COPY the secret out");
	($rc, $out, $err) = run_sql($n1,
		q{SELECT encode(secret, 'hex') FROM (SELECT * FROM kingres.cluster_secret) s},
		extra_params => [ '-U', $role ]);
	unlike($out, qr/\Q$secret\E/, "S3-04: $role cannot read the secret through a subquery");
}
is($n1->safe_psql('postgres', q{SELECT relrowsecurity AND relforcerowsecurity FROM pg_class WHERE oid = 'kingres.cluster_secret'::regclass}),
	't', 'the table has row-level security enabled and forced, and no policy');
is($n1->safe_psql('postgres', q{SELECT count(*) FROM pg_policy WHERE polrelid = 'kingres.cluster_secret'::regclass}),
	'0', 'there is no policy on it');

# The database owner is not the owner of the extension's tables.
{
	my ($rc, $out, $err) = run_sql($n1, q{ALTER TABLE kingres.cluster_secret DISABLE ROW LEVEL SECURITY},
		extra_params => [ '-U', 'dbo' ]);
	isnt($rc, 0, 'S3-04: a role that is not the owner cannot switch the protection off');
	($rc, $out, $err) = run_sql($n1, q{CREATE POLICY p ON kingres.cluster_secret USING (true)},
		extra_params => [ '-U', 'reader' ]);
	isnt($rc, 0, 'S3-04: nor add a policy');
}

# pg_dump, with and without row security, whoever runs it.
for my $role ('reader', 'filer')
{
	for my $opts ([], ['--enable-row-security'])
	{
		my ($stdout, $stderr);
		IPC::Run::run(
			[ 'pg_dump', '-h', $n1->host, '-p', $n1->port, '-U', $role, @$opts, 'postgres' ],
			'>', \$stdout, '2>', \$stderr);
		unlike($stdout, qr/\Q$secret\E/, "S3-04: pg_dump @$opts by $role does not contain the secret");
		IPC::Run::run(
			[ 'pg_dump', '-h', $n1->host, '-p', $n1->port, '-U', $role, @$opts,
			  '-t', 'kingres.cluster_secret', '--data-only', 'postgres' ],
			'>', \$stdout, '2>', \$stderr);
		unlike($stdout, qr/\Q$secret\E/, "S3-04: pg_dump of the secret table by $role (@$opts) does not contain the secret");
	}
}

# ---------------------------------------------------------------------------
# S3-17: the token is bound to the database name; the secret can be rotated.
# ---------------------------------------------------------------------------
my $tok_bob_2 = $n2->safe_psql('postgres', q{SELECT kingres.internal_forward_token('bob', 2)});
$n2->safe_psql('postgres', 'CREATE DATABASE copy TEMPLATE postgres');
my $tok_copy = $n2->safe_psql('copy', q{SELECT kingres.internal_forward_token('bob', 2)});
isnt($tok_copy, $tok_bob_2, 'S3-17: the token of a copy of the database under another name differs');
{
	local $ENV{PGOPTIONS} = '-c kingres_shard.forwarded=on -c kingres_shard.forward_token=' . $tok_copy;
	my ($rc, $out, $err) = run_sql($n2, 'SELECT 1', extra_params => [ '-U', 'bob' ]);
	isnt($rc, 0, 'S3-17: a token made in the copy is refused by the original database');
	like($err, qr/cannot prove it/, 'S3-17: for lack of proof');
}
$n2->safe_psql('postgres', 'DROP DATABASE copy');

# Rotation: a new secret everywhere, the old tokens stop working, routed
# statements keep working from every node.
{
	my ($rc, $out, $err) = run_sql($n2, 'SELECT kingres.rotate_secret()');
	isnt($rc, 0, 'rotate_secret() runs on the map group primary only');
	($rc, $out, $err) = run_sql($n1, 'SELECT kingres.rotate_secret()', extra_params => [ '-U', 'bob' ]);
	isnt($rc, 0, 'and for superusers only');
	$n1->safe_psql('postgres', 'SELECT kingres.rotate_secret()');
	my $new = secret_of($n1);
	like($new, qr/^[0-9a-f]{64}$/, 'a new secret exists');
	isnt($new, $secret, 'it is not the old one');
	is(secret_of($_), $new, 'the new secret reached ' . $_->name) for ($n2, $n3);
	local $ENV{PGOPTIONS} = '-c kingres_shard.forwarded=on -c kingres_shard.forward_token=' . $tok_bob_2;
	($rc, $out, $err) = run_sql($n2, 'SELECT 1', extra_params => [ '-U', 'bob' ]);
	isnt($rc, 0, 'a token made from the old secret is refused after the rotation');
}
for my $origin ($n1, $n2, $n3)
{
	my $k = $origin->name eq $n2->name ? $k0 : $k1;
	$origin->safe_psql('postgres', "INSERT INTO public.d VALUES ($k, 1) ON CONFLICT (id) DO UPDATE SET v = d.v + 1");
}
is($n3->safe_psql('postgres', "SELECT sum(v) FROM public.d"), '3', 'routed writes and a scan work after the rotation, from every node');
$n3->safe_psql('postgres', 'INSERT INTO public.r VALUES (1, 1)');
is($_->safe_psql('postgres', 'SELECT count(*) FROM public.r'), '1', 'a reference write reached ' . $_->name) for @nodes;
$secret = secret_of($n1);

# ---------------------------------------------------------------------------
# S3-06: a failing push of the secret leaks it nowhere (parameters are logged
# in full by these nodes, also for statements that fail).
# ---------------------------------------------------------------------------
{
	$n2->safe_psql('postgres', q{
		CREATE FUNCTION public.boom() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'boom'; END $$;
		CREATE TRIGGER boom BEFORE INSERT OR UPDATE ON kingres.cluster_secret FOR EACH ROW EXECUTE FUNCTION public.boom();
	});
	my ($rc, $out, $err) = run_sql($n1, 'SELECT kingres.sync_catalog()');
	isnt($rc, 0, 'the push fails when the node refuses the secret');
	like($err, qr/boom/, 'with the error of the node');
	unlike($err, qr/\Q$secret\E/, 'S3-06: the error of the origin does not contain the secret');
	($rc, $out, $err) = run_sql($n1, 'SELECT kingres.rotate_secret()');
	isnt($rc, 0, 'a rotation fails the same way');
	unlike($err, qr/\Q$secret\E/, 'S3-06: its error does not contain the secret either');
	unlike($err, qr/[0-9a-f]{64}/, 'S3-06: nor any 64-digit hex string');
	$n2->safe_psql('postgres', q{DROP TRIGGER boom ON kingres.cluster_secret; DROP FUNCTION public.boom()});
	is(secret_of($n1), $secret, 'the failed rotation changed nothing on the origin');
	is(secret_of($n2), $secret, 'nor on node 2');
}

# ---------------------------------------------------------------------------
# S3-09: a node of the cluster without a secret refuses the claim of a role
# that is not a superuser; the map group primary can push the secret back.
# ---------------------------------------------------------------------------
{
	my $tok = $n2->safe_psql('postgres', q{SELECT kingres.internal_forward_token('bob', 2)});
	$n2->safe_psql('postgres', 'DELETE FROM kingres.cluster_secret');
	is(secret_of($n2), '', 'node 2 lost its secret');
	{
		local $ENV{PGOPTIONS} = '-c kingres_shard.forwarded=on';
		my ($rc, $out, $err) = run_sql($n2, 'INSERT INTO public.r VALUES (990, 1)', extra_params => [ '-U', 'bob' ]);
		isnt($rc, 0, 'S3-09: the forged flag of an ordinary role is refused on a node without a secret');
		like($err, qr/cannot prove it/, 'S3-09: for lack of proof');
	}
	is($n2->safe_psql('postgres', 'SELECT count(*) FROM public.r WHERE id = 990'), '0', 'nothing was written');
	# Routed statements of ordinary roles to that node fail closed too, with a message that says what to do.
	my ($rc, $out, $err) = run_sql($n1, "INSERT INTO public.d VALUES ($k1, 5) ON CONFLICT (id) DO NOTHING", extra_params => [ '-U', 'bob' ]);
	isnt($rc, 0, 'S3-09: a statement of an ordinary role routed to the node is refused');
	like($err, qr/sync_catalog/, 'S3-09: the message names kingres.sync_catalog()') or diag($err);
	# The administrator's sync restores it (a superuser's session is accepted without proof for this).
	$n1->safe_psql('postgres', 'SELECT kingres.sync_catalog()');
	is(secret_of($n2), secret_of($n1), 'S3-09: sync_catalog() pushed the secret back');
	($rc, $out, $err) = run_sql($n1, "INSERT INTO public.d VALUES ($k1, 5) ON CONFLICT (id) DO NOTHING", extra_params => [ '-U', 'bob' ]);
	is($rc, 0, 'routed statements of ordinary roles work again') or diag($err);
}

# ---------------------------------------------------------------------------
# No node logged the secret, an old secret, or a token (statements were logged
# in full with their parameters).
# ---------------------------------------------------------------------------
for my $n (@nodes)
{
	my $log = slurp_file($n->logfile);
	unlike($log, qr/\Q$secret\E/, 'S3-06: the current secret is not in the log of ' . $n->name);
	unlike($log, qr/\Q$first_secret\E/, 'S3-06: the first secret is not in the log of ' . $n->name);
	unlike($log, qr/\Q$tok_bob_2\E/, 'S3-06: a token is not in the log of ' . $n->name);
}

done_testing();
