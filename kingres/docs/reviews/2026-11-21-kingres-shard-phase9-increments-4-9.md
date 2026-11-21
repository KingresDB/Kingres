<!-- SPDX-License-Identifier: AGPL-3.0-only -->

# Review of kingres_shard phase 9, increments 9.4, 9.5, 9.8 and 9.9

Reviewer: independent (did not write the code). Tree under review: branch kingres-18 at 8a9843c475, extension version 0.7.
Increments: 83efaba126 (9.4: `undistribute_table`, `alter_distributed_table`, and the online `_concurrently` forms),
b6397c18b0 (9.5: user-partitioned tables as distributed tables, the four partition functions, `partition_leaves`, the bucket
move of a tree, queries) and 8a9843c475 (9.8: `kingres_shard.dependent_views = recreate`; 9.9: storage parameters carried).
Nothing in the tree was changed. All builds, experiments and mutants ran in private installs and a private worktree.

## Scope and method

Read: `design/shard-phase9.md` (the sections for 9.4, 9.5, 9.8, 9.9 and the dated notes at its end), the extension README
sections for these increments, the two earlier reviews of phase 9 (`2026-11-19-...-increments-1-2.md`, `2026-11-20-...-rebuild.md`),
the 0.7 SQL for `undistribute_table`, `alter_distributed_table`, the node-local functions of the online forms
(`internal_switch_attempt_dist`), the four partition functions and `internal_leaf_op`, the view capture/drop/recreate functions,
`internal_reloptions_of`/`internal_with_clause`, `check_cluster`'s partition check, and the C changes (`utility.c` partition
statement mapping, `dist_table.c` partition tree and storage parameters, `bucket_move.c` leaf rename, `rebuild.c`).

The lessons of the earlier reviews were applied to each new call: (1) a writer that commits during the call must not lose its
change; (2) code of an untrusted role must not run as superuser; (3) exact keys in the capture; (4) the DDL guard.

Method, in this order:

1. Clean build with `-Werror` of kingres_shard, kingres_sketch, kingres_timepart and kingres_columnar on PostgreSQL 18.7
   and 19rc2, into private copies of the two installs; the whole phase 9 test set t/185 to t/196 on both majors, and the
   pg_regress suite of kingres_shard on both.
2. Throw-away TAP scripts (private, not in the tree), about twenty, each a question of the brief; a suspicion is a finding
   only when a script showed it. Every finding below was reproduced on both majors unless the text says otherwise.
3. Mutants of the new SQL and C code, one line each, clean build and install into a separate private copy, judged by the
   phase 9 tests that cover the line.

## What was run

| What | PostgreSQL 18.7 | PostgreSQL 19rc2 |
|---|---|---|
| Clean build `-Werror` of the four extensions | 0 warnings | 0 warnings |
| t/185 to t/196 (12 files), one run each | pass, 1883 tests, 210 s | pass, 1883 tests, 208 s |
| pg_regress of kingres_shard | pass | pass |
| Experiment scripts X1 to X21 (below) | all | X1, X3, X5, X8, X10, X14, X18 repeated: same results |
| Mutants (below) | 22 | not run |

All clusters were three nodes, one shard group each, ports 59100 to 59199 only, at most three test processes at a time.

Experiments (each is one TAP file, 1 to 12 clusters in a run):

