<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
# Adversarial review of the fifth fix round: contrib/kingres_columnar 0.1

Reviewer: an independent reviewer (not the author). The change under review
answers `kingres/docs/reviews/2026-11-16-kingres-columnar-0.1-fourth-fixes.md`
(C5-01 to C5-08). The earlier reviews are
`kingres/docs/reviews/2026-11-15-kingres-columnar-0.1.md` (C1-01 to C1-14),
`kingres/docs/reviews/2026-11-15-kingres-columnar-0.1-fixes.md` (C2-01 to
C2-09), `kingres/docs/reviews/2026-11-16-kingres-columnar-0.1-second-fixes.md`
(C3-01 to C3-06) and
`kingres/docs/reviews/2026-11-16-kingres-columnar-0.1-third-fixes.md` (C4-01 to
C4-06). The reviewed state (the fix round and the directory around it) was
exported with `git archive` (paths `contrib/kingres_columnar`, `kingres/dev`,
`kingres/docs`) into a private work area; the working tree was not used for any
result and nothing in the repository was changed except this file. Line numbers,
where given, are those of that export.

## Summary

The five central fixes of the round hold against what I could build, and the
round is the first whose tests are, for the most part, tests that fail when the
fix is removed. I found no wrong answer from the table-creation rule (C5-02), no
row lost or doubled by the second write buffer, the hand-over, the cleanup after
a rewrite or the repair of the earlier catalog, and nothing that crashes the
server through a path core PostgreSQL itself runs. The pattern of the earlier
reviews repeats in a milder form: each new mechanism is right for the shapes the
author tried and has an edge that user code, or another extension, reaches.

* The new two-phase rule for rows in the window of a hand-over refuses a write to
  the relation being handed over, but not a write to **another** columnar
  relation. That write can raise the memory over the allowance, and the memory
  pressure flush then writes the very buffer that is waiting to be published: the
  server stops at an assertion (assertion builds) or, with the assertion
  compiled out, the child's rows are in the table twice (7 000 rows for 4 000,
  committed). Only code of another extension can run in the window (C6-02).
* The commit path has the same kind of hole one step later: a callback of
  another extension that writes to a columnar table at `XACT_EVENT_PRE_COMMIT`
  (or `PRE_PREPARE`) after this extension's own pre-commit flush has run loses
  its rows without an error: the row is neither written nor refused, and the
  transaction commits. Heap keeps the row (C6-01).
* The memory bound stated in the README is not a bound: it is per level of
  subtransaction (and, for cursors, per cursor), and the number of levels and of
  cursors a role can open is not limited (C6-03).
* Three mechanisms assume the event triggers of the extension run, or that the
  database was created by the new install script. With `event_triggers = off`,
  a disabled trigger, single-user mode, or a database whose script is the one of
  the previous round, a change away from columnar leaves stripe rows behind
  (harmless to readers, cleaned by `VACUUM`), and the old, linear and
  contending cleanup stays (C6-04, C6-05).
* Tests: the table-creation, window and freeze tests (`t/021`, `t/022`, `t/002`,
  `t/013`) fail for every mutant I built of their fix. The new wide-row memory
  cases of `t/017` kill none of the five mutants of C5-01 that I built (`t/022`
  or `t/014` kill three of them with one or two checks; no file of the round
  kills the other two), and `t/023` kills neither the mutant that makes the
  cleanup delete the stripe rows of a file that was not replaced (that mutant
  empties a table in an ordinary failed-rewrite-in-a-savepoint case) nor the one
  that moves the end-of-command cleanup to the commit (C6-07).

| Severity | Count |
|---|---|
| critical (wrong answer, lost or doubled committed rows, crash, a SERIALIZABLE anomaly heap prevents, an unreadable table) | 0 |
| high | 1 |
| medium | 2 |
| low | 3 |
| note | 1 |

The two code defects that lose or double rows (C6-01, C6-02) need code of another
extension that runs inside the commit of a transaction or subtransaction; nothing
in the KingresDB tree has such code (the transaction callbacks of the other
extensions do not write local tables at those events). By the letter of the
definition of "critical" a lost or doubled committed row is critical; I count
C6-01 as high and C6-02 as medium for that condition and say so here so that the
owner can decide otherwise.

| Id | Severity | Component | Statement |
|---|---|---|---|
| C6-01 | high | `kc_write.c` `kc_xact_callback`, `kc_write_row_values` | Rows written to a columnar table by another module's `XACT_EVENT_PRE_COMMIT` / `PRE_PREPARE` callback that runs after this extension's flush are discarded at `XACT_EVENT_COMMIT`/`PREPARE` with no error. |
| C6-02 | medium | `kc_write.c` `kc_flush_pressure`, `kc_buf_flushable`, `kc_subxact_publish` | A write to another columnar table in the window of a hand-over can flush the buffer that is prepared for the hand-over: assertion failure (server stops) or, without assertions, the child's rows twice. |
| C6-03 | medium | `kc_write.c` allowance, README "Memory" | No bound on the memory of the write buffers: levels of subtransaction (12 levels: 687 MB against 128 MB) and several open cursors (40 cursors: 114 MB against 8 MB) each add to it. |
| C6-04 | low | `kingres_columnar--0.1.sql`, `t/023` | A change away from columnar leaves the old file's stripe rows when the table_rewrite trigger does not run (`event_triggers = off`, trigger disabled, single-user mode); `t/023` says the opposite for "event triggers off". |
| C6-05 | low | install script, README | A database created by the previous round's script has the new library with the old script: no table_rewrite trigger, the linear cleanup of every `ALTER TABLE` and its contention (C5-04, C5-05) stay; `ALTER EXTENSION UPDATE` has nothing to do. |
| C6-06 | note | `columnar_identity`, `columnar_creating_xid`, `columnar_table_xid` | Any role can read the identity and the creation ids of any columnar table. |
| C6-07 | low | tests | Five of the mutants I built survive every test file of the round that claims the fix, among them one that empties a table and one that flushes a buffer under an open cursor; the probe module cannot reach the commit path. |

