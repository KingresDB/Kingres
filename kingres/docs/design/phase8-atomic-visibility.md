<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
# Phase 8, part 1: atomic visibility of distributed transactions (design)

Status: Proposed design, 2026-11-28, written after the owner's decision that **no core patch to
PostgreSQL is made**, and revised the same day after an independent review (section 19). It replaces the
timestamp design, which is kept as a rejected alternative in
[phase8-timestamp-snapshot-rejected.md](phase8-timestamp-snapshot-rejected.md). Not implemented. Where this
document says "exists" it describes `kingres-18` at d1344c1aa9 and names the file.

Builds on [shard-phase2a.md](shard-phase2a.md) (2PC, the distributed scan),
[shard-phase2b.md](shard-phase2b.md), [shard-phase4.md](shard-phase4.md), [ha-phase5.md](ha-phase5.md),
[ha-phase5-part2.md](ha-phase5-part2.md), [ADR 0009](../adr/0009-two-phase-commit-no-global-snapshot.md),
[ADR 0022](../adr/0022-the-complete-scan-stays.md), [ADR 0023](../adr/0023-one-extension-source-for-several-majors.md).
Part 2 is [phase8-multinode-ledger.md](phase8-multinode-ledger.md). Decisions: proposed ADRs 0030 to 0034.

Kinds of statements: **[Fact]** (read in the repository or the two PostgreSQL trees, with
`file:function`, or in a cited source), **[Recommendation]**, **[Needs verification]**. "PG18" is this
repository's tree (PostgreSQL 18.7), "PG19" the tree of branch `kingres-19` (19.0).

## 0. The guarantee in plain words

With **atomic visibility** switched on for a database, a **read-only statement that reads more than
one shard group** is designed so that it never sees a distributed (two-phase commit) transaction on one
group and not on another, and sees each group in one state for the whole statement. Where the protocol
cannot be sure of that, **the statement fails with a clear error** instead of reading.

**Status of this sentence.** Until the model P8-M, the harness P8-T and an independent review have
closed the conditions of 7.1, it is the **design goal**, not a claim that the product makes. When they
have, it is a claim with exactly those conditions. One stated exception: a transaction whose part was
lost at a participant (listed in `kingres.dtx_incident`, after an asynchronous failover) is partial in
the database itself, not only in visibility, and is read as it is.

