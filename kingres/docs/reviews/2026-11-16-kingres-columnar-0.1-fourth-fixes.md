<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
# Adversarial review of the fourth fix round: contrib/kingres_columnar 0.1

Reviewer: an independent reviewer (not the author). The change under review
answers `kingres/docs/reviews/2026-11-16-kingres-columnar-0.1-third-fixes.md`
(C4-01 to C4-06). The earlier reviews are
`kingres/docs/reviews/2026-11-15-kingres-columnar-0.1.md` (C1-01 to C1-14),
`kingres/docs/reviews/2026-11-15-kingres-columnar-0.1-fixes.md` (C2-01 to
C2-09) and `kingres/docs/reviews/2026-11-16-kingres-columnar-0.1-second-fixes.md`
(C3-01 to C3-06). The reviewed state was exported with `git archive` (paths
`contrib/kingres_columnar`, `kingres/dev`, `kingres/docs`) into a private work
area; the working tree was not used for any result and nothing in the
repository was changed except this file. Line numbers, where given, are those
of that export.

## Summary

None of the six earlier findings failed again in the form it was reported, and
nothing from the three earlier reviews came back. The central fix of the round,
the two-phase hand-over of a subtransaction's rows (C4-01), holds against
everything I could build on both majors: about 2 000 fault-injected hand-overs per major in
random savepoint structures (model-checked after every one, including reads in
the middle of the transaction), DDL inside the failing subtransaction
(TRUNCATE, ADD COLUMN, a type change, DROP of another table), a failing
callback of another extension in `PRE_COMMIT_SUB`, serializable write skew with
the new flush at the commit of a child, a standby with
`wal_consistency_checking = all`, crash recovery and a prepared transaction.
The compaction fix (C4-02) holds against 300 random interleavings of writers,
open writers, repeatable-read readers and compactors at all three isolation
levels per major. The tests of the round are real tests: for every fix I wrote
a mutant and the test of the round failed (with two weak spots, see
"The tests of this round").

The pattern of the earlier reviews repeats in a milder form. The fixes are
right for the cases that were reported and incomplete at their edges:

* The soft memory limit is honoured by the flush of a child's commit only by
  row count, so a loop of exception blocks that writes wide rows keeps
  7.5 times the allowance in memory (954 MB against 128 MB at 100 kB per row,
  and it grows with the row width): C5-01.
* The rule for the table's creation, `table_xid`, is taken for a table that
  came from heap from the `xmin` of its `pg_class` row, which any
  `GRANT`, `ALTER TABLE ... OWNER`, `SET (...)` or `ADD COLUMN` moves. The
  same conversion that gives an older snapshot the serialization failure gives
  it a silent empty table after such a command (C5-02).
* Phase one of the hand-over leaves a stripe that holds rows that are still in
  the parent's buffer; code that runs between the two phases (another
  extension's callback) reads them twice (C5-03).
* The new cleanup at the end of every `ALTER TABLE` costs time proportional to
  the number of columnar tables, for every `ALTER TABLE` of any table
  (C5-04), and two sessions that run it at the same time over leftover rows
  block each other and one fails (C5-05).

The stated deviation (a rewriting `ALTER TABLE` in REPEATABLE READ shows the
rewriter the rows committed after its snapshot) is equal to heap in all seven
variants I built, on both majors. A build without assertions has none of the
fault-injection code or settings and passes the same regression output.

| Severity | Count |
|---|---|
| critical (wrong answer, lost or doubled committed rows, crash, a SERIALIZABLE anomaly heap prevents, an unreadable table) | 0 |
| high | 1 |
| medium | 1 |
| low | 4 |
| note | 2 |

