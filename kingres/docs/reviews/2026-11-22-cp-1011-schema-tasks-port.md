<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026 RooyeKhat Media Co -->
# Independent review: schema tasks port on cp-1011 (2026-11-22)

## Scope

Commits on branch cp-1011 on top of 01aa5b986a:

- e6bb60f841 task engine: Step.Recover, Env.CancelRequested, migration 0041, memory-store rules, tests
- b22fde947d control plane: schema task steps, Recover on the two make steps, left-behind-move follower, keyless checks,
  cancel semantics, withCancelGuard, unit and cluster tests
- bfc79e515e web: schema panel changes and tests

Reviewed as `git diff 01aa5b986a bfc79e515e`, in a detached worktree. Intended behaviour taken from
kingres/docs/design/shard-phase11.md (sections 8.1, 13.1) and the extension sources (contrib/kingres_shard, recovery.c,
kingres_shard--0.7.sql). The writer's comparison notes were read but not taken as proof.

## Method

- Read of the full diff and of the surrounding engine code (run loop, honourCancel, watchCancel, cancelTask, leaseLost, fail).
- Read of the extension side that the control plane relies on: recover_prepared_transactions (what it resolves),
  resume_bucket_moves (what it skips, which phase it leaves), cancel_bucket_move (which phases it refuses).
- Go unit tests: internal/task, internal/ops, internal/httpapi pass. Web tests (schemas-panel-0-16, elasticity-0-16, 27 tests) pass.
- PostgreSQL 18 runs on lane l4 with extensions of commit 28a28bf518 (installed into both l4 installations): the trigger test
  in internal/rebalance and the full TestSchemaTasksOnACluster (3 servers) pass, 135 s.
- Own throwaway unit test (deleted) for the engine path of Recover with a cancelled context.
- Mutants (below), each reverted afterwards.

Result: no critical and no high finding. Two medium findings, the rest low. The core design (trigger, Recover, truthful
cancel) is sound; the problems are at the edges.

## Findings

### SP1 (medium) Recover ends the task as step_interrupted when the run is shut down or loses its lease during Recover

File: kingres/control-plane/internal/task/task.go, the new block at line 628 onward (`if st.Recover == nil || rerr != nil { return e.fail(...) }`).

Scenario: a leader dies inside Make the schema distributed. The new leader starts the run, finds the step `running`, calls
Recover; Recover runs `recover_prepared_transactions()` (bounded by 4 x node_timeout). Meanwhile kingresd gets SIGTERM, or the
lease renewal fails and runCtx is cancelled. Recover returns a context error. Only ErrLeaseLost and fence.ErrFenced are handled
(`leaseLost`), so the engine records the task as failed with the non-retryable code step_interrupted, although the step may
well have taken effect and the next holder could have settled it. The normal step path treats this case correctly
(`runCtx.Err() != nil && ctx.Err() == nil` gives ErrLeaseLost, `ctx.Err() != nil` returns ctx.Err()); the Recover path lacks it.

Verified with a throwaway test (crash at FaultStepRan of the non-idempotent step, Recover that cancels the run context and
returns its error): RunOnce returns nil and the task is `failed`, code step_interrupted. A permanent failure is recorded by a
run that was being shut down, which defeats the purpose of Recover.

Fix: after Recover returns an error, mirror the step path: if runCtx.Err() != nil and ctx.Err() == nil return ErrLeaseLost, if
ctx.Err() != nil return ctx.Err(); only a genuine error of the callback fails the task. Add a test with a cancelled context.

### SP2 (medium) Test quality: the kill tests do not exercise the new recovery paths; one subtest passes without its fix

Files: kingres/control-plane/internal/clustertest/schemas_tasks_test.go (the two kill subtests).

a) Mutant M1 (below): reverting the make step to its old form (NonIdempotent false, no Recover) leaves "the step that distributes
is not run twice after the leader was killed in it" green in both variants. The old step already read dist_schema before calling
the extension, and the crash points step_started and step_ran sit on engine step boundaries, never inside the call. So the test
proves nothing about Recover and nothing about prepared transactions. The unit test in internal/task/recover_test.go covers the
engine part (done / not done / error / none) but the product Recover functions (recoverCall, the call of
recover_prepared_transactions, the "schema made local on another group" branch) are not run by any test. There is no test at all
for the undistribute make step with a kill.

