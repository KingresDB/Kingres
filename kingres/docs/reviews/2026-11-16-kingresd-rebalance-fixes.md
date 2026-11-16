<!-- SPDX-License-Identifier: AGPL-3.0-only -->

# Independent review of the fix round for the rebalancing review of kingresd

Commit reviewed: `046b0baa52` (the fix round). Code before the fix:
`2aba6ea01e`. First review: `2026-11-16-kingresd-rebalance.md`, written for
`5196e3da5a`. Reviewer: an independent reviewer, not the author of the code.
Paths are repository-relative; `cp/` abbreviates `kingres/control-plane/`.

## Scope and method

**Read.** An export of the commit (`git archive` of `kingres` and
`contrib/kingres_shard/README.md`) in a scratch directory outside the
repository; the working tree was not used. Read closely: the whole diff of
the commit; `cp/internal/rebalance/` (guard.go, loop.go, pg.go, service.go),
the task engine (`task.go`, `pgstore.go`, `memstore.go`, `record.go`),
`fence/fence.go`, migrations 0008, 0009 and 0010, `ops/servergroups.go`
(`UpdateServer`), `membership/facts.go`, `membership/add.go`,
`cmd/kingresd/internal.go` (`runEngineOnLeader`), the README of the control
plane ("Rebalancing after the review"), and the extension's
`internal_begin_move` and its README on moves and recovery.

**Run.** Unit tests on Windows (`go test` of `internal/task` and
`internal/rebalance`, vitest of the panel with the export's `node_modules`
pointing at the repository's installed packages). Integration tests on
PostgreSQL 18 in WSL through `scripts/run-integration.ps1`, selected tests
only: the exclusion and resume tests, the guard tests and four throw-away
tests of mine (`review_rbr_test.go`, `review_rbr_fake_test.go`, kept only in
the export). The "fails without the fix" runs were made by reverting one fix
at a time in a copy of the export and restoring it afterwards. No test of
`internal/clustertest` (three real servers) was run: see "Not covered".

Each finding says how it was shown.

## Summary

Counts: critical 0, high 0, medium 2, low 3, note 2 (7 in all).

| Id | Severity | Summary |
|---|---|---|
| RBR-01 | medium | A record that arrives later than the 30 s guard hold cancels the call and burns an attempt; a lock wait on the cluster lock is enough (stub of the extension) |
| RBR-02 | medium | A transient error of the system database (not a lost lease) in the middle of a move abandons the move in flight; the task fails and the hook marks the live move "interrupted" (fake rows) |
| RBR-03 | low | Progress and report counters mix rounds: a finished run shows 60 percent and a failed move that was later done (fake rows) |
| RBR-04 | low | The guard blocks `ALTER TABLE`, `DELETE` and `FOR UPDATE` of the task row for its whole hold; a guard void for another reason than a lost lease ends the run as "lease lost" with a valid lease |
| RBR-05 | low | `findMove` matches a move by the extension's clock against this server's clock minus 10 s (by reading) |
| RBR-06 | note | A rebalance excludes a failover for as long as it runs |
| RBR-07 | note | The quiet end has no log line that says why, and a fence that never clears loops silently |

No finding is critical or high: in every case the extension's rules (one
move per bucket, recovery worker, `resume_bucket_moves` never touching a live
session) still stand between the control plane's mistake and the data, which
stayed correct in every run.

## Fate of the first review's findings

"Fails without" says whether I reverted the fix myself and saw the fix's own
test fail. "Not repeated" means the test passes with the fix and I did not
revert it.

