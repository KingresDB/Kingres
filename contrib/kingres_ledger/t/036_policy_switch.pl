# SPDX-License-Identifier: AGPL-3.0-only
#
# contrib/kingres_ledger/t/036_policy_switch.pl
#
# L21-02, L21-03 and L21-04 of the review of client signatures: the switch of a
# table to the policy "required" is ordered with the writers of the table, so
# that after the switch has committed no transaction commits (or prepares) an
# entry of the table without an attestation, whatever its isolation level, what
# it cached, how old its snapshot is and what its transaction id is.
#
# The switch takes SHARE ROW EXCLUSIVE on the table (it waits for the open
# writers, later writers wait for it), a writer reads the policy with a snapshot
# of now when it first writes the table, and the date of the switch is read
# under the lock.  Each sequence is run in READ COMMITTED, REPEATABLE READ and
# SERIALIZABLE, and ends in KL010 or in a commit with attestations, never in a
# commit of an unattested entry:
#
#   A  the writer has written the table before the switch starts: the switch
#      waits for it (it is the lock, not the clock, that orders them), a writer
#      that comes after the switch waits for it too, and the first writer
#      commits unattested because it committed before the switch did;
#   B  the writer's snapshot is older than the switch, and it writes the table
#      after the switch has committed (KL010; attested: commits);
#   C  the writer begins while the switching transaction is open, and its write
#      waits for it (KL010; attested: commits);
#   D  the writer only read the table's identity (ledger_row_hash) before the
#      switch, which used to leave the answer "optional" cached;
#   E  the write is made by a deferred trigger at COMMIT;
#   F  the writer has a transaction id below the date of the switch;
#   G  (L21-03) a transaction with an id above the switching transaction's
#      commits before the switch: ledger_verify does not report it;
#   H  (L21-04) the switching transaction's own earlier writes, in savepoints too;
#   and: a prepared writer that holds the lock, a prepared switch, the bound on
#   the wait and on the hold of a role that is not a superuser, the order of the
#   locks.
#
# No test depends on the speed of the machine: a session is known to be waiting
# for a lock by asking pg_stat_activity (a condition, polled), never by a sleep;
# the timed cases use limits of seconds against waits and sleeps of a minute.
#
# Portions Copyright (c) 2026, RooyeKhat Media Co

use strict;
use warnings FATAL => 'all';

use FindBin;
use lib $FindBin::RealBin;

use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;
use KlSign qw(have_openssl new_key sign_message message_bytes hex_of unhex_of random_nonce);

plan skip_all => 'the openssl command (3.0 or later) is needed to make Ed25519 signatures'
  unless have_openssl();

$ENV{PGOPTIONS} = '-c search_path=public';

my $node = PostgreSQL::Test::Cluster->new('policy_switch');
$node->init;
$node->append_conf('postgresql.conf',
	"shared_preload_libraries = 'kingres_ledger'\n"
	  . "kingres_ledger.block_interval = 0\n"
	  . "max_prepared_transactions = 10\n"
	  . "deadlock_timeout = 300ms\n");
$node->start;

my $dir = PostgreSQL::Test::Utils::tempdir;
my $k1 = new_key($dir, 'k1');

$node->safe_psql('postgres', q{CREATE ROLE app LOGIN; CREATE ROLE other LOGIN; CREATE DATABASE d});
$node->safe_psql('d', q{
	CREATE EXTENSION kingres_ledger;
	GRANT USAGE ON SCHEMA kingres TO PUBLIC;
	GRANT EXECUTE ON FUNCTION kingres.ledger_attest(regclass, bytea, "char", bytea, bytea, bytea),
		kingres.ledger_set_signature_policy(regclass, text) TO app, other;
	CREATE TABLE plain (x int);
	GRANT ALL ON plain TO PUBLIC;
});

sub db { return $node->safe_psql('d', @_); }

my $chain = unhex_of(db(q{SELECT encode(chain_id, 'hex') FROM kingres.ledger_chain}));
db(sprintf(q{SELECT kingres.ledger_client_key_register(decode('%s', 'hex'), NULL, 'k1')}, hex_of($k1->{pub})));

