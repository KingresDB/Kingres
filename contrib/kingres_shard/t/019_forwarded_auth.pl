# SPDX-License-Identifier: PostgreSQL
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/019_forwarded_auth.pl
#
# Review finding S2-04: a session is treated as forwarded (opened by another
# node of the cluster) only if it proves it with a token derived from the
# cluster secret.  An ordinary role with write privilege on a reference table
# cannot, by any combination of the forwarded flag and the fragment markers,
# make the copies differ, run DDL without propagation, or write a bucket on one
# node only; the token and the secret never appear where a user can read them.

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(make_cluster keys_of_group run_sql);

my ($n1, $n2, $n3) = make_cluster('fa', 3);
my @nodes = ($n1, $n2, $n3);

$_->safe_psql('postgres', 'CREATE ROLE bob LOGIN; CREATE ROLE alice LOGIN; GRANT CREATE ON SCHEMA public TO bob') for @nodes;
$n1->safe_psql('postgres', q{
	CREATE TABLE public.r (id int PRIMARY KEY, v int);
	CREATE TABLE public.d (id int PRIMARY KEY, v int);
	INSERT INTO public.r VALUES (1, 1);
	SELECT kingres.create_reference_table('public.r');
	SELECT kingres.create_distributed_table('public.d', 'id', bucket_count => 12);
	GRANT SELECT, INSERT, UPDATE, DELETE ON public.r, public.d TO bob;
});
$n1->safe_psql('postgres', 'SELECT kingres.recover_prepared_transactions()');
my ($k0) = keys_of_group($n1, 1, 0, 1);
my ($k1) = keys_of_group($n1, 1, 1, 1);

sub secret_of { return $_[0]->safe_psql('postgres', 'SELECT encode(secret, $$hex$$) FROM kingres.cluster_secret'); }

# ---------------------------------------------------------------------------
# The secret exists on every node, is the same, and no role may read it.
# ---------------------------------------------------------------------------
my $secret = secret_of($n1);
like($secret, qr/^[0-9a-f]{64}$/, 'init_cluster made a random secret');
is(secret_of($_), $secret, 'the secret reached ' . $_->name) for ($n2, $n3);
for my $role ('bob', 'public')
{
	is($n1->safe_psql('postgres', "SELECT has_table_privilege('$role', 'kingres.cluster_secret', 'SELECT')"),
		'f', "$role has no privilege on kingres.cluster_secret") unless $role eq 'public';
}
my ($rc, $out, $err) = run_sql($n1, 'SELECT * FROM kingres.cluster_secret', extra_params => [ '-U', 'bob' ]);
like($err, qr/permission denied for table cluster_secret/, 'an ordinary role cannot read the secret');
is($n1->safe_psql('postgres',
		q{SELECT count(*) FROM information_schema.role_table_grants WHERE table_name = 'cluster_secret' AND grantee <> current_user}),
	'0', 'no grant on the secret to anyone');

# ---------------------------------------------------------------------------
# Forged sessions are refused.
# ---------------------------------------------------------------------------
my $t_alice_2 = $n2->safe_psql('postgres', q{SELECT kingres.internal_forward_token('alice', 2)});
my $t_bob_3 = $n2->safe_psql('postgres', q{SELECT kingres.internal_forward_token('bob', 3)});
my $t_bob_2 = $n2->safe_psql('postgres', q{SELECT kingres.internal_forward_token('bob', 2)});
like($t_bob_2, qr/^[0-9a-f]{64}$/, 'a token is the hex form of an HMAC-SHA-256');
isnt($t_alice_2, $t_bob_2, 'the token depends on the role');
isnt($t_bob_3, $t_bob_2, 'the token depends on the node');

