<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
# Adversarial review of the sixth fix round: contrib/kingres_columnar 0.1

Reviewer: an independent reviewer (not the author). The change under review
answers `kingres/docs/reviews/2026-11-17-kingres-columnar-0.1-fifth-fixes.md`
(C6-01 to C6-07). The earlier reviews are
`kingres/docs/reviews/2026-11-15-kingres-columnar-0.1.md`,
`kingres/docs/reviews/2026-11-15-kingres-columnar-0.1-fixes.md`,
`kingres/docs/reviews/2026-11-16-kingres-columnar-0.1-second-fixes.md`,
`kingres/docs/reviews/2026-11-16-kingres-columnar-0.1-third-fixes.md` and
`kingres/docs/reviews/2026-11-16-kingres-columnar-0.1-fourth-fixes.md`; the
suites that cover their findings were run in full (baseline below). The reviewed
state (the fix round and the directory around it) was exported with `git
archive` (paths `contrib/kingres_columnar`, `kingres/dev`, `kingres/docs`) into a
private work area; the working tree was not used for any result and nothing in
the repository was changed except this file. Line numbers, where given, are those
of that export.

## Summary

The three central mechanisms of the round do what they claim in every case I
could build that a PostgreSQL core path or a plausible module reaches: a row
written after the commit-time flush is committed once, aborted with the
transaction, equal to a heap twin under SERIALIZABLE (write skew) and across
PREPARE TRANSACTION, a restart and promotion; the ceiling on the write buffers
holds in the shapes I measured and leaves the transaction usable; the object
access hook removes the stripe rows of every replaced or dropped file in 25
command shapes, never the rows of a file that still exists. I found no wrong
answer, no lost or doubled committed row and no SERIALIZABLE anomaly. WAL
consistency checking, a standby, a crash with a prepared transaction and a
promotion are clean on both majors.

What does not hold:

* The answer the round chose for a write that arrives when the transaction is no
  longer in progress, "an error", cannot be given where it matters. An ERROR
  raised in a commit, prepare or abort callback is escalated by PostgreSQL: a
  write at `XACT_EVENT_COMMIT` ends in `PANIC: cannot abort transaction ... it
  was already committed`, one at `XACT_EVENT_PREPARE` in a failed assertion, one
  at `SUBXACT_EVENT_ABORT_SUB` in `PANIC: ERRORDATA_STACK_SIZE exceeded`. A
  server stop on both majors. It needs a module that writes at those events
  (C7-01).
* The tests of the round are, again, weaker than their names. The random
  comparison with a heap twin (`t/026`) reaches memory pressure 11 to 14 times in
  70 to 250 episodes, never the hand-over window, never a write-through, never
  the ceiling; its seed is fixed unless the environment sets it. Of 21 mutants I
  built of this round's fixes, 8 survive every test file that claims them, among
  them the guard that keeps prepared buffers from being flushed (reachable, and
  the server stops without it), the factor of the ceiling, the ceiling of a
  hand-over, and a pair of defenses whose joint removal empties a table (C7-02,
  C7-03).
* The README's memory formula is not a bound for a value larger than about 32
  MB: a single value of 400 MB under the smallest allowance grew the backend by
  1 911 MB (heap: 1 144 MB) against about 1 200 MB plus a stripe term that the
  formula treats as small (C7-04).
* Two small leaks and one cost: stripe rows stay behind for a file dropped by a
  late callback with event triggers off, or dropped in a session that did not
  load the library; a late row is a stripe of its own, 8.2 kB for one row
  (C7-05, C7-06).

| Severity | Count |
|---|---|
| critical (wrong answer, lost or doubled committed rows, crash, a SERIALIZABLE anomaly heap prevents, an unreadable table) | 0 |
| high | 1 |
| medium | 1 |
| low | 4 |
| note | 1 |

