<!-- SPDX-License-Identifier: AGPL-3.0-only -->
# Adversarial review of the third fix round: contrib/kingres_columnar 0.1

Reviewer: an independent reviewer (not the author). The change under review
answers `kingres/docs/reviews/2026-11-16-kingres-columnar-0.1-second-fixes.md`
(C3-01 to C3-06). The earlier reviews are
`kingres/docs/reviews/2026-11-15-kingres-columnar-0.1.md` (C1-01 to C1-14) and
`kingres/docs/reviews/2026-11-15-kingres-columnar-0.1-fixes.md` (C2-01 to
C2-09). The reviewed state was exported with `git archive` (paths
`contrib/kingres_columnar`, `kingres/dev`, `kingres/docs`) into a private work
area; the working tree was not used for any result and nothing in the
repository was changed except this file. Line numbers are those of that
export.

## Summary

Five of the six findings of the second review are fixed as far as I could
attack them, and none of the C1 or C2 fixes failed again. The central fix of
the round, the mapping of the conflict-out to the top-level transaction
(C3-01), holds: 21 hand-built shapes (suboverflowed writer, savepoints, an
exception block, three levels, prepared writer, reader inside a savepoint)
agree with heap on both majors, and 60 extended random three-transaction
schedules per major give no anomaly. The hold on write buffers (C3-02) is
released on every way of abandoning a scan that I found, on both majors.

The pattern of the earlier reviews repeats in two places, both in code of the
round. The merge of a subtransaction's buffer "reserves first and publishes
last" **per relation**, but the commit of a subtransaction merges the buffers
of every relation it wrote, one after the other: when the merge of a later
relation runs out of memory, the rows of the earlier relations stay in the
parent and are committed although the subtransaction aborted (C4-01,
reproduced on both majors). And "compaction takes no part in serializable
isolation" is true for other transactions but not for the compacting one: a
REPEATABLE READ or SERIALIZABLE transaction that calls `columnar_compact`
reads rows committed after its snapshot afterwards, and a SERIALIZABLE one
commits (C4-02, both majors). The soft memory limit, measured as asked, turns
a loader that uses exception blocks into a table of one stripe per few rows
(C4-03). The fix of C3-05 is correct for the shapes in the test and incomplete
for the next ones (C4-04).

The tests of the round are real tests (each one failed when I removed its
fix, except as listed), but five of the new mechanisms are not tested at all
and survive removal: the merge under memory exhaustion, the flag handling
across a merge, the reader's raised row limit, the recovery conflict of the
freeze record, and the stale-catalog tolerance (C4-05).

| Severity | Count |
|---|---|
| critical (wrong answer, lost or doubled committed rows, crash, a SERIALIZABLE anomaly heap prevents, an unreadable table) | 2 |
| high | 0 |
| medium | 1 |
| low | 1 |
| note | 2 |

| Id | Severity | Component | Statement |
|---|---|---|---|
| C4-01 | critical | `kc_write.c` `kc_subxact_callback` (line 1385), `kc_buf_merge_into` (line 1338) | The commit of a subtransaction merges the buffers of its relations one at a time. An out-of-memory error in the merge of one relation leaves the rows of the relations merged before it in the parent, and they are committed; the subtransaction aborted. |
| C4-02 | critical | `kc_vacuum.c` `kc_compact`, `kc_catalog.c` `kc_catalog_snapshot` | `columnar_compact` in a REPEATABLE READ or SERIALIZABLE transaction reads with a fresh snapshot and writes the rows it copies as the transaction's own stripe, so the transaction afterwards sees rows committed after its snapshot (3 rows, then 4) and under SERIALIZABLE records no dependency. The README says compaction "changes nothing logically". |
| C4-03 | medium | `kc_write.c` `kc_flush_largest` (line 401), `kc_buf_flushable` (line 376) | When the buffers that merged subtransactions left in an outer subtransaction use the memory allowance, every later row is flushed by itself: 250 000 rows in 250 exception blocks became 14 519 stripes and a 113 MB relation (the same rows in one statement: 2 stripes, under 1 MB). |
| C4-04 | low | `kc_storage.c` `kc_check_snapshot_age` (line 740) | A table created after a snapshot and then truncated, vacuumed full or rewritten still gives that snapshot 40001 where heap answers an empty table; the C3-05 rule looks at the last file, not at the table. |
| C4-05 | note | tests | Mutants that survive: merge under memory exhaustion, flag OR at merge, the reader limit, the freeze conflict, the 13-column catalog. |
| C4-06 | note | `columnar_check` | After `VACUUM FULL` or a rewriting `ALTER TABLE`, `columnar_check()` reports `orphan_catalog_rows` until the next `VACUUM` (not from this round). |

