<!-- SPDX-License-Identifier: AGPL-3.0-only -->
# Adversarial review of the eighth fix round: contrib/kingres_columnar 0.1

Reviewer: an independent reviewer (not the author). The change under review
answers `kingres/docs/reviews/2026-11-17-kingres-columnar-0.1-seventh-fixes.md`
(C8-01 to C8-07). The seven earlier reviews are
`kingres/docs/reviews/2026-11-15-kingres-columnar-0.1.md`,
`kingres/docs/reviews/2026-11-15-kingres-columnar-0.1-fixes.md`,
`kingres/docs/reviews/2026-11-16-kingres-columnar-0.1-second-fixes.md`,
`kingres/docs/reviews/2026-11-16-kingres-columnar-0.1-third-fixes.md`,
`kingres/docs/reviews/2026-11-16-kingres-columnar-0.1-fourth-fixes.md`,
`kingres/docs/reviews/2026-11-17-kingres-columnar-0.1-fifth-fixes.md` and
`kingres/docs/reviews/2026-11-17-kingres-columnar-0.1-sixth-fixes.md`; the suites
that cover their findings were run in full (baseline below).

Method. The reviewed state (the fix round, and its parent for comparisons) was
exported with `git archive` (paths `contrib/kingres_columnar`, `kingres/dev`,
`kingres/docs`) into a private work area; the working tree was not used for any
result and nothing in the repository was changed except this file. PostgreSQL
installations with assertions: copies of the two given installations, 18.7 and
19rc2 (prefix fixed by copying: the binaries find their own directory; the
library path was set to the copy). Installations without assertions: built from
the `kingres-18` and `kingres-19` branches with meson, `-Dcassert=false`, the
extension built against each out of tree. Ports 59900 to 59999 only, one long
run at a time, nothing in the system's scratch or memory-backed directories. The test module
`t/kcevents` was used as the author uses it, and in a copy of mine that adds one
operation (it calls an SQL function of the extension through `fmgr`, with a
result set, an active snapshot and a standalone expression context) so that the
SQL functions classed as reads could be called at the forbidden moments; the
shell, SQL and Perl harnesses are mine and are not part of the repository. Line
numbers, where given, are those of the export.

## Summary

The round does what it says for the hostile-module half, and for ordinary use it
does no harm: I could not make any guard of `kc_entry_refused` misfire in
ordinary operation. Thirteen scenario groups (savepoints released and rolled back
in every order, 3000 exception blocks in a loop, recursion of subtransactions,
`TRUNCATE` in the transaction that created the table in every shape the review
asked for, `WITH HOLD` cursors, deferred constraint triggers firing columnar
inserts at commit, `ON COMMIT DROP` and `DELETE ROWS` temporary tables beside
columnar writes, `COMMIT AND CHAIN`, drops, event triggers, `VACUUM`, `VACUUM
FULL`, `ANALYZE`, autovacuum, `COPY`, `CREATE TABLE AS`, parallel scans, prepared
transactions with subtransactions across two immediate stops with
`wal_consistency_checking = all`) gave the same result as a heap twin wherever
the extension supports the statement, and no guard warning appeared in any server
log of the whole suite (outside the three test files that provoke it) or of my
runs (section "Ordinary use under the new guards"). The remembered refusal
(`refused_live`) is attributed to the right subtransaction in every nesting I
built and does not survive an error exit, a `DISCARD ALL` or a second attempt.
The new decision table is sound: of 25 cells each run on both majors, all 25 are
identical on 18.7 and 19rc2, the server stayed up in every one (the calls outside
the contract aside), and 7 of 7 guard removals, an added member with no decision,
the removal of the commit-time raise, of the move to the parent and of the clearing
at abort, and the removal of the truncate's command counter increment are killed
by `t/031`, `t/027` and `t/033` on both majors.

What does not hold:

* A critical section entered by another module at `XACT_EVENT_PRE_COMMIT` or
  `XACT_EVENT_PRE_PREPARE`, from a callback that runs after the extension's own
  (the order the README table is built for), is classed as "alive": the call is
  refused with a WARNING that says the transaction "fails with an error at its
  next pre-commit", the refusal is remembered, and nothing ever raises it: the
  transaction commits (or prepares) having lost the call. The README sentence
  "a transaction never commits having lost a call that it could still have failed
  for" is false for this moment (C9-01).
* `t/026` kills the same four of nine mutants of the flush and hand-over logic as the
  parent's file did (default seed; the survivors again with seeds 11, 22, 33), so the rewrite did not
  weaken it; the other five (four clause-level mutants of a redundant condition, and one of
  the break of a window hold) are killed, where they are not equivalent, by `t/022` and not by
  the random comparison, and the scripted prelude takes paths without checking them (C9-02, a note).
* `columnar_check(false)` is classed as a read "like the reads above". At the
  commit and at the abort of a subtransaction it begins an internal
  subtransaction, which PostgreSQL answers with a FATAL for the session (and, at the
  abort, a lock that stays); heap with a PL/pgSQL exception block does the same (C9-03).
* Smaller points: a table that is locked is now reported as `not_checked`
  instead of waited for (C9-04); the wording of the refusal and of the error
  (C9-05); a state variable that survives a commit without any test noticing
  (C9-06).

| Severity | Count |
|---|---|
| critical (wrong answer, lost or doubled committed rows where the caller did nothing PostgreSQL forbids, an ordinary transaction refused or failed by the new guards, a server crash, a SERIALIZABLE anomaly, an unreadable table) | 0 |
| high | 0 |
| medium | 1 |
| low | 2 |
| note | 3 |

C9-01 is a lost call and a promise in the README that is not kept; it needs a
module that enters a critical section at the pre-commit event after the
extension's callback, so by the rule of the earlier rounds (C6-01, C7-01, C8-01,
C8-02) it is medium and not critical; nothing in the KingresDB tree does it.

