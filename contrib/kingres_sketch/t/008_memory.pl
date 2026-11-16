# SPDX-License-Identifier: AGPL-3.0-only
# contrib/kingres_sketch/t/008_memory.pl
#
# The memory of aggregate states (K12S-03): one state per group and per
# parallel worker, not limited by work_mem, bounded by the setting
# kingres_sketch.state_memory_limit.
#
#   * the reproduction of the review: many groups of a topn aggregate with
#     large items.  With the default limit the query runs (and the peak of the
#     backend, VmHWM, shows what it needs); with a low limit it fails with
#     54000 and the peak of a fresh backend stays near the limit;
#   * parallel workers: each is a backend with its own count.  A limit below
#     what one worker needs fails in the worker (the error carries the context
#     "parallel worker"); a limit above what any worker needs but below what the
#     leader holds after the deserialisation and the combine fails in the
#     leader; in both cases the counter of the session is zero afterwards;
#   * who may raise the limit, by SET, by the options of a connection (a
#     setting made before the library is loaded too) and by ALTER ROLE;
#   * many failing queries in a row leave the counter at zero.
#
# Nothing here depends on timing.
#
# Portions Copyright (c) 2026, RooyeKhat Media Co
# SPDX-License-Identifier: AGPL-3.0-only

use strict;
use warnings FATAL => 'all';

use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;

my $node = PostgreSQL::Test::Cluster->new('memory');
$node->init;
$node->append_conf('postgresql.conf',
	"max_worker_processes = 8\nmax_parallel_workers = 8\n");
$node->start;
$node->safe_psql('postgres', 'CREATE EXTENSION kingres_sketch');
$node->safe_psql('postgres', <<'EOS');
CREATE ROLE ks_user LOGIN;
GRANT USAGE ON SCHEMA kingres_sketch TO ks_user;
EOS

my $proc = -r '/proc/self/status';

# One session: the given statements, then the peak of its own backend in kB
# (VmHWM) before and after, printed by psql itself.
sub session_peak
{
	my ($sql) = @_;
	my ($out, $err);
	$node->psql(
		'postgres',
		"SELECT pg_backend_pid() AS pid \\gset\n\\setenv P :pid\n"
		  . "\\echo before \\\\ \n\\! grep VmHWM /proc/\$P/status\n"
		  . "$sql\n"
		  . "\\echo after\n\\! grep VmHWM /proc/\$P/status\n",
		stdout => \$out,
		stderr => \$err,
		on_error_stop => 0);
	my ($before, $after) = $out =~ /VmHWM:\s+(\d+) kB.*VmHWM:\s+(\d+) kB/s;
	return ($before, $after, $out, $err);
}

my $groups = <<'EOS';
SET work_mem = '4MB'; SET max_parallel_workers_per_gather = 0; SET enable_sort = off;
SELECT sum(kingres_sketch.topn_total(t)) FROM (SELECT grp, kingres_sketch.topn_add_agg(repeat('x', 900) || i::text, 100000) AS t
  FROM (SELECT i, i % 300 AS grp FROM generate_series(1, 600000) i) s GROUP BY grp) q;
SELECT kingres_sketch.state_bytes();
EOS

SKIP:
{
	skip 'no /proc', 6 unless $proc;

	# the default limit leaves the query alone; its peak is what it needs
	my ($b1, $a1, $o1, $e1) = session_peak($groups);
	like($o1, qr/600000/, 'default limit: the query with 300 large states runs');
	is($e1, '', 'default limit: no error');
	cmp_ok($a1 - $b1, '>', 300_000, 'default limit: the peak of the backend is hundreds of MB (the review: 659 MB)');

	# a low limit stops it, cleanly, and the peak stays near the limit
	my ($b2, $a2, $o2, $e2) = session_peak(
		"SET kingres_sketch.state_memory_limit = '64MB';\n$groups");
	like($e2, qr/ERROR:  aggregate states of kingres_sketch hold \d+ bytes in this backend, over the limit of 67108864 bytes/,
		'limit 64MB: the query fails with the message');
	like($e2, qr/HINT:  .*kingres_sketch\.state_memory_limit/s, 'the hint names the setting');
	cmp_ok($a2 - $b2, '<', 160_000,
		"limit 64MB: the peak of a fresh backend stays bounded (" . ($a2 - $b2) . " kB)");
}

