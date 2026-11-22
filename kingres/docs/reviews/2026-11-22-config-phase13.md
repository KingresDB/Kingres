<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026 RooyeKhat Media Co -->

# Independent review of phase 13, configuration from the console

Date: 2026-11-22
Scope: branch `p13-fin` at f99beaf8c3 (closing commits 29ffe13f05 and f99beaf8c3 since 94b8c3d562), with the phase-13 code that was already on
`kingres-18`: `internal/pgconfig`, `internal/instance/managed.go` and `configfile.go`, `internal/httpapi/handlers_config.go`, the contract
`kingres/api/openapi.yaml` 0.17.0, the console (`kingres/web/src/components/config`, `lib/config*.ts`, `mocks/config*.ts`) and the cluster tests
`internal/clustertest/config_test.go` and `config_closing_test.go`. Design read end to end: `kingres/docs/design/config-phase13.md`.

## Method

1. Read the design, then the code of the preview and apply flow (`change.go`, `apply.go`, `leader.go`, `agent.go`, `policy.go`, `value.go`,
   `restart.go`, `converge.go`, `time.go`), the override-file renderer and the handlers.
2. Ran the Go unit tests of `pgconfig` and `instance` (pass), the console unit tests matching "config" (6 files, 304 tests, pass), and the whole
   `TestConfig*` set of the cluster tests on three servers in lane l4 with PostgreSQL 18 (16 tests, all pass, about 5 minutes).
3. Wrote throw-away probes on the real three-server cluster (three tests, not kept): which parameters the classifier calls settable, what happens
   when a server in the middle is down, what happens after `default_transaction_read_only = on` at cluster level.
4. Ran mutants (section "Mutants") against the unit and cluster tests.

Everything below marked "verified" was reproduced on a running cluster or with a test run; "by reading" means source reading only.

## Findings

### P13R-01 (high): a valid, unwarned change of `default_transaction_read_only` puts the whole control plane into read-only mode, with no way back from the console

- Where: `internal/pgconfig/policy.go:57-79` (the parameter is in the cluster-only table, which makes it settable at cluster and database level) and
  `policy.go:169-178` (everything not refused is settable); `internal/pgconfig/change.go` (the preview raises no warning for it).
- Scenario (verified): an administrator sets `default_transaction_read_only = on` at cluster level. The preview answers `valid: true` with no
  warning. The apply task starts, the first server reloads, and from then on every `kingresd` connection (also to `kingres_system`) opens read-only
  transactions: the log fills with `cannot execute UPDATE in a read-only transaction` from the task engine, the alert updater, the clock alert and the
  standby code. The apply task stays at "Apply on server" (33 percent) and never finishes, so `busy()` keeps answering 409 for every later
  configuration change, including the revert. The same holds for `ALTER DATABASE postgres SET default_transaction_read_only` at database level
  (the service connections use that database; by reading, not run). The only repair is to edit the override file and the table by hand on every server.
- Fix: put the parameters that `kingresd` itself depends on in the managed or refused list (`default_transaction_read_only`, and review
  `default_transaction_isolation`, `idle_session_timeout`, `idle_in_transaction_session_timeout`, `statement_timeout`, `lock_timeout`, `search_path`,
  `session_replication_role`, `row_security` for the same reason), or make every `kingresd` service connection set what it needs explicitly
  (`default_transaction_read_only=off`) and add a preview warning for the rest. Add a cluster test that sets it and expects a 422.

### P13R-02 (high): settings that stop PostgreSQL from starting or `kingresd` from connecting are settable, and the preview does not say so

