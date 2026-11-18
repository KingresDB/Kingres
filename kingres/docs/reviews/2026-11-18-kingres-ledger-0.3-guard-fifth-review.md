<!-- SPDX-License-Identifier: AGPL-3.0-only -->
# Fifth independent review of the guard of kingres_ledger 0.3 (the fixes for L15)

Reviewer: an independent reviewer. Date: 2026-11-18.

## 1. Scope and method

Scope: part A (the guard) of `kingres_ledger` 0.3 after the round that answers
`2026-11-17-kingres-ledger-0.3-guard-fourth-review.md` (L15-01 to L15-14): `src/write.c`, `src/protect.c`,
the three SQL scripts, the README, corrections 49 to 54, 66 to 68 and 74 to 80 of section 14 of
`ledger-phase7.md`, and the new and changed tests (`t/019` to `t/023`, `t/009`, `t/010`).

Method. A read-only export was copied into a private area with one copy of each installation (18.7 and 19rc2,
with assertions). The extension was built out of tree with `kingres/dev/ext.sh` (clean build, warnings as
errors) and the whole suite was run on each major. Then throwaway clusters (library preloaded,
`lock_hold_limit` 6 s, `max_prepared_transactions = 10`) were attacked by hand with `psql` and shell loops.
Roles: a member of the owner role (INHERIT, no SET), a member of `pg_write_all_data` and `pg_maintain`, owners
of application tables with EXECUTE on the writers, roles with ADMIN OPTION, CREATEROLE and CREATEDB, a tablespace
owner; databases owned by a non-superuser, by a superuser role and by `pg_database_owner`. Nine mutants of
the round were built against 19rc2 (section 7). Everything was removed at the end.

## 2. What was run

| Run | 18.7 | 19rc2 |
|---|---|---|
| clean build, `-Werror` | no warning | no warning |
| regression tests | 18 of 18 | 18 of 18 |
| TAP suite (`t/001` to `t/023`) | 23 files, 1138 tests, all pass (374 s) | 23 files, 1138 tests, all pass (350 s) |
| installs 0.3, 0.1 to 0.3, 0.2 to 0.3, 0.1 to 0.2 to 0.3: fingerprint of functions (definition hash, ACL, security, volatility, config), relations (columns, ACL, options), views, indexes, constraints, triggers, event triggers, sequences, extension members | identical, all four (197 lines) | identical, all four |
| hand attacks (sections 4 and 5) | droppers, ADMIN OPTION, `pg_database_owner`, tablespace owner, PREPARE, subtransactions, renewal, deadlocks, 50 DDL forms, ids, first invariant | droppers (ADMIN OPTION), PREPARE, subtransactions, renewal, deadlocks, ids at three isolation levels, wrap, first invariant |
| mutants | 9, on 19rc2 only | see section 7 |

The suite is green on both majors; the main findings below are things it does not see.

## 3. Fate of L15-01 to L15-14