| Id | Severity | Component | Statement |
|---|---|---|---|
| C5-01 | medium | `kc_write.c` `kc_merge_flush_prepare`, `kc_min_flush_rows` | The flush at the commit of a subtransaction asks only for a row count (`max(chunk_row_count, 1000)`), never for bytes: a loop of exception blocks that writes wide rows holds up to `chunk_row_count` rows in memory whatever `write_buffer_memory` says (954 MB against 128 MB at 100 kB a row; 10 000 rows of 1 MB would be 10 GB). |
| C5-02 | high | `kc_am.c` `kc_table_creation_xid`, `kc_storage.c` `kc_check_snapshot_age` | A table converted from heap takes `table_xid` from the `xmin` of its `pg_class` row. After `GRANT`, `ALTER TABLE ... OWNER TO`, `SET (fillfactor ...)`, `ADD COLUMN` or `ENABLE ROW LEVEL SECURITY` an older snapshot that saw the rows reads an empty table; after nothing, or after `ANALYZE`, `VACUUM`, `COMMENT`, `SET STATISTICS`, it gets the 40001 the rule requires. |
| C5-03 | low | `kc_write.c` `kc_subxact_prepare`, `kc_merge_flush_prepare`, `kc_subxact_publish` | Between phase one and phase two a read of the table (a `SUBXACT_EVENT_PRE_COMMIT_SUB` callback of another extension that runs after this one) sees the rows of the stripe that phase one wrote and the rows still in the buffers: 2002 rows for 1002. A write between the phases takes the "else" branch of phase two and leaves rows in a stripe and in a buffer. |
| C5-04 | low | `kingres_columnar--0.1.sql` `columnar_check_ddl` | The cleanup after `ALTER TABLE` scans `pg_class` and reads block 0 of every columnar table for every `ALTER TABLE` of any table: 0.14 ms at 10 columnar tables, 34.9 ms at 5 000; a `pg_restore` of 3 000 columnar and 1 500 heap tables takes 22.5 s against 3.9 s. |
| C5-05 | low | `kc_catalog.c` `kc_reconcile_ex`, same trigger | When leftover stripe rows exist, two sessions that run `ALTER TABLE` (any tables) at the same time contend for the same rows: the second waits for the first's commit and then fails with `ERROR: tuple concurrently deleted` (XX000). |
| C5-06 | low | `kc_catalog.c` `kc_catalog_check_shape` | A database written by the earlier build cannot read a single table, and the hint ("drop and create the extension again") cannot be followed without `CASCADE`, which drops the tables. One `ALTER TABLE` repairs the catalog and was verified, but nothing says so. |
| C5-07 | note | tests | Weak and missing checks found while mutating: the freezing of `table_xid` is tested only by the file that needs an optional module, `t/016` is skipped in the packaging build, one mutant of C4-04 is caught by one assertion only, the new section of `t/002` ends on a `die` and not on an assertion, the memory case of `t/017` uses narrow rows. |
| C5-08 | note | `kingres_columnar.c` `kc_check_one`, `kc_catalog.c` | A stale comment and message: `columnar_check` still says a stripe over the row limit "can be read" and that "the reader accepts it", which is false since the reader's limit was lowered to the writer's. |

## Fate of C4-01 to C4-06

| Id | Fate | Evidence |
|---|---|---|
| C4-01 | Fixed (residual C5-03 for code of other extensions) | Mutant that hands over each relation at `COMMIT_SUB` one after the other (the old behaviour): `t/016` fails 4 checks ("merge #2" to "#5": rows of the earlier relations stay) and then the server aborts on an assertion. My own sweep: 30 random savepoint structures per major (up to 5 levels, `ROLLBACK TO`, releases, inserts of 1 to 1 200 rows into 3 tables with 1 000-row stripes), then a failing exception block with three tables and a nested block, `fault_point` merge, flush and alloc, `fault_nth` 1 to 60 until no failure: 0 differences from the model on 18.7 and 19rc2, mid-transaction reads included; the same harness against the old-behaviour mutant fails. DDL in the failing child (TRUNCATE, ADD COLUMN, type change, DROP of another table): all sweeps equal to the state before the child. A failing `PRE_COMMIT_SUB` callback of another extension after this one prepared: parent rows intact (999 and 1), child rows gone. |
| C4-02 | Fixed | Mutant "REPEATABLE READ and SERIALIZABLE compact with `NULL` (latest) snapshot": `t/018` fails 8 checks, my model (below) fails 23 of 60 episodes. Mutant "the delete count is not checked": `t/018` fails 2 (the replaced-source cases); my random model passes it, as expected, because it never runs two compactors. 300 episodes of 60 operations per major (autocommit and open writers, repeatable-read readers, a compactor at READ COMMITTED, REPEATABLE READ or SERIALIZABLE, plain `VACUUM`, reads of the compactor, of old readers and of fresh sessions against a snapshot model): 0 differences on 18.7 and 19rc2. Two compactors: the second gets 40001 at REPEATABLE READ and SERIALIZABLE, whether it started before or waited behind the first. |
| C4-03 | Fixed for narrow rows, not for wide ones (C5-01) | The original reproduction (250 blocks of 1 000 rows of 1 kB): 2 stripes of 119 000 and 131 000 rows, 118 MB of write buffers at the end of the loop, 72 kB file (was 14 519 stripes, 113 MB). Mutants: the commit of a child never flushes the parent's buffer (`t/017` fails 4, `t/014` fails 1); the minimum flush rule removed (`t/017` fails 8). |
| C4-04 | Fixed for tables created in Postgres; incomplete for tables converted from heap (C5-02) | Mutants: the rule removed from the reader (`t/019` fails 9), TRUNCATE not carrying `table_xid` (3), a rewrite taking its own creation (1). 11 further shapes against heap (attach, detach, truncate, attach; SET TABLESPACE of a new and an old table; drop and create again; create in an aborted transaction; create in a subtransaction; VACUUM FULL of an old table): equal to heap or 40001, never an empty table for content that existed, on both majors. |
| C4-05 | Tests added; see "The tests of this round" | Mutants for the freeze conflict (the file dies), the reader's limit (6 checks fail), the event trigger (6), the commit-time cleanup (1). |
| C4-06 | Fixed | `columnar_check` is empty after VACUUM FULL, a rewriting ALTER TABLE, both changes of access method, TRUNCATE, DROP and a compaction. A rolled-back rewrite brings the rows back. Cost and a concurrency problem of the way it is done: C5-04, C5-05. |

## Scope and method

