# SPDX-License-Identifier: AGPL-3.0-only
#
# contrib/kingres_ledger/t/031_server_file_roles.pl
#
# L19-05 of the sixth review of the guard: the two lists of the roles that can
# drop the database (kingres.ledger_status: database_droppers and
# database_potential_droppers, and the warnings of ledger_enable) were computed
# from the privileges of the owner of the database and from ADMIN OPTION, and a
# member of pg_execute_server_program, which is in neither, can drop it:
#
#   COPY (SELECT 1) TO PROGRAM '(sleep 1; psql ... -c "DROP DATABASE x") &'
#
# runs the command as the operating-system user of the server, which is a
# superuser of the cluster.  A member of pg_write_server_files writes any file
# that user can write, the files of the cluster included.  Both count as
# superusers for this purpose: they are listed in database_droppers (directly,
# through a chain with INHERIT, and by SET ROLE), and the roles with ADMIN OPTION
# on those two roles in database_potential_droppers.
#
# The other predefined roles of PostgreSQL 18 and 19 (the same fifteen besides
# pg_database_owner, which has its own rule and its own test, t/021) do not give
# that power and are not listed: a member of each is created, and none is.
# pg_read_server_files reads files and writes none (the test shows that the
# member cannot write a file or run a program).
#
# The proofs are the real acts: the drop of a database by a member of
# pg_execute_server_program, and a file written into the data directory by a
# member of pg_write_server_files.  Nothing is decided by a duration: the drop is
# waited for as a condition (the database is gone from pg_database).
#
# Portions Copyright (c) 2026, RooyeKhat Media Co

use strict;
use warnings FATAL => 'all';

use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;

$ENV{PGOPTIONS} = '-c search_path=public';

my $node = PostgreSQL::Test::Cluster->new('server_files');
$node->init;
$node->append_conf('postgresql.conf',
	"shared_preload_libraries = 'kingres_ledger'\n"
	  . "kingres_ledger.block_interval = 0\n");
$node->start;

my $psql = $node->installed_command('psql');
my $datadir = $node->data_dir;

$node->safe_psql('postgres', q{
	CREATE ROLE sowner SUPERUSER NOLOGIN;
	CREATE ROLE prog LOGIN IN ROLE pg_execute_server_program;
	CREATE ROLE wr LOGIN IN ROLE pg_write_server_files;
	CREATE ROLE rd LOGIN IN ROLE pg_read_server_files;
	CREATE ROLE grp NOLOGIN IN ROLE pg_execute_server_program;
	CREATE ROLE chain_m LOGIN IN ROLE grp;
	CREATE ROLE so_m LOGIN;
	GRANT pg_write_server_files TO so_m WITH INHERIT FALSE, SET TRUE;
	CREATE ROLE none_m LOGIN;
	GRANT pg_execute_server_program TO none_m WITH INHERIT FALSE, SET FALSE;
	CREATE ROLE adm LOGIN;
	GRANT pg_execute_server_program TO adm WITH ADMIN TRUE, INHERIT FALSE, SET FALSE;
});

sub make_db
{
	my ($db) = @_;
	$node->safe_psql('postgres', "CREATE DATABASE $db OWNER sowner TEMPLATE template0");
	$node->safe_psql($db, q{
		CREATE EXTENSION kingres_ledger;
		CREATE TABLE first_t (id int PRIMARY KEY);
	});
	return;
}

sub lists
{
	my ($db) = @_;
	return $node->safe_psql($db, q{SELECT database_droppers::text || ' ' || database_potential_droppers::text
		FROM kingres.ledger_status});
}

make_db('d1');

#--------------------------------------------------------------------------
# The lists
#--------------------------------------------------------------------------
is(lists('d1'), '{chain_m,grp,prog,so_m,wr} {adm}',
	'the members of pg_execute_server_program and pg_write_server_files (directly, through a chain, by SET only) are in database_droppers; the holder of ADMIN OPTION on one is in database_potential_droppers; a member of pg_read_server_files and a membership with neither option are not');

