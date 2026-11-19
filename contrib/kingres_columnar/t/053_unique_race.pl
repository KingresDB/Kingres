# contrib/kingres_columnar/t/053_unique_race.pl
#
# C6c: a unique index on a columnar table never admits a duplicate.
#  1. pgbench with 2, 4 and 8 sessions inserting overlapping keys into a columnar table
#     and into a heap twin in the same subtransaction: afterwards each key is in each
#     table once and the same keys have won in both.
#  2. A model check: random interleavings of two transactions (insert of one of three
#     keys, commit, rollback, prepare, commit prepared, rollback prepared, with a lock
#     timeout so a wait ends) are run in lockstep against the columnar table and the
#     heap twin, at each isolation level; every step has the same outcome in both
#     (done, unique violation, serialization failure, timeout) and at the end the
#     tables hold the same keys.
#
# Portions Copyright (c) 2026, RooyeKhat Media Co
# SPDX-License-Identifier: AGPL-3.0-only

use strict;
use warnings FATAL => 'all';

use FindBin;
use lib $FindBin::RealBin;
use KcReserve qw(new_node q1);
use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;

my $node = new_node('urace');

q1($node, q{
	CREATE TABLE c (k int, who int, b text) USING columnar;
	CREATE UNIQUE INDEX c_k ON c (k);
	CREATE TABLE h (k int, who int, b text);
	CREATE UNIQUE INDEX h_k ON h (k);
});

# ---------------------------------------------------------------------------
# 1. pgbench
# ---------------------------------------------------------------------------
my $script = $node->basedir . '/ukeys.pgb';
append_to_file($script, q{\set k random(1, 300)
DO $$ BEGIN
  BEGIN
    INSERT INTO h VALUES (:k, :client_id, 'x');
    INSERT INTO c VALUES (:k, :client_id, 'x');
  EXCEPTION WHEN unique_violation THEN NULL;
  END;
END $$;
});

for my $clients (2, 4, 8)
{
	q1($node, "TRUNCATE c, h");
	$node->command_ok([ 'pgbench', '-n', '-c', $clients, '-j', $clients, '-t', 150, '-f', $script, 'postgres' ],
		"$clients sessions insert overlapping keys");
	is(q1($node, "SELECT count(*) - count(DISTINCT k) FROM c"), 0, "$clients sessions: no key twice in the columnar table");
	is(q1($node, "SELECT count(*) - count(DISTINCT k) FROM h"), 0, "$clients sessions: no key twice in the heap table");
	is(q1($node, "SELECT count(*) FROM (SELECT k, who FROM c EXCEPT SELECT k, who FROM h) s"), 0,
		"$clients sessions: the same sessions won the same keys");
	is(q1($node, "SELECT count(*) FROM (SELECT k, who FROM h EXCEPT SELECT k, who FROM c) s"), 0, "... in both directions");
	cmp_ok(q1($node, "SELECT count(*) FROM c"), '>', 100, "$clients sessions: many of the 300 keys were taken");
	is(q1($node, "SELECT count(*) FROM c WHERE k IN (SELECT k FROM c)"), q1($node, "SELECT count(*) FROM c"), "the index and the table agree");
	is(q1($node, "SELECT count(*) FROM kingres.columnar_check_indexes('c')"), 0, "$clients sessions: columnar_check_indexes finds nothing");
}

# ---------------------------------------------------------------------------
# 2. the model check
# ---------------------------------------------------------------------------
srand(20261119);
q1($node, 'TRUNCATE c, h');
my %mine;    # per table: two sessions
for my $t ('c', 'h')
{
	$mine{$t} = [ map { $node->background_psql('postgres', on_error_stop => 0) } 1 .. 2 ];
}

# one step: run it and say what happened
sub step
{
	my ($s, $sql) = @_;
	$s->{stderr} = '';
	$s->query($sql);
	my $e = $s->{stderr};
	return 'ok' if $e eq '';
	return 'dup' if $e =~ /duplicate key/;
	return 'serial' if $e =~ /could not serialize/;
	return 'timeout' if $e =~ /lock timeout/;
	return 'nogid' if $e =~ /prepared transaction with identifier/;
	return 'aborted' if $e =~ /current transaction is aborted/;
	return 'warn' if $e =~ /WARNING/ && $e !~ /ERROR/;
	$e =~ s/\s+/ /g;
	return "other: $e";
}

