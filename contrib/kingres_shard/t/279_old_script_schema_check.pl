# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/279_old_script_schema_check.pl: the check of the schema runs in every script of kingres_shard that can be run, not only in the default version (XCAT-2, L2).
#
#   The install scripts of the older versions (CREATE EXTENSION ... VERSION) and the upgrade scripts (ALTER EXTENSION ... UPDATE) used not to look at the schema,
#   so a schema that a role that is not a superuser holds, or can create objects in, was taken over by the extension all the same.
#   A. every install script: refused over a schema owned by a role that is not a superuser, and over one that gives CREATE to PUBLIC; it installs over a clean database
#   B. every upgrade script: refused after the schema was given to such a role (or CREATE granted to PUBLIC), applied once it is the superuser's again

use strict;
use warnings FATAL => 'all';
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;

my $ext = 'kingres_shard';
my @installs = ('0.1', '0.2', '0.3', '0.4', '0.5', '0.6', '0.7', '0.8', '0.9');
my @upgrades = (['0.1','0.2'], ['0.2','0.3'], ['0.3','0.4'], ['0.4','0.5'], ['0.5','0.6'], ['0.6','0.7'], ['0.7','0.8'], ['0.8','0.9']);

my $node = PostgreSQL::Test::Cluster->new('os');
$node->init;
$node->append_conf('postgresql.conf', "shared_preload_libraries = 'kingres_shard'\nmax_prepared_transactions = 10\n");
$node->start;
$node->safe_psql('postgres', q{CREATE ROLE tenant LOGIN});

my $n = 0;
sub fresh_db { my $db = 'os' . ++$n; $node->safe_psql('postgres', "CREATE DATABASE $db"); return $db; }
sub run_in { my ($db, $sql) = @_; my ($o, $e); my $rc = $node->psql($db, $sql, stdout => \$o, stderr => \$e, timeout => 120); return ($rc, $o, $e); }

for my $v (@installs)
{
	my $db = fresh_db();
	my ($rc, $o, $e) = run_in($db, "CREATE SCHEMA kingres AUTHORIZATION tenant; CREATE EXTENSION $ext VERSION '$v'");
	isnt($rc, 0, "A: $ext $v is not installed over a schema of a role that is not a superuser");
	like($e, qr/is not a superuser/, 'A: ... the script says why') or diag($e);
	$db = fresh_db();
	($rc, $o, $e) = run_in($db, "CREATE SCHEMA kingres; GRANT CREATE ON SCHEMA kingres TO PUBLIC; CREATE EXTENSION $ext VERSION '$v'");
	isnt($rc, 0, "A: $ext $v is not installed over a schema that lets PUBLIC create objects");
	like($e, qr/grants CREATE/, 'A: ... the script says why') or diag($e);
	$db = fresh_db();
	($rc, $o, $e) = run_in($db, "CREATE EXTENSION $ext VERSION '$v'");
	is($rc, 0, "A: $ext $v installs in a clean database") or diag($e);
}

for my $u (@upgrades)
{
	my ($from, $to) = @$u;
	my $db = fresh_db();
	my ($rc, $o, $e) = run_in($db, "CREATE EXTENSION $ext VERSION '$from'");
	is($rc, 0, "B: $ext $from installs") or diag($e);
	($rc, $o, $e) = run_in($db, "ALTER SCHEMA kingres OWNER TO tenant; ALTER EXTENSION $ext UPDATE TO '$to'");
	isnt($rc, 0, "B: $from to $to is refused when the schema belongs to a role that is not a superuser");
	like($e, qr/is not a superuser/, 'B: ... the script says why') or diag($e);
	($rc, $o, $e) = run_in($db, "ALTER SCHEMA kingres OWNER TO CURRENT_USER; GRANT CREATE ON SCHEMA kingres TO PUBLIC; ALTER EXTENSION $ext UPDATE TO '$to'");
	isnt($rc, 0, "B: $from to $to is refused when PUBLIC may create objects in the schema");
	like($e, qr/grants CREATE/, 'B: ... the script says why') or diag($e);
	($rc, $o, $e) = run_in($db, "REVOKE CREATE ON SCHEMA kingres FROM PUBLIC; ALTER EXTENSION $ext UPDATE TO '$to'");
	is($rc, 0, "B: $from to $to is applied once the schema is closed") or diag($e);
}

$node->stop;
done_testing();
