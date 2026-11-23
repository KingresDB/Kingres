# contrib/kingres_timepart/t/010_schema_spoof.pl
#
# The schema of the extension is the extension's own (the security note of 2026-11-23): CREATE EXTENSION fails when a role that is not a superuser made the
# schema in advance, or when the schema grants CREATE to such a role (or to PUBLIC).  Otherwise that role would own what the service role, a superuser, reads and
# runs.  The superuser's own schema is used as before.
#
# Portions Copyright (c) 2026, RooyeKhat Media Co
# SPDX-License-Identifier: AGPL-3.0-only

use strict;
use warnings FATAL => 'all';

use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;

my $node = PostgreSQL::Test::Cluster->new('spoof');
$node->init;
$node->start;
$node->safe_psql('postgres', <<'EOS');
CREATE ROLE tenant LOGIN;
GRANT CREATE ON DATABASE postgres TO tenant;
EOS

# a schema that the tenant made in advance
$node->safe_psql('postgres', "SET ROLE tenant; CREATE SCHEMA kingres; CREATE VIEW kingres.fake AS SELECT 1 AS a");
my ($ret, $out, $err) = $node->psql('postgres', 'CREATE EXTENSION kingres_timepart');
isnt($ret, 0, 'CREATE EXTENSION fails when the tenant made the schema');
like($err, qr/schema kingres already exists and is owned by tenant, which is not a superuser/, '... and says why');
is($node->safe_psql('postgres', "SELECT count(*) FROM pg_extension WHERE extname = 'kingres_timepart'"), '0', '... and nothing is installed');
$node->safe_psql('postgres', 'DROP SCHEMA kingres CASCADE');

# a schema of the superuser that grants CREATE to PUBLIC, and to the tenant
for my $grantee ('PUBLIC', 'tenant')
{
	$node->safe_psql('postgres', "CREATE SCHEMA kingres; GRANT CREATE ON SCHEMA kingres TO $grantee");
	($ret, $out, $err) = $node->psql('postgres', 'CREATE EXTENSION kingres_timepart');
	isnt($ret, 0, "CREATE EXTENSION fails when the schema grants CREATE to $grantee");
	like($err, qr/schema kingres grants CREATE to a role that is not a superuser/, '... and says why');
	$node->safe_psql('postgres', 'DROP SCHEMA kingres CASCADE');
}

# a schema of the superuser, with no grants, is used as before
$node->safe_psql('postgres', 'CREATE SCHEMA kingres');
$node->safe_psql('postgres', 'CREATE EXTENSION kingres_timepart');
is($node->safe_psql('postgres', "SELECT extnamespace::regnamespace::text FROM pg_extension WHERE extname = 'kingres_timepart'"), 'kingres', 'the superuser\'s schema is used');
$node->stop;
done_testing();