| Finding | State | Evidence |
|---|---|---|
| L15-01 drop of the database by a member of the owner | partly fixed | README exception 6 now tells the truth; the view (`kingres_ledger--0.3.sql:2289-2295`) and the warning of `ledger_enable` (`:1443-1456`, same rule) list INHERIT and SET chains correctly: owner `nsown` with INHERIT member, SET-only member, INHERIT chain, SET chain, broken chain, superuser owner with an INHERIT member: listed set equals the set that really dropped it (statement `DROP DATABASE`, also after `SET ROLE`), 19rc2. Gaps: ADMIN OPTION (L17-02), `pg_database_owner` (L17-03). A mutant that removes the warning is killed (`t/021`). |
| L15-02 PREPARE keeps locks | fixed | `write.c:316-325` (`XACT_EVENT_PRE_PREPARE`); refused with 0A000 after verify, after a released savepoint, after a PL/pgSQL block that completed, after `ledger_enable` (18 and 19). Mutant 1 killed by `t/022`. Superuser may prepare and `lock_hold_limit = 0` tracks nothing: documented, accepted. |
| L15-03 cancel swallowed | fixed | lock taken outside `PG_TRY` (`write.c:1036`); `statement_timeout` 2 s on a wait for a held verifications' lock: `canceling statement due to statement timeout` at 2.004 s. The cancel-inside-the-run path (`write.c:1084`) is untested: mutant 5 survives (L17-08). |
| L15-04 RR / SERIALIZABLE duplicate key | fixed | `kl_next_id` takes `GetLatestSnapshot()` (`write.c:562-571`); 4 sessions x 5 calls at each of the three levels, after a snapshot older than another commit: 0 errors, 60 of 60 distinct ids. Mutant 4 killed (`t/023`). |
| L15-05 hand-made digest vs honest writer | fixed for the case of the review | superuser inserts a digest by hand (takes all four locks, `write.c:589`), honest `ledger_generate_digest` waits and then takes the next id (3 hand, 4 honest). Mutant 6 killed. A related deadlock class remains (L17-04). |
| L15-06 three surviving mutants | fixed | timer not disarmed at transaction end: killed (8 tests of `t/022`); no renewal per thousand rows: killed (2 tests); owner warning off in all three scripts: killed (`t/021`, 3 tests). Concurrency of two writers is now run by `t/023`. But the renewal itself is the defect L17-01. |
| L15-07 timer after rollback of savepoint | fixed | per-subtransaction entries (`write.c:339-380`); `BEGIN; SAVEPOINT s; verify; ROLLBACK TO s; pg_sleep(9)`: survives; nested, released and exception-block variants behave; mutants 2 and 3 killed. |
| L15-08 id handed out and given again | documented | README "Identifiers" and `write.c:536-556` say so; behaviour unchanged, by design. |
| L15-09 digest id scan | fixed | `ledger_removed_digest_idx` is in the three scripts (fingerprints equal); with 41,000 removed rows the plan is `Index Scan Backward using ledger_removed_digest_idx` (min/max). `t/023:337` checks it. |
| L15-10 REVOKE on functions | fixed | 12 forms as the member of the owner role (plain, `ROUTINE`, `GRANTED BY`, `ALL ROUTINES IN SCHEMA`, with another role in the list, `GRANT OPTION FOR`, `CASCADE`/`RESTRICT`, by the grantee of a `GRANT ... WITH GRANT OPTION`): all refused with `KL002` and the new hint. `ALTER DEFAULT PRIVILEGES` stays allowed and harmless (affects only future objects). |
| L15-11 weakened assertions of `t/009` | fixed, small residue | positive events first (log line of the launcher, "ready to accept read-only connections"), then the negative checks. The negative checks are right after those events; sound by the order of events, not by elapsed time. |
| L15-12 kill not named | fixed, one flaw | log line `ended the session of role "app": it held the ... for longer than ...` is written (seen). The lock list is not shrunk when a savepoint releases a lock (L17-05). |
| L15-13 pre-round databases | answered | README 229-243 says drop and create. I had no pre-round script, so I did not run a pre-round database (section 8). |
| L15-14 dead pipeline half, no renewal for builders | answered | comment at `write.c:172-179`; README says no test claims it. |

## 4. New findings

### L17-01 high: the limit on holding a lock is renewed by work that is rolled back, so a role holds the builders' lock (and the others) for ever

File: `src/write.c:444-448` (`kl_hold_progress`), called from `write.c:1445` (batch loop of `ledger_register_table`); README.md (exceptions 1 and 2: "work that makes progress ... gets a new period").

What is wrong. The renewal re-arms the one timer of the transaction whenever a batch of one row or more comes back from the table that `ledger_enable` is entering. Nothing ties the renewal to the lock that is held or to work that stays: when the subtransaction that did it is rolled back, the renewal stays. Roles that hold EXECUTE on `ledger_enable` and on a builder (or on `ledger_verify_recorded`) and own one table with a few rows can keep any lock for as long as they like.

Statements (19rc2 and 18.7, `lock_hold_limit` 6 s, role `app` owns table `big` with 3000 rows, EXECUTE on `ledger_generate_digest`, `ledger_enable`, `ledger_verify_recorded`):

