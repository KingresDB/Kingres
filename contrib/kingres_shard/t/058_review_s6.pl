# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/058_review_s6.pl
#
# The findings of the review of the port of 0.4 to PostgreSQL 18 and 19
# (kingres/docs/reviews/2026-11-15-kingres-shard-0.4-port.md) that need no second
# installation of PostgreSQL:
#
#	S6-03	a reference table has no inheritance children, parents or partitions
#	S6-04	a write that other nodes take part in is refused while
#			session_replication_role is not "origin"
#	S6-06	a node that refuses the claim of this node's session and runs another
#			version is reported with both versions, not as unreachable
#	S6-07	the 0.3 scripts leave the session's check_function_bodies as it was
#	S6-09	a node of another major is not used by statements that need it
#	S6-10	the message about a node that reports no version
#
# (S6-01 is t/056, S6-02 is t/057, S6-08 is t/059.)

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(make_node make_cluster keys_of_group run_sql prepared_count);

sub q1 { my ($n, $sql) = @_; return $n->safe_psql('postgres', $sql); }

# ---------------------------------------------------------------------------
# S6-03: inheritance and partitions of a reference table
# ---------------------------------------------------------------------------
{
	my ($n1, $n2, $n3) = make_cluster('s603', 3);
	q1($n1, q{
		CREATE TABLE public.r2 (id int PRIMARY KEY, seen int);
		SELECT kingres.create_reference_table('public.r2');
		CREATE TABLE public.plain (id int, seen int);
		CREATE TABLE public.parted (id int, seen int) PARTITION BY RANGE (id);
		CREATE TABLE public.d (id int PRIMARY KEY, v int);
		SELECT kingres.create_distributed_table('public.d', 'id', bucket_count => 6);
	});
	my $refused = qr/is not supported on reference table "r2": a reference table has no inheritance children or parents and no partitions/;
	for my $routing ('on', 'off')
	{
		for my $case (
			[ 'CREATE TABLE ... INHERITS (r)', 'CREATE TABLE public.c1 () INHERITS (public.r2)' ],
			[ 'CREATE TABLE ... INHERITS (a, r)', 'CREATE TABLE public.c2 () INHERITS (public.plain, public.r2)' ],
			[ 'CREATE TABLE ... PARTITION OF r', 'CREATE TABLE public.c3 PARTITION OF public.r2 FOR VALUES FROM (0) TO (10)' ],
			[ 'ALTER TABLE plain INHERIT r', 'ALTER TABLE public.plain INHERIT public.r2' ],
			[ 'ALTER TABLE r INHERIT plain', 'ALTER TABLE public.r2 INHERIT public.plain' ],
			[ 'ALTER TABLE parted ATTACH PARTITION r', 'ALTER TABLE public.parted ATTACH PARTITION public.r2 FOR VALUES FROM (0) TO (10)' ],
			[ 'ALTER TABLE r ATTACH PARTITION plain', 'ALTER TABLE public.r2 ATTACH PARTITION public.plain FOR VALUES FROM (0) TO (10)' ],
		  )
		{
			my ($rc, $out, $err) = run_sql($n1, "SET kingres_shard.enable_routing = $routing;\n$case->[1]");
			isnt($rc, 0, "S6-03: $case->[0] is refused (routing $routing)");
			like($err, $refused, '...with the reason');
		}
	}
	is(q1($n1, q{SELECT count(*) FROM pg_inherits WHERE 'public.r2'::regclass IN (inhparent, inhrelid) OR 'public.plain'::regclass IN (inhparent, inhrelid)}), '0', 'S6-03: no inheritance was made');
	is(q1($n1, q{SELECT count(*) FROM pg_class WHERE relname IN ('c1', 'c2', 'c3')}), '0', 'S6-03: and no table');
	# Other inheritance is the user's business, as it was: an ordinary table may have children.
	my ($rc, $out, $err) = run_sql($n1, q{CREATE TABLE public.kid () INHERITS (public.plain)});
	is($rc, 0, 'an ordinary table keeps its children') or diag $err;
	($rc, $out, $err) = run_sql($n1, q{CREATE TABLE public.part1 PARTITION OF public.parted FOR VALUES FROM (0) TO (10)});
	is($rc, 0, 'and its partitions') or diag $err;
	# A table that takes part in inheritance is not made a reference table (as before).
	($rc, $out, $err) = run_sql($n1, q{SELECT kingres.create_reference_table('public.plain')});
	isnt($rc, 0, 'a table with children is not made a reference table');
	like($err, qr/inheritance is not supported/, '...as before');
	# The statements on the reference table itself still work.
	($rc, $out, $err) = run_sql($n2, q{INSERT INTO public.r2 VALUES (1, 0); UPDATE public.r2 SET seen = 1});
	is($rc, 0, 'a write to the reference table works') or diag $err;
	is(join(',', map { q1($_, 'SELECT id || chr(124) || seen FROM public.r2') } ($n1, $n2, $n3)), '1|1,1|1,1|1', 'on every node');
	is(q1($n1, q{SELECT coalesce(string_agg(check_name || ' ' || node_id, ', '), '') FROM kingres.check_cluster()}), '', 'check_cluster finds nothing');
}

