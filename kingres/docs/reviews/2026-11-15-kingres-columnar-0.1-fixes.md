<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
# Adversarial review of the fix round: contrib/kingres_columnar 0.1 (commit 5555b252a0)

Reviewer: independent (not the author). Reviewed state: commit 5555b252a0,
exported with `git archive` (paths `contrib/kingres_columnar` and
`kingres/dev`) into a private directory (`~/kw-review9/src`). The working tree
was not used for any result. Line numbers are those of that commit. Nothing in
the repository was changed except this file. The change under review answers
`kingres/docs/reviews/2026-11-15-kingres-columnar-0.1.md` (findings C1-01 to
C1-14).

## Summary

The fixes hold for what they were written against: eleven of the twelve
reproductions of the first review (C1-01 to C1-12) now give the right result on
PostgreSQL 18.7 and on 19rc2 (C1-10, the publications, is closed only in part,
C2-03), the new identity-keyed stripe catalog survived every attempt to
make two relations share an identity or one relation lose its own, the
hand-written catalog visibility check matches `HeapTupleSatisfiesMVCC` in every
case I could construct, and no committed row was lost or doubled in the durability
campaign (kills of the postmaster and of single backends during loads and during
every rewriting command, synchronous standby promotion, `wal_level = minimal`,
`wal_consistency_checking` on primary and standby). The defects are in what the
fixes did not look at: the isolation level SERIALIZABLE, the memory a write
buffer keeps per subtransaction, and one remaining way into a publication.

| Severity | Count |
|---|---|
| critical (wrong answer or committed data wrong, without an error) | 1 |
| high (crash or unbounded resource use reachable by a user or a damaged file) | 1 |
| medium | 2 |
| low | 3 |
| note | 2 |

| Id | Severity | Component | Statement |
|---|---|---|---|
| C2-01 | critical | `kc_write.c` (`kc_flush_buf`), `kc_scan.c` | Under SERIALIZABLE a reader does not get a conflict with a concurrent writer whose rows it cannot see; classic write skew commits on both transactions (heap aborts one). |
| C2-02 | high | `kc_write.c` (`kc_subxact_callback`) | Every committed subtransaction that inserted a row leaks its write buffer until the transaction ends (12 to 20 kB each, so a loop of 100 000 `BEGIN ... EXCEPTION` blocks takes 1.2 to 2 GB); with a cursor open on the table the leak is quadratic (30 000 subtransactions take 4 GB). |
| C2-03 | medium | `kingres_columnar--0.1.sql` (`columnar_check_ddl`) | A publication with `publish_via_partition_root = true` that lists the root of a partitioned table with a columnar partition is accepted; the columnar rows are silently not replicated (the case C1-10 was meant to close). |
| C2-04 | low | `kc_catalog.c` (`kc_tuple_visible`) | `columnar_compact` and a reader in one statement: the reader sees 0 rows (heap semantics give all of them). |
| C2-05 | low | `kc_storage.c` (`kc_check_snapshot_age`) | A READ COMMITTED statement that reads a table through a STABLE function fails with 40001 after a `VACUUM FULL` that heap answers; the README promises the error only to REPEATABLE READ and SERIALIZABLE. |
| C2-06 | medium | `kc_storage.c` (`kc_check_snapshot_age`), `KcMetaData.created_xid` | The creation xid in the metapage is never frozen; after one lap of the 32-bit id space (4.29 billion transactions) any open transaction whose id equals it makes every read of the table fail with 40001 ("rewritten or truncated") for as long as it runs. |
| C2-07 | low | `kc_am.c`, README | A columnar table with a row over 8 kB cannot be converted back with `ALTER TABLE ... SET ACCESS METHOD heap` ("row is too big"): the table has no TOAST table and the rewrite does not create one. |
| C2-08 | note | `kc_encode.c` | A jsonb chunk damaged with its checksums recomputed crashes the backend (documented class; measured: 15 of 4 179 hostile runs). |
| C2-09 | note | `kc_catalog.c` | The identity is 63 random bits with no collision check; every `DROP TABLE`, `VACUUM` of a columnar table and `columnar_check` read the metapage of every columnar relation of the database. |

## Scope and method

Read completely: `kc_catalog.c`, `kc_storage.c`, `kc_write.c`, `kc_scan.c`,
`kc_am.c`, `kc_vacuum.c`, `kc_cscan.c`, `kingres_columnar.c`,
`kingres_columnar--0.1.sql`, the decode half of `kc_encode.c`, the README and the
commit message; `kc_skip.c` only where it reads statistics. The diff of the
commit was used to find what changed; every conclusion below comes from running
the code.

Built and run on PostgreSQL 18.7 and 19rc2, both with assertions enabled
(`USE_ASSERT_CHECKING`), each copied into my own area (`pg_config` reports paths
inside it) with `kingres/dev/ext.sh kingres_columnar sync clean build install
regress tap` and `EXT_WERROR=1`: no warning, 11 regression files pass on both
majors, and the 8 TAP files (993 tests) pass on both majors. The 18.7 build has
no libxml, the 19rc2 build has; both have lz4 and zstd. The machine was shared
(load average 2 to 6); everything ran under `nice`, at most `-j4`, on ports and
directories of my own.

