<!-- SPDX-License-Identifier: AGPL-3.0-only -->
# Tenth independent review of kingres_columnar 0.1 (the fixes for C9 and the autovacuum follow-up)

Reviewer: an independent reviewer. Date: 2026-11-18.

## Scope and method

Reviewed: the read-only export of `contrib/kingres_columnar/` (C, SQL, tests, README), `kingres/docs/reviews/`
and `kingres/dev/` (`ext.sh`). The previous review (C9-01 to C9-06) and the earlier ones were read first, then the
README (the part "Writes while a transaction is ending", the decision table, the paragraph about autovacuum) and the
code behind each row. Line numbers are those of the export.

Method. I worked on copies only: one copy of each installation (PostgreSQL 18.7 and 19rc2, both built with
assertions), one copy of the export, build and tests out of tree with `ext.sh` (the copy of the script was changed
only to move its socket directory and ports into my own area; the TAP framework copy only to confine its port range
to 59400-59499). Everything ran under `nice`, at most two test or server processes at a time, nothing in shared
scratch or memory-backed directories. Every server I started was stopped; the area was removed at the end.

My own tools (not part of the repository):

* A test module `rc10` (about 200 lines of C) that registers transaction and subtransaction callbacks and, at an
  event named by a setting, optionally begins an internal subtransaction, optionally enters a critical section, and
  then calls one table access method entry point (`tuple_insert`, `relation_nontransactional_truncate`), a C
  function of the extension (`columnar_check_clean`, `columnar_identity`), or raises an ERROR. It was loaded before
  the extension in `shared_preload_libraries` (its callbacks run after the extension's) and after it (they run
  before), on both majors. It is not `t/kcevents`.
* A workload generator (250 random transactions of inserts, savepoints, `ROLLBACK TO`, `RELEASE`, PL/pgSQL
  exception blocks, aborted transactions, `columnar_compact`) that sends the same statements to a columnar table
  and a heap twin, and a driver for crash, base backup, standby, promotion, dump and restore.
* One-line mutants of the extension, built into a separate copy of the 19rc2 installation.

## What I ran

