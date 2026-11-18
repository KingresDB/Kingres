# SPDX-License-Identifier: AGPL-3.0-only
#
# contrib/kingres_ledger/t/018_second_invariant.pl
#
# The second invariant (L11-01 to L11-05):
#
#   a role that is not a superuser cannot stop the block builder or the capture
#   of rows for longer than one of its own statements runs, and cannot remove
#   the ledger or the protection of a ledger table.
#
# The regression test ledger_stops runs every route in one session and undoes it.
# What a regression test cannot show is what a role *keeps*: a lock that is held
# in an open transaction, which the builder and the capture triggers wait for,
# and the wait of a statement that is refused but queues for a lock first.  Every route here is run by a role in a transaction that
# the role leaves open; a second session then looks at pg_locks for what the
# role holds on the tables of the ledger, and does what a writer and the block
# builder do (an insert into a ledger table, ledger_build_block) under a short
# lock_timeout.  A control runs first: a superuser that takes the lock gets it
# (and stops the builder), so the test can fail.
#
# It also drives what the earlier reviews listed as not covered: a prepared
# transaction that holds entries, two registrations of the same table at once,
# ledger_remove_stray against a running builder, and the write guard under
# debug_parallel_query.
#
# Portions Copyright (c) 2026, RooyeKhat Media Co

use strict;
use warnings FATAL => 'all';

use PostgreSQL::Test::Cluster;
use PostgreSQL::Test::Utils;
use Test::More;

$ENV{PGOPTIONS} = '-c search_path=public';

my $node = PostgreSQL::Test::Cluster->new('invariant2');
$node->init;
$node->append_conf('postgresql.conf',
	"shared_preload_libraries = 'kingres_ledger'\n"
	  . "kingres_ledger.block_interval = 0\n"
	  . "max_prepared_transactions = 5\n");
$node->start;

my $super = $node->safe_psql('postgres', 'SELECT current_user');

$node->safe_psql('postgres', qq{
	CREATE ROLE inh LOGIN;
	GRANT "$super" TO inh WITH INHERIT TRUE, SET FALSE;
	CREATE ROLE maint LOGIN IN ROLE pg_maintain;
	CREATE ROLE wad LOGIN IN ROLE pg_write_all_data;
	CREATE ROLE app LOGIN;
	CREATE ROLE aud LOGIN;
	CREATE DATABASE d;
});
$node->safe_psql('d', q{
	CREATE EXTENSION kingres_ledger;
	GRANT USAGE ON SCHEMA kingres TO PUBLIC;
	GRANT CREATE ON SCHEMA public TO inh, maint, wad, app, aud;
	GRANT EXECUTE ON FUNCTION kingres.ledger_build_block(), kingres.ledger_generate_digest() TO aud;
	CREATE TABLE docs (id int PRIMARY KEY, body text);
	ALTER TABLE docs OWNER TO app;
	CREATE TABLE acct (id serial PRIMARY KEY, body text);
	SELECT kingres.ledger_enable('docs');
	SELECT kingres.ledger_enable('acct');
	INSERT INTO docs VALUES (1, 'a');
	INSERT INTO acct (body) VALUES ('a');
	SELECT kingres.ledger_build_block();
});
sub as_role
{
	my ($role, $sql) = @_;
	return $node->safe_psql('d', $sql, extra_params => [ '-U', $role ]);
}
as_role('maint', q{
	CREATE VIEW v_blocks AS SELECT * FROM kingres.ledger_blocks;
	CREATE VIEW v_pend AS SELECT * FROM kingres.ledger_pending;
	CREATE VIEW vv_pend AS SELECT * FROM v_pend;
	CREATE VIEW v_sub AS SELECT id FROM docs WHERE id IN (SELECT height FROM kingres.ledger_blocks);
	CREATE VIEW v_harmless AS SELECT * FROM acct;
});
as_role('wad', q{
	CREATE VIEW w_blocks AS SELECT * FROM kingres.ledger_blocks;
	CREATE VIEW w_pend AS SELECT * FROM kingres.ledger_pending;
	CREATE VIEW ww_pend AS SELECT * FROM w_pend;
});

