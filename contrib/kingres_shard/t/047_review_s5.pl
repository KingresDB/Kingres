# SPDX-License-Identifier: PostgreSQL
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/047_review_s5.pl
#
# The findings of the review of 0.4 (kingres/docs/reviews/2026-11-15-kingres-shard-0.4.md):
#
#	S5-01	a trigger on a reference table is refused, with routing off too
#	S5-02	nodes of two versions: the message names the versions, and
#			check_cluster() does not call equal copies different
#	S5-03	internal_partition_identity() needs the right to read the table
#	S5-04	the warning after a stalled participant names the one that is prepared
#	S5-05	a cancel of a move is honoured before the switch, and says what happened
#	S5-06	wording of the refusal on a reference table; a node without a secret
#	S3-14	a floating point aggregate in a write of a reference table

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use Time::HiRes qw(time);
use KingresShardTest qw(make_cluster keys_of_group run_sql prepared_count wait_prepared_count
  wait_for_fault_file move_state);

my $tmp = $PostgreSQL::Test::Utils::tmp_check;

# Every process of a node: the postmaster and its children.
sub node_pids
{
	my ($node) = @_;
	my $pm = $node->{_pid};
	my @kids = split ' ', scalar(readpipe("ps -o pid= --ppid $pm"));
	return ($pm, @kids);
}

# SQL as an ordinary role.
sub as_role
{
	my ($node, $role, $sql) = @_;
	return run_sql($node, $sql, extra_params => [ '-U', $role ]);
}

# ---------------------------------------------------------------------------
# S5-04: a reference-table write whose participant is stopped after the local
# commit: the warnings name the stopped node and no other.
# ---------------------------------------------------------------------------
{
	my ($n1, $n2, $n3) = make_cluster('s504', 3, extra => "kingres_shard.node_timeout = 3\n");
	$n1->safe_psql('postgres', q{
		CREATE TABLE public.rr (id int PRIMARY KEY, val text);
		SELECT kingres.create_reference_table('public.rr');
	});
	$n1->safe_psql('postgres', 'SELECT kingres.recover_prepared_transactions()');

	my $key = 0;
	for my $frozen ($n2, $n3)
	{
		my $other = $frozen == $n2 ? $n3 : $n2;
		my $gate = "$tmp/gate_s504_$$";
		append_to_file($gate, 'x');
		$key++;
		my $h = $n1->background_psql('postgres', on_error_stop => 0, timeout => 60);
		$h->query_until(qr/started/,
			"SET kingres_shard.fault_injection = 'after_commit:wait:$gate';\n"
			  . "BEGIN; INSERT INTO public.rr VALUES ($key, 'a');\n"
			  . "\\echo started\nCOMMIT;\n");
		ok(wait_prepared_count($n2, 1), 'prepared on node 2');
		ok(wait_prepared_count($n3, 1), 'prepared on node 3');
		my @pids = node_pids($frozen);
		kill 'STOP', @pids;
		unlink $gate;
		eval { $h->query('SELECT 1') };
		my $err = $h->{stderr};
		# Look before the frozen node answers again.
		my $left_other = prepared_count($other);
		kill 'CONT', @pids;
		$h->quit;
		is($left_other, '0', 'the node that answered has committed (' . $other->name . ')');
		my @named = $err =~ /on node [^:]+:(\d+); recovery will commit it/g;
		is(join(',', @named), $frozen->port,
			'S5-04: the warnings name the stopped node only (' . $frozen->name . ')');
		$n1->safe_psql('postgres', 'SELECT kingres.recover_prepared_transactions()');
		ok(wait_prepared_count($frozen, 0), 'the stopped node is resolved once it answers');
	}
}

