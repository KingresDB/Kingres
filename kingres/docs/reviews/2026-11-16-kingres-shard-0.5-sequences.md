<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
# Adversarial review: contrib/kingres_shard 0.5, the call sequences and the S8 fix round

Reviewer: an independent reviewer (not the author of the code, not the reviewer of the
earlier rounds). Findings of this review are numbered S9-nn (the previous review,
`2026-11-16-kingres-shard-0.5-fixes.md`, used S8-nn).

Reviewed state: the fix round that answers S8-01 to S8-08 (the last commit that touches
`contrib/kingres_shard`), exported with `git archive` of `contrib/kingres_shard`,
`contrib/kingres_ledger`, `kingres/dev` and `kingres/docs` into a private area. The working tree
was not used for any result.

## Scope and method

Read: the previous review (findings, the table of sixteen guarantees, the verdicts on the
sequences, "Not reproduced", "Not covered"); the README chapter "High availability" in full,
above all "What the control plane calls"; `src/failover.c` in full; in `src/ha.c` the functions
`kingres_shard_sync_confirmed`, `kingres_shard_wait_confirmed`, `kingres_internal_sync_confirm`
and the probe; in `src/node_conn.c` `prepare_one`, `prepare_collect`, `bounded_get_last`,
`log_decision`, `precommit`, `postcommit`; in `src/recovery.c` the resolution loop
(`origin_decision`, the commit/rollback branch, `purge_log`, `check_participant_parts` by their
calls); `kingres_rotate_secret` in `src/node_admin.c`; the move loop of `kingres.move_bucket` and
the new helper functions in the 0.5 install script; `internal_promotion_state`. I did not read
`bucket_move.c`, the planner or the scan code.

Environment: private copies of the PostgreSQL installations with assertions (18.7 and 19rc2),
relocated (the `pgxs` makefile prefix fixed in my copy, `pg_config` reports paths in my area;
nothing was written into the originals); the extension built out of tree with
`kingres/dev/ext.sh`, `EXT_WERROR=1`, `-j2`, under `nice`. Baseline runs of the whole suite on
each major. Then my own experiments as TAP files (clusters of two to five nodes with real
streaming standbys, driven by the helper module of the suite for the set-up only; each call
sequence was written from the README text, not from `t/073`), ports 59700 to 59899 (the port range
patched in my copy of the Perl cluster module), every server stopped afterwards. Where timing
mattered I made it deterministic: a stopped standby, a cancel of a backend found by its wait
event (`SyncRep`), a fault point that waits on a file, a held lock, a decision retention of 8
seconds. Experiments marked "both majors" gave the same result on 18.7 and 19rc2.

"Reproduced" means I observed it. "By reading only" means I did not run it. A partition was
emulated by stopping the nodes of the other side; no packet filter was used (see "Not covered").

## Baseline runs

| Major | Regression tests | TAP files | TAP tests | Failed | Result |
|---|---|---|---|---|---|
| 18.7 (assertions) | 12 of 12 | 74 | 7021 | 1 (t/027, the throughput threshold "the writers ran 160 operations", threshold 200) | t/027 alone, rerun on the quiet machine: passes; no compiler warning with -Werror |
| 19rc2 (assertions) | 12 of 12 | 74 | 7679 | 1 (the same test, 189 operations) | t/027 alone: passes; no compiler warning with -Werror |

The baseline ran while the machine carried a load average above 50 (other jobs and my own
experiments); t/027 fails there on the number of operations the writers manage, not on any
correctness check, and passes alone on both majors. The commit message says 7051 and 7709 tests; I
count 30 fewer on each major, as the previous reviewer did (same files, same result; I did not look
for the difference).

## Summary

New findings: 1 critical, 2 high, 3 medium, 3 low, 1 note.

