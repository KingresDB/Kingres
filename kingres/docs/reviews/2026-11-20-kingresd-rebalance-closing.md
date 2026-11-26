<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->

# Closing review of phase 4 (bucket movement and rebalancing)

Commit reviewed: `1adba64515` (tip of the 18 branch). It contains the fixes `191bba73ee` (ST-01 to ST-04) and `f895d8fb26`
(ST-05 to ST-10) for `2026-11-17-kingresd-stabilisation-and-rebalance-second-fixes.md`, and all later work: failover
increments 3 to 7, switchover, phase 2b steps 1 to 13, phase 9 up to 9.4.
Reviewer: an independent reviewer, not the author. Paths are repository-relative; `cp/` is `kingres/control-plane/`.
Earlier reviews: `2026-11-16-kingresd-rebalance.md`, `2026-11-16-kingresd-rebalance-fixes.md`, and the one above.

## Scope and method

**Read.** The three reports; `docs/design/shard-phase4.md`; the rows on moves in `docs/design/ha-phase5-part2.md` (4.5, B7, T9);
the phase 4 row of `kingres/README.md`; the work log since 2026-11-17; the control plane README (rebalancing, "Review ST-01 to
ST-13"); `cp/internal/rebalance` (`pg.go`, `loop.go`, `guard.go`, `retry.go`, `service.go`, `mapgroup.go`),
`cp/internal/lease` (`manager.go`, `watchdogfile.go`), `cp/cmd/kingres-watchdog`, the lifecycle lock in `cp/internal/instance`,
`cp/internal/peerclient`, `cp/internal/pgconfig` (`converge.go`, `agent.go`), `cp/internal/auth/retry.go`, the task engine's gate and
step loop, migrations 0009, 0014, 0016, `cp/internal/httpapi/handlers_rebalance.go`, `cp/cmd/kingresd/autofailover.go` and
`mapfailover.go`; in the extension the diff of `8fa976c577` in `bucket_move.c` and the 0.7 script (`internal_begin_move`, the table
operation claim, `internal_move_alive`).

**Run.** Everything on my own copies, in a detached worktree, with private PostgreSQL installs copied from the shared ones
(nothing was installed into a shared install); the Go tests that start instances used ports 59000 to 59099.
- Extension TAP tests of moves (`025` to `031`, `033` to `037`, `054`) through `ext-majors.sh kingres_shard` (clean build with
  `-Werror`, install, tap) on PostgreSQL 18.7 and 19rc2.
- Go unit tests, with a real PostgreSQL 18, of `rebalance`, `lease`, `task`, `standby`, `pgconfig`, `peerclient`, `auth`,
  `instance`, `internalapi`, `cmd/kingres-watchdog`, `sysdb`, `httpapi`.
- 17 one-line mutants of the third round's fixes (clean copy each, the tests of the touched package).
- Cluster tests: **not run** (see "Not covered").

## Summary

Counts: critical 0, high 0, medium 1, low 4, note 3 (8 in all).

| Id | Severity | Summary |
|---|---|---|
| P4D-01 | medium | The test the design names for the one interplay that can break a move (`TestFailoverDuringBucketMove`, T9) was never written; no cluster test runs a failover, a switchover or a map group failover with a move in flight |
| P4D-02 | low | Both arms of the ST-02 fix are unpinned: mutants "an error of the running-check means not running" and "the observation session is not reopened" both survive; the second one, by reading, still ends in the cancel of a recorded move after 5 minutes |
| P4D-03 | low | The fallback match of a row without a mark (rows older than migration 0012, or a crash between the row and its mark) can adopt the record of an earlier attempt of the same bucket within 10 s (by reading) |
| P4D-04 | low | Finished tasks are still never pruned (ST-08, second half): stated open in the README, not closed |
| P4D-05 | low | The fix of `/config/restart` (`ifPending`) is pinned only by a cluster test; the mutant survives every unit test |
| P4D-06 | note | The README row says the fixes "have not been reviewed again"; with this report that sentence must change, and the row does not say how a move meets a failover (it is stopped first, a manual failover or switchover is refused while a rebalance runs) |
| P4D-07 | note | A bucket move started by hand (not by a rebalance) is not looked at by the failover's preflight; the extension's own recovery is what settles it |
| P4D-08 | note | `RunMove` cancels the call when the run's context ends without one last look at the record (a window of one poll, 100 ms) |

No finding is a lost or doubled row, a bucket with two owners or none, a move that never ends, a rebalance run by two leaders, or
a fenced holder that writes. I found none, and the places where one could arise held (see "What held").

## Findings

### P4D-01 (medium) - no cluster test of a failover with a move in flight

**Place.** `docs/design/ha-phase5-part2.md` row T9 names `TestFailoverDuringBucketMove` (a rebalance moves a bucket; the server
of the source group is killed in each phase `preparing`, `copying`, `catching_up`, `switching`, `cleaning`; the automatic failover
stops the rebalance first, the extension settles the move, `check_cluster()` is empty, the rebalance can be started again; a move that
cannot stop for 5 s makes the failover wait and raise `failoverBlocked`). `grep -rn FailoverDuringBucketMove` finds only the design.
`cp/clustertest/failover_auto_test.go` mentions the rebalance in a comment only. The pieces exist and are tested alone:
`rebalance.StopForFailover`/`WaitStopped` and `PauseForMapGroupFailover` against a real system database
(`failover_pg_test.go`, `mapgroup_pg_test.go`, `failover_exclusion_pg_test.go`), the migration trigger KX001/KX002, and, in the
extension, `054_failover_move.pl` (passes on both majors, see below). `autoPorts.StopRebalance` has a fake in `failover/auto_test.go`.
**Why it matters.** This is the only place where phase 4's own guarantees (no move left in `switching`, no prepared transaction left,
a rebalance that can be started again) meet code written after the last phase 4 review. By reading it is right (the loop reads the
stop flag at every poll, a move that is switching is waited for, the run ends `paused`, a paused rebalance does not exclude the
failover, the extension's recovery settles the move when the rebalance is started again), but nothing runs it end to end.
**A fix must achieve.** The cluster test of T9 for at least `copying` and `switching` (the phases are held with
`rebalance_faults_test.go`'s mechanism), plus one for a map group failover with a move recorded after the standby's replay point.

### P4D-02 (low) - the two arms of the ST-02 fix are not pinned by a test

**Place.** `cp/internal/rebalance/pg.go` `RunMove`: `reopen()` after a failed `findMove`, and `running = true` after a failed
`callRunning`.
**Shown (mutants, unit tests of `internal/rebalance`, PostgreSQL 18).**
- M04: `running = true` changed to `running = false` in the error branch of `callRunning`: **all tests pass** (survives).
- M05b: the failed `findMove` no longer reopens the observation session: **all tests pass** (survives).
`TestABrokenObservationSessionDoesNotCancelARecordedMove` passes with both broken: it ends because the call returns, not
because the loop saw the record.
**Why it matters.** By reading the code is right today. The first arm is the cancel that ST-02 was about. The second one matters more
than it looks: without the reopen, a dead observation session never sees the record, the guard is never released (the task cannot be
claimed by another server for as long as the move runs), and when `maxCallWait` (5 minutes) is reached `RunMove` withdraws, which
cancels a call whose move the extension has recorded (possibly in `switching`). So the 5 minute bound turns a broken observation
session into the very cancel the fix removed.
**A fix must achieve.** A test with a terminated observation session and a move that outlasts `hold`, asserting that the record is
seen (guard released) and that the call is not cancelled; a test with a `callRunning` that returns an error. Consider that the
`maxCallWait` withdrawal should not apply once the observation is known to have failed.

### P4D-03 (low) - the time fallback can adopt an earlier attempt's record (by reading)

**Place.** `pg.go` `findMove` (default branch, `started_at >= since - 10 s`), used by `Recover` for a row with no mark.
**What.** A row has no mark if it is older than migration 0012 or if the holder died between `StartMove` and `SetMark` (milliseconds).
For such a row, a record of the same bucket that the extension wrote up to 10 s before the row was started matches. A failed attempt
that was made seconds before (the retry backoff is 30 s times the failures, so it is normally longer) would be taken as the result
of the new attempt: the row is settled `failed` with the old error and counts an attempt without a call having been made. Only an
attempt is lost; no data is touched, and the `done` case needs the same bucket moved twice within 10 s, which the planner does not do.
**A fix must achieve.** Write the mark in the same statement as `StartMove` (the mark is `max(move_id)`, readable first), so that the
time path exists only for rows of the old format.

### P4D-04 (low) - nothing prunes finished tasks

The retry back-off (`retryWait`, 1, 2, 4, 8 then 10 minutes) halves the growth of ST-08 (mutant M12 killed by
`TestRetryWaitGrowsToTenMinutes`), but a server that never applies a configuration still costs a task every 10 minutes, about 144 a
day, and no code deletes tasks. The README and the commit say so ("open"). It is not a phase 4 defect, it is the other half of ST-08
and stays open until retention is decided.

### P4D-05 (low) - the `ifPending` fix is pinned by a cluster test only

M16 (`ifPending && !restartNeeded` replaced by `false`, `cp/internal/pgconfig/agent.go`): the unit tests of `internal/pgconfig` pass.
The test that pins it, `TestARepeatedRestartCallRestartsOnlyWhatIsPending`, is a cluster test, which I could not run on the mutant
(see "Not covered"). The same applies to the rebuild-generation arm of ST-03 (an agent over a fake `pg_ctl`; I ran it unmutated and
it passes, I did not mutate it).

### P4D-06 (note) - the README row

The phase 4 row says the fixes of ST-01 to ST-10 "have not been reviewed again". This report reviews them; the row must say so, and
what remains (P4D-01 to P4D-05) in one sentence. The row's statements about what is built are true: one move at a time (the contract
says `maxConcurrentMoves` is accepted and recorded and does not run moves together), a plan by bucket count, stop and start again,
takeover after a restart or leader change (cluster tests of the earlier rounds name them), a report per run, the page in the
panel (`web/src/api`). It does not say that a rebalance and a failover exclude each other (KX001 and KX002), that an automatic
failover stops the rebalance first (`StopForFailover`, `safetyRule`), and that a manual failover or a switchover is refused while a
rebalance runs.

### P4D-07 (note) - a move started by hand and the failover preflight

`cp/cmd/kingresd/failover.go` `RebalanceRunning` looks only at tasks of kind `rebalance`. A live move that a person started with
`kingres.move_bucket` is not seen by the preflight of a manual failover or switchover. The extension protects the data (054 and the
move worker), so this is a question of the operator's experience, not of safety.

### P4D-08 (note) - the last look before the cancel

In `RunMove`, `case <-ctxDone:` cancels the call at once. If the extension recorded the move in the last 100 ms (one poll) the call
is cancelled in its first phase. That phase is undone by the extension's own cancel path, and `switching` cannot be reached in so short
a time, so no state is left behind. Reading the record once more before the cancel would close it.

## Fate of the findings of the third review

| Id | Fate | Evidence |
|---|---|---|
| ST-01 (high) | **fixed, holds.** The final design is not the one the commit message names: a group that is not enforced keeps its file fresh (`writeWatchdogIdle`, called from `Renew` and from a loop of its own, `touchIdle`, every `min(renew, 1 s)`), so a dead or hung kingresd still ends the primary and a live one never does | unit tests pass; mutants M01 (idle write off), M02 (`touchIdle` off) and M03 (idle write in `Renew` off) are each killed by a lease test. The cluster test `TestSwitchingEnforcementOnAndOffNeverFencesAHealthyPrimary` was not run by me |
| ST-02 (high) | **fixed by reading; the tests do not pin it** | P4D-02 |
| ST-03 | fixed: `clientTimedOut` (M15 killed), `ifPending` (M16 survives units, P4D-05), rebuild generation (test passes) | |
| ST-04 | fixed: the lifecycle lock is a channel left on the context (M14 killed), the fence does not wait for `m.op` and a start that met a fence is stopped again (M17 killed) | |
| ST-05 | fixed: the mark is kept with the row (migration 0012); `Recover` uses it. M07 (ignore the mark), M10 (no tolerance) and M11 (mark not written) are killed. Residual: P4D-03 | |
| ST-06 | fixed: M08 (no check before the insert) and M09 (no `NOT EXISTS` in `AddSkipped`) are killed | |
| ST-07 | fixed: a live move is waited for beyond `RecoverWait`; M06b (no extension of the deadline while alive) is killed by `TestRecoverWaitsForALiveMoveBeyondTheBound` | |
| ST-08 | half fixed: back-off yes, pruning no (P4D-04) | |
| ST-09 | fixed in the parts named | |
| ST-10 | fixed: `RetryingLimited` with eight waiters on the join call only; M13 killed | |

The earlier findings (RB-01 to RB-14, RBR-01 to RBR-07): the tests pinning them pass (the whole `internal/rebalance` and
`internal/task` packages pass with a real PostgreSQL), and the guard, the fence token, the `UndoStart`, the three-withdrawals bound and
the stop-while-waiting path read as before. I did not repeat their real-cluster runs.

## What held

- **Extension, moves.** `t/025` to `t/031`, `t/033` to `t/037` and `t/054_failover_move.pl`: 13 files, 972 tests, PASS on PostgreSQL
  18.7 and on 19rc2 (clean build with `-Werror`, no warnings). They cover readers and writers during a move, faults at each phase,
  the switch stalled, the move worker, transactions, `check_cluster`, and a failover in the middle of a move.
- **A move and a table operation (phase 9).** `internal_begin_move` takes `LOCK TABLE kingres.map_version IN EXCLUSIVE MODE` before it
  reads `table_operation_lock`, and the online operation takes the same lock before it reads `bucket_move` and inserts its claim, so
  the two cannot both pass their check; `kingres_shard_assert_no_move` (the 0.7 diff) refuses statements that change the tables of a
  group that a move or an operation uses. The 0.7 change to `internal_switch_bucket` (bucket modulus) equals the bucket count today.
- **A rebalance that two leaders run.** The claim skips a task whose guard (shared advisory lock) is held; the guard is proved against
  holder, epoch, state and expiry; `RunMove` makes no call without it; the task engine's gate (failover increment 2) is read only at step
  boundaries and does not touch the long rebalance step, whose end is decided by the lease guard and the fence.
- **Failover and a rebalance.** Migration 0016 keeps both exclusions (a rebalance cannot start while a `server.failover` or
  `restorePreferredPrimaries` task is unfinished, and the reverse); switchover is the same task kind in mode graceful, so it is excluded
  the same way; `PauseForMapGroupFailover` pauses the run with the safety rule and leaves the move to the extension's recovery;
  `Recover` waits for a live move and settles the rest.
- **The watchdog and a move.** A move does not change the lease or the file; the watchdog is told by the file only. A group without a
  standby, which a rebalance can still move buckets away from, is no longer fenced by a stale file (ST-01).
- **Tests of the round.** All unit packages listed above pass, except `internal/httpapi`, whose integration tests need
  `kingres/scripts/fixture_roles.sql` at a path my copy did not have (a property of my setup, not of the tree).

## Mutants

| Mutant | Place | Result |
|---|---|---|
| M01 idle watchdog write returns at once | `lease/watchdogfile.go` | killed (3 tests) |
| M02 `touchIdle` never writes | `lease/manager.go` | killed |
| M03 `Renew` does not write the idle file | `lease/manager.go` | killed |
| M04 an error of `callRunning` means not running | `rebalance/pg.go` | **survives** (P4D-02) |
| M05b no reopen of the observation session | `rebalance/pg.go` | **survives** (P4D-02) |
| M06b a live move does not extend the recover deadline | `rebalance/pg.go` | killed |
| M07 `Recover` ignores the mark | `rebalance/pg.go` | killed |
| M08 `StartMove` does not look for the row of the attempt | `rebalance/pg.go` | killed |
| M09 `AddSkipped` does not check for a duplicate | `rebalance/pg.go` | killed |
| M10 no tolerance in the time path | `rebalance/pg.go` | killed |
| M11 the mark is not written | `rebalance/loop.go` | killed |
| M12 the retry wait does not grow | `pgconfig/converge.go` | killed |
| M13 the join cap is far above eight | `auth/retry.go` | killed |
| M14 the lifecycle lock ignores the context | `instance/manager.go` | killed |
| M15 a client timeout is repeated | `peerclient/peerclient.go` | killed |
| M16 `ifPending` ignored | `pgconfig/agent.go` | **survives units** (P4D-05) |
| M17 the start does not check for a fence that arrived | `lease/manager.go` | killed |

M05 and M06 as first written did not compile (an unused variable); they were redone as M05b and M06b. 14 of 17 killed; the three
survivors are named above.

## Not covered

- **Cluster tests.** I queued `run-integration.ps1 -Packages ./internal/clustertest -Run 'Rebalance|MoveAlive|RemovingAServer|LowerReplica|Enforc'`
  at the start. The machine-wide mutex was held by other runs (one after the other) for the whole session, about two hours, and mine never started;
  I withdrew it. So the cluster tests of rebalance, takeover, the extension's `internal_move_alive` through a real kingresd, the
  real removal of a server under load (ST-01's first trigger) and the repeats of ST-03 were not run by me. They passed in the integrator's
  complete runs recorded in the commit messages (PostgreSQL 18: 919 passed, none failed; 19rc2: two failed, one of them
  `TestInstanceMappingAfterAHandFailover`, a failover test whose cause is open, not a move test).
- No move was run by me under the failover code of increments 4 to 7 (P4D-01).
- The extension was reviewed through its tests and the diff of the later changes, not line by line again; the move code
  (`bucket_move.c`) was last changed on 2026-11-19 and reviewed in the earlier phase 2b and 9 rounds.
- Phase 2b pushdown under moves: not repeated here (the step 13 report ran 5120 statements per major before and after moves).
- `internal/httpapi` integration tests (path of the fixture in my copy).
- Windows-specific paths, packaging, the panel in a browser.

## Judgement

**Can phase 4 be called done?** Yes, with one condition. Nothing found threatens a row, an owner, the end of a move, the single
leader of a rebalance or the fence; the two high findings of the third review are fixed (ST-01 holds and is pinned by unit tests
that fail when it is broken; ST-02 is right by reading), the extension passes its 972 move tests on both majors, and the later
failover, switchover, phase 2b and phase 9 work did not break a guarantee of a move by reading and by those tests.

What remains before the word "done" is exact and small:
1. P4D-01: write the cluster test of a failover (and a map group failover) with a move in flight, at least for `copying` and
   `switching`, the one test of the design's table that is missing.
2. P4D-02: two tests that fail without the two arms of the ST-02 fix (a killed observation session on a move longer than the hold,
   and a failing `callRunning`), and a decision whether the 5 minute `maxCallWait` may withdraw a call whose observation failed.
3. P4D-06: change the README row (the fixes are now reviewed; name what stays open: P4D-01 to P4D-05, and pruning of tasks).
Items P4D-03, 04, 05, 07 and 08 may stay open and be listed as such. The cluster tests I could not run (see "Not covered") should be
run once on the commit that closes these items.
