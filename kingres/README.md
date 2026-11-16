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

There is one product branch per PostgreSQL major, with the same KingresDB code on each: `kingres-18` on upstream tag `REL_18_7`, and `kingres-19` on `REL_19_RC2` (a pre-release until PostgreSQL 19 is released); see [docs/dev/releases.md](docs/dev/releases.md). State on 2026-11-16, by phase of the roadmap in [docs/architecture.md](docs/architecture.md):

| Phase | State |
|---|---|
| 0 Foundation, 1 Sharding MVP, 2 Distributed DML and DDL, two-phase commit, reference tables | Done (`kingres_shard` 0.2) |
| 3 Control plane, REST API, panel, package | Done; the control plane passes its integration suite on PostgreSQL 18 and on 19 |
| 4 Bucket movement and rebalancing | Built; its review ([report](docs/reviews/2026-11-16-kingresd-rebalance.md)) had two high findings about fencing; they and the other findings are fixed (a holder that lost the lease of a task writes nothing, runs no hook and raises no alert; the call that starts a move is made under a lock on the task) and the fixes have not been reviewed again: `kingres_shard` moves buckets, and the control plane drives a rebalance (a plan by bucket count, one move at a time, stop and start again, taken over after a restart or a leader change, a report per run) with its page in the panel. Several moves at once, the strategy by disk size and draining a server are phase 10 |
| 5 High availability | In progress: heartbeats, leases, standbys and the client entry point are in the control plane; `kingres_shard` 0.5 has standby nodes, `promote_node`, prepared transactions across a failover, distributed deadlock detection and coordinated restore points; failover itself (the control plane promoting a standby) is not written yet. The independent review of 0.5 found one critical and six high defects in these functions ([report](docs/reviews/2026-11-15-kingres-shard-0.5.md)); they were fixed, each with a test. The review of those fixes ([report](docs/reviews/2026-11-16-kingres-shard-0.5-fixes.md)) found one high defect (a participant whose wait for its own synchronous standby is cancelled still answers that it prepared and committed, so after a failover of that participant its part of a committed transaction can be gone) and three medium ones in `promote_node` and the majority rule; these are being fixed, and a failover must not be relied on before that and before the control plane drives it |
| 6 Ledger | Done (`kingres_ledger` 0.2, the verifier, the Ledger page) |
| 2b, 9 to 12 | Designed, not implemented, except the three standalone extensions of phase 12 listed below |
| 7 Signing, anchoring, publication, aggregate digest | Designed ([docs/design/ledger-phase7.md](docs/design/ledger-phase7.md)); of 25 steps the first two are built: the verifier checks signed checkpoints, tile logs and consistency proofs, and `kingres_ledger` 0.3 has the functions from which the chain is published |
| 8 | Future roadmap: not part of the first release |

For what has been done, how it was verified and what is still open, read the work log: [docs/worklog.md](docs/worklog.md).

## Repository map

The repository root is the PostgreSQL source tree (the fork). KingresDB's own code is in two places: its extensions under `contrib/`, and the top-level `kingres/` directory.

| Path | Contents |
|---|---|
| `contrib/kingres_shard/` | Sharding extension (C): catalog, bucket mapping, planner and executor hooks, two-phase commit, background worker, bucket movement. |
| `contrib/kingres_ledger/` | Ledger extension (C): ledger tables, block builder, digest and verify functions. |
| `contrib/kingres_columnar/` | Columnar storage (C): a table access method that stores a table column-wise with compression for analytic scans (phase 12; first version: insert-only, no indexes, serial scan). **Not for data that matters yet:** the independent review of 0.1 found five cases of a wrong answer or wrong committed data ([report](docs/reviews/2026-11-15-kingres-columnar-0.1.md)); they are fixed, each with a test, and the on-disk format changed with the fixes. A second review ([report](docs/reviews/2026-11-15-kingres-columnar-0.1-fixes.md)) confirmed those fixes and found one more critical defect (under SERIALIZABLE a write that a concurrent reader cannot see is not detected as a conflict) and a memory leak with many subtransactions; they were fixed with the other findings of that review except two notes. A third review ([report](docs/reviews/2026-11-16-kingres-columnar-0.1-second-fixes.md)) found that the SERIALIZABLE fix did not hold for rows flushed inside a subtransaction (critical) and two high defects in the write buffers; they are fixed, each with a test, and these fixes have not been reviewed again. |
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
