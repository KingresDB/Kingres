<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026 RooyeKhat Media Co -->

# Independent review: the fixes of failover round 2 (branch fo-r2wip, 36e73e61b3)

## Scope and method

Scope: the 48 commits of fo-r2wip on e279c14091, in particular the fix commits for F5-01 to F5-12, the three defects found by cluster tests
(828719283d, 21689b6fb4 with 81fbfdd74f, 7ac59bee1a) and the questions: two writable primaries of one group, a lost acknowledged write, a group without a
primary and without a repair.

Method: read the review and the diffs of every fix commit and the code around them (mapfailover.go, switchover.go, failover.go of kingresd; failover
switchover.go, steps.go, repair.go, restore.go; mapgroup; journal; lease watchdogfile.go and manager.go). Unit tests of failover, failover/journal,
failover/mapgroup, lease and cmd/kingresd were run on the tip (all pass). 22 mutants of the fixes were run (list at the end). Cluster tests of
failover_mapfaults_test.go were run in a private lane (result at the end).

Result: critical 0, high 1, medium 2, low 3 new findings (F6-01 to F6-06). Every F5 finding is fixed as far as its own text goes; F6-01 and F6-02 are
what remains of the F5-01/F5-03/F5-09 class.

## Verdict per finding

| Finding | Fixed | New defect from the fix | A test fails without it |
| --- | --- | --- | --- |
| F5-01 | Yes. The rollback takes the candidate from the step (`sc.newServer`) and an unreadable term is -1. | No new one. It makes F6-01 reachable in a different form (see there). | Yes: mutants F5-01a and F5-01b killed (TestAProofThatCannotBeSentRollsTheMapGroupBack). |
| F5-02 | Yes. `abandonLocal` lets go of the fence lease; the monitor path goes through the same function. | No. | Yes: F5-02 killed (3 tests). |
| F5-03 | Mostly. The intent is on disk before the call, the old leader asks the candidate first. A window stays (F6-02) and the refusal to prove "in recovery" can leave the old leader held (F6-01). | Yes: F6-01. | Yes: F5-03a to F5-03e all killed. |
| F5-04 | Yes. `standby_behind` is polled for `VerifyWait`. | No. | Yes: killed. |
| F5-05 | Yes (documents and ADR 0026). The sentence "no other member ever receives" is no longer in README, internal-api.md or fwdproof. | No. | Not testable (documents). |
| F5-06 | Yes. `mapHandedOver` returns an error, the step retries, and a group whose row says promoted or done is not rolled back. | No. One note: the guard reads the row, which is unreadable after the stop of group 0, so after the stop it is the MapAbandon-first rule (F5-03) that protects a promoted group, not this guard. | Yes: F5-06a and F5-06b killed. |
| F5-07 | Yes. `Relinquish` on the control data error. | No. | Yes: killed. |
| F5-08 | Yes for the caller check and for telling the old leader. Telling the old leader is useless when its task is not waiting any more (F6-01). | See F6-01. | Yes: F5-08a and F5-08b killed. |
| F5-09 | Yes for the task key and for abandoning the journal although the old leader could not be told. | Yes: F6-01 (a journal abandoned and the old leader never resumed). | Yes: F5-09 and F5-09b killed. |
| F5-10 | Yes. Only group 0 of a graceful run is bounded. | No. | Yes: killed. |
| F5-11 | Yes (spot checks: the "Not reachable yet" text and the "Not built, and refused" text are gone; one leftover sentence in the design progress paragraph says "the four open points stood (they are settled below)", which is true). | No. | Not testable. |
| F5-12 | Yes. The checkpoint is answered only while a marker holds the primary back; the callers have tests. | No. | Yes: F5-12a killed. |

Defects found by cluster tests:

- 828719283d (the step ran on the task lease): fixed. `context.WithoutCancel` after the clean stop is needed; mutant `ctxcancel` is killed. See F6-04 for the
  side effect (the step now outlives a leader change for up to 6 minutes).
- 21689b6fb4 and 81fbfdd74f (a second switchover from the same task): fixed; mutants 21689 and 81fbf killed.
- 7ac59bee1a (nil dereference in `writeWatchdogFile`): fixed; mutant nilguard killed with the original panic. Another race in the same function remains
  (F6-05).

## New findings

### F6-01 (high) The old leader of a planned switchover of the map group can be left held back for ever: no primary, and nothing repairs it

Where: internal/failover/switchover.go `rollbackSwitchover` (the `IsCode(err, "conflict")` branch returning `handedOver`), `rollbackFailed`;
cmd/kingresd/mapfailover.go `abandonLocal`, `inRecovery`, `tellOldLeader`, `handleMapRefused`, `plannedBox`; internal/failover/repair.go (the map group is not in
`Primaries`, so it is never repaired); internal/failover/mapgroup/mapgroup.go `promote` (F5-09 part).

