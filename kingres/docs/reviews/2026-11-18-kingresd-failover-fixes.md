<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
# Independent review of the fixes for the first failover increments and for the second review of rebalancing in kingresd

Reviewer: an independent reviewer. Date: 2026-11-18.

## Scope and method

Reviewed: the read-only export of the repository state that carries the fixes for FO-01 to FO-16 (review of the failover increments 0 to 2) and
ST-05 to ST-10 (second review of rebalancing and stabilisation). Read closely: `internal/lease` (grantor, transfer, release, adopt, start gate,
watchdog file, manager), `cmd/kingres-watchdog`, `cmd/kingresd` (`ha_leases.go`, `ha_failover.go`, the start sequence in `main.go`),
`internal/instance` (role file, start gate, `Manager.start`), `internal/cluster/roles.go`, `internal/standby` (agent operations,
`provedStandby`, sync names, freeze, catalog registrar, leader steps), `internal/task` (engine, cancel, guard, gate, PostgreSQL store),
`internal/rebalance` (`findMove`, `Recover`, `StartMove`, `AddSkipped`, marks), `internal/sysdb/migrations` 0011 to 0013,
`internal/ops/instancectl.go` and `instancerun.go`, `internal/auth` (revalidator, retry), `internal/httpapi/guard.go`, the README sections
named in the request and the design file with its last section "Found while building".

Method: reading with a list of attacks per property (below); one-line mutants of the fixes run against the unit tests (each mutant is one
changed line in a copy of the tree, one test binary at a time); unit-level probe tests of my own, written in the copy only, for every
finding that says "probe". Failover itself is not in this tree, so each mechanism was judged by what a failover needs of it, with the design
(sections 2, 3, 6) as the specification.

This review is not complete either; the last section says what was covered and what was not.

## What I ran

- Whole module except the cluster tests (`internal/clustertest`), on Linux, with no PostgreSQL: every package passes (the contract tests read the
  `api` directory next to the module, which I copied). In the verbose run of
  ten packages (lease, standby, instance, task, rebalance, cluster, ops, watchdog, contract, httpapi): 464 passed, 40 skipped (all of them need a
  PostgreSQL build).
- The same five packages that skip without PostgreSQL (standby, instance, task, rebalance, sysdb) with the PostgreSQL 18.7 build of the
  project: 155 top-level tests passed (213 with subtests), none skipped. So the tests of ST-05 to ST-07, migrations 0011 to 0013 and the
  read-back of the catalog were run, not only compiled. The files named `*_linux_test.go` ran on Linux and passed.
- 9 probe tests of my own (all in the copy, removed afterwards): all 9 fail, each one showing a finding below (F2-01, F2-02, F2-03, F2-04,
  F2-05 twice, F2-07, F2-08, F2-11).
- 44 mutants (one changed line each, one test binary at a time): 37 killed, 7 survived. Of the 7, one is equivalent (the explicit "two claims"
  check in `PrimaryOf` only adds a log line; the code falls through to the same answer). The other 6 are gaps in the tests and are named in the table:
  "no recovery check" and "running and cannot be asked is allowed" in `provedStandby`, "cannot be asked" in `primaryRunning`, "task unreadable at
  the boundary cancels", "the freeze is not written when it is made" (`Freeze` does not persist; only the loading is tested), and "`Check` skips a
  group whose lease is marked lost" removed (the retry of a fence is neither built nor tested, F2-05).
- Cluster tests: none. The script that starts them and takes the machine-wide lock is not part of the export and I did not start the cluster
  tests around it. No claim in this report rests on a cluster run. PostgreSQL 19 was not available.

## Fate of the earlier findings

State: fixed / partly / not. "Mutant" is the result of my one-line mutant of the fix against the unit tests ("killed": a test fails).