```
-- as app, one statement, builders' lock:
DO $$ DECLARE i int; BEGIN
  PERFORM kingres.ledger_generate_digest();
  FOR i IN 1..8 LOOP
    PERFORM pg_sleep(4);
    BEGIN PERFORM kingres.ledger_enable('big'); RAISE EXCEPTION 'undo';
    EXCEPTION WHEN OTHERS THEN NULL; END;
  END LOOP;
  RAISE NOTICE 'still alive at %', clock_timestamp(); END $$;
```
Result: NOTICE after 32 s, `DO`, no session ended (0 log lines of the limit). In another session, 14 s into it, a superuser's `SET lock_timeout='3s'; SELECT kingres.ledger_build_block()` returned `ERROR: canceling statement due to lock timeout / waiting for ExclusiveLock on object ... of class 3079`. So the block builder (the worker is the same caller) is stopped for as long as the role loops: the second invariant is broken. Variant without a DO block, verifications' lock: `BEGIN; SELECT kingres.ledger_verify_recorded(); SELECT pg_sleep(4);` then four times `SAVEPOINT s; SELECT kingres.ledger_enable('big'); ROLLBACK TO s; SELECT pg_sleep(4)`: still holding the lock after 20 s (count of granted object locks = 1). A role with `ledger_enable` alone keeps the registry's lock the same way (enable one table at top level, then loop).

What a fix must achieve: a renewal must not outlive the work it stands for and must not extend a lock other than the one that work holds. For instance: renew only while the registry's lock is the only lock the transaction holds; remember the deadline per entry and restore it when the subtransaction that renewed aborts; or cap the total renewal per transaction. Add a test that renews inside a rolled-back savepoint and inside an EXCEPTION block and expects the session to be ended at the limit.

### L17-02 medium: `database_droppers` omits roles that can make themselves droppers (ADMIN OPTION, CREATEROLE)

File: `kingres_ledger--0.3.sql:2289-2295` (view), `:1443-1456` (warning, same rule, also in the two update scripts); README exception 6 ("computed as PostgreSQL does", "truthfully and as of now").

What is wrong. The rule looks at memberships that exist. A role with ADMIN OPTION on a role that has the privileges of the owner can `GRANT` it to itself with INHERIT and SET and then drop the database. Since PostgreSQL 16 a role with CREATEROLE gets ADMIN on every role it creates, so this is the normal state of a deployment that lets a control plane create roles.

Statements (18.7: whole sequence; 19rc2: the listing and the self-grant):
```
CREATE ROLE nsown LOGIN; CREATE ROLE m_adm LOGIN;
CREATE DATABASE dbt OWNER nsown;   -- extension and a ledger table inside
GRANT nsown TO m_adm WITH ADMIN TRUE, INHERIT FALSE, SET FALSE;
SELECT database_droppers FROM kingres.ledger_status;   -- {nsown}
-- as m_adm:  GRANT nsown TO m_adm WITH INHERIT TRUE, SET TRUE;   DROP DATABASE dbt;   -- succeeded
```
19rc2, superuser role `own` as owner: `crr` (CREATEROLE) created `kid2`, a superuser granted `own` to `kid2` WITH INHERIT. `database_droppers` = `{kid2}`; after `crr` ran `GRANT kid2 TO crr WITH INHERIT TRUE, SET TRUE` it was `{crr,kid2}`. Only roles that can be granted without the SUPERUSER attribute are affected (`GRANT own TO adm` for a superuser `own` is refused: "Only roles with the SUPERUSER attribute may grant roles with the SUPERUSER attribute"), so a superuser owner without non-superuser members and no CREATEROLE role administering a member stays safe.

What a fix must achieve: list (or flag in a separate column) every role that has ADMIN OPTION, directly or through a chain, on a role that qualifies, and say it in the README; `t/021` must try an ADMIN OPTION holder and a CREATEROLE creator by real drops, not only compare lists.

