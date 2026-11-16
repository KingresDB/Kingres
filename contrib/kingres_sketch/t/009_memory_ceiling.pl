# SPDX-License-Identifier: AGPL-3.0-only
# contrib/kingres_sketch/t/009_memory_ceiling.pl
#
# K12T-01: no role that is not a superuser can make a backend of its own run
# with a higher limit on the memory of aggregate states than the
# administrator allowed, by any means.
#
# The rule is two settings: kingres_sketch.state_memory_ceiling (PGC_SUSET,
# the administrator's) and kingres_sketch.state_memory_limit (any role's own);
# the limit in force is the smaller one (state_limit()).  Every way a role can
# try to raise the limit is tried below, and after each the NEXT session of the
# role is opened: it must report the administrator's limit and refuse, with
# SQLSTATE 54000, a query that the administrator's limit forbids.  (An
# earlier test let ALTER ROLE of a role for itself succeed without looking at
# the next session, which is where the earlier design failed.)
#
# Everything runs twice: on a server that preloads the library, and on one
# where the library is loaded by the first function call of a session (the
# settings then do not exist before the load, which PostgreSQL treats
# differently: a value for an unknown setting is a placeholder).
#
# Nothing here depends on timing.
#
# Portions Copyright (c) 2026, RooyeKhat Media Co

use strict;
use warnings FATAL => 'all';

use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;

# The limit the administrator gives in the configuration file.
my $ceiling_kb = 4096;
my $ceiling_bytes = $ceiling_kb * 1024;

# A query whose states need about 9 MB: refused under the ceiling, run under
# a larger limit.  It is a DO block that reports what happened.
my $big = q{DO $$ BEGIN
  PERFORM kingres_sketch.topn_total(t) FROM (SELECT kingres_sketch.topn_add_agg(repeat('x', 200) || i::text, 40000) AS t
    FROM generate_series(1, 40000) i GROUP BY i % 4) q;
  RAISE NOTICE 'ran';
EXCEPTION WHEN program_limit_exceeded THEN RAISE NOTICE 'refused 54000';
END $$;};

sub run_as
{
	my ($node, $user, $sql, %o) = @_;
	my ($out, $err);
	local $ENV{PGOPTIONS} = $o{options} if defined $o{options};
	my $ret = $node->psql($o{db} // 'postgres', $sql, stdout => \$out,
		stderr => \$err, extra_params => ['-U', $user], on_error_stop => 0);
	$out //= '';
	$err //= '';
	return ($out, $err, $ret);
}

# The next session of a role: the limit in force and what the big query does.
sub next_session
{
	my ($node, $user, %o) = @_;
	my ($out, $err) = run_as($node, $user,
		"SELECT kingres_sketch.state_limit();\n$big", %o);
	my ($limit) = $out =~ /^(\d+)$/m;
	my $refused = $err =~ /refused 54000/ ? 1 : 0;
	my $ran = $err =~ /NOTICE:  ran/ ? 1 : 0;
	return ($limit, $refused, $ran, $err);
}

sub same_as_admin
{
	my ($node, $user, $what, %o) = @_;
	my ($limit, $refused, $ran, $err) = next_session($node, $user, %o);
	is($limit, $ceiling_bytes, "$what: the next session runs with the administrator's limit");
	is($refused, 1, "$what: the next session refuses the big query (54000)");
}

