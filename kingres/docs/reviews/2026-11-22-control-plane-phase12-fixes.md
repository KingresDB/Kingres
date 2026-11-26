<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
# Review of the fixes of the control plane of phase 12 (branch p12-cp, bdd4fc2f4b, c68226f22a, 22a40eb52f on ac408d28bd)

Reviewed on a detached worktree at 22a40eb52f. Paths are relative to kingres/control-plane unless they start with kingres/.
Go unit tests of ops, partsched, instance, task and httpapi pass on Windows. PostgreSQL-backed runs: PostgreSQL 18, private install with the five
extensions built from 22a40eb52f, lane l4. PostgreSQL 19 was not run (the machine was loaded; the fixes have no major-specific code, but this is not verified here).

Summary: no critical finding. All 16 findings are fixed in the sense that the reported scenario no longer happens and a test fails without each fix
(see the mutants, with the exceptions named). Four new findings of medium severity and five of low severity. The decision for P12C-04 is sound as a
trust decision but the stated guard ("the owner needs CREATE on the schema") is not a guard.

## Verdicts on the findings

| Finding | Fixed | New defect from the fix | Test fails without the fix |
|---|---|---|---|
| P12C-01 asOf unbounded | yes for a request; only partly for a clock jump | P12D-01, P12D-08 | yes (mutant M1 killed) |
| P12C-02 failed runs never finished | yes for runs that exist; not for failures before a run exists | P12D-02 | yes (M3, M7, M12 killed) |
| P12C-03 preload list | yes for the local instance and the primaries of other servers; standbys not covered | P12D-03, P12D-07 | yes for the list (M8 killed); the reload part has no test (M-reload survived) |
| P12C-04 non-superuser owner | yes | none in the code; see the security judgement | yes (M5 killed) |
| P12C-05 trust documented | yes, but the text is inaccurate and the guard is weak | P12D-04 | text |
| P12C-06 columnar options | yes | none found | yes (test passes; mutants not run) |
| P12C-07 lock_timeout | yes, with a residual stall | P12D-05 | yes (limit and give-up tested); the "reader is not blocked" assertion is checked only after the task ended |
| P12C-08 missing run | yes | none found (a crash edge, P12D-09) | yes (test present) |
| P12C-09 alerts of unread databases | yes | P12D-06 | yes (mutant killed) |
| P12C-10 scheduled run needs no library | yes | none found | indirectly |
| P12C-11 reads do not install | yes | none found | yes (test present) |
| P12C-12 limit in text and 404 | yes | none found | yes (test present) |
| P12C-13 put during a run | yes | none found | yes (test present) |
| P12C-14 ActiveFor | yes | fail-open on a store error (P12D-09) | yes (test present) |
| P12C-15 subject and 55P03 | yes | none found | yes (test present) |
| P12C-16 smaller points | yes | none found | yes for the version compare |

## Answers to the specific questions

### The five-minute asOf rule
- The rule compares asOf with now() of the instance the service connects to. That instance runs on the same host as the leader process (the service
  connection is the local instance), so both read the same operating system clock. A real clock jump of the host (a step by NTP, a resumed VM, a
  wrong date set by hand) moves both: the scheduler's asOf and now() agree, the check passes, and retention runs on the jumped time exactly as before.
  The fix covers the request (the 2100 call) and a clock difference between the leader process and the instance, which is the case the test builds by
  hand. The claim in the Fixes section "a leader whose clock jumped starts nothing" is therefore true only for that second case (P12D-01).
- A plan with a free asOf (planPartitionPolicy) is not stored, so there is no plan to apply later; the same asOf given to a run is refused after five
  minutes. That is usability, not a defect, but the contract text should say that a replay at another time cannot be executed.
- A manual run up to five minutes in the past is accepted. next_run_at is then as_of plus run_every, a few minutes early; harmless.

### The OnFail hook
- Double close: not possible. failRun marks and closes, then the hook reads the state through runInfo and returns when it is not running; finishRun is
  idempotent; the finish step counts the run only when it was running (but see the mutant on this guard below).
