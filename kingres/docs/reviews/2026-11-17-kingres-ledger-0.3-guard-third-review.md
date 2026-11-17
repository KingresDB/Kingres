<!-- SPDX-License-Identifier: AGPL-3.0-only -->
# kingres_ledger 0.3, part A: independent review of the fixes for the second review of the guard (third round)

## 1. Scope and method

An independent reviewer examined the fix round that answers
`2026-11-17-kingres-ledger-0.3-guard-second-review.md` (findings L11-01 to L11-05): the
utility-hook and object-access-hook checks of `protect.c`, the refusal of the builders
inside a transaction block (`write.c`), the registry cache with its hash (`row_hash.c`,
`capture.c`), the new README section "What a role that is not a superuser can stop or
remove", `sql/ledger_stops.sql`, `t/018_second_invariant.pl` and the changes in
`ledger_paths` and `ledger_review`. Everything found by the earlier reviews
(`2026-11-16-ledger-phase7-steps-1-2.md`, its fixes, and the second and third fixes of
2026-11-17) was treated as a regression check; none of it came back.

Method. The committed tree was exported (not the working tree) and built out of tree
with `kingres/dev/ext.sh`, warnings as errors, against copies of the two installations
with assertions (PostgreSQL 18.7 and 19rc2). The existing suite was run once on each
major as a baseline. Then throwaway clusters (library preloaded, roles: a member of the
owner role with INHERIT and no SET, a member of `pg_maintain`, a member of
`pg_write_all_data`, the owner of an application ledger table, a role with EXECUTE on the
builders, a plain role, a database owner with CREATEDB) were attacked by hand. The
second invariant was attacked by class: locks, drops and changes, stalls, the registry
cache. Six mutants of the round were built that its author did not list. The cost was
measured against the two previous states of the extension. The most serious
reproductions were run on both majors; where a result below names one major only, the
other was not run.

Machine load. The machine was shared; the load average read between 3.8 and 6.9 during
the whole review (other people's builds and test suites). Timing results are medians of
interleaved runs for that reason, and the load is named in section 8.

## 2. Summary

| Severity | Count | Findings |
|---|---|---|
| critical | 0 | |
| high | 2 | L13-01, L13-02 |
| medium | 2 | L13-03, L13-04 |
| low | 1 | L13-05 |
| note | 2 | L13-06, L13-07 |

No row was written to a ledger table and no chain was forged by a role outside the first
invariant. No role that is not a superuser removed the ledger or the protection of a
ledger table, except through `DROP DATABASE` by the owner of a database (L13-03, a
claim in the README that is narrower than the fact). No crash and no loss of entries
for committed rows were seen.

The second invariant holds for the routes the round closed, and for every lock route,
every drop route and every change route that was added to the author's table here.
It does not hold for two things the author did not list:

1. `setval` on the identity sequences of `ledger_digest`, `ledger_verification` and
   `ledger_removed` by a member of `pg_write_all_data` (no owner membership needed)
   makes `ledger_generate_digest`, `ledger_verify_recorded` and `ledger_remove_stray`
   fail for every caller including a superuser, until a superuser repairs the
   sequences (L13-01). The README names the registry's sequence and the owner role only.
2. The "named exceptions" of long statements and of a pipeline without Sync are
   unbounded, and the remedies the README gives for them do not work (L13-02):
   `idle_in_transaction_session_timeout` does not end a pipeline that has no Sync, and
   `statement_timeout` and `transaction_timeout` that an administrator sets for a role
   can be switched off by that role in its own session.

Verdict on the round's tests: they are good where they were written to be (they fail
without the fix for four of the six mutants built here), and two mutants survive the
whole suite of 750 TAP tests and 18 regression tests (L13-04).

## 3. Fate of L11-01 to L11-05

