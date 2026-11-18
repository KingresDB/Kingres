# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/118_review_steps_1_2.pl
#
# The fixes for the independent review of steps 1 and 2 of phase 2b (findings P2B-01 to P2B-07).
# Every answer is compared with a single ordinary node that gets the same statements.
#
#   A. P2B-01  the mark of a local part cannot be read by SQL code, and a statement that SQL code
#              writes with it is routed (a CHECK function of a table owned by a role that is not a
#              superuser reads the error context inside the local part of a fan-out write and of a
#              batch of the row router);
#   B. P2B-04  the memory of thousands of rescans of a scan with a large IN list is flat;
#   C. P2B-02  restrictions that deparse with COLLATE or mix name, text, bpchar and varchar are
#              answered, not refused (the receiving group decides on the same tree as the origin);
#   D. P2B-03  a set operation with an arm that contradicts itself is answered in every form;
#   E. P2B-05  an inequality on the key never prunes;
#   F. P2B-07  FOR UPDATE on a read that mixes a remote key with a local table says what is wrong.

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(run_sql);
use KingresShardCompare;

my $c = KingresShardCompare->new('rf');
my ($n1, $n2, $n3) = $c->nodes;

$c->on_every_node(q{CREATE ROLE lowpriv LOGIN; GRANT CREATE ON SCHEMA public TO lowpriv});
$c->on_both(q{CREATE TABLE public.kv (id int PRIMARY KEY, v int)});
$c->on_cluster(q{SELECT kingres.create_distributed_table('public.kv', 'id', bucket_count => 12)});
$c->on_both(q{INSERT INTO public.kv SELECT g, g FROM generate_series(1, 60) g});
$c->on_both(q{CREATE TABLE public.tx (k text PRIMARY KEY, v int)});
$c->on_cluster(q{SELECT kingres.create_distributed_table('public.tx', 'k', bucket_count => 12)});
$c->on_both(q{INSERT INTO public.tx SELECT chr(96 + g) || repeat('x', g % 3), g FROM generate_series(1, 26) g});
$c->on_every_node(q{CREATE TABLE public.loc (id int, w text)});
$c->on_single(q{CREATE TABLE public.loc (id int, w text)});
$c->on_every_node(q{INSERT INTO public.loc SELECT g, 'l' || g FROM generate_series(1, 5) g});
$c->on_single(q{INSERT INTO public.loc SELECT g, 'l' || g FROM generate_series(1, 5) g});

# ---------------------------------------------------------------------------
# A. the mark of a local part
# ---------------------------------------------------------------------------
{
	$c->on_cluster(q{
		CREATE TABLE public.mine2 (id int PRIMARY KEY, v int);
		SELECT kingres.create_distributed_table('public.mine2', 'id', bucket_count => 12);
		INSERT INTO public.mine2 SELECT g, g FROM generate_series(1, 40) g;
		ALTER TABLE public.mine2 OWNER TO lowpriv;
	});
	# The table of the log is local to every node, owned by the role.
	$c->on_every_node(q{
		CREATE TABLE public.lp_log (marker boolean, plain bigint, forged bigint);
		ALTER TABLE public.lp_log OWNER TO lowpriv;
		CREATE FUNCTION public.lp_steal(x int) RETURNS boolean LANGUAGE plpgsql AS $f$
		DECLARE ctx text; plain bigint; forged bigint;
		BEGIN
		  IF coalesce(current_setting('lp.armed', true), '') <> 'on'
		     OR coalesce(current_setting('kingres_shard.forwarded', true), '') = 'on' THEN
		    RETURN true;
		  END IF;
		  BEGIN PERFORM 1/0; EXCEPTION WHEN OTHERS THEN GET STACKED DIAGNOSTICS ctx = PG_EXCEPTION_CONTEXT; END;
		  EXECUTE 'SELECT count(*) FROM public.mine2' INTO plain;
		  EXECUTE '/* kingres:local 0000000000000000 */ SELECT count(*) FROM public.mine2' INTO forged;
		  PERFORM set_config('lp.captured', ctx, false);
		  INSERT INTO public.lp_log VALUES (ctx ~ 'kingres:local', plain, forged);
		  RETURN true;
		END $f$;
		ALTER FUNCTION public.lp_steal(int) OWNER TO lowpriv;
	});
	$c->on_cluster(q{ALTER TABLE public.mine2 ADD CONSTRAINT lp_c CHECK (public.lp_steal(v))});

	# The local part of a fan-out write.
	my ($rc, $out, $err) = run_sql($n1, q{SET lp.armed = on; UPDATE public.mine2 SET v = v + 1 WHERE v > 0;
		SELECT current_setting('lp.captured', true) ~ 'kingres:local'}, extra_params => [ '-U', 'lowpriv' ]);
	is($rc, 0, 'the fan-out write of a role that is not a superuser runs') or diag($err);
	like($out, qr/f\s*$/, 'the error context of the local part carries no mark');
	is($n1->safe_psql('postgres', q{SELECT bool_or(marker) || '|' || min(plain) || '|' || max(plain) || '|' || min(forged) FROM public.lp_log}),
		'false|40|40|40', 'a statement of the function inside the local part is routed (sees every row), also with a forged comment');
	is($n1->safe_psql('postgres', q{SELECT count(*) FROM public.mine2 WHERE v > 1}), 40, 'and the write reached every row');
	# A value captured during the call does nothing after it.
	($rc, $out, $err) = run_sql($n1, q{SELECT count(*) FROM public.mine2},
		extra_params => [ '-U', 'lowpriv' ]);
	is($out, '40', 'after the call a plain statement is routed');
	$n1->safe_psql('postgres', q{DELETE FROM public.lp_log});

	# The local batch of the row router.
	($rc, $out, $err) = run_sql($n1, q{SET lp.armed = on; INSERT INTO public.mine2 SELECT g, g FROM generate_series(1001, 1040) g;
		SELECT current_setting('lp.captured', true) ~ 'kingres:local'}, extra_params => [ '-U', 'lowpriv' ]);
	is($rc, 0, 'the row router of a role that is not a superuser runs') or diag($err);
	like($out, qr/f\s*$/, 'the error context of the local batch carries no mark');
	is($n1->safe_psql('postgres', q{SELECT bool_or(marker)::text || '|' || (min(plain) >= 40)::text || '|' || (min(forged) >= 40)::text FROM public.lp_log}),
		'false|true|true', 'a statement of the function inside the local batch is routed');
	is($n1->safe_psql('postgres', q{SELECT count(*) FROM public.mine2}), 80, 'and the batch stored its rows once');
}

