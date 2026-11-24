# Review of the control plane of phase 12 (branch p12-cp, 7d43a79a3f..ac408d28bd)

Reviewed on a detached worktree at ac408d28bd. Go unit tests of partsched, ops, httpapi and instance pass. PostgreSQL-backed probes ran on PG 18 and PG 19
(private installs with the five extensions built from ac408d28bd), lane l4. Paths below are relative to kingres/control-plane unless they start with kingres/.

Summary: no critical finding. Four high findings (P12C-01 to P12C-04), several medium, and low items. The three deviations are judged at the end.

## Findings

### P12C-01 (high) runPartitionPolicy takes an unbounded asOf: one request can drop all partitions and park the policy
- internal/ops/maintain.go:84-90 (PrepareMaintain), internal/httpapi/handlers_analytics.go (runPartitionPolicy passes req.AsOf), kingres/api/openapi.yaml:2078.
- Scenario (reproduced on PG 18 and 19): policy P1D, retention P5D, drop. A kingres_admin calls runPartitionPolicy with asOf 2100-01-01. The task is done; the table has
  0 rows, partitions exist only around 2100, and next_run_at is 2100-01-01 01:00, so the scheduler does not touch the policy again for 74 years. The contract calls asOf "for tests and dry
  replays", but this run is neither dry nor reversible. A forward jump of the leader's clock does the same through the scheduler (asOf is the leader's clock).
- Fix: refuse an asOf later than now plus a small skew (for the real run) and one earlier than now minus run_every; keep free asOf only on planPartitionPolicy, which is a dry run.
  Better: compute asOf in the task from the instance's now() and drop the field from the manual run. Reword the contract.

### P12C-02 (high) a run that fails before it can finish never moves next_run_at: the scheduler starts a new task and run at every tick, forever
- internal/ops/maintain.go: stepMaintainFinish (state != running returns without calling timepart_finish_run), stepMaintainPhase (failRun then returns an error, so the finish step never runs).
  The extension moves next_run_at and prunes old runs only in timepart_finish_run (contrib/kingres_timepart/kingres_timepart--0.2.sql, timepart__finish_run).
- Scenario (reproduced on PG 18 and 19): the owner of the table is changed (ALTER TABLE ... OWNER TO) after the policy was set. begin_run records the run as failed
  (owner_changed). Five scheduler ticks give five tasks and five failed runs; next_run_at stays in the past. That is 1440 tasks and run rows per day, and pruning never runs, so
  timepart_run grows without bound. The same loop follows any step error (lost connection, an unexpected exception in a phase). Also policy_unverified.
- Fix: call kingres.timepart_finish_run when the run is failed too (it accepts a failed run and sets next_run_at and prunes), and call it from a failure path of the task as well
  (a compensation or a deferred call). As a second line, give the scheduler a back-off per table after a failed task.