| Id | State | Evidence | Test that fails without the fix |
|---|---|---|---|
| FO-01 | partly | The file of a group without a standby is written at every pass, valid for duration minus margin (`lease/watchdogfile.go:84-93`, `lease/manager.go:238-241`); `off` is written only by `DisarmWatchdog`, which still has no production caller. Left: it is written only when a file exists (a group that never was enforced has no watchdog at all, `watchdogfile.go:85`), the watchdog starts at the first round after the enforcement, and the file now runs out during a stall of the renewal pass (F2-07). | Mutants "idle write removed", "idle file written as off", "watchdog fences on off": killed |
| FO-02 | partly | The no-name branch tests `Frozen` first (`standby/syncnames.go:78`). The branch of a streaming standby does not (`syncnames.go:90-96`): F2-01. | Mutant killed by `TestAFrozenListIsNotEmptiedWhenNoNameIsPlanned` |
| FO-03 | fixed | `markerExists` for an unknown group looks at the markers of every group (`lease/startwait.go:63-69`). Gate is keyed on the role file's group; see F2-06 for a role file that names another group. | Mutant killed by `TestTheGateOfAnInstanceWithoutAGroupHoldsForMigratedMarkers` |
| FO-04 | partly | `provedStandby` (`standby/agent_ops.go:393-435`) guards `Remove`, `Rebuild`, `Create` and the supervision. Two of its legs have no test, a stopped directory with an unreadable meta file is thrown away without looking at its data, and the check and the removal are not one step: F2-08. | Mutants "no standby.signal check" and "role does not say standby": killed. "No recovery check" and "running and cannot be asked is allowed": SURVIVED |
| FO-05 | fixed | `Off` is written with `validMs` 24 h (`watchdogfile.go:104`). I did not run an old watchdog binary (the first review did). | Mutant "validMs 0" killed by `TestTheWatchdogFileFollowsEnforcementBothWays` |
| FO-06 | fixed | `transferDone` is `Holder == to`, `releaseDone` is `Granted && Holder == ""` (`lease/adopt.go:359-363`). A grantor of the older version extends the caller's own lease and counts for nothing: harmless. | Both mutants killed |
| FO-07 | partly | `primaryRunning` refuses while the instance runs (`adopt.go:369-382`); it passes when no instance is found, which includes an instance whose role file is unreadable and two directories claiming one group: F2-06. | Mutant "runs" killed; "cannot be asked" SURVIVED |
| FO-08 | partly | The read-back reads every primary of the catalog (`standby/catalog.go:259-306`). Not in the branch of `42710` (`catalog.go:150-157`), the repeat after a timeout, which is the case the read-back is for; and it needs every primary reachable (F2-10). | Mutant "no remote read-back" killed |
| FO-09 | partly | `MustRole` answers `RoleUnknown` (`instance/role.go:126-133`); two claims give no primary (`cluster/roles.go:97-100`). The explicit check for two claims is redundant (the code falls through to "not found" anyway: mutant survives, equivalent), and with no primary found nothing protects either directory: F2-06. | Mutant "default role" killed by `TestRoleFileAndDefaults` |
| FO-10 | fixed, with a side effect | `Adopt` clears the marker (`adopt.go:70-75`). The marker was what held the old primary directory back (F2-04). | Mutant killed by `TestTransferStopsRenewingAndSurvivesARestart` |
| FO-11 | partly | `Cancel` refuses when the task cannot be read; `honourCancel` asks the guard at the boundary (`task/task.go:501-516, 970-981`); migration 0013 covers every instance of the server by host and `config.restart` cluster-wide. Left: the trigger is bypassed when the recording fails, and one-sided (F2-09); "task unreadable at the boundary is not cancelled" has no test. | Mutants "cancel blind", "always honour", "no cancel guard", both 0013 clauses: killed. "Unreadable at the boundary cancels": SURVIVED |
| FO-12 | fixed, small gaps | `failover-freeze.json` is written and read (`standby/freeze.go:213-271`); gaps in F2-12. | Mutants "freeze not loaded", "gone not loaded": killed. "The freeze is not written when it is made": SURVIVED |
| FO-13 | partly | `ReleaseGroup` waits for the round in flight (`adopt.go:273-274`). A round that `Renew` starts after that wait from a list taken before is not stopped (F2-02). | Mutant killed by `TestAReleaseWaitsForTheRenewalInFlight` |
| FO-14 | fixed | The gate is asked again when pg_ctl returns (`instance/manager.go:377-387`). | Mutant killed by `TestAnInstanceThatCameUpAgainstTheGateIsStopped` |
| FO-15 | not changed (declared) | A listed name stays until the plan drops the standby (`agent_primary.go:266-268`). Increment 3 must refuse a failover while a listed name is not registered; see F2-01 for a way the list changes anyway. | no test, none claimed |
| FO-16 | partly | Start, stop, restart act on every primary and try every one (`ops/instancectl.go:126-158`); the watchdog of another group is killed (`ha_leases.go:322-366`) but only one that this process started. The removal before the primary applied the list: unchanged, bounded by the grace. | Mutants "start stops at the first", "stop stops at the first": killed |
| ST-05 | fixed | The mark is kept with the row (migration 0012, `rebalance/pg.go:1048-1054`); the time path remains only for rows without a mark, with a tolerance of 10 s (`pg.go:559-585`). Residual (low): `StartMove` of a retry resets the mark, so until `SetMark` runs the row is found by the time path, whose 10 s of tolerance can match the record of the attempt before it (a quick failure), and the new attempt is then settled as failed with the old error. | Mutants "time path only", "no tolerance", "mark not kept": killed (the tests use a stub of the extension) |
| ST-06 | fixed | `StartMove` looks for the row of this attempt first, `AddSkipped` inserts only when absent (`pg.go:836-899`). | Both mutants killed |
| ST-07 | fixed | A session that is alive is waited for past `RecoverWait` (`pg.go:650-711`). The call `internal_move_alive` is only tested against a stub; an error of it is read as "not alive" (note in F2-16). | Mutant "no wait for a live move" killed by `TestRecoverWaitsForALiveMoveBeyondTheBound` |
| ST-08 | fixed | `retryWait` doubles up to ten minutes (`pgconfig/converge.go:73,101-115`). The task list is still not pruned (README says so). | Mutant killed by `TestRetryWaitGrowsToTenMinutes` |
| ST-09 | fixed | Mutants of the round are now killed by tests (ST-05, ST-06, ST-08, ST-10 above). | - |
| ST-10 | fixed | `RetryingLimited` with 8 waiters for the join path (`cmd/kingresd/main.go`, `auth/retry.go:35-39`). The first attempt of every caller is not counted. | Mutant killed by `TestRetryingLimitedAnswersAtOnceBeyondTheBound` |

