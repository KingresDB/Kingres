<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
# Fifth independent review of the call sequences of kingres_shard 0.5 (the fixes for S12)

Reviewer: an independent reviewer. Date: 2026-11-18.

## Scope and method

Reviewed state: the read-only export of `contrib/kingres_shard`, `kingres/docs` and `kingres/dev`. Nothing in the
export was changed. I read the README chapter "What the control plane calls" (SQLSTATE table, bounds, phase table,
sequences 8 and 9), `src/node_conn.c`, `src/node_admin.c`, `src/failover.c`, `src/recovery.c`, `src/forward_auth.c`,
`t/KsSweep.pm`, `t/KsProxy.pm`, `t/100`, `t/102`, `t/104`, and the previous review. Private copies of both
installations (18.7 and 19rc2, port range of the test cluster module narrowed to 59800-59999) and a private copy of
the export were used; extension built with `kingres/dev/ext-majors.sh` (`-Werror`, clean build, both majors: ok).
Own test files: a copy of `t/KsProxy.pm` with three added modes (the connection is closed after chunk n, the node's
answer to chunk n is lost, the node answers chunk n with an injected ErrorResponse), a copy of `t/100` that sweeps
those modes with the same judge, and eight files of my own cases (savepoint around a call, a hung call, rotation with a
lost second phase, two nodes at two positions, role and database `lock_timeout`, killed sessions, rollback and prepare
of the caller's transaction). Seven mutants of the extension (one change each, own installation) were run against the
test that claims the fix.

The machine was lightly loaded (load average 0.5 to 5). No failure in this report is a load effect. One of my own
files hung because I took an `ACCESS EXCLUSIVE` lock on a table that new sessions of the node read at start; that was
a fault of my test and is not counted.

## What I ran

| Major | Files | Tests | Result | Wall time |
|---|---|---|---|---|
| 18.7 | `t/093` to `t/104` (12 files, default matrix) | 1802 | all pass | 1033 s |
| 19rc2 | `t/093` to `t/104` (12 files, default matrix) | 1803 | all pass | 1031 s |

The rest of the suite (`t/001` to `t/092`) I did not run; that is the part of the "once on one major" request that I
did not do (see "Not checked"). Own cases: 18.7 for everything; on 19rc2 the hung-call case, the killed-session case
(`D1` to `D4`). Results equal 18.7.

Own sweeps with the judge of `KsSweep.pm` (rotate, add_node, remove_node on all three modes, sync_catalog on
two): the forwarder closes the connection after chunk n (rotate 12 positions, add_node 21, remove_node 9): pass; the
node's answer to chunk n is lost (rotate, sync, add_node, remove_node): pass (for example `remove_node`, lost answer to
`PREPARE`: `08006`, state old, 1 prepared transaction, resolved by one recovery pass); the node answers chunk n with an
error (rotate, sync, add_node, remove_node): pass. Two nodes failing at two positions of one `rotate_secret`
(9 x 9 positions, silent and closed, 162 calls): no success without a warning with a different secret on some node,
no error that left a mixture, nothing prepared after one recovery pass.

## The fate of S12-01 to S12-06

| ID | State | Evidence |
|---|---|---|
| S12-01 (high) | **Fixed for the autocommit call; the same class remains for a call inside a subtransaction (S13-01)** | `node_conn.c:1716-1748` (leave sends nothing), `1060-1096` (discard only where told). Mutant M1 (a `RESET lock_timeout` at the leave whose failure discards the session): `t/100` fails 30 of 160. Sweep over every chunk for all seven calls passes on both majors; my close, lost-answer and error modes pass too. |
| S12-02 (medium) | Fixed | README bounds table names `t/100` to `t/104`; `t/102` runs all calls but `promote_node` and `t/103` runs it, at 0.9 x `node_timeout` with one slow node (default run). |
| S12-03 (medium) | Fixed by design; the price is S13-02 | `node_conn.c:407-416`, `1556-1635`: no budget, one wait per statement. Mutant M4 (the per-statement bound halved): `t/102` fails 29 of 64. |
| S12-04 (low) | Fixed | `node_admin.c:1326-1355` (`clear_removed_node`). Mutant M2 (rethrow instead of abandon): `t/100` fails 2 of 167 (the `no_error_like` of `remove_node`). My lost-answer and close sweeps of `remove_node`: success with a warning, node removed, nothing prepared, except the lost `PREPARE` answer. |
| S12-05 (low) | Fixed | `forward_auth.c:243-250` (own SQLSTATE `KS014` for a proof that does not match; `28000` and "holds no cluster secret" for the other cause), `node_conn.c:762`, `failover.c:121-123`. Mutant M3 (the node refuses a bad proof with `28000` again): `t/104` fails 1 of 40. |
| S12-06 (low) | Fixed (a precise error and the statement to run; no update path, as said) | `node_admin.c:994-1017`, `recovery.c:796`. Mutant M5 (check removed): `t/104` fails 2 of 40. |

