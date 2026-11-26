<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
# kingresd failover, increments 0 to 2: independent review

## Scope and method

Reviewed: the commit that adds increments 0, 1 and 2 of ha-phase5-part2.md and the fixes ST-01 to ST-04, exported with `git archive` into a scratch
directory; nothing in the repository was changed except this file. Method: reading of the lease, standby, instance, cluster, task, ops, sysdb
and watchdog code against section 2 of the design; reproduction in unit tests written in a scratch copy (named below), a real watchdog binary
(new and old) with a fake `pg_ctl` in WSL, and cluster tests started through the shared integration script.

This review is INCOMPLETE (see "Not covered"). The section "Found while building" named in the request does not exist in the design file of that
commit; the departures are in the README of the control plane, which I used instead.

## Summary

| Severity | Count |
|---|---|
| critical | 0 |
| high | 5 (FO-01 to FO-05) |
| medium | 7 (FO-06 to FO-12) |
| low | 4 (FO-13 to FO-16) |
| note | 0 |

In the code of this commit alone I found no schedule in which two primaries of one group accept writes beyond the lease margin (failover does not
exist yet). FO-01, FO-02, FO-04 and FO-06 are where increment 3 would create one or lose data. A healthy primary can still be stopped by a watchdog
started before the upgrade (FO-05).

## Fate of ST-01 to ST-04

- ST-01: the current watchdog no longer stops a healthy primary of a group that stops being enforced (real binary, fake pg_ctl: a stale `off` file
  is not fenced after 8 s; a stale normal file is fenced after 1 s). Not fully fixed: FO-01 (no expiry of `off`), FO-05 (old watchdogs).
- ST-02: held by reading (an observation error sets `running = true`, bounded by `maxCallWait`). Mutant not run.
- ST-03: held by reading (the rebuild no-op needs the meta file, which createLocked writes last, so a half-built copy never matches). The mutant that
  repeats a client timeout is killed by `TestAClientTimeoutIsNotRepeatedButAFailedConnectionIs`.
- ST-04: held for the lease manager's own start (mutant removing the post-start check is killed by
  `TestAFenceDoesNotWaitForAStartAndTheStartDoesNotSurviveIt`); other starters rely on the watchdog (FO-14).

## Increment 0: standbys in the catalog

Held: the removal order (plan, instance, remove_node with retry on KS009, record) and each interruption is repeated idempotently by the next check
(a missing row and `ErrNoRows` return nil); `42710` is read as success only when host, port, group and role standby match; a row of another role or
group is refused. Mutant "KS009 not retried" is killed. Weak: FO-08 (read-back), FO-15 (names that stay), FO-16 (instance removed before the primary
applied the list).

## Increment 1: the instance mapping

Findings FO-03, FO-04 and FO-09. Held: markers per group; the legacy marker migration writes the per-group files before removing the old one and is
idempotent. The role file is written atomically (`pki.WriteFile`).

## ST-01 in particular

The `off` file is written on every successful renewal round of an unenforced group and disarms the watchdog until a normal file arrives. Enforcement
turns on at a primary only after its kingresd received the plan (createStandby publishes with `must` to the primary's server before the copy), so
within this commit I found no schedule giving two writers; the guarantee rests on that ordering and not on the watchdog (FO-01). A healthy primary
can still be stopped by an old watchdog (FO-05).

## Increment 2: the lease transfer and the rest

Held: grantor `Transfer` only from the holder of record, persisted before the answer, refused on a persist error, survives a restart (unit tests and
reading); a delayed renewal of the old holder after a transfer is refused (holder changed). Read calls: the query string becomes the body only for a
GET with an empty body; no smuggling found; reads are for members only. Freeze and gone are leader-only. Findings FO-02, FO-05 to FO-07, FO-10, FO-11.

## The lease under load (the two tests)

Idle (other users gave a load average of 6 to 7), PostgreSQL 18.7, count 2 each: both tests passed 2 of 2 (51 s and 40 s; 120 s and 140 s). Under my
burner (48 spinning loops at normal priority beside the other users, 24 cores) both passed (59 s and 155 s). A second burner of 120 loops was started
with a log copier; after 8 minutes that run had still not finished (the tests were far slower than idle) and I stopped my burner; I did not read any leader or grantor log. I could not reproduce the author's failures. Verdict: NOT
SETTLED. By reading, a lease is lost when no round that started within the last 7 s (10 s duration minus 3 s margin) succeeded, so a stall of 5 to 7 s
of the leader's kingresd or of two grantors loses it by design (designed behaviour); a test of fencing under load is missing (the tests only assert
that nothing is lost).

