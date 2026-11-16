<!-- SPDX-License-Identifier: AGPL-3.0-only -->
# Adversarial review: contrib/kingres_shard 0.5, the two fix rounds (S6 and S7)

Reviewer: an independent reviewer (not the author of the code, not the reviewer of the
earlier rounds). Findings of this review are numbered S8-nn (the findings of the review of
the port were S6-nn, of the first review of the high-availability part S7-nn).

Reviewed states: commit f47ce21bf5 (the fixes for S7-01 to S7-10) and commit 0ec398f6a2
(the fixes for S6-01 to S6-10, never reviewed), both exported with `git archive` of
`contrib/kingres_shard`, `contrib/kingres_ledger`, `kingres/dev` and `kingres/docs` into a
private area. The working tree was not used for any result.

## Scope and method

Read: the review of 0.5 (`2026-11-15-kingres-shard-0.5.md`) with its reproductions and its
"not covered" lists; the summary of the review of the port; both commit messages; the README
chapter "High availability" (the sixteen guarantees and the five call sequences); `src/ha.c`,
`src/term.c`, `src/failover.c`, `src/forward_auth.c` in full; the diff of `src/recovery.c`
between the two commits; `postcommit`, `log_decision`, `precommit`, `prepare_one` and
`kingres_internal_dtx_applied` in `src/node_conn.c`; `internal_apply_catalog`,
`internal_cancel_waiter`, the wait-edge functions and the new tables in the 0.5 install
script; `t/061` and the helper module. I did not read `bucket_move.c`, `planner.c`,
`dist_scan.c` or `fanout.c` line by line, and I did not read the design document
(`ha-phase5.md`) beyond what the README repeats.