| Finding | Fate | Evidence |
|---|---|---|
| L11-01 lock through a view | fixed | `LOCK TABLE` in ACCESS EXCLUSIVE, SHARE and ROW EXCLUSIVE mode, as `pg_maintain` and `pg_write_all_data` members, of 14 forms of view (plain, security_barrier, security_invoker, view of view, CTE, UNION, LATERAL, EXISTS, scalar subquery in the target list, CASE, function in FROM, VALUES) all refused with KL002 before any lock; 60 refused statements of the owner role, `pg_maintain` and `pg_write_all_data` run while a reader held ACCESS SHARE on all nine catalog tables: none waited, and an insert plus `ledger_build_block` in a third session never waited (18.7). A view whose function body reads a catalog table is not followed, as in core (`LOCK` does not follow function bodies either). |
| L11-02 `DROP EXTENSION plpgsql CASCADE` | fixed | as a member of the owner role: the NOTICE lists the cascade, then `ERROR: cannot drop event trigger kingres_ledger_guard_rewrite: it belongs to the ledger`; the registry still has its rows (18.7 and 19rc2). `DROP LANGUAGE plpgsql CASCADE` is refused by core. `DROP SCHEMA kingres CASCADE`, `DROP OWNED BY`, `DROP EXTENSION kingres_ledger [CASCADE]`, `REASSIGN OWNED` refused. |
| L11-03 builder held in a transaction | fixed for BEGIN, savepoint, exception block, multi-command string, WITH HOLD cursor, a second statement of a pipeline, `PREPARE TRANSACTION`: all give 25001. Not bounded for the rest: see L13-02 | the first Execute of a pipeline without Sync and a long statement still hold the lock, and the documented bounds do not work |
| L11-04 test of the ownership check | not re-run as a mutant | the new case is read; the author's mutant list was not repeated |
| L11-05 schema ACL, owner, sequence | fixed for schema GRANT/REVOKE/OWNER and `ALTER SEQUENCE`; the underlying hole (`setval`) is wider than the README says: L13-01 | `REVOKE USAGE ON SCHEMA kingres FROM PUBLIC`, `GRANT ALL ON SCHEMA`, `ALTER SCHEMA ... OWNER TO`, `GRANT ... ON ALL SEQUENCES IN SCHEMA kingres`, `ALTER SEQUENCE ... MAXVALUE` refused; `setval` runs |

## 4. Locks

Tried, by a member of `pg_maintain` (M), of `pg_write_all_data` (W), of the owner role
(O) or the owner of an application table (A), on 18.7 unless said:

- Direct `LOCK` above ACCESS SHARE (M, W, O), `LOCK ... ONLY`, `NOWAIT`, a list with an
  application table first, inside a `DO` block: refused.
- View forms listed in section 3; a materialized view over a catalog table
  (`LOCK` on it is "cannot lock relation"; `REFRESH` takes ACCESS SHARE on the source
  only); a partitioned application table: harmless.
- `SELECT ... FOR UPDATE` on a catalog table by W: permission denied (W has no SELECT on
  the queue); `DECLARE ... WITH HOLD ... FOR UPDATE` is refused by core.
- Making a catalog table a child, a parent or a partition, by `CREATE TABLE ... INHERITS`,
  `PARTITION OF`, `ALTER TABLE INHERIT` in both directions, `ATTACH PARTITION` both ways;
  also the same for an application ledger table, as its owner: all KL002. `CREATE TABLE
  ... (LIKE kingres.ledger_x INCLUDING ALL)` makes an independent table with no link.
- Foreign keys that reference a catalog table: `CREATE TABLE ... REFERENCES`, column and
  table constraint, `ADD FOREIGN KEY ... NOT VALID`, `ADD CONSTRAINT`, `ADD COLUMN ...
  REFERENCES`, on a partitioned referencing table: all refused. A foreign key from the
  role's own table to an application ledger table is allowed (README) and takes no lock
  on the ledger.
- `CLUSTER`, `REINDEX` (not concurrently), `VACUUM FULL` of a catalog table by M or O:
  refused inside a transaction block; as a statement of their own they run and the
  ledger works afterwards. `REINDEX TABLE/INDEX/SCHEMA/DATABASE CONCURRENTLY`,
  `VACUUM (ANALYZE)`, `VACUUM (TRUNCATE)`: run, ledger intact.
- `CREATE PUBLICATION ... FOR TABLE` of catalog tables by O: runs (note L13-07); all
  catalog tables have a primary key, so no publication can make the builder's `DELETE`
  fail for lack of a replica identity.
- Queue effect: the 60 statements above (50 for O, 6 for M, 2 for W, plus the lock
  forms) each ran while a reader held ACCESS SHARE on every catalog table; each was
  refused at once or ran without a lock request that conflicts with a writer; none made
  a capture insert or `ledger_build_block` wait (1 s timeout).