## Fate of C5-01 to C5-08

| Id | Fate | Evidence |
|---|---|---|
| C5-01 | Fixed for the reported shape; the bound itself is not one (C6-03) | The loop of exception blocks with rows of 100 kB: 3 000 blocks under 8 MB end with 5.7 MB of write buffers, resident set +32 MB (before: 954 MB at the default setting in the review); 1 MB rows, 400 blocks: 3.8 MB; 10 MB rows, one statement: 0 MB at the end, +50 MB resident (the stripe being built, not counted, stated in the README). COPY of 1 500 rows of 100 kB with a BEFORE ROW trigger sampled at every row: 7 MB. Both majors equal. |
| C5-02 | Fixed | 12 command sequences after the snapshot (conversion alone, after `GRANT`, owner, storage option, `ADD COLUMN`, row security, `REPLICA IDENTITY`, rename, `SET SCHEMA`, 300 grant/revoke cycles, `VACUUM`, `VACUUM FULL pg_class`, `VACUUM (FREEZE) pg_class`, TRUNCATE, VACUUM FULL, back to heap and again) at REPEATABLE READ and SERIALIZABLE, an exported snapshot, a cursor `WITH HOLD`, a table made after the snapshot in a prepared transaction, in a subtransaction, a hot standby with and without `hot_standby_feedback`: the answer was always the error (40001), the rows, or an empty table where the table did not exist for the snapshot; never an empty table for content that existed. Details under "What held". |
| C5-03 | Fixed for a read in the window and for a write to the relation being handed over; a write to another relation under memory pressure is not (C6-02) | Window mutants die in `t/022`. The window is not reachable in core PostgreSQL: between `PRE_COMMIT_SUB` and `COMMIT_SUB`, `CommitSubTransaction` runs a command counter increment, `AfterTriggerEndSubXact` (moves events, fires nothing), `AtSubCommit_Portals` and the large object and notify hooks. |
| C5-04 | Fixed for `ALTER TABLE`; `DROP TABLE` still linear (stated) | `ALTER TABLE ... SET DEFAULT` on a heap table: 0.58 ms with 0 columnar tables and 0.52 ms with 3 000 (0.25 ms with the trigger off, so about 0.3 ms for the triggers, as stated). `CREATE`+`DROP` of an unrelated heap table: 1.14 ms with 0 columnar tables, 12.75 ms with 3 000; one `DROP` of a heap table 6.6 ms and 22.8 to 26 ms with 4 000. Two concurrent drops: no error, no leftover. |
| C5-05 | Fixed | `ALTER TABLE` of unrelated tables, `columnar_reconcile()` and `VACUUM` while another session holds a rewrite open: no wait, no error; with a mutant that waits (`wait = true`) `t/023` fails and then does not finish by itself within eight minutes. The `TM_Deleted` branch has no test (C6-07). |
| C5-06 | Fixed | A database written by each of three earlier builds (two with the 13-column catalog, one with the 14-column catalog) was read by this build; the 13-column ones were refused with the statement, which was run: every row of six tables came back (sums equal), the flag `maintenance` is false on all 13 old rows, the prepared transaction of the old build committed, writes, `VACUUM`, compaction, TRUNCATE, rewrites and a drop worked and `columnar_check` ended clean. The hint no longer says to drop the extension (mutant: `t/023` fails 6 checks). |
| C5-07 | Mostly fixed | `t/002` now fails by assertions (2 of 75) when the freeze conflict is removed, not by a `die`; `t/013` and `t/021` both fail when either id of the metapage is not frozen; `t/019` states exact answers; `t/016` arms the fault inside the block (control present). Not fixed in spirit: C6-07. |
| C5-08 | Fixed | The text of `stripe_over_row_limit` in `kc_check_one` and in the README names the developer setting. |

## Scope and method

Read: the whole diff of the round, `kc_write.c` in full (buffers, accounting,
flush, holds, hand-over, callbacks), the table-creation rule in `kc_am.c`,
`kc_storage.c` (`kc_check_snapshot_age`, `kc_table_existed_for`, the freeze and
its redo), the changes of `kc_catalog.c`, `kc_scan.c`, `kingres_columnar.c`, the
install script, the README and the test files of the round
(`t/017`, `t/021`, `t/022`, `t/023`, the changes in `t/002`, `t/013`, `t/014`,
`t/016`, `t/019`, and the probe module `t/kcprobe`). PostgreSQL's own
`CommitTransaction` and `CommitSubTransaction` were read to see what runs between
the callbacks.