| ID | Severity | Summary |
|---|---|---|
| S9-01 | critical | A committed transaction's part on a participant is rolled back by recovery, with no record anywhere, when the participant's `COMMIT PREPARED` was not confirmed by its standby and the participant fails over after `decision_retention`. The README says "after the retention only the incident record (`participant_lost`) is left"; no record is left |
| S9-02 | high | The strict-majority rule for the map group is bypassed: `promote_node` of an ordinary group is accepted on a promoted map standby that the catalog has not yet recognised (it still holds the old map primary's node id), with no electorate; the group it just promoted then counts as "reached" and the map group promotion passes. Two sides can both promote |
| S9-03 | high | A node that dies between PREPARE and COMMIT PREPARED of `rotate_secret()` is cut off from the cluster for ever: the origin already holds the new secret, the node the old one, every node-to-node session between them is refused in both directions, and recovery needs such a session to commit the prepared rotation. A human must run `COMMIT PREPARED`. Not in the README |
| S9-04 | medium | "Repeat `promote_node` until `already`" can loop for ever: any role that can read `kingres.map_version` can prepare a transaction of its own that holds a lock on it, and `promotion_complete` counts that as an unfinished catalog change on that node: every repeat answers `completed`, `final: false` |
| S9-05 | medium | "Failover when OLD is reachable": the call fails with KS005 after `2 x node_timeout + 5` seconds, every time, as long as OLD's `synchronous_standby_names` names NEW (the normal configuration); the text says the call is the same as the ordinary failover and the answer lists no unreached node. What is needed (clear OLD's list first) is not in the text |
| S9-06 | low | With OLD reachable, `promote_node` leaves two false `participant_lost_committed_transaction` incidents (30 days) and a non-empty `check_cluster()`: its own catalog transaction is judged "lost" on the promoted group |
| S9-07 | medium | "Planned switchover without loss": the nothing-is-lost claim holds only if NEW was streaming when OLD stopped, and nothing after the stop checks that NEW has OLD's last WAL; with the allowed precondition "or is caught up" three acknowledged rows were lost with no record. For the map group the step "MAP: promote_node" must run on NEW; the text does not say so |
| S9-08 | low | Wrong-order calls are refused, but slowly and obscurely: `remove_node` before clearing the list takes `2 x node_timeout + 5` s (125 s by default) and ends in KS005, which is not among the answers named for that step; a typo in the name of step 2 of "Registering a standby" hangs single-group commits with no bound until corrected |
| S9-09 | low | `internal_sync_confirm` "refuses all but a node session and a superuser": on a node that is not yet part of a cluster a plain role that connects with `-c kingres_shard.forwarded=on` is let in (answer `true`) |
| S9-10 | note | A cluster with one other group (or a majority of groups lost together with the map group) cannot promote its map group, and the README gives the operator no way out |

Rating of S9-01: by the letter of the rubric (a transaction committed on one participant and gone
on another without a record) it is critical. The conditions are narrow and each of them is a
documented weaker case (a participant whose standby did not confirm, a failover after the
retention); what is wrong is that the README promises a record and none is made.

No server crash, no wrong answer from a scan and no escalation of privilege was found. A committed
row lost or doubled with no record: S9-01 only. Two primaries of one group both serving: S9-02
(through the cluster, on the two sides of a partition, with the fencing the text demands skipped
or impossible).

## Fate of S8-01 to S8-08

