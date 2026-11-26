<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
# Adversarial review of the second fix round: contrib/kingres_columnar 0.1 (commit 87611197d3)

Reviewer: an independent reviewer (not the author). Reviewed state: commit
87611197d3, exported with `git archive` (paths `contrib/kingres_columnar`,
`kingres/dev`, `kingres/docs`) into a private directory. The working tree was
not used for any result. Line numbers are those of that commit. Nothing in the
repository was changed except this file. The change under review answers
`kingres/docs/reviews/2026-11-15-kingres-columnar-0.1-fixes.md` (C2-01 to
C2-09); the first review is `kingres/docs/reviews/2026-11-15-kingres-columnar-0.1.md`
(C1-01 to C1-14).

## Summary

Seven of the nine findings of the second review are fixed, and I could not
make any C1 fix fail again. The critical finding C2-01 is **not** fully fixed:
the new conflict-out report names the wrong transaction id whenever the
stripe of a concurrent writer was flushed inside a subtransaction, and then
the write skew of the second review commits on both sides again (reproduced
on both majors, in 12 of 48 checks of one file, in 20 of 200 random
three-transaction schedules, and with the default options). The new
mechanisms of this round also carry three more defects: a scan that is
abandoned by an error leaks a pin that stops every size-based flush of its
table for the rest of the transaction (343 MB against 3.5 MB for the same
load); the merge of subtransaction buffers can build a stripe larger than the
reader accepts, which makes the table unreadable by every statement; and
`columnar_compact` in a SERIALIZABLE transaction takes part in conflict
detection as a writer and aborts unrelated readers. One more fails a
workload that heap serves (a snapshot that predates the creation of a
columnar table gets 40001 where heap answers).

| Severity | Count |
|---|---|
| critical (wrong answer, lost or doubled committed rows, server crash, or a SERIALIZABLE anomaly that heap prevents) | 1 |
| high | 2 |
| medium | 2 |
| low | 1 |
| note | 0 |

| Id | Severity | Component | Statement |
|---|---|---|---|
| C3-01 | critical | `kc_catalog.c` (`kc_catalog_load`, line 374) | The conflict-out of a skipped stripe is reported with the xid in the stripe row, which is a subtransaction xid when the stripe was flushed in a subtransaction; PostgreSQL's SSI wants the top-level xid, so the dependency is silently dropped. Write skew commits on both transactions (heap aborts one). Default options, 300 000 rows in a savepoint are enough. |
| C3-02 | high | `kc_scan.c` (pin, line 149), `kc_write.c` (`kc_buf_flushable`, line 368) | A scan that is abandoned by an error (division by zero in a caught exception block, `ROLLBACK TO` after an error) never gives its pin back: from then on no buffer of the table is flushed by size or by `write_buffer_memory` until the transaction ends. Memory grows with the load (343 MB against 3.5 MB at 2 million rows of 100 bytes; unbounded). |
| C3-03 | high | `kc_write.c` (`kc_buf_merge_into`, line 1094; `kc_flush_buf`), `kc_catalog.c` line 327 | Merging committed subtransaction buffers into the parent has no size limit, and `kc_flush_buf` writes the whole buffer as one stripe. A stripe over 40 million rows is written and committed, and every later read of the table (and `SET ACCESS METHOD`, `VACUUM FULL`, `columnar_compact`, `COPY`) fails with "the stripe catalog row of stripe 1 is corrupt". Needs `write_buffer_memory` of about 0.6 GB or more. |
| C3-04 | medium | `kc_write.c` (lines 439 and 840), `kc_vacuum.c` (`kc_compact`) | `columnar_compact` in a SERIALIZABLE transaction (or with `default_transaction_isolation = serializable`) reports a write conflict-in against every reader of the table and so makes an unrelated reader fail with 40001 where nothing changed logically. |
| C3-05 | medium | `kc_storage.c` (`kc_check_snapshot_age`, line 731) | A REPEATABLE READ or SERIALIZABLE snapshot that predates the *creation* of a columnar table (or partition) gets 40001 "rewritten or truncated" when it reads the table; heap answers (empty or the visible rows). A long reporting transaction on a partitioned table fails when the next partition is created. |
| C3-06 | low | `t/009_serializable.pl` | The test cases that claim to flush "by size" never flush by size (the `SET` comes after the tables exist and options are read when a table is created), so the matrix of 168 checks did not cover the path that C3-01 breaks. |

