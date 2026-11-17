<!-- SPDX-License-Identifier: AGPL-3.0-only -->

# Independent review of the stabilisation and second rebalancing fix round of kingresd

Commit reviewed: the one that follows the fix round described in `2026-11-16-kingresd-rebalance-fixes.md`
(its message names the causes of the failures of complete integration runs and the fixes of RBR-01 to RBR-07).
Earlier reviews: `2026-11-16-kingresd-rebalance.md` and `2026-11-16-kingresd-rebalance-fixes.md`.
Reviewer: an independent reviewer, not the author of the code. Paths are repository-relative; `cp/`
abbreviates `kingres/control-plane/`. Times are as measured on a loaded shared machine; where a number depends
on the lease settings of the tests (6 s lease, 2 s margin, 1 s renewal) the defaults (10 s, 3 s, 2 s) are given too.

## Scope and method

**Read.** An export of the committed tree (`git archive` of `kingres` and `contrib/kingres_shard/README.md`) in a
scratch directory outside the repository; the working tree, which another person was changing, was not used. Read
closely: the whole diff; `cp/internal/internalapi` (httpserver.go, internalapi.go), `cp/internal/instance/manager.go`,
`cp/internal/lease` (manager.go, watchdogfile.go), `cp/cmd/kingres-watchdog/main.go`, `cp/cmd/kingresd`
(main.go, internal.go, ha_leases.go), `cp/internal/peerclient`, `cp/internal/fence`, `cp/internal/task`
(task.go, pgstore.go), `cp/internal/rebalance` (guard.go, pg.go, loop.go, retry.go, service.go), `cp/internal/standby`
(agent_ops.go, agent_primary.go, syncnames.go, leader.go), `cp/internal/membership` (handle.go, remove.go, add.go),
`cp/internal/auth/retry.go`, `cp/internal/pgconfig/converge.go`, `cp/internal/pki/files.go`, the README of the
control plane (the two named sections and "Leases and fencing"), and, in the extension, `node_conn.c` and the
`bucket_move` table of the 0.5 script.

**Run.** Unit tests on Windows (`go test ./...`: every package passes, apart from my own throw-away tests).
Integration tests in WSL, selected tests only, from my own build of the export. The machine-wide queue of
`scripts/run-integration.ps1` was held for more than half an hour by a complete run of the other person, so my runs
were made from my own directory with a port range of my own (57000-57199) and the private PostgreSQL 18.7
installation of the machine used read-only, one run at a time, using the same environment variables as
`scripts/run-wsl-tests.sh`; every process I started ended and none is left. PostgreSQL 19 was not used. The mutants
were made by editing one line in my copy, running the test that claims the fix, and restoring the line.

Every finding below says how it was shown. A finding that is only by reading says so.

## Summary

Counts: critical 0, high 2, medium 3, low 5, note 3 (13 in all).

