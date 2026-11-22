<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
# Changelog

All notable changes of KingresDB are listed here, by release. A release is made for PostgreSQL 18 and PostgreSQL 19 at the same time from one source, and has one version (`kingres_version` in the top-level `meson.build`); see [docs/dev/releases.md](docs/dev/releases.md). The dated, detailed record of the work is [docs/worklog.md](docs/worklog.md), and the independent reviews are in [docs/reviews/](docs/reviews/).

The release script ([packaging/release/release.sh](packaging/release/release.sh)) takes the notes of a release from the section of this file whose heading is `## [<version>]`. The section of the first release is a draft: it is written from what is on `kingres-18` and from the state of the unmerged branches, and the lines marked **Pending** must be checked and moved or removed when those branches are merged.

## [Unreleased]

Nothing yet.

## [0.1.0] - not released

The first release. KingresDB is a thin fork of PostgreSQL 18 and 19 with sharding, a tamper-evident ledger, a control plane and a web console, and extensions for analytics.

### Added

- **Fork and packaging.** A thin fork with a small register of core patches ([docs/dev/core-patches.md](docs/dev/core-patches.md)); one source of every extension for PostgreSQL 18 and 19 ([ADR 0023](docs/adr/0023-one-extension-source-for-several-majors.md)); KingresDB code under the AGPL-3.0-only, PostgreSQL code keeps its licence ([ADR 0024](docs/adr/0024-agpl-for-kingresdb-code.md)). A reproducible release build, a Debian package per major (`kingres_<version>+pg18-<revision>_amd64.deb`, `+pg19`), a container image per major (`kingres:<version>-pg18`, `-pg19`) and a three-node development cluster in Docker Compose ([kingres/packaging](packaging/README.md)).
- **Sharding** (`kingres_shard` 0.7). Tables split into hash buckets over several servers; writes and reads on any server; two-phase commit with recovery; reference tables; distributed DDL; bucket moves with logical replication; standbys, `promote_node`, prepared transactions across a failover, distributed deadlock detection and coordinated restore points. Pushdown (phase 2b): closed queries, partial aggregation, sorted streams with limit and joins of colocated tables run in the shard groups, and a row router for multi-row `INSERT`, `INSERT ... SELECT` and `COPY FROM`; the complete scan stays as the fallback for every query ([ADR 0022](docs/adr/0022-the-complete-scan-stays.md)). Table lifecycle (phase 9): distribute a table that already holds rows, in blocking and online form; turn a table back into an ordinary table; change the distribution column, the bucket count or the colocation group; distribute user-partitioned tables; carry dependent views and storage parameters.
- **Control plane** (`kingresd`, REST contract 0.14.0). Cluster creation and joining, servers, roles and grants, network access, databases, tasks, alerts, logs and metrics; the manager leader on the primary of the map group, the cluster state in a system database, certificates with renewal and revocation; rebalance as a task with stop, resume and reports; failover inside `kingresd` fenced by leases (manual failover, failover of the map group, automatic failover with a counter, planned switchover of an ordinary group); configuration of PostgreSQL parameters and time from the console (phase 13); table lifecycle tasks (phase 9, increment 9.6).
- **Console.** A Vue 3 web console on every node: dashboard, servers, shards, rebalance, replication and failover, databases, security, configuration, ledger, workbench, alerts, tasks, logs and this server.
- **Ledger** (`kingres_ledger` 0.3). Append-only and updatable ledger tables with blocks, digests, inclusion proofs and verification; client signatures with a signature policy per table; a write guard; signing keys, a transparency log per chain and an aggregate log per database published on a listener of its own; anchoring to a WORM object store, an RFC 3161 authority and OpenTimestamps; bundles and the audit of the publication; the acknowledgement of a diverged chain; the acceptance of a restored system database as a signed event ([ADR 0025](docs/adr/0025-restore-of-the-system-database-is-accepted-by-an-operator.md)).
- **Verifier** (`kingres-verify`). A standalone, standard-library Go command that checks digests, proofs, signed checkpoints, tile logs, consistency proofs, aggregates, bundles, client signatures and anchors without trusting the database; a reproducible build and a release script that produces signed checksums.
- **Analytics extensions.** `kingres_sketch` 1.0 (distinct count, top-n and percentiles, with partial aggregation that the shard groups compute); `kingres_columnar` 0.2 (an append-only columnar table access method with B-tree indexes, parallel scan, logical decoding of inserts and reuse of free space); `kingres_timepart` 0.2 (time partitions created ahead and dropped by policy, and conversion of old partitions to columnar storage and back).
- **Documentation.** A user guide ([docs/guide](docs/guide/README.md)): quickstart, installation, operator guide, upgrade and limitations.

### Pending (to be checked and moved into the list above, or removed, when the branches are merged)

- **Phases 10 and 11 in `kingres_shard` 0.8** (branch `m18`): splitting a bucket, weights, `plan_rebalance` by disk size, distributed schemas; and their control plane and console on contract 0.16.0 (branch `cp-1011`): strategies, threshold, drain, automatic rebalance, split and schema tasks.
- **Failover, second round** (branch `fo-r2wip`, contract 0.15.0): restoring preferred primaries, a switchover plan and phases, a planned switchover of the map group, and the fixes of the review of that round.
- **Phase 12 in `kingres_shard`** (branch `shard-p12`): repartitioned joins and `MERGE` on distributed tables.
- **Phase 9 console** (branch `web-15`): table pages and dialogs for the table lifecycle.
- **Phase 13 closing** (branch `p13-fin`, contract 0.17.0): two more preview warnings and four integration tests.
- **Phase 7 fix rounds** (branches `p7r-fix`, `p7f-fix`): fixes of the reviews of the ledger work.
- **Release items:** `kingres-19` on the final PostgreSQL 19 release; the preload of `kingres_columnar` by `kingresd`; a reserved WAL resource manager id for `kingres_columnar`; the packages and images of the final contents run through the package tests on a machine with Docker.

### Known limitations

No global snapshot across shard groups (phase 8, a future roadmap item); no control plane or console part for the phase 12 analytics yet; the library of `kingres_columnar` is not preloaded, so columnar writes and the conversion of partitions are refused; no move of a cluster between PostgreSQL majors; no signed packages, no apt repository. The full list is in [docs/guide/limitations.md](docs/guide/limitations.md).
