<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
# ADR 0030: The global snapshot is a hybrid-clock timestamp that each group turns into an ordinary snapshot

- Status: Proposed
- Date: 2026-11-28
- Source: [../design/phase8-global-snapshot.md](../design/phase8-global-snapshot.md) (sections 3, 5 and 11), [../architecture.md](../architecture.md) ("Distributed transactions: 2PC, without a global snapshot at the start", phase 8)

## Context

ADR 0009 accepted 2PC without a global snapshot and said that a move to a CSN or HLC snapshot needs a core patch and a new ADR. On 2026-11-28 the owner decided to build phase 8. A statement that spans shard groups reads each group at its own snapshot, so a distributed transaction can be seen on one group and not on another, a causal order across groups can be inverted, and one statement can see a group in two states.

Sourced facts: Spanner (bounded clocks and commit wait), CockroachDB and YugabyteDB (hybrid clocks with uncertainty intervals and restarts), Clock-SI (loosely synchronised clocks; a read that meets a prepared version waits for it), Postgres Pro Shardman (CSN snapshots in a patched PostgreSQL). No CSN or Clock-SI patch is in upstream PostgreSQL (commitfest entry 4912 is a smaller, different use). The design document has the table of sources and the facts read in the PG18 and PG19 trees.

## Decision

1. The commit timestamp of every transaction is a value of a hybrid logical clock, kept in PostgreSQL's commit timestamp store (`track_commit_timestamp = on`) and in the commit record.
2. A global snapshot is a timestamp `S`. Each group builds, at `S`, an ordinary snapshot (`xmin`, `xmax`, `xip`) that lists as in progress every transaction that is running or committed after `S`; the candidates come from periodic samples of the group's running list, so the cost does not depend on the oldest running transaction. Tuple visibility code is not changed.
3. Every timestamp a transaction can commit or prepare with is covered by a bound it published before it drew it (publish, then draw); a reader that has raised the clock to `S` either sees the bound and waits, or the draw is above `S`. A 2PC transaction's commit timestamp is the maximum of the prepare timestamps, **including the origin's own local part**, which follows the participant protocol; a participant raises its clock to the commit timestamp before the commit is visible; the drift guard never refuses a commit after the decision is durable.
4. A statement in `READ COMMITTED` gets one `S` for all its fragments, a `REPEATABLE READ` transaction one `S`. The result is snapshot isolation across groups; `SERIALIZABLE` stays per node.
5. The clock is a data-path clock. The lease and failover code keeps its monotonic clock and never uses it. After a promotion the clock stalls until physical time passes the persisted limit instead of jumping.
6. It lives in a new extension, `kingres_snapshot`, that does not depend on `kingres_shard`.

## Consequences

- Fractured reads, and two states of one group in one statement, disappear; write skew remains; causal order holds only through a carried timestamp (ADR 0031); reads older than a horizon (default 30 s), than a bucket switch, or than a `TRUNCATE` fail or are as wrong as PostgreSQL's own `TRUNCATE` is.
- Costs: `track_commit_timestamp` and a restart; a horizon that holds back vacuum by up to the horizon; a clock limit row; readers wait for in-doubt and committing transactions (per snapshot, not per row).
- The technique (a snapshot rebuilt from commit timestamps and slot bounds) was derived by the author and is not found in a source. Its correctness is gated by a model and a spike with differential tests (ADR 0032); if the gate fails, the fallback applies and this ADR is replaced.
- Once accepted this supersedes ADR 0009 in its decision "no global snapshot"; ADR 0009's status changes then, and not before.
- ADR 0022 is unchanged: every path, including the complete scan, runs at the snapshot.

## Alternatives considered

- CSN in the core (Shardman): the largest core change; the last fallback.
- A central timestamp oracle or GTM: a single point of failure or an extra service, and the same visibility work.
- TrueTime-style bounded clocks: hardware KingresDB does not assume; the wait idea stays as the bounded-recency mode.
- Restarts with uncertainty intervals: needs client retries inside explicit transactions.
- Atomic visibility only, no core change: not a lighter mechanism (ADR 0032); the fallback.
- Documentation only: rejected by the owner.
