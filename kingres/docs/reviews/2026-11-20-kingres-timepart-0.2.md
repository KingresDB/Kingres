<!-- SPDX-License-Identifier: AGPL-3.0-only -->

# Independent review of kingres_timepart 0.2: conversion of partitions to columnar storage and back

Reviewed commits: 554a81bc4c (T2, conversion of partitions to columnar storage and back) and
a3499aa2fc (conversion of partitions whose table has indexes), on top of 6b8ac23f68.
Findings are numbered T13-01 and on. Nothing was fixed; nothing was committed.

## 1. Scope and method

Scope: contrib/kingres_timepart at 6b8ac23f68, version 0.2: convert_partition, convert_old_partitions,
the convert phase of timepart_run (policy option columnar_after), the refusals, the swap, the
history, the upgrade script. kingres_columnar itself was not reviewed (done separately today); a
columnar behaviour is reported only where the conversion exposes it.

Method: read the code (full script 0.2, upgrade script 0.1--0.2, README, design notes), then work
adversarially against private PostgreSQL installs:

* PostgreSQL 18.7 and PostgreSQL 19rc2, private copies of the build installs, with kingres_columnar
  and kingres_timepart built from this commit and installed into those copies (clean build, warnings
  as errors, through kingres/dev/ext-majors.sh).
* Hand-driven clusters (one cluster at a time, ports 58800 to 58899): fidelity comparison of a
  partition before and after, with a generated table that has every kind of object; concurrent
  writers (pgbench through the parent and by the partition name) during six conversions; sessions
  held open across a conversion; cancel, statement timeout, pg_terminate_backend, kill of a backend,
  immediate stop in the middle of the copy; roles (owner, member of the owner role, stranger,
  superuser with SET ROLE, trust_table_owners on and off); hostile names; time zones and daylight
  saving in the policy path; logical decoding of a conversion.
* Mutation testing: one-line mutants of the full script, one at a time, each installed into a clean
  build, then the regression tests and (for survivors) the TAP tests run.

## 2. What was run

| What | PostgreSQL 18.7 | PostgreSQL 19rc2 |
|---|---|---|
| ext-majors.sh kingres_timepart (sync, clean build, install, regress, tap), build with -Werror | ok | ok |
| pg_regress (16 tests: setup, grid, create, policy, run, retention, names, hostile, security, history, privilege, forged, owner_code, convert, drop, upgrade) | 16 of 16 pass | 16 of 16 pass |
| TAP (8 files: 001_locks, 002_interrupt, 003_dump_restore, 004_owner_and_races, 005_convert_concurrent, 006_convert_crash, 007_convert_standby, 008_convert_prepared) | 8 files, 606 tests, pass | 8 files, 606 tests, pass |
| fidelity comparison heap -> columnar -> heap of a partition with about 60 objects | done | done |
| lost-column and lost-index race (T13-01) | reproduced | reproduced |
| REPEATABLE READ reader across the swap (T13-02) | reproduced | reproduced |
| concurrent writers during conversions, cancel and crash points, hostile names, roles, publications, time zones, default partition, late failures, mutants | done | not repeated (the mutants and the time-zone, role and crash runs were made on 18 only) |

The extension has no C code; the mutants are therefore edits of the SQL script, and "a clean build"
is a fresh copy of the extension installed into the private install before each run.

## 3. Summary

| Severity | Count | Findings |
|---|---|---|
| critical | 0 | |
| high | 1 | T13-01 |
| medium | 4 | T13-02, T13-03, T13-04, T13-05 |
| low | 7 | T13-06, T13-07, T13-08, T13-09, T13-10, T13-11, T13-12 |
| note | 2 | T13-13, T13-14 |

The most serious: T13-01. The statements of a conversion (column list, index list, refusal checks)
are built before the lock on the partition is taken. A DDL transaction that adds a column and fills it,
or creates an index, on the partition or its table, and that commits while the conversion waits for
its lock, is half lost: the new column is copied as null for every row, and a new index (also a
unique index) is not on the new partition. The conversion reports success and the row counts match.

What held under attack is listed in section 5, after the answers.

## 4. Answers to the questions

### 4.1 Does a conversion lose, double or change a row, an index entry, a constraint, a default, a privilege, a comment, a trigger, a policy or ownership?

Rows: no row was lost, doubled or changed in any run of mine that was not racing with DDL. A partition
of 6003 rows with every type that is easy to get wrong (arrays with lower bound 0 and two dimensions,
NaN and -0 numerics, char(n) padding, enum, composite, domain, range, jsonb with toasted values,
NULLs, stored and virtual generated columns, a dropped column and a different physical column
order) gave the same text of every row, by md5 over the rows, before, after heap -> columnar and after
columnar -> heap, on both majors. With the DDL race of T13-01 a column is lost.

