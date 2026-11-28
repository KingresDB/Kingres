<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
# Phase 8, part 1: a global snapshot across shard groups (design)

Status: Proposed design, 2026-11-28, revised the same day after two independent reviews (sections 16 and 16.1);
not implemented. Nothing here is built. Where this document says "exists" it describes the
repository at `kingres-18` (d1344c1aa9) and names the file.

Builds on [shard-phase2a.md](shard-phase2a.md) (2PC, the distributed scan),
[shard-phase2b.md](shard-phase2b.md), [shard-phase4.md](shard-phase4.md) (bucket moves),
[ha-phase5.md](ha-phase5.md) and [ha-phase5-part2.md](ha-phase5-part2.md) (failover),
[ledger-phase7.md](ledger-phase7.md) (aggregate digest), [ADR 0009](../adr/0009-two-phase-commit-no-global-snapshot.md),
[ADR 0022](../adr/0022-the-complete-scan-stays.md), [ADR 0023](../adr/0023-one-extension-source-for-several-majors.md).
The second half of phase 8 is [phase8-multinode-ledger.md](phase8-multinode-ledger.md).
The decisions are the proposed ADRs 0030 to 0034 ([../adr/README.md](../adr/README.md)).

On 2026-11-28 the project owner decided to build phase 8 (it had been kept out of 0.1.0 as
"future roadmap"). This document is the design that comes first.

Kinds of statements: **[Fact]** (checked in the repository, with `file:function`, or in a cited
source), **[Recommendation]**, **[Needs verification]**. "PG18" is this repository's tree
(PostgreSQL 18.7, `src/backend/...`); "PG19" is the tree of branch `kingres-19` (PostgreSQL 19.0).
A source marked "background" was not re-fetched on 2026-11-28.

## 1. What exists, and what the problem is

### 1.1 Facts about KingresDB's code (read on 2026-11-28)

1. **[Fact]** A transaction that wrote on two or more nodes uses 2PC (`contrib/kingres_shard/src/node_conn.c`,
   header comment, `precommit()`, `postcommit()`): `PREPARE TRANSACTION '<gid>'` on every remote
   writer; then one row per prepared remote in the origin's `kingres.dtx_log`, written by the
   origin's own local transaction (the row is the commit decision); then `COMMIT PREPARED`
   after the local commit. The origin always has an xid (the `dtx_log` insert assigns it), so it
   is itself a participant whose local part is never `PREPARE`d. `recovery.c` resolves prepared
   transactions the node originated every `recovery_interval`; `kingres.dtx_incident` records the
   cases asynchronous replication can make wrong.
2. **[Fact]** Each remote session starts its transaction with the local isolation level
   (`kingres_shard_begin_xact`, `node_conn.c`: `BEGIN ISOLATION LEVEL ...; <settings>; <time zone>;
   <dtx label>; <term claim>`). A statement that spans groups therefore reads every group at that
   group's own snapshot: one per statement per group in `READ COMMITTED`, one per transaction per
   group (taken when the group is first used) in `REPEATABLE READ`. Nothing relates them.
3. **[Fact]** Remote sessions prove they are node sessions (`term.c`, `forward_auth.c`) and go to
   the primary of the group.
4. **[Fact]** KingresDB patches no server code: [../dev/core-patches.md](../dev/core-patches.md) has
   KP-0001 to KP-0004 (a version, a README section, a build line, the licence file).
5. **[Fact]** `track_commit_timestamp` is not set by `kingresd` or used by any extension (no match in
   `contrib/kingres_*` or `kingres/control-plane`).
6. **[Fact]** `kingresd` uses a monotonic clock for every decision and never sends a wall-clock
   time between servers (`kingres/control-plane/internal/clock/clock.go`). The hybrid clock of this
   design is data-path time and must never be used by lease or failover code.
7. **[Fact]** The row-moving distributed scan answers every query (ADR 0022). Every path of
   `kingres_shard` must run at the statement's snapshot, or fail with the existing retry error.
8. **[Fact]** The ledger block builder orders by transaction id (`contrib/kingres_ledger/README.md`)
   and phase 7 says the aggregate does not prove that the heads were read at one instant
   (ledger-phase7.md 5.6).

### 1.2 Facts about PostgreSQL, verified in the two trees

Each item was read in both trees on 2026-11-28; differences are stated.

1. **[Fact] Commit timestamp and commit record.** `RecordTransactionCommit` (`access/transam/xact.c`)
   writes the commit record with `GetCurrentTransactionStopTimestamp()` inside a critical section,
   then sets `TransactionTreeSetCommitTsData(xid, children, ts, origin)` from
   `replorigin_session_origin_timestamp` (PG18) / `replorigin_xact_state.origin_timestamp` (PG19), or
   from the stop timestamp when no origin is set; this is before `XLogFlush`, before the clog update
   (`TransactionIdCommitTree`) and before `ProcArrayEndTransaction` (called from `CommitTransaction`,
   `xact.c`). A standby sets the timestamp from the commit record at replay (`xact_redo_commit`,
   `xact.c`).
2. **[Fact] Synchronous replication wait.** The wait (`SyncRepWaitForLSN` in `RecordTransactionCommit`)
   is after the clog update and before `ProcArrayEndTransaction`; the comment says the transaction
   "still show[s] as running in the procarray". A committing transaction thus stays in every
   snapshot's `xip` for the length of the wait. Same in `RecordTransactionCommitPrepared`
   (`twophase.c`).
3. **[Fact] Commit of a prepared transaction.** `RecordTransactionCommitPrepared` (`twophase.c`) takes
   `committs = GetCurrentTimestamp()`: at the top of the function in PG18 (before the critical
   section), and in PG19 inside the critical section after `DELAY_CHKPT_IN_COMMIT` is set and a
   `pg_write_barrier()` (PG18 uses `DELAY_CHKPT_START`). The code differs per branch; the hook point
   is not the same line.
4. **[Fact] Prepare.** `PrepareTransaction` (`xact.c`) takes `prepared_at = GetCurrentTimestamp()` and
   passes it to `MarkAsPreparing` (`twophase.c`; PG19 takes a `FullTransactionId`, PG18 a
   `TransactionId`); `gxact->prepared_at` is written to the two-phase state file (`hdr.prepared_at`,
   `StartPrepare`) and shown as `pg_prepared_xacts.prepared`. `MarkAsPrepared` (`twophase.c`) adds the
   dummy PGPROC to the procarray (`ProcArrayAdd`), after which the xid is shown running through the
   dummy proc.
5. **[Fact] No snapshot hook.** `GetTransactionSnapshot` and `GetLatestSnapshot` (`utils/time/snapmgr.c`)
   call `GetSnapshotData(&CurrentSnapshotData)` / `GetSnapshotData(&SecondarySnapshotData)`, static
   structs whose `xip`/`subxip` arrays are `malloc`ed once with `GetMaxSnapshotXidCount()` /
   `GetMaxSnapshotSubxidCount()` entries (`procarray.c`). In transaction-snapshot mode the first
   snapshot is `CopySnapshot`ed (`CopySnapshot` allocates exactly `xcnt` and `subxcnt` entries, so
   any array pointer is acceptable). There is no hook in `snapmgr.h` or `procarray.h`. PG19 has the
   same structure (line numbers differ).
6. **[Fact] Snapshot reuse.** `GetSnapshotData` (`procarray.c`) first calls `GetSnapshotDataReuse`:
   if `snapshot->snapXactCompletionCount` equals `TransamVariables->xactCompletionCount` it returns
   the snapshot struct as it is, without rebuilding. A function that edited `CurrentSnapshotData` in
   place would leave the edited content for the next ordinary statement that meets the reuse path.
   Same in PG19.
7. **[Fact] xmin protection.** `GetSnapshotData` advertises `MyProc->xmin`; `SnapshotResetXmin`
   (`snapmgr.c`) only moves it forward and takes the minimum of the registered snapshots, so a
   registered snapshot with a lowered `xmin` keeps it. `ProcArrayInstallRestoredXmin(xmin, proc)`
   (`procarray.c`, declared in `storage/procarray.h` in both trees, exported) installs a given `xmin`
   into `MyProc->xmin` under `ProcArrayLock` exclusive, if `proc` runs in the same database and its own
   advertised `xmin` is not later than the one installed. It is used for parallel workers and can be
   called from an extension with the PGPROC of a background worker that holds an old snapshot.
   **Consequence: no core patch is needed to advertise a lower xmin**; the earlier design's KP-0007 is
   dropped. The keeper must therefore run per database (the function requires the same database).
8. **[Fact] pg_subtrans and commit_ts retention.** `TruncateSUBTRANS(GetOldestTransactionIdConsideredRunning())`
   is called at checkpoints (`xlog.c`); `ComputeXidHorizons` (`procarray.c`) includes every backend's
   advertised `xmin` and `xid` in `oldest_considered_running`, so `pg_subtrans` is retained back to
   any advertised `xmin`. The commit timestamp SLRU is truncated by vacuum (`vac_truncate_clog`,
   `commands/vacuum.c`: `AdvanceOldestCommitTsXid(frozenXID)`, `TruncateCommitTs(frozenXID)`), and
   `TransactionIdGetCommitTsData` (`commit_ts.c`) returns "no value" for an xid below
   `oldestCommitTsXid`, for a frozen/bootstrap xid, and for a transaction without a recorded value
   (aborted, or committed while the setting was off); it errors when the module is disabled.
9. **[Fact] Sub-transactions.** `TransactionTreeSetCommitTsData` (`commit_ts.c`) records the same
   timestamp for the top-level xid and every committed subxid. `XidInMVCCSnapshot` (`snapmgr.c`)
   with `suboverflowed` set maps a sub-xid to its top-level xid through `SubTransGetTopmostTransaction`
   and then searches `xip` only.
10. **[Fact] Wait primitive.** `XactLockTableWait` (`storage/lmgr/lmgr.c`) waits for an xid to finish,
    including the xid of a prepared transaction (its dummy proc holds the lock).

### 1.3 The anomaly, precisely

Documented today ([../guide/limitations.md](../guide/limitations.md), "Consistency"). Three things
hide under one sentence:

- **A. Atomic visibility.** Distributed transaction D commits on groups A and B; a reader sees D at A
  and not at B.