### L17-03 low: a database owned by `pg_database_owner` is droppable by the owner of the database the dropper is connected to, and is not listed

Files as L17-02. The membership of `pg_database_owner` is implicit and relative to the database the session is connected to; the view evaluates it in the ledger's database.

Statements (18.7): `CREATE DATABASE ddo OWNER oo; CREATE DATABASE dbx OWNER pg_database_owner;` extension in `dbx`. `database_droppers` = `{pg_database_owner}` (a role nobody logs in as). As `oo` connected to `ddo`: `DROP DATABASE dbx` succeeded. Setting that owner needs a superuser, so low. A fix: report the case explicitly (every role that owns any database with CONNECT), or warn when the owner is `pg_database_owner`.

### L17-04 medium: the lock order is not total; two writers called in one statement or transaction in opposite orders deadlock, and a hand write deadlocks with DML on a table under `ledger_enable`

Files: `write.c:452-463` ("so no two of them can wait for each other"), README "The order of the locks" ("One order, for every writer", "no deadlock").

What is wrong. The order holds inside each function; it is not enforced across calls. A non-superuser may call two writers in one statement (the builders are refused only in a transaction block). A deadlock is detected after `deadlock_timeout`, so nothing hangs, but one of two honest callers gets `40P01`, and a role can cause it on purpose.

Statements (19rc2; all roles `app`, ordinary, with EXECUTE on the functions):
- `SELECT kingres.ledger_verify_recorded(), pg_sleep(2), kingres.ledger_generate_digest()` in one session and `SELECT kingres.ledger_generate_digest(), pg_sleep(2), kingres.ledger_verify_recorded()` in another: `ERROR: deadlock detected ... waits for ExclusiveLock on object ... of class 3079` in one of them.
- `SELECT kingres.ledger_enable('pa'), pg_sleep(2), kingres.ledger_build_block()` against `SELECT kingres.ledger_build_block(), pg_sleep(2), kingres.ledger_enable('pb')`: deadlock.
- A superuser with `allow_manual_write`: `BEGIN; INSERT INTO kingres.ledger_digest ...` (takes all four locks), `pg_sleep(2)`, `INSERT INTO pc VALUES (1)`; meanwhile app calls `ledger_enable('pc')` (takes the table lock, waits for the registry): `deadlock detected` in the superuser's statement; the first and the last of the four locks can also order against a transaction that took the verifications' lock before the builders'.

What a fix must achieve: either say in the README and the comment that the order is per function and a caller must call in the order builders, registry, verifications, removals (and take no table lock after a hand write), or make each writer fail early with a clear error when it would take an earlier lock after a later one.

### L17-05 low: the log line of an ended session lists locks that were released

File: `write.c:428-438`. The list is built when locks are noted and is not shrunk when a subtransaction aborts. Statements: `BEGIN; SAVEPOINT s1; SELECT kingres.ledger_verify_recorded(); SAVEPOINT s2; SELECT kingres.ledger_enable('pg_'); ROLLBACK TO s2; SELECT pg_sleep(9);` (limit 6 s). Log: `it held the verifications' lock and the registry's lock of the extension for longer than ...`; only the verifications' lock was held. A fix: build the list from the entries at the time of the kill (copy it in the timer handler path, or keep a count per name).

### L17-06 low: an identifier is reused for a different row after a hand-made delete of a digest

A superuser with `allow_manual_write` ran `DELETE FROM kingres.ledger_digest WHERE digest_id = 1` (height 0). It is allowed and nothing is recorded in `ledger_removed`; the next `ledger_generate_digest` gave `digest_id` 1 to the digest of height 1. Only `ledger_remove_stray` records removed digest ids (`write.c:519-525`). Nothing in a hash refers to a digest id, but an outside system that stored id 1 now has another row under it. A fix: record the delete, or say in the README that only `ledger_remove_stray` keeps ids from coming back. (Recorded verifications cannot be deleted: `table "ledger_verification" is an append-only ledger table`.)

### L17-07 note: top of the type