It does **not** give a global snapshot: the order of two transactions that are independent in the
database but causally related in the application can still be seen inverted across groups, and write
skew across groups remains (section 3.3, with examples and workarounds). **It covers only read-only
statements** (analytic reads and reports, autocommit or before a transaction's first write); an
application that writes and then reads in one transaction gets nothing from it (3.4). The mode is named
for what it gives, and it is **off by default, enabled per database, with a kill switch**.

## 1. What exists, and what is relied on

### 1.1 Facts about KingresDB's code (read on 2026-11-28)

1. **[Fact]** A transaction that wrote on two or more nodes uses 2PC (`contrib/kingres_shard/src/node_conn.c`,
   header comment, `precommit()`, `postcommit()`): `PREPARE TRANSACTION '<gid>'` on every remote writer;
   one row per prepared remote in the origin's `kingres.dtx_log`, written by the origin's own local
   transaction (the row is the commit decision); `COMMIT PREPARED` after the local commit. The origin
   always has an xid (the `dtx_log` insert assigns it), so it is itself a participant that is never
   prepared. The gid is `kingres_<origin node>_<origin pid>_<origin full xid>_<target node>`
   (`recovery.c`, `parse_gid`): the part before the target is common to all participants, and it names
   **nodes**, which change when a group fails over (5.2).
2. **[Fact]** `recovery.c` resolves prepared transactions the origin created (worker every
   `recovery_interval`, default 30 s, `kingres_shard.c`), keeps decisions for `decision_retention`
   (300 s), lists `pg_prepared_xacts` on every active primary, and records `kingres.dtx_incident` for the
   cases that asynchronous replication can make wrong (`record_incident`).
3. **[Fact]** Each remote session begins its transaction with the local isolation level
   (`kingres_shard_begin_xact`, `node_conn.c:1430`), one session per node per backend, obtained with
   `kingres_shard_get_conn(host, port, ...)` (`node_conn.c:990`), which is called from `fragment_scan.c:521`
   (followed by `kingres_shard_begin_xact` at 522), `batch_sender.c`, `copy_routed.c`, `fanout.c` and others.
   So today a statement that spans groups reads each group at its own snapshot.
4. **[Fact]** `kingres_shard` already chains `ExecutorStart_hook` (`fanout.c`, `fo_executor_start`).
5. **[Fact]** KingresDB patches no server code (core patch register: KP-0001 to KP-0004). **The owner
   decided on 2026-11-28 that it stays so.**
6. **[Fact]** `kingresd` and the failover code use a monotonic clock for decisions
   (`kingres/control-plane/internal/clock/clock.go`); this design uses **no wall clock and no clock
   comparison between nodes** (4.1).
7. **[Fact]** The SQLSTATEs `KS001` to `KS023` are in use (`contrib/kingres_shard/README.md`, the
   p15 line may add more); new codes are taken from the next free numbers at implementation time.
8. **[Fact]** The repartitioned join reads one relation through the origin's sessions and ships it as
   parameters to another group (`repartition.c` header; called from the paths of `fragment_scan.c`).

### 1.2 Facts about PostgreSQL (read in both trees on 2026-11-28)

1. **[Fact] Executor hook and snapshot ownership.** `ExecutorStart_hook` has the type
   `void (*)(QueryDesc *, int)` in both trees (`executor/executor.h`, called from `ExecutorStart`,
   `execMain.c`). `CreateQueryDesc` registers `queryDesc->snapshot` (`tcop/pquery.c`, `RegisterSnapshot`)
   and `FreeQueryDesc` unregisters it. `ExecutorStart` (`execMain.c`) sets
   `estate->es_snapshot = RegisterSnapshot(queryDesc->snapshot)`, so **swapping `queryDesc->snapshot` in the
   hook is what the executor uses**. `PortalRunSelect` pushes `queryDesc->snapshot` as the active snapshot
   again before running (`pquery.c`), so replacing the active-stack top inside the hook is not what matters.
   `ExecInitParallelPlan` asserts `GetActiveSnapshot() == estate->es_snapshot` (`execParallel.c:740` PG18,
   `:794` PG19): **the candidate snapshot must be registered (copied) before the hook returns**, or a
   cassert build fails. `portal->portalSnapshot` is set only for utility statements, so no dangling
   pointer arises for `SELECT`. **[Needs verification]** by the spike (P8-S) with cassert and parallel query.
2. **[Fact] Read-committed snapshots are per statement.** `exec_simple_query` (`tcop/postgres.c`) takes
   a snapshot for analysis and pops it; `PortalStart` takes a fresh one for the executor. In
   `REPEATABLE READ` the transaction snapshot is fixed at the first query (`GetTransactionSnapshot`,
   `utils/time/snapmgr.c`, `FirstXactSnapshot`) and no extension API replaces it.
3. **[Fact] `GetLatestSnapshot`** (`snapmgr.c`) returns a fresh snapshot "even if we are executing in
   transaction-snapshot mode"; it is exported.
4. **[Fact] SPI.** `SPI_execute(..., read_only = true)` runs queries with the active snapshot, taking
   none (`executor/spi.c`: the read-only branch sets `snapshot = GetActiveSnapshot()`, and the comment
   on the case "snapshot == InvalidSnapshot, read_only = true: do nothing for queries"). A function that
   pushes a candidate snapshot as the active one and calls SPI read-only reads under it.
5. **[Fact] Prepared transactions are visible to snapshots as in progress.** `MarkAsPrepared`
   (`twophase.c`) adds the dummy PGPROC to the procarray (`ProcArrayAdd`); a prepared transaction's xid is
   in every concurrent snapshot's `xip`. `pg_prepared_xacts` is generated by `pg_prepared_xact`
   (`twophase.c`) with the columns `transaction, gid, prepared, ownerid, dbid` (`pg_proc.dat`); it shows
   the state at the time of the call, not a snapshot.
6. **[Fact] Snapshot functions at SQL level.** `pg_current_snapshot()`, `pg_snapshot_xip(snapshot)` and
   `pg_visible_in_snapshot(xid8, snapshot)` (`utils/adt/xid8funcs.c`) exist in both trees.
7. **[Fact] Snapshot import** requires `REPEATABLE READ` or `SERIALIZABLE` (`snapmgr.c`,
   `SetTransactionSnapshot`). This design does **not** use it; it is listed in 9.3.
8. **[Fact] A prepared transaction stays in doubt until `COMMIT PREPARED`/`ROLLBACK PREPARED`**, which
   runs in the committer's backend (`FinishPreparedTransaction`, `twophase.c`).
9. **[Needs verification]** `CLOCK_MONOTONIC` on Linux does not count time while the system is suspended
   (`clock_gettime(2)`), while `CLOCK_BOOTTIME` does; a frozen or migrated virtual machine may move
   either. The design uses `CLOCK_BOOTTIME` for spans and ages and compares it with `CLOCK_MONOTONIC`
   (4.1, 4.3); the behaviour on the project's targets is checked in the harness.

### 1.3 What is not used

No commit timestamps (`track_commit_timestamp` stays as it is), no replication origin, no custom WAL
resource manager, no `ProcArrayInstallRestoredXmin`, no new preload library, **no restart** to
switch the feature on. This is deliberate (section 9).

## 2. Why this and not something stronger

The owner's decision: no core patches; fall back to the design that "fixes fractured reads of 2PC
transactions without the causal-order guarantee". **Was anything stronger possible with existing
hooks only?** The analysis:

1. **A consistent cut (snapshot isolation across groups) needs a total order of commits that every
   group's snapshot respects.** The only way to give a PostgreSQL snapshot a position in a global order
   is a commit timestamp that the snapshot compares against. Stock PostgreSQL offers (a) the commit
   timestamp module, whose value is the wall-clock time of the commit record
   (`RecordTransactionCommit`, `xact.c`), which an extension cannot choose for an ordinary commit; (b) a
   replication-origin session that lets a session set the timestamp (`replorigin_session_origin_timestamp`,
   PG18; `replorigin_xact_state`, PG19: exported variables, not an API), which would have to be set for
   **every** commit of every session, adds an origin record to every commit record and changes what logical
   replication (the bucket moves) decodes. Neither gives the "publish a bound, then draw" step that the
   timestamp design needs (its review found that this ordering is what makes the cut sound), because the
   draw happens in code the extension cannot hook. **So: not achievable with existing hooks alone, without
   hacks on exported internals that the project would not call safe.** The timestamp design, its review and
   its hooks are the rejected alternative (17).
2. **Causal order across groups** needs the same order. Not available.
3. **What is achievable** is atomic visibility of 2PC transactions and one state per group per
   statement, by *validating* ordinary snapshots against durable per-participant evidence (this
   document). It adds to what exists: no fractured reads of distributed transactions and no two states of
   one group inside one covered statement. It does not add: causal order, snapshot isolation, write-skew
   protection, coverage of statements that already wrote.

## 3. Goals, non-goals, and the guarantee

### 3.1 Goals

1. For a covered statement: **atomic visibility of every distributed transaction** across the groups the
   statement reads, and **one state per group** (anomalies A and C of 3.3).
2. **An error rather than a possibly fractured read** (section 7, with its conditions).
3. **Single-group statements pay nothing**; cross-group read-only statements pay about one extra round
   trip; writers pay one small insert per participant per distributed transaction.
4. **No server change, no restart to enable, per database, a kill switch, a staged rollout** (section 9).

### 3.2 Non-goals

Causal order across groups; snapshot isolation or serializability across groups; reads at a time in the
past; reads on standbys; making statements of a transaction that has already written atomic; cursors;
user functions that read tables; repartitioned joins; `REPEATABLE READ` and `SERIALIZABLE` transactions
(4.7).

### 3.3 The anomalies, with examples

| Anomaly | Example | Today | With atomic visibility (covered statements) |
|---|---|---|---|
| **A. Fractured read** | A transfer moves 10 from account 1 (group A) to account 2 (group B) in one transaction. A report that reads both sees account 1 debited and account 2 not yet credited | Possible | **Gone**: both or neither |
| **C. Two states of one group in one statement** | `SELECT ... FROM orders o JOIN order_items i ...` where both tables are distributed and one group is read twice | Possible in `READ COMMITTED` | **Gone**: one state per group |
| **B. Causal order across groups** | A user posts (group A) and the application then writes a reply (group B) in a **separate** transaction. A reader can see the reply and not the post | Possible | **Remains** |
| **Write skew across groups** | Two sessions each check "at least one doctor on call" on different groups and each remove their own doctor | Possible | **Remains** (and `SERIALIZABLE` is per node) |
| **Staleness** | A read on another node right after a commit may miss it | Possible | **Remains**, as on any asynchronous system |
| **A lost part** | After an asynchronous failover a participant lacks its part of a committed transaction (`dtx_incident`) | Possible | **Remains**: partial in the database itself |

Practical workarounds, to be in the documentation next to the mode:

- **Colocate related data in one group** (distribute related tables by the same key, or use a
  distributed schema): a statement inside one group is atomic natively (anomaly B cannot arise).
- **Write dependent rows in one transaction**: the post and its reply in one transaction become one
  distributed transaction, which atomic visibility covers.
- For a causal chain across transactions, read in the order of the chain (post, then reply), and
  accept that a reply may be visible before its post: do not draw conclusions from the absence of a row on
  another group.
- Do reads that must be consistent inside **covered** statements: autocommit statements, or read-only
  statements before the first write of a transaction (3.4).

### 3.4 Which statements are covered

A statement is covered when **all** hold:

1. It is a top-level `SELECT` (including `WITH ... SELECT` without data-modifying parts; nesting level 0,
   not a statement run by SPI from a function, not `EXPLAIN` without `ANALYZE`) that reads distributed
   tables of **two or more groups**, counting the local group when it reads local buckets or a reference
   table. A statement that reads one group needs nothing.
2. Its transaction has **not written** anything (`GetTopTransactionIdIfAny()` invalid and no remote
   session has written): autocommit statements, and statements of an explicit transaction before its
   first write.
3. It runs in `READ COMMITTED`. **`REPEATABLE READ` and `SERIALIZABLE` are not covered in this
   release** (4.7).
4. It has no cursor (`DECLARE`), no user-defined function that can read tables (decided with the
   existing shippability analysis, `shippable.c`; **[Needs verification]**), no `FOR UPDATE`, and **no
   repartitioned join** (8 of 1.1: the join ships rows through the origin's sessions to temporary
   relations in the target group's transaction session, which is not the helper snapshot).

`EXPLAIN` shows the verdict (4.8). A statement that is not covered **runs exactly as today** and says
why. (A statement that is covered but whose evidence is missing **fails**, section 7: that is not
"not covered".)

## 4. The protocol

### 4.1 Evidence written by writers (`kingres_shard` 0.11)

New objects, in a new version of `kingres_shard` (0.9 and older scripts stay byte-identical to tag
`kingres-0.1.0-pg18`; 0.10 belongs to the phase 15 lines):

```
kingres.dtx_member(
  dtx_id   text   primary key,   -- 'kingres_<origin>_<pid>_<origin full xid>' (the gid without the target)
  parts    int[]  not null,      -- the GROUP ids of every participant, origin's group included (H2)
  xid      xid8   not null,      -- this node's transaction id (pg_current_xact_id())
  boot_id  bigint not null,      -- random number chosen when the instance started
  mono_us  bigint not null       -- kingres.mono_us(): CLOCK_BOOTTIME microseconds, this node
)  -- indexes on (boot_id, mono_us) and on (xid)

kingres.av_state(                -- one row per group, replicated with the group
  group_id int primary key,
  ready    boolean not null,     -- recording is continuous and no older prepared transaction exists (H3)
  note     text,                 -- why it is not ready
  changed_at timestamptz
)
```

**Writer side (record).** Every participant inserts its row **inside the distributed transaction,
before `PREPARE`**: a remote participant in the same batch as `PREPARE TRANSACTION` (no extra round
trip), the origin in its local transaction next to the `dtx_log` rows. `parts` lists **group ids**, built
by the origin from the catalog, because a failover changes the node that serves a group and the gid names
nodes (the validation maps group ids to the current primaries, H2). The row is MVCC data written
atomically with the transaction's other writes, so **"the row is visible under a snapshot" means "the
transaction is visible at this group under that snapshot"**.

**Last-prepare mark.** Every participant, **the origin included** (M7), sets the shared-memory value
`last_prepare_mono` when it inserts its row; the report skips the table read only if nothing was inserted
on this node during the window.

**Bounded prepare phase.** The origin measures (`CLOCK_BOOTTIME`) the time from sending the first
`PREPARE` to having every answer. If it exceeds `kingres_shard.dtx_window` (default 1 s) the origin acts
according to `kingres_shard.dtx_window_action`: `count` (only the counter `dtx_window_overrun_total`;
**the default in `record` and `observe`**: those modes change nothing for writers, M4) or `rollback` (roll
the transaction back with an error before writing the decision; **the default in `enforce`**). The
rollback happens before the decision row, so it cannot abort a transaction that any participant has
committed.

**Recording watermark (H3).** `av_state.ready` is `true` only while **every distributed transaction that
prepares at this group writes its row, and no prepared kingres transaction older than the last gap
exists anywhere**. These events make a group not ready (a **gap**): `record` switched off and on again;
an extension version change (upgrade or downgrade); a restore or point-in-time recovery of the group
(`kingresd` knows, because it runs them); an explicit operator reset (`kingres.av_reset_ready()`, for
suspected tampering). A **new group** starts ready (no earlier transaction could involve it). A group
returns to ready when `kingresd` (the task of 8) has seen that `record` is on, that no prepared
transaction with a gid of the cluster that was prepared before the gap exists on any primary
(a cluster-wide look, which `recovery.c` already does node by node), and that `Wt` has passed. Rows
removed by hand are tampering that the design cannot detect and says so (7.1). `record = off` is refused
by the setting's check hook while the database's mode is `observe` or `enforce`.

**Clock.** `kingres.mono_us()` reads `clock_gettime(CLOCK_BOOTTIME)` and checks it against
`CLOCK_MONOTONIC`: if the two diverge by more than 100 ms since the instance started, the node
reports `clock_anomaly` (7.1). Nothing compares values of two nodes. `boot_id` changes at each instance
start, so a value is only compared within one boot.

**Garbage (M1, M8).** A row is deleted only when **all** hold: it is older than
`kingres_shard.dtx_member_retention` (default **60 s**, never less than `2 * Wt`); its transaction is not
prepared on this node; and every other participant group (from `parts`) confirms, in the same pass of the
recovery worker, that it has no prepared transaction with that `dtx_id` (recovery already connects to
every primary). A row is therefore never removed while its transaction is in doubt at any participant.
Size: at 5 000 distributed transactions per second, 3 participants, 60 s, about 300 000 rows per node
plus two indexes; checked in the rollout (9.2).

### 4.2 The report

`kingres.av_report(window_us bigint)` runs **inside the reader's snapshot** (a helper session in
`REPEATABLE READ READ ONLY`, or SPI read-only under a candidate snapshot at the local group) and
returns, for this group:

- `mono_now`, `boot_id`, **`ready`**, **`young`** (true when the instance started, or was promoted from
  a standby, less than `Wt + dtx_window` ago, H1), `clock_anomaly`, `recording`;
- **`cands`**: `(dtx_id, parts, age_us)` for every `dtx_member` row **visible under the snapshot**
  with `boot_id = current` and `mono_us >= mono_now - window_us`. If no distributed transaction has
  prepared on this node during the last `window_us` (`last_prepare_mono`), the table is not read;
- **`pending`**: `(dtx_id, parts)` for every transaction whose xid is **in the snapshot's `xip`** and
  that is a KingresDB distributed transaction: the function takes `xip` (`pg_snapshot_xip`) and maps each
  xid to a `dtx_id` through (a) `pg_prepared_xacts` (the `dtx_id` is the prefix of the gid; transactions
  still prepared at the time of the call: **no row is needed**) and (b) a lookup in `dtx_member` by `xid`
  with `GetLatestSnapshot()` (transactions that committed after the snapshot and are no longer prepared,
  the "gap"). A transaction in `xip` that is neither is not a prepared distributed transaction.

### 4.3 The reader

For one covered statement with the groups `G` (two or more), **in rounds**. A round is **one
acquisition at all groups in parallel**; the proof uses only the reports and snapshots of **one round**.

1. **Acquire, all groups, in parallel.** For every remote group `g`, on its helper session (`BEGIN
   ISOLATION LEVEL REPEATABLE READ READ ONLY; SELECT * FROM kingres.av_report($Wt)`): the snapshot `s_g` is
   taken by the first statement and kept until the helper transaction ends; the report is computed under
   it. For the **local** group, a candidate snapshot is taken (`RegisterSnapshot(GetLatestSnapshot())`),
   pushed as the active snapshot, and the report is computed by SPI read-only. The coordinator measures
   (`CLOCK_BOOTTIME`) the **acquisition span**, from the first send to the last answer.
   `Wt = 2 * (dtx_window + slop)`, `slop = kingres_shard.av_slop` (default 500 ms), so 3 s by default.
   If the span is larger than `slop`, the round is **not trusted**: start a new round (step 4).
2. **Evidence checks first.** If any report says `ready = false`, `young`, `clock_anomaly`, or
   `recording = false`, or a `boot_id` differs from one seen earlier in the same statement, **the
   statement fails** (7) after waiting for `young` to pass, within `av_wait_timeout`.
3. **Validate** (at the coordinator, no further round trip in the common case). A violation is a pair
   `(D, g)`: "D is visible at some group of `G` and not visible at `g`, a participant in `G`".
   - **Seeds.** For each `g1` and each `D` in `cands_g1` with `age <= Wt / 2`: for every group `g2` among
     the participants of D that are in `G`, other than `g1`: if `D` is not in `cands_g2`, then `(D, g2)` is
     a violation.
   - **Pending.** For each `g2` and each `D` in `pending_g2`: for every `g1` among the participants of D
     that are in `G`, `g1 != g2`: if `D` is in `cands_g1`, `(D, g2)` is a violation; otherwise `(D, g1)` is
     **unresolved**.
   - **Unresolved lookups (second round trip, only if any, on the same round's helper sessions).** Ask
     `g1` under its snapshot `s_g1`: `SELECT dtx_id FROM kingres.dtx_member WHERE dtx_id = ANY ($1)`. If D
     is visible at `g1`, `(D, g2)` is a violation. **A miss is trustworthy only because `g1` reported
     `ready`** (H3): D prepared at `g1` after `g1` became ready wrote its row, and one prepared before was
     resolved before ready; a miss at a node that is not ready was already refused in step 2.
   - Transactions listed in `kingres.dtx_incident` at any participant are ignored for violations (their
     part is lost in the database, not hidden) and counted.
4. **Resolve: retake everything (C1).** If there is a violation, or the span was too large, **end every
   helper transaction and every candidate, wait (backoff 1, 2, 4, ... ms), and start a new round with new
   snapshots at all groups**; **never keep a snapshot of an earlier round across a wait**. Reason: the
   argument of 4.6 needs every pair of snapshots in use to be within `slop` of each other; a snapshot kept
   across a wait of `w` makes the gap `w`, and a transaction D' that committed everywhere during the wait
   would be seen at the retaken group and not at the kept one, with an age beyond the seeds and no pending
   entry: a fractured read. A new round costs one round trip.
5. **Run** when a round validates (4.4).

### 4.4 Running the statement

The statement's fragments for each remote group `g` run **as cursors in the helper transaction of `g`
of the validated round**, so every cursor of the statement sees `s_g` (anomaly C is gone). The local group
reads with the candidate snapshot as `queryDesc->snapshot`: the hook registers it
(`RegisterSnapshot`, **registered, not only pushed**: the assert of `execParallel.c`, 1.2 item 1),
unregisters the old one and calls the standard start; the executor then uses it (`es_snapshot`) and
pushes it again at run. Helper transactions are committed when the statement ends; the helper connection
stays open for the next statement (one per node per backend, which doubles the connection use of readers
at the nodes: counted in 6.1). The existing complete scan and the pushdown paths are unchanged (ADR 0022):
they are told which session to use.

**Every sender goes through the helper (M5).** The point where a fragment obtains its connection is the
pair `kingres_shard_get_conn` + `kingres_shard_begin_xact` (`node_conn.c`), used at `fragment_scan.c:521-522`
and by the other senders (`batch_sender.c`, `copy_routed.c`, `fanout.c`; the list is to be completed in P8-S).
Design: (1) a **session role** argument (`transaction` or `read helper`) in those two functions; a covered
statement sets a per-statement context that makes them return the helper; (2) a **runtime guard**: while the
context is active, any read through a connection that is not the helper is an error
(`evidence_missing`); (3) a **build-time registry** of the call sites of `kingres_shard_begin_xact`, with
a test that fails when a call site is added that is not in the registry. The **repartitioned join** is the
reason for the exclusion in 3.4: its shipped relation is written into the transaction session of the target
group, so it is not covered; the joins line is changing repartitioning now, and the integration point to
adapt later is the fragment start above.

A transaction that is not covered keeps using the **transaction's own sessions** (as today); the helper
sessions exist only for covered statements, so the semantics of writers are untouched.

### 4.5 In-doubt wait

A violation `(D, g)` means D is decided (visible elsewhere, so committed there, so its decision is
durable) and will become visible at `g` when `COMMIT PREPARED` arrives there or recovery commits it.
**Resolution: a new round after a backoff (4.3 step 4).** Bounds, all settings: `av_wait_timeout`
(default 5 s for the whole statement) and `av_max_rounds` (default 20). When a bound is exceeded the
statement fails with the error of 7. Notes:

- **Only a decided transaction makes a reader wait.** A prepared transaction that is undecided is
  invisible everywhere, which is consistent; it makes no statement wait.
- A new round can reveal another transaction D' that straddles the groups, and the loop continues; the
  likelihood grows with the rate of distributed commits times the commit-phase skew and the acquisition
  span; the metric `av_rounds` shows it (section 8).
- With the origin dead and D decided, recovery commits D (every `recovery_interval`; **1 s when the
  mode is `enforce`**). If the participant is down, the statement cannot read it anyway.

### 4.6 Why it is sound (the argument, which the model must check)

Let `vis(D, g, s)` mean the row of D is visible at `g` under snapshot `s`. The validated state is
consistent when, for every D and every pair of participants in `G`, `vis` is equal. A violation is
`vis(D, g1) and not vis(D, g2)`. The argument is about **one round**: all `s_g` were taken within one
acquisition of span at most `slop`. Suppose a violation exists; D is decided before `s_g1` was taken (it
committed at `g1`), and D was prepared at `g2` before the decision.

1. **D has an xid at `g2` in `xip(s_g2)`**, so it is pending at `g2` and appears in `pending_g2`
   if it is prepared at the time of the report (no row needed) or committed after (the gap lookup, which
   needs the row: present because `g2` is ready). It is detected by the pending rule (`D` in `cands_g1`, or
   the lookup at `g1`). If instead D is *running, not yet prepared* at the report time, then its prepare,
   decision and commit at `g1` all come after the report at `g2`, hence within the acquisition span, which
   is the next case.
2. **D has no xid at `g2` yet at `s_g2` (or only a running one), or was prepared at `g2` within the span.**
   Then the prepare at `g2` is after `s_g2`, so the decision is after `s_g2`; D's row at `g1` was inserted
   at its prepare at `g1`, at most `dtx_window` before the decision (the origin enforces it in `enforce`).
   The age of that row when the report at `g1` is read is at most `span + dtx_window <= slop + dtx_window =
   Wt/2`: **D is a seed of `cands_g1`**. Its row at `g2` was inserted at the prepare at `g2`, within
   `dtx_window` of the prepare at `g1`, so its age at the report of `g2` is at most
   `Wt/2 + dtx_window + slop = Wt`: if the row were visible at `g2` it would be in `cands_g2`, so its
   absence means "not visible", and the seed rule detects the violation.
3. The symmetric pair is the same argument with the roles exchanged, because the rules quantify over
   ordered pairs.

**Preconditions**, each a mutant of the model (11): **(i)** every participant that prepared D wrote its
row, ensured by `ready` and the watermark rule (and a miss at a not-ready node is refused); **(ii)** the
origin enforces `dtx_window` (mode `enforce`); **(iii)** the proof's snapshots are of **one round** with
span at most `slop` (retake all, C1); **(iv)** ages are read from one boot with `CLOCK_BOOTTIME`, and no
node is `young` (a boot or promotion less than `Wt + dtx_window` ago: rows of an earlier boot are not
seeds, H1) or in `clock_anomaly`; **(v)** rows are read under the snapshot (MVCC), not by a fresh read, except
the gap lookup by xid which is by design; **(vi)** `Wt/2 >= dtx_window + slop`; **(vii)** `parts` holds
group ids, mapped to the current primary for the connection (H2); **(viii)** a row is never deleted while
its transaction is in doubt at any participant (M1); **(ix)** every participant, the origin included,
updates `last_prepare_mono` when it inserts a row (M7).

### 4.7 Levels of coverage

- **Level 1 (autocommit):** a covered statement outside an explicit transaction block.
- **Level 2 (read-only part of a transaction):** statements of an explicit `READ COMMITTED` transaction
  before its first write; each is validated separately (a new round), so two reads in one transaction may
  see different, each consistent, states, as `READ COMMITTED` does.
- **Level 3 (`REPEATABLE READ` first query) is NOT in this release (H4).** The local snapshot is the
  transaction snapshot, taken at parse analysis, before planning and before locks that may wait behind a DDL
  for any time; the span bound of 4.6 cannot be met, and the undetected case is exactly the one where no
  violation is seen. A later design may bound the age of the local snapshot by the statement start
  (`GetCurrentStatementStartTimestamp()` against the local clock at the hook, failing if more than `slop`)
  and fail when the local group would need a retake; that is a separate increment outside this release,
  with its own review. `REPEATABLE READ` and `SERIALIZABLE` statements run as today and `EXPLAIN` says
  `not covered: isolation level`.

### 4.8 EXPLAIN and the visible verdict

`EXPLAIN` of a statement shows one line from the custom scan:
`Atomic visibility: enforce, covered (groups: 3)`, or `... off`, `... observe`, or
`... not covered: <reason>` with the reasons of 3.4 (`transaction has written`, `single group`, `cursor`,
`user function`, `repartitioned join`, `isolation level`, `kill switch`). `EXPLAIN ANALYZE` adds `rounds`,
`waited ms`, `violations seen`. A user can therefore always see what a statement ran under.

## 5. Failure handling

| Event | Behaviour |
|---|---|
| Origin dies after prepare, before its decision | Undecided prepared transactions are invisible everywhere; no reader waits; recovery rolls them back |
| Origin dies after its decision, before `COMMIT PREPARED` | Decided: visible at the origin, pending elsewhere. Readers that include both groups wait (4.5); recovery commits within 1 s in `enforce` |
| A participant down | Its group cannot be read; statements that need it fail as today |
| Failover of a group mid-statement | The helper session dies: the statement fails, retryable (as today). `parts` holds group ids, so the participants of D in flight still map to the new primary (H2) |
| **Failover or restart inside a transaction's prepare window (H1)** | The node reports `young` for `Wt + dtx_window` after its start or promotion; covered statements that include it **wait until it is no longer young, within `av_wait_timeout`, or fail**; they never use rows of an earlier boot as "not seeds". After the young period no transaction that was in its prepare phase at the failover can be undecided without being pending |
| Failover to an asynchronous standby that lacks D's row | The incident of today (`dtx_incident`): D's part is lost; it is ignored by validation, counted, and is the stated exception of 0 |
| Instance restart | `boot_id` changes; the node is `young`; old-boot rows are not enumerated; pending transactions come from `pg_prepared_xacts`, which survives restart |
| **Recording gap (H3)** | `record` off and on, extension change, restore, operator reset: the group is **not ready**; covered statements that include it **fail** until it is ready again (the task of 8) |
| Wall clock stepped on any node | **No effect on correctness**: no wall clock is used; the harness steps clocks to prove it |
| VM freeze or suspend (M6) | `CLOCK_BOOTTIME` counts suspend; a divergence from `CLOCK_MONOTONIC` is `clock_anomaly` and fails statements that include the node |
| `dtx_window` exceeded by a prepare phase | `count` in `record` and `observe`; the origin rolls the transaction back in `enforce` |
| Acquisition span above `slop` | A new round; after the limit, error |
| Evidence missing (the function or table absent, an older version, `record` off at a participant of a seen transaction) | **Error** (7) |
| Kill switch on | Statements run as without the mode (4.8 says so) |

## 6. Performance and how it is measured

No figure from a source exists for this design; every statement below is **[Needs verification]** by
measurement, with the protocol of 6.3.

### 6.1 Costs

| Cost | Who pays | Expectation |
|---|---|---|
| One `INSERT` into `dtx_member` per participant per distributed transaction, one row of about 100 bytes, plus its garbage | writers of 2PC transactions only | one more statement in the `PREPARE` batch; no extra round trip. Measured as the throughput of the existing 2PC benchmark with `record` on and off |
| Origin's prepare-phase measurement | origin of 2PC | a clock read and a compare (`count`); a rollback only in `enforce` |
| Acquisition and report round | every covered statement | one round trip to every group in parallel before the fragments start, plus the report function: an index range scan of the rows prepared in the last 3 s (`D rate x 3 s` entries; empty when none prepared in the window) |
| Helper connections | readers | one more connection per node per backend; counted against `max_connections` of the nodes |
| Unresolved lookups | covered statements while some old transaction is pending | a second round trip; rare |
| New rounds | covered statements that overlap a distributed commit | one round trip per round **at all groups** (C1), more than a one-group retake; the probability grows with the distributed commit rate and the commit-phase skew; the metric `av_rounds` |
| Single-group statements, writers' ordinary commits | nobody | nothing |

### 6.2 What is measured

A three-group cluster; a cross-group `SELECT` at 1, 8 and 64 clients; a distributed write load of 0, 100,
1 000 and 5 000 transactions per second as background; the modes `off`, `record`, `observe`, `enforce`.
Reported: latency percentiles of the `SELECT`, the number of rounds per statement, 2PC throughput, the
size of `dtx_member` (target: a few hundred thousand rows at the highest load), the rate of errors.
**Acceptance proposed for the owner to confirm:** `observe` adds at most one round trip to a cross-group
statement and under 5 percent to 2PC throughput; `enforce` at 1 000 distributed commits per second keeps
the error rate under 0.1 percent of covered statements; single-group statements are indistinguishable
from `off`.

### 6.3 Protocol

Five runs of three minutes after a one-minute warm-up, same machine and data directory, median and spread
(min to max); a difference counts only if the spreads do not overlap; I/O-bound runs on a real disk
(never tmpfs), `pg_test_fsync` recorded.

## 7. An error rather than a fractured read: where the protocol fails the statement

**Rule:** if the protocol is not sure that the validated snapshots are consistent, the statement fails
with a retryable error and a message that says why; it does not read. Three SQLSTATEs from the next free
`KS` numbers (names here, numbers at implementation): `atomic_visibility_unavailable` (a bound was
exceeded: wait, rounds, span, `dtx_window`), `atomic_visibility_evidence_missing` (a record, function or
condition of 7.1 is missing or untrustworthy), `atomic_visibility_not_covered` (only when the session asked
for strict coverage with `kingres_shard.atomic_visibility_strict = on`: the statement could not be
covered).

| Uncertain point | Behaviour |
|---|---|
| Wait for a decided transaction exceeds `av_wait_timeout`, or `av_max_rounds` | `unavailable` |
| Acquisition span above `slop` in every round | `unavailable` |
| A report is missing or a helper session breaks | the statement fails as a failed remote session does today |
| `dtx_member`, `av_state` or `av_report` missing at a node (old version, not installed) | `evidence_missing` |
| A group is `not ready`, `young`, or in `clock_anomaly`, or reports `recording = false` | `evidence_missing` (after waiting for `young`, within the bound) |
| `boot_id` of a report is not what the node reported before in the same statement | `unavailable` |
| A monotonic value goes backward within one boot | `evidence_missing`, alert `monotonicClockAnomaly` |
| A read through a non-helper session while a covered statement is active (M5) | `evidence_missing` (a bug, caught) |
| The kill switch is on | not an error: the statement runs as today and `EXPLAIN` says so |
| Strict coverage requested and the statement is not covered | `not_covered` |

### 7.1 The conditions of the sentence of section 0

The statement "an error rather than a fractured read" holds under these conditions, which the
model, the harness and the review must establish and the operator guide must state:

1. The mode is `enforce` for the database and the kill switch is off.
2. Every group in the statement is `ready`, not `young`, without `clock_anomaly`, and recording
   (4.1).
3. The origin enforces `dtx_window` (`dtx_window_action = rollback`).
4. The statement is one round with span at most `slop` (4.3).
5. `dtx_member` rows are not deleted or truncated by hand (tampering is not detected; the setting is a
   superuser one and an audit line is written when `av_reset_ready` is used).
6. The PostgreSQL internals of 9.3 behave as listed (the table is checked on every upgrade).
7. The exception of `dtx_incident` (0).

**Until the model, the harness and an independent review have closed the model's mutants for these
conditions, the sentence is a design goal and the documentation says "designed to".**

## 8. Interfaces: REST, console, `kingres-cli`

**[Recommendation]** One contract revision (number at implementation time); the generator and console
types in the same change; the CLI command in the same change (rule of 2026-11-28; if the CLI line has not
merged, the report lists the commands).

| REST operation (new) | Purpose | `kingres-cli` |
|---|---|---|
| `getAtomicVisibility` | Per database: mode, scope, kill switch state, `ready` per group, counters (validations, rounds, waits, errors by SQLSTATE, shadow violations in `observe`), oldest decided-and-pending transaction | `kingres-cli atomic-visibility status [--database d]` |
| `setAtomicVisibility` (task) | Change the mode of a database: `off`, `record`, `observe`, `enforce`, and the scope `autocommit` or `statements`; the task checks every node (version, record on, every group ready, no old transaction pending) and says what blocks; it also runs the readiness steps of 4.1 | `kingres-cli atomic-visibility set --database d --mode enforce --scope autocommit` |
| `killAtomicVisibility`, `unkillAtomicVisibility` | Cluster-wide kill switch, effective for new statements at once (9.2) | `kingres-cli atomic-visibility kill`, `unkill` |
| Alerts (new kinds) | `atomicVisibilityWaitHigh`, `inDoubtDecidedTransaction` (critical after 60 s), `atomicVisibilityErrorsHigh`, `atomicVisibilityGroupNotReady`, `dtxWindowOverrun`, `dtxIncident`, `monotonicClockAnomaly`, `clockSkew` (information: wall clocks are not used for correctness) | `kingres-cli alerts` |
| Metrics | `kingres_av_validations_total{result}`, `kingres_av_rounds`, `kingres_av_wait_seconds`, `kingres_av_errors_total{sqlstate}`, `kingres_av_shadow_violations_total`, `kingres_av_decided_pending_age_seconds`, `kingres_dtx_member_rows`, `kingres_dtx_window_overrun_total` | `kingres-cli metrics` |

Console: a card "Atomic visibility" on the Dashboard and a tab on the Databases page (mode, scope,
kill, per group readiness, the counters, the examples of 3.3 in a help panel). `kingres-cli` prints the
same status.

## 9. Risk minimisation

### 9.1 Correctness first (the order of work)

1. **Before any code:** the model P8-M with mutants (section 11) and the differential/twin-row tests
   written against a fake store.
2. **A Jepsen-style harness (P8-T) gates every increment:** node crash (kill -9 of an instance and of
   `kingresd`), network cut between groups, **wall clock steps**, **a suspend of a node's process**
   (to test the clock rule), failover in the middle of a transaction **and inside the prepare window**
   (H1), in-doubt 2PC at every `kingres_shard_fault_point` (`after_first_prepare`, `after_prepare`,
   `after_log`, and recovery's), **recording gaps** (record off and on, restore from backup, a node added,
   an old extension version, rows deleted by hand: each must be detected or refused, H3), and a lock wait
   between snapshot and executor start. The checker is the fractured-read detector of 11 plus the
   invariants of 4.6; **an increment merges only when the harness passes on PG18 and PG19 and its negative
   controls fail on the broken builds.**
3. **An independent review per increment** by someone who did not write it, with the protocol document
   and the model as the review input; findings fixed before merge; a record in `docs/reviews/`.
4. **The rule of 7:** an error is always acceptable; a possibly fractured read is not (under the
   conditions of 7.1).

### 9.2 Honest mode, off by default, a kill switch, a staged rollout

- **Name.** The setting is `atomic_visibility` and the documentation says "atomic visibility of
  distributed transactions", never "global snapshot" or "snapshot isolation across groups".
- **Per database.** `ALTER DATABASE ... SET kingres_shard.atomic_visibility = 'off' | 'record' |
  'observe' | 'enforce'` (set by `kingresd` through the configuration of phase 13). Default `off`.
  `kingres_shard.atomic_visibility_scope` = `autocommit | statements` selects the levels of 4.7.
- **Kill switch.** `kingres_shard.atomic_visibility_kill = on` (a `SIGHUP` setting, superuser; also
  `kingres-cli atomic-visibility kill`, which sets it on every instance through `kingresd`): every
  statement that starts after the reload runs as in 0.1.0; running statements finish; `record` goes on
  (a row costs little and keeps the evidence continuous, so that switching back on is safe). Further
  levels: `record = off` per database (a gap: the group is not ready until the task restores it), and the
  documented downgrade path of the extension.
- **Staged rollout** (each stage is a setting and a gate with its metrics and harness run):
  1. `record`: rows are written; **no reader change and no change for writers beyond the insert**: a slow
     prepare phase is only counted (`dtx_window_action = count`), not rolled back (M4). Measures writer cost
     and the overrun rate. Must run at least `dtx_member_retention + Wt` before stage 2 and until every group
     is `ready` (the task refuses earlier).
  2. `observe`: **covered statements run exactly as today, on the legacy sessions** (M3); in addition the
     protocol runs as a **shadow**: it acquires and validates a set of helper snapshots before the statement
     and **counts** how many rounds and violations it needed (`av_shadow_violations_total`, `av_rounds`),
     never waiting, never failing, never changing the data the statement reads. The counters measure the
     **protocol's cost and the rate at which it would have retaken**, not the fractures of the statement's
     own data; the harness's twin-row detector measures the latter in `off` and `enforce` and must agree
     with the shadow counts in order of magnitude. A writer is still not rolled back.
  3. `enforce` with scope `autocommit` (read-only autocommit statements first). Reads come from the
     validated helper snapshots; slow prepare phases are rolled back at the origin.
  4. scope `statements` (read-only part of explicit transactions).
  The console and the operator guide show the stage and what to check before the next one.
  **Level 3 is not a stage of this release (4.7).**
- **Metrics and alerts** (section 8). A rising error rate at stage 3 or later is the signal to lower the
  stage or press the kill switch.

### 9.3 PostgreSQL internals relied on (the full list)

One table, also placed in [../dev/upstream-sync.md](../dev/upstream-sync.md) as the first thing to check on
every minor and major upgrade. Version differences are confined to `src/pg_compat.h`; the tests run on
PG18 and PG19.

| # | Internal | Where | Relied on for | How a change would show | Tests |
|---|---|---|---|---|---|
| 1 | `ExecutorStart_hook`, type `void (*)(QueryDesc *, int)` | `executor/executor.h`, `execMain.c` | taking the local snapshot and validating before the executor starts | compile error (type change); chain order with `fanout.c` | spike, twin-row test, `check_cluster` |
| 2 | `QueryDesc.snapshot` replaced in the hook (registered before returning); `es_snapshot = RegisterSnapshot(queryDesc->snapshot)`; `CreateQueryDesc` registers and `FreeQueryDesc` unregisters; `ExecInitParallelPlan` asserts the active snapshot equals `es_snapshot` | `execMain.c`, `tcop/pquery.c`, `execParallel.c` | the statement's local scan and its parallel workers read the validated snapshot | leak, Assert (cassert build), wrong visibility | cassert run of the harness; parallel-query test |
| 3 | `PushActiveSnapshot`, `PopActiveSnapshot`, `RegisterSnapshot`, `UnregisterSnapshot`, `GetLatestSnapshot`, `GetActiveSnapshot` | `utils/time/snapmgr.c`, `snapmgr.h` | candidate and active snapshot | signature or semantics change | spike; cassert |
| 4 | `SPI_execute(read_only = true)` uses the active snapshot | `executor/spi.c` | the local report under the candidate snapshot | the report reads a different snapshot: caught by the equality of the report with `pg_current_snapshot()` | unit test in the extension |
| 5 | `pg_prepared_xacts` (`transaction`, `gid`, `prepared`, `ownerid`, `dbid`) | `twophase.c` `pg_prepared_xact`, `pg_proc.dat` | mapping pending xids to transactions without a row | a changed column list | SQL regress |
| 6 | `pg_current_snapshot`, `pg_snapshot_xip`, `pg_visible_in_snapshot` | `utils/adt/xid8funcs.c` | reading `xip` and visibility in the report | changed behaviour | SQL regress |
| 7 | `SnapshotData` fields `xmin`, `xmax`, `xip`, `xcnt` (only if C reads them; the SQL functions are preferred) | `utils/snapshot.h` | optional fast path | layout change | compile |
| 8 | Prepared transactions are in the procarray and stay in `xip` until decided | `twophase.c` `MarkAsPrepared` | the pending rule | a change in how prepared transactions appear in snapshots | harness (in-doubt nemesis) |
| 9 | 2PC SQL commands and the gid being an opaque string up to 200 characters | `PREPARE TRANSACTION`, `COMMIT PREPARED` | the protocol and `dtx_id` | none expected | existing 2PC tests |
| 10 | Transaction callbacks (`RegisterXactCallback`, events `PRE_PREPARE`, `PRE_COMMIT`, `COMMIT`, `ABORT`) | `access/xact.h` | writer side hooks (already used) | event semantics change | existing tests |
| 11 | `GetTopTransactionIdIfAny`, `GetCurrentTransactionNestLevel`, `XactReadOnly`, `XactIsoLevel` | `access/xact.h` | which statements are covered | semantics | unit |
| 12 | Background worker API | `postmaster/bgworker.h` | garbage collection in the recovery worker (existing) | none expected | existing |
| 13 | `clock_gettime(CLOCK_BOOTTIME)` and `CLOCK_MONOTONIC` | POSIX / Linux | `mono_us`, spans, anomaly check | none | unit, suspend nemesis |
| Not used | `ProcArrayInstallRestoredXmin`, `SET TRANSACTION SNAPSHOT`/`pg_export_snapshot`, commit timestamps, replication origins, custom WAL resource managers, shared preload of a new library | | listed so that a reviewer knows they were considered | | |

### 9.4 Review, honesty and the documentation

The limitations guide and the operator guide carry the examples of 3.3, the staged rollout and the
conditions of 7.1. Every public statement of the guarantee is the sentence of section 0 with its status.

## 10. Upgrade and compatibility

- **No server change, no restart, no new preload library.** The feature is `kingres_shard` 0.11
  (new objects only, in new files plus small commits in the existing ones) installed by
  `ALTER EXTENSION UPDATE`.
- **Mixed versions in a rolling upgrade:** the mode of a database cannot leave `off` until every
  node reports `kingres_shard` 0.11 (`check_cluster` and the `setAtomicVisibility` task refuse). A node on
  0.10 does not write rows, which is why the mode cannot be `record` or above on a mixed cluster; **an
  upgrade or downgrade step is a recording gap** (4.1): the group is not ready until the task restores it.
  A node that lacks `av_report` makes a covered statement fail `evidence_missing`.
- **Installed 0.1.0 and 0.10 clusters:** unchanged until the operator sets a mode.
- **One source for PG18 and PG19:** `src/pg_compat.h` for any difference.
- **Contract:** the next free revision at implementation time (0.35.0 is on the phase 15 branch).

## 11. Test plan

1. **Model P8-M (first).** TLA+ checked with TLC, or a randomised interleaving model in Go with an
   invariant checker. Entities: nodes with a clock and a boot id; groups with a primary that can fail
   over; a distributed transaction with prepare at each participant (any order within `dtx_window`), a
   decision, commits in any order and with arbitrary delays; a reader with rounds of acquisitions at
   arbitrary instants with a bounded span and waits between rounds; the report (cands with window,
   pending with gap, ready, young), the validation rules, the retake-all loop, recording gaps, retention,
   incident. **Invariant:** when the statement runs (does not fail), for every D and every pair of
   participants in `G`, `vis` is equal. **Safety first, then liveness (no starvation under a bounded
   rate).** One **mutant per precondition of 4.6**, each of which the model must catch:
   (m1) check only one direction of the pair; (m2) `Wt/2 < dtx_window + slop`; (m3) no pending rule;
   (m4) pending from `pg_prepared_xacts` only (no gap lookup); (m5) the origin does not enforce
   `dtx_window`; (m6) the span is not checked; (m7) the report reads with a fresh instead of the
   snapshot's visibility; (m8) retake only the violating group and skip re-validation; (m9) the origin
   writes no row; (m10) a participant with `record` off is not detected; (m11) previous-boot rows mixed
   with the current boot; (m12) a stale `last_prepare_mono` shortcut; (m13) unresolved lookups skipped;
   **(m14) retake with a delay larger than `slop` keeping the other groups' snapshots (C1); (m15) a failover
   or restart during a transaction's prepare window with rows of the earlier boot excluded (H1); (m16)
   `parts` with node ids after a failover (H2); (m17) a recording gap (record off, restore, node added,
   old version) with a lookup miss read as "invisible" (H3); (m18) a snapshot of level 3 older than the
   statement start (H4); (m19) retention deleting a row while the transaction is in doubt at another
   participant (M1); (m20) a clock freeze or suspend between acquisitions with `CLOCK_MONOTONIC` (M6);
   (m21) the origin not updating `last_prepare_mono` (M7).**
2. **Differential tests on stock PG18 and PG19** (no kingres code needed): the twin-row test (each
   distributed transaction sets the same counter in rows of two groups; a covered statement must read
   equal counters) with the harness's own detector, in modes `off` (must fail: the anomaly is real and
   the detector sees it), `observe` (counts shadow violations), `enforce` (zero).
3. **Harness P8-T** (9.1) on the cluster tests: nemeses as listed; checker; each negative control is a
   build flag that breaks one assumption and must make the harness fail. Includes the wall clock step,
   the process suspend, the failover-in-the-prepare-window, and the **evidence missing** cases of 7.
4. **Spike P8-S** (before the protocol is built into `kingres_shard`): the snapshot swap in
   `ExecutorStart_hook` on PG18 and PG19, with cassert; the candidate **registered**; parallel query under
   the swapped snapshot (the assert of `execParallel.c`); the helper session; the report function; the
   equality of the report with `pg_current_snapshot()`; `EXPLAIN`; the **sender registry** (every call
   site of `kingres_shard_begin_xact` and `kingres_shard_get_conn` listed, including the repartitioned
   join); the `CLOCK_BOOTTIME` behaviour on the targets.
5. **Regression and TAP** of the SQL objects; **upgrade test** from 0.10 to 0.11 (a gap, then ready);
   **mixed version** refusal.
6. **Existing statements:** every cross-group test runs in `off` and `enforce` with equal results;
   pushdown paths are compared with the complete scan (ADR 0022).
7. **Bucket move interplay:** a statement that reads a group that received a bucket whose rows came from a
   distributed transaction recorded at the old group (the copy has other xids and `parts` names the old
   group): the interaction with the move switch (phase 4) is stated and tested; a statement that reads the
   new group and the other group of D is not checked for D (**limitation**, listed in 3.3 table row "A
   lost part" neighbours and in the limitations guide).
8. **Performance** (section 6) before and after each increment.
9. **Kill switch test:** under load, the switch returns to today's behaviour within one reload and no
   statement fails because of it.

## 12. Security review points

1. `av_report` and the member table are readable by the roles that already read the data (the report
   reveals transaction ids and node ids of recent distributed commits); the function is `SECURITY
   DEFINER` for the node session only (`forward_auth.c`), not callable by ordinary roles. `av_reset_ready`
   and the setting of `record` are superuser operations and are audited.
2. Helper sessions are node sessions with the same proof as remote sessions (`forward_auth.c`, `term.c`);
   they carry the term, so a superseded node's reads are refused as today.
3. Denial of service: a stuck decided transaction makes statements wait up to `av_wait_timeout` and
   then fail; the alert `inDoubtDecidedTransaction` names it; the kill switch removes the effect.
4. `dtx_member` growth is bounded by the retention and the garbage collector.
5. Tampering with `dtx_member` (deleting rows) can hide a fracture; it needs superuser rights on a
   data node and is stated as outside the guarantee (7.1).
6. No new network port, no new key, no new file; the ledger part has its own review.

## 13. Increments and order

Sizes are relative (S, M, L). Model and tests come first; **no increment merges without the harness and
an independent review** (9.1).

| # | Increment | Size | Depends on | Touches `kingres_shard` files used by other lines? |
|---|---|---|---|---|
| P8-M | The model with its mutants m1 to m21 (11.1) | S to M | none | no |
| P8-T | The harness: nemeses, twin-row detector, negative controls, wall-clock step, process suspend, gap nemeses | L | P8-M (a fake store at first) | no (cluster test code) |
| P8-S | **Spike** on stock PG18 and PG19 in a scratch extension (11.4); decision gate below | M | P8-M | no |
| P8-1 | `kingres_shard` 0.11 SQL: `dtx_member`, `av_state`, `av_report`, `mono_us`, garbage with participant confirmation; **record mode** in the prepare batch and the origin; `dtx_window` and its action; recovery interval | M | P8-S and the model clean on C1, H1 to H3 | small commits in `node_conn.c`, `recovery.c` and the 0.11 SQL: after the phase 15 and joins merges |
| P8-2 | Reader: rounds, report, validate, retake-all, helper sessions with the session role and guard, snapshot swap, **observe (shadow)** mode, metrics, EXPLAIN (new files `atomic_read.c`, `atomic_report.c`) with small hook-ins at the fragment start (`fragment_scan.c`) and in `node_conn.c` | L | P8-1 | yes, small: after the merges |
| P8-3 | **enforce**, waits, bounds, SQLSTATEs, scopes `autocommit` and `statements` | M | P8-2 | small |
| P8-4 | `kingresd`: settings, readiness task, kill switch, alerts, metrics, REST, console, `kingres-cli`, docs | M | P8-1 (the SQL interface) | no |
| P8-6 | Hardening, tuning, reviews, the operator guide chapter | M | all | |
| (later, not in this release) | Level 3 for `REPEATABLE READ` (4.7) | M | its own design and review | small |

**Beside other work in `kingres_shard`:** P8-M, P8-T, P8-S, P8-4 and the whole ledger part (14) touch none of
the files the phase 15 and join lines change. P8-1 to P8-3 wait for those lines to merge and are small
commits on top; the new code is in new files so that the merges stay easy.

**Decision gate after P8-S (all must hold):** the snapshot swap works on PG18 and PG19 with cassert and no
leak, with a registered candidate and parallel query; the report equals the helper snapshot's own content;
`EXPLAIN` behaves; **the sender registry is complete and the guard works**; the model has passed with all
mutants caught (m1 to m21); the harness catches the broken builds. If the swap is not possible without
server changes, the plan is **restricted**: only remote groups are validated and the local group is read
through a loopback helper session (slower), still without core change; if neither works, the feature is not
built and the limitation stays documented (a new ADR, the owner informed).

**P8-1 may not start until** the text of 4.3 (retake all), 4.1 (group ids, boot and promotion, readiness)
is in the model and passes: those were the findings that held back code.

## 14. Relation to the ledger part

The consistent-cut idea of the first ledger design is **dropped**: it needed a global order. The
aggregate digest keeps the statement of phase 7 (the heads are read one after another). Atomic
visibility does not change the ledger's guarantees. The ledger part ships in the same release
([phase8-multinode-ledger.md](phase8-multinode-ledger.md)); the project-run witnesses and validators
there are one trust domain, and that document and the trust file say what they prove and what they do not.

## 15. Upstream proposal (long term)

**[Recommendation]** In parallel, and not as a dependency, the project proposes to the PostgreSQL
project on `pgsql-hackers`, as a design discussion and without a patch commitment:

1. a **commit timestamp hook** (the value used for the commit record and the commit timestamp, and the
   `prepared_at` of a prepared transaction), and
2. a **statement snapshot hook** (called where `GetTransactionSnapshot` and `GetLatestSnapshot` return a
   statement snapshot, allowing an extension to return its own).

The text would cite the CSN thread (commitfest entry 4912, in which Andrey Borodin already noted that a
distributed system would need hooks), explain the use (a hybrid-logical-clock snapshot for sharding
extensions), and offer the review findings of the rejected design as the list of what such a hook must
guarantee. **If accepted in a later PostgreSQL major, causal order could be built as an extension on top
of the same evidence tables, with no fork patch**; nothing in this design depends on it, and the project
would add it by a new ADR. This is an owner action (who posts, and when). The earlier design is in git at
commits `3858bd7e0f`, `344375cc7f` and `f01ff9459a` (branch `p8-design`) and in
[phase8-timestamp-snapshot-rejected.md](phase8-timestamp-snapshot-rejected.md).

## 16. Open questions for the owner (recommended default)

1. **`dtx_window` (default 1 s)** and the rule that the origin rolls back a transaction whose prepare
   phase is longer **in `enforce` only** (`record` and `observe` only count). Default: yes; the answer
   "writers fail rather than readers" is conditional on the overrun rate measured in `record`.
2. **Retention and window defaults** (`dtx_member_retention` 60 s, `slop` 500 ms, `av_wait_timeout` 5 s,
   `av_max_rounds` 20). Default: as stated.
3. **Posting the upstream proposal** (15): who and when. Default: after P8-S, so that the proposal can
   cite measurements.
4. **Acceptance numbers of 6.2.** Default: as proposed.
5. **Level 3 (`REPEATABLE READ`)** is **out of this release** (4.7); building it later is a separate
   decision. Default: out.

Answered by the owner on 2026-11-28 and no longer open: core patches (declined); both parts in one
release including the notary; the project's own test witnesses and validators; per-database enabling and
a name that says what it gives.

## 17. The rejected alternative: timestamp snapshot with core hooks

The first design of this part (commits `3858bd7e0f`, `344375cc7f`, `f01ff9459a`) built a hybrid-clock
**timestamp snapshot** (Clock-SI style) from two core hooks (a commit/prepare timestamp hook and a
snapshot hook). It gave more (a consistent cut, causality through a carried timestamp) and was **declined
by the owner** because it requires patching the server. It was reviewed twice by an independent reviewer
(4 critical, 8 high, 9 medium, 4 low, then 1 critical, 3 high, 6 medium, 3 low); every finding was answered
in that document. It is kept in [phase8-timestamp-snapshot-rejected.md](phase8-timestamp-snapshot-rejected.md)
as the record of what the stronger guarantee would need. Lessons carried into this design: the **model and
differential tests first**; **an error rather than a wrong answer**; the guard against refusing after a
durable decision (here: the origin's prepare bound is checked **before** the decision, never after); and
the habit of testing every ordering assumption with a mutant.

| Earlier finding class | In this design |
|---|---|
| Timestamp draw, observe, publish/draw barriers (C1 to C3, N1, N2) | Not applicable (no timestamps) |
| Snapshot hook and reuse path (C4) | Not applicable: no hook; the snapshot is replaced in `ExecutorStart_hook` (spike, 9.3 rows 1 to 3) |
| Prepared wait per snapshot (H3, N6) | Only **decided** transactions make a reader wait, and only readers of both groups (4.5) |
| Horizon, keeper, vacuum (H1, N7) | Not applicable: snapshots are fresh |
| Clock limit and promotion (H4, N3), bounded recency (N4), tokens (N2) | Not applicable (no clock); the causal guarantee is not given; promotion appears here as `young` (H1 of the review of this design) |
| Correctness criterion in the gate (H8) | Kept and made first: the model and the differential tests (11) |

## 18. Sources

Fetched or searched on 2026-11-28: commitfest entry 4912 and its thread; the CockroachDB HLC package
page and blog, Jepsen's analyses; the YugabyteDB read restart page; the HLC paper (these are the
background of the rejected alternative). PostgreSQL internals: read in the two trees as cited in 1.2 and
9.3. Background, to check before ADR 0030 is accepted: the Spanner and Clock-SI papers (for the contrast
only); the RAMP paper (Bailis et al., "Scalable Atomic Visibility with RAMP Transactions", SIGMOD 2014) as
the name and idea of "atomic visibility" without a global order (**[Needs verification]** that the term
fits); `clock_gettime(2)` for `CLOCK_BOOTTIME` and `CLOCK_MONOTONIC` behaviour across suspend.

## 19. Review of 4807f69c96 and where each finding is answered

An independent review of the first version of this plan (1 critical, 4 high, 9 medium, 5 low) found the
argument sound for one acquisition round and unsound where the implementation leaves that round. It
allowed the model, the harness and the spike to start, and held P8-1 and later until C1 and H1 to H3 were
in the text and the model. Each finding:

| Finding | Answer | Where |
|---|---|---|
| C1 retake keeping old snapshots breaks the span bound | After any wait, **all groups are retaken in one fresh parallel acquisition**; the proof is about one round; mutant m14 | 4.3 steps 1 and 4, 4.6, 11.1 |
| H1 rows of a previous boot dropped from seeds | A node is `young` for `Wt + dtx_window` after boot or promotion; covered statements wait for it or fail; rows of other boots are never treated as non-seeds; mutants m11, m15 | 4.2, 4.3 step 2, 5, 4.6 (iv) |
| H2 node ids in `parts` | `parts` holds **group ids**, mapped to the current primary only for the connection; mutant m16 | 4.1, 4.3 |
| H3 recording gaps | A durable per-group `av_state.ready`, with the gap events (off and on, version change, restore, node added, reset), restored only when no older prepared transaction exists anywhere and `Wt` has passed; a lookup miss is trusted only at a ready node, a not-ready node fails the statement; `record = off` refused while `observe` or `enforce`; harness nemeses for the cases; tampering stated as outside; mutants m10, m17 | 4.1, 4.3, 7, 7.1, 9.1, 11 |
| H4 level 3 cannot meet the span bound | **Level 3 is dropped from this release**; a later design is sketched; mutant m18 | 3.4, 4.7, 13 |
| M1 retention deletes needed evidence | Delete only if old, not prepared here, and every other participant confirms it has no prepared transaction with that id; retention 60 s; mutant m19 | 4.1 Garbage |
| M2 `dtx_incident` vs the sentence | The exception is stated in 0 and 3.3 | 0, 3.3, 5 |
| M3 `observe` semantics | `observe` runs statements on the legacy sessions and a **shadow** protocol that counts shadow violations and rounds, not the fractures of the statement's data | 9.2 |
| M4 `record` is not passive | The rollback is a setting (`dtx_window_action`): `count` in `record` and `observe`, `rollback` only in `enforce`; the answer to the open question is conditional | 4.1, 9.2, 16 |
| M5 every sender through the helper | A session role in `kingres_shard_get_conn`/`kingres_shard_begin_xact`, a runtime guard, a build-time registry, the fragment start named; repartitioned joins not covered; the registry is a spike gate item | 4.4, 3.4, 13 |
| M6 `CLOCK_MONOTONIC` across suspend | `CLOCK_BOOTTIME` for spans and ages, compared with `CLOCK_MONOTONIC`, divergence is `clock_anomaly`; a suspend nemesis; mutant m20 | 1.2 item 9, 4.1, 5, 9.1 |
| M7 origin and `last_prepare_mono` | The origin updates it when it inserts its row; mutant m21 | 4.1, 4.6 (ix) |
| M8 table size | Retention 60 s, sizes stated, checked in the rollout | 4.1, 6.2 |
| M9 project-run validators | Ledger design: what they prove and do not, in the guarantee table and the trust file | [phase8-multinode-ledger.md](phase8-multinode-ledger.md) 18 |
| Low: top-level statements only | Added to 3.4 | 3.4 |
| Low: helper connections | Counted | 4.4, 6.1 |
| Low: candidate registered, not only pushed (`execParallel.c:740`) | Verified in both trees; stated; a spike check | 1.2 item 1, 4.4, 11.4 |
| Low: bucket move interplay | Stated and tested | 11.7 |
| Low: ADR consistency | ADRs updated | ADRs 0030 to 0034 |
| "never a wrong answer" wording | Weakened to "designed to", with the conditions of 7.1, restored only when they are established | 0, 7, 7.1 |
| Mutants m14 to m21 | Added | 11.1 |
