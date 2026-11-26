<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
# Adversarial review of the seventh fix round: contrib/kingres_columnar 0.1

Reviewer: an independent reviewer (not the author). The change under review
answers `kingres/docs/reviews/2026-11-17-kingres-columnar-0.1-sixth-fixes.md`
(C7-01 to C7-07). The six earlier reviews are
`kingres/docs/reviews/2026-11-15-kingres-columnar-0.1.md`,
`kingres/docs/reviews/2026-11-15-kingres-columnar-0.1-fixes.md`,
`kingres/docs/reviews/2026-11-16-kingres-columnar-0.1-second-fixes.md`,
`kingres/docs/reviews/2026-11-16-kingres-columnar-0.1-third-fixes.md`,
`kingres/docs/reviews/2026-11-16-kingres-columnar-0.1-fourth-fixes.md` and
`kingres/docs/reviews/2026-11-17-kingres-columnar-0.1-fifth-fixes.md`; the suites
that cover their findings were run in full (baseline below).

Method. The reviewed state (the fix round and the directory around it) was
exported with `git archive` (paths `contrib/kingres_columnar`, `kingres/dev`,
`kingres/docs`) into a private work area; the working tree was not used for any
result and nothing in the repository was changed except this file. PostgreSQL
installations with assertions: 18.7 and 19rc2 (copies of the two given
installations, ports 59900 to 59999). Installations without assertions: built
from the `kingres-18` and `kingres-19` branches with `-Dcassert=false`, the
extension built against each. The test module `t/kcevents` was used as the
author uses it (and extended by me with a scan and a non-transactional
truncate); the shell and Perl harnesses I wrote are not part of the repository.
Line numbers, where given, are those of the export.

An infrastructure event happened during the review: the Windows system drive
filled up, the WSL disk went read-only and WSL was restarted. Everything that ran
between about 14:15 and 14:40 was discarded; every result below is from a run
made afterwards (the event tables, the orphan scenarios, the baseline of 19rc2),
except the baseline of 18.7 (14:08 to 14:16, before the event, all passed; it
was not repeated).

## Summary

The central decision of the round is sound as an engineering answer and is
mostly what the README says: where PostgreSQL forbids an error the access method
refuses with a WARNING and keeps the server up. I checked it event by event, heap
against columnar, on a build with and a build without assertions, on both majors
(table below): the table in the README is right for every row except one claim
about heap (a row written by heap at `XACT_EVENT_COMMIT` is visible but not
durable, C8-03). Heap itself is not a better reference there: with assertions it
stops the server at nine of the thirteen moments; in a production build it
PANICs at the three phases of the release of a resource owner, and its row at
`XACT_EVENT_COMMIT` can vanish at the next crash.

What does not hold:

* One entry point of the access method that can be reached at those moments is
  not guarded: `relation_nontransactional_truncate`. A module that calls it at
  `XACT_EVENT_COMMIT` gets `ERROR: attempted to delete invisible tuple` in a
  commit callback and then `PANIC: cannot abort transaction ... it was already
  committed`; at `XACT_EVENT_PREPARE` the ERROR; at `SUBXACT_EVENT_COMMIT_SUB`
  (18.7, no assertions) a backend that sleeps in a loop for more than seven
  minutes; and on 19rc2 at `XACT_EVENT_ABORT`, `SUBXACT_EVENT_COMMIT_SUB` and
  `SUBXACT_EVENT_ABORT_SUB` the file is truncated while the stripe rows stay, so
  that every read of the table fails with "columnar table is corrupt" (C8-01).
* For the two events where the transaction can still end cleanly and an error
  can still be given (`SUBXACT_EVENT_COMMIT_SUB`, and a critical section inside a
  live transaction), the row is lost with a WARNING and the transaction commits;
  the answer "an error" is still available at the next pre-commit and is not used
  (C8-02).
* The tests of the round skip themselves, with a pass for the file, where the
  test module cannot be built (C8-04); `columnar_check(true)` waits without end
  behind a prepared transaction that holds a lock on a columnar table (C8-05).

The things I attacked hardest hold: the removal of orphaned stripe rows never
removed the rows of a file that exists or will exist (table below), the memory
bound holds in every worst shape I could build, the random comparison reaches
every path it names in 30 of 30 seeds on 19rc2 (minimum per path in the section
on `t/026`), and of 17 mutants I built 14 are killed by the tests of the round
(the three survivors are listed). One more thing does not hold: the random
comparison is itself flaky. A seed that fails with no defect in the code exists
(C8-07), a test file that the author's own rule 5 of `testing.md` forbids.

| Severity | Count |
|---|---|
| critical (wrong answer, lost or doubled committed rows, crash, a SERIALIZABLE anomaly heap prevents, an unreadable table) | 0 |
| high | 1 |
| medium | 2 |
| low | 3 |
| note | 1 |

