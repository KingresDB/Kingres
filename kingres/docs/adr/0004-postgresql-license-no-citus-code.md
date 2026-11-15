<!-- SPDX-License-Identifier: AGPL-3.0-only -->
# ADR 0004: PostgreSQL License; no Citus code

- Status: Superseded by [ADR 0024](0024-agpl-for-kingresdb-code.md) in its choice of licence (2026-11-15); its rule that no Citus code is copied stays
- Date: 2026-11-14
- Source in architecture.md: "License and trademark"; "The key decision: an extension, not changes to the executor"; Risks table (AGPL in Citus); Open decisions #3

## Context

Sourced facts: the PostgreSQL License permits use, modification and distribution for any purpose, provided the copyright notice, the licence text and both disclaimer paragraphs are kept in all copies. According to PGXN metadata, Citus is licensed under AGPL-3. The marks "Postgres", "PostgreSQL" and the Slonik logo are registered trademarks of the PostgreSQL Community Association of Canada; saying that software is "derived from PostgreSQL" is allowed without approval, while using the marks in a company name, product name or domain needs permission.

**[Recommendation]** in the document: keep the `COPYRIGHT` file untouched and add a copyright line for KingresDB; say "derived from PostgreSQL" in the documentation; do not use Slonik or the name Postgres in the domain or logo; rebuild Citus concepts rather than copy code unless AGPL is acceptable.

## Decision

KingresDB's own code is released under the PostgreSQL License, the same licence as upstream. No code from Citus (AGPL-3) is copied; Citus is used only as a source of concepts and published architecture. The same rule applies to other code whose licence is incompatible. New files carry an SPDX identifier (`SPDX-License-Identifier: AGPL-3.0-only`) as described in `kingres/docs/dev/coding-standards.md`.

## Consequences

- Derivative work is free for others under permissive terms; this also means there is no copyleft protection for KingresDB's own code. The document does not discuss this cost; the project owner has accepted it.
- Citus code, tests and comments cannot be reused, so the equivalent work is written from the concepts. The architecture document's Citus-based sections (catalog design, planner layers, 2PC, rebalance) are concepts only.
- Developers must know the boundary between concept and code. The coding standards state the rule and ask for a question when in doubt.
- Third-party dependencies must have compatible licences; each new dependency gets a licence check.
- **[Needs verification]** The interaction of two clauses of the trademark policy (use "on competing products, that is, databases derived from PostgreSQL" is prohibited, yet "derived from" is allowed as a factual statement) is ambiguous and no source clarifying it was found. Legal counsel must review it before any public release. This is open and does not change the licence decision.
- **[Needs verification]** The licences of VTAdmin, TiDB Dashboard and Patroni (MIT in background knowledge) have not been confirmed; this matters for bundling (ADR 0012, ADR 0014).

## Alternatives considered

- **Apache-2.0.** Listed as an option in decision #3. The document recommends the PostgreSQL License, the same as upstream; it gives no separate reason against Apache-2.0.
- **AGPL.** Listed as an option. The document says that anything other than AGPL means no Citus code can be copied, and treats copying Citus as off the table for any other licence; choosing AGPL would allow copying but would be a different product position. The document also notes CockroachDB's licence change (November 2024) as a warning for open-core positioning.
- **Proprietary.** Listed as an option; not recommended. No reasoning beyond the above is given.