Compared object by object (catalog dump of the partition before and after, both directions, both
majors), these are kept: column names, types, order, defaults, collations, NOT NULL, generated
columns, comments on table, columns, indexes and constraints, table and column privileges with
GRANT OPTION, owner, partition bound, replica identity FULL, inherited CHECK constraints with their
inheritance flags, NO INHERIT checks, NOT ENFORCED checks (PostgreSQL 18), all indexes (partial,
expression, INCLUDE, DESC, with options, unique, primary key, unique constraint) with their names and
their attachment to the parent's indexes, cloned triggers of the parent, per-column storage and
compression.

These are changed or lost silently (findings):

* the enabled state of a cloned trigger: disabled, replica and always all become enabled (T13-04);
* storage parameters of the table (fillfactor, autovacuum_enabled, parallel_workers), the statistics
  target and the n_distinct option of a column, the statistics target of an index column, the CLUSTER
  marker (T13-05);
* a NOT VALID check constraint becomes valid, and the conversion fails if a row violates it (T13-06);
* the grantor of a privilege, and privileges the owner had revoked from itself (T13-07);
* any column, index or other object added while the conversion waits for its lock (T13-01).

Refused cleanly, with a message that names the object (each seen in my runs or covered by the
regression test timepart_convert, whose rules the mutants below exercise): partitioned or default partition,
partition of another owner, unlogged, row-level security, policy, own trigger, rule, publication of
the partition or of the table (columnar target), extended statistics, identity column, owned
sequence, view over the partition, foreign key to or from the table (columnar target), deferrable
unique, exclusion constraint on the parent or the partition (columnar target), another access
method, a name that is taken (`__kc`), a stranger or a member of the owner role.

Not refused, and failing late with a raw error after a complete copy: objects that depend on the
partition without being a view: a function with the partition's row type as an argument, an SQL-body
function that reads it, a policy of another table that reads it, a default of another table that casts a
name to regclass (T13-08).

### 4.2 Concurrency: what do readers and writers of the parent and of the partition see, what do they wait for, can a writer's row land in the old partition after the copy and be lost?

Readers of the partition and of the parent go on during the copy; the swap asks for ACCESS EXCLUSIVE
on the parent for at most 500 ms (or half of deadlock_timeout), and readers that arrive meanwhile queue
behind that request. Writers that name the partition wait for the copy, then go on against the new
partition. Writers through the parent keep the parent; if one is waiting for the partition, the swap
sees it in pg_locks and gives way at once (T13-13 on whether that look matters).

A row cannot land in the old partition after the copy and be lost: the lock SHARE ROW EXCLUSIVE is
taken before the copy and held to the commit; the copy and the row count run after it, each statement
with a new snapshot (the transaction must be READ COMMITTED, which is enforced). In a run with
pgbench (3 clients inserting through the parent, 3 clients inserting by the partition name, 25 s,
about 28800 transactions) during six conversions, heap -> columnar -> heap -> ..., the number of rows
found equals the number of transactions that pgbench reports as processed, for both writers, and no
conversion failed. The TAP tests 005 and 008 cover the cycle with writers, long readers and prepared
transactions and pass.

Two things a reader or writer sees that the README does not say:

* A REPEATABLE READ or SERIALIZABLE transaction that took its snapshot before the swap and reads the
  parent after it sees the converted partition empty (T13-02). The README says a reader of the parent
  "sees the old partition or the new one, never neither".
* With a schema publication covering the partition's schema, the copy of a columnar -> heap conversion
  is published as inserts into the temporary table (T13-03).

### 4.3 Crash and cancel at every step

The table was either as before or as after in every case I tried: statement_timeout during the copy,
pg_cancel_backend, pg_terminate_backend, kill -9 of the backend, and pg_ctl stop -m immediate in the
middle of the copy on a 2.5 million row partition (heap -> columnar). After each, the partition was
heap, the table had the same row count (2506003) and the same md5 over all rows, there was no
`__kc` table and no leftover index, the table had the same three partitions; the conversion could be
repeated at once, and undone (columnar -> heap) with the same result. The TAP test 006 (crash before
each of the nine statements, in the copy, just after the commit) passes on both majors.

One thing the README does not say (T13-11): after a crash (not after a cancel or an error) the
unfinished new relation's files stay in the database directory. A kill -9 in the middle of a
columnar -> heap copy left a 187 MB file that no catalog row names, and that no checkpoint removes.
This is how PostgreSQL treats a table created by a transaction that was in flight at a crash, but a
conversion makes the file as large as the partition.