- A race between the hook's name lookup (no lock) and the lookup of `LOCK` itself, with
  two schemas on the `search_path` and a second session that creates and drops a view
  over a catalog table in the first schema: 114,000 successful attempts in 90 s, no
  lock obtained (see L13-07). Not exploitable in practice; present in principle.
- Not run: `postgres_fdw` / `dblink` loopback (a non-superuser needs password
  authentication; the cluster used trust), `CREATE SUBSCRIPTION` with copy, `COPY ...
  FREEZE`, `ALTER TABLE ... ATTACH PARTITION` of a table that has a foreign key.
  Reasoning only: a loopback session is a second session of the same role, and every
  statement there goes through the same hooks.

## 5. Drops and changes

- The object access hook refuses every cascade tried (section 3). Its exception, "the
  scripts of the extension", is `creating_extension && CurrentExtensionObject ==
  extension`; there is no check of the statement text in the object access hook (the
  statement hook compares the query string pointer). A role cannot supply a script
  (`extension_control_path`, `dynamic_library_path` and the share directory are not
  settable by a non-superuser); a second extension's script does not satisfy the test.
  The widening of the exception to any script survives the tests (L13-04).
- Indexes of catalog tables are not covered by the hook. A role cannot reach a cascade
  to them: the opclasses, collations and functions they depend on belong to the
  bootstrap superuser, and the objects the owner role may drop do not carry them. Not
  reproduced.
- `DROP OWNED BY` the owner role, an application role that owns a ledger table, and
  `REASSIGN OWNED` are refused (also for a superuser: a control plane that drops an
  application role must drop or keep its ledger tables first; it cannot).
- `ALTER` forms on a user ledger table as its owner: `DROP COLUMN`, `ALTER TYPE`,
  `RENAME COLUMN`, `ENABLE REPLICA TRIGGER`, `DISABLE TRIGGER`, `SET UNLOGGED`, `ADD
  COLUMN` with default or generated, rules, inheritance, `ATTACH PARTITION`: refused.
  `SET ACCESS METHOD heap`, `SET STORAGE`, `SET COMPRESSION`, `REPLICA IDENTITY`, `DROP
  CONSTRAINT` (the primary key), RLS and policies, a `BEFORE INSERT` trigger that
  returns NULL, `RENAME`: allowed as the README says. Renaming the table and creating
  a twin with the old name makes a table that is not a ledger table; the old table
  keeps its identity (by oid) and its capture.
- `ALTER FUNCTION ... SET/RESET`, `CREATE OR REPLACE FUNCTION` of a ledger function,
  `ALTER EVENT TRIGGER DISABLE/RENAME`, `ALTER SCHEMA RENAME`, `ALTER EXTENSION SET
  SCHEMA/DROP member`, `DROP EVENT TRIGGER`, `DROP TRIGGER` of a capture trigger:
  refused.
- `DROP DATABASE` by the owner of the database, who is not a superuser: runs (L13-03).
- `setval` and `nextval` on the identity sequences: L13-01.
- Database and role settings: the worker resets 24 settings itself (test `010_review2`);
  settings that are superuser-only cannot be changed by a database owner. Not re-run
  here: `ALTER DATABASE ... ALLOW_CONNECTIONS false` and `CONNECTION LIMIT` by the
  owner (by core semantics they stop every new connection, the worker's included).
- Own-script path: `ALTER EXTENSION kingres_ledger UPDATE` by the owner-role member on
  an up-to-date database is a no-op; the upgrade tests pass.

## 6. Stalls

- Refused with 25001 for a role that is not a superuser: `BEGIN`, savepoint, exception
  block, multi-command string, WITH HOLD cursor, `PREPARE TRANSACTION` after a builder,
  a builder as the second statement of a pipeline (all on 18.7; the first three also
  by the suite). `ledger_build_block` as one statement, or inside a function called as
  one statement, works.
- A builder as the first Execute of a pipeline that never sends Sync holds the lock of
  the builders as long as the client likes. The README says `idle_in_transaction_session_timeout`
  ends it. It does not (L13-02): the backend shows `active`, with the timeout at 2 s it
  was still holding the lock after 14 s (18.7 and 19rc2).
- One long statement (`SELECT kingres.ledger_build_block(), pg_sleep(8)`): ended at the
  administrator's `statement_timeout` of 1 s in the control; with `SET statement_timeout
  = 0` by the role itself it ran 8 s and a builder in another session timed out on the
  lock (L13-02).
- An open write transaction of another role does not stop the builder (it built block
  28 and a digest, `ledger_verify` clean). A prepared transaction holding entries does not
  either; `COMMIT PREPARED` afterwards verifies clean. A REPEATABLE READ snapshot left
  open does not stop the builder (VACUUM in a block is refused by core, not tested).
- Pending queue growth is not bounded by the capture (a role's own inserts), as before.
  Not measured.
- False positives of 25001, all stated in the README for the control plane: any
  application transaction that calls the digest, an ORM with autocommit off,
  `psql --single-transaction`, a PL/pgSQL exception block. A pooler in transaction
  mode sends autocommit statements and is not affected.

## 7. The registry cache

A transaction on 18.7: savepoint `a`, `ledger_enable('c1')`, savepoint `b`, an insert
(cache filled), `RELEASE b`, `ROLLBACK TO a`, then `ledger_enable('c2')`, `ledger_enable('c1')`
again (a new table id), inserts into both, a rename swap of the two tables inside the
transaction, inserts again, `COMMIT`. Entries went to the table ids the registry has (two
per table), `ledger_verify` reported 0 problems. A second transaction with three nested
savepoints, a unique violation in the capture's table and `ROLLBACK TO` also verified
clean. The cache is keyed by relation oid, stores positive answers only, is dropped at
every transaction end and at every subtransaction abort; one entry per ledger table
written by a transaction bounds its size by the number of ledger tables. No wrong
identity was served. Not covered: a parallel worker (triggers do not run there), a
prepared transaction with its commit in another session beyond the check above.

## 8. Honest operation

On both majors the baseline passed (section 11). By hand on 18.7: `REINDEX ... CONCURRENTLY`
of table, index, schema and database, `VACUUM (ANALYZE)`, `VACUUM FULL`, `CLUSTER`, as the
owner and as a `pg_maintain` member: all ran, the ledger worked afterwards.
`pg_dump` and restore: the ledger tables are not dumped and the restored database has
an empty registry (README); the restored copy reports problems for the entries of the
tables it lost, as documented, and `ledger_enable` has to be called again.
`DROP EXTENSION kingres_ledger`: works with an empty registry, refused with a non-empty
one; the README gives no way out inside the database (only the catalogs or `DROP
DATABASE` by a superuser); a superuser `DROP DATABASE` with ledger tables works.
`DROP OWNED BY` an application role that owns a ledger table is refused for a superuser
too. The three upgrade paths, `pg_upgrade` and the standby are covered by TAP files that
passed (`007`, `011`, `016`, `002`); `pg_upgrade` from 18.7 to 19rc2 and a base backup
were not run by hand. Logical replication of an application ledger table is covered by
`010` (passed). The control plane's own calls (`ledger_enable`, `ledger_generate_digest`,
`ledger_verify_recorded` as a superuser, dropping a database) ran without a refusal.
Sampled statements of the owner rule that are not in the author's list all gave the
README's outcome.

## 9. The tests of this round

Preconditions are sound (`ledger_stops` runs in its own database and drops it first, as
a superuser; `t/018` has a control that the lock is held and that the work stops).

Mutants built by the reviewer (each run against the regression suite and `t/018`; the
two that survived against the whole suite):

| Mutant | Result |
|---|---|
| view walker stops after one level of views | killed by `ledger_stops` |
| object access hook ignores columns (`subId != 0`) | killed, by `ledger_review`; `ledger_stops` itself did not fail |
| "own script" exception widened to any extension script in the object access hook | survives all 18 regression tests and all 750 TAP tests (L13-04) |
| 25001 check skipped for pipelines only | survives all 750 TAP tests (L13-04); a second statement of a pipeline is refused by the unmutated code, so the check works, it is not tested |
| TRUNCATE refusal after the lock | killed by `t/018` ("does not queue behind a reader") |
| hash kept across a subtransaction abort | killed by `ledger_review2` |

Timing. `t/018` checks that a refused statement does not queue by sleeping 0.5 s after
sending it and then doing the writer's work with a 2 s lock timeout; it never checks that
the statement is in fact waiting. On a slow machine the sleep can end before the
statement asks for its lock, and the mutant that locks first would pass (L13-05). Five
statements are tested for the queue; the reviewer's list had 60. During the review a
full TAP run with the machine at load 6 to 7 failed 16 tests in 4 files (builder lock,
`ledger_verify` rows, standby); the same mutant passed all 750 on a rerun at load 3.8.
The suite is sensitive to load.

## 10. Cost

Method. Three servers (this round, the L10 round, and the state before the L10 round)
on 19rc2, the same settings (`fsync` off, `synchronous_commit` off, no builder, no
autovacuum), each run on a fresh database with 19 ledger tables; one pgbench client for 8 s per
scenario; seven repetitions per cell, versions interleaved inside each repetition so that
drift of the machine falls on all three. Load: not quiet. The load average was 3.3 to 6.9
from other people's work for the whole run, so the spread is large (min to max about 40
percent in the single-row case).

Transactions per second, median of 7 (min to max):

| Scenario | before L10 | L10 round | this round | this vs L10 | this vs before L10 |
|---|---|---|---|---|---|
| single-row insert, one table | 6511 (5075-7678) | 5895 (4470-7491) | 6094 (4260-6964) | +3 percent | -6 percent |
| 50 + 50 inserts alternating between 2 tables in one transaction | 76.4 (66.5-88.9) | 66.7 (51.3-79.7) | 68.8 (60.7-88.3) | +3 percent | -10 percent |
| 10 x 16 inserts alternating between 16 tables in one transaction | 47.3 (39.1-52.7) | 42.4 (28.4-44.7) | 39.3 (33.7-47.2) | -7 percent | -17 percent |
| bulk insert of 100,000 rows into one table | about 0.4-0.5 statements per second in all three; no difference visible at that resolution | | | | |

Reading. A difference of 5 percent cannot be stated from these runs: the spread inside one
cell is larger than the differences between the L10 round and this round, and the means
(alt2 66.9 vs 71.2, alt16 40.1 vs 40.5) have the opposite sign to two of the medians. What
the data does say: this round is not slower than the L10 round beyond the noise for one
table or two, the bulk case shows no cost, and against the state before the guard both
medians for the alternating cases are lower (10 and 17 percent), more than the author's
figure of 7 percent for two tables, but with a spread of the same size. Sixteen tables
alternating is not in the author's table; no gain over the L10 round shows there, and
the cause was not profiled. A quiet machine is needed to settle it.

## 11. New findings

### L13-01 (high): `setval` on three more sequences, by a member of `pg_write_all_data`, stops the digest, the recorded verification and `ledger_remove_stray` for everybody

Steps (18.7 and 19rc2; database with the extension and a ledger table; `w` is a member
of `pg_write_all_data` and nothing else):

```
-- as w
select setval('kingres.ledger_digest_digest_id_seq', 9223372036854775807);
select setval('kingres.ledger_verification_verification_id_seq', 9223372036854775807);
select setval('kingres.ledger_removed_removal_id_seq', 9223372036854775807);
-- as the superuser
select kingres.ledger_generate_digest();
--   ERROR: nextval: reached maximum value of sequence "ledger_digest_digest_id_seq"
select kingres.ledger_verify_recorded();
--   ERROR: nextval: reached maximum value of sequence "ledger_verification_verification_id_seq"
```

`ledger_build_block` and the capture go on; the digests the control plane stores outside
the database can no longer be made, and recorded verifications fail (a verification that
fails also fails to record its own failure). A superuser repairs it with `setval` to
max+1. The registry's sequence is the same: `w` can `setval` it (and `nextval` it, 2,000,000
values in 0.8 s: USAGE and UPDATE on every sequence come with `pg_write_all_data`), so
`ledger_enable` fails for everybody once the value is the maximum. Observed: `setval`
accepted from `w`, refused from `pg_maintain` members and plain roles. Expected: the
second invariant says such a role cannot stop the builder; the README's exception 4
names only the registry's sequence and only a member of the owner role.

Place: the sequences created by the install script; no hook sees a function call.
Direction: do not depend on the sequence value. Let each of the four functions take the
next identifier as `max + 1` under the lock they already hold (or call `setval` to
`max + 1` themselves, as the owner, before the insert), so that a forged value heals at
the next call; and state in the README that `pg_write_all_data` can set every sequence.

### L13-02 (high): the exceptions "a long statement" and "a pipeline without Sync" are unbounded, and the remedies named for them do not work

Steps (18.7 and 19rc2; role `aud` has EXECUTE on the builders; the administrator ran
`ALTER ROLE aud SET statement_timeout = '1s'` and, in the second case,
`ALTER ROLE aud SET idle_in_transaction_session_timeout = '2s'`):

```
-- as aud, two statements sent separately
SET statement_timeout = 0;
SELECT kingres.ledger_build_block() IS NOT NULL, pg_sleep(8);
-- another session, lock_timeout 1.5 s:  select kingres.ledger_build_block();
--   ERROR: canceling statement due to lock timeout
--   CONTEXT: waiting for ExclusiveLock on object ... of class 3079 ...
```

Without the `SET` the same call is cancelled at 1 s (control). Pipeline (psql 18
`\startpipeline`, `SELECT kingres.ledger_build_block() \bind \sendpipeline`, `\flushrequest`,
`\getresults`, no Sync, then wait): `pg_stat_activity` shows the backend `active` for
more than 14 s with `idle_in_transaction_session_timeout = 2s`, the lock held, other
builders timing out. With `transaction_timeout = '2s'` set for the role the session is
ended after 2 s (control), but the role removes it with `SET transaction_timeout = 0`
before the pipeline. A function-level `SET statement_timeout` does not re-arm the
running statement's timer.

Expected: README exception 3 says `idle_in_transaction_session_timeout` ends the
pipeline; exception 1 says to set `statement_timeout` for roles you do not trust. All
three settings are user-settable; a role can switch off any of them for its own session,
so nothing an administrator configures bounds the time. With no bound, a role that was
given EXECUTE on a builder can stop every builder, the worker's included, for as long
as it likes, which is what the second invariant says no non-superuser can do.

Place: `src/write.c`, `kl_refuse_open_transaction` and `kl_lock_builders`; README "What a
role that is not a superuser can stop or remove", exceptions 1 and 3. Direction: for a
caller that is not a superuser, bound the time inside the function: arm an own timeout
when the lock is taken and cancel at, say, `kingres_ledger.builder_hold_limit`; or take
the builders' lock for a non-superuser only for the part of the work that needs it, or
make the non-superuser entry point post a request that the worker runs. At least correct
the README: the idle timeout does not apply to a pipeline, and the three timeouts can be
overridden by the role, so the exception is unbounded and `pg_terminate_backend` is the
only remedy.

### L13-03 (medium): the owner of a database that is not a superuser can drop the database and with it the ledger

Steps (18.7): `CREATE ROLE dbo LOGIN CREATEDB; CREATE DATABASE dbd OWNER dbo;` in `dbd`, as a
superuser, `CREATE EXTENSION kingres_ledger`, a table, `ledger_enable`; then as `dbo`,
connected to `postgres`: `DROP DATABASE dbd` succeeds.

Expected: README exception 6 attributes `DROP DATABASE` to "what a superuser does" and the
invariant says no non-superuser can remove the ledger. It is true only if every database
with a ledger is owned by a superuser. Place: README section on the second invariant
and the control plane notes. Direction: say that a database owner can drop it, and that
a control plane must create databases with a superuser as owner (or with an owner it
trusts); nothing in the extension can prevent it (`DROP DATABASE` runs no object access
hook for the objects).

### L13-04 (medium, tests): two mutants survive the whole suite

(a) In `kl_object_access`, replacing `creating_extension && CurrentExtensionObject ==
extoid` by `creating_extension` (any extension's script bypasses the hook) passes all 18
regression tests and 750 TAP tests on 19rc2. (b) Removing the pipeline half of the 25001
test (`(MyXactFlags & XACT_FLAGS_PIPELINING) != 0`) passes all of them too. The code is
right in both cases as written, but nothing keeps it so. Direction: (a) a test extension
(a control and a script file in a temporary directory, which `extension_control_path`
allows for a superuser) whose script runs `DROP FUNCTION kingres.ledger_head()` or
`DROP EXTENSION plpgsql CASCADE` while a table is under ledger control, expected to be
refused; (b) a pipeline test with a first statement and then a builder.

### L13-05 (low, tests): the queue test can pass by accident

See section 9: a 0.5 s sleep stands for "the refused statement is now asking for its
lock", and only five statements are tested. Direction: wait until the backend of the
refused statement is seen in `pg_locks` with `granted = false` or has finished, and
loop over a longer list; the reviewer's list of 60 statements in section 4 can be used.

### L13-06 (note): `PUBLIC`'s `SELECT` on the readable catalog tables can be revoked by the owner-role member

`REVOKE SELECT ON kingres.ledger_table FROM PUBLIC`, `REVOKE ALL ON ALL TABLES IN SCHEMA
kingres FROM PUBLIC` and `ALTER DEFAULT PRIVILEGES IN SCHEMA kingres ...` run for a
non-superuser member of the owner role (the README allows it for an operator). Reading
and publication of the chain by plain roles stops; the builder and the capture are
unaffected. Say in the README that the second invariant does not cover reading.

### L13-07 (note): the lock check resolves the name without a lock, then `LOCK` resolves it again

`kl_check_lock` calls `RangeVarGetRelid(..., NoLock, true)` and core resolves the name
again. With two schemas on the `search_path` and a second session that creates and drops
a view over a catalog table in the first schema, 114,000 successful `LOCK` attempts in
90 s (three clients) did not once end with a lock on a catalog table, so the window is
too small to use; a view owner cannot redefine a view that is already locked (the check
holds ACCESS SHARE), but nothing protects the interval before that. Direction: resolve
once with `RangeVarGetRelidExtended` and a callback, as the earlier name-resolution
fix did for other statements, or let the executor hook refuse the lock where it is taken.

## 12. What held under attack

- Every direct and view-based `LOCK`; `TRUNCATE` of catalog tables with and without CASCADE;
  every foreign key form; inheritance and partitioning in every direction.
- `DROP EXTENSION plpgsql CASCADE`, `DROP SCHEMA ... CASCADE`, `DROP OWNED BY`,
  `REASSIGN OWNED`, `DROP EVENT TRIGGER`, `ALTER FUNCTION/EVENT TRIGGER/SCHEMA/EXTENSION`
  forms, schema GRANT/REVOKE/OWNER, `ALTER SEQUENCE`.
- The owner rule on an application ledger table (60 statements).
- The registry cache under savepoints, renames and errors.
- Honest maintenance (`REINDEX CONCURRENTLY`, `VACUUM`, `CLUSTER`) by the owner and by
  `pg_maintain`; the refused statements leave no lock behind.

## 13. Not reproduced

- The lock-check race of L13-07 (114,000 attempts, no hit).
- A route to drop an index of a catalog table through a cascade (the objects the
  indexes depend on are not owned by any role that can drop them).
- A way to make the "own script" exception serve a role (needs control of the share
  directory or of `extension_control_path`).
- A stale identity in the registry cache.
- A stall by an old snapshot, a prepared transaction or an open writer.

## 14. Not covered

`postgres_fdw`/`dblink` loopback, `CREATE SUBSCRIPTION` with copy, `COPY ... FREEZE`,
`ATTACH PARTITION` of a table that has the foreign key, `ALTER DATABASE ... SET` and
`ALLOW_CONNECTIONS` by a database owner beyond the author's 24 settings, the mutant for
L11-04 and the author's mutants f1 to f8, a base backup and a standby by hand,
`pg_upgrade` by hand, `ALTER EXTENSION UPDATE` from 0.1 and 0.2 by hand (the TAP files
passed), the growth of the pending queue, and `DROP EXTENSION plpgsql CASCADE` racing a
first `ledger_enable` in another session.

## 15. What a control plane must know

- Call `ledger_build_block` and `ledger_generate_digest` as a statement of their own
  when the role is not a superuser (25001 otherwise); give a superuser service role the
  calls that must run in a transaction.
- A superuser is not refused by anything here, but a drop of any ledger object, of a
  database's extension, or of an application role that owns a ledger table, is refused
  with KL002 even for a superuser; the only way to switch a ledger off is `DROP DATABASE`.
- Create databases with a superuser as owner (L13-03). Do not give `pg_write_all_data`
  to roles you do not trust (L13-01), and give EXECUTE on a builder only to roles you
  can terminate (L13-02).
- Watch `ledger_generate_digest` errors that mention `nextval` and repair the sequence
  with `setval` to max+1 as a superuser.

## 16. Baseline

Out of tree on 18.7 with assertions, warnings as errors: 18 regression tests passed and
18 TAP files with 750 tests passed (227 s). On 19rc2: the same, 18 and 750 (225 s).