my %tid;
sub new_table
{
	my ($name, $owner) = @_;
	$owner //= 'app';
	db("CREATE TABLE $name (id int PRIMARY KEY, body text);
		ALTER TABLE $name OWNER TO $owner;
		SELECT kingres.ledger_enable('$name')");
	$tid{$name} = db("SELECT table_id FROM kingres.ledger_table WHERE relid = '$name'::regclass");
	return $name;
}

sub policy_of
{
	return db("SELECT signature_policy FROM kingres.ledger_table WHERE relid = '$_[0]'::regclass");
}

# Entries of the table that ledger_verify asks an attestation of (after the date of the switch) and that have none.
sub unattested
{
	return db(qq{SELECT count(*) FROM kingres.ledger_entry e JOIN kingres.ledger_table t USING (table_id)
		WHERE t.relid = '$_[0]'::regclass AND t.signature_policy = 'required'
		AND e.ledger_txid > t.signature_required_from
		AND NOT EXISTS (SELECT 1 FROM kingres.ledger_attestation a WHERE a.ledger_txid = e.ledger_txid AND a.seq = e.seq)});
}

sub session_as
{
	my ($role) = @_;
	local $ENV{PGUSER} = $role if defined $role;
	my $s = $node->background_psql('d', on_error_stop => 0);
	$s->query_safe(q{\set VERBOSITY sqlstate});
	return $s;
}

sub failure_of
{
	my ($s, $sql) = @_;
	$s->{stderr} = '';
	$s->query($sql);
	my ($state) = $s->{stderr} =~ /ERROR:\s+(\S+)/;
	$s->{stderr} = '';
	return $state // '';
}

# Start a statement that is expected to wait; the answer is read by finish_async.
sub start_async
{
	my ($s, $sql) = @_;
	$s->query_until(qr/async_started/, "\\echo async_started\n$sql;\n");
}

sub finish_async
{
	my ($s) = @_;
	$s->{stderr} = '';
	$s->query('SELECT 1');
	my ($state) = $s->{stderr} =~ /ERROR:\s+(\S+)/;
	$s->{stderr} = '';
	return $state // '';
}

# True once a backend whose statement contains the text is waiting for a lock.
sub waits_for_lock
{
	my ($text) = @_;
	$text =~ s/'/''/g;
	return $node->poll_query_until('d', qq{SELECT EXISTS (SELECT 1 FROM pg_stat_activity
		WHERE datname = 'd' AND pid <> pg_backend_pid() AND wait_event_type = 'Lock'
		AND query LIKE '%$text%')});
}

sub row_hash
{
	my ($table, $id, $body) = @_;
	return db(sprintf(q{SELECT encode(kingres.ledger_row_hash(ROW(%d, '%s')::%s), 'hex')}, $id, $body, $table));
}

sub attest_sql
{
	my ($table, $hash_hex) = @_;
	my $nonce = random_nonce();
	my $msg = message_bytes(chain_id => $chain, table_id => $tid{$table}, op => 'I',
		row_hash => unhex_of($hash_hex), key_id => $k1->{key_id}, nonce => $nonce);
	my $sig = sign_message($k1, $msg);
	return sprintf(q{SELECT kingres.ledger_attest('%s'::regclass, decode('%s', 'hex'), 'I', decode('%s', 'hex'), decode('%s', 'hex'), decode('%s', 'hex'))},
		$table, $hash_hex, hex_of($k1->{key_id}), hex_of($nonce), hex_of($sig));
}

sub switch_sql { return "SELECT kingres.ledger_set_signature_policy('$_[0]', 'required')"; }

my @levels = ('READ COMMITTED', 'REPEATABLE READ', 'SERIALIZABLE');

foreach my $iso (@levels)
{
	my $tag = lc($iso);
	$tag =~ s/ /_/g;

	#----------------------------------------------------------------------
	# A: the writer wrote the table before the switch started
	#----------------------------------------------------------------------
	{
		my $t = new_table("a_$tag");
		my $w = session_as('app');
		my $s = session_as('app');
		my $w2 = session_as('app');
		$w->query_safe("BEGIN ISOLATION LEVEL $iso");
		$w->query_safe("INSERT INTO $t VALUES (1, 'w1')");
		start_async($s, switch_sql($t));
		ok(waits_for_lock("ledger_set_signature_policy('$t'"), "[$iso A] the switch waits for the open writer of the table");
		is(policy_of($t), 'optional', "[$iso A] and has switched nothing");
		$w2->query_safe("BEGIN ISOLATION LEVEL $iso");
		start_async($w2, "INSERT INTO $t VALUES (5, 'w2')");
		ok(waits_for_lock("INSERT INTO $t VALUES (5"), "[$iso A] a writer that comes later waits behind the switch");
		$w->query_safe("INSERT INTO $t VALUES (2, 'w1b')");
		is(failure_of($w, 'COMMIT'), '', "[$iso A] the first writer commits: it committed before the switch did");
		is(finish_async($s), '', "[$iso A] and then the switch completes");
		is(policy_of($t), 'required', "[$iso A] the policy is required");
		is(finish_async($w2), '', "[$iso A] the later writer's INSERT completes after the switch");
		is(failure_of($w2, 'COMMIT'), 'KL010', "[$iso A] and its COMMIT without an attestation is refused");
		is(db("SELECT count(*) FROM $t"), '2', "[$iso A] only the first writer's rows are there");
		is(db("SELECT bool_and(e.ledger_txid <= t.signature_required_from)::text FROM kingres.ledger_entry e
			JOIN kingres.ledger_table t USING (table_id) WHERE t.relid = '$t'::regclass"), 'true',
			"[$iso A] the date of the switch is above the entries that were committed before it");
		is(unattested($t), '0', "[$iso A] ledger_verify has nothing to report for the table");
		$_->quit for ($w, $s, $w2);
	}

	#----------------------------------------------------------------------
	# B: the snapshot is older than the switch, the write comes after it
	#----------------------------------------------------------------------
	foreach my $attested (0, 1)
	{
		my $t = new_table("b${attested}_$tag");
		my $w = session_as('app');
		$w->query_safe("BEGIN ISOLATION LEVEL $iso");
		$w->query_safe("SELECT count(*) FROM $t");		# the snapshot, before the switch started
		db(switch_sql($t));
		is(policy_of($t), 'required', "[$iso B$attested] the switch committed while the writer was open");
		$w->query_safe("INSERT INTO $t VALUES (1, 'late')");
		if ($attested)
		{
			$w->query_safe(attest_sql($t, row_hash($t, 1, 'late')));
			is(failure_of($w, 'COMMIT'), '', "[$iso B$attested] attested: it commits");
			is(db("SELECT count(*) FROM $t"), '1', "[$iso B$attested] and the row is there");
		}
		else
		{
			is(failure_of($w, 'COMMIT'), 'KL010', "[$iso B$attested] unattested: KL010 (the snapshot was older than the switch)");
			is(db("SELECT count(*) FROM $t"), '0', "[$iso B$attested] and nothing was written");
		}
		is(unattested($t), '0', "[$iso B$attested] no unattested entry");
		$w->quit;
	}

	#----------------------------------------------------------------------
	# C: the writer begins while the switching transaction is open
	#----------------------------------------------------------------------
	foreach my $attested (0, 1)
	{
		my $t = new_table("c${attested}_$tag");
		my $sw = session_as();		# a superuser: the switch inside a block
		my $w = session_as('app');
		$sw->query_safe('BEGIN');
		$sw->query_safe(switch_sql($t));
		$w->query_safe("BEGIN ISOLATION LEVEL $iso");
		start_async($w, "INSERT INTO $t VALUES (1, 'c')");
		ok(waits_for_lock("INSERT INTO $t VALUES (1"), "[$iso C$attested] the writer that began during the switch waits for it");
		$sw->query_safe('COMMIT');
		is(finish_async($w), '', "[$iso C$attested] its INSERT completes after the switch committed");
		if ($attested)
		{
			$w->query_safe(attest_sql($t, row_hash($t, 1, 'c')));
			is(failure_of($w, 'COMMIT'), '', "[$iso C$attested] attested: it commits");
			is(db("SELECT count(*) FROM $t"), '1', "[$iso C$attested] and the row is there");
		}
		else
		{
			is(failure_of($w, 'COMMIT'), 'KL010', "[$iso C$attested] unattested: KL010");
			is(db("SELECT count(*) FROM $t"), '0', "[$iso C$attested] and nothing was written");
		}
		is(unattested($t), '0', "[$iso C$attested] no unattested entry");
		$_->quit for ($sw, $w);
	}

	#----------------------------------------------------------------------
	# D: the transaction only read the identity of the table before the switch
	#----------------------------------------------------------------------
	{
		my $t = new_table("d_$tag");
		my $w = session_as('app');
		$w->query_safe("BEGIN ISOLATION LEVEL $iso");
		$w->query_safe(qq{SELECT kingres.ledger_row_hash(ROW(1, 'd')::$t)});
		$w->query_safe("SELECT count(*) FROM $t");
		db(switch_sql($t));
		$w->query_safe("INSERT INTO $t VALUES (1, 'd')");
		is(failure_of($w, 'COMMIT'), 'KL010', "[$iso D] what a reader learned of the table before the switch is not kept: KL010");
		is(db("SELECT count(*) FROM $t"), '0', "[$iso D] nothing was written");
		$w->quit;
	}

	#----------------------------------------------------------------------
	# E: the write is made at COMMIT by a deferred trigger
	#----------------------------------------------------------------------
	{
		my $t = new_table("e_$tag");
		db("CREATE TABLE src_$tag (id int);
			GRANT ALL ON src_$tag TO PUBLIC;
			CREATE FUNCTION fn_$tag() RETURNS trigger LANGUAGE plpgsql AS \$f\$
				BEGIN INSERT INTO $t VALUES (NEW.id, 'deferred'); RETURN NULL; END \$f\$;
			ALTER FUNCTION fn_$tag() OWNER TO app;
			CREATE CONSTRAINT TRIGGER trg_$tag AFTER INSERT ON src_$tag DEFERRABLE INITIALLY DEFERRED
				FOR EACH ROW EXECUTE FUNCTION fn_$tag()");
		my $w = session_as('app');
		$w->query_safe("BEGIN ISOLATION LEVEL $iso");
		$w->query_safe("INSERT INTO src_$tag VALUES (1)");	# the table is not written yet: no lock on it
		db(switch_sql($t));								# so the switch does not wait
		is(failure_of($w, 'COMMIT'), 'KL010', "[$iso E] a deferred trigger that writes the table at COMMIT after the switch: KL010");
		is(db("SELECT count(*) FROM $t") . db("SELECT count(*) FROM src_$tag"), '00', "[$iso E] nothing was written");
		$w->quit;
	}

	#----------------------------------------------------------------------
	# F: the writer's transaction id is below the date of the switch
	#----------------------------------------------------------------------
	{
		my $t = new_table("f_$tag");
		my $w = session_as('app');
		$w->query_safe("BEGIN ISOLATION LEVEL $iso");
		$w->query_safe('INSERT INTO plain VALUES (1)');		# the transaction has an id now
		db(switch_sql($t));
		$w->query_safe("INSERT INTO $t VALUES (1, 'f')");
		is(failure_of($w, 'COMMIT'), 'KL010', "[$iso F] a writer with an old transaction id is asked for an attestation at COMMIT");
		$w->quit;
	}
}

#--------------------------------------------------------------------------
# G (L21-03): a transaction with an id above the switching transaction's, committed before it
#--------------------------------------------------------------------------
{
	my $t = new_table('g1');
	my $sw = session_as();
	my $w = session_as('app');
	$sw->query_safe('BEGIN');
	$sw->query_safe('INSERT INTO plain VALUES (1)');			# the switching transaction takes its id first
	$w->query_safe('BEGIN');
	$w->query_safe("INSERT INTO g1 VALUES (1, 'v')");			# an id above the switching transaction's
	is(failure_of($w, 'COMMIT'), '', '[G] it commits: the policy was not in force');
	$sw->query_safe(switch_sql($t));
	$sw->query_safe('COMMIT');
	db('SELECT kingres.ledger_build_block()');
	is(db(q{SELECT count(*) FROM kingres.ledger_verify() WHERE check_name = 'unattested_entry'}), '0',
		'[G] ledger_verify does not report an entry that was committed before the switch committed');
	is(db(q{SELECT count(*) FROM kingres.ledger_verify()}), '0', '[G] and the ledger verifies');
	$_->quit for ($sw, $w);
}

#--------------------------------------------------------------------------
# H (L21-04): the switching transaction's own earlier writes
#--------------------------------------------------------------------------
{
	my $n = 0;
	my $case = sub {
		my ($label, $steps, $expect, $policy_after) = @_;
		my $t = new_table('h' . ++$n);
		my $s = session_as();
		$s->query_safe('BEGIN');
		foreach my $step (@$steps)
		{
			my $sql = ref($step) ? $step->($t) : $step;
			$s->query_safe($sql);
		}
		is(failure_of($s, 'COMMIT'), $expect, "[H] $label");
		is(policy_of($t), $policy_after, "[H] $label: the policy afterwards");
		is(unattested($t), '0', "[H] $label: nothing unattested is committed");
		$s->quit;
	};
	my $ins = sub { my ($id) = @_; return sub { "INSERT INTO $_[0] VALUES ($id, 'v$id')" } };
	my $sw = sub { switch_sql($_[0]) };
	my $att = sub { my ($id) = @_; return sub { attest_sql($_[0], row_hash($_[0], $id, "v$id")) } };

	$case->('a row written before the switch in the same transaction is owed an attestation',
		[ $ins->(1), $sw ], 'KL010', 'optional');
	$case->('and with it the transaction commits and the table is switched',
		[ $ins->(1), $sw, $att->(1) ], '', 'required');
	$case->('a row that was attested (optionally) before the switch is not owed another',
		[ $ins->(1), $att->(1), $sw ], '', 'required');
	$case->('the first row attested, the second written after the switch is not',
		[ $ins->(1), $att->(1), $sw, $ins->(2) ], 'KL010', 'optional');
	$case->('a switch in a savepoint that is rolled back takes the debt with it',
		[ $ins->(1), 'SAVEPOINT s', $sw, 'ROLLBACK TO s' ], '', 'optional');
	$case->('a switch in a savepoint that is released keeps it',
		[ $ins->(1), 'SAVEPOINT s', $sw, 'RELEASE s' ], 'KL010', 'optional');
	$case->('the row and the switch in nested savepoints that are rolled back by the outer one',
		[ 'SAVEPOINT s', $ins->(1), 'SAVEPOINT t', $sw, 'RELEASE t', 'ROLLBACK TO s' ], '', 'optional');
	$case->('the row in an outer savepoint, the switch in the inner one that is released, attested outside',
		[ 'SAVEPOINT s', $ins->(1), 'SAVEPOINT t', $sw, 'RELEASE t', 'RELEASE s', $att->(1) ], '', 'required');
	$case->('the attestation made in a savepoint that is rolled back is owed again',
		[ $ins->(1), $sw, 'SAVEPOINT s', $att->(1), 'ROLLBACK TO s' ], 'KL010', 'optional');
}

#--------------------------------------------------------------------------
# A prepared writer holds the lock across the switch; a prepared switch holds it too
#--------------------------------------------------------------------------
{
	my $t = new_table('p1');
	my $w = session_as();
	$w->query_safe('BEGIN');
	$w->query_safe("INSERT INTO p1 VALUES (1, 'p')");
	$w->query_safe(q{PREPARE TRANSACTION 'pw'});
	my $sw = session_as();
	is(failure_of($sw, q{SET lock_timeout = '1s'; } . switch_sql($t)), '55P03',
		'[P] the switch waits for a prepared writer (a lock timeout of the caller ends the wait)');
	is(policy_of($t), 'optional', '[P] and has switched nothing');
	db(q{COMMIT PREPARED 'pw'});
	$sw->query_safe(switch_sql($t));
	is(policy_of($t), 'required', '[P] it succeeds when the prepared writer is committed');
	is(unattested($t), '0', '[P] the prepared writer committed before the switch: not asked for');
	$_->quit for ($w, $sw);

	$t = new_table('p2');
	$sw = session_as();
	$sw->query_safe('BEGIN');
	$sw->query_safe(switch_sql($t));
	$sw->query_safe(q{PREPARE TRANSACTION 'ps'});
	$w = session_as('app');
	$w->query_safe('BEGIN');
	start_async($w, "INSERT INTO p2 VALUES (1, 'p')");
	ok(waits_for_lock("INSERT INTO p2 VALUES (1"), '[P] a writer waits for a prepared switch (it holds the lock across the prepare)');
	db(q{COMMIT PREPARED 'ps'});
	is(finish_async($w), '', '[P] and goes on when it is committed');
	is(failure_of($w, 'COMMIT'), 'KL010', '[P] with the policy in force: KL010');
	$_->quit for ($w, $sw);
}

#--------------------------------------------------------------------------
# A role that is not a superuser: the wait is bounded, the hold is bounded, the call is a statement of its own
#--------------------------------------------------------------------------
{
	$node->safe_psql('d', q{ALTER ROLE other SET kingres_ledger.lock_hold_limit = '2s'});
	my $t = new_table('q1', 'other');
	my $w = session_as();
	$w->query_safe('BEGIN');
	$w->query_safe("INSERT INTO q1 VALUES (1, 'q')");
	my $o = session_as('other');
	$o->{stderr} = '';
	$o->query(switch_sql($t));
	like($o->{stderr}, qr/ERROR:\s+55P03/, '[Q] the wait of a role that is not a superuser for the writers of the table ends at the limit');
	$o->{stderr} = '';
	is(policy_of($t), 'optional', '[Q] the table is not switched');
	$w->query_safe('ROLLBACK');

	# after the lock is granted the hold is bounded: the session is ended at the limit
	my $log_start = -s $node->logfile;
	my ($ret, $out, $err) = $node->psql('d', switch_sql($t) . ', pg_sleep(60)', extra_params => [ '-U', 'other' ]);
	isnt($ret, 0, '[Q] a statement that holds the lock after the switch is ended');
	like($err, qr/terminating connection/, '[Q] by a terminate request');
	ok($node->wait_for_log(qr/ended the session of role "other": it held the lock on the table of a signature policy of the extension for longer than kingres_ledger\.lock_hold_limit \(2000 ms\)/, $log_start),
		'[Q] and the log says which limit did it');
	is(policy_of($t), 'optional', '[Q] and the switch went with the transaction');

	$o->quit;
	$o = session_as('other');
	$o->query_safe('BEGIN');
	is(failure_of($o, switch_sql($t)), '25001', '[Q] inside a transaction block the call is refused');
	$o->query_safe('ROLLBACK');
	$o->query_safe(switch_sql($t));
	is(policy_of($t), 'required', '[Q] as a statement of its own it works');
	$_->quit for ($w, $o);
}

#--------------------------------------------------------------------------
# The order of the locks: no wait for a table while a lock of the extension is held
#--------------------------------------------------------------------------
{
	my $t = new_table('o1');
	my $w = session_as();
	$w->query_safe('BEGIN');
	$w->query_safe("INSERT INTO o1 VALUES (1, 'o')");
	my $sw = session_as();
	$sw->query_safe('BEGIN');
	$sw->query_safe('SELECT kingres.ledger_verify_recorded()');
	is(failure_of($sw, switch_sql($t)), 'KL006',
		'[O] a transaction that holds the verifications\' lock does not wait for a table (a writer of it could wait for that lock)');
	$sw->query_safe('ROLLBACK');
	$w->query_safe('ROLLBACK');
	$sw->query_safe('BEGIN');
	$sw->query_safe('SELECT kingres.ledger_verify_recorded()');
	$sw->query_safe(switch_sql($t));
	$sw->query_safe('COMMIT');
	is(policy_of($t), 'required', '[O] when no writer holds the table there is no wait, and the call passes');
	$_->quit for ($w, $sw);
}

db('SELECT kingres.ledger_build_block()');
is(db(q{SELECT count(*) FROM kingres.ledger_verify() WHERE check_name = 'unattested_entry'}), '0',
	'at the end ledger_verify has no unattested entry to report');
is(db(q{SELECT count(*) FROM kingres.ledger_verify()}), '0', 'and the ledger verifies');

done_testing();