my @forgeries = (
	[ 'the flag alone',                  '-c kingres_shard.forwarded=on' ],
	[ 'the flag with a made-up token',   '-c kingres_shard.forwarded=on -c kingres_shard.forward_token=' . ('ab' x 32) ],
	[ 'the flag with a short token',     '-c kingres_shard.forwarded=on -c kingres_shard.forward_token=abcd' ],
	[ 'the flag with a token and more',  '-c kingres_shard.forwarded=on -c kingres_shard.forward_token=' . $t_bob_2 . 'ff' ],
	[ 'a token made for another role',   '-c kingres_shard.forwarded=on -c kingres_shard.forward_token=' . $t_alice_2 ],
	[ 'a token made for another node',   '-c kingres_shard.forwarded=on -c kingres_shard.forward_token=' . $t_bob_3 ],
);
my @statements = (
	[ 'a reference-table insert',      q{INSERT INTO public.r VALUES (99, 99)} ],
	[ 'a reference-table update',      q{UPDATE public.r SET v = 7 WHERE id = 1} ],
	[ 'DDL on a table of the role',    q{CREATE TABLE public.forged (a int)} ],
	[ 'a write fragment',              q{/* kingres:write-fragment */ DELETE FROM public.d} ],
	[ 'a scan fragment',               q{/* kingres:scan-fragment */ SELECT count(*) FROM public.d} ],
	[ 'a bucket write on one node',    qq{INSERT INTO public.d VALUES ($k1, 1)} ],
	[ 'a plain select',                q{SELECT 1} ],
);
for my $f (@forgeries)
{
	for my $st (@statements)
	{
		local $ENV{PGOPTIONS} = $f->[1];
		my ($rc2, $out2, $err2) = run_sql($n2, $st->[1], extra_params => [ '-U', 'bob' ]);
		isnt($rc2, 0, "forged ($f->[0]): $st->[0] is refused");
		like($err2, qr/cannot prove it/, "forged ($f->[0]): $st->[0]: the reason is the missing proof");
		unlike($err2, qr/[0-9a-f]{40}/, "forged ($f->[0]): $st->[0]: the message shows no token");
	}
}
for my $n (@nodes)
{
	is($n->safe_psql('postgres', 'SELECT count(*) FROM public.r WHERE id = 99'), '0', 'nothing was written to r on ' . $n->name);
	is($n->safe_psql('postgres', q{SELECT count(*) FROM pg_class WHERE relname = 'forged'}), '0', 'no table "forged" on ' . $n->name);
	is($n->safe_psql('postgres', 'SELECT v FROM public.r WHERE id = 1'), '1', 'r is unchanged on ' . $n->name);
}
is($n1->safe_psql('postgres', "SELECT count(*) FROM public.d WHERE id = $k1"), '0', 'no bucket was written');

# ---------------------------------------------------------------------------
# A session with the proof is accepted (a token replayed by whoever holds it).
# ---------------------------------------------------------------------------
{
	local $ENV{PGOPTIONS} = '-c kingres_shard.forwarded=on -c kingres_shard.forward_token=' . $t_bob_2;
	my ($rc3, $out3, $err3) = run_sql($n2, 'SELECT 1', extra_params => [ '-U', 'bob' ]);
	is($rc3, 0, 'a session with a valid token for its role and node is accepted') or diag($err3);
	# The same token for the same role on another node does not work.
	($rc3, $out3, $err3) = run_sql($n3, 'SELECT 1', extra_params => [ '-U', 'bob' ]);
	isnt($rc3, 0, 'the token does not work on another node');
}

# ---------------------------------------------------------------------------
# Markers in the text of an ordinary session mean nothing.
# ---------------------------------------------------------------------------
{
	$n1->safe_psql('postgres', "INSERT INTO public.d VALUES ($k0, 1), ($k1, 2)") if 0;
	$n1->safe_psql('postgres', "INSERT INTO public.d VALUES ($k0, 1)");
	$n1->safe_psql('postgres', "INSERT INTO public.d VALUES ($k1, 2)");
	my ($rc4, $out4, $err4) = run_sql($n3, '/* kingres:scan-fragment */ SELECT count(*) FROM public.d', extra_params => [ '-U', 'bob' ]);
	is($out4, '2', 'a scan marker in an ordinary session still reads the whole table');
	($rc4, $out4, $err4) = run_sql($n3, "/* kingres:write-fragment */ DELETE FROM public.d WHERE v > 0", extra_params => [ '-U', 'bob' ]);
	is($err4, '', 'a write marker in an ordinary session runs as a normal statement');
	is($n1->safe_psql('postgres', 'SELECT count(*) FROM public.d'), '0', 'and it deleted the rows of every group');
}

# ---------------------------------------------------------------------------
# The token and the secret are never shown.
# ---------------------------------------------------------------------------
{
	local $ENV{PGOPTIONS} = '-c kingres_shard.forwarded=on -c kingres_shard.forward_token=' . $n2->safe_psql('postgres', 'SELECT kingres.internal_forward_token(current_user, 2)');
	my ($rc5, $out5, $err5) = run_sql($n2, 'SHOW kingres_shard.forward_token');
	is($out5, '<hidden>', 'SHOW does not show the token');
	($rc5, $out5, $err5) = run_sql($n2, q{SELECT count(*) FROM pg_settings WHERE name = 'kingres_shard.forward_token' AND setting <> '<hidden>' AND setting <> ''});
	is($out5, '0', 'pg_settings does not show the token');
	($rc5, $out5, $err5) = run_sql($n2, q{SELECT current_setting('kingres_shard.forward_token')});
	is($out5, '<hidden>', 'current_setting does not show the token');
}
for my $n (@nodes)
{
	my $log = slurp_file($n->logfile);
	unlike($log, qr/\Q$secret\E/, 'the secret is not in the log of ' . $n->name);
	unlike($log, qr/\Q$t_bob_2\E/, 'a token is not in the log of ' . $n->name);
	unlike($log, qr/\Q$t_alice_2\E/, 'a token (2) is not in the log of ' . $n->name);
}

done_testing();