| Id | Question | Result |
|---|---|---|
| X1, X18 | caller in REPEATABLE READ / SERIALIZABLE, a writer commits between the snapshot and the lock | rows lost: P9C-01 |
| X2 | create_partition under another TimeZone; detach_partition and drop of the parent | bounds equal on every node; privileges lost on detach: P9C-10 |
| X3 | default privileges of the superuser and recreated views; hook statement details; hostile partition names | P9C-05, P9C-09, P9C-11, P9C-12 |
| X4 | six writers on three nodes, each transaction touching two nodes (a data row and a counter row, kept equal by the writer), while: online alter (bucket count), online alter (distribution column), blocking alter, blocking undistribute, online undistribute | invariants held in all five (below) |
| X5 | untrusted function reachable through a cascade member | P9C-04 |
| X6 | kill of the coordinator at `after_prepare`, `after_log`, `after_commit`, `between_commit_prepared` for undistribute, alter, create_partition, detach_partition | all-or-nothing after recovery |
| X7 | matrix of 7 alterations (bucket counts 8, 12, 24, 5, 7; distribution column int, text, date, bigint), blocking and online, 3000 rows with NULLs, NaN, quotes | online: all 7 right (digest, placement); blocking: P9C-02 |
| X8 | attach_partition of a table with its own CHECK, indexes, owner and ACL, then a bucket move | P9C-03 |
| X9, X10 | blocking alter and undistribute with 100 to 10000 rows | P9C-02 |
| X11 | kill of the process at 13 points of the online engine, for online alter and online undistribute (26 runs), then recovery | all undone or finished, no leftover |
| X12 | alter colocate_with a group whose bucket is moving | P9C-06 |
| X13, X14 | `pg_dump -s` of a table with 20 kinds of object before and after distribute, alters, undistribute | P9C-07, P9C-08 |
| X15 | non-superuser roles against the new functions and the hook; views on partitions; DDL on a partitioned distributed table, tree compared on three nodes | refused; tree identical |
| X16 | hostile identifiers in schema, table, column, index, role, view, comment, through all calls, blocking and online, with views recreated | held |
| X17 | privileges on the retained old copies of an online operation | held |

## Findings

Severities: critical (data lost or exposed silently on a supported use), high (a documented call does not work or an integrity
guarantee is lost), medium, low, note. Counts: 1 critical, 2 high, 4 medium, 5 low, 2 notes (14 in all).

### P9C-01 (critical): `undistribute_table` and `alter_distributed_table` lose committed rows when the caller's transaction is REPEATABLE READ or SERIALIZABLE

