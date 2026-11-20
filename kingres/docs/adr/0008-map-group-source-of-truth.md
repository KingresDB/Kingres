<!-- SPDX-License-Identifier: AGPL-3.0-only -->
# ADR 0008: Map group in PostgreSQL is the source of truth; etcd only for leader election

- Status: Accepted
- Date: 2026-11-14
- Source in architecture.md: "Map nodes: the data mapping inside PostgreSQL itself"; "Proposed architecture (recommendation)"; Risks table

## Context

Sourced facts: Citus keeps its mapping on the coordinator (group 0); Couchbase keeps metadata in Chronicle, an Apache-2.0 Raft implementation written in Erlang, which the document says is usable only if you use Erlang. Patroni needs etcd (or an equivalent) anyway.

**[Recommendation]** in the document: the catalog tables `kingres.node`, `kingres.dist_table`, `kingres.bucket_map`, `kingres.colocation` and `kingres.task` are kept on a "map group", one PostgreSQL primary with one or two synchronous replicas (quorum commit) and Patroni failover. No separate Raft store is built for the map. etcd holds only manager leader election and node health. The reason is one fewer component, and map changes in the same PostgreSQL transaction as the rest of the metadata. Rules: the bucket is the mapping unit; the map is readable on other nodes and writes happen only on the map group primary, propagated to others with 2PC; every change has a monotonic `map_version` and workers answer a "stale map" error to routers with an old version; `bucket_map` points to `group_id`, not a host, so shard-group failover does not need a map write, and routers read each group's current primary from etcd/Patroni.

## Decision

The authoritative cluster state (nodes, bucket map, colocation, tasks, desired state) is stored in catalog tables on the map group, a Patroni-managed PostgreSQL group with synchronous replicas. etcd is used only for manager leader election and node health (and by Patroni for its own purposes). `kingres-manager` is stateless.

## Consequences

- No Raft store is built or maintained; the manager reuses PostgreSQL's transactions and replication.
- While the map group is failing over, rebalance, distributed DDL and topology changes pause. Ordinary queries continue on other routing nodes with the cached map. The document lists this as a risk ("The coordinator as a SPOF and DDL bottleneck") with the mitigation: a synchronous replica and Patroni for the map group, a map cache on routing nodes, metadata synchronisation to all nodes.
- Correctness depends on the map version protocol and on the routers reloading and retrying. This must be built and tested.
- Before metadata is synchronised to every node (roadmap phase 5), all clients must connect through the map group primary.
- The document says map updates propagate to other nodes "with 2PC" while also saying the map change is made in a single PostgreSQL transaction on the map group. These describe different steps (the local change, then propagation to copies), but the exact protocol is not specified and must be designed.
- **[Needs verification]** The behaviour when the map group and a shard group fail simultaneously must be tested with a prototype. The document expects routing not to lock up because the router reads current primaries from etcd/Patroni, and says this is unproven.
- The Patroni and etcd availability become part of the routing path for failover cases. How embedded etcd membership grows is also unconfirmed (ADR 0014).

## Alternatives considered

- **A separate Raft store for the map** (the Couchbase Chronicle model). Rejected: one more component to build and keep coordinated, and map changes could not share a transaction with the rest of the metadata.
- **Row-to-node directory.** Rejected: see ADR 0007.
- **etcd as the map store.** Not explicitly discussed in the document. etcd is used for leader election and health only; the document does not evaluate holding the map there.

## Note of 2026-11-20: Patroni and etcd

The parts of this ADR that name Patroni (failover of the map group) and etcd (leader election and health) are superseded. [ADR 0021](0021-failover-in-kingresd.md) puts failover, for the map group too, into `kingresd` with leases, and does not bundle Patroni or etcd; [ADR 0018](0018-manager-leader-follows-the-map-group-primary.md) takes the manager leader from the map group primary, not from etcd. The decision that the map group in PostgreSQL is the source of truth for the catalog stands.
