<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
# ADR 0031: Snapshot isolation across groups, with a skew-bounded recency, is the guarantee

- Status: Proposed
- Date: 2026-11-28
- Source: [../design/phase8-global-snapshot.md](../design/phase8-global-snapshot.md) (section 6), [ADR 0030](0030-global-snapshot-by-hybrid-clock-timestamps.md)

## Context

A global snapshot can promise more or less. Strict serializability needs bounded-error clocks with commit wait, or restarts; serializability across groups needs a distributed SSI; snapshot isolation needs a consistent cut. The documentation must state exactly what a statement gets, and what a single-group statement still gets.

## Decision

1. With `snapshot.mode = on`, a statement or transaction that spans groups gets snapshot isolation across the groups: no fractured reads, no inverted causal order for causally related transactions, one state per group per statement. It does not promise serializability across groups (write skew remains), and `SERIALIZABLE` stays per node.
2. Recency: a transaction that is not causally related to an earlier commit may miss it by up to the clock skew (bounded by `max_forward_drift`, default 500 ms). A session carries its last commit timestamp, and a token (`last_commit_ts`, `min_ts`) carries causality between sessions.
3. A single-group statement keeps its local snapshot and pays nothing.
4. Waits and refusals are bounded and retryable (`KS001`): prepared transactions, the horizon, a bucket switch.
5. The mode is off by default in the first release; `setSnapshotMode` is a checked task.

## Consequences

- limitations.md changes from "no global snapshot" to the exact list of what remains.
- The Jepsen-style tests check that G1 and G-single are absent and allow G2 (write skew).
- An optional bounded-recency mode (wait out the skew) is possible later.

## Alternatives considered

- Promise strict serializability: needs hardware or restarts; hybrid clocks alone do not give it (Jepsen's CockroachDB analysis).
- Promise serializability across groups: a research project (distributed SSI).
- Promise less (atomic visibility only): ADR 0032's fallback.
