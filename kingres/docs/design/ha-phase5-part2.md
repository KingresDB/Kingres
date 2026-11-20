<!-- SPDX-License-Identifier: AGPL-3.0-only -->
# kingresd: design of high availability, part 2 (failover, switchover, the old primary, preferred primaries)

Status: Proposed design, 2026-11-16; not implemented. Where it says "exists", it describes the working
tree and names the file; where it could not be checked, it says so and the item is in the list "To be
verified" at the end.

Builds on [ha-phase5.md](ha-phase5.md) (Part B is the outline this document turns into a design),
[ADR 0018](../adr/0018-manager-leader-follows-the-map-group-primary.md) (the manager leader is the server
of the map group primary), [ADR 0019](../adr/0019-cluster-state-in-a-system-database.md) (cluster state in
the system database on the map group), [ADR 0021](../adr/0021-failover-in-kingresd.md) (failover is part
of `kingresd`, with leases; no Patroni, no etcd), the sharding extension 0.5 and its chapter "What the
control plane calls" (`contrib/kingres_shard/README.md`, nine call sequences), the control plane README
("Sixth increment: high availability, part 1", "The fencing rule of the task engine", "Rebalancing") and
the contract 0.9.0 (`kingres/api/openapi.yaml`).

The words of this document, once, so that they are used the same way below. They are the extension's:
**MAP** is the primary of the map group (shard group 0), **OLD** the primary of the group that fails,
**NEW** the standby that is promoted, **OTHERS** the other standbys of that group. A **group** is a shard
group; a **holder** is the server whose `kingresd` renews the lease of a group; the **leader** is the
manager leader. A **step** is a step of the task engine; a **call** is an internal API call between
`kingresd` processes (`kingres/control-plane/internal-api.md`) or one SQL statement of the extension.

## Sources read

| Source | What it settled |
|---|---|
| `kingres/docs/design/ha-phase5.md`, ADR 0018, 0019, 0021 | The outline of failover, the rules of automatic failover, why the lease and not a store |
| `contrib/kingres_shard/README.md` (working tree), chapter "High availability (version 0.5, phase 5)", "What the control plane calls" (sequences 1 to 9, bounds, preconditions, postconditions, answer tables), the function table of "SQL interface", `kingres_shard--0.4--0.5.sql` (`standby_status` columns) | Every call and answer used below |
| `kingres/control-plane/README.md` (heartbeats, leases, standbys, client entry point, decisions, "What part 2 must add", "The fencing rule of the task engine", "Task engine: crash points", "Rebalancing"), `internal-api.md` | The facts of part 1 |
| `internal/lease` (`grantor.go`, `manager.go`, `startwait.go`), `internal/ha`, `internal/liveness`, `internal/standby` (`agent.go`, `agent_primary.go`, `agent_ops.go`, `syncnames.go`, `wire.go`, `catalog.go`, `store.go`, `call.go`), `internal/task` (`task.go`, `record.go`), `internal/rebalance` (`service.go`), `internal/leader`, `internal/instance` (`standby.go`, `manager.go`), `internal/internalapi`, `internal/clientproxy`, `internal/fault`, `internal/clustertest`, `cmd/kingresd/ha_*.go`, `cmd/kingresd/leadermemory.go`, the migrations 0001 to 0010 | Real type and function names; what the code assumes |
| `kingres/api/openapi.yaml` 0.9.0 and `kingres/api/README.md` | What the contract has, how it is versioned |
| `kingres/docs/reviews/2026-11-16-kingres-shard-0.5-fixes.md`, `...-0.5-sequences.md`, `...-kingresd-rebalance.md`, `...-kingresd-rebalance-fixes.md` | What a control plane must do and must never do (S8-nn, S9-nn, RB-nn, RBR-nn) |
| `kingres/web` (`ServerActions.vue`, `ServerDetailPage.vue`, `ServersPage.vue`, `ReplicationPage.vue`, `RebalancePage.vue`, `RebalanceReportDialog.vue`, `GroupLeaseInfo.vue`, `GlobalBanners.vue`, `SettingsPage.vue`, `TaskProgress.vue`, `composables/useTask.ts`, `mocks/`, README) | How a long operation with steps is shown today |
| `shard-phase10.md`, `ledger-phase7.md` | Tone and structure of a design document of this directory |

## 1. Scope and what exists

### 1.1 What part 2 adds

1. **Failover on request** (`failoverServer`, mode `hard` or `graceful`): the task `server.failover`
   promotes a standby for every group whose primary runs on a server, one group at a time, map group first.
2. **Automatic failover**: the leader (and, for the map group, the follower that holds the best standby)
   starts the same task for a server whose heartbeats stopped, under the rules of `ha-phase5.md`.
3. **Planned switchover**: a healthy primary hands its group to a standby with no loss (mode `graceful`),
   and **restoring preferred primaries** (`restorePreferredPrimaries`) is the same, in the other direction,
   for every group that is not on its home server.
4. **The old primary comes back as a standby**: by itself when it returns after a failover (it is fenced and
   must not start as a primary), rebuilt with `pg_rewind` or a new base backup.
5. **The pages of the console** for all of this, and **the contract 0.10.0** that carries them.
6. The groundwork part 1 did not have: standbys registered in the sharding catalog (the extension's
   sequence 1), the instance of a group's primary no longer assumed to be instance 1 on its server, a
   handover of a lease from one holder to another without a free moment, a freeze of a group, and the
   gate that keeps the task engine of a new leader from running before a map group promotion is complete.

### 1.2 What part 2 does not do

- No Patroni, no etcd, no external consensus store (ADR 0021). The authority to promote is a majority
  lease (part 1) plus, for the map group, the extension's majority rule.
- No change of the extension. Everything below is written against kingres_shard 0.5 as the README of the
  working tree describes it.
- No change of the lease protocol's numbers or of its safety argument (README "Why this is safe"); it gets
  two additions (section 3.1.1: `transferTo`, `release`) that only make a holder hand a lease over earlier,
  and a per-group fence marker (section 3.7).
- No automatic failback: a group goes back to its preferred server only on request.
- No failover of a group without a standby (nothing to promote), no automatic failover of a cluster with
  fewer than three servers (no majority; `ha-phase5.md` says the same).
- No forced failover without a majority in this part (section 4.7 and open point 11.4): the operator's way
  out stays the documented SQL of the extension.
- No parallel failover of several groups: they are done one at a time (section 3.3), which costs seconds per
  group and keeps `promote_node` serialized.

### 1.3 Facts of part 1 that part 2 builds on

| Fact | Where it is |
|---|---|
| Heartbeats all to all, every `heartbeat-interval` (1 s); `suspect` after 5 s, `dead` after 10 s of silence, on the monotonic clock of the receiver; a stalled process gives everybody a fresh window | `internal/liveness`, `ha.Liveness` (`State`, `Silence`, `LastHeard`), README "Heartbeats and liveness" |
| A lease per group, granted to a server by a majority of the distinct hosts of `kingres.node`; the grantor refuses another holder until `receipt + duration + margin`, persists a change of holder in `<data-dir>/lease/grants.json` before it answers, and after a restart assumes every grant was made just now | `internal/lease/grantor.go` (`Grantor.Grant`, `NewGrantor`) |
| `POST /lease/grant` takes `{group, durationMs, dryRun}` and answers `{granted, holder, waitMs}`; `GET /lease/state` lists grants and leases | `cmd/kingresd/ha_leases.go`, `lease.Request`, `lease.Reply`, `internal-api.md` |
| The holder renews every 2 s; a round asks every member; the lease is valid from the round's start for 10 s; the holder acts on it until 7 s after the round start (`Holds`); a check every 100 ms fences (fence marker, `pg_ctl stop -m immediate`) when it has passed; a group without a standby is not fenced, only alerted | `internal/lease/manager.go` (`Manager.round`, `Check`, `lose`, `Fence`, `Holds`) |
| The fence marker `<data-dir>/lease/fenced` and the wait marker `<data-dir>/lease/waitlease` keep the primary from starting without a lease; the start gate is `lease.Gate` | `internal/lease/manager.go`, `startwait.go`, `instance.Manager.SetStartGate` |
| The watchdog `kingres-watchdog` stops the instance when `instances/1/lease.json` has not changed for `validMs`; it writes the same marker | `cmd/kingresd/ha_leases.go` (`watchdogRunner`), `internal/lease/watchdogfile.go` |
| The lease topology (which groups this server runs the primary of, and the members) is read from the local catalog, and cached in `lease/topology.json` | `catalogTopology.Topology` in `cmd/kingresd/ha_leases.go` |
| Standbys: records in `kingres_cp.standby` (`standby_id`, `group_id`, `server_id`, `primary_server`, `instance_id`, `port`, `state`, `needs_rebuild`, `generation`, `catalog_node_id`), the whole list pushed to every server (`POST /standby/plan`), slots `kingres_standby_<id>`, `application_name` equal to the slot name, `ANY 1 (all ready standbys)` once one streams | `internal/standby` (`store.go`, `wire.go`, `leader.go`, `agent_primary.go`, `syncnames.go`), migration 0006 |
| `Agent.Frozen(group)` exists and `SyncState.Decide` obeys it (the list is never emptied while it says true), but nothing sets it | `internal/standby/agent.go`, `syncnames.go`, `agent_primary.go` |
| `POST /standby/rebuild` takes `{entry, diverged}` and tries `pg_rewind` first when `diverged`; nothing sets `diverged` | `internal/standby/agent_ops.go` (`Rebuild`, `rewindLocked`), `instance.Manager.Rewind` |
| The catalog registration of standbys is a seam: `CatalogRegistrar` with the implementation `SkipCatalog`; the step `standby.register_in_catalog` ends `skipped` | `internal/standby/catalog.go`, `cmd/kingresd/ha_standby.go` |
| The client entry point chooses the local primary only if the catalog calls it active and `Leases.Holds(group)`; otherwise another server's entry point; open connections are closed when the destination changes | `internal/clientproxy/destination.go`, `cmd/kingresd/ha_proxy.go` |
| The task engine: steps recorded `running` then `done`, at-least-once, `NonIdempotent`, `Skip`, `Pause`, lease of 30 s with an epoch, a lost lease ends a run quietly and runs no hook, remote calls carry `X-Kingres-Fence: <task>:<epoch>` and the callee refuses a lower epoch | `internal/task/task.go`, `internal/fence`, README "The fencing rule of the task engine", "Carry-overs" |
| `Record`, `Settle` and `ReserveID` write tasks the engine does not run (instance start and stop), with the internal calls `POST /task/record`, `/task/settle`, `/task/reserve` | `internal/task/record.go`, `internal-api.md` |
| Migration 0009 refuses, under one advisory lock, a rebalance while a task of kind `server.add`, `server.remove`, `server.leave` or `server.failover` is unfinished, and the other way round (SQLSTATE `KX001`, `KX002`); a paused rebalance does not exclude | `internal/sysdb/migrations/0009_task_exclusion.sql`, `kingres_cp.lock_operations()` |
| A rebalance is stopped by `rebalance.Service.Stop` (flag, `cancel_bucket_move`, the task becomes `paused`, `startRebalance` resumes it) | `internal/rebalance/service.go`, README "Rebalancing" |
| Cluster settings `autoFailover.enabled`, `timeoutSeconds`, `maxCount` are keys of `kingres_cp.cluster_setting`; `eventsSinceReset` is in the contract and in `api.AutoFailoverSettings`, but nothing in `kingresd` writes or counts it | `internal/store/pgstore/databases.go`, `internal/ops/settings.go`, grep for `eventsSinceReset` |
| `preferredPrimaryServerId` of a group is the server of the first node of the group in the catalog (lowest node id) | `internal/store/pgstore/topology.go` (`buildGroup`) |
| `kingresd` creates every instance with `--data-checksums`, so `pg_rewind` is possible | `internal/instance/manager.go` (`Init` arguments) |

### 1.4 Facts of the extension that part 2 builds on

Each is in `contrib/kingres_shard/README.md`; the number of the guarantee or of the sequence is given.

