<!-- SPDX-License-Identifier: AGPL-3.0-only -->
# Closing check of kingres_columnar 0.1 (the fixes for C12)

Reviewer: an independent reviewer. Date: 2026-11-18.

## Scope

The round is the 14-file diff between the tree before the fix round and the committed tree. I read the twelfth review first, then
every hunk of the diff, and the code the hunks touch (the callers of the changed functions, `kc_check_guarded`,
`kc_check_one`, `kc_check_list_tables`, `kc_live_files`, the write entry points). I did not widen beyond that.
I worked on copies of the export and of both installations (PostgreSQL 19rc2 and 18.7, both with assertions).

## What was run

| Run | 19rc2 | 18.7 |
|---|---|---|
| Clean build with `-Werror` (the extension, PGXS) | no warning | no warning |
| TAP suite, `t/001` to `t/042`, one prove run | 42 files, 10265 tests, all pass (460 s) | 42 files, 10263 tests, all pass (436 s) |
| `pg_regress` (`make installcheck`, 11 tests, with the new `columnar_security` output) | 11 of 11 pass | 11 of 11 pass |
| Nine one-line mutants (below) | built with `-Werror` and run | not run |
| Two probes of my own | run | not run |

The new and changed test files ran inside the full suite (`t/036`, `t/039`, `t/042` green on both majors). No timing failure
occurred, so no rerun was needed. Everything marked "read" below I only read.

## The five findings

| Finding | Verdict | Evidence |
|---|---|---|
| C12-01 a stand-down is visible | **Fixed** | `kc_reconcile_ex` returns -1 on both stand-down paths (the first and the second read of `pg_class`); `kc_check_run` turns it into a `not_checked` row with no table and the reason; `columnar_check_clean()` is false; `columnar_reconcile()` returns NULL with a WARNING; `VACUUM` stays quiet (`kc_catalog_reconcile(NULL, true)`; the `why_out` pointer is checked for NULL). The unreadable-metapage branch is covered too (`t/039` 5b damages a metapage on disk). My probe 2: with an orphan row present and a handler that errors, `columnar_check_clean()` is `f`, `columnar_check()` returns the one `not_checked` row, `VACUUM` is quiet and deletes nothing, `columnar_reconcile()` is NULL with the WARNING; with the handler fine again the orphan is reported and then deleted. Mutants C12-01a and C12-01b killed. |
| C12-02 `not_checked` memory | **Fixed** | One `GetLockStatusData` copy per 64 rows, freed when used up and at both exits of `kc_check_run`; the gate poll frees its copy; the tuplestore values are freed per row. On an error exit the copy lives in the caller's context (the row code runs in the context that `kc_check_guarded` switches back to), so it goes with that context. `t/042` (800 locked tables, each row names the holder, peak memory growth under 14 MB) passes; the old behaviour grew by 37.7 MB and fails it (mutant C12-02). One accuracy point is C13-02 (note). The README sentence was corrected. |
| C12-03 one definition for the guard | **Fixed** | `kingres.columnar_is_columnar_am(oid)` is `STRICT STABLE`, security invoker, pinned `search_path`, revoked from PUBLIC (the `columnar_security` output shows permission denied for an ordinary role and the function listed with its pin). A NULL oid gives NULL (not true in the guard); a dropped or invalid oid gives `KC_AM_UNKNOWN`, so false. Both queries of the guard use it. `t/039` section 6 (a second SQL function over the C symbol: foreign key, AFTER ROW trigger and publication, for a real and an alias table, with a heap table left alone) passes. Mutants C12-03a (publication query) and C12-03b (the other query) killed. Read: the guard now calls the handler of the access method of each table in the command, so a broken foreign handler makes DDL on its own tables fail; the answer is cached. |
| C12-04 write to a table that is not recognised | **Fixed** | `kc_write_check_state` (once per row on the row path, once per batch on the late path; one pointer comparison) raises `0A000` when `rd_tableam` is not the extension's routine. My probe 1 found no legitimate path refused. `t/039` section 8 (insert of many rows, one row, `COPY`, CTAS into the copy access method) passes; mutant C12-04 killed. |
| C12-05 tests for the two surviving mutants and the new cells | **Fixed** | M10 (shallow snapshot copy) now fails `t/036` test 102; M14 (cache never cleared) now fails `t/039` test 56. Cells for C12-01 and C12-03 exist in `t/039`. |