### 4.4 Indexes, unique and primary keys, foreign keys, partition-wise constraints, default partition

* Indexes, primary key, unique constraints: made on the new table after the load from
  pg_get_indexdef and pg_get_constraintdef; the attach finds the parent's index for each; the names,
  options, comments and the uniqueness are the same afterwards (tested both directions). An index created
  while the conversion waits is lost (T13-01). With a multibyte partition name of 58 bytes and two
  indexes the conversion fails with "relation ... already exists" because the temporary index names
  are cut by characters, not bytes (T13-09).
* Foreign keys: a foreign key from the table, or to it (also the clone on each partition) is refused for a
  columnar target with a message that names it; a foreign key to the parent cannot exist on a columnar
  partition at all (kingres_columnar refuses it), so the heap direction could not be exercised with
  one. Nothing was lost.
* Exclusion constraint on the parent (btree, PostgreSQL 17 and later): refused for a columnar target,
  named.
* NOT VALID constraints: see T13-06.
* Default partition of the parent: the conversion of another partition is not refused. ATTACH
  PARTITION scans the default partition under ACCESS EXCLUSIVE on the parent (T13-10). The refusal
  of a default partition applies only to converting the default partition itself.

### 4.5 Security

* Who may convert: the owner of the table, connected as itself, or a superuser. A member of the owner
  role (inheriting or not), a stranger, and a superuser that has done SET ROLE to the owner are all refused
  (permission denied, or "must be owner"). With trust_table_owners off a superuser is refused for a
  non-superuser owner (owner_code_not_trusted); with it on it is accepted, as documented.
* Whose code runs: the owner's own expressions (index expressions, CHECK constraints, generated
  columns) run as the owner, in the owner's own session (direct mode), or inside a SECURITY
  DEFINER function owned by the owner in a superuser's session (frame mode, only with the
  trust setting). The copy names the columns one by one, so no default is evaluated. ANALYZE after the
  commit runs as the caller, and PostgreSQL runs it as the table owner for the expressions.
* Injection: a schema, table, partition, column and index named with quotes, $tpl$, $$, a backslash,
  a semicolon and "--", a comment with the same characters, a default with the same characters, a
  sort column with a quote: the conversion worked and kept all of them, nothing outside was touched.
  The statements are built with quote_ident and %L and the DO body holds no name of its own.
* search_path: every function has SET search_path = pg_catalog, kingres, pg_temp (or pg_catalog,
  pg_temp for the executors); the procedures set 'pg_catalog, pg_temp' as their first statement and
  after every COMMIT; the DO body qualifies everything. The text of an index definition is produced
  under one path and executed under another (kingres is not on the executing one); it matters only for an
  index expression that calls a function in schema kingres, which would fail cleanly. I did not test it.
* The check that decides whether a superuser's session may run the owner's code, session_user =
  current_user in convert_partition, is not covered by any test (mutant M23 survives, T13-12).
* The helper functions timepart__convert_stmts and timepart__special_table are executable by PUBLIC
  (T13-14); nothing of the owner's is reachable through them.

### 4.6 The scheduler (policy option columnar_after)

There is no autonomous scheduler in the extension: timepart_run (or the phases) is called by a driver,
and timepart_due says which tables are due. Within a run the convert phase acts on exactly the
partitions that: are not the default partition, have both bounds on the policy's grid, end at or before
as_of minus columnar_after (calendar arithmetic in the policy's time zone for timestamptz; UTC wall
clock for timestamp and date), are not already columnar by name, and are not about to be retired. A
partition off the grid is reported as "misaligned" and left alone, also when it is old enough (checked
with a partition of 12 hours in a 1 day grid, as_of ten days later): not converted by the run,
converted by convert_old_partitions, as documented.

Checked in America/New_York around the change of 2026-03-08 (a day of 23 hours): with columnar_after
of 2 days, as_of 2026-03-10 00:00 local converts the partitions that end at or before 2026-03-08
00:00 local, one second earlier converts one fewer, one second later converts the same three; the
plan for the same instant is the same in an Asia/Tehran session; a real run recorded one action per
partition, with rows, sizes and time. A second run at the same time did nothing but report the
misaligned partition again. A run that meets a partition it cannot convert records "skipped" for
a refusal (not a failure), or "failed" for a late error, and tries again in the next run (T13-08).

### 4.7 Tests: mutants

See section 6. 39 mutants, 28 killed, 11 survive; one of the survivors is equivalent, three are
timing rules, and seven are real gaps (one of them a security rule).