## Fate of C3-01 to C3-06

| Id | Fate | Evidence |
|---|---|---|
| C3-01 | Fixed | 21 shapes (writer: plain flush by size, savepoint released, suboverflowed writer with a savepoint and with an exception block, aborted savepoint then the parent writes more, three nested levels, a savepoint left open; reader: plain, inside a savepoint, writer prepared and committed by a third session), serializable write skew, heap twin against columnar: 0 differences on 18.7 and 19rc2. 60 random schedules of three transactions per major (with `READ ONLY` readers, reads inside savepoints, one compaction, 70-subtransaction writers that overflow the subtransaction cache, rolled-back and nested writes): 0 non-serializable outcomes, 0 unexpected errors, both majors. Removing the mapping (mutant M01) fails 38 checks of `t/009`. |
| C3-02 | Fixed | 11 ways to end or abandon a scan (see "What held"), both majors: stripes are written by size after every one of them, backend memory stays at 2.3 MB after 20 000 caught errors in one transaction, no "resource was not closed" in the logs. |
| C3-03 | Fixed for every writer path; see C4-03 for what the merge still does to memory | COPY, `INSERT ... SELECT`, `CREATE TABLE AS`, `VACUUM FULL`, `ALTER TABLE ... TYPE`, round trip through heap, `columnar_compact`, 30 merged subtransactions at commit and at `PREPARE TRANSACTION`, the tail kept after a flush: with `max_stripe_rows` at 1000 every stripe had at most 1000 rows and no row was lost or doubled. A crafted catalog and header with 40 000 001 and 2 147 483 647 rows and an unchanged footer are refused ("chunk group rows do not add up") without allocating from the count. |
| C3-04 | Fixed for other transactions; the compacting transaction itself is wrong (C4-02) | Seven serializable schedules of reader, writer and compactor (compaction before, after, concurrent with, and in the same transaction as the real write; with the writer committed or not): outcome and reads equal to the heap twin on both majors. |
| C3-05 | Fixed for the shapes in the test, incomplete (C4-04) | 25 shapes of an old snapshot against creation, TRUNCATE, rewrite, `SET ACCESS METHOD`, `SET TABLESPACE`, attach and detach: all agree with heap or give 40001 where the file replaced content, except the table-created-after-the-snapshot shapes listed in C4-04. |
| C3-06 | Fixed | `t/009` rewritten: every case asserts stripes and subtransaction stripes from the writer's own session; the cases really flush by size (the probes are asserted, and removing the C3-01 fix fails 38 of them). |

## Scope and method

Read: the whole diff of the round; `kc_catalog.c` (visibility, load, the new
conflict report), `kc_write.c` (buffers, flush, merge, holds), `kc_scan.c`,
`kc_storage.c`, `kc_vacuum.c`, `kc_am.c`, `pg_compat.h`, the README, the three
test files; and from the PostgreSQL branches `kingres-18` and
`kingres-19` the files `heapam.c` (`HeapCheckForSerializableConflictOut`),
`predicate.c` (`CheckForSerializableConflictOut`), `xact.c`, `subtrans.c`,
`resowner.c`, `portalmem.c`. `HeapCheckForSerializableConflictOut` and
`CheckForSerializableConflictOut` are identical on both majors. Compared line
by line with `kc_report_conflict_out`:

* The horizon skips (`xid` older than `TransactionXmin`, and the same for the
  top-level id) are unreachable: a stripe row is reported only when
  `XidInMVCCSnapshot` said the id is in the snapshot, which implies it is not
  older than the snapshot's xmin, and a subtransaction id is never older than
  its top-level id's lifetime. Both are defensive and equal to heap; removing
  either (M04a, M04b) changes nothing in any test, as expected.
* The reader's own transaction: equal to heap. An own aborted subtransaction
  is caught by the in-progress and did-commit test; in the suboverflowed case
  of the reader itself the lookup falls through to the own top-level id and
  returns.
* An aborted writer subtransaction: equal to heap's `HEAPTUPLE_DEAD`. In the
  suboverflowed case `TransactionIdIsInProgress` answers by the top-level id
  and the report is made for an aborted subtransaction (a false dependency,
  never a missed one); heap behaves the same through
  `HeapTupleSatisfiesVacuum`.