C7-01 is a server stop, which by the letter of the definition is critical; as in
the previous review (C6-01, C6-02) it needs code of another module that writes
a columnar table from a commit, prepare or abort callback, and nothing in the
KingresDB tree does, so I count it as high and say so for the owner to decide
otherwise.

| Id | Severity | Component | Statement |
|---|---|---|---|
| C7-01 | high | `kc_write_check_state` (`kc_write.c`) | A write from a commit, prepare or abort callback gets an ERROR that PostgreSQL turns into a PANIC or a failed assertion: the server stops. |
| C7-02 | medium | `t/026_random_cursors.pl` | "Random comparison under memory pressure": 11 to 14 flushes in 70 to 250 episodes, no window, no late write, fixed seed. |
| C7-03 | low | tests of the round | Eight of 21 mutants survive; one pair of independent defenses can be removed together with `t/023` passing, and the table is then emptied. |
| C7-04 | low | README "Memory" | The formula is not a bound for large values (400 MB value: +1 911 MB, 4.8 times the value, heap 2.9 times). |
| C7-05 | low | hook, README "VACUUM and space" | Stripe rows of a file dropped by a late callback (event triggers off) or in a session without the library stay until `VACUUM`; the README says correctness never depends on a trigger. |
| C7-06 | low | `kc_write_late`, comment | One stripe per late row (8.2 kB per row, 20 001 stripes for 20 001 rows, also for a late rewrite); the comment says `VACUUM` joins them, it does not (`columnar_compact` does). |
| C7-07 | note | ceiling | At the smallest allowance about 800 nested savepoints of one row each reach the ceiling; heap does not. |

## Fate of C6-01 to C6-07

| Id | Fate | Evidence |
|---|---|---|
| C6-01 | Fixed for pre-commit, pre-prepare and the window of a hand-over; the error path for writes later than that is a crash (C7-01) | A probe module loaded before the extension writing at pre-commit: 2 rows, 20 000 rows, nested exception blocks with a rollback, a prepared transaction committed after a crash and restart, a promotion: equal to a heap twin on 18.7 and 19rc2, `columnar_check` empty. Write skew between two SERIALIZABLE sessions where one side writes late: 40001 for the columnar tables exactly as for heap, late and normal. A late write costs one stripe per row (C7-06). |
| C6-02 | Fixed; the new guards are reachable and no test covers them | The reproduction of the previous review is routed to write-through and gives 4 000 for 4 000. Removing the guards in `kc_buf_flushable` and `kc_flush_pressure` (mutant M4) passes every test, but a `COPY` run in the window into a table made in the same transaction reaches them through `kc_finish_bulk_insert`: with the guards removed the server stops at the assertion of `kc_subxact_publish`; unmodified: 1 007, 13 and 2 508 rows, once each. |
| C6-03 | Fixed in the shapes measured; the formula is wrong for large values (C7-04) | 60 levels under 8 MB: error "hold N bytes ... limit". 900 levels of one row under 1 MB: error at about 800, backend +9 MB (heap +10 MB). 12 levels of 60 MB under the default setting: no error, +194 MB. 1 500 columns, 20 000 rows: equal to heap. No drift in ten loops of 3 000 iterations (commit and abort of subtransactions, cursors, TRUNCATE, create and drop, widen): a probe write at the ceiling always succeeded. After the error, `ROLLBACK TO` works and the counts equal the heap twin's. |
| C6-04 | Fixed for the commands of the review; two leaks (C7-05) | 25 command shapes (below): stripe rows of the old file gone, of a file in use kept, `columnar_check` empty, with event triggers on and off. |
| C6-05 | Fixed | A database made with the earlier script (old trigger functions) keeps working: `columnar_note_rewrite` is still exported. The README says what a developer database needs (replace two functions, leave the old trigger); I did not run that statement. |
| C6-06 | Not re-verified | The round says the three functions already require SELECT; the regression test `columnar_security` passes. |
| C6-07 | Mostly fixed | The five mutants of the previous review: the cleanup deleting rows of every noted file survives alone, but is redundant (see below); the others die. The flush under a cursor dies in `t/026` only. |

