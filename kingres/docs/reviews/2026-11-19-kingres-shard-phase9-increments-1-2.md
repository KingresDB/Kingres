<!-- SPDX-License-Identifier: AGPL-3.0-only -->
# Review of kingres_shard 0.7, phase 9 increments 9.1 and 9.2

## 1. Scope and method

Reviewed: the exported tree of the change (extension version 0.7: catalog, `table_change_report`, `create_distributed_table` of a table with rows, `copy_routed.c`, the upgrade script). The diff was read for the C part, the upgrade script and the README. Everything below marked "run" was run on private copies of the 18.7 and 19rc2 assertion builds with the tree built clean with `-Werror` (no warnings on either major; `default_version` is 0.7 in both copies). Clusters of three nodes (one per shard group) plus a twin single node that kept the plain table. Helper scripts live in the reviewer's own area, not in the tree.

Not examined or only partly examined is listed in section 6. In particular the whole TAP suite was NOT run (see 2).

## 2. What was run

| Run | 18.7 | 19rc2 |
|---|---|---|
| Clean build `-Werror`, shard and ledger | pass, 0 warnings | pass, 0 warnings |
| `PG_VERSION_NUM` outside `src/pg_compat.h` (grep) | none | none |
| pg_regress, 18 tests | 18/18 pass | 18/18 pass |
| TAP `t/140`..`t/143` | 140 ran inside the mutant runs, all pass unmutated (443 + tests of 142/143) | pass: 140, 141, 142, 143 (564 tests) |
| TAP `t/046_two_majors` | not run (needs `KINGRES_PG18`) | skipped by the test itself for the same reason |
| Whole TAP suite (about 127 files) | NOT run | NOT run |
| Generated tables (random types, constraints, indexes, grants, rules, sequences, blockers; twin compared) | 188 tables | 121 tables |
| ... of which distributed / refused by the report with a code / failed with no blocker in the report | 71 / 75 / 38 | 62 / 37 / 21 |
| Rows compared (coordinator, each group directly by per-row md5, bucket of each row recomputed) | no difference in any distributed table | same |
| Fault matrix (error at 14 points, kill of the origin at 14 points, immediate stop of node 2 or 3 at 7 points) | 42 runs: always old or new, never half; no shadow, no prepared transaction after `recover_prepared_transactions()`; every old case repeated and succeeded | not repeated |
| Mutants (see 5) | 29 | not run |

Totals on both majors: pg_regress 36 of 36; the change's TAP tests pass on 19 (4 files, 564 tests) and unmutated on 18; the full TAP suite was not run on either major.

The generator covered integers, numeric (with and without modifier, NaN), floats (NaN, infinities, -0, denormals), text with awkward characters, varchar, char, uuid, date and timestamps with infinity, interval, bool, bytea, enums, domains with CHECK, arrays, jsonb, inet, cidr, macaddr, money, composite, ranges, json, point, tsvector, bit varying; NULL keys; 0 to 40000 rows; defaults, stored and (19) virtual generated columns, identity, serial; CHECK, NOT NULL, UNIQUE, PRIMARY KEY, EXCLUDE; expression, partial, INCLUDE, opclass, collation, hash, gin, brin indexes; comments; table and column privileges with grant options and several grantors; RLS; triggers; rules; views; inheritance; publications; reloptions; replica identity; statistics objects; TOASTed values; a dropped column and a later added one; deletes and vacuum full. The comparison with the twin after the call excluded only: column statistics target, storage and compression (reported as warning), replica identity (warning), and the extra default-privilege grants of P9A-07.

Rows were never lost, doubled or changed by a call that started on a quiet table. The one data-loss path found needs a concurrent writer (P9A-01).

## 3. Findings

### P9A-01 (critical) A committed change made by a transaction that was open when the call started is silently lost

Place: `src/dist_table.c`, `kingres_create_distributed_table_body`: the `ExclusiveLock` at line 516 is taken, then `count(*)` (about line 596) and the copy cursor (`copy_routed.c`, `SPI_cursor_open`) read with a snapshot taken before the wait for the lock.

