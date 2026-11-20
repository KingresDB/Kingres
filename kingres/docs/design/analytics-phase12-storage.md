<!-- SPDX-License-Identifier: AGPL-3.0-only -->
# KingresDB: design of phase 12, part 2 (columnar storage, time partitioning and retention)

Status: Proposed design, 2026-11-15; not implemented.

Part 1 of phase 12 ([analytics-phase12-queries.md](analytics-phase12-queries.md))
covers repartitioned joins, `MERGE` and approximate aggregates. This part adds
three things, each with its control in the web console:

1. **Columnar tables.** A table or a partition can be stored column by column
   with compression, through a new table access method in a new extension,
   `kingres_columnar` (version 0.1).
2. **Time partitioning with creation ahead of time and retention.** A new
   extension, `kingres_timepart` (version 0.1), keeps a policy per
   range-partitioned table, creates the partitions of the coming period, drops or
   detaches the partitions that are past retention, and converts old partitions to
   columnar storage.
3. **A scheduler** in `kingresd` that applies the policies, and the REST and
   console additions that show and change storage formats, policies and their
   runs.

It builds on [shard-phase2b.md](shard-phase2b.md) (row router, aggregate
pushdown), [shard-phase4.md](shard-phase4.md) (bucket moves),
[ha-phase5.md](ha-phase5.md) (standbys and failover),
[control-plane-3.md](control-plane-3.md) (task engine), and on
[shard-phase9.md](shard-phase9.md) (partitioned tables as distributed tables:
hash on top, the user's partitions below), and it takes the split of buckets of
[shard-phase10.md](shard-phase10.md) into account (section 9.7). The
non-distributed case is designed completely and does not wait for phase 9; section
11 says how the same policy runs on a distributed table with the functions phase 9
provides.

How this note was checked. Claims about PostgreSQL are checked against the
18.7 source in this tree with the file named, or against the 18 documentation
with its URL. Claims about the existing extensions and control plane are checked
against the committed files (`git show HEAD:<path>`), because other work is
editing `contrib/kingres_shard` and `kingres/control-plane` at the same time.
Claims about third-party projects come from the pages named in section 2 and are
as current as those pages. Anything not checked is marked **[Needs
verification]** with what would settle it, and collected in section 19. This note
did not read or copy any Citus source code: the Citus documentation was read for
the behaviour a user sees, and the Citus licence (AGPL, see
[ADR 0004](../adr/0004-postgresql-license-no-citus-code.md)) rules out its code
and its forks.

## Sources read

Project documents:

- `contrib/kingres_shard/README.md` (whole file, committed version) and
  [shard-phase4.md](shard-phase4.md) (bucket move by copy and logical
  replication, with its implementation notes).
- [shard-phase2b.md](shard-phase2b.md): sections 4 (fragments), 7 (aggregate
  pushdown, partial states), 8, 11 (the row router and `COPY FROM`, 11.8 ledger
  tables), 17 and 19 (shape of this note).
- [ADR 0022](../adr/0022-the-complete-scan-stays.md) (the complete scan stays),
  [ADR 0001](../adr/0001-thin-fork-with-extensions.md) (thin fork),
  [core-patches.md](../dev/core-patches.md), [documentation.md](../dev/documentation.md),
  [testing.md](../dev/testing.md).
- [shard-phase9.md](shard-phase9.md) (finished while this note was written): the layout of a
  user-partitioned distributed table, `kingres.dist_partition`, `create_partition`,
  `drop_partition`, `detach_partition`, `attach_partition`, `partition_leaves`,
  `max_partition_leaves`, the tree-aware move, and the section "What phase 12's
  time-partitioning tools can rely on". [shard-phase10.md](shard-phase10.md), sections 3.1
  (preconditions), 3.2 (the split algorithm), 3.5 and 17 (what it asks of this part).
- `kingres/api/openapi.yaml` and `kingres/api/README.md` at 0.6.0: `listTables`,
  `getTable`, `distributeTable`, `enableTableLedger`, the task model (`Task`,
  `TaskStep`, `TaskKind`, the table "Task kinds and subjects"), `ProblemCode`,
  the four cluster roles.
- `kingres/control-plane`: `internal/task/task.go` (task engine: one unfinished
  task per subject, lease and epoch fencing), `internal/monitor/runner.go` (the
  only periodic loop of `kingresd` today), `internal/ops/tables.go`,
  `README.md`; [control-plane-3.md](control-plane-3.md) (leader, internal API).
- `kingres/web/README.md` and the component list of `kingres/web/src`.
- `kingres/packaging/README.md` (extensions in the single package, compression
  libraries enabled in the release build).
- `contrib/kingres_ledger/README.md` (what a ledger table is, which DDL its event
  triggers reject) and a search of its sources for any check of the table access
  method.

PostgreSQL 18.7 source in this tree: `src/include/access/tableam.h` (the
`TableAmRoutine` struct and its comments), `src/backend/access/table/tableamapi.c`
(which callbacks are required), `src/include/access/xlog_internal.h`,
`access/rmgr.h` and `access/transam/rmgr.c` (custom resource managers),
`access/generic_xlog.h`, `src/backend/replication/logical/decode.c`
(`LogicalDecodingProcessRecord`, `heap_decode`, `DecodeInsert`),
`src/backend/commands/tablecmds.c` (lock levels of `SET ACCESS METHOD`, `ATTACH`,
`DETACH`, `CREATE TABLE ... PARTITION OF`, the access method of a partition),
`src/backend/tcop/utility.c` (`DETACH ... CONCURRENTLY` and transaction blocks),
`src/backend/commands/analyze.c` (block sampling),
`src/backend/access/transam/xloginsert.c` (WAL compression with `USE_LZ4` and
`USE_ZSTD`), `src/backend/access/common/toast_compression.c`,
`src/include/storage/off.h`, `meson.build` (the `lz4` and `zstd` options), and
`contrib/kingres_shard/src/dist_scan.c` at HEAD (how the distributed scan hooks
`set_rel_pathlist_hook`).

Web sources (read on 2026-11-15; each licence statement is what the page or the
licence file said that day):

- PostgreSQL 18 documentation: Table Access Method Interface Definition
  <https://www.postgresql.org/docs/18/tableam.html>, Custom WAL Resource Managers
  <https://www.postgresql.org/docs/18/custom-rmgr.html>, Generic WAL Records
  <https://www.postgresql.org/docs/18/generic-wal.html>; the registry of
  resource manager ids <https://wiki.postgresql.org/wiki/CustomWALResourceManagers>
  (named by the documentation; not opened).
- Citus documentation (behaviour only):
  <https://docs.citusdata.com/en/stable/admin_guide/table_management.html>
  (columnar storage and its limitations) and
  <https://docs.citusdata.com/en/stable/develop/api_udf.html>
  (`alter_table_set_access_method`, `alter_columnar_table_set`,
  `create_time_partitions`, `drop_old_time_partitions`,
  `alter_old_partitions_set_access_method`).
- Candidates for section 2: <https://github.com/hydradatabase/columnar>,
  <https://github.com/citusdata/cstore_fdw>, <https://github.com/duckdb/pg_duckdb>,
  <https://github.com/Mooncake-Labs/pg_mooncake>,
  <https://github.com/adjust/parquet_fdw>,
  <https://github.com/CrunchyData/pg_parquet>,
  <https://github.com/timescale/timescaledb> with
  <https://raw.githubusercontent.com/timescale/timescaledb/main/tsl/LICENSE-TIMESCALE>,
  <https://github.com/orioledb/orioledb>, and the search result that names the
  PGXN `storage_engine` fork of Hydra Columnar
  (<https://www.pgxn.org/dist/storage_engine/>).
- Time partitioning tools: <https://github.com/citusdata/pg_cron>,
  <https://github.com/pgpartman/pg_partman> with
  <https://raw.githubusercontent.com/pgpartman/pg_partman/master/doc/pg_partman.md>
  and <https://raw.githubusercontent.com/pgpartman/pg_partman/master/LICENSE.txt>,
  and the TimescaleDB retention policy page
  <https://www.tigerdata.com/docs/use-timescale/latest/data-retention/create-a-retention-policy>.

## Goals and scope

| # | Item | Where |
|---|---|---|
| 1 | A table access method `columnar`: stripes, chunk groups, one compressed stream per column, min/max skipping, append-only, WAL-logged, replicated, readable on standbys | sections 2 to 8 |
| 2 | Columnar storage inside the sharding extension: distributed tables, bucket moves, the row router, the complete scan, ledger tables | section 9 |
| 3 | Time partitioning: create ahead, retention, conversion of old partitions, a policy per table, a history of runs | section 10 |
| 4 | Where the schedule runs | sections 10.1 and 10.6 |
| 5 | Distributed time-partitioned tables, and what moves and splits do with columnar and time-partitioned tables | sections 9.7 and 11 |
| 6 | Control plane and REST additions, console | sections 13 and 14 |
| 7 | Tests, implementation order, open questions, limits | sections 15 to 18 |

Not in this part:

- `UPDATE`, `DELETE`, row locks and `MERGE` into a columnar table (section 7.7
  says why). Rows leave a columnar table by dropping a partition, or by
  converting it back to row storage, changing it, and converting it again.
- B-tree indexes, unique constraints, primary keys, foreign keys and `ON CONFLICT
  DO UPDATE` on columnar tables in the first version (increment C6 adds
  B-tree indexes; the others stay refused).
- Vectorized execution, late materialization, run-length or dictionary
  encoding, storing a table in Parquet or Iceberg files, a global columnar
  cache.
- Repartitioned joins, approximate aggregates and `MERGE` of distributed tables:
  part 1.
- Partitioning by anything but a single `date`, `timestamp` or `timestamptz`
  column with range partitions. Integer or epoch partition keys, list partitions
  and multi-column keys are refused by the policy functions.
- Retention by row (deleting old rows inside a partition), and any scheduler
  other than the one of section 10.6.
- Making a ledger table columnar or time-partitioned (section 9.6).

## 1. Decisions in one place

| # | Decision |
|---|---|
| D1 | Columnar storage is a table access method written in this repository, extension `kingres_columnar`, access method name `columnar`. No third-party columnar code is bundled (section 2). |
| D2 | The data lives in the relation's main fork in standard pages, through the shared buffer cache, logged by a custom resource manager that writes whole-page images. Stripe metadata is ordinary heap rows in an extension catalog table, so visibility, rollback, crash recovery, replication and backup come from PostgreSQL (sections 5 and 7). |
| D3 | The first version is insert-only (`INSERT`, `COPY`, `CREATE TABLE AS`), no indexes, no `UPDATE`/`DELETE`, serial and parallel scan through a custom scan with column projection and min/max skipping. |
| D4 | Codecs: `none`, `pglz` (always built in), `lz4` and `zstd` (when the server was built with `-Dlz4` and `-Dzstd`, which the release build enables). Default `zstd` level 3, falling back to `lz4`, then `pglz`. |
| D5 | Logical decoding of columnar inserts is a later increment (C7) and is required before a bucket of a columnar distributed table can move. Until then moves of such buckets are refused with a clear error, and no publication can silently miss columnar changes (section 9.3). |
| D6 | Time partitioning is a separate extension, `kingres_timepart`, in PL/pgSQL with no C code and no preload requirement. It keeps a policy table and a history of runs per database. |
| D7 | The schedule runs in `kingresd`, as a scheduler on the manager leader that starts tasks of kind `partition.maintain`, not in a background worker and not in `pg_cron` (section 10.6). All work is done by SQL procedures of `kingres_timepart`, which also work when called by hand. |
| D8 | An old partition is converted by building a columnar copy, then swapping it for the original in one transaction with short, bounded locks and retries. Writers of that partition wait during the copy. |
| D9 | REST changes are additive (a new contract revision, number assigned at merge): a `storage` property on tables, partition and policy resources, two task kinds, three problem codes. Policy, storage and run-now changes need `kingres_admin`; reads need `kingres_readonly`. |
| D10 | No core patch. The only change outside the two new directories is the two `subdir` lines in `contrib/meson.build`, which updates the row of KP-0003 in the core patch register. |
| D11 | Moves and splits (sections 9.7 and 11.6): a bucket move recreates each leaf with its source access method and works for columnar and time-partitioned distributed tables once logical decoding exists (C7, S3), and is refused with a stated message before; a bucket split (phase 10) is refused for columnar tables and, as phase 10 already says, for user-partitioned (time-partitioned) buckets; conversion of leaves of a distributed table waits for S3. |

## 2. Ways to get columnar storage, and the decision

### 2.1 What was compared

The question is how a table or a partition of a KingresDB database can be stored
column-wise with compression for analytic scans, with the guarantees of the rest
of the product: one WAL, streaming replication to standbys, base backups,
failover, transactions, and the sharding extension on top.

| Candidate | Kind | Licence (source) | PostgreSQL 18 | State | Verdict |
|---|---|---|---|---|---|
| Write a table access method in this repository | table AM, own code | PostgreSQL (this repository) | Built and tested on 18.7 by definition | New code | **Chosen** |
| Citus columnar | table AM | AGPL ([ADR 0004](../adr/0004-postgresql-license-no-citus-code.md)) | Not relevant | Not relevant | Cannot be used or copied |
| Hydra Columnar, <https://github.com/hydradatabase/columnar> | table AM, a fork of Citus columnar | The README says "AGPL 3.0 for Hydra Columnar, and all other code is Apache 2.0"; the repository sidebar and the root `LICENSE` file say Apache-2.0 (conflicting) | The page lists no supported versions; it says only that the table AM API was added in PostgreSQL 12. A packager's page (Pigsty) lists it for 14 to 16 only | The page shows no release; 359 commits, 26 open issues | Rejected: a Citus fork, so AGPL code (the README states AGPL for the columnar directory) |
| `storage_engine` (PGXN), <https://www.pgxn.org/dist/storage_engine/> | table AM, described by a search summary as a fork of Hydra Columnar | **[Needs verification]** (not read) | Its README mentions 18 in a benchmark guide (search summary only) | Unknown | Rejected: derived from Hydra, so from Citus; licence not checked |
| `cstore_fdw`, <https://github.com/citusdata/cstore_fdw> | foreign data wrapper | Apache-2.0 (page) | No: the page says PostgreSQL 9.3 to 12 and that columnar storage "is now part of the Citus extension" | Superseded | Rejected: no 18 support, and a foreign table is not a table (no `ATTACH PARTITION` of a plain partition, no logging of the data in the WAL **[Needs verification]**) |
| `pg_duckdb`, <https://github.com/duckdb/pg_duckdb> | query engine (DuckDB inside the server); reads and writes Parquet, Iceberg, Delta; one example uses `USING duckdb` | MIT (page) | Yes (14 to 18 on the page) | 3.3k stars, release v1.2.0 shown | Rejected as the storage of a table: the data would live in DuckDB's or object-store files, outside the WAL and replication; it runs queries, which is part 1's question, not storage. It remains a candidate for part 1 |
| `pg_mooncake`, <https://github.com/Mooncake-Labs/pg_mooncake> | a columnstore mirror of a table kept in Apache Iceberg, ingestion by a separate "moonlink" service, queries through `pg_duckdb` | MIT (page) | Build instructions cover 14 to 18 | 122 commits; no release history on the page | Rejected: an extra service and files outside the WAL; not one package with one WAL |
| `parquet_fdw`, <https://github.com/adjust/parquet_fdw> | read-only FDW over Parquet files | PostgreSQL licence (page) | The page names no version; it needs `libarrow` and `libparquet` | Marked "experimental" on the page | Rejected: read-only, a heavy C++ dependency, no transactions |
| `pg_parquet`, <https://github.com/CrunchyData/pg_parquet> | `COPY` to and from Parquet files and functions on them | PostgreSQL License, "Copyright (c) 2024, Crunchy Data Solutions, Inc." (the `LICENSE` file) | Yes (14 to 19 on the page) | 204 commits | Not a storage engine. It is useful for export of old partitions and is listed as a possible later addition, not bundled in this part |
| TimescaleDB, <https://github.com/timescale/timescaledb> | extension with compression of chunks into a columnar form | The repository is Apache-2.0 with a `tsl` directory under the Timescale License, which forbids using the licensed software to offer a database-as-a-service or software-as-a-service (section 2.2 of `tsl/LICENSE-TIMESCALE`). Whether the columnar compression is in the `tsl` directory was **[Needs verification]** (the page does not say) | Docker examples use a pg18 image | Active | Rejected: not under the PostgreSQL License, and it brings its own partitioning model (hypertables) that would replace the partitioning of phases 2 and 9 |
| OrioleDB, <https://github.com/orioledb/orioledb> | table AM with its own storage engine | Apache-2.0 or PostgreSQL License, at the user's choice (page) | `.pgtags` pins patched builds for 16, 17 and 18 | "Public beta", "not for production" (page) | Rejected: needs a patched PostgreSQL, and it is a row store, not a columnar one |

### 2.2 Time partitioning candidates (for section 10)

| Candidate | Licence (source) | PostgreSQL 18 | How it runs | Verdict |
|---|---|---|---|---|
| `pg_cron`, <https://github.com/citusdata/pg_cron> | PostgreSQL license (page) | The install instructions on the page target 18 | A background worker reads `cron.job` and runs jobs; needs `shared_preload_libraries`; the metadata is in one database (`cron.database_name`); jobs do not run in hot standby | Not used as the scheduler (section 10.6). Licence is compatible; the reasons are architecture |
| `pg_partman`, <https://github.com/pgpartman/pg_partman> | PostgreSQL License (`LICENSE.txt`) | "PostgreSQL >= 14"; 18 not named on the page | A function `run_maintenance()` run by an outside scheduler, or its own background worker `pg_partman_bgw` | Not bundled. Its model is the reference for the policy fields (`premake`, `retention`, `retention_keep_table`, a default partition) |
| TimescaleDB retention policies | see above | see above | `add_retention_policy(table, drop_after)` schedules a job that drops whole chunks whose range is outside the window; future chunks are not touched | Model only: retention by whole partition |

### 2.3 Decision

**Write the table access method in this repository** (`contrib/kingres_columnar`).
The reasons:

1. Every permissive-licence candidate that stores data in PostgreSQL is either
   not a table access method (FDWs, query engines, mirrors), or unmaintained for
   18 (`cstore_fdw`), or not production-ready and needing a patched server
   (OrioleDB). The only maintained table access methods that are columnar are
   Citus columnar and its forks, under AGPL.
2. The requirements of the product are the ones a table access method inside the
   WAL gives: the same WAL, the same replication slots and failover, the same
   backup, MVCC through PostgreSQL's own catalog rules, `ATTACH PARTITION` of a
   plain table. A storage in external files breaks each of them.
3. The thin-fork policy ([ADR 0001](../adr/0001-thin-fork-with-extensions.md))
   allows it: a table access method, a custom resource manager, a custom scan and
   a utility hook are all extension interfaces (section 3). No core patch.

The cost is honest and large. The columnar extension is the biggest single
piece of phase 12: an estimated 8 000 to 12 000 lines of C for the storage
engine, scan, vacuum and the later logical decoding, plus about as much again in
tests (**[Recommendation]** for the estimate; it has no measurement behind it;
phase 2b's steps are sized at 1 000 to 1 500 lines of C each, and section 16 has
eight columnar steps). The first version leaves out indexes, `UPDATE`/`DELETE`,
logical decoding, reuse of freed space and many encodings (section 16 names
exactly what version one is).

Compression libraries: the release build enables `-Dlz4=enabled`, `-Dzstd=enabled`
and `-Dzlib=enabled` (`kingres/packaging/README.md`, table "Meson options and
why"). PostgreSQL 18.7 already links and uses them: `xloginsert.c` and
`xlogreader.c` call `ZSTD_compress` and `ZSTD_decompress` under `USE_ZSTD` and
the LZ4 functions under `USE_LZ4`; `toast_compression.c` calls
`LZ4_compress_default`; `pglz_compress` and `pglz_decompress` are in
`common/pg_lzcompress.h` and always present. The extension adds no new
third-party library. It needs `liblz4` and `libzstd` at build and run time (the
package's run-time dependencies are computed with `dpkg-shlibdeps`, per the
packaging README), and its `meson.build` must name the `lz4` and `zstd`
dependencies defined in the top-level `meson.build` (lines 1071 to 1090 and 1702
to 1720). **[Needs verification]** that a `contrib` subdirectory can use those
variables as `contrib/meson.build` is written today (no existing contrib module
links either library).

## 3. Mechanism in PostgreSQL 18 terms

| Need | Interface, with the place in the 18.7 source | Use |
|---|---|---|
| Store and scan a table | `TableAmRoutine`, `src/include/access/tableam.h`; `CREATE ACCESS METHOD columnar TYPE TABLE HANDLER ...`; the handler returns the struct | Section 6 |
| Durable, replicated, crash-safe data pages | A custom resource manager: `RegisterCustomRmgr` from `_PG_init`, only while `shared_preload_libraries` is processed (`rmgr.c`: "Custom resource manager must be registered while initializing modules in shared_preload_libraries"), ids 128 to 255, `RM_EXPERIMENTAL_ID` (128) during development, a reserved id before release (documentation and `rmgr.h`) | Section 7.3. Generic WAL is the alternative; it has at most `MAX_GENERIC_XLOG_PAGES` buffers per record (`XLR_NORMAL_MAX_BLOCK_ID`, 4) and "Generic WAL records are ignored during Logical Decoding" (18 documentation), so it cannot serve section 9.3 |
| Logical decoding of columnar inserts | `rm_decode` of the resource manager. `LogicalDecodingProcessRecord` (`decode.c`) calls it for the records of a custom rmgr; a custom rmgr without `rm_decode` only has its xid noted and its changes are silently ignored (`decode.c` lines 115 to 120) | Section 9.3 |
| Read pushdown (columns, skipping) | `set_rel_pathlist_hook` plus a `CustomPath` and `CustomScan` (`extensible.h`, `ExplainCustomScan`). A plain `table_beginscan` carries no column list and no qualifiers (the `scan_rescan` key argument is unused by the executor), so the table AM alone cannot push anything down | Section 8 |
| Stripe visibility, rollback, standby reads | Heap rows of a catalog table, read with the scan's snapshot | Section 7.1 |
| Flush buffered rows at the right time | `RegisterXactCallback` (`XACT_EVENT_PRE_COMMIT`, `XACT_EVENT_PRE_PREPARE`, abort) and `RegisterSubXactCallback`; `finish_bulk_insert` | Section 7.2 |
| Refuse unsupported DDL | The table AM callbacks themselves (`index_build_range_scan`, `tuple_update`, ...), and one `ddl_command_end` event trigger (`ENABLE ALWAYS`, as the ledger does) for foreign keys | Sections 6 and 7.8 |
| Time-partition maintenance | Ordinary DDL: `CREATE TABLE ... (LIKE ...)`, `ALTER TABLE ... ATTACH PARTITION`, `DETACH PARTITION`, `DROP TABLE` | Section 10 |
| Scheduling | The task engine of `kingresd` (`internal/task`) | Section 10.6 |

**Core patches: none.** Every interface above is a public extension point. The
two new directories are added to `contrib/meson.build` (`subdir('kingres_columnar')`
and `subdir('kingres_timepart')`), which is the file already registered as
KP-0003 ("Adds `subdir('kingres_ledger')` and `subdir('kingres_shard')`"). The
register row of KP-0003 is updated in the commit that adds the lines, as
[core-patches.md](../dev/core-patches.md) rule 7 requires. If a later increment
finds that something cannot be done without a core change, it goes through the
register with its justification, as the ADR says. Candidates that were looked at
and are not needed: a hook for publication membership (section 9.3 works
without it), a table AM callback at relation drop (section 5.5 reconciles
instead).

## 4. Extensions, versions, installation and SQL interface

### 4.1 Extensions

| Extension | Version | Language | Needs `shared_preload_libraries` | Depends on | Schema |
|---|---|---|---|---|---|
| `kingres_columnar` | 0.1 | C | Yes (the resource manager must be registered before recovery; on every node, standbys included) | none | `kingres` |
| `kingres_timepart` | 0.1 | PL/pgSQL | No | none; uses `kingres_columnar` and `kingres_shard` when present | `kingres` |

The existing extensions put their objects in schema `kingres` (for example
`kingres.ledger_enable`, `kingres.move_bucket`); these two do the same, with
object names prefixed `columnar_` and `timepart_` so that they never collide.
Both are superuser-install (`CREATE EXTENSION` by a superuser; not "trusted").
Upgrade scripts follow the existing naming (`kingres_columnar--0.1--0.2.sql`).
Each increment of section 16 that changes the SQL interface bumps the version.

Because the resource manager is registered at preload, a server that has ever
written columnar WAL needs `kingres_columnar` in `shared_preload_libraries` for
as long as that WAL exists (the 18 documentation: "Otherwise PostgreSQL will not
be able to apply or decode the custom WAL records, which may prevent the server
from starting"). The package default configuration lists `kingres_shard`,
`kingres_ledger` and now `kingres_columnar`; `kingresd` checks that the library
is loaded on a node before it offers the `columnar` format and answers
`extension_missing` (section 13) otherwise.

### 4.2 How an extension gets onto every node of a database

`CREATE EXTENSION` is local to a node and a database. The control plane already
creates "the extensions each database uses" on a new server (control-plane-3,
decision D4 step 3) and on database creation. This part adds the two names to
that list and to the database settings (`database.create`, `database.update`
tasks): the option "columnar storage" and the option "time partitioning" each
produce one step per server that runs `CREATE EXTENSION` in the database (and,
for columnar, checks that the library is preloaded and restarts nothing: if it is
not, the step fails with a message naming the setting and the server). The
`kingres_shard.add_node` function creates only the sharding extension on the new
node, so for a database that has columnar tables the control plane's
add-server task creates `kingres_columnar` on the new node **before** it calls
`add_node`, because `add_node` creates the tables there, and a table `USING
columnar` needs the access method (section 9.1). **[Needs verification]** that
`add_node` creates tables from a definition that names the access method; today
the code does not mention it (section 20, finding 3).

### 4.3 SQL interface of `kingres_columnar` 0.1

Access method: `columnar`, handler `kingres.columnar_handler(internal) returns
table_am_handler`. Use: `CREATE TABLE t (...) USING columnar`, `CREATE TABLE ...
USING columnar AS SELECT`, `ALTER TABLE t SET ACCESS METHOD columnar` (PostgreSQL's
own command; rewrites the table under `ACCESS EXCLUSIVE`, `tablecmds.c` line 4650
`AT_SetAccessMethod: must rewrite heap`). On a partitioned table `USING columnar`
or `SET ACCESS METHOD columnar` sets the access method of partitions created
afterwards (`tablecmds.c` lines 1055 to 1066: a partition takes the parent's
access method; `ATExecSetAccessMethodNoStorage` is a catalog-only change for a
partitioned table). The name `columnar` is a **[Recommendation]**; section 17.

Functions (all `SET search_path = pg_catalog, kingres, pg_temp`, as the existing
extensions do; `EXECUTE` revoked from `PUBLIC`):

| Signature | Meaning |
|---|---|
| `kingres.columnar_set_options(rel regclass, compression text default null, compression_level int default null, stripe_row_count int default null, chunk_row_count int default null) returns void` | Change the options used for stripes written from now on. A `null` argument keeps the current value. Existing data is not rewritten. Needs ownership of the table. Not transactional (like a sequence): a rolled-back call stays applied (section 5.1: the options are in the metapage). Error on a table that is not columnar |
| `kingres.columnar_options(rel regclass) returns table (compression text, compression_level int, stripe_row_count int, chunk_row_count int)` | The options in force |
| `kingres.columnar_info(rel regclass) returns table (stripes bigint, chunk_groups bigint, rows bigint, bytes_on_disk bigint, bytes_uncompressed bigint, compression_ratio float8)` | Totals from the stripe catalog; `compression_ratio` is `bytes_uncompressed / bytes_on_disk`. A table with no data returns zeros and a null ratio |
| `kingres.columnar_stripes(rel regclass) returns table (stripe_id bigint, first_row bigint, row_count int, chunk_groups int, bytes_on_disk bigint, bytes_uncompressed bigint, written_by_xmin xid)` | One row per visible stripe. No min/max values (section 12) |
| `kingres.columnar_compact(rel regclass, min_rows int default null) returns int` | Merge stripes with fewer rows than `min_rows` (default `stripe_row_count / 4`) into full stripes; returns the number of stripes written. Needs `SHARE UPDATE EXCLUSIVE`; works from increment C4 |
| `kingres.columnar_check(fix boolean default false) returns table (check_name text, relation regclass, detail text)` | Reports and, with `fix`, repairs: catalog rows whose relation file no longer exists, stripes that overrun the file, page headers that disagree with the catalog. Superuser |

Settings (`kingres_columnar.*`):

| GUC | Default | Meaning |
|---|---|---|
| `kingres_columnar.compression` | `auto` | Codec of a new table: `none`, `pglz`, `lz4`, `zstd`, or `auto` (`zstd` if built in, else `lz4`, else `pglz`). Stored in the metapage of a table when it is created |
| `kingres_columnar.compression_level` | 3 | Level for `zstd` (1 to 19) and `lz4` (ignored: the fast mode is used); ignored by the others |
| `kingres_columnar.stripe_row_count` | 150000 | Rows of a full stripe (1000 to 10 000 000) |
| `kingres_columnar.chunk_row_count` | 10000 | Rows of a chunk group (100 to 100 000; at most the stripe size) |
| `kingres_columnar.write_buffer_memory` | 128MB | Memory of one backend for stripes being built, all tables together. When it is exceeded the largest buffer is flushed as a smaller stripe |
| `kingres_columnar.enable_custom_scan` | on | Use the "Columnar Scan" custom scan (projection, skipping). With it off, scans decode every column and skip nothing |
| `kingres_columnar.enable_chunk_skipping` | on | Use min/max statistics to skip stripes and chunk groups |
| `kingres_columnar.analyze_sample_stripes` | 0 | Reserved; 0 means the block-based sampling of section 7.9 |

The defaults 150 000 rows per stripe and 10 000 rows per chunk group are the
documented defaults of Citus's columnar tables (`stripe_row_count` 150 000 and
`chunk_row_count` 10 000 in its `alter_columnar_table_set`), taken as starting
points, not as a result. **[Recommendation]** to tune them with a benchmark in
increment C3 (section 16) before the first release.

### 4.4 SQL interface of `kingres_timepart` 0.1

Catalog tables (schema `kingres`, owned by the extension owner, no privileges
for anyone else; dumped with `pg_extension_config_dump` so a logical backup
carries policies; the functions below are the interface):

```sql
kingres.timepart_policy (
  schema_name     name        not null,
  table_name      name        not null,
  parent_oid      oid         not null,   -- a hint, refreshed on rename (10.2)
  partition_every interval    not null,   -- width of one partition, see 10.3
  timezone        text        not null default 'UTC',
  create_ahead    interval    not null default '7 days',
  retention       interval,               -- null: keep everything
  retention_action text       not null default 'drop' check (retention_action in ('drop','detach')),
  columnar_after  interval,               -- null: never convert
  columnar_sort   text[],                 -- columns to order by when converting; null: the partition column
  columnar_options jsonb,                 -- {"compression":"zstd","compression_level":3,...} or null
  run_every       interval    not null default '1 hour',
  enabled         boolean     not null default true,
  next_run_at     timestamptz not null default now(),
  created_by      name        not null,
  created_at      timestamptz not null default now(),
  updated_at      timestamptz not null default now(),
  primary key (schema_name, table_name),
  check (columnar_after is null or retention is null or columnar_after <= retention)
)

kingres.timepart_run (
  run_id      bigint generated always as identity primary key,
  schema_name name not null, table_name name not null,
  as_of       timestamptz not null,
  trigger     text not null check (trigger in ('schedule','manual','sql')),
  state       text not null check (state in ('running','done','partial','failed')),
  started_at  timestamptz not null default now(),
  finished_at timestamptz,
  started_by  name not null,
  error       text
)

kingres.timepart_run_action (
  run_id bigint not null references kingres.timepart_run on delete cascade,
  seq    int not null,
  kind   text not null check (kind in ('create','convert','drop','detach')),
  partition_schema name not null, partition_name name not null,
  range_from timestamptz, range_to timestamptz,
  state  text not null check (state in ('running','done','failed','skipped')),
  detail text,
  bytes_before bigint, bytes_after bigint,
  started_at timestamptz not null default now(), finished_at timestamptz,
  primary key (run_id, seq)
)
```

Runs are kept for 90 days and at most 1000 per table (`kingres.timepart_prune_runs`,
called at the end of every run). Policies are node-local rows: a plain table lives
on one node (README of `kingres_shard`: a plain table has a home shard group), and
its policy is on the same node. The policy of a distributed table is on the map group primary (section 11.2).

Functions and procedures:

| Signature | Meaning |
|---|---|
| `kingres.create_time_partitions(rel regclass, partition_every interval, end_at timestamptz, start_from timestamptz default now(), access_method name default null, timezone text default 'UTC') returns int` | Create, as separate tables attached with `ATTACH PARTITION`, every missing partition from the one that contains `start_from` to the one that contains `end_at` (inclusive, as `create_time_partitions` of Citus documents: "The last partition will contain the point end_at, and no later partitions will be created"). Returns the number created; 0 when all exist. `access_method` null means the access method of the parent. Function (one transaction) |
| `kingres.drop_old_time_partitions(rel regclass, older_than timestamptz, detach_only boolean default false) returns int` | Drop (or only detach) the partitions whose upper bound is `<= older_than`. Returns the number. Function; the lock behaviour is in 10.5 |
| `CALL kingres.convert_partition(parent regclass, part regclass, access_method name default 'columnar', sort_by text[] default null, options jsonb default null)` | Replace one partition by a copy in another access method (section 10.4). Procedure: commits between its internal steps, so it cannot run inside a transaction block |
| `CALL kingres.convert_old_partitions(parent regclass, older_than timestamptz, access_method name default 'columnar', sort_by text[] default null, options jsonb default null)` | `convert_partition` for every partition whose upper bound is `<= older_than` and whose access method differs; oldest first |
| `kingres.timepart_set_policy(rel regclass, partition_every interval, create_ahead interval default '7 days', retention interval default null, retention_action text default 'drop', columnar_after interval default null, columnar_sort text[] default null, columnar_options jsonb default null, timezone text default 'UTC', run_every interval default '1 hour', enabled boolean default true) returns void` | Create or replace the policy of a table. Validates the table (10.2) and the intervals (10.3). Does not create partitions; the next run does. Needs ownership |
| `kingres.timepart_unset_policy(rel regclass) returns void` | Remove the policy. History is kept |
| `kingres.timepart_plan(rel regclass, as_of timestamptz default now()) returns table (seq int, kind text, partition_name name, range_from timestamptz, range_to timestamptz, reason text)` | The actions a run would take at `as_of`, without taking any. Pure function of the catalog state and `as_of` |
| `kingres.timepart_begin_run(rel regclass, as_of timestamptz default now(), trigger text default 'sql') returns bigint` | Record a run in state `running`; takes the per-table advisory lock (10.7) and returns the run id, or raises `55P03` when another run of the table is live |
| `CALL kingres.timepart_run_phase(run_id bigint, phase text)` | Execute one phase of a run: `create`, `convert`, `retire` (drop or detach). Each phase re-plans from the current catalog state at the run's `as_of`, so it is idempotent: running it again after an interruption does what is left |
| `kingres.timepart_finish_run(run_id bigint) returns void` | Compute the final state (`done`, `partial` when some action failed, `failed` when the run itself failed), set `next_run_at = now() + run_every`, or `now() + least(run_every, 5 minutes)` when an action was `skipped` with `retry_later` (a refusal because a move or operation held the colocation group, 11.2), prune old runs |
| `CALL kingres.timepart_run(rel regclass, as_of timestamptz default now(), phases text[] default '{create,convert,retire}')` | All of the above in sequence, for use from `psql`. The scheduler of section 10.6 calls the pieces separately so each is a visible task step |
| `kingres.timepart_due(as_of timestamptz default now()) returns table (schema_name name, table_name name, as_of timestamptz)` | Enabled policies whose `next_run_at <= as_of`, oldest first. The scheduler's query |
| `kingres.timepart_partitions(rel regclass) returns table (partition regclass, range_from timestamptz, range_to timestamptz, access_method name, size_bytes bigint, row_estimate bigint, compression_ratio float8, attached boolean)` | Partitions of the table with bounds, size, format. The console's list. `compression_ratio` is null for heap |
| `kingres.timepart_check(min_ahead interval default null) returns table (schema_name name, table_name name, problem text, detail text)` | `ahead_low` (the last partition ends before `now() + min_ahead`, default `create_ahead / 2`), `last_run_failed`, `policy_orphan` (the table no longer exists or is no longer partitioned), `default_partition_present`. Feeds the alerts of section 13 |

View `kingres.timepart_policies`: the policy, `next_run_at`, and the last run's
state, joined. A database without `kingres_columnar` can still use every function
except the `convert_*` ones, which fail with a message that names the missing
extension.

## 5. The columnar format

### 5.1 Where the data lives

The data of a columnar table is in the main fork of its relation, in 8 kB pages
of the standard layout, read and written through the shared buffer cache. No
other fork is used (no free space map, no visibility map). This gives, with no
extra code: buffer caching and eviction, page checksums when the cluster has them
(`PageSetChecksum` at write-out needs a standard `PageHeader`), `pg_basebackup`,
`pg_upgrade` (the files are copied; section 7.6) and the existing
`pg_relation_size` family (`relation_size` callback). The relation is
`RELPERSISTENCE_PERMANENT` only: `relation_set_new_filelocator` raises an error
for an unlogged or temporary table (the Citus documentation lists "No UNLOGGED
columnar tables" as a limit too; here temporary tables are also refused to keep
the first version small).

```
block 0           metapage
blocks 1 ..       stripes, one after the other; a stripe is a run of consecutive blocks
```

**Metapage** (block 0; a standard page with the payload in the usable area):
magic, format version (1), the options (codec, level, stripe and chunk row
counts: so options travel with the relation file through `relation_copy_data`,
rewrites and base backups, and need no catalog row; section 4.3 says what this
means for dumps), `next_row` (the next row number, 64 bits), `next_stripe_id`
(64 bits), and a reserved area. It is updated under an exclusive buffer lock and
logged with a small record (section 7.3). It is the only page that is ever
modified in place after it is written; every other page is written once.

**Stripe pages.** Every page of a stripe has a standard `PageHeader` and a
special area of 24 bytes: a 16-bit magic, 16 flag bits, the 32-bit page number
within the stripe, the 32-bit page count of the stripe, the 64-bit stripe id and
the 32-bit transaction id of the writer. The payload is what is between the
header and the special area, about 8 144 bytes (8192 minus the 24-byte header
minus 24; the arithmetic is the design's, not a measurement). `pd_lower` is the
start and `pd_upper` the end of the payload, so there is no "hole" and the page
is always logged as a full image (section 7.3).

### 5.2 Stripes, chunk groups, column chunks

- A **stripe** is the unit of writing, of visibility, of catalog metadata and of
  parallel scan. A full stripe holds `stripe_row_count` rows. The rows of a
  stripe get consecutive **row numbers** starting at `first_row`.
- A stripe is a **blob** laid out over its pages: a fixed header, then the
  **chunk groups** one after the other, then the **footer**.
- A **chunk group** holds up to `chunk_row_count` consecutive rows and contains,
  for every column, one **column chunk**: the values of that column for those
  rows. Skipping works at chunk-group level (a group is skipped as a whole) and
  projection works at column level (the chunks of a column that is not needed
  are never read from the buffer cache: their pages are not touched, except for
  pages shared with a neighbouring chunk).
- The blob is built entirely in memory by the writer (section 7.2) and written
  in one pass, so the layout can put the footer last.

**Column chunk encoding** (before compression):

1. If any value is null, a null bitmap of `ceil(rows / 8)` bytes (bit set means
   not null); otherwise none (a flag in the footer says which).
2. The non-null values, densely:
   - fixed-length by-value types (`typlen` 1, 2, 4, 8): the values in the byte
     order of the server, as `Datum` bytes of that length;
   - fixed-length by-reference types (`uuid`, `macaddr`, `timetz`, ...): the
     bytes;
   - varlena types: an array of 32-bit lengths of the non-null values, then the
     value bytes. Values are first detoasted and decompressed (a value that is an
     external TOAST pointer or an expanded object is flattened with the normal
     detoast functions), and stored without the varlena header, which is rebuilt
     with a 4-byte header on read.
3. The whole buffer is compressed as one unit with the column's codec. If the
   result is not smaller than the input the chunk is stored with codec `none`.
   The footer records the codec of every chunk, so options can change over time.

No other encoding (run length, delta, dictionary) in the first version; the
codecs give most of the gain for the data that matters here (time series with
low-cardinality and slowly changing columns compress well with `zstd`). Types
with `typlen = -2` (C strings) cannot be column types of a table and are not
considered. The byte order makes the file not portable between machines of
different endianness, as for heap.

**Footer.** For each chunk group: the row count; for each column: the offset and
the compressed and raw lengths of its chunk (offsets are bytes inside the blob),
the codec, the number of nulls, a flag "has min/max", and the minimum and maximum
as serialized datums (`datumSerialize`/`datumRestore` of `utils/datum.h`). A CRC
32C of the footer is stored in the stripe header. The footer is what a scan reads
first for a stripe it did not skip.

**Min/max rule.** Statistics are stored for a column chunk only if (a) the type
has a default B-tree operator class (`GetDefaultOpClass`) and (b) the type is not
collatable, or the column's collation is `C`, `POSIX` or `ucs_basic`, and (c) the
serialized minimum and maximum are each at most 64 bytes (a `numeric` or a short
`text` fit; a long text value turns the statistic off for that chunk group). The
reason for (b): an ordering that depends on `libc` or ICU can change when the
library is upgraded (the same hazard `kingres_shard` guards against when it
requires agreement on collation versions), and a stale statistic would skip
rows that match. A comparison on a collatable column with another collation is
never used for skipping. Floating-point statistics use the B-tree comparison, so
`NaN` sorts above every number as in an index.

**Stripe statistics in the catalog.** The same minimum, maximum and null count
are also stored per column for the **whole stripe**, in the stripe's catalog row
(`col_stats`), so that a scan can skip a whole stripe from the catalog row alone,
without reading any page of the stripe.

### 5.3 Row numbers and TIDs

Table access method rows are identified by a TID of a block number and an item
number (18 documentation, "Table Access Method Interface Definition": "if the AM
wants to support modifications and/or indexes, it is necessary for each tuple to
have a tuple identifier (TID) consisting of a block number and an item number").
A row with row number `n` has the TID `(n / 2048, n % 2048 + 1)`. The divisor
2048 is `MaxOffsetNumber` for 8 kB pages (`off.h`: `BLCKSZ / sizeof(ItemIdData)`),
so every item number is valid in `OffsetNumberIsValid`. The block number is a
virtual block, unrelated to the file, and the bound is `2^32 * 2048`, about
8.8 x 10^12 rows per relation. In the first version nothing but the scan itself
produces a TID (there are no indexes, no `UPDATE`, no `ctid` projection); the
mapping is fixed now so that the file format does not change when increment C6
adds indexes.

### 5.4 The stripe catalog

```sql
kingres.columnar_stripe (
  filenode      oid      not null,   -- relfilenode of the relation the stripe belongs to
  stripe_id     bigint   not null,
  first_row     bigint   not null,
  row_count     int      not null,
  chunk_groups  int      not null,
  natts         smallint not null,   -- columns the table had when the stripe was written
  first_block   bigint   not null,
  block_count   int      not null,
  blob_bytes    bigint   not null,   -- uncompressed bytes of all chunks
  footer_offset int      not null,
  cid_points    bytea    not null,   -- 7.1: (row index, command id) change points
  col_stats     bytea    not null,   -- stripe-level min/max/null count per column
  primary key (filenode, stripe_id),
  unique (filenode, first_row)
)
```

It is an ordinary heap table with two indexes, in the extension's schema, with
no privileges granted to anyone except the extension owner. The extension reads
and writes it with the heap and index APIs, which do not check privileges, as
`kingres_shard` does for `dtx_log` and the cluster secret. Its rows carry the
transaction id of the writer, so MVCC applies to stripes as to rows of any table.

**Keyed by relfilenode, not by relation id.** `VACUUM FULL`, `CLUSTER` and the
rewrite of `ALTER TABLE` build a new relation, copy into it, and swap the two
relations' files (`finish_heap_swap`); the table keeps its OID and gets the new
file. A catalog keyed by the OID would describe the wrong file after the swap. A
catalog keyed by the file number is right by construction: the rows written for
the new file become the rows of the old table. The same holds for `TRUNCATE`
(a new file; section 6) and `ALTER TABLE ... SET TABLESPACE` (the copy is
re-keyed by `relation_copy_data`).

### 5.5 What cleans the catalog

A table access method has no callback at relation drop (`tableam.h`). The catalog
rows of a dropped table, and of the temporary relation of a rewrite, are removed
by reconciliation: delete the stripe rows whose `filenode` is not the current
`pg_class.relfilenode` of any relation (a join on `pg_class` with
`pg_relation_filenode`). It runs (a) in a `sql_drop` event trigger of the
extension after every `DROP TABLE`, `DROP SCHEMA` and `DROP EXTENSION`'s cascade,
(b) at the end of `relation_vacuum` of any columnar table, (c) in
`kingres.columnar_check(fix => true)`. **[Needs verification]** that the
`sql_drop` event fires for the internal drop of the temporary relation of a
rewrite (`performDeletion` with `PERFORM_DELETION_INTERNAL`); if it does not,
(b) and (c) are the only cleanup, and rows of a rewritten table's old file stay
until the next vacuum of any columnar table. They are small and harmless.

## 6. The table access method: what is implemented and what is refused

`GetTableAmRoutine` asserts that a fixed set of callbacks is non-null
(`tableamapi.c` lines 45 to 95); optional ones are `scan_set_tidrange` and
`scan_getnextslot_tidrange` (both or neither: `tableam.h` lines 353 to 369),
`scan_bitmap_next_tuple` ("This is an optional callback") and the toast
callbacks. A refused callback is a function that raises
`ERRCODE_FEATURE_NOT_SUPPORTED` with the text in the table.

| Callback | Version 0.1 | Notes |
|---|---|---|
| `slot_callbacks` | `TTSOpsVirtual` | Rows are produced as virtual slots filled from decoded vectors. `ctid` and other system columns are not available (a `SELECT ctid` fails with PostgreSQL's own message for virtual slots **[Needs verification]** of the exact text) |
| `scan_begin`, `scan_end`, `scan_rescan`, `scan_getnextslot` | Implemented | Serial scan of all visible stripes in `first_row` order, all columns decoded. Used by `ANALYZE`, `COPY TO`, table rewrites, logical replication's initial copy, and any scan the custom scan of section 8 does not replace. Any snapshot: the stripe catalog is read with the scan's snapshot (7.1); for `SnapshotAny` a stripe counts when its writer committed or is the current transaction |
| `parallelscan_estimate`, `_initialize`, `_reinitialize` | Implemented in C5; in 0.1 the callbacks report a size and the planner gets no parallel path (`consider_parallel` is false for the relation by the custom path) | Section 8.4 |
| `scan_set_tidrange`, `scan_getnextslot_tidrange` | Not provided (both null) | **[Needs verification]** that the planner then makes no TID range path for the relation |
| `scan_bitmap_next_tuple` | Not provided | Bitmap scans need index support; the 18 documentation adds "the block number needs to provide locality" for bitmap scans |
| `scan_sample_next_block`, `scan_sample_next_tuple` | Refused: "TABLESAMPLE is not supported on columnar tables" | Required non-null by the assertion |
| `index_fetch_begin`, `_reset`, `_end`, `index_fetch_tuple`, `tuple_fetch_row_version`, `tuple_tid_valid`, `tuple_get_latest_tid` | `index_fetch_begin` and `tuple_fetch_row_version` refuse ("columnar tables have no indexes"); the others are trivial | C6 implements them (lookup of a row number in the stripe catalog, decode of the chunk, a one-chunk cache per fetch) |
| `tuple_satisfies_snapshot` | Implemented | A row satisfies a snapshot if its stripe does (7.1) |
| `index_delete_tuples` | Refused | Needed only for index maintenance |
| `tuple_insert` | Implemented | Appends to the transaction's write buffer for the relation (7.2). Honors `TABLE_INSERT_NO_LOGICAL` from C7 on |
| `tuple_insert_speculative`, `tuple_complete_speculative` | Refused: "INSERT ... ON CONFLICT DO UPDATE is not supported on columnar tables" | Speculative insertion exists for unique-index arbitration. `ON CONFLICT DO NOTHING` with no conflict target needs no arbiter and works |
| `multi_insert` | Implemented | The `COPY` path, no per-row heap tuple formed |
| `tuple_delete`, `tuple_update` | Refused: "columnar tables are append-only: UPDATE and DELETE are not supported" | 7.7 |
| `tuple_lock` | Refused: "columnar tables do not support row locks" | `SELECT ... FOR UPDATE/SHARE`, and foreign-key checks that lock |
| `finish_bulk_insert` | Flushes the write buffer of the relation when its file was created in this transaction (7.2) | |
| `relation_set_new_filelocator` | Implemented | Creates the storage, writes the metapage (WAL-logged unless `wal_level = minimal` and the file is new in this transaction), deletes the stripe rows of the old file (rolled back with the transaction on abort), discards any write buffer of the relation, sets `*freezeXid = InvalidTransactionId` and `*minmulti = InvalidMultiXactId`, which `tableam.h` allows for AMs that do not need them (the data pages hold no row transaction ids; the catalog table has its own freezing by its own vacuum) |
| `relation_nontransactional_truncate` | Implemented | Reset to an empty metapage, delete the stripe rows |
| `relation_copy_data` | Implemented | Copies the main fork file as it is (all content is in standard pages), re-keys the stripe rows to the new file number (`SET TABLESPACE`) |
| `relation_copy_for_cluster` | Implemented for `VACUUM FULL` (no index, no sort); `CLUSTER` is refused ("CLUSTER needs an index; columnar tables have none in this version") | Reads the visible rows in `first_row` order and writes full stripes into the new relation; the swap makes its catalog rows the table's |
| `relation_vacuum` | Implemented (section 7.5) | |
| `scan_analyze_next_block`, `scan_analyze_next_tuple` | Implemented (section 7.9) | |
| `index_build_range_scan`, `index_validate_scan` | Refused: "indexes on columnar tables are not supported in this version" in 0.1; implemented in C6 (`CREATE INDEX CONCURRENTLY` stays refused) | The assertion says these cannot be made optional ("would require throwing error during parse-analysis") |
| `relation_size` | Implemented | Number of blocks times `BLCKSZ` of the main fork; other forks 0 |
| `relation_needs_toast_table` | `false` | Values are stored inline in chunks |
| `relation_toast_am`, `relation_fetch_toast_slice` | Not provided | |
| `relation_estimate_size` | Implemented | Pages from the file; tuples from `pg_class.reltuples` scaled by `pages / relpages` as `table_block_relation_estimate_size` does; with no statistics yet, from the stripe catalog's row sum (an index scan on a catalog of at most a few ten thousand rows). Estimating from tuple width would be wrong because the file holds compressed bytes |

Statistics counters: the insert path calls `pgstat_count_heap_insert` so that
autovacuum's insert-driven vacuum and analyze thresholds see the table (an
insert-only table is vacuumed and analysed because of inserts since PostgreSQL
13; **[Needs verification]** that the counters of a table AM feed
`n_ins_since_vacuum` the same way).

Further refusals, outside the callbacks:

- A foreign key that references a columnar table, or is defined on one, is
  refused at `ALTER TABLE ... ADD FOREIGN KEY` and `CREATE TABLE` by an event
  trigger (`ddl_command_end`, `ENABLE ALWAYS`). The referencing side would need
  `SELECT ... FOR KEY SHARE` (a row lock); the referenced side needs a unique
  index. The Citus documentation lists the same limit ("No support for foreign
  keys").
- `CLUSTER`, `CREATE INDEX`, `ALTER TABLE ... ADD PRIMARY KEY/UNIQUE`,
  `TABLESAMPLE`, `UNLOGGED`, `TEMPORARY`, `REFRESH MATERIALIZED VIEW CONCURRENTLY`
  of a columnar materialized view, `ALTER TABLE ... SET (fillfactor ...)` have no
  effect or are refused by the callbacks above.
- Row-level triggers `BEFORE INSERT` and `AFTER INSERT`, `INSERT ... RETURNING`,
  row security on `SELECT` and `INSERT`, generated columns, defaults, `CHECK`
  constraints, `NOT NULL` and `CREATE TABLE AS` all work: they are executor-level.
  `UPDATE` and `DELETE` triggers can be created and never fire.

## 7. Transactions, visibility, WAL, replication, vacuum

### 7.1 Visibility

A row of a columnar table is visible to a snapshot exactly when its stripe is.
A stripe is visible when its row in `kingres.columnar_stripe` is visible to that
snapshot under the ordinary heap rules (`HeapTupleSatisfiesVisibility`). So a
stripe whose writer has not committed, or has aborted, is invisible to everyone
else; one committed before the snapshot is visible; read-committed, repeatable
read and serializable behave as for any table; a standby sees exactly the stripes
whose catalog rows it has replayed and committed.

One rule is needed for the writer's own transaction, which must see its own rows
but not the rows of the command that is running (the "Halloween problem": `INSERT
INTO t SELECT * FROM t` must not read its own output). Heap handles it with the
command id of every tuple. Here:

- The writer's stripes are inserted into the catalog at **flush** time, which can
  be many commands after the rows were written (7.2). The catalog row's own
  `cmin` is therefore the flush command, not the writing command, and the heap
  rule would hide rows that earlier commands wrote.
- So the stripe row carries `cid_points`: the list of `(row index within the
  stripe, command id)` at which the command id changed while the buffer was
  filled. Rows are in insertion order, so the rows visible to a scan of command
  `c` of the same transaction are a **prefix** of the stripe: those whose command
  id is below `c`. The scan stops after that prefix.
- The catalog scan for such a stripe uses a copy of the scan's snapshot with
  `curcid` raised so that the transaction's own catalog rows are all seen
  (`TransactionIdIsCurrentTransactionId` still says which are its own); the
  prefix rule then decides how much of each own stripe the scan reads. For any
  other transaction's stripe the copy changes nothing.
- A scan of the same transaction flushes the transaction's buffers for that
  relation when it starts (the leader, before a parallel scan starts workers), so
  the rows are in a stripe and the rule above applies. This is a flush inside a
  read, in the writer's own transaction; it is never done by a parallel worker or
  a standby.
- `TRUNCATE` of the table in the same transaction deletes the old file's stripe
  rows at the command that ran it; a cursor opened before the `TRUNCATE` in the
  same transaction then sees no rows. PostgreSQL documents `TRUNCATE` as not
  MVCC-safe for concurrent readers; this is the same boundary.

### 7.2 The write path

1. `tuple_insert` and `multi_insert` append the row to the **write state** of the
   backend for that relation (a hash table in a dedicated memory context):
   per-column builders for the current chunk group, the compressed chunk groups of
   the stripe so far, and the current command id. Values are detoasted and
   flattened as they are added.
2. When a chunk group reaches `chunk_row_count` rows, each column chunk is encoded
   and compressed, its min/max computed, and appended to the stripe buffer.
3. When the stripe reaches `stripe_row_count` rows, or the total memory of all
   buffers of the backend exceeds `kingres_columnar.write_buffer_memory` (the
   largest buffer is flushed first), the stripe is **flushed**:
   a. take the metapage buffer exclusively, read and advance `next_row` and
      `next_stripe_id`, log the change (record `COLUMNAR_ALLOC`), release;
   b. extend the relation by the stripe's page count while holding the relation
      extension lock for the whole extension, so that the pages of the stripe are
      contiguous (`LockRelationForExtension`; `ExtendBufferedRelBy` takes the same
      lock and the lock manager is re-entrant; **[Needs verification]** that
      holding it across several calls keeps other backends from extending in
      between; if not, the catalog row stores an extent list instead of one
      `first_block`/`block_count`);
   c. fill the pages (using a bulk-write buffer ring so one load does not flush
      the cache), and log them: one `COLUMNAR_PAGES` record per up to 32 pages
      (`XLR_MAX_BLOCK_ID` is 32), each page registered with
      `REGBUF_FORCE_IMAGE` and no `REGBUF_STANDARD`, so the record carries the
      whole page (7.3). With `wal_level = minimal` and a relation file created
      in this transaction, `RelationNeedsWAL` is false and nothing is logged;
      PostgreSQL's pending-sync mechanism at commit makes the file durable, as it
      does for heap;
   d. insert the stripe's row into `kingres.columnar_stripe` (a normal heap
      insert, logged by the heap's rmgr).
4. At the end of the transaction (`XACT_EVENT_PRE_COMMIT`, and
   `XACT_EVENT_PRE_PREPARE` for `PREPARE TRANSACTION`, which the two-phase
   commit of `kingres_shard` uses) every remaining buffer is flushed, partial
   stripes included. On abort the buffers are discarded. A relation that was
   dropped in the transaction has its buffer discarded.
5. `finish_bulk_insert` flushes the buffer of a relation whose file was created in
   this transaction (`rd_createSubid` or `rd_firstRelfilelocatorSubid` is set), so
   that the table rewrite of `ALTER TABLE ... SET ACCESS METHOD` has its rows in
   stripes before `finish_heap_swap` swaps the files, which happens before
   pre-commit. For any other relation it does nothing, so a transaction that
   issues many `COPY` or batched `INSERT` commands (the row router of phase 2b
   sends batches of 1000 rows in one remote transaction) fills full stripes, not
   one small stripe per command.
6. **Subtransactions.** A write belongs to the subtransaction nesting level that
   made it. The write state keeps a stack of buffers by level: a write at a new
   level pushes a new buffer. At subtransaction commit (`SUBXACT_EVENT_PRE_COMMIT_SUB`)
   the level's buffer is flushed in the subtransaction (its stripe row carries
   the subtransaction's xid, so an abort of a parent still rolls it back); at
   subtransaction abort the level's buffer is discarded. The cost is a small
   stripe per savepoint that wrote, which is documented (7.5 compacts them).

Memory is bounded by `write_buffer_memory` per backend, whatever the number of
partitions a `COPY` into a partitioned table touches. Rows buffered at a
transaction's end are the only rows that exist in no WAL: a crash loses them
with the transaction, as for any uncommitted change.

### 7.3 WAL and crash safety

**Resource manager** `KingresColumnar`, id `RM_EXPERIMENTAL_ID` (128) during
development and an id reserved on the registry named by the 18 documentation
before the first release (**[Recommendation]**; the id is part of the WAL format
forever). Records:

| Record | Content | Redo |
|---|---|---|
| `COLUMNAR_INIT` | metapage image | restore the image |
| `COLUMNAR_ALLOC` | new `next_row` and `next_stripe_id`, the page | set the two values if the page's LSN is older than the record (`XLogReadBufferForRedo` returns `BLK_NEEDS_REDO`) |
| `COLUMNAR_OPTIONS` | new options, the page | same |
| `COLUMNAR_PAGES` | up to 32 blocks, each with a full-page image | `XLogReadBufferForRedo` for each block restores the image (`BLK_RESTORED`) and releases the buffer; no other logic. **[Needs verification]** that a restored image beyond the end of the file extends it in redo (`XLogReadBufferForRedoExtended` zero-extends for image blocks; test: `test_custom_rmgrs` in `src/test/modules` is the reference module) |
| `COLUMNAR_LOGICAL_INSERT` (C7) | the rows of one stripe in row form, for decoding | none (it carries no page) |

Because the redo of the page records is a restore of images, there is no redo
logic to get wrong and `wal_consistency_checking` can be enabled for the
resource manager with an `rm_mask` that masks only the page LSN and checksum.
`rm_desc` and `rm_identify` give `pg_waldump` readable output.

**Crash safety at each step of a flush.**

| Crash after | State on restart |
|---|---|
| Counter advanced, nothing else | Row numbers skipped. Harmless: row numbers need not be dense |
| Pages logged (and perhaps written), catalog row not inserted | The pages are in the file and belong to no stripe. Invisible, because visibility comes from the catalog row. They stay as unreferenced space (7.5, increment C8 reuses it) |
| Catalog row inserted, transaction not committed | The row's writer is not committed: crash recovery treats the transaction as aborted; the row is invisible; same as the line above |
| Commit record in the WAL | Replay restores every page image and the catalog row; the stripe is visible. `synchronous_commit` and synchronous standbys apply to the commit record as for any transaction, and the pages precede it in the WAL |
| `PREPARE TRANSACTION` done, then crash | The prepared transaction's catalog rows and pages are recovered with it; `COMMIT PREPARED` makes the stripes visible |

A torn page cannot happen to a stripe page: it is written once, logged as a full
image, and if `full_page_writes` is off the image is still in the record because
the record's blocks are registered with `REGBUF_FORCE_IMAGE`. The metapage is
logged with a record that redo applies to a page it first read; with
`full_page_writes` on, the first change after a checkpoint also carries the
image.

### 7.4 Replication and standbys

The streaming standby replays the same records, so its files are identical and
it can serve reads (hot standby): the scan reads the catalog and the pages
through its own buffer cache, and never writes (no hint bits, no flush: section
7.1 says a flush is only done in a writing transaction). The library must be
preloaded on every standby and every node that restores a base backup and replays
WAL (7.3 and the 18 documentation). Failover (phase 5) promotes a standby with
nothing to rebuild: write states are per backend and die with the old primary.
`pg_rewind` reads block references of records generically **[Needs
verification]** for records of a custom resource manager. Synchronous replication
needs no special treatment: stripes are ordinary WAL.

Logical replication of columnar tables is section 9.3. Until C7 a columnar table
cannot be decoded and the decoder, which ignores a resource manager without
`rm_decode`, would drop its changes silently; section 9.3 explains how that is
prevented.

### 7.5 `VACUUM`, `ANALYZE`, autovacuum, space

`relation_vacuum` runs for `VACUUM` and autovacuum. In 0.1 (increment C4):

1. Update `pg_class` through `vac_update_relstats`: `relpages` from the file,
   `reltuples` from the stripe catalog's row sum, `relallvisible` 0.
2. Truncate the unreferenced tail of the file: the highest block that belongs to
   a stripe with a live catalog row is found from the catalog; blocks after it
   are truncated (`RelationTruncate`) if a conditional `ACCESS EXCLUSIVE` lock is
   obtained (as `lazy_truncate_heap` does), which on a standby is a WAL-logged
   lock that cancels conflicting queries.
3. Reconcile the catalog (5.5).
4. `kingres.columnar_compact` is not run automatically by `VACUUM`; it is
   called by hand or by the time-partition policy after a conversion.

What a heap vacuum does and this does not: no tuple pruning (rows are not
dead), no index vacuum (no indexes), no freezing of rows (no row transaction ids;
`relfrozenxid` stays invalid, which autovacuum's wraparound logic skips; the
catalog table `kingres.columnar_stripe` is a heap table and is frozen by its own
autovacuum).

**Space is not reused in the middle of the file in 0.1.** Pages of aborted or
crashed writers, and of stripes replaced by compaction, stay allocated unless
they are at the end (then 7.5 step 2 returns them) or the table is rewritten
(`VACUUM FULL` is implemented and gives the space back). This is a real
limitation: a failed 10 GB load leaves 10 GB of dead pages in the middle of the
file. Increment C8 adds a free-extent list found by walking the stripe headers
(each page's special area names its stripe id and writer transaction, so a page
run whose stripe has no catalog row and whose writer is not in progress is
free). **[Recommendation]** to keep it out of the first release and to say so in
the README and the console ("space of failed loads returns after `VACUUM FULL`").

`ANALYZE` and autovacuum: see 7.9. The custom scan and the AM scan both work on
a table that is being vacuumed.

### 7.6 Backup, restore, upgrade

- `pg_basebackup`, filesystem snapshots and archive recovery copy the relation
  files and the WAL; nothing else is needed. Point-in-time recovery needs the
  library preloaded. **[Needs verification]** that the incremental backup of
  PostgreSQL 17 and 18 (`pg_basebackup --incremental`, WAL summarizer) treats
  records of a custom resource manager like any other, from their block
  references.
- `pg_dump` writes `CREATE TABLE ... USING columnar` and the rows through `COPY`;
  restore re-inserts them as new stripes in one transaction per table, which is
  efficient. The per-table options live in the metapage and are **not** in a
  dump: after a restore the table has the options of the session's settings at
  creation (`kingres_columnar.*`). The time-partition policy keeps its
  `columnar_options`, so tables created by policy get them again; for other
  tables, call `columnar_set_options` after the restore. This is a stated
  limitation (section 18).
- `pg_upgrade` copies the files; the format version in the metapage (1) is how a
  later release finds an old table. A later format change adds an upgrade step in
  `relation_vacuum` or a rewrite by `VACUUM FULL`; the first release has one
  version.

### 7.7 `UPDATE` and `DELETE`: append-only, and why

Both are refused (section 6). The reasons:

1. Heap handles them with a version chain in the table: `xmax` in each tuple,
   and a new tuple for an update. A columnar table has no per-row header, and
   adding one (an `xmin`/`xmax` pair per row) costs more space than good
   compression saves on narrow rows and defeats the encoding of a column chunk.
   The alternative, a per-stripe deletion mask, needs a visibility rule for the
   mask itself (which transaction set which bit), row locks, `ctid` chains for
   concurrent updates, and an index story. That is a feature as large as the rest
   of the extension.
2. The use the product aims at does not need it. Time series and event data are
   appended, read, and removed by partition (section 10). The Citus
   documentation lists the same limit for its columnar tables ("Append-only (no
   UPDATE/DELETE support)").
3. The sharding extension's guarantees (two-phase commit across groups, bucket
   moves by logical replication, the stale-map rule) get simpler: a columnar
   table never produces an `UPDATE` or `DELETE` change, so a publication of it
   needs `INSERT` only (9.3).
4. There is a way to change old data: `CALL kingres.convert_partition(parent,
   part, 'heap')` turns a partition back into row storage, an ordinary `UPDATE`
   or `DELETE` fixes it, and the policy (or a call) converts it again. For a
   whole table, `ALTER TABLE ... SET ACCESS METHOD heap` does the same.

A statement that would modify a columnar row fails with the message above when it
reaches the row. A statement that matches no row of a columnar partition succeeds,
which matters for statements on a partitioned table that has both formats (the
Citus documentation says the same: "On a partitioned table that mixes row and
columnar partitions, updates must target row partitions or filter out columnar
ones").

A later release could add deletes with a per-stripe deletion vector and a
`columnar_delete_rows` maintenance function; **[Recommendation]** not in phase
12.

### 7.8 Indexes

0.1 has none and refuses their creation (`index_build_range_scan` fails), so a
columnar table cannot have a primary key or a unique constraint. Consequences,
stated here because they reach the product:

- A time-partitioned table whose partitioned (parent) index exists cannot get a
  columnar partition: `ATTACH PARTITION` and the rewrite create the partition's
  matching index, and that fails. The conversion function checks and refuses with
  the name of the index (section 10.4). Until increment C6, tables meant for
  columnar partitions have no index on the parent; a time index on heap
  partitions is created per partition.
- A hash-distributed columnar table has no primary key, so the replica identity
  rule of the bucket move (section 9.3) is met with `REPLICA IDENTITY FULL`, set
  by the conversion.

Increment C6 adds B-tree indexes: `index_build_range_scan` (scan with TID),
`index_fetch_tuple` (TID to row), reservation of row numbers at insert time (the
index entry is inserted by the executor right after `table_tuple_insert`, so the
TID must exist before the stripe is flushed; the stripe's catalog row is then
inserted at the first insert and updated at flush, so that vacuum treats the
reserved range as live), and index vacuum (`index_bulk_delete` with a callback
that says a TID is dead when its row number is in no live stripe). Unique
indexes then work, with uniqueness checked against the write buffer as well.
Hash, GiST, GIN and BRIN stay refused.

### 7.9 `ANALYZE`

`acquire_sample_rows` chooses `totalblocks = RelationGetNumberOfBlocks` and a
block sample, calls `scan_analyze_next_block` per sampled block and
`scan_analyze_next_tuple` until it returns false, and estimates the table's rows
as `liverows / blocks_visited * totalblocks` (`analyze.c` lines 1221 and 1332).
So a block must stand for a share of the rows. The design: block `b` of a stripe
that has `P` pages and `R` rows yields the rows whose index within the stripe
modulo `P` equals the page number of `b` (about `R / P` rows each, a partition of
the stripe's rows over its pages); the metapage and pages of no stripe yield none
and return `false` from `scan_analyze_next_block` (allowed: "the callback can
return false if the block is not suitable for sampling, e.g. because it's a
metapage"). The block sampler visits blocks in ascending order, so consecutive
sampled pages of one stripe hit the decoded chunk cached by the scan. The
estimate of the total is then right in expectation and the sample is spread
over the whole table. **[Needs verification]** by an `ANALYZE` test that compares
`reltuples` and the histogram of a column with the true values on a table of many
stripes.

## 8. Reading: scan, pushdown, parallel, EXPLAIN

### 8.1 The Columnar Scan

`set_rel_pathlist_hook` (chained after `kingres_shard`'s hook, which calls the
previous hook first and then returns for a table it does not own; the two are
independent) adds, for a base relation whose access method is `columnar`, a
`CustomPath` "Columnar Scan" with `scanrelid` set. It is a plan node over one
relation that returns rows of that relation, so every plan above it
(joins, aggregates, the distributed scan of ADR 0022, partitionwise paths) is
unchanged. The ordinary sequential scan path stays in the path list and is
chosen when cheaper or when `enable_custom_scan` is off.

- **Projection.** The needed columns are those of `rel->reltarget` and of the
  restriction clauses (`pull_varattnos`); the others are neither read nor
  decoded; their slot values are null and never requested. A scan of a table with
  100 columns that needs 3 reads the pages of 3 column chunks per chunk group.
- **Predicate pushdown (skipping).** For each restriction of the form `col OP
  const-expression`, with `OP` in the B-tree operator family of the column
  (resolved with `get_op_btree_interpretation`, so cross-type comparisons such as
  `int4` column against `int8` constant work), `col IS NULL`, `col IS NOT NULL`,
  `col = ANY (array)`, `col IN (...)`, and `AND` and `OR` of these (an `OR` skips
  only when every arm skips; `NOT` never skips), the scan evaluates the
  constant side once at start (external parameters and stable functions such as
  `now()` are evaluated at executor start; volatile ones are not used), then
  compares it with the stripe's catalog statistics (skip the stripe), and with
  the chunk group's footer statistics (skip the group). The restrictions stay in
  the plan as filters: skipping is only an optimization, so a wrong or missing
  statistic cannot change an answer, only the speed. A comparison under a
  collation other than the column's is not used (5.2).
- **Order of work.** Read the stripe list with the snapshot; drop stripes by
  catalog statistics; for each remaining stripe read the footer; drop chunk
  groups; for each remaining group read and decompress the needed column chunks,
  produce rows.
- **Cost.** The path's cost is `startup + cpu_tuple_cost * rows * (needed / all
  columns weighted by raw bytes) + seq_page_cost * pages * (needed fraction)`,
  using `pg_class.relpages` (compressed pages). Selectivity of skipping is not
  estimated in 0.1 (the planner's own selectivity gives the output rows; the
  page estimate is the unskipped one); **[Recommendation]** to estimate it from
  the stripe statistics at plan time in a later increment, after the cost
  constants are tuned on real data (open question 10 of phase 2b has the same
  shape).
- **What is not decoded lazily.** The first version decodes a whole chunk of a
  needed column when a chunk group is entered, not on first use. Rows are
  produced one at a time into a virtual slot; there is no vectorized executor.

### 8.2 The plain table access method scan

`table_beginscan` on a columnar table (for `ANALYZE`, `COPY TO`, rewrites, the
initial copy of logical replication, `pg_dump`) decodes every column and skips
nothing. It is correct and slower; those paths are not analytic queries.

### 8.3 Partitions and the distributed scan

For a partitioned table, each partition is its own base relation with its own
access method: a heap partition gets a Seq Scan, a columnar partition a Columnar
Scan, under one Append. Partition pruning by the planner removes whole partitions
before any of this. The distributed scan of phase 2a replaces the *partitioned
table's* path by a custom scan whose child is "the best local path of the table's
partitions" (`dist_scan.c`, `ds_set_rel_pathlist`), so the columnar scans are
the local part of it; the other groups' fragments are plain SQL
(`DECLARE ... SELECT <columns> FROM <table> WHERE <restrictions>`), planned by the
other node's planner, which uses its own Columnar Scan. So **every statement that
works on a heap distributed table works on a columnar one, through the complete
scan, from the first increment that has the access method**, which is what
[ADR 0022](../adr/0022-the-complete-scan-stays.md) point 4 requires. The scan
needs only one change in `kingres_shard` (section 9.1).

### 8.4 Parallel scan

Increment C5. The scan is parallel-aware (`path->parallel_aware`, a partial path
under `Gather`): the unit of work is the stripe. The shared state is one
`pg_atomic_uint64` (next stripe index) in dynamic shared memory
(`EstimateDSMCustomScan`, `InitializeDSMCustomScan`, `InitializeWorkerCustomScan`,
`ReInitializeDSMCustomScan`). Every participant reads the stripe list itself with
the same snapshot (the snapshot is restored in workers) and takes the next index.
The leader flushes the transaction's own buffers for the relation before it
starts workers (7.1). The plain `parallelscan_*` callbacks use the same counter.
**[Needs verification]** that the leader's flush in `BeginCustomScan` happens
before `EnterParallelMode`, which would forbid the heap insert of the catalog row.
A table with few stripes gets few workers (`parallel_workers` is capped by the
stripe count).

### 8.5 `EXPLAIN`

```
Custom Scan (Columnar Scan) on events_p20260101 e  (cost=... rows=... width=...)
  Filter: ((ts >= '2026-01-01 06:00:00+00') AND (device_id = 42))
  Columnar Projected Columns: 2 of 8 (ts, device_id)
  Columnar Stripes: 12 total
  Columnar Skip Conditions: ts >= ..., device_id = 42
  Columnar Stripes Skipped: 9               -- with ANALYZE
  Columnar Chunk Groups Skipped: 31 of 45   -- with ANALYZE
  Columnar Compressed Bytes Read: 14 MB     -- with ANALYZE
```

`ExplainCustomScan` prints the first three lines always (the stripe total is
read at plan time only when `VERBOSE`; otherwise it is shown at execution) and
the counters with `ANALYZE`. `EXPLAIN` also shows the access method per
partition through the node name. A statement on the distributed scan shows its
remote SQL as in phase 2a; each remote group's own plan is not shown (as today).

### 8.6 What is refused or slow on read

`TABLESAMPLE` (refused), `SELECT ... FOR UPDATE/SHARE` (refused), TID scans and
`ctid` (not available), index scans (none), Bitmap Heap Scan (none), `CLUSTER`.
Point lookups by key are slow (a stripe scan with skipping): the format is for
scans.

## 9. Columnar and the sharding extension

### 9.1 A columnar distributed table

`create_distributed_table` recreates the table as `PARTITION BY HASH` with one
partition `<table>_b<NNNN>` per bucket. The partitions are created with `CREATE
TABLE ... PARTITION OF` and so take the parent's access method (`tablecmds.c`
lines 1055 to 1066), provided the new partitioned parent has it set. The code of
`create_distributed_table` does not read `pg_class.relam` anywhere (a search of
`contrib/kingres_shard/src` and `kingres_shard--0.3.sql` at HEAD for `relam`,
"access method" and `default_table_access_method` finds nothing), so a columnar
source table would become a **heap** distributed table without a message.
Required change in `kingres_shard` (increment S1, in the next free extension
version after phase 11's 0.8; phase 2b is 0.5, phase 9 0.6, phase 10 0.7): read the
access method of the source table, create the partitioned parent `USING <am>`, and
create the copy tables of a bucket move (phase 4, "copying") and the shadow tables
of phase 9's rebuild `USING <am>` too (`LIKE ... INCLUDING ALL` does not carry the
access method; section 11.6 decides that moves and rebuilds recreate each leaf with
its source access method); and `add_node` creates the tables on a new node with the
same access method.
Without this the combination is silently wrong, so the first columnar increment
that reaches `kingres_shard` is S1, and until it lands `create_distributed_table`
refuses a table whose access method is not `heap` (a two-line check that the
extension can have at once).

The extension must exist on every node, and `kingres_columnar` must be listed
before the table is created (4.2). A hash-distributed columnar table has no
primary key (7.8), which `create_distributed_table` allows (its rule is that
every unique constraint must include the distribution column; "no unique
constraint" passes).

### 9.2 Reads, writes and the row router

- **Reads:** complete scan, section 8.3. The pushdown of part 1 and phase 2b
  (partial aggregates, colocated joins, sort and limit) works because it ships
  SQL; on each group the Columnar Scan answers. The phase 2b partial-aggregate
  plans (`partialize_plan`, section 7.3 there) take the plan of the fragment
  whatever its scan node is; **[Needs verification]** that the `Agg` is still on
  top of a custom scan child (the partial plan rule needs only the top node).
- **Writes:** `INSERT`, multi-row `INSERT` and `COPY FROM` through the row router
  (phase 2b, section 11) send batches `INSERT ... SELECT FROM unnest(...)` to each
  group, in one remote transaction each; the batches of one remote transaction
  land in the same write buffers and become full stripes at that transaction's
  end (7.2, point 5), not one stripe per 1000-row batch. `RETURNING` works.
  `INSERT ... ON CONFLICT DO NOTHING` works; `DO UPDATE` is refused by the access
  method on the group. The row router's per-group atomicity (two-phase commit) is
  unchanged: the flush at `XACT_EVENT_PRE_PREPARE` makes a prepared
  transaction's stripes durable and invisible until `COMMIT PREPARED` (7.3).
- **Fan-out `UPDATE` and `DELETE`** (phase 2a): the statement runs on every group
  that owns buckets and fails on the first group that matches a row of a columnar
  partition, with the append-only message; the two-phase transaction rolls back
  the rest. A distributed table with both formats (time partitioning, section 11)
  can be updated through its heap partitions.
- **The stale-map rule (`KS001`)** concerns which node serves which bucket. The
  placement hash and the partition count are read from the catalog and from
  `pg_inherits`, not from the storage, so they are not affected by the access
  method. **Converting a partition changes the relation id of a bucket leaf** but
  not its bucket, its group, or the number of partitions, and it happens
  atomically (10.4): a scan that planned before the swap holds `AccessShareLock`
  on the old partition, which the swap cannot take away (it waits for the scan,
  within its lock timeout), and a scan that plans after the swap sees the new
  one. No new `KS001` source. For a user-partitioned table (phase 9) the swap happens
inside one bucket partition, and the partition-count check compares bucket
partitions, so it does not change either (11.3).

### 9.3 Bucket moves and logical decoding

A bucket move (phase 4; phase 9 extends it to user-partitioned buckets) copies a
bucket's partitions with the initial copy of a logical subscription and then applies
the changes through logical decoding.

**What the access method must give.** (a) The initial copy reads the source with
`COPY (SELECT ...) TO` or the table copy of `tablesync`, which use `table_beginscan`:
works from 0.1. (b) The target writes the standalone copy with `COPY`-style
`table_multi_insert` in the table sync worker and `ExecSimpleRelationInsert` in the
apply worker (`simple_table_tuple_insert`, `execReplication.c` line 676), so inserts
work on a columnar target. The apply worker's per-transaction buffers are flushed at
its commit; many tiny source transactions make many small stripes on the target,
which `columnar_compact` merges (the move calls it before the switch). (c) The
changes made on the source during the copy and catch-up must be **decoded**. `UPDATE`
and `DELETE` cannot occur (7.7); `INSERT` must be decodable. (d) The copy tables are
created with the source's access method (11.6).

**Decoding in 0.1 is not provided.** Without `rm_decode` the decoder notes the xid and
ignores the record (`decode.c` lines 115 to 120). A columnar insert during a move
would be lost, and a publication of a columnar table would silently miss rows. This
must not be possible, so until C7:

1. `kingres.move_bucket` checks in `preparing`, on the owning node, that no table of
   the colocation group has a columnar relation anywhere in the bucket's tree (a
   columnar bucket partition, or a columnar leaf of a user-partitioned table), and
   refuses before it changes anything with `ERROR: bucket move refused: relation
   "public.events_p20261101_b0007" has columnar storage, and the installed
   kingres_columnar cannot decode its changes`, SQLSTATE `0A000`, with the hint
   `Move buckets of this table after kingres_columnar is upgraded to a version that
   supports logical decoding (0.2), or convert the table to row storage`. The same
   check runs for the rebuild operations of phase 9 that use logical replication.
2. `kingres_columnar` installs an event trigger (`ddl_command_end`, `ENABLE
   ALWAYS`) that refuses `CREATE PUBLICATION` and `ALTER PUBLICATION` naming a
   columnar table, and a `FOR ALL TABLES` or `FOR TABLES IN SCHEMA` publication that
   would cover one; `kingres.columnar_check()` reports a publication that covers a
   columnar table (created before the extension, or `FOR ALL TABLES` created later:
   PostgreSQL has no hook for "a table became part of a publication", so the trigger
   and the check are the best available without a core patch). **[Needs
   verification]** by a test that creates every kind of publication around a
   columnar table.
3. The console and the documentation say it, and a conversion of a leaf of a
   distributed table is refused until S3 (11.4, 9.7).

**Increment C7 implements decoding** and lifts the refusal:

- When `wal_level = logical` and `RelationIsLogicallyLogged(rel)` (the macro in
  `rel.h`, line 721) the flush of a stripe also writes `COLUMNAR_LOGICAL_INSERT`
  records: the rows of the stripe in the heap tuple form (`heap_form_tuple` images
  with a small header), in chunks of up to a few hundred rows. This roughly doubles
  the WAL of a load (the raw rows are logged in addition to the compressed pages) and
  only when `wal_level` is `logical` (heap logs tuples then too).
- `rm_decode` does what `heap_decode` and `DecodeInsert` do for an insert:
  `ReorderBufferProcessXid`; return when the snapshot builder is below
  `SNAPBUILD_FULL_SNAPSHOT`; `SnapBuildProcessChange` and not `ctx->fast_forward`;
  skip other databases (`ctx->slot->data.database`) and filtered origins
  (`FilterByOrigin`); allocate a `ReorderBufferChange` with
  `REORDER_BUFFER_CHANGE_INSERT`, the relation's `RelFileLocator`, a tuple buffer
  (`ReorderBufferAllocTupleBuf`) filled from the record; queue it
  (`ReorderBufferQueueChange`). The reorder buffer spills to disk under
  `logical_decoding_work_mem` as for heap. **[Needs verification]** that every
  function needed is exported to extension code on all supported platforms and that
  nothing private to `decode.c` is needed beyond what `DecodeInsert` shows.
- Changes are queued per transaction and emitted at commit, so the order "pages
  record, then logical record, in the same transaction" is correct.
- **Replica identity.** A hash-distributed columnar table has no primary key (7.8).
  Phase 4 requires "a primary key or `REPLICA IDENTITY FULL`", and a publication that
  publishes `update` and `delete` needs one. The conversion of a leaf and
  `create_distributed_table` (S1) therefore set `REPLICA IDENTITY FULL` on every
  columnar relation of a distributed table. No `UPDATE` or `DELETE` ever reaches the
  publication; the identity only satisfies the check. No second, insert-only
  publication is needed. **[Needs verification]** that
  `ALTER TABLE ... REPLICA IDENTITY FULL` works on a table of a custom access method
  and that a publication of such a table accepts it.
- **User-partitioned buckets.** Phase 9's move publishes the root of the bucket's tree
  with `publish_via_partition_root = true`; changes of a leaf are published as changes
  of the root, and the target applies them by tuple routing into its own tree, whose
  leaves have the source's access methods (11.6). Heap and columnar leaves can sit in
  one tree. **[Needs verification]** (R11) that a columnar leaf's decoded insert is
  mapped to the root by pgoutput as a heap leaf's is.

The bucket move's other steps do not depend on the format: the standalone copy with
`CHECK (satisfies_hash_partition(...))` attaches without a scan; the marker row proof
of replay goes through a heap table; the switch locks the parents. The copy of a
columnar bucket costs a full scan on the source and a `COPY` into stripes on the
target.

### 9.4 Two-phase commit, standbys, failover

Section 7.3 and 7.4: stripes are WAL, flushed before `PREPARE`; standbys of
shard groups hold the same files; failover (phase 5) needs nothing columnar-
specific. The recovery worker and `resume_bucket_moves` act on prepared
transactions, which carry columnar changes like any others.

### 9.5 Reference tables

A reference table (copied to every node; writes run on every node) may be
columnar: reads are local; the write rule needs the same rows on every node and
refuses non-immutable expressions already. The row-number counters differ between
nodes (each node numbers its own stripes), which no query can see. Reference
tables get little from columnar storage; the console offers the format on them
but nothing depends on it. **[Recommendation]** to refuse it in 0.1 (`create_reference_table`
checks the access method like `create_distributed_table`) and revisit with
demand.

### 9.6 Ledger tables

A ledger table cannot be columnar, and a columnar table cannot be put under
ledger control. Reasons, from the committed ledger code and documentation:

1. A ledger table is protected by blocking triggers for `UPDATE`, `DELETE` and
   `TRUNCATE`, an insert-capture trigger, and a `table_rewrite` event trigger that
   rejects every rewrite of a ledger table ("when ALTER TABLE rewrites a ledger
   table", `kingres_ledger` README). `ALTER TABLE ... SET ACCESS METHOD` is a
   rewrite, so a ledger table cannot be converted in place.
2. The append-only and updatable guarantees and `ledger_verify` were designed and
   tested for heap: the capture code writes history rows "through the table
   access method" and reads old tuples as heap tuples (`heap_deform_tuple`,
   `heap_getattr` in `capture.c` and `row_hash.c`); the verification recomputes
   row hashes from the stored rows. Nothing in them is tested against another
   storage, and a tamper-evidence claim must not rest on an untested substitution
   (section 5 point on the ledger design; shard-phase2b 11.8 reaches the same
   conclusion for distributed ledger tables).
3. `ledger_enable` does not check the access method today (a search of
   `contrib/kingres_ledger/src` for `relam` finds nothing), so a columnar table
   could be put under ledger control. **Required change in `kingres_ledger`
   (increment L1):** `ledger_enable` refuses a table whose access method is not
   `heap`, and the event trigger of an existing ledger table rejects `SET ACCESS
   METHOD` (already true through `table_rewrite`; the test must show it). On the
   other side, `kingres.convert_partition` and `kingres.columnar_*` refuse a table
   that is a ledger table or a ledger history table, and the console does not
   offer the action on one.

### 9.7 Moves and splits, stated for each kind of table

Phase 10 ([shard-phase10.md](shard-phase10.md), sections 3.1 and 3.2, read for this
note) splits a bucket in place into 2, 4, 8 or 16 children: it creates each child as
a standalone table `LIKE parent INCLUDING ALL` (a heap table, because `LIKE` does not
carry the access method), puts an `AFTER INSERT OR UPDATE OR DELETE` capture trigger
on the old partition that upserts into the child (`INSERT ... ON CONFLICT (<key>) DO
UPDATE`), backfills with `INSERT ... SELECT ... FOR SHARE SKIP LOCKED ... ON CONFLICT
DO NOTHING`, and swaps with `DETACH`/`ATTACH` in one two-phase transaction. It
requires a primary key on every table of the colocation group, and refuses a bucket
whose tables are user-partitioned. A move still uses logical replication between
nodes. The answers for this part:

| Kind of table | Bucket **move** | Bucket **split** | Refusal text (stable words in `Problem.detail`, SQLSTATE `0A000` in SQL) |
|---|---|---|---|
| Hash-distributed, **columnar** (the bucket partitions are columnar) | **Before C7 and S3: refused.** After: works, by logical replication of decoded inserts and the stripe-wise `COPY` of the initial copy (9.3), with the copy tables created `USING columnar` (11.6) and `REPLICA IDENTITY FULL` | **Refused**, always in the versions of this part | Move: `bucket move refused: relation "public.events_b0007" has columnar storage, and the installed kingres_columnar cannot decode its changes` (until C7). Split: `bucket split refused: table "public.events" is columnar; a split needs a primary key, row locks and update triggers on the table, which columnar tables do not have` (problem code `split_unsupported`, 422, from phase 10's list) |
| Hash-distributed, heap, **time-partitioned** (hash on top, range below; phase 9 layout), all leaves heap | Works: phase 9's tree-aware move (publication of the root with `publish_via_partition_root`, target tree built from the source's leaves) | **Refused** by phase 10 (user-partitioned bucket) | Split: phase 10's own refusal (`split_unsupported`); this part adds nothing |
| Hash-distributed, time-partitioned, **some leaves columnar** (the policy converted old partitions) | Before C7 and S3: refused, with the text above naming the leaf. After: works; the target tree has heap and columnar leaves as the source | Refused (user-partitioned bucket, and columnar) | As above; the console lists the columnar leaves that block |
| Colocated group mixing a columnar table and heap tables | The group moves together (phase 4): refused as a whole while any table of the group has a columnar relation in the bucket, before C7; after, works | Refused as a whole: one split per colocation group moves every table of the group, so one columnar table blocks the split of every table of the group | The same texts, naming the first columnar table |
| Reference table, columnar | Not moved (reference tables are never moved or split) | Not split | Columnar reference tables are refused in 0.1 (9.5) |

Why a split cannot work on a columnar table, in terms of the algorithm: (1) **no
primary key**: columnar tables have no index before C6, and phase 10 requires a
primary key to match rows between the old partition and the children; (2) **row
locks**: the backfill takes `FOR SHARE SKIP LOCKED`, which is `tuple_lock`, refused by
the access method; (3) **`UPDATE` and `DELETE` capture**: columnar rows are never
updated, so those triggers would never fire, which is harmless, but the trigger's
`ON CONFLICT (<key>) DO UPDATE` into a child needs a unique index on the child, which
a columnar child could not have; and (4) the children are created heap (`LIKE`),
which would silently change the table's format mid-split. A **row trigger on a
columnar table works for `INSERT`** (executor-level, 6): that part of phase 10's
design would run; the three other reasons stop it. After C6 a columnar table can have
a primary key (increment C6), and (2) remains, so a split stays refused;
**[Recommendation]** to keep a columnar bucket unsplittable: a columnar table is
append-only and the way to rebalance load is to move buckets, or to convert the
table to heap, split, and convert back.

The check is made in `preparing`, and again before the switch (as phase 10 checks its
other preconditions); `kingres.check_cluster()` is not involved. For a split of a
time-partitioned table there is nothing for this part to refuse: phase 10 already
does, and the message above is phase 10's.

What this means for a policy that converts old partitions of a distributed table:
**until C7 and S3 are installed on every node, `convert_partition` of a leaf of a
distributed table is refused** (`conversion_refused`, reason `bucket_move_unsupported`)
so that a policy cannot make a table unmovable by accident. After S3 a converted leaf
moves with its bucket.

## 10. Time partitioning, creation ahead and retention

### 10.1 Model

A **policy** belongs to a table that is partitioned by `RANGE` on one column of
type `date`, `timestamp` or `timestamptz`. Partitions have equal width
`partition_every`, aligned to a grid (10.3). A run, at time `as_of`:

1. **create**: every missing partition whose lower bound is before `as_of +
   create_ahead`, starting from the first existing partition's grid position (or
   the interval containing `as_of` for a table with no partitions), so that no
   gap appears between the first and the last partition; partitions are created
   `USING` the access method of the parent unless the policy says otherwise;
2. **convert**: every heap partition whose upper bound is `<= as_of -
   columnar_after` becomes columnar, oldest first, one at a time;
3. **retire**: every partition whose upper bound is `<= as_of - retention` is
   dropped, or detached when `retention_action` is `detach`.

The three are independent and in this order, so a partition that is both old
enough to convert and old enough to drop is only dropped. The policy requires
`columnar_after <= retention`.

This is the model of the tools that were read: `pg_partman` has `premake`
(partitions to stay ahead, default 4) and `retention` with `retention_keep_table`
(default: keep the table and only detach it); TimescaleDB's `add_retention_policy`
drops "entire chunks once their time range falls outside your window" and does not
touch future chunks; Citus documents `create_time_partitions`,
`drop_old_time_partitions` and `alter_old_partitions_set_access_method`.
Differences, each deliberate: the default `retention_action` is `drop` (a
retention policy that keeps the disk full surprises operators; the console asks
for the table name to confirm a retention, section 14); no default partition is
created or kept (10.5); the lookahead is an interval (what a user thinks in), not
a count.

### 10.2 What a table must be

`timepart_set_policy` and `create_time_partitions` check, and name the offending
object in the error:

- `relkind = 'p'`, `partstrat = 'r'`, one key column of type `date`, `timestamp`
  or `timestamptz`, no key expression;
- the table owner is the caller (or a member of the owner role); superusers may
  use any table;
- no default partition: with one, `ATTACH PARTITION` scans it under a strong lock
  to check that it holds no row of the new range (the PostgreSQL rule for
  attaching next to a default), and `DETACH ... CONCURRENTLY` is refused
  (`tablecmds.c` line 21694: "cannot detach partitions concurrently when a default
  partition exists"). A table with a default partition is refused with a hint to
  create partitions far enough ahead instead;
- every existing partition's bounds lie on the grid; otherwise the run reports
  `misaligned` for that partition, leaves it alone and creates around it
  (`timepart_check` reports it).

Renames: an event trigger (`ddl_command_end`, `ENABLE ALWAYS`) updates `schema_name`
and `table_name` of the policy and its `parent_oid` when the table is renamed or
moved, using the `objid` of the command; a dump and restore keep the names (the
OID is a hint) and the run resolves the table by name if the OID no longer
matches. A `sql_drop` event on the table removes the policy (history stays).

### 10.3 Intervals, grid, time zone, names

`partition_every` must be one of: `N hours` with `24 mod N = 0`; `N days`; `1
week`; `N months` with `12 mod N = 0`; `N years`. (Checked by the function, which
reads the interval's months, days and microseconds and rejects mixtures.) The
grid: hours and days count from `2000-01-01 00:00:00` in the policy's time zone;
weeks start on Monday; months and years on the first. For `date` keys the time
zone is not used. The default time zone is `UTC`, not the session's `TimeZone`:
the bounds must not depend on who runs the maintenance (the extension sets
`timezone` for the transaction from the policy). Bounds are computed with
`date_trunc` and `+ interval` on `timestamptz AT TIME ZONE tz`; **[Needs
verification]** with a test across a daylight-saving change for a `1 day`
interval in a zone that has one.

Partition names: `<table>_p<suffix>` with the suffix `YYYYMMDDHH24` for hours,
`YYYYMMDD` for days and weeks (the first day), `YYYYMM` for months, `YYYY` for
years, in the policy's zone; the base name is cut so that the whole name fits in
63 bytes, and an existing relation of the same name that is not the expected
partition makes that action fail with `name_conflict` (no rename, no overwrite).

### 10.4 The algorithms, step by step

**Create one partition** (`create_time_partitions`, in the transaction of the
caller or of the run):

1. `SET LOCAL lock_timeout` (default 5 s) and, for the run, `SET LOCAL ROLE` to the
   table's owner (section 12).
2. `CREATE TABLE <schema>.<name> (LIKE <parent> INCLUDING DEFAULTS INCLUDING
   CONSTRAINTS INCLUDING INDEXES INCLUDING STORAGE INCLUDING COMMENTS [USING
   <am>])` plus `ALTER TABLE ... ADD CONSTRAINT <name>_bound CHECK (<key> >= from
   AND <key> < to)`. The `LIKE` reads the parent with `AccessShareLock`. **This is
   used instead of `CREATE TABLE ... PARTITION OF`, which takes `AccessExclusiveLock`
   on the parent** (`tablecmds.c` line 872: `parentLockmode = (stmt->partbound !=
   NULL ? AccessExclusiveLock : ...)`).
3. `ALTER TABLE <parent> ATTACH PARTITION <name> FOR VALUES FROM (...) TO (...)`.
   `ATTACH PARTITION` takes `ShareUpdateExclusiveLock` on the parent
   (`tablecmds.c` line 4854), which does not block reads or writes of the parent;
   the existing `CHECK` that implies the bound makes the attach skip the scan (the
   same mechanism phase 4 uses for bucket copies: "ATTACH PARTITION scans the
   partition unless an equal check exists"); the table is empty anyway. Then drop
   the helper check.
4. Indexes of the parent are matched or created on attach; foreign keys are cloned
   by PostgreSQL.

What is blocked: nothing on reads; writers of the parent are not blocked by
`ShareUpdateExclusiveLock`; two creators of partitions of the same table block
each other (and the advisory lock of 10.7 prevents it). Crash safety: all in one
transaction (an orphan standalone table cannot survive a crash, because it is
created in the same transaction as its attach). A run creates its partitions one
transaction each, so a failure in the fifth leaves four.

**Retire a partition** (`drop_old_time_partitions`):

1. `lock_timeout` 2 s, `SET LOCAL ROLE`; `ALTER TABLE <parent> DETACH PARTITION <p>`
   (not concurrent: needs `AccessExclusiveLock` on the parent, `tablecmds.c`
   line 4859 to 4862, and so waits for running statements on the parent and
   queues new ones behind it for at most the timeout), then `DROP TABLE <p>`
   (or, for `detach`, `ALTER TABLE <p> RENAME TO <p>__detached`? **no**: the
   partition keeps its name and its data; the run records it).
2. On `lock_not_available` the run waits a random 0.2 to 2 s and retries, up to 5
   times, then records the action as `failed` with `detail = lock_timeout` and goes
   on to the next partition. The retry loop is a PL/pgSQL block with an exception
   handler, which cannot contain a `COMMIT`; the `COMMIT` comes after the block.
3. **`DETACH PARTITION ... CONCURRENTLY` is not used by the SQL layer.** It cannot
   run inside a transaction block (`utility.c` lines 1285 to 1295:
   `PreventInTransactionBlock(isTopLevel, "ALTER TABLE ... DETACH CONCURRENTLY")`),
   and a statement inside a procedure is not top level, so it fails inside
   `kingres.timepart_run_phase`. **[Needs verification]** by a test that calls it
   from a procedure. **[Recommendation]** to accept the short `AccessExclusiveLock`
   with retries (the same trade-off phase 4 made for the bucket switch), and to
   revisit if a user needs no blip: `kingresd` could then issue the detach as a
   top-level statement of its own step.
4. The drop of a columnar partition leaves catalog rows for its file; the
   `sql_drop` trigger reconciles (5.5).

What is blocked: the parent for up to 2 s per attempt, 5 attempts. Crash safety:
one transaction per partition; the detach and the drop are in the same one, so
either the partition exists attached or it is gone.

**Convert one partition** (`convert_partition(parent, p, 'columnar', sort_by,
options)`), a procedure; one transaction for the whole conversion:

1. **Checks** (before any lock beyond `AccessShareLock`; each failure names the
   reason and raises `conversion_refused`): `p` is a leaf partition of `parent`;
   its access method is not already the target; the target extension is
   installed; `p` is not a ledger table; `p` has no row-level triggers of its own,
   no rules, no row security policies, is not in a publication
   (`pg_publication_rel`), has no dependent view or foreign key reference, and is
   not a partitioned table; for `columnar`, `p` has no index and the parent has no
   partitioned index (7.8) until increment C6 (the error names the index); the
   owner is the one running or the caller may `SET ROLE` to it; and the owner is a
   superuser, or no `CHECK`, generated column or column default of the table
   calls a function outside `pg_catalog` (12).
2. `CREATE TABLE <p>__kc (LIKE <p> INCLUDING DEFAULTS INCLUDING CONSTRAINTS
   INCLUDING COMMENTS) USING columnar`, set the options (`columnar_set_options`),
   copy the owner, the privileges (`aclexplode` of `relacl`, as `GRANT`
   statements) and `REPLICA IDENTITY FULL` when the old one is `nothing` and the
   table is a distributed leaf.
3. `LOCK TABLE <p> IN SHARE MODE`: **writers of `p` wait, readers go on.** This is
   the lock for the whole copy. (Writers to an old partition are rare by
   definition of the policy; a writer that waits waits as long as the copy, and
   gets its own `lock_timeout` or `statement_timeout` error if it set one.
   **[Recommendation]** this stays, and the console says it.)
4. `INSERT INTO <p>__kc (<columns>) SELECT <columns> FROM <p> ORDER BY <sort_by>`;
   `sort_by` defaults to the partition column, so chunk groups cover narrow time
   ranges and skipping works within the partition. The statement lists the columns
   explicitly (no default expression is evaluated) and reads with a plain scan.
5. **Verify** under the lock that `p` cannot change: `count(*)` of both tables are
   equal, and the row counts of `columnar_info` agree. A mismatch rolls back and
   fails with `verification_failed`.
6. `lock_timeout` 500 ms (below `deadlock_timeout`; the function reads the setting
   and uses the smaller of 500 ms and half of it, which keeps the lock wait
   shorter than a deadlock check), then `LOCK TABLE <parent> IN ACCESS EXCLUSIVE
   MODE`. If the lock times out, the transaction rolls back (the copy is
   discarded: the new table never existed outside it) and the run records the
   action as `failed`, `detail = swap_lock_timeout`; the next run tries again.
   The wait is short on purpose: a writer of `p` holds the parent in `RowExclusive`
   and waits for our `SHARE` lock on `p`, we hold `SHARE` on `p` and wait for the
   parent: a deadlock cycle that only the timeout (and the deadlock detector after
   it) breaks. The timeout is below the detector's, so the conversion loses, not
   the user's statement.
7. `ALTER TABLE <parent> DETACH PARTITION <p>`; `ALTER TABLE <parent> ATTACH
   PARTITION <p>__kc FOR VALUES ...` (the `CHECK` of the bound was in the copy's
   definition, so no scan); `DROP TABLE <p>`; `ALTER TABLE <p>__kc RENAME TO <p>`;
   drop the helper check. All in the same transaction, so a reader sees the old
   partition or the new one, never neither and never both.
8. `COMMIT`; then `ANALYZE` of the new partition in a separate transaction.

What is blocked: **writers of the partition for the whole copy** (SHARE), **all
statements on the parent for the swap** (a fraction of a second after the lock is
granted; at most 500 ms of waiting per attempt). Crash safety: one transaction; a
crash before commit leaves the old partition untouched and no trace of the new
one except unreferenced pages in a table that does not exist; after commit the
new partition is in place. The temporary table name `<p>__kc` cannot clash with a
user table without failing the action cleanly. A conversion of `p` takes time
proportional to its size (the copy reads at the speed of the heap scan and writes
at the speed of stripe compression; **no measurement exists**; the console shows
elapsed time and the partition's size).

Converting back (`access_method => 'heap'`) is the same algorithm in the other
direction.

**Plain, non-partitioned table to columnar.** `ALTER TABLE ... SET ACCESS METHOD
columnar` (PostgreSQL's command, `ACCESS EXCLUSIVE`: it blocks readers and writers
for the whole rewrite). The REST operation uses it and says so; the console warns
before it and prefers partitioning for large tables. There is no build-and-swap
for a non-partition table in this part: dependent views, foreign keys and
publications make the swap much more than a rename (open question 8).

### 10.4a Fixes of the review of the conversion (kingres_timepart 0.2, findings T13-01 to T13-14)

Decisions, in the order of the findings. The code is in
`contrib/kingres_timepart/kingres_timepart--0.2.sql` (and the same text in
`kingres_timepart--0.1--0.2.sql`); the tests are `sql/timepart_convert_more.sql`
and `t/009_convert_ddl_race.pl`.

**T13-01. Read the catalog after the lock.** The statements of a conversion used
to be built before `LOCK TABLE <partition>`, which can wait for writers for
`create_lock_timeout_ms`; a session that held a lock on the partition could add a
column, create an index or drop a column before it committed, and the copy then
lost the new column (null for every row) and the new index. Now everything the
conversion reads from the catalog (the refusals, the names, the bound, the column
list, the sort, the tablespace, the index statements and what is needed to carry
over) is made by one function, `timepart__convert_plan`, which is VOLATILE (a new
snapshot for every statement). `timepart__convert_stmts` calls it once, before any
lock, only to refuse early; the DO block it returns holds the OIDs of the table and
the partition and the caller's arguments, no name, and calls the plan again right
after the lock. A fingerprint compared after the lock was the other choice; it
would turn the race into an error, where the plan turns it into a correct copy.
The lock is taken by the name of the moment (from the OID); if the name no longer
exists, the block fails with `schema_changed` (SQLSTATE 55P03: retryable, the
policy run records it as a failed action and is due again within five minutes), and
the block checks in `pg_locks` that the lock it holds is on the OID it means. The
swap needed no change: a session that waits for the partition while it holds the
table is the cycle that the swap already breaks by giving way.

**T13-02. Not MVCC-safe, as TRUNCATE and the rewriting forms of ALTER TABLE.**
The new partition is filled by the conversion's transaction, so a REPEATABLE READ
or SERIALIZABLE snapshot older than the swap, on a table that the transaction has
not read yet, finds the partition empty. No swap of one table for another in one
transaction can avoid it (the rows would need an older xmin, and only COPY FREEZE
into a table made in the same transaction gives that, from a client, not from SQL
inside a block). Detecting the snapshot is not possible either: the isolation
level of another session is not visible, `backend_xmin` of other roles' sessions
is not visible to an owner, and waiting for older snapshots as CREATE INDEX
CONCURRENTLY does needs two transactions, because a snapshot taken after the wait
and before the commit is as old as one taken before it. So this is a documented
limit: the README says which transactions are affected (READ COMMITTED statements
are not: they take their snapshot after the lock; a transaction that has read the
table is not: the swap gives way to it; `pg_dump` is not once it has locked the
tables) and `t/009_convert_ddl_race.pl` keeps the behaviour under test. The claim
"never neither" of the README is now limited to statements.

**T13-03. Publication of the copy.** A heap target is refused when a publication
of the partition's schema, or of all tables, would take in the new table at its
creation and publish the copy under its temporary name. (A publication of all
tables cannot even be created while kingres_columnar is installed; the check is
there for the order of events in which it was.) The columnar target keeps its
refusal of every publication that would include the partition.

**T13-04, T13-05. What LIKE does not carry.** After the attach the enabled state of
each cloned trigger is put back (`DISABLE`, `ENABLE REPLICA`, `ENABLE ALWAYS`). The
storage parameters of the table and of its TOAST table, the statistics target and
the options of each column (not the server's own version stamp `pg_attver`), the
statistics target of an index expression and the clustering index are carried;
each is a statement of its own in a sub-block, so a parameter that the target
access method does not accept is a WARNING and the rest goes on. The storage
parameters are set after the load and the indexes: `parallel_workers` set before
makes a columnar table refuse a parallel index build of rows that the transaction
has not written, and a fillfactor set after the load applies to what is written
next, as it would after an ordinary ALTER TABLE. A columnar table has no TOAST
table, so those parameters do not go to it and do not come back.

**T13-06. NOT VALID.** Refused, by name (CHECK and NOT NULL). Recreating the
constraint NOT VALID after the load was the other choice; refusing is the rule of
this extension for what the new table cannot carry exactly.

**T13-07. Grantor.** The privileges that the owner took from itself are revoked
again on the new table. A privilege that a role other than the owner granted (with
GRANT OPTION passed on) is refused: GRANT cannot name another grantor for a
non-superuser, and the owner as grantor would change what REVOKE ... CASCADE does.

**T13-08. Dependents.** The refusal reads `pg_depend` for every normal dependency
on the partition, on its row type and on the array type of the row type, except the
partition's own objects (generated column expressions, constraints, triggers,
policies, indexes), and names the first with `pg_describe_object`. DROP TABLE
without CASCADE would stop at exactly those, after the whole copy and in every run.

**T13-09. Names.** The temporary names of the indexes are
`kc_<12 characters of md5 of the partition name>_<n>`: short, the same on every
call, no cut by characters. They are checked against existing relations.

**T13-10. Default partition.** Refused when it is larger than the setting
`kingres_timepart.convert_default_max_mb` (default 256, 0 to 1048576): the attach
scans it under ACCESS EXCLUSIVE on the table and `lock_timeout` does not bound a
scan. A helper CHECK on the default partition (NOT VALID, then VALIDATE) would
keep SHARE ROW EXCLUSIVE on it to the commit, which is worse for its writers.

**T13-11. Files after a crash.** Documented, with a query that lists the files of
the database directory that no `pg_class` row names; nothing in the extension can
remove them.

**T13-12, T13-13. Tests.** Survivors of the review's mutants are now killed: M15
replica identity, M32 constraint comments, M34 default privileges, M37 another
table access method, M18 the grid in the plan of a run (`timepart_convert_more`);
M23 the session_user rule, as a superuser that has done SET ROLE (the same file);
M16 the tablespace and M04/M05/M40 the wait of the swap (`t/009`). M26 stays
equivalent.

**T13-14. Helpers.** `timepart__special_table` (SECURITY DEFINER, reads the private
catalogs of the ledger and the distributed tables) answers only a superuser or a
role that has the privileges of the owner of the table, judged by `session_user`
(inside a SECURITY DEFINER function nothing else says who called it). The other
helpers that PUBLIC may execute read `pg_catalog` only and are left as they are.
The new `timepart__convert_plan` is among them: it runs as the owner, after the
lock, so the owner needs USAGE on the schema `kingres`, as for every function of
the extension.

**Version.** 0.2 has not been released (no beta, no tag), so its two scripts were
edited in place and still give the same catalog (the `upgrade` test); there is no
0.2--0.3 script. A database that installed 0.2 from a development build earlier
re-creates the extension. After the first release a change of the script is a new
version with an upgrade script.

### 10.5 Locks at a glance

| Action | Lock on the parent | Lock on the partition | Blocks readers of the parent | Blocks writers |
|---|---|---|---|---|
| create (LIKE + ATTACH) | `ShareUpdateExclusive` | new table | no | no |
| convert, copy | `AccessShare` (the `INSERT ... SELECT` reads `p` directly) | `p`: `Share` | no | writers of `p` for the copy |
| convert, swap | `AccessExclusive`, at most 500 ms of waiting per attempt | `AccessExclusive` after detach | yes, for the swap | yes, for the swap |
| retire (drop or detach) | `AccessExclusive`, at most 2 s of waiting per attempt, 5 attempts | `AccessExclusive` | yes, while it waits and works | yes |

All waits are `lock_timeout`s: nothing waits indefinitely, and every refusal is a
recorded action, retried by the next run.

### 10.6 Where the schedule runs

Choices: (A) a background worker in a database extension, (B) `pg_cron`, (C) the
task engine of `kingresd` on the manager leader.

**Decision: (C).** A scheduler in `kingresd` starts a task `partition.maintain`
for each due policy; the task calls the SQL procedures of 4.4. The reasons:

1. **Visibility and control.** A task has named steps, a state, a history,
   cancellation, an audit entry, a lease with fencing, and appears in the console's
   task list with no new UI; an operator sees a run in progress and can cancel it.
   A background worker would need all of that built again in SQL and C.
2. **One place to be right.** The manager leader follows the primary of the map
   group ([ADR 0018](../adr/0018-manager-leader-follows-the-map-group-primary.md)),
   the task engine takes at most one unfinished task per subject, and a lost
   leader's tasks are taken over from their lease. A background worker runs on every
   instance and would need its own election to avoid two runs of a distributed
   table's policy on two nodes. The existing workers (`kingres_shard`'s recovery
   worker and launcher, `kingres_ledger`'s launcher and per-database workers) show
   the size of that machinery.
3. **Distributed tables.** The operations on a distributed table are DDL
   propagated from the map group primary through two-phase commit and, for
   conversion, a fan-out over groups. `kingresd` already holds service
   connections to every instance and runs the multi-node tasks (rebalance, add
   server). A worker inside one node would have to open node-to-node sessions
   with the forwarded-session proof of phase 2a.
4. **`pg_cron`** has a compatible licence (the PostgreSQL license, per its page)
   and supports 18, but: it needs another preloaded library and a
   `cron.database_name` (one database holds the jobs, so a database-per-tenant
   cluster needs a job in each), its jobs do not run on a hot standby and so do not
   move with a failover until the new primary starts them, it adds a second
   scheduler with its own history table to the product, and it cannot see the
   task engine. It is not used. A user who wants it can still call
   `kingres.timepart_run(...)` from a `pg_cron` job; the SQL layer is a complete
   interface by itself.

Costs of (C), accepted: if `kingresd` is not running, nothing runs. The
mitigation is in the design: `create_ahead` defaults to 7 days, `timepart_check`
reports `ahead_low` when the last partition ends less than half of `create_ahead`
ahead, and that becomes an alert (`partitionsAheadLow`, section 13) before an
`INSERT` could fail with "no partition of relation found for row". The monitor
runner already evaluates alert conditions every 10 seconds.

**The scheduler** (`kingres/control-plane/internal/partsched`, new; active only on
the manager leader): every `partition_scheduler_interval` (default 60 s, setting of
`kingresd`), for each database that has `kingres_timepart`, and for each group
primary that can hold a policy table (a plain table is on one node; the
leader asks every primary through the internal API's statement call, control-plane-3
decision D2, **[Needs verification]** that the call may run a catalog query on any
server's local instance for a non-system database), run `SELECT * FROM
kingres.timepart_due(now())` and, for each row not already covered by an
unfinished task of the same subject, create a task `partition.maintain` with
subject `table` and id `database/schema.table`. The scheduler holds no durable
state: what is due is in the policy row (`next_run_at`), what is running is in the
task table, and after a leader change the new leader asks again. A clock is
injected, so the tests of section 15 control time.

The steps of `partition.maintain`, each idempotent (a step that finds nothing to
do succeeds):

| Step | Does |
|---|---|
| `begin` | `timepart_begin_run` (records the run, takes the advisory lock; if another run is live the step waits and retries, with the reason in `TaskStep.detail`) |
| `create` | `CALL timepart_run_phase(run, 'create')` |
| `convert` | `... 'convert'`; `itemsDone` and `itemsTotal` are the partitions converted and to convert |
| `retire` | `... 'retire'` |
| `finish` | `timepart_finish_run` (state, `next_run_at`, pruning) |

The task's `createdBy` is `system` for a scheduled run and the caller's role for
`runPartitionPolicy`. A step that raises an SQL error fails the task and the
run's state is `failed`; an action that failed (a lock timeout) does not fail the
step (the run ends `partial`); the next run tries again after `run_every`.

### 10.7 Concurrency and crash safety of a run

- **One run per table.** `timepart_begin_run` takes a session-level advisory lock
  keyed by the table's name hash (`pg_try_advisory_lock`); it is released when the
  session ends or at `timepart_finish_run`. A second session that cannot get it
  raises `55P03`. Together with the task engine's one-task-per-subject rule, a run
  cannot overlap itself, even if a deposed leader's session is still running a long
  conversion (the new leader's `begin` waits).
- **A deposed leader's session.** The service session sets
  `client_connection_check_interval` (10 s) so that a statement whose client has
  gone is cancelled **[Needs verification]** (a Linux-only setting of PostgreSQL 14
  and later; no effect elsewhere); otherwise it ends at its own end.
- **Idempotent phases.** `timepart_run_phase` re-plans from the current state at
  the run's `as_of`; running `create` twice creates nothing the second time;
  running `convert` twice converts nothing more; an interrupted `retire` finishes
  what is left. The run's action rows say what was done.
- **A crash of the server in the middle of a procedure.** Every action is a
  transaction; the run row stays `running`; `timepart_begin_run` of the next run
  marks runs `running` whose advisory lock is not held as `failed` with `error =
  'interrupted'`.
- **Clock.** `as_of` is an argument everywhere; nothing in the algorithms calls
  `now()` except the default of that argument and `timepart_due`. Tests pass any
  `as_of`.

### 10.8 What is not done by retention, on purpose

Retention drops whole partitions only. A partition that spans the retention
boundary stays whole until its upper bound passes it, so data may be kept up to one
`partition_every` longer than `retention`. A table with no partition older than
the boundary is untouched. Nothing deletes rows, and a retention of 0 is refused
(`retention` must be at least one `partition_every`).

## 11. Distributed time-partitioned tables

[shard-phase9.md](shard-phase9.md) is written and fixes the layout and the functions
this part uses. This section says how the policy runs on a distributed table, what
phase 9 gives, and what this part still needs from phase 9 and phase 10.

### 11.1 The layout, as phase 9 defines it

A user-partitioned distributed table is **hash on top, the user's partitions
below**: `T` is hash-partitioned by the distribution column; each bucket partition
`T_bNNNN` is partitioned by `RANGE` (or `LIST`) of the user's key; the leaves are
`<partition>_bNNNN`. A time partition such as `events_p20261101` is a **logical
object**: a row of `kingres.dist_partition (parent, partition_name, bound)` and one
leaf per bucket, inside the bucket partitions of the node that owns the bucket.
`kingres.dist_table.partition_method` and `partition_key` say how the table is
partitioned; `kingres.partition_leaves(parent, partition)` returns `(node_id, bucket,
leaf)`; a user partition name has at most 57 bytes (`name_too_long` otherwise), so
the policy's partition names (10.3) cut the table name to fit; the number of leaves
is bounded by `kingres_shard.max_partition_leaves` (default 20 000). Phase 9 states
these as stable contract for phase 12.

### 11.2 How a policy runs on a distributed table

The policy row is on the map group primary (the node where the DDL of a distributed
table runs); it is in this part's own table `kingres.timepart_policy`, which phase 9
allows ("phase 12 may add columns ... to its own table; it must not change
these"), is not copied by `sync_catalog` (it is read and written on the map group
primary only) and reaches the standbys of group 0 by physical replication. The
scheduler asks the map group primary for it, as for a plain table whose home is
group 0. The three phases of a run become:

| Phase | Plain table (section 10) | Distributed table |
|---|---|---|
| create | `LIKE` + `ATTACH PARTITION` | `kingres.create_partition(parent, name, bound, if_not_exists => true)`. Phase 9 builds it from `ATTACH` with a bound `CHECK` too (not `PARTITION OF`, whose parent lock is `ACCESS EXCLUSIVE`), atomically on every node in one two-phase transaction. Always `heap`: phase 9 accepts `USING` only for `heap` |
| convert | `convert_partition` on the partition | the same algorithm, **once per leaf on the node that holds it**, with the bucket partition `T_bNNNN` as the parent (11.4) |
| retire | `DETACH` + `DROP` | `kingres.drop_partition(parent, name, missing_ok => true)`; for `retention_action = 'detach'`, `kingres.detach_partition(parent, name)`, which turns the partition into a distributed table of its own in the same colocation group (so a detached table stays distributed and is the user's to drop) |

The SQL procedures of `kingres_timepart` detect a distributed table by looking it up
in `kingres.dist_table` (when `kingres_shard` is installed) and call these functions;
the policy table, the history and the scheduler are the same. The check of a policy
(10.2) uses `partition_method = 'range'` and `partition_key` for a distributed table
and `pg_partitioned_table` for a plain one. A distributed table with a DEFAULT
partition is accepted by phase 9 with a warning; the policy refuses it as for a plain
table (the attach scans the default partition once per bucket and per node).

**Refusals and retries.** Phase 9: creating, dropping, attaching and detaching a
partition is refused (an error of the "object in use" class, `55006`, not a hang)
while a bucket move or a table operation claims the colocation group, and with
`lock_timeout` set by the caller a lock that cannot be had fails and rolls back on
every node. The runner therefore:

- treats that error as `retry_later`: the action is recorded `skipped`, the step
  retries it with exponential backoff (5 s, up to `partition_retry_window`, default
  10 minutes) inside the same run, and, when it is still refused, the run ends
  `partial` and `next_run_at` is set to `now() + least(run_every, 5 minutes)` (so a
  refusal does not wait a whole `run_every`);
- keeps `create_ahead` larger than the longest move it expects (default 7 days; a
  bucket move of a large bucket takes hours): the alert `partitionsAheadLow` fires at
  half of the lookahead, which is the signal that moves have been holding creation
  off;
- never runs `convert` of a leaf while a move claims its colocation group (11.4).

An `INSERT` of a row that fits no partition fails with PostgreSQL's "no partition of
relation found for row" from the group that owns its bucket (the row router adds its
own context line); the lead time and the alert are the only defence, as for a plain
table.

**Relation count.** A daily policy on 128 buckets adds 128 leaves a day (about 47 000
a year before retention), over `max_partition_leaves` after five months with no
retention. `timepart_set_policy` asks `table_change_report('create_partition', ...)`
(phase 9) how many leaves one more partition adds and refuses a policy whose
steady-state leaf count (`retention / partition_every x buckets`, or the count at one
year when there is no retention) exceeds `max_partition_leaves`, with the numbers in
the message. **[Recommendation]** the console's default `partition_every` for a
distributed table is 1 month and it warns above 10 000 leaves
(`partitionLeafWarning`).

### 11.3 Reads, writes, and the complete scan

Unchanged from section 8.3 and phase 9: a read prunes at both levels; a remote
fragment names the parent; each leaf is a base relation with its own access method,
so a table with old columnar leaves and recent heap leaves reads through the complete
scan with a Columnar Scan on the old ones. Writes through the row router reach the
leaf by PostgreSQL's tuple routing on the owner. The partition-count and placement
checks of the scan count buckets (`rel->nparts` is per bucket partition tree), which a
leaf swap does not change (9.2).

### 11.4 Converting the leaves of one logical partition

Phase 9 (section "What phase 12's time-partitioning tools can rely on", item 3) leaves
the per-leaf change of storage to this part, to be implemented with `partition_leaves`
and repeatable after a move. The conversion of logical partition `s` of `T`:

1. **Plan** (map group primary): `partition_leaves(T, s)` gives the leaves and their
   nodes; the control plane creates one task step `convert` per node that holds
   leaves (R6 below), each running, on that node, for each of its leaves
   `CALL kingres.convert_partition(<bucket partition>, <leaf>, 'columnar', ...)`.
   The parent is the **bucket partition** `T_bNNNN` (a plain PostgreSQL range
   partitioned table on that node), so the algorithm of 10.4 is unchanged and its
   locks are on the bucket partition and the leaf, not on `T`: statements on other
   buckets are not touched; the swap blocks the one bucket for at most 500 ms of
   waiting plus the swap itself.
2. The new leaf is created with `USING columnar`, `LIKE` the old leaf, and
   `REPLICA IDENTITY FULL` (7.8, 9.3); it keeps the leaf's name `<s>_bNNNN`; the
   `dist_partition` row is not touched (the logical partition is the same).
3. **Mixed state.** Between the steps of two nodes some leaves of `s` are heap and
   some columnar; reads are correct (8.3), and a `listTablePartitions` row shows
   `accessMethod: mixed` with the counts. A failed or cancelled task leaves the mix;
   the next run converts the rest (`convert_old_partitions` converts the leaves that
   are still heap).
4. **Exclusion from moves and other operations** (the requirement R5 below): the
   conversion of a leaf is DDL on the colocation group, so it must obey the rule of
   phase 4 and phase 9, which refuse DDL while a bucket of the group moves, and it
   must keep a move from starting while a swap is in flight. The conversion step
   asks, before it starts and again inside the swap transaction, that no
   `bucket_move` or `table_operation` claims the group and the bucket is `active`; if
   it does, the action is `skipped` with `retry_later`.

### 11.5 What this part needs from phase 9 and phase 10

Each item is a requirement stated so that the other design can confirm it or say what
it does instead. What phase 9 already provides (the layout, the four partition
functions, the refusal during moves, the leaf budget, the catalog) needs no
requirement; the numbering of the open items is kept stable for the tests.

| # | Need | From | Why |
|---|---|---|---|
| R4 | An authorization for an extension-owned DDL on a leaf: today the ProcessUtility hook refuses `ATTACH`, `DETACH`, `RENAME` and `DROP` of a bucket partition or leaf unless `internal_ddl_authorized()` is set by the extension itself. `kingres_timepart` is a separate extension and needs the same permission for its swap (a function of `kingres_shard` that runs a leaf swap on the local node, or a scoped switch a caller sets that is checked by the node-to-node proof) | phase 9 / `kingres_shard` | Section 11.4 step 1 |
| R5 | A claim for a leaf conversion: a `kind` of `begin_table_operation` (for example `convert_leaf`), or a documented lock that a move's `preparing` step and a table operation honour and that the conversion takes for the swap transaction only; and the converse, that the conversion refuses to start when the claim is held. A candidate is `SHARE` on `kingres.map_version` for the swap transaction only; **[Needs verification]** against the mode phase 4's and phase 9's claims use | phase 9 | A move's publication and standalone copy name the leaves; a swap during a move would drop one |
| R6 | A statement run on one node as a step: the control plane already runs per-server steps (control-plane-3, D4 and "operations that act on every primary"); `convert` needs "run this procedure on the node that holds leaf X", which the internal API's statement call gives; **[Needs verification]** that it may run a procedure that commits | control plane | One step per node |
| R10 | The move and the rebuild recreate each leaf **with its access method** (11.6) | phase 9 (increment 9.5, which "passes the access method of each leaf explicitly") and phase 4's move | Item 3 of phase 9's list; without it a moved columnar leaf arrives as heap |
| R11 | Phase 9's move of a user-partitioned bucket publishes the root with `publish_via_partition_root = true` and creates the target tree from `pg_partition_tree`: **[Needs verification]** that decoding the inserts of a columnar leaf (increment C7) is delivered as changes of the root, which pgoutput does for any leaf whose relation is in the tree | phase 9, C7 | Section 9.3 |
| R12 | Phase 10's split refuses a bucket whose tables are user-partitioned, and (this part's request) one that has a columnar table: section 9.7 states both refusals so phase 10 can adopt the messages | phase 10 | Section 9.7 |

### 11.6 What happens to a leaf when its bucket moves

A bucket move (phase 4, and phase 9's tree-aware version) creates, on the target, the
bucket's partitions as standalone tables built with `CREATE TABLE ... (LIKE ...
INCLUDING ALL)`. **`LIKE ... INCLUDING ALL` does not carry the access method**, so
the decision is needed, and it is: **the move and the rebuild recreate every leaf
with the access method it has on the source** (`pg_class.relam` of the source leaf
read in the move's `preparing` step and passed to `CREATE TABLE ... USING`). The
other possibility, a moved leaf arriving as heap and converted again by the policy,
is rejected: it would make a cold partition occupy its uncompressed size on the
target for up to a policy interval, could overflow the target's disk during a
rebalance of a large cluster, and would let the policy and the move race for the same
leaf. The target must have `kingres_columnar` installed, which the move checks in
`preparing` on both nodes (9.3).

A moved columnar leaf is filled by the table-sync `COPY` (bulk, full stripes) and then
by decoded inserts (C7). Before C7 the move is refused (9.3, 9.7).

## 12. Security

- **Who may change storage and policies.** In SQL: `kingres.columnar_set_options`,
  `convert_partition`, `convert_old_partitions`, `timepart_set_policy`,
  `timepart_unset_policy`, `create_time_partitions` and `drop_old_time_partitions`
  need ownership of the table (membership in the owner role) or superuser; the
  administration functions (`columnar_check`, `timepart_run_phase`, `timepart_due`,
  `timepart_begin_run`) are superuser only. `EXECUTE` is revoked from `PUBLIC` on
  every function. In REST: changing the storage format of a table, setting or
  removing a policy and running a policy now need `kingres_admin`; reading storage,
  partitions, policies and runs needs `kingres_readonly`. The reason for admin on
  retention: it destroys data. `kingres_ops` can cancel a running task (existing
  rule) but not start one of these.
- **What the scheduler runs as.** `kingresd` connects with its service role, which
  is a superuser. A run first checks that the table's owner is `created_by` of the
  policy or a role the policy's creator was a member of **[Needs verification]**
  (an ownership change by an unrelated role must stop the policy until someone with
  rights re-creates it; the simple form is: the owner at run time must be the owner
  recorded at creation, else the run fails with `owner_changed`), then runs each
  action under `SET LOCAL ROLE <owner>`, so the DDL and the data copy have the
  owner's privileges, and the new tables are owned by the owner. A superuser
  session can undo `SET ROLE` (`RESET ROLE`), so user code running during a
  conversion could regain superuser rights. The conversion therefore evaluates **no
  user code**: the copy lists columns explicitly, the new table has no triggers,
  and conversion is refused when a `CHECK` constraint, a generated column or a
  column default of the table calls a function that is not in `pg_catalog`, unless
  the table owner is a superuser (10.4 step 1). Creation and retirement run no
  user code (`LIKE` copies default expressions as text; the attach of an empty
  table checks no rows).
- **Search path and quoting.** Every function has `SET search_path = pg_catalog,
  kingres, pg_temp` and builds statements with `format('%I.%I', ...)` or
  `quote_ident`; the `pg_regress` test `security` of `kingres_shard` (every function
  has the setting) is extended to the two new extensions.
- **The stripe catalog holds data values** (minimum and maximum per column). It has
  no privileges for anyone but the extension owner and is read by the access
  method's C code with the heap API; the view `columnar_stripes` returns no
  statistics; `pg_statistic` has the same exposure for a heap table.
- **Corrupt or hostile files.** A reader validates what it decodes before it
  allocates: the page magic and stripe id against the catalog row, `raw_len` of a
  chunk against `rows * typlen` for fixed types and against 1 GB (`MaxAllocSize`
  family limit) for the sum of variable-length ones, the footer CRC, and the offsets
  against the blob length; a failure is `ERRCODE_DATA_CORRUPTED` naming the
  relation and stripe, never a crash or an unbounded allocation. Page checksums
  (if enabled) are the first line.
- **Memory.** One backend holds at most `write_buffer_memory` of stripes and one
  decoded chunk group per needed column; many backends loading at once multiply
  that; the documentation says so.
- **Audit.** `setTableStorage`, `putPartitionPolicy`, `deletePartitionPolicy` and
  `runPartitionPolicy` are audit actions (section 13); a scheduled run is recorded
  with actor `system`. The retention action itself is in the run's action rows.

## 13. Control plane and REST contract

All additions are additive to the contract as it is today (0.6.0 at the time of
writing). They make a new revision whose number is assigned at merge, as the other
phase designs say of theirs; nothing here depends on the number. Phase 9 also adds
`createTablePartition` as `POST .../tables/{tableName}/partitions`; the `GET`
of `listTablePartitions` below is the same path and complements it (phase 9's
`Table.partitioning` property and this part's `partitioning` and `storage`
properties are merged at the revision). `kingres/api/openapi.yaml` is changed before the code, as
[documentation.md](../dev/documentation.md) requires; `kingres/api/README.md` gets
the new rows in "Task kinds and subjects" (the contract check fails if a
`TaskKind` is not in that table, per `scripts/check-contract.mjs`).

### 13.1 Resource additions

`Table` and `TableDetail` (response schemas) gain two optional, nullable
properties (not `required`, so no existing client breaks; `null` means "not
available", as the contract says for unknowns):

```yaml
storage:                       # TableStorage | null
  accessMethod: string | null  # "heap", "columnar", ...; null for a partitioned table
  partitionFormats: { heap: int, columnar: int, other: int } | null   # partitioned tables
  columnar:                    # ColumnarStats | null: totals over the table or its partitions
    compression: none|pglz|lz4|zstd | null
    compressionLevel: int | null
    stripeRowCount: int | null
    chunkRowCount: int | null
    stripes: int64
    rows: int64
    bytesOnDisk: int64
    bytesUncompressed: int64
    compressionRatio: number | null
partitioning:                  # TablePartitioning | null
  column: string
  columnType: date|timestamp|timestamptz
  partitionCount: int
  hasPolicy: boolean
  defaultPartition: boolean
```

`listTables` gains the query parameter `storage` (`heap` or `columnar`); a
partitioned table matches `columnar` when any of its partitions is columnar.
`TableClass` is not changed (the format is not a class).

New operations (every one lists the shared responses; 404 for an unknown table or
database):

| operationId | Method and path | Role | Request | Response |
|---|---|---|---|---|
| `setTableStorage` | `PUT /databases/{db}/schemas/{schema}/tables/{table}/storage` | `kingres_admin` | `{ accessMethod: heap\|columnar, compression?, compressionLevel?, stripeRowCount?, chunkRowCount? }` | 202 `Task` kind `table.setStorage` |
| `setPartitionStorage` | `PUT /databases/{db}/schemas/{schema}/tables/{table}/partitions/{partition}/storage` | `kingres_admin` | the same, plus `sortBy?: string[]` | 202 `Task` kind `table.setStorage`, subject `database/schema.partition` |
| `listTablePartitions` | `GET /databases/{db}/schemas/{schema}/tables/{table}/partitions` | `kingres_readonly` | query `limit`, `cursor` | `PartitionPage`: `items: TablePartition[]` (`name`, `rangeFrom`, `rangeTo`, `accessMethod`, `sizeBytes`, `estimatedRows`, `compressionRatio` or null), `nextCursor` |
| `getPartitionPolicy` | `GET .../tables/{table}/partition-policy` | `kingres_readonly` | | `PartitionPolicy` or 404 `not_found` when the table has none |
| `putPartitionPolicy` | `PUT .../tables/{table}/partition-policy` | `kingres_admin` | `PartitionPolicyRequest` | 200 `PartitionPolicy` (synchronous: it writes a catalog row) |
| `deletePartitionPolicy` | `DELETE .../tables/{table}/partition-policy` | `kingres_admin` | | 204 |
| `runPartitionPolicy` | `POST .../tables/{table}/partition-policy/runs` | `kingres_admin` | `{ asOf?: date-time }` (only for tests and dry replays) | 202 `Task` kind `partition.maintain` |
| `listPartitionPolicyRuns` | `GET .../tables/{table}/partition-policy/runs` | `kingres_readonly` | `limit`, `cursor`, `state` | `PartitionRunPage` |
| `getPartitionPolicyRun` | `GET .../partition-policy/runs/{runId}` | `kingres_readonly` | | `PartitionRun` with `actions` |
| `planPartitionPolicy` | `POST .../tables/{table}/partition-policy/plan` | `kingres_readonly` | `{ asOf?: date-time, policy?: PartitionPolicyRequest }` | `PartitionPlan`: the actions a run would take (a dry run: no change; used by the dialog's preview) |

Schemas:

```yaml
PartitionPolicyRequest:            # all durations are ISO 8601 (P1D, P1M, PT6H)
  partitionEvery: duration         # required
  createAhead: duration            # default P7D
  retention: duration | null
  retentionAction: drop | detach   # default drop
  columnarAfter: duration | null
  columnarSortBy: [string] | null
  compression: none|pglz|lz4|zstd | null
  compressionLevel: int | null
  timezone: string                 # default "UTC"
  runEvery: duration               # default PT1H
  enabled: boolean                 # default true
PartitionPolicy:                   # the request plus
  partitionColumn: string
  partitionColumnType: date|timestamp|timestamptz
  nextRunAt: date-time | null
  lastRun: PartitionRunSummary | null
  health: { partitionsAhead: duration | null, ok: boolean, problems: [aheadLow|lastRunFailed|defaultPartitionPresent|misaligned] }
  createdBy: string
  updatedAt: date-time
PartitionRun:
  id: int64
  asOf: date-time
  trigger: schedule | manual
  state: running | done | partial | failed
  startedAt: date-time
  finishedAt: date-time | null
  startedBy: string                # role, or "system"
  taskId: int64 | null
  error: string | null
  actions: [PartitionAction]
PartitionAction:
  seq: int
  kind: create | convert | drop | detach
  partition: string
  rangeFrom: date-time | null
  rangeTo: date-time | null
  state: running | done | failed | skipped
  detail: string | null            # for example "lock_timeout", "retry_later", "name_conflict"
  bytesBefore: int64 | null
  bytesAfter: int64 | null
  startedAt: date-time
  finishedAt: date-time | null
```

Durations are ISO 8601 strings in the API and `interval` in SQL (PostgreSQL
accepts ISO 8601 durations as `interval` input). The server validates them with
the rules of 10.3 and answers 400 `validation_failed` with the JSON pointer of the
field. `putPartitionPolicy` is synchronous because it only writes the policy; it
does not create partitions, and the response's `nextRunAt` is now, so the
scheduler picks it up at its next tick. `runPartitionPolicy` is the console's "Run
now".

**Task kinds** (added to `TaskKind` and to the README table): `table.setStorage`
(subject type `table`, id `database/schema.table`; for a partition the id is the
partition's `database/schema.partition`) and `partition.maintain` (subject type
`table`, id `database/schema.table`). Steps:

- `table.setStorage` (plain table): `check`, `convert` (`ALTER TABLE ... SET ACCESS
  METHOD`, under `ACCESS EXCLUSIVE`; the step `detail` says so), `analyze`.
- `table.setStorage` (partition): `check`, `convert` (`CALL convert_partition`),
  `analyze`; for a distributed leaf, one `convert` step per group (R6).
- `partition.maintain`: `begin`, `create`, `convert`, `retire`, `finish`
  (10.6).

All steps are idempotent. Cancellation (`cancelTask`, `kingres_ops`) at a step
boundary marks the task failed with `cancelled`; a step in a long SQL call is
cancelled with `pg_cancel_backend` on its session, which rolls the conversion
back (it is one transaction).

**Problem codes** (additive to `ProblemCode`): `extension_missing` (409:
`kingres_columnar` or `kingres_timepart` is not installed in the database, or its
library is not preloaded on a server; the detail names the extension and the
servers), `conversion_refused` (409: the storage change is not possible, the
detail lists the reasons: ledger table, indexes, triggers, publication, a
partition that is a parent, a default partition, `owner_changed`), and
`partition_policy_conflict` (409: a policy cannot be applied to this table, for
example it is not range-partitioned on a time column; 422 `unsupported_operation`
stays for "this server version lacks the operation"). A second request for a
subject with a running task is the existing 409 `operation_in_progress`. Clients
must tolerate unknown codes, as the contract already says.

**Alert kinds** (`AlertKind`, additive): `partitionsAheadLow` (the policy's last
partition ends before the lookahead floor) and `partitionPolicyFailed` (the last
run ended `failed`, or `partial` twice in a row). The monitor runner evaluates
them from `timepart_check` on the leader every few minutes, not every 10 s.
**Audit actions** (`AuditAction`, additive): `setTableStorage`,
`putPartitionPolicy`, `deletePartitionPolicy`, `runPartitionPolicy`.

### 13.2 `kingresd`

- `internal/partsched` (new): the scheduler of 10.6, with an injected clock, a
  setting `partition_scheduler_interval`, and metrics (`kingres_partition_runs_total`,
  `kingres_partition_actions_failed_total`) on the existing Prometheus endpoint.
- `internal/ops/tables.go`: `PrepareSetStorage`, `PrepareMaintain` validate the
  request against the catalog of the database (the table exists, is of the right
  kind, the extension is installed on every server where the table's data lives,
  the library is preloaded) and return the task input, as `PrepareDistribute` does;
  the tasks are in `internal/ops` with the step definitions.
- `internal/store/pgstore/tables.go`: `listTables`/`getTable` add the `storage`
  and `partitioning` properties from a catalog query on the node(s) holding the
  table: `pg_class.relam`, `kingres.columnar_info` summed over partitions,
  `pg_partitioned_table`.
- The `database.create`/`database.update` tasks and `server.add` install the two
  extensions (4.2).
- Forwarding: all of these are mutating cluster operations and are forwarded to
  the leader like the others; reads are answered locally for the catalog and from
  the node that owns a plain table.

## 14. Web console

Pages and components follow `kingres/web/README.md` (Vue 3, TypeScript, generated
types, the mock API, hostile-name tests, the content security policy: no inline
style or script). The tables list is in the database detail page
(`DatabaseDetailPage.vue`), which already has `DistributeTableDialog` and
`EnableLedgerDialog`; these additions sit beside them.

**Tables list.**

- A new column **Storage**: `Row` or `Columnar` as a `StatusBadge`, `Mixed (3
  columnar of 31)` for a partitioned table, and for a columnar table the
  compression ratio as text (`6.2 x`) with the bytes on disk, or `n/a` when null
  (never `0`, per the contract's null rule). A `storage` filter next to the class
  filter.
- The row's actions menu (a `GateButton` per action, so a user without the right
  sees it disabled with the reason, not hidden) gets **Convert storage...** and
  **Time partitioning...** (the latter only for a partitioned table with a time key,
  or a plain table, which first offers to partition it? **no**: it explains that the
  table must already be partitioned by range on a time column and shows the
  statement to do it).

**Convert storage dialog** (`SetStorageDialog.vue`, a `TaskActionDialog`): target
format (Row or Columnar); for columnar the codec (zstd, lz4, pglz, none), level,
and rows per stripe and per chunk group under an "Advanced" disclosure; a notice
that is specific to the kind of table:

- plain table: "Converting rewrites the table and blocks readers and writers until
  it finishes. Use time partitioning for large tables." and the table size;
- partition: "Writers of this partition wait while it is copied; readers are not
  blocked. The partition is replaced in one short step."

Refusals appear before submit, from `conversion_refused` details (a list: "has an
index (`orders_ts_idx`): columnar tables have no indexes in this version", "is a
ledger table", "is in a publication", "has triggers"), not as an opaque error.
After submit the dialog follows the task with `TaskProgress` and its steps.

**Time partitioning dialog** (`PartitionPolicyDialog.vue`): the partition column
(read-only, from the table), **interval** (a select of the allowed shapes: hour,
day, week, month, year, and "every N" for the divisors), **create ahead** (a number
and unit; hint "How far in advance partitions exist; keep it larger than a typical
bucket move"), **retention** (off, or a number and unit; **action** drop or
detach), **convert to columnar after** (off, or a number and unit; with the same
sort and compression options as the storage dialog), time zone, run every. A
**Preview** button calls `planPartitionPolicy` and lists what the next run would
create, convert and drop, with sizes: the user sees the data a retention would
delete before saving. Saving a retention (or lowering it) requires typing the table
name (`TypeToConfirm`), because it deletes data. Validation messages come from the
`validation_failed` JSON pointers, field by field. A table with a default
partition shows the refusal and the hint.

**Partitions panel** (a tab of a table's detail, `PartitionsPanel.vue`): a table of
partitions: name, range, format (badge), size, rows (estimate), ratio, with filters
for the format; a running conversion marks its row with a spinner and the task's
progress; partitions in a column that has no stripe statistics (heap) show `n/a`
for the ratio. The panel shows the policy's **health** line: partitions ahead (for
example `7 days ahead`, or a warning `1 day ahead, below the 3.5 days the policy
keeps`), next run time, last run state, and **Run now**.

**Policy runs** (below the partitions): a table of runs (started, duration, trigger,
state, who, counts of created, converted, dropped, failed), each opening a drawer
with the actions (kind, partition, state, detail, bytes before and after, times). A
failed action shows its detail in words: "Waited 0.5 s for the lock on `events` and
gave up; will retry at the next run" for `swap_lock_timeout`, "Cannot create the
partition: another table is called `events_p20260101`" for `name_conflict`.

**Failure and degraded states.**

- The extension is missing: the Storage column shows `Row` and the actions are
  replaced by an inline notice "Columnar storage is not installed in this database"
  with the one action that installs it (a `database.update` task, admin only).
- The library is not preloaded on one server: the notice names the server and the
  setting.
- A failed `table.setStorage` or run is shown in the Tasks page and in the panel;
  the table is unchanged (conversion is one transaction).
- `unsupported_operation` from an older server shows the standard "not available on
  this server version yet" state.
- The alert `partitionsAheadLow` appears in the alert list and on the dashboard's
  alert count; its link opens the table's panel.

**A read-only user** (`kingres_readonly`): sees the Storage column, the partitions
panel, the policy (read-only fields, no Edit, no Run now), the runs and their
actions; every mutating button is a disabled `GateButton` with the reason ("needs
the kingres_admin role"). A `kingres_ops` user sees the same and can cancel a
running task from the Tasks page. A `kingres_stats` user does not reach these
pages (existing rule).

All names (tables, partitions, columns) go through `SafeName`, as elsewhere, so
the hostile-name mock (`?mock=hostile`) covers the new pages.

## 15. Tests

Conventions follow [testing.md](../dev/testing.md) and the extensions' own layout
(`sql/` and `expected/` for `pg_regress`, `t/` for TAP, `kingres/dev/ext.sh`).

**`kingres_columnar`, regression (one node):**

- `columnar_basic`: create, insert, `COPY`, `CREATE TABLE AS`, select; every codec;
  `TRUNCATE`; rollback and savepoints; `INSERT INTO t SELECT FROM t` (the own-command
  rule, 7.1); a transaction that inserts then selects.
- `columnar_types`: every built-in type class (fixed by value, fixed by reference,
  varlena, arrays, `jsonb`, `numeric`, `text` with long values, composite, domain,
  toasted inputs, expanded objects, nulls in every position), compared with the same
  data in a heap table row by row.
- `columnar_ddl`: `ADD COLUMN` (with and without a default, on a table with old
  stripes), `DROP COLUMN`, `ALTER COLUMN TYPE` (rewrite), `SET ACCESS METHOD` both
  ways, `VACUUM FULL`, `ALTER TABLE ... SET TABLESPACE`, rename, drop and the catalog
  cleanup (5.5).
- `columnar_refusals`: every refusal of section 6 with its message and SQLSTATE,
  including a statement that matches no row of a columnar partition and succeeds.
- `columnar_scan_plans`: `EXPLAIN` of the Columnar Scan, projection, skipping for
  every operator form of 8.1, collation rule, `OR`, parameters, stable functions;
  answers compared with a heap copy and with `enable_custom_scan = off`.
- `columnar_stats`: min/max rule (5.2); `NaN`; long text; `C` and non-`C` collation.
- `columnar_vacuum`: vacuum statistics, tail truncation, `columnar_compact`,
  `columnar_check`, autovacuum triggering from inserts.
- `columnar_analyze`: the sampling of 7.9 against known data.
- `columnar_security`: every function has the pinned `search_path`; privileges.

**`kingres_columnar`, TAP (`t/`):**

- `001_crash_recovery`: kill the server at fault points (before the counter advance,
  after the pages, after the catalog row, after commit), restart, compare; a
  prepared transaction across a crash; `wal_level = minimal` with a table created and
  loaded in one transaction and a crash.
- `002_standby`: streaming standby with `hot_standby`; load, read on the standby
  during the load, equality of counts and sums; a long standby query across a
  `VACUUM` truncation (conflict handling); promotion; `wal_consistency_checking =
  all`.
- `003_backup_pitr`: base backup, archive recovery to a point, equality.
- `004_concurrent_writers`: eight sessions loading and four reading; no lost or
  duplicated row; subtransaction loaders.
- `005_parallel`: C5; every plan shape; the leader's own writes.
- `006_logical` (C7): publications and subscriptions of insert changes, apply to a
  heap target and to a columnar target, decode across restarts and a slot's catch-up,
  `COPY` of a million rows, the refusals of 9.3 before C7.
- `007_two_phase`: `PREPARE TRANSACTION` with columnar inserts, crash, `COMMIT
  PREPARED` and `ROLLBACK PREPARED`.

**`kingres_timepart`, regression with a controlled clock** (`as_of` argument, no
`now()`): `timepart_create` (every interval shape, a time zone with daylight saving,
leap years, month ends, misaligned existing partitions, name conflicts, a default
partition refusal), `timepart_retention` (drop and detach, a partition spanning the
boundary, `columnar_after > retention` refused), `timepart_convert` (a table with
data, the verification, every refusal reason, the sort order effect on skipping,
convert back), `timepart_policy` (set, replace, rename and dump/restore of the
policy, history pruning), `timepart_locks` (a second session holds a lock: the
action fails with `lock_timeout` and the next run succeeds), `timepart_security`.
TAP: a writer to a partition during its conversion (waits, then proceeds against the
new partition with no lost row), a reader across the swap (never sees zero or two
copies), the deadlock-avoidance path of 10.4 step 6, a crash during each phase and
the `interrupted` handling.

**Sharding extension (`kingres_shard`) TAP** (with `KingresShardCompare.pm`): a
columnar distributed table versus the same data in heap on a single node, through
the complete scan, and with every pushdown of phase 2b and part 1 on and off
(ADR 0022 point 3); the row router with `COPY` and batched `INSERT` into a columnar
table; fan-out `UPDATE` fails and rolls back everywhere; a refusal of
`move_bucket` before C7; after C7 a bucket move of a columnar distributed table with
writers running (the test of phase 4, "Writers during a move", with inserts only),
the stale-map `KS001` and a scan racing a switch (`t/026`, `t/031` forms); a
conversion of a leaf racing a move (R5); two-phase commit crash points with columnar
writes; a standby of a shard group and a failover with columnar data; a ledger table
cannot be made columnar and the reverse (L1).

**Control plane** (Go): scheduler unit tests with a fake clock (due selection,
one task per subject, leader change mid-task, a task that fails and the next tick,
idempotent steps interrupted at every step), request validation, role matrix per
operation, contract tests (`contracttest`: every new operationId in
`x-required-role`, every new `TaskKind` in the README table, schema examples),
integration tests with one and three `kingresd` processes: a policy end to end on a
real database (create, run, partitions exist, retention drops, conversion converts,
failure on a lock timeout shown, a second run does nothing), a server restart during
a conversion, `extension_missing`, the read-only role gets 403 on `PUT`.

**Panel** (Vitest and Testing Library on the mock API, plus the browser tests of
`npm run test:browser`): the new column and filter, the dialogs' validation and
preview, `TypeToConfirm` on retention, a conversion's progress and failure, the
health line and the alert, `?mock=hostile` names, 320 px layout and keyboard use,
axe, and the three roles (what a read-only user sees: disabled buttons with a
reason).

## 16. Implementation order

Each step is committable and testable alone, leaves the product correct if the next
never lands, and is sized for one engineer and one independent reviewer. Honest
size: the columnar extension is the largest piece of phase 12. The numbers are
estimates of changed lines of C (not measured; **[Recommendation]**: re-estimate
after C1).

**The smallest useful first version of the columnar format is C1 to C4**, about
5 000 to 7 000 lines of C with tests: `CREATE TABLE ... USING columnar`, inserts and
`COPY`, stripes of compressed column chunks with min/max, the Columnar Scan with
projection and skipping, `VACUUM` and `VACUUM FULL`, crash recovery, standby reads,
`ALTER TABLE ... SET ACCESS METHOD`, conversion of a partition in `kingres_timepart`.
What that version **leaves out**: indexes (so no primary keys, and no columnar
partitions under a parent that has an index), `UPDATE` and `DELETE`, parallel scan,
logical decoding (so no bucket moves of columnar distributed tables and no
publications), reuse of freed space in the middle of the file, compression better
than a general codec, vectorized execution, and any cost estimate of skipping. Time
series data with a time column, no indexes on cold partitions and a retention policy
is the use it serves.

| Step | Content | Depends on | Tests |
|---|---|---|---|
| C1 | `kingres_columnar` skeleton: handler, metapage, resource manager (init, alloc, pages), the stripe catalog, write buffer and flush, serial scan of whole stripes with codec `none`, one chunk group per stripe, `TRUNCATE`, refusals; `contrib/meson.build` lines and the KP-0003 row | none | `columnar_basic`, `columnar_refusals`, `001_crash_recovery`, `002_standby` (basic) |
| C2 | Chunk groups, column chunk encoding of every type class, codecs (`pglz`, `lz4`, `zstd`), options in the metapage and `columnar_set_options`, GUCs, `ADD/DROP COLUMN`, `ALTER TYPE`, `ANALYZE` sampling, `pgstat` counters, the foreign-key event trigger | C1 | `columnar_types`, `columnar_ddl`, `columnar_analyze`, `columnar_security` |
| C3 | Min/max statistics (chunk and stripe), the Columnar Scan custom scan (projection, skipping), `EXPLAIN`, cost, the `kingres_shard` hook chaining check | C2 | `columnar_stats`, `columnar_scan_plans`, tuning benchmark of stripe and chunk sizes |
| C4 | `relation_vacuum` (statistics, tail truncation), `columnar_compact`, `VACUUM FULL` (`relation_copy_for_cluster`), `relation_copy_data`, catalog reconciliation, `columnar_check`, `columnar_info` and `columnar_stripes`, subtransaction buffers, `PREPARE TRANSACTION` flush | C3 | `columnar_vacuum`, `004_concurrent_writers`, `003_backup_pitr`, `007_two_phase` |
| T1 | `kingres_timepart`: tables, `timepart_set_policy`, `create_time_partitions`, `drop_old_time_partitions`, `timepart_plan`, `timepart_run*`, history, `timepart_due`, `timepart_check`; no columnar needed | none (parallel to C1 to C4) | `timepart_*` regression (create, retention, policy, locks, security) |
| T2 | `convert_partition`, `convert_old_partitions`, convert back | C4, T1 | `timepart_convert`, the writer and reader TAP |
| S1 | `kingres_shard`: `create_distributed_table` and `create_reference_table` carry the access method; refusal until then; a bucket move's standalone copies use it; `add_node` | C2 | `kingres_shard` TAP: columnar distributed table versus single node through the complete scan |
| S2 | `kingres_shard`: `move_bucket` and the phase 9 rebuilds refuse columnar relations in the tree with the message of 9.7; `create_distributed_table` sets `REPLICA IDENTITY FULL`; the publication guard; L1 in `kingres_ledger`; phase 10's split refusal text (9.7) | S1 | refusal tests, ledger tests |
| P1 | The contract revision in `openapi.yaml` and the README, generated server types, the mock API of the panel, the contract tests | none | contract tests, panel typecheck |
| P2 | `kingresd`: `storage` and `partitioning` in `listTables` and `getTable`, `listTablePartitions`, the extension install steps in `database.*` and `server.add` | C1, T1, P1 | control-plane integration tests |
| P3 | `table.setStorage` (plain table and partition), `setTableStorage`, `setPartitionStorage` | C4, T2, P2 | integration tests |
| P4 | Policy resources: `get/put/deletePartitionPolicy`, `planPartitionPolicy` | T1, P2 | integration tests, role matrix |
| P5 | The scheduler and `partition.maintain`, `runPartitionPolicy`, the runs resources, the alerts and audit | T2, P4 | scheduler tests with a fake clock, three-server integration test |
| W1 | Console: the Storage column and filter, the partitions panel, the runs list (read-only) | P2, P5 | panel and browser tests |
| W2 | Console: the convert-storage and time-partitioning dialogs, preview, confirmation | P3, P4, W1 | panel and browser tests, end-to-end run |
| C5 | Parallel scan | C4 | `005_parallel`, plan tests |
| C6 | B-tree indexes: row-number reservation, TID fetch, index build and vacuum, unique | C4 | index tests, `timepart_convert` with indexed parents |
| C7 | Logical decoding of inserts (`COLUMNAR_LOGICAL_INSERT`, `rm_decode`), lifting the publication guard | C4 | `006_logical` |
| S3 | `kingres_shard`: bucket moves of columnar relations (the copy tables `USING` the source's access method, `columnar_compact` before the switch), lifting the refusal of S2 | C7, S1, phase 9 increment 9.5 | the move tests of 9.3 and 15 |
| C8 | Free-extent reuse | C4 | `columnar_vacuum` additions |
| D1a | Policies on distributed tables, heap only: `create_partition`, `drop_partition`, `detach_partition`, retry and backoff, the leaf budget (11.2) | phase 9, T1, P5 | `0xx_timepart_dist` TAP: creation during a move is retried, retention, `max_partition_leaves` |
| D1b | Conversion of the leaves of a distributed table (11.4), R4, R5, R6 | D1a, T2, S3 | TAP: a conversion racing a move, a mixed state, a move of a table with columnar leaves |

A first release can ship after W2 (C1 to C4, T1, T2, S1, S2, P1 to P5, W1, W2):
columnar storage and time partitioning for non-distributed tables and for
distributed tables with heap partitions only (D1a, which needs phase 9). S3 and D1b
make the combination of columnar storage, moves and time partitioning complete.

## 17. Open questions, with recommendations

| # | Question | **[Recommendation]** (decided unless a verification item changes it) |
|---|---|---|
| 1 | Name of the access method: `columnar` or `kingres_columnar` | `columnar`: short, the name users of other systems expect (`USING columnar`), no clash because nothing else providing that name is bundled. An ADR records it. If a user installs a foreign extension that also names its access method `columnar`, `CREATE ACCESS METHOD` fails clearly |
| 2 | Default codec | `zstd` level 3 where built in, then `lz4`, then `pglz`; a table's codec is fixed at creation in its metapage and changed only for new stripes |
| 3 | Stripe and chunk sizes | 150 000 and 10 000 rows (the documented defaults of an existing columnar product, taken as a starting point); tune in C3 with a benchmark on a time series of 20 columns |
| 4 | `UPDATE` and `DELETE` | Not in phase 12 (7.7); a deletion-vector design is a separate later phase if demand appears |
| 5 | Resource manager id | `RM_EXPERIMENTAL_ID` in development; reserve a real id on the registry before the first release; record it in the extension README, because it is part of the WAL format |
| 6 | Is `shared_preload_libraries` acceptable for `kingres_columnar` | Yes. It is required by the resource manager, the other two extensions already require it, and the package default lists all three |
| 7 | Retention default | `drop`, with no default retention value (a policy without `retention` keeps everything), the table name typed in the console to confirm, and a preview of the data it would delete |
| 8 | Converting a plain (non-partitioned) table without blocking | Not in this part: `ALTER TABLE ... SET ACCESS METHOD` with a clear warning. Build-and-swap needs dependent views, foreign keys, publications and privileges handled; revisit if users ask. The recommended path for large tables is to partition them by time first |
| 9 | Should partitions older than a limit stop accepting writes after conversion | No: they accept inserts like any columnar table; updates fail (7.7) |
| 10 | Default partition | Refused for policy tables (10.2) |
| 11 | Scheduler tick and per-policy interval | 60 s tick; `run_every` default 1 hour; lookahead 7 days; `ahead_low` below half of the lookahead |
| 12 | Where policies live for plain tables | In the database, in `kingres.timepart_policy` on the node holding the table, dumped with the extension configuration (4.4) |
| 13 | Integer or epoch partition keys, list partitions | Not in this part: refused with a message. Revisit after the date-based case is used |
| 14 | `pg_cron` support | Not used; the SQL procedures are callable from it (10.6) |
| 15 | Columnar reference tables | Refused in 0.1 (9.5) |
| 16 | `convert` sort order | Default: the partition column. Option `columnarSortBy` for others. A sort of a large partition spills to disk by `work_mem`; the conversion step sets `work_mem` for the transaction from a `kingres_timepart.convert_work_mem` setting (default 256MB) |
| 17 | When the console warns of leaf counts (11) | `partitionLeafWarning` threshold 10 000 leaves; adjustable setting of `kingresd` |
| 18 | Does a moved leaf keep its access method, or arrive as heap and be converted again by the policy | It keeps it (11.6): the move and the rebuild pass `USING <access method>` for each leaf; the alternative doubles the cold data's disk use on the target and races the policy |
| 19 | Should a columnar bucket be splittable | No (9.7); rebalance by moving buckets, or convert to heap, split, convert back |

## 18. Known limits after this phase

Columnar tables:

- Append-only: no `UPDATE`, `DELETE`, row locks, `ON CONFLICT DO UPDATE`,
  `MERGE` writes, `TABLESAMPLE`, `ctid`.
- No indexes (until C6; then B-tree only), so no primary key or unique constraint
  before C6; no foreign keys at all; no `CLUSTER`.
- No unlogged or temporary columnar tables.
- Tiny transactions make tiny stripes; `columnar_compact` and the policy's
  conversion are the remedy. Savepoints that write each make a stripe.
- Space of failed loads is not reused in the middle of the file before C8;
  `VACUUM FULL` returns it.
- The options of a table are not in a dump.
- No logical decoding before C7: no publication of a columnar table, no bucket move
  of a columnar bucket, an event trigger and a check to prevent a silent miss.
- No parallel scan before C5; no vectorized execution; whole chunks are decoded;
  point lookups are slow; skipping is not in the cost estimate.
- The files are not portable across endianness; every node, standby and restore
  target must preload the library for as long as its WAL exists.
- A table with a ledger cannot be columnar.

Time partitioning:

- Range partitions on one `date`, `timestamp` or `timestamptz` column, equal width
  on a grid; no default partition; no other key types.
- Retention drops whole partitions; data may live up to one interval longer than
  `retention`.
- A conversion blocks writers of the partition while it copies and the parent for a
  short swap, and fails (and retries at the next run) when it cannot get the swap
  lock in 500 ms; a busy table may need several runs.
- `DETACH ... CONCURRENTLY` is not used by the SQL layer.
- Nothing runs when `kingresd` is down; the lookahead and the alert are the
  defence.
- A plain table converts with a full rewrite under `ACCESS EXCLUSIVE`.
- Distributed time-partitioned tables are available only as far as phase 9 and
  step D1 provide (section 11); a daily policy on a table with many buckets makes
  many relations.
- A superuser session that runs a conversion for a table owned by an ordinary role
  evaluates no user code; tables with user functions in constraints or defaults are
  refused for conversion unless the owner is a superuser.

## 19. Needs verification

| # | Item | What would settle it |
|---|---|---|
| V1 | A `contrib` module can link `libzstd` and `liblz4` through the top-level `meson.build` variables, and the build settings of the release package give both (2.3) | A meson build of `contrib/kingres_columnar` against the release options; `ldd` of the result |
| V2 | The exact text of PostgreSQL's error for a system column (`ctid`) of a virtual slot (section 6) | A test |
| V3 | The planner makes no TID range path for a relation whose access method has no `scan_set_tidrange` (section 6) | `EXPLAIN` of `WHERE ctid > ...` |
| V4 | The `pgstat` counters of an AM's `pgstat_count_heap_insert` drive autovacuum's insert-based vacuum and analyze (section 6) | An insert-only table with a low threshold; `pg_stat_user_tables` |
| V5 | `sql_drop` fires for the internal drop of a rewrite's temporary relation (5.5) | An event trigger test around `VACUUM FULL` and `SET ACCESS METHOD` |
| V6 | A restored full-page image beyond the end of the file extends it in redo (7.3) | `002_standby` with a load larger than the standby's file, and `test_custom_rmgrs` as the reference |
| V7 | Holding the relation extension lock across several `ExtendBufferedRelBy` calls keeps the pages of a stripe contiguous (7.2) | A concurrent-writer test that checks `first_block + block_count` ranges for overlap and holes |
| V8 | `pg_rewind` and the incremental backup (WAL summarizer) handle records of a custom resource manager from their block references (7.4, 7.6) | A rewind and an incremental backup test with columnar data |
| V9 | The leader's flush in `BeginCustomScan` happens before `EnterParallelMode`, and a parallel worker never writes (8.4) | `005_parallel` with a transaction that wrote first |
| V10 | All functions needed by `rm_decode` are exported and sufficient (9.3) | Writing it in C7; a build on every platform of the package |
| V11 | `REPLICA IDENTITY FULL` and a publication work on a table of a custom access method; the publication guard catches every form (9.3) | `006_logical`, and a test per form of `CREATE/ALTER PUBLICATION` |
| V12 | The `Agg` of a phase 2b partial fragment is still the top node over a custom scan (9.2) | The aggregate pushdown tests with columnar tables |
| V13 | `ANALYZE`'s estimate and histogram from the stripe-share sampling are right (7.9) | `columnar_analyze` on a table of many stripes against exact values |
| V14 | `DETACH PARTITION ... CONCURRENTLY` fails inside a procedure (10.4) | A test calling it from a procedure |
| V15 | Daylight-saving behaviour of bounds in a zone that has it (10.3) | `timepart_create` with `Europe/Berlin` over a change |
| V16 | `client_connection_check_interval` cancels a statement of a vanished client in the service session (10.7) | A TAP test that kills the client mid-conversion |
| V17 | The internal API's statement call can run the policy catalog query on any server for a user database (10.6) | An integration test of the scheduler on three servers |
| V18 | The lock mode phase 4's `preparing` step takes on `kingres.map_version`, for R5 | Read `bucket_move.c` and test a swap against a move |
| V19 | `add_node` creates tables with their access method (4.2) | Read `node_admin.c` and test with a columnar table |
| V20 | Owner check of a policy run (12): whether "owner at creation" is the right rule | A security review of the run, with the ownership-change cases |
| V21 | Whether the columnar compression of TimescaleDB is in its `tsl` directory (2.1) | Read the repository's licence map; not needed for the decision, which rejects it on its own terms |
| V22 | The licence of the PGXN `storage_engine` fork (2.1) | Read its `LICENSE`; not needed, it is derived from Hydra Columnar |
| V23 | A foreign data wrapper's data is not in the WAL (`cstore_fdw`, 2.1) | Read its code; not needed, it does not support 18 |
| V24 | `kingres_ledger`'s `table_rewrite` trigger rejects `SET ACCESS METHOD` of a ledger table (9.6) | A test in the ledger suite |
| V25 | `create_partition` of phase 9 refuses with an error of class `55006` while a move or operation holds the group, and the runner can tell it from other errors (11.2) | Read phase 9's implementation notes; a TAP test that starts a move and creates a partition |
| V26 | A procedure called through the internal API's statement call may `COMMIT` (11.5, R6) | An integration test |
| V27 | Phase 10's split of a bucket whose tables include a columnar table is refused in `preparing` with the text of 9.7 (the refusal belongs in phase 10's code, S2 asks for it) | A split test on a columnar table |

## 20. Findings in existing documents and code

1. **`create_distributed_table` ignores the access method** (section 9.1). The
   committed `kingres_shard` code and install script never mention `relam`, so a
   columnar source table would silently become a heap distributed table. Needs the
   check of S1 now, whichever phase lands first.
2. **`ledger_enable` does not check the access method** (section 9.6): nothing in
   `contrib/kingres_ledger/src` mentions `relam`, so nothing stops a ledger on a
   columnar table once the access method exists.
3. **`add_node` and the bucket-move copy tables** are created from "the columns,
   defaults, constraints and indexes of the parent" (phase 4, "copying") with no
   mention of the access method (section 9.1).
4. **Phase 4's replica-identity precondition** ("a primary key or `REPLICA IDENTITY
   FULL`") is met by columnar tables only through `REPLICA IDENTITY FULL`, which
   `create_distributed_table` and the conversion must set (9.3); a columnar table has
   no primary key before C6.
5. **Phase 9 accepts `USING` only for `heap` in `create_partition`** and says that
   `LIKE ... INCLUDING ALL` does not carry the access method; phase 9 increment 9.5
   passes the access method of each leaf (section 11.6 relies on it). Phase 10 creates
   its split children with `LIKE ... INCLUDING ALL` too, so a split of a non-heap
   bucket would silently produce heap children; section 9.7 refuses the split instead.
   Phase 10 requires a primary key for a split and takes `FOR SHARE SKIP LOCKED`
   row locks in the backfill: neither is possible on a columnar table.
6. **`kingres_shard` refuses `ATTACH`/`DETACH PARTITION` and `SET ACCESS METHOD` on
   distributed tables** (README, "DDL on distributed and reference tables"), and
   calls a bucket partition "not a table of the user". Phase 9 turns the statements
   on user partitions into function calls; a conversion of a leaf is still refused
   unless an extension-owned authorization exists (section 11.5, R4).
5. **`kingres_shard` refuses `ATTACH`/`DETACH PARTITION` and `SET ACCESS METHOD` on
   distributed tables** (README, "DDL on distributed and reference tables"), and
   calls a bucket partition "not a table of the user". That is correct for phase 2a
   to 4 and is the reason section 11 needs R4.
7. **KP-0003 in the core patch register** lists exactly two `subdir` lines; adding
   two more updates that row (section 3). No new patch id.
8. **`PrepareDistribute` in `kingres/control-plane/internal/ops/tables.go`** (at HEAD)
   answers reference tables with "Reference tables are not supported by the installed
   kingres_shard version", while the contract (`distributeTable`, method
   `reference`) and the extension (0.2 onward) support them. It may be a stale guard;
   **[Needs verification]** before section 13's `table.setStorage` copies its
   pattern.
9. **The control plane has no recurring scheduler** (only the monitor's ticker and
   the task lease renewal), and the comment "the scheduled digest" in
   `internal/ops/ledger.go` refers to a digest schedule that is not in the committed
   code **[Needs verification]**. Section 10.6 adds the first one; if a digest
   scheduler is planned, the two should share `internal/partsched`'s tick and clock
   (name it `internal/schedule`).
10. **`kingres/api/README.md` and `check-contract.mjs`** require every `TaskKind` to
   have a row in the "Task kinds and subjects" table (section 13), so the
   contract change touches the README in the same commit.

## 21. Found while building

### C5, parallel scan (2026-11-19)

- **The test is `t/043_parallel.pl`**, with the plan test `columnar_parallel`; section 15 names it
  `005_parallel`, and `t/005` is the corruption test.
- **No plan has a parallel sequential scan of a columnar table (6, 8.4).** The rows a transaction
  wrote and still holds in memory must be written as stripes before the executor enters parallel
  mode, because a catalog row cannot be inserted in it. For a plan node the only place that runs
  before that is `BeginCustomScan`; the table access method has no such callback. So the plain
  `parallelscan_*` callbacks stay as they were, the planner's partial sequential scan paths of a
  columnar table are removed, and with `kingres_columnar.enable_custom_scan` off there is no
  parallel scan.
- **What cannot be written before parallel mode** (rows of an enclosing subtransaction, rows that
  another scan of the session still reads) makes the planner offer no parallel path for the table.
  A plan prepared earlier that meets such rows fails with an error; it never answers with rows
  missing.
- **The scan without workers is parallel safe**: in a worker it reads the whole table (the inner
  side of a partial join, a whole child of a Parallel Append, the single copy that
  `debug_parallel_query` runs). A query that a function runs inside a worker is still refused: the
  worker remembers the plan it was started for, and only scans of that plan run there.
- **The shared state** holds, beside the counter of the next stripe, the length and a fingerprint
  of the leader's stripe list, which every participant compares with its own, and one slot of
  counters per participant for `EXPLAIN ANALYZE`.
- **Workers**: at most the number of stripes less one. The size the planner compares with
  `min_parallel_table_scan_size` is the compressed size, so a columnar table reaches a parallel
  plan later than the same data in a heap table; the storage parameter `parallel_workers`
  overrides it. A factor for the compressed size is open.
- **No change** of the on-disk format, the metapage, the WAL, the SQL objects or the version.

### Decisions for C6, indexes (2026-11-19)

C6 is built in four parts: the reservation of row numbers and the early catalog row with no index
yet (C6a), the fetch by TID and the index build (C6b), unique indexes and index vacuum (C6c),
compaction with the standby and crash tests (C6d). Decided where sections 5.3 and 7.8 are silent:

- A write buffer that reserves takes a stripe's worth of row numbers; the unused tail is given back
  when it is still the end of the range, otherwise the gap stays.
- While a buffer holds a reservation, a subtransaction that commits writes it as a stripe of its
  own; it is not merged into the buffer of its parent.
- Reservation applies only to a table that has an index.
- `columnar_compact` on a table with indexes rebuilds every index of the table in the same
  transaction under the lock that this needs; `CLUSTER` stays refused.
- The catalog of stripes gains a state "reserved, not written", so C6 is version 0.2 with an
  upgrade script; a table written by 0.1 stays readable and writable.

### T2, conversion of partitions (2026-11-19)

`kingres_timepart` is version 0.2 with this increment (an upgrade script and a full script; eight new
functions, eleven replaced with their signatures, one dropped; the `upgrade` regression test compares
an upgraded catalog with a fresh one).

- **The reasoning about deadlocks in 10.4 step 6 and 10.5 does not hold for a writer that has
  waited.** A writer through the parent holds a lock on the parent and waits for the partition that
  the copy holds; its own `deadlock_timeout` began when it started to wait, perhaps a second before
  the swap asks for the parent. When it fires inside the 500 ms of the swap, the writer is the victim
  (seen in a test). Before it asks for the parent, the swap therefore reads `pg_locks` and gives up
  at once (`swap_lock_timeout`) when a session that holds the parent waits for the partition. The
  500 ms wait stays for readers and prepared transactions.
- **"A writer waits, then goes on against the new partition" (10.4 step 3, 15) is true only for a
  writer that names the partition.** A writer through the parent makes the conversion give way; its
  row goes into the old partition and the next run converts it. A partition that is written through
  the parent at every attempt is never converted; `timepart_check` reports it as `convert_blocked`.
- **The lock on the partition is `SHARE ROW EXCLUSIVE`, not `SHARE`**: the same for readers and
  writers, and it also keeps out a plain `CREATE INDEX`, whose index would be dropped with the old
  partition, and a second conversion.
- **The row counts of `columnar_info` cannot be compared inside the transaction** (10.4 step 5): a
  columnar table writes its stripes at commit. The two `count(*)` are compared under the lock; the
  sizes and the rows go into the history in a transaction after the commit, which also runs the
  `ANALYZE`.
- **A conversion is tried once per run**, not five times: it copies the whole partition. A lock that
  is not granted is a failed action of a partial run, and the next run is due in at most five
  minutes. A refusal is a skipped action of a run that is done.
- **Refusals added to the list of 10.4 step 1**, each because the drop of the old partition or the
  attach would lose or break something: a trigger of the partition's own of any kind, extended
  statistics, an identity column, a sequence the partition owns, an owner other than the table's, an
  isolation level other than READ COMMITTED (a row committed between an older snapshot and the lock
  would be dropped uncopied), a default partition, a partition that is not permanent, options with
  a heap target. For a columnar target, what the columnar method refuses is found before the attach
  fails: an index on the partition or on the parent (named in the message; lifted when C6 is
  built), a foreign key, an `AFTER ROW` trigger, a publication that would include the partition.
- **The rule of section 12 about functions outside `pg_catalog`** is replaced by the rule every
  other action of a run has: a superuser's session runs the code of an owner that is not a superuser
  only with `kingres_timepart.trust_table_owners` on. Whether the control plane needs the narrower
  rule of the design is a decision for the part that builds `table.setStorage`.
- **`convert_old_partitions` stops at the first partition that is refused or fails** (a procedure
  that commits cannot catch an error); the policy path records each partition and goes on.
- **`columnar_options` of a policy is checked when the policy is set** (the keys and ranges of
  `columnar_set_options`); 0.1 accepted any object.
- **Tests need `kingres_columnar` installed and preloaded** in the test server.

What C6 changes for the conversion: the two index refusals go; the conversion creates the indexes of
the parent on the new table before the swap, so that the attach finds them and scans nothing under
the strongest lock; an index of the partition's own is created again on the new table; a primary key
or unique constraint of the parent needs the uniqueness check of C6c.

### C6a, row numbers reserved at insert time (2026-11-19)

`kingres_columnar` is version 0.2 with this increment. It builds the base for indexes and no index;
a developer setting (`kingres_columnar.reserve_row_numbers`) switches it on until C6b does so for a
table that has an index.

- **Reservation.** A write buffer that reserves takes `stripe_row_count` numbers from the metapage
  with the existing allocation record when its first row goes in; row `i` of the buffer is number
  `first + i`, and its TID is the one every scan already showed. The unused tail goes back, with the
  new record `XLOG_KC_RETURN`, only when it is still the end of the range. An abort gives nothing
  back, since the abort callbacks may not touch the file: an aborted writer wastes at most one
  stripe's worth of numbers.
- **The largest row number** is the largest that fits a TID (about 8.8e12), no longer the largest
  64-bit number.
- **The early catalog row.** `kingres.columnar_stripe` has a column `reserved`. The row of a stripe
  is inserted at the first insert with the numbers reserved and nothing else, and the flush updates
  it. Two things section 7.8 does not say: PostgreSQL refuses to update a row of its own command, so
  the writer passes the command counter before the update when the flush happens in the command
  that inserted the row; and the row can move between the insert and the flush (a rewrite of the
  catalog), so the flush finds it by its key and keeps no TID.
- **Hand-over.** A buffer that reserves is never merged; a subtransaction that commits writes it as
  a stripe of its own; a buffer that a cursor holds becomes the parent's with its reservation. A row
  goes only into a buffer made under the same setting.
- **Not reserved**: the rows of a rewrite and of compaction, and rows written in parallel mode (a
  `CREATE TABLE AS` with a parallel query writes in the leader in parallel mode, where the update of
  the catalog row is not allowed).
- **Versions.** Format version 3 of the metapage means "may reserve"; version 2 stays readable and
  writable and never reserves; a new file is version 3 only when the catalog has the new column.
  Both shapes of the catalog are served, so the time between replacing the library and `ALTER
  EXTENSION UPDATE` works. The 0.1 library refuses a version 3 file and the new catalog: there is no
  way back.
- **The check.** A reserved row of an aborted or crashed writer is an ordinary dead row; one of a
  live or prepared writer is never visible to the check or the reconcile. A reserved row that a
  committed transaction left is a new kind of finding, `reserved_row_left`.
- **Serializable.** A reader that skips the reserved row of a writer in progress reports the
  dependency at the writer's first insert, earlier than before.
- **On a hot standby** every stripe of a reserving buffer leaves one dead version of a catalog row,
  and the replay of its pruning can cancel a query with an old snapshot.
- **Open.** With reservation on everywhere, the memory of a backend that writes 1500 partitions is
  27 MB where the test of 0.1 allows 18 MB; this is examined with C6b, where tables with an index
  reserve.

What C6b finds: a function from a row number to a TID, the numbers returned by the insert paths, one
place that decides whether a buffer reserves, and no fetch by TID yet for rows still in a buffer.

### C6b, B-tree indexes that are not unique (2026-11-19)

No change of the on-disk format, the catalog or the WAL beyond C6a.

- **The TID of a row** is its row number: block `number / 2048`, offset `number % 2048 + 1`.
- **No bitmap scans.** The bitmap of the server holds at most `MaxHeapTuplesPerPage` (291) offsets
  per block and a columnar block has 2048, so the method offers none; no index-only scans, for the
  lack of a visibility map; no parallel index scans. Ordered and backward scans, merge joins and
  nested loops with an inner index scan work. PostgreSQL 18 has a hook where the planner reads the
  properties of an index and 19 has not, so the properties are cleared in the path-list hook and
  the index paths are made again: one source for both majors.
- **The cost of a fetch** is one chunk group with all its columns, kept for the next fetch of the
  same group. On 4 million rows a point lookup took 0.8 ms where a heap table took 0.2 ms; a range
  of one percent took 11 ms by the index against 425 ms by the scan (a shared machine: indicative).
- **The first index of a table whose rows the transaction still holds in memory**: the build writes
  the rows of the running subtransaction first, so they get their numbers; rows of an enclosing
  subtransaction, or rows a cursor holds, make the statement fail.
- **`VACUUM` leaves the entries of rolled-back rows** in the index until C6c; the fetch filters
  them, since a number that no visible stripe and no buffer of the transaction has answers nothing.
- **Refused until later increments**: unique indexes, primary keys and unique constraints (C6c);
  `columnar_compact` on a table with an index (C6d). Refused for good: `CONCURRENTLY` builds, which
  are refused before anything is made, the other index methods, `CLUSTER`, exclusion constraints,
  and an index on a file written by 0.1, which must be rewritten first.
- **A parallel index build** uses the parallel-scan callbacks of the access method with a shared
  counter of stripes; it is refused when the transaction holds unwritten rows.
- **The memory of reservation** (the open point of C6a) is about 0.7 kB of private memory per
  stripe reserved in a transaction; the 27 MB seen before was shared memory that a process counts
  when it first touches pages.
- **`columnar_check` does not compare index entries with stripes** yet; it comes with index vacuum.
- **Open.** One guard of the fetch (a row offset beyond the visible rows of a stripe) is neither
  reached by a test nor proven unreachable.

For the conversion of partitions in `kingres_timepart`: the refusal of a partition whose parent has
an index can be lifted for B-tree indexes that are not unique once the conversion creates them on
the new table; unique indexes of the parent wait for C6c.

### C6c, unique indexes and index vacuum (2026-11-19)

- **The unique check** of a B-tree asks the access method for each candidate TID with a dirty
  snapshot; the answer is the state of the row number in the stripe catalog alone: a superseded
  version holds nothing, a version of this transaction or a committed one is a row, a version of a
  transaction in progress means "wait for its id", a dead one is nothing. A row still in a buffer of
  the same backend is found there by its number. The claim of every writer is its reserved catalog
  row, so another backend finds it.
- **A lock of the reservation.** Between taking row numbers from the metapage and inserting the
  claim, an index vacuum must not read the metapage and the catalog, or it would call entries of a
  number no range covers dead. Writers hold a lock of an advisory class in a shared mode across
  that gap; the index vacuum holds it exclusively while it reads. No writer waits for another
  transaction inside the gap, so the lock is never part of a deadlock; a waiter on a key waits on
  the writer's transaction id, never on this lock or a catalog row.
- **Index vacuum** removes entries whose number lies below the lowest number of a running or own
  version and in no live stripe range, then cleans up the index; `INDEX_CLEANUP off` is respected.
  An index file does not shrink, as for a heap table; what goes is its entries.
- **Refused**: exclusion constraints, deferrable unique constraints, every `ON CONFLICT` on a
  columnar table with an index (speculative insertion needs a row lock), and a foreign key that
  references a columnar table.
- **`kingres.columnar_check_indexes(rel)`** reports a visible row that an index lacks; an index of
  more than 50 million entries is not checked.
- **Not done**: the return of the row numbers of aborted writers (a mistake there is a duplicate
  TID; the cost is that workloads which roll back large buffers use the number space faster), and a
  fetch that decodes only the columns a plan reads (the interface does not say which).
- **For `kingres_timepart`**: a partition of a parent with unique or non-unique B-tree indexes,
  primary keys included, can be converted when its file is of format 3 and no constraint is
  deferrable or an exclusion; the conversion must create the indexes on the new table before the
  swap, and rows get new numbers. Foreign keys stay refused.
- On 6 million rows of random text (217 MB columnar, 669 MB heap): an index build took 4.6 s
  against 5.8 s for heap; a point lookup 4.7 ms against 0.6 ms; a range of one percent by the index
  43 ms against 42 ms (indicative, a shared machine).

### C6d, compaction of a table with indexes (2026-11-19)

- **Decided differently from the plan of C6b.** `columnar_compact` does not rebuild the indexes of
  the table: a rebuild would name only the new row numbers, and a snapshot older than the compaction
  that still sees the old stripes would find none of its rows through an index; preventing that
  needs a lock that blocks every reader for the whole compaction. The compaction adds an entry for
  every copied row to every index instead (predicates and expressions evaluated, no uniqueness
  check), and VACUUM removes the old entries once no snapshot can see the old stripes. Locks: those
  of a writer. Cost: a second decode of the copied rows, an insertion per row and index, and two
  entries per copied row until the next VACUUM.
- **The unique check** treats a stripe row deleted by a running or aborted transaction as still
  holding its rows (after a rolled-back compaction it would otherwise admit a duplicate).
- **The row numbers of aborted writers are never given back**: their index entries would find
  another row with another key, since a plain index scan does not recheck the key. Only the unused
  tail of a committed buffer is returned.
- **For `kingres_timepart`**: compaction of a columnar partition with indexes is allowed now;
  VACUUM after it removes the old entries.

### T2 with indexes (2026-11-19)

- A partition whose table has B-tree indexes of immediate checking (unique ones and primary keys
  included) is converted: the new table is loaded first, its indexes are built afterwards under
  temporary names so that the attach builds nothing under the lock of the parent, and the names and
  comments are restored after the swap. No index entry is carried.
- Refused: other index methods, exclusion constraints, deferrable keys, invalid indexes, foreign keys,
  `AFTER ROW` triggers, a replica identity that uses an index, and a table with indexes while
  `kingres_columnar` is still at 0.1 (a new table would be of format 2, which cannot have an index;
  no SQL function reads the format of an existing columnar file).
- **A conversion cannot end with `VACUUM`**: PostgreSQL refuses it inside a function or procedure,
  also after a `COMMIT`. The conversion runs `ANALYZE`; whoever schedules it runs `VACUUM` afterwards
  as a statement of its own.

### C7, logical decoding of inserts (2026-11-19)

- **The record.** Every stripe that is not a compaction copy also writes `LOGICAL_INSERT`
  records (`XLOG_KC_LOGICAL`, no page): the rows as heap tuples, 512 rows or about 1 MB to a
  record, by the transaction or subtransaction that writes the stripe, with its origin. They are
  written only when `wal_level = logical` and the table is logically logged, so the WAL of a load
  roughly doubles there and is unchanged at any other level. Redo ignores them, so standbys and
  base backups are not affected.
- **`rm_decode`** queues one insert per tuple, as `heap_decode` does for an insert; the same
  source builds on both majors. Inside one transaction the order across tables is not the
  statement order (rows are decoded when their stripe is written: at commit, at prepare, or at a
  flush by size or memory); the order within a table is.
- **Subtransactions and two-phase.** The xid of a record is the one whose commit makes the
  stripe visible, so a stripe flushed inside a subtransaction that rolls back is dropped with it.
  The stripe is written at `PREPARE`, so two-phase decoding holds the rows until
  `COMMIT PREPARED`.
- **The guard.** A publication that names a columnar table is accepted now. Still refused, once a
  columnar table exists: a publication that would include the stripe catalog
  `kingres.columnar_stripe` (`FOR ALL TABLES`, `FOR TABLES IN SCHEMA kingres`), whose rows a
  subscriber must not apply. Only inserts exist, so no replica identity is needed;
  `REPLICA IDENTITY FULL` is accepted.
- **A defect of C6c found on the way.** A unique index whose leaf page filled with the entries of
  rolled-back inserts of the same keys made the B-tree ask the access method for bottom-up
  deletion, which raised "columnar tables have no indexes in this version" and failed the insert.
  The answer is now "no entry may go": the page splits and `VACUUM` removes the entries.
- **For bucket moves in `kingres_shard` (S3).** The copy tables are created `USING` the source's
  access method and loaded through `COPY`, then compacted before the switch. The move's
  publication names tables with `FOR TABLE` (or the root with `publish_via_partition_root`),
  never `FOR ALL TABLES`. The publisher needs the library preloaded and `wal_level = logical`. A
  columnar leaf under a root published through the root is not tested yet. The refusal of
  columnar relations in step 1 of increment 9.3 can be removed when S3 is built.
- **Tests and mutants.** `t/061_logical.pl` compares a subscriber row by row with the publisher
  (every column type, a 2 MB value, `COPY`, 30 001 rows, savepoints, prepared transactions,
  reserved row numbers and an index, four sessions, compaction, `TRUNCATE`, two crashes, the
  origin, a physical standby). Of 17 one-line mutants 12 are killed; the 5 that survive change
  nothing observable (for example the top transaction id, which at `wal_level = logical` is never
  needed because the assignment of a subtransaction is logged at once).

### C8, reuse of free space in the file (2026-11-19)

- **Where it differs from section 7.5.** The design finds free pages by walking stripe headers
  and the writer transaction named on each page. Pages carry no transaction id here (a choice of
  C1: an id on a page would need freezing), so such a walk cannot tell a crashed writer's pages
  from a running one's. Instead `VACUUM` takes a `SHARE` lock on the table without waiting,
  retrying every 50 ms for `vacuum_truncate_lock_timeout`, which excludes every running writer
  (prepared transactions included); the stripe catalog alone then says which blocks are used. No
  page is read and readers are not blocked. The cost: a table that never has a moment without a
  writer gets no list, and its file grows as before (`DEBUG2` says that `VACUUM` gave up).
  Decided by the integrator: accepted.
- **The list.** Up to 400 free extents in the metapage, in space that was zero, with a checksum of
  its own; a list that is not sound (unsorted, overlapping, past the end of the file) reads as
  empty. An older binary ignores the list and drops it when it rewrites the page, which loses
  space, not data, so the version stays 0.2. `pd_lower` now covers the list, or a full-page image
  would leave it out.
- **The take.** A stripe write takes the first extent that fits, under the metapage lock, and
  logs the metapage before any page is written; otherwise the file is extended as before. A crash
  after the take and before a page loses only the run's place in the list; the next `VACUUM`
  lists it again. A taken run is never given out twice after a crash, because the take is logged.
- **Standbys.** `XLOG_KC_FREE` carries the newest transaction that deleted a stripe whose blocks
  are listed for the first time, and redo cancels the standby queries that may still read them
  before any of them can be written over. With `hot_standby_feedback` such stripes are not listed.
- **Check.** `columnar_check` reports `free_extent_overlaps_stripe`. A damaged list is not tested
  (it needs a hand-made page checksum).
- **Tests and mutants.** `t/062_free_extents.pl`: in a steady load of small inserts, compactions
  and vacuums over 40 rounds the vacuumed file stays at 2.0 MB against 23.4 MB for a control that is
  never vacuumed; older snapshots, failed loads in the middle and at the end of the file, exact
  and one-block-short runs, a writer in flight, two crashes, a hot standby with and without
  feedback, four writers. All 12 one-line mutants are killed.