C8-01 is a server stop, a hang and an unreadable table, which by the letter of
the definition is critical; as for C6-01, C6-02 and C7-01 it needs code of
another module that calls the access method from a callback or a critical
section, and nothing in the KingresDB tree does, so I count it as high and say so
for the owner to decide otherwise.

| Id | Severity | Component | Statement |
|---|---|---|---|
| C8-01 | high | `kc_relation_nontransactional_truncate` (`kc_am.c`) | Not guarded: PANIC at COMMIT, ERROR at PREPARE, a hang at COMMIT_SUB (18.7), an unreadable table after ABORT, COMMIT_SUB and ABORT_SUB (19rc2). |
| C8-02 | medium | `kc_write_refused` | At COMMIT_SUB and in a critical section of a live transaction the row is lost with a WARNING and the transaction commits; an ERROR at the next pre-commit would still be possible. |
| C8-03 | low | README "Writes while a transaction is ending" | "heap, production build: writes; the row is committed" at `XACT_EVENT_COMMIT`: the row is visible but not durable (lost at an immediate stop two seconds later). |
| C8-04 | low | `t/KcProbe.pm`, `t/026` to `t/028` | A module that cannot be built makes the file skip: `Result: NOTESTS`, no failure; a compile error shows only as a diagnostic. |
| C8-05 | low | `columnar_check(true)` | Waits without end behind a prepared transaction that holds a lock on any columnar table (ShareUpdateExclusiveLock on every table). |
| C8-07 | medium | `t/026_random_cursors.pl` | Fails on some seeds with no defect: seed 2088425692 on 19rc2 gives `prepared_skip` 0 and "path prepared_skip was taken by the random episodes (0 times)"; the replay is exact. |
| C8-06 | note | `kc_orphans_keep` | Allocates in `TopMemoryContext` at `XACT_EVENT_COMMIT`, the moment the round says allocation must not happen (an out of memory error there is a PANIC); not reproduced. |

## Fate of C7-01 to C7-07

| Id | Fate | Evidence |
|---|---|---|
| C7-01 | Fixed for insert, multi insert, speculative insert, delete, update, lock and the bulk-insert finish; the entry point `nontransactional_truncate` is not guarded (C8-01) | Event tables below: the module's `table_tuple_insert` at every event of the transaction and subtransaction callbacks, the three release phases and a critical section, heap against columnar, 18.7 and 19rc2, with and without assertions: no PANIC and no assertion from the access method; the WARNING is printed once and counted as `refused_ending`. Delete, update, lock, bulk, speculative, finish: same, 18.7 and 19rc2 without assertions, and 18.7 and 19rc2 with assertions. |
| C7-02 | Fixed in what it reaches; the file is flaky by its design (C8-07) and one exclusion hides a class of difference (section on `t/026`) | 30 seeds on 19rc2 (all pass) and 30 on 18.7 (one fails, seed 126: `prepared_skip` 0); the seed is printed and replays exactly; minimum per path below. |
| C7-03 | Fixed for the cases named; the new tests kill the mutants of the round's own code; the survivors are listed (M10, M17, M20) | Mutants below. |
| C7-04 | Fixed in the shapes I could build | Five worst shapes, measured, below: none above the stated bound; the largest ratio to heap is 2.4. |
| C7-05 | Fixed as described; one leak by design and one cost (below) | Orphan scenarios below. |
| C7-06, C7-07 | README states them; not changed in code | Read; `t/030` passes on both majors. |

## Writes while a transaction is ending

### Table of events

Method: the module `t/kcevents` holds a table open and writes one row with
`table_tuple_insert` at the event named by `kcevents.at`; the transaction itself
wrote 100 (and 101 in a savepoint). One server per case; after the case the rows
are read, then the server is stopped with `pg_ctl -m immediate`, started and the
rows read again ("after a crash"). Heap and columnar: the same script on a heap
table `h` and a columnar table `t`. "kept" means the row is in the table
afterwards. Both orders of loading the two libraries were run on 18.7 and 19rc2 without
assertions and on 19rc2 with assertions; 18.7 with assertions only in the order
where the module's callbacks run after the access method's. The results were the
same in every order and on both majors, so one column each is shown. The "before
the round" column is taken from the sixth review and the README (I did not rebuild
the previous commit).