# ---------------------------------------------------------------------------
# S6-04: session_replication_role
# ---------------------------------------------------------------------------
{
	my ($n1, $n2, $n3) = make_cluster('s604', 3);
	q1($n1, q{
		CREATE TABLE public.rparent (id int PRIMARY KEY);
		SELECT kingres.create_reference_table('public.rparent');
		CREATE TABLE public.rchild (id int PRIMARY KEY, pid int);
		SELECT kingres.create_reference_table('public.rchild');
		ALTER TABLE public.rchild ADD FOREIGN KEY (pid) REFERENCES public.rparent(id) ON DELETE CASCADE;
		INSERT INTO public.rparent VALUES (1), (2);
		INSERT INTO public.rchild VALUES (10, 1);
		INSERT INTO public.rchild VALUES (20, 2);
		CREATE TABLE public.d (id int PRIMARY KEY, v int);
		SELECT kingres.create_distributed_table('public.d', 'id', bucket_count => 6);
	});
	q1($n1, join('', map { "INSERT INTO public.d VALUES ($_, $_);\n" } 1 .. 12));
	for my $role ('replica', 'local')
	{
		my $named = qr/cannot be written while session_replication_role is "$role"/;
		for my $case (
			[ 'a delete of a reference table with a foreign key action', 'DELETE FROM public.rparent WHERE id = 1' ],
			[ 'an insert into a reference table', 'INSERT INTO public.rparent VALUES (3)' ],
			[ 'an update of a reference table', 'UPDATE public.rparent SET id = id + 10 WHERE id = 2' ],
			[ 'an insert into a distributed table', 'INSERT INTO public.d VALUES (100, 1)' ],
			[ 'an update of one row of a distributed table', 'UPDATE public.d SET v = 0 WHERE id = 3' ],
			[ 'an update that spans groups', 'UPDATE public.d SET v = v + 1' ],
			[ 'a delete that spans groups', 'DELETE FROM public.d WHERE v > 3' ],
		  )
		{
			my ($rc, $out, $err) = run_sql($n1, "SET session_replication_role = $role;\n$case->[1]");
			isnt($rc, 0, "S6-04: $case->[0] is refused (role $role)");
			like($err, $named, '...naming the setting');
		}
	}
	# Nothing changed, and no copy differs.
	is(join(',', map { q1($_, q{SELECT string_agg(id::text, '+' ORDER BY id) FROM public.rchild}) } ($n1, $n2, $n3)), '10+20,10+20,10+20',
		'S6-04: the child table is as it was on every node');
	is(q1($n1, 'SELECT sum(v) FROM public.d'), '78', 'S6-04: and so is the distributed table');
	is(q1($n1, q{SELECT coalesce(string_agg(check_name || ' ' || node_id, ', '), '') FROM kingres.check_cluster()}), '', 'check_cluster finds nothing');
	# Reads are not affected, and a session with the role "origin" again writes.
	my ($rc, $out, $err) = run_sql($n1, "SET session_replication_role = replica;\nSELECT count(*) FROM public.d; SELECT count(*) FROM public.rparent");
	is($rc, 0, 'a read under the role replica works') or diag $err;
	($rc, $out, $err) = run_sql($n1, "SET session_replication_role = replica;\nSET session_replication_role = origin;\nDELETE FROM public.rparent WHERE id = 1");
	is($rc, 0, 'the same session writes again once the role is origin') or diag $err;
	is(join(',', map { q1($_, q{SELECT string_agg(id::text, '+' ORDER BY id) FROM public.rchild}) } ($n1, $n2, $n3)), '20,20,20',
		'the cascade ran on every node');
	# The way a superuser does what the role is for: by hand on each node, with routing off.
	for my $n ($n1, $n2, $n3)
	{
		($rc, $out, $err) = run_sql($n, "SET session_replication_role = replica;\nSET kingres_shard.enable_routing = off;\nDELETE FROM public.rparent WHERE id = 2");
		is($rc, 0, 'by hand with routing off the role replica works on ' . $n->name) or diag $err;
	}
	is(join(',', map { q1($_, q{SELECT count(*) FROM public.rchild}) } ($n1, $n2, $n3)), '1,1,1',
		'with the role replica nothing cascaded, on every node alike');

	# A cluster with one shard group has no other copy: the role changes nothing.
	my ($solo) = make_cluster('s604s', 1);
	q1($solo, q{
		CREATE TABLE public.r (id int PRIMARY KEY);
		SELECT kingres.create_reference_table('public.r');
		CREATE TABLE public.d (id int PRIMARY KEY, v int);
		SELECT kingres.create_distributed_table('public.d', 'id', bucket_count => 4);
	});
	($rc, $out, $err) = run_sql($solo, "SET session_replication_role = replica;\nINSERT INTO public.r VALUES (1); INSERT INTO public.d VALUES (1, 1)");
	is($rc, 0, 'one shard group: a write under the role replica works') or diag $err;
}

