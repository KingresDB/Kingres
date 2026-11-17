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

# the primary of a group without a registered standby answers false whatever the name
is($n[0]->safe_psql('postgres', "SELECT kingres.standby_name_matches('*')"), 'f',
	'a node whose group has no standby answers false');

done_testing();
