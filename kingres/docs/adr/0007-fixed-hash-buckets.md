<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
# ADR 0007: Fixed hash buckets (default 1024)

- Status: Accepted
- Date: 2026-11-14
- Source in architecture.md: "Catalog and bucket mapping"; "Map nodes: the data mapping inside PostgreSQL itself"

## Context

Sourced facts: Citus keeps shard mapping in catalog tables whose placements refer to a `groupid` rather than a host, so failover changes only the node table. Couchbase uses 1024 vBuckets with CRC32, the cluster manager sends the map to clients, and each replica is a separate set placed on a different node.

**[Recommendation]** in the document: choose a fixed, large number of buckets (for example 1024 or more) when creating the cluster, because changing N means a complete resplit; rebalance is then only the movement of whole buckets, not a rehash. The bucket function is `bucket = hash(key) % N`, using `hashint4extended`/`hashtextextended` with a fixed seed, so that routing is consistent with a local `PARTITION BY HASH (MODULUS N, REMAINDER r)` on the worker. The unit of mapping is the bucket: with N=1024 the map is about a thousand rows per colocation group, is cached in every routing node, and each query only computes `hash(key) % N`. The Databases page sets the bucket count (default 1024) at creation, and it cannot be changed later.

## Decision

Distributed tables use a fixed number of hash buckets per colocation group, 1024 by default, chosen at creation. A key maps to a bucket with `hash(key) % N`, and the bucket map assigns each bucket to a group. Rebalance moves whole buckets.

## Consequences

- Rebalance and replica placement are simple: only whole buckets move, nothing is rehashed.
- The map is small enough to cache everywhere and no per-row lookup is needed.
- The bucket count cannot be changed without a complete resplit, so a cluster that grows far beyond the planned bucket count has a hard limit. The default must be large enough; the document suggests 1024 or more but gives no sizing evidence.
- Granularity is coarse: with few servers many buckets sit on each; with very many servers, 1024 buckets limits the useful number of groups. The document does not quantify this. Hotspots inside one bucket cannot be split.
- **[Needs verification]** The details of the PostgreSQL partitioning hash (the seed and `hash_combine64`) come from background knowledge; check `src/backend/partitioning/partbounds.c` before the bucket function is fixed, because a wrong match with local hash partitioning would break routing consistency. The Citus default of 32 shards was also not confirmed.
- The hash function becomes part of the on-disk and cross-version contract; changing it is a data migration.

## Alternatives considered

- **Consistent hashing with virtual nodes.** Rejected in the document: a fixed bucket count is the simplest model for rebalancing and replica placement.
- **Range sharding.** Rejected for the same reason; the document notes it is useful for range scans but has a hotspot risk.
- **Row directory (row-to-node mapping).** Rejected: the mapping table grows as large as the data itself and every query needs an extra lookup.
- **Citus-style shards as plain `tbl_<shardid>` tables over a 32-bit hash space divided into N contiguous ranges.** Mentioned in the document as an alternative to the local-partition approach; it was not chosen for the recommended design, and the document does not weigh the two in detail.

## Note of 2026-11-20: the default that was implemented

The decision above says 1024 buckets by default. The implemented default is **128**, decided during the sharding MVP (work log of 2026-11-14; [design/shard-mvp.md](../design/shard-mvp.md)): a bucket is stored as one hash partition, and 1024 partitions per table is too many relations on a small cluster. It is set in two places: the setting `kingres_shard.default_bucket_count` of the extension (default 128, range 1 to 4096; `contrib/kingres_shard/src/kingres_shard.c`), and the control plane, which offers and uses 128 as the default of `sharding.bucketCount` (range 1 to 4096; `kingres/control-plane/internal/ops/databases.go` and the contract). The rest of this ADR, a fixed number of buckets chosen at creation, stands. The title and the index keep the figure of the time of the decision.
