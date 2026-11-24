<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026 RooyeKhat Media Co -->

# Independent review of the fixes for phase 13 (P13R-01 to P13R-15)

Date: 2026-11-22
Scope: branch p13-fix at d0785e4166 (commits 12ad23438f, 3607b1ed5a, 50439c120a, d0785e4166) on top of p13-fin. The first review and its
Fixes section were read first, then the diffs of pgconfig, instance, ops, httpapi and the console dialog.

## Method

- Go unit tests of pgconfig, instance and ops on Windows: pass. pgconfig and instance with PostgreSQL 18 in WSL (private install, lane l4):
  all pass (86 passing tests in the run).
- Probes on a plain PostgreSQL 18 instance (initdb, own port) for settings that the lists do not cover.
- Two throw-away tests (not kept): classification of candidate names; a restart rollback with two applies and no good file.
- Mutants (below). Everything marked "verified" was run; "by reading" is source reading only.
- Not re-run by me: the PostgreSQL 19 suite, the console tests, and the cluster tests other than the three named in the mutant table.

## Verdicts on the first review

| Finding | Verdict | Note |
|---|---|---|
| P13R-01 | fixed | `controlPlaneSettings` is enforced in `Classify` and in `Dropped`; mutant M6 (one entry removed) is killed. The list is not complete for the other things the task named, see P13S-02 to P13S-09. |
| P13R-02 | fixed, with gaps | Socket settings are managed, start-risk names and every `ssl*` are refused, `postgres -C` runs before a restart (M2 killed), a failed start puts a file back. The rollback has a hole (P13S-01), and `postgres -C` does not catch everything (verified: `max_connections = 3` with `superuser_reserved_connections = 3`, a `shared_buffers` of 4000GB and `unix_socket_permissions = 0000` all pass `-C`; the first one fails at start, so only the rollback saves it). |
| P13R-03 | fixed, with a gap | The step records `waiting` and the task ends done; the loop catches up (M9 killed by `TestConfigServerThatIsDownGetsTheChange`). The loop is blind to standby instances (P13S-05). |
| P13R-04 | fixed (by reading) | `hidden()` fails closed on an error and on an empty set; `alwaysHidden` is applied in `Classify`. Author's mutants not repeated. |
| P13R-05 | fixed (by reading) | `Dropped` is applied by the leader (`buildRequest`) and by the server; removal is accepted. Not repeated. |
| P13R-06 | fixed, partly | `BeginConfigRestart` brackets only the instance that holds the system database (other instances rely on the manager's gate); the primary of a pair comes from `group_replication_state`. A promoted-standby cluster test is still missing (the authors say so). |
| P13R-07 | fixed (by reading) | The zone is now chosen against the host zone. Not repeated. |
| P13R-08 | fixed (by reading) | Not repeated. |
| P13R-09 | fixed (by reading) | Not repeated. |
| P13R-10 | fixed (by reading) | Not repeated. |
| P13R-11 | fixed, with a race | `checkExpected` is killed by M5c. The precondition is not atomic with the record step (P13S-07). |
| P13R-12 | fixed (by reading) | The handler diff matches the described details. |
| P13R-13 | fixed (by reading) | The stopped-instance path writes, runs `CheckConfig`, restores on failure; tested with a fake `postgres`. |
| P13R-14 | fixed | `log_destination` and `syslog_*` are managed. See P13S-02 for a log setting that leaks statement text. |
| P13R-15 | fixed (by reading) | Coded task errors. |

## New findings

### P13S-01 (medium): the rollback has no good file in the common case, and falls back to the previous file, which can be the bad one

- Where: `internal/pgconfig/agent.go:924-945` (`restartAndWait`), `internal/instance/configfile.go:203-232` (`MarkConfigGood`,
  `RestoreGoodConfig`, `RestoreConfig`), `agent.go:625-653` (`clean`).
- Scenario (verified with a fake `pg_ctl`): the good file only exists after an apply that left nothing pending, or after a successful restart. On
  an instance that has had neither (a new instance, or one that went straight to a restart-required change), apply 1 writes a
  `shared_buffers` value the host cannot honour (the file is not "good": `clean` is false because the value is pending), then apply 2 changes
  `work_mem`. `WriteConfig` copied the apply-1 file to `.prev`. The restart fails, `RestoreGoodConfig` finds nothing, `RestoreConfig` puts back
  `.prev`, which still holds the bad value, the second start fails too, and the instance stays down ("could not be started again with the
  previous file"). The rollback gives no protection exactly when two changes are made before the first restart.
- Related gaps: (a) `clean` is false while any parameter is pending, so a reload-only change made while a restart is pending never reaches the
  good file, and a rollback later drops it (the loop rewrites it, so this is transient); (b) the rollback runs only inside `config.restart`:
  a host reboot, a crash, or `kingresd` stopping between the stop and the start of a restart leaves the instance down with the bad override
  file, and the ordinary start code of the operations package never restores it; (c) if the restore or the second start fails there is no
  retry and no state on the server page beyond the task error.
- Fix: write the good file when the instance is created or before the first change, never fall back to `.prev` (when there is no good file,
  write the empty header file), and make the ordinary instance start path try the good file once when a start fails on an override file. Add a
  unit test with two writes before the restart and a test where the second start fails too. Mutants M1 and M3 (below) show that the current
  tests would not notice.

### P13S-02 (medium): three settable log parameters write statement text to the log, which the managed list exists to prevent

- Where: `internal/instance/managed.go:60-72` (`log_statement`, `log_min_duration_statement`, `log_min_error_statement` and the parameter-length
  ones are managed; the sampling parameters are not), `internal/pgconfig/policy.go` (settable).
- Scenario (verified on PostgreSQL 18): `log_transaction_sample_rate = 1` made a `select 'sentinel_text'` appear in the log as
  `duration: ... statement: select 'sentinel_text'`. `log_min_duration_sample = 0` with `log_statement_sample_rate = 1` does the same. All three
  classify as settable at cluster, group, server and database level. kingresd runs statements with secrets in them (role and password
  statements), so this defeats the rule "statement text must never be written to a log".
- Fix: add `log_transaction_sample_rate`, `log_min_duration_sample` and `log_statement_sample_rate` to the logging-managed list and to the
  managed-list test.

### P13S-03 (medium): `archive_mode` is settable while `archive_command` is refused

- Where: `internal/pgconfig/policy.go` (no entry for `archive_mode`).
- Scenario (verified on PostgreSQL 18): `archive_mode = on` with no `archive_command` starts, logs a warning, and keeps every WAL segment as a
  `.ready` file (four segments after four `pg_switch_wal`). Nothing in the console can set the command, so the data disk fills until the
  instance stops. A restart is needed to switch it on, and the restart is offered by the console.
- Fix: refuse `archive_mode` (reason: archiving needs a command that cannot be set here), or accept only `off`.

### P13S-04 (medium): `max_worker_processes` has no floor, and a low value silently disables the background workers of the extensions

- Where: `internal/pgconfig/policy.go` `Floor` (only `max_prepared_transactions`, `max_wal_senders`, `max_replication_slots`); the extensions
  register workers in `contrib/kingres_ledger/src/worker.c` and `contrib/kingres_shard/src/recovery.c`.
- Scenario (verified on PostgreSQL 18, private install with both extensions preloaded): `max_worker_processes = 1` starts normally, and the log
  says "too many background workers" for each registration; the ledger and shard workers never run and nothing in the console says so. The
  restart check and the rollback do not catch it because the start succeeds.
- Fix: add a floor for `max_worker_processes` (the number of workers the two extensions register plus what PostgreSQL itself needs), and
  repeat it in the restart warning.

### P13S-05 (medium, by reading): the convergence loop compares only the file of instance 1, so a standby instance is never brought up to date

- Where: `internal/pgconfig/agent.go:376-379` (`State`: `ConfigHash` and `Revision` come from `ReadConfig(cluster.NodeID)` only),
  `internal/pgconfig/converge.go:65`, `internal/instance/standby.go:125` (a standby is a `pg_basebackup` copy, which carries the override file of
  the source server).
- Scenario: `Apply` reports `waiting` for "standby instance N is not ready yet" or "is starting", and the fix for P13R-03 leans on the loop to
  finish the job, but the loop sees instance 1 up to date and starts no task. A standby created later by `pg_basebackup` holds the override
  file of the primary's server (including that server's server-level values) until the next change that touches its own server. Server-level
  overrides therefore apply to the wrong standbys, and the five standby settings (`max_connections` and the like) can differ from what the
  restart order assumed.
- Fix: hash the override files of every managed instance of the server (or report a hash per instance and compare all), and write the
  rendered file for the server when a standby instance is created.

### P13S-06 (low, by reading): `client_encoding` is settable at cluster and database level

- Where: `internal/pgconfig/policy.go` (no entry). The connections of kingresd do not send `client_encoding` (pgx sends only user, database and
  `application_name`; see `internal/auth/postgres.go` and `internal/cluster/cluster.go`), so they take the cluster or database default. A
  non-UTF8 default turns non-ASCII names and text into errors or wrong bytes on every service connection.
- Fix: refuse `client_encoding` at every level, or make the service connections send `client_encoding=UTF8`. The same reasoning applies to
  `DateStyle`, `IntervalStyle`, `extra_float_digits` and `bytea_output` (cluster-only, settable). I found no break there: the Go code reads times
  and bytes in binary and quotes literals with `E''` (`sqlx.QuoteLiteral`), so `standard_conforming_strings` is harmless to it. There is no
  test for either.

### P13S-07 (low, by reading): `expectedRevision` is not enforced where the value is recorded, and the config tasks have no exclusion in the engine

- Where: `internal/pgconfig/apply.go:50-52` (`checkExpected` runs before `busy`), `apply.go:151-205` (`stepRecord` does not look at the
  revision), `apply.go:29-34` (`busy` is a check followed by a separate insert), `internal/task/memstore.go:293-314` (no rule for `config.apply`
  or `config.restart`), `ConfigEditDialog.vue` (`seenRevision` stays null until the summary is fetched after the dialog opened, and the request
  then carries no revision).
- Scenario: two requests with the same `expectedRevision` that pass the check and `busy` within the same few milliseconds both start a task;
  the second overwrites the first without a 409 (the history keeps both). The converge loop and a user request can do the same. Effects are
  small because `buildRequest` reads the stored values fresh at every server step and `Agent.Apply` is serialised by a mutex per server, so
  servers do converge on the last recorded values. The dialog can send no revision if the summary is slow, which fails open.
- Fix: check `expectedRevision` inside the `stepRecord` transaction (after `FOR UPDATE`) and make the engine refuse a second unfinished task of
  kind `config.apply` or `config.restart` (as it does for failovers); have the dialog wait for the revision before enabling Apply.

### P13S-08 (low): test gaps found by the mutants

- M1 (always restore `.prev`, never the good file), M3 (do not mark the file good after a successful restart) and M4 (`clean` ignores pending
  restart and errors) all survive the whole pgconfig suite: the rollback test builds a `.prev` that equals the `.good` file, and nothing checks
  that the good file is marked at all after a restart or an apply. P13S-01 shows that these paths matter.
- There is no test for a rollback whose second start fails, or for two writes before the restart.
- Fix: add them (the reproduction for P13S-01 is a fake instance with two writes before a restart).

### P13S-09 (low, by reading): other settable parameters that can hurt a cluster

`max_slot_wal_keep_size` (a small value invalidates the slot of a lagging standby), `recovery_min_apply_delay` (delays a standby and a
failover), `restart_after_crash = off` (a crash leaves the instance down), `log_file_mode` (a mode without owner read stops the console from
reading the files). All classify as settable. A preview warning, as for `max_connections`, would be enough.

## Checked and found fine

- `default_tablespace`, `temp_tablespaces`, `default_table_access_method`, `check_function_bodies` and the timeout, isolation, read-only and
  search path family are refused at every level; `Dropped` skips stored rows with those names.
- `wal_level`, `hot_standby`, `primary_slot_name`, `primary_conninfo`, `shared_preload_libraries`, `password_encryption`, `synchronous_*`, the
  `ssl*` family, `*_command`, `*_library`, `*_libraries` and every name with `preload` are managed or refused. `max_wal_senders` and
  `max_replication_slots` have floors; `max_connections` and `superuser_reserved_connections` have the preview warning, and an impossible pair
  is stopped by the restart rollback (verified that `-C` alone passes it and the start fails).
- Rollback race: restart and apply both take the agent mutex, so a rollback cannot interleave with a write on the same server. A rollback that
  restores the good file puts back a file that the loop rewrites to the desired state later; no value is lost.
- Waiting server: `stepApply` builds the request from the stored values at the time of its step, the loop does the same, and a stale revision
  cannot be sent. A server that is unreachable has no state row and is skipped until it answers; when it answers, its file hash differs and a
  task is started.
- The `expectedRevision` check, in the single-request case, is correct for removals, for a value that already holds the wanted value, and for
  a revert chain.

## Mutants

All run in lane l4 on PostgreSQL 18, over the whole `pgconfig` package unless noted; all restored.

| id | change | result |
|---|---|---|
| M1 | restart rollback never uses the good file (always `.prev`) | survived (P13S-08) |
| M2 | no `CheckConfig` before a restart | killed by `TestRestartIsRefusedWhenPostgreSQLRefusesTheFile` |
| M3 | no `MarkConfigGood` after a successful restart | survived (P13S-08) |
| M4 | `clean` ignores `PendingRestart` and `Error` | survived (P13S-08) |
| M5 | `last <= expected` to `last < expected` in `checkExpected` | survived at unit level; killed by `TestConfigExpectedRevision` (cluster, M5c) |
| M6 | remove `idle_session_timeout` from the control-plane list | killed by `TestControlPlaneSessionSettingsAreRefused` |
| M7 / M7b | remove `event_source` from the start-risk list (M7 did not compile; M7b is the corrected edit) | M7b killed by `TestSettingsThatStopTheServerAreNotSettable` |
| M8 | the `ssl` prefix rule in `Classify` off | killed by `TestSettingsThatStopTheServerAreNotSettable` |
| M9 | the convergence loop never starts a task (`HashOf(text) == st.ConfigHash` made always true) | killed by `TestConfigServerThatIsDownGetsTheChange` (cluster, about 4 minutes) |

Probes (not kept): classification of 12 candidate names (all settable: `archive_mode`, the three sampling names, `client_encoding`,
`max_worker_processes`, `log_file_mode`, `max_slot_wal_keep_size`, `recovery_min_apply_delay`, `restart_after_crash`, `datestyle`,
`standard_conforming_strings`); a fake-instance restart with two writes and no good file (the instance is left down); PostgreSQL 18 start with
the sampling log settings, `archive_mode = on`, `max_connections = 3`, `shared_buffers = 4000GB`, `unix_socket_permissions = 0`, and
`max_worker_processes = 1` with both extensions preloaded.

## Fixes

Branch `p13-fix`, the commit after d0785e4166 (code, tests and console; `git log` on the branch). Every fix has a test that fails without it; the mutant that was run is named.

| Finding | Fix | Test | Mutant (killed) |
|---|---|---|---|
| P13S-01 | The last good override file is kept by **every successful start** (`Manager.start` marks it), also at the first start of a new instance; kingresd adopts the file of a running instance that has none at its start. A start that fails with the override file of the console is tried once more with the last good file, **in the manager, for every start** (the task, a reboot, a crash, an operator), and leaves a mark that the task reads. Never `.prev`: with no good file the empty file is written. The `clean` marking in `Apply` is removed. | `TestTwoWritesBeforeARestartRollBackToTheLastGoodFile`, `TestRollbackWithoutAGoodFileWritesTheEmptyFile`, `TestRollbackWhoseSecondStartFailsToo`, `TestAnOrdinaryStartGoesBackToTheLastGoodFile`, `TestASuccessfulStartKeepsTheFileAsGood`, cluster `TestConfigRestartThatCannotStartRollsBack` (now with a second change before the restart) | M1 (use `.prev`): 3 tests; M3 (no mark after a start): 1; no empty-file fallback: 1; no fallback in `start`: 3 |
| P13S-02 | `log_transaction_sample_rate`, `log_statement_sample_rate`, `log_min_duration_sample` managed (statement text) | `TestMoreParametersAreNotSettable`, cluster `TestConfigSettingsThatStopTheServerAreNotSettable` | entry removed |
| P13S-03 | `archive_mode` refused | same | entry renamed |
| P13S-04 | floor of 8 for `max_worker_processes` (two launchers, the block worker of each ledger database, the recovery and learn workers, the logical replication launcher) | `TestMaxWorkerProcessesHasAFloor` | floor 1 |
| P13S-05 | `ConfigHash` and `Revision` of a server cover the override file of every instance that has a data directory (`configIdentity`); a standby that holds another file makes the server differ and the loop applies; `InitStandby` replaces the file of the copy with the one of this server (or the empty file) and removes the copied `.good`, `.prev` and `.rolledback` | `TestConfigIdentityCoversEveryInstance`, `TestAStandbyCopyGetsTheOverrideFileOfThisServer` | identity only of instance 1; the standby keeps `override := "x"` |
| P13S-06 | `client_encoding` refused (the other session formats were found harmless by the review) | `TestMoreParametersAreNotSettable`, cluster | entry removed |
| P13S-07 | `expectedRevision` is checked again inside the record step (an advisory lock makes the record steps one at a time); the check "no configuration task is unfinished" and the start are one step under a mutex of the leader (apply, restart and the loop); the dialog keeps Apply disabled until the revision it saw is known (and goes without it when the summary cannot be read) | `TestSupersedes`, dialog tests "does not enable Apply before the revision is known" and "goes without the revision when the summary cannot be read" | `last <= expected` changed; `return !same` made false; the dialog check removed |
| P13S-08 | the tests above | | M1, M3 and the `clean` path no longer exist or are killed |
| P13S-09 | `restart_after_crash`, `recovery_min_apply_delay`, `max_slot_wal_keep_size` refused; `log_file_mode` managed | `TestMoreParametersAreNotSettable`, cluster | entries removed |

### What is left, and why

- P13S-01 (c): the server page shows no state for an instance that went back to the last good file at a reboot; the mark stays in the data directory until the next configuration restart and the log says it. The task error carries it when the task caused it.
- P13S-04: only `max_worker_processes` got a floor. For `max_parallel_workers` and `max_locks_per_transaction` nothing in the extensions needs a value above the PostgreSQL default; no floor was invented.
- P13S-07: the exclusion between `config.apply` and `config.restart` is a mutex of the leader (the engine's database rule is per subject and the two have different subjects); it holds for one leader, and a leader change starts a new process. A rule of the engine would need a migration of the trigger.
- P13S-05: the effect on an instance that is not ready yet is that its server shows `waiting` until the file is written; the loop retries with its back-off.
- P13S-06: `DateStyle`, `IntervalStyle`, `extra_float_digits`, `bytea_output` stay settable (cluster-only), as the review found no break.
