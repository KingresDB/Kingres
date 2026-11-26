<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
# Adversarial review: contrib/kingres_shard 0.5, the fix round for the call sequences (S9-01 to S9-10)

Reviewer: an independent reviewer (not the author of the code, not the reviewer of the earlier
rounds). Findings of this review are numbered S10-nn (the previous review,
`2026-11-16-kingres-shard-0.5-sequences.md`, used S9-nn).

## Scope and method

Reviewed state: the last commit that touches `contrib/kingres_shard` (the fix round that answers
S9-01 to S9-10), exported with `git archive` of `contrib/kingres_shard`, `contrib/kingres_ledger`,
`kingres/dev` and `kingres/docs` into a private area. The working tree was not used for any
result. Nothing in the repository was changed except this file.

Read first: `2026-11-16-kingres-shard-0.5-sequences.md` (findings, "Not reproduced", "Not covered").
Read in full: the chapter "What the control plane calls" of the extension's README at that
commit, and the guarantees on prepared transactions. Read in the code: `recovery.c` (the whole
pass, `purge_log`, `participant_confirmed`, `check_participant_parts`, `origin_decision`,
`parse_gid`), `prepare_one`/`prepare_collect` and the second phase in `node_conn.c`,
`promotion_complete` and the participant loop of `failover.c`, `kingres_shard_require_map_group`,
`kingres_shard_is_recognized_map_primary`, `require_primaries_can_commit` and `rotate_secret` in
`node_admin.c`, and the 0.5 install script for every function that changes a catalog, the
switchover functions, `standby_name_matches` and the privileges of the internal functions. I did not
read the planner, the scan code or the bucket move code beyond the move's switch.

Environment: private copies of the PostgreSQL installations with assertions (18.7 and 19rc2),
relocated (the `pgxs` makefile prefix fixed in my copies, nothing written into the originals); the
extension built out of tree with `kingres/dev/ext-majors.sh`, `EXT_WERROR=1`, `-j4`, under `nice`;
the port range of the Perl cluster module patched in my copies to 59800-59999, every server stopped
afterwards. Baseline: the whole suite once per major. Then my own experiments as TAP files (clusters
of three to five nodes with real streaming standbys; the helper module of the suite for set-up only;
every sequence written from the README text, not from `t/073`, `t/082` or `t/084`), 23 files, each run
on 18 first and on 19 when it showed something. Where timing mattered I made it deterministic
(a stopped standby, a held `ACCESS EXCLUSIVE` lock, a fault point that waits on a file, a kill at a
fault point) or counted runs: the tests that the README names for the answer tables were run 30
times each on each major (see "The answer tables as tables").

"Reproduced" means I observed it. "By reading only" means I did not run it. A partition was
emulated by stopping nodes; no packet filter was used (see "Not covered").

## Baseline

| Major | Regression tests | TAP files | TAP tests | Failed | Wall time |
|---|---|---|---|---|---|
| 18.7 (assertions) | 12 of 12 | 84 | 7276 | 0 | 1346 s |
| 19rc2 (assertions) | 12 of 12 | 84 | 7934 | 0 | 1354 s |

No compiler warning with `-Werror` on either major. Both runs happened at the same time on a machine
with a load average of about 5 or less. The numbers equal those of the commit message.

Repeats of the tests that produce the answers of the tables (6 streams at the same time, 30 rounds
each, all 30 rounds passed on both majors, no failure to examine):

| Stream | Files | Rounds per major | Result 18.7 | Result 19rc2 |
|---|---|---|---|---|
| A | `t/070` | 30 | 30 of 30 | 30 of 30 |
| B | `t/071`, `t/073`, `t/084` | 30 | 30 of 30 | 30 of 30 |
| C | `t/078`, `t/079`, `t/080`, `t/081`, `t/082` | 30 | 30 of 30 | 30 of 30 |

## Summary

New findings: 1 critical, 0 high, 1 medium, 4 low, 2 notes.

| ID | Severity | Summary |
|---|---|---|
| S10-01 | critical | Planned switchover, step 4: after a stop of OLD that was not clean (`-m immediate`, a crash, which the README itself allows when OLD hangs), `switchover_verify(M)` and `switchover_verify(X, true)` both answer `t` although NEW lacks rows that OLD acknowledged; 3 acknowledged rows were lost with no record, on both majors. The second check is exactly the one meant to catch writes after the mark |
| S10-02 | medium | A control-plane call has no deadline against a node that accepts the session and then does not answer: `promote_node`, `sync_catalog`, `rotate_secret`, `remove_node`, `check_cluster()` and `recover_prepared_transactions()` waited for the whole time an `ACCESS EXCLUSIVE` lock on a catalog table was held on one participant (69 s with `node_timeout` 3 and a 70 s hold; 24 s on 19). No answer names the node (no `unreached`, no error), so the repeat loop of sequence 9 repeats a call that cannot end |
| S10-03 | low | `kingres.standby_name_matches()` compares case-sensitively and does not know `*`; PostgreSQL matches the names of `synchronous_standby_names` case-insensitively: the README tells the operator to refuse a name that PostgreSQL accepts (`f` for a name that makes the standby `quorum`) |
| S10-04 | low | After a rotation the previous secret stays on every participant until the next rotation (only the origin drops it); by reading, the tag check of prepared-transaction names also accepts a tag made with the previous secret, so a leaked old secret keeps one of its powers |
| S10-05 | low | The answer tables have wrong or missing rows: the "registered on port" row is also the answer of a call sent to the wrong node; `add_node` answers that occur are not listed; a postcondition contradicts the chapter; the warning of `promote_node` is not in any table |
| S10-06 | low | Sequence 6 sends the operator to `pg_rewind` after a failover with an unreachable OLD; in my run `pg_rewind` could not run (WAL needed from the divergence point was gone after its own crash recovery); no fallback is named, and a rebuilt standby on another address stays `standby_unreachable` |
| S10-07 | note | A second caller of a catalog-changing call waits for the first with no bound and no answer; two controllers that call at the same time serialize (no damage), but behind a stuck first call the second hangs too |
| S10-08 | note | Every refusal of the tables carries SQLSTATE 55000 (`object_not_in_prerequisite_state`) except KS005; the text is the only discriminator a caller can match on, so the texts are an interface |