# The locks of a backend on the tables of the ledger that conflict with a write
# of the capture triggers or of the builder (ROW EXCLUSIVE on the queue, the
# entries, the registry, the blocks and the transactions).
sub harmful_locks
{
	my ($pid) = @_;
	return $node->safe_psql('d', qq{
		SELECT coalesce(string_agg(c.relname || ':' || l.mode, ',' ORDER BY c.relname, l.mode), '')
		  FROM pg_locks l JOIN pg_class c ON c.oid = l.relation
		 WHERE l.pid = $pid AND l.locktype = 'relation'
		   AND c.relnamespace = 'kingres'::regnamespace
		   AND l.mode IN ('ShareLock', 'ShareRowExclusiveLock', 'ExclusiveLock', 'AccessExclusiveLock')});
}

# What a writer and the block builder do, as a superuser, under a short timeout.
sub honest_work
{
	my ($ret, $out, $err) = $node->psql('d', q{SET lock_timeout = '10s';
		INSERT INTO acct (body) VALUES ('x');
		SELECT kingres.ledger_build_block() IS NOT NULL});
	return ($out =~ /^t$/m && $err !~ /lock timeout/) ? 'works' : "stopped: $err";
}

# A role runs statements in a transaction that it leaves open (errors do not
# end the session: the transaction is then aborted, which releases its locks).
# Returns what it holds on the ledger and whether the ledger still works.
sub route
{
	my ($role, @stmts) = @_;
	my $s;
	{
		local $ENV{PGUSER} = $role;
		$s = $node->background_psql('d', on_error_stop => 0);
	}
	my $pid = $s->query_safe('SELECT pg_backend_pid()');
	$pid =~ s/\s+//g;
	$s->query('BEGIN');
	$s->query($_) for @stmts;
	my $held = harmful_locks($pid);
	my $work = honest_work();
	$s->query('ROLLBACK');
	$s->quit;
	return ($held, $work);
}

sub route_ok
{
	my ($role, $label, @stmts) = @_;
	my ($held, $work) = route($role, @stmts);
	is($held, '', "$role: $label: holds nothing that conflicts with the ledger");
	is($work, 'works', "$role: $label: a writer and the block builder are not stopped");
}

#--------------------------------------------------------------------------
# The control: the harness sees a lock that stops the ledger
#--------------------------------------------------------------------------
{
	my ($held, $work) = route($super, 'LOCK TABLE kingres.ledger_pending IN ACCESS EXCLUSIVE MODE');
	is($held, 'ledger_pending:AccessExclusiveLock', 'control: a superuser that locks the queue is seen holding it');
	like($work, qr/^stopped: .*lock timeout/s, 'control: and the writer and the builder are stopped by it');
	is(honest_work(), 'works', 'control: and everything works again when it lets go');
}

