<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
# Eleventh independent review of kingres_columnar 0.1 (the fixes for C10)

Reviewer: an independent reviewer. Date: 2026-11-18.

## Scope and method

Reviewed: the read-only export of `contrib/kingres_columnar/` (C, SQL, tests, README), `kingres/docs/reviews/` and
`kingres/dev/ext.sh`. I read the tenth review (C10-01 to C10-08) first, then the README (check functions, vacuum and
truncation, autovacuum and the frozen id, writes while a transaction is ending, the decision table), then the code
behind each claim: `kingres_columnar.c` (check functions and the gate), `kc_catalog.c` (snapshots, conditional locks,
reconciliation), `kc_vacuum.c` (truncation), `kc_am.c` (new file, in-place truncate, rewrite), `kc_write.c` (the
"decided / alive" rule), and the tests `t/036` to `t/038` and the parts of `t/027`, `t/031` they touch. Line numbers
are those of the export.

I worked on copies only (one copy of each installation, one copy of the export, build and tests out of tree with a
copy of `ext.sh` whose socket directory and ports were moved into my own area; the TAP framework copy only to confine
its port range to 59400-59499). I built a production server (no assertions) of both majors from the PostgreSQL
sources (19rc2 with meson, 18.7 with configure) in my own area, next to the two installations with assertions.
Everything ran under `nice`, at most two test or server processes at a time, no work files in shared or memory-backed
directories. Every server I started was stopped, and the area was removed at the end. (My area peaked at about 11 GB
because of the two source copies and build directories, over the 4 GB I was asked to keep to; I removed it at once.)

My own tools (not part of the repository):

