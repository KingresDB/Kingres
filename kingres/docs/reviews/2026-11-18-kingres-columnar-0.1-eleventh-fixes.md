<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
# Twelfth independent review of kingres_columnar 0.1 (the fixes for C11)

Reviewer: an independent reviewer. Date: 2026-11-18.

## Scope and method

Reviewed: the read-only export of `contrib/kingres_columnar/` (C, SQL, tests, README), `kingres/docs/reviews/` and
`kingres/dev/ext.sh`. I read the eleventh review (C11-01 to C11-09, what each fix must achieve) first, then the code
behind each claim: `kc_catalog.c` (`kc_am_class` and its cache, `kc_catsnap_copy`, `kc_live_files`,
`kc_reconcile_ex`), `kingres_columnar.c` (`kc_check_one`, `kc_check_guarded`, `kc_check_list_tables`, `kc_check_run`,
the gate, `kc_lock_blockers`), `kc_vacuum.c` (failsafe, order of the frozen id and the truncation), `kc_am.c`
(in-place truncate), `kc_write.c` (drop note, flush), `kc_cscan.c` (planner hook), `kingres_columnar--0.1.sql` (the DDL
guard), and the tests `t/036`, `t/039`, `t/040`, `t/041`. Line numbers are those of the export. The eleventh review
asked for a short round, deep on the fixes and on "whatever else uses `relam` or the access method name instead of
the handler"; this one is shaped that way.

I worked on copies only: a copy of each installation (PostgreSQL 19rc2 and 18.7, both built with assertions; I
checked `SHOW debug_assertions` = on on 19rc2 only), a copy of the export, `ext.sh` copied with its socket directory
and port moved, and the TAP framework copy with its port range. At most two test or server
processes at a time, everything under `nice` and `timeout`, no suite run at the same time as another suite, no
work file in a shared or memory-backed directory. No probe could use more than about 150 MB: the largest was a
session that listed 1200 empty tables while another session locked them (measured peak 103 MB, below), the random
workload used small tables (a few thousand rows), and the mutant builds are ordinary compiles. Every server I started
was stopped and all my work files were removed at the end.

