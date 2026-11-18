<!-- SPDX-License-Identifier: AGPL-3.0-only -->
# Sixth independent review of the call sequences of kingres_shard 0.5 (the fixes for S13)

Reviewer: an independent reviewer. Date: 2026-11-18.

## Scope and method

Reviewed state: the read-only export of `contrib/kingres_shard`, `kingres/docs` and `kingres/dev`. Nothing in the export
was changed; all work was done on copies. I read the fifth review first, then `src/node_conn.c` in full where it matters
(connect and probe 340-900, discard and conn state 960-1160, quiesce 1297-1352, abort path 2536-2601, prepare 2604-2817,
precommit/postcommit 2895-3195, both callbacks 3205-3417), the parts of `node_admin.c`, `failover.c`, `recovery.c` that open
raw sessions, `forward_auth.c`, the README chapter "What the control plane calls" (rows of S13-01 to S13-06), `t/107` to
`t/111`, `t/KsProxy.pm`, `t/KsSweep.pm`, and the PostgreSQL sources of `CommitSubTransaction`, `AbortSubTransaction`,
`RollbackAndReleaseCurrentSubTransaction`, `AbortCurrentTransactionInternal` (19rc2).

Two private copies of the installations were used (PostgreSQL 18.7 and 19rc2; the harness port range narrowed to a
private block). Both installations carry assertions (`USE_ASSERT_CHECKING` in `pg_config.h`; an assertion fired in one
of my runs). The extension was built from the export with `-Werror` after a clean build, on both majors: no warning.
Version tests outside `src/pg_compat.h`: none (grep over `src`, `t`, `sql`).

Own material: eleven TAP files built on a copy of `t/KsProxy.pm` and `t/KsSweep.pm` (unchanged modes), a user-transaction
sweep of my own (three transaction shapes with savepoints, four failure modes, every position), probes for nested
subtransactions, for the doomed state, for the memory over thousands of failures, for `control_call_timeout`, an
instrumented build that logs every `COMMIT TRANSACTION` of a control-work session, and mutants of the extension (one
change each, own installation, removed after the result was read).

Load: other work ran on the machine; no failure in this report is a load effect (every one was reproduced alone or is a
deterministic crash).

## What was run

| Major | What | Result |
|---|---|---|
| 18.7 | full suite `t/001` to `t/111` (111 files, default matrix) | 12434 tests, all pass, 3628 s |
| 19rc2 | full suite `t/001` to `t/111` (111 files, default matrix) | 13093 tests, all pass, 3695 s |
| 18.7 and 19rc2 | own user-transaction sweep: 3 shapes (ROLLBACK TO, PL/pgSQL EXCEPTION block, RELEASE) x failure at every position of a session to one node (closed, error answer at every chunk; silent and lost answer at every statement the extension bounds): 113 positions per major, plus clean runs | 119 tests per major, all pass; no mixture, no row of a rolled-back savepoint, nothing prepared after recovery |
| 18.7 | `t/107` against 4 mutants (below) | see "Test quality" |
| 18.7 | instrumented build (log line at every `COMMIT TRANSACTION` of a control-work session), 16 files: `t/009 010 020 025 029 049 050 059 064 069 080 085 086 093 096 100` | 1293 tests pass, 0 log lines |
| 18.7 and 19rc2 | crash probe (below) | PANIC on both |
| 18.7 and 19rc2 | memory probe (8300 failed RELEASEs) | growth on both |
| 18.7 | `control_call_timeout` probes | bound does not hold |

Not run by me: `KINGRES_LONG_TESTS=1` (every position of `t/108`, `t/109`, `t/110`, `t/111`), more than two nodes behind
forwarders in one call beyond what `t/111` does.

## The fate of S13-01 to S13-06

