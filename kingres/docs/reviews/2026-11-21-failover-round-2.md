<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026 RooyeKhat Media Co -->

# Independent review: failover, round 2 (branch fo-r2wip)

Scope: commits 099ec5aab1 to aa48b684c4 (without ae4c20bdb5): restore of preferred primaries, plan of a switchover and its phases, the map group with a live
old leader (bootstrap side, leader side, F4-06 abandon, contract 0.15.0), HoldBack, F4-05 (signed map start), the two settled open points (refused proof,
checkpoint read from the old leader's server), OldServerWait.

Method: read the diffs and the surrounding code of kingresd (mapfailover.go, switchover.go, failover.go), failover (switchover.go, steps.go, restore.go,
service.go, repair.go, rows_pg.go), mapgroup, journal, lease (adopt.go), fwdproof, the contract and the docs. Unit tests of failover, failover/mapgroup,
failover/journal, lease and fwdproof were run from an export of the branch (all pass). No integration or cluster test was run. The export was deleted.

Result by severity: critical 0, high 1, medium 5, low 6.

Line numbers are those of the branch tip; where a function moved I give the function name.

---------------------------------------------------------------------------------------------------------------------------------------

## Findings

### F5-01 (high) The rollback after the map group's clean stop reads the stopped system database, so the candidate is neither released nor given up

Where: internal/failover/switchover.go, `rollbackFailed` (about line 163) and `rollbackSwitchover` (about line 135); the verdict loop in `stopClean` (about lines 303-350);
`stepCtx.row` and `runRow` in internal/failover/steps.go; internal/failover/rows_pg.go `PGRows.sys`.

What happens. For group 0, `stopClean` stops the old leader's primary, which is the system database. Everything after that point that reads rows goes through
`PGRows` -> `Conns.ServiceConn` -> `homeConn`, a connection to the instance that was just stopped. `rollbackFailed` does `gr, _ := sc.row(ctx)` and
`run, err := sc.runRow(ctx)` and ignores the errors: `gr.NewServer` is then "" and the term is -1. `rollbackSwitchover` only releases the lease on the new
server and sends `MapAbandon` when `newServer != ""`.

Failure scenario (the 6 minute silence, `MapVerdictWait`). The old leader stopped cleanly and sent the proof. The candidate does not promote (its standby
instance is down, `pg_promote` keeps answering `unavailable`, or the candidate is paused after 4 minutes of `PlannedWait`). After 6 minutes the task calls
`rollbackFailed`. The rows cannot be read, so `newServer == ""`: the fence/holder lease on the candidate is not released and the bootstrap is not given up.
`Resume` on the old leader removes its marker and writes the wait marker, but the old leader cannot win its lease while the candidate renews it. The map
group, and with it the whole control plane, stays down; the doc says "the old leader is started again". The same path is taken for `!ans.Released` after a
clean stop (the release did not reach a majority): the old leader's `ReleaseGroup` puts the renewal back, the task rolls back with `newServer == ""`, and the
candidate stays paused with its journal active (its engine gate closed) and its lease adoption alive.

The refused-proof case works only because the candidate lets go of its own lease before it tells the old leader (`PlannedRefused`); the unit tests use fake
rows that stay readable after the stop, so neither path is tested the way it runs.

Fix. `stopClean` already holds `nw` (the pinned standby): pass `nw.Server` to the rollback explicitly (a new `rollbackSwitchoverTo(newServer)` or a field on
`stepCtx`) and never derive it from a row that cannot be read after the stop of group 0. Treat an unreadable term as -1 on purpose and say so. Also make the
candidate's own handling of `map/abandon` release its adoption (see F5-02) so that a lost `MapAbandon` cannot keep the old leader out.

Test. Unit: in `TestACandidateThatNeverAnswersEndsTheMapGroupSwitchoverBySilence` make the fake rows return an error from `Group` and `Run` once the old leader's
`demote` has been answered, and assert `lease-release s2` and `map-abandon s2` are logged, and `resume s1`. A second case with `Released: false`. Cluster:
fault `promote:` = `unavailable` on the candidate (a new mode that answers 503 for ten minutes), the old leader must end with its primary running and the map
group writable within `MapVerdictWait` + one lease duration.

### F5-02 (medium) An abandoned bootstrap keeps renewing its lease

Where: internal/failover/journal/journal.go `Abandon`; cmd/kingresd/mapfailover.go `abandonLocal`, `handleMapAbandon`, and `Stuck`/monitor abandon.

`Journal.Abandon` only marks the journal and opens the gate. The fence/holder adoption that B3 and B5 created lives in the lease manager's memory and is not
unadopted (only `PlannedRefused` and the old leader's rollback, through `Lease(release)`, do that). After an operator's `abandonMapGroupBootstrap`, a
`Stuck` abandon by the monitor, or the old leader's `MapAbandon`, the candidate goes on renewing the lease of group 0 at a majority. If the old primary is
stopped (a planned switchover after the stop, or a hard failover whose old server came back with its primary held) it cannot win the lease and cannot start.
Recovery needs a restart of the candidate's kingresd (the adopted lease is not restored without an active journal). This makes F5-01 worse and affects the
F4-06 operation, which is documented as the operator's remedy.

Fix. In `abandonLocal`, after `Journal.Abandon` succeeds, call the lease manager's `Unadopt(0)` (it already refuses an attached, i.e. promoted, adoption, which
is the right answer after `ErrPromoted`). Test: unit with a fake lease port in `mapabandon_test.go` asserting the adoption is dropped; cluster: abandon a
bootstrap that waits for a majority and check that the old primary can be started after the old server is back.

### F5-03 (medium) Abandon can succeed on a standby that is already promoted (journal records the promotion after the call)

Where: internal/failover/mapgroup/mapgroup.go `promote` (about lines 541-600); journal.go `Abandon` (refuses only when `Done[StepPromote]` is set).

`StepPromote` is marked done by `Journal.Event` after the `Promote` call returned. In the interval in which `pg_promote` has run and the answer has not been
recorded (or kingresd crashed in it), `Abandon` (operator, old leader's rollback through `MapAbandon`, monitor) succeeds: the gate opens, the journal says
"abandoned", while the instance is writable and attached to the lease. Nothing then ends the bootstrap, the catalog does not follow, the old primary cannot
get the lease back, and the only way out is by hand. The refusal "`409 conflict` after the promotion" in the contract is therefore not exact. Combined with
the order inside `rollbackSwitchover` (the lease on the candidate is released and the old leader resumed BEFORE `MapAbandon` is asked), a rollback that races
a promotion can also make the old leader start a primary while a promoted copy exists unguarded (the promote handler checks the lease just before
`pg_promote`, so the window is a few milliseconds, but it exists; the lease manager then stops the promoted copy, which loses what it accepted, if anything).

Fix. (1) Write an intent event ("promoting") to the journal before calling `Promote`, and make `Abandon` refuse once it exists unless the standby is proven
still in recovery (ask the instance). (2) In `rollbackSwitchover` for group 0 call `MapAbandon` first and stop the rollback (keep the old leader held, report
`409`) when the candidate answers that it promoted; only then release leases and resume the old leader. Test: unit on mapgroup with a fake `Promote` that blocks,
call `Abandon` meanwhile, expect `ErrPromoted`; unit on `rollbackSwitchover` with a fake `MapAbandon` answering 409, expect no `resume`.

### F5-04 (medium) The map bootstrap turns "standby not yet caught up" into a final refusal

Where: mapgroup.go `promote`, case `IsCode(err, "unprovable"), IsCode(err, "standby_behind")`; cmd/kingresd/failover.go `handleFailoverPromote` (`ok == false` is
`standby_behind`).

`switchover_verify` answers `f` while the standby has not yet replayed the shutdown checkpoint, and raises KS006 only for a proof that can never be made
(design, S6: the ordinary groups poll for 30 s with `VerifyWait`). The map bootstrap calls `Promote` once the proof arrives, immediately after the old leader's
stop, and a single `f` ends the switchover: the candidate lets go of the lease, the old leader is restarted, and the task fails with `switchover_unproven`.
Under replication lag, a loaded machine or a slow disk this makes the planned switchover of the map group fail needlessly (safe direction, but it will be
the common failure outside the quiet test cluster). The ordinary-group path avoids it through the `Verify` step, which the map group skips
(`mapHandedOver` in `verify`).

Fix. Treat `standby_behind` as retryable for `VerifyWait` (30 s) inside `promote`, and refuse at once only for `unprovable`. Test: unit with a fake `Promote` that
answers `standby_behind` twice and then succeeds; cluster: delay replay on the standby (fault on the receiver) for 10 s after the stop.

### F5-05 (medium) F4-05 hands a password-equivalent value to a member that is not the leader; the documents still say the opposite

Where: cmd/kingresd/mapfailover.go `sign`, `openMapStart`; internal/fwdproof/fwdproof.go (package comment); README.md "CP2-01" row and the "Forwarded identities carry a
SCRAM-style proof" item; internal-api.md "Forwarded identities need a proof".

The fwdproof design says: the receiver of a proof recovers the user's ClientKey (it is `Value XOR HMAC(StoredKey, ...)`), the ClientKey is password-equivalent, a
proof is therefore sent to the manager leader only, and "no other member ever receives a value from which a ClientKey can be derived". The signed form of
`map/start` sends such a proof to the candidate, a member that is not the leader (the leader is gone, so it cannot be the receiver). A compromised candidate
learns the operator's ClientKey and can use it toward every server for that user until the password changes. The candidate becomes the leader if the
bootstrap succeeds, which is the argument for accepting it, but the candidate is chosen by the receiving member among all servers that host a ready standby of
the map group, and it is exposed even when the bootstrap is refused. Neither the ADR nor the documents mention the change; the README and internal-api.md
statements are now false.

Not a replay or forgery problem: the proof is bound to kind, sender, receiver, user, roles, operation, parameter, request id and body bytes; the nonce cache
works; the role check runs on the proven roles; the plain form is refused (see "found sound").

Fix. Decide and write down: either accept it in an ADR (candidate is the next leader; the operator already trusts every server with a standby of the system
database) and correct the three documents, or avoid it (for example sign with a key derived per request that the verifier can check without learning the
ClientKey is not possible with this scheme; an operator-confirmed one-time token on the candidate's own HTTP API is). A session of the token kind
(`KindToken`, no `ProofKey`) always gets `503 service_unavailable` from `Start` when the candidate is another server: say so in the operation text. Test:
a doc check in `check-contract` or a README test that fails when the "no other member ever receives" sentence and the signed `map/start` coexist is overkill; a
unit test that a token session answers a documented code is enough.

### F5-06 (medium) `mapHandedOver` fails open

Where: internal/failover/switchover.go `mapHandedOver` (about line 537) and its callers `stopClean`, `verify`, `handOver`, `promote`, `takeFence`.

`mapHandedOver` returns false when `Peers.State` returns an error. On the new leader's engine, which re-runs the old leader's task, a transient error on that
call (the call is to its own server) makes `stopClean` start a fresh planned bootstrap for the old server (refused by `Decide`: the catalog primary changed),
and the refusal goes through `rollbackFailed` -> `rollbackSwitchover`, which releases a lease on this server, sends `Resume` and `Unquiesce` to the OLD
leader's server (its primary is the old, stopped one with the role file still "primary") and gives the bootstrap up. A step that can only be right when the
group was NOT handed over must not run on a failed read.

Fix. Return `(bool, error)` and make the step return the error (a retry) when the state cannot be read; never roll back a group of a task whose row says
`promoted`. `rollbackFailed` should also refuse to run for a group whose row is `promoted` (the comment on `rollbackSwitchover` says it never runs for a promoted
group, but nothing enforces it). Test: unit with `State` failing once on the new leader after the row says promoted; expect a retry and no `resume`.

### F5-07 (low) `demoteClean` leaves the renewal running on the `pg_controldata` error path

Where: cmd/kingresd/switchover.go `demoteClean`, the `ControlData` error return.

After `HoldBack` the renewal runs until `ReleaseGroup` or `Relinquish`. The branch `cd, err := ControlData(...)` returns 503 without either: the instance is stopped, the
marker keeps it stopped, and the lease of the group goes on being renewed by a primary that does not run. The repair loop skips group 0, and for other groups
only undoes a hold after 60 s with no unfinished task. Fix: `defer` a `Relinquish` that is cancelled only on the two successful ends (release done, or the
renewal handed over); simplest is `Relinquish` in that branch. Test: unit with a fake manager whose `ControlData` fails; assert the renewal ends (the `lease`
package already has the rig).

### F5-08 (low) `POST /failover/map/abandon` is accepted from any peer without proof, and abandoning a planned bootstrap does not tell the old leader

Where: internalapi.go (policy `AnyPeer`), mapfailover.go `handleMapAbandon`.

Any member can abandon the bootstrap in progress on another server (the caller and the reason are not checked; the audit event exists only for the HTTP
operation). For an ordinary bootstrap that is the F4-06 intent. For a planned bootstrap after the old leader stopped, the abandon leaves the old leader
waiting the whole `MapVerdictWait` (6 min) with the control plane down, because the candidate does not call `PlannedRefused` (and the lease is not released,
F5-02). Fix: for a journal with `Planned`, `abandonLocal` should do what the refused path does (release the lease, tell the old leader); accept
`map/abandon` between servers only from a server that hosts a standby of the map group or the old server of the journal, as `map/continue` does
(`isCandidate`). Test: handler unit tests for the forbidden caller and for the planned case.

### F5-09 (low) The old leader's memory of the candidate and of a refusal is a process global

Where: cmd/kingresd/switchover.go `MapStart`, mapfailover.go `plannedBox`, `handleMapRefused`.

`mapPlanned` is one box per process. A refusal is remembered after the task that asked for it has ended and is cleared only by the next `MapStart`; it is lost
when the old leader's kingresd restarts after the stop (the candidate's `refused` call then answers 409, `PlannedRefused` returns an error, and the candidate
pauses with its lease already released and its journal active; a later repeat of the request can win the lease again and, with the same proof, promote
after the old leader's task was already rolled back or failed: safe for the data because `switchover_verify` and the lease still decide, but the
old leader's task and the real state then disagree, and `RecordPlanned` updates a row of a failed task).
Fix: key the box by task id (carry the task id in `refused`), and on a failed `refused` call keep the journal abandoned rather than active (the lease is
already released). Test: unit for a stale refusal and for a refusal of another task id.

### F5-10 (low) `OldServerWait` (15 s) weakens the fence of last resort in every failover

Where: internal/failover/steps.go `oldState`, used by `checkCatalog` ("The fence of last resort") and `records`.

The 15 s bound was added for the old leader of the map group, but it applies to all groups and modes. `checkCatalog` asks the old server's state and demotes an old
primary that still runs; when the old server answers "not ready" or slowly for longer than 15 s the check is skipped and the task goes on. The lease still
fences, so this is not a safety hole, but the alert `oldPrimaryRunning` and the immediate stop used to be reached by a wait of minutes. Fix: use the short bound
only for group 0 of a planned switchover (where the old server's instance is the stopped system database), keep the old wait elsewhere; or log and raise an
alert when the check is skipped. Test: unit with a slow fake `State` for a hard failover: the old behaviour must hold.

### F5-11 (low) Documents and contract text that do not match what is built

- internal-api.md, `POST /failover/map/proof`: "Not reachable yet: the old leader's task that sends it is built with ..." is false now.
- design ha-phase5-part2.md still carries "Not built, and refused: a live old leader" in item 4 of the review list and the progress paragraph "Not built: the leader's side
  ... the four open points stand", followed by later paragraphs that say the opposite; a reader of the review list is misled.
- openapi.yaml `restorePreferredPrimaries`: the new paragraph is headed "0.13.0, how the restore works" inside the 0.15.0 revision, and it lists the skip reasons
  as `preferredNotReady`, `mapGroup` while the schema says `mapGroup` is reserved and never produced. README changelog 0.14.0 was edited retroactively (a new
  bullet for `restorePreferredPrimaries`); it belongs to 0.15.0.
- The new operation `abandonMapGroupBootstrap` needs `kingres_admin` while the failover that it undoes needs `kingres_ops`; fine if intended, but the text should say so.
- F4-05 changes (F5-05) are not in internal-api.md's trust section.

`check-contract.mjs` is consistent with the server: version 0.15.0, the changelog words, the singular "1 operation added", 141 operations and 114 paths in the
README, `operations_gen.go` has the new operation and the audit action. The script cannot check the prose above.

### F5-12 (low) Test gaps and the checkpoint endpoint

Gaps found while reading the tests (the unit tests that exist pass; the cluster tests were not run):
1. `handleMapStart` is not unit tested for the planned form from a caller other than `b.Server` (403) or with `TaskID <= 0`; `openMapStart` is.
2. `handleMapRefused` has no test for a caller that is not the server the task started (409) or for the box being cleared.
3. `handleMapProof` has no test for the wrong caller (403) or for a differing second proof.
4. `handleMapCheckpoint` has no test (running instance 409, no primary 409, `ControlData` failure 503).
5. The rollback tests use rows that stay readable after the stop (F5-01), and the silence test does not assert the lease release or the abandon.
6. No test calls `Abandon` while `Promote` is in flight (F5-03), and none restarts the candidate between `PlannedRefused` and `Abandon` (F5-09).
7. No test for `standby_behind` that clears (F5-04).
8. `TestHoldBackKeepsRenewingThroughASlowStop` shows the renewal continues; it was not shown failing on `Relinquish` (the documented plan says it will be in the gate). I
   did not run a mutation (the tool that would do it was refused), so I cannot say it fails without the code.

`handleMapCheckpoint` ignores the caller and answers any peer. The content (an LSN and a state) gives nothing away, and the candidate only uses it after the
standby's `switchover_verify`, so this is acceptable; but `Running` and `ControlData` are two separate steps (a start between them gives "in production",
which the candidate refuses). Cheap hardening: answer only while the relinquished/fenced marker for the group exists on this server, so that an answer can
never be read from a primary that the start gate would allow to start.

---------------------------------------------------------------------------------------------------------------------------------------

## What was checked and found sound

Split brain and data loss
- Lease exclusivity argument for the planned switchover: the old leader renews until the stop ends (HoldBack), releases at a majority after a clean stop
  (`ReleaseGroup`), and only then can the candidate hold the lease; the candidate holds it before the proof is used, and the promote handler checks `Holds`
  immediately before `pg_promote`. A kingresd that dies in the held stop stops renewing, so the lease lapses and the watchdog fences a hung instance; the marker
  file survives and the start gate refuses a start (`lease.Gate`, `loadRelinquished`). Reads of the marker after a restart do not renew.
- The proof around a promotion: `switchover_verify` runs again inside the promote call, before any change, for sent and for read proofs; a standby that is
  behind, or that saw more WAL than the shutdown checkpoint (the old primary restarted and wrote), is refused. A restarted old primary between the checkpoint
  read and the promotion is therefore caught by the proof and also cannot hold the lease while the candidate holds it. A made-up state or checkpoint from an
  old server cannot cause a promotion: the pulled proof is accepted only with state `shut down` and a non-empty checkpoint, and the standby checks it.
- The refused-proof path releases the candidate's lease before it tells the old leader, and nothing was promoted when `unprovable` or `standby_behind` came
  from the callee (the refusal happens before the role file is written). Release of an attached (promoted) adoption is refused by `Unadopt` (F3-01), so the
  leader's rollback cannot take the lease from a promoted candidate once it is attached (the remaining window is in F5-03).
- No path found in which an acknowledged write is lost without a leader or candidate failure at the same time; the F5-03 window is the one to close.
- Idempotence of the journal: `SetPlanned` is idempotent for the same task and standby, `SetProof` keeps the first proof, `Begin` returns the active journal of
  the same term and old server, `ResumeOnStart` undoes lease and promote only when the catalog step is not done, and a promote repeated on a promoted standby
  answers `already` before the proof is asked.
- The map group is last in a switchover and restore (`mapLast`), the freeze is asked again before every step, the re-run steps on the new leader skip what the
  bootstrap did (except for the fail-open read in F5-06), and `runRowOf` reads the term the bootstrap raised, `checkCatalog` compares with the term the run began with.

Authentication and authorization (F4-05 and the callers)
- The signed request is bound to kind, from, to, user, roles, operation, parameter, content type, body bytes and request id (`ProofMessage`), the nonce cache rejects
  a reused proof within the window, a proof made for another sender or receiver fails, a changed body fails, a wrong key fails, the role check uses the proven
  roles and requires `kingres_ops`. `allowDataLoss` and the author come from the proven request only (`RequestedBy` is overwritten by the proven user); the
  plain form is accepted only with `planned`, from the old server named in the body, which `Decide` requires to be the catalog primary of group 0 and live;
  `planned` inside a signed request is refused; `Trigger` is forced to `manual`; a planned request has `AllowDataLoss` forced to false and its proof fields
  cleared. A member cannot start a bootstrap with data loss accepted, or under another user's name, without that user's ClientKey. The nonce cache is in memory:
  a candidate restart opens a replay window of at most the 2 minutes of the proof window; the replay can only repeat a request that `Decide` then refuses unless
  the catalog still names the old server.
- `map/proof` is accepted from the journal's old server only and only for a planned journal; the standby's own check decides; `map/continue` requires that the caller hosts
  a ready standby and that the journal stops at B4; `map/refused` is accepted only from the server the task started the bootstrap on (but see F5-09);
  `map/checkpoint` reveals an LSN only (F5-12); `map/abandon` has no caller check (F5-08).
- Roles in the signed request are asserted by the signer, as for a forwarded request to the leader: same trust, noted for completeness.

Contract, docs, rules
- Contract 0.15.0 matches the server: `abandonMapGroupBootstrap` (`POST /failover/map/abandon`, `kingres_admin`, 400/401/403/405/409/422/503, body `{reason}` 1 to
  500 characters, control characters refused in the handler), `FailoverGroupPlan.skipped`, audit action, `operations_gen.go`, `check-contract.mjs` revision and rules.
  A hard failover of a live leader's server is still 422 in `Preflight` and in `Decide` (manual) and 409 (automatic); the documents say so.
- New and changed files carry the SPDX AGPL-3.0-only header; added lines are ASCII; no machine-local path or user name in the added lines.
- Unit tests pass: failover, failover/mapgroup, failover/journal, lease, fwdproof (run with `-p 2` from an export; the export was removed).

Not reviewed in depth: the restore resolution (`restoreInput`, `restoreContinues`, `resumeRestore`) was read once and looked consistent (the preferred server is the
node with the lowest id, a group without a ready standby on it is skipped with a reason, a failure before the promotion rolls the group back and goes on; a
failure after the promotion is the task's). One question for the author: `preflightGraceful` returns a refusal for the whole request when group 0 is in the
list and the old server of group 0 is not this server; for a restore that could be a skip of the group instead of a refusal of the restore.

---------------------------------------------------------------------------------------------------------------------------------------

## Runs I would like in a lane

1. Cluster: planned switchover of the map group where the candidate's promote answers `unavailable` for longer than `MapVerdictWait` (F5-01). Expected after the fix:
   the old leader's primary runs again, the candidate's journal is abandoned and its lease released.
2. Cluster: `abandonMapGroupBootstrap` for a bootstrap that holds a fence lease; then start the old primary (F5-02).
3. Cluster or fault test: `Abandon` while `Promote` is in flight (F5-03), with the old leader's rollback racing it.
4. Cluster: delay the standby's replay of the shutdown checkpoint by 10 s after the stop (F5-04); the switchover must succeed.
5. Cluster: kill the candidate's kingresd between the `refused` call and the abandon (F5-09); and restart the old leader's kingresd between the proof and the verdict.
6. Cluster: `pg_controldata` failure injected in `demoteClean` (F5-07); the lease must lapse.
7. Gate runs of the existing three map-group cluster tests (`TestSwitchoverOfTheMapGroup`, `...WhoseProofIsRefused`, `...WhoseOldLeaderEndsAtTheStop`) and the HoldBack test, each shown failing on the code it covers (mutation: HoldBack replaced by Relinquish; `promote:unproven` removed; `ProofPullAfter` set to an hour).

---------------------------------------------------------------------------------------------------------------------------------------

## Fixes

Fixed on the branch of the review (fo-r2wip), one commit per finding or pair of findings, titled "fixes for the review of failover round 2 (F5-xx)".
The commit ids are those after the rebase onto kingres-18 at e279c14091. For each fix a test fails without it (a mutant of the fix was run and killed);
F5-05 and F5-11 are documents and have no mutant.

| Finding | Commit | What changed |
| --- | --- | --- |
| F5-01 | d56f075ff5 | The rollback takes the candidate's server from the step, not from the stopped system database; an unreadable term is -1; the fake makes rows unreadable after the stop. |
| F5-02, F5-08 | 22636424ed | A bootstrap that is given up lets go of its lease; a planned one tells the old leader; the abandon call checks its caller for a planned journal. |
| F5-03 | 7fd5bee3d2 | The intent to promote is on disk before the call; an abandon refuses once it exists unless the instance is still in recovery; the rollback asks the candidate before it releases a lease or resumes the old leader. |
| F5-04, F5-09 | 3cd0bed0c2 | `standby_behind` is polled on the map group like on ordinary groups; the old leader's memory of the candidate and of a refusal is keyed by task. |
| F5-06 | 6c4cbbf01f | An error from the state call is not read as "not handed over"; a group whose row says promoted or done is never rolled back. |
| F5-07 | cd2fcf75fe | The renewal of the lease ends when `pg_controldata` of the stopped instance cannot be read. |
| F5-10 | 132f2a6d29 | The 15 s bound on calls to the old server applies only to the map group of a planned switchover. |
| F5-12 | d8e417f50c | The checkpoint is answered only while a marker holds the primary back; tests of the callers of map/start, map/proof, map/refused and map/abandon. |
| F5-05 | f528900284 | ADR 0026 accepts that the candidate of a map group failover receives the operator's proof; fwdproof, README and internal-api.md say the same. |
| F5-11 | 48689bc894 | Documents and contract text say what is built (restore text moved to 0.15.0, old-leader and proof text). |

The cluster tests that the last section of the review asks for found three more defects, all fixed:

- **The step ended with the task lease** (828719283d). About ten seconds after the clean stop of the map group the engine cancels the context of the step, because the task
  lease is in the stopped system database. The verdict wait ended with it, no rollback ran, and the old primary stayed stopped. After the stop the step now runs on a context
  that is not cancelled, bounded by the verdict wait; a proof that cannot be sent rolls back.
- **A second switchover** (21689b6fb4, 81fbfdd74f). A rollback that finishes after the lease ran out cannot be recorded, so the engine runs the step again. The candidate now
  refuses a planned start from a task whose switchover it gave up, and the step ends as `switchover_unproven`.
- **A nil dereference in the lease manager** (7ac59bee1a). A release can drop a group's state between a renewal round and the write of the watchdog file; kingresd crashed on it in
  the rollback of a map group switchover. The write returns when the state is gone. The code was like that before the review; the rollback makes the race likely.

Cluster tests (lane l3, PostgreSQL 18 and 19, `internal/clustertest/failover_mapfaults_test.go`, with the fault points `promote:unavailable`, `promote:slow`, `promote:behind`
and `demote:controldata`): a candidate that never promotes, an operator abandon, an abandon while the promoting call is in flight, a standby that is behind, and a
`pg_controldata` that cannot be read. The review's item 5 (the candidate killed between a refusal and the abandon, the old leader restarted between the proof and the verdict) is
too hard to time for a cluster test and is covered by unit tests.
