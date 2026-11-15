<!-- SPDX-License-Identifier: AGPL-3.0-only -->
# Adversarial review: contrib/kingres_columnar 0.1 (phase 12, increments C1 to C4)

Reviewer: independent (not the author). Reviewed state: commit d305dbb9fe,
exported with `git archive` (paths `contrib/kingres_columnar` and
`kingres/dev`) into a private directory (`~/kw-review6/src`). The working tree
was not used for any result. Line numbers below are those of that commit.
Nothing in the repository was changed except this file.

## Summary

The storage layer survived everything I did to it in terms of durability: no
committed data was lost and no torn or half-visible transaction appeared in 58
crash and promotion rounds (details below). The defects are elsewhere: the
interaction of the write buffers with table rewrites, snapshots and
subtransactions, the choice of plans, and the validation of what is read from
disk. Five findings return wrong answers or wrong committed data without any
error, two crash the server, and all seven reproduce on PostgreSQL 18.7 and on
19rc2.

| Severity | Count |
|---|---|
| critical (committed data wrong, or wrong answer without error) | 5 |
| high (crash reachable by a user or a damaged file) | 2 |
| medium | 3 |
| low | 2 |
| note | 2 |

| Id | Severity | Component | Statement |
|---|---|---|---|
| C1-01 | critical | `kc_catalog_rekey`, `kc_relation_copy_for_cluster` | `VACUUM FULL` and `SET TABLESPACE` give an older REPEATABLE READ or SERIALIZABLE snapshot an empty table; heap keeps the rows visible. |
| C1-02 | critical | `kc_write.c`, `kc_am.c` | Rows buffered by the transaction are written twice when the same transaction rewrites the table (`ALTER COLUMN TYPE`, `ADD COLUMN` with a volatile default, a `SET ACCESS METHOD` round trip); the duplicates are committed. |
| C1-03 | critical | `kc_encode.c`, `kc_skip.c` | After a binary-coercible `ALTER COLUMN TYPE` (int4 to oid) stripes keep minimum and maximum computed under the old ordering and the custom scan skips stripes that contain matching rows. |
| C1-04 | critical | `kc_write.c` (`kc_subxact_callback`), `kc_scan.c` | A cursor opened before `RELEASE SAVEPOINT` loses the rows the released subtransaction buffered. |
| C1-05 | critical | `kc_cscan.c` (`kc_set_rel_pathlist`) | The plain sequential scan of a columnar partition stays parallel-safe; as a non-partial child of a Parallel Append it runs in a worker that cannot see the leader's buffered rows. |
| C1-06 | high | `kc_write.c`, `kc_scan.c` | The buffered rows keep the old column type after a type-changing rewrite; reading them in the same transaction segfaults. |
| C1-07 | high | `kc_encode.c` (`kc_decode_chunk`) | A damaged byte inside a by-reference value (an array) segfaults the server; the README says everything read from disk is validated. |
| C1-08 | medium | `kc_catalog.c`, `kc_scan.c` | Under SERIALIZABLE, transactions that use different columnar tables abort each other (all stripes share one catalog index; no relation-level predicate lock). |
| C1-09 | medium | `kingres_columnar--0.1.sql` | A `CREATE SCHEMA ... CREATE TABLE` command bypasses the DDL event trigger; the state it leaves makes every later `CREATE TABLE`, `ALTER TABLE` and `CREATE TRIGGER` of every user fail. |
| C1-10 | medium | design | A publication on a columnar table, or `FOR ALL TABLES`, is accepted and silently replicates none of the columnar rows. |
| C1-11 | low | `t/002_standby.pl` | The test fails on 19rc2 (and can on 18) because it waits for the primary's write position, which lies before the last record of a rolled-back load. |
| C1-12 | low | `sql/columnar_types.sql` | The test needs a server built with libxml; without it the file fails and `ext.sh` stops before the TAP tests. |
| C1-13 | note | `kc_vacuum.c` | Columnar relations have `relfrozenxid = 0`, so `age(relfrozenxid)` is 2147483647 for them. |
| C1-14 | note | packaging | When the role name equals the extension schema (`kingres`), `$user` in the default search path resolves into the extension's schema. |

## Scope and method

Read completely: all of `src/` (6219 lines: `kc_am.c`, `kc_storage.c`,
`kc_catalog.c`, `kc_encode.c`, `kc_write.c`, `kc_scan.c`, `kc_skip.c`,
`kc_cscan.c`, `kc_vacuum.c`, `kingres_columnar.c`, the two headers),
`kingres_columnar--0.1.sql` and the README. I did not read the design document
line by line, and I read the test files only to see which situations they
cover; every conclusion below comes from running the code. I did not read the
source of any other columnar implementation.

Built and run, on PostgreSQL 18.7 and on PostgreSQL 19rc2, both with
assertions enabled, each copied into my own area (`pg_config` reports paths
inside it): `kingres/dev/ext.sh kingres_columnar sync clean build install
regress tap` with `EXT_WERROR=1`. The build has no warning on either major.
The machine was shared and loaded (load average 6 to 8); everything ran with
`nice` and at most `-j4`.