| Id | Fate | Evidence | Fails without (mine) |
|---|---|---|---|
| RB-01 | fixed | `TestSupersededHolderRunsNoHookWhateverTheStepError`: three step errors (plain, lease, fenced) end in `ErrLeaseLost`, no hook, row of the new holder untouched | yes: "OnFail ran 1 times" x3 |
| RB-02 | fixed | `TestExpiredUnclaimedLeaseIsClaimedAgainNotFailed`: stays running, claimed again, epoch 2, step run twice | yes: first run returns nil, task failed |
| RB-03 | fixed, remainder as stated | `guard_pg_test.go` (4 tests) pass on PG 18; the guard stops the claim (skipped row) and does not block renew, cancel flag, state change | not repeated |
| RB-04 | fixed | a non-statement error goes to `Recover` first; `RunMove` cancels the backend on return (see Not reproduced) | not repeated |
| RB-05 | fixed | `inspect` retries four times, then a safety stop with a fixed text | not repeated |
| RB-06 | fixed | `TestNonConvergingPlanStopsTheRun` | yes: does not end (test ran 98 s, FAIL) |
| RB-07 | fixed | `TestStartAgainBeginsANewRound` | yes: verdict failed, owners unchanged |
| RB-08 | fixed | `Cancellable` true for paused; hook sees the recorded state; outcome `cancelled`; `TestCancelOfAStoppedRebalanceIsNotAFailure` passes on PG 18 | yes for `Cancellable` ("a paused task must say that it can be cancelled") |
| RB-09 | fixed | `asProblem` has a fixed sentence; `Check` names only `check_name` and node | not repeated |
| RB-10 | fixed | three PG tests (both orders, and resume) pass | yes: "the second start never waited for the lock" x2, "resume: <nil>, want ExcludedError" |
| RB-11 | fixed | `TestResumeRunBeginsARoundAndTakesTheNewOptions` | yes: options stayed `[shop]` |
| RB-12 | fixed | `defaultMaxConcurrentMoves = 1` and the schema text agree | not repeated |
| RB-13 | fixed | test "survives an outcome it does not know and a bucket that moved twice" | yes: TypeError, "reading 'cls'" |
| RB-14 | partly | (d) the duplicate audit is gone; (a), (b), (c) left as they were and listed as open | not repeated |

## Findings

### RBR-01 (medium) - a slow record cancels the call and burns an attempt

**Where.** `cp/internal/rebalance/pg.go` `RunMove` (the `hold.C` branch, 30 s,
`guardHold`), `loop.go` `execute` (the error is neither a `MoveError` nor
`ErrLeaseLost`, so it goes to `Recover`), `pg.go` `Recover` ("no record").
Extension: `internal_begin_move` starts with `LOCK TABLE kingres.map_version
IN EXCLUSIVE MODE`, before the move is recorded.

**What happens.** The guard is held until the extension has recorded the
move, at most 30 s. If the call waits for the cluster lock (another session
of the extension holds it: a long `add_node`, a distribution of a large
table, another move being recorded) or for a slow node for more than 30 s,
`RunMove` cancels the call. The error is a plain error, so the loop asks
`Recover`, which answers "no record" at once and the move is recorded
`failed` with an attempt used. Three such waits give a bucket up and the run
ends `failed` with an alert, though nothing was wrong with the data. Before
the fix the call simply waited.

**Shown** (PG 18, `TestRBRSlowRecordCancelsTheCall`, a stub of
`kingres.move_bucket` that records after 6 s, `GuardHold` 2 s):
```
RunMove after 2.013s: *errors.errorString the extension did not record the
  move within 2s; the call was cancelled
Recover: state="failed" text="the move had not been recorded by the
  extension when the run was interrupted" err=<nil>
```
That a real lock wait does the same is by reading of `internal_begin_move`;
I did not hold the lock on a real cluster.

**Expected.** A cancelled wait is not an attempt at the move: retry without
counting it, or record a skip such as "cluster busy", or extend the hold
while the backend waits on a lock. **Direction.** Raise the hold to a value
above the extension's `node_timeout` (60 s), or ask `pg_stat_activity` for a
lock wait of the call and keep waiting.

### RBR-02 (medium) - a blip of the system database strands a live move

**Where.** `loop.go` `execute`: `UpdateMove`, `Rows.Run` and `FinishMove`
errors are returned at once while `RunMove` runs in its goroutine;
`task.go` `run` then takes the failure branch (`leaseLost` is false for a
connection error); `service.go` `onFail` marks every `running` move
`failed` ("interrupted") and raises `taskFailed`.

**What happens.** RB-05's retry covers `Databases`, `Check` and `Plan`, not
the writes of the loop. One refused connection to the system database during
a move (restart, failover, connection limit) ends the step with that error.
The call to the extension is not cancelled by the step ending (the engine
cancels its context only after `fail` returned), so the move runs on and
succeeds. The report says the move failed, the task is `failed` (a start
again is then a new run, not a resume), and an alert is raised.