| Fact | Where |
|---|---|
| `kingres.add_node(host, port, group_id, 'standby')`, `remove_node`, `standby_status()` (columns `node_id, group_id, host, port, primary_node_id, replication_state, sync_state, replay_lsn, replay_lag_bytes, problem, detail`), `standby_name_matches(name)` on the primary of the group | sequence 1, 8 |
| `promote_node(node_id)` answers `{"status": promoted \| completed \| already, "term", "final", "unreached", "blocked_by"}`; the caller repeats until `already` with `final` true; it refuses unless NEW is out of recovery, runs the same library and can commit; OLD is not waited for when it can be reached | guarantee 7, sequence 2, 9 |
| The catalog term is raised by every `promote_node`; a node refuses a catalog push or a forwarded statement of an older term with `KS002`; a map primary that is superseded refuses every catalog change | "The catalog term" |
| The map group is promoted on NEW itself; NEW must reach the primary of a strict majority of the groups other than the map group; until it has been accepted NEW changes no catalog; `promote_map_group_without_majority(node_id, acknowledgement)` is the operator's way out | guarantee 9, sequence 3 |
| `switchover_mark()` on OLD returns M; `switchover_replayed(M)` on NEW says that NEW has replayed up to M and nothing more; the proof is `switchover_verify(X, state)` on NEW, X being "Latest checkpoint location" and state "Database cluster state" of `pg_controldata` of OLD's data directory, read after OLD stopped: `t` proven, `f` not replayed yet, error `KS006` never provable (the state is not `shut down`, or X is not OLD's shutdown checkpoint as the last record in NEW's WAL) | sequence 5 |
| The refusals a caller must tell apart carry SQLSTATEs: `KS006` switchover not provable, `KS007` a node did not answer within `node_timeout` (do not repeat against that node), `KS008` another catalog call is running on the node (repeat later), `KS009` the node cannot commit, `KS010` wrong node, `KS011` still in recovery, `KS012` promotion not safe (an operator's decision), `KS013` transactions in doubt; `kingresd` decides by the SQLSTATE, never by the text | "SQLSTATEs of the answers" |
| While a catalog call runs, every statement to a node waits at most `node_timeout`; a call ends within `2 x node_timeout + 2` s for each node that does not answer (`promote_node` and `check_cluster`: see the README table); a lock held on a node answers `KS007` or `55P03`, both naming the node | "Bounds that apply to every call" |
| `recover_prepared_transactions()` on NEW and MAP; `check_cluster()` rows `standby_unreachable`, `standby_not_streaming`, `standby_not_in_recovery`, `prepared_rolled_back_after_failover`, `participant_lost_committed_transaction`, `decision_unconfirmed`, `foreign_prepared_transaction`; incidents are kept 30 days | "Detecting damage afterwards", guarantee 11, 12 |
| The extension does not fence and never edits `synchronous_standby_names`; a promoted standby inherits its primary's list and every commit on it then waits for ever | precondition 1 and 3 |
| Bounds: `promote_node` client timeout at least `3 x node_timeout + node_connect_timeout x nodes`; repeat loop at most 10 calls 2 s apart; a refused call at once; a commit that waits for a standby `2 x node_timeout + 5` s then `KS005` | "Bounds that apply to every call" |

### 1.5 What does not exist (the work of part 2, found by reading)

- Nothing promotes a standby or calls `promote_node`; `failoverServer` and `restorePreferredPrimaries`
  answer `unsupported_operation`.
- `kingres.node` has no standby rows, because `SkipCatalog` is the registrar. `promote_node(node_id)` needs
  one. Part 2 begins with sequence 1 (section 10, increment 0).
- Part 1 assumes that the primary of a group is instance 1 of its server (section 3.7 lists the places).
- Nothing grants a lease to a server whose group is not yet in its catalog; `lease.Manager` renews only the
  groups its catalog row names. Nothing makes a live holder stop renewing; the grantor waits for expiry.
- `Agent.Frozen` is never set, and nothing stops the leader's reconcile loop from touching a group that is
  being failed over.
- A `server.failover` task cannot exist while the map group has no primary, because tasks live in the
  system database on the map group (ADR 0019); and the engine of a server whose instance has just been
  promoted must not claim tasks before the promotion is complete.
- The CA key and the revocation list live on the first server only (README "Not done in this increment, and
  the manager's state on a leader change"): a new leader cannot issue certificates.
- Nothing counts automatic failovers or resets the counter.

## 2. The safety argument

The mechanism is the one of ADR 0021 and `ha-phase5.md`, stated in the order of events. Every statement
below is either a property of code that exists (named) or a requirement on the code of part 2 (marked
"part 2").

### 2.1 The invariants

| # | Invariant | What guarantees it | Established before which step relies on it |
|---|---|---|---|
| I1 | At most one writable primary per shard group at any time that a client can reach through the product | The lease: a group's primary acts only while its holder holds a majority lease (`Holds`), the holder fences itself (`Check`, `Fence`) and the watchdog covers a dead or hung `kingresd`; a new holder is granted only after the old one's lease expired plus the margin at a majority of grantors. Part 2 adds: `pg_promote` is issued only while NEW's server `Holds` the group lease, and every destructive call carries the expected catalog term | The lease of the group is held by the new holder (section 3.3 steps F3 and F5) before `pg_promote` (F6) |
| I2 | No acknowledged commit is lost when the databases of the group use durability `sync` or `syncApply` | A synchronous standby streaming at the failure (`ANY 1 (all ready standbys)`, `kingres_cp.standby`, `SyncState.Decide`), NEW chosen by the highest flushed WAL position of **all** ready standbys read after OLD was fenced (extension precondition 2: with `ANY k (n names)` at least `n - k + 1` standbys must be read; k is 1 here, so all), the extension's guarantee 11 for distributed transactions | The positions are read (F4) after the group lease is held by a fence holder (F3), so OLD cannot write any more; NEW is pinned (F4) before it is promoted (F6) |
| I3 | A superseded manager leader, or a holder of a task that lost its lease, does nothing more in the name of the task | The task engine's rule (README "The fencing rule of the task engine"): conditional records, no hook after a lost lease, `X-Kingres-Fence` on calls. Part 2 adds: the failover tables are written only by transactions that prove holder and epoch (as `internal/rebalance` does), and the one irreversible call, `pg_promote`, is guarded where it runs (lease held, expected term) so that a late call of a superseded run cannot do harm | Every step; in particular F6 |
| I4 | The map group's primary and the manager leader are the same server (ADR 0018) | The leader is read from the catalog (`internal/leader`, `catalogTopology`); `promote_node` for the map group changes the catalog's map row on NEW and pushes it; the task engine works on the task table of the system database, which only the instance of the map group's primary holds writable. Part 2 adds the **engine gate**: a server that was promoted in PostgreSQL as the map group's standby runs no task until its bootstrap has finished (section 3.4) | After `promote_node` answered `already` on NEW and before the gate opens |
| I5 | A group is promoted once per decision: two standbys of one group are never both promoted | The group lease has one holder, the task pins its choice in a conditional record before the lease is handed to the chosen server (F4 then F5), a resumed run never chooses again once a choice is pinned, and a second instance of the group found out of recovery stops the task with the critical alert `splitPromotion` | F4 before F5 |
| I6 | An old primary does not return as a writable primary | The per-group fence marker and the relinquish marker (section 3.7) stay until the instance is a standby of the new primary; the lease manager does not start a primary without a lease; the extension tells a demoted node to forward and refuse (guarantee 5, 6; it does not stop ordinary writes: the fence does) | After F2 or after the lease passed (F3) |

### 2.2 Clock assumptions and bounds, from the code

All judgements use the monotonic clock of the machine (`internal/clock`); only durations travel. A step of
the system clock changes nothing (`TestWallClockStepChangesNothingForLeases`). The settings and their
defaults (`internal/config/ha.go`, README "Settings (flags, environment, file keys)"):

| Name | Default | Meaning |
|---|---|---|
| `lease-duration` D | 10 s | validity of a lease from the start of the round that obtained it |
| `lease-renew` | 2 s | a round every 2 s (at most half of D minus the margin) |
| `lease-margin` M | 3 s | the grantor waits it after expiry; the holder fences that long before expiry |
| round time-out | D - M = 7 s | `context.WithTimeout(ctx, Duration-Margin)` in `Manager.round` |
| holder acts until | round start + D - M = 7 s | `Holds` |
| check interval | 100 ms | `min(100ms, Renew/4)` in `Manager.Run` |
| watchdog poll | 250 ms | README "Watchdog" |
| grantor refuses other holders until | receipt + D + M = 13 s | `Grantor.Grant` |
| grantor after a restart | refuses other holders for D + M = 13 s | `NewGrantor` |
| `heartbeat-interval`, `suspect-timeout`, `failure-timeout` | 1 s, 5 s, 10 s | liveness |
| `leader-loss-bound` | 30 s | a follower trusts its cached roles this long |
| task lease | 30 s, renewed every third | `task.Options.Lease` |
| `standby-sync-grace` | 30 s | no standby streaming this long: the list is emptied (not while frozen) |

The argument (README "Why this is safe", repeated because part 2 stands on it): let `s` be the start of the
last successful round of OLD's holder, `r >= s` the instant a grantor of the majority received its request.
OLD's holder stops acting at `s + D - M` and has stopped the instance moments after (immediate shutdown;
the 100 ms check and the stop must fit in the margin). The grantor refuses every other holder until
`r + D + M`. A new holder needs a majority, and two majorities share a server, so no new holder can have
the lease before `r + D + M`. Between the last act of OLD and the first act of the new holder lie at least
`2M = 6 s` minus the difference in the speed of the two clocks over D and the scheduling delay of the check.
That is the assumption: **the margin is larger than the clock-rate error over D plus the scheduling delay of
the check and of the stop**, on every server of the cluster, and `lease-margin` and `lease-duration` are
equal on every server. (Today they are per-server settings; part 2 makes the grantor report its margin in
`GET /lease/state`, so that the leader refuses a failover when two servers disagree, section 3.3 step F0.)

The new code adds one more assumption, of the same kind: **between the moment `kingresd` checks `Holds`
and the moment `pg_promote` is issued fewer than `M` seconds pass.** Part 2 writes both in one handler,
with nothing slow between them (section 3.3 step F6). If the process is paused between the two for longer,
the lease has expired at that holder (its own monotonic clock keeps running), and the check is made again
by NEW's own `Check` after the promotion: a promoted instance whose lease is not valid is fenced like any
primary (`Manager.lose`).

### 2.3 The order of events, for one group whose primary's server died

`T0` is the last instant OLD's holder obtained a majority grant (the start of its last successful round).
Nothing below needs `T0` to be known; the line says what holds at each point.

| Time | Event | What is true from here |
|---|---|---|
| `T0` | OLD's server gets its last majority grant | OLD may act until `T0 + 7 s` |
| `T0 + x` | OLD's server dies (or is cut off) | its watchdog or its `Check` stops the instance at the latest at `T0 + 7 s` |
| `T0 + 10 s + x` | the leader's tracker calls the server `dead` | (liveness only; no authority) |
| `Td` | the failover starts: operator request, or `autoFailover.timeoutSeconds` of silence (section 4) | the group is frozen (F1) |
| `Td + w` | the fence holder F (the leader's server) obtained the group lease at a majority (F3) | OLD cannot act any more and can never again obtain a majority; `w` is zero when OLD died more than 13 s ago, else at most `13 s - (Td - T0)` |
| after F3 | positions are read (F4), the choice is pinned | OLD wrote nothing after `T0 + 7 s`; the positions are stable |
| after F4 | F hands the lease to NEW's server with `transferTo` (F5) | NEW's server holds and renews the lease; no free moment exists in which OLD could be granted again |
| after F5 | `pg_promote` on NEW (F6), only if `Holds` | NEW is writable; the catalog still says OLD is the primary: nobody routes to NEW |
| after F6 | `promote_node` answers `already` (F7) | every node that was reached has the new catalog and the new term; clients are routed to NEW; the client entry point of NEW's server accepts connections |
| after F7 | OTHERS re-pointed (F8), the list set (F10), prepared transactions resolved (F11) | the group is healthy again (the extension's definition, "Postconditions") |
| later | OLD's server returns | it is fenced; the leader rebuilds it as a standby (section 6) |

### 2.4 Why each invariant holds, and how it could fail

**I1.** OLD cannot be a client-reachable writer after `T0 + 7 s` if its holder acts on the lease as part 1
does. It can be one earlier only because it still holds the lease, in which case no new holder can be
granted (the grantors refuse; F3 never succeeds; the task stops with `lease_still_held`). A partition
between the leader and OLD's server while clients still reach OLD therefore changes nothing: if OLD's
holder reaches a majority it keeps the lease and the failover waits; if it does not, it fences itself.
How it could fail: a clock-rate error or a scheduling delay larger than M; a machine that is frozen as a
whole and resumed with processes in the middle of a write that was already acknowledged to the kernel
(README "Limits"); a bug in the new code that issues `pg_promote` without the lease. The tests of
section 9 attack exactly these.

**I2.** With `ANY 1 (a, b)`, a commit acknowledged to a client exists on at least one of `a`, `b`. NEW is
the one with the highest flushed position among *all* ready standbys, read after OLD stopped writing, so it
holds every acknowledged commit (extension precondition 2). If one of the ready standbys cannot be read,
the argument fails for it ("a standby that you cannot read must be assumed to hold the commits that the
others lack"): the failover then needs a decision (section 2.6). Distributed transactions are covered by the
extension (guarantee 11) provided the participants' groups also have a synchronous standby.

**I3.** See section 3.9: every record is conditional on holder and epoch; the destructive calls are guarded
where they run. The residue stays what the README says ("Limits that remain"): a process paused between an
atomic `running` mark and the first side effect of a step can still act after its lease ended; for the
destructive calls of part 2 this is closed by the lease check and the expected term at the callee, because
a late `pg_promote` finds the lease held by someone else or the term changed, and a late `demote` finds the
instance is no longer the group's primary.

**I4.** The new leader's own `kingresd` learns it is the leader from its catalog, which `promote_node`
changed on NEW before it answered. Other servers learn from the push, or at the first call of a node that
knows (the term). The leader cache of the internal API (`internal/leader`, 2 s) and the 60 s memory of the
last verified leader (`cmd/kingresd/leadermemory.go`) must be dropped when the catalog's term changes
(part 2, section 3.7). Until the other servers know, leader-only calls from NEW are refused with
`not_leader`, which the bootstrap treats as "not yet" (section 3.4).

**I5 and I6.** Section 3.3 (F4 to F6) and section 6.

### 2.5 How long a failover takes

The only numbers that come from code are the lease numbers. The others are targets that the tests of
section 9 measure and the implementer reports; they are stated so that a reviewer can hold the
implementation to them.

| Part | Best | Worst (bounded) |
|---|---|---|
| Detection | none for a request; `autoFailover.timeoutSeconds` (default 120 s, section 4) for the automatic one | the same |
| Waiting for the old lease (F3) | 0 s: OLD's server died more than 13 s ago, which is always so for an automatic failover (the timeout is at least 30 s), or OLD's server transferred the lease itself (F2) | 13 s from the last grant that a grantor of the majority received, plus the polling step of 1 s; the step's bound is 25 s, then the task stops with `lease_still_held` |
| Reading the positions (F4) | about 2 s (two reads 1 s apart, equal) | 15 s |
| Handing the lease over (F5) | one round trip | 10 s |
| `pg_promote` (F6) | seconds (target: under 5 s) | 60 s (`wait_seconds`), then the task asks `already` again before it fails |
| `promote_node` (F7) | under 2 s (target) | 10 calls and a budget of 15 min; a call itself is bounded by `3 x node_timeout + node_connect_timeout x nodes` (195 s with three nodes and the defaults) |
| Re-pointing, list, prepared transactions (F8 to F11) | about 10 s | 5 min in all; none of them blocks the group (clients are already on NEW after F7) |

So a failover of a dead server's group takes **about 15 to 30 s after the decision, and 0 to 13 s more
when the decision comes less than 13 s after the last grant** (a manual request right after a crash). The
group accepts writes again at the end of F7. A group that has several groups on the server takes that long
for each, map group first. The worst case is the sum of the bounds, about 20 min, dominated by the
`promote_node` budget, and it is reached only when a participant of the promotion is stuck: the task then
pauses with a reason and an alert (section 3.3, F7), it does not wait for ever.

### 2.6 Asynchronous durability and a standby that is behind

What can be lost. A primary acknowledges a commit to a client after its own flush when the database's
durability is `async` (`synchronous_commit = local`), after the flush on a quorum of synchronous standbys
for `sync`, and after their apply for `syncApply` (contract `Durability`; `kingres/api/README.md`, design
note 2). Three situations:

1. **The group ran with a synchronous standby that streamed, and NEW is the highest of all ready
   standbys.** Nothing acknowledged under `sync` or `syncApply` is lost. Commits acknowledged under `async`
   after the standby's last confirmed position may be lost: they are the replication lag at the failure,
   unknown in size.
2. **The group ran without its synchronous standby** (`SyncState.Decide` emptied the list after
   `standby-sync-grace`; the alert `groupWithoutSynchronousStandby` was raised): commits acknowledged under
   any durability after the standby stopped streaming may be lost. That is the trade `ha-phase5.md` states.
3. **A ready standby cannot be read**, or the best standby is not the one requested: commits that only the
   unread or the better standby has may be lost (extension precondition 2).

How the operator is told, and what is recorded:

- `previewFailover` (the `dryRun` of the request, section 7) answers per group before anything happens:
  the chosen standby, its flushed position and lag against the last position of OLD that the leader knows
  (section 3.6), the databases of the group by durability, whether the group ran without its synchronous
  standby, whether a standby could not be read, and the verdict `lossPossible: none | asyncOnly |
  possible | unknown` with the reason in words.
- A request whose verdict is not `none` and that does not say `allowDataLoss: true` is answered 409
  `failover_needs_decision` before a task exists (a request on a running task: the task pauses at F4).
- **Automatic failover** proceeds when the verdict is `none`, or `asyncOnly` and every database that holds
  buckets in the group (plus every non-sharded database, on the map group) has durability `async` (the rule
  of `ha-phase5.md`); otherwise the task pauses at F4 with the alert `failoverNeedsDecision` (critical)
  naming the group and the loss window, and waits for an operator. The contract's wording stays: a standby
  that was not synchronous is promoted automatically only if every database allows it.
- Recorded: `failover_group.decision` (every candidate with its position, state and sync state at the last
  check, the verdict), the audit event with `allowDataLoss`, the report, the alert, and after the promotion
  the extension's own records (`check_cluster()`, `dtx_incident`, section 3.3 F11).

The size of the loss is bounded from below by nothing and from above by what the leader knew: the standby
check (every 10 s, `standby.Leader`) records per group the last flushed WAL position of the primary and the
time (`group_replication_state`, section 3.8), so the report says "at most `X` bytes were written after the
last position the leader saw, and the standby's position was `Y` bytes behind it, at `t` seconds before the
failure". The extension cannot tell which transactions these were, only that a distributed one was half
committed (guarantee 12).

### 2.7 What the argument does not cover

- Two servers can have different `lease-margin` or `lease-duration`: refused at F0, not proven safe.
- A whole machine frozen and resumed, a standby promoted by hand (`pg_promote` on a standby behind the
  product's back: the next check finds two out-of-recovery instances of a group and raises `splitPromotion`;
  the product does not stop it), a PostgreSQL bug.
- The extension's rule for the map group cannot be satisfied by a cluster of two groups or by one that lost
  half or more of the groups with the map group (guarantee 9): the failover then waits (section 3.4) and the
  operator uses the documented SQL.
- An old primary that was reachable by clients and not fenced (the extension's "unreached" case): section 6.

## 3. The failover task

### 3.1 Shape, and the pieces it is built from

**Task.** Kind `server.failover` (contract 0.9.0 already has it), subject `server` / the id of the server
whose groups are failed over (the table "Task kinds and subjects" says so), created by the manager leader
for `failoverServer` and for an automatic failover, `createdBy` the role or `system`. Its steps are dynamic
(`Definition.Dynamic`, as for a task with one step per server): a first step for the whole task, then
**a block of steps per group**, map group first, then ascending group id, then a last step. A group's
steps are named `Group <n>: <what>` and carry the group id in the new `TaskStep.shardGroupId` (section 7).
The input records the request (mode, groups, targets, `allowDataLoss`, trigger) and nothing secret.

**Why one task per server, groups in sequence.** The contract's operation is `POST
/servers/{serverId}/failover`, its task subject is the server, and `promote_node` of two groups must not
run at the same time (the extension promotes "two groups that failed together one after the other"). A
failover of one server is therefore one task. **Decision:** at most one task of the kinds `server.failover`
and `replication.restorePreferredPrimaries` is unfinished at any time, cluster-wide (migration 0011,
SQLSTATE `KX003`, 409 `failover_in_progress`, the same advisory lock as migration 0009). A second server
that fails meanwhile waits: the automatic trigger tries again every 5 s; a manual request is refused with the
code and the id of the running task. Two servers lost together are two events for the counter of section 4.

**The pieces.**

| Piece | What it is | Where |
|---|---|---|
| `failover` package | the task definition, the steps, the choice of the standby, the `promote_node` loop, the report, the bootstrap of the map group | new `internal/failover` |
| the report tables | `failover_run`, `failover_group`, `failover_event`, `group_replication_state` | migration `0011_failover.sql` (section 3.8) |
| group freeze | `Agent.Frozen(group)` answered from a map of frozen groups with a time-to-live | `internal/standby` (`Agent.Freeze`, `Unfreeze`) |
| lease additions | `Manager.Adopt`, `Attach`, `Transfer`, `Relinquish`, `Unrelinquish`; grantor `transferTo` and `release` | `internal/lease` (section 3.1.1) |
| calls between servers | the table of 3.1.2 | `cmd/kingresd/ha_failover.go` (new), `internal/internalapi` (call table), `internal-api.md` |
| engine gate and cancel guard | `task.Options.Gate`, `Definition.Cancellable` | `internal/task` |

#### 3.1.1 Additions to the lease protocol

All of them keep the safety argument of section 2.2: a grantor never grants a lease to a holder other than
the current one before `receipt + D + M`, except when the current holder itself says so.

1. **`transferTo`.** `POST /lease/grant` accepts `{group, durationMs, transferTo: "<server>"}`. The grantor
   executes it only when the caller (named by its certificate) is the current holder of the group, even if
   the lease has not expired: the holder becomes `transferTo` for a fresh duration from the receipt, the
   change is persisted before the answer (as every change of holder is), and the answer is `{granted: true,
   holder: "<server>"}`. From any other caller it answers `granted: false`. This is the atomic hand-over:
   there is no instant at which the group has no holder, so the old holder cannot be granted it again.
2. **`release`.** `{group, release: true}` from the current holder deletes the grant (persisted). It is used
   only when the caller has **confirmed that no primary runs under the lease and the fence marker is on
   disk**. It is optional (without it the lease runs out); it exists so that a switchover does not wait 13 s
   and so that a holder that is being removed gives its leases up.
3. **`GET /lease/state`** also reports `durationMs` and `marginMs` of the answering server, so that the leader
   can refuse a failover when two servers disagree (section 3.3 step F0).

`lease.Manager` gets:

- `Adopt(group, kind)`: renew the lease of a group that the catalog does not (yet) name this server as the
  primary of. `kind` is `fence` (no instance is attached; nothing is fenced when it is lost) or `holder` (the
  server is about to run the primary). Adopted groups are added to `Topology.Groups` in `Renew`.
  `AdoptionStatus(group)` answers `{held, waitMs, refusedBy}` from the last round, so that a step can wait
  without a second protocol.
- `Attach(group, instanceID)`: the adopted lease now guards the instance (after `pg_promote`); losing it from
  then on fences that instance like any primary's.
- `Transfer(group, to)`: a round with `transferTo`; the manager stops renewing the group when a majority
  acknowledged, and drops the adoption.
- `Relinquish(group)`: stop renewing but **keep checking** (`Check` still fences at `validUntil - M`), and
  write the markers of section 3.7. `Unrelinquish(group)` removes them (a cancelled switchover; a server that
  became a standby of the group).
- The majority is counted as today, over every distinct host of `kingres.node` (`catalogTopology`).

#### 3.1.2 The calls

New internal calls (names in the style of `internal-api.md`; policy as in `internalapi.Calls`). The task's
calls carry `X-Kingres-Fence`; every destructive call also carries `expectedTerm`, the catalog term the task
read at F0, and the callee refuses it with `409 stale_term` when its own catalog's term is higher (someone
promoted meanwhile). The receiving server acts on its own instances only.

| Call | Policy | Runs on | Does |
|---|---|---|---|
| `GET /failover/position?group=<g>` | any peer | a server that hosts a standby of g | read-only: `{server, group, instanceId, inRecovery, timeline, receiveLsn, replayLsn, receiverState, systemIdentifier, catalogTerm}`, from `pg_is_in_recovery()`, `pg_last_wal_receive_lsn()`, `pg_last_wal_replay_lsn()`, `pg_stat_wal_receiver`, `pg_control_system()`, `pg_control_checkpoint()` and `kingres.catalog_status` |
| `GET /liveness` | any peer | any | this server's `Liveness` view of every server (state, silence in ms); corroborates a death (section 4.2) |
| `POST /lease/grant`, `GET /lease/state` | any peer | any | with the additions of 3.1.1 |
| `POST /failover/freeze`, `/unfreeze` | leader | the server of OLD | `{group, ttlMs, task}`: sets or clears the freeze; it lapses after `ttlMs` unless renewed (the task renews it every 30 s while it runs); `Agent.Frozen` reads it |
| `POST /failover/standbys-gone` | leader | every server that runs the primary of some group | `{server}`: the agent treats the standbys on that server as not planned, so its list is emptied now and not after the grace (step F0b) |
| `POST /failover/lease` | leader | the fence holder, then NEW's server | `{group, role: "fence" \| "holder", transferTo?, waitMs, expectedTerm}`: `Adopt`, or `Transfer` first, and report `AdoptionStatus` |
| `POST /failover/demote` | leader | the server of OLD | `{group, mode: "fast" \| "immediate", expectedTerm, transferTo, mark}`: `Relinquish`, optionally `switchover_mark()`, stop the instance, confirm it is not running, read `pg_controldata` (after the stop, as the last thing before the answer), then `transferTo` (or `release`); answer `{stopped, clean, mark, checkpoint, clusterState, transferred}`, `clusterState` being the text of "Database cluster state" as printed and `clean` true only when it is exactly `shut down` |
| `POST /failover/quiesce`, `/unquiesce` | leader | the server of OLD | switchover only (section 5.2): stop writes to the group, and let them in again |
| `POST /failover/verify` | leader | NEW's server | `{group, mark, checkpoint, clusterState}`: `switchover_replayed(mark)` and `switchover_verify(checkpoint, clusterState)` on NEW's instance; answers `{replayed, verified, unprovable, detail}`: `unprovable` is true when the proof raised `KS006` (the text in `detail`) |
| `POST /failover/promote` | leader (or the bootstrap, locally) | NEW's server | step F6: the lease check, the empty list, `pg_promote`, the role file, `Attach` |
| `POST /failover/promote-node` | leader | NEW's server | map group only: `SELECT kingres.promote_node(id)` on NEW's own instance, answer as JSON; the repeat loop is the caller's |
| `POST /failover/prepared` | leader | NEW's server | `SELECT kingres.recover_prepared_transactions()` on NEW's instance, and the count of `kingres_h%` prepared transactions there |
| `GET /failover/state?group=<g>` | leader | any | the server's knowledge of one group: role file, `inRecovery`, `Holds`, adopted or relinquished, the term of its catalog, prepared count, destination of its client entry point |
| `POST /standby/repoint` | leader | a standby's server | step F8 |
| `POST /standby/adopt` | leader | the old primary's server | section 6.3: the old primary becomes a standby (clean or diverged) |
| `POST /failover/map/start`, `/continue` | forwarded operator request, or another candidate (new policy `MapCandidate`, section 3.4) | the best standby of the map group | the bootstrap |
| `POST /pki/escrow` | leader | a server that hosts a standby of the map group | section 3.4, the certificate authority |

The generic call `POST /exec` (leader-only SQL on the callee's primary instance) is **not** used: the
instance a statement must reach is no longer the callee's instance 1, and a call that names a group and an
instance is easier to authorize and to test than SQL in a body.

### 3.2 How a step reacts, and what it records

The same words are used in every table below.

- **go on**: the step is done; the next step runs.
- **repeat (bound)**: the step runs the call again; the bound is a count and a time.
- **wait**: the step polls at the stated period; waiting is bounded, and a bounded wait that ends is a
  `fail` or an `ask`, never silence.
- **fail (code)**: the task is recorded failed with that code (`task.Error`), `Retryable` as stated, the
  report written, the alert raised; the group is left in a state that the extension and the markers keep
  safe (section 3.9).
- **ask**: `task.Pause(reason)`: the task is `paused`, the reason is in `Task.detail` and in the step's
  `detail`, an alert says what the operator must do. A repeated `failoverServer` for the same server
  **resumes** the same task with the new options (as `startRebalance` resumes a stopped rebalance), and
  `cancelTask` cancels it (section 3.9 says what cancel undoes).
- **A blip of the system database never fails a task** (RBR-02). Every record of the failover goes through
  one function that tells apart `ErrLeaseLost` (stand down quietly: no hook, no record), a refusal
  (`*problem.Problem`) and a transient error (connection lost, restart or failover of the system database,
  idle-in-transaction timeout). The third is retried with a growing wait (1 s to 8 s) for up to 5 minutes,
  and the call in flight is never abandoned because of it: the step asks the callee what it did (`GET
  /failover/state`) before it records. After the 5 minutes the run stands down quietly and logs why (a line
  with task, step, holder, epoch and error: RBR-07); the task is claimed again when its lease lapses.
- **A call that is merely slow is never cancelled by a hold** (RBR-01). The failover takes no guard lock with
  a time limit around a call. The calls are repeatable, or guarded where they run (lease, expected term), so
  a superseded holder's late call is harmless; the lock on the task row that `internal/rebalance` keeps
  across `move_bucket` is not needed and is not used.
- **A loop that cannot end has a bound and an alert** (RBR-07). Every repeat and wait states its bound;
  reaching it raises `failoverBlocked` (or `failoverNeedsDecision`) and logs the reason once a minute until
  it clears.
- **Recorded by every step**: the step state (`running`, `done`, `skipped`, `failed`) and `detail`; a row of
  `failover_event` (section 3.8) for what a report should show; for the destructive steps an audit event
  (`failoverGroupStarted`, `failoverGroupPromoted`, `failoverGroupFailed`, section 7).

### 3.3 (a) Failover of an ordinary group

The groups of the server are processed in order; this section describes one group `g` whose primary is OLD
on server `S_old`, with NEW a ready standby of `g` on server `S_new`. The leader runs the task on its server
`L`. F0 and the last step belong to the task, F1 to F13 to each group.

#### F0. Check that the failover can start (task)

Precondition: the request passed the API (role `kingres_ops`, valid body). Calls: SQL on MAP (local) and `GET
/lease/state` of every member.

| Check | Answer if it fails |
|---|---|
| the server exists; every group of the request is a group whose primary runs on it (the catalog) | 404 / 422 `unprocessable` (before a task exists) |
| every group has at least one ready standby in the plan (not `creating`, not `rebuilding`) | 409 `no_replica_available` |
| mode `graceful`: the server is `live`, its primary instance runs and its lease is held | 422 `node_state_invalid` |
| the leader hears a majority of the members (itself counted) and can open a connection to MAP | 409 `quorum_unavailable` (new) |
| every reachable member reports the same `durationMs` and `marginMs` | 409 `lease_settings_differ` (new): the safety argument needs equal settings |
| the extension version and the PostgreSQL major are equal everywhere (as the rebalance's check) | 409 `version_mismatch` |
| no other task of the failover class is unfinished (migration 0011) | 409 `failover_in_progress` |
| no rebalance is pending or running (migration 0009); a stop is requested first (section 4.5) | 409 `rebalance_in_progress` if it did not stop within 60 s |
| the verdict of section 2.6 for each group is `none`, or the request has `allowDataLoss` | 409 `failover_needs_decision` (the body lists the groups and the loss in words) |

Records: `failover_run` (request, trigger, plan, `expectedTerm` from `SELECT term FROM kingres.catalog_status`
on MAP), one `failover_group` per group with the node ids of OLD and of the candidates (`SELECT node_id,
group_id, host, port, role FROM kingres.node`). Idempotent: it only reads and upserts the same rows.

**F0b. Stop waiting for the standbys of the failed server (task, after F0).** When the request is `hard` and
the server is not `live`, the leader calls `POST /failover/standbys-gone {server: S_old}` on every server that
runs the primary of a group. Reason: `promote_node` is a two-phase transaction on every primary of the
cluster, and a primary whose synchronous standby sat on the dead server makes the call fail with `KS005` (or
"could not prepare the transaction on node ...") until its own agent has emptied its list, which the agent
does only after `standby-sync-grace` (30 s) of no streaming standby (`SyncState.Decide`); extension
precondition 3, sequence 8, `t/073` B. Without this step every failover of a dead server would wait for the
grace of the other groups. With it their lists are emptied at once; each such group raises
`groupWithoutSynchronousStandby` (which is true: its standby is gone). The step does not touch group `g`
(it is frozen). Idempotent (the set of "gone" servers is replaced by the same value). A server that cannot be
reached is skipped and recorded; its list is emptied by its own grace.

#### F1. Freeze the group

Precondition: F0 done. Call: `POST /failover/freeze {group: g, ttlMs: 120000, task, epoch}` on `S_old` (best
effort when it does not answer); the leader also marks the group frozen in its own state and in
`failover_group.state = 'frozen'`. Effect: `Agent.Frozen(g)` is true on `S_old`, so a standby that stops
streaming does not make OLD empty its list and acknowledge commits that NEW will not have (the reason of the
hook in `syncnames.go`); and the leader's reconcile loop (`standby.Leader`) does not create, remove or
rebuild a standby of `g` while the group is in a failover task (section 3.7). The task renews the freeze every
30 s (a renewer tied to the step context; it stops when the run stands down).

| Answer | Reaction |
|---|---|
| ok | go on |
| unreachable | go on (a dead server needs no freeze) |
| `409 stale_term` | fail `superseded` (retryable false) |

Idempotent. A lapsed freeze is harmless: OLD is stopped or fenced by F2 or F3 before anything depends on
it. Recorded: step state, `failover_event('frozen')`.

#### F2. Stop the old primary (only when OLD's server answers)

Precondition: F1 done; `S_old` answers `GET /failover/state?group=g`. When it does not, the step is
`skipped` with the detail "the server does not answer; its lease fences it (F3)".

Call: `POST /failover/demote {group: g, mode: "immediate", expectedTerm, transferTo: L}` on `S_old` (mode
`hard`; the `graceful` path is section 5). The handler (1) checks that its instance serving `g` is the
primary by its role file and that its own catalog's term is not above `expectedTerm`; (2) `Relinquish(g)`:
no more renewals, markers written; (3) stops the instance with `-m immediate` and waits until it is not
running (bound 30 s); (4) only after (3), asks a majority of grantors to `transferTo` the fence holder `L`,
which makes F3 immediate. If the instance cannot be stopped (a process stuck in the kernel) the lease runs out
by itself, the manager's `Check` keeps retrying the stop every 5 s, and the critical alert
`oldPrimaryNotStopped` is raised (section 6.4).

| Answer | Reaction |
|---|---|
| `stopped: true, transferred: true` | go on to F4 (the fence lease is held already; F3 is `skipped`) |
| `stopped: true, transferred: false` | go on to F3 (the transfer did not reach a majority; the lease expires by itself) |
| `stopped: false` after 30 s | go on to F3; the alert; the lease protects (OLD's holder stopped renewing) |
| `409 stale_term` | fail `superseded` (another failover happened), not retryable |
| `409 not_primary` | the instance is not the primary of `g` (already demoted, or promoted away): read `GET /failover/state` and go on to F3 |
| unreachable, timeout | treat as dead: go on to F3 |

Idempotent (every part checks what exists: a stopped instance is stopped; markers exist; a transfer is
refused from a non-holder, and then F3 finds the lease free or held by `L`). After a restart or a change of
holder the step runs again from its start. Recorded: clean or crash stop, `transferred`.

#### F3. Take the fence lease

Precondition: F1 done (F2 done or skipped). Call: `POST /failover/lease {group: g, role: "fence", waitMs:
25000, expectedTerm}` on `L` itself (the leader's own `lease.Manager.Adopt(g, fence)`), then poll
`AdoptionStatus(g)` every 1 s. The manager asks every member for the lease as a holder would (a normal
round, not `dryRun`); a refusal carries `waitMs`; the majority-th smallest remaining wait is the time until
the lease can be had. At least a majority of members must **answer** at all.

| State | Reaction |
|---|---|
| held (`Holds(g)` at `L`) | go on: OLD cannot act any more (I1) and can never again obtain a majority |
| refused, holder is `S_old`, `waitMs` falls | wait |
| refused for 25 s, holder still `S_old` and its remaining time was renewed (the grantor says `holder == S_old` and `remainingMs` close to D) | fail `lease_still_held` (retryable true): a majority of the servers still hear OLD's holder, so the primary is alive for the cluster. The alert `failoverBlocked` says so; the freeze is lifted |
| fewer than a majority of members answer | wait up to 60 s, then **ask** (`quorum_unavailable`): the leader is on the minority side; nothing is done; the alert `failoverBlocked` |
| another server holds the lease (not `S_old`): a failover of this group is under way elsewhere (a candidate of a map-group bootstrap, a task that was superseded) | wait 25 s, then fail `lease_held_elsewhere` (retryable true) |

Idempotent: adopting again changes nothing; if `L` already holds the lease the step is immediate. After a
restart or a change of leader the new leader adopts (it waits for the previous leader's adoption at the
grantors, at most 13 s, or finds it released). Recorded: the wait (`lease_wait_ms`), which grantors refused,
the time the lease was held (`failover_event('leaseHeld')`).

**Why the leader's server and not NEW's.** NEW is not known yet: the positions must be read after OLD is
fenced (I2), and the fence is the lease. The fence holder holds the group lease without any instance and is
replaced by NEW's server with an atomic hand-over (F5). When `L` itself is NEW's server, F5 only attaches.
Reading positions *before* the lease is held would let a live-but-partitioned OLD go on writing after the
reading: a grantor that expired OLD's grant still grants OLD again when OLD asks (it is the same holder), so
only a change of holder closes the door.

#### F4. Choose the standby (and pin the choice)

Precondition: F3 done. Calls: `GET /failover/position?group=g` on the server of every ready standby of `g`
(parallel, 5 s each). The algorithm, the pin and the verdict are in section 3.6.

| Result | Reaction |
|---|---|
| a candidate is out of recovery and it is the pinned one, or the only one | it was promoted by an earlier run: pin it; F5 is `skipped`; F6 answers `already` |
| two or more instances of `g` are out of recovery | fail `split_promotion` (retryable false), critical alert `splitPromotion`; nothing else is done; the operator decides which to keep (I5) |
| all ready standbys read, verdict `none` or accepted (`allowDataLoss`, or the automatic rule of 2.6) | pin, go on |
| a ready standby cannot be read | verdict `unknown`: **ask** unless `allowDataLoss`; automatic: the alert `failoverNeedsDecision` |
| no standby can be read | fail `no_candidate` (retryable true), lift the freeze; the leader keeps the lease, so nobody else promotes; `failoverBlocked` |
| the requested `target` is not among the ready standbys | 422 at the request; on resume: fail `target_not_available` |
| the requested `target` is behind the best | verdict at least `possible` (the difference is lost): needs `allowDataLoss` |

The pin is one conditional transaction (`UPDATE failover_group SET chosen_standby_id = ... WHERE task_id AND
group_id AND holder AND epoch`); a resumed run reads it and never chooses again. Idempotent. Recorded:
`failover_group.decision`.

#### F5. Hand the lease to the new server

Precondition: F4 pinned NEW. Calls: (1) when `S_new != L`: `POST /failover/lease {group: g, role: "fence",
transferTo: S_new}` on `L` (`Transfer`); (2) `POST /failover/lease {group: g, role: "holder", waitMs: 15000,
expectedTerm}` on `S_new` (`Adopt(g, holder)`; it answers when `Holds(g)`).

| Answer | Reaction |
|---|---|
| (1) transferred by a majority and (2) `Holds` | go on |
| (1) refused (`L` is no longer the holder) and `GET /failover/state` on `S_new` says `Holds` | go on (a repeated run) |
| (1) refused and nobody holds | go back to F3 (once); a second time: fail `lease_lost` (retryable true) |
| (2) not held after 15 s | repeat (3 times, 5 s apart), then fail `lease_lost` |
| `409 stale_term` | fail `superseded` |

Idempotent as above. Recorded: `failover_event('leaseHandedOver')`.

#### F6. Promote the standby (the point of no return)

Precondition: NEW pinned and its server `Holds(g)`. Call: `POST /failover/promote {group: g, expectedTerm,
standbyId}` on `S_new`. The handler, in this order and with nothing slow between the check of the lease and
the promotion:

1. the instance of standby `standbyId` exists and its role file says standby of `g` (else `409 not_standby`);
2. its catalog's term is not above `expectedTerm` (else `409 stale_term`);
3. if the instance is already out of recovery: answer `already` with timeline and position (idempotent);
4. `Leases.Holds(g)`, else `409 lease_not_held`;
5. write `synchronous_standby_names` empty into the managed file and reload (harmless while in recovery; the
   extension's precondition 3: a promoted standby inherits its primary's list and every commit on it would
   wait for ever). The write goes through the agent (`Agent.SetSyncNames`), so the agent's own loop and this
   call never write the file at the same time;
6. `SELECT pg_promote(true, 60)` on the instance;
7. on success: write the role file (primary of `g`; fsync; atomic), `Attach(g, instance)`, start the watchdog
   for the instance (its own `lease.json`), drop the agent's cached group;
8. answer `{timeline, lsn, tookMs, status: "promoted" | "already"}`.

| Answer | Reaction |
|---|---|
| `promoted` or `already` | record `promoted_at`, timeline and position; count the event (section 4.4); audit `failoverGroupPromoted`; go on |
| `409 lease_not_held` | go back to F5 once; then fail `lease_lost` |
| `409 stale_term` | fail `superseded` |
| `409 not_standby` | unpin and go back to F4, unless an instance of `g` is out of recovery (then F4's rule) |
| `pg_promote` returned false (not finished in 60 s) | repeat (3 times, 10 s apart: the call is idempotent), then fail `promote_timeout` (retryable true; the pin stays, a resume finds the instance promoted) |
| connection broken, timeout | repeat; the callee's state is the truth (`GET /failover/state`) |

This step is `Irreversible` and **not** marked `NonIdempotent` (a crash must not fail the task: every part of
the handler checks what exists). After it is recorded `done`, `Task.cancellable` is false (the engine guard,
`Definition.Cancellable`), and `cancelTask` answers 409 `task_not_cancellable`: the task rolls forward only.
The "point of no return" of this document is exactly this call: before it OLD's data directory can still be
started as the primary (a switchover that fails verification is rolled back, section 5.3); after it the
timelines differ.

#### F7. Make the catalog follow (`promote_node`)

Precondition: F6 done. Call: on MAP (the leader's local connection to its instance serving group 0)
`SELECT kingres.promote_node(<NEW node id>)`, repeated by the rule of sequences 2 and 9: at most 10 calls, 2 s
apart, ending at `already` with `final` true; a client timeout of `3 x node_timeout + node_connect_timeout x
nodes` (read from MAP's settings); and a budget of 15 min for the step. **Decision:** the README bounds the
count of calls, not the time, and one call can last 195 s with three nodes. A call is cancelled only by its
client timeout, never by a hold (RBR-01), and the repeat is the extension's own contract ("a cancel is as
good as a crash here").

| Answer (README sequences 2 and 9) | Reaction |
|---|---|
| `promoted`, `final` false | repeat |
| `promoted` or `completed`, `unreached` not empty | record the ids; repeat to `already`; the ids that are primaries are demoted by the check below; `sync_catalog()` is scheduled for when they return (F13) |
| `completed`, `final` false | repeat |
| `already`, `final` true | go on; record `term` and `unreached` |
| any status with `blocked_by`, class `cluster` | call `recover_prepared_transactions()` on MAP once, repeat |
| `blocked_by` with a `foreign` gid, the same gid in two answers in a row | **ask** (alert `failoverBlocked`, critical): "the prepared transaction `<gid>` on node `<N>` is not the cluster's and holds a lock the catalog change needs; end it with `ROLLBACK PREPARED` on that node, then repeat the failover request" |
| error "node N cannot commit: its synchronous standbys do not confirm", N is NEW | the list on NEW was not emptied: call `POST /failover/promote` again (it re-applies the empty list), repeat |
| the same, N is another group's primary; or `KS005` ("did not confirm that its part of the transaction is durable") | wait and repeat: that group's list names a standby that is gone. If F0b ran, the list is empty already, so this is a standby that is down for another reason, and the agent of that group empties the list after the grace (30 s). The task's detail says "waiting for the standby of group G" |
| "could not prepare the transaction on node H:P ..." (any text) | **open point 11.1**: handled as `KS005`: repeat within the budget; the text goes into the step detail; nothing was committed (README) |
| error "node N is still in recovery" | wait 2 s, repeat (bound 60 s, then fail `promote_incomplete`) |
| error "could not connect to node N (H:P), which is to become the primary" | wait 2 s, repeat (bound 60 s, then fail `promote_incomplete`) |
| error "this function can only run on the map group primary" | the leader's connection is not to MAP: fail `not_map_primary` (retryable true; the run stands down) |
| error "this node was promoted in PostgreSQL after the catalog named it the map group primary" | the map-group bootstrap is not finished: wait 5 s, repeat (bound 5 min) |
| error "node N does not exist", or a NULL answer | fail `bad_promote_target` (a bug of the task; retryable false) |
| any other error, a broken connection, a client timeout | repeat (sequence 9) |
| after 10 calls or 15 min without `already` | **ask**; read `blocked_by`, `unreached` and the log of MAP into the step detail |

After `already`: **check** with two reads on MAP (`SELECT node_id, role FROM kingres.node ORDER BY 1`,
`SELECT term, superseded FROM kingres.catalog_status`) that NEW is `primary` and OLD `standby`, and, for
each server in `unreached` whose instance is a primary and that answers, call `POST /failover/demote` on it
now (the extension: "if OLD is not reached it goes on taking writes that nobody else has"). If OLD runs as a
primary here, F2 was skipped wrongly, and this is the fence of last resort; the critical alert
`oldPrimaryRunning` is raised, with the window of writes it may have taken (section 6.2).

Idempotent by the extension's design (`already`). Recorded: every answer in `failover_event`, `term_after`.
**From this moment clients are routed to NEW**: the client entry points choose by catalog and lease
(`clientproxy.PrimaryResolver`) and the extension's routing follows the catalog.

#### F8. Point the other standbys at the new primary

For every ready standby of `g` other than NEW: `POST /standby/repoint {entry}` on its server, with the
`PrimaryServer` and `PrimaryPort` of NEW. The handler creates the physical slot of that standby on NEW
(`pg_create_physical_replication_slot` with immediate reservation, over the certificate connection, as
`Agent.ensureSlot` does for a new standby), rewrites `primary_conninfo` and `primary_slot_name` in the
managed file, reloads (`primary_conninfo` is reloadable), and waits up to 60 s for `pg_stat_wal_receiver` to
say `streaming`.

| Answer | Reaction |
|---|---|
| `streaming` | go on |
| not streaming after 60 s | the standby is ahead of NEW or on another timeline (extension: "one that is ahead of NEW cannot follow"): mark it `needs_rebuild` with `diverged` and let the reconcile loop rebuild it (`pg_rewind`, else a base backup); `skipped` for that standby with the reason; do not fail |
| server unreachable | skipped for it; its agent re-points itself when it returns (section 3.7) |

Idempotent. Recorded per standby.

#### F9. Update the standby records and send the plan

One transaction (conditional on holder and epoch): delete NEW's `kingres_cp.standby` row (the instance is the
primary now), set `primary_server` of the others, **insert the row of OLD** as a standby (`group_id`,
`server_id = S_old`, `primary_server = S_new`, `instance_id` = the directory id of OLD's instance on
`S_old`, `state = 'rebuilding'`, `needs_rebuild = true`, `diverged = true` (new column), `generation + 1`,
`catalog_node_id` = OLD's node id, which the extension kept), advance `standby_plan_version`, and send the
plan to every server (`POST /standby/plan`, the existing push). Idempotent (upserts by `(group_id,
server_id)`). This is also what makes `HasStandby(g)` true on `S_new`, so that its lease is enforced.

#### F10. Check `synchronous_standby_names` (never fails)

NEW's agent (`maintainPrimary` for the group named by the role file) applies `SyncState.Decide`: `ANY 1
(<ready standbys that stream>)` as soon as one streams, else it leaves the empty list of F6. The task waits
up to 2 min for `GET /standby/status` of `S_new` to show a non-empty list or no planned standby, then records
the state. "The group runs without a synchronous standby until a standby streams" is not a failure
(`groupUnderReplicated` and `groupWithoutSynchronousStandby` are the alerts of part 1). When NEW has no
OTHERS the group has no standby until OLD is rebuilt or a new one is created; the extension's guarantee 12
applies to commits meanwhile, and the report says so.

#### F11. Resolve prepared transactions and read the damage

Calls: `POST /failover/prepared` on `S_new` (`recover_prepared_transactions()` on NEW), the same on MAP
locally, then `SELECT * FROM kingres.check_cluster()` on MAP; the count of `kingres_h%` prepared
transactions on every primary through `GET /failover/state`.

| Result | Reaction |
|---|---|
| count 0 on every primary within 2 min (calls repeated every 30 s) | go on |
| prepared transactions remain after 2 min | go on; the report lists them (the worker resolves them within `recovery_interval`); no failure |
| rows `standby_unreachable` for OLD | expected until it is rebuilt |
| rows `prepared_rolled_back_after_failover`, `participant_lost_committed_transaction` | recorded in the report as `incidents`, each with the extension's text; the alert `failoverDataLoss` (warning when the group had a synchronous standby, critical when it ran without) |
| `standby_not_in_recovery` for OLD | OLD runs as a primary: F7's last-resort demote; critical alert `oldPrimaryRunning` |
| `foreign_prepared_transaction`, `decision_unconfirmed` | recorded; not the task's business |
| a call fails | repeat 3 times, then `skipped` with the detail; never fails the task |

Idempotent. Recorded: the incidents.

#### F12. Check that clients arrive at NEW

`GET /failover/state?group=g` on `S_new` until it says: `Holds(g)`, its catalog names it the primary, and the
destination of its client entry point for `g` is local; and on `S_old` (when it answers) that its entry point
no longer forwards to its old instance (`clientproxy` closes such connections within a second). Bound 30 s;
the end of the bound is a warning in the step detail, not a failure.

#### F13. Lift the freeze and hand the old primary to the reconcile loop

`POST /failover/unfreeze` on `S_old` (best effort) and in the leader; `failover_group.state = 'done'`;
`failover_group.old_primary` is set to `rebuildScheduled` when `S_old` is reachable and its instance is
stopped, `waitingForServer` when it does not answer; the leader's reconcile loop is kicked
(`standby.Leader.Kick`); the nodes in `unreached` are kept in `failover_group.unreached` for the
`sync_catalog()` that the loop makes when they are `live` again. The old primary is **not** rebuilt inside
the task: a base backup takes minutes and must not hold the task, the lease or the exclusion (section 6).

#### Last step: write the report (task)

Sets `failover_run.outcome` (`completed` when every group ended `done`; `partial` when some did and some did
not; `failed`; `cancelled`) and `finished_at`, raises `autoFailoverTriggered` for an automatic run, clears
`failoverBlocked` and `failoverNeedsDecision`, writes the audit events. It increments nothing (the counter
was counted at F6). Idempotent.

### 3.4 (b) Failover of the map group

**The problem.** The system database, the task engine and the leader are on the map group. When MAP is lost,
nothing can record a task, nothing can read settings from the leader, and the leader's `kingresd` is gone
with it. The standby of the map group must be promoted, and the leader role must move, **before any task
state can be written**. Meanwhile every other server runs without a leader: reads and ordinary queries keep
working, changes to the cluster wait (ADR 0008, ADR 0018).

**Decision.** The promotion of the map group is run by a **bootstrap** on the server that hosts the best
standby of group 0, under its own authority and without the task engine; the task is recorded as soon as the
system database is writable again, and the engine finishes the work. The bootstrap uses the same step
functions as the task (`internal/failover`), with a different recorder: a local journal instead of task rows.

**Who runs it.** Every server that hosts a ready standby of group 0 (its plan file `standby/state.json` says
so) runs a *candidate monitor*. The candidate that acts first is the one the ranking puts first (the order of
F4, computed from the positions of all standbys of group 0 read with `GET /failover/position`; ties by ring
distance from the old primary, then standby id). The other candidates wait a stagger of 30 s per rank and
re-check that no newer term exists (`kingres.catalog_status` of their own catalog, and the other servers'
answers to `GET /failover/state?group=0`) before they would act. The lease makes the choice safe whatever
the order: only one server holds the group-0 lease.

**The authority a follower proves.**

1. *The evidence of the loss.* Automatic: its own tracker says the leader's server has been silent for at
   least `autoFailover.timeoutSeconds` (read from the replicated copy of `kingres_cp.cluster_setting` on its
   hot standby), and **a majority of the servers agree** (`GET /liveness`: each reachable server reports its
   own view; the candidate counts itself; a candidate that does not hear a majority does not act). Manual: an
   operator's request, forwarded by the server that received it with the proof that `internal/fwdproof`
   already carries for forwarded operations, accepted by the candidate with the same role check the leader
   makes.
2. *The lease.* The candidate obtains the group-0 lease from a majority of all servers (F3, waiting for the
   old lease): this proves that a majority can talk to it, that the old holder's lease has run out at a
   majority, and that the old holder can never again get one.
3. *The extension's rule.* `promote_node` of the map group runs on NEW itself and refuses unless NEW reaches
   the primary of a strict majority of the groups other than the map group (guarantee 9). Until it is
   accepted the extension refuses every catalog change on NEW.

Two majorities (of servers by the lease, of groups by the extension) must therefore both be reachable. A
partition that leaves a candidate with one but not the other makes the failover wait, which is the safe
direction. The map group of a cluster of two groups, or of one that lost half of the other groups with MAP,
cannot satisfy the extension's rule; the bootstrap waits and alerts, and the operator uses
`promote_map_group_without_majority` by hand (README sequence 3; open point 11.4).

**The bootstrap, step by step.** It writes `<data-dir>/failover/map-journal.json` (fsync, atomic replace)
after every step; a restarted `kingresd` reads it first. B3 to B6 are the functions F3 to F7 with the
differences noted.

| Step | What | Differences from the ordinary group |
|---|---|---|
| B1. Decide | evidence (above), a ready standby of group 0 here, no journal of a newer term | the checks of F0 that need MAP cannot run; those that can (versions, lease settings) are made from the members' answers |
| B2. Freeze | the candidate marks group 0 frozen locally | local only: the leader is dead, there is no OLD agent to tell |
| B3. Take the fence lease | as F3, `Adopt(0, fence)` on the candidate, waiting for the old lease | if `S_old` answers a call (a switchover, or a partition on the leader's side), it is asked to `demote` first (section 3.5) |
| B4. Read and pin | `GET /failover/position?group=0` on every ready standby of group 0, ranking, pin in the journal. If another candidate is better, the candidate `Transfer`s the lease to it, calls `POST /failover/map/continue` there and stops; the better one resumes at B4 with the same journal id | the rules of F4; an unreadable standby, or a candidate seen out of recovery: no automatic action, alert `failoverNeedsDecision` in the candidate's local alert store (the system database is not writable) |
| B5. Promote | `POST /failover/promote` locally (F6, steps 1 to 8) | none |
| B6. Make the catalog follow | `promote_node` **on NEW's own instance**, by the rules of sequence 3 (the table of F7 with these differences): "only k of the n other shard groups can be reached from this node" -> wait 5 s and repeat (bound 10 min, `failoverBlocked` after 60 s); "holds catalog term T, newer than the T0 of this node" -> another server promoted the map group: stop, fence this instance at once (`StopImmediate`, markers), alert `splitPromotion`; "this node is not a copy of the primary of group 0" or "registered on port P, this server listens on port Q" -> fail (a bug of the plan or the registration) | the call is local |
| B7. Make the new leader | wait until the local catalog names this server the primary of group 0 and the other servers' answers to `GET /failover/state?group=0` show the new term (the extension's push; bound 60 s); then **pause what the old leader left running** and record the task (below) | leader-only calls from this server are refused with `not_leader` by servers that do not yet have the catalog; B7 waits for that |
| B8. Open the gate | write `catalogDone`; open the engine gate | see below |

**What B7 does, exactly.** The new leader's database is the copy the standby replayed up to the loss. (a) A
task that was `running` with an expired lease stays; the engine, once its gate is open, claims it like any
leftover (the existing recovery). (b) A **rebalance** that was `pending` or `running` is paused by the
bootstrap first, with a direct conditional update (`state = 'paused'`, `stopped_by = 'safetyRule'`, text "the
map group was failed over"): migration 0009 would otherwise refuse the failover task, and the engine must not
resume a rebalance in the middle of a promotion; the extension's recovery resolves the move in flight when
the rebalance is started again (`resume_bucket_moves`). (c) The task `server.failover` for `S_old` is
inserted with `Engine.Record` as `pending`, with the steps of group 0 up to F7 marked `done` and the later
steps `pending`, `createdBy` the operator or `system`; the report rows (`failover_run`, `failover_group` for
group 0 with the decision, positions and `lease_wait_ms`, and the journal's events) are written from the
journal. The ordinary groups of `S_old` get their blocks with the normal steps. (d) The engine claims the
task and runs F8 to F13 for group 0 and F0 to F13 for the others, **from the new leader** (`L` is now the
new leader's server and MAP its local instance).

**The engine gate.** `task.Options.Gate func() bool`: while it returns false the engine neither claims nor
runs. The gate of a server is closed while (1) its instance serving group 0 is in recovery, or (2) its
journal says a bootstrap is in progress. It opens at B8, and at an ordinary start when neither is true. This
keeps a server that was promoted in PostgreSQL but is not yet the recognised map primary from starting tasks
that call `promote_node` (they would be refused: guarantee 9) or from resuming the old leader's rebalance.

**The task id of the bootstrap phase.** The task row does not exist until B7 (no sequence can be read from a
database that is not writable). A manual request answered during the bootstrap gets `202` with a `Task` whose
`id` is 0, `state` `running`, `kind` `server.failover` and the journal's steps, and `Location:
/api/v1/failover` (the status resource of section 7); a client polls `GET /failover` until its `taskId` is
non-null and then `GET /tasks/{id}`. Id 0 never names a stored task; `GET /tasks/0` on the server that runs
the bootstrap answers from the journal. Open point 11.5.

**If the bootstrap is interrupted.** A restart of the candidate's `kingresd` continues from the journal (B5
and B6 are idempotent; B4's pin is in the journal). If the candidate never returns, the other candidates'
monitors, after the stagger and the old lease's expiry, find (in `GET /failover/position`) that an instance
of group 0 is out of recovery, or is unreadable with a journal that says it promoted: they **do not promote
their own** (I5), and the alert `failoverNeedsDecision` in their local stores asks for a decision, because a
catalog change may have been left prepared on the promoted instance (the extension finishes it only from that
node, or by a repeated `promote_node`). The operator either restarts the candidate's server or declares it
lost (`failoverServer` on it with `allowDataLoss: true`), which promotes the next standby with the loss the
report states.

**The certificate authority.** The new leader needs `ca.key` and `revoked.json` to issue and revoke
certificates (README "What has to move with the manager"). **Decision:** the leader keeps an encrypted copy
of both on the servers that host a ready standby of group 0 (`<data-dir>/pki/escrow/`, mode 0600, sealed with
a key derived from the cluster secret that every server already holds for node sessions; sent by the internal
call `POST /pki/escrow` after every change of `revoked.json` and every 6 hours), and B8 adopts it before the
gate opens. Without the escrow the new leader answers `certificate_authority_unavailable` (an existing
problem code) for the operations that need the CA, and the renewal jobs of the other servers fail visibly
until it is adopted; their certificates last a year, so a failover without escrow is survivable for a long
while but not acceptable for ever. Open point 11.2.

### 3.5 (c) The old primary is reachable

Sequence 4 of the extension: demote first. The task already does: F2 runs whenever `S_old` answers. What
differs from a dead OLD:

- OLD's server is asked to `Relinquish` and stop before anything depends on the lease, and transfers the lease
  to the fence holder, so F3 takes no time (the 13 s of section 2.5 do not apply).
- `mode: hard` stops with `-m immediate` (the operator called the server unresponsive; a fast shutdown that
  hangs is the thing to avoid). `mode: graceful` is section 5.
- If `S_old` answers `GET /failover/state` but the instance cannot be stopped, the lease still decides: the
  holder has stopped renewing; at expiry the manager fences (retrying the stop every 5 s); F3 waits for the
  lease; the alert `oldPrimaryNotStopped` is raised. Nothing promotes while an old primary might still be
  writing: the lease, not the stop, is the proof.
- If OLD was **not reached** by `promote_node` (it is in `unreached`) and runs as a primary, F7's last-resort
  demote stops it. The writes it took between the promotion and that stop are the extension's "acknowledged
  and then gone" case; the report records the window (the end of OLD's WAL from `pg_controldata` against the
  divergence point of the timeline history read at rebuild time, section 6.2).
- The map group with a reachable OLD (a switchover of the leader, or the leader's server alive but its
  instance sick): the leader's task runs F0 to F2 and then **hands over** to the bootstrap with a leader-only
  call `POST /failover/map/start` on the candidate. The old leader's instance is stopped by F2, so its engine
  stops with it; the task row (replicated to the candidate before the stop, and `switchover_verify` asks the
  same thing for the data) is claimed by the new leader's engine after B8, and its step "hand over" finds the
  work done.

### 3.6 Choosing the standby

**Candidates.** The ready standbys of `g` in the leader's plan (`kingres_cp.standby`, state `ready`, not
`needs_rebuild`, registered in the catalog), plus an instance of `g` that is found out of recovery (F4's
first row).

**Reading.** `GET /failover/position` on each (5 s). Two consecutive reads 1 s apart must give the same
`receiveLsn` for every candidate (bound 15 s), so that WAL in flight when OLD stopped has arrived; if a
candidate's value still moves at the bound, the last values are used and the step detail says so.
`receiveLsn` (flushed) is the position that counts (extension precondition 2); `replayLsn` is shown.
Candidates on another timeline, with another `systemIdentifier`, or not in recovery are not eligible (the
last is F4's rule).

**Order.**

1. A requested `target` is eligible only if listed; the verdict compares it to the best.
2. Highest `receiveLsn` (`pg_wal_lsn_diff`).
3. Equal: the standby that was `sync` or `quorum` at the last check (`group_replication_state`), then the one
   on the group's preferred server (so that the restore of section 5 is free), then the one nearest to OLD's
   server in ring order (the placement order of `ha-phase5.md`), then the lowest standby id. The order is a
   pure function of its inputs (unit test).

**Verdict** (section 2.6): `none` when every ready standby was read, the best is NEW, and the group streamed a
synchronous standby at a check younger than 30 s or every database of the group is `async`; `asyncOnly` when
the same holds but some database of the group is not `async` and the loss is therefore only of the
asynchronous commits; `possible` when the group ran without its synchronous standby or the target is behind
the best; `unknown` when a ready standby could not be read. The databases of the group and their durability
are read from MAP (the source of `Database.replication.durability` in `listDatabases`; to be verified).

**What the leader knows of OLD (`group_replication_state`).** The standby check of `standby.Leader` runs every
10 s and reads `GET /standby/status` of the primary's server. Part 2 adds to the answer the primary's flush
position (`pg_current_wal_flush_lsn()`) and writes it, with the time of the reading and whether a standby was
streaming and synchronous, to one row per group. This is the "last known position of OLD" of the report.

**No candidate qualifies.** No ready standby: 409 `no_replica_available` at the request; nothing happens
automatically (the alert `failoverBlocked`: "group g has no standby to promote", next to the existing
`serverDown` and `unavailableBuckets`). Standbys exist but none can be read: F4 `no_candidate`. All
candidates ineligible (timeline): fail `no_candidate`; the operator rebuilds.

**The pin.** Section 3.3 F4. A pinned standby whose server is found dead before F6: F4 unpins (recording the
event) and chooses again, unless it was promoted.

### 3.7 Everything in part 1 that assumes the primary of a group is instance 1

The instance directories are `instances/1` (the server's first instance, which `kingresd` creates and
configures) and `instances/<100 + group>` (the standby of a group). After a failover a server can run two
primaries (`instances/1` for its own group and `instances/<100+g>` for a promoted standby), or none (its
instance 1 became a standby). **Decision:** a directory keeps its id for life; what it serves is recorded in
`kingres-role.json` in the instance directory (`{group, role: primary | standby, origin: own | copy, term}`,
written atomically with fsync at creation, promotion and demotion). The catalog maps a (host, port) to a
node and the role files map a port to a directory, so no new cluster-wide state is needed. `origin: own`
means the data was initialised by this server (this server's service password is in it); `copy` means a
physical copy of another server's instance (a standby, or an old primary rebuilt from one), which `kingresd`
reaches by peer authentication on the private socket (today decided by `IsStandbyDirID`).

| Place (file) | Assumption today | Change |
|---|---|---|
| `internal/cluster`: `NodeID = 1`; `Service.ServiceConn`, `ServiceConnAt` (`connect` goes to instance 1; the password is used unless `IsStandbyDirID`) | the server's connection is to instance 1 with the service password | `ServiceConn` stays "the home instance" for what is per server; new `Service.SystemConn` (database `kingres_system` on the instance that serves group 0 when it is local, else the home instance), `Service.PrimaryConn(group)`, `Service.WritableInstances()`; password or peer follows `origin`, not the directory id |
| `cmd/kingresd/ha_leases.go`: `instDir` of `NodeID`, `localInstance{Running,Start,StopImmediate}` on `NodeID`, `catalogTopology` (`host == ServerHost && port == PGPort`) | one primary per server, on `PGPort` | the lease manager resolves the instance per group from the role files; `catalogTopology` matches the ports of every local instance whose role file says primary; `Topology.Groups` carries `instanceId` |
| `internal/lease`: the markers `fenced`, `waitlease`, `enforced.json`, the watchdog file `lease.json` in `instances/1`, the single watchdog (`watchdog.lock`) | one group per server | per group: `fenced.<g>`, `relinquished.<g>`, `waitlease.<g>`, `enforced.json` keyed by group (it is a list already), `instances/<id>/lease.json`, one watchdog per instance (`watchdog.<id>.lock`); `lease.Gate(dir)` takes the instance id and looks the group up |
| `internal/standby/agent.go`: `localGroup` caches the first group for ever (`a.group`); `once` returns unless `Exists(NodeID)`; `Probe` keeps one `a.primary` | one primary per server | `localGroups(ctx)` re-read from the role files every round; `maintainPrimary` runs for each local primary (its own instance id for the settings, `WriteManaged`, `Reload`, `ServiceConn`); `Probe` reports per group |
| `agent_ops.go`: `Create` and `Rebuild` use `StandbyDirID(e.Group)`; they copy settings and trust of `NodeID` | the standby directory is `100 + group` | `Entry.InstanceID` (the record has `instance_id` already) names the directory; the settings template stays the home instance's |
| `agent.go` `superviseStandbys` | a standby follows `PrimaryServer`/`PrimaryPort` of the plan only when it is created | compares `primary_conninfo` with the plan every round and fixes it (re-points a standby after a failover, also when its server was down at F8) |
| `instance.IsStandbyDirID` in the start gate exemption (`lease.Gate`) and in `ServiceConnAt` | a directory id >= 100 means standby | the role file's `role` and `origin` |
| `cluster.go` `Stop`, `StopForShutdown`, `Start` (stops standbys first, then instance 1) | instance 1 is the primary | stop every instance, standbys first; start every instance, a primary only through the lease gate; `-stop-instances` writes the stand-down for every primary |
| `cmd/kingresd/main.go` (near lines 191, 278, 432 to 453, 576), `internal.go` (162) | instance 1 for the monitor, the proxy reload, the running check | the home instance for what is per server; the writable instances for what must reach every primary |
| `internal/ops/network.go` `WriteHBA(NodeID)`, `instancectl.go`, `instancerun.go` (start, stop, restart "this server's instance") | one instance | the rules are written to every local instance; the start, stop and restart operations of this server act on all its instances (standbys stopped first); the contract's instance ids are catalog node ids and map to directories through the role files |
| `internal/pgconfig/agent.go` (reads and applies the settings of `NodeID`) | one instance | per instance (the apply already iterates the ids); the "effective settings" of the contract show the home instance |
| membership fan-outs (`database.create`, `database.drop`, `networkAccess.apply`, role and grant replication, `globals/apply`, `POST /apply`) | the callee applies to its one instance | the callee applies to every **writable** local instance (a primary); standbys receive it by replication; a server with no primary applies nothing. "A step per server" now means "a step per server, with every primary on it" |
| `internal/clientproxy` `Env.LocalPort`, `PrimaryResolver` (`own` is the one primary of this server) | one local primary | `LocalPorts` for every local primary; among those whose lease is held the lowest group is chosen (any primary is a correct entry point: the extension routes); the standby entry point lists local instances whose role is standby |
| `internal/store/pgstore/standbys.go` `ManagedStandbyInstanceBase` (instance id `1048576 + id` for standbys not in the catalog) | standbys are not in the catalog | removed with the registration (increment 0) |
| `cmd/kingresd/leadermemory.go` (60 s memory of the last verified leader) and `internal/leader` (2 s cache) | the leader does not change | both are dropped when the local catalog's term changes |

The mapping is the largest single change of part 2 and has no behaviour of its own; it is the first
increment after the catalog registration (section 10) and ships with a test that runs the existing suites
unchanged on a cluster whose group 1 has been failed over by hand through the extension and the markers.

### 3.8 Tables (migration `0011_failover.sql`) and the report

```sql
-- one row per run (the task id is the report id, as for the rebalance)
failover_run(task_id bigint PRIMARY KEY, kind text CHECK (kind IN ('failover','restorePreferredPrimaries')),
  trigger text CHECK (trigger IN ('manual','automatic')), mode text, server_id text,
  requested_by text, options jsonb, expected_term bigint,
  started_at timestamptz, finished_at timestamptz,
  outcome text CHECK (outcome IN ('completed','partial','failed','cancelled')),
  counted boolean DEFAULT false, error text)
-- one row per group of a run
failover_group(task_id bigint REFERENCES failover_run ON DELETE CASCADE, group_id int, seq int,
  state text CHECK (state IN ('pending','frozen','fenced','pinned','promoted','done','failed','skipped')),
  old_server text, old_node_id int, new_server text, new_node_id int, chosen_standby_id bigint,
  decision jsonb,        -- candidates, positions, order, verdict, reasons
  data_loss jsonb,       -- verdict, lastKnownPrimaryLsn, chosenReceiveLsn, asyncDatabases, groupWithout
  term_before bigint, term_after bigint, timeline bigint, lease_wait_ms int,
  unreached jsonb, incidents jsonb, old_primary text, pending_sync_catalog boolean DEFAULT false,
  started_at timestamptz, promoted_at timestamptz, finished_at timestamptz, error text,
  PRIMARY KEY (task_id, group_id))
-- the timeline of the report, one row per fact
failover_event(task_id bigint, seq int, at timestamptz, group_id int, step text, kind text, detail jsonb,
  PRIMARY KEY (task_id, seq))
-- what the leader last knew of each group's primary (updated by the standby check, every 10 s)
group_replication_state(group_id int PRIMARY KEY, primary_server text, flush_lsn text,
  sync_streaming boolean, without_sync boolean, seen_at timestamptz)
-- kingres_cp.standby gets: diverged boolean DEFAULT false
```

- Every write of a run is **one transaction that proves holder, epoch and unexpired lease**, with the same
  helper as `internal/rebalance` (`leased`): a run that lost the lease records nothing (RB-01, RB-02). The
  counter `autoFailover.eventsSinceReset` (a key of `cluster_setting`, a JSON number) is incremented in the
  transaction that records F6 `done` of the first group of an automatic run, guarded by
  `failover_run.counted`, so that a repeated step counts once.
- Exclusion (migration 0011 replaces the function of 0009): `replication.restorePreferredPrimaries` is added
  to the kinds that exclude a rebalance; a new check refuses a second unfinished task of the failover class
  (`KX003`); the records of `instance.stop`, `instance.restart` and `config.restart` are refused while a
  failover task is unfinished and the instance belongs to a group of the run (`KX004`, 409
  `failover_in_progress`).
- **The report** is read from these tables: `GET /failover/reports/{id}` (section 7) gives the run and, for
  each group, `decision`, `dataLoss`, the term before and after, the lease wait, the unreached nodes, the
  incidents, the old primary's state (read live: `rebuildScheduled`, `rebuilding`, `standby`,
  `waitingForServer`, `removed`) and the events. It survives the task, like `rebalance_run`.

### 3.9 Recovery: `kingresd` killed, or the holder changes, at every step

The engine promises: a `done` step never runs again; a step that ran without a recorded `done` runs again; a
task whose holder died is claimed after the lease (30 s) and resumed at the first step not `done`. Every step
above is idempotent; none is `NonIdempotent`. What matters is what is true at the instant of the kill:

| Killed during | State left | The next holder |
|---|---|---|
| F0, F0b | only reads and upserts; maybe lists emptied early | repeats |
| F1 | a freeze that lapses in 120 s | sets it again |
| F2 | markers on `S_old`; the instance stopped or stopping; maybe the lease transferred | repeats: stopped is stopped; `transferTo` is refused from a non-holder and F3 then finds the lease held by `L_old` (its adoption lapses in 13 s when `L_old` died) or free |
| F3 | the previous leader's fence adoption expires at the grantors (at most 13 s) | adopts and waits |
| F4 before the pin | nothing | reads again |
| F4 after the pin | the pin in `failover_group` | uses it; never chooses again |
| F5 between the transfer and the adopt | the grantors name `S_new` as holder, nothing renews yet | `S_new`'s adopt renews within one round; if `S_new` died too, its lease expires and the group has no primary (safe); F3 starts again |
| F6 before `pg_promote` | NEW still a standby | repeats; the handler re-checks `Holds` |
| F6 during `pg_promote` | NEW promoting or promoted | repeats; `already` |
| F6 after `pg_promote`, before the role file or `Attach` | NEW is writable but not routed (the catalog says OLD) and has no lease check | repeats: the handler sees "out of recovery", writes the role file, attaches; a kill of `S_new`'s `kingresd` in this window is covered by the watchdog only after `Attach`; the instance is not routed, so no client is on it |
| F7 anywhere | the extension's own resumability (the change is prepared or committed on some nodes) | repeats; `promoted` or `completed`, then `already` (README sequence 9) |
| F8 | some standbys re-pointed | repeats; the agents also self-heal |
| F9 | the transaction is atomic | repeats (upserts) |
| F10 to F12 | read-only waits | repeat |
| F13 | the old primary may be unscheduled | repeats |
| the leader's server dies | the system database may move (map group) | an ordinary group: the next leader of the same map primary after a restart; if the leader's server is also OLD's or NEW's, the case is a failover of its groups (section 3.4) |
| bootstrap B1 to B4 | journal; the fence lease held by the candidate (lapses if it dies) | restart continues; or another candidate after the stagger |
| bootstrap B5, B6 | NEW promoted or promoting, catalog change possibly prepared | restart continues from the journal; `promote_node` is repeatable |
| bootstrap B7, B8 | catalog done, task not recorded | restart records the task (idempotent by `(kind, subject)` and the journal id) |

A **change of holder** (the leader's engine restarts, the task's lease lapses, another server claims) needs
nothing else: the new holder reads `failover_run` and `failover_group` and the callees' `GET /failover/state`,
never trusts its memory, and the superseded holder stands down at its next conditional write or on `409
fenced`.

### 3.10 The lessons of the reviews, restated as requirements

- RB-01, RB-02: a superseded holder runs no hook and records nothing; a lease that merely ran out while
  nobody claimed it is claimed again and the step repeats. The failover has no `OnFail` that writes rows (its
  cleanup is a rollback of markers before F6 and nothing after it, section 5.3), and `OnFail` runs only after
  the engine recorded the failure itself.
- RBR-01: no bounded hold around a slow call; calls are repeatable or guarded where they run.
- RBR-02: a blip of the system database is retried, not fatal; a call in flight is never abandoned for it.
- RBR-06: an automatic failover stops the rebalance first (section 4.5).
- RBR-07: every quiet end and every loop has a log line that says why, and a bound with an alert.
- S8-02, S9-04 (sequence 2): `promoted` is not done until `already` with `final: true`; `blocked_by` of a
  foreign transaction stops the loop after two identical answers.
- S8-04, S9-02 (sequence 3): nothing is promoted on the map group without the lease; an ordinary group is
  never promoted from a node that is not recognised as MAP (the gate).
- S9-05, S9-06 (sequence 4): OLD is not waited for and leaves no incident; it is still stopped.
- S9-08 (sequence 8): the list is changed before `remove_node` (section 6.5).

## 4. Automatic failover

### 4.1 Where it runs

Two loops, one rule.

- **`failover.Auto`**, on the manager leader only (it starts and stops with the leader's engine, every 5 s):
  looks at every other server and starts the task `server.failover` with `trigger: automatic`,
  `createdBy: system`, actor `system`.
- **The candidate monitor** (section 3.4), on every server that hosts a ready standby of group 0: the same
  evidence and the same settings, for the one case in which the leader is the server that is gone.

The settings are the contract's (`autoFailover.enabled`, `timeoutSeconds`, `maxCount`, `eventsSinceReset`). The
leader reads them from `kingres_cp.cluster_setting` (as `internal/store/pgstore/databases.go` does), a
candidate from the replicated copy.

### 4.2 The trigger: what "dead for the timeout" means, who measures it

- **Measured by the leader's own tracker** (`ha.Liveness.Silence(server)`), on the monotonic clock of the
  leader: a server is a candidate for failover when `State(server) == PeerDead` and
  `Silence(server) >= timeoutSeconds`. The contract says "seconds without heartbeat before auto-failover", so
  the count starts at the last heartbeat heard, not at the transition to `dead` (the README's "What part 2
  must add" says to count from the dead transition, which is 10 s later; **decision**: the contract's words, and the
  difference is stated). The time never passes while the leader's own
  process was stalled (the tracker already gives everybody a fresh window after a stall).
- **Corroborated by a majority.** The leader asks every other server `GET /liveness` and counts the servers
  (itself included) that report the candidate as not `live`. It acts only when a majority of the members
  does, and only when it hears a majority of the members itself. A leader that sees "everybody dead" because
  it is alone does nothing (it could not get a lease anyway); a server that is dead for the leader only (a
  one-way cut, README: "a one-way cut changes nothing for 14 s") is not failed over, because the others still
  hear it.
- **The floor.** The contract allows `timeoutSeconds` 1 to 3600 (0.9.0). `ha-phase5.md` says "minimum 30 here
  because of the lease durations". **Decision:** the minimum becomes 30 in the contract 0.10.0 (section 7;
  nothing was released, so this is a correction), and the leader additionally uses
  `max(timeoutSeconds, 3 x failure-timeout)` so that a cluster with a longer failure timeout is not failed
  over faster than it can tell a failure from a pause. This also guarantees that the old lease has run out
  (13 s) before the automatic task starts, which is why section 2.5 says the lease wait is zero for it.
- **What it fails over.** Every group whose primary the dead server runs, in one task. A group without a ready
  standby is not failed over (nothing to promote): the alert `failoverBlocked` says "group g has no standby to
  promote" and `unavailableBuckets` and `serverDown` already say the rest. A server that runs only standbys is
  not failed over (nothing to promote); `groupUnderReplicated` and the grace of part 1 apply.

### 4.3 What stops it

| Condition | What the loop does |
|---|---|
| `enabled` is false | nothing; the alerts `serverDown` and `unavailableBuckets` stand |
| `eventsSinceReset >= maxCount` | nothing, and raises `autoFailoverLimitReached` once (it clears when the counter is reset); the panel tells the operator to fail over by hand |
| another task of the failover class is unfinished | waits (tries again in 5 s); a task that is `paused` waiting for an operator keeps the others waiting, and the alert of that task says so |
| the leader does not hear a majority | nothing |
| the leader and a majority disagree on liveness | nothing |
| a failover of this group ended less than 5 minutes ago (decision: a fixed cool-down against ping-pong; a manual failover is not subject to it) | waits; `failoverBlocked` after 60 s |
| the data-loss rule of section 2.6 | starts the task; F4 **asks** and raises `failoverNeedsDecision` |
| `lease_still_held`, `quorum_unavailable` at F3 | the task fails or asks as F3 says; the loop does not start another for this server for 60 s (a growing back-off up to 10 min), and the failed attempt is not counted |
| a rebalance is pending or running | section 4.5 |
| a server of the cluster runs another extension version, or the lease settings differ | F0 refuses (`version_mismatch`, `lease_settings_differ`); `failoverBlocked` |

### 4.4 The counter

- `eventsSinceReset` counts **automatic failovers that promoted at least one group**: it is incremented once
  per automatic task, in the transaction that records F6 `done` of its first group (`failover_run.counted`
  makes a repeated step count once). A task that fails before F6, or is refused, counts nothing. The loss of
  one server whose groups are several is **one** event, as `ha-phase5.md` says. Two servers lost together are
  two events (two tasks); with the default `maxCount` of 1 the second waits for an operator, which is the
  intended brake.
- **Reset.** By the new operation `resetAutoFailoverCounter` (role `kingres_admin`, as the other settings
  changes; audit action `resetAutoFailoverCounter`), and by a rebalance that completes (the contract says
  "reset by a rebalance": the step "Write the report" of `internal/rebalance` sets the key to 0 in the same
  transaction when the outcome is `completed`; today nothing does, section 1.5). A time-based reset does not
  exist.
- A manual failover does not count and is not limited.

### 4.5 A rebalance is running (RBR-06)

Migration 0009 refuses to create a `server.failover` task while a rebalance is `pending` or `running`. A
rebalance with a stuck move (a dead target, `RecoverWait` of 10 minutes, then retries with a back-off) could
hold an automatic failover back for as long as it runs. **Decision:** a failover is more urgent than a
rebalance, and the loop (and the manual operation) **stop the rebalance first**:

1. `rebalance.Service.StopForFailover(ctx, serverID)` (new): the same flag as `Stop`, with `stopped_by =
   'safetyRule'` and the paused reason "stopped for the failover of server `<id>`" (`RequestStop` of
   `internal/rebalance` takes the stopped-by value as an argument; no new contract value is needed: `stoppedBy`
   is `safetyRule`). The
   run asks the extension to cancel the move in flight (`cancel_bucket_move`: before the switch it undoes the
   move; a switching move cannot be cancelled and is asked again until it has ended, so the switch is never
   interrupted).
2. The caller waits until the task is `paused` (or finished): for a manual request 60 s, then 409
   `rebalance_in_progress` with the text "a stop was requested; repeat in a minute"; for the automatic loop
   up to 12 minutes (the stuck move's own `RecoverWait` of 10 minutes plus the stop), logging every minute
   why, raising `failoverBlocked` ("the rebalance does not stop: a move is switching") after 5 minutes, and
   never giving up: the rebalance cannot hold a failover for ever because its move ends by the extension's
   own bounds (`node_timeout`).
3. A paused rebalance does not exclude the failover (migration 0009), and migration 0009 refuses to resume it
   while the failover task is unfinished. When the failover is done the operator starts the rebalance again
   (`startRebalance` continues the same task); nothing resumes it automatically. If the failover moved
   primaries, the rebalance's next plan is made from the new placement (it plans again before every move).

In the map-group case the rebalance is paused by the bootstrap after the promotion (section 3.4, B7).

### 4.6 Partitions: a leader that cannot see a primary which clients still reach

The lease decides, not the leader's opinion. The cases, with the settings of 2.2 (`T0` is the start of OLD's
last successful round):

| Partition | What happens | Why it is safe |
|---|---|---|
| the leader cannot hear OLD's server; OLD's server hears a majority; clients reach OLD | the leader's tracker says `dead` after 10 s and the timeout passes; the trigger needs a majority to agree: the servers that hear OLD say `live`, so the loop does not start. If it starts anyway (manual request), F3 is refused for as long as OLD's holder renews at a majority and ends with `lease_still_held` | OLD keeps its lease and stays the only primary; nobody was promoted |
| OLD's server is cut off from a majority; clients still reach OLD | OLD's holder fails its rounds; at `T0 + 7 s` it fences itself (marker, immediate stop); the watchdog does the same if `kingresd` is dead or hung. The majority side sees the server `dead`, the timeout passes (at least 30 s), F3 takes the lease at once (the grants expired at `T0 + 13 s` at the latest) | OLD's last act precedes the first possible grant by at least `2M`; a client that kept writing to OLD is acknowledged only until `T0 + 7 s` (`TestPartitionedPrimaryWithStandbyStopsBeforeANewHolderCouldBeGranted`: last write 3.4 s after the cut, lease free after 7.4 s, with D=6 s, M=2 s) |
| the leader is on the minority side | it does not hear a majority: the trigger does not fire; a manual request answers 409 `quorum_unavailable` | no lease can be had without a majority |
| the leader and OLD's server are on the minority side, the other servers (a majority) on the other | the leader cannot start a failover (it hears no majority) and the primaries on its side fail their rounds and fence themselves, the map primary among them. On the majority side the candidate monitors see the leader silent for the timeout with a majority agreeing, and promote the map group (section 3.4); the new leader then fails over the ordinary groups whose primaries were on the minority side, after the timeout | no group has two writers: each primary on the minority side stops at its own `T0 + 7 s`, and every promotion on the majority side needs a lease that a majority grants only after that |
| the map primary's server is cut off from a majority; the other side has its standby | the candidate waits for the old lease, obtains the group-0 lease, promotes, and `promote_node` needs a majority of the other groups. The old map primary fences itself at `T0 + 7 s` | two majorities needed on the candidate's side; OLD's side has neither |
| one-way cuts, a paused leader (SIGSTOP), a step of the wall clock | no judgement changes (part 1's tests); a resumed leader's task stands down on its next conditional write | monotonic clocks, the epoch |

### 4.7 Flapping, two groups at once, a whole server, the leader's server

- **Flapping.** A server that comes back before F3 takes the lease makes F3 fail with `lease_still_held` or the
  tracker reset `Silence`: the event is not counted and the loop backs off. A server that fails again after a
  failover is a different failure: the cool-down of 5 minutes and `maxCount` bound the sequence. There is
  **no automatic failback**: a group stays where it is until `restorePreferredPrimaries`.
- **Two groups lose their primary at once.** If it is one server, one task and one event; the groups are done
  in order (map group first, then ascending), each with its own F0-to-F13 block, so the cost is the time of a
  group times their number (section 2.5). If they are two servers, two tasks one after the other, two events.
- **The whole server is lost.** Its primaries are failed over by the one task; the standbys that lived on it
  are marked gone at F0b (so `promote_node` does not wait for them); `groupUnderReplicated` is raised for the
  groups that lost a standby; the reconcile loop places replacements (section 6.5).
- **The manager leader's own server is lost.** It is the map group's primary: section 3.4. The candidates'
  monitors act after the same timeout and the same majority of opinions; the new leader then also fails over
  the ordinary groups of that server (the recorded task continues with their blocks), after the map group.
- **A cluster of two servers.** No majority without both: no automatic failover (`ha-phase5.md`). The surviving
  primaries fence themselves; the operator's way out is outside this part (open point 11.4).

### 4.8 What it records and raises

The task (visible in `GET /tasks`, kind `server.failover`, `createdBy` `system`); the audit event
`autoFailover` (actor `system`, resource the server, the groups and the result in the details) when it starts
and `failoverGroupPromoted` per group; the alert `autoFailoverTriggered` (info) when it ends; the alerts of
2.6 and 4.3; the counter. `GET /cluster` shows `autoFailover` with the counter, the last event's time and the
running task (section 7).

## 5. Planned switchover and restoring preferred primaries

A **switchover** is a failover with a healthy OLD: the contract's mode `graceful` (it "waits for the standbys
to catch up and requires a `live` server"). It is the extension's sequence 5; the point is that no
acknowledged commit is lost for any durability, because OLD stops writing, hands over its WAL, and NEW is
verified to have all of it before it is promoted.

### 5.1 What the request can say

`failoverServer` with `mode: graceful`: for each group the server is primary of, or only the groups of
`shardGroupIds`, to the standby named by `targets` (a server id: a server has at most one standby of a group)
or, if none is named, to the best standby by the order of 3.6 (the preferred server first when tied). The
verdict of 2.6 is always `none` for a switchover that completes: it either completes with no loss or rolls
back.

### 5.2 The steps of a group

These replace F1 to F6 of section 3.3 for `mode: graceful`; F7 to F13 are the same.

| Step | What | Answers and reactions |
|---|---|---|
| S0. Check (task) | F0 with: NEW streams from OLD (`standby_status()` row `streaming`, `sync_state` `sync` or `quorum`, or a lag of 0 within 10 s); NEW is named in OLD's `synchronous_standby_names` or is caught up; the checks of F0 | NEW not streaming: 409 `group_degraded` (the contract reserves the code for a later increment; this is its first use); no ready standby: `no_replica_available`; `failover_needs_decision` is never asked, because a switchover has no data-loss option (the operator uses `hard`) |
| S1. Freeze | F1 | as F1 |
| S2. Stop the writes | `POST /failover/quiesce {group, task}` on OLD's server: (1) the client entry point of that server stops sending new connections to the instance (`clientproxy` hold flag, per group); (2) the access rules of the instance are replaced by the **hold rules**: only the replication connections of the standbys and the private socket of `kingresd`, written with `WriteHBA` and reloaded (so neither a client nor another node of the extension can open a new session); (3) every other session is ended with `pg_terminate_backend`, repeated every second until none is left except `kingresd`'s (bound 30 s); (4) `default_transaction_read_only = on` is not trusted (a session can override it) and is not used | `quiesced` go on; sessions remain after 30 s: **repeat** the termination, then fail `cannot_quiesce` after 2 minutes and roll back (5.3) |
| S3. Let distributed transactions end | wait until `SELECT count(*) FROM pg_prepared_xacts WHERE gid LIKE 'kingres\_h%'` is 0 on every primary (`GET /failover/state`), calling `recover_prepared_transactions()` on MAP every 10 s (extension sequence 5, step 1) | zero: go on; not zero after 60 s: roll back (5.3), `prepared_transactions_pending` |
| S4. Mark | `POST /failover/demote {mark: true, mode: "fast"}` on OLD's server calls `SELECT kingres.switchover_mark()` and keeps **M**; before the stop, `POST /failover/verify {mark: M}` on NEW's server must answer `replayed` within 10 s (README sequence 5, step 2) | answer `mark`; an error here, or NEW does not replay M: roll back, `standby_behind` |
| S5. Stop OLD cleanly | the same call: `Relinquish`, then `pg_ctl stop -m fast` (never smart: README sequence 5 step 3; bound 120 s, then a second try, then fail and roll back: it is not turned into `-m immediate`, because an immediate stop is not a clean one and the verification below would be the only protection), confirm not running, read **X** ("Latest checkpoint location") and the state ("Database cluster state") from `pg_controldata`, then `release` the lease (the instance is stopped and the marker is on disk) | `clean: true`, `mark`, `checkpoint`, `clusterState: "shut down"`; any other state: the stop was not clean, roll back (5.3) and fail `switchover_unproven` without asking NEW |
| S6. Verify | `POST /failover/verify {mark: M, checkpoint: X, clusterState}` on NEW's server; polled every second for 30 s while NEW may still be receiving: `switchover_verify(X, state)` must be `t` | `t`: go on. Stays `f` for 30 s: **do not promote** (the extension: "NEW is missing what OLD wrote"): roll back (5.3) and fail `standby_behind`. `KS006`: it can never be proven (X is not a shutdown checkpoint at the end of NEW's WAL): **do not promote**, roll back (5.3) and fail `switchover_unproven`; a switchover never turns itself into a failover |
| S7. Take the lease | `POST /failover/lease {group, role: "holder"}` on NEW's server: the lease was released at S5, so the first round obtains it; if it was not released the step waits as F3 | held: go on |
| S8. Promote | F6 (`POST /failover/promote`), the same handler, with the lease check and the empty list; after this the point of no return is passed | as F6 |

Then F7 (`promote_node`: `unreached` is `[<OLD>]` for the stopped OLD, extension `t/073` A), F8, F9, F10, F11,
F12 and F13 as in section 3.3. The old primary becomes a standby by **adoption**, not rebuild (6.3).

**What the operator sees while it runs.** The task shows the steps by name with the group number; the status
resource (`GET /failover`) shows for the group in flight: `phase` (`quiescing`, `waitingForTransactions`,
`stopping`, `verifying`, `promoting`, `followingCatalog`, `finishing`), the time since the writes stopped (the
only time clients feel: the write pause of a switchover is S2 to F12, a few seconds when OLD stops quickly),
and whether the task can still be cancelled. The panel shows these (section 8).

### 5.3 Cancel and roll back, before the point of no return

The point of no return is F6/S8, `pg_promote` on NEW (section 3.3 F6). Until then the data of OLD is the
cluster's truth and can be started again. A failure of S0 to S6, a cancel (`cancelTask`, which the engine
guard allows while the task is not past S8), or a failed verification rolls back, and the rollback is the
task's `OnFail`-free cleanup: it runs inside the step that failed, as the last act of that step, so that it
runs once and is recorded under the task's lease like any step.

1. S8 not reached, OLD stopped: `Unrelinquish(g)` on OLD's server, **write the wait marker** (not the
   fence marker), restore the access rules, unquiesce. The lease manager starts the instance when it wins
   the lease (README decision 7: a primary with an enforced lease is not started before it holds one). The
   lease was released at S5, so the first round wins it unless someone else took it; nobody did, because
   NEW's server adopts only at S7.
2. S7 reached but S8 not: `Transfer` the lease back to OLD's server (or let it lapse), then as 1.
3. S2 to S4 (OLD still running): restore the rules, lift the hold flag, unfreeze.
4. The task is recorded `failed` (code `standby_behind`, `switchover_unproven`, `cannot_quiesce`,
   `prepared_transactions_pending`, `cancelled`) with the report; the alert `taskFailed` (for the first
   four), not for a cancel.
5. After S8 the cancel is refused (409 `task_not_cancellable`) and the task rolls forward only.

A rollback never runs after S8, and never because a hook ran: the task engine's rule (RB-01) is kept.

### 5.4 One group at a time

The groups of one server are done one after the other, map group first for a failover and **last** for a
switchover and for `restorePreferredPrimaries` (the leader moves with the map group; doing it last means the
earlier work was done on a stable leader and the final hand-over is the bootstrap's). Between two groups the
task waits for the previous group to be healthy (the extension's definition, "Postconditions"): NEW the
primary on every node, its standbys streaming, the list set, `check_cluster()` showing nothing for the group;
bound 5 minutes, then it **asks** (a switchover that leaves a group degraded does not start the next one).

### 5.5 Restoring the preferred primaries (sequence 7)

Task `replication.restorePreferredPrimaries`, subject `replication` / `preferred-primaries` (the contract's
table), created by `restorePreferredPrimaries`; it is a switchover per group in the other direction, with the
preferred server's standby as NEW.

1. **Find the groups** whose primary is not on its preferred server (`ShardGroup.preferredPrimaryServerId`:
   the server of the group's first node by id, `internal/store/pgstore/topology.go`; the extension keeps node
   ids across `promote_node`, so the home node stays the first). A group already at home, or whose preferred
   node has no ready standby (it was never rebuilt, or its server is `dead`), is **skipped** with the reason
   in the report (`preferredNotReady`); the task does not fail for it.
2. For each remaining group, the steps S0 to S8 and F7 to F13, with `targets` the preferred server. The old
   primary is then the server that took over; it becomes a standby by adoption (6.3). Because the preferred
   node's data was rebuilt as a standby of the current primary, it already streams and is named in the list
   (README sequence 7: "the term goes up by one; `unreached` lists the stopped NEW"). After each group the
   leader waits for health (5.4).
3. The report is the same schema as the failover's (section 7), with `kind: restorePreferredPrimaries`. The
   task is excluded from a rebalance and from `server.failover` by migrations 0009 and 0011.
4. `restorePreferredPrimaries` never promotes a standby that is behind: S6 is the same verification, so a
   preferred node that is not caught up makes that group fail the step (rolled back), the report says
   `standby_behind`, and the task goes on to the next group (`partial` outcome) after asking nothing: failing
   back is not urgent, and the operator reads the report.

## 6. The old primary afterwards

### 6.1 It comes back by itself after a failover

What the fenced server does when it starts (part 1's rules, per group after 3.7): its `kingresd` reads the
marker files: `fenced.<g>` and `relinquished.<g>` exist (or the instance was stopped by the watchdog, which
writes the same marker), so the start gate refuses to start the instance as a primary; if the instance was
left running while only `kingresd` restarted, the first lease round fails (NEW's server holds the lease at a
majority), `Check` fences it. **It cannot start as a primary**: there is no path that removes the marker
except a won lease (a group that is not the group's holder cannot win) or the adoption of 6.3.

Who notices and who acts: the leader's heartbeats hear the server again; the reconcile loop of
`standby.Leader` (every 10 s) finds, for the group, the record that F9 inserted: a standby `rebuilding` with
`diverged`. It calls `POST /standby/rebuild {entry, diverged: true}` on the server of the old primary. The
server's own agent also checks, every round, that a catalog node of this server whose role is `standby`
(learned from `kingres.catalog_status` and the plan) is not running as a primary: that is the
`standby_not_in_recovery` of the extension and raises `oldPrimaryRunning` if the marker logic ever failed.

### 6.2 Rebuilding it: `pg_rewind`, or a new base backup

The agent's `Rebuild` is extended (the code exists for a standby; nothing called it with `diverged`):

1. Make sure the instance is stopped; keep the fence markers (they say "not a primary").
2. **Measure the divergence** before it is destroyed: `pg_controldata` of the stopped instance (its last
   checkpoint and timeline), then `pg_rewind --dry-run --progress` against NEW (the replication connection
   with the certificate): the point where the histories parted, the bytes that would be rewound, the WAL of
   OLD beyond the point. They go into `failover_group.old_primary` as `{divergenceLsn, oldEndLsn,
   rewindBytes, method}`. The extension's own guidance ("compare what it holds with `pg_rewind --dry-run`")
   is made automatic; the writes that OLD took and NEW does not have **are discarded** by the rewind and the
   report says how large they were. Keeping a copy of the old data directory is not done (disk), but a request
   option `keepOldDataDirectory` is proposed as an open point (11.6).
3. **Rewind.** `instance.Manager.Rewind` runs `pg_rewind --no-ensure-shutdown`, which refuses a target that
   was not shut down cleanly; a fenced primary was stopped with `-m immediate`. Part 2 therefore lets
   `pg_rewind` run its own single-user crash recovery for a target that was stopped immediately (the program's
   default `--ensure-shutdown`; the instance's `shared_preload_libraries` are in its configuration, so the
   crash recovery loads the extension as the instance would). The instance has `--data-checksums`, so
   `pg_rewind` is possible. The `source-server` is NEW over the replication/superuser certificate connection.
4. **If it cannot be rewound** (`pg_rewind` fails: WAL that NEW needs from the divergence point is gone, the
   timelines are unrelated, the crash recovery cannot run): the agent deletes the directory's data and takes a
   new base backup (`pg_basebackup -X stream -S <slot>`) as for any new standby (the existing fallback). The
   report says `method: basebackup`. If the base backup fails (disk full, network): the record goes back to
   `rebuilding` with the error, is tried again after 30 s (the existing rule), and after 10 minutes the alert
   `oldPrimaryNotRebuilt` (warning) is raised.
5. Write `kingres-role.json` (`role: standby, origin: copy`), the managed `primary_conninfo` and
   `primary_slot_name` for NEW, `standby.signal`; create the slot on NEW; **remove `fenced.<g>`,
   `relinquished.<g>` and any `waitlease.<g>`** (`lease.FencedMarker`: the README's "remove
   `lease.FencedMarker` when a server becomes a standby"); `Unrelinquish`; start it as a standby; wait for
   `streaming` (bound 5 minutes).
6. The record becomes `ready`; the plan is pushed; NEW's agent names it in `synchronous_standby_names` when it
   streams; `check_cluster()` loses `standby_unreachable`; `catalog_status` on it shows NEW's term and
   `superseded = false` (extension sequence 6, steps 3 and 4). If the cluster had `unreached` nodes, the loop
   runs `SELECT kingres.sync_catalog()` on MAP now (and clears `pending_sync_catalog`).

**The writes an unfenced old primary took** (extension precondition 1): when OLD was in `unreached`, ran as a
primary and clients wrote to it, those writes are acknowledged and then gone after the rewind. They cannot be
recovered by the product; the report states the byte window; the critical alert `oldPrimaryRunning` was
raised at F7; and `keepOldDataDirectory` (open point 11.6) would let an operator keep the evidence.

### 6.3 Adoption after a clean stop (switchover, restore)

`POST /standby/adopt {entry, clean: true}` on the old primary's server after S8 and F7 (OLD stopped cleanly:
every WAL record is on NEW, `switchover_verify` proved it): no divergence, so no `pg_rewind`: write
`standby.signal`, the managed `primary_conninfo` and slot, the role file, restore the access rules,
`Unrelinquish`, remove the markers, start. It follows the timeline switch by itself (extension sequence 6,
step 1: "it follows the timeline switch"). If it does not stream within 60 s (a history mismatch), the agent
falls back to 6.2 (`diverged`), which also works on a cleanly stopped instance.

### 6.4 An old primary that cannot be stopped, or runs although it should not

- **Cannot be stopped** (F2): the holder stopped renewing, so the lease lapses; the manager's `Check` retries
  the stop every 5 s; the critical alert `oldPrimaryNotStopped` names server, group and instance; the group
  is protected by the lease only as long as the process does not write: a process that cannot be killed
  (kernel wait) is not writing, and one that is writing can be killed. The failover goes on because the lease
  is the proof, and F7's last-resort demote tries again.
- **Runs although it should not** (it was `unreached` and alive, or its marker was removed by hand): F7's
  check demotes it; `check_cluster()` shows `standby_not_in_recovery`; the critical alert `oldPrimaryRunning`.
  The task never rebuilds an instance it has not first confirmed stopped.

### 6.5 It never comes back

- The group has one standby fewer. `groupUnderReplicated` (existing, raised by the leader after three checks
  that fewer standbys stream than the replica count) says so, with the group, and `groupWithoutSynchronousStandby`
  if none is left.
- The reconcile loop would place a replacement on another server at once (the placement rule of part A). To
  avoid building a full copy for a server that merely reboots, **decision**: the old primary's record counts
  as a planned standby (`waitingForServer`) for `standby-replace-after` (new setting, default 30 minutes,
  in `internal/config/ha.go` and the table of configuration keys of the README); after it the record is removed and a replacement is
  created by the ordinary reconcile. The alert `oldPrimaryNotRebuilt` (warning) is raised after 10 minutes of
  `waitingForServer`.
- **Removing it (extension sequence 8).** Whether by the operator (`removeServer` of the dead server, which
  the contract allows once it has been failed over) or by the 30-minute rule: (1) the primary's list is
  changed first: its agent drops the standby from the planned set, so `synchronous_standby_names` no longer
  names it (a name that nobody serves makes every commit that writes in the group wait, `KS005` for a
  distributed one); (2) `SELECT kingres.remove_node(<standby node id>)` on MAP; answers: nothing = done;
  "node N cannot commit: its synchronous standbys do not confirm" = step 1 was not done (repeat after it);
  "cannot remove node N: shard group G still has the standby node M" = a primary with standbys, wrong order;
  "node N does not exist" = done already; (3) `standby_status()` has no row, `check_cluster()` no `standby_*`
  row for it. The membership code's step "Remove the standbys of the server" calls this order (it exists for
  the standbys of a server; part 2 adds the extension call to it, because the registration of increment 0
  puts the rows in the catalog).
- **When it is rebuilt but its server is later removed**: the same.

## 7. The contract

The contract is `kingres/api/openapi.yaml`, 0.9.0, revised by hand with `scripts/check-contract.mjs` as the
mechanical check (README of the contract: every authenticated operation lists 401, 403, 422, 503 and 405;
every operation with a body or query parameters lists 400; every 202 has a `Location` header and a `Task`
body; `AuditAction` covers every mutating operation; no `required` property of a request body is `readOnly`).
Part 2 is **contract 0.10.0**, a minor bump like every revision since 0.2.0, with one correction that is not
additive (the floor of `timeoutSeconds`; nothing was released, as for the corrections listed under 0.9.0
"Breaking"). The panel's generated files (`src/api/schema.d.ts`, `operations.generated.ts`, `npm run
api:generate`) follow the contract and must be regenerated; the web README says they are one revision behind
today.

### 7.1 What 0.9.0 has, and what it lacks

| Item | In 0.9.0 | Verdict |
|---|---|---|
| `failoverServer` (`POST /servers/{serverId}/failover`, role `kingres_ops`), 202 `AcceptedTask`, 409, 422 | yes | **needs a change**, compatible: `FailoverRequest` gets `shardGroupIds`, `targets`, `allowDataLoss`, `dryRun`, `comment`; a 200 answer `FailoverPlan` for `dryRun`; the text says that `graceful` is the planned switchover; new 409 and 422 codes (7.2) |
| `FailoverRequest` `{mode}` | yes | compatible additions as above |
| `restorePreferredPrimaries` (`POST /replication/restore-preferred-primaries`, no body) | yes | **needs a change**, compatible: an optional body `{shardGroupIds, dryRun}`; 200 `FailoverPlan` for `dryRun` |
| task kinds `server.failover`, `replication.restorePreferredPrimaries`; subjects `server` / `<id>` and `replication` / `preferred-primaries` | yes | as needed; the README table rows get the step names |
| `Task.cancellable` | yes ("true for a pending, running or paused task that the engine runs") | text change: false for `server.failover` and the restore task once a standby has been promoted (the point of no return) |
| `TaskStep` | yes, `name`, `state`, `detail`, `itemsDone`, `itemsTotal` | **lacks** `shardGroupId` (optional, nullable): so that a client groups the steps of a multi-group task |
| `ClusterSettings.autoFailover` (`enabled`, `timeoutSeconds` 1 to 3600, `maxCount` 1 to 100, `eventsSinceReset` read-only) | yes | **change**: `timeoutSeconds` minimum 30 (a correction; `ha-phase5.md` said 30); **lacks** `lastEventAt` (nullable date-time, read-only) |
| `ClusterOverview.autoFailover` (`enabled`, `eventsSinceReset`, `maxCount`) | yes | **lacks** `limitReached` (boolean) and `lastEventAt` |
| `ClusterOverview` | no failover member | **lacks** `failover` (running, task, server, phase) as `rebalance` has |
| reset of the counter | contract text: "reset by a rebalance" | **lacks** the operation `resetAutoFailoverCounter` |
| failover status and report | none (the rebalance has `getRebalanceStatus`, `listRebalanceReports`, `getRebalanceReport`) | **lacks** `getFailoverStatus`, `listFailoverReports`, `getFailoverReport` |
| `getReplicationTopology`: `ShardGroup` has `health`, `primary`, `standbys` (`syncState`, `lagBytes`, `lagSeconds`), `slots`, `lease` (`GroupLease`) | yes | **lacks** `failover` (`FailoverReadiness`): whether a failover of the group would be possible now |
| `AlertKind`: `serverDown`, `serverSuspect`, `unavailableBuckets`, `autoFailoverTriggered`, `autoFailoverLimitReached`, `mapGroupUnavailable`, `leaseLost`, `groupWithoutSynchronousStandby`, `groupUnderReplicated`, `taskFailed` | yes | as needed for those; **lacks** seven kinds (7.2) |
| `AlertSubject` (`serverId`, `shardGroupId`, `taskId`, ...) | yes | as needed; the new kinds use `serverId`, `shardGroupId`, `taskId`; `taskId` is no longer only for rebalance (text) |
| `AuditAction`: `failoverServer`, `autoFailover`, `restorePreferredPrimaries`, `cancelTask`, `leaseLost`, `leaseFenced`, `leaseAcquired`, `standby*` | yes | as needed; **lacks** `resetAutoFailoverCounter`, `failoverGroupStarted`, `failoverGroupPromoted`, `failoverGroupFailed`, `oldPrimaryRebuilt` (the first is an operation id, the others are the closed list of non-operation events) |
| `ProblemCode`: `failover_in_progress`, `rebalance_in_progress`, `operation_in_progress`, `no_replica_available`, `node_state_invalid`, `version_mismatch`, `task_not_cancellable`, `group_degraded` (reserved), `leader_unavailable` | yes | as needed; **lacks** `failover_needs_decision`, `quorum_unavailable`, `lease_settings_differ` |
| `Server.instances`, `ServerStatus`, `Server.lastHeartbeatAt` | yes | as needed; the instances list gains the standby instances once they are in the catalog (increment 0); `server_owns_buckets` already ignores standbys (0.9.0) |
| the switchover | `failoverServer` mode `graceful` ("waits for the standbys to catch up and requires a `live` server") | **exists** for a whole server; **lacks** the choice of group and target: covered by `shardGroupIds` and `targets` of the request, so **no new operation** |
| `TaskError.code` values | free text codes | the failover's codes are documented in the README table (7.4) |

### 7.2 What to write in the YAML

**Operations** (all under tag `Replication` unless noted; every one lists the uniform responses):

1. `getFailoverStatus`: `GET /failover`, role `kingres_readonly`, 200 `FailoverStatus`. Modelled on
   `getRebalanceStatus`: it answers on any server, **also during the bootstrap of the map group** (from the
   journal, 7.6), so a client can follow a failover when no leader exists.
2. `listFailoverReports`: `GET /failover/reports`, role `kingres_readonly`, query `limit` (1 to 100, default
   25) and `cursor`, 200 `FailoverReportPage` (`items`, `nextCursor`), newest first; 400 for a bad cursor.
3. `getFailoverReport`: `GET /failover/reports/{reportId}`, path `reportId` int64 (the id of the task),
   role `kingres_readonly`, 200 `FailoverReport`, 404 when there is no report.
4. `resetAutoFailoverCounter`: `POST /settings/cluster/auto-failover/reset`, role `kingres_admin`, no body,
   200 `ClusterSettings` (the settings after the reset), 409 `operation_in_progress` never (it is immediate).
   Audit action `resetAutoFailoverCounter`.
5. `failoverServer` extended, `restorePreferredPrimaries` extended (7.1).

**Schemas** (names, members, types):

- `FailoverRequest` (extended): `mode` (required, `hard` or `graceful`); `shardGroupIds` (array of int32,
  `minItems` 1, optional: the groups whose primary runs on the server that are failed over; default all);
  `targets` (array of `FailoverTarget`, optional); `allowDataLoss` (boolean, default false: the operator
  accepts the loss the plan states; ignored by a switchover); `dryRun` (boolean, default false: compute and
  answer the plan, change nothing); `comment` (string, 0 to 500, optional, recorded in the audit event).
- `FailoverTarget`: `shardGroupId` (int32), `serverId` (`ServerId`): the standby of that group on that server
  (a server has at most one standby of a group).
- `RestorePreferredPrimariesRequest`: `shardGroupIds` (optional), `dryRun` (boolean).
- `FailoverPlan` (the 200 of a dry run): `generatedAt`, `kind` (`failover` or `restorePreferredPrimaries`),
  `mode` (`hard`, `graceful` or `restore`), `serverId` (null for restore), `canProceed` (boolean),
  `blockers` (array of `FailoverBlocker`), `willStopRebalance` (boolean), `groups` (array of
  `FailoverGroupPlan`).
- `FailoverBlocker`: `code` (string: the problem code the request would get, or `failoverNeedsDecision`),
  `shardGroupId` (nullable), `message` (string).
- `FailoverGroupPlan`: `shardGroupId`, `isMapGroup`, `fromServerId`, `toServerId` (nullable when no
  candidate), `candidates` (array of `FailoverCandidate`), `dataLoss` (`DataLossAssessment`),
  `estimatedSeconds` (integer, nullable), `writesPausedSeconds` (integer, nullable; only for a switchover),
  `steps` (array of strings: the step names, English), `movesTheManager` (boolean: the map group).
- `FailoverCandidate`: `serverId`, `standbyId` (int64), `readable` (boolean), `eligible` (boolean),
  `ineligibleReason` (string, nullable), `receiveLsn`, `replayLsn` (strings in PostgreSQL's `X/Y` form,
  nullable), `lagBytes` (int64, nullable: against the last position of the primary the leader knew),
  `syncState` (`ReplicationSyncState`, nullable, from the last check), `chosen` (boolean).
- `DataLossAssessment`: `verdict` (`none`, `asyncOnly`, `possible`, `unknown`), `message` (string, in
  words), `lastKnownPrimaryLsn` (nullable), `chosenReceiveLsn` (nullable), `lastKnownAgeSeconds` (nullable),
  `groupRanWithoutSynchronousStandby` (boolean), `asyncDatabases` (array of strings), `unreadableServerIds`
  (array of `ServerId`).
- `FailoverStatus` (modelled on `RebalanceStatus`): `state` (`idle`, `running`, `paused`, `failed`),
  `phase` (nullable string: `checking`, `waitingForLease`, `choosing`, `promoting`, `followingCatalog`,
  `repointing`, `finishing`, and for a switchover `quiescing`, `waitingForTransactions`, `stopping`,
  `verifying`; for the map group `bootstrap`), `kind`, `trigger` (`manual`, `automatic`), `taskId` (nullable:
  null during a bootstrap), `serverId` (nullable), `startedAt` (nullable), `pausedReason` (nullable string),
  `needsDecision` (array of `FailoverBlocker`: the groups waiting for the operator), `groups` (array of
  `FailoverGroupProgress`), `cancellable` (boolean), `lastReport` (`FailoverReportSummary`, nullable).
- `FailoverGroupProgress`: `shardGroupId`, `state` (`pending`, `frozen`, `fenced`, `pinned`, `promoted`,
  `done`, `failed`, `skipped`), `phase` (nullable), `fromServerId`, `toServerId` (nullable),
  `writesPausedSince` (date-time, nullable: set during a switchover from the quiesce to the end of the
  catalog change), `message` (nullable).
- `FailoverReportSummary`: `id`, `kind`, `trigger`, `serverId` (nullable), `outcome` (`completed`,
  `partial`, `failed`, `cancelled`), `startedAt`, `finishedAt`, `requestedBy`, `totals` (`groupsTotal`,
  `groupsDone`, `groupsFailed`, `groupsSkipped`, `durationSeconds`), `dataLossPossible` (boolean), `error`
  (nullable string).
- `FailoverReport`: `allOf` the summary and `mode`, `options` (the request as recorded, `FailoverRequest`),
  `groups` (array of `FailoverGroupReport`), `events` (array of `FailoverEvent`).
- `FailoverGroupReport`: `shardGroupId`, `outcome` (`done`, `failed`, `skipped`), `skipReason` (nullable),
  `fromServerId`, `toServerId`, `termBefore`, `termAfter` (int64, nullable), `timeline` (int64, nullable),
  `leaseWaitMs` (nullable), `durationSeconds`, `candidates`, `dataLoss`, `unreachedServerIds`, `incidents`
  (array of `FailoverIncident`), `oldPrimary` (`OldPrimaryReport`), `error` (nullable).
- `FailoverIncident`: `kind` (the extension's names: `prepared_rolled_back_after_failover`,
  `participant_lost_committed_transaction`, `foreign_prepared_transaction`, ...), `message`.
- `OldPrimaryReport`: `state` (`rebuildScheduled`, `rebuilding`, `standby`, `waitingForServer`, `removed`,
  `failed`), `method` (`clean`, `pgRewind`, `baseBackup`, nullable), `divergenceBytes`, `rewindBytes`
  (int64, nullable), `message` (nullable).
- `FailoverEvent`: `at`, `shardGroupId` (nullable), `step`, `kind`, `message`.
- `FailoverReadiness` (new optional member `ShardGroup.failover`): `possible` (boolean), `candidateServerId`
  (nullable), `dataLossVerdict` (as above, nullable), `blockers` (array of `FailoverBlocker`), `checkedAt`.
  It is computed by the leader from the plan, the last standby check and the liveness view, without a
  network call per request; it never says `possible` while a failover is running.
- `TaskStep.shardGroupId` (int32, nullable, optional). `ClusterSettings.autoFailover.lastEventAt` and
  `ClusterOverview.autoFailover.lastEventAt` (date-time, nullable), `ClusterOverview.autoFailover.limitReached`
  (boolean), `ClusterOverview.failover` (`running` boolean, `taskId` nullable int64, `serverId` nullable,
  `phase` nullable).

**Status codes.** `failoverServer`: 202 `AcceptedTask` (a `Task`; during a bootstrap of the map group its
`id` is 0, 7.6), 200 `FailoverPlan` for `dryRun`, 400, 401, 403, 404 (unknown server), 405, 409
(`failover_in_progress`, `rebalance_in_progress`, `operation_in_progress`, `no_replica_available`,
`failover_needs_decision`, `quorum_unavailable`, `lease_settings_differ`, `version_mismatch`), 422
(`node_state_invalid`: `graceful` on a server that is not `live`; `unprocessable`: a group that the server is
not primary of, a target that is not a standby of the group; `unsupported_operation` in the increments that
lack a part), 503. `restorePreferredPrimaries`: the same without 404. A request to a server that is not the
leader is forwarded to it as every cluster operation; when the leader cannot be reached and the request
concerns the server of the map group primary, 7.6.

**Problem codes** (`ProblemCode` additions, 409 unless noted; their sentences, which are `detail` texts):

- `failover_needs_decision`: "This failover could lose data. Read the plan, and repeat the request with
  `allowDataLoss` set to true if you accept it." (the body's `blockers` lists the groups and the loss.)
- `quorum_unavailable`: "This server cannot reach a majority of the servers, so no shard group can be
  handed to another server safely. Repair the network or fail over from a server that can."
- `lease_settings_differ`: "The servers do not use the same lease duration and margin; a failover is not
  safe until they do. Set the same values on every server and restart `kingresd`."
- (already present, used with new texts) `failover_in_progress`: "A failover of server `<id>` is running;
  wait for it to end." `no_replica_available`: "Shard group `<n>` has no standby that is ready to take over."
  `task_not_cancellable`: "The standby has been promoted; the task can only go forward."

**Alert kinds** (added to `AlertKind`; severity, subject members):

| Kind | Severity | Raised when | Subject members |
|---|---|---|---|
| `failoverNeedsDecision` | critical | a failover waits for the operator: data loss possible, a standby unreadable, a foreign prepared transaction in the way | `shardGroupId`, `serverId` (old primary), `taskId` (null during a bootstrap) |
| `failoverBlocked` | warning, critical for the foreign prepared transaction | an automatic or requested failover cannot proceed: no majority, the old holder still renews, a rebalance does not stop, no standby, versions or lease settings differ | `shardGroupId`, `serverId`, `taskId` |
| `failoverDataLoss` | warning, critical for a group that ran without its synchronous standby | `check_cluster()` after a failover shows `prepared_rolled_back_after_failover` or `participant_lost_committed_transaction` | `shardGroupId`, `taskId` |
| `splitPromotion` | critical | two instances of one group are out of recovery, or another server promoted the map group first | `shardGroupId` |
| `oldPrimaryRunning` | critical | an old primary was found running (or reachable and writable) after the catalog moved | `shardGroupId`, `serverId` |
| `oldPrimaryNotStopped` | critical | an old primary could not be stopped | `shardGroupId`, `serverId` |
| `oldPrimaryNotRebuilt` | warning | an old primary has not become a standby again 10 minutes after its server returned or after the failover | `shardGroupId`, `serverId` |

`autoFailoverTriggered` (info, at the end of an automatic run) and `autoFailoverLimitReached` (warning, once
per limit) exist and are used as `ha-phase5.md` intended; `taskFailed` is raised for a failed failover task
with `taskId`.

**Audit.** `failoverServer` (the request: server, mode, groups, targets, `allowDataLoss`, `dryRun` is not
audited, `comment`), `autoFailover` (actor `system`), `restorePreferredPrimaries`, `resetAutoFailoverCounter`,
and the events `failoverGroupStarted`, `failoverGroupPromoted` (details: old and new server, term, timeline,
position, `allowDataLoss`), `failoverGroupFailed`, `oldPrimaryRebuilt` (details: method, bytes). Resource
type `shardGroup` (resource id the group), as for the lease events.

### 7.3 The documentation rows to change

README "Task kinds and subjects": `server.failover` gets "steps: `Check that the failover can start`, then per
group `Group <n>: Freeze the group`, `Stop the old primary`, `Wait for the old lease`, `Choose the standby`,
`Hand the lease to the new server`, `Promote the standby`, `Make the catalog follow`, `Point the other
standbys at the new primary`, `Update the standby records`, `Check synchronous_standby_names`, `Resolve
prepared transactions`, `Check that clients arrive`, `Lift the freeze`, then `Write the report`; a task that
waits for an operator is `paused`; the report id is the task id". `replication.restorePreferredPrimaries`
gets its steps. The design note 9 ("Auto-failover settings ... The counter is reset by a rebalance") gets
"and by `resetAutoFailoverCounter`". The note about `Server.status` and the 0.9.0 text of `failoverServer`
("Promotes the standbys of all primaries on this server to primary on their own servers") is rewritten to
what 7.2 says.

### 7.4 Task error codes (README table, not an enum in the YAML)

`superseded`, `lease_still_held`, `lease_held_elsewhere`, `lease_lost`, `quorum_unavailable`, `no_candidate`,
`target_not_available`, `split_promotion`, `promote_timeout`, `promote_incomplete`, `not_map_primary`,
`bad_promote_target`, `standby_behind`, `switchover_unproven`, `cannot_quiesce`, `prepared_transactions_pending`, `cancelled`,
`step_interrupted` (the engine's, not used by these tasks), `step_failed` (the engine's). Clients tolerate
codes they do not know.

### 7.5 The version and the changelog

`info.version` 0.10.0; the README's first line says 0.10.0; `check-contract.mjs` gets the 0.10.0 rules (the
new operations list the uniform responses; `AuditAction` covers `resetAutoFailoverCounter`).

Changelog entry (to be written into `kingres/api/README.md` as "### 0.10.0"):

> Revision written with high availability, part 2: failover, switchover, restoring preferred primaries and
> the old primary's return, driven by `kingresd`. Design: `kingres/docs/design/ha-phase5-part2.md`. Summary for
> the panel: 4 operations added (`getFailoverStatus`, `listFailoverReports`, `getFailoverReport`,
> `resetAutoFailoverCounter`), 2 operations implemented that answered `unsupported_operation` (`failoverServer`,
> `restorePreferredPrimaries`), 3 problem codes, 7 alert kinds, 5 audit actions, **1 change that is not
> additive**.
>
> **Breaking.** `ClusterSettings.autoFailover.timeoutSeconds` has the minimum 30 (it was 1; the design always
> said 30, and a shorter value cannot tell a failure from a pause). Nothing was released.
>
> **Additive.** `FailoverRequest` gains `shardGroupIds`, `targets`, `allowDataLoss`, `dryRun`, `comment`;
> `failoverServer` and `restorePreferredPrimaries` answer 200 with a `FailoverPlan` for `dryRun`;
> `restorePreferredPrimaries` accepts an optional body; the failover status and report schemas;
> `ShardGroup.failover`; `TaskStep.shardGroupId`; `ClusterSettings.autoFailover.lastEventAt`,
> `ClusterOverview.autoFailover.limitReached` and `lastEventAt`, `ClusterOverview.failover`; the alert kinds
> `failoverNeedsDecision`, `failoverBlocked`, `failoverDataLoss`, `splitPromotion`, `oldPrimaryRunning`,
> `oldPrimaryNotStopped`, `oldPrimaryNotRebuilt`; the problem codes `failover_needs_decision`,
> `quorum_unavailable`, `lease_settings_differ`; the audit actions `resetAutoFailoverCounter`,
> `failoverGroupStarted`, `failoverGroupPromoted`, `failoverGroupFailed`, `oldPrimaryRebuilt`.
>
> **Documentation.** The meaning of `graceful` (the planned switchover), of `Task.cancellable` after the
> promotion, of the counter's reset, of the task id 0 during the bootstrap of the map group.

### 7.6 The map group during a bootstrap (what a client can read)

While no leader exists, a server that runs a bootstrap answers `GET /failover` and `GET /tasks/0` from its
journal (state `running`, phase `bootstrap`, the steps B1 to B8 as a task's steps) and the other servers
answer `leader_unavailable` (503) as today; `failoverServer` for the server of the map group primary, received
by any server when the leader is unreachable, is forwarded to the best candidate (`POST /failover/map/start`)
and answered 202 with the pseudo task; the console (section 8) follows `GET /failover` until `taskId` is set.
This is the one place where the contract names a task that is not stored; open point 11.5.

## 8. The console

The panel is a Vue 3 client of the contract with an in-memory mock (`src/mocks`, switches in the URL and
`window.__kingresMock`, README of `kingres/web`). Existing pieces: `ServerActions.vue` (kind `failover`, mode
`hard` or `graceful`, type-to-confirm for `hard`), `ServerDetailPage.vue` and `ServersPage.vue` (the
`Failover` button, `GateButton op="failoverServer"`), `ReplicationPage.vue` (a card per group with
`GroupLeaseInfo.vue`), `RebalancePage.vue` and `RebalanceReportDialog.vue` (the model of a long operation
with steps and a report), `TaskProgress.vue`, `composables/useTask.ts` (`useTaskAction`), `GlobalBanners.vue`,
`SettingsPage.vue` (the auto-failover fields), `AlertSubject.vue`, `lib/status.ts` (labels of alert kinds and
task kinds), `lib/lease.ts`, `lib/standby.ts`.

### 8.1 What an operator sees before a failure

On **`ReplicationPage`**, each group's card gains a **readiness line** from `ShardGroup.failover`: "Failover
is possible: standby on `<server>`, no data loss" (a tick and text), "Failover is possible, but commits made
without a synchronous standby could be lost" (a warning), or "Failover is not possible: `<blocker text>`"
(a cross and the blocker: no standby, none ready, no majority, versions differ). Next to the existing sync
state, lag and `GroupLeaseInfo`, each standby row shows its **sync or async** state, lag in bytes and
seconds, and "streaming" or the reason it does not. The page header shows how many groups are **away from
their preferred server** and the button **Restore preferred primaries** (`GateButton op="restorePreferredPrimaries"`).
Each group card has **Switch over...** (a graceful failover of that group to a chosen standby).

`ServerDetailPage` and `ServersPage` keep the **Failover** button, which now opens the new dialog; for a
server that is `dead` the button is the primary action and the text says "Fail over"; for a `live` server the
default mode is graceful.

### 8.2 The failover dialog (`FailoverDialog.vue`, replaces the failover branch of `ServerActions.vue`)

1. **Choose.** Mode (`graceful`: "hand over without loss; the server must be live"; `hard`: "the server is
   down or not answering; commits that reached no standby can be lost"), the groups (a checkbox each; the map
   group is marked "moves the manager of the cluster"), a target per group (a select of the group's standbys
   with their lag and sync state). The mode defaults from the server's status.
2. **What it will do.** The dialog calls `failoverServer` with `dryRun: true` and shows the `FailoverPlan` as
   a table per group: from, to, the candidates with their positions (readable text, not colour only), the data
   loss verdict **in words** ("No acknowledged commit can be lost" / "Commits written under asynchronous
   durability in the last seconds can be lost" / "Commits acknowledged since `<time>` can be lost: group `<n>`
   ran without its synchronous standby" / "The standby on `<server>` could not be read; commits that only it
   holds can be lost"), the estimated time, for a switchover the time writes will pause, and the steps. A
   blocker lists as a message and disables the button; `willStopRebalance` is stated ("the running rebalance
   will be stopped first").
3. **Confirm.** For `hard` the address of the server is typed (existing `TypeToConfirm`); when the verdict is
   not `none` a checkbox "I accept that data may be lost" sets `allowDataLoss`, and the typed confirmation is
   required in any mode. Submitting starts the task; the dialog closes and the progress card opens.

### 8.3 Progress and report

- **`FailoverProgress.vue`** (new; shown on the Servers page above the table, on the Replication page, and in
  the task detail): the steps of `TaskProgress` grouped by `shardGroupId` ("Group 2: Promote the standby"),
  the phase, for a switchover the line "Writes to group 2 have been paused for 3 s", the lease wait when it
  waits ("Waiting 8 s for the old primary's lease to run out"), and the actions: **Cancel** (enabled while
  `cancellable`; after the promotion it is disabled with "A standby has been promoted; the failover can only
  go forward"), and for a paused task a panel "Waiting for you" with the blockers and the buttons **Accept
  data loss and continue** (repeats the request with `allowDataLoss`) and **Cancel**. The component reads
  `GET /failover` (it also works during a map-group bootstrap) and the task through `useTask`.
- **`FailoverReportDialog.vue`** (new, modelled on `RebalanceReportDialog.vue`): the summary (outcome, who,
  when, how long), per group: from and to, the candidates table with their positions and the order that chose,
  the data loss assessment in words with the numbers, terms before and after, the lease wait, the unreached
  nodes, the incidents with the extension's text, the state of the old primary ("rebuilding, `pg_rewind`, 12
  MB rewound"), and the timeline of events. Reached from the Tasks page (a task row of kind `server.failover`
  or `replication.restorePreferredPrimaries` links to it), from the Replication page ("Last failover") and
  from an alert.
- **Banner.** `GlobalBanners.vue` gets `banner-failover` (inside the same polite live region): "Failover of
  server `<id>` is running: group 2 of 3, `<step>`" with a link to the progress; danger tone and "waiting for
  you" when a decision is needed; "The manager is moving to `<server>`" while the map group's bootstrap runs. It
  is shown to roles that may call `getFailoverStatus`.
- **Announcements** (`useAnnounce`): start, each group's promotion, the end, and a need for a decision.

### 8.4 Automatic failover settings and the counter

`SettingsPage.vue`, the "Auto-failover" fieldset: the enabled switch, the timeout (minimum 30, the hint says
why and shows the effective floor), the maximum count, **Events since the last reset: n of m** with the time
of the last event, the **Reset counter** button (`GateButton op="resetAutoFailoverCounter"`, role
`kingres_admin`, a `ConfirmDialog`: "Automatic failover will be allowed again up to `<m>` times"), a line
"The limit is reached: further failures need an operator" when `limitReached`, and the requirements in words
(three servers, a standby for every group, a synchronous standby for a loss-free failover). `DashboardPage`
already shows the counter; it gains the limit tone.

### 8.5 Alerts and tasks

`lib/status.ts` gets the labels of the seven new alert kinds and of `replication.restorePreferredPrimaries`;
`AlertSubject.vue` links `failoverNeedsDecision`, `failoverBlocked`, `failoverDataLoss` to the failover
progress or report and the group; `oldPrimary*` to the server. The Tasks page lists the failover tasks like
any task, with the report link.

### 8.6 What each role may see and do

| Role | Sees | Does |
|---|---|---|
| `kingres_readonly` | readiness lines, topology, status, reports, alerts, the banner, the settings | nothing of the below (buttons are disabled with the reason, as `GateButton` does) |
| `kingres_ops` | the same | dry run and start a failover or switchover, restore preferred primaries, resume with `allowDataLoss`, cancel, start a rebalance again |
| `kingres_admin` | the same | and the auto-failover settings and the reset of the counter |
| `kingres_stats` | the dashboard only (summary) | nothing; no banner (it cannot call `getFailoverStatus`) |

### 8.7 Components and mock states to add

New: `FailoverDialog.vue`, `FailoverPlanTable.vue`, `FailoverProgress.vue`, `FailoverReportDialog.vue`,
`FailoverReadiness.vue` (the line on a group card), `DataLossText.vue` (the verdict in words, shared by the
dialog, the progress and the report), `lib/failover.ts` (labels, phases). Changed: `ServerActions.vue`,
`ReplicationPage.vue`, `GlobalBanners.vue`, `SettingsPage.vue`, `DashboardPage.vue`, `TasksPage.vue`,
`AlertSubject.vue`, `lib/status.ts`, `api/endpoints.ts` and the generated types.

Mock (`src/mocks/handlers.ts` and `switches.ts`; the existing `failoverServer` handler is extended):

| Switch | State |
|---|---|
| `?failover=needs-decision` | the plan's verdict is `possible`; the task pauses at "Choose the standby" until resumed with `allowDataLoss` |
| `?failover=blocked:rebalance`, `blocked:lease`, `blocked:quorum`, `blocked:noreplica` | the corresponding 409 codes and readiness blockers |
| `?failover=fail:<step>` | the task ends failed at that step with a code |
| `?failover=slow` | steps take about three times as long, so that cancel and the phases can be tried |
| `?failover=map` | the server is the map group's primary: the bootstrap, task id 0, then a real id |
| `?failover=old:rebuilding`, `old:waiting`, `old:running`, `old:failed` | the state of the old primary in the report |
| `?failover=auto:<server>` | an automatic failover is under way (`trigger: automatic`) |
| `?failover=limit` | the counter is at its maximum |
| `?failover=switchover:behind` | verification fails: rollback, `standby_behind` |
| `window.__kingresMock` | `failServer(id)`, `reviveServer(id)`, `setReadiness(group, state)`, `resetCounter()` |

Tests: vitest and Testing Library against the mock for each component and role (the pattern of
`tests/*-0-9.test.ts`), the axe test for the dialog and the progress card, and a contract test that every new
operation and schema in the mock is present in `openapi.yaml`.

## 9. Tests required

The harness is `internal/clustertest` (real `kingresd` processes and real instances, in WSL, three or more
servers; `Node.Kill`, `Stop`, `Pause` (SIGSTOP of `kingresd`), `Isolate`, `Partition`, `Heal`, `StopInstance`,
`StopStandbys`, `FormWith`), and the fault hook of `internal/fault` (`faults.json` in `fault-dir`: blocked
servers in each direction, a wall-clock offset, `forceEnforce`, delays). The tests of the part-1 leases
(`lease_test.go`) and the standby tests (`standby_test.go`) are the models. The tests below add to the
harness:

- `Node.PauseInstance(group)` and `ResumeInstance(group)`: SIGSTOP and SIGCONT to every process of an
  instance (the group of the postmaster), read from `postmaster.pid`; `Node.KillInstance(group)`.
- `Writers`: N client connections through the **client entry points** of different servers, each inserting
  rows with a unique id inside its own transaction at durability `sync`, recording an id as acknowledged only
  when the commit returned; it reports the acknowledged set, the errors and the time of the last
  acknowledgement per connection. A second kind writes distributed transactions (a row in group `a` and one
  in group `b` per transaction).
- A `failover.Options.Fault(point string)` hook, the failover's counterpart of `task.FaultPoints`, called
  before and after every step and at the named points inside a step (`f6_after_pg_promote`,
  `f7_after_first_call`, `b6_after_pg_promote`, ...); the tests make it end the process (`os.Exit(137)`) or
  block on a file (`:wait:<file>`) through a new member of the fault spec; nil in the product.
- The extension's own `kingres_shard.fault_injection` (`promote_before_change`, `promote_after_first_push`,
  `promote_after_push`, the commit-protocol points with `:kill`, `:error`, `:wait:<file>`) for the points
  inside `promote_node` and inside distributed commits.
- A sampler that, every 100 ms, reads `pg_is_in_recovery()` on every instance of a group and fails the test
  at the first instant two instances of one group are out of recovery **and** accept a write.

A test that cannot be made deterministic is run many times in a loop, and the count of runs is reported
(`run-integration.ps1 -Count N`, as for the part-1 tests). Tests marked (loop) are run 30 times in the
definition of done.

### 9.1 One test per invariant

| # | Invariant | Test (name to give it) | How it tries to break it | How it is made deterministic |
|---|---|---|---|---|
| T1 | I1, I2 | `TestKillPrimaryUnderLoad` (loop) | three servers, `replicaCount` 1, `sync`; `Writers` on every entry point; `Kill` the server of group 1's primary after N acknowledged rows; start the failover (`hard`); after it, the survivors keep writing | the kill happens at a counted acknowledgement; the check is set equality: every acknowledged id is on NEW and in the table read through every entry point; the sampler shows no instant of two writable instances; the old server restarted afterwards does not start a primary (no postmaster for the group, the marker exists) |
| T2 | I1 | `TestPausedOldPrimaryThenResumed` (loop) | `PauseInstance` and `Pause` on the old primary's server through the failover (longer than `D + M + 5 s`), then resume both; a client with an open connection to the old instance writes before the pause and keeps trying after the resume | with `sync` the resumed instance cannot get a confirmation from a standby of the new timeline, so no commit is acknowledged after the resume (assert zero); the instance is stopped by the watchdog or the lease check within 2 s of the resume; the async variant counts acknowledgements after the resume (expected 0 or a few) and asserts that none of the rows survives the rebuild and the report's window includes them |
| T3 | I1 | `TestLeaderCannotSeePrimaryClientsCan` | `Isolate` the leader from the primary's server in both directions of the internal API only (clients and the third server still reach it); request `hard` failover | OLD's holder still hears a majority and renews; the task ends `lease_still_held` after at most 25 s; assert OLD still primary, NEW still in recovery, writes acknowledged throughout, the group unfrozen, the alert raised |
| T3b | I1 | `TestPrimaryCutFromMajorityStillReachedByClients` (loop) | `Isolate` the primary's server from both others (its clients still reach it, writing continuously); request the failover from the majority side | the primary fences itself at about 7 s: the last acknowledgement is earlier than the instant at which the grantors would grant (read from `GET /lease/state` of both, the margin of `TestPartitionedPrimaryWithStandbyStopsBeforeANewHolderCouldBeGranted`); the failover succeeds once the lease is free; acknowledged equals surviving |
| T4 | I3 | `TestKingresdKilledAtEveryStep` (one run per point) | the leader's `kingresd` ends at each point of F0 to F13 and in the middle of F6 and F7, then restarts (same server: the map primary stays) | the fault point is exact; after the restart the task completes; assertions: exactly one writable primary at every sample, no acknowledged commit lost, one `failover_group` row with one pin, the counter incremented once, the report complete |
| T5 | I3 | `TestLeaderChangeAtEveryStep` | four servers; a failover of group 2 is running; the **map group's server** is killed at each point (so the leader moves by a bootstrap); the new leader resumes the task | a fault file blocks the old leader's task at the named step until the kill; assertions: group 2 ends healthy, group 0 promoted once, the old leader's late calls (a paused old leader resumed) are refused (`fenced`, `stale_term`), no second primary in either group |
| T5b | I3, I4 | `TestSupersededLeaderPausedAndResumed` | the leader's whole server is paused (SIGSTOP of `kingresd` and instances) after F5 of group 1, the cluster promotes the map group elsewhere, the old leader is resumed | its next conditional write is refused, its late `promote` finds `stale_term` or the lease held by another, no hook runs, the old map primary is fenced by its lease; assertions on both groups |
| T6 | I4 | `TestMapGroupPrimaryLost` | kill the map group's server under distributed-transaction load (automatic failover on) | the journal steps B1 to B8 in order; the engine gate stays closed until B8 (a fault file holds B7; during the hold `GET /tasks` on the new leader shows no claimed task and the old rebalance row is `paused`); the new leader's catalog and `kingresd`'s leader view move; the task is recorded with the right steps and finishes; `certificate_authority_unavailable` is absent when the escrow exists and present when it does not; no acknowledged commit lost |
| T6b | I1, I4 | `TestMapGroupMinorityCandidateDoesNotPromote` | five servers; partition `{candidate, one other}` from `{three}`; the map primary's server on the minority side | the minority candidate cannot get the lease (assert it stays a standby of group 0), the majority side's candidate promotes; heal; assert one map primary, the old one superseded (`catalog_status.superseded`) and rebuilt |
| T6c | I4 | `TestMapGroupNeedsMajorityOfOtherGroups` | two groups only, or half the other groups lost with the map group | `promote_node` answers the extension's refusal; the bootstrap waits, raises `failoverBlocked`, changes nothing; the manual SQL of README sequence 3 then works and the task is not corrupted |
| T7 | I2 | `TestTwoFailuresAtOnce` | five servers, kill two servers that host primaries; `maxCount` 2 then 1 | two tasks one after the other, the second waits and is not refused by a lock; with `maxCount` 1 the second waits for the operator and `autoFailoverLimitReached` is raised; for one server hosting two primaries (after a first failover): one task, map group first |
| T8 | I2 | `TestChosenStandbyIsTheMostAdvanced` | `replicaCount` 2, `ANY 1 (a, b)`; make `b` lag by pointing its `primary_conninfo` at a closed port, write rows acknowledged through `a`, kill the primary | the chosen standby is `a`; with `a` unreadable (its server also dead) the verdict is `unknown`, the task pauses with `failoverNeedsDecision`, nothing is promoted; resumed with `allowDataLoss` it promotes `b` and the report lists the rows lost; with `targets` naming `b` while `a` is better: 409 `failover_needs_decision` |
| T8b | I2 | `TestChoiceOrderIsAPureFunction` (unit) | ties on position, sync state, preferred server, ring distance, id | table-driven, no clock |
| T9 | I2 | `TestFailoverDuringBucketMove` | a rebalance moves a bucket from group 1 to group 2; kill group 1's server in each phase of the move (`preparing`, `copying`, `catching_up`, `switching`, `cleaning`) | the automatic failover stops the rebalance first (`stoppedBy` `safetyRule`, `pausedReason` names the server), waits for `paused`; the extension settles the move (`check_cluster()` empty afterwards); the failover completes; the rebalance can be started again; with a move that cannot stop for 5 s (a held target) the failover waits and raises `failoverBlocked`, and proceeds when it ends. The phases are held with the same mechanism as `rebalance_faults_test.go` (to be verified for each phase) |
| T10 | I2 | `TestFailoverDuringDistributedTransactions` | `Writers` of distributed transactions; the origin's `kingres_shard.fault_injection` holds at `before_prepare`, `after_first_prepare`, `after_prepare`, `after_log`, `after_commit`, `between_commit_prepared`; kill a participant's group primary (and in a second series the origin's) at each point; fail over | at `sync`: atomic outcome on every node (guarantee 11), no prepared transaction left after F11; at `async`: the incidents appear in the report (`prepared_rolled_back_after_failover` or `participant_lost_committed_transaction`) and the alert is raised |
| T11 | I1, I6 | `TestOldPrimaryReturns` | after a failover restart the old server; make a divergence first (an asynchronous write acknowledged by the old primary and never replicated) | the instance is not started as a primary (marker), `lease_state` shows `none`; the leader rebuilds it: `pgRewind` with `rewindBytes > 0`, the divergent row gone; `baseBackup` when `pg_rewind` is made to fail (a fault flag); streaming, named in the list, `check_cluster()` empty, markers gone, `catalog_status` shows the new term; a second failover back works |
| T11b | I6 | `TestOldPrimaryNeverReturns` | `standby-replace-after` 20 s, the old server stays down | `groupUnderReplicated` raised, `oldPrimaryNotRebuilt` after 10 s of the shortened rule, the record removed in the order of sequence 8 (the list first: no commit blocks, a distributed commit does not get `KS005`), a replacement created on another server |
| T11c | I6 | `TestOldPrimaryRunningUnfenced` | stop its `kingresd` and its watchdog, keep the instance (the rare case), cut it off, fail over, let it back | F7's last-resort demote stops it, `oldPrimaryRunning` is raised, the report states the window, the rebuild discards the writes |
| T12 | switchover | `TestSwitchoverAndBack` (loop) | `Writers` at `sync`; `failoverServer` `graceful`; then `restorePreferredPrimaries` | zero acknowledged commits lost, the old primary adopted with method `clean` (no rewind), the write pause (S2 to F12) is measured and reported, the term rises by one each time, `unreached` is `[<old>]` at F7; the map group last |
| T12b | switchover | `TestSwitchoverVerificationFails` | break NEW's replication (closed port) just before S6 | `switchover_verify` stays false, the task rolls back (a second case: OLD killed instead of stopped, the state is not `shut down`, `switchover_unproven`): OLD started again, rules restored, writes accepted again, `standby_behind`, NEW never promoted |
| T12c | switchover | `TestCancelBeforeAndAfterThePointOfNoReturn` | cancel at S2, S3, S5, S6, S7 and after S8 | before S8 the rollback of 5.3 and `cancelled`; after it 409 `task_not_cancellable` and the task finishes |
| T13 | auto | `TestAutomaticFailoverRules` | `timeoutSeconds` 30 and lease settings shortened by flags | disabled: nothing; limit reached: nothing and the alert, then `resetAutoFailoverCounter` and it proceeds; no standby: `failoverBlocked`; asynchronous standby with a `sync` database: `failoverNeedsDecision` and no promotion; all-async: promotes; a rebalance running: stopped first; a server that returns before the timeout: nothing; a one-way cut: nothing |
| T14 | fencing | `TestStalledProcessesDoNotJudge` | pause the leader's `kingresd` for longer than the timeout, resume; step the wall clock of a server by an offset during F3 and F6 | no failover starts from the stall; the clock step changes no decision |
| T15 | extension answers | `TestPromoteNodeAnswers` (unit, a fake of the extension in the style of `internal/rebalance` `loop_test.go`) and `TestPromoteNodeAnswersReal` | each row of the table of F7: `promoted` (final false) then `already`; `completed`; `unreached`; `blocked_by` of class cluster (resolved) and foreign (two identical answers: ask); "cannot commit" for NEW and for another group's primary; `KS005`; "could not prepare ... did not answer"; "still in recovery"; "could not connect"; the two map-group refusals; NULL; a broken connection; the 10-call and 15-minute bounds | the fake returns the scripted answer; the real test provokes the ones that can be provoked (a foreign prepared transaction holding `kingres.map_version`; a list that names a missing standby; a participant's standby stopped) and **records which message the extension gives for a participant whose standby is dead**, which settles open point 11.1 |
| T16 | tasks | unit and PG tests of the engine additions | `Gate` (a closed gate claims and runs nothing), `Definition.Cancellable`, migration 0011 and the exclusion (`KX003`, `KX004`, the 0009 list with the restore task), the conditional records (a run that lost the lease writes nothing: `TestSupersededHolderWritesNothing` in the style of `fencing_test.go`), a transient database error is retried and not fatal (RBR-02), a quiet end is logged (RBR-07) | memory store and real system database as the part-1 task tests |
| T17 | leases | unit tests of the grantor and manager additions | `transferTo` only from the holder, persisted before the answer, survives a restart; `release`; `Adopt`, `Attach`, `Transfer`, `Relinquish` keeps `Check`; `marginMs` mismatch refused | fake clock, as `lease_test.go` |
| T18 | mapping | `TestInstanceMappingKeepsTheSuites` | the existing suites on a cluster whose group 1 was failed over by hand through the extension and the markers | no behaviour change; the new `SystemConn`, `PrimaryConn` and the per-group markers exercised |
| T19 | agent | unit tests | the sync rule while frozen and after the freeze lapses; `standbys-gone`; the self-healing `primary_conninfo` | as `rules_test.go` |
| T20 | console | the vitest and browser tests of 8.7 | every role, every switch | the mock |

### 9.2 Things the tests must show about the report

Every failing test of T1 to T13 also checks the report: the term before and after differs by one per promoted
group, the pinned standby equals the promoted one, the data-loss verdict equals what the plan said before the
request, and the old primary's state is the one the test produced.

## 10. The order of work

Increments that can each be committed, tested and reviewed alone. After each, what is refused is stated.

0. **Register the standbys in the catalog** (extension sequence 1 and 8): the `CatalogRegistrar` over
   `add_node(..., 'standby')`, `standby_name_matches` before a name goes into the list, `catalog_node_id`
   stored, the step `standby.register_in_catalog` no longer skipped, `remove_node` in the order of sequence 8,
   `ManagedStandbyInstanceBase` removed, the topology reads the standbys from the catalog. *Refused*:
   `failoverServer`, `restorePreferredPrimaries` (as today, `unsupported_operation`).
1. **The instance mapping of 3.7**: role files, `SystemConn`, `PrimaryConn`, per-group markers and watchdog,
   the agent per group, the proxy's local ports, the fan-outs to every writable instance. No behaviour of its
   own; test T18. *Refused*: the same.
2. **The lease additions and the read calls**: `transferTo`, `release`, `Adopt`, `Attach`, `Transfer`,
   `Relinquish`, `marginMs`; `GET /failover/position`, `GET /liveness`, `GET /failover/state`; the freeze and
   `standbys-gone` on the agent; the engine gate and the cancel guard; migration 0011 (tables and exclusion).
   Tests T16 to T19. *Refused*: the same.
3. **Manual failover of an ordinary group** (`hard`, a dead or a reachable server): F0 to F13, the choice, the
   pin, `promote_node` loop, the report, `getFailoverStatus`, the reports, contract 0.10.0 (the parts these
   need), `dryRun`, `allowDataLoss`, the alerts of 2.6. Tests T1, T2, T3, T3b, T4, T8, T9, T10, T15. *Refused*
   (`unsupported_operation`): `graceful`, a group that is the map group, `restorePreferredPrimaries`,
   automatic failover (the setting is accepted and does nothing, as today).
4. **The old primary back**: `diverged` rebuild with `pg_rewind` and the fallback, the markers removed on
   becoming a standby, `standby-replace-after`, the alerts, the removal order. Tests T11, T11b, T11c.
   *Refused*: the same as 3.
5. **The map group**: the bootstrap, the journal, the engine gate use, the pause of the old rebalance, the
   recorded task, the CA escrow, the candidate monitors **for a manual request only**, the pseudo task. Tests
   T5, T5b, T6, T6b, T6c. *Refused*: `graceful`, `restorePreferredPrimaries`, automatic failover.
6. **Automatic failover**: the leader's loop, the candidates' monitors on their own authority, the counter and
   its reset operation, stop-the-rebalance, the cool-down, the alerts. Tests T7, T13, T14. *Refused*:
   `graceful`, `restorePreferredPrimaries`.
7. **Switchover and preferred primaries**: quiesce, verify, adoption after a clean stop, rollback, the
   restore task. Tests T12, T12b, T12c. *Refused*: nothing of this part.
8. **The console**, in three parts that follow 3, 6 and 7 (each is mock-first and can ship before its
   backend, because the mock is the contract's executable form): the dialog, progress and report with the
   readiness lines (after 3); the automatic settings, the counter, the banner and the alerts (after 6); the
   switchover and restore actions (after 7). Test T20 each time.
9. **Hardening**: the loops of the definition of done, the measured times of section 2.5, the review of the
   whole part by someone who did not write it (the practice of the earlier parts: a review round and a fix
   round), the README of the control plane and `internal-api.md` brought to the final state, ADR 0021 updated
   with the lease additions.

## 11. Open points and risks

Each has a recommendation, so that the implementer does not have to stop and ask.

1. **A commit that touches a group whose standby is dead: `KS005` or "could not prepare the transaction on
   node ..."?** The README of the extension does not settle which (`KS005` is produced by `t/070` and `t/084`
   for a participant whose standby does not confirm; the other text is "not produced by a test"). *Design for
   both*: F7 treats them alike (repeat within the budget, record the text), F0b empties the lists of the groups
   whose standby sat on the failed server, and test T15 records which message really comes. *Recommendation*:
   keep the two branches until the README says; then delete one.
2. **The certificate authority on a map-group failover.** `ca.key` and `revoked.json` live on the first
   server. *Recommendation*: the encrypted escrow of 3.4 to the servers that host standbys of group 0, adopted
   at B8, in increment 5. Alternative: a map failover that leaves the CA unavailable (acceptable for a short
   time, not for a year). Risk: more copies of the key; every server is already fully trusted by every other
   (README, decision D3), and the copies are on a few servers.
3. **The instance mapping (3.7) is the largest risk**: it touches the lease manager, the agent, the proxy, the
   fan-outs and `kingresd`'s own connections, and a server with two primaries changes what "apply to this
   server" means. *Recommendation*: its own increment (1), no behaviour change, tested by running the existing
   suites on a hand-failed-over cluster; and a review round before increment 3.
4. **Failover without a majority, two-server clusters, the map group of a cluster of two groups.**
   `ha-phase5.md` promises an audited operator override; the extension offers
   `promote_map_group_without_majority` for the map group. *Recommendation*: not in this part. The documented
   SQL is the way out; a later increment adds a request member that carries the extension's acknowledgement
   text and a lease override with an alert, after the first beta. This is a departure from `ha-phase5.md`,
   stated here.
5. **The task id during the bootstrap (`id 0`).** *Recommendation*: as designed (the status resource carries
   the truth; `taskId` becomes non-null at B7). Alternative: answer 202 only after B7, which makes the
   operator wait up to a minute with no answer. Rejected: the console needs something to follow at once.
6. **Keeping the old primary's data directory for an audit** (the extension suggests it). *Recommendation*:
   not in v1; the report records the divergence size from `pg_rewind --dry-run`. Add a request option
   `keepOldDataDirectory` when servers report free disk (the README says no server does yet).
7. **Failover triggered by a group-level failure** (the instance is down, `kingresd` alive). The contract's
   trigger is "seconds without heartbeat". *Recommendation*: v1 does server death only; a later increment
   adds "the primary instance of group `g` has been down for `timeoutSeconds` while its `kingresd` answers" after
   checking how the instance supervisor restarts a crashed primary (to be verified).
8. **The contract's `timeoutSeconds` floor** (30). *Recommendation*: accept the tightening in 0.10.0.
9. **Sequential groups.** A server with many primaries (after repeated failovers) takes the sum. *Recommendation*:
   accept; a later change can run F1 to F6 of several groups in parallel and keep F7 serial, which the design
   permits (the lease and the pin are per group).
10. **The follower's settings are a replicated copy** (map-group bootstrap): `maxCount` and `eventsSinceReset`
    may be a little stale. *Recommendation*: accept; the effect is at most one extra automatic failover of the
    map group, never an unsafe one; the new leader recounts.
11. **`pg_rewind` of a fenced (immediately stopped) primary** needs the program's crash recovery; today
    `Manager.Rewind` passes `--no-ensure-shutdown`. *Recommendation*: let `pg_rewind` run its own crash
    recovery and fall back to a base backup on any failure; if tests show crash recovery on a primary of this
    product is unreliable, use a base backup always for increment 4 and add the rewind later.
12. **Where the extension's node ids and the plan's standby ids meet.** After F9, `kingres_cp.standby` rows and
    catalog nodes must agree (`catalog_node_id`); a mismatch is a bug that `check_cluster()` plus a reconcile
    check should alarm on. *Recommendation*: the reconcile loop verifies it every check and raises
    `groupUnderReplicated` with a message that names the mismatch.
13. **Hot-standby reads** (`client-standby-listen`): after a failover the old primary, once a standby, serves them;
    the standby entry point lists local instances by role. *Recommendation*: nothing more in this part.
14. **The 60-second leader memory** (`leadermemory.go`) and the 2 s leader cache must be dropped when the catalog
    term changes; otherwise a server accepts the old leader's calls for a minute. *Recommendation*: in
    increment 2; the callee also checks the term of leader-only calls where it can.
15. **Alerts raised before the system database is writable** (a candidate in a bootstrap) live in the
    candidate's local alert store and reach the cluster view through the merge that already exists for
    per-server alerts (to be verified). *Recommendation*: if the merge is absent, the bootstrap also logs them
    and `GET /failover` carries `needsDecision`.

## Decisions and departures from ha-phase5.md

Where the earlier design left something open or this document differs from it, in one line each (the reason
is in the section named).

1. One task per failed server, groups in sequence, map group first; at most one failover-class task at a time
   (3.1).
2. The positions are read only after the group lease is held by a fence holder; the lease is then handed to
   NEW's server by an atomic `transferTo`, not released (3.3 F3 to F5; 3.1.1).
3. A planned switchover releases the lease after a clean stop (5.2); a failover transfers it atomically, and a
   server that did not hold it never releases it.
4. The choice of the standby is pinned in a conditional record and never made twice; two promoted instances of
   a group stop everything (`splitPromotion`) (3.3 F4; I5).
5. `promote_node` has a time budget of 15 minutes next to the extension's 10 calls (3.3 F7).
6. `standbys-gone` empties the lists of the other groups at once when a server is failed over (3.3 F0b).
7. The map group is promoted by a bootstrap run by a follower, with a local journal, the lease and the
   extension's majority rule as its authority; the task is recorded afterwards; the engine has a gate (3.4).
8. The certificate authority is escrowed, sealed, to the servers that host standbys of the map group (3.4).
9. Instance directories keep their ids; a role file says what an instance serves; the catalog and the role
   files replace "instance 1" everywhere (3.7).
10. The old primary is rebuilt by the reconcile loop, not inside the task; it counts as a planned standby for
    30 minutes before a replacement is made (6).
11. Automatic failover counts from the last heartbeat (the contract's words), needs a majority of opinions, has
    a 5 minute cool-down per group, counts one event per task, and stops the rebalance first (4).
12. `timeoutSeconds` has the minimum 30 in the contract (7).
13. A switchover is `failoverServer` with mode `graceful`, extended with groups and targets; no new operation
    (5.1, 7.1).
14. The write stop of a switchover is the hold rules in `pg_hba.conf` plus termination of sessions, not
    `default_transaction_read_only` (5.2).
15. A forced failover without a majority is **not** in this part; `ha-phase5.md` promised it (4.7, 11.4).
16. The task id of a map-group bootstrap is 0 until the task is recorded (3.4, 11.5).
17. After the second review of the extension's call sequences
    ([reviews/2026-11-16-kingres-shard-0.5-second-sequences.md](../reviews/2026-11-16-kingres-shard-0.5-second-sequences.md))
    the proof of a switchover takes the control file's state as well as the checkpoint position and has
    three answers; a proof that can never be given fails the task with `switchover_unproven` and rolls
    back, it does not promote (5.2, S4 to S6). `kingresd` reads the SQLSTATE of a refusal of the
    extension, and a node that did not answer (`KS007`) is not asked again by the same step (1.4).

## To be verified

Things this document uses or proposes that the sources read did not settle; each is checked before or in the
increment named.

1. Where the leader reads the durability of each database for the verdict of 2.6 (the source of
   `Database.replication.durability` in `listDatabases`), and which databases hold buckets in a group (the
   view `kingres.buckets`). Increment 3.
2. That `Engine.Record` accepts a task in state `pending` with some steps `done`, and that the engine's claim
   takes it (B7). Increment 5; otherwise add `Engine.SubmitResumed`.
3. That `internal/monitor` has a per-server alert store usable while the system database is not writable, and
   that `GET /alerts` merges it (`gather.AlertID`). Increment 5.
4. That `kingres_system` exists with the extension's catalog on every instance (`catalogTopology` reads
   `kingres.node` from it on every server) and that `kingres_cp` is complete on the map group's standbys
   (physical copy), readable while in recovery for the follower's settings. Increment 5.
5. That the catalog lists the nodes of a group in node-id order, so that `preferredPrimaryServerId` (the first
   node) is the home server after any number of promotions (`buildGroup` uses `nodes[0]`; the order of
   `cat.groups` comes from `loadCatalog`). Increment 7.
6. That the managed configuration of a standby, written by its server as "a copy of the primary instance's",
   holds this server's certificate paths for `kingres_shard.node_conninfo`, and whether it carries the old
   primary's `synchronous_standby_names` in `kingres-managed.conf` (the extension says it is inherited). It
   decides what F6 step 5 must rewrite. Increment 3.
7. That `primary_conninfo` is reloadable (PostgreSQL 13 and later; the repository's 18 and 19 are expected to
   be) and that a reload is enough to switch a running walreceiver (F8). Increment 3.
8. `pg_promote(true, 60)` timing, and what it does with a standby that is still replaying; the default
   `wal_receiver_status_interval`. Increment 3.
9. That `kingresd` can reach a promoted instance whose data came from another server (`origin: copy`) by peer
   authentication on its private socket for every use of `ServiceConn`, including the sign-in paths and the
   proxy's reads (`StandbyIdentMap`). Increment 1.
10. That `pg_rewind` without `--no-ensure-shutdown` runs its crash recovery with the instance's
    `shared_preload_libraries`, and what it needs on the source (a superuser connection over the replication
    certificate). Increment 4.
11. How the `instance.stop`, `instance.restart` and `config.restart` records reach the leader
    (`POST /task/record`, the engine's `Record`) and where the `KX004` check goes. Increment 2.
12. The phases of a bucket move can be held in a test (`rebalance_faults_test.go`) for each of `preparing`,
    `copying`, `catching_up`, `switching`, `cleaning`. Increment 3 (T9).
13. Where `internal/rebalance` ends a run (`stepWrite`) so that the counter's reset is in the same
    transaction. Increment 6.
14. That `GET /standby/status` may carry the primary's flush position without a change of the call's policy
    (`LeaderOnly`) and that `pg_current_wal_flush_lsn()` is cheap to read every 10 s. Increment 3.
15. That `internal/fwdproof` can carry an operator's identity to a server that is not the leader (the
    forwarding of a manual map-group request, 3.4). Increment 5.
16. That `clientproxy` closes connections within a second when a group's destination changes while the lease
    flips on the same server (F12). Increment 3.
17. The exact columns of `check_cluster()` (the README names the problem kinds, not the column names), for the
    parsing of F11. Increment 3.
18. That a server whose home instance is a standby (no primary at all) can run every part of `kingresd` that
    reads the catalog through `SystemConn` and writes nothing locally (3.7). Increment 1.
19. That the extension keeps node ids across `promote_node` (the README says the roles of the two rows are
    swapped and the term raised; the catalog's node-id stability is what `catalog_node_id` and
    `preferredPrimaryServerId` rely on). Increment 0.
20. Whether the contract check script and the generated client accept a `Task` with `id` 0 in a 202 body
    (the script requires a `Task` body and a `Location` header). Increment 5.

## Found while building

### Increments 0 to 2 (2026-11-17)

Where this document and the code differ after the first three increments;
the code was followed, and where the README of `kingres_shard` and this
document differed, the README.

**Statements of "To be verified" that are settled.**

- 19 (node ids across `promote_node`): settled by reading the extension:
  `promote_node` changes the `role` of the rows by `node_id` and never an id.
  A test against a running cluster belongs to increment 3.
- 9 (a promoted instance is reached through the paths of a primary): shown by
  the cluster test of a group failed over by hand (`pg_promote`,
  `promote_node` and the role file): the lease manager, the standby agent,
  the fan-out of roles and the service connection reach the instance whose
  origin is a copy. Not covered by it: the role connections of the client
  entry point and the sign-in paths, which use the same function.
- 18 (a server whose home instance is a standby): by reading only. One writer
  that would have failed there was found and changed: the migration of the
  system database is skipped on an instance that is in recovery. No test has
  such a server.
- 11 (the exclusion of instance operations during a failover): the trigger of
  migration 0011 refuses `instance.stop` and `instance.restart` of an instance
  of a group with an unfinished failover (`KX004`, answered as 409
  `failover_in_progress`), and at most one failover-class task is unfinished
  (`KX003`). It cannot see `config.restart`, whose subject is not an instance:
  that task needs a check of its own in its step, in increment 3.

**Section 1.5.** The registration of a standby in the catalog of the
extension is done by `kingresd` now: the step `standby.register_in_catalog`
is `done`, never `skipped`, and a standby made before the registration
existed is registered by the next check of the leader.

**Section 3.1.1.** `release` is `Manager.ReleaseGroup`. A transfer or a
release deletes the manager's state of the group; `Relinquish` keeps it, and
a relinquished group is not renewed and holds its instance back, also across
a restart (a marker `relinquished.<group>`).

**Section 3.1.2.** `GET /liveness` and `GET /failover/position` are answered
to any peer, `GET /failover/state` by the leader only, and a GET carries its
parameters in the query string.

**Section 3.7.** `lease.Gate` takes a function from an instance id to its
group and whether it is a primary, not an id. The lifecycle operations of the
contract (start, stop, restart of a server's instances) act on the primaries
of the server; its standbys stay the business of the standby agent, and are
stopped with the server only in the orderly stop.

**The answers of `add_node`.** It can answer `42710` (already registered),
which the registration reads as success after reading the row back, and
`42704` (the group has no primary), which is a refusal. Neither was in this
document.

**Deadlines of control calls.** The table of bounds in the README of the
extension is not used for deadlines: the reviews of 2026-11-17 changed the
bound twice. Each control call has a named deadline (five minutes by default)
and a timeout means "read the state and repeat", in a session of its own set
to READ COMMITTED, one statement per call.

**The order of naming a standby.** `standby_name_matches` needs the standby
to be registered, so the order is: registration, then the name in
`synchronous_standby_names`. A name that is already in the list stays, so a
cluster that is upgraded does not change its list.

**Not followed yet.** The third review of the call sequences asks that nodes
are registered by address and not by a host name (a name that does not
resolve is not bounded); standbys are still registered by the server's host
string, which may be a name.

**For increment 3.** A separate step that reads the result back after
`add_node`, `remove_node`, `sync_catalog` and `rotate_secret` (a success of
those calls is not proof for a node that was slow or restarted during the
call); the watchdog is told explicitly whenever the set of primaries of a
server or its enforcement changes; a node is not fenced or marked failed on
`KS007` alone; `KS014` is never repeated; after a failover the standbys of
the dead primary are removed from the catalog before the secret is rotated;
nothing sets the gate of the task engine yet: the bootstrap of the map group
(increment 5) must.

### The fixes for the review of increments 0 to 2 (2026-11-18)

What the review (`reviews/2026-11-17-kingresd-failover-increments-0-2.md`,
FO-01 to FO-16) changed against sections 3 to 6, stated as the rules the
later increments rely on.

**A directory is a standby only when it is proved to be one.** The standby
agent deletes, rebuilds or rewinds an instance directory only when its role
file is valid and says standby, and the instance is in recovery when it runs
or carries `standby.signal` when it does not; the refusal is `not_a_standby`
(409). A role file that cannot be read means "unknown": such an instance is
not writable, is not a standby and is not copied, and the start gate holds it
like a primary. Increment 3 therefore writes the role file before it calls
`pg_promote`, so that a crash between the two leaves a directory that no
agent removes. Removing the instances of a server that is itself removed or
reset is that server's own act and is not guarded.

**The watchdog file.** `off` means only "this instance is not a primary" and
never fences, however old the file. A primary of a group without a standby
gets a counting file at every renewal pass, whether or not the lease was
renewed, so that a dead or hung `kingresd` leaves a file that runs out; this
closes the case of a primary that was told for ever that nothing needs
protection and later runs beside a standby that was added and promoted. Every
file carries a validity above zero (24 hours for `off`), because a watchdog of
the version before ignores the state and would fence at once on zero.

**Leases.** A transfer is done at a grantor when the holder it names is the
target, and a release when it names no holder, so a call repeated after a lost
answer succeeds. Neither is carried out while an instance of the group runs on
the server that gives the lease up. A grantor of the version before these
calls counts for nothing, so a hand-over in a cluster that is not upgraded
everywhere is refused. An adoption clears the marker of an earlier hand-over,
and a release waits for the renewal round in flight before it asks the
grantors.

**Freezes survive a restart.** The frozen groups and the servers that are
gone are kept in `failover-freeze.json` in the data directory. The list of a
frozen group is never changed, also when no name is planned for it.

**The catalog.** The row is read back on every primary of the catalog after
`add_node` and `remove_node`; a primary that cannot be read is a refusal that
the next check repeats.

**Tasks.** A cancel is refused when the task cannot be read, the guard of the
kind is asked again at each step boundary, and a cancel mark is ignored past
the point of no return. Migration 0013 refuses (`KX004`) a stop or a restart
of an instance of a server that an unfinished failover names as its old or
new server, and a restart for settings while any failover is unfinished.

**Open after this round.** A name in the synchronous list of a standby that
is never registered (FO-15) is left to increment 3, which refuses a failover
while a named standby is not in the catalog. On PostgreSQL 19rc2 the server
whose wall clock steps by an hour answers 503 for a read of the panel API
("the role of this session cannot be checked right now") in about one run of
five of the liveness test; it is in the tree of increments 0 to 2 as well, so
it does not come from these fixes, and its cause is not known.

### Increment 3 and the fixes for the second review (2026-11-18)

What the first failovers on a real cluster, and the review of the fixes
(`reviews/2026-11-18-kingresd-failover-fixes.md`, F2-01 to F2-17), changed
against sections 3 to 6.

**Every database with the extension has a catalog of its own.** The table of
nodes exists in each database in which `kingres_shard` is created, with node
identifiers of its own. A standby is therefore registered and removed in
every such database, not in the system database only; the leader repairs a
missing registration at every check; a failover resolves the old and the new
node by host and port in each database and runs `promote_node` and the read
back in each; and the preflight refuses (`cluster_inconsistent`) while a
database lacks a ready standby of the group. Section 4 spoke of "the
catalog"; it is one catalog per database.

**Term 0 is a term.** A cluster that never failed over is at term 0, and the
check of a stale term was skipped for a value that is not positive, so the
first failover had no protection. A negative value now means "not checked";
0 is compared like every other term.

**Roles and grants on the leader's own server.** A change of roles made
through the API was applied to the other servers and to one primary of the
leader's server. It is now applied to every writable instance of the local
server, so an instance promoted on the leader's server has the roles made
after the failover.

**An internal read may carry a query.** The client for calls between servers
refused every path with a query, so the reads of a position and of a state
(`?group=`) failed. The query is split off and checked against a narrow
pattern of its own.

**A promoted instance whose `kingresd` restarted.** Such an instance is held
back until the lease is held. The call that promotes answers
`409 lease_not_held` while a writable instance lacks the lease, and the step
then hands the lease over again; an instance that is starting (503) is waited
for.

**A fence and a lease that comes back.** The 503 of the liveness test on
PostgreSQL 19 (open in the section before) was not the clock: a server lost
its lease, the fence began to stop the primary, a renewal round won the lease
again in the same moment and "started" the instance that was still running,
the stop then ended it, and nothing started it again while the lease was
held. A fence now marks its group for the whole stop, and a round that wins
the lease meanwhile leaves the marker in place so that the next round starts
the instance. The same code runs on 18.

**The age of a verdict.** The verdict about lost data used the age of the
last check at the time of the decision, so every failover that took longer
than 30 seconds read "possible". It is the age of the last check less the
silence of the primary's server. After a restart of the leader's `kingresd`
the silence is not known; the verdict then reads "possible" and the task
pauses for a decision.

**Prepared transactions.** `recover_prepared_transactions()` is also run on
the promoted instance.

**From the review.** The list of a frozen group is changed in no branch of
the decision (F2-01). A freeze belongs to a task and an epoch: only its owner
lifts it, another caller gets `409 fenced`, the owners are kept in the freeze
file and a file that cannot be written is an error (F2-12). A release writes
its marker before it asks the grantors; an adoption keeps the marker file of
the directory it adopts, and only an attach or the withdrawal of a hand-over
removes it; an attach is refused when the lease is not held (F2-03, F2-04,
F2-11). A stop that failed in a fence is repeated every five seconds, and
whether a group is enforced is read at every check (F2-05). The idle watchdog
file is written by a loop of its own, and the rule for a group without a
standby is: its primary is never stopped for a lapsed lease, only for a
`kingresd` that stopped running (F2-07). The read back after `add_node` also
runs when the node exists already, and a server known to be dead does not
block it (F2-10). The state of a failover says whether a watchdog holds the
old primary's lock; the preflight refuses an old primary that runs without
one (F2-13). Migration 0014 refuses a failover or a restore while a stop or
restart of an instance, or a restart for settings, is pending or running
(`KX005`, 409); the other direction was migration 0013 (F2-09).

**Open after this round.** F2-06, F2-08, F2-16; the design text that F2-14
names; F2-02 has its check but no test that fails without it; an operation on
an enforced primary still runs when its record cannot be written (the rest of
F2-09). Not tested yet: a standby that lags at the failover, a second
failover while one is paused, and a lost answer of each internal call (the
fault hook cannot drop only an answer). The takeover points and the four
refusal tests were run once or three times each, not repeated as the
dead-server test was.

### Increment 4 (the old primary back) and the rest of the second review (2026-11-19)

What increment 4 did differently from sections 3 to 6, and the open findings
of the second review (F2-06, F2-08, F2-16, the other direction of F2-09, a
test for F2-02, the statements of F2-14).

**The record of the old primary is made at the promotion.** The failover
inserts the standby record of the old primary in the step after the
promotion, not later, so that a leader that ends there leaves a record the
next one finds. Migration 0015 adds the time since which the cluster waits
for the old server, and the report of the old primary on the group row
(method, bytes, message, divergence).

**How a directory is proved to be a former primary.** The role file says
primary of this group; the instance is stopped; the plan names another
primary; the term of the plan is newer; and the lease state says that this
server is held back, does not hold the lease and adopted nothing. A directory
that fails one of these is not touched; a primary that still runs is its own
error, and it is stopped by the fence, never by the rebuild.

**The order of the rebuild.** A mark that says the copy is unfinished, then
the role file, then the slot, then the data. The mark is cleared only after
the standby settings, the access rules and the meta file are written, and
then the instance starts. An agent treats a directory with that mark as a
standby whose copy must be made again, whatever else it holds (F2-08).

**`pg_rewind`.** Its dry run refuses a primary that was not shut down
cleanly, so the instance first runs crash recovery once and is stopped again.
The figures of the report come from the real run. With the default
`wal_keep_size` the WAL from before the divergence is usually gone and the
rewind fails; the rebuild then falls back to a base backup into the same
directory. A default `wal_keep_size` (or a slot for the purpose) would make
the cheap path the common one; it is not set yet.

**A server that holds the former primary is held back, and still has to be
told.** Its home instance may not start before it is a standby, so it cannot
read its catalog and did not know the leader: the calls that plan, rebuild
and report a standby answered 503 for five minutes. They now accept the
leader that the other members name (`GET /leader` of the peers); the call
proves what it needs itself.

**Placement.** The ring holds every active server, also one that holds no
standby; a server that is dead gets no new standby.

**When the old primary does not come back.** An alert after the smaller of
ten minutes and half of `standby-replace-after`; a replacement standby is
scheduled after `standby-replace-after` (30 minutes by default). When the
divergence cannot be measured the report gives a lower bound or none and
states the position in its message.

**An instance that runs as a primary where this server holds a standby role**
is stopped, and the alerts `oldPrimaryRunning` and `oldPrimaryNotStopped` are
raised from the lease conditions.

**A failover of two groups.** The expected term of the second group did not
include the change the first group made, so the second ended "superseded".
The terms of the earlier groups are added. This was a defect of increment 3.

**From the second review.** An instance that cannot be attributed to a group
(its role file contradicts the catalog, or two claims exist) is stopped by a
fence and blocks an adoption: the lease fails closed (F2-06). A stop or a
restart of an instance answers 503 when its record cannot be written and a
primary is enforced, and the promotion takes the same lock as those
operations (F2-09). The statements that F2-14 named, corrected: the fence
marker is one per group (`fenced.<group>`); a primary of a group without a
standby is never stopped for a lapsed lease, only for a `kingresd` that
stopped running; a stop that failed is repeated every five seconds until it
succeeds or the lease is won again, and `oldPrimaryNotStopped` is raised by
the monitor and by the failover task; that the catalog names no primary for
two claims is not a protection; a marker is removed by a won lease, by an
adoption, or when the rebuild made the directory a standby.

**Open after this round.** The findings of the review of increment 3 (F3-01
to F3-15) are not touched by it. Not tested: the promotion's lock by a test
that fails without it. A server whose home instance is a standby cannot write
alerts of the monitor (a read-only transaction). The reading of the topology
answers 503 at the formation of a cluster in about one formation in twenty.
The text of `notScheduled` in the contract no longer fits and waits for the
next revision.

### The fixes for the review of increment 3 (2026-11-19)

The review of increment 3 (F3-01 to F3-16) found four high defects by reading. What the fixes
changed against sections 3 to 7, and what the contract gained (0.12.0, additive).

**A failed failover does not take the lease from the primary it promoted (F3-01).** The release of
an adoption is refused when the adoption is attached to a running primary, and the call answers
that the lease is still held.

**Fencing of the old primary must be proved (F3-02, F3-12).** The leader records how many standbys
the plan of each group carried (migration 0018). When the old server does not answer and the leader
has no proof that its plan carried a standby, a group without a standby is never stopped by a lapsed
lease, so the failover is refused with `cluster_inconsistent` unless the caller allows a loss of
data; a dry run shows it as a blocker. The other way, a lease that fences whatever the plan says, is
not built.

**The verdict about lost writes (F3-03).** The time in which the old primary was not observed is
taken only from the silence that a majority of the members share, and only what exceeds the lease
and its margin; the leader asks the other members. A leader that alone lost the old server no longer
concludes "none".

**A failover and a change of the databases exclude each other (F3-04).** The preflight checks that
the old primary is in the catalog of every database. A failover is refused while a database is
created or dropped or a table is distributed, and those are refused while a failover is unfinished
(migration 0016; the database operations answer `failover_in_progress`, the failover
`operation_in_progress`). A failure to read the targets after the promotion is a pause, never a
failed task.

**Smaller ones.** A promotion that was begun and not finished is finished, not refused (F3-05). A
pin that can be read is kept; one that cannot is withdrawn while nothing is promoted (F3-06). A
primary that still runs is stopped only after some standby was read (F3-07). The attach of the
promoted instance answers `lease_not_held` on both paths (F3-08). The entry point of a server whose
own catalog cannot be read takes the nodes from a peer (F3-09). Prepared transactions are recovered
and the cluster checked in every database (F3-10). A standby that is found out of recovery and that
this task did not pin pauses the task with the alert `splitPromotion`; it is adopted only when a
loss is allowed, with the verdict "possible" (F3-11). The calls that demote, promote, change a lease
or recover prepared transactions name the group, and demote and promote need the fence header
(F3-13). A freeze is lifted only by the task that set it (F3-14). A failover that ended with an
incident of the cluster check ends `completedWithWarnings` (F3-15, migration 0017).

**The contract, 0.12.0.** `FailoverReportSummary.outcome` has `completedWithWarnings`; the problem
of a 409 `failover_needs_decision` carries the plan; the text of `notScheduled` is corrected.

**The lease code.** Renewal rounds, releases and transfers returned at a majority and left requests
in flight; a late one could reach a grantor after the group was released. A release now waits for
the requests of its group.

**A member that does not know the leader** answers 503 with `Retry-After`, and a client repeats; it
does not forward or queue. The window without a leader is the election plus the lease. Tests that
read the topology or a task once while the state moved now wait through that answer.

**Open after this round.** The test of the entry point (F3-09) passes without the fix on a real
cluster, so only a unit test shows it. No cluster test for F3-05, F3-08 and F3-10. Real writers on
a primary that is cut off or killed are not tested for F3-03. Not covered of F3-16: a standby that
lags, a lost answer of a call, an instance killed under writers. Of increment 5 (the failover of the
map group) only the journal of a bootstrap exists (`internal/failover/journal`), used by nothing.

### Increment 5 (the failover of the map group) and the open items of the third review (2026-11-19)

**What was built.** A request to fail over the server that runs the primary of the map group (the
leader) is answered by any member that cannot hear it, in mode `hard` only. The member relays it to
a candidate, a server with a ready registered standby of group 0, over the internal call
`POST /internal/v1/failover/map/start` (`/continue` for a hand-over). The candidate runs the
bootstrap B1 to B8 from a journal that survives a restart of `kingresd`:

- **B1** decides: the catalog names the old server primary of group 0, a ready standby is here, a
  majority is heard.
- **B2** freezes locally.
- **B3** asks the old server to demote if it answers, and takes the fence lease from a majority.
- **B4** reads the positions and ranks the standbys as an ordinary failover does, and writes the pin
  once. If another standby is better, the lease and the journal are handed to it.
- **B5** takes the holder lease and makes the same promote call as the task.
- **B6** runs `promote_node` on the promoted instance itself in every database. It repeats every 5
  seconds while the majority rule of the extension refuses; after 60 seconds it raises
  `failoverBlocked`, and after 10 minutes it pauses.
- **B7** waits until the local catalog names this server primary of group 0 with a higher term,
  moves the system database and pauses the rebalance that was running (outcome stopped,
  `stopped_by` safetyRule, "The map group was failed over."). It then records the run as an
  ordinary failover task, whose group-0 steps up to F7 are done.
- **B8** checks for the key of the certificate authority and opens the gate of the task engine.

While the bootstrap runs, `GET /tasks/0` and `GET /failover` are answered by the candidate. Task id 0
never names a stored task. REST contract 0.13.0 (additive) has this, and the alert
`certificateAuthorityUnavailable`.

**Departures from the design.**

- **The key of the certificate authority is not escrowed** (section 11.2). No sealing key could be
  derived from what exists without a new construction, and the cluster secret lives in the
  extension, not in `kingresd`. The bootstrap runs without it. A new leader that has no
  `pki/ca.key` raises the critical alert `certificateAuthorityUnavailable` at B8. No certificate can
  then be issued, renewed or revoked until the operator restores `pki/ca.key` and
  `pki/revoked.json` from the old leader's backup and restarts `kingresd`.
- **Authority of a manual request.** The member that receives the request authenticates the
  operator and relays it. The candidate does not check the role again; it proves everything else
  itself (the catalog, the standby, the majority, the lease, the extension's majority rule). The
  design asked for the same role check as on the leader; this is weaker.
- **A restart of the candidate** undoes B3 and B5 (idempotent) when B6 is not done, because the
  adoption of the lease lived in memory. Members then come from the peer directory when the
  catalog cannot be read.
- **The stores follow the promoted instance.** The task engine's store and the REST store were
  bound to the address of the home instance; they now follow the system database where it moves.
  The sign-in authenticator stays on the home instance.
- **A promoted copy keeps the rule that lets `kingresd` connect to it.** The rules a restarted
  `kingresd` rewrote for a primary dropped the `kingres_standby` line from `pg_hba` and `pg_ident`.
  This also affected every ordinary promoted instance after a restart.
- **Not built:** monitors for an automatic failover of the map group (increment 6), `graceful` and
  the dry run for it, and the second condition of the gate (the group-0 instance in recovery). When
  the extension's majority can never be reached, the bootstrap only pauses after 10 minutes, with no
  operation of its own for the operator.

**The open items of the third review.** F3-05, F3-08, F3-09 and F3-10 have cluster tests. F3-05 and
F3-08 use one-shot fault points that stand in for their causes: a promote call that ends after the
role file, and a lease that cannot be attached. F3-09 uses a failover paused after the old primary
stopped, so the old server's catalog cannot be read. The test of F3-10 shows that prepared
transactions are recovered in every database, but makes no prepared transaction of its own.

**Open.**

- In 2 of 12 runs, the last step of two tests (resuming a paused failover after the heal and a
  repeated request) timed out after 6 minutes. The assertion each test is about had already passed,
  and the cause is not known.
- A map group failover whose old server also held primaries of ordinary groups is not tested.
- A hand-over at B4 and a demotion at B3 by a live old server are tested only with fakes.
- A kill of the old leader under writers is not tested.

### Increment 6 (automatic failover, 2026-11-19)

Built to section 4 without a change of the contract (the settings, the reset, the alerts `autoFailoverTriggered`, `autoFailoverLimitReached` and
`failoverBlocked`, and the trigger `automatic` of the report existed in 0.10.0); no migration was needed (`failover_run.counted` and the trigger
exist from migration 0015).

- **One rule, two loops.** `failover.Evaluate` is a function of a view (settings, the servers with their silence and the corroboration, the
  majority this server hears, an unfinished task, the end of the last failover of each group) and has a test for each row of 4.2 and 4.3.
  `failover.Auto` runs it every 5 s on the manager leader (`cmd/kingresd/autofailover.go` reads the view; it does nothing on a server that is not
  the leader and reads nothing while the setting is off). The candidate monitor of the map group (`cmd/kingresd/mapmonitor.go`) runs the same rule
  on every server that hosts a ready standby of group 0, for the one case in which the leader's server is the server that is gone.
- **Evidence.** The server's own tracker says "dead" for at least the effective timeout, `max(timeoutSeconds, 3 x failure-timeout)`, and a strict
  majority of the members (this server included) report the server as not live (`GET /liveness`); a server that hears no majority starts nothing.
  Exactly half is not a majority.
- **Order of events.** The loop stops a rebalance first (`StopForFailover`, then `WaitStopped` for up to 60 s per look) and starts the task only when
  it is stopped; a failed start backs off 60 s, doubling to 10 minutes, per server; the cool-down of 5 minutes holds back a server one of whose
  groups was failed over lately. The alert `failoverBlocked` ("no standby to promote") is raised and cleared per group by the loop.
- **The counter.** `autoFailover.eventsSinceReset` is incremented once per automatic task, in the transaction that records the first promoted
  group (`CountAutomaticSQL`, guarded by `failover_run.counted`; the map group's run is recorded with it). A rebalance that completes and
  `resetAutoFailoverCounter` set it to 0. At the limit the loop raises `autoFailoverLimitReached` once and clears it when the counter is lower.
- **The map group.** The candidates are ranked by the id of their registered standby; rank r waits `r x 30 s` after the evidence holds and looks
  again at the same leader and term before it starts the bootstrap with the trigger `automatic` (never with `allowDataLoss`, so the rule of the
  journal verdict decides). The lease still makes the choice safe whatever the order: one holder, and B4 hands the journal to a better candidate.
  The settings come from the replica of the system database that the candidate holds.
- **Alerts and audit.** `autoFailoverTriggered` (info, subject `cluster`) is raised when an automatic run ends (the report step of the task; the
  promotion record of the map group); the audit event `autoFailover` is written when the loop starts the task.
- **Tests.** Unit: the table of `Evaluate` and the loop with fake ports (`internal/failover/auto_test.go`). Cluster: T13
  `TestAutomaticFailoverRulesAndTheCounter` (off: nothing; on: one automatic failover, the counter, the limit alert, the reset) and
  `TestAutomaticFailoverOfTheMapGroup` (`internal/clustertest/failover_auto_test.go`).
- **Not built and not tested:** two failures at once (T7 needs five servers: a cluster of three has no majority after the second), a rebalance that
  is running when the failover starts (covered by the unit test of the order and by the rebalance's own test of `StopForFailover`, not by a cluster
  test), `failoverNeedsDecision` for an automatic run with an asynchronous standby (the journal and the task already ask; no cluster test), and the
  stall tests of T14.

### Increment 7, round 1 (the planned switchover, 2026-11-20)

`failoverServer` with `mode: graceful` for ordinary groups, built to section 5 with these departures; the restore of the preferred primaries and
the dry run for a switchover are round 2. The contract is unchanged (0.13.0, text only: the steps and codes in the description of the operation,
`group_degraded` used for the first time).

- **Steps per group** (`internal/failover/switchover.go`): `Freeze`, `Choose the standby`, `Stop the writes`, `Let distributed transactions end`,
  `Stop the old primary cleanly`, `Verify the new primary`, then `Wait for the old lease`, `Hand the lease`, `Promote` and F7 to F13. The choice is
  made first so that the standby that must replay the mark is known; only a standby that streams from the primary is a candidate, and the verdict is
  `none` by construction. S4 and S5 are one step (mark, the standby replays the mark within 10 s, fast shutdown); S6 reads the shutdown
  checkpoint again with the same idempotent call and asks the standby, repeating for 30 s. S7 is the existing fence step: the lease was released by
  the clean stop, so the leader's first round obtains it and then hands it to the new server as in a failover.
- **Stop of the writes** (`cmd/kingresd/switchover.go`, `internal/instance/quiesce.go`): the group is added to a set that the entry point reads
  (the proxy then sees a group whose lease is not held and sends new connections to another group's primary), `pg_hba.conf` is replaced by the hold
  rules (replication lines, `local` lines and the service role's loopback lines; the previous file is kept next to it), and every client session
  that is not the service role's is ended; the leader repeats the call every second for two minutes. The hold is not persisted across a restart of
  kingresd.
- **Rollback** (`rollbackSwitchover`): the leases that the run holds are released (the leader's fence adoption, the new server's holder adoption),
  the old primary is resumed (`Manager.Resume`: the hand-over marker is removed and, when the instance does not run, the wait marker is written so
  that the lease rounds start it) and the writes are let in again. A step that fails with a code does it before it returns; a cancel and any other
  failure do it in `OnFail` for the groups that were begun and not promoted. The cancel guard refuses from the start of `Promote`.
- **Departures.** (1) The old primary becomes a standby through the rebuild of increment 4 (`pg_rewind`, which has nothing to rewind after a clean
  stop), not through `/standby/adopt {clean}` of 6.3; the report therefore does not say `method: clean`. (2) The phases `quiescing`,
  `waitingForTransactions`, `stopping`, `verifying` and `writesPausedSince` of `GET /failover` are not reported; the pause of the writes is the
  event `writesResumed` (`pauseMs`) of the report. (3) The map group is refused for `graceful` (422); it needs the bootstrap with a live old server.
  (4) The wait for prepared transactions looks at the old primary only and asks every catalog database to recover them every ten seconds.
- **Tests.** Unit: `internal/failover/switchover_test.go` (every step, every rollback, the refusals, the cancel guard, `OnFail`) and
  `internal/instance/quiesce_test.go`. Cluster (`internal/clustertest/failover_switchover_test.go`): T12 `TestSwitchoverOfAGroup`, T12b
  `TestSwitchoverVerificationFails` and `TestSwitchoverWithAnUncleanStop`, T12c `TestSwitchoverCancelBeforeAndAfterThePointOfNoReturn`. The faults
  that stand in for the causes are callee-side points of the fault file: `quiesce:stall`, `verify:false`, `demote:state`.