#--------------------------------------------------------------------------
# L11-01: a lock through a view, as pg_maintain and as pg_write_all_data
#--------------------------------------------------------------------------
my @maint = (
	[ 'lock the table (direct)', q{LOCK TABLE kingres.ledger_blocks IN ACCESS EXCLUSIVE MODE} ],
	[ 'lock a view over the blocks', q{LOCK TABLE v_blocks IN ACCESS EXCLUSIVE MODE} ],
	[ 'lock a view over the queue, share row exclusive', q{LOCK TABLE v_pend IN SHARE ROW EXCLUSIVE MODE} ],
	[ 'lock a view over a view', q{LOCK TABLE vv_pend IN EXCLUSIVE MODE} ],
	[ 'lock a view with a subquery over the blocks', q{LOCK TABLE v_sub IN SHARE MODE} ],
	[ 'lock ONLY a view', q{LOCK TABLE ONLY v_blocks IN ACCESS EXCLUSIVE MODE} ],
	[ 'lock a list that has a view in it', q{LOCK TABLE acct, v_pend IN ACCESS EXCLUSIVE MODE} ],
	[ 'lock the status view', q{LOCK TABLE kingres.ledger_status IN ACCESS EXCLUSIVE MODE} ],
	[ 'lock a view in a transaction after a harmless lock', q{LOCK TABLE v_harmless IN ACCESS EXCLUSIVE MODE; LOCK TABLE v_pend IN ROW EXCLUSIVE MODE} ],
	[ 'reindex the blocks in a transaction block', q{REINDEX TABLE kingres.ledger_blocks} ],
	[ 'cluster the blocks in a transaction block', q{CLUSTER kingres.ledger_blocks USING ledger_blocks_pkey} ],
	[ 'select for update through a view', q{SELECT * FROM vv_pend FOR UPDATE} ],
	[ 'a cursor for update through a view', q{DECLARE c CURSOR FOR SELECT * FROM vv_pend FOR UPDATE} ],
	[ 'alter a view', q{ALTER VIEW v_blocks SET (security_barrier = true)} ],
	[ 'alter a view, rename', q{ALTER TABLE v_blocks RENAME TO v_blocks2} ],
);
foreach my $r (@maint)
{
	route_ok('maint', $r->[0], $r->[1]);
}
my @wad = (
	[ 'lock the table (direct)', q{LOCK TABLE kingres.ledger_pending IN ROW EXCLUSIVE MODE} ],
	[ 'lock a view over the blocks', q{LOCK TABLE w_blocks IN ACCESS EXCLUSIVE MODE} ],
	[ 'lock a view over a view', q{LOCK TABLE ww_pend IN SHARE ROW EXCLUSIVE MODE} ],
	[ 'lock a view, row exclusive', q{LOCK TABLE w_pend IN ROW EXCLUSIVE MODE} ],
	[ 'select for update through a view', q{SELECT * FROM ww_pend FOR UPDATE} ],
	[ 'select for share on the table', q{SELECT * FROM kingres.ledger_pending FOR SHARE} ],
	[ 'select for key share through a view', q{SELECT * FROM w_pend FOR KEY SHARE} ],
	[ 'a cursor for update through a view', q{DECLARE c CURSOR FOR SELECT * FROM ww_pend FOR UPDATE} ],
);
foreach my $r (@wad)
{
	route_ok('wad', $r->[0], $r->[1]);
}