Rating of S10-01: by the letter of the rubric (a wrong answer, and committed rows lost with no
record) it is critical. The conditions are narrow: OLD must be stopped without a clean shutdown and
NEW must be behind when it stops; both are the normal case when the reason for the switchover is that
OLD hangs, and README precondition 1 and step 3 name `-m immediate` as the way to stop it.

No server crash, no escalation of privilege and no committed row lost or doubled by the extension's
own call paths was found; two primaries of one group both serving was not found through the cluster
(the unfenced OLD keeps taking the writes of its own group, which the README states).

## Fate of S9-01 to S9-10

| ID | Fate | Evidence |
|---|---|---|
| S9-01 (critical) | Fixed, with the residue the README states | `t/075` passes in the baseline on both majors. My R06: a participant whose standby is away at recovery: `COMMIT PREPARED` is cancelled after `node_timeout`, counted as resolved once, the second pass finds nothing prepared, the 4 decisions stay `durable = false`, `dtx_unconfirmed` has the row and `check_cluster()` says `decision_unconfirmed`; with the standby back and the retention over, the rows go and `check_cluster()` is empty (both a commit and a rollback decision, 18). I found no path that forgets a decision while a registered synchronous standby has not confirmed. The residue (a participant that requires no confirmation) is by reading only |
| S9-02 (high) | Fixed | R02: the promoted map standby answers `add_node` with "this node was promoted in PostgreSQL after the catalog named it the map group primary" until `promote_node` ran on it; both majors. R15: the promoted node killed at 10 fault points, all converge (see sequence 3). Every function that changes a catalog was listed from the install script (see "What held"): all reach `kingres_shard_require_map_group()` |
| S9-03 (high) | Fixed; a leftover is S10-04 | R05: the first rotation prepared on a node that dies; the second rotation is refused ("cannot be rotated while distributed transactions are in doubt", counting the first), recovery commits the first through the previous secret, the origin drops its previous secret on the pass after; `check_cluster()` empty. The participants keep theirs (S10-04) |
| S9-04 (medium) | Fixed as far as tested | Every `promote_node` answer carries `blocked_by` (seen `[]`); `t/078` 30 of 30 on both majors. An ordinary role holding the lock cannot be constructed (only ACCESS SHARE, by reading `t/078` and the privileges); I did not construct a blocker that changes between calls |
| S9-05 (medium) | Fixed as far as tested | `t/079` 30 of 30 on both majors; my R08 and R09 promote with OLD alive and reachable and it is demoted without KS005 (OLD's list did not name NEW there, so not the exact scenario) |
| S9-06 (low) | Fixed as far as tested | `t/079` 30 of 30 on both majors; I did not look at `check_cluster()` after a promotion with OLD reachable myself |
| S9-07 (medium) | Partly fixed: see S10-01 | R03, clean stop (`-m fast`): the late rows are caught by the second check (`verify(X, true)` = `f`), 18 and 19. Not caught after an immediate stop |
| S9-08 (low) | Fixed | R17: with a wrong list on one primary, `add_node`, `remove_node`, `rotate_secret` and `sync_catalog` all refuse after 3 s (node_timeout 4) with "node 2 cannot commit: its synchronous standbys do not confirm" and change nothing; the correct order works; the R20 loop below shows KS005 as the one answer |
| S9-09 (low) | Fixed | R19, R21: a plain role is refused by `internal_sync_confirm`, `internal_dtx_applied`, `internal_sync_probe`, `switchover_mark`, `standby_name_matches` on a node in a cluster and on a node that is not in one yet, with and without `kingres_shard.forwarded=on` (the first by the function, the second by "cannot prove it" at the session) |
| S9-10 (note) | Fixed | R04: `promote_map_group_without_majority` is refused to a plain role ("permission denied"), refused with a wrong acknowledgement (exact text in the hint), refused for an unknown node (after a warning, harmless); with all other nodes alive it promotes with term 1000 (0 + 1000), tells every reachable node, the old map primary becomes `standby` in the catalog and refuses catalog changes ("this function can only run on the map group primary"); a second call answers `already`. The partitioned case (the other side promoting meanwhile, then healing) I did not run |

## The answer tables as tables

### How the rows were audited

1. Every test that the tables name produced its row: the baseline passes (84 files), and the 30-round
   repeats above (the files that name the rows of sequences 1, 2, 3, 5, 8 and 9) did not fail once on
   either major.
2. The race of the last round (a commit that touches a group whose standby is away, answered by KS005
   or by an empty text). My R20: a standby away, synchronous; a distributed commit through the origin,
   12 runs for each of `node_timeout` 2, 3, 5 and 7, on each major: 48 of 48 answered with one text,
   SQLSTATE KS005 "did not confirm that its part of the transaction is durable on its synchronous
   standby"; nothing was committed anywhere and nothing was left prepared. The race is closed for
   those values.
3. Calls driven in states that the tables do not name (R07, R08, R17, R18, R22): the rows that are
   wrong or missing are listed per sequence below.

### Pairs of bounds

| Pair | Schedule | Result |
|---|---|---|
| origin's wait for the participant's PREPARE (`node_timeout`, then `max(T, min(T,5)+2)` after the cancel) against the participant's confirmation wait `min(T,5)` | standby stopped, `T` = 2, 3, 5, 7 (R20, 48 runs per major); `t/070`, `t/084` 30 rounds | one answer (KS005); the origin always waits longer than the participant. No variation found |
| second phase `COMMIT PREPARED` bounded by `node_timeout` (then another `node_timeout`) against the participant's wait for its standby (none) | by reading; recovery's own `COMMIT PREPARED` in R06 (standby stopped, `T` = 3) | the cancel ends the participant's wait after the commit; the origin counts the part committed, asks, gets "not confirmed" and keeps the decision. One answer. For `T` <= 5 the origin's bound for the confirmation question (`T`) equals the participant's own (`min(T,5)`): the participant answers `f` or is cancelled; both give the same warning text, so the client cannot see which; not a finding |
| `promote_node` probe `min(T,5) - 1` against the prepare confirmation `min(T,5)` | by reading; `t/071` | a standby that is late by between the two is refused ("cannot commit") although the commit would have succeeded; a fixed threshold, the refusal says to repeat, not a race. Not measured |
| recovery's wait for the decision to be durable `min(T,5)` plus its question `min(T,2)` against the cancel of `COMMIT PREPARED` (`T`) | R06, `T` = 3, standby away: 8 s and 5 s per pass, deterministic | one answer, no empty text in the log |
| recovery worker's interval against `promote_node`'s own recovery pass against a hand call of `recover_prepared_transactions()` | by reading (a second `COMMIT PREPARED` of a gone transaction is the SQLSTATE `undefined_object` branch "resolved by someone else"); not constructed | not reproduced |
| `decision_retention` against the durable mark | R06 (retention 3 s), `t/075` | rows go only after both |
| remote lock waits against the origin's bound | a lock held on a participant while a control-plane call runs (R09, R11, R23); a row lock held while a DML statement runs through the origin (R12) | control-plane calls: no bound, S10-02. DML: waits for the lock without error (took 8 s with `node_timeout` 3 and an 8 s hold, 6 of 6 runs); that is the documented "each node applies its own `lock_timeout`" (README, "not carried"), not a finding |
| concurrent callers of catalog changes | R07, R18: a second `promote_node` and an `add_node` of the same node and of another node while the first call is held at a fault point | the second waits for the first with no bound, then answers `already` (`promote_node`), "already registered" (same node) or succeeds (other node, the ids are 3 and 4, every node knows itself by its id, nothing prepared); S10-07 |
| `kingres_shard_exec_params` (no deadline) against `node_timeout` | R09, R11, R23 | S10-02 |

### Rows per sequence

The verdicts and lists are in the per-sequence sections below.

## Sequences

For each sequence: I ran the text as a script, in the order given, on both majors (1, 2, 3, 8 and 9
fully; 4 and 6 in part; 5 in part; 7 only through 5 and 6).

### 1. Registering a standby

Verdict: right as written for the answers that occur, incomplete in the table; one step misleads for
some names.

Run (R01, R17, R13): `add_node` of two standbys (ids returned), `standby_name_matches` `t` for both,
list `ANY 1 (a, b)`, `standby_status()` `streaming` and `quorum`; the failure answers of step 1 were
reproduced as listed.

Rows missing from the table (all reproduced on 18):

| answer | situation |
|---|---|
| error "group 9 has no primary node" | a standby for a group id that does not exist |
| error "group 1 already has a primary (node 2)" | a node that is a standby given with role `primary` |
| error "invalid node role "bogus"" | a wrong role |
| error "could not connect to node H:P" (with the libpq text) | an unreachable address registered as a primary |
| error "kingres_shard must be loaded via shared_preload_libraries" with the context `SQL statement "CREATE FUNCTION kingres.internal_wal_timeline() ..."` | a primary registered on a node that has no library loaded: the text is right, the context points at an internal function and is not the first thing the operator should see |

Rows whose answer can vary for one situation: none found (the standby-away answers of the group are the
KS005 answers of R20).

Change: step 2 says the check "must answer `t`": say that it answers `f` for a name that differs only
in case and for `*` although PostgreSQL accepts both (S10-03); until that is fixed, the control plane
must compare names case-insensitively itself and treat `f` for a name that matches case-insensitively
a streaming standby as inconclusive.

### 2. Failover of an ordinary group

Verdict: right as written for every state I could construct with all nodes either dead or answering;
incomplete for a node that is up and does not answer (S10-02).

Run (R01, both majors): three groups, two standbys of group 1, synchronous list, 20 distributed
commits touching groups 1 and 2, `pg_ctl stop -m immediate` of OLD, positions read (equal), one standby
promoted, its list set to `''`, `promote_node` on MAP: call 1 `promoted`, `final` false, `unreached`
`[2]`, call 2 `already`, `final` true. Postconditions: roles `2:standby, 4:primary` the same on MAP and
the other primary, term 1 everywhere, no prepared transaction, 40 rows before and 40 after, 5 more
distributed commits succeeded, the second standby re-pointed and named in NEW's list was `streaming` and
`quorum`. All the postconditions held except the one that says every standby is `streaming` with an
empty `problem`: OLD, which is down, shows `unknown [standby_unreachable]` (see S10-05).

The loop "repeat at most 10 times" was never needed beyond 2 calls in any run, including 10 runs with the
call killed at a fault point (sequence 9).

Rows wrong or missing:

| row | problem |
|---|---|
| "`promoted` / `completed` with `unreached`" | the same call also prints a `WARNING` ("the former primary, node 2 (...) cannot be reached and keeps its old catalog") on the session, with a connection error as detail; not in the table, and a caller that treats warnings on stderr as failures would stop |
| "any other error, or the connection breaks, or the client times out -> repeat" | a call that waits on a node that accepts the session and does not answer never returns (S10-02); the repeat hangs the same way. Add a row: "no answer within the client timeout twice in a row: probe every primary with a read-only call of your own with a deadline; the one that does not answer is the problem" |
| `already`, `final` true | also seen as `NOTICE: node N is already the primary of group G` on the session and as the answer of `promote_node(<MAP's own id>)` and of the id of any primary of any group (`"term": 0`); harmless, add one line |

Rows whose answer can vary for one situation: none in the situations constructed (R20 above for the
KS005 row).

### 3. Failover of the map group

Verdict: right as written, one row misdescribed.

Run (R02, both majors): four groups, a standby of the map group; OLD (MAP) stopped immediately; the
standby promoted; before the call `add_node` on it answers the refusal "this node was promoted in
PostgreSQL after the catalog named it the map group primary ..." (the README lists this answer in the table of sequence 2 only;
it is the answer of every catalog call on that node); `promote_node` on NEW, call 1
`promoted`, `unreached` `[1]`, call 2 `already`; roles and terms the same on NEW and all three other
primaries (term 1); `sync_catalog()` and `recover_prepared_transactions()` ran without output;
distributed writes through another primary worked and reached NEW (45 rows before the failover, 55 after
five commits through another node, 61 after three more; 61 on the re-added standby too).