Failure scenarios.

1. The candidate's standby instance is down (or its connection cannot be made) after the promotion intent was written. The old leader's verdict wait ends
   after 6 minutes and `rollbackSwitchover` asks `MapAbandon`. `abandonLocal` cannot prove "still in recovery" (`inRecovery` returns an error) and answers 409.
   The old leader reads that as "the candidate promoted or may have", keeps the old primary held back, and the step returns success. The candidate's runner
   gives up after its tries (20 rounds), the journal stays active, and nothing resumes it. An operator who later calls abandonMapGroupBootstrap after the
   instance is back succeeds (in recovery), the candidate lets go of the lease and calls `map/refused` on the old leader, but the old leader's task is gone:
   `plannedBox` records the refusal and nobody reads it (`MapRefusal` is read only inside `stopClean`). The old primary stays held back (marker file). Repair
   skips the map group, so no one resumes it, and the system database is down: the whole control plane is down.
2. The old leader's kingresd restarts between the proof and the verdict (its instance stays held by the marker). The standby then refuses the proof
   (`unprovable` or `standby_behind` for longer than 30 s). Since F5-09 the candidate abandons its journal "whether or not the old leader could be told"; the
   `refused` call fails (the box is empty after the restart), and the old leader is never resumed. The error text claims "its task rolls back by its own wait";
   after a restart there is no task and no wait.

Suggested fix. (a) Let the candidate resume the old leader itself after it abandons a planned journal: call `/failover/resume` on `cur.OldServer` (the same
call the rollback uses, with term -1) in addition to `map/refused`, and retry it from the monitor while the journal is abandoned and the catalog still names the
old server. (b) Let the old leader's kingresd, at start, find a held-back primary of group 0 with no task, ask the servers hosting a standby whether a bootstrap
that names this server as old server is active, and resume when none is. (c) Make `inRecovery` work for a stopped instance: read `pg_controldata`
("in archive recovery") when the connection fails, so that an unreachable standby is not read as "may have promoted". Test: unit on `abandonLocal` with an
`inRecovery` that fails and a control data of a standby; cluster: stop the candidate's instance for more than the verdict wait, then abandon, and check that
the old primary runs.

### F6-02 (medium) An abandon still races the promoting call: the journal is abandoned before the lease is released, and the probe takes no lifecycle lock

Where: cmd/kingresd/mapfailover.go `abandonLocal` (order: `Abandon`/`AbandonUnpromoted`, then `releaseFence`) and `inRecovery`; cmd/kingresd/failover.go
`handleFailoverPromote` (the lock `LockLifecycle` is held across `pg_promote`; the lease check is before it); internal/failover/mapgroup/mapgroup.go `promote`
(the retry loop never reads the journal again).

Scenario. The promote call has passed the lease check and runs `pg_promote(true, 60)`. The abandon probes `pg_is_in_recovery()` (still true during the
promotion), marks the journal abandoned, then releases the lease. Or a retry of the loop reaches the check between the journal mark and the release. The
instance ends promoted. `attachPromotedInjected` fails or the later release is refused for an attached adoption, so a writable primary of group 0 exists on the
candidate, the journal says abandoned, the bootstrap never records the catalog follow-up and the old leader either cannot win the lease or, if the lease was
released first, starts its own primary while the promoted copy exists unguarded until the lease manager stops it. The window is small (the length of a promotion),
and the cluster test `promote:slow` holds the call before the lock and the role file, so it does not exercise it.

Suggested fix. Take the standby agent's lifecycle lock in `abandonLocal` before the probe (the promote handler holds it across `pg_promote`), and keep it until
`releaseFence` returned; have the promote loop in `mapgroup` stop when `Journal.Active()` is false. Test: a fake `Promote` that blocks inside the lock while an
abandon runs; expect the abandon to wait and then answer 409.

### F6-03 (medium) The map group's repair gap is documented nowhere and the operator has no force

Where: internal/failover/repair.go (comment: "the map group excluded"); abandonMapGroupBootstrap text.

The abandon refuses with 409 for as long as the candidate cannot show that its standby is in recovery, and there is no operation that resumes the old
leader of the map group without the old leader's own task. Together with F6-01 an operator has only the internal call. Suggested fix: give
`abandonMapGroupBootstrap` an explicit answer that names the old server and the resume step, or add the map group to Repair with the old server known from the journal.

### F6-04 (low) After the clean stop of the map group the old leader's step outlives a leader change

