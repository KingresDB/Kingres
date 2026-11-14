# SPDX-License-Identifier: PostgreSQL
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# The comparison harness: the main correctness guard of phase 2a.  It builds a
# three-node cluster and, next to it, a single ordinary node; the test loads the
# same data into both, and the harness runs a list of queries on both and checks
# that the results are identical.  Later phases add queries to the list in the
# test file (t/013_dist_scan.pl), not to this module.
#
# Usage:
#   my $c = KingresShardCompare->new('prefix');
#   $c->on_cluster($sql);       # a statement on node 1 of the cluster (its DDL
#                               # and routed writes)
#   $c->on_every_node($sql);    # on each of the three nodes (local tables, functions)
#   $c->on_single($sql);        # on the single node
#   $c->on_both($sql);          # on the cluster's first node and the single node
#   $c->compare($name, $sql, ordered => 1);  # the query's output on each node
#                                            # of the cluster equals the single node's
#
# Output is compared line by line; unless ordered is true the lines are sorted
# first (the query has no ORDER BY the result could be compared by).

package KingresShardCompare;

use strict;
use warnings FATAL => 'all';
use FindBin;
use lib $FindBin::RealBin;
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KingresShardTest qw(make_cluster run_sql);

# Session settings under which compare() repeats every query (review finding
# S2-03): a restriction shipped to another node must mean what it means in the
# session of the user, so the whole list is run on the cluster and on the single
# node under each of these profiles too.  Pass profiles => 0 to compare() to
# skip them.
our @PROFILES = (
	q{SET DateStyle = 'SQL, DMY'; SET extra_float_digits = 0},
	q{SET DateStyle = 'German, DMY'; SET IntervalStyle = 'sql_standard'; SET bytea_output = 'escape'},
	q{SET DateStyle = 'Postgres, MDY'; SET IntervalStyle = 'postgres_verbose'; SET extra_float_digits = -3; SET TimeZone = 'America/New_York'},
	q{SET TimeZone = 'Asia/Tokyo'; SET IntervalStyle = 'iso_8601'; SET lc_monetary = 'C'; SET lc_numeric = 'C'; SET lc_time = 'C'},
);

