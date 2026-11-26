<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
# kingres_ledger 0.3, part A: independent review of the fixes for the third review of the guard (fourth round)

## 1. Scope and method

An independent reviewer examined the fix round that answers
`2026-11-17-kingres-ledger-0.3-guard-third-review.md` (findings L13-01 to L13-07): the
identifiers that no writer takes from a sequence any more (`write.c`,
`kingres_ledger.c`, `kingres_ledger--0.3.sql` and the two update scripts), the limit on
holding a lock of the extension (`kingres_ledger.lock_hold_limit`, a timer of the
extension's own), the owner of the database in `ledger_status` and in the warning of
`ledger_enable`, the refusal of a REVOKE of PUBLIC's privileges on the catalog tables, the
pipeline half of the 25001 check, the two new TAP files `t/019_ids_and_hold.pl` and
`t/020_not_covered.pl`, and the waits that replaced fixed sleeps in `t/004`, `t/009`,
`t/010`. Everything the earlier reviews found (the five files of the same directory
that precede this one) was treated as a regression check; none of it came back in the
runs below.

Method. The committed tree (not the working tree) of the round and, beside it, the tree
before it were exported. The extension was built out of tree with `kingres/dev/ext.sh`,
warnings as errors, against private copies of the two installations with assertions
(PostgreSQL 18.7 and 19rc2; the installation prefix was fixed in the copy's
`Makefile.global`). The existing suite was run once on each major for a baseline. Then
throwaway clusters (library preloaded; roles: the superuser that owns the extension, a
member of the owner role with INHERIT and no SET, a member of `pg_write_all_data`, the
owner of an application table, a role with EXECUTE on the builders) were attacked by
hand, with `psql` pipeline mode for the protocol cases and `pgbench` for the honest load.
Nine mutants of the round were built by the reviewer (section 8) and run against the
tests that are meant to catch them. The load experiment of section 7 runs the complete
TAP suite beside a CPU burner. Where a result below names one major only, the other was
not run. Every reproduction in section 9 was run on 18.7 and on 19rc2 except L15-09 and
L15-10 (18.7 only).

Machine. 24 cores, otherwise idle apart from other people's work; the load average read
1 to 4 during the reviewer's own runs except where the section on load says so.

## 2. Summary

| Severity | Count | Findings |
|---|---|---|
| critical | 1 | L15-01 |
| high | 1 | L15-02 |
| medium | 4 | L15-03, L15-04, L15-05, L15-06 |
| low | 5 | L15-07, L15-08, L15-09, L15-10, L15-11 |
| note | 3 | L15-12, L15-13, L15-14 |

What held: no row was written to a ledger table and no chain was forged by a role
outside the first invariant; no identifier was ever given twice by two honest writers
(including the sixty-second pgbench of 930,000 transactions with digests, verifications
and the builder running); the sequence attacks of the third review (every `setval` and
`nextval` loop, as a member of `pg_write_all_data` and of the owner role) leave every
writer working on both majors; a pipeline whose client never sends Sync, a statement
that sleeps after the call and an idle transaction are all ended by the limit; the
worker and a superuser are not; an enable of a table of three million rows by a role
that is not a superuser took 75 s, was not ended (the renewal works) and stopped neither
the builder nor the capture.

What did not hold:

1. The README's second invariant says a role that is not a superuser cannot remove the
   ledger "in a database that a superuser owns". A member of the owner role with INHERIT
   and no SET, the role the README names first, drops such a database with `DROP DATABASE`
   from another database, ledger included, because the ownership check of PostgreSQL is
   `has_privs_of_role`. The warning of `ledger_enable` and the columns of `ledger_status`
   test only whether the owner is a superuser (L15-01, critical).
2. The limit is switched off by `PREPARE TRANSACTION`. A role that is not a superuser
   holds the registry's lock and the verifications' lock for ever in a prepared
   transaction: every `ledger_enable` and every `ledger_verify_recorded` of every role,
   a superuser included, then waits until a superuser rolls the prepared transaction
   back. Wherever `max_prepared_transactions` is above zero (every cluster that uses
   two-phase commit) it is open (L15-02, high).
3. `ledger_verify_recorded` swallows the first cancel of a lock wait, so `statement_timeout`
   does not bound it (L15-03), it fails with a duplicate key under REPEATABLE READ and
   SERIALIZABLE (L15-04), and an honest `ledger_generate_digest` fails with a duplicate
   key, or is chosen as a deadlock victim, when a superuser's hand-made digest is open
   (L15-05). All three come from the new `max + 1` allocation and did not exist with
   sequences.
4. Three of the nine mutants survive the whole suite: the timer that is not cleared at
   the end of the transaction, the renewal at every thousand rows, and the warning for
   the owner of the database (L15-06).

## 3. Fate of L13-01 to L13-07

| Finding | Fate | Evidence |
|---|---|---|
| L13-01 sequences moved by `pg_write_all_data` | fixed, with three new defects that come from the replacement | every `setval` (to 1, to the maximum of the type, to the middle) as the member of `pg_write_all_data` on the four sequences, then `ledger_enable`, `ledger_generate_digest`, `ledger_verify_recorded`, `ledger_verify`: all work, the sequences are put right by the writers (18.7 and 19rc2). Defects: L15-03, L15-04, L15-05; id reuse L15-08 |
| L13-02 no bound on holding a lock | fixed for the cases of the review; escapes by `PREPARE TRANSACTION` (L15-02); ends a session that holds no lock after a savepoint is rolled back (L15-07) | pipeline without Sync (the call first, then a silent client): ended at the limit; long statement after the call; idle in transaction; superuser not ended; superuser after `SET ROLE` to an ordinary role ended; both majors |
| L13-03 owner of the database | the columns and the warning exist and are appended at the end of the view; the claim behind them is false for members of the owner role (L15-01); no test of the warning (L15-06) | `ledger_enable` warns for every call in a database owned by a role that is not a superuser; `ledger_status` shows `rep`/`f`; the 0.2 to 0.3 script uses `CREATE OR REPLACE VIEW` with the columns last |
| L13-04 widened exception of the object access hook | the test exists (`t/017`, three statements of a second extension's script); not re-run as a mutant | read; the suite passes |
| L13-05 second mutant (pipeline half) | the author's claim is right: the half is dead code, see section 8 | mutant 9 passes every test; P2 below |
| L13-06 REVOKE from PUBLIC on the catalog tables | closed for table forms; function privileges and default privileges are left open on purpose (L15-10) | 15 forms tried as the member of the owner role, all table forms refused |
| L13-07 name resolved twice | left open and stated; not re-tested | not attacked again |

## 4. Identifiers

How the allocation works: `ledger_enable` takes `max(table_id) + 1` under the registry's
lock (taken in `ledger_lock_table` after the lock of the table), `ledger_register_table`
requires `tid = max + 1` under the same lock, the digest takes
`greatest(max(digest_id), max(digest_id of a removed digest)) + 1` under the builders'
lock, a verification takes `max(verification_id) + 1` under the verifications' lock, the
records of removals and of manual writes take `max(removal_id) + 1` under the removals'
lock; each writer sets the sequence to the id it took with `setval`.

Measured and reproduced:

- Uniqueness under concurrency (honest roles): `pgbench`, 16 clients, 60 s, two inserts
  per transaction into four ledger tables (930,440 transactions, 15,508 per second),
  with a digest every 5 s and a recorded verification every 10 s by an ordinary role:
  no error, no `FATAL`, no deadlock in the server log, 100,017 digests with 100,017
  distinct ids, every ledger entry accounted for (1,860,880 rows, 1,860,884 entries; the
  four extra are earlier rows). That cluster also held 100,000 digests and 100,000
  verifications written by hand by the reviewer for the cost measurement, so its
  `ledger_verify` listed 200,002 rows of `manual_write` and `operator_removal`; the run was
  repeated on a fresh cluster: 674,899 transactions in 60 s (11,248 per second, on a busier machine), 16
  clients, a digest every 5 s and a recorded verification every 10 s by the ordinary role: no
  error, no `FATAL`, no deadlock in the log, 11 distinct digest ids for 11 digests, 6
  verifications all `clean`, 1,349,798 rows and 1,349,798 ledger entries, and
  `ledger_verify()` returned no row at all (18.7).
- Table ids: nothing removes a registry row except a superuser's catalog edit, and a
  registry row cannot be removed while entries refer to it (a foreign key). A
  registration that is rolled back gives its id to the next one; nothing outside the
  transaction saw it (the capture trigger is invisible until commit, the entries too). A
  prepared `ledger_enable` keeps its id and its lock until the prepared transaction ends.
  Four sessions enabling tables in a loop with two sessions enabling in opposite order in
  one transaction produced the expected `deadlock detected` between the two opposite
  sessions (the same as any two DDL transactions locking two tables in opposite order); no
  duplicate id, no mismatch between the id in a capture trigger and the registry (243
  tables, 243 distinct ids).
- Lock order of the four writers and the builder: enable takes table lock, then the
  registry's; register takes the registry's; verify takes the verifications'; remove_stray
  takes the builders', then the removals'; a manual write takes the removals'; the
  builder takes the builders'. No cycle among the honest writers was found. The cycle that
  exists involves a manual write, L15-05.
- Cost: `max(digest_id)` and `max(verification_id)` are index-only backward scans (3
  buffers). The digest id also scans `ledger_removed` for the largest removed digest, a
  sequential scan (no index): 17 ms, parallel, at 200,000 rows (100,000 manual digests
  and as many manual-write records), under the builders' lock; it grows with every
  manual write and every removal (L15-09). A recorded verification with 100,000 rows
  already: 0.02 ms.
- The hostile case: every `setval` loop as `pg_write_all_data` (to 1, to the maximum of
  the type, into the middle), then every writer, on both majors: the writers work (the
  `nextval` burn and the member of the owner role are what `t/019` runs; both pass). The
  manual-write path after a hostile `setval`: after
  `setval` of the digest sequence to the maximum of `bigint`, a superuser's hand insert with
  the default id fails with `nextval: reached maximum value of sequence`; one
  `ledger_generate_digest` later (which sets the sequence to the id it took) the same insert
  works and takes the next id (13). A superuser's own path, healed by the next writer, as
  the README says (18.7).
- Update paths: `t/011` compares installing 0.3, updating from 0.2 and from 0.1 with a
  fingerprint of the catalogs and the full definition of every function; it passes on both
  majors. `pg_upgrade` of the cluster of the experiments from 18.7 to 19rc2 (243 tables,
  the digests, the sequences): the registry, the sequences (243 and 243) and the chain are
  intact, `ledger_enable`, a digest and a recorded verification work afterwards, and the
  only two problems `ledger_verify` reports are the reviewer's own hand-made digest. A
  database created with 0.3 before this round has the old view and the old
  `ledger_enable`; there is no path for it (same version number) and the README does not
  say what to run (L15-13).
- Identifiers that were visible and given again: the id of a recorded verification is
  returned to the caller, and after `BEGIN; SELECT ledger_verify_recorded(); ROLLBACK;` the
  next call returns the same id (L15-08). The digest document, the block and the leaves
  carry no id, so nothing outside the database holds a digest id.

L15-03, L15-04 and L15-05 are in section 9.

## 5. The limit on holding a lock

Implementation read: `kl_hold_arm` registers a `USER_TIMEOUT` and enables it for the
limit when `GetOuterUserId()` is not a superuser; the handler sets `ProcDiePending`,
`InterruptPending` and the latch (it does nothing else, so it is safe at any moment); an
xact callback disables the timer at commit, abort and prepare; `kl_hold_progress` re-arms
it. `statement_timeout`, `lock_timeout`, `idle_in_transaction_session_timeout` and
`transaction_timeout` use their own timeout ids and are independent of it.

Run, with the limit set to 3 s, 18.7 and 19rc2:

| Case | Result |
|---|---|
| ordinary role, builder then `pg_sleep(8)` in one statement | `FATAL: terminating connection due to administrator command` (57P01) at 3.0 s |
| ordinary role, builder as a statement, then a second transaction that sleeps 5 s | not ended |
| superuser, builder then sleep | not ended |
| superuser after `SET ROLE app`, builder then sleep | ended (the outer user counts; so does an ordinary role inside a superuser's `SECURITY DEFINER` function; the README does not say it) |
| `BEGIN; ledger_verify_recorded(); (idle)` | ended while idle, at the limit |
| pipeline: the builder is the first statement, `pg_sleep(6)` the second | ended at the limit |
| pipeline: builder first, `Flush`, then no further message | ended at the limit (the backend is in `ProcessClientRead`; the flag reaches it) |
| pipeline: the builder is the second statement | refused 25001 |
| pipeline: Sync between the statements | the builder runs |
| simple protocol, two statements in one string | refused 25001 |
| `BEGIN; verify; COMMIT;` then 5 s in a new transaction | not ended |
| `BEGIN; SAVEPOINT s; verify; ROLLBACK TO s; pg_sleep(5)` | ended, although the lock went with the savepoint (L15-07) |
| `BEGIN; verify; ledger_enable; PREPARE TRANSACTION` | the locks stay for ever (L15-02) |
| `ledger_enable` of a table of 3,000,000 rows by `app` (75 s, limit 60 s) | not ended, nobody else waited except another `ledger_enable` (it timed out on the registry's lock) |
| a policy with a sleeping function on a table with `FORCE ROW LEVEL SECURITY` to slow the batches | no effect: the extension reads the table with its own rights, 12,000 rows in 0.3 s |

The wrong session: the timer is disabled in the callback of commit, abort and prepare, so
a timer that fires after the end of a transaction cannot happen; the only other window is
a timer that fires between the last interrupt check and the commit, which ends the
session just after a transaction that held the lock for longer than the limit anyway.
Not reproduced: a kill of a session that had held nothing beyond the limit, apart from
L15-07. A pooler hands the backend to another user only after the transaction, when the
timer is off. The apply worker of logical replication never calls a writer of the
extension (the capture takes none of the locks of the extension: the only callers of the
lock function are the five writers and `ledger_lock_table`); run on 18.7: a subscription owned by a role that is not a superuser
(`pg_create_subscription`, password authentication, the owner of the ledger table) with the
limit set to 1 s applied one transaction of 1,500,000 rows into a ledger table, which took
well over a minute; the apply worker was not ended, all 1,500,000 rows and their 1,500,000
ledger entries arrived. `t/020` creates its subscription as the superuser of the test and so
does not cover this case.
Superusers and the workers are not limited.

Evasion: `SET` of the setting is refused to the role; a new period by taking the lock
again is not possible (`kl_hold_arm` arms once per transaction); the thousand-rows rule
renews only when a batch comes back with rows, so a table that feeds rows slowly needs a
heap that makes one thousand rows cost nearly a minute (not reproduced: a sleeping policy
does not slow the batches, a bloated heap would need terabytes). A non-superuser's
`ledger_enable` of an enormous table is bounded only by the size of the table, which is
the same as the stated progress rule.

What an honest role can no longer do: keep a transaction of more than the limit open after
`ledger_enable`, `ledger_verify_recorded` or a builder; call a builder in a transaction
block (refused earlier); run `ledger_generate_digest` for longer than the limit in one
statement (a very large backlog: the session is ended and the work rolls back; the worker,
a superuser, builds it). The README says the first two ("What a control plane must know")
and not the third. It does not say `PREPARE TRANSACTION` (L15-02), `SET ROLE` by a
superuser, or that a kill by the limit looks the same as an administrator's kill, with no
log line that names the limit (L15-12).

## 6. Honest operation

- The load of section 4 ran with the default limit (60 s); no session was ended and no
  deadlock was reported.
- The control plane's calls as a superuser inside transactions: `BEGIN; ledger_generate_digest;
  ledger_verify_recorded; ledger_build_block; COMMIT` in a loop for 40 s beside the other
  writers worked; the only failures were the ones of L15-05 and the opposite-order
  `ledger_enable` deadlock of the test itself.
- `pg_dump`/restore and a standby: only through the suite (`t/002`, `t/016`, `t/009`,
  all pass); no own run.
- `pg_upgrade` 18.7 to 19rc2 with ledger tables: see section 4.

## 7. The suite under load

Method. The complete TAP suite of the extension (20 files, 997 tests, out of tree on
18.7 with assertions, started with `nice`) was run ten times, each run beside a burner of the
reviewer's own: busy loops, one per unit of load wanted, started 20 s before the run and
killed after it. Five runs with 8 burners, five with 16. The burners were stopped after the
last run (checked: none left). Other people's jobs ran on the same machine during the runs
(a suite of another extension, started by someone else, was running throughout), so the
load average read higher than the burner count alone.

| Burners | Run | Load average at start (1, 5, 15 min) | Wall clock | Result |
|---|---|---|---|---|
| 8 | 1 | 4.9 / 2.6 / 2.7 (rising) | 290 s | 997 of 997, no failure |
| 8 | 2 | 9.5 / 7.5 / 4.9 | 290 s | 997 of 997 |
| 8 | 3 | 10.1 / 9.4 / 6.4 | 302 s | 997 of 997 |
| 8 | 4 | 12.3 / 11.3 / 8.1 | 303 s | 997 of 997 |
| 8 | 5 | 11.4 / 11.7 / 9.3 | 287 s | 997 of 997 |
| 16 | 1 | 13.9 / 12.5 / 10.3 | 330 s | 997 of 997 |
| 16 | 2 | 18.1 / 17.4 / 13.2 | 325 s | 997 of 997 |
| 16 | 3 | 19.5 / 18.9 / 15.2 | 392 s | 997 of 997 |
| 16 | 4 | 26.9 / 25.1 / 19.5 | 400 s | 997 of 997 |
| 16 | 5 | 29.1 / 29.6 / 23.7 | 350 s | 997 of 997 |

No failure in any file in ten runs: no test name or log excerpt to report. The idle baseline
is 264 s (18.7). The load the third review saw (6 to 7) is below the lowest here once the
burners were running (the 5 and 15 minute figures of the first run still rose); the suite
passed at 10 to 12 and at 14 to 30 on 24 cores. Verdict: the suite is now safe to run on a
busy machine of this size; what the replaced waits can no longer show (L15-11) is a
property of the assertions, not of the load. What was not tried: a machine with fewer
cores than burners by a larger factor, 19rc2 under load, and memory pressure.

## 8. The tests of this round

Preconditions. `t/019` sets its own limit of 8 s per role, so the file takes about a minute
(a failing wait costs 180 s each: a failing mutant makes the run long). `t/020` needs
password authentication, `wal_level = logical` and the contrib modules `dblink` and
`postgres_fdw`; it runs its subscription as the superuser of the test. `t/019` does not
need prepared transactions, and for that reason does not see L15-02. All run on both
majors in the baseline.

Nine mutants of the reviewer's own, built against 19rc2, each run against the regression
suite and the TAP files `010`, `011`, `017`, `018`, `019`, `020` (the complete suite was
not repeated per mutant):

| Mutant | Result |
|---|---|
| 1 the registry's, verifications' and removals' locks not taken (the "next id without the lock") | killed by `t/019` (two tests, because the lock is seen in `pg_locks`); **no test shows a duplicate id**: nothing runs two writers at once |
| 2 the digest's `max + 1` ignores removed digests | killed by `t/019` (one test) |
| 3 the sequence not set after the insert | killed (`t/018`, `t/019`, four regression tests) |
| 4 the timer not disabled at the end of the transaction | **survives** everything |
| 5 the timer applied to superusers | killed (`t/019`) |
| 6 no renewal at every thousand rows | **survives** everything |
| 7 the owner warning removed (all three scripts) | **survives** everything |
| 8 the refusal limited to the plain table form | killed by the regression test `ledger_stops` only |
| 9 the pipeline half of the 25001 check removed | survives, as the author says: equivalent |

Mutant 4 means: after one call the session is ended a minute later, whatever it does, and
no test notices, because every case ends the session or uses a new one. Mutant 6 means: an
honest `ledger_enable` of a table that takes longer than the limit is ended; the largest
table in the suite is small. Mutant 7: no test reads a warning of `ledger_enable` or the
columns of the view in a database owned by a role that is not a superuser.

The pipeline half (L13-04/05). Read `exec_execute_message` and `start_xact_command` of
18.7: `XACT_FLAGS_PIPELINING` is set when an Execute completes or is suspended without the
transaction ending; the next message of the same transaction calls `start_xact_command`,
which calls `BeginImplicitTransactionBlock`, so `IsTransactionBlock()` is true whenever the
flag is set and a statement runs. Experiments, 18.7 and 19rc2: first statement of a
pipeline the builder (runs, then bounded by the limit); second statement (25001); Sync
between (runs); simple protocol with two statements (25001). The half is dead code. It
costs nothing and guards against a core change; keep it with a comment that says it is
unreachable today, or remove it; do not claim a test for it.

Waits that replaced sleeps. `wait_no_workers`, `wait_for_log` and `poll_query_until` have
the library's default bound of 180 s, so none waits for ever. Two of them lost an
assertion: in `t/009` the `sleep 3` after the insert at `block_interval = 0` is gone, so
"the transaction stays pending" is checked at once and no longer shows that no worker
builds later; the `sleep 3` before "a standby has no launcher and no worker" is gone too
(L15-11, not shown with a mutant). The growing-delay check of `t/010` is a lower bound on
elapsed time, which a slow machine only lengthens, and it still tells five retries in 1.5 s
from five in 4.5 s. `t/017` raised its `lock_timeout` to 60 s: a builder that is queued
now fails the test after a minute instead of three seconds, which is only slower.

## 9. New findings

### L15-01 critical: a member of the superuser's owner role drops a database that holds a ledger

Steps (18.7 and 19rc2). The role `ow` is the README's role: member of the extension
owner with INHERIT TRUE, SET FALSE, not a superuser.

```
-- as the owner (superuser): CREATE DATABASE dbx; in dbx: CREATE EXTENSION kingres_ledger;
--   CREATE TABLE z(i int primary key); SELECT kingres.ledger_enable('z'); INSERT INTO z VALUES (1);
-- datdba of dbx = the superuser
-- as ow, connected to postgres:
DROP DATABASE dbx;        -- DROP DATABASE
```

Observed: the database and its ledger are gone. Expected by the README (second invariant,
exception 6): "in a database that a superuser owns" a role that is not a superuser cannot
remove the ledger; only the owner of a database that is not a superuser can drop it, and
the cure is "every database that holds a ledger is owned by a superuser". The ownership
test of `DROP DATABASE` is `has_privs_of_role(user, owner)`, so every role that inherits
the privileges of the owning superuser role passes it, and that is exactly the role the
second invariant is about. The warning of `ledger_enable` and the columns of
`ledger_status` look at `rolsuper` of `datdba` only, so they say "fine" here.

Place: `kingres_ledger--0.3.sql` (the owner check in `ledger_enable` and the view);
README exception 6 and "What a control plane must know".

Direction: the extension cannot prevent a drop made from another database. Say it in the
README (the exception is "the owner of the database and every role that has the privileges
of the owner"); make the warning and the view report the roles that are not superusers and
have the privileges of the database owner (`pg_has_role(r.oid, d.datdba, 'USAGE')`); tell the
control plane not to grant the owner role of a database with a ledger with INHERIT to
anything, and that an outside digest is the only protection.

### L15-02 high: `PREPARE TRANSACTION` ends the limit and keeps the registry's and the verifications' locks for ever

Steps (both majors), `max_prepared_transactions = 20`, role `app` with EXECUTE on
`ledger_verify_recorded` and `ledger_enable`, owner of the table `pe`:

```
-- as app:
BEGIN; SELECT kingres.ledger_verify_recorded(); SELECT kingres.ledger_enable('pe');
PREPARE TRANSACTION 'hold1';
-- any other role, including a superuser:
SELECT kingres.ledger_verify_recorded();                    -- waits
SET lock_timeout = '3s'; SELECT kingres.ledger_enable('pe2'); -- ERROR: canceling statement due to lock timeout
SELECT locktype, objsubid, mode, granted FROM pg_locks WHERE locktype = 'object' AND objsubid > 0;
--  object | 19278 | ExclusiveLock | t     (verifications)
--  object | 19277 | ExclusiveLock | t     (registry)
```

Observed: the prepared transaction holds both locks until a superuser runs `ROLLBACK
PREPARED`; the timer is disabled at prepare (`XACT_EVENT_PREPARE` in `kl_hold_xact_callback`),
the session is gone, nothing ends it. Expected: the README (exceptions 1 and 2) says every
lock of the extension is held "for no longer than `kingres_ledger.lock_hold_limit`" for a
role that is not a superuser. The builders' lock cannot be held this way (the builders are
refused in a transaction block), so the builder and the capture are not stopped; the
provisioning of tables and the recorded verifications are, for everybody, until a
superuser notices.

Place: `kl_hold_xact_callback` in `write.c`; `kl_lock_extension_object`.

Direction: refuse `PREPARE TRANSACTION` while a non-superuser holds one of the extension's
locks (the transaction callback `XACT_EVENT_PRE_PREPARE` can raise an error when `kl_hold_armed`
is set), or take these locks at session level so that a prepared transaction cannot own them;
and put it in the README next to exception 1.

### L15-03 medium: `ledger_verify_recorded` swallows the first cancel of its lock wait

Steps (both majors): with the prepared transaction of L15-02 open (or any transaction that
holds the verifications' lock):

```
SET statement_timeout = '3s'; SELECT kingres.ledger_verify_recorded();
```

Observed: still waiting after 10 s. The wait for the lock is inside `PG_TRY`; the
`statement_timeout` error is caught, the subtransaction rolled back, the failure recorded,
and the failure path takes the same lock again, where the statement timeout, which has
fired once, does not fire again. One more cancel (`pg_cancel_backend`) ends it. The
verification that was done is lost. Before the round the call did not wait there.
Expected: a cancel ends the call.

Place: `write.c`, `kingres_ledger_verify_recorded`: the `kl_lock_extension_object` calls at
the insert and in the failure path.

Direction: take the verifications' lock before `BeginInternalSubTransaction`, outside the
`PG_TRY`, or rethrow `QUERY_CANCELED` and `LOCK_NOT_AVAILABLE`; and check the lock wait is
cancelable in a test.

### L15-04 medium: `ledger_verify_recorded` fails under REPEATABLE READ and SERIALIZABLE when another one commits

Steps (both majors):

```
-- session 1 (app):  BEGIN ISOLATION LEVEL REPEATABLE READ; SELECT count(*) FROM kingres.ledger_blocks;
--                    SELECT pg_sleep(2);
-- session 2 (app):  SELECT kingres.ledger_verify_recorded();     -- commits id 6
-- session 1:        SELECT kingres.ledger_verify_recorded();
```

Observed: `ERROR: duplicate key value violates unique constraint "ledger_verification_pkey"
DETAIL: Key (verification_id)=(6) already exists`. With `ALTER ROLE app SET
default_transaction_isolation = 'repeatable read'` and four sessions calling it in a loop,
nine of twelve calls failed (the failure path fails the same way). The builders and
`ledger_enable` refuse these levels with a clear message; `ledger_verify_recorded` is
allowed in a transaction block and uses the snapshot of the transaction for `max + 1`.
Expected: the call works or says why not. Before the round the sequence made it work.

Place: `KL_SQL_NEXT_VERIFICATION_ID` in `write.c`.

Direction: require READ COMMITTED like the other writers, or take a fresh snapshot after
the lock (`PushActiveSnapshot(GetLatestSnapshot())`), or read the maximum with a
`SELECT ... FOR UPDATE`-free catalog snapshot.

### L15-05 medium: a superuser's hand-made digest makes an honest `ledger_generate_digest` fail or deadlock

Steps (both majors):

```
-- session 1 (superuser): SET kingres_ledger.allow_manual_write = on; BEGIN;
--    INSERT INTO kingres.ledger_digest (generated_at, generated_by, height, block_hash, digest)
--    SELECT now(), 'm', 0, decode(repeat('ab', 32), 'hex'), '{}' RETURNING digest_id;   -- 100023 (the sequence)
-- session 2 (app):  SELECT kingres.ledger_generate_digest();       -- waits on the uncommitted row
-- session 1:        COMMIT;
```

Observed: session 2 fails with `duplicate key value violates unique constraint
"ledger_digest_pkey" DETAIL: Key (digest_id)=(100023) already exists`. The sequence follows
the last id taken, so the default id of a hand-made row and the `max + 1` of the next
writer are the same number, and the two do not share a lock (a manual insert takes only
the removals' lock). With a superuser who writes a digest by hand and then calls
`ledger_remove_stray` in the same transaction, two ordinary `ledger_generate_digest`
calls and the stray removal formed a cycle (builders' lock, wait for the uncommitted row,
builders' lock) five times in 40 s of the stress run; three of the five deadlock victims
were `ledger_generate_digest` calls (the stress ran it as an ordinary role and, inside a
transaction, as a superuser; the log does not say which), two were `ledger_remove_stray`.
The same holds for the verification id and the removal id (not run separately).

Place: `kl_record_manual_insert` takes `KL_REMOVED_LOCK_SUBID` only; the writers use
`max + 1`; the sequence default.

Direction: take the lock of the table's writer in the manual-insert hook as well (the
builders' lock for `ledger_digest`, the verifications' for `ledger_verification`), or give
hand-made rows an id from a range the writers skip; document that a hand-made row inserted
with the default id races with the writers.

### L15-06 medium: three of the nine mutants survive the whole suite

See section 8: timer not disabled at transaction end (4), no renewal at every thousand
rows (6), owner warning removed (7). Mutant 1 is killed only because the lock is seen in
`pg_locks`; no test starts two writers at once to see a duplicate. Direction: a test that
runs a call, commits, and stays in the session past the limit (kills 4); a test that
enables a table with 3,000 rows under a limit of 2 s whose batches sum to more than the
limit (kills 6; the progress is the point; use a function in a default expression only
if the extension reads it, which it does not); a regression test in a database owned by a
role that is not a superuser that reads the warning and the two view columns (kills 7); and a
concurrency test of two sessions per writer.

### L15-07 low: the timer is not disarmed when a subtransaction aborts

Steps (both majors, limit 3 s): `BEGIN; SAVEPOINT s; SELECT kingres.ledger_verify_recorded();
ROLLBACK TO s; SELECT pg_sleep(5);` as `app`. Observed: `FATAL: terminating connection due
to administrator command` at 3 s, although the lock was released with the savepoint (locks
taken in a subtransaction are released when it aborts). Expected: not ended; the limit is on
holding a lock. Place: `kl_hold_xact_callback` handles only the top-level events; add
`RegisterSubXactCallback` and disarm when the subtransaction that took the lock aborts
(or record the subtransaction id), or document it. Matters for an ORM that wraps each call
in a savepoint.

### L15-08 low: the id of a recorded verification is returned and then given again

Steps: `BEGIN; SELECT kingres.ledger_verify_recorded(); ROLLBACK;` returns 100018 (18.7;
4 on 19rc2); the next call returns the same number. With sequences it never repeated. The
number is in no hash and no digest, but a control plane that stores the returned id
before commit now has a record that names another verification. Direction: say so in the
README, or return the id only with `RETURNING` after commit is not possible; at least
document it.

### L15-09 low: the digest id scans `ledger_removed` without an index

200,000 rows in `ledger_removed` (manual writes and removals): the subquery for the largest
removed digest is a parallel sequential scan, 17 ms, inside the builders' lock; it grows
linearly and every manual write adds a row. Direction: an index on `((row_data ->> 'digest_id')::bigint)`
for `source_table = 'ledger_digest'` (a partial expression index in the install and the update
scripts), or keep the largest removed digest id in the chain row.

### L15-10 low: REVOKE refusal is for tables only, with a misleading hint

The member of the owner role cannot revoke the PUBLIC `SELECT` on any catalog table or view
(plain, `ALL TABLES IN SCHEMA`, column forms, `GRANT OPTION FOR`, `CASCADE`, `GRANTED BY`,
`ALL PRIVILEGES` in a list). Still allowed: `REVOKE EXECUTE ON FUNCTION kingres.ledger_head()
FROM PUBLIC` (and the other publication functions), and `ALTER DEFAULT PRIVILEGES`
(no effect on what exists). The functions are what plain roles use to publish the chain, so the
refusal of the table form buys little, and the README says it on purpose (exception 7). The
hint of the refusal says "Ledger tables are append-only; the ledger can only be switched off
outside the database", which is not the reason. An honest administrator who is a superuser
is not blocked. Direction: refuse the functions as well or say in the README that plain roles
can be cut off from publication by the owner role through the functions; change the hint.

### L15-11 low: two assertions of `t/009` no longer test what they say

`sleep 3` after the insert at `block_interval = 0` and `sleep 3` before the check that a
standby has no workers were replaced by no wait: both are negative statements ("nothing
is built", "no worker exists") that need time to be true. Not shown with a mutant. Direction:
wait for a positive sign that the launcher has had its turn (a log line, a counter in
`ledger_worker_state`) and then check.

### L15-12 note: the kill by the limit cannot be told from an administrator's

The message is the standard one (57P01) and the server log has no line that names the limit.
A log line with the role, the lock and the limit before the session ends would help the
operator and the control plane (it cannot be written in the signal handler; the extension
would have to check its own flag at an interrupt point). Also: a superuser that did `SET ROLE`
to an ordinary role is limited (the outer user is the role), and the README does not say it.

### L15-13 note: no path for a database that was created with 0.3 before this round

The three scripts were changed in place. Such a database keeps the old `ledger_status` and the
old `ledger_enable` (which takes its id from the sequence, so `ledger_register_table` now
refuses the first id that is not `max + 1`). There is no beta yet, so only development
databases are affected; the README should say "drop and create".

### L15-14 note: `kl_hold_progress` renews only for `ledger_enable`

The recorded verification and the builders are never renewed (correct: they do not make
progress under a lock). A very large block build by an ordinary role is ended at the limit
and rolls back; the worker builds it.

## 10. What held under attack

- Every `setval`/`nextval` loop of the third review, as `pg_write_all_data` and as the member of
  the owner role, then every writer, on both majors.
- A pipeline that never sends Sync (builder first, silent client): ended at the limit.
- A superuser and the worker are not limited; `lock_hold_limit` cannot be set by the role it
  limits; `ALTER ROLE ... SET` by an administrator holds.
- `ledger_enable` and the builders refuse REPEATABLE READ and SERIALIZABLE with a clear error.
- The update paths: the three ways to 0.3 give one fingerprint (`t/011`).
- `pg_upgrade` 18.7 to 19rc2 with ledger tables, 243 registered tables and 100,000 digests.
- `REVOKE USAGE ON SCHEMA kingres FROM PUBLIC` and every table-form REVOKE of PUBLIC: refused.
- Apply of logical replication into a ledger table by a role that is not a superuser: not ended.

## 11. Not reproduced

- A session ended by a timer that fired after its transaction ended: not possible by reading
  (the callback disables the timer first); no stress run was made with a limit of a few
  milliseconds.
- A kill of the apply worker of a non-superuser subscription owner: tried (section 5), not
  killed.
- An id of the registry given to two tables: not seen in the stress run; no way found.
- A bypass of the pipeline half of the 25001 check: none, which is why it is dead code.
- A slow-batch evasion of the renewal: a sleeping row-security policy does not slow the
  extension's reads.

## 12. Not covered

- `pg_dump`/restore, a base backup and a standby were run only through the author's suite.
- A pgbench with a builder running while a prepared transaction holds an id.
- The hand-made digest and removal races with `ledger_remove_stray` beyond the stress run.
- Parallel leaders, `idle_in_transaction_session_timeout` and `transaction_timeout` set together
  with the limit.
- The complete suite per mutant (only the files named in section 8).
- 19rc2 for the 3,000,000-row enable, the apply worker, the `pgbench` runs and the load
  experiment (18.7 only).
- The pipeline mutant (9) on 19rc2 was run against the suite only, not against my own
  pipeline cases; the cases ran against the real build.

## 13. Baseline numbers

| Major | Regression tests | TAP files | TAP tests | Time |
|---|---|---|---|---|
| 18.7 (assertions, `-Werror`, out of tree) | 18 of 18 | 20 | 997 | 264 s wall clock |
| 19rc2 (assertions, `-Werror`, out of tree) | 18 of 18 | 20 | 997 | 270 s wall clock |