## New findings

Summary: 1 high, 8 medium, 7 low, 1 note. Nothing here lets two instances of one group accept writes in the tree as it is (failover does not
exist); F2-01 to F2-06 are where increment 3 would create that, or lose acknowledged data, if it is built on them as they are.

### F2-01 (high): the list of a frozen group still changes when a standby is added or replaced

Where: `internal/standby/syncnames.go:90-96` (the branch where a planned standby streams has no test of `Frozen`), `internal/standby/agent_primary.go:180`
(`names` is built with `dropGoneEntry(e, gone, frozen)` only, so a new name enters while the group is frozen) and `:198`.

What is wrong: the fix of FO-02 tests `Frozen` first only when no name is planned. The design (I2, section 2.4) rests on "the list of a frozen group
is never changed": the failover reads the positions of all ready standbys that the list names and picks the highest.

Sequence: group 1 has `ANY 1 (kingres_standby_7)` and is frozen by a failover. The leader's check (standby tasks are not excluded by
migration 0011 or 0013) plans standby 8 for the same group; it is registered and streams. At the next pass the agent builds
`Planned = [7, 8]`, `Applied != want`, `Decide` returns `ANY 1 (7, 8)` and the agent writes and reloads it. A plan that dropped 7 gives
`ANY 1 (8)`. Commits are then acknowledged by 8 alone, which the failover (it read 7) may not have read. Probe: `Decide` with `Frozen: true`
returns a new list in three cases (replaced, widened, an empty list given a name).

A fix must achieve: nothing changes `synchronous_standby_names` of a frozen group (every `d.Set` is dropped when `Frozen`), a new name is not
added while the group is frozen, and a test for each of the three cases. The freeze should also carry the epoch (F2-12).

### F2-02 (medium): a round of `Renew` that started from an old list is sent after `ReleaseGroup` returned

Where: `lease/manager.go:228-237` (the list of groups is taken once; the loop does not look at `relinq` again before `m.round`), `lease/adopt.go:267-274`.

What is wrong: the fix of FO-13 waits for the round in flight (`rmu`) but a round that is not in flight yet can start after the wait.

Sequence: a server runs groups 1 and 2; `Renew` has taken `[1, 2]`; while it works on group 1 the failover releases group 2
(`relinq[2]` is set, `rmu` is free for an instant, the release goes to the grantors); `Renew` then reaches group 2 and sends a plain renewal for
the group that was just released. Probe (two groups, the idle write of group 1 slowed): the calls are `release:2:c release:2:b grant:2:c grant:2:b`
and both grantors hold group 2 for the releaser afterwards.

Effect: the new holder is refused until duration plus margin (13 s with the defaults); in the design a refusal of the grantors ends the step
(`lease_still_held`), so a switchover can fail. Liveness, not safety (the grantors refuse every other holder while the old one is the holder of record).

A fix must achieve: the loop checks `relinq` and adoption under the same lock as the round (or `round` itself refuses a relinquished group).

### F2-03 (medium): `ReleaseGroup` keeps its mark in memory only

Where: `lease/adopt.go:263-325` (only `m.relinq` is set; compare `Transfer`, `adopt.go:248`, which writes the marker file).

What is wrong: after a release a restart of kingresd forgets it. Probe: after `ReleaseGroup` no `relinquished.<group>` exists and a manager built
on the same directory has `Relinquished(group) == false`.