Test-suite results on my installations:

| Suite | 18.7 | 19rc2 |
|---|---|---|
| pg_regress, 10 files | 9 pass; `columnar_types` fails (C1-12: my 18.7 build has no libxml) | 10 pass |
| TAP `001_crash_recovery`, `003_backup_dump`, `004_concurrent`, `005_corruption` | pass | pass |
| TAP `002_standby` | pass (3 runs) | fails, subtest 32, in 2 of 2 runs (C1-11) |
| TAP `006_compare` | pass | pass |

Ran by hand, mostly in a throwaway server per experiment (ports 55xxx), with
the extension preloaded. Server processes were killed with `kill -9` only
inside my own data directories.

| Experiment | Runs |
|---|---|
| Crash harness 1: four concurrent sessions (a writer of random batches of 500 to 4500 rows in one transaction, a two-phase writer using `PREPARE TRANSACTION` and `COMMIT PREPARED`, two maintenance loops running `columnar_compact`, `VACUUM`, `VACUUM FULL`, `CHECKPOINT`, `SET ACCESS METHOD heap` and back on a second table); `kill -9` of the postmaster at a random moment (0.1 to 7 s); restart; every acknowledged batch must be complete, no batch partial (row count and the sum of a column agree), `columnar_check` clean except orphan catalog rows, the table writable. Stripe size 1000 rows so every batch crosses many stripes | 14 rounds on 18.7 (`full_page_writes = on`), 12 rounds on 19rc2 |
| The same, but `kill -9` of one random backend (the postmaster restarts the server itself) and `full_page_writes = off` | 12 rounds on 18.7 |
| Crash harness 2: one table, transactions that `TRUNCATE` and reload it (with a second `TRUNCATE` in the same transaction, and with a `TRUNCATE` rolled back by `ROLLBACK TO SAVEPOINT`), while two other sessions loop over `SET TABLESPACE` (both ways), `ADD COLUMN` with default, `DROP COLUMN`, `ALTER COLUMN TYPE` both ways, `VACUUM` and `CHECKPOINT`; after the kill the table must hold exactly one complete generation | 12 rounds on 18.7 |
| Crash harness 3: a synchronous streaming standby (`synchronous_standby_names`), the harness 1 workload plus a reader looping on the standby, `kill -9` of the primary postmaster, promote the standby, check the same properties | 8 rounds on 18.7 |
| `pg_basebackup -X stream` taken while a loop of loads, `columnar_compact`, `VACUUM` and `CHECKPOINT` runs; start the backup, check every batch acknowledged before the backup began is present and complete | 1 |
| `wal_consistency_checking = 'KingresColumnar'` on primary and a standby over a load, rolled-back load, compaction, `VACUUM FULL`, two rewrites, `TRUNCATE`, `ADD COLUMN`, `ALTER TYPE` | 1 |
| Visibility battery against heap, same statements on both: savepoints (rollback, release, nested), cursors opened before inserts, `INSERT ... SELECT` from the same table (two levels), a statement that fills 5 stripes, a function that inserts and reads, rollback; row counts and sums compared at every step | 1 script, 6 transactions |
| Random savepoint program (6 transactions of 20 to 60 steps: inserts of 1 to 2500 rows, `SAVEPOINT`, `ROLLBACK TO`, `RELEASE`, a cursor check) compared with heap after every step; also with `write_buffer_memory = 1MB` so buffers flush early inside subtransactions | 26 + 26 seeds, no difference |
| 121 predicates (all the skip logic shapes: operators, cross-type int2/int4/int8/float4/float8/numeric/date/timestamp/timestamptz, `IN`, `= ANY` with NULL, `BETWEEN`, `IS [NOT] NULL`, `OR`, casts, a `COLLATE "C"` constant on a default-collation column, `varchar`/`text`/`name`/`bpchar`/`uuid`/`inet`/`money`/`oid`/`interval`/`bytea`/`"char"`, NaN, infinities, negative zero, empty strings, NULLs, two stripe orderings) run on a columnar table (custom scan forced) and on a heap table, count and sum compared | 121, all equal |
| Custom scan in nested loops with parameters, lateral subqueries with `LIMIT`, scroll cursors (forward, backward, absolute, relative, `LAST`, past both ends) with the custom scan on and off | all equal to heap |
| DDL battery compared with heap: `ADD COLUMN` with constant, volatile and text defaults, `DROP COLUMN`, `ALTER TYPE` (also `USING`), `SET DEFAULT`, `SET ACCESS METHOD` both ways, `VACUUM FULL`, `columnar_compact`, `SET TABLESPACE`, `TRUNCATE`; each result compared by hash of all rows | all equal when the transaction had no buffered rows; C1-02 and C1-06 when it had |
| Refusals: indexes by every route I could think of (primary key, unique, exclusion, `ALTER TABLE ADD`, `LIKE INCLUDING INDEXES`, partition of an indexed parent, attach to an indexed parent, `default_table_access_method`), temporary and unlogged tables, `UPDATE`, `DELETE`, `MERGE` (matched and not matched), `SELECT FOR UPDATE/SHARE`, `ON CONFLICT`, `TABLESAMPLE`, `CLUSTER`, `ctid` and TID scans, `xmin`, foreign keys in both directions, `AFTER` row trigger | all refused cleanly except the `CREATE SCHEMA` route (C1-09) |
| Partitioning with columnar and heap partitions, partition pruning, attach and detach of populated tables, inheritance, `CREATE TABLE AS`, materialized view create and refresh, `CREATE DATABASE ... TEMPLATE` from a database with columnar tables, `pg_dump` and restore | correct (options are not dumped, as the README says) |
| Privileges: a non-owner and a column-only grantee calling every `kingres.columnar_*` function, the stripe catalog, the event trigger functions, creating objects in schema `kingres` | all refused; SQL injection by table name: nothing in the C code or the SQL builds a query from a name |
| Wraparound: 160 000 transactions with `autovacuum_freeze_max_age = 100000` and `vacuum_freeze_min_age = 0` | the stripe catalog was frozen by autovacuum (stripe xmin shown as 2), the data stayed readable, `columnar_check` clean |
| Mutation of damaged files (checksums off): a script rewrites the relation file of four tables (codecs none, pglz, lz4, zstd; 13 columns of 12 types with NULLs) in seven ways: bytes of the stripe header, footer fields with the CRC recomputed, bytes of the chunk data, page headers and special areas, the metapage, 4-byte fields of chunk data set to extreme values, a zeroed page or a truncated file. After each, one session runs a sequential scan, the custom scan with seven filter shapes, `columnar_info`, `columnar_stripes`, `COPY TO`, `ANALYZE`, `VACUUM`, `columnar_compact`, `columnar_check`, `VACUUM FULL` | 420 iterations on the normal build |
| The same, with the extension compiled with `-fsanitize=address,undefined -fno-sanitize-recover=undefined` and loaded into the assertion-enabled server with the sanitizer runtime preloaded | 360 iterations |
| Mutation of the catalog: each numeric column of `kingres.columnar_stripe` set to 0, -1, the type maximum, and plausible-but-wrong values; `col_stats` and `cid_points` replaced by truncated, oversized, all-zero and all-ones values; the same battery | 63 + 11 iterations, no crash |
| Concurrency of two-session experiments: older snapshots against `VACUUM FULL`, `SET TABLESPACE`, `SET ACCESS METHOD`, `columnar_compact`; two SERIALIZABLE transactions on related and unrelated tables | see C1-01, C1-08 |
| Logical decoding with the `pgoutput` plugin on a publication with one heap and one columnar table | see C1-10 |
| 1500-column table, 30000 commands in one transaction (240 kB of command-id points), 100 000 one-row transactions (100 000 stripes) | correct; the last table is 781 MB for 100 000 rows and a `count(*)` takes 0.5 s |

