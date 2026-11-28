<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
# ADR 0030: Atomic visibility of distributed transactions by validating ordinary snapshots against durable per-participant evidence

- Status: Proposed
- Date: 2026-11-28
- Source: [../design/phase8-atomic-visibility.md](../design/phase8-atomic-visibility.md), [../architecture.md](../architecture.md) ("Distributed transactions: 2PC, without a global snapshot at the start", phase 8)

## Context

ADR 0009 accepted 2PC without a global snapshot. On 2026-11-28 the owner decided to build phase 8 and, in the same decision, that **no core patch to PostgreSQL is made**. A statement that spans shard groups reads each group at its own snapshot, so a distributed transaction can be seen on one group and not on another (a fractured read), and one statement can see a group in two states.

A consistent cut across groups would need a total order of commits that every snapshot respects; with existing extension APIs no such order can be made (the commit timestamp of an ordinary commit is not under an extension's control, and the draw happens in code no hook reaches); the design that builds it with two core hooks was reviewed twice and is kept as a rejected alternative ([../design/phase8-timestamp-snapshot-rejected.md](../design/phase8-timestamp-snapshot-rejected.md)).

## Decision

1. Every participant of a distributed transaction writes, inside that transaction and before `PREPARE`, one row (`dtx_member`) with the transaction's common id and the list of participating **groups** (group ids, not node ids). A durable per-group recording watermark (`ready`) says that no row can be missing; a group that is not ready, or was started or promoted recently, makes covered statements fail or wait. The row is visible exactly when the transaction is visible at that group.
2. A **read-only statement that reads two or more groups** acquires one snapshot per group in one parallel **round** (a helper `REPEATABLE READ READ ONLY` session for each remote group; a candidate snapshot for the local group), reads a report computed under each snapshot, and **validates** that every distributed transaction visible at one group is visible at every participant in the statement. A violation is resolved by **a new round at all groups** after a wait (a snapshot of an earlier round is never kept across a wait); the wait is bounded; a bound exceeded **fails the statement** with a retryable error.
3. The origin bounds the prepare phase (`dtx_window`) so that the reader's window is complete.
4. Nothing is drawn from a clock; only monotonic values of one boot of one node are compared. No server change, no restart, no preload, no commit timestamps.
5. Every uncertainty is an error rather than a read (design section 7), under the conditions of 7.1; until they are established by the model, the harness and a review this is a design goal.
6. The model and the differential tests come first; a Jepsen-style harness gates every increment (ADR 0034).

## Consequences

- Fractured reads of distributed transactions and two states of one group in one covered statement disappear. Causal order across groups, write skew and staleness remain (ADR 0031).
- Writers pay one insert per participant per distributed transaction and the bounded prepare phase; covered cross-group statements pay about one round trip; single-group statements pay nothing.
- The correctness argument is the design's section 4.6, with six assumptions each of which is a mutant of the model.
- The local snapshot swap in `ExecutorStart_hook` is the one technical risk that the spike checks (design 1.2 item 1).
- Once accepted this supersedes ADR 0009 in its "no global snapshot" decision for covered statements; ADR 0009's status changes then, and not before.
- ADR 0022 is unchanged.

## Alternatives considered

- Timestamp snapshot with two core hooks: stronger (cut, causality with tokens), declined (ADR 0032).
- CSN in the core: a large core change; declined with the same decision.
- A central timestamp oracle or GTM: a single point of failure, and the visibility work remains.
- A commit-phase lock between readers and distributed commits: serialises commits against reads and needs several round trips.
- Documentation only: rejected by the owner.
