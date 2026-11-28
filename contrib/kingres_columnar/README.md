<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
<!-- Portions Copyright (c) 2026, RooyeKhat Media Co -->
# kingres_columnar

`kingres_columnar` adds a table access method, `columnar`, to KingresDB. A
columnar table stores each column on its own, compressed, in stripes of rows. A
query that reads a few columns of a wide table reads only those columns, and a
query with a filter can skip whole stripes and chunk groups with the stored
minimum and maximum. It is meant for append-only analytic data: time series,
events, logs, and old partitions of such data. The design is in
`kingres/docs/design/analytics-phase12-storage.md`; this file says what version
0.2 (the default version, `kingres_columnar--0.2.sql`) does.

It runs on PostgreSQL 18.7 and 19.0 from one source. It adds no core patch and
bundles no third-party code.

What it provides:

- the table access method `columnar`: append-only, compressed (codecs `none`,
  `pglz`, `lz4`, `zstd`), with chunk-group statistics that let a scan skip data,
  a Columnar Scan with column projection, and a parallel scan;
- B-tree indexes, unique ones included (since 0.2), found by TID through
  row numbers that a write buffer reserves when a row is inserted;
- `VACUUM`, `VACUUM FULL`, reuse of free space, standby reads, base backup,
  logical decoding of inserts and `amcheck` of the indexes;
- the functions `kingres.columnar_*` (options, information, compaction, checks)
  and the settings `kingres_columnar.*` (see "Functions" and "Settings").

What it refuses is listed in "What is refused, and why": `UPDATE`, `DELETE`,
row locks, foreign keys, non-B-tree indexes, temporary and unlogged tables.

Requirements: the library has to be in `shared_preload_libraries` on every node
that writes, replays or restores columnar data (see "Installing"), and the
extension needs a superuser to create it. `kingres_timepart` uses it to convert
old partitions, and the control plane installs it with the database.

Build and test: see "Building and testing" at the end. The extension is built and
tested on PostgreSQL 18 and 19.

Map of this document: "Installing" and "Use" (functions, settings, `EXPLAIN`);
"On-disk format"; "B-tree indexes"; "Reserved row numbers"; "Parallel scan";
"What is refused, and why"; "VACUUM and space"; "Backup, restore, upgrade";
"Converting"; "Isolation, transaction ages, names"; "Limits" (memory and costs,
and the rules for writes while a transaction ends); "Security"; "Building and
testing". The sections that carry increment names (C1 to C14) and review ids
(`C10-01`, ...) are the record of how the extension was built, in the wording of
its time; the current state is what this file says as a whole and what the guide
says.

## Installing

The library has to be preloaded on every node that writes, replays or restores
columnar data, standbys included, because it registers a WAL resource manager:

```
shared_preload_libraries = 'kingres_columnar'
```