#--------------------------------------------------------------------------
# What a member of the owner role could do to take the locks itself: it owns the
# tables, and the statement hook used to ask for the lock of the statement before
# it rejected it (the wait that causes is the next section)
#--------------------------------------------------------------------------
my @inh = (
	[ 'alter table add column', q{ALTER TABLE kingres.ledger_blocks ADD COLUMN x int} ],
	[ 'alter table disable trigger', q{ALTER TABLE kingres.ledger_pending DISABLE TRIGGER ALL} ],
	[ 'drop table', q{DROP TABLE kingres.ledger_blocks} ],
	[ 'drop table cascade', q{DROP TABLE kingres.ledger_pending CASCADE} ],
	[ 'truncate', q{TRUNCATE kingres.ledger_blocks} ],
	[ 'truncate cascade', q{TRUNCATE kingres.ledger_blocks CASCADE} ],
	[ 'create trigger', q{CREATE TRIGGER x AFTER INSERT ON kingres.ledger_pending FOR EACH ROW EXECUTE FUNCTION kingres.ledger_block_modify()} ],
	[ 'create index', q{CREATE INDEX x ON kingres.ledger_pending (queued_at)} ],
	[ 'create rule', q{CREATE RULE x AS ON INSERT TO kingres.ledger_pending DO INSTEAD NOTHING} ],
	[ 'create policy', q{CREATE POLICY x ON kingres.ledger_pending USING (true)} ],
	[ 'rename', q{ALTER TABLE kingres.ledger_pending RENAME TO ledger_pending2} ],
	[ 'set schema', q{ALTER TABLE kingres.ledger_blocks SET SCHEMA public} ],
	[ 'inherit from it', q{CREATE TABLE x () INHERITS (kingres.ledger_blocks)} ],
	[ 'a foreign key to the queue', q{CREATE TABLE x (p bigint REFERENCES kingres.ledger_pending (ledger_txid))} ],
	[ 'a foreign key to the blocks, added', q{CREATE TABLE x (h bigint); ALTER TABLE x ADD FOREIGN KEY (h) REFERENCES kingres.ledger_blocks (height)} ],
	[ 'a foreign key to the blocks, as a column', q{CREATE TABLE x (y int); ALTER TABLE x ADD COLUMN h bigint REFERENCES kingres.ledger_blocks (height)} ],
	[ 'lock the table', q{LOCK TABLE kingres.ledger_blocks IN ACCESS EXCLUSIVE MODE} ],
	[ 'lock a view over it', q{CREATE VIEW vi AS SELECT * FROM kingres.ledger_pending; LOCK TABLE vi IN ACCESS EXCLUSIVE MODE} ],
	[ 'select for update', q{SELECT * FROM kingres.ledger_pending FOR UPDATE} ],
	[ 'reindex', q{REINDEX TABLE kingres.ledger_pending} ],
	[ 'alter the sequence', q{ALTER SEQUENCE kingres.ledger_table_table_id_seq RESTART} ],
	[ 'comment', q{COMMENT ON TABLE kingres.ledger_blocks IS 'x'} ],
	[ 'analyze', q{ANALYZE kingres.ledger_blocks} ],
);
foreach my $r (@inh)
{
	route_ok('inh', $r->[0], $r->[1]);
}
#--------------------------------------------------------------------------
# The wait of a refused statement: a reader (pg_dump, ledger_verify) holds ACCESS
# SHARE on the queue and the blocks; a statement of the owner role that the hook is
# going to refuse used to ask for its own lock first (ACCESS EXCLUSIVE), and every
# writer and the builder queued behind that request until the reader let go
#--------------------------------------------------------------------------
# The reader holds ACCESS SHARE on every table of the catalog for the whole
# section.  Each refused statement is started in a session of its own; the test
# waits (with a bound that only fails it when the condition never comes) until the
# statement has returned, which it can do only by being refused: a statement that
# asks for its lock first stays "active", with a lock request in pg_locks that is
# not granted, behind the reader.  Then the capture insert and the builder run under
# a short lock_timeout.  A control first: a statement that does queue behind the
# reader (a superuser's LOCK ... ACCESS EXCLUSIVE) is seen waiting, so the harness
# can see what it looks for.
my @catalog_tables = split /\n/, $node->safe_psql('d',
	q{SELECT 'kingres.' || relname FROM pg_class
	   WHERE relnamespace = 'kingres'::regnamespace AND relkind = 'r' ORDER BY 1});
is(scalar(@catalog_tables), 12, 'the reader locks the twelve tables of the catalog (nine, and the three of client signatures)');
my $reader = $node->background_psql('d', on_error_stop => 0);
$reader->query_safe('BEGIN; ' . join('', map { "LOCK TABLE $_ IN ACCESS SHARE MODE; " } @catalog_tables));

# Start a statement in a session of its role; returns the session and its pid.
sub start_statement
{
	my ($role, $stmt, $marker) = @_;
	my $s;
	{
		local $ENV{PGUSER} = $role;
		$s = $node->background_psql('d', on_error_stop => 0);
	}
	my $pid = $s->query_safe('SELECT pg_backend_pid()');
	$pid =~ s/\s+//g;
	$s->query_until(qr/started/, "\\echo started\nBEGIN;\n$stmt;\nSELECT '$marker';\n");
	return ($s, $pid);
}

