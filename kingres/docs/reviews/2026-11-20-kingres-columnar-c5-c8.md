<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->

# Independent review of kingres_columnar, increments C5 to C8

## Scope and method

Reviewed: the extension `contrib/kingres_columnar` at commit 24a124fbbd, for the increments
C5 (parallel scan, 0ba0e045d3), C6a (row numbers reserved at insert, 75d62f13a1),
C6b (non-unique B-tree, 65e050e5b1), C6c (unique indexes and index vacuum, d35f611dae),
C6d (compaction with indexes, c33794b4c5), C7 (logical decoding of inserts, ad9d1a334d)
and C8 (reuse of free space, 3d83cc28c2). The fixed findings of the 0.1 reviews and the use of
RM_EXPERIMENTAL_ID were not repeated. Nothing was fixed; the main checkout was not touched
(detached worktree, removed at the end after a clean `git status`, no commit).

Method: reading the diffs of the seven commits and the notes in
`kingres/docs/design/analytics-phase12-storage.md`; the extension's own suites on both majors;
my own differential, crash, concurrency, snapshot, standby, decoding and security probes against
private installs (`$HOME/kw-revcol`, ports 58701 and 58702, one cluster at a time, all removed at
the end); and twelve one-line mutants, each with a clean build.

## What was run, per major