# ---------------------------------------------------------------------------
# B. the memory of a rescan
# ---------------------------------------------------------------------------
SKIP:
{
	skip 'needs /proc', 3 unless -r '/proc/self/status';
	my $b = $n1->background_psql('postgres', on_error_stop => 1);
	my $pid = $b->query_safe('SELECT pg_backend_pid()');
	chomp $pid;
	my $hwm = sub {
		open(my $fh, '<', "/proc/$pid/status") or return -1;
		while (<$fh>) { return $1 if /^VmHWM:\s+(\d+) kB/ }
		return -1;
	};
	my $array = 'ARRAY[' . join(',', 1 .. 20000) . ']';
	$b->query_safe(q{SET enable_hashjoin = off; SET enable_mergejoin = off; SET enable_material = off});
	my $plan = $b->query_safe(qq{EXPLAIN (COSTS OFF) SELECT count(*) FROM (SELECT g FROM generate_series(1, 600) g) s LEFT JOIN public.kv ON kv.id = ANY ($array)});
	like($plan, qr/Nested Loop Left Join/, 'the scan is the inner side of a nested loop (it is rescanned for every outer row)');
	# 20 rescans first (the peak of the planning and of the first starts), then 600: a start that
	# keeps its memory until the statement ends makes the peak grow by about 250 kB per rescan more.
	my $n = $b->query_safe(qq{SELECT count(*) FROM (SELECT g FROM generate_series(1, 20) g) s LEFT JOIN public.kv ON kv.id = ANY ($array)});
	chomp $n;
	is($n, 20 * 60, 'the join answers');
	my $before = $hwm->();
	$n = $b->query_safe(qq{SELECT count(*) FROM (SELECT g FROM generate_series(1, 600) g) s LEFT JOIN public.kv ON kv.id = ANY ($array)});
	chomp $n;
	my $after = $hwm->();
	cmp_ok($after - $before, '<', 40 * 1024, "580 rescans more with an array of 20000 values leave the memory flat (peak grew by " . ($after - $before) . " kB)");
	$b->quit;
}

