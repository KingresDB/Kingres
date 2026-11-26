<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
# Sixth independent review of the guard of kingres_ledger 0.3 (the fixes for L17)

Reviewer: an independent reviewer. Date: 2026-11-18.

## 1. Scope and method

Scope: part A (the guard) of `kingres_ledger` 0.3 after the round that answers the fifth review (L17-01 to
L17-09): `src/write.c` (locks, timer, credit, order check, identifiers), `src/kingres_ledger.c`
(`ledger_lock_table`), `src/capture.c`, `src/protect.c`/`src/guard.c` (read for the first invariant only), the three
SQL scripts, the README, corrections 98 to 100 of section 14 of `ledger-phase7.md`, and the new tests `t/024`,
`t/025`, `t/026`. As asked, most of the effort went into the code that limits how long a role holds a lock.

Method. A read-only export was copied into a private area with one copy of each installation (18.7 and 19rc2) and
built out of tree with `kingres/dev/ext.sh` (clean build, warnings as errors). The whole suite was run on each major.
Then throwaway clusters (library preloaded, `lock_hold_limit` 6 s, `max_prepared_transactions = 10`) were attacked by
hand with the PostgreSQL command-line client and shell loops, using ordinary roles (an owner of application tables with EXECUTE on the writers, a
member of `pg_write_all_data`, `pg_maintain` and `pg_read_all_data`, roles with ADMIN OPTION, CREATEROLE and CREATEDB,
a member of `pg_execute_server_program`). Three one-line mutants were built against 19rc2. Everything was removed at
the end; no server of mine is left.

## 2. What was run