The handler-error guard (`kc_am_class_guarded`, called C12-06 in the README) is not one of the five findings; I read it line by line and tested it.

### The handler-error guard

- The pattern is the one of `plpgsql` and of `kc_check_guarded`: `BeginInternalSubTransaction`, switch back to the caller's context,
  and on the error path `MemoryContextSwitchTo(oldcxt)`, `CopyErrorData`, `FlushErrorState`,
  `RollbackAndReleaseCurrentSubTransaction`, switch back again, restore `CurrentResourceOwner`. The copy of the error lives in the
  caller's context and is freed or re-thrown. Right.
- What is re-raised: class 57 (cancel, shutdown) and out of memory; everything else is logged and becomes "unknown". A FATAL is not caught by `PG_CATCH`. Right.
- Where a subtransaction is not begun: in a critical section, with no transaction in progress, in a committing subtransaction, in parallel mode, and
  where `kc_error_forbidden()` holds; there the old unguarded call is used (the behaviour before the round). `columnar_reconcile()`
  uses the guard only at nest level 1, which my probe confirms (below). `VACUUM` is always at the top level.
- The open scans (the dirty `systable` scan of `kc_live_files`, the catalog heap scan of `kc_check_list_tables`) hold their buffers,
  snapshot and lock under the parent's resource owner, which the abort of a subtransaction does not release. Memory: no growth over 1500 calls that each hit the error path (the sum over the backend's memory contexts grew by 0 bytes).
- Only a cache miss calls the handler, so the normal path (the cache of 8 entries) costs nothing. See C13-01 for the miss path under a broken handler.

## The mutants

Each is one line (C12-02 is two edits in one function: the old behaviour needs a copy per row and no free), built with `-Werror`, run on 19rc2.

| Mutant | Fix it removes | Test run | Result |
|---|---|---|---|
| C12-01a `if (d < 0)` becomes `if (d < -5)` in `kc_check_run` (no `not_checked` row) | the check shows the stand-down | `t/039` | killed (6 assertions) |
| C12-01b `if (n < 0)` becomes `if (n < -5)` in `columnar_reconcile` (no NULL, no WARNING) | reconcile says so | `t/039` | killed (5) |
| C12-02 lock data read per row and never freed (`KC_LOCKDATA_USES 1`, both `pfree` removed) | memory linear | `t/042` | killed (37.7 MB against a bound of 14 MB) |
| C12-03a the publication query asks the handler function again | one definition | `t/039` | killed (1) |
| C12-03b the foreign key and trigger query asks the handler function again | one definition | `t/039` | killed (2) |
| C12-04 `if (!kc_rel_is_columnar(rel))` becomes `if (0)` | the write is refused | `t/039` | killed (4) |
| M10 `cs->snap.xip = base->xip` (shallow copy) | deep copy of the snapshot | `t/036` | killed (1) |
| M14 the invalidation callback of the class cache does nothing | cache cleared | `t/039` | killed (1) |
| Handler guard off (`memo->guard && 0`) | the guard of the handler call | `t/039` | killed (4) |

Nine of nine killed; none survives, among them the two that survived the twelfth review. All mutant trees were removed after reading the result.
One slip of mine, for the record: the last mutant stayed installed in my copy of the installation and my first probe ran against it; the probe contradicted `t/039`, which is how I saw it. I rebuilt the real tree and ran both probes again; the results below are from the real tree.

## Probes of my own

1. **The write-path check against every way of writing a real columnar table** (19rc2, one database, every statement checked):
   multi-row `INSERT`, one-row `INSERT`, `COPY FROM PROGRAM`, `CREATE TABLE AS USING columnar`, `SELECT INTO` with `default_table_access_method = columnar`,
   `CREATE MATERIALIZED VIEW USING columnar` and `REFRESH`, `ALTER TABLE SET ACCESS METHOD columnar` of a heap table with inserts after it, `VACUUM FULL`,
   `columnar_compact`, a table moved heap to columnar to heap to the alias access method to columnar inside one transaction with an insert after each move, a rolled-back move followed by inserts,
   a move to heap and back with inserts in one transaction, `TRUNCATE` and insert in one transaction, a table of the alias access method (create, insert, CTAS), a partition of a
   partitioned table, a savepoint with a rollback to it, a prepared transaction that inserts and is committed, and a data-modifying `WITH ... INSERT ... RETURNING`.
   Result: every statement succeeded, every count was right (for example the five-move table 50 rows, the truncated table 50 rows), and `columnar_check()` returned no row. No legitimate
   path received the new error. (Temporary columnar tables are refused by the extension for another reason, as before.) The late-flush path is exercised by the suite (`t/022`, `t/024`, `t/027`, `t/030`), green on both majors.