A superuser inserts, with `OVERRIDING SYSTEM VALUE`, a digest and a verification with id 9223372036854775807. Then an honest `ledger_generate_digest` fails with `bigint out of range` (the SQL adds 1) and `ledger_verify_recorded` fails with `setval: value -9223372036854775808 is out of bounds` because `kl_next_id` computes `m + 1` in C (`write.c:571`) and wraps (signed overflow is undefined; PostgreSQL builds with `-fwrapv`). The verification row cannot be removed (append-only, and `ledger_remove_stray` does not take verifications), so recorded verification is lost for ever in that database; the digest is cured by deleting the row. A superuser's act. A fix: test `m == PG_INT64_MAX` and raise a clear error, and give `ledger_remove_stray` a path for it.

### L17-08 low: the cancel path of `ledger_verify_recorded` has no test

Mutant 5 (`write.c:1084`, the try-lock after a cancel replaced by `if (0)`, so the failure record is written with no lock at all) passes `t/019`, `t/022`... and `t/023`: nothing cancels the run of `ledger_verify` inside the subtransaction (the tests cancel the wait for the lock, which is a different code path). A test: cancel the run (large ledger, `statement_timeout`) while another session holds the verifications' lock, and expect the cancel to be raised; with the lock free, expect a `failed` record.

### L17-09 note: README claims the code does not keep

- "One order, for every writer ... finds no deadlock" (L17-04).
- "(at most `lock_hold_limit` ... counted again at every thousand rows)" for a role that is not a superuser (L17-01).
- exception 6 "computed as PostgreSQL does" (L17-02, L17-03). `t/021` compares the formula with a list written by hand in the same file; it runs a real drop for six configurations only.
- The listing includes group roles that cannot log in (`NOLOGIN`, members only): over-reporting, harmless, not stated.

## 5. What held under attack

- First invariant, 18.7 and 19rc2: as the member of the owner role (INHERIT, no SET), as a member of `pg_write_all_data` + `pg_maintain`, and as the owner of an application table, `INSERT`, `UPDATE`, `DELETE`, `TRUNCATE`, `COPY FROM`, `LOCK ... ACCESS EXCLUSIVE`, `ALTER TABLE ... SET`, on all 11 catalog tables: no row changed (counts 1/4/2/1/0/0 before and after, `ledger_verify` clean); only `VACUUM FULL`, `CLUSTER`, `REINDEX` run, which README exception 1 documents. Nothing new in 19rc2 against 18.7 was found.
- Hostile `setval` to 9223372036854775806 by the member of `pg_write_all_data` on the sequences: `ledger_generate_digest`, `ledger_verify_recorded`, `ledger_enable` still work (18.7).
- DDL battery as the member of the owner role, 50 statements: every `ALTER FUNCTION` form, `CREATE OR REPLACE` of a ledger function, `ALTER SCHEMA`, `ALTER EXTENSION ADD/DROP/SET SCHEMA`, `REASSIGN OWNED`, `DROP OWNED BY` the owner, `ALTER TABLE` forms on ledger tables, policies, rules, triggers, indexes, views, sequences, `GRANT/REVOKE` on the schema: refused. Allowed: grants to another role, `COMMENT ON FUNCTION`, `ALTER DEFAULT PRIVILEGES`, `DROP OWNED BY` the member itself (no objects). Nothing harmful found.
- `lock_hold_limit` cannot be changed by the owner of the database or the role (`ALTER DATABASE/ROLE ... SET`, `SET`, `set_config`, function `SET` clause: all `permission denied to set parameter`); a placeholder set before the library is loaded is refused with a warning and the value stays `1min` (18.7, library not preloaded).
- Per-subtransaction timer: `COMMIT AND CHAIN`, `ROLLBACK AND CHAIN`, savepoint rolled back, exception block that caught an error after the lock, an error in the top transaction followed by a new one that sleeps 9 s: all survive; a lock held by the top transaction and the same lock taken again in a rolled back savepoint is still limited (killed at 6.0 s).
- PREPARE: refused after a lock taken directly, in a released savepoint, in a completed PL/pgSQL block.
- Identifiers: uniqueness at READ COMMITTED, REPEATABLE READ and SERIALIZABLE under four concurrent sessions; digests with three concurrent writers (18 distinct of 18); hand-made digest racing an honest one.
- The three install paths produce one catalog (section 2); the new index and view columns are in all of them.
- Cancel of the wait for the verifications' lock under `statement_timeout`: ended at 2.004 s.