- A run closed while another leader continues it: the engine does not run the hook for a holder that lost its lease (documented in the task engine and
  in the code of onFail), so a leader that took over is not affected. A cancel recorded by the new leader does close the run, which is intended, and
  a late action of the old holder's session is then protected by the extension's claim lock.
- The registration of the hook in the task definition is not tested (mutant M2 survived): the test calls maintainOnFail directly.

### The library detection
- pg_config is run from the manager's own binary directory (m.run joins m.bin), so it reports the pkglibdir of the same installation whose postgres is
  started. A mix of a PATH pg_config and another postgres is not possible here. Good.
- When pg_config cannot be run the library is assumed present: the old behaviour; a failed start is then possible only as before.
- The reload marks the setting pending a restart in PostgreSQL itself (a postmaster-level setting changed in the file). The reload branch has no test
  (mutant "no reload" survived; the instance tests use a fake pg_ctl that never reports a running instance), so nothing proves that a running
  instance shows the pending restart.
- The check "every server has loaded the library" asks the home instance of each server (current_setting of shared_preload_libraries through the
  service connection). A standby instance is not asked (P12D-03).
- A list without kingres_columnar is written by every WriteManaged when the file is missing at that moment. During a package upgrade (old library removed
  before the new one is installed) the file can be absent for a short time and kingresd would write the shorter list, though columnar tables may exist
  (P12D-07).

### Typed blockers and dryRun
- The preview and the real call share checkSetStorage and storageRefusals, so the blockers are the same list, produced by the same code, including the
  convert_refusal of the extension for a partition. Differences found by reading: (1) a refusal that PostgreSQL gives only at the ALTER (an index the
  format cannot take, a trigger on a plain table) is in neither; the contract says so. (2) For a partition in a database without kingres_timepart the
  preview can only say that the reasons are known after the install. (3) A request that asks for the format the table has already: the preview says
  willConvert false, the real call starts a task that ends "already stored as". (4) tableConversionReasons swallows a query error (one() returns
  false), so a transaction that is aborted by the first query hides the next reasons in both. None of these makes the preview promise something
  the real call refuses at request time.

### lock_timeout retry and its ten-minute bound
- The loop retries only on 55P03, rolls the transaction back each time, and fails with operation_in_progress, retryable, after ten minutes; the bound is
  checked only after a try fails, so a slow rewrite is not cut. A cancelled context ends the loop. Correct.
- Residual stall of readers (P12D-05). The ALTER's request for ACCESS EXCLUSIVE queues for up to lock_timeout (3 s), and every reader that arrives in
  that window queues behind it. Measured on PostgreSQL 18 with a holder of ACCESS SHARE: the worst latency of a plain SELECT was 2.99 s, repeating
  every 5 s (3 s of queue, 2 s of pause) for the whole wait. "A reader of the table is never queued behind a conversion" (README, Fixes) is not true.
- The analyze step uses the same loop after the table is already converted; if it gives up, the task fails though the conversion is done (a repeat
  ends with "already"). Acceptable; the message does not say that the conversion is done.

## The security judgement on trust_table_owners everywhere (P12C-04)

Who can make the control plane run whose code, and with what privilege:
- Partition path (convert_partition, and every run of a policy): the code of a non-superuser owner (a check or partition-constraint expression, an
  index expression, a default) runs inside a SECURITY DEFINER function owned by that owner, created in pg_temp of the service session, so it
  runs with the owner's privileges (current_user is the owner), while session_user is the service superuser. PostgreSQL refuses SET ROLE inside it.
  The leftovers that the extension's own comment lists (a deferred trigger, a temporary object, a session setting, a cursor WITH HOLD, a prepared
  statement, LISTEN) are the way from the owner's privilege to the service role's, because they run after the function returns.
- I tried the strongest of them on PostgreSQL 18: a deferred constraint trigger of the owner on the table, then convert_partition with the setting on.
  The extension refuses it (an AFTER ROW trigger cannot be on a columnar table; a trigger on the partition itself is refused too). I found no way
  to leave residue through that path, so I did not find a working escalation. I also tried the plain-table path (ALTER TABLE ... SET ACCESS METHOD run
  by the superuser connection): a stored generated column whose function the owner redefined was not executed by the rewrite, so no owner code runs there
  and the setting has no effect on that path.