# The state of the statement: 'returned' (the marker query ran after it), or
# 'waiting' with the lock it waits for.
sub wait_for_end_or_lock
{
	my ($pid, $marker) = @_;
	my $state = '';
	for my $i (1 .. 600)
	{
		$state = $node->safe_psql('d', qq{SELECT CASE
			WHEN exists (SELECT 1 FROM pg_locks WHERE pid = $pid AND NOT granted) THEN 'waiting'
			WHEN exists (SELECT 1 FROM pg_stat_activity WHERE pid = $pid AND state LIKE 'idle%'
			                AND query LIKE '%$marker%') THEN 'returned'
			ELSE '' END});
		last if $state ne '';
		select(undef, undef, undef, 0.1);
	}
	return $state;
}

{
	my ($cs, $cpid) = start_statement($super, 'LOCK TABLE kingres.ledger_pending IN ACCESS EXCLUSIVE MODE', 'ctl');
	is(wait_for_end_or_lock($cpid, 'ctl'), 'waiting',
		'control: a statement that does queue behind the reader is seen waiting in pg_locks');
	is($node->safe_psql('d', q{SELECT count(*) FROM pg_stat_activity WHERE state = 'active' AND wait_event_type = 'Lock'}),
		'1', 'control: and in pg_stat_activity');
	my ($ret, $out, $err) = $node->psql('d', q{SET lock_timeout = '1s'; INSERT INTO acct (body) VALUES ('c')});
	like($err, qr/lock timeout/, 'control: and the capture insert is stopped behind it');
	$node->safe_psql('d', qq{SELECT pg_cancel_backend($cpid)});
	$cs->query('SELECT 1');
	$cs->quit;
}

my $nstmt = 0;
sub refused_without_queue
{
	my ($role, $stmt) = @_;
	$nstmt++;
	my $marker = "kl_done_$nstmt";
	my ($s, $pid) = start_statement($role, $stmt, $marker);
	my $state = wait_for_end_or_lock($pid, $marker);
	is($state, 'returned', "$role: '$stmt' returns its refusal and asks for no lock");
	my $work = honest_work();
	is($work, 'works', "$role: and the capture insert and the builder are not stopped by it");
	$s->query('SELECT 1');
	like($s->{stderr}, qr/ERROR/, "$role: the statement was refused");
	$s->quit;
	$node->safe_psql('d', 'DROP TABLE IF EXISTS x, vi CASCADE');
}
foreach my $r (@inh)
{
	# the forms that are an error of another kind, or that run in a block, are not in this list
	next if $r->[1] =~ /^(ANALYZE|COMMENT|REINDEX)/;
	refused_without_queue('inh', $r->[1]);
}
foreach my $r (@maint)
{
	next if $r->[1] =~ /^(REINDEX|CLUSTER|ALTER VIEW|ALTER TABLE v_blocks RENAME|DECLARE)/;
	refused_without_queue('maint', $r->[1]);
}
foreach my $r (@wad)
{
	next if $r->[1] =~ /^DECLARE/;
	refused_without_queue('wad', $r->[1]);
}
$reader->query_safe('ROLLBACK');
$reader->quit;
# The owner of an application table may hold locks on its own table: that stops
# writes to that table, which the lock means, and nothing else.
{
	my ($held, $work) = route('app', q{LOCK TABLE docs IN ACCESS EXCLUSIVE MODE},
		q{ALTER TABLE docs DISABLE TRIGGER ALL});
	is($held, '', 'app: the locks on its own ledger table are not locks on the ledger');
	is($work, 'works', 'app: and the writes of other tables and the builder go on');
}
is($node->safe_psql('d', q{SELECT count(*) FROM pg_trigger WHERE tgrelid = 'docs'::regclass AND tgenabled = 'A'}),
	'3', 'the triggers of docs are still armed');

