<!-- SPDX-License-Identifier: AGPL-3.0-only -->

# Review of the failover work, increments 4 to 7 (round 1), at commit 6ca50ff811

## Scope and method

Commits read: cf390ffee8 (increment 4, the old primary comes back as a standby), 4574d26244 (fixes F3-01 to F3-15),
bed0881cc3 and 8135c4b66e (increment 5, map group failover and its journal), 1f868cc33d and 00bf7ef2f5 (increment 6,
automatic failover), 6ca50ff811 (increment 7 round 1, switchover, and the fault file change).

Read in full or in the parts that matter: internal/failover (switchover.go, auto.go, steps.go promote/hand-over/records/OnFail,
service.go Start/Resolve/resume/StartAutomatic, types.go counter SQL), internal/failover/journal, internal/failover/mapgroup,
cmd/kingresd (switchover.go, failover.go handlers for lease/demote/promote/staleTerm, autofailover.go, mapfailover.go,
mapmonitor.go, ha_proxy.go), internal/instance (quiesce.go, SyncAccess, WriteHBA, Rewind), internal/standby/oldprimary.go,
internal/lease/adopt.go, internal/internalapi (policy table, fence header rule), internal/task (Cancel, cancelTask),
contrib/kingres_shard/src/ha.c (kingres.switchover_verify), the design (ha-phase5-part2.md sections 3.4, 4, 5, 6), README,
internal-api.md and the failoverServer text of openapi.yaml. The three earlier reviews were used to avoid repeating findings.

Method: reading with an adversarial question per rule; unit tests on Windows; one-line mutants of the key rules (a script that
changes one expression, runs the package tests and restores the file; run in a detached worktree, a second one for the
parallel batch); three small repro tests written against the fakes (removed afterwards, not committed). Cluster tests: see
"What was run".

## What was run

- Windows unit tests, `go test -count=1 -v` of internal/failover/..., internal/lease/..., internal/instance/...,
  cmd/kingresd/..., internal/standby/...: all 7 packages ok; 204 top-level tests passed, 0 failed, 49 skipped (the skips are
  the tests that need PostgreSQL or Linux).
- 70 one-line mutants: 40 killed, 30 survived (table at the end). Four more patterns did not compile and were fixed and rerun.
- 3 repro tests (not kept): (a) the automatic loop starts again at once after a task that failed before it promoted;
  (b) the map group request repeated with allowDataLoss on the same journal does not go on; (c) the rollback hook resumes the
  old primary after the promote step began and failed.
- Cluster tests: queued behind the Windows mutex Global\kingres-cp-integration, which was held by another run for the whole
  time of this review (checked at intervals by a non-blocking probe; a queued run for TestSwitchoverOfAGroup,
  TestSwitchoverVerificationFails and TestFailoverCalleeRefusesUnfencedAndUnnamedCalls waited for it). The queued run waited about 30 minutes and was then withdrawn (its process was stopped before it took the mutex), so no cluster test was run for this review.
  No finding below depends on a cluster run; those that would profit from one say so.

## Summary of findings

| Id | Severity | Title |
|---|---|---|
| F4-01 | high | The map group failover cannot accept a loss of data on a repeated request (verified) |
| F4-02 | high | The switchover rollback is one best-effort pass; a group can be left unreachable or without a primary, and nothing repairs it |
| F4-03 | high | A rollback runs for a promote step that began and failed (promote_timeout), contrary to the design: acknowledged writes on a promoted standby can be lost |
| F4-04 | medium | Automatic failover starts again at once after a task that failed before promoting (counter, cool-down and back-off do not see it) (verified) |
| F4-05 | medium | The map group calls start and continue are open to every member and trust the caller's body (allowDataLoss, trigger, journal steps) |
| F4-06 | medium | A stuck map journal cannot be abandoned: gate, monitor and status stay as if a failover ran |
| F4-07 | medium | The proof of a switchover is not repeated at the promotion; the window between verify and promote is unguarded |
| F4-08 | medium | A cancel is honoured between steps only, and the cancel test does not prove that it works |
| F4-09 | medium | The new switchover calls are not fenced by the task run (F3-13 was not extended); the term check fails open |
| F4-10 | medium | The hold rules leak to the standbys of the same server, and a rewrite of the access rules during the hold is lost or reverted |
| F4-11 | low | The quiesce is not complete (local sockets, substring match, asynchronous reload); safe only because the proof does not depend on it |
| F4-12 | low | The proxy hold is in memory while the hold rules are on disk |
| F4-13 | low | The alert autoFailoverLimitReached is never cleared after a leader change followed by a reset |
| F4-14 | low | The candidate monitor of the map group has no cool-down and a stagger shorter than a bootstrap |
| F4-15 | low | Smaller input and consistency points |
| F4-16 | note | The proof of replay is sound; its premises |
| F4-17 | note | Test gaps shown by the surviving mutants |