sub profile_script
{
	my ($sql) = @_;
	return join('', map { "DISCARD ALL;\n\\echo \@\@profile $_\n$PROFILES[$_];\n$sql\n;\n" } (0 .. $#PROFILES));
}

# The output of a profile script, split by profile (a reference to a hash).
sub split_profiles
{
	my ($out) = @_;
	my @parts = split /^\@\@profile (\d+)(?:\n|\z)/m, $out, -1;
	shift @parts;				# text before the first marker
	my %by;
	while (@parts) { my ($n, $text) = splice(@parts, 0, 2); $by{$n} = $text; }
	return \%by;
}

sub new
{
	my ($class, $prefix, %opt) = @_;
	my @nodes = make_cluster($prefix . 'c', 3, %opt);
	my $single = PostgreSQL::Test::Cluster->new($prefix . 's');
	$single->init;
	$single->start;
	return bless { nodes => \@nodes, single => $single, ncompared => 0 }, $class;
}

sub nodes { return @{ $_[0]{nodes} } }
sub single { return $_[0]{single} }

sub on_cluster
{
	my ($self, $sql) = @_;
	return $self->{nodes}[0]->safe_psql('postgres', $sql);
}

sub on_every_node
{
	my ($self, $sql) = @_;
	$_->safe_psql('postgres', $sql) for @{ $self->{nodes} };
}

sub on_single
{
	my ($self, $sql) = @_;
	return $self->{single}->safe_psql('postgres', $sql);
}

sub on_both
{
	my ($self, $sql) = @_;
	$self->on_cluster($sql);
	$self->on_single($sql);
}

# A script of statements, one per line, run on the cluster and on the single node.
sub script_on_both
{
	my ($self, $script) = @_;
	$self->on_cluster($script);
	$self->on_single($script);
}

sub normalize
{
	my ($out, $ordered) = @_;
	my @lines = split /\n/, $out, -1;
	@lines = sort @lines unless $ordered;
	return join("\n", @lines);
}

# Run $sql on the single node and on every node of the cluster and compare.
sub compare
{
	my ($self, $name, $sql, %opt) = @_;
	my $ordered = $opt{ordered} ? 1 : 0;
	my @which = defined $opt{nodes} ? @{ $opt{nodes} } : (0 .. 2);
	my ($rs, $os, $es) = run_sql($self->{single}, $sql);

	if ($opt{expect_error})
	{
		for my $i (@which)
		{
			my ($rd, $od, $ed) = run_sql($self->{nodes}[$i], $sql);
			like($ed, $opt{expect_error}, "$name: node " . ($i + 1) . " reports the error");
		}
		return;
	}

	is($rs, 0, "$name: the single node runs the query") or diag($es);
	for my $i (@which)
	{
		my ($rd, $od, $ed) = run_sql($self->{nodes}[$i], $sql);
		is($rd, 0, "$name: node " . ($i + 1) . " runs the query") or diag($ed);
		is(normalize($od, $ordered), normalize($os, $ordered),
			"$name: the result on node " . ($i + 1) . " equals the single node's");
		$self->{ncompared}++;
	}

	if (!defined $opt{profiles} || $opt{profiles})
	{
		my $script = profile_script($sql);
		my ($rsp, $osp, $esp) = run_sql($self->{single}, $script);
		is($rsp, 0, "$name: the single node runs the query under the session profiles") or diag($esp);
		my $want = split_profiles($osp);
		for my $i (@which)
		{
			my ($rdp, $odp, $edp) = run_sql($self->{nodes}[$i], $script);
			is($rdp, 0, "$name: node " . ($i + 1) . " runs the query under the session profiles") or diag($edp);
			my $got = split_profiles($odp);
			for my $p (0 .. $#PROFILES)
			{
				is(normalize($got->{$p} // '<missing>', $ordered), normalize($want->{$p} // '', $ordered),
					"$name: node " . ($i + 1) . " under session profile $p equals the single node's");
			}
		}
	}
	return $os;
}


# Run a data-modifying statement on the single node and on one node of the
# cluster (the node $opt{node}, default 0) and compare the number of rows it
# reports.  The caller then compares the tables with compare().
sub compare_dml
{
	my ($self, $name, $dml, %opt) = @_;
	my $node = $self->{nodes}[ $opt{node} // 0 ];
	my $pre = $opt{settings} ? "$opt{settings}\n" : '';
	my $do = "${pre}DO \$\$ DECLARE ks_rows bigint; BEGIN $dml; GET DIAGNOSTICS ks_rows = ROW_COUNT; RAISE NOTICE 'ROWS=%', ks_rows; END \$\$";
	# A statement that only the cluster refuses is not run on the single node (it would change its data).
	my ($rs, $os, $es) = ($opt{expect_error} && !$opt{single_fails}) ? (0, "", "") : run_sql($self->{single}, $do);
	my ($rd, $od, $ed) = run_sql($node, $do);
	if ($opt{expect_error})
	{
		isnt($rd, 0, "$name: the cluster refuses the statement");
		isnt($rs, 0, "$name: the single node refuses it too") if $opt{single_fails};
		like($ed, $opt{expect_error}, "$name: the error");
		return;
	}
	is($rs, 0, "$name: the single node runs the statement") or diag($es);
	is($rd, 0, "$name: the cluster runs the statement") or diag($ed);
	my ($cs) = $es =~ /ROWS=(\d+)/;
	my ($cd) = $ed =~ /ROWS=(\d+)/;
	is($cd, $cs, "$name: the statement affected the same number of rows ($cs)");
	return $cs;
}

1;