#--------------------------------------------------------------------------
# L11-03: a role that may call a builder holds the builder lock for its own
# statement and no longer
#--------------------------------------------------------------------------
$node->safe_psql('d', q{INSERT INTO acct (body) VALUES ('p')});
{
	my $s;
	{
		local $ENV{PGUSER} = 'aud';
		$s = $node->background_psql('d', on_error_stop => 0);
	}
	my $pid = $s->query_safe('SELECT pg_backend_pid()');
	$pid =~ s/\s+//g;
	$s->query('BEGIN');
	$s->query('SELECT kingres.ledger_generate_digest()');
	like($s->{stderr}, qr/ledger_generate_digest cannot run inside a transaction block for a role that is not a superuser/,
		'an auditor cannot call the digest function inside a transaction block');
	my $lock = $node->safe_psql('d', qq{SELECT count(*) FROM pg_locks WHERE pid = $pid AND locktype = 'object' AND objsubid = 19276});
	is($lock, '0', 'and it holds no lock of the builders');
	is(honest_work(), 'works', 'so the builder of the server is not stopped');
	$s->query('ROLLBACK');
	$s->query('BEGIN');
	$s->query('SAVEPOINT a');
	$s->query('SELECT kingres.ledger_build_block()');
	like($s->{stderr}, qr/ledger_build_block cannot run inside a transaction block/,
		'nor the builder, after a savepoint');
	$s->query('ROLLBACK');
	# As a statement of its own it works, and the lock is gone when it returns.
	my $res = $s->query('SELECT kingres.ledger_generate_digest() IS NOT NULL');
	like($res, qr/^t$/m, 'as a statement of its own it works');
	$lock = $node->safe_psql('d', qq{SELECT count(*) FROM pg_locks WHERE pid = $pid AND locktype = 'object' AND objsubid = 19276});
	is($lock, '0', 'and holds no lock when it has returned');
	$s->quit;

	# L13-04: the second statement of a pipeline is in an implicit transaction
	# that the first one opened; the flag of the pipeline is what shows it.
	my ($pret, $pout, $perr) = $node->psql('d',
		"\\startpipeline\nSELECT 1 \\bind \\sendpipeline\n"
		  . "SELECT kingres.ledger_build_block() \\bind \\sendpipeline\n\\endpipeline\n",
		extra_params => [ '-U', 'aud' ]);
	like($perr, qr/ledger_build_block cannot run inside a transaction block for a role that is not a superuser/,
		'a builder that is not the first statement of a pipeline is refused');
	is($node->safe_psql('d', q{SELECT count(*) FROM pg_locks WHERE locktype = 'object' AND objsubid = 19276}),
		'0', 'and nothing holds the lock of the builders afterwards');
	($pret, $pout, $perr) = $node->psql('d',
		"\\startpipeline\nSELECT kingres.ledger_build_block() IS NOT NULL \\bind \\sendpipeline\n\\endpipeline\n",
		extra_params => [ '-U', 'aud' ]);
	unlike($perr, qr/cannot run inside a transaction block/,
		'a builder that is the only statement of a pipeline that ends with Sync is served');
}
{
	# A superuser (the worker is one) may, and holds the lock to the end of its
	# transaction; a second builder waits for it and goes on when it ends.
	my $holder = $node->background_psql('d', on_error_stop => 0);
	$node->safe_psql('d', q{INSERT INTO acct (body) VALUES ('q')});
	$holder->query_safe('BEGIN; SELECT kingres.ledger_build_block() IS NOT NULL;');
	my ($ret, $out, $err) = $node->psql('d', q{SET lock_timeout = '1s'; SELECT kingres.ledger_build_block()});
	like($err, qr/lock timeout/, 'a second builder waits for the first, as it must');
	$holder->query_safe('COMMIT');
	$holder->quit;
	$node->safe_psql('d', q{INSERT INTO acct (body) VALUES ('r')});
	is($node->safe_psql('d', q{SELECT kingres.ledger_build_block() IS NOT NULL}), 't', 'and builds when the first has ended');
}
is($node->safe_psql('d', q{SELECT count(*) FROM kingres.ledger_verify()}), '0', 'the ledger verifies');

