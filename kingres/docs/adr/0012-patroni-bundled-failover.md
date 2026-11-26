<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
# ADR 0012: Patroni bundled as the failover engine

- Status: Superseded by [ADR 0021](0021-failover-in-kingresd.md)
- Date: 2026-11-14
- Source in architecture.md: Open decisions #7; "HA, replication, DDL and backup"; "One package, and a panel on every node"

## Context

Sourced facts: Patroni has a documented Citus mode in which each group is a separate Patroni cluster with its own `citus.group` (coordinator = 0), sets `synchronous_mode` to quorum if unset, keeps `pg_dist_node` updated after failover, and pauses the group's traffic on the coordinator during a switchover. Patroni's REST API (`/cluster`, `/switchover`, `/failover`, `/reinitialize`, `/config`) is a usable pattern for HA operations. Patroni needs a distributed store such as etcd.

**[Recommendation]** in the document: each shard group is a Patroni cluster or equivalent; the map group is HA with Patroni; quorum commit prevents loss of acknowledged commits. Patroni is bundled with its Python runtime in the package. The alternative, native failover in the agent, drops the Python dependency but is more work and is worth doing only if bundling Python proves to be a packaging problem.

## Decision

Patroni is bundled in the `kingresdb` package and is the failover engine for every group, including the map group. `kingresd` starts and supervises it. Native failover in the agent is not built unless bundling proves to be a packaging problem.

## Consequences

- Failover logic is not written from scratch; Patroni's behaviour is documented and used in the field.
- The package carries a Python runtime and a Python application next to a Go binary, which increases size and the build matrix.
- **[Needs verification]** Bundling Patroni with a Python runtime in `.deb` and `.rpm`, the licence of every bundled component (Patroni's MIT licence is from background knowledge), and the resulting package size are unconfirmed and must be checked before packaging starts. If bundling fails, the fallback is native failover in the agent, which would need a new ADR.
- **[Needs verification]** Running several Patroni-managed instances on one host (needed by the chained placement, ADR 0010) has not been tested.
- Patroni's Citus mode is Citus-specific; KingresDB uses Patroni's generic cluster management and does its own work for bucket and node bookkeeping, so the Citus-specific behaviours listed above are a pattern, not a feature that can be switched on.
- Failover of a shard group must not require a map write (ADR 0008); routers read the current primary from etcd/Patroni state.

## Alternatives considered

- **Failover implemented in the agent.** Not chosen: more work. It would remove the Python dependency and would be reconsidered only if bundling Python causes a packaging problem.
- **Use only an external, user-installed Patroni.** Not discussed in the document; it conflicts with the single-package goal (ADR 0014).