| Run | 18.7 | 19rc2 |
|---|---|---|
| clean build, `-Werror` | no warning | no warning |
| regression tests | 18 of 18 | 18 of 18 |
| TAP suite (`t/001` to `t/026`) | 26 files, 1328 tests, all pass (609 s) | 26 files, 1328 tests, all pass (656 s) |
| install paths 0.3, 0.1 to 0.3, 0.2 to 0.3, 0.1 to 0.2 to 0.3: fingerprint (function definitions, ACL, security, volatility, config, relations with columns, ACL and options, views, indexes, constraints, triggers, event triggers, sequences, extension members, comments; 304 lines) | not repeated by me (the suite's `t/011` ran) | identical in all four |
| hand attacks (sections 4 and 5) | the timer hole (L19-01), single-error control | everything else |
| mutants (section 7) | none | 3, on `t/024` and `t/025` |

The suite is green on both majors; the main findings below are things it does not see.

## 3. Fate of L17-01 to L17-09

| Finding | State | Evidence |
|---|---|---|
| L17-01 renewal by rolled-back work | fixed for the statements of the review; the replacement rule leaves two holes | The three review statements end the session at the limit: DO block with the EXCEPTION loop, savepoint loop with the verifications' lock, savepoint loop with `ledger_enable` alone (`t/024`, pass on both majors). Rules (a) to (c) read right (`write.c:627-668`, frames at `:513-543`). Hand checks on 19rc2: builders' lock then `ledger_enable` of a large table is ended at the limit; a renewal made in a savepoint that is released and then rolled back by the parent is undone (killed at 6.0 s); credit kept by a released savepoint stays (alive). Holes: L19-01 (timer lost after a second error, high), L19-02 (credit is unbounded for the registry's lock, medium). Mutants: rule (b)/re-arm removed: killed (5 failing tests); rule (c) removed: survived one run, killed in the second (L19-03). |
| L17-02 droppers by ADMIN OPTION / CREATEROLE | fixed | Real drops (19rc2): `CREATEROLE` creator of a member of a superuser-less owner role was in `database_potential_droppers`, self-granted and dropped; a member of a group that holds ADMIN OPTION on the owner role (INHERIT and SET on the membership) listed and dropped after the self-grant; a role reached only by `SET ROLE` to a group that inherits the ADMIN OPTION (INHERIT FALSE, SET TRUE on the first link) listed in `potential` and really able (`SET ROLE g2; GRANT own1 TO me; DROP DATABASE` succeeded); a role three links down (SET only, INHERIT chain below) listed in `database_droppers` and dropped via `SET ROLE`. No role that really dropped was missing from the two lists. See L19-05 for the roles that can drop it and are in neither. |
| L17-03 `pg_database_owner` | fixed | Database owned by `pg_database_owner`, role `x_other` owns another database: `database_droppers` = `{x_other}`; connected to that other database, `x_other` ran `DROP DATABASE` on the first one: succeeded. The warning of `ledger_enable` says so. |
| L17-04 lock order not total | partly fixed | `KL006` is raised before the wait for every pair of the review (`write.c:679-705`), in one statement, one transaction and one savepoint; 6 of 6 pairs in `t/025`; mutant with the check off: killed (8 failing tests). Residual deadlock: L19-04. Sequences that are now refused: L19-06. |
| L17-05 log line lists released locks | fixed | Log: `it held the registry's lock ... (3000 ms)` for `SAVEPOINT s1; enable; SAVEPOINT s2; verify; ROLLBACK TO s2; sleep`, and the same wording in my own runs (`it held the verifications' lock`, `... the registry's lock`). |
| L17-06 id reused after a hand delete of a digest | fixed | Three digests deleted by hand in one statement: three `removed` rows with ids 1 to 3, next digest took id 4, ids of the digests 1, 2, 3 not reused; the multi-row delete takes the next removal id per row without a clash. |
| L17-07 top of the type | fixed as documented | Digest and verification with id 9223372036854775807 inserted by a superuser: `no identifier is left after 9223372036854775807` (22003) for `ledger_generate_digest` and `ledger_verify_recorded`, nothing wraps. The digest row can be deleted by hand and the writers recover (id 1, then 2). The verification row cannot be removed (append-only, `ledger_remove_stray` does not take verifications): README says restore or catalog edit. |
| L17-08 cancel inside recorded verification | fixed | `statement_timeout` 2 s, lock free: WARNING `ledger verification 1 failed: canceling statement due to statement timeout`, row with state `failed` and sqlstate 57014 (returned at 2.10 s). Lock held by another session: `ERROR: canceling statement due to statement timeout` at 2.04 s, no row written. `t/026` tests both. I did not build the one-line mutant of `write.c:1084`. |
| L17-09 README claims | partly | The order paragraph and the dropper paragraph are now accurate (but see L19-04, L19-05). Two sentences are still false: README exception 1 and correction 98 say that "an error inside a savepoint does not leave a lock of the parent without a limit" (L19-01); the property sentence is true as written but weaker than it reads (L19-02). |

## 4. The limit: what holds and what does not

The property, restated as I found it in the code:

1. A role that is not a superuser holds a lock of the extension from the moment it is granted until the end of the
   transaction (commit, abort, PREPARE is refused) or until the subtransaction that took it aborts. A timer of the
   backend (`USER_TIMEOUT`) is armed with the first lock and ends the session at `deadline = first lock + lock_hold_limit`.
2. The deadline moves later only inside `ledger_register_table`, after each batch of 1000 rows, by the real time of
   that batch, only while every held lock is the registry's lock, only for rows beyond the highest count credited for
   that relid in this top-level transaction, and the move is undone when the subtransaction that made it aborts.
3. Therefore the deadline is never more than one period after "now", and a lock taken now is held at most one period.
   That is true: credit is `now - batch start`, batches do not overlap, so `deadline <= now + limit` always; the
   builders' lock can never be held with a credit because it comes first in the order and a role may call a builder only
   outside a transaction block. I could not break (3).
4. After an error inside a savepoint the timer is started again. **This is not true** (L19-01).

What a role needs to break the limit, exactly:

- For L19-01: EXECUTE on `ledger_verify_recorded` (or on `ledger_enable` and a table it owns), and a client that sends
  one more statement after an error. No privilege beyond what the control plane grants an agent. The locks it can hold
  without a timer are the verifications' lock and the registry's lock (the builders' lock cannot be held in a
  transaction block by a role that is not a superuser, so the block builder and the capture are not stopped by it).
- For L19-02: the right to create tables in a schema and EXECUTE on `ledger_enable`. It holds the registry's lock for as
  long as it likes, at a cost to itself of a few percent of the time it makes the server spend.

What I tried and could not break (all on 19rc2, limit 6 s unless said): savepoint, release and rollback to in every
nesting I tried (released child under a rolled-back parent, released child under a parent that has its own frame,
parent frame below a rolled-back child), exception blocks, `COMMIT AND CHAIN` (alive for 8 s after the chain),
procedure with `COMMIT` after the lock (alive for 8 s), autocommit call followed by idle time in the same session,
`statement_timeout` and `lock_timeout` firing inside a savepoint while the parent holds the lock (one error: ended at
6.08 s and 6.05 s), `pg_cancel_backend` of oneself (one error), a builder inside a savepoint (refused 25001), the
registry credit followed by the verifications' lock (ended at the deadline, at most one period after the lock),
`SET ROLE` and `SECURITY DEFINER` wrappers (the check uses the outer user), a superuser's lock followed by a
non-superuser's (not limited, as documented). A real large `ledger_enable` (100000 rows, 3.2 s, limit 6 s, and a
second run with the limit lower than the run time): completes, the session is not ended; one batch longer than the
period is ended, which correction 98 states.

An honest large enable: the rule does not end work it should not, except (a) one batch of 1000 rows that takes longer
than the period (documented), (b) the retry of the same table in the same transaction after a rolled-back first try:
the credit for those rows was spent and undone, so the retry is limited to what is left of the first period
(note, L19-07), and (c) a long `ledger_verify_recorded` that follows `ledger_enable` in one transaction: the registry's
lock was taken first, so the whole verification counts (legal by the order; the control plane should not do it). In
each case the client gets only `FATAL: terminating connection due to administrator command`, the same text as a
`pg_terminate_backend`; the server log has the line that names the role, the lock and the limit. There is no hint in
the client message (note, L19-07).

## 5. New findings

### L19-01 high: the timer is lost again by a second error inside an aborted subtransaction, so a role keeps the verifications' lock or the registry's lock for ever

File: `src/write.c:513-543` (`kl_hold_subxact_callback`, `SUBXACT_EVENT_ABORT_SUB`), comment at `:529-540`; README exception 1;
correction 98 ("an error inside a savepoint does not leave a lock of the parent without a limit").

What is wrong. The code starts the timer again in the abort of a subtransaction, because `PostgresMain` calls
`disable_all_timeouts` when an error reaches the main loop, before it aborts the subtransaction. That repairs the first
error. But after the abort the transaction is in the state "aborted subtransaction"; every further statement the client
sends is refused with `current transaction is aborted, commands ignored until end of transaction block`, and that is
an error that reaches the main loop too: `disable_all_timeouts` runs again, and no subtransaction is aborted this time,
so no callback runs and the timer stays off. `ROLLBACK TO s` starts a new subtransaction and calls no callback of the
extension either. The parent's lock is then held with no timer and no end. The test of the round (`t/024`, `1/0` then
`ROLLBACK TO s`) sends exactly one error and the property test has no error step, so neither sees it.