Steps (READ COMMITTED, both majors):
```
A: CREATE TABLE lu(id int primary key, v text); INSERT ... 10 rows 'orig';
B: BEGIN; UPDATE lu SET v='UPDATED' WHERE id<=3;          -- not committed
A: SELECT kingres.create_distributed_table('lu','id');    -- waits for the lock
B: COMMIT;
A: (call returns, no error)
A: SELECT count(*), count(*) FILTER (WHERE v='UPDATED') FROM lu;   ->  10 | 0
```
The three committed updates are gone. Second variant: B deletes id 1 and inserts id 100, commits during the wait: after the call `count=10`, row 100 is missing, row 1 is back (`10|0|1`). Run on 18.7 and 19rc2, same result.

Why the check does not catch it: the count taken "under the lock" uses the same old snapshot, so rows read, rows stored and count agree. With only an INSERT in B the counts differ (11 against 10) and the call fails with "the copy of table is not complete" (safe); updates, deletes and delete+insert pairs are silent. The old table is dropped in the same transaction, so the committed data is destroyed.

A fix must achieve: everything read after the lock is granted (count and copy) sees all changes committed before the lock was granted (a snapshot taken after the lock, with the lock held through the commit), or the call refuses; and a test that has a writer commit during the wait and checks values, not only counts.

### P9A-02 (high) A statement queued behind the call runs after the swap as if the table were plain, and its effect stays on one node

Place: the statement's utility hook decision is made before the wait for the table lock; the call swaps the table meanwhile.

Steps (run, 18.7; the call held at `distribute_after_shadow`):
```
ALTER TABLE q RENAME COLUMN v TO w;     -- queued on the old table
ALTER TABLE q ADD COLUMN extra int;     -- queued
```
After the call both succeed and nodes are `cols=id,w` / `cols=id,v` / `cols=id,v` (and `id,v,extra` on node 1 only): the distributed table has a different definition on the groups. Also: a `COPY ... FROM STDIN` queued behind the call fails with "no partition of relation found for row" on the new table (safe, nothing written); a queued `INSERT` worked and went to the right bucket; queued `UPDATE`, `DELETE`, `SELECT FOR UPDATE`, `TRUNCATE`, `VACUUM FULL` behaved (the last only warned). A queued `DROP TABLE` was refused by the distributed-table guard (good).

Why it matters: silent schema divergence between nodes of a distributed table, which later moves, adds nodes and scans will meet.

A fix must achieve: a statement that waited for the table's lock re-decides, after the lock, whether the table is distributed (as the guard does for DDL), or the call takes the lock in a way that refuses queued DDL; plus a test with DDL and COPY queued behind the call.

### P9A-03 (high) Rules on the table are neither refused nor carried

Place: `table_change_report`, kind distribute: the dependent-object query excludes rules whose `ev_class` is the table itself (`rw.ev_class <> relation`), and `internal_table_ddl` does not carry rules.

Steps (run, both majors; generator seeds 133 and 138, and a case `CREATE RULE r_audit AS ON INSERT TO t DO ALSO INSERT INTO audit ...`): report says nothing, the call succeeds, `pg_rules` of the table is empty afterwards on every node; the twin keeps the rule. An `INSTEAD NOTHING` rule that protected rows, or an audit rule, disappears without a warning. The README lists neither rules among the refused things nor among the things not carried.

A fix must achieve: a rule on the table is a blocker (or is carried on every node), and the README says which.

### P9A-04 (medium) The report promises a call that then fails (or fails late); refusals that happen after the copy