| ID | Fate | Evidence |
|---|---|---|
| S8-01 | fixed for the cancel and the termination of the participant; **the retention edge is not safe (S9-01)** | Participant's PREPARE wait cancelled with `pg_cancel_backend` (standby stopped, `node_timeout` 20): after the participant's own question the origin aborts with KS005, nothing committed, nothing prepared on any node afterwards, 25 s (20 s wait plus 5 s question; both majors). Database setting `synchronous_commit = off` and role setting `local` on the participant: raised to `on`, KS005 after 12 s with `node_timeout` 4 (both majors). Unconfirmed `COMMIT PREPARED` (cancelled): client gets COMMIT with a WARNING that names the node, rows visible, decision kept; a failover of the participant inside the retention: recovery commits the part again (both rows present, control run with retention 300 s). After the retention: S9-01 |
| S8-02 | fixed | An inherited list that nobody serves: refused in 4 s with "node 4 cannot commit: its synchronous standbys do not confirm", nothing changed, nothing prepared (list `ANY 1 (a, b)` with both down, and a list naming a stopped standby; both majors). After the list is cleared: `promoted` (`final: false`), then `already` (`final: true`). The loop rule has an exit that an outsider can close: S9-04 |
| S8-03 | fixed | `rotate_secret()` while a tagged transaction is prepared (origin killed after PREPARE): "cannot be rotated while distributed transactions are in doubt"; `recover_prepared_transactions()` resolves it; the next rotation works. After a rotation a distributed commit from a session opened before it works, a transaction that prepares after it and whose origin dies is resolved by recovery (18.7). A node that is down: the rotation is refused up front, nothing changes. A node that dies between PREPARE and COMMIT PREPARED of the rotation itself: S9-03 |
| S8-04 | fixed for the direct case, **bypassed (S9-02)** | Five groups, electorate of four: 2 of 4 reached (tie): refused; 1 of 4: refused; 2 of 4 again: refused; 3 of 4: promoted (18.7). Three groups, map and group 1 down together: refused with 1 of 2. Two groups, the other one down: refused with "only 0 of the 1 other shard groups can be reached" |
| S8-05 | fixed | A repeated call lists the nodes it cannot reach: `already`, `"unreached": [2]` after the first call told nobody; with a second standby of the group that is down: `[2, 5]` (both majors) |
| S8-06 | fixed | `dtx_applied_applied_at_idx` is in the install script and in the 0.4 to 0.5 script; the cleanup follows `decision_retention + 60` (by reading; `t/072` passes) |
| S8-07 | documented | README precondition 1 states it. Reproduced once more: with OLD reachable and not stopped, a write on OLD (its own group's key, routing off) is accepted (rc 0) and is not visible through any other node afterwards |
| S8-08 | bounded | The commit of a participant whose standby is down ends in KS005 after about `2 x node_timeout + 5` s: 12 s (`node_timeout` 4), 17 s (6), 21 s (8), 125 s (60, the default; `remove_node`). The README formula is right. The cancel that "may not arrive" was not testable, see "Not reproduced" |

## What the control plane calls: the nine sequences

Method for every sequence: the text was executed literally, as a script that does only what it
says, on a cluster with real standbys; then each step was broken. "As written" means a script
following the text reaches the postconditions.

### 1. Registering a standby

Verdict: right, incomplete.

Executed: `add_node(..., 'standby')` on MAP; the standby id returned; the name put in the primary's
list; `standby_status()` read.

Answers observed:

| Call | Answer |
|---|---|
| `add_node` on a node that is not MAP | `this function can only run on the map group primary` (not named in the text) |
| `add_node` for a standby with the wrong group | `node ... is not a copy of the primary of group 2` (named) |
| the same standby twice | `node ... is already registered as node N` (not named) |
| `standby_status()` before step 2 | `streaming`, `async` |
| after step 2 with the right name | `streaming`, `quorum` |
| after step 2 with a typo | `streaming`, `async`, `problem` empty; `check_cluster()` empty |

What to add: (1) step 2 is made before step 3 checks the name, and with a name that matches
nobody every commit that writes in that group only waits for ever (PostgreSQL's wait, no timeout;
a writer was still waiting 13 s later and was released the moment the list was corrected), and a
commit that also writes in another group ends in KS005 after `2 x node_timeout + 5` s. Put the
check before the edit: the primary's `pg_stat_replication.application_name` must equal the name
that goes into the list. (2) Name the two other errors above. (3) `check_cluster()` reports nothing
for an `async` standby that is named in no list; if that is intended, say so.

Breaks: caller on the wrong node, wrong group, repeated call: clear refusals. Not tested: MAP
killed during `add_node`, a second caller at the same moment (suite only).

### 2. Failover of an ordinary group

Verdict: right as written for the dead-OLD case; incomplete in two places.

Executed: OLD stopped `immediate`, NEW promoted with `pg_promote`, list cleared on NEW, then the
call on MAP, repeated until `already`, OTHERS re-pointed, list set.

Answers observed (all named unless marked): `promoted` with `"final": false` then `already` with
`"final": true`; `unreached` `[2]`, and `[2, 5]` when OTHERS is down; the refusal "node N cannot
commit" after 4 s for an inherited list (also for a list naming only a down standby); "node N is
still in recovery" for OTHERS; "could not connect to node N ... which is to become the primary" for
the stopped OLD; "this function can only run on the map group primary" on a non-MAP node; "node 99
does not exist" (not named); `promote_node(NULL)` returns NULL with no error (not named; strict
function); `promote_node` of a primary that already is one answers `already`.

Breaks:
- Three callers at the same moment: `promoted`, `already`, `already`; roles and terms identical on
  all nodes, nothing prepared (both majors).
- NEW killed at `promote_before_change` while the call waits: the call fails with "server closed the
  connection unexpectedly" (the named class "connection breaks"), nothing changed, nothing prepared;
  after NEW restarts the repeat answers `promoted`, then `already`.
- Application writing distributed transactions (a row in group 0 and one in group 1 per
  transaction, 1100 attempts) while group 1 is failed over with OLD stopped fast: every acknowledged
  transaction is present in both groups, no half transaction, no prepared transaction left, no
  incident (both majors).
- A foreign prepared transaction that holds a lock on `kingres.map_version` on another primary:
  the repeat loop never ends, S9-04.
- A second node of the same group (OTHERS) promoted by mistake: refused while it is in recovery.

What to add: (1) a bound on the repeat loop and what to do when it is exceeded (S9-04). (2) Step 5
tells the caller to read `check_cluster()`; with OLD reachable it is not empty for a false reason
(S9-06). (3) `promote_node(NULL)` and the two unnamed errors.

### 3. Failover of the map group