- **B. Cut consistency.** W1 commits at A, W2 at B later; a reader sees W2 and not W1 although W1
  happened first. No distributed transaction is involved. Whether "later" is by real time or by
  causality matters (section 6).
- **C. One statement, two states.** One statement that scans two distributed tables, or the same group
  twice, sees the group in two states in `READ COMMITTED`.

## 2. Goals and non-goals

Goals:

1. A statement (READ COMMITTED) or transaction (REPEATABLE READ) that spans groups reads **one
   consistent cut**: A and C disappear; B disappears for transactions that are causally linked
   through a carried timestamp, and is bounded by clock skew for the rest (6.3).
2. Read-write distributed transactions use the same cut, with first-committer-wins conflict
   detection per group as PostgreSQL does per node.
3. Single-group statements pay almost nothing (measured, section 10).
4. Thin fork: two small hooks, written so they could be offered upstream; no visibility function
   changed; PG18 and PG19 carry their own patch of the same shape (4.4).
5. Opt-in per cluster, with an upgrade path from 0.1.0 and a defined mixed-version behaviour.

Non-goals: strict serializability or external consistency; cross-group constraints; reads on
standbys at a timestamp (first release reads primaries only); lowering 2PC cost; changing the
complete scan (ADR 0022).

## 3. Research: the options

### 3.1 What real systems do (sources)

