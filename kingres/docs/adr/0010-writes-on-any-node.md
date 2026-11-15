<!-- SPDX-License-Identifier: AGPL-3.0-only -->
# ADR 0010: Writes on any node: a primary per server and a router in the agent; no conflict-resolving multi-master

- Status: Accepted
- Date: 2026-11-14
- Source in architecture.md: "Writes on any node: a primary on every server plus a router on every node"; Risks table; "Interaction with sharding"

## Context

The goal in the document is that a client can connect to any server in the cluster and both read and write, without giving up strong consistency. The document says this whole subsection is a design decision without a direct source. Sourced fact: libpq supports multiple hosts and `target_session_attrs` for client-side failover.

**[Recommendation]** in the document: every server accepts writes, but each bucket has exactly one writable primary at any moment, and a write to a server that does not own the bucket is forwarded to the owner. Placement is chained: with N servers there are N shard groups; server *i* runs the primary of group *i* and, for `replica_count = 1`, a standby of group *i-1*, each instance with its own port and data directory. Primary and standbys of a group never share a server (or server group). `kingres-agent` listens on one well-known port and proxies connections without parsing SQL: the read-write port goes to the local primary if healthy, otherwise any healthy primary from etcd/Patroni state; an optional read-only port goes to a local standby. Conflict-resolving active-active replication (pgEdge Spock, EDB PGD) is rejected.

## Decision

Every server runs a writable primary of one shard group and standbys of neighbouring groups. The agent on each server runs a client router that picks a target instance without parsing SQL. Each bucket has one writer at a time. KingresDB does not do conflict-resolving multi-master replication.

## Consequences

- Clients can write through any server, with one writer per bucket, so there is no conflict resolution and a single commit order per shard, which the ledger's hash chain needs.
- A write to a bucket owned by another server takes an extra network hop.
- Two or more PostgreSQL instances per server split that server's memory (`shared_buffers`, work memory, connection slots).
- Every primary keeps pooled connections to every other primary, so connections grow with the square of the number of servers; per-node pool limits are required.
- When a server fails, its neighbour runs two primaries and carries about double the write load until recovery (listed in the Risks table). Capacity headroom, a "restore preferred primaries" action and alerts are the mitigations.
- Read-your-writes is not guaranteed on the read-only port when replication is asynchronous.
- **[Needs verification]** Running several Patroni-managed instances on one host, the pool sizing, and the router's behaviour during a switchover must be confirmed with a prototype and failure tests. This design depends on metadata being synchronised to all nodes (roadmap phase 5); before that, clients connect through the map group primary.
- Ties to ADR 0008 (map), ADR 0011 (replicas) and ADR 0012 (Patroni).

## Alternatives considered

- **Conflict-resolving active-active (Spock, PGD).** Rejected: it gives up strong consistency, and without a single commit order per shard the ledger hash chain cannot work. The document says these systems suit HA and geo distribution, not data sharding.
- **Dedicated coordinator or router nodes only.** Not discussed as a rejected option; the document's design (every node can plan and route) follows the Couchbase and CockroachDB pattern of identical nodes.
- **Client-side routing only** (clients list several hosts). Kept as an extra, not as the mechanism: the agent's router gives a writable session from any server.