| Id | Severity | Component | Statement |
|---|---|---|---|
| C9-01 | medium | `kc_ending`, `kc_entry_refused` (`kc_write.c`), README | A refusal in a critical section at `XACT_EVENT_PRE_COMMIT` / `PRE_PREPARE` after the extension's own callback is remembered as "alive" and never raised: the transaction commits or prepares without the call, the WARNING and the README promise an error. |
| C9-02 | note | `t/026_random_cursors.pl` | Same teeth as the parent's file (4 of 9 mutants killed); the five others survive it on both majors and with three more seeds each, and `t/022` kills the combined mutants; the prelude takes the paths without comparing anything. |
| C9-03 | low | `columnar_check(false)`, README | Classed as a read; at `SUBXACT_EVENT_COMMIT_SUB` and `ABORT_SUB` it ends the session with FATAL and a lock stays behind (as for a PL/pgSQL exception block on heap). |
| C9-04 | low | `kc_check_one` | A table that cannot be locked at once gives a `not_checked` row instead of the answer; a script that filters on the names of the error rows passes silently. |
| C9-05 | note | warning and error texts | "the transaction is not in progress" at the commit of a subtransaction; the error does not say that a module wrote at a forbidden moment. |
| C9-06 | note | `kc_ending_reset`, tests | Removing the reset at `XACT_EVENT_COMMIT` survives every test: stale subtransaction state across a commit is untested. |

## Fate of C8-01 to C8-07

| Id | Fate | Evidence |
|---|---|---|
| C8-01 | Fixed in what it reaches, with one hole in the claim that follows from it (C9-01) | `ntrunc` and every other entry point at nine moments, 25 cells on each major (below), `t/031` 2 x 9 x 18 cells; no PANIC, no hang of the extension's own, `columnar_check()` empty after each cell that does not call a function outside the contract. `relation_nontransactional_truncate` called from a module at `XACT_EVENT_COMMIT`: `WARNING: truncate refused on columnar table "t": the transaction is not in progress`, rows 1..5,100 unchanged, on 18.7 and 19rc2. On 19rc2 at ABORT, COMMIT_SUB and ABORT_SUB (the readability failure of the seventh review): rows read, `columnar_check()` returns 0. |
| C8-02 | Fixed for the commit of a subtransaction, for a critical section in a live transaction, `PREPARE`, `ROLLBACK TO`, `DISCARD ALL`; **not** for a critical section after the extension's own pre-commit callback (C9-01) | Scenarios A1 to A10 below; `commit_sub` + truncate: `ERROR: a call on columnar table "t" was refused earlier in this transaction, so it cannot commit`, rows 1..5, `refused_live` 1. |
| C8-03 | Fixed (README) | Read: the heap production-build cell now says the row is visible and not durable. |
| C8-04 | Fixed | `t/032` passes; in the meson build of the `kingres-19` and `kingres-18` branches `kcevents.so` and `kcprobe.so` are built (`build/contrib/kingres_columnar/kcevents.so`) and `ninja install` does not install them (`find install -name "kc*"` gives only `kingres_columnar.so`, the control file and the script); `shared_module(..., kwargs: pg_test_mod_args)` has `install: false`. |
| C8-05 | Fixed, with a cost (C9-04) | `columnar_check(true)` behind a prepared `TRUNCATE`: not_checked row at once (`t/028`); locks it takes: `ConditionalLockRelationOid` in `ShareUpdateExclusiveLock` (fix) or `AccessShareLock` on every columnar table in turn, kept until the end of the calling transaction; none for a table it reports as not checked. |
| C8-06 | Fixed | `kc_orphans` is a static array of 10000 entries of 24 bytes in bss, the commit event copies into it; a build with assertions brackets the copy in a critical section (`t/028` passes); the setting `orphan_notes_max` is superuser only and can only lower (below). |
| C8-07 | Fixed as to flakiness: 50 seeds on each major pass; teeth unchanged from the parent, five survivors in both (C9-02) | Below. |

## Ordinary use under the new guards

### What the guard tests, and why an ordinary workload does not reach it

`kc_entry_refused` computes `kc_ending()`: `IsTransactionState()` true (state
`TRANS_INPROGRESS`) is "an error is allowed" unless `CritSectionCount > 0`; when
the state is not in progress it reads the subtransaction state the extension
recorded (`kc_sub_precommitted`, `kc_sub_ended`) and answers "alive" for a
subtransaction between its pre-commit and its end, "decided" for everything
else. Every ordinary call the extension serves (a statement, a trigger, a
function, `VACUUM`, autovacuum, `ANALYZE`, a parallel worker, a deferred trigger
at commit, a `WITH HOLD` cursor materialised at commit, `ON COMMIT` actions of
other tables) runs in `TRANS_INPROGRESS`: PostgreSQL's own callbacks at
`XACT_EVENT_PRE_COMMIT`, `PRE_PREPARE`, `START_SUB` and `PRE_COMMIT_SUB` run in
that state, and deferred triggers, portals and `ON COMMIT` actions run before
them. Temporary and unlogged columnar tables are refused at creation (checked:
`CREATE TEMP TABLE ... USING columnar ON COMMIT DELETE ROWS` and `CREATE UNLOGGED
TABLE ... USING columnar` give `ERROR: columnar tables cannot be temporary or
unlogged`), so `ON COMMIT DELETE ROWS` and `ON COMMIT DROP` reach the extension
only through the object access hook for heap tables, where the guard answers
"allowed". `relation_nontransactional_truncate` is called by PostgreSQL for a
table created in the same (sub)transaction (`heap_truncate_one_rel`), which is
an ordinary path; it is guarded and calls `CommandCounterIncrement()`, see below.

### What I ran

