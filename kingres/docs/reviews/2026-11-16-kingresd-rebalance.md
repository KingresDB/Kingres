<!-- SPDX-License-Identifier: AGPL-3.0-only -->

# Independent review of the rebalancing work of kingresd (contract 0.9.0)

Commit reviewed: `5196e3da5a`. Reviewer: an independent reviewer, not the author of the code. Paths are
repository-relative at that commit; `kingres/control-plane/` is abbreviated `cp/`. Line numbers are those of the commit.

## Scope and method

**Read.** An export of the commit (`git archive` of `kingres` and `contrib/kingres_shard`) in a scratch directory; the
working tree was not used. Read closely: `cp/internal/rebalance/` (loop.go, pg.go, service.go), the task engine
(`task.go`, `pgstore.go`, `memstore.go`, `record.go`), migration `0008_rebalance.sql`, `httpapi/handlers_rebalance.go`,
the diffs of `forward.go`, `guard.go`, `handlers_ops.go`, `membership/facts.go`, `membership/remove.go`,
`ops/servergroups.go`, `ops/tables.go`, `ops/settings.go`, `cluster/binaries.go`, `store/pgstore/*`, `standby/leader.go`,
the 0.9.0 parts of `kingres/api/openapi.yaml` (rebalance operations, `cancelTask`, `Task`), the extension README and the
SQL of `rebalance_plan`, `cleanup_moved_buckets`, `remove_node`. Console: `RebalancePage.vue`,
`RebalanceReportDialog.vue`, `taskPoller.ts`, `TaskNotices.vue`, `TaskProgress.vue`.

**Run.** In the export, on Windows: `go vet`, `go test` of `internal/httpapi` (passes), and throw-away tests
(`review_rb_*_test.go`, kept only in the export; nothing was added to the repository but this report): seven on fakes
(`internal/task` with `MemStore`; `internal/rebalance` with the `memRows` and `fakeCluster` of `loop_test.go`), and two
against a real PostgreSQL 18 system database through `scripts/run-integration.ps1`
(`TestReviewStaleOnFailRewritesTheNewRun`, `TestReviewSysdbRaces`). Each finding says which kind of proof it has.
Findings whose proof is a fake carry the model they assume.

## Summary

| Id | Severity | Summary |
|---|---|---|
| RB-01 | high | A superseded holder runs `OnFail` unfenced and rewrites the new holder's run: its in-flight move and the run become `failed`, an alert is raised (reproduced, system database) |
| RB-02 | medium | A fenced write that finds the lease expired (process frozen, nobody took over) fails the whole task for good (reproduced) |
| RB-03 | high | The lease is proven for the record of a move's start, not for the start: `move_bucket` is called with no check (reproduced on a fake; the commit message claim is wrong) |
| RB-04 | medium | A lost connection of the session that runs `move_bucket` is recorded as a failed move at once; the extension is not asked, a second move starts beside it (model) |
| RB-05 | low | A transient network error pauses the run as "safetyRule" in one place and fails the task in another (reproduced) |
| RB-06 | low | Nothing bounds the number of moves of a run; a plan that does not converge never ends (reproduced) |
| RB-07 | medium | Attempts and skips belong to the run: after a stop and a repair, "Start again" never retries the exhausted bucket and the run ends `failed` (reproduced) |
| RB-08 | medium | Cancel of a paused rebalance: `cancellable` is false while the cancel is accepted, and the run is recorded with the wrong text and a `taskFailed` alert (reproduced) |
| RB-09 | low | Text of internal errors (connection strings, role and database names) goes to the client in problem details, also for `kingres_readonly` (reproduced) |
| RB-10 | medium | A rebalance start and a `server.remove` submit have no common lock; both tasks can exist (reproduced, system database) |
| RB-11 | low | "Start again" with no database selected keeps the old restriction; the page says none selected means all (reproduced) |
| RB-12 | low | Contract: `maxConcurrentMoves` defaults to 1, the contract says the cluster setting (by reading) |
| RB-13 | low | Console: report dialog crashes on an outcome it does not know, and repeats a row key when one bucket moves twice in a run (by reading) |
| RB-14 | note | Cost and polish: a connection per call and a full plan before every move; `cleanup_moved_buckets(true)` is cluster-wide; duplicate audit event (by reading) |