| Moment | heap, assertions | heap, no assertions | columnar before the round | columnar now (all four builds) |
|---|---|---|---|---|
| `XACT_EVENT_PRE_COMMIT`, `PRE_PREPARE` | kept, durable | kept, durable | kept (write-through) | the same |
| `SUBXACT_EVENT_START_SUB`, `PRE_COMMIT_SUB` | kept, durable | kept, durable | kept | the same |
| `XACT_EVENT_COMMIT` | server stops (assertion) | kept; visible at once with the committing transaction's id as `xmin`; **lost** at an immediate stop even two seconds later (its WAL record is not covered by any flush) | PANIC | WARNING, row not kept, commit stands |
| `XACT_EVENT_PREPARE` | server stops | kept; `COMMIT PREPARED` commits it, durable | assertion in `procarray.c` | WARNING, row not kept, prepared transaction intact |
| `XACT_EVENT_ABORT` | server stops | written, invisible (aborted) | PANIC | WARNING |
| `SUBXACT_EVENT_COMMIT_SUB` | server stops | kept, commits with the parent | the subtransaction aborted | WARNING, row not kept, the subtransaction's other rows commit |
| `SUBXACT_EVENT_ABORT_SUB` | server stops | written, invisible | PANIC | WARNING |
| `RESOURCE_RELEASE_BEFORE_LOCKS`, `LOCKS`, `AFTER_LOCKS` | `ERROR: ResourceOwnerEnlarge called after release started`, then PANIC | the same PANIC | PANIC | WARNING |
| critical section | server stops (allocation assertion) | kept | kept if no allocation was needed | WARNING, row not kept |

The README's table is right on every row but one (C8-03: its cell for heap at
`XACT_EVENT_COMMIT`, "the row is committed"). Heap with assertions stops the
server at the commit, prepare, abort, both sub-abort and sub-commit events and in
the critical section; the README says the same.

### Other table access method calls at the same moments

Run with `kcevents.op` on 18.7 and 19rc2 without assertions, at commit, prepare,
abort, commit_sub, abort_sub, a release phase and a critical section:

| Call | Result |
|---|---|
| `table_multi_insert`, `table_tuple_insert_speculative`, `table_tuple_delete`, `table_tuple_update`, `table_tuple_lock` | WARNING naming the table, `TM_Invisible` returned where there is a result; no error, no PANIC; rows of the table afterwards exactly those written before |
| `table_finish_bulk_insert` | nothing happens, no warning (the flush does nothing there) |
| table scan (`table_beginscan`, `table_scan_getnextslot`) | works at commit, prepare, abort, both sub events and in a critical section, 5 rows read (checked in the server log on 18.7); at a release phase: `ResourceOwnerEnlarge` PANIC, the same as heap |
| `table_relation_nontransactional_truncate` | **C8-01** |

With assertions both heap and columnar trap in a scan at those moments (the
`IsTransactionState` assertion of the relation cache); that is PostgreSQL's
rule, not the access method's. With assertions the `bulk` operation inside a
critical section also stops the server, in the module's own allocation of its
slots, before the access method is reached; every other operation and event on
the two builds with assertions ended without a trap from the access method.

### What `TM_Invisible` does to a caller

`ExecDelete`, `ExecUpdate` (`nodeModifyTable.c`), `ExecLockRows`
(`nodeLockRows.c`), `ExecOnConflictUpdate` and the trigger code map `TM_Invisible`
to `elog(ERROR, "attempted to ... invisible tuple")` or "unrecognized status";
none of them loops on it, so there is no endless retry. The executor cannot run
at any moment where the refusal answers (the states are not "in progress", or a
critical section), so no executor path reaches it; a module that calls the
access method directly gets the status and the warning. I could not make
`kc_write_refused` return true while an executor ran.

### The warning itself

`ereport(WARNING)` allocates only in `ErrorContext`, which PostgreSQL allows
in a critical section, and runs the error context callbacks; the stack depth was
never an issue (one frame). I ran every event above with the default settings;
I did not run it with `emit_log_hook` set, with a broken client connection or
with `client_min_messages` and `log_min_messages` changed (Not covered). A hook
that raises an error in a critical section would turn the WARNING into a PANIC,
but it does so for every WARNING of the server.

### Judgement

The refusal is the least bad behaviour for `XACT_EVENT_COMMIT`,
`XACT_EVENT_PREPARE`, the three release phases, `XACT_EVENT_ABORT` and
`SUBXACT_EVENT_ABORT_SUB`, and the README says what it is. After the commit
record an ERROR is a PANIC; the access method cannot write a stripe without
catalog access, WAL and allocation, each of which can fail; catching the error
inside the callback is not an option (the buffer pins and lightweight locks of a
failed catalog insert cannot be undone without a subtransaction, which cannot be
started there). For an aborting transaction the row is lost anyway. Heap is not
the reference of a safe behaviour there (it stops a server with assertions and
its row at the commit event is not durable).