Killing the node that runs the call (R15, 18): the promoted node killed at each of the fault points
`promote_before_change`, `promote_after_local_change`, `promote_after_first_push`, `promote_after_push`,
`promote_after_recover`, `promote_after_sessions`, `after_prepare`, `after_log`, `after_commit`,
`between_commit_prepared`, restarted, and the call repeated: for the first eight the restarted node had
changed nothing and the repeat answered `promoted/false`, `already/true`; for the last two the change was
committed on the origin only (and on one more node for `between_commit_prepared`) and the repeat answered
`completed/false`, `already/true`; in every case the roles and terms were the same on every node at the
end, nothing was left prepared, `check_cluster()` showed only the unreachable old primary, a distributed
write worked and no row was lost (10 of 10; the same set for an ordinary group in R14, 12 fault points).

Rows wrong or missing:

| row | problem |
|---|---|
| "node N is registered on port P, this server listens on port Q" -> "the node row of NEW names another port than the server listens on" | the same text is the answer when `promote_node(<the map standby's id>)` is sent to MAP (or to any node other than NEW) with the standby still in recovery; the hint of the error says so ("Run kingres.promote_node() on the standby that was promoted"), the table sends the operator to repair a catalog that is not wrong (R07). Name both causes |
| (missing) `WARNING` and a connection error text for OLD | as in sequence 2 |