## The tests

Mutants in a scratch copy (killed = a test failed): cancel guard removed KILLED; broken role file read as default KILLED; client timeout repeated
KILLED; KS009 not retried KILLED; adopted unattached lease fences KILLED; gate ignores relinquished marker KILLED; post-start fence check removed
KILLED. Not judged, because my own failing test files were in the same package: transfer accepts non-holder, transfer not persisted, marker migration
order. Not run: freeze never lapses, gone ignores freeze, `off` never re-arms, trigger conditions removed, ST-02, read-back after add_node. Gaps: nothing
tests the unknown-group gate (FO-03), the repeated transfer (FO-06), the frozen list with no planned name (FO-02), a role file in the middle of a write.

## Findings

### FO-01 (high): the watchdog `off` state has no expiry
Steps: group not enforced, kingresd writes `{"off":true}`; kingresd then dies or hangs. The watchdog (cmd/kingres-watchdog/main.go, `if !off && ...`) never
fences; the same holds after a restart of kingresd until its first round. Real binary, fake pg_ctl: normal stale file fenced in 1 s; off file not fenced in
8 s. Expected: the watchdog exists for a dead or hung kingresd. Direction: give `off` a validity refreshed by every renewal pass whether or not the lease
is held; make increment 3 refuse a failover of a group whose primary's file says off.

### FO-02 (high): a frozen list is emptied when no name is planned
`internal/standby/syncnames.go`, `Decide`: `if len(in.Planned)==0 { if Applied != "" { clear } }` comes before the Frozen check. Planned is empty when the
only standby is not ready (rebuild), has a lost slot, or was dropped by the plan. Test `TestR36FrozenListIsEmptiedWhenNoNameIsPlanned` (scratch copy)
fails: the list is emptied at once. Expected: never emptied while frozen (design I2). Direction: test Frozen first.

### FO-03 (high): the start gate ignores migrated markers when the role file has no group
`lease.Gate` is asked with the role file's group (`GateInfo`), markers are per catalog group. A directory without a role file (every cluster made by the earlier
version) has group -1 until the first lease round, but `PrepareStart` has already migrated a legacy `fenced` marker to `fenced.<g>` and removed the legacy
file, and `Resume` starts the instance before the lease manager runs. Test `TestR36GateOfAnInstanceWithoutAGroupIgnoresMigratedMarkers` fails: a fenced
primary starts without a lease (up to one round). The existing marker test uses group = directory id. Direction: for an unknown group hold back for a marker
of any group, or run SyncRoles before the start.

### FO-04 (high, increment 3 precondition): the standby agent deletes any directory it takes for a standby
`removeLocked` (internal/standby/agent_ops.go) and `Manager.Remove` (internal/instance/trust.go) delete a managed directory without checking role or recovery.
`superviseStandbys` removes every directory that is not writable by role file (an absent or unreadable file defaults to standby for ids from 100), carries a
standby meta file and is not in the plan; the leader's remove call does the same for a record. A promoted standby whose role file is not yet written (crash
between `pg_promote` and the file) or is broken is deleted with its data. By reading. Direction: refuse unless the instance is in recovery or has
`standby.signal`; write the role file before `pg_promote`.

### FO-05 (high): old watchdogs fence on `off`
A watchdog started by the earlier version survives an upgrade of kingresd, ignores `off`, reads `validMs` 0 and fences at once. Real old binary: fenced in the
first poll. The old lock name `watchdog.lock` differs from the new `watchdog.<id>.lock`, so two watchdogs then run. Direction: write a small validMs with
`off` for one release, or tell old watchdogs apart by a new file name.

### FO-06 (medium): Transfer repeats and mixed versions
A repeated Transfer is refused at every grantor ("not the holder", Holder = `to`) and `Manager.Transfer` counts only `Granted`: test
`TestR36RepeatedTransferReportsFailureAfterItSucceeded` fails. An old grantor treats `transferTo` and `release` as a plain grant for the caller (unknown JSON
fields; the internal version is still "1"): test `TestR36TransferToOldGrantorsIsReportedAsDone` shows Transfer reporting 2 of 2 with no remote grantor moved.
Direction: count `Holder == to` as done; require it on a transfer answer; bump the internal version or probe `marginMs` in `/lease/state` first.

