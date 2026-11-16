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
at commit. Many tiny transactions therefore make tiny stripes; load in big
transactions and use `kingres.columnar_compact` afterwards.

### Functions

| Function | Meaning |
|---|---|
| `kingres.columnar_set_options(rel, compression, compression_level, stripe_row_count, chunk_row_count)` | Options for stripes written from now on (a null argument keeps the value). Owner only. Not transactional. |
| `kingres.columnar_options(rel)` | The options in force. |
| `kingres.columnar_info(rel)` | Stripes, chunk groups, rows, bytes on disk, uncompressed bytes, ratio. |
| `kingres.columnar_stripes(rel)` | One row per visible stripe. |
| `kingres.columnar_compact(rel, min_rows)` | Merge stripes with fewer than `min_rows` rows (default a quarter of the stripe size); returns the number of stripes written. Owner only. It changes nothing logically, so under SERIALIZABLE it creates no dependency (see "Isolation"). |
| `kingres.columnar_check(fix)` | Superuser. Reports catalog rows of files nobody uses, stripes beyond the end of the file, stripes whose first page is not theirs, stripes with more rows than a stripe may have (`stripe_over_row_limit`: readable, see "Limits"), and relations that cannot be read (`unreadable`). `fix` deletes the first two kinds. |
| `kingres.columnar_reconcile()` | Superuser. Deletes catalog rows of files no relation uses. |

`SELECT` privilege on the table is needed for `columnar_options`, `columnar_info` and `columnar_stripes`.

### Settings

| Setting | Default | Meaning |
|---|---|---|
| `kingres_columnar.compression` | `auto` | Codec of a new table: `none`, `pglz`, `lz4`, `zstd`, `auto` (zstd, else lz4, else pglz). Kept in the metapage. |
| `kingres_columnar.compression_level` | 3 | zstd level (1 to 19); lz4 and pglz ignore it. |
| `kingres_columnar.stripe_row_count` | 150000 | Rows of a full stripe (1000 to 10000000). |
| `kingres_columnar.chunk_row_count` | 10000 | Rows of a chunk group (100 to 100000). A group also ends when its raw size reaches 32 MB. |
| `kingres_columnar.write_buffer_memory` | 128MB | Memory for stripes being built, per backend. A soft limit: buffers of an outer subtransaction cannot be flushed early. |
| `kingres_columnar.max_stripe_rows` | 10000000 | Developer setting, superuser only: lowers the most rows the writer puts in one stripe (and the size above which `columnar_check` reports a stripe), so the limit can be tested with a few thousand rows. It cannot raise the compiled limit and does not touch the reader, so no value can make a table unreadable. |
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
  the error is raised only for a file that replaced older content, that is a
  file with `REPLACES_CONTENT`, or created for another relation than the one
  it now belongs to (the temporary relation of a rewrite).
  `TRUNCATE`, `SET ACCESS METHOD` and `ALTER TABLE` that rewrite the rows
  (`ALTER COLUMN TYPE`, `ADD COLUMN` with a volatile default) change what the
  table contains: a snapshot older than the commit does not get an empty table
  (as heap gives for these): its read fails with SQLSTATE `40001`, naming the
  rewrite. `CLUSTER` is refused (a columnar table has no index).
* Transaction ids: a columnar file holds exactly one, in its metapage: that of
  the transaction that created it (`kingres.columnar_creating_xid(rel)`, 2 once
  frozen); for a table created in a subtransaction this is the subtransaction's
  id, which is right for every use made of it (comparison with a snapshot and
  with the oldest running transaction, `relfrozenxid`, the horizon of the
  freeze record: plain id comparisons that PostgreSQL makes with subtransaction
  ids itself). VACUUM freezes it once every snapshot can see it (it precedes the
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
rewritten with `VACUUM FULL`. Catalog rows of the old file of a rewrite stay
until the next `VACUUM` of any columnar table (`columnar_check` reports them as
`orphan_catalog_rows`); a `DROP TABLE` removes them at once.

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
  the relation). A buffer that also holds a row written for another reason
  keeps all its checks, so a real write of the same transaction is never
  hidden;
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

* Rows of a transaction are buffered until the stripe is full, the buffers
  exceed `kingres_columnar.write_buffer_memory` or the transaction ends; rows
  of an outer subtransaction cannot be flushed from an inner one, so the
  limit is soft there. The rows of committed subtransactions are merged into
  their parent's buffer, which can then hold more than a stripe; whatever
  flush comes next cuts it into stripes of at most the table's stripe size (and
  never above 10000000 rows, `KC_MAX_STRIPE_ROWS`, the one definition the
  writer, the option and `columnar_check` share), keeping a short tail
  buffered when more rows are coming. The reader accepts every stripe the
  writer can write and larger ones too (up to the 2^31-1 rows the catalog
  column holds), so a table written by an earlier build that merged without a
  limit (the reader then refused a stripe over 40 million rows) is read
  normally; `columnar_check` reports such a stripe as `stripe_over_row_limit`,
  and `VACUUM FULL` or `ALTER TABLE ... SET ACCESS METHOD` (to heap and back)
  writes it again within the limit. The merge is exception safe: the slots are
  reserved first, rows are copied past the parent's count, and the count is
  raised last. A scan holds the buffers it reads still; one abandoned by an
  error is never ended, so its hold is remembered in the resource owner that
  was current when it began (a portal's, or the (sub)transaction's) and
  released when the error is cleaned up: no error leaves a table unable to
  flush by size. A committed subtransaction hands its rows to its parent
  and frees its buffer: the memory of a transaction with any number of
  subtransactions (with or without an open cursor) is what its pending rows
  need.
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
  such command.
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
kingres_columnar` for PostgreSQL 18 and 19. The tests: eleven `pg_regress` files
(`sql/`, `expected/`) and fifteen TAP files in `t/`: crash recovery,
standby and `pg_rewind`, backup/restore/dump, concurrent writers, corruption,
a randomised comparison against heap (`KC_COMPARE_SEED`, `KC_COMPARE_STEPS`), older snapshots, array validation, serializable isolation against heap twins, the visibility rules and `VACUUM FULL`, memory with many subtransactions, publications and wide rows, and two laps of the transaction id space (`xid_wraparound` module; `KC_SKIP_WRAPAROUND=1` skips).
`perf/perf.sh` measures size and scan time against heap.
