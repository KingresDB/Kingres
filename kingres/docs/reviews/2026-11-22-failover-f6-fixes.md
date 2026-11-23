<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026 RooyeKhat Media Co -->

# Independent review: the fixes of F6-01 to F6-06 (branch fo-r2wip, 074f111584 to 3ccd5d8810)

## Scope and method

Read the report of round 2 (F6-01 to F6-06), the diff of the nine commits (mapheldback.go, mapfailover.go, failover.go, mapgroup.go, journal.go,
switchover.go, standby/oldprimary.go, lease watchdogfile.go and adopt.go) and the code around them (cleanStop, the lease manager's Resume, HoldBack,
ReleaseGroup, BecameStandby, handleFailoverPromote, handleFailoverResume). Unit tests of cmd/kingresd, failover, journal, mapgroup, standby and lease
pass on the tip (Windows). 21 mutants were run (table at the end). Cluster tests were run in lane l4 on a private PostgreSQL 18 copy (result below).

Result: critical 0, high 1, medium 1, low 3 new findings (F7-01 to F7-05). F6-01 to F6-06 are fixed as far as their own text goes; the new
auto-resume has a hole of its own (F7-01): its evidence is "some standby is in recovery", and that proves nothing about the standby that was promoted.

## Answer to the overriding question

Can the auto-resume resume the old map-group primary while a standby has been, or is being, promoted, giving two writable primaries for group 0?