Verdict: right for the plain case; **wrong as a guarantee**, because the rule that the text sells
(two sides cannot both promote) can be walked around (S9-02); incomplete for what the operator does
when the rule refuses (S9-10).

Executed: map primary stopped, standby promoted, list cleared, `promote_node(NEW)` on NEW itself,
`sync_catalog()` once the others are back; electorates of one, two, three and four other groups.

Answers observed: `promoted` then `already` (`unreached` lists the old map primary); "only k of the
n other shard groups can be reached from this node; the map group cannot be promoted from a minority
of them" (named); "this node is not a copy of the primary of group 0 and cannot become it" on a
non-promoted node (named); "this function can only run on the map group primary" when the
promotion of an ordinary group is asked of the promoted node of that group itself (named for the
other sequence). With five groups (electorate of four):
`2 of 4` refused, `1 of 4` refused, `2 of 4` refused again, `3 of 4` promoted; a node that was down
during it learned the term from `sync_catalog()` (term 1 at once, nothing prepared).

Breaks:
- Map primary and group 1's primary down together (three groups, standbys for both): the map
  promotion is refused (1 of 2); the caller promotes group 1's standby "from the promoted map
  standby" (the only node that can still run an ordinary promotion): accepted with no majority check
  (`promoted`, `unreached [2]`); the map promotion is then accepted (it counts the group it just
  promoted as reached): term 2, roles consistent on three nodes. Both majors. S9-02.
- The same with the old map primary alive and its side of the partition emulated by stopping the
  other nodes: the promoted standby changes its own catalog and raises the term to 1 while the old
  map primary still says term 0 (18.7).
- A write through the promoted map standby and a write through the third node, both accepted.

What to change in the text: the sentence "Two sides of a partition therefore cannot both promote the
map group" is false until S9-02 is fixed; until then say that no ordinary-group promotion may be run
on a node that has not been recognised as the map primary, and that the control plane must make the
decision itself.

### 4. Failover when OLD is reachable but must be demoted

Verdict: **wrong as written** (S9-05, S9-06).

Executed: NEW promoted with `pg_promote`, OLD left running (its list `ANY 1 (<NEW>)`, unchanged),
list cleared on NEW, `promote_node` on MAP.

Answers observed: after `2 x node_timeout + 5` seconds (21 s with `node_timeout` 8) the error "node ...
did not confirm that its part of the transaction is durable on its synchronous standby" (KS005) and
the warning "could not roll back prepared transaction ... recovery will resolve it"; the catalog is
unchanged; a repeat does the same. The error is in the table of sequence 2 ("a participant of the
call ... cannot get its own standby to confirm; fix that node's list or standby"), but this
sequence promises the opposite and the node whose list must be fixed is OLD, which this text never
touches. After OLD's list is cleared and reloaded: `promoted`, `"unreached": []` (as the text
says), then `already`; OLD's roles show it as `standby`, term 1 (both majors).

What the text must say: before the call, clear (or repoint) OLD's `synchronous_standby_names`, or
the call cannot succeed; expect up to `2 x node_timeout + 5` s per refused call; and that the first
`check_cluster()` shows the two false rows of S9-06 for transactions of the call itself.

Break: a write on OLD (own group's key, routing off) during the window: accepted, unseen through the
cluster afterwards (S8-07, documented).

### 5. Planned switchover without loss

Verdict: right when the preconditions hold; **incomplete**: "nothing is lost" is not checked after
the stop (S9-07), the alternative branch "without the stop" does not work with a synchronous
standby (S9-05), and the map-group variant names the wrong node.

Executed, ordinary group: an application writes pairs of rows (one per group) in a loop; OLD
`pg_ctl stop -m fast`; NEW promoted; list cleared; `promote_node` on MAP: `promoted`, `unreached`
`[OLD]`; repeat: `already`. Two runs on each major: connection through MAP (OLD a participant) and
connection through OLD (OLD the origin of the transactions that are cut off). Result in all four
runs: every acknowledged pair is present in both groups, every other pair is absent from both, no
half pair, no prepared transaction on any node afterwards, no incident, `check_cluster()` shows
only OLD as `standby_unreachable` (about 420 to 1080 acknowledged pairs per run). I could not lose
anything with the text followed.

Break that loses data (S9-07, 18.7): NEW's walreceiver disconnected (wrong `primary_conninfo`,
reload), NEW not named in OLD's list (the allowed "or is caught up"), three rows written and
acknowledged on OLD, OLD stopped fast, NEW promoted, `promote_node`: answer `promoted`; the three
rows are gone; `check_cluster()` shows only `standby_unreachable`; `dtx_incident` empty. Before the
stop, `standby_status()` had shown `not connected` and `standby_not_streaming`, so a caller that
reads it is safe; the text does not make that a step, and nothing after the stop compares NEW's
received position with OLD's last WAL position (after a clean stop the shutdown checkpoint of OLD is
the end of its WAL; its location can be read from the stopped data directory).