b) "a move that is running when the leader is killed is followed": the test kills only kingresd; the PostgreSQL session that runs
move_schema stays alive, held at the fault point, so the extension's live mark is held and the follower simply follows a live
move. The path that matters after a real leader loss (the session is gone, resume_bucket_moves undoes the move to phase `failed`,
the step restarts it, up to 3 tries) is never run. Mutant M5 shows the follow branch itself is covered (the test fails
with "bucket 0 of colocation group 2 is already being moved"), but restart-after-undo, the 10 minute bound, and the
different-target refusal are not.

c) The cluster test for the rebalance exclusion covers only one direction (a rebalance started while a schema move runs). The other
direction (schema.move while a rebalance runs) is covered only by the trigger test in internal/rebalance (verified by mutant
M4a), not by a cluster test of the request path (the Go precheck rebalanceActive is what answers there; the trigger is the
backstop).

Fix: crash inside the call (a fault point of the extension such as after the prepare, or a connection kill of the backend before
the commit) for distribute and undistribute; for the move, terminate the backend of move_schema together with the leader and
check that the task ends done after resume_bucket_moves. Until then, mark the distribute kill test as a regression test of the
read-before-call only.

### SP3 (medium) followMove can wait forever, and calls resume_bucket_moves only once

File: kingres/control-plane/internal/ops/schemas.go, followMove (line 890 onward) and runSchemaMove.

a) Fresh path (a call of this run, bound = 0). When the call returns with an error (connection reset, node restart, backend
killed) while the record is still non-terminal (`preparing`, `copying`, `catching_up`, `switching`), `callDone` becomes true,
`errc` is nil, and the loop polls once per second with no exit: `callDone && have && last.terminal()` is false, and `bound > 0`
is false. resume_bucket_moves is never called by the task. The task stays running until the extension's own recovery worker
(recovery.c, every recovery_interval) undoes the move; if the worker is disabled or the node is not the map group primary, forever.

b) Left-behind path. resume_bucket_moves is called once before the follow. The extension skips a move in `preparing` or `switching`
whose updated_at is newer than 2 x node_timeout (kingres_shard--0.7.sql, resume_bucket_moves), so the single call can be a no-op
for a move left a few seconds ago, which is exactly the case after a fast leader takeover. The remaining 10 minutes then depend on
the background worker as well.

Fix: while waiting (both paths, once the call is done), call resume_bucket_moves again every node_timeout or so, and give the fresh
path with a finished call the same bound as the left-behind path.

### SP4 (low) A transient error of cancel_bucket_move is reported as "too late" and the cancel is dropped

File: ops/schemas.go:946 (`tooLate = true` for every error of the call).

The extension refuses a move that is switching with SQLSTATE 55000 (object_not_in_prerequisite_state). Any other error
(connection lost, timeout) also sets tooLate, which suppresses further attempts (`!tooLate`) and writes the "cancel request came
too late" note, though the move was still stoppable. The person's cancel is silently lost and the task goes on to done.

Fix: treat only 55000 as too late; retry the call on other errors.

### SP5 (low) The "too late" note is overwritten by the clean-up step

File: ops/schemas.go:1003 (sMoveClean sets the detail unconditionally).

README and the commit message say a late cancel "ends done and says so in its detail". followMove writes the note (also when the
request was accepted but the move completed), then the next steps run: Switch does not touch the detail, but Clean up the old copy
replaces it with the generic "The old copy is emptied ..." text. For a schema.move the note is therefore lost after a second.
No test checks the note (the cluster test refuses the late cancel at the API, so the mark is never recorded there).

Fix: sMoveClean keeps a detail that starts with moveTooLateText (or appends to it); add an assertion in a test of the race.

### SP6 (low) Recover false followed by a rerun can fail on "already distributed" while a backend of the dead leader still commits

File: ops/schemas.go, recoverCall (line 627) with sDistMake / sUndistMake.