Built and run on PostgreSQL 18.7 and 19rc2, both with assertions, each copied into
a private area with the TAP harness pointed at my own range of ports and used out
of tree with `kingres/dev/ext.sh` (`EXT_WERROR=1`, no warning, 11 regression tests
and the TAP files). The test module `xid_wraparound` of the PostgreSQL tree was
built and installed into my copies so that `t/013` could run. Mutants are copies
of the extension with one change each, built into a second copy of the
installation and run with the one test file that claims the fix; experiments are
TAP scripts with `PostgreSQL::Test::Cluster` and `BackgroundPsql`, or short shell
scripts for a database written by an earlier build and for single-user mode. All
work ran under `nice`, with at most 4 jobs, on ports 59900 to 59999; every
server I started is stopped.

| Experiment | What was run |
|---|---|
| E1 old snapshots | Twelve command sequences after an older REPEATABLE READ or SERIALIZABLE snapshot, each against a heap twin; five "table made after the snapshot" shapes (prepared, savepoint); exported snapshot; `WITH HOLD`; both majors. |
| E10 standby | Old snapshot on a standby, four shapes, `hot_standby_feedback` on and off, `VACUUM FULL pg_class` and `VACUUM (FREEZE) pg_class` on the primary. |
| E2, E2b, E2c, E2d memory | Real memory (`VmHWM` of the backend) and the memory contexts of the buffers: wide rows (100 kB, 1 MB, 10 MB), exception loops, 5 to 60 nested savepoints, 12 levels of 60 MB, 40 cursors, 2 000 and 8 000 tables, 4 000 partitions, four tiny text columns, COPY with a BEFORE ROW trigger, self-feeding INSERT SELECT; a heap twin for the many-tables case. |
| E3 differential fuzz | Random sessions (BEGIN, savepoints, release, rollback to, inserts of 1 to 2 500 rows of 10 to 100 000 bytes, `INSERT ... SELECT` from the same table, exception loops that fail every seventh row, plain and `WITH HOLD` cursors with partial `FETCH`, `COMMIT` and `ROLLBACK`) against a heap twin in the same transaction, `write_buffer_memory` 1 to 128 MB, stripe size 500 to 2 000; a variant with `ADD COLUMN` (constant and volatile default) and `DROP COLUMN` between transactions. About 850 episodes on 18.7 and 1 310 on 19rc2, 0 differences; about 15 percent of the episodes ended early on an error that both twins raise (a cursor that a rollback to a savepoint closed). |
| E4 rewrites | 16 ways to replace a file (event triggers off, the trigger disabled, the other trigger disabled, all disabled, replica role, in a function, TRUNCATE, VACUUM FULL, savepoint rollbacks, PREPARE then commit or roll back, partitioned table, partition, `SET ACCESS METHOD`), what is left in the catalog afterwards and whether the table reads; the privileges of the functions behind the trigger for an unprivileged owner; cost of `DROP`, `ALTER TABLE`; two concurrent drops. |
| E5 pre-commit writer | A module (30 lines) whose `XACT_EVENT_PRE_COMMIT`/`PRE_PREPARE` callback runs a statement, loaded before and after this extension; plus a deferred constraint trigger. |
| E6 WAL | Primary and hot standby with `wal_consistency_checking = all`: writes in nested subtransactions, rollbacks, exception loops, dropped and added columns, a partitioned table, conversion both ways, rewrite, VACUUM FULL, compaction, TRUNCATE, prepared transactions, an immediate stop with a prepared transaction open, recovery, promotion and writes on the promoted node. |
| E7 misc | BEFORE ROW triggers that write or read the same table, UPDATE/DELETE with pending rows, TRUNCATE and ALTER with an open cursor, an error in the middle of an `INSERT ... SELECT` with buffered rows, PREPARE with two buffers, two `WITH HOLD` cursors on two buffers, each against a heap twin. |
| E8 earlier builds | A database written by three earlier builds read by this build, the statement of the hint run, then the same workload. |
| E9, E11, E12 | Cost per statement; single-user mode; the server restarted without the library in `shared_preload_libraries`. |
| E13, E14 | The cleanup of noted files in a failed rewrite inside a savepoint (with mutants); a write to another table in the window (with and without the assertion). |

## The tests of this round

Method for each fix: a copy of the extension with the fix broken in one place and
the test file(s) of the round that claim it. A mutant "fails" when the file
reports failed checks or dies. Numbers are failed checks, equal on 18.7 and
19rc2. Each file passes on both majors without the mutant (baseline below).

### `t/021_table_creation.pl` (C5-02)

Claims: a snapshot that could have seen content never reads an empty table in
its place after any command that rewrites `pg_class`; a table made after the
snapshot reads empty whatever happens to it; the creation recorded in the
metapage for each source; both ids frozen by `VACUUM`; an old metapage with 0.

Preconditions: established. Every scenario runs the same commands against a heap
twin and asserts the heap answer (0), so the snapshot is known to be older than
the rewrite; the old snapshot is a REPEATABLE READ transaction that reads another
table first; part 4 edits the metapage of a stopped cluster and asserts that
`columnar_table_xid` reads 0 afterwards.