#--------------------------------------------------------------------------
# What the owner role's member cannot remove (the cascades, run for real)
#--------------------------------------------------------------------------
foreach my $stmt (
	'DROP EXTENSION plpgsql CASCADE',
	'DROP LANGUAGE plpgsql CASCADE',
	'DROP SCHEMA kingres CASCADE',
	"DROP OWNED BY \"$super\"",
	'DROP EXTENSION kingres_ledger CASCADE')
{
	my ($ret, $out, $err) = $node->psql('d', $stmt, extra_params => [ '-U', 'inh' ]);
	like($err, qr/ERROR:.*(ledger|it belongs to|requires it)/, "inh: $stmt is refused");
}
is($node->safe_psql('d', q{SELECT (SELECT count(*) FROM pg_extension WHERE extname = 'kingres_ledger')
	|| ' ' || (SELECT count(*) FROM pg_trigger WHERE tgname = 'kingres_ledger_capture')
	|| ' ' || (SELECT count(*) FROM pg_event_trigger)}), '1 5 3', 'the extension, the capture triggers (two tables of the user and the three of client signatures) and the event triggers are all there');
my ($uret, $uout, $uerr) = $node->psql('d', q{UPDATE docs SET body = 'x'});
like($uerr, qr/append-only/, 'and the table still refuses an update');

#--------------------------------------------------------------------------
# Not covered before: a prepared transaction that holds entries
#--------------------------------------------------------------------------
$node->safe_psql('d', q{
	BEGIN;
	INSERT INTO acct (body) VALUES ('prepared');
	PREPARE TRANSACTION 'p1';
});
$node->safe_psql('d', q{INSERT INTO acct (body) VALUES ('after the prepare')});
is($node->safe_psql('d', q{SELECT kingres.ledger_build_block() IS NOT NULL}), 't',
	'a block is built while a prepared transaction holds entries');
is($node->safe_psql('d', q{SELECT count(*) FROM kingres.ledger_verify()}), '0', 'and the ledger verifies');
is($node->safe_psql('d', q{SELECT count(*) FROM kingres.ledger_pending}), '0',
	'the prepared transaction is not in the queue yet');
$node->safe_psql('d', q{COMMIT PREPARED 'p1'});
is($node->safe_psql('d', q{SELECT count(*) FROM kingres.ledger_pending}), '1', 'it is when it commits');
is($node->safe_psql('d', q{SELECT kingres.ledger_build_block() IS NOT NULL}), 't', 'and the next block takes it');
is($node->safe_psql('d', q{SELECT count(*) FROM kingres.ledger_verify()}), '0', 'the ledger verifies');
$node->safe_psql('d', q{
	BEGIN;
	INSERT INTO acct (body) VALUES ('rolled back');
	PREPARE TRANSACTION 'p2';
});
$node->safe_psql('d', q{ROLLBACK PREPARED 'p2'});
is($node->safe_psql('d', q{SELECT count(*) FROM kingres.ledger_pending}), '0', 'a prepared transaction that is rolled back leaves nothing');
$node->restart;
$node->safe_psql('d', q{
	BEGIN;
	INSERT INTO acct (body) VALUES ('prepared across a restart');
	PREPARE TRANSACTION 'p3';
});
$node->restart;
$node->safe_psql('d', q{COMMIT PREPARED 'p3'});
is($node->safe_psql('d', q{SELECT kingres.ledger_build_block() IS NOT NULL}), 't', 'a transaction prepared before a restart is built after it');
is($node->safe_psql('d', q{SELECT count(*) FROM kingres.ledger_verify()}), '0', 'and the ledger verifies');