Sequence: switchover of group 1: the old primary is stopped (fence marker), the lease is released at the grantors, kingresd on the old server
restarts (upgrade, crash, OOM); the catalog still names this server as primary until `promote_node`; the first round wins the free lease, `acquired` sees
the fence marker, `startPrimary` removes it and starts the old primary under a lease, while the other server is adopting the group. If the other server
already holds the lease the old one is refused and nothing happens; if not, the failover waits behind it or fails.

A fix must achieve: write the marker before the first release request (and remove it only by `Unrelinquish` or the adoption of F2-04), as `Transfer` does.

### F2-04 (medium): `Adopt` removes the only mark that keeps an old primary directory from starting

Where: `lease/adopt.go:70-75`; gate `lease/manager.go:181-198`.

What is wrong: FO-10 made `Adopt` clear the relinquished marker so that a later failover can adopt again. After a clean hand-over there is no
fence or wait marker, so the gate no longer holds the directory back. Probe: after `Transfer` the gate refuses, after `Adopt(group, fence)` it lets the
directory start, while this server now renews the lease of the group.

Sequence: switchover A to B (A keeps its old primary directory with role file `primary`, as long as the demotion of section 6.3 has not rewritten it);
a later failover of the same group takes A as fence holder (`AdoptFence`); kingresd on A restarts; `startWritable` starts the old primary of the group, and the
lease for the group is held by A as fence holder. Two writable instances of the group, one with the lease.

A fix must achieve: an adoption that has no attached instance replaces the relinquished marker by a hold of the same strength for every directory that
the role files name primary for that group; the clearing belongs to `Attach` or `Unrelinquish`, not to `Adopt`.

### F2-05 (medium): a fence whose stop failed is not tried again; a lease marked lost is never judged again

Where: `lease/manager.go:432-446` (`st.lost` is set before the fence and only a successful round clears it), `:453-469`, `:473-498`.

What is wrong: design 6.4 says "the manager's `Check` retries the stop every 5 s" and raises `oldPrimaryNotStopped`; the code does neither. Probes:
(a) `StopImmediate` fails: after the lease ran out and five more checks the manager tried once and the instance still runs; (b) the group is not
enforced when the lease runs out (marked lost, nothing stopped) and becomes enforced afterwards (a standby is added): five checks later the instance
runs with an expired lease and the manager never stops it. Only the watchdog is left, and it exists only if it was started (F2-13). A mutant that removes
the `lost` skip survives: nothing tests a second attempt.

A fix must achieve: the stop is repeated until it succeeds (or the lease is held again), `Enforced` is read at every check and not once when the lease
is marked lost, and the failure is an alert.

### F2-06 (medium): an instance that the role files do not name as the primary of a group is invisible to the lease, the watchdog and the hand-over

Where: `cluster/roles.go:88-108` (`PrimaryOf`), `:179-215` (`SyncRoles` fills only an unknown group), `lease/manager.go:480-483`, `lease/adopt.go:369-372`,
`cmd/kingresd/ha_leases.go:58-62,316-321`, `cluster/roles.go:73-82,284-300` (`WritableIDs` falls back to the home instance, `startWritable` starts it).

What is wrong: three states give "no instance for this group": (1) a role file that cannot be read (`RoleUnknown`), (2) two directories that claim one group
(FO-09 made `PrimaryOf` answer nothing), (3) a role file whose group is not the group the catalog gives for its port (nothing corrects a known group).
In each: `Fence` writes the marker and returns nil without stopping anything, `primaryRunning` lets `Transfer` and `ReleaseGroup` go on, no watchdog is
started (its file name comes from `PrimaryOf`), the group is not renewed in state 1 (an unknown role is not writable, so its port is not matched) and in state 3 it is renewed with no instance behind it, and
the start gate is keyed on the role file's group, so a marker of the catalog's group does not hold it. In state 1 `startWritable` starts the home
instance through the fallback.

Sequence: the role file of a running primary becomes unreadable (restore, permission, disk fault); the failover finds the lease expired at the
grantors, adopts and promotes; the old primary has no lease logic and keeps accepting writes. The FO-09 text calls "unknown" safe because nothing
deletes it; it is not safe for split brain.

A fix must achieve: fail closed. A directory that may be a primary and cannot be attributed to a group is held by the gate (any marker, and a marker of
its own), is stopped by the first fence of any group of this server's catalog rows with its port, makes `Transfer` and `Release` refuse, and raises an
alert; the role file of a known group is compared with the catalog at every round.

### F2-07 (medium): the file of a group without a standby runs out while the renewal pass stalls