The brief's rule: a call the report does not block must succeed, and a refusal must come before anything is changed. Found by run, all rolled back correctly (old table intact, nothing left), but not predicted by the report:
- A column of another table has the table's row type: the call fails at `DROP TABLE` after the whole copy, with the writers' lock held (generator seeds 117, 129, 136, 160 and others; both majors). The report checks functions on the row type, not columns of other tables.
- A `NOT VALID` CHECK constraint with violating rows: the copy fails at the first violating row (`new row ... violates check constraint "nv"`), because the constraint is recreated validated on the shadow.
- A column default that calls `nextval` of a sequence that no column owns: fails on node 2, "relation ... does not exist".
- A custom collation that exists only on the origin: fails on node 2.
- An owner that has `CREATE` on the schema only on the origin (`GRANT ... ON SCHEMA`): fails on the other nodes with "permission denied for schema" (about 30 of the generated tables on the two majors; the schema is created by `CREATE SCHEMA IF NOT EXISTS` without grants).
- An enum as the distribution column: the hash of an enum value depends on the enum's OID, which differs between nodes created separately; the call fails with "no partition of relation found for row" (generator seeds 109, 113, 132, run on both majors). The report has no code for it, and the error does not say why. If such a table were completed (for example with all rows hashing to a bucket another node owns) the placement would be wrong; here the router's destination check refused it.
- `name_in_use` looks only at the origin: a relation named `kingres_shadow_<oid>` on another node, or a sequence named `kingres_shadow_<oid>_c1` on the origin, is not reported and the call fails.

A fix must achieve: each of these is a report row with a code (checked on every node where it matters), or the call proves it before the copy starts; the "enum" case is a blocker or the hash is made portable.

### P9A-05 (medium) The call runs code of the table's owner as a superuser

Run: role `mallory` (no privilege) owns `mal.t2` with `CHECK (mal.f(v))` and `mal.f` logs `current_user`. A superuser calls `create_distributed_table('mal.t2','id')`: the log shows the function ran as the superuser on every node (the copy evaluates the check on the shadow). The design's Security section asks to verify a rule for this; the code sets no role and no `SECURITY_RESTRICTED_OPERATION`. A superuser who distributes a table of an untrusted owner executes the owner's code with superuser rights on all nodes. (Same trust as `pg_dump | psql` by a superuser, but here it is on every node and the README does not say it.)

A fix must achieve: the copy and the constraint checks run with the privileges of the table owner, or the README states that only tables of trusted owners may be distributed.

### P9A-06 (low) The state of a serial sequence is not the state the table had

Run (18.7 and 19): a serial column whose sequence was `setval(seq, 3, false)` (or 50 calls with rows deleted) ends at `max(column)` with `is_called = true` (`48|t` against `3|f` before). The next value of the distributed table differs from the twin (`5000049|49` against `5000049|3`). No duplicate is produced (the value is above every existing row on that node), and the design says each node's sequence is at least the largest value of its rows; but "sequences' next values" differ, and the README says "with their state".

A fix must achieve: either the state is kept on the origin (not advanced past the old state unless a row needs it) or the README says it is advanced to the maximum of the column.

### P9A-07 (low) Things lost or changed without a warning

Run, both majors: attribute options (`ALTER COLUMN SET (n_distinct=100)`) are lost, no warning; an invalid index (`indisvalid = false`) becomes a valid one built on the data; `FORCE ROW LEVEL SECURITY` on a table without RLS enabled is dropped; default privileges of the schema (`ALTER DEFAULT PRIVILEGES ... GRANT ... ON TABLES`) give the new table extra grants in the shadow that the old table did not have (when the owner has default privileges, the new table gets them in addition to the carried ACL: seen as `ADDED acl ...` lines, 8 privileges to a role on a table that had one grant). Column statistics target, storage, compression, `CLUSTER ON` and replica identity are warned as documented.

A fix must achieve: the carried ACL is exactly the old ACL (reset the default grants), and each loss is a `not_carried` warning.

### P9A-08 (note) Behaviour of the call inside transactions

Run: a call inside an explicit transaction that is rolled back, inside a rolled-back savepoint, and inside a `DO` block with an exception handler leaves the old table, rows and no leftover on any node; the table is usable in the same transaction after the call (select, insert, update). `PREPARE TRANSACTION` after the call is refused with "cannot PREPARE a transaction that has operated on remote nodes". In `REPEATABLE READ` the call fails with a serialization error on `kingres.map_version` if another session wrote since the snapshot (safe). A long `REPEATABLE READ` reader that started before keeps reading the old table until it ends: the call then waits at the swap and ends with a lock timeout when `lock_timeout` is set (checked at 5 s); without a timeout it waits for the reader. Readers do not block during the copy; a reader that starts after the swap waits until commit (checked with the fault points of the TAP test). Prepared statements, a held cursor and cached plans that used the table before the call work after it. Two simultaneous calls on the same table: one wins, the other fails ("relation with OID ... does not exist"); two calls on two tables, no deadlock. A foreign key between plain tables in either order is refused with the report's code in both orders; keys to reference tables are carried and validated (violating insert is refused from every node).