| System | Mechanism | Cost | Source |
|---|---|---|---|
| Google Spanner | TrueTime bounded clocks; commit wait; lock-free reads at a timestamp | GPS and atomic clocks; commit latency of the error bound | Corbett et al., OSDI 2012 (background) |
| CockroachDB | Hybrid logical clock per node; a read that meets a value inside its **uncertainty interval** (reader timestamp to timestamp plus `max_offset`, default 500 ms) restarts at a later timestamp; a node whose offset exceeds the limit is meant to stop | Restarts; bounded offset; not strictly serializable (Jepsen) | [pkg.go.dev hlc](https://pkg.go.dev/github.com/cockroachdb/cockroachdb-parser@v0.25.2/pkg/util/hlc), [Cockroach Labs blog](https://cockroachlabs.com/blog/living-without-atomic-clocks/), [Jepsen](https://jepsen.io/analyses/cockroachdb-beta-20160829), [SIGMOD 2022 summary](https://muratbuffalo.blogspot.com/2022/06/enabling-next-generation-of-multi.html) (secondhand) |
| YugabyteDB | Hybrid time; "read restart" error for a row committed inside the skew window (`max_clock_skew_usec`, 500 ms); `DEFERRABLE` waits out the skew | Restart errors the application must retry in explicit transactions | [YugabyteDB docs](https://docs.yugabyte.com/stable/architecture/transactions/read-restart-error/) |
| Clock-SI | Snapshot isolation with loosely synchronised clocks; a read **that meets** a prepared version with prepare time below the snapshot waits for the decision; commit time is the maximum of the prepare times | Staleness and delay bounded by skew | Du, Elnikety, Zwaenepoel, SRDS 2013 (background) |
| Hybrid logical clocks | A clock that never goes back, within a bound of physical time, carries causality through messages | One 64-bit value | Kulkarni et al., 2014 ([PDF](https://www.cse.buffalo.edu//~demirbas/publications/hlc.pdf)) |
| Postgres-XL | Central GTM | Single point of failure and a round trip; fell behind upstream | architecture.md, [Postgres-XL](https://en.wikipedia.org/wiki/Postgres-XL) |
| TiDB (timestamp oracle) | Central oracle | Same, mitigated by batching and replication | TiDB documentation (background) |
| Citus | None (atomic, durable, consistent, no distributed snapshot) | Documented anomalies | [Citus README](https://raw.githubusercontent.com/citusdata/citus/main/src/backend/distributed/README.md) |
| Postgres Pro Shardman | CSN snapshots over a patched PostgreSQL | A large core patch | [Shardman architecture](https://postgrespro.com/docs/shardman/17/tutorial-arch), [thread](https://postgrespro.ru/list/thread-id/2385436) |

### 3.2 State of the PostgreSQL core

**[Fact, searched 2026-11-28]** No CSN or Clock-SI patch is in PostgreSQL. The last CSN activity
found is Heikki Linnakangas' "CSN snapshots in hot standby" (commitfest
[4912](https://commitfest.postgresql.org/50/4912), last change 2024-12): a different, smaller use,
unmerged when last seen. In that thread Andrey Borodin noted that a distributed system would need
the CSN from another source and that hooks might be needed
([thread](https://www.postgresql.org/message-id/08da26cc-95ef-4c0e-9573-8b930f80ce27@iki.fi)).
**[Needs verification]** whether anything newer exists; re-check before ADR 0030 is accepted.

### 3.3 The options for KingresDB

**Option 1: CSN in the core (Shardman style).** The most direct Clock-SI visibility; a large core
patch (a commit-sequence store, visibility, vacuum, standby). The deep fork architecture.md warns
against. Kept only as the last fallback.

**Option 2: HLC commit timestamps and a classic snapshot rebuilt from a timestamp (recommended).**
Commit timestamps come from a hybrid clock and live in PostgreSQL's commit timestamp store. A global
snapshot is a timestamp `S`; each group builds an ordinary `xmin/xmax/xip` snapshot that lists as
in progress every transaction that is running or committed with a timestamp above `S`. Visibility
functions are unchanged. Two small hooks (4). Correctness is the subject of the spike (11).

**Option 3: central oracle or GTM.** Single point of failure or an extra service, and the same
visibility work. Rejected.

**Option 4: TrueTime-style bounded clocks.** Needs hardware. Rejected; the wait idea is kept as the
bounded-recency mode (6.3).

**Option 5: restarts with uncertainty intervals.** Needs statement restart; inside `REPEATABLE READ`
only the client can retry. Rejected as the base, but its idea (a read that meets a value in the
window) is the reason for 6.3.

**Option 6: atomic visibility with no core change (fallback ladder, level 1).** Fixes A, and C only if
the statement's snapshots are agreed before any fragment runs; not B. It is not a "no new
mechanism" option: it needs (i) a durable per-participant record of recently committed
distributed transactions (one extra write per participant), (ii) an in-doubt wait as well (a group
whose snapshot saw D prepared must be re-taken after D commits, unbounded for a lost coordinator),
(iii) snapshot export and import so that one statement uses one snapshot per group, (iv) validation
before streaming fragments start, since a streaming result cannot be re-taken, and a pushed-down
partial aggregate cannot be repaired afterwards. ADR 0022 is respected (the complete scan runs at
the re-taken snapshots). It is a correct fallback for A and C only if all of that is built.

**Option 7: do nothing.** Rejected by the owner.

### 3.4 Option 2 against option 1

| | Option 1 (CSN) | Option 2 |
|---|---|---|
| Core change | Large | Two hooks; no visibility function; no snapshot-struct change |
| Cost of a snapshot at "now" | O(1) | O(running transactions + commits since the last sample), small |
| Old `S` | O(1) | Grows with commits since the sample at `S`; bounded by a horizon |
| Rebase per major | High | Low, but the patch differs per branch (4.4) |
| Risk | Known (Shardman) | The technique is new: the spike (11) |

## 4. The core change (two hooks, per branch)

**[Recommendation]**, each a core patch of the register (ADR 0032), with a test that fails without
it (rule 6 of core-patches.md).

### 4.1 KP-0005: timestamp hook for commit and prepare

`TimestampTz (*xact_timestamp_hook)(XactTimestampKind kind, TransactionId xid, TimestampTz proposed)`
with kinds `commit`, `commit_prepared`, `prepare`. Call sites, per branch:

- `RecordTransactionCommit` (`xact.c`): where the commit record's time is fixed. The returned value is
  used for the commit record, for `TransactionTreeSetCommitTsData` and as the origin timestamp, so
  the primary's commit timestamp and the standby's replayed one are the same value.
- `RecordTransactionCommitPrepared` (`twophase.c`): in place of `committs = GetCurrentTimestamp()`
  (PG18: top of the function; PG19: after the barrier, inside the critical section).
- `PrepareTransaction` (`xact.c`): in place of `prepared_at = GetCurrentTimestamp()`. This is not in a
  critical section.

The hook is called in a critical section for `commit`, and for `commit_prepared` **inside it in PG19
and outside it in PG18** (the line of `committs` differs, 1.2 item 3); the extension's code must be
valid in both contexts, so for both kinds: it may use atomics and `pg_memory_barrier()` and must not
allocate, lock, **wait** or fail, and the tests run on both majors. Any waiting the clock needs
(promotion, 5.6) happens **before** a transaction can reach the hook. Size: about 40 changed lines
per branch, excluding tests. **Not** a `pg_compat.h` matter: these are server files, so the two
patches differ (the origin timestamp name, `DELAY_CHKPT_*`, `MarkAsPreparing`'s argument) and are
carried on `kingres-18` and `kingres-19` as separate commits of the same register row; each
major's rebase test is in [../dev/upstream-sync.md](../dev/upstream-sync.md) (a line is added
there). `pg_compat.h` belongs to the extension, which must build against both patched trees.

### 4.2 KP-0006: snapshot hook in `snapmgr.c`

`Snapshot (*statement_snapshot_hook)(SnapshotUse use)`, called in
`GetTransactionSnapshot` (the three places that return `CurrentSnapshot`, except the serializable
branch) and `GetLatestSnapshot` (only for `use = statement`) **before** the caller's own
`GetSnapshotData` call. **The hook takes no snapshot from the caller (N1):** it must itself call
`observe(S)` and **then** `GetSnapshotData` into its own struct, because the correctness argument of
5.2 (I1, the Dekker order: observe, then snapshot, then slot scan) needs the snapshot to be taken
after `observe(S)`; a snapshot that the caller took earlier would miss a writer that started after it,
drew a timestamp `<= S` before `observe(S)` ran, and committed. The hook returns **`NULL`** when
the backend has no timestamp set (the caller then proceeds exactly as before), or **a snapshot that
the extension owns** (its own `SnapshotData` with its own growable arrays). Lifetime: the returned
snapshot is valid until the next call of the hook in the same backend, as `CurrentSnapshotData` is
valid until the next `GetSnapshotData`; callers already register or copy before doing anything else
(`GetTransactionSnapshot` comment), and `CopySnapshot` for the first `REPEATABLE READ` snapshot
copies it. The hook never edits `CurrentSnapshotData` or
`SecondarySnapshotData`: the reuse path of `GetSnapshotDataReuse` (1.2 item 6) keeps working on
unmodified structs, and a later plain statement is unaffected. For the first snapshot of a
`REPEATABLE READ` transaction the hook runs before `CopySnapshot`, which copies `xcnt` and
`subxcnt` entries from any array. `SnapshotUse` tells the hook which caller it is in, so that catalog
(`GetCatalogSnapshot`, a separate struct), historic, and non-MVCC snapshots never reach it. The hook
Parallel workers: the leader serialises the
active snapshot (`SerializeSnapshot` copies `xmin`, `xmax`, the arrays), so a parallel query under
a rewritten snapshot works; **[Needs verification]** by test. Size: about 30 lines in
`snapmgr.c` and `snapmgr.h` per branch.

### 4.3 What is not patched

No visibility function. No `procarray.c` change: advertising a lower `xmin` uses the existing
exported `ProcArrayInstallRestoredXmin` (1.2 item 7). No clog, commit_ts or subtrans change.
SERIALIZABLE (`GetSerializableTransactionSnapshot`) is not rewritten: a session under SERIALIZABLE
that sets a timestamp gets an error (per-node serializability only).

### 4.4 Size gate and the library check

**[Recommendation]** The spike's gate on size is: the non-test diff of KP-0005 and KP-0006 together is
under about 200 lines per branch, touching `xact.c`, `twophase.c`, `snapmgr.c`, `snapmgr.h` and one
new header, and no visibility function. (The earlier gate of 300 lines was for three patches; with
the placement moved to `snapmgr.c` and KP-0007 gone, the estimate is about 110 lines per branch;
the gate leaves room.) `kingres_snapshot` fails to load on an unpatched server (an undefined symbol
at load, which the library turns into a clear message in `_PG_init`); the install guide says that
the feature needs the patched server, which is what every KingresDB package contains.

### 4.5 Settings the groups need

`track_commit_timestamp = on` (restart; 10 bytes per transaction in `pg_commit_ts`); the preload
list gains `kingres_snapshot`. `kingresd` writes `shared_preload_libraries` itself and must also
set `track_commit_timestamp`, as a setting with a floor in the Configuration page, restart needed,
standbys first (the preload rule already stated for columnar). **Commit timestamps only exist from
the moment the setting is on**: transactions committed before have none (`TransactionIdGetCommitTsData`
returns no value, 1.2 item 8). `snapshot.mode` therefore cannot be switched on until the horizon (5.5)
has passed since the setting took effect everywhere; the task checks it.

## 5. The design

### 5.1 One new extension, `kingres_snapshot`

Everything that is not a hook lives in a new extension, `kingres_snapshot` (AGPL header, one source,
`src/pg_compat.h`, ADR 0023, built against both patched trees). Other lines of work change
`kingres_shard` now; `kingres_snapshot` does not depend on it and can be built and tested on one
node. `kingres_shard` integrates it later with a small change (11). The extension holds the clock,
the hook functions, the per-backend slots, the horizon keeper, and the SQL: `hlc_now()`,
`hlc_observe(ts)`, `last_commit_ts()`, `session_token()`, `prepare_ts()` (the value the last
`PREPARE` in this session drew), the session settings `kingres_snapshot.ts`, `commit_ts`, `min_ts`, and a
status view.

### 5.2 The clock, the slots, and the invariants

**The clock.** A hybrid logical clock with the precision of `TimestampTz` (microseconds since
2000-01-01), so the value stored in `pg_commit_ts`, commit records, `pg_xact_commit_timestamp()`,
logical replication origin timestamps and `pg_prepared_xacts.prepared` stays a readable timestamp.
`next()` returns `max(physical_now, last + 1)`; `observe(t)` sets `last = max(last, t)`; both are a
compare-and-swap on a 64-bit value in shared memory. **Visible side effects:** `recovery_target_time`
(PITR), `pg_xact_commit_timestamp()` and the commit record times show HLC values, up to the drift
guard ahead of wall time.

**The slots.** One slot per backend in shared memory: `(xid, bound)`, written only by its owner,
read by anyone.

**The protocol of a commit with no external timestamp** (the commit hook, in the critical section):

1. `slot.xid = xid; pg_memory_barrier(); slot.bound = clock.read();` (publish a lower bound `L`
   of the value that will be drawn: the clock never goes back, so the draw is `>= L`);
2. `pg_memory_barrier(); t = clock.next();` the draw; the hook returns `t`;
3. `slot.bound = t` (the exact value);
4. the commit proceeds (commit record, commit timestamp, clog, sync replication wait,
   `ProcArrayEndTransaction`); the slot is cleared after `ProcArrayEndTransaction` (an
   `XACT_EVENT_COMMIT` callback, which runs after it, `CommitTransaction`, `xact.c`), so a slot may
   outlive the xid's presence in the procarray by a moment, never the reverse.

**The protocol of a 2PC participant (remote, and the origin's own local part, C1):**

1. before the first `PREPARE` is sent, and for the origin before its local commit work starts, the
   participant draws its **prepare timestamp `p`** with the same publish-then-draw order: remote
   participants do it inside `PrepareTransaction` through the `prepare` hook (`slot.bound = L`, barrier,
   `p = clock.next()`; `p` becomes `prepared_at`); the origin does it by calling `prepare_ts()`
   through its own extension function before it sends the first `PREPARE`, and keeps the slot
   published. The origin may have no xid yet at that point (the `dtx_log` insert assigns it), so
   `prepare_ts()` first calls `GetTopTransactionId()` and only then publishes `(xid, bound)`;
2. the slot stays published for a remote participant until its prepared transaction is `valid` in the
   two-phase state (`MarkAsPrepared`); from then on `gxact->prepared_at` (durable in the state
   file) is the published bound, found by scanning the prepared transactions
   (`GetPreparedTransactionList`, `twophase.c`), which are not per-backend and survive the
   backend. The overlap has no gap: `MarkAsPrepared` sets `valid` before the backend's slot is
   cleared by its post-prepare callback (`XACT_EVENT_PREPARE`);
3. the originator collects every `p_i` and computes **`C = max(p_1, ..., p_k, p_local)`**, with no clock
   term. It raises the clock to `C` (`observe(C)`) before the local commit, and the local commit uses
   **exactly `C`** (the hook returns the backend's forced value; it never takes the max with the
   clock: the clock is raised to `C` first, never the reverse). The origin's slot stays published
   (bound `p_local`, then `C`) until the local xid leaves the procarray, so a reader at the origin
   with `S >= p_local` waits for the origin's own xid;
4. `COMMIT PREPARED` runs at a participant with `kingres_snapshot.commit_ts = C`; the
   `commit_prepared` hook returns `C`, and **before the commit becomes visible** it runs
   `observe(C)` unconditionally (it must also come before `ProcArrayEndTransaction`, the invariant I3).

**The reader** (the snapshot at `S`, 5.3), **in this order and inside the hook (N1)**: `observe(S)`;
barrier; **then** `GetSnapshotData` (taken by the hook itself, never by the caller before the
hook); barrier; scan the slots and the prepared transactions; wait for each running xid that might
commit at or below `S`.

**The token prefix (N2).** A session that holds a token (6.4) sends it in front of **every** group
transaction it opens, including plain single-group ones: the batch that begins the transaction on a
group calls `kingres_snapshot.hlc_observe(token)` first (one function call in the same batch, no
round trip; one CAS on the group's clock). A transaction that runs on the session's own node does
the same locally. Without it a single-group write at a group whose clock is behind would get a
timestamp below an earlier commit of the same session.

**Invariants** (each is tested by the differential test and the model of 11):

- **I1 (publish before draw).** Every timestamp a transaction can commit or prepare with is `>=` a
  bound it published in its slot before drawing. A reader that did `observe(S)` and then reads the
  slots either sees a bound `<= S` (and waits) or the writer's draw came after the observe and is
  `> S`. Needs the two barriers above (M9).
- **I2 (a prepared transaction's `p` is covered from the moment it is drawn to the decision).** The slot
  covers it until `MarkAsPrepared`; `prepared_at` covers it after. Hence `C >= p` and a reader with
  `S >= p` waits for the decision (C2).
- **I3 (observe before visible).** Any timestamp is observed by the clock of the node that commits it
  before `ProcArrayEndTransaction`, so the clock at any later read is `>=` every timestamp of a
  transaction that the read sees as finished. For a participant this is the `observe(C)` at `COMMIT PREPARED`
  (C3). For the origin, `observe(C)` before the local commit.
- **I4 (the origin is a participant).** The origin's local part follows the participant protocol (C1).
- **I5 (the clock never goes back, also across restarts and promotion)** (5.6), and **no transaction
  reaches the commit or prepare hook before the clock is ready** (the stall is outside the hook).
- **I6 (snapshot after observe).** The snapshot of a reader is taken by the hook after `observe(S)`,
  never before (N1).
- **I7 (token before write).** Every group transaction of a session that holds a token calls
  `observe(token)` before it can draw a timestamp (N2).

**Drift guard (H2).** A timestamp received at the **start of a statement** (`S`) or in a **prepare
answer** (`p_i`) that is more than `kingres_snapshot.max_forward_drift` (default 250 ms) above the
node's physical clock is refused with a retryable error and the alert `clockSkew`. The origin
checks every `p_i` before it writes the decision row and rolls back if one exceeds the guard. The
guard **never applies after the decision is durable**: `COMMIT PREPARED` with a logged `C` calls
`observe(C)` unconditionally, raises the alert if `C` is above the node's guard, and never refuses;
recovery uses the same call. A backward step of the system clock does nothing to the HLC and raises
the alert.

### 5.3 What a snapshot at `S` is, at one group (H1, C3, C4)

Inputs: `S` and the group's primary. Output: a PostgreSQL snapshot that sees exactly the
transactions committed with a timestamp `<= S`.

**State kept by the horizon keeper** (5.5), per database: a ring of **samples**. Sample `i`
holds: the snapshot `i` itself taken with `GetSnapshotData` (its `xmax_i = latestCompleted + 1` and its
running list `xip_i`), and **`h_i`, a clock reading taken after that call returned** (I3 gives:
every xid `< xmax_i` that is not in `xip_i` finished before the call, hence with a timestamp
`<= h_i`).

**Steps of the hook** (a statement with `kingres_snapshot.ts = S` set):

1. `observe(S)`; barrier; if `S` is older than the oldest sample, or no sample exists yet (after a
   restart or promotion until the first sample): error `KS001` ("older than the horizon").
2. `plain = GetSnapshotData(&own_struct)`, **taken by the hook itself, after step 1** (N1; `xmax_now`,
   `xip_now`). The reuse path of `GetSnapshotData` may return the cached content of `own_struct`
   only if no transaction with an xid completed since, which is safe.
3. **Wait rule (Clock-SI), against `plain`.** For every slot with `bound <= S` whose `xid` is in
   `xip_now`, and every valid prepared transaction with `prepared_at <= S` that is in `xip_now`:
   wait until it finishes (`XactLockTableWait`), then repeat from
   step 2 (it terminates: any transaction that publishes after the `observe(S)` draws above `S`, and
   the set of transactions with a bound `<= S` is finite and only shrinks; **L3**). The wait is
   bounded (`prepared_wait_timeout`, 5.9).
4. **Late commits.** Take the latest sample `i` with `h_i <= S`. Candidates are the xids in `xip_i` and
   the xids in `[xmax_i, xmax_now)`: **the number of candidates depends on the transactions that were
   running at the sample and the transactions started since it, never on the oldest running
   transaction (H1)**. For each candidate not in `xip_now` (finished), read its commit timestamp
   (`TransactionIdGetCommitTsData`): `> S` puts it in the rebuilt `xip`; no value for a candidate that
   clog says committed is an **error** (`KS001`, "no commit timestamp"), never "visible" (the rows
   below the horizon of commit timestamps, or before the setting was on); no value for an aborted one
   is ignored (aborted is invisible through clog). Candidates in `xip_now` (running or prepared) are
   in the rebuilt `xip` already.
5. The result is an extension-owned snapshot: `xmax = xmax_now`; `xip` = `xip_now` plus the late
   commits; `xmin` = the smallest xid in `xip`, or `xmax_now` when empty; `suboverflowed = true`
   (visibility goes through `pg_subtrans` for any sub-xid, 1.2 items 8 and 9); `curcid` from the
   plain snapshot; `snapXactCompletionCount = 0` (never reused); `copied = false`.
6. **Advertise `xmin`.** `ProcArrayInstallRestoredXmin(xmin, keeper_proc)` for the keeper of this
   database (1.2 item 7): it fails if the keeper's `xmin` is newer than the snapshot's (the horizon
   was passed): error `KS001`. **It takes `ProcArrayLock` exclusive (N5)**, the lock that
   `ProcArrayEndTransaction` and every commit take, so it is not called for every statement: the
   backend installs it **once per transaction** (per statement in `READ COMMITTED` only when the
   rebuilt `xmin` is older than the `xmin` the backend already advertises, which is the case only when
   the window has a late commit or a running transaction below the plain `xmin`), and the benchmark of
   10 includes a cross-group `SELECT` at high concurrency with concurrent commits.

Why the cut is a prefix of the timestamp order: visibility of `x` at `S` is "committed and timestamp
`<= S`". Transactions below `xmax_i` and outside `xip_i` have timestamps `<= h_i <= S` (I3). The
others are classified by their stored timestamp or by running state. A transaction with a timestamp
`<= S` that has not finished is caught by the slots (I1, I2) and waited for (step 3); one that
finishes after step 2 had a slot with a bound above `S` (I1) so its timestamp is above `S`.

**Cost.** The candidates are `|xip_i|` plus the transactions started since the sample, which is
`commit rate x (now - h_i)` plus the running list. For an `S` of about now at a group whose clock
is within a few milliseconds this is tens of ids; for `S` older by the sample interval (20 ms) at
20 000 commits per second it is about 400 reads of the commit timestamp SLRU. **[Needs
verification]** by the spike. Forced `suboverflowed` makes every tuple whose xmin lies at or above
the rebuilt `xmin` pay a `SubTransGetTopmostTransaction` lookup (M1); **[Needs verification]**
measured, with the SLRU hit rate.

### 5.4 Statements and transactions across groups

**Statement snapshot, `READ COMMITTED`.** The node that received the statement (any node, ADR 0010)
takes `S = max(clock.next(), session.min_ts)` at the start, sets it locally, and sends every fragment
with `SET LOCAL kingres_snapshot.ts = <S>;` in the same simple-query batch (no extra round trip).
Every sender of a fragment must do it: all go through `node_conn.c`; **[Needs verification]** the list
(`fragment_scan.c`, `remote_scan.c`, `dist_scan.c`, `batch_sender.c`, `row_router.c`, `copy_routed.c`,
`merge.c`, `repartition.c`). Functions that run in the statement call `GetTransactionSnapshot`, which in
`READ COMMITTED` takes a fresh snapshot; the hook rewrites it too while the session value is set
(5.8). Anomaly C is gone.

**Transaction snapshot, `REPEATABLE READ`.** `S` is taken at the first statement and kept. A group's
snapshot at `S` is built when the group is first used; if `S` is older than the horizon at that
moment, the statement fails with `KS001`. **Limit (M3):** a cross-group transaction cannot first
touch a new group more than the current hold (`horizon`, default 5 s; up to `horizon_max`, 30 s, while a cross-group RR transaction is registered, 5.5) after its start, and after a restart or a
promotion the ring is empty for one sample interval to one horizon, during which an `S` older than
the first sample is refused. Both are stated in limitations.md.

**A single-group transaction that never leaves its group** runs on a plain local snapshot with no
extension snapshot work (and pays the commit hook, I1). A `REPEATABLE READ` transaction under
`snapshot.mode = on` takes its first snapshot through the hook from the start, because it may reach
another group later (cost: steps 1 to 6 with a window of about the sample interval, plus the
prepared wait): the claim for single-group statements is **"almost nothing"**, measured for `READ
COMMITTED`, `REPEATABLE READ` and `pgbench -S`/`-N` (M2).

**One writer**: as today, no 2PC; the commit timestamp is drawn at commit (5.2).

**Two or more writers (2PC)**, in the protocol of phase2a section 1 with these additions (5.2 has
the order and the invariants):

1. Each remote participant: the batch is `PREPARE TRANSACTION '<gid>'; SELECT kingres_snapshot.prepare_ts();`:
   the hook draws and publishes `p` inside the prepare, the second statement returns it. No
   extra round trip. The origin drew `p_local` before sending (I4).
2. The origin computes `C`, checks every `p_i` against its guard (H2), writes the `dtx_log` row **with
   `C`** (new column `commit_ts`, `kingres_shard` 0.11, section 8), `observe(C)`, commits locally with
   exactly `C`.
3. After the local commit, `SET kingres_snapshot.commit_ts = C; COMMIT PREPARED '<gid>'` at each
   participant (one batch).
4. Recovery commits a prepared transaction with the `C` of its `dtx_log` row; with none it rolls
   back, as today. No participant can commit with another timestamp.

**Why it is correct (Clock-SI argument, with I1 to I4):** a reader at `S'` waits for every prepared
transaction with `p <= S'` at its group, and for the origin's local xid when `p_local <= S'`; so it
cannot build its snapshot while a transaction that might commit at or below `S'` is undecided. After the
decision the timestamp `C >= p` is final and the same on all groups. A reader with `S' < p_i` for every
`i` sees D nowhere (`C >= p_i > S'`); a reader with `S' >= C` sees D everywhere; a reader with some
`p_i <= S' < C` waits at that group, then finds `C > S'`, invisible; at the group where `p_j > S'`
nothing is needed. **[Needs verification]** by the model and the differential test of 11.

**Write-write conflicts.** Under `REPEATABLE READ` a concurrent update that committed after `S` is
invisible and `heap_update` reports `40001` as on one node, now across groups because every group
uses `S`; in `READ COMMITTED` `EvalPlanQual` follows the update chain as today. **[Needs
verification]** that nothing in `ExecUpdate` assumes the snapshot struct came from `GetSnapshotData`.

### 5.5 The horizon keeper

A background worker **per database that has the extension** (the same launcher pattern as
`recovery.c`; `ProcArrayInstallRestoredXmin` needs the same database):

- Every `kingres_snapshot.sample_interval` (default 20 ms) it takes `GetSnapshotData`, stores the
  snapshot's `xmax` and running list, then reads the clock (**after**, C3) and stores `h`. The ring
  keeps the **current hold** of seconds (below; the running lists are short on a healthy system
  and the ring has a size cap).
- **Adaptive and on demand (N7).** A worker per database that does nothing but hold an old `xmin`
  is a cost for every database even when no global statement exists, and the hold has a price: at
  30 s a hot row updated 5 000 times per second keeps about 150 000 dead versions in its HOT chain,
  which slows every access to that row. So: the keeper starts for a database only when a
  cross-group statement first asks for a snapshot there (a signal to the launcher, the same pattern
  as `recovery.c`; it needs a free `max_worker_processes` slot, which `kingresd` accounts for) and
  stops after `keeper_idle_stop` (default 5 minutes) without a request. Its **default hold is 5 s**
  (`kingres_snapshot.horizon`), enough for a statement or a short transaction; it is raised
  to `kingres_snapshot.horizon_max` (default 30 s) only while a cross-group `REPEATABLE READ`
  transaction is registered on the cluster (the originator tells each group at the transaction's first
  use there, and the hold shrinks back when it ends). A snapshot older than the current hold gets `KS001`.
  The first request after a start waits for one sample interval for a sample to exist. The hot-row test
  is part of 10.
- It keeps the oldest sample's snapshot registered, so its own `MyProc->xmin` is that snapshot's `xmin`;
  it registers a new oldest snapshot before it releases the old one, so the floor never has a gap.
  Vacuum, pruning and freezing cannot pass it (`ComputeXidHorizons` includes its `xmin`, 1.2 item 8);
  this protects tuple versions, commit timestamps (truncation follows frozen xids) and `pg_subtrans`.
- It is the reason a snapshot older than the horizon is refused. The cost is the cost of any
  transaction that holds an old `xmin`: bloat of up to `horizon` seconds of dead rows. This is the idea
  of `old_snapshot_threshold` (removed in PostgreSQL 17, background), with the bound enforced at the
  creation of a snapshot.
- A frozen tuple is visible to everyone; the floor prevents freezing of any transaction newer than
  the oldest sample's `xmin`.

### 5.6 Failover, restart and the clock (H4)

**[Recommendation]** The new primary must give timestamps above everything the old primary gave,
including snapshot timestamps handed out and never logged.

1. The clock has a persisted **limit**, carried **in WAL by a custom resource manager** (N3), not in
   a table: a table is not readable before the new primary is open, and the limit is per instance
   while the extension is per database. `kingres_snapshot` registers a resource manager in
   `_PG_init` (`RegisterCustomRmgr`, available to a preloaded library in both PG18 and PG19). A limit
   worker writes one small record every `limit_interval` (default 500 ms) with the value
   `max(clock, physical_now + max_forward_drift) + 2 * limit_interval` (no xid, no heap, no dead
   tuples: **L1** is thereby smaller, but with a synchronous standby down the record flush stalls like
   every commit, and the clock stalls when it would pass the limit). The redo function of the
   resource manager, on a standby, stores the replayed value in shared memory. Values handed out are
   `<=` the limit, because a received `S` is bounded by `now + max_forward_drift`. **Owner action:**
   a custom resource manager needs an ID; the project registered ID 255 for columnar on the PostgreSQL
   wiki ("Custom WAL Resource Managers"); this needs a **second ID requested the same way** before
   release (a development value from the experimental range may be used until then, as columnar did).
2. **Promotion and start in the primary role (N3, I5):** at the end of recovery, before the first
   write transaction can reach a hook, the extension computes the start value
   `max(physical_now, replayed limit, GetLatestCommitTsData() + 1)`. **If that is above `physical_now`,
   `clockReady` stays false until physical time reaches it**, a stall of at most
   `2 * limit_interval + max_forward_drift` (1.25 s by default, plus the replication lag of the
   limit record); other nodes would refuse a timestamp further ahead (the guard), so a jump is not
   allowed. **The stall is never inside the commit hook**: while `clockReady` is false a transaction
   that would take a timestamp fails **at its start** (the first call that assigns an xid or sets a
   snapshot timestamp, outside every critical section) with a retryable error, and the failover task
   keeps the promoted primary out of the router until `clockReady` and shows the stall
   (`clockStallMs`). With asynchronous replication the replayed limit may be older; the start value
   then also takes `physical_now + max_forward_drift`, an upper bound on anything the old primary can
   have handed out when its own clock was no more than the guard ahead of physical time. Every
   statement and commit path that bypasses the check is a mutant of the model of 11.
3. Prepared transactions after a crash keep `prepared_at` (durable in the state file, 1.2 item 4), so
   a reader after a restart knows `p`. A prepared transaction prepared before the extension was on has a
   wall-clock `prepared_at`, which is below any HLC value of the moment and makes readers wait for it, which
   is conservative.
4. A remote session at a failed primary is gone: a `REPEATABLE READ` transaction that used it fails, as
   today.

### 5.7 Bucket moves, splits, DDL (M4, L2)

- A bucket move copies current row versions by logical replication, then switches placement
  (shard-phase4.md). Older versions are not copied. A snapshot at `S` older than the switch of a
  bucket cannot be answered at the target. **Invariant:** the switch records its own commit timestamp
  `switch_ts` in the move's catalog row, and a fragment sent for that bucket with `S < switch_ts` fails
  with `KS001`; reads of the bucket at the target all start after `switch_ts`.
- The apply worker commits copied rows with the **source's** timestamp (origin timestamp, 1.2 item 1:
  an origin timestamp replaces the stop timestamp), which is a value of the source clock and not of the
  target's. The `commit` hook therefore calls `observe(origin_timestamp)` without refusing and
  alerts if it is beyond the guard. These timestamps can be below the target's horizon; that is
  harmless, because all reads of those rows begin after `switch_ts`.
- A split is handled like a move. DDL runs on every group in a transaction of its own; catalog snapshots
  are not rewritten, and PostgreSQL itself is not MVCC-safe for `TRUNCATE` and table rewrites: a read
  at an `S` older than a `TRUNCATE` or a rewriting `ALTER TABLE` can see an empty or rewritten table.
  Stated in limitations.md (L2). **[Needs verification]** that the existing placement-version `KS001`
  also covers a statement planned before a DDL.

### 5.8 What is not rewritten

Catalog snapshots (`GetCatalogSnapshot`, a separate struct), snapshots for logical decoding,
`SnapshotDirty`, `SnapshotSelf`, `CLUSTER`, `VACUUM`, index builds, background workers (the 2PC
recovery worker, the ledger block builder before its optional increment), and `GetLatestSnapshot` for
referential-integrity checks (it keeps its purpose, "now"). A function that calls
`GetTransactionSnapshot` in `READ COMMITTED` gets the rewritten snapshot while the session value is set;
under `REPEATABLE READ` it gets the transaction snapshot.

### 5.9 Waits, in-doubt transactions and availability (H3)

The wait of 5.3 step 3 is per snapshot, not per row, because visibility is not changed. Consequences
that are stated in limitations.md and measured:

- Every ordinary 2PC adds its remaining time between prepare and commit to every concurrent global
  statement at that group whose `S >= p`. A committing transaction waits in `xip` through the
  synchronous replication wait (1.2 item 2), so a **down synchronous standby** makes every global
  statement at that group with `S` after the commit began wait for the commit wait length; writers
  already hang in that case.
- **Ordinary commits in flight (N6).** In normal operation, every global statement waits at each group
  for every transaction that has already drawn a timestamp `<= S` and has not left the procarray: its
  WAL flush plus, with synchronous replication (the KingresDB default), the standby round trip, at
  **every group and every statement**, and again when step 2 of 5.3 is repeated. With `S` about now
  this is a few milliseconds on a healthy cluster and the replication latency on a loaded one; the
  benchmark of 10 measures it with synchronous replication on. For a statement that does not need
  recency, `kingres_snapshot.recency = stale` takes `S = clock - stale_ms` (default off, `stale_ms`
  default 20), which skips the wait for commits that began in the last milliseconds; it is a session
  choice and is documented as the trade of recency for latency.
- **At the origin (review note on C1).** A reader at the origin with `S >= p_local` waits for the
  origin's own xid through the origin's **whole prepare phase** (every `PREPARE` round trip), not only
  through the commit. Deadlock: the wait is on an xid whose owner is waiting for network answers, not
  for a lock the reader holds; a reader that is itself a distributed writer holding locks at other
  groups is the one case to test (12 item 11) against the existing distributed deadlock detection.
- One **in-doubt** prepared transaction (lost coordinator) makes every global statement at that group with
  `S >= p` wait until it is decided. The wait is bounded by `prepared_wait_timeout` (default 5 s), after
  which the statement fails with `KS001`; this bounds time, **not success**.
- Mitigations that are part of the design: (a) when `snapshot.mode = on`, the 2PC recovery interval
  for in-doubt transactions is 1 s (the query is cheap when nothing is prepared); (b) a reader that
  waits longer than 100 ms nudges the recovery worker on its own node, and the node of the origin
  learns of the wait through the next recovery pass; (c) metrics and the alert `preparedWaitLong`;
  (d) the statement of availability: **global reads depend on prompt 2PC recovery**.
- A per-row wait (lazy, inside visibility) would remove most of this and is rejected because it changes
  visibility code; it stays as a possible later patch if measurements show the cost.

## 6. Guarantees, in precise words

### 6.1 With `snapshot.mode = on`

| Anomaly | After |
|---|---|
| A. Fractured read of a distributed transaction | Not possible: a read sees it on every group or on none |
| C. One statement, two states of a group | Not possible: one `S` per statement |
| Read skew in a `REPEATABLE READ` transaction that spans groups | Not possible: one `S` per transaction |
| B. Inverted order of W1 then W2 | Not possible **when W2's session carried W1's timestamp** (6.4); otherwise W2 can be visible without W1 for up to the clock skew (6.3) |
| Write skew across groups, and every other serialization anomaly of snapshot isolation | **Remains** |

### 6.2 What remains

1. The result is snapshot isolation across groups, not serializability; `SERIALIZABLE` stays per node.
2. **Causal order is guaranteed only through a carried timestamp (H5).** No mechanism of this design
   detects, at read time, a value committed in the window `(S, S + skew]` by a transaction that in real
   time or by data dependence preceded the statement; CockroachDB and YugabyteDB close it with
   uncertainty restarts and a wait, this design closes it only with 6.4 and the bounded mode of 6.3.
3. Recency is bounded by clock skew (6.3). Across pooled or load-balanced connections, a read on another
   node within the skew after a commit may miss it (M7); the router and `kingres-cli` carry the token
   per client connection, and the documentation says so in plain words.
4. A snapshot older than the horizon, older than a bucket switch, older than a `TRUNCATE` or rewrite,
   or whose group has no commit timestamps for it, fails with a retryable `KS001` or is wrong in the
   way PostgreSQL's own `TRUNCATE` is (5.7).
5. Reads on standbys are not covered.
6. Single-group statements keep a local snapshot: atomic per group, complete up to the start of the
   statement, a distributed transaction seen at that group whole or not; read-your-writes holds on one
   connection as today.
7. Availability of global reads depends on prompt 2PC recovery (5.9).

### 6.3 Clock skew, and the bounded-recency mode

Correct (never a state that is not a consistent cut) for any skew, with I1 to I5. Skew changes only
recency: if node X's clock is `e` ahead of Y's, a commit at X at wall time `t` gets about `t + e`, and
a statement started at Y at wall time `t + d` with `d < e`, in no carried relation, takes `S` near `t + d`
and misses it. **Two different bounds (N4).** The **guard** `max_forward_drift` (default 250 ms) is what a node
accepts from another node: a statement timestamp or a prepare answer further ahead of its own physical
clock is refused. The **assumed skew bound** `skew_epsilon` (default 100 ms, always smaller than the
guard minus the network time) is what the bounded mode assumes about the real offsets between nodes;
`kingresd` measures the offsets, alerts at half of `skew_epsilon` and marks a node whose offset is
above it `clockSkew` critical, and the mode is **correct only while the real skew is below
`skew_epsilon`**.

**Bounded-recency mode (`kingres_snapshot.recency = bounded`, built in the same increment as the
integration, P8-5, not later):** the statement takes `S = physical_now + skew_epsilon` at the
originator and waits there until physical time passes `S` (the mirror of Spanner's commit wait on the
read side and of YugabyteDB's `DEFERRABLE`), so `S` is above every timestamp of a commit that
completed before the statement began, provided the real skew is below `skew_epsilon`; every group
`observe`s `S`. Because `skew_epsilon` is below the guard, a receiver whose clock is not ahead of
the originator's by more than `guard - skew_epsilon` accepts `S`; with the first version of this
design (`S = physical_now + guard`) nearly every receiver would have refused it. The mode costs
`skew_epsilon` of latency per statement, so it is a per-session or per-statement choice, and it holds
every node's clock up to `skew_epsilon` ahead of physical time while it is in use, which the guard
margin already allows for. The default guard is **250 ms** (lowered from 500 ms); the alert is at
half.

### 6.4 Causality tokens

The session token is `max(last commit timestamp, last snapshot S, min_ts)`; it is carried
automatically within one session and by the router per client connection. The next statement takes
`S >= token`, and **every group transaction of the session, including a plain single-group write,
calls `observe(token)` first** (5.2, "The token prefix", I7), so that a transaction that **read** at
`S`, or a write after a commit at a node whose clock is ahead, commits above the token: W2 after
reading W1 gets a timestamp above W1's. The cost is one function call in the batch that opens the
transaction on a group and one CAS; it is part of the "almost nothing" measurement (10). Across sessions the client passes the token
(`SELECT kingres_snapshot.session_token()`, `SET kingres_snapshot.min_ts = ...`; `kingres-cli`
prints and accepts it). **Not available:** a token for "the maximum commit timestamp of the rows a
statement read" (visibility does not return it); the token is the snapshot `S`, an upper bound.

## 7. Failure handling

| Event | Behaviour |
|---|---|
| Origin lost after prepare, before its decision | No row: prepared transactions roll back at recovery (1 s pass in mode on); readers at `S >= p` wait up to `prepared_wait_timeout`, then `KS001` |
| Origin lost after its decision row (with `C`), before `COMMIT PREPARED` | Recovery commits with `C`, unconditionally observing it (H2); phase 2a's rules and `dtx_incident` as today |
| Origin failed over to an asynchronous standby that lacks the decision | As today (`dtx_incident`); a reader at `S >= C` may have seen D committed at a participant that is later told to roll back: the incident, now also visible as a snapshot that showed a transaction later undone. Not possible with synchronous replication |
| A participant fails over mid-commit | The prepared transaction is replayed with its `prepared_at`; recovery commits with `C` |
| Promotion of a group | Clock stall (5.6), shown on the task |
| System clock stepped | HLC unchanged; alert; the guard governs what is accepted |
| Node with a very wrong clock | Its statements are refused by the guard at others; `clockSkew` critical |
| Horizon, bucket switch, no commit timestamp | `KS001`, retry |
| A sync standby down | Commits at that group hang as today; readers with `S` after the commit began wait (5.9) |
| Library not loaded or setting off at a node | The node refuses a statement with a timestamp, naming the setting; the mode cannot be switched on until every instance is ready |

## 8. Upgrade and compatibility

### 8.1 One source for the extensions, one patch per branch for the core

The extensions (`kingres_snapshot`, `kingres_shard` 0.11, `kingres_ledger` 0.4) build from one source
for PG18 and PG19 (ADR 0023). **The core patches are not under that rule** (they are server files;
ADR 0023 speaks of the extensions and of `pg_compat.h`, which is an extension's header): KP-0005 and
KP-0006 exist once per branch (`kingres-18`, `kingres-19`) as separate commits of one register row,
and a major rebase re-tests both (upstream-sync.md). The extension's code that touches the hook
signatures is the same on both branches, because the hook types are declared by the patches in the
same way. **[Needs verification]** that the two patches can declare identical hook types.

### 8.2 Installed 0.1.0 / 0.10 clusters, and mixed versions

- The patches mean a **new server binary**, delivered in a new release; a 0.1.0 cluster upgrades the
  usual way ([../guide/upgrade.md](../guide/upgrade.md)). Until every instance runs it and
  `kingres_snapshot` is created in every database with distributed tables (for new databases by the
  extension registry's hook of phase 15; for existing ones an operator-visible step), `snapshot.mode`
  stays `off` and nothing changes.
- `snapshot.mode` is a **cluster setting** (phase 13 configuration, a contract field), `off` by default.
  The switch is a task that checks: the library is preloaded and `track_commit_timestamp` effective on
  every instance (standbys restarted first); the horizon has passed since then; the extension exists in
  every database with distributed tables; no `kingres_shard` older than 0.11; a clock offset below half
  the guard. It sets the mode in the catalog, and nodes learn it with the catalog push. A switch back
  to `off` is allowed at any time; a running statement finishes at its `S`.
- **Mixed versions in a rolling upgrade**: a node that is not ready never receives `S` while the mode is
  off cluster-wide; the 0.11 code is dormant before the switch (`dtx_log.commit_ts` null, no timestamp
  asked at prepare). After the switch a node with old 0.10 code is refused by `check_cluster` and by the
  task. Going back to 0.1.0 binaries after the mode was on: switch off, wait for the horizon, downgrade;
  the extension keeps its clock row.
- **Objects:** `kingres_shard` 0.11 (new objects only; 0.9 and older scripts stay identical to tag
  `kingres-0.1.0-pg18`, 0.10 belongs to the phase 15 lines and merges first); `kingres_snapshot` 0.1;
  `kingres_ledger` 0.4 for the optional cut. The REST contract takes the next free revision at
  implementation time (0.35.0 is on the phase 15 branch).
- **Always-on risk (the review's note on order):** once `kingres_snapshot` is preloaded, its commit hook
  and `track_commit_timestamp` are live on every commit, with the mode off. P8-2 is therefore tested as
  an always-on component, including the restart cost and the benchmarks, before P8-4 preloads it for
  everyone.

### 8.3 Cost on upgrade

`track_commit_timestamp = on`: a restart of every instance and 10 bytes of SLRU per transaction; one more
library in the preload list. The packaging and the install guide state both.

## 9. Interfaces

### 9.1 SQL

`kingres_snapshot.hlc_now()`, `hlc_observe(timestamptz)`, `last_commit_ts()`, `session_token()`,
`prepare_ts()`, `current_snapshot_ts()`; settings `ts`, `commit_ts` (internal), `min_ts`,
`max_forward_drift` (250 ms), `horizon` (5 s), `horizon_max` (30 s), `keeper_idle_stop` (5 min), `skew_epsilon` (100 ms), `stale_ms` (20 ms), `sample_interval` (20 ms), `limit_interval` (500 ms),
`prepared_wait_timeout` (5 s), `recency` (`default`, `bounded`, `stale`); a view `kingres_snapshot.status`.
`kingres_shard`: `kingres.check_cluster()` gains the kinds `snapshot_not_ready`, `clock_skew`,
`dtx_commit_ts_missing`.

### 9.2 REST, console, `kingres-cli`

**[Recommendation]** One contract revision (number taken at implementation time), generator and console
types updated in the same change (rule of 2026-11-28: every REST change gets its `kingres-cli`
command in the same change; if the CLI line has not merged, the report lists the commands).

| REST operation (new) | Purpose | `kingres-cli` command |
|---|---|---|
| `getSnapshotStatus` | Per server: library, clock, offset against the leader, limit, floor age, counters (snapshots, waits, `KS001` by cause), the last clock stall | `kingres-cli snapshot status` |
| `setSnapshotMode` (task) | Switch the mode; the task checks 8.2 and reports what blocks | `kingres-cli snapshot enable`, `snapshot disable` |
| Configuration entries | `snapshot.mode`, `maxForwardDrift`, `horizon`, `recency`; `track_commit_timestamp` as a restart-needed setting with a floor | `kingres-cli config get/set` |
| Alerts (new kinds) | `clockSkew`, `snapshotHorizonTooShort`, `preparedWaitLong` | `kingres-cli alerts` |
| Metrics | `kingres_snapshot_wait_seconds`, `kingres_snapshot_ks001_total{cause}`, `kingres_clock_offset_seconds`, `kingres_clock_stall_seconds` | `kingres-cli metrics` |
| Token | `getSessionToken` through SQL only; the console and CLI keep it per session | `kingres-cli token` |

## 10. Performance and how it is measured

No figure from a source exists for this design; all of the following are **[Needs verification]** by
measurement.

| Cost | Where | How measured |
|---|---|---|
| Commit hook, slot publish, two barriers, clock CAS | every commit | CPU-bound case: `pgbench -N` with `synchronous_commit = off`; I/O-bound case: `fsync = on` on a real disk (never tmpfs); each with and without the extension, 1, 8 and 64 clients |
| `track_commit_timestamp` | every commit | the same runs |
| Snapshot rebuild | each cross-group statement, each group | the window (`commit rate x age of S`) swept from 0 to 100 ms at 1 000 to 50 000 commits per second; a long-running writer present (H1) |
| Forced `suboverflowed` | tuples above the rebuilt `xmin` | a read-heavy test over recent data; the SLRU hit rate |
| Prepared wait | a reader meeting an in-doubt transaction | a test that holds a prepared transaction |
| 2PC latency | distributed write | no extra round trip; the existing 2PC benchmark |
| Keeper | up to the current hold (5 s, 30 s with an RR transaction) of dead rows; 50 snapshots per second per database while it runs | table size after one hour of `pgbench -N` |
| Clock limit worker | 2 small WAL records per second (a custom resource manager, no xid) | negligible; recovery replay measured on a standby |
| `ProcArrayInstallRestoredXmin` (N5) | exclusive `ProcArrayLock` once per transaction | a cross-group `SELECT` at 64 and 256 clients **with concurrent commits**, against the same statement with the mode off |
| In-flight commit wait (N6) | every global statement at every group | the same, with synchronous replication on and a standby 1 ms and 10 ms away; and with `recency = stale` |
| Token prefix (N2) | one function call and one CAS per group transaction of a session with a token | `pgbench -N` through the router with and without a token |
| Hot row under the keeper (N7) | HOT chain length | one row updated 5 000 times per second for 60 s with the keeper at 5 s and at 30 s; time per read of that row |

**Benchmark protocol (H8):** five runs of three minutes after a one-minute warm-up, same machine,
same data directory, the median and the spread (min to max) reported; the I/O-bound case on a real disk
(not tmpfs) with `pg_test_fsync` recorded beforehand; a difference counts only if the spread
intervals of the two builds do not overlap. **Acceptance:** CPU-bound `pgbench -N` with
`synchronous_commit = off` within 5 percent in median; the I/O-bound case within the spread; a
cross-group `SELECT` with a window of 3 ms within 20 percent of the same statement with the mode off
at the same concurrency. (These numbers are the author's proposal for the owner to confirm.)

## 11. Increments and order

Sizes are relative (S, M, L).

| # | Increment | Size | Depends on | Touches `kingres_shard`? |
|---|---|---|---|---|
| P8-M | **Model** (before any code): a model of publish, draw, observe, prepare, wait, snapshot rebuild and failover as a TLA+ spec checked with TLC, or a randomised interleaving model in Go with an invariant checker; scenarios of C1, C2, C3, H2, H4 and N1 to N4; the model includes the custom-resource-manager limit record and the promotion; each invariant has a mutant that the model must catch: **remove a barrier; draw before publish; take the clock before the snapshot; take the max with the clock at the local commit; N1 take the snapshot before `observe(S)` (the caller's snapshot); N2 omit `observe(token)` before a plain single-group write; N3 stall inside the commit hook, or start the clock from a limit that is not yet replayed, or let a transaction reach the hook before `clockReady`; N4 bounded mode with `S = physical_now + guard`, refused by a receiver whose clock is not ahead** | S to M | none | no |
| P8-S | **Spike and gate** (6 criteria below) | M | P8-M | no |
| P8-1 | Core patches in the register per branch, with tests and the rebase test of upstream-sync.md; the upstream proposal text | S | P8-S | no |
| P8-2 | `kingres_snapshot` 0.1: clock, slots, limit worker, hooks, keeper, SQL, status view, TAP tests on one node and on a primary with a standby; always-on tested | L | P8-1 | no |
| P8-3 | Test harness: Jepsen-style checker, nemeses, negative controls (section 12) | L | P8-M (a fake store first) | no |
| P8-4 | `kingresd` surface: settings, task, alerts, metrics, REST, console, `kingres-cli`, docs | M | P8-2 | no |
| P8-5 | `kingres_shard` 0.11: statement and transaction `S`, the `SET LOCAL` prefix in every sender, the 2PC protocol of 5.4, `dtx_log.commit_ts`, recovery with `C` at a 1 s interval, bounded recency, `check_cluster` kinds | L | P8-2 and the merge of phase 15 and the joins | **yes**, last, small commits |
| P8-6 | Bucket move and split (`switch_ts`) | M | P8-5 | yes |
| P8-7 | Failover (clock stall, prepared transactions after a crash, every crash point of the failover tests) | M | P8-2, P8-5 | small |
| P8-8 | Ledger consistent cut | M | P8-5 | `kingres_ledger`, `kingresd` |
| P8-9 | Hardening, tuning | M | all | |

**Beside other work in `kingres_shard`:** P8-M, P8-S, P8-1, P8-2, P8-3, P8-4 touch no file of
`kingres_shard` (P8-4 touches `kingresd`, the contract, the console and the CLI in small separate
places). P8-5, P8-6 and P8-7 wait for phase 15 rest and the join work to merge.

**The gate after P8-S (every item must hold):**

1. **Correctness, three tests** (H8):
   - **D1, stability.** At random moments under concurrent load (prepared transactions, 2PC with an
     external `C`, sub-transactions, aborted sub-transactions, asynchronous commit, `COMMIT PREPARED`
     from another session, a long-running writer) the test fixes an `S` from the clock and builds the
     snapshot at `S`; later, at several later times, it rebuilds at the **same `S`**; the visible sets
     (every xid in the window classified visible or not) must be **identical**.
   - **D2, prefix.** For the rebuilt snapshot, `visible(x)` must equal "committed and
     `commit_ts(x) <= S`" for every xid.
   - **D3, inclusion.** Every ordinary snapshot taken before the clock reading `h` is a subset of the
     snapshot rebuilt at `h` (the plain snapshot is recorded with `pg_current_snapshot()`).
   - A build with the wait rule removed, and a build with the clock read before the snapshot, must fail
     D1 or D2 (negative controls); so must a build that takes the snapshot before `observe(S)` (N1) and a build that omits the token prefix before a single-group write (N2, tested by the causal-pair test).
2. The model of P8-M passes, and its mutants fail.
3. The non-test core diff of KP-0005 and KP-0006 is under about 200 lines per branch, with no
   visibility function changed.
4. PG19 builds and passes the same tests with its own patch.
5. The benchmark protocol of section 10 meets its acceptance.
6. An independent review of KP-0005, KP-0006 and the extension's slot protocol.

If the gate fails, the fallback (option 6, which is real work, section 3.3) or a larger core patch
(option 1) needs a new ADR and the owner's acceptance.

## 12. Test plan

1. **Unit and regression, one node.** Visibility at `S` for committed before, after and equal to `S`;
   running; aborted; sub-transactions committed and aborted; prepared with `p < S`, `p > S`, decided
   either way; frozen tuples (freeze after `S` with the keeper floor); pruning; concurrent updates;
   `TRUNCATE`, `CLUSTER`, `VACUUM FULL`; parallel query under a rewritten snapshot; PG18 and PG19.
2. **Differential tests D1 to D3** of 11, run in every CI-less local gate of the extension and on
   PG18 and PG19.
3. **Clock tests with a fake clock.** Monotone across a backward step; the guard before the decision
   and the unconditional observe after it (H2); the limit; promotion with a lagging standby and its
   stall (H4); restart.
4. **Jepsen-style checks** (P8-3), on the three-group cluster of `internal/clustertest`:
   - *Bank*: accounts across groups, 2PC transfers, readers sum all accounts at one `S`; the total is
     constant (A and C).
   - *Causal pair*: W1 at A, the client passes the token to a second client, which commits W2 at B; a
     reader that sees W2 sees W1; repeated without the token (expected to fail within the skew: that
     is the documented bound) and with `recency = bounded` (expected to pass).
   - *List-append* histories checked for snapshot isolation by a cycle detector in Go (**[Needs
     verification]** port of the published Elle algorithm; Elle itself is Clojure): G1 and G-single
     absent, G2 allowed.
   - *Negative controls*: with the mode off the bank test must fail; with the wait rule off, with a
     draw-before-publish debug build, with the origin's local part not published, and with the clock
     read before the snapshot, with the snapshot taken before `observe(S)` (N1), with no token observed before a single-group write at a group whose clock is behind (N2), and with a promoted primary that takes timestamps before `clockReady` (N3), each must fail.
5. **Crash points.** Every `kingres_shard_fault_point` of 2PC (`after_first_prepare`, `after_prepare`,
   `after_log`, and recovery's) with a reader at `S` before, between and after: D whole or not at all,
   and the wait ends. Kill the origin, a participant, a primary (failover), with and without
   synchronous replication; kill during the stall.
6. **Clock skew injection.** The extension's test build offsets its physical clock by +/- 100, 200, 400
   ms; correctness holds at all; recency is measured; above the guard timestamps are refused and the
   alert is raised; a participant behind by almost the guard still commits (H2).
7. **Upgrade.** A 0.1.0 cluster with data upgraded, the mode switched on under a running bank workload
   and off again; a mixed-version attempt is refused.
8. **Performance** (section 10) before and after each increment from P8-2.
9. **Existing statements.** Every cross-group statement test runs with the mode on and compared with off;
   pushdown paths compared with the complete scan (ADR 0022).
10. **Stress of M9.** A build with a delay injected between publish and draw, and between `observe(S)`
    and the slot scan, run under the differential tests.
11. **Deadlocks and the origin wait (N10).** A reader at the origin with `S >= p_local` while the origin is in its prepare phase, where the reader is itself a distributed writer holding locks at other groups: run against the existing distributed deadlock detection (phase 5); no hang beyond `prepared_wait_timeout`.
12. **D1 load additions.** `COMMIT PREPARED` with a `C` below the local clock, and a writer delayed between publish and draw, are part of the D1 workload (re-review).
13. **Promotion (N3).** A promoted primary refuses a transaction before `clockReady` at its start (outside a critical section); the stall is measured and bounded; a commit never waits inside the hook; kill during the stall; the limit record is replayed on a standby (tests of the custom resource manager on PG18 and PG19).
14. **Token (N2).** A session commits W1 at A (clock ahead), then a plain single-group write W2 at B
    (clock behind): W2's timestamp is above W1's, and a global reader never sees W2 without W1.

## 13. Security review points

1. **Who may set a timestamp.** `ts` and `commit_ts` are accepted only from a node session that proved
   its identity (`forward_auth.c`, as `forward_term`); a plain client cannot set them (a far-past `ts`
   would read a state the role may no longer be allowed to see; a forged `commit_ts` would break the
   cut for everyone). `min_ts` is allowed to any role and clamped by the guard.
2. **A compromised node** can give a wrong `S` to a statement it originates. It cannot rewrite another
   node's past: timestamps are bounded by the guard and the horizon. "A compromised leader is a
   compromised cluster" (control-plane README) already holds.
3. **Information disclosure through waits:** a reader learns that a prepared transaction exists
   (timing); `pg_prepared_xacts` discloses more to the same roles.
4. **Denial of service:** a long-held prepared transaction delays readers above its `p` (bounded in
   time by `prepared_wait_timeout`, 5.9); old `S` costs window reads (bounded by the horizon); the
   keeper's bloat is bounded.
5. **Horizon floor and vacuum:** an error in the floor can let vacuum remove a tuple a snapshot needs
   (silent wrong result). The tests of 12 include vacuum and freezing at the edge of the horizon; the
   keeper registers its new snapshot before releasing the old one; the independent review of the
   gate covers it.
6. **Core patches and licence:** PostgreSQL-licensed files, registered as KP-0001 to KP-0004 are; no
   Citus or Shardman code is read or copied (ADR 0004); the design is derived from the papers and the
   PostgreSQL source.
7. **Audit:** `setSnapshotMode` and the setting changes go through the existing audit log.

## 14. Open questions only the owner can decide

Each has a recommended default; none blocks P8-M or the spike.

1. **Accept the first core patches** (KP-0005, KP-0006, one per branch). Default: yes, behind the gate.
2. **Guarantee wording:** snapshot isolation across groups, with causality only through tokens and a
   skew-bounded recency, plus the bounded mode from P8-5. Default: as in 6; ADR 0031 says "causal"
   only with the token.
3. **`snapshot.mode` default after the feature ships.** Default: `off` first.
4. **Horizon.** Default hold 5 s, 30 s while a cross-group RR transaction is registered, keeper on demand (N7); the RR limit and the empty-ring window are stated.
5. **Drift guard default.** Default 250 ms (changed from 500 ms).
5a. **Assumed skew bound `skew_epsilon`** for the bounded mode, default 100 ms (N4).
5b. **A second custom WAL resource manager ID** for the clock limit record: an owner action on the PostgreSQL wiki, as was done for columnar (255) (N3).
6. **`SERIALIZABLE` across groups.** Default: no.
7. **Reads on standbys at a timestamp.** Default: later.
8. **Release train.** Default: part 1 after ledger witnesses, which need no core patch (the ledger
   document's first deliverable).

## 15. Sources

Fetched or searched on 2026-11-28: commitfest entry 4912 and the linked thread; the CockroachDB HLC
package page, the Cockroach Labs clock blog, the SIGMOD 2022 summary and Jepsen's analyses; the
YugabyteDB read restart page; the HLC paper. PostgreSQL internals: read in the two trees as cited in
1.2 (not from memory). Background, to check before ADR 0030 is accepted: the Spanner OSDI 2012 paper; the
Clock-SI SRDS 2013 paper; the TiDB oracle documentation; the PostgreSQL 17 release notes (removal of
`old_snapshot_threshold`); Elle (Kingsbury and Alvaro, VLDB 2020).

## 16. Review of 2026-11-28 and where each finding is answered

The independent review of the first version (4 critical, 8 high, 9 medium, 4 low) found that the core
idea is sound as a definition and that the text broke its invariant in four places. Each finding
and its answer:

| Finding | Answer | Where |
|---|---|---|
| C1 the origin's local part never prepared | The origin draws `p_local`, publishes it, `C = max(p_i, p_local)` with no clock term, the local commit uses exactly `C`, the clock is raised first | 5.2 (2PC protocol, I4), 5.4 |
| C2 publish-then-draw must cover the prepare timestamp | The prepare hook draws with the same publish-then-draw; the slot covers it until `MarkAsPrepared`, then `prepared_at` does; invariant I2 | 5.2, 4.1 |
| C3 clock read after `GetSnapshotData`; observe before visible | The sample's clock is read after its snapshot; `observe(C)` before `ProcArrayEndTransaction` at `COMMIT PREPARED`; invariants I3 | 5.3, 5.5, 5.2 |
| C4 hook placement vs snapshot reuse | Hook in `snapmgr.c`, never edits the static snapshots, returns an extension-owned snapshot, kinds; array growth in the extension; `CopySnapshot` accepts any array; size gate re-estimated; the reuse path and `CopySnapshot` verified in the trees | 4.2, 4.4, 1.2 items 5 to 7 |
| H1 window bounded by the oldest running xid | Samples store `xmax_i` and `xip_i`; candidates are `xip_i` plus xids since the sample | 5.3 steps 4 and 5, 5.5 |
| H2 guard refusing after the decision | The guard applies before the decision only; `COMMIT PREPARED` and recovery observe unconditionally; the origin checks every `p_i` first | 5.2 (drift guard), 7 |
| H3 per-row or bounded prepared wait; sync standby down | Not per row (it would change visibility): the limit is stated, the recovery interval is 1 s in mode on, readers nudge recovery, metrics and an alert; the sync-standby case is stated | 5.9, 6.2 item 7 |
| H4 promotion clock vs the guard | Stall until physical time reaches the limit instead of jumping; limit relative to the guard; shown on the failover task | 5.6 |
| H5 causality overclaimed | Wording changed; token includes read `S`; router carries it; bounded recency moved to P8-5; guard 250 ms; no uncertainty restarts, stated | 6.1 to 6.4, 14 |
| H6, H7 | Ledger design | [phase8-multinode-ledger.md](phase8-multinode-ledger.md) section 17 |
| H8 correctness criterion in the gate | D1 to D3, a model before code (P8-M), negative controls, a benchmark protocol, an independent review | 11, 12 |
| M1 subtrans retention, forced `suboverflowed` | Verified (`ComputeXidHorizons` includes backends' xmin); cost measured | 1.2 item 8, 5.3, 10 |
| M2 "pay nothing" | "Almost nothing", RR case measured | 2, 5.4 |
| M3 horizon vs RR | 5 s hold, 30 s with a cross-group RR transaction (16.1, N7); RR limit and empty ring stated | 5.4, 5.5, 14 |
| M4 apply workers and timestamps | Hook observes origin timestamps without refusing; invariant on `switch_ts`; visible side effects stated | 5.7, 5.2 |
| M5 ADR 0023 misapplied; unpatched server | Core patches per branch; `pg_compat.h` is the extension's; library check; upstream-sync line | 4.1, 4.4, 8.1; ADR 0032 |
| M6 fallback description | Rewritten as real work | 3.3 option 6; ADR 0032 |
| M7 read-your-writes across connections | Stated plainly; router carries the token per connection | 6.2 item 3, 6.4 |
| M8 notary | Ledger design | [phase8-multinode-ledger.md](phase8-multinode-ledger.md) section 17 |
| M9 barriers | Specified in I1; stress test of 12 item 10 | 5.2, 12 |
| L1 limit row cost and stall | Stated | 5.6 |
| L2 `TRUNCATE` and rewrites | Stated | 5.7, 6.2 item 4 |
| L3 termination of the wait loop | Argued; timeout | 5.3 step 3 |
| L4 block builder under a rewritten snapshot; format 2 | Ledger design | ledger document 5.3 and 17 |

### 16.1 Re-review of 344375cc7f (second round)

The second review found 20 of 25 first-round findings fixed and 5 partly fixed (C4, H3, H4, H5, M7),
and 14 new problems. Each answer:

| Finding | Status | Answer | Where |
|---|---|---|---|
| N1 (critical) the hook got a snapshot taken before `observe(S)` | fixed | The hook takes no snapshot from the caller: it runs `observe(S)`, then `GetSnapshotData` into its own struct, then the slot scan; invariant I6; mutant "snapshot before observe" in the model, D1 and the negative controls | 4.2, 5.2, 5.3 step 2, 11, 12 |
| N2 (high) the token not observed at plain single-group writes | fixed | Every group transaction of a session that holds a token calls `observe(token)` as a batch prefix (one call, one CAS); invariant I7; causal-pair negative control; measured | 5.2 (token prefix), 6.4, 10, 12 item 14 |
| N3 (high) promotion stall inside the critical section; limit unreadable before open | fixed | The limit is carried in WAL by a custom resource manager (redo fills shared memory on the standby); the start value is computed at end of recovery; `clockReady` gates a transaction **at its start**, never in the hook; the failover task keeps the primary out of the router until ready. **Owner action:** a second WAL resource manager ID on the PostgreSQL wiki, as was done for columnar (255). Mutants in the model | 5.6, 4.1, 11, 12 item 13, 14 |
| N4 (high) bounded mode refused by its own guard | fixed | A separate assumed skew bound `skew_epsilon` (default 100 ms) below the guard (250 ms): `S = physical_now + skew_epsilon`; both specified; the mode is correct only while the real skew is below `skew_epsilon`; mutant in the model | 6.3, 11 |
| N5 exclusive `ProcArrayLock` per global statement | fixed (measure) | Installed once per transaction, and per statement only when the rebuilt `xmin` is older than the advertised one; benchmark at high concurrency with concurrent commits | 5.3 step 6, 10 |
| N6 waiting for in-flight ordinary commits | stated, optional mitigation | Stated with the synchronous replication cost; `recency = stale` takes `S` slightly in the past (default off); measured with a standby 1 ms and 10 ms away | 5.9, 10 |
| N7 keeper per database, always on, 30 s hold, HOT chains | fixed | On demand per database, stops when idle; default hold 5 s, 30 s only while a cross-group RR transaction is registered; hot-row test | 5.5, 10 |
| N8 epoch successor | fixed | Ledger design | [phase8-multinode-ledger.md](phase8-multinode-ledger.md) 5.1, 17 |
| N9 `max_cosignature_age` vs archived bundles | fixed | Ledger design | same |
| N10 deadlock with the origin's own xid wait | fixed (test) | Stated; deadlock test with a reader that is itself a distributed writer | 5.9, 12 item 11 |
| C4 (partly) | fixed | See N1; lifetime of the extension-owned snapshot stated; the size gate stays about 200 lines per branch | 4.2, 4.4 |
| H3 (partly) | stated | Ordinary in-flight commits added to the limits (N6); the origin's whole prepare phase is stated; mitigation (a) helps only when the origin is alive, which is said | 5.9 |
| H4 (partly) | fixed | See N3 | 5.6 |
| H5 (partly) | fixed | See N2 and N4 | 6.3, 6.4 |
| M7 (partly) | fixed | See N2: the write group observes the token | 6.4 |
| Low: snapshot lifetime | fixed | Valid until the next hook call | 4.2 |
| Low: origin xid before `p_local` | fixed | `prepare_ts()` calls `GetTopTransactionId()` first | 5.2 |
| Low: commit_prepared hook context PG18 and PG19 | fixed | Code valid in both contexts; tests on both majors | 4.1 |
