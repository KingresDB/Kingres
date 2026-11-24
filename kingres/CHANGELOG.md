<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
# Changelog

All notable changes of KingresDB are listed here, by release. A release is made for PostgreSQL 18 and PostgreSQL 19 at the same time from one source, and has one version (`kingres_version` in the top-level `meson.build`); see [docs/dev/releases.md](docs/dev/releases.md). The dated, detailed record of the work is [docs/worklog.md](docs/worklog.md), and the independent reviews are in [docs/reviews/](docs/reviews/).

The release script ([packaging/release/release.sh](packaging/release/release.sh)) takes the notes of a release from the section of this file whose heading is `## [<version>]`, and refuses while that section has a `### Pending` part. The section of the first release is written from the staging branch of the first release on 2026-11-23; its Pending part lists what is still open and is removed, with the items it lists done or moved out, at the real release.

## [Unreleased]

Nothing yet.

## [0.1.0] - not released

The first release. KingresDB is a thin fork of PostgreSQL 18.7 and PostgreSQL 19.0 with sharding, a tamper-evident ledger, a control plane and a web console, and extensions for analytics. The REST contract is 0.19.0.

### Added

- **Bases.** PostgreSQL 18.7 (`kingres-18`, upstream tag `REL_18_7`) and PostgreSQL 19.0 (`kingres-19`, upstream tag `REL_19_0`).
- **Fork and packaging.** A thin fork with a small register of core patches ([docs/dev/core-patches.md](docs/dev/core-patches.md)); one source of every extension for PostgreSQL 18 and 19 ([ADR 0023](docs/adr/0023-one-extension-source-for-several-majors.md)); KingresDB code under the AGPL-3.0-only, PostgreSQL code keeps its licence ([ADR 0024](docs/adr/0024-agpl-for-kingresdb-code.md)). A reproducible release build, a Debian package per major (`kingres_<version>+pg18-<revision>_amd64.deb`, `+pg19`), a container image per major (`kingres:<version>-pg18`, `-pg19`), a three-node development cluster in Docker Compose ([kingres/packaging](packaging/README.md)), and a release script with its test.
- **Sharding** (`kingres_shard` 0.9). Tables split into hash buckets over several servers; writes and reads on any server; two-phase commit with recovery; reference tables; distributed DDL; bucket moves with logical replication; standbys, `promote_node`, prepared transactions across a failover, distributed deadlock detection and coordinated restore points.
  - Pushdown (phase 2b): closed queries, partial aggregation, sorted streams with limit and joins of colocated tables run in the shard groups, and a row router for multi-row `INSERT`, `INSERT ... SELECT` and `COPY FROM`; the complete scan stays as the fallback for every query ([ADR 0022](docs/adr/0022-the-complete-scan-stays.md)).
  - Table lifecycle (phase 9): distribute a table that already holds rows, in blocking and online form; turn a table back into an ordinary table; change the distribution column, the bucket count or the colocation group; distribute user-partitioned tables; carry dependent views and storage parameters.
  - Elasticity (phase 10, 0.8): splitting a bucket, weights per server, plans by disk size.
  - Distributed schemas (phase 11, 0.8 and 0.9): a schema placed and moved as a unit, schemas in the rebalance plan and the drain, and the schema checks of `check_cluster`.
  - Analytics (phase 12, 0.9): repartitioned joins on columns other than the distribution column, `MERGE` on distributed tables, columnar tables that can be distributed and moved (blocking variants), approximate `count(DISTINCT)` through `kingres_sketch`, and the explicit refusal of ledger tables by `create_distributed_table` and `create_reference_table` (SQLSTATE `KS016`).