| Id | Severity | Summary |
|---|---|---|
| ST-01 | high | The lease watchdog stops a healthy primary with an immediate shutdown when its group stops being enforced (the group's standby is removed, because its server was removed or the replica count was lowered). **This is the cause of the failure the round did not explain.** Reproduced 2 times in 5 runs of a removal, and every time when lowering the replica count |
| ST-02 | high | `RunMove` reads a failing observation session as "the call is not running" and cancels a call whose move the extension has recorded (the cancel in the switch that the round set out to end); shown against a stub of the extension |
| ST-03 | medium | A call inside a task step is repeated after the connection fails, also when the server did the work and only the answer was lost: `POST /standby/rebuild` rebuilds the standby twice, `POST /config/restart` restarts twice, and a call that hangs is made three times (30 minutes instead of 10) |
| ST-04 | medium | The lifecycle lock does not honour the caller's context, and the lease manager's start of the primary (which holds the lock the fence needs) now waits behind other callers' stops and starts; a fence waited 5.2 s behind a restart in a test |
| ST-05 | medium | A row left by an earlier holder is found by comparing the system database's clock with the extension's `started_at` with no tolerance; the README says they are one server, which they are not; no test |
| ST-06 | low | `retryRows` repeats `StartMove` and `AddSkipped` after a lost answer: a duplicate `running` row and a duplicate `skipped` row (shown) |
| ST-07 | low | A new holder gives up on a live long move after 10 minutes (`RecoverWait`) and fails its step (shown with a shortened wait) |
| ST-08 | low | A server that can never apply the configuration costs one task (and its rows) every 60 to 90 s now, and tasks are never deleted (by reading) |
| ST-09 | low | Gaps in the tests of the round: the write deadline, the converge loop, RBR-05, the call sites of two fixes and the reset of the fenced counter have no test that fails without them; the lease tests force enforcement and so cannot see ST-01 |
| ST-10 | low | An unauthenticated caller of `/bootstrap/join` holds a goroutine for 10 s per request while the instance is unavailable, with no cap (by reading) |
| ST-11 | note | The lifecycle lock is per process; two `kingresd` on one data directory bring the original hang back, and nothing prevents two |
| ST-12 | note | Statements in the README and in `loop.go` that are not true (where the system database is; no clock compared) and a contract that is silent on the lease window |
| ST-13 | note | The public listener keeps its 60 s write timeout from the request; I did not enumerate the handlers that can outlast it |

No finding is critical: no data was lost or doubled in any run, a superseded holder acted in none, and no
authentication or authorization bypass was found. ST-01 is high because a healthy primary is crashed, every time,
by an operator action the product documents (removing a server, lowering the replica count), and the cluster's own
tests do not see it.

## Fate of RBR-01 to RBR-07

"Mutant" says whether I broke the fix in my copy and what the test that claims it did.

| Id | Fate | Evidence | Mutant (mine) |
|---|---|---|---|
| RBR-01 | fixed, with one hole (ST-02) | the withdrawn call is no attempt (`UndoStart`) and three in a row stop the run; but the check that decides "withdrawn" ignores the error of its own query | `UndoStart` removed: `TestAWithdrawnCallIsNotAnAttempt` fails; bound of three removed: `TestWithdrawnCallsInARowStopTheRunWithoutFailingTheBucket` fails |
| RBR-02 | fixed in the loop; the retried writes are not all idempotent (ST-06) | `retryRows` wraps every write; a blip mid-move no longer fails the move | one try instead of four: `TestABlipOfTheSystemDatabaseMidMoveDoesNotFailTheMove` / `TestASystemDatabaseThatStaysAwayPausesTheRun` fail |
| RBR-03 | fixed | `Current` leaves repaired rows out | predicate removed: `TestTotalsDoNotCountTheRowsOfAnEarlierRoundThatWereRepaired` fails |
| RBR-04 | fixed | advisory shared lock on a session outside a transaction; the claim skips; the lock key space does not meet the other advisory keys (see "What held"); a void guard under a valid lease is proved again | not run (PostgreSQL tests of the guard were not mutated, see "Not covered") |
| RBR-05 | partly: the id above a mark is sound; the time path (ST-05) is not | no test of its own, as stated; my test shows the time path failing with a clock 50 ms apart | no test to break |
| RBR-06 | fixed as an interface; its limits matter for the failover (see "For the failover work") | `StopForFailover` sets the flag; the wait for a leftover and for a lock ends at the flag; a call in flight waits for the extension | not run |
| RBR-07 | fixed | the fifth fenced run in a row fails the task with `fenced`; a step that ends resets the count | bound raised: `TestACalleeThatAlwaysRefusesTheFenceFailsTheTaskAfterABound` fails; reset of the count removed: **survives** |

First review (RB-01 to RB-14) and the second (RBR-01 to RBR-07): the unit tests of `internal/task`, `internal/rebalance`
and `internal/lease` all pass on this tree (every package of the module, my own failing test aside). I did not repeat the
real-cluster runs of the earlier reviews.

## The causes the commit names for the failures of complete runs

### 1. "tls: bad record MAC" under a handler that ran long

Cause: plausible and tested (the test keeps the old configuration as a control and fails with it). Fix: the internal
listener has no `WriteTimeout`; the deadline of the write is set when the first byte of the answer is written.

Is the fix complete? For the listener, yes, with two remarks. (a) Mutating the fix away does not fail the test the
way the README says: a server rebuilt *with* `WriteTimeout: 1s` and the late deadline in front still answers a 1.5 s
call, because the late `SetWriteDeadline` overrides the one the server set. So the listener is safe because of the
late deadline, not because the field is absent; and the late deadline itself has no test: with the call to
`SetWriteDeadline` removed every test of the package passes (ST-09). (b) The public listener (`cmd/kingresd/main.go`)
keeps `WriteTimeout` of 60 s counted from the request, the same configuration that caused the symptom; one handler
(`handlers_ops.go`) extends it by hand, I did not enumerate the others (ST-13).

Attacks on the listener, all against `NewHTTPServer` in my export:

- A request whose body never arrives: with HTTP/1.1 the 30 s `ReadTimeout` ends it (by reading, not run); with
  HTTP/2, which `ServeTLS` enables, the handler's read ended after 30 s with `i/o timeout` (shown: the
  unauthenticated bootstrap path, a stalled body, protocol `HTTP/2.0`).
- A handler that runs longer than 30 s: its request context was alive after 45 s on HTTP/1.1 for a GET and a POST
  (shown), so `ReadTimeout` does not cancel a long call.
- A client that never reads the answer: bounded by the late deadline, 30 s from the first byte (by reading; the
  deadline is not tested, see above).
- A handler that never returns: nothing bounds it on the server; it ends when the caller closes the connection
  (request context) or when the handler ends by itself. The callers' own client timeout (10 minutes for the members'
  client) is the only bound, and ST-03 shows that a retry multiplies it.
- No limit on the number of connections or of handlers exists on the internal listener (as before); the
  unauthenticated calls are the bootstrap ones.

### 2. A stop that waited for a pid file written by a start (the lock per data directory)

Cause: right, and the fake `pg_ctl` test reproduces it (`TestAStartDuringAStopDoesNotMakeTheStopFail` passes on this
tree). Is the fix complete? The serialization does what it says and I found no lock cycle: while the lifecycle lock is
held the code calls `pg_ctl`, the start gate (file tests only) and `Running`; nothing takes the standby agent's `opMu`
or the lease manager's `m.op` under it; the other direction (`opMu` then lifecycle, `m.op` then lifecycle) is a
chain, not a cycle. `StopImmediate` and `StopForShutdown` are unlocked and returned in 0 s while a start held the
lock (shown). What is wrong with it is in ST-04 (not bounded by the context, and the lease manager waits behind it)
and ST-11 (per process only).

A fencing stop racing a locked start: the start gate is evaluated after the lock is taken, so a start that waited
behind the fence sees the marker and is refused. The race that remains is the one that existed before: between
the gate and the moment `pg_ctl start` creates its pid file the instance does not count as running, so an
immediate stop in that window finds nothing and the start then succeeds with the marker present. The marker keeps
the next start away, the watchdog covers the running instance, and the manager fences once per loss; I did not
make this window deterministic.

### 3. The catalog that differed after a task changed holder (the call is not cancelled from the record on)