Where: internal/failover/switchover.go `stopClean` (`ctx = context.WithoutCancel(ctx)`).

The step of the old leader now runs for up to `MapVerdictWait` (6 minutes) on a context nothing cancels, while the new leader's engine claims the same task and
runs its own steps. They agree because every step on the new leader skips when `mapHandedOver`, and the old step returns when the candidate is out of recovery.
The rollback at the end of the 6 minutes is guarded only by MapAbandon-first (a 409 answer) and not by the row (unreadable). If the candidate's server cannot be
reached at that moment (an error other than 409), `rollbackSwitchover` goes on to release the leases and resume the old leader. That is safe against two
primaries only because the candidate's lease is renewed by its own kingresd, which is the one that cannot be reached; say so in the comment and log the case
as a warning that names the risk. Suggested test: a MapAbandon error that is not 409 after the candidate promoted (fake rows say out of recovery): expect
no `resume`, or document the choice.

### F6-05 (low) `writeWatchdogFile` writes the file outside the lock, so a stale write can overtake the Off file

Where: internal/lease/watchdogfile.go `writeWatchdogFile` and `writeFile`.

The nil guard closes the crash. The write itself still happens after `m.mu.Unlock()`: a round that took `seq` and `rem` can write after `DisarmWatchdog` (adopt.go) has
written the Off file with a later `seq`, which re-arms a watchdog for a demoted instance for `rem` milliseconds; no later file comes, and the watchdog
would fence a standby that runs correctly. Not shown by a test; the window is a few milliseconds and the effect is a restart of a standby. Suggested fix: write the file
under a per-group write lock and re-check that the state still exists inside it.

### F6-06 (low) Test gaps

1. `TestAnAbandonWhilePromoteIsInFlightLeavesExactlyOnePrimary` holds the promote call before the lock, so it does not test F6-02.
2. No test of `rollbackSwitchover` where `MapAbandon` fails with an error that is not a conflict after a promotion (F6-04).
3. No test that a refused planned journal leads to the resume of the old leader when the old leader's task is gone (F6-01).

## Verified as correct

- Two primaries: the lease argument of the planned switchover stands. The promote handler still checks the lease immediately before `pg_promote`; the abandon
  now releases the candidate's lease and the old leader releases only after the candidate answered; an attached (promoted) adoption cannot be released.
- Abandon racing a completing bootstrap: after `Done[StepPromote]` the abandon is 409; after the intent, 409 unless the standby is proven in recovery; the
  cases left are F6-01 and F6-02.
- Paused writes after a crash: Repair resumes holds of ordinary groups after the grace; the HoldBack marker survives a restart and the start gate refuses a
  start; `demoteClean` ends the renewal on every error path (mutant F5-07 killed).
- Restore of preferred primaries during a failover or a rebalance: `StartRestore` refuses while any failover or restore task is unfinished and the engine
  exclusion (KX codes) covers a rebalance; the old leader of each group is checked against the catalog at the step (`checkCatalog` with the term the run began
  with). The map group is last.
- A signed `map/start` is bound to sender, receiver, user, roles, request id and body; the plain form is accepted only for `planned` from the old server.
- `map/abandon` of a planned journal is accepted from the old leader, from the server that took the operator's call, or from a server hosting a standby.
- No machine-local paths in the diff of the fixes; the new files carry the licence header.

## Mutants

All were run on the tip with the unit tests of the package named; each was restored with `git checkout` before the next. K = killed, by the test named.

