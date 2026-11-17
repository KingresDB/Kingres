<!-- SPDX-License-Identifier: AGPL-3.0-only -->
# Adversarial review: contrib/kingres_shard 0.5, the fix round for the third review of the call sequences (S11-01 to S11-12)

Reviewer: an independent reviewer (not the author of the code, not the reviewer of the earlier
rounds). Findings of this review are numbered S12-nn (the previous review,
`2026-11-17-kingres-shard-0.5-third-sequences.md`, used S11-nn).

## Scope and method

Reviewed state: the fix round that answers S11-01 to S11-12, exported with `git archive` of
`contrib/kingres_shard`, `contrib/kingres_sketch`, `kingres/dev` and `kingres/docs` into a private area;
its parent was exported the same way as the control for "what did this round change". The working tree was
not used. Nothing in the repository was changed except this file.

Read first: `2026-11-17-kingres-shard-0.5-third-sequences.md` (findings, "For the control plane", "Not
reproduced", "Not covered"). Read in the code: the whole diff of the round for `node_conn.c`/`node_conn.h`
(control depth, budget, lock timeout restore, lost-session note, transaction callbacks), `node_admin.c`,
`recovery.c`, `failover.c`, `ha.c`, `dist_table.c`, `remote_scan.c`, the upgrade script and the install
script, every reader of the depth, every place that closes a node session, and the chapter "What the control
plane calls" of the README (all nine sequences, the SQLSTATE table, the bounds tables). Read as tests:
`t/093` to `t/097`, the changes of `t/088` to `t/090`, `t/KsProxy.pm`.

Environment: private relocated copies of the two installations with assertions (18.7 and 19rc2; nothing
written into the originals), the port range of the Perl cluster module narrowed to 59900-59999, extension
built out of tree with `kingres/dev/ext-majors.sh` (`-Werror`, `-j4`, `nice`), my own TAP files (clusters
of three to five nodes, real streaming standbys, the TCP forwarder in front of nodes, SIGSTOP on node
backends, `pg_terminate_backend`, node restarts). Mutated copies of the extension (one line broken per
copy) were run against the test that claims the fix. A build of the parent of the round served as the
control. My own experiments ran on 18.7; the baseline suite ran on both majors.

The machine was shared and heavily loaded (load average 10 to 20). Near the end the Linux subsystem of the
machine stopped answering (an error of the service itself, not of my jobs); every run that was in flight then
was discarded and repeated, and nothing in this report comes from a run that ended that way.

"Reproduced" means I observed it, with the output given. "By reading only" means I did not run it.

## Baseline

| Major | Regression tests | TAP files | TAP tests | Failed | Wall time |
|---|---|---|---|---|---|
| 18.7 (assertions) | 12 of 12 | 97 | 7999 | 2 tests of `t/056_ri_distributed.pl` (see N3) | 2741 s |
| 19rc2 (assertions) | 12 of 12 | 97 | 8657 | 0 | 2369 s |

The totals equal the ones in the commit message (7999 and 8657). No compiler warning with `-Werror` on
either major. The two failures on 18 are `could not connect to node ...: timeout expired` during a run
with a load average above 20; the file passes alone (215 of 215, 20 s) on the same build (N3).

## Summary

New findings: 0 critical, 1 high, 2 medium, 3 low, 4 notes.

| ID | Severity | Summary |
|---|---|---|
| S12-01 | high (critical by the rubric where it applies to a participant that holds a write of the call) | The new eager `RESET lock_timeout` at the end of a control call closes a node session that still carries the call's own uncommitted remote work when the node does not answer that one statement. The session is dropped without a note (the writes of a control call are exempt from the lost-session note), the commit then skips it (`xact_depth` 0), and the call returns success. Reproduced with `remove_node`: `rc = 0`, no `WARNING`, the node removed from the catalog, its own catalog **not** cleared; the README promises a `WARNING`. Before the round the same silence failed the call at `PREPARE` (08006). Reproduced also with `rotate_secret`: silent after the 9th message gives `rc 0`, no `WARNING`, and the silent node holds a different secret from MAP's (a part of the call committed on some nodes only). By reading: the same for `add_node`, `sync_catalog`, `create_*_table` |
| S12-02 | medium | The README attributes the bound of seven calls to `t/096`; `t/096` runs two of them (`sync_catalog`, `rotate_secret`) plus the node to promote. The other calls' bounds and the answers `add_node`/`remove_node`/`recover_prepared_transactions`/`create_*` give for a silent node have no test under that name. The "slow" mode of `t/096` is 0.5 x `node_timeout`, not the 0.9 x the round claims to have considered |
| S12-03 | medium | A healthy but slow node (every answer takes 0.5 x `node_timeout` or more) now fails `sync_catalog`, `rotate_secret`, `add_node`, `remove_node`, `create_*_table` with `KS007`, where before it succeeded. The state left is clean, but the README says for `KS007` "do not repeat against that node", so a control plane stops for a node that is merely slow; the same call fails again on every repeat while the node stays slow. Documented budget, undocumented consequence |
| S12-04 | low | `remove_node` of a node that answers its first statements and then stops (silent after the 4th or 5th message) ends with `KS007` and the node stays in the catalog; the README row says the node is removed with a `WARNING`. The README only holds for a node that cannot be connected to |
| S12-05 | low | `promote_node` answers `KS014` ("rebuild the node") for every session refusal whose text is "claims to have been opened by another node", also for the other cause of the same text (a node that belongs to a cluster but holds no secret, for a non-superuser). By reading only |
| S12-06 | low | The columns of `kingres.pending_node` were added "in place" to 0.5; a database that already installed an earlier 0.5 has no `node_id` and `add_node`/`remove_node` then fail on the column (the install and the 0.4 to 0.5 upgrade give the same catalog, checked). By reading only for the installed-0.5 case |

Notes: N1 a lost read-only session in READ COMMITTED still fails the commit (same on the parent); N2 the
forwarder counts chunks, not messages; N3 `t/056` failed once under load; N4 `t/097` case M3 asserts "replayed
beyond", not "replayed to the end".

Rating. S12-01: by the rubric a control call is a distributed transaction that commits in part with a
success answer. I reproduced it with `remove_node` (the node's catalog is not cleared) and with `rotate_secret` (a participant keeps the old secret); the same code path drops the remote part of any participant. The window is the
time between the last statement of the call on a node and the end of the call, in which the node must not
answer one statement (a node that stalls while the call goes on with other nodes; a node restart; the
forwarder makes it deterministic). Everything the earlier reviews found and that has a test stayed fixed.

## Fate of S11-01 to S11-12

| ID | Fate | Evidence |
|---|---|---|
| S11-01 (critical) | Fixed; opened S12-01 | `t/093` passes (103 of 103). My E1: after a swallowed `KS007`, a control call in a `FOR` loop over a distributed scan, a control call from a function in the target list of a distributed scan, a nested control call, `rotate_secret` inside a savepoint that is rolled back: a user `UPDATE` to a node stopped by SIGSTOP for 6 s with `node_timeout` 2 waited 6.0 s, no deadline text, `COMMIT` ok, row stored (20 of 20). Forwarded DDL and a fan-out `DELETE` after a control call: 6.0 s and 7.4 s, no deadline. 27 combinations of (read committed, repeatable read, serializable) x (write A, kill A's session, write B, savepoints, release, node restart): atomic in every one, `COMMIT` always an error, nothing committed anywhere; identical on the parent (the kill is found at `PREPARE` there too), so these do not exercise the new note |
| S11-02 (high) | Fixed for the cases I tried | the node's own `lock_timeout` configured by the administrator (`ALTER DATABASE ... SET lock_timeout = '1000ms'`): after none, a control call in the transaction, a swallowed error, savepoint-control-release, control-savepoint-rollback, control inside savepoint then rollback then control, the user `UPDATE` was cut at 1.01 to 1.06 s with the node's own `55P03`, never at 0.75 x `node_timeout` (1.5 s), never unbounded (6 of 6) |
| S11-03 (critical by rubric) | Fixed | `t/094` passes; mutant M5 (the new bound removed) fails 16 of 17 tests of it (assertion crash on this build) |
| S11-04 (high) | Fixed | my R-series: standby down for a rotation, three recovery passes: previous secret kept; second rotation `KS014` naming the standby; standby back and replayed: previous dropped, rotation allowed; standby down for a rotation then removed with `remove_node`: rotation allowed; failover of the other standby while one is away: `promoted`, and the next rotation is refused with `KS014` naming the away standby until it is removed or back (the wait ends only with the standby, which the README says). Mutants M6 and M7 fail `t/095` (6 of 26 each) |
| S11-05 (medium) | Fixed | the row with `node_id` is written before the removal; `remove_node` with the origin silent at the 8th message leaves 1 prepared transaction on the removed node and the pending row, as designed (recovery resolves it); mutant M8 (the record removed) fails `t/089` (6 of 194). The upgrade 0.4 to 0.5 gives the same columns, functions, constraints and table comment as a fresh 0.5 (my E10) |
| S11-06 (medium) | Fixed (reading and `t/088`) | not re-run beyond the baseline |
| S11-07 (medium) | Partly fixed as stated | measured below: within the documented bound in every cell I ran (largest 20.3 s against 20 s + tolerance); the opposite defect is S12-03; mutant M9 (the budget removed) fails `t/096` (8 of 56) |
| S11-08 (medium) | Documentation only | see "The caller's duties" below; complete as far as I could find |
| S11-09 (low) | Fixed | `t/096` promote part passes; `KS007` naming "which is to become the primary of group G" |
| S11-10 (low) | Fixed | the new tests are not decorative: see "The tests of this round" |
| S11-11 (low) | Documentation only | the README now says a name that does not resolve is not bounded |
| S11-12 (low) | Fixed | `t/093` section 6; mutant M4 fails it (4 of 107) |

## The tests of this round

For each fix I broke the fix in a copy and ran the test that claims it (18.7, same installation for all
mutants):

| Mutant | What was broken | Test | Result |
|---|---|---|---|
| M2 | `check_session_lost()` removed from pre-commit, parallel pre-commit and pre-prepare | `t/093` | fails, 4 of 107 not ok |
| M4 | the error for a statement not carried out (`fetch_remote`) removed | `t/093` | fails, 4 of 107 |
| M5 | the position bound of `switchover_verify` removed | `t/094` | fails, 16 of 17 (crash) |
| M6 | `unconfirmed_standbys == 0` removed from the drop of the previous secret | `t/095` | fails, 6 of 26 |
| M7 | the "has the standby confirmed" check of `rotate_secret` removed | `t/095` | fails, 6 of 26 |
| M9 | the per-node budget removed | `t/096` | fails, 8 of 56 |
| M8 | the record of the removed node removed | `t/089` | fails, 6 of 194; `t/092` still passes (it does not cover this fix) |

Each establishes its precondition: `t/093` stops the client backends of the node after the session exists,
holds a row lock longer than `node_timeout`, and checks the rows on the nodes afterwards; `t/095` checks
`has_previous` before it acts; `t/097` M2 and M4 use a real earlier clean stop of a restarted primary, M3
a standby paused by `recovery_target_lsn`. Weaknesses: (1) `t/096` drives two of the seven calls the
README names (S12-02); (2) no test closes a node session at the end of a control call (S12-01), which is
the one place where the new code closes a session that holds the call's own writes; (3) N4: the M3 case
asserts `replayed > x`, so the "replayed exactly to the end" branch is not what it isolates (the text of
the error does separate the cases, so the mutant is still killed); (4) the forwarder `t/KsProxy.pm` does
go silent (it keeps reading and drops what it reads, the connection stays open) and does not close the
connection, which is the case it claims; it counts chunks, not protocol messages (N2), and a delay is
applied per chunk, so an answer that arrives in two chunks is delayed twice (a "1.5 s" mode behaves like 3 s
for such answers: `create_reference_table` failed at 6.1 s with "did not answer within 3 seconds", a single
wait, in my run).

## The answer tables

Rows I produced myself against the README (18.7):

| Row | Result |
|---|---|
| `KS007` for a participant that is silent after the 12th message of `sync_catalog` | `rc 0` with `WARNING could not commit prepared transaction ... recovery will commit it`; 1 prepared transaction on two nodes; `rotate_secret` then `KS013`; one recovery pass: 0 prepared. As documented |
| `KS007` for `add_node` of a node silent after the 6th / 12th message | `KS007` naming the node, 3.1 / 3.2 s, nothing prepared |
| `KS007` "answers are slow" | produced for `sync_catalog`, `rotate_secret`, `create_*`, `add_node`, `remove_node` with answers 2.7 s late at `node_timeout` 3 (text names the 8 seconds); `recover_prepared_transactions` ends with a `WARNING` carrying the same code and `rc 0`, as the row says |
| `KS014` for `rotate_secret` | produced (see S11-04); the text names the standby |
| `KS014` for `promote_node` | `t/095` section 4; text-matching, see S12-05 |
| connect phase: node accepts and never answers the startup | `08001 could not connect`, not `KS007` (the README table has the class `08` row; the `KS007` row says "accepted the session") |
| `remove_node` of a node that cannot be connected to | `WARNING could not reach node`, node removed, 2.0 s, as the row says |
| `remove_node`, node silent after the 4th/5th message | `KS007`, node still in the catalog (S12-04) |
| `remove_node`, node silent after the 6th message | `rc 0`, no warning, node removed, its catalog not cleared (S12-01) |
| `remove_node`, node silent after the 7th message | `08006 could not prepare the transaction on node ...`, 8.1 s, node still in the catalog |
| a control call in a repeatable-read or serializable transaction | `recover_prepared_transactions()` and `promote_node` refuse with `25001` ("must run in a READ COMMITTED transaction"); a control plane whose role defaults to repeatable read cannot promote; not in the README |

Rows found wrong: the `remove_node` row of sequence 8 and of the phase table (S12-01, S12-04); the
`t/096` attribution in the bounds table (S12-02). Not in the README: the `25001` answer for a control call in
a repeatable-read or serializable transaction (low; the README says nothing about the isolation level).

### Measured against the documented bounds (my E5)

`node_timeout` 3 s, `node_connect_timeout` 2 s, two nodes behind the forwarder (k = 2), one node to add.
Documented per node: `2 x 3 + 2` s of waiting plus 2 s for connecting, so 20 s for k = 2 plus the 4 s
tolerance. Seconds, 18.7.

| call | every answer 2.7 s late | every answer 1.5 s late | silent after 6 | silent after 12 | silent from the start |
|---|---|---|---|---|---|
| `add_node` (new node slow) | 17.2 `KS007` | 14.1 `KS007` | 3.1 `KS007` | 3.2 `KS007` | 6.0 `08001` |
| `sync_catalog` | 16.2 `KS007` | 12.6 `KS007` | 6.1 `KS007` | 6.1 ok + `WARNING` | 4.1 `08001` |
| `rotate_secret` | 20.3 `KS007` | 15.6 `KS007` | 3.1 `KS007` | 0.1 `KS013` (the above) | 4.0 `08001` |
| `recover_prepared_transactions` | 16.1 ok + `WARNING` | 12.1 ok | 0.1 ok | 0.1 ok | 4.1 ok + `WARNING` |
| `create_reference_table` | 8.0 `KS007` | 6.1 `KS007` | 6.2 `KS007` | 1.7 ok | 2.0 `08001` |
| `create_distributed_table` | 8.1 `KS007` | 18.6 `KS007` | 6.2 `KS007` | 0.2 ok | 2.1 `08001` |

Nothing exceeds the bound; prepared transactions per node after each call were 0 except the
`sync_catalog` silent-after-12 case above, which recovery resolved. I did not run k = 4 or 19rc2 for these
(see "Not covered"). The opposite defect is S12-03: the first two columns are calls that used to succeed
with a node whose every statement is slow and now end in `KS007`.

### The caller's duties for the switchover proof (S11-08)

The list in step 5 of sequence 5 (OLD unable to start, `pg_controldata` read again immediately before
`pg_promote()`, every line unchanged, `postmaster.pid` absent) is, as far as I could find, complete: the
two ways to a false `t` that I know are a restart-and-stop cycle of OLD while NEW was disconnected (the
re-read position changes with every shutdown checkpoint, so a caller that follows the list sees it) and a
position from another node (refused by the WAL read). A control plane that follows the list exactly cannot
promote a standby that lacks acknowledged rows by this route. I did not find another. The `f` and `KS006`
rows are as before. Tests `t/097` M2 and M4 give the "replayed beyond" and "left the timeline" texts.

## Findings

### S12-01 (high) The eager `RESET lock_timeout` at the end of a control call can drop the call's own remote work, and the call succeeds

Where: `kingres_shard_control_leave()` (`restore_lock_timeouts()`), `restore_lock_timeout()` and
`discard_connection()` in `src/node_conn.c`; the commit skips what it cannot see in `precommit()`.

Observed: when the outermost control call is left, every node session that carries the call's
`SET LOCAL lock_timeout` gets a `RESET lock_timeout` through the plain bounded wait. If the node does not
answer that one statement within `node_timeout`, `restore_lock_timeout()` calls `discard_connection()`. The
session is closed in the middle of the remote transaction that holds the call's own writes. The note of a
lost session is not made for them (`wrote_user` is false: "the writes of a control call are its own
business: it decides per node"), but nobody decides: the call has already returned. At commit,
`precommit()` looks only at sessions with `xact_depth > 0`; the discarded session has none, so no
`PREPARE` is sent to it, the node rolls its part back with the session, and the origin commits the rest
with a success answer. Before the round the same silence ended the call at the `PREPARE` (08006, see the
`prepare_one` text in the output below), so this is new.

Steps (18.7, forwarder in front of the node; `node_timeout` 3, `node_connect_timeout` 2): a cluster of two
nodes, the second registered by the address of the forwarder (`kingres.add_node('127.0.0.1', <proxy port>)`),
`kingres.remove_node(2)` on the first with the forwarder set to `swallow_after => 6` (it forwards the first
six chunks of a session and then swallows):

    remove_node, silent after 6:  t=3.1 s  rc=0  err=[]
      node 2 in the catalog of MAP afterwards: 0
      rows of kingres.node on the removed node: 2      (a normal removal leaves 0)
      prepared transactions on the removed node: 0
    the log of the removed node: ... SET LOCAL lock_timeout = 2250 // SELECT kingres.internal_reset_node()
      (no RESET lock_timeout, no PREPARE TRANSACTION ever reaches it)
    control runs: silent after 4 and 5: KS007 (the call fails, node stays)
                  silent after 7: 08006 "could not prepare the transaction on node ..." (the node stays)
                  silent after 8: rc 0 with "WARNING could not commit prepared transaction", 1 prepared
                    transaction left, recovery resolves it (documented)
                  no proxy: rc 0, 0 rows left on the node

The run was repeated in two files (7 modes and a 3-mode repeat with the statements of the removed node
logged); the result for "silent after 6" was the same each time.

Expected: a part of a control call that cannot be delivered ends the call, or at least says so (the README
row of sequence 8: "`WARNING: could not reach node H:P; its catalog was not cleared`"). Here the call says
nothing, and the removed node keeps the catalog and the cluster secret it had.

Why it matters beyond `remove_node`: the same `RESET` follows every call that wrote on a node. For
`add_node` the new node's part (its catalog and secret) is that write; for `rotate_secret` and
`sync_catalog` a participant's secret or catalog. A participant that stalls (or restarts) between its last
statement and the end of the call is skipped at commit and the call answers success. Sweep over the position of the silence (`swallow_after`, 18.7, `node_timeout` 3; E15):

    rotate_secret, k=9:  rc=0, no message, node 2's secret hash DIFFERENT from MAP's, 0 prepared
    rotate_secret, k=8:  KS007; k=10: 08006 could not prepare; k=11: rc 0 + WARNING commit prepared (documented);
                         k=12: rc 0 + WARNING standby; k=13,14: ok (same secret)
    sync_catalog,  k=10: rc=0, no message (the node's state looked normal in my check; no control to tell)
                         k=6..9: KS007
    add_node,      k=15: rc=0, registered; k=10..14: KS007; k=16: 08006   (my fixture node has no extension, so
                         I cannot say from this run what its part was; no control run without the forwarder)

So the partial commit with a success answer is reproduced for `rotate_secret`, and the window is a
single chunk wide in each call (one position of the silence in the sweep). The window is real for
`sync_catalog`, which talks to its nodes one after the other: a node served first that stalls for
`node_timeout` while the call goes on with the others is dropped at the leave.

Direction: do not send the `RESET` at the end of a call as a new statement whose failure closes a
session. The `SET LOCAL` already ends with the remote transaction, and `kingres_shard_conn_quiesce()`
restores it before any user statement (so the eager restore is not needed for S11-02). If it stays, a
failure must leave the session and its `wrote` flag alone, mark the session for the quiesce, and let
`PREPARE` fail as before; and `discard_connection()` of a session with `wrote` set must note the loss for
control writes too (the exemption cannot stay once the discard can happen after the call is left).
Add a test that silences a node after the last statement of each call (a loop over `swallow_after` as in
my sweep is enough).

### S12-02 (medium) The bounds of seven calls are credited to a test that runs two

The bounds table of the README names `t/096` for `add_node`, `remove_node`, `sync_catalog`,
`rotate_secret`, `recover_prepared_transactions`, `create_distributed_table` and `create_reference_table`
("late answers, silence after the 6th message, silence from the start"). `t/096` runs `sync_catalog` and
`rotate_secret` in those modes, and `promote_node` for the node to promote. The measurements in the table
above are mine and all within the bound, but the table of the README must be read as "`t/096` for two calls".
The "late answers" mode of `t/096` is 1.5 s at `node_timeout` 3 (0.5 x): the case "answers in 0.9 x
`node_timeout` each time" that the round says it studied is not in any test, and S12-03 is exactly about it.
Direction: loop `t/096` over the other calls (the fixture already builds the cluster once), or change the
README's attribution.

### S12-03 (medium) A healthy but slow node now fails calls that used to succeed

Steps (18.7): `node_timeout` 3, nodes behind the forwarder with every answer delayed 2.7 s (and 1.5 s),
`sync_catalog`, `rotate_secret`, `create_reference_table`, `create_distributed_table`, `add_node`,
`remove_node`: each ends with `KS007` "did not answer within the 8 seconds that one call gives a node (its
answers are slow)", after 8 to 20 s (table above). Nothing is left prepared; the same call succeeds once the
node is fast. At 1.5 s per answer `create_distributed_table` failed after 18.6 s. A node whose statements
each take half of `node_timeout` is not stuck.

Expected: the budget is documented, but the README treats every `KS007` as "do not repeat against that node:
probe it, fence or restart it" and "repeating does not help". For a slow node the control plane would fence
a working node, or stop. The text of the error already says "its answers are slow" and a different hint
would be cheap. At the default `node_timeout` (60 s) the budget is 122 s of waiting per node and call, so
the case needs statements of many seconds; a very large `COPY` into a node by the two `create_*_table` calls
is the realistic candidate (I did not measure it, see "Not covered").
Direction: give the "slow, not stuck" text its own SQLSTATE or at least its own hint and README row, and
state in the README that the budget counts waiting for answers per node and per call.

### S12-04 (low) `remove_node` of a node that answers and then stops is refused

With the forwarder silent after the 4th or 5th message `remove_node` ends with `KS007` naming the node and
`internal_reset_node()` as the statement; the node stays in the catalog (reproduced twice). The README row
("the node is removed from the catalog and its own catalog is not cleared (`WARNING` names it)") is true
only when the session cannot be made; for a node that is up and stuck, the removal needs the node to be
fenced first (the `KS007` hint says so, the sequence does not). Direction: one sentence in sequence 8 and
in the phase table: "a node that is up and not answering is not removed: fence it, then repeat".

### S12-05 (low) `KS014` names a cause the node did not prove

`target_failure()` in `src/failover.c` maps every refusal whose text contains "claims to have been opened
by another node" to `KS014` with the advice to rebuild the node. The same text is raised in
`forward_auth.c` for two causes: a token that does not match (wrong secret, or a wrong user, database or
node id in the claim) and a node that belongs to a cluster but holds no secret (for a non-superuser, with a
hint to run `sync_catalog()`). For the second the advice is wrong. By reading only; `t/095` section 4 covers
the first cause. Direction: use the `errdetail` of the node (it differs: "holds no cluster secret") or give
the second cause its own text.

### S12-06 (low) `pending_node.node_id` was added to 0.5 in place

The fresh 0.5 script and the 0.4 to 0.5 script give the same columns, functions, constraints and table
comment (checked: 4 of 4 equal). A database that already has an earlier build of 0.5 has no `node_id`, and
the statements of `add_node` (every call, not only `remove_node`) list the column, so `add_node` fails with
an undefined column there. If 0.5 was never released this is a note; if any database holds the earlier
0.5, a `0.5--0.5.1` step or a note in the README is needed. By reading only.

### Notes

N1. A transaction whose only session to a node is a read in READ COMMITTED, with that session killed, fails
at `COMMIT` ("terminating connection due to administrator command"); a transaction that lost nothing is
refused. Identical on the parent, so it is not new. N2. The forwarder counts chunks, not messages; "silent
after 6" means after six reads of the client side. N3. `t/056` failed two tests under a load above 20 with
`could not connect to node ...: timeout expired`, and passed alone. N4. `t/097` M3 (see above).

## For the control plane

- Treat a `remove_node` / `add_node` / `rotate_secret` / `sync_catalog` that answers success without a
  `WARNING` as unverified for the nodes that were slow or restarted during the call, until S12-01 is
  fixed: read back `kingres.node` on the touched node (a removed node must have 0 rows, a new node the
  catalog) and compare the hash of `kingres.cluster_secret` on every primary after each rotation. The cheap check is `check_cluster()` plus the row count
  on the node.
- A role that defaults to repeatable read or serializable cannot call `recover_prepared_transactions()` or
  `promote_node()` (`25001`); use a role with `default_transaction_isolation = 'read committed'` for the
  control plane.
- `KS007` with "its answers are slow" is not a stuck node. Probe it before fencing; if it is only slow,
  repeat when it is quieter or raise `kingres_shard.node_timeout` for the call.
- A node that is up and stuck cannot be removed with `remove_node` (`KS007`, node stays): fence it first,
  then repeat.
- A second `rotate_secret` is refused (`KS014`) until every standby of the catalog has confirmed the last
  one; a standby that will not come back must be removed (`remove_node`) or the previous secret stays for
  ever. After a failover the old standbys of the dead primary are still standbys of the catalog until
  they are re-pointed or removed, and keep blocking rotation (reproduced).
- The switchover duties are complete as written; follow them literally, including the second read of
  `pg_controldata` right before `pg_promote()`.

## What held under attack

- The control depth: no path of the 20 shapes of E1 left the data path in a deadline state; the depth is
  restored by `PG_FINALLY` on every way out of the six wrapped functions and the two hand-written ones, and
  the data path reads it only in `kingres_shard_note_remote_write`/`begin_write_xact` (to tell a call's own
  write from a user's), the probe, `control_session_ready`, the error text for a lock wait, and the restore in
  the quiesce. Cursors, planner statements, a loop over a distributed scan with a control call inside,
  forwarded DDL and fan-out `DELETE` all waited for a stopped node and succeeded.
- The node's own `lock_timeout` (administrator configuration) comes back in every order I tried.
- Atomicity of user writes with a killed, restarted or terminated node session at every point of 27
  scenarios on three isolation levels: never a part.
- The upgrade path of `pending_node`.
- The previous secret: kept while a standby is away, dropped when it confirmed, rotation refused with the
  documented code, promotion of a lagging standby works.
- `switchover_verify`: the position fix and the tests that establish its checks (see mutants).
- The forwarder really goes silent instead of closing.

## Not reproduced

- A `lock_timeout` left on a user statement by a failed `RESET`: I could not make `restore_lock_timeout`
  fail without S12-01's discard; the discard path then closes the session, so no statement runs on it.
- A control call that closes a session with only its own writes and then makes the next transaction fail:
  the note is cleared at the end of every top-level transaction (the next transaction committed in every
  run).
- A wrong depth after an error caught by a PL/pgSQL block, from a trigger, a cursor, `ROLLBACK TO`, in a
  prepared transaction, in the recovery worker or in a parallel worker: by reading, the scope is a saved
  value restored in `PG_FINALLY` and forced to zero at the end of every top-level transaction; the error
  swallowed by a block was run (E1), the others were not.

## Not covered

- S12-01 for `create_*_table`; for `add_node` and `sync_catalog` the sweep shows a silent success at one
  position each, but without a control run I do not claim what part was lost.
- k = 4 nodes and 19rc2 for the timing table; the opposite defect on 19rc2.
- A large `COPY` of an existing table by `create_*_table` under the per-node budget.
- A promotion of a node's standby as the cause of a lost session; stale-term refusals; the idle-session drop
  after a catalog change (by reading, it only touches sessions outside a transaction).
- Name resolution that does not answer; a postmaster stopped by SIGSTOP; `ALTER ROLE` instead of
  `ALTER DATABASE` for the node's `lock_timeout`.
- A pending row for an address that now belongs to another node or to a reinstalled node (by reading: the
  row is deleted when the address is in the catalog, and recovery's session is refused by a node that holds
  another secret; no test).
- Mutants of `t/090` and of the standby-name matching.

## Baseline numbers

18.7: 12 of 12 regression tests, 97 TAP files, 7999 tests, 2741 s, the one failure explained in N3; 19rc2:
12 of 12, 97 files, 8657 tests, 2369 s, no failure. Both with assertions, `-Werror`, no warning. Mutants:
7 built, 7 killed by the test that claims the fix (M8 by `t/089`; `t/092` does not cover it).
