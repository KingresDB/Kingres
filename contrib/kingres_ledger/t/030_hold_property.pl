# SPDX-License-Identifier: AGPL-3.0-only
#
# contrib/kingres_ledger/t/030_hold_property.pl
#
# The property of the limit, attacked with random sequences instead of the
# statements of a review (the property test of t/024 had no error step, which is
# why it did not see L19-01):
#
#   P1  A role that is not a superuser never holds a lock of the extension across
#       statements.  Sessions of such a role send random sequences of statements:
#       the three writers it may call, a call in a transaction block, in a
#       savepoint, in a multi-command string (\;), in an EXCEPTION block, after
#       the first statement of a pipeline, an error (division by zero) in and out
#       of a block, a DO block that catches an error after the call, one that ends
#       with an error after the call, a rollback.  After every step the session is
#       alive, holds no lock of the extension, and is in the state that the model
#       says (idle, in a transaction block, or in an aborted one).
#
#   P2  Within one statement, a role that holds a lock for longer than the limit
#       is ended, and one that holds it for less is not, whatever errors the
#       statement catches or ends with.  Random DO blocks (the calls at the top
#       level, nested blocks that complete or are rolled back by a caught error,
#       caught errors inside them, sleeps, and an error that ends the statement)
#       against a model with the virtual time of the sleeps: killed when the
#       model says the lock was held for the limit and more; finished (with the
#       error the block ends with) when every hold was shorter than the limit by
#       the margin that the real cost of the steps needs.  The margins are what
#       makes the verdicts independent of the speed of the machine: a case that
#       the model cannot decide is not run.
#
# The steps are drawn from a seed, so a failure is reproduced by it;
# KL_PROP_SEEDS and KL_PROP_DO_SEEDS say how many seeds to run.
#
# Portions Copyright (c) 2026, RooyeKhat Media Co

use strict;
use warnings FATAL => 'all';

use IPC::Run ();
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;

$ENV{PGOPTIONS} = '-c search_path=public';

my $node = PostgreSQL::Test::Cluster->new('hold_property');
$node->init;
$node->append_conf('postgresql.conf',
	"shared_preload_libraries = 'kingres_ledger'\n"
	  . "kingres_ledger.block_interval = 0\n");
$node->start;

my $psql = $node->installed_command('psql');
my $LIMIT = 3;					# seconds, for the statements of P2

$node->safe_psql('postgres', q{CREATE ROLE app LOGIN; CREATE DATABASE d});
$node->safe_psql('d', q{
	CREATE EXTENSION kingres_ledger;
	GRANT USAGE ON SCHEMA kingres TO PUBLIC;
	GRANT CREATE ON SCHEMA public TO app;
	GRANT EXECUTE ON FUNCTION kingres.ledger_build_block(), kingres.ledger_generate_digest(),
		kingres.ledger_verify_recorded(bigint, bigint), kingres.ledger_enable(regclass, text) TO app;
});
for my $i (1 .. 160)
{
	$node->safe_psql('d', "CREATE TABLE p$i AS SELECT g AS id FROM generate_series(1, 3) g; ALTER TABLE p$i OWNER TO app");
}

sub sess_open
{
	my ($user) = @_;
	my %s = (in => '', out => '', err => '', n => 0);
	$s{h} = IPC::Run::start(
		[ $psql, '-XAtq', '-v', 'ON_ERROR_STOP=0', '-d', $node->connstr('d') . " user=$user", '-f', '-' ],
		'<' => \$s{in}, '>' => \$s{out}, '2>' => \$s{err},
		IPC::Run::timeout(300));
	my $r = sess_send(\%s, 'SELECT pg_backend_pid()');
	$s{pid} = $r->{out} =~ /^(\d+)$/m ? $1 : die "no pid: $r->{out} $r->{err}";
	return \%s;
}

# raw: the text is a line of meta-commands (or ends with one); no ";" is added.
sub sess_send
{
	my ($s, $sql, $raw) = @_;
	my $marker = 'KLSEP' . (++$s->{n});
	$s->{out} = '';
	$s->{err} = '';
	$s->{in} .= ($raw ? "$sql\n" : "$sql;\n") . "\\echo $marker\n\\warn $marker\n";
	while ($s->{out} !~ /$marker\n/ || $s->{err} !~ /$marker\n/)
	{
		last unless $s->{h}->pumpable;
		$s->{h}->pump;
	}
	my $gone = ($s->{out} !~ /$marker\n/) ? 1 : 0;
	return { out => $s->{out}, err => $s->{err}, gone => $gone };
}

sub sess_close
{
	my ($s) = @_;
	eval { $s->{h}->kill_kill; };
	return;
}

sub ext_locks
{
	my ($pid) = @_;
	return $node->safe_psql('d', qq{SELECT coalesce(string_agg(objsubid::text, ',' ORDER BY objsubid), '')
		FROM pg_locks WHERE locktype = 'object' AND classid = 'pg_extension'::regclass
		AND objsubid > 0 AND pid = $pid AND granted});
}