| Mutant | Result |
| --- | --- |
| F5-01a: `sc.newServer` not set in `stopClean` | K, TestAProofThatCannotBeSentRollsTheMapGroupBack |
| F5-01b: `newServer := ""` in `rollbackFailed` | K, same test |
| F5-02: no `releaseFence` in `abandonLocal` | K, TestAnAbandonedBootstrapLetsGoOfItsLease and two more |
| F5-08a: no caller check in `handleMapAbandon` | K, TestOnlyTheOldLeaderOrACandidateGivesUpAPlannedBootstrap |
| F5-08b: no `tellOldLeader` | K, TestAGivenUpPlannedBootstrapTellsTheOldLeader |
| F5-03a: no `PromotionIntent` | K, TestAnAbandonWhilePromoteIsInFlightIsRefused |
| F5-03b: `Abandon` ignores the intent | K, TestAPromotionThatWasBegunCannotBeGivenUpByTheOrdinaryCall |
| F5-03c: `inRecovery` never used | K, TestABegunPromotionIsGivenUpOnlyForAStandbyThatIsStillInRecovery |
| F5-03d: a 409 of MapAbandon not read as handed over | K, TestARollbackThatMeetsAPromotedCandidateLeavesTheOldLeaderHeld |
| F5-03e: MapAbandon not asked first | K, TestAProofThatCannotBeSentRollsTheMapGroupBack |
| F5-04: `standby_behind` not polled | K, TestAStandbyThatIsBehindForAWhileIsWaitedFor |
| F5-06a: `mapHandedOver` returns false on error | K, TestAStateThatCannotBeReadIsNotReadAsNotHandedOver |
| F5-06b: no guard for promoted/done rows | K, TestAGroupThatWasPromotedIsNeverRolledBack |
| F5-07: no `Relinquish` on control data error | K, TestTheRenewalEndsOnEveryEndOfTheCleanStop |
| F5-10: `oldCtx` always bounded | K, TestOnlyTheMapGroupOfAPlannedSwitchoverBoundsTheCallsToTheOldServer |
| F5-12a: no held-back check in the checkpoint | K, TestTheCheckpointIsGivenOnlyForAHeldBackStoppedPrimary |
| F5-09: refusal not keyed by task | K, TestARefusalBelongsToTheTaskAndTheServerThatWereAsked |
| F5-09b: journal kept when the old leader cannot be told | K, TestAGivenUpJournalDoesNotDependOnTellingTheOldLeader |
| nilguard: no nil check | K, TestTheWatchdogFileOfAGroupWhoseStateWasDroppedIsNotWritten (panic) |
| ctxcancel: no `WithoutCancel` after the stop | K, TestAProofThatCannotBeSentRollsTheMapGroupBack |
| 21689: a given-up task may start again | K, TestAGivenUpPlannedSwitchoverIsNotStartedAgainByTheSameTask |
| 81fbf: no handling of the 409 of `MapStart` | K, TestAMapGroupSwitchoverThatWasGivenUpEndsAsUnprovenWhenTheStepRunsAgain |

Two mutants first failed to build (unused variable) and were redone as compiling ones; their kills in the table are of the second form. No mutant survived.

## Cluster tests

Run in a private lane on PostgreSQL 18 (not 19), tip unmodified: TestSwitchoverOfTheMapGroupWhoseCandidateNeverPromotes (486 s), TestAbandoningTheBootstrapOfASwitchoverRestartsTheOldLeader (126 s), TestAnAbandonWhilePromoteIsInFlightLeavesExactlyOnePrimary (140 s; the abandon answered 200 abandoned), TestSwitchoverOfTheMapGroupWaitsForAStandbyThatIsBehind (197 s), TestSwitchoverWhoseControlDataCannotBeReadRollsBack (194 s): all PASS. No cluster test was run under a mutant, and PostgreSQL 19 was not run. The scenarios of F6-01 and F6-02 are not covered by these tests.

---------------------------------------------------------------------------------------------------------------------------------------

## Fixes

Fixed on the branch of the review (fo-r2wip), rebased onto kingres-18 at e279c14091. For each fix a test fails without it (a mutant of the fix was run and killed; the list is below).

| Finding | Commit | What changed |
| --- | --- | --- |
| F6-01 | 7c33f7e24e, 19340fef33 | The old leader ends a hold that nothing owns. Its own loop (every ten seconds, from the start of kingresd) asks every other server `GET /failover/map/bootstrap?old=<server>` and ends the hold only when no bootstrap that names it is active, no standby of the map group was promoted, and a standby is shown to be in recovery. The candidate that gives its bootstrap up (the operator, the monitor, or a refused proof) asks the old leader to do the same (`POST /failover/map/resume`), after the journal is given up. The question whether the standby is still one falls back to `pg_controldata` when the instance cannot be reached. |
| F6-02 | 7c33f7e24e, 19340fef33, 88a94774ba | The abandon takes the standby agent's lifecycle lock around the probe, the mark of the journal and the release of the lease. The promoting loop, every step of the bootstrap and the wait for the lease read the journal, and the journal refuses the intent to promote for a bootstrap that was given up. The last two came from the lane: an abandon that arrived while the runner waited for the lease was followed by a promotion. |
| F6-03 | 7c33f7e24e | `abandonMapGroupBootstrap` with no bootstrap in progress asks every server to end a hold that nothing owns (the same checks as F6-01), and the answer names the server that was started again (`resumedServerId`, contract 0.15.0). A planned bootstrap that is given up names the old leader it resumed. No separate force: the checks are the safety. |
| F6-04 | 19340fef33 | The rollback that cannot ask the candidate (an error other than a conflict) goes on and says in the log that the lease decides which primary may run; a test states the choice. The step is not cancelled on a leader change: it ends when the candidate is out of recovery, and after six minutes at the latest. |
| F6-05 | 074f111584 | The three writers of the watchdog file (a round, the idle refresh, the Off file of a demotion) exclude each other under one lock. |
| F6-06 | 7c33f7e24e, c07f1d0524, 2967b22897 | The three gaps are tests now: an abandon that waits for a promotion inside the lock (cluster), a rollback with an error that is not a conflict (unit), a refused proof with the old leader's task gone (cluster and unit). |

