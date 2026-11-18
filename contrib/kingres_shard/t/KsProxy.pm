# SPDX-License-Identifier: AGPL-3.0-only
# Portions Copyright (c) 2026, RooyeKhat Media Co
#
# A TCP forwarder for the tests of the bounds of the control plane (S11-07, S11-10): it stands in
# front of a node and makes the node answer slowly, or stop answering after a given number of
# messages of a connection, so that "a node that answers every statement just before node_timeout"
# and "a node that stops answering at the Nth statement" can be made without stopping a process.
#
#   my $p = KsProxy->new(backend_port => $node->port, host => '127.0.0.1', delay => 2);
#   $p->port;			# the port to register the node under
#   $p->stop;
#
# Options: delay (seconds that every chunk from the node to the client waits), swallow_after (after
# that many chunks from the client on one connection nothing is forwarded in either direction: the
# node accepted the session and goes silent), swallow_all (accept and say nothing from the start),
# start_delay (seconds before the first chunk of the node reaches the client), skip_chunks (how many
# chunks of the node, per connection, are not delayed by delay: the start of a session), delay_turn
# (seconds that the answer to each message of the client takes, counted from the moment the message is
# forwarded to the node, once per answer however many chunks it arrives in: a node whose every answer takes
# exactly delay_turn seconds, whatever the node's own time is as long as it is less; skip_turns of the first
# answers of a connection are not delayed), turn_log (a file to which every delayed answer appends a line:
# the number of statements that a call sent to the node), chunk_log (a file to which every
# chunk from the client appends "<connection> <number of the chunk>", so that a test knows how many
# chunks a call sends: the sweep over swallow_after runs up to that number), text_log (a file to which every chunk
# from the client appends "<connection> <number of the chunk> <its first 100 bytes, printable>": the sweeps find the
# chunk that carries a RELEASE, a COMMIT, a PREPARE), and three more ways to fail at the n-th chunk of a connection, which
# the sweeps run like swallow_after (S13-05): close_after (the chunk after the n-th closes the connection), lose_answer
# (the node gets the n-th chunk and what it answers is dropped, the connection stays open), error_at (the node never gets
# the n-th chunk; the client is answered with an ErrorResponse, XX000, and a ReadyForQuery that says "failed transaction").
# A statement that spans several chunks is cut by error_at into a protocol error on the node, which ends as a closed
# connection.

package KsProxy;

use strict;
use warnings FATAL => 'all';
use IO::Socket::INET;
use IO::Select;
use POSIX ();
use Time::HiRes qw(sleep clock_gettime CLOCK_MONOTONIC);

# the forwarders made by this process (see END below)
my @instances;

sub new
{
	my ($class, %opt) = @_;
	my $listen = IO::Socket::INET->new(Listen => 64, LocalAddr => '127.0.0.1', LocalPort => 0, ReuseAddr => 1)
	  or die "proxy listen: $!";
	my $self = bless { %opt, listen => $listen, port => $listen->sockport, kids => [] }, $class;
	$self->{ctl} //= ($ENV{TESTDATADIR} // ".") . "/ksproxy_$$" . "_" . $self->{port};
	$self->set(map { $_ => $self->{$_} } grep { defined $self->{$_} } qw(delay swallow_after swallow_all start_delay skip_chunks delay_turn skip_turns chunk_log turn_log text_log close_after lose_answer error_at));
	my $pid = fork();
	die "fork: $!" unless defined $pid;
	if ($pid == 0)
	{
		# A forwarder that outlives a test that died must not hold the pipes of the test harness open
		# (prove waits for them): it has no use for the handles of its parent.
		open(STDIN, '<', '/dev/null');
		open(STDOUT, '>', '/dev/null');
		open(STDERR, '>', '/dev/null');
		POSIX::setpgid(0, 0);
		$SIG{CHLD} = 'IGNORE';
		$SIG{TERM} = sub { POSIX::_exit(0) };
		while (my $c = $listen->accept)
		{
			my $k = fork();
			if (defined $k && $k == 0)
			{
				$listen->close;
				$self->serve($c);
				POSIX::_exit(0);
			}
			$c->close;
		}
		POSIX::_exit(0);
	}
	$self->{pid} = $pid;
	$listen->close;
	$self->{owner} = $$;
	push @instances, $self;
	return $self;
}

# Every forwarder that a test made and did not stop goes with the test, also when the test dies.
END
{
	my $status = $?;
	$_->stop for grep { $_->{owner} == $$ } @instances;
	$? = $status;
}

sub port { return $_[0]->{port} }

# Change the behaviour of every connection, also the ones that are open: set(delay => 2) etc.  The
# options are read from a file by the processes that serve the connections.
sub set
{
	my ($self, %opt) = @_;
	my $f = $self->{ctl};
	open(my $h, '>', "$f.tmp") or die "proxy ctl: $!";
	print $h "$_=$opt{$_}\n" for sort keys %opt;
	close $h;
	rename "$f.tmp", $f;
}