#--------------------------------------------------------------------------
# Not covered before: two registrations of the same table at once
#--------------------------------------------------------------------------
$node->safe_psql('d', q{CREATE TABLE twice (id int)});
{
	my $a = $node->background_psql('d', on_error_stop => 0);
	my $b = $node->background_psql('d', on_error_stop => 0);
	# The warning about who can drop the database is true here (L15-01) and is not the subject.
	$a->query_safe(q{SET client_min_messages = error; BEGIN; SELECT kingres.ledger_enable('twice') IS NULL;});
	$b->query_until(qr/started/, "\\echo started\nSET client_min_messages = error;\nSELECT kingres.ledger_enable('twice');\n");
	$a->query_safe('COMMIT');
	$b->query('SELECT 1');
	like($b->{stderr}, qr/already|ledger table|ledger control/, 'the second registration of a table is refused');
	$a->quit;
	$b->quit;
}
is($node->safe_psql('d', q{SELECT count(*) FROM kingres.ledger_table WHERE relid = 'twice'::regclass}), '1',
	'exactly one registration exists');
$node->safe_psql('d', q{INSERT INTO twice VALUES (1)});
is($node->safe_psql('d', q{SELECT count(*) FROM kingres.ledger_verify()}), '0', 'and the ledger verifies');

#--------------------------------------------------------------------------
# Not covered before: ledger_remove_stray against a running builder
#--------------------------------------------------------------------------
{
	my $holder = $node->background_psql('d', on_error_stop => 0);
	$node->safe_psql('d', q{INSERT INTO acct (body) VALUES ('s')});
	$holder->query_safe('BEGIN; SELECT kingres.ledger_build_block() IS NOT NULL;');
	my ($ret, $out, $err) = $node->psql('d', q{SET kingres_ledger.allow_manual_write = on;
		SET lock_timeout = '1s'; SELECT kingres.ledger_remove_stray('test')});
	like($err, qr/lock timeout/, 'ledger_remove_stray waits for a running builder');
	$holder->query_safe('COMMIT');
	$holder->quit;
	is($node->safe_psql('d', q{SET kingres_ledger.allow_manual_write = on; SELECT kingres.ledger_remove_stray('test')}), '0',
		'and runs when it has ended, with nothing to remove');
}
is($node->safe_psql('d', q{SELECT count(*) FROM kingres.ledger_verify()}), '0', 'the ledger verifies');

#--------------------------------------------------------------------------
# Not covered before: the check of a manual write under parallel query
#--------------------------------------------------------------------------
{
	my $hand = q{INSERT INTO kingres.ledger_digest (generated_at, generated_by, height, block_hash, digest)
		SELECT now(), 'hand', height, block_hash, '{}' FROM kingres.ledger_blocks ORDER BY height DESC LIMIT 1};
	my $par = q{SET debug_parallel_query = on; SET parallel_setup_cost = 0; SET parallel_tuple_cost = 0;
		SET min_parallel_table_scan_size = 0; };
	my ($ret, $out, $err) = $node->psql('d', $par . $hand);
	like($err, qr/cannot write to table "ledger_digest"/, 'a write by hand without the setting is refused under parallel query');
	($ret, $out, $err) = $node->psql('d', $par . 'SET kingres_ledger.allow_manual_write = on; ' . $hand);
	is($err, '', 'with the setting a superuser writes, under parallel query too');
	is($node->safe_psql('d', q{SELECT count(*) FROM kingres.ledger_removed WHERE action = 'inserted'}), '1',
		'and the write leaves its record');
	($ret, $out, $err) = $node->psql('d', $par . q{SELECT kingres.ledger_build_block(); SELECT count(*) FROM kingres.ledger_verify()});
	is($err, '', 'the builder and the verifier run under parallel query');
}

done_testing();
