<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
# ADR 0031: What atomic visibility guarantees, and what it does not

- Status: Proposed
- Date: 2026-11-28
- Source: [../design/phase8-atomic-visibility.md](../design/phase8-atomic-visibility.md) (sections 0, 3 and 7), [ADR 0030](0030-atomic-visibility-by-validated-snapshots.md)

## Context

The mode must be named for what it gives and must state its limits with examples, so that nobody reads it as a global snapshot. Operators decide what to run on it from this text.

## Decision

1. **The guarantee, in one sentence:** with atomic visibility enabled for a database, a read-only statement that reads more than one shard group never sees a distributed transaction on one group and not on another, and sees each group in one state for the whole statement; if the protocol cannot be sure, the statement fails with a clear error.
2. **Covered:** read-only `SELECT` statements reading two or more groups, in autocommit, or in an explicit `READ COMMITTED` transaction before its first write; and, at a separate level, the first query of a `REPEATABLE READ` transaction. Not covered: statements of a transaction that has written, cursors, statements with user functions that can read tables, `FOR UPDATE`. A statement that is not covered runs exactly as today, and `EXPLAIN` says so with the reason.
3. **Not given, and documented with examples and workarounds:** causal order across groups (a reply visible without its post), snapshot isolation, write skew across groups, serializability across groups (`SERIALIZABLE` stays per node), staleness. Workarounds: colocate related data in one group; write dependent rows in one transaction.
4. The name of the setting and of every page is "atomic visibility"; the words "global snapshot" are not used for it.
5. Every uncertain point ends in one of three SQLSTATEs (retryable unavailability, evidence missing, not covered under strict coverage); a wrong answer is never returned.

## Consequences

- limitations.md changes from "no global snapshot" to this exact text, with the examples of the design (section 3.3).
- The harness checks equality of twin rows read in one covered statement and expects the anomalies that remain to be possible (they are documented, not defects).
- A later causal-order mode would be a new ADR (design section 15, the upstream proposal).

## Alternatives considered

- Promise snapshot isolation: needs a global order (ADR 0032).
- Call the mode "global snapshot": would promise more than it gives.
- Cover statements of write transactions: needs the validation inside the transaction's own sessions (a rewrite of every fragment); possible later.
