<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
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

## Update 2026-11-21: what the lease became in parts 1 and 2

The decision stands. What changed in the mechanism, so that the ADR matches the code (`internal/lease`, `docs/design/ha-phase5-part2.md`):

- A lease can be held by a server that does not run the primary of the group: the **fence lease** of a failover is adopted (`Adopt`) by the leader and handed to the server of the standby (`Transfer`, `transferTo`), and the old holder can `Release` it at a clean stop. An adopted lease guards no instance until it is **attached** to the promoted one (`Attach`); an attached adoption is never forgotten by a late release of a failed task.
- The grantors answer a request of another holder with the time to wait: the old lease plus the safety margin. The margin (`marginMs`) is part of `GET /lease/state` and the servers must agree on it.
- A primary that was stopped without its lease is kept down by **markers** in the lease directory (`fenced`, `waitlease`, `relinquished`), per group, and by the start gate of the instance manager; the lease rounds start it again when they hold the lease and remove the markers. A planned switchover's rollback (`Resume`) writes the wait marker and does not start anything itself.
- A promotion carries a proof: the catalog term, the fence of the task run and, for a planned switchover, the shutdown checkpoint that `switchover_verify` checks on the standby before `pg_promote`.
- The read of the topology in a renewal round is bounded by one renewal period (a catalog that does not answer does not keep the rounds from being asked), a round is decided by the first majority and never waits for the slowest grantor, and the log of a lost lease says what the rounds saw.
- Not changed: the lease is still the proof. The stop of an instance, the term and the fence are what a task checks around it, not instead of it.

## Update 2026-11-23: the clean stop keeps its lease, and the fenced stop of the map group

Two more things of the lease mechanism that the code has and this ADR did not say (`internal/lease`, `cmd/kingresd/failover.go`, `docs/design/ha-phase5-part2.md`):

- **`HoldBack`.** The clean stop of a planned switchover writes the `relinquished` marker and **keeps renewing the lease** until the stop is over (a fast shutdown can outlast the lease); the renewal ends with `ReleaseGroup` after a clean stop and with `Relinquish` after any other end. The authority of the primary still ends at its last renewal plus `duration - margin`; only the last renewal moves later.
- **The fenced stop** (`fencedStop`, mode `immediate`), used by a hard failover of any group whose old primary answers and, since the hard failover of the map group of a live leader's server, by that case: the candidate server is recorded (map group only), the renewal ends and the marker is written, the instance is stopped with an immediate shutdown, and **only when it is seen stopped** is the lease handed over (`Transfer`, which also refuses while a primary runs under the lease). A primary that does not stop keeps a lease that nobody renews: its watchdog stops it when the lease file runs out, and the candidate takes the lease after that. Nothing in this path promotes without the lease.