Read: the whole diff of the round, `kc_write.c` (all of the hand-over, the
flush, the buffers and the callbacks), `kc_vacuum.c`, `kc_catalog.c`,
`kc_storage.c`, `kc_am.c`, `kc_scan.c`, the SQL script, the README and the
seven test files of the round. Built and run on PostgreSQL 18.7 and 19rc2,
both with assertions, each copied into a private area with the prefix fixed and
used out of tree with `kingres/dev/ext.sh` (`EXT_WERROR=1`, no warning); a
PostgreSQL 18 without assertions (configured with ICU, LZ4 and zstd like the
other) for the production-build checks. All work ran under `nice`, with at most
4 jobs, on ports 59600 to 59699 (the TAP harness of my copy of each
installation was pointed at that range); every server I started is stopped.
Experiments were TAP scripts with `PostgreSQL::Test::Cluster` and
`BackgroundPsql`, run with the same environment as `ext.sh`. Mutants are copies
of the extension with one change each, built against a hard-linked copy of the
installation so that the installation in use is never touched, run with the one
test file that belongs to the fix.

| Experiment | What was run |
|---|---|
| Hand-over sweeps (`e3`) | Random savepoint structures with a model of what each transaction sees; a failing exception block at the end; the three fault points swept from the first hit; 30 seeds per major. |
| DDL in the child (`e14`) | TRUNCATE, ADD COLUMN, type change and DROP inside the failing subtransaction, three fault points swept. |
| Other extension's callbacks (`e4`) | A small module with a `SubXactCallback` that runs a query or fails at `PRE_COMMIT_SUB` or `COMMIT_SUB`, loaded before this extension so that its callback runs after it. |
| Compaction model (`e5`) | 300 episodes of 60 operations per major against a snapshot model; two compactors (`e17`). |
| Memory (`e1`, `x2`, `x3`, `x4`) | Exception loops with narrow and wide rows against `write_buffer_memory`; 100 000 iterations; 2 000 relations touched and 20 000 empty blocks. |
| Old snapshots (`e9`, `e9b`) | 11 shapes against heap; 11 catalog changes before a heap to columnar conversion. |
| Rewrite in REPEATABLE READ (`e12`) | Heap and columnar twins, seven variants. |
| Cost and concurrency of the cleanup (`e7`, `e8`, `e11`, `e15`) | 10, 1 000, 5 000 columnar tables; a concurrent rewrite, creation and TRUNCATE; leftovers and two sessions; `pg_dump` and `pg_restore` with and without event triggers. |
| Serializable (`e13`) | Write skew with the writer's stripes flushed by the commit of a child, four orders, heap twin. |
| WAL, standby, crash (`e10`, `e16`) | `wal_consistency_checking = all` on both nodes, a standby, `PREPARE TRANSACTION`, immediate stop, recovery, promotion; a standby query in the middle of a transaction that wrote 300 subtransactions. |
| Production build | A server without assertions: the extension built and the whole suite run. |
| Old catalog | A database written by the previous build with the catalog of the build before it, then read by this one (`old`). |

## The tests of this round

Method for each fix: a copy of the extension with the fix broken in one place,
built without `-Werror` (a mutant may leave a function unused), and the one test
file of the round that claims the fix. A mutant "fails" when the file reports
failed checks or dies. Numbers are failed checks.

### `t/016_atomic_handover.pl` (C4-01)

Claims: the rows of a subtransaction become the parent's for every relation or
for none, under an injected failure of the nth allocation, nth hand-over or nth
flush; the commit and the PREPARE of a transaction whose nth relation fails to
flush leave nothing; a cursor does not break an abort.

Preconditions: established. Every case counts the rows in all five relations
and the distinct values, the failure is injected in "at least five places"
(asserted per point), the sweep runs until the statement no longer fails, and
the file asks the server whether the settings exist and skips itself if not.
One weakness: for the point `alloc` the failing allocation is often one of the
`INSERT`s inside the block and not the hand-over, so the check "failed in at
least five places" does not establish that the hand-over was among them; the
point `merge` does, and with that point the mutant is caught.

| Mutant | Result |
|---|---|
| C4-01: each relation handed over at `COMMIT_SUB`, one at a time | 4 failed ("merge #2" to "#5"), then the server aborts in part 2 (assertion), exit 29 |

What it does not cover: a hand-over three levels deep or over a grandparent's
buffer, a failing callback of another extension, the production build (the file
is skipped: the packaging build has no test of C4-01 at all). My harness `e3`
covers the first two and was killed by the same mutant (2 of 14 checks failed
on the first six seeds).

### `t/017_stripe_sizes.pl` (C4-03)

Claims: the number and size of stripes do not depend on the grouping into
subtransactions; the write buffers stay within four times `write_buffer_memory`.

Preconditions: established for the stripe shapes (the one-statement reference
is run first and its shape asserted; blocks of 1, 7, 100, 999, 1 000 and 5 500
rows, one and three levels deep; a cursor; a heap twin for visibility). Not
established for the memory claim: the rows of the memory case are 200 bytes
wide, 500 per block, `chunk_row_count` is 500, so one allowance of 1 MB holds
about 5 000 rows and the row-count rule of `kc_min_flush_rows` is reached long
before the bytes matter. A case with 100 kB rows (C5-01) passes none of these
bounds and the file does not contain one.