- Where: `internal/instance/managed.go:41-72` (the managed list), `internal/pgconfig/policy.go:134-180`.
- Scenario (verified for classification, by reading for the effect): `unix_socket_permissions` and `unix_socket_group` (both context `postmaster`, not
  superuser-only) answer `kind: settable` at cluster, server group and server level. `unix_socket_permissions = 0` is accepted by the checker
  and by PostgreSQL's own parser; after the restart that the console then offers, the socket cannot be opened by `kingresd` (the design
  protects `listen_addresses`, `port` and `unix_socket_directories` for exactly this reason), the leader can no longer read `pg_settings`,
  every configuration operation answers 503 and the change cannot be reverted through the API. `unix_socket_group = nosuchgroup` is valid
  syntax and makes the postmaster refuse to start. Others of the same kind: `huge_pages = on` on a host without huge pages, `event_source`,
  `bonjour`, a `ssl_crl_dir` that does not exist, a `shared_buffers` above the host memory. Nothing in the file check (V2) catches these because
  they fail at start, not at reload; `postgres -C` does not start the server.
- Fix: add `unix_socket_permissions` and `unix_socket_group` to the managed list (or refuse them); for the remaining postmaster-context
  parameters run `postgres -C` over the whole candidate file before the restart task starts and refuse the restart when it fails; make
  `config.restart` stop and report when an instance does not come back (it does stop at the first failure, but the instance is then down).
  Extend the existing `max_connections` warning into a general "this value stops the server from starting" check.

### P13R-03 (medium): one unreachable server in the middle blocks the change for the servers after it and blocks every other configuration task

- Where: `internal/pgconfig/apply.go:113` (one sequential step per server), `apply.go:215-245` (`stepApply` loops until the server answers),
  `apply.go:29` (`busy`).
- Scenario (verified): three servers, the second one stopped. A change of `work_mem` reaches the leader (first) and then the task waits at the second.
  After 60 seconds the third server still has the old value, and a second change request answers 409 `operation_in_progress`. The design says the
  task "continues with the other servers" on failure and rejects stopping because it leaves an arbitrary subset on the new value; the wait case
  gives the same subset (leader new, third old) and, in addition, nobody can choose which. The existing test stops the last server only
  (`TestConfigServerThatIsDownGetsTheChange`, `Nodes[2]`), so it never meets this. The same task also blocks the 30-second convergence loop
  (`converge.go`, `busy`) and any `config.restart`.
- Fix: run the per-server steps so that a server that cannot be reached is marked `waiting` and the loop goes on to the next one, then returns to
  the waiting ones (or run the steps in parallel); keep the task open (or finish it as `waiting`) but let a revert and the convergence loop run.
  Add a test that stops `Nodes[1]`.

### P13R-04 (medium): the "hidden" and "superuser-only" classification fails open

- Where: `internal/pgconfig/agent.go:340-345` (`State`: `h, herr := hidden(...)`, an error only logs and returns an empty `Hidden`), `agent.go:472`
  (`h, _ := hidden(...)` in `Apply`), `internal/pgconfig/policy.go:157-164` and `leader.go snapshot` (no check that the set is not empty).
- Scenario (by reading): `hidden()` runs `SET LOCAL ROLE pg_database_owner` in a transaction; a transient error (connection reset, statement
  timeout, role renamed) gives an empty set. From then on `Classify` sees no superuser-only parameter: names that are not in the name tables
  (`ssl_crl_dir`, `ssl_*` file settings, `log_*` file settings, extension settings that are superuser-only, a parameter a later PostgreSQL makes
  superuser-only) become settable, and the managed parameters `primary_conninfo` and `kingres_shard.node_conninfo` (which can carry a password) have
  `hidden: false`, so `listConfigParameters` returns their values to a `kingres_readonly` account. The per-server re-validation in `Apply`
  uses the same function, so the defence in depth has the same hole. The result is cached per call, so the window is one request.
- Fix: treat an error as "unknown" and fail the request (503) instead of continuing with an empty set; refuse when the set is empty on a version
  that is known to hide parameters; return `hidden: true` for every managed parameter regardless.

### P13R-05 (medium): a stored override that later becomes managed or refused blocks every configuration change on every server and cannot be removed

