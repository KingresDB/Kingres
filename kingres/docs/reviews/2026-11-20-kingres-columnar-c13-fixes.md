<!-- SPDX-License-Identifier: AGPL-3.0-only -->

# Independent review of the fixes for the review of kingres_columnar C5 to C8 (branch `col-c13`)

## Scope and method

Reviewed: `contrib/kingres_columnar` on branch `col-c13`, commits 8891a9e3bb..7d927f54a6 (the nine commits after 8891a9e3bb), which answer `kingres/docs/reviews/2026-11-20-kingres-columnar-c5-c8.md`:

* C13-01, an index built or rebuilt while an older snapshot lives also indexes the rows of the stripes that snapshot
  still sees (`kc_catalog_load_dead`, the extra pass in `kc_index_build_range_scan`, t/063);
* C13-02, reservations of row numbers start at 64 and grow in place, the unused tail of an aborted writer is given back
  at the session's next reservation, warning at 90 percent, error with the way out (t/066);
* C13-03, decoding of a column added later (t/064);
* C13-04, amcheck `heapallindexed` (t/065);
* C13-05, VACUUM merges small stripes and, to do it, clears `PROC_IN_VACUUM` in `MyProc` and in the process array
  and runs only while the vacuum has no transaction id (t/067);
* C13-06, comments and Asserts.

Nothing was fixed or committed. The main checkout was not touched: a detached worktree of `col-c13` was used and removed
at the end after a clean `git status`. All servers were private copies of the two installs (PostgreSQL 18.7 and
19rc2, both built with assertions), the extension was built clean with `-Werror` on both majors from the
worktree's source, the ports were in 58900 to 58999, one cluster per scenario (two for the standby scenarios), and
every cluster, scratch area and private install was removed at the end.

Method: reading the diff and the code around it (`kc_write.c`, `kc_vacuum.c`, `kc_catalog.c`, `kc_storage.c`,
`kc_am.c`, and `procarray.c`, `vacuum.c`, `nbtsort.c` of the server for the flag and the build callbacks); my own
adversarial TAP scripts (generated comparisons with heap twins, concurrent sessions, crashes with `pg_ctl stop -m immediate`
at random moments and in the middle of a merging VACUUM, a hot standby that is promoted, a slowed VACUUM (`vacuum_cost_delay`
with `vacuum_cost_limit = 1`) to hold a window open); a one-line mutant of the flag clearing; the writer's own new tests
t/063 to t/067 on PostgreSQL 19 and t/004 against the mutant. The control-plane integration run that shares the
machine did not report the end marker while this review ran, so only light load was used (one test process, two at
most for a few minutes, no stress loops); the heavier loops below are short for that reason.

## Summary

| Severity | Count | Findings |
|---|---|---|
| critical | 1 | C14-01 |
| high | 0 | |
| medium | 2 | C14-02, C14-03 |
| low | 2 | C14-04, C14-05 |
| note | 2 | C14-06, C14-07 |