Built and ran, on private copies of the PostgreSQL installations with assertions (18.7 and
19rc2; the copies were relocated, `pg_config` reports paths in my area; nothing was written
into the originals): the extension out of tree with `kingres/dev/ext.sh`, `EXT_WERROR=1`,
`-j2`, under `nice`. One baseline run of the whole suite on each major, started together.
Then experiments of my own as TAP files (clusters of two to four nodes with real streaming
standbys, built with the helpers of `t/KingresShardTest.pm`), ports 59500 to 59699 (I
patched the port range in my copies of the Perl test module; the two baseline runs used the
framework's own range), every server stopped afterwards. Each experiment below was run on
18.7, and the ones marked "both majors" also on 19rc2 with the same result. The machine was
shared; where timing mattered I made it deterministic (a stopped standby, a cancel of a
backend found by its wait event, a fault point that waits on a file, `recovery_min_apply_delay`).

"Reproduced" means I observed it. "By reading only" means I did not run it.

## Baseline runs

| Major | Regression tests | TAP files | TAP tests | Wall time | Result |
|---|---|---|---|---|---|
| 18.7 (assertions) | 12 of 12 | 69 | 6828 | 885 s | all passed, no compiler warning with -Werror |
| 19rc2 (assertions) | 12 of 12 | 69 | 7486 | 880 s | all passed, no compiler warning with -Werror |

The commit message says 6858 and 7516 tests; I count 30 fewer on each major (the files and
the result are the same; I did not look for the difference, as in the previous round).

`t/028_move_faults.pl` on 19rc2: see "Not reproduced" below (the hang was not reproduced).

## Summary

New findings: 0 critical, 1 high, 3 medium, 2 low, 2 notes.

| ID | Severity | Summary |
|---|---|---|
| S8-01 | high | The S7-01 check protects only the origin: a participant whose wait for its own standby is cancelled answers PREPARE (and COMMIT PREPARED) as done; after a failover of the participant its part of a committed transaction is gone. Recorded (`participant_lost`) only within the retention time |
| S8-02 | medium | `promote_node` answers `"status": "promoted"` when its own commit was deferred (the standby did not confirm); the other nodes are left prepared and on the old catalog. The README sequences never mention `synchronous_standby_names` of the promoted node, which a standby inherits |
| S8-03 | medium | `rotate_secret()` with a distributed transaction in doubt leaves its prepared transactions "foreign" for ever: recovery ignores them, they hold locks, `check_cluster()` says they were not made by this cluster |
| S8-04 | medium | The majority rule for promoting the map group counts group 0 on both sides of a partition: with three groups both sides can hold a "majority" (2 of 3); the README says a minority cannot take the map group |
| S8-05 | low | The answer of a repeated `promote_node` (`completed`) has `"unreached": []` although the old primary was never told; the list is only filled by the first call |
| S8-06 | low | `kingres.dtx_applied`: a row per participant per distributed commit, kept one day whatever `decision_retention` says, cleaned by a sequential scan on every recovery pass (no index on `applied_at`) |
| S8-07 | note | What happens to writes on an old primary that `promote_node` did not reach (established, see below): acknowledged, then gone, no record; `check_cluster()` flags the node, not the writes |
| S8-08 | note | A participant whose standby is down stalls every multi-node commit that touches it, with no timeout (not `node_timeout`, not `statement_timeout`); the natural remedy (cancel the stalled participant backend) is the trigger of S8-01 |

No committed row lost or doubled without a record, no wrong answer, no server crash and no
escalation of privilege was found. S8-01 is "high" and not "critical" because the loss is
recorded in `kingres.dtx_incident` when the origin's recovery pass runs within the retention
time (reproduced); it is a transaction that is committed on one node and gone on another,
with a record only by luck of timing.

## Fate of the findings of the review of 0.5 (S7)

| ID | Fate | Evidence |
|---|---|---|
| S7-01 | fixed for the origin; **the same hole remains on the participant (S8-01)** | `t/061` passes on both majors. Mine: remote_apply with the apply delayed by 60 s and the wait cancelled: second phase deferred, participant stays prepared. The sync list emptied during the wait releases the commit, participants commit (documented weaker case, no incident: the README says so). Smart shutdown during the wait (18.7): the origin stays up, the participant stays prepared; I then started the stopped standby and the commit did not complete in the two minutes I allowed (I take it that the postmaster, shutting down, refuses the standby's connection; I did not confirm that in its log). `t/061` does not cover this |
| S7-02 | fixed | Forged `application_name` = `ks-dtx:1:99999:1000` on two sessions (the steps of the old review, detector at one second): the innocent transaction is not cancelled (both majors) |
| S7-03 | fixed | Forged `ks-dtx:1:1:abc` label: 0 detector errors in the log of the map node (both majors) |
| S7-04 | fixed | Two groups stopped together, both standbys promoted, three `promote_node` calls at once: terms 1 and 2, third answers `already`, `unreached` lists the dead nodes. Map group with the old primary and one group down: promoted with `unreached [1, 2]` (this is also S8-04) |
| S7-05 | fixed | Kill at `after_prepare`, `after_first_prepare`, `after_commit`, `between_commit_prepared` of the map primary during `promote_node`, restart, repeat the call: `promoted`, `promoted`, `completed`, `completed`; nothing left prepared, all four nodes agree on roles and term, `check_cluster()` shows only the stopped old primary. Defect in the answer: S8-05 |
| S7-06 | fixed in the connected case | Old map primary restarted unfenced after the new one was promoted: its `create_distributed_table`, `sync_catalog`, `ALTER TABLE` (DDL), `add_node`, `promote_node` and distributed scan are all refused (KS002 or "superseded"), `catalog_status.superseded` is true, no other node changed. A node two terms behind learns on the first statement (the first scan fails with a retryable error, the second works and the node forwards). Not fixed for a partition: S8-04 |
| S7-07 | partly | `t/062` passes. The forcing of `synchronous_commit` works, but a cancelled wait on the participant still reports success (S8-01) |
| S7-08 | fixed | `create_restore_point` with `statement_timeout = 2s` while a commit is between prepare and decision: both barriers inactive afterwards; the same with `pg_terminate_backend` of the caller; the next restore point works |
| S7-09 | fixed | A name with a bad tag is reported as `foreign_prepared_transaction`, never touched, never an incident (reproduced as a side effect of S8-03). The cost of the rule: S8-03 |
| S7-10 | fixed by reading and `t/065` | Not attacked by me beyond reading; see "Not reproduced" for the race that remains in the 0.1 second window |

## Fate of the findings of the review of the port (S6)

| ID | Fate | Evidence |
|---|---|---|
| S6-01 | fixed | `t/056` passes on both majors (12 sections). Mine, both majors: foreign key from a distributed table to a reference table, `ON DELETE CASCADE` of one reference row removes the rows of every group, `TRUNCATE ... CASCADE` empties the reference table and the child on every node, copies equal |
| S6-02 | fixed | Both majors: 60 caught errors in a PL/pgSQL `EXCEPTION` block around a distributed scan, a cursor with `ROLLBACK TO SAVEPOINT`, an error in a scan then a write, a timeout in the middle of a scan: no crash, later statements work |
| S6-03 | fixed | by `t/058`; not attacked by me |
| S6-04 | fixed | by `t/058`; not attacked by me |
| S6-05 | accepted, documented | README line on stored generated columns (the extension trusts the IMMUTABLE declaration); the commit message does not list it |
| S6-06 to S6-10 | fixed | `t/058`, `t/059`, `t/046`, the `upgrade` regression test pass on both majors. Extensions created and dropped in both orders (`kingres_shard`, `kingres_ledger`) on both majors: no error. The schema `kingres` is left behind (empty) after both are dropped; a manual `DROP SCHEMA` is needed to get rid of it; I take this as intended |

Both majors behaved the same in every experiment of this review that was run on both
(each is marked "both majors").

## The sixteen guarantees of the README

| # | Verdict | Evidence |
|---|---|---|
| 1 | holds (by the suite) | `t/048`, `t/054`; the standby of my clusters was skipped by routing, scans and writes |
| 2 | holds | an unreachable standby appears as `standby_unreachable`, the primaries are unaffected (reproduced) |
| 3 | not tested by me | suite only |
| 4 | holds for unreachable and not in recovery | `check_cluster()` showed `standby_unreachable` and `standby_not_in_recovery` (the old primary restarted unfenced); not streaming: suite only |
| 5 | holds | an old primary that was reached forwards; one that was not reached and learns later forwards afterwards (reproduced); the old map primary refuses scans and writes of distributed tables |
| 6 | holds as stated | and see S8-07 for exactly what it means |
| 7 | holds with two defects | resumable at four fault points (S7-05 row), concurrent calls serialize; answer defects S8-02, S8-05 |
| 8 | not tested by me | suite only (`t/049`, `t/054`, `t/069`) |
| 9 | **does not hold as written** | "a minority can therefore not take the map group": S8-04. The term, the learning of a stale node and the refusal of an old map primary hold |
| 10 | holds | forged tag left alone and reported; a tag made while the cluster has no secret is sixteen zeros (by reading only); a rotation breaks it in practice (S8-03) |
| 11 | **does not hold for a participant** | S8-01. For the origin it holds (cancel, terminate, fast shutdown by `t/061`; remote_apply by me) |
| 12 | holds | `participant_lost` recorded in the experiment of S8-01 (within the retention); beyond the retention: by design, not tested |
| 13 | holds against the labels | forged labels do nothing (S7-02, S7-03 rows); false positives and negatives of the detector itself: not attacked |
| 14 | holds by the suite | not independently tested |
| 15 | holds | cancel by `statement_timeout`, termination of the caller, both barriers down at once; a crash of each party: not tested by me |
| 16 | holds for an ordinary role | the functions executable by PUBLIC are the 13 that the old review listed plus `internal_dtx_applied`; an ordinary role is refused on `internal_dtx_applied`, on `kingres.dtx_applied` and on `kingres.catalog_status` (the view is for superusers). The claim "refuses everyone but a node session": by reading, on a node that is not yet part of a cluster the check passes for a session that merely claims to be forwarded (`local_node` is 0); I did not run it |

## What the control plane calls: verdicts on the sequences

| Sequence | Verdict |
|---|---|
| Registering a standby | right, incomplete. Missing: the name in `synchronous_standby_names` is the standby's `application_name` (the README says `<standby>` without saying what it is, and the registration does not check it); the order (registering before or after the name is added changes whether a commit can block); removing a standby that is still named in the list blocks commits |
| Failover of an ordinary group | **incomplete, and one answer can mislead.** (1) With `ANY 1 (a, b)` only the standby with the highest flushed position is safe to promote; the sequence does not say how to choose, and nothing in the extension checks it. (2) The promoted node inherits `synchronous_standby_names` from the old primary's configuration: with a name that no one serves, every commit on it blocks (PostgreSQL's behaviour), recovery cannot commit a decision, and `promote_node` of the map group hangs (S8-02); the sequence must say to set or clear the list on the promoted node before step 1 or right after. (3) `"promoted"` does not mean the catalog change is committed on every node (S8-02): the control plane must check `catalog_status` and `pg_prepared_xacts` of the nodes, or read the warnings. (4) After the failover the other standbys of the group must be re-pointed and re-listed. (5) `"unreached"` of a repeated call is not complete (S8-05) |
| Failover of the map group | **wrong in one sentence, incomplete in another.** "A minority can therefore not take the map group": false when group 0 is counted on both sides (S8-04); the control plane must itself prove that the old map primary is fenced. The sync-list point of S8-02 applies with more force: reproduced here (the call hangs until cancelled, then reports `promoted`, two nodes keep a prepared catalog change and the old catalog until the list is cleared and recovery runs). The rest (call it on the promoted node, `sync_catalog`, the old primary refuses changes and shows `superseded`) held |
| A promotion that stopped half way | right. Four kill points converge with one repeated call; the answers (`promoted` or `completed`) are as the README says. The answer's `unreached` is wrong (S8-05) |
| Re-adding an old primary | right. Reproduced: old primary stopped, standby promoted, `promote_node`, old primary rebuilt with `pg_basebackup` on its old address as a standby: `standby_status()` says `streaming`, `catalog_status` on it shows the new term and `superseded = false`, `check_cluster()` is empty, and a failback (stop the new primary, promote the rebuilt node, `promote_node` of its id) works with term 2 and a write afterwards. The rebuilt node holds the other node's `local_node` until it is promoted again, which `promote_node` sets; harmless |

## New findings

### S8-01 (high) The participant's wait for its own standby can end early, and PREPARE says done

Where: `node_conn.c` (`prepare_one`: the participant raises `synchronous_commit` and runs
`PREPARE TRANSACTION`; the origin only checks its own commit record, `postcommit`).

The README says the participant's `PREPARE` is acknowledged by its standby before the origin
decides (guarantee 11). PostgreSQL ends the wait of `PREPARE TRANSACTION` and of `COMMIT
PREPARED` for a cancel (`pg_cancel_backend`, an administrator) with a WARNING and a successful
result, exactly as it does for a commit (the situation S7-01 was about). The origin cannot see
the warning: the participant answers "PREPARE TRANSACTION" as done. The check of S7-01 is made
on the origin only.

Steps (reproduced on 18.7 and 19rc2; node 1 map group primary, node 2 primary of group 1 with
a standby that is the only synchronous one):

```
-- stop the standby of node 2
-- session A on node 1: SET kingres_shard.node_timeout = 20; BEGIN;
--     INSERT INTO public.kv VALUES (<key of group 0>, 'a');
--     INSERT INTO public.kv VALUES (<key of group 1>, 'b'); COMMIT;
-- node 2: wait_event = 'SyncRep' (the participant's PREPARE waits)
-- node 2: SELECT pg_cancel_backend(pid) FROM pg_stat_activity WHERE wait_event = 'SyncRep';
-- the origin goes on and commits; COMMIT PREPARED reaches node 2, which waits again
-- node 2: the same cancel again; session A returns COMMIT with no warning and no error
-- node 2: stop immediate; start its standby; pg_promote(); on node 1:
SELECT kingres.promote_node(<standby>);          -- {"status": "promoted", "unreached": [2], ...}
SELECT kingres.recover_prepared_transactions();  -- on node 1 and on the promoted node
SELECT count(*) FROM public.kv WHERE id = <key of group 0>;   -- 1
SELECT count(*) FROM public.kv WHERE id = <key of group 1>;   -- 0
SELECT kind, gid FROM kingres.dtx_incident;                   -- participant_lost ...
```

Observed: the origin's part is committed, the participant's part is gone; the origin's
recovery pass found it (`participant_lost` in `dtx_incident`, `check_cluster()` shows
`participant_lost_committed_transaction`) because the pass ran inside the retention time. The
client was told `COMMIT` with no warning of any kind.

Expected: the guarantee says "atomic ... across a failover of a participant's group" with a
synchronous standby. A cancelled wait on the participant must make the origin abort, or defer.

Why it matters beyond a mistaken cancel: S8-08. A participant whose standby is down stalls
the commit with no timeout; the only ways out are to cancel the participant's backend
(this finding) or to cancel the origin (safe).

Direction: after `PREPARE TRANSACTION` (and after `COMMIT PREPARED`, which cannot be undone,
so there only the record matters) ask the participant the question the origin asks itself:
a function `internal_sync_confirmed()` that returns whether the position flushed so far is
confirmed as the configuration requires (`kingres_shard_sync_confirmed`), run in the same
session; if it says no, the origin treats the PREPARE as failed and rolls back. For
`COMMIT PREPARED` a "no" is only a reason to keep the decision row longer.

### S8-02 (medium) `promote_node` says "promoted" when its commit was deferred; the sequences leave out the sync list

Where: `failover.c` (the answer is built after the transaction commits; the deferred second
phase of S7-01 only emits a WARNING), README "What the control plane calls".

Steps (reproduced on 18.7 and 19rc2; nodes 1 (map), 2, 3; node 1 has a standby registered as
a node of group 0 and listed as its synchronous standby; the standby has the same
`synchronous_standby_names = 'ANY 1 (<its own name>)'`, which is what a copy of the
configuration of the primary gives):

```
-- node 1: stop immediate; pg_promote() its standby
-- on the standby: SELECT kingres.promote_node(<its node id>);   -- does not return
-- (wait_event SyncRep on the promoted node; it is its own commit waiting for a standby
--  that does not exist); cancel it:
--   WARNING: canceling wait for synchronous replication ...
--   WARNING: the commit of this distributed transaction was not confirmed ...
--   {"term": 1, "status": "promoted", "unreached": [1]}
-- nodes 2 and 3: one prepared transaction each; catalog still term 0, naming node 1;
-- on the promoted node: recover_prepared_transactions() -> 0 (the decision is not
--   confirmed: no standby); after the list is cleared and reloaded: -> 2
```

Observed: the answer is `promoted`, an ordinary success, while nothing is committed on
the other two nodes until the list is changed and recovery runs; the warning is on the
session only. A control plane that reads the jsonb believes the promotion is done.

Expected: the answer says what is true (`"status": "deferred"`, or `"completed": false`), or
`promote_node` waits for the second phase, or raises an error that says "repeat the call".
The README sequences must say that the promoted node needs a correct `synchronous_standby_names`
(empty or naming standbys that exist) before the call, and that the call itself needs it.

Direction: after the commit, compare the number of prepared catalog changes the call left
(it already has `promotion_complete`) and answer from that; add the sync-list step to both
failover sequences.

### S8-03 (medium) `rotate_secret()` strands transactions that are in doubt

Where: `forward_auth.c` (`kingres_shard_gid_tag`), `recovery.c` (`GID_FORGED`),
`kingres.rotate_secret()`; README: "rotate when no distributed transaction is prepared".

Steps (reproduced on 18.7; two nodes):

```
-- node 1: SET kingres_shard.fault_injection = 'after_prepare:kill';
--         BEGIN; <a write on each node>; COMMIT;           -- the origin dies after PREPARE
-- node 1: restart; node 2: pg_prepared_xacts has 1 row, name ...<tag>
-- node 1: SELECT kingres.rotate_secret();
-- node 1: SELECT kingres.recover_prepared_transactions();   -- 0
-- node 2: the prepared transaction is still there, with its locks
-- node 1: SELECT check_name, detail FROM kingres.check_cluster();
--   foreign_prepared_transaction: a prepared transaction with the name of one of ours that does
--   not carry a valid tag; it was not made by a node of this cluster and recovery leaves it ...
```

Observed: the transaction is the cluster's own, and in doubt (no decision, so recovery would
roll it back); after the rotation it is never resolved and the alarm says it is not ours.
The only exit is `ROLLBACK PREPARED` by hand on the participant, with the knowledge of
`dtx_log`. Nothing stops the rotation: `rotate_secret()` does not look at `pg_prepared_xacts`.