| Run | 18.7 | 19rc2 |
|---|---|---|
| clean build with `EXT_WERROR=1` | no warning | no warning |
| `pg_regress` (11 tests) | 11 of 11 pass | 11 of 11 pass |
| TAP, 34 files (all but `t/013`) | pass, 9785 tests, 447 s | pass, 9787 tests, 456 s |
| `t/013_xid_wraparound.pl` (the `xid_wraparound` module was not in the copied installations: I copied it in; out of tree the test skips) | pass, 23 tests, 101 s | pass, 23 tests, 103 s |
| own workload, seed 11 (below) | all comparisons same | all comparisons same |
| own scenarios with `rc10` (decision cells, 2PC, subtransactions, errors after the extension's callback) | as described below | identical to 18.7 in the cells I repeated |
| 9 one-line mutants (below) | | 8 killed, 1 equivalent |

In the TAP run no whole file was skipped; `t/031` skipped 148 assertions on these assertion builds by design (see
C10-07). I had no production build (no assertions); see "Not checked".

## Fate of the earlier findings

| Id | State | Evidence |
|---|---|---|
| C9-01 (critical section after the extension's own pre-commit loses the call, transaction commits) | **Fixed** as the review directed: that moment is "decided"; the README, the code comment and the warning text now say so. One residue (C10-03). | `kc_write.c:1133-1157` (`kc_crit_after_own_precommit`, `kc_ending`), warning text `kc_write.c:1229-1236`, README "A critical section entered after the extension's own pre-commit callback". With `rc10` loaded before the extension (callbacks after): crit insert at `pre_commit`: `WARNING: insert refused ... inside a critical section`, detail "if the transaction commits (or prepares) it does so without this call", commit stands, rows 6, the module's row absent; `ntrunc`: warning, rows unchanged (7 after the next commit); at `pre_prepare`: warning, `PREPARE TRANSACTION` succeeds, `COMMIT PREPARED` commits without the row. Loaded after (callbacks before): `ERROR: a call on columnar table "t" was refused earlier in this transaction, so it cannot commit`, rows 1..5, `pg_prepared_xacts` empty after the prepare attempt. Identical on 19rc2. Mutant m901 (`kc_ending` returns `KC_END_LIVE` for a critical section always) is killed by `t/031` only through its text assertions (`the detail says what is true of this cell`, `promises no error that cannot come`, 10 failures); `t/027`, `t/033`, `t/034` pass, because the two states commit the same data: the test that matters here is a text test. |
| C9-02 (t/026 has the teeth of its parent, survivors) | **Fixed as directed.** | The prelude now reads each scripted path back and compares with a heap twin. m26a (`kc_buf_flushable` without `b->refs == 0`): killed by `t/026` (`targeted: the cursor returned the rows it began with`, 3 assertions). m26c (the whole prepared-skip condition removed): killed by `t/026` (`prelude: late_window was taken (0 times)`, `prepared_skip was taken`, table differs from the heap twin) and by `t/022`; in the previous round `t/026` let this mutant through. m26b (only the `kc_commit_flushed` clause removed): survives `t/026` and `t/022`; the clause is redundant with the three others, so I count it as equivalent. |
| C9-03 (`columnar_check(false)` not a read at the end of a subtransaction) | **Fixed** by guarding the three check functions with `kc_subxact_refused` (`kc_write.c:1310`, `kingres_columnar.c:845,875`), not by reclassifying. | Through `rc10` calling `columnar_check_clean` at `commit`, `abort`, `commit_sub`, `abort_sub`, inside a critical section and at `pre_commit_sub` (extension's callback first): `WARNING: columnar_check_clean refused: ...`, the session lives, rows unchanged, 18.7 and 19rc2. Served at `pre_commit` and `pre_prepare` inside `BEGIN ... COMMIT` and for `PREPARE` (returned true, no FATAL). With the module's callback before the extension's at `pre_commit_sub`: `FATAL: BeginInternalSubTransaction: unexpected state SUBRELEASE`, the session ends; that is the residue the README states. Mutant m903 (`kc_subxact_refused` returns false) is killed (`t/031` dies, exit 29). |
| C9-04 (`not_checked` hides an error row from a filtering script) | **Fixed** as to the stated direction: the README says "any row is not clean", and `columnar_check_clean()` was added. The new function has its own defects: C10-01, C10-02. | `kingres_columnar.c:863-890`. Mutant m904 (a `not_checked` row not counted) is killed by `t/028` (2 assertions). |
| C9-05 (texts) | **Fixed.** | Warning for a subtransaction commit now reads "at the commit of a subtransaction" (`kc_write.c:1229-1236`); the error now carries a detail that says code of another module called the table at a moment where the server forbids an error (`kc_write.c:1183-1200`); seen in every run above. |
| C9-06 (stale subtransaction state across a commit untested) | **Fixed.** | `t/027` lines 224-242. Mutant m906 (no `kc_ending_reset()` at `XACT_EVENT_COMMIT`) is killed by `t/027` (3 assertions, order "before"). |

## New findings

| Id | Severity | Place | Statement |
|---|---|---|---|
| C10-01 | medium | `kc_check_one`, `kingres_columnar.c:684` | `columnar_check_clean()` and `columnar_check()` read the stripe catalog with the caller's own snapshot: in REPEATABLE READ or SERIALIZABLE they say clean for a damaged table whose stripes the old snapshot cannot see. |
| C10-02 | medium | `kc_check_guarded`, `kingres_columnar.c:749-775`, `kc_reconcile_ex` | The per-table `PG_CATCH` swallows a query cancel and `statement_timeout`; the orphan step then waits for a lock on the stripe catalog without end. README: "It never waits for a lock". |
| C10-06 | medium | `kc_relation_vacuum`, `kc_vacuum.c:128` | Every `VACUUM` and autovacuum visit takes `ACCESS EXCLUSIVE` on the table before it knows whether anything is to be truncated; on a primary with a hot standby that cancels the standby's queries on the table. Heap's vacuum does not, when nothing is to be truncated. |
| C10-03 | low | `kc_ending`, `kc_write.c:1139-1157`; README | After the extension's own pre-commit the extension's subtransaction callbacks still run when a later module begins a subtransaction; the rule "decided" ignores the nesting level and loses a call that the transaction could still have failed for. "Nothing of this extension runs again before the commit record" is not true. |
| C10-04 | low | `kc_am.c:584`, `kc_vacuum.c:154,177` | `TRUNCATE` sets `relfrozenxid` above the transaction id the metapage still holds; the autovacuum log line prints a `relfrozenxid` that is not the stored one. |
| C10-05 | low | `kc_relation_nontransactional_truncate`, `kc_am.c:607-620` | Where allowed, the call destroys committed data non-transactionally; an error raised afterwards by another callback leaves a table that cannot be read. |
| C10-07 | note | tests | 148 assertions of `t/031` are skipped on builds with assertions; `t/013` skips by message when its module is missing; judgement of the new threshold. |
| C10-08 | note | `columnar_check` | What "clean" covers. |

Severity scale as in the earlier rounds: critical = wrong, lost or doubled committed data where the caller did
nothing PostgreSQL forbids, a crash, an unreadable table; I found none.

### C10-01 (medium) `columnar_check_clean()` is blind to what an old snapshot cannot see

Place: `kingres_columnar.c:684` (`cs = kc_catalog_snapshot(GetActiveSnapshot(), &mvcc)` in `kc_check_one`);
`kc_catalog_snapshot` copies an MVCC snapshot and uses it for the stripe rows. The list of tables comes from
`pg_class` with a fresh catalog snapshot (`kc_check_run`), so a table made after the caller's snapshot is listed,
"checked", and its stripes are invisible.

Steps (18.7 and 19rc2, identical). Session A: `BEGIN ISOLATION LEVEL REPEATABLE READ; SELECT 1;` and keep it open.
Session B: `CREATE TABLE d1 (a int, b text) USING columnar; INSERT ... 20000 rows; CHECKPOINT;` then the file of
`d1` was cut to 2 blocks at the operating-system level (a damaged table).

```
B: SELECT kingres.columnar_check_clean();             -> f
B: SELECT check_name FROM kingres.columnar_check();   -> stripe_beyond_file
A: SELECT kingres.columnar_check_clean();             -> t
A: SELECT count(*) FROM kingres.columnar_check();     -> 0
```

README: "`true` only when every columnar table was checked and nothing was found ... It is never `true` for a table
that was not looked at". It is true here for a table that was listed and whose stripes were not read. The same holds
for a table whose damage is in stripes committed after the snapshot, and for `columnar_check(true)` (it cannot
repair what it cannot see). A check that runs inside `pg_dump`-like consistent transactions, or in a long
REPEATABLE READ session, gives a false all-clear.

A fix must achieve: the check reads the catalog as of now whatever the caller's isolation level (the orphan step
already does: `kc_latest_snapshot`), or refuses with an error in a transaction whose snapshot is not current. Add a
test with an older REPEATABLE READ session and a table made after it.

### C10-02 (medium) The check swallows a query cancel, and waits for the stripe catalog without end

Place: `kc_check_guarded`, `kingres_columnar.c:754-775`: `PG_CATCH` turns every error, a query cancel and a
statement timeout included, into an `unreadable` row (`FlushErrorState`) and the loop goes on. The orphan step
(`kc_catalog_count_orphans`, `kc_catalog.c:910`: `table_open(kc_catalog_oid(), RowExclusiveLock)`) is outside it and
waits for its lock, and `kc_catalog_load` takes `AccessShareLock` on the same catalog.

Steps (18.7): one table `t`; a prepared transaction `BEGIN; LOCK TABLE kingres.columnar_stripe IN ACCESS
EXCLUSIVE MODE; PREPARE TRANSACTION 'cat'` (the sort of lock an administrator's `REINDEX`, `VACUUM FULL` or `LOCK
TABLE` of the catalog holds; ordinary use never does).

```
SET statement_timeout = 4000;
SELECT kingres.columnar_check_clean();    -> still running after 40+ seconds (ungranted RowExclusiveLock on columnar_stripe in pg_locks)
SELECT count(*) FROM kingres.columnar_check();   -> the same, with statement_timeout set again
```

`pg_cancel_backend` sent once during the first wait: the statement stays `active`, waiting (the cancel went into the
`PG_CATCH`); sent a second time it ends with `canceling statement due to user request`. So one cancel and the
whole of `statement_timeout` (it fires once) are lost, and the call then waits for ever. The README says the opposite
for the function ("It never waits for a lock: a table that another transaction holds ... is reported as
`not_checked`"), and its reason for the conditional lock is exactly a prepared transaction that never ends. The
same swallowing makes a user's Ctrl-C end only the table being read, with a row `unreadable: canceling statement
...`, and the check goes on to the next table (the answer is not "clean", so the direction is safe).

A fix must achieve: the catch rethrows query cancel, administrator shutdown and the other errors that are not about
the table; the catalog locks of the check are taken conditionally (or bounded by `lock_timeout`) and a catalog that
cannot be locked is a `not_checked` row. Add a test with a prepared `LOCK TABLE kingres.columnar_stripe` and a
`statement_timeout`.

### C10-06 (medium) Every vacuum of a columnar table cancels queries on a hot standby

Place: `kc_vacuum.c:128`: `ConditionalLockRelation(rel, AccessExclusiveLock)` is taken before `kc_needed_blocks`
decides whether there is anything to truncate (`if (need < nblocks) RelationTruncate`). An `ACCESS EXCLUSIVE` lock
on a logged table is WAL-logged and replayed on a standby, where it conflicts with every query that holds the
table. Heap takes it only when the truncation is worth doing (`lazy_truncate_heap`). The README says the lock is
taken "as heap does".

Steps (18.7 and 19rc2): primary with a standby (`max_standby_streaming_delay = 1s`); the same 100000 rows in a
columnar table `c` and a heap table `h`; both vacuumed once before; a query on the standby that holds the table for
6 seconds (`SELECT pg_sleep(6), count(*) FROM <t>`); `VACUUM <t>` on the primary after 1.5 s, with no change to the
table since its last vacuum.

```
h:  standby query returns 100000
c:  ERROR:  canceling statement due to conflict with recovery
    DETAIL:  User was holding a relation lock for too long.
```

(If rows were inserted before the vacuum, heap's query is cancelled too, with a different cause; the case above has
nothing to prune.) Autovacuum does the same on every visit, which for append-only data happens by the insert
threshold. Long analytic queries on a standby are what the extension is for.

A fix must achieve: decide under the existing `ShareUpdateExclusiveLock` whether the unreferenced tail is worth
removing (the heap rule: a minimum number of blocks or a fraction of the file), take the exclusive lock only then,
and re-check after getting it. Add a standby test with a query that holds the table across a vacuum with nothing to
truncate.

### C10-03 (low) The "decided" rule ignores the nesting level; a call that could still fail the transaction is lost

Place: `kc_write.c:1133-1157`, README "A critical section entered after the extension's own pre-commit callback".
The rule is `IsTransactionState() && CritSectionCount > 0 && kc_commit_flushed`. A callback of a later module can
begin a subtransaction (a PL/pgSQL exception block does); the extension's subtransaction callbacks then run
(`START_SUB`, `PRE_COMMIT_SUB`, `COMMIT_SUB`), so a refusal in a critical section inside it could be remembered
and raised at its pre-commit, as at any other subtransaction. The rule calls it decided.

Steps (`rc10` loaded before the extension; script `pre_commit=sub+crit+ins`, then the same at `pre_prepare`):
`BEGIN; INSERT INTO t VALUES (100); COMMIT;` gives `WARNING: insert refused ... inside a critical section`
(detail: "if the transaction commits (or prepares) it does so without this call"), the commit stands, the module's
row is absent; same for `PREPARE TRANSACTION` / `COMMIT PREPARED`. The same scenario with the module loaded
the other way round raises the error (the pre-commit of the extension comes first there; my module does not catch it,
so that run ended in the server's own `FATAL: EndTransactionBlock: unexpected state END`, which is the module's
doing and not a comparison).

It is a hostile or buggy module and the warning is given, hence low. The README sentence "nothing of this extension
runs again before the commit record" and the comment on `kc_crit_after_own_precommit` are not true for it. A fix
must achieve: either `kc_ending` returns "live" when `GetCurrentTransactionNestLevel() > 1` (then the
subtransaction's pre-commit raises), or the README and the comments say that a subtransaction begun there loses the
call. Add the cell to `t/031`.

### C10-04 (low) `TRUNCATE` puts `relfrozenxid` ahead of the metapage; the log line prints a number that is not stored

Place: `kc_am.c:584` (`*freezeXid = GetCurrentTransactionId()`, also for a truncate, whose new metapage keeps the
older `table_xid`: `kc_am.c:569-575`), `kc_vacuum.c:154,177`.

Steps (18.7), two runs.
(a) A table `tr` created at xid 755, 2000 ids consumed, `TRUNCATE tr`:

```
before any vacuum:  relfrozenxid 2758, kingres.columnar_table_xid('tr') 755    (an id older than relfrozenxid)
```

(b) `autovacuum_freeze_max_age = 100000`, a prepared transaction (xid 758) left open, a table `c_trunc` made
before it and truncated 1000 ids later; autovacuum visits it:

```
autovacuum line:    ... "postgres.kingres.c_trunc": columnar: 10 rows, 2 blocks, relfrozenxid 759, creation id kept
pg_class after it:  relfrozenxid 1762
```

(1) Until the next vacuum `relfrozenxid` claims every id before it is frozen while the metapage holds an older one,
so `datfrozenxid` and the server's warnings run late by the distance between the creation and the truncate
(bounded by about `autovacuum_freeze_max_age` in normal use). When a vacuum can freeze it (no old transaction) it
does at once (checked: manual `VACUUM` gave `table_xid` 2). It is the transient window of the earlier rounds,
extended to `TRUNCATE`. (2) `vac_update_relstats` only moves `relfrozenxid` forward, so the "relfrozenxid 759" that
the new log line prints (the id passed in, not the one stored) is not what `pg_class` says (1762); the README says
the line gives "relfrozenxid X". A fix must achieve: for a truncate, `freezeXid` is the older of the current id and
the normal ids in the old metapage; the line prints the stored value (or is worded as "oldest id still held").

### C10-05 (low) `relation_nontransactional_truncate` can corrupt a table that exists outside the transaction

Place: `kc_am.c:607-620`. PostgreSQL calls it for a table made in the same transaction, or a temporary one; a
module can call it for any table at a moment where an error is allowed. It deletes the catalog rows (transactional),
cuts the file and resets the metapage (not transactional).

Steps (`rc10` after the extension, script `pre_commit=ntrunc,pre_commit=raise`, table of 30000 rows committed
earlier):

```
ERROR:  rc10: raised at pre_commit            (the transaction aborts)
columnar_stripes('t') -> 1 row                (the catalog rows are back)
columnar_check()      -> stripe_beyond_file: stripe 1 lies at blocks 1 to 9, the file has 1 blocks
SELECT count(*) FROM t -> ERROR: columnar table "t" is corrupt: stripe 1: the stripe lies beyond the end of the file
```

Heap loses the rows silently in the same sequence, so this is outside PostgreSQL's contract, and the README
sentence ("an error in the first step is undone ... and leaves a readable table") is true as worded; but it is
exactly "a stripe row without its data" produced by "an error raised by another callback after the extension's".
A fix must achieve: refuse (error where one is allowed, warning where not) unless the relation was created or given
a new file in the current (sub)transaction (`rd_createSubid` or `rd_firstRelfilelocatorSubid` valid), the only case
where an abort removes the file anyway.

### C10-07 (note) Test quality

* `t/031` skips 148 assertions on a build with assertions, in both orders, by `skip ..., 1; ok(1);` (the blocks
  at lines 286, 509 and 544): every read of the table (including the C9-03 conditional-lock cell for
  `columnar_stripes`) is not run there as "PostgreSQL's own rule". The tests of that half of the decision table run
  on production builds only; the README's claims for the reads at forbidden moments are not exercised by the build
  most people test with.
* `t/013` skips with a message when `xid_wraparound` is not installed, which is the case for an installation made
  for an out-of-tree build; the run then ends "All tests successful". It is the only test of the anti-wraparound
  path.
* The new threshold (`age(relfrozenxid) < 160000000` against `autovacuum_freeze_max_age = 150000000`): it cannot
  hide a systematic failure to freeze. A batch moves the age by 100 million; a vacuum that never advances
  `relfrozenxid` passes the first wait and fails the second (age 200 million, past the trigger, so the wait runs for
  its 15 minutes and dies). Checked with a mutant that passes `InvalidTransactionId` to `vac_update_relstats` (the
  table's frozen id never moves): `t/013` was still waiting after 400 s, against 101 s for the passing run. What it
  cannot see: a vacuum that advances to the wrong id (the `columnar_creating_xid`/`columnar_table_xid` checks after
  each lap cover the metapage), `relminmxid` (not read anywhere in the test), the converted table `c0` and the heap
  twin's age (never waited for), and a run that never starts a vacuum below 160 million (the only assertion there
  is "not above").
* Fixed sleeps: `t/001` (50 ms), `t/002` (20 ms), `t/005` (250 ms) are poll steps; `t/003` (`pg_sleep(3)`) and
  `t/027` (`pg_sleep(4)`) hold a session open while the test polls `pg_stat_activity`; none decides a verdict by
  its length.
* `t/012` reads `server_version_num` in Perl (a test, not the source); the C and SQL carry no version test outside
  `src/pg_compat.h`; the test module has its own compat header.

### C10-08 (note) What "clean" covers

`columnar_check_clean()` is true when the catalog and the first page of each stripe agree and no catalog rows are
orphaned; it does not read the other pages of a stripe, and a table created during the call is not in its list
(the list is made first). The check keeps an `AccessShareLock` on every columnar table until the caller's
transaction ends (the subtransaction's locks pass to the parent), so in a transaction block it blocks DDL on all of
them, and it keeps every table's stripe list in memory until the call ends. The README should say these.

## Questions the task asked, answered

**A critical section after the extension's pre-commit / an "alive" module that does not fail the transaction.**
With `rc10` in both load orders on both majors: after the extension's callback (module before it in the preload list)
the call is refused with a warning, never silently, the transaction commits without it, nothing half-applied (rows
unchanged for `ntrunc`, the module's row absent for the insert); at prepare the same, `COMMIT PREPARED` then
commits without it. Before the extension's callback (module after it in the list) the transaction fails at the
extension's pre-commit or pre-prepare, 0 prepared transactions, rows 1..5. An error raised by a module's callback
after the extension's (`raise` at `pre_commit` and `pre_prepare`, with and without a refused call and with savepoints
and a 200000-row insert whose stripes were already written): abort, rows unchanged, `columnar_check_clean()` true,
one stripe, the next transactions normal (the state `kc_commit_flushed` is reset at the abort). I found no sequence
with silent loss and none with a stripe row without data or data without a stripe row, except through C10-05 (a
call outside PostgreSQL's contract) and the loss with a warning of C10-03.

**`columnar_check_clean()` true for something not checked.** Yes in an old snapshot (C10-01). Not for: a table
locked by another session (an uncommitted `DROP TABLE` in another session, a prepared `LOCK TABLE ... ACCESS
EXCLUSIVE`; `not_checked`, false), a table in another schema or owned by another role (checked, true when
sound), a partition of a partitioned table (checked), a materialized view `WITH NO DATA` (true, no spurious row), a
table with a prepared transaction's rows (true; the in-doubt stripes are invisible to any check, as to any reader),
a temporary table (cannot exist: `ALTER TABLE ... SET ACCESS METHOD columnar` on a temporary heap table gives
`columnar tables cannot be temporary or unlogged`). A call at a forbidden moment returns false with a warning.

**Non-superusers.** `columnar_check`, `columnar_check_clean` and the other write-side functions are revoked from
PUBLIC (`permission denied for function`); after a `GRANT EXECUTE` the function still says `must be superuser to run
columnar_check_clean`. Nothing leaks.

**Reading functions that ask for the lock without waiting.** `columnar_identity` called by `rc10` at the commit and
at the abort event on a table that another session held `ACCESS EXCLUSIVE`: `WARNING: columnar_identity refused: the
table is locked exclusively ...`, a null answer, no crash, transaction commits. When the lock is free the same
call at the commit event stops the server with the relation cache assertion (`IsTransactionState()`,
`relcache.c:2126`), which is the README's stated PostgreSQL rule for reads there, not a defect of the extension.

**The autovacuum line and statistics.** An anti-wraparound autovacuum visit logged
`automatic aggressive vacuum to prevent wraparound of table "postgres.kingres.c_empty": columnar: 0 rows, 1
blocks, relfrozenxid 758, creation id frozen` (and one such line for each columnar table, `columnar_stripe` and the
catalogs); a table with a name containing `%` and `"` vacuumed without a problem; `n_live_tup`, `reltuples`,
`vacuum_count`, `autovacuum_count`, `last_autovacuum` are set; `ANALYZE` alone changes no `relfrozenxid`;
`VACUUM FULL` and `VACUUM (FREEZE)` set it; a manual `VACUUM` prints no line (as for `log_autovacuum_min_duration`
in heap); mutant m905 (the line never printed) is killed by `t/035`. The number the line prints is not always the stored one (C10-04).

**Does an anti-wraparound autovacuum advance `relfrozenxid` and `relminmxid` in every case?** Tried
(`autovacuum_freeze_max_age = 100000`, 250000 ids consumed with `consume_xids`): an empty table, a table with only
aborted stripes (one block left: the aborted tail is truncated), a converted table, a truncated table, a table with
a prepared transaction's rows: all visited, all aged 251167 at first, all held back by the prepared transaction's id
exactly like the heap table beside them (`relfrozenxid` 758, the prepared id); after `COMMIT PREPARED` and another
250000 ids all columnar tables and the heap table were at age 1 to 5. Concurrent writes do not touch the metapage
ids; `t/013` passes on both majors, two laps. `relminmxid` with `autovacuum_multixact_freeze_max_age = 10000`:
multixact age reached 5870 (below the trigger); autovacuum had visited the columnar and heap tables and all of them
stood at the same `mxid_age`, i.e. it moves with the heap's. I could not build a table whose frozen id never advances
by anything except what holds the heap table back too (a prepared transaction, a long transaction, a replication
slot) or a corrupt metapage or catalog (the vacuum then raises every time).

## What held under attack

* The decision cells of the extension (insert and truncate in a critical section, at commit, prepare, abort, commit
  and abort of a subtransaction; before and after the extension's callback) on both majors with identical results.
* Two-phase commit with a refused call, an error after the extension's callback, and a later `COMMIT PREPARED`;
  `kc_commit_flushed` state across commit, prepare and abort: the next transaction is normal in every run.
* An allowed non-critical write by a later module's callback at `pre_commit` or inside a subtransaction begun there
  is carried out (written at once), rows present once.
* Own data, seed 11, on 18.7 and 19rc2: 250 random transactions (about 49000 rows kept, 10 stripes) against a heap
  twin: after the load same count, sum and md5 of every row in id order; after an immediate stop and crash recovery
  the same; a base backup restored as a standby the same; after more writes on the primary the standby the same;
  after `pg_ctl promote` the same, a write on the promoted node works and `columnar_check_clean()` is true;
  `pg_dump` into a fresh database the same; a REPEATABLE READ cursor read 100 rows and `MOVE FORWARD ALL` then moved
  49287 (= 49387 - 100) while another session ran `columnar_compact`, `VACUUM` and `VACUUM FULL`; the final table
  had the heap's 49387 rows and a clean check. (Seed 7 on 18.7 showed one difference after my own fixture inserted
  `now()` in two separate statements: fixture error, rerun with a constant timestamp, same.)
* SERIALIZABLE: only through the suite (`t/009`, passes); I did not write my own.

## Verdict

For data that matters, within what the extension claims (append-only analytic data, insert-only, no indexes, serial
scan): in my own runs on 18.7 and 19rc2 I found no wrong answer, no lost or duplicated row and no unreadable table in
ordinary use, through crash recovery, a promoted standby, base backup, dump and restore, aborted subtransactions,
compaction and `VACUUM FULL` under a reader, nor any way for the new "decided, not alive" rule to lose data without
a warning. The round did what it said for C9-01 to C9-06, and mutants of five of the six are killed by tests. The
extension is usable on a primary for such data. It is not ready to be called finished for operations: (1) the strict
check can say clean for a damaged table and can hang beyond a timeout (C10-01, C10-02), so a monitor must not be
built on it yet; (2) on a hot standby every vacuum of a columnar table cancels the long queries that are the point of
the table (C10-06); fix these two before a beta. C10-03 to C10-05 are corners of hostile modules and
a bookkeeping gap that heals at the next vacuum.

## What I did not check

* A production build (no assertions): everything here ran with assertions, where 148 assertions of `t/031` are
  skipped; the reads at forbidden moments in a production build were not run.
* SERIALIZABLE anomalies with my own workload; logical decoding beyond `t/034`; `pg_upgrade`; `pg_rewind`; data
  checksums; `wal_consistency_checking`.
* A second lap of `t/013` with different settings, and `relminmxid` past its trigger (the multixact age I reached
  was 5870 of 10000).
* Databases with thousands of tables or very many stripes (memory and locks of the check); more than one database
  in the cluster.
* The ordering of callbacks with three modules at once; parallel workers and the `PARALLEL_*` events; `START_SUB`
  with a module loaded before the extension (the README states the FATAL).
* The `xid_wraparound` run on a build where the module is missing from the start (I copied the module in).
* The text of the README beyond the decision table, the paragraph on autovacuum and the `check` entries.
