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
#   mode      how the node fails at the position: swallow_after (silent, the default), close_after,
#             lose_answer or error_at (t/KsProxy.pm), S13-05
#   wrap      optional sub ($sql) -> $sql: the call is run inside 'savepoint', 'plpgsql' or 'block' (S13-01), see wrap_sql
#   sample    true: the positions of the default run are a sample (the first, the last, and the chunks around the
#             ones that carry a RELEASE, a COMMIT, a PREPARE or a second phase, and one in each third of the call);
#             every position runs with KINGRES_LONG_TESTS=1
#   left      a regex that the WARNING of a success must match, or the word proxy for the address of the node behind the
#             forwarder: the warning names the node that was left (S13-05); left_if_left: only when something was left
#             (promote_node warns about the dead old primary in every run, so a warning proves nothing there)

package KsSweep;

use strict;
use warnings FATAL => 'all';
use Test::More;
use Time::HiRes qw(time sleep);
use Exporter 'import';

our @EXPORT_OK = qw(sweep_call sweeps chunk_max wrap_sql sample_positions text_chunks settle);

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
	my ($log, $tlog);
	if (!$positions)
	{
		$log = ($c->{tmp} // $ENV{TESTDATADIR} // ".") . "/chunks_" . ($name =~ s/\W+/_/gr);
		unlink $log;
		$tlog = "$log.text";
		unlink $tlog;
		$c->{proxy}->set(chunk_log => $log, text_log => $tlog);
	}
	my ($rc, $out, $err) = $c->{call}->($c->{wrap} ? $c->{wrap}->($c->{run}) : $c->{run});
	$c->{proxy}->set(chunk_log => '', text_log => '') if $log;
	is($rc, 0, "$name: the clean run succeeds ($err)");
	if ($log)
	{
		my $max = chunk_max($log);
		diag("$name: the call sends up to $max chunks to the node");
		$positions = [ 0 .. $max ];
		$positions = sample_positions($max, $tlog, ($c->{mode} // 'swallow_after') ne 'swallow_after' ? 1 : 0)
		  if $c->{sample} && !$ENV{KINGRES_LONG_TESTS};
		diag("$name: positions " . join(',', @$positions));
	}
	my $new = $c->{state}->();
	$c->{cleanup}->();
	$c->{prepare}->();
	my $old2 = $c->{state}->();
	ok(!$c->{kinds} ? !states_equal($old, $new) : 1, "$name: the call changes the state (old: " . show($old) . "; new: " . show($new) . ")");
	ok($c->{kinds} || states_equal($old, $old2), "$name: the cleanup restores the old state (" . show($old2) . ")");
	$c->{cleanup}->();

	my $how = (($c->{mode} // 'swallow_after') eq 'swallow_after') ? 'silent' : ($c->{mode} =~ s/_/ /gr);
	foreach my $pos (@$positions)
	{
		$c->{prepare}->();
		my $before = $c->{state}->();
		is(kind_of($c, $before, $old, $new), 'old', "$name, $how after chunk $pos: the precondition is the old state (" . show($before) . ")");
		$c->{proxy}->set(($c->{mode} // 'swallow_after') => $pos);
		my $t0 = time;
		($rc, $out, $err) = $c->{call}->($c->{wrap} ? $c->{wrap}->($c->{run}) : $c->{run});
		my $t = time - $t0;
		$c->{proxy}->set(swallow_after => '');
		settle($c);
		my $state = $c->{state}->();
		my $kind = kind_of($c, $state, $old, $new);
		my $prep = prepared_total(@{ $c->{nodes} });
		my $warn = ($err =~ /WARNING/) ? 1 : 0;
		my ($sqlstate) = ($err =~ /\b((?:KS|08|57|55|25|40|XX|58|53)[0-9A-Z]{3}):/);
		$sqlstate //= '-';
		my $label = sprintf('%s, %s after chunk %d: %s%s in %.1f s, SQLSTATE %s, state %s, %d prepared',
			$name, $how, $pos, ($rc == 0 ? 'success' : 'error'), ($warn ? ' with WARNING' : ''), $t, $sqlstate, $kind, $prep);
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
			{
				my $left = $c->{left};
				# 'proxy': the address of the node behind the forwarder, as the extension names it (host:port)
				my $pport = $c->{proxy}->port;
				if (defined $left && !ref $left && $left eq 'proxy')
				{
					# by address, or by the id alone (the warnings of recovery say "on node N")
					my $id = $c->{n1}->safe_psql('postgres', "SELECT coalesce((SELECT node_id FROM kingres.node WHERE port = $pport), 0)");
					$left = qr/(?::\Q$pport\E\b|\bnode \Q$id\E\b)/;
				}
				like($err, $left, "$label: and names the node that was left")
				  if defined $left && (!$c->{left_if_left} || $kind ne 'new' || $prep > 0);
			}
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
			ok($t <= $c->{bound}, sprintf('%s, %s after chunk %d: returned in %.1f s (bound %s s)', $name, $how, $pos, $t, $c->{bound}));
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


# The call as the caller of a transaction block writes it (S13-01): the statements that the extension sends to a
# node for the end of a session are not those of the autocommit call.  savepoint: inside SAVEPOINT / RELEASE;
# plpgsql: inside a block with an EXCEPTION clause that raises the error again; block: inside a transaction block
# that the caller commits.
sub wrap_sql
{
	my ($variant, $sql) = @_;
	return $sql if !$variant || $variant eq 'plain';
	return "BEGIN;\nSAVEPOINT ks_sp;\n$sql;\nRELEASE ks_sp;\nCOMMIT;" if $variant eq 'savepoint';
	return "DO \$ks\$ BEGIN BEGIN EXECUTE \$q\$$sql\$q\$; EXCEPTION WHEN OTHERS THEN RAISE; END; END \$ks\$" if $variant eq 'plpgsql';
	return "BEGIN;\n$sql;\nCOMMIT;" if $variant eq 'block';
	die "unknown variant $variant";
}

# The numbers of the chunks, over all sessions, whose text matches $re, from the text log of the forwarder.
sub text_chunks
{
	my ($file, $re) = @_;
	my %n;
	if (open(my $h, '<', $file))
	{
		while (<$h>)
		{
			my ($k, $text) = /^\d+ (\d+) (.*)$/ ? ($1, $2) : ();
			$n{$k} = 1 if defined $k && $text =~ $re;
		}
		close $h;
	}
	return sort { $a <=> $b } keys %n;
}

# The positions of the default run of a sweep that does not run every position (S13-05): the first, the last, one
# in each third of the call, and the chunks around the ones that carry the statements that end the work of a session
# (a RELEASE, a COMMIT, a PREPARE, a second phase): those are where the phases of a call change.  $min is the lowest
# position that the way of failing has (1 for a lost answer and an error answer).
sub sample_positions
{
	my ($max, $tlog, $min) = @_;
	my %p = map { $_ => 1 } ($min, $max, map { int($max * $_ / 3 + 0.5) } (1, 2));
	if ($tlog)
	{
		# the chunk of the PREPARE batch starts with internal_dtx_applied (the text log keeps the first 100 bytes)
		foreach my $re (qr/RELEASE SAVEPOINT/, qr/COMMIT TRANSACTION/, qr/internal_dtx_applied/, qr/COMMIT PREPARED/)
		{
			my @c = text_chunks($tlog, $re);
			# the first and the last chunk that carries it; chunk c is swallowed after c - 1
			my @pick = @c ? ($c[0], $c[-1]) : ();
			$p{$_} = 1 for map { ($_ - 1, $_) } @pick;
		}
	}
	return [ sort { $a <=> $b } grep { $_ >= $min && $_ <= $max } keys %p ];
}

# Wait until no session of the nodes is working for the call that ended (a node that is still rolling back reads as
# locked): a condition, not a fixed sleep.  Gives up after 30 s and lets the judge read what there is.
sub settle
{
	my ($c) = @_;
	my $busy = q{SELECT count(*) FROM pg_stat_activity WHERE backend_type = 'client backend' AND pid <> pg_backend_pid() AND state IS DISTINCT FROM 'idle' AND datname = 'postgres'};
	my $end = time + 30;
	while (time < $end)
	{
		my $n = 0;
		$n += $_->safe_psql('postgres', $busy) for @{ $c->{nodes} };
		return if $n == 0;
		sleep 0.2;
	}
}

# The scenarios that a sweep file runs, chosen by the environment variable KS_SWEEP_SCENARIO (t/108, t/109 and t/110
# set it and run t/100 and t/101): silent (the default: the forwarder goes silent, the call in autocommit, every
# position), modes (the connection is closed after chunk n, the node's answer to chunk n is lost, the node answers
# chunk n with an error: a sample of the positions) and variants (the call inside SAVEPOINT / RELEASE, inside a
# PL/pgSQL block with an EXCEPTION clause, inside a transaction block that the caller commits: a sample).
my @scenarios;
{
	my @which = split /,/, ($ENV{KS_SWEEP_SCENARIO} // 'silent');
	my %w = map { $_ => 1 } @which;
	push @scenarios, { mode => 'swallow_after', variant => 'plain', sample => 0, label => '' } if $w{silent};
	push @scenarios, map { +{ mode => $_, variant => 'plain', sample => 1, label => $_ } } qw(close_after lose_answer error_at) if $w{modes};
	push @scenarios, map { +{ mode => 'swallow_after', variant => $_, sample => 1, label => $_ } } qw(savepoint plpgsql block) if $w{variants};
	push @scenarios, { mode => 'swallow_after', variant => 'savepoint', sample => 1, label => 'savepoint' } if $w{savepoint};
	die "unknown KS_SWEEP_SCENARIO @which" unless @scenarios == ($w{silent} ? 1 : 0) + ($w{modes} ? 3 : 0) + ($w{variants} ? 3 : 0) + ($w{savepoint} ? 1 : 0);
}

# The sweep of one call in every scenario of the file; the judge also checks that a warning names the node that was left.
sub sweeps
{
	my ($c) = @_;
	my $name = $c->{name};
	my @rows;
	$c->{left} //= 'proxy';
	foreach my $sc (@scenarios)
	{
		$c->{name} = $sc->{label} ne '' ? "$name [$sc->{label}]" : $name;
		$c->{mode} = $sc->{mode};
		$c->{sample} = $sc->{sample};
		$c->{wrap} = $sc->{variant} eq 'plain' ? undef : sub { return wrap_sql($sc->{variant}, $_[0]); };
		# the RELEASE of the session that carried the call's work belongs to the call's transaction: its failure is an
		# error, not the warning that remove_node gives for a node that does not answer while it clears it (S13-01)
		my $keep = $c->{no_error_like};
		delete $c->{no_error_like} if $sc->{variant} eq 'savepoint' || $sc->{variant} eq 'plpgsql';
		push @rows, @{ sweep_call($c, undef) };
		$c->{no_error_like} = $keep if defined $keep;
	}
	$c->{name} = $name;
	return \@rows;
}

1;