| Mutant | Result |
|---|---|
| the commit of a child never flushes the parent's buffer | 4 failed (the memory bound of the four cases: 12.5 MB peak against 4 MB); the stripe shapes still pass, because the later flush of the parent cuts the same stripes |
| `kc_min_flush_rows` returns 1 (the minimum rule removed: the defect of the third review) | 8 failed (2 224 stripes against 11, smallest 20 rows) |

### `t/018_maintenance_snapshots.pl` (C4-02)

Claims: no operation changes which rows a transaction's snapshot sees, for the
compacting transaction or any other, at REPEATABLE READ and SERIALIZABLE; a
stripe replaced since the snapshot is a serialization failure.

Preconditions: established. Every scenario runs against a heap twin (the
compaction a no-op) and asserts the numbers the review reproduced (before=3,
after=3/6, latest=4), and the interleaving is explicit (snapshot, concurrent
commit, compaction, reads).

| Mutant | Result |
|---|---|
| compaction reads with the latest snapshot (the defect) | 8 failed |
| the delete count is not checked | 2 failed (the "replaced since the snapshot" cases) |

The file has no case with two compactors that wait for each other in the
background; my `e17` shows 40001 for both orders. It has no random part; my
`e5` is one.

### `t/019_table_age.pl` (C4-04)

Claims: a snapshot older than the table reads an empty table after any chain of
TRUNCATE, VACUUM FULL, rewriting ALTER, change of access method or partition
creation; content that existed for the snapshot still gives 40001.

Preconditions: established (the older snapshot is a repeatable-read transaction
that read another table first; every chain is run against heap and the heap
answer, 0, is asserted). Weakness: the "opposite direction" for
`SET ACCESS METHOD heap` accepts `0`, `2` or an error, so it cannot fail; and
the tests start from tables created in Postgres. The conversion of a heap table
that existed before the snapshot is not tried, which is where C5-02 is.

| Mutant | Result |
|---|---|
| the rule removed from the reader | 9 failed |
| TRUNCATE does not carry `table_xid` | 3 failed |
| a rewrite takes the creation of the rewrite as the table's | 1 failed ("ADD COLUMN ... DEFAULT random() of content that existed for the snapshot") |
| `table_xid` never frozen by `VACUUM` | `t/019` and `t/020` pass; `t/013` (module built) dies after 200 s in `poll_query_until` on `age(relfrozenxid) < 140000000` because the id stays in the file and holds `relfrozenxid` back: caught, but only by the slow wraparound file |

### `t/020_limits_flags_check.pl` (C4-05, C4-06)

Claims: `columnar_check` is empty after every command that replaces a file; the
reader refuses a row count above 10 000 000 before allocating; a catalog of
another shape is refused; the maintenance flag follows compaction alone.

Preconditions: established. Each step of part 1 asserts the report is empty
directly after the command; the limit cases craft a catalog row at the limit and
above it; the flag case asserts the stripes' flag from the stripe catalog.

| Mutant | Result |
|---|---|
| the commit-time cleanup of a VACUUM FULL removed | 1 failed |
| the cleanup in the `ALTER TABLE` event trigger removed | 6 failed (note: one failure makes later steps fail too, the leftovers stay) |
| the reader's limit back to 2 147 483 647 | 6 failed |

### `t/002_standby.pl` (new section, C4-05 freeze conflict)

Claims: the record that freezes a file's creating id cancels a standby query
that might hold it. Mutant: the conflict removed from the redo routine: the
file dies (exit 255 after 46 checks) at `wait_for_log(qr/conflict with
recovery/)`. So the section does fail when the fix is removed, but by a `die`
of the helper and not by an assertion, and the final `unlike` passes whenever
the session is gone. The session that holds the old snapshot reads `ct`, not
the table that is frozen, so it is the snapshot and not a lock that is
cancelled; the only other possible conflict (the truncation lock) does not
concern `ct`. Two statements in the section are dead (`log_is_clean ... if 0`).

### `t/014_buffers.pl` (changed expectation)

The expectation changed from "nothing flushed by the merge" to "the commits of
the subtransactions wrote the full stripes" (`BEFORE=15`). It is the intended
new behaviour, and the later checks of the same case (16 stripes, none over
1 000 rows, 15 002 rows) are unchanged. With the mutant "the commit of a child
never flushes the parent's buffer" the changed check fails (1 failed:
"the commits of the subtransactions wrote the full stripes of the merged
buffer"); with the minimum rule removed the file passes (that rule is tested by
`t/017` only).

### Gaps that no test file covers

