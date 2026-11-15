# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/060_shared_schema.pl
#
# kingres_shard shares the schema "kingres" with kingres_columnar, kingres_ledger and
# kingres_timepart, whose control files say "schema = kingres" (CREATE EXTENSION then
# makes the schema a member of the extension that made it).  The install script of
# kingres_shard ran CREATE SCHEMA kingres unconditionally, so it failed after any of
# them.  Now the schema exists afterwards whoever made it, the objects of kingres_shard
# are members of the extension, and the schema is not a member of kingres_shard, so
# DROP EXTENSION kingres_shard neither drops it nor fails because others use it.
#
# Every order of creating kingres_shard with the other extensions that are installed
# (the others are skipped, not failed, when their files are not: an out-of-tree run
# has only some), every order of dropping them, and a dump and restore.

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(make_node run_sql);

my $node = make_node('ss');
sub q1 { my ($sql, $db) = @_; return $node->safe_psql($db // 'postgres', $sql); }

my @others = grep {
	q1(qq{SELECT count(*) FROM pg_available_extensions WHERE name = '$_'}) eq '1'
} qw(kingres_ledger kingres_timepart kingres_columnar);
my @all = ('kingres_shard', @others);
diag('extensions available: ' . join(' ', @all));

sub permutations
{
	my (@l) = @_;
	return ([]) unless @l;
	my @r;
	for my $i (0 .. $#l)
	{
		my @rest = @l;
		my ($x) = splice(@rest, $i, 1);
		push @r, [ $x, @$_ ] for permutations(@rest);
	}
	return @r;
}

my $n = 0;
sub fresh_db
{
	my $db = 'ss' . ++$n;
	q1("CREATE DATABASE $db");
	return $db;
}

# What must hold when the extensions in @$have are installed.
sub check_state
{
	my ($db, $have, $what) = @_;
	is(q1(q{SELECT count(*) FROM pg_namespace WHERE nspname = 'kingres'}, $db), '1', "$what: the schema kingres exists");
	return unless grep { $_ eq 'kingres_shard' } @$have;
	is(q1(q{SELECT count(*) FROM pg_depend d WHERE d.classid = 'pg_namespace'::regclass AND d.objid = 'kingres'::regnamespace
	         AND d.refclassid = 'pg_extension'::regclass AND d.refobjid = (SELECT oid FROM pg_extension WHERE extname = 'kingres_shard')
	         AND d.deptype = 'e'}, $db), '0', "$what: the schema is not a member of kingres_shard");
	is(q1(q{SELECT count(*) > 50 FROM pg_depend d WHERE d.classid = 'pg_proc'::regclass AND d.refclassid = 'pg_extension'::regclass
	         AND d.refobjid = (SELECT oid FROM pg_extension WHERE extname = 'kingres_shard') AND d.deptype = 'e'}, $db), 't',
		"$what: the functions of kingres_shard are members of it");
	is(q1(q{SELECT count(*) FROM kingres.node}, $db), '0', "$what: and kingres_shard works");
}

my @orders = permutations(@all);
for my $order (@orders)
{
	my $name = join(' ', map { s/^kingres_//r } @$order);
	my $db = fresh_db();
	my ($rc, $out, $err);
	for my $e (@$order)
	{
		($rc, $out, $err) = run_sql($node, "CREATE EXTENSION $e", extra_params => [ '-d', $db ]);
		is($rc, 0, "create in the order $name: $e") or diag $err;
	}
	check_state($db, $order, "created in the order $name");

	# Dump and restore into another database.
	my $dump = PostgreSQL::Test::Utils::tempdir() . "/d.sql";
	command_ok([ 'pg_dump', '-h', $node->host, '-p', $node->port, '-d', $db, '-f', $dump ], "dump ($name)");
	my $db2 = fresh_db();
	($rc, $out, $err) = run_sql($node, '', extra_params => [ '-d', $db2, '-v', 'ON_ERROR_STOP=1', '-f', $dump ]);
	is($rc, 0, "restore of the dump ($name)") or diag $err;
	check_state($db2, $order, "restored ($name)");
	my $have = q1(q{SELECT string_agg(extname, ',' ORDER BY extname) FROM pg_extension WHERE extname LIKE 'kingres%'}, $db2);
	is($have, join(',', sort @$order), "restored ($name): every extension is there");

	# Drop in the order of creation and in the reverse order, in the original and in the restored database.
	for my $case ([ $db, [ @$order ], 'in the order of creation' ], [ $db2, [ reverse @$order ], 'in the reverse order' ])
	{
		my ($d, $list, $how) = @$case;
		my @left = @$order;
		for my $e (@$list)
		{
			($rc, $out, $err) = run_sql($node, "DROP EXTENSION $e", extra_params => [ '-d', $d ]);
			is($rc, 0, "created $name, dropped $how: $e") or diag $err;
			@left = grep { $_ ne $e } @left;
			next unless @left;
			is(q1(q{SELECT count(*) FROM pg_namespace WHERE nspname = 'kingres'}, $d), '1',
				"the schema is still there for " . join(' ', map { s/^kingres_//r } @left));
		}
	}
}

done_testing();
