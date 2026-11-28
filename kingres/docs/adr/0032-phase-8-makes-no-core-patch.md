<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
# ADR 0032: Phase 8 makes no core patch; the hook design is a rejected alternative

- Status: Proposed
- Date: 2026-11-28
- Source: [../design/phase8-atomic-visibility.md](../design/phase8-atomic-visibility.md) (sections 2, 9.3 and 15), [../design/phase8-timestamp-snapshot-rejected.md](../design/phase8-timestamp-snapshot-rejected.md), [../dev/core-patches.md](../dev/core-patches.md), [ADR 0001](0001-thin-fork-with-extensions.md)

## Context

The first design of phase 8 proposed two core patches, KP-0005 (a commit and prepare timestamp hook) and KP-0006 (a statement snapshot hook), per product branch, behind a spike gate; it was reviewed twice by an independent reviewer. **On 2026-11-28 the owner declined core patches**: the server code stays as upstream plus KP-0001 to KP-0004.

## Decision

1. **No change to PostgreSQL server code is made for phase 8.** KP-0005 and KP-0006 are not registered. The extension relies only on APIs and hooks that PostgreSQL already exports, listed in one table (design 9.3, repeated in [../dev/upstream-sync.md](../dev/upstream-sync.md) as the first check on every minor and major upgrade); version differences stay in `src/pg_compat.h`; the tests run on PG18 and PG19.
2. The timestamp design with hooks is **kept only as a rejected alternative**, with its review record, so that nobody has to rediscover it.
3. **What is given up:** a consistent cut across groups and causal order (they need a total order of commits that a snapshot respects). The weaker guarantee is stated honestly in ADR 0031.
4. **Reducing the risk of relying on internals:** the table of internals, the spike on both majors with assertions enabled, tests of the differential kind, and the rule that a change in any listed internal fails a test.
5. **Long term:** the project proposes the hooks to the PostgreSQL project as a design discussion (design 15), with no dependency; if accepted upstream, a later ADR can build causal order as an extension, without a fork patch.
6. If the spike shows that the local snapshot swap in `ExecutorStart_hook` is unsafe, the plan restricts to validating remote groups with the local group read through a loopback helper session; if neither works the feature is not built (design 13, gate).

## Consequences

- The fork stays thin (ADR 0001); no per-branch patch, no rebase of server code for this phase.
- The extension is more complicated than the hook design would have been (evidence rows, validation, retakes), which is why the model and the harness come before code.
- The ADR 0023 rule (one extension source for PG18 and PG19) applies unchanged.

## Alternatives considered

- Two core hooks (KP-0005, KP-0006), per branch: declined by the owner.
- A full CSN patch: declined with the same decision.
- Hacks on exported internals (setting the replication-origin timestamp of every commit): not safe, and it changes what logical replication decodes (design 2).