**Shown** (`TestRBRSysdbBlipMidMove`, fake rows whose first `UpdateMove`
fails with a connection error, a blocked fake move):
```
Moves returned: failed to connect to host=... dial error (connection refused)
move rows: [{Seq:1 ... State:running Attempts:1 ...}]
move_bucket calls so far [0]; the call has not returned
after the move finished by itself: owner of bucket 0 = 2 (the target is 2);
  row state recorded by the run: running
```
The hook's marking of the row is by reading of `onFail` (not run here).

**Expected.** A transient error of the system database pauses the run (safety
stop) after a bounded retry, as for checks, and waits for the move in flight.
**Direction.** Retry the loop's own writes with the same `inspect` rule, and
on giving up wait for the in-flight call through `Recover` before returning.

### RBR-03 (low) - counters mix rounds

**Where.** `service.go` `progressOf` (counts every row of the task,
`MovesTotal = max(planned, len(moves))`) and `summarySQL` (counts rows of all
rounds).

**Shown** (`TestRBRProgressAfterAStartAgain`: one bucket exhausted and one
skipped in round 0, both repaired and moved in round 1):
```
rows=5 planned=3  progress: done=3 failed=1 skipped=1 total=5 percent=60
verdict: {Outcome:completed Failed:0 Skipped:1 Done:3}
```
A run in which every bucket moved shows 60 percent while it runs, a
`movesFailed` of 1 and `movesSkipped` of 1 in the summary of a `completed`
run, and the audit event carries the same `movesSkipped`. The rows of the
earlier round are meant to stay; the totals should say which round they
count, or count the last round for failed and skipped.

### RBR-04 (low) - what the guard blocks, and a guard void for another reason

**Where.** `cp/internal/rebalance/guard.go`.

**Shown** (PG 18, `TestRBRWhatTheGuardBlocks`, a second session, 3 s limit,
guard held):
```
renew (UPDATE lease_expires_at)       ok 5ms
cancel flag (UPDATE cancel_requested) ok 5ms
state change (UPDATE state)           ok 5ms
SELECT FOR NO KEY UPDATE              ok 8ms
CREATE INDEX                          ok 12ms
SELECT FOR UPDATE                     blocked (timeout 3 s)
DELETE                                blocked
ALTER TABLE ADD COLUMN                blocked
```
So the lock does what is claimed for the engine's own writes (the partial
unique index on the subject is not a key, so a state change does not
conflict). It blocks, for up to the hold (30 s, 45 s when the holder is
frozen), a schema change of `kingres_cp.task` (a migration that starts then
waits, and queues every later reader of the table behind its
`ACCESS EXCLUSIVE` request) and a delete of the row. Normal holds are
hundreds of milliseconds, so this matters only with the slow records of
RBR-01.

Second part, by reading: `alive` and `acquireGuard` answer `ErrLeaseLost`
whenever the guard's session is gone, also when the cause is a restart or a
failover of the system database or the idle timeout and the lease is still
valid. The run then stands down quietly, its renewals stop, the task sits
`running` until the lease expires (up to 30 s), is claimed again, and the
row left `running` is recovered as "not recorded": an attempt is used. A
failure to connect for the guard (not `ErrLeaseLost`) is also treated as a
failed move. Neither costs data.

### RBR-05 (low) - the match of a move by time

**Where.** `pg.go` `findMove` (`started_at >= since - 10 s`) for `RunMove`,
`Progress`, `CancelMove`, `Recover` when the extension id is not yet known.
`since` is this server's clock; `started_at` is the map group primary's. A
clock difference of more than 10 s makes the record invisible: the guard is
held for the whole 30 s and the call is cancelled (RBR-01), every time. A
retry that starts within 10 s of the previous attempt of the same bucket (the
backoff is configurable) matches the previous record and releases the guard
early. By reading. **Direction.** Match on the id returned by the call, or
on a marker in the call.

### RBR-06 (note) - rebalance and failover