Statements (limit 6 s, role `app` with EXECUTE on the functions, database with a small ledger; 19rc2 and 18.7 alike):
```
BEGIN;
SELECT kingres.ledger_verify_recorded() > 0;   -- t
SAVEPOINT s;
SELECT 1/0;                                    -- ERROR: division by zero (timer started again)
SELECT 1;                                      -- ERROR: current transaction is aborted ... (timer disabled again)
ROLLBACK TO s;
SELECT pg_sleep(40);
```
19rc2: the sleep returned after 40.06 s and the next statement ran. At 20 s `pg_locks` held 1 lock of the extension
(objsubid 19278, granted) and another session's `SET lock_timeout='3s'; SELECT kingres.ledger_verify_recorded()` failed
with `canceling statement due to lock timeout / waiting for ExclusiveLock on object ... of class 3079`. The same with
`ledger_enable('q9')` instead of the verification (registry's lock): alive after 12 s. 18.7: the sleep(20) returned
after 20.03 s, 1 lock held at 12 s; the control with a single error (`1/0` then `ROLLBACK TO s`) was ended at 6.02 s,
as the round intends. Idle variant: the same two errors and then no more statements leave the session idle in the
aborted transaction holding the lock, with the timer off.

What it stops: every `ledger_verify_recorded` (the control plane's recorded verification) and every superuser write by
hand (all four locks) for the verifications' lock; every `ledger_enable` of every role, `ledger_remove_stray` and
every write by hand for the registry's lock. It does not stop the block builder or the capture: the builders' lock
cannot be reached this way (a builder is refused in a transaction block, and an error inside a function that catches it
never reaches the main loop). It is still the limit that the README states for every lock, broken by a role with the
smallest grants, and it is the third round in a row in which the code that limits the hold time is the finding.

Side effect (note): after such a session finally runs `ROLLBACK`, the abort of the restarted subtransaction starts the
timer with a deadline that is long past; the handler fires at once and the session is ended after the lock has gone
(seen: the first session ended at the end of its 40 s sleep, with the log line naming the verifications' lock). A session
that holds nothing is ended; it only happens after the limit was already exceeded.

What a fix must achieve: do not depend on the timer surviving errors. Re-arm it whenever a statement starts or a
subtransaction is started or rolled back to while a limited lock is held (for example in the `ExecutorStart` and
`ProcessUtility` hooks the extension already has, and in the start of a subtransaction), and also check the deadline
there (if it has passed, end the session at that point). Add tests: two and three errors in a row inside a savepoint,
an error in a nested savepoint, a protocol-level error (Parse) followed by Sync and another statement, then
`ROLLBACK TO`, then a sleep, for the verifications' lock and the registry's lock; and an error step in the property
test.

### L19-02 medium: the credit of `ledger_enable` has no bound, so a role holds the registry's lock for ever at a small cost to itself

File: `src/write.c:627-668` (`kl_hold_credit`), `:1742`; README exception 1 ("for as long as the entering takes"),
correction 98.

What is wrong. The rules make the credit equal to real server time, one table at a time, and never more than the time
actually spent. They do not bound the total. The server's time per row is much more than the role's cost to make the
rows, and each new table is credited again (the count is per relid, and a ledger table cannot be dropped, so the role
makes a new table each time).

Statement (limit 6 s, role `app` owns nothing at the start, may create tables in `public`; 19rc2):
```
DO $$ DECLARE i int; BEGIN FOR i IN 1..14 LOOP
  EXECUTE format('CREATE TABLE public.x%s AS SELECT g id, repeat(''x'',20) b FROM generate_series(1,100000) g', i);
  PERFORM kingres.ledger_enable(format('public.x%s', i)::regclass);
END LOOP; END $$;
```
Result: `done total 00:01:15.3 ; own creation cost 00:00:01.88`: the session held the registry's lock for 75 s with a
limit of 6 s (12.5 times), of which 1.9 s were the role's own work (a leverage of 40 to 1), and the loop can be 14 or
14000 rounds. A superuser's `SET lock_timeout='2s'; SELECT kingres.ledger_enable('p1')` in another session at 14 s:
`canceling statement due to lock timeout / waiting for ExclusiveLock on object ... (waiting at ledger_lock_table)`. A
`ledger_build_block` in the same window returned at once (the builder is not stopped, as the README says).

Other ways to spend server time for little of the role's own, each credited because the batch is real work: rows of
1000 values that are large and compress well (each batch is hashed in full), a table with a huge number of dead tuples
between its live rows (the scan of the batch reads them). I did not run those two.

What it stops: other roles' `ledger_enable` (onboarding of tables), `ledger_remove_stray` (the operator's recovery
act) and every write by hand, for as long as the role wants; every table the role enters is a permanent registry row
and a permanent block of entries. It does not stop the builder or the capture, so the second invariant as the README
words it (builder and capture) is not broken, but the claim "no lock longer than the limit plus the real work" is only
as strong as the role's patience.

What a fix must achieve: a bound on the total credit of one transaction that does not scale with what the role
chooses to create: for example a second superuser setting (a cap on the credited time, or on the rows credited, per
transaction), or credit only for tables that existed before the transaction began (a role cannot create them cheaply in
the same transaction), or do not hold the registry's lock while rows are entered (take it for the registration and the
id only). Whatever is chosen, say it in the README and test the loop above.

### L19-03 low: the test for rule (c) can pass with rule (c) removed; the new timing tests depend on the speed of the machine in both directions

File: `t/024_hold_renewals.pl` (the rule (c) block, the precondition), `write.c:641`.

Mutant: the line `if (slot != NULL && rows <= slot->rows) return;` made inert (`if (0 && ...)`), built clean against 19rc2.
`t/024`, first run: 66 of 66 pass (the mutant survives). Second run of the same mutant: tests 15, 16 and 19 fail (killed).
The test enters a table in a savepoint, rolls back, enters it again, with a limit of 1.4 D where D is a measurement of
another table by a superuser (2.5 to 2.8 s here). It expects the session to end. With rule (c) removed the session
ends anyway whenever the first entering took more than 1.4 D, because then the deadline restored by the rollback has
already passed; the test only separates rule (c) when the first entering is between 1.0 D and 1.4 D, which depends on
the load of a shared machine. The mutant of rule (b) (no re-arm) is killed reliably (7 failing tests), and the mutant
of the order check is killed reliably (`t/025`).

Other points of the file: `cmp_ok($D, '>', 600)` is a failure, not a skip, so a machine that enters 100000 rows in
less than 0.6 s fails the whole file; the property test (12 seeds) has no error step (the hole of L19-01); the checks
use margins of 300 to 500 ms against a clock read over a pipe, which fail in the safe direction on a slow machine.
A fix: make the first entering of rule (c) not depend on a factor of a measurement, for example by measuring the
first entering inside the test transaction and setting the limit from that (the limit is a per-role setting that can
change between the statements), and make the precondition scale the table instead of failing.

### L19-04 low: `ledger_enable` waits for the table lock before the order check, so a caller that is out of order can still deadlock an honest session

File: `src/kingres_ledger.c:225-241` (`ledger_lock_table`: `LockRelationOid` at `:230`, the order check inside
`kl_lock_extension_object` at `:241`); README "The order of the locks" ("what cannot be ordered this way ...
a transaction that holds the registry's lock ... then takes a lock on a table").

What is wrong. The README names the case of a transaction that holds the registry's lock. The same happens with the
verifications' lock (or the builders', inside a statement): `ledger_enable` first waits for the lock on the table, and
only then does the order check refuse the call. While it waits it holds the later lock, so the other session, which
holds the table lock and wants that later lock, deadlocks with it, and the detector aborts one of the two after
`deadlock_timeout`, either of them.

Statements (19rc2, role `app` owns `q8`, small ledger):
- session A: `BEGIN; SELECT kingres.ledger_verify_recorded(); SELECT pg_sleep(1); SELECT kingres.ledger_enable('q8');`
- session B: `BEGIN; LOCK TABLE q8 IN SHARE ROW EXCLUSIVE MODE; SELECT pg_sleep(0.5); SELECT kingres.ledger_verify_recorded();`

Result: B got `ERROR: deadlock detected ... Process waits for ExclusiveLock on object ... of class 3079 ... Process waits
for ShareRowExclusiveLock on relation ...`; A got the KL006 error right after. Both sessions are the same owner, so a
role can only do this to itself or to a role that owns the same table; low.

What a fix must achieve: call the order check for the registry's lock (without taking the lock) before
`LockRelationOid` in `ledger_lock_table`, so the call is refused before it waits; and say in the README that the table
lock is the only one that cannot be ordered after a registry lock is held.

### L19-05 medium: roles that can drop the database or damage its files are in neither list and the README does not name them

File: `kingres_ledger--0.3.sql:1468-1480` and the view (same rule), README "What a role that is not a superuser can
stop or remove" and exception 6 ("a superuser role that has no members ... is what protects a deployment").

What is wrong. The two lists are computed from the privileges of the database owner and from ADMIN OPTION. A member of
`pg_execute_server_program` is outside both and can drop the database, and so can a member of `pg_write_server_files`
write files of the cluster. The README invariant ("cannot remove the ledger ... with any statement that runs in the
database that holds it") and the recipe ("the database is owned by a superuser role without members ... and no role
that is not a superuser may SET ROLE to a superuser") do not mention these built-in roles.

Statements (19rc2): database owned by a superuser, no members: `database_droppers` = `{}`,
`database_potential_droppers` = `{}`, `database_owner_is_superuser` = true. Role `prog` with
`GRANT pg_execute_server_program TO prog`:
```
COPY (SELECT 1) TO PROGRAM '(sleep 1; <client> -d postgres -c "DROP DATABASE dbz") >/dev/null 2>&1 &';
```
Result: the database was gone four seconds later (`count(*)` of `pg_database` for it: 0). (A drop run directly from that
session waits for itself on the barrier of the drop: it has to be detached, as above.)

What a fix must achieve: list the members of `pg_execute_server_program` and `pg_write_server_files` (directly or by a
chain, as the other lists do) in a third list or in the warning of `ledger_enable`, and name them in README exception 6
as roles that are superusers for this purpose.

### L19-06 note: sequences that a control plane may use are refused by the order check, and the README states only one direction

File: `write.c:679-705`, README "The order of the locks".

Statements (19rc2, superuser, one transaction block each), each answered with `KL006`:
`SELECT ledger_enable('t'); SELECT ledger_generate_digest();` (builder after registry),
`SELECT ledger_enable('t');` then a hand write into `ledger_removed` (a hand write takes the builders' lock first),
`SELECT ledger_verify_recorded(); SELECT ledger_remove_stray('r');`, `SELECT ledger_verify_recorded(); SELECT ledger_build_block();`.
In one statement for a role that is not a superuser: `SELECT ledger_generate_digest(), ledger_enable('q6')` is allowed
(order), `SELECT ledger_enable('q7'), ledger_generate_digest()` is refused. The README says "a builder and then
`ledger_enable` is legal"; it does not say in the same breath that the reverse, which is the natural order for a script
that enables a table and then pins a digest in one transaction, is refused, and the hint says "in separate
transactions", which is right. Not a defect of the code; a control plane has to be told. The tests' own sequences and
the single-call-per-transaction use of the extension are not affected (the whole suite passes).

### L19-07 note: what an operator sees when the limit ends an honest session

The client gets the text of a `pg_terminate_backend`; only the server log says why. The retry of a table after a
rolled-back first try (rule (c): no credit twice) is limited to what is left of the first period, which for a large
table is not enough. A hint in the log line ("a large `ledger_enable` ... run it in a transaction of its own, raise the
limit for the role with `ALTER ROLE ... SET`") costs nothing. Also: the search for a table's slot in `kl_credited`
(`write.c:639-640`) is linear and does not stop at the first match, so a role that enters tens of thousands of tables in one
transaction pays quadratic time in the extension (low).

## 6. What held under attack

- First invariant, 19rc2, with my own roles (a member of `pg_write_all_data`, `pg_maintain` and `pg_read_all_data`; the
  owner of application tables with EXECUTE on the writers): `INSERT`, `UPDATE`, `DELETE`, `TRUNCATE`, `COPY FROM`,
  `LOCK TABLE ... ROW EXCLUSIVE`, `ALTER TABLE ... DISABLE TRIGGER`, `DROP TABLE`, `CREATE OR REPLACE FUNCTION` of a ledger
  function, `DISABLE TRIGGER` and `DROP TABLE` of an application ledger table, `UPDATE`/`DELETE`/`TRUNCATE` of one: all
  refused by the extension or by privilege; counts of the catalog tables unchanged. `setval` on an identity sequence
  by the member of `pg_write_all_data` still works (documented, harmless: the writers do not read the sequences).
- The order check: no way found to hold a lock without it being listed, or to list one that is not held, across savepoints,
  exception blocks, errors with and without a savepoint, `COMMIT AND CHAIN` and a lock wait that fails; a lock that is
  held is asked for again without refusal; `ledger_remove_stray`, a hand write and the worker take their locks in the order;
  `t/023` (2750 rounds) and `t/025` pass.
- Identifiers at the edges, the cancel inside the recorded verification, and PREPARE (refused after a lock): as in
  section 3.
- The three install paths give one catalog on 19rc2 (304 lines identical, four paths).
- The timer is not lost by `statement_timeout`, `lock_timeout` or a cancel inside a savepoint when it is the only error;
  it is not left on after commit, rollback, `COMMIT AND CHAIN` or a procedure's `COMMIT` (a session that holds nothing
  stayed alive for 8 s in each).

## 7. Mutants of the round (19rc2, one line each, built clean)

| # | Mutant | Test run | Result |
|---|---|---|---|
| 1 | rule (c) off (`write.c:641`) | `t/024`, twice | survived once, killed once (L19-03) |
| 2 | no re-arm of the timer in the abort of a subtransaction (`write.c:541`) | `t/024` | killed (tests 8, 9, 11, 12, 31, 34, 35) |
| 3 | order check off (`write.c:693`) | `t/025` | killed (pairs 1 to 3, the error tests) |

Not built: the mutants of rules (a) and (b) alone, the mutant of the cancel path (`write.c:1084`), and the mutants of
the PREPARE refusal; the author reports them killed and I read the tests, not the results.

## 8. Verdict

Part A cannot be called closed. The three statements of the fifth review are answered, the renewal is now tied to the
registry's lock, to real batches and to the savepoint stack, the lists of droppers match real drops in every case I
ran, the order check refuses before it waits, and the suite is green on 18.7 and 19rc2 (1328 tests each). But the timer
that the whole limit stands on is lost by a second error in an aborted subtransaction, with statements that need no
privilege beyond EXECUTE on `ledger_verify_recorded` or `ledger_enable`, and the role then holds the verifications' lock
or the registry's lock for ever (L19-01, high). The builder and the capture are not stopped by it, but the property the
README, the comments and correction 98 state is false. L19-02 (credit with no bound, medium) and L19-05 (roles with
server file or program access, medium) need a fix or a README correction before the beta; L19-03 to L19-07 are small.
Nothing found lets a role that is not a superuser write a ledger table or forge a block.

## 9. Not checked

- The mutants listed as not built in section 7, and the one-line mutant of the cancel path (`write.c:1084`).
- The two other ways of spending server time cheaply for L19-02 (compressible large values, dead tuples between live rows).
- The tick of the timer against a commit that takes long (a false end of a session that has just committed): by reading
  only. The only false end I could produce is the one after the limit has passed (L19-01, side effect).
- The extended protocol (Parse, Bind, Execute without Sync) for the hole of L19-01: the simple protocol shows it; the
  protocol-level error is expected to behave the same, not run.
- Another extension's use of timeouts, a parallel worker, a background worker started by a role, `dblink` and
  `postgres_fdw` loopback sessions, a subscription owned by a role that is not a superuser.
- `lock_hold_limit` at values other than 6 s and 3 s, and `0`.
- The dropper lists on 18.7 (run on 19rc2 only), and the install fingerprint on 18.7 (the suite's `t/011` and `t/016`
  ran there).
- A restore of a dump that contains a ledger with the new order check beyond `t/016`; `pg_upgrade`; a standby; load.