Script `ord1.sql` (heap twin in a second database, each statement echoed, the
two outputs compared with `diff`), on 18.7 with assertions, then once more on
19rc2 without assertions and against the parent's extension (section "The tests
of this round"). Autovacuum was on with a one second nap and tiny thresholds so
that it visited every table.

| Group | Statements | Result against heap |
|---|---|---|
| Savepoints | insert, `SAVEPOINT a`, `b`, `RELEASE a`, insert, `SAVEPOINT c`, `ROLLBACK TO c`, `SAVEPOINT e`, `f`, `ROLLBACK TO e`, `RELEASE e`, commit | same rows |
| Exception blocks in a loop | 3000 iterations, every third raises | same count and sum |
| Nesting and recursion | 60 nested blocks with an exception at the innermost; `rec(40)` recursive function, every fourth level raises | same |
| `TRUNCATE` | created and truncated in one transaction, `TRUNCATE n1, n2 RESTART IDENTITY` with an identity column, inside a savepoint that is rolled back, a table created in a subtransaction and truncated there (released, and rolled back), twice in one transaction | same rows and sequence values |
| `TRUNCATE` and a cursor or a function | truncate with an open cursor on the table; `TRUNCATE` in a function that the calling query reads | the same error as heap: `cannot TRUNCATE "cc" because it is being used by active queries in this session` |
| `WITH HOLD` cursor | declared after writes, committed, fetched, moved | same |
| Deferred trigger at commit | a heap table with a deferred constraint trigger inserting into a columnar table at commit (plain, with savepoints, with `SET CONSTRAINTS ALL IMMEDIATE`, prepared) | same rows |
| Temporary heap tables | `ON COMMIT DROP` and `ON COMMIT DELETE ROWS` beside columnar writes | same |
| `COMMIT AND CHAIN`, `ROLLBACK AND CHAIN` | with writes before and after | same |
| Drops | `DROP TABLE` in a savepoint rolled back, then really dropped; `DROP` after writes | same |
| Event trigger | `ddl_command_end` trigger notices during create, alter add column, truncate | same notices |
| `VACUUM`, `VACUUM FULL`, `ANALYZE`, `VACUUM (ANALYZE)` | on tables with many small stripes | same counts |
| Bulk | `COPY FROM STDIN`, `CREATE TABLE AS`, rolled-back `CTAS`, `ALTER TABLE SET ACCESS METHOD` both ways, a materialised view `USING columnar` and `REFRESH` | same |
| Parallel | `SELECT count(*), sum(a)` over 200000 rows with `max_parallel_workers_per_gather = 2` | same |

Differences from heap in the output of that run, all statements the extension
documents as unsupported: row-level `AFTER` triggers (the deferred trigger test
was therefore redone with the trigger on a heap table, above), a foreign key
declared on a columnar table, `TABLESAMPLE` (`ERROR: TABLESAMPLE is not supported
on columnar tables`, the same text as before the round), and `UPDATE`, `DELETE`,
`MERGE ... WHEN MATCHED`, `SELECT ... FOR UPDATE`, `CREATE INDEX`, which refuse
as they did. Nothing else differed.

A concurrent workload: `pgbench` with 8 clients for 25 seconds on 19rc2 without
assertions, each transaction inserting the same rows into a columnar and a heap
table with two levels of savepoints, `ROLLBACK TO` one of them half of the time,
`RELEASE`, and one transaction in 25 calling `columnar_compact`: 140181
transactions, 0 failed, both tables 1101604 rows and the same sum, `columnar_check()`
empty, 0 guard warnings in the server log, `refused_live` and `lost_raised` 0.