| Mutant | Result |
|---|---|
| the rule of the earlier round back (creation = xmin of the `pg_class` row, shortcuts removed) | 7 or more failed (`GRANT`, owner, fillfactor, autovacuum, `ADD COLUMN`, row security, `REPLICA IDENTITY`) |
| the reader never asks the catalog (`table_xid = 0` always gives the error) | 5 failed |
| the "no storage" shortcut removed | 4 failed |
| the "made in this transaction" shortcut removed | 1 failed (a metadata check; the behaviour is equal, because a table made in the same transaction is also invisible to the older snapshot) |
| `table_xid` never frozen | 1 failed; `t/013` also fails |
| `created_xid` never frozen | 2 failed; `t/013` also fails |

What it does not cover (my experiments E1 and E10 do, and found nothing):
`VACUUM FULL pg_class`, `VACUUM (FREEZE) pg_class`, rename, `SET SCHEMA`, many
updates of the row, an exported snapshot, `WITH HOLD`, a standby. `t/019` does
not kill the old rule (it never converts a table), as the previous review said.

### `t/022_handover_window.pl` and the probe (C5-03, C5-01 part)

Claims: a reader in the window sees each row once, a write to the relation is
refused, a cursor opened in the window cannot go on, and the memory in the window
is not a copy of a big child.

Preconditions: established. The probe is asserted to have run exactly once (the
test compares the list of callback outputs with `1:<expected>`), the setting is
cleared before the next statement, a control shows another table is writable, and
the file skips itself with a reason where it cannot build the module.

| Mutant | Result |
|---|---|
| visible part starts at 0 (the reader sees the rows twice) | 4 failed |
| the write in the window is not refused | 3 failed (the file then ends early at 29 of 45 checks) |
| the cursor that began in the window is not stopped | 1 failed |
| the child's own big buffer is not written by the child (`kc_flush_own_buffer` off) | 1 failed (the memory in the window, 13.0 MB for an 8 MB allowance) |
| `kc_worth_flushing` looks at rows only | 1 failed (same assertion) |

What it does not cover, and what I found: a write to **another** relation in the
window under memory pressure (C6-02), a callback at `XACT_EVENT_PRE_COMMIT`
(C6-01: the probe registers only a subtransaction callback), a read in the window
of a table with a pinned buffer, a nested hand-over of three levels in the window.

### `t/017_stripe_sizes.pl` (C5-01)

Claims: the write buffers stay within the allowance for rows of 100 kB, in loops
of exception blocks, three levels deep, with a cursor, over 24 tables; no stripe
but the last is small.

Preconditions: established for the bounds (the allowance is 8 MB, the peak is
measured from the memory contexts of the buffers after every statement and every
block, the reference shape is asserted first). The numbers are within the stated
bounds in the unmodified build.

| Mutant | Result |
|---|---|
| `kc_worth_flushing` looks at rows only | passes; killed by `t/022` (1 check) |
| the child's own big buffer is copied into the parent's | passes; killed by `t/022` (1 check) |
| accounting counts the payload (`*sz`) in place of the memory of the chunk | passes; killed by nothing. The effect I could measure with four tiny text columns was 10 MB against 8 MB, so the mutant is nearly equivalent, but the claim "the accounting counts real memory" is tested by no file |
| a buffer that a cursor reads takes more rows | passes; killed by `t/014` (2 checks) |
| a buffer that a cursor reads is flushed (`refs` not looked at) | passes `t/014`, `t/017` and `t/022`; my fuzz (E3) shows wrong table contents with this mutant in at least 6 of 150 episodes (a `count(*)` of the table that is short of rows, for example 555 for 564) |

So the claim of the commit that the tests "make the mutants fail" holds for
`t/022` and `t/014` and not for the new cases of `t/017`: the wide-row bound is
satisfied also by the broken builds, because the bytes rule of `kc_flush_pressure`
(half of the allowance in the flushable buffers) is enough in every case the file
builds. The file does establish its measure and its reference shapes; what it
lacks is a case where only the bytes rule or only the pinned-buffer rule keeps the
bound.

### `t/023_rewrite_cleanup.pl` (C5-04, C5-05, C5-06)

Claims: after each command that replaces a file nothing is left; an unrelated
`ALTER TABLE` deletes nothing (not even an orphan); unrelated commands do not wait
or fail; a row that another session deleted is not an error; the earlier catalog
is refused with the repair and the repair works.

Preconditions: established. Each case asserts the table had at least 5 stripe
rows, that `columnar_check` is empty before the command, and checks the rolled
back form too; the orphan is made by hand and asserted; the earlier shape is
asserted to have 13 columns.

| Mutant | Result |
|---|---|
| the table_rewrite trigger notes nothing | 2 failed (`SET ACCESS METHOD heap`) |
| the cleanup deletes the rows of every noted file, whether replaced or not | **passes** |
| the end-of-command cleanup (`columnar_finish_rewrites` in the trigger) removed | **passes** (the commit-time cleanup does the work) |
| the reconcile waits for a row that another session is deleting | fails 2 and then does not finish by itself within the several minutes I allowed (I stopped it) |
| the `TM_Deleted` result is an error | **passes** |
| the hint tells to drop the extension | 6 failed |

The surviving mutants are the ones that matter:

* "delete the rows of every noted file": the comparison of the noted file with the
  file the table has at the end is the safety of the whole mechanism, because a
  rewrite that fails after the file was noted leaves the note behind. With this
  mutant, on both majors, the commands

  ```
  BEGIN;
  DO $$ BEGIN BEGIN ALTER TABLE x ALTER COLUMN b TYPE int2 USING (b / (b - 4000));
        EXCEPTION WHEN division_by_zero THEN NULL; END; END $$;
  ALTER TABLE x ADD COLUMN c int;
  COMMIT;
  SELECT count(*) FROM x;
  ```

  leave `x` with 0 rows (5 000 before). The test of the failing rewrite
  (`ALTER ... USING (b / (b - 4000))`) runs outside a transaction block, so the
  whole transaction aborts and the deletion is rolled back: it cannot distinguish
  the mutant. The same happens with a savepoint rolled back around a rewrite when
  the check_ddl trigger is disabled. In the unmodified build both give 5 000.
* "no end-of-command cleanup": inside a transaction block the old file's rows are
  still there after the command (5 rows) and are removed only at commit. The
  README and the commit message say "at the end of the `ALTER TABLE` command or
  at its pre-commit", so the behaviour is as stated, but the test cannot tell the
  two apart.

What it says and does not do: the comment "a session that turned the event
triggers off leaves nothing either" is shown only for a rewrite from columnar to
columnar; for `SET ACCESS METHOD heap` the rows stay (C6-04).

### `t/002_standby.pl` (C5-07, freeze conflict), `t/013`, `t/014`, `t/016`, `t/019`

* `t/002`: with the conflict removed from the redo routine the file now fails 2
  checks of 75 (before: it died in a helper). The last `unlike` still passes for
  a dead session, but the new `like` on the reason does not.
* `t/013` runs only where the `xid_wraparound` module exists; with it (built by
  me) 23 checks pass in 67 s on both majors; with `table_xid` or `created_xid`
  never frozen it fails (exit status 1 after the poll for the age gives up), and
  `t/021` fails first, in 3 seconds, so the claim is now also tested without the
  module.
* `t/014`: the changed expectation (stripes written while a cursor is open) is
  right and is killed by the mutant "the cursor's buffer takes more rows"; it does
  not detect a buffer flushed under the cursor.
* `t/016`: read, not mutated this round. The arming inside the block (set the
  point as the last statement of the block whose end is the hand-over) does count
  only allocations of the hand-over; the control arms it where no subtransaction
  ends and sees no failure; at least five places fail per point. The previous
  weakness is gone.
* `t/019`: `is($col, '0', ...)` for the change to heap and `is(..., '2')` for
  `VACUUM FULL` are exact now. The file still never converts a table.

### Gaps that no test file of the round covers

* A callback of another module at `XACT_EVENT_PRE_COMMIT` / `PRE_PREPARE` that
  writes a columnar table (C6-01).
* A write to another relation, or memory pressure of any kind, in the window
  (C6-02).
* More than one level of subtransaction, or more than one cursor, holding the
  allowance (C6-03).
* A rewrite that fails or is rolled back inside a transaction block followed by a
  commit (C6-07).
* `event_triggers = off`, a disabled trigger, single-user mode for a change away
  from columnar (C6-04).
* A database whose script is the old one (C6-05).

## Findings

### C6-01 (high) Rows written after the pre-commit flush are lost without an error

Place: `kc_write.c`, `kc_xact_callback` (line 1887): at `XACT_EVENT_PRE_COMMIT`
and `PRE_PREPARE` it calls `kc_flush_all()`; at `XACT_EVENT_COMMIT`, `PREPARE` and
`ABORT` it calls `kc_state_reset()`, which deletes the memory context of every
buffer. `kc_write_row_values` (line 815) has no notion that the flush has run, so
a row written after it is buffered, never written, and dropped at commit.
Callbacks run last-registered first, so a module that is listed before this one in
`shared_preload_libraries` has its `XACT_EVENT_PRE_COMMIT` callback run after the
flush. PostgreSQL permits such a callback to run SQL (`postgres_fdw`-like and
auditing code do).

Steps (a 30-line module whose callback runs the statement of a setting through
SPI at `XACT_EVENT_PRE_COMMIT` and `PRE_PREPARE`; `shared_preload_libraries =
'probe,kingres_columnar'`; 18.7 and 19rc2 alike):

```
CREATE EXTENSION kingres_columnar;
CREATE TABLE p (a int) USING columnar;  CREATE TABLE h (a int);
SET probe.sql = 'INSERT INTO p VALUES (777)';
BEGIN; INSERT INTO p VALUES (1); INSERT INTO h VALUES (1); COMMIT;
SET probe.sql = 'INSERT INTO h VALUES (777)';   -- the same on the heap twin
BEGIN; INSERT INTO h VALUES (2); COMMIT;
SELECT * FROM p;  SELECT * FROM h;
```

Observed: `p` holds `1`; `h` holds `1, 2, 777, 777` (the `SET` itself is a
statement of its own and runs the callback too). With the library order reversed
(`kingres_columnar,probe`) the callback runs before the flush and `p` holds
`1, 777, 777`. A `PREPARE TRANSACTION` with the same callback loses `888` in the
same way, and `COMMIT PREPARED` shows only the rows written before the flush. No
warning, no error, the commit returns normally.

Expected: the row is committed, or the write is refused with an error. Where the
right answer cannot be given, an error is. The order of two libraries in a
configuration file must not decide whether committed rows exist.