Every reproduction below was repeated on PostgreSQL 19rc2 and gives the same
result, except where noted. Sanitizer: AddressSanitizer does not instrument
`palloc`, so it sees only the `malloc`-sized chunks (`MemoryContextAllocHuge`),
the stack and globals; UndefinedBehaviorSanitizer sees the whole extension.
Neither reported anything in the extension's own code during the 360
iterations; the one crash among them is the one in C1-07 and happens inside
`array_out` of the server. valgrind is not installed on the machine, so I did
not use it.

Not done, see "Not covered": logical apply into a columnar table, `pg_rewind`,
archive recovery, relation files of more than 1 GB (several segments), hot
standby long query against a truncation (the TAP test covers it; I did not
repeat it), `pg_upgrade` (out of scope), a standby promotion and the DDL crash
harness on 19rc2.

Two harness remarks so that nobody trusts a number too much. First, the crash
harnesses printed a handful of lines like "batch 11500086 acked n= got=1177":
those are two-phase batches whose `PREPARE` succeeded on the server but whose
client was killed before it recorded the batch size; I committed them after
restart, so they are committed batches with an unknown expected size, not
losses (the sum check of the same round passed). Second, a kill at a random
moment does not hit every window of a flush on purpose. I could not stop the
server between "pages logged" and "catalog row inserted" deterministically
(no debugger on the machine); with stripes of 1000 rows the harnesses flushed
tens of thousands of stripes in total, so the window was probably hit many
times, but I cannot say how many.

## Findings

### C1-01 critical: VACUUM FULL and SET TABLESPACE are not MVCC-safe

Component: `kc_relation_copy_for_cluster` (kc_am.c:468), `kc_catalog_rekey`
(kc_catalog.c:416).

A stripe is visible when its catalog row is visible. `VACUUM FULL` writes new
stripes with new catalog rows (xmin is the `VACUUM FULL` transaction);
`SET TABLESPACE` updates every catalog row to the new filenode (the new row
versions have the `ALTER` transaction as xmin). A REPEATABLE READ or
SERIALIZABLE transaction whose snapshot was taken before the command committed
therefore sees no stripe at all, and gets an empty table with no error. For a
heap table both commands are MVCC-safe (the documentation lists only `TRUNCATE`
and the table-rewriting forms of `ALTER TABLE` as exceptions), so users have no
reason to expect it. The README says that `VACUUM FULL` follows "from
PostgreSQL's own rules".