My own scripts are PostgreSQL TAP scripts (`PostgreSQL::Test::Cluster`) in a
private directory, one per question; the table gives what each did.

| Experiment | What was run |
|---|---|
| Old snapshots (item 2) | 12 commands (`VACUUM FULL`, `SET TABLESPACE`, `SET ACCESS METHOD` round trip, `TRUNCATE` and reload, `ALTER COLUMN TYPE`, `columnar_compact` + `VACUUM`, plain `VACUUM`, insert, and four rolled-back or subtransaction variants) against a REPEATABLE READ and a SERIALIZABLE snapshot taken before the command, read afterwards, each compared with a heap twin (18.7). READ COMMITTED with a STABLE function that sleeps, then reads, with five commands (18.7). A hot standby (streaming, `hot_standby_feedback = on`) with the same commands at REPEATABLE READ and READ COMMITTED, a query holding the lock, promotion (both majors). Prepared rewriting transaction. |
| Write buffers (item 4) | 26 hand-written statement sequences run on a columnar table and a heap twin, every query and every error compared: savepoint depth to 3, `ROLLBACK TO`, `RELEASE`, `TRUNCATE`, `ALTER COLUMN TYPE` twice, `SET ACCESS METHOD` round trip, `ADD COLUMN` with a volatile default, `DROP COLUMN`, `DECLARE ... CURSOR` (also WITH HOLD) before and after each of these, `PREPARE TRANSACTION` with pending rows and after a rewrite, `INSERT ... SELECT` from itself in a subtransaction, a flush forced by `stripe_row_count` in a nested subtransaction, 600 000 rows across a rewrite, with `write_buffer_memory = 1MB`. Both majors. Memory: RSS of the backend measured for 500 nested savepoints, 300 partitions, and 20 000 to 100 000 `BEGIN ... EXCEPTION` blocks, with and without an open cursor, columnar and heap. |
| Identity (item 1) | `CREATE DATABASE ... TEMPLATE` with both strategies (identity, reads, writes, `VACUUM`, `VACUUM FULL`, compact, `SET TABLESPACE`, `TRUNCATE`, `DROP` in the clone; the source checked afterwards), `LIKE`, `CREATE TABLE AS`, `SET TABLESPACE` with `TRUNCATE` or a rewrite in the same transaction (commit and rollback), rolled back `SET TABLESPACE`, `TRUNCATE` and rewrite, partition attach and detach, orphan rows after `DROP`. Both majors (18.7 only for the standalone orphan counts). |
| Publications (item 6) | 40 statements: `FOR ALL TABLES`, `TABLES IN SCHEMA`, `ALTER TABLE ... SET SCHEMA`, partitions created, attached, `publish_via_partition_root`, `ALTER PUBLICATION ADD/SET`, inheritance, tables converted afterwards, a grandfathered `FOR ALL TABLES` publication (event trigger disabled while it was made), `pg_dump` and `pg_restore` of that state, `test_decoding` of a transaction that writes both kinds of table. Both majors. |
| Parallel safety (item 7) | A partitioned table with two heap and one columnar partition and one with one heap and two columnar partitions, a heap table and a columnar table, all analyzed, with `parallel_setup_cost = 0`, `parallel_tuple_cost = 0`, `min_parallel_table_scan_size = 0`, partitionwise join and aggregate on, and the transaction holding unflushed rows in three columnar relations: 15 queries (counts, sums, partitionwise self join, grouped aggregate, hash join of a heap and a columnar table, `IN`, `EXISTS`, scalar and correlated sub-selects, `UNION ALL` of heap and columnar, a materialized CTE, a join of a columnar partition with a heap table) with `debug_parallel_query` off and on, each result compared with a serial run with the custom scan off: 60 comparisons on the two majors, all equal, never a columnar scan below a Gather. PARALLEL SAFE SQL functions that read the table, called from a qual and from the target list under a Gather, `CREATE INDEX` on another table with such an expression and parallel maintenance workers, `CREATE TABLE AS`, `VACUUM (PARALLEL 3)`. |
| Serializable | The case of the README (read, then write) and the opposite order (write, then read), one table and two tables, columnar and heap in every combination. Both majors. |
| Hostile files (item 5) | Data checksums off. Two tables of 300 rows with ten columns of ten types (`int`, `text`, `int[]`, `numeric`, `jsonb`, `timestamptz`, `bytea`, `text[]`, `float8`, `varchar`, with nulls), codec none and zstd, chunk groups of 100 rows, and a third table of 50 rows; the files of the stopped server mutated one field at a time and the server started on each variant. Metapage: all ten fields, 7 to 8 values each (0, 1, 2, 2^31 - 1, 2^31, 2^32 - 1, 65536, ...), with the metapage CRC recomputed and not. Stripe header: all ten fields. Footer: all eleven fields of the chunk entries of groups 0 and 1 for every column, and the row count of each group, the footer CRC recomputed. Chunk data: 30 random multi-byte flips per column (half with the chunk and footer CRCs recomputed), 4-byte words set to extreme values at 15 offsets of six columns (CRCs recomputed). Page header, special area, zeroed pages, truncation to 0, 1, 2 blocks, three zero blocks appended, a file replaced by another table's file (the identity claimed twice), a file with other columns. After each variant one session ran: whole-row text of a sequential scan, the custom scan with five filter shapes, a sorted read, `ANALYZE`, `columnar_info`, `columnar_stripes`, `columnar_options`, `COPY ... TO`, `columnar_check`, `VACUUM`, `columnar_compact`, `columnar_reconcile`, `VACUUM FULL`, each as its own statement with a 20 s timeout, then the log was searched for signals, assertion failures and sanitizer reports. 4 179 variants on 18.7 (full), 1 755 on 18.7 and 1 755 on 19rc2 (short form: three footer columns, fewer flips). Catalog: each numeric column of the stripe row of both tables set to 8 values (0, -1, maximum, plausible and wrong) and `cid_points` and `col_stats` replaced by six malformed values, 158 mutations on each major, nine statements after each. Sanitizer: the extension built with `-fsanitize=address,undefined -fno-sanitize-recover=undefined` and loaded into the 18.7 server with the ASan runtime preloaded : 1 755 variants of the short form and the 158 catalog mutations; three AddressSanitizer reports, all inside the server's jsonb and json code (`jsonb_cmp`, `escape_json`) on forged jsonb chunks, none with a frame in the extension, no UndefinedBehaviorSanitizer report. AddressSanitizer does not instrument `palloc`, so it sees only the `malloc`-sized chunks, the stack and globals; UndefinedBehaviorSanitizer sees the whole extension. |
| Durability | A columnar table `c` and a heap twin `h` receive identical rows in the same transaction (plain, with a savepoint and a rollback to it, with `PREPARE TRANSACTION` and `COMMIT PREPARED`, with two type changes in the transaction) from 8 `pgbench` clients while other clients run, concurrently, `columnar_compact`, `VACUUM`, `CHECKPOINT`, `VACUUM FULL`, a `SET ACCESS METHOD` round trip, `SET TABLESPACE` both ways, `ALTER COLUMN TYPE` both ways, and transactions that `TRUNCATE` both tables and reload them, or insert, change the type and insert again. After `kill -9` of the postmaster at a random time (1 to 6 s) or of one random server process (including the I/O workers of 18), restart, commit the prepared transactions, and require: row counts equal, no row in only one table (`EXCEPT ALL` both ways), `columnar_check` clean, the table still columnar and writable. 8 rounds of each kind on each major (the backend kills with `full_page_writes = off`, the postmaster kills with it on). A synchronous streaming standby with `wal_consistency_checking = 'KingresColumnar'` on both nodes, the client recording every batch it saw committed, `kill -9` of the primary, promotion, then a new standby from the new primary: 5 rounds on each major (22 to 184 acknowledged batches per round). `wal_level = minimal` (create, fill, `TRUNCATE`, rewrite, `VACUUM FULL` in one transaction, then an immediate stop) with `wal_skip_threshold` at 0, 64 kB and 1 GB: both majors. Hot standby and promotion in the old-snapshot script, also with `wal_consistency_checking`. Base backup under load. |
| Real wrap | The `xid_wraparound` module built against both installations. 18.7: a table created at transaction id 754, 46 batches of `consume_xids(100000000)` with autovacuum running (4.6 billion ids, one wrap), after each batch a REPEATABLE READ read and an insert; then on to the second lap (8.59 billion ids in total) and an open transaction whose id equals the creation id (C2-06). |