Migration 0009 refuses to create a `server.failover` task while a rebalance
is pending or running (the contract says so). A rebalance with a stuck move
(a dead target, `RecoverWait` of 10 minutes, then retries with backoff) can
therefore hold an automatic failover back for as long as it runs. Failover
does not exist yet; say in the README of the control plane that the
automatic failover will have to stop the rebalance first.

### RBR-07 (note) - the quiet end is quiet

`leaseLost` ends a run with `ErrLeaseLost` and the poll loop logs it at
error level as "task engine"; nothing says which step, which holder and why.
A fence that never clears (a callee's persisted high-water mark above the
epoch of a task, for instance after a restore of the system database, so
that task ids repeat) makes the task loop forever: claim, fenced, stand down,
lease expiry, claim. Each pass takes a lease period, no alert is raised and
the task never fails. By reading; not reproduced.

## What held under attack

- Fencing at engine level: all three kinds of step error of a superseded
  holder end quietly; the new holder's task, run and move rows are untouched
  (system database test, passes).
- The guard: the claim skips the locked row, so a holder cannot be replaced
  between the check and the record; the holder's own renew, cancel flag and
  state changes are not blocked (measured above); a void guard makes no
  call (`TestVoidGuardMakesNoCall`).
- Exclusion, both orders: the trigger takes the advisory lock before it
  reads; the second insert waits and then sees the first (the two tests that
  start a rebalance and a `server.remove` at once pass, and fail with the
  trigger disabled). Resume of a stopped rebalance is refused while a server
  operation is unfinished. A paused rebalance does not exclude, as the
  contract says. No path I found leaves the exclusion held: a server
  operation ends as `done` or `failed`, a rebalance as well, and a lease that
  expired is claimed again.
- `UpdateServer` takes the same lock for its check and its change; no
  deadlock found with the creation of tasks (the task insert is never in a
  transaction that holds the server row).
- Migration 0010 on an existing run: the new columns default to round 0; the
  outcome check is replaced by name (`rebalance_run_outcome_check` is the
  name 0008 gives); a run that was stopped before the upgrade starts round 1
  when it is started again, so its exhausted attempts are forgotten as
  intended.
- Ending a stopped run: outcome `cancelled`, no `taskFailed` alert, the
  hook runs once for the request that ended the task.
- The answers of `Recover`: `done` and `cleaning` are done, `failed` and
  `cancelled` are taken as the extension says, every phase before the switch
  asks `resume_bucket_moves` and waits while the session is alive.

## Not reproduced

- A client connection lost while the backend goes on, before the move is
  recorded. With a stub whose record appears at the end of the call, I cut
  the call's connection after 0.7 s (`TestRBRSeveredClientBeforeTheRecord`):
  `RunMove` returned "use of closed network connection", `Recover` answered
  "no record" at once, and 5 s later the extension had no record at all,
  because the deferred `cancel()` of `RunMove` sent a cancel request that
  reached the backend. So the answer "no record" is right whenever the cancel
  arrives. It would be wrong when the cancel cannot be delivered, or after a
  crash of `kingresd` (nobody cancels): then a move may be recorded a moment
  after `Recover` said "no record", and run unrecorded; the extension's
  refusal of a second move of the bucket keeps the data right. This is the
  stated remainder, not worse; not shown on a real cluster.
- A real lock wait of more than 30 s on the cluster lock (RBR-01 uses a
  stub), and a restart or failover of the system database while the guard is
  held (RBR-04 second part).
- A second manager really taking a task over (the harness cannot).

## Not covered

- `internal/clustertest`: the three-server tests of the commit
  (`TestRebalanceUnderFaults`) were not run, and the "fails without" runs of
  RB-03, RB-04, RB-05, RB-09, RB-12 and RB-14 were not made.
- The contract checker, PostgreSQL 19, the browser tests, the complete
  integration suite.
- The order of the calls in `AddServer` (whether a refusal by the trigger
  after the server was introduced leaves a signed certificate and a pending
  server until the hourly sweep) was not read to the end.
- The one stated remainder (a process frozen exactly between the guard check
  and the call for longer than the idle timeout) and the writes of other task
  kinds that are not conditional on the lease were taken as stated.