## 5. What held under attack

* No lost, doubled or changed row in any conversion that did not race with DDL, under writers by
  parent and by partition name, on both majors.
* A conversion that fails, is cancelled, timed out, terminated, killed or cut by an immediate stop leaves
  the partition as it was (row count and md5 of all rows equal) and no `__kc` table; it can be repeated.
* The upgrade script and the full script define 19 functions and procedures with identical text.
* Refusals of dangerous objects name the object and are recorded as "skipped".
* Roles: only the owner connected as itself or a superuser; no deputy through SET ROLE.
* Hostile identifiers and strings.
* The swap never made a user's statement the victim of a deadlock in my runs.
* DST and time zone of the plan.
* A conversion of a partition under the policy is one transaction with its history row.

## 6. Mutants

One-line edits of kingres_timepart--0.2.sql, one at a time, each in a clean copy installed into the
private PostgreSQL 18 install; first pg_regress, then (for survivors) the TAP tests 001, 003, 004,
005, 006, 007, 008. The regression test "upgrade" fails for any edit of the full script, because it
compares the upgrade path with it; its failure alone is not counted as a kill. Verdict: killed (by which
test) or survived.

| Id | Mutant | Verdict |
|---|---|---|
| M01 | row count check of the copy off | killed (timepart_convert) |
| M02 | lock SHARE instead of SHARE ROW EXCLUSIVE | killed (TAP 005) |
| M03 | refusal of other isolation levels off | killed (timepart_convert) |
| M04 | no look at pg_locks before the swap | SURVIVES (T13-13) |
| M05 | swap waits 60 s instead of the short wait | SURVIVES (T13-13) |
| M06 | column privileges not copied | killed (timepart_convert) |
| M07 | table privileges not copied | killed (timepart_convert) |
| M08 | no default sort for columnar | killed (timepart_convert) |
| M09 | refusal of a partition of another owner off | killed (timepart_convert) |
| M10 | refusal of the default partition off | killed (timepart_convert) |
| M11 | refusal of an own trigger off | killed (timepart_convert) |
| M12 | refusal of an identity column off | killed (timepart_convert) |
| M13 | refusal of row-level security off | killed (timepart_convert) |
| M14 | table comment not copied | killed (timepart_convert) |
| M15 | replica identity FULL not copied | SURVIVES (gap, T13-13) |
| M16 | tablespace not copied | SURVIVES (gap, T13-13) |
| M17 | convert threshold one day late | killed (timepart_run, timepart_convert) |
| M18 | convert plan without the grid check | SURVIVES (gap, T13-13) |
| M19 | convert also what is about to be retired | killed (timepart_convert) |
| M20 | CHECK constraints not copied | killed (timepart_convert) |
| M21 | column comments not copied | killed (timepart_convert) |
| M22 | helper CHECK of the bound not dropped | killed (timepart_convert) |
| M23 | no session_user = current_user check in convert_partition | SURVIVES (T13-12) |
| M24 | no ANALYZE after a run's conversion | killed (timepart_convert) |
| M25 | a conversion retried inside a run | killed (TAP 005) |
| M26 | convert_old_partitions without the UTC reading of the limit | SURVIVES, equivalent (the reading with zone UTC is the identity) |
| M27 | convert_old_partitions: strict instead of at or before | killed (timepart_convert) |
| M28 | refusal of a partition in a publication off | killed (timepart_convert) |
| M29 | refusal of an index as replica identity off | killed (timepart_convert) |
| M31 | index comments not copied | killed (timepart_convert) |
| M32 | constraint comments not copied | SURVIVES (gap, T13-13) |
| M33 | columnar options not applied | killed (timepart_convert) |
| M34 | default privileges of the new table not revoked | SURVIVES (gap, T13-13) |
| M35 | refusal when already stored with the target method off | killed (timepart_convert) |
| M36 | refusal of an unlogged or temporary partition off | killed (timepart_convert) |
| M37 | refusal of another access method off | SURVIVES (gap, T13-13) |
| M38 | primary key and unique constraints rebuilt as plain unique indexes | killed (timepart_convert) |
| M39 | temporary index names all the same | killed (timepart_convert) |
| M40 | no short lock_timeout for the swap | SURVIVES (T13-13) |

(M30 was a duplicate of M38 whose pattern did not apply; it was replaced by M38. Counting M01 to M29
and M31 to M40: 39 mutants, 28 killed, 11 survive.)

Behaviour checked by hand against the code for the survivors that are gaps: replica identity,
constraint comments and default-privilege revocation are all correct in the real code (my fidelity
run shows it); the grid rule keeps a misaligned old partition out of a run (section 4.6). The rules
are right, but no test would notice if they broke.