| ID | State | Evidence |
|---|---|---|
| S13-01 (high) | **Fixed as claimed for the cases it names; four new problems in the same code (S14-01, S14-03, S14-04, S14-05)** | `node_conn.c:3351-3360` (RELEASE bounded by `force_bound`, error names the node), `3395-3412` (ROLLBACK TO and RELEASE after it, warning, session closed), `1973-1997` (`control_get_result`), `2932`, `2946` (COMMIT bounded for control-work sessions). Run: `t/107` passes on both majors; my own sweep passes; a silent RELEASE ends in 2.0 s with KS007 naming `node 3 (127.0.0.1:port)`, the commit then fails with 08006 (my probe, 18.7). Mutant mA (RELEASE unbounded again): `t/107` dies at its first case (2 tests, the call hangs 120 s). The COMMIT bound of a control-work session is **unreachable**: my instrumented build over 16 files including all of `t/100` logged no such commit; the README says so and I confirm it (S14-06). A deadline after the call was left is not remembered: `control_deadline` (`1950`); retry loop probe: later iterations ask the node again and the transaction still cannot commit (as documented). |
| S13-02 (medium) | **Fixed for a call whose statements go through the extension's own sessions; the bound does not hold as stated (S14-02)** | `t/107`: KS015 at 6 s for L = 6; my probes: L = 1 with one slow node gives `rotate_secret` 16.2 s and `sync_catalog` 10.8 s; L = 2 with a node silent at its first statement gives 6.0 s, and the error says "between two statements". Recovery of 6 prepared transactions with a node answering at 1.8 s and L = 7 ends in 7.2 s (that part holds). |
| S13-03 (medium) | Fixed (rows say the proof's lock wait is KS007) | `node_conn.c:694-708`, README `KS007`/`55P03` rows; `t/086`, `t/104` pass. By reading and the suite; I did not re-run a proof-lock case of my own. |
| S13-04 (low) | Fixed for `promote_node` (`failover.c:130-146`: both possibilities named, advice to ask a third node). The add_node standby refusal (`node_admin.c:952-956`) still names only the node | by reading |
| S13-05 (low) | **Done, and works; weaknesses in S14-06** | `t/108` to `t/111` pass on both majors (165 s, 387 s, 365 s, 40 s on 18.7). The sample positions find no violation that my own full-position sweep of user transactions finds. |
| S13-06 (low) | Fixed | `node_admin.c:1314-1316`; `t/107` asserts the text (names the node, says rotate_secret) and passes |
| N1 (note) | **Partly fixed** (S14-07) | the prepare error names id and address; the other 40 messages that name a node do not |
| leak of the stuck text | Fixed | `free_stuck_nodes`; `t/107` memory case passes; a different leak was added next to it (S14-03) |

## New findings

Summary: 0 critical, 1 high, 2 medium, 3 low, 2 notes.

| ID | Severity | Summary |
|---|---|---|
| S14-01 | high | A node connection that breaks while the transaction is inside two nested subtransactions makes the rollback of the outer one raise `unexpected remote savepoint depth` from an abort callback, recursively, until `PANIC: ERRORDATA_STACK_SIZE exceeded`: the whole server restarts (both majors) |
| S14-02 | medium | `control_call_timeout` does not bound the call as the README says: raw sessions, the probe of a new session and the connect are outside it (16 s for L = 1); and KS015 then says "between two statements" and hides the KS007 of a silent node |
| S14-03 | medium | Every failed RELEASE or ROLLBACK TO leaks about 144 bytes in `TopMemoryContext` (`note_session_failed` formats the node label inside `TopMemoryContext`): a long-lived backend grows without bound |
| S14-04 | low | A `ROLLBACK TO` that a node does not answer dooms the transaction also when that session only read in READ COMMITTED: a refusal of something right |
| S14-05 | low | An error raised from the subtransaction-commit callback runs PostgreSQL's abort of a subtransaction in the state COMMIT: a visible `WARNING: AbortSubTransaction while in COMMIT state` on every caught failure, and the safety of the local state rests on the doomed note alone |
| S14-06 | low | Test quality: three mutants survive the round's tests (reader COMMIT bound, doom at RELEASE, doom at PREPARE); two assertions of `t/107` are vacuous or 5 s wide |
| S14-07 | note | 40 messages still name a node as `host:port`; for a node registered by socket directory they name the directory |
| S14-08 | note | A user's cancel that lands in the RELEASE wait dooms the transaction with the text "its session was closed", which is not true (by reading) |

### S14-01 (high) Two nested subtransactions and a broken node connection crash the server

Where: `conn_subxact_callback()`, `node_conn.c:3335-3338` (`if (kc->xact_depth > curlevel) elog(ERROR, "unexpected remote
savepoint depth")`) and `3386-3391` (a session that is already broken: `kc->have_error = true; continue;`, which leaves
`xact_depth` where it was). The round changed the neighbouring failure branches (`abort_savepoint_failed` closes the
session and sets the depth to 0) but not this one, and `t/107` and `t/109` do not reach it. I cannot diff against the
earlier state (export only), so I do not know whether the branch is older; it is in the callback the round rewrote and
it is reached by the round's own scenario (a node that stops answering inside savepoints).

Steps (18.7 and 19rc2, three nodes, the third behind the forwarder `KsProxy` with `close_after 6`, a distributed table,
`ON_ERROR_STOP=0`):

    BEGIN; SAVEPOINT a; SAVEPOINT b;
    SELECT count(*) FROM public.r_t;     -- the session to node 3 is at depth 3; the forwarder closes it at this statement
    ROLLBACK TO a;  SELECT 1;  COMMIT;

    ERROR:  XX000: unexpected remote savepoint depth   (repeated 5 times, each followed by
    WARNING: AbortSubTransaction while in ABORT state)
    PANIC:  XX000: ERRORDATA_STACK_SIZE exceeded
    server closed the connection unexpectedly
    postmaster: client backend terminated by signal 6

The same crash with `ROLLBACK;` instead of `ROLLBACK TO a`, and with a PL/pgSQL block:

    DO $$ BEGIN
      BEGIN
        BEGIN PERFORM count(*) FROM public.r_t;       -- node 3 closed under it
        EXCEPTION WHEN OTHERS THEN RAISE; END;
      EXCEPTION WHEN OTHERS THEN RAISE NOTICE 'caught %', SQLERRM; END;
    END $$;                                           -- PANIC, both majors

Why: a session that broke at depth d (d = the nesting level of the statement, here 3) keeps `xact_depth = d`. The abort of
level d leaves it alone (`have_error`, `continue`), the abort of level d-1 finds `xact_depth > curlevel` and raises an
`ERROR` inside `AbortSubTransaction`; PostgreSQL aborts again, the callback raises again, and the error stack overflows.
Two nested subtransactions are enough (two savepoints, or two PL/pgSQL blocks with an `EXCEPTION` clause, which ORMs and
retry loops write); the trigger is any loss of the connection after the session reached depth 3: a node restart, a reset,
`idle_in_transaction_session_timeout` on the node, an administrator's `pg_terminate_backend`. With one savepoint, or when
the connection is lost before the session reaches the depth, nothing happens (I ran both: `one_level` variant clean). Also
reproduced as a disconnect of the client in the aborted state (`FATAL` from the exit path, then an assertion in the
statistics code, signal 6).

A fix must achieve: no callback of a subtransaction raises an error (no `elog(ERROR)` in `conn_subxact_callback`; a depth
that does not fit is repaired to `curlevel - 1` and the session is marked lost), a broken session at any depth is brought
to depth 0 or marked so that every level above it skips it, a test that runs nested savepoints and nested EXCEPTION blocks
with the connection closed at every chunk after the session has reached depth 3 (modes `close_after` and `error_at`),
on both majors, with assertions.

### S14-02 (medium) `control_call_timeout` does not bound the call as stated, and KS015 can hide a silent node

Where: `node_conn.c:1973-1997` (the cut applies only to statements sent by `exec`, `exec_internal_ext`, `exec_params`);
`node_conn.c:690-699` (the probe of a new session uses `kingres_shard_bounded_exec`, a fixed `node_timeout`);
`node_conn.c:652` (`PQconnectdbParams`, `node_connect_timeout`, not cut); raw sessions that never see the budget:
`node_admin.c:154` (`require_primaries_can_commit`, every primary, at the start of `add_node`, `remove_node`,
`rotate_secret`, `sync_catalog`), `node_admin.c:909` (`ask_node`), `failover.c:189`, `216`, `254`. README lines 495 and
2015-2016 say the call ends "at most L plus the time to cancel one statement".

Steps (18.7; `node_timeout` 6, `node_connect_timeout` 30, the third node behind a forwarder that delays each answer after
its start by 5.4 s = 0.9 x T, L = `control_call_timeout`):

    SET kingres_shard.control_call_timeout = 1; SELECT kingres.rotate_secret();
      -> ERROR KS015 after 16.2 s      (L = 0: success after 59.4 s)
    SET kingres_shard.control_call_timeout = 1; SELECT kingres.sync_catalog();
      -> ERROR KS015 after 10.8 s      (L = 0: success after 64.8 s)

A node silent at the probe (forwarder silent after the startup packet), `node_timeout` 6, L = 2:

    ERROR  KS015: the call took longer than kingres_shard.control_call_timeout (2 seconds) as a whole
    DETAIL: The time ran out between two statements that it sends to nodes.  Nothing says that a node is dead or slow.
    after 6.0 s

A node silent at the connect (accepts, never answers the startup), `node_connect_timeout` 5, L = 1: KS015 after 5.0 s.
Two things are wrong: the bound is 3 to 16 times the limit, and the call that waited 6 s for a silent node is reported
as "the call took too long, do not fence a node": the KS007 that the same call gives without the setting is lost, and the
detail text ("between two statements") is false. (The path through the recovery loop holds: L = 7 ended at 7.2 s.)

A fix must achieve: every wait of a call is cut to what is left (raw sessions, probe, connect, `cancel_after_timeout`
second read), or the README states exactly which waits are outside; an error raised because the budget ran out while a
silent node was being waited for says KS007 for that node (or both), and the detail names the node.

### S14-03 (medium) Each failed RELEASE or ROLLBACK TO leaks in `TopMemoryContext`

Where: `note_session_failed()`, `node_conn.c:505-516`: `old = MemoryContextSwitchTo(TopMemoryContext)` and then
`psprintf("node %s did not complete %s, ...", conn_label(kc), what)`: the label that `conn_label()` allocates is built in
`TopMemoryContext` and never freed (only `session_lost_msg` itself is freed by `forget_session_lost`). The same pattern
is in `note_session_lost()` (`483-496`, the label from `node_label`). The round's own memory case (`t/107`) covers the
stuck-node text only.

Steps (18.7 and 19rc2, a script of `BEGIN; SAVEPOINT a; SELECT .. node 3; RELEASE a; COMMIT;` repeated, the forwarder closing
the session at the RELEASE; `pg_backend_memory_contexts` after 300 and after 8300 failures):

    TopMemoryContext used:  128944 -> 1281104 bytes (18.7);  130976 -> 1283136 (19rc2)   (about 144 bytes per failure)
    healthy control run (same script, no fault): flat
    mutant: the label computed before the context switch (3 lines): 85664 -> 85664, flat

A fix must achieve: no allocation of a temporary in the long-lived context (compute the label first, or build the
message with `MemoryContextAlloc` of the final text only), and a memory case that runs the failure of RELEASE and of
ROLLBACK TO, not only the stuck node.

### S14-04 (low) A read-only user session that did not answer `ROLLBACK TO` dooms the transaction

Steps (18.7): `BEGIN; SAVEPOINT a; SELECT count(*) FROM public.r_t; ROLLBACK TO a; SELECT 1; COMMIT;` with node 3 silent at
the `ROLLBACK TO SAVEPOINT` chunk: `WARNING KS007 ... did not complete ROLLBACK TO SAVEPOINT within 2 seconds`, the
statements after it run, and the COMMIT fails with `08006 ... did not complete ROLLBACK TO SAVEPOINT, and its session was
closed`. The session had only read, in READ COMMITTED; closing it loses nothing, and nothing needs the refusal
(`note_session_failed` is called for every session, `3396-3411`, whatever `wrote`, `wrote_user`, `control_work` and the
isolation level say). The README row says "the same" for user sessions, so this is documented; it is a new refusal of a
transaction that was right, in the case an ORM produces. A session that is already broken (`3386`) does not doom: the two
cases are not alike. A fix must achieve: the note only when the session held work that is lost (`wrote`, `wrote_user`,
a snapshot isolation level, or `control_work`).

### S14-05 (low) The subtransaction-commit callback raises an error in the state COMMIT

`CommitSubTransaction()` calls `SUBXACT_EVENT_COMMIT_SUB` after `s->state = TRANS_COMMIT`, after the child XIDs and the
deferred trigger events and the pending notifications were merged into the parent. An error from the callback then runs
`AbortSubTransaction()` in that state: PostgreSQL warns (`WARNING: 01000: AbortSubTransaction while in COMMIT state`, seen
on every caught failure in my runs: PL/pgSQL blocks, retry loop) and the parent receives the child XIDs and trigger events
of a subtransaction that is being rolled back. Today this is harmless only because the doomed note makes the outer
transaction unable to commit (my probes: local rows written in the failing block, in a committed child block and after the
catch were never committed, 3 modes, both an autocommit `DO` and an explicit transaction, `COMMIT` and `PREPARE
TRANSACTION`). That protection is a single condition, and mutant mC (the note removed from the `PG_CATCH`) survives the
round's tests (S14-06). A fix must achieve: the RELEASE is sent at `SUBXACT_EVENT_PRE_COMMIT_SUB`, where an error is
legal (as `postgres_fdw` does), so no local state of a rolled-back subtransaction is merged into its parent; the doomed
note stays as the second protection, with a test that catches the error in PL/pgSQL and goes on.

### S14-06 (low) Test quality

Mutants of my own, one change each, run against `t/107` on 18.7 (and the full suites are unchanged):

| Mutant | Result |
|---|---|
| mA: RELEASE in the commit callback unbounded again (`force_bound` false) | killed (the first silent case hangs; 2 tests, FAIL) |
| mB: the `COMMIT TRANSACTION` of reader and single-writer control sessions unbounded (`kc->control_work` replaced by `false`, both places) | **survives** (76 of 76 pass); the instrumented build shows no test and no call reaches the bounded branch |
| mC: no doomed note when RELEASE fails (the `note_session_failed` in the `PG_CATCH`) | **survives** (76 of 76): every case re-raises the error or ends the statement, so the note is not observable |
| mH: no `check_session_lost()` at `XACT_EVENT_PRE_PREPARE` | **survives** (76 of 76): no case prepares the caller's transaction |
| mD..: (the ROLLBACK TO note) | not run; `t/107` asserts the 08006 at COMMIT, so it is covered by reading |
| leak fix (a repair, not a mutant) | the memory probe goes flat; `t/107` does not notice either way |

My own probes kill mC and mH (doomed note, retry loop, COMMIT and PREPARE of the caller's transaction): they should be
added. Also: (a) `t/107` "a call that has used its time before it starts": `ok($rc == 0 || $err =~ /KS015/)` accepts both
outcomes, so a mutant that ignores the budget passes it; (b) the KS015 case allows `limit + T3 + 3` s (12 s for L = 6 and
T = 3), the README says L plus one cancel (about 7 s): a regression of 5 s passes; (c) the `plpgsql` variant of the sweeps
re-raises (`EXCEPTION WHEN OTHERS THEN RAISE`), so it never exercises "catch and go on"; (d) `error_at` on a statement
that spans several chunks becomes a protocol error and a closed connection (the module says so), so those positions
repeat `close_after` and the label "error answer" is not true for them; (e) the default positions are a sample: my full
sweep of user transactions found nothing the sample misses, but none of the round's tests runs a user transaction with
savepoints under faults (the whole of S14-01 is in that gap); (f) the verdict of the user-COMMIT case of `t/107` depends
on time (still waiting after 3 x the bound), which is sound only because the forwarder cannot answer.

### S14-07 (note) Names of nodes

`node_conn.c:1055`, `1061`, `1076`, `2293`, `2322`, `2324`, `2348`, `2357` (the context line `remote node host:port` of every
remote error), `2551`, `2561`, `2577`, `3108`, `3119`, `3163`, `3185` (the second-phase warnings), `bucket_move.c:192`,
`218`, `240`, `254`, `dist_scan.c:1013`, `1318`, `placement_check.c`, `node_admin.c:551`, `955`-`985`, `1120`, `1197`,
`1244`-`1265` all print `node host:port`. For a node registered by a socket directory the commit-phase warnings that an
operator acts on ("recovery will commit it") name the directory. The README sentence "errors that name a node print id
(host:port)" holds for the call-bound errors and the prepare error only.

### S14-08 (note) A cancel inside the RELEASE wait

`bounded_get_last_limit(..., interruptible = true)` is used for RELEASE; a cancel that arrives there raises from
the callback, and the `PG_CATCH` notes "node N did not complete RELEASE SAVEPOINT, and its session was closed" although
the session was not closed (the abort callback rolls it back normally). Harmless unless the caller catches
`query_canceled` and goes on, when the transaction is refused with a false text. By reading only.

## Answers to the questions of the brief

**Every wait of `node_conn.c`, its bound or the reason it has none.**

| Wait | Bound |
|---|---|
| connect (`PQconnectdbParams`, 652) | `node_connect_timeout`, per attempt; not interruptible, not cut by `control_call_timeout` (S14-02); several attempts per node are possible (other ids, previous secret) |
| probe `SELECT 1` / `SET lock_timeout..` (696-699) | `node_timeout`; not cut by the call budget (S14-02) |
| a control-call statement (`exec`, `exec_internal_ext`, `exec_params`, `control_get_result` 1990) | `node_timeout`, cut to the budget |
| the same statement of a user (`control_get_result` 1984, `libpqsrv_get_result_last`) | none, by design: a user statement is cancellable. My user sweep: a node that stops answering a data statement holds the backend until a cancel (confirmed at the 76th second) |
| scan fetches of a user (`dist_scan.c` 940, 1123, 1155) and `quiesce` (1299-1303), which reads a user's prefetch before any other statement on that session | none (user statement). **A control call that touches a session with a prefetch of a user's scan in flight inherits that wait** (by reading; not tested) |
| `RELEASE`, `ROLLBACK TO` (3353, 3393, 3404) | `node_timeout` for every session (S13-01, verified) |
| `COMMIT TRANSACTION` of a control-work session (2932, 2946) | `node_timeout`; unreachable today; the COMMIT of a session that carried only a user's work: none (documented, tested by `t/107`) |
| `PREPARE` batch (`prepare_collect`, 2620-2687) | `node_timeout`, the cancel, the answer to the question: bounded |
| `COMMIT PREPARED` (3115), the durability question (3179), `ROLLBACK PREPARED` / `ROLLBACK TRANSACTION` (2573, 2589) | `node_timeout` each, sequentially over the sessions |
| cancel of a statement (`kingres_shard_cancel_with_deadline`, 1575-1606; `libpqsrv_cancel` at 2547 with 5 s) | `max(1, min(node_connect_timeout, T/3))` s; 5 s per session in the abort path |
| sending a statement (`PQsendQuery`, 1731, 2037, 2078, 2128, 2751, 3158) | blocking mode: a statement larger than the socket buffers sent to a node that does not read waits for the kernel's `tcp_user_timeout` (`node_timeout`); the control calls send small statements; not tested |
| `PQfinish` (1117) | non-blocking set first; none needed |
| recovery `COMMIT PREPARED` (`exec_cancel_after_timeout`, 1744) | two reads of `node_timeout`; not cut by the call budget but the loop is (my probe: 7.2 s for L = 7) |

**A transaction on some groups and not others, or work of a rolled-back subtransaction.** My sweep of user transactions
(ROLLBACK TO, EXCEPTION block, RELEASE; closed connection, error answer, silent and lost answer at the bounded statements;
every position; 113 positions on each major): no mixture, no committed row of a rolled-back savepoint, no row lost in a
success, nothing prepared after at most three recovery passes. Doomed transactions: `COMMIT` and `PREPARE TRANSACTION`
both refused with 08006 naming the node (autocommit `DO`, explicit transaction, retry loop); the local rows written in
the failing block, in a committed child block and after the catch were never committed. Single-writer and read-only
paths: only reachable through the S14-06(mB) branch; read by me, not exercised. The one crash is S14-01.

**The doomed state leaking into the next transaction, or lost inside a subtransaction.** Not lost: `session_lost_msg` is
static in `TopMemoryContext` and a subtransaction abort does not clear it. Not leaking: `end_of_xact()` clears it on
commit, prepare and abort; after a doomed transaction the next distributed write on the same session committed. The
S14-03 leak is of a temporary, not of the note.

**Ordinary workload when every node is healthy.** 2000 PL/pgSQL blocks with an `EXCEPTION` clause (a read of three
nodes per block, and an update of two nodes per block): 2.2 ms and 1.9 ms per block, no error, no warning. The
RELEASE uses the same latch wait as before (a different function, the same round trip); I did not measure an older build.
New errors only where a node does not answer or answers an error at RELEASE or ROLLBACK TO (S14-04).

**Memory over thousands of failures.** S14-03 (144 bytes per failure, both majors). Healthy runs flat. The stuck-node text
is freed.

**Both majors, `-Werror`, assertions.** Clean build on both; full suite passes on both; the assertion build found the
extension's `PANIC` path and the statistics assertion that follows it (S14-01).

## What held under attack

- RELEASE, ROLLBACK TO and the PREPARE batch bounded as claimed on both majors; closed connection, lost answer, error
  answer, silence at every sampled position of seven calls (`t/108` to `t/111`, default) and at every position of my own
  user transactions.
- A failed RELEASE in a PL/pgSQL block with an `EXCEPTION` clause, caught, with local writes in the block and in a committed
  child block: the transaction cannot commit or be prepared; no local row survives (the doomed note works).
- A retry loop that catches the RELEASE failure and goes on: later iterations ask the node again and succeed; the
  transaction is refused at the end, as the README says; the next transaction is clean.
- `control_call_timeout` through the recovery loop (L = 7: 7.2 s) and through the statements of the extension's own sessions.
- `clear_removed_node` does not let a node go because of KS015 or a cancel (`node_admin.c:1356`); the `remove_node`
  warning names the node, its id and address, and says to rotate the secret.
- One source for both majors; `t/098` and my grep agree.

## Verdict

The fixes of S13 do what they claim for the statements they name: a node that does not answer at RELEASE, ROLLBACK TO or
PREPARE ends the call or the commit with an error or a warning that names it, within `node_timeout`, and the transaction
cannot commit work that a closed session took with it. That held in every position I swept, on both majors. It cannot be
accepted as the end of the track:

1. S14-01 is a server crash (PANIC, all backends restart) that two nested savepoints or two nested EXCEPTION blocks and a
   lost node connection produce, in the callback the round rewrote and in the scenario it was written for. A control plane
   or an ORM that retries inside nested blocks meets it the first time a node restarts under a transaction.
2. S14-02: `control_call_timeout` is documented as a bound and is not one; and it can turn the answer for a silent node into
   an answer that tells the operator not to look at the node.
3. S14-03 is a leak per failure in a long-lived backend.

Another review of the sequences is needed after S14-01 to S14-03 are fixed: the nested-subtransaction callbacks
(S14-01, S14-05) have not been exercised by any of six rounds, and the fix for S14-05 (move the RELEASE to the pre-commit
event) changes the very callback that this round fixed. After that, the track can be closed if a user-transaction sweep with
savepoints (like the one I used) is part of the repository and passes on both majors.

## Not checked

- `KINGRES_LONG_TESTS=1` runs of `t/108` to `t/111` (every position); more than two forwarded nodes in one call beyond `t/111`.
- The wait of `quiesce` on a user's prefetch inside a control call, and a statement larger than the socket buffers sent to a
  node that does not read (by reading only).
- S14-08 (a cancel inside the RELEASE wait) and the add_node standby refusal text (by reading only).
- `promote_node` and `recover_prepared_transactions` under my four fault modes beyond what `t/101`, `t/108` to `t/110` run.
- Whether S14-01 existed before this round (no earlier state in the export); a node whose postmaster is stopped, a name that
  does not resolve; a PostgreSQL 19rc2 run of the closed-connection probe for the single-level case and of `control_call_timeout`.
- Two of my helper scripts in the shared scratch directory had the same names as scripts of the caller and overwrote them;
  nothing else outside my own area was written, and my area and every server I started were removed at the end.