2. **The stand-down seen through `VACUUM`, the check and `columnar_reconcile()` while a handler errors** (19rc2; an access method whose handler raises an error, 150 tables of it interleaved with 150 heap tables, an orphan stripe row, a columnar table of 1000 rows):
   with the handler failing, `columnar_check_clean()` is false; `columnar_check()` has one `not_checked` row with the reason; `VACUUM` of the live table gives no error and keeps the orphan row; `columnar_reconcile()` is NULL with the WARNING;
   the same inside a savepoint (the call works, the transaction goes on, the table reads 1000 rows); `columnar_check_clean()` inside a plpgsql exception block (nest level 2) works and answers false;
   `columnar_reconcile()` inside such a block raises the handler's error, as the README says (the documented exception; the session stays fine); with the handler working again the orphan is reported and deleted (count 1);
   1500 calls in one session: no memory growth. Nothing crashed and the server log has no assertion failure.

## New findings

No critical, high or medium finding. One low and one note.

### C13-01 (low) A broken handler is called again at every switch of access method in a loop, with one log line each

Place: `kc_catalog.c`, `kc_am_class_memo` (it keeps only the last access method) and `kc_am_class_guarded` (an error is not cached, correctly, because "unknown" depends on the transaction).
Steps: an access method whose handler raises an error; 150 tables of it interleaved with 150 heap tables (so `pg_class` alternates); `columnar_check_clean()` with the handler failing. Each switch back to
the broken access method begins a subtransaction, fails, rolls back and writes one `LOG` line, on each of the three passes over `pg_class` (the table list and the two reads of the orphan step); 1500 calls wrote about 672000 lines. A database with
a few thousand tables of a broken handler would write that many lines per call of the check and per `VACUUM` of any columnar table (the clean-up runs at every `VACUUM`).
It needs a superuser to install a broken access method, and the damage is log volume and some time, so I rate it low.
A fix must achieve: a handler that failed is called at most once per access method per call (the memo of the loop remembers a failed oid for the rest of the loop, or a short list of them), and the log line is written once per access method per call.

### C13-02 (note) A lock snapshot reused for up to 64 rows can name a holder that is gone, or none for a lock taken after it

Place: `kingres_columnar.c`, `kc_lock_blockers` (the copy in `sink->ld` serves 64 rows). Read, not run. A row of a table that was locked after the copy was made can end with an empty "held by" text; a holder that let go after the copy is still named.
This is the "moment's view" the comment states, now up to 64 rows old instead of one row old; the row itself (`not_checked`) is never wrong. I would not change the code; a sentence in the README ("the process text is a view that can be as old as the first of its 64 rows") is enough.

## Verdict

Yes, the review track of this extension can be closed. C12-01 to C12-05 are fixed, each with a test that fails for a one-line mutant (nine of nine killed, among them the two that survived the twelfth review); the extension builds with `-Werror`, and the whole
suite (42 files, 10265 and 10263 tests) and the 11 `pg_regress` tests pass on PostgreSQL 19rc2 and 18.7. The new subtransaction around a handler call is built the way the server's own code builds it, re-raises what must not be swallowed, does not leak,
is not begun where it may not be, and my probes found no path that loses a row or loses the stand-down. What remains is the log volume of a broken handler (C13-01, low) and a README sentence (C13-02, note); neither is a reason to keep the track open.

## What was not checked

- The mutants and the probes ran on PostgreSQL 19rc2 only; the suite and `pg_regress` ran on both.
- The 64-row quota against a lock table that changes during the call (C13-02) is read, not run; I did not repeat the 1200-table memory measurement of the twelfth review (`t/042` with 800 tables stands for it).
- A handler error raised while a statement cancel is pending (the re-raise of class 57) is read, not run.
- The behaviour of `columnar_is_columnar_am` for an access method of a transaction that has not committed is read, not run (it returns false, as an unknown class does everywhere else).
- Physical replication, logical decoding, crash recovery and the on-disk format were not looked at: the round does not touch them; their tests are part of the green suite.
- Nothing outside the 14 files of the diff and what they call.