* `table_xid` freezing and the wraparound of the id: `t/013` is skipped in the
  runs the author's out-of-tree note names and in the installations I was
  given (the module is not installed). I built the module for both majors and
  ran the file: 17 of 17 pass on 18.7 and 19rc2 (70 s each). It names
  `created_xid` only, but through `relfrozenxid` it also fails when `table_xid`
  is never frozen (the mutant above), so the claim is tested, slowly and by a
  timeout. A lap-aliasing check for `table_xid` itself (a transaction that
  has the table's number) is not in it.
* An old metapage that holds 0 for `table_xid`: no test and none by me (the
  rule is by reading: 0 is "not normal", so the older rule applies).
* The production build never runs `t/016` (skipped): the atomicity of the
  hand-over, which is the "critical" fix of the round, has no test where it
  ships. An `injection_points`-style hook would not need assertions.

## Findings

### C5-01 (medium) The memory allowance does not hold for wide rows in a loop of exception blocks

Place: `kc_write.c`, `kc_merge_flush_prepare` (line 1705) decides to write the
parent's buffer at a child's commit from `total - total % limit` (whole stripes)
or, when the allowance is used up, from `total - towrite >= kc_min_flush_rows()`
(line 1718), which is `max(chunk_row_count, 1000)` rows (10 000 by default).
The bytes enter nowhere. The plain write path asks both (`kc_worth_flushing`,
line 540: rows or an eighth of the allowance in bytes) and flushes at once; but
the buffer of a child is flushable only by the running child, and the merged
buffer of its parent is not.

Steps (default settings, 100 kB per row; both majors):

```
CREATE TABLE tw (id int, pad text) USING columnar;
BEGIN;
DO $$ BEGIN FOR i IN 1..11000 LOOP
  BEGIN INSERT INTO tw VALUES (i, repeat('x', 100000));
  EXCEPTION WHEN division_by_zero THEN NULL; END;
  -- read pg_backend_memory_contexts at i = 1000, 5000, 9000, 9999, 10001
END LOOP; END $$;
COMMIT;
```

Observed (18.7; 19rc2 the same in a shorter run): write buffers 95 MB after
1 000 rows, 477 MB after 5 000, 859 MB after 9 000, 954 MB after 9 999 rows,
then 0 after row 10 001, when the row-count rule is finally met. The same loop
without the exception block (one insert per iteration in the same transaction)
stays at 126 MB. Earlier: 4 000 iterations, `write_buffer_memory` default, 382
MB against 126 MB; at 16 MB, 1 500 rows, 143 MB.

Expected: the allowance is soft, but its overrun must not scale with the row
width. 10 000 rows of 1 MB (a json document, a text) is 10 GB: an
out-of-memory error at best, the kernel's killer at worst, from a loader that
is correct under heap. README says "the memory of a transaction with any number
of subtransactions is what its pending rows need"; it is, and the rows are not
written.

Direction: in `kc_merge_flush_prepare` use `kc_worth_flushing()` (rows or bytes)
for the tail test as the plain path does, i.e. write everything but a tail that
is neither `min_flush_rows` rows nor an eighth of the allowance. Add a case with
100 kB rows to `t/017`.

### C5-02 (high) A table converted from heap gets the 40001 or an empty table, depending on an unrelated earlier command

Place: `kc_am.c` `kc_table_creation_xid` (line 417): for a rewrite whose old
table is not columnar the table's creation is "the transaction that wrote its
`pg_class` row, which is its creation unless it was altered since". Any update
of that row moves it: `GRANT`, `ALTER ... OWNER TO`, `ALTER TABLE ... SET (...)`,
`ADD COLUMN`, `ENABLE ROW LEVEL SECURITY` (not `ANALYZE`, `VACUUM`, `COMMENT`
or `SET STATISTICS`, which do not write a new version of the row). The reader
(`kc_storage.c` `kc_check_snapshot_age`, line 804) then finds that `table_xid`
is not visible to the snapshot and returns the empty table; the extension's own
invariant says "an error is for content that existed for the snapshot and is
gone".

Steps (a repeatable-read session first, then another session; the same
statements, major 18 and 19):

```
-- setup
CREATE TABLE other (x int); INSERT INTO other VALUES (1);
CREATE TABLE x (a int); INSERT INTO x VALUES (1), (2), (3);
-- session 1
BEGIN ISOLATION LEVEL REPEATABLE READ;  SELECT count(*) FROM other;
-- session 2, one line of this list per run
GRANT SELECT ON x TO rr;        -- or ALTER TABLE x OWNER TO rr, SET (fillfactor=60), ADD COLUMN c int,
                                -- ENABLE ROW LEVEL SECURITY, SET (autovacuum_enabled=false)
ALTER TABLE x SET ACCESS METHOD columnar;
-- session 1
SELECT count(*) FROM x;
```

Observed: 0 after any command of the first group; `ERROR: could not read
columnar table "x": it was rewritten or truncated after this snapshot was
taken` (40001) with no command, or after `ANALYZE`, `VACUUM`, `COMMENT ON`,
`SET STATISTICS`. Heap, with a rewriting `ALTER TABLE ... TYPE` in place of the
conversion, answers 0 in every case. The three rows existed for session 1.

Why this is a finding although heap answers 0: heap's answer is the documented
exception of its own rewrites, and the extension chose, for every other rewrite
of its own tables, to give the error (README, `t/019` "content that existed for
the snapshot ... the serialization failure stays"). For this one conversion the
choice depends on whether somebody ran `GRANT` on the table after the snapshot,
which no user can predict. If the choice is "as heap", an error is wrongly given
in the first row of the table above; if it is "an error", the reader gets a
silent empty table in the second. I count it as the reader losing rows that the
extension's rule says must be an error.

Direction: for a table that was not columnar, do not guess the table's creation
from `pg_class`: store 0 (the older rule: always the error, never the empty
table), except where the creation is known (the table is made in this
transaction). The cost is an error for a snapshot older than a conversion of a
table created after it, which `t/019` tests for `CREATE TABLE AS` and `SELECT
INTO` followed by the conversion; those can be kept by using, for a table made
by the same command, the transaction of the `pg_type` row of its row type
(written at creation) when it is older than the `pg_class` row.

### C5-03 (low) A reader between the two phases sees rows twice

Place: `kc_write.c`, phase one (`kc_subxact_prepare`, line 1793) writes the
stripes of the parent's buffer and leaves them there, and copies the child's
rows into slots past the parent's row count "where nobody looks"; phase two
(`kc_subxact_publish`, line 1829) removes the written rows from the buffer. In
core no code reads a table between `PRE_COMMIT_SUB` and `COMMIT_SUB`. A
`SubXactCallback` of another extension that was registered before this one
(callbacks run last-registered first) can.

Steps: a module (30 lines: `RegisterSubXactCallback` that runs
`SELECT count(*) FROM p` through SPI at `SUBXACT_EVENT_PRE_COMMIT_SUB`), loaded
as `shared_preload_libraries = 'kcprobe,kingres_columnar'`:

```
CREATE TABLE p (a int) USING columnar;
SELECT kingres.columnar_set_options('p', stripe_row_count => 1000, chunk_row_count => 100);
BEGIN; INSERT INTO p SELECT generate_series(1,999);
SAVEPOINT s; INSERT INTO p SELECT generate_series(1000,1002);
SET kcprobe.sql = 'SELECT count(*) FROM p';
RELEASE s;
```

Observed (18.7): `NOTICE: kcprobe at pre of sub 2: 2002`; after the release
and after the commit, 1002 (correct). With 5 and 3 rows (no stripe written) the
callback sees 8, which is right. A write to the table by such a callback between
the phases is the same by reading: phase two finds `merge_rows != nrows`, takes
the "else" branch (it adopts the child's buffer) and leaves the parent's rows
both in the stripe that phase one wrote and in the buffer, so the commit writes
them twice. I did not run that variant.

An error in a callback of another extension at `COMMIT_SUB` after this one's
phase two is not attributable to this extension: PostgreSQL itself leaves the
subtransaction's id in the committed list and the parent's commit stops at an
assertion in `clog.c` (`TransactionIdSetPageStatusInternal`), without
assertions it sets inconsistent status; I saw that with the same module. It is
the reason phase two must never throw, and it does not.

Expected: a reader in that window gets the right rows or an error.

Direction: mark the relation's state while a hand-over is prepared and not
published, and raise an error from a scan that starts in it ("the table is being
handed over"), or hide the stripes of a prepared child from the scans of the
transaction until phase two.

### C5-04 (low) The cleanup after `ALTER TABLE` costs time proportional to the number of columnar tables, for every `ALTER TABLE`

Place: `kingres_columnar--0.1.sql`, `columnar_check_ddl` ends with
`IF TG_TAG = 'ALTER TABLE' AND EXISTS (SELECT 1 FROM columnar_stripe) THEN
PERFORM columnar_reconcile()`. `kc_reconcile_ex` (`kc_catalog.c`, line 775)
scans all of `pg_class` and reads block 0 of the file of every columnar table
with `smgrread`, for every `ALTER TABLE` of any table, columnar or not.

Steps and numbers (no assertions, `fsync = off`, one row in every columnar
table; time of one `ALTER TABLE h ALTER COLUMN z SET DEFAULT ...` on an
unrelated heap table):

| Columnar tables | Event trigger disabled | Enabled |
|---|---|---|
| 10 | 0.14 ms | 0.67 ms |
| 1 000 | 0.14 ms | 5.94 ms |
| 5 000 | 0.16 ms | 34.89 ms |

`pg_dump -Fc` and `pg_restore` of 1 500 heap tables with a primary key and 100,
1 500 and 3 000 columnar tables: 3.3 s, 11.2 s and 22.5 s; the same restore with
`event_triggers = off` 1.6 s, 2.4 s and 3.9 s. The product of the number of
`ALTER TABLE` statements (every constraint of the dump) and the number of
columnar tables.

Expected: the cleanup runs when a columnar table was rewritten.

Direction: use a `table_rewrite` event trigger (`pg_event_trigger_table_rewrite_oid`),
which fires only for rewrites, or set a flag in `kc_relation_set_new_filelocator`
for a file that replaces another and test the flag, as `kc_write_request_reconcile`
already does for VACUUM FULL.

### C5-05 (low) Two `ALTER TABLE` of unrelated tables contend for leftover stripe rows

Place: the same trigger. When stripe rows of no live file exist (made here with
`SET event_triggers = off; ALTER TABLE c2 ALTER COLUMN b TYPE bigint;` on a
columnar table, which is what a session that disabled event triggers, or a
restore of an older database, leaves), the first `ALTER TABLE` of any table
deletes them and keeps the row locks until it commits.

Steps (18.7; 19rc2 the same):

```
CREATE TABLE ha (a int); CREATE TABLE hb (a int);
CREATE TABLE c2 (a int, b int) USING columnar; INSERT INTO c2 SELECT g, g FROM generate_series(1,100) g;
SET event_triggers = off; ALTER TABLE c2 ALTER COLUMN b TYPE bigint;   -- leaves one orphan stripe row
-- session A:  BEGIN; ALTER TABLE ha ADD COLUMN x int;                 -- deletes it, stays open
-- session B:  ALTER TABLE hb ADD COLUMN y int;                        -- waits for A
-- session A:  COMMIT;
```

Observed: B waits (one `Lock` wait in `pg_stat_activity`) and then fails with
`ERROR: tuple concurrently deleted` (XX000). Heap's `ALTER TABLE` of an
unrelated table does neither. With no leftovers there is no contention, and a
TRUNCATE, a VACUUM FULL, a rewriting ALTER and a DROP leave none (checked), so
it needs the leftovers.

Direction: in `kc_reconcile_ex` delete with the tuple-lock protocol of
`table_tuple_delete` and ignore a row that another session already deleted
(`TM_Deleted`), or do not wait at all (skip the cleanup if a row is locked).

### C5-06 (low) A catalog of the earlier shape: nothing can be read, and the hint loses the tables

Place: `kc_catalog.c` `kc_catalog_check_shape` (line 318). A database whose
stripe catalog has 13 columns, written by the build before the one that added
the `maintenance` column, gives on every read, insert, `COPY`, `VACUUM`,
`ALTER TABLE ... SET ACCESS METHOD heap` and `pg_dump` of a columnar table:
`ERROR: the stripe catalog has 13 columns, this build uses 14` with `HINT: ...
drop and create the extension again`. `columnar_check` reports it as
`unreadable`. `DROP EXTENSION` fails because the tables depend on the access
method; `DROP EXTENSION ... CASCADE` drops the tables with their data.

What I verified: `ALTER TABLE kingres.columnar_stripe ADD COLUMN maintenance
boolean NOT NULL DEFAULT false` makes every table readable at once (5 010 rows,
the sum equal to the old build's). So the data can be saved, and the hint says
the one thing that cannot.

Direction: say that in the hint (the statement above), or add the column in the
extension's upgrade path; do not tell anyone to drop the extension.

### C5-07 (note) Weak and missing checks in the tests

* `t/013` is the only file that notices a `table_xid` that `VACUUM` never
  freezes (it dies on a timeout after 200 s; `t/019`, `t/020` and `t/002`
  pass the mutant), and it is skipped wherever the `xid_wraparound` module is
  not installed, which was every installation I was given.
* `t/016` is skipped in the packaging build and the hand-over has no test where
  it ships.
* `t/019`: the mutant "a rewrite takes its own creation as the table's" is
  caught by one assertion only; "the opposite direction" for `SET ACCESS METHOD
  heap` accepts an empty table.
* `t/002` new section: the failing mutant dies in the helper (`wait_for_log`)
  and the `unlike` after it holds for a dead session; two statements are dead
  code.
* `t/017` memory bound with narrow rows only (C5-01); `t/016` part 1 point
  `alloc` does not establish that the hand-over was among the failing
  allocations.

### C5-08 (note) Stale text about the reader's limit

`kingres_columnar.c` `kc_check_one` (line 484) still says, for
`stripe_over_row_limit`, "Versions that merged the buffers of subtransactions
without a limit could write one; the reader accepts it, so the data can be
read, and a rewrite cuts it into stripes within the limit", and the detail
returned to the user says it "can be read, and VACUUM FULL or SET ACCESS METHOD
writes it again". Since the reader's limit is the writer's (10 000 000 rows) a
stripe over it is refused with "the stripe catalog row ... is corrupt" and a
rewrite cannot read it. The text is right only for a stripe over a lowered
`max_stripe_rows`. A table written by an earlier build with a stripe between
10 000 001 and the old limit of 40 000 000 rows cannot be read, copied out or
rewritten by this build (stated in the commit as "0.1 is not released").

## What held under attack

* The hand-over: every sweep above, on both majors, equal to the model;
  `ROLLBACK TO` after released savepoints, a child that commits into a parent
  that then aborts, three levels of nested commits, 300 exception blocks that
  fail every seventh time, a cursor open on the parent's buffer, a prepared
  transaction with a phase-one stripe, a hot standby that reads in the middle
  of the transaction (committed rows only, then the whole transaction), a crash
  with a prepared transaction, promotion and a write on the new primary, with
  `wal_consistency_checking = all` on both nodes and no consistency message.
* Time and memory of the hand-over: 100 000 exception blocks insert one row
  each in 0.42 s (heap: 0.32 s), 7.9 MB of write buffers at the end; 20 000
  empty blocks after 2 000 relations were written in the transaction: 0.9 s.
  Not quadratic.
* Phase two: read line by line; it does not allocate (list deletion and
  context deletion only), raise, check for interrupts or assert.
* Serializable: four write-skew schedules with a writer whose stripes are
  flushed by the commit of a child (reader before, after, inside a savepoint,
  older snapshot after the writer's commit): the commits equal heap's (reader
  40001, writer ok; with the older snapshot both ok), both majors.
* The fault injection: absent from the build without assertions (no string
  `fault_point`, `fault_nth` or "Injected" in the library; `KC_FAULT` is
  `((void) 0)`; the only two `#ifdef USE_ASSERT_CHECKING` of the extension are the
  definitions); with assertions the settings are superuser-only. The regression
  output is the same file for both builds (11 of 11 in each), the
  tests of the production build 3 060 against 3 229 with assertions (the 169
  of `t/016`).
* `columnar_compact` against every interleaving of the model (see above), and a
  second compactor that waits, a repeatable-read compactor behind a committed
  one (40001) and a read-committed compactor behind a committed one (it merges
  the copies; returns 0).
* The rewrite in REPEATABLE READ "as heap does": seven variants (type change
  with the snapshot first taken on the table or on another one, an own insert
  before, ADD COLUMN with a volatile default, serializable, in a savepoint that
  is rolled back), heap and columnar equal on 18.7 and 19rc2.
* Event trigger: with another session creating, filling, truncating or
  rewriting a columnar table (committed and rolled back), `ALTER TABLE` of an
  unrelated table deletes none of their rows and loses none; `ALTER TABLE` in a
  function, in an aborted subtransaction, on a temporary table and by an
  ordinary role works; a rolled-back rewrite brings the rows back and
  `columnar_check` is clean.
* A user's table named `pg_temp_<oid>`: the recognition needs the
  `AccessExclusiveLock` of this transaction on a relation of that oid in the
  same schema, and the worst result is an older `table_xid` for the user's own
  table, which makes the check stricter. No gain or loss for anyone else.
* The reader's limit: 10 000 001, 40 000 001 and 2 147 483 647 are refused from
  the catalog row (`t/020` and its mutant).
* Production-build suite, 18.7 without assertions: all 20 files pass.

## Not reproduced

* A loss or doubling of rows by the commit of a subtransaction under injected
  failure: none, in any of the shapes above.
* A compaction that changes what any snapshot sees: none in 600 episodes.
* A `table_xid` that makes a snapshot see an empty table for a table that was
  created in Postgres: none (11 shapes, plus `t/019`). The only silent empty
  answer is C5-02, for a conversion from heap.
* A race of the cleanup that deletes rows of a file that is about to become
  live: none (creation, rewrite, TRUNCATE in progress in another session).
* A deadlock between the cleanup and VACUUM, compaction or a rewrite: none
  seen; compaction and VACUUM serialize on the table lock and the trigger takes
  only the catalog's row lock.
* The slow `pg_restore` as a wrong answer: it is only slow (C5-04).
* An old metapage that holds 0: not constructed.

## Not covered

* Memory exhaustion by a real address-space limit: the faults are injected at
  the allocations of the write path, not at the others (hash, list, relcache);
  phase two has none by reading.
* `table_xid` across a lap of the transaction id space: the module test does
  not look at it.
* A table dropped and created again with the same file number after an oid
  wraparound; a restore from a base backup with an old snapshot beyond the
  standby experiment; archive recovery to a point in time; `pg_upgrade`
  (documented as unsupported).
* A stripe above 10 000 000 rows written by an earlier build, and the cost of
  reading one.
* Parallel workers (the scan refuses them; the callbacks do nothing without
  state), `session_replication_role = replica` and event triggers disabled for
  the whole run of a restore beyond the one leftover shape of C5-05.
* Concurrent DDL campaigns and kill-based crash campaigns; multixacts;
  SERIALIZABLE with DDL in the transaction; `READ ONLY DEFERRABLE`.

## Baseline numbers

Existing suite, out of tree, `EXT_WERROR=1`, no compiler warning, before any
mutant or experiment:

| Major | Regression | TAP |
|---|---|---|
| 18.7 (assertions) | 11 of 11 pass | 20 files, 3 229 tests, pass; `t/013` skipped (module not installed), 446 s wall |
| 19rc2 (assertions) | 11 of 11 pass | 20 files, 3 231 tests, pass; `t/013` skipped, 422 s wall |
| 18.7 (no assertions) | 11 of 11 pass | 20 files, 3 060 tests, pass; `t/016` skipped (169 tests fewer), 308 s wall |
| 18.7, `t/013` alone, module built | | 17 of 17, 70 s |
| 19rc2, `t/013` alone, module built | | 17 of 17, 67 s |

`t/016` takes 6 s, `t/017` 2 s, `t/018` 2 s, `t/019` 2 s, `t/020` 1 s, `t/009`
50 s on both majors.