Rows whose answer can vary for one situation: none found.

Not run: the minority error (`t/063` D) and the partition itself; the electorate rule is by reading.

### 4. Failover when OLD is reachable but must be demoted

Verdict: right as written for the path I ran (OLD reachable, its list not naming NEW); the exact
S9-05 scenario (OLD's list naming NEW) only through `t/079`, 30 of 30.

Run (R08, R09): OLD (group 1) alive and reachable, the standby promoted and its list emptied: call 1
`promoted`, `unreached: []`, call 2 `already`; OLD became `standby` in the catalog of the third node (seen there).

Unfenced return (R22, 18): OLD stopped, `promote_node` (`unreached [2]`), OLD started again with the
old catalog and not fenced, as the README describes. `catalog_status` on OLD says term 0, not
superseded. A write of a key of OLD's own group is accepted and acknowledged (one row is in OLD's
local shard with routing off, none on any other node); a cross-group transaction started on OLD is
refused ("the catalog of the sending node (term 0) has been superseded by term 1", a retryable
refusal); after that refusal OLD's own statements route to NEW and the row it took is invisible to
OLD itself; `check_cluster()` on MAP says `standby_not_in_recovery` for OLD; `sync_catalog()` brings OLD
to term 1. This is what the README states for an unfenced OLD (the loss of the acknowledged write); no
new finding.

### 5. Planned switchover without loss

Verdict: wrong at step 4 (S10-01).

Run (R03, both majors): a group with one asynchronous standby, `CHECKPOINT`, `switchover_mark()` returns
M, NEW replays it (`switchover_verify(M)` = `t`). Then, to model the writes that arrive between the
mark and the stop (the README stops them in step 1; the second check exists for the case that they do
not stop), NEW is disconnected and three rows are written on OLD and acknowledged. Stops:

| stop of OLD | `pg_controldata` "Database cluster state" | `verify(M)` | `verify(X, true)` | late rows on NEW after `pg_promote()` |
|---|---|---|---|---|
| `pg_ctl stop -m fast` | `shut down` | `t` | **`f`** (caught) | 0 of 3 (and the check said so) |
| `pg_ctl stop -m immediate` | `in production` | `t` | **`t`** | 0 of 3 (the check said everything was there) |

With the immediate stop X is the last checkpoint that OLD completed before the writes, so any standby
that has replayed past M has replayed past X: the check cannot fail. Same on 18.7 and 19rc2.
Additional inputs: `switchover_verify('0/1')` and `switchover_verify('0/1', true)` answer `t` (the
function trusts the caller's position: an LSN of another node or timeline is not noticed); `NULL`
answers NULL.

What must be added or changed in the text: step 3 must say that the stop has to be clean for the
switchover to be "without loss", and step 4 must make the second check conditional on
`Database cluster state: shut down` in the same `pg_controldata` output (and say that after any other
state the promotion is a failover with loss: sequence 2, guarantee 12, and the rows written after the
mark are the loss); precondition 1's "or `-m immediate` when it hangs" must not be reachable from
step 3 without that consequence. Better in the code: `switchover_verify` with a third argument for the
state, or a function that reads the control file of the stopped node itself.

Rows: not a table; the step-4 statement "In `t/080` NEW is disconnected before the last writes: both
checks say `f`" holds for the clean stop only.

### 6. Re-adding an old primary as a standby

Verdict: incomplete (S10-06); the catalog side is right.

Run (R02): after the map failover, the old map primary rebuilt as a standby of NEW (`pg_basebackup`
style copy, `application_name` set): its node row was already `standby`; `catalog_status` on it shows
term 1 and `superseded = false` on the standby; `standby_name_matches` `t`; the list set; commits on
the group wait for it and succeed; 61 rows on both. `pg_rewind` of the crashed old primary was tried
first as the text says: it ran the crash recovery in single-user mode (the preloaded library did not
interfere) and then stopped with "could not find previous WAL record" because the segment holding the
last common checkpoint was gone. The tiny test sizes make this likely, and a production setup with
`wal_keep_size` or an archive would not hit it, but the README presents `pg_rewind` as the step after
every crash. Say so, name `pg_basebackup` as the fallback, and say that a standby rebuilt on another
host:port is a different node: `remove_node` the old row and `add_node` the new (the old row stays
`standby_unreachable` in `check_cluster()` otherwise; I saw it).

### 7. Restoring the preferred primary

Verdict: not run separately; right by construction from 5 and 6 (it is sequence 5 with P as NEW). The
same defect as 5 applies. For the map group the call is on P itself and records P's timeline (see "What
held"); the text says so in step 6 of sequence 5.

### 8. Removing a dead standby

Verdict: right as written.

Run (R17, both orders): with a ghost name in the list of a primary, `remove_node(<standby>)` is
refused after 3 s (`node_timeout` 4, so `min(T,5) - 1`) with "node 2 cannot commit: its synchronous
standbys do not confirm" and nothing is changed; so are `add_node`, `rotate_secret` and `sync_catalog`
(the table names the first two only). After the list is cleared, `remove_node` returns nothing and
`standby_status()` has no row for the node; a second `remove_node` and a `promote_node` of the removed
id answer "node N does not exist". "At once" in the Bounds table is up to `min(T,5) - 1` seconds.
`remove_node` of a primary without standbys (node 3, in a cluster with no distributed table, R07) succeeds and
returns nothing; I did not test it with a table.

### 9. A call that did not finish

Verdict: right as written for a call that dies (a kill of the node that runs it at 22 fault points of
the ordinary and the map case; a client cancel is `t/082`); wrong in its promise for a call that hangs
(S10-02).

R14 and R15: killing the node that runs the call at every fault point of `promote_node` and at the
generic two-phase points (`before_prepare`, `after_first_prepare`, `after_prepare`, `after_log`,
`after_commit`, `between_commit_prepared`): the repeated call converged within 2 calls every time (the
two orders `promoted/false` then `already/true`, or `completed/false` then `already/true`), the catalogs
agreed on every node, nothing stayed prepared, rows were intact, a distributed write worked afterwards.
The bound "at most 10 calls" was never approached.

For the call that never returns, add: the control plane must put its own deadline on every call of the
chapter (including `recover_prepared_transactions()` and `check_cluster()`), and after two deadlines
in a row probe each primary with a read-only call with its own deadline before calling again.

## Findings

### S10-01 (critical) Planned switchover: both checks pass after an unclean stop of OLD while acknowledged rows are missing

Steps (R03, 18.7 and 19rc2): a group with an asynchronous standby; `CHECKPOINT`; `M = switchover_mark()`
on OLD; wait until NEW has replayed M (`switchover_verify(M)` = `t`); disconnect NEW (set an invalid
`primary_conninfo`, reload); write three rows on OLD (acknowledged, async); stop OLD with
`pg_ctl stop -m immediate`; read `pg_controldata` of OLD: "Latest checkpoint location" = X,
"Database cluster state" = `in production`; on NEW `switchover_verify(M)` and `switchover_verify(X, true)`.

Observed: `t` and `t`; after `pg_promote()` NEW holds none of the three rows ("late rows: 0"). With
`-m fast` instead, the same script answers `t` and `f` and the rows are indeed missing (the check
works).

Expected: `f` (or an error) in the first case. The second check's reasoning ("the start of the shutdown
checkpoint, the last record OLD wrote") is true only when the shutdown checkpoint exists; for a node
that did not shut down, X is an older checkpoint and says nothing about what OLD wrote after it.

Place: `switchover_verify` in the 0.5 install script; README step 3 and 4 of sequence 5 and
precondition 1.

Direction: make the second check refuse unless the caller says the state of the stopped node was
`shut down` (an argument that must be that text), or read the control file in a C function; and
rewrite step 3 so that anything but a clean stop leaves the switchover (it becomes a failover with
loss).

### S10-02 (medium) No deadline against a node that accepts the session and does not answer

Steps (R09 on 18 and 19, R11, R23 on 18): three groups, `kingres_shard.node_timeout = 3`; a session on
a participant runs `BEGIN; LOCK TABLE kingres.map_version IN ACCESS EXCLUSIVE MODE; SELECT pg_sleep(70)`;
the control plane calls `promote_node(<id>)` on MAP.

Observed: the call returns after 69 s (hold 70) on 18 (24 s with a 25 s hold on 19), with a normal
`promoted` answer and `"unreached": []`. With the lock held for 20 s (R11), `sync_catalog()`,
`rotate_secret()`, `check_cluster()`, `recover_prepared_transactions()` and `remove_node(3)` each took
18 to 20 s and then succeeded; with the same lock on `kingres.cluster_secret` (R23) `sync_catalog()`
took 41 s with a 40 s hold. While it waits, nothing on the participant is a "node did not answer" error
and nothing names the node.

Expected: the README says that `node_timeout` "bounds every wait on another node that must not last for
ever" and the table of sequence 2 has a row for a participant that does not answer ("the node did not
answer within kingres_shard.node_timeout ..., nor in the time it gets after the command was
cancelled"). That row is produced only in the PREPARE phase; the statements before it
(`internal_promotion_state()` in `read_state`, the term read of the participants, the push) use
`kingres_shard_exec_params`, which waits with no deadline (`libpqsrv_get_result_last`; the exact
waiting call was not stack-sampled, the code path is by reading), and the user
call of `recover_prepared_transactions()` does not set `statement_timeout` or `lock_timeout` on the
remote session (the worker does). A wedged node that still accepts sessions (a stuck disk, an
`ALTER EXTENSION UPDATE` in progress on it, a `VACUUM FULL` of the catalog) stops every failover of
every group, and the repeat loop of sequence 9 only repeats the wait. An ordinary role cannot take such
a lock (only ACCESS SHARE); a superuser or a maintenance job can.

Place: `kingres_shard_exec_params` in `node_conn.c`, `read_state` and the participant loops of
`failover.c`, the pass of `recovery.c` when called from a session.

Direction: a deadline and cancel on `exec_params` of the control-plane calls (as `bounded_exec`), an
answer that names the node (`unreached` with a reason "did not answer", or an error with its address),
and the same `statement_timeout` and `lock_timeout` on the remote session of a user-called
`recover_prepared_transactions()` that the worker sets.

### S10-03 (low) `standby_name_matches()` is stricter than PostgreSQL

Steps (R13, 18): a standby with `application_name = My-Standby`; `standby_name_matches` of
`My-Standby`, `my-standby`, `MY-STANDBY`, `*`; then lists `ANY 1 ("My-Standby")`, `ANY 1 ("my-standby")`,
`ANY 1 (*)`, `ANY 1 (nobody)`.

Observed: `t f f f`; the primary sees `quorum`, `quorum`, `quorum`, `async`.

Expected: the README says the function answers `t` for a name that the standby streams under and `f`
for a typo. PostgreSQL matches standby names case-insensitively and `*` matches every standby. The
operator who follows step 2 refuses a correct list. There is no false `t` (an exact match is a match).

Place: `standby_name_matches` in the install script (`=` on `application_name`). Direction: compare with
`lower()` on both sides and answer `t` for `*` when any streaming standby exists, or say in the text
that the check is by exact name.

### S10-04 (low) The previous secret is never wiped on a participant, and its tags are still accepted

Steps (R05, 18): rotation prepared on a node that dies, that node comes back, recovery commits; final
state: origin `fae070/-` (secret/previous: previous gone), participants `fae070/0f1800`: the old
secret stays in `kingres.cluster_secret.previous` on both participants until the next rotation, since
only the origin's recovery clears it. By reading `parse_gid`: a prepared-transaction name whose tag is
valid under the previous secret counts as the cluster's (`GID_TAGGED`), so whoever holds the old secret
can still forge a name that recovery will resolve (commit or roll back its own prepared transaction)
and that `rotate_secret` counts as in doubt; I did not run that. A node never accepts the previous
secret for a session (`t/076`, case 4, shows it; I did not repeat it).

Expected: a rotation done because a secret leaked leaves the old one with no power. Direction: clear
`previous` on every node when the origin's pass has reached every primary with the new secret (it
knows), and accept the previous tag only for the names of a rotation (as the comment says).

