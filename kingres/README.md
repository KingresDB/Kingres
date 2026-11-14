<!-- SPDX-License-Identifier: PostgreSQL -->
<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="brand/svg/kingres-db-horizontal-color-dark.svg">
    <img src="brand/svg/kingres-db-horizontal-color-light.svg" alt="KingresDB" width="420">
  </picture>
</p>

# KingresDB

KingresDB is a thin fork of PostgreSQL 18 that adds three things:

- **Sharding.** Tables are split into a fixed number of hash buckets spread over several servers. The logic lives in an extension (`kingres_shard`), not in the executor.
- **A tamper-evident ledger.** Ledger tables keep a hash chain and Merkle blocks, so changes to history can be detected, with digests that can be anchored outside the database (`kingres_ledger`).
- **An admin panel.** Every node serves a web panel and a REST API for creating or joining a cluster, managing databases, users, servers, rebalance and failover. The pattern follows the Couchbase cluster manager.

KingresDB is derived from PostgreSQL. It is not PostgreSQL, and it is not endorsed by the PostgreSQL project. The name "KingresDB" deliberately avoids the PostgreSQL marks; see the trademark section of the architecture document.

The design is in [docs/architecture.md](docs/architecture.md). It separates sourced facts, **[Recommendation]** design decisions and **[Needs verification]** items. Documents derived from it keep that distinction.

## Status

Implementation has started. The base is upstream tag `REL_18_7`, checked out as the local branch `kingres-18`. The extensions and the control plane are not written yet. For what has been done, how it was verified and what is still open, read the work log: [docs/worklog.md](docs/worklog.md).

## Repository map

The repository root is the PostgreSQL source tree (the fork). KingresDB's own code is in two places: two extensions under `contrib/`, and the top-level `kingres/` directory.

| Path | Contents |
|---|---|
| `contrib/kingres_shard/` | Sharding extension (C): catalog, bucket mapping, planner and executor hooks, 2PC, background worker. Not written yet. |
| `contrib/kingres_ledger/` | Ledger extension (C): ledger tables, block builder, digest and verify functions. Not written yet. |
| `kingres/api/` | The OpenAPI contract (`openapi.yaml`). The REST API and the web client are generated from or checked against it. |
| `kingres/control-plane/` | Go code: `kingresd` (agent, manager, client router, REST API), `kingres-cli`. |
| `kingres/web/` | The admin panel: Vue 3 and TypeScript single-page application. |
| `kingres/verifier/` | Standalone command-line verifier for ledger digests and proofs. It does not trust the database. |
| `kingres/packaging/` | The single `kingresdb` package: `.deb`, `.rpm`, Docker image, systemd unit. |
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
- [docs/dev/testing.md](docs/dev/testing.md) - test strategy and the definition of done.
- [docs/dev/documentation.md](docs/dev/documentation.md) - what is documented where.

Decisions:

- [docs/adr/README.md](docs/adr/README.md) - index of architecture decision records and how to add one.

## License

KingresDB's own code is released under the PostgreSQL License, the same license as upstream. The upstream `COPYRIGHT` file is kept unchanged. See [docs/dev/coding-standards.md](docs/dev/coding-standards.md) for the file header convention.