Found by the cluster tests of these fixes, and fixed: the abandon that arrives while the bootstrap waits for the lease (F6-02, 88a94774ba, see above); a divergence of an old primary that needed crash recovery was reported without a number when its WAL ends before the point where the histories parted (4a97280ccb, 3ccd5d8810: the report carries the number, zero in that case, when the directory is shut down; the cluster test of the old primary accepts a missing number only with the documented message).

### Mutants of these fixes

| Mutant | Result |
| --- | --- |
| F6-01 no resume asked on abandon | killed, TestAGivenUpPlannedBootstrapAsksTheOldLeaderToEndTheHold |
| F6-01 no resume after a refused proof | killed, TestARefusedProofAsksTheOldLeaderToEndItsHoldAfterTheJournalIsGivenUp |
| F6-01 resume before the journal is given up | killed, same test |
| F6-01 active bootstrap ignored | killed, TestTheOldLeaderEndsAHoldThatNothingOwns |
| F6-01 promoted standby ignored | killed, same test |
| F6-01 no proof of a standby needed | killed, same test |
| F6-01 the loop never resolves | killed, TestTheOldLeadersLoopEndsAHoldThatNothingOwns |
| F6-01 the loop ignores the owner | killed, TestTheOldLeadersLoopLeavesAHoldThatHasAnOwner |
| F6-01 no control data fallback | killed, TestAnUnreachableStandbyIsAskedByItsControlData |
| F6-01 a stopped primary read as a standby | killed, TestTheControlDataTellAStandbyFromAPrimary |
| F6-01 status call not checked against its caller | killed, TestTheBootstrapStatusIsAnsweredToTheOldServerOnly |
| F6-02 lifecycle lock not taken | killed, TestTheAbandonWaitsForAPromotionThatIsRunningAndThenRefuses |
| F6-02 promoting loop ignores the abandon | killed, TestAGivenUpBootstrapDoesNotRepeatThePromotingCall |
| F6-02 no look at the journal before each step | killed, TestABootstrapGivenUpBetweenTwoStepsGoesNoFurther |
| F6-02 no look in the wait for the lease | killed, TestABootstrapGivenUpWhileItWaitsForTheLeaseDoesNotPromote |
| F6-02 the journal accepts the intent after the abandon | killed, TestThePromotionIntentIsRefusedAfterTheAbandon |
| F6-02 the refusal of the intent not read as given up | killed, TestABootstrapGivenUpWhileItWaitsForTheLeaseDoesNotPromote |
| F6-03 the operator's resume loop removed | killed, TestTheOperatorsAbandonResumesAnOldLeaderThatNothingOwns |
| F6-03 the peer's answer drops the resumed server | killed, same test |
| F6-04 a failed ask ends the rollback | killed, TestARollbackThatCannotAskTheCandidateGoesOnAndTheLeaseDecides |
| F6-05 the Off file written outside the lock | killed, TestTheWritersOfTheWatchdogFileExcludeEachOther |
| F6-05 a round writes outside the lock | killed, same test |
| divergence: zero not reported for a WAL that ends before the divergence | killed, TestTheDivergenceIsMeasuredFromAShutDownDirectory |
| divergence: a directory that is not shut down read as measured | killed, TestOnlyAShutDownDirectoryHasItsCheckpointAsTheEndOfTheWal |

### Cluster tests (lane l3, PostgreSQL 18 and 19.0, extensions of e279c14091)

New: a candidate that cannot be asked whether it is still a standby (the first case of F6-01), an old leader that restarted while the proof is refused (the second case), and an abandon that waits for a promotion inside the lock (F6-02). They pass on both majors. On the last full run (after the F7 fixes) the switchover and abandon set passed on PostgreSQL 18 and the failover, old-primary and map-group set on both majors; on PostgreSQL 19.0 the switchover and abandon set had one failure, `TestSwitchoverOfTheMapGroup` (a 10 minute timeout, the new leader answering 503), whose cause is not known: it passed in 4 of 4 reruns alone (3 on PostgreSQL 19.0, 1 on PostgreSQL 18) and the failure output of the test was enlarged. The run described here is the one before the F7 fixes; the F7 fixes are in the next review.