- Where: `internal/instance/configfile.go:44-46` (`RenderConfig` refuses a managed name), `internal/pgconfig/change.go:85-88` (`CheckLevel` runs
  before the value check, so removing such a row is refused as `config_parameter_managed`), `agent.go` `Apply` (the error aborts the whole apply).
- Scenario (verified with a mutant that let a refused name into the table): once `kingres_cp.config_value` holds a row whose name `RenderConfig` refuses, every apply and every
  convergence run fails on every server with `(server): config_failed: ... is managed and cannot be written here`, the state shows `failed`, and
  the API refuses to remove the row. This happens in practice at an upgrade that adds a name to `ManagedParameters` (the design itself reserves
  `wal_level` "until rebalancing is implemented") for an operator who had set it before. The only repair is SQL on `kingres_system`.
- Fix: let a removal (value `null`) through for managed and refused names, and have the renderer skip, with a warning, a stored name that has become
  managed instead of failing the whole file. Add a migration step when a name is added to the managed list that deletes or reports existing rows.

### P13R-06 (medium, by reading): the restart bypasses the instance lifecycle of the operations code and assumes that instance 1 is the primary

- Where: `internal/pgconfig/agent.go:789-799` (`RestartInstance` calls `mgr().Restart` directly), `internal/pgconfig/change.go` `restartOrder`
  (`add(id, 1, "primary", 0)`, `idx[node{p.PrimaryServer, 1}]`).
- Scenario: `ops.RestartInstance` goes through `beginOp`, the lifecycle state (`restarting`), the primaries wrapper with its lease handling
  (FO-16) and `waitSystemDatabase`; the phase-13 restart does none of that, so the server page shows "running" while the instance is down
  and a failover running at the same time is not excluded. After a failover the primary of a group is the promoted standby instance, not
  instance 1 of `primary_server`, so the pair ordering of `restartOrder` (and the five-settings rule) can put the new primary after its
  standby. The design said the task uses "the existing instance operations" and "the existing refusal" for the instance that holds the
  system database.
- Fix: restart through the operations code (or take `beginOp` and the lease); derive the primary of a pair from the failover state, not from
  instance 1; add a cluster test with a promoted standby.

### P13R-07 (medium): the time-zone tests, including the new standby test, cannot fail on a machine whose zone is Asia/Tehran

- Where: `internal/clustertest/config_test.go:471-510` and `config_closing_test.go:152,170` use `Asia/Tehran` as the "changed" value.
- Scenario (verified): the machine that runs the tests is in `Asia/Tehran`, so `initdb` gives every instance and every database that zone. With the
  whole body of the database-level `SET` in `applyDatabases` disabled, `TestConfigDatabaseTimeZoneReachesTheStandbys` and
  `TestConfigClusterAndDatabaseTimeZone` both still pass. The same holds for the removal (`RESET`) path. The test added by the closing work to prove
  replay to standbys therefore proves nothing on the author's machine.
- Fix: use a zone that is not the host's (compute one from `SHOW TimeZone` before the change, or use a fixed zone and skip when it equals the
  default); assert that the value before the change differs from the wanted one. A run with `Pacific/Auckland` is in the mutants section.

### P13R-08 (low): the executed order of `config.restart` is not tested

- Where: `internal/clustertest/config_test.go:619-770`.
- Scenario (verified with a mutant): iterating the computed restart order backwards in `StartRestart` (`restart.go:66`) leaves
  `TestConfigPendingRestartAndRestartTask` and `TestConfigRestartOrderOfStandbys` green, because the second test only reads the `restartOrder` of the
  preview and the first does not look at the order of the task steps.
- Fix: assert the order of the step names of the `config.restart` task against the preview.

### P13R-09 (low): the leading-zero case of the checker test is shadowed by the range check

- Where: `internal/pgconfig/value_test.go:42` (`{workMem, "010", false, ...}`).
- Scenario (verified with a mutant): the regular expression `numberRE` can be changed to accept leading zeros and `TestCheck` still passes, because `010`
  is below the minimum of `work_mem` and is refused for that reason. PostgreSQL reads `0100` as octal 64, so a regression here makes the preview
  show 100 while the server applies 64. The differential test against PostgreSQL covers it, but it only runs with a live instance.
