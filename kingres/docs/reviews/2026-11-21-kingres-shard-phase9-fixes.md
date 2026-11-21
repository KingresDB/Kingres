<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026 RooyeKhat Media Co -->

# Review of the fixes for the kingres_shard phase 9 review (P9C-01 to P9C-14)

Reviewer: independent (did not write the code). Commit under review: 286f63522c on branch p9c-fix, parent e969205376 (kingres-18),
extension version 0.7. The review it answers: `2026-11-21-kingres-shard-phase9-increments-4-9.md`.
Nothing in the tree was changed. All builds, experiments and mutants ran in private copies of the two installs and in private
copies of the sources.

## Verdict

The fixes do what they claim for 13 of the 14 findings and the critical one (P9C-01) is closed. I found no data-loss or
integrity defect in the fixes. I found one regression that a user can hit (P9F-01, medium: a bound with a cast to a
user-defined type, written with its schema, worked before the commit and is now refused) and four low findings (a rule of
`attach_partition` that cannot be satisfied when the table has privileges, TOAST options of existing partitions that are still
dropped, a wrong SQLSTATE in the documentation, and test claims that are too strong). Nothing blocks the commit; P9F-01 should be
fixed before the beta.

Counts: 0 critical, 0 high, 1 medium, 4 low, 2 notes (7 in all).

## Scope and method

Read: the whole diff (SQL install script, upgrade script, `copy_routed.c`, `utility.c`, `meson.build`, README, design section,
both new tests), the earlier review, the callers of the copy (SQL and C), the control plane (for calls of anything that changed),
`node_conn.c` (how the remote transaction level is chosen), `recovery.c` (the session level of the recovery worker).

Run (all in private installs; PostgreSQL 18.7 and 19rc2; the extension built clean with `-Werror`, 0 warnings, on both):

| What | PostgreSQL 18.7 | PostgreSQL 19rc2 |
|---|---|---|
| Clean build `-Werror` of kingres_shard at 286f63522c | ok | ok |
| t/185 to t/198 (14 files, 2113 tests) | pass (204 s) | pass (201 s) |
| t/197 and t/198 alone (230 tests) | (in the run above) | pass |
| Upgrade 0.6 to 0.7 compared with a fresh 0.7 (script X-up) | identical | identical |
| Red run: t/197 and t/198 against the sources of the parent commit | not run | 96 assertions fail (see below) |
| Mutants of the fixes (below) | not run | 6 run, all killed |
| Probe scripts X1 to X5 and Y1 (below) | X1, X2a, X3, X4 repeated: same results | all |

Not run: the pg_regress suite (the dev script starts its server in a shared socket directory, which I did not use), the in-tree
meson run, crash and kill tests, the mutants S18, S19, S22 and C02 of the earlier review (read only: the design text says they are
killed; I did not repeat them), anything that needs more than three nodes.

The upgrade check (script X-up): two clusters, one with `CREATE EXTENSION kingres_shard VERSION '0.7'`, one with version 0.6
and `ALTER EXTENSION ... UPDATE TO '0.7'`. Compared: every function of the schema (identity arguments, md5 of the definition,
ACL, security definer), every view definition, every column. 198 functions in each; all three comparisons identical on both
majors. The five new functions are present after the upgrade. The tail of the two SQL files (the last 2200 lines, which hold all
the new text) is byte for byte the same.

## Callers of the copy and of the refusals (the "who breaks" question)

`kingres_shard_copy_routed_ex` now refuses `IsolationUsesXactSnapshot()`. Its callers, by reading:

| Caller | Level it runs at | Affected? |
|---|---|---|
| `create_distributed_table` / `create_reference_table` (`dist_table.c`, `kingres_shard_copy_routed`) | already refuses REPEATABLE READ and SERIALIZABLE (`require_read_committed`, P9A-01) before the copy | no |
| `undistribute_table`, `alter_distributed_table` via `internal_copy_routed` (SQL lines 9240 and 9558) | now refused at the entry, before the lock | intended |
| the online engine, `internal_copy_chunk` (SQL lines 7737 and 7854), from `table_operation_advance` | `table_operation_advance` already refuses all but READ COMMITTED; the recovery worker sets `default_transaction_isolation = read committed` (`recovery.c`, line 1483) | no |
| bucket move, rebalance | do not use this copy (no call in `bucket_move.c`); `move_bucket` already handles a transaction snapshot (`bucket_move.c`, line 741) | no |
| control plane (`kingres/control-plane`) | calls `kingres.move_bucket()` and `rebalance_plan()` only; none of the changed functions; its sessions are READ COMMITTED (`failover.go`, `standby/catalog.go`) | no |
| remote nodes | the remote transaction level follows the local one (`node_conn.c`, `kingres_shard_begin_xact`), so a READ COMMITTED call never reaches a remote copy at another level | no |