Counts: critical 0, high 3, medium 7, low 5, note 2.

## Answers to the questions

### 1. Lost acknowledged write or two writable primaries

No path was found that makes two instances writable at the same time for a group: every promotion needs the group lease
(F4-16, F4-03 for the one deviation that matters), and the old primary is kept down by the relinquished marker, the wait
marker and the lease. What was found is a way to lose acknowledged writes (not to split the brain) in a double fault:

- Failover and switchover: F4-03. If the call that promotes the standby is answered with a time-out, the step fails with
  promote_timeout after the standby may have been promoted; the hook then rolls back (resume the old primary, lift the hold,
  release the leases) because the group row still says "pinned". If that server then dies, the old primary starts on the old
  timeline when the lease runs out, and what the promoted standby acknowledged is lost when it comes back. The design says that a
  rollback never runs after S8.
- Automatic failover and map group failover: no loss path beyond what the verdict allows; the automatic task never accepts a
  loss (it pauses). The one way a loss can be accepted without an operator is F4-05 (a member sets allowDataLoss in the call to a
  candidate).
- Cancel at every step: a cancel is honoured only between steps (F4-08), so it cannot interrupt the promotion; the guard has a
  non-atomic check, which is safe only for that reason.
- Leader change or restart in the middle: the steps are repeated from the group row and the state calls; a superseded run is
  fenced only for demote and promote (F4-09). The proof is derived again from the last clean stop, which keeps it sound; the
  gap is F4-07.
- Partition: the lease needs a majority; the leader that hears no majority pauses (takeFence). A pause after the old primary was
  stopped keeps the group down until an operator cancels (documented nowhere; note in F4-02).
- The old primary coming back: it cannot start without the lease (marker); see question 5.

### 2. Switchover

- Proof of replay: sound as designed (F4-16). The standby must have replayed exactly to the end of the shutdown checkpoint that
  kingresd read from pg_controldata of the stopped instance, and the record is read in the standby's own WAL. Anything beyond
  the checkpoint, a different record, a non-shutdown record or another timeline is an error. The weak points are not in the
  check but around it: the proof is made before the lease is taken and not repeated (F4-07), and a rollback after a failed
  promote (F4-03).
- Quiesce: it does not stop all writers (F4-11): sessions on the local Unix socket are not ended and the "local" rules stay for
  every role, the pattern for replication is a substring match, and the reload is asynchronous. This does not matter for
  durability: every commit acknowledged before the fast shutdown is in the WAL that the proof checks; the quiesce only makes the
  pause orderly. Prepared transactions: only the extension's (gid kingres_h%) are waited for; a missing count (nil) is treated as
  "none" (mutant D1 survived); user prepared transactions replicate and are resolved on the new primary. The entry point hold
  covers the entry point of the old server only (the other entry points forward to it).
- Failures before promotion: every step that fails with a code gives the writes back, and OnFail repeats it for a crash or a
  cancel. The rollback is a single pass whose errors are swallowed when the old server is unreachable (F4-02), so "every
  failure gives the writes back" does not hold when the failure is the old server being unreachable.
- kingresd restart while the writes are paused: the hold rules and the backup are files and survive; the proxy hold, the
  quiesced flag in the state call and the wait of the leader's measurement are memory (F4-12). The leader's task resumes and
  repeats its step. Nothing looks at the leftover hold file when kingresd starts (Manager.Quiesced is never called).

### 3. Automatic failover

- It cannot fire for a server that is only slow for the leader: the dead state needs the tracker's dead state, silence of at
  least max(timeout, 3 x failure timeout), a majority of all members that report the server as not live, a leader that hears a
  majority, no unfinished failover and the cool-down. All of this is covered by killed mutants (A01 to A12).
