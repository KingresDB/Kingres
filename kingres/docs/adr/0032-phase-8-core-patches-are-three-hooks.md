<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
# ADR 0032: Phase 8 adds three small core patches (hooks) to the server, with a decision gate and a fallback

- Status: Proposed
- Date: 2026-11-28
- Source: [../design/phase8-global-snapshot.md](../design/phase8-global-snapshot.md) (sections 4 and 11), [../dev/core-patches.md](../dev/core-patches.md), [ADR 0001](0001-thin-fork-with-extensions.md), [ADR 0023](0023-one-extension-source-for-several-majors.md)

## Context

KingresDB patches no server code (KP-0001 to KP-0004 are a version, a README, a build line and a licence). The global snapshot needs the commit timestamp to come from a hybrid clock and a snapshot to be rewritten from a timestamp. Verified in the trees: the commit timestamp and the commit record time come from `GetCurrentTransactionStopTimestamp()` unless a replication origin supplies one (PG18: `replorigin_session_origin_timestamp`; PG19: `replorigin_xact_state.origin_timestamp`); there is no snapshot hook.

## Decision

1. Register KP-0005 (a commit and prepare timestamp hook in `RecordTransactionCommit`, `RecordTransactionCommitPrepared` and `PrepareTransaction`), KP-0006 (a snapshot hook at the end of `GetSnapshotData`) and KP-0007 (a function that lets a backend advertise a lower `xmin`, validated against a floor). Each is a hook shaped so that it could be offered upstream, has a test that fails without it, and a `pg_compat.h` path for PG19.
2. A spike (P8-S) comes first and gates the rest: continue only if the three patches are under about 300 changed lines, change no visibility function, port to PG19 with only the rename above, and meet the benchmark limits of the design (section 10).
3. If the gate fails, the fallback is atomic visibility with no core change (design option 6), recorded by a new ADR; a larger core patch (CSN) only with the owner's explicit acceptance of its rebase cost.
4. These are the project's first changes to the server's transaction code: each patch is its own commit and register row, and is reviewed by someone who did not write it.

## Consequences

- A package that includes the patches is a new server binary; the upgrade guide says so.
- The rebase cost per major is a review of three small patches, listed in upstream-sync.md.
- An upstream proposal for the hooks is prepared (P8-1); its outcome does not block the work.

## Alternatives considered

- No core change, ever: leaves the causal anomaly and gives a more expensive protocol (design option 6).
- A full CSN patch: large and risky (design 3.4).
- A fork of the visibility functions: the deep fork the architecture document rejects.
