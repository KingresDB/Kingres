<!-- SPDX-License-Identifier: AGPL-3.0-only -->
# Independent review of the manual failover of kingresd (increment 3) and of the fixes for F2

Reviewer: an independent reviewer. Date: 2026-11-18. Tree: the committed export `88f66c7d1d` (control plane, contract 0.10.0, design `ha-phase5-part2.md`).

## Scope and method

Read closely: `internal/failover` (`steps.go`, `choose.go`, `promote.go`, `service.go`, `rows_pg.go`), `cmd/kingresd/failover.go` and `ha_failover.go` (the callee side of every call), `cmd/kingresd/ha_proxy.go`, `internal/lease` (`adopt.go`, `manager.go`), `internal/standby` (`freeze.go`, `syncnames.go`, `catalog.go`, the leader's check), `internal/internalapi` and `internal/fence` (authorisation of internal calls), `internal/httpapi/handlers_failover.go`, migration 0014, the contract text of `failoverServer`, and the cluster tests `failover_test.go`, `failover_calls_test.go`. The earlier review (F2-01 to F2-17) was the checklist.

Method: reading with a list of attacks per property; one-line mutants of the F2 fixes against the unit tests (Windows, one package at a time); two cluster probes of my own (a copy of the harness, a new file `zz_review_test.go`, removed with the work area) on PostgreSQL 18.7 and on 19rc2. Anything below that says "read" was not run.

Stated limits up front: I did not run the complete suite (the integrator did, 1255 per major). The failover cluster tests that exist were run by me on 19rc2 only (see below). The two-primary and "verdict none with data missing" cases below are conclusions from the code, not reproduced on a cluster; where I could not reproduce I say so in the finding.

## What was run

- Windows, copy of the tree: `go build ./...` clean; `go vet` of `failover`, `lease`, `standby`, `cmd/kingresd` clean; `go test` of `internal/failover`, `lease`, `standby`, `httpapi`, `contract`: all `ok`.
- Mutants of F2 fixes (below): 6 one-line mutants applied, 6 killed (three first attempts did not compile or did not apply and were redone).
- Cluster, PostgreSQL 18.7: `TestReviewFailoverOfALivePrimary` (my probe: failover of a group whose primary is alive, writers on all three entry points, sampler) and `TestReviewPromoteAfterRoleFileWritten` (my probe). Result: the first FAILS on one assertion of mine (F3-09), the second PASSES as a probe (it prints the wedge of F3-05). 2 tests, 1 failed.
- Cluster, PostgreSQL 19rc2: the same two probes with the same results, plus the existing `TestFailoverOfAGroupWhoseServerIsDead` and `TestFailoverRefusals`: 4 tests run, 3 passed, 1 failed (my probe A, on my assertion F3-09). The existing tests were not run by me on 18.7.
- What the live-primary probe showed on both majors: the task reached `done`, all rows acknowledged before and after the failover were present when read through the entry points of the leader and of the new primary's server (755 rows on 18.7, 763 on 19), the sampler (two writable instances of one group, an out-of-recovery instance with role standby) recorded nothing, and the old instance did not come back writable for 20 s. The event list shows `demoted {stopped true, transferred true}`, `leaseHeld waitMs 1017`, `pinned`, `leaseHandedOver`.

## The fate of the F2 fixes

| Id | State | Evidence | Test that fails without it |
|---|---|---|---|
| F2-01 | fixed | `Decide` ends with `if in.Frozen { d.Set = nil }` (`standby/syncnames.go`, `Decide`), and the branches test it too. | Mutant "the outer frozen guard is `if false`": killed by the unit tests of `standby`. |
| F2-03 | fixed | `ReleaseGroup` writes the marker before the first request (`lease/adopt.go:279`). | Mutant "the marker write is a no-op": killed. |
| F2-04 | fixed | `Adopt` no longer removes the marker; only `Attach` and `Unrelinquish` do (`adopt.go:78-100, 123-146`). | Mutant "Adopt removes the marker file": killed. |
| F2-05 | fixed | `Check` repeats the stop every `FenceRetry` (5 s) and asks `Enforced` each time (`manager.go:483-546`). | Not mutated by me; the review of the unit tests names one. Read only. |
| F2-07 | fixed by changing the rule | the idle file is written by a loop of its own (`manager.go:259-272, 637`); the rule is now "a group without a standby is never stopped for a lapsed lease". Consequence: see F3-12. | Not mutated. Read only. |
| F2-10 | fixed | the `42710` branch reads back everywhere (`standby/catalog.go:150-160`), `Skip` for dead servers. | Not mutated. Read only. |
| F2-11 | fixed | `Attach` refuses when the lease is not held (`adopt.go:131-135`). The call site swallows the refusal: F3-08. | Mutant "the held check removed": killed. |
| F2-12 | partly | owner and epoch, and an error when the file cannot be written (`freeze.go:95-118`). Only the same task is protected, and a call with task 0 bypasses the check: F3-14. | Mutants "epoch comparison weakened" (two forms): killed. |
| F2-13 | partly | `State.Watchdog`, and the check in `Preflight` (`steps.go:362-367`). The check is skipped when the old server does not answer, which is the case it exists for: F3-12. | Not mutated. |
| F2-09 (done half) | fixed | migration 0014 (reading; its test needs PostgreSQL and was not run by me). | Not mutated. |
| F2-02 | check present, no test | `mayRenew` under `rmu` (`manager.go:240-247`); known open as a test. | none, as stated by the author |

The eight product defects of the last section: the per-database catalog (F3-04, F3-09 show what else read "the catalog" as one), term 0 (`staleTerm` now skips only a negative value, `failover.go:731`; the cluster test refuses with `curTerm - 1`, which is 0 on the first failover, so by reading it would kill a mutant `<= 0`; not run by me), roles on the leader's server, the query on an internal path, the promoted instance whose kingresd restarted (`lease_not_held`), the fence that meets a lease coming back (the 503 of 19 was reproduced as fixed by the author; I did not rerun it), the age of the verdict (F3-03), prepared transactions (F3-10). What these suggest was never exercised: a failed-over server that stays a member (F3-09), a leader's server that is also the new primary (F3-01), and anything between F6 and F9 that fails.

## New findings

Summary: 0 critical, 4 high, 8 medium, 3 low, 1 note. No sequence reproduced two writable primaries or lost an acknowledged row on a cluster. The four high findings are reachable by reading and each needs a second condition.

### F3-01 (high): a failed task un-adopts the lease of the promoted primary when the leader's server is the new primary

Place: `internal/failover/steps.go:1512-1522` (`OnFail`: for every group of the input, `Peers.Lease(self, {Role: LeaseRelease})`), `cmd/kingresd/failover.go:764-767` (the handler calls `Unadopt` without looking at the adoption), `internal/lease/adopt.go:148-153` (`Unadopt` deletes `adopted[group]` and `groups[group]`). Read, not run.

Steps: the leader's server `L` also hosts the chosen standby (three servers: this is one case in two). F5 hands the lease to `L` itself as holder, F6 promotes, `Attach` makes the adoption `attached`. A later step fails with a code and not a pause: for example F7/F8 `bad_promote_target` (F3-04), `promote_incomplete`, `target_unavailable`. The engine records the task failed and calls `OnFail`, which sends `LeaseRelease` to `L` for the group. `Unadopt` removes the adoption and the state of the group. The catalog still names the old server as primary, so `groupsToRenewLocked` does not list the group for `L` either (`mayRenew` is false): nothing renews, `Check` iterates `m.groups` and no longer sees the group, and the watchdog file of the promoted primary is not refreshed. The lease runs out at the grantors and the watchdog stops the promoted primary. Result: the group is down, the only promoted copy is stopped, its role file says primary, and a new request cannot choose it (`Choose` makes an unreadable candidate ineligible, `no_candidate`).

A fix must achieve: `LeaseRelease` and `Unadopt` refuse an adoption that is attached (or `OnFail` releases only the groups whose step "Hand the lease" has not finished, and never on the server of a promoted instance); a test with the leader's server as the new primary and a failure injected after F6.

### F3-02 (high): the lease stops an old primary only if that primary's own server knows it has a standby, and the failover does not check it

Place: `cmd/kingresd/ha_leases.go:66` (`Enforced` is `HasStandby(g)` of the local plan), `internal/lease/manager.go:549-560` (`lose` returns when the group is not enforced), `internal/failover/steps.go:362-367` and `:373` (the watchdog and the list of names are checked only when the old server answers). Read, not run.

Steps: the first standby of group g becomes `ready` while the old primary's server is cut off from the leader, so its plan never gets the entry. That server is later also cut off from the other servers (or only its kingresd is slow); its lease expires; `lose` sees "not enforced" and stops nothing; the leader's `Preflight` was satisfied by its own records (ready standby, registered everywhere), F2 is skipped (the server does not answer), F3 obtains the lease, F6 promotes. Clients that reach the old instance directly can still write: two writable primaries. The F2-07 rule makes this explicit: a primary that is judged not enforced is never stopped for a lapsed lease.

A fix must achieve: before F3 the leader must know, for an old server that does not answer, that its plan carried the standby (the plan version the server last acknowledged), or the lease must fence regardless of the plan when a failover could start (the grantors can tell: a transfer or a failed round of a group that has ever had a standby); a test with a stale plan on the old server.

### F3-03 (high): the verdict can be `none` for a window in which the primary ran unobserved

Place: `cmd/kingresd/failover.go:494-501` (`LastKnownAge -= silence`), `internal/failover/choose.go:212-218`, `standby/leader.go:1035-1056` (what the check records). Read, not run.

Steps: the leader loses the heartbeats of the old primary's server at time T (a partition of the leader only; the old server still holds its lease through the other two servers and keeps acknowledging writes). At T+10 its standby disconnects; at T+40 its agent empties the list after the 30 s grace; writes go on without a standby until T+100 when the server really stops. The leader's last record says `sync_streaming = true`, `seen_at = T-1`. At T+110 the operator asks for the failover: age of the check = 111 s, silence = 110 s, `LastKnownAge` = 1 s, below 30 s; `SyncStreaming` is true: the verdict is `none`, and no `allowDataLoss` is required. Everything acknowledged between T+40 and T+100 exists on the old primary only. The subtraction treats the leader's silence as the server's death, which is the "unreached" case of design 4.6, not the dead server.

A fix must achieve: the silence is subtracted only when the failover itself established that the server was down for the whole of it (the grantors' view: no renewal by that holder for the silence, which the leader can read with `GET /lease/state`), otherwise the age is the plain age; a test with a server that is silent to the leader and alive for the grantors.

### F3-04 (high): a database that is created, dropped or inconsistent after F0 fails the task after the point of no return

Place: `internal/failover/steps.go:1085-1130` (`targets`, `fail(CodeBadPromoteTarget, false)`), `:1166-1219`, `:1227-1262` (`targets` is computed again at F8), migrations 0011/0013/0014 (no exclusion of `database.create`, `database.drop`, `table.distribute`, standby tasks), `standby/leader.go:1062` (the leader does not register standbys of a group that is in a failover). Read, not run.

Steps: a sharded database is created while a failover runs (after `registeredEverywhere` in F0 and before F7, or between F7 and F8). Its catalog has the old primary and no standby of the group (the leader leaves the group alone). `targets` finds no `newID` and fails with a non-retryable code at F7 (before any `promote_node` is sent, so no database was moved) or at F8 (every earlier database was moved, this one not). The group is promoted and the catalogs are half moved; the task is `failed` and `OnFail` runs (F3-01). A database dropped meanwhile gives an error (`3D000` is tolerated in the listing, `55006` "being dropped" is not) at a step that retries nothing visible. A database whose catalog lacks the old node by host and port is not caught by F0 (`registeredEverywhere` checks the standbys only), so it also fails after F6.

A fix must achieve: every refusal of `targets` that can be known at F0 is made at F0 for all databases (the old primary as well); the failover excludes database creation and drop and the distribution of tables (a migration clause, as for rebalance); a failure of `targets` after F6 is a pause with a retry, never a failed task; a test that creates a database between F6 and F7.

### F3-05 (medium): the promote handler leaves a standby with role file `primary` that no later call can promote

Place: `cmd/kingresd/failover.go:881-883` and `:899-911`. Run on 18.7 and 19rc2 (probe B).

Steps: the handler writes the role file as primary (`:899`), checks the lease, then calls `pg_promote(true, 60)`. If it ends between the two (kingresd killed, request cancelled, `pg_promote` returns false after 60 s on a standby with a long replay backlog), the instance is still in recovery and the role file says primary. Probe: the old server killed and stopped, the lease handed to the standby's server (`Holds true, Adopted holder`), the role file set to primary by hand, then `POST /failover/promote` three times: `409` each time. The state afterwards: instance 101 `role primary`, `inRecovery` set, `Attached false`. The code path is `!inRecovery` false, then `!li.Role.IsStandby()` returns `not_standby`. The step maps that to withdrawing the pin and failing `target_unavailable` (`steps.go:1002-1010`), and every later run meets the same answer. Only an edit of the role file by hand ends it. With the slow-promotion variant the signal is already sent and the instance leaves recovery by itself, so the task is failed but resumes; with the crash variant it never does.

A fix must achieve: the handler treats "role primary and still in recovery" as a promotion to finish (call `pg_promote` again), or writes the role file only after `pg_promote` answered; a test that stops the handler between the two.

### F3-06 (medium): a pinned standby cannot be replaced before F6

Place: `steps.go:743-818` (`choose` runs again on a resumed task but `Pin` keeps the first writer's choice, `rows_pg.go:226-250`), `steps.go:1002-1010` (the only `ClearPin`). Read.

Steps: F4 pins standby X. Before F6 the server of X dies. A resumed or repeated request re-reads, finds X unreadable, chooses Y, and `Pin` returns X; `afterPin` records X again; F5/F6 fail with `lease_lost` or `promote_timeout` every time. Nothing was promoted, but no way is offered to change the pin. The verdict of the resumed F4 was also computed for Y while the run goes on with X.

A fix must achieve: `choose` withdraws a pin whose standby is unreadable or not a standby and no instance of the group is out of recovery, then chooses again (with the verdict of the new choice); a test.

### F3-07 (medium): a failover that stops a live old primary and then fails leaves the group down, with no operation to start it again

Place: `steps.go:577-613` (F2 stops and relinquishes before any candidate was read), `internal/lease/adopt.go:172` (`Unrelinquish` has no caller in the product), `service.go:Start` (the positions are read at the request only when `allowDataLoss` is false). Read.

Steps: a live server, `hard`, `allowDataLoss: true` (or the only standby's server dies after the request). F2 stops the primary and writes the marker; F4 finds no readable standby: `no_candidate`, task failed, `Cancellable` false since F2. The old primary's data directory is intact but the marker holds it back; `startInstance` through the API is refused by the gate. The operator has to delete `relinquished.<group>` by hand.

A fix must achieve: F2 does not stop a primary that is alive before a standby is proven readable (the plan at the request, repeated at F0, is made even with `allowDataLoss`), or a documented operation (`unrelinquish`) that the failed task offers.

### F3-08 (medium): the promote call answers success when the lease could not be attached

Place: `cmd/kingresd/failover.go:914-917` (the error of `Attach` is logged and `promoted` is answered), `lease/manager.go:549-556` (`lose` does nothing for an adoption that is not attached). Read.

The instance is writable, the manager has `attached = false`, so a lapsed lease fences nothing, and no watchdog was started (`ensureWatchdog` is in `Attach`). The repeat path (`:878`) also ignores the error. The primary is unguarded until the catalog follows at F7 and the normal rule takes over.

A fix must achieve: the handler answers `lease_not_held` (the step repeats `holdLease`) and the repeat attaches or fails; a test with the lease lost between the check and the attach.

### F3-09 (medium): the entry point of a failed-over server resets every connection (run)

Place: `cmd/kingresd/ha_proxy.go:134-148` (the resolver reads the catalog through `ServiceConn(sysdb.Database)`, the local home instance), `internal/failover/steps.go` (nothing rebuilds the old instance). Run on 18.7 and 19rc2 (probe A).

After a completed failover of a live server (task `done`, group moved, 755/763 rows readable through the other two servers), a client that connects through the old server's entry point gets `tls error: read ... connection reset by peer`, repeatedly for 75 s in both runs (120 s in the first). The old server's only primary is stopped and held by the marker; the resolver cannot read the catalog and closes the connection instead of forwarding to the ring. The existing test reads only through the survivors, so it never sees this. The report says "stays stopped and fenced" and the status is `completed`.

A fix must achieve: the resolver falls back to a catalog source that does not need the local primary (any peer's, or the last cached copy) so that a server without a primary still forwards; and a test that reads through the old server's entry point after the failover. (The same dependency on the local home instance affects `staleTerm` below.)

### F3-10 (medium): prepared transactions and `check_cluster` are looked at in the system database only

Place: `cmd/kingresd/failover.go:373-409` (`RecoverPrepared`, `CheckCluster` open `kingres_system`), `:926-949` (`handleFailoverPrepared` too), `steps.go:1403-1457`, `promote.go:164-171`. Read.

Every sharded database has its own catalog and secret, and `recover_prepared_transactions()` acts on the catalog of the database it runs in. F11 recovers and reads incidents in the system database; a `blocked_by` answer of `promote_node` in another database is "recovered" in the system database too. Prepared transactions of database `fo` are left to the worker, the step waits 2 min for a zero count and goes on, and the `incidents` and the `failoverDataLoss` alert never see that database. The report can therefore say "no incident" for a database where the extension would name one.

A fix must achieve: F11 and the loop of F7 run recovery and `check_cluster` in every database of `targets`; a test with a prepared transaction in database `fo`.

### F3-11 (medium): a hand-promoted standby is adopted without a verdict, and the summary says no loss

Place: `steps.go:768-783` (one out-of-recovery instance: `Pin(..., nil, nil)`), `service.go:411-422` (`DataLossPossible` is set only for a recorded non-`none` verdict). Read.

If no pin exists yet (this task did not promote) and one standby is out of recovery, someone promoted it by hand (it may be behind or diverged, and may have taken writes). The task pins it, hands the lease to it and moves the catalog with no verdict and no `allowDataLoss`. The group report shows `unknown` (the default) while the summary `dataLossPossible` is `false` because an empty verdict is not counted.

A fix must achieve: a first pin of an already promoted instance is a pause that names it (`splitPromotion` class), proceeding only with `allowDataLoss`; the summary counts an empty or `unknown` verdict as possible.

### F3-12 (medium): the watchdog precondition and the F2-07 rule leave an unknown in the case they exist for

Place: `steps.go:362-367`, `ha_leases.go:66`. Read.

`Preflight` refuses an old primary with no watchdog only when the old server answers, so for a dead kingresd with a live instance (the case of the watchdog) the question is not asked. Together with F3-02 the first failover on a cluster is the least protected one. A fix must achieve: the leader asks the grantors/lease files, or the standby plan version, instead of the old server, or says in the plan (`dryRun`) that fencing of that server is unproven.

### F3-13 (low): destructive calls default to the map group, and the fence header is optional

Place: `failover.go:758, 795, 832` (a missing `group` is 0, only negative values are refused), `internalapi/internalapi.go:458` (a call without `X-Kingres-Fence` is accepted from the leader, without the check of a run). `staleTerm` (`failover.go:735-744`) lets a call through when the callee cannot read its catalog (it logs). A caller that holds the leader's certificate (or a bug) can send `demote` with no group and relinquish and stop the primary of the map group. A fix must achieve: `group` required (a pointer or a presence check) on every destructive call, the fence header required for them, and `stale_term` refusing when the term cannot be read.

### F3-14 (low): the freeze owner check protects the same task only

Place: `standby/freeze.go:98-100, 116-118` (`own.Task == task && epoch < own.Epoch`, and `task != 0`). A late unfreeze of another task, or a call with task 0, lifts the freeze of the running task; the next step re-freezes (`ensureFrozen` runs in F2, F3, F4), so the window is small. A fix must achieve: an unfreeze lifts only a freeze of its own owner, and a task of 0 is refused.

### F3-15 (low): report and status words

`unfreeze` records `old_primary = waitingForServer` when the old server is still out of recovery, and `completed` is reported when `repoint` skipped a standby or the list is still empty (`steps.go:1480-1493`, `1495-1507`); both are stated as "never fails" by the design but the report text does not say it. The read-only role sees host names and SQL texts in the events (`service.go:Report`). A fix must achieve: the outcome `completed` is `completedWithWarnings` when any step recorded `skipped`, `syncNamesPending`, `preparedRemain` or an incident.

### F3-16 (note): test quality

- `TestFailoverOfAGroupWhoseServerIsDead` stops the old primary with `pg_ctl stop -m fast` (`clustertest.go:221-229`), which flushes all WAL to the standby first. It cannot show a commit that only the dying primary and its standby hold, nor a primary that is alive and cut off. The verdict `none` it asserts is therefore never tested against a crash.
- The sampler needs the same set of two writable instances on two readings 250 ms apart (`failover_test.go:443`); an overlap shorter than half a second, or a write that reaches the old instance by a direct connection, is invisible. It reads through the leader and skips when the leader is down. It cannot see writes at all: only the writers' acknowledgements decide.
- No cluster test covers: a live old primary (my probe showed it works), the leader's server as the new primary, a lagging standby, a failure after F6, a database created during a failover, a second failover while one is paused, a lost answer of any call. All of these are named as not tested by the author; F3-01, F3-04, F3-05 and F3-09 are in that list.
- Verdicts that depend on timing: the "age 30 s" of the verdict and the 2 s positions loop (`readPositions`, 15 s bound) are wall-time dependent; a failover that takes longer than the check age after a live stop reads `possible` (conservative).

## What held under attack

- Two writable primaries in the failover steps as written: the lease is taken by the fence holder before the positions are read, handed over atomically (`Transfer`), and `pg_promote` is called only while NEW's server holds it (checked twice in the handler); in the live probe the sampler saw nothing and the rows were all there.
- A repeated `Preflight`/`check` after a crash does not redo the record (the run row is the proof).
- `Pin` is one conditional transaction; a resumed run never chooses a different standby than the first pin (the downside is F3-06).
- A stale run: the callee refuses a lower fence epoch, and a late `promote`/`demote`/`lease` with an older term gets `409` (tested by the dead-server test; term 0 is compared).
- The list of a frozen group is not changed in any branch (mutant killed); an unreadable standby makes the verdict `unknown` and a lagging target `possible`.
- Cancel after F2 is refused; migration 0014 and 0013 make the stop/restart/config restart and the failover exclude each other in both directions (read).
- Both majors: the same behaviour in the probes and the same pass of the existing tests I ran on 19rc2; no code path depends on a PostgreSQL version.

## Verdict

An operator may use a manual failover of a group whose primary's server is dead, on a cluster where the standby is registered everywhere and the plan was delivered to the old server, as the dead-server test shows (acknowledged rows kept, no second primary seen on 19rc2). I would not yet rely on it for data that matters until these are fixed:

1. F3-01 (the leader's server is the new primary and any later step fails), F3-04 (databases created or inconsistent during the task, and the exclusion of database creation) and F3-05 (the promote handler's state that cannot be resumed): each ends in a group that is down or a task that cannot finish.
2. F3-02 and F3-12 (fencing of an unreachable old primary rests on a plan the leader never checks) and F3-03 (the verdict `none` after an unobserved window): these are the two ways the product can say "no loss" or "fenced" without proof.
3. F3-07, F3-08, F3-09 and F3-11 before a failover of a live server is offered to operators.

## What was not checked

- Any cluster run of the cases in F3-01 to F3-04, F3-06 to F3-08, F3-10 and F3-11 (read only); a failover with writers on a primary that is cut off, not stopped; a standby that lags; two databases; PostgreSQL 18.7 for the existing failover tests.
- The web panel, the CLI, the contract file beyond the text of `failoverServer`, the `resetAutoFailoverCounter` operation, and `graceful`/map group/automatic paths (unsupported on purpose).
- The extension's own functions (`promote_node` and its answers) beyond how the control plane reads them; a real `pg_rewind` or base backup; Windows.