Map group (executed on 18.7): stop OLD (the map primary) fast, `pg_promote` NEW, then the text's step
4 "MAP: promote_node": MAP is the stopped node; run on another primary it is refused ("this node is
not a copy of the primary of group 0 and cannot become it"); on NEW it works (`promoted`,
`unreached [1]`, then `already`). Add "for the map group the call runs on NEW".

### 6. Re-adding an old primary as a standby

Verdict: right (executed for the map group's OLD after a clean stop, 18.7).

OLD started with `standby.signal` and `primary_conninfo` to NEW: `standby_status()` shows it
`streaming`, `async` until NEW's list names it; `catalog_status` on OLD: term 1, `superseded` false;
roles equal to the primary's; a read on OLD answers (hot standby), a write on it is refused
("cannot execute a write statement in a read-only transaction"). The `pg_rewind` branch (OLD that
was crashed or unreachable) was not run.

### 7. Restoring the preferred primary

Verdict: right (executed for the map group, 18.7): the old node, now a standby, promoted after a
fast stop of the interim primary; `promote_node(1)` on it: `promoted`, `"term": 2`, `unreached`
`[the stopped interim primary]`, then `already`; data written under both primaries present; nothing
prepared. For an ordinary group the suite runs it (`t/073`); I did not repeat it.

### 8. Removing a dead standby

Verdict: right when done in the written order; the wrong order is refused, slowly and with an
unnamed answer (S9-08).

Wrong order executed: standby stopped, list still names it, `remove_node` first: the call takes 125 s
(default `node_timeout` 60) and ends in KS005 "did not confirm that its part of the transaction is
durable", plus the warning about the rollback; repeated: the same. `remove_node` of a node that does
not exist: "node 99 does not exist"; of a primary that still has a standby: "cannot remove node 2:
shard group 1 still has the standby node 4" (named). While this happened a single-group write to the
group waited (PostgreSQL's wait) until the list was cleared and reloaded, then went through.

What to add: say that the removal of the name from the list comes first because the catalog
change itself commits on the group's primary, and name KS005 and its cost as the answer of step 2
in the wrong order.

### 9. A call that did not finish

Verdict: right for the cases named; **"until it answers `already`" has no exit when a node holds a
lock-holding prepared transaction of its own** (S9-04).

Executed: NEW killed while the call waited at `promote_before_change` (nothing changed, nothing left
prepared; repeat `promoted`, then `already`); three concurrent callers; five repeats in the S9-04
setting. The kills of MAP at every fault point are the suite's (`t/064`, passes on both majors); I
did not repeat them. A client that gives up (statement timeout, cancel) was not tried.

## New findings

### S9-01 (critical) A committed part is rolled back after the retention, with no record

Where: `node_conn.c` (`postcommit`: the unconfirmed `COMMIT PREPARED` of a participant is only a
warning), `recovery.c` (`origin_decision` finds no decision row, the same-timeline rule rolls back;
`purge_log` and `check_participant_parts` forget the decision after `decision_retention`), README
"The participant says whether its record is durable", table row 4: "after the retention only the
incident record (`participant_lost`) is left".

Steps (reproduced on 18.7 and 19rc2; three groups; standbys for groups 1 and 2, both the only
synchronous standby of their primary; `decision_retention = 8`):

```
-- session on node 1: SET kingres_shard.fault_injection = 'after_commit:wait:<file>';
--   BEGIN; INSERT INTO kv VALUES (<key of group 0>, 'v1'); INSERT INTO kv VALUES (<key of group 2>, 'v1'); COMMIT;
-- the origin waits between its commit and the second phase; stop the standby of group 2;
-- remove <file>; node 3 (group 2) now waits in COMMIT PREPARED for its standby (SyncRep);
-- cancel that backend: pg_cancel_backend(pid) where wait_event = 'SyncRep'
--   client: COMMIT, WARNING "node ... has committed its part ... but its standby has not confirmed the commit"
--   both rows are visible through the cluster; dtx_log on node 1 has the decision
-- wait 10 s (past the retention); SELECT kingres.recover_prepared_transactions() on node 1:
--   the decision row is purged (dtx_log: 0 rows)
-- stop node 3 immediate; start its standby (it has the PREPARE, not the COMMIT PREPARED); pg_promote;
-- on node 1: SELECT kingres.promote_node(<standby>); (twice: promoted, already)
SELECT id, val FROM kv WHERE val = 'v1';            -- 1 row (the group 0 row); the group 2 row is gone
SELECT count(*) FROM kingres.dtx_incident;          -- 0 (on node 1 and on the promoted node)
SELECT * FROM kingres.check_cluster();              -- only standby_unreachable
-- log of node 1: "kingres_shard recovery: rolled back prepared transaction ... on node 5"
```

Control, same script with `decision_retention = 300`: both rows present after the failover (the
prepared part is committed by recovery from the kept decision row), no incident needed.

Observed: the client was told COMMIT (with a warning), the transaction was committed on the origin,
and after the participant failed over to the standby that lacked the commit the part is gone and
nothing, anywhere, says so. The rollback is made by the origin's recovery as an ordinary
"prepared, no decision, origin gone" case; it is not on an earlier timeline, so no
`rolled_back_after_failover` incident is written either.

Expected: the README says the loss is recorded after the retention. Either the decision of a
participant that did not confirm is kept until the participant's standby confirms (or the
participant's group changes primary) instead of for a fixed time, or the purge of such a decision
writes an incident, or recovery refuses to roll back a prepared transaction of a live origin whose
decision row was purged by the retention rule and leaves it prepared with an alarm.

Note on the conditions: it needs an unconfirmed second phase (a participant standby down, slow or
cancelled) and a failover of that participant after `decision_retention` (300 s by default). The
same holds when the list is emptied during the wait (the documented weaker case, row 11): there the
incident `participant_lost` was recorded in my run when the failover came inside the retention and
was kept 30 days.

### S9-02 (high) The majority rule for the map group is bypassed by promoting an ordinary group first

Where: `failover.c` (`kingres_shard_require_map_group()` in the non-map branch; the electorate check
exists only in `map_case`; `reached_groups` counts the group promoted a moment before), README
guarantee 9 and "Failover of the map group".

A promoted map standby holds a physical copy of the old map primary's catalog and `local_node = 1`:
it passes `require_map_group()` and runs the ordinary-group promotion as if it were MAP, with no
electorate check.

Steps (reproduced, 18.7 and 19rc2): three groups, standbys for groups 0 and 1; stop the primaries of
groups 0 and 1; `pg_promote` both standbys; clear their lists; then on the promoted map standby:

```
SELECT kingres.promote_node(<map standby>);     -- refused (1 of 2 other groups reachable): the rule works
SELECT kingres.promote_node(<group 1 standby>); -- {"status": "promoted", "term": 1, "unreached": [2]}   (no electorate)
SELECT kingres.promote_node(<map standby>);     -- {"status": "promoted", "term": 2, "unreached": [1]}   (the group just promoted counts)
```

With the old map primary alive and the other nodes stopped (18.7): the promoted standby accepted
`promote_node(<group 1 standby>)` (term 1, `unreached [2, 3]`) while the old map primary kept
term 0 and its old roles, told nothing: two nodes that each hold the map group's catalog at
different terms.

Expected: the sentence "two sides of a partition cannot both promote the map group" holds.

Direction: the electorate check belongs to every promotion run by a node that is not yet the
catalog's recognised map primary (a node that is out of recovery but whose own promotion is not in
the catalog), or an ordinary-group promotion must be refused on a node that was promoted in
PostgreSQL after the catalog was last written (compare its timeline with the one recorded).

### S9-03 (high) An interrupted `rotate_secret()` cuts a node off, and recovery cannot repair it

Where: `node_admin.c` (`kingres_rotate_secret`: one two-phase transaction, the origin switches to the
new secret when it commits), `recovery.c` (it commits a prepared transaction over a node session),
`forward_auth.c` (a session is proved with the secret).

Steps (reproduced, 18.7 and 19rc2; three nodes):

```
-- node 1: SET kingres_shard.fault_injection = 'after_log:wait:<file>'; SELECT kingres.rotate_secret();
-- the rotation is prepared on nodes 2 and 3; stop node 2 immediate; remove <file>
--   WARNING: could not commit prepared transaction ... on node 2; recovery will commit it
-- start node 2
SELECT count(*) FROM kv;                      -- on node 2: FATAL: this session claims to have been opened by another
                                              --   node of the cluster but cannot prove it   (both directions)
SELECT kingres.recover_prepared_transactions();   -- on node 1: 0; log: "could not set up the session on node 2, skipped"
BEGIN; INSERT (group 0); INSERT (group 1); COMMIT;   -- FATAL, as above, and again after the recovery call
-- only by hand: COMMIT PREPARED '<the gid>' on node 2: then everything works and check_cluster() is empty
```

Observed: node 2 holds the old secret and the rotation prepared; node 1 holds the new one; no
session either way is accepted; the promise "recovery will commit it" cannot be kept because the
recovery needs the session it is refused. A node the cluster cannot talk to is also a node
`promote_node` cannot reach, and a node that must be failed over this way is in `unreached` for good.

Expected: a second phase that failed leaves the cluster able to finish it. Direction: recovery of a
prepared transaction whose gid is the rotation's must try the previous secret (the origin keeps it
until the rotation is known to be complete) or use a superuser control session; or the origin keeps
accepting both secrets until every node has committed; and the README should name the manual way out
until then.