Two points are weaker than the README says. First, "columnar loses a row that
heap keeps" is less than half of the comparison: heap keeps a visible row that
the next crash may take back (C8-03). Second, `SUBXACT_EVENT_COMMIT_SUB` and the
critical section are moments where the enclosing transaction is still alive and
can still abort cleanly; a WARNING that discards the row there and lets the
transaction commit is the wrong end of "where the right answer cannot be given, an
error is" (C8-02). The pre-commit of a subtransaction is served, as the round
says; the commit of a subtransaction is not unservable, only unserved: the rows
are handed to the parent in phase two, after which a late row could be appended
to the parent's buffer (its subtransaction id is the parent's), or the refusal
can be remembered and raised at the parent's next pre-commit.

### Entry points of the access method at such a moment

| Entry point | Guarded | What happens unguarded |
|---|---|---|
| `tuple_insert`, `multi_insert` | yes (twice: in `kc_am.c` and in `kc_write_row_values` / `kc_write_rows`; the second guard is redundant for these callers, see mutants M16 and M20) | |
| `tuple_insert_speculative`, `tuple_complete_speculative` | yes | |
| `tuple_delete`, `tuple_update`, `tuple_lock` | yes, `TM_Invisible` | |
| `finish_bulk_insert` | yes, in `kc_write_flush_relation` | |
| `relation_nontransactional_truncate` | **no** | catalog delete in a committed or committing transaction: ERROR then PANIC, a hang, a half done truncate (C8-01) |
| `relation_set_new_filelocator`, `relation_copy_data`, `relation_copy_for_cluster` | no (the last warns per row through `kc_write_row_values`, then flushes nothing) | read from the code: catalog and file work, errors possible; not run (Not covered) |
| index build and validate callbacks | no | raise "no indexes" errors; not run |
| scan, analyze, sample and estimate callbacks | no | reads: work where heap works (table above) |

## The tests of this round

Method. Mutants: copies of the extension with one change, built into a private
copy of the 19rc2 installation (assertions on), run with seven test files
(`t/022`, `t/023`, `t/025`, `t/026`, `t/027`, `t/028`, `t/030`). "Killed" means a
file fails or does not finish. The author's list of mutants is not in the
repository, so these are my own: seventeen (the numbers skip M13, M15, M16 and
M18, which I did not build), all built and run on 19rc2 with assertions; the
author's 18.7-only runs are not repeated.

| Mutant | Change | Result |
|---|---|---|
| M1 | no refusal in a critical section | killed, `t/027` (critical section, both orders) |
| M2 | no refusal by the transaction state | killed, `t/027` (commit, prepare, abort, both sub events) |
| M3, M4, M5, M6 | no guard in delete, update, lock, speculative insert | killed, `t/027` (the file died) |
| M7 | no guard in `kc_write_flush_relation` | killed, `t/027` |
| M8 | `kc_orphans_keep` keeps nothing | killed, `t/028` |
| M9 | `kc_orphans_adopt` adopts nothing | killed, `t/028` |
| M10 | the cap of 10 000 kept files removed | **survives** the cap (no test has more than a few files); `t/026` failed in that run for the reason in C8-07, not because of the change |
| M11 | the dirty snapshot of `pg_class` replaced by a current one | killed: `t/023`, `t/027`, `t/028`, `t/030` |
| M12 | the path counters do not count | killed: `t/022`, `t/025`, `t/026` |
| M14 | ceiling factor 4 in place of 2 | killed: `t/025`, `t/026` |
| M17 | `kc_orphans_done` removed (the kept list is never freed after adoption) | **survives** all seven files: every later transaction of the backend adopts the same notes again, at the cost of a table open per note; no test measures it |
| M19 | the refusal is not counted | killed, `t/027` |
| M20 | the guard in `kc_multi_insert` removed | survives: equivalent, `kc_write_rows` guards again (so the guard in `kc_am.c` for insert and multi insert is redundant) |
| M21 | `kingres_columnar_note_rewrite` opens without a lock | killed by `t/023` on this build (assertions); a build without assertions would not notice |

### `t/027_ending_writes.pl`

Preconditions: established. It builds the module, starts a server per order of
loading, checks the server start time and the log for `PANIC|TRAP|terminated`,
the rows, the warning text and the counter, and compares with a heap twin only
where heap does not stop the server. It tests insert only; delete, update, lock,
bulk, speculative and finish (guards M3 to M6) are in `t/027` according to its
header, and the mutants say whether they are killed (table above). It does not
call `nontransactional_truncate` (C8-01). Build failure: C8-04.

### `t/028_orphan_rows.pl`

Preconditions: established for the late drop (event triggers off, a probe module
that drops at pre-commit), a session without the library, and a table being
created in another session. It does not run VACUUM against a rewrite, a
truncate, a tablespace move or a prepared transaction; I did (below).

### `t/025_memory_bound.pl`, `t/022`, `t/023`