# the SQLSTATE
my ($ret, $out, $err);
$node->psql('postgres',
	"SET kingres_sketch.state_memory_limit = '1MB';\n"
	  . "DO \$\$ BEGIN PERFORM kingres_sketch.topn_add_agg(repeat('x', 500) || i::text, 100000) FROM generate_series(1, 20000) i; "
	  . "EXCEPTION WHEN OTHERS THEN RAISE NOTICE 'sqlstate %', SQLSTATE; END \$\$;",
	stderr => \$err);
like($err, qr/NOTICE:  sqlstate 54000/, 'SQLSTATE 54000 (program_limit_exceeded)');

# ---- parallel workers ----

$node->safe_psql('postgres', <<'EOS');
CREATE TABLE big AS SELECT i, repeat('x', 200) || i::text AS item FROM generate_series(1, 20000) i;
ALTER TABLE big SET (parallel_workers = 2);
ANALYZE big;
EOS
my $par = "SET max_parallel_workers_per_gather = 2; SET parallel_setup_cost = 0; SET parallel_tuple_cost = 0; "
  . "SET min_parallel_table_scan_size = 0; SET parallel_leader_participation = off;";
my $q = "SELECT kingres_sketch.topn_total(kingres_sketch.topn_add_agg(item, 20000)) FROM big";

like($node->safe_psql('postgres', "$par EXPLAIN (COSTS OFF) $q"),
	qr/Finalize Aggregate.*Gather.*Partial Aggregate/s,
	'the forced plan is a Gather over partial aggregates');

# S: the bytes a serial run holds (the states stay while a cursor is open)
my $S = $node->safe_psql('postgres', <<"EOS");
SET max_parallel_workers_per_gather = 0;
BEGIN;
DECLARE c CURSOR FOR SELECT kingres_sketch.topn_total(t) FROM (SELECT kingres_sketch.topn_add_agg(item, 20000) AS t FROM big GROUP BY i % 1) q;
FETCH 1 FROM c;
SELECT kingres_sketch.state_bytes();
CLOSE c;
COMMIT;
EOS
$S =~ s/.*\n//s if $S =~ /\n/;
cmp_ok($S, '>', 1_000_000, "a serial state holds $S bytes");
is($node->safe_psql('postgres', "SET max_parallel_workers_per_gather = 0; $q"),
	'20000', 'serial run of the parallel test query');

# limit 1.3 * S: every worker holds at most S and passes, the leader holds the
# deserialised states and the combined state (about 2 * S) and fails
my $kb_leader = int(1.3 * $S / 1024);
($ret, $out, $err) = $node->psql('postgres',
	"$par SET kingres_sketch.state_memory_limit = '${kb_leader}kB'; $q;\nSELECT kingres_sketch.state_bytes();",
	on_error_stop => 0);
like($err, qr/ERROR:  aggregate states of kingres_sketch hold/, 'limit above any worker, below the leader: fails');
unlike($err, qr/CONTEXT:  parallel worker/, 'it fails in the leader, which counts its own states');
like($out, qr/^0$/m, 'the counter of the session is zero afterwards');

# limit 0.3 * S: a worker holds at least S / 2 and fails in the worker
my $kb_worker = int(0.3 * $S / 1024);
($ret, $out, $err) = $node->psql('postgres',
	"$par SET kingres_sketch.state_memory_limit = '${kb_worker}kB'; $q;\nSELECT kingres_sketch.state_bytes();",
	on_error_stop => 0);
like($err, qr/ERROR:  aggregate states of kingres_sketch hold/, 'limit below a worker: fails');
like($err, qr/CONTEXT:  parallel worker/, 'it fails in a worker, which counts its own states');
like($out, qr/^0$/m, 'the counter of the session is zero afterwards');

