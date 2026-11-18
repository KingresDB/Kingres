# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# The sweep of S12-01: a control call is run against a node behind a forwarder (t/KsProxy.pm) that
# goes silent after the n-th chunk of a session, for EVERY n from the first chunk to the last chunk
# the call sends, and the answer is judged against the standard of a control call: it never
# returns success for work that was not done on a node without saying so.  An answer is accepted
# when it is
#   - an error, and the state of every node is the old one (or, after the documented repeat, the new
#     one everywhere), never a mixture;
#   - a success without a WARNING, and the state of every node is the new one at once, nothing is
#     left prepared;
#   - a success with a WARNING that names the node or says that recovery will finish the work, and
#     after the documented repeat (one recovery pass) the state is the new one, nothing is prepared
#     and check_cluster() is empty.
# Everything else is a failed test with the position, the answer and the state in its name.
#
# A call is described by a hash:
#   name      the name used in test names
#   prepare   sub: establishes the old state (called before every position; may die)
#   run       the SQL of the call
#   state     sub: the state of every node as a hash reference of strings (one entry per node)
#   cleanup   sub: leaves the cluster ready for the next position (called with the forwarder open)
#   repeat    optional sub: the documented repeat, returns (rc, out, err); default: one recovery pass
#             (the fields n1, proxy and nodes are read afresh, so a call whose prepare builds a new
#             cluster for every position can replace them)
#   kinds     optional sub ($state, $old) -> 'old' | 'new' | 'mixed' | 'partial'; the default compares
#             each entry with the entry of the old state and of the state after a clean run
#   no_error_like  optional regex: an error that matches it is a failure (remove_node of a node that stops
#             answering must end with a WARNING, never with KS007)
#   partial   true when a success with a WARNING may leave the state 'partial' (remove_node: the node
#             is removed from the catalog and its own catalog is not cleared, as the README says)
#   n1        the node that runs the call; nodes: every node whose prepared transactions are counted
#   proxy     the forwarder in front of the node that goes silent
#   bound     seconds that a call may take (the README's bound for a node that stops answering)

package KsSweep;

use strict;
use warnings FATAL => 'all';
use Test::More;
use Time::HiRes qw(time sleep);
use Exporter 'import';

our @EXPORT_OK = qw(sweep_call chunk_max);

sub prepared_total
{
	my (@nodes) = @_;
	my $t = 0;
	$t += $_->safe_psql('postgres', 'SELECT count(*) FROM pg_prepared_xacts') for @nodes;
	return $t;
}

sub states_equal
{
	my ($a, $b) = @_;
	return 0 unless join(',', sort keys %$a) eq join(',', sort keys %$b);
	foreach my $k (keys %$a) { return 0 if $a->{$k} ne $b->{$k}; }
	return 1;
}

sub show
{
	my ($s) = @_;
	return join(' ', map { "$_=$s->{$_}" } sort keys %$s);
}

# The highest chunk number that the logged sessions reached.
sub chunk_max
{
	my ($file) = @_;
	my $max = 0;
	if (open(my $h, '<', $file))
	{
		while (<$h>) { $max = $1 if /^\d+ (\d+)$/ && $1 > $max; }
		close $h;
	}
	return $max;
}

sub kind_of
{
	my ($c, $state, $old, $new) = @_;
	return $c->{kinds}->($state, $old) if $c->{kinds};
	return 'old' if states_equal($state, $old);
	return 'new' if states_equal($state, $new);
	return 'mixed';
}

