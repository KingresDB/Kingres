<!-- SPDX-License-Identifier: AGPL-3.0-only -->
# Adversarial review: contrib/kingres_shard 0.5, the fix round for the second review of the call sequences (S10-01 to S10-08)

Reviewer: an independent reviewer (not the author of the code, not the reviewer of the earlier
rounds). Findings of this review are numbered S11-nn (the previous review,
`2026-11-16-kingres-shard-0.5-second-sequences.md`, used S10-nn).

## Scope and method

Reviewed state: the last commit that touches `contrib/kingres_shard` (the fix round that answers
S10-01 to S10-08), exported with `git archive` of `contrib/kingres_shard`, `kingres/dev` and
`kingres/docs` into a private area. The working tree was not used for any result. Nothing in the
repository was changed except this file.

Read first: `2026-11-16-kingres-shard-0.5-second-sequences.md` in full (findings, "For the control
plane", "Not reproduced", "Not covered"). Read in the code: the whole diff of the round (`ha.c`,
`node_conn.c`, `node_admin.c`, `failover.c`, `recovery.c`, `term.c`, `dist_table.c`, `bucket_move.c`,
`kingres_shard.h`), the callers of `kingres_shard_exec_params`, `remote_scan.c` (`fetch_remote`),
`fault.c`, the install script for the new functions, the chapter "What the control plane calls" of the
README (all of it), the scanner of PostgreSQL's `synchronous_standby_names` (`syncrep_scanner.l`) of the
18 branch, and all the new and changed tests (`t/085` to `t/092`, `t/071`, `t/072`, `t/078`, `t/080`,
`t/084`).

Environment: private relocated copies of the two installations with assertions (18.7 and 19rc2; nothing
written into the originals); the extension built out of tree with `kingres/dev/ext.sh`, one clean build
per major with `EXT_WERROR=1`, `-j4`, under `nice`; the port range of the Perl cluster module narrowed to
59900-59999 in my copies, one reservation directory for all my jobs, every server stopped afterwards. The
baseline (the whole suite once per major) ran first, at the same time on both majors; then my own TAP
files, 23 of them, in clusters of one to six nodes with real streaming standbys, run on 18 first and on
19 when the result mattered (most of them on both). For the deadlines I put a small TCP proxy of my own
(a script that forwards bytes, and can be told to delay each statement, to swallow all traffic after the
first N messages of a connection, or to accept and swallow from the start) in front of the nodes that
the cluster registers, and SIGSTOP on backends where a proxy cannot reproduce the situation. To audit
the tests as tests I built mutated copies of the extension (one line broken per copy, against a private
copy of the 18 installation) and ran the test that claims the fix.

"Reproduced" means I observed it, with the output given. "By reading only" means I did not run it.
Where timing mattered I made it deterministic (a stopped backend, a proxy, a fault point that waits on a
file) or counted runs.

## Baseline

| Major | Regression tests | TAP files | TAP tests | Failed | Wall time |
|---|---|---|---|---|---|
| 18.7 (assertions) | 12 of 12 | 92 | 7640 | 0 | 1562 s |
| 19rc2 (assertions) | 12 of 12 | 92 | 8298 | 0 | 1575 s |

No compiler warning with `-Werror` on either major. Both runs happened at the same time, on a machine
with a load average of 5 to 6 at the start. The commit message of the round says 7670 and 8328 tests;
both of my totals are 30 lower and every file passed (see note N2). Everything the earlier reviews found
and that has a test in this suite (the 92 files include the regression files of all earlier rounds)
stayed fixed; I did not repeat the 30-round streams of the previous review.

## Summary

New findings: 2 critical, 2 high, 4 medium, 4 low, 5 notes.