# ---------------------------------------------------------------------------
# S5-01, S5-03, S5-06, S3-14 on one cluster.
# ---------------------------------------------------------------------------
{
	my ($n1, $n2, $n3) = make_cluster('s501', 3);
	$_->safe_psql('postgres', 'CREATE ROLE alice LOGIN; CREATE ROLE bob LOGIN') for ($n1, $n2, $n3);
	$n1->safe_psql('postgres', q{
		CREATE TABLE public.d (id int PRIMARY KEY, v int, f float8);
		SELECT kingres.create_distributed_table('public.d', 'id', bucket_count => 6);
		CREATE TABLE public.r (id int PRIMARY KEY, seen int, at timestamptz);
		SELECT kingres.create_reference_table('public.r');
		CREATE TABLE public.rf (id int PRIMARY KEY, f float8);
		SELECT kingres.create_reference_table('public.rf');
		GRANT SELECT ON public.d TO alice;
	});
	$n1->safe_psql('postgres', join('', map { "INSERT INTO public.d VALUES ($_, $_, $_ / 7.0);\n" } 1 .. 30));

	# S5-01: the trigger of the review (a count of a distributed table, a clock).
	$_->safe_psql('postgres', q{
		CREATE FUNCTION public.cnt() RETURNS trigger LANGUAGE plpgsql AS
		  $$ BEGIN NEW.seen := (SELECT count(*) FROM public.d); NEW.at := clock_timestamp(); RETURN NEW; END $$;
	}) for ($n1, $n2, $n3);
	for my $routing ('on', 'off')
	{
		my ($rc, $out, $err) = run_sql($n1, "SET kingres_shard.enable_routing = $routing;\n"
			  . "CREATE TRIGGER t BEFORE INSERT ON public.r FOR EACH ROW EXECUTE FUNCTION public.cnt()");
		isnt($rc, 0, "S5-01: CREATE TRIGGER on a reference table is refused (routing $routing)");
		like($err, qr/a trigger on reference table "r" is not supported/, 'with the reason');
		($rc, $out, $err) = run_sql($n1, "SET kingres_shard.enable_routing = $routing;\n"
			  . "CREATE CONSTRAINT TRIGGER tc AFTER INSERT ON public.r FOR EACH ROW EXECUTE FUNCTION public.cnt()");
		like($err, qr/a trigger on reference table "r" is not supported/, "a constraint trigger too (routing $routing)");
	}
	is($n2->safe_psql('postgres', q{SELECT count(*) FROM pg_trigger WHERE tgrelid = 'public.r'::regclass AND NOT tgisinternal}),
		'0', 'no trigger was made on node 2');
	$n1->safe_psql('postgres', q{INSERT INTO public.r VALUES (1, 0, '2026-01-01')});
	my @rows = map { $_->safe_psql('postgres', q{SELECT id || '|' || seen FROM public.r}) } ($n1, $n2, $n3);
	is(join(',', @rows), '1|0,1|0,1|0', 'the copies of the reference table are equal');
	# A trigger on a distributed table is the user's business, as before.
	my ($rc, $out, $err) = run_sql($n1, q{CREATE TRIGGER t BEFORE INSERT ON public.d FOR EACH ROW EXECUTE FUNCTION suppress_redundant_updates_trigger()});
	like($err, qr/CREATE TRIGGER is not supported on distributed tables yet/, 'a trigger on a distributed table is still refused as before');

	# S5-03.
	($rc, $out, $err) = as_role($n1, 'bob', q{SELECT kingres.internal_partition_identity('public.d'::regclass)});
	isnt($rc, 0, 'S5-03: a role without the right to read the table is refused');
	like($err, qr/permission denied for the internal functions of kingres_shard/, 'with the message of the other helpers');
	($rc, $out, $err) = as_role($n1, 'bob', q{SELECT kingres.internal_partition_identity('pg_class'::regclass)});
	isnt($rc, 0, 'nor is it an oracle for a table that is not distributed');
	($rc, $out, $err) = as_role($n1, 'alice', q{SELECT kingres.internal_partition_identity('public.d'::regclass)});
	is($rc, 0, 'a role that may read the table gets the answer') or diag $err;
	($rc, $out, $err) = as_role($n1, 'alice', q{SELECT count(*) FROM public.d});
	is($out, '30', 'and its distributed scan, which asks the other nodes for it, works');

	# S5-06: the wording.
	($rc, $out, $err) = run_sql($n1, q{ALTER TABLE public.r ADD COLUMN c timestamptz DEFAULT 'now'});
	like($err, qr/ALTER TABLE is not supported on reference tables yet/, 'S5-06: the refusal on a reference table says so');
	($rc, $out, $err) = run_sql($n1, q{ALTER TABLE public.d ALTER COLUMN v TYPE bigint});
	like($err, qr/ALTER TABLE is not supported on distributed tables yet/, 'and the one on a distributed table too');

	# S5-06: a node without the secret is named.
	is($n1->safe_psql('postgres', q{SELECT count(*) FROM kingres.check_cluster() WHERE check_name = 'secret_missing'}),
		'0', 'S5-06: no node lacks the secret');
	$n2->safe_psql('postgres', q{SET kingres_shard.enable_routing = off; DELETE FROM kingres.cluster_secret});
	is($n1->safe_psql('postgres', q{SELECT check_name || ':' || node_id FROM kingres.check_cluster() WHERE check_name = 'secret_missing'}),
		'secret_missing:2', 'check_cluster names the node that holds no secret');
	$n1->safe_psql('postgres', 'SELECT kingres.sync_catalog()');
	is($n1->safe_psql('postgres', q{SELECT count(*) FROM kingres.check_cluster() WHERE check_name = 'secret_missing'}),
		'0', 'sync_catalog gives it back');

	# S3-14: a floating point aggregate over a distributed table in a write of a reference table.
	($rc, $out, $err) = run_sql($n1, q{INSERT INTO public.rf SELECT 1, sum(f) FROM public.d});
	my @copies = map { $_->safe_psql('postgres', q{SELECT coalesce(string_agg(f::text, ','), '-') FROM public.rf}) } ($n1, $n2, $n3);
	if ($rc != 0)
	{
		like($err, qr/writes reference table "rf" and reads a distributed table is not supported/, "S3-14: the write of a floating point aggregate over a distributed table is refused");
		is(join(' ', @copies), '- - -', 'and nothing was written');
	}
	else
	{
		is(scalar(keys %{{ map { $_ => 1 } @copies }}), 1, 'S3-14: the write was accepted, and the copies are equal: ' . join(' ', @copies));
	}
}

