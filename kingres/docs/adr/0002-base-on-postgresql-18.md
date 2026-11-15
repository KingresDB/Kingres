<!-- SPDX-License-Identifier: AGPL-3.0-only -->
# ADR 0002: Base on PostgreSQL 18 (`REL_18_7`), rebase to 19 later

- Status: Accepted
- Date: 2026-11-14
- Source in architecture.md: "Branch choice and mirror mechanics"; Open decisions #1

## Context

Sourced facts in the architecture document: PostgreSQL ships one major release a year, supports each for five years, and publishes minor releases at least every three months. Version 18 is supported until 2030-11-14. PostgreSQL 18.7 is the latest GA release as of 2026-11-14. PG19 is at RC2 with GA scheduled for 2026-11-19, which would give support to about 2031. Citus 14.x supports PG 16 to 18.

Verified when this ADR was written: upstream has both `REL_18_STABLE` and `REL_19_STABLE` branches (`git ls-remote`). The local tree is the tag `REL_18_7` (commit `f5c41f6`, "Stamp 18.7."), branch `kingres-18`.

**[Recommendation]** in the document: stable branches receive only fixes and low-risk changes, whereas `master` means chasing unreleased changes. If the team can wait a few days, PG19 after GA gives the longest support horizon.

## Decision

The base is PostgreSQL 18 at upstream tag `REL_18_7`, kept as the product branch `kingres-18` and rebased onto each later `REL_18_x` tag. The project moves to PostgreSQL 19 later, once the tooling around it has caught up, by creating a `kingres-19` branch. The mechanics are in `kingres/docs/dev/upstream-sync.md`.

## Consequences

- Development starts immediately on a released, supported version.
- Moving to PG19 is a planned cost, not an option to skip. Part of that cost is the document's own risk: "annual rebase cost on each new major". The size of the PG19 move depends on the number of core patches (ADR 0001) and on PG19's breaking changes.
- **[Needs verification]** The list of breaking changes in PG19 (`release-19.html`) was not read. Until it is, the effort to move to PG19 is unknown. Check it before scheduling the move.
- **[Needs verification]** The current Windows build requirements for PG18/19 were not confirmed; this does not block the decision because builds happen in WSL2 (ADR 0005).
- Extensions must compile against PG18 headers first; porting to PG19 headers is a later task.
- Starting on 18 means the first KingresDB releases carry about four years of upstream support, not five.

## Alternatives considered

- **PG19 after GA.** Longest support horizon (about 2031). Not chosen now because GA was not yet out when the decision was taken (scheduled 2026-11-19) and the PG19 breaking-change list had not been read. It remains the planned next base.
- **`master`.** Rejected in the document: it means chasing unreleased changes.
- **Older supported versions (for example PG 16 or 17).** Not discussed in the architecture document. Citus 14.x covers PG 16 to 18, but the document gives no reason to prefer an older base.
