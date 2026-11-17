<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Portions Copyright (c) 2026, RooyeKhat Media Co -->
# kingres_columnar

`kingres_columnar` adds a table access method, `columnar`, to KingresDB. A
columnar table stores each column on its own, compressed, in stripes of rows. A
query that reads a few columns of a wide table reads only those columns, and a
query with a filter can skip whole stripes and chunk groups with the stored
minimum and maximum. It is meant for append-only analytic data: time series,
events, logs, and old partitions of such data. The design is in
`kingres/docs/design/analytics-phase12-storage.md`; this file says what version
0.1 does.

Version 0.1 is "increments C1 to C4" of the design: insert-only, no indexes,
serial scan, codecs `none`, `pglz`, `lz4` and `zstd`, `VACUUM`, `VACUUM FULL`,
standby reads. It adds no core patch and bundles no third-party code.

## Installing

The library has to be preloaded on every node that writes, replays or restores
columnar data, standbys included, because it registers a WAL resource manager:

```
shared_preload_libraries = 'kingres_columnar'
```

then `CREATE EXTENSION kingres_columnar;` (superuser). Objects are in schema
`kingres`. Without the preload `CREATE EXTENSION` works and tables that
already exist can be read, but everything that would write columnar data is
refused with an error that names the setting: `CREATE TABLE ... USING
columnar`, `INSERT`, `COPY`, `TRUNCATE`, a rewrite into columnar, and
`columnar_compact`. A server that has ever written columnar WAL needs the
library for as long as that WAL exists (crash recovery, archive recovery,
base backups, `pg_rewind`). Resource manager id: 128 (`RM_EXPERIMENTAL_ID`).
It must be replaced by an id reserved on the public registry before a release,
because the id is part of the WAL format.

## Use

```sql
CREATE TABLE events (ts timestamptz, device int, value float8) USING columnar;
INSERT INTO events SELECT ...;            -- INSERT, INSERT ... SELECT, COPY, CREATE TABLE AS
ALTER TABLE t SET ACCESS METHOD columnar; -- rewrites the table; "heap" converts back
```

Rows are buffered in the backend and become a stripe when 150000 rows are
buffered, when the buffers exceed `kingres_columnar.write_buffer_memory`, or
at commit (the memory a backend may hold is stated under "Memory and costs").
Many tiny transactions therefore make tiny stripes; load in big transactions
and use `kingres.columnar_compact` afterwards.

### Functions

| Function | Meaning |
|---|---|
| `kingres.columnar_set_options(rel, compression, compression_level, stripe_row_count, chunk_row_count)` | Options for stripes written from now on (a null argument keeps the value). Owner only. Not transactional. |
| `kingres.columnar_options(rel)` | The options in force. |
| `kingres.columnar_info(rel)` | Stripes, chunk groups, rows, bytes on disk, uncompressed bytes, ratio. |
| `kingres.columnar_stripes(rel)` | One row per visible stripe. |
| `kingres.columnar_compact(rel, min_rows)` | Merge stripes with fewer than `min_rows` rows (default a quarter of the stripe size); returns the number of stripes written. Owner only. It changes nothing logically: for every transaction, the one that calls it and every other, the rows each snapshot sees are the same before and after, and under SERIALIZABLE it creates no dependency (see "Isolation"). |
| `kingres.columnar_check(fix)` | Superuser. Reports catalog rows of files nobody uses, stripes beyond the end of the file, stripes whose first page is not theirs, stripes with more rows than the developer setting `max_stripe_rows` lets a stripe have today (`stripe_over_row_limit`: readable, a rewrite writes them again), and relations that cannot be read (`unreadable`, with the reason). `fix` deletes the first two kinds. |
| `kingres.columnar_reconcile()` | Superuser. Deletes catalog rows of files no relation uses (it reads the first block of every columnar file of the database). A row that another session is deleting or has deleted is left to it, never an error. |
| `kingres.columnar_identity(rel)`, `kingres.columnar_creating_xid(rel)`, `kingres.columnar_table_xid(rel)` | The identity of the stored content, the id of the transaction that created the file (2 once frozen) and the id that stands for the creation of the table (0 when it is not known, 2 once frozen): what the metapage holds (see "On-disk format"). |