So no existing caller breaks. The one visible behaviour change is intended: a statement `CREATE TABLE ... PARTITION OF`, `ALTER
TABLE ... ATTACH/DETACH PARTITION` or `DROP TABLE` of a partition, run in a REPEATABLE READ or SERIALIZABLE transaction (for
example by a role whose default level is REPEATABLE READ), is refused with "the partition functions cannot run in a REPEATABLE READ
or SERIALIZABLE transaction" (reproduced, X4). Before, the same statement ran. This is documented in the README; the message
names "the partition functions" although the user wrote a statement, which is a little confusing, no more.

The order change in the copy (the batch sender finished before the cursor is closed) holds: the error paths are unchanged (an
error aborts the transaction and the portal is dropped with it), the limit and chunk modes read the same cursor, and the
30000-row cases of t/197 pass on both majors. With the old order restored (mutant M2 below) four assertions fail, so the test
covers the line.

## Findings

Severities: critical, high, medium, low, note.

### P9F-01 (medium): a cast to a user-defined type in a partition bound is no longer accepted by the statement mapping

Place: `append_bound_expr`, `utility.c` line 1798 (`format_type_with_typemod(typoid, typmod)` replaces `TypeNameToString`).

Steps (script X1, reproduced on 18.7 and 19rc2): `CREATE TYPE public.color AS ENUM ('red','green','blue','pink')`; a list
partitioned table `lt` on a column of that type, distributed. Then:

- `CREATE TABLE public.lt_c PARTITION OF public.lt FOR VALUES IN ('blue'::public.color)`.

Before the commit (parent, same script): works; the catalog keeps `IN ('blue'::public.color)`, the leaves are made. After the
commit: `ERROR: type "color" does not exist`. The same statement written with `'green'::color` fails before and after (the bound
text is evaluated by a function whose `search_path` is `pg_catalog, kingres, pg_temp`). The call
`kingres.create_partition('public.lt', 'lt_d', 'IN (''pink''::public.color)')` works, because the text is passed as written.

Cause: `format_type_with_typemod` writes a name without its schema when the type is visible in the caller's `search_path`.
`TypeNameToString` wrote the name as the user wrote it, so a user who qualified the name got a text that the function (with
its own `search_path`) can resolve. The text is resolved later in another `search_path`, so the type must always be written with
its schema; `format_type_extended(typoid, typmod, FORMAT_TYPE_FORCE_QUALIFY | FORMAT_TYPE_TYPEMOD_GIVEN)` does that and keeps the
modifier.

Why it matters: a documented use of the statement form (casts in bounds) now fails for every non-built-in type that is
schema-qualified: enums, domains, extension types. Built-in types (`date`, `timestamptz`, `numeric(3,0)`) are not affected, which
is why the new tests do not see it. Nothing is lost: the statement fails closed and the function form works.

A fix must: write the type schema-qualified (for built-in types `pg_catalog` is accepted by the parser) and add a test with an
enum or domain partition key and a qualified cast.

### P9F-02 (low): `attach_partition` refuses a table whose privileges are the same as the partitioned table's; the hint cannot be followed