## 7. Findings

### T13-01 (high) Statements are built before the lock: a column, index or constraint added meanwhile is lost silently

Where: timepart__convert_stmts builds, as text, the column list of the copy (v_cols), the sort, the
index statements (v_ix) and the refusals; the DO block then starts with LOCK TABLE ... SHARE ROW
EXCLUSIVE, which can wait up to create_lock_timeout_ms for writers (default 5 s). Everything
read from the catalog before the lock is stale after the wait. CREATE TABLE (LIKE ...) inside the
DO reads the catalog again, so the new table has the new column, but the INSERT lists only the old
columns and the new column gets its default (null).

Locks taken while the statements are built are momentary (reading the bounds opens relations and
releases them), so nothing keeps a DDL transaction out between building and locking.

Reproduction (owner session A, owner session C connected as the owner; table ev partitioned by
range on ts, partition ev_p1 of 6003 rows, PRIMARY KEY (id, ts), other indexes):

    -- A
    BEGIN;
    INSERT INTO ev (id, ts, kind) VALUES (777777, '2026-01-10', 'x');   -- ROW EXCLUSIVE on ev_p1
    -- C (a second session, at once)
    SET kingres_timepart.create_lock_timeout_ms = 60000;
    CALL kingres.convert_partition('ev', 'ev_p1', 'columnar');          -- builds its text, waits at LOCK TABLE
    -- A, three seconds later
    ALTER TABLE ev ADD COLUMN extra int;
    UPDATE ev SET extra = id::int WHERE ts < '2026-02-01';
    CREATE UNIQUE INDEX p1_late_unique ON ev_p1 (id, ts, num);
    COMMIT;
    -- after C returns (CALL succeeded):
    SELECT count(*), count(extra) FROM ev_p1;                          -- 6004 | 0
    SELECT count(*) FROM pg_class WHERE relname = 'p1_late_unique';    -- 0

Result on 18.7 and 19rc2: 6004 rows, the new column null in every row (6004 values lost), and the
unique index is gone, so uniqueness is no longer enforced on the partition. A CHECK constraint added in
the same way is kept (LIKE ... INCLUDING CONSTRAINTS runs under the lock). The conversion
succeeds, the row counts match, the history says "converted". The same text is used by the policy
path, so a run can do this too.

Fix: make the DO block the only place that reads the catalog: take the lock first, then compute the
column list, the sort, the index statements and the refusals inside the block. A smaller fix: have
convert_stmts hand a fingerprint (md5 of the column list, the index definitions and the constraint
list of the partition and of the parent), recompute it in the block right after the LOCK, and raise a
retryable error (for example SQLSTATE 55P03, "schema_changed") when it differs. Add a test with a
second session that holds ROW EXCLUSIVE, does DDL on the table and commits while the conversion
waits.

### T13-02 (medium) A REPEATABLE READ or SERIALIZABLE reader that began before the swap sees the converted partition empty

Where: the new table is filled by the conversion's own transaction; its rows (and the columnar
stripes) are newer than any snapshot taken before the commit. A transaction with an older snapshot
reads the parent after the swap, finds the new partition in the catalog (current catalog), and sees none
of its rows. The README (Conversion, step 7) says "A reader of the parent sees the old partition or
the new one, never neither".

Reproduction (18.7 and 19rc2), partition ev_p1 holds all 6003 rows of ev:

    -- reader R
    BEGIN ISOLATION LEVEL REPEATABLE READ;
    SELECT 1;                      -- snapshot taken; R has not touched ev
    -- converter, owner session
    CALL kingres.convert_partition('ev', 'ev_p1', 'columnar');
    -- R
    SELECT count(*) FROM ev;       -- 0 (the true count is 6003)
    SELECT count(*) FROM ev_p1;    -- 0
    COMMIT;

A reader that already read the parent holds ACCESS SHARE on it, so the swap gives way (documented);
the window is the reader that has a snapshot and has not yet touched the table. pg_dump is such a
reader (it takes its snapshot first and locks the tables after). I did not run SERIALIZABLE; its
snapshot rule is the same, so the same result is to be expected.

Fix: say it in the README (a long REPEATABLE READ or SERIALIZABLE transaction, pg_dump included,
must not span a conversion), and, better, make the conversion wait or give way when a session has
a snapshot older than the conversion (pg_stat_activity.backend_xmin older than the transaction id of
the copy, for a session that is not running a statement), as CREATE INDEX CONCURRENTLY waits for
old snapshots. For a heap target the copy could be frozen so that every snapshot sees it.

