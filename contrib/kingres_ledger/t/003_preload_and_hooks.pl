
# contrib/kingres_ledger/t/003_preload_and_hooks.pl
#
# ledger_enable refuses to run when the library is not in
# shared_preload_libraries, and the protection hook chains to the hook of
# another extension (pg_stat_statements) in both load orders.
#
# Portions Copyright (c) 2026, RooyeKhat Media Co
# SPDX-License-Identifier: AGPL-3.0-only

use strict;
use warnings FATAL => 'all';

use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;

# 1. Without shared_preload_libraries the extension can be created, but
# nothing can be put under ledger control.
my $plain = PostgreSQL::Test::Cluster->new('plain');
$plain->init;
$plain->start;
$plain->safe_psql('postgres',
	'CREATE EXTENSION kingres_ledger; CREATE TABLE public.t (a int);');
is($plain->safe_psql('postgres', 'SELECT kingres.ledger_hook_active()'),
	'f', 'the hook is not active without shared_preload_libraries');
my ($ret, $stdout, $stderr) = $plain->psql('postgres',
	"SELECT kingres.ledger_enable('public.t')",
	extra_params => [ '-v', 'VERBOSITY=sqlstate' ]);
isnt($ret, 0, 'ledger_enable fails without shared_preload_libraries');
like($stderr, qr/KL003/, 'with SQLSTATE KL003');
$plain->stop;

# 2. Hook chaining, in both orders.
foreach my $libs ('pg_stat_statements,kingres_ledger',
	'kingres_ledger,pg_stat_statements')
{
	my $node = PostgreSQL::Test::Cluster->new('chain_' . ($libs =~ s/\W/_/gr));
	$node->init;
	$node->append_conf('postgresql.conf',
		"shared_preload_libraries = '$libs'\npg_stat_statements.track_utility = on\n");
	$node->start;
	$node->safe_psql('postgres',
		'CREATE EXTENSION pg_stat_statements; CREATE EXTENSION kingres_ledger;'
		  . 'CREATE TABLE public.t (a int); SELECT kingres.ledger_enable(\'public.t\');');

	($ret, $stdout, $stderr) = $node->psql('postgres',
		'ALTER TABLE t DISABLE TRIGGER ALL',
		extra_params => [ '-v', 'VERBOSITY=sqlstate' ]);
	isnt($ret, 0, "[$libs] the ledger hook rejects the statement");
	like($stderr, qr/KL002/, "[$libs] with SQLSTATE KL002");

	$node->safe_psql('postgres', 'ALTER TABLE t ADD COLUMN b int');
	$node->safe_psql('postgres', 'INSERT INTO t VALUES (1, NULL)');
	is( $node->safe_psql('postgres',
			q(SELECT count(*) > 0 FROM pg_stat_statements WHERE query LIKE 'ALTER TABLE t ADD COLUMN%')),
		't', "[$libs] pg_stat_statements still sees the allowed utility statement");
	is($node->safe_psql('postgres', 'SELECT count(*) FROM kingres.ledger_verify()'),
		'0', "[$libs] the ledger verifies");
	$node->stop;
}

done_testing();