### P9A-09 (note) Cost of the lock, memory, WAL, cancel (run, 18.7)

- Table of 3,000,000 narrow rows (297 MB with the primary key): 10.3 s with writers waiting; 500,000 rows 3.9 s; 2,000 rows of 300 kB (624 MB): 13.6 s. Writers wait for the whole copy, linear in the size.
- Memory: origin backend peak 163 to 181 MB, remote backends 85 to 164 MB, independent of the row count (600,000, 3,000,000 and the wide table within 20 MB of each other); under the 2 GB limit.
- WAL (sum over three nodes): 2.2 times the table size for the narrow table with an index, 1.04 times for the wide table.
- `pg_cancel_backend` during the copy ended the call at once with "canceling statement due to user request" and the old table intact; `statement_timeout` (1.5 s on 300,000 rows) and `lock_timeout` likewise; `work_mem = 64kB` made no difference.
- Stopping node 2 or 3 at any phase: the call fails after the node timeout (about 67 s), old table back; stopping a node after the prepare gave "new" after recovery.

### P9A-10 (low) Test gaps (see section 5)

Seven of 29 mutants survive all of the change's tests (including `t/140`, `t/142`, `t/143`): see the table. None of the tests has a writer committing during the wait for the lock, a queued DDL, or a rule, which is why P9A-01 to P9A-03 were not found by the author's tests.

## 4. Other checks

- Security (run): the call and the report are refused to a role with only INSERT, to the owner and to a member of the owning role ("permission denied for function"); only superusers may call. No new function is `SECURITY DEFINER` (only `forget_dropped_tables`); every function of the change has a fixed `search_path`; the new functions are not executable by PUBLIC. `table_operations`, `partitions` are readable by `pg_monitor` only; the node-local operation tables are closed to others. Hostile identifiers (quotes, a semicolon and `DROP TABLE` inside a table name, dots, newlines in comments, a role with a quote, 63-byte names) were distributed with identical definition and rows (the quoting is right); a 58-byte table name is refused with the report's code. A table of the shadow's name owned by another role is reported (`name_in_use`) and refused before anything changes; a hostile `search_path` of the caller changes nothing (the call refused a table it could not resolve before any change).
- Row-level security: tables with RLS or policies are refused (`row_level_security`), so no row can be hidden from the copy by policy.
- Version handshake (read, plus the TAP mutants C10 and S12 killed by `t/140` and `upgrade`): a node with 0.6 is refused naming node and versions.
- Upgrade (run): the `upgrade` pg_regress test passes on both majors (the comparison covers columns, defaults, constraints, indexes, views and their definitions, functions with body digests and ACLs, triggers, event triggers, sequences, comments, privileges, membership, dumped configuration). Because both scripts are mutated together in my mutants, the test compares the two paths and cannot notice a change made to both (S09 to S17 are examples of what it is not for). Mixed 0.6/0.7 operation, `pg_dump`/`pg_restore` of a database with 0.7 and a restore's colocation ids: not run (see 6).
- Colocation ids (run in `upgrade`): never reused after the delete of a group; mutant C12 (max+1) is killed by `upgrade`.

## 5. Mutants

Each mutant is one change (SQL mutants in both `--0.7.sql` and `--0.6--0.7.sql`). Tests: the 8 pg_regress tests of the change (`version catalog distribute distribute_rows security reference upgrade pg_facts`), and for survivors `t/140`, `t/142`, `t/143`. Builds were clean.

