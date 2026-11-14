<!-- SPDX-License-Identifier: PostgreSQL -->
# ADR 0009: 2PC without a global snapshot at first

- Status: Accepted
- Date: 2026-11-14
- Source in architecture.md: "Distributed transactions: 2PC, without a global snapshot at the start"; Risks table; roadmap phases 2 and 8

## Context

Sourced facts: Citus states that cross-node transactions are atomic, durable and consistent but do not have full distributed snapshot isolation; its maintenance daemon performs 2PC recovery and distributed deadlock detection. In PostgreSQL, `PREPARE TRANSACTION` is the XA-style first phase; `max_prepared_transactions` defaults to 0, so it must be raised and the server restarted; a prepared transaction holds locks and, if orphaned, blocks VACUUM. The "Global snapshots" (Clock-SI) patches on pgsql-hackers did not reach the core. Postgres Pro Shardman does global visibility with a CSN snapshot from a physical clock in a patched PostgreSQL.

**[Recommendation]** in the document, three options in increasing complexity: (a) 2PC only, Read Committed in each shard, no global snapshot promise; (b) a Postgres-XL-style GTM or timestamp oracle, with a single point of failure and added latency; (c) CSN/HLC with a core patch. Start with (a), accept the anomaly "a cross-shard read may see a partially committed distributed transaction", and do (c) only after the rest has stabilised. Single-shard transactions are fully correct.

## Decision

Distributed transactions use two-phase commit with Read Committed isolation in each shard and no global snapshot. The anomaly above is documented and tested. Transactions that touch one shard are fully correct. A global snapshot (CSN/HLC) is considered only in roadmap phase 8 and only with a specific use case.

## Consequences

- No core patch is needed for transactions at this stage, which fits ADR 0001.
- Applications can see a partially committed cross-shard transaction in a cross-shard read. Documentation and routing advice (keep transactions on one shard where correctness needs it) must say so. Citus has the same limitation (risks table).
- `max_prepared_transactions` must be raised and the server restarted; a recovery worker must resolve orphaned prepared transactions, or VACUUM is blocked. Patroni, in its Citus mode, sets `max_prepared_transactions` to `2*max_connections` if not configured; whether KingresDB relies on this is not decided.
- Orphan handling and in-doubt transaction persistence after failover are the hard parts. **[Needs verification]** The Citus prepared-transaction naming pattern, the `pg_dist_transaction` table, the reconciliation algorithm and the wait-for graph for distributed deadlock detection come from background knowledge; they are Citus internals and must not be copied (ADR 0004), so KingresDB's own design must be derived and tested. **[Needs verification]** Fencing and in-doubt persistence after failover also come from background knowledge.
- A later move to (c) needs a core patch and a new ADR.
- The ledger must record global IDs and prepared hashes across shards (see the document's "Interaction with sharding"); the lack of a global snapshot is compatible with per-shard chains.

## Alternatives considered

- **(b) GTM or timestamp oracle.** Rejected for the start: single point of failure and added latency (Postgres-XL's GTM is cited as a failure example).
- **(c) CSN/HLC global snapshot.** Deferred: it needs a core patch and the document wants the rest stable first; Postgres Pro Shardman's approach requires patched PostgreSQL.
- **No cross-shard writes at all.** Not discussed in the document.