Counts: critical 0, high 2, medium 5, low 6, note 1 (14 in all).

No finding is critical: in every case reproduced here the extension's own rules (one move per bucket, recovery
worker, `resume_bucket_moves` never touching a live session) still stand between the control plane's mistake and lost
data. RB-01 and RB-03 are the two cases of "a superseded leader acting"; they change records and may start one move.

## Findings

### RB-01 (high) - a superseded holder runs `OnFail` against the new holder's run

**Where.** `cp/internal/task/task.go:533-565` (`run`: a step error is a failure unless `runCtx` is already cancelled;
`fail` then calls `e.onFail` whatever `finish` answered, `:733-742`), `cp/internal/rebalance/service.go:164-186` (`onFail`:
unfenced `UPDATE`s of `rebalance_move` and `rebalance_run`, an alert, an audit event), `cp/internal/rebalance/pg.go:538-563`
(`leased` returns `ErrLeaseLost` as a plain error).

**What happens.** A holder that was frozen (SIGSTOP, a paused VM, a long GC) wakes up with its context still live. Its next
fenced write (`UpdateMove` from the poll) is refused with `ErrLeaseLost`, correctly. The step returns that error. The
engine only maps it to "lease lost" when `runCtx` is cancelled, which happens when its renew loop ticks (every lease/3,
10 s), so it takes the failure branch: `finish` fails (not the holder), but `onFail` runs anyway. `onFail` has no lease check
and no epoch: it marks every `running` move of the task `failed` ("interrupted") and the run `failed`, and raises
`taskFailed`, while the new holder is in the middle of that move.

**Reproduced (system database, PostgreSQL 18).** Task claimed by A (epoch 1), lease expired, claimed by B (epoch 2). B
creates the run and starts a move. A's `UpdateMove` and `StartMove` are refused (`ErrLeaseLost`): the fence works. Then the
`onFail` of the service is called as the engine of A calls it:
```
before A's OnFail (B holds the task, its move is in flight): task=running run.outcome=<null> move.state=running taskFailed alerts=0
after A's OnFail: task=running run.outcome=failed move.state=failed taskFailed alerts=1
```
At engine level (`TestReviewSupersededHolderRunsOnFail`, `MemStore`): `A.RunOnce err=task lease lost; task state=running
holder=B epoch=2; OnFail calls by A=1`.

**Expected.** A step error that is `ErrLeaseLost` ends the run quietly (no `OnFail`, no record); `OnFail` runs only after
`finish` succeeded. **Direction.** In `run`, test `errors.Is(err, ErrLeaseLost)` before the failure branch; in `fail`, call
`onFail` only when `finish` returned nil; make `onFail` of the rebalance write with the task's epoch where it can.

### RB-02 (medium) - a lease hiccup fails the task for good

**Where.** Same branch of `run`; `cp/internal/task/pgstore.go:Finish` checks holder and epoch, not expiry; `leased`
requires `lease_expires_at > clock_timestamp()`.

**What happens.** If the process is frozen longer than the lease and nobody took the task over, the next `leased` write finds
the lease expired and returns `ErrLeaseLost`; the engine records the task `failed` (`step_failed`, retryable true) and the
`Finish` succeeds because the epoch still matches. The run ends `failed` with an alert although nothing went wrong with the data.

**Reproduced** (`TestReviewExpiredButUnclaimedLeaseFailsTask`): `RunOnce err=<nil>; state=failed; error=step_failed:
recording the move: task lease lost`. **Direction.** As RB-01: `ErrLeaseLost` returns to the claim loop; the task stays
`running` with an expired lease and is claimed again (epoch grows, `recoverLeftovers` settles the move).

### RB-03 (high) - the start of a move is not fenced

**Where.** `cp/internal/rebalance/loop.go:394-403` (`execute`: `StartMove` then `go RunMove`), `pg.go:367-373` (`RunMove`,
`Progress`, `CancelMove`, `Cleanup`, `CheckMove`: no lease, no epoch in the call).

**What happens.** The commit message says a superseded leader "can neither record nor start a move". It cannot record. The
guard is a transaction on the system database that ends before `move_bucket` is called on another connection; nothing in the
call carries the epoch (the `fence` package that does this for peer calls is not used here). A holder frozen or slow between
the two statements calls `move_bucket` after the new holder was granted the task. The new holder's `recoverLeftovers` meanwhile
finds the row `running` without an extension record and records it `failed` ("not recorded by the extension"), and the late
move then runs unrecorded.