My own tools (not part of the repository): a loop of DDL against a loop of `columnar_check()` (six kinds of DDL); a
prepared-transaction matrix (five commands, each ended by `COMMIT PREPARED` and by `ROLLBACK PREPARED`, with the check
dry, `check(true)`, a vacuum of another table and `columnar_reconcile()` while the transaction is prepared); a random
workload against a heap twin with three access-method names over the one handler; a small C module with two handlers
(one returns the extension's routine, one returns a copy of it); a memory probe; and eleven one-line mutants.

## What was run

| Run | PostgreSQL 19rc2 (assertions) | PostgreSQL 18.7 (assertions) |
|---|---|---|
| Clean build of the extension with `-Werror` (`EXT_WERROR=1`) | no warning | no warning |
| `pg_regress` (11 tests) | all passed | all passed |
| TAP, `t/001` to `t/041` | 41 files, 10216 tests, all passed | 41 files, 10214 tests, all passed |
| Skipped by message | `t/013` (the `xid_wraparound` test module is not installed in this installation) | the same |

Production (no assertions) build of a server: not done (see "What I did not check"). Crash recovery workload against
a heap twin: not done.

Probes (19rc2, assertions), each with its result:

| Probe | Result |
|---|---|
| Two further access methods over `kingres.columnar_handler`; tables in each; `VACUUM` of another table, `columnar_check_clean()`, `columnar_check()`, `columnar_reconcile()` | 100000 rows of the alias table still there; check clean; nothing reported |
| `CREATE ACCESS METHOD` / table / `DROP` both / `CREATE` again, all in one transaction, then `VACUUM` | rows of the re-made table (1 row) there; check clean |
| A C wrapper handler that returns the extension's routine (`am_same`) | classified columnar, 5000 rows inserted, 5000 read, survives `VACUUM` of another table |
| A C handler that returns a copy of the routine (`am_copy`) | see C12-04 |
| DDL loops against a looping check (40 checks each): `SET ACCESS METHOD` heap and back (150), create/insert/drop (300), `VACUUM FULL` (150), `TRUNCATE` and reload (200), `SET TABLESPACE` and back (100), `CREATE TABLE AS` and drop (200) | 240 checks in all: `orphan_catalog_rows` 0, `unreadable` 0, any other row than `not_checked` 0 (`not_checked` 7 to 41 per 40 checks, by design); `columnar_check_clean()` true after each; `rw` has 100 rows at the end as it should |
| Prepared transaction held across the call: `SET ACCESS METHOD heap`, `SET ACCESS METHOD columnar`, `TRUNCATE`, `DROP TABLE`, `SET ACCESS METHOD` of another table; each ended both ways; the check dry, `check(true)`, `VACUUM` of another table, `columnar_reconcile()` while prepared | no false orphan, nothing deleted that the commit or the rollback needed: row counts and the stripe catalog count are exactly those the ending predicts in all 10 cells |
| Lock leftovers (1200 tables): caller holds `ACCESS EXCLUSIVE`, `ACCESS SHARE` (with `check(true)`), `SHARE UPDATE EXCLUSIVE`; savepoint and `ROLLBACK TO`; `statement_timeout = 5 ms` in a savepoint and without one; three calls in a transaction; `PREPARE TRANSACTION` after a check | the caller's lock is kept in its own mode and no other lock of the tables remains in any cell; both cancelled calls ended with the cancel error and left 0 locks; prepared transaction made |
| Random workload against a heap twin (seed 11): three tables on three access-method names, random insert / `TRUNCATE` / `SET ACCESS METHOD` among `columnar`, `columnar2`, `columnar3`, `heap` / `VACUUM` / `VACUUM FULL` / `columnar_check(true)` / `columnar_reconcile()` / `columnar_compact`, count, sum and md5 of every row compared after every operation, while a second session looped `VACUUM`, `columnar_check(true)`, `columnar_reconcile()` | 6731 operations, 0 mismatches, 0 errors other than the expected "not a columnar table"; clean at the end. (A first run of 8295 operations had a defect in my own comparison, which gave NULL for an empty table; it flagged only empty tables, and I discarded it.) |
| Memory of the check over many calls: 9000 `columnar_reconcile()` and 9000 `columnar_check_clean()` in one session, 1200 tables | RSS 15.9 MB -> 19.9 MB -> 44.9 MB; the growth is sublinear when the call count is tripled (3000 calls: +16.6 MB, 9000: +25 MB), so a retained allocator, not a per-call leak |
| Memory and time of `not_checked` rows (1200 empty tables, a second session holds `ACCESS EXCLUSIVE` on N of them, `columnar_check()` once) | peak RSS 39 MB (N=300), 51 MB (N=600), 103 MB (N=1200); every one of the N rows names a process: quadratic, see C12-02 |

## Fate of the C11 findings

| Id | State | Evidence |
|---|---|---|
| C11-01 (false orphans while DDL commits) | **Fixed.** | 240 checks under six kinds of DDL gave no false orphan (the eleventh review: 10 to 30 percent). The prepared-transaction matrix gave none and no loss in the deleting forms. Mutant M3b (second snapshot never consulted): `t/036` fails 6 assertions. Mutant M4 (no second claim): fails 2. The deep copy is not tested: M10 (shallow copy again: `xip` and `subxip` point at the static snapshot) passes `t/036` (C12-05). The snapshot copies are never freed in `kc_reconcile_ex` (a few hundred bytes per call, in the caller's context); no growth measured over 9000 calls. |
| C11-02 (alias access method loses all rows) | **Fixed for the access methods the report describes; three defects remain behind a superuser's deliberate act** (C12-01, C12-03, C12-04). | The 100000-row table of `columnar2` survives `VACUUM` of another table, `columnar_reconcile()`, `check(true)`; it is checked and named when its file is cut (`t/039`). Re-creating the access method in one transaction and across sessions: correct. The random workload with three names and a heap twin: 0 mismatches in 6731 operations. Mutants: M2 (unclassified relation ignored) fails 4 assertions of `t/039`; M12 (the drop note off) fails 64 assertions over `t/023`, `t/028`, `t/039`. M14 (the cache never cleared) passes (C12-05). M1 (a name test inside `kc_am_class`) did not compile twice and is not counted. |
| C11-03 (check needs a lock entry per table) | **Fixed.** | Lock-leftover cells above: none in any cell, including a cancel. `t/040` runs with a lock table of 100 entries and 1000 tables. Mutant M5 (lock kept to the end): `t/040` dies on its first call ("out of shared memory"). |
| C11-04 (in-place truncate and the parent subtransaction) | **Fixed.** | `kc_am.c:634-635` compares with the current subtransaction as the core does. Mutant M6 (accept any subtransaction): `t/031` fails 10 of 5503. Suite green on both majors. |
| C11-05 (failsafe for the table's own age) | **Fixed.** | `kc_vacuum.c` `kc_failsafe_active` uses the core's `vacuum_xid_failsafe_check`; the frozen id is recorded (`vac_update_relstats`) before `kc_truncate_tail`, so a cancelled or given-up truncation has already recorded an id the file holds (`kc_meta_freeze` ran first). Mutant M7 (failsafe never on): `t/041` fails 1 of 16. I did not run the age comparison against a heap twin at the 105 percent boundary myself (read only). |
| C11-06 (`unreferenced_bytes`) | **Fixed** (SQL line 62, README line 58, `pg_regress` green). | Read, and the regression and TAP suites pass. |
| C11-07 (gate is a production knob) | **Fixed.** | `kc_gate_held_by_superuser` (`kingres_columnar.c:729`); the check functions are superuser only (`:1166`, `:1196`), so the process ids in a `not_checked` row go only to a superuser. Mutant M9 (any holder counts): `t/036` fails 4. |
| C11-08 / C11-09 (tests, operator text) | **Fixed**, with the defect in C12-02. | Mutant M13 (no blocker text): `t/036` fails 1. In my runs every `not_checked` row of a locked table named the holder (`It is held by process N.`). |

## New findings

| Id | Severity | Place | Statement |
|---|---|---|---|
| C12-01 | medium | `kc_catalog.c:1162, 1180-1190, 1231`; `kingres_columnar.c:1128-1141` | When the orphan step stands down (a relation whose access method cannot be classified, or an unreadable metapage), `columnar_check()` returns no row and `columnar_check_clean()` returns true with orphan rows present; the only trace is a `LOG` line. `VACUUM` and `columnar_reconcile()` silently clean nothing for as long as the condition lasts. |
| C12-02 | medium | `kingres_columnar.c:801-840, 871, 1100` | Every `not_checked` row asks the lock manager for a full copy of its lock data and never frees it until the call ends: memory and time grow with the square of the number of locked tables; the README says nothing grows with the number of tables. |
| C12-03 | medium | `kingres_columnar--0.1.sql:218, 273` against `kc_catalog.c:203` | The DDL guard recognises a columnar table by `pg_am.amhandler = 'kingres.columnar_handler(internal)'` (the function's oid); C recognises it by the routine the handler returns. An access method over another SQL function with the extension's C entry point is columnar for everything but the guard: foreign keys, `AFTER ROW` triggers and publications are accepted on its tables. |
| C12-04 | medium | `kc_write.c:2095, 2950`; `kc_catalog.c:233` | A table of an access method whose handler returns a copy of the extension's routine accepts inserts, reports success, and drops the rows at commit; nothing is read back and no error is raised. |
| C12-05 | low | tests | Two fixes have no test that can fail: the deep copy of the snapshot (C11-01) and the clearing of the classification cache (C11-02). Nothing tests the alias handler function, the stand-down seen from the check, or a second session's cache. |

Severity scale as in the earlier rounds. I found no critical finding and no sequence in which the reconciliation,
`VACUUM`, `columnar_check(true)` or `columnar_reconcile()` deletes stripe rows of a live table of an access method that
the extension classifies as its own.

### C12-01 (medium) The cleanup that stands down is invisible to the check

Place: `kc_catalog.c:1162` (`unclassified = true`), `:1180-1190` (the stand-down: a `LOG` line, `return false`), `:1231`
(`if (!kc_live_files(&live, &nlive)) return 0;`), `kingres_columnar.c:1128-1141` (the dry count is added as an
`orphan_catalog_rows` row only when it is above zero).

The stand-down is right as a safety rule (delete nothing you cannot attribute). What it does to the callers is not:
the dry form is the check's orphan step, and a count of 0 from a step that did not run is the same value as a count
of 0 from a step that ran and found nothing. `columnar_check_clean()` and `columnar_check()` then say what the README
says they must never say ("never true for a table that was not looked at"; "any row at all means not clean"). The
README states that the clean-up stands down; it does not say that the check inherits it as a clean answer.

Steps (19rc2 with assertions; database with a few columnar tables):

```
-- a stripe row of no table, made by hand as a drop that left it would
INSERT INTO kingres.columnar_stripe SELECT 777777, <the other columns of any one row> ...;
SELECT kingres.columnar_check_clean();           ->  f      (orphan_catalog_rows: 1 stripe catalog rows belong to files no relation uses)
-- a second session of a superuser, still open:
BEGIN; CREATE ACCESS METHOD c9 TYPE TABLE HANDLER kingres.columnar_handler; CREATE TABLE z9 (a int) USING c9; SELECT pg_sleep(40);
-- meanwhile, in the first session:
SELECT kingres.columnar_check_clean();           ->  t      (the orphan row is still there)
SELECT * FROM kingres.columnar_check();          ->  0 rows
SELECT kingres.columnar_reconcile();             ->  0
VACUUM a1;                                       -- no error
SELECT count(*) FROM kingres.columnar_stripe WHERE rel_uid = 777777;   ->  1
-- server log, three times: columnar stripe catalog cleanup skipped: a relation has an access method that cannot be classified
-- after the other session rolls back: check_clean -> f, orphan row reported
```

How long it lasts: as long as one superuser transaction that has created an access method and a table of it stays open
(a long single-transaction restore that creates the extension and a table of it is the ordinary case), or, for the
unreadable-metapage branch of the same function (read, not run), as long as one columnar file has a bad first page.
Only a superuser can create the access method, so no ordinary role can cause it; the damage is a monitor that is
silently blind to orphans, and a vacuum that silently does no reconciliation, for the duration.

A fix must achieve: a stand-down is a visible outcome of the check: `kc_catalog_count_orphans` (or its caller) reports
"could not be done" and `kc_check_run` turns that into a `not_checked` row with the reason (and `columnar_check_clean()`
into false), in the same words as the other `not_checked` rows; `columnar_reconcile()` says so (a WARNING or a NULL
result, not 0). A test: the steps above with `columnar_check_clean()` required to be false while the transaction is
open.

### C12-02 (medium) `not_checked` rows cost the lock data of the whole server each

Place: `kingres_columnar.c:817` (`ld = GetLockStatusData();` inside `kc_lock_blockers`), called for every
`not_checked` table at `:871` and for the catalog row at `:1100`; there is no `pfree(ld)`, and the call runs after
`kc_check_guarded` has switched back to the caller's context, so the copy stays until the end of the call. (The same
call is also made, and never freed, at every 10 ms poll of a waiting gate, `:731`: tests only.)

`GetLockStatusData` takes every lock-manager partition lock in turn and copies one entry for every lock held in the
whole server (about 48 bytes each in my numbers). A check that finds N tables locked therefore allocates about N times
the size of the lock table and holds the lock-manager partitions N times. A transaction that locks a big partitioned
table (`ALTER TABLE` on the parent holds `ACCESS EXCLUSIVE` on every partition) is the case this meets.

Steps (19rc2, 1200 empty columnar tables, default settings; a second session holds `ACCESS EXCLUSIVE` on N tables;
one `columnar_check()`; the peak memory of the checking backend is its `VmHWM`):

```
N=300    rows 300    rows naming a process 300    VmHWM  39380 kB
N=600    rows 600    rows naming a process 600    VmHWM  50948 kB
N=1200   rows 1200   rows naming a process 1200   VmHWM 103124 kB
```

Going from 300 to 600 adds 11.5 MB, from 600 to 1200 adds 52 MB: quadratic, about 48 bytes times N squared. At N =
5000 locked partitions that is about 1.2 GB in the monitoring backend, and every one of those 5000 reads stalls every
other backend's lock acquisition for the length of a copy (not run: the lock table of my server holds 1600 entries;
the figure is extrapolated from the three measured points). An out-of-memory error ends the call (it is not about a
table, C10-02), so a check made while a large DDL runs can fail where it should report `not_checked`. The README
states the opposite: "nothing grows with the number of rows or the number of tables except the oids" (README line 61).

A fix must achieve: read the lock data once per call (or once per fixed number of rows) and free it; or give only the
first rows the process text (and say so in the rest); the memory of a call with N `not_checked` rows must be linear in
N with a small constant; and correct the README statement. A test: 600 locked tables, memory of the backend (or
`pg_backend_memory_contexts`) below a bound that the quadratic version exceeds.

### C12-03 (medium) The DDL guard recognises the handler by function, C by routine

Place: `kingres_columnar--0.1.sql:218` and `:273` (`a.amhandler = 'kingres.columnar_handler(internal)'::regprocedure`)
against `kc_catalog.c:203` (`GetTableAmRoutine(am->amhandler) == kc_get_routine()`).

The round's claim is that a columnar relation is recognised "by the handler of its access method everywhere". In C it
is the routine the handler returns; in the two queries of the guard it is the one function. A superuser may make a
second SQL function with the same C entry point and an access method over it; every C path calls its tables columnar,
and the guard does not.

Steps (19rc2, assertions):

```
CREATE FUNCTION kingres.h2(internal) RETURNS table_am_handler AS 'kingres_columnar','kingres_columnar_handler' LANGUAGE C;
CREATE ACCESS METHOD columnar3 TYPE TABLE HANDLER kingres.h2;
CREATE TABLE hp (a int PRIMARY KEY);
CREATE TABLE fk_real  (a int) USING columnar;   ALTER TABLE fk_real  ADD FOREIGN KEY (a) REFERENCES hp(a);
   ->  ERROR:  foreign keys on or to columnar tables are not supported
CREATE TABLE fk_alias (a int) USING columnar3;  ALTER TABLE fk_alias ADD FOREIGN KEY (a) REFERENCES hp(a);
   ->  ALTER TABLE            (accepted; the first write fails: "columnar tables do not support fetching a row by its TID")
CREATE PUBLICATION pb_real  FOR TABLE fk_real;    ->  ERROR:  columnar tables are not replicated logically yet
CREATE PUBLICATION pb_alias FOR TABLE fk_alias;   ->  CREATE PUBLICATION   (pb_alias lists the table)
CREATE TRIGGER tr AFTER INSERT ON tr_alias FOR EACH ROW ...;   ->  CREATE TRIGGER   (the same for a table USING columnar is refused)
```

For the foreign key and the trigger the first write fails with an error from the access method, so nothing is
silently wrong. For the publication, the rows of the table are not decoded, so a subscriber silently misses them:
the very case the guard exists for. The second access method over the extension's own function (`columnar2` in
`t/039`) is guarded; the one over another function is not.

A fix must achieve: one definition for the guard and for C. Either make the guard ask C (a small `STRICT STABLE` function
`kingres.columnar_is_columnar_am(oid)` that calls `kc_am_class`, used in both queries), or refuse in the extension
every access method whose handler is not exactly the extension's function. Add a `t/039` cell with a second SQL
function over the C symbol, with a foreign key, an `AFTER` trigger and a publication.

### C12-04 (medium) A table over a copy of the routine loses every inserted row at commit

Place: `kc_write.c:2095` (`kc_flush_buf_ex`: `if (!kc_rel_is_columnar(rel) || ...) { kc_buf_clear(b); return; }`),
`:2950` (the same for a merge), `kc_catalog.c:233` (`kc_rel_is_columnar` compares `rd_tableam` with the one routine).

Classification and relation test agree here (both say "not ours"), so the cleanup does not delete anything. But the
callbacks of the copy are the extension's own, so the table accepts a write, the buffer is built, and at commit the
flush discards it because the relation "is not columnar" ("converted to another access method, truncated or rewritten
meanwhile" is the comment's reason, not the case).

Steps (19rc2; a module with `kcw_copy` that `memcpy`s `GetTableAmRoutine(kingres.columnar_handler)` into a
`TopMemoryContext` struct and returns it; `kcw_same` that returns the extension's pointer):

```
CREATE ACCESS METHOD am_copy TYPE TABLE HANDLER kcw_copy;   CREATE ACCESS METHOD am_same TYPE TABLE HANDLER kcw_same;
CREATE TABLE w_same (a int, b text) USING am_same;   INSERT ... 5000 rows;   SELECT count(*) ->  5000
CREATE TABLE w_copy (a int, b text) USING am_copy;   INSERT INTO w_copy SELECT g, 'c' FROM generate_series(1,5000) g;   -> INSERT 0 5000
SELECT count(*) FROM w_copy;                                       ->  0
SELECT * FROM kingres.columnar_stripes('w_copy');                  ->  ERROR:  "w_copy" is not a columnar table
```

This is not a regression of this round (the wrapper that returns the extension's routine, the case the brief
named, works). It needs a superuser to load compiled code that copies another module's routine, which no
ordinary installation does; I rate it medium for the silent success of the insert and not higher for that
precondition.

A fix must achieve: a write entry point (`tuple_insert`, `multi_insert`, the begin of a bulk write) of a relation
whose `rd_tableam` is not the extension's routine raises an error (it has the extension's callbacks and is not
recognised by it), so a table that cannot keep its rows does not take them.

### C12-05 (low) Two of the fixes survive a mutant, and the cases behind C12-01 and C12-03 are untested

Mutants (each one line, one run of the file named; result):

| Mutant | Fix it removes | Test run | Result |
|---|---|---|---|
| M2 `unclassified = false` | stand-down | `t/039` | killed (4) |
| M3b the second snapshot never consulted | C11-01 | `t/036` | killed (6) |
| M4 the second claim off | C11-01 | `t/036` | killed (2) |
| **M10 shallow copy of the snapshot (`xip`, `subxip` pointing at the static snapshot)** | the deep copy | `t/036` | **survives** (101 of 101 pass) |
| M5 lock kept to the end of the transaction | C11-03 | `t/040` | killed (the first call fails) |
| M6 any subtransaction accepted | C11-04 | `t/031` | killed (10) |
| M7 failsafe never on | C11-05 | `t/041` | killed (1) |
| M9 any advisory holder counts at the gate | C11-07 | `t/036` | killed (4) |
| M12 drop note off | C11-02 | `t/023`, `t/028`, `t/039` | killed (64) |
| M13 no blocker text | C11-09 | `t/036` | killed (1) |
| **M14 the cache of access-method classes never cleared** | C11-02 | `t/039`, `t/036` | **survives** (131 of 131 pass) |
| M1 name test in `kc_am_class`; M3 | (compile failures of my own mutants, not counted) | | |

Place: `t/039_alias_am.pl`, `t/036_check_view.pl`. The cache clears on `pg_am` and `pg_proc` changes, and no test
shows that a second backend sees a `DROP ACCESS METHOD` or a replaced handler. The shallow-copy mutant is the bug
C11-01's fix comment describes; a test needs a transaction that commits while the call is held at the gate between
the two snapshots and whose id the first snapshot lists as running. The gate verdicts in `t/036`, `t/041` do not
depend on timing as I read them (they wait on advisory locks and wait events); I reran nothing for a timing failure,
because none occurred.

A fix must achieve: a test that fails for M10 (a committed transaction that was running at the first snapshot,
visible in the second, and a stripe row it deleted); a test that fails for M14 (a second session classifies a table,
the first session drops the access method and commits, the second session's next `VACUUM` still deletes nothing live;
or a cached answer for a handler that was replaced); the cells of C12-01 and C12-03.

## What held under attack

- The reconciliation never deleted a stripe row of a live table: in the DDL loops, the prepared-transaction matrix
  (deleting forms included), the random workload with a second session cleaning in a loop, three access-method names
  and rewrites in every direction, and after dropping and re-creating an access method in one transaction.
- The two-snapshot, two-claim rule closes the false orphan under `DROP TABLE`, `TRUNCATE`, `VACUUM FULL`, both
  directions of `SET ACCESS METHOD`, `SET TABLESPACE`, `CREATE TABLE AS`, and a rolled-back or committed prepared
  rewrite. A table made and dropped inside the window was the loop with `CREATE ... INSERT ... DROP`.
- Locks: nothing left behind in any exit I tried; a lock the caller held stays, in its own mode, weaker or stronger;
  a cancel in the middle ends the call and releases the table; the check needs one lock-table entry at a time (`t/040`).
- The check functions and the gate settings are for superusers; an ordinary role cannot hold the gate, read the
  process ids, or call the functions.
- A table moved between access methods (columnar, two aliases, heap) keeps its rows and its stripe rows follow.
- The classification cache is bounded (8 entries) and the per-call memory of the check (without `not_checked` rows)
  does not grow with the number of calls.

## Verdict

Is the extension fit for data that matters within what it claims? Yes for what the claims cover, with one condition
on the hand-made access methods: every sequence I could build with the extension's own handler, the aliases an
administrator would write, ordinary DDL, vacuum, check and prepared transactions kept the rows of the live tables, and
the full suite is green on both majors. What remains needs a superuser to do something deliberate and unusual: a
second SQL function over the C symbol (C12-03, a replication gap that the guard was meant to close), or a compiled
module that copies the routine (C12-04, rows dropped at commit). Neither is a way to lose data by ordinary use.

Is `columnar_check_clean()` fit for a monitor? Not completely: during the time a superuser's open transaction makes a
relation unclassifiable it answers true while orphans are present (C12-01), and a monitor that runs while a large DDL
holds many tables can use unbounded memory (C12-02). Both are bounded in what they cover (orphan rows only; superuser
caused or administrator DDL), but the first is exactly the "never true for something not looked at" promise.

Is another review needed? A full round is not. The path that moves and deletes data has held in this round and in the
random run, and the open points are in one place each. After C12-01 to C12-04 are fixed with their tests, a short
check of the fixes (a read of the diff, the new tests with a mutant each, the suite on both majors) is enough, and
the track can be closed if that is clean. If the project prefers not to close it, the one thing I would still want
is a crash-recovery run against a heap twin on a production build (below).

## What I did not check

- A production (no assertions) server build and a crash-recovery workload through it: the disk and time rule did not
  allow a server build, and the extension code that differs under assertions is the fault-point code
  only. My runs are on the two assertion builds.
- A standby or a promoted node; `t/002`, `t/033`, `t/037` cover them in the suite and passed.
- A handler that raises an error for a table access method other than the extension's: `kc_am_class` calls every
  handler of an access method that has a relation, so a broken third-party handler would make the reconciliation
  and the check raise instead of stand down (read, not run).
- The failsafe decision against a heap table at the exact 105 percent boundary, and the cancellation of a truncation
  while the lock is awaited beyond `t/041` (read; `t/041` and mutant M7 pass and fail as they should).
- Real contention of the check with a concurrent rewrite or truncate beyond the gate tests: the lock is held while the
  table is read and released after, so by the code a file cannot go away under the read; I did not stress it.
- `kc_am_class` after `CREATE OR REPLACE FUNCTION` of a handler that changes what it returns (the relation cache is not
  rebuilt by it, so the two definitions can differ until a relcache reset); a superuser act, not run.
- The `unreadable-metapage` branch of the stand-down (read only); PostgreSQL 18.7 for the probes (they ran on 19rc2;
  the suite ran on both).