### S9-04 (medium) "Repeat until `already`" has no exit when a node holds a foreign prepared transaction

Where: `ha.c`/install script (`internal_promotion_state`: `pending` counts every prepared
transaction that holds a relation lock on `kingres.map_version`), `failover.c`
(`promotion_complete` returns false when a primary has `pending > 0`).

Steps (reproduced, 18.7 and 19rc2): on a primary of another group, as an ordinary role that can read
the table: `BEGIN; LOCK TABLE kingres.map_version IN ACCESS SHARE MODE; PREPARE TRANSACTION 'x';`
(a plain `SELECT` also holds the lock). Fail over another group and call `promote_node` five times:

```
{"status": "promoted",  "final": false, ...}
{"status": "completed", "final": false, ...}     -- every repeat, with the notice "the promotion of node 4 is not complete; finishing it"
```

`recover_prepared_transactions()` does not touch the transaction (its name is not the extension's),
so the loop does not end; each repeat pushes the catalog again to every node.

Expected: `already` once the catalog is on every node. Direction: count only prepared transactions
whose gid is one of the cluster's (valid tag) or that are catalog changes of the call; and say in
the text how many repeats are reasonable.

### S9-05 (medium) "Failover when OLD is reachable" cannot succeed as written

See sequence 4. Where: `node_conn.c` (`prepare_one`: OLD is a participant of the call; its own
standby, NEW, left it), README sequence 4 and the planned-switchover step "Without the stop".
Reproduced on both majors: 21 s per call with `node_timeout` 8, every call, until OLD's list is
cleared; then `promoted`.

Direction: either the call detects that OLD's list names a node that is now a primary (it can read
`pg_stat_replication` of OLD and refuse at once, with the hint), or the text puts the step in.