- Two writable primaries at the same moment: no path found. Resume only removes the hand-over marker and writes the wait marker
  (internal/lease/adopt.go `Resume`); the primary starts only when the lease rounds of this server win the group lease. A bootstrap that is alive holds
  the fence/holder lease and renews it through its own kingresd, and a promoted instance is attached to the lease (an attached adoption is never
  released), so the old primary cannot win while the promoted one is guarded. The promote handler still checks the lease immediately before pg_promote.
  Every path of the new code (loop, `handleMapResume`, the abandon, the runner's `ResumeOldLeader`) ends in the same `Resume`, so every path
  inherits that lease condition; none starts the instance directly.
- Races with a bootstrap that starts just after the "no active bootstrap" check: closed by ordering. `stopClean` sends `MapStart` (which creates the journal
  on the candidate) before `Demote`, and the marker (`HoldBack`) is written inside `Demote`. So whenever the marker exists for a live task, the candidate's
  journal is already active. `handleMapBootstrap` reads the standby state first and the journal second, so a promotion that begins between the two reads
  is seen as Active. A hard map failover that a third server starts after the check is arbitrated by the lease.
- A standby promoted but not yet visible as out of recovery: covered while the journal is active (the intent is on disk before pg_promote, the journal stays
  active until it is done). After the end the journal is not active any more and the promoted instance reports "primary".
- pg_controldata of a stale directory: the fallback is used only when the connection fails; the states "in production" and "shut down" read as primary, and a
  crash during a promotion leaves "in archive recovery" with standby.signal still present, which is a standby. No hole found there.
- What is NOT safe is data, not simultaneity. If the evidence is missing the old primary can start from its stale directory after the lease of the new
  primary has lapsed, which loses acknowledged writes of the new primary and gives the metadata of the cluster two diverged histories (F7-01).
  The lease is the final authority for who runs, but it is not an authority for which history is the right one, and the hold was that authority.

## Verdict per finding

| Finding | Verdict |
| --- | --- |
| F6-01 | Fixed for the cases of its text: a given-up bootstrap asks the old leader to resume (after the journal is abandoned), the old leader's loop resolves a hold that nothing owns after a restart, a stopped standby is read by its control data, the operator's abandon with no bootstrap asks every server. Mutants of the three guards are killed. The evidence rule of the loop is too weak (F7-01). |
| F6-02 | Fixed as far as the text goes: lifecycle lock around probe, mark and release; `PromotionIntent` refuses an abandoned journal (killed); the runner reads the journal before each step, in the lease wait and before every repeat of the promote call (all killed). Two gaps: the failure of the lease release is only a warning (F7-02), and no unit test fails when the lock is let go before the release (mutant M11b survived; F7-04). |
| F6-03 | Fixed. The operator's abandon names the resumed server, and `handleMapResume` explains why it did nothing (`TestTheResumeCallSaysWhyItDidNothing`). It shares the evidence rule of F7-01. |
| F6-04 | Fixed as a warning and a comment; the behaviour is as described in the text and is safe against two writers by the lease. |
| F6-05 | Fixed. `wmu` is taken before `mu` in all three writers; the test checks the exclusion for each writer. No lock-order inversion found (wmu is never taken under mu). |
| F6-06 | Gaps 1 and 3 are covered now (`promote:slowlock`, `TestAnAbandonWaitsForAPromotionThatIsRunningInsideTheLock`, the old leader restart test). Gap 2 is covered by a unit test of the rollback. New gaps: F7-04. |
| divergenceBytes | Truthful in kind, off by a record in size (F7-05). The state is read after the dry run, which has finished the crash recovery, so "shut down" is the real condition; a directory that did not finish its recovery gives the lower bound or no number, and the cluster test accepts only a number or the documented message. |

## New findings

### F7-01 (high) The loop resumes the old leader on the evidence of ANY standby in recovery; the promoted standby can be the one it cannot reach

Where: cmd/kingresd/mapheldback.go:144-167 (`resolveHeldBack`), :96-121 (`localStandbyState`); test
cmd/kingresd/mapheldback_test.go:195 ("the other server does not answer" expects a resume).

Scenario. Three servers: A old leader, B candidate, C hosts a second standby of the map group. The planned switchover completes: B is promoted, B is
the primary of group 0 and takes writes, B's bootstrap journal is done. A's marker stays until A is rebuilt as a standby (`BecameStandby`), and
nothing bounds that time. B's server fails (or is cut off from A and from C and stops renewing; its watchdog fences it). The loop of A (10 s) asks: B
does not answer (skipped, no state), C answers "in_recovery" (it follows B's timeline), so `proven` is 1, nothing is Active, nothing says "primary".
`endTheHold` runs, A starts the stale directory as primary as soon as it wins the lease that B's failure freed, which can be before the failover of the
map group promotes C. All writes that B acknowledged after the switchover are lost, the catalog (in group 0) goes back to the state before the
switchover, and C's standby now follows a timeline that A's does not hold. The same happens with one server unreachable while the candidate is in the middle
of its bootstrap (the candidate's journal is not seen), if its lease lapses because it is cut off from the others as well. The same weakness: a standby
reported as "unknown" does not block the resume while another server shows "in_recovery"; and a stale or orphaned standby directory on any server that
reads "in_recovery" counts as proof.

Why the lease is not enough. It decides who runs, not whose data is right. The hold was the guard against reviving a stale primary after a hand-over; the
loop removes it on weaker evidence than the hold needed.

Suggested fix. The old leader records the candidate in the marker or a file at the demote (the task knows `nw.Server` and the standby id; the same call
that sends `MapStart`). The loop resumes only when the recorded candidate itself answers: "in_recovery", no active bootstrap, and its journal for this
bootstrap given up or never begun; an unreachable or unknown candidate blocks the resume, and a candidate that cannot answer for a long time goes to an
operator, not to a timer. For the operator's call and for a hold without a record, require that every other member answered and that none says "primary",
"unknown", or Active; an unanswered member blocks. Test: the table of `TestTheOldLeaderEndsAHoldThatNothingOwns` with ("s2 in_recovery", "s3 down") and
("s2 in_recovery", "s3 unknown") expecting no resume.

### F7-02 (medium) The abandon goes on after the release of the lease failed; a promoting call that waited for the lock then finds the lease held

Where: cmd/kingresd/mapfailover.go (`abandonLocalFull`: `releaseFence` error is logged and the function goes on, answers abandoned, asks the old leader to resume).

Scenario. The runner's promote call is blocked at `LockLifecycle` in `handleFailoverPromote` behind something else on the agent (a rebuild, a probe), or has
just reached it. The abandon takes the lock, sees the standby in recovery, marks the journal abandoned, and `LeaseRelease` fails (a majority of grantors
cannot be reached, or the context ends). The abandon unlocks and answers "abandoned"; the old leader is resumed. The queued promote call gets the lock, the
lease check passes (the lease was never released), `pg_promote` runs and `attachPromotedInjected` attaches the instance. Result: a promoted writable
primary of group 0 on the candidate, a journal that says abandoned, no catalog follow-up, and the old primary waiting for a lease that the promoted instance
renews. No second writer (the lease holds), but the system database is unusable until an operator rebuilds, which is what F6-02 wanted to close.
The handler does not consult the journal, and the lease is the only thing that stops it.

Suggested fix. Treat a failed release as a failed abandon: undo the mark (or do not mark until the release succeeded) and answer 503 so that the caller
retries; or let `handleFailoverPromote` for group 0 refuse when `MapFailover.journal` is present and not active. Test: `releaseFence` returning an error with
a promote call queued behind the lock; expect no promotion.

### F7-03 (low) The 10 s loop and the operator call have no bound or visible state for a hold that cannot be resolved

Where: mapheldback.go `WatchHeldBack` (logs at debug level only).

When the loop keeps answering "no standby could be shown to be in recovery" (for example the only standby host is down), the primary of the system database stays
held back with nothing at warning level and nothing in the API. After F7-01 is fixed this state becomes more likely, so it needs a warning every minute or so that
names the reason and the operator's call. Fix: log at warn level once per minute with `why`, and expose it in the failover status.

### F7-04 (low) Test gaps that survive mutants

1. M11b: letting go of the lifecycle lock before the lease release in `abandonLocalFull` is not caught by any test. The F6-02 closure depends on that order.
2. M18b: the loop is not started in main.go and no test fails (the wiring is not tested; the loop tests call `WatchHeldBack` directly).
3. TestTheAbandonWaitsForAPromotionThatIsRunningAndThenRefuses failed once in one of 21 mutant runs (a 200 ms wait under load); 30 repeats without a
   mutant all passed, so it looks timing sensitive but not broken.

### F7-05 (low) `divergenceBytes` is measured from the START of the shutdown checkpoint record

Where: internal/standby/oldprimary.go `endOfOldWal`, `completeFromRewind`.

"Latest checkpoint location" is where the shutdown checkpoint record begins, not where the WAL ends, and that record was written by the recovery after the
old primary's last write. The number is therefore smaller than the true end by the size of that record (about 100 to 150 bytes), and the message says
"measured". Not a lie about what was lost, but "measured" is slightly strong. Fix: say "at least" in all cases or add the checkpoint's record length.

## Verified as correct

- Every path of the new code ends in the lease manager's `Resume`, which does not start the instance; the start waits for the lease (README decision 7).
- `handleMapBootstrap` answers only the server that is named in the query (`old` equals the authenticated caller), and Active requires that the journal
  names the caller as old server.
- Ordering: journal on the candidate (MapStart) before the marker on the old leader (Demote); promotion intent on disk before pg_promote; abandon
  refuses (409) when the intent exists and the standby is not shown in recovery; the runner re-reads the journal in its three places; the old
  leader is asked to resume only after the journal is abandoned.
- The watchdog file writers take wmu before mu in all three places; no inversion.
- No machine-local paths and no non-ASCII text in the new files and tests that I read.

## Mutants

All run with the unit tests of the package named; restored with git checkout after each. K = killed.

| Mutant | Result |
| --- | --- |
| M1 loop ignores Active | K, TestTheOldLeaderEndsAHoldThatNothingOwns and TestTheOldLeadersLoopLeavesAHoldThatHasAnOwner |
| M2 loop ignores "primary" | K, TestTheOldLeaderEndsAHoldThatNothingOwns |
| M3 no proof of a standby in recovery needed | K, same |
| M4 no held-back check in resolveHeldBack | K, same and TestTheResumeCallSaysWhyItDidNothing |
| M5 Active for any old server (build error first, redone as M5b) | K, TestTheBootstrapStatusIsAnsweredToTheOldServerOnly |
| M6 caller check of handleMapBootstrap removed (redone as M6b) | K, same |
| M7 "in production" not read as primary | K, TestTheControlDataTellAStandbyFromAPrimary |
| M8 control data fallback always says in recovery | K, two tests |
| M9 no lifecycle lock in the abandon | K, TestTheAbandonWaitsForAPromotionThatIsRunningAndThenRefuses |
| M10 no resume of the old leader after an abandon | K, TestAnAbandonedBootstrapLetsGoOfItsLease and one more |
| M11 lock held longer (not a real defect, equivalent) | survived, expected |
| M11b lock let go before the lease release | SURVIVED (F7-04) |
| M12 journal accepts an intent after the abandon | K, TestThePromotionIntentIsRefusedAfterTheAbandon |
| M13 runner does not re-read the journal before a repeat | K, TestAGivenUpBootstrapDoesNotRepeatThePromotingCall |
| M14 runner does not re-read between steps | K, TestABootstrapGivenUpBetweenTwoStepsGoesNoFurther |
| M15 runner does not re-read in the lease wait | K, TestABootstrapGivenUpWhileItWaitsForTheLeaseDoesNotPromote |
| M16 runner does not ask the old leader to resume | K, TestARefusedProofAsksTheOldLeaderToEndItsHoldAfterTheJournalIsGivenUp |
| M17 endOfOldWal says shut down always | K, TestOnlyAShutDownDirectoryHasItsCheckpointAsTheEndOfTheWal |
| M18 / M18b loop not started in main | SURVIVED on rerun (the first failure was the flaky test of F7-04) |

A mutant that the table does not have: one that makes the loop need ALL members to answer. It would fail the existing case "the other server does not
answer", which is exactly the case F7-01 says is wrong.

## Cluster tests

Run in lane l4 on a private PostgreSQL 18 copy with the extensions of e279c14091, tip unmodified: TestSwitchoverOfTheMapGroupWhoseCandidateCannotBeAskedIfItIsStillAStandby (533 s), TestSwitchoverOfTheMapGroupWhoseOldLeaderRestartedAndTheProofIsRefused (260 s), TestAnAbandonWaitsForAPromotionThatIsRunningInsideTheLock (210 s; the abandon answered 409 while the promotion ran): all PASS. No cluster test was run under a mutant and PostgreSQL 19 was not run. The scenarios of F7-01 and F7-02 are not covered by any test (the first needs a partition between the old leader and the candidate with a third standby host).


---------------------------------------------------------------------------------------------------------------------------------------

## Fixes

Fixed on the branch of the review (fo-r2wip). For each fix a test fails without it (the mutants are listed below).

| Finding | Commit | What changed |
| --- | --- | --- |
| F7-01 | 8b8cdb4f27 | The clean stop of the map group names the server the group is handed to (`candidate` of the demote call); the old leader records it on disk (`candidate.0` in the lease directory) before it stops and removes it with the hold. The old leader resumes only when that server answers and shows its standby in recovery, no bootstrap that names it is active anywhere, and no standby was promoted. An unreachable candidate, or one whose standby cannot be read (unknown, or none), blocks. Without a record, and for the operator's call (`strict` in `POST /failover/map/resume`), every server must answer and none may say unknown. The test table of the old leader's rule has the reviewer's cases: the candidate down with a second standby in recovery gives no resume. A cluster test of the scenario was not written (it needs a partition between the old leader and the candidate and a third standby host); the rules are tested with fakes, and each rule says its own reason, which a test checks. |
| F7-02 | 8b8cdb4f27 | Both fixes the review offers: a lease that cannot be let go of fails the abandon (503, nothing answered as given up, the old leader is not asked to resume; the repeat of the abandon finishes the release), and the promoting call refuses a journal that was given up (it is consulted under the lifecycle lock, where the abandon marks it). |
| F7-03 | 8b8cdb4f27 | A hold that cannot be resolved is named in the log at warning level once a minute with the reason and the call an operator can make, and the failover status of that server is `paused` with the reason. The system database is the held-back primary, so no alert can be recorded and the status can only be read where a session can be made; the log line is the signal that always exists. |
| F7-04 | 8b8cdb4f27 | M11b: a test that the lifecycle lock is still held when the lease is let go of. M18b: a test of the wiring (main.go starts the loop; the promoting handler asks the journal); the loop tests call it directly. The timing-sensitive test of the abandon (200 ms wait) is left as it is: 30 repeats pass. |
| F7-05 | 6170828196 | The report no longer says "measured": the number is at least the distance to the shutdown checkpoint (its own record is not counted), zero with the message "it wrote nothing after the divergence but its shutdown checkpoint" when the old WAL ends before the divergence. The contract text says so. |

### Mutants of these fixes

All killed, each by the test named in the run (`mutate9.sh` of the lane work, unit tests of the package): the candidate that does not answer, the final check of the candidate, an unknown standby of the candidate, an unanswered server without a record, the operator's call that is not strict, a clean stop that does not name the candidate, an ordinary group that names one, a record that outlives the hold, a failed release that does not fail the abandon, a repeat that does not finish the release, a promoting call that ignores the journal (two forms), no warning, a warning at every look, no status, a lock let go before the release, a loop that main does not start, and a number called measured. Three of the first ones survived until a test asserted the reason of each rule separately (the rules cover each other, so one lost rule changes no outcome); the reasons are asserted now.

### Cluster tests of the fixes (lane l3, PostgreSQL 18 and 19.0, extensions of e279c14091)

Tip 6170828196: the three new cluster tests of F6 pass on both majors; the switchover and abandon set passes on PostgreSQL 18; the failover, old-primary and map-group set passes on both
majors. The switchover and abandon set on PostgreSQL 19.0 had one failure, `TestSwitchoverOfTheMapGroup` (timed out after 10 minutes waiting for the task on the new leader, which answered
503 "The manager leader cannot be reached"); the other tests of that step passed. The log tail that the test printed did not show the cause. The test passed in 4 of 4 reruns alone
(3 on PostgreSQL 19.0, 1 on PostgreSQL 18), so it did not reproduce; the cause is NOT known and it may be a timing flake or a rare effect of the F7 changes. The failure output of the
test was enlarged (up to 120 KB per node, filtered) so that the next occurrence can be read. No cluster test of the scenario of F7-01 (a partition between the old leader and the
candidate with a third standby host) exists; it is covered by unit tests with fakes.