A mutant that returns success after losing work (M7: a deadline of a statement with parameters is downgraded to a
`WARNING` and the call goes on without the node) is caught by the sweep judge: `rotate_secret` 15 of 75 tests,
`sync_catalog` 1 of 79, `add_node`/`remove_node` 1 of 165. The judge is therefore not blind to that class. (My first
"lost work" mutant, M6, was not one: the session was dropped and the next statement failed with an error; the file
died instead of failing a test. I do not count it.)

## New findings

Summary: 0 critical, 1 high, 2 medium, 3 low, 2 notes.

| ID | Severity | Summary |
|---|---|---|
| S13-01 | high | A control call inside a subtransaction (`SAVEPOINT`, `EXCEPTION` block, driver autosave, `ON_ERROR_ROLLBACK`) sends `RELEASE SAVEPOINT` to the nodes after the call; that wait is unbounded and not covered by `statement_timeout`; a node silent at that statement hangs the backend, holds the catalog lock and refuses every other control call with `KS008` |
| S13-02 | medium | No bound for a call that is working: `s x node_timeout` per node, locks held on every node meanwhile; README line 2431 still says every call returns within its bound |
| S13-03 | medium | The proof's own lock wait is not covered by the "first statement" `lock_timeout`; the README row for `55P03` is wrong for the common case |
| S13-04 | low | `KS014` is decided by a fact the node reports but says nothing about which side is stale; the advice names the target |
| S13-05 | low | The sweep tests do not cover a call that is not in autocommit, a lost answer, a closed connection, an error, or two nodes; the bound assertion is 12 s wider than the README |
| S13-06 | low | `remove_node` of a stuck node leaves it holding the cluster secret; the warning does not say to rotate |
| N1 | note | The message of a failed prepare names a node registered by socket path as the socket directory |
| N2 | note | The default run no longer covers two and four slow nodes and 0.5 x `node_timeout` (long matrix only) |

### S13-01 (high) A hung call after the call: `RELEASE SAVEPOINT` is sent at depth 0

Where: `conn_subxact_callback()` (`node_conn.c:3107-3189`, line 3133 `exec_internal(kc, sql)` for
`RELEASE SAVEPOINT s%d`), `exec_internal()` (`1855`) and `control_get_result()` (`1818-1833`). When the subtransaction
commits, the call has been left, `control_depth` is 0, and `control_get_result()` takes the unbounded path
(`libpqsrv_get_result_last`, line 1825). The README says "a call sends nothing to a node after its last piece of
work" (line 1906) and "the calls of this chapter do not wait for ever" (line 2431).

Steps (18.7 and 19rc2, `node_timeout` 2, three nodes, the third behind the forwarder, a distributed table so the
sessions are the ones of a real cluster), the forwarder silent after chunk 10 of a session (the chunk that carries
`RELEASE SAVEPOINT s2`; positions 11 to 14 give `08006` at `PREPARE`, the `COMMIT PREPARED` warning, and the standby
warning, as documented):

    BEGIN; SAVEPOINT a; SELECT kingres.sync_catalog(); RELEASE a; COMMIT;

    client waited 12 to 30 s, then left (client timeout).
    pg_stat_activity of the origin: the backend is still active, wait event Extension
    another control call:  ERROR KS008 "another call that changes the catalog is running on this node"
                           (repeated until the backend is ended)
    SET statement_timeout = '4s' before the call: the call is not ended (30.8 s, the client gave up)
    pg_cancel_backend(pid): the backend ends within 4 s

