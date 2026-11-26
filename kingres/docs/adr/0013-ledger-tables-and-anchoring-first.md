<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
# ADR 0013: Ledger tables and external anchoring first; consensus only with a consortium use case

- Status: Accepted
- Date: 2026-11-14
- Source in architecture.md: "Blockchain: tamper-evident ledger tables first, consensus last"; Open decisions #8; Risks table; Conclusion

## Context

The document splits "blockchain" into three models: (1) a centralised tamper-evident ledger with an external digest, (2) a multi-node permissioned blockchain database with consensus and deterministic execution, (3) anchoring a digest to a public chain or timestamp authority.

Sourced facts: consensus is needed only when several mutually distrusting parties write shared data without a trusted operator (Wuest and Gervais). Microsoft says its ledger verifies centrally hosted data "without the complexity and performance cost of network consensus". Microsoft SQL Ledger uses Merkle trees with SHA-256 and block hashes chained; an administrator with machine control can edit files, and the ledger guarantees detection, not prevention. Amazon QLDB reached end of support on 2025-07-31. The IBM research prototype (about 4,000 lines of C on PostgreSQL) needed determinism restrictions and no maintained public version was found. Live model-2 products on PostgreSQL are application-chain platforms.

**[Recommendation]**: phase 1 ledger tables, phase 2 signing and anchoring (WORM storage plus OpenTimestamps or a TSA, a witness copy, a standalone verifier), phase 3 consensus only with a specific consortium use case (order-then-execute on a deterministic SQL subset). The document treats the ledger as the main product (inference).

## Decision

KingresDB builds ledger tables (append-only and updatable) with block hashes and Merkle roots, then signing and external anchoring, and a standalone verifier. Multi-node consensus is built only when a concrete consortium use case exists.

## Consequences

- The ledger detects tampering; it does not prevent it. A superuser, file access, restoring an older backup or replacing the binary cannot be prevented from within the database; the defence is an external digest and verification off the host. Product documentation must say this.
- The ledger needs a deterministic canonical serialisation, versioned with `hash_version`, independent of `DateStyle`, `extra_float_digits`, `bytea_output` and locale.
- The ledger fits ADR 0010: the hash chain depends on a single commit order per shard.
- Sharded ledger: each shard group has its own chain, and the map group builds an aggregate digest. The document says no source was found for a ledger on sharded PostgreSQL, so this is an untested design proposal and a possible differentiator only if validated early with a prototype.
- **[Needs verification]** The mechanics of options (a) to (d) (for example `XACT_EVENT_PRE_COMMIT` Merkle root and commit-order block membership, bypassing triggers with `session_replication_role`), ed25519 support in pgcrypto, S3 Object Lock semantics, and Ethereum gas cost were not checked. Whether the ledger can be protected from superusers without core patches is unknown (ADR 0001).
- A consortium use case can later require deterministic SQL restrictions and consensus; the document lists the nondeterminism sources that would then need to be removed.
- This ADR does not set the order of phases relative to sharding; see ADR 0006.

## Alternatives considered

- **Multi-node consensus (model 2) now.** Rejected: only needed with mutually distrusting writers; costly; IBM's prototype had major restrictions and throughput drops as orderers are added; the market evidence shows model-2 database projects abandoned or turned into application chains.
- **Anchoring to a public blockchain only (model 3 alone).** The document treats anchoring as phase 2 on top of a ledger; it does not consider it as a standalone product.
- **Audit logging only (pgAudit, Database Activity Streams).** The document notes this gives auditing without cryptographic verifiability.
- **Hash-chain triggers only.** Recommended against for production: bypassable by privileged users and serialise writes at the chain tail; the document allows it only for simple cases, with MVP path (b) plus a block-builder worker.
