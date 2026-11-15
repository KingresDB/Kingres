<!-- SPDX-License-Identifier: AGPL-3.0-only -->
# ADR 0001: Thin fork with minimal core patches; logic in extensions

- Status: Accepted
- Date: 2026-11-14
- Source in architecture.md: "Short answer"; "The pattern of existing forks: a small patch set plus extensions"; "The key decision: an extension, not changes to the executor"; Risks table

## Context

Sourced facts in the architecture document: deep forks fall behind upstream. Greenplum is based on PostgreSQL 9.4; Apache Cloudberry 2.x is on PG 14 and its main branch on PG 16, and the upgrade from 14.4 to 16.9 brought in more than 5,700 upstream commits; YugabyteDB stayed on PG11 until v2025.1; Postgres-XL stopped in 2018. OrioleDB, Babelfish and Percona pg_tde keep a small published patch set plus extensions. Citus is a pure extension (planner hook, CustomScan, executor hooks, xact callbacks, utility hook, background worker) and tracks PG versions. Timescale removed multi-node in 2.14 because it cost every new feature extra work. The PostgreSQL wiki says sharding forks lag behind community versions.

## Decision

KingresDB is a thin fork of a stable PostgreSQL branch. Sharding and ledger logic live in extensions (`contrib/kingres_shard/`, `contrib/kingres_ledger/`). A change to an upstream file is made only when an extension cannot do the job. Each such change is a small, documented commit listed in `kingres/docs/dev/core-patches.md`.

## Consequences

- Rebase cost on each new major release stays proportional to the number of core patches, which is the mitigation the architecture document gives for its first risk.
- Some designs are constrained by what hooks allow. The ledger is the clearest case: the architecture document says the strongest ledger option (a custom WAL resource manager or table AM, with UPDATE/DELETE/TRUNCATE/DROP forbidden in the core rather than by triggers) may need more than triggers and hooks. A custom resource manager can be registered from an extension; whether protection "even from superusers" can be done without core patches is not established. **[Needs verification]** If it cannot, the specific patch goes through the register with its justification.
- Each core patch needs a justification, a test and an upstream-status entry, which costs effort on every change to upstream files.
- Global snapshots (the CSN/HLC option in the architecture document) need a core patch; this ADR does not forbid that later, but it will have to meet the same bar.
- Wire-protocol-level or planner-level features are limited to what hooks expose.

## Alternatives considered

- **Deep fork** (changing the planner, executor and storage directly, as Postgres-XL, Greenplum and Cloudberry did). Rejected: the evidence above shows the fork then falls years behind upstream.
- **Pure extension on stock PostgreSQL, no fork.** The architecture document does not list this as a rejected option; it recommends a thin fork so that the small core patch set, the rebrand and the bundled package are possible. The document does not give a dedicated argument for choosing a fork over no fork at all beyond those points; treat the exact reasoning as a gap.
- **External router or proxy only** (SPQR, PgDog, Multigres, Neki in the document's landscape table). Not rejected explicitly; the document notes none of them has cross-shard ACID with a global snapshot and says that gap is where a fork could add something.