### S9-06 (low) False `participant_lost_committed_transaction` rows from the promotion itself

Steps (both majors): the setting of S9-05 after OLD's list is cleared; call `promote_node`; call it
again (the repeat runs recovery); `check_cluster()` shows two `participant_lost_committed_transaction`
rows ("the transaction was committed by its origin, and this node ... has no trace of it") and
`dtx_incident` two rows, kept 30 days. The transactions are the call's own catalog changes, whose
marks are on OLD (reached and told) and not on NEW, which is the new primary of OLD's group.

Expected: no incident for a catalog change of a promotion whose old primary took part in it, and the
postcondition "`check_cluster()` shows nothing for the group" holds. Direction: skip the mark check
for transactions whose participant set contains the node being replaced.

### S9-07 (medium) The planned switchover does not check that nothing was lost

See sequence 5. Reproduced on 18.7 (script described there). Direction: after the stop of OLD the
sequence must say: read OLD's last WAL position from its stopped data directory and wait for (or
compare with) `pg_last_wal_receive_lsn()` on NEW, and refuse to promote when NEW is behind; the
precondition "or is caught up" must say how the caller knows. A cheap guard inside the extension is
impossible (OLD is stopped), which is why the text must carry it. The map-group variant must say that
the call runs on NEW.

### S9-08 (low) Wrong-order calls are refused slowly and with an answer the text does not name

See sequences 1 and 8. Direction: name the answers; consider a shorter bound for a catalog change that
only needs the group's primary to commit when that primary's standbys are known to be absent.

### S9-09 (low) `internal_sync_confirm` lets a claimed node session in outside a cluster

Where: `ha.c` (`kingres_internal_sync_confirm`, `kingres_shard_session_forwarded()`), README guarantee
16 and the earlier "Not reproduced" entry.

Steps (18.7): a standalone server with the extension created and no `init_cluster`; role `bob`
without privileges; `PGOPTIONS='-c kingres_shard.forwarded=on' psql -U bob -c "SELECT
kingres.internal_sync_confirm(0)"`: answer `true`. `internal_sync_probe` (not executable by PUBLIC)
and `internal_dtx_applied('x')` with a malformed name: refused at the grant, or by the function's own
argument check after the claim passed. On a cluster node the same claim ends with `FATAL ... cannot
prove it` (also reproduced).