**Reproduced on a fake** (`TestReviewMoveStartsAfterTheLeaseIsGone`, `memRows` loses the lease right after `StartMove`
returned): `Moves returned task lease lost; move_bucket was called for buckets [0]; bucket 0 now in group 2`. The window is
short in normal running; it is the SIGSTOP-between-two-statements case from the brief. The extension refuses a second move of
the same bucket, so no data is doubled; the harm is a move nobody recorded and a run that loses track of it.

**Direction.** Make the start conditional on the lease in the same session: take the task row `FOR SHARE` on the connection
that then calls `move_bucket` and hold the transaction until the call has recorded its extension id (the `move_bucket` call
blocks on the long statement, so use a short lock-and-check immediately before it plus the extension id check in `Recover`);
or pass the epoch to the extension's call and let it refuse a lower epoch. At least say in the commit/README that the start
is only fenced up to the check.

### RB-04 (medium) - a lost connection is recorded as a failed move

**Where.** `loop.go:407-411,442-459` (`settleMove`: any error of `RunMove` is `failed`), `pg.go:367`.

**What happens.** `move_bucket` is one statement of one session. When the client connection breaks (network, `kingresd`
killed by the OOM killer, a proxy), the error is a transport error, and the PostgreSQL backend goes on until it needs to talk
to the client (the extension README: "a move that is alive is never touched by resume"). The loop records the move `failed`
(attempt 1), asks nothing of the extension, plans again and starts the next bucket, so two moves run at once, and the report
says a move failed that finished.

**Reproduced on a fake** (`TestReviewConnectionLossIsRecordedAsFailure`; model: `RunMove` returns `io.ErrUnexpectedEOF` and the
move completes 300 ms later in the background): `bucket 0: recorded state=failed attempts=1 error="unexpected EOF"; the data:
bucket 0 is in group 2 (target 2); verdict=completed done=1 failed=0; moves in flight at once (max)=2`. The summary then shows
`movesFailed 1` with outcome `completed`. Not run against a real severed connection (see Not covered).

**Direction.** On an error that is not a `pgconn.PgError`, settle through `Recover`/`findMove` (as after a restart) before
recording or planning again.

### RB-05 (low) - a transient network error pauses in one place and fails in the other

**Where.** `loop.go:269-276` (any error of `Check`, including a refused connection, pauses the run as `stoppedBy:
safetyRule`), `loop.go:277-280` (the same error from `Plan` returns and fails the task).

**Reproduced** (`TestReviewTransientErrorsEndTheRun`): a refused connection in `Check` gives `task paused ... run.outcome="stopped"
stoppedBy="safetyRule"`; in `Plan` it gives a plain error (pause=false), i.e. a failed task and an alert. The first needs a human
to press "Start again" for a condition that is not a safety rule; the second ends the run. **Direction.** Retry transient
errors inside the loop with a bound; pause only on a `*problem.Problem`.

### RB-06 (low) - no bound on the moves of a run

**Where.** `loop.go:255-314`: `planned` only grows; no cap on done moves.

**Reproduced** (`TestReviewNonConvergingPlanNeverEnds`; model: something flips the target after each move, e.g. two automations
toggling `shouldHaveShards` while the run is stopped and started): after 2 s, 2482 moves of one bucket, run still going. `rebalance_plan`
alone converges (fewest moves, only surplus moves), so this needs an outside actor; stop works. **Direction.** Stop the run
(pause, `safetyRule`) when a bucket is moved twice in one run, or when done moves exceed a multiple of the first plan.

### RB-07 (medium) - a stopped run keeps its exhausted attempts and its skips

