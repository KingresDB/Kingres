<!-- SPDX-License-Identifier: AGPL-3.0-only -->
# ADR 0006: Sharding MVP first

- Status: Accepted
- Date: 2026-11-14
- Source in architecture.md: "Phased roadmap (recommendation)"; "Query path"; Open decisions #5

## Context

The roadmap in the document is phased: phase 0 foundation, phase 1 sharding MVP, phase 2 DML/DDL propagation, 2PC and pushdown, phase 3 panel MVP, phase 4 bucket movement and the Databases and Security pages, phase 5 HA, phase 6 ledger, phase 7 signing and anchoring, phase 8 optional global snapshot and multi-node ledger. The document states that no sourced time estimate was found for any phase, so it gives no person-month estimates.

Open decision #5 notes that the ledger does not depend on sharding and can be moved earlier. For the query path, the document recommends the router layer first (`planner_hook`, CustomScan, a connection pool to workers), then colocated pushdown and aggregates, and defers the multi-shard optimizer. It recommends `postgres_fdw` plus partitioning only for a first prototype.

The document also concludes that the main value of the project lies in thin maintainable added logic, that the ledger should be treated as the main product, and that a control plane probably gives the greatest differentiation (these are inferences).

## Decision

After phase 0, the first deliverable is the sharding MVP (phase 1): an extension skeleton, the catalog, `create_distributed_table` with N buckets, a node registry, and a router or fast-path planner with CustomScan and a connection pool. The ledger and the panel follow the roadmap order.

## Consequences

- Sharding is the part with the highest risk (the document names the maintenance tax of sharding, citing Timescale), so its problems surface first.
- The panel and the ledger are delayed. The ledger is independent of sharding and can be moved earlier if priorities change; that would need an update to the roadmap and a new ADR.
- The document's own prioritisation view treats the ledger as "the main product". Starting with sharding is therefore a sequencing choice, not a statement of priority. The two statements are in tension and the document does not resolve them.
- Each phase must include tests, documentation and a release tag.
- Every **[Needs verification]** item that phase 1 depends on (for example the hash function details in `src/backend/partitioning/partbounds.c`) must be checked before it is relied on.

## Alternatives considered

- **Ledger first.** Listed in decision #5. The document notes it does not depend on sharding and can be moved earlier. Not chosen because the recommendation follows the roadmap.
- **Panel first.** Listed in decision #5; no reasoning given beyond the roadmap order. The panel needs a cluster to manage, which the sharding work provides (inference).
- **A `postgres_fdw`-based prototype as the product.** Rejected for the product: weak at shard-aware pruning, parallel async execution over many shards and distributed DML/2PC; acceptable only for the first prototype.
