<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="brand/svg/kingres-db-horizontal-color-dark.svg">
    <img src="brand/svg/kingres-db-horizontal-color-light.svg" alt="KingresDB" width="420">
  </picture>
</p>

# KingresDB

KingresDB is a thin fork of PostgreSQL that adds three things:

- **Sharding.** Tables are split into a fixed number of hash buckets spread over several servers. The logic lives in an extension (`kingres_shard`), not in the executor.
- **A tamper-evident ledger.** Ledger tables keep a hash chain and Merkle blocks, so changes to history can be detected, with digests that can be anchored outside the database (`kingres_ledger`).
- **An admin panel.** Every node serves a web panel and a REST API for creating or joining a cluster, managing databases, users, servers, rebalance and failover. The pattern follows the Couchbase cluster manager.

KingresDB is released for PostgreSQL 18 and for PostgreSQL 19 from one source: every extension builds and passes its tests on both majors ([ADR 0023](docs/adr/0023-one-extension-source-for-several-majors.md)). The integration suite of the control plane and the tests of the package, the image and the compose cluster pass on 19 as well (the work log has the runs).

KingresDB is derived from PostgreSQL. It is not PostgreSQL, and it is not endorsed by the PostgreSQL project. The name "KingresDB" deliberately avoids the PostgreSQL marks; see the trademark section of the architecture document.

The design is in [docs/architecture.md](docs/architecture.md). It separates sourced facts, **[Recommendation]** design decisions and **[Needs verification]** items. Documents derived from it keep that distinction.

## Status

There is one product branch per PostgreSQL major, with the same KingresDB code on each: `kingres-18` on upstream tag `REL_18_7`, and `kingres-19` on `REL_19_0` (PostgreSQL 19.0; the branch moved from `REL_19_RC2` on 2026-11-22); see [docs/dev/releases.md](docs/dev/releases.md). No release has been made. The table is the state of the staging branch of the first release on 2026-11-23, by phase of the roadmap in [docs/architecture.md](docs/architecture.md). The REST contract is 0.19.0. Items marked "in progress" are being written now and are not part of what is listed as done.

| Phase | State | Detail |
|---|---|---|
| 0 Foundation, rebrand, AGPL | Done | [ADR 0024](docs/adr/0024-agpl-for-kingresdb-code.md) |
| 1 Sharding MVP, 2 Distributed DML and DDL, two-phase commit, reference tables | Done; reviewed and fixed | [shard-mvp](docs/design/shard-mvp.md), [shard-phase2a](docs/design/shard-phase2a.md) |
| 2b Pushdown, row router, colocated joins | Done; every step reviewed and its findings fixed | [shard-phase2b](docs/design/shard-phase2b.md) |
| 3 Control plane, REST API, console, package | Done; the integration suite passes on PostgreSQL 18 and 19 | [control-plane-2](docs/design/control-plane-2.md), [control-plane-3](docs/design/control-plane-3.md) |
| 4 Bucket movement and rebalance | Done; reviewed three times, closed on 2026-11-20 | [shard-phase4](docs/design/shard-phase4.md) |
| 5 High availability | Done: standbys, failover, automatic failover, planned switchover, and round 2 (restore of preferred primaries, switchover of the map group); reviewed in rounds and fixed. hard failover of a live map group leader (contract 0.28.0) and escrow of the CA key (contract 0.26.0) are built, and the hardening loops are run by `run-failover-loops.ps1` | [ha-phase5](docs/design/ha-phase5.md), [ha-phase5-part2](docs/design/ha-phase5-part2.md) |
| 6 Ledger | Done (`kingres_ledger` 0.3, the verifier, the Ledger page) | [ledger-mvp](docs/design/ledger-mvp.md), [ledger-0.2](docs/design/ledger-0.2.md) |
| 7 Signing, anchoring, publication, aggregate digest | Done except step 24, which is dropped from the first release; fix rounds reviewed and merged | [ledger-phase7](docs/design/ledger-phase7.md) |
| 8 Global snapshot, multi-node ledger | Future roadmap, not part of the first release | [architecture](docs/architecture.md) |
| 9 Table lifecycle | Done (`kingres_shard` extension work, control plane, console pages of increment 9.7); reviewed. Open: distribute with related tables, the operation indicator on a table | [shard-phase9](docs/design/shard-phase9.md) |
| 10 Elasticity | Built (`kingres_shard` 0.8, control plane, console): strategies, weights, automatic mode, drain, bucket split. Drain step 4 (hand-over of the map group, contract 0.27.0) and the standby lag rule are built | [shard-phase10](docs/design/shard-phase10.md) |
| 11 Distributed schemas | Built (extension, schema tasks, console). Schema items in rebalance plans and in `check_cluster` are in `kingres_shard` 0.9; the control plane does not use them yet | [shard-phase11](docs/design/shard-phase11.md) |
| 12 Real-time analytics | Built: `kingres_sketch` 1.0, `kingres_columnar` 0.2, `kingres_timepart` 0.2; in `kingres_shard` 0.9 repartitioned joins, `MERGE`, columnar tables, approximate `count(DISTINCT)`; control plane (contract 0.18.0) and console. Time partitioning of distributed tables is built (contract 0.25.0) | [queries](docs/design/analytics-phase12-queries.md), [storage](docs/design/analytics-phase12-storage.md) |
| 13 Configuration from the console | Done; reviewed twice and fixed | [config-phase13](docs/design/config-phase13.md) |
| 14a Table CSV export and import | Done (control plane and console, contract 0.19.0); reviewed and fixed | [data-export-import](docs/design/data-export-import.md) |
| 14b, 14c Database export and import | Built (contract 0.29.0 and 0.30.0); the archive is checked as hostile input; after the first release | [data-export-import](docs/design/data-export-import.md) |