The statement timer is off while the commit of a statement runs (it is disarmed before the end of the command is
processed), so the caller's own `statement_timeout` does not bound the wait, and a client that has gone away does
not end it either (the backend only watches the node's socket). The only exit is a cancel from another session.
While it hangs, the backend holds the lock of `kingres.map_version` taken by the call, so every other call that
changes the catalog is refused with `KS008`: one silent node at one statement stalls the whole control plane. Any
driver that wraps statements in savepoints (psql with `ON_ERROR_ROLLBACK`, JDBC `autosave`) or a PL/pgSQL retry loop
with an `EXCEPTION` block (the README's repeat loop of `promote_node` is exactly what a control plane would write so)
creates the subtransaction. `ROLLBACK TO` is bounded (`bounded_exec`, line 3165); `RELEASE` is not. By reading,
`precommit()` has the same unbounded `COMMIT TRANSACTION` (`2744`, `2758`) for a read-only or single-writer session of
the transaction; the eight calls always write locally, so their remote writers go through `PREPARE`, which is bounded;
I did not reproduce that one.

Why the sweep missed it: `t/100` and `t/101` run every call in autocommit; the position that matters is chunk 10 of a
call that is wrapped in a savepoint, which no test makes.

A fix must achieve: a statement that the extension sends to a node for a session that carries a control call's work
(`RELEASE`, `COMMIT TRANSACTION`, `ROLLBACK TO`) waits at most `node_timeout`, whatever `control_depth` is, and a
failure is an error that names the node (the transaction then aborts and the remote part is rolled back with the
session); the sweep gets a variant that runs every call inside `SAVEPOINT`/`RELEASE` and inside a PL/pgSQL block
with an `EXCEPTION` clause; the README line 2431 is then true as written.

### S13-02 (medium) A call that is working has no bound, and holds locks for `s x T`

The standard of the round is one deadline per statement. A node that answers every statement just inside the bound
keeps a call running without limit in the sense of the README: the bound is `s x T` (README lines 1914-1925). With
`t/102` at `T` = 3 and one node answering every statement at 0.9 x `T`: `sync_catalog` 32.5 s, `rotate_secret`
29.8 s, `add_node` 40.6 s, `create_reference_table` of a large table 35.9 s, `promote_node` 54.1 s. At the default
`node_timeout` (60 s) the same node gives `sync_catalog` about 11 minutes and `promote_node` about 18 minutes. During
that time: the origin holds `ExclusiveLock` on `kingres.map_version` (every other control call is `KS008` after `T`,
and bucket moves and DDL that take the catalog lock wait), and every other participant holds its remote transaction
(catalog rows, new tables) open and not yet prepared. The README says it honestly for the formula, but line 2431
("every call returns within its bound ... a call still running after that bound plus 10 s has a local reason") and
the client-timeout row of the bounds table are written for the older bound and mislead a control plane that sizes its
client timeout from them. A fix must achieve: either a wall-clock bound for a call (a total, with its own code that
does not say "fence the node"), or a statement in the README that a slow node holds the catalog lock and every
participant's locks for `s x T` and that the control plane must cancel such a call from another session; line 2431 is
corrected.

### S13-03 (medium) The "first statement" `lock_timeout` does not cover the proof

README `55P03` row and `node_conn.c:372-377`, `638-692`: the first statement of a session of a call "runs with a
`lock_timeout` of three quarters of `T`". The proof of the session is checked by the node at its first statement,
which is the `SET lock_timeout` itself, so the wait for a lock that the proof needs is not under it. Own case
(`node_timeout` 2, a session of the node holds `ACCESS EXCLUSIVE` on `kingres.cluster_secret`, `sync_catalog` from the
map node), role `lock_timeout` unset, `0` or `10s`: `KS007` after 2.0 s ("accepted the session and did not answer its
first command"); with the role at `500ms`: `55P03` after 0.5 s. The text "its first query did not get a lock within
its lock timeout" is reachable only when the administrator gave the role a short `lock_timeout`. Not a hazard (the
state is clean in every case, the repeat after release works), but the README row and the test (`t/086`, `t/104`
test locks that the statement, not the proof, waits for) say a thing that is false for the proof. A fix must achieve:
either the rows say "the proof's own lock wait is `KS007`" or the `SET` is sent by a statement the proof does not
precede (set it in the connection options).

The other cases of the question on `lock_timeout` held (each run on 18.7, `node_timeout` 2): a lock wait on a later
statement with role `lock_timeout` `1s`: `55P03` after 1.0 s; `10s`, `0` and unset: `KS007` after 2.0 s; the
database's own `lock_timeout` of 5 s comes back after a control call on the same connection and a user `UPDATE`
behind a lock waits 5.0 s (and 7.0 s under the user's `statement_timeout` on a node with none); a lock wait that ends at the
bound (role `lock_timeout` 1900, 2000, 2100 ms): `55P03` x6; `55P03` x5 and `KS007` x1; `KS007` x6; nothing prepared
and the repeat succeeds in every run. Either answer is acceptable at the race.

### S13-04 (low) `KS014` does not say which side holds the foreign secret

`KS014` is now what the node says (`forward_auth.c:243-250`), which settles the S12-05 case of two causes with the
same words. It cannot say whether the node or the caller is behind. For a caller that is the map primary the caller's
secret is current (the rotation runs there), so "the node holds a secret no node has" is right. For `promote_node` of
a map standby (sequence 3, the call runs on the standby itself) the caller can be the node that missed the
rotation; every other node then refuses its proof, the first refusal names a healthy node and the advice is to
rebuild that node. By reading only (I did not build a map group with a streaming standby that missed a rotation).
I did produce the node-side facts: after a rotation whose second phase was lost on one participant (a `WARNING`
"recovery will commit it", one prepared transaction), that participant holds the old secret and its own statements to
the others end with `08006` "FATAL ... cannot prove it" (the data path does not carry the code, as before); the
others reach it with the previous secret. A node that is slow (`KS007`), restarted (a connect error) or holds the
previous secret (reached with the previous secret) was never misread as `KS014` in my cases. A fix must achieve: the
text of `KS014` names both possibilities, or the control call asks the other direction (the version of the
secret, a hash) before it names a node.

### S13-05 (low) What the sweep does not cover

(a) Every position is swept with the forwarder silent only; the closed connection, the lost answer and the error
answer at each position pass with my copy of the judge (above), so the standard holds, but nothing in the repository
runs them. (b) One node is faulted at a time; two nodes at two positions pass in my 162 calls of `rotate_secret`,
nothing runs them for the others. (c) `bound` is `2 x T + 12 + C + 8` in `t/100` (26 s at `T` = 2); the README's
documented bound is `2 x T + 8 + C` (14 s) and the slowest measured is 6.2 s, so a regression of 8 s would pass.
(d) The judge accepts a success with a `WARNING` when the text contains "node" or "recovery" and the state after the
repeat is right; it does not check that the warning names the node that was left. (e) `state` of `sync_catalog` is the
row count of one table; a call that loses another catalog table on a node passes. (f) `sleep 0.5` decides when the
state is read; a slow node that is still rolling back reads as `locked` and fails the test (a false failure, not a
false pass). (g) The case of S13-01.

### S13-06 (low) `remove_node` of a stuck node keeps the secret on it

The removed node is left with its catalog and the current cluster secret (README row, sequence 8, says to reset it
when it is back). Until then it can open sessions to every node with a valid proof. The row does not say to run
`rotate_secret` (which excludes it, being no longer in the catalog) or that the removed node is trusted until then.
The pending row for it lives one hour; recovery visits it at `LOG` level only. Repeating the call is safe (`42704`,
node does not exist).

### Notes

N1. An error that names a node registered by a socket path shows the socket directory (`could not prepare the
transaction on node /tmp/...:59809`). N2. The default `t/102` and `t/103` run one slow node at 0.9 x `T`; two and
four slow nodes and 0.5 x `T` need `KINGRES_LONG_TESTS=1` (the README says so). Nothing else in the round's tests
skips; no fixed sleep decides a verdict except the one in S13-05(f).

## Answers to the questions of the brief

Node that answers each statement just inside the bound: the call runs without a wall-clock limit (S13-02); the
catalog lock of the origin and the remote transactions of the other nodes are held meanwhile. A node slow only at
`COMMIT PREPARED`: a warning and recovery, as documented (`t/100` positions 12 and 13). A node that closes the
connection: error or warning, same as silence, in every position of four calls. A node that answers with an error:
same. Two nodes: no bad outcome. A call cancelled by the caller or a backend terminated at a position: abort callbacks
cancel and discard; with a hang, `pg_cancel_backend` ends the call within 4 s. A call in a transaction block that is
rolled back (`BEGIN; rotate_secret; ROLLBACK`, and `SAVEPOINT; rotate_secret; ROLLBACK TO`): secrets unchanged on all
nodes; `PREPARE TRANSACTION` of the caller's transaction: refused with the documented text and nothing changed.

What the removal of the eager `RESET` changed elsewhere: nothing relied on it. Bucket moves set their own
`SET LOCAL lock_timeout` (`bucket_move.c:1152`, `1165`), the recovery worker sets its own session settings
(`recovery.c:893`), forwarded user statements never had one. The probe's `SET lock_timeout`/`RESET` is one simple
query in one implicit transaction, so nothing is left on the session (checked: the database's own value is back).
After a statement is abandoned the session is closed, not reused.

The data path (item 5) is as it was: a user write to a node whose session is killed before `COMMIT` fails the commit
with `could not prepare the transaction on node ...` and changes nothing on any node; the same with a control call in
the middle; a control call alone (`rotate_secret`, `sync_catalog`) with the session of one participant killed fails
the commit, the secrets are equal and unchanged, nothing prepared. Both majors. The S11 finding is not reopened.

## What held under attack

- The standard of a control call at every chunk of every session for the seven calls, in four fault modes (silence,
  closed connection, lost answer, error answer), 18.7; silence on both majors.
- Two nodes failing at different positions of one call (162 calls).
- The node's own `lock_timeout` (role, database) in every order I tried; a lock wait that ends at the bound.
- Both majors from one source: the only `PG_VERSION_NUM` tests in `src/` are in `src/pg_compat.h`; `t/098` enforces it.
- Mutants M1 to M5 and M7 are killed by the test that claims the fix.
- Rollback and prepare of the caller's transaction around a call.

## Verdict

A control plane can rely on the calls as documented when it calls them in autocommit: every position of silence,
closed connection, lost answer and error answer that I tried ends in an error or a warning that says what was left,
and in the state the README names. It cannot rely on "every call returns within its bound" when the call runs
inside a savepoint or an exception block (S13-01): one silent node can hang the backend, immune to
`statement_timeout`, with the catalog lock held, until someone cancels the backend. Until S13-01 is fixed the
control plane must call in autocommit and set its own client deadline with a cancel from a second session; for S13-02 it
must size that deadline from `s x T` and not from the older bound.

## Not checked

- `t/001` to `t/092` (neither major); the long matrix (`KINGRES_LONG_TESTS=1`); k = 4 nodes; 19rc2 for the closed,
  lost-answer and error modes and for the two-node case.
- A map group with a streaming standby that missed a rotation (S13-04, by reading).
- The unbounded `COMMIT TRANSACTION` of a read-only session in a control call's transaction (by reading).
- `promote_node` and `recover_prepared_transactions` under my four fault modes (only `t/101` silence).
- A node whose postmaster is stopped, a name that does not resolve, a very large copy under `s x T`.
- The leak of the `msg` of a stuck node (`list_free_deep(stuck_nodes)` does not free it, a few dozen bytes per event
  in a long-lived backend; by reading, not measured).
