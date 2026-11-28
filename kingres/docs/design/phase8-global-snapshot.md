<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
# Phase 8, part 1: a global snapshot across shard groups (design)

Status: Proposed design, 2026-11-28; not implemented. Nothing here is built. Where this
document says "exists" it describes the repository at `kingres-18` (d1344c1aa9) and names the file.

Builds on [shard-phase2a.md](shard-phase2a.md) (2PC, the distributed scan),
[shard-phase2b.md](shard-phase2b.md), [shard-phase4.md](shard-phase4.md) (bucket moves),
[ha-phase5.md](ha-phase5.md) and [ha-phase5-part2.md](ha-phase5-part2.md) (failover),
[ledger-phase7.md](ledger-phase7.md) (aggregate digest), [ADR 0009](../adr/0009-two-phase-commit-no-global-snapshot.md),
[ADR 0022](../adr/0022-the-complete-scan-stays.md), [ADR 0023](../adr/0023-one-extension-source-for-several-majors.md).
The second half of phase 8, the multi-node ledger, is [phase8-multinode-ledger.md](phase8-multinode-ledger.md).
The decisions are the proposed ADRs 0030 to 0034 ([../adr/README.md](../adr/README.md)).

On 2026-11-28 the project owner decided to build phase 8 (it had been kept out of 0.1.0 as
"future roadmap"). This document is the design that comes first.

Kinds of statements are kept apart as in architecture.md: **[Fact]** (checked in the repository or
in a cited source), **[Recommendation]**, **[Needs verification]**. A source marked "background"
was not re-fetched on 2026-11-28; check it before an ADR that depends on it is accepted.

## 1. What exists, and what the problem is

### 1.1 Facts about the code (read on 2026-11-28)

1. **[Fact]** A transaction that wrote on two or more nodes uses 2PC (`contrib/kingres_shard/src/node_conn.c`,
   header comment and `precommit()`): `PREPARE TRANSACTION '<gid>'` on every remote writer, one row
   per prepared remote in the origin's `kingres.dtx_log` written by the origin's own local
   transaction (the row is the commit decision), then `COMMIT PREPARED` after the local commit
   (`postcommit()`). `recovery.c` resolves prepared transactions the node originated, with a
   worker every `recovery_interval`; `kingres.dtx_incident` records the cases that asynchronous
   replication can make wrong.
2. **[Fact]** Each remote session starts its transaction with the isolation level of the local one
   (`kingres_shard_begin_xact`, `node_conn.c`: `BEGIN ISOLATION LEVEL ... ; <settings>; <time zone>;
   <dtx label>; <term claim>`). So a statement that spans groups reads every group at that group's
   own snapshot: in `READ COMMITTED` one per statement per group, in `REPEATABLE READ` one per
   transaction per group, taken when the group is first used. Nothing relates the snapshots.
3. **[Fact]** Every remote session is opened with `kingres_shard.forward_term` and proves it is a
   node session (`term.c`, `forward_auth.c`); remote statements go to the primary of the group.
4. **[Fact]** KingresDB patches no server code. The core patch register
   ([../dev/core-patches.md](../dev/core-patches.md)) has four entries (KP-0001 to KP-0004): a
   version, a README section, a build line and the licence file. Phase 8 is the first need for a
   change to the server's transaction code, which makes this the most important decision of the
   phase (section 4).
5. **[Fact]** `track_commit_timestamp` is not set by `kingresd` or used by any extension
   (`grep` over `contrib/kingres_*` and `kingres/control-plane`: no match).
6. **[Fact]** In the PG18 tree the commit timestamp of a transaction is the time of its commit
   record: `RecordTransactionCommit` (`src/backend/access/transam/xact.c`, about line 1462) takes
   `GetCurrentTransactionStopTimestamp()` unless a replication origin session set
   `replorigin_session_origin_timestamp`, writes it into the commit record and into the commit
   timestamp SLRU (`TransactionTreeSetCommitTsData`); `RecordTransactionCommitPrepared`
   (`twophase.c`, about line 2381) does the same with the `committs` it is given; a standby takes
   the value from the commit record at replay (`xact_redo_commit`, `xact.c` about line 6175).
   **[Fact]** In the PG19 tree (`kingres-19`) the same code reads
   `replorigin_xact_state.origin_timestamp` instead of `replorigin_session_origin_timestamp`. This
   is the kind of difference ADR 0023 puts in `pg_compat.h`.
7. **[Fact]** `XidInMVCCSnapshot` (`src/backend/utils/time/snapmgr.c`) decides visibility from
   `xmin`, `xmax`, `xip[]`, `subxip[]` and `suboverflowed`; there is no hook in snapshot creation
   (`snapmgr.h`, `procarray.h`: no hook declared).
8. **[Fact]** `kingresd` uses a monotonic clock for every decision (lease, heartbeat) and never
   sends a wall-clock time between servers (`kingres/control-plane/internal/clock/clock.go`). The
   hybrid clock of this design is data-path time. It must not be used by the lease code, and the
   two never mix.
9. **[Fact]** The row-moving distributed scan is the fallback of every query (ADR 0022). A global
   snapshot is a property of how the groups are read, not a new path; every path of `kingres_shard`
   (scan, pushdown, repartition, routed DML) must run at the statement's snapshot, or be refused
   with the existing retry error.
10. **[Fact]** The ledger's block builder orders transactions by id, not by commit order
    (`contrib/kingres_ledger/README.md`), and phase 7 says plainly that the aggregate "does not
    prove that the heads were read at the same instant" (ledger-phase7.md, 5.6).

### 1.2 The anomaly, precisely

Documented today ([../guide/limitations.md](../guide/limitations.md), "Consistency"): a reader can
see a distributed transaction's effects on one group before it sees them on another (a fractured
read). Two separate things hide under the one sentence, and the design fixes them in two levels:

- **A. Atomic visibility.** Distributed transaction D commits on groups A and B; a reader sees D
  at A and does not see D at B. Cause: A's `COMMIT PREPARED` finished before the reader's snapshot
  at A, B's had not before the reader's snapshot at B.
- **B. Cut consistency (snapshot isolation across groups).** W1 commits at A (single-group).
  Afterwards, caused by it or only later in real time, W2 commits at B. A reader sees W2 but not
  W1 because it took its snapshot at A before W1 and at B after W2. No distributed transaction
  is involved. Atomic visibility does not prevent this.

Also **C**: one statement that scans two distributed tables, or the same group twice, can see that
group in two states in `READ COMMITTED` (limitations.md says so). With a statement-level snapshot
it cannot.

## 2. Goals and non-goals

Goals:

1. A statement (READ COMMITTED) or a transaction (REPEATABLE READ) that spans shard groups reads
   **one consistent global snapshot**: A, B and C above disappear, with the exact exceptions of
   section 6.
2. Read-write distributed transactions get globally consistent snapshots and first-committer-wins
   conflict detection per group, as PostgreSQL does per node.