- The scheduler repeats the partition path forever for any table that has a policy, without a person in the loop each time: an owner who obtains a
  policy gets the chance at every run. The policy is put by a kingres_admin, and the API does not look at who owns the table.
- Judgement: the decision is sound if the sentence "trusted as the administrator is" is a real rule of the operator, and it is the same rule the
  extension demands, so one rule for both paths is better than two. It is not a safe default for a multi-tenant cluster. "The owner needs CREATE on the schema" is
  not a guard: it is a functional prerequisite (the new partition is created in that schema), and an owner of a tenant table owns or can create in its
  schema, can create temporary objects with the default TEMP privilege, and can create functions. It adds no isolation. The text in the contract
  and the README ("runs as the service role") is also not exact: the code runs as the owner in the session of the service role, and the risk is the
  residue (P12D-04).

## New findings

### P12D-01 (medium) the skew rule does not protect against a real clock jump of the host
- internal/ops/maintain.go:106-114 (PrepareMaintain), internal/partsched/partsched.go (asOf of a tick), kingres/api/openapi.yaml (text of runPartitionPolicy).
- Scenario: the host clock is stepped one year forward (NTP step, resumed VM, date set by hand). The scheduler's asOf and the instance's now() are the same
  clock, so the difference is zero and the check passes. The run retires every partition older than the retention of the jumped time and parks
  next_run_at after it. When the clock steps back, the policy is parked a year into the future (it is not due) until the next manual run.
- Fix: bound the time of a run against the last run of the policy as well: refuse a run whose asOf is later than the last run's as_of plus a few
  run_every (and report the policy as "clock jumped" in the alert), or retire only partitions that the last successful run's time would have retired
  plus one period. Reword the text of the finding and the README: the rule bounds the request and a difference between the leader process and the
  instance, not a clock jump of the host.

### P12D-02 (medium) the task storm continues when begin fails before a run exists
- internal/ops/maintain.go: stepMaintainBegin, recentlyFailed (the back-off reads kingres.timepart_run), maintainOnFail (returns when runOfTask is 0).
- Scenario (reproduced on PostgreSQL 18): kingres.timepart_begin_run raises for every call (permission, a missing function after a package problem, a
  lost connection that stays). Four ticks gave four tasks, zero runs, and the policy was still due. No run row exists, so neither the closing of
  the run, nor the five-minute back-off (which looks for a failed run) applies. This is the P12C-02 storm for the errors that come before the run is made;
  it is at most one task per tick (1440 a day), each failing.
- Fix: let the scheduler keep the back-off per table in memory (or in the task store: the last failed task of the subject), and count a failed
  partition.maintain task as the reason to wait, not only a failed run.

### P12D-03 (medium) the "every server has loaded the library" check does not cover standbys
- internal/ops/storage_task.go:ensureStorageExtensions, internal/ops/extensions.go:207 (Preloaded), cmd/kingresd/config_wiring.go:65.
- Scenario: the upgrade rewrote the list on a standby (EnsureConfig runs for every instance of the server) but the standby was not restarted; the
  primary was restarted and loads the library. The check passes for the primary (the home instance of its server), a conversion to columnar is
  accepted, and the standby cannot replay the WAL of the columnar resource manager. The README states this very risk and says that the order
  "standbys first" is the safe one; nothing checks the standbys.
- Fix: ask each standby instance of the group as well (SHOW on the standby through the manager of its server), or read pg_settings.pending_restart
  for shared_preload_libraries on every instance of the cluster, and refuse the conversion while any instance is pending.

### P12D-04 (medium) the trust text and the guard
- kingres/api/openapi.yaml:1923, 2025-2026, README.md section "One rule for the owners of tables".
- The text says the code "runs as the service role" and that "the owner needs CREATE on the schema". It runs as the owner inside the session of the service
  role, and the CREATE requirement is not a guard (see the judgement above). An administrator who reads it does not learn that a policy on a tenant's
  table is a standing, scheduled invitation to try the residue paths, or that the API does not check the owner.