### P12C-03 (high) the preload change reaches an existing cluster silently, with no restart order and no check of the standbys or of the library file
- internal/instance/configfile.go:146-158 (EnsureConfig), cmd/kingresd/config_wiring.go:65 (the result "changed" is ignored, nothing is logged), internal/ops/storage_task.go
  (ensureExtensions checks Preloaded on the leader's own instance only).
- (a) At upgrade, kingresd rewrites kingres-managed.conf and says nothing: no log line, no alert, no restart-required marker. Operators learn of it from a 409 on the first conversion.
  The packaging README itself says that once columnar WAL exists the library must be preloaded on every standby, or it "may fail to start or to replay". If the primary is
  restarted first and a conversion is done, a standby that was not restarted cannot replay WAL of the columnar resource manager. Nothing in the code orders the restarts or
  checks the standbys.
- (b) Observed: a PG 18 install without the library file (the first install of my run had only kingres_shard and kingres_ledger) made every instance started with the new
  list fail with "pg_ctl: could not start server". The same happens in production if kingresd is upgraded and the PostgreSQL package is not (or an image mixes versions), and
  the first restart (even a crash restart) takes the cluster down.
- Fix: log a warning and raise an alert (or a pending-restart marker visible in getConfigSummary) when EnsureConfig rewrites the list; rewrite only when the library exists in
  pkglibdir (otherwise keep the old list and say so); make the Preloaded check cover the standbys of the group, or document and enforce "standbys first"; keep the old-list
  case readable in listConfigServers as pending restart.

### P12C-04 (high) setPartitionStorage cannot convert a partition of a table that a non-superuser owns
- internal/ops/storage_task.go stepStorageConvert / storageRefusals open the service connection without kingres_timepart.trust_table_owners; internal/ops/maintain.go maintainConn
  sets it. The design (analytics-phase12-storage.md, near line 2673) left this decision "for the part that builds table.setStorage"; it was not taken.
- Scenario (reproduced): table and partition owned by a login-less application role. The task fails at "Convert the partition" with step_failed, "owner_code_not_trusted:
  the code of app_owner would run in the session of kingresd", retryable true. A retry fails the same way. The tests use a superuser owner, so they never see it. The same table
  converts fine through partition.maintain (columnarAfter), so the two paths disagree.
- Fix: decide once. Either set the flag in the storage connection as maintainConn does (and document what it gives away, see P12C-05), or refuse at request time with
  conversion_refused naming the owner and the remedy. Map 42501 to a non-retryable code in storageSQLError.

### P12C-05 (medium) trust_table_owners is on for every run and the consequence is documented nowhere in the API or the README
- internal/ops/maintain.go:131-141.
- The extension says (its comment before timepart__exec_as) that code of a non-superuser owner then runs inside the superuser session of the service role, and can leave behind a
  deferred trigger, a temporary object or a setting that runs later with superuser rights. Any role that owns a partitioned table can prepare such code; the administrator who sets
  the policy does not see it. The session is closed after each step, which limits the reach, but the guarantee relies on every step closing it.
- Fix: say it in the description of putPartitionPolicy and in the README; consider refusing a policy for a table whose owner is not a superuser unless the cluster setting
  allows it, and show the owner in the policy.

### P12C-06 (medium) setStorage with new columnar options on a table that is already columnar succeeds and changes nothing
- internal/ops/storage_task.go stepStorageConvert (plain table branch): ALTER TABLE ... SET ACCESS METHOD to the access method the table already has is a no-op in PostgreSQL.
- Scenario (reproduced on PG 18 and 19): a plain table converted with zstd level 5; a second request columnar, lz4, level 9 returns a done task, and kingres.columnar_options still
  says zstd 5. The contract says the operation "sets the columnar options".
- Fix: for columnar to columnar, call the options function of the extension (new stripes only, say so in the detail), or refuse with a clear problem.

### P12C-07 (medium) the conversion of a plain table waits for ACCESS EXCLUSIVE with no lock_timeout
- internal/ops/storage_task.go stepStorageConvert, the ALTER TABLE branch.
- Scenario (reproduced): a transaction holds ACCESS SHARE on the table. The task waits for ACCESS EXCLUSIVE; a plain SELECT from another session then waits behind it and times
  out. Every reader and writer of the table queues behind a conversion that waits for one long transaction. The step is interruptible, so a cancel works, but nobody is told.
- Fix: SET LOCAL lock_timeout (a few seconds), retry in a loop with a detail text ("waiting for a lock held by pid N") and a bounded total wait; same for the ANALYZE step.

### P12C-08 (medium) a run that is missing on the instance that continues the task ends the task as done
- internal/ops/maintain.go: stepMaintainPhase and stepMaintainFinish treat "" (no row in kingres.timepart_run_data) like a finished run and succeed with "nothing to do".
- Scenario (reproduced by deleting the run row after begin): create and finish steps return nil, the task is done, nothing was done. In a cluster this is what happens when the
  task is taken over by a new manager leader whose own instance is not the one that holds the run: partition_run_task (system database) names a run the new instance does not have.
- Fix: when partition_run_task names a run and the instance does not have it, fail the step with a clear error (and fail the task), not success. runState should return
  not found as an error.

### P12C-09 (medium) the alert lifecycle clears an alert when a database could not be read
- internal/ops/partition_backend.go (databasesWithTimepart skips a database on error, Checks logs and continues), internal/partsched/partsched.go:CheckAlerts clears every open alert
  without a finding.
- Scenario: partitionPolicyFailed is open for a table; the next evaluation cannot open that database (restart, connection limit). Checks returns no finding for it, so
  CheckAlerts clears the alert; five minutes later it is raised again (flapping, and a lost acknowledgement). The same on a leader change: the new leader's instance does not
  show the tables of the old one and clears their alerts.
- Fix: Checks returns the list of databases it read; clear only alerts whose database was read.

### P12C-10 (medium) a policy with columnarAfter stops all maintenance when the library is not preloaded
- internal/ops/maintain.go:92-96 (PrepareMaintain), also reached from StartMaintain at every tick.
- Scenario: after the upgrade of P12C-03, before the restart, the preload check fails; every tick returns extension_missing for the policy, logged and counted. Creation of future
  partitions and retention stop too, though only the conversion needs the library. With no default partition the inserts fail when the lookahead is used up.
- Fix: run create and retire, and let the extension refuse the conversion actions (it records them as skipped or failed). Keep the check only for putPartitionPolicy and
  the manual run's warning.

### P12C-11 (medium) the read operations install and update extensions
- internal/ops/storage_task.go ensureExtensions called by GetPartitionPolicy, PlanPartitionPolicy, ListPartitionPolicyRuns, GetPartitionPolicyRun (all kingres_readonly).
- A read by a read-only role runs CREATE EXTENSION, and, after a package upgrade, ALTER EXTENSION kingres_timepart UPDATE while a run may be in flight. These reads are not
  leader-bound (only getApproximateAggregates was added to leaderBoundReads), so on a follower they try to write to a standby and fail with a server error.
- Fix: the reads call Installed only; not installed means 404 or an empty answer. Install on write paths, not on reads.

### P12C-12 (low) the limit "only this server's instance" is not in the API text and the 404 is untrue
- kingres/api/openapi.yaml (setTableStorage, setPartitionStorage, the policy operations); internal/ops/storage_task.go storageRefusals (not_found "does not exist").
- Only the worklog and the control-plane README say it. A plain table on another group exists (listTables on that server shows it) yet the answer is 404 "does not exist".
  The console branch is not in this range; I could not check its text.
- Fix: add one sentence to each of the 11 operations, answer with a distinct problem whose detail names the limit (for example the group of the table), and show the limit in the
  console where the buttons are.

### P12C-13 (low) putPartitionPolicy during a run lets the run overwrite the new schedule
- internal/ops/partition_policy.go PutPartitionPolicy versus extension timepart__finish_run (next_run_at = as_of + run_every). deletePartitionPolicy refuses during a run; put does not.
- A policy replaced while a run is going is due "now", but the finish of the old run sets next_run_at to as_of plus the old run_every, so the new settings can wait up to one old
  run_every. Fix: refuse with operation_in_progress as delete does, or set next_run_at again after the run.

### P12C-14 (low) maintainRunning only sees the newest 200 tasks
- internal/ops/partition_policy.go:maintainRunning (eng.List(ctx, 200)).
- With more than 200 newer tasks a running run is not seen: deletePartitionPolicy goes ahead during a run. The engine's one-task-per-subject rule still protects StartMaintain.
  Fix: ask by kind and subject.

### P12C-15 (low) the conversion of a partition has its own subject
- internal/httpapi/handlers_analytics.go setStorage: subject is database/schema.partition, while partition.maintain uses database/schema.table. A conversion by hand and a run on the same table
  do not exclude each other at the task level; they rely on the extension's NOWAIT claim, and the loser gets a raw 55P03 as step_failed. Fix: map 55P03 to a clear retryable
  problem, or hold the table subject too.

### P12C-16 (low) smaller points
- internal/ops/approx.go getServerStates compares versions as strings (the reference is the highest by string order; 0.10 sorts below 0.9).
- internal/ops/approx.go: the list of servers is fixed when the task is made (a server added meanwhile is not covered); an unreachable server blocks the task with no way to cancel
  (the steps are not interruptible); a failure at the apply step leaves ALTER DATABASE SET applied on some servers and not recorded.
- Contract text: setTableStorage lists "indexes, triggers" as conversion_refused reasons but the request-time check covers ledger, distributed, publication and foreign keys only;
  the other refusals appear later as a failed task. The message "add it and restart the instance" (extension_missing) tells the operator to edit a parameter that the settings API refuses
  (config_parameter_managed); the real remedy is to restart the instance after the upgrade.
- A partition is addressed in the schema of its parent (qualified(p.Schema, p.Partition)); a partition in another schema is reported as not a partition of the table.
- The clock of the leader sets asOf for scheduled runs; a forward jump runs retention on the jumped time (see P12C-01 fix).

## Verdicts on the deviations

1. Extensions installed lazily by ensureExtensions: acceptable for the write operations (setStorage, putPartitionPolicy, run), where the user asked for the change and the install is
   idempotent. Not acceptable on the read operations (P12C-11), and the implicit ALTER EXTENSION UPDATE should be a decision of its own, not a side effect. extension_missing remains
   truthful for a package without the extension; its wording for the preload needs the fix in P12C-16.
2. Storage and policy operations on the leader's own instance: acceptable for the first release if stated plainly. Today it is in the README and worklog only, the 404 text is untrue,
   and a leader change makes a continuing task succeed without doing work (P12C-08) and clears alerts of tables it cannot see (P12C-09). With P12C-08, P12C-09 and P12C-12 fixed I accept it.
3. partition.maintain and approximateAggregates outside the exclusion triggers: safe for data, because every action is PostgreSQL DDL in its own transaction and the extension
   records and repeats actions; a failover can at worst leave a failed action and a partial run. For approximateAggregates every step is idempotent (CREATE EXTENSION IF NOT EXISTS,
   ALTER DATABASE SET, a known-answer check), so a failover only delays it. Not safe in one respect: P12C-08 (the new leader reports done). The table lifecycle tasks of phase 9.6 have
   the same absence, so this is consistent. I would not add triggers; fix P12C-08.

## Verified correct
- Injection: schema, table and partition names go through sqlx.QualifiedIdent (quoted) and are passed as text parameters cast to regclass; every policy field is a typed parameter;
  timezone refuses quotes and control characters and the extension validates it; sortBy uses a column-name pattern and is passed as text[]; the database settings are a closed list with
  a validator for each value and the value is a quoted literal; the internal call kinds are closed and strict-decoded.
- Privileges: mutating operations are kingres_admin, reads including plan are kingres_readonly in the contract, and the contract test ties handlers to it.
- Begin is idempotent: the run of a task is found through partition_run_task before a new one is made; a crash between the run and its record leaves a run that the next begin
  marks interrupted (the extension) and a new run for the same asOf. asOf is fixed when the task is made, so a takeover plans the same actions. Phases record each action before and after it.
- Two runs on one table: the extension's claim lock and the backend check give 55P03 while the first run's session lives; begin waits and says so; the engine's one task per subject
  covers the rest (PM3 below shows that this layer alone is what the scheduler test checks).
- Scheduler: no durable state, ticks skip a table with an unfinished task, one failing policy does not stop the others, a cancelled context stops the pass, alerts are raised with an
  upsert on the open alert and message refresh.
- Migrations 0035 and 0036 are additive with defaults and valid in one transaction. Contract: problem codes and statuses added match the code (409 for the 409 codes, 422 for
  extension_unavailable); the generated files are in step.
- Version check across servers: all servers must show the same default version before any install, and the verify step compares version and known-answer hash and names every server that differs.
- EnsureConfig keeps the other settings of the file (test) and does not rewrite a current file.

## Mutants run
Windows unit tests (go test of the package named):
- M1 alerts never cleared: killed (partsched tests).
- M2 tick counts a skipped policy as started: killed.
- M3 StartMaintain without maintainRunning (unit): survived on Windows; on PostgreSQL (PM3) also survived, because the engine's one task per subject returns operation_in_progress and
  StartMaintain turns that into false. The check is redundant; the scheduler test cannot tell.
- M4 ahead_low finding not mapped to an alert: killed.
- M5 isoDuration accepts anything: killed.
- M6 sortBy allowed on a plain table: survived (no unit test; the integration test refusals do not cover it).
- M7 libraryListed by prefix: killed.
- M8 partial-twice rule off: survived on Windows and on PostgreSQL (PM4): no test makes two partial runs in a row.
- M9 identRE accepts anything: killed.
- M10 Tick stops at the first start error: killed.
- M11 Tick ignores a cancelled context: killed.
- M12 the tick's asOf not passed to Start: survived (the fake backend ignores asOf).
- M13 error rate bounds widened: killed. M14 error rate allowed with exact: killed. M15 version mismatch not flagged: killed.
- Two build-failing mutants (hash check, settings validator) were discarded.
PostgreSQL-backed (PG 18, lane l4):
- PM1 EnsureConfig ignores the old preload list: killed (TestEnsureConfigBringsThePreloadUpToDate; note it is skipped on Windows, where no PostgreSQL directory is set).
- PM2 begin not idempotent: killed by TestIntegrationMaintainBeginIsIdempotent only; the interrupted-at-every-step test passed with it.
- PM3 see M3: survived. PM4 see M8: survived.
- PM5 failRun disabled: survived (no test checks that a failing phase marks the run failed).
All mutants were restored through git checkout and the probe test file was deleted before the worktree was removed.

## Probes (not part of the repository, deleted)
Reproduced P12C-01, -02, -04, -06, -07, -08 on PG 18; P12C-01, -02, -06 again on PG 19. P12C-03(b) was seen as a start failure of every instance when the library file was
absent from the install.


## Fixes (2026-11-22, branch p12-cp)

All 16 findings are fixed; the code is on `p12-cp` after ac408d28bd. Tests run on PostgreSQL 18 and 19.0 (private installations built from e279c14091).

| Finding | What was done | Test |
|---|---|---|
| P12C-01 (high) | `PrepareMaintain` compares the time of a run with the time of the instance (`now()` of the database): within five minutes (`maintainSkew`), otherwise 400 `validation_failed` on `/asOf`. The scheduler passes through the same check, so a leader whose clock jumped starts nothing. `planPartitionPolicy` keeps a free `asOf`. The contract text says so. | `TestIntegrationAManualRunMustBeForTheTimeOfTheServer` (2100, ten minutes ahead and back, the scheduler path, `next_run_at` after a legal run), cluster test (400 for 2100 and for two days ago) |
| P12C-02 (high) | A run is always closed with `timepart_finish_run`: the finish step does it for every state; a run that `begin_run` records as failed is closed in the begin step and fails the task with `partition_policy_conflict` and the reason; an error of a phase goes through `failRun` (mark failed, close); the hook `OnFail` of the kind closes a run that no step saw fail (cancel, lost lease). The scheduler also leaves a table alone for five minutes after a failed run (`maintainBackoff`). | `TestIntegrationAFailedBeginMovesTheScheduleAndNothingStorms` (the reviewer's owner change, five ticks, one task and one run), `TestIntegrationAFailedStepClosesTheRun`, `TestIntegrationOnFailClosesARunThatIsStillGoing` |
| P12C-03 (high) | `kingres_columnar` is in `shared_preload_libraries` only when the installation has the library file (`pg_config --pkglibdir`), for a new instance, a standby, every `WriteManaged` and `EnsureConfig`; `EnsureConfig` repairs a list that names a missing library, logs a warning (and a second one when the library is missing), and reloads a running instance so that the setting shows as pending a restart. `ensureStorageExtensions` asks every other server (`extension.check` with the preload check) before a conversion to columnar and names the server that must restart its instance (standbys first). The text no longer tells the operator to edit a managed setting. | `TestPreloadLeavesOutALibraryThatTheInstallationLacks` and `TestEnsureConfigAddsTheLibraryWhenTheInstallationHasIt` (a fake installation with and without the `.so`), the older EnsureConfig test, `TestLifecycle` on both majors; `TestSetStorageNeedsThePreloadedLibrary` |
| P12C-04 (high) | The rule is decided and shared: the conversion opens its connection through `trustedConn`, like the runs (`kingres_timepart.trust_table_owners` on, closed at the end of the call). 42501 is `conversion_refused` (not a retryable step failure). | `TestIntegrationAPartitionOfANonSuperuserOwnerConverts` (partition and plain table of a role that is not a superuser) |
| P12C-05 (medium) | Documented in the contract text of `setTableStorage` and `putPartitionPolicy` and in the README (what the setting gives away, that the owner needs CREATE on the schema, that only trusted owners should be given policies). No refusal of non-superuser owners: the rule is "trusted as the administrator is"; a cluster setting for it is open. | text |
| P12C-06 (medium) | Columnar to columnar with options calls `columnar_set_options` (for the stripes to come; the step says so); the preview says it too. | `TestIntegrationSetStorageChangesTheOptionsOfAColumnarTable` |
| P12C-07 (medium) | The `ALTER TABLE` and the `ANALYZE` ask for their lock with `lock_timeout` (3 s) and retry (`withLockRetry`) with "Waiting for a lock held by another session" in the detail, for ten minutes at most, then fail with the retryable `operation_in_progress`. | `TestIntegrationConversionWaitsForALockInsteadOfBlockingTheTable` (gives up; goes through when the lock goes; a reader is not blocked) |
| P12C-08 (medium) | `runInfo` returns an error for a run that the instance does not have; the phase and finish steps fail with a clear message. | `TestIntegrationAMissingRunFailsTheStep` |
| P12C-09 (medium) | `Checks` returns the databases it read; `CheckAlerts` clears only alerts of databases that were read. | `TestAnAlertOfADatabaseThatWasNotReadIsLeftAlone` |
| P12C-10 (medium) | A scheduled run no longer needs the library; only a run by hand (and `putPartitionPolicy`) is refused when it is missing. | covered by the scheduler and manual-run tests; the scheduled path has no preload check |
| P12C-11 (medium) | Reads ask `Installed` only (404 / empty list / `extension_missing` for a plan with a policy in the request); `getPartitionPolicy`, `listPartitionPolicyRuns`, `getPartitionPolicyRun` and `listTablePartitions` are leader-bound. | `TestIntegrationReadsDoNotInstallTheExtension` |
| P12C-12 (low) | The contract text of every storage and policy operation names the limit; `notOnThisInstance` is a 404 whose detail names it; the console notes are in `p12cp/contract-changes.txt`. | `TestNotOnThisInstanceSaysTheLimit`, `TestIntegrationATableOfAnotherInstanceIsNotFoundWithTheReason` |
| P12C-13 (low) | `putPartitionPolicy` is refused (`operation_in_progress`) while a run is unfinished. | `TestIntegrationPutAndDeleteAreRefusedDuringARun` |
| P12C-14 (low) | `Engine.ActiveFor` asks the store by kind and subject (SQL in `PGStore`, a scan in `MemStore`; a window of 1000 for a store without it). | `TestActiveForSeesATaskBeyondTheNewestWindow`, `TestActiveForFallsBackToTheNewestTasks`, the 250-task case above |
| P12C-15 (low) | A conversion of a partition is refused while a `partition.maintain` task of its table is unfinished; 55P03 of the extension is `operation_in_progress`, retryable. | `TestIntegrationPutAndDeleteAreRefusedDuringARun` |
| P12C-16 (low) | Versions compare as numbers (`compareVersions`); the steps of `database.approximateAggregates` are interruptible; the contract text of `conversion_refused` says which reasons are checked at the request and which fail the task; the message for the library says "restart the instance"; the partition is addressed in the schema of its table, which the message says. The list of servers fixed at the start and the settings that stay on the servers that took them after a failed apply are documented (repeat the request). | `TestCompareVersionsAsNumbers` |

Mutants that survived in the review: M3/PM3 (`StartMaintain` without `maintainRunning`) is equivalent, because the engine's one task per subject returns `operation_in_progress` and `StartMaintain` turns that into false: kept as a cheap first test. M6
(`sortBy` on a table) is killed by `TestValidateStorageSortByBelongsToAPartition`. M8 (partial twice) is killed by `TestSecondPartial` (the rule is a function now). M12 (the tick's time not passed to `Start`) is killed by the fake backend that records it. PM5 (`failRun` disabled) is killed by
`TestIntegrationAFailedStepClosesTheRun`.

Console notes (`web-contract-notes.txt`): (1) typed `tableSchema`/`tableName` in `AlertSubject`; (2) `dryRun` and typed `blockers`; (3) no install button, the install is lazy on write; (4) sentence step names in the README, a disable runs the same five steps; (5) the shapes of `partitionEvery` are listed in the schema; (6) `listTablePartitions` text for plain tables. Every change is in `p12cp/contract-changes.txt`.