### S10-05 (low) Answer tables: wrong or missing rows

All reproduced or read from the text and the code as noted.

1. The "registered on port" row of sequence 3 (see there).
2. `add_node` answers that occur and are not listed: "group N has no primary node", "group N already has a
   primary", "invalid node role", "could not connect to node H:P", the `shared_preload_libraries` error.
3. `promote_node`'s `WARNING` (OLD or another node not reached) and `NOTICE` (already) on the session.
4. `rotate_secret` and `sync_catalog` have the same "cannot commit" refusal as `add_node`/`remove_node`.
5. A postcondition says every standby is `streaming` with an empty `problem`, while the same list says
   `standby_unreachable` for OLD until it is rebuilt: the first holds only after sequence 6.
6. The bound "a refused call: at once" is up to `min(T,5) - 1` seconds per node (3 s with `T` 4, per node,
   so more than one node in the wrong state is several waits in a row in a cluster with many groups;
   I saw one node).
7. A second concurrent caller (S10-07) has no row.

### S10-06 (low) Sequence 6: `pg_rewind` after a crash

Steps: see sequence 6. Observed once, in R02 (18): "pg_rewind: error: could not find previous WAL
record at 0/2000120" after the single-user crash recovery that `pg_rewind` runs. Expected: the README's
text to say what `pg_rewind` needs (WAL since the divergence on OLD, or `restore_command`) and to name
`pg_basebackup` as the fallback. Not a defect of the extension.

