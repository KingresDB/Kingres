# SPDX-License-Identifier: PostgreSQL
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/044_reference_lock.pl
#
# Review finding S3-03: the lock that serializes the writers of a reference
# table must be one no unprivileged role can take.  0.2 used an advisory lock
# in the key space every role shares, so any role could stop every writer of a
# reference table cluster-wide.  Now the lock is a heavyweight lock of a class of
# its own; the wait for it is bounded; and a lock held by a transaction that
# waits for a crashed origin gives a clear error instead of a silent stall.

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use Time::HiRes qw(time);
use KingresShardTest qw(make_cluster run_sql);

my ($n1, $n2, $n3) = make_cluster('rl', 3);
my @nodes = ($n1, $n2, $n3);

$_->safe_psql('postgres', q{CREATE ROLE bob LOGIN; CREATE ROLE eve LOGIN}) for @nodes;
$n1->safe_psql('postgres', q{
	CREATE TABLE public.r2 (id int PRIMARY KEY, v int);
	SELECT kingres.create_reference_table('public.r2');
	GRANT INSERT, SELECT ON public.r2 TO bob;
});

sub as_role
{
	my ($node, $role, $sql, %opt) = @_;
	return run_sql($node, $sql, %opt, extra_params => ['-U', $role]);
}

# ---------------------------------------------------------------------------
# The key of the 0.2 lock, taken by a role that has no privilege on the table,
# stops nothing.
# ---------------------------------------------------------------------------
my $eve = $n2->background_psql('postgres', on_error_stop => 0, extra_params => ['-U', 'eve']);
$eve->query_safe(q{SELECT pg_advisory_lock(1263751750, hashtext('public.r2'))});
$eve->query_safe(q{SELECT pg_advisory_lock(1263751750, 0)});
$eve->query_safe(q{SELECT pg_advisory_lock(12345)});
my $t0 = time;
for my $node ($n1, $n2, $n3)
{
	my ($rc, $out, $err) = as_role($node, 'bob',
		q{SET statement_timeout = '3s'; INSERT INTO public.r2 VALUES (} . (100 + $node->port % 1000) . q{, 1)});
	is($rc, 0, 'a writer is not stopped by the advisory lock of the old key, from ' . $node->name) or diag $err;
}
cmp_ok(time - $t0, '<', 6, 'the three writes were not delayed');
$eve->quit;

# No role can take the lock through SQL: the function that takes it for another
# node answers superusers and node-to-node sessions only.
{
	my ($rc, $out, $err) = as_role($n2, 'eve', q{SELECT kingres.internal_lock_reference_table('public.r2')});
	isnt($rc, 0, 'an unprivileged role cannot take the writer lock');
	like($err, qr/permission denied/, 'the refusal says so');
	($rc, $out, $err) = as_role($n2, 'bob', q{SELECT kingres.internal_lock_reference_table('public.r2')});
	isnt($rc, 0, 'neither can a role that may write the table');
	is($n2->safe_psql('postgres', q{SELECT count(*) FROM pg_locks WHERE locktype = 'object' AND classid = 1263751750}),
		'0', 'no lock of the extension class is held');
}

# ---------------------------------------------------------------------------
# A lock that is held (here by a superuser's transaction) makes a writer wait
# for a bounded time and then fail with a message that says what it waited for;
# readers go on, and other tables are not affected.
# ---------------------------------------------------------------------------
$n1->safe_psql('postgres', q{
	CREATE TABLE public.r3 (id int PRIMARY KEY, v int);
	SELECT kingres.create_reference_table('public.r3');
});
my $holder = $n2->background_psql('postgres', on_error_stop => 0);
$holder->query_safe(q{BEGIN; SELECT kingres.internal_lock_reference_table('public.r2')});
is($n2->safe_psql('postgres', q{SELECT count(*) FROM pg_locks WHERE locktype = 'object' AND classid = 1263751750 AND granted}),
	'1', 'the holder holds the lock of the extension class');

$t0 = time;
my ($rc, $out, $err) = run_sql($n2, q{SET kingres_shard.node_timeout = '2s'; INSERT INTO public.r2 VALUES (7001, 1)});
my $waited = time - $t0;
isnt($rc, 0, 'a writer behind the held lock fails');
like($err, qr/lock timeout/, 'with the lock timeout error');
like($err, qr/writer lock of reference table "public.r2"/, 'and a context that names the table and what holds it');
cmp_ok($waited, '<', 15, "the wait was bounded (waited $waited s with node_timeout = 2s)");
cmp_ok($waited, '>=', 1.5, 'and it was a wait, not an immediate refusal');

# lock_timeout of the session shortens the wait further.
$t0 = time;
($rc, $out, $err) = run_sql($n2, q{SET lock_timeout = '500ms'; INSERT INTO public.r2 VALUES (7002, 1)});
cmp_ok(time - $t0, '<', 3, 'the session lock_timeout is honoured');
isnt($rc, 0, '... and fails');

is($n3->safe_psql('postgres', q{SELECT count(*) FROM public.r2 WHERE id IN (7001, 7002)}), '0', 'nothing was written');
is($n2->safe_psql('postgres', q{INSERT INTO public.r3 VALUES (1, 1) RETURNING id}), '1', 'a writer of another reference table is not delayed');
$holder->query_safe('ROLLBACK');
$holder->quit;
$n2->safe_psql('postgres', q{INSERT INTO public.r2 VALUES (7003, 1)});
is($n3->safe_psql('postgres', q{SELECT count(*) FROM public.r2 WHERE id = 7003}), '1', 'after the holder ends the writer succeeds on every node');

# ---------------------------------------------------------------------------
# S3-13: a write that cascades into another reference table takes that table's
# lock too, so it queues behind a writer of the child alone (and the reverse).
# ---------------------------------------------------------------------------
$n1->safe_psql('postgres', q{
	CREATE TABLE public.fp (id int PRIMARY KEY);
	CREATE TABLE public.fc (id int PRIMARY KEY, p int);
	SELECT kingres.create_reference_table('public.fp');
	SELECT kingres.create_reference_table('public.fc');
	ALTER TABLE public.fc ADD CONSTRAINT fc_fk FOREIGN KEY (p) REFERENCES public.fp ON DELETE CASCADE;
	INSERT INTO public.fp VALUES (1), (2);
	INSERT INTO public.fc VALUES (10, 1), (20, 2);
});
for my $case ([ 'fc', 'DELETE FROM public.fp WHERE id = 1', 'the child lock' ],
	[ 'fp', 'INSERT INTO public.fc VALUES (30, 2)', 'the parent lock' ])
{
	my ($held, $stmt, $what) = @$case;
	my $h = $n2->background_psql('postgres', on_error_stop => 0);
	$h->query_safe(qq{BEGIN; SELECT kingres.internal_lock_reference_table('public.$held')});
	my ($rc2, $o2, $e2) = run_sql($n2, qq{SET kingres_shard.node_timeout = '1s'; $stmt});
	isnt($rc2, 0, "a write waits for $what through the foreign key");
	like($e2, qr/writer lock of reference table "public\.$held"/, "and the wait names public.$held");
	$h->query_safe('ROLLBACK');
	$h->quit;
}
is($n3->safe_psql('postgres', q{SELECT count(*) FROM public.fc}), '2', 'the refused writes changed nothing');
$n2->safe_psql('postgres', q{DELETE FROM public.fp WHERE id = 1});
is($n3->safe_psql('postgres', q{SELECT count(*) FROM public.fc}), '1', 'once the holder is gone the cascade runs on every node');

done_testing();