sub nothing_held
{
	return $node->safe_psql('d', q{SELECT count(*) FROM pg_locks
		WHERE locktype = 'object' AND classid = 'pg_extension'::regclass AND objsubid > 0}) eq '0';
}

sub backend_state
{
	my ($pid) = @_;
	return $node->safe_psql('d', qq{SELECT coalesce((SELECT state FROM pg_stat_activity WHERE pid = $pid), 'gone')});
}

#--------------------------------------------------------------------------
# P1
#--------------------------------------------------------------------------
my $tn = 0;
my @calls = (
	sub { 'SELECT kingres.ledger_verify_recorded() > 0' },
	sub { 'SELECT kingres.ledger_generate_digest() IS NULL' },
	sub { 'SELECT kingres.ledger_build_block() IS NULL' },
	sub { 'SELECT kingres.ledger_enable(\'p' . (++$tn) . '\') IS NULL' },
);

sub run_p1
{
	my ($seed) = @_;
	srand($seed);
	my $s = sess_open('app');
	my $state = 'idle';			# idle, block, aborted
	my @fail;
	my @log;
	my $steps = 14 + int(rand(10));
	for my $step (1 .. $steps)
	{
		last if $tn > 100;
		my $call = $calls[ int(rand(@calls)) ]->();
		my @choices = ('call', 'call', 'call', 'error', 'sleep', 'begin', 'do_caught', 'do_error', 'multi', 'pipeline');
		push @choices, ('block_call', 'savepoint_call') if $state eq 'block';
		push @choices, ('rollback', 'rollback') if $state ne 'idle';
		my $c = $choices[ int(rand(@choices)) ];
		my ($sql, $raw, $expect_err);
		my $new = $state;
		if ($c eq 'call')
		{
			if ($state eq 'idle') { $sql = $call; $expect_err = 0; }
			elsif ($state eq 'block') { $sql = $call; $expect_err = 1; $new = 'aborted'; }	# 25001
			else { $sql = $call; $expect_err = 1; }										# aborted: ignored
		}
		elsif ($c eq 'block_call' || $c eq 'savepoint_call')
		{
			$sql = $call;
			$expect_err = 1;
			$new = 'aborted';
		}
		elsif ($c eq 'error') { $sql = 'SELECT 1/0'; $expect_err = 1; $new = 'aborted' if $state eq 'block'; }
		elsif ($c eq 'sleep') { $sql = 'SELECT pg_sleep(0.05)'; $expect_err = ($state eq 'aborted') ? 1 : 0; }
		elsif ($c eq 'begin') { $sql = 'BEGIN'; $new = 'block' if $state eq 'idle'; $expect_err = ($state eq 'aborted') ? 1 : 0; }
		elsif ($c eq 'rollback') { $sql = 'ROLLBACK'; $new = 'idle'; $expect_err = 0; }
		elsif ($c eq 'multi')
		{
			# one message: the call is in an implicit block, refused
			next if $state ne 'idle';
			$sql = "$call \\; SELECT 1";
			$expect_err = 1;
		}
		elsif ($c eq 'pipeline')
		{
			next if $state ne 'idle';
			$raw = 1;
			$sql = "\\startpipeline\nSELECT 1 \\bind \\sendpipeline\n$call \\bind \\sendpipeline\n\\endpipeline";
			$expect_err = 1;
		}
		elsif ($c eq 'do_caught')
		{
			next if $state ne 'idle';
			(my $p = $call) =~ s/^SELECT /PERFORM /;
			$sql = "DO \$\$ BEGIN $p; BEGIN PERFORM 1/(SELECT 0); EXCEPTION WHEN division_by_zero THEN NULL; END; END \$\$";
			$expect_err = 0;
		}
		else
		{
			next if $state ne 'idle';
			(my $p = $call) =~ s/^SELECT /PERFORM /;
			$sql = "DO \$\$ BEGIN $p; RAISE EXCEPTION 'ends'; END \$\$";
			$expect_err = 1;
		}
		push @log, $sql;
		my $r = sess_send($s, $sql, $raw);
		if ($r->{gone})
		{
			push @fail, "step $step '$sql': the session is gone (" . join('; ', @log) . ")";
			last;
		}
		my $err = $r->{err} =~ /ERROR/ ? 1 : 0;
		push @fail, "step $step '$sql' ($state): expected " . ($expect_err ? 'an error' : 'no error') . ", got [$r->{err}] (" . join('; ', @log) . ")"
		  if $err != $expect_err;
		# the refusals have the code of the rule (a call in a block, a multi string, a pipeline)
		if ($c =~ /^(block_call|savepoint_call)$/ || ($c eq 'call' && $state eq 'block') || $c eq 'multi' || $c eq 'pipeline')
		{
			push @fail, "step $step '$sql': expected the refusal of the rule, got [$r->{err}]"
			  unless $r->{err} =~ /cannot run inside a transaction block for a role that is not a superuser/;
		}
		$state = $new;
		my $held = ext_locks($s->{pid});
		push @fail, "step $step '$sql': the session holds [$held] (" . join('; ', @log) . ")" if $held ne '';
		my $bs = backend_state($s->{pid});
		my $want = $state eq 'idle' ? 'idle' : $state eq 'block' ? 'idle in transaction' : 'idle in transaction (aborted)';
		push @fail, "step $step '$sql': the backend is '$bs', the model says '$want' (" . join('; ', @log) . ")" if $bs ne $want;
		last if @fail;
	}
	sess_close($s);
	return @fail;
}