## Earlier findings

| Id | Result | How it was checked |
|---|---|---|
| C1-01 | fixed | Old REPEATABLE READ and SERIALIZABLE snapshots against `VACUUM FULL`, `SET ACCESS METHOD` round trip, `TRUNCATE` and reload, `ALTER COLUMN TYPE`: error 40001 naming the rewrite, never an empty table (18.7, 24 combinations; standby on both majors). `SET TABLESPACE`, `columnar_compact`, `VACUUM`, a plain insert, a rolled back rewrite, a rolled back `TRUNCATE` and a rewrite rolled back with `ROLLBACK TO` keep the old snapshot working and give the heap answer. |
| C1-02 | fixed | The first review's statements (type change, volatile default, access method round trip, rewrite after a savepoint) give 2 rows, committed; 26 differential sequences equal to heap on both majors. |
| C1-03 | fixed | `int4` to `oid`: 5 and 1 rows with the custom scan forced, as heap. |
| C1-04 | fixed | Cursor opened before `RELEASE`: 1 and 2. Cursor opened in the subtransaction, `WITH HOLD`, `ROLLBACK TO`, several released subtransactions: equal to heap. |
| C1-05 | fixed | Parallel Append over a columnar partition and the single-table `debug_parallel_query` case: 2 001 010 and 110 rows, no Gather over the columnar child. The 60 parallel-versus-serial comparisons of the parallel script (both majors, also with unflushed rows in the columnar relations) are all equal. |
| C1-06 | fixed | Type change to `text` and back with buffered rows: 2 rows, no crash. |
| C1-07 | fixed (jsonb and similar types remain, as documented) | Array chunks (`int[]`, `text[]`) mutated with a recomputed chunk CRC and footer CRC (4-byte words set to 0, 1, 2^31, 2^32-1 and others at 15 offsets, 30 random byte flips per column, half of them with the CRCs recomputed): errors of the form "columnar table is corrupt: stripe N: array data offset is wrong / array element length is wrong / array has an impossible number of dimensions", no crash, on 18.7 (4 179 runs) and 19rc2. The only type that still crashes is jsonb (C2-08), which the README documents. |
| C1-08 | fixed, but see C2-01 | Unrelated tables no longer abort each other; the README's read-then-write case aborts one transaction. The opposite order is not detected (C2-01). |
| C1-09 | fixed | `CREATE SCHEMA ... CREATE TABLE ... REFERENCES` and `... CREATE TRIGGER ... AFTER` are refused; later DDL of other tables (also `ALTER TABLE`) works. |
| C1-10 | partly fixed | Refused: `FOR TABLE`, `FOR ALL TABLES`, `TABLES IN SCHEMA`, creating or attaching a columnar table into such a scope, `ALTER PUBLICATION ADD/SET`, `ALTER TABLE ... SET SCHEMA`, `SET ACCESS METHOD`, `CREATE SCHEMA` with a table. Not refused: `publish_via_partition_root = true` (C2-03). |
| C1-11 | fixed | `002_standby` waits for the insert position and passes on both majors. |
| C1-12 | fixed | `columnar_types` no longer uses xml; it passes on the 18.7 build without libxml. |
| C1-13 | documented | `relfrozenxid` is still 0 and `age()` still the maximum; the README now says so and explains why autovacuum is not affected. Autovacuum was checked: a columnar table is vacuumed and analyzed once after its inserts, does not repeat, and logs no error. |
| C1-14 | documented | README paragraph; the effect is unchanged (a table of the role `kingres` lands in schema `kingres`). My own first tables did. |