### T13-03 (medium) A schema publication publishes the whole copy, under the temporary table name

Where: for a heap target (columnar -> heap) the refusals look only at publications that name the
partition itself. A publication FOR TABLES IN SCHEMA that covers the partition's schema (and a FOR
ALL TABLES publication, where one exists) includes the new table `<partition>__kc` from the moment it is
created, so every row copied is a published INSERT.

Reproduction (wal_level = logical): partition pbs.ev_p1 columnar, 1000 rows; CREATE PUBLICATION
pub_schema FOR TABLES IN SCHEMA pbs; a pgoutput slot; CALL kingres.convert_partition('pbs.ev',
'pbs.ev_p1', 'heap'). Decoding the slot with pub_schema gives 1000 insert messages for the relation
pbs.ev_p1__kc. A subscriber that has no table of that name stops applying with an error, and a
subscriber that has the parent only gets no rows for it.

(The heap -> columnar direction is refused when a publication would include the columnar partition,
as documented, and the refusal held in my runs.)

Fix: refuse the conversion in both directions when a publication covers the partition, its table or
its schema, or build the new table in a schema no publication covers and move it with ALTER TABLE
... SET SCHEMA before the attach (the load is then not published).

### T13-04 (medium) The enabled state of a trigger of the partition is reset to enabled

Where: the new table gets its triggers from the parent's cloning at the attach; the state of the clone
on the old partition is not read.

Reproduction: parent with BEFORE INSERT row triggers tr1, tr2, tr3 (cloned to partition p1);
ALTER TABLE p1 ENABLE REPLICA TRIGGER tr1; ENABLE ALWAYS TRIGGER tr2; DISABLE TRIGGER tr3; convert p1.
Before: tgenabled R, A, D. After: O, O, O (both majors, both directions). A trigger that an
administrator disabled on a partition (a common way to load or repair data) runs again, and replica
and always triggers lose their mode, without a message.

Fix: after the attach, read tgenabled of the old partition's cloned triggers (by tgparentid, by name)
and apply ALTER TABLE ... ENABLE REPLICA / ENABLE ALWAYS / DISABLE TRIGGER on the new table; or
refuse a partition whose cloned triggers are not in state O, naming the trigger. Add it to the README
list of what the new partition is.

### T13-05 (medium) Storage parameters, statistics targets and the CLUSTER marker are dropped; the README says "the same table"

Where: CREATE TABLE ... (LIKE ... INCLUDING STORAGE ...) does not carry reloptions, attstattarget,
attoptions or the clustering index, and the DO block does not copy them. Round trip heap -> columnar ->
heap on both majors:

| Object | Before | After |
|---|---|---|
| table storage parameters | fillfactor=70, autovacuum_enabled=false, parallel_workers=3 | none |
| ALTER COLUMN ... SET STATISTICS 500 | 500 | default |
| ALTER COLUMN ... SET (n_distinct = 100) | set | gone |
| statistics target of an index column | 777 | default |
| ALTER TABLE ... CLUSTER ON | marked | not marked |

autovacuum_enabled = false being lost means autovacuum starts working on a table whose owner had
turned it off. For a columnar target most of these have no meaning; the loss on columnar -> heap
(the way back) is complete.

Fix: copy the parameters that the target access method accepts (ALTER TABLE ... SET (...) in the
DO block, checking each against the target), the column statistics targets and options, and the
index options; or list them in "What the new partition is" as not carried, and refuse when a
partition has storage parameters, for the owner to decide, as is done for the other objects.

### T13-06 (low) A NOT VALID constraint becomes valid, and a violating row fails the conversion late

Where: LIKE ... INCLUDING CONSTRAINTS makes the check on the empty new table, where it is validated,
and the INSERT then checks every row.

Reproduction: ALTER TABLE p1 ADD CONSTRAINT bad CHECK (id < 1500) NOT VALID (rows of p1 with larger id
exist); CALL kingres.convert_partition(...): ERROR: new row for relation "p1__kc" violates check
constraint "bad". The partition can never be converted; under the policy the action is recorded
"failed" (not "skipped", it is not a refusal) in every run, each run copying until the first violating row.
With data that does not violate the constraint, the partition ends with a VALID constraint (convalidated
f -> t), which is harmless but was not asked for.

Fix: refuse in timepart__convert_refusal when a CHECK or NOT NULL constraint of the partition is not
validated, naming it; or recreate it NOT VALID after the load.

### T13-07 (low) The grantor of privileges is changed and the owner's own revoked privileges come back

Where: the privileges of the old partition are replayed as GRANTs by the owner on the new table, from
aclexplode; the owner's own entry is skipped.