Built and run on PostgreSQL 18.7 and 19rc2, both with assertions, each copied
into a private area with the prefix fixed and used out of tree with
`kingres/dev/ext.sh` (`EXT_WERROR=1`, no warning). Everything ran under
`nice`, with at most 4 build jobs, on ports 59900 to 59999; every server I
started is stopped. Load average 1 to 60 from other jobs. The experiments
were TAP scripts with `PostgreSQL::Test::Cluster` and `BackgroundPsql`. The
mutants of the "tests" section are copies of the extension with one change
each, built into a private copy of the installation, run with the one test
file that belongs to the fix.

| Experiment | What was run |
|---|---|
| Subtransactions and serializable (`e_sub`) | 7 writer shapes x 3 reader shapes (plain, inside a savepoint, writer prepared), heap and columnar, both majors. |
| Random schedules (`e_ssi`) | The generator of `t/009` extended: compaction by one transaction, reads inside savepoints, `READ ONLY` transactions, writers with 70 subtransactions (suboverflowed), nested and rolled-back writes; 60 schedules per major, seeds 918 and 919 (an earlier run stopped on my own harness: two compactions of one table block each other). |
| Holds (`e_hold`) | `WITH HOLD` across commit, reparented cursor, `RELEASE` with an open cursor, `PREPARE TRANSACTION` with an open cursor, 3 000 open cursors, 20 000 caught errors, timeout in a scan, `COPY (...) TO`, `INSERT ... SELECT` from itself, prepared statement with a join. |
| Maintenance (`e_maint`, `e_rrc`, `e_phantom`, `e_13col`) | Seven serializable schedules with compaction; REPEATABLE READ and SERIALIZABLE compactor; a catalog with 13 columns installed from the previous round's SQL script. |
| Old snapshots (`e_snap`) | 25 shapes of create, TRUNCATE, rewrite, tablespace, access method changes, partitions, matview refresh against a REPEATABLE READ snapshot taken first, heap against columnar. |
| Limit (`e_limit`, `e_limit2`, `e_craft`, `e_foot`) | Every writer path under `max_stripe_rows = 1000`; the setting from a `SECURITY DEFINER` function, from `ALTER DATABASE`, by an ordinary role; crafted row counts in the catalog and in the stripe header. |
| Memory (`e_mem`, `e_mem3`, `e_merge`, `e_merge2`) | Merged buffers against `write_buffer_memory`; address-space limit set on the backend in front of the merge. |
| Standby and crash (`e_standby`, `e_freeze`, `e_crash`, `e_prep`) | Hot standby with `wal_consistency_checking = all` on both nodes: `VACUUM FULL` and `VACUUM` against standby queries, the freeze record with and without its recovery conflict, immediate stop and recovery with a prepared transaction, promotion; `VACUUM FULL` against a prepared writer. |
| Read paths (`e_paths`) | 19 statements that read a table (select, copy, cursor, CTAS, `INSERT ... SELECT`, PL/pgSQL, forced parallel plan, EXPLAIN ANALYZE, ANALYZE, inspection functions, TABLESAMPLE, FOR UPDATE, join, subquery in a write) as the reader of a write-skew schedule, writer with 1 and with 1 500 rows, heap against columnar. |

## The tests of this round

Method for each fix: a copy of the extension with the fix broken in one place
(or removed), built with `COPT=-Werror` (a few with warnings off), and the one
test file of the round that claims the fix. A mutant "fails" when the file
reports failed checks or dies. Numbers are the failed checks of the file.

### `t/009_serializable.pl` (rewritten)

Claims: the writer's rows reach the table in every way a stripe can be
written, the conflict-out is reported for each, and the outcome equals heap's;
thirty seeded random schedules are explained by a serial order.

Preconditions: established. Each case reads the writer's stripes and the
number written by a subtransaction from the stripe catalog in the writer's
own session (the probe is asserted with `like`), the random schedules assert
`sub>=1` for every savepoint or exception-block write of a non-small size, and
`size`, `exact`, `mem` and `dflt` really flush by size or by memory (the
last round's defect, the `SET` after creation, is gone: options are set per
table before the rows go in). The file runs 1 766 checks in 58 seconds.

| Mutant | Result |
|---|---|
| M01: conflict reported with the stored (sub)transaction id | 38 failed |
| M02: report also for an aborted writer subtransaction | 4 failed |
| M16: conflicts never reported by any scan | 154 failed |
| M16b: every catalog load reports (inspection functions, estimates) | 3 failed, only because the probes are inspection calls inside serializable transactions; no check names the claim "inspection functions do not report" |
| M04a, M04b: the two horizon skips removed | pass (the skips are unreachable, see above) |