Pass on both majors; the mutants M14 (ceiling factor) and M21 (note without a
lock) say what they kill. The note-without-lock case needs a build with
assertions: `t/023` creates `columnar_note_rewrite` in the file and calls it, so
it is covered there only on such a build.

### `t/026_random_cursors.pl`

Is it random over the space it names? Yes in the choice of profile (40 percent
"normal", 48 percent "pressure", 12 percent "ceiling"), of widths, counts, nesting
(up to six savepoints), cursors (up to three) and of the probe statements
(a write to another table, a write to the same table, a `COPY` into a table made
in the transaction); each run draws its seed from the clock and the process id
and prints it. The space is narrower than its name: two integer and text columns,
no NULL, no update or delete (append-only), the default codec, row contents that
are a repeated character (only the lengths and the ids are compared, never the
bytes of `pad`), 90 episodes.

Replay: yes. Seed 2088425692 on 19rc2 was replayed on the unmodified extension and
gave the same counters (`ceiling_write=14 handover_flush=14 late_commit=195
late_window=465 own_flush=21 prepared_skip=0 pressure_flush=92 size_flush=32`) and
the same failure.

Does it reach each path every run, or on average? Thirty seeds on each major, the
count of each path summed over the 90 episodes of a run (minimum, maximum; runs
with a count of zero):

| Path | 19rc2 | 18.7 |
|---|---|---|
| `late_commit` | 73 to 272 (0) | 48 to 293 (0) |
| `late_window` | 235 to 784 (0) | 151 to 1041 (0) |
| `pressure_flush` | 81 to 242 (0) | 63 to 164 (0) |
| `own_flush` | 16 to 45 (0) | 13 to 48 (0) |
| `handover_flush` | 10 to 27 (0) | 11 to 31 (0) |
| `size_flush` | 17 to 57 (0) | 18 to 55 (0) |
| `prepared_skip` | **2** to 28 (0) | **0** to 32 (**1**) |
| `ceiling_write` | 4 to 16 (0) | 3 to 17 (0) |

Every path but one is reached with a margin of at least ten in every run.
`prepared_skip` is not: it is the path the guards of prepared buffers take, it
has a minimum of 2 on 19rc2 and of 0 on 18.7 (seed 126), and the unmodified
extension with seed 2088425692 on 19rc2 also gave 0. Two zeros in 62 runs, so the
file fails by itself on roughly one run in thirty (C8-07). The author's ranges
over ten seeds did not show it. `ceiling_handover` is taken only by the scripted
case (0 or 1 or 2 in the random part), as the file says.

What did the author exclude, and does it hide a difference? Unordered cursors are
compared as sets, and only when the cursor was read to its end and every fetch is
still logged (a `ROLLBACK TO` an older savepoint takes the log of a fetch away).
What that cannot see: the order of the rows a cursor returns (heap and columnar
differ legitimately), the prefix a partial fetch returned (only the union over the
whole life of a drained cursor is compared, so a partial fetch that returned
a wrong subset and a later fetch that returned the rest compensating for it would
not show), and a cursor that is never drained. A row returned twice or never by a
drained cursor does show (count, sum and md5 of the sorted ids). The statements
that fail are excluded when the error is the ceiling, at most 40 percent of the
episodes. I found no real difference hidden by these exclusions; I could not
construct one either.

### Can a test module that is built at test time fail silently?

Yes. `KcProbe::build_module` returns `undef` when `pg_config` cannot be run, the
headers are missing, or the compiler fails; the file then does `plan skip_all`.
With `PG_CONFIG=/bin/false`:

```
t/027_ending_writes.pl ... skipped: the test module (t/kcevents/kcevents.c) could not be built here ...
t/026_random_cursors.pl .. skipped: the probe module (t/kcprobe/kcprobe.c) could not be built here ...
t/028_orphan_rows.pl ..... skipped: the probe module (t/kcprobe/kcprobe.c) could not be built here ...
Result: NOTESTS
```

No failure. A compile error (a header of another major) is a `diag` line and the
same skip. `t/029` and `t/030` use the same helper and skip the same way; `t/022` and
`t/024` build their own probe in the file. Together these are the files that test
this round's central claims, and a machine without `cc` runs the other files and
reports success (C8-04). The only sign is a drop in the count of tests.

## Findings

### C8-01 (high) An unguarded entry point: `relation_nontransactional_truncate`

Place: `kc_am.c`, `kc_relation_nontransactional_truncate`: `kc_write_discard_relation`,
`kc_catalog_delete_uid`, `RelationTruncate`, `kc_meta_reset`, with no
`kc_write_refused`.