# ---------------------------------------------------------------------------
# S5-02: nodes of two versions.
# ---------------------------------------------------------------------------
{
	my ($n1, $n2, $n3) = make_cluster('s502', 3);
	$_->safe_psql('postgres', 'CREATE ROLE alice LOGIN') for ($n1, $n2, $n3);
	$n1->safe_psql('postgres', q{
		CREATE TABLE public.d (id int PRIMARY KEY, v int);
		SELECT kingres.create_distributed_table('public.d', 'id', bucket_count => 6);
		CREATE TABLE public.r (id int PRIMARY KEY, v int);
		SELECT kingres.create_reference_table('public.r');
		GRANT SELECT ON public.d TO alice;
		INSERT INTO public.r VALUES (1, 1), (2, 2);
	});
	my $problems = sub {
		return $n1->safe_psql('postgres', q{
			SELECT coalesce(string_agg(check_name || ':' || node_id, ' ' ORDER BY check_name, node_id), '')
			  FROM kingres.check_cluster()});
	};
	is($problems->(), '', 'a cluster of one version has no problem');

	# Node 2 as an older version would be: its objects say 0.3, and it lacks the helper
	# that 0.3 lacks; with another library it would not accept the secret either.
	$n2->safe_psql('postgres', q{
		SET kingres_shard.enable_routing = off;
		UPDATE pg_extension SET extversion = '0.3' WHERE extname = 'kingres_shard';
		ALTER EXTENSION kingres_shard DROP FUNCTION kingres.internal_reference_digest(text);
		DROP FUNCTION kingres.internal_reference_digest(text);
		ALTER EXTENSION kingres_shard DROP FUNCTION kingres.internal_local_state();
		DROP FUNCTION kingres.internal_local_state();
		CREATE FUNCTION kingres.internal_local_state() RETURNS jsonb LANGUAGE sql AS
		  $$ SELECT jsonb_build_object('node_id', 2, 'catalog', kingres.internal_catalog_snapshot(),
		       'partitions', '[]'::jsonb, 'standalone', '[]'::jsonb, 'orphans', '[]'::jsonb, 'slots', '[]'::jsonb,
		       'subscriptions', '[]'::jsonb, 'publications', '[]'::jsonb, 'origins', '[]'::jsonb, 'prepared', '[]'::jsonb) $$;
		ALTER EXTENSION kingres_shard ADD FUNCTION kingres.internal_local_state();
	});
	my $p = $problems->();
	like($p, qr/extension_version_differs:2/, 'S5-02: check_cluster reports the node of another version');
	unlike($p, qr/reference_table_differs/, 'and does not call the equal copies of the reference table different');

	# The refusal of a session that the node does not accept, with the node's version in it.
	$n2->safe_psql('postgres', q{SET kingres_shard.enable_routing = off; UPDATE kingres.cluster_secret SET secret = decode(repeat('ab', 40), 'hex')});
	my ($rc, $out, $err) = run_sql($n1, q{SELECT count(*) FROM public.d}, extra_params => [ '-U', 'alice' ]);
	isnt($rc, 0, 'a scan that needs the node fails');
	like($err, qr/node \S+:\d+ runs kingres_shard 0\.3, this node runs 0\.4/, 'S5-02: and the message names the version of the node');
}