3. Single-group statements and transactions pay (almost) nothing.
4. Thin fork: the core change is as small as a hook can be, written so that it could be offered
   upstream, one source for PG18 and PG19 (ADR 0023).
5. Opt-in per cluster at first, with an upgrade path from 0.1.0/0.10 and a defined behaviour
   during a rolling upgrade.

Non-goals (and why):

1. **Strict serializability or external consistency.** That needs bounded-error clocks with commit
   wait (Spanner) or restarts (CockroachDB); the design gives snapshot isolation with a
   documented staleness bound by clock skew (section 6.3). `SERIALIZABLE` stays serializable per
   node only.
2. **Cross-group foreign-key or unique-constraint enforcement.** Not a snapshot matter.
3. **Reads on standbys at a global timestamp.** First release of the feature reads primaries only
   (as the router does today). A later increment can wait for replay to pass the timestamp.
4. **Lowering 2PC cost.** 2PC protocol is kept; one timestamp is added to it.
5. **Changing the complete scan** (ADR 0022). It stays; it simply runs at the snapshot.

## 3. Research: the options

### 3.1 What real systems do (sources)

| System | Mechanism | What it costs the system | Source |
|---|---|---|---|
| Google Spanner | TrueTime: a clock API that returns an interval `[earliest, latest]` with a known error bound; a commit timestamp is chosen, then the commit waits until the timestamp is surely in the past ("commit wait"); reads at a timestamp need no locks | GPS and atomic clocks in every data centre; commit latency of about the error bound | Corbett et al., "Spanner: Google's Globally-Distributed Database", OSDI 2012 (background) |
| CockroachDB | Hybrid logical clock (HLC) per node; a transaction has a timestamp; a read that meets a value inside its **uncertainty interval** (reader timestamp to timestamp plus `max_offset`, default 500 ms) moves its timestamp forward and refreshes (a restart); a node whose offset exceeds the limit is meant to stop | Restarts of reads; a bounded `max_offset`; no hardware; not strictly serializable per Jepsen's analysis | [pkg.go.dev hlc](https://pkg.go.dev/github.com/cockroachdb/cockroachdb-parser@v0.25.2/pkg/util/hlc), [Cockroach Labs, "living without atomic clocks"](https://cockroachlabs.com/blog/living-without-atomic-clocks/), [Jepsen, CockroachDB beta 20160829](https://jepsen.io/analyses/cockroachdb-beta-20160829), SIGMOD 2022 paper as summarised at [muratbuffalo.blogspot.com](https://muratbuffalo.blogspot.com/2022/06/enabling-next-generation-of-multi.html) (secondhand) |
| YugabyteDB | Hybrid time; "read restart" error when a read meets a row committed within the clock-skew window (`max_clock_skew_usec`, default 500 ms); `DEFERRABLE` read-only transactions wait out the skew instead | Restart errors that the application must retry inside explicit transactions | [YugabyteDB docs, read restart error](https://docs.yugabyte.com/stable/architecture/transactions/read-restart-error/) |
| Clock-SI (research) | Snapshot isolation for partitioned stores with loosely synchronised physical clocks: a transaction's snapshot is its start time; a partition **delays** a read until its clock passes the snapshot time, and delays a read that meets a prepared transaction whose prepare time is before the snapshot until it is decided; the commit time is the maximum of the prepare times | No central service; staleness and delay bounded by skew; no restarts | Du, Elnikety, Zwaenepoel, "Clock-SI", SRDS 2013 (background) |
| Hybrid logical clocks (research) | A clock that never goes back, stays within a bound of physical time, and carries causality through messages | One 64-bit value | Kulkarni et al., "Logical Physical Clocks", 2014 ([PDF](https://www.cse.buffalo.edu//~demirbas/publications/hlc.pdf)) |
| Postgres-XL | A central GTM hands out transaction ids and snapshots | Single point of failure and a round trip per transaction; the project fell behind upstream (architecture.md, "The pattern of existing forks") | architecture.md, [Postgres-XL](https://en.wikipedia.org/wiki/Postgres-XL) |
| TiDB (and other timestamp oracles) | A central timestamp oracle (PD) | Same as GTM, mitigated by batching and a replicated oracle | TiDB documentation (background) |
| Citus | None: "atomic, durable and consistent but not full distributed snapshot isolation"; 2PC with a maintenance daemon | Documented anomalies | architecture.md; [Citus README](https://raw.githubusercontent.com/citusdata/citus/main/src/backend/distributed/README.md) |
| Postgres Pro Shardman | CSN snapshots over a patched PostgreSQL (Clock-SI style, physical clock) | A large core patch carried by a commercial vendor | [Shardman architecture](https://postgrespro.com/docs/shardman/17/tutorial-arch); the "Global snapshots" thread of 2018-05 ([index](https://postgrespro.ru/list/thread-id/2385436)) |

### 3.2 State of the PostgreSQL core

**[Fact, searched on 2026-11-28]** No CSN or Clock-SI patch is in PostgreSQL. The last CSN activity
found is Heikki Linnakangas' "CSN snapshots in hot standby" (commitfest entry
[4912](https://commitfest.postgresql.org/50/4912), last change 2024-12), which is a different, much
smaller use of CSN (replacing known-assigned-xids on standbys) and was still unmerged when last
seen. In that thread Andrey Borodin noted that a distributed system would need to set the CSN from
another source (TrueTime or Clock-SI) and that hooks might be needed
([thread](https://www.postgresql.org/message-id/08da26cc-95ef-4c0e-9573-8b930f80ce27@iki.fi)).
**[Needs verification]** whether anything newer exists in 2026; an ADR that depends on "no
upstream CSN" must re-check the commitfest app.

Upstream has the building blocks this design uses: the commit timestamp SLRU
(`track_commit_timestamp`, `commit_ts.c`), per-transaction commit timestamps recorded in the commit
record and replayed on standbys, replication-origin timestamps, `PREPARE TRANSACTION` with a
`prepared_at` time kept in the two-phase state, `pg_visible_in_snapshot`, and `ExecutorStart_hook`.

### 3.3 The options for KingresDB

**Option 1: CSN in the core (Shardman style).** Replace the xip-based snapshot by a commit sequence
number per transaction, set from a distributed clock. Gives the most direct implementation of
Clock-SI visibility. Needs a core patch of a size that nobody has upstreamed in years, touches
procarray, clog or a new CSN SLRU, vacuum, hot standby and every visibility function. That is the
"deep fork" that architecture.md warns against (Greenplum and Postgres-XL). Rejected as the
first choice; kept as the fallback if option 2 fails its spike.

**Option 2: HLC commit timestamps, and a classic snapshot rewritten from a timestamp (recommended).**
The commit timestamp of every transaction is a value of a hybrid logical clock, stored where
PostgreSQL already stores commit timestamps. A global snapshot is a timestamp `S`. At each group
the snapshot at `S` is built as an ordinary PostgreSQL snapshot (`xmin`, `xmax`, `xip`) that lists
as in-progress every transaction that is running now **or committed with a timestamp above `S`**.
Tuple visibility code (`XidInMVCCSnapshot`, `HeapTupleSatisfiesMVCC`) is **not changed**. The core
gains two small hooks and one small function (section 4). 2PC commit timestamp is the maximum of
the prepare timestamps, as in Clock-SI. This is the recommended option; it rests on a technique that
the author derived and did not find in a source, so it is gated by a spike (section 11, P8-S).

**Option 3: A central timestamp oracle or GTM.** Single point of failure unless replicated;
adds a round trip to every distributed statement; still needs the same visibility mechanism as
option 2 at the groups. It only replaces the clock. Rejected: it adds a service and does not
remove the core work (limits already in ADR 0009).

**Option 4: TrueTime-style bounded clocks and commit wait.** Needs hardware or a cloud time
service with a published error bound. KingresDB runs on ordinary servers. Rejected for the
default; the commit-wait idea is kept as an optional bounded-staleness mode (6.3).

**Option 5: Restarts with uncertainty intervals (CockroachDB, YugabyteDB).** Needs the executor to
be able to restart a statement and, inside an explicit transaction, the application to retry. A
`READ COMMITTED` statement can restart safely, a `REPEATABLE READ` transaction cannot without
the client. Clock-SI's wait is simpler in PostgreSQL because the wait is inside the snapshot
creation. Rejected.

**Option 6: No core change, atomic visibility only (fallback ladder, level 1).** Solve anomaly A and
C but not B: each remote snapshot is exported with the in-doubt distributed gids; a coordinator
compares them and re-takes the snapshot of a group that disagrees (a "read atomic" protocol in the
RAMP spirit, using `pg_visible_in_snapshot`). Needs a durable record of the distributed
transactions that committed in a recent window, per group (one extra write per participant), and
a validation round per statement. It cannot repair a pushed-down partial aggregate unless the
snapshots are validated before the fragments run, which makes the statement wait for a
two-phase snapshot start. It gives no answer to B. Kept as the **fallback** if the core hooks are
refused or the PG19 port of them is too costly (decision gate in section 11), not as the plan.

**Option 7: Do nothing but document (today).** The owner has decided against.

### 3.4 Why option 2 and not option 1

| | Option 1 (CSN in core) | Option 2 (HLC + rewritten snapshot) |
|---|---|---|
| Core change | Large: new commit-sequence store, visibility, vacuum, standby | Two hooks (commit/prepare timestamp, snapshot) and one function (advertise a lower xmin); no visibility function changed |
| Cost of a snapshot at "now" | O(1) | O(1) at the group (nothing to rewrite when `S` is not older than the last commit seen); O(commits in the window) otherwise |
| Cost for old `S` | O(1) | Grows with the transactions committed between `S` and now; bounded by a horizon |
| Rebase cost per PG major | High | Low (hooks are a few lines each; PG19 already differs in the names the hook touches, see 1.1 item 6) |
| Risk | Known (it exists in Shardman) | The technique is new: needs the spike |

## 4. The core change (hooks, not a fork)

All of it is **[Recommendation]**; each item is a core patch of the register (ADR 0032). Each is
written as a hook that upstream might accept, has a regression or TAP test that fails without it
(rule 6 of core-patches.md), and has a `pg_compat.h` path for PG19.

**KP-0005, commit and prepare timestamp hook.** Declares
`TimestampTz (*commit_timestamp_hook)(TransactionId xid, bool prepared, TimestampTz proposed)` and
calls it where the commit record time is fixed: `RecordTransactionCommit` (before
`XactLogCommitRecord` and `TransactionTreeSetCommitTsData`, so both carry the same value, which
the standby replays), `RecordTransactionCommitPrepared`, and in `PrepareTransaction` for the
`prepared_at` of the two-phase state (stored in the two-phase file and shown by
`pg_prepared_xacts`, so the prepare timestamp survives a crash and is visible after a restart).
The hook runs inside a critical section: it may use atomics and a spinlock and must not allocate
or fail. PG19: the code reads `replorigin_xact_state.origin_timestamp`; the patch is the same shape
with that name. Size: tens of lines.

**KP-0006, snapshot hook.** Declares `void (*snapshot_hook)(Snapshot snap, SnapshotKind kind)`,
called at the end of `GetSnapshotData` for a snapshot that will be used for a statement or kept as
a transaction snapshot (not for catalog, historic or non-MVCC snapshots). The hook may replace the
`xip` and `subxip` arrays and lower `xmin`; it must set `suboverflowed` when it cannot list sub
transactions (so that visibility goes through `pg_subtrans`). Size: tens of lines.

**KP-0007, advertise a lower xmin.** A function in `procarray.c` modelled on
`ProcArrayInstallImportedXmin` that lets a backend set `MyProc->xmin` to a value older than its
fresh snapshot's, only if a registered floor (the horizon keeper, 5.5) is not above it, checked
under `ProcArrayLock`. Without it the rewritten snapshot's older `xmin` is not protected from
vacuum. Size: small; the part that needs the most review.

**[Needs verification]** (answered by the spike, P8-S): that these three are enough; that the
subtransaction rule is sound with `suboverflowed` forced; that a transaction snapshot under
`REPEATABLE READ` can be replaced once, at its creation, without side effects; that
`ExecutorStart_hook` alone (no KP-0006) is not enough for functions that take their own snapshots
(the hook version is needed because plpgsql statements inside a function call
`GetTransactionSnapshot` themselves). If the spike shows more than about 300 changed lines, or a
visibility function that has to change, the decision gate of section 11 applies.

**Settings the groups need.** `track_commit_timestamp = on` (restart; 10 bytes per transaction in
`pg_commit_ts`, and `shared_preload_libraries` carries the new library). `max_prepared_transactions`
is already required. **[Fact]** `kingresd` writes `shared_preload_libraries` itself (limitations.md,
"Columnar storage"); it must also set `track_commit_timestamp`, as a setting with a floor in the
Configuration page, restart needed, standbys first (the preload rule already stated for columnar).

## 5. The design

### 5.1 One new extension, `kingres_snapshot`

Everything that is not a core hook lives in a new extension, `kingres_snapshot` (AGPL header, one
source, `src/pg_compat.h`, ADR 0023). Reason: other lines of work currently change `kingres_shard`
(phase 15, repartitioned joins and writes), and `kingres_snapshot` does not depend on it at all:
it is a clock and a snapshot constructor that can be built and tested on one node. `kingres_shard`
integrates it later with a small change (section 11). The extension holds:

- the **hybrid clock** (5.2) in shared memory;
- the hook functions of KP-0005 and KP-0006;
- the **horizon keeper** background worker (5.5);
- SQL: `kingres_snapshot.hlc_now()`, `hlc_observe(ts)`, `current_snapshot_ts()`,
  `last_commit_ts()` (the session's causality token), the setting `kingres_snapshot.ts` (a session
  or `SET LOCAL` value that makes the next snapshots of this backend be taken at that timestamp), and
  a status view.

### 5.2 The clock

A hybrid logical clock with the precision of `TimestampTz` (microseconds since 2000-01-01), so the
value stored in `pg_commit_ts`, in commit records, in `pg_xact_commit_timestamp()`, in logical
replication origin timestamps and in `pg_prepared_xacts.prepared` stays a valid timestamp that
people can read. The logical counter is folded into the microsecond: `next()` returns
`max(physical_now, last + 1)`, `observe(t)` sets `last = max(last, t)`. One backend does an atomic
compare-and-swap on a 64-bit shared value; no lock. Rate limit: more than one million
timestamps per second per node would push the clock ahead of physical time by the excess; at that
rate the node is far beyond what PostgreSQL commits.

Rules:

1. Every commit, prepare and snapshot timestamp of a node comes from the clock, and the clock
   never goes back (also across a restart, 5.6).
2. Every message between nodes that carries a timestamp calls `observe` at the receiver, so a
   timestamp given out anywhere is below every timestamp that a later event of its causal chain
   gets anywhere.
3. **Forward drift guard.** A timestamp received that is more than `kingres_snapshot.max_forward_drift`
   (default 500 ms) above the node's physical clock is refused (error `KS` retryable, an alert
   `clockSkew`). It stops a node with a wrong clock from dragging the cluster's clock into the
   future. The value is the same kind of limit as CockroachDB's `max_offset`.
4. **Backward steps.** A step of the system clock backwards does nothing to the HLC (it holds the
   last value until physical time passes it) and raises the alert.

### 5.3 What a snapshot at timestamp `S` is, at one group

Inputs: `S` (an HLC value) and the group's primary. Output: a PostgreSQL snapshot that sees exactly
the transactions committed with a commit timestamp `<= S`, and every frozen/old transaction.

1. `observe(S)` (this raises the node's clock to at least `S`: every commit that gets its timestamp
   afterwards gets one above `S`).
2. Take the ordinary snapshot with `GetSnapshotData`. Everything in its `xip` is running now (or
   prepared).
3. **Wait rule (Clock-SI).** For each xid `x` in `xip`: if `x` is a prepared transaction whose
   prepare timestamp `p` (`prepared_at`) is `<= S`, or is a running transaction that has already
   drawn a commit timestamp `c <= S` but is not yet finished, then `x` may have to be visible at
   `S`. Wait for `x` to finish (the xid lock; bounded by `kingres_snapshot.prepared_wait_timeout`,
   default 5 s, after which the statement gets a retryable error `KS001`) and repeat from step 2. A
   prepared transaction with `p > S` cannot commit at or below `S` (its commit timestamp is the
   maximum of its prepare timestamps) and is simply in-progress for this snapshot. To make "has
   drawn a commit timestamp" decidable, the commit hook publishes in a shared slot (one per backend)
   a lower bound of the timestamp **before** it draws; step 3 waits for any slot whose bound is
   `<= S`. This is the "publish, then draw" order.
4. **Late commits.** Find the transactions that committed after `S` but before now: those with a
   commit timestamp `> S`. They must be invisible. Take the oldest `xmin_S` that can matter, from the
   horizon keeper's samples (5.5): the sample `i` with the largest `h_i <= S` gives `xmin_S = x_i`
   (every transaction with an id below `x_i` had finished with a timestamp `<= h_i` or aborted).
   For each xid in `[xmin_S, next_xid)` that is not in `xip`, read its commit timestamp from the
   commit timestamp SLRU; those with a timestamp `> S` are added to `xip`. Set `xmin = xmin_S`.
   Set `suboverflowed` (visibility will map subtransaction ids to their top-level transaction
   through `pg_subtrans`, which is retained for ids above `xmin`).
5. Advertise `xmin_S` with KP-0007 (refused when `S` is older than the horizon floor: error
   `KS001`, "snapshot older than the horizon").

Cost: step 4 reads the commit timestamp of every transaction id in the window. For a snapshot whose
`S` is about now (the coordinator's clock within a few milliseconds of the group's) the window holds
the transactions of those milliseconds. At 20 000 commits per second and a 3 ms window that is
about 60 ids. For a long window it is linear; the horizon bounds it. **[Needs verification]** by the
spike, measured (section 10).

### 5.4 Read-only and read-write transactions across groups

**Statement snapshot, `READ COMMITTED`.** The node that received the statement (the originator,
any node: writes are accepted anywhere, ADR 0010) takes `S = hlc.now()` when the statement
starts, takes its own group's snapshot at `S` (it observes `S`, which is a no-op for itself), and
sends every fragment with `kingres_snapshot.ts = S`. Where fragment commands are built today (all
go through `node_conn.c`; **[Needs verification]** the list: `fragment_scan.c`, `remote_scan.c`,
`dist_scan.c`, `batch_sender.c`, `row_router.c`, `copy_routed.c`, `merge.c`, `repartition.c`) the
command is prefixed with `SET LOCAL kingres_snapshot.ts = <S>;` in the same simple-query batch (no
extra round trip). Result: one statement, one timestamp, every group, every fragment, including
the local part and a statement that scans the same group twice (anomaly C gone).

**Transaction snapshot, `REPEATABLE READ`.** `S` is taken at the first statement of the
transaction (lazily, when it first needs a snapshot) and kept: each group's snapshot at `S` is
created when the group is first used, later than `S` for some groups. The group's snapshot at `S`
is valid as long as `S` is within the horizon at that moment; a transaction that touches a new
group more than `horizon` after its start fails there with `KS001` (retryable). A group already
used keeps its snapshot as any long transaction does.

**A single-group transaction** (all its statements routed to one group, no remote session at all,
the common case) runs on a plain local snapshot as today, with no HLC and no rewrite. It still
draws its commit timestamp from the clock (5.2), which is the only cost it pays. In `REPEATABLE
READ`, a transaction that is local at first and later reaches another group needs `S` from its first
statement, so under `snapshot.mode = on` a `REPEATABLE READ` transaction takes its snapshot
through the extension from the start (cost: step 2 only, since `S` is about now).

**Read-write transaction, one writer**: as today, no 2PC; the commit timestamp is drawn at the commit.

**Read-write transaction, two or more writers (2PC).** Added to the protocol of
phase2a section 1 (`precommit`, `postcommit`):

1. Prepare, as before, with a call in the same batch that takes the participant's prepare timestamp
   (`PREPARE TRANSACTION` stores it as `prepared_at`, and the extension returns it): the answer of
   the prepare batch carries `p_i`.
2. The originator computes `C = max(p_1 .. p_k, p_local, hlc.now())`, `observe(C)`, and writes the
   decision row in `kingres.dtx_log` **with `C`** (new column `commit_ts`, in `kingres_shard` 0.11,
   section 8). The local commit gets `C` as its commit timestamp (the hook returns the value the
   backend set, never lower than the clock's).
3. After the local commit, `COMMIT PREPARED` runs on each participant in the same batch as
   `SET kingres_snapshot.commit_ts = C` (a session value consumed by the hook of
   `RecordTransactionCommitPrepared`; it is reset by the next statement).
4. Recovery (`recovery.c`) commits a prepared transaction with the `C` that the `dtx_log` row holds.
   A prepared transaction with no row is rolled back, as today. No participant can commit with a
   timestamp that differs from `C`.

Why it is correct (sketch, Clock-SI): a reader at `S'` waits for every prepared transaction with
`p <= S'`, so it cannot take its snapshot while a transaction that might commit at or below `S'` is
undecided; after it is decided the commit timestamp `C >= p` is final and equal on all groups. A
reader with `S' < p_i` at any group sees D at no group (`C >= p_i > S'`). A reader with
`S' >= C` sees D at every group. A reader with `p_i <= S' < C` waits at group `i` until D is decided
and then compares `C` with `S'` (invisible). The same decision holds at every group, so atomic
visibility (anomaly A) holds; with the monotonic observe rule, anomaly B holds too.

**Write-write conflicts.** Under `REPEATABLE READ` with a rewritten snapshot, a concurrent update
that committed after `S` is invisible and `heap_update` reports the usual serialization failure
(`40001`), exactly as on one node, now across groups because every group uses the same `S`. In
`READ COMMITTED` the per-group `EvalPlanQual` re-check follows the update chain as it does today.
**[Needs verification]** that nothing in `ExecUpdate` assumes the snapshot has a consistent `xmax`
with the rewritten fields (the spike's regression tests include concurrent updates).

### 5.5 The horizon keeper

A background worker per instance (in `kingres_snapshot`):

- Every `kingres_snapshot.sample_interval` (default 20 ms) it records a pair `(h, xmin)`: the clock
  value read **before** a `GetSnapshotData` call, and that snapshot's `xmin`. A ring holds the pairs
  of the last `kingres_snapshot.horizon` seconds (default 10 s; 500 pairs).
- It keeps one registered snapshot at the oldest sample, so that `MyProc->xmin` of the worker is
  that `xmin` and vacuum, pruning and freezing never pass it (this is how the worker is the "floor"
  of KP-0007). It moves the registered snapshot forward as the oldest sample ages out.
- It is the reason a timestamp older than the horizon is refused. The cost is the cost of any
  transaction that holds an old `xmin`: bloat of at most `horizon` seconds of dead rows. This is
  the idea that `old_snapshot_threshold` implemented and that upstream removed in PostgreSQL 17
  (**[Fact]**: stated in the PG17 release notes; background). It is different in that the bound is
  enforced at the creation of a snapshot, not at its use.

A frozen tuple is visible to every snapshot. A snapshot at `S` could therefore see, through
freezing, a transaction that committed after `S`; the keeper's floor prevents freezing of any
transaction newer than the oldest sample (freezing requires the id to be below the oldest xmin).

### 5.6 Failover, restart and the clock

**[Recommendation]** The new primary must give timestamps above everything the old primary gave,
including snapshot timestamps that were handed out and never logged.

1. The clock has a persisted **limit**: a row in a one-row table of `kingres_snapshot`, updated by
   a worker to `now + max_forward_drift + 2 * limit_interval` every `limit_interval` (default 1 s),
   in an ordinary WAL-logged transaction. The clock never returns a value above the limit; if it
   would, it first waits for the update (a stall only when the worker is late). A snapshot or an
   observed `S` is bounded by `now + max_forward_drift`, so it never exceeds the limit.
2. At startup in the primary role and at **promotion**, the clock starts at
   `max(physical_now, persisted limit, GetLatestCommitTsData())`. With synchronous replication
   (the default of KingresDB for the cluster) the standby has the latest limit. With asynchronous
   replication the limit may be one interval behind; the promotion uses the physical clock plus
   `max_forward_drift` instead when the standby is known to lag (the standby's last replayed limit
   is older than two intervals). That is a deliberate stall, shown on the failover task.
3. Prepared transactions after a crash: `prepared_at` survives in the two-phase file, so a reader
   knows `p` after a restart (this is why KP-0005 stores it there). A prepared transaction whose
   `prepared_at` is older than the clock's limit at the previous start is treated as `p = 0`
   (a reader at any `S` waits for its outcome); recovery decides within `recovery_interval`.
4. A remote session at a failed primary is gone, so a `REPEATABLE READ` transaction that used it
   fails, as it does today (that is not changed).

### 5.7 Bucket moves, splits and DDL

- A bucket move copies the current row versions to the target by logical replication and switches
  the placement ([shard-phase4.md](shard-phase4.md)). Older versions are not copied. A snapshot at
  `S` older than the switch of a bucket cannot be answered at the target. **[Recommendation]** the
  switch records its commit timestamp as `switch_ts` in the move's catalog row; a fragment for a
  bucket sent with `S < switch_ts` fails with `KS001` and the statement is repeated by the client
  with a fresh `S` (the error text already exists for changing placement, limitations.md). A
  statement in progress at the switch gets the error; one transaction in `REPEATABLE READ` that
  started before a switch of a bucket it has not read yet gets it too.
- The commit timestamps of copied rows: logical replication applies with the origin's commit
  timestamp when `track_commit_timestamp` is on, so the moved rows keep their timestamps; this
  matters only for visibility at the target (all of them are `<= switch_ts`).
- A bucket **split** is handled like a move. DDL is run on every group in a transaction of its
  own (phase 2a, section 4): a statement that reads a table at `S` older than a DDL commit sees
  the catalog of its own group's snapshot, as on one node. **[Needs verification]** catalog
  snapshots are not rewritten (they are not MVCC statement snapshots), so a statement planned
  before a DDL may meet a table that changed; the check is that the existing `KS001` placement
  version error also covers it.

### 5.8 What is not rewritten

Catalog snapshots, snapshots for logical decoding, `SnapshotDirty`, `SnapshotSelf`, `CLUSTER`,
`VACUUM`, index builds, and the snapshots of background workers (including 2PC recovery and the
ledger block builder, which stay on the ordinary snapshot until the optional increment of section
11). Functions that run inside a statement use the statement's snapshot only if they call
`GetTransactionSnapshot()`, which under `REPEATABLE READ` returns the transaction snapshot (rewritten) and under
`READ COMMITTED` returns a fresh local snapshot **unless the hook rewrites it too**, which KP-0006
does when `kingres_snapshot.ts` is set: so a `READ COMMITTED` statement that runs a function that
reads a distributed table sees `S` in every statement of the function while the session value is set.

## 6. Guarantees, in precise words

### 6.1 What disappears (with `snapshot.mode = on`)

| Anomaly | Before | After |
|---|---|---|
| A. Fractured read of a distributed (2PC) transaction | Possible | Not possible: a read sees a distributed transaction on every group or on none |
| B. Causal anomaly across groups (sees W2 on B, not W1 on A, with W1 before W2) | Possible | Not possible when W2's client observed W1 (same session, or the token of 6.4); with no causal link it is bounded by clock skew (6.3) |
| C. One statement sees a group in two states | Possible in `READ COMMITTED` | Not possible: one `S` per statement |
| Read skew in a `REPEATABLE READ` transaction that spans groups | Possible | Not possible: one `S` per transaction |
| Write skew across groups | Possible | **Remains** (see 6.2) |

### 6.2 What remains

1. **Write skew and every serialization anomaly of snapshot isolation.** The result is snapshot
   isolation across groups. `SERIALIZABLE` is serializable per node only and the documentation says
   so (limitations.md keeps its sentence, with "across groups" made explicit).
2. **Staleness bounded by clock skew** (6.3).
3. **A snapshot older than the horizon** and **a snapshot older than a bucket switch**: refused with
   a retryable error.
4. **Reads on standbys** are not covered (non-goal 3).
5. **Single-group statements** keep the local snapshot of today: they are atomic per group, include
   every transaction committed on the group before they start, and see a distributed transaction
   either completely or not at all at that group (the 2PC `COMMIT PREPARED` is atomic on a group);
   the originator's client is told of a commit after every participant committed, so
   read-your-writes holds as it does today.
6. **Hot standby and ledger:** see 8.2 and the ledger document.
7. **Prepared and in-doubt transactions delay readers** (the Clock-SI wait): a reader whose `S` is
   above a prepared transaction's `p` waits until the transaction is decided. With a lost
   coordinator that is up to `recovery_interval` plus the time to commit or roll back (30 s by
   default), bounded by `prepared_wait_timeout` after which the reader gets `KS001`.

### 6.3 Clock skew, precisely

The design is correct (never shows a state that is not a consistent snapshot) for any skew. What
skew changes is **recency**: if node X's clock is `e` ahead of node Y's, a transaction that
commits on X at wall time `t` gets a timestamp near `t + e`; a client that starts a statement at
Y at wall time `t + d` (`d < e`) in no causal relation to the transaction takes `S` near `t + d`
and does not see it. So "after the commit returned, a new statement elsewhere sees it" holds
without a causal link only up to the skew bound. The guard of 5.2 refuses skew above
`max_forward_drift` and `kingresd` alerts at half of it. **Optional later:** a session setting
`kingres_snapshot.recency = bounded` makes a snapshot wait `max_forward_drift` (Spanner's commit
wait on the read side, YugabyteDB's `DEFERRABLE`); default off.

### 6.4 Causality across sessions

A session can read `kingres_snapshot.last_commit_ts()` after a commit and give it to another
session (`SET kingres_snapshot.min_ts = ...`); that statement takes `S = max(hlc.now(), min_ts)`. The
console and `kingres-cli` use it for "read after write" in a script. Within one session the value is
carried automatically (the session's last commit timestamp is the lower bound of its next `S`).

## 7. Failure handling

| Event | Behaviour |
|---|---|
| Coordinator (originator) lost after prepare, before its decision | No row, prepared transactions roll back at recovery; readers at `S >= p` wait until then (`<= recovery_interval + retention`), at most `prepared_wait_timeout`, then `KS001` |
| Coordinator lost after its decision row (with `C`) and before `COMMIT PREPARED` | Recovery on the originator (or after its failover, the standby that has the row: today's rules, phase 2a section 1 and `dtx_incident`) commits with `C` |
| Origin failed over to an asynchronous standby that lacks the decision | As today: `dtx_incident` kind `rolled_back_after_timeline_change` or `participant_lost`. A reader at `S >= C` may have seen D committed on a participant that later is told to roll back: the same incident, now also visible as a snapshot that "showed" a transaction that was undone. Documented; with synchronous replication it cannot happen |
| A participant fails over mid-commit | The prepared transaction is replayed on the standby with its `prepared_at`; the commit arrives with `C` from recovery |
| Clock stepped backwards or forwards on a node | HLC unchanged (5.2); forward steps beyond the guard raise `clockSkew`; the node refuses timestamps above its guard |
| A node with a very wrong clock | Its physical clock part is ignored by others only through the guard; it is refused as a source of `S` above the guard; `kingresd` marks it `clockSkew` critical and `check_cluster` reports it |
| Horizon exceeded | `KS001`, retry |
| Bucket switch | `KS001`, retry |
| `kingres_snapshot` not loaded or `track_commit_timestamp` off on a node | The node refuses a statement with a snapshot timestamp (error naming the setting); cluster-level `snapshot.mode` cannot be switched on until every instance reports it ready |

## 8. Upgrade and compatibility

### 8.1 One source, two majors

Everything above compiles for PG18 and PG19 from one source (ADR 0023): the two names of the
replication origin timestamp are behind `pg_compat.h`; the hooks are the same function pointers
in both trees; both are core patches carried on `kingres-18` and `kingres-19` with the same
files. **[Needs verification]** that KP-0005 to KP-0007 apply to the 19 tree unchanged except for
that rename (the spike builds both).

### 8.2 Installed 0.1.0 / 0.10 clusters, and mixed versions

- Core patches mean a **new server binary** for the feature. The package that contains them is a
  new release; a 0.1.0 cluster is upgraded the usual way ([../guide/upgrade.md](../guide/upgrade.md)).
  Until every instance of every server runs the new binary and `kingres_snapshot` is created in
  the databases (`CREATE EXTENSION`, which the extension registry's hook of phase 15 performs for
  new databases; for existing ones an operator-visible step of the upgrade), `snapshot.mode`
  stays `off` and nothing changes.
- `snapshot.mode` is a **cluster setting** (phase 13 configuration, with a contract field):
  `off` (default), `on`. The switch is a task: it checks that every instance has the library
  preloaded, `track_commit_timestamp = on` effective (after the restart, standbys first), the
  extension in every database that has distributed tables, no `kingres_shard` version older than 0.11
  and a clock offset below half the guard; it then sets the mode in the catalog (map group) and the
  nodes pick it up with the catalog push. A switch back to `off` is allowed at any time (a
  statement that is running finishes at its `S`).
- **Mixed versions during a rolling upgrade**: a node that is not ready never receives `S`
  because `snapshot.mode` is off cluster-wide until all are ready. Before the switch the
  `kingres_shard` 0.11 code is dormant: the `dtx_log.commit_ts` column is null, the prepare batch
  does not ask for a timestamp. After the switch, a node that has the old 0.10 code and was
  missed is refused by `check_cluster` and by the switch task (the version check exists for
  extension versions, limitations.md "Nodes must agree"). Rolling back to 0.1.0 binaries after the
  mode was on is supported only by switching the mode off first, running until the horizon passed,
  and downgrading; the extension keeps its clock row.
- **Upgrade ordering of the objects**: `kingres_shard` 0.11 (new objects only; 0.9 and older scripts
  stay identical to tag `kingres-0.1.0-pg18`, 0.10 belongs to the phase 15 lines and is released
  before this work merges); `kingres_snapshot` 0.1 new; `kingres_ledger` 0.4 for the optional cut.
- The REST contract takes the next free revision at implementation time (0.35.0 is on the phase
  15 branch).

### 8.3 Cost on upgrade

`track_commit_timestamp = on` needs a restart of every instance and adds 10 bytes of SLRU per
transaction; the preload list gains one library. The packaging and the install guide state both.

## 9. Interfaces

### 9.1 SQL

`kingres_snapshot.hlc_now()`, `hlc_observe(timestamptz)`, `last_commit_ts()`,
`current_snapshot_ts()`, settings `kingres_snapshot.ts`, `commit_ts` (internal), `min_ts`,
`max_forward_drift`, `horizon`, `sample_interval`, `limit_interval`, `prepared_wait_timeout`,
`recency`. A view `kingres_snapshot.status` (clock, limit, floor, window, counters). `kingres_shard`:
`kingres.check_cluster()` gains kinds `snapshot_not_ready`, `clock_skew`, `dtx_commit_ts_missing`.

### 9.2 REST, console, `kingres-cli`

**[Recommendation]** One contract revision for the feature (a placeholder number, taken at
implementation time), with the generator and the console types updated in the same change, as the
earlier phases did:

| REST operation (new) | Purpose | `kingres-cli` command |
|---|---|---|
| `getSnapshotStatus` | Per server: library state, clock value, clock offset against the leader, limit, horizon floor age, counters (snapshots taken, waits, `KS001` by cause) | `kingres-cli snapshot status` |
| `setSnapshotMode` (task) | Switch `snapshot.mode`; the task checks section 8.2 and reports what blocks | `kingres-cli snapshot enable`, `snapshot disable` |
| Configuration entries (existing `getConfiguration` style) | `snapshot.mode`, `maxForwardDrift`, `horizon`; `track_commit_timestamp` as a restart-needed setting with a floor | `kingres-cli config get/set` (exists or is added with the CLI line) |
| Alerts (new kinds) | `clockSkew` (warning at half the guard, critical at the guard), `snapshotHorizonTooShort` (rate of `KS001` for the horizon), `preparedWaitLong` | `kingres-cli alerts` |
| Metrics | `kingres_snapshot_wait_seconds`, `kingres_snapshot_ks001_total{cause}`, `kingres_clock_offset_seconds` | `kingres-cli metrics` |

The console: a "Consistency" card on the Dashboard and a tab on the Configuration page; the
Ledger page shows "consistent cut" for an aggregate that has one (ledger document, section 6).
**Rule since 2026-11-28:** every REST change gets the matching `kingres-cli` command in the same
change; if the CLI line has not merged, the change's report lists the commands needed.

## 10. Performance and how it is measured

Costs (each **[Needs verification]** by measurement; there is no figure from a source for this
design):

| Cost | Where | Expectation |
|---|---|---|
| Commit hook and clock CAS | every commit | tens of nanoseconds, no lock; measured by `pgbench -N` and `-S` with the extension against without, 1 to 64 clients, and by the commit rate of `pg_test_fsync`-bound workloads |
| `track_commit_timestamp` | every commit | one SLRU write of 10 bytes; measured as above (upstream measured it as small, **[background]**) |
| Snapshot rewrite | each cross-group statement, each group | steps 2 and 4 of 5.3; measured as a function of window length (commits per second times the age of `S` at the group), 1 to 10 ms at 1 000 to 50 000 commits per second |
| Prepared wait | a reader meeting an in-doubt transaction | measured by a test that holds a prepared transaction and reads at `S >= p` |
| 2PC latency | distributed write | no extra round trip (timestamp rides the prepare batch and the commit batch); measured with the existing 2PC benchmark |
| Horizon keeper | bloat of up to `horizon` seconds of dead rows; CPU of 50 snapshots per second | measured by `pgbench -N` with the keeper on, table size after one hour |
| Clock limit worker | one tiny WAL-logged update per second | negligible |

Benchmarks live under `kingres/dev/` and run on a three-group cluster (the standard test cluster),
with the numbers recorded in the work log. The acceptance rule of the spike: on a single node,
commit throughput within 3 percent of the unpatched server for `pgbench -N`; a cross-group
`SELECT` at 3 ms window within 20 percent of the same statement with `snapshot.mode = off` at the
same concurrency. (The two percentages are the author's proposal for the owner to confirm, not
measured values.)

## 11. Increments and order

Sizes are relative (S, M, L), not time.

| # | Increment | Size | Depends on | Touches `kingres_shard`? |
|---|---|---|---|---|
| P8-S | **Spike and decision gate**: KP-0005 to KP-0007 on PG18 and PG19, a throwaway `kingres_snapshot` that rewrites a snapshot at `S` on one node; regression tests for visibility, subtransactions, prepared waits, vacuum with an old `S`; the benchmarks of section 10 | M | none | no |
| P8-1 | Core patches in the register, with their tests and `pg_compat.h` path; upstream proposal text | S | P8-S | no |
| P8-2 | `kingres_snapshot` 0.1: clock, limit worker, hooks, horizon keeper, SQL, status view, TAP tests on one node and on a primary with a standby (promotion keeps the clock) | L | P8-1 | no |
| P8-3 | Test harness: Jepsen-style history checker for snapshot isolation across groups (Go, in `internal/clustertest`), nemeses (kill, partition, clock skew by the fault hook, 2PC crash points), fed by a bank and a list-append workload | L | P8-S (can start before P8-2 with a fake) | no |
| P8-4 | `kingresd`: preload and `track_commit_timestamp` settings, `snapshot.mode` task, alerts, metrics, REST, console, `kingres-cli`, docs (operator guide, upgrade, limitations) | M | P8-2 | no |
| P8-5 | `kingres_shard` 0.11: statement and transaction `S` in `node_conn.c`, `SET LOCAL` prefix in every sender, 2PC timestamps, `dtx_log.commit_ts`, recovery uses `C`, `check_cluster` kinds | L | P8-2, and the merge of the phase 15 and joins lines (they change the same files) | **yes**, last and small per file |
| P8-6 | Bucket move and split interplay (`switch_ts`), `REPEATABLE READ` handling of a bucket switch | M | P8-5 | yes (`bucket_move.c`, SQL 0.11) |
| P8-7 | Failover interplay: clock restart after promotion, prepared transactions after a crash, tests at every crash point of phase5-part2's list | M | P8-2, P8-5 | small |
| P8-8 | Ledger consistent cut (ledger document, increment) | M | P8-5 | `kingres_ledger`, `kingresd` |
| P8-9 | Hardening: review rounds, performance tuning of the window read, optional `recency = bounded` | M | all | |

**What can run beside other work in `kingres_shard`:** P8-S, P8-1, P8-2, P8-3, P8-4 (they touch no
file of `kingres_shard`; P8-4 touches `kingresd`, the contract, the console and the CLI, which the
CLI and phase 15 lines also touch in small, separate places: keep edits local). P8-5, P8-6 and P8-7
must wait for phase 15 rest and the join work to merge (same files `node_conn.c`, `recovery.c`,
`bucket_move.c`, SQL 0.10 pieces), and are written as a series of small commits on top.

**Decision gates.**

1. After P8-S: continue with option 2 only if (a) the core change is under about 300 changed
   lines in the three patches and changes no visibility function, (b) PG19 needs only the rename of
   1.1 item 6, (c) the benchmark thresholds hold. Otherwise fall back, with a new ADR, to option 6
   (atomic visibility with no core change), or to a larger core patch (option 1) only if the owner
   accepts the rebase cost.
2. After P8-3 has a first red run on a deliberately broken build (a build with the wait rule
   removed must fail the checker): the checker is trusted only after it catches a known bug.

## 12. Test plan

1. **Unit and regression (single node).** Visibility at `S` for: committed before, after, equal to
   `S`; in progress; aborted; subtransactions committed and aborted; prepared with `p < S`, `p > S`,
   decided commit and rollback; frozen tuples (freeze after `S` with the keeper floor); heap pruning;
   concurrent update conflicts; sequences, `TRUNCATE`, `CLUSTER`, `VACUUM FULL` interplay; PG18 and PG19
   with the same expected files.
2. **Clock tests with a fake clock.** Monotone after a backwards step; the guard; the limit
   (never above it; stall when the worker is late; restart takes the maximum; promotion with a
   lagging standby).
3. **Jepsen-style anomaly checks** (P8-3), on the three-group cluster of `internal/clustertest`:
   - *Bank*: accounts spread over groups, transfers by 2PC, readers sum all accounts at one `S`; the
     total must be constant (anomalies A and C); transfers within and across groups.
   - *Causal pair*: a writer commits W1 at group A, tells a second client, which commits W2 at B; a
     reader that sees W2 must see W1 (anomaly B), repeated with the token of 6.4 and without.
   - *List-append* history checked for snapshot isolation by an Elle-like cycle detector (a
     generalised-isolation checker written in Go; **[Needs verification]** whether to port the
     published algorithm or call it as a tool; Elle is Clojure) with the expected result "G-single and
     G1 absent, G2 allowed".
   - *Negative controls*: with `snapshot.mode = off` the bank test must fail (the checker works);
     with the wait rule disabled by a debug setting it must fail.
4. **Crash points.** Every fault point of 2PC that exists (`kingres_shard_fault_point`:
   `after_first_prepare`, `after_prepare`, `after_log`, and those of recovery) crossed with a reader
   at `S` before, between and after: the reader sees D completely or not at all and the wait ends.
   Kill the originator, a participant, the primary of a group (failover), with and without
   synchronous replication.
5. **Clock skew injection.** The fault hook of `kingresd` steps a node's wall clock; add a mode for
   the extension that offsets the physical part of the clock (a setting that exists only in the
   test build) by +/- 100 ms, 400 ms, 600 ms: correctness checks pass at all; recency measured;
   at 600 ms the guard refuses and the alert is raised.
6. **Upgrade tests.** 0.1.0 cluster with data upgraded to the new binary and extension, `snapshot.mode`
   switched on under a running bank workload, and off again.
7. **Performance.** Section 10, run before and after each increment from P8-2 on, recorded in
   the work log.
8. **Fault of the fallback.** Every statement of the existing cross-group tests is run with
   `snapshot.mode = on` and compared with its result at `off`; pushdown paths compared with the
   complete scan, as ADR 0022 requires.

## 13. Security review points

1. **Who may set a snapshot timestamp.** `kingres_snapshot.ts`, `commit_ts` and `min_ts` are
   settings. A client could use an old `ts` to read a past state (a time-travel read the role may not
   be allowed: an old snapshot only shows rows the role could read then, but the role may have been
   revoked since) or a far-future one (refused by the guard) or a forged `commit_ts` (it would make a
   transaction commit at a chosen time: it breaks snapshot consistency for others). All three are
   accepted **only from a node session** that proved its identity (`forward_auth.c`, the same
   mechanism as `forward_term`); a plain client cannot set `ts` or `commit_ts`; `min_ts` is allowed
   for any role and is clamped by the guard.
2. **A compromised node** can give a wrong timestamp to a statement it originates (it is the
   coordinator of that statement). It cannot rewrite another node's past: a timestamp is bounded by
   the guard and the horizon. "A compromised leader is a compromised cluster" (control-plane README)
   already holds; the new assumption is the same one.
3. **Information disclosure through waits:** a reader learns that a prepared transaction exists
   (timing). The existing `pg_prepared_xacts` view discloses more; no new exposure beyond the
   cluster's own roles.
4. **Denial of service:** a prepared transaction held for long delays every reader above its `p`
   (bounded by `prepared_wait_timeout`); an old `S` costs window reads (bounded by the horizon); the
   horizon keeper bloat is bounded. `kingresd` alerts on prepared transactions older than the
   retention (exists in part: `dtx_incident`).
5. **Horizon floor and vacuum:** an error in KP-0007 can let vacuum remove a tuple a snapshot needs
   (silent wrong result). The tests of 12.1 include a vacuum running while `S` is at the edge of the
   horizon, and the spike has an independent review before the core patches merge.
6. **Core patches and the PostgreSQL licence:** the patches are to PostgreSQL files and keep their
   licence, and carry the project's attribution in the register, as KP-0001 to KP-0004 do. No Citus
   or Shardman code is read or copied (ADR 0004; the Shardman and Postgres Pro patches are
   PostgreSQL-licensed, but the design here is derived from the papers and the PostgreSQL
   source only).
7. **Audit.** `setSnapshotMode` and the setting changes go through the existing audit log.

## 14. Open questions only the owner can decide

Each has a recommended default; none blocks the spike.

1. **Accept a core patch series (KP-0005 to KP-0007)** for the first time in the project (ADR 0001
   allows "a small number of core patches"). Default: yes, behind the spike's gate; fall back to
   option 6 if the gate fails.
2. **Guarantee level to state:** snapshot isolation across groups with a skew-bounded staleness
   (default), or add the optional bounded-recency mode in the first release. Default: SI, with
   the bounded mode in P8-9.
3. **`snapshot.mode` default after the feature ships:** `off` for the first release, `on` for new
   clusters one release later. Default: `off` first.
4. **Horizon default (10 s)** and bloat trade-off. Default 10 s; configurable.
5. **Whether `SERIALIZABLE` across groups is wanted** (it would need SSI across groups, a research
   project). Default: no; documented as per node.
6. **Reads on standbys at a timestamp.** Default: later, only on request.
7. **Name and release train:** one release with phase 8 part 1 and part 2, or part 1 alone. Default:
   part 1 first (the ledger cut needs it).

## 15. Sources

Fetched or searched on 2026-11-28: commitfest entry 4912 and the linked thread; the CockroachDB
HLC package page, the Cockroach Labs clock blog and SIGMOD 2022 summary, Jepsen's CockroachDB
analyses; the YugabyteDB read restart page; the HLC paper PDF. Cited from background and to be
checked before an ADR that relies on them is accepted: the Spanner OSDI 2012 paper; the Clock-SI
SRDS 2013 paper; the TiDB timestamp oracle documentation; the PostgreSQL 17 release notes (removal of
`old_snapshot_threshold`); the upstream measurements of `track_commit_timestamp`; Elle (Kingsbury and
Alvaro, "Elle: Inferring Isolation Anomalies from Experimental Observations", VLDB 2020).