**Where.** `loop.go:238-253,325-354` (`attempts` and `skipped` are rebuilt from the run's rows), `service.go:331-348` (`resume`).

**Reproduced** (`TestReviewStoppedRunDoesNotRetryExhaustedBucket`): bucket 0 fails three times, the operator stops, the cause is
repaired, "Start again": `move_bucket calls so far [0 0 0]; bucket 0 state=failed attempts=3; bucket 0 is in group 1; verdict
Outcome:failed ... given up after 3 attempts`. The documented retry rule is per run, but "Start again" is presented as going on
with the run after a repair. Skips (`targetDiskLow`, `groupDegraded`, `groupLeaseLost`) stay for the same reason, and the run ends
`completed` with buckets unmoved and `needed` still true. The only way to a clean retry is to let the run end and start a new one,
or to cancel the paused task (RB-08).
**Direction.** Reset `attempts` and the skip set when a stopped run is started again by an operator.

### RB-08 (medium) - cancel of a paused rebalance

**Where.** `cp/internal/task/record.go:210` (`Cancellable`: pending or running only), `task.go:439-451` (`Engine.Cancel` calls
`onFail` with the task read before the cancel), `service.go:164-186`; contract `cancelTask` ("pending, running or paused") and
`Task.cancellable` ("whether cancel is accepted now").

**Reproduced** (`TestReviewCancelPausedTask`): `paused task: state=paused Cancellable()=false`; after `Cancel`: store `state=failed
error=cancelled`, but `OnFail saw state=paused error=<nil>`. In the rebalance the run is then recorded with "The rebalance task
failed." (not "canceled") and a `taskFailed` warning alert is raised for an action the operator chose. The console cannot offer
the cancel because the field says no. Also by reading: cancel of a *running* rebalance is only honoured between steps
(`Cancel` says so), and "Move buckets" is one step, so a cancel arrives after the whole run; then step "Write the report" is
skipped by `cancelTask` and the run is recorded `failed` although every move was done. The console does not call it.
**Direction.** `Cancellable` true for paused; call `onFail` with the task read after the cancel; make the rebalance step honour
`cancel_requested` like a stop.

### RB-09 (low) - internal text in problem details

**Where.** `service.go:368-375` (`asProblem`: `"The cluster cannot be inspected now: " + clipText(err.Error(), 200)`), used by `Start`
and `Preview` (role `kingres_readonly`); `pg.go:216-229` and `loop.go:272` (text of `check_cluster()` findings goes to the
problem, to `rebalance_run.error` and so to `pausedReason` and the report).

**Reproduced** (`TestReviewAsProblemLeaksInternalText`): `service_unavailable: The cluster cannot be inspected now: failed to connect
to host=/var/run/kingres/kw/n1 user=kingres_service database=shop: dial error`. No password was seen; role, database, socket path
and extension error text (which the extension README treats as sensitive for non-superusers) are. **Direction.** Log the text,
answer with a fixed sentence.

### RB-10 (medium) - start of a rebalance and `server.remove` are not exclusive

**Where.** `pg.go:119-139` (`Check` reads tasks), `service.go:305-314` (`Submit`), `membership/facts.go:105` (`RebalanceRunning`),
`ops/servergroups.go:280`. The one-task-per-subject index covers `cluster/cluster` and `server/<id>` separately.

**Reproduced** (system database): both tasks created, no refusal: `rebalance task 1 (err <nil>) and server.remove task 2 (err <nil>)
are both pending`. In the real flow each side first reads the other's state. The per-move `Check` then pauses the rebalance at the
next move (`safetyRule`), and `remove_node` refuses a group in a move (`55006`), so the end state is recoverable by hand, but the
removal task may fail or the move may be undone; nobody resumes either automatically. **Direction.** Take one advisory lock (or a
conditional insert) around "check and submit" on both sides.

### RB-11 (low) - "Start again" with empty options keeps the old restriction

**Where.** `service.go:331-336` (`opts` is nil when the request has no databases and no `maxConcurrentMoves`), `pg.go:738`;
`RebalancePage.vue` (`selectedDbs` is not filled from `st.options` when stopped; the legend says none selected means all).

**Reproduced** (system database): run created with `Databases:[shop]`, stopped, `ResumeRun(nil)`: `options of the run after Start
again with none selected: {Databases:[shop] MaxConcurrentMoves:0}`. **Direction.** Always pass the request's options on resume, or
prefill the page.

### RB-12 (low) - default of `maxConcurrentMoves`

`service.go:311` sets 1; the schema (`RebalanceOptions.maxConcurrentMoves`) says "defaults to the cluster setting (4 unless
changed)", and `status.options` shows the recorded value. By reading and the README of 0.9.0 ("one move at a time"); either the
description or the recorded value should change.