Expected: either the rotation refuses while a transaction named `kingres_h...` is prepared on
any node, or the nodes keep the previous secret and verify a tag against both for a while
(until `pg_prepared_xacts` has no name with the old tag), or the alarm says "made before the
last rotation".

### S8-04 (medium) The majority rule counts group 0 on both sides of a partition

Where: `failover.c` (`reached_groups` starts at 1, "itself counting for group 0"); README,
guarantee 9 and "Failover of the map group": "A minority can therefore not take the map
group".

Three groups (0, 1, 2). A partition puts the old map primary and group 1 on one side and the
promoted standby of group 0 and group 2 on the other. The side of the standby reaches group 0
(itself) and group 2: 2 of 3, "a majority"; the side of the old map primary has groups 0 and
1: also 2 of 3, and it never needed a promotion. Both sides hold "a majority of the groups"
because group 0 is counted on both.

Reproduced on 18.7 (three groups with a standby for group 0; nodes 1 and 2 stopped, the
standby promoted, `promote_node` on it):

```
{"term": 1, "status": "promoted", "unreached": [1, 2]}
```

The call proceeded on one group out of the two others. In a real partition the old side
continues with its own catalog at term 0; the term refuses nothing until the two sides meet,
and meanwhile both accept writes for group 0's data.