| Run | PostgreSQL 18 | PostgreSQL 19 |
|---|---|---|
| Extension regression tests (`make installcheck`) | 17 of 17 pass | 17 of 17 pass |
| Extension TAP suite (`t/001` to `t/062`) | 62 files, 11044 tests, PASS | 62 files, 11047 tests, PASS |
| Differential test, columnar against heap (300 000 generated rows; 7 indexes: unique, composite with DESC, partial, expression, covering, timestamp; 34 query shapes; 4 plan modes each: default, index scan forced, bitmap forced, parallel with 7 workers; stripes from several transactions, savepoint rollbacks, 300 one-row transactions, 100 failed inserts, one compaction) | 136 comparisons, 0 mismatches | 136 comparisons, 0 mismatches |
| Crash loop: 8 clients, unique-violation inserts in exception blocks, rolled back and committed inserts, concurrent VACUUM and `columnar_compact` loop, `pg_ctl stop -m immediate` at a random moment, restart, 3 rounds | seq scan and index scan return the same rows (md5), `columnar_check()` and `columnar_check_indexes()` empty, primary key had no duplicate, all 3 rounds | 2 rounds, same result |
| Hot standby with `wal_consistency_checking = 'all'`, `hot_standby_feedback = off`, same workload, plus standby readers on REPEATABLE READ | standby equal to primary (rows, sum, index scan); no consistency or FATAL message in either log; no standby reader returned a wrong count | not run (the suite's t/060 and t/062 pass there) |
| Parallel scan, 7 workers against serial scan inside one REPEATABLE READ snapshot, 3 writers and a compaction loop running, 12 rounds | 0 mismatches (rows, sum, distinct) | not run |
| Security probes with a role with no privilege, an INSERT-only role and an owner | see Q7 | not run |
| Decoding probes with `test_decoding` | see C13-03 | same result |

The server of the suite is built with assertions (the tests say so), so fault points were available
to the extension's own tests; my probes ran without needing them.

## Answers to the questions

**1. Crash safety and WAL.** No defect found. After immediate stops under load (inserts into unique
and non-unique indexes, savepoints, rollbacks, vacuum and compaction running) every restart gave a
table whose sequential and index scans agreed, with an empty `columnar_check()` and
`columnar_check_indexes()`. The standby with full WAL consistency checking stayed equal to the primary
while vacuum, compaction and free-space reuse ran. The design choices that make this hold (take of a
free extent logged before any page, full-page images for every block, a free list with its own
checksum that reads as empty when unsound, conflict horizon in `XLOG_KC_FREE`) held up under reading;
the horizon logic is sound even when the catalog row of a deleted stripe has been pruned before
`VACUUM` lists its blocks, because the prune record of the catalog cancels the same standby queries.
What I could not test is a physical standby that is promoted in the middle of a free-list change (see
"Not examined").

**2. MVCC and concurrency.** One wrong-result defect with old snapshots (C13-01). Concurrent unique
inserts, aborted inserts, exception-block inserts, deferred and `ON CONFLICT` cases behaved correctly:
`ON CONFLICT` on a table with an index, deferrable unique constraints, exclusion constraints, foreign
keys to or from the table, `CLUSTER`, `FOR UPDATE`, TID scans, `TABLESAMPLE` and UPDATE/DELETE are
refused with a clear error; a duplicate inside the same transaction and against a concurrent writer
that aborts or commits is handled right (also t/053). Cursor and "Halloween" cases are right: a cursor
declared before an INSERT does not see the new rows, `INSERT ... SELECT` from the same table through
an index scan does not read its own new rows (300 rows, as a heap table gives).

**3. Indexes.** Index scans, ordered and backward scans, IN lists, ranges, partial, expression,
composite and covering indexes returned exactly what the heap twin returns (differential test, both
majors). The planner offers no bitmap scan and no index-only scan (forcing `enable_indexscan = off` or
`enable_seqscan = off` yields a disabled index scan, never an error). `REINDEX` works, but see
C13-01. `CREATE INDEX CONCURRENTLY` and `REINDEX ... CONCURRENTLY` are refused cleanly before anything
is made and leave no invalid index. `amcheck`: `bt_index_check(idx)`, `bt_index_check(idx, false, true)`
and `bt_index_parent_check(idx)` pass; the `heapallindexed` form fails (C13-04).

**4. Free space reuse (C8).** The rule "free only blocks whose deleting transaction committed before
the oldest snapshot" is implemented in one function and is correct; with old REPEATABLE READ snapshots,
a prepared compaction (the VACUUM waits for it, as for heap), and four writers in the suite, no
stripe was overwritten while visible. Removing the rule makes an old snapshot fail with "block 1
belongs to another stripe or is damaged" (mutant A below), and t/062 kills that mutant. I found no
case where reused space holds two live versions or loses one. Cost, accepted by the design: a
table that never has a moment without a writer never gets a list.

**5. Logical decoding (C7).** Inserts are decoded once, savepoint rollbacks and rolled-back prepared
transactions produce nothing, a two-phase commit decodes at `COMMIT PREPARED`, a dropped column is
absent and the order within a table is kept; UPDATE and DELETE are refused at the table, so they
cannot be decoded wrongly. A defect: rows that were inserted before an `ALTER TABLE ... ADD COLUMN
... DEFAULT` in the same transaction are decoded with NULL in the new column (C13-03). A row of about
1 GB (two values of 520 MB) was stored and decoded without error at `wal_level = logical`. The
publication guard keeps `kingres.columnar_stripe` out of publications, but `test_decoding` shows its
rows (note C13-08).

**6. Parallel scan (C5).** Exactly-once held: 12 rounds of seven workers against a serial scan in one
snapshot with writers and compaction running gave identical rows, sums and distinct counts; the
differential test with seven workers matched. The mutant that makes the stripe counter non-atomic is
killed.

**7. Security.** The functions that act on a table check ownership or are revoked from PUBLIC:
a role with no privilege gets "permission denied" from `columnar_info`, `columnar_stripes`,
`columnar_options`, `columnar_check`, `columnar_check_indexes`, `columnar_write_paths`, the stripe
catalog and the table, and "must be owner" from `columnar_compact` and `columnar_set_options`; the
developer settings (`reserve_gate`, `max_stripe_rows`, truncate timeout) are superuser only; fault
points exist in assertion builds only. I found no crash or read of unprivileged data. One denial of
service reachable by a role that has only INSERT: C13-02.

**8. Tests.** See the mutant table. Eleven of eleven valid mutants are killed once each is run
against the right tests; two mutants initially "survived" only because I ran them against the wrong
test files (the two `TransactionIdPrecedes` lines of `kc_catalog.c` are in `kc_catalog_live_ranges`
for index vacuum and in `kc_catalog_live_blocks` for C8, in that order of the file); rerun with the
right tests both are killed. The gaps are in what no test tries: an index created or rebuilt while an
older snapshot lives (C13-01), the exhaustion of row numbers (C13-02), a column added in the
transaction that inserted the rows (C13-03), `amcheck` (C13-04).

## Findings

### C13-01 (high) An old snapshot loses all rows through an index that was created or rebuilt after a compaction

A REPEATABLE READ transaction, or any older snapshot, that still sees the stripes a compaction
replaced, reads nothing through an index that was built after the compaction committed. The build
(`kc_index_build_range_scan` with a null snapshot) indexes only the stripes visible now; the old,
recently deleted stripes get no entry. Heap avoids this by indexing recently dead tuples. The sequential
scan of the same snapshot returns the rows, so one transaction gets two answers.

Reproduction (identical on both majors):

1. Table `c2` columnar, 4 inserts of 20 000 rows, no index.
2. Session A: `BEGIN ISOLATION LEVEL REPEATABLE READ; SELECT count(*) FROM c2;` (80 000).
3. Session B: `SELECT columnar_compact('c2', 1000000); CREATE INDEX c2_k ON c2(k);`
4. Session A: `SET enable_seqscan = off; SET kingres_columnar.enable_custom_scan = off;`
   `SELECT count(*) FROM c2 WHERE k < 500;` gives 0 through the index; the sequential scan gives 40 000.
   The same with a heap table (delete, then create the index) gives 40 000.
5. Same result when the index existed before and session B runs `REINDEX INDEX c2_k` after the
   compaction and a `VACUUM`, provided session A had not touched the table (so it holds no lock on the
   index) before.

Suggested fix: build and rebuild indexes with the entries of every stripe version that is not yet
removable (the same set `kc_catalog_live_ranges` keeps), as heap indexes recently dead tuples; or set
`indcheckxmin` and a build xmin so that the planner does not use the index for an older snapshot (this
is what heap does for broken chains); add a TAP test with an older snapshot, a compaction and then
`CREATE INDEX` and `REINDEX`.

### C13-02 (high) Any role with INSERT can use up the row numbers of an indexed table in minutes

A table with an index reserves `stripe_row_count` row numbers (150 000 by default, up to 10 000 000)
for every writer, and an aborted transaction or subtransaction gives nothing back (documented in the
README as "58 million such writers"). Measured: 20 000 rolled-back one-row inserts in a PL/pgSQL loop
took 170 ms (about 120 000 per second) and moved the next row number from 0 to 3 000 000 000. The
table has 8.8 x 10^12 numbers, so about 59 million aborts, roughly 8 minutes of one session, end the
table: every later insert fails with "has no row numbers left". A failed statement in autocommit (a
unique violation, a check constraint, a cancelled query) wastes the same amount. With the table option
`stripe_row_count = 10000000` (owner) the same is 880 000 aborts, a few seconds. Only a rewrite of the
table (`VACUUM FULL` resets the numbers, tested) recovers it, which needs the owner. The README states
the arithmetic but not that the rate is reachable by any role that may INSERT, nor that concurrent
committers also burn numbers (a commit behind another writer's reservation leaves a gap).

Reproduction: `CREATE TABLE w(id int, k int) USING columnar; CREATE INDEX ON w(k);` then
`DO $$ BEGIN FOR i IN 1..20000 LOOP BEGIN INSERT INTO w VALUES (i,i); RAISE EXCEPTION 'x';
EXCEPTION WHEN OTHERS THEN NULL; END; END LOOP; END $$;` then
`SELECT first_row FROM kingres.columnar_stripes('w')` after one more insert: 3000000000.

Suggested fix: reserve in small steps (for example 1 000 numbers, doubling only while the same buffer
keeps filling), so that waste per abort is bounded by a small number; or give back the reservation in
the abort callback by a deferred, logged return performed by the next writer or by VACUUM; or make
numbers reusable once index vacuum has proved no entry names them (VACUUM already knows this, the
claim being "no entry below `low` outside a live range"); at least raise a warning when the numbers
used pass a threshold, and add a test of the exhaustion error.

### C13-03 (medium) Logical decoding gives NULL for a column added with a default after the rows were inserted in the same transaction

`kc_log_logical_rows` pads the rows of a buffer that is narrower than the table with NULL, while a
scan fills them with the column's missing value. Reproduction (test_decoding, both majors):
`BEGIN; INSERT INTO d VALUES (1,'x',1); ALTER TABLE d ADD COLUMN z int DEFAULT 5; COMMIT;`
`SELECT z FROM d` gives 5; the decoded row is `z[integer]:null`. The same sequence on a heap table
gives a tuple that PostgreSQL's decoder completes with the default 5. A subscriber therefore differs
from the publisher.

Suggested fix: fill the missing columns with the attribute's missing value (the function a scan uses,
`kc_missing_value`) when forming the logical tuple, or flush the buffers of the table at `ALTER TABLE
ADD COLUMN`. Add the case to t/061.

### C13-04 (medium) `amcheck` with `heapallindexed` cannot verify a columnar index, and the message is wrong

`bt_index_check(idx, true)` builds its bloom filter through the access method with a concurrent
index info; `kc_index_build_range_scan` refuses it with "CREATE INDEX CONCURRENTLY and REINDEX
CONCURRENTLY are not supported on columnar tables", which is misleading for a check. So the one
tool that compares an index with its table (the request of question 3) does not work; the extension's
own `columnar_check_indexes()` covers only the "row without entry" direction, 50 million entries at
most, and superuser only.

Reproduction: `SELECT bt_index_check('m_k'::regclass, true);` on any columnar table with an index.

Suggested fix: accept `ii_Concurrent` when the call comes with a scan that is not a build (amcheck
passes one), or detect amcheck by the absence of a build state; at least change the message. Add an
amcheck call to the index tests.

### C13-05 (medium) A committed subtransaction writes a stripe of its own: one page per row in a savepoint loop

On a table with an index, every subtransaction that inserted rows and committed while its parent also
holds a buffer writes its own stripe (decision 3 of C6a). A PL/pgSQL loop with one inserting
`BEGIN ... EXCEPTION` block per row made 2 001 stripes and 2 002 pages (16 MB) for 2 001 rows; a single
statement made 1 stripe and 3 pages. The sequential scan took 6.1 ms against 0.22 ms. Each such
stripe also takes a catalog row and (see C13-02) a reservation. Not a correctness problem, but a common
application pattern (exception block per row) makes a columnar table much larger and slower until
`columnar_compact`.

Suggested fix: let a committed subtransaction hand rows with their numbers to the parent's buffer when
the parent's reservation has room (numbers are still unique), or compact small stripes automatically
at VACUUM; document the pattern.

### C13-06 (low) The README and the design say compaction takes "the locks of a writer"; the code comment says SHARE UPDATE EXCLUSIVE

The comment on `kc_compact_index_entries` says compaction holds SHARE UPDATE EXCLUSIVE on the table.
That mode conflicts with `VACUUM` and with another compaction, which is also what keeps index vacuum
from running while compaction adds entries (index vacuum would remove entries of numbers that have no
claim yet). The property is therefore essential, but the C6d notes call the locks "those of a writer".
Suggested fix: state the real mode and the reason in the notes and README, and add an assertion that
index vacuum and compaction hold conflicting locks.

### C13-07 (note) A prepared compaction blocks VACUUM of the table

`VACUUM` waits for a prepared `columnar_compact` (SHARE UPDATE EXCLUSIVE held until `COMMIT PREPARED`),
as it would for heap; free-space reuse and index vacuum are delayed with it. No fix needed; worth a line
in the operations notes.

### C13-08 (note) `test_decoding` shows the stripe catalog rows

With `test_decoding` (no publication filter) the `kingres.columnar_stripe` inserts appear beside the
table's rows, as for any table. The guard only keeps the catalog out of publications. Nothing to fix;
a slot with a custom plugin must filter it.

### C13-09 (note) Mutants that cannot be built

One mutant (the probe of the unique check falling back to the older rule) fails to build with
`-Werror` because the replaced helper becomes unused; the property is covered by the other mutants of
`kc_version_gone`.

## Mutant table

Each mutant is one changed line, built clean (all warnings are errors) on PostgreSQL 18 with the
installed library replaced, and the named tests run. "Killed" means at least one test failed.

| # | File and change | Tests run | Result |
|---|---|---|---|
| A | `kc_catalog_live_blocks`: free blocks of deleted stripes without the "before the oldest snapshot" test | t/062 | killed |
| B | `kc_catalog_live_ranges`: index vacuum drops entries of deleted stripes without the same test | t/056, t/059 | killed |
| 3 | `kc_free_space`: conflict horizon not passed to the free record | t/062 | killed (standby query not cancelled) |
| 4 | `kc_vacuum_dead`: no `low` guard (claims of running writers) | t/056, t/053 | killed |
| 5 | `kc_version_gone`: a stripe deleted by an aborted transaction counts as gone | t/059, t/053 | killed |
| 6 | `kc_scan_fetch_row`: no guard on `visible_rows` | t/051, t/058 | killed |
| 7 | `kc_scan_fetch_row`: own-row command rule `>=` to `>` | t/058, t/051 | killed |
| 8 | parallel scan: stripe counter read and written, not fetch-and-add | t/043 | killed |
| 9 | `kc_write_stripe`: compaction copies are also logged for decoding | t/061 | killed |
| 10 | `kc_meta_take_extent`: run one block short accepted | t/062 | killed |
| 11 | `kc_index_vacuum`: reservation lock exclusive to share | t/056 | killed |
| 12 | `kc_catalog_probe_row`: older rule instead of `kc_version_gone` | t/059, t/053 | not built (unused helper under -Werror) |

Not covered by any test as written (so a mutant here would survive): an older snapshot with a later
`CREATE INDEX` or `REINDEX` (C13-01), the exhaustion error, a column added in the transaction that
inserted the rows (C13-03), `amcheck`.

## Not examined

- Promotion of a physical standby in the middle of a free-list change, and a standby crash during
  replay of `XLOG_KC_FREE`.
- A real logical subscriber (pgoutput) fed by a crashing publisher; only `test_decoding` was used here
  (t/061 does the subscriber case).
- Rows of a size near 1 GB with several columns at `wal_level = logical` beyond the one case tried.
- Exhaustion itself: the error at the end of the number space was not reached (the last 8.8 x 10^12
  numbers were extrapolated from the measured rate).
- Serializable isolation with unique indexes and `columnar_check` against reused pages while VACUUM
  lists them (a false report is possible in theory; not seen).
- Partition conversion in `kingres_timepart` that creates indexes, and `kingres_shard` bucket moves.
- Autovacuum triggered work on tables with indexes under sustained load; transaction id wraparound
  with indexes.
- PostgreSQL 19 for the standby and parallel probes (the suites pass there).