The detail of each phase, with every review and what it found, is in the [work log](docs/worklog.md) and in [docs/reviews/](docs/reviews/); the extension READMEs list the limits of each extension.

For what has been done, how it was verified and what is still open, read the work log: [docs/worklog.md](docs/worklog.md).

## Repository map

The repository root is the PostgreSQL source tree (the fork). KingresDB's own code is in two places: its extensions under `contrib/`, and the top-level `kingres/` directory.

| Path | Contents |
|---|---|
| `contrib/kingres_shard/` | Sharding extension (C): catalog, bucket mapping, planner and executor hooks, two-phase commit, background worker, bucket movement. |
| `contrib/kingres_ledger/` | Ledger extension (C): ledger tables, block builder, digest and verify functions. |
| `contrib/kingres_columnar/` | Columnar storage (C): a table access method that stores a table column-wise with compression for analytic scans (phase 12; insert-only). **Reviewed in twelve rounds and a closing check, whose result is at the end of this entry; the parallel scan was added after them and is not reviewed yet; no release has been made.** The history: the independent review of 0.1 found five cases of a wrong answer or wrong committed data ([report](docs/reviews/2026-11-15-kingres-columnar-0.1.md)); they are fixed, each with a test, and the on-disk format changed with the fixes. A second review ([report](docs/reviews/2026-11-15-kingres-columnar-0.1-fixes.md)) confirmed those fixes and found one more critical defect (under SERIALIZABLE a write that a concurrent reader cannot see is not detected as a conflict) and a memory leak with many subtransactions; they were fixed with the other findings of that review except two notes. A third review ([report](docs/reviews/2026-11-16-kingres-columnar-0.1-second-fixes.md)) found that the SERIALIZABLE fix did not hold for rows flushed inside a subtransaction (critical) and two high defects in the write buffers; they were fixed, each with a test. A fourth review ([report](docs/reviews/2026-11-16-kingres-columnar-0.1-third-fixes.md)) confirmed those fixes and their tests and found two more critical defects (when memory runs out while a subtransaction is committed, rows of the aborted subtransaction can be committed; a REPEATABLE READ or SERIALIZABLE transaction that compacts a table then reads rows committed after its snapshot); they were fixed, each with a test. A fifth review ([report](docs/reviews/2026-11-16-kingres-columnar-0.1-fourth-fixes.md)) confirmed those fixes and was the first without a critical finding: one high (a table converted from heap could show an older snapshot an empty table instead of an error), one medium and four low; they are fixed, each with a test. A sixth review ([report](docs/reviews/2026-11-17-kingres-columnar-0.1-fifth-fixes.md)) found no critical defect, one high (rows written by code of another extension inside the commit could be lost or doubled), two medium and three low; they are fixed, each with a test. A seventh review ([report](docs/reviews/2026-11-17-kingres-columnar-0.1-sixth-fixes.md)) found no wrong answer and no lost row, and one high defect (**a write that reaches the table while its transaction is ending, from a commit callback of another module, stops the server**), one medium and four low. They are fixed, each with a test: such a write is refused with a warning and the server goes on; at some of those moments a heap table would have kept the row, which the README of the extension states. Four more reviews followed ([eighth](docs/reviews/2026-11-17-kingres-columnar-0.1-seventh-fixes.md), [ninth](docs/reviews/2026-11-17-kingres-columnar-0.1-eighth-fixes.md), [tenth](docs/reviews/2026-11-18-kingres-columnar-0.1-ninth-fixes.md), [eleventh](docs/reviews/2026-11-18-kingres-columnar-0.1-tenth-fixes.md)); their findings up to the tenth are fixed. The last two found no wrong answer, no lost or doubled row and no unreadable table in ordinary use on either major, through crash recovery, a promoted standby, backup and restore. The eleventh found one high defect (if a superuser created a second access method over the handler of the extension, a routine `VACUUM` deleted every row of the tables that used it) and that the check function `columnar_check_clean()` was not fit for a monitor (false alarms while tables were dropped or rewritten; a failure with more columnar tables than the lock table holds); these are fixed, each with a test. The twelfth review ([report](docs/reviews/2026-11-18-kingres-columnar-0.1-eleventh-fixes.md)) confirmed the fixes, found no critical and no high defect and no sequence in which the clean-up deletes rows of a live table, and found four medium ones: the check says "clean" while its orphan step could not run, the memory of the check grows with the square of the number of locked tables, and two cases that need a superuser's deliberate act (an access method over a second SQL function escapes the refusal of foreign keys, triggers and publications; a module that copies the routine gets a table that drops its rows at commit). They are fixed, each with a test. The closing check ([report](docs/reviews/2026-11-18-kingres-columnar-0.1-closing-check.md)) confirmed the five fixes, killed nine of nine one-line mutants, found no path that loses a row, and closed the review track of this extension; the twelfth review's judgement stands that it is fit for data that matters within what it claims (insert-only analytic data, no indexes, serial scan). The low point the closing check left open (a broken access method of another module made the check write a log line per table) is fixed. After the reviews a parallel scan was added (stripes are handed to the workers of a Gather; not reviewed yet). Version 0.2 has B-tree indexes, unique ones and primary keys included, and index vacuum (a row has its final row number when it is inserted; index scans, no bitmap or index-only scans; not reviewed yet); compaction of an indexed table works without blocking readers or writers. Also not reviewed yet: logical decoding of inserts (a columnar table can be in a publication when `wal_level = logical`) and the reuse of the space that compaction and failed loads free in the file. A review of these increments ([report](docs/reviews/2026-11-20-kingres-columnar-c5-c8.md)) found no crash, no WAL defect and no difference from a heap table in its generated comparisons. It found two high defects: **a `REPEATABLE READ` transaction whose snapshot was older than a compaction found no rows through an index built or rebuilt after it**, and any role that may insert into an indexed table could use up its row numbers with rolled-back transactions, after which no insert succeeded until `VACUUM FULL`. It also found three medium defects: logical decoding of rows inserted before an `ADD COLUMN ... DEFAULT` in the same transaction, `amcheck` with `heapallindexed`, and the cost of one insert per savepoint. They are fixed, each with a test; the waste of row numbers is bounded, not removed (ending a table takes about 1.4 x 10^11 rolled-back inserts; the owner recovers with `VACUUM FULL`), and `VACUUM` now merges small stripes. A review of these fixes ([report](docs/reviews/2026-11-20-kingres-columnar-c13-fixes.md)) found one critical defect that the fixes had introduced (**the return of unused row numbers raced an index vacuum, which then deleted the index entries of committed rows**) and two medium ones in the merge; they are fixed, and a second look at them found that they hold and nothing that blocks (one low and two notes are open). |
| `contrib/kingres_sketch/` | Approximate aggregates (C): distinct count, top-n and percentiles, with partial aggregation (phase 12; `kingres_shard` has the shard groups compute them in part since phase 2b, step 8). The memory of aggregate states of a backend is bounded by a ceiling that only a superuser sets and a limit of the role's own; five reviews, the last ([report](docs/reviews/2026-11-16-phase12-extensions-fourth-fixes.md)) with one low finding, which is open. |
| `contrib/kingres_timepart/` | Time partitioning (SQL and PL/pgSQL): partitions created ahead of time and retention by policy, for ordinary tables (phase 12; the control plane has policies and a scheduler for it; distributed tables are in progress). A superuser's session does not run the maintenance for a table of another role unless a setting says that table owners are trusted; an owner drives its own tables. A policy acts only when the interface sealed it, so a row restored from a dump does nothing until a superuser adopts it. Five reviews (the same reports as `kingres_sketch`); the last one did not cover the seal and the owner-driven path in depth. Version 0.2 adds the conversion of old partitions to columnar storage and back, by hand or by a policy (also for partitions whose table has B-tree indexes). Its review ([report](docs/reviews/2026-11-20-kingres-timepart-0.2.md)) found no lost, doubled or changed row under concurrent writers, cancels and crashes, and found one high defect: **a column or unique index added to the partition while the conversion waited for its lock was lost** (the new column was empty and the index gone after the conversion). It also found four medium defects (a reader with an older snapshot sees the converted partition empty; a conversion back to heap published the whole copy as inserts when a publication covered the schema; trigger states and storage parameters were not kept), seven low ones and two notes. They are fixed, each with a test, and the fixes are not reviewed yet: the conversion reads the catalog again after it holds the lock. One stays and is stated: a `REPEATABLE READ` or `SERIALIZABLE` transaction whose snapshot is older than the swap and that has not read the table yet sees the partition empty, as with `TRUNCATE`. |
| `kingres/api/` | The OpenAPI contract (`openapi.yaml`). The REST API and the web client are generated from or checked against it. |
| `kingres/control-plane/` | Go code: `kingresd` (agent, manager, client router, REST API) and `kingres-watchdog`; `kingres-cli` is planned and not built. |
| `kingres/web/` | The admin panel: Vue 3 and TypeScript single-page application. |
| `kingres/verifier/` | Standalone command-line verifier for ledger digests and proofs. It does not trust the database. |
| `kingres/packaging/` | The single `kingres` package: `.deb`, `.rpm`, Docker image, systemd unit. |
| `kingres/dev/` | Developer scripts. |
| `kingres/docs/` | All KingresDB documentation. |
| everything else | Upstream PostgreSQL, changed only through the documented core patches. |