# ---------------------------------------------------------------------------
# C. the origin and the group decide on the same tree
# ---------------------------------------------------------------------------
{
	my @restrictions = (
		q{k = 'a' COLLATE "C"},
		q{k LIKE 'a%' COLLATE "C"},
		q{k IN ('a'::name, 'b')},
		q{k = coalesce('a '::char(3), 'ab'::name)},
		q{k COLLATE "C" = 'a'},
		q{k = ANY ('{a,bx}'::name[])},
		q{k::name = 'a'},
		q{k = 'a'::varchar},
		q{k IN ('a'::bpchar, 'b')},
		q{k = ('a' COLLATE "POSIX")},
		q{k <> 'a' COLLATE "C"},
		q{lower(k) = 'a'},
		q{k = 'a' OR k = 'b'::name},
		q{k IN ('a'::name, 'b'::varchar, 'c'::text)},
		q{k = ANY (ARRAY['a'::name, 'b'])},
	);
	for my $r (@restrictions)
	{
		$c->compare("restriction $r", "SELECT k, v FROM public.tx WHERE $r ORDER BY k", ordered => 1, profiles => 0);
	}

	# A fragment that a group does not accept is refused with 0A000, not with the retryable KS001:
	# the origin and the group decide on the same tree, so a repeat gives the same answer.
	my $tok = $n2->safe_psql('postgres', "SELECT kingres.internal_forward_token(current_user, 2)");
	local $ENV{PGOPTIONS} = "-c kingres_shard.forwarded=on -c kingres_shard.forward_token=$tok";
	for my $f ([ 'scan', q{/* kingres:scan-fragment */ SELECT count(*) FROM public.kv} ],
			   [ 'write', q{/* kingres:write-fragment */ UPDATE public.kv SET v = (random() * 10)::int} ],
			   [ 'insert', q{/* kingres:insert-fragment place=1 */ INSERT INTO public.kv SELECT g, g FROM generate_series(1, 3) g} ])
	{
		my ($rc, $out, $err) = run_sql($n2, "\\set VERBOSITY verbose\n$f->[1]");
		isnt($rc, 0, "a forged $f->[0] fragment is refused");
		like($err, qr/does not accept the fragment that another node sent it/, "$f->[0]: with its own message");
		like($err, qr/0A000/, "$f->[0]: with the SQLSTATE of a feature that is not supported");
		unlike($err, qr/KS001/, "$f->[0]: not the retryable error");
	}
}

# ---------------------------------------------------------------------------
# D. set operations with an arm that contradicts itself
# ---------------------------------------------------------------------------
{
	my @arms = (
		[ q{id = 1 AND id = 2}, q{id = 1} ],
		[ q{id = 1}, q{id = 1 AND id = 2} ],
		[ q{id = 1 AND id = 2}, q{id = 3} ],
		[ q{id = 1 AND id = 2}, q{id = 1 AND id = 2} ],
		[ q{id = 1 AND id = 2}, q{id < 10} ],
	);
	for my $op ('UNION', 'UNION ALL', 'INTERSECT', 'INTERSECT ALL', 'EXCEPT', 'EXCEPT ALL')
	{
		for my $a (@arms)
		{
			$c->compare("$op: $a->[0] / $a->[1]",
				"SELECT id FROM public.kv WHERE $a->[0] $op SELECT id FROM public.kv WHERE $a->[1] ORDER BY 1", ordered => 1, profiles => 0);
		}
	}
}

# ---------------------------------------------------------------------------
# E. an inequality on the key never prunes
# ---------------------------------------------------------------------------
{
	for my $p ('id < 5', 'id <= 5', 'id > 55', 'id >= 55', 'id <> 5', 'id > 3 AND id < 9', 'id < 5 OR id > 57')
	{
		$c->compare("inequality: $p", "SELECT count(*), sum(id) FROM public.kv WHERE $p", profiles => 0);
		my ($rc, $out, $err) = run_sql($n1, "EXPLAIN (COSTS OFF) SELECT id FROM public.kv WHERE $p");
		unlike($out, qr/pruned by/, "inequality: $p does not prune");
	}
	my ($rc, $out, $err) = run_sql($n1, q{EXPLAIN (COSTS OFF) SELECT id FROM public.kv WHERE id IN (1, 2, 3, 4)});
	like($out, qr/pruned by|Remote Statement|Seq Scan|Append/, 'an equality list is still planned');
}

# ---------------------------------------------------------------------------
# F. FOR UPDATE on a read that mixes a remote key with a local table
# ---------------------------------------------------------------------------
{
	my $remote = $n1->safe_psql('postgres', q{SELECT g FROM generate_series(1, 100) g
		WHERE (SELECT m.group_id FROM kingres.bucket_map m WHERE m.colocation_id = 1 AND m.bucket = kingres.bucket_for(1, g)) = 1 LIMIT 1});
	my ($rc, $out, $err) = run_sql($n1, "SELECT * FROM public.kv, public.loc WHERE kv.id = $remote AND loc.id = kv.id FOR UPDATE");
	isnt($rc, 0, 'FOR UPDATE on a remote key and a local table is refused');
	like($err, qr/a local table cannot be read in a statement that locks rows of a distributed table of another shard group/,
		'with a message that says what is wrong');
	unlike($err, qr/spans shard groups/, 'and does not claim that the statement spans groups');
	($rc, $out, $err) = run_sql($n1, 'SELECT * FROM public.kv FOR UPDATE');
	like($err, qr/row-level locking on a statement that spans shard groups is not supported yet/, 'a statement that does span groups keeps its message');
}

done_testing();