my $seqs = $ENV{KC_URACE_SEQS} || 300;
my $mismatch = 0;
my %seen;
my $gid = 0;
for my $n (1 .. $seqs)
{
	my $iso = ('read committed', 'repeatable read', 'serializable')[ $n % 3 ];
	my @pending;    # prepared transactions in this run
	my @trace;
	my %state = (0 => 'idle', 1 => 'idle');
	# both tables run the same script
	my @script;
	for my $i (1 .. 7)
	{
		my $who = int(rand(2));
		my $r = rand();
		my $op;
		if ($state{$who} eq 'idle' && $r < 0.85) { $op = [ 'begin', $who ]; $state{$who} = 'in'; }
		elsif ($state{$who} eq 'in' && $r < 0.55) { $op = [ 'ins', $who, 1 + int(rand(3)) ]; }
		elsif ($state{$who} eq 'in' && $r < 0.70) { $op = [ 'commit', $who ]; $state{$who} = 'idle'; }
		elsif ($state{$who} eq 'in' && $r < 0.82) { $op = [ 'rollback', $who ]; $state{$who} = 'idle'; }
		elsif ($state{$who} eq 'in') { $op = [ 'prepare', $who, ++$gid ]; $state{$who} = 'idle'; push @pending, $gid; }
		elsif (@pending && $r >= 0.85) { $op = [ rand() < 0.5 ? 'cp' : 'rp', $who, shift @pending ]; }
		else { $op = [ 'ins', $who, 1 + int(rand(3)) ]; }    # outside a transaction: autocommit
		push @script, $op;
	}
	# the rest: end every open transaction and prepared one
	for my $who (0, 1) { push @script, [ 'rollback', $who ] if $state{$who} eq 'in'; }
	push @script, [ 'rp', 0, $_ ] for @pending;

	my (%out, %sess);
	for my $t ('c', 'h')
	{
		$sess{$t} = $mine{$t};
		$_->query("SET lock_timeout = '30ms'; SET default_transaction_isolation = '$iso';") for @{ $sess{$t} };
		my @res;
		for my $op (@script)
		{
			my ($kind, $who, $arg) = @$op;
			my $s = $sess{$t}[$who];
			my $sql =
			    $kind eq 'begin' ? 'BEGIN;'
			  : $kind eq 'ins' ? "INSERT INTO $t VALUES ($arg, $who, 'm');"
			  : $kind eq 'commit' ? 'COMMIT;'
			  : $kind eq 'rollback' ? 'ROLLBACK;'
			  : $kind eq 'prepare' ? "PREPARE TRANSACTION 'g$arg$t';"
			  : $kind eq 'cp' ? "COMMIT PREPARED 'g$arg$t';"
			  :                 "ROLLBACK PREPARED 'g$arg$t';";
			# a prepared transaction can only be ended outside a transaction block
			push @res, step($s, $sql);
		}
		$out{$t} = join(',', @res);
	}
	# prepared transactions that failed to prepare are gone; the others are ended above
	my $keys_c = q1($node, "SELECT coalesce(string_agg(k || ':' || who, ',' ORDER BY k), '') FROM c");
	my $keys_h = q1($node, "SELECT coalesce(string_agg(k || ':' || who, ',' ORDER BY k), '') FROM h");
	if ($out{c} ne $out{h} || $keys_c ne $keys_h)
	{
		$mismatch++;
		diag("sequence $n ($iso): " . join(' ', map { join('/', @$_) } @script));
		diag("columnar: $out{c} -> $keys_c");
		diag("heap:     $out{h} -> $keys_h");
	}
	$seen{$_}++ for split /,/, $out{c};
	is(q1($node, "SELECT count(*) - count(DISTINCT k) FROM c"), 0, "sequence $n: no key twice") if $n % 20 == 0;
	q1($node, "SELECT gid FROM pg_prepared_xacts") eq '' or do {
		q1($node, "DO \$\$ DECLARE g text; BEGIN FOR g IN SELECT gid FROM pg_prepared_xacts LOOP EXECUTE format('ROLLBACK PREPARED %L', g); END LOOP; END \$\$");
	};
	q1($node, "TRUNCATE c, h");
}
is($mismatch, 0, "$seqs random interleavings: the columnar table and the heap twin behave the same");
cmp_ok($seen{dup} // 0, '>', 5, "the interleavings include unique violations ($seen{dup})");
cmp_ok($seen{timeout} // 0, '>', 2, "... and waits that ended in a timeout ($seen{timeout})");
note(join(', ', map { "$_=$seen{$_}" } sort keys %seen));
$_->quit for map { @$_ } values %mine;

is(q1($node, "SELECT count(*) FROM kingres.columnar_check_indexes('c')"), 0, 'columnar_check_indexes finds nothing at the end');
$node->stop;
done_testing();