- Fix: say what the extension says; add a cluster setting (default off) that allows a policy and a conversion on a table whose owner is not a
  superuser, refuse with a clear problem when it is off, and show the owner in the policy and in the storage preview.

### P12D-05 (low) readers are queued for up to lock_timeout during every try
- internal/ops/storage_task.go:withLockRetry and stepStorageConvert.
- Measured: a SELECT waited 2.99 s, every 5 s, during the whole wait for a holder of ACCESS SHARE. The text "a reader of the table is never queued behind a
  conversion that waits for a long transaction" is wrong.
- Fix: a shorter lock_timeout (a few hundred milliseconds) with a longer pause, or poll pg_locks and ask for the lock only when no conflicting holder
  exists; correct the text.

### P12D-06 (low) alerts of a database that is gone or has no extension any more are never cleared; the subject format changed
- internal/ops/partition_backend.go:databasesWithTimepart and Checks, internal/partsched/partsched.go:CheckAlerts, internal/monitor/alerts.go:parseSubject.
- A dropped database or a dropped kingres_timepart is not in the list of databases that were read, so its open alerts stay for ever (before the fix they
  were cleared). Open alerts in the old subject form (|table:) are cleared once and raised again in the new form, which loses an acknowledgement at the
  upgrade. parseSubject unescapes the table parts but not the database part, which alertKey escapes: a database name with '|' or '%' shows escaped.
- Fix: also clear alerts of databases that are not in pg_database, and of databases where the extension is absent (a read that found no extension is a
  successful read); unescape the database part in parseSubject.

### P12D-07 (low) a transient absence of the library file shortens the preload list of an instance that may have columnar tables
- internal/instance/configfile.go:preloadList, libraryExists; internal/instance/manager.go:WriteManaged.
- Scenario: during a package upgrade the file is missing for a moment; any WriteManaged (a setting changed in the console) in that moment writes a
  list without kingres_columnar and, in EnsureConfig, a reload. The next restart starts without the library; columnar tables cannot be read.
  Before the fix the same restart would not start at all, so it is a trade, but it fails silently.
- Fix: never remove the library from the list of an instance whose database has the kingres_columnar extension installed (warn and keep it), and
  give the list without it only to a new instance.