The full tree and the rule for where new code goes are in [docs/dev/repository-layout.md](docs/dev/repository-layout.md).

## Documents

Start here:

- [docs/architecture.md](docs/architecture.md) - design, evidence and open decisions.
- [docs/dev/getting-started.md](docs/dev/getting-started.md) - set up a development machine and build.
- [docs/worklog.md](docs/worklog.md) - dated record of work done.
- [docs/guide/README.md](docs/guide/README.md) - the user guide: quickstart, installation, operator guide, upgrade, limitations.
- [CHANGELOG.md](CHANGELOG.md) - what each release changes.

Developer standards:

- [docs/dev/coding-standards.md](docs/dev/coding-standards.md) - standards per language, with the enforcing tool.
- [docs/dev/repository-layout.md](docs/dev/repository-layout.md) - directory tree and placement rules.
- [docs/dev/core-patches.md](docs/dev/core-patches.md) - register of changes to upstream files.
- [docs/dev/upstream-sync.md](docs/dev/upstream-sync.md) - branches, rebasing, new major versions.
- [docs/dev/releases.md](docs/dev/releases.md) - one release per PostgreSQL major: the product branches, how a change reaches each, what a release consists of.
- [docs/dev/testing.md](docs/dev/testing.md) - test strategy and the definition of done.
- [docs/dev/documentation.md](docs/dev/documentation.md) - what is documented where.

Decisions:

- [docs/adr/README.md](docs/adr/README.md) - index of architecture decision records and how to add one.

## License

KingresDB's own code (everything under `kingres/` and `contrib/kingres_*`) is released under the GNU Affero General Public License, version 3 (`AGPL-3.0-only`); the text is in [LICENSE](LICENSE) and at the top of the repository. PostgreSQL's code stays under the PostgreSQL License: the upstream `COPYRIGHT` file is kept unchanged. [COPYRIGHT](COPYRIGHT) says which licence covers which part, and [ADR 0024](docs/adr/0024-agpl-for-kingresdb-code.md) records the decision. See [docs/dev/coding-standards.md](docs/dev/coding-standards.md) for the file header convention.
