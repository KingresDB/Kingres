<!-- SPDX-License-Identifier: PostgreSQL -->
# ADR 0021: Failover is part of kingresd, with leases; Patroni and etcd are not bundled

- Status: Accepted
- Date: 2026-11-14
- Supersedes: [ADR 0012](0012-patroni-bundled-failover.md)
- Source: [design/ha-phase5.md](../design/ha-phase5.md)

## Context

ADR 0012 chose to bundle Patroni, with etcd as its store, as the failover engine. That decision was made before the control plane existed. Since then `kingresd` has become the component that creates, configures, starts and stops every PostgreSQL instance, renders `postgresql.conf`, `pg_hba.conf` and `pg_ident.conf`, holds the cluster's certificates, and talks to the other servers over an authenticated internal API (control-plane-2 and control-plane-3).

Patroni is built to own exactly those things: it writes the configuration, starts and stops PostgreSQL, and decides who is primary. Running it under `kingresd` would mean two managers for one instance, a Python runtime and an etcd cluster in every package, and a second membership protocol (etcd's) next to the cluster's own. The architecture document already listed bundling Patroni and embedding etcd as items that needed verification.

## Decision

Standby management and failover are implemented in `kingresd`. Two primaries for one group are prevented by leases: the `kingresd` next to a primary must keep renewing a time-limited lease with a majority of the cluster's servers and stops its instance when it cannot, and a standby is promoted only after a majority has seen the old lease expire and granted the lease to the new holder. Patroni and etcd are not shipped. Automatic failover is a setting that is off by default; manual failover and switchover use the same fencing.

## Consequences

- One manager per instance, one package without a Python runtime or a separate consensus store, and one membership list (the cluster's own).
- The safety of failover now rests on code written for this project instead of on Patroni's years of production use. This is the main cost. It is addressed by keeping the mechanism small (majority-granted leases with a safety margin and self-fencing, nothing else), by partition and clock tests that are part of the phase's definition of done, and by shipping automatic failover disabled by default.
- At least three servers are needed for automatic failover to survive the loss of one. With two servers, losing either leaves no majority, and the surviving primaries stop accepting writes when their leases expire.
- Lease timing depends on bounded differences in clock rate between servers and on `kingresd` being scheduled within the safety margin. The margin is a setting, and the tests include paused processes and clock steps.
- Operators who already run Patroni cannot plug it in.

## Alternatives considered

- **Patroni with etcd, as in ADR 0012.** Proven, but it duplicates the instance manager and adds two runtimes to the package.
- **Embedding etcd in kingresd and writing only the failover logic.** It would give a proven consensus store, at the cost of managing etcd membership inside the product; the lease protocol needs only "a majority acknowledged within a time bound", which the internal API already provides.
- **Manual failover only.** Simpler and safe, but the roadmap requires automatic failover with safety rules.