Steps (two sessions; the first has not touched the table, so no lock blocks
the second):

```
CREATE TABLE c (a int) USING columnar;  INSERT INTO c SELECT generate_series(1,1000);
CREATE TABLE h (a int);                 INSERT INTO h SELECT generate_series(1,1000);
CREATE TABLE other (x int);
-- session A
BEGIN ISOLATION LEVEL REPEATABLE READ;  SELECT count(*) FROM other;   -- snapshot taken
-- session B
VACUUM FULL c;   VACUUM FULL h;         -- or: ALTER TABLE c SET TABLESPACE other_ts;
-- session A
SELECT count(*) FROM c;                 -- 0   (heap: SELECT count(*) FROM h gives 1000)
```

What happens: 0 rows. What should happen: 1000, as for heap. The same with
`ALTER TABLE c SET TABLESPACE ...` (0 against 1000 for heap). `columnar_compact`
is not affected (old stripes stay visible to the old snapshot). Reproduced on
18.7 and 19rc2.

Suggested fix: make the stripe's visibility independent of the filenode and of
the transaction that moved it. For `SET TABLESPACE` key the catalog by the
relation OID (the blocks do not move, nothing needs to be updated). For
`VACUUM FULL` carry the xmin of each source stripe over to the new stripe's
catalog row (for example an explicit `writer_xmin` column checked with
`XidInMVCCSnapshot` in the extension, or freeze the new rows and refuse the
rewrite while a snapshot older than the oldest stripe is active), or refuse
`VACUUM FULL` and `SET TABLESPACE` while `GetOldestNonRemovableTransactionId`
is older than the newest stripe, with a clear message.

### C1-02 critical: a rewrite in the transaction that buffered rows duplicates them

Component: `kc_write.c` (buffers are keyed by relation OID), `kc_am.c`
(`kc_finish_bulk_insert`, `kc_relation_copy_for_cluster`).

Rows an `INSERT` has written but not yet flushed live in a buffer keyed by the
OID of the table. A table rewrite (`ALTER COLUMN TYPE`, `ADD COLUMN` with a
volatile default, `SET ACCESS METHOD` to the same table again) scans the old
file (the scan includes the pending rows), writes them into a new file
through the new transient table, and swaps the files. The old OID's buffer is
never emptied, so at commit its rows are flushed again into the new file.
After `COMMIT` the rows are in the table twice.

```
CREATE TABLE r5 (id int, f int) USING columnar;
BEGIN;
INSERT INTO r5 VALUES (1,1),(2,2);
ALTER TABLE r5 ALTER COLUMN f TYPE bigint;
SELECT count(*) FROM r5;                  -- 4
COMMIT;
SELECT count(*) FROM r5;                  -- 4 (expected 2)
```

What happens: every row inserted by the transaction before the rewrite is
doubled, and stays doubled (the table is append-only; there is no way to remove
a row). Variants that give the same result: `ADD COLUMN d float8 DEFAULT
random()` instead of the type change; a table created in the same transaction;
the insert in an outer level and the `ALTER` after a `SAVEPOINT`; `BEGIN;
INSERT ...; ALTER TABLE t SET ACCESS METHOD heap; ALTER TABLE t SET ACCESS
METHOD columnar; COMMIT`. A rewrite with no buffered rows is correct (the DDL
battery against heap passes). Reproduced on 18.7 and 19rc2.