my ($ret, $out, $err) = $node->psql('d1', q{CREATE TABLE t1 (id int); SELECT kingres.ledger_enable('t1') IS NULL});
like($err, qr/WARNING:  database "d1" can be dropped, and the ledger with it, by roles that are not superusers: chain_m, grp, prog, so_m, wr\n/,
	'ledger_enable warns, and names them');
like($err, qr/WARNING:  database "d1" can also be dropped, and the ledger with it, by roles that are not superusers after they grant themselves a role they administer.*: adm\n/s,
	'and names the holder of ADMIN OPTION in the sentence of its own');

# Every other predefined role: a member of each, listed nowhere.
my @others = qw(pg_read_all_data pg_write_all_data pg_monitor pg_read_all_settings pg_read_all_stats
	pg_stat_scan_tables pg_signal_backend pg_checkpoint pg_maintain pg_use_reserved_connections
	pg_create_subscription pg_signal_autovacuum_worker);
my $present = $node->safe_psql('postgres', q{SELECT string_agg(rolname, ' ' ORDER BY rolname) FROM pg_roles WHERE oid < 16384 AND NOT rolsuper});
my @predef = split(' ', $present);
my %known = map { $_ => 1 } (@others, qw(pg_execute_server_program pg_write_server_files pg_read_server_files pg_database_owner));
my @unknown = grep { !$known{$_} } @predef;
is(scalar(@unknown), 0, 'every predefined role of this major is accounted for in this test (a new one must be looked at: ' . join(', ', @unknown) . ')');
$node->safe_psql('postgres', "CREATE ROLE mem_$_ LOGIN IN ROLE $_") for @others;
is(lists('d1'), '{chain_m,grp,prog,so_m,wr} {adm}',
	'a member of each of the other predefined roles is listed nowhere: ' . join(', ', @others));

#--------------------------------------------------------------------------
# The real acts
#--------------------------------------------------------------------------
# A member of pg_execute_server_program drops a database.  The drop is run by a
# detached command (a drop from the session of the database that is dropped waits
# for itself), from another database.
make_db('d_victim');
my $conn = '-h ' . $node->host . ' -p ' . $node->port . ' -d postgres';
$ret = $node->psql('d1',
	qq{COPY (SELECT 1) TO PROGRAM '($psql -X $conn -c "DROP DATABASE d_victim") >/dev/null 2>&1 &'},
	extra_params => [ '-U', 'prog' ], on_error_stop => 0);
ok($node->poll_query_until('postgres', q{SELECT count(*) = 0 FROM pg_database WHERE datname = 'd_victim'}),
	'a member of pg_execute_server_program dropped a database that holds a ledger');

# A member of pg_write_server_files writes a file into the data directory.
($ret, $out, $err) = $node->psql('d1', qq{COPY (SELECT 'kl_probe') TO '$datadir/kl_probe'},
	extra_params => [ '-U', 'wr' ], on_error_stop => 0);
is($err, '', 'a member of pg_write_server_files writes a file into the data directory');
ok(-f "$datadir/kl_probe", 'and the file is there');
unlink("$datadir/kl_probe");

# Neither is a power of a member of pg_read_server_files, or of a membership with neither option.
foreach my $r (qw(rd none_m))
{
	($ret, $out, $err) = $node->psql('d1', qq{COPY (SELECT 'kl_probe') TO '$datadir/kl_probe_$r'},
		extra_params => [ '-U', $r ], on_error_stop => 0);
	like($err, qr/permission denied to COPY to a file/, "$r cannot write a file");
	($ret, $out, $err) = $node->psql('d1', q{COPY (SELECT 1) TO PROGRAM 'true'},
		extra_params => [ '-U', $r ], on_error_stop => 0);
	like($err, qr/permission denied to COPY to or from an external program/, "$r cannot run a program");
	ok(!-e "$datadir/kl_probe_$r", "$r wrote nothing");
}

#--------------------------------------------------------------------------
# The control: take the memberships away and the lists are empty again.
#--------------------------------------------------------------------------
$node->safe_psql('postgres', q{
	REVOKE pg_execute_server_program FROM prog, grp, none_m, adm;
	REVOKE pg_write_server_files FROM wr, so_m;
});
is(lists('d1'), '{} {}', 'with the memberships revoked, both lists are empty');

done_testing();