What it does not cover although its name suggests it: writers with more
subtransactions than the cache holds, readers that are themselves in a
savepoint, `READ ONLY` readers, compaction inside a schedule, a prepared
writer in the random part (the fixed part has `PREPARE TRANSACTION`). I ran
all of these (`e_sub`, `e_ssi`): no difference from heap.

### `t/014_buffers.pl`

Claims: no scan, however it ends, keeps a hold (C3-02); buffers merged from
subtransactions are cut into stripes of at most the limit, no stripe is
written that the reader refuses (C3-03).

Preconditions: established for part 1 (the 16 cases assert five stripes
before COMMIT after the caught error, the plan is asserted to use the
Columnar Scan node or not, the memory case asserts at least 100 stripes and a
context size). Part 2 asserts the numbers of stripes of every merge.

| Mutant | Result |
|---|---|
| M08: the hold is not remembered in the resource owner (the C3-02 fix) | 20 failed |
| M17: `kc_scan_drop_hold` does not release the hold | 1 failed (check 36 only) |
| M09: the flush does not cut the buffer at the limit (the C3-03 fix) | the part-2 run dies: with assertions the server aborts at `Assert(count <= kc_stripe_limit(...))` (exit 29 after 2 checks); without assertions it would write the stripe and the check "cuts them into full stripes" fails |
| M09b: the setting `max_stripe_rows` ignored | 3 failed |
| M15: the tail kept after a flush is not removed from the buffer | 2 failed |
| M10: the reader's limit back to 40 million rows | passes |

What it does not cover: the merge under memory exhaustion (C4-01), more than
one relation in a merging subtransaction, a stripe above 40 million rows (no
check creates one; the claim "the reader accepts what the writer ever wrote"
rests on a `StaticAssert` and on reading the code), the memory the merged
buffers hold (C4-03), `WITH HOLD`, 3 000 open cursors. I ran the cursor
cases: they hold (see "What held"). Part 1 would not have found C4-03 because
its memory case has no merged buffer.

### `t/015_creation_compaction.pl`

Claims: a snapshot older than the first file of a table reads an empty table;
`columnar_compact` creates no dependency and hides none.

Preconditions: established for compaction (each case asserts the number of
stripes before and after, and that the compaction returned 1). For part 1 the
case list is the one of the review; "an older snapshot" is really older
(taken before the command and tested by the heap run, `is($heap, $expect)`).

| Mutant | Result |
|---|---|
| M11: the first-file rule removed (the C3-05 fix) | 6 failed |
| M12: the rule without the `created_relid` test | 2 failed |
| M13: `REPLACES_CONTENT` never set | 2 failed |
| M05: a reader reports a dependency on maintenance stripes | 2 failed |
| M06: the per-row conflict-in check runs for compaction | 5 failed |
| M07: stripes never marked as maintenance | 3 failed |
| M07b: the flush check runs for maintenance buffers | 5 failed |
| M18: the merge of buffers drops the "a user row is in here" flag | passes |

What it does not cover: C4-02 (the compacting transaction's own reads; the
file checks only third parties), C4-04 (tables created after the snapshot and
then rewritten), a catalog with 13 columns (the claim is only in a comment),
the flag after dump and restore (my check: dump and restore rewrite the
rows, so the flag is not carried; the stripes written by the restore are
ordinary ones).

### Not tested by any file, and surviving removal

