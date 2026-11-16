# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# t/078_promote_blocked.pl
#
# Review S9-04: "repeat promote_node until it answers already" must have an exit.  A
# prepared transaction of somebody else that holds a lock on kingres.map_version on
# another primary survives its session, and used to make every repeat answer
# "completed" with "final": false for ever.
#
#  - an ordinary role cannot read (and so cannot lock) kingres.map_version at all
#  - a prepared transaction that holds only ACCESS SHARE on it blocks nobody and is
#    not counted: the loop ends with "already"
#  - one that holds a conflicting lock is named in "blocked_by" (node, gid, and
#    whether the gid is the cluster's or foreign), with "final" false; the caller
#    stops repeating, the operator ends that transaction, and the next repeat
#    answers "already"

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(make_cluster make_standby register_standby wait_catchup
  promote_standby run_sql);

my $seq = 0;

sub setup
{
	my $prefix = 'pb' . (++$seq);
	my ($n1, $n2, $n3) = make_cluster($prefix, 3, replication => 1);
	my $s2 = make_standby($n2, "${prefix}_s2");
	my $id2 = register_standby($n1, $s2, 1);
	wait_catchup($n2, $s2);
	$n2->stop('immediate');
	promote_standby($s2);
	return ($prefix, $n1, $n3, $s2, $id2);
}

# The calls of the loop, at most $max, stopping at "already"; returns the answers.
sub loop_promote
{
	my ($n1, $id, $max) = @_;
	my @answers;
	for (1 .. $max)
	{
		my $a = $n1->safe_psql('postgres', "SELECT kingres.promote_node($id)");
		push @answers, $a;
		last if $a =~ /"status": "already"/;
	}
	return @answers;
}

# --- an ordinary role cannot lock the version row ----------------------------------------
{
	my ($prefix, $n1, $n3, $s2, $id2) = setup();
	$n3->safe_psql('postgres', 'CREATE ROLE bob LOGIN');
	my ($rc, $out, $err) = run_sql($n3, "SET ROLE bob; BEGIN; LOCK TABLE kingres.map_version IN ROW EXCLUSIVE MODE; PREPARE TRANSACTION 'bobs'");
	isnt($rc, 0, "[$prefix] an ordinary role cannot take a conflicting lock on the version row");
	like($err, qr/permission denied/, "[$prefix] it is refused by privilege");
	($rc, $out, $err) = run_sql($n3, "SET ROLE bob; BEGIN; SELECT version FROM kingres.map_version; PREPARE TRANSACTION 'bobs'");
	is($rc, 0, "[$prefix] it can read it, in a prepared transaction of its own that holds ACCESS SHARE");
	my @a = loop_promote($n1, $id2, 6);
	like($a[-1], qr/"status": "already"/, "[$prefix] and that does not keep the loop from ending");
	$n3->safe_psql('postgres', "ROLLBACK PREPARED 'bobs'");
}

# --- ACCESS SHARE blocks nobody and is not counted -----------------------------------------
{
	my ($prefix, $n1, $n3, $s2, $id2) = setup();
	$n3->safe_psql('postgres', "BEGIN; LOCK TABLE kingres.map_version IN ACCESS SHARE MODE; PREPARE TRANSACTION 'readers'");
	my @a = loop_promote($n1, $id2, 6);
	like($a[0], qr/"status": "promoted"/, "[$prefix] the first call promotes");
	like($a[-1], qr/"status": "already"/, "[$prefix] the loop ends with already: a reader's lock does not count");
	cmp_ok(scalar(@a), '<=', 3, "[$prefix] within three calls") or diag(join("\n", @a));
	$n3->safe_psql('postgres', "ROLLBACK PREPARED 'readers'");
}

# --- a conflicting lock is named ------------------------------------------------------------
{
	my ($prefix, $n1, $n3, $s2, $id2) = setup();
	$n3->safe_psql('postgres', "BEGIN; LOCK TABLE kingres.map_version IN ROW EXCLUSIVE MODE; PREPARE TRANSACTION 'in_the_way'");
	my @a = loop_promote($n1, $id2, 4);
	is(scalar(@a), 4, "[$prefix] the loop does not end while the transaction is there");
	like($a[-1], qr/"final": false/, "[$prefix] and no answer says final");
	like($a[-1], qr/"blocked_by": \[\{"node": 3, "prepared": \[\{"gid": "in_the_way", "class": "foreign"\}\]\}\]/,
		"[$prefix] the answer names the node and the transaction, and says that it is not the cluster's");
	$n3->safe_psql('postgres', "ROLLBACK PREPARED 'in_the_way'");
	@a = loop_promote($n1, $id2, 4);
	like($a[-1], qr/"status": "already".*"blocked_by": \[\]/s, "[$prefix] once it is gone the next repeats end with already");
}

done_testing();