| ID | Severity | Summary |
|---|---|---|
| S11-01 | critical | The control-call deadline stays switched on for the rest of the transaction that made a control call. A user statement that is sent to a node that does not answer within `node_timeout` is then dropped with only a `WARNING`, its session is closed (rolling back every earlier write of the transaction on that node), and the transaction commits: writes are lost silently, and a transaction that wrote on two nodes commits on one (atomicity broken). Deterministic, both majors |
| S11-02 | high | The same switch is left on when a control call fails before the backend has made its first remote connection (a call sent to a node that is not the map primary, `KS008`, a refusal): the next transaction of the session is bounded, its remote lock waits are cut at 0.75 x `node_timeout` with `55P03`, and a statement that hits the deadline is dropped silently as in S11-01 |
| S11-03 | critical | `switchover_verify` aborts the backend (assertion) for every position from `0/18` to `0/1FF8`; the round closed `0/1` only. Both majors. Critical by the rubric (server crash); on assertion builds only, by reading no crash without assertions |
| S11-04 | high | The previous secret is dropped on every primary by the first recovery pass that reaches them all. A standby that missed the rotation (down or lagging) and is then promoted cannot be reached by any node: `promote_node` answers `08006` "could not ask node N whether it is in recovery" for ever, with no reason; with the previous secret kept (before the pass) the same promotion works. Both majors |
| S11-05 | medium | `remove_node` killed after the origin's commit leaves a prepared transaction on the removed node for ever: recovery does not visit it (it is no longer in the catalog), `check_cluster()` is empty, `add_node` of it is refused ("already belongs to a cluster") and the prepared transaction holds 24 locks on its catalog tables. Both majors |
| S11-06 | medium | `internal_sync_name_matches` cuts the element of the list to 63 bytes (PostgreSQL does not): an element of 64 bytes or more answers `t` for a standby that PostgreSQL treats as not matching (it stays `async`). It also answers `t` for names that cannot be written bare in the list (the whole list is then rejected). Both majors |
| S11-07 | medium | There is no bound for a whole call. A node that answers every statement just before `node_timeout` makes `sync_catalog` take 30 s with `node_timeout` 3 (documented: 2 x T + 2 = 8 s per node that does not answer; the client timeout the README calls "always enough", k x 3 x T + 10 s = 19 s, is exceeded), `add_node` and `remove_node` with four such nodes 86 s and 85 s, `promote_node` 70 s (documented 27 s). With the default 60 s the same node costs minutes |
| S11-08 | medium | A `switchover_verify` that answers `t` can still lose acknowledged rows when the caller holds the position and state of an earlier clean stop and the old primary was started and stopped again while the new node was disconnected (reproduced: 10 acknowledged rows missing). The function cannot see it; the README never says that the stopped node must stay stopped, and that the control file must be read again just before `pg_promote()` |
| S11-09 | low | A node to promote that accepts the session and does not answer gives `08001` or `08006` ("could not ask node N whether it is in recovery", no reason), never `KS007`; the README row says `KS007` ("the same code path"), which is false |
| S11-10 | low | Survivors of the mutations: a third of the claims of the tests of this round are not established by them (the check of WAL beyond the record, of a shutdown checkpoint as the record, of the end of the timeline, the deadline of every statement after the first, the reset of the node's lock timeout) |
| S11-11 | low | `node_connect_timeout` does not bound a connection to a node registered by name when the resolver does not answer (libpq resolves before it starts its timer): 10 to 11 s per attempt with `connect_timeout` 2; and for the connect phase the documented per-node bound ignores the connect timeouts (`check_cluster()` 41 s with four such nodes, documented 24 s at `node_timeout` 3) |
| S11-12 | low | A forwarded `SELECT` that hits the deadline in a flagged transaction ends with `ERROR: remote node returned 0 columns, expected 2` (no SQLSTATE of the extension, text that blames the node's answer) |

Notes: N1 `pg_controldata` is translated; N2 the baseline totals differ from the commit message; N3
`promote_node` can end with `KS007` for a participant, the README row says `unreached`; N4 `55000` is
still the code of "the cluster is not initialized on this node"; N5 a proof asked after `pg_promote()` can
turn into `KS006` once NEW recycles the segment.

Rating. S11-01: by the rubric, an acknowledged transaction in which a write is lost and the other nodes
committed. The preconditions are ordinary: any control call in the same transaction (or an earlier failed
one in the session, S11-02) and a node that takes longer than `node_timeout` to answer one statement (a
stuck node, a statement that simply runs longer than 60 s). S11-03: a server crash is critical by the
rubric; the superuser who calls the function must give a position in the first 8 kB of the WAL, and only
assertion builds abort. S11-04: not a lost row, but a failover that the extension cannot complete and
cannot repair by any documented call.

No committed row lost or doubled by the extension's own call paths (no flag involved) was found; the
participant kills of `move_bucket`, `rotate_secret`, `sync_catalog` and `add_node` and the origin kills of
`remove_node` and `sync_catalog` converged (S11-05 is the one residue). No privilege problem was found.

## Fate of S10-01 to S10-08

| ID | Fate | Evidence |
|---|---|---|
| S10-01 (critical) | Fixed for the unclean stop; the stale proof of a clean restart cycle remains (S11-08); a crash class opened (S11-03) | `t/085` passes on both majors. My R01: an immediate stop with NEW behind, a kill, a stop during a checkpoint: `KS006`, never `t`. A clean stop with NEW disconnected: `f` for ever, rows missing, as the README says. With archive_mode on, a logical slot, `track_commit_timestamp`, `wal_log_hints` and (19) `summarize_wal` on OLD, the shutdown checkpoint is still the last record (the proof answers `t`, 400 of 400 rows after promotion, `t` again after promotion), on both majors. With NEW streaming, the stale position of an earlier clean stop is refused ("replayed WAL beyond the shutdown checkpoint"), before and after promotion. No leak in 70 000 failing calls in one session (flat memory contexts, file descriptors and resident size after the first batch). Crash: S11-03 |
| S10-02 (medium) | Fixed for a node that stops answering at a statement; not for a node that answers slowly (S11-07), a name that does not resolve (S11-11), and it opened S11-01 and S11-02 | measured table below: every call and every way of not answering that stops at a statement is within the documented bound at `node_timeout` 3 with up to four nodes (18 and 19), except two overruns of 0.1 s (`check_cluster()`, `add_node`/`remove_node` at late statements) |
| S10-03 (low) | Fixed, with a false `t` for elements of 64 bytes and more (S11-06) | `t/088`; my R03: 14 real walsender connections (mixed case, spaces, quotes, doubled quotes, `*`, `"*"`, numbers, keywords, a dollar sign, a trailing space, non-ASCII, 63 and 70 bytes) against 25 list elements: the answer equals the `sync_state` that PostgreSQL reports for every element of 63 bytes or less; `*` and `"*"` both match everything in PostgreSQL and in the function |
| S10-04 (low) | Fixed as designed; the design strands a standby (S11-04) | `t/087` passes; my R06 shows the wipe on every node and the consequence |
| S10-05 (low) | Mostly fixed; the `KS007` row for NEW is wrong (S11-09) and the `unreached` row of `promote_node` is not the only answer for a participant (N3) | rows read against runs |
| S10-06 (low) | Fixed (text); the sequence works as written | my R07, both majors: after a proven switchover OLD follows NEW without `pg_rewind` (170 of 170 rows, `timeline_id` of its control file 1, in recovery); `pg_rewind` of a crashed OLD with `wal_keep_size` 256MB: ok, the 10 rows only OLD had are gone, OLD's own configuration files must be put back (the module overwrote `postgresql.conf`); without `wal_keep_size` (the default of a test cluster) it fails with "could not find previous WAL record" in both of my runs, as the README says, and with the WAL since the divergence recycled by 6 checkpoints it fails the same way |
| S10-07 (note) | Fixed | `KS008` after 4.0 s for `rotate_secret`, `sync_catalog`, `add_node`, `promote_node` behind a call that was really running (held after its prepare); a plain read of the catalog meanwhile: 0.0 s; the holder terminated: nothing left prepared and the next call ran at once |
| S10-08 (note) | Fixed | `KS006` to `KS013` exist; mutation M9 (the code of `KS010` changed to `55000`) is caught by `t/091` |

## The tests of this round

How: for each fix, one line of the extension was broken in a private copy, the copy built, and the
test that names the fix run. M1 to M13 below; "caught" means the file fails.

| Mutation | Test | Result |
|---|---|---|
| M1 `switchover_verify` does not look at the state text | `t/085` | caught (2 of 55) |
| M2 the check "replayed WAL beyond the record" removed | `t/085` | **not caught** (all pass) |
| M3 the check "the record is a shutdown checkpoint" removed | `t/085` | **not caught** |
| M4 the check "NEW left the timeline at the end of the record" removed | `t/085` | **not caught** |
| M5 every statement of a control call waits for ever again (only the connect-phase waits stay bounded) | `t/086` | **not caught** |
| M6 the second caller waits for the catalog lock for ever again | `t/086` | caught, by hanging: the test did not end in 23 minutes (no timeout of its own on that call) |
| M7 the recovery pass does not tell the other primaries to drop the previous secret | `t/087` | caught (4 tests) |
| M8 `internal_sync_name_matches` compares exactly | `t/088` | caught (5 tests) |
| M9 the code of `KS010` is `55000` | `t/091`, `t/071` | caught (1 test) |
| M10 a node that did not answer is not remembered for the rest of the call | `t/086` | caught (1 assertion of 80) |
| M11 `recover_prepared_transactions` is not a control call | `t/086` | caught (2 tests) |
| M13 the sessions keep the node's `lock_timeout` after the transaction (`RESET` not sent) | `t/086`, `t/084` | **not caught** |

Per file:

- `t/085_switchover_stops.pl`. Claims: the proof is `t` only when no acknowledged row is missing, every
  other case is `f` or `KS006`. Preconditions are established well: every stop mode is run with rows
  counted on NEW after `pg_promote()` and a `t` with a missing row fails the file. What it does not
  separate: for the unclean stops the answer `KS006` comes from "the record is not a shutdown checkpoint"
  or from "replayed beyond", never from the state check alone or from the timeline check; M2, M3 and M4
  survive because another check always answers first. The stale-position case (S11-08) is not in it, nor a
  position in the first page (S11-03: its list of positions has `0/1`, `0/0`, a far one, never one in
  page 0), a record that ends at a page end, or the received-but-not-replayed WAL.
- `t/086_node_deadlines.pl`. Claims: every call returns within a bound and names the node. The bound it
  asserts is `2 x T + 8` s (and `3 x T + 8`), 14 s at `T` = 3, while the README says `2 x T + 2`: the margin
  is as large as the bound. The stopped-backend case SIGSTOPs sessions the call has cached, so the call
  runs into the deadline at its first statement (`SET lock_timeout`), which is bounded by its own code;
  M5 shows that the deadline of every later statement is never exercised (my proxy runs exercise it, and
  it holds). No case has more than one stuck node, none a slow node, none a node that is stuck from the
  connection on. The lock cases hold `ACCESS EXCLUSIVE` only on two catalog tables. The case for the
  second caller has no timeout and hangs when the fix is removed. M13 shows nothing checks that the node
  session gets its own `lock_timeout` back. The names of the cases promise "against a node that does
  not answer"; what is tested is "against a node whose first statement does not answer".
- `t/087_previous_secret.pl`. Establishes its precondition (the old tag verifies in the window, a pass that
  cannot reach every primary wipes nothing, the pass that reaches all wipes all) and fails when the fix is
  broken (M7). It does not test a standby (S11-04), a node added during the pass, or two rotations.
- `t/088_standby_names.pl`. Compares the function with the `sync_state` of one real standby for 11
  elements. Fails when broken (M8). The README promises the decision of PostgreSQL "for names ... cut to
  the name length" (the commit message): no element longer than 8 bytes is in it, no non-ASCII name, no
  doubled quote (S11-06).
- `t/089_kill_points.pl`. The origin is killed at six points for `rotate_secret` and `add_node`; the
  precondition (the node dies) is asserted. For `rotate_secret` the assertion "the secret is not the old
  one" is true after the repeated call whatever happened to the first one. It checks `prepared` on the new
  node for `add_node` but there is no case for `remove_node`, where the node that keeps a prepared
  transaction is the one that left the catalog (S11-05); the writes it makes ("distributed write") are
  single-key inserts, which commit on one node. No participant is killed (my R05: converged for
  `rotate_secret`, `sync_catalog`, `add_node`, `move_bucket`).
- `t/090_readd_old_primary.pl`. Runs the sequence as written, with `wal_keep_size` 256MB on OLD (the
  condition under which `pg_rewind` works, and which a default cluster does not meet). Its "fallback" case
  is a base backup on another address; it does not produce the failure of `pg_rewind` that the README
  names as the reason for the fallback, so the sentence "When it is not, `pg_rewind` stops with 'could not
  find previous WAL record'" has no test (I produced it, S10-06 row above).
- `t/091_answer_rows.pl`. Asserts the rows of the tables; the SQLSTATE rows fail when the code is changed
  (M9). The row "node N did not answer ... KS007 for NEW itself" says "not produced by a test" and is
  wrong (S11-09).
- `t/092_recovery_gone.pl`. Gates recovery at a fault point and resolves the prepared transaction by hand;
  the precondition is asserted ("recovery has listed the transaction"), commit and rollback both. It tests
  existing behaviour; there is no fix to break.
- The changed lines of `t/071`, `t/072`, `t/078`, `t/080`, `t/084` assert the new SQLSTATEs and names and
  add one case (a blocker that changes between two calls, `t/078`); read, not mutated. `t/080` now tests
  `switchover_replayed` and the new `verify`.

## Measured against the documented bounds

`node_timeout` 3 s (T), `node_connect_timeout` 5 s (default), five nodes: MAP and four groups, `k` of the
groups behind my proxy. Documented (README, "Bounds"): `add_node`, `remove_node`, `sync_catalog`,
`rotate_secret`, `recover_prepared_transactions`, `create_*_table`: `2 x T + 2` s per node that does not
answer (8 s for k = 1, 32 s for k = 4); `check_cluster()`: `2 x T` per node (6 s, 24 s); `promote_node`:
`3 x T` per node (9 s for k = 1, 27 s for k = 3). Seconds, 18.7; the 19rc2 numbers I took are the same
within a second (in the paragraph below the table). "EXC" = exceeds. The way of not answering is what my
proxy does after the first N messages of a connection: after 2 (the first command of the session is
not answered), after 4 (the first statement of the call), after 6, from the start (the connection phase),
a slow start (the startup message is held 12 s), and "slow": every statement is answered 2 s after it is
sent (so no single wait ever expires).

| call | after 2, k=1 / k=4 | after 4, k=1 / k=4 | after 6, k=1 / k=4 | connect phase, k=1 / k=4 | slow start, k=1 | slow answers, k=1 / k=4 |
|---|---|---|---|---|---|---|
| `sync_catalog` | 3.0 / 12.4 | 6.0 / 15.0 | 3.1 / 12.1 | 11.0 EXC / 26.4 | 10.1 EXC | 30.1 EXC / 64.1 EXC |
| `rotate_secret` | 3.0 / 12.0 | 6.1 / 24.1 | 3.1 / 3.1 | 10.1 EXC / 26.4 | 10.1 EXC | 28.1 EXC / 60.3 EXC |
| `recover_prepared_transactions` | 3.0 / 13.4 | 3.0 / 13.0 | 3.0 / 12.1 | 5.0 / 20.1 | 6.4 | 5.0 / 49.2 EXC |
| `create_reference_table` | 3.0 / 3.0 | 3.0 / 3.0 | 3.2 / 12.4 | 5.1 / 6.3 | 5.0 | 18.2 EXC / 9.0 |
| `create_distributed_table` | 3.0 / 3.0 | 3.0 / 3.0 | 3.2 / 12.2 | 5.0 / 5.0 | 5.0 | 19.5 EXC / 45.2 EXC |
| `add_node` | 3.1 / 12.1 | 6.1 / 15.1 | 8.1 EXC / 18.3 | 11.1 EXC / 26.4 | 10.1 EXC | 25.4 EXC / 86.0 EXC |
| `remove_node` | 3.0 / 12.0 | 6.1 / 15.1 | 8.1 EXC / 17.1 | 10.1 EXC / 25.1 | 11.4 EXC | 22.1 EXC / 84.6 EXC |
| `check_cluster()` | 6.1 EXC / 24.1 EXC | 0.1 / 0.1 | 0.1 / 0.1 | 10.1 EXC / 41.4 EXC | 11.4 EXC | 5.3 / 17.1 |
| `promote_node` (k = 1 / 3 other groups) | 9.1 (k=3) | 3.1 / 9.1 | 10.4 (k=3) | 10.1 (k=1) | not run | 70.6 EXC (k=3) |
| a second caller of a catalog call | 4.0 (KS008) | | | | | |

19rc2, taken for `after 4, k=4` (every call within the bound: `sync_catalog` 15.0, `rotate_secret` 24.1,
`add_node` 15.1, `remove_node` 15.6, `recover` 12.1) and `slow, k=1` (`sync_catalog` 31.4 EXC,
`rotate_secret` 19.1 EXC, `recover` 12.1 EXC, `add_node` 13.0 EXC, `remove_node` 22.1 EXC, `create_*`
18.2 and 19.6 EXC) and `promote_node` `slow, k=3` 70.7 EXC.

Reading the table. Where the node stops answering at a statement (columns 1 to 3) every call is within its
documented bound and ends with `KS007` naming the node (or `ok` with a `WARNING` for `recover`), with
nothing left prepared (the counts of prepared transactions per node after each combination that I looked
at were all 0); the
"remembered node" does what it claims (a call that touches the node twice pays once). The documented bound
does not hold (a) for a node that is slow without being stuck (S11-07), (b) at the connection phase and a slow
start (the bound has no term for `node_connect_timeout`, which is 5 s per attempt, twice per call, and is
larger than `2 x T` at `T` = 3; with the default `node_timeout` this column is harmless, S11-11), (c) for
`check_cluster()` by a tenth of a second at `T` = 3 (6.1 s for 6.0 and 24.1 s for 24.0: the bound has no
margin), and (d) for `add_node` and `remove_node` at the late statements (8.1 s for 8.0). The wrong code
is in S11-09. A node that does not answer in a different phase of `promote_node` (the 8th statement on)
ends the call with `KS007` and changes nothing (N3). A black hole after the connection is up (my proxy)
and a SIGSTOP of the backend gave the same answers; the postmaster stopped (SIGSTOP) and the forced
`lock_timeout` by `ALTER ROLE` I did not run (Not covered).

## Findings

### S11-01 (critical) A deadline of the control plane silently drops a user statement and half-commits the transaction

Steps (R02f, R02g, R02e; 18.7 and 19rc2; `node_timeout` 4): a cluster of MAP, node 2 (group 1) and
node 3 (group 2), `kv(id int primary key, val text)` distributed, one key owned by each group.
In a session on MAP:

```
BEGIN;
SELECT kingres.sync_catalog();                        -- any control call; create_reference_table() and
                                                      -- create_distributed_table() are the same
INSERT INTO kv VALUES (<key of group 2>, 'first-on-node3');
INSERT INTO kv VALUES (<key of group 1>, 'on-node2');
-- stop (SIGSTOP) the client backends of node 3
INSERT INTO kv VALUES (<another key of group 2>, 'second');   -- returns after 4.0 s
-- continue them
COMMIT;
```

Observed: the second insert returns after 4.0 s with `WARNING: node 3 (...) did not answer within 4 seconds`
and nothing else; `COMMIT` returns with no message and no error; afterwards `kv` has the row of node 2 and
neither row of node 3 (physical rows: map 0, node 2: 1, node 3: 0; nothing prepared anywhere). The same
transaction without the `sync_catalog()` call commits both rows of node 3 and the row of node 2. An
`UPDATE` of a row of node 3 in the same position returns after 4.0 s with the warning and the row keeps its
old value (R02f: "orig" after the client wrote "user_write"). A forwarded `SELECT` ends with an error with
a misleading text (S11-12). With a held row lock instead of a stopped node (a lock held 8 s on the row,
`node_timeout` 4, so the node's own lock timeout of 3 s should come first): in 2 of 12 rounds on 18 and 1
of 12 on 19 the deadline came first and the `UPDATE` was dropped silently; in the other rounds it ended
with `55P03` (an error, but not the behaviour of a user statement, which waits for the lock: 9.4 s and
success in the same setup without the control call).

Expected: README: "Queries and DML of users are not bounded by this." A user statement is never
cancelled by the control deadline; and, whatever bounds a statement, a statement that was cancelled is an
error that aborts the transaction, never a warning that lets it commit, and closing the session of a node
that holds the transaction's earlier writes must make the commit fail.

Place and cause: `kingres_shard_control_begin()` (called by `sync_catalog`, `rotate_secret`, `add_node`,
`remove_node`, `promote_node`, `recover_prepared_transactions`, `create_distributed_table`,
`create_reference_table`) sets the global `kingres_shard_control_active`, which is cleared only by
`release_control_sessions()` at the end of the top-level transaction (`conn_xact_callback`). Every
`exec`, `exec_internal` and `exec_params` after it, including the ones that the planner's remote statement
and scan nodes make for the user, waits at most `node_timeout`. `remote_scan.c: fetch_remote` calls
`kingres_shard_exec_params(..., throw_on_error = false)`; on the deadline this raises only a `WARNING`
(`control_deadline(kc, sql, WARNING)`), returns NULL, and `discard_connection()` has already closed the
session and cleared `xact_depth` and `wrote`, so that the commit no longer knows the node took part;
`fetch_remote` then counts `PQcmdTuples(NULL)`, zero rows.

Direction: scope the deadline to the control function (set it on entry, restore in `PG_FINALLY`, a
counter so that nested calls work), not to the transaction; never let the data path use the control
deadline; make `control_deadline` an `ERROR` whenever the session held a write, and have
`discard_connection` of a session with `wrote` set mark the transaction as failed.

### S11-02 (high) The deadline switch is left on after a control call that fails early

Steps (R02, R02i; 18.7 and 19rc2): a session on node 2 (not the map node). `SELECT kingres.sync_catalog();`
answers `ERROR: KS010: this function can only run on the map group primary`. In the next transaction of
the same session: an `UPDATE` of a row of node 3 while a row lock on it is held on node 3 for 8 s,
`node_timeout` 4.

Observed: the `UPDATE` fails after 3.0 s with `ERROR: 55P03: lock not available on node 3 ... the node did
not get a lock within its lock timeout`; the transaction after that is normal (8.0 s, success). Without
the earlier call the first `UPDATE` waits 9.4 s and succeeds. With node 3's backend stopped instead of a
lock, the `UPDATE` of that next transaction is dropped with the warning and `COMMIT` succeeds (R02i: the
value stayed `user_write_plain`, the client believes `user_write_flagged` was committed). The same in the
same transaction when the control call is made inside a PL/pgSQL block whose exception handler swallows
its error (`DO $$ BEGIN BEGIN PERFORM kingres.sync_catalog(); EXCEPTION WHEN OTHERS THEN NULL; END;
UPDATE ...; END $$`: `55P03` after 3.0 s).

Expected: a failed control call leaves nothing behind.

Place: `conn_xact_callback` returns at its first line when `conn_hash == NULL` (no remote connection was
ever made by this backend), so `release_control_sessions()` is not called at the abort that follows the
early error and the flag stays true until the end of the next transaction that has a connection. Calls
that fail before any connection: wrong node (`KS010`), `KS008`, "not initialized", most refusals.

Direction: as S11-01 (a scoped flag); at least clear the flag in the callback before the `conn_hash`
check.

### S11-03 (critical by the rubric) `switchover_verify` aborts the backend for positions in the first page

Steps (R01b; 18.7 and 19rc2, both with assertions): on a standby NEW of a clean-stopped OLD, as
superuser: `SELECT kingres.switchover_verify('0/18', 'shut down')`.

Observed: `TRAP: failed Assert("wantPage != InvalidXLogRecPtr && wantPage % XLOG_BLCKSZ == 0"), File:
"xlogutils.c", Line: 737`; the backend ends with signal 6 and the postmaster restarts the node (with
`restart_after_crash` off, as the test clusters have it, the whole server ends). The same for `0/28`,
`0/40` and `0/1FF8`: every multiple of 8 from `0/18` to `0/1FF8`, which passes the new tests (`x % 8 == 0`
and `XRecOffIsValid`). Other malformed positions I tried were answered correctly: `0/8` and `0/10` and
page starts (`no WAL record can start at this position`), `0/2018` and `0/FFFFF8` (segment removed,
`KS006`), `1/0` (`KS006`), `1/18`, the last 8 and 16 bytes of the segment, the next segment, ten
segments ahead, `7FFFFFFF/FFFFFFF0` and `FFFFFFFF/FFFFFFF8` (`f`), the second and third long-header
offsets of the current segment (`KS006`), the middle of the previous record (`f`).

Expected: `KS006`, as for `0/1`. The round says "an assertion crash on '0/1' was found and closed": it
closed one position, not the class (the position is in the first page, whose address is
`InvalidXLogRecPtr`). Without assertions I expect `KS006` ("requested WAL segment ... has already been
removed", the answer of `0/2018`); I have no build without assertions to run it.

Place: `kingres_switchover_verify` in `ha.c`, before `XLogBeginRead`. Direction: refuse every position
below `XLOG_BLCKSZ` (and below the first segment of the node's own history), and call the reader only for
positions whose segment file exists.

### S11-04 (high) Dropping the previous secret strands a standby that missed the rotation

Steps (R06; 18.7 and 19rc2): three groups, a standby `SB` of group 1 registered, `SB` stopped,
`rotate_secret()` on MAP (rc 0), `recover_prepared_transactions()` on MAP (the pass that reaches every
primary: `previous` is NULL on all three primaries afterwards), OLD of group 1 stopped, `SB` started and
promoted, its synchronous list emptied, `promote_node(<SB>)` on MAP.

Observed: `ERROR: 08006: could not ask node 4 (...) whether it is in recovery` (no reason in the text),
instantly, on every repeat (3 of 3), `check_target_node`. `sync_catalog()` and `rotate_secret()` do not repair it
(they stop at the dead OLD first; I did not try to remove OLD and call them again). The same steps without the
recovery pass: `promote_node` answers `promoted`, then `already`, and `sync_catalog()` and `rotate_secret()`
succeed (a node that holds the old secret is reached with the previous one, which is what `previous` is
for).

Expected: the failover of S10 sequence 2 completes. The README says the previous secret "exists only
between a rotation and the first recovery pass that reached every primary": it does not say that a
standby of one of those primaries may still hold the older secret, and a pass by the worker does it
without the operator's knowledge. By reading: a standby that misses two rotations is out of reach
before the wipe as well (`previous` holds one secret).

Place: `recovery.c` (the loop of `internal_drop_previous_secret()`), the README paragraph of
"Postconditions". Direction: do not drop the previous secret until every registered standby of every
primary has replayed the rotation (`pg_stat_replication.replay_lsn` after the commit of the rotation);
give `promote_node` a way in for a node that holds another secret (the operator supplies it, or the call
says what to do); and give `check_target_node` the reason of the node's refusal.

### S11-05 (medium) `remove_node` killed after the commit leaves a prepared transaction on the removed node for ever

Steps (R05, R05c; 18.7 and 19rc2): cluster of 3 nodes plus a node X added as a primary of a new group
(no data); `SET kingres_shard.fault_injection = 'after_commit:kill'; SELECT kingres.remove_node(<X>)` on
MAP; MAP restarted; `recover_prepared_transactions()` three times on MAP; repeat of `remove_node`.

Observed: the node row is gone on MAP and on the others; X holds one prepared transaction
(`kingres_h0_1_...`) holding 24 locks on its catalog tables, and its catalog still lists the four nodes.
Recovery on MAP counts 2 resolved on its first pass and none after; `recover_prepared_transactions()` run
on X itself finds nothing to do; `check_cluster()` is empty; the repeated `remove_node` answers "node N
does not exist"; `add_node` of X is refused ("already belongs to a cluster"). The same at
`between_commit_prepared`. At the four points before the commit the repeat converged (X not removed
until the repeat; prepared 0 everywhere).

Expected: README, sequence 9: "nothing prepared, check_cluster() empty ... converges". The README says
for a `remove_node` of a node that does not answer that its own catalog "is not cleared" (a `WARNING`);
it does not say that a prepared transaction can stay on it, and `check_cluster()` cannot see it, because
the node left the list that every check walks.

Place: `remove_node` / recovery (`kingres_shard_recover_prepared_with` walks the nodes of the catalog).
Direction: remember the removed node until its part is resolved (recovery walks `pending` removed
nodes), or resolve it from the decision log by address; until then say in sequence 8 and 9 that the
operator must look for `kingres_h%` prepared transactions on the removed node and commit them.

### S11-06 (medium) `internal_sync_name_matches` is `t` for list elements that PostgreSQL does not match

Steps (R03; 18.7 and 19rc2): two walsender connections whose `application_name` is 63 `x` (and 70 `x`,
which PostgreSQL cuts to 63 at the startup packet: `pg_stat_replication` shows 63 for both); the list
`ANY 1 (<63 x>)`: both are `quorum` (right); the list `ANY 1 (<70 x>)` (bare, or in quotes): both stay
`async`.

Observed: `internal_sync_name_matches('<70 x>', '<63 x>')` and with the quoted element: `true` (two of two
rows). The function cuts the element to `NAMEDATALEN - 1` bytes ("as the list scanner cuts its
identifiers"); the scanner of the 18 branch (`syncrep_scanner.l`: `yylval->str = pstrdup(yytext)`, and for
quoted names the buffer) does not truncate. The false `t` is the dangerous direction: the operator puts a
name into the list that matches nothing, the standby is asynchronous, no commit waits for it.
Also: the function is `t` for `My-Standby` and `any` written bare, while PostgreSQL rejects the whole list
(`accepted = 0` in my run: the old list stays in force); a name that is not an identifier must be written
in double quotes and the README does not say so. Everything else I tried agreed with PostgreSQL on both
majors: mixed case, spaces in quotes, doubled quotes, `*` and `"*"` (both match all), digits, quoted
keywords, a dollar sign, a trailing space, non-ASCII bytes (a standby's name is cleaned to `\xHH` text by
the server before it is compared).

Place: `kingres_internal_sync_name_matches`. Direction: do not cut the element; say in the README that
names go in double quotes and are at most 63 bytes; refuse (`f` with a reason) a longer element.

### S11-07 (medium) No bound for a whole call: a slow node costs one `node_timeout` per statement

Steps (R04, R04b; 18.7 and 19rc2): the node's answers are held back 2 s each (`node_timeout` 3), so that no
wait ever expires.

Observed: see the table: `sync_catalog` 30.1 s (k=1; 19rc2: 31.4 s), `rotate_secret` 28.1 s, `add_node`
25.4 s, `remove_node` 22.1 s, `create_distributed_table` 19.5 s; with four such nodes `add_node` 86.0 s,
`remove_node` 84.6 s, `sync_catalog` 64.1 s (ended `KS007`), `rotate_secret` 60.3 s; `promote_node` with
three such nodes 70.6 s (19rc2: 70.7 s), with the answer `promoted`. The documented bounds are 8 s and 32 s
(27 s). The README's own client timeout, "`k x 3 x T + 10 s` is always enough", is 19 s for k = 1: a control
plane that follows it gives up on a call that was going to succeed and repeats it, and "gives up twice in a
row, stop and probe the primaries" then fences a node that answers.

Expected: the README: "A call that is still running after that bound plus 10 s has a local reason (MAP
itself is stuck)". A node that answers slowly, an overloaded node, is the usual case before a node
stops, and costs a call one `node_timeout` per statement (about 15 delayed round trips for `sync_catalog`, so
about 15 minutes at the default 60 s).

Place: `bounded_get_last` (the deadline is taken afresh for each statement). Direction: a deadline for the
call (a timestamp taken in `kingres_shard_control_begin`, each wait is `min(node_timeout, what is left)`),
and say what the bound is in the table.

### S11-08 (medium) The proof can say `t` after a later clean restart of OLD; the README does not tell the caller to prevent it

Steps (R01 E1, E2; 18.7): OLD with an asynchronous standby NEW; rows written; OLD stopped with `-m fast`;
the caller reads `Database cluster state: shut down` and `Latest checkpoint location` X1; NEW answers `t`.
NEW is disconnected (`primary_conninfo` invalid). OLD is started again (the control plane's retry, an
operator, a supervisor) and 10 more rows are written and acknowledged, then OLD is stopped with `-m fast`
again (X2). The caller calls `switchover_verify(X1, 'shut down')` with what it read first.

Observed: `t`. `switchover_verify(X2, 'shut down')`: `f`. After `pg_promote()` NEW has 150 rows, OLD
acknowledged 160. With NEW connected the same stale call is refused (KS006: "this node has replayed WAL
beyond the shutdown checkpoint"), before and after promotion (E2). With NEW paused by `pg_wal_replay_pause()`
the same `t` (E3b), but there the later WAL could not be made to arrive (E3: the walreceiver is not
restarted while replay is paused), so I could not make it true "with WAL on NEW's disk after the record". A
`recovery_target_lsn` stopped NEW one record after the checkpoint, which the function refuses
(`replayed > end`), so that variant is safe.

Expected: the commit message says "a stop, restart and crash of the old primary after the caller read its
control file cannot be seen by the function, so the caller reads the state last". That sentence is not
in the README, whose steps 3 to 6 say stop, read, call, promote, and nothing that keeps OLD from starting
in between (a supervisor with a restart policy, a second operator). The read of the state while a
postmaster is starting is the same window: by reading `StartupXLOG`, a node that was shut down cleanly
keeps `shut down` in its control file until recovery has opened it (not timed).

Place: README sequence 5 and precondition 1. Direction: write that OLD must be made unstartable (unit
masked, taken out of the supervisor's care) before step 3 and stay so until the end of the sequence; that
the state and the position are read after the proof and again just before `pg_promote()` and compared;
that `postmaster.pid` of OLD's directory must be absent; and that `pg_controldata` runs with `LC_ALL=C`
(N1). The function could additionally refuse when `GetWalRcvFlushRecPtr()` is beyond the record.

### S11-09 (low) A node to promote that does not answer: `08001` or `08006`, not `KS007`

Steps (R04c; 18.7): the standby to promote is registered through my proxy; after promotion in PostgreSQL
the proxy swallows the traffic after the first 2 / 3 / 4 messages of every connection; `node_timeout` 3;
`promote_node(<NEW>)` on MAP.

Observed (3.0 s each): first and second case: `ERROR: 08001: could not connect to node 3 (...), which is to
become the primary of group 1` with the detail "did not answer within 3 seconds: it accepted the session
and did not answer its first command"; third case: `ERROR: 08006: could not ask node 3 (...) whether it is
in recovery` and nothing else. Repeating after the node was healthy: `promoted`. The README row of sequence
2 says `KS007` "do not repeat ... the same code path", "not produced by a test"; the table of SQLSTATEs
says `08001`/class `08` means "repeat". The same `08006` without a reason is the answer of S11-04.

Place: `check_target_node` (`failover.c`). Direction: `KS007` and the node's text for every bounded wait
that fails there; a reason in the `08006` text.

### S11-10 (low) The suite does not detect what the mutations broke

See "The tests of this round": M2, M3, M4, M5 and M13 survive. Add: a case where only the shutdown
checkpoint check can answer (OLD runs, `CHECKPOINT`, NEW is caught up to the record exactly), one where only
the history check can (promoted NEW, X inside the timeline), one with a stale position (S11-08), one where
a statement after the first is not answered, one that looks at `lock_timeout` of a cached node session
after the transaction, and a timeout on the call that waits for the catalog lock.

### S11-11 (low) The connection phase has bounds of its own, and DNS has none

Steps: libpq with a resolver that does not answer (a private network namespace with a route to an
address that does not reply, `/etc/resolv.conf` replaced inside it): `psql "host=<name> connect_timeout=2"`:
11.4 s and 10.0 s for two attempts, error "Temporary failure in name resolution". The extension connects
with `PQconnectdbParams` in the foreground; the resolver is not interrupted by a cancel or by
`statement_timeout`. A node registered by name costs every call that touches it at least 10 s per connect
attempt, twice per call (the `hole` column shows two attempts per call), whatever `node_connect_timeout` is.
Also the connection phase is `2 x node_connect_timeout` per node and per call (5 s each, my `hole` column:
10 to 11 s per node) and the README's `2 x T + 2` has no such term; harmless at `T` = 60, a violation of
the stated bound for `T` below 5.

Direction: register nodes by address (or resolve with `hostaddr` and the timer); state the connect
terms in the bounds table.

### S11-12 (low) A forwarded `SELECT` that hit the deadline answers with a misleading error

Steps (R02h; 18.7): the setup of S11-01 with a `SELECT val FROM kv WHERE id = <key of group 2>` as the
statement that is not answered: `WARNING: node 3 ... did not answer within 4 seconds`, then `ERROR: remote
node returned 0 columns, expected 2`, no SQLSTATE of the extension. It is an error, which is better than
the `UPDATE`; the text sends the operator to the wrong place. Same cause as S11-01.

### Notes

N1. `pg_controldata` is translated (NLS): a control plane that runs it under a non-English locale reads
another word for `shut down` and the label of the line is another word too. The proof then answers `KS006`
for every planned switchover (the safe direction, a failover that was avoidable). The README must say
`LC_ALL=C`. By reading only.

N2. The totals of the baseline are 7640 and 8298; the commit message says 7670 and 8328. All files pass; I
did not find which file differs by 30 tests (a counted loop that depends on the environment, or the
integrator ran one more file).

N3. `promote_node` answers `unreached` for a participant that stops answering early (the `after 2` and
`after 4` columns: `"unreached": [2, 3, 4, 5]`, with `KS007` warnings), but a participant that answers the
first 8 or so messages and then stops ends the call with `ERROR: KS007` and changes nothing (the repeat
after healing: `promoted`). The README row "a node other than the one to promote -> unreached" is half of it.

N4. "the cluster is not initialized on this node" is `55000`, the same code as every input error; a caller
that sends a call to the wrong host cannot tell it from a mistake of the operator.

N5. A proof asked after `pg_promote()` can become `KS006` when NEW has recycled the segment: on 19rc2,
R09, the shutdown checkpoint of OLD 32 bytes before the end of a segment: `t` before the promotion, and
about a second after it `ERROR: KS006 ... the WAL at this position cannot be read on this node: requested
WAL segment ... has already been removed` (the end-of-recovery checkpoint of NEW removed the segment of the
old timeline). The README says "before or after `pg_promote()`" and "after the promotion the proof still
holds" (`t/085`, which runs it at once). The error is the safe direction, but a switchover that was
provable becomes "a failover": ask before `pg_promote()`, and say so.

## What held under attack

- `switchover_verify`: the unclean stops (immediate, kill, a stop during a checkpoint) are `KS006`; a clean
  stop with NEW behind is `f` and rows are indeed missing; the stale position with NEW connected is
  refused before and after promotion; the positions listed in S11-03 other than page 0; a position of
  another node's shutdown checkpoint, of an ordinary checkpoint, of the new timeline: not `t`; the
  non-superuser is refused; 70 000 failing calls in one session (7 positions, 10 000 each, inside
  exception blocks; and again on a promoted node) leave resident size, memory contexts and file
  descriptors flat after the first batch; a shutdown checkpoint 24 bytes before a page end: `t`, before and
  after promotion, on both majors; 32 bytes before a segment end: `t` before promotion on 19rc2 (18's
  run had NEW far behind, `f`), and `KS006` after promotion once the segment was recycled (N5); I did not
  hit the exact page or segment end (see Not covered); archive_mode, a logical slot, `track_commit_timestamp`, `wal_log_hints`,
  `summarize_wal` (19) on OLD: the checkpoint is the last record, the proof is `t`, no row missing.
- The deadlines of statements and of the first command of a session (the table), the memory of a node that
  did not answer, the `WARNING`s that name the nodes, nothing left prepared, repeats converge.
- `KS008`: 4.0 s for four different calls behind a call that was really running, 0.0 s for a plain read,
  the killed holder leaves nothing prepared, the next caller runs at once.
- Origin kills at the six commit points: `sync_catalog` (6 of 6 converged), `remove_node` (4 of 6; S11-05
  for the other two). Participant kills (a node killed while the origin waits at the point):
  `rotate_secret`, `sync_catalog`, `add_node` at five points each (15 of 15 converged: nothing prepared,
  catalogs equal, one secret, `check_cluster()` empty), and `move_bucket` with a third group's node killed at
  the five points of the switch's commit (18.7: rows 60 of 60 physically and through the scan, prepared 0,
  `check_cluster()` empty, the move `done` at four points, undone at the first).
- Sequence 6: the old primary follows after a proven switchover without `pg_rewind` (both majors); `pg_rewind`
  with `wal_keep_size` large; the rows only OLD had are gone; the configuration files must be put back.
- The privileges of the new functions (`permission denied` for an ordinary role), by the tests and by
  reading the `REVOKE`s.

## For the control plane

What `kingresd` must do, and must not assume, given the above; it is written against the calls as they
are today.

1. Never share a session. Every control call gets a session of its own, opened for it and closed after it,
   whatever the answer (S11-01, S11-02). Do not call `sync_catalog`, `rotate_secret`, `add_node`,
   `remove_node`, `promote_node`, `recover_prepared_transactions`, `create_*_table` inside a transaction
   that does anything else, and do not reuse a pooled session after one of them failed: its next
   transaction may be bounded and may lose a write silently.
2. Put the deadline on the client side, and do not use the README's formula. The extension bounds each
   statement and not the call (S11-07): use `max(2 x T + 2, ...) x nodes` as a floor for the first
   expiry, treat a call that is slow but answering as slow, not as stuck, and probe the primaries before
   fencing anything: a timeout is not a diagnosis. Never run two controllers (`KS008` after
   `node_timeout`).
3. Promotion of a standby after a rotation: before `promote_node`, compare what the standby has replayed
   with the commit of the last rotation; if the standby may have missed it, `promote_node` will fail with
   `08006` and no reason (S11-04) and nothing in the extension repairs it. Keep the recovery worker's
   pass (and your own call of `recover_prepared_transactions()`) from running after a rotation until
   every standby of every primary has replayed the rotation.
4. Planned switchover (S11-08, N1, N5): mask OLD's unit before the stop; stop with `-m fast`; run
   `pg_controldata` with `LC_ALL=C` on the stopped node and check `postmaster.pid` is absent; call
   `switchover_verify(X, state)` on the standby you will promote, before `pg_promote()` (after it the
   record can be recycled); read the control file again right before `pg_promote()` and compare; take every answer but `t` as "failover" (`f` for ever too); never pass a
   position read before the state; never call it with a position you computed. Do not call it with a
   position below 8192 (S11-03).
5. After any failed or killed `remove_node`: list `pg_prepared_xacts` of the removed node and commit
   the `kingres_h%` entries by hand (S11-05); `check_cluster()` will not show them.
6. Standby names: double-quote every name in `synchronous_standby_names`, keep them at 63 bytes or less,
   compare `pg_stat_replication.sync_state` after the reload instead of trusting
   `standby_name_matches` for a long name (S11-06).
7. Codes: match on `KS007`, `55P03` and `08001`/`08006` together for "this node does not answer or refuses
   the session"; do not repeat any of them without probing (S11-09, N3). `KS010`, `KS002` mean "wrong
   node or superseded: stop sending from here". Treat `WARNING` lines of `promote_node` and `recover` as
   information.
8. Nodes registered by name can stall a call for 10 s per attempt on a resolver that does not answer
   (S11-11): register by address.
9. The README's "repeat at most 10 times until `already`" held in every run; after `promoted` with
   `unreached` containing a node you did not expect, check the node, do not repeat.

## Not reproduced

- Paused replay with WAL beyond the record on NEW's disk: the walreceiver is not started again while
  replay is paused, so I could not make "replayed = end of the record, WAL follows" with a clean
  restart of OLD (S11-08, E3b gave the stale `t` but no WAL beyond); with a `recovery_target_lsn`
  NEW stops one record after the checkpoint and the function refuses.
- A standby promoted and demoted again, a cascading standby behind another, a standby restored from a base
  backup of another timeline with the same LSN: not constructed (the tests of `t/085` cover the cascading
  case and the position of another node; the history check rejects another timeline's record by reading).
- A shutdown that writes anything after the checkpoint: none found with archive_mode, logical slots,
  `summarize_wal`; I did not compare with the source of `ShutdownXLOG` and `CreateCheckPoint` of the 19
  branch line by line (I read the 18 one).
- The previous secret: a session that was authenticated with the previous secret and is still open when
  it is dropped (a session is not re-verified: by reading), two rotations in quick succession (by reading:
  the node that holds neither the current nor the previous secret cannot be reached), a node added during
  the pass.
- The flag across a prepared transaction or a cursor: `PREPARE TRANSACTION` after a control call is
  refused by the extension's own check when a remote transaction is open; a cursor's fetches use the scan's
  own bounded wait, not the flag. By reading only.
- A role whose `lock_timeout` is forced by `ALTER ROLE ... SET` on the nodes: the control session's `SET`
  overrides it and `RESET` returns to it, by reading; not run.

## Not covered

- A record that ends exactly at a page end or at a segment end: my tuning (messages of computed size) put
  the shutdown checkpoint 24 bytes short of a page end and 32 bytes short of a segment end, not on it.
  The two results above are for those near positions.
- A build without assertions (S11-03 expected behaviour is by reading).
- The postmaster stopped with SIGSTOP (new connections never served), a firewall that drops the packets
  instead of my proxy (the keepalive and `tcp_user_timeout` path), a lock on every catalog table of a
  participant (only through the earlier round's tests), 2 and 8 nodes of which several do not answer (I
  ran 1, 2, 3 and 4), the 30-round repeats of the earlier streams.
- Two callers on different nodes (a superseded map primary and the new one), the map group's failover in
  the presence of a flagged transaction, `promote_node` of the map group with a stuck participant.
- `rebalance`, the web panel, `kingresd` (not under review).
- Machine: 31 GB, 24 cores, load average of 3 to 11 while my jobs ran side by side; the port range of
  100 was the limit of my parallelism (a long TAP file keeps every port it ever took until it ends).

## Baseline numbers

18.7: 12 regression tests, 92 TAP files, 7640 tests, 0 failed, 1562 s. 19rc2: 12 regression tests, 92 TAP
files, 8298 tests, 0 failed, 1575 s. My own TAP files: 23 (the experiments above), about 14 of them on both
majors. Mutations: 12 built and run (M1 to M11 and M13; M10 was run twice, the first run did not start
its test and is not counted). Bounds: 13 combinations of way of not answering and number of nodes on 18
and 2 on 19 for the catalog calls, 7 on 18 and 2 on 19 for `promote_node`, 3 for a stuck node to promote.