- **Control plane** (`kingresd`, REST contract 0.19.0). Cluster creation and joining, servers, roles and grants, network access, databases, tasks, alerts, logs and metrics; the manager leader on the primary of the map group, the cluster state in a system database, certificates with renewal and revocation.
  - Rebalance as a task with stop, resume and reports; strategies by bucket count or disk size, a threshold, weights, several moves at a time, an automatic mode (off by default), draining a server, splitting a bucket (phase 10).
  - Distributed schemas: distribute, move and make local as tasks, and the sharding mode of a database (phase 11).
  - Failover inside `kingresd` fenced by leases: manual failover, failover of the map group, automatic failover with a counter, planned switchover of an ordinary group and of the map group, restore of preferred primaries, and the call that gives up a stuck map group bootstrap (phase 5, rounds 1 and 2).
  - Table lifecycle tasks (phase 9, increment 9.6).
  - Analytics (phase 12, contract 0.18.0): approximate aggregates per database, conversion of a table or partition between row and columnar storage with a preview, time-partitioning policies with a plan, runs and a scheduler, and the preload of `kingres_columnar`. Conversion and policies are open for tables of superusers unless the cluster setting `analytics.allowNonSuperuserOwners` is on.
  - Configuration of PostgreSQL parameters and time from the console, with preview warnings (phase 13).
  - Export and import of a table as CSV (phase 14a, contract 0.19.0): resumable uploads and downloads of sealed staged files, all-or-nothing import with a commit marker, off until the cluster setting `dataTransfer.enabled` is on. The search path of the service connections is fixed, and the marker tables have row security and a checked tag.
- **Console.** A Vue 3 web console on every node: dashboard, servers, shards, rebalance, replication and failover, databases, tables (the table page with the lifecycle dialogs of increment 9.7), security, configuration, settings, ledger, workbench, alerts, tasks, logs and this server. The analytics console (approximate aggregates, the Storage column, partitions, policies and their runs, the plan summary in the workbench) and the transfers panel with the export and import dialogs.
- **Ledger** (`kingres_ledger` 0.3). Append-only and updatable ledger tables with blocks, digests, inclusion proofs and verification; client signatures with a signature policy per table; a write guard; signing keys, a transparency log per chain and an aggregate log per database published on a listener of its own; anchoring to a WORM object store, an RFC 3161 authority and OpenTimestamps; bundles and the audit of the publication; the acknowledgement of a diverged chain; the acceptance of a restored system database as a signed event ([ADR 0025](docs/adr/0025-restore-of-the-system-database-is-accepted-by-an-operator.md)); a rotation generation of the credentials of anchor targets.
- **Verifier** (`kingres-verify`). A standalone, standard-library Go command that checks digests, proofs, signed checkpoints, tile logs, consistency proofs, aggregates, bundles, client signatures and anchors without trusting the database; a reproducible build and a release script that produces signed checksums.
- **Analytics extensions.** `kingres_sketch` 1.0 (distinct count, top-n and percentiles, with partial aggregation that the shard groups compute); `kingres_columnar` 0.2 (an append-only columnar table access method with B-tree indexes, parallel scan, logical decoding of inserts and reuse of free space); `kingres_timepart` 0.2 (time partitions created ahead and dropped by policy, and conversion of old partitions to columnar storage and back).
- **Documentation.** A user guide ([docs/guide](docs/guide/README.md)): quickstart, installation, operator guide, upgrade and limitations.

### Pending (open items; this part is removed at the real release, with each item done or moved out)

- **Phase 5, in progress:** a hard failover of a server whose map group leader is alive (refused with 422 today; the planned switchover covers the map group); the hardening loops of the whole failover part (measured loops and a review of everything together); escrow of the certificate authority key (today the operator restores `pki/ca.key` and `pki/revoked.json` from a backup).
- **Phase 10, open:** step 4 of the drain, the hand-over of the map group (a drain of a server that hosts it is refused with `server_hosts_map_group`); the standby lag rule of a rebalance (`maxStandbyLagBytes` is a setting and the rule is not evaluated).
- **Phase 11, open:** the control plane does not use the schema items of the rebalance plan and `schemas_on_group()`; a drain moves buckets only.
- **Phase 12, in progress:** time partitioning and conversion of distributed tables.
- **Phases 14b and 14c, in progress, after the first release:** export and import of a whole database.
- **Phase 7, step 24, dropped from the first release:** an external signer command, publication mirrors and the anchor probe from another server.
- **Phase 8, future roadmap, not part of the first release:** a global snapshot and a multi-node ledger.
- **Phase 9, open:** distribute with related tables, and the operation indicator of a table (needs `Table.operation` in the contract).
- **Release items:** a reserved WAL resource manager id for `kingres_columnar` (it uses the experimental id 128); the packages and images of the final contents run through the package tests on a machine with Docker; the final checks recorded for both majors on the release commits.

### Known limitations

No global snapshot across shard groups (phase 8, a future roadmap item); no move of a cluster between PostgreSQL majors; no signed packages, no apt repository. The full list is in [docs/guide/limitations.md](docs/guide/limitations.md).
