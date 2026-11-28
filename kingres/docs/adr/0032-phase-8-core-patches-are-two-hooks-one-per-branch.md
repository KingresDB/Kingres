<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
# ADR 0032: Phase 8 adds two small core patches (hooks), one per product branch, with a decision gate and a fallback

- Status: Proposed
- Date: 2026-11-28
- Source: [../design/phase8-global-snapshot.md](../design/phase8-global-snapshot.md) (sections 1.2, 4 and 11), [../dev/core-patches.md](../dev/core-patches.md), [ADR 0001](0001-thin-fork-with-extensions.md), [ADR 0023](0023-one-extension-source-for-several-majors.md)

## Context

KingresDB patches no server code (KP-0001 to KP-0004 are a version, a README, a build line and a licence). The global snapshot needs the commit and prepare timestamps to come from a hybrid clock and a statement snapshot to be replaceable. Verified in both trees on 2026-11-28 (the design lists `file:function` for each): the commit timestamp and the commit record time come from `GetCurrentTransactionStopTimestamp()` unless a replication origin supplies one (PG18 `replorigin_session_origin_timestamp`; PG19 `replorigin_xact_state.origin_timestamp`); `RecordTransactionCommitPrepared` takes its time at the top of the function in PG18 and inside the critical section in PG19; `MarkAsPreparing` takes a `TransactionId` in PG18 and a `FullTransactionId` in PG19; `snapmgr.c` has no hook; `GetSnapshotData` has a reuse path that returns the static snapshot unchanged; and `ProcArrayInstallRestoredXmin` is exported in both trees, so no patch is needed to advertise a lower `xmin`.

## Decision

1. Register KP-0005 (a timestamp hook for commit, commit of a prepared transaction and prepare, in `xact.c` and `twophase.c`) and KP-0006 (a statement-snapshot hook in `snapmgr.c` that never edits the static snapshots and returns a snapshot the extension owns). KP-0007 of the first draft is dropped.
2. **Each patch is carried once per product branch.** Core patches are server files and differ between PG18 and PG19 (different field names, line positions and a signature); they are separate commits of one register row on `kingres-18` and `kingres-19`, and a major rebase re-tests both (a line in [../dev/upstream-sync.md](../dev/upstream-sync.md)). `pg_compat.h` and the one-source rule of ADR 0023 apply to the extensions, which must build against both patched trees.
3. Before code, a **model** of the clock protocol (publish, draw, observe, wait, prepare, failover) is checked. A **spike** then gates the rest. Continue only if: the differential tests D1 to D3 pass and a build with the wait rule removed or the clock read in the wrong place fails them; the model passes and its mutants fail; the non-test diff of the two patches is under about 200 lines per branch and changes no visibility function; PG19 passes with its own patch; the benchmark protocol of the design is met; an independent review of the patches and the slot protocol is done.
4. If the gate fails, the fallback is **atomic visibility with no core change** (design option 6). It is real work, not a lighter mechanism: a durable per-participant record of recent distributed transactions, an in-doubt wait as well, snapshot export and import so that one statement uses one snapshot per group, and validation before streaming fragments start; it fixes fractured reads and the two-states-of-a-group anomaly but not the causal one. A larger core patch (CSN) only with the owner's explicit acceptance of its rebase cost. Either way a new ADR records it.
5. These are the project's first changes to the server's transaction code: each patch is its own commit and register row, reviewed by someone who did not write it.

## Consequences

- A package that includes the patches is a new server binary; the upgrade guide says so, and `kingres_snapshot` refuses to load on a server without them.
- The rebase cost per major is a review of two small patches per branch.
- An upstream proposal for the hooks is prepared (P8-1); its outcome does not block the work.

## Alternatives considered

- No core change, ever: leaves the causal anomaly and gives a more expensive protocol (design option 6).
- A full CSN patch: large and risky (design 3.4).
- A fork of the visibility functions: the deep fork the architecture document rejects.
- Editing the static snapshot in a hook at the end of `GetSnapshotData`: breaks the reuse path (design 1.2 item 6).
