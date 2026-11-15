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
`kingres`. Without the preload the extension still loads and reads, and
refuses to write. A server that has ever written columnar WAL needs the
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
| `kingres.columnar_compact(rel, min_rows)` | Merge stripes with fewer than `min_rows` rows (default a quarter of the stripe size); returns the number of stripes written. Owner only. |
| `kingres.columnar_check(fix)` | Superuser. Reports catalog rows of files nobody uses, stripes beyond the end of the file, stripes whose first page is not theirs, and relations that cannot be read (`unreadable`). `fix` deletes the first two kinds. |
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

* Block 0, the metapage: magic, format version (1), options, the next row
  number and the next stripe id. Changed in place and logged as a whole page.
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
  and the stripe-level statistics (`col_stats`). A stripe is visible exactly
  when its catalog row is, so rollback, crash recovery, standbys, backup and
  `VACUUM FULL` follow from PostgreSQL's own rules. A byte-for-byte copy of the file (`SET TABLESPACE`, a base backup) keeps
  its identity and so its stripes, with nothing to update; a rewrite or
  `TRUNCATE` creates a new file with a new identity and an empty catalog.
  Older snapshots: a REPEATABLE READ or SERIALIZABLE transaction whose snapshot
  is older than the commit of a `VACUUM FULL`, `TRUNCATE`, `SET ACCESS METHOD`
  or rewriting `ALTER TABLE` of the table does not get an empty table (as heap
  gives for some of these): its read fails with SQLSTATE `40001`, naming the
  rewrite. `SET TABLESPACE` and `columnar_compact` change nothing for it.
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
trigger). Back: `ALTER TABLE t SET ACCESS METHOD heap`. A partitioned table's
`SET ACCESS METHOD` changes the method of partitions created afterwards. A
rewrite other than `VACUUM FULL` and `SET TABLESPACE` (for example `ALTER
COLUMN TYPE`) gives the new file the default options.

## Isolation, transaction ages, names

All isolation levels work. Under SERIALIZABLE a scan takes a relation-level
predicate lock and an insert checks for conflicts on the relation, so a real
write skew on one columnar table is detected; the stripe catalog is read
without predicate locks, so transactions on unrelated columnar tables do not
abort each other.

A columnar table has `relfrozenxid = 0` (it holds no row transaction ids):
`age(relfrozenxid)` shows the maximum and monitoring that alerts on table age
will alert on every columnar table; ignore them. Autovacuum's wraparound logic
skips a table whose `relfrozenxid` is not a normal transaction id
(`autovacuum.c`, `relation_needs_vacanalyze`), and `datfrozenxid` is not held
back; the stripe catalog is an ordinary heap table and is frozen as usual.

If the role that creates tables is named `kingres`, the default search path
(`"$user", public`) puts unqualified tables into the schema `kingres`, which
belongs to the extension (and is shared with the other KingresDB extensions).
Set `search_path` explicitly, or create the tables in another schema.

## Limits

Append-only, no indexes, no TOAST (values up to the 1 GB limit of one value are
stored inline), at most about 8.8 x 10^12 rows per table, 150000-row stripes
mean small transactions make small stripes, serial scans only, no vectorised
execution, skipping is not in the cost estimate, logical decoding is not
supported (do not publish a columnar table), and a hostile or damaged file can
give a clean error but not always the right data.

## Building and testing

Out of tree against an installation (`pg_config` on the path):

```
make USE_PGXS=1 && make USE_PGXS=1 install
make USE_PGXS=1 installcheck       # needs a server with the library preloaded
```

or `kingres/dev/ext.sh kingres_columnar` and `kingres/dev/ext-majors.sh
kingres_columnar` for PostgreSQL 18 and 19. The tests: ten `pg_regress` files
(`sql/`, `expected/`) and six TAP files in `t/`: crash recovery,
standby and `pg_rewind`, backup/restore/dump, concurrent writers, corruption,
and a randomised comparison against heap (`KC_COMPARE_SEED`, `KC_COMPARE_STEPS`).
`perf/perf.sh` measures size and scan time against heap.