then `CREATE EXTENSION kingres_columnar;` (superuser). Objects are in schema
`kingres`. Without the preload `CREATE EXTENSION` works and tables that
already exist can be read, but everything that would write columnar data is
refused with an error that names the setting: `CREATE TABLE ... USING
columnar`, `INSERT`, `COPY`, `TRUNCATE`, a rewrite into columnar, and
`columnar_compact`. A server that has ever written columnar WAL needs the
library for as long as that WAL exists (crash recovery, archive recovery,
base backups, `pg_rewind`). Resource manager id: 255, registered as
`KingresColumnar` on the PostgreSQL wiki list of custom resource managers
(<https://wiki.postgresql.org/wiki/CustomWALResourceManagers>). The id
is part of the WAL format and never changes.

## Use

```sql
CREATE TABLE events (ts timestamptz, device int, value float8) USING columnar;
INSERT INTO events SELECT ...;            -- INSERT, INSERT ... SELECT, COPY, CREATE TABLE AS
ALTER TABLE t SET ACCESS METHOD columnar; -- rewrites the table; "heap" converts back
```

Rows are buffered in the backend and become a stripe when 150000 rows are
buffered, when the buffers exceed `kingres_columnar.write_buffer_memory`, or
at commit (the memory a backend may hold is stated under "Memory and costs").
Many tiny transactions therefore make tiny stripes; load in big transactions
and use `kingres.columnar_compact` afterwards.

### Functions

| Function | Meaning |
|---|---|
| `kingres.columnar_set_options(rel, compression, compression_level, stripe_row_count, chunk_row_count)` | Options for stripes written from now on (a null argument keeps the value). Owner only. Not transactional. |
| `kingres.columnar_options(rel)` | The options in force. |
| `kingres.columnar_info(rel)` | Stripes, chunk groups, rows, bytes on disk, uncompressed bytes, ratio, and `unreferenced_bytes`: what the file holds that no stripe visible now uses (the pages of rolled-back loads, of stripes that compaction replaced, of a load still in progress), read with a snapshot of this moment whatever the caller's snapshot is.  VACUUM gives back such pages at the end of the file only when they are worth it (see "VACUUM and space"); a figure that stays high after a VACUUM is waiting for `VACUUM FULL`. |
| `kingres.columnar_stripes(rel)` | One row per visible stripe. |
| `kingres.columnar_compact(rel, min_rows)` | Merge stripes with fewer than `min_rows` rows (default a quarter of the stripe size); returns the number of stripes written. Owner only. On a table with indexes the copies get entries in every index (see "B-tree indexes"). It changes nothing logically: for every transaction, the one that calls it and every other, the rows each snapshot sees are the same before and after, and under SERIALIZABLE it creates no dependency (see "Isolation"). |
| `kingres.columnar_check(fix)` | Superuser. Reports catalog rows of files nobody uses, stripes beyond the end of the file, stripes whose first page is not theirs, stripes with more rows than the developer setting `max_stripe_rows` lets a stripe have today (`stripe_over_row_limit`: readable, a rewrite writes them again), and relations that cannot be read (`unreadable`, with the reason: damage that the table shows, an unreadable or missing file, a format this server does not read). `fix` deletes the first two kinds. **It reads the stripe catalog as of now whatever the isolation level of the caller** (an older `REPEATABLE READ` or `SERIALIZABLE` snapshot does not see a table made after it, nor stripes committed after it, and would call a damaged table clean): the list of tables and the stripes are read by this call, not through the caller's snapshot. **It never waits for a lock, for every lock it takes**: the stripe catalog, its primary key index, its TOAST table and its index, and `pg_class` are locked without waiting before anything is read, and each table is locked without waiting; whatever cannot be locked (an administrator's `LOCK TABLE`, `REINDEX` or `VACUUM FULL` that is running, a prepared transaction that holds a lock until someone ends it) is reported as `not_checked` with the reason (for a catalog: for no table in particular, and nothing is checked), and the rest is checked.  The row says which processes hold the lock, read from the lock manager's tables without waiting (`It is held by process 1234.`, or `... by a prepared transaction (see pg_prepared_xacts).`: a prepared transaction has no process); for a catalog it also names the relation that could not be locked.  What remains is what every statement does: the relation cache reads the system catalogs such as `pg_attribute`, and a short wait for a buffer.  **One exception to "never waits"**, argued from the code and not tested: when the backend's cache is cold, resolving the stripe catalog by name (the first step of every call) takes ordinary, waiting locks on `pg_extension` and `pg_class`; a session that holds `pg_class` `ACCESS EXCLUSIVE` stalls nearly every statement of a database in the same way, so this is not a property of the check, but the sentence above does not cover that first lookup.  A table dropped, made another kind of table, or made while the call runs is a `not_checked` row as well, never counted as clean.  **A query cancel, a statement timeout, a lock timeout, an out-of-memory error or any other error that is not about the table being read ends the call with that error**; only damage that the table shows becomes an `unreadable` row. **What a caller must look for: any row at all means "not clean".** `not_checked` is not an error row and not a clean result: it says that this table was not looked at, so a monitor that tests for the names of the error rows (`stripe_beyond_file`, `unreadable`, `page_header_mismatch`, `orphan_catalog_rows`) would pass over a damaged table that happens to be locked.  A monitor tests that the result is empty, or uses `columnar_check_clean()`.  **What "clean" covers**: the stripe catalog against the files, and the first page of each stripe (its place in the file, its header, its identity and size); orphaned catalog rows; **not the other pages of a stripe** (a damaged later page is found when the stripe is read, by the checksums); **and not a stripe that only an old snapshot can still see** (one that compaction replaced and that is not yet invisible to every snapshot): the stripes looked at are those visible now.  **Which tables**: a table is a columnar table, here and everywhere else (the planner's path, the list of live files of the clean-up of the stripe catalog, the drop note, the event trigger that refuses foreign keys and publications), when the handler of its access method is this extension's handler, never by the name of the access method: a second access method made over `kingres.columnar_handler` by a superuser (`CREATE ACCESS METHOD columnar2 TYPE TABLE HANDLER kingres.columnar_handler`) is checked, cleaned up after and refused foreign keys like `columnar` (before this round the check did not list such a table and the clean-up deleted every stripe row of it).  The clean-up of the stripe catalog deletes nothing while a table-like relation has an access method that cannot be classified (an access method that a transaction has made and not committed, or a handler that raises an error), or while a columnar file has an unreadable metapage; **that is a visible outcome of the check (C12-01)**: the orphan step could not be done, so the call returns a `not_checked` row with no table, whose detail says `the orphan step could not be done: ` and the reason, `columnar_check_clean()` is `false`, and no `orphan_catalog_rows` row is made up; the answer is the same for `fix`, which deletes nothing then.  The handler of an access method that is not this extension's is code of somebody else: the check, VACUUM and `columnar_reconcile()` at the top level of a transaction call it inside a subtransaction (only when the answer is not already cached; `columnar_reconcile()` inside a subtransaction does not, because the start of one is not visible to the extension, and there such an error is raised), and an error it raises (other than a cancel, a shutdown or running out of memory) leaves the access method unclassified, with a line in the log, and does not fail the call (C12-06); a handler that failed is not called again by any pass of the same call, and its error is logged once (C13-01).  **What it leaves behind**: no lock.  Each table's lock (`ACCESS SHARE`, `SHARE UPDATE EXCLUSIVE` with `fix`) is let go as soon as the table has been looked at, and the catalogs' at the end of the call, so the call needs one entry of the shared lock table at a time and works with any number of columnar tables (it used to keep them all until the end of the transaction, and failed with `out of shared memory` with more tables than the lock table has room for); a lock that the caller's transaction already held on a table stays the caller's.  **What it costs in memory**: the stripe list of one table at a time (a descriptor of about 100 bytes and the stripe's statistics, a few hundred bytes for a table of a few columns, for each stripe; freed after the table), and the list of table oids; nothing grows with the number of rows; with the number of tables only the list of their oids grows, and, for the tables that are `not_checked` because they are locked, the rows themselves (about 700 bytes each in the result) and one copy of the server's lock data, which is read once for each 64 such rows and freed, and at the end of the call (a "held by" text is therefore a view of the lock manager as old as the first of the up to 64 rows that share one copy: it can name a holder that has let go since, or none for a lock taken after the copy was made; the `not_checked` row itself is never wrong, C13-02) (it was read for every row and kept to the end, so that a large DDL that locks many tables made the memory of the call grow with the square of their number, C12-02).  **The orphan step** reads the stripe catalog and `pg_class` at two moments; a row is reported (and, with `fix`, deleted) only if two snapshots, one taken before and one after the read of `pg_class`, both see it, and no live file claims it in a second read of `pg_class` after the catalog was read, so a `DROP TABLE`, `VACUUM FULL` or `SET ACCESS METHOD` that commits meanwhile makes no false `orphan_catalog_rows` (it did, in 10 to 30 percent of the checks while DDL ran).  Where PostgreSQL cannot begin a subtransaction (a callback of another module at the end of a transaction, in a critical section: see "Writes while a transaction is ending") the call is refused with a WARNING and returns no rows, which is not a clean result either; no statement of a session is ever in that state. |
| `kingres.columnar_check_clean()` | Superuser. The strict form of `columnar_check()`: `true` only when every columnar table was checked and nothing was found, `false` for any row `columnar_check()` would return, a `not_checked` table included (and for a refused call, with the warning).  It is never `true` for a table that was not looked at (a table made or dropped during the call included), so a script that tests this function and not the names of rows cannot pass silently; when it is `false`, `columnar_check()` says why.  It reads, locks and fails exactly as `columnar_check()` does (as of now, never waiting, ending with a cancel or a timeout), and covers what "clean" covers there.  **How to use it on a busy database.** Rows that are false alarms by design appear while DDL runs: a table dropped or made during the call, a table that another transaction holds `ACCESS EXCLUSIVE` (a rewrite, `TRUNCATE`, a vacuum's truncation) and, with `fix`, one that a vacuum holds `SHARE UPDATE EXCLUSIVE` are `not_checked`.  So `columnar_check_clean()` is a good "is it all fine right now" on a quiet database and a flapping signal on a busy one: a monitor should treat `false` as "look again", run `columnar_check()` to see why, and raise an alarm only when the same damage row (`stripe_beyond_file`, `unreadable`, `page_header_mismatch`, `orphan_catalog_rows`) appears in two runs a short time apart, or a `not_checked` row for the same table keeps appearing for longer than any DDL or vacuum of yours takes.  A `not_checked` row names the processes that hold the lock. |
| `kingres.columnar_check_indexes(rel regclass)` | Superuser. The cross-check of the indexes of one table against its stripes: `index_entry_missing` for an index that lacks entries for rows the stripes hold (see "B-tree indexes"); entries without a row are expected garbage and not reported; an index above 50 000 000 entries is `not_checked`. |
| `kingres.columnar_reconcile()` | Superuser. Deletes catalog rows of files no relation uses (it reads the first block of every columnar file of the database) and returns how many. A row that another session is deleting or has deleted is left to it, never an error. **It returns NULL, with a WARNING that gives the reason, when it did nothing** because a relation has an access method that cannot be classified or a columnar file has an unreadable metapage (C12-01): `0` means "looked, nothing to delete", NULL means "did not look".  `VACUUM` does the same work and stays quiet; its only trace is the log line. |
| `kingres.columnar_write_paths(reset)` | Superuser (grantable). How often each path of the write side was taken in this session (`late_commit`, `late_window`, `pressure_flush`, `own_flush`, `handover_flush`, `size_flush`, `prepared_skip`, `ceiling_write`, `ceiling_handover`, `refused_ending`, `orphans_cleaned`, `refused_live`, `lost_raised`) and, as `bytes_held` and `orphans_kept`, two numbers that are not counts: the memory the write buffers are charged for now, and the files the backend's last commit noted for its next transaction to clean up after; with `reset` the counts start again. A diagnostic, and what the tests of those paths read. |
| `kingres.columnar_is_columnar_am(amoid)` | Superuser only by default (the install script's event trigger runs it as the owner). Whether an access method is this extension's, by what its handler returns: the one definition that every C path uses.  The DDL guard (foreign keys, `AFTER ROW` triggers, publications) asks it, so an access method over any SQL function that has the extension's C entry point is guarded like the one over `kingres.columnar_handler` (C12-03). |
| `kingres.columnar_handler(internal)`, `kingres.columnar_finish_rewrites()` | The handler of the access method (not called from SQL), and the function that the event triggers call to remove the stripe rows of the files a command dropped (see "VACUUM and space"); `EXECUTE` is revoked from `PUBLIC`. |
| `kingres.columnar_identity(rel)`, `kingres.columnar_creating_xid(rel)`, `kingres.columnar_table_xid(rel)` | The identity of the stored content, the id of the transaction that created the file (2 once frozen) and the id that stands for the creation of the table (0 when it is not known, 2 once frozen): what the metapage holds (see "On-disk format"). |

`SELECT` privilege on the table is needed for `columnar_options`,
`columnar_info`, `columnar_stripes`, `columnar_identity`, `columnar_creating_xid`
and `columnar_table_xid`: the last three return a stable identifier of the
stored content and transaction ids, which say when the table was made and
rewritten.  PostgreSQL's size functions (`pg_relation_size`) ask for nothing
because they return a size; these return identifiers of a table's content, so
they ask for what reading the table asks for, as `columnar_info` and
`columnar_stripes` already did (a role without the privilege gets "permission
denied for table", tested in `columnar_security`).  `columnar_set_options` and
`columnar_compact` need ownership.

### Settings

| Setting | Default | Meaning |
|---|---|---|
| `kingres_columnar.compression` | `auto` | Codec of a new table: `none`, `pglz`, `lz4`, `zstd`, `auto` (zstd, else lz4, else pglz). Kept in the metapage. |
| `kingres_columnar.compression_level` | 3 | zstd level (1 to 19); lz4 and pglz ignore it. |
| `kingres_columnar.stripe_row_count` | 150000 | Rows of a full stripe (1000 to 10000000). |
| `kingres_columnar.chunk_row_count` | 10000 | Rows of a chunk group (100 to 100000). A group also ends when its raw size reaches 32 MB. |
| `kingres_columnar.write_buffer_memory` | 128MB | Memory for the rows of stripes being built, per backend, all tables together (1MB to the largest value). A soft limit with a hard ceiling of twice this value, see "Memory and costs": the buffers are written as soon as they are over it and the running subtransaction may write them, and a write that would go on from above the ceiling is refused with an error. A buffer that a running scan reads is never flushed under it. A flush that memory asks for writes whole buffers, the largest first, and a buffer is written alone only when it has `max(chunk_row_count, 1000)` rows or half of this memory, so the stripes of a table do not depend on how its rows were grouped into subtransactions. |
| `kingres_columnar.orphan_notes_max` | 10000 | Developer setting, superuser only: lowers the most files that a commit keeps for the next transaction to clean up after (those another module dropped after the clean-up had run), so the limit can be tested with a handful of tables.  It cannot raise the compiled limit. |
| `kingres_columnar.check_gate`, `kingres_columnar.check_gate_orphans`, `kingres_columnar.check_gate_claims` | 0 | Developer settings, superuser only (a role that was given `GRANT SET ON PARAMETER` could set them too, and can do nothing with them): the first integer of an advisory lock `pg_advisory_lock(key, 0)` that `columnar_check` and `columnar_check_clean` wait on, the first at the start of the look at each table (after the list of tables is made), the second in the orphan step between its first snapshot and its read of `pg_class`, the third between that read and its second snapshot, until the lock is no longer held, so that `t/036` can drop or make a table, commit DDL, or cancel the call, at that moment and not at one it can only hope for.  0 is no gate.  **They exist in every build** (the tests that use them run on production builds, which skip fewer tests than assertion builds).  So that they cannot be used to hold up an administrator's check (any role can take any advisory lock), a gate waits **only for an advisory lock that a session of a superuser holds**; the wait is bounded (two minutes) and can be cancelled.  A check run with a gate set by an administrator (`ALTER ROLE ... SET`) can therefore wait only for another administrator's session. |
| `kingres_columnar.vacuum_truncate_lock_timeout` | 5s | Developer setting, superuser only: how long the truncation of a `VACUUM` asks for its exclusive lock (every 50 ms) before it gives up for this visit.  The default is heap's 5 seconds; the tests raise it so that a verdict does not depend on how fast the machine is, or lower it to see the give-up quickly. |
| `kingres_columnar.max_stripe_rows` | 10000000 | Developer setting, superuser only: lowers the most rows the writer puts in one stripe (and the size above which `columnar_check` reports a stripe), so the limit can be tested with a few thousand rows. It cannot raise the compiled limit and does not touch the reader, so no value can make a table unreadable. |
| `kingres_columnar.reserve_row_numbers` | off | Developer setting, superuser only, for tests (0.2, C6a): the session reserves row numbers when it inserts (see "Reserved row numbers"). A row then has its TID when the insert returns, `INSERT ... RETURNING ctid` shows it, and it is the TID the row keeps. Off: nothing changes, the same stripes and the same row numbers as 0.1. It decides per buffer, when the buffer is made: set it before the first write of a transaction. It is ignored for a table whose file is of format version 2 (written by 0.1) and while the stripe catalog is the one of 0.1. A buffer for a table that has an index reserves by itself (C6b); the setting makes the sessions of the tests reserve for a table without one. |
| `kingres_columnar.reserve_min_rows` | 64 | Developer setting, superuser only (0.2, C13-02): how many row numbers a writer reserves at first for a table that has an index; the reservation grows in place while its buffer fills, up to the stripe size.  0: a whole stripe's worth, as 0.2 did before C13 (the tests of C6a that describe that layout set it).  It can be set in `postgresql.conf` (the extension's settings of the write path could not be before C13: they were defined after the extension's prefix was reserved, so a line in the file was dropped with a warning). |
| `kingres_columnar.vacuum_compact_stripes` | 128 | Superuser only (0.2, C13-05): `VACUUM` merges the small stripes of a table when at least this many are smaller than a sixteenth of the stripe size; 0 turns it off.  The merge is the last step of the `VACUUM`, is skipped when 90 percent of the table's row numbers are used, and an error in it is a WARNING, not a failed `VACUUM` (C14-02). |
| `kingres_columnar.vacuum_compact_rows` | 1000000 | Superuser only (0.2, C14-03): the most rows one `VACUUM` copies when it merges small stripes (only stripes smaller than a sixteenth of the stripe size are merged); the rest is merged by the next `VACUUM`.  0: no limit.  See "Subtransactions" below for what the merge costs the rest of the cluster. |
| `kingres_columnar.reserve_gate`, `kingres_columnar.reserve_gate_at` | 0, 0 | Developer settings, superuser only, for tests: a writer that reserves waits, at the point `reserve_gate_at` names (1 the numbers are taken from the metapage, 2 the reserved catalog row is inserted, 3 the pages of the stripe are written, 4 the reserved row is updated; 6 an index vacuum has read the metapage and the catalog and not yet scanned the index, 7 a VACUUM has done its merge of small stripes, 8 an index vacuum holds its lock and has not yet read the metapage and the catalog), for as long as a session of a superuser holds the advisory lock `pg_advisory_lock(reserve_gate, 0)`, as `check_gate` does for the check. `t/046` stops the server at each of them. 0 is no gate; the wait is bounded (two minutes) and can be cancelled. |
| `kingres_columnar.fault_point`, `kingres_columnar.fault_nth` | off, 0 | Developer settings, superuser only, present only when the server is built with assertions (`USE_ASSERT_CHECKING`): make the nth allocation of the write path (`alloc`), the nth relation of a subtransaction's hand-over to its parent (`merge`) or the nth relation of a flush (`flush`) fail once with "out of memory". Used by `t/016_atomic_handover.pl`; absent from production builds. |
| `kingres_columnar.enable_custom_scan` | on | Use the Columnar Scan (projection, skipping). |
| `kingres_columnar.enable_chunk_skipping` | on | Use minimum and maximum to skip. |

### Changes made in place to the 0.1 interface in the tenth round

The three sections "Changes made in place ..." are the record of the review rounds
of version 0.1, in the wording of their time: 0.1 was not released then, so its
interface was changed in place. The interface described in this file is the
current one; what a round changed is part of it. This round changed:

* `columnar_check(fix)` and `columnar_check_clean()` read the stripe catalog as of
  now, not through the caller's snapshot (C10-01); they report a table dropped,
  made another kind of table, or made during the call as `not_checked` (a new
  reason, with a table); they take every lock without waiting, and a catalog they
  cannot lock is one `not_checked` row with no table (`relation` null) and no
  further row (C10-02); an error that is not damage to the table (a cancel, a
  statement timeout, a lock timeout, out of memory, ...) is now raised, where it
  used to be an `unreadable` row (C10-02).
* `relation_nontransactional_truncate` raises an error, where an error is allowed,
  for a table that was not made or given a new file in the transaction (C10-05).
* A critical section inside a subtransaction begun after the extension's own
  pre-commit is alive, not decided: the transaction fails at that
  subtransaction's pre-commit where it used to commit without the call (C10-03).
* `VACUUM` and autovacuum give back the tail of the file only by heap's rule, so a
  vacuum with nothing worth cutting takes no `ACCESS EXCLUSIVE` lock (C10-06).
* `relfrozenxid` of a truncated or rewritten table is not newer than the ids its
  file holds, and the autovacuum line prints the value `pg_class` holds (C10-04).
* The developer setting `kingres_columnar.check_gate` (the table above) was added
  for the tests of the check functions.

### Changes made in place in the eleventh round

* One definition of "a columnar relation": the handler of the access method, never
  its name (C11-02).  Every place that compared `relam` with the access method named
  `columnar` now asks `kc_am_class` (`kc_catalog.c`): the list of tables of the check,
  the list of live files of the clean-up of the stripe catalog, the drop note
  (`kc_write_note_dropped`), the planner's path (`kc_cscan.c`), the transient table
  of a rewrite (`kc_am.c`) and, in the install script, the event trigger that refuses
  foreign keys and the publications that would include the stripe catalog.  The clean-up deletes nothing while a relation's
  access method cannot be classified.
* The orphan step reads with two snapshots and a second claim (C11-01); the check
  lets go of each table's lock when it has looked at it (C11-03); the in-place truncate
  (`relation_nontransactional_truncate`) is for the current subtransaction only (C11-04); the
  vacuum decides the failsafe for the table's own age and records the frozen id before
  it truncates (C11-05); `columnar_info` has a seventh column, `unreferenced_bytes`
  (C11-06); the not_checked rows name the holders of the lock (C11-09).
* New developer settings: `check_gate_orphans`, `check_gate_claims` and `vacuum_truncate_lock_timeout`
  (the table above); the gates now wait only for an advisory lock held by a
  session of a superuser (C11-07).
* The stripe catalog's snapshot copies are private: the snapshot that
  `GetLatestSnapshot()` hands out is a static one whose arrays the next call
  overwrites, and a shallow copy kept across two calls saw the second one's running
  transactions with the first one's bounds (found while writing the test of C11-01).

### Changes made in place in the twelfth round

* A clean-up that stands down is visible (C12-01): `columnar_check()` returns a
  `not_checked` row for it, `columnar_check_clean()` is false, `columnar_reconcile()`
  returns NULL with a WARNING (it returned 0).  `VACUUM` is unchanged (quiet, one log line).
* The lock data of the server is read once for each 64 `not_checked` rows of a call and
  freed, not once for each row and kept (C12-02); the gate's poll frees its copy too.
* One definition of a columnar access method for the SQL guard and for C (C12-03): the new
  function `kingres.columnar_is_columnar_am(oid)`; the install script is changed in place.
* A write to a relation that has the extension's callbacks and is not recognised by it (its
  access method's handler returns a copy of the routine) is an error where it used to be
  accepted and dropped at commit (C12-04).
* The handler of another access method that raises an error no longer ends the check, the
  clean-up or `VACUUM`: the access method stays unclassified and the clean-up stands down
  (C12-06).
* The test module `t/kcprobe` has two table access method handlers for these cases
  (`kcprobe_am_copy`, `kcprobe_am_error`); `t/042` is new.

### EXPLAIN

```
Custom Scan (Columnar Scan) on events
  Filter: (ts >= ...)
  Columnar Projected Columns: 2 of 3 (ts, value)
  Columnar Skip Conditions: ts
  Columnar Stripes Skipped: 0 of 4            -- with ANALYZE
  Columnar Chunk Groups Skipped: 31 of 45     -- with ANALYZE
  Columnar Compressed Bytes Read: 1234 bytes  -- with ANALYZE
```

A parallel scan is a `Parallel Custom Scan (Columnar Scan)` under a `Gather`; with
`ANALYZE` the counters are the sum over every participant, and one more line says
who read what:

```
  Columnar Stripes Read By: leader 12 (12000 rows), worker 0 31 (31000 rows), worker 1 37 (37000 rows)
```

(only the workers that read a stripe are listed; `BUFFERS` shows the pages all of
them read).  For a plan that is stopped early (a `LIMIT`) the figures of workers
that were still running are the last ones they published.

Skipping is an optimisation only: the clauses stay in the plan as filters.
Skipping understands `col OP expr` (OP in the btree family of the column's
type, constants, parameters and stable functions on the other side),
`IS [NOT] NULL`, `= ANY (array)` and `AND`/`OR` of these. Statistics are kept
for plain base types without a collation or with the `C` collation, and when
a minimum and a maximum are at most 64 bytes each; arrays, ranges, composites,
enums, domains and text under a non-`C` collation have none.

## On-disk format

One main fork, no other fork.

* Block 0, the metapage: magic, format version, options, the next row
  number and the next stripe id, the identity, the creating transaction, the
  relation the file was created for and a flags word (`REPLACES_CONTENT`: the
  file replaced the content of an existing table, TRUNCATE). Changed in place and logged as a whole page.
  The format version decides what a table is: **2** is what 0.1 writes (row
  numbers are given when a stripe is written); **3** (0.2) is a table whose
  writers may reserve row numbers at insert and whose stripe catalog may hold
  reserved rows.  This server reads both.  A new file is version 3 when the stripe
  catalog has the column `reserved` (the extension is at 0.2) and version 2 when it
  does not (the library is new and `ALTER EXTENSION ... UPDATE` has not run yet:
  the file is then byte for byte what 0.1 writes).  Only a file of version 3
  reserves; the library of 0.1 refuses a file of version 3 by its version.  The
  version of a file never changes: `VACUUM FULL`, `TRUNCATE` and every rewrite
  write a new file, which is version 3.
* Every other block belongs to a stripe: a run of consecutive blocks written
  once, never changed, each with a 24-byte special area (magic, page number,
  page count, writer transaction, stripe id). The payload of the pages, 8144
  bytes each, is the stripe blob.
* The stripe blob: a 64-byte header, the chunk groups, the footer. A chunk
  group holds, for each column, one chunk: an optional null bitmap, then the
  non-null values densely (fixed-width values in server byte order;
  variable-length values as an array of 32-bit lengths and the bytes without
  their header), compressed as a unit; a chunk that does not shrink is stored
  uncompressed. The footer lists, per chunk group and column, the offset, the
  compressed and raw sizes, the codec, the null count and the minimum and
  maximum. Its CRC-32C is in the header. Large values are stored inline (there
  is no TOAST); a chunk group closes early to keep its raw size near 32 MB.
* The stripe catalog `kingres.columnar_stripe`, an ordinary heap table keyed by
  the identity of the stored content (a random id and the creating transaction, in
  the metapage, protected by its own checksum): stripe id, first row, counts, first block,
  block count, sizes, the command-id change points of the writer (`cid_points`)
  the stripe-level statistics (`col_stats`) and a `maintenance` flag (the stripe
  was written by `columnar_compact`: it copies rows readers see in the stripes
  it replaces) and, since 0.2, a `reserved` flag (a claim on row numbers whose
  stripe is not written yet, see "Reserved row numbers"). A stripe is visible exactly
  when its catalog row is, so rollback, crash recovery, standbys, backup and
  `VACUUM FULL` follow from PostgreSQL's own rules. A byte-for-byte copy of the file (`SET TABLESPACE`, a base backup) keeps
  its identity and so its stripes, with nothing to update; a rewrite or
  `TRUNCATE` creates a new file with a new identity and an empty catalog.
  Older snapshots, in every isolation level (a READ COMMITTED statement that
  reads through a STABLE function has a snapshot older than the command that
  ran meanwhile): `SET TABLESPACE`, `columnar_compact` and `VACUUM` change
  nothing for them. `VACUUM FULL` does not change what the table contains, and
  keeps it for them as heap does, when every stripe was committed before the
  oldest snapshot (so before every snapshot that exists): the new stripes get
  frozen catalog rows and the new file records no creating transaction. When
  some stripe is newer than a snapshot that is still open, or something is in
  progress or recently deleted (`columnar_compact`), the old snapshot would
  have to see a different set of stripes and gets the error instead (below).
  A snapshot older than the *creation* of a table is a different case: the
  table's first file (created for that very relation, not by `TRUNCATE`)
  replaced nothing, every stripe in it is newer than the snapshot, and the
  snapshot is handed an empty table exactly as heap does (a new partition,
  `CREATE TABLE AS`, `ATTACH PARTITION` of a table created after the
  snapshot, a table dropped and created again under the same name). The rule:
  the error is raised only for content that existed for the snapshot and was
  replaced. The metapage therefore holds, besides the transaction that created
  the file, the one that created the *table* (`table_xid`), recorded only when
  it is known for certain: a table made by `CREATE TABLE` (also `CREATE TABLE
  AS`, a partition, a materialized view) has the transaction that made it;
  `TRUNCATE` copies it from the old file and a rewrite (`VACUUM FULL`, `ALTER
  TABLE`, `REFRESH MATERIALIZED VIEW`) from the columnar table it rewrites; a
  table that was not columnar and is converted has the transaction that
  converts it when it was made in that very transaction or has no storage at
  all (nothing could have been seen), and otherwise none (0, "not known"). The
  transaction that last wrote the table's `pg_class` row is not used: any
  `GRANT`, new owner, storage option or `ADD COLUMN` writes the row again, later
  than the creation, and a snapshot that saw rows would then be given an empty
  table. A snapshot that cannot see `table_xid` is older than the table, so
  whatever files the table has had since, there was nothing for it to read and
  it gets an empty table, like heap: a table created after the snapshot and
  then truncated, vacuumed full, rewritten or moved to columnar. When
  `table_xid` is 0 (a table converted from another access method, or a file of
  an earlier build) the reader asks the catalog instead: the snapshot sees an
  old version of the table's `pg_class` row, and one stays in the catalog for as
  long as the snapshot lives, whatever has been done to the row since, exactly
  when the table existed for it. If none is visible the table did not exist for
  the snapshot and it gets the empty table; if one is, content may have existed
  that the conversion replaced, and the snapshot gets the error. An error is
  therefore the answer to every older snapshot that may have seen content of the
  table, which is stricter than heap, which answers such a snapshot with an
  empty table after its own rewrites (TRUNCATE, `ALTER TABLE ... TYPE`,
  `SET ACCESS METHOD`); the extension keeps the strictness on purpose, and
  never an empty table for content that existed. A heap table that was
  truncated or emptied and vacuumed before it was converted has no storage and
  gives the empty table, as heap does. `table_xid` is an id in the file like
  the creating one: VACUUM freezes it (0 stays 0) and counts it for
  `relfrozenxid`. `TRUNCATE`, `SET ACCESS METHOD` and `ALTER TABLE` that
  rewrite the rows (`ALTER COLUMN TYPE`, `ADD COLUMN` with a volatile default)
  change what the table contains: a snapshot older than the commit does not get
  an empty table (as heap gives for these): its read fails with SQLSTATE
  `40001`, naming the rewrite. `CLUSTER` is refused (a columnar table has no
  index).
* Transaction ids: a columnar file holds two, in its metapage: that of the
  transaction that created the file (`kingres.columnar_creating_xid(rel)`, 2
  once frozen) and that of the one that created the table
  (`kingres.columnar_table_xid(rel)`, see above; 2 once frozen, 0 when it is not
  known). For a table created in a
  subtransaction the id is the subtransaction's
  id, which is right for every use made of it (comparison with a snapshot and
  with the oldest running transaction, `relfrozenxid`, the horizon of the
  freeze record: plain id comparisons that PostgreSQL makes with subtransaction
  ids itself). VACUUM freezes each once every snapshot can see it (it precedes the
  oldest transaction still running; the change is WAL-logged and cancels
  hot-standby queries that might hold it) and reports the oldest id that is
  left as the table's `relfrozenxid`. A new file (`TRUNCATE`, a rewrite, a new
  table) is given a `relfrozenxid` that is not newer than any normal id its
  metapage holds (C10-04): the id of the creating transaction, and the table's
  own id that a truncate or a rewrite keeps from the old file, which can be
  older; so `relfrozenxid` never claims more than is true, and `datfrozenxid` and
  the server's wraparound warnings do not run late by the distance between the
  table's creation and its truncate.  The pages of a stripe hold no
  transaction id (that field is reserved, zero).
* WAL: the resource manager `KingresColumnar` logs whole-page images only
  (`STRIPE_PAGES`, `METAPAGE`, `FREEZE`, and since 0.2 `RETURN`); redo restores
  images and has no logic of its own. A stripe's pages are logged before its catalog row
  is inserted.  A reservation of row numbers is the existing `METAPAGE` record (the
  metapage image after the next row number moved), and the tail of a reservation
  that is given back is the new `RETURN` record (the image, and the numbers
  given back for a person who reads the log): no existing record changed its
  meaning, and a log without `RETURN` records replays exactly as before.

What is validated when a chunk is read: page and stripe identity, the footer
CRC, offsets, counts, codec ids, decompressed sizes, statistic lengths and
headers, and a CRC-32C of every stored chunk, checked before the chunk is
decoded, independent of data checksums, so accidental damage gives SQLSTATE
`XX001`. The structure of array values is checked (header, dimensions, null
bitmap, element sizes) before the array code sees them. Other variable-length
types (numeric, jsonb, ranges, tsvector, geometric types, ...) are handed to the
type's own code after their lengths are checked, as heap does for its pages: a
file written on purpose, with valid checksums, by someone who can write the data
directory can still crash the server through such a type, as with heap. The
metapage has its own checksum. A damaged byte inside a stripe page is caught by
the chunk checksum; page checksums, when enabled, catch it earlier.


## B-tree indexes (0.2, increments C6b, C6c and C13)

`CREATE INDEX` on a columnar table builds a B-tree over the rows of
the table, and index scans find rows by TID: a TID is a row number
(`block * 2048 + offset - 1`).  What is served: plain, multi-column, expression and
partial indexes, `INCLUDE` columns, `DESC` and `NULLS FIRST`, collations, and any
column type that has a B-tree operator class (arrays, `jsonb`, `numeric`, text,
dates, ...); the build reads the stripes with the maintenance rule and the rows
of its own transaction after writing them.  Planner: index scans, ordered index
scans (`ORDER BY`, merge joins, `LIMIT`) and the inner side of nested loops.  **No
bitmap scans**: the bitmap code of the server holds at most `MaxHeapTuplesPerPage`
(291) offsets for a block and a columnar block has 2048, so such a bitmap cannot
be made (the flags of the index descriptions are switched off for a columnar table in `set_rel_pathlist_hook`, which makes the index paths again, because PostgreSQL 19 has no hook where they are made).  **No index-only scans** (there is no visibility map: each
would fetch every row, so `canreturn` is off) and **no parallel index scans** (a
worker cannot read the rows of the leader's transaction; `amcanparallel` is off).
A reindex after a rewrite is the server's own (`VACUUM FULL`, `ALTER TABLE ... SET
ACCESS METHOD` in both directions, `TRUNCATE`, `REINDEX`).

* **Refused, with a message** (`columnar_refusals`, `columnar_index`, `columnar_unique`):
  exclusion constraints and deferrable unique constraints (the check cannot be
  postponed: a duplicate is found when the row goes in), `ON CONFLICT` of every form
  on a columnar table that has an index (`DO NOTHING` and `DO UPDATE` need the
  speculative insert and the row lock of a conflicting row; there is no row lock), hash,
  GiST, GIN, BRIN and SP-GiST indexes, `CREATE INDEX CONCURRENTLY` and
  `REINDEX CONCURRENTLY`, `CLUSTER` (it would reorder the rows, and an index names rows
  by their number), a build on a file of format version 2 ("its file was written by
  version 0.1": rewrite the table first).  A build in a subtransaction over rows that
  an enclosing one wrote, or while a cursor reads the buffers, is refused: the rows
  cannot be written first.  **Foreign keys in both directions are refused**: from a
  columnar table (as before) and now also **to** one (`columnar_check_ddl` looks at the
  referenced table of a key too): a key check reads the referenced row with a row lock
  that a columnar row cannot give.
* **Unique indexes, primary keys and unique constraints (0.2, C6c)** work as on a
  heap table, and are never allowed to admit a duplicate.  A row's index entry is made
  when the row is inserted, with its reserved number, so the check sees the rows of
  other sessions: the answer of `kc_catalog_probe_row` for a number is "no row",
  "a row" (committed or this transaction's own) or "a row of a transaction in progress,
  wait for it" (the stripe catalog holds the claim of every writer from its first insert,
  the reserved stripe row); the B-tree waits on that transaction (`XactLockTableWait`)
  and asks again, so a prepared transaction holds a key until `COMMIT PREPARED` or
  `ROLLBACK PREPARED`, savepoints give their keys back at once, and deadlocks between
  two writers are found by the server's detector and reported as for a heap table.  The
  claim is made while the writer holds a **reservation lock** in `SHARE` mode (an advisory
  class lock on the relation, taken from "numbers taken from the metapage" to "claim row
  inserted"), which an index vacuum takes in `EXCLUSIVE` mode while it reads the
  metapage and the live ranges, so a vacuum never reads a state with a number taken and
  no claim.  Writers do not wait for each other for it.  `CREATE UNIQUE INDEX` waits for
  running writers (its table lock conflicts with their `ROW EXCLUSIVE`), then sees their rows
  if they committed and reports the duplicate as the server does.  Tested against a heap
  twin under concurrency (`t/053`: pgbench with 2, 4 and 8 sessions, and a thousand random
  interleavings of two transactions at the three isolation levels with prepare, commit
  prepared and rollback prepared), with a crash (`t/054`), with savepoints, a waiter and
  a writer that flushes meanwhile, a deadlock and a build under a running writer
  (`t/055`), and on a standby (`t/057`).
* **VACUUM cleans the indexes (0.2, C6c)**: `VACUUM` of a table with indexes calls the
  B-tree's bulk delete with a callback that says an entry is dead when its row number is
  in no live range: the ranges of the stripe rows whose writer did not abort, minus
  versions deleted before the oldest snapshot, merged; every number from the lowest
  range that a running or own writer holds upward is kept; numbers below the metapage's
  next number that no range covers are the rolled-back writers' and are removed.
  `VACUUM (INDEX_CLEANUP off)` leaves the indexes alone; autovacuum does the same as
  `VACUUM`.  The index file does not shrink (a B-tree only reuses freed pages; the
  index of the heap twin and this one are of the same size after the same work), the
  garbage entries are gone (`t/056`: a million rolled-back entries are removed).
  The numbers of **rows** that were put in are **never given back** to the metapage: a rolled-back writer
  has index entries for them (they are made when the row is inserted), and a number that is used again by
  another row would make such an entry find the other row, with a key that is not its key.  What can go
  back is the unused tail of a reservation, after the last row's number: at the flush of a buffer
  that commits, and, since C13, at the **next reservation of the session** for the unused tail of a
  buffer that an abort dropped (see "What gives the numbers back, and what does not").
* **Compaction of a table with indexes (0.2, C6d).**  `columnar_compact` works on a table that has
  indexes of any kind this version allows (unique and primary keys too).  The copies of the rows get
  new row numbers, so after the copy **each index gets an entry for each copied row**
  (`index_insert` without a uniqueness check, from the stripes the compaction wrote, with the
  predicate and the expressions of the index); the entries of the old numbers stay.  **The lock is
  the one the compaction had already**: `SHARE UPDATE EXCLUSIVE` on the table, and `ROW EXCLUSIVE` on
  each index, which is what a writer takes; readers and writers of the table are not blocked, and
  nothing waits for the compaction but another compaction or a `VACUUM` of the table.  **Why the
  indexes are not rebuilt**: a rebuild replaces the index with one that names only the new numbers,
  and a snapshot older than the compaction (REPEATABLE READ or a cursor, which still sees the old
  stripes) that reads by the index after the commit would find none of its rows; with entries for
  both, such a reader finds its rows through the old entries and every later one through the new
  (`t/059` has the reader).  A unique index is safe through it: while the compaction runs a writer
  that meets a key finds the row in the old stripe (a stripe a running transaction deleted still
  holds its rows) and gets the duplicate error, and after the commit the copy is found the same
  way.  **Cost**: the compaction reads the stripes it wrote once more (a decode of every column
  of every copied row) and makes one index insertion per row per index, so it takes about as long as
  an index build over the copied rows plus the copy; the indexes hold twice the entries of the copied
  rows until the next `VACUUM`, which removes the entries of the numbers that no stripe any
  snapshot can see has any more (the ranges of stripes deleted before the oldest snapshot are not
  live, see "VACUUM cleans the indexes"); a compaction that rolls back or crashes leaves entries
  for numbers of stripes that never committed, which are filtered like those of a rolled-back
  writer and removed the same way.  A table that is compacted again and again between vacuums
  grows its indexes by the copied rows each time; vacuum after compacting.
* **An index built or rebuilt while an older snapshot lives (C13-01).**  `CREATE INDEX` and `REINDEX`
  (and the build behind `ALTER TABLE ... SET ACCESS METHOD`) put into the index the rows of the stripes visible
  now **and** those of the stripes only an older snapshot can still see (a compaction replaced them and no
  VACUUM has made them removable yet), the second kind as "not alive", the way heap indexes recently dead tuples:
  without them a REPEATABLE READ snapshot older than a compaction found no row through an index created after
  it, while its sequential scan found all of them.  In a unique build the copies repeat the keys of the replaced
  rows, which is no duplicate (not alive rows are kept out of the uniqueness check).  In a parallel build the leader
  alone indexes the replaced stripes.  The next `VACUUM` removes the entries once no snapshot can see those stripes
  (`t/063`).
* **`amcheck` (C13-04)** works: `bt_index_check` and `bt_index_parent_check`, **with `heapallindexed` too** (it
  reads the table through the access method's index build with a concurrent index description, accepted for an index
  that is valid and ready; `CREATE INDEX CONCURRENTLY` and `REINDEX CONCURRENTLY` build indexes that are not ready
  and stay refused with their own message).  `heapallindexed` reports a row that has no entry and so finds what
  `columnar_check_indexes` finds, without its limit of 50 million entries and its superuser rule (`t/065`).
* **`kingres.columnar_check_indexes(rel regclass)`** (superuser by default; `REVOKE`d
  from `PUBLIC`): reads every index of the table in full and the rows of the stripes
  as of now, and returns one row `index_entry_missing` for an index that has no entry
  for rows it should have (the count and the first three row numbers in `detail`); an
  entry without a row is the garbage the next `VACUUM` removes, and is not reported.  The
  stripe list is taken first, then the index, so a writer that commits in between
  cannot make a row look unindexed.  Cost: one pass over the index and one over the
  table, 8 bytes of memory per entry of an index; above 50 000 000 entries an index is
  reported as `not_checked` instead.  `columnar_check` does not do this (its cost must not
  grow with the size of the indexes).
* **Reservation switches on by itself**: a buffer made for a table that has an
  index reserves (`kc_rel_reserves`), with or without the developer setting.  A table
  that gets its first index while the transaction holds rows in buffers that did not
  reserve: the utility hook writes the rows of the running subtransaction as stripes
  before the build (they get their numbers then), and the later inserts reserve.
* **The fetch** (`kc_scan_fetch_row`): a row of a buffer of the fetching transaction
  that reserved is taken from the buffer by the command-id rule; any other from the
  visible stripe whose numbers hold it (binary search in the stripe list, kept for the
  scan and read again when this backend wrote a stripe since); the footer is kept
  for the open stripe and **one chunk group is decoded, all columns, and kept**, so
  consecutive fetches in the same group cost one decode.  A number that no visible
  stripe and no buffer has (an entry of an aborted or rolled back writer or
  subtransaction, of a writer not committed before the snapshot, of a removed stripe)
  returns nothing: dead entries are filtered by the fetch, and removed by `VACUUM`
  (see below).
* **Cost**: a point lookup decodes one chunk group of every column (100 to 10 000
  rows by `chunk_row_count`), so it is much dearer than a heap fetch; the index
  pays when a query returns few rows of a large table.  Measurements are in the
  design notes.

## Reserved row numbers (0.2, increment C6a)

A B-tree index needs a row's TID when the row is inserted, and other sessions
need to see that a writer is working on a range of row numbers.  Until 0.2 the
numbers were given when a stripe was written, at the flush, long after the
insert.  A write buffer that **reserves** takes **a small block of numbers** (since C13: 64, or what
the session's last buffer of the table needed, rounded up to a power of two; the developer setting
`kingres_columnar.reserve_min_rows`, 0 for the whole stripe of 0.2 before C13) from the metapage when its first row goes
in, and grows the block in place while the buffer fills and it is still the end of the numbers (up to
`stripe_row_count`, 150000 by default: a load by one session is one stripe, as before; the claim row
of the catalog grows with it), and inserts the stripe's catalog row in the state `reserved`, by the
transaction (or subtransaction) that inserts the row.  The rows after it have the
numbers after it, so a row has its TID when the insert returns it (block
`number / 2048`, offset `number % 2048 + 1`, as every scan shows it), and it is
the TID the row has after the commit whatever happens to the buffer: a flush by
size, by memory, one that a read causes (a parallel plan), a savepoint, `PREPARE
TRANSACTION`, write-through, a standby (`t/044`, `t/048`).  A scan of the rows a
transaction has not yet written shows the same TIDs.

* **The catalog row.**  `kingres.columnar_stripe` has one more column, `reserved
  boolean NOT NULL DEFAULT false` (`kingres_columnar--0.1--0.2.sql`).  A reserved
  row has the numbers it claims in `first_row` and `row_count` and nothing else
  (no pages, no statistics).  Readers skip it, whoever it belongs to.  A flush
  writes the pages of the stripe and then **replaces** the reserved row by the
  stripe's row (an ordinary heap update, with the same key, so a stripe is visible
  exactly when its row is, as before), and only then offers the numbers that no row
  used back.  What is left of a reserved row when its transaction ends is heap's to
  say: a transaction that aborts, or a backend that dies, leaves a row nobody sees,
  which a vacuum of the catalog removes with the other dead rows (a standby replays
  the same).  While its writer lives (an open transaction, a prepared one) the row is
  in progress: no check, reconcile or vacuum looks at it or touches it, and its range
  is the range of a writer that can be waited for (what a unique index will need,
  C6c).  **Because the update is made by a later command than the insert, the
  extension passes the command counter before it updates a reserved row**
  (`CommandCounterIncrement` where a flush happens in the statement that inserted the
  first row, as the flush by size does).  The reserved row is found again by its key
  at the flush, not by a kept place: the writer holds no lock on the catalog between
  the two, so a `VACUUM FULL` or `CLUSTER` of the catalog may move it.  **Cost**:
  every stripe written by a buffer that reserves is one insert and one update of the
  catalog where it used to be one insert, and leaves one dead row version for heap's
  pruning and vacuum; on a hot standby the WAL of that pruning cancels queries with
  an old snapshot (after `max_standby_streaming_delay`) as the WAL of any heap
  update does, which the catalog of 0.1 did not cause while only inserting.
* **What gives the numbers back, and what does not.**  The unused tail of a
  reservation is given back only if it is still the end of the range (nobody
  reserved after it), under the lock of the metapage, and only after the
  catalog row says how many numbers the stripe has (`RETURN`); a transaction that
  commits with a few rows and nobody after it wastes nothing.  Otherwise the numbers stay
  unused, **a gap**: a reservation behind another writer's.  A transaction or subtransaction that
  **aborts** cannot touch the file in its abort callback, so it notes the unused tail of its reservation
  (the numbers after the rows that were put in: the numbers of a row are never given back, an index
  entry may name them) and the session gives it back at its next reservation for the table, when it is
  still the end of the numbers (C13-02).  **The worst case of waste** is now **one small reservation** (64
  numbers, or the block the session's last buffer of the table needed) for every writer that aborts or
  commits while another has reserved after it, and what an aborting session gets back when
  nobody interleaves.  A table has about 8.8 x 10^12 numbers (the TID space): one session that
  aborts in a loop wastes about the numbers of its rows (measured: 3000 aborted one-row subtransactions
  use about 3000 numbers, where each used 150 000 before); two or more sessions that interleave their
  aborts waste up to 64 each time, so using the numbers up takes at least 1.4 x 10^11 aborted
  transactions: at the rate one session was measured at (about 120 000 aborts per second) about two weeks of
  continuous attack by two sessions or more, and a few days with many cores; it is a bound, not a proof of
  safety against a determined role with a connection to spare; and **any role that may INSERT can reach this bound, nothing else gives it more**.  When the
  numbers are used up an insert fails with "has no row numbers left"; the table's **owner** recovers it by
  rewriting the table (`VACUUM FULL`, or `ALTER TABLE ... SET ACCESS METHOD columnar`), which starts the numbers
  again; a WARNING at 90 percent used (once in a session) says so in time.  The numbers are
  not reused otherwise: no number that a row has or had, or that a live writer holds, is ever handed
  to another row (`t/047`, `t/066`).
* **Subtransactions (decision 3 of C6a).**  A buffer that reserves is never merged
  into another.  A committing subtransaction **writes such a buffer as a stripe of its own**
  (its own flush, the `own_flush` path) instead of handing the rows to the parent, so
  the rows keep the numbers and the stripe row the subtransaction inserted; one that
  a cursor still reads cannot be written, and becomes the parent's, which writes it.
  The cost is **one stripe per savepoint (or `EXCEPTION` block) that wrote**, however few rows
  (a stripe, a page of 8 kB and a row of the stripe catalog each),
  where without reservation the rows of savepoints merge into the parent's buffer
  and make full stripes.  **It is bounded since C13-05**: `VACUUM` merges the small stripes when at least
  `kingres_columnar.vacuum_compact_stripes` (128; 0 turns it off) of them are smaller than a sixteenth of the stripe
  size, as `columnar_compact` does with its defaults (the indexes get entries for the copies; the replaced
  stripes' blocks are listed free by the next `VACUUM` and reused), so a table that takes a savepoint per row is as
  large as that many stripes until its next vacuum (autovacuum runs after the inserts it counts, as for any
  append-only table), not for good; `columnar_compact` does it by hand, and renumbers the rows (below).
  **Operations notes (C14).**  The merge is the last step of a `VACUUM`, it runs under the transaction id the `VACUUM` then gets, and **that id holds back the cleanup horizon of every table in the cluster until the `VACUUM` of the table ends**: a lazy `VACUUM` normally takes no part in the horizon, and this one does, for the time of the merge.  The time is bounded: only stripes smaller than a sixteenth of the stripe size are copied, and at most `kingres_columnar.vacuum_compact_rows` (1 000 000) rows in one `VACUUM`; a table with more is merged over several vacuums.  The copies take new row numbers and the numbers of the replaced stripes are not used again, so **a merge burns row numbers** (about the number of rows it copies); it is skipped when 90 percent of the numbers are used, and the table's owner who sees the warning about the numbers should rewrite the table (below).  A writer that reserves row numbers for a table with an index updates the claim row of the stripe catalog each time its reservation grows (about a dozen times for a full stripe of a bulk load), which is catalog churn a bulk load of an indexed table pays that a load without an index does not.  The unused tail of an aborted writer's reservation is given back by its next reservation only when no index vacuum of the table is running (C14-01): it stays a gap then, at most the small reservation (63 row numbers) for each commit or abort, for the whole run of the index vacuum (hours, for a big table): on a table with a very high commit rate (100 000 commits per second) this is about 0.2 percent of the row numbers per hour of vacuum, and the warning at 90 percent remains the way to see it (C14-09).  A merge that is skipped because the `VACUUM` already has a transaction id (it deleted catalog rows of files that no longer exist) says so at `DEBUG1` (C14-10) and the next `VACUUM` merges.  An autovacuum that is cancelled in its merge (a DDL storm on the table) leaves the pages it wrote as blocks no stripe uses (the next `VACUUM` lists them free) and the numbers they took are gone.
  The coalescing of the buffers that cursors kept apart leaves buffers that
  reserve alone, too.  Nothing that reserves holds a list of reserved runs in the merge code.
* **Which tables, which sessions.**  Only a table whose file is of format version 3, in a
  database whose stripe catalog has the column (the extension is at 0.2), and only the
  rows of a session that has `kingres_columnar.reserve_row_numbers` on when its buffer is
  made.  The rows of a rewrite (`VACUUM FULL`, `SET ACCESS METHOD`, `ALTER TABLE ... TYPE`)
  and of `columnar_compact` are not reserved: they are written with the numbers of
  the flush, and the indexes of a rewritten table are built again.  `COPY`, `INSERT ... SELECT`
  and `CREATE TABLE AS` reserve like `INSERT`.  Rows written after the commit-time
  flush (write-through) take exactly the numbers they need and return them.  **Not in
  parallel mode**: a `CREATE TABLE AS` (or materialized view) whose query is parallel
  writes its rows in the leader while the executor is in parallel mode, where heap
  forbids the update (and the command counter passing) that replaces a reserved row, so
  such a buffer could never be written by size or by memory; its rows do not reserve
  and have their numbers when their stripes are written, as without the setting (the
  target of such a statement is new and cannot have an index).
* **What is as in 0.1.**  A TID scan (`WHERE ctid = ...`) is refused; a row is fetched by
  TID only through an index (see "B-tree indexes").  `columnar_compact` gives the rows it copies new numbers, so the TIDs of a
  table change when it is compacted, as they do when it is rewritten (and the indexes get
  entries for the new numbers, see "Compaction of a table with indexes"). `reserved_row_left` is a new row of `columnar_check`: a reserved row that a
  **committed** transaction left (no code path does; a writer replaces it before it commits
  or prepares), the only reserved row that is damage.  Under SERIALIZABLE a reader that skips the
  reserved row of a writer in progress reports the dependency on that writer, as it does
  for a stripe row it skips; with the setting off no reserved row exists and nothing is
  reported that was not before.

## Parallel scan

A table of more than one stripe can be read by a `Gather` (version 0.1, increment
C5; no change to the files, the catalog, the WAL or the SQL objects).  The unit of
work is the stripe: every participant (the leader and the workers) reads the list
of stripes visible to the query's snapshot itself, and takes the next stripe nobody
has taken from one atomic counter in the query's shared memory.  Each participant
computes a fingerprint of its list (the number, ids, first rows and visible rows of
the stripes); a list that differs from the leader's fails the query ("found another
list of stripes than its leader") and never answers with rows read twice or not at
all.  Projection and skipping work in every participant.

* **When a plan is parallel.**  A partial path ("Parallel Custom Scan (Columnar
  Scan)") is offered when `max_parallel_workers_per_gather` is above 0 and the
  relation is parallel-safe for the planner.  The number of workers follows the
  size of the file as for a heap table (`min_parallel_table_scan_size`, compared
  with the *compressed* pages, which is why a columnar table needs a lower setting
  than the same data in heap: **a columnar table reaches parallelism later than the same data in heap**, and `ALTER TABLE ... SET (parallel_workers = n)` overrides the threshold) or the table's `parallel_workers` option, and never
  exceeds the number of stripes minus one: a table of one stripe is never read in
  parallel.  The cost divides the per-row work by the participants and leaves the
  page cost whole, as a sequential scan does.  `kingres_columnar.enable_custom_scan
  = off` leaves no parallel scan of a columnar table.
* **What else may run in a worker.**  The plain (non-parallel) Columnar Scan is
  parallel-safe too: in a worker it reads the whole table, as the inner side of a
  partial join, under a `Parallel Append` as a whole child, or as the single copy
  that `debug_parallel_query` runs.  The access method's own scan (a `Seq Scan`) is
  not, and the planner never puts it under a `Gather`.  A query that a function
  runs inside a worker is not the plan the leader started, and is refused as before
  (`columnar tables cannot be read in a parallel worker`).
* **The rows of the leader's own transaction** exist only in its memory, and a
  worker cannot see them.  When the executor starts a plan that has a `Gather`, the
  Columnar Scan nodes of the leader write the running (sub)transaction's buffered
  rows of their table as stripes, before the executor enters parallel mode (where a
  catalog row cannot be written); the stripes carry the command ids that keep the
  rows of the running command out of the scan (design 7.1), so `INSERT ... SELECT`
  from the same table still reads only the rows of earlier commands.  Rows that
  cannot be written that way, those of an enclosing savepoint and those a scan of
  the same session is still reading, make the planner offer no parallel plan for
  the table.  A plan made earlier and run later (a prepared statement, a cached
  plan) that meets such rows fails with `columnar table "t" has rows that this
  transaction wrote and parallel workers cannot read`, and never answers without
  them.  A flush that a read causes makes a small stripe if the transaction wrote
  little (`columnar_compact` repairs it), the cost the savepoint design already has.
* **The plain `parallelscan_*` callbacks** of the access method are unchanged (a
  descriptor of no state): no plan has a parallel `Seq Scan` over a columnar table,
  because nothing could flush the leader's rows before the executor enters
  parallel mode on that path.

## What is refused, and why

All with SQLSTATE `0A000`.

* `UPDATE`, `DELETE`, `MERGE` that matches a row: "columnar tables are
  append-only". A row has no version header and no deletion mask. Convert the
  table or partition to `heap`, change it, convert it back, or drop whole
  partitions.
* Row locks (`SELECT ... FOR UPDATE/SHARE`), `ON CONFLICT DO UPDATE`,
  `TABLESAMPLE`, `CLUSTER`.
* Exclusion constraints, deferrable unique constraints, `ON CONFLICT` on a table with an index, foreign keys to a columnar table, indexes of any access method but B-tree, `CREATE INDEX CONCURRENTLY`, `REINDEX CONCURRENTLY` and `CLUSTER`: refused with a message that says why (see "B-tree indexes").
* Foreign keys on or to a columnar table, and row-level `AFTER` triggers
  (event triggers refuse them at creation; the executor re-reads such rows
  by TID, which a columnar row has not). `BEFORE ROW` and statement
  triggers work.
* Temporary and unlogged tables.
* A parallel-safe function that reads a columnar table inside a parallel worker
  fails with `0A000`: only the scans of the plan the leader started may run in a
  worker (see "Parallel scan").
* **Logical replication (0.2, C7): the inserts of a columnar table are decoded.**  A columnar
  table can be in a publication (`FOR TABLE`, also through the root of a partitioned table), and
  a subscriber (a heap or a columnar table) receives its rows: the initial copy is an ordinary
  read, and what is inserted afterwards is decoded from the log.  How: when `wal_level` is
  `logical` and the table is logically logged (`RelationIsLogicallyLogged`: not a catalog, not
  unlogged, not temporary), the flush of a stripe also writes `LOGICAL_INSERT` records, the rows of the stripe
  as heap tuples, up to 512 rows or about 1 MB to a record (**the WAL of a load at `wal_level =
  logical` is about twice what it is otherwise**, the raw rows in addition to the compressed pages; at any
  other level nothing is written), and the resource manager's `rm_decode` queues them as
  inserts of the reorder buffer, as `heap_decode` does for a heap insert (it needs the library on the
  publisher, and the standbys replay the records without a page).  The record is written by the
  transaction or subtransaction that writes the stripe, which is the one whose commit makes
  the stripe visible: the rows are sent when a reader could see them, a savepoint that is rolled back
  sends nothing even if its rows were flushed by memory pressure, a prepared transaction is decoded at
  `PREPARE TRANSACTION` when the subscription has `two_phase`, and a crash loses only what the
  log does not hold.  **What differs from a heap table**: the rows of a stripe are decoded when it is
  written (at commit, at `PREPARE`, at a flush by size or memory), so the order of the changes of
  one transaction across tables is not the statement order (the changes of a table are in order);
  `UPDATE` and `DELETE` do not exist, so a publication needs no replica identity for them
  (`REPLICA IDENTITY FULL` is accepted, for the checks that ask for one); `TRUNCATE` is decoded by the
  server as for any table; the copies `columnar_compact`, `VACUUM FULL` and
  `SET ACCESS METHOD` make are no new rows and are not logged.  **What is still refused**: a publication
  that would include the stripe catalog `kingres.columnar_stripe` (`FOR ALL TABLES`, `FOR TABLES IN
  SCHEMA kingres`), when a columnar table exists or is made: its rows describe the files of one server
  and must not be applied by a subscriber; list the tables (`FOR TABLE ...`) instead.  **A column added
  with a default after rows were inserted in the same transaction** (the ALTER comes before the stripe is written) is
  logged with the value a scan returns for those rows, the default, not NULL (C13-03, `t/064`); a heap row is decoded as
  it was when it was inserted, with no such column, and a subscriber gives it its own default.  Tested in
  `t/061_logical.pl` against a subscriber row by row.
* A TID scan (`WHERE ctid = ...`, a `Tid Scan`) is refused, and a row is fetched
  by TID only through an index. `ctid` shows a number made from the row's
  position; without reserved row numbers it is not stable and means nothing, and
  with them (0.2, "Reserved row numbers") it is the TID the insert returned, until
  `columnar_compact` or a rewrite gives the rows new numbers.

## VACUUM and space

`VACUUM` records page and row counts, returns the unreferenced tail of the file
when it is worth it (below), deletes catalog rows of dead files and resets the
insert counters so autovacuum does not repeat.

The tail is given back by the rule of heap's vacuum (`should_attempt_truncation`
and `lazy_truncate_heap` in `vacuumlazy.c`, the same in PostgreSQL 18 and 19),
because the lock that truncation needs is `ACCESS EXCLUSIVE`, which on a logged
table is written to the WAL and replayed on a hot standby, where it cancels every
query that holds the table (C10-06; before this every vacuum and every visit of
autovacuum took it, and ended the long queries on the standby that the table is
for).  The decision is made under the lock the vacuum already holds
(`SHARE UPDATE EXCLUSIVE`): the tail is the part of the file after the last stripe
that anything may still need (a stripe whose catalog row is live, in progress, or
deleted but not yet invisible to every snapshot).  Only when it is not empty and
holds at least 1000 pages or a sixteenth of the file, and the wraparound failsafe
is not on for this table (below), and neither `VACUUM (TRUNCATE false)` nor the table option
`vacuum_truncate = false` says no (`VACUUM (TRUNCATE true)` overrides the option),
is the exclusive lock asked for: without waiting, tried again every 50 ms for 5
seconds (checking for interrupts; wait event `VacuumTruncate`), after which the
truncation is given up for this visit without an error.  Under the lock the file
is measured again: if it has grown since the decision (the new pages belong to a
writer) nothing is done; else the tail is worked out again, exactly, now that no
writer can be in the middle of a flush, and cut.  A vacuum with nothing to cut
(the usual case for append-only data) takes no `ACCESS EXCLUSIVE` lock and writes
nothing a standby has to replay against its readers.  It differs from heap's in
two ways: the tail comes from the stripe catalog and the file size, not from a
scan that notes the last page with data, and heap's loop that goes on while a
waiting lock request interrupted the scan of the tail has no counterpart (finding
the tail is one catalog read, and a waiter does not wait behind it).  `t/037`
tests it with a primary and a hot standby (`t/037`, which also has a file over
16000 pages, where the 1000-page limb alone decides, and a reader that goes
away or stays while the vacuum waits; its wait for the lock is the developer
setting `vacuum_truncate_lock_timeout`, 5 seconds by default).

**The wraparound failsafe** is decided for the table's own age, as heap does it
(C11-05): `vacuum_xid_failsafe_check` of the server, the age of `relfrozenxid`
and `relminmxid` against `vacuum_failsafe_age` and
`vacuum_multixact_failsafe_age`, with the floor of 105 percent of
`autovacuum_freeze_max_age`.  Past it the vacuum warns in heap's words
(`bypassing nonessential maintenance of table ... as a failsafe`) and does not
truncate, because the exclusive lock is WAL-logged and so needs a transaction id,
exactly where the server may refuse to assign one.  It still advances
`relfrozenxid`: the frozen id is recorded **before** the truncation is tried, so
a truncation that is cancelled while it waits for its lock, or given up, has
already done what matters for the age (`t/041`).

**Space in the middle of the file is reused (0.2, C8).**  Pages of rolled-back or
crashed loads and of stripes replaced by `columnar_compact` are listed by `VACUUM`
and written over by the next stripes, so a steady load of inserts, compactions and
vacuums stops growing the file (`t/062`: 40 rounds, the file of the vacuumed table
stays within 1.5 times its size after ten rounds, the control table that is never
vacuumed grows several times larger).  **How**: the metapage holds, after its fixed
part, a list of up to 400 free extents (first block, number of blocks; its own
checksum; a list that is missing, damaged or not sound reads as empty, which loses
space and never data).  `VACUUM` takes a `SHARE` lock on the table (without waiting,
retried every 50 ms for `vacuum_truncate_lock_timeout`, 5 seconds by default, the way
the truncation takes its lock; given up for this visit when writers do not leave, and
the next vacuum tries again), reads the stripe catalog and makes the list the complement of
the block ranges that some version of a stripe row still needs: every version
whose writer did not abort, except the ones deleted by a transaction that committed before the
oldest transaction any snapshot may still have, which the catalog's own vacuum is
about to remove.  The lock is what makes this safe: no writer is running, so pages
written for a stripe whose catalog row is not there yet cannot be taken for free.  It is
held for the catalog read and one metapage write and is not WAL-logged (a standby does
not see it); readers are not blocked.  A stripe that is written takes the first fit of
the list before it extends the file (`kc_meta_take_extent`: the take is logged before
any page is written, the pages are written over the run, each with a full-page
image, the rest of the extent stays); a crash between the take and the pages loses the
run's place in the list and nothing else (`t/062`: the blocks are free and the next vacuum lists them again).
**On a standby**: the record that replaces the list carries the newest transaction that deleted a
stripe whose blocks are listed for the first time, and its replay cancels the standby queries
that may still hold it (a recovery conflict, as for the removal of a heap tuple), before any
page can be written again; with `hot_standby_feedback` the primary's oldest snapshot
includes the standby's, so it does not list the blocks of a stripe the standby can still see
and the query goes on.  A tail that is free is still given back by the truncation, which
trims the list.  **What it costs**: one catalog read and one metapage image per `VACUUM`,
an extra metapage image per stripe that takes a run; and a `VACUUM` of a table that never has a
quiet moment lists nothing (long writer transactions and continuous load: `DEBUG2` says so),
so the file grows as before until it has one.  The list holds 400 extents: more runs than that
keep the largest, the rest stays free and unlisted until a later vacuum has room.
`kingres.columnar_info(rel).unreferenced_bytes` still counts the blocks the file holds that no
stripe visible now uses, listed or not; what the next stripes will reuse is part of it.  A rewrite
(`VACUUM FULL`, `SET ACCESS METHOD`) starts with an empty list, as with a new file.
`kingres.columnar_check()` reports a list that names a block of a stripe visible now
(`free_extent_overlaps_stripe`), which would be overwritten.  An earlier version of this section
said that reusing the space was left out on purpose (C10-06, C11-06) for the cost of an exclusive
lock at every vacuum; the shared lock above is not that cost, and the design's walk of the stripe
headers is not needed: the stripe catalog already says what is used.

**Operations notes for tables with indexes (C13).**  (1) `columnar_compact` holds `SHARE UPDATE EXCLUSIVE` on the
table, which conflicts with `VACUUM` and with another compaction (it is not a writer's lock): no index vacuum and no
free-space pass runs while a compaction writes its copies and their index entries, and the other way round; readers
and writers go on.  (2) A compaction that is **prepared** (`PREPARE TRANSACTION` after `columnar_compact`) holds that lock
until `COMMIT PREPARED` or `ROLLBACK PREPARED`, so `VACUUM` of the table waits for it, as it would for heap; index
vacuum and the reuse of free space are delayed with it: end prepared transactions.  (3) The rows of the stripe catalog
`kingres.columnar_stripe` show up in the output of `test_decoding` (it has no publication filter) beside the table's own
inserts, as any table's do; a publication never includes the catalog (see "Logical replication"), and a slot of a
custom plugin must filter it out.  (4) A role that may `INSERT` into a table with an index can make it use numbers,
see "What gives the numbers back, and what does not" for how fast and how the owner recovers.

How the stripe rows of a file that goes away are removed.  Correctness never
depends on an event trigger firing; the triggers are only a way to do it earlier.
The access method itself learns that a file goes away: it hooks the drop of a
relation (an object access hook), and every way a columnar file is replaced or
dropped ends in the drop of a relation that holds it: `DROP TABLE`, `DROP
SCHEMA`, `DROP OWNED`, `DROP EXTENSION`, and every rewrite (`VACUUM FULL`, a
rewriting `ALTER TABLE`, a change of access method in either direction,
`REFRESH MATERIALIZED VIEW`), which makes a transient table, swaps the files and
drops the transient table, which then holds the OLD file, whatever access method
the table has become.  The hook notes the identity of that file (read from its
first block) in the memory of the transaction, for the subtransaction that
dropped it (a rollback to a savepoint takes the note back, a release hands it to
the parent).  The stripe rows of the noted files whose relation is gone, or has
another file by then, are deleted by the transaction that dropped them, at the
latest at its pre-commit or pre-prepare, and come back if it rolls back.  A note
of a file that is still in use (a drop that was rolled back, a rewrite that
failed) deletes nothing.  The two event triggers (`DROP`, and `ALTER TABLE`) call
the same cleanup at the end of the command, so that `columnar_check` has nothing
to report inside the transaction either; without them (`event_triggers = off`,
a trigger disabled, single-user mode, a database whose install script is an
earlier one) the rows are removed at the commit instead, which is tested for
every command above (`t/023_rewrite_cleanup.pl`).  The cost is that of the
files the command replaced, the same for an `ALTER TABLE` or a `DROP TABLE` in a
database with ten or five thousand columnar tables, and two commands on
different tables share no row, so neither waits for the other.  A row that
another session deleted meanwhile (`VACUUM`, `columnar_reconcile()`) is already
gone and not an error; one that it is deleting is left to it.  `VACUUM`,
`columnar_check(true)` and `columnar_reconcile()` remove the rows of any file
that is left (a crash, a restore) and read the first block of every columnar
file; leftover rows can never be mistaken for the stripes of a later file,
because a new file draws an identity that no row of the catalog, live or dead,
has.  A database whose install script is an earlier one (it has a
`table_rewrite` trigger and an `ALTER TABLE` trigger that runs the full
reconcile after every `ALTER TABLE`, or lacks the newer functions) works fully:
the cleanup is done by the access method, its old triggers only add the cost of
the full reconcile.  To get the current trigger functions, run the `CREATE
FUNCTION columnar_on_drop()` and `CREATE FUNCTION columnar_check_ddl()`
statements of `kingres_columnar--0.1.sql` as `CREATE OR REPLACE FUNCTION`, as
superuser; the old `table_rewrite` trigger can stay (it only names a file that
the access method names itself, and `columnar_note_rewrite` is still there for
it; tested in `t/023`).  The stripe catalog of an earlier shape is refused
with the statement that repairs it, as before.

What can still leave stripe rows behind is a leak and never an emptied table
(`t/028_orphan_rows.pl`): (1) a callback of another module (loaded before this
one, so that its pre-commit runs after ours) that drops or rewrites a columnar
table at pre-commit with event triggers off: the transaction's own cleanup has
run and the commit cannot delete rows, so the access method keeps the dropped
files (at most 10 000) and the next transaction of the same backend that reaches
its pre-commit deletes the rows of those that are gone, with the same
comparison as always (`columnar_write_paths` counts them as `orphans_cleaned`);
a backend that ends first leaves the rows; (2) a session that did not load the
library (the server runs without `shared_preload_libraries`, the extension
still installed) and drops a columnar table with event triggers off: no hook
runs there, and the rows stay.  Both are found by `columnar_check()`
(`orphan_catalog_rows`, with the number of rows) and removed by
`columnar_check(true)`, `columnar_reconcile()` and `VACUUM`; the removal reads
the identity of every live columnar file with a dirty snapshot of `pg_class`, so
the file of a table that another session is creating, uncommitted, keeps its
rows, and a new file always draws an identity that no row of the catalog, live
or dead, has, so leftover rows are never taken for the stripes of a later file.

`ANALYZE` samples blocks: a page of a stripe stands for the rows at the same
relative position in the stripe. It ignores rows buffered by the running
transaction.

## Backup, restore, upgrade

**From 0.1 to 0.2**: install the new library, then `ALTER EXTENSION kingres_columnar
UPDATE` (`kingres_columnar--0.1--0.2.sql`: one `ALTER TABLE ... ADD COLUMN reserved
boolean NOT NULL DEFAULT false` on the stripe catalog, which stores nothing in
the existing rows).  The tables 0.1 wrote stay readable and writable, before the
update (the library of 0.2 serves the catalog of 0.1) and after it; their files stay
of format version 2 and never reserve row numbers, whatever the setting says, until
a rewrite (`VACUUM FULL`, `ALTER TABLE ... SET ACCESS METHOD`) gives the table a new
file, which is version 3.  A transaction prepared by 0.1 is committed by 0.2.  There
is no way back: the library of 0.1 refuses a file of version 3 by its format
version, and the catalog of 0.2 by its shape.  `t/049_upgrade_data.pl` carries a
data directory written by the code of commit bfda9e8842 (built into a private
installation named by `KC_OLD_INSTALL`) across the update, and `sql/upgrade.sql`
compares an upgraded catalog with a fresh one.  A **base backup taken while writers
hold reservations** restores to a clean state: the reserved rows belong to
transactions that never commit, so nobody sees them and the vacuum of the
catalog takes them away (`t/048`).

`pg_basebackup`, incremental backup, archive recovery and `pg_rewind` work
with the library preloaded. `pg_dump` writes `CREATE TABLE ... USING columnar`
and the rows as `COPY`; the options of a table live in its metapage and are
not in a dump, so a restored table takes the settings of the restoring
session. `pg_upgrade` is not supported in this version: the stripe catalog is
extension data and is not carried over. Files are not portable between
machines of different byte order, and a table written by one major version is
not claimed to be readable by another.

## Converting

To columnar: `ALTER TABLE t SET ACCESS METHOD columnar` (rewrite under
`ACCESS EXCLUSIVE`; the table must have no index, foreign key or `AFTER ROW`
trigger). Back: `ALTER TABLE t SET ACCESS METHOD heap` (also for rows over 8 kB: a
columnar table with a column that heap would toast has an empty TOAST table, so
that the heap table the rewrite creates has one). A partitioned table's
`SET ACCESS METHOD` changes the method of partitions created afterwards. A
rewrite other than `VACUUM FULL` and `SET TABLESPACE` (for example `ALTER
COLUMN TYPE`) gives the new file the default options.

## Isolation, transaction ages, names

All isolation levels work. A columnar table takes part in serializable
snapshot isolation as a heap table does, at the granularity of the relation:

* a scan takes a relation-level predicate lock before it reads the stripe
  catalog;
* a write checks for conflicts with those locks when a row is buffered and
  again when the buffer becomes a stripe (at pre-commit or pre-prepare, which
  PostgreSQL runs before its own serialization check, or when the buffer is
  flushed by size), so a reader whose lock came after the first check is
  found;
* a scan that skips a stripe because its writer had not committed before the
  snapshot reports the dependency reader-before-writer
  (`CheckForSerializableConflictOut`), as a heap scan does for a tuple it skips.
  The id in a stripe row is that of the subtransaction that flushed the stripe
  (a savepoint, an exception block, a flush by size or memory inside one), and
  the serializable machinery knows top-level transactions only, so the id is
  mapped to its top-level transaction (`SubTransGetTopmostTransaction`) first;
  nothing is reported when the id is older than the backend's oldest snapshot
  xmin (as in heap, which also avoids a `pg_subtrans` lookup for an id whose
  entry may be gone), when it is the reader's own transaction, or when the
  writing subtransaction aborted (heap sees such a tuple as dead);
* `columnar_compact` copies rows that readers see already. Its copies take no
  conflict-in check, its scan takes no predicate lock, and the stripes it
  writes carry the `maintenance` flag so that a reader whose snapshot cannot
  see them reports no dependency on the compacting transaction; heap's
  `VACUUM FULL` and `CLUSTER` do the same (they only move predicate locks to
  the relation). The copies go through a buffer of their own that no
  subtransaction hands over or merges, so a stripe is a maintenance stripe
  exactly when compaction wrote it, whatever else the transaction did, and a
  real write of the same transaction keeps all its checks. The stripes it
  writes are the compacting transaction's own, and a transaction sees all of
  its own stripes, so what it copies must be what its snapshot sees: in
  READ COMMITTED every statement has a fresh snapshot and the sources are the
  committed stripes; in REPEATABLE READ and SERIALIZABLE only the stripes the
  transaction's own snapshot sees are copied and replaced, a stripe committed
  after it is left alone (it stays invisible to this transaction, and visible
  to everyone with a later snapshot), and a source another transaction
  replaced since fails with a serialization failure. Other snapshots see the
  sources until the compaction commits and the copies after, never both and
  never a different set of rows. A `VACUUM FULL` has no earlier snapshot
  (it cannot run in a transaction block) and a rewriting `ALTER TABLE` reads
  what is committed and writes its rows as its own, as it does for heap;
* `columnar_info`, `columnar_stripes` and the other inspection functions read
  the stripe catalog and take no part in serializable isolation (no dependency
  is created or reported, no predicate lock is taken): a decision based on
  their result has no write-skew protection, as for any catalog query.

The set of transactions that commit is therefore one that heap also allows
(`t/009_serializable.pl` compares both, with write skew in both orders, the
read-only anomaly, buffered and size-flushed rows, savepoints, `PREPARE
TRANSACTION`, and mixed columnar and heap tables). Because the granularity is
the table, a reader of a columnar table conflicts with every writer of it, as a
heap sequential scan does. The stripe catalog is read without predicate locks,
so transactions on unrelated columnar tables do not abort each other.

A columnar table has a real `relfrozenxid`: the oldest transaction id its file
still holds (the creating transaction of the metapage; see "On-disk format").
VACUUM, including autovacuum's anti-wraparound vacuum, freezes it and advances
`relfrozenxid`, so `age(relfrozenxid)` is meaningful and the table takes part
in the protection against transaction id wraparound as a heap table does. The
stripe catalog is an ordinary heap table and is frozen as usual.  Autovacuum
acts on the table, as on a heap table, only once `age(relfrozenxid)` is past
`autovacuum_freeze_max_age` (or an insert or analyze threshold is reached): a
table at an age just under it is left alone.  With `log_autovacuum_min_duration`
set, a visit of autovacuum to a columnar table is logged in the words heap
uses (`automatic [aggressive] vacuum [to prevent wraparound] of table "d.s.t":
columnar: N rows, B blocks, relfrozenxid X, creation id frozen|kept`, where X is the
`relfrozenxid` that `pg_class` holds after the visit: `vac_update_relstats` only
moves it forward, so it can be later than the id the vacuum passed in, which is what
the line used to print; C10-04), and
`pg_stat_all_tables` reports `autovacuum_count` and `last_autovacuum` for it.
Before that line existed the log showed a visit of autovacuum to a columnar table
only as an `automatic analyze` line, and its absence was no sign of an absent visit.

If the role that creates tables is named `kingres`, the default search path
(`"$user", public`) puts unqualified tables into the schema `kingres`, which
belongs to the extension (and is shared with the other KingresDB extensions).
Set `search_path` explicitly, or create the tables in another schema.

## Limits

Append-only, B-tree indexes only (unique ones too), no TOAST (values up to the 1 GB limit of one value are
stored inline), at most about 8.8 x 10^12 rows per table, 150000-row stripes
mean small transactions make small stripes, serial and parallel scans of the table (no bitmap, index-only or parallel index scans), no vectorised
execution, skipping is not in the cost estimate, logical decoding covers inserts only (there is
no update or delete) and doubles the log of a load at `wal_level = logical`, and a hostile or damaged file can
give a clean error but not always the right data.

Memory and costs worth knowing:

* Memory of the write buffers.  Rows of a transaction are buffered until the
  stripe is full, the buffers are over `kingres_columnar.write_buffer_memory`
  (A) or the transaction ends.  What counts is memory, not payload: the space
  of every by-reference value (its chunk in the allocator, header and rounding
  included), the arrays of row slots (their capacity), and 2 kB for every
  buffer that exists (its context and header, which an empty buffer still
  holds), so the number of buffers, whether levels of subtransaction, relations,
  partitions or buffers that cursors keep apart, is part of the bound and not
  beside it.  One accounting covers the whole backend.  **The bound**, which
  follows from the setting alone: with A = `write_buffer_memory` and R the bytes
  of the row being written,
  * the buffers hold at most 2A + R after every write (the ceiling, twice the
    allowance, enforced where a row is added: a write that finds more than 2A
    held, after writing everything that may be written, is refused with the
    error "the write buffers of columnar tables hold N bytes, more than the
    limit of M bytes", whatever the number of subtransaction levels, cursors,
    relations and partitions) and at most A when everything they hold may be
    written;
  * for the length of the commit of a subtransaction its rows exist twice (the
    copy in the parent's buffer and the original), so the buffers hold at most
    2 * (2A + R) between the two phases of the hand-over; a hand-over that would
    leave more than 2A in the parent is refused (the subtransaction aborts
    cleanly);
  * the memory the backend takes is at most 2 * (2A + R) for the buffers (the
    allocator's blocks, at most 1 MB each, add up to as much as the rows),
    + (2A + R) for the rows of a committing subtransaction, + S for the stripe
    being built (not counted by the accounting, but part of this bound) + what
    the backend needs to run the statements (12 MB in the tests).  S is the
    raw chunk the encoder builds (the rows of one chunk group: at most 32 MB,
    or one value if that is larger), its compressed output, and the compressed
    stripe so far, which is at most as large as the buffers it was made from:
    **S <= 2 * (32 MB + V) + 2A + V**, with V the largest single value of the
    statement (a text or bytea of up to 1 GB).  For a large value that is the
    largest term: a value of V bytes is held by the executor (V), copied into
    the buffer (V), copied into the raw chunk (V) and compressed (its output
    and the copy of it in the stripe: up to 2 V more), so one value of V bytes
    takes **at most 4 V more than the same insert into heap** (measured for a
    64 MB text: 5.0 V in all against heap's 2.9 V; a 100 MB value 4.8 V against
    2.9 V, a 400 MB one 4.8 V against 2.9 V, so a value at the 1 GB limit of
    PostgreSQL takes about 5 GB).  An earlier version of this paragraph said
    3 R plus an S that the caller chooses, which is not a bound for such a
    value; the number is pinned by a measurement with a 64 MB value in `t/025`.
    Measured as the
    peak size of the backend (`VmPeak`), not by a counter of the extension
    (`t/025_memory_bound.pl`): 60 nested levels of 3.6 MB under A = 8 MB grew
    the backend by 19 MB (206 MB held before the limit existed), 40 cursors
    22 MB (114 MB before), 1 500 partitions and 1 500 tables of one or five rows
    under A = 1 MB by 1 to 2 MB more than the same load into heap tables, rows
    of 5 MB under 8 MB by 40 MB (a stripe being built);
  * what is written, and when.  The buffers of the running (sub)transaction are
    written, the largest first, as soon as the buffers together are over A, so
    when everything may be written they hold at most A and the row being
    written; an empty buffer left over is freed with the state of its relation
    (the next row makes them again).  Rows of enclosing subtransactions
    (savepoints, exception blocks) cannot be written by an inner one: a stripe
    carries the id of the subtransaction that writes it, and the rows would be
    lost with an abort of the inner one (the earlier rounds' hand-over writes
    the parent's rows from the committing child only because the rows stay in the
    buffer until the child's id is the parent's).  A write that is refused at
    the ceiling is therefore the answer where writing cannot help, and it is
    exact: the transaction can release or commit its levels, close its cursors,
    or raise the setting.  A level of subtransaction that wrote a row costs
    the 2 kB of its buffer and the room for its first 16 rows (2 320 bytes for
    a table of one int column), so at the smallest setting (A = 1 MB, a ceiling
    of 2 MB) 904 nested savepoints of one row each reach the ceiling where heap
    does not, and at the default of 128 MB about 115 000 do; the charge is
    pinned by a measurement of the accounting (`bytes_held`) and the number of
    levels by a run in `t/025`.  A buffer of less than `max(chunk_row_count, 1000)`
    rows and half of A is written only while the flushable buffers together
    are half of A, so that a few rows are never made a stripe because outer
    levels hold the memory;
  * a cursor on the table holds the buffers that had rows when it began (they
    are read by position and are neither written nor merged under it, and take
    no more rows); rows written after that go to a buffer of their own that is
    written under A.  The held rows count against the ceiling, which bounds
    the number of cursors that can hold rows at once by it;
  * for the length of the commit of a subtransaction its buffers of less than
    half of A exist twice, as above; a bigger one is written by the
    subtransaction itself instead of being copied when the copy would take
    the memory over A.
  The stripes of a table depend on the data and the options, not on the grouping
  into subtransactions, except where the bound forces a flush: when the buffers
  are over A and the running subtransaction may write them (the largest first),
  when a hand-over leaves more than A, and (with a stripe of one row or more,
  never more than the stripe size) for rows written after the commit-time
  flush, below.

* Rows that reach the access method after its own commit-time flush.  The access
  method writes every buffer in its pre-commit (and pre-prepare) callback.
  PostgreSQL runs callbacks of other modules that were registered earlier (a
  module listed before this one in `shared_preload_libraries`) after it, and
  they may run SQL.  Everything that can write after the flush and before the
  commit record is: such callbacks (`XACT_EVENT_PRE_COMMIT`, `PRE_PREPARE`;
  in a parallel worker a write is refused); the callbacks of a committing
  subtransaction that run after the first phase of its hand-over (see below);
  and nothing else in `CommitTransaction` and `PrepareTransaction`: deferred
  triggers and portals run before the callbacks, `PreCommit_on_commit_actions`
  acts only on temporary tables (a columnar table cannot be temporary or
  unlogged, so `ON COMMIT DELETE ROWS` and `ON COMMIT DROP` never reach one),
  and the notify, large object and serializable steps write no tables.  A row
  written from then on is written to a stripe at once (write-through), in the
  caller's memory and outside the buffers, so it is committed with its
  transaction whatever the order of the libraries; a `COPY` or a multi-row
  insert is one stripe per batch, and a single row (an `INSERT`, or each row of
  an `INSERT ... SELECT`) is a stripe of one row.  That is correct and rare, and
  it has a price: 100 000 rows written one at a time by a callback at
  pre-commit make 100 000 stripes and 819 200 000 bytes on disk (8 192 bytes, one
  page, for each row of 4 bytes), and a full scan reads them far more slowly than
  the same rows in one stripe (`t/030_late_stripes.pl` pins the shape with 5 000
  rows).  `VACUUM` joins them when at least 128 are
  smaller than a sixteenth of the stripe (since C13-05; an earlier version of this paragraph said it does not, and `t/030` runs
  with the merge turned off to pin the shape); `columnar_compact` does at once (the 100 000 rows
  of that example become one stripe of 245 760 bytes), but the pages of the old
  stripes stay in the file until `VACUUM FULL` returns them.  An operator whose
  module writes late runs `columnar_compact` and, to return the space, `VACUUM
  FULL`; the module's own remedy is to write its rows with `COPY` or one
  multi-row statement.  A delete or an update is refused with an error, as
  always (the table is append-only), where an error is allowed.  Tested with the
  probe module and a heap twin, both load orders, on both majors
  (`t/024_commit_window.pl`).

* Writes while a transaction is ending (C7-01, C8-01, C8-02, C8-03, C9-01, C9-03, C9-05).  The
  invariant: **at every event of the transaction and subtransaction callbacks,
  and at every other moment at which PostgreSQL forbids an error, a call that
  reaches the access method is either done correctly or refused in a way that
  the server survives and that the caller sees, and the access method never
  raises an error there; and a transaction never commits having lost a call
  that it could still have failed for, and what a warning says of the
  transaction is true at the moment it is given.**  PostgreSQL escalates an ERROR at
  those moments: in a critical section, at `XACT_EVENT_COMMIT` (after the
  commit record: "cannot abort transaction ... it was already committed", a
  PANIC), at `XACT_EVENT_PREPARE` (a failed assertion in `procarray.c`), at
  `XACT_EVENT_ABORT` and `SUBXACT_EVENT_ABORT_SUB` ("ERRORDATA_STACK_SIZE
  exceeded", a PANIC), at `SUBXACT_EVENT_COMMIT_SUB` (the subtransaction is
  aborted after it committed) and in the phases of the release of a resource
  owner.  Version 0.1 as first written raised an error there, which stopped the
  server; it now warns and does nothing.  The access method cannot do the work
  instead: writing a stripe or changing the catalog reads the catalogs and
  allocates memory, which PostgreSQL forbids at the same moments (that is the
  assertion heap trips in a build with assertions), and a failure in it, an
  error, would be the PANIC again.

  **What a refused call means depends on what is still possible**, and there
  are two cases, told apart by what PostgreSQL still allows at the moment:

  * *The fate of the transaction is decided*: `XACT_EVENT_COMMIT`,
    `XACT_EVENT_PREPARE`, `XACT_EVENT_ABORT`, `SUBXACT_EVENT_ABORT_SUB`, the
    three phases of the release of a resource owner, the exit of the process.
    The commit record exists, or the transaction is aborting; nothing better
    than a WARNING exists.  In an aborting transaction the data is lost anyway,
    which is what the warning says; after the commit record a call made by
    another module at that moment is lost, and nothing else of the transaction
    is touched.  A prepared transaction is intact.
  * *The top-level transaction is still alive*: `SUBXACT_EVENT_COMMIT_SUB` (the
    top-level transaction has not committed, and can still abort cleanly) and a
    critical section entered by a transaction that is in progress **before the
    extension's own pre-commit (or pre-prepare) callback has run**.  The call is
    refused with the WARNING, and the refusal is remembered (a counter and a
    name, in static memory: nothing is allocated where an allocation is
    forbidden).  The transaction then **fails with an error at the next moment
    an error is allowed**: at the pre-commit of the subtransaction that owns
    what was lost (a refusal made while a subtransaction commits belongs to its
    parent, so a `RELEASE` of the parent fails, and so does the `COMMIT` that
    releases it), at the pre-commit or pre-prepare of the top-level transaction
    (`PREPARE TRANSACTION` fails and prepares nothing), and at the next write to
    a columnar table.  The error is "a call on columnar table "t" was refused
    earlier in this transaction, so it cannot commit".  A refusal belongs to a
    subtransaction, and goes with it: if that subtransaction is rolled back
    (`ROLLBACK TO`), the loss is gone and the transaction commits.  The loss is
    remembered whether or not the warning is sent or logged
    (`client_min_messages`, `log_min_messages` change nothing), and an
    `emit_log_hook` of another module that raises an error for the warning
    makes it an error of the subtransaction that was committing, which aborts,
    before anything is remembered (a hook that raises at a moment where the
    fate is decided or in a critical section makes any WARNING of the server a
    PANIC, for heap as for columnar: not the access method's doing).  A client
    that is gone when the warning is sent changes nothing either.

  * *A critical section entered after the extension's own pre-commit callback*
    (C9-01): a callback of another module that runs after the extension's own
    at `XACT_EVENT_PRE_COMMIT` or `XACT_EVENT_PRE_PREPARE` (the module is
    loaded before the extension in `shared_preload_libraries`, which makes its
    callbacks run after) and enters a critical section there.  This moment is
    **decided**, not alive.  PostgreSQL still allows an error between the
    pre-commit callbacks and the commit record (the serialization failure check
    is one), but nothing there is a callback that an extension can register
    for the purpose: `CommitTransaction` and `PrepareTransaction` call the
    pre-commit callbacks, then run PostgreSQL's own closing work (the
    deferred-trigger manager, `ON COMMIT` actions, pending file syncs, large
    objects, `NOTIFY`, the serializable check), then write the commit or
    prepare record, on PostgreSQL 18 and 19 alike (read in both sources); the
    only extension code that can run in that stretch is the object access hook,
    for an `ON COMMIT DROP` table, which a transaction need not have, and the
    extension's own callback has run by then.  A refusal cannot be remembered
    and raised later, so the call is refused with the warning "Nothing was
    done; if the transaction commits (or prepares) it does so without this
    call", and the transaction commits (or prepares) without it.  With the
    module loaded after the extension (its callbacks run before) the same
    critical section is alive, and the transaction fails at the extension's
    pre-commit.  **Inside a subtransaction that such a callback began** (C10-03;
    a callback of a later module can: a PL/pgSQL exception block does) the
    critical section is alive again, in both orders: the transaction callbacks of
    this extension for the top-level transaction do not run again, but its
    callbacks for the subtransaction do (its start, its pre-commit, its commit),
    so a refusal there is remembered for that subtransaction and its pre-commit
    raises the error, as for any subtransaction; the error ends the callback of
    the other module, and the transaction aborts (`PREPARE TRANSACTION` prepares
    nothing).  The rule is the nesting level: a critical section at level 1 after
    the extension's own pre-commit is decided, at a deeper level it is alive.  A
    call that may be skipped (a vacuum) is not made worse by it: a warning, and
    the transaction commits.  A module that must not lose a call writes at
    `XACT_EVENT_PRE_COMMIT` or `PRE_PREPARE` outside a critical section, where
    the call is carried out (written at once, see above).

  What heap does at the same moments was found with a module that writes a row
  with `table_tuple_insert` into a table it holds open at the event a setting
  names (`t/kcevents/kcevents.c`), run on a heap table against a server built
  with assertions and one built without (PostgreSQL 18.7 and 19, both load
  orders); the access method's column is `t/027_ending_writes.pl`, which
  compares with a heap twin only at the moments where heap writes without
  stopping the server:

  | Moment | heap, server with assertions | heap, production build | columnar before | columnar now |
  |---|---|---|---|---|
  | `XACT_EVENT_PRE_COMMIT`, `PRE_PREPARE` | writes, durable | writes, durable | writes at once (write-through) | the same |
  | `SUBXACT_EVENT_START_SUB`, `PRE_COMMIT_SUB` | writes, durable | writes, durable | writes (a buffer, or write-through in the window) | the same |
  | `XACT_EVENT_COMMIT` | stops: `Assert(IsTransactionState())` in `relcache.c` | writes; the row is visible at once (with the committing transaction's id as its `xmin`) and **is not durable**: its WAL record is flushed by nothing until the next commit of any session, and an immediate stop loses it, two seconds later as well | PANIC | warning, nothing written; the commit stands (decided) |
  | `XACT_EVENT_PREPARE` | stops (the same assertion) | writes; `COMMIT PREPARED` commits the row, durable | assertion failure in `procarray.c` | warning, nothing written; the prepared transaction is intact (decided) |
  | `XACT_EVENT_ABORT` | stops (the same assertion) | writes a row of the aborted transaction, which nobody sees | PANIC (error stack) | warning (decided; the row would vanish anyway) |
  | `SUBXACT_EVENT_COMMIT_SUB` | stops (the same assertion) | writes; the row commits with the parent, durable | the subtransaction aborted | warning, and the transaction fails with an error at its next pre-commit (alive) |
  | `SUBXACT_EVENT_ABORT_SUB` | stops (the same assertion) | writes a row of the aborted subtransaction, which nobody sees | PANIC (error stack) | warning (decided; as for an abort) |
  | resource owner release, the three phases | ERROR "ResourceOwnerEnlarge called after release started", escalated to a PANIC | the same | PANIC | warning (decided) |
  | inside a critical section | stops (`allowInCritSection` assertion in `mcxt.c`) | writes | writes when no allocation is needed, else the same assertion | warning, and the transaction fails with an error at its next pre-commit (alive), if the critical section was entered before the extension's own pre-commit callback ran, or inside a subtransaction that a later module's callback began after it (C10-03: the subtransaction's pre-commit raises); decided (warning: "if the transaction commits (or prepares) it does so without this call") if it was entered after it at the top level, at `XACT_EVENT_PRE_COMMIT` or `PRE_PREPARE` by a callback that runs later, and if it is entered at one of the moments above |
  | `proc_exit` callbacks | no transaction and no relation to write to; nothing of the access method runs there | | | |

  The server survives at every moment, which heap does not: with assertions it
  stops at every moment of the table except the first two rows, and in a
  production build at the three phases of the release of a resource owner (the
  same PANIC), and its row at `XACT_EVENT_COMMIT` can vanish at the next crash.
  Where heap writes in a production build (the commit, the prepare, the commit
  of a subtransaction, a critical section) the access method does not keep the
  row, for the reason above; "columnar loses a row that heap keeps" is less
  than the comparison: heap shows a row that the next crash may take back
  (the commit), and the refusal is the least bad of a refusal and a stop of the
  server.  Keeping the row for a later safe moment (the next transaction of the
  backend) was considered and rejected: the row would appear in a snapshot of
  a transaction that did not write it, and for a prepared transaction that is
  rolled back later it would be a row that never committed.  Where the top-level
  transaction is still alive the row is not kept either, but the transaction
  does not commit without it: it fails.

  **Every entry point of the access method and of the extension has a decision**,
  one helper makes it (`kc_entry_refused` in `kc_write.c`: "may this entry point
  touch storage or the catalog now"), the table is data (`kc_am_decisions` in
  `kc_am.c`), and a test walks the members of the routine and fails for one that
  has no decision (`t/031_entry_points.pl`, the module `t/kcevents`, which calls
  each entry point at each moment above, in both load orders, on both majors):

  | Entry point | Decision | At a moment where an error is forbidden |
  |---|---|---|
  | `tuple_insert`, `multi_insert`, `tuple_insert_speculative`, `tuple_complete_speculative` | guarded | warning, nothing written (`insert`, `multi insert`, ...) |
  | `tuple_delete`, `tuple_update`, `tuple_lock` | guarded | warning; `TM_Invisible` is returned |
  | `finish_bulk_insert` | guarded | the flush it would make is left to the pre-commit; no warning (nothing is lost) |
  | `relation_nontransactional_truncate` (C8-01, C10-05) | guarded; **only for a relation made, or given a new file, in the current subtransaction (C11-04)** | warning, nothing changed: not the half of a truncate (a file shortened with the stripe rows left, or the other way round).  Where an error is allowed, for a relation that was not created, or given a new file, in the current subtransaction (neither `rd_createSubid` nor `rd_newRelfilelocatorSubid` is the current subtransaction id, as `ExecuteTruncateGuts` compares them; a table of an outer subtransaction is refused: an abort of the child would not remove its file) it is an error, `cannot truncate columnar table "t" in place: it was not created or given a new file in the current (sub)transaction`: PostgreSQL itself calls it only for such a relation (`TRUNCATE` of a table of the transaction, and a temporary table, which a columnar table cannot be), and it is the only case where an abort removes the file anyway; for any other table the cut file and the reset metapage are not undone by an abort while the deleted catalog rows come back, and the table cannot be read.  For a relation of the transaction it deletes the catalog rows, shortens the file and resets the metapage, in that order: an error in the first step is undone with the transaction and leaves a readable table (`t/031`, `t/033`, `t/034`). |
  | `relation_set_new_filelocator`, `relation_copy_data`, `relation_copy_for_cluster` | guarded | warning; no file is made, nothing copied, the returned ids name nothing |
  | `relation_vacuum` | guarded | warning, skipped (the next vacuum does the same work; no failure of the transaction) |
  | `index_fetch_begin`, `index_fetch_tuple` | guarded | the fetch of a B-tree index scan (B-tree indexes exist since 0.2); where an error is allowed they work; here a warning, and `index_fetch_tuple` answers "nothing" |
  | `index_delete_tuples` | guarded | bottom-up deletion answers none and `VACUUM` removes the entries; a warning |
  | `index_build_range_scan` | guarded | reads the rows for the entries of a new index (`CREATE INDEX`, `REINDEX`, and the read of `amcheck` `heapallindexed`); a warning here. `CREATE INDEX CONCURRENTLY` and `REINDEX CONCURRENTLY` are refused here with an error where one is allowed (`kc_am.c`: "CREATE INDEX CONCURRENTLY and REINDEX CONCURRENTLY are not supported on columnar tables") |
  | `index_validate_scan` | guarded | the second pass of `CREATE INDEX CONCURRENTLY`, which the build above already refuses, so it is a second guard: an error ("CREATE INDEX CONCURRENTLY is not supported on columnar tables"), or a warning here |
  | `tuple_fetch_row_version`, `tuple_tid_valid` | guarded | fetching a row by its TID and a TID scan are refused: an error, or a warning here |
  | `scan_sample_next_block`, `scan_sample_next_tuple` | guarded | `TABLESAMPLE` is refused: an error, or a warning here |
  | `scan_begin`, `scan_getnextslot`, `scan_rescan`, `scan_end`, `scan_analyze_next_block`, `scan_analyze_next_tuple`, `relation_size`, `relation_estimate_size` | reads | work wherever PostgreSQL lets a heap table be read (the relation cache asserts a transaction in progress, and the buffer pin of any table access method fails in a release phase of a resource owner: PostgreSQL's rule, the same for heap) |
  | `slot_callbacks`, `parallelscan_*`, `index_fetch_reset`, `index_fetch_end`, `tuple_get_latest_tid`, `tuple_satisfies_snapshot`, `relation_needs_toast_table`, `relation_toast_am` | nostorage | nothing to do |
  | the object access hook (a drop of a relation) | guarded | warning, the file is not noted (its leftover rows are removed by `VACUUM` and `columnar_check(true)`) |
  | `columnar_compact`, `columnar_set_options` | guarded | warning, nothing done |
  | `columnar_reconcile`, `columnar_finish_rewrites`, `columnar_note_rewrite` | guarded | warning, skipped (the commit or the next call does the same work) |
  | `columnar_check(false)`, `columnar_check(true)`, `columnar_check_clean()` (C9-03) | guarded: *needs a subtransaction* | warning, nothing checked, and no row (`columnar_check_clean()`: `false`), at every moment at which PostgreSQL cannot begin a subtransaction (`BeginInternalSubTransaction` ends the session with a FATAL for a subtransaction that is starting, committing or aborting, and the call allocates): no transaction in progress (`XACT_EVENT_COMMIT`, `PREPARE`, `ABORT`, `SUBXACT_EVENT_COMMIT_SUB`, `ABORT_SUB`, the release phases, the exit of the process), a critical section, and a subtransaction between its pre-commit and its end as the extension's callback saw it.  Served at `XACT_EVENT_PRE_COMMIT` and `PRE_PREPARE`.  Two moments the extension cannot see: `SUBXACT_EVENT_START_SUB`, and `SUBXACT_EVENT_PRE_COMMIT_SUB` when the other module's callback runs before the extension's; there the server's own FATAL applies, as it does to a PL/pgSQL exception block on a heap table.  A refused call is not an answer that the tables are clean; no statement of a session is ever in these states |
  | the other SQL functions (`columnar_options`, `_info`, `_stripes`, `_identity`, `_creating_xid`, `_table_xid`, `_write_paths`, `_handler`) | reads | work wherever PostgreSQL lets a heap table be read, with one rule of their own (C9-03): where an error is forbidden the table's `ACCESS SHARE` lock is asked for without waiting (a wait could end in a deadlock error, a PANIC there); a table locked exclusively by another transaction gets a warning and a null (or empty) answer.  A table the transaction holds a lock on is never refused |
  | the transaction and subtransaction callbacks of the extension | they are the moments | what they do at each (flush, hand-over, clean-up, remembering the refusal) was designed for it; the memory the commit event needs is static (below) |
  | the planner hook and the executor methods of the Columnar Scan, the event trigger functions, the redo of the resource manager | not reachable | they need a transaction in progress (planning, executing, DDL) or recovery, and none of them runs where an error is forbidden |

  **Reads at the moments where an error is forbidden are tested on builds without
  assertions only** (C10-07).  A server built with assertions cannot run them, and
  not because of this extension: at `XACT_EVENT_COMMIT`, `PREPARE`, `ABORT`, at the
  commit and abort of a subtransaction and in the three phases of the release of a
  resource owner no transaction is in progress, and the first lookup of a relation
  or a catalog row stops the server (`AssertCouldGetRelation`, `relcache.c` and
  `catcache.c`, `Assert(IsTransactionState())`, which fails for heap just the same);
  in a critical section a transaction is in progress, but a read allocates and
  `palloc` asserts that it is not in one (`mcxt.c`).  So for these cells the
  assertion makes the run impossible on such a build, with no exception.  They are,
  for each order of loading the test module (74 cells each, 148 in all): the scan of a
  table (`scan_begin`, `scan_getnextslot`, `scan_rescan`, `scan_end`) at each of the
  eleven moments of the table above (11); the seven SQL functions that read
  (`columnar_options`, `_info`, `_stripes`, `_identity`, `_creating_xid`,
  `_table_xid`, `_write_paths`) at the eight moments that are not a release phase
  (7 x 8 = 56); `size` and `estimate` (the length of the file, which allocates in the
  smgr) in a critical section, three moments (2 x 3 = 6); and `columnar_stripes` with
  a table that another session holds exclusively, which is called through the
  function manager (1).  `t/031` skips each of them with its reason and prints how
  many it skipped, for each order of loading, at the end of its output.  They are run
  on a server built without assertions, where the function manager and the relation
  cache accept them: on production builds of PostgreSQL 18.7 and 19 `t/031` ran
  all of them but three cells for each order of loading (6 in all), the scan of a table
  in the three phases of the release of a resource owner, which PostgreSQL itself
  forbids there (the buffer pin needs the resource owner that is being released, for
  heap as well); those are tested nowhere, and `t/031` says so in its output.  `size`
  and `estimate` outside a critical section run on both kinds of build.

  A caller sees, at a moment where the fate is decided, the warning
  `<what> refused on columnar table "t": the transaction is not in progress`
  (`inside a critical section`), with a detail that says what was not done and
  what that means for the transaction: "if it commits or prepares it does so
  without this call" (decided), "if the transaction commits (or prepares) it
  does so without this call" (a critical section after the extension's own
  pre-commit), "skipping this call loses nothing of the transaction" (a call
  that may be skipped); where the transaction is alive (`<what> refused on
  columnar table "t": at the commit of a subtransaction`, or inside a critical
  section before the extension's pre-commit) the same warning, with the detail
  that the transaction fails at its next pre-commit, and later the error, whose
  detail says that code of another module called the table at a moment where
  the server forbids an error.  The text of the warning is true in every cell of
  the table above (`t/031_entry_points.pl` checks the primary message and the
  class of the detail in each).  `columnar_write_paths()`
  counts the refusals as `refused_ending`, those made while the transaction was
  alive as `refused_live`, the errors raised for them as `lost_raised`, and
  reports as `orphans_kept` the files the commit of the backend's last
  transaction noted for the next one (not a counter).  Nothing on the refusal
  path allocates: the counters and the name of the first refused call are static,
  the message is formatted by the logging code in its own memory
  (`ErrorContext`, which PostgreSQL allows in a critical section), and noting
  the files that another module dropped after the clean-up (below) is a copy
  into a static array of at most 10000 entries (a developer setting,
  `kingres_columnar.orphan_notes_max`, lowers it for tests), done inside a
  critical section in a build with assertions so that an allocation there
  traps.  The module that finds a row missing writes at the wrong moment:
  write at `XACT_EVENT_PRE_COMMIT` or `PRE_PREPARE`, or in a transaction of its
  own.  Tested for every moment and entry point, both load orders, both
  majors, with savepoints nested and released, `PREPARE TRANSACTION`, the
  message settings, a log hook that raises and a client that is gone
  (`t/027_ending_writes.pl`, `t/031_entry_points.pl`).

* The rows of a committing subtransaction are handed to its parent in two
  phases (see `kc_write.c`): everything that can fail (memory, the flush) is
  done for every relation in `SUBXACT_EVENT_PRE_COMMIT_SUB`, while the
  subtransaction can still abort cleanly, and nothing is published;
  `SUBXACT_EVENT_COMMIT_SUB` only assigns counts and pointers.  An error in any
  relation therefore leaves the rows of none of them in the parent (PostgreSQL
  aborts a subtransaction whose commit callback throws, and runs its abort
  callbacks, but only the first phase may fail).  When the parent's buffer, with
  the child's rows, holds a stripe or more, or the memory allowance is used up,
  the commit of the child writes it as stripes of the table's size (under the
  child's transaction id, which is the parent's when it commits; the rows stay
  in the buffer until then, so an abort loses nothing), and keeps a tail shorter
  than a stripe.  The number and size of the stripes of a table depend on the
  data and the options, not on the grouping into subtransactions.  Between the
  phases the stripe and the buffer both hold the rows that went into it, so a
  reader there sees only the part of each buffer that is not in a stripe (one
  definition of the visible part of a buffer, used by every reader); PostgreSQL
  itself runs nothing between the phases, but a callback of another extension
  can: a read there sees each row once, a write to any table (the one whose
  rows are being handed over or another) is written to a stripe at once, as
  above, and no buffer is touched: from the moment a buffer is prepared until
  it is published or dropped, nothing flushes, frees, moves or adds to it (no
  chooser of a victim for the memory limit, no flush of a relation, no join of
  buffers sees it as flushable), which the production build keeps as well as the
  one with assertions (`t/022_handover_window.pl`, run on a server built without
  assertions as well: a write to other tables under memory pressure in the
  window gave 7000 rows for 4000 before); a truncate of a table whose rows are
  being handed over, that no SQL can reach, is refused with an error; and a cursor opened there cannot go on after the phases are done and says so,
  rather than read other rows than it began with.  Whatever flush comes next
  cuts a buffer into stripes of at most the table's stripe size (and never above
  10000000 rows, `KC_MAX_STRIPE_ROWS`, the one definition the writer, the option
  and `columnar_check` share), keeping a short tail buffered when more rows are
  coming.  The reader accepts exactly what the writer can write, 10000000 rows a
  stripe, and refuses a larger row count in a catalog row before it allocates
  anything from it (no build since the limit
  exists writes more); `columnar_check` reports a stripe above a lowered
  `max_stripe_rows` as `stripe_over_row_limit`: it is readable, and `VACUUM
  FULL` writes it again within the limit.  A scan holds the buffers it reads
  still; one abandoned by an error is never ended, so its hold is remembered in
  the resource owner that was current when it began (a portal's, or the
  (sub)transaction's) and released when the error is cleaned up: no error leaves
  a table unable to flush by size.
* The stripe catalog must have the columns this build uses.  One made by the
  build before the column `maintenance` existed (13 columns) is refused, on
  every read, insert, `COPY`, `VACUUM` and rewrite, with the statement that
  repairs it and loses nothing: `ALTER TABLE kingres.columnar_stripe ADD COLUMN
  maintenance boolean NOT NULL DEFAULT false` (as the owner of the extension),
  after which every table reads at once; `columnar_check` reports the tables as
  `unreadable` with the same reason.  The error never tells anyone to drop and
  create the extension, which drops the tables with their data (`t/023`
  builds the earlier shape and runs the statement).  A catalog of any other
  shape is refused with a hint to restore it from a backup.
* A chunk read allocates the size its footer records, bounded by what a chunk
  group can hold (32 MB plus one value, at most 1 GB); the footer arrays are
  sized from the footer's length, which must fit its group count.
* The identity of a file is 63 random bits, redrawn when the catalog already
  holds it. `DROP TABLE`, `VACUUM` and `columnar_check` remove catalog rows of
  files no relation uses any more, which reads the first block of every
  columnar relation file of the database (linear in their number). A table access method has no callback at drop and PostgreSQL
  keeps no identity in `pg_class` for it, so there is nothing to look up
  instead; a database with very many columnar partitions pays this on every
  such command.  A rewrite, `VACUUM FULL`, `TRUNCATE` and `ALTER TABLE` do not:
  they know the file they replaced (see "VACUUM and space").
* A `jsonb` value in a chunk written on purpose with valid checksums can still
  crash the server (the structure of a jsonb container is not validated, as it
  is not for heap); the checksums cover damage, not malice.

## Security

The library is preloaded, so its code runs in every database of the server, and
it finds the stripe catalog by name in the schema of the extension. The
extension therefore does not trust a schema that another role can write to, nor
a catalog that is not its own:

- **The schema.** Every install script and every upgrade script refuses a schema
  `kingres` that exists already when a role that is not a superuser owns it, or
  when it grants `CREATE` to such a role or to `PUBLIC` (`insufficient_privilege`,
  `42501`, with a hint). Tests: `t/071_schema_spoof.pl` and
  `t/073_old_script_schema_check.pl`.
- **The stripe catalog.** The library uses `kingres.columnar_stripe` only when the
  catalog table and its primary key index are members of the installed extension,
  have the shape this build needs, and the schema is owned by a superuser and
  grants `CREATE` to no role that is not one (`kc_catalog.c`). In a database
  without the extension, or with a look-alike relation, an attempt to make or write
  a columnar table is refused with an error that says why, and nothing is written
  to the look-alike. Test: `t/072_catalog_trust.pl`.
- **Functions.** `EXECUTE` is revoked from `PUBLIC` on every function except the
  read-only inspection functions that need `SELECT` on the table (see
  "Functions"). The functions that are `SECURITY DEFINER` carry a fixed
  `search_path`.

## Building and testing

Out of tree against an installation of PostgreSQL 18 or 19 (`pg_config` on the
path):

```
make USE_PGXS=1 && make USE_PGXS=1 install
make USE_PGXS=1 installcheck       # needs a server with the library preloaded
```

or `kingres/dev/ext.sh kingres_columnar` for one installation and
`KINGRES_PG18=<install> KINGRES_PG19=<install> kingres/dev/ext-majors.sh
kingres_columnar` for both majors (see
[`kingres/docs/dev/testing.md`](../../kingres/docs/dev/testing.md)). The tests are
seventeen `pg_regress` files (`sql/`, `expected/`; the list is `REGRESS` in the
`Makefile`) and seventy-four TAP files in `t/` (`t/067` to `t/073` cover the merge
of small stripes by `VACUUM`, the give-back of space, and the two security tests
above; `t/074` checks the resource manager id): crash recovery,
standby and `pg_rewind`, backup/restore/dump, concurrent writers, corruption,
a randomised comparison against heap (`KC_COMPARE_SEED`, `KC_COMPARE_STEPS`),
older snapshots, array validation, serializable isolation against heap twins,
the visibility rules and `VACUUM FULL`, memory with many subtransactions,
publications and wide rows, the atomic hand-over of a subtransaction under
injected failures (`t/016`), stripe sizes and the memory bound under any
grouping, row width and cursor (`t/017`), the maintenance commands against
older snapshots, tables older and younger than a snapshot (`t/019`) and the
creation of a table from every source (`t/021`), limits and flags, a reader or
writer between the two phases of a hand-over (`t/022`), the cleanup after a
rewrite (also with event triggers off or disabled, in single-user mode and in a
database made by an earlier script), concurrent commands and a stripe catalog of
the earlier shape (`t/023`), a callback of another module that writes after the
access method's own commit-time flush, in both load orders, against a heap twin
(`t/024`), the bound on the memory of the write buffers measured as the memory of
the backend, for levels of subtransaction, cursors, relations, partitions and wide
rows, a value larger than a chunk group, and each constant of the ceiling pinned by
a measurement (`t/025`), a random comparison with heap of cursors, savepoints
and writes under real memory pressure, with a probe that writes in the window
of a hand-over and after the commit-time flush, whose seed is random per run,
printed, replayed with `KC_FUZZ_SEED` (`KC_FUZZ_EPISODES`, `KC_FUZZ_TRACE`), and
which takes each path of the write side that it names once by construction (a
scripted prelude) and reports how often the random episodes took it (`t/026`), a write at every event of the end of a transaction and
subtransaction, the release of a resource owner and a critical section, in both
load orders, a refusal that fails a transaction that is still alive, savepoints,
`PREPARE TRANSACTION`, the message settings, a log hook that raises and a client
that is gone (`t/027`), stripe rows left behind, the cap on the files kept, and
the way to find and remove them (`t/028`), `COMMIT AND CHAIN`, a parallel leader
and a base backup under load, restored (`t/029`), the cost of rows written late
(`t/030`), the parallel scan against a heap twin with 0, 1 and 4 workers, the workers that really read stripes, a rescan, a concurrent writer, the leader's own rows and a standby (`t/043`, `columnar_parallel`), the stand-down of the orphan step as the check sees it, the guard for an access method over a second SQL function, a replaced handler seen by another session, a copy of the routine and a handler that raises an error (`t/039`), the two snapshots of the orphan step (`t/036`), the memory of a check with hundreds of locked tables (`t/042`), every entry point of the access method at every moment where an error
is forbidden, and the walk that fails for a callback without a decision (`t/031`),
the way a module that cannot be built is treated (`t/032`), the same write paths on a standby with `wal_consistency_checking = all` and across a promotion, and a late drop under a hot standby's reader (`t/033`), logical decoding of transactions that had a refused call (`t/034`), the log line of autovacuum (`t/035`), the check functions: the snapshot they read with, the locks they never wait for, the errors they never hide and what they leave behind, the orphan step against DDL that commits between its reads, the process ids in the `not_checked` rows (`t/036`), the rule for giving the tail of the file back, with a hot standby, the 1000-page limb, and the lock wait (`t/037`), the `relfrozenxid` of a truncated or rewritten table and the autovacuum line (`t/038`), a second access method over the handler (`t/039`), the check with more tables than the lock table holds (`t/040`), the wraparound failsafe and the order of the frozen id and the truncation (`t/041`), the reservation of row numbers (0.2, C6a): the TID of a row at insert and after every kind of flush against a heap twin with a generator of random transactions (`t/044`, seed printed, `KC_SEED`), model-based savepoints and exception blocks three levels deep (`t/045`), an immediate stop at each point of a reservation and a prepared transaction across a crash (`t/046`), two and four sessions and holders of reservations (`t/047`), a standby, the log and a base backup with reservations open (`t/048`), the check functions against live, prepared, aborted and killed writers (`t/050`), the plain regression test with the setting on (`columnar_reserve`) and the update from 0.1 (`upgrade`; `t/049_upgrade_data.pl` carries a data directory of the 0.1 code, commit bfda9e8842, built into an installation named by `KC_OLD_INSTALL`, across it, and is skipped without it), and two laps of the
transaction id space (`t/013`).

Some files do not run everywhere, and say why when they are skipped:

* `t/049_upgrade_data.pl` needs an installation of the code of 0.1 (the commit
  bfda9e8842 of the repository, built and installed into a copy of the
  installation under test), named by `KC_OLD_INSTALL`; without it the file is
  skipped, because a test run cannot build an older tree.  The update itself is
  tested without it by `sql/upgrade.sql`, which installs 0.1 with the new
  library (the catalog of 0.1, files of format version 2) and updates;
* `t/016_atomic_handover.pl` needs the developer settings `fault_point` and
  `fault_nth`, which exist only in a server built with assertions
  (`USE_ASSERT_CHECKING`); a production build has no fault injection at all
  (that is intended: the code is not compiled there), so the atomicity of the
  hand-over is tested on assertion builds only, which is where the tests are
  run, and a build for packaging skips the file;
* `t/013_xid_wraparound.pl` needs the `xid_wraparound` module of the
  PostgreSQL tree and takes minutes; `KC_SKIP_WRAPAROUND=1` skips it.  Where the
  module is not installed (an installation made for an out-of-tree build does not
  have it; the in-tree run does) the file is a counted skip, with the reason and
  the words "the anti-wraparound path ... was NOT tested by this run", and
  `KC_REQUIRE_WRAPAROUND=1` turns it into a failure.  The freezing of the ids of
  the metapage is also tested without it, in `t/021_table_creation.pl`;
* `t/031_entry_points.pl` skips, on a server built with assertions, 142 cells
  (71 for each order of loading) of reads at moments where an error is forbidden that
  PostgreSQL's own assertions forbid (see "Writes while a transaction is ending",
  which lists them), counted and named at the end of its output; they run on a
  server built without assertions.  Six more (3 for each order: the scan of a table in
  the three release phases of the resource owner) run on no build, because PostgreSQL
  itself refuses the open of a relation and the buffer pin there, for heap as well;
  they are counted apart and named as that in the output (C11-08), not as skipped for
  assertions, which was not true of them;
* `t/022`, `t/024`, `t/026`, `t/028`, `t/029` and `t/030` build the probe module
  `t/kcprobe/kcprobe.c` (callbacks at the end of a subtransaction and at the
  pre-commit and pre-prepare, which run SQL from the setting `kcprobe.sql` or
  `kcprobe.commit_sql`), and `t/027` and `t/031` build `t/kcevents/kcevents.c` (a callback
  for every event of the transaction and subtransaction callbacks, for the
  three phases of the release of a resource owner and for the exit of the
  process, which writes with the table access method calls into a table it holds
  open at the moment its setting `kcevents.at` names), both through
  `t/KcProbe.pm`, with the compiler and headers of the installation
  (`pg_config`) as the modules of `src/test/modules` are built for their tests;
  they are never installed.  A machine without a C compiler skips those files with
  a message that names the module; a compiler that runs and a build that fails
  fails the file (`t/032`), and the in-tree (meson) build compiles both modules
  with the extension, so a tree that cannot build them fails the build;
* `t/025_memory_bound.pl` reads `/proc/self/status` and is skipped where there is
  no `/proc`.

`perf/perf.sh` measures size and scan time against heap.