Where: `lease/manager.go:234-242` (rounds are sequential, the idle write comes after the round of its group), `lease/watchdogfile.go:84-93` (validity
duration minus margin, 7 s with the defaults), `lease/manager.go:274` (a round that gets no answer lasts 7 s).

What is wrong: the rule (README, "Enforcement rule", and the comment of `writeWatchdogIdle`: "a group that is not enforced does not lose its primary for that") is not kept by the fix of FO-01 when
the grantors do not answer (a partition that drops packets, hung peers). A pass with no majority lasts one round per group, 7 s each; the file of
group 1 is then written every 7 s plus the rounds of the other groups: with two groups every 14 s, against a validity of 7 s. Probe (real clock,
duration 1 s, margin 0.4 s, two groups, voters that hang): the longest gap between two writes of group 1 was 1.53 s against a `validMs` of 600
ms. The watchdog (it exists for any group that was enforced once) then stops a healthy single-copy primary: availability lost, as
the rule "stopping the only copy would only cost availability" wanted to avoid. With one group the gap is about equal to the validity and the
result depends on the 250 ms poll.

A fix must achieve: the idle file is written by a loop of its own, independent of rounds, or its validity covers the longest pass; and the rule
(README) says which of the two is wanted.

### F2-08 (medium): `provedStandby` has three weak points

Where: `standby/agent_ops.go:49-62, 172-187, 393-435`.

1. "Unfinished copy" is decided by any error reading the meta file, not by its absence (`merr != nil`), and for a stopped directory the branch
   `!up && unfinished` returns nil without looking at the data (`:408`). Probe: role file standby, no `standby.signal`, `PG_VERSION` present, meta file
   `{broken`: `Remove` deletes the directory. A completed standby whose meta file cannot be parsed (a half-write cannot happen, the write is
   atomic; a restore, a disk fault can) is deleted; so is a former primary that was given a standby role file before its copy was started.
   A fix must achieve: an unfinished copy is recognised by a positive mark that `InitStandby` writes first and removes last, not by the lack of the
   last file.
2. Two legs have no test: "running and not in recovery" and "running and cannot be asked" (mutants survive). Both are the legs that matter on a
   promoted instance.
3. The check and the removal are two steps (`provedStandby`, then `Manager.Remove`, which stops and deletes). The agent's own lock (`opMu`)
   does not stop a promotion that does not take it. Increment 3's promote must run under `opMu` or take the lifecycle lock across check and delete.

### F2-09 (medium): migration 0013 is bypassed when the record cannot be written, and does not look at tasks already running

Where: `ops/instancerun.go:95-115` (`openTask`: any error but "busy" or "excluded" keeps the task in memory and the operation runs),
`migrations/0013_failover_exclusion.sql` (the branch of `server.failover` checks only KX002 and KX003).

What is wrong: (1) a stop or restart on a server that cannot reach the leader (the partition a failover is for), or whose record the leader cannot
write, is not refused: the exclusion error is the only error that stops it; (2) the rule works in one direction: it refuses to record a stop or
restart while a failover group is unfinished, but a failover can start while an `instance.stop`, `instance.restart` or `config.restart` of the same
servers is already `running`; (3) a failover group row that is still `pending` may have no servers yet (the columns are nullable), and then nothing is excluded for it.
No wedge was found: the rule joins the task, which is finished when the task is, so an unfinished group row of a finished task excludes nothing; a paused
failover task does block stops, config restarts, rebalances and every other failover until someone resumes or cancels it, which is the intent.