sub matrix
{
	my ($preloaded) = @_;
	my $tag = $preloaded ? 'preloaded' : 'loaded by first call';
	my $node = PostgreSQL::Test::Cluster->new($preloaded ? 'ceil_pre' : 'ceil_lazy');
	$node->init;
	$node->append_conf('postgresql.conf', "max_worker_processes = 8\nmax_parallel_workers = 8\n");
	$node->append_conf('postgresql.conf', "shared_preload_libraries = 'kingres_sketch'\n") if $preloaded;
	# the administrator's ceiling, in the configuration file
	$node->append_conf('postgresql.conf', "kingres_sketch.state_memory_ceiling = '${ceiling_kb}kB'\n");
	$node->start;
	$node->safe_psql('postgres', <<'EOS');
CREATE EXTENSION kingres_sketch;
CREATE ROLE low LOGIN;
CREATE ROLE maker LOGIN CREATEROLE;
CREATE ROLE dbo LOGIN CREATEDB;
CREATE ROLE grantee LOGIN;
CREATE ROLE sup2 LOGIN SUPERUSER;
CREATE ROLE member_of_sup NOLOGIN;
CREATE ROLE viaset LOGIN;
GRANT sup2 TO viaset;
GRANT USAGE ON SCHEMA kingres_sketch TO PUBLIC;
GRANT CREATE ON SCHEMA public TO PUBLIC;
CREATE DATABASE dbo_db OWNER dbo;
EOS
	$node->safe_psql('dbo_db', "CREATE EXTENSION kingres_sketch; GRANT USAGE ON SCHEMA kingres_sketch TO PUBLIC; GRANT CREATE ON SCHEMA public TO PUBLIC;");
	$node->safe_psql('postgres', "GRANT CONNECT ON DATABASE dbo_db TO PUBLIC");
	my ($out, $err);

	# the baseline: the administrator's value, a query it forbids
	same_as_admin($node, 'low', "[$tag] baseline");

	# --- SET, SET LOCAL, set_config in one session (the library may or may not be loaded yet) ---
	for my $stmt (
		"SET kingres_sketch.state_memory_ceiling = '2GB';",
		"BEGIN; SET LOCAL kingres_sketch.state_memory_ceiling = '2GB';",
		"SELECT set_config('kingres_sketch.state_memory_ceiling', '2GB', false);",
		"BEGIN; SELECT set_config('kingres_sketch.state_memory_ceiling', '2GB', true);")
	{
		# first statement in the session: the setting may be a placeholder
		# now (on a server that preloads the library it never is)
		if (!$preloaded)
		{
			($out, $err) = run_as($node, 'low',
				"$stmt\nSELECT kingres_sketch.state_limit();\n$big\nCOMMIT;");
			unlike($out, qr/^2147483648$/m, "[$tag] '$stmt' does not raise the limit in this session");
			like($out, qr/^$ceiling_bytes$/m, "[$tag] '$stmt': the limit is the administrator's");
			like($err, qr/refused 54000/, "[$tag] '$stmt': the big query is refused");
		}
		# and with the library loaded first
		($out, $err) = run_as($node, 'low',
			"SELECT kingres_sketch.state_bytes();
$stmt
ROLLBACK;
SELECT kingres_sketch.state_limit();
$big");
		like($err, qr/permission denied to set parameter/, "[$tag] '$stmt' is refused with the library loaded");
		like($out, qr/^$ceiling_bytes$/m, "[$tag] '$stmt', loaded: the limit is the administrator's");
	}
	same_as_admin($node, 'low', "[$tag] after the SET attempts");

	# --- the role's own limit may be set to anything and changes nothing upward ---
	($out, $err) = run_as($node, 'low',
		"SET kingres_sketch.state_memory_limit = '2GB';\nSELECT kingres_sketch.state_limit();");
	like($out, qr/^$ceiling_bytes$/m, "[$tag] the own limit above the ceiling is capped by the ceiling");
	($out, $err) = run_as($node, 'low',
		"SET kingres_sketch.state_memory_limit = '1MB';\nSELECT kingres_sketch.state_limit();");
	like($out, qr/^1048576$/m, "[$tag] the own limit below the ceiling lowers the limit");

	# --- ALTER ROLE for itself, then the next session ---
	($out, $err) = run_as($node, 'low', "ALTER ROLE low SET kingres_sketch.state_memory_ceiling = '2GB';");
	like($err, qr/permission denied to set parameter/, "[$tag] ALTER ROLE <self> SET ceiling is refused (library not loaded in that session)");
	same_as_admin($node, 'low', "[$tag] after ALTER ROLE <self> SET ceiling (not loaded)");
	($out, $err) = run_as($node, 'low',
		"SELECT kingres_sketch.state_bytes();\nALTER ROLE low SET kingres_sketch.state_memory_ceiling = '2GB';");
	like($err, qr/permission denied to set parameter/, "[$tag] ALTER ROLE <self> SET ceiling is refused (loaded)");
	same_as_admin($node, 'low', "[$tag] after ALTER ROLE <self> SET ceiling (loaded)");
	($out, $err) = run_as($node, 'low', "ALTER ROLE low SET kingres_sketch.state_memory_limit = '2GB';");
	if ($preloaded)
	{
		is($err, '', "[$tag] ALTER ROLE <self> SET limit (the role's own) is allowed");
	}
	else
	{
		# PostgreSQL: a value for a setting that is not known yet (the library is not loaded in
		# this session) is a placeholder that only a superuser may store, for any prefix
		like($err, qr/permission denied to set parameter/, "[$tag] ALTER ROLE <self> SET limit is refused while the library is not loaded in the session");
		($out, $err) = run_as($node, 'low', "SELECT kingres_sketch.state_bytes();
ALTER ROLE low SET kingres_sketch.state_memory_limit = '2GB';");
		is($err, '', "[$tag] ALTER ROLE <self> SET limit is allowed once the library is loaded");
	}
	same_as_admin($node, 'low', "[$tag] after ALTER ROLE <self> SET limit = 2GB: still the ceiling");
	($out, $err) = run_as($node, 'low', "ALTER ROLE low RESET kingres_sketch.state_memory_limit;");
	($out, $err) = run_as($node, 'low',
		"ALTER ROLE low IN DATABASE postgres SET kingres_sketch.state_memory_ceiling = '2GB';");
	like($err, qr/permission denied/, "[$tag] ALTER ROLE <self> IN DATABASE SET ceiling is refused");
	same_as_admin($node, 'low', "[$tag] after ALTER ROLE IN DATABASE");

	# --- a role with CREATEROLE, for a role it made ---
	run_as($node, 'maker', "CREATE ROLE made LOGIN;");
	($out, $err) = run_as($node, 'maker', "ALTER ROLE made SET kingres_sketch.state_memory_ceiling = '2GB';");
	like($err, qr/permission denied to set parameter/, "[$tag] CREATEROLE cannot set the ceiling of another role");
	same_as_admin($node, 'made', "[$tag] after CREATEROLE ALTER ROLE ceiling");
	($out, $err) = run_as($node, 'maker', "ALTER ROLE maker SET kingres_sketch.state_memory_ceiling = '2GB';");
	like($err, qr/permission denied to set parameter/, "[$tag] CREATEROLE cannot set its own ceiling");
	same_as_admin($node, 'maker', "[$tag] after CREATEROLE ALTER ROLE <self> ceiling");

	# --- ALTER DATABASE by a database owner who is not a superuser ---
	($out, $err) = run_as($node, 'dbo', "ALTER DATABASE dbo_db SET kingres_sketch.state_memory_ceiling = '2GB';");
	like($err, qr/permission denied to set parameter/, "[$tag] a database owner cannot set the ceiling of its database");
	same_as_admin($node, 'dbo', "[$tag] after ALTER DATABASE ceiling (owner)", db => 'dbo_db');
	same_as_admin($node, 'low', "[$tag] after ALTER DATABASE ceiling (other role in that database)", db => 'dbo_db');
	($out, $err) = run_as($node, 'dbo', "ALTER ROLE low IN DATABASE dbo_db SET kingres_sketch.state_memory_ceiling = '2GB';");
	like($err, qr/permission denied/, "[$tag] a database owner cannot store a ceiling for a role in its database");
	($out, $err) = run_as($node, 'dbo', "SELECT kingres_sketch.state_bytes();
ALTER DATABASE dbo_db SET kingres_sketch.state_memory_limit = '2GB';");
	is($err, '', "[$tag] a database owner may store the (own) limit");
	same_as_admin($node, 'low', "[$tag] after ALTER DATABASE limit = 2GB", db => 'dbo_db');
	run_as($node, 'dbo', "ALTER DATABASE dbo_db RESET kingres_sketch.state_memory_limit;");

	# --- the SET clause of a function the role owns ---
	for my $pre ('', "SELECT kingres_sketch.state_bytes();\n")
	{
		($out, $err) = run_as($node, 'low',
			$pre . "CREATE FUNCTION public.f_ceil() RETURNS bigint LANGUAGE sql "
			  . "SET kingres_sketch.state_memory_ceiling = '2GB' AS 'SELECT kingres_sketch.state_limit()';");
		like($err, qr/permission denied to set parameter/, "[$tag] a function SET clause cannot carry the ceiling" . ($pre ? ' (loaded)' : ''));
	}
	run_as($node, 'low', "SELECT kingres_sketch.state_bytes();
CREATE FUNCTION public.f_limit() RETURNS bigint LANGUAGE sql "
		  . "SET kingres_sketch.state_memory_limit = '2GB' AS 'SELECT kingres_sketch.state_limit()';");
	($out, $err) = run_as($node, 'low', "SELECT public.f_limit();");
	like($out, qr/^$ceiling_bytes$/m, "[$tag] a function SET clause for the own limit is capped by the ceiling");
	# a SECURITY DEFINER function of a superuser runs with the session's settings
	$node->safe_psql('postgres',
		"CREATE FUNCTION public.f_def() RETURNS bigint LANGUAGE sql SECURITY DEFINER AS 'SELECT kingres_sketch.state_limit()'");
	$node->safe_psql('postgres', "GRANT EXECUTE ON FUNCTION public.f_def() TO low");
	($out, $err) = run_as($node, 'low', "SET kingres_sketch.state_memory_limit = '2MB';\nSELECT public.f_def();");
	like($out, qr/^2097152$/m, "[$tag] a SECURITY DEFINER function of a superuser sees the role's session (own limit 2MB)");
	($out, $err) = run_as($node, 'low',
		"SET kingres_sketch.state_memory_ceiling = '2GB';\nSELECT public.f_def();");
	like($out, qr/^$ceiling_bytes$/m, "[$tag] and the role cannot raise it for that function");

	# --- options of the connection (PGOPTIONS), before the library is loaded in the session ---
	for my $opt ('-c kingres_sketch.state_memory_ceiling=2GB', '-c kingres_sketch.state_memory_ceiling=2147483648kB',
		'-c kingres_sketch.state_memory_ceiling=1GB', '-c kingres_sketch.state_memory_ceiling=64MB')
	{
		# a refused value in the options is the reset value of the placeholder when the library is not
		# loaded yet and replaces the administrator's value by the built-in default (1GB), also when
		# the options carry exactly that default; the library notes it and the limit is at most 1MB
		my ($limit, $refused, $ran, $e) = next_session($node, 'low', options => $opt);
		if ($preloaded)
		{
			# the connection itself is refused: a value of a PGC_SUSET setting in the options
			ok(!defined($limit), "[$tag] options '$opt': the connection is refused");
			like($e, qr/permission denied to set parameter/, "[$tag] options '$opt': with the reason");
		}
		else
		{
			is($limit, 1048576, "[$tag] options '$opt': the limit is at most the failsafe of 1MB");
			is($refused, 1, "[$tag] options '$opt': the big query is refused");
			like($e, qr/WARNING:  the value of kingres_sketch.state_memory_ceiling that was set before/, "[$tag] options '$opt': and the library says so");
		}
	}
	{
		my ($limit, $refused) = next_session($node, 'low', options => '-c kingres_sketch.state_memory_limit=2GB');
		is($limit, $ceiling_bytes, "[$tag] the own limit in the options is capped by the ceiling");
		is($refused, 1, "[$tag] and the big query is refused");
	}

	# --- RESET, RESET ALL, SET ROLE ---
	($out, $err) = run_as($node, 'low',
		"SELECT kingres_sketch.state_bytes();\nSET kingres_sketch.state_memory_limit = '1MB';\nRESET kingres_sketch.state_memory_limit;\n"
		  . "SELECT kingres_sketch.state_limit();\nSET kingres_sketch.state_memory_limit = '1MB';\nRESET ALL;\nSELECT kingres_sketch.state_limit();");
	my @l = $out =~ /^(\d+)$/mg;
	shift @l;	# the counter of the first call
	is("@l", "$ceiling_bytes $ceiling_bytes", "[$tag] RESET and RESET ALL go back to the administrator's value");
	# the superuser's session: its own ceiling stays when it becomes an ordinary role; a role cannot raise it
	($out, $err) = run_as($node, 'sup2',
		"SET kingres_sketch.state_memory_ceiling = '16MB';\nSET ROLE low;\nSELECT kingres_sketch.state_limit();\n"
		  . "SET kingres_sketch.state_memory_ceiling = '2GB';\nSELECT kingres_sketch.state_limit();");
	like($out, qr/^16777216$/m, "[$tag] a superuser's own ceiling stays after SET ROLE <ordinary role>");
	like($err, qr/permission denied to set parameter/, "[$tag] and the ordinary role cannot raise it");
	unlike($out, qr/^2147483648$/m, "[$tag] the limit is not raised");
	# a member of a superuser role is a superuser once it takes the role: this is the documented rule
	($out, $err) = run_as($node, 'viaset',
		"SET ROLE sup2;\nSET kingres_sketch.state_memory_ceiling = '2GB';\nSELECT kingres_sketch.state_limit();");
	like($out, qr/^2147483648$/m, "[$tag] a role that can SET ROLE to a superuser has the superuser's rights");
	same_as_admin($node, 'viaset', "[$tag] and it does not last into the next session");

	# --- GRANT SET ON PARAMETER: the holder may set the ceiling, nobody else ---
	$node->safe_psql('postgres', "GRANT SET ON PARAMETER kingres_sketch.state_memory_ceiling TO grantee");
	($out, $err) = run_as($node, 'grantee',
		"ALTER ROLE grantee SET kingres_sketch.state_memory_ceiling = '64MB';");
	is($err, '', "[$tag] the holder of GRANT SET ON PARAMETER may store a ceiling for itself");
	my ($limit, $refused, $ran) = next_session($node, 'grantee');
	is($limit, 64 * 1024 * 1024, "[$tag] and its next session has it");
	is($ran, 1, "[$tag] the big query runs for the holder");
	same_as_admin($node, 'low', "[$tag] the grant does not reach other roles");
	$node->safe_psql('postgres', "REVOKE SET ON PARAMETER kingres_sketch.state_memory_ceiling FROM grantee");
	($limit) = next_session($node, 'grantee');
	is($limit, 64 * 1024 * 1024, "[$tag] a revoked grant does not remove a stored value: the administrator must reset it");
	($out, $err) = run_as($node, 'grantee', "SELECT kingres_sketch.state_bytes();
SET kingres_sketch.state_memory_ceiling = '2GB';");
	like($err, qr/permission denied to set parameter/, "[$tag] after the revoke the role cannot set it any more");
	$node->safe_psql('postgres', "ALTER ROLE grantee RESET kingres_sketch.state_memory_ceiling");
	same_as_admin($node, 'grantee', "[$tag] after the administrator reset it");

	# --- parallel workers use the values of the leader, which cannot exceed the ceiling ---
	$node->safe_psql('postgres', <<'EOS');
CREATE TABLE big AS SELECT i, repeat('x', 200) || i::text AS item FROM generate_series(1, 40000) i;
ALTER TABLE big SET (parallel_workers = 2);
GRANT SELECT ON big TO PUBLIC;
ANALYZE big;
EOS
	my $par = "SET max_parallel_workers_per_gather = 2; SET parallel_setup_cost = 0; SET parallel_tuple_cost = 0; "
	  . "SET min_parallel_table_scan_size = 0; SET parallel_leader_participation = off;";
	my $pq = "SELECT kingres_sketch.topn_total(kingres_sketch.topn_add_agg(item, 40000)) FROM big";
	like($node->safe_psql('postgres', "$par EXPLAIN (COSTS OFF) $pq"),
		qr/Gather.*Partial Aggregate/s, "[$tag] the forced plan is parallel");
	($out, $err) = run_as($node, 'low',
		"$par SET kingres_sketch.state_memory_limit = '2GB';\n$pq;\nSELECT kingres_sketch.state_bytes();");
	like($err, qr/aggregate states of kingres_sketch hold \d+ bytes in this backend, over the limit of $ceiling_bytes bytes/,
		"[$tag] a parallel query of a role is refused under the ceiling, whatever its own limit");
	like($out, qr/^0$/m, "[$tag] the counter is zero afterwards");
	($out, $err) = run_as($node, 'sup2',
		"$par SET kingres_sketch.state_memory_ceiling = '1GB';\n$pq;\nSELECT kingres_sketch.state_bytes();");
	like($out, qr/^40000$/m, "[$tag] with a larger ceiling of a superuser the same parallel query runs");

	# --- the administrator's ways keep working ---
	# ALTER ROLE and ALTER DATABASE by a superuser
	$node->safe_psql('postgres', "ALTER ROLE low SET kingres_sketch.state_memory_ceiling = '64MB'");
	($limit, $refused, $ran) = next_session($node, 'low');
	is($limit, 64 * 1024 * 1024, "[$tag] ALTER ROLE by a superuser: the next session has the larger ceiling");
	is($ran, 1, "[$tag] and the big query runs");
	$node->safe_psql('postgres', "ALTER ROLE low SET kingres_sketch.state_memory_ceiling = '1MB'");
	($limit, $refused) = next_session($node, 'low');
	is($limit, 1024 * 1024, "[$tag] ALTER ROLE by a superuser: a smaller ceiling");
	is($refused, 1, "[$tag] and the big query is refused");
	$node->safe_psql('postgres', "ALTER ROLE low RESET kingres_sketch.state_memory_ceiling");
	$node->safe_psql('postgres', "ALTER DATABASE dbo_db SET kingres_sketch.state_memory_ceiling = '64MB'");
	($limit, $refused, $ran) = next_session($node, 'low', db => 'dbo_db');
	is($limit, 64 * 1024 * 1024, "[$tag] ALTER DATABASE by a superuser: the next session has it");
	is($ran, 1, "[$tag] and the big query runs");
	$node->safe_psql('postgres', "ALTER DATABASE dbo_db RESET kingres_sketch.state_memory_ceiling");
	# a role's own lower limit still applies below the administrator's higher ceiling
	$node->safe_psql('postgres', "ALTER ROLE low SET kingres_sketch.state_memory_ceiling = '64MB'");
	($out, $err) = run_as($node, 'low',
		"SET kingres_sketch.state_memory_limit = '1MB';\nSELECT kingres_sketch.state_limit();");
	like($out, qr/^1048576$/m, "[$tag] a role lowers a larger ceiling for itself");
	$node->safe_psql('postgres', "ALTER ROLE low RESET kingres_sketch.state_memory_ceiling");
	# ALTER SYSTEM (needs a restart here to be seen by every new session without waiting for a reload)
	$node->safe_psql('postgres', "ALTER SYSTEM SET kingres_sketch.state_memory_ceiling = '64MB'");
	$node->restart;
	($limit, $refused, $ran) = next_session($node, 'low');
	is($limit, 64 * 1024 * 1024, "[$tag] ALTER SYSTEM: the next session has the new ceiling");
	is($ran, 1, "[$tag] and the big query runs");
	$node->safe_psql('postgres', "ALTER SYSTEM RESET kingres_sketch.state_memory_ceiling");
	$node->restart;
	same_as_admin($node, 'low', "[$tag] back to the configuration file");

	$node->stop;
}

matrix(1);
matrix(0);

done_testing();