- Fix: add `{maxConn, "0100", false}` and `{maxConn, "00", false}` to the unit table.

### P13R-10 (low): the reserved-connections warning has gaps

- Where: `internal/pgconfig/warnings.go:77-90` and `:100-117`.
- Scenario (by reading): (a) removing an override of `max_connections` makes `effectiveInt` fall back to the running value (`meta.Setting`), not to the
  value the lower level will give after the reload, so the warning can be wrong in both directions; (b) only the first offending server is named;
  (c) the message is a warning only, and the restart task does not repeat it (see P13R-02).
- Fix: compute the post-removal value from the lower levels and the derived defaults; list all offending servers.

### P13R-11 (low): no concurrency precondition, and Retry in the dialog resubmits a stale request

- Where: `kingres/web/src/components/config/ConfigEditDialog.vue:214-236` (`sent` is reused by `retry`), `apply.go stepRecord`.
- Scenario: two administrators open the same parameter; the second applies later and silently overwrites the first (the history keeps both, the
  second never sees the first). Retry sends the first request again, so after another administrator changed the value in between, Retry overwrites it.
- Fix: carry the revision of the preview (`revision` of `listConfigParameters`) in `applyConfigChange` and answer 409 when a touched name changed since;
  make Retry use the task retry (same task) or the stored revision, not a new change.

### P13R-12 (low): audit details are incomplete or not normalized

- Where: `internal/httpapi/handlers_config.go:125,147`, `internal/pgconfig/apply.go:299-322` (`ChangeDetails`).
- Scenario: the design says `applyConfigChange` details are `{revision, changes, servers}` and that `revertConfigChange` is audited with old and new
  value. The code records `{changes, taskId}` for the apply (no revision, no servers) and `{reverts, taskId}` for the revert (no values).
  `newValue` is the text the caller sent (`TRUE`, ` 8MB `), not the normalized text that was written. A refused or invalid request
  records no change details, so an attempt to set `archive_command` leaves no trace of the value.
- Fix: write the audit details from the recorded `config_change` rows in the task (after the record step), and add the failed attempt with the name and level
  (never the value of a refused parameter).

### P13R-13 (low): an instance that is not running gets its file without any check, and database values are re-checked with `hidden = false`