* A test module `rc11` (about 150 lines of C) that registers transaction and subtransaction callbacks and, at an
  event named by a setting (`pre_commit`, `pre_prepare`, `commit`, `abort`, `start_sub`, `pre_commit_sub`,
  `commit_sub`, `abort_sub`), optionally begins an internal subtransaction, optionally enters a critical section, and
  calls an entry point of the table access method (`tuple_insert`, `relation_nontransactional_truncate`), the C
  function `columnar_check_clean` directly (not through the function manager), or raises an error. It also has an SQL
  function that calls `relation_nontransactional_truncate` at an ordinary moment. It was loaded before the extension
  (its callbacks run after the extension's) and after it (they run before), on both majors.
* A random workload generator (two runs of 120 transactions each: inserts of 1 to 80000 rows, savepoints, `ROLLBACK TO`,
  `RELEASE`, PL/pgSQL exception blocks, aborted transactions, SERIALIZABLE transactions, `columnar_compact`, `VACUUM`,
  `VACUUM FULL`) that sends the same statements to a columnar table and a heap twin, with a driver for crash, base
  backup, archive-recovery standby with promotion, dump and restore, and rewrites.
* Scripts for the races of the check, the lock footprint, the vacuum thresholds, the frozen id and the in-place
  truncate; and 18 one-line mutants of the extension (below), built into a separate copy of the 19rc2 production
  installation.

## What I ran

| Run | 18.7 assertions | 19rc2 assertions | 18.7 production | 19rc2 production |
|---|---|---|---|---|
| clean build, `-Werror` | no warning | no warning | no warning | no warning |
| `pg_regress` (11 tests) | 11 of 11 | 11 of 11 | 10 of 11 (see note) | 11 of 11 |
| TAP, 38 files | PASS, 10079 tests, 783 s | PASS, 10081 tests, 546 s | PASS, 10728 tests, 667 s | PASS, 10760 tests, 682 s |
| skipped files | `t/013` (module `xid_wraparound` not installed, by message) | the same | `t/016` (fault injection only in assertion builds); `t/034` (module `test_decoding` not installed in my build: I installed it and ran the file alone, 30 tests pass) | `t/016` |
| `t/013` (the anti-wraparound path) | skipped | skipped | pass, 112 s | pass, 114 s |
| own workload, seeds 5, 6, 7 | - | - | seeds 5 and 7: all comparisons same | seeds 5 and 6: all comparisons same |
| own module `rc11`, cells A-K below | 2 load orders, as described | 2 load orders, as described | - | order "before" |
| 18 one-line mutants | - | - | - | 15 killed, 3 survive (below) |

Note on the one failed `pg_regress` test: `columnar_review` on my 18.7 production build, whose only difference is
`ERROR: collation "en-x-icu" for encoding "UTF8" does not exist` for `ALTER TABLE ... COLLATE "en-x-icu"`. I had
configured that build without ICU; the test has no skip for a server without ICU (a low test-quality point, C11-08).

Own workload, what "all comparisons same" means: after the load (about 1.04 million rows, 34 stripes), after a second
load (about 1.83 million rows), after an immediate stop and crash recovery, after `pg_dump -Fc` and restore into a
new database, after `columnar_compact`, `VACUUM FULL`, `ALTER TABLE ... SET ACCESS METHOD heap` and back, on a standby
made from a base backup taken between the two loads and the archived WAL (hot standby on, replayed to the end, then
promoted, a write on the promoted node, then the same comparison): count, sum of one column and the md5 of every
column of every row in id order are identical to the heap twin each time; `columnar_check_clean()` is true after each
step (and true on the standby in recovery and in a `READ ONLY` transaction). The standby was fed by archive recovery,
not streaming (the machine limit on servers); streaming is covered by `t/002`, `t/033`, `t/037`.

## Fate of the earlier findings

| Id | State | Evidence |
|---|---|---|
| C10-01 (check blind to what an old snapshot cannot see) | **Fixed**, with a new defect in the orphan step (C11-01). | `kingres_columnar.c:747` reads the stripes with `kc_latest_snapshot()` after the table lock (`:714`), so the snapshot is consistent with the lock; the list of tables and the second list (`kc_check_run`) report a table made during the call. Mutant M1 (the caller's snapshot instead): `t/036` fails 12 assertions (both levels). Mutant M13 (no "made during the call" row) fails 1; M14 (no row for a dropped table) fails 1. |
| C10-02 (cancel swallowed, wait for the catalog) | **Fixed.** | `kc_check_error_is_about_table` (`:829`), rethrow at `:869`; all catalog locks conditional (`kc_catalog_lock_nowait`, `kc_catalog.c`), each table conditional (`:714`). M2 (every error becomes a row again): `t/036` fails 12 (cancel and timeout cells). M3 (blocking lock on each table): `t/036` and `t/028` fail 6. M3b (blocking lock on the catalogs): `t/036` fails 18. Residual wait before the nowait step is argued in C11-09, not a defect of the claim as worded. |
| C10-03 (the "decided" rule ignores the nesting level) | **Fixed.** | `kc_write.c:1143-1147` (`GetCurrentTransactionNestLevel() == 1`). With `rc11`, both orders, both majors (cells A, B, K): a subtransaction begun after the extension's pre-commit and a refused insert in a critical section inside it: `WARNING: insert refused ...` then `ERROR: a call on columnar table "t" was refused earlier in this transaction, so it cannot commit`; the transaction aborts (rows 1..5, module row and user row absent), `PREPARE TRANSACTION` fails (0 prepared). Crit without a subtransaction after the extension's callback: warning only, the commit stands, the module's row is lost with the warning (the documented "decided"). Mutant M6 (rule without the nesting test): `t/031` fails 16. |
| C10-04 (relfrozenxid ahead of the metapage; log line) | **Fixed.** | `kc_am.c:595-597` (older of the current id and the table's id), `kc_am.c` rewrite path (`xid_cutoff` limited by `table_xid`), `kc_vacuum.c:276` (stored value). My runs: table made at 678, 2000 ids used, `TRUNCATE`: `relfrozenxid` 678 (not newer than either id of the metapage 2182/678); with a prepared transaction (2183) open: table made after it (2185), `TRUNCATE` (cx 2487): `relfrozenxid` 2185, `VACUUM FULL`: 2183 (metapage creating id frozen, table id 2185), `VACUUM`: 2185 (the id the file holds), `ALTER TABLE ... SET ACCESS METHOD heap` and back: 2183 then 2183 (creating id 2491, table id 0); after `COMMIT PREPARED` and `VACUUM`: 2492; never more than the file holds. `relminmxid` stays 1 (it follows the oldest multixact). Mutants M7 (no min with the table id in the new-file path): `t/038` fails 4; M12 (rewrite path): 1; M7b (log line prints the passed id): `t/038` fails 1. |
| C10-05 (in-place truncate on a table that exists outside the transaction) | **Partly** (C11-04). | `kc_am.c:632-633` refuses a table that was not made or given a new file in the transaction; M8 (no refusal): `t/031` fails 23. But the condition accepts a table made or rewritten in a parent subtransaction; PostgreSQL's own call is only for the current subtransaction. |
| C10-06 (every vacuum takes ACCESS EXCLUSIVE) | **Fixed**, with differences from heap's behaviour (C11-05, C11-06). | `kc_vacuum.c:134-170`, `:234`. Rule compared with `should_attempt_truncation` / `lazy_truncate_heap` of 18.7 and 19rc2: same thresholds (1000 pages or a sixteenth), same lock retry (50 ms, 5 s), same give-up without error; params are resolved by the core (`vacuum.c`) so `vacuum_truncate` and `VACUUM (TRUNCATE ...)` arrive in `params->truncate`. M4 (no threshold): `t/037` fails 1. M19 (option ignored): 2. M20 (tail not computed): 4. M17 (no retries): fails. M16 (the 1000-page limb removed) and M15 (failsafe test removed) survive; M5 (file-growth re-check removed) survives, argued equivalent below. |
| C10-07 (test quality) | **Partly** | Production builds now run what the assertion builds skip: 10728 and 10760 tests against 10079 and 10081; `t/013` runs and passes on both majors where the module is installed. Remaining: three cells per load order of `t/031` are skipped on every build and the message blames assertions (C11-08); `t/013` still skips by message without its module. |
| C10-08 (what "clean" covers) | **Fixed** (README), one gap (C11-03). | README states the first-page-only coverage, the locks kept, the memory. `t/036` tests the lock claims. The README's memory statement does not mention the shared lock table. |

## New findings

| Id | Severity | Place | Statement |
|---|---|---|---|
| C11-01 | medium | `kc_reconcile_ex`, `kc_catalog.c:989-1044` | The orphan step takes its stripe snapshot first and reads `pg_class` afterwards; a `DROP TABLE` or rewrite that commits between the two makes the old file's rows "orphans": `columnar_check()` returns `orphan_catalog_rows` and `columnar_check_clean()` is false for a healthy database, in 10 to 30 percent of runs under ordinary DDL. A monitor would page. |
| C11-02 | high | `kc_catalog.c:149-151` against `:138-146`, `kingres_columnar.c:894`, `kc_catalog.c:1000` | A second access method created over the extension's handler is a columnar table for the scan and the writer, but not for the check and not for the reconciliation: a routine `VACUUM` of any table deletes every stripe row of such a table (silent loss of all rows) and the check never looks at it. Needs a superuser to create the access method; nothing refuses it. |
| C11-03 | medium | `kc_check_guarded`, `kingres_columnar.c:714, 855` | The check holds a lock on every columnar table until the end of the transaction: with more columnar tables than the shared lock table has room for, both functions fail with `out of shared memory` (and take the room other sessions need while they run). Reproduced; at the default settings this is about 6000 tables. |
| C11-04 | low | `kc_am.c:632-633` | C10-05 is only partly fixed: a table made or rewritten in a parent subtransaction passes the test, an in-place truncate in a child subtransaction that is then rolled back leaves a committed table whose stripe rows have no data (`stripe_beyond_file`, `columnar table ... is corrupt`). |
| C11-05 | low | `kc_vacuum.c:134`, README | "Not while the wraparound failsafe is on" is not true for the table's own age: only a flag leaked by a heap table of the same command is looked at. Heap skips the truncation of a table past the failsafe age; the columnar vacuum takes `ACCESS EXCLUSIVE` and cuts, before it records the frozen id. |
| C11-06 | low | `kc_vacuum.c:139-142`, README "VACUUM and space" | A failed load whose aborted tail is below the threshold becomes permanent garbage once another load commits after it; before the fix a vacuum in between reclaimed it. No reuse exists, so the file grows without bound under repeated failed loads. |
| C11-07 | low | `kingres_columnar.c:177-181` | The gate setting is in production builds, delegable with `GRANT SET ON PARAMETER`, and can hold a check (and its locks) for up to two minutes per table for anyone who can take an advisory lock. |
| C11-08 | low | tests | `t/031` message wrong and three cells never run on any build; `t/027` poll proves nothing about the moment; `t/037` 5-second constant decides a verdict; the 1000-page limb and the failsafe have no test; a `pg_regress` test needs ICU without a skip. |
| C11-09 | note | operator view | What the operator is told when the check says `not_checked`; false alarms by design; what "clean" cannot see. |

Severity scale as in the earlier rounds: critical = wrong, lost or doubled committed data where the caller did nothing
PostgreSQL forbids, a crash, an unreadable table; high = the same behind a precondition that is possible but unusual,
or a loss that needs a deliberate act; medium = a wrong or hanging answer of an operations function, a limit that
defeats a stated purpose; low and note as before. I found no critical finding.

### C11-01 (medium) The orphan step reports healthy tables as orphans while DDL commits

Place: `kc_catalog.c:989` (`Snapshot catsnap = kc_latest_snapshot();` is the first statement of `kc_reconcile_ex`),
`:993-1002` (the dirty scan of `pg_class` for the files that are live, one `smgrread` per columnar table), `:1043-1044`
(`if (dry || kc_catalog_delete_tuple(...)) ndel++`: in the dry form, which `columnar_check` and `columnar_check_clean`
use, every candidate is counted without being looked at again).

The order is right for one race (a table made and committed after the snapshot cannot lose rows it was not seen to
have) and wrong for the other: a transaction that drops a table, or rewrites it (`VACUUM FULL`, `SET ACCESS METHOD`),
deletes the old file's stripe rows at its pre-commit and commits. A reader that took its snapshot before that commit
sees the rows; the dirty scan after it no longer sees the old file; the rows are "files no relation uses". The window
is the whole scan of `pg_class` and the first block of every columnar file, so it grows with the number of tables.
Nothing is damaged (the final check is clean every time), the answer is a false alarm.

Steps (19rc2 production; 150 small columnar tables, one table `rw`, one session running 100 times
`SELECT check_name, relation::regclass::text, left(detail, 60) FROM kingres.columnar_check()` while another runs DDL;
the checker's rows by kind, 100 checks each time):

```
DDL: ALTER TABLE rw SET ACCESS METHOD heap / columnar (150 times)  not_checked 61   orphan_catalog_rows 10
DDL: CREATE TABLE d(i) USING columnar; INSERT; DROP TABLE d (300x)   not_checked 76   orphan_catalog_rows 29
DDL: VACUUM FULL rw (150 times)                                      not_checked 91   orphan_catalog_rows 30
every case, afterwards:                  SELECT kingres.columnar_check_clean()  ->  t
sample row: orphan_catalog_rows | | 1 stripe catalog rows belong to files no relation uses
```

(On the 19rc2 build with assertions, with a writer, `columnar_compact`, a rewriter and `VACUUM` running together:
10 of 80 checks returned `orphan_catalog_rows`; the `not_checked` rows for the table being rewritten are the
documented, by-design ones.) With `fix` the same candidates are tried for deletion without waiting and the delete is a
no-op, so nothing is lost; only the count is wrong.

A fix must achieve: a row is reported as an orphan only if it is still visible, and its file still unclaimed, after
the `pg_class` scan: take a second snapshot after the scan and drop every candidate that is gone in it (a candidate
visible in the first snapshot means its table's `pg_class` row was committed before the first snapshot, so a missing
file means a drop or rewrite committed since, and that transaction deletes the rows). Add a test that commits a
`DROP TABLE` or a rewrite while the check is held between the two reads (a second gate), and one that runs the DDL in
a loop against the check and requires every answer to be clean.

### C11-02 (high) A second access method over the handler loses all rows to a routine VACUUM

Place: `kc_catalog.c:149-151` (`kc_rel_is_columnar`: `rel->rd_tableam == kc_get_routine()`, true for every access
method made with the extension's handler), against `kc_am_oid()` (`:138-146`, the access method with the name
`columnar`), the list of tables (`kingres_columnar.c:894`, `cl->relam == amoid`) and the list of live files in the
reconciliation (`kc_catalog.c:1000`, `cl->relam != amoid` then `continue`).

`CREATE ACCESS METHOD columnar2 TYPE TABLE HANDLER kingres.columnar_handler` is allowed to a superuser and nothing in
the extension refuses it. A table `USING columnar2` is handled by the extension for every read and write, but the
reconciliation does not count its file as live, so every stripe row of it is an orphan, and `VACUUM`,
`columnar_check(true)` and `columnar_reconcile()` delete them. The check does not list the table at all.

Steps (19rc2 production build; I did not run it on 18.7, the code path has no version test):

```
CREATE EXTENSION kingres_columnar;
CREATE ACCESS METHOD columnar2 TYPE TABLE HANDLER kingres.columnar_handler;
CREATE TABLE a2 (a int, b text) USING columnar2;  INSERT INTO a2 SELECT g, 'x' FROM generate_series(1,100000) g;
CREATE TABLE a1 (a int, b text) USING columnar;   INSERT INTO a1 ... 1000 rows;
SELECT count(*) FROM a2, kingres.columnar_check_clean();      ->  100000 | f     (a healthy database: false alarm)
-- cut the file of a2 at the operating-system level (a damaged table)
SELECT kingres.columnar_check_clean(), count(*) FROM kingres.columnar_check();   ->  f | 1   (the one row is orphan_catalog_rows; a2 is not named)
CREATE TABLE a3 (a int, b text) USING columnar2;  INSERT INTO a3 SELECT g, 'y' FROM generate_series(1,50000) g;
SELECT count(*) FROM a3;                                       ->  50000
VACUUM a1;                                                     -- another table
SELECT count(*) FROM a3;                                       ->  0
SELECT count(*) FROM kingres.columnar_stripes('a3');           ->  0
```

The rows are gone for good (the catalog rows are deleted by a committed transaction; the file stays). The damage is
done by a vacuum of an unrelated table, or by the vacuum of `a3` itself, or by the first `columnar_check(true)`, with
no error and no warning. I believe the reconciliation predates the last round, and I did not find the point in the earlier reviews; the round's strict `columnar_check_clean()` sharpens it, because a script that tests it now believes it covers
"every columnar table" (README: "never true for a table that was not looked at"), and a table of the alias is not
looked at.

Whether it can happen by accident: it needs a superuser to create the second access method, which is unusual (a
rename of an access method does not exist in PostgreSQL, a restore re-creates the access method from the extension
script). I rate it high and not critical for that precondition; the loss, when it happens, is total and silent.

A fix must achieve: one definition of "a columnar relation" everywhere: identify the relations by the access method
whose handler is the extension's handler (compare `pg_am.amhandler` with the handler's oid, or test `rd_tableam`
through the relation cache), in the list of tables, in the live-file list, and in every other place that tests
`relam`; or refuse in the handler any access method name other than `columnar`; and never delete rows of a file whose
owner could not be classified. Add a test with a second access method over the handler, a vacuum of another table,
and a comparison of rows before and after.

### C11-03 (medium) The check needs a lock-table entry for every columnar table at once

Place: `kingres_columnar.c:714` (`ConditionalLockRelationOid(relid, lockmode)` inside the per-table subtransaction)
and `:855` (`ReleaseCurrentSubTransaction()`, which hands the lock to the parent), README ("What it leaves behind: an
`ACCESS SHARE` lock on every columnar table until the end of the caller's transaction").

The locks are kept on purpose and the README says so; what it does not say is that the shared lock table has a fixed
size (`max_locks_per_transaction` times `max_connections` plus prepared transactions), that this holds one entry for
each table for as long as the call runs, and that an error "out of shared memory" ends the call and is raised to the
caller (it is rightly not turned into an `unreadable` row: C10-02).

Steps (19rc2 production, `max_locks_per_transaction = 10`, `max_connections = 10`, no prepared transactions, 700
columnar tables):

```
SELECT kingres.columnar_check_clean();           ->  ERROR:  out of shared memory
SELECT count(*) FROM kingres.columnar_check();   ->  ERROR:  out of shared memory
```

With the default settings (64 and 100) the lock table has 6400 entries; a database with that many columnar
tables (a table per partition of a time series is the use the extension is for) cannot be checked at all, and
well below that number the check takes entries that other sessions need, so they may fail with the same error while it
runs. Nothing in a monitor's output says why; the README's "what it costs in memory" paragraph says nothing grows with
the number of tables.

A fix must achieve: the check releases each table's lock when it has looked at it (`UnlockRelationOid` after
`relation_close`, a read leaves nothing that must be protected until commit), keeping the lock only when the
caller's transaction already held it; or the README states the shared-memory cost and the call raises a clear error
early. Add a test with a small lock table and more tables than it holds.

### C11-04 (low) The in-place truncate test still accepts a table of a parent subtransaction

Place: `kc_am.c:632-633`:
`if (rel->rd_createSubid == InvalidSubTransactionId && rel->rd_firstRelfilelocatorSubid == InvalidSubTransactionId) ereport(ERROR ...)`.

PostgreSQL calls the in-place truncate only when the relation was made, or given its new file, in the current
subtransaction (`ExecuteTruncateGuts` compares `rd_createSubid` and `rd_newRelfilelocatorSubid` with the current
subtransaction id); an abort of that subtransaction removes the file. The extension's test is true for a table made
or rewritten in any open (sub)transaction, so a child subtransaction that truncates in place and is rolled back
brings the catalog rows back and not the data.

Steps (`rc11`'s SQL function `rc11_ntrunc(regclass)`, which calls `table_relation_nontransactional_truncate`; 18.7 and
19rc2, identical):

```
CREATE TABLE x3 (a int) USING columnar; INSERT INTO x3 SELECT generate_series(1,50000);
BEGIN; ALTER TABLE x3 SET ACCESS METHOD heap; ALTER TABLE x3 SET ACCESS METHOD columnar;
SAVEPOINT s; SELECT rc11_ntrunc('x3'); ROLLBACK TO s; COMMIT;           -- no error
SELECT count(*) FROM x3;      ->  ERROR:  columnar table "x3" is corrupt: stripe 1: the stripe lies beyond the end of the file
columnar_check()              ->  stripe_beyond_file | stripe 1 lies at blocks 1 to 15, the file has 1 blocks
columnar_check_clean()        ->  f
```

(A table made in the parent with 200000 rows gives `duplicate key value violates unique constraint
"columnar_stripe_pkey"` at the commit instead, which aborts the transaction.) PostgreSQL never makes this call, a
module must; hence low, but it is the corruption C10-05 asked to prevent, by the same call, one level down.

A fix must achieve: compare with `GetCurrentSubTransactionId()` as PostgreSQL does (`rd_createSubid` or
`rd_newRelfilelocatorSubid` equal to it), and add the nested cell to `t/031`.

### C11-05 (low) The failsafe is not evaluated for a columnar table's own age

Place: `kc_vacuum.c:134` (`if (VacuumFailsafeActive) return;`), README ("and the wraparound failsafe is not on").

`VacuumFailsafeActive` is set by heap's own check of the table being vacuumed; for a columnar table nothing sets it
(it is only a global that an earlier heap table of the same `VACUUM` command may have left set). Heap, past the
failsafe age, skips truncation and index work so as not to need new transaction ids; the columnar vacuum does what it
does at any age, and it does the truncation before it records the frozen id (`vac_update_relstats` is after it).

Steps (19rc2 production, `autovacuum_freeze_max_age = 100000`, `vacuum_failsafe_age = 100000`; table `c` columnar
with an aborted tail, table `h` heap with a long dead tail; 300000 transaction ids consumed):

```
before:  c blocks 1183   h blocks 10811        ages of relfrozenxid: c 300008, h 300006
VACUUM (VERBOSE) h;   WARNING:  bypassing nonessential maintenance of table "...h" as a failsafe after ...    (h not truncated)
VACUUM (VERBOSE) c;   (no warning)
after:   c blocks 198    h blocks 10811       (c was cut: the exclusive lock was taken)
```

Why it matters: taking `ACCESS EXCLUSIVE` on a logged table writes a lock record, which assigns a transaction id
(`LockAcquireExtended` calls `LogAccessExclusiveLockPrepare`, which calls `GetTopTransactionId`); at the limit where the
server refuses to assign ids, that is the reason heap has the failsafe. I did not reach that limit (about 2.1 billion
ids), so I did not reproduce a failed vacuum; by the code a columnar vacuum of a table with a worthwhile tail would
fail there before the frozen id is advanced, in exactly the case the frozen id must advance. Mutant M15 (the line
removed) is not killed by any test.

A fix must achieve: decide the failsafe for the table's own age the way heap does (the age of `relfrozenxid` and
`relminmxid` against `vacuum_failsafe_age`, with the same floor of 105 percent of the freeze age) and skip the
truncation then; record the frozen id before the truncation, not after. Add a test with a low failsafe age.

### C11-06 (low) Aborted space below the threshold becomes permanent

Place: `kc_vacuum.c:139-142` (the threshold), README "VACUUM and space".

The rule is heap's: a tail is cut when it is at least 1000 pages or a sixteenth of the file. Heap reuses the free
space of the middle of the file; the columnar table never does. A failed load that leaves a tail below the threshold is
cut by no vacuum, and the next load that commits builds on top of it: from then on the pages are in the middle of the
file for good (before this round a vacuum between the failure and the retry gave them back, whatever their size).

Boundary runs (production build; row width 16 bytes, about 507 rows to a page; `VACUUM` after each):

```
file 986 blocks, tail 197 (a sixteenth is 61)          -> cut to 789      as the rule says
file 17731 blocks, tail 788 (1000 and a sixteenth: 1108) -> left
file 17928 blocks, tail 985                             -> left (just below 1000)
VACUUM (TRUNCATE false) with a tail 985 of 1774; vacuum_truncate = false  -> left, both
VACUUM (TRUNCATE true) with vacuum_truncate = false                       -> cut
growth: one row, then 4 times {rolled-back load of 150000 rows (197 pages), commit one row, VACUUM}:
        5 rows, 794 blocks (6 needed); VACUUM FULL -> 2 blocks
```

So each failed load of up to 999 pages (8 MB) is unreclaimable except by `VACUUM FULL` once anything commits after it, and a
workload of failing and retried loads grows without bound. The README's "Limits" paragraph says space in the middle is
not reused, but not that the round turned a reclaimable tail into one. A fix may be a documented choice; if it is a
choice, say so in the vacuum paragraph and give the operator a way to see the garbage (the file size against the
stripes' bytes: `columnar_info` could report it), or let the writer start its next stripe at the first needed block
when it holds the lock for another reason.

### C11-07 (low) The gate setting is a production knob

Place: `kingres_columnar.c:177-181`; README ("Developer setting, superuser only").

Unlike `fault_point`, which exists only in builds with assertions, `check_gate` is defined in every build. Who can set
it: a superuser, and any role that was given `GRANT SET ON PARAMETER kingres_columnar.check_gate` (run: an ordinary
role is refused with `permission denied to set parameter`; after the grant it sets the value and `SHOW` returns it; it
still cannot run the check, `permission denied for function columnar_check_clean`). What it lets an ordinary role do:
nothing by itself; but a check running with the gate set (set by an administrator, for instance as a role default by
`ALTER ROLE ... SET`) waits on the advisory lock `(key, 0)`, which any role can take: with the key 77 set for the
superuser and an ordinary role holding `pg_advisory_lock(77, 0)` for 8 seconds, `columnar_check_clean()` returned
after 6 seconds (one table; the wait is 10 ms steps, bounded at two minutes per table, so for N tables up to 2N minutes).
While it waits the call holds the `pg_class` and stripe catalog locks, the locks of the tables already looked at and,
with `fix`, `SHARE UPDATE EXCLUSIVE` on them (blocking vacuum and DDL on them). The wait can be cancelled.

A fix must achieve: compile the setting in only where the tests need it (assertion builds, as `fault_point`), or make
the wait require the setting's owner to hold the lock key himself; and say in the README that it exists in every
build until then.

### C11-08 (low) Tests

* `t/031` (the diagnostic at the end of each load order): on a production build three cells per order (the reads
  at the `res_*` release phases, `031_entry_points.pl:305`) are skipped, and the text says "this server is built with
  assertions"; the text for "every cell was run" can never appear, so those three cells run on no build at all.
* `t/027:307` polls `pg_stat_activity` for a query text containing `pg_sleep(4)`; the whole script is one query
  message, so the text is there from the first moment, before `BEGIN` has run; the `KILL` that follows does not prove
  that the client was gone "at commit". The verdict (`100` rows or none) holds either way, so the cell passes without
  testing the moment (the same family as the helper race the author fixed in `t/036`: asking about a state by a
  pattern that is true earlier). `t/003` has the same shape with `pg_sleep(3)` and a verdict that cannot fail.
* `t/037` (retries): the vacuum waits at most 5 seconds for the lock; the test sees the wait event, then rolls the
  reader back. On a loaded machine the 5 seconds can pass first; the vacuum gives up, and
  `is(blocks('c'), $live, 'the reader went away while the vacuum waited ...')` fails. A fixed time decides the verdict.
* Not killed: M16 (the 1000-page limb of the threshold: every test table is smaller than 16000 pages, so only the
  sixteenth is exercised) and M15 (the failsafe line). M5 (the file-size re-check under the lock) is not killed and is
  argued equivalent: the re-check can only differ when the file grew, the grown part is the pages of a stripe whose
  catalog row is committed (a writer holds `ROW EXCLUSIVE` until its end, so none is in progress when the exclusive
  lock is granted; a prepared transaction's rows count as in progress), and the recomputed `need` already covers
  committed stripes, so the cut is to the same place with or without the check; I could not construct a race that the
  re-check decides.
* The `columnar_review` regression test needs a server with ICU and has no skip.
* `t/036`'s "no verdict depends on how long anything takes" holds; I found no sleep that decides a verdict there
  (its gate helper waits for the call's pid at the gate).

### C11-09 (note) What the operator is told, and what the check cannot see

* A session that holds the stripe catalog (or `pg_class`, its index, its TOAST table or the TOAST index) locked for a
  long time makes every check say one `not_checked` row with no table ("pg_class or the stripe catalog ... is locked by
  another transaction ...; no table was checked, and this is not a clean result"). It cannot hide damage (the answer is
  never clean), but nothing tells the operator who holds the lock: no process id, no log line. A monitor sees `f`
  for ever. Naming the blocking process (from the lock table, which a conditional lock attempt can read) would make it
  actionable.
* Rows that are false alarms by design, and which a monitor will see on a database with ordinary DDL: a table
  dropped or made while the call runs is `not_checked`; a table that another transaction holds `ACCESS EXCLUSIVE`
  (rewrite, `TRUNCATE`, a vacuum's truncation) is `not_checked` (46 of 80 runs with a rewrite loop); with `fix` a
  table that a vacuum holds `SHARE UPDATE EXCLUSIVE` is `not_checked`. `columnar_check_clean()` is therefore a good
  "is it all fine right now" for a quiet database and a flapping signal for a busy one; the README could say how to
  use it (retry).
* "Clean" is judged against the stripes visible now: damage to a stripe that only an old snapshot can still see (replaced
  by compaction, not yet invisible to every snapshot) is not looked at; and the other pages of a stripe are not read.
  The README says the second, not the first.
* The wait before the first conditional lock: `kc_catalog_oid()` (called first in `kc_catalog_lock_nowait`) resolves
  the catalog by name through the system caches when the backend's cache is cold, which takes ordinary, waiting locks on
  `pg_extension` and `pg_class`. I argued this from the code and did not test it; a session that holds `pg_class`
  `ACCESS EXCLUSIVE` stalls nearly every statement of a database in the same way, so I do not count it as a defect of
  the check, but the README's "never waits for a lock, for every lock it takes" has this exception, and the next
  sentence (relation cache reads) does not name it.
* Each vacuum scans the whole stripe catalog twice more than before (`kc_needed_blocks`, once before and once under the
  lock) besides the reconciliation; the cost is the size of the catalog times the number of tables vacuumed. Not a
  problem at the sizes I ran (about 35 stripes to 100 thousand rows); a note for a catalog of millions of stripes.

## Questions the task asked, answered

**The check reading the stripe catalog as of now.** Table created, dropped, rewritten (`VACUUM FULL`, `SET ACCESS
METHOD`), truncated, or compacted while the check runs: the lock is taken before the snapshot (`:714` then `:747`), and
a table that cannot be locked, was dropped, or is not columnar any more is a `not_checked` row, so the stripes and the
file are read under a lock that excludes the rewrite, the truncation and the vacuum's cut (`SHARE` side: `ACCESS SHARE`
conflicts only with `ACCESS EXCLUSIVE`). `t/036` covers REPEATABLE READ and SERIALIZABLE callers (a table made after the
snapshot, stripes committed after it, both damaged; the fresh session and the old one agree; M1 fails 12 assertions) and
the gate for a table dropped or made during the call; I ran the other levels only through my own loops in READ
COMMITTED. Damage visible only in an old snapshot: not looked at (C11-09). Can `columnar_check_clean()` return true for a
table it did not look at: yes for a table of a second access method over the handler (C11-02); no for a locked table, a
dropped one, one made during the call, one in another schema or of another owner, a partition, a materialized view (a
table made during the call is in the second list). Can it report damage for a healthy table that is merely being
written: a table that is being written, compacted and vacuumed is never reported (my writer/compaction/`VACUUM w` loop:
no row for `w` in 80 checks); the false alarm is the orphan row of C11-01, produced by DDL, not by writing.
What the gate lets an ordinary role do: C11-07.

**The check never waiting.** Locks it takes, all conditional: `pg_class` and the stripe catalog (`AccessShare`, plus
`RowExclusive` with `fix`), the catalog's primary key index (`AccessShare`), its TOAST table (with the catalog's mode)
and each index of the TOAST table (same), and each columnar table (`AccessShare`, `ShareUpdateExclusive` with `fix`).
Errors it raises again: every error whose class is not 22, XX, 58 or `0A000` (`feature_not_supported`): query cancel,
statement timeout (57014), lock timeout, deadlock, shutdown (FATAL is not catchable), out of memory and "out of shared
memory" (53), a failure of the transaction machinery. Errors it turns into a row: only the table's own damage (corrupt
data, I/O, missing file, undecodable value, unsupported format). Cancel and timeout: `t/036` and mutant M2. The
`not_checked` for the catalog cannot hide damage for ever in the sense that it is never clean; what tells the operator
is C11-09.

**Vacuum truncating only when it is worth it.** Compared with heap's rule in 18.7 and 19rc2 (`vacuumlazy.c`:
`should_attempt_truncation`, `lazy_truncate_heap`; `vacuum.c` for the option): same two thresholds and "greater than
zero", same failsafe skip in intent, same conditional lock, 50 ms retries for 5 seconds (checking for interrupts, wait
event `VacuumTruncate`), same give-up at DEBUG2, same "file grew: give up". Differences: no scan for the last page
with data (the tail comes from the catalog), no loop that continues while a waiter interrupts the scan (nothing to
interrupt), no evaluation of the table's own age for the failsafe (C11-05). Tail just below and above the thresholds:
see C11-06 (197 of 986 cut, 788 and 985 of 17731 and 17928 left). The conditional lock against a reader (t/037:
waits for the reader, cuts when it leaves; gives up after 5 s with no error), a writer (a writer holds `ROW EXCLUSIVE`,
so no lock is granted while one is open; the vacuum gives up after 5 s) and a hot standby query (t/037: a vacuum with
nothing to cut does not cancel the standby query, heap twin the same; a cut does cancel it, as heap's would). The
re-check under the lock: argued above (C11-08, M5). `vacuum_truncate` off, `(TRUNCATE false)`, `(TRUNCATE true)`:
tested by me (above) and by `t/037` (M19 fails 2 assertions). Failsafe: C11-05. A table that is never truncated any
more: C11-06; aborted space below the threshold is not reused.

**The frozen id after `TRUNCATE` and `VACUUM FULL`.** Never claims more than is true in the orders I ran (create,
write, truncate, rewrite, vacuum, `ALTER TABLE ... SET ACCESS METHOD`, old transaction and prepared transaction
open); the numbers are in the table above. The autovacuum line prints the stored value (`t/038`, M7b).

**In-place truncate refused.** Every path by which PostgreSQL calls it still works: `TRUNCATE` of a table made in
the same transaction (with a following insert, and with `RESTART IDENTITY`: the serial restarts at 1), a table made and
truncated in a subtransaction that rolls back (the table is gone), a table truncated in a savepoint that rolls back
(rows come back: 100), a table made in the transaction with a savepoint around the truncate (the table's own
`TRUNCATE` gets a new file; rolled back, the table is empty then holds the next insert), `COPY ... FREEZE` into a table
made in the transaction and into one truncated in it; `ON COMMIT DELETE ROWS` cannot be tested (a columnar table
cannot be temporary: `ERROR: columnar tables cannot be temporary or unlogged`). A refused call leaves the table
readable: with `rc11` at `pre_commit`, a refused in-place truncate of a committed table is an error, count and check
unchanged (cell E, G); in a critical section a warning, the table unchanged (F). Not refused where it should be: C11-04.

**The "alive inside a subtransaction begun after the extension's pre-commit" rule, with a test module of my own.**
Cells run with `rc11`, both load orders, 18.7 and 19rc2 (assertion builds), and the order "before" on the 19rc2
production build; all cells gave the same outcome on every build:

| Cell | Script at the event | Result |
|---|---|---|
| A | `pre_commit`: subtransaction, critical section, insert into the table | warning, then error at the subtransaction's pre-commit, transaction aborts, 5 rows (module's and user's row absent) |
| B | the same at `pre_prepare` | same, 0 prepared transactions |
| C | `pre_commit`: critical section, insert (no subtransaction) | order "before" (decided): warning, the commit stands, the module's row is lost with the warning (user row present); order "after" (live): error, abort |
| D | `pre_commit`: insert, no critical section | the row is written at once, present once |
| E | `pre_commit`: in-place truncate of a committed table | error "cannot truncate columnar table ... in place", table readable, check empty |
| F | the same inside a critical section | warning (decided) or error (live), table unchanged |
| G | in-place truncate, then raise | error, abort, check empty |
| H, I | `columnar_check_clean` at `pre_commit` in a critical section, with and without a subtransaction | `WARNING: columnar_check_clean refused: inside a critical section`, returns false |
| J | `columnar_check_clean` at `commit_sub`, `abort_sub`, `abort`, `commit` | `WARNING: ... refused: the transaction is not in progress`, false |
| K | a user savepoint; the module inserts inside a critical section at `pre_commit_sub` | warning, error, abort |

A first version of my module crashed the server in cells H and I (it called the function through the function manager,
which allocates in a critical section: an assertion in `mcxt.c`); that was my module's call, and the direct call
(what the earlier reviewer's module did) is refused cleanly.

## What held under attack

* Crash recovery, a promoted standby (archive recovery with hot standby on), base backup and restore, `pg_dump` and
  restore, aborted subtransactions, SERIALIZABLE transactions in the workload, compaction and rewrite between loads:
  no wrong answer, no lost or doubled row, against a heap twin, on both majors, on production builds; the stripe
  catalog, the check and the file sizes agree after each (my seeds 5, 6, 7). The suites on four builds (about 41000
  assertions and tests in all) pass.
* The strict check does not hang: a cancel at the gate and a statement timeout end the call; a catalog that is locked
  by a prepared `LOCK TABLE`, `REINDEX INDEX` or `REINDEX` of the TOAST table gives one `not_checked` row at once, with
  and without `fix`, with `statement_timeout` not needed (t/036, M3b kills the blocking variant).
* The "decided / alive" rule in both load orders on both majors (above).
* The vacuum's lock behaviour against a reader and a hot standby query (t/037 on four builds).
* The frozen id and the autovacuum line (above; `t/038`, three mutants killed).
* Version tests: only in `src/pg_compat.h` and the module's own compat header (`grep` for `PG_VERSION_NUM` finds
  nothing else in source, SQL, Makefile or meson file; `t/012` reads the version in Perl, as before).
* Privileges: the gate setting needs a superuser (or a delegated `SET` privilege); the check functions are
  superuser-only (`permission denied for function`, and the body still says `must be superuser`).
* Mutants: 15 of 18 killed (M1, M2, M3, M3b, M4, M6, M7, M7b, M8, M12, M13, M14, M17, M19, M20); M5 is equivalent
  (argued in C11-08); M15 and M16 are not covered by any test (C11-08).

## Verdict

For data that matters, within what the extension claims (append-only analytic data, insert-only, no indexes, serial
scan): on both majors, on builds with and without assertions, I found no wrong answer, no lost or duplicated row and
no unreadable table in ordinary use, through crash recovery, a promoted standby, base backup and restore, dump and
restore, aborted subtransactions, SERIALIZABLE transactions, compaction and rewrite; the last round did what it
said for C10-01, C10-02, C10-03, C10-04 and C10-06 (every one has a test that fails without the fix), and for C10-05
partly. The extension can be used on a primary for such data **provided no second access method is created over its
handler** (C11-02: a routine VACUUM then deletes that table's rows without a word; the fix is small and should be made
before a beta, or at the least refused in the handler and written in the README).

It is not yet fit to carry a monitor: `columnar_check_clean()` gives a false alarm in 10 to 30 percent of runs while
tables are dropped or rewritten (C11-01), and it cannot run at all in a database with more columnar tables than the
lock table has room for (C11-03). Those two should be fixed before the check is recommended in the README; the other
findings are low (a failsafe that is not the table's own, permanent garbage from failed loads below the threshold, a
production knob, three test points, and one corner of a hostile module).

Another review round is needed, but a short one: the fixes for C11-01, C11-02 and C11-03 (and C11-04, C11-05 if
chosen) with their tests, and a look at whatever else uses `relam` or the access method name instead of the handler.

## What I did not check

* On 18.7 my own scripts for the truncate paths, the frozen id, the alias access method, the lock footprint, the vacuum thresholds and the check races were not run (they ran on 19rc2, the frozen-id and truncate-path scripts on the build with assertions, the others on production); the code has no version test outside the compat header and the suites pass on 18.7, but I have no 18.7 output of those scripts (the workload, the module cells and the in-place truncate case ran on both).
* SERIALIZABLE conflicts with my own workload (the suite's `t/009` passes on all four builds); my workload used
  SERIALIZABLE transactions without a concurrent conflicting writer.
* A streaming standby of my own (my standby is archive recovery with hot standby on; the suite's `t/002`, `t/033`
  and `t/037` use streaming and pass). Queries on a standby against my own vacuum, apart from `t/037`.
* The exact boundary at 1000 pages (my tails were 788 and 985 against files of 17731 and 17928 pages; I could not
  make a tail of exactly 1000), and the cut when a tail of at least 1000 pages exists in a file above 16000 pages
  (M16 is untested; I argued the code, not a run).
* The failsafe at the real limit (about 2.1 billion ids), and any failure of the exclusive lock's id assignment there
  (argued from the lock manager, not run).
* The cold-start wait on `pg_class` and `pg_extension` before the first conditional lock (argued).
* A write by the caller while the check runs at every isolation level other than READ COMMITTED in my own loops
  (the suite covers REPEATABLE READ and SERIALIZABLE for the snapshot, not for the races).
* The earlier install script shapes (databases with the old trigger functions), a restore through another access
  method name, `pg_upgrade`, other platforms, address and thread sanitizers, `valgrind`, and the other extensions
  (`kingres_shard`, `kingres_ledger`) that sit on columnar tables.
* Whether C11-02 can arise without a deliberate `CREATE ACCESS METHOD` by a superuser (I found no other way).
