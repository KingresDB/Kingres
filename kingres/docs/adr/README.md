<!-- SPDX-License-Identifier: AGPL-3.0-only -->
# Architecture decision records

An ADR records one decision: the context, the decision, its consequences and the alternatives that were rejected. The design itself is in [../architecture.md](../architecture.md); ADRs record that a recommendation in it has become a decision, and why. The rules for what goes where are in [../dev/documentation.md](../dev/documentation.md).

## Index

| ADR | Title | Status | Date | Open decision # |
|---|---|---|---|---|
| [0001](0001-thin-fork-with-extensions.md) | Thin fork with minimal core patches; logic in extensions | Accepted | 2026-11-14 | - |
| [0002](0002-base-on-postgresql-18.md) | Base on PostgreSQL 18 (`REL_18_7`), rebase to 19 later | Accepted | 2026-11-14 | 1 |
| [0003](0003-single-repository-layout.md) | Single repository layout | Accepted | 2026-11-14 | 2 (partly) |
| [0004](0004-postgresql-license-no-citus-code.md) | PostgreSQL License; no Citus code | Superseded by 0024 in its choice of licence | 2026-11-14 | 3 |
| [0005](0005-wsl2-build-environment.md) | WSL2 as the build environment | Accepted | 2026-11-14 | 4 |
| [0006](0006-sharding-mvp-first.md) | Sharding MVP first | Accepted | 2026-11-14 | 5 |
| [0007](0007-fixed-hash-buckets.md) | Fixed hash buckets (default 1024; the implemented default is 128, see the note in the ADR) | Accepted | 2026-11-14 | - |
| [0008](0008-map-group-source-of-truth.md) | Map group in PostgreSQL is the source of truth; etcd only for leader election | Accepted; the Patroni and etcd parts are superseded by 0021 and 0018 | 2026-11-14 | - |
| [0009](0009-two-phase-commit-no-global-snapshot.md) | 2PC without a global snapshot at first | Accepted | 2026-11-14 | - |
| [0010](0010-writes-on-any-node.md) | Writes on any node: a primary per server and a router in the agent; no conflict-resolving multi-master | Accepted | 2026-11-14 | - |
| [0011](0011-replica-count-per-cluster.md) | Replica count per cluster, durability per database | Accepted | 2026-11-14 | 6 |
| [0012](0012-patroni-bundled-failover.md) | Patroni bundled as the failover engine | Superseded by 0021 | 2026-11-14 | 7 |
| [0013](0013-ledger-tables-and-anchoring-first.md) | Ledger tables and external anchoring first; consensus only with a consortium use case | Accepted | 2026-11-14 | 8 |
| [0014](0014-one-package-panel-on-every-node.md) | One package and a panel on every node | Accepted | 2026-11-14 | 9 (first OS) |
| [0015](0015-panel-accounts-are-postgresql-roles.md) | Panel accounts are PostgreSQL roles | Accepted | 2026-11-14 | 10 |
| [0016](0016-go-backend-react-frontend-openapi-first.md) | Go backend and React/TypeScript frontend, OpenAPI first | Accepted; frontend framework superseded by 0017 | 2026-11-14 | - |
| [0017](0017-vue-for-the-web-panel.md) | Vue for the web panel | Accepted | 2026-11-14 | Supersedes the frontend framework of 0016 |
| [0018](0018-manager-leader-follows-the-map-group-primary.md) | The manager leader is the server of the map group primary | Accepted | 2026-11-14 | - |
| [0019](0019-cluster-state-in-a-system-database.md) | Control-plane state lives in a system database with embedded migrations | Accepted | 2026-11-14 | - |
| [0020](0020-no-bundled-metrics-store.md) | No bundled metrics store; kingresd samples and exports metrics itself | Accepted | 2026-11-14 | - |
| [0021](0021-failover-in-kingresd.md) | Failover is part of kingresd, with leases; Patroni and etcd are not bundled | Accepted | 2026-11-14 | Supersedes 0012 and the Patroni and etcd parts of 0008 |
| [0022](0022-the-complete-scan-stays.md) | The row-moving distributed scan stays as the path that answers every query | Accepted | 2026-11-15 | - |
| [0023](0023-one-extension-source-for-several-majors.md) | One source of every extension builds against each supported PostgreSQL major (18 and 19) | Accepted | 2026-11-15 | 1 (extends 0002) |
| [0024](0024-agpl-for-kingresdb-code.md) | KingresDB's own code is licensed under the GNU Affero General Public License, version 3 | Accepted | 2026-11-15 | 3 (supersedes 0004) |
| [0025](0025-restore-of-the-system-database-is-accepted-by-an-operator.md) | A restored system database is accepted by an operator, and the acceptance is a signed event | Accepted | 2026-11-23 | - |
| [0026](0026-the-candidate-of-the-map-group-receives-the-proof-of-the-operators-session.md) | The candidate of a map group failover receives the proof of the operator's session | Accepted | 2026-11-23 | - |
| [0027](0027-transfers-are-staged-encrypted-files-run-as-the-requesting-role.md) | Exports are staged encrypted files, and transfers run as the role that asked | Accepted | 2026-11-23 | - |
| [0028](0028-ledger-tables-are-carried-as-data-and-restart-their-chain.md) | Ledger tables are carried as data, and the imported ledger starts a new chain | Accepted | 2026-11-22 | - |
"Open decision #" refers to the table "Open decisions" in architecture.md.

## Statuses

- **Proposed**: written, not yet accepted by the project owner.
- **Accepted**: the project owner accepted it. The rest of the project follows it.
- **Superseded by ADR NNNN**: replaced. The text of a superseded ADR is kept.
- **Accepted; the part X is superseded by ADR NNNN**: the decision stands except for the named part.

An accepted ADR is not rewritten. If the decision changes, write a new ADR that says what it supersedes, and change only the status line of the old one. A dated note at the end of an ADR may state what became of a detail (an implemented default, a tool that was replaced) without rewriting the decision.

## How to add an ADR

1. Copy [0000-template.md](0000-template.md) to `NNNN-short-title.md`, where `NNNN` is the next unused number (never reuse a number) and the title is lower case with hyphens.
2. Fill in every section. Cite the section of architecture.md the decision comes from, or say that none does. Keep sourced facts, **[Recommendation]** items and **[Needs verification]** items apart, as architecture.md does.
3. State the costs honestly. State which **[Needs verification]** items the decision depends on.
4. Set the status to Proposed, and add a row to the index above in the same change.
5. When the project owner accepts it, set the status to Accepted and put the date of acceptance in the Date field.
6. Add a work-log entry (see [../dev/documentation.md](../dev/documentation.md)).