sub options
{
	my ($self) = @_;
	my %o = map { $_ => $self->{$_} } grep { defined $self->{$_} } qw(delay swallow_after swallow_all start_delay skip_chunks delay_turn skip_turns chunk_log turn_log text_log close_after lose_answer error_at);
	if (open(my $h, '<', $self->{ctl}))
	{
		%o = ();
		while (<$h>) { chomp; $o{$1} = $2 if /^(\w+)=(.*)$/; }
		close $h;
	}
	return \%o;
}

sub serve
{
	my ($self, $c) = @_;
	my $s;
	my $swallowing = $self->options->{swallow_all} ? 1 : 0;
	if (!$swallowing)
	{
		$s = IO::Socket::INET->new(PeerAddr => $self->{host}, PeerPort => $self->{backend_port}) or return;
	}
	my $sel = IO::Select->new($c, $s ? ($s) : ());
	my $chunks = 0;
	my $schunks = 0;
	my $turn_pending = 0;
	my $t_req = 0;
	my $turns = 0;
	while (1)
	{
		foreach my $fh ($sel->can_read(1))
		{
			my $n = sysread($fh, my $buf, 65536);
			return unless $n;	# either side closed: both are closed, so the node's backend ends too
			my $o = $self->options;
			if ($fh == $c)
			{
				$chunks++;
				if ($o->{chunk_log} && open(my $lh, '>>', $o->{chunk_log}))
				{
					print $lh "$$ $chunks\n";
					close $lh;
				}
				if ($o->{text_log} && open(my $xh, '>>', $o->{text_log}))
				{
					my $t = substr($buf, 0, 100);
					$t =~ s/[^ -~]/./g;
					print $xh "$$ $chunks $t\n";
					close $xh;
				}
				# close_after n: the chunk after the n-th closes the connection (both sides), so the client sees it closed
				return if defined $o->{close_after} && $o->{close_after} ne '' && $chunks > $o->{close_after};
				# error_at n: the node never sees the n-th chunk; the client is answered with an ErrorResponse and a
				# ReadyForQuery that says "failed transaction": the answer of a node that refused the statement.  For
				# the startup packet it is a FATAL error and the connection is closed.
				if (defined $o->{error_at} && $o->{error_at} ne '' && $chunks == $o->{error_at})
				{
					my $sev = $chunks == 1 ? 'FATAL' : 'ERROR';
					my $f = "S$sev\0V$sev\0CXX000\0Mfault injected by the test forwarder\0\0";
					syswrite($c, pack('A1 N', 'E', 4 + length($f)) . $f . ($chunks == 1 ? '' : pack('A1 N A1', 'Z', 5, 'E')));
					return if $chunks == 1;
					next;
				}
				$swallowing = 1
				  if defined $o->{swallow_after} && $o->{swallow_after} ne '' && $chunks > $o->{swallow_after};
				syswrite($s, $buf) unless $swallowing;
				# the clock of a statement starts at its first chunk, as the client's does: a large statement is
				# relayed in several chunks, and the time that takes counts against the answer
				$t_req = clock_gettime(CLOCK_MONOTONIC) unless $turn_pending;
				$turn_pending = 1;
			}
			else
			{
				next if $swallowing;
				# lose_answer n: what the node sends while the n-th chunk of the client is the last one is dropped
				next if defined $o->{lose_answer} && $o->{lose_answer} ne '' && $chunks == $o->{lose_answer};
				$schunks++;
				sleep $o->{start_delay} if $schunks == 1 && $o->{start_delay};
				sleep $o->{delay} if $o->{delay} && $schunks > ($o->{skip_chunks} // 0);
				my $turn_log;
				if ($turn_pending)
				{
					$turn_pending = 0;
					$turns++;
					if ($o->{delay_turn} && $turns > ($o->{skip_turns} // 0))
					{
						my $natural = clock_gettime(CLOCK_MONOTONIC) - $t_req;
						my $wait = $o->{delay_turn} - $natural;
						sleep $wait if $wait > 0;
						$turn_log = [ $o->{turn_log}, $natural ] if $o->{turn_log};
					}
				}
				syswrite($c, $buf);
				if ($turn_log && open(my $th, '>>', $turn_log->[0]))
				{
					# the number of the process, the time the node itself took, the time the client waited
					printf $th "%d %.3f %.3f\n", $$, $turn_log->[1], clock_gettime(CLOCK_MONOTONIC) - $t_req;
					close $th;
				}
			}
		}
	}
}

sub stop
{
	my ($self) = @_;
	return unless $self->{pid};
	kill 'TERM', -$self->{pid};
	waitpid($self->{pid}, 0);
	$self->{pid} = 0;
	unlink $self->{ctl};
}

1;
