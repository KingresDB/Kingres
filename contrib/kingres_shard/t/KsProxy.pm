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
# chunks of the node, per connection, are not delayed by delay: the start of a session).

package KsProxy;

use strict;
use warnings FATAL => 'all';
use IO::Socket::INET;
use IO::Select;
use POSIX ();
use Time::HiRes qw(sleep);

sub new
{
	my ($class, %opt) = @_;
	my $listen = IO::Socket::INET->new(Listen => 64, LocalAddr => '127.0.0.1', LocalPort => 0, ReuseAddr => 1)
	  or die "proxy listen: $!";
	my $self = bless { %opt, listen => $listen, port => $listen->sockport, kids => [] }, $class;
	$self->{ctl} //= ($ENV{TESTDATADIR} // ".") . "/ksproxy_$$" . "_" . $self->{port};
	$self->set(map { $_ => $self->{$_} } grep { defined $self->{$_} } qw(delay swallow_after swallow_all start_delay skip_chunks));
	my $pid = fork();
	die "fork: $!" unless defined $pid;
	if ($pid == 0)
	{
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
	return $self;
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
	my %o = map { $_ => $self->{$_} } grep { defined $self->{$_} } qw(delay swallow_after swallow_all start_delay skip_chunks);
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
				$swallowing = 1
				  if defined $o->{swallow_after} && $o->{swallow_after} ne '' && $chunks > $o->{swallow_after};
				syswrite($s, $buf) unless $swallowing;
			}
			else
			{
				next if $swallowing;
				$schunks++;
				sleep $o->{start_delay} if $schunks == 1 && $o->{start_delay};
				sleep $o->{delay} if $o->{delay} && $schunks > ($o->{skip_chunks} // 0);
				syswrite($c, $buf);
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