## 6. Verdict

Not ready to call part A closed. Every L15 finding is fixed or answered, the suite is green on both majors, and nine mutants of the round are mostly killed. But the new renewal of the lock limit breaks the limit it is meant to bound: an ordinary role with EXECUTE on `ledger_enable` and a builder (or `ledger_verify_recorded`) keeps the builders' lock, and so stops the block builder, for as long as it wants, which is the second invariant itself (L17-01, high). The list of roles that can drop the database is not "exactly" the set of roles that can (L17-02, L17-03), and the README claims a total lock order that two honest calls in one statement defeat (L17-04). None of the first-invariant checks found a way to write a ledger table or forge a block. L17-01 must be fixed and tested before the beta; L17-02 and L17-04 need a fix or a README correction.

## 7. Mutants of the round (19rc2, one-line, built clean)

| # | Mutant | Test run | Result |
|---|---|---|---|
| 1 | no PREPARE refusal (`write.c:317`) | `t/022` | killed (6 tests; the run then waits on locks left by prepared transactions and had to be stopped by timeout) |
| 2 | no disarm when the last entry goes with an aborted subtransaction (`write.c:375`) | `t/022` | killed (tests 18-23) |
| 3 | locks of a released subtransaction not moved to the parent (`write.c:353`) | `t/022` | killed (tests 20, 21) |
| 4 | `kl_next_id` with the snapshot of the transaction (`write.c:564`) | `t/023` | killed (test 19, REPEATABLE READ) |
| 5 | failure record after a cancel without the try-lock (`write.c:1084`) | `t/023` | **survives** (L17-08) |
| 6 | no locks for a hand-made row (`write.c:589`) | `t/023` | killed (test 35) |
| 7 | timer not disarmed at commit, abort, prepare (`write.c` callback) | `t/022` | killed (tests 16, 17, 24-29) |
| 8 | no renewal at every thousand rows (`write.c:1445`) | `t/022` | killed (tests 42, 43); but see L17-01: no test renews inside a rolled-back savepoint |
| 9 | the owner warning off in the three scripts | `t/021` | killed (tests 10, 15, 36) |

A first batch of mutants was invalidated by a stale object file (the build was not cleaned) and was discarded and redone with a clean build; the results above are from the redone runs. Fixed sleeps that decide a verdict: `t/022` and `t/019` use sleeps of 3 s and 6 s against limits of 2 s and 8 s as negative checks ("not ended"); they only fail on a machine that is paused for more than the margin, in the safe direction. `t/010` `sleep 20` bounds a count from above, safe on a slow machine. No test can fail for a reason other than the mutant above, except that mutant 5 exposes one that cannot fail.

## 8. Not checked

- A database of 0.3 from before this round (no pre-round script in the export): what it does with the new library was reasoned (old `ledger_enable` takes ids from the sequence and `ledger_register_table` now needs `max + 1`, so a moved sequence gives an error, not a silent mismatch; no warning, no new view columns, no index, only slower digests), not run.
- `dblink` or `postgres_fdw` loopback sessions, a background worker started by a role, a subscription owned by a non-superuser, and a `WITH HOLD` cursor at commit beyond what the suite does.
- The ids and deadlock cases at 19rc2 beyond those named; the pg_database_owner and tablespace cases on 19rc2 only by reading.
- `REASSIGN OWNED` as a way to take ownership of a database: needs the privileges of the old and the new owner already, so it adds no path (reasoned).
- A race of the timer that fires while a transaction ends (the flag `ProcDiePending` stays set and ends the session after the commit): by reading only.
- `pg_upgrade`, `pg_dump` restore and a standby beyond the author's suite; load behaviour.