The suspicions of the first review: decompression is now bounded by the recorded
size (checked in the code and by the mutation run); a write buffer of an aborted
subtransaction is freed (30 000 aborted subtransactions with one 100-column row
each: backend 15 to 21 MB, flat); the stale `KcWriteBuf` of a merged
subtransaction is not (C2-02).

## Findings

### C2-01 critical: SERIALIZABLE does not detect a write the reader cannot see

Component: `kc_write.c` (`kc_write_row_values` line 414, `kc_flush_buf` line 758),
`kc_scan.c` (`kc_scan_begin` line 110).

The README says that under SERIALIZABLE "a scan takes a relation-level predicate
lock and an insert checks for conflicts on the relation, so a real write skew on
one columnar table is detected". That holds when the reader reads first and the
writer writes afterwards (the insert finds the reader's SIREAD lock). It does
not hold the other way round. A heap reader that meets a tuple of a concurrent
transaction calls `CheckForSerializableConflictOut` and so records the
dependency "reader before writer". A columnar writer keeps its rows in a private
buffer until pre-commit, its only conflict check (`CheckForSerializableConflictIn`)
ran when the row was buffered, the reader's SIREAD lock did not exist yet, and
the flush at commit does not check again. The reader never sees a stripe, a
catalog row or any other trace of the writer, and the scan never calls
`CheckForSerializableConflictOut`. The dependency is lost, and a cycle of two
transactions is not detected.

Steps (two sessions, `c` columnar, `d` heap or columnar):

```
CREATE TABLE c (a int) USING columnar;  CREATE TABLE d (a int);
-- session 1: BEGIN ISOLATION LEVEL SERIALIZABLE;  SELECT 1;
-- session 2: BEGIN ISOLATION LEVEL SERIALIZABLE;  SELECT 1;
-- session 1: INSERT INTO c VALUES (1);
-- session 2: SELECT count(*) FROM c;      -- 0
-- session 2: INSERT INTO d VALUES (1);
-- session 1: SELECT count(*) FROM d;      -- 0
-- session 1: COMMIT;
-- session 2: COMMIT;
```

What happens: both commit, with no error, on 18.7 and on 19rc2, whether `d` is
heap or columnar, and also when `c` is heap and only `d` is columnar. Session 2
read `c` without session 1's row (so session 2 is before session 1) and session 1
read `d` without session 2's row (so session 1 is before session 2): no serial
order exists. With `c` and `d` both heap, the commit of session 2 fails with
`could not serialize access due to read/write dependencies among transactions`.
A single table does the same: two transactions each insert a row into `c`, each
then counts the rows of `c` (each sees 1), both commit; heap aborts the second
commit.

What should happen: one of the two transactions is aborted, as for heap.

Suggested fix: (1) call `CheckForSerializableConflictIn(rel, NULL,
InvalidBlockNumber)` again in `kc_flush_buf` (a reader that arrived after the
row was buffered but before the flush then finds its lock held against the
writer); (2) in the scan, for every catalog row that is invisible because its
inserter is in the snapshot or still running, call
`CheckForSerializableConflictOut(rel, xmin, snapshot)` (the case of a stripe
already flushed by a size-forced flush). The test in `t/007_snapshots.pl` should
get the opposite order.

### C2-02 high: a committed subtransaction leaks its write buffer; quadratic with an open cursor

Component: `kc_write.c`, `kc_subxact_callback` (lines 1008 to 1079).

When a subtransaction that inserted into a columnar table commits, its buffer is
merged into the parent's: the rows are copied, `b->nrows` is set to 0, the
buffer's memory context is re-parented under the parent's context, and nothing is
freed. The `KcWriteBuf`, its context and the arrays of 1024 rows (`kc_buf_reserve`
allocates at least 1024 rows) stay allocated until the transaction ends. Every
commit of a subtransaction also builds a new `keep` list of all buffers and
assigns it to `st->bufs` without freeing the old one. While a scan has pinned
the relation (an open cursor over a table that has pending rows) buffers are not
merged at all, so the list grows by one buffer per subtransaction and the rebuild
makes the leak quadratic.

Steps (any role that owns a columnar table; `BEGIN ... EXCEPTION` in PL/pgSQL
opens a subtransaction, a loop like this is the usual shape of row-by-row ETL):

```
CREATE TABLE cn (a int) USING columnar;
BEGIN;
DO $$ BEGIN FOR i IN 1..100000 LOOP
  BEGIN INSERT INTO cn VALUES (i); EXCEPTION WHEN OTHERS THEN NULL; END;
END LOOP; END $$;
-- backend resident size: 1219 MB (heap table: 26 MB)
```

Measured on 18.7 and on 19rc2 (identical): 30 000 blocks, 378 MB (heap 21 MB);
100 000 blocks, 1219 MB; a table with four columns (int, text, float8,
timestamptz), 100 000 blocks, 2010 MB; a 100-column table, 20 000 blocks with an
open cursor, 2313 MB; a one-column table with an open cursor, 30 000 blocks,
4128 MB and 4 seconds (against 0.2 s without the cursor); 20 000 blocks with the
cursor, 1924 MB. `write_buffer_memory` does not limit it (the counter counts row
bytes only, and buffers of subtransactions cannot be flushed). A loop of a
million iterations needs 12 to 20 GB: a user who owns a columnar table can exhaust the
memory of the machine (the OOM killer ends a backend and the postmaster restarts
every session).
Aborted subtransactions are fine (30 000 with a 100-column row each: 15 to 21 MB, flat),
and so is a deep stack of 500 unreleased savepoints (the arrays are not touched,
so they do not become resident).

What should happen: memory proportional to the rows buffered, as for heap.

Suggested fix: on merge, copy the by-reference values into the parent's context
(or move them with `MemoryContextSetParent` and delete `b`'s arrays), then
`MemoryContextDelete(b->cxt)` and `pfree(b)`; `list_free` the old list (or edit
it in place with `foreach_delete_current`); start `kc_buf_reserve` at the size
of the first insert instead of 1024 rows; for the pinned case let a scan pin a
buffer by serial number and merge with an alias (the first review's suggestion
for C1-04), so that the list does not grow.

### C2-03 medium: a publication with publish_via_partition_root accepts a columnar partition

Component: `kingres_columnar--0.1.sql`, `columnar_check_ddl` (lines 202 to 213).

The publication side of the refusal asks `pg_get_publication_tables()` which
tables the publication contains and refuses when one is columnar. With
`publish_via_partition_root = true` that function returns the partitioned root,
whose access method is not columnar, so a publication that includes columnar
partitions through their root is accepted and the rows of those partitions are
silently not replicated (the table side is closed: creating or attaching a
columnar partition below a published root is refused, by the ancestors query).

```
CREATE TABLE par (k int, v int) PARTITION BY RANGE (k);
CREATE TABLE hp  PARTITION OF par FOR VALUES FROM (0)  TO (10);
CREATE TABLE cp  PARTITION OF par FOR VALUES FROM (10) TO (20) USING columnar;
CREATE PUBLICATION p FOR TABLE par WITH (publish_via_partition_root);   -- accepted
-- or: CREATE PUBLICATION q WITH (publish_via_partition_root) ...; ALTER PUBLICATION q ADD TABLE par;
-- on 19rc2 also: CREATE PUBLICATION r FOR TABLES IN SCHEMA public WITH (publish_via_partition_root);
```

With `wal_level = logical`, a slot and `pgoutput` for `p`, then `INSERT INTO par
SELECT g, g FROM generate_series(0,19) g`: 10 Insert messages decoded, the 10
rows of `cp` are missing, no error on either side. Without the option the
publication is refused. Reproduced on 18.7 (`FOR TABLE` and `ADD TABLE`) and on
19rc2 (also the schema form; on 18.7 the schema form is refused).

What should happen: refused, as without the option.

Suggested fix: in the publication branch, join each listed relation with
`pg_partition_tree()` and test the access method of the leaves.

### C2-04 low: compaction and a reader in one statement

Component: `kc_catalog.c`, `kc_tuple_visible` (line 233).

For the running transaction's own deletion the check returns "not visible" at
once, whatever the command, while the rows the same command wrote are hidden by
the command-id rule. A statement that compacts and reads the table in one query
therefore sees neither the old stripes nor the new one.

```
CREATE TABLE c (a int) USING columnar;
INSERT INTO c SELECT generate_series(1,1000);     -- three such inserts, three stripes
CREATE FUNCTION cnt() RETURNS bigint LANGUAGE sql STABLE AS 'SELECT count(*) FROM c';
SELECT kingres.columnar_compact('c', 100000), cnt();     -- 1 | 0   (3000 rows in the table)
```

Heap semantics (a row deleted by the current command is still seen by it) give
3000. The next statement sees 3000. Other shapes (an uncorrelated sub-select,
a lateral sub-select) evaluate before the compaction and give 3000. Wrong answer
without an error, but it needs `columnar_compact` and a reader of the same table
in one statement.

Suggested fix: for a deleter that is the current transaction, compare the
deleting command id with the scan's real command id (`HeapTupleHeaderGetCmax`)
instead of treating every own deletion as past.

### C2-05 low: READ COMMITTED statements fail with 40001 after VACUUM FULL

Component: `kc_storage.c`, `kc_check_snapshot_age`; README ("Older snapshots").

```
-- function slp(): STABLE, PL/pgSQL: PERFORM pg_sleep(2); RETURN (SELECT count(*) FROM c);
-- session A (READ COMMITTED): SELECT slp();
-- session B, while A sleeps: VACUUM FULL c;
-- session A: ERROR: could not read columnar table "c": it was rewritten or truncated after this snapshot was taken
```

The same with `SET ACCESS METHOD`, `ALTER COLUMN TYPE` and `TRUNCATE` (heap
returns 0 rows for these three, which is its own known exception; for `VACUUM FULL`
heap returns the 1000 rows). A READ COMMITTED statement keeps its snapshot for
the statement, and a STABLE or IMMUTABLE function (or a cached plan locked after
the snapshot, a statement that waits) opens the table after the command
committed. The README promises the error to REPEATABLE READ and SERIALIZABLE
only. It is an error, not a wrong answer, and a retry succeeds; the README should
say that it covers every isolation level.

### C2-06 medium: after 2^32 transactions a table can be unreadable while an unrelated transaction runs

Component: `kc_storage.c`, `kc_check_snapshot_age` (lines 606 to 627); `KcMetaData.created_xid`.

The identity records the 32-bit id of the transaction that created the file and
the metapage is never rewritten, so the id is never frozen. The age test ignores an
id older than 2^30 transactions, but after exactly one full lap of the id space
(4.29 billion transactions) the same 32-bit value is again "recent", and the
check `XidInMVCCSnapshot(created_xid, snapshot)` is true for every snapshot
taken while the transaction that now owns that id value is running. Every read
of the table then fails with SQLSTATE 40001 and the text "it was rewritten or
truncated after this snapshot was taken", for as long as that one transaction
lives (an idle-in-transaction session, a prepared transaction left behind, a
long batch job), and again once per lap. The table was never rewritten.

Steps (the `xid_wraparound` test module, built against the same installation;
this took about 4 minutes):

```
CREATE EXTENSION xid_wraparound;
CREATE TABLE c (a int, b text) USING columnar;  INSERT INTO c SELECT g, 'x'||g FROM generate_series(1,2000) g;
-- the creation xid of the file is in bytes 48..51 after the page header: here 754
-- SELECT consume_xids(100000000) in batches (autovacuum on, one `txid_current()` after each batch),
-- twice around the id space, then SELECT consume_xids_until(<xid8 whose low 32 bits are 750>);
-- three more `SELECT txid_current()`; then:
-- session A: BEGIN; SELECT pg_current_xact_id()::text::bigint % 4294967296;   -- 754
-- session B: SELECT count(*) FROM c;
-- ERROR:  could not read columnar table "c": it was rewritten or truncated after this snapshot was taken
```

The first lap is clean: 46 batches of 100 million ids, a REPEATABLE READ read
and an insert after each, including the batch that crosses the wrap, no error
(the age test works as designed there). While the transaction with id 754 is
open, both READ COMMITTED and REPEATABLE READ reads of `c` fail on 18.7; after it
ends the table reads again (2000 rows). The README rules out a wraparound
problem ("nothing in the extension compares transaction ids"); the comparison
was added by this fix round.

Suggested fix: record the 64-bit id (`FullTransactionId`, as `xid8`) in the
metapage and compare with `ReadNextFullTransactionId()` (a file older than 2^30
ids is ignored, with no wrap), converting to the 32-bit id only for the
snapshot test.

### C2-07 low: a columnar table with wide rows cannot be converted back to heap

Component: `kc_am.c` (`kc_relation_needs_toast_table` is false), README ("Back:
`ALTER TABLE t SET ACCESS METHOD heap`").

A columnar table has no TOAST table. PostgreSQL's table rewrite creates the new
table's TOAST table only when the old table has one, so converting such a table
to heap fails as soon as a row does not fit in a heap page.

```
CREATE TABLE w (id int, t text) USING columnar;
INSERT INTO w SELECT 1, (SELECT string_agg(md5(i::text), '') FROM generate_series(1,5000) i);   -- 160 kB
ALTER TABLE w SET ACCESS METHOD heap;
-- ERROR:  row is too big: size 160032, maximum size 8160
```

The same happens to a heap table that was converted to columnar and back (it
lost its TOAST table). `CREATE TABLE ... AS SELECT` into a heap table works, the
transaction aborts cleanly and nothing is lost. Reproduced on 18.7 and 19rc2.
The README should say that the way back for tables with rows over 8 kB is
`CREATE TABLE ... AS` (or the extension should create the TOAST table of the heap
target first).

### C2-08 note: a damaged jsonb chunk with valid checksums still crashes

Documented ("a file written on purpose, with valid checksums ... can still crash
the server through such a type, as with heap"); recorded here because the
campaign measured it. In the 4 179 hostile runs of the full campaign on 18.7
(checksums off) fifteen backends crashed (six in the short form of 1 755 runs),
all on the `jsonb` column and all after the chunk (or a 4-byte word inside it)
was changed and the chunk's own CRC-32C and the footer CRC were recomputed: some
by a segmentation fault, some by `Assert("JBE_ISCONTAINER(entry)")` in
`jsonb_util.c`. Random
damage without recomputed checksums (the case checksums are for) never crashed
anything: it gave "is corrupt" errors. Arrays (`int[]`, `text[]`), numeric, text, bytea,
varchar, timestamptz and float8 did not crash under the same mutations with recomputed
checksums in these runs (clean "is corrupt" errors or accepted values); the README says numeric may crash too.

### C2-09 note: identity and cleanup costs

Not defects of correctness, listed so that nobody is surprised. (a) The identity
is 63 random bits and nothing checks for a collision at file creation; the
chance is 2^-63 per pair and I did not try to force one. (b) Every `DROP TABLE`
(of any table), every `VACUUM` of a columnar table and `columnar_check` read the
first block of the file of every columnar relation of the database
(`kc_uid_of_file`); with 1 500 empty columnar tables a create and drop of an
unrelated heap table took 0.02 s, so it is linear and cheap, but it is a cost
that grows with the number of columnar partitions. (c) `columnar_identity` is
executable by PUBLIC (SELECT privilege on the table is needed) and shows the
identity.

## Checked and found correct

Listed so that the absence of a finding is not read as absence of effort.

- Identity (item 1). Two relations of one database never shared an identity and
  none lost its own: `CREATE DATABASE ... TEMPLATE` with `file_copy` and with
  `wal_log` (the clone keeps the identity; its reads, inserts, `VACUUM`,
  `VACUUM FULL`, compaction, `SET TABLESPACE`, `TRUNCATE` and `DROP` leave the
  source untouched, and the source's `columnar_check` stays clean), `LIKE` and
  `CREATE TABLE AS` (new identity), `SET TABLESPACE` (same identity, an old
  snapshot still reads), a rolled back `SET TABLESPACE`, `TRUNCATE` and rewrite
  (the old identity is back), `SET TABLESPACE` followed by `TRUNCATE` or a
  rewrite in the same transaction (commit and rollback, rows right in both),
  `TRUNCATE` in the creating transaction and in a subtransaction, with `ROLLBACK
  TO`, partition attach and detach, orphan rows after `DROP` (counted, removed
  at once).
- Old snapshots (item 2). The 40001 appears exactly with a rewriting command:
  never for `SET TABLESPACE`, `columnar_compact`, `VACUUM`, a plain insert, a
  rolled back command or rewrite (also in a subtransaction that was rolled back),
  and never for the rewriting transaction itself at every later command, in its
  subtransactions after `RELEASE` and after a `TRUNCATE`, at REPEATABLE READ and
  SERIALIZABLE. A snapshot taken while a rewriting transaction was prepared fails
  after `COMMIT PREPARED`, and a snapshot taken after it reads. A hot standby
  query with an older snapshot fails with 40001 after the commit is replayed; a
  READ COMMITTED statement and a new snapshot read the new file; a standby query
  that holds the table is ended by the recovery conflict. Frozen and old ids: the
  first lap of the real wrap (46 batches of 100 million) shows no error.
- Catalog visibility (item 3). I compared `kc_tuple_visible` with
  `HeapTupleSatisfiesMVCC` branch by branch: xmin frozen, invalid, own (also an
  aborted own subtransaction, which `XidInMVCCSnapshot` and
  `TransactionIdDidCommit` classify as invisible, and a committed own
  subtransaction, which `TransactionIdIsCurrentTransactionId` accepts), in
  progress, in the snapshot; xmax invalid, lock only, own (including an aborted
  own subtransaction as deleter, which leaves the row visible), in the snapshot,
  committed, aborted; prepared transactions (their ids are in the snapshot);
  standby snapshots (the same `XidInMVCCSnapshot`). The only difference is the
  command id of an own deletion (C2-04). Hint bits are not set by the extension,
  which costs time only. The catalog rows of a scan are loaded once at scan start,
  so a concurrent compaction, a `VACUUM` tail truncation (it needs `ACCESS
  EXCLUSIVE`) or another writer's flush cannot change what a running scan reads;
  compaction always writes the new stripe after the old ones, so the truncated
  tail never holds a stripe an old snapshot needs.
- Write buffers (item 4). All 26 differential sequences equal to heap (both
  majors); stale buffers are hidden and dropped; aborted subtransactions free
  their buffer; no row was committed twice or lost; no pin leaked after errors
  inside exception blocks (a scan aborted by an error releases its pin).
- Parallel safety (item 7). See the table above: the columnar relation never
  appears below a Gather, and a PARALLEL SAFE function that reads one fails with
  `columnar tables cannot be read in a parallel worker` when a worker runs it,
  never with a wrong answer; parallel `CREATE INDEX` with such an expression fails
  the same way; `CREATE TABLE AS` and `VACUUM (PARALLEL n)` work.
- Publications (item 6), apart from C2-03: refused as the README says, and the
  refusal does not touch unrelated work (heap DDL, other publications, `ALTER
  TABLE` of a columnar table in a database with only heap publications all
  pass). In the state where a publication already included a columnar table
  (made while the event trigger was disabled) every `ALTER TABLE` of that table
  and every `ALTER PUBLICATION` of that publication is refused, `DROP TABLE` and
  heap DDL are not; a `pg_dump` of that state restores the table and its rows and
  fails on `CREATE PUBLICATION`, as expected. `test_decoding` of a transaction
  that writes a heap and a columnar table decodes the heap rows and, for the
  columnar write, only the inserts into the catalog table
  `kingres.columnar_stripe` (a publication of that table alone would ship catalog
  rows without pages; nothing prevents `CREATE PUBLICATION ... FOR TABLE
  kingres.columnar_stripe`).
- Logical apply into a columnar table (first review, "not covered"): a
  subscriber with a columnar table of the same name takes the initial copy (2 000
  rows) and a later insert stream (2 500 rows, 2 stripes), `columnar_check`
  clean, on both majors. An `UPDATE` on the publisher makes the apply worker exit
  with code 1 in a loop (a table without replica identity, as for any
  append-only target); the subscriber keeps its rows.
- Relation files over 1 GB: a 45 million row table of 1.9 GB (two segment files,
  codec none) with stripes across the boundary: counts, sums, a filter on the
  tail, `ANALYZE`, `VACUUM` (a rolled back load of 3 million rows truncated the
  file from 1938 MB to 1900 MB across the segment end), `VACUUM FULL`,
  `columnar_compact` all consistent, on both majors.
- Autovacuum: a columnar table is vacuumed and analyzed once after its inserts
  and not again; `relfrozenxid` 0 does not make it loop.
- Real wrap with the `xid_wraparound` module (built against both installations,
  run on 18.7): 4.6 billion ids consumed in batches; reads and inserts of a
  columnar table stayed correct across the wrap (but see C2-06).

## Not reproduced

- `kc_footer_load` allocates `ngroups * ncols` chunk entries and two pointer
  arrays before it has validated the footer entries; the group count is bounded
  only by the footer length divided by 4, so a footer of 64 kB on a table of 800
  columns (catalog row and header edited consistently, footer CRC recomputed)
  would ask for about 0.5 GB plus 0.2 GB. A table of 1 600 columns and the same
  footer asks for more than 1 GB and fails cleanly ("invalid memory alloc request
  size"). Only a deliberately written file can do it (a damaged file fails the
  footer CRC); I did not build such a stripe.
- The same for `kc_decode_chunk`: for a variable-length column the claimed raw
  size may be up to about 1 GB plus 32 MB before the codec is asked; a crafted
  zstd frame header with a matching content size, a few bytes long, would make the
  server allocate it. Not built.
- A snapshot older than 2^30 transactions is ignored by the age test by design;
  not tried.
- The 63-bit identity collision of C2-09.

## Not covered

- `pg_rewind`: only the existing `002_standby` test (it passes on both majors); I
  did not write a case of my own.
- Archive recovery to the end of an archive: my harness (a base backup taken by
  `pg_basebackup` while `pgbench` loads and rewrites, then a restore with
  `restore_command`) stalled at a segment boundary with "contrecord is requested
  by ..." and "waiting for WAL to become available" on both majors, and it
  stalled the same way with heap tables only (the extension not involved), so I
  do not attribute it to the extension and could not use it. What I did
  establish: the base backup taken under load, started on its own, has the
  columnar table equal to its heap twin and a clean `columnar_check`, on both
  majors (4 runs on 18.7, 3 on 19rc2); a restore that replays a prefix of the archive is consistent
  (columnar equals heap twin, `wal_consistency_checking` silent).
- `valgrind` (not installed); memory use of the write path with more than 100 000
  subtransactions at once (extrapolated from C2-02); `pg_upgrade` (out of scope).
- Row-level security, the leak of skip counters through `EXPLAIN (ANALYZE)`,
  generated columns and identity columns beyond one `TRUNCATE ... RESTART
  IDENTITY`.