# ---------------------------------------------------------------------------
# S6-06, S6-10: a node that runs another version and refuses the claim of this node
# ---------------------------------------------------------------------------
{
	my ($n1, $n2, $n3) = make_cluster('s606', 3, logical => 1);
	q1($n1, q{
		CREATE TABLE public.t (id int PRIMARY KEY, v int);
		SELECT kingres.create_distributed_table('public.t', 'id', bucket_count => 6);
	});
	q1($n1, join('', map { "INSERT INTO public.t VALUES ($_, $_);\n" } 1 .. 12));
	# Node 2 as a node with another library would be: its objects say 0.4, and it does not
	# accept the proof of a session of this node.
	q1($n2, q{
		SET kingres_shard.enable_routing = off;
		UPDATE pg_extension SET extversion = '0.4' WHERE extname = 'kingres_shard';
		UPDATE kingres.cluster_secret SET secret = decode(repeat('ab', 40), 'hex');
	});
	my $out = q1($n1, q{SELECT check_name || '|' || node_id || '|' || detail FROM kingres.check_cluster() ORDER BY 1});
	like($out, qr/^extension_version_differs\|2\|the node runs kingres_shard 0\.4, this node runs kingres_shard 0\.5;/m,
		'S6-06: check_cluster names both versions of the node that refuses the session');
	unlike($out, qr/node_unreachable/, 'S6-06: and does not call the node unreachable');
	my $bucket = q1($n1, q{SELECT min(bucket) FROM kingres.bucket_map WHERE colocation_id = 1 AND group_id = 0});
	my ($rc, $o, $err) = run_sql($n1, "SELECT kingres.move_bucket(1, $bucket, 2)");
	isnt($rc, 0, 'S6-06: a move is refused');
	like($err, qr/node 2 \(\S+:\d+\) has kingres_shard 0\.4, a bucket move needs 0\.5 on every node/, 'S6-06: with both versions named');
	unlike($err, qr/cannot be asked/, 'S6-06: and not as a node that cannot be asked');
	# The helper is for superusers.
	q1($n1, 'CREATE ROLE alice LOGIN');
	($rc, $o, $err) = run_sql($n1, q{SELECT kingres.internal_remote_extension_version('127.0.0.1', 5432)}, extra_params => [ '-U', 'alice' ]);
	isnt($rc, 0, 'the helper that asks for a version refuses an ordinary role');
	like($err, qr/permission denied/, '...with the permission error');
	# A node that is really down is still unreachable.
	$n3->stop;
	$out = q1($n1, q{SELECT check_name || '|' || node_id FROM kingres.check_cluster() WHERE node_id = 3});
	like($out, qr/^node_unreachable\|3$/m, 'S6-06: a node that is down is still unreachable');
	$n3->start;

	# S6-10: a node whose state says no version.
	$n3->safe_psql('postgres', q{
		SET kingres_shard.enable_routing = off;
		ALTER EXTENSION kingres_shard DROP FUNCTION kingres.internal_local_state();
		DROP FUNCTION kingres.internal_local_state();
		CREATE FUNCTION kingres.internal_local_state() RETURNS jsonb LANGUAGE sql AS
		  $$ SELECT jsonb_build_object('node_id', 3, 'catalog', kingres.internal_catalog_snapshot()) $$;
		ALTER EXTENSION kingres_shard ADD FUNCTION kingres.internal_local_state();
	});
	$out = q1($n1, q{SELECT detail FROM kingres.check_cluster() WHERE check_name = 'extension_version_differs' AND node_id = 3});
	like($out, qr/^the node runs a kingres_shard that does not report its version, this node runs kingres_shard 0\.5;/,
		'S6-10: the message about a node that reports no version reads as a sentence');
	unlike($out, qr/kingres_shard an older version/, 'S6-10: not "kingres_shard an older version"');
}

