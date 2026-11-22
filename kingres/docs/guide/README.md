<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
# KingresDB user guide

This guide is for the people who install and run KingresDB. It says what to do and what to expect; where it needs the full reasoning it links to the design documents, the READMEs of the components and the work log. Nothing in it is a promise beyond what those documents and the tests support.

| Page | What it covers |
|---|---|
| [quickstart.md](quickstart.md) | One node, then three nodes, up in a few commands |
| [install.md](install.md) | Install from the Debian package or from source, for PostgreSQL 18 and 19 |
| [operator-guide.md](operator-guide.md) | The control plane and the console, servers, rebalance, failover and switchover, backups and the system database, ledger anchoring and the verifier |
| [upgrade.md](upgrade.md) | Updating the extensions (`ALTER EXTENSION ... UPDATE`), `kingresd` and the package |
| [limitations.md](limitations.md) | What KingresDB does not do, in the first release |

Also useful: [../../CHANGELOG.md](../../CHANGELOG.md) (what changed), [../../README.md](../../README.md) (the state of every phase), [../architecture.md](../architecture.md) (the design), [../dev/releases.md](../dev/releases.md) (how a release is made) and [../../packaging/README.md](../../packaging/README.md) (the packages and images in full detail).

## How to read the markers

The first release is made from the two product branches (`kingres-18`, `kingres-19`). Some finished work was still on other branches when this guide was written. A section that depends on such work carries the line **To be checked when `<feature>` is merged**: the text below it describes what the design and the branch say, not what was run on the product branches, and it must be checked against the merged code before the release. Do not rely on such a section before then.

## KingresDB in one page

KingresDB is a thin fork of PostgreSQL. It adds three things:

- **Sharding** (`kingres_shard`): a table is split into hash buckets that live on several servers. Every server accepts reads and writes.
- **A tamper-evident ledger** (`kingres_ledger`): ledger tables keep a hash chain, with digests that can be anchored outside the database and checked by a standalone verifier (`kingres-verify`).
- **A control plane and a console** (`kingresd`): every node serves a web console and a REST API to create or join a cluster, and to manage databases, roles, servers, rebalance, failover and the ledger.

It also ships three extensions for analytics: `kingres_sketch` (approximate aggregates), `kingres_columnar` (columnar storage) and `kingres_timepart` (time partitioning).

KingresDB is released for PostgreSQL 18 and for PostgreSQL 19, from one source. A cluster runs one major: all its servers are installed from the same branch.