Place: `kingres.internal_attach_differences`, SQL line 11373 (`tc.relacl IS NOT NULL AND tc.relacl::text[] IS DISTINCT FROM
acldefault(...)`), and the HINT of `attach_partition` ("Give the table the same indexes and constraints, owner and privileges as
the partitioned table").

Steps (script X2a, reproduced on both majors): `GRANT SELECT ON xa TO r1` on the partitioned table; a distributed table `xa_same`
with the same `GRANT SELECT ... TO r1`; `attach_partition('public.xa', 'public.xa_same', ...)` is refused with
"privileges of its own". Before the commit it was accepted.

Why it matters, and why it is only low: the test is "any privilege that is not the default", not "a privilege that the
partitioned table does not have". A parent with grants can therefore only take tables with the default ACL, and the hint says the
opposite. Leaves of partitions carry no ACL in this design (the sibling leaf has a NULL ACL in the same script), so a
privilege on the table would not be lost by a move in any case; the refusal is stricter than the reason for it. Nothing is lost.

A fix must: compare the table's ACL with the partitioned table's ACL (or with the default when the parent has none), or change
the hint and the README to say "privileges other than the default are refused".

### P9F-03 (low): TOAST options of an existing partition are not seen by the report of `create_distributed_table` and are lost

Place: the partition checks of `table_change_report` (SQL line 6301: `r.reloptions IS NOT NULL`; the new
`internal_table_options` is not used here).

Steps (script X3, reproduced on both majors): a range partitioned table `tp` whose partition `tp_a` has
`ALTER TABLE tp_a SET (toast.autovacuum_enabled = false)`. `table_change_report('distribute', 'tp', ...)` has no blocker
and no note; `create_distributed_table('tp', 'k')` succeeds; the TOAST table of the leaf has no options afterwards.

Why it matters: this is the same hole as P9C-03 for storage parameters, one level down: the fix for P9C-08 reads the options of
the TOAST table in the attach rule (through `internal_reloptions_of`) and for ordinary tables, but the check that stops a
partition with "a storage parameter ... of its own" reads only `pg_class.reloptions`, and TOAST options are in the TOAST table's
row. The loss is the one the review called low (an autovacuum setting on the TOAST table).

A fix must: use `internal_table_options(r.oid)` in that test.

### P9F-04 (low): the documentation gives SQLSTATE 55006 for the refusal; the code raises 25001

Place: README, the paragraph of the blocking calls ("refuse the other levels with SQLSTATE `55006`"); design section of the
fixes ("SQLSTATE `55006`, the same words as the create call").

The SQL (`internal_require_read_committed`, line 11330: `active_sql_transaction`) and the C copy (`ERRCODE_ACTIVE_SQL_TRANSACTION`)
raise 25001, as `create_distributed_table` does. 55006 is `object_in_use`, which is what the earlier text uses for busy groups.
By reading; no test checks the code. A client that retries on 55006 as the design intends for busy groups would not retry on
this error, which is right; only the text is wrong.

### P9F-05 (low): "every new test fails on the tree of the review" is not true, and part of the red run cannot be read

Place: design section of the fixes, first paragraph.

Red run (t/197 and t/198 against the sources of the parent, 19rc2): 96 assertions fail (43 in t/197, 53 in t/198). t/197
then dies at its section E (the first call there is an error on the old code), so section F is not reached and its assertions
are neither failed nor passed in that run. Assertions that pass on the old code: the controls (a call in READ COMMITTED
succeeds), and the negative-bound cases of t/198 section D, which cover mutant C01 and not a finding. I killed E and F with
mutants (below), so both are covered; the sentence is still wrong. The same section says the mutants S18, S19, S22, C01, C02 and
S10 are killed: I confirmed C01 and S10, not the others.

### P9F-06 (note): the extension version 0.7 was changed in place

The commit adds five functions and rewrites many others in `kingres_shard--0.7.sql` and `kingres_shard--0.6--0.7.sql`, with no
0.7 to 0.8 step. A database that was made from the earlier build of 0.7 has no way to get them. The upgrade from 0.6 and the
fresh install are identical (above). This is fine before the first release; a database of a development build has to be made
again.

### P9F-07 (note): claims in the README that were not checked

The README says the deadlock pass of the recovery worker cancels one of the two sessions of a distributed deadlock between a
blocking call and a writer. The earlier review saw that in its cluster the worker did not do it before the function was called by
hand, and said it was not shown to be a defect. The new text states it as fact. I did not test this. Also not done by the
fix (and stated so in the README): the grantor chain of grants made with the grant option by a role other than the owner of a
recreated view.

## What held

- P9C-01 in the code: both blocking calls and the partition context call `internal_require_read_committed` before any lock; the
  copy refuses a transaction snapshot as the last line; the error text names the call. The functions are VOLATILE plpgsql, so
  in READ COMMITTED every statement takes a fresh snapshot after the lock; the writer-that-commits-while-waiting case passes for
  both calls with a digest of every row (t/197, section A). The session default (`default_transaction_isolation =
  serializable`) is refused for a single statement.
- P9C-04: the set of tables is the named table, the foreign key component for an undistribute with `cascade_via_foreign_keys`, and the
  hash tables of the group for an alter with `cascade_to_colocated`; the table is named in the detail; nothing runs with
  superuser rights (t/197, section C).
- P9C-06: the target group is checked in the report and so in the call; the `default` searches use the same
  function; a held move, a table operation and a bucket that is not active are each refused (t/197, section D).
- Dynamic SQL added by the commit: `internal_copy_acl_comment` (role names through `quote_ident`, the table through a
  regclass, privilege names from `aclexplode`, the comment through `%L`), `internal_reset_acl` (owner through `%I`),
  `internal_view_recreate` (unchanged quoting), `user_partition_exists` (parameters). I found no injection.
- `parse_ident` in `check_cluster`: the leaf name is built by `partition_leaves` with `%I`, so it always parses; a name with a
  double quote, a quote or dots is found (t/198, section F).
- ACL of recreated views: every grant is taken back when the new view has an ACL or the old one had; the owner's entries are now
  captured; the test sets default privileges and compares the ACL and owner of both views before and after.
- The ACL of the table made by `detach_partition` and its comment: copied on every node (t/198, section E).
- Attach refusals: a candidate with a check, an index (other name, same definition is accepted: probe X5), a default, storage
  parameters, another owner or privileges is refused with each named. A column comment, a column privilege, a statistics target
  and a replica identity on the table are accepted, and the leaves have none of them before the move either (they live on the
  root table; probe Y1), so a move loses nothing.
- Install script and upgrade script: identical new text (above).

## Mutants and the gap named by the fix writer

Each mutant is one change, built clean on 19rc2 into its own install, judged by the named test. Killed means a test failed.

| Id | Change | Judged by | Result |
|---|---|---|---|
| M1 (old review's C01) | `append_bound_expr`: a negative integer constant is refused | t/198 | killed: 7 assertions (section D, 86 to 92) |
| M1b | `append_bound_expr`: the unary-minus branch (`-200::int`) removed | t/198 | killed: 3 assertions (section D, 88 to 90) |
| M2 | `copy_routed.c`: the sender is finished after the cursor is closed (old order) | t/197 | killed: 4 assertions (section B) |
| M3 | `internal_view_recreate`: the reset of the ACL removed | t/198 | killed: 3 assertions (section H) |
| M4 | the `deferrable_unique` blocker made unreachable | t/197 | killed: 7 assertions (section E) |
| M5 | `internal_table_options` reads the table's options only (no TOAST) | t/197 | killed: 11 assertions (section F) |

The gap the fix writer named (mutant C01, corrected assertion run only against the fixed code): I ran it against the mutant, in two
forms. The old C01 mutant is not clearly defined by its name, so I made both plausible ones. The parser already folds `-100` into a
negative integer constant, so the case `FROM (-100) TO (0)` goes through the constant branch (M1 kills it: 7 assertions fail,
including "a partition with negative numbers in its bound"); the minus operator branch is reached only by a minus in front of a
cast (`-200::int`), which the corrected assertions cover (M1b kills it: "a bound with a minus in front of a cast", "the minus is
written back", "the leaves have the negative bounds"). Both forms of the mutant are therefore killed by t/198.

## Table of the findings of the earlier review

| Id | Verdict | Test fails without the fix? | Remarks |
|---|---|---|---|
| P9C-01 | fixed | yes: 20 assertions of section A (red run); the C check by "with the reason, before it looks for the destination" | the refusal is at the entry of both calls, the partition functions and the copy; no existing caller breaks; wrong SQLSTATE in the text (P9F-04) |
| P9C-02 | fixed | yes: 4 assertions of section B (red run); mutant M2 | 30000 rows alter, undistribute to another group, reference table of 6000 rows pass on both majors |
| P9C-03 | fixed (by refusing); partly | yes: about 20 assertions of t/198 section A (red run) | the refusal is too strict for identical privileges (P9F-02); the check that stops partitions with TOAST options of their own at distribute time is not extended (P9F-03); `check_cluster()` still does not compare leaves with the parent (stated by the fix writer) |
| P9C-04 | fixed | yes: 12 assertions of section C (red run) | |
| P9C-05 | fixed | yes: mutant M3 (3 assertions); red run 3 assertions | the grantor chain is still not carried (documented, P9F-07) |
| P9C-06 | fixed | yes: 8 assertions of section D (red run) | |
| P9C-07 | fixed | yes: mutant M4 (7 assertions); the red run dies at section E | the blocker is in the report for the three online forms; the blocking forms are untouched |
| P9C-08 | fixed for ordinary tables and the move; only documented-and-partial for existing partitions | yes: mutant M5 (11 assertions) | P9F-03 |
| P9C-09 | fixed for the listed items; a regression is introduced by the cast change | yes: the schema, IF NOT EXISTS, UNLOGGED and typmod assertions fail on the old code (red run) | P9F-01 |
| P9C-10 | fixed | yes: 5 assertions of section E (red run) | column privileges are not carried (stated) |
| P9C-11 | fixed | yes: "check_cluster() sees the leaves" fails on the old code | |
| P9C-12 | fixed | yes: section F fails on the old code | `is_default` and `internal_leaf_op` take any case of DEFAULT |
| P9C-13 | only documented | not applicable | the deadlock detector claim is not verified (P9F-07) |
| P9C-14 | fixed (the view on a partition is a blocker); the other three items documented | yes: 3 assertions of section G (red run) | |

## What was not covered

- Standbys, failover and crash and kill points for the changed calls (the fix does not change the commit paths; the
  blocking calls' fault tests of t/185 and t/186 pass).
- The control plane beyond reading its callers.
- The mutants S18, S19, S22 and C02 (read only).
- pg_regress, the in-tree meson run, large tables beyond 30000 rows, timing.
- The behaviour of `colocate_with => 'default'` when a bucket is `orphan` and awaits cleanup: `internal_group_busy` treats any bucket
  that is not `active` as busy, so such a group is not offered until the cleanup ran; I did not test whether this produces extra
  colocation groups in practice.