recover_prepared_transactions() leaves a transaction whose origin is still in progress (recovery.c: "origin's transaction is still
in progress: leave it"). Killing kingresd does not kill the PostgreSQL backend that runs distribute_schema; if it is between
prepare and commit when Recover reads the catalog, Recover returns false and the step calls the extension again. The second call
waits for the first, then fails with "already distributed", and the task fails although the effect is there. The window is
small (the call takes seconds, the takeover waits for a lease) but real on a loaded cluster.

Related, harmless: recoverCall returns "took effect" for undistribute when the schema has no row at all, which is also true for a
dropped schema (the comment explains only the made-local-on-another-group case).

Fix: in sDistMake / sUndistMake, when the call fails, read the catalog again and succeed if the schema is in the wanted state.

### SP7 (low) The web note for a failed move can be wrong

File: kingres/web/src/components/TaskProgress.vue (moveFailed).

It says "The schema is still on its original group and in use" for every failed schema.move. The task can also fail in the step
Switch ("the schema is on shard group X, not on Y"), where the schema is on neither the target nor necessarily the original
group, and in the clean-up of a lease problem. The comment next to it says "a move that is past its switch does not fail", which
is true of the copy step only.

Fix: show the note only when the failed step is Check the move or Copy the data, or when the error code is cancelled.

### SP8 (low) Keyless check skips partitions

File: ops/schemas.go:220 (`NOT c.relispartition`).

A partition of a keyed partitioned table can have relreplident n or d without its own key; it is not listed, so the move is not
refused and setIdentityFull does not touch it. The first update or delete on that partition during the copy fails on the source.
Not reproduced (no test of partitions); the extension's copy of partitions may refuse earlier. Worth a cluster test.

### SP9 (low) A cancel of a make-local between its move and its making says nothing about the move

File: ops/schemas.go undistributeCancellable (line 535) with task.cancelTask.

The cancel is accepted after Move first is done and before Make the schema local; the task ends failed/cancelled with the generic
text "The task was canceled between steps." The schema has moved to the target group and stays distributed there, which the
README states but the task does not. A person reading the task cannot tell.

Fix: a hook or the detail for schema.undistribute naming where the schema is now.

### SP10 (low, process) Migration 0041 versus the other implementation

0041 is free on cp-1011 (its chain ends at 0040; kingres-18, cp-int and cp-push end at 0034 because 0035..0040 are cp-1011's own
phase 10 work), so there is no numbering conflict on this line. Branch p11-cp-steps13-14 carries migration 0050_schema_tasks_exclusion
for the same rule, with a function of its own. If both lines are merged, two triggers would enforce overlapping rules (KX006/KX007
twice, which is harmless) but the rebalance direction would exist only once; decide on one before merging.

### SP11 (low, scope note) No exclusion of schema moves against server add/remove/leave

0009 excludes a rebalance against server.add, server.remove, server.leave because both change bucket placement. 0041 excludes
schema.move and schema.undistribute only against rebalance and failover. A schema move to a group whose server is being drained
or removed is not refused by the database (Prepare checks the target group at the request only). Design does not require it, so
low; say so in the README or add server operations to the kinds.

## Verified as correct

- Trigger atomicity: all branches take kingres_cp.lock_operations() (an advisory transaction lock) before the SELECT; plpgsql
  statements get a new snapshot per statement under READ COMMITTED, so the second inserter sees the first after waiting. Both the old
  trigger and the new one take the same lock (re-entrant in one transaction). Pending to running updates are not checked
  (only inserts and paused to active resumes), as in 0009/0016. Paused failover counts as unfinished for schema tasks, a paused
  rebalance does not block them (as 0009). The Go side maps KX001..KX007 to ExcludedError; the Problem text for schema.* is fine and a
  refused rebalance keeps rebalance_in_progress.
- Memory store rules equal the trigger (both directions, active vs pending/running distinction).
- Migration numbering on cp-1011: 0041 is unique.
- Recover cannot resolve foreign work wrongly: kingres.recover_prepared_transactions() resolves only prepared transactions that the
  connected node originated, skips those whose origin is in progress, and decides by dtx_log; it is also what the extension's own
  worker does every interval. It is safe to call. The step is also reread by sDistMake / sUndistMake before they call the
  extension, so a Recover false that follows a recovery that committed the transaction does not call the extension twice.
- Engine Recover path: done records the step and honours a pending cancel at the boundary like the normal path; false runs the step;
  error or no Recover keeps step_interrupted (except SP1).
- Cancel semantics: copy step is not interruptible; the engine's boundary check consults the guard again (honourCancel), and with the
  copy step done the guard says false, so a mark that arrived late is not followed. An undone move fails with code cancelled and
  "unchanged" (cluster test, mutant M2 proves it is tested). A request after the switch is refused 409 task_not_cancellable (mutant M3).
- withCancelGuard: the guards of all kinds (failover, table, split, schema) read only state, steps and detail, which the api task
  carries, so it cannot hide a real cancellable task of any registered kind; it is a pure function over data already read, so no
  cost on list endpoints; a kind without a guard keeps the flag. The cancel answer, getTask and listTasks agree.
- Left-behind move: a move to the same target is followed, another target is refused schema_move_in_progress, `failed` is restarted
  at most 3 times and not after a request to cancel, `cancelled` is returned only for a record that a cancel produced
  (resume_bucket_moves undoes with phase `failed`, not `cancelled`, so an abandoned move is not misreported as cancelled);
  a lease loss or shutdown returns ctx.Err and the next holder follows again.
- Keyless check: relreplident n is flagged even with a primary key, d without key is flagged, i and f are not; the undistribute-with-target
  check and the rebalance precheck are in place (cluster test, first subtest).
- Web behaviour against the API contract: DistributeSchemaRequest has exactly shardGroupId and setReplicaIdentityFull, the dialog sends
  only chosen values; Create/Edit mode defaults follow the design text; the Shards page follows cursors for all schemas.

## Mutants run

All reverted afterwards (worktree clean of source changes).

| id | change | test run | outcome |
|----|--------|----------|---------|
| M1 | make step back to NonIdempotent false, no Recover | cluster: distribute kill subtests | PASS (test does not catch it; SP2a) |
| M2 | followMove never asks the extension to cancel | cluster: cancel before switch | FAIL (timed out waiting for the move to be flagged) |
| M3 | withCancelGuard returns its input | cluster: after the switch | FAIL (task never said it cannot be cancelled) |
| M4 | schema branch of trigger no longer refuses schema.move | cluster: schema move excludes a rebalance | PASS (wrong direction for that test; SP2c) |
| M4a | same as M4 | trigger test in internal/rebalance | FAIL (schema.move accepted while a rebalance runs) |
| M4b | rebalance branch of trigger no longer looks at schema.move | cluster: schema move excludes a rebalance | FAIL (rebalance accepted, a task is left) |
| M5 | follower never follows a left-behind move | cluster: move running when the leader is killed | FAIL ("already being moved") |
| web | revert each of SchemaDialogs, TaskProgress, EditDatabaseDialog, CreateDatabaseDialog, ShardsPage to the parent commit | vitest | each makes at least one test fail |
| engine | throwaway test, Recover with cancelled context | go test | shows SP1 |

Not run: Go unit mutants of recover_test (the unit tests assert the three outcomes of Recover and fail by construction without it),
PostgreSQL 19 (the same source builds for both majors; only the 18 installation was exercised).

## Fixes (2026-11-22)

All on cp-1011, after the three commits reviewed above. Each new test was run against a mutant (the table follows).

| id | what was done |
|----|---------------|
| SP1 | After `Recover` returns an error, the engine treats a shut down or a lost lease as on the path of a step that runs (`ctx.Err()` or `ErrLeaseLost`); only an error of the callback fails the task. Test `TestRecoverCutOffByAShutdownDoesNotFailTheTask`. |
| SP2 | New cluster subtests: a distribute and a make local that the leader's death cut off inside the call of the extension (the call waits for a lock held by the test), the other direction of the rebalance exclusion at the request, a move whose backend was terminated with kingresd alive, and a move whose leader and backend both died. The distribute kill at an engine boundary stays as a regression test of the read before the call. A leftover prepared transaction itself cannot be made on demand: the extension has no fault point inside `distribute_schema`, and a prepared transaction written by hand is not one that `recover_prepared_transactions()` owns; so the test asserts that the product `Recover` ran its not-taken-effect branch (the log line before `recover_prepared_transactions()`), and that the two calls that meet end in one distribution. |
| SP3 | `followMove` asks the extension to settle a move that no call of this run drives (the call returned, or a dead leader left it) every twice `kingres_shard.node_timeout` plus a second, starting at once, and gives up ten minutes after the call ended, on the fresh path too. Cluster tests with the recovery worker made slow. |
| SP4 | Only SQLSTATE 55000 of `cancel_bucket_move` is "too late" (`cancelRefused`, unit test); any other failure is tried again, and after five the detail says the request could not be handed over. |
| SP5 | `Clean up the old copy` keeps a detail that starts with the late-cancel note (unit test with the engine). |
| SP6 | `Make the schema distributed` and `Make the schema local` read the catalog after a failed call and succeed when the schema is in the state the call was to make (`settledAfter`); the in-call cluster subtests need it. |
| SP7 | The note "still on its original group" is shown for a failed `schema.move` only when the failed step is `Check the move` or `Copy the data`, or the code is `cancelled` (web tests). |
| SP8 | The keyless check no longer skips partitions; cluster test with a partition whose identity is NOTHING. Not changed: the two counts of the schema list still skip partitions. |
| SP9 | `Move first` records "The schema has moved to shard group N; if the task is cancelled before it is made local, it stays distributed there." (cluster test). |
| SP10 | None needed: migration 0041 stays; the branch with 0050 will not be merged. |
| SP11 | Migration 0041 (edited in place, it is not released) and the memory store also exclude `schema.move` and `schema.undistribute` against `server.add`, `server.remove` and `server.leave`, both ways (KX007 for the schema task, KX006 for the server task); `schema.distribute` is not excluded. Trigger test and memory-store test. |

Left as it was: `recoverCall` says "took effect" for a make local when the schema has no row on the map group, which is also true for a schema that was dropped meanwhile; the map group cannot tell the two apart without asking the owner, and a dropped schema leaves nothing to make local.

Found while testing the fixes (not in the review): a make local that moves the schema first always failed in its last step, because the extension refuses `undistribute_schema` while the move that the task itself just made is in the phase `cleaning` (ten minutes by default). `Make the schema local` now ends the cleaning of that colocation group's moves at once (`kingres.internal_continue_cleaning`, the call of `cleanup_moved_buckets`), so the schema of a make local with a target ends local on the target. A cleaning that an earlier move left is still refused at the request. Cluster test: the first subtest ends with such a make local.

### Mutants of the fixes

| change | test | outcome |
|--------|------|---------|
| Recover ignores a shut down run (SP1) | `TestRecoverCutOffByAShutdownDoesNotFailTheTask` | fails (task failed with step_interrupted) |
| make steps back to idempotent, no Recover, no log (M1) | the two in-call cluster subtests | fail (the new run never settles the step) |
| Recover kept, `settledAfter` returns the error (SP6) | the two in-call cluster subtests | fail ("already distributed", "not a distributed schema") |
| no periodic `resume_bucket_moves` (SP3) | move whose session died | fails (the task never asked) |
| `cancelRefused` true for any error (SP4) | `TestCancelRefusedIsTheExtensionsRefusalOnly` | fails |
| clean-up step overwrites the detail (SP5) | `TestCleanUpStepKeepsTheLateCancelNote` | fails |
| web: note for every failed move (SP7) | schemas-panel test of a Switch failure | fails |
| keyless check skips partitions again (SP8) | first cluster subtest | fails (`parted_p1` not named) |
| `Move first` does not record where the schema went (SP9) | first cluster subtest | fails |
| no `finishCleaning` | first cluster subtest | failed in the run before it was added ("is being moved") |
| memory store and trigger without the server operations (SP11) | `TestSchemaTasksExcludeAFailoverAndARebalance`, `TestSchemaMovesAndServerChangesExcludeEachOtherInTheDatabase` | fail |
