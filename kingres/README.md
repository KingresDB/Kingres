<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="brand/svg/kingres-db-horizontal-color-dark.svg">
    <img src="brand/svg/kingres-db-horizontal-color-light.svg" alt="KingresDB" width="420">
  </picture>
</p>

# KingresDB

KingresDB is a thin fork of PostgreSQL that adds three main things. It also ships analytics extensions, export and import of tables and databases, and a standalone verifier (see the repository map below):

- **Sharding.** Tables are split into a fixed number of hash buckets spread over several servers. The logic lives in an extension (`kingres_shard`), not in the executor.
- **A tamper-evident ledger.** Ledger tables keep a hash chain and Merkle blocks, so changes to history can be detected, with digests that can be anchored outside the database (`kingres_ledger`).
- **An admin panel.** Every node serves a web panel and a REST API for creating or joining a cluster, managing databases, users, servers, rebalance and failover. The pattern follows the Couchbase cluster manager.

KingresDB is built for PostgreSQL 18 and for PostgreSQL 19 from one source: every extension builds and passes its tests on both majors ([ADR 0023](docs/adr/0023-one-extension-source-for-several-majors.md)).

KingresDB is derived from PostgreSQL. It is not PostgreSQL, and it is not endorsed by the PostgreSQL project. The name "KingresDB" deliberately avoids the PostgreSQL marks; see the trademark section of the architecture document.

The design is in [docs/architecture.md](docs/architecture.md). It separates sourced facts, **[Recommendation]** design decisions and **[Needs verification]** items. Documents derived from it keep that distinction.

## Status

There is one product branch per PostgreSQL major, with the same KingresDB code on each: `kingres-18` on upstream tag `REL_18_7`, and `kingres-19` on `REL_19_0` (PostgreSQL 19.0); see [docs/dev/releases.md](docs/dev/releases.md). No release has been made. The state is that of 2026-11-26. The first release is 0.1.0; its notes are in [CHANGELOG.md](CHANGELOG.md). The table is the state of the code by phase of the roadmap in [docs/architecture.md](docs/architecture.md). The REST contract is 0.34.0. "Done" means written, tested and reviewed where the work log says so.

| Phase | State | Detail |
|---|---|---|
| 0 Foundation, rebrand, AGPL | Done | [ADR 0024](docs/adr/0024-agpl-for-kingresdb-code.md) |
| 1 Sharding MVP, 2 Distributed DML and DDL, two-phase commit, reference tables | Done | [shard-mvp](docs/design/shard-mvp.md), [shard-phase2a](docs/design/shard-phase2a.md) |
| 2b Pushdown, row router, colocated joins | Done | [shard-phase2b](docs/design/shard-phase2b.md) |
| 3 Control plane, REST API, console, package | Done | [control-plane-2](docs/design/control-plane-2.md), [control-plane-3](docs/design/control-plane-3.md) |
| 4 Bucket movement and rebalance | Done | [shard-phase4](docs/design/shard-phase4.md) |
| 5 High availability | Done: standbys, failover, automatic failover, planned switchover, restore of preferred primaries, switchover and hard failover of the map group (`lostServerId`), local release of a held map group, and escrow of the CA key (off until switched on) | [ha-phase5](docs/design/ha-phase5.md), [ha-phase5-part2](docs/design/ha-phase5-part2.md) |
| 6 Ledger | Done (`kingres_ledger` 0.3, the verifier, the Ledger page) | [ledger-mvp](docs/design/ledger-mvp.md), [ledger-0.2](docs/design/ledger-0.2.md) |
| 7 Signing, anchoring, publication, aggregate digest | Done, except step 24 (an external signer command, publication mirrors, the anchor probe from another server), which is not in the first release | [ledger-phase7](docs/design/ledger-phase7.md) |
| 8 Global snapshot, multi-node ledger | Future roadmap, not part of the first release | [architecture](docs/architecture.md) |
| 9 Table lifecycle | Done, including distribute with related tables and the operation indicator of a table | [shard-phase9](docs/design/shard-phase9.md) |
| 10 Elasticity | Done: strategies, weights, automatic mode, drain (with the hand-over of the map group), bucket split, the standby lag rule | [shard-phase10](docs/design/shard-phase10.md) |
| 11 Distributed schemas | Done, including schema items in rebalance plans and the drain | [shard-phase11](docs/design/shard-phase11.md) |
| 12 Real-time analytics | Done: `kingres_sketch` 1.0, `kingres_columnar` 0.2, `kingres_timepart` 0.3; repartitioned joins, `MERGE`, columnar tables and approximate `count(DISTINCT)` in `kingres_shard` 0.9; time partitioning of distributed tables; control plane and console | [queries](docs/design/analytics-phase12-queries.md), [storage](docs/design/analytics-phase12-storage.md) |
| 13 Configuration from the console | Done | [config-phase13](docs/design/config-phase13.md) |
| 14a Table CSV export and import | Done | [data-export-import](docs/design/data-export-import.md) |
| 14b, 14c Database export and import | Done; the archive is checked as hostile input | [data-export-import](docs/design/data-export-import.md) |
| Security hardening of the extensions, `pg_stat_statements` and PL/pgSQL | Done: the extensions do not trust a schema that another role can write to; `pg_stat_statements` is preloaded and created in every database made by `kingresd`; `plpgsql` is in every such database | [operator guide](docs/guide/operator-guide.md#14-query-statistics-pg_stat_statements-and-plpgsql) |
| 15 Management of third-party extensions | Designed; it comes after the first release and is not part of it | - |

The detail of each phase, with every review and what it found, is in the [work log](docs/worklog.md) and in [docs/reviews/](docs/reviews/); the extension READMEs list the limits of each extension.

## Repository map

The repository root is the PostgreSQL source tree (the fork). KingresDB's own code is in two places: its extensions under `contrib/`, and the top-level `kingres/` directory.

| Path | Contents |
|---|---|
| `contrib/kingres_shard/` | Sharding extension (C): catalog, bucket mapping, planner and executor hooks, two-phase commit, background worker, bucket movement. |
| `contrib/kingres_ledger/` | Ledger extension (C): ledger tables, block builder, digest and verify functions. |
| `contrib/kingres_columnar/` | Columnar storage (C): a table access method that stores a table column-wise with compression for analytic scans. It is append-only, has B-tree indexes and a parallel scan, and can be in a publication. Its reviews are listed in [docs/reviews/](docs/reviews/) and its limits are in its README. |
| `contrib/kingres_sketch/` | Approximate aggregates (C): distinct count, top-n and percentiles, with partial aggregation that the shard groups compute. |
| `contrib/kingres_timepart/` | Time partitioning (SQL and PL/pgSQL): partitions created ahead of time and retention by policy, for ordinary tables and for user-partitioned distributed tables; old partitions can be converted to columnar storage and back. The control plane has policies and a scheduler for it. |
| `kingres/api/` | The OpenAPI contract (`openapi.yaml`). The REST API and the web client are generated from or checked against it. |
| `kingres/control-plane/` | Go code: `kingresd` (agent, manager, client router, REST API) and `kingres-watchdog`; `kingres-cli` is planned and not built. |
| `kingres/web/` | The admin panel: Vue 3 and TypeScript single-page application. |
| `kingres/verifier/` | Standalone command-line verifier for ledger digests and proofs. It does not trust the database. |
| `kingres/packaging/` | The single `kingres` package: `.deb`, Docker image, systemd unit, the release script. |
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