# $positions: a reference to the list of the positions, or undef: 0 .. the highest chunk that the clean run
# sent to the node (the field tmp of the call names a directory for the log of the forwarder).
# Returns a reference to the list of the outcomes, one hash per position.
sub sweep_call
{
	my ($c, $positions) = @_;
	my $name = $c->{name};
	my @rows;

	# the old and the new state: a clean run, with the forwarder open
	$c->{prepare}->();
	$c->{proxy}->set(swallow_after => '', delay => 0, delay_turn => 0);
	my $old = $c->{state}->();
	my $log;
	if (!$positions)
	{
		$log = ($c->{tmp} // $ENV{TESTDATADIR} // ".") . "/chunks_" . ($name =~ s/\W+/_/gr);
		unlink $log;
		$c->{proxy}->set(chunk_log => $log);
	}
	my ($rc, $out, $err) = $c->{call}->($c->{run});
	$c->{proxy}->set(chunk_log => '') if $log;
	is($rc, 0, "$name: the clean run succeeds ($err)");
	if ($log)
	{
		my $max = chunk_max($log);
		diag("$name: the call sends up to $max chunks to the node");
		$positions = [ 0 .. $max ];
	}
	my $new = $c->{state}->();
	$c->{cleanup}->();
	$c->{prepare}->();
	my $old2 = $c->{state}->();
	ok(!$c->{kinds} ? !states_equal($old, $new) : 1, "$name: the call changes the state (old: " . show($old) . "; new: " . show($new) . ")");
	ok($c->{kinds} || states_equal($old, $old2), "$name: the cleanup restores the old state (" . show($old2) . ")");
	$c->{cleanup}->();

	foreach my $pos (@$positions)
	{
		$c->{prepare}->();
		my $before = $c->{state}->();
		is(kind_of($c, $before, $old, $new), 'old', "$name, silent after chunk $pos: the precondition is the old state (" . show($before) . ")");
		$c->{proxy}->set(swallow_after => $pos);
		my $t0 = time;
		($rc, $out, $err) = $c->{call}->($c->{run});
		my $t = time - $t0;
		$c->{proxy}->set(swallow_after => '');
		sleep 0.5;
		my $state = $c->{state}->();
		my $kind = kind_of($c, $state, $old, $new);
		my $prep = prepared_total(@{ $c->{nodes} });
		my $warn = ($err =~ /WARNING/) ? 1 : 0;
		my ($sqlstate) = ($err =~ /\b((?:KS|08|57|55|25|40|XX|58|53)[0-9A-Z]{3}):/);
		$sqlstate //= '-';
		my $label = sprintf('%s, silent after chunk %d: %s%s in %.1f s, SQLSTATE %s, state %s, %d prepared',
			$name, $pos, ($rc == 0 ? 'success' : 'error'), ($warn ? ' with WARNING' : ''), $t, $sqlstate, $kind, $prep);
		my $outcome;
		if ($rc != 0)
		{
			$outcome = 'error';
			ok($kind ne 'mixed', "$label: an error leaves no mixture (" . show($state) . ") [$err]");
			unlike($err, $c->{no_error_like}, "$label: and is not an answer that the README gives for another situation")
			  if $c->{no_error_like};
		}
		elsif (!$warn)
		{
			$outcome = 'success';
			is($kind, 'new', "$label: a success without a warning is done on every node (" . show($state) . ")");
			is($prep, 0, "$label: and leaves nothing prepared");
		}
		else
		{
			$outcome = 'success+warning';
			ok($err =~ /node|recovery/i, "$label: the warning names the node or recovery [$err]");
		}
		# the documented repeat: one recovery pass, then the state is settled
		if ($rc != 0 || $warn || $prep > 0)
		{
			my ($rrc, $rout, $rerr) = $c->{repeat} ? $c->{repeat}->() : $c->{call}->('SELECT kingres.recover_prepared_transactions()');
			is($rrc, 0, "$label: the recovery pass runs ($rerr)");
			$state = $c->{state}->();
			$kind = kind_of($c, $state, $old, $new);
			$prep = prepared_total(@{ $c->{nodes} });
			is($prep, 0, "$label: after the recovery pass nothing is prepared");
			if ($rc == 0)
			{
				ok($kind eq 'new' || ($kind eq 'partial' && $c->{partial}),
					"$label: after the recovery pass the work is done (" . show($state) . ")");
			}
			else
			{
				ok($kind ne 'mixed', "$label: after the recovery pass no mixture (" . show($state) . ")");
			}
		}
		if ($c->{bound})
		{
			ok($t <= $c->{bound}, sprintf('%s, silent after chunk %d: returned in %.1f s (bound %s s)', $name, $pos, $t, $c->{bound}));
		}
		push @rows, { pos => $pos, rc => $rc, t => $t, warn => $warn, sqlstate => $sqlstate, kind => $kind, outcome => $outcome };
		$c->{cleanup}->();
		$c->{proxy}->set(swallow_after => '');
	}
	my %count;
	$count{ $_->{outcome} }++ for @rows;
	my $max = 0;
	foreach my $r (@rows) { $max = $r->{t} if $r->{t} > $max; }
	diag("$name sweep: " . join(', ', map { "$_ $count{$_}" } sort keys %count) . sprintf('; slowest %.1f s', $max));
	return \@rows;
}

1;