Steps (test module `kcevents` with one more operation that calls
`table_relation_nontransactional_truncate(rel)` on the held relation at the chosen
event; `t` a columnar table with 5 rows in 1 stripe, `h` a heap table with the same
rows; the transaction inserts 100):

```
SET kcevents.at = '<event>'; SET kcevents.op = 'ntrunc';
BEGIN; SELECT kcevents_open('t'); INSERT INTO t VALUES (100); COMMIT;
```

Observed, 18.7 without assertions (heap: the table is emptied at every event, no
error):

| Event | columnar |
|---|---|
| `XACT_EVENT_COMMIT` | `WARNING: AbortTransaction while in COMMIT state`, `ERROR: attempted to delete invisible tuple`, `PANIC: cannot abort transaction N, it was already committed`: every connection restarts |
| `XACT_EVENT_PREPARE` | `ERROR: attempted to delete invisible tuple` in the prepare callback |
| `SUBXACT_EVENT_COMMIT_SUB` | the backend does not return; after seven minutes still active, no wait event, sleeping in `hrtimer_nanosleep`, 13 s of CPU; the same script ran in 0 s on the heap table; reproduced by timeout in 40 s |

On 19rc2 without assertions: at `XACT_EVENT_COMMIT` the same PANIC; at
`XACT_EVENT_PREPARE` the same ERROR; at `XACT_EVENT_ABORT`: afterwards `SELECT
count(*) FROM t` gives `ERROR: columnar table "t" is corrupt: stripe 1: the stripe
lies beyond the end of the file`, and the same after a restart; at
`SUBXACT_EVENT_COMMIT_SUB` and `SUBXACT_EVENT_ABORT_SUB` `ERROR: columnar table
"t" is corrupt: stripe 1: the stripe header disagrees with the catalog`; inside a
critical section the table reads as empty. With assertions (18.7 and 19rc2) the
server stops at every event, as it does for the scan: the assertion of the
relation cache.

Expected: the same answer as for the other entry points: a warning, nothing done,
and above all not a half done truncate: the file is shortened and the stripe rows
stay (or the other way round), a state the extension itself refuses to read, which
the rule of the review forbids ("a table is never brought into a state that the
extension refuses to read") whoever calls it. The call is made by PostgreSQL
itself only for temporary tables, which a columnar table cannot be, so as for
C7-01 it needs a module.

Direction: `if (kc_write_refused(rel, "truncate")) return;` first in the function,
and decide the order of the three steps (catalog rows, file, metapage) so that a
failure between them leaves a readable table; add the operation to `t/027`.

### C8-02 (medium) The row is lost where an error can still be given

Place: `kc_write_refused`, `kc_write_state_forbids_error`.

At `SUBXACT_EVENT_COMMIT_SUB` the enclosing transaction is in progress, and in a
critical section the transaction that entered it is in progress: the commit record
of the top-level transaction does not exist yet. The round refuses the write there
as it does after the commit record, with a WARNING and no error, and the
transaction commits without the row (`rows=100,101` for a module row written at
`commit_sub`, with the WARNING; heap keeps it). An error at the parent's pre-commit
is allowed (the access method raises errors there today) and would make the loss
loud for the one caller who can still react: the transaction aborts instead of
committing without data. Where the transaction is aborting or committed, nothing
better exists, and the WARNING is right.

Direction: `kc_write_refused` sets a flag (a count and the table name) when the
state is a critical section or `SUBXACT_EVENT_COMMIT_SUB` of a transaction that is
still in progress; `kc_xact_callback` at `XACT_EVENT_PRE_COMMIT` and
`PRE_PREPARE` raises an ERROR when the flag is set and clears it at abort. Or
serve the commit of a subtransaction by appending to the parent's buffer. The
README sentence "the row is lost with a warning, which is the least bad of what is
left" is then true for the other events only.

### C8-03 (low) The README's cell for heap at `XACT_EVENT_COMMIT` overstates what heap does

README table, column "heap, production build", row `XACT_EVENT_COMMIT`: "writes;
the row is committed". Measured (18.7 and 19rc2 without assertions, order
"after", and order "before"): the row is in the table, visible to a new session
with the committing transaction's id as `xmin`, and after `pg_ctl -m immediate
stop` and a start, two seconds after the commit (default `wal_writer_delay`), it
is gone while the other row of the same transaction is there. Its WAL record is
written after the commit record's flush and nothing flushes it until the next
commit of any session. So "heap keeps the row" is true until the next crash;
the sentence "columnar here loses a row that heap keeps, by design" should say
"a row that heap shows and may lose". At `XACT_EVENT_PREPARE` and
`SUBXACT_EVENT_COMMIT_SUB` the row is durable (the later commit flushes it).

### C8-04 (low) The tests of the central claims skip silently

See "Can a test module that is built at test time fail silently?". Direction:
make the skip a failure when an environment variable (set by the integrator's
scripts) says the compiler must exist, or at least print the file's name in the
summary of `ext.sh`; build the modules once in the directory of the run and
fail on a build error on a machine where `cc` runs.

### C8-05 (low) `columnar_check(true)` waits behind a prepared transaction

`kingres.columnar_check(true)` takes a `ShareUpdateExclusiveLock` on every columnar
table (`kingres_columnar.c`, the `fix` branch). A prepared transaction that holds
an `AccessExclusiveLock` on one table (a prepared `TRUNCATE` or rewrite, the
shape of the orphan scenarios) makes it wait: the query was active, waiting on the
relation lock, after 9 minutes 54 seconds, until I cancelled it; the README
recommends it for the removal of leftovers. `VACUUM` of another table and
`columnar_reconcile()` do not wait (they were run in the same state, below).

Direction: `try` the lock and skip the table with a note in the output; or say in
the README that the call waits for prepared transactions.

### C8-06 (note) Allocation at `XACT_EVENT_COMMIT`

`kc_orphans_keep` (called at `XACT_EVENT_COMMIT`) allocates in `TopMemoryContext`
(`palloc` of 24 bytes per kept file, a list cell). An out of memory error there is
a PANIC, which the round forbids for the write path. Only when a late drop
happened; I could not make the allocation fail. Direction: keep the notes in
memory allocated before the commit (at pre-commit), and only move pointers at the
commit event.

### C8-07 (medium) `t/026` fails on some seeds with no defect

Steps: `KC_FUZZ_SEED=2088425692 PROVE_TESTS=t/026_random_cursors.pl` (19rc2,
unmodified extension) or `KC_FUZZ_SEED=126` (18.7).

Observed: `# Failed test 'path prepared_skip was taken by the random episodes (0
times)'`, `Result: FAIL`; the counters of the run are above. Every other check of
the file passed: no difference from the heap twin.

