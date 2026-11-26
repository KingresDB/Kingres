<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
# ADR 0018: The manager leader is the server of the map group primary

- Status: Accepted
- Date: 2026-11-14
- Source: [design/control-plane-2.md](../design/control-plane-2.md), decision D1; architecture.md, "Map nodes" and "Proposed architecture"

## Context

The architecture gives every server a `kingresd` and says one of them is the elected manager that runs tasks and accepts changes to the cluster. It planned to take the election from the same external store the failover engine uses. That store does not exist before the high-availability phase, but tasks and cluster changes are needed earlier, for a one-server cluster and for joining servers.

The map group is already the source of truth for cluster state (ADR 0008), and it has exactly one primary at a time.

## Decision

Until the high-availability phase, the manager leader is the `kingresd` on the server that holds the primary instance of the map group (shard group 0). Every other `kingresd` forwards mutating cluster operations to it. No second agreement protocol is run.

## Consequences

- No consensus store has to be shipped, embedded or supervised for the first releases.
- Leadership cannot disagree with the place where cluster state is written: the leader is, by definition, next to the primary it writes to.
- While the map group has no primary, nothing that changes the cluster can run. Reads and ordinary queries keep working. This is the cost already accepted in ADR 0008.
- Automatic failover of the map group needs an external decision maker, so this rule is revisited in the high-availability phase; the task engine uses leases on task rows, which keep working when leadership moves.

## Alternatives considered

- **Embedded etcd in every `kingresd` now.** It gives an election independent of PostgreSQL, but adds membership management and a failure mode before anything needs it.
- **A lease row in the system database.** Equivalent in effect to this decision, since only the map group primary can write the row, with more moving parts.