Suggested fix: flush the relation's buffers before any command that rewrites
or scans-and-swaps it, from a `ProcessUtility` hook for `ALTER TABLE`,
`TRUNCATE`, `REFRESH` and similar (a rewrite then reads the stripes and the old
file's catalog rows become orphans that `columnar_reconcile` already removes).
Buffers of an outer subtransaction cannot be flushed from an inner one; refuse
the command in that case, as `TRUNCATE` already does. As a safety net,
`relation_set_new_filelocator` for the new table could record that its source
was consumed, so a second flush of the same rows is impossible.

### C1-03 critical: stale minimum and maximum after a binary-coercible type change

Component: `kc_encode.c` (`kc_typeinfo_build`), `kc_skip.c`, footer format.

Stripes store the minimum and maximum of each chunk but not the type that
produced them. `ALTER COLUMN TYPE` between binary-coercible types does not
rewrite the table (int4 to oid, and the reverse, are examples; the comparison
of oid is unsigned, of int4 signed). The old stripes then keep minimum and
maximum computed with the old ordering, and the skip logic reads them with the
new ordering. The custom scan skips stripes that hold matching rows.

```
CREATE TABLE s1 (a int) USING columnar;
SELECT kingres.columnar_set_options('s1', stripe_row_count => 1000, chunk_row_count => 100);
INSERT INTO s1 SELECT generate_series(-5, 2000);
ALTER TABLE s1 ALTER COLUMN a TYPE oid;
SET enable_seqscan = off;
SELECT count(*) FROM s1 WHERE a > 3000000000::oid;            -- 0
SET kingres_columnar.enable_chunk_skipping = off;
SELECT count(*) FROM s1 WHERE a > 3000000000::oid;            -- 5 (right; heap gives 5)
```

What happens: 0 rows and `Columnar Stripes Skipped: 3 of 3` in `EXPLAIN
(ANALYZE)`. What should happen: 5. The same holds for `= 4294967291::oid`
(0 against 1). It is a wrong answer without error and it needs no privilege
beyond owning the table. Reproduced on 18.7 and 19rc2.

Suggested fix: record the type OID (and collation) of each column in the
footer and in `col_stats`, and ignore the statistics of a chunk when they do
not equal the column's current type and collation. This also protects the
collation case (by reading the code, a column moved from "C" to a collation
without statistics is safe today only because the skip logic tests the current
column; I did not run that case) and any future type with a changed ordering.
It is a format change (stripe version 2).

### C1-04 critical: a cursor loses rows when a savepoint is released

Component: `kc_write.c` (`kc_subxact_callback`, `kc_write_pending_fetch`),
`kc_scan.c` (`kc_next_pending`).

A scan remembers the buffers it must read as (serial, row count) pairs and
looks the buffer up by serial on every row. When a subtransaction commits, its
buffer is merged into its parent's buffer and the child's buffer, with its
serial, is removed. A cursor that was opened inside the subtransaction (or any
scan that is still open when the savepoint is released) then finds no buffer for
its reference and ends without the child's rows.

```
CREATE TABLE cu (a int) USING columnar;
BEGIN;
INSERT INTO cu VALUES (1);
SAVEPOINT s;
INSERT INTO cu VALUES (2);
DECLARE cc CURSOR FOR SELECT a FROM cu ORDER BY a;
RELEASE s;
FETCH ALL FROM cc;        -- 1   (heap: 1 and 2)
COMMIT;
SELECT * FROM cu;         -- 1 and 2: the rows are there, only the cursor missed them
```

What happens: the cursor returns 1 row, and no error. What should happen: 2
rows. Reproduced on 18.7 and 19rc2.

Suggested fix: when buffers are merged keep the child's serial as an alias of
the parent with the row offset (a small list in the parent), or have scans copy
the row pointers they need when they start, or refuse to merge while a scan
pins the relation.

### C1-05 critical: a parallel plan does not see the leader's buffered rows

Component: `kc_cscan.c` `kc_set_rel_pathlist` (lines 176 to 216).

The hook removes `Gather` paths and the partial path list of a columnar table
but leaves the ordinary sequential scan path marked parallel-safe. That path is
a legal non-partial child of a `Parallel Append` over a partitioned table. It
then runs in a worker, which has the leader's snapshot and transaction state
(so it sees the leader's flushed stripes) but not the leader's backend-local
write buffers. Rows inserted by the transaction and not yet flushed are
missing from the answer, without an error. The custom scan path is
parallel-unsafe, but it loses to the plain scan whenever the costs tie or the
plain scan is cheaper, which is common for small partitions.

```
CREATE TABLE pp (k int, v int) PARTITION BY RANGE (k);
CREATE TABLE pp_h1 PARTITION OF pp FOR VALUES FROM (0) TO (1000000);
CREATE TABLE pp_h2 PARTITION OF pp FOR VALUES FROM (1000000) TO (2000000);
CREATE TABLE pp_c  PARTITION OF pp FOR VALUES FROM (2000000) TO (3000000) USING columnar;
INSERT INTO pp SELECT g, g FROM generate_series(0,1999999) g;
INSERT INTO pp_c SELECT 2000000+g, g FROM generate_series(1,1000) g;
ANALYZE pp;
SET parallel_setup_cost = 0; SET parallel_tuple_cost = 0;
SET min_parallel_table_scan_size = 0; SET max_parallel_workers_per_gather = 3;
BEGIN;
INSERT INTO pp_c SELECT 2500000+g, g FROM generate_series(1,10) g;
SELECT count(*) FROM pp;       -- 2001000 (expected 2001010)
SELECT count(*) FROM pp_c;     -- 1010, right
EXPLAIN (COSTS OFF) SELECT count(*) FROM pp;
-- Gather > Partial Aggregate > Parallel Append > Seq Scan on pp_c, Parallel Seq Scan on pp_h1, pp_h2
COMMIT;
```

The same happens on a single table with `SET debug_parallel_query = on` and
`SET kingres_columnar.enable_custom_scan = off` (Gather over `Seq Scan`, count
100 instead of 110). Reproduced on 18.7 and 19rc2.

Suggested fix: in the hook set `parallel_safe = false` on every path left in
`rel->pathlist` of a columnar relation (the hook already empties the partial
list); the paths of the parent `Append` are built afterwards, so the Parallel
Append is no longer offered. As a safety net make `kc_scan_begin` raise an
error when `IsParallelWorker()`.

### C1-06 high: reading buffered rows after a type-changing rewrite crashes the server

Component: `kc_write.c`, `kc_scan.c` (`kc_next_pending`).

The same stale buffer as C1-02, seen from the reader's side: the buffered rows
hold datums of the old column type, and the scan stores them into a slot of the
new descriptor. If the new type is by reference (text) the old by-value datum
(an integer) is used as a pointer.

```
CREATE TABLE r2 (id int, f int) USING columnar;
INSERT INTO r2 VALUES (1,1);
BEGIN;
INSERT INTO r2 VALUES (2,2);
ALTER TABLE r2 ALTER COLUMN f TYPE text USING f::text;
SELECT * FROM r2;           -- server process terminated by signal 11
```

What happens: segmentation fault of the backend, crash recovery of the whole
server (all sessions are dropped). No data is lost. The statement needs only
ownership of a columnar table. The opposite direction (text to int4) reads a
pointer as an integer and returns garbage. Reproduced on 18.7 and 19rc2. The fix
for C1-02 removes it; independent of that, `kc_next_pending` should compare the
buffer's per-column type signature with the descriptor before storing a datum.

### C1-07 high: a damaged value inside a chunk crashes the server

Component: `kc_encode.c` `kc_decode_chunk` (the by-reference branches, lines 768
to 838).

The decoder validates every length, count and codec of a chunk, as the README
says, but it hands the bytes of each variable-length value to the type's own
code as a varlena without any check of what is inside. For a type with internal
structure (arrays, ranges, numeric, jsonb, tsvector, geometric types, ...) a
damaged byte reaches `array_out`, `numeric_out` and so on, which trust it. The
README states "a hostile or damaged file can give a clean error but not always
the right data"; it can also crash the server. Page checksums (on by default
since PostgreSQL 18) catch random damage; a file that was damaged with checksums
off, restored from an unverified copy, or built on purpose is not caught.

Steps (checksums off, shown for `int[]`):

```
initdb --no-data-checksums ...  (shared_preload_libraries = 'kingres_columnar')
CREATE TABLE ta (id int, arr int[]) USING columnar;
SELECT kingres.columnar_set_options('ta', 'none');
INSERT INTO ta VALUES (1,'{1,2,3}'), (2,'{4,5,6}');  CHECKPOINT;
-- stop the server; in the relation file find the 32 bytes
--   01000000 00000000 17000000 03000000 01000000 01000000 02000000 03000000
-- (ndim 1, dataoffset 0, element type 23, dimension 3, lower bound 1, values 1 2 3)
-- and overwrite bytes 4 to 7 (the data offset) with f0ffff7f; start the server
SELECT * FROM ta;           -- server process terminated by signal 11
```

What happens: segmentation fault in `array_out`. In the random mutation runs
the same crash appeared twice in 780 iterations (once without, once with the
sanitizer build), both times a single flipped byte inside an `int[]` chunk of a
table with codec none; compressed chunks turn the same damage into "chunk does
not decompress to its recorded size". Reproduced on 18.7 and 19rc2.

What should happen: a data-corrupted error (SQLSTATE XX001).

Suggested fix: a per-chunk CRC-32C (the footer already has one) turns random
damage into a clean error at the price of a few bytes per chunk and one pass
over the data; it does not help against a hostile file. Against that the only
complete answer is to say plainly, in the README, that the files must be
trusted as much as heap files are, or to decode through each type's binary
receive function (`typreceive`), which validates, at some cost in speed. I would
add the CRC and correct the README now.

### C1-08 medium: SERIALIZABLE transactions on unrelated columnar tables abort each other

Component: `kc_catalog.c` (`kc_catalog_load`, `kc_catalog_insert`), `kc_scan.c`.

A scan takes no relation-level predicate lock. Reading the stripe list scans the
stripe catalog through its primary key index, and Serializable Snapshot
Isolation records page-level locks on that index; a flush inserts a catalog row
and conflicts with every reader of the same index page. All columnar tables
share the catalog, so for the small catalogs of a normal installation any two
transactions that read one columnar table each and write another conflict.

```
CREATE TABLE a1 (a int) USING columnar; CREATE TABLE b1 (a int) USING columnar;
CREATE TABLE a2 (a int) USING columnar; CREATE TABLE b2 (a int) USING columnar;
INSERT INTO a1 VALUES (1); INSERT INTO a2 VALUES (1);
-- session 1: BEGIN ISOLATION LEVEL SERIALIZABLE; SELECT count(*) FROM a1;
-- session 2: BEGIN ISOLATION LEVEL SERIALIZABLE; SELECT count(*) FROM a2;
-- session 1: INSERT INTO b1 VALUES (1);
-- session 2: INSERT INTO b2 VALUES (2); COMMIT;
-- session 1: COMMIT;
-- ERROR:  could not serialize access due to read/write dependencies among transactions
```

The tables share nothing; the heap equivalent commits both. The effect is an
accidental protection (true dependencies are detected, I checked) at the price
of false failures. Reproduced on 18.7 and 19rc2.

Suggested fix: take `PredicateLockRelation` on the columnar table at scan start
and `CheckForSerializableConflictIn(rel, NULL, InvalidBlockNumber)` at flush,
and keep the catalog accesses out of predicate locking. The README should
state what isolation level the access method supports.

### C1-09 medium: CREATE SCHEMA bypasses the DDL event trigger and breaks every later DDL

Component: `kingres_columnar--0.1.sql` (`columnar_check_ddl`, the event trigger
`WHEN TAG IN ('CREATE TABLE', 'ALTER TABLE', 'CREATE TRIGGER')`).

The event trigger does not fire for the commands inside `CREATE SCHEMA ...
CREATE TABLE ...`. So a foreign key on a columnar table, and an `AFTER ROW`
trigger on one, can be created that way. Worse, the trigger function does not
look at the command that fired it: it raises an error if any foreign key or
any row-level `AFTER` trigger on a columnar table exists anywhere in the
database. The state left by the bypass then makes every `CREATE TABLE`, `ALTER
TABLE` and `CREATE TRIGGER` of every user (the superuser included) fail until
someone drops the offending table.

```
-- as a role with CREATE on the database
CREATE SCHEMA sx
  CREATE TABLE pk1 (a int PRIMARY KEY)
  CREATE TABLE cfk (a int REFERENCES pk1) USING columnar;
-- as any other role that may create tables
CREATE TABLE victim (a int);
-- ERROR:  foreign keys on or to columnar tables are not supported
-- CONTEXT: PL/pgSQL function columnar_check_ddl() line 8 at RAISE
```

The columnar table with the foreign key is unusable (an insert fails with
"columnar tables do not support fetching a row by its TID"), so the damage is
the denial of DDL for the others, not wrong data. The same route creates an
`AFTER` row trigger (`CREATE SCHEMA sy CREATE TABLE t (...) USING columnar
CREATE TRIGGER ...`); inserting then fails cleanly. Reproduced on 18.7 and
19rc2.

Suggested fix: check only the objects named by `pg_event_trigger_ddl_commands()`
for the command that fired, add `CREATE SCHEMA` to the tag list, and refuse
the two cases also at the point where they would be used (the access method
already does, with the TID message).

### C1-10 medium: publications on columnar tables are accepted and silently empty

Component: design (no decoding support), no guard.

The README says logical decoding is not supported and not to publish a columnar
table. Nothing enforces it. With `wal_level = logical`:

```
CREATE TABLE lc (a int) USING columnar;  CREATE TABLE lh (a int);
CREATE PUBLICATION p FOR TABLE lc, lh;                      -- accepted, no warning
SELECT pg_create_logical_replication_slot('s','pgoutput');
INSERT INTO lc SELECT generate_series(1,10);  INSERT INTO lh SELECT generate_series(1,10);
-- decode with pgoutput: 10 Insert messages, all for lh; none for lc
```

A `FOR ALL TABLES` publication behaves the same. A subscriber receives the heap
rows and none of the columnar rows, without an error on either side. Reproduced
on 18.7 (I did not repeat it on 19rc2).

Suggested fix: refuse `CREATE PUBLICATION ... FOR TABLE` and `ALTER PUBLICATION
... ADD TABLE` for a columnar table from an event trigger, and warn on `FOR ALL
TABLES` (or `FOR TABLES IN SCHEMA`) when columnar tables exist.

### C1-11 low: TAP test 002_standby fails on 19rc2

`t/002_standby.pl` line 265 compares the file size of the table on the standby
with the primary's right after `wait_for_catchup`, which waits for the primary's
write position (`pg_current_wal_lsn()`). After a rolled-back transaction the
last records are not flushed: in my run the write position was 0/039B0000 and
the insert position 0/039B0338, the standby reported 220 blocks and, 0.3 s
later, the primary's 221. The test failed in 2 of 2 runs on 19rc2 and passed in
3 of 3 on 18.7; I did not find a storage problem behind it. The fix is for the
test to wait for the primary's insert position (or to run `pg_switch_wal()` or a
commit first).