Expected: a test that fails only when something is wrong (rule 5 of
`kingres/docs/dev/testing.md`: "a test that is flaky is a bug"). The file draws a
new seed each run and requires that each path was taken at least once; the guard
path `prepared_skip` is taken 2 to 32 times per run, so about one run in thirty
has none. The failure prints a seed that replays it, which is what makes it
diagnosable, but the integrator's gate (the suite must pass) will fail at random.

Direction: make the path certain (a scripted case, which `t/022` already is, counts
it) and keep the random part for the comparison with the heap twin; or draw the
episodes until each required path is taken, up to a bound; or require a path only
when its expectation is above, say, 20 for the number of episodes.

## Orphaned stripe rows (C7-05)

Method: the scenarios of `t/028` plus those below, run on 18.7 (assertions). A
second session holds a transaction open for 7 seconds; in the middle the first
session runs `VACUUM`, `columnar_reconcile()` and `columnar_check(true)` twice,
then the second session ends with ROLLBACK or COMMIT; counts are read in the
second session before it ends, afterwards, and again after the cleaners.

| Scenario of the other session | rollback | commit |
|---|---|---|
| `TRUNCATE` and `COPY` 3000 rows into a table that had 5000 | 5000, kept | 3000 |
| rewriting `ALTER TABLE ... TYPE bigint` | 5000 | 5000 |
| `ALTER TABLE ... SET ACCESS METHOD columnar` of a heap table | 4000 | 4000 |
| `SET TABLESPACE` (and back) | 5000 | 5000 |
| `CREATE TABLE` and `COPY` 3000 rows (uncommitted new file) | not run | 3000 |
| a prepared transaction (truncate and copy, a rewrite, a new table with 2500 rows) with an immediate stop and a restart before the cleaners, then `COMMIT PREPARED` | not run | 3000, 5000, 2500 |
| a prepared transaction (a new table, truncate and copy) rolled back after a restart | 5000 | not run |

Every count was the expected one, in every state, and `columnar_check` reported no
orphan afterwards. Not run: a standby, relfilenode reuse (the identity of a new file
is 63 random bits not in the catalog, so reuse of a file number cannot match a
row).

The late drops (a probe module drops at pre-commit, event triggers off): 10 050
tables with one row each dropped in one statement. After the commit 50 stripe rows
were left, `columnar_check` reported them as one `orphan_catalog_rows` row with the
count 50, `orphans_cleaned` counted 10 000, and `columnar_check(true)` removed the
rest. The 10 001st file and every one after it are left to `VACUUM`,
`columnar_check(true)` and `columnar_reconcile()`, as the README says; a backend
that exits before its next transaction leaves them to the same; after a crash they
are the same orphans (the list is memory). A read only transaction after a late
drop did not write WAL here because the next ordinary transaction had already
consumed the list; I did not make the adoption happen in a read only transaction.