# ---------------------------------------------------------------------------
# S6-07: the 0.3 scripts and the session's check_function_bodies
# ---------------------------------------------------------------------------
{
	my $node = make_node('s607');
	for my $case ([ 'on (the default)', undef, 'on' ], [ 'off', 'off', 'off' ], [ 'on, set explicitly', 'on', 'on' ])
	{
		my ($what, $set, $want) = @$case;
		my $pre = defined $set ? "SET check_function_bodies = $set;\n" : '';
		my ($rc, $out, $err) = run_sql($node, $pre
			  . "CREATE EXTENSION kingres_shard VERSION '0.2';\n"
			  . "ALTER EXTENSION kingres_shard UPDATE TO '0.3';\n"
			  . "SHOW check_function_bodies;\n"
			  . "ALTER EXTENSION kingres_shard UPDATE TO '0.5';\n"
			  . "SHOW check_function_bodies;\n"
			  . "DROP EXTENSION kingres_shard;\n"
			  . "CREATE EXTENSION kingres_shard VERSION '0.3';\n"
			  . "SHOW check_function_bodies;\n"
			  . "DROP EXTENSION kingres_shard;\n");
		is($rc, 0, "S6-07: the scripts run with check_function_bodies $what") or diag $err;
		is($out, "$want\n$want\n$want", "S6-07: the session's check_function_bodies is $want after 0.2 -> 0.3 -> 0.5 and after 0.3");
	}
	# One transaction: the rest of the chain is still checked after the function
	# whose body cannot be.  A script that would update through the whole chain at
	# once is one transaction too.
	my ($rc, $out, $err) = run_sql($node, "BEGIN;\nCREATE EXTENSION kingres_shard VERSION '0.2';\n"
		  . "ALTER EXTENSION kingres_shard UPDATE TO '0.5';\nSHOW check_function_bodies;\nROLLBACK;\n");
	is($rc, 0, 'S6-07: the whole chain in one transaction') or diag $err;
	is($out, 'on', 'S6-07: ...leaves the setting as it was');
}

# ---------------------------------------------------------------------------
# S6-09: a node of another major is not used for statements that need it
# ---------------------------------------------------------------------------
{
	my ($n1, $n2, $n3) = make_cluster('s609', 3);
	q1($n1, q{
		CREATE TABLE public.d (id int PRIMARY KEY, v int);
		SELECT kingres.create_distributed_table('public.d', 'id', bucket_count => 6);
		CREATE TABLE public.r (id int PRIMARY KEY);
		SELECT kingres.create_reference_table('public.r');
	});
	q1($n1, join('', map { "INSERT INTO public.d VALUES ($_, $_);\n" } 1 .. 12));
	my $major = q1($n1, q{SELECT current_setting('server_version_num')::int / 10000});
	my $other = $major + 1;
	# The nodes this session connects to report another major than this node's: what a node
	# that was upgraded in place looks like to its neighbours.
	my ($remote_key) = keys_of_group($n1, 1, 1, 1, 200);
	my $believe = "SET kingres_shard.fault_injection = 'peer_major:value:$other';\n";
	my $msg = qr/node \S+:\d+ runs PostgreSQL $other, this node runs PostgreSQL $major/;
	for my $case (
		[ 'a scan of a distributed table', 'SELECT count(*) FROM public.d' ],
		[ 'a write to a reference table', 'INSERT INTO public.r VALUES (1)' ],
		[ 'a write to a distributed table on another node', "INSERT INTO public.d VALUES ($remote_key, 1)" ],
	  )
	{
		my ($rc, $out, $err) = run_sql($n1, $believe . $case->[1]);
		isnt($rc, 0, "S6-09: $case->[0] is refused");
		like($err, $msg, '...naming both majors');
		like($err, qr/HINT:  Upgrade every node/, '...with what to do') if $rc != 0;
	}
	is(q1($n1, 'SELECT count(*) FROM public.r'), '0', 'S6-09: nothing was written');
	is(q1($n1, 'SELECT count(*) FROM public.d'), '12', 'S6-09: and the scan works with the right majors');
	is(prepared_count($_), '0', 'nothing is left prepared on ' . $_->name) for ($n1, $n2, $n3);
	# What a statement that needs no other node does is not affected.
	my ($rc, $out, $err) = run_sql($n1, $believe . q{SELECT 1; SELECT 2});
	is($rc, 0, 'a statement that needs no other node runs') or diag $err;
	is(q1($n1, q{SELECT count(*) FROM kingres.check_cluster() WHERE check_name = 'server_major_differs'}), '0', 'check_cluster has nothing to report here');
}

done_testing();