## The tests of this round

Method: a copy of the extension with one change, built into a second copy of
the installation (assertions on), and the test files that claim the fix. "Killed"
means the file reports failed checks or does not finish. All results are from
18.7; the unmodified files pass on both majors.

### `t/024_commit_window.pl` (C6-01)

Claims: a row written by a module's pre-commit or pre-prepare callback is in the
table once, in both library orders, with COPY, savepoints, a table made in the
transaction, a restart, a prepared transaction, a rollback; deletes and updates
there are refused; an error in the callback leaves nothing.

Preconditions: established. The same statement writes the columnar table and a
heap twin and the two are compared; both library orders run; the "after" order
is the one that matters.

| Mutant | Result |
|---|---|
| `kc_commit_flushed = true` not set at pre-commit (M2) | killed, 16 of 60 checks fail |
| no check that the transaction is in progress (M19) | **survives** (no test writes at COMMIT or later) |
| no parallel-worker check (M20) | survives (a worker cannot reach the write; effectively equivalent) |

Not covered although the commit message says it: "A write when no transaction is
in progress ... is an error." No file writes at commit, prepare or abort time.
Doing it is C7-01. The comment of the file says ON COMMIT actions cannot reach a
columnar table because there is no temporary one; that is right (the statement is
refused).

### `t/022_handover_window.pl` (C6-02)

Claims: a reader in the window sees each row once, a write there goes to a
stripe, nothing is doubled or lost, the memory in the window is bounded.

| Mutant | Result |
|---|---|
| the window is not part of "late" (M3) | killed, 2 of 29 |
| `kc_buf_flushable` ignores prepared buffers and `kc_flush_pressure` ignores the window (M4) | **survives all of `t/022`, `t/024`, `t/026`** |
| the handover ceiling removed (M18) | **survives** `t/022` and `t/025` |
| the tail charge not added at install (M21) | survives (probably equivalent: the memory is charged when the tail is built) |