Direction: after `kc_flush_all()` set a flag that `kc_write_row_values` tests
(`ERROR: columnar table "p" cannot be written after the pre-commit flush`), and
reset it in the commit, prepare and abort callbacks. An error is safe there: it
aborts the transaction. A flush at `XACT_EVENT_COMMIT` is not possible (the
transaction is already committed). Extend the probe module with a callback at the
two events and add the case to `t/022`.

### C6-02 (medium) A write to another table in the window flushes the buffer being handed over

Place: `kc_write.c`: `kc_flush_pressure` (line 691) picks the largest buffer for
which `kc_buf_flushable` (line 604) is true: `refs == 0 && subid ==
GetCurrentSubTransactionId()`. Between the two phases of a hand-over the current
subtransaction is still the committing child, so the child's buffer that phase
one has copied into the parent's slots (`merge_into` set, `merge_rows` fixed) is
flushable. `kc_write_row_values` refuses a write to a relation that has a
prepared pair (line 831) but a write to **another** relation runs the pressure
loop, which can write and clear the child's buffer. Phase two (`kc_subxact_publish`,
line 2391) then publishes `merge_rows` rows that are already in a stripe.

Steps (the probe module of `t/kcprobe`, loaded before this extension; four
columnar tables `p`, `q1`, `q2`, `q3`; `p` has a stripe size of 100 000 rows;
18.7 and 19rc2 alike):

```
SET kingres_columnar.write_buffer_memory = '8MB';
BEGIN;
INSERT INTO p SELECT g, repeat('x', 1000) FROM generate_series(1, 1000) g;
SAVEPOINT s;
INSERT INTO p SELECT g + 100000, repeat('y', 1000) FROM generate_series(1, 3000) g;
SET kcprobe.sql = 'INSERT INTO q1 SELECT g, repeat($$z$$, 1000) FROM generate_series(1, 2200) g;
                   INSERT INTO q2 SELECT ...;  INSERT INTO q3 SELECT ...';
RELEASE s;
SELECT count(*), count(DISTINCT id) FROM p;
```

Observed (assertions on): `TRAP: failed Assert("b->merge_into == prev && b->merge_rows == b->nrows && b->merge_base == prev->nrows"), File: "src/kc_write.c", Line: 2393`; the backend is aborted and the server restarts every connection. With the same statements and the assertion compiled out (a copy with that one `Assert` turned into a no-op, which is what a build without assertions has): the transaction commits and `p` has 7 000 rows for 4 000 distinct ids (`IN_TXN=7000/4000`, `FINAL=7000/4000`): the child's 3 000 rows are in a stripe and in the parent's buffer. With no write in the window (control) the result is 4 000 and 4 000.

Expected: no row doubled, no crash. The commit message says "a write to such a
relation in that window is refused", and that is true of the relation, not of the
transaction's state.