# ---------------------------------------------------------------------------
# S5-05: a cancel that the move does not honour before its switch.
# ---------------------------------------------------------------------------
{
	my @nodes = make_cluster('s505', 3, logical => 1, extra => "kingres_shard.node_timeout = 2\n");
	my ($n1) = @nodes;
	$n1->safe_psql('postgres', q{
		CREATE TABLE public.t (id int PRIMARY KEY, v int);
		SELECT kingres.create_distributed_table('public.t', 'id', bucket_count => 6);
	});
	$n1->safe_psql('postgres', join('', map { "INSERT INTO public.t VALUES ($_, $_);\n" } 1 .. 12));
	my $b = 1;
	my $src = $n1->safe_psql('postgres', "SELECT group_id FROM kingres.bucket_map WHERE colocation_id = 1 AND bucket = $b");
	my $tgt = ($src + 1) % 3;
	my $file = "$tmp/wait_s505";
	open(my $fh, '>', $file) or die;
	close($fh);
	my $pid = fork();
	die "fork" unless defined $pid;
	if ($pid == 0)
	{
		my $sql = "SET kingres_shard.fault_injection = 'move_after_catchup:wait:$file'; SELECT kingres.move_bucket(1, $b, $tgt)";
		system("psql -X -q -h " . $n1->host . " -p " . $n1->port . " -d postgres -c \"$sql\" > $tmp/child_s505.out 2>&1");
		POSIX::_exit(0);
	}
	ok(wait_for_fault_file("$file.reached", 60), 'the move reached the end of its catch-up');
	my $id = $n1->safe_psql('postgres', 'SELECT max(move_id) FROM kingres.bucket_move');
	my ($rc, $out, $err) = run_sql($n1, "SELECT kingres.cancel_bucket_move($id)");
	is($rc, 0, 'the cancel returns') or diag $err;
	like($err, qr/request to stop bucket move $id is recorded/, 'S5-05: it says the request is recorded, not that the move stopped');
	unlink($file);
	waitpid($pid, 0);
	like(move_state($n1, $id), qr/^cancelled\|/, 'S5-05: the move did not enter its switch: it is cancelled');
	is($n1->safe_psql('postgres', "SELECT group_id FROM kingres.bucket_map WHERE colocation_id = 1 AND bucket = $b"),
		$src, 'and the bucket stayed where it was');
	is($n1->safe_psql('postgres', 'SELECT count(*) FROM public.t'), '12', 'with all the rows');

	# A move that is past the point: the cancel says so.
	$file = "$tmp/wait_s505b";
	open($fh, '>', $file) or die;
	close($fh);
	$pid = fork();
	die "fork" unless defined $pid;
	if ($pid == 0)
	{
		my $sql = "SET kingres_shard.fault_injection = 'move_switch_after_lock:wait:$file'; SELECT kingres.move_bucket(1, $b, $tgt)";
		system("psql -X -q -h " . $n1->host . " -p " . $n1->port . " -d postgres -c \"$sql\" > $tmp/child_s505b.out 2>&1");
		POSIX::_exit(0);
	}
	ok(wait_for_fault_file("$file.reached", 60), 'the second move reached its switch');
	$id = $n1->safe_psql('postgres', 'SELECT max(move_id) FROM kingres.bucket_move');
	($rc, $out, $err) = run_sql($n1, "SELECT kingres.cancel_bucket_move($id)");
	isnt($rc, 0, 'a cancel of a move that is switching fails');
	like($err, qr/is switching and cannot be cancelled/, 'with the reason');
	unlink($file);
	waitpid($pid, 0);
	like(move_state($n1, $id), qr/^(cleaning|done)\|/, 'and the move completes');
}

done_testing();