Reproduction: owner grants SELECT WITH GRANT OPTION to role B; B grants SELECT to C; owner revokes
UPDATE and DELETE from itself. Before: {owner=arDxtm/owner, B=r*/owner, C=r/B}. After:
{owner=arwdDxtm/owner, B=r*/owner, C=r/owner}. The README says "copied exactly, including GRANT
OPTION". The grant of C now belongs to the owner (REVOKE ... CASCADE of B's right no longer takes
C's with it), and the owner can update and delete again.

Fix: replay with GRANT ... GRANTED BY the original grantor where PostgreSQL allows it, or copy the
owner's entry by REVOKE of the privileges missing from it; or state the difference in the README.

### T13-08 (low) Dependencies that are not views are found by the DROP, after the whole copy, in every run

Where: timepart__convert_refusal looks at views, rules, foreign keys, owned sequences and so on, but
not at other objects that depend on the partition. DROP TABLE (without CASCADE) then fails with SQLSTATE
2BP01 after the copy, the indexes and the swap wait.

Reproduction: CREATE FUNCTION f(x ev_p1) ...; or CREATE FUNCTION g() ... BEGIN ATOMIC SELECT count(*)
FROM ev_p1; END; or a policy of another table whose USING reads ev_p1; or a default 'ev_p1'::regclass.
CALL convert_partition: ERROR: cannot drop table ev_p1 because other objects depend on it, after the
copy (everything rolled back, nothing lost). Under a policy with columnar_after, every run (with
create_ahead and run_every as usual, hourly by default) copies the whole partition again and records
"failed" (seen in two consecutive runs); a partition of 1.5 million rows took about 1.3 s per run for the copy. timepart_check
reports convert_blocked with the raw message.

Fix: before any lock, scan pg_depend for every object that depends on the partition or one of its
columns (other than its indexes, constraints, own triggers, toast and the parent) and refuse, naming
it with pg_describe_object, as is done for views; recorded as "skipped" it is not a failure of the run.

### T13-09 (low) Multibyte partition names near the limit: the temporary index names collide

Where: v_ixtmp := left(v_tmp, 55) || '_' || v_ixn cuts by characters; the name of a partition is cut to
59 bytes by bytes. For a name of 58 bytes of two-byte characters, v_tmp is 62 bytes, the suffix is
cut off by the 63 byte limit of identifiers, and every temporary index gets the same name.

Reproduction: a partition named with 29 Arabic letters (58 bytes), two indexes on it, convert:
ERROR: relation "<name>__kc_" already exists. A name of the same length in ASCII works.
Partitions of such tables can never be converted.

Fix: build the temporary names from the OID or a counter ('kc_' || oid || '_' || n), which is short and
cannot collide, or cut by bytes.

### T13-10 (low) The attach scans the default partition under ACCESS EXCLUSIVE on the parent

Where: the swap does DETACH then ATTACH of the new table while holding ACCESS EXCLUSIVE on the parent;
if the parent has a default partition, ATTACH scans it for rows of the new range (the helper CHECK on the
new table does not help the default partition). The README's section "Default partition" explains this
for creation only, and set_policy refuses a table with a default partition, but a default partition
added later is not refused by convert_refusal.

Measured: a default partition of 4 million rows (322 MB) added about 0.2 s to the attach (an
ordinary DETACH then ATTACH of the partition measured 0.2 s), so the stall grows by about 50 ms per
million rows of the default partition; all readers and writers of the parent wait for it, and lock_timeout does
not bound a scan.

Fix: refuse the conversion (or make timepart_check say so) when the parent has a default partition
larger than a setting, or move the check out of the lock: ALTER TABLE default ADD CONSTRAINT ... CHECK
(NOT (ts >= lo AND ts < hi)) NOT VALID, VALIDATE before the swap.

### T13-11 (low) After a crash the files of the unfinished new table stay behind

Where: README, Conversion, "Crash behaviour": "a crash ... leaves the old partition as it was, whole, and
no `__kc` table". True for the catalog; the new table's files are not removed by crash recovery.

Reproduction: 2.5 million row partition columnar; start convert_partition(..., 'heap'); kill -9 the backend
after 2.5 s; restart is automatic; the partition is columnar and whole; after two checkpoints a file of
187 MB that no pg_class row names is still in the database directory (and a smaller one for the
heap -> columnar direction, which writes its data at the commit). Cancel, error, timeout and terminate do not
leave anything (checked after two checkpoints).

Fix: mention it in the README; document how to find such files (a file of the database directory whose
filenode is not in pg_class). Nothing in the extension can remove it.