A fix must achieve: a failover refuses to start (or waits) while an operation of the same servers is active; the operation on an enforced
primary does not run when its record cannot be written (or the failover step re-checks the servers' state itself).

### F2-10 (low): the read-back of the catalog skips the repeat after a timeout and needs every primary reachable

Where: `standby/catalog.go:150-157` (branch of `42710`: the coordinator's row only), `:259-306`.

The README says "`42710` is read as success only when the row is read back"; in the branch the read-back on the other primaries is not done, and that
branch is the repeat after a call that did not finish, the case FO-08 describes. Second, `readBackEverywhere` fails (and retries) while any primary
of the catalog cannot be read: registration and removal of a standby then block while a server is down, which is when increment 3 has to remove the
standbys of the dead primary. A fix must achieve: the read-back is also done in the `42710` branch, and it skips servers that are declared gone or dead
with a recorded incident instead of blocking.

### F2-11 (low): `Attach` before the first round of an adopted group panics

Where: `lease/adopt.go:84` (`Adopt` deletes the state of the group) and `:124-135` (`Attach` calls `writeWatchdogFile`), `lease/watchdogfile.go:59-61`
(`st := m.groups[group]` is dereferenced when the group is enforced). Probe: `Adopt` then `Attach` on an enforced group before a round: nil pointer dereference.
A step that attaches before it waited for the held lease would crash kingresd. A fix must achieve: `Attach` refuses (an error) when the lease is not held.

### F2-12 (low): the freeze is not bound to the task that made it, and its durability is best effort

Where: `standby/freeze.go:171-176` (`Task` and `Epoch` of the request are only logged), `:223-244` (a failed write is logged, the call answers
success), `:251-258` (a file that cannot be parsed is ignored), `:213-220`.

A deposed leader's late `unfreeze` lifts the freeze of the new holder of the task; a freeze that could not be written is lost at the next restart
although the caller was told it was kept; a wall-clock step forward before a restart drops it (stated). A fix must achieve: epoch per group, a lower epoch is
refused; an error to the caller when the file cannot be written; the test of the write side (a mutant of it survives).

### F2-13 (low): nothing says whether a watchdog runs; process-local kill; respawn churn

Where: `cmd/kingresd/ha_leases.go:256-271` (the program is not found: one warning), `:322-366`, `lease/manager.go:415-418`.

(1) The watchdog is started at the first round after the group is enforced, and only for an enforced group; a hung kingresd in that window, or a
server without the program, has no cover and nothing reports it. Increment 3 must read "watchdog running" for the old primary's server before it counts
the old primary as fenceable (and fail the first-failover precondition otherwise). (2) `kill()` ends only a watchdog that this process started; one that
survived a restart of kingresd keeps the marker of the old group. (3) After a restart the previous watchdog holds the lock, so every
`ensure` starts a process that exits at once and `exited` is closed: one fork per pass and group, for as long as the old watchdog lives.

### F2-14 (low): statements of the README and the design that the code does not keep

- README, "Leases and fencing": the file is `instances/1/lease.json` with `{seq, validMs}` (now per instance, with `off`), the marker is
  `lease/fenced` (now per group), "for a group without a standby nothing stops the instance" (the watchdog may, F2-07), "a step of the system clock
  does not matter because no decision reads it" (the freeze file, task leases and the forwarding proof read it; see "Clocks").
- README, the paragraph "Review ST-01 to ST-13 ... what was fixed in this step", lines 2749-2750, still says "Not done in this step: ST-05 ... ST-10".
- Design 6.4: "`Check` retries the stop every 5 s" and the alert `oldPrimaryNotStopped`: not built (F2-05).
- README, FO-09 paragraph: `PrimaryOf` "answers nothing when two directories claim one group": true, but it is not a protection of either directory (F2-06).

### F2-15 (note): clocks outside the lease that a failover will meet

- The task lease is written and compared with `clock_timestamp()` of the system database's host (`task/pgstore.go:90-99,146-165`). When the system
  database moves to another server (the map group's failover), leases written under the old host's clock are read under the new one: with a new
  clock ahead every running task is treated as lapsed and stands down at its next write (safe: the epoch fences it), with a clock behind a dead
  holder's task waits longer.
- `clock.Real.Mono` is Go's monotonic reading, which on Linux does not count time while the machine is suspended (a paused virtual machine): the
  README's "frozen as a whole" limit; the margin does not cover it.
- The lease, the watchdog, the liveness decisions, the freeze lapse and the session revalidator use durations or Go's monotonic readings; see below.

### F2-16 (low): test quality

- The tests of ST-05 and ST-07 run against a stub of the extension made in the test (`rebalance/st05_st07_pg_test.go:21-35`): the table
  `bucket_move`, `resume_bucket_moves` and an advisory-lock version of `internal_move_alive`. The call in `Recover` hides an error as "not alive", so
  if the real function has another signature the wait silently becomes the old 10-minute bound. No cluster test covers it.
- `TestAReleaseWaitsForTheRenewalInFlight` decides on `time.Sleep(300 ms)` (a pass when the release is slow to start); the mutant is killed, so
  the test is not empty, but it is a timing verdict.
- The negative waits (the watchdog test of `off` waits 3 s and passes if the process never ran) are covered by the control tests beside them.
- Survived mutants: see the list in "What I ran".
- Nothing tests two groups on one server in the lease manager (F2-02, F2-07 are both invisible to the one-group rig).

### F2-17 (low, test only): the cluster test of the wall-clock step does not wait for the instance

See the next section.

## The open defect: `TestHeartbeatsAndLiveness`, "a step of the wall clock changes nothing", 503 on PostgreSQL 19

What I could establish by reading (I could not run the cluster test):

1. Nothing on the path of `GET /servers` reads the stepped clock. The offset of the fault hook reaches exactly three clocks:
   the lease manager's and the grantor's (`ha_leases.go:44`: its `Mono` is the unstepped monotonic reading, only `Wall` is shifted and nothing
   decides on it), the liveness tracker's (`ha_liveness.go:27`: decisions on `Mono`, `Wall` for display) and the configuration agent's `Now`
   (`config_wiring.go:31`). The guard's own `now` is `time.Now` (`httpapi/server.go:179-185`, `deps.Now` is never set), the revalidator and the
   session store compare Go times with monotonic readings, `rolvaliduntil > now()` is PostgreSQL's own clock, and the forwarding proof uses
   `time.Now`. So the step cannot be what makes `currentRoles` fail.
2. The message is the one of an error from `Authenticator.CurrentRoles` that is not "invalid credentials" (`httpapi/guard.go:441-444`): an
   `ErrUnavailable` that survived the retry of 10 s (`auth/retry.go:41-80`). Two things produce it: the instance cannot be connected to
   (connect timeout 5 s per try), or `Confirming()` is true for the whole wait (`lease/manager.go:597-616`).
3. The subtest before it pauses kingresd of node 1 (SIGSTOP of the process only, `clustertest/proc_unix.go:24-32`) until node 0 shows it dead (6 s). The
   cluster has a standby for that group, so the group is enforced: the watchdog (a separate process) fences node 1's primary after the file
   runs out (about 4 s), with an immediate shutdown. On `SIGCONT` kingresd's `Check` finds the lease over (the monotonic clock ran on), the
   manager starts the instance again once a round succeeded (`startPrimary`, crash recovery), and the same subtest ends when node 0 shows node 1
   healthy, which only says that kingresd answers heartbeats. The 6 s loop in that subtest reads `lvStatus(n1, ...)` and ignores a 503
   (it returns "" and only "dead" fails), so the restart is invisible to it. The next subtest reads at once and fails on the first status that is "".
4. So the likeliest cause is not the clock but the restart of node 1's primary after the watchdog fence of the previous subtest, which on PostgreSQL 19
   takes longer (or its start is delayed by a round) in about one run of five. What would show it: the kingresd log of node 1 in a failing run
   has `re-checking the role of a session` lines; "connection refused" or "starting up" confirm the instance, "has not renewed the lease of its group"
   would point at `Confirming()`; the PostgreSQL log of node 1 has `database system was interrupted` shortly before.
5. The fix of the test: before the first read of the step subtest (and at the end of the paused subtest) wait for the lease to be freshly held and the
   server to answer a read, as the blip subtest already does with `lvLeaseJustRenewed(t, n0, n1)`. The fix of the code, if the cause is
   `Confirming()`: not needed unless the log shows it.

I state this as the best reading, not as proved; the log lines above decide it.

## Clocks: what uses which clock

- Lease holder (`validUntil`, `Check`, `Holds`, `Confirming`), grantor (`expiry`), the watchdog (deadline since its own start), the freeze lapse
  in memory, the task engine's local guard, the revalidator and session store: monotonic, or Go time values that keep their monotonic reading. A step of
  the wall clock of one server changes none of them (the grantor's persisted file holds durations only; the watchdog file holds a sequence
  number whose base is the wall time at start, compared for inequality only).
- Wall clock on purpose: `failover-freeze.json` (the end of a freeze and of a "gone" server in milliseconds of the wall clock, read at start): a
  step forward before a restart ends a freeze early, a step back extends it up to ten minutes; the proof of a forwarded request and of an internal
  call (a window around `time.Now`: a server whose clock stepped by more than the window has its forwarded requests refused and refuses
  others'); PostgreSQL's `now()` for `VALID UNTIL` of roles.
- The clock of the system database's host: the task lease (`clock_timestamp()`), `started_at` of rebalance rows (written by the system database;
  compared with the extension's clock only for rows without a mark, with 10 s of tolerance). A forward step there makes every running task's lease look
  lapsed (claim by the next holder, the epoch fences the old run); a backward step delays the takeover of a dead holder's task.
- The step has no effect on a lease, the watchdog file, a session check or a task lease of the stepped server in a way that is unsafe; the effects
  above are liveness or documented.

## What held under attack

- Split brain through the lease: a stale renewal after a transfer is refused (holder changed); a transfer or release repeated after a lost answer is
  done at the grantors (no second effect); a transfer to grantors of the older version is refused cleanly and their extension of the caller's own
  lease is harmless; a grantor that cannot write the change does not grant; a grantor restart assumes the full duration; the lease is valid from the
  start of the round and the late answers of a paused holder give nothing; the gate is asked twice around `pg_ctl start`.
- The watchdog file in each state: `off` never fences (until a normal file arrives); missing file at the start fences at once; an unreadable or
  unparseable file leaves the countdown of the last good one running (fail safe); a file from the older version has `validMs` above zero; a kingresd
  with a stepped wall clock only changes the base of the sequence number.
- Start gate: unknown group with a migrated marker (FO-03); a role file that cannot be read is held like a primary; a role file that is missing for
  directory 1 is the any-group case; the wait marker for an enforced group at a cold start.
- Data loss: a role file that says primary, is unreadable or is missing, a stopped directory without `standby.signal`, and a standby that is running outside recovery or
  cannot be asked (the last two by reading: the legs are untested) all refuse; the crash between `pg_promote` and the role file is closed by writing
  the role file first (design), and the agent then sees `primary`. The sequences that remain are in F2-08.
- Removal order of a standby: the plan without it, the instance (refused when not proved), the catalog row (retry on KS009, the extension refuses
  while the list names it), the record; every interruption is repeated idempotently; a failed call leaves a record `removing` that nothing recreates.
- Tasks: a cancel is refused when the task cannot be read, the guard is asked again at the boundary and a mark past the point of no return is
  ignored; the lost task lease stands down at a boundary with nothing recorded and no hook; a step that is not idempotent and was interrupted fails
  with a code instead of running twice.
- Migration 0013 refuses a stop or restart of any instance of a server that an unfinished failover group names (by host, not by the first
  instance only) and a configuration restart while any group is unfinished (both clauses are killed by a test), with the limits of F2-09; it cannot
  wedge the cluster: a group row of a finished task is never counted.
- The standby steps (create, remove, rebuild, registration) were walked at each boundary: a crash after any step is finished or repeated by the next holder.

## Verdict

Increment 3 (a failover started by hand) can be built on this ground, with one blocker and a short list of things that must be done before the step
that depends on each:

- Before anything else: F2-01 (the list of a frozen group changes). The invariant I2 is the data-loss argument of the whole failover, and the
  freeze, as it is, does not hold it.
- Before the hand-over steps (release, transfer, adoption): F2-02, F2-03, F2-04.
- Before the old primary is counted as fenced: F2-05, F2-06, F2-13 (a way to ask whether the watchdog runs).
- Before the failover starts: F2-09 (exclusion of operations in flight).
- Alongside: F2-07 (a decision on what the watchdog does for a group without a standby), F2-08 (a positive mark of an unfinished copy; the promote under
  the agent's lock), F2-10, F2-11, F2-12.

Of the twenty-two earlier findings, twelve are fixed (FO-03, FO-05, FO-06, FO-10, FO-12, FO-14, ST-05 to ST-10; FO-10 and FO-12 with a
small residual), nine are partly closed (FO-01, FO-02, FO-04, FO-07, FO-08, FO-09, FO-11, FO-13, FO-16; the residual of each is a finding above)
and one is declared open (FO-15). The tests are real: 37 of 44 mutants of the fixes are killed, and the
surviving six name the legs to add.

## What I covered and what I did not

Covered: every fix named in the request, by reading the code at the lines cited, by mutants (44) and by unit-level probes (9); the lease
protocol (grantor, holder, transfer, release, adoption, watchdog file, start gate, restart); the standby agent's guard, sync list decision, freeze,
catalog registrar and the leader's steps at each boundary; the task engine's cancel, guard and gate; migrations 0011 to 0013 by reading, with the
PostgreSQL tests of them run; ST-05 to ST-10; the clock inventory; the open defect by reading.

Not covered:
- Any cluster test (no run of the cluster suite, none of the three allowed short runs): the 503 defect is not reproduced; the claims about a real
  watchdog binary (old and new together), real fencing under SIGSTOP and the interplay of restarts are by reading and by the unit tests only.
- PostgreSQL 19; Windows (`lock_windows.go`, the Windows process code, a run of the module there).
- The real extension: ST-05 and ST-07 were tested against a stub; the extension's own functions and the read-back against a real multi-node catalog were
  not exercised (the registrar tests that need it ran against the project's PostgreSQL 18.7 build with whatever those tests set up).
- The REST and panel layers, the proxy, peer authentication, the client entry point, the contract file, the console and the extension's README.
- A real `pg_rewind` or base backup; `Manager.Remove` of a real data directory (the standby tests use a fake `pg_ctl`).
- Mixed-version clusters on real processes; two kingresd on one data directory (ST-11); load tests of the lease.
- The timing of the 250 ms watchdog poll against the margin on a loaded machine.
