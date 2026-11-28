<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
# ADR 0031: Snapshot isolation across groups, causal only through a carried timestamp, with a skew-bounded recency, is the guarantee

- Status: Proposed
- Date: 2026-11-28
- Source: [../design/phase8-global-snapshot.md](../design/phase8-global-snapshot.md) (sections 5.9 and 6), [ADR 0030](0030-global-snapshot-by-hybrid-clock-timestamps.md)

## Context

A global snapshot can promise more or less. Strict serializability needs bounded-error clocks with commit wait, or restarts; serializability across groups needs a distributed SSI; snapshot isolation needs a consistent cut. CockroachDB and YugabyteDB close the window of clock skew with uncertainty restarts; this design does not restart. The documentation must state exactly what a statement gets and what a single-group statement still gets.

## Decision

1. With `snapshot.mode = on`, a statement or transaction that spans groups reads one consistent cut: no fractured reads of a distributed transaction, one state per group per statement, one cut per `REPEATABLE READ` transaction. It does not promise serializability across groups (write skew remains), and `SERIALIZABLE` stays per node.
2. **Causal order holds only through a carried timestamp.** The session token is `max(last commit, last snapshot, min_ts)`; it is carried automatically inside a session and by the router per client connection; across sessions the client passes it. Without it, a transaction can be invisible for up to the clock skew after a commit that preceded it. The wording "no inverted causal order" is not used without the token.
3. Recency is bounded by `max_forward_drift` (default 250 ms). A bounded-recency mode (the statement waits out the guard) is built in the same increment as the integration, not later.
4. A single-group statement keeps its local snapshot and pays almost nothing (measured).
5. Waits and refusals are bounded in time and retryable (`KS001`): prepared transactions, the horizon, a bucket switch. **Bounded in time is not a promise of success:** availability of global reads depends on prompt 2PC recovery, stated in the limitations.
6. The mode is off by default in the first release; `setSnapshotMode` is a checked task.

## Consequences

- limitations.md changes from "no global snapshot" to the exact list of what remains, including read-your-writes across pooled connections and the horizon limits.
- The Jepsen-style tests check that G1 and G-single are absent and allow G2 (write skew); the causal test fails without the token by design and passes with it and with bounded recency.

## Alternatives considered

- Promise strict serializability: needs hardware or restarts; hybrid clocks alone do not give it (Jepsen's CockroachDB analysis).
- Uncertainty restarts: needs statement restart, and a client retry inside `REPEATABLE READ`.
- Promise serializability across groups: a research project.
- Promise less (atomic visibility only): ADR 0032's fallback.