Place: the 0.7 SQL of `kingres.undistribute_table` and `kingres.alter_distributed_table` (neither checks the isolation level;
the online engine's `table_operation_advance` and the C `create_distributed_table` do, P9A-01).

Steps (script X18, also X1):

1. `CREATE TABLE t (tenant int, id int, v int, PRIMARY KEY (tenant, id))`, 100 rows, `create_distributed_table('t', 'tenant', bucket_count => 4)`.
2. Node 2: `BEGIN; INSERT INTO t SELECT g % 7, g, g FROM generate_series(1001, 1050) g;` (left open).
3. Node 1: `BEGIN ISOLATION LEVEL REPEATABLE READ; SELECT kingres.alter_distributed_table('t', bucket_count => 8); COMMIT;` The call waits for the table lock.
4. Node 2: `COMMIT`.

Result: the call returns without an error; `count(*)` is 121, not 150: 29 committed rows are gone. The same with
`undistribute_table`. The cause: the call's snapshot predates the writer's commit; the rows of the local node's partitions are
read through that snapshot (the other nodes' branches take fresh ones), the count that the copy is compared with is taken through
the same snapshot, so the "rows read equal rows stored" check cannot see the loss; then the old table is dropped. Variants X1 (a
writer commits between the first statement and the call, with updates and deletes too) lose and change rows the same way, for both
isolation levels and both calls. Mutant S04 shows that the count comparison is redundant in the other cases.

Why it matters: a superuser or a role with `default_transaction_isolation = 'repeatable read'` hits it with an ordinary
single-statement call and one slow writer; nothing says that rows were lost, and the old copy is dropped in the same transaction.

A fix must: refuse any isolation level other than READ COMMITTED at the entry of both blocking calls (as the earlier fix does for
the create call and for `table_operation_advance`), before the lock; and compare the copy with a count taken under the lock by a
statement that takes a fresh snapshot. Add the X18 interleaving to t/185 and t/186.

### P9C-02 (high): blocking `alter_distributed_table`, and `undistribute_table` with a `target_group` other than the map group, fail for tables of more than a few thousand rows

Place: the copy of both calls, `internal_copy_routed` on a distributed source (`copy_routed.c`) followed by `DROP TABLE` of the old partitions in the same transaction.

Steps (scripts X9, X10, X7): a distributed table with 8 buckets and N rows (`tenant int, id int, v text`, PK on both),
`alter_distributed_table(t, bucket_count => 12)`.

- N = 100, 600, 1000, 1100, 2000: ok.
- N = 2400, 2600, 2800, 3000, 10000: `ERROR: cannot DROP TABLE "kingres_old_NNN_b0005" because it is being used by active queries in this session` (remote node context). The call is rolled back, nothing changes.
- `undistribute_table(t, target_group => 2)` with 6000 rows: `ERROR: cannot DROP TABLE "w1_b0002" because it is being used by active queries in this session`. With the default target (the map group), `undistribute_table` of 10000 rows works.

The node logs show the cause: the remote scan cursor `ks_scan_N` that the copy reads from the last node is fetched
(`FETCH FORWARD 1000`) but not closed before the `DROP TABLE` on that node; the failing sizes are those where the map group's node holds more
than `kingres_shard.insert_batch_rows` (1000) rows (2400 rows: 1371 / 343 / 686 per node), which fits the threshold between 2000
and 2400 rows; I did not isolate the exact condition.

Why it matters: the blocking alter and the undistribute to another node, the two calls that 9.4 adds, do not work on any table of
real size. They fail closed, so no data is lost, which is why the suite (tables of 100 to 600 rows) does not see it. The online
forms are not affected.

A fix must: finish and close the remote cursors of the copy (or end the portal's executor) before the old tables are dropped;
add tests with more than two batches per node for blocking alter, undistribute to another group, and the reference-table copy.

### P9C-03 (high): `attach_partition` accepts a table with its own constraints and indexes; a later bucket move silently drops them

Place: `kingres.attach_partition` (no `partition_local_object` check) and the bucket move of a tree (`internal_move_inspect`, `internal_create_move_tables`; the leaf made on the target lacks what the leaf had; I did not trace which of the two builds it).

Steps (script X8): `p` (range partitioned, distributed, 6 buckets); `t2` with `CHECK (qty >= 0)`, a unique index, a second index, owner `own1`, `GRANT SELECT TO r1`, distributed in the group of `p`; `attach_partition('p', 't2', 'FROM (...) TO (...)')`. Leaf `t2_b0000` on node 1: 1 check, 3 indexes, owner `own1`, ACL for `r1`. `move_bucket(group, 0, 1)`; the same leaf on node 2: 0 checks, 1 index (the inherited key), owner the superuser, no ACL.

Why it matters: the attach, unlike the create, does not apply the `partition_local_object` rule; the move then loses a CHECK
constraint and indexes of exactly the partition that had them: an integrity guarantee disappears at a routine rebalancing, with no
error and no warning, and the leaves of one logical partition differ from their siblings (the design says all leaves have the
definition of the parent). Same result on both majors.

A fix must: refuse the attach (the blocker `partition_local_object`, naming the object) or carry the leaf's own objects, owner and
ACL through the move, and have `check_cluster()` compare the leaves of a partition with the parent.

### P9C-04 (medium): the untrusted-code check names one table; the other tables of a cascade run the code of a non-superuser as superuser

Place: `table_change_report` for kinds `undistribute` and `alter` (`internal_untrusted_functions(relation)` only), and the copies of `cascade_via_foreign_keys` and `cascade_to_colocated`.

Steps (script X5): tables `a` and `b` in one colocation group; `b` has `CHECK (public.evil(id))`, where `evil` belongs to a role who is not a superuser and writes `current_user` to an audit table. `table_change_report('alter', 'b', ...)` says `function_owner_not_superuser`. `table_change_report('alter', 'a', {"bucket_count":8,"cascade_to_colocated":true}')` says nothing, and `alter_distributed_table('a', bucket_count => 8, cascade_to_colocated => true)` runs and the audit table shows the function ran 50 times, as the superuser, on every node.

Why it matters: the rule of the second review (P9B-01) holds for the table the caller names and not for the tables the call takes with it. (In my clusters a non-superuser owner cannot add such a constraint to a distributed table, because the bucket partitions belong to the creating superuser, so the exposure needs a superuser to add the constraint, or a table made before it was distributed; the check exists because the design does not rely on that.)

A fix must: run the check for every table of the set or component, name the table in the blocker, and test it with a cascade member.

### P9C-05 (medium): recreated views get the superuser's default privileges

Place: `internal_view_recreate` (`CREATE VIEW` as the caller, `REVOKE ALL ... FROM PUBLIC` only when the view had an ACL).

Steps (script X3a): a view `vv` owned by `vown` with no ACL (owner only); as the superuser `ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT SELECT ON TABLES TO r1`; `SET kingres_shard.dependent_views = recreate; SELECT kingres.alter_distributed_table('vt', bucket_count => 8)`. Afterwards `relacl` of `vv` is `{vown=arwdDxtm/vown,r1=r/vown}` and `r1` can read the view; before the call it could not. (The table itself is handled: its ACL is reset by `internal_reset_acl`.)

Why it matters: a read privilege appears on a view that did not have it. Mutant S10 (no `REVOKE ALL ... FROM PUBLIC`) survives, so no test covers the reset. Related and smaller: grants are recreated by the owner, so the grantor of a grant made by another role with grant option is lost, and an owner who had revoked their own privileges gets them back.

A fix must: reset the new view's ACL completely (as for tables) before applying the captured entries, whether or not the old view had an ACL, with a test that sets default privileges.

### P9C-06 (medium): `alter_distributed_table(colocate_with => ...)` does not check the target group for a move or an operation

Place: `table_change_report` kind `alter` (the busy check is made for the table's own group only); the `'default'` and explicit forms of `alter_distributed_table`.

Steps (script X12): tables `t1`, `t2` in separate groups of the same placement; `move_bucket` on `t2`'s group is held (`move_after_catchup:wait`); the bucket is `moving`. `alter_distributed_table('t1', colocate_with => 't2')`: the report says nothing and the call succeeds. The move then fails at its switch (`relation "public.t1_b0001" does not exist`) and is undone. Counts stayed right in my run, but a long move was aborted by a catalog-only change, and a table joined a group that was changing under it. The design says the catalog-only cases check "that no bucket of either group is moving and that no operation claims either".

A fix must: apply the group check to the target group (and to every group a `'default'` search picks), and test it with a held move.

### P9C-07 (medium): the online forms fail late on a table with a DEFERRABLE unique constraint

Place: the replay and copy of the engine (`INSERT ... ON CONFLICT` without a conflict target), reported by neither report nor README.

Steps (script X13): a table with `UNIQUE (a, b, g) DEFERRABLE INITIALLY DEFERRED`; `alter_distributed_table_concurrently(...)` and `undistribute_table_concurrently(...)` fail with `ON CONFLICT does not support deferrable unique constraints/exclusion constraints as arbiters` after the operation has started (it is undone; the blocking calls work on the same table).

A fix must: refuse in the report (`deferrable_unique` blocker for the online calls) or name the arbiter index.

### P9C-08 (low): README says `toast.*` storage parameters are carried; they are not

Place: README, "Storage parameters (increment 9.9)": "(`fillfactor`, `autovacuum_*`, `toast.*`, ...)".

Steps (script X14): `CREATE TABLE ... WITH (fillfactor = 70, toast.autovacuum_enabled = false, toast_tuple_target = 256)`; the report's note names `fillfactor=70, toast_tuple_target=256`; after `create_distributed_table`, an alter and `undistribute_table`, the TOAST table has no options (originally `{autovacuum_enabled=false}`). The options of the TOAST relation are in the TOAST table's `reloptions`, not in the table's; `internal_reloptions_of` reads the table's. Nothing warns that they are not carried.

A fix must: read and apply the TOAST table's options too, or state the limit in the README and the report (warning `not_carried`).

### P9C-09 (low): the statement mapping ignores parts of the statement it does not refuse

Place: `map_partition_statement` (`utility.c`).

Steps (script X3b): `CREATE TABLE other.pp2 PARTITION OF public.pp FOR VALUES ...` creates the leaves of `public.pp2` (the schema of the new partition is ignored) and `DROP TABLE other.pp2` then says the table does not exist; `CREATE TABLE IF NOT EXISTS public.pp1 PARTITION OF ...` for an existing partition raises "already exists" instead of a notice; `UNLOGGED` is accepted and ignored. The cast in a bound is written back with `TypeNameToString` (type modifiers and quoting are lost, so `5.7::numeric(3,0)` is written `5.7::numeric`).

A fix must: refuse what the statement says and the mapping does not carry (another schema, `IF NOT EXISTS` handled as PostgreSQL does, a persistence other than permanent), and write casts with their typmod and quoted names, or refuse them.

### P9C-10 (low): `detach_partition` does not give the new table the privileges and comment of the table

Steps (script X2b): `p` has `GRANT SELECT TO r1` and a comment; after `detach_partition(p, m1)` the new distributed table `m1` has the owner of `p` but no ACL and no comment; `r1` cannot read `m1`. The README says "a hash parent made like the table"; the design says owner and privileges. Access is lost, not widened.

### P9C-11 (low): `check_cluster()` reports a missing leaf for a partition name that contains a double quote

Steps (script X3b): `create_partition(p, 'a"b''c', ...)` creates the leaves; `check_cluster()` then reports `dist_partition_mismatch` for `a"b'c_b0000` that exists (`internal_check_partitions` strips quotes from a quoted leaf name with two regular expressions; a name with a dot is mishandled the same way by reading, not run).

### P9C-12 (low): `create_partition` and `attach_partition` refuse a bound written in lower case

Steps (script X3b): `create_partition(p, 'lc', 'from (''2027-03-01'') to (''2027-04-01'')')` fails with "invalid partition bound"; `FROM`, `IN`, `WITH` and `DEFAULT` must be upper case. The statement form is not affected (it writes upper case).

### P9C-13 (note): availability of the switch and of the lock loop under saturating multi-node writers

Script X4 with six writers, each transaction on two nodes, no pause between transactions: both online alters failed to switch in 61 to 64 s ("attempt 30: canceling statement due to lock timeout") and were undone, nothing lost; the blocking alter waited more than three minutes for its locks in distributed deadlocks (the alter holds `EXCLUSIVE` on a node while a writer that holds locks on the next node waits for it), until `detect_distributed_deadlocks()` was called by hand (in my cluster the background worker did not do it; this is not shown to be a defect). With 30 ms between transactions all five calls finished, with the invariants intact. The README says writers wait; it does not say how long a call can wait.

### P9C-14 (note): smaller observations

- `create_distributed_table` of a partitioned table that has a view on one of its partitions fails with PostgreSQL's raw "cannot drop table" error; the report says nothing (X15). The suite's refusal text for the same case with a view on the table itself is good.
- `dist_partition.bound` keeps the bound as the user wrote it (`FROM ('2026-03-01')`), which depends on the session's TimeZone for timestamptz columns; the leaves themselves are right on every node (X2a), so the text is informational only.
- The retained old copies of an online operation double the disk use for `table_cleanup_delay` (10 minutes by default); their ACL is reset to the owner (X17), so no role keeps access through them.
- Reference table undistribute does not compare the copies first (documented in the README).

## What held

- Crash and recovery (X6, X11): for the blocking calls, `undistribute_table`, `alter_distributed_table`, `create_partition` and `detach_partition`, a kill of the coordinator at `after_prepare` and `after_log` leaves every node as before, and at `after_commit` and `between_commit_prepared` leaves every node in the new state after `recover_prepared_transactions()`; no prepared transaction, shadow or old table remains; `check_cluster()` clean. For the online calls, a kill at each of 13 points of the engine (capture, copy, replay, switch before and after each swap, verify, cleanup), followed by `resume_table_operations()`, ends in the old table (operation `failed`, triggers removed on every node) or the new one (`done`), with the same digest and no leftover. (The first resume needs the operation to be older than `2 x node_timeout`; documented.)
- Writers (X4): online alter of bucket count, online alter of the distribution column, blocking alter, blocking undistribute and online undistribute, under six writers on three nodes whose transactions kept a counter row equal to the number and sum of their rows: after each call every counter equalled its rows, no key was duplicated, every row was in the bucket of its key on its node. Writers on other nodes got `relation does not exist` after an undistribute and wrote nothing.
- Alteration matrix (X7): blocking and online, bucket counts that are not multiples of each other, a distribution column of type text, date, bigint and int, keep the digest of 3002 rows (including NULL, NaN, a text with quotes and an umlaut) and every row in its bucket partition on its node (online: all seven; blocking: see P9C-02).
- Definitions (X13): after create, alter and undistribute, `pg_dump -s` of the table shows the same columns, defaults, generated column, constraints, indexes (partial, expression, `INCLUDE`, `NULLS NOT DISTINCT`, fillfactor), owner, ACL (column and grant option), comments and table `fillfactor` / `autovacuum_enabled`. What is lost (replica identity, column statistics target, storage, `n_distinct`, `CLUSTER ON`, extended statistics, TOAST options) is warned for at distribute (`not_carried`) except TOAST options (P9C-08).
- Hostile names (X16): schema `S p" q`, table `T;--x`, column `c;--`, index, role `r;x'y`, view and comments with quotes and SQL text went through distribute, alter (both forms), undistribute (both forms) and recreated views with the digest, ACL and comments intact; leaves of partitions named `a"b'c` and `x; drop table public.pp` were made and dropped with no injection.
- Roles (X15): every new function and the statement mapping refuse a role that is not a superuser (`permission denied`), including the owner of the table; the state setting `dependent_views` gives no right.
- Tree (X15, t/190, t/191, t/193): after `create_partition`, an index, a new column, a check constraint and a second `create_partition`, the leaves of every node have the same index and check count, columns and ACL; the detach and the move keep the leaves.
- Retained old copies (X17): not readable by a role whose access was revoked after the operation.
- DDL guard, floats and capture keys: covered by t/187 and t/188, which pass; not re-attacked.
- The refusals of the report that I exercised (busy own group, `foreign_key_linked`, `foreign_key_colocation`, `dependent_object`, `not_available_yet` for a user-partitioned table) hold.

## Mutants

Each mutant is one line of the SQL or C text; clean build and install into a separate private install; judged by the phase 9 tests that cover the line (named). Killed = a test failed.

| Id | Change | Judged by | Result |
|---|---|---|---|
| S01 | undistribute: `EXCLUSIVE` lock on the table becomes `ROW EXCLUSIVE` | t/185 | killed |
| S02 | alter: the same | t/186 | killed |
| S04 | undistribute: the check "rows read = rows stored" removed | t/185 | **survived** (the check cannot fail in that code; see P9C-01) |
| S05 | alter: the retirement of the old partitions removed | t/186 | killed |
| S06 | placement for a bucket count that is a multiple of the old one: condition false | t/186, t/188 | killed |
| S07 | partition functions: refusal when an operation holds the group removed | t/190 | killed |
| S08 | create_partition: owner of the leaf not set | t/190, t/189 | killed |
| S09 | views dropped first-first instead of last-first | t/195 | killed |
| S10 | recreated view: `REVOKE ALL ... FROM PUBLIC` removed | t/195 | **survived** (P9C-05) |
| S11 | the `;`, `--`, `/*` refusal of a bound removed | t/190 | killed |
| S13 | the untrusted-function check for undistribute and alter removed | t/185 to t/188 | killed |
| S14 | recorded storage parameters ignored | t/196 | killed |
| S18 | attach_partition: the schema check removed | t/190, t/191 | **survived** |
| S19 | create_partition: the 57-byte limit becomes 70 | t/189, t/190 | **survived** (a 58-byte name makes leaf names that PostgreSQL truncates; no test uses a long name) |
| S22 | partition functions: refusal for a bucket that is not `active` removed | t/190 | **survived** (no test holds a bucket in another state than `active` while a partition function runs) |
| S23 | attach_partition: the colocation-group check removed | t/190 | killed |
| S24 | view capture: the trigger blocker removed | t/195 | killed |
| S25 | recreated view: owner not set | t/195 | killed |
| C01 | `utility.c`: a negative number in a bound is refused | t/191 | **survived** (no test writes a negative bound) |
| C02 | `dist_table.c`: the leaf limit is `>=` instead of `>` | t/189 | **survived** (no test at the limit) |
| C04 | `dist_table.c`: storage parameters not given to the remote bucket partitions | t/196 | killed |
| C05 | `bucket_move.c`: leaves of an orphan not moved to the schema `kingres` | t/193 | killed |

The mutants that survive are gaps of the tests, not of the code, except S04 (a redundant check) and S10 (P9C-05).

## What was not covered

- The control plane and console of 9.6 and 9.7 (not in the commits).
- Standbys, failover and synchronous replication during the new calls.
- Kills at the four function fault points inside the blocking calls (`undistribute_after_lock` and the others): errors there are in t/185 and t/186; kills only at the commit points.
- The recovery worker's own resumption (I called the functions by hand) and its distributed deadlock pass against the blocking calls.
- The online form of a partitioned table (refused as `not_available_yet`), undistribute and alter of a user-partitioned table (refused), the ordinary-table attach (not built).
- Queries on partitioned tables beyond t/194 (planner and pushdown are untouched by the increments; I did not fuzz them).
- A second access method for leaves (the build has heap only); `orphan_table` of `check_cluster()` for a partitioned orphan (stated as not done).
- The upgrade path 0.1 to 0.7 beyond the `upgrade` pg_regress test; the in-tree meson run.
- Large tables (more than 10000 rows per call) and timing.
- Mutants of the online engine and of the capture were not repeated (the second review did them).