my $seeds = $ENV{KL_PROP_SEEDS} || 15;
for my $seed (1 .. $seeds)
{
	my @fail = run_p1($seed);
	is(scalar(@fail), 0, "P1, seed $seed: after every step the session is alive, holds nothing, and is in the state of the model");
	diag(join("\n", @fail)) if @fail;
}
ok(nothing_held(), 'P1: nothing is held at the end');

#--------------------------------------------------------------------------
# P2
#--------------------------------------------------------------------------
$node->safe_psql('d', "ALTER ROLE app SET kingres_ledger.lock_hold_limit = '${LIMIT}s'");

sub model_do
{
	my ($seed) = @_;
	srand($seed * 7919);
	my $vt = 0;							# virtual seconds
	my $armed_at;						# virtual time the timer was armed
	my $max_age = 0;
	my $killed = 0;
	my $text = '';
	my $steps = 6 + int(rand(8));
	my $open = 0;
	my $n = 0;
	for my $step (1 .. $steps)
	{
		last if $killed;
		my @choices = ('sleep', 'sleep', 'call', 'caught', 'nest');
		push @choices, ('close_rollback', 'close_keep') if $open > 0;
		my $c = $choices[ int(rand(@choices)) ];
		$c = 'sleep' if $c eq 'call' && $open > 0;			# a call in a sub-block is refused (t/027)
		if ($c eq 'nest')
		{
			$text .= "BEGIN\n";
			$open++;
		}
		elsif ($c eq 'close_keep')
		{
			$text .= "EXCEPTION WHEN OTHERS THEN NULL;\nEND;\n";
			$open--;
		}
		elsif ($c eq 'close_rollback')
		{
			$text .= "RAISE EXCEPTION 'undo';\nEXCEPTION WHEN OTHERS THEN NULL;\nEND;\n";
			$open--;
		}
		elsif ($c eq 'caught')
		{
			$text .= "BEGIN PERFORM 1/(SELECT 0); EXCEPTION WHEN division_by_zero THEN NULL; END;\n";
		}
		elsif ($c eq 'sleep')
		{
			# 0.5 s when there is room, otherwise one that outlasts the limit
			my $age = defined $armed_at ? $vt - $armed_at : 0;
			my $d = 0.5;
			if (defined $armed_at)
			{
				if ($age + $d > $LIMIT - 1.5 && $age + $d < $LIMIT + 0.3) { $d = $LIMIT - $age + 1.0; }
				elsif (rand() < 0.15) { $d = $LIMIT - $age + 1.0; }
			}
			$text .= "PERFORM pg_sleep($d);\n";
			$vt += $d;
			if (defined $armed_at)
			{
				my $a = $vt - $armed_at;
				$max_age = $a if $a > $max_age;
				$killed = 1 if $a >= $LIMIT + 0.2;
			}
		}
		else
		{
			my $kind = int(rand(3));
			if ($kind == 0) { $text .= "PERFORM kingres.ledger_verify_recorded();\n"; }
			elsif ($kind == 1) { $text .= "PERFORM kingres.ledger_generate_digest();\n"; }
			else { $n++; $text .= "PERFORM kingres.ledger_enable('p" . (100 + $seed * 3 + $n) . "');\n"; }
			$armed_at = $vt unless defined $armed_at;
		}
	}
	while ($open-- > 0) { $text .= "EXCEPTION WHEN OTHERS THEN NULL;\nEND;\n"; }
	my $verdict;
	if ($killed) { $verdict = 'killed' }
	elsif (!defined $armed_at || $max_age <= $LIMIT - 1.5) { $verdict = 'finished' }
	else { $verdict = 'unclear' }
	return ($text, $verdict);
}

sub do_trial
{
	my ($seed) = @_;
	my ($text, $verdict);
	for my $try (0 .. 60)
	{
		($text, $verdict) = model_do($seed + 1000 * $try);
		last if $verdict ne 'unclear';
	}
	return if $verdict eq 'unclear';
	my $stmt = "DO \$\$ BEGIN\n$text RAISE EXCEPTION 'the end';\nEND \$\$";
	my ($ret, $out, $err) = $node->psql('d', $stmt, extra_params => [ '-U', 'app' ]);
	my $res = $err =~ /FATAL:  terminating connection due to administrator command/ ? 'killed'
	  : $err =~ /the end/ ? 'finished' : "other: $err";
	is($res, $verdict, "P2, seed $seed: the model says $verdict")
	  or diag($stmt . "\n" . $err);
	ok(nothing_held(), "P2, seed $seed: nothing is held after it");
	return;
}

my $do_seeds = $ENV{KL_PROP_DO_SEEDS} || 10;
do_trial($_) for 1 .. $do_seeds;

done_testing();