### S10-07 (note) A second caller of a catalog call waits for the first with no bound

Steps (R07, R18, 18): the first `promote_node` held at a fault point; a second `promote_node` of the same
node and an `add_node` of the same node and of another node start. After 8 s (15 s in R07) all three still run; when
the first ends: the second `promote_node` answers `already`; the second `add_node` of the same node
answers "node ... is already registered as node 3"; the `add_node` of another node succeeds with node
id 4; nothing is prepared, `check_cluster()` is empty. Correct and serialized; no damage. If the first
call is stuck (S10-02) the second is stuck behind it with no message. Say so in sequence 9.

### S10-08 (note) The answers have one SQLSTATE

The refusals of the tables ("cannot commit", "still in recovery", "can only run on the map group
primary", "was promoted in PostgreSQL after ...", "node N holds catalog term ...") all carry 55000;
"could not connect" carries the connection class; KS005 is the only distinct state. A caller that matches
on text must match the whole phrases listed; keep them stable and list them as an interface.

## What held under attack

- Every function of the install script that changes a catalog (listed by script) goes through
  `kingres_shard_require_map_group()`, which includes the timeline check: `add_node`, `remove_node`,
  `sync_catalog`, `rotate_secret`, `create_distributed_table`, `create_reference_table`, `move_bucket`,
  `cancel_bucket_move`, `set_node_property`, `create_restore_point`, `promote_node` (the ordinary-group path;
the map-group path is the call on the promoted node itself). The functions that
  write a catalog table and do not call it are `init_cluster` (refuses on a node that is initialised),
  `forget_dropped_tables` (an event trigger that removes a row for a dropped table),
  `resume_bucket_moves` and `cleanup_moved_buckets` (drive moves that exist from the node the catalog
  calls the map primary; not catalog changes in the sense of the term; on a promoted but unrecognised
  map standby they would send their commands to the old node's address by reading only). I found no
  entry that bypasses the check.
- The timeline check: a promoted map standby refuses every catalog call until `promote_node` ran on it
  (R02, R04, R15); crash recovery of a map primary does not change its timeline (R14/R15 restarts after
  `immediate` stops kept working). The way back for a restored node (`promote_node` of itself) refuses
  when any reachable group holds a newer term ("node N holds catalog term T, newer than ..."); I did
  not run an honest restore.
- Two rotations in a row with a node stranded at the first: refused while the first is in doubt (R05);
  a session proof made with the previous secret is refused (`t/076`, case 4; not repeated by me).
- The previous round's race (KS005 or an empty text): 48 of 48 per major, and the 30-round repeats of
  the tests that name the rows.
- Recovery against a participant whose standby is away (R06): `COMMIT PREPARED` and `ROLLBACK PREPARED`
  are cancelled after `node_timeout`, the part is counted once, a second pass finds nothing prepared,
  the unconfirmed decision is kept, no text of the log lines was empty.
- The move whose target's standby is away at the switch (R16, 18): the switch is refused with KS005
  ("did not confirm that its part ... is durable"), the bucket stays on the source, 300 rows, the move
  is `stalled_move` in `check_cluster()` and `resume_bucket_moves()` finishes it; with a healthy
  synchronous standby, a move followed at once by `immediate` stop and promotion of the target's
  standby loses no row (300 of 300; the moved bucket is on the target).
- Kills at 22 fault points of `promote_node` (R14, R15): see sequence 9.
- `promote_map_group_without_majority` (R04): who may call it, the acknowledgement, the term (+1000),
  the demotion of a reachable old map primary.
- The session proof of the confirmation calls (R19, R21).
- `blocked_by` in the answers (present as `[]`); no ordinary role can hold a stronger lock than ACCESS
  SHARE on the version table (by reading, `t/078`).

## For the control plane

What the failover implementation must do differently from what the README says today, in the order of
the sequences, so that it can be read alone:

0. Put an own deadline on every call of the chapter, `recover_prepared_transactions()` and
   `check_cluster()` included (at least `3 x node_timeout + node_connect_timeout x nodes`, longer for the
   2PC calls: `2 x node_timeout + 5` for a commit that waits for a standby). The extension has no bound
   against a node that accepts a session and does not answer; there is no answer that names it. After
   two deadlines in a row, probe every primary with a read-only call of your own with a deadline and
   fence or restart the one that does not answer; do not run the 10-call loop against it (S10-02).
   Never let two controllers call a catalog-changing function at once without a lease: the second waits
   with no answer (S10-07). Match the refusals on the whole text (they all share SQLSTATE 55000;
   S10-08), and treat `WARNING` lines on the session as information, not as failure.
1. Registering a standby: check the name yourself, case-insensitively, against
   `pg_stat_replication.application_name` before it goes into the list; `standby_name_matches` says `f`
   for `My-Standby` against `my-standby` and for `*` (S10-03). Handle the extra `add_node` answers
   ("group N has no primary node", "could not connect to node").
2. Failover of an ordinary group: works as written, in both majors. Call once, expect `promoted`,
   then `already`; ignore the `WARNING` for OLD; after `already`, recover on NEW, check
   `check_cluster()`. A killed or cancelled call converges within two repeats; it did in every case.
3. Failover of the map group: send the call to NEW itself and nowhere else; the error "node N is
   registered on port P, this server listens on port Q" means the call went to the wrong node as often
   as it means the catalog is wrong; before blaming the catalog, check where the call went. Until the
   call has been accepted NEW refuses every catalog call; do not call `add_node` and the like on it.
4. OLD reachable: stop it first, always. An unfenced OLD takes the writes of its own group and they are
   invisible and lost; `check_cluster()` shows it as `standby_not_in_recovery` only after the fact.
5. Planned switchover: only a clean stop (`-m fast`) is a switchover. After the stop read `pg_controldata`:
   "Database cluster state" must be `shut down`; then call `switchover_verify(M)` and
   `switchover_verify(X, true)`. If the state is anything else, or the stop had to be `immediate`, do not
   run the second check as a proof: it answers `t` (S10-01); treat the promotion as a failover with loss
   (sequence 2) and tell the operator which rows may be missing: those written after M. Use the same
   standby for the check and for the promotion (the function trusts the position it is given).
6. Re-adding OLD: try `pg_rewind`, but be ready for it to fail after a crash and fall back to
   `pg_basebackup`; a standby that comes back on another host:port is a new node: `remove_node` the old row
   and `add_node` the new one.
7. Failing back: as 5; for the map group the call is on P itself.
8. Removing a dead standby: clear the list first, as the README says; the refusal comes within
   `min(node_timeout, 5) - 1` seconds per node and applies to `rotate_secret` and `sync_catalog` too;
   alert on `decision_unconfirmed` that lasts: it means a participant's standby has not confirmed and
   the decisions are kept without limit (a few dozen bytes per distributed transaction).
9. A call that did not finish: repeat as written (it converges), but only after step 0.

## Not reproduced

- A foreign prepared transaction that blocks `promote_node` and is held by an ordinary role: not
  constructible by reading (ACCESS SHARE only; `t/078` 30 of 30); a blocker that changes between calls:
  not constructed.
- A second `COMMIT PREPARED` of a gone transaction, and a cancel that arrives exactly after the
  participant confirmed: by reading only (the `undefined_object` branch and the order of the cancel and
  the confirmation question); the constructed standby-away case behaves as described.
- A participant restored from a backup, a participant whose group gains or loses a standby between
  prepare and purge, retention 0 with every node restarted: by reading and `t/075` only; the
  conditions in `purge_log` (`nids > 1`, `durable OR NOT nids > 1`) read right.
- An honest restore or `pg_upgrade` of the map primary that changes its timeline and locks the cluster
  out, and `promote_node` of itself when it is in fact the stale node: by reading only (the refusal by
  a newer term in the loop of reached groups).
- Two nodes that both believe they are the map primary after a partition, each with its own timeline:
  not run (no packet filter).
- An empty text from the remaining uses of `PQerrorMessage` (`ask_node` in `node_admin.c`,
  `probe_session` and the connect error in `node_conn.c`, `PQresultErrorMessage` in the worker's
  `SET statement_timeout` branch): by reading they can be empty after a timeout (no
  fallback text as `kingres_shard_conn_error_text` has); I could not make one empty (R23 waited
  instead of timing out, S10-02).
- The tag check accepting the previous secret for a forged name (S10-04): by reading.

## Not covered

- Cursors and COPY across a promotion; a kill of the map primary at every fault point was run for
  `promote_node` (R14, R15, 18 only), not for `rotate_secret`, `add_node` or `move_bucket`.
- A partition with a packet filter; the healing of a forced promotion with an unfenced old side, and
  what a catalog change from the stale side does (a bucket move run there could lose rows; only the
  stale `add_node` refusal was seen).
- An upgrade from 0.4 with transactions prepared across the upgrade: I could not build one. A cluster
  made of 0.4 SQL objects under the 0.5 library cannot add a node (`add_node` fails with "function
  kingres.internal_dtx_applied(unknown) does not exist"), so there is no mixed state in which a
  distributed transaction prepares; a transaction prepared by a real 0.4 library was not available.
- Sequence 7 on its own; sequence 4 with OLD's list naming NEW; the 19rc2 runs of R04 to R08, R10 to
  R19 and R22 to R23 (only R01, R02, R03, R09 and R20 were run on both majors; the code is one source).
- `rebalance`, the web panel, the bucket move code beyond the switch.
- The 30-round repeats ran six streams at the same time; a quieter machine may show different timing
  but not fewer failures.
- Memory: 31 GB, 24 cores, load average below 5 during the baseline; the repeats and my own runs raised it.

## Baseline numbers

18.7: 12 regression tests, 84 TAP files, 7276 tests, 0 failed, 1346 s. 19rc2: 12 regression tests,
84 TAP files, 7934 tests, 0 failed, 1354 s. Repeats: 90 rounds of the named files per major (3 streams of
30; 180 in all), 0 failed. My own TAP files: 23, 5 of them on both majors.