- It can fire for a server whose kingresd is hung or restarting while its PostgreSQL is healthy (the heartbeat is the control
  plane's). The old primary is then fenced by the lease; the task never accepts a loss, so a lagging standby pauses it. This is
  by design; it is noted for the operator documentation.
- Flapping and repeated starts: F4-04 (verified). The counter counts at the promotion, the cool-down reads group rows that are
  finished only when the group is done, and the back-off is for a start that returned an error, not for a task that failed.
- Clock jumps: the silence is measured by the tracker; the cool-down compares the leader's wall clock with the database's
  clock_timestamp() of finished_at (F4-15).
- Counter and limits under concurrency and leader change: the increment is one SQL statement guarded by failover_run.counted,
  and the failover class is exclusive by trigger (0009, 0011, 0013, 0014), so two starts cannot count twice. A limit alert raised
  by one leader is not cleared by the next (F4-13). A rebalance that completes resets the counter by design.

### 4. Map group

- The journal is written with write, fsync, rename and a directory flush (not on Windows) after every step and event; a journal
  that cannot be parsed refuses to start. Each step is idempotent and the test crashes at every boundary (killed mutants for
  the pin, the out-of-recovery refusal, the loss acceptance and the Decide refusals). Gaps: a repeated request cannot change
  the acceptance (F4-01), the stuck journal (F4-06), the freeze (B2) is memory and is not redone after a restart (the standby
  agent may then change the list of synchronous standbys; no loss), B3 (take the fence lease) has no unit test of the "not held"
  branch (mutant G04 survived), and a restart between B3 and B5 depends on the holder adoption being taken again by B5 (the
  test crashes at boundaries with a fake that always grants).
- Two members acting: the lease allows one holder, so only one promotes. Both can run B1 to B3 for the same term (the stagger
  is 30 s, a bootstrap takes longer); the loser keeps an active journal (F4-06, F4-14). The calls that start and continue a
  bootstrap are open to every member (F4-05).

### 5. The old primary coming back

No path was found where a rewound or cloned old primary serves stale data as a primary or accepts writes before it is a
proper standby: the directory is turned into a standby (mark, role file, then the data), the rewind step writes standby.signal itself
and an interrupted rewind is repaired by a base backup (the mark names the step), and enterFormer refuses a directory that is
running, of another group, with a lease held by this server, named primary by the plan, with a term not newer than its role file,
or without the proof function. These refusals have a table test, but the test is build-tagged for Linux, so the Windows run
cannot kill the mutants O01 to O07 (all survived there). I read the table: each refusal has a row. They were not run on Linux
(mutex). A standby that lags serves reads as every standby does; it is offered by the entry point only when its record is ready
(not examined further).

### 6. Authorization and input validation

- quiesce, unquiesce, mark, verify, resume: the policy is LeaderOnly (certificate of the cluster and the callee's own knowledge of
  the leader), the body must name the group (requireGroup), and the callee acts on its own instances only. mark and the fast
  demote carry ExpectedTerm; quiesce, unquiesce, verify and resume carry nothing that proves a task run, and the fence header
  (F3-13) is required only for demote and promote (F4-09). Replays are harmless for the idempotent calls; a replay or a late call
  of a superseded run is not refused.
- The term check fails open: a negative ExpectedTerm skips it, and an unreadable catalog skips it for the lease and the fast
  demote (strict is false). These were meant for the stopped instance, but they apply to every caller (F4-09).
- map start and map continue: AnyPeer, trusting the body (F4-05). The public endpoints: failoverServer needs kingres_ops;
  Resolve checks mode, comment length, duplicate or negative group ids, targets with a server, a known server and the groups of
  the request; a target for a group outside shardGroupIds is refused only when shardGroupIds is given (F4-15). A graceful
  request for the map group is refused (422) but no test pins it (mutant S03 survived).

### 7. Do the tests prove what they claim

Mostly yes for the rules in Evaluate, the journal, the hold rules and the steps of the switchover (40 of 70 mutants killed, all
12 of the automatic rule). Not for: the repeated request of the map group (the test builds a fresh world, so its comment "the
pause did not close the journal" is not shown); the cancel of a switchover (F4-08); the callee handlers (no test runs on
Windows; all 8 mutants survived); the rollback of the lease of the new server (S10), the term of the mark and the stop (S06,
S12, M18); the guard of a graceful request for the map group (S03); the lease check of B3 (G04). See F4-17.

## Findings

### F4-01 (high) The map group failover cannot accept a loss of data on a repeated request

Code path: mapFailover.begin calls Runner.Decide, which calls Journal.Begin(id, oldServer, term, trigger, requestedBy,
allowDataLoss). Begin returns the journal in progress unchanged when the term and the old server are the same and it is not
finished (journal.go, the case with finishedLocked), so the new allowDataLoss is dropped. B4 (mapgroup.pin) then judges with
j.AllowDataLoss of the first request. The pause text tells the operator: "Repeat the request with allowDataLoss set to true".

Reproduction (a test against the fakes of the mapgroup tests, removed afterwards): a world where the standby on s3 cannot be
read; Run(req) pauses; Run(req with AllowDataLoss true) on the same runner and journal pauses again with the same text
("promoted=false"). TestAnUnreadableStandbyNeedsTheAcceptanceOfALoss does not catch it because its second half uses a new world
and a new journal; its comment says the pause did not close the journal.

Effect: when the map group's standbys are behind or unreadable the operator can never accept the loss, so the cluster stays
without a manager leader. The same sticky copy works the other way: a stale journal begun with allowDataLoss true makes a later
automatic trigger accept a loss, and Trigger and RequestedBy are kept from the first request.

Fix: let Begin (or a new SetAcceptance) update AllowDataLoss, RequestedBy and Trigger of an in-progress journal when the new
request is a manual one (record an event); never let an automatic request keep a manual acceptance of an earlier journal; change
the test to repeat the request on the same journal.

### F4-02 (high) The switchover rollback is one best-effort pass; nothing repairs what it leaves

Code path: rollbackSwitchover (switchover.go) releases the leases, calls Resume and Unquiesce on the old server and swallows
ErrUnreachable and no_instance without a log; it is called once, from the step that failed or from OnFail. What it undoes is
persisted on the old server: the relinquished marker (lease/adopt.go Relinquish writes it, only Unrelinquish and BecameStandby
remove it), the hold rules and their backup (instance/quiesce.go), the wait marker. Nothing reads them at start or later:
Manager.Quiesced has no caller, loadRelinquished only loads the marker into memory, and the lease manager does not clear a
marker when it knows that no task runs. handleFailoverUnquiesce lifts the proxy flag first and then restores the file; an error
there is returned to the leader, which only logs it.

Effects (by reading; not reproduced on a cluster):
(a) If the old server cannot be reached during the rollback (the usual reason for a failed verify or stop is a network or
server fault), the old primary, when it returns, runs with the hold rules and rejects every client while it renews its lease: a
silent outage of the group, with a task that says "failed, rolled back".
(b) If the old primary was stopped, its relinquished marker stays; it never starts again and the lease is free: the group has no
primary until a hard failover.
(c) A pause between the stop of the old primary and the promotion (takeFence pauses when the leader cannot hear a majority)
leaves the group down with no timer; only the operator's cancel (or the repeat) ends it. Tasks that are paused also block the
automatic failover of every other server (Unfinished).

Fix: record the intent on the old server (task id, step) when the quiesce or the relinquish is written; on kingresd start and in
every lease round, if a hold backup or a relinquished marker exists and the group's task is not running (ask the leader),
restore it; make the rollback retry until the old server answers, or pause the task (not fail it) when the old server is
unreachable; log a swallowed error at warn; clear the proxy flag only after the file is restored.

### F4-03 (high) The rollback hook runs for a promote step that began and failed

Code path: Runner.promote ends with fail(CodePromoteTimeout, true, ...) after three tries when the callee does not answer and
the state call does not show an attached promoted primary (steps.go, the default branch). The engine records the task failed and
OnFail calls rollbackUnpromoted for a graceful run (steps.go OnFail), which skips only the group states pending, promoted, done,
failed and skipped (switchover.go). The group row is "pinned" (state "promoted" is written only after the call answered) and
nothing writes "failed" (grep: no setter), so the group is rolled back: Resume and Unquiesce on the old server, release of the
leases.

Reproduction (test against the fakes, removed afterwards): a graceful world, group row "pinned", a task whose promote step is
failed and whose error is promote_timeout; r.OnFail(...) logs "lease-release s2, resume s1, unquiesce s1". The guard that the
design describes (5.3: "a rollback never runs after S8") exists only for a cancel (Cancellable); the hook does not look at the
step list.

Effect: with the standby promoted but not reachable (the cause of the time-out), the old primary is resumed. The lease at the
grantors is still the new server's while it renews, so the old primary stays down until the new server's lease runs out; if the
new server dies (or is cut from the majority) first, the old primary starts on the old timeline and the commits the promoted
standby acknowledged in between (direct connections; the catalog has not followed) are lost. A double fault, but it is the one
class the point of no return exists for. F3-01 refuses to release an adoption that is attached to a running primary, which
protects the case in which the new server answers.

Fix: in rollbackUnpromoted skip a group whose promote step is not pending (read the step list of the task, as Cancellable
does), or set the group state to "promoting" before the call and treat it as past the point of no return; after a promote
time-out ask the standby's state again until it answers or an operator decides; add a test with this input.

### F4-04 (medium) Automatic failover starts again at once after a task that failed before promoting

Code path: Evaluate gates a start on Enabled, the limit (EventsSinceReset), majority, evidence, Unfinished and the cool-down.
The counter is incremented in the transaction that records the promotion of the first group (rows_pg.go Set with Promoted,
CountAutomaticSQL). The cool-down reads max(finished_at) of failover_group, which Set writes only when a group is done
(steps.go, "done"). Unfinished reads tasks pending, running or paused. The back-off (Auto.until) applies only when
autoPorts.Start returns an error, that is when the task could not be submitted. A task that is submitted and then fails (for
example lease_still_held, the very error the unit test uses as the start error, standby_behind, no_candidate) leaves none of
these.

Reproduction (a test against autoFake, removed afterwards): five Steps with the same view give five Starts.

Effect: while the evidence holds, a failing automatic task is submitted again as soon as the previous one ends: a stream of
task rows, reports, freezes and audit events, never counted against the limit. Safety is not affected (a failing task
rolls back or never promotes).

Fix: count a task when it starts (or when it fails), record the end of a failed run per group for the cool-down, and apply the
back-off per server to a task that ended failed; add a test that submits, fails and evaluates again.

### F4-05 (medium) map start and map continue are open to every member and trust the body

Code path: internalapi policy: "POST /failover/map/start" and "POST /failover/map/continue" are AnyPeer. handleMapStart reads
{server, trigger, requestedBy, allowDataLoss} from the body and runs Decide; Decide only logs when the old server is live. The
caller's identity is not used. handleMapContinue adopts a Journal from the body: it checks the pin names this server and the
catalog term and primary match, then Adopt stores the journal including its Done steps, Pin, AllowDataLoss and Trigger.

Effects: any server with a cluster certificate can (a) make a candidate begin a bootstrap against a healthy map primary (the
fence lease then fails with lease_still_held after retries, leaving the journal active, F4-06), (b) set allowDataLoss true so that
B4 accepts a loss that the operator was never asked about, when the map primary is truly gone, (c) send trigger "automatic" and
requestedBy "system" to count against the limit and to look like the system, (d) forge a journal with finished steps and a
pin of its choice (the promotion itself still needs the lease and the term, so no promotion without the lease was found).

Fix: carry the operator's decision as a leader-signed or session-bound assertion (the gather-authorize mechanism exists), refuse
allowDataLoss and trigger from a body that has none; in Decide require the same evidence as the monitor (Evaluate) for an
automatic trigger and refuse a live old server unless the request is manual; for continue require the caller to be the server that
holds the journal's earlier steps and refuse Done beyond B4.

### F4-06 (medium) A stuck map journal cannot be abandoned

Code path: Journal has no way to close a bootstrap except B8 (Done(StepGateOpen)); Active() is true until then. Active closes the
engine gate on that server (Gate), makes mapMonitor.step return false, makes Status and TaskZero report a running failover, and
makes Begin return the same journal for the same term and old server. Ways to get there: the loser of two concurrent candidates
(the stagger is 30 s per rank; B1 to B4 take 25 s of lease wait plus 15 s of position reading), a bootstrap against a live map
primary (F4-05, or a partition on the leader's side), a non-retryable failure (no candidate, a newer term).
resumeInBackground tries 20 times and ends; the journal stays.

Effect: that server's monitor never evaluates again, so a later real loss of the map primary is not failed over automatically by
it; GET /failover on it shows a running map failover. A stale journal of a lower term is replaced by the next Begin, but the
monitor never calls Begin while Active.

Fix: a journal whose term is below the catalog's current term, or whose lease attempt ended with lease_held_elsewhere or
lease_still_held, is closed by the monitor (event "abandoned"); give the operator a call to abandon; show the age in Status.

### F4-07 (medium) The proof is not repeated at the promotion

Code path: verify (S6) asks the standby for switchover_verify(X); then takeFence and handOver (S7, up to 25 s of lease wait
and 3 x 15 s of hand-over) and only then promote, whose request has no checkpoint (PromoteRequest: Group, StandbyID, InstanceID,
ExpectedTerm). Between S6 and S8 only the relinquished marker and the lease keep the old primary down. A late call of a superseded
run (F4-09: resume has no fence) writes the wait marker; the old server's lease rounds win the free lease and start it, and
the restored rules let clients in; if the old server then fails before S7, the lease is free again, the new server takes it and
promotes a standby that lacks what the old primary acknowledged after S6.

Effect: a triple fault (zombie run, restart of the old primary, its loss), but it is the one way found in which a "no loss"
switchover could lose acknowledged writes without a rollback, and it is cheap to close.

Fix: put X and the cluster state into the promote request and have the callee run switchover_verify in the same call before
pg_promote (a standby that replayed beyond X raises KS006, which is exactly this case); keep the fence header for it.

### F4-08 (medium) A cancel is honoured between steps; the cancel test does not prove it

Code path: Engine.Cancel checks the kind's guard on a snapshot and then calls RequestCancel (not atomic); cancelTask runs "between
steps" (task.go, the message says so). A cancel during the quiesce, the drain, the stop or the verify waits for the step to end:
up to 2 min (quiesce), 1 min (drain), 120 s plus the mark wait (stop), 30 s (verify), with the writes paused.
The non-atomic check is harmless only because of that: a cancel that passes the guard while the promotion starts is honoured
after the promotion, when the group is "promoted" and the hook skips it.

Test: TestSwitchoverCancelBeforeAndAfterThePointOfNoReturn stalls the quiesce (fault quiesce:stall), sends the cancel and waits up
to 3 minutes for "failed" without reading the error code. The step fails by itself with cannot_quiesce after QuiesceWait (2 min),
so the test passes whether or not the cancel works. TestSwitchoverIsCancellableOnlyBeforeThePromotion tests the guard function
only. The README says a cancel does the same through OnFail; nothing shows it for a running step.

Fix: cancel the context of a reversible step when cancel_requested is set (the engine already polls the lease), keep the check
of the guard in the same statement as the flag, and make the test assert code "canceled" and a short time.

### F4-09 (medium) The new calls are not fenced by the task run; the term check fails open

Code path: internalapi.fenceRequired has two entries, demote and promote (F3-13). quiesce (ends every client session and
replaces pg_hba.conf), unquiesce, resume (clears the relinquished marker and writes the wait marker), verify and mark are
LeaderOnly with no fence header; the lease call (with TransferTo, which hands the lease to another server) is not in the
list either. staleTerm returns nil for a negative ExpectedTerm and, with strict false (lease, fast demote), when the catalog
cannot be read; the quiesce family has no term at all.

Effect: a deposed leader whose callee still names it the leader, or a defect, can end the sessions of a primary, hold its rules,
resume a primary that a newer run stopped (F4-07), or hand over a lease, without being a live run. No exploit beyond what
the lease allows was found, so this is hardening, but the F3-13 rule was written for exactly this and was not extended.

Fix: add the new calls (and lease with a transfer) to fenceRequired; make ExpectedTerm mandatory (reject negative) for calls
from a task, and make an unreadable catalog a refusal when the instance runs.

### F4-10 (medium) The hold rules leak to the standbys of the same server; a rewrite of the access rules during the hold is lost or reverted

Code path: Manager.SyncAccess copies pg_hba.conf of AccessSourceID (WritableIDs()[0]) into every local standby at every round of
the standby agent (agent_ops.go) and reloads it. While that primary is quiesced its file holds the hold rules, so the
standbys of other groups on the same server get them (local, replication, kingresd only) and refuse clients until the hold ends
and the next round copies the real rules. Separately, WriteHBA (network rule changes through ops, node join in cluster/member.go,
the plan's access rules) replaces pg_hba.conf during the hold: the hold is gone while the backup file stays, and Unquiesce then
writes the old backup over the operator's new rules.

Effect: reads from standbys of other groups on the old primary's server stop for the whole pause plus one round; a rule change
made during a switchover is silently reverted. Not reproduced (needs a server that hosts a primary and standbys of other groups,
which the cluster tests have).

Fix: make the standby sync read the backup (the real rules) when the source is quiesced; make WriteHBA write the backup while the
hold file exists (and keep the hold rules in hba); test both.

### F4-11 (low) The quiesce is not complete

Code path: handleFailoverQuiesce ends only sessions with client_addr IS NOT NULL and a role other than the service role;
HoldRules keeps every "local" line for every role and keeps a line when the database field merely contains "replication" (a
database named xreplicationx matches); Reload is asynchronous and the call answers quiesced as soon as the count is zero, which can
be before the postmaster applied the file.

Effect: a process of the same operating system user (the socket directory is private) can keep or open a session; a client can
connect in the instant before the reload takes effect. Neither threatens the proof (see F4-16), but the design says the quiesce
stops every writer and the README repeats it.

Fix: also end sessions without an address (all roles but the service role), match the database field exactly
(replication or a list element equal to it), wait until pg_hba_file_rules shows the hold rules before answering, and say in the
design that the proof, not the quiesce, protects the data.

### F4-12 (low) The proxy hold is memory, the hold rules are files

quiesced (cmd/kingresd/switchover.go) is a map in memory; it also feeds the Quiesced field of the state call and the entry point
(HoldsLease && !quiesced). After a restart of the old server's kingresd in the middle of a switchover the entry point sends new
connections to the primary, whose rules reject them (an authentication error instead of a wait), and the state says
quiesced false. Fix: derive it from Manager.Quiesced(id) (the backup file).

### F4-13 (low) The alert autoFailoverLimitReached is not cleared after a leader change

Auto.limited and Auto.blocked are memory. If a leader raised the alert, a different leader (or the same one after a restart) starts
with limited false; when the counter is reset the new leader sees "not limited" equal to its memory and never sends the clear.
Nothing else clears it (ResetAutoFailoverCounter only writes the setting). Fix: clear the alert in the reset, or read the alert
state in Step instead of keeping a flag.

### F4-14 (low) The map group monitor has no cool-down and a short stagger

mapMonitor.step builds an AutoView without Unfinished and LastEnded, so the 5 minute cool-down and the unfinished check do not
apply to it; the counter limit applies. The stagger (30 s x rank) is shorter than B1 to B4, so the second candidate starts its own
bootstrap while the first is still taking the lease (F4-06). Fix: pass the journal's activity and a time of the last bootstrap;
make the stagger longer than LeaseWait plus ReadBound plus PromoteWait for the first rank, or ask the first rank before starting.

### F4-15 (low) Smaller points

- The cool-down compares the leader's wall clock with the database's clock_timestamp(); a step of the leader's clock changes it.
- Resolve ignores a target for a group that is not in the request when shardGroupIds is empty (it only checks when the list is
  given); the comment is length limited but not otherwise checked.
- The B2 freeze of a map bootstrap is memory (10 min) and is not redone by ResumeOnStart, which only undoes B3 and B5, and only
  when B5 is done and B6 is not: a restart between B3 and B5 relies on B5 taking the holder adoption again.
- The old primary's hold backup stays if Unquiesce finds the group is no longer a primary on that server (handleFailoverUnquiesce
  acts only when PrimaryOf is true); pg_rewind or a base backup removes the file, so this is only a window (mutant I04 survived:
  nothing tests that the backup is removed, and a stale backup would stop the next Quiesce from writing the hold rules because
  Quiesce skips both writes when a backup exists).
- Deviation noted by the author: the old primary is rebuilt with pg_rewind after a clean stop instead of adoption; fine.

### F4-16 (note) The proof of replay is sound; its premises

kingres.switchover_verify(X, state): refuses a state other than "shut down"; refuses a position that cannot start a record;
reads the record at X in the standby's own WAL; requires a shutdown checkpoint; in recovery requires replayed == end of that record
(true when equal, false when less, KS006 when more); promoted, requires the standby to have left the checkpoint's timeline exactly
at its end. kingresd takes X and the state from pg_controldata after a confirmed stop (never an immediate stop), re-reads them in
verify (a stopped instance is read again), and the pinned standby is one that streams from the primary. Premises: the old
server's kingresd reports its own control file truthfully, the stop is a fast shutdown, and nothing starts the old primary
after the last read (F4-07). Sessions on a local socket, or any write acknowledged before the stop, are inside X. The mark and the
wait for its replay are an early warning only.

### F4-17 (note) Test gaps shown by the mutants

Survivors that name a rule with no test: M02 (the lease must be released by the stop), M17 and M18 (the mark failure guard, the
stale term at the stop), S06 and S12 (the term in mark and in the stop), S03 (the map group refusal for graceful), S09
(non-ready standbys are not candidates), S10 (the new server's lease is released by the rollback), D1 (an unknown count of
prepared transactions is treated as none), G02 (the pin names this server at B5), G04 (the lease check of B3: a fake that always
grants), G07 (term > versus >=), I03 (the service role is allowed from loopback only), I04 (Unquiesce removes the backup), C01 to C08
(the callee handlers: no Windows test), O01 to O07 (Linux-only tests). The F3 author's note that one-line mutants were run on a
cluster for F3-01 to F3-04 has no equivalent for the increments 4 to 7 in the repository.

## Mutants

Unit tests on Windows, `go test` of the package named. K killed, S survived. Test names are the first killer.

| Id | Change | Package | Result |
|---|---|---|---|
| M01 | the proof accepted without v.Ok | failover | K (ProofStaysFalse) |
| M02 | stop need not report Released | failover | S |
| M03 | stop need not report state "shut down" | failover | K |
| M04 | verify re-read of the stop weakened | failover | K |
| M05 | quiesce accepted without Quiesced | failover | K |
| M06 | drain accepts any count | failover | K |
| M07 | rollback without Unquiesce | failover | K |
| M08 | rollback without Resume | failover | K |
| M09 | rollback without releasing the leader's lease | failover | K |
| M10 | preflight without streaming | failover | K |
| M11 | choose without streaming | failover | K |
| M12 | preflight without Holds | failover | K |
| M14 | rollbackUnpromoted rolls back "promoted" | failover | K |
| M15 | stop does not wait for the mark replay | failover | K |
| M17 | mark failure guard removed | failover | S |
| M18 | stale_term at the stop not a rollback | failover | S |
| M19 | unprovable not a rollback | failover | K |
| D1 | unknown prepared count is not drained | failover | S |
| S01 | cancel guard ignores the promote step | failover | K |
| S02 | preflight without the live check | failover | K |
| S03 | map group graceful not refused | failover | S |
| S04 | OnFail does not roll back a switchover | failover | K |
| S06 | mark with ExpectedTerm -1 | failover | S |
| S09 | non-ready standbys are candidates | failover | S |
| S10 | rollback does not release the new server's lease | failover | S |
| S11 | mark poll without sleep | failover | K |
| S12 | stop with ExpectedTerm -1 | failover | S |
| A01 | corroboration by a majority removed | failover | K |
| A02 | limit > instead of >= | failover | K |
| A03 | majority of the leader ignored | failover | K |
| A04 | cool-down removed | failover | K |
| A05 | silence ignored | failover | K |
| A06 | 3 x failure timeout floor becomes 1 x | failover | K |
| A07 | unfinished ignored | failover | K |
| A08 | back-off ignored | failover | K |
| A09 | no-standby guard removed | failover | K |
| A10 | enabled ignored | failover | K |
| A11 | partial-standby block removed | failover | K |
| A12 | limit alert removed | failover | K |
| J01 | journal of a newer term not refused | journal | K |
| J02 | restart does not return the journal in progress | journal | K |
| J03 | the pin can be replaced | journal | K |
| G02 | B5 does not check the pin names this server | mapgroup | S |
| G04 | B3 counts any lease answer as held | mapgroup | S |
| G05 | out-of-recovery instance ignored | mapgroup | K |
| G06 | loss acceptance ignored | mapgroup | K |
| G07 | leader check term >= | mapgroup | S |
| G08 | live old server log removed | mapgroup | S (log only) |
| G09 | Decide does not need a majority | mapgroup | K |
| G10 | Decide accepts the old server as this one | mapgroup | K |
| G11 | Decide does not check the catalog's primary | mapgroup | K |
| I01 | hold rules drop the "local" lines | instance | K |
| I02 | hold rules drop replication | instance | K |
| I03 | hold rules allow the service role from any address | instance | S |
| I04 | Unquiesce keeps the backup | instance | S |
| C01 | mark without the term check | cmd/kingresd | S |
| C02 | quiesce does not set the proxy hold | cmd/kingresd | S |
| C03 | quiesce always answers quiesced | cmd/kingresd | S |
| C04 | quiesce does not need an address (ends local sessions) | cmd/kingresd | S |
| C05 | release the lease whatever the state | cmd/kingresd | S |
| C06 | fast demote does not relinquish | cmd/kingresd | S |
| C07 | staleTerm never refuses | cmd/kingresd | S |
| C08 | staleTerm skips terms below 1 | cmd/kingresd | S |
| O01 | running old primary is rebuilt | standby | S (Linux-only test) |
| O02 | term not newer is accepted | standby | S (Linux-only test) |
| O03 | the lease proof is not asked | standby | S (Linux-only test) |
| O04 | the plan naming this server is accepted | standby | S (Linux-only test) |
| O05 | another group's role file is accepted | standby | S (Linux-only test) |
| O06 | no proof function is accepted | standby | S (Linux-only test) |
| O07 | an interrupted rewind is not repaired by a backup | standby | S (Linux-only test) |

Totals: 70 mutants, 40 killed, 30 survived. The C and O groups (15 of the survivors) are survivors because no test that can
fail runs on Windows; the table test for O01 to O06 has a row for each, but it was not run.

## What was not examined

- The lease package beyond adopt.go (grantor, transfer, startwait, watchdog file), the client proxy, the rebalance interplay
  (StopForFailover, RBR-06), the extension C code other than switchover_verify, the ledger and signing changes that share the
  commit range, the console, the migrations 0015 to 0018 beyond their names, PostgreSQL 19, the race detector.
- The standby leader loop that watches old primaries (oldprimary_pg_test and leader.go), the entry point's choice of standbys,
  and "ready" for a rebuilt old primary.
- Time behaviour under a real clock for the stagger of the map monitor, and the effect of a clock step on the tracker.
- Any cluster test: the mutex was not free (see "What was run"). Mutants of the callee handlers (C, O) and the effect of F4-10
  on a real server therefore have no cluster evidence.
- The task engine's own recovery after a leader change (read only for Cancel, cancelTask and the lease guard), the F3-05,
  F3-08 and F3-10 fixes (the author says they have no cluster test), and the audit trail of the new calls.