C14-01 is introduced by C13-02 (the giving back of an aborted writer's tail): an index vacuum that runs concurrently deletes
the index entries of rows that were inserted into the given-back numbers, so committed rows disappear from index scans
and a unique constraint stops being enforced. It is reproduced, deterministically, on both majors.

The other fixes held: no wrong answer, lost or doubled row, crash or recovery defect was found in the index build under old
snapshots, in the extension of reservations, in decoding, in amcheck, or in the VACUUM merge itself (see "What held").
The change to `PROC_IN_VACUUM` is correct by reading and I could not break it; its cost to the rest of the cluster is
C14-03.

## What was run

| Run | PostgreSQL 18 | PostgreSQL 19 |
|---|---|---|
| Clean build with `-Werror` of the branch | 0 warnings | 0 warnings |
| Writer's tests t/063, t/064, t/065, t/066, t/067 | not repeated (the writer ran them) | all 5 pass |
| rv01, rv11: abort, then insert, while a slowed index vacuum runs (C14-01) | FAIL, 0 rows found by index of 20 committed; unique key not enforced | FAIL, same |
| rv01 control (no abort first) | 20 of 20 found | not run |
| rv02: CREATE INDEX, CREATE UNIQUE INDEX (nulls in a key, composite) built in parallel (1 worker requested, leader participation as the planner sets it) after a compaction, older REPEATABLE READ snapshot, index scan against sequential scan | all equal | not run |
| rv03: `bt_index_check` and `bt_index_parent_check` with `heapallindexed` 80 times while two sessions inserted 300 000 rows | 0 failures | not run |
| rv04, rv05: VACUUM modes | see C14-02 | MAINTAIN probe not usable, see "Not covered" |
| rv06: 2 readers (READ COMMITTED, count against count of distinct ids, monotonic count, index scan against earlier scan, about 81 000 scans) against a writer that makes 140 savepoint stripes and runs VACUUM (so the merge runs), 60 cycles | 0 anomalies; stripes 1 at the end; `columnar_check_indexes` empty | not run |
| rv06 against the mutant that never clears the flag | 0 anomalies (8 600 scans); t/004 4 of 4 pass | not run |
| rv07: 4 rounds of (savepoint inserts with random aborts, aborted multi-row inserts, committed bulk inserts, VACUUM, `columnar_compact`) with `stop -m immediate` at a random moment, recovery, comparison with the heap twin; then the standby, promote, writes, VACUUM on the promoted node | all 36 checks pass | not run |
| rv08: generated mix (single rows, multi-row, savepoints, nested savepoints, exception blocks, rollbacks, up to 2500 rows) by 2 writers with `stripe_row_count = 1000` and a VACUUM plus `columnar_compact` loop; `reserve_min_rows` 64 and 1; equality with the twin, no TID twice, both indexes against scans, `columnar_check*` empty, after two more vacuums | seeds 1 and 5: pass | seeds 7 (min 1) and 11 (min 0): pass |
| rv10: VACUUM that merges 200 small stripes (xid assigned, copies written) killed by `stop -m immediate` three times, then a complete VACUUM, reuse of the freed blocks, standby, promote, VACUUM and insert on the promoted node | all 19 checks pass | not run |
| rv09: backend xid and the dead rows of an unrelated heap table during a merging VACUUM (C14-03) | measured | not run |

TRAP lines in the server logs of all runs: 0. The assertion build means the Asserts of C13-06 ran in every VACUUM above
and never fired.

## Findings

### C14-01 (critical) Rows inserted into given-back row numbers lose their index entries to a concurrent index vacuum; committed rows vanish from index scans and a unique constraint is bypassed

**Place.** `kc_apply_abandoned` and `kc_buf_reserve_numbers` in `kc_write.c` (C13-02) against `kc_index_vacuum` in
`kc_vacuum.c` and `kc_catalog_live_ranges` in `kc_catalog.c`.

**What happens.** A writer that aborts leaves the unused tail of its reservation noted; the session's next reservation gives
it back with `kc_meta_return` (without the reservation lock, before `kc_reserve_lock(ShareLock)`), so `next_row` goes down and
the next reservation takes numbers that are below it. The index vacuum decides what is dead from two things read under the
reservation lock and used afterwards, while the index scan runs without any lock: `next_row` (the `low` bound: every number at or above
`low` is kept) and the list of live claims. The claim row of an aborted writer is `KC_VER_DEAD`, so it is skipped and does not
lower `low` (a running writer's claim does, which is why the old give-back of a running writer's own tail was safe). So:

1. session A inserts a row and aborts; its reservation `[T, T+64)` has a claim row that is dead, `next_row = T+64`;
2. VACUUM (autovacuum) computes `low = T+64` and no live range for `[T, T+64)`, then starts `index_bulk_delete`;
3. session A inserts again: the tail `[T+1, T+64)` is given back, the new reservation takes `[T+1, ...)`, the rows get numbers
   below `low`, committed, with fresh index entries;
4. the bulk delete reaches those entries: row number below `low`, not in any recorded range, so `kc_vacuum_dead` says dead and the
   entries are removed. The rows are committed and in the table; no index finds them.

**Steps to reproduce** (`rv01`, `rv11`; the vacuum is slowed so that the window is long, but it needs no slowdown, only a vacuum whose index scan
has not reached the touched pages yet, which is the normal case for a large index):

```sql
CREATE TABLE r (id int, k int) USING columnar;
CREATE UNIQUE INDEX r_k ON r (k);
INSERT INTO r SELECT g, g FROM generate_series(1, 1000000) g;
-- session A:  BEGIN; INSERT INTO r VALUES (1, 9000001); ROLLBACK;
-- session V:  SET vacuum_cost_delay = 20; SET vacuum_cost_limit = 1; VACUUM r;   -- runs for about a minute
-- session A, after a few seconds, 20 times:  INSERT INTO r VALUES (i, 2000000 + i);
-- when V has finished:
SET enable_seqscan = off; SET enable_bitmapscan = off; SET kingres_columnar.enable_custom_scan = off;
SELECT count(*) FROM r WHERE k > 1999999;     -- 0; a sequential scan gives 20
INSERT INTO r VALUES (99, 2000001);           -- accepted: the unique index no longer knows the key
```

Both majors: 0 of 20 found. Without the first (aborting) step the same run finds 20 of 20.

**Why it matters.** Silent wrong answers through any index, and for a unique index a duplicate key that PostgreSQL's rules say
cannot happen, with no error anywhere; `columnar_check_indexes` would report it afterwards but nothing alerts. It needs only
a session that aborts and then writes (a failed statement in a session that goes on is enough, as is a savepoint
rollback), and a vacuum, with autovacuum on, that starts in the window; the aborting session does not have to be
malicious. The 64 numbers of a small reservation make the window as wide as the index scan lasts.

**What a fix must achieve.** A number that was handed to any writer in a time covered by an index vacuum's decision must not
reach a row with an index entry unless the vacuum cannot judge that entry dead: either the vacuum's bound `low` must be
taken so that returned numbers are never below it (for example by recording, while the vacuum runs, the lowest number any
give-back has brought `next_row` to and keeping every entry above it, or by refusing to give back while an index vacuum of the
table is running), or the give-back of an abort's tail must not lower `next_row` below the vacuum's `low` and instead
leave a gap. It must hold for the abort callbacks (`kc_note_abandoned_all`, `kc_subxact_abort`) and for the existing
give-back of a flush. A test must run an index vacuum that is held between the computation of the ranges and the scan (a
gate in the vacuum, or the cost-delay trick above) while a session aborts and reinserts.

### C14-02 (medium) The VACUUM merge can fail the whole VACUUM, and at the end of the row numbers it always does

**Place.** `kc_vacuum_compact_small` in `kc_vacuum.c`, called before everything else in `kc_relation_vacuum`.

**What happens.** The merge is optional work, but any error in it aborts the vacuum before the index vacuum, the free-space
pass, `kc_meta_freeze` and `vac_update_relstats`. The merge copies rows into new numbers, so a table whose numbers are
used up cannot be vacuumed at all. Measured (`rv04`, PostgreSQL 18): a table with an index and 200 one-row stripes, `next_row`
set near the end:

```
VACUUM endt;            -- ERROR:  columnar table "endt" has no row numbers left   (the same for VACUUM (FREEZE))
```

**Why it matters.** `VACUUM` is what advances `relfrozenxid`, so a failing VACUUM (and every autovacuum of that table, including
the one to prevent wraparound) stops the table's protection against transaction id wraparound until the owner rewrites the
table with `VACUUM FULL` (an `ACCESS EXCLUSIVE` rewrite of the whole table) or `SET ACCESS METHOD`. The warning at 90 percent
that C13-02 added tells the owner, but the vacuum failure itself is new with C13-05. Other errors of the merge (a full disk: the merge needs new
space where a VACUUM used to free some; a cancel by DDL, which a user will see as an autovacuum that keeps being cancelled) have the same
effect on the rest of the vacuum.

**What a fix must achieve.** The vacuum's own work (freeze, index vacuum, free-space pass, truncation) must not depend on
the success of the merge: skip the merge when fewer numbers are left than it would use (and count the numbers it will burn: each
merged row takes a new number), and run it so that an error ends only the merge (for example last, after `vac_update_relstats`,
or in its own transaction). A test: a table at the end of its numbers with many small stripes must still VACUUM.

### C14-03 (medium) A merging VACUUM holds a transaction id and an xmin for its whole run, which holds back the cleanup of every other table

**Place.** `kc_vacuum_compact_small`, the flag clearing.

**What happens.** `PROC_IN_VACUUM` exists so that a lazy vacuum does not hold back the horizons of other backends
(`ComputeXidHorizons` skips such a process; `vacuum_rel` says why it must stay set until commit). The merge clears it for the
rest of the vacuum and the vacuum then holds the xid the merge assigned. Measured (`rv09`, PostgreSQL 18): during the index phase of a
VACUUM that merged 200 stripes, `pg_stat_activity` shows `backend_xid` set and an xmin; a heap table whose 2000 rows were deleted
meanwhile could not be cleaned: `tuples: 0 removed, 2000 remain, 2000 are dead but not yet removable`. The same VACUUM with
`vacuum_compact_stripes = 0`: `2000 removed`. The hold lasts for the merge (which copies every stripe under a
quarter of the stripe size, not only the tiny ones, so a table of many 30 000-row stripes with 128 tiny ones is rewritten as a whole by a
routine VACUUM), the index vacuum, the free-space pass and the truncation; autovacuum does it unthrottled (the copies are not
subject to the cost delay), also when it is vacuuming to prevent wraparound and the failsafe is not active yet.

**Why it matters.** One columnar table's autovacuum can stop the cleanup of all tables in the cluster for as long as it runs (minutes to hours for a
large table), and a replication slot's or hot-standby-feedback consumer sees the same hold. Before C13-05 a VACUUM never did that.
The correctness of the clearing is not in doubt (see "What held"); the price is.