Reading `kc_reconcile_ex`: the live files are read from a dirty snapshot of
`pg_class` after the catalog snapshot is taken, so a file created, rewritten,
truncated or moved by an uncommitted or prepared transaction is live (both versions
of an updated `pg_class` row are seen); a file whose first block is not yet on disk
makes the whole cleanup skip, not delete (`unknown`); a file that does not exist yet
is not a file of any stripe row. I found no way to make the cleanup remove the rows
of a file that exists or will exist.

## The memory bound (C7-04)

`write_buffer_memory` = 1 MB (A), 18.7 without assertions, source values in a heap
table so that no expression adds noise, growth of `VmPeak` in the session against
the same insert into a heap table, and the README's bound (3 (2A + R) + S + 12 MB,
S = 2 (32 MB + V) + 2A + V; V the largest value, R the row):

| Shape | columnar | heap | bound |
|---|---|---|---|
| one value of 64 MB, 3 rows | +259 MB | +129 MB | 468 MB |
| 8 nested savepoints, each a 30 MB row | +81 MB | +33 MB | far above |
| 12 columns of 3 MB (R = 36 MB), 24 rows | +112 MB | +41 MB | 201 MB |
| 4 columns of 40 MB (R = 160 MB), 4 rows | +483 MB | +201 MB | 684 MB |
| a small parent row, three nested 30 MB rows released inward | +65 MB | +33 MB | far above |

The largest ratio to heap is 2.4 and every shape is under the bound; the 64 MB
shape is 2 V more than heap, as the README says (at most 4 V). Compression that
expands (the values were random bytes) is part of these shapes. I did not build a
shape with more than 12 wide columns, nor 1 GB values.

## What held under attack

* The round's own cases: all of `t/022`, `t/023`, `t/025`, `t/026`, `t/027`,
  `t/028`, `t/029`, `t/030` pass on both majors (baseline).
* Heap against columnar at every event: no wrong answer, no row of the
  transaction lost except the module's own row, no assertion and no PANIC from the
  access method for insert, multi insert, speculative insert, delete, update, lock,
  finish, scan (C8-01 excepted), on 18.7 and 19rc2, with and without assertions,
  in both orders of loading.
* Prepared transactions across an immediate stop with the cleaners running before
  and after the restart: nothing lost, nothing doubled.
* The late-drop cap behaves as the README says.

## Not reproduced

* A lost or doubled committed row from any scenario above: none.
* The allocation failure at `XACT_EVENT_COMMIT` (C8-06): not made to fail.
* Adoption of the orphan list in a read only transaction writing WAL: not seen.
* A hang or a PANIC from the guarded entry points: none.
* `columnar_write_paths`: it reports the counters and `bytes_held` of the
  calling backend only (static memory of the backend), so it shows a grantee
  nothing of other sessions; `reset` resets the caller's counters only; no race
  is possible between sessions because nothing is shared. Declared `SECURITY
  INVOKER`, `REVOKE ALL FROM PUBLIC`, tested by `columnar_security`. I found
  nothing to report.
* `kingres_columnar_note_rewrite`: SQL callable only in a database made with the
  earlier install script (the current script does not create it); the lock is
  now `AccessShareLock`, released at once by `relation_close`; a role granted it
  can make a session wait behind an exclusive lock, nothing more. A note of a file
  the table still has deletes nothing (the comparison by identity).

## Not covered

* A standby for the orphan functions (a file whose first block is only in the
  buffers of a standby makes `columnar_check` skip or under-report; not run),
  promotion with the changes of this round, `wal_consistency_checking = all` on a
  crash and a standby for this round's code (only the baseline TAP files, which
  include `t/001` and `t/002`, ran with it).
* `emit_log_hook`, a broken client connection, `client_min_messages` and
  `log_min_messages` at the refusal; `relation_set_new_filelocator`,
  `relation_copy_data` and `relation_copy_for_cluster` at those moments (read, not
  run); index build callbacks.
* The out of memory error at the commit event.
* The author's own list of mutants (not in the repository) and a rerun of the
  author's four.
* A build without assertions for `t/022` to `t/030`; the event tables ran without
  assertions, the TAP files with assertions only.
* Hot standby promotion after a late drop; `DROP DATABASE`.

## Baseline numbers

Unmodified extension, out of tree, `EXT_WERROR=1`, no compiler warning, TAP ports
restricted to 59900 to 59999:

| Major | Regression | TAP |
|---|---|---|
| 18.7 (assertions) | 11 of 11 pass | 30 files, 4 148 tests, pass; `t/013` skipped (module not installed); 504 s |
| 19rc2 (assertions) | 11 of 11 pass | 30 files, 4 150 tests, pass; `t/013` skipped; 509 s |
