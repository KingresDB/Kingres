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

KingresDB is released for PostgreSQL 18 and for PostgreSQL 19 from one source: every extension builds and passes its tests on both majors ([ADR 0023](docs/adr/0023-one-extension-source-for-several-majors.md)). The control plane and the packaging have not been checked on 19 yet.

KingresDB is derived from PostgreSQL. It is not PostgreSQL, and it is not endorsed by the PostgreSQL project. The name "KingresDB" deliberately avoids the PostgreSQL marks; see the trademark section of the architecture document.

The design is in [docs/architecture.md](docs/architecture.md). It separates sourced facts, **[Recommendation]** design decisions and **[Needs verification]** items. Documents derived from it keep that distinction.

## Status

There is one product branch per PostgreSQL major, with the same KingresDB code on each: `kingres-18` on upstream tag `REL_18_7`, and `kingres-19` on `REL_19_RC2` (a pre-release until PostgreSQL 19 is released); see [docs/dev/releases.md](docs/dev/releases.md). State on 2026-11-15, by phase of the roadmap in [docs/architecture.md](docs/architecture.md):

| Phase | State |
|---|---|
| 0 Foundation, 1 Sharding MVP, 2 Distributed DML and DDL, two-phase commit, reference tables | Done (`kingres_shard` 0.2) |
| 3 Control plane, REST API, panel, package | Done |
| 4 Bucket movement and rebalancing | In progress: `kingres_shard` 0.4 moves buckets (0.4 is the fix round for the review of 0.3; its fixes are being reviewed again); rebalancing is not yet driven by the control plane |
| 5 High availability | In progress: heartbeats, leases, standbys and the client entry point are in the control plane; `kingres_shard` 0.5 has standby nodes, `promote_node`, prepared transactions across a failover, distributed deadlock detection and coordinated restore points; failover itself (the control plane promoting a standby) is not written yet. The independent review of 0.5 found one critical and six high defects in these functions ([report](docs/reviews/2026-11-15-kingres-shard-0.5.md)); they are being fixed, and a failover must not be relied on before that |
| 6 Ledger | Done (`kingres_ledger` 0.2, the verifier, the Ledger page) |
| 2b, 9 to 12 | Designed, not implemented, except the three standalone extensions of phase 12 listed below |
| 7 | Not started |
| 8 | Future roadmap: not part of the first release |

For what has been done, how it was verified and what is still open, read the work log: [docs/worklog.md](docs/worklog.md).

## Repository map

The repository root is the PostgreSQL source tree (the fork). KingresDB's own code is in two places: its extensions under `contrib/`, and the top-level `kingres/` directory.

| Path | Contents |
|---|---|
| `contrib/kingres_shard/` | Sharding extension (C): catalog, bucket mapping, planner and executor hooks, two-phase commit, background worker, bucket movement. |
| `contrib/kingres_ledger/` | Ledger extension (C): ledger tables, block builder, digest and verify functions. |
| `contrib/kingres_columnar/` | Columnar storage (C): a table access method that stores a table column-wise with compression for analytic scans (phase 12; first version: insert-only, no indexes, serial scan). **Not for data that matters yet:** the independent review of 0.1 found five cases of a wrong answer or wrong committed data ([report](docs/reviews/2026-11-15-kingres-columnar-0.1.md)); the fixes are in progress. |
| `contrib/kingres_sketch/` | Approximate aggregates (C): distinct count, top-n and percentiles, with partial aggregation (phase 12; works on one node, pushdown across shard groups comes with phase 2b). |
| `contrib/kingres_timepart/` | Time partitioning (SQL and PL/pgSQL): partitions created ahead of time and retention by policy, for ordinary tables (phase 12; distributed tables and the scheduler in the control plane come later). |
| `kingres/api/` | The OpenAPI contract (`openapi.yaml`). The REST API and the web client are generated from or checked against it. |
| `kingres/control-plane/` | Go code: `kingresd` (agent, manager, client router, REST API), `kingres-cli`. |
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