M4 matters. Since writes in the window go to write-through, the C6-02
reproduction never reaches the choosers any more, and the one remaining way is a
command that ends with `kc_write_flush_relation` (`kc_finish_bulk_insert`): a
`COPY` or `INSERT ... SELECT` into a table made in the same transaction, run by
a callback in the window. With M4 the server stops at the assertion in
`kc_subxact_publish` (and, as in the previous review, a build without assertions
would commit the child's rows twice; I did not build one). No test of the round
runs that command.

### `t/025_memory_bound.pl` (C6-03)

Claims: the memory of the backend stays within the formula for levels, cursors,
relations, wide rows and tiny values, measured as `VmPeak`.

Preconditions: established for the measure (base read in the same session, a
heap twin for the relation cases, the refused transactions leave nothing). The
bound it checks is loose: 6A + 3R + S + 12 MB, with S a number the test chooses
(4 MB, 40 MB).

| Mutant | Result |
|---|---|
| no ceiling check on a write (M17) | killed, 5 failed |
| the rows of a committing subtransaction not charged (M15) | killed, 11 failed in `t/025`/`t/022`/`t/017` |
| ceiling factor 4 in place of 2 (M5) | **survives**: the error is still raised and the growth is still under the loose bound |
| no 2 kB per buffer (M6) | **survives** (the relation cases compare with heap plus a generous allowance) |
| the handover ceiling removed (M18) | **survives** |

So the test establishes that there is a ceiling, not that it is twice the
setting nor that every buffer is charged. It does not test a value larger than
the stripe chunk (C7-04), and it tests no change of the setting in a transaction
(I did: below).

### `t/026_random_cursors.pl` (C6-07)

Claims: a seeded random comparison with a heap twin of cursors, savepoints,
exception blocks and writes "under memory pressure".

Preconditions: partly. The twin runs in the same transaction, a failure prints
the episode's script and the seed is printed and can be set
(`KC_FUZZ_SEED`, `KC_FUZZ_EPISODES`), so a failure is replayable. But the
default seed is fixed (20261117) and 70 episodes, so the file is deterministic
and runs the same sequence every time unless the environment sets a seed; the
operations are inserts of 1 to 1 500 rows, inserts from the table itself,
savepoints (at most three deep), rollback to, release, exception blocks,
cursors (at most four) and checks, on one columnar table. No second table, no
`TRUNCATE`, no rewrite, no prepared transaction, no `SET` of the memory setting,
no more than three levels. An episode that reaches the ceiling is skipped
without comparing.

How many runs reach the paths the round changed (an `elog` counter in a copy,
71 episodes with the default seed; 250 episodes each with seeds 11, 22, 33):

| Path | 71 episodes | 250 episodes (three seeds) |
|---|---|---|
| prepare of a hand-over (`kc_buf_merge_prepare`) | 175 | 630, 617, 573 |
| a buffer a cursor reads checked for flushing | 158 | 452, 478, 363 |
| memory-pressure flush (`kc_flush_pressure` wrote a buffer) | 11 | 14, 12, 12 |
| the buffer's own flush at hand-over (`kc_flush_own_buffer`) | 0 | 0 |
| a write-through (`kc_write_late`) | 0 | 0 |
| a prepared buffer met by a chooser | 0 | 0 |
| the ceiling (write or hand-over) | 0 | 0 |

The episodes use an allowance of 2 MB and write at most about 300 kB a
statement, so the pressure the name promises is almost never reached. The file
does kill the flush under a cursor (M14: with the pinned-buffer test removed the
default seed fails, as do six other seeds I tried; `t/014`, `t/017` and `t/022`
pass with that mutant), so it earns its place for that defect; it does not
test the paths this round added.

### `t/023_rewrite_cleanup.pl` (C6-04, C6-05)

| Mutant | Result |
|---|---|
| the hook notes nothing (M9) | killed (2 failed, then the file does not finish by itself) |
| the end-of-command cleanup removed (M11) | killed, 3 failed |
| the commit-time cleanup removed (M12) | killed, 13 failed |
| notes survive an abort of the subtransaction (M8) | killed, 1 failed |
| the cleanup deletes the rows of every noted file, no comparison (M7) | survives alone |
| a committed subtransaction does not hand its notes to the parent (M13) | survives alone |
| M7 and M13 together (M713) | **survives; with `event_triggers = off` the table is emptied** |

M7 and M13 are each covered by the other defense, so each alone changes no
behaviour I could find. Together they empty a table in this case, which no test
runs (`event_triggers` off, because with the trigger on the end-of-command
cleanup consumes the note first):

```
SET event_triggers = off;
BEGIN; SAVEPOINT a; SAVEPOINT b; DROP TABLE x; RELEASE b; ROLLBACK TO a; COMMIT;
SELECT count(*) FROM x;     -- 0 with M713, 5000 unmodified
```

The same with `ALTER TABLE x ALTER a TYPE bigint` and `SET ACCESS METHOD heap`.
The unmodified build gives 5 000 in all three. This is the shape that the
previous review asked for (a rewrite or drop around a savepoint, followed by a
commit); the test has the single-level form only.

### Changed older files

`t/022` is rewritten for write-through (above). `t/021`, `t/002`, `t/013`,
`t/014`, `t/016`, `t/019` pass unchanged on both majors (`t/013` is skipped here:
the `xid_wraparound` module is not in the installations given). I did not mutate
them this round.

## Findings

### C7-01 (high) A write when the transaction is ending raises an ERROR that PostgreSQL makes a PANIC or an assertion failure

Place: `kc_write.c`, `kc_write_check_state` (about line 957): `if
(!IsTransactionState()) ereport(ERROR, ...)`.

Steps (a module of 90 lines that holds an open relation and calls
`table_tuple_insert` from a callback at the chosen event; loaded before the
extension; 18.7 and 19rc2 alike):

```
CREATE TABLE p (a int) USING columnar;
BEGIN; SET LOCAL p2.rel = 'p'; SET LOCAL p2.at = 'pre_commit,commit';
INSERT INTO p VALUES (1); COMMIT;
```

Observed, event by event:

| Event of the callback that writes | Observed |
|---|---|
| `XACT_EVENT_COMMIT` | `ERROR: columnar table "p" cannot be written now: the transaction is ending`, then `WARNING: AbortTransaction while in COMMIT state`, then `PANIC: cannot abort transaction N, it was already committed`: the server restarts every connection |
| `XACT_EVENT_PREPARE` | the ERROR, then `TRAP: failed Assert("TransactionIdIsValid(proc->xid)")` in `procarray.c`; without assertions the abort of an already prepared transaction |
| `SUBXACT_EVENT_ABORT_SUB` | the ERROR raised while a subtransaction is aborting, again and again, then `PANIC: ERRORDATA_STACK_SIZE exceeded` |
| `SUBXACT_EVENT_COMMIT_SUB` | the ERROR, `WARNING: AbortSubTransaction while in COMMIT state`, the transaction is aborted (no crash) |
| `XACT_EVENT_ABORT` | not shown: the probe could not hold an open relation in that state, so no write was attempted (not covered) |

Expected: no server stop. In these states PostgreSQL escalates any ERROR, so
"an error" is not an available answer; the access method has to refuse without
raising (a WARNING and the row discarded is the only thing a post-commit
callback can be given, and it must be said in the README), or write through
as it does before the commit.

Direction: in `kc_write_check_state`, test `GetCurrentTransactionState`-free
conditions that tell the callback phase (`kc_commit_flushed` has been reset by
then, so a "committing" flag set at `PRE_COMMIT` and cleared at the abort or
the end of the commit would do) and `ereport(WARNING)` and return without
writing in those phases only; keep the error for `IsParallelWorker()`. Add the
five events to the probe module and to `t/024` (a test that expects the server
to stay up and the committed rows to be exactly those of the transaction).

### C7-02 (medium) The random comparison does not run under memory pressure and reaches none of the new paths

See "The tests of this round", `t/026`. 11 to 14 pressure flushes in 71 to 250
episodes; no write-through, no prepared buffer, no ceiling, not once. The
default seed is fixed. The file is a good test of the flush under a cursor, and
that is all it tests of this round.

Direction: allowance 1 MB with rows of a few hundred kilobytes in some episodes;
a second columnar table; a probe module in the file (it already builds one in
`t/022`) so that a share of the episodes write late and in the window; a seed
from the clock by default, printed, as `t/006` does; count the paths as
assertions (for example `elog` counters read back) so that a change of the
generator that stops reaching them fails the file.

### C7-03 (low) Mutants that survive

M4, M5, M6, M18, M19, M7 and M13 (together), M21: see the tables above. The
two that matter: the guard for prepared buffers (reachable with a `COPY` into a
table made in the transaction, run in the window; the server stops without the
guard) and the nested release-then-rollback form of a drop or rewrite with
event triggers off (a table emptied if two defenses fail at once).

Direction: for the first, a case in `t/022` with a `COPY` into a table created
in the transaction, in the window, with rows pending in the parent and the child;
for the second, `t/023` with the form above and `event_triggers = off`; for the
ceiling, assert the number in the error message (2 times the setting, and the
`2 kB` per buffer, by writing rows of a known size) instead of a loose growth
bound.

### C7-04 (low) The memory formula is not a bound for a large value

Place: README "Memory", `t/025` section 4.

A single value (`repeat('x', N)`) in one statement, `write_buffer_memory` 1 MB,
`VmPeak` of the backend:

| Value | Columnar | Heap twin | Formula (3R plus a stripe term) |
|---|---|---|---|
| 100 MB | +479 MB | +286 MB | about 300 MB + S |
| 400 MB | +1 911 MB | +1 144 MB | about 1 200 MB + S |

The README says S, the stripe being built, is "the compressed bytes and a chunk
of at most 32 MB raw" and "not counted". For a value larger than that chunk the
stripe term is the value several times (the copy, the compression work space,
the output) and is the largest term; the formula then promises about 3R and the
backend takes 4.8R, 1.7 times what heap takes. A text or bytea of 1 GB, the
PostgreSQL limit, would take about 5 GB. It is not a wrong answer and heap needs
a multiple of the value too, but the sentence "the memory the backend takes is at
most" is not true, and the test chooses S freely (40 MB for rows of 5 MB).

Direction: state the large-value term (about 4.8 times the largest value) in the
README, or compress such a value in pieces; make `t/025` assert it with a
value larger than the chunk.

### C7-05 (low) Stripe rows stay for a file dropped late, or dropped without the library

Two ways I found, both leaving stripe rows of a file that is gone, removed by the
next `VACUUM` and reported by `columnar_check` as `orphan_catalog_rows`:

1. A callback of another module (loaded before the extension) that runs DDL at
   pre-commit: the hook notes the file after the commit-time cleanup has run, and
   the list is cleared at the commit. With event triggers on, the trigger of the
   statement itself does the cleanup. With `SET event_triggers = off`: `DROP
   TABLE x`, `ALTER TABLE x SET ACCESS METHOD heap` and a type change at that
   point leave the 5 stripe rows of x (5 of 5 for each; `TRUNCATE` is clean).
2. A session that did not load the library (the server restarted without
   `shared_preload_libraries`, the extension still installed): `DROP TABLE x` of
   a columnar table leaves its 5 stripe rows; 1 orphan reported. The README
   requires the preload for writes; it does not say that a drop without it
   leaks.

The README says "Correctness never depends on an event trigger firing" and "the
rows are removed at the commit instead, which is tested for every command above".
Both statements are true for the cases it names. They are not true for 1 and 2,
and neither case is tested.

Direction: run the cleanup again at `XACT_EVENT_COMMIT`-adjacent time is not
possible (no transaction); the cheap fix is to clean in the hook's own note when
`kc_commit_flushed` is set (the transaction is still in progress at pre-commit,
so the rows can be deleted at once) and to say in the README that a drop without
the library leaves rows for `VACUUM`.

### C7-06 (low) A late row is a stripe of its own

Place: `kc_write_late`, `kc_write_rows`.

A callback at pre-commit running `INSERT INTO p SELECT g FROM
generate_series(1,20000) g`: 20 001 rows and 20 001 stripes, 163 856 384 bytes
(8.2 kB a row), `columnar_check` empty; a late rewrite (`ALTER TABLE x ALTER a
TYPE bigint` at pre-commit) makes one stripe per copied row, 5 000 for 5 000
rows. `VACUUM p` leaves them (20 001 stripes after it); `columnar_compact('p')`
joins them to one. The comment of `kc_write_late` says "VACUUM and
columnar_compact join small stripes"; the first part is wrong (the README says
only `columnar_compact`). Reading such a table is not measured. The cost is
stated by the author; I record the figures: one million late rows is 8 GB.
Multi-row writes (COPY) are batched, as `t/024` shows.