# a limit of 5 * S lets the same parallel query run, and leaves zero
is($node->safe_psql('postgres',
		"$par SET kingres_sketch.state_memory_limit = '" . int(5 * $S / 1024) . "kB'; $q;"),
	'20000', 'limit above what the leader holds: the parallel query runs');
is($node->safe_psql('postgres', "$par $q; SELECT kingres_sketch.state_bytes();"),
	"20000\n0", 'the counter is zero after a completed parallel query');

# ---- who may raise it ----

sub as_user
{
	my ($sql, %opt) = @_;
	my ($o, $e);
	local $ENV{PGOPTIONS} = $opt{options} if defined $opt{options};
	$node->psql('postgres', $sql, stdout => \$o, stderr => \$e,
		extra_params => ['-U', 'ks_user'], on_error_stop => 0);
	return ($o, $e);
}

# the library is loaded first: before that, a setting of the prefix is a placeholder and is judged when the library loads (below)
my $load = "SELECT kingres_sketch.state_bytes(); ";
($out, $err) = as_user($load . "SET kingres_sketch.state_memory_limit = '2GB';");
like($err, qr/permission denied to raise "kingres_sketch.state_memory_limit"/, 'a role cannot raise the limit with SET');
($out, $err) = as_user($load . "SET kingres_sketch.state_memory_limit = '10MB'; SHOW kingres_sketch.state_memory_limit; "
	  . "RESET kingres_sketch.state_memory_limit; SHOW kingres_sketch.state_memory_limit;");
is($err, '', 'a role can lower it and reset it');
like($out, qr/10MB.*1GB/s, 'the values are 10MB, then the default again');

# a setting made at the connection, before the library is loaded in the session
($out, $err) = as_user("SELECT kingres_sketch.state_bytes(); SHOW kingres_sketch.state_memory_limit;",
	options => '-c kingres_sketch.state_memory_limit=8GB');
like($err, qr/WARNING:  permission denied to raise/, 'a raise in the connection options is refused when the library loads');
like($out, qr/1GB/, 'and the limit is the default');
($out, $err) = as_user("SELECT kingres_sketch.state_bytes(); SHOW kingres_sketch.state_memory_limit;",
	options => '-c kingres_sketch.state_memory_limit=10MB');
like($out, qr/10MB/, 'a lower value in the connection options holds');

# the administrator gives a role a higher ceiling by ALTER ROLE
$node->safe_psql('postgres', "ALTER ROLE ks_user SET kingres_sketch.state_memory_limit = '4GB'");
($out, $err) = as_user($load . "SHOW kingres_sketch.state_memory_limit; SET kingres_sketch.state_memory_limit = '3GB'; "
	  . "SHOW kingres_sketch.state_memory_limit; SET kingres_sketch.state_memory_limit = '5GB'; "
	  . "RESET kingres_sketch.state_memory_limit; SHOW kingres_sketch.state_memory_limit;");
like($out, qr/4GB.*3GB.*4GB/s, 'the role setting is the ceiling: 4GB, 3GB, then 4GB after RESET');
like($err, qr/permission denied to raise.*4194304 kB/s, 'above the role setting it is refused');
($out, $err) = as_user($load . "ALTER ROLE ks_user SET kingres_sketch.state_memory_limit = '8GB';");
like($err, qr/permission denied/, "a role cannot raise its own stored setting");
$node->safe_psql('postgres', "ALTER ROLE ks_user RESET kingres_sketch.state_memory_limit");

# ---- no leak over many failures ----

my $leak = join('', map { "SELECT kingres_sketch.topn_total(kingres_sketch.topn_add_agg(repeat('x', 300) || i::text, 5000)) FROM generate_series(1, 5000) i;\n" } 1 .. 40);
($ret, $out, $err) = $node->psql('postgres',
	"SET kingres_sketch.state_memory_limit = '1MB';\n$leak"
	  . "SELECT kingres_sketch.state_bytes();",
	on_error_stop => 0);
my $nerr = () = $err =~ /ERROR:  aggregate states/g;
is($nerr, 40, 'forty failing queries in one session');
like($out, qr/^0$/m, 'the counter is zero after forty failures');

$node->stop;
done_testing();