Direction: make `kc_buf_flushable` false for a buffer with `merge_into != NULL`
(and for the parent's buffer of a prepared pair), or skip `kc_flush_pressure`
while `kc_handover_prepared` is set; add the case to `t/022`.

### C6-03 (medium) The write-buffer memory is not bounded: levels of subtransaction and several cursors

Place: `kc_write.c`, the rule that a buffer of an enclosing level cannot be
written by an inner one (a stripe carries the id of the subtransaction that
writes it) and that a buffer a cursor reads is not written. The README states
this ("an open level that holds rows adds less than half of A"; "A plus what the
cursor holds") and the commit lists it as left open; I measured how far it goes.

Measured (resident set of the backend, `VmHWM`, and the memory contexts of the
buffers; 18.7 and 19rc2 equal to the megabyte):

| Case | Allowance | Buffers at the end | Resident set |
|---|---|---|---|
| 5 nested savepoints, 3.6 MB each | 8 MB | 17 MB | +19 MB |
| 20 nested savepoints, 3.6 MB each | 8 MB | 68 MB | +71 MB |
| 60 nested savepoints, 3.6 MB each | 8 MB | 206 MB | +209 MB |
| 12 nested savepoints, 60 MB each | 128 MB (the default) | 687 MB | +690 MB |
| 40 cursors, each opened after 3 MB were written | 8 MB | 114 MB | +119 MB |
| the same writing without savepoints or cursors | 8 MB | at most 8 MB | +30 MB |

Answer to "how many levels can a role open": any number; `SAVEPOINT` nests until
memory ends, and every level may hold just under half of the allowance. Answer to
"many open cursors, each pinning a buffer": each cursor pins the buffer written
since the previous one, so the total is the sum, again without a limit. The
README names the level case and the single cursor, not the sum over cursors.
Heap holds a few kilobytes per subtransaction and nothing per cursor. A role
with `CREATE` on a schema and a columnar table can therefore take the memory of
the machine (12 levels at the default setting held 687 MB; the growth is linear,
so about 100 levels would hold 6 GB), and the kernel's
out-of-memory killer takes a backend, which restarts every connection. `INSERT ...
SELECT` of a table into itself shows the same on a small scale (23 MB for 8 MB).

Expected: the allowance is soft but the excess does not grow without a limit with
what a role can open.

Direction: an error ("columnar write buffers hold N bytes, more than M times
write_buffer_memory; end a subtransaction or close cursors") when the total passes
a multiple of the allowance that nothing can lower (no flushable buffer is left);
or, for cursors, let a scan that is about to pin a buffer over the allowance take
a private copy of the rows it needs and release the buffer.

### C6-04 (low) A change away from columnar leaves stripe rows when the event trigger does not run

Place: the access method notes the old file of a rewrite to columnar itself (the
transient table is created through it), but for a rewrite away from columnar it
depends on the `table_rewrite` trigger (README, "VACUUM and space").

Steps: `ALTER TABLE x SET ACCESS METHOD heap` on a columnar table with 5 stripes,
with `SET event_triggers = off`, or with `ALTER EVENT TRIGGER
kingres_columnar_on_rewrite DISABLE`, or in single-user mode (`postgres
--single`, where the triggers do not run). Observed on both majors: the table
reads correctly (5 000 rows), 5 stripe rows of the old file stay,
`columnar_check` reports one `orphan_catalog_rows`; `VACUUM` of any columnar
table removes them. A type change (columnar to columnar) with the same settings
leaves nothing. Replica role, a function, a partitioned table, VACUUM FULL,
TRUNCATE, a failed rewrite, a savepoint rollback, `PREPARE TRANSACTION` then
commit or roll back leave nothing.

`t/023` says "a session that turned the event triggers off leaves nothing either:
the access method knows which files its rewrites replaced" and tests the type
change only. No reader sees the rows (they are keyed by the identity of the old
file), so this is a leak and not a wrong answer.

Direction: the heap side of such a rewrite does not call the extension, so the
access method cannot note the file itself; state in the README that the cleanup of
a change away from columnar needs the trigger and that `VACUUM` is the fallback
(it is), and correct the sentence of the test.

### C6-05 (low) A database created by the previous script keeps the old script; no upgrade path

The round changes version 0.1 in place: the install script has a new event trigger
and three new functions. A database made by the previous round has the new
library (the extension is "installed", the script is not re-run) and the one
version of the extension is 0.1, so `ALTER EXTENSION kingres_columnar UPDATE` has
no script to run (I did not run it). I wrote such a
database with the previous build and opened it with this one (E8): it reads and
writes, and its triggers are `kingres_columnar_on_drop` and
`kingres_columnar_check_ddl` only (no `table_rewrite` trigger, no
`columnar_table_xid`), the `check_ddl` trigger is the old one that calls
`columnar_reconcile()` after every `ALTER TABLE`. That is the linear cost and the
contention of C5-04 and C5-05, which this round fixes only for databases that run
the new script (`DROP EXTENSION` is not an option: it drops the tables).

Not a wrong answer; the README's claims about the cost of `ALTER TABLE` are true
only for a fresh install. Direction: an `ALTER EXTENSION UPDATE` script from a new
version number (0.1.1 or the like), or a sentence in the README that databases
made before this round must run the statements of the new script by hand.

### C6-06 (note) Identity and creation ids are readable by any role

`columnar_identity`, `columnar_creating_xid` and the new `columnar_table_xid` are
granted to PUBLIC and do not check privileges on the table: an unprivileged role
read the 63-bit identity and both ids of a table of another owner on both majors.
The README says the functions that act on one table check ownership themselves;
these three do not. No data of the table is revealed (the stripe catalog is
revoked), so it is a note.

### C6-07 (low) Tests: mutants that survive, and a probe that cannot reach the commit path

See "The tests of this round". Five mutants of the round's fixes survive every
file that claims them: the cleanup that deletes the rows of every noted file (it
empties a table in a case with an ordinary failed rewrite in a savepoint), the
cleanup moved from the end of the command to the commit, the `TM_Deleted` branch,
the accounting by payload, and the flush of a buffer under an open cursor (wrong
counts: it survives all of `t/014`, `t/017` and `t/022` and is found only by a
randomised comparison with heap); three more (`t/017` against the bytes rule, the
child's own flush and the pinned buffer taking rows) survive `t/017` and die in
one of the other two files. The probe module registers a
subtransaction callback only, so the commit-time half of the same family of
defects (C6-01) cannot be written in it.

Direction: add to `t/023` the transaction-block form of a failed rewrite and of a
savepoint rolled back around a rewrite, followed by a commit and a read, and a
look into `columnar_stripe` inside the transaction; add to `t/017`/`t/014` the
cursor-with-flush case against a heap twin (the random comparison `t/006` does not
include cursors with partial fetches and savepoints in this way); extend the probe
with a callback at `PRE_COMMIT`.

## What held under attack

* The creation rule and the catalog question (E1, E10), both majors: no empty
  answer for content that existed for the snapshot in any sequence, including
  `VACUUM FULL pg_class` and `VACUUM (FREEZE) pg_class` after the conversion, 300
  grant/revoke updates of the row, rename and `SET SCHEMA`, an owner change before
  and after, a table dropped and made again, a table made after the snapshot in a
  prepared transaction or in a savepoint that was rolled back and repeated, an
  exported snapshot imported after the conversion (the error), and a cursor
  `WITH HOLD` taken before it (the three rows). On a hot standby the answer is the
  error or a cancelled session (recovery conflict), never an empty table, with
  feedback on or off. The error is also given where heap gives 0 for a table that
  existed and was empty-handed for the snapshot (stricter, as stated).
* The "no storage" shortcut: a heap table emptied and vacuumed, truncated after
  the snapshot, or made empty and converted gives the empty table, as heap does.
* The second buffer (E3, E6, E7): about 850 episodes on 18.7 and 1 310 on 19rc2
  against a heap twin, including cursors with partial fetches and `WITH HOLD`,
  savepoint rollbacks that remove one of the two buffers, `INSERT ... SELECT` from
  the same table, `ADD COLUMN`/`DROP COLUMN`, 0 differences. BEFORE ROW triggers
  that read or write the same table, PREPARE and commit with two buffers, two
  `WITH HOLD` cursors on two buffers: equal to heap.
* WAL (E6): `wal_consistency_checking = all` on both nodes, a standby, an
  immediate stop with a prepared transaction open, recovery, promotion and writes
  on the promoted node, partitioned and dropped-column tables: no inconsistency
  message, `columnar_check` empty on both nodes, equal contents.
* The cleanup after a rewrite (E4, E13): VACUUM FULL, TRUNCATE, type change,
  `SET ACCESS METHOD`, REFRESH of a materialized view, a partitioned table and a
  partition, a rewrite in a function, with the replica role, in a savepoint that is
  rolled back, a failed rewrite, `PREPARE TRANSACTION` followed by `COMMIT
  PREPARED` or `ROLLBACK PREPARED`: the stripe rows are removed exactly for the
  files that were replaced and come back with a rollback; the table reads as
  before in every case. Nothing wrong was deleted.
* The privileges of the functions behind the trigger: `columnar_note_rewrite`,
  `columnar_finish_rewrites`, `columnar_reconcile`, `columnar_on_rewrite`,
  `columnar_check_ddl` give "permission denied" to an unprivileged owner; the
  event triggers are `ENABLE ALWAYS`.
* The library not preloaded (E12): `ALTER TABLE` of a heap table, a heap `DROP`
  and `CREATE` work, a rewrite of a columnar table gives the error that names the
  setting, a change to heap works and reads the rows.
* `DROP TABLE` with two concurrent drops: no error, nothing left.
* The catalog repair (E8): three earlier builds, all rows and flags kept.
* The window for reads and for writes to the relation being handed over (the
  mutants of `t/022` die).
* The unmodified build under the whole suite on both majors, below.

## Not reproduced

* A wrong answer from `kc_table_existed_for`: none, after vacuum, freeze, rename,
  schema change, owner change, wraparound-style freezing of the `pg_class` row
  (`VACUUM (FREEZE)` with `vacuum_freeze_min_age` irrelevant because the older
  snapshot holds the horizon back), an exported snapshot, a standby.
* A snapshot that reads an empty table for content that existed: none.
* A cleanup that removes rows of the wrong file: none; only the mutant of C6-07
  does.
* Two sessions failing each other through the cleanup: none.
* A doubled or lost row from the second buffer, the hand-over or a cursor in the
  fuzz: none.
* A deadlock between the reader's look at `pg_class` and `VACUUM FULL pg_class`
  or a reindex: none seen; the reader takes `AccessShareLock` on `pg_class` after
  its lock on the table, and the other side takes no lock on the table.
* A concurrency effect of `DROP`: only the stated cost.

## Not covered

* Builds without assertions: all my builds had assertions. C6-02's production
  behaviour is shown with a copy in which the one `Assert` is a no-op, which is
  what a build without `USE_ASSERT_CHECKING` compiles, not with a packaged build.
* A real address-space limit for memory; the many-table and many-partition cases
  measured only against a heap twin for the tables (equal: 265 MB for heap and 240
  MB for columnar for 8 000 tables, so the cost is the catalog's).
* Logical decoding output plugins (a columnar table is refused for publications;
  an output plugin reading the table in the commit was not tried), a base backup
  restored with an old snapshot, archive recovery to a point in time, `pg_upgrade`
  (documented as unsupported), an object identifier wraparound.
* Parallel workers (the scan refuses them), `READ ONLY DEFERRABLE`, multixacts.
* SERIALIZABLE write-skew schedules with cursor-split buffers: `t/009` passes;
  I did not write new schedules.
* Mutants of `t/016`, `t/019` and `t/005`/`t/006`/`t/009` this round.
* Binary upgrade and `pg_restore` with event triggers disabled for the whole run,
  beyond the single-session cases above.
* The user-visible effect of C6-01 with a real third-party module; I used a small
  module of my own.

## Baseline numbers

Unmodified extension, out of tree, `EXT_WERROR=1`, no compiler warning, before any
mutant or experiment; both majors ran at the same time on a loaded machine:

| Major | Regression | TAP |
|---|---|---|
| 18.7 (assertions) | 11 of 11 pass | 23 files, 3 492 tests, pass; `t/013` skipped (module not installed in the given installation); 387 s wall |
| 19rc2 (assertions) | 11 of 11 pass | 23 files, 3 494 tests, pass; `t/013` skipped; 353 s wall |
| 18.7 and 19rc2, `t/013` alone, module built by me | | 23 checks, pass, 67 s each |

`t/016` takes 3 s, `t/017` 3 s, `t/021` 3 to 4 s, `t/022` 1 s, `t/023` 2 s,
`t/002` 36 s, `t/009` 48 s and `t/006` 160 to 200 s on both majors. The `t/022`
probe module builds with the compiler and headers of the installation on both.