Direction: write the rows of one statement of a late callback through one
buffer that lives until the statement ends (the executor calls
`finish_bulk_insert`), or at least batch up to the stripe size per call from
`kc_write_row`; correct the comment.

### C7-07 (note) The ceiling at the smallest setting, and the exception block

With `write_buffer_memory = 1MB` the ceiling is 2 MB and a charge of 2 kB a
buffer: 900 nested savepoints with one row each reach it at about 800 ("hold
2 097 744 bytes, more than the limit of 2 097 152 bytes"), where heap runs them
with 10 MB. At the default setting the same shape reaches it at about 100 000
levels. A function with `SET kingres_columnar.write_buffer_memory = '1MB'` and
an exception block, called with 20 MB pending at an enclosing level, raises the
error at the block; a handler `WHEN OTHERS` catches it (the heap twin's block
succeeds), a transaction without a handler gets the error and `ROLLBACK TO` a
savepoint leaves it usable with the pending rows intact (counts equal to the
twin). `SET LOCAL` lower in the middle of a transaction behaves the same. None
of this is a wrong answer; it is the stated "error at the ceiling".

## Items the round left open

* **Deep nesting with much data gets the error at the ceiling.** Acceptable as
  stated: the error is exact, names the limit and the way out, leaves nothing
  half written and the transaction usable after `ROLLBACK TO`; the situation needs
  more than 2A held by enclosing levels, which 12 levels of 60 MB under the default
  setting no longer reach (194 MB measured, no error). The better answer, writing
  an enclosing level's rows with the right transaction id, is not available in
  the format as it is.
* **A row-at-a-time write after the commit-time flush makes one stripe per
  row.** A defect of cost only (C7-06), 8.2 kB a row; I would not accept it as
  "stated" without the comment being corrected and `VACUUM` or the late buffer
  taking care of it.
* **The restore of a base backup read by an old snapshot: tested.** A base
  backup taken with `pg_basebackup` while two sessions committed one paired
  insert (columnar and heap) per transaction and rewrote another pair of
  tables twenty times: the restored cluster has equal counts for both pairs (4 570
  and 2 000 rows), `columnar_check` is empty, and a REPEATABLE READ snapshot
  taken before `ALTER TABLE ... TYPE`, `VACUUM FULL` of a table and a type change
  of another reads the rows (2 000 and 4 570) with no error. Equal on 18.7; not
  repeated on 19rc2.
* **One mutant equivalent after the round.** M7 and M13 are each redundant with
  the other defense; the combination is not tested (C7-03).

## What held under attack

* Late writes (probe module loaded before and after the extension): 2 rows, 20
  000 rows, COPY, wide values, savepoints released and rolled back in the late
  statement, nested exception blocks in the late statement (rows equal to the
  heap twin: 1 105 rows, same set), a table made in the transaction, a prepared
  transaction with a late row committed after an immediate stop and restart,
  a rolled back prepared transaction, a failing callback (nothing committed), a
  callback that deletes or updates (refused, nothing committed).
* SERIALIZABLE: write skew with one side writing at pre-commit: 40001 at the
  write, equal to heap, late and normal, for columnar and heap tables.
* The window of a hand-over: reads once, writes to the same table, writes to
  another table, a `COPY` into a table made in the transaction (the case that
  needs the guards), three sizes of parent and child: 1 007, 13, 2 508 rows,
  once each.
* The ceiling: the error, `ROLLBACK TO`, commit afterwards, counts equal to the
  twin; `SET LOCAL` to a lower value in the middle of a transaction, a function
  with a `SET` clause with and without an exception block.
* Accounting: no drift in ten loops of 3 000 iterations in one transaction
  under a 1 MB allowance (subtransaction commit and abort, nested abort, cursor
  open and close with and without further writes, `TRUNCATE`, create and drop of a
  table, a second relation, rows of 3 kB with a division by zero in the block,
  `ADD COLUMN` in a loop); the resident set does not grow with the loop; 1 500
  columns of 20 000 rows: as heap.
* The hook: `DROP TABLE`, `DROP SCHEMA CASCADE`, `DROP OWNED`, `TRUNCATE`
  (alone and in the creating transaction), `VACUUM FULL`, a type change, `SET
  ACCESS METHOD` to heap, to heap and back, from heap, `SET TABLESPACE` (alone
  and back: no rows lost, none left), detach and drop of a partition, a drop in
  a savepoint that is rolled back (alone and followed by an `ALTER TABLE`), in a
  released savepoint, followed by an abort, followed by a new table of the same
  name, a rewrite twice with one rolled back, a failed rewrite in an exception
  block followed by another `ALTER TABLE`, a prepared drop, rewrite and change
  of access method committed or rolled back by another session, with event
  triggers on and off: stripe rows removed exactly for the files that went,
  every table that exists reads its rows, `columnar_check` empty.
* A table whose file was deleted, emptied, shortened or overwritten with
  garbage: `DROP TABLE` still works (the hook cannot fail the drop).
* WAL: `wal_consistency_checking = all` on a primary and a hot standby, writes
  late and in the window, a prepared transaction across an immediate stop, a
  rewrite, `VACUUM FULL`, `columnar_compact`, a drop and re-creation, a standby
  caught up, promotion and writes on the promoted node, an immediate stop and a
  restart of it: no inconsistency message, no assertion, equal contents, empty
  `columnar_check`. 18.7 and 19rc2.
* An unmodified build under the whole suite on both majors (below).

## Not reproduced

* A wrong answer, a lost or doubled committed row, or a SERIALIZABLE anomaly
  from write-through, the window or the hook: none.
* A leak of memory or of the account across transactions in a long-lived
  backend: none seen (the account is reset at each commit and abort; the loops
  above show no growth inside one transaction either).
* A cleanup that deletes the rows of a file that still exists: none in the
  unmodified build (only the combined mutant, C7-03).
* Late writes breaking `columnar_check` or the catalog at 20 000 stripes: none
  (the cost is C7-06).
* A deadlock or a wait in the cleanup: none (`try_table_open` on a table this
  transaction has itself locked).

## Not covered

* A build without assertions (all my builds had assertions). C6-02's production
  behaviour (doubled rows) was not counted; with the guards removed the
  assertion stops the server.
* Real third-party modules (pgaudit-like, postgres_fdw-like); a hook chain with
  another module that raises (sepgsql-like): the hook calls the previous hook
  first, so an error there stops the drop before the note is made; not run.
* `DROP DATABASE`, `DROP OWNED` for another database, a standby promoted after a
  drop (the promotion case above has a drop and re-creation before it; a drop
  replayed on the standby is covered by the clean `columnar_check` there), partition
  detaches under load, `CLUSTER`, object-identifier wraparound.
* The exact value of the accounting (`kc_total_bytes`) against the contexts: I
  inferred it from the absence of a spurious error at the ceiling, not from the
  counter.
* The restore of a base backup read by an old snapshot was run on 18.7 only.
* The fuzz of `t/026` with 250 episodes was run on 18.7 with three seeds, no
  difference; not on 19rc2. SERIALIZABLE write skew with cursor-split buffers
  and open cursors under late writes.
* C6-06 beyond the regression test; the update statements the README gives for
  a developer database made with the earlier script.
* Memory of parallel leaders, `COMMIT AND CHAIN` with late writes, event
  triggers raising at the moment of a hand-over, logical decoding.

## Baseline numbers

Unmodified extension, out of tree, `EXT_WERROR=1`, no compiler warning; both
majors ran at the same time on a loaded machine, each in its own copy of the
installation, with the TAP harness restricted to ports 59900 to 59999:

| Major | Regression | TAP |
|---|---|---|
| 18.7 (assertions) | 11 of 11 pass | 26 files, 3 693 tests, pass; `t/013` skipped (module not installed); 394 s wall |
| 19rc2 (assertions) | 11 of 11 pass | 26 files, 3 695 tests, pass; `t/013` skipped; 409 s wall |

`t/022` 2 s, `t/023` 4 s, `t/024` 4 s, `t/025` 9 s, `t/026` 5 s (default seed) on
both majors. 21 mutants were built (M2 to M9, M11 to M15, M17 to M21, M713) and
each run with the files that claim the fix; the ones not named in the tables
above died. Experiments: eleven TAP scripts with a probe module of my own
(events: pre-commit, commit, pre-prepare, prepare, abort, and the four
subtransaction events; SQL or a direct tuple insert through a held relation),
`BackgroundPsql` for the SERIALIZABLE schedules and the backup, and one
instrumented copy for the path counts.