Cause: mechanism plausible and shown by the author at the fault point; the state of the original failing run was not
reproduced, as the message says. Is the fix complete? No: ST-02. The rule "from the record on the call is not
cancelled" depends on the loop seeing the record, and the loop sees it through a session whose errors it discards, so
one more path ends in `cancel()` on a recorded move. A superseded holder's call that keeps running while the new
holder acts: the new holder finds the move by its id and waits for it (`Recover`), the extension's own rule of one
move per bucket keeps the two apart, and the superseded one writes nothing because every write goes through the
lease check. The wait of the new holder is bounded by 10 minutes, which a long copy can exceed (ST-07). The call
itself is not bounded by anything on the control plane's side after the record: the goroutine of `RunMove` stays in
`<-errc` until the extension returns, and `execute` returns at once when its context ends (the goroutine ends later,
no connection or goroutine accumulates beyond one per running call).

The withdrawn-call rule: the call is judged by `pg_stat_activity` on the `pid` of its own backend, which is the right
key (the connection is held by this process, so the pid cannot be recycled while it is open; another session with
similar text cannot match). It cannot be judged withdrawn while running by that key, but it can by the error path
(ST-02), and there is a race of one poll in which a call that has just returned has its answer discarded by
`withdraw` (`<-errc` is read and dropped): a move that finished is then reported as not started and the row is taken
back. I did not show that race; it needs the call to end inside one 100 ms poll after the hold.

### 4. A follower that refused a read during a short loss of the leader (a test change)

The author's reading is right as far as I can check: the follower that runs a primary with a standby serves only
while its group lease holds (README, "Leases and fencing"), and the test before had used the lease up with its
partition. `lvLeaseJustRenewed` makes the test start when more than 6 s of the 7 s remain. With the defaults (10 s,
3 s) the follower serves between 4 and 7 s after a lost majority. Is a 503 or a refused connection for up to 7 s after
a 4 s blip acceptable? Not for a blip of the leader alone: a follower that still has a majority of voters keeps its
lease and serves, which is what the subtest proves. After a *full* partition longer than the lease, the instance is
fenced and comes back only at the next renewal round after the heal plus a start with crash recovery; the README
documents the 503 answers for sign-in and the role check (decision 12 and the "Waiting for the instance" section),
but the OpenAPI contract says nothing about this window beyond the lease view (ST-12). The test change does not hide a
product defect in this case. It does show the cost of what ST-01 does: the same "instance is simply not there" state is
reached by a healthy group without any partition.

The rule for `Confirming()` is as stated (an enforced group without a valid lease and not yet judged lost; false after
a loss and while a failed renewal still leaves the lease valid); its two unit tests fail when I make `lost` count as
confirming. The gated and retried readers never turn a refusal into an acceptance: only `ErrUnavailable` is retried,
every retry asks again from the start (the role is read again, not remembered), the wait ends with the request's
context, and after it the answer is still 503. The cost is ST-10 (goroutines held for 10 s).

### 5. Joining and resetting

By reading and by the round's own tests (not rerun on a cluster): `FinishMember` publishes the connection settings and
runs the migration before the state becomes `clustered`; `internalCertificate` presents the bootstrap certificate for
a member only while `Resetting()`; `EnsureBootstrapCert` is under one mutex and checks that the files make a pair (the
mutex mutant fails `TestBootstrapCertIsCreatedOnceUnderConcurrentCalls`, the `resetting` mutant fails
`TestInternalCertificateDuringAReset`). A server that presents the bootstrap certificate cannot be mistaken for a
member: the peer client verifies the server certificate against the cluster CA and a self-signed one is
`x509.UnknownAuthorityError`, which the client maps to `ErrNotMember` (no downgrade); the calls that need a member
certificate need a *client* certificate that the verifier checks against the CA files, which a reset server no longer
has, so it answers 401. What the bootstrap calls allow during the window is the same as when uninitialized (setup
token or administrator credentials, rate limited), and the token handler answers 409 "already initialized" while the
token file is gone. A revoked server's path back in is unchanged: it needs a new `server.add` by an administrator.

### 6. Servers that stayed behind a configuration

See ST-08. The change from ten minutes to one for the same configuration is not covered by a unit test; it is
exercised only by `TestConfigServerGroupAndServerLevels` on a cluster, which I did not run.

### 7. The failure the round did not explain

Found: ST-01. It is the one thing in this review I consider more valuable than the rest, so the evidence is given in
full in the finding.

## New findings

### ST-01 (high) - the watchdog crashes a healthy primary when its group stops being enforced

**Where.** `cp/internal/lease/watchdogfile.go` `writeWatchdogFile` (returns without writing when
`!m.c.Enforced(group)`), `cp/internal/lease/manager.go` `afterAcquired` (writes the file and ensures the watchdog
only when `Enforced`), `cp/cmd/kingres-watchdog/main.go` (a detached process; fences when the lease file has not
changed for the `validMs` it last read; exits only when the instance is not running or an orderly stop wrote the
stand-down file), `cp/cmd/kingresd/ha_leases.go` `watchdogRunner` (starts it, never stops it).

**What happens.** A group is "enforced" while it has at least one standby (`Standbys().HasStandby`). The first
successful renewal after that starts the watchdog next to the instance and kingresd rewrites `lease.json` after every
renewal. When the group loses its last standby the group is not enforced any more: the manager is right not to
fence, and it stops writing `lease.json`. The watchdog, which nobody stops, keeps reading the same file, sees no new
sequence number, and after the last `validMs` (at most `lease-duration - lease-margin`, 7 s with the defaults, 4 s
in the tests) it writes the fence marker and runs `pg_ctl stop -m immediate` on a primary that has its lease and
needs no standby. The manager then finds the marker (`Waiting`), and at its next round starts the instance again,
which runs crash recovery. No `leaseLost` event is raised: the manager never lost anything. Whatever removes the last
standby of a group triggers it:

1. A server is removed. The task first removes the standbys that run on the server (`PurgeServer`), and a group whose
   standby was on that server stops being enforced. The group is a *healthy group of another server*, which is
   exactly the unexplained failure: "a healthy group that refused connections, for seconds, after ANOTHER server was
   removed".
2. The replica count is lowered to 0 (`PATCH /settings/cluster`), which every group takes as "no standby".
3. (By reading, not run) a standby that is deleted for any reason and not yet replaced.

**Shown.** Test `TestReviewRemovalUnderLoad` in my export (three servers with standbys, `-lease-duration 6s
-lease-renew 1s -lease-margin 2s`, a table distributed over the groups; for each of the two servers that stay, every
50 ms a connection, a distributed read and a distributed write; then `DELETE /servers/<third>`). Five runs: three
passed, two failed. In the failing runs, for example:

```
removal task done at +4s
714 sample rounds, 58 failures
+3.36s node 1 dist write: sqlstate=- unexpected EOF
+3.4s ... node 1 connect: dial tcp 127.0.0.1:...: connect: connection refused      (until about +4.8 s)
+4.9s ... node 1 connect: FATAL: the database system is starting up (SQLSTATE 57P03) (until about +6.5 s)
```

Kept logs of one failing run: the watchdog log of that server says `kingres-watchdog: the lease has run out, stopping the
instance`; the PostgreSQL log of its instance says `received immediate shutdown request` at 07:04:35.2 and later
`database system was not properly shut down; automatic recovery in progress`; the audit log of the server that was
removed says `standbyInstanceRemoved` for that group, the audit log of the group's own server says
`synchronousStandbyNamesChanged ... ""` for that group, and **no** `leaseLost` or `leaseFenced` event exists for it
(the removed server's own group does raise `leaseLost`, as it should); the file `lease.json` was written again only
after the group got a new standby, with a later modification time. A sequence of the instance's own log: the kingresd
of the server printed `the group lease is held: the primary instance was started` at the end of the 3 s of recovery
and nothing before, i.e. the in-process manager did not see a problem at all.

The deterministic version, `TestReviewLoweringReplicasFencesHealthyPrimaries` (same cluster, no removal; connection to
every instance every 100 ms; at +3 s `PATCH /settings/cluster {"defaultReplicaCount": 0}`): all three primaries were
stopped by their watchdogs, 112 refused or "starting up" connections, first at +7.1 s, last at +11.1 s; the watchdog
log of all three servers has the line above. One run; the mechanism does not depend on timing.

SQLSTATE 08001 is what a statement of another group gets from the extension when it must connect to the stopped
node (`node_conn.c`: `could not connect to node`); I did not run that statement during the window, I sampled the
instances themselves. The author's test `TestRemovingAServerCostsNoOtherGroupItsLease` cannot see this: it
sets `ForceEnforce`, which makes every group count as enforced whatever its standbys are, and the real removal of a
standby then never changes `Enforced`. The test that has real standbys (`TestRemovingAServerRemovesItsStandbys`)
does not sample the instances of the groups that stay.

**Expected.** Ending the enforcement of a group must end the watchdog's claim on its instance: no fence, no
interruption of connections, no crash recovery.

**Direction.** When `Enforced(group)` turns false, tell the watchdog (write the stand-down file the watchdog already
reads for an orderly stop, or write `lease.json` with a validity that never runs out and let the file be removed when the
group is enforced again), or have the watchdog ask kingresd whether it is still needed; and add a test that
removes a server (or lowers the replica count) with real standbys and samples every instance, without
`ForceEnforce`. The same hole exists in the other direction for the failover work: the watchdog must be told whenever
the set of groups a server is primary of changes (a promotion, a demotion).

### ST-02 (high) - a failing observation session makes the loop cancel a recorded move

**Where.** `cp/internal/rebalance/pg.go` `RunMove`: `findMove(...)` is called with `err == nil && found && e.id > after`
(an error means "not seen"), and `if running, _ := b.callRunning(ctx, watch, pid); !running` discards the error of
`callRunning`, which returns `false, err` when its query fails; then `withdraw` cancels the call.

**What happens.** The call and the observation use two sessions. If the observation session fails (it is killed, its
network path fails, the instance restarts it only, a statement timeout) while the call session is fine, every
`findMove` fails silently, the record is never seen, the guard is never released (it is kept alive instead), and
after `GuardHold` (30 s) the question to `pg_stat_activity` fails too; the failure is read as "not running" and the
call is cancelled. If the extension has recorded the move meanwhile (it is copying, catching up or switching) this
is the cancel that the previous round removed because it leaves the move in `switching` or prepared transactions
behind (`check_cluster` then reports `catalog_differs` and `partition_misplaced`, the next holder stops). The loop
also reports the call as `NotStartedError`, which is not an attempt: the row of a first try is deleted
(`UndoStart`), and the move that exists in the extension has no row in the report.

**Shown.** `TestReviewBrokenWatchSessionCancelsARecordedMove` (PostgreSQL 18, a stub `kingres` schema with a
`bucket_move` table and a `move_bucket` that sleeps 40 s, `GuardHold` 2 s). At 0.7 s the observation session is
terminated with `pg_terminate_backend`; at 1 s the test inserts the extension's record (phase `switching`):

```
terminated 1 watch session(s)
RunMove returned after 2s: the move was not started: the extension did not record the move within 2s and the call is not running
  (NotStartedError=true, ErrLeaseLost=false)
record of the move in the extension: 1 row(s) in 'switching'; the call was cancelled mid-statement
```

How likely is the trigger in production? It needs the observation session to fail while the call session lives for
the whole hold; I have no evidence of it happening. It is an error path that decides to take an irreversible action
(cancel) on a failed observation, which is the pattern the round's own README forbids ("from the record on the move is
the extension's"). By the extension's own recovery the data stays correct.

**Expected.** An observation that failed says nothing: no withdrawal on an error of `callRunning` (keep waiting, or
stop watching and wait for the call), and a failing `findMove` for the whole hold should be retried on a new session.

**Direction.** Treat an error of `callRunning` as "running" (the bound `guardMaxWait` still ends the wait), reopen
`watch` after a failure of it, and add a test with a killed observation session.

### ST-03 (medium) - a repeated call in a step repeats work that was done; a hung call is made three times

**Where.** `cp/internal/peerclient/peerclient.go` `repeatable` (`inStep || GET`), `transient`, `do`.

**What happens.** The classification "every call a task step makes may be repeated" holds for calls that are
idempotent, and the code relies on the callee being so, but the case that matters is "the server did the work and the
answer was lost": the request is sent again with the same fence token, which the callee accepts (equal epoch). I
listed every internal POST handler that a step can reach and what a second execution does (by reading, except where
shown):

| Call | Second execution |
|---|---|
| `/instance/create` | no-op when the instance exists (it trusts, starts, waits); a second call that arrives while the first still initialises has no lock of its own besides the lifecycle one |
| `/instance/start`, `/instance/stop` | no-op (under the lifecycle lock) |
| `/instance/remove`, `/node/finish`, `/node/reset` | no-op, or answered as already done / certificate refused (the step handles 401) |
| `/instance/trust`, `/pki/install`, `/peers`, `/config/apply`, `/standby/plan`, `/globals/apply` | rewrite the same files or rows, reload: idempotent |
| `/apply` (`database.create` and the other kinds) | `createDB` checks that the database exists; I did not read every kind |
| `/standby/create`, `/standby/remove` | no-op (they check what exists) and serialized by the agent's `opMu` |
| **`/standby/rebuild`** | **not a no-op**: a standby that was rebuilt is rebuilt again (rewind and restart, or removal, new slot and a new base backup); the group runs without a streaming standby meanwhile |
| **`/config/restart`** | **not a no-op**: the instance is restarted twice |
| `/config/state`, `/config/check` | reads |
| `/task/*`, `/leave-*`, `/add-server`, `/forward`, `/pki/renew`, `/exec` | made by servers outside a step (or by no step code); never repeated |

**Shown.** `TestReviewRetryAfterTheServerActed`: a server whose handler acts and then ends the connection without an
answer once; a call `POST /standby/rebuild` inside a step returned success and the handler had acted twice. And
`TestReviewClientTimeoutIsRetried`: a handler that never answers, a client timeout of 1 s (standing for the 10 minutes
of the members' client): the call was made three times and failed after 4.2 s, i.e. a hung call now costs three times
the client timeout (30 minutes for the members' client) before the step fails, with the lease of the task renewed
meanwhile; `transient` counts the client's own timeout as a connection failure.

**Expected.** A repeat must not repeat the work: for the two calls above, a token of the request, or a check of the
state before acting (the rebuild knows whether the standby is already of the generation it was asked for). A timeout
of the client is not "a connection that failed under the call" and should not be repeated.

**Interaction with fencing.** A repeat that arrives after a newer run was seen by the callee is refused (same token,
lower epoch); a repeat of the same run is accepted. Two executions of the same call can overlap (the first one has
not ended when the connection failed); only the standby calls are serialized by a lock of their own.

### ST-04 (medium) - the lifecycle lock ignores the context, and the fence path waits behind it

**Where.** `cp/internal/instance/manager.go` `lifecycle` (a plain `sync.Mutex.Lock()`), `cp/internal/lease/manager.go`
`startPrimary` (holds `m.op` across `Instance.Start`) and `Fence` (takes `m.op`).

**What happens.** (a) A caller with a context that ends still waits for the lock for as long as the holder takes: a
`Stop` with a 500 ms context returned after 7.7 s, when a start that held the lock ended, with the error of its own
expired context (shown, `TestReviewLifecycleLockIgnoresTheContext`). A start that hangs (`pg_ctl start -w`, at most 90 s,
or a stop of 60 s) therefore holds up every other start, stop and restart of the instance for that long, whatever
timeouts their callers set. (b) The lease manager's start of the primary after a won lease (`startPrimary`) holds `m.op`
and calls `Instance.Start`, which now also waits for the lock of any other caller. `Fence`, the in-process fence, takes
`m.op` first, so it waits for all of it. Shown with the real instance manager and a fake `pg_ctl`
(`TestReviewFenceWaitsBehindALockedStart`): another caller restarts the instance (the stop takes 6 s) while the
manager wins the lease and starts the primary; the lease is then lost and `Fence` is called: it returned after 11.2 s,
of which 5.2 s were the wait for the lock (the other 6 s are the fake stop of the fence itself). Before the lock the
primary start did not wait for anyone else's operation, though `Fence` has always waited for the manager's own start
(up to 90 s); the lock adds the other callers' time to it. The independent watchdog is the backstop for a primary that
runs without a lease, so this is a delay of the in-process path, not a hole; but it contradicts the rule that a fencing
stop does not wait behind anything.

**Expected.** The lock wait ends with the caller's context; the manager's fence does not wait for `m.op` held by a start.

**Direction.** `lifecycle(ctx, id)` with a context-aware lock (a channel of size 1), and let `Fence` write the marker and
call `StopImmediate` without `m.op` (`StopImmediate` is already unlocked); keep `m.op` for the start only.

### ST-05 (medium) - a row left by an earlier holder is found by comparing two servers' clocks, with no tolerance

**Where.** `cp/internal/rebalance/pg.go` `findMove` (default branch: `started_at >= ref.Since`), used by `Recover` for a
row without an extension id (`ExtMoveID == 0`), `cp/internal/rebalance/loop.go` (the comment on `Ref`), and the README
("found by the time the row was started, read from the system database, which is on the server of the extension").

**What happens.** `ref.Since` is `started_at` of the loop's own row, written with `clock_timestamp()` of the system
database, which is the database of the leader's instance. `kingres.bucket_move.started_at` is written by the map
group primary of the sharded database. Those are two servers in any cluster where the leader does not run the map
group primary; the README sentence is true only for one server. The first review (RBR-05) asked for an id because of
a clock skew of more than 10 s; the new code has no tolerance at all, though a call starts after `StartMove`, `Mark`
and the guard, so the extension's time is already behind `Since` by tens of milliseconds even with equal clocks.

**Shown.** `TestReviewRecoverBySinceNeedsEqualClocks` (a record `done` with `started_at` = `Since` + 30 ms + skew):
skew 0 gives `done`; skew -50 ms and -2 s give `failed` with "the move had not been recorded by the extension when
the run was interrupted". The other direction (the extension's clock ahead) matches the previous attempt's record
of the same bucket (by reading).

**Effect.** A holder that took over a row whose id the earlier holder had not yet written (the record was less than a poll
old) settles a move that is running or done as failed, and counts an attempt; a retry is then refused by the extension
("already moving") or repeats a finished move, and three of those give the bucket up. No data is touched.

The id path (`Mark` before the call, record above it) is sound for the cases I could think of: two runs in different
databases have separate `bucket_move` tables; a move started by a person on another bucket does not match; a person's
move on the *same bucket* between `Mark` and the record would be taken as the run's own (the loop would then follow
and, on a stop, cancel the person's move) and the extension refuses the loop's own call, which the loop reads as a
failed attempt; the identity column is never behind the table's maximum except after a restore that resets the
sequence below existing rows, when the record is not found and a call waits until `guardMaxWait` (5 minutes) and is
cancelled while running, which is ST-02's cancel; neither is shown.

**Direction.** Store the mark (`After`) with the row (it is lost with the process today) so that the time path is never
needed, or match on a marker carried by the call.

### ST-06 (low) - retried writes that are not idempotent duplicate rows

**Where.** `cp/internal/rebalance/retry.go` `retryRows`, `pg.go` `StartMove` (the `INSERT` branch with `max(seq)+1`),
`AddSkipped`.

**Shown.** `TestReviewRetriedWritesAreNotIdempotent`: a real system database, the real `PGRows`, a wrapper that returns
a connection error after the write was committed, once, under `retryRows`:

```
StartMove once, answer lost, repeated by retryRows: rows now: 2
   seq=1 bucket=3 state=running attempts=1
   seq=2 bucket=3 state=running attempts=1
AddSkipped ...: rows now: 2 (two 'skipped' rows of the bucket)
```

The orphan `running` row is settled at the next pass of `recoverLeftovers` by the extension's record of the same
bucket, which makes one move count twice in `movesMade` and in the report (and, with enough of them, in the
`MaxMovesFactor` check). `UpdateMove`, `FinishMove`, `UndoStart` (absolute values), `SetPlanned`, `Settle` and
`CreateRun` (`ON CONFLICT DO NOTHING`) are idempotent; no counter or audit event is incremented by a write of the
loop (by reading). The trigger is the answer of a commit being lost, which is rare.

**Direction.** `StartMove` and `AddSkipped` check for the row of this attempt before inserting (the key is
`(task_id, bucket, attempts, state)` or a client-chosen `seq`).

### ST-07 (low) - the new holder gives up on a live long move after 10 minutes

**Where.** `cp/internal/rebalance/pg.go` `Recover` (`RecoverWait` 10 minutes), `loop.go` `recoverLeftovers` (a failure
returns an error that fails the step).

**Shown.** `TestReviewNewHolderGivesUpOnALiveLongMove` with `RecoverWait` of 2 s and a record in `copying`: after 2 s
`Recover` returns the error "the move is still not settled after 2s; run kingres.resume_bucket_moves() ...". With the
real 10 minutes the same happens to a holder that follows one whose call is still copying a large bucket (a copy "is
unbounded by design"). The step fails, the row stays `running`, the move goes on and finishes by itself; the next start
again waits for another 10 minutes. No data is touched; the run ends `failed` with an alert for a move that is fine.

**Direction.** A move whose record is in a live phase and whose session is alive is waited for as long as it stays
alive (or the run is paused with a stop of its own), not for a fixed time.

### ST-08 (low) - a server that can never apply the configuration costs a task every minute (by reading)

**Where.** `cp/internal/pgconfig/converge.go` (`retryAfter` one minute, the entry is forgotten only when the server is
seen up to date), `cp/internal/pgconfig/apply.go` `stepApply`.

A server that answers `/config/state` but fails `/config/apply` (a value it refuses, a file it cannot write) is never
"up to date", so every 30 s tick that finds the hash unchanged and one minute passed starts a `config.apply` task for
it: one every 60 to 90 s, 1,000 to 1,400 a day, against 100 to 150 before. Each is a row of `kingres_cp.task` with its
steps and an audit record; I found no code that deletes tasks. A server that does not answer is not in the set (it is
skipped), and a server that hangs inside `/config/apply` holds the task for the 2 minute call timeout while the others
of the same task wait behind it in order. It does not starve other servers beyond that, because `busy()` only
suppresses a new pass while a task runs. The cost is growth of the system database and of the task list. Not run.

**Direction.** Back off for a server that failed (grow the wait up to ten minutes again), and prune finished tasks.

### ST-09 (low) - the tests of the round: what they do not see

See "The tests of this round". The two that matter: the lease tests force enforcement (ST-01), and the fix for RBR-05
has no test.

### ST-10 (low) - unauthenticated callers hold goroutines for 10 s while the instance is unavailable (by reading)

**Where.** `cp/cmd/kingresd/main.go` (the receiver of the internal API uses the retrying authenticator),
`cp/internal/membership/handle.go` `join` (`POST /bootstrap/join`, reachable without a client certificate, rate
limited per user name and per address only after a failure is recorded).

While the instance cannot be asked (restart, or the window of `Confirming`) each such request waits up to
`DefaultUnavailableWait` (10 s) with growing pauses, and an unavailable instance is not a failure that the limiter
records. A caller that rotates user names holds as many goroutines and sockets as it opens; the server has no
connection cap on the internal listener. The wait ends with the request's context. It does not turn a refusal into an
acceptance. The REST listener has its own limits in front of the same authenticator.

**Direction.** Do not retry for a caller that has not presented a client certificate, or cap the concurrent waiters.

### ST-11 (note) - the lifecycle lock is per process

`lifecycleLocks` is a `sync.Map` of the process. The README does not say that two `kingresd` on one data directory are
unsupported and nothing refuses a second one (the watchdog has a lock file, `kingresd` has none; two with different
ports start). With two, the original hang (a start between the stop and its pid check) is back, and so are two lease
managers over one primary. Not run.

### ST-12 (note) - statements that are not true

- README ("RBR-05") and the comment on `Ref` in `loop.go`: "the system database, which is on the server of the
  extension". It is on the leader's server (ST-05).
- The commit message says "no clock of this process is compared with the extension's": literally true, but the time path
  compares the system database's with the extension's.
- The contract (OpenAPI) describes `GroupLease` but not that a follower's API answers 503 or loses its instance for the
  seconds of a fence and of the start after it; the README documents it for sign-in. If clients are meant to retry it,
  the contract should say so.

### ST-13 (note) - the public listener

`cmd/kingresd/main.go` creates the public `http.Server` with `WriteTimeout` of 60 s from the request, the configuration
that caused "bad record MAC" on the internal one; one handler extends its own deadline. A synchronous handler that
runs longer than 60 s (a fan-out to a slow server, for example) would show the same symptom to a client. I did not
enumerate the handlers.

## The tests of this round

Mutants: one line changed in my copy, the test that the commit or the README names run, line restored.

| Mutant | Test | Result |
|---|---|---|
| peer client repeats every call | `TestAConnectionThatFailsUnderACallIsRetriedWhereTheCallMayBeRepeated` | killed |
| transient also repeats the test's own blocked partitions and revoked certificates | the same package | killed |
| `Retrying` never retries | `TestRetrying*` | killed |
| `Confirming` counts a lost group | `TestConfirming*` | killed |
| fenced bound raised to 1,000,000 | `TestACalleeThatAlwaysRefusesTheFence...` | killed |
| **count of fenced runs not reset by a step that ends** | `TestACallee...`, `TestSuperseded...` | **survives** |
| `UndoStart` not called (withdrawn call is an attempt) | `TestAWithdrawnCallIsNotAnAttempt`, `TestWithdrawnCalls...` | killed |
| bound of three withdrawn calls removed | `TestWithdrawnCallsInARowStop...` | killed |
| `retryRows` makes one try | `TestABlip...`, `TestASystemDatabaseThatStaysAway...` | killed |
| `Current` returns every row | `TestTotalsDoNotCount...` | killed |
| bootstrap certificate mutex removed | `TestBootstrapCertIsCreatedOnceUnderConcurrentCalls` | killed |
| `internalCertificate` ignores `resetting` | `TestInternalCertificateDuringAReset` | killed |
| internal server given a `WriteTimeout` | `TestACallLongerThanTheResponseTimeoutIsAnswered` | **survives, and is equivalent**: the late deadline overrides it |
| late write deadline (`SetWriteDeadline`) removed | every test of `internal/internalapi` | **survives** (the deadline is untested) |
| `isMember` call site replaced by the plain `Member` | `TestMembershipIsAskedAgain...` | **survives** (the test calls `isMember` directly; the call site in `ServeHTTP` has no test) |
| `recordDirectory` call site replaced by `Replace` | `TestRecordDirectoryKeepsAServerThatIsBeingAdded` | **survives** (the test calls the function; the integration test of the same name in the README was not run) |
| converge: the up-to-date server not forgotten / wait back to ten minutes | `internal/pgconfig` tests | **survive** (no unit test; only a cluster test) |

Not run: the PostgreSQL tests of the guard (`TestGuardBlocksOnlyTheClaim`, `TestVoidGuard*`, `TestGuardKeeps...`) and the
two takeover tests on a cluster. For `TestGuardBlocksOnlyTheClaim`, which the author never ran against the old guard:
by reading it does discriminate (an old row lock `FOR SHARE` in a transaction would make its `SELECT FOR UPDATE` and
`ALTER TABLE` probes time out), but I did not run it so. The other things the author states are as stated: no test for
RBR-05, and the takeover-in-the-switch test was not run in its final form against the old code (not run by me).

The cluster-level removal test `TestRemovingAServerCostsNoOtherGroupItsLease` is blind to ST-01 by construction
(`ForceEnforce`). The relaxation in `rebalance_faults_test.go` (`retried < 1` instead of exactly one retried move)
says "on a loaded machine another move can hit a timeout of its own"; that is a product behaviour (a move failing by a
timeout of the extension and being tried again) that the test now tolerates silently, and it would also hide a
regression that makes every move need two attempts. The other patience changes (`lvLeaseJustRenewed`, retry on 409
`operation_in_progress`, waiting for `done`, waiting for the state `pendingRestart`) are by design as far as I could
check: the product answers 409 for a second configuration task (documented), the report is written by the last step
and the task is recorded as done a moment after, and a follower serves only while its lease holds.

## For the failover work

- **The watchdog is a second authority over the primary and nothing tells it when enforcement changes (ST-01).** A
  failover will change which groups a server is primary of, whether a group has a standby, and the set that is
  enforced. Every such change must update or stop the watchdog explicitly; a stale `lease.json` is a fence.
- **A fence must not wait.** `Fence` takes `m.op` and the lifecycle lock is not context-aware (ST-04). A promotion or
  a demotion that stops the old primary must use `StopImmediate` and must not queue behind a start; and nothing that
  holds `m.op` should call a locked operation.
- **Calls in a step are repeated.** The failover steps (promote, repoint, rebuild) will be made twice when an answer is
  lost: each must be a no-op on its second execution, and a hung call is made three times at the client's timeout
  (ST-03). `/standby/rebuild` and `/config/restart` already are not.
- **`StopForFailover` only sets a flag.** The rebalance stops when the call in flight returns: a move in `switching`
  is not interrupted, the extension ends it within `2 x node_timeout` plus the cancel bound, and `WaitStopped` can
  return false for that long; the failover must keep asking, as the README says, and not assume a stop.
  A superseded holder's call is not cancelled and is not bounded by the control plane, so after a failover of the map
  group primary or of a target the call may return an error long after the new holder took over.
- **A move is found by an id above a mark kept in memory; a row's time is the system database's clock (ST-05).** After
  a failover of the map group primary the mark taken from the old primary is still valid only if no committed move was
  lost; the time path is not usable across servers. The system database lives on the leader's server and moves with
  the leader: the guard session, the advisory lock and the claim are on it, and a failover of it voids the guard (the
  code proves the lease again, correctly).
- **The claim skips a task whose guard is held.** A frozen holder keeps the task unclaimable for up to the idle
  timeout of its guard session (45 s) and a call that waits for a lock is kept alive for up to 5 minutes; the
  failover task is another task, but the exclusion of rebalance and failover (migration 0009) is by state.
- **Sign-in and role checks wait up to 10 s and then answer 503** for an instance that cannot be asked, and are gated
  while `Confirming()`; a failover that restarts primaries should expect clients to see that.
- **`Enforced` is the group's, `Holds` is the lease's:** a group that has no standby stops being enforced
  immediately, and what that means for the instance depends on which of the two authorities looks (ST-01).

## What held under attack

- No lock cycle between the lifecycle lock, `opMu`, `m.op` and `m.mu` by reading; `StopImmediate` is not delayed by a
  locked start (0 s while a start held the lock for 8 s).
- The bootstrap certificate cannot be mistaken for a member's certificate by a peer (verification against the CA,
  `UnknownAuthorityError` maps to `ErrNotMember`); a stalled body on the unauthenticated bootstrap path ends after 30 s
  on HTTP/2; a handler's context is not cancelled by `ReadTimeout`.
- The advisory key space: the guards use `5278100627742580736 + task id`, the exclusion trigger and the ledger
  verification table use `hashtext(...)` (a 32 bit value), the migrator uses `0x4b494e4752455344`; none collides in the
  system database. `kingres_shard` takes its locks as lock tags with a fourth field, outside the space of the SQL
  advisory functions, and `kingres_ledger` uses `hashtextextended` (64 bit) keys in the databases where it is
  installed, so a collision with a guard is possible only by a 2^-60 chance.
- `Retrying` and `Gated` never change a refusal into an acceptance: only `ErrUnavailable` is retried and every attempt
  reads the role again.
- `fence.Registry` refuses a repeat of a superseded run; the claim SQL's `pg_try_advisory_xact_lock` does not take
  locks beyond the transaction; the guard's `idle_session_timeout` bounds a frozen holder.
- The unit tests of every package pass on the tree (my own failing test aside); 11 of the 18 mutants above are killed
  (one of the seven that survive is equivalent).

## Not reproduced

- The cancel of a recorded move by a real extension (ST-02) on a real cluster: shown against a stub of
  `kingres.move_bucket` and `bucket_move`, not at a fault point of the real extension.
- SQLSTATE 08001 itself: I sampled the instances (refused connections, `57P03`); the 08001 is what the extension
  answers when it must connect to such a node, not run.
- The race in which a call that has just returned has its answer discarded by `withdraw`.
- A clock difference between two real servers (ST-05): shown with a skew of the stored time, not with two machines.
- Two kingresd on one data directory (ST-11), a task that grows without bound (ST-08).

## Not covered

- PostgreSQL 19rc2: nothing was run on it.
- The real-cluster tests of the round (`TestRebalanceTakeoverInTheSwitch`, `TestRebalanceTakeoverAtTheCatchUp`,
  `TestRebalanceCallWaitsForTheClusterLockBeyondTheGuardHold`, `TestNoSignInAgainstAnInstanceTheLeaseIsAboutToStop`,
  the join and reset tests, `TestConfigServerGroupAndServerLevels`) and the complete suite.
- The PostgreSQL tests of the guard, not mutated (see above); a slowloris on HTTP/2 headers and CONTINUATION frames; the
  public listener's other handlers (ST-13); every kind of `/apply`; the Windows build of the internal listener beyond
  the unit tests.
- Whether the alert of a failing `config.apply` is deduplicated (ST-08).
- The first review's fourteen findings and the second's seven on a cluster: only their unit tests were run.