| Id | Mutant | Killed by |
|---|---|---|
| C01 | count compare removed (`nread != nrows` never true) | t/140 |
| C02 | lock mode `ExclusiveLock` -> `RowExclusiveLock` | t/140, t/143 |
| C03 | `overriding_system_value = false` in the copy | distribute_rows |
| C04 | `check_identity = false` in the copy | SURVIVES (redundant or untested: the router's destination check is not exercised by a wrong placement) |
| C05 | `lc_monetary = C` on the origin side of the copy removed | SURVIVES (no test copies money under another `lc_monetary`) |
| C06 | foreign keys not added on the origin | distribute_rows |
| C07 | foreign keys not added on the other nodes | t/140 |
| C08 | old table renamed instead of dropped | distribute, distribute_rows, security, reference, upgrade |
| C09 | `internal_ddl_leave()` not called after the keys | SURVIVES (no test runs a statement after a call that added keys in the same session) |
| C10 | version handshake removed from the call | t/140 |
| C11 | `require_current_sql_objects()` removed | upgrade |
| C12 | colocation id by `max+1` instead of the sequence | upgrade |
| S01 | owner not carried | distribute_rows |
| S02 | table privileges not carried | distribute_rows |
| S03 | column privileges not carried | distribute_rows |
| S04 | comments not carried | distribute_rows |
| S05 | sequence state not advanced | distribute_rows |
| S06 | `GENERATED ALWAYS` identity made `BY DEFAULT` | distribute_rows |
| S07 | report: RLS refusal removed | distribute_rows |
| S08 | report: trigger refusal removed | distribute, distribute_rows, reference |
| S09 | report: exclusion constraint refusal broken | SURVIVES (cause not established: the test may not reach this branch, or the branch is redundant) |
| S10 | report: dependent view refusal removed | distribute_rows |
| S11 | report: tablespace refusal removed | distribute_rows |
| S12 | handshake accepts any version | upgrade |
| S13 | applying a catalog does not advance `colocation_id_seq` | SURVIVES (missing test: a node that becomes map group primary after a catalog sync) |
| S14 | copy function revoked from `kingres` instead of PUBLIC | security |
| S15 | bucket move not refused in a claimed group | t/140 |
| S16 | `colocation_id_seq` not revoked from PUBLIC | SURVIVES (missing test: `nextval` by a role without privilege; the security test lists the sequence only through the functions) |
| S17 | privileges: owner revoked but PUBLIC not | SURVIVES (missing test: a table that had a grant to PUBLIC) |

Twenty-nine mutants, twenty-two killed, seven survivors (C04, C05, C09, S09, S13, S16, S17).

## 6. Not examined, or only partly

- The whole TAP suite on either major (about 75 minutes per major each was not run; only the change's tests, 046 skipped for lack of a second installation).
- Mixed 0.6/0.7 clusters upgraded node by node, `pg_dump`/`pg_restore` and a `pg_upgrade`-style dump, and a restore's colocation ids: not run.
- Crash (immediate stop) of a group at each phase: only the immediate stop at 7 phases for nodes 2 and 3 and `kill` of the origin at 14 phases were run; kill of a backend on a group was not.
- Out-of-memory with a memory-context limit (the build has none that was set); disk-full.
- Bucket move, rebalance, `add_node`, `promote_node` at the same time as a call: one bucket move was attempted while a call was held, but the move was refused by `wal_level = replica` first, so the exclusion of moves and calls was only checked through the TAP test 140 (mutant S15 is killed by it).
- Concurrent `VACUUM`/`ANALYZE` during the copy: the sessions did not return within the 20 s window (the call was held at a wait gate), then the call succeeded; whether they waited on the table lock was not distinguished.
- Foreign keys: a key to a colocated distributed table (the notes say it needs one group), composite, deferrable and `MATCH FULL` keys were not generated; `NOT VALID` keys to a reference table were reported `foreign_key_plain_table` in my case because the target was a plain table.
- Virtual generated columns on 19 were generated but the generator seldom drew them; no difference was seen.
- The README states what the call does to readers and writers and what is carried; it states nothing about rules, attribute options, invalid indexes, or the snapshot behaviour of P9A-01, and says "keeps the state" of sequences (P9A-06).