### FO-07 (medium): Transfer and ReleaseGroup do not check that no primary runs here
They delete the group's state, so `Check` never fences; the primary runs until the watchdog (7 s) while `to` holds a fresh lease. Test
`TestR36TransferWithARunningPrimaryLeavesItRunning`: still running after 20 s, no marker, no stop. Direction: refuse when the group's instance runs.

### FO-08 (medium): the read-back reads the coordinator only
`catalog.go` reads `kingres.node` on the session that made the call, but the extension review (S12-01) asks for a read on the touched node; the
coordinator's row is committed first, so the check cannot detect the defect it was added for. `Unregister` compares host only, not port.

### FO-09 (medium): a broken role file is read as the id's default
`MustRole` falls back to `DefaultRole`, against the comment of `RoleOf`: a promoted primary in a directory from 100 becomes a standby (no lease, no
watchdog, `Fence` returns nil, FO-04); a demoted home instance becomes a primary (fan-outs write to a standby). The existing test asserts the first. Two
directories claiming one group are not detected (`PrimaryOf` returns the lower id; the other has no lease or watchdog).

### FO-10 (medium): the relinquished marker of Transfer is never cleared on the fence holder
Test `TestR36MarkerOfATransferBlocksALaterAdoption`: after a Transfer, `Adopt` of the same group is refused for ever; the marker survives restarts, removes the
group from renewal even if the catalog names this server primary, and is invisible (no alert, not in `Info`). No production code calls `Unrelinquish`.

### FO-11 (medium): cancel guard and trigger fail open
`Engine.Cancel` skips the guard when `Get` fails, and the guard and `RequestCancel` are two steps (the run must ignore the mark past the point of no
return). KX004 is fail-open when the leader is unreachable (`openTask` keeps the task in memory), keys on the server's first instance only while stop
and restart act on every primary (`primaries` wrapper in ops/instancectl.go), and never applies to `config.restart` (no instance subject). By reading.

### FO-12 (medium): freeze and gone are memory only
A restart of kingresd on the primary's server forgets the freeze; the list can then be emptied after the grace (30 s). Increment 3 must resend it and
verify with `/failover/state`.

### FO-13 (low): `Release` can be undone by a renewal in flight
A renewal sent before `ReleaseGroup` and received after it re-grants the group to the releaser at that grantor (liveness only). By reading.

### FO-14 (low): fence versus a start by another caller
The post-start check exists only in `startPrimary`; an operator start or restart that passed the gate just before a fence, while `Running` is still false,
leaves the instance up; only the watchdog covers it. By reading.

### FO-15 (low): a listed name stays although the standby is never registered
The upgrade path has no alert; increment 3 must refuse a failover while a named standby is not in the catalog.

### FO-16 (low): smaller items
Removal deletes the instance before the primary applied the list without it (bounded by the 30 s grace); `StartInstance` stops at the first gated primary so the
other primaries are not started; a watchdog runner is replaced when the group of an instance changes while the old process keeps its old marker path.

## For increment 3

- Write the role file before `pg_promote`; guard every standby deletion by recovery state.
- Call `DisarmWatchdog` when an instance is demoted but kept running (no production caller exists).
- Transfer only after the stop is confirmed; treat `Holder == to` as done; probe versions first.
- Between `pg_promote` and the catalog flip a role-file primary has no lease, watchdog or fence unless `Attach` is called.
- Resend the freeze and read `/failover/state`; `ReceiveLsn` is empty when no walreceiver ran: use the maximum with `ReplayLsn`; a 404 `not_found` is an
  old server, `no_instance` is a real answer.
- After a map group promotion call `MigrateSystem` (it skips a server in recovery and runs only at start).
- The run must ignore a cancel mark past the point of no return.

## Not reproduced

The author's load failures; the deletion of a promoted directory (FO-04); mixed versions on real processes; two directories of one group.

## Not covered

Cluster-level interleavings of start, stop, restart and fence; the proxy changes; peer authentication following origin; the extension README line by
line; the ST-02 test and the trigger by mutant; most mutants named in the request; the whole-module run on Windows; PostgreSQL 19.

## Baseline numbers

Cluster tests on 18.7: 2 of 2 and 2 of 2 idle; 1 of 1 and 1 of 1 under 48 loops. Scratch unit tests of mine: 6 failing (each a finding). 7 mutants
killed. The Windows module run, 19rc2 and the full integration suite were not run.