### RB-13 (low) - report dialog

By reading, `RebalanceReportDialog.vue`: `OUTCOME[m.outcome].cls` throws when a newer server sends another outcome (the rest of the
page uses `tolerant`); the row key `databaseName-colocationId-bucket` repeats when one bucket was moved twice in one run (a done
row is not reused, `StartMove` reuses only failed, cancelled, skipped), which breaks list patching. The page also has no state for
"task disappeared": a status read that fails shows the boundary error and loses the Stop button while the run goes on.

### RB-14 (note)

By reading. (a) Every call of the loop (`Progress`, `Rows.Run`, `UpdateMove`, `CancelMove`) opens a new service connection, several per
500 ms poll. (b) `Plan` asks for the whole plan, with the size of every bucket, before every move: quadratic for large clusters.
(c) `membership/remove.go` calls `cleanup_moved_buckets(true)`, which finishes every move in `cleaning` in the cluster, not only those
of the removed server, and ends the retention of old copies early. (d) `stepWrite` and `onFail` both audit `rebalance.failed`.

## What held under attack

- Every write of a run (`SetPlanned`, `StartMove`, `UpdateMove`, `FinishMove`, `AddSkipped`, `Settle`) goes through `leased`: `FOR SHARE`
  on the task row with epoch, state and `lease_expires_at > clock_timestamp()`. A holder of an older epoch was refused in the system
  database test for `UpdateMove` and `StartMove`.
- Two starts at once: the unique subject index gives `rebalance_in_progress`; resume of a task that is not paused gives `ErrNotPaused`.
- A restart or leader change leaves a move `running` in the rows; `recoverLeftovers` settles it through the extension and does not
  decide; the extension's recovery worker covers a `kingresd` that never returns. A move whose session is alive is waited for.
- The stop: cancel before the switch, finish after it, repeated every poll; stop while backing off or between moves reaches the top of the
  loop; a second stop is refused with `rebalance_not_running` (system database: `RequestStop` false once `outcome='stopped'`).
- `removeServer` and `updateServer` do not treat a paused rebalance as running; `remove_node` of the extension refuses a group in a
  move; the forced cleanup in `unregisterNode` only affects moves in `cleaning`.
- Roles: start and stop need `kingres_ops`, reads and preview `kingres_readonly`; `go test ./internal/httpapi` (role matrix) passes. Reads
  are leader-bound so followers forward them; `Prefer` and `Preference-Applied` are forwarded.
- No SQL is built from values: names go through `ServiceConn` and parameters; `onNode` only runs fixed texts.
- The paused and skipped states are handled in the task list filter, `taskPoller.ts` (`isSettled`), `TaskNotices.vue`, `TaskProgress.vue`.

## Not reproduced

- A real severed connection during `move_bucket`, a node going down during copy, catch-up or switch, a leader change with a real
  failover: only modelled (RB-03, RB-04) or read.
- The race between `Recover`'s "not recorded by the extension" answer and a late `move_bucket` (RB-03 consequence) was not run against the
  extension; `findMove` also matches by start time minus 10 s, so a quick retry of the same bucket could pick up the previous attempt's
  record (only with a backoff under 10 s; the default is 30 s).
- Drop of a database during a run: by reading, `Check` fails with `3D000` and the run pauses (`safetyRule`) until a human starts it again.
- Details of the extension's `detail` text in move errors reaching the report were not shown (RB-09 shows the path with a fake text).

## Not covered

- Console not run: the mock (`npm run dev`), the browser tests and `npm run test` were not run (the dependencies are not part of the
  export, and the machine was loaded); the console findings are by reading only. Widths, long names and roles of the page were not tried.
- Contract: handlers were compared by reading and by the existing role-matrix test; `check-contract.mjs` was not run; status codes and
  headers of every changed operation were not exercised against a running `kingresd`. `Preference-Applied` and `Location` were read.
- `distributeTable` with reference tables, `VerifyBinaries` for two majors, PostgreSQL 19, the wal_level setting for upgraded
  instances: read only; the "rows" check of `PrepareDistribute` is a check before the task, not in the step.
- No multi-server run (`internal/clustertest`) was made; the full integration suite was not run.