- Where: `internal/pgconfig/agent.go:440-443` (the file is written, `waiting` is reported, no `postgres -C` as the design's step 4 says) and
  `agent.go:686` (`Classify(s, false)`).
- Scenario: a value that the subset checker accepts but PostgreSQL refuses (an extension setting, see "Known limits" of the design) is written to a
  stopped instance and found only when it starts; the instance then does not start. For database values the server-side defence ignores the
  hidden set, so a compromised leader could set a superuser-only parameter of context `superuser` that is not in the name tables.
- Fix: run `postgres -C` per parameter for a stopped instance, and pass the real hidden set to the database branch.

### P13R-14 (low): `log_destination`, `syslog_*`, `log_line_prefix` and similar are settable and can blind the console log reader

- Where: `internal/instance/managed.go:41-72` (only the collector and file names are managed).
- Scenario: `log_destination = 'syslog'` at cluster level makes the collector write nothing; the console log page and the logs that the monitor reads
  go quiet on every server, and that is also the place where an operator would look for the cause. No data is lost, but diagnosis is.
- Fix: manage `log_destination`, `syslog_facility`, `syslog_ident`, `log_line_prefix` and `log_error_verbosity` or warn in the preview.

### P13R-15 (low): the task error code of a failed validation is `step_failed`, not `validation_failed`

- Where: `internal/pgconfig/apply.go:142`.
- Scenario: the design table says the failed `Validate the change` step gives error code `validation_failed`; the engine wraps the plain error as
  `step_failed` with the text `validation_failed: ...`. Clients that branch on the code (the console shows the text only) cannot tell it from a crash.
- Fix: return a `task.Coded` error with code `validation_failed` and `Retryable: false`.

## Checked and found correct

- A change that the preview did not show cannot be applied through the API: `previewConfigChange` and `applyConfigChange` call the same `check`;
  the task validates again in its first step against the current state; values are stored normalized; the apply body is the checked list, not
  the request. The refused, managed and cluster-only rules are enforced in `check` before any value is looked at, and again on each server
  (`Apply` classifies every name with that server's own `pg_settings`): a mutant that disabled the leader's `CheckLevel` was stopped by the server
  (every server answered `config_failed ... is managed`), so a compromised leader cannot make a server write `archive_command`.
- File content: names are matched by `^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)?$`, values are quoted by `confValue` with NUL, CR, LF and invalid UTF-8
  refused, the file is generated whole and replaced atomically (mode 0600) after a `.prev` copy; no path of a request reaches the file system. There
  is no way to name another file or an `include` through the API; `include*` names are not parameters. `shared_preload_libraries`,
  `*_command`, `*_library`, `*_libraries`, `*preload*`, the superuser-only set (when `hidden()` works, see P13R-04) and the developer options
  are refused; the `postgres -C` check runs without a shell with `name=value` as one argument.
- Database values reach SQL only through `format(%I, %L)` in PostgreSQL itself; `kingres_system` and template databases are excluded from the
  scope list.
- Failure on one server: the server restores its previous file (`RestoreConfig`, killed by mutant C1 in the closing test), the task continues with the
  other servers, and ends `failed` with `config_incomplete`; the desired state is recorded first, a retry is idempotent (a repeated change adds no
  history row). A server that fails does not keep a half-written file.
- Restart-required parameters are never restarted by an apply: they show `pendingRestart` through `pending_restart`; the restart needs
  `acknowledgeDowntime`, stops at the first failure and runs only when no apply is unfinished (the `busy` check; tiny race between `busy()` and the
  task start for apply versus restart, which have different subjects, is not closed but harmless in practice).
- Revert is a new change, refused with 409 when a later change touched the same level, scope and name (killed by mutant C2).
- Console: no `v-html` or `innerHTML` anywhere in `src`; every server text goes through `SafeName`/`safeText`; the preview is debounced, results of an
  older request are dropped (`previewSeq`), and Apply needs a valid preview of the same request; the types match the contract (`ConfigWarning`
  generated into `schema.d.ts`; 304 unit tests pass; the mock and the dialog show both warnings).
- The new warnings: edges of `config_value_unusual` (exactly ten times, exactly a tenth, non-memory units, zero default) are tested at unit level and
  in the cluster test and all six unit mutants on them were killed.
- Contract 0.17.0 is additive: `warnings` is optional, `valid` still depends on `problems` only.

## Mutants

Unit level (`go test ./internal/pgconfig ./internal/instance`):

| id | change | result |
|---|---|---|
| U1 | `>` to `>=` at ten times the default (`warnings.go`) | killed |
| U2 | reserved-connections floor 1 to 0 | killed |
| U3 | `<` to `<=` at a tenth of the default | killed |
| U4 | server-group precedence rank 1 to 3 (`Pick`) | killed |
| U5 | drop the `preload` name rule (`codeNameReason`) | survived (redundant with the `_libraries` suffix and the name table) |
| U6 | `numberRE` accepts leading zeros | survived (P13R-09) |
| U7 | delete `fsync` from the policy refusals | killed |
| U8 | `max_wal_senders` floor 4+r to 2+r | killed |
| U9 | reserved check `max < su+rs+1` to `max < su+rs` | killed |

Cluster level (three servers, PostgreSQL 18, lane l4):

| id | change | result |
|---|---|---|
| C1 | do not restore the override file after a rejected file (`agent.go`) | killed by `TestConfigValueRejectedByPostgreSQLInTheFileRestoresTheFile` |
| C2 | drop the superseded check of a revert (`apply.go:75`) | killed by `TestConfigHistoryAndRevert` |
| C3 | execute the restart order backwards (`restart.go:66`) | survived (P13R-08) |
| C4 | do not set the audit details of `applyConfigChange` | killed by `TestConfigClusterLevelAuditAndHistory` |
| C5 | skip `CheckLevel` at the leader | killed (and the servers refused the managed and refused names, see "Checked") |
| C6 | skip the `RESET` of removed database values | survived on the default zone (P13R-07) |
| C7 | `unusualFactor` 10 to 20 | killed by `TestConfigPreviewWarnings` |
| C8 | skip the database `SET` altogether | survived: `TestConfigDatabaseTimeZoneReachesTheStandbys` and `TestConfigClusterAndDatabaseTimeZone` pass (P13R-07) |
| C9 | C6 with the zone tests changed to `Pacific/Auckland` | see the note below |

C9 result: killed. With the zone tests using `Pacific/Auckland`, `TestConfigClusterAndDatabaseTimeZone` fails at the removal check ("database cfgtz TimeZone Pacific/Auckland after the removal, want Asia/Tehran"), so the tests are effective once the value differs from the host zone, and C6 and C8 are real gaps of the original tests, not equivalent mutants.

All mutants were reverted; the review worktrees were removed.

## Fixes

Branch `p13-fix` (from `p13-fin`). Commits: **A** 12ad23438f (instance manager and operations code), **B** 3607b1ed5a (control plane code and tests), **C** 50439c120a
(contract 0.17.0 and console). Every fix has a test that fails without it; the mutant that was run against it is named.

| Finding | Fix | Commit | Test | Mutant (killed) |
|---|---|---|---|---|
| P13R-01 | the settings of the own sessions of `kingresd` (`default_transaction_read_only`, `transaction_read_only`, `default_transaction_isolation`, `session_replication_role`, the timeouts, `search_path`, `row_security`, ...) are refused at every level (`pgconfig/policy.go`) | B | `TestConfigControlPlaneSessionSettingsAreRefused` (cluster, the reviewer's scenario at cluster and database level, then a write on every server and a next change), `TestControlPlaneSessionSettingsAreRefused` | the refusal branch off; `default_transaction_read_only` out of the table |
| P13R-02 | `unix_socket_permissions`, `unix_socket_group` managed (A); `huge_pages`, `shared_memory_type`, `bonjour`, `event_source` and every `ssl*` refused (B); restart: `postgres -C` over the whole file first, and a start that fails puts `kingres-config.conf.good` back and starts again (A, B) | A, B | `TestConfigSettingsThatStopTheServerAreNotSettable`, `TestConfigRestartThatCannotStartRollsBack` (a 4000GB `shared_buffers` on a real server), `TestRestartThatCannotStartPutsTheLastGoodFileBack`, `TestRestartIsRefusedWhenPostgreSQLRefusesTheFile`, `TestSettingsThatStopTheServerAreNotSettable` | no rollback; no pre-check; `ssl` prefix off; `unix_socket_permissions` out of the managed list |
| P13R-03 | `stepApply` tries once, records `waiting` and goes on; the convergence loop brings the server up to date | B | `TestConfigServerInTheMiddleThatIsDownDoesNotBlock` (stops `Nodes[1]`), `TestConfigServerThatIsDownGetsTheChange` (now: the task ends, the loop catches up) | the step returns an error after `waiting` |
| P13R-04 | an error or an empty hidden set fails the request (503) in `State` and `Apply`; `primary_conninfo` and `kingres_shard.node_conninfo` are always hidden | B | `TestHiddenFailsClosed` (real PostgreSQL), `TestConnectionStringsAreAlwaysHidden` | the empty-set check off; `alwaysHidden` off |
| P13R-05 | removal of a stored value is always accepted (`check`); a stored value of a name that is managed or refused is left out by the leader and by the server, and does not fail the apply | B | `TestConfigStoredValueOfAManagedNameCanBeRemoved` (cluster: a stored `wal_level` row), `TestDroppedNames`, `TestStoredRowsAreFound` | the removal shortcut off; `Dropped` always false |
| P13R-06 | `Ops.BeginConfigRestart` brackets the restart of the instance that holds the system database (A); the pair order uses the primary the leader last saw (`group_replication_state`), not instance 1 (B); the failover exclusion of `config.restart` is the existing task trigger | A, B | `TestConfigRestartUsesTheInstanceLifecycle`, `TestConfigRestartOfAStoppedOrAnotherInstance`, `TestRestartOrderAfterFailover` | no `beginOp`; `primaryNode` ignores the record |
| P13R-07 | the time-zone tests use `Pacific/Auckland` (or `Asia/Tehran` when that is the host's zone) | B | `TestConfigClusterAndDatabaseTimeZone`, `TestConfigDatabaseTimeZoneReachesTheStandbys` | database `SET` skipped; `RESET` skipped (both killed, as the review's C6 and C8) |
| P13R-08 | the names of the steps of the `config.restart` task are compared with the order of the preview | B | `TestConfigPendingRestartAndRestartTask` | the order of `StartRestart` reversed |
| P13R-09 | `0100`, `00`, `007` in the unit table | B | `TestCheck` | `numberRE` accepts leading zeros |
| P13R-10 | every offending server is named; a removed override that the running value comes from makes no claim | B | `TestReservedConnectionsWarningEdges` | the source-file rule off; the list of other servers off |
| P13R-11 | `expectedRevision` (contract 0.17.0): 409 `config_change_superseded` when a later revision changed a value of the request and it does not hold the wanted value already; the dialog sends the revision it saw, a Retry resends the same request | B, C | `TestConfigExpectedRevision`, the dialog test "sends the revision it saw" | `checkExpected` off; the dialog does not send it |
| P13R-12 | audit details `{changes (old and normalized new), servers, basedOnRevision, taskId}`; a refused request is audited with names and levels only | B | `TestConfigAuditDetails`, `TestAttemptedHasNoValues`, `TestDescribeChanges` | `servers` removed; the attempt not audited |
| P13R-13 | a stopped instance gets `postgres -C` over the file and the file is restored when refused; database values are classified with the hidden set and an unknown name is refused | B | `TestApplyToAStoppedInstanceChecksTheFile` | the check off |
| P13R-14 | `log_destination` and `syslog_*` managed | A | `TestSettingsThatStopTheServerAreNotSettable`, `TestConfigSettingsThatStopTheServerAreNotSettable` | covered by the managed-list mutant |
| P13R-15 | `validation_failed` and `config_incomplete` are coded task errors | B | `TestTaskErrorsAreCoded`, `TestConfigFailedServerIsReported` (checks the code) | not run (a one-line type) |

### What is left, and why

- P13R-06: the reviewer asked for a cluster test with a promoted standby. The order is tested on constructed snapshots (a promoted primary on another server); a failover inside the cluster
  suite costs several minutes and the code path is pure. Not done.
- P13R-04: the failure of the hidden-set read inside `State` has no test of its own (PostgreSQL gives no way to make that query fail on a live instance); the function it calls is tested.
- P13R-10: the value a removed `max_connections` override falls back to (a derived default of `kingresd`) is not known to the leader, so no warning is given for it.
- P13R-14: `log_line_prefix` and `log_error_verbosity` stay settable: nothing of the console parses the line shape.
- The mock of the console still models a waiting task for a server that is down; the real task ends (the dialog text says what is true).
- P13R-03 changes the design: the task no longer waits for a server that is down; the design says so in "Failure cases".

### Results after the fixes

PostgreSQL 18 and 19 (private installs, lane l5): `pgconfig`, `instance`, `httpapi`, `ops` packages (257 passing tests each) and the 23 `TestConfig*` cluster tests, all passing.
Console: typecheck, lint, 1462 unit tests, the browser tests, `api:check`; `check-contract.mjs` holds.