## Fate of the second review's findings

| Id | Fate | Evidence |
|---|---|---|
| C2-01 | Not closed in all variants (C3-01) | 48 checks of my file `ssi1` per major: the 12 checks where the writer flushes in a subtransaction fail on 18.7 and on 19rc2 (`1=C 2=C`, heap `1=C 2=A`); 200 random three-transaction schedules with a serial-order check of the committed set: 20 non-serializable outcomes on each major, 0 with the one-line change of C3-01 (19rc2) and none for heap. Every other shape held: rows buffered, flushed at commit, flushed by size at top level, savepoints that roll back, `PREPARE TRANSACTION` also across a restart (checked against heap), read-only anomaly (inside the random schedules). |
| C2-02 | Fixed | 100 000 `BEGIN ... EXCEPTION` blocks with an open cursor: 11 MB in the backend's memory contexts. 2 400 random statement sequences with savepoints, `ROLLBACK TO`, cursors opened and read across `RELEASE`, `COPY`, PL/pgSQL loops that read, and a cursor that is read while the same transaction flushes by size: results and errors equal to a heap twin on both majors. See C3-02 and C3-03 for what the merge and the pins still do wrong. |
| C2-03 | Fixed | 350 random DDL sequences of 15 to 45 statements (150 on 18.7, 200 on 19rc2: all forms of CREATE and ALTER PUBLICATION, `FOR ALL TABLES`, schemas, `ONLY`, column lists, row filters, `EXCEPT` on 19, partition roots, `publish_via_partition_root`, attach, detach, `SET SCHEMA`, `SET ACCESS METHOD`, `CREATE TABLE AS`, partitions of two levels), after every statement a check that no publication publishes a columnar table or a partition tree that holds one: 0 violations. |
| C2-04 | Fixed | `SELECT columnar_compact('cc', 1000), (SELECT count(*) FROM cc)` on three stripes returns 1 and 3, then 3 rows and 1 stripe; same on both majors. |
| C2-05 | Fixed as far as I attacked it | The suite's visibility file passes on both majors; I read `kc_catalog_all_visible` against the cases of the task (snapshot between check and swap, prepared transaction, a transaction without a snapshot yet, stripes deleted by compaction) and found each excluded by the lock order or by the oldest-xmin test. No experiment of my own beyond the suite (see "Not covered"). |
| C2-06 | Fixed | A table created (plain, by `TRUNCATE`, by `SET ACCESS METHOD`, by `VACUUM FULL`) at 0, 1, 2 and 3 billion transactions into a run of 6 billion, autovacuum on, `autovacuum_freeze_max_age = 150000000`: `age(relfrozenxid)` stays below 200 million, the metapage id is frozen, REPEATABLE READ reads work (18.7). The suite's two-lap test passes on both majors with the module built by me. Standby: a query whose snapshot predates the id is cancelled by the replayed freeze record ("terminating connection due to conflict with recovery"), `wal_consistency_checking = all` on both nodes silent, promotion works (18.7). |
| C2-07 | Fixed | Four ways to get a text column (create, `ADD COLUMN`, `ALTER COLUMN TYPE`, `SET STORAGE`) and jsonb values of 64 000 and 100 000 bytes: `SET ACCESS METHOD heap` and back work, the TOAST table exists and is empty, `pg_dump` and restore reproduce data and TOAST tables, `VACUUM`, `VACUUM FULL`, `REINDEX` run (both majors). |
| C2-08 | Open as documented | Not retried; the suite's corruption file passes. |
| C2-09 | Fixed in part, not independently tested | The suite passes; I did not test the identity redraw or the footer bound (the author's own note says no test exists). |

## Scope and method

Read: the whole diff of the commit, `kc_catalog.c` (visibility, load, insert),
`kc_write.c`, `kc_scan.c`, `kc_storage.c` (freeze, snapshot age, redo),
`kc_vacuum.c`, `kc_am.c`, the SQL script (event trigger), the README, and,
from the PostgreSQL branches `kingres-18` and `kingres-19`, `xact.c` and
`predicate.c`. Order of the pre-commit callback checked on both majors: in
`CommitTransaction` the callbacks (`XACT_EVENT_PRE_COMMIT`) run before
`PreCommit_CheckForSerializationFailure` on 18 and on 19; in
`PrepareTransaction` `XACT_EVENT_PRE_PREPARE` runs before it on both. The
claim of the commit message holds on both majors.

Built and run on PostgreSQL 18.7 and 19rc2, both with assertions, each copied
into a private area and used out of tree with `kingres/dev/ext.sh`
(`EXT_WERROR=1`, no warning). The `xid_wraparound` test module was built for
both from the matching branch. Everything ran under `nice`, at most `-j4`,
on ports of my own; every server I started is stopped. Load average 2 to 10
from other jobs.

| Experiment | What was run |
|---|---|
| Serializable, fixed scenarios (`ssi1`) | The write skew of the second review, writer in the savepoint or not, 1 000, 2 000 and 2 500 rows with the stripe size of the tables set to 1 000 rows (so that flushing by size really happens), one and two savepoint levels, savepoint kept; compared with the heap twin for column pairs (col, col), (col, heap), (heap, col). Both majors. Then the same with default options and 300 000 rows. |
| Serializable, random (`ssi2`) | 200 schedules of three transactions with two to four statements each (counts, small inserts, 2 000-row inserts, the same inside savepoints, rolled back savepoints, commit or rollback), random interleaving, three table-kind variants each. For every run: the set of transactions that committed, and a check that some serial order of that set explains every count each of them read (own writes included). Heap passed the check in every schedule. 18.7 and 19rc2 unpatched: 20 non-serializable outcomes each; with the change of C3-01: 0 on 19rc2 (and 0 on 18.7 in an earlier partial run that had it installed). |
| Two-phase | Write skew with `PREPARE TRANSACTION` of one side, committed afterwards, plain and with a server restart in between: columnar equals heap (T2 aborted) on 18.7. |
| Subtransactions, cursors | 2 400 generated statement sequences per major (seeds 100 to 2 500), each run on a columnar table (stripe size 1 000) and a heap twin and every output line and error compared: savepoints to any depth, `ROLLBACK TO`, `RELEASE`, `DECLARE` and `FETCH` of ordered cursors (also across savepoint rollback), PL/pgSQL loops with exception blocks that read in between, a PL/pgSQL cursor read before and after inserts and a rolled back block, `COPY` of up to 1 100 rows, `INSERT ... SELECT` from the table itself, checks with the custom scan on and off, after commit or rollback and in a REPEATABLE READ transaction. 0 differences on both majors. |
| Freezing | `relfrozenxid`, metapage id and `xmin` of the relation after create, `TRUNCATE`, `SET ACCESS METHOD`, `VACUUM`, `VACUUM FULL`, `CREATE TABLE AS`, a savepoint around `TRUNCATE`; the module run described above; a streaming standby with a long REPEATABLE READ query, `VACUUM` on the primary, promotion. |
| Publications | See C2-03 above. |
| Memory | 100 000 exception blocks with and without a cursor; caught errors and the buffer (C3-02); merged buffers (C3-03). |
| Baseline | Existing suite, see "Baseline numbers". |

## Findings

### C3-01 (critical) Conflict-out is reported with a subtransaction id; write skew commits

Place: `contrib/kingres_columnar/src/kc_catalog.c`, `kc_catalog_load`, line
374: `CheckForSerializableConflictOut(rel, concurrent, snap)`, where
`concurrent` is the raw `xmin` of the stripe row (`kc_tuple_visible`, line
~262). The function documents (predicate.c, comment of
`CheckForSerializableConflictOut`) that the caller passes "the top level xid"
and looks it up in `SerializableXidHash`, which holds top-level ids only
(`RegisterPredicateLockingXid` is called for top-level ids only,
`xact.c`). A stripe that is flushed by size or by memory inside a
subtransaction (`kc_write_row_values`, line 482; `kc_flush_largest`) is
inserted into the catalog by the subtransaction, so its `xmin` is the
subtransaction id, the hash lookup fails, and the function returns as if the
writer were not serializable. Heap converts with
`SubTransGetTopmostTransaction` before the call (`HeapCheckForSerializableConflictOut`).

The conflict-in check at flush time does not rescue the case when nothing is
left to flush at commit: at pre-commit the buffers are empty, so no second
check runs. With a remainder (for example 2 500 rows with a stripe size of
1 000) the commit-time flush finds the reader's lock and the bug stays hidden;
this is why the author's matrix passed.

Steps (stripe size of `ta` set to 1 000 rows; the same happens with the default
150 000 and 300 000 rows):

```
CREATE EXTENSION kingres_columnar;
CREATE TABLE ta (v int) USING columnar;  CREATE TABLE tb (v int) USING columnar;
SELECT kingres.columnar_set_options('ta', stripe_row_count => 1000, chunk_row_count => 100);
INSERT INTO ta VALUES (0);  INSERT INTO tb VALUES (0);
-- session 1:  BEGIN ISOLATION LEVEL SERIALIZABLE;  SELECT 1;
-- session 2:  BEGIN ISOLATION LEVEL SERIALIZABLE;  SELECT 1;
-- session 1:  SAVEPOINT x;  INSERT INTO ta SELECT g FROM generate_series(1,2000) g;  RELEASE x;
-- session 2:  SELECT count(*) FROM ta;      -- 1
-- session 2:  INSERT INTO tb VALUES (1);
-- session 1:  SELECT count(*) FROM tb;      -- 1
-- session 1:  COMMIT;
-- session 2:  COMMIT;
```

Observed (18.7 and 19rc2): both commit. Each read a state without the other's
write: no serial order exists. Expected (heap twin, same statements): session 2
fails with SQLSTATE 40001. Same with `ta` columnar and `tb` heap; same with
`SAVEPOINT x; SAVEPOINT y;` (two levels) and with the savepoint left open at
commit. A PL/pgSQL `BEGIN ... EXCEPTION` block is a savepoint, so a loader
function with an exception handler triggers it. In the random schedules the
violation shows with the writer's statements `SAVEPOINT; INSERT ... 2000 rows;
RELEASE` and any concurrent reader-writer pair (seeds 78, 96, 98, 123, 152,
167, 172, 213, 228, 259 of my run, both majors).

Suggested direction: in `kc_catalog_load` pass
`SubTransGetTopmostTransaction(concurrent)` and skip it when it equals
`GetTopTransactionIdIfAny()`, as heap does. I made exactly that change in my
private copy of both majors: the 12 failing checks pass and the 200 random
schedules give 0 non-serializable outcomes (19rc2). Add a test that sets the
stripe size before the tables are created (C3-06).

### C3-02 (high) A scan abandoned by an error keeps its pin: no flush by size, memory without bound

Place: `kc_scan.c` line 149 (`kc_write_pin_relation(..., 1)` when the
transaction has pending rows), released only in `kc_scan_end` (line 175);
`kc_write.c` `kc_buf_flushable` (line 368: `st->pins == 0 && ...`) is the
test for every flush by size (line 482), by `write_buffer_memory`
(`kc_flush_largest`) and for `kc_write_flush_relation`. When the executor is
abandoned by an error that a subtransaction catches (an exception block, a
savepoint and `ROLLBACK TO`), `kc_scan_end` is never called; nothing releases
the pin or the buffer references (`b->refs`, line 937) when the subtransaction
aborts, and the state is only reset at the end of the whole transaction.

Steps:

```
CREATE TABLE pn (a int) USING columnar;
SELECT kingres.columnar_set_options('pn', stripe_row_count => 1000, chunk_row_count => 100);
BEGIN;
INSERT INTO pn SELECT generate_series(1,10);
SAVEPOINT s;  SELECT 1/(a-a) FROM pn;  ROLLBACK TO s;       -- ERROR: division by zero
INSERT INTO pn SELECT generate_series(1,5000);
SELECT count(*) FROM kingres.columnar_stripes('pn');          -- 0 (control without the failed scan: 5)
COMMIT;
```

Same inside `DO $$ BEGIN BEGIN PERFORM 1/(a-a) FROM pn; EXCEPTION WHEN
division_by_zero THEN NULL; END; END $$`. Memory, with
`write_buffer_memory = '16MB'`, one 10-row insert, the caught failed scan, then
`INSERT INTO pn SELECT g, repeat('y', 100) FROM generate_series(1,2000000) g`:
backend contexts 343 MB and 0 stripes, against 3.5 MB and 15 stripes without
the failed scan (18.7; 19rc2 shows the same 0 stripes). A loop of that shape
that loads tens of millions of rows after one caught error ends in the
operating system's out-of-memory kill, which restarts the whole server. The
stale `refs` also stop the merge of later subtransaction buffers (C2-02
regresses for the buffers the dead scan held).

Expected: the limit of `write_buffer_memory` holds after a caught error, as
without it (the README promises a soft limit only for outer subtransactions).

Suggested direction: register a memory-context callback on `scan_cxt` that
undoes the pin and the references, or record pins and references per
subtransaction and clear them in `SUBXACT_EVENT_ABORT_SUB`.

### C3-03 (high) Merged buffers can become a stripe the reader refuses; the table becomes unreadable

Place: `kc_write.c` `kc_buf_merge_into` (line 1094) appends the rows of a
committed subtransaction to its parent's buffer with no look at the size;
the test for a flush (line 482) runs only when a row is inserted into the
*current* buffer, which is the child's; `kc_flush_buf` writes the whole buffer
as one stripe whatever its row count. `kc_catalog.c` line 327 refuses a stripe
row with more than `KC_MAX_STRIPE_ROWS * 4` = 40 million rows as corrupt.
A parent that only collects children's remainders (a loop of exception
blocks, each inserting fewer rows than a stripe) grows without a flush; with
the default `write_buffer_memory` of 128 MB the memory limit flushes the child's
buffer instead and the parent stays near 128 MB (about 10 million rows), but a
setting of about 0.6 GB or more (a plausible setting for a loading server) lets
it pass 40 million rows.

Steps (18.7; the 19rc2 code is the same):

```
CREATE TABLE big2 (a int) USING columnar;
SET kingres_columnar.write_buffer_memory = '2000MB';
BEGIN;
INSERT INTO big2 VALUES (0);
DO $$ BEGIN FOR i IN 1..450 LOOP BEGIN
  INSERT INTO big2 SELECT g FROM generate_series(1,100000) g;
EXCEPTION WHEN others THEN RAISE; END; END LOOP; END $$;
COMMIT;                                              -- succeeds, 8 seconds
SELECT count(*) FROM big2;      -- ERROR: the stripe catalog row of stripe 1 is corrupt
```

Observed: one stripe of 45 000 001 rows, committed. `SELECT`, `COPY`,
`ALTER TABLE ... SET ACCESS METHOD heap`, `VACUUM FULL`,
`kingres.columnar_compact`, `kingres.columnar_info` all fail with the same
error; `columnar_check` reports nothing wrong. With `stripe_row_count` at its
maximum and five blocks of 9 999 999 rows the same happens. No supported
statement gets the committed rows out again.

Expected: the writer never produces what the reader refuses: either a flush
(split into stripes of at most `stripe_rows`) when a merge makes the parent
exceed the stripe size, or `kc_flush_buf` cutting a large buffer into several
stripes, or an error at write time.

### C3-04 (medium) `columnar_compact` takes part in SSI as a writer

Place: `kc_write.c` lines 439 (`kc_write_row_values`, per row) and 840
(`kc_flush_buf`): `CheckForSerializableConflictIn` runs for the rows that
`kc_compact` (`kc_vacuum.c`) rewrites, which are rows that readers already
see. `kc_scan_begin` exempts the maintenance *read* from SSI
(`KC_SCAN_MAINTENANCE`); the maintenance *write* is not exempt.

Steps (table `a` columnar with three one-row stripes, `b` any table):

```
s1: BEGIN ISOLATION LEVEL SERIALIZABLE; SELECT count(*) FROM a;
s2: BEGIN ISOLATION LEVEL SERIALIZABLE; SELECT count(*) FROM b;
s1: INSERT INTO b VALUES (1);
s3: BEGIN ISOLATION LEVEL SERIALIZABLE; SELECT kingres.columnar_compact('a'::regclass, 100); COMMIT;
s1: COMMIT;      -- ERROR: could not serialize access due to read/write dependencies
s2: COMMIT;
```

Observed: s1 (an application transaction that reads `a`) fails with 40001
(18.7; 19rc2 uses the same code). With s3 in READ COMMITTED both commit, and
without the compaction there is nothing to conflict. Expected: compaction
changes nothing logically and must not create dependencies. It matters where
`default_transaction_isolation = serializable` is set globally and compaction
runs from a scheduler.

Suggested direction: suppress both checks while the writer is the maintenance
path (a flag as for `KC_SCAN_MAINTENANCE`).

### C3-05 (medium) A snapshot older than the creation of a columnar table fails with "rewritten or truncated"

Place: `kc_storage.c` `kc_check_snapshot_age` (line 731): the creating
transaction id in the metapage is tested with `XidInMVCCSnapshot` whether the
file replaced content (rewrite, `TRUNCATE`) or is the first file of a new
table.

Steps (both majors):

```
CREATE TABLE pt (d date, v int) PARTITION BY RANGE (d);
CREATE TABLE pt1 PARTITION OF pt FOR VALUES FROM ('2026-01-01') TO ('2026-02-01') USING columnar;
INSERT INTO pt VALUES ('2026-01-05', 1);
-- session 1: BEGIN ISOLATION LEVEL REPEATABLE READ; SELECT 1;
-- session 2: CREATE TABLE pt2 PARTITION OF pt FOR VALUES FROM ('2026-02-01') TO ('2026-03-01') USING columnar;
--            INSERT INTO pt VALUES ('2026-02-05', 2);
-- session 1: SELECT count(*) FROM pt;
```

Observed: `ERROR: could not read columnar table "pt2": it was rewritten or
truncated after this snapshot was taken` (40001, a message that names the
wrong cause). Expected (heap twin): `count = 1`. Any new table read by an older
snapshot behaves the same. This is an error, not a wrong answer, but a
monthly partition job makes long REPEATABLE READ or SERIALIZABLE reports on
partitioned tables fail. Suggested direction: remember in the metapage that
the file is the first of its relation (or that no content existed before),
and skip the check then; the old snapshot sees no stripes anyway.

### C3-06 (low) The "flushed by size" cases of `t/009_serializable.pl` do not flush by size

`t/009_serializable.pl` creates the tables in `run_scenario` through
`safe_psql` (another session) and sets `kingres_columnar.stripe_row_count` in
the two transaction sessions afterwards. The setting is read when a table is
created and kept in the metapage, so the tables have 150 000-row stripes and
2 500 rows are never flushed by size; the matrix covers buffered rows and rows
flushed at commit only. With table options set (or the GUC set before
creation) the same file would have found C3-01. The commit message's list
("rows buffered, flushed by size and flushed at commit") is therefore not
shown by the test. Direction: set `columnar_set_options` per table in the
scenario setup and assert the number of stripes mid-transaction.

## What held under attack

* The order of the pre-commit callback against PostgreSQL's own serialization
  check, both majors (read in `xact.c`), and the conflict-in checks at insert and at flush.
* Every serializable schedule of my random set in which the writer does not
  flush in a subtransaction: plain flush at commit, flush by size at top
  level, rolled back savepoints, committed savepoints, three transactions, the
  read-only anomaly shape, heap and columnar mixed. Unrelated tables never
  aborted each other. Prepared transactions with a restart in between.
* Write buffers of subtransactions: 2 400 random sequences per major equal to
  heap (see the method table), memory bounded (11 MB for 100 000 blocks with a
  cursor), cursors opened in a subtransaction and read in another, a scan that
  holds a buffer while the parent inserts enough to flush by size.
* The hand-written visibility check of the stripe catalog against frozen and
  hint-bit cases (read against `HeapTupleSatisfiesMVCC`; frozen rows, aborted
  inserters, deleters in progress, lock-only `xmax` all agree).
* `relfrozenxid` never newer than the id the metapage still holds (every
  create path, rewrites included); real wraparound of 6 billion transactions
  with five creation paths; recovery conflict on a standby; replay with
  `wal_consistency_checking = all`; promotion and writes after it.
* Publications and TOAST as listed in the fate table; dump and restore of a
  table with a TOAST table and of a partitioned table with a columnar
  partition (both majors).
* The first review's fixes: every C1 regression and TAP test still passes.

## Not reproduced

* Partial state when memory runs out in the middle of `kc_buf_merge_into`:
  rows are copied and `prev->nrows` is raised one by one, so an error in the
  allocation of a later by-reference value (inside the commit of a
  subtransaction, caught by an exception block) would leave the first rows of
  the aborted child in the parent's buffer, where they would be committed.
  I tried to hit it with `ulimit -v` on a backend (300 MB value after a few
  small rows, limits from 0.6 to 1.9 GB virtual): the failures came earlier
  (in the stripe build) or not at all. Found by reading only; the direction is
  to reserve and copy first and raise the counts at the end.
* A standby snapshot older than a `VACUUM FULL` that froze its stripes sees rows
  committed after it (no conflict record exists for that rewrite, and the
  primary does not know the standby's snapshot without feedback). Heap's
  `VACUUM FULL` has the same exposure; not tried.
* `kingres.columnar_info`, `columnar_stripes` and the other inspection
  functions read the stripe catalog with the active snapshot and report
  conflict-out under SERIALIZABLE without taking the relation lock; a
  decision based on `columnar_info(...).rows` has no write-skew protection.
  Not run.
* A serializable transaction that reads `kingres.columnar_stripe` directly
  takes page-level predicate locks on the catalog index and heap pages and
  would then conflict with the stripe inserts of every columnar table. Not run.

## Not covered

* `pg_upgrade`, `pg_rewind`, base backup under load, archive recovery to a point
  in time (only the existing suite), kill-based crash campaigns, concurrent DDL
  campaigns.
* Independent experiments on the `VACUUM FULL` boundary (C2-05) beyond reading
  and the suite; the freeze record on a standby with `hot_standby_feedback = on`;
  multixacts (the table holds none; `relminmxid` was only read).
* SERIALIZABLE with DDL in the transaction, parallel query, `READ ONLY
  DEFERRABLE` readers, `UPDATE`/`DELETE`/`MERGE`/`FOR UPDATE` (refused by the
  access method, not exercised against heap twins).
* Hostile files (the new footer bound, the identity redraw), the jsonb
  validation (C2-08), performance.
* 19rc2 for the `kc_catalog_all_visible` and the standby experiments (18.7 only
  where the text says so).

## Baseline numbers

Existing suite out of tree, `EXT_WERROR=1`, no warning, before any change of
mine:

| Major | Regression | TAP |
|---|---|---|
| 18.7 (assertions) | 11 of 11 pass | 13 files, 1 240 tests, pass (407 s) |
| 19rc2 (assertions) | 11 of 11 pass | 13 files, 1 242 tests, pass (385 s) |

The wraparound test was skipped in those runs (module not installed). After I
built the module for both majors I ran the five newest files (009 to 013, the
wraparound test included, 66 s) on a clean rebuild: 18.7 264 tests pass, 19rc2
266 tests pass. (The first 18.7 run overlapped with an installation of my
patched library for its last files; the second run replaces it.)
