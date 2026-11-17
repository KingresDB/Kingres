# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/088_standby_names.pl
#
# Review S10-03: kingres.standby_name_matches() compared names case-sensitively and did not know
# "*", so it answered false for names that PostgreSQL accepts for synchronous_standby_names.  The
# invariant: the function answers what PostgreSQL decides for an element of the list.  Each name
# is put into the list of the primary, and the answer of the function is compared with the
# sync_state that the primary then reports for the standby ("quorum" or "sync" when the name
# matches, "async" when it does not).

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(make_cluster make_standby register_standby wait_catchup run_sql);

my @n = make_cluster('sn', 2, replication => 1);
my $sb = make_standby($n[1], 'Std_Mixed');
register_standby($n[0], $sb, 1);
wait_catchup($n[1], $sb);
is($n[1]->safe_psql('postgres', "SELECT application_name FROM pg_stat_replication"), 'Std_Mixed',
	'the standby streams under a mixed-case name');

# element as written in the list => should it match this standby?
my @cases = (
	[ 'Std_Mixed', 1 ],
	[ 'std_mixed', 1 ],
	[ 'STD_MIXED', 1 ],
	[ '"Std_Mixed"', 1 ],
	[ '"std_mixed"', 1 ],
	[ '*', 1 ],
	[ '"*"', 1 ],
	[ 'nobody', 0 ],
	[ 'Std_Mixe', 0 ],
	[ 'Std_Mixed2', 0 ],
	[ '"Std Mixed"', 0 ],
);
foreach my $c (@cases)
{
	my ($elem, $expect) = @$c;
	$n[1]->append_conf('postgresql.conf', "synchronous_standby_names = 'ANY 1 ($elem)'\n");
	$n[1]->reload;
	my $state = $n[1]->poll_query_until('postgres',
		"SELECT sync_state = " . ($expect ? "'quorum'" : "'async'") . " FROM pg_stat_replication") ? ($expect ? 'quorum' : 'async')
	  : $n[1]->safe_psql('postgres', 'SELECT sync_state FROM pg_stat_replication');
	is($state, $expect ? 'quorum' : 'async', "PostgreSQL: the element $elem " . ($expect ? 'matches' : 'does not match') . " the standby");
	my $arg = $elem;
	$arg =~ s/'/''/g;
	is($n[1]->safe_psql('postgres', "SELECT kingres.standby_name_matches('$arg')"), $expect ? 't' : 'f',
		"standby_name_matches('$elem') agrees with PostgreSQL");
}

# --- long names (S11-06): the standby's name is cut by the server to 63 bytes, the elements of the
# list are not.  Two more standbys: one of 62 bytes and one of 200, which the server shows as 63.
my $long62 = 'y' x 62;
my $long200 = 'x' x 200;
my $s62 = make_standby($n[1], $long62);
my $s200 = make_standby($n[1], $long200);
ok($n[1]->poll_query_until('postgres', "SELECT count(*) = 3 FROM pg_stat_replication WHERE state = 'streaming'"),
	'all three standbys stream (wait_for_catchup cannot find the one whose name the server cut)');
is($n[1]->safe_psql('postgres', "SELECT string_agg(length(application_name)::text, ',' ORDER BY length(application_name)) FROM pg_stat_replication"),
	'9,62,63', 'the server shows the names with 9, 62 and 63 bytes (200 is cut to 63)');
my @long = (
	[ 'y' x 62, 1 ], [ 'y' x 63, 0 ], [ 'x' x 62, 0 ], [ 'x' x 63, 1 ], [ 'x' x 64, 0 ], [ 'x' x 65, 0 ], [ 'x' x 200, 0 ],
	[ '"' . ('x' x 63) . '"', 1 ], [ '"' . ('x' x 64) . '"', 0 ], [ '"' . ('x' x 200) . '"', 0 ],
);
foreach my $c (@long)
{
	my ($elem, $expect) = @$c;
	my $label = length($elem) > 20 ? substr($elem, 0, 3) . '...(' . length($elem) . ' bytes)' : $elem;
	$n[1]->append_conf('postgresql.conf', "synchronous_standby_names = 'ANY 1 ($elem)'\n");
	$n[1]->reload;
	my $got = $n[1]->poll_query_until('postgres',
		"SELECT count(*) " . ($expect ? ">= 1" : "= 0") . " FROM pg_stat_replication WHERE sync_state = 'quorum'")
	  ? $expect : 1 - $expect;
	is($got, $expect, "PostgreSQL: the element $label " . ($expect ? 'matches' : 'does not match') . " a standby");
	is($n[1]->safe_psql('postgres', "SELECT kingres.standby_name_matches('$elem')"), $expect ? 't' : 'f',
		"standby_name_matches agrees for $label");
}
# names that cannot be written bare make PostgreSQL reject the whole list, so nothing matches them
foreach my $elem ('My-Standby', 'any', 'FIRST', '1abc', 'a b')
{
	is($n[1]->safe_psql('postgres', "SELECT kingres.internal_sync_name_matches('$elem', '$elem')"), 'f',
		"a bare element $elem that the list scanner does not read as a name matches nothing");
}
is($n[1]->safe_psql('postgres', q{SELECT kingres.internal_sync_name_matches('"My-Standby"', 'my-standby')}), 't',
	'the same name in double quotes matches');

# the primary of a group without a registered standby answers false whatever the name
is($n[0]->safe_psql('postgres', "SELECT kingres.standby_name_matches('*')"), 'f',
	'a node whose group has no standby answers false');

done_testing();