### P12D-08 (low) the leader's clock for Due and the instance's clock for the check can disagree in the other direction
- internal/partsched/partsched.go (Due(asOf) with the leader's clock), internal/ops/maintain.go:PrepareMaintain.
- If the leader process clock is more than five minutes ahead of the instance's (separate hosts or containers), Due selects policies that
  PrepareMaintain then refuses every tick with a validation problem: logged, counted as a failure, no run, no progress until the clocks agree. This is
  the intended refusal but it is reported as a failure of every policy; say so once (one warning, one alert "clock differs").

### P12D-09 (low) smaller points
- internal/ops/partition_policy.go:maintainRunning returns false when ActiveFor fails (fail open): a delete or a put goes ahead during a run when the
  task store is not readable. Return an error instead.
- internal/ops/maintain.go:stepMaintainBegin: a begin that crashed after the run was recorded as failed but before it was closed returns nil on the
  next pass ("the run of an earlier pass"); the phases do nothing, the finish step closes the run, and the task is done although the run failed and
  the reason is lost. Check the state in that branch too.
- internal/ops/maintain.go:recentlyFailed looks at any failed run in the last five minutes, not at the last run: a manual run that succeeded after a
  failure does not lift the back-off.
- The tests of the fixes use a fake pg_ctl that is never running, so the reload in EnsureConfig and the hook registration of the kind are unproven.

## Mutants run

All on 22a40eb52f in a detached worktree, restored by git checkout; the probe file was deleted.
PostgreSQL 18 (lane l4) unless marked "Windows".

| Id | Mutant | Result |
|---|---|---|
| M1 | skew check disabled (PrepareMaintain) | killed (TestIntegrationAManualRunMustBeForTheTimeOfTheServer) |
| M2 | OnFail not registered in the task definition | survived (the test calls maintainOnFail directly) |
| M3 | begin does not close a run recorded as failed | killed (TestIntegrationAFailedBeginMovesTheScheduleAndNothingStorms) |
| M5 | trustedConn sets trust_table_owners off | killed (TestIntegrationAPartitionOfANonSuperuserOwnerConverts) |
| M7 | recentlyFailed always false (no back-off) | killed (TestIntegrationAFailedStepClosesTheRun) |
| M8 | libraryExists ignored (list always full) | killed (TestPreloadLeavesOutALibraryThatTheInstallationLacks) |
| M9 | 42501 not mapped to conversion_refused | survived (only the success case of a non-superuser owner is tested) |
| M11 | finish step counts the run at every call (no "first close" guard) | survived (no test reaches the finish step of a failed run and reads the metrics) |
| M12 | failRun does not call finishRun | killed (TestIntegrationAFailedStepClosesTheRun) |
| M-reload | EnsureConfig does not reload a running instance | survived (no test with a running instance) |
| M-alerts | CheckAlerts clears alerts of unread databases (Windows) | killed (partsched_test) |

Probes (not kept):
- Begin storm: begin_run renamed so it raises; four ticks gave four tasks and no run, policy still due (P12D-02).
- Reader latency under a conversion that waits for ACCESS SHARE: worst SELECT 2.99 s (P12D-05).
- Owner-code residue: a deferred constraint trigger on a table of a role without superuser, then convert_partition with trust on: refused by the
  extension (no escalation found). A plain-table rewrite with an owner-defined generated-column function: the function was not run.

## Fixes (2026-11-22, branch p12-cp, second round)

All nine findings are fixed. Tests run on PostgreSQL 18 and 19.0 (private installations).

| Finding | What was done | Test |
|---|---|---|
| P12D-01 | A scheduled run is compared with the history of the policy: more than seven days plus `create_ahead` after the last run (or `updated_at`) is a `ClockError`, one alert, no task. A run by hand is bounded by the skew only. | `TestIntegrationAScheduledRunFarAfterTheLastRunIsRefused` |
| P12D-02 | `OnFail` with no run moves `next_run_at` by the back-off on the policy; the process remembers the failure by table; a begin that finds a failed run closes it. | `TestIntegrationABeginThatRaisesBacksOffAndMovesTheSchedule` (four ticks, one task) |
| P12D-03 | The library check asks every local instance (standbys too) through `PreloadedOnAll`; the message names the instance and says to restart standbys first. | `TestStorageNeedsTheLibraryLoadedOnEveryServer` (extended) |
| P12D-04 | Cluster setting `analytics.allowNonSuperuserOwners` (default off): refusal with `owner_not_trusted` / `partition_policy_conflict`; the scheduler records one issue and one alert, no retry. Trust text corrected in the contract and README. | `TestIntegrationANonSuperuserOwnerIsRefusedUnlessTheSettingIsOn`, `TestIntegrationAConversionOfANonSuperuserOwnerIsRefusedUnlessTheSettingIsOn`, `TestUntrustedOwner` |
| P12D-05 | `lock_timeout` 200 ms with a 3 s pause between tries; the README says what remains. | existing lock-wait test |
| P12D-06 | Alerts of dropped databases and extensions are cleared; only a database that could not be read keeps its alert. | `TestAnAlertOfADroppedDatabaseIsCleared`, `TestAnAlertOfADatabaseThatWasNotReadIsLeftAlone` |
| P12D-07 | A marker file keeps the library in the list of an instance that had it. | `TestPreloadLeavesOutALibraryThatTheInstallationLacks` |
| P12D-08 | A clock or owner refusal is one issue and one alert, not a failure per tick. | `TestAClockOrBlockedPolicyIsOneAlertAndNotAFailurePerTick` |
| P12D-09 | `maintainRunning` fails closed; the back-off looks at the last run only; tests for OnFail, 42501 and the reload. | `TestMaintainRunningFailsClosed`, `TestIntegrationBackOffLooksAtTheLastRunOnly`, `TestMaintainDefinitionHasTheHookAndTheSteps`, `TestEnsureConfigReloadsARunningInstanceOnlyWhenTheListChanged` |