### C1-12 low: columnar_types needs libxml

`sql/columnar_types.sql` inserts values of type `xml`. On a server built without
libxml the file fails with "unsupported XML feature" and, because `ext.sh` stops
at the first failing stage, the TAP tests do not run. The test should skip the xml columns when the
server was built without libxml (for example by probing with `SELECT
'<a/>'::xml` in a `DO` block). It passed on my 19rc2 installation, which has
libxml.

### C1-13 note: age(relfrozenxid) of a columnar table is the maximum

`vac_update_relstats` is called with an invalid `relfrozenxid`, and the relation
is created with the same value, so `age(relfrozenxid)` is 2147483647 for every
columnar table. The stripe catalog, an ordinary heap table, is vacuumed and
frozen by autovacuum (checked, see the table above), and `datfrozenxid` is not
held back; but monitoring that alerts on table age will alert on every columnar
table, and `VACUUM (FREEZE)` on one does nothing visible. Worth a sentence in
the README.

### C1-14 note: the schema name equals the default role name

The bootstrap superuser of a KingresDB cluster initialised by the OS user
`kingres` is the role `kingres`, and the extension's schema is `kingres`. With
the default search path (`"$user", public`) every unqualified `CREATE TABLE`
of that role lands in the extension's schema (my first test tables did). That
is not a defect of the extension code, but such a table depends on the
extension's schema (by reading the dependency rules, `DROP EXTENSION
kingres_columnar CASCADE` would take it along; I did not try), and the schema is
shared with other extensions. The review of `kingres_sketch` and
`kingres_timepart` (K12-17) met the same effect.

## Checked and found correct

The following were attacked and held; they are listed so that the absence of a
finding is not read as absence of effort.

- Durability and recovery: committed batches survived every kill (postmaster
  and single backend, `full_page_writes` on and off, 2PC prepared and committed
  at random moments, standby promotion with synchronous replication, truncation,
  tablespace moves, type changes, compaction, `VACUUM FULL`, the access-method
  round trip). `columnar_check` was clean (except orphan catalog rows, which are
  documented) and the table writable after each recovery.
- A stripe's pages are logged before its catalog row, the metapage is logged as
  a full image, and the redo function only restores images; I found no ordering
  or masking problem, and `wal_consistency_checking` for the resource manager
  was silent on primary and standby.
- Tail truncation by `VACUUM` honours older snapshots (it uses
  `HeapTupleSatisfiesVacuum` with the removable horizon over the catalog) and
  concurrent writers (the lock is conditional and `ACCESS EXCLUSIVE`).
- Wraparound: the stripe catalog is frozen like any heap table; nothing in the
  extension compares transaction ids except `TransactionIdIsCurrentTransactionId`.
- Visibility inside a transaction: the command-id rule for flushed and buffered
  rows, savepoints with rollback and release at three levels, `INSERT ... SELECT`
  from the same table, a statement that fills several stripes, cursors opened
  before an insert. The one failure is C1-04.
- Skipping: all 121 predicates agreed with heap, including cross-type
  comparisons, `COLLATE` mismatch, NaN and NULL handling and parameterised
  rescans; the one failure is C1-03 (stale statistics, not a logic error of the
  comparison).
- Refusals are clean and use SQLSTATE 0A000; every route to an index I tried is
  closed. The only bypass is C1-09.
- Privileges: owner and `SELECT` checks are enforced; the event trigger
  functions are `SECURITY DEFINER` with a fixed search path and no dynamic SQL;
  nobody but the owner can read the stripe catalog; `columnar_reconcile` cannot
  delete a row that a live relation uses (it reads `pg_class` with a dirty
  snapshot).
- Damaged files: 780 mutation runs and 74 catalog mutations produced clean
  errors with SQLSTATE XX001 on every path (scan, custom scan, `COPY`, `ANALYZE`,
  `VACUUM`, compact, check, the info functions), except C1-07.

## Not reproduced

- A stripe whose chunk header claims a large raw size: `kc_decode_chunk`
  allocates `raw_len` bytes (up to 4 GB) with `MemoryContextAllocHuge` before it
  decompresses, and for a variable-length column the size is bounded only by
  the codec's expansion limit (zstd: the frame content size, which the file
  controls). A crafted zstd frame with a small compressed size and a large
  content size could force a multi-gigabyte allocation per chunk. I did not
  build such a frame; the mutation runs never produced one.
- The minimum and maximum of a stripe are validated for length only. A
  damaged numeric or other structured value in a statistic is passed to the
  type's comparison function; this is the C1-07 class and I did not exercise
  it separately.
- `kc_subxact_callback` removes an aborted subtransaction's buffer from the list
  but does not free the `KcWriteBuf` structure itself (kc_write.c line 1033 to
  1037). The leak is bounded by the transaction (the memory context is deleted at
  commit or abort) but grows with the number of aborted subtransactions that
  buffered rows in one transaction. I did not measure it.
- Row numbers above about 8.8 x 10^12 wrap the 32-bit block number of the
  synthetic TID (`kc_store_group_row`) although `kc_meta_allocate` allows up to
  2^63 rows. Unreachable in practice.
- Skipping with a user-defined non-strict operator in the column type's default
  btree family: `KS_CMP` treats a NULL expression as "skip". Operators of
  builtin families are strict; I did not create a counter-example.

## Not covered

- Logical replication apply into a columnar table (needs two clusters), and
  `pg_rewind`, archive recovery from a base backup with a restored `pg_wal`.
- A relation of more than one segment file (1 GB): the largest table I loaded
  was 781 MB.
- A hot standby long query against a truncation by `VACUUM`: the TAP test covers
  it and passes on 18.7; I did not repeat it. A standby promotion and the
  truncate-and-DDL crash harness were run on 18.7 only.
- Row-level security and the leak of statistics through `EXPLAIN (ANALYZE)`
  skip counters; triggers other than the refusals; generated columns and
  identity only at the level of the table of experiments above.
- Many writers into one table beyond the existing `004_concurrent` test, a
  backend killed while it holds the relation extension lock (the lock is
  released with the backend; I did not test the window), deadlock analysis.
- `valgrind` (not installed). The `xid_wraparound` test module (not built); I
  simulated wraparound only with 160 000 transactions and aggressive
  autovacuum, and no real wrap occurred.
- README claims no test or experiment of mine covers: "a page of a stripe
  stands for the rows at the same relative position" (the quality of
  `ANALYZE` statistics on sorted data), the exact autovacuum counter reset after
  `VACUUM`, and the portability statements.