`SELECT` privilege on the table is needed for `columnar_options`,
`columnar_info`, `columnar_stripes`, `columnar_identity`, `columnar_creating_xid`
and `columnar_table_xid`: the last three return a stable identifier of the
stored content and transaction ids, which say when the table was made and
rewritten.  PostgreSQL's size functions (`pg_relation_size`) ask for nothing
because they return a size; these return identifiers of a table's content, so
they ask for what reading the table asks for, as `columnar_info` and
`columnar_stripes` already did (a role without the privilege gets "permission
denied for table", tested in `columnar_security`).  `columnar_set_options` and
`columnar_compact` need ownership.

### Settings

| Setting | Default | Meaning |
|---|---|---|
| `kingres_columnar.compression` | `auto` | Codec of a new table: `none`, `pglz`, `lz4`, `zstd`, `auto` (zstd, else lz4, else pglz). Kept in the metapage. |
| `kingres_columnar.compression_level` | 3 | zstd level (1 to 19); lz4 and pglz ignore it. |
| `kingres_columnar.stripe_row_count` | 150000 | Rows of a full stripe (1000 to 10000000). |
| `kingres_columnar.chunk_row_count` | 10000 | Rows of a chunk group (100 to 100000). A group also ends when its raw size reaches 32 MB. |
| `kingres_columnar.write_buffer_memory` | 128MB | Memory for the rows of stripes being built, per backend, all tables together (1MB to the largest value). A soft limit with a hard ceiling of twice this value, see "Memory and costs": the buffers are written as soon as they are over it and the running subtransaction may write them, and a write that would go on from above the ceiling is refused with an error. A buffer that a running scan reads is never flushed under it. A flush that memory asks for writes whole buffers, the largest first, and a buffer is written alone only when it has `max(chunk_row_count, 1000)` rows or half of this memory, so the stripes of a table do not depend on how its rows were grouped into subtransactions. |
| `kingres_columnar.max_stripe_rows` | 10000000 | Developer setting, superuser only: lowers the most rows the writer puts in one stripe (and the size above which `columnar_check` reports a stripe), so the limit can be tested with a few thousand rows. It cannot raise the compiled limit and does not touch the reader, so no value can make a table unreadable. |
| `kingres_columnar.fault_point`, `kingres_columnar.fault_nth` | off, 0 | Developer settings, superuser only, present only when the server is built with assertions (`USE_ASSERT_CHECKING`): make the nth allocation of the write path (`alloc`), the nth relation of a subtransaction's hand-over to its parent (`merge`) or the nth relation of a flush (`flush`) fail once with "out of memory". Used by `t/016_atomic_handover.pl`; absent from production builds. |
| `kingres_columnar.enable_custom_scan` | on | Use the Columnar Scan (projection, skipping). |
| `kingres_columnar.enable_chunk_skipping` | on | Use minimum and maximum to skip. |

### EXPLAIN

```
Custom Scan (Columnar Scan) on events
  Filter: (ts >= ...)
  Columnar Projected Columns: 2 of 3 (ts, value)
  Columnar Skip Conditions: ts
  Columnar Stripes Skipped: 0 of 4            -- with ANALYZE
  Columnar Chunk Groups Skipped: 31 of 45     -- with ANALYZE
  Columnar Compressed Bytes Read: 1234 bytes  -- with ANALYZE
```

Skipping is an optimisation only: the clauses stay in the plan as filters.
Skipping understands `col OP expr` (OP in the btree family of the column's
type, constants, parameters and stable functions on the other side),
`IS [NOT] NULL`, `= ANY (array)` and `AND`/`OR` of these. Statistics are kept
for plain base types without a collation or with the `C` collation, and when
a minimum and a maximum are at most 64 bytes each; arrays, ranges, composites,
enums, domains and text under a non-`C` collation have none.

## On-disk format

One main fork, no other fork.

* Block 0, the metapage: magic, format version, options, the next row
  number and the next stripe id, the identity, the creating transaction, the
  relation the file was created for and a flags word (`REPLACES_CONTENT`: the
  file replaced the content of an existing table, TRUNCATE). Changed in place and logged as a whole page.
* Every other block belongs to a stripe: a run of consecutive blocks written
  once, never changed, each with a 24-byte special area (magic, page number,
  page count, writer transaction, stripe id). The payload of the pages, 8144
  bytes each, is the stripe blob.
* The stripe blob: a 64-byte header, the chunk groups, the footer. A chunk
  group holds, for each column, one chunk: an optional null bitmap, then the
  non-null values densely (fixed-width values in server byte order;
  variable-length values as an array of 32-bit lengths and the bytes without
  their header), compressed as a unit; a chunk that does not shrink is stored
  uncompressed. The footer lists, per chunk group and column, the offset, the
  compressed and raw sizes, the codec, the null count and the minimum and
  maximum. Its CRC-32C is in the header. Large values are stored inline (there
  is no TOAST); a chunk group closes early to keep its raw size near 32 MB.
* The stripe catalog `kingres.columnar_stripe`, an ordinary heap table keyed by
  the identity of the stored content (a random id and the creating transaction, in
  the metapage, protected by its own checksum): stripe id, first row, counts, first block,
  block count, sizes, the command-id change points of the writer (`cid_points`)
  the stripe-level statistics (`col_stats`) and a `maintenance` flag (the stripe
  was written by `columnar_compact`: it copies rows readers see in the stripes
  it replaces). A stripe is visible exactly
  when its catalog row is, so rollback, crash recovery, standbys, backup and
  `VACUUM FULL` follow from PostgreSQL's own rules. A byte-for-byte copy of the file (`SET TABLESPACE`, a base backup) keeps
  its identity and so its stripes, with nothing to update; a rewrite or
  `TRUNCATE` creates a new file with a new identity and an empty catalog.
  Older snapshots, in every isolation level (a READ COMMITTED statement that
  reads through a STABLE function has a snapshot older than the command that
  ran meanwhile): `SET TABLESPACE`, `columnar_compact` and `VACUUM` change
  nothing for them. `VACUUM FULL` does not change what the table contains, and
  keeps it for them as heap does, when every stripe was committed before the
  oldest snapshot (so before every snapshot that exists): the new stripes get
  frozen catalog rows and the new file records no creating transaction. When
  some stripe is newer than a snapshot that is still open, or something is in
  progress or recently deleted (`columnar_compact`), the old snapshot would
  have to see a different set of stripes and gets the error instead (below).
  A snapshot older than the *creation* of a table is a different case: the
  table's first file (created for that very relation, not by `TRUNCATE`)
  replaced nothing, every stripe in it is newer than the snapshot, and the
  snapshot is handed an empty table exactly as heap does (a new partition,
  `CREATE TABLE AS`, `ATTACH PARTITION` of a table created after the
  snapshot, a table dropped and created again under the same name). The rule:
  the error is raised only for content that existed for the snapshot and was
  replaced. The metapage therefore holds, besides the transaction that created
  the file, the one that created the *table* (`table_xid`), recorded only when
  it is known for certain: a table made by `CREATE TABLE` (also `CREATE TABLE
  AS`, a partition, a materialized view) has the transaction that made it;
  `TRUNCATE` copies it from the old file and a rewrite (`VACUUM FULL`, `ALTER
  TABLE`, `REFRESH MATERIALIZED VIEW`) from the columnar table it rewrites; a
  table that was not columnar and is converted has the transaction that
  converts it when it was made in that very transaction or has no storage at
  all (nothing could have been seen), and otherwise none (0, "not known"). The
  transaction that last wrote the table's `pg_class` row is not used: any
  `GRANT`, new owner, storage option or `ADD COLUMN` writes the row again, later
  than the creation, and a snapshot that saw rows would then be given an empty
  table. A snapshot that cannot see `table_xid` is older than the table, so
  whatever files the table has had since, there was nothing for it to read and
  it gets an empty table, like heap: a table created after the snapshot and
  then truncated, vacuumed full, rewritten or moved to columnar. When
  `table_xid` is 0 (a table converted from another access method, or a file of
  an earlier build) the reader asks the catalog instead: the snapshot sees an
  old version of the table's `pg_class` row, and one stays in the catalog for as
  long as the snapshot lives, whatever has been done to the row since, exactly
  when the table existed for it. If none is visible the table did not exist for
  the snapshot and it gets the empty table; if one is, content may have existed
  that the conversion replaced, and the snapshot gets the error. An error is
  therefore the answer to every older snapshot that may have seen content of the
  table, which is stricter than heap, which answers such a snapshot with an
  empty table after its own rewrites (TRUNCATE, `ALTER TABLE ... TYPE`,
  `SET ACCESS METHOD`); the extension keeps the strictness on purpose, and
  never an empty table for content that existed. A heap table that was
  truncated or emptied and vacuumed before it was converted has no storage and
  gives the empty table, as heap does. `table_xid` is an id in the file like
  the creating one: VACUUM freezes it (0 stays 0) and counts it for
  `relfrozenxid`. `TRUNCATE`, `SET ACCESS METHOD` and `ALTER TABLE` that
  rewrite the rows (`ALTER COLUMN TYPE`, `ADD COLUMN` with a volatile default)
  change what the table contains: a snapshot older than the commit does not get
  an empty table (as heap gives for these): its read fails with SQLSTATE
  `40001`, naming the rewrite. `CLUSTER` is refused (a columnar table has no
  index).
* Transaction ids: a columnar file holds two, in its metapage: that of the
  transaction that created the file (`kingres.columnar_creating_xid(rel)`, 2
  once frozen) and that of the one that created the table
  (`kingres.columnar_table_xid(rel)`, see above; 2 once frozen, 0 when it is not
  known). For a table created in a
  subtransaction the id is the subtransaction's
  id, which is right for every use made of it (comparison with a snapshot and
  with the oldest running transaction, `relfrozenxid`, the horizon of the
  freeze record: plain id comparisons that PostgreSQL makes with subtransaction
  ids itself). VACUUM freezes each once every snapshot can see it (it precedes the
  oldest transaction still running; the change is WAL-logged and cancels
  hot-standby queries that might hold it) and reports the oldest id that is
  left as the table's `relfrozenxid`. The pages of a stripe hold no
  transaction id (that field is reserved, zero).
* WAL: the resource manager `KingresColumnar` logs whole-page images only
  (`STRIPE_PAGES`, `METAPAGE`); redo restores images and has no logic of its
  own. A stripe's pages are logged before its catalog row is inserted.

What is validated when a chunk is read: page and stripe identity, the footer
CRC, offsets, counts, codec ids, decompressed sizes, statistic lengths and
headers, and a CRC-32C of every stored chunk, checked before the chunk is
decoded, independent of data checksums, so accidental damage gives SQLSTATE
`XX001`. The structure of array values is checked (header, dimensions, null
bitmap, element sizes) before the array code sees them. Other variable-length
types (numeric, jsonb, ranges, tsvector, geometric types, ...) are handed to the
type's own code after their lengths are checked, as heap does for its pages: a
file written on purpose, with valid checksums, by someone who can write the data
directory can still crash the server through such a type, as with heap. The
metapage has its own checksum. A damaged byte inside a stripe page is caught by
the chunk checksum; page checksums, when enabled, catch it earlier.

## What is refused, and why

All with SQLSTATE `0A000`.

* `UPDATE`, `DELETE`, `MERGE` that matches a row: "columnar tables are
  append-only". A row has no version header and no deletion mask. Convert the
  table or partition to `heap`, change it, convert it back, or drop whole
  partitions.
* Row locks (`SELECT ... FOR UPDATE/SHARE`), `ON CONFLICT DO UPDATE`,
  `TABLESAMPLE`, `CLUSTER`.
* Indexes, primary keys, unique constraints (no row identity until a stripe is
  written); `CREATE INDEX` fails in the access method.
* Foreign keys on or to a columnar table, and row-level `AFTER` triggers
  (event triggers refuse them at creation; the executor re-reads such rows
  by TID, which a columnar row has not). `BEFORE ROW` and statement
  triggers work.
* Temporary and unlogged tables.
* Parallel scans: no plan over a columnar table runs in a worker (rows the
  transaction has not flushed exist only in the leader), and a parallel-safe
  function that reads one inside a worker fails with `0A000`.
* Logical replication: a columnar table cannot be put in a publication, a
  publication that would include one (`FOR ALL TABLES`, `TABLES IN SCHEMA`) is
  refused, and so is creating a columnar table into such a scope: "columnar
  tables are not replicated logically yet".
* A row cannot be fetched by TID. `ctid` shows a number made from the row's
  position; it is not stable and means nothing.

## VACUUM and space

`VACUUM` records page and row counts, returns the unreferenced tail of the file
(it takes a conditional `ACCESS EXCLUSIVE` lock, as heap does; on a standby
that lock cancels conflicting queries), deletes catalog rows of dead files and
resets the insert counters so autovacuum does not repeat. Space in the middle
of the file is not reused: pages of rolled-back or crashed loads and of stripes
replaced by `columnar_compact` stay until they are at the end or the table is
rewritten with `VACUUM FULL`.

How the stripe rows of a file that goes away are removed.  Correctness never
depends on an event trigger firing; the triggers are only a way to do it earlier.
The access method itself learns that a file goes away: it hooks the drop of a
relation (an object access hook), and every way a columnar file is replaced or
dropped ends in the drop of a relation that holds it: `DROP TABLE`, `DROP
SCHEMA`, `DROP OWNED`, `DROP EXTENSION`, and every rewrite (`VACUUM FULL`, a
rewriting `ALTER TABLE`, a change of access method in either direction,
`REFRESH MATERIALIZED VIEW`), which makes a transient table, swaps the files and
drops the transient table, which then holds the OLD file, whatever access method
the table has become.  The hook notes the identity of that file (read from its
first block) in the memory of the transaction, for the subtransaction that
dropped it (a rollback to a savepoint takes the note back, a release hands it to
the parent).  The stripe rows of the noted files whose relation is gone, or has
another file by then, are deleted by the transaction that dropped them, at the
latest at its pre-commit or pre-prepare, and come back if it rolls back.  A note
of a file that is still in use (a drop that was rolled back, a rewrite that
failed) deletes nothing.  The two event triggers (`DROP`, and `ALTER TABLE`) call
the same cleanup at the end of the command, so that `columnar_check` has nothing
to report inside the transaction either; without them (`event_triggers = off`,
a trigger disabled, single-user mode, a database whose install script is an
earlier one) the rows are removed at the commit instead, which is tested for
every command above (`t/023_rewrite_cleanup.pl`).  The cost is that of the
files the command replaced, the same for an `ALTER TABLE` or a `DROP TABLE` in a
database with ten or five thousand columnar tables, and two commands on
different tables share no row, so neither waits for the other.  A row that
another session deleted meanwhile (`VACUUM`, `columnar_reconcile()`) is already
gone and not an error; one that it is deleting is left to it.  `VACUUM`,
`columnar_check(true)` and `columnar_reconcile()` remove the rows of any file
that is left (a crash, a restore) and read the first block of every columnar
file; leftover rows can never be mistaken for the stripes of a later file,
because a new file draws an identity that no row of the catalog, live or dead,
has.  A database whose install script is an earlier one (it has a
`table_rewrite` trigger and an `ALTER TABLE` trigger that runs the full
reconcile after every `ALTER TABLE`, or lacks the newer functions) works fully:
the cleanup is done by the access method, its old triggers only add the cost of
the full reconcile.  To get the current trigger functions, run the `CREATE
FUNCTION columnar_on_drop()` and `CREATE FUNCTION columnar_check_ddl()`
statements of `kingres_columnar--0.1.sql` as `CREATE OR REPLACE FUNCTION`, as
superuser; the old `table_rewrite` trigger can stay (it only names a file that
the access method names itself, and `columnar_note_rewrite` is still there for
it; tested in `t/023`).  The stripe catalog of an earlier shape is refused
with the statement that repairs it, as before.

`ANALYZE` samples blocks: a page of a stripe stands for the rows at the same
relative position in the stripe. It ignores rows buffered by the running
transaction.

## Backup, restore, upgrade

`pg_basebackup`, incremental backup, archive recovery and `pg_rewind` work
with the library preloaded. `pg_dump` writes `CREATE TABLE ... USING columnar`
and the rows as `COPY`; the options of a table live in its metapage and are
not in a dump, so a restored table takes the settings of the restoring
session. `pg_upgrade` is not supported in this version: the stripe catalog is
extension data and is not carried over. Files are not portable between
machines of different byte order, and a table written by one major version is
not claimed to be readable by another.

## Converting

To columnar: `ALTER TABLE t SET ACCESS METHOD columnar` (rewrite under
`ACCESS EXCLUSIVE`; the table must have no index, foreign key or `AFTER ROW`
trigger). Back: `ALTER TABLE t SET ACCESS METHOD heap` (also for rows over 8 kB: a
columnar table with a column that heap would toast has an empty TOAST table, so
that the heap table the rewrite creates has one). A partitioned table's
`SET ACCESS METHOD` changes the method of partitions created afterwards. A
rewrite other than `VACUUM FULL` and `SET TABLESPACE` (for example `ALTER
COLUMN TYPE`) gives the new file the default options.

## Isolation, transaction ages, names

All isolation levels work. A columnar table takes part in serializable
snapshot isolation as a heap table does, at the granularity of the relation:

* a scan takes a relation-level predicate lock before it reads the stripe
  catalog;
* a write checks for conflicts with those locks when a row is buffered and
  again when the buffer becomes a stripe (at pre-commit or pre-prepare, which
  PostgreSQL runs before its own serialization check, or when the buffer is
  flushed by size), so a reader whose lock came after the first check is
  found;
* a scan that skips a stripe because its writer had not committed before the
  snapshot reports the dependency reader-before-writer
  (`CheckForSerializableConflictOut`), as a heap scan does for a tuple it skips.
  The id in a stripe row is that of the subtransaction that flushed the stripe
  (a savepoint, an exception block, a flush by size or memory inside one), and
  the serializable machinery knows top-level transactions only, so the id is
  mapped to its top-level transaction (`SubTransGetTopmostTransaction`) first;
  nothing is reported when the id is older than the backend's oldest snapshot
  xmin (as in heap, which also avoids a `pg_subtrans` lookup for an id whose
  entry may be gone), when it is the reader's own transaction, or when the
  writing subtransaction aborted (heap sees such a tuple as dead);
* `columnar_compact` copies rows that readers see already. Its copies take no
  conflict-in check, its scan takes no predicate lock, and the stripes it
  writes carry the `maintenance` flag so that a reader whose snapshot cannot
  see them reports no dependency on the compacting transaction; heap's
  `VACUUM FULL` and `CLUSTER` do the same (they only move predicate locks to
  the relation). The copies go through a buffer of their own that no
  subtransaction hands over or merges, so a stripe is a maintenance stripe
  exactly when compaction wrote it, whatever else the transaction did, and a
  real write of the same transaction keeps all its checks. The stripes it
  writes are the compacting transaction's own, and a transaction sees all of
  its own stripes, so what it copies must be what its snapshot sees: in
  READ COMMITTED every statement has a fresh snapshot and the sources are the
  committed stripes; in REPEATABLE READ and SERIALIZABLE only the stripes the
  transaction's own snapshot sees are copied and replaced, a stripe committed
  after it is left alone (it stays invisible to this transaction, and visible
  to everyone with a later snapshot), and a source another transaction
  replaced since fails with a serialization failure. Other snapshots see the
  sources until the compaction commits and the copies after, never both and
  never a different set of rows. A `VACUUM FULL` has no earlier snapshot
  (it cannot run in a transaction block) and a rewriting `ALTER TABLE` reads
  what is committed and writes its rows as its own, as it does for heap;
* `columnar_info`, `columnar_stripes` and the other inspection functions read
  the stripe catalog and take no part in serializable isolation (no dependency
  is created or reported, no predicate lock is taken): a decision based on
  their result has no write-skew protection, as for any catalog query.

The set of transactions that commit is therefore one that heap also allows
(`t/009_serializable.pl` compares both, with write skew in both orders, the
read-only anomaly, buffered and size-flushed rows, savepoints, `PREPARE
TRANSACTION`, and mixed columnar and heap tables). Because the granularity is
the table, a reader of a columnar table conflicts with every writer of it, as a
heap sequential scan does. The stripe catalog is read without predicate locks,
so transactions on unrelated columnar tables do not abort each other.

A columnar table has a real `relfrozenxid`: the oldest transaction id its file
still holds (the creating transaction of the metapage; see "On-disk format").
VACUUM, including autovacuum's anti-wraparound vacuum, freezes it and advances
`relfrozenxid`, so `age(relfrozenxid)` is meaningful and the table takes part
in the protection against transaction id wraparound as a heap table does. The
stripe catalog is an ordinary heap table and is frozen as usual.

If the role that creates tables is named `kingres`, the default search path
(`"$user", public`) puts unqualified tables into the schema `kingres`, which
belongs to the extension (and is shared with the other KingresDB extensions).
Set `search_path` explicitly, or create the tables in another schema.

## Limits

Append-only, no indexes, no TOAST (values up to the 1 GB limit of one value are
stored inline), at most about 8.8 x 10^12 rows per table, 150000-row stripes
mean small transactions make small stripes, serial scans only, no vectorised
execution, skipping is not in the cost estimate, logical decoding is not
supported: a publication that would publish a columnar table is refused,
whatever form it has (also `publish_via_partition_root` through the root of a
partitioned table, and the table side of it), and a hostile or damaged file can
give a clean error but not always the right data.

Memory and costs worth knowing:

* Memory of the write buffers.  Rows of a transaction are buffered until the
  stripe is full, the buffers are over `kingres_columnar.write_buffer_memory`
  (A) or the transaction ends.  What counts is memory, not payload: the space
  of every by-reference value (its chunk in the allocator, header and rounding
  included), the arrays of row slots (their capacity), and 2 kB for every
  buffer that exists (its context and header, which an empty buffer still
  holds), so the number of buffers, whether levels of subtransaction, relations,
  partitions or buffers that cursors keep apart, is part of the bound and not
  beside it.  One accounting covers the whole backend.  **The bound**, which
  follows from the setting alone: with A = `write_buffer_memory` and R the bytes
  of the row being written,
  * the buffers hold at most 2A + R after every write (the ceiling, twice the
    allowance, enforced where a row is added: a write that finds more than 2A
    held, after writing everything that may be written, is refused with the
    error "the write buffers of columnar tables hold N bytes, more than the
    limit of M bytes", whatever the number of subtransaction levels, cursors,
    relations and partitions) and at most A when everything they hold may be
    written;
  * for the length of the commit of a subtransaction its rows exist twice (the
    copy in the parent's buffer and the original), so the buffers hold at most
    2 * (2A + R) between the two phases of the hand-over; a hand-over that would
    leave more than 2A in the parent is refused (the subtransaction aborts
    cleanly);
  * the memory the backend takes is at most 2 * (2A + R) for the buffers (the
    allocator's blocks, at most 1 MB each, add up to as much as the rows),
    + (2A + R) for the rows of a committing subtransaction, + S for the stripe
    being built (the compressed bytes and a chunk of at most 32 MB raw; it
    depends on the stripe size and the width of the rows, not on A, and is not
    counted) + what the backend needs to run the statements.  Measured as the
    peak size of the backend (`VmPeak`), not by a counter of the extension
    (`t/025_memory_bound.pl`): 60 nested levels of 3.6 MB under A = 8 MB grew
    the backend by 19 MB (206 MB held before the limit existed), 40 cursors
    22 MB (114 MB before), 1 500 partitions and 1 500 tables of one or five rows
    under A = 1 MB by 1 to 2 MB more than the same load into heap tables, rows
    of 5 MB under 8 MB by 40 MB (a stripe being built);
  * what is written, and when.  The buffers of the running (sub)transaction are
    written, the largest first, as soon as the buffers together are over A, so
    when everything may be written they hold at most A and the row being
    written; an empty buffer left over is freed with the state of its relation
    (the next row makes them again).  Rows of enclosing subtransactions
    (savepoints, exception blocks) cannot be written by an inner one: a stripe
    carries the id of the subtransaction that writes it, and the rows would be
    lost with an abort of the inner one (the earlier rounds' hand-over writes
    the parent's rows from the committing child only because the rows stay in the
    buffer until the child's id is the parent's).  A write that is refused at
    the ceiling is therefore the answer where writing cannot help, and it is
    exact: the transaction can release or commit its levels, close its cursors,
    or raise the setting.  A buffer of less than `max(chunk_row_count, 1000)`
    rows and half of A is written only while the flushable buffers together
    are half of A, so that a few rows are never made a stripe because outer
    levels hold the memory;
  * a cursor on the table holds the buffers that had rows when it began (they
    are read by position and are neither written nor merged under it, and take
    no more rows); rows written after that go to a buffer of their own that is
    written under A.  The held rows count against the ceiling, which bounds
    the number of cursors that can hold rows at once by it;
  * for the length of the commit of a subtransaction its buffers of less than
    half of A exist twice, as above; a bigger one is written by the
    subtransaction itself instead of being copied when the copy would take
    the memory over A.
  The stripes of a table depend on the data and the options, not on the grouping
  into subtransactions, except where the bound forces a flush: when the buffers
  are over A and the running subtransaction may write them (the largest first),
  when a hand-over leaves more than A, and (with a stripe of one row or more,
  never more than the stripe size) for rows written after the commit-time
  flush, below.

* Rows that reach the access method after its own commit-time flush.  The access
  method writes every buffer in its pre-commit (and pre-prepare) callback.
  PostgreSQL runs callbacks of other modules that were registered earlier (a
  module listed before this one in `shared_preload_libraries`) after it, and
  they may run SQL.  Everything that can write after the flush and before the
  commit record is: such callbacks (`XACT_EVENT_PRE_COMMIT`, `PRE_PREPARE`;
  in a parallel worker a write is refused); the callbacks of a committing
  subtransaction that run after the first phase of its hand-over (see below);
  and nothing else in `CommitTransaction` and `PrepareTransaction`: deferred
  triggers and portals run before the callbacks, `PreCommit_on_commit_actions`
  acts only on temporary tables (a columnar table cannot be temporary or
  unlogged, so `ON COMMIT DELETE ROWS` and `ON COMMIT DROP` never reach one),
  and the notify, large object and serializable steps write no tables.  A row
  written from then on is written to a stripe at once (write-through), in the
  caller's memory and outside the buffers, so it is committed with its
  transaction whatever the order of the libraries; a `COPY` or a multi-row
  insert is one stripe per batch, and a single row is a stripe of one row
  (correct, and rare; `VACUUM` and `columnar_compact` join small stripes).  A
  delete or an update is refused with an error, as always (the table is
  append-only).  A write after the point where the transaction state is no
  longer "in progress" (`XACT_EVENT_COMMIT`, `SUBXACT_EVENT_COMMIT_SUB`) is
  refused with an error and never dropped silently; an error in
  `SUBXACT_EVENT_COMMIT_SUB`, which PostgreSQL treats as the abort of a
  subtransaction that has committed, is the one thing this extension cannot
  undo for the rows it has already handed to the parent, and no module should
  raise one.  Tested with the probe module and a heap twin, both load orders, on
  both majors (`t/024_commit_window.pl`).

* The rows of a committing subtransaction are handed to its parent in two
  phases (see `kc_write.c`): everything that can fail (memory, the flush) is
  done for every relation in `SUBXACT_EVENT_PRE_COMMIT_SUB`, while the
  subtransaction can still abort cleanly, and nothing is published;
  `SUBXACT_EVENT_COMMIT_SUB` only assigns counts and pointers.  An error in any
  relation therefore leaves the rows of none of them in the parent (PostgreSQL
  aborts a subtransaction whose commit callback throws, and runs its abort
  callbacks, but only the first phase may fail).  When the parent's buffer, with
  the child's rows, holds a stripe or more, or the memory allowance is used up,
  the commit of the child writes it as stripes of the table's size (under the
  child's transaction id, which is the parent's when it commits; the rows stay
  in the buffer until then, so an abort loses nothing), and keeps a tail shorter
  than a stripe.  The number and size of the stripes of a table depend on the
  data and the options, not on the grouping into subtransactions.  Between the
  phases the stripe and the buffer both hold the rows that went into it, so a
  reader there sees only the part of each buffer that is not in a stripe (one
  definition of the visible part of a buffer, used by every reader); PostgreSQL
  itself runs nothing between the phases, but a callback of another extension
  can: a read there sees each row once, a write to any table (the one whose
  rows are being handed over or another) is written to a stripe at once, as
  above, and no buffer is touched: from the moment a buffer is prepared until
  it is published or dropped, nothing flushes, frees, moves or adds to it (no
  chooser of a victim for the memory limit, no flush of a relation, no join of
  buffers sees it as flushable), which the production build keeps as well as the
  one with assertions (`t/022_handover_window.pl`, run on a server built without
  assertions as well: a write to other tables under memory pressure in the
  window gave 7000 rows for 4000 before); a truncate of a table whose rows are
  being handed over, that no SQL can reach, is refused with an error; and a cursor opened there cannot go on after the phases are done and says so,
  rather than read other rows than it began with.  Whatever flush comes next
  cuts a buffer into stripes of at most the table's stripe size (and never above
  10000000 rows, `KC_MAX_STRIPE_ROWS`, the one definition the writer, the option
  and `columnar_check` share), keeping a short tail buffered when more rows are
  coming.  The reader accepts exactly what the writer can write, 10000000 rows a
  stripe, and refuses a larger row count in a catalog row before it allocates
  anything from it (version 0.1 is not released and no build since the limit
  exists writes more); `columnar_check` reports a stripe above a lowered
  `max_stripe_rows` as `stripe_over_row_limit`: it is readable, and `VACUUM
  FULL` writes it again within the limit.  A scan holds the buffers it reads
  still; one abandoned by an error is never ended, so its hold is remembered in
  the resource owner that was current when it began (a portal's, or the
  (sub)transaction's) and released when the error is cleaned up: no error leaves
  a table unable to flush by size.
* The stripe catalog must have the columns this build uses.  One made by the
  build before the column `maintenance` existed (13 columns) is refused, on
  every read, insert, `COPY`, `VACUUM` and rewrite, with the statement that
  repairs it and loses nothing: `ALTER TABLE kingres.columnar_stripe ADD COLUMN
  maintenance boolean NOT NULL DEFAULT false` (as the owner of the extension),
  after which every table reads at once; `columnar_check` reports the tables as
  `unreadable` with the same reason.  The error never tells anyone to drop and
  create the extension, which drops the tables with their data (`t/023`
  builds the earlier shape and runs the statement).  A catalog of any other
  shape is refused with a hint to restore it from a backup.
* A chunk read allocates the size its footer records, bounded by what a chunk
  group can hold (32 MB plus one value, at most 1 GB); the footer arrays are
  sized from the footer's length, which must fit its group count.
* The identity of a file is 63 random bits, redrawn when the catalog already
  holds it. `DROP TABLE`, `VACUUM` and `columnar_check` remove catalog rows of
  files no relation uses any more, which reads the first block of every
  columnar relation file of the database (linear in their number: 0.02 s for
  1500 tables). A table access method has no callback at drop and PostgreSQL
  keeps no identity in `pg_class` for it, so there is nothing to look up
  instead; a database with very many columnar partitions pays this on every
  such command.  A rewrite, `VACUUM FULL`, `TRUNCATE` and `ALTER TABLE` do not:
  they know the file they replaced (see "VACUUM and space").
* A `jsonb` value in a chunk written on purpose with valid checksums can still
  crash the server (the structure of a jsonb container is not validated, as it
  is not for heap); the checksums cover damage, not malice.

## Building and testing

Out of tree against an installation (`pg_config` on the path):

```
make USE_PGXS=1 && make USE_PGXS=1 install
make USE_PGXS=1 installcheck       # needs a server with the library preloaded
```

or `kingres/dev/ext.sh kingres_columnar` and `kingres/dev/ext-majors.sh
kingres_columnar` for PostgreSQL 18 and 19. The tests are eleven `pg_regress`
files (`sql/`, `expected/`) and twenty-six TAP files in `t/`: crash recovery,
standby and `pg_rewind`, backup/restore/dump, concurrent writers, corruption,
a randomised comparison against heap (`KC_COMPARE_SEED`, `KC_COMPARE_STEPS`),
older snapshots, array validation, serializable isolation against heap twins,
the visibility rules and `VACUUM FULL`, memory with many subtransactions,
publications and wide rows, the atomic hand-over of a subtransaction under
injected failures (`t/016`), stripe sizes and the memory bound under any
grouping, row width and cursor (`t/017`), the maintenance commands against
older snapshots, tables older and younger than a snapshot (`t/019`) and the
creation of a table from every source (`t/021`), limits and flags, a reader or
writer between the two phases of a hand-over (`t/022`), the cleanup after a
rewrite (also with event triggers off or disabled, in single-user mode and in a
database made by an earlier script), concurrent commands and a stripe catalog of
the earlier shape (`t/023`), a callback of another module that writes after the
access method's own commit-time flush, in both load orders, against a heap twin
(`t/024`), the bound on the memory of the write buffers measured as the memory of
the backend, for levels of subtransaction, cursors, relations, partitions and wide
rows (`t/025`), a seeded random comparison with heap of cursors, savepoints and
writes under memory pressure (`t/026`; `KC_FUZZ_SEED`, `KC_FUZZ_EPISODES`), and
two laps of the transaction id space.

Four files do not run everywhere, and say why when they are skipped:

* `t/016_atomic_handover.pl` needs the developer settings `fault_point` and
  `fault_nth`, which exist only in a server built with assertions
  (`USE_ASSERT_CHECKING`); a production build has no fault injection at all
  (that is intended: the code is not compiled there), so the atomicity of the
  hand-over is tested on assertion builds only, which is where the tests are
  run, and a build for packaging skips the file;
* `t/013_xid_wraparound.pl` needs the `xid_wraparound` module of the
  PostgreSQL tree and takes minutes; `KC_SKIP_WRAPAROUND=1` skips it.  The
  freezing of the ids of the metapage is also tested without it, in
  `t/021_table_creation.pl`;
* `t/022_handover_window.pl` and `t/024_commit_window.pl` build the probe module
  `t/kcprobe/kcprobe.c` (callbacks at the end of a subtransaction and at the
  pre-commit and pre-prepare, which run SQL from the setting `kcprobe.sql` or
  `kcprobe.commit_sql`) with the compiler and headers of the installation
  (`pg_config`), and are skipped, with that reason, where they cannot;
* `t/025_memory_bound.pl` reads `/proc/self/status` and is skipped where there is
  no `/proc`.

`perf/perf.sh` measures size and scan time against heap.