This does not replace fencing, which the README assigns to the control plane, but the
sentence in the README promises more than the rule delivers, and a control plane that
trusts it will not fence.

Direction: the promoted node proves a majority among the groups other than the one it replaces
(here 1 of 2 others: refused), or count the old primary's group only when the old primary
is positively known to be down (the control plane's fencing token), and say so in the README.

### S8-05 (low) `unreached` is not filled by a repeated call

Where: `failover.c` (`promotion_complete` and the resume path only visit nodes that the
catalog calls primary, and the target).

Reproduced on 18.7: kill the map primary at `after_commit` (or `between_commit_prepared`)
during `promote_node` of an ordinary group whose old primary is stopped; restart; call again:
`{"status": "completed", "unreached": []}`. The first call, had it finished, would have said
`[2]`. The README says `"unreached"` lists the nodes that were not told, and the control
plane's sequence uses it to decide on `sync_catalog()` and fencing. After an interrupted
promotion the list is empty although the old primary has never been told.

Direction: iterate over every active node (old primaries included) in the repeated call, as
the first call does.

### S8-06 (low) `dtx_applied`: cost and cleanup

Where: `kingres_shard--0.5.sql` (the table has only the primary key on `gid`),
`recovery.c` (`DELETE ... WHERE applied_at < now() - 1 day`), `node_conn.c` (one insert per
participant per distributed commit, on the commit path).

`EXPLAIN` of the cleanup, with 200 000 rows: `Seq Scan on dtx_applied`, filter on `applied_at`
(reproduced). At 1000 distributed commits per second and two participants each, that is
about 170 million rows a day across the cluster (arithmetic, not measured) and a scan of
every row of a node on every recovery pass of that node; the
retention that the design needs is `decision_retention` (300 s by default), not a day. The
rows are WAL-logged and replicated. The measured 0.2 ms per commit is, by reading, the sum
of this insert and the participant-side forcing of `synchronous_commit`; I did not split it
by cause.

Direction: an index on `applied_at` (or partition by day), a retention that follows
`decision_retention` (plus a margin), and the same for the rows of `dtx_log` that are kept.

### S8-07 (note) An old primary that was not reached keeps taking writes

What the README leaves to fencing, established (reproduced on 18.7, three nodes, standbys for
two groups; both old primaries stopped, both standbys promoted, `promote_node` for both
from the map primary, `unreached` = the old primaries; the old primary of group 1 restarted):

- A single-group write on the old primary (key of its own group) succeeds, with no error and
  no warning, and is visible on that node only. The new primary does not have it. It is
  acknowledged and gone, with no record anywhere; `check_cluster()` on the map primary flags
  the node as `standby_not_in_recovery` ("registered as a standby and is not in recovery"),
  which is the only trace, and it names the node, not the writes.
- A distributed statement through that node fails (its claim of term 0 is refused by every
  node that knows term 2, the node learns and the next statement works); the first scan went
  to a stopped node and failed with a connection error, not with the term error.
- After the node has learned, it forwards (a write through it ended on the new primary), and
  the rows it took before are not visible through any node, itself included (they sit in its
  own storage until it is rebuilt).
- Rows are therefore visible "from both nodes" only in the sense that each node answers for
  its own copy before the stale one learns; through the cluster (any other node) only the new
  primary's rows exist.

### S8-08 (note) A stalled participant PREPARE has no timeout

An origin transaction whose participant waits for a synchronous standby that is down
(reproduced, 18.7): the origin's `COMMIT` was still waiting after 2 minutes 47 seconds with
`kingres_shard.node_timeout = 20`, and the participant database had `statement_timeout = 2s`
(which does not apply: PostgreSQL ends the statement timer before the commit part of a
`PREPARE`). The origin holds its row locks and its place in the barrier count all that time.
This is PostgreSQL's behaviour for commits, but here it hits a different node than the one the
client is connected to, and the only remedy is to cancel (S8-01) or terminate. A smart
shutdown of the origin in this state did not complete in my run either (see the S7-01 row).

## What held under attack

- **The origin's durability check.** Cancel, terminate and fast shutdown (`t/061`), and
  remote_apply with the apply delayed (mine): second phase deferred, participants prepared,
  recovery commits after the standby has confirmed. The check follows `syncrep.c`: the same
  `lsn[mode]` test under `SyncRepLock`, the same fallback for the status bits, the mode taken
  from the session's setting after it is raised. The case "list emptied during the wait"
  releases the commit as PostgreSQL does and the README says so.
- **The catalog term cannot be forged by a plain client** (18.7). As an ordinary role: `SET`,
  `set_config` and `ALTER ROLE ... SET` of `forward_term`, and `SET` of `forward_origin` and
  `forward_dtx`, all fail with "can only be set by a session opened by another node";
  `kingres_shard.forwarded` cannot be set after connection; with
  `PGOPTIONS='-c kingres_shard.forwarded=on'` the first statement that touches the setting
  ends the session with "cannot prove it" (FATAL), including an `ALTER ROLE ... SET`, and
  nothing is stored in `pg_db_role_setting`; the same role's later distributed scan and
  two-phase write worked and no node changed its term. A function with a `SET` clause,
  `RESET ALL` and `DISCARD ALL`: by reading only (the settings are `GUC_NO_RESET_ALL` and the
  check hook refuses any non-empty value outside a node session).
- **An old map primary** is refused everywhere (see S7-06 row); it stays superseded across
  restarts (the flag is in the catalog).
- **Promote_node under concurrency**: three calls at once for two groups serialize; the same
  node twice gives `promoted` and `already`.
- **A node two terms behind** learns by itself on its first statements.
- **Re-adding and failing back** an old primary (sequence table above).
- **Both majors** gave the same result in every experiment that was run on both
  (S8-01, S8-02, the extension creation orders, the foreign-key and scan-error variants).
- **The name length**: the longest name is 105 bytes by my count (`KS_GID_LEN` 160, PostgreSQL's
  limit 200); no truncation path.

## Not reproduced

- **The hang of `t/028_move_faults.pl` on 19rc2.** 28 runs of the file on 19rc2 (10 one at a
  time, then 18 in three parallel loops with six busy-loop processes on the machine, about
  90 s each) all passed; no hang. What I could determine by reading: the subscription of a move is created
  with `create_slot = false` on a slot made earlier, and the table-sync slots
  (`pg_<suboid>_sync_...`) are made by the target's workers on the source; the cleanup
  `internal_drop_move_publication` drops them by name prefix. A target that restarts in the
  middle of the copy while the cleanup of the move has already run on the source leaves a
  subscription whose table-sync worker asks for a slot that no longer exists, and
  `DROP SUBSCRIPTION` then has to stop that worker; the fallback
  (`DISABLE; SET (slot_name = NONE); DROP`) exists. This is a hypothesis; I could not make
  the order of events deterministic.
- **Soft edges of the deadlock detector.** A cycle through a queue edge that PostgreSQL would
  reorder away: no stable construction found.
- **The remaining window of S7-10.** The detector removes an unconsumed mark after 0.1 second;
  a victim that handles the cancel later than that gets 57014 instead of 40P01 (a lost
  rewrite, not a wrong one). By reading only.
- **`internal_dtx_applied` on a node outside a cluster**: by reading, a session that claims
  `forwarded` passes (`local_node` is 0). Impact: a row in a table that nothing reads.
- **Hostile feed to the detector**: only nodes that the catalog calls primary are asked; a
  node that holds the secret can do more than that.
- **Pid reuse and the crash restart of the shared memory**: by reading, the slots are checked
  against the pid and the memory is cleared at start; not run.

## Not covered

- A restore from backup or `pg_rewind` of a node and what terms it comes back with: reasoned
  (a lower term is refused and learns), not run.
- The loss with an asynchronous standby after the retention time: by design, not run beyond
  confirming that the rows are kept for `decision_retention` and purged after.
- An upgrade from 0.4 with transactions prepared across it (`t/051`, `t/067` pass).
- The restore of three servers under load (`t/053` passes), a crash of each party of a
  restore point, the barrier under the shared-memory restart.
- Cursors, rescans and `COPY` across a promotion (`t/069` passes on both majors, not varied).
- The measured 0.2 ms: not split by cause.
- Standbys: a standby of the wrong group, a lagging one; hot-standby reads.
- The control plane (`kingresd`), the in-tree build, the web panel, `bucket_move.c` beyond
  the one cause hypothesis above.
- A distributed deadlock detector's false positives and false negatives beyond the forged
  labels.