**What a fix must achieve.** Do the merge where it can have its own transaction (a separate step with its own commit before or
after the vacuum's transaction, with the flag handling of `vacuum_rel` untouched), or bound its size and throttle it
by the vacuum cost delay, and document in the README (operations notes) that VACUUM can now hold the horizon. A test: during a
merging VACUUM the horizon of another table is not held after the merge has committed.

### C14-04 (low) The flag is cleared after the vacuum's snapshot has been taken, so a horizon can go backwards for a moment

`vacuum.c` says the flag stays set "until we reset `MyProc->xid/xmin`, otherwise `GetOldestNonRemovableTransactionId()` might
appear to go backwards, which is probably Not Good", and sets it before the snapshot so that the xmin never becomes visible ahead of it.
`kc_vacuum_compact_small` clears it with the xmin already set, so a horizon computed by another backend in the first
milliseconds of a merging VACUUM (the xmin ignored) can be larger than one computed after the clearing (the xmin counted). The heap code is
protected by `GlobalVis` (monotonic), but direct callers of `GetOldestNonRemovableTransactionId` are not. The new dead-stripe pass of C13-01
uses it as the bound for "not removable yet", and a smaller bound includes more stripes, among them stripes whose blocks a vacuum
on another table had already listed as free (C8) and a writer had reused; the pass would then read blocks that are no longer the stripe's.
I could not construct it (the window is the first milliseconds of a merging VACUUM, a second vacuum must compute its horizon in it, a stripe
must have been deleted in the same window, and an index must be built afterwards). A fix for C14-03 that moves the merge out of the
vacuum's transaction removes it; otherwise the pass should take its bound from the free-list horizon the table's metapage records, not from a fresh
horizon.

### C14-05 (low) The index build of old-snapshot rows rests on a compile-time choice of the B-tree build

The dead-stripe pass runs "in the leader only" of a parallel build (`scan == NULL || !IsParallelWorker()`). That is complete only because
`_bt_begin_parallel` always has the leader take part (`DISABLE_LEADER_PARTICIPATION` is a compile-time symbol, not
`parallel_leader_participation`). Measured: with `parallel_leader_participation = off` the leader still took part and the results were
equal (rv02). If a build ever ran only in workers the pass would be skipped silently and old snapshots would lose rows again, which t/063
cannot see (it does not prove that the leader ran the pass, only that one result is right). A comment at the place and a test with a leader-less
build (a debug symbol, or a count of the dead rows passed to the callback exposed to a test module) would keep it.

### C14-06 (note) Nothing I ran distinguishes the flag clearing from its absence

The mutant that never clears `PROC_IN_VACUUM` (a one-line change, clean build) passed t/004 four times of four (the writer measured
one failure in six on the first version, so four passes prove little), t/067, and my reader loop (about 8 600 scans with a merging
VACUUM running, 0 anomalies; the unmutated build 81 000 scans, 0 anomalies). The race (a reader's catalog scan straddling the vacuum's
commit) is too narrow for loops of this length; a deterministic test needs a gate between the commit-time check of the stripes and the
commit, or a count of how many snapshots listed the xid. I read the argument through `GetSnapshotData` and the xid assignment
(snapshot taken before the assignment has `xmax` at or below the xid, snapshot taken after lists it, the clearing is under
`ProcArrayLock` in both copies of the flags) and found it correct, and `PROC_VACUUM_FOR_WRAPAROUND` is kept by the way the bit is cleared.

### C14-07 (note) Small things

* `kc_meta_allocate` refuses a reservation of 64 numbers when fewer than 64 are left although a few rows would fit: harmless (the
  owner must rewrite the table anyway) but the message says there are none left.
* Each extension of a reservation (about a dozen per full stripe of a bulk load) updates the claim row of the stripe catalog and may pass the
  command counter: more catalog churn for bulk loads, which the README does not say.
* An aborted merging autovacuum leaves orphan copies and burns the numbers of the copied rows (a DDL storm cancelling autovacuum
  repeatedly rewrites the stripes each time); nothing tests it.

## What held

* **C13-01.** CREATE INDEX, CREATE UNIQUE INDEX (nulls in the first and later keys, composite), partial and expression
  indexes, REINDEX, built in parallel with the leader's pass, with an older REPEATABLE READ snapshot taken before the compaction: the index scan
  and the sequential scan agree (rv02, t/063 on both majors). Uniqueness is not tripped by the copies (they are put in the
  second spool of the build, which `_bt_load` merges without the uniqueness check; nulls are not special in `_bt_build_callback`). The table is
  append-only (no UPDATE or DELETE), so only compaction supersedes a stripe, which is why the dead list is complete.
* **C13-02, apart from C14-01.** No two rows shared a TID and every index found every row in the generated mixes (rv07, rv08: seeds 1, 5, 7, 11;
  minimum reservation 64, 1 and 0; 2 concurrent writers, savepoints, nested savepoints, exception blocks, rollbacks, a concurrent
  VACUUM and `columnar_compact`; PostgreSQL 18 and 19). An aborted writer's tail starts after the numbers of the rows that were put in
  the buffer, as the design says, so a number with an index entry is not reused by the abort path. Crash and standby: the metapage is
  logged as a full image on every change (allocate, extend, return), so replay and a promoted standby agree (rv07, rv10). The growth in place uses
  the same reservation lock and command passing as a flush.
* **C13-03.** Decoding with `getmissingattr`: t/064 on PostgreSQL 19.
* **C13-04.** amcheck with `heapallindexed` against a table written by two sessions at the same time: 80 calls, 0 false reports (rv03);
  t/065 on PostgreSQL 19. The concurrent builds stay refused (`indisready` is false while they build).
* **C13-05, the merge.** With the flag cleared and the id still unassigned, the merge, the index entries for the copies, the free-space pass and the
  truncation in one transaction gave the same rows as the heap twin, in readers' loops, after crashes with the id assigned (rv06, rv07, rv10), with a
  promoted standby afterwards. A VACUUM in a session with `default_transaction_read_only = on` works (rv04) and so does `VACUUM (ANALYZE)`.
  The flag clearing is correct by reading (C14-06); the guard (no id yet) covers every path in `vacuum_rel` that I could find.
* **C13-06.** The Asserts held in all runs (assertion builds).

## Mutants

| Id | One-line change | Clean build | Result |
|---|---|---|---|
| M1 | never clear `PROC_IN_VACUUM` (the first version of C13-05) | yes | survived t/004 (4 runs) and rv06 (8 600 scans, 0 anomalies); t/067 not run against it; see C14-06 |

I built this one mutant only; the writer's list (G01 to G16) was not repeated. The new test t/066 passes on PostgreSQL 19 with the code of C14-01 in it, so the
writer's suite does not cover that race. My control (the script of C14-01 without the first abort) passes and the same script with the abort fails,
which shows that the script discriminates.

## Not covered

* Disk full during the merge, `ENOSPC` in the middle of the copies (read, not run).
* A role with only `MAINTAIN` vacuuming a table that needs the merge: my probe could not run (as a non-superuser even a heap table of the
  same database was "not found" by VACUUM in the private cluster, a setup problem I did not resolve), so it is untested whether the merge
  runs for such a role; the code takes no privilege check of its own.
* A vacuum cancelled by a conflicting DDL in the middle of the merge, and an anti-wraparound autovacuum that merges (read only).
* Hot standby with `hot_standby_feedback` and long queries during a merging VACUUM, logical decoding or a publication that includes the output of the merge
  (C13-08 says the stripe catalog is visible to `test_decoding`; the merge adds catalog traffic to that).
* The horizon hole of C14-04 (not constructed), the exact timing of the flag race (C14-06).
* The C14-01 race in the autovacuum setting without a slowed vacuum (shown with the cost-delay trick, not under a natural race), and the same race
  in a promoted standby (it is a primary-side decision).
* The regression suite and the full TAP suite on both majors (the writer ran them; I ran the five new test files on PostgreSQL 19 only and t/004
  against a mutant), the in-tree meson run, Windows.

## Second look at the fixes

Scope: commits 38bcd6148e, f5f1f77b5f, b077a254e7, 923f7c2a50 on `col-c13` (after 7d927f54a6), which answer C14-01 to C14-07.
Method as before: reading the diff and the code around it; private installs of both majors, the extension built clean with `-Werror`
(0 warnings); my own scripts of the first round re-run against the new code, plus new ones; five one-line mutants of the lock.
At most three test processes at a time.

### What was run

| Run | PostgreSQL 18 | PostgreSQL 19 |
|---|---|---|
| Writer's new tests t/068 (give-back and vacuum) and t/069 (safe merge, gate, snapshot of another session) | pass | pass |
| rv01 (abort, slowed index vacuum, inserts; C14-01 as I reproduced it) | now 20 of 20 found (was 0 of 20); 1 280 row numbers burned by 20 inserts, the gap the fix chooses | same |
| rv11 (the same with a UNIQUE index, then a duplicate insert) | duplicate refused, 1 row | not run |
| rv04 (a table at the end of its numbers with 200 small stripes: `VACUUM`, `VACUUM (FREEZE)`, read-only session, `VACUUM (ANALYZE)`) | not run | pass (the MAINTAIN part is my setup problem, as before) |
| rv12 (fault injected into the merge, `VACUUM a, b`: the merge of `a` fails with a WARNING, `b` is then vacuumed and merged; rows equal the twin, indexes equal scans, `columnar_check*` empty; afterwards a heap table's dead rows are removed at once, so no xid or xmin is held after the error; three fault points) | pass | not run |
| rv06 (2 readers against a writer that makes savepoint stripes and runs VACUUM) | not run | 0 anomalies; 3 stripes at the end, so the merge being last still keeps up |
| rv08 (generated mix, 2 writers, VACUUM and `columnar_compact` loop, 370 235 rows) | not run | pass |
| rv10 (VACUUM killed with `stop -m immediate` three times, standby, promote) | not run | pass |
| rv07 (4 rounds of crashes under the mixed load, standby, promote, writes) | not run | pass |
| rv13 (new): VACUUM stopped by the gate after its merge, so that the merge's id is held and the copies are uncommitted; then the standby and the primary are checked against the heap twin; round 1 crash and recovery of the primary, round 2 promote the standby and VACUUM on it | not run | 26 checks pass |

TRAP lines: none in the runs I looked at (assertion builds).

### The lock of C14-01 (`kc_vacuum_lock`)

Read interleaving by interleaving. The index vacuum takes the lock EXCLUSIVE (waiting) before it takes the reservation lock and reads the ranges, and holds it until
after the last index scan (a transaction-level lock: an error or a cancel releases it). The only code that lowers `next_row` is `kc_meta_return`, whose only caller is
`kc_give_back_tail`, which takes the lock SHARE without waiting and leaves the numbers a gap when that fails.

* A give-back that finished before the vacuum took the lock: the numbers may be reused, but the vacuum reads the metapage after the lock, so `next_row` and the claims are
  current, and a reservation made after the give-back has a live claim or a stripe (a claim half made is excluded by the reservation lock, as before).
* A vacuum that starts after a give-back took SHARE: its wait is for that SHARE only, held for the metapage update; it then reads the lowered `next_row`.
* A give-back while the vacuum holds the lock or waits for it (a waiting EXCLUSIVE conflicts with a new SHARE request): the numbers stay a gap. This covers the give-back of a flush
  and of a released empty reservation too, so no path lowers `next_row` while the ranges are in use.
* Autovacuum: the same code. Parallel index vacuum workers: none, the access method vacuums its indexes in the process that vacuums the table. Two vacuums of one table exclude
  each other by the table lock.
* A crash: the lock is in memory; after the restart no vacuum runs and `next_row` is whatever the last logged image says.
* Deadlock and waits: no process waits for anything while holding SHARE (taken without waiting, released at once); the vacuum waits for it before it holds the reservation lock;
  the advisory tag `(0x4B43, relid, 8)` cannot be reached by the SQL advisory functions (their last field is 1 or 2). Writers never wait for the vacuum: in t/068 and in my
  run an insert and commit went through while the vacuum stood in the gate holding the lock.
* Mutants (clean build each, run against t/068): M5 no lock in the vacuum, M6 the give-back ignores the lock, M8 the lock removed in front of the reservation lock: all killed.
  M7 (the vacuum never releases the lock before the end of its transaction) survives, correctly: it only holds longer. M9 (the lock taken after the ranges are read) survives: see C14-08.

### The merge in a subtransaction (C14-02, C14-03)

* Locks: those taken in the subtransaction are released if it rolls back and passed to the vacuum's transaction if it is released, as for any internal subtransaction.
* Transaction id and flag: the flag is cleared before `BeginInternalSubTransaction`, so the first id is assigned inside the subtransaction (the parent's first), with the flag cleared,
  and nothing sets it again: after an error the state is the same as after a success (flag cleared, an id held until the vacuum ends). The next relation of `VACUUM a, b` gets a
  new transaction, which resets the flags (rv12 showed `b` merged after the failure of `a`).
* Errors: the maintenance buffer is discarded, `kc_write_set_maintenance` is reset by the `PG_FINALLY` in `kc_compact`, the abort of the subtransaction frees its buffers, and the
  copies and the deletion marks go with it. Class 57 errors (cancel, shutdown) are re-thrown and abort the VACUUM, which is right; out of memory and a full disk (class 53)
  become a WARNING.
* The merge is the last step, so a failure or a cancel loses nothing of the vacuum's own work: the relation statistics are in-place updates, the metapage and the free list are logged images.
* The skip at 90 percent and the cap: `vacuum_compact_rows` (default 1 000 000, 0 for none, superuser) caps the rows picked, the rest are merged by later vacuums; only stripes under a
  sixteenth are merged now. A table at the end of its numbers is vacuumed (rv04, t/069).
* The horizon: the xid the merge needs is held until the vacuum ends, so the effect of C14-03 is bounded to the merge and what follows it (the truncation) instead of the whole index
  phase, and the README says so. Accepted. C14-04 stays documented only; accepted for the reasons given there.

### Findings

**C14-08 (low) No test pins the order "lock first, ranges second".** Mutant M9 takes the lock after the ranges are read and passes t/068 (the gate stands after both, so the writer's
give-back is refused either way). The window it reopens is the time between the read of the ranges and the lock, microseconds, but it is the exact interleaving of C14-01. A second
developer gate between the lock and the read of the ranges would pin it. Not a defect of the code.

**C14-09 (note) The give-back is refused for the whole run of an index vacuum.** Every commit or abort during the vacuum burns up to the small reservation (63 numbers) instead of
returning it. Measured: 20 single-row inserts used 1 280 numbers during the slowed vacuum. For a vacuum of hours on a table with a very high commit rate this is the old waste in a
smaller form (at 100 000 commits per second, about 0.2 percent of the numbers per hour of vacuum); the warning at 90 percent and the way out remain. One sentence in the README.

**C14-10 (note) The merge is skipped without a word when the vacuum already has a transaction id.** The guard returns when `GetTopTransactionIdIfAny()` is valid. The merge now comes after
`kc_catalog_reconcile`, which deletes catalog rows (and so assigns an id) when it finds rows of files that no longer exist; in that vacuum the merge does not run. Rare and harmless (the
next vacuum merges; rv06 and the generated mixes show the merge keeps up). A DEBUG message would make it visible.

### Judgement

C14-01 is closed: for the interleavings above the lock holds, the reproduction that failed on both majors now passes (the unique index refuses the duplicate), t/068 kills the mutants of the
lock that matter (M5, M6, M8; M7 is equivalent, M9 is C14-08), and there is no deadlock or writer wait by construction and by test. C14-02 and C14-03 hold: the merge cannot fail the
vacuum (also at the end of the numbers: rv04, t/069, rv12), and the hold of the horizon is bounded and documented. The gate and t/069 give the flag clearing the test that C14-06 lacked
(I did not rebuild the no-clearing mutant against it). Crash recovery with an uncommitted merge, a standby during it, and a promoted standby after it (rv13), and the crash, generated-mix and
reader runs, found nothing.

Counts of this second look: critical 0, high 0, medium 0, low 1 (C14-08), note 2 (C14-09, C14-10).

**Judgement: `col-c13` can be merged.** None of the three new items is a reason to hold it back; C14-08 asks for one more test, C14-09 for a sentence in the README.

### Not covered in the second look

The no-clearing mutant against t/069 (not rebuilt), a MAINTAIN-only role, a full disk in the merge, cancels at each point of the merge, wraparound autovacuum, and the PostgreSQL 18 runs of
rv04, rv06, rv07, rv08, rv10 and rv13 (the same scripts passed on 18 on the earlier code, and on 19 on this code).

### Fixes of the low finding and the two notes (2026-11-22)

* **C14-08 fixed.** A developer gate at point 8 (`KC_RES_VAC_LOCKED`) stands in `kc_index_vacuum` between the vacuum lock and the read of the ranges. `t/070` stops the vacuum
  there, lets a writer that aborted insert again, and checks that the tail was not given back (the insert takes 64 row numbers, not the few of a give-back). Mutant M9 (the lock taken
  after the ranges are read, the gate left in front) fails that check; the clean code passes it. The vacuum's lock is taken before the ranges, and a test now says so.
* **C14-09 documented.** The README says that the give-back is refused for the whole run of an index vacuum, up to 63 row numbers per commit or abort, with the rate worked out
  (about 0.2 percent of the numbers per hour at 100 000 commits per second) and the 90 percent warning as the way to see it. No code change.
* **C14-10 fixed.** A merge that is skipped because the `VACUUM` already has a transaction id now says so at `DEBUG1`. `t/070` drops a columnar table in a server without the library, so
  that the reconcile at the start of the next `VACUUM` deletes its catalog rows, and checks the message and that no stripe was merged; the next `VACUUM` merges. Mutant M10 (the message
  at `DEBUG5`) fails the check.