### T13-12 (low) The session_user check of convert_partition is untested (mutant M23)

Where: IF v_self = v_info.r_owner_oid AND session_user = current_user THEN exec_direct. With the second
condition removed, a superuser that did SET ROLE owner and called convert_partition would run the
owner's code (CHECK functions, index expressions) as the owner in the superuser's session, where
RESET ROLE brings the superuser back. The real code refuses that case (checked: "permission denied: only
the owner of the table itself or a superuser may convert its partitions"), so the rule is right; but no
regression test and no TAP test fails when it is removed, although it is the boundary against the deputy
problem of K12-09. The suite ran 16 regression tests and 606 TAP tests on each major without noticing.

Fix: a regression test in timepart_privilege or timepart_owner_code: as a superuser, SET ROLE to the
owner and CALL convert_partition and convert_old_partitions; expect the error, and the partition
unchanged.

### T13-13 (note) Test gaps shown by the surviving mutants

Survivors (section 6) and what each rule needs:

* M15, replica identity FULL copied: no test of a partition with REPLICA IDENTITY FULL or NOTHING.
* M16, tablespace copied: no test with a tablespace.
* M18, grid check in the convert plan: no test of a misaligned old partition with columnar_after set; the
  rule holds (checked by hand).
* M32, constraint comments copied (the comment of a primary key or unique constraint): not tested;
  correct in the real code.
* M34, default privileges revoked on the new table: not tested (ALTER DEFAULT PRIVILEGES in the schema of
  the table); correct in the real code.
* M37, refusal of another table access method: no third access method in the tests.
* M04, M05, M40, the swap wait: the pg_locks look and the 500 ms bound are not asserted by any test; with
  the default deadlock_timeout the look is redundant (the short lock_timeout gives way first), so M04 is
  nearly equivalent; M05 and M40 would hold a reader's or a writer's table up to 60 s or 5 s instead of 500 ms,
  and no test measures it. A test: hold ACCESS SHARE on the parent in another session and assert that the
  conversion fails within about one second.
* M26 is equivalent (timepart__eff with the zone UTC is the identity); the argument could be dropped.

### T13-14 (note) Helper functions are executable by PUBLIC

timepart__convert_stmts (returns the text of the DO, checks that the caller owns the table),
timepart__convert_refusal, timepart__am_kind, timepart__columnar_options, timepart__need_columnar and
timepart__special_table are not in the REVOKE list. They give nothing that pg_catalog does not give, with
one exception: timepart__special_table is SECURITY DEFINER and answers whether any table OID belongs to a
ledger or a distributed table, which are catalogs private to their extensions. An unprivileged role
called it with two arbitrary OIDs and got null without error. Fix: revoke from PUBLIC (the callers
are SECURITY INVOKER functions that run as the owner; grant it where needed) or make it STABLE
invoker.

## 8. Not reproduced

* A lost or doubled row with writers and readers in any order I could produce (also by TAP 005 and 008).
* A deadlock victim among the user's sessions.
* An injection through names, comments, defaults or sort columns.
* Any privilege gain through convert_partition for a role that is not the owner.
* A half-converted table after any cancel, timeout, termination, kill or immediate stop.

## 9. Not examined

* kingres_columnar itself (reviewed separately); its behaviour with a type or an encoding other than the
  ones in the fidelity table.
* Tablespaces (no tablespace was made), the standby (TAP 007 passes; I did not add cases), prepared
  transactions beyond TAP 008.
* Two hand conversions of different partitions of one table at the same time (they can collide on the
  swap and waste a copy; not run).
* A partition of several hundred gigabytes, parallel workers during the copy, memory use of the copy.
* The 0.1 -> 0.2 upgrade on a database with policies, history and a running scheduler (the function
  texts are identical; the upgrade regression test passes).
* A foreign key to a columnar partition's parent in the heap direction (kingres_columnar refuses the
  state), exclusion constraints in the heap direction (cannot exist on columnar).
* The frame mode (superuser driving a table of another owner) with hostile owner code beyond what the
  existing tests do.
* pg_dump and restore of a table with converted partitions, and logical replication to a subscriber
  other than the decoding check in T13-03.
* The PostgreSQL 19 runs of the hand-driven checks other than T13-01, T13-02 and the fidelity
  comparison.

## 10. Housekeeping

Private installs, build trees and data directories of this review were removed at the end; the
disk use peaked above 2 GB for a few minutes while a 2.5 million row partition was converted and its
files and logs were kept (the limit of 2 GB was passed, not by much; I noticed it at the end of the
mutant runs and cleaned up at once). The detached worktree was removed after a clean git status; nothing
was committed or pushed.