* The merge "reserves first, publishes last" (the claim that closes the
  third review's "Not reproduced"): no test injects a failure. It does not
  hold across relations (C4-01).
* M30: the recovery conflict of the freeze record removed from the redo
  routine: `t/002_standby.pl` passes (the wraparound file was skipped, as in
  the author's run: the `xid_wraparound` module is not installed in the
  installations I was given). My experiment (`e_freeze`): with the conflict
  the standby session holding an old snapshot is terminated ("terminating
  connection due to conflict with recovery"); without it the same session
  reads 0 rows after `TRUNCATE` and `VACUUM` on the primary, which is what a
  heap table gives too, so the missing test hides no wrong answer, only the
  difference between "terminated" and "0".

## Findings

### C4-01 (critical) A failed merge of one relation leaves the rows of the other relations in the parent

Place: `contrib/kingres_columnar/src/kc_write.c`, `kc_subxact_callback`
(line 1385) loops over the relations of the transaction (`hash_seq_search`)
and, for each, merges the committing subtransaction's buffer into the
parent's with `kc_buf_merge_into` (line 1338) and frees it. The routine is
atomic for one buffer ("reserve first, publish last"); the loop is not atomic
for several. The state of relation A is published (its rows now belong to the
parent subtransaction) before relation B is merged. If the merge of B fails
(`kc_buf_reserve` or one of the copies runs out of memory), the commit of the
subtransaction raises an error ("WARNING: AbortSubTransaction while in COMMIT
state"), the subtransaction is aborted, `SUBXACT_EVENT_ABORT_SUB` frees only
the buffers that still carry the child's id, and the buffers already merged
stay in the parent and are written at its commit. The same loop also leaves
the rows of the relations merged first when the error is caught by an
exception block, which is the normal use of subtransactions.

Steps (a private cluster with the extension, 40 small columnar tables `a1` to
`a40`, one columnar table `b` with `stripe_row_count` at its maximum; the
backend's address space is limited from outside with `prlimit` after the first
3 000 000 rows of `b` are buffered; the limit is VmSize + 70 MB):

```
-- setup
CREATE TABLE b (x int) USING columnar;
SELECT kingres.columnar_set_options('b', stripe_row_count => 10000000, chunk_row_count => 10000);
CREATE TABLE a1 (x int) USING columnar;  ... a40
-- session
SET kingres_columnar.write_buffer_memory = '2000MB';
BEGIN;
INSERT INTO b SELECT generate_series(1, 3000000);
-- from a shell: prlimit --pid <backend pid> --as=<VmSize of the backend + 70 MB, in bytes>
DO $$ BEGIN BEGIN
  INSERT INTO a1 VALUES (1); INSERT INTO a2 VALUES (2); ... INSERT INTO a40 VALUES (40);
  INSERT INTO b SELECT generate_series(1, 3000000);
EXCEPTION WHEN OTHERS THEN RAISE NOTICE 'CAUGHT: %', sqlerrm; END; END $$;
-- from the shell: prlimit --pid <pid> --as=unlimited
COMMIT;
SELECT (SELECT count(*) FROM a1) + ... + (SELECT count(*) FROM a40);
```

Observed: `NOTICE: CAUGHT: out of memory` preceded by `WARNING: AbortSubTransaction
while in COMMIT state`; the commit succeeds; `b` has 3 000 000 rows (the child's
rows are gone, as they must be); the sum over the `a` tables is **14** on
18.7 and **22** on 19rc2: the single rows that the aborted subtransaction wrote
into the first 14 (22) tables in the order of the state table are committed.
The same at every limit I tried from 70 MB to 135 MB (six values, both
majors); at 50 MB the error comes earlier, in the insert, and the sum is 0; at
150 MB no error and the sum is 40 (the control). Heap: the whole
subtransaction disappears.

Expected: 0 rows in every `a` table. The subtransaction aborted, so none of
its rows may exist; a table that is not even the one that failed must not
change.

Direction: make the merge of all relations atomic: first reserve in every
parent buffer (the loop that can fail), then publish all (the loop that
cannot); or avoid the copy: re-label the child's buffer with the parent's
subtransaction id when there is no parent buffer for the relation, and merge
into an existing parent buffer only after all reservations succeeded. Add a
test with a failure injected into the second relation (an `injection_points`
style hook, or `max_stripe_rows`-like developer setting that makes the
reservation fail).

### C4-02 (critical) A compacting transaction reads rows committed after its snapshot

Place: `kc_vacuum.c` `kc_compact` reads with `KC_SCAN_MAINTENANCE`, which
replaces the snapshot by a fresh `GetLatestSnapshot()` copy
(`kc_catalog.c`, `kc_catalog_snapshot`, the non-MVCC branch), and writes the
copies through the ordinary buffer, so they become stripes of the compacting
transaction itself. Every stripe of the running transaction is visible to it
in full (`kc_tuple_visible`, "the own transaction's insertions are all
visible"), and the stripes it replaced are deleted by it, so for the rest of
the transaction the table is read from the new, own stripes. A row committed
by another transaction after the compactor's snapshot but before the
compaction is in those stripes, so it appears in the compactor's later
reads. The README says compaction "changes nothing logically"; for the
compacting transaction it adds rows.

Steps (both majors; same result with `REPEATABLE READ`):

```
CREATE TABLE ca (v int) USING columnar; INSERT INTO ca VALUES (1); INSERT INTO ca VALUES (2); INSERT INTO ca VALUES (3);
CREATE TABLE obs (before_n int, after_n int);
-- session 1
BEGIN ISOLATION LEVEL SERIALIZABLE;
SELECT count(*) FROM ca;                      -- 3
-- session 2 (autocommit)
INSERT INTO ca VALUES (100);
-- session 1
SELECT kingres.columnar_compact('ca', 100);   -- 1
SELECT count(*) FROM ca;                      -- 4   (heap twin: 3)
INSERT INTO obs VALUES (3, 4);
COMMIT;                                       -- succeeds
```

Observed (18.7 and 19rc2): the transaction reads 3, then 4, and commits with
`obs = (3, 4)`; no serial order of the two transactions produces that, and no
dependency is recorded for the inserted row (the stripe is the transaction's
own, and the maintenance flag removes the report for everyone else). Heap
twin: 3 and 3. With `REPEATABLE READ` the second read is 4 as well.

Expected: the compacting transaction keeps reading what its snapshot sees
(compaction must copy the rows its own snapshot sees and leave the other
stripes to their writer's visibility), or compaction must be refused when
the transaction has a snapshot older than the stripes it would merge (an
error is the right answer where it cannot be done), or the stripes it
writes must be invisible to its own snapshot's rows. Not new in the round (by
reading, the compaction read path did not change; I did not run the earlier
code), but the round's C3-04 extends it: the maintenance flag now also removes
every dependency a reader would have reported on the transaction's copy.

### C4-03 (medium) The soft memory limit makes every row after it a stripe of its own

Place: `kc_write.c` `kc_flush_largest` (line 401) and `kc_buf_flushable` (line
376). A buffer is flushable only by the subtransaction that owns it. The
merge (C2-02, kept by C3-03 without a flush) moves the rows of every
committed subtransaction into the parent's buffer, so the parent's buffer
grows past `write_buffer_memory` and cannot be flushed; from then on the
test `kc_total_bytes > limit` is true at every row, and the only flushable
buffer is the running child's, which is flushed as it is: a stripe of the
rows inserted since the last flush, one or two rows long.

Steps (default settings, one columnar table, loader with a block per batch):

```
BEGIN;
DO $$ BEGIN FOR i IN 1..250 LOOP BEGIN
  INSERT INTO tm SELECT g, repeat('x', 1000) FROM generate_series(1, 1000) g;
EXCEPTION WHEN division_by_zero THEN NULL; END; END LOOP; END $$;
COMMIT;
SELECT count(*), min(row_count), max(row_count), pg_relation_size('tm') FROM kingres.columnar_stripes('tm');
```

Observed (18.7 and 19rc2): 14 519 stripes for 250 000 rows (smallest 8 rows,
largest 130 809), a relation of 113 MB, backend memory 142 MB at the end of
the loop with a 128 MB limit. The same 250 000 rows in one `INSERT`: 2
stripes, under 1 MB. With `write_buffer_memory = 16MB`, 20 blocks of 1 000 rows and one block of
5 000 rows: 3 352 stripes for 25 000 rows; 150 blocks of 3 000 rows: 70 853
stripes for the 433 653 rows flushed so far, 27.9 MB of backend memory against
the 16 MB limit. No row is lost; the table is readable; the
catalog has one row per stripe and every scan walks all of them.

Expected: a limit that "stays soft" for outer buffers must not turn the rest
of the load into one stripe per row; the ratio between the limit and the
memory (1.1 to 1.7 here) is acceptable, the stripes are not.

Direction: at the commit of a subtransaction, when the merged parent buffer
has reached the stripe size or the memory allowance, flush it then, in the
child's callback (the rows belong to the parent, which the child just joined:
its transaction id is a committed child of the parent and the stripe stands
or falls with it); or flush the largest buffer of any subtransaction that
belongs to the committed chain, not only of the running one.

### C4-04 (low) A table created after the snapshot and then rewritten still gives 40001

Place: `kc_storage.c` `kc_check_snapshot_age` (line 740). The first-file rule
(C3-05) looks at the metapage of the file being read: the file is "first"
when it is not flagged `REPLACES_CONTENT` and was created for this relation.
A table that was created after the snapshot and then truncated, vacuumed
full or rewritten in a later transaction has a file that is not first, but
the table did not exist for the snapshot either, so nothing older was
replaced.

Steps (the tables are made in separate transactions; `other` is any table):

```
-- session 1:  BEGIN ISOLATION LEVEL REPEATABLE READ;  SELECT count(*) FROM other;
-- session 2:  CREATE TABLE x (a int) USING columnar;
--             INSERT INTO x VALUES (1), (2);
--             TRUNCATE x;                       -- or VACUUM FULL x, or
--                                               -- ALTER TABLE x ADD COLUMN b float DEFAULT random(), or
--                                               -- CREATE TABLE x AS ... then ALTER TABLE x SET ACCESS METHOD columnar
--             INSERT INTO x VALUES (3);
-- session 1:  SELECT count(*) FROM x;
```

Observed: `ERROR: could not read columnar table "x": it was rewritten or
truncated after this snapshot was taken` (40001); the same for a new
partition that is created, filled and truncated, and for a table created
by `SELECT * INTO` and moved to columnar. Heap answers 0 (and 2 for the
partitioned parent). It is a retryable error, not a wrong answer, and the
case of `t/015` ("TRUNCATE in the creating transaction") passes.

Direction: carry the creation transaction of the relation (not of the file)
through `TRUNCATE` and rewrites in the metapage (for a rewrite the new file
is made for a transient relation: copy the field from the old file), and
decide on that.

### C4-05 (note) Surviving mutants

The mutants of the "tests" section that no test notices, with what each would
do. M10 (reader limit at 40 million rows): a table whose stripe the old
writer made over the limit becomes unreadable again. M18 (the merge drops the
user-rows flag): a buffer of compaction rows that absorbs a child's user rows
is flushed as maintenance, without the conflict-in check at the flush; the
per-row check has run already, so the hole is the reader whose lock came
between (I could not build a failing schedule without the mutation; the real
code ORs the flag). M30 (freeze conflict): see above. M19 (the catalog with 13
columns): my experiment on a catalog made by the previous round's script:
create, insert, compact, `VACUUM FULL`, serializable read, `TRUNCATE`, `SET
ACCESS METHOD` both ways, all work. The merge under memory exhaustion (C4-01).
Direction: one test for each, the merge with an injected failure first.

### C4-06 (note) `columnar_check` reports transient orphan rows after a rewrite

Not from this round; seen while testing. After `VACUUM FULL`, `ALTER TABLE ... TYPE`,
`SET ACCESS METHOD` or `columnar_compact` the function
`kingres.columnar_check()` reports `orphan_catalog_rows: N stripe catalog rows
belong to files no relation uses` until the next `VACUUM` (then "none");
`columnar_reconcile()` returns 0. The rows are the dead catalog rows of the old
file that an older snapshot may still need, and the report is not an error, but
an operator who runs the check after a maintenance window sees "problems" that
resolve themselves.

## What held under attack

* Conflict-out through the top-level transaction: the 21 shapes and 60 random
  schedules per major of the summary, suboverflowed writers (70 committed
  subtransactions, so the process's cache is full), prepared writers,
  readers in a savepoint, `READ ONLY` readers, compaction in the schedule.
* Every read path: select, `COPY (...) TO`, cursor, `CREATE TABLE AS`,
  `INSERT ... SELECT`, PL/pgSQL, forced parallel plan (the worker refuses),
  `EXPLAIN ANALYZE`, join, subquery inside a write all take the relation
  lock and report conflicts (write skew aborts as with heap, writer with 1
  and with 1 500 rows); `ANALYZE` and `EXPLAIN` without execution take
  none (heap: the same); the inspection functions take none, by design;
  `TABLESAMPLE` and `FOR UPDATE` are refused with an error.
* Holds: `WITH HOLD` across commit, a cursor opened in a released savepoint
  and read afterwards (hold kept, then given back at `CLOSE`), `PREPARE
  TRANSACTION` with an open cursor and commit prepared, 3 000 open cursors
  at once, 20 000 caught errors in one transaction (2.3 MB), a failed
  `COPY`, a failed `INSERT ... SELECT` from the same table, a statement
  timeout in the middle of a join, a prepared statement that fails twice.
  No "resource was not closed" in either server log. Both majors.
* The limit: all of C3-03's writer paths; the setting is refused for an
  ordinary role and for a `SET` clause created by a non-superuser; set by a
  superuser in a database it lowers every user's stripes (as designed) and
  never makes anything unreadable (a stripe above the lowered limit is
  reported by `columnar_check` and rewritten by `VACUUM FULL`).
* A stripe header and catalog that agree on a row count above the old limit
  (40 000 001, 10 000 001, 2 147 483 647) with the footer untouched: every
  read, `ANALYZE`, `VACUUM FULL` give "chunk group rows do not add up"; no
  allocation follows the count.
* Compaction with other transactions (C3-04): compaction before, after, in
  the same transaction as a write, and concurrent with a writer that is not
  committed: reader and writer outcomes equal to heap's; the stripes it
  replaces stay visible to older snapshots (a REPEATABLE READ session
  that started before the compaction reads 3, and keeps reading 3).
* Crash and recovery with `wal_consistency_checking = all` on the primary and
  the standby, both majors: a workload with flushes in savepoints, `TRUNCATE`,
  `VACUUM FULL`, a type change, round trips through heap, a prepared
  transaction, immediate stop, recovery, `COMMIT PREPARED`, promotion and
  writes on the new primary: counts equal at every step (7 300 / 1 234 rows),
  no consistency-check message.
* `VACUUM FULL` against a prepared writer: it waits for the lock behind the
  prepared transaction, then rewrites with all rows (107 of 107); an older
  snapshot gets the serialization failure.
* `VACUUM FULL` against a query on a standby: a standby snapshot taken before
  the primary inserted rows and ran `VACUUM FULL` reads the rows after the
  rewrite (200 instead of 100), and so does a heap table in the same
  experiment: with `hot_standby_feedback = off` the primary does not know the
  standby's snapshot, and heap's rewrite has the same exposure. Not a defect
  of the extension.
* The freeze record with its conflict: the standby query is terminated
  (above).
* Both majors gave the same result in every experiment except the numbers
  of C4-01 (14 against 22, hash order).

## Not reproduced

* An error in the middle of `kc_buf_merge_into` of one relation leaving a
  partial state (the suspicion of the last review): not possible within one
  relation, the code is right. The partial state across relations is C4-01.
* A reader that skips a maintenance stripe hiding a real conflict: I built
  seven schedules (compaction before, after, concurrent and in the same
  transaction as the real write, writer committed or not, compaction by the
  reader itself) and none differed from heap. The only gap found is the
  compactor's own view (C4-02). The M18 hole needs an exact interleaving of a
  reader's lock between a user insert and the merge; I could not build it
  against the real code, which ORs the flag.
* A stripe above the writer's limit by any path: none found (see "What held").
* Allocation sizes from a crafted file with a row count in the old over-limit
  range: only the two checks I could craft (header and catalog changed, footer
  not) were refused before any allocation. A fully consistent crafted file
  (footer rewritten with a valid checksum and 20 000 chunk groups) was not
  built.
* The flag after dump and restore: not carried over (restore rewrites the
  rows); not a defect.
* An aborted writer subtransaction hiding a real dependency through the
  suboverflowed cache: the false direction exists (a dependency on an aborted
  subtransaction is reported, as heap does), the missing direction does not.

## Not covered

* The `xid_wraparound` test (`t/013`): the module is not installed in the
  installations I was given and I did not build it, so the author's own
  skip stands for me too. The wraparound claims of the round (the new
  metapage fields across a lap) were not exercised.
* A table dropped and created again with the same relfilenode after
  wraparound of oids; unlogged to logged (refused by the access method);
  a restore from a base backup and a standby with an old snapshot beyond the
  freeze experiment; archive recovery to a point in time; `pg_upgrade`.
* A real stripe of more than 40 million rows (only the old writer could
  make one; I did not build the old version for it), and the memory a read of
  it takes.
* Memory exhaustion elsewhere than the merge (flush, `kc_buf_drop_prefix`
  after the stripes were written: an error there aborts the subtransaction
  that owns both the stripes and the buffer, so no duplicate, by reading
  only).
* Concurrent DDL campaigns, kill-based crash campaigns, multixacts,
  SERIALIZABLE with DDL in the transaction, `READ ONLY DEFERRABLE`.

## Baseline numbers

Existing suite, out of tree, `EXT_WERROR=1`, no compiler warning, before any
mutant or experiment:

| Major | Regression | TAP |
|---|---|---|
| 18.7 (assertions) | 11 of 11 pass | 15 files, 2 947 tests, pass; `t/013` skipped (module not installed); 1 943 s wall under a machine load of 40 to 60 (`t/006` alone 29 min) |
| 19rc2 (assertions) | 11 of 11 pass | 15 files, 2 949 tests, pass; `t/013` skipped; 1 938 s wall |

`t/009` takes 58 s, `t/014` 2 s, `t/015` 1.5 s on both majors.