Autovacuum and wraparound (the question about `relation_vacuum` being skipped
every time): `t/013` is skipped in an out-of-tree run (the `xid_wraparound` test
module is not installed: "skipped: the xid_wraparound test module is not
installed" on both majors), so I ran the case myself. `autovacuum_freeze_max_age =
100000`, 130000 transactions from `pgbench`, a columnar and a heap table of 1000
rows: both tables had `age(relfrozenxid)` 25079 and 25077 afterwards, i.e. the
anti-wraparound autovacuum visited the columnar table and `relation_vacuum` ran;
18.7 and 19rc2 (with assertions). With default settings and an insert threshold
of 20 rows, `pg_stat_user_tables` shows `last_autovacuum` and `last_autoanalyze`
set for the columnar table. (The columnar vacuum prints no "automatic vacuum of
table" line; that is not new and is not a sign that it did not run.)

`relation_vacuum` called by a module with only `RowExclusiveLock` crashes: I
reproduced it (the module calls it at `XACT_EVENT_PRE_COMMIT`: `WARNING: missing
lock for relation "t"` and then a segmentation fault in 18.7 and 19rc2 without
and with assertions). The same module call on a **heap** table (`table_relation_vacuum` on
a heap table opened with `RowExclusiveLock`) aborts the backend too (signal 6,
18.7), so the contract is PostgreSQL's (`vac_update_relstats` needs
`ShareUpdateExclusiveLock`), not the extension's. No ordinary path reaches it:
`VACUUM`, autovacuum and `VACUUM FULL` take `ShareUpdateExclusiveLock` or more
before they call the callback.

### The log search

Every TAP log of the baseline run on each major (95 log files per major) and the
`server.log` of the regression run were searched for `refused on columnar`,
`refused: the transaction` / `refused: inside`, `forbids an error` and `was
refused earlier`. Files with a hit, 18.7 and 19rc2 alike: `t/027` (two servers),
`t/031` (two servers) and `t/033` (the primary), which are the tests that provoke
the guard. No other file has one, and nor does the `server.log` of any run of mine
outside the cells that call the module at a forbidden moment.

### The remembered refusal

Scenarios on 18.7 and 19rc2 with assertions, module `t/kcevents` (`ntrunc` at
`commit_sub` unless noted; `A9` and `A10` ran after a `DISCARD ALL`, so they used
the module's default operation, an insert, which changes nothing here):

| Case | Expected | Observed |
|---|---|---|
| A1: `SAVEPOINT a; SAVEPOINT b;` refusal at the commit of b; `ROLLBACK TO a`; insert; commit | the refusal goes with a: commit | rows 1,2; `refused_live` 1, `lost_raised` 0 |
| A2: the same, `RELEASE a` | the release fails, the session is usable | `ERROR: a call on columnar table "t" was refused earlier ...`; `ROLLBACK`; the next transaction commits |
| A3: refusal in a critical section at top level (`kcevents_crit`), then `SAVEPOINT a; INSERT; ROLLBACK TO a; COMMIT` | the refusal belongs to the top level: nothing clears it | the `INSERT` raises at once, the `COMMIT` raises as well; rows unchanged |
| A4: refusal, then the next write | raises at the write | raises at the `INSERT` |
| A5: refusal, then `PREPARE TRANSACTION` | nothing is prepared | the error; `pg_prepared_xacts` empty; a later `PREPARE` and `COMMIT PREPARED` work |
| A6: refusal inside a PL/pgSQL exception block | the block's handler catches the error of the release | caught, the transaction goes on |
| A7, A8: the transaction after an error exit; `DISCARD ALL` after a refusal in an aborted transaction | clean | commit |
| A9: refusal at the commit of b under a, `COMMIT AND CHAIN` | fails, the chained transaction is clean | fails; the next transaction commits |
| A10: refusal at the commit of c under b under a, `ROLLBACK TO b`, `COMMIT` | the refusal belonged to c, b is rolled back: commit | commit |

The state is static memory of the backend and is reset by `kc_ending_reset()` at
`XACT_EVENT_COMMIT`, `PREPARE` and `ABORT` of the top-level transaction, so a
backend reused by a pool for another user starts clean (checked by the next
transaction of the same session in A2, A5, A7, A8, A9). A refusal remembered at a subtransaction
is moved to its parent at `COMMIT_SUB` and forgotten at `ABORT_SUB` of its owner
only; the owner is the outermost subtransaction of those that lost something,
which makes a later `ABORT_SUB` of an inner one unable to clear a refusal of an
outer one (I could not build a sequence that clears one it should not; the
reading of `kc_lost_owner` is by minimum, and the clearing by equality, which is
safe). With `emit_log_hook` raising at the warning (`hook_raise`): the commit of
the subtransaction aborts with `ERROR: kcevents: the log hook raises for a
refusal of the extension`, the transaction is in the aborted state, `bytes_held` 0,
`refused_live` 0, `orphans_kept` 0, the next transaction of the session commits
and `columnar_check()` is empty. With a hook that raises inside the critical
section the server stops with a PANIC, which the README states for any WARNING.

### `CommandCounterIncrement()` in the truncate callback

`relation_nontransactional_truncate` is reached by PostgreSQL from
`heap_truncate_one_rel`, for a table created in the same (sub)transaction, after
the checks that no query uses the table (`CheckTableNotInUse`). The increment
makes the stripe rows that the same command wrote visible to the delete that
follows; I looked for a caller for which that is wrong and found none: `TRUNCATE`
of several tables, `RESTART IDENTITY`, in a savepoint that is rolled back, a table
created and truncated in a subtransaction that is rolled back or released, in a
function (the same refusal as heap when the calling query reads the table), with
a held cursor on the table (the same refusal as heap), all gave the heap twin's
rows. `ON COMMIT DELETE ROWS` cannot reach a columnar table. The mutant without
the increment is killed by `t/031` ("attempted to delete invisible tuple" at pre-commit);
no ordinary statement reaches the case it exists for.

## The decision table

### The cells I ran

Module `t/kcevents` with the test's own scripts, 25 cells on each of 18.7 and
19rc2 with assertions (order "the module's callbacks run after the access
method's"); the result of each cell was identical on the two majors (the two
outputs differ in one line, a TID in a warning of a call that is outside the
contract). After each cell: rows, `columnar_check()`, whether the server was
alive and whether the log has a PANIC or a signal.

| Moment | Entry point | Rows afterwards | Warning / error |
|---|---|---|---|
| commit | `ntrunc` | 1..5,100 | warning `truncate refused`, commit stands |
| commit | `setnew` | 1..5,100 | warning `new file refused` |
| commit | `sample` | 1..5,100 | two warnings `sampling refused` |
| prepare | `cluster` | 1..5,100 | warning `rewrite refused`, prepared transaction intact |
| prepare | `ntrunc` | 1..5,100 | warning |
| abort | `copydata` | 1..5 | warning `file copy refused` beside the transaction's own error |
| abort | `fn_write` (compact, set_options) | 1..5 | two warnings |
| abort_sub | `vacuum` | 1..5,100 | warning `vacuum refused`, nothing lost |
| release phase `locks` | `idxdel` | 1..5,100 | warning |
| release phase before locks | `fn_write` | 1..5,100 | two warnings |
| commit_sub | `ntrunc`, `setnew`, `copydata`, `idxbuild`, `tidvalid` | 1..5 | warning, then `ERROR: a call on columnar table "t" was refused earlier ...`, transaction aborted |
| commit_sub | `fn_safe` (reconcile, finish_rewrites, note_rewrite) | 1..5,100,101 | three warnings, commits (nothing lost) |
| critical section | `cluster` | 1..5 | warning, then the error at the pre-commit |
| critical section | `vacuum`, `objhook` | 1..5,100 | warning, commits |
| pre_commit (allowed) | `setnew` | 1..5 | `ERROR: attempted to delete invisible tuple` (the call is carried out, with a locator of the module's making) |
| pre_prepare (allowed) | `setnew` | 1..5 | the same error |
| pre_commit, start_sub (allowed) | `vacuum` | | the backend dies, the server restarts (signal 11; the same call on a heap table: signal 6), see above |

The cells that the author did not run:

* `setnew`, `copydata`, `cluster` and `vacuum` at the allowed moments. The module
  calls them for the table it holds with a locator and arguments of its own, outside
  the contract (PostgreSQL calls `relation_set_new_filelocator` and
  `relation_copy_data` only together with the update of `pg_class`, and
  `relation_vacuum` only under `ShareUpdateExclusiveLock`). `setnew` raises
  `attempted to delete invisible tuple` and the transaction aborts cleanly; `copydata`
  leaves a table whose file the catalog no longer has (`could not read blocks 0..0 in
  file`, `columnar_check()` reports it), `cluster` called with the table as its own
  destination doubles the rows, `vacuum` crashes as above. None of this is the extension's
  to prevent: the same abuse of heap gives an equally broken table, and the
  crash of `vacuum` is heap's too. I list them for the record; not findings.
* Reads at the forbidden moments on builds **without assertions**, 18.7 and 19rc2,
  28 cells on each (a server built from the `kingres-18` / `kingres-19` branch with
  `-Dcassert=false`): `scan` (begin, next slot, end) at commit, prepare, abort,
  abort_sub, commit_sub and in a critical section; `size` and `estimate` at commit,
  commit_sub, abort and in a critical section; `fetchrow` at commit; the index fetch
  in a critical section. All read what they should (rows 1..5 with 100 and 101 where they
  belong), no PANIC, no hang, no signal, `columnar_check()` empty, the guarded ones
  warn (`fetch by TID refused ...`, `index fetch refused ...`). The results of the two
  majors are identical.
* The SQL functions classed as reads, called from the module with `fmgr`, an active
  snapshot and a result-set context: `columnar_info` at commit, prepare, abort,
  abort_sub, commit_sub and in a critical section; `columnar_write_paths` at commit,
  abort_sub, commit_sub and in a critical section; all eight of `columnar_options`,
  `_info`, `_stripes`, `_identity`, `_creating_xid`, `_table_xid`, `_write_paths` and
  `columnar_check(false)` together at commit, prepare, abort and in a critical
  section: no crash, no hang, no change of any row, on both majors. At
  `SUBXACT_EVENT_COMMIT_SUB` and `SUBXACT_EVENT_ABORT_SUB` the call of
  `columnar_check(false)` ends the session: C9-03. Without an active snapshot
  `columnar_info` dereferences a null pointer at `XACT_EVENT_PRE_COMMIT`
  (`GetActiveSnapshot()` in `kc_catalog_snapshot`; I found it by calling the function from my
  module without one): a caller's duty, any SQL function runs with a snapshot, not a finding.

### The tests that walk the decisions

* A member added in a copy: `.scan_set_tidrange = kc_extra_cb` in the routine, no row
  in `kc_am_decisions`: `t/031` fails with `every callback of the columnar routine
  has a decision recorded` in both orders, on both majors.
* Guard removals, `t/031` each, both majors: the guards of `truncate`, `new file`,
  `vacuum`, the object access hook, `columnar_compact`, `file copy` and `fetch by TID`:
  all seven killed on 18.7 and on 19rc2 (the file dies at the first cell that reaches the
  removed guard; the log of the run has the server's crash).
* The remembered refusal, both majors: removing the raise at the pre-commit
  (`kc_lost_raise` in `XACT_EVENT_PRE_COMMIT`) and the move to the parent at `COMMIT_SUB`:
  killed by `t/027`, `t/031` and `t/033`; removing the clearing at `ABORT_SUB`: killed by
  `t/027` only; removing `kc_ending_reset` at `XACT_EVENT_COMMIT` survives all three (C9-06).
* The truncate without `CommandCounterIncrement`: killed by `t/031` only (the cell that
  truncates at pre-commit, after the commit-time flush); `t/028` and `t/033` pass with it
  removed. No SQL statement needs the increment, since the statements of a script are
  separate commands.

## The tests of this round

### `t/031_entry_points.pl` (C8-01)

Claims: every entry point, at nine moments (commit, prepare, abort, abort_sub, the three
release phases, commit_sub, a critical section), in both orders of loading the module, is
refused with a warning and a surviving server, fails the live transaction where it must, and
works at the moments where an error is allowed; every member of the routine has a decision.

Preconditions: established. It builds `t/kcevents` against the installation (a failure to
build fails the file), starts one server per order, checks the start time and the log for
`PANIC|TRAP:|terminated by signal`, the rows, the warning text, the number of returns, and
`columnar_check()` after each cell. I used its scripts for my own cells, and the 25 cells
I ran by hand on each major give the results it asserts.

Teeth: all seven guard removals I built are killed, the added member is killed, the removal
of the commit-time raise and of the move to the parent are killed (together with `t/027`,
`t/033`). What it does not have, in the table of "live" moments (`commit_sub` and a critical
section called from SQL), is the critical section entered **at the pre-commit event by a
callback that runs after the extension's own**, which is the cell of C9-01; the file's
header and the README state that class as handled. It also skips the reads at the forbidden
moments on a build with assertions (`kind` `r` and `s`): I ran them on builds without
(28 cells on each major, above). The cells for `setnew`, `copydata`, `cluster` and `vacuum` at
the allowed moments are excluded by its header with the reason that I found right: a module
that calls them with a locator of its own breaks any table.

### `t/027_ending_writes.pl`, `t/033_standby_late.pl`

Pass on both majors and without assertions. `t/027` is the only file that kills the removal of the
clearing at `ABORT_SUB` (mutant `r_noclear_abort`). `t/033` runs the refusal, the truncate at
pre-commit and the late drops on a primary and a standby with `wal_consistency_checking = all`
and across a promotion; I repeated the equivalent by hand (prepared transactions, two immediate
stops, `wal_consistency_checking = all`, below) and the rows agree with heap's in each state.

### `t/026_random_cursors.pl` (C8-07)

See C9-02. The file is no longer flaky: 50 seeds on each major (`KC_FUZZ_SEED = 7919 * n + 13`,
n = 1..50, 90 episodes) all pass, 50 of 50 on 18.7 and 50 of 50 on 19rc2; per run, over the random
part only, the counters of the paths: `ceiling_write` 2 to 16, `handover_flush` 7 to 32,
`late_commit` 21 to 378, `late_window` 57 to 892, `own_flush` 16 to 42, `prepared_skip` 1 to 28,
`pressure_flush` 79 to 154, `size_flush` 16 to 56, none of them 0 in any of the 100 runs
(`ceiling_handover` is 0 in 33 of 50 and is not part of the random assertions; the identical
numbers on the two majors show that the accounting is deterministic). The report line
"path X: the prelude n times, the random episodes m times" is printed for every path.

### `t/032_module_build.pl` (C8-04)

Passes. Its two outcomes (no compiler gives a skip that names the module, a compiler that
fails gives a failure with the compiler's output) are run against a lying `pg_config`. The meson
build builds both modules and installs neither.

### Timing

The complete suite, 33 files, was run once on 19rc2 with assertions with twelve busy loops of
my own on the 24 hardware threads of the machine (started before the run, stopped by pid after
it, 13.5 minutes): 33 of 33 files, 7352 tests, all passed; the wall clock was 812 seconds against 564
without the load. No verdict of any file changed. The tests that wait on time
(`pg_sleep(4)` in `t/027` while a client is killed, `lock_timeout` of 300 ms and 20 s, the
statement timeouts of 240 s as a guard against hangs) either wait for a condition first
(`poll_query_until`) or only use the timeout to turn a hang into an error. `t/013`
(the wraparound test) is skipped out of tree on both majors because the `xid_wraparound` module
is not installed; I covered the case by hand (above).

## New findings

### C9-01 (medium) A critical section after the extension's pre-commit loses the call and the transaction commits

Place: `kc_ending()` (`kc_write.c`: `IsTransactionState()` true and `CritSectionCount
> 0` gives `KC_END_LIVE`), `kc_entry_refused()`, `kc_xact_callback()` (the only
raise for the top-level transaction is at `XACT_EVENT_PRE_COMMIT` and `PRE_PREPARE`,
before `kc_commit_flushed = true`), README ("the transaction fails with an error at its
next pre-commit (alive)", row "inside a critical section"; the invariant "a
transaction never commits having lost a call that it could still have failed for").

Steps (18.7 and 19rc2, assertions; the module is loaded after the extension in
`shared_preload_libraries`, so that its callbacks run after the extension's):

```
CREATE TABLE t (a int) USING columnar; INSERT INTO t SELECT generate_series(1,5);
SET kcevents.at = 'pre_commit'; SET kcevents.op = 'insert'; SET kcevents.crit = on;
BEGIN; SELECT kcevents_open('t'); INSERT INTO t VALUES (100); COMMIT;
```

Observed: `WARNING: insert refused on columnar table "t": inside a critical section`
(its detail says "the transaction is still open and fails with an error at its next
pre-commit"), `COMMIT` succeeds, rows 1..5,100: the module's row is lost and nothing
fails. The same with `kcevents.op = 'ntrunc'` (the truncate is lost) and at
`pre_prepare` (the `PREPARE TRANSACTION` succeeds; `COMMIT PREPARED` commits without the
row). With the order reversed (the module's callbacks run before the extension's) the
same script gives `ERROR: a call on columnar table "t" was refused earlier in this
transaction, so it cannot commit` and rows 1..5. At `pre_commit_sub` and `start_sub`
the error is raised (the parent's pre-commit is still ahead).

Expected: by the README, no commit with a lost call. After `XACT_EVENT_PRE_COMMIT` of the
extension no moment is left at which an error is allowed and the extension's code runs
(an error in a critical section is a PANIC, `XACT_EVENT_COMMIT` is decided), so the
promise cannot be kept there; the honest answer is that this moment is "decided" like
the commit event.

Direction: in `kc_ending()` return `KC_END_DECIDED` for a critical section when
`kc_commit_flushed` is set (the top-level pre-commit has already run) and
no subtransaction is open, and let the WARNING say "if the transaction was committing,
the data is lost" as it does for the decided moments; correct the row of the README table
("alive" only until the extension's own pre-commit); add the cell `pre_commit` + `crit`
in both orders to `t/031` (it has `commit_sub` and `none` as its only live cells).

### C9-02 (note) `t/026` has the teeth of the parent's file, and does not kill the prepared-skip family; `t/022` does

Place: `t/026_random_cursors.pl` (the prelude, the check after the random part),
`kc_buf_flushable`, `kc_holds_break_window`.

Mutants built from the round's extension, `t/026` alone, 18.7 and 19rc2 with
assertions (the default random seed; then seeds 11, 22, 33 for the survivors; the
same nine mutants with the parent's `t/026`):

| Mutant | New `t/026` | Three more seeds | Parent's `t/026` |
|---|---|---|---|
| `kc_buf_flushable` without `b->refs == 0` (a flush under a cursor) | killed (`targeted: the cursor returned the rows it began with, once each`) | | killed (the same test) |
| without `b->subid == GetCurrentSubTransactionId()` | killed (`scripted: the release of a level that would leave more than twice the allowance is refused`) | | killed |
| `kc_write_is_late` without `kc_handover_prepared` | killed (prelude, `late_window`) | | killed (by the path count) |
| pressure threshold `allowance / 2` replaced by `* 4` | killed (prelude, `pressure_flush` 0 times) | | killed (by the path count) |
| `kc_buf_flushable` without `kc_commit_flushed` | **survives** | survives 3 of 3 | survives |
| without `kc_handover_prepared` | **survives** | survives 3 of 3 | survives |
| without `b->merge_into != NULL` | **survives** | survives 3 of 3 | survives |
| without `b->tail_ready` | **survives** | survives 3 of 3 | survives |
| `kc_holds_break_window` without `h->broken = true` | **survives** | survives 3 of 3 | survives |
| the whole prepared-skip condition of `kc_buf_flushable` removed (`if (0)`) | survives `t/026`; **killed by `t/022`** (`COPY into a table made in the transaction in the window, parent 999 rows, child 1: no error, no assertion failure`) | | not run |
| the same, keeping only `kc_commit_flushed` | survives `t/026`; **killed by `t/022`** | | not run |

(`t/016`, `t/024` and `t/030` do not kill the combined mutants; `t/022` kills them and the
`h->broken` mutant on both majors.)

So the rewrite did not take teeth from the file: it kills the same four mutants as
the file of the seventh review and lets the same five through. The four clause-level
survivors are equivalent mutants of a condition made of four redundant clauses (any three
of them keep the guard); what the clauses guard is killed as a whole by `t/022`, and the
`broken` mutant by `t/022`. The point of C8-07 was to make the file honest, and it is as to
flakiness (50 seeds on each major pass, below). What it still is, and what the commit message
and the header say a little too strongly ("takes every named path once, by construction"): the
prelude asserts `cmp_ok($one{$path}, '>', 0)`, that a path was taken, and compares nothing with
the heap twin; the random part compares but no longer has to reach `prepared_skip`. A change that
breaks the prepared-skip family and the break of a window hold is therefore found by `t/022` only
and not by the random comparison, as in the seventh review's round.

Direction (optional): after each prelude script read the table back and compare it with the heap
twin (count, sum, order of ids), so that a path that is taken is also a path that is checked.

### C9-03 (low) `columnar_check(false)` is not a read at the end of a subtransaction

Place: `kc_check_guarded()` (`kingres_columnar.c`: `BeginInternalSubTransaction` for each
table), README ("the other SQL functions ... `columnar_check(false)` | reads | the same as
the reads above"), `kc_am_decisions`/the table in the README.

Steps (19rc2 without assertions and 18.7/19rc2 with; the module registers
`kingres.columnar_check(boolean)` with `kcevents_sql` and calls it at `abort_sub`):

```
BEGIN; SELECT kcevents_open('t'); SELECT kcevents_sql('kingres.columnar_check(boolean)'::regprocedure::oid, -1);
INSERT INTO t VALUES (100); SAVEPOINT s; INSERT INTO t VALUES (101); ROLLBACK TO s; COMMIT;
```

Observed at `abort_sub`: `WARNING: AbortSubTransaction while in ABORT state`, `FATAL:
BeginInternalSubTransaction: unexpected state SUBRESTART`; the session is gone and the transaction with
it (rows 1..5); and a lock stays: `pg_locks` has a `RowExclusiveLock` on `t` with no pid and virtual
transaction `-1/0`, and `DROP TABLE t` waits behind it until cancelled (`statement_timeout`: 4 s, 6 s,
20 s, 120 s, all expired), for as long as the server runs. At `commit_sub`: `AbortSubTransaction
while in COMMIT state` and `FATAL: BeginInternalSubTransaction: unexpected state SUBRELEASE`, the
session is gone, the transaction with it, and the next `DROP` was not blocked. The server stays up in
both. The same happens on a **heap** table when the module calls an ordinary PL/pgSQL function with
an exception block at `abort_sub` (19rc2 without assertions: FATAL, the same stuck lock, `DROP TABLE`
blocked) and at `commit_sub` (the FATAL; no stuck lock), so the FATAL and the lock are PostgreSQL's
answer to a subtransaction begun in those states and not something the extension does; what is the
extension's is the README's classification of the function.

Expected: the README classes the function with the reads that "work wherever PostgreSQL lets
a heap table be read"; it does not, because it is the one read that starts a subtransaction.

Direction: either list `columnar_check` among the guarded entry points and answer "not
checked" through `kc_entry_refused` at the moments where the state is not in progress (the
helper already knows them), or say in the README that it must not be called there. No ordinary
call is affected: SQL runs in `TRANS_INPROGRESS`.

### C9-04 (low) `not_checked` can hide an error row from a script that filters

Place: `kc_check_one()`: `ConditionalLockRelationOid(relid, lockmode)`.

Observed: `columnar_check()` and `columnar_check(true)` take their lock conditionally (in
`AccessShareLock` and `ShareUpdateExclusiveLock`) for each columnar table in turn; a table
that is locked in a conflicting mode (a prepared `TRUNCATE` or rewrite; but also an
ordinary `ALTER TABLE` or `VACUUM FULL` that is running, for `AccessShareLock` any
`AccessExclusiveLock`) produces one row `not_checked` with the table's oid and the rest of
the check goes on. A caller that tests "the function returns no row" fails loudly, as
it should. A caller that tests for the names of error rows (`stripe_beyond_file`, `unreadable`,
`page_header_mismatch`, `orphan_catalog_rows`) passes while a damaged table is locked,
where it used to wait and then report. Before the round a plain `columnar_check()` waited
behind an ordinary lock for as long as it lasted.

Expected: either behaviour is defensible; the README names the row (`not_checked`) but not this
consequence, and the plain check, which has no cause to fear a long wait behind a prepared
transaction in the way `fix` has, changed too.

Direction: keep the blocking wait for the check without fix and add `lock_timeout`-style
bounded waiting (`ConditionalLockRelationOid` in a loop for a configurable time), or document
that a monitor must treat the `not_checked` row as a failure.

### C9-05 (note) The texts

The warning for a call at the commit of a subtransaction says `the transaction is not in
progress`, while its own detail says the transaction is still open and will fail; the
first line is true of the subtransaction only. The error `a call on columnar table "t" was
refused earlier in this transaction, so it cannot commit` names the table and the first call
(its detail counts the calls and names the first) but does not say that another module wrote at a
moment where PostgreSQL forbids it: that is only in the WARNING, which a client library that
drops notices never shows, and which can be filtered by `client_min_messages`. The hint of the
error is addressed to a module's author ("write only at XACT_EVENT_PRE_COMMIT ..."); a user
gets no pointer to the module, which the extension cannot know. Direction: put "a module called the
table at a moment where an error is forbidden" in the primary message or the detail of the error.

### C9-06 (note) Stale subtransaction state across a commit is untested

Mutant `r_noreset` (no `kc_ending_reset()` at `XACT_EVENT_COMMIT`, so `kc_sub_ended` and
`kc_lost_*` of the finished transaction stay) passes `t/027`, `t/031` and `t/033` on both
majors. A stale `kc_sub_ended` equal to the id of a subtransaction of the next transaction
would classify its abort as an end that committed and attribute a refusal to its parent. The
code resets at commit, prepare and abort, so this is a test gap, not a defect; one test that
makes two transactions in one session, the first ending with a refusal at the commit of a
subtransaction number 2 and the second with a refusal at the abort of its own subtransaction
number 2, would kill it.

## What held under attack

* No ordinary statement, in any of the scenarios of the table above, produced a guard warning or
  a difference from heap; no wraparound, vacuum or autovacuum path was skipped.
* The attribution of a refusal to a subtransaction, A1 to A10, in every nesting I could build
  with `RELEASE`, `ROLLBACK TO`, `COMMIT AND CHAIN`, `DISCARD ALL`, `PREPARE`, exception blocks.
* `emit_log_hook` that raises at a refusal at the commit of a subtransaction: nothing left over.
* The server never stopped for any of about 130 cells that call an entry point at a moment where
  an error is forbidden (25 on each major with assertions, 28 on each without, the critical-section
  cells in both orders, the SQL-function cells, the extra truncate cells at abort and abort_sub; the only
  stops are the calls outside the contract, which stop heap as well: `relation_vacuum` under a row
  lock, and a function that starts a subtransaction at a commit or abort event of a subtransaction,
  which ends the session for heap as well), and the extension left no unreadable table.
* A prepared transaction with subtransactions, a deferred trigger and a savepoint torture, across
  two `pg_ctl -m immediate` stops with `wal_consistency_checking = all`: the visible rows
  equal heap's in every state, `columnar_check()` returns 0, no consistency failure.
* The developer setting `kingres_columnar.orphan_notes_max`: `SET` above 10000 or 0 is refused
  ("is outside the valid range ... (1 .. 10000)"), `ALTER ROLE ... SET` and `ALTER DATABASE ...
  SET` and `ALTER SYSTEM` to 99999 are refused the same way, an ordinary role cannot set it
  (`permission denied to set parameter`), a superuser can lower it per session, role or
  database; lowering it only means that fewer leftover file notes are kept for the next
  transaction (VACUUM and `columnar_check(true)` remove the rest).
* The meson build builds `kcevents` and `kcprobe` and `ninja install` installs neither.

## Not reproduced

* A refusal that is cleared while it should stay: I tried A3 and A10 and the variants with a
  second refusal of an outer owner after one of an inner owner; the clearing by equality
  against the minimum owner is safe by construction.
* A guard warning in an ordinary autovacuum, `ANALYZE`, parallel worker, `ON COMMIT` action or deferred
  trigger: none.
* A heap-twin difference after `COMMIT PREPARED` from another session: none (the preparing
  session's static state is reset at `PREPARE`).

## Not covered

* Logical decoding and logical replication apply of columnar tables, background workers of other
  extensions, hot standby with a late drop (the author's own "not run"), out of memory at the commit event.
* `SERIALIZABLE` anomalies: the round does not touch the visibility code; the suite's `t/009`
  passes.
* The order "before" (the module's callbacks run before the extension's) for the 25 cells of the
  table: run only for the critical-section cells, the 28 reads and the suite's own both-order tests.
* A standby of my own: I relied on `t/002`, `t/033` and the author's promotion test, all of which pass
  on both majors, and on my own crash-recovery run with `wal_consistency_checking = all`; I did
  not build another standby.
* Concurrency between a refusal and other sessions (a refusal leaves no shared state; I did not test it).
* The cells for `kcevents.op` `bulk`, `finish`, `speculative`, `delete`, `update`, `lock` (they
  are `t/027`'s; the baseline passes).

## Baseline

Out of tree with `kingres/dev/ext.sh` (copy of the installations, `EXT_WERROR=1`, `KINGRES_JOBS=4`,
the extension built from the export of the commit):

| Build | pg_regress | TAP |
|---|---|---|
| 18.7, assertions | 11 of 11 | 33 files, 7350 tests, pass; `t/013` skipped (module not installed) |
| 19rc2, assertions | 11 of 11 | 33 files, 7352 tests, pass; `t/013` skipped |
| 18.7, no assertions | 11 of 11 | 33 files, 7269 tests, pass; `t/013` and `t/016` skipped (`t/016`: fault injection exists only in assertion builds) |
| 19rc2, no assertions | 11 of 11 | 33 files, 7271 tests, pass; `t/013` and `t/016` skipped |
| 19rc2, assertions, under twelve busy loops | not run | 33 files, 7352 tests, pass, 812 s |

No compiler warning with `-Werror` on any of the four builds. The in-tree (meson) build of the
two branches builds the extension and both test modules.
Against the parent's extension my ordinary-use script gives byte for byte the same output
(0 lines of difference, 529 lines, 19rc2 without assertions) and the parent's own 11 regression tests pass
with their own expected outputs; the round's expected outputs differ from the parent's only in the new
setting `kingres_columnar.orphan_notes_max` in the row list of `columnar_setup`.
The author's and the integrator's numbers (11 and 33 files with 7352 tests on 19rc2, 37 of 37 in tree on
18.7) agree with mine where they overlap.