Impact: a role can call a function that waits up to the argument it passes for the node's standbys,
on a node that has not joined a cluster: a held backend, no data. Direction: when `local_node` is
absent, treat the claim as unproven.

### S9-10 (note) The operator's way out of a refused map promotion

With two groups, or when the groups lost together with the map group are half or more of the others,
`promote_node` refuses for ever ("only 0 of the 1 other shard groups can be reached"; five groups: a
tie of 2 of 4, and 1 of 4, also refuse). The text says the control plane decides to bring nodes back.
If the other group is the one that is down for good, nothing in the extension lets the cluster go on,
and `init_cluster` is refused ("already initialized"). The README should say what a human does
(for example: restore the other group from its backup or standby first, or a documented override
that takes a term and a fencing token as its argument).

## What held under attack

- **`internal_sync_confirm` and the other helpers against a plain role** (on a cluster node, both
  majors): `internal_sync_confirm`, `internal_sync_probe`, `internal_dtx_applied`,
  `internal_dtx_identity` refused to a role (`permission denied for function`); a `forwarded=on`
  claim at connect time ends with FATAL "cannot prove it"; the public functions are the 13 listed
  before plus `internal_sync_confirm`.
- **The wait after the cancel of the participant's PREPARE**: the origin aborts with KS005, nothing is
  committed, nothing is left prepared (both majors).
- **`synchronous_commit` of the user, role or database**: `off` and `local` in the origin's session
  with its standby down: the commit waits as PostgreSQL's does (the extension raises the setting);
  a cancel gives the PostgreSQL warning and the extension's "second phase was not sent"; the
  participants stay prepared and recovery commits them after the standby is back (rows present
  once each). A database setting `off` and a role setting `local` on the participant: raised, KS005
  when its standby is down.
- **Emptied list during the wait** (documented): the commit completes, the participant's failover loses
  its part, `participant_lost` is recorded and kept (the weaker case is in the README, row 11).
- **The probe**: a list that nobody serves is refused in about 4 s, nothing changed; callers at the
  same moment serialise.
- **The electorate** with five groups for every split of a lost set: ties and minorities refused,
  a strict majority promotes.
- **Rotation**: refused while a tagged transaction is in doubt; refused up front when a node is
  down; sessions opened before the rotation keep working after it; new transactions are tagged with
  the new secret.
- **A planned switchover under a distributed-write load**, origin on MAP and origin on OLD, both
  majors: no loss, no half transaction, no leftover.
- **Re-adding and failing back** the old map primary: term, roles and data right.

## Not reproduced

- **A false positive of the lost-slot rule of a bucket move.** By reading: the subscription is made
  with `synchronous_commit = 'on'`, and the table-sync worker drops its slot on the source before the
  local commit that moves the table from `f` to `s`; that commit waits for the target group's standby,
  and while it waits the target still shows `f` and the source has no slot. A target standby that
  stays unresponsive for more than `max(10 s, 2 x node_timeout)` in exactly that window would make a
  healthy move fail (and be undone) with a hint that blames a stopped target. The window is narrow
  (the state `f` to `s` runs in milliseconds, and the copy's own commits need the same standby); I
  could not make it deterministic. Whether the undo of such a move always completes was not run
  (`t/074` and `t/028` cover the cases of a lost slot).
- **The cancel that does not arrive** (S8-08). My attempt (new connections refused by `pg_hba`) did
  not stop it, because a cancel request is handled by the postmaster before authentication. By
  reading: after the second `node_timeout` the origin gives up on the connection with `prepared` false
  and sends no `ROLLBACK PREPARED`; the transaction stays prepared on the participant (its PREPARE
  returns when the standby is back) and recovery rolls it back (no decision row). I did not run it.
- **Slot created late (122 s).** Not run.

## Not covered

- An upgrade from 0.4 with transactions prepared across it: the 0.5 library cannot make 0.4's
  identifiers, so a real test needs a 0.4 build; `t/051` and `t/067` fake the names. Not run.
- Cursors and `COPY` across a promotion (`t/069`, passes, not varied).
- `pg_rewind` of an old primary that was crashed or unreachable (sequence 6, second branch).
- Kills of MAP at the fault points of `promote_node`, a second caller during `add_node`, and a client
  that cancels `promote_node` (suite only).
- A real network partition (packet filter); stopping a node is not the same as a node that serves
  the other side.
- The restore points, the distributed deadlock detector, the web panel and the control plane.
- The move loop beyond the one reading above; a move undone after a lost slot.
- A `synchronous_commit` of `remote_apply` at the origin of the second phase, and quorum lists with
  `ANY 2` or `FIRST` (only `ANY 1` was run).
- Hot-standby reads during a promotion.
