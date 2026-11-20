<!-- SPDX-License-Identifier: AGPL-3.0-only -->
# kingres_shard: design of phase 2b (pushdown, ordered merge, colocated joins, row routing)

Status: draft for review, written 2026-11-14 and **revised 2026-11-15 against
extension 0.4** (the version at the last commit; 0.5, the high-availability part
of phase 5, is being written in the working tree and was read where it matters).
Builds on [shard-phase2a.md](shard-phase2a.md) (extension version 0.2), on
[shard-phase4.md](shard-phase4.md) (0.3, bucket moves, and 0.4, its fix round)
and on [ha-phase5.md](ha-phase5.md) (0.5), and produces extension version
**0.6** with the upgrade script `kingres_shard--0.5--0.6.sql` (see
[Version and merge order](#14-version-and-merge-order)). The numbers are the
ones of "Versions of `kingres_shard`" in [architecture.md](../architecture.md);
the first draft of this note said 0.5, which is now the high-availability
release.

What the revision of 2026-11-15 changed. Each item is a change of the sections
named, not a note:

1. **Placement check.** A fragment no longer carries a "map version `V`". The
   extension compares the placement number of the colocation group and the
   identity of the attached partitions before and after the cursors are
   opened; fragments use that mechanism, written so that the epoch of phase 10
   makes it simpler and not different:
   [4.3](#43-the-placement-check),
   [10.2](#102-the-placement-check-and-a-bucket-that-is-moving-or-has-just-moved),
   [step 2 of 17](#17-implementation-order).
2. **One bucket lookup.** Nothing computes `hash % bucket_count`; every use goes
   through one function in `hashing.c`: [4.7](#47-one-bucket-lookup).
3. **`ON CONFLICT DO UPDATE` in a colocated `INSERT ... SELECT` shipped
   whole** is allowed, with its conditions:
   [11.7](#117-colocated-insert-from-select-shipped-whole-p6).
4. **Functions and aggregates of extensions** may be shipped by an allow-list
   in code, with a version check on the other node:
   [5.1](#51-the-rule-for-an-expression-that-depends-on-data),
   [5.5](#55-objects-of-extensions), [7.2](#72-rules-of-the-extension-on-top-of-the-planners),
   [4.2](#42-markers-and-what-a-receiving-group-checks) (the sub-select of
   phase 12).
5. **The batch sender takes an explicit destination** (a placement, every node,
   one node) and checks that destination's placement:
   [11.9](#119-the-batch-sender-and-its-destination).
6. **A read that mixes a distributed table of another group with a local
   table** is answered by the row-moving scan, not refused:
   [6.5](#65-reads-that-mix-a-distributed-table-with-a-local-table).
7. **PostgreSQL 18 and 19.** Every hook and structure the design relies on was
   compared with `REL_19_RC2`; what differs and how `src/pg_compat.h` hides it:
   [2.3](#23-postgresql-18-and-19).
8. **High availability.** What every new path does when the primary of a group
   changes during the statement, ending in the retryable `KS001`:
   [10.3](#103-standbys-and-a-change-of-primary),
   [4.4](#44-how-the-origin-runs-fragments-kingres-fragment-scan),
   [4.6](#46-errors).
9. **Review lessons.** Each mechanism was checked against the classes of defect
   found in the reviews of 0.2, 0.3 and 0.4, and the design was changed where
   it was exposed: [10.5](#105-what-the-earlier-reviews-found-and-how-each-mechanism-avoids-it),
   [16](#16-tests).
10. **Order of implementation.** Increments that are independently
    committable and releasable, each with its own switch and a comparison with
    the scan: [17](#17-implementation-order), [13](#13-settings).

Phase 2a made every statement correct or refused. Phase 2b makes the common
ones fast and lifts the refusals that remain on writes. It adds no catalog
table and no SQL object; it changes how statements are planned and executed.

How this note was checked. Claims about PostgreSQL are checked against the
18.7 source in this tree, with the file and function named, and, for what the
design relies on, against the tag `REL_19_RC2` of PostgreSQL 19, read with `git
show REL_19_RC2:<path>` because the 19 source is not in this tree (section
2.3). Claims about the extension are checked against the code under
`contrib/kingres_shard/src` at the last commit (0.4); where the working tree
differs because 0.5 is being written, the file and the difference are named.
Anything not checked is marked **[Needs verification]** with what would settle
it, and collected at the end.

## Sources read

- Documents: `shard-mvp.md`, `shard-phase2a.md` (including its implementation
  notes and the fix round), `shard-phase4.md`, `ha-phase5.md` (part C),
  `shard-phase9.md` (what it needs from the row router), `shard-phase10.md`
  (the bucket modulus and the placement epoch),
  `analytics-phase12-queries.md` (sections 3 and 4.10) and the README of
  `contrib/kingres_sketch`, `ledger-mvp.md` and `ledger-0.2.md` (what a ledger
  table is), the reviews of `kingres_shard` 0.1, 0.2, the fixes of 0.2, 0.3 and
  0.4 (`kingres/docs/reviews`), ADR 0022 and ADR 0023, the roadmap section
  "Phases 9 to 12" of `architecture.md`, `coding-standards.md`, `testing.md`,
  `documentation.md`, and the README of the extension at the last commit (0.4).
- Extension at the last commit (0.4): `src/planner.c`, `dist_scan.c`,
  `remote_scan.c`, `shippable.c`, `fanout.c`, `catalog.c/h`, `hashing.c/h`,
  `bucket_move.c` (the helper functions the scan calls on other nodes),
  `node_conn.c/h`, `utility.c` (the `COPY` rule), `fault.c`, `pg_compat.h`, the
  install and upgrade scripts, and the test harness `t/KingresShardCompare.pm`
  and `t/KingresShardTest.pm` with the list of tests in `t/`.
- Extension, different in the working tree (0.5 in progress): `catalog.c` (the
  placement number now includes the node that is the primary of each group, and
  a node that is not the primary of its group has `local_group_id = -2`),
  `dist_scan.c` and `planner.c` (the handling of that value), `node_conn.c`
  (`kingres_shard_conn_drop_stale`), `failover.c` (the header of
  `promote_node`) and the tests `t/048` to `t/054`. Nothing else in the files
  this note relies on differs from the commit.
- PostgreSQL 18.7: `src/backend/optimizer/plan/planner.c`
  (`standard_planner`, `grouping_planner`, `create_ordinary_grouping_paths`,
  `create_partial_grouping_paths`, `make_partial_grouping_target`,
  `mark_partial_aggref`, `can_partial_agg`), `optimizer/prep/prepagg.c`
  (`preprocess_aggref`), `optimizer/plan/setrefs.c`
  (`convert_combining_aggrefs`, `add_rte_to_flat_rtable`,
  `set_customscan_references`, `set_upper_references`),
  `optimizer/plan/createplan.c` (`create_customscan_plan`),
  `optimizer/path/allpaths.c` and `joinpath.c` (the hook call sites),
  `executor/nodeAgg.c`, `executor/execAmi.c` (`ExecSupportsBackwardScan`),
  `executor/spi.c` (`SPI_cursor_open_internal`), `tcop/pquery.c`
  (`PortalStart`), `commands/copy.c` (`DoCopy`), `include/commands/copy.h`,
  `include/nodes/pathnodes.h`, `include/optimizer/*.h`,
  `commands/explain.c`, and `src/include/catalog/pg_aggregate.dat`.
- PostgreSQL 19 (`REL_19_RC2`): `include/optimizer/planner.h`, `paths.h`,
  `pathnode.h`, `prep.h`, `optimizer.h`, `include/nodes/pathnodes.h`,
  `plannodes.h`, `parsenodes.h`, `nodes.h`, `primnodes.h`, `extensible.h`,
  `include/executor/executor.h`, `tuptable.h`, `spi.h`,
  `include/commands/copy.h`, `explain.h`, `explain_format.h`,
  `include/libpq/libpq-be-fe.h`, `include/utils/sortsupport.h`, `ruleutils.h`,
  `backend/optimizer/plan/planner.c`, `setrefs.c`, `createplan.c`,
  `prep/prepagg.c`, `path/joinpath.c`, `joinrels.c`, `allpaths.c`,
  `util/relnode.c`, `executor/nodeAgg.c`, `spi.c`, `commands/copy.c`,
  `utils/adt/ruleutils.c`.
- `contrib/postgres_fdw` (18.7 and 19): `postgres_fdw.c` (`foreign_join_ok`,
  `foreign_grouping_ok`, `postgresGetForeignUpperPaths`,
  `add_foreign_ordered_paths`, `add_foreign_final_paths`), `deparse.c`
  (`foreign_expr_walker`, `is_foreign_pathkey`), `shippable.c` (`is_builtin`,
  `is_shippable`); `contrib/file_fdw/file_fdw.c` (use of `BeginCopyFrom`).

## Goals and scope

| # | Item | Delivered by |
|---|---|---|
| 1 | Aggregate pushdown: partial aggregation per group and a final aggregation at the origin, and whole-aggregate pushdown when grouping by the distribution key | sections 7 and 6 |
| 2 | Sort and limit pushdown: ordered merge of per-group streams, `LIMIT limit+offset` per group, more of `WHERE` and the target list shipped | sections 8 and 5 |
| 3 | Colocated join pushdown, including reference tables | section 9 |
| 4 | Row routing for bulk writes: multi-row `INSERT`, `INSERT ... SELECT`, `COPY FROM`, atomically under two-phase commit, bounded memory | section 11 |
| 5 | What 2a deferred | the table below |

### What phase 2a deferred, and what happens to each item

| Item (source) | Decision | Reason |
|---|---|---|
| Aggregate, sort, limit and colocated-join pushdown (2a "Not in phase 2a") | In | The goal of this phase. |
| Multi-row `INSERT`, `INSERT ... SELECT`, `COPY FROM` across groups (2a "Not in phase 2a", README "Limitations") | In | Section 11. |
| Single-row `INSERT` whose key is not a constant (a default, `gen_random_uuid()`, a function) is refused today as "spans shard groups" (planner.c, `restrict_to_value` finds no constant) | In | The row router computes the key at the origin; this is the same code path as a one-row batch. |
| No bucket pruning in the distributed scan (2a notes, "Distributed scan") | In | Pruning from the planner's restriction lists is cheap and is needed by every fragment (section 4.5). |
| The scan did not check that placement changed while it ran, "because buckets do not move in this phase" (2a notes) | Done in 0.4; fragments reuse it | 0.4 compares the placement number and the identity of the attached partitions before and after the cursors are opened (README, "The map version rule"; `dist_scan.c`, `ds_start`). Section 4.3 generalizes it to fragments that read several tables. |
| A read that combines a distributed table of another shard group with a local table: "statements that combine distributed tables of another shard group with local tables are not supported yet" (`planner.c`) | In | A refused read contradicts ADR 0022. Section 6.5 shows which statements the message covers and answers them with the scan. |
| A global snapshot (2a "Not in phase 2a") | Out | Phase 8. Section 10 says what pushdown does and does not change. |
| Distributed deadlock detection (2a) | Done in 0.5 | Phase 5 (part C). It needs nothing from this phase. |
| Row locking (`FOR UPDATE`, `FOR SHARE`) on a statement that spans groups (2a section 5) | Out | Needs a row identifier valid across nodes. Such a statement is refused as today. |
| System columns and `PlaceHolderVar`s computed on a distributed table (2a notes) | Out | Unchanged refusals. |
| `UPDATE ... FROM`, `DELETE ... USING`, `MERGE` that read a distributed table and write another table (review S2-18) | Out | Needs a row identifier of the read table; unchanged. |
| A reference-table write that reads a distributed table, or uses a volatile value shipped as a constant (2a section 3 "out of scope here: reject") | Out | The router could compute such rows once at the origin and ship them to every node, but then the whole stream needs the cluster-wide writer serialization of review S2-06 and a per-batch equality check of the copies. That is a separate increment. Still refused. |
| Views over distributed tables whose owner differs from the session role (S2-15) | Out | Unchanged. A view, a subquery, a CTE or a set operation is not a pushdown unit in 2b (section 6), so it uses the 2a scan. |
| Parallel query inside the origin's part of a scan, and parallel plans on the other groups | Out | Fragments run through cursors, which cannot be parallel; section 4.4. |
| Statement-level retry after a stale placement for fragments (2a retries only a routed statement on a fresh remote transaction) | Out | A fragment that meets a changed placement or a changed primary fails with `KS001`; the client retries. Sections 10.2 and 10.3. |
| `enable_partitionwise_join`/`_aggregate` being switched off for statements planned with the scan (S2-20) | Out | Unchanged. |
| Generic plans cannot route a statement restricted by a parameter | Out | Unchanged. Fragment plans do work as generic plans (section 10.4). |
| Approximate aggregates of `kingres_sketch` (phase 12) cannot be pushed down because only built-in functions are shipped | In | An allow-list of extensions, in code, with a version check (section 5.5). |
| `INSERT ... ON CONFLICT DO UPDATE` through the router | Out | Per-batch application cannot reproduce "cannot affect row a second time" (section 11.3). A colocated `INSERT ... SELECT` that is shipped whole is not batched and is allowed (section 11.7). |

## 1. The starting point

Phase 2a plans a `SELECT` that spans groups with the standard planner while
`set_rel_pathlist_hook` (`dist_scan.c`, `ds_set_rel_pathlist`) replaces the
paths of each distributed table by one `CustomPath`, "Kingres Distributed
Scan": its child is the best local path of the table, and at execution it
sends `DECLARE ... CURSOR FOR SELECT <columns> FROM <table> AS ks_t WHERE
<shippable restrictions>` to every other group. PostgreSQL joins, aggregates,
sorts and limits above it at the origin. This is always correct and moves every
needed row of every table to the origin. The decision to use it is made in
`planner_hook_body` (`planner.c`), on the `Query` as the rewriter delivers it,
before `standard_planner` runs.

Phase 2b keeps this scan as the fallback and adds paths that let each group do
more of the work. Every new path is an alternative `Path` in the same planner
run, chosen by cost, so a statement that cannot use any of them gets exactly
the 2a plan.

What 0.4 and 0.5 added to the starting point, and what this design relies on:

- A scan is checked for a change of placement while it starts: the placement
  number of the colocation group (a hash of which group, and since 0.5 which
  node as primary, holds each bucket) and the identity of the partitions each
  node has of the table are read from the catalog tables before the cursors are
  opened and again after, and compared with the numbers of the plan
  (`dist_scan.c`, `ds_start`; `catalog.c`, `kingres_shard_placement_hash*` and
  `kingres_shard_partition_identity_now`). A difference raises the retryable
  `KS001` before the first row. Section 4.3 uses this for fragments.
- A node that the catalog does not call the primary of its group (the old
  primary after `promote_node`, a standby) has `local_group_id = -2`: it owns
  no bucket, runs nothing as local, and its scan has no local part. Sections
  4.4 and 10.3 use this.
- The partition list of a table is made as new as the catalog cache before a
  statement is planned (`kingres_shard_ensure_fresh_partitions`, called by the
  planner hook for every distributed table of the statement). The local part of
  a fragment must pass through it too (section 4.4).
- A distributed table has no user triggers, no row-level security, and every
  unique index contains the distribution column; `create_distributed_table`
  and the utility hook enforce this, and several arguments below rest on it.

## 2. Mechanism in PostgreSQL 18 terms

### 2.1 Hooks used

| Need | Hook or API, with the place in the 18.7 source | Why this one |
|---|---|---|
| The routing decision and the analysis of whether a statement is a pushdown unit | `planner_hook` (already installed, `kingres_shard_planner_hook`) | It is the only point where the `Query` is still in the shape the rewriter produced. After `subquery_planner` has pulled sub-links up, they have become semi and anti joins and outer joins may have been reduced, and the deparser has no text for such a tree (`get_from_clause_item` in `ruleutils.c` raises "unrecognized join type" for anything but the four SQL join types, line 12709), so the text of a fragment is always built from a copy of the `Query` taken here. |
| Base relation: pruning and the 2a scan | `set_rel_pathlist_hook` (`allpaths.c:542`, already used) | Unchanged role. |
| Join of distributed and reference tables | `set_join_pathlist_hook` (`joinpath.c:345`, signature `(root, joinrel, outerrel, innerrel, jointype, extra)`) | Called for every pair of inputs of every join relation; used only for the join relation that covers every relation of the query level (`bms_equal(joinrel->relids, root->all_query_rels)`), so the cost of the hook is a bitmap comparison for all other pairs; on 19 it also returns at once for a grouped relation (2.3, D1). |
| Aggregate pushdown | `create_upper_paths_hook` at `UPPERREL_GROUP_AGG` (`planner.c:4384`, end of `create_ordinary_grouping_paths`; its `extra` is a `GroupPathExtraData` whose `flags` carry `GROUPING_CAN_PARTIAL_AGG`, `GROUPING_CAN_USE_HASH`, `GROUPING_CAN_USE_SORT`) | The planner has already decided, with its own rules (`can_partial_agg`, `prepagg.c:preprocess_aggref`), whether the aggregates can be split. The hook reuses that decision instead of re-deriving it. |
| A whole query that every group can answer by itself, with its `ORDER BY` and `LIMIT` | `create_upper_paths_hook` at `UPPERREL_FINAL` (`planner.c:2391`, `FinalPathExtraData` has `limit_needed`, `count_est`, `offset_est`) | The final relation is where `ModifyTable`, `LockRows` and `Limit` have been added; a path added here replaces the whole upper pipeline. |
| Executing the work | `CustomPath` and `CustomScan` ("Kingres Fragment Scan", new) | The same executor interface as the 2a scan. |
| Producing partial states on a group | The planner hook of the group that runs the fragment, which post-processes the plan (section 7.3) | Needed because no SQL expression returns the transition state of an aggregate. |
| Merging ordered streams | `PrepareSortSupportFromOrderingOp` (`utils/sortsupport.h`) inside the custom scan | The same data `nodeSort` and `nodeMergeAppend` use. |
| Row routing | `ProcessUtility_hook` for `COPY` (exists, `utility.c`), and the planner hook for `INSERT` | A `CustomScan` as the whole plan, as the "Kingres Remote Statement" is today. |

Not used: `join_search_hook`, `GetForeignUpperPaths` and every other FDW
callback (the extension has no foreign tables), `ExecutorStart_hook`.

### 2.2 How postgres_fdw does it, and what is reused

`postgres_fdw` pushes work down through the FDW callbacks
`GetForeignJoinPaths` (`postgresGetForeignJoinPaths`, which calls
`foreign_join_ok`) and `GetForeignUpperPaths` (`postgresGetForeignUpperPaths`,
`postgres_fdw.c:6748`), which handles exactly the three upper stages this note
uses: `UPPERREL_GROUP_AGG` (`foreign_grouping_ok`), `UPPERREL_ORDERED`
(`add_foreign_ordered_paths`) and `UPPERREL_FINAL` (`add_foreign_final_paths`,
which pushes `LIMIT`/`OFFSET`). It builds the remote SQL from the planner's
`RelOptInfo` structures with its own deparser (`deparse.c`), decides
shippability expression by expression (`foreign_expr_walker`), and keeps the
conditions it cannot ship in `local_conds`.

Reused as ideas only (no code, no FDW):

- the stage structure (group, order, final) and one path per stage that
  extends the path of the stage below;
- all-or-nothing at a stage: `foreign_grouping_ok` returns false when the
  underlying relation has `local_conds`, and `add_foreign_final_paths` refuses a
  `LIMIT` when the relation has local conditions or `WITH TIES`; this note uses
  the same rule (section 5.4);
- the join rules of `foreign_join_ok`: inner, left, right, full and semi joins
  may be pushed, anti joins are not; a join clause of an outer join must be
  shippable; this note is stricter (section 9);
- the ordering rule of `is_foreign_pathkey`: only an ordering whose operator
  family is shippable and whose expression is evaluated remotely.

Not reused: remote cost estimates (`use_remote_estimate`; fragments are costed
from the origin's statistics), deparsing from `RelOptInfo` (fragments are built
from the `Query`, section 4.1), user-visible server options.

`postgres_fdw.c` does not mention partial aggregation (a search for "partial"
finds nothing): it ships an aggregate only when the whole aggregation can
happen on one server. Splitting an aggregate across servers is therefore not
something to borrow from it; section 7 uses PostgreSQL's own split
(`AGGSPLIT_INITIAL_SERIAL` and `AGGSPLIT_FINAL_DESERIAL`, the machinery of
parallel aggregation).

### 2.3 PostgreSQL 18 and 19

ADR 0023 requires that this phase builds and passes on 18 and on 19, from one
source, with the differences in `src/pg_compat.h`. The hooks, nodes and
functions below were compared between the 18.7 tree and `REL_19_RC2` (headers by
`diff`, function bodies by extracting each function from both trees). `pg_compat.h`
already hides the planner hook's new `ExplainState` argument
(`KS_PLANNER_ES_PARAM`, `KS_PLANNER_ES_ARG`), `resultRelations` against
`resultRelationRelids` (`ks_result_relations`, `KS_CLEAR_RESULT_RELATIONS`),
`table_beginscan` (`KS_TABLE_BEGINSCAN`), `ClusterStmt` against `RepackStmt`, and
the libpq result wrapper (`KS_LIBPQ_RESULTS_OWNED_BY_CONTEXT`).

**Identical on both, so nothing to hide** (this is a result; the first draft
could not state it):

| What 2b uses | Evidence |
|---|---|
| `create_upper_paths_hook` and its call sites for `UPPERREL_GROUP_AGG` and `UPPERREL_FINAL`; `GroupPathExtraData` flags (`GROUPING_CAN_PARTIAL_AGG`, `_USE_HASH`, `_USE_SORT`), `partial_costs_set`; `FinalPathExtraData` | `planner.h` hook type unchanged; the calls are at 4384 and 2391 (18.7) and 4459 and 2473 (19) |
| `set_rel_pathlist_hook`, `set_join_pathlist_hook` (type and call in `add_paths_to_joinrel`) | `paths.h`, `joinpath.c` |
| The custom scan interface: `CustomPath`, `CustomScan`, `CustomPathMethods`, `CustomScanMethods`, `CustomExecMethods`, `CUSTOMPATH_SUPPORT_*` | `extensible.h` differs only in the copyright line; `create_customscan_plan` has the same body |
| `mark_partial_aggref`, `make_partial_grouping_target`, `can_partial_agg`, `create_agg_path`, `create_sort_path`, `create_limit_path`, `create_group_result_path`, `get_agg_clause_costs` | declarations identical; the body of `mark_partial_aggref` identical |
| `set_customscan_references`, `convert_combining_aggrefs`, `set_upper_references`, `add_rte_to_flat_rtable` (so every `RTE_RELATION` of the query reaches `relationOids` and `perminfo` even when the plan does not scan it) | bodies identical, except `palloc_object` in the last |
| `nodeAgg.c`: `Assert(aggref->aggsplit == aggstate->aggsplit)`, the skip-final and serialize paths | present at 3774 (18.7) and 3797 (19) |
| `SPI_cursor_open_internal`: a read-only cursor uses `GetActiveSnapshot()` | body identical |
| `pg_get_querydef(Query *, bool)`, `deparse_expression` | declarations identical; the deparse of a join raises "unrecognized join type" for anything but the four SQL join types on both |
| `PrepareSortSupportFromOrderingOp`, `ApplySortComparator` | present on both |
| `BeginCopyFrom`, `NextCopyFrom`, `EndCopyFrom`, `NextCopyFromRawFields`, `CopyFromErrorCallback` | signatures identical |
| `RTE_*` kinds, the expression node types of `primnodes.h` | the same sets (19 adds no node type the walker could meet; the walker refuses unknown kinds by default) |
| `MakeSingleTupleTableSlot`, `ExecInitExtraTupleSlot`, `ExecStoreVirtualTuple` | unchanged |

**Different, with the effect on 2b and how `pg_compat.h` hides it** (each is a
definition to add in step 1 of section 17):

| # | 18.7 | 19 (`REL_19_RC2`) | Effect on 2b | Hidden by |
|---|---|---|---|---|
| D1 | A join relation is built from two inputs only | **Eager aggregation** (`enable_eager_aggregate`, default on): a grouped join relation (`RelOptInfo.agg_info`, `IS_GROUPED_REL`) is a copy of the join relation with the same `relids`, and `populate_joinrel_with_paths` calls `add_paths_to_joinrel`, so `set_join_pathlist_hook` runs for it. `build_grouped_rel` clears the FDW fields because "FDWs don't know how to handle grouped relations" | The P2 hook would add a pushed join to a relation that holds partial aggregates. It returns at once for a grouped relation. The group's own plan of a `kind=partial` fragment must not contain a partial `Agg` below a join, so it also switches `enable_eager_aggregate` off | `KS_IS_GROUPED_REL(rel)` (19: `IS_GROUPED_REL(rel)`; 18: false). `KS_PARTIAL_PLAN_GUCS`: the list of settings the group sets while planning a partial fragment: `max_parallel_workers_per_gather = 0`, `enable_partitionwise_aggregate = off` on both and `enable_eager_aggregate = off` on 19 only |
| D2 | `ExecInitScanTupleSlot(estate, ss, desc, ops)`, `MakeTupleTableSlot(desc, ops)`, `ExecAllocTableSlot` | each gets a `uint16 flags` argument; new slot flag `TTS_FLAG_OBEYS_NOT_NULL_CONSTRAINTS` | 2b calls none of them (the scan slot of a custom scan is made by `ExecInitCustomScan`; the router uses `MakeSingleTupleTableSlot`). A slot the extension makes must not claim the flag: its tuples come from other nodes | none needed; rule in 4.4 |
| D3 | `PlannedStmt.resultRelations` (list) | `resultRelationRelids` (set); new fields `planOrigin`, `subrtinfos`, `rowMarkRelids`, `elidedNodes`, `extension_state` | The router and the fragment plans are made by hand (`makeNode(PlannedStmt)`, as `remote_scan.c` does and works on 19): the new fields are zero, which is valid. **[Needs verification]** that `planOrigin = PLAN_STMT_UNKNOWN` is harmless to everything that reads it (it is reporting data) | existing macros; optional `KS_SET_PLAN_ORIGIN(pstmt)` |
| D4 | `OnConflictAction`: `NONE`, `NOTHING`, `UPDATE` | adds `ONCONFLICT_SELECT` (`ON CONFLICT DO SELECT`, with `OnConflictExpr.lockStrength`) | The router and P6 accept `NONE`, `NOTHING` and (P6 only) `UPDATE`; every other value is refused. A walker that tests `action == ONCONFLICT_UPDATE` and treats the rest as `NOTHING` would ship `DO SELECT` unchecked | `KS_ONCONFLICT_IS_SELECT(oc)` (19: the comparison; 18: false); the walker uses an explicit allow-list of actions |
| D5 | `CopyStmt` options parsed into `CopyFormatOptions` with `binary`, `csv_mode`, `header_line` as an enum | `format` enum (`COPY_FORMAT_JSON` added; core refuses it for `COPY FROM`), `header_line` is an int (`HEADER n` skips lines), `ON_ERROR set_null`, `force_array` | The handler passes the options to `BeginCopyFrom` and never reads the parsed struct; refusals are by option **name** (`freeze`, `where`, `on_error`, `reject_limit`, `log_verbosity` is allowed) so the new values are refused too, and `FORMAT json` by the core's own error | none: by-name refusal |
| D6 | libpq results are plain `PGresult` | `libpqsrv_PGresult`: a wrapper allocated in **`CurrentMemoryContext` when the result is received**, freed by a reset callback of that context | A result that outlives the call that received it (a batch of a fragment, the in-flight result of a router batch, a probe answer kept across a rescan) is freed when the short-lived context current at receipt is reset, and used afterwards | rule in 4.4 and 10.5; `KS_LIBPQ_RESULTS_OWNED_BY_CONTEXT` (exists) and a new `ks_result_adopt(res, ctx)` (19: `libpqsrv_PGresultSetParent`; 18: identity) for the rare result that must change owner |
| D7 | `WindowFunc` has no null treatment | `WindowFunc.ignore_nulls` (`IGNORE NULLS`, `RESPECT NULLS`) | The deparse prints it, and one major runs per cluster, so a shipped window function means the same on both ends. **[Recommendation]** the walker still refuses a window function with `ignore_nulls != 0` until a test covers it | `ks_window_ignore_nulls(wfunc)` (19: the field; 18: 0) |
| D8 | `sortsupport.h` has inline `ApplyUnsignedSortComparator`, `ApplySignedSortComparator`, `ApplyInt32SortComparator` | removed | The merge calls `ApplySortComparator` only | none: rule in 8.1 |
| D9 | `planner_hook` has four arguments | five (`ExplainState *es`); `planner_setup_hook` and `planner_shutdown_hook` exist; `standard_planner` takes `es`; `subquery_planner(glob, parse, plan_name, parent_root, alternative_root, ...)` | Hidden already. `standard_planner` still passes `parse` itself to `subquery_planner` on both, so `root->parse == pd->parse` (4.1) holds. 2b does not use the new hooks | existing `KS_PLANNER_ES_*` |
| D10 | `UniquePath` / `UpperUniquePath`, `create_unique_path` | `UniquePath` is the old `UpperUniquePath`; semi-join unique-ification is `RelOptInfo.unique_rel` | A pushdown unit has no sub-links and so no semi join; the code never meets either | none |
| D11 | `Agg.numGroups` is `long` | `Cardinality` (`double`) | `partialize_plan` does not touch it; `create_agg_path` takes `double numGroups` on both | none |
| D12 | `ExecInsertIndexTuples(...)`, `RelationFindReplTupleByIndexExt` | flags argument, removed | Not used: the local batch of the router runs through SPI | none |
| D13 | `postgres_fdw` deparse refuses `ArrayCoerceExpr` | pushes it when the element coercion is a relabel only (`deparse.c`, `foreign_expr_walker`) | Confirms rule 4 of 5.1 (a coercion that is not a relabel is not shipped). Not adopted; `ArrayCoerceExpr` stays not shipped | none |

The tests of section 16 run on both majors. The 19 differences with an effect
on behaviour (D1, D4, D6) each have a test that fails if the hiding is missing:
a join query with an aggregate on 19 whose plan must show a pushed join and no
pushed join below a grouped relation (`062_pushdown_join`), `ON CONFLICT DO
SELECT` refused by the router and by P6 (`056_bulk_insert`), and a cancel in
every wait of a fragment scan on a build with assertions and with
`MEMORY_CONTEXT_CHECKING` (`055_fragment_foundation`).

## 3. Overview of the new paths

| Path | Stage | What each group returns | What the origin does |
|---|---|---|---|
| P1 scan (2a) | base relation | rows of one table that satisfy the shippable restrictions | joins, aggregates, sorts, limits |
| P2 join rows | the join relation of the whole level | rows of the join of all tables of the level | everything above the join |
| P3 partial aggregate | `UPPERREL_GROUP_AGG` | one row per group key, with the serialized transition state of each aggregate | final aggregation (`Finalize`), then everything above |
| P4 closed aggregate | `UPPERREL_GROUP_AGG` | final aggregate rows (the group key contains the distribution key) | concatenates, then everything above |
| P5 whole query | `UPPERREL_FINAL` | the final rows of the whole query, sorted if it has an `ORDER BY`, at most `limit+offset` rows | merges, applies `OFFSET` and `LIMIT` |
| P6 colocated insert | the whole statement | nothing (the count, and `RETURNING` rows) | adds the counts; no row passes through the origin |

P1 exists today. P2 to P5 are new and are called "pushdown paths". All of them
are created only when the analysis of section 6 says the statement is a
pushdown unit. A query that is a pushdown unit still gets P1 (it costs nothing
to keep) and the planner chooses by cost. P6 is the write counterpart (11.7);
the row router (11.1) is the write path for everything P6 does not cover. A
statement that is not a pushdown unit, for example one that reads a local table
(6.5), is answered by P1 exactly as in 2a.

## 4. Fragments: the machinery shared by P2 to P5 and the row router

A **fragment** is a `SELECT` statement text that each shard group runs against
its own buckets and answers with rows. The origin sends it to every other
group over the cached connection of `node_conn.c` and runs the same text on its
own buckets.

### 4.1 Where the text comes from

`planner_hook_body`, when it has decided that a top-level `SELECT` is a
pushdown unit, makes `pd->raw = copyObject(parse)` before it calls the
standard planner, and keeps `pd` in a static variable for the duration of the
call (restored in `PG_FINALLY`, exactly as `distscan_active` is). The hooks
recognize their statement by `root->parse == pd->parse` (`standard_planner`
passes `parse` itself to `subquery_planner`, so the pointer is the same, and a
statement planned by a function while this one is planned has another pointer).

A fragment is built from `pd->raw` by changing top-level clauses and deparsed
with `pg_get_querydef()` under the transmission modes
(`kingres_shard_set_transmission_modes()`, which fixes `DateStyle`,
`IntervalStyle`, `extra_float_digits`, `standard_conforming_strings`,
`bytea_output` and `search_path = pg_catalog`), the technique already used for a
routed statement. The `FROM`, `WHERE` and `GROUP BY` always come from the copy;
the select list of P2 and P3 comes from the planner's own target (the columns
the planner needs above the node), so that the columns the node returns are
exactly the columns the plan above expects. Every node that is deparsed has
passed the shippability test of section 5 in the form in which it is
deparsed.

The text starts with a marker comment (section 4.2) and, for a fragment that
the groups read through a cursor, is wrapped as
`DECLARE ks_frag_<n> NO SCROLL CURSOR FOR <text>` (the 2a form).

### 4.2 Markers and what a receiving group checks

The 2a marker `/* kingres:scan-fragment */` (its prefix is compared byte for
byte by an old node) is unchanged. New fragments carry, as a second comment,

```
/* kingres:frag v=2 kind=<query|partial|insert> place=<n> [ident=<n>] [ext=<name>/<version>[,...]] [rp=<n>] */
```

- `place` is the placement number of the colocation group in the origin's plan
  (4.3), a decimal `int64`. The group compares it with its own after planning;
  this is a fast failure and a guard against a demoted or stale node, and the
  argument that every bucket is read once does **not** rest on it (4.3 and
  10.2 give the argument).
- `ident` is used by the insert fragment of an explicit destination that is not
  in the catalog (11.9): the identity of the destination's partitions on this
  node as the sender read it.
- `ext` lists the extensions whose objects the statement uses and the version
  installed at the origin (5.5).
- `rp` is the number of shipped sub-selects (below). 2b never sends it.
- A marker with a key that this node does not know is refused with `KS001`, so
  that a later phase can add keys safely; so is a marker without `place`, except
  the one of an insert to an explicit destination that is not in the catalog,
  which carries `ident` instead (11.9).

A comment is inert text; a client that writes one gets nothing it could not get
without it, because a session is treated as forwarded only if it carries the
token of `forward_auth.c` (review S2-04), and the receiving planner hook
re-checks the statement instead of trusting the marker.

For `kind=query` and `kind=partial` the forwarded session accepts the statement
only if all of these hold, and raises `KS001` (with a detail naming the
failed rule) otherwise:

1. it is a `SELECT` without sub-selects, CTEs, row locking, set operations or
   data-modifying parts. **The one exception** is the shipped side of a
   repartitioned join of phase 12
   ([analytics-phase12-queries.md](analytics-phase12-queries.md), section
   3.1): a sub-select whose only range table entry is `pg_catalog.unnest` over
   `text[]` parameters, whose target list is casts of its columns and `NULL`
   constants of built-in or allowed types, and which has no other clause. It is
   accepted only when the marker has `rp=n` and the statement has exactly `n`
   such sub-selects. 2b implements this as a function
   (`fragment_shipped_side_ok`, `pushdown.c`) that the walker calls for every
   sub-select, and tests it with forged fragments (accepted with the right
   `rp`, refused without it, with another count, or with any other shape). The
   origin side has the same exception in eligibility (6.1 rule 2) as a function
   that returns false in 2b. Phase 12 then adds a producer and no new rule;
2. every relation is a distributed table of one colocation group or a
   reference table, with at least one distributed table, and no relation has
   row-level security or a `checkAsUser` other than the session role;
3. the whole statement passes the same shippability walker (section 5) that the
   origin applied, run **on this node** with this node's catalog, and every
   extension named by `ext` is installed here at exactly that version (5.5),
   and every object of an allowed extension the statement uses is named in
   `ext`;
4. this node is the primary of its shard group (`local_group_id >= 0`): a
   demoted old primary or a standby refuses every fragment kind. The 2a rule
   "run a statement locally only if every bucket it can touch is provably
   local" refuses such a node for a statement without the marker (it owns no
   bucket); the marker relaxes that rule, so this refusal is explicit;
5. after the statement has been planned, the placement number of the colocation
   group computed from the catalog tables now equals `place`.

What the marker relaxes is only this: the 2a rule "run a statement locally only
if every bucket it can touch is provably local" (`plan_forwarded`) would reject
a query that reads a whole distributed table. A fragment *means* "answer from
the buckets of this node", so it is accepted without a bucket proof. It does
not relax privileges (the session is the role of the origin's session), it does
not allow anything that is not shippable, and it never writes. The `insert`
kind is described in section 11.5.

The receiving planner hook handles a fragment after the analysis and after
`kingres_shard_ensure_fresh_partitions` has run for every distributed table of
the statement (4.4), as it does for any forwarded statement.

### 4.3 The placement check

**What 0.4 does.** The first draft of this note made every fragment carry the
"map version" `V` of the origin's plan and had the group compare it with its own
after planning, arguing that planning takes locks and that taking a lock
processes invalidation messages. That is not what the extension does, and the
argument does not hold. `kingres_internal_placement_hash()` says why: a commit
becomes visible a moment before its invalidation messages reach the other
backends, so a long-lived backend can read the new catalog and still hold the
old partition list. The version also counts every catalog change, so a
comparison of it would fail statements that no move touches. What `dist_scan.c`
(`ds_start`, `check_local_partitions`) compares instead is two numbers per
node, read from the catalog tables with the latest committed snapshot and not
from a cache:

- the **placement number** of the colocation group, `kingres.internal_placement_hash(colocation_id
  [, parent regclass])` (C: `kingres_shard_placement_hash_now`; the origin's
  cached value is `kingres_shard_placement_hash`): a sum of mixed terms, one per
  bucket, of the bucket, its group and (since 0.5) the node that is the primary
  of that group;
- the **identity of the partitions** of a table on a node,
  `kingres.internal_partition_identity(parent regclass)`: a sum over the OIDs of
  the partitions attached to it. Review finding S4-01 showed that the placement
  number alone is blind to a bucket that moves away and comes back inside the
  window (the placement is the same, the partition is another table); the
  identity sees it.

The scan asks every remote node for both, with the parent named (this call also
makes the node's relcache entry of the parent at least as new as the answer),
**before** it opens its cursor there, and again **after** all the cursors are
open. The placement of every node must equal the plan's, and the identity of
every node must equal what it was before the cursor was opened; the origin's
own placement is read again and its own partitions are compared with the
partitions of the plan. A difference raises `KS001` before the first row is
returned. The origin does **not** reload its catalog while it raises the error
(the reload would run in the failing transaction and wait for, and deadlock
with, the very switch that made the numbers differ, `dist_scan.c`); the next
statement meets the new catalog when the invalidation arrives, and the first
draft's statement that the origin "reloads before the error reaches the client"
is withdrawn.

**What a fragment does.** The same, for a fragment that reads one or several
tables of one colocation group (a pushed join reads several), as a procedure
that is written once (`placement_check.c`, step 2 of section 17) and used by
the 2a scan, the fragment scan, the fan-out write and the row router:

1. *Plan number.* When the scan starts, and again on every rescan, before
   anything is sent, the origin's cached placement number of the group equals
   the one stored in the plan (`plan_placement`). Otherwise `KS001`. This covers
   cached and generic plans, a plan made before a move or a failover, and a
   change of primary on the origin's own cache.
2. *Probe before.* For each node of the fragment set, in one statement:
   `SELECT internal_placement_hash(<c>, <t1>), ..., internal_partition_identity(<t1>), ...,
   <extension version>`, one term per distributed table of the fragment, in
   table OID order (reference tables never move and are not asked). The answer
   must have the plan's placement number; the identities are kept. The last
   column is the node's installed version of the extension
   (`pg_catalog.pg_extension`, a plain query that every version answers); a node
   that runs another version is refused with an error that names it and says
   to finish the upgrade, which is not retryable (rolling upgrade, section 14).
3. *Declare.* Every group is sent its cursor (`DECLARE`), concurrently, with
   the marker of 4.2. The group's own check (4.2 rule 5) is a fast failure.
4. *Probe after.* `SELECT internal_placement_hash(<c>), internal_partition_identity(<t1>), ...`
   on every node of the fragment set: the placement equals the plan's, every
   identity equals the one from the first probe. The origin's own placement is
   recomputed from the catalog tables, and the identities of its own partitions
   are compared as in 4.4.
5. No row is returned, and no `FETCH` is sent, before step 4 has passed for
   every node.

The existing fault-injection facility gets one point per gap, named `frag_<what>`
as the bucket-move points are named `move_<what>`: `frag_after_probe_before`,
`frag_after_declare`, `frag_after_probe_after`, `frag_local_open` and
`frag_first_fetch`. The tests of sections 10.2, 10.3 and 16 use them with
`:wait`, `:error` and `:kill` to place a switch or a failover in each gap.

**Why this is enough, and where the argument is.** The placement and the
partitions of a node change only in the transaction of a switch on that node.
Equal numbers before and after the cursor was declared mean that no switch
committed on the node in between, so the cursor was planned with the partitions
of the placement the plan assumed. The argument that a bucket is read exactly
once is section 10.2. It needs three facts that are properties of 0.4: the
numbers are read from the catalog tables and not from a cache; the identity
sees a bucket that left and came back; and the cursor holds `ACCESS SHARE` on
the parent until its remote transaction ends, which a `DETACH` waits for.

**What 2b needs from phase 10.** [shard-phase10.md](shard-phase10.md) plans a
monotonic `colocation.placement_epoch` that only a switch (a move or a split)
increments, in the transaction that changes the node's partitions, and that
`internal_placement_hash` and the origin's cached value both include
(`hash(epoch, placement)`). 2b asks for three things and for nothing else:

1. the epoch is part of the number that `internal_placement_hash` returns and
   that `kingres_shard_placement_hash` computes, under the **same names and
   signatures**, so that nothing in 2b is renamed;
2. it changes on every node whose partitions the switch changes, in that
   transaction, so a move away and back changes it twice (the sequence of
   review S4-01, and the same on a pair of groups that does not include the
   origin, the reviewer's unconfirmed second item);
3. it is a property of the colocation group, not of a table, so it covers every
   table of a join fragment with one number.

When that exists, the identity terms of the probes are not needed for the
exactly-once argument, and the probes of 4.3 shrink from `1 + n` numbers per
node (`n` tables) to one. Nothing else changes, because the probe statements
are built by two functions in `placement_check.c` (`probe_before_sql`,
`probe_after_sql`) and the comparisons by one; the identity terms are one
constant (`KS_PROBE_IDENTITY`) that phase 10 turns off once its own tests (a
move away and back during a fragment, on the origin's pair of groups and on
another pair, and a split in the middle) pass against fragments. Until then
the identity stays, because it is what closes S4-01 in 0.4. The explicit
destination of 11.9 keeps its identity check, because a table that is not in
the catalog has no epoch.

**What the check does not see.** A stale origin and an old primary that the
catalog change did not reach (a node that is cut off and not yet fenced) hold
the same old catalog and give equal numbers. The check cannot detect it; the
fencing of [ha-phase5.md](ha-phase5.md) part B (the lease) is what stops such a
node before the new primary accepts writes, and 2b adds no protection of its
own.

### 4.4 How the origin runs fragments ("Kingres Fragment Scan")

One custom scan runs all the fragments of a path:

- The plan stores the placement number of the colocation group and the
  fragment text. The set of groups is computed when the scan starts
  (`BeginCustomScan`, and again on rescan), **after** step 1 of 4.3 has shown
  that the origin's cache is the placement of the plan, from the pruning rules
  of 4.5 and the bucket lookup of 4.7. A group is the primary that the cache
  names for it (`kingres_shard_group_primary`); a node that is not the primary
  of its group (`local_group_id < 0`, 0.5) has no local part, and every group,
  its own included, is asked as a remote one. Its own partitions are a stale
  copy and are never read.
- The remote groups are started first: for each, `begin_xact`, the probe of
  4.3 step 2, then `PQsendQueryParams` of the `DECLARE`, so that the groups
  plan and start concurrently (this is what `ds_start` does today). The origin's
  own part runs after, as a cursor (below). Rows are then read in batches of
  `kingres_shard.scan_fetch_size` rows per group (existing setting), the next
  batch of a group being requested as soon as the current one is used up
  (`frag_send_fetch`), and only after the probe of 4.3 step 4 has passed.
- Before a connection to a node is used, the scan checks that this transaction
  has no open remote transaction on **another node of the same group**: a
  transaction that read or wrote through the old primary of a group and then
  meets the new one (after `promote_node`) would see two states of the group
  and would lose the writes it made on the first. Such a statement fails with
  `KS001` (10.3). The check is a loop over the connection cache with the node
  list of the catalog, as `kingres_shard_conn_drop_stale` does.
- **The origin's own part is the same text run through SPI**, as a cursor opened
  with `SPI_cursor_open_with_args(..., read_only = true)` and read in batches
  with `SPI_cursor_fetch`. For a read-only cursor `SPI_cursor_open_internal`
  uses `GetActiveSnapshot()` (spi.c, "if (read_only) snapshot =
  GetActiveSnapshot()"; the body is the same on 19), that is the snapshot of the
  statement that is running, so the local part sees what the other local scans
  of the statement see. **No SPI connection is held between calls of the
  node.** The cursor is opened and every batch is fetched inside its own
  `SPI_connect`/`SPI_finish` pair, with the batch copied into the scan's batch
  context before `SPI_finish`; the portal survives `SPI_finish` (its query
  string and plan are copied into the portal context in
  `SPI_cursor_open_internal`; PL/pgSQL returns cursors the same way) and is
  closed by `SPI_cursor_close` when the scan ends or restarts, and by the abort
  of the transaction on an error. A function that the statement calls between
  two rows may then use SPI freely. **[Needs verification]** by a test that
  calls such a function between rows and cancels at every wait.
- The text of the local part is prefixed with `/* kingres:local <nonce> */`,
  where `<nonce>` is a 64-bit random number made when the scan starts and held
  in a static variable only while that scan's SPI call is active. The planner
  hook recognizes the nonce **after** its analysis and after
  `kingres_shard_ensure_fresh_partitions` has run for every distributed table of
  the statement, and then plans the statement without routing and without the
  scan (it is the local part, by construction), applying `partialize_plan` for
  `kind=partial`. This replaces the 2a mechanism of
  `kingres_shard_in_fanout_local()` (`fanout.c`, `planner.c`), which recognizes
  the local part by comparing the whole statement text with the text being run
  and which returns from the hook **before** the catalog is read: any nested
  statement with the same text, run by a function that the statement calls,
  would also be planned without routing, and the local part would be planned
  against a partition list that nothing has made as new as the catalog (the
  class of review finding S4-01 and of the "argued, not forced" case of the 0.3
  review). A nonce cannot be guessed by such a function. Step 2 of section 17
  moves the 2a fan-out local part to the same order and the same mechanism,
  because it is the same function.
  The check of 4.3 applies to the local part like to a remote one: the
  identities of the origin's own partitions of every table of the fragment are
  read before the cursor is opened and after it is open, and the placement is
  recomputed from the catalog tables. In `REPEATABLE READ` and `SERIALIZABLE`,
  `ensure_fresh_partitions` also raises the serialization failure for a bucket
  that arrived after the snapshot (`check_partitions_in_snapshot`), which the
  local part needs for the same reason a routed statement does.
- The local rows come back as `Datum`s of the types the plan declares (for a
  partial fragment `bytea` for an `internal` state); remote rows go through the
  input functions of the column types, as in 2a. Both fill the same scan tuple.
- **Memory is bounded** by one batch per group (plus the SPI batch): nothing is
  materialized. A cursor on a group is closed when the scan ends early
  (`LIMIT` above, a closed portal), as `ds_stop` does today.
- **Who frees a result.** On 19 a libpq result is wrapped in an object allocated
  in the memory context that is current when the result is received, and freed
  when that context is reset (2.3, D6); on 18 the code that got it frees it.
  Every result that is kept across calls (the batch of a group, the answer of a
  probe) is therefore received with the scan's own batch context current, is
  owned by one structure (`FragBatch`) and is released by one function
  (`frag_batch_release`) that is also the reset callback of that context, which
  does nothing when `KS_LIBPQ_RESULTS_OWNED_BY_CONTEXT` is true. Nothing else
  calls `PQclear` on a batch. A result is never received while the executor's
  per-tuple context is current. The tests of section 16 run every fragment path
  with a cancel in each wait, on a build with assertions and
  `MEMORY_CONTEXT_CHECKING`.
- Rescan restarts the fragments with the new parameter values, through the
  whole of 4.3; the node supports no parameterized path (the pushdown paths are
  created only for paths without `param_info`).
- It is not parallel-aware, and the paths set `parallel_safe = false` and the
  relations `consider_parallel = false` (the base relations by the 2a hook, the
  join relation by the P2 hook, the upper relations by the P3 to P5 hooks), as
  2a does (review S2-08). A plan of the node that reaches a parallel worker
  (a cached plan, a function) raises the 2a error in `BeginCustomScan`, whatever
  the planner did; and the planner hook of a worker raises the same error
  instead of planning without pushdown, so that a `PARALLEL SAFE` function that
  runs a pushdown statement cannot answer from a snapshot of its own.

The 2a scan keeps its local part as a planner path. The pushdown paths run the
local part as a fragment because there is no local path for a join, an
aggregate or an ordered result of the whole level, and building one by hand
would mean calling `add_paths_to_joinrel` on a scratch relation, which is not an
interface of the planner.

Cost of this choice, accepted: the local part is planned a second time (the
outer plan only prices it) and has no parallel workers, because a cursor cannot
be parallel (`DECLARE` of a remote fragment is a cursor too, so no group runs
a fragment in parallel; partial fragments also set
`max_parallel_workers_per_gather = 0`, 7.3). The parallelism of the cluster is
across groups. A table that is large on every group is scanned by one backend
per group; the tests of step 6 measure that against the 2a scan so that the
cost is known.

### 4.5 Which groups are asked: pruning

A fragment is sent to a group only if the group can own a bucket that the query
can touch. The set is computed from the restriction clauses the planner
attaches to the relation (`rel->baserestrictinfo`, which has the right range
table indexes at path time, unlike the parse-level analysis of `planner.c`):

- `dist_col = <constant>`, `dist_col IN (<constants>)`, `dist_col =
  ANY(<array constant>)`, and the same with an external parameter (evaluated
  when the scan starts, so a generic plan prunes too);
- the operator must be an equality operator of the hash operator family of the
  column, and the clause's input collation must equal the collation the table
  was distributed with and be deterministic (the rule of `restrict_from_conjunct`,
  review finding 2);
- for a pushed join, a restriction on the distribution key of any relation of
  the key class (section 6.3) prunes for the whole join;
- an `OR` of such restrictions prunes to the union; anything else prunes
  nothing.

The bucket of a constant is found by `kingres_shard_try_bucket`, which uses the one
bucket lookup of 4.7; pruning never computes `hash % bucket_count` itself and walks
the buckets of the group with the accessors of 4.7. When nothing prunes,
every group that owns a bucket of the colocation group is asked. When pruning
leaves no group, the fragment set is empty and the scan returns no rows (a
partial aggregate over an empty set returns the initial states from no group, and
the final aggregate then returns the right empty result, section 7.6). The 2a
scan gets the same pruning; it asked every group before.

### 4.6 Errors

An error on a group (lost connection, `KS001`, a remote error) raises an
`ERROR` at the origin; the transaction aborts, which rolls back every remote
transaction (`conn_xact_callback`). `node_timeout` bounds the waits that cannot
be interrupted, as in 2a. A cancelled statement cancels nothing remotely; the
batch in flight is read and discarded, as `frag_drain` does, because a cancel
would abort the remote transaction.

One function (`fragment_error`, used by every path of this note and by the 2a
scan) turns what a failed group can mean into the right error. The error is
`KS001`, with a detail that names the node and says that a failover may be in
progress, when all of these hold:

- the failure is one that a change of primary causes: a lost or refused
  connection (SQLSTATE class 08), `57P01`, `57P02` or `57P03` (the server is
  shutting down or starting), `25006` or a "read-only transaction" refusal (a
  demoted node), or a `KS001` of the group itself; and
- the origin's catalog has a registered standby for the group of that node, or
  the node is no longer the primary of its group in the origin's cache.

A failure on a group that has no standby registered stays the plain connection
error: `KS001` says "repeat the statement", and repeating a statement against a
node that is simply down fails at the next connect with the connection error,
so a client is not led into a loop. **[Recommendation]** This makes the
statement end in a retryable error in the only situation in which a retry can
help, and it never produces a row.

### 4.7 One bucket lookup

Today the bucket of a key is computed in one place, `kingres_shard_try_bucket`
(`hashing.c`), as `hash % bucket_count`, and the code that walks the buckets
loops `for (b = 0; b < bucket_count; b++)` over `coloc->bucket_group[b]`
(`planner.c`: `all_buckets_local`, `plan_distributed_write`; `dist_scan.c`:
`remote_groups_of`, `ds_set_rel_pathlist`; `kingres_bucket_for_*`). Phase 10
gives every bucket its own modulus (a bucket is a `(modulus, remainder)` pair
and routing resolves a hash through an array of `max_modulus` entries), and the
roadmap of [architecture.md](../architecture.md) lists "every place that computes a
bucket as `hash % bucket_count`, in the code and in the phase 2b design" as a
cross-phase point: a place that is missed gives a wrong answer without an error.
So 2b has no such place. Every bucket computation, in the pruning of 4.5, the
row router of section 11, `COPY`, P6, `kingres.bucket_for` and the explicit
destination of 11.9, goes through two functions that live in `hashing.c`
(declared in `hashing.h`):

```
/* The hash of one key value, with the hash function of the distribution
 * column's type; false when the type has no suitable hash function. */
bool  kingres_shard_hash_of_value(Oid column_type, Oid collation, Oid value_type,
                                  Datum value, bool isnull, uint64 *hash);

/* The number of the bucket that holds a hash value in this colocation group.
 * Today: hash % coloc->bucket_count.  After phase 10: the entry of the routing
 * array for hash % coloc->max_modulus. */
int   kingres_shard_bucket_for_hash(const KsColocation *coloc, uint64 hash);
```

`kingres_shard_try_bucket` and `kingres_shard_bucket` keep their names and are
written as the two calls above; their `int bucket_count` argument becomes a
`const KsColocation *` (the callers in `planner.c` and `hashing.c` change in
step 1). A caller that needs to walk the buckets uses
`kingres_shard_coloc_nbuckets(coloc)`, `kingres_shard_coloc_bucket(coloc, i)`
(the number of the i-th bucket) and `kingres_shard_bucket_group(coloc, bucket)`;
none of them assumes that bucket numbers are `0 .. bucket_count - 1`. A
destination that is not in the catalog (11.9) is described by a `KsColocation`
built by the caller, so the same functions serve it. `bucket_count` is read
only in `catalog.c` (loading), `hashing.c` and `dist_table.c` (creating the
partitions); a test (`055_fragment_foundation`) searches `src/*.c` for
`bucket_count` and `% ` on a hash and fails when it appears in another file, and
the new functions call `kingres_shard_assert_bucket_layout` of phase 10 once it
exists.

## 5. Shippability

The rule is the one `shippable.c` implements and the 0.1 and 0.2 reviews
shaped; this section states it in one place and says what 2b adds.

### 5.1 The rule for an expression that depends on data

An expression that contains a `Var` of a distributed or reference table may be
evaluated on another node only if **every** node of it satisfies all of:

1. functions, operators and aggregates are **built in** (`oid <
   FirstGenbkiObjectId`, which `postgres_fdw` also uses as its `is_builtin`) or
   are **objects of an allowed extension** (5.5), and in both cases are
   `IMMUTABLE`. Stable functions are not shipped: their result may depend on
   session state that differs between nodes (`TimeZone`, `lc_*`, `search_path`,
   `now()`). The first draft said "built in" and nothing else; that made the
   approximate aggregates of `kingres_sketch`, which are immutable, partial-capable
   and parallel safe, impossible to push down;
2. every type of a column, a constant, a parameter, a result or a transition
   state is built in (`type < FirstGenbkiObjectId`) or a type of an allowed
   extension (5.5), so that its text form and its meaning are the same on every
   node. OIDs between 10000 and 16383 are assigned by `genbki` and the `initdb`
   scripts and are not used. **The type `money`, and any array, range,
   multirange or composite type that contains it, is not shipped**: its text
   form depends on `lc_monetary`, which the transmission modes do not fix (5.3),
   and a partial `sum(money)` or a returned `money` column would be read by the
   origin under another setting. A statement that needs it is not a pushdown
   unit; the 2a scan keeps reading such a column as it does today. The same
   exclusion applies to the restrictions of the 2a scan (the shared test is
   `type_ok` in `shippable.c`), which closes a latent case of the class of
   review S2-03;
3. the collation of every comparison, ordering or grouping is the default
   collation or `C` (`collation_ok`). The default collation is the same on
   every node because `add_node` refuses a node whose default collation,
   `LC_CTYPE` or provider version differs (README, "Nodes must agree"), and
   the default collation of a database is deterministic. A column or an
   expression with another collation, in particular a non-deterministic ICU
   collation, makes the clause local;
4. no conversion goes through the text form of a value (`CoerceViaIO`):
   `DateStyle`, `IntervalStyle`, `extra_float_digits`, `bytea_output` and
   `TimeZone` change it (review S2-03). An `ArrayCoerceExpr` is not shipped
   either (19's `postgres_fdw` ships it only when the element coercion is a
   relabel, 2.3 D13);
5. no system column; no `tableoid`, `ctid`, `xmin` (the values name one node's
   layout);
6. no `Const` of a `reg*` type whose value is an OID of a user object or of an
   object of an extension (the OID is local); built-in objects are fine;
7. only the node kinds `ship_walker` lists, and for `OnConflictExpr` only the
   actions `NOTHING` and `UPDATE` (2.3 D4). Anything else (`SQLValueFunction`
   such as `current_user`, `FieldSelect`, `RowExpr`, `SubPlan`, `XmlExpr`,
   `JsonExpr`, a window function with `IGNORE NULLS`, ...) is not shipped.

Every one of these is a plain function of the expression, so it is decided once
at plan time and has a test (section 16).

### 5.2 The rule for an expression that depends on no data

A maximal subexpression that contains no `Var`, no `Aggref`, no `WindowFunc` and
no volatile function (immutable **or stable**, built in or user-defined) is
**evaluated at the origin when the scan starts and shipped as a parameter**. This is
what makes `WHERE ts > now() - interval '1 day'`, `WHERE d = current_date`,
`WHERE x = my_stable_function(1)` and `LIMIT least($1, 100)` shippable: the
value is computed once by the origin's own functions under the origin's own
session, which is the meaning the statement has on a single node (stable
functions may be evaluated once per scan). The result type must satisfy rule
2 of 5.1; if it does not, the expression is not shippable. Parameters made this
way are numbered after the statement's external parameters and sent exactly as
those are (`fragment_params`). A volatile function never ships and never
folds: a clause that has one stays at the origin.

### 5.3 Session settings and the rest of the environment

| Thing | How it is kept equal |
|---|---|
| `DateStyle`, `IntervalStyle`, `extra_float_digits`, `standard_conforming_strings`, `bytea_output` | Fixed on the remote session (connection options and `SET LOCAL` per transaction, 2a) and fixed around every deparse and every output of a parameter (`kingres_shard_set_transmission_modes`). The local fragment runs under the same fixed settings (the SPI call is wrapped in the same nest level). |
| `TimeZone` | Sent to the remote transaction and re-sent when the session changes it (`sync_timezone`, 2a). Functions that depend on it are stable and not shipped (5.1 rule 1); it matters for the text form of `timestamptz` that crosses the wire, which carries its own offset. |
| `search_path` | `pg_catalog` on the remote session; the deparser qualifies every relation, function and type. The fragment text never contains an unqualified user name. |
| Collations | 5.1 rule 3. |
| `lc_messages`, `lc_time`, `lc_monetary`, `lc_numeric` | Not controlled. Results of `to_char` with locale-dependent patterns (`TM` prefix) and of `money` depend on them; the functions are marked immutable by PostgreSQL, so they ship, except that a `money` value is never shipped (5.1 rule 2). **[Needs verification]** whether the remote fixed settings should also pin `lc_time`; settle it with a test that sets `lc_time` differently on two nodes and compares `to_char(d, 'TMDay')`. Until then the README says the nodes must have the same `lc_*` settings, as it already says for `lc_monetary` and `lc_numeric`. |
| Row-level security | A table with row-level security is refused by `create_distributed_table`, and `ALTER TABLE ... ENABLE ROW LEVEL SECURITY` and `CREATE POLICY` are refused by the utility hook. As defence in depth, the analysis refuses a pushdown unit when any range table entry has `securityQuals` or its relation has `relrowsecurity`, and the receiving group checks the same (4.2 rule 2). |
| Security-barrier views | A view is a subquery range table entry after rewriting, which is not a pushdown unit (6.1); it is read by the 2a scan, where the view's own qualifications run first at the origin. |
| `checkAsUser` | A range table entry with a `checkAsUser` other than the session role makes the unit refuse (`kingres_shard_refuse_view_owner`, 2a). |
| User-defined objects | Never shipped as part of an expression that depends on data; only through 5.2 as a computed value. |

### 5.4 How a non-shippable expression composes with each pushdown

A pushdown path is **all or nothing for its level**: it is created only if
every expression it would ship is shippable. When some are not:

| What is not shippable | Result |
|---|---|
| a conjunct of `WHERE` or of an inner join's `ON`, query has only inner joins | P2 is still created; the conjunct is left in the custom scan's `qual` and applied at the origin to the rows returned (the fragment returns the columns the conjunct needs). This is the 2a rule for P1 (shippable conjuncts shipped, all rechecked). |
| the same, but the query has an outer join | No pushdown path; the 2a scans of each table push their own shippable restrictions. |
| a conjunct, with P3, P4 or P5 | No such path (the conjunct would have to be applied before aggregation or limiting, which is at the groups). |
| an aggregate argument, a `FILTER`, a group expression, a `HAVING` expression, an order expression, a window | No P3, P4 or P5; P2 if the rest is shippable. |
| a target-list expression | P5 is not created. P2, P3 and P4 are unaffected for a target expression above the pushed part: the planner computes it at the origin from the columns the path returns. |
| a `LIMIT` or `OFFSET` expression | The limit is not pushed; the ordered merge still is. |

### 5.5 Objects of extensions

**Decision [Recommendation].** The functions, operators, casts, types and
aggregates of an extension may be shipped when the extension is on a list that is
**compiled into `kingres_shard`**, when the version installed on the node is one
the list accepts, and when the object meets the object rules below. The list has
one entry today, `kingres_sketch` (`contrib/kingres_sketch`), version `1.0`. A
setting can **narrow** the list and cannot add to it.

```
typedef struct KsShippableExtension
{
	const char *name;			/* "kingres_sketch" */
	const char *const *versions;	/* the installed versions that share one state format, NULL terminated */
} KsShippableExtension;
```

**Object rules**, checked in `shippable.c` by `ext_object_ok(classid, objid)`,
which `func_ok`, `op_ok` and `type_ok` call for an object whose OID is not below
`FirstGenbkiObjectId`:

1. `getExtensionOfObject(classid, objid)` is an extension of the list, and its
   installed version (`pg_extension.extversion`, by the syscache) is in the
   entry's `versions`;
2. a function or aggregate is written in `C` or `internal` (the code is the
   extension's shared library, not a body that a user can change), is
   `IMMUTABLE` in the catalog, is not `SECURITY DEFINER`, has no `proconfig`
   (no `SET` clause that would change the session inside the call), and its
   argument and result types are built in or types of the list. An aggregate
   also needs, for P3, `aggcombinefn`, and for a transition type `internal`,
   `aggserialfn` and `aggdeserialfn` (the planner's own test, 7.1);
3. a type is a member of the extension, with input, output, `send` and
   `receive` functions that are members too; its text form is fixed by the
   extension (the sketch types are `\x` and the hex digits of the bytes);
4. an operator or a cast is a member and its function meets rule 2.

The walker records `name/version` of every extension it used in the context; the
marker carries them (`ext=`, 4.2) and the group checks them against its own
catalog. A node that runs another version, or does not have the extension,
raises `KS001` with the detail "extension kingres_sketch is at version X here and
Y at the origin". The statement is not retryable until the upgrade is finished,
and the detail says so. This cannot be proved at plan time, because the origin
does not know the version on the other nodes; it is the one place where a
pushed statement can fail for a reason the scan would not have: the escape is
`SET kingres_shard.shippable_extensions = 'none'`, with which the planner does
not ship extension objects and the scan answers (the scan reads sketch columns
as text and runs the aggregate at the origin).

**The setting.** `kingres_shard.shippable_extensions` (`USERSET`, default
`all`) is `all`, `none`, or a comma-separated list of names from the compiled-in
list; another name is an error when it is set. It is read at the origin only,
and it is a narrowing: it cannot make anything shippable that the code does not
allow, so an ordinary user may set it. It is useful to stop shipping during a
rolling upgrade of an extension.

**Why a compiled-in list, and not "any immutable function of any extension" or a
list the administrator edits.** Think of a role that can create a function: any
role with `CREATE` on a schema can, and a database owner can create an extension
that is marked `trusted`.

- If the rule were "any immutable function", the role would write `app.f`,
  label it `IMMUTABLE` (a label that nothing checks: it can read a table, a
  setting, the clock) and call it in a query. Functions are not synchronized
  between nodes (README, "Roles, schemas created for tables, and user-defined
  types are not synchronized"), so the fragment would call, on every group,
  whatever `app.f` is *there*: absent (an error), or a body that another role
  wrote on that node, run with the privileges of the calling role on that node.
  Where the caller is more privileged than the author (a superuser with a view
  or a query that names the schema) this runs code of a low-privileged role on
  every node, which today runs only on the node the user chose. And a function
  that differs per node makes the answer depend on which group a row lives on,
  with no error: the class of S2-05.
- `SECURITY DEFINER` and `SET` clauses change who runs the body and the session
  while it runs; the object rules exclude them.
- If the rule were "any object of any extension", a third-party extension with
  a function that is mislabelled `IMMUTABLE`, or whose state format changed
  between two versions that two nodes run, would return wrong answers silently.
  An extension that a superuser installs is not a new capability for the
  superuser, but the guarantee "a pushed statement means what it means at the
  origin" needs a reviewed list.
- A list the administrator edits (a catalog table) lets anybody with write
  access to it widen what runs on other nodes. A list in a setting that adds
  names would be superuser-only but still moves the review of determinism from
  the code to an operator's judgement, with a silent wrong answer as the cost of
  a mistake. Nothing in this phase needs it. Adding an extension is a code
  change with its own test, as phase 12 says
  ([analytics-phase12-queries.md](analytics-phase12-queries.md), section 3.3).

What the list relies on in the extension (from its README): every function is
`IMMUTABLE` and `PARALLEL SAFE`, none is `SECURITY DEFINER` or has a setting,
the aggregates have combine, serial and deserial functions, and the state of an
aggregate is the same byte string as the value of its type. Its hash of a key
depends on the database encoding and on `float8`/byte-order details; the
extension's `self_test()` must be equal on nodes that exchange sketches. 2b
checks the extension version; comparing `self_test()` (and the encoding) belongs
to `add_node` and `check_cluster`, which phase 12 changes, and this note asks
for it there. The aggregates push down through P3 (7.1, 7.2) and, when the query
is closed, P4.

**The sub-select of phase 12.** A repartitioned join ships rows to a group as a
sub-select over `unnest` of `text[]` parameters whose columns may be of a type of
an allowed extension (carried, not computed on). Rule 2 of 5.1 lets such a type
through as a value; the group's rule 1 of 4.2 lets the one shape of the
sub-select through, counted by `rp`.

## 6. The pushdown unit: analysis in the planner hook

The analysis runs in `planner_hook_body` on a statement that the existing
decision has classified as "spans groups" and that is a `SELECT` (including a
cursor, `CREATE TABLE AS` and `EXPLAIN` of one: the planner hook receives the
`Query` of the `SELECT`). It produces `PushdownInfo` or a refusal reason; the
reason is shown by `EXPLAIN (VERBOSE)` on the 2a scan (section 12).

### 6.1 Eligibility

A query is a pushdown unit when all of these hold:

1. it is a top-level `SELECT` (`root->query_level == 1`), on a cluster with
   more than one group, in a session that is not a forwarded session, with
   `kingres_shard.enable_pushdown`, `enable_distributed_scan` and
   `enable_routing` on. In a parallel worker the planner hook raises the 2a
   error ("cannot be read from a parallel worker") instead of planning without
   pushdown, so that a function marked `PARALLEL SAFE` cannot answer from the
   snapshot of a worker (review S2-08);
2. it has no `WITH`, no set operation, no sub-links, no `FOR UPDATE`/`SHARE`,
   no set-returning function in the target list, and every range table entry is
   a plain table (`RTE_RELATION`), a join (`RTE_JOIN`) or the group entry
   (`RTE_GROUP`); a subquery, a view, a function, a `VALUES` list, a CTE
   reference or a table sample makes it not a unit. The one exception, a
   sub-select that is the shipped side of a repartitioned join, is decided by
   `unit_subselect_exception()`, which returns false in 2b (4.2 rule 1 has the
   shape; phase 12 supplies the producer);
3. every table is a distributed table of one colocation group or a reference
   table, at least one is distributed, and no ordinary local table is present
   (a local table does not exist on the other groups). Such a statement is not
   refused: it is read by the scan, section 6.5;
4. every distributed table is connected to the others by distribution-key
   equalities that make one key class (6.3);
5. the join types are those of section 9.1;
6. no range table entry has row-level security or a different `checkAsUser`
   (5.3);
7. grouping sets are not used (they are not partial-capable either; a query
   with grouping sets stays on the 2a scan);
8. every type that a fragment would carry is shippable (5.1 rule 2: `money` is
   not), and every object of an extension is on the list (5.5).

A statement that fails any rule is planned exactly as in 2a. A statement
inside a subquery, a CTE, a view, a function or the source of an `INSERT` is
planned at its own level (`query_level > 1`) and is not a unit in 2b; it uses
the 2a scan with the new pruning and the placement check. The source `SELECT` of
a router `INSERT` is the exception that confirms the rule: the router runs it
through SPI as a statement of its own, which the hook plans at level 1 and may
make a unit (11.1).

### 6.2 Reference tables

A reference table is on every group, so it is *replicated*. A fragment that
reads it reads the group's own full copy. It never makes a query a unit by
itself (rule 3 needs a distributed table). Inside the join rules of section 9
it behaves as a table that matches on every group.

### 6.3 The key class

For each distributed range table entry `r`, the key variable is the `Var` of its
distribution column. Two key variables are *equated* by a conjunct `x = y` when:

- the operator is an equality operator of the hash operator family of the
  colocation's distribution type, the clause's input collation equals the
  colocation's collation and is deterministic, and both sides are bare `Var`s
  (a `RelabelType` is looked through) of key variables of this colocation group;
- the conjunct is in the `WHERE` clause or the `ON` clause of an **inner** join,
  or in the `ON` clause of the outer join whose two arms contain the two
  variables.

The key class is the connected set of key variables under these equalities.
The unit needs one class that contains every distributed table of the query.
A self join is two entries. A restriction `k = <constant>` on a member of the
class prunes for the whole class (4.5).

Why this is exact: tables of one colocation group have the same bucket layout
(the same buckets, and after phase 10 the same moduli, because a split applies to
the bucket of the whole group), the same distribution type and collation and the
same placement, so two rows whose keys are equal under a hash-compatible
deterministic equality have the same hash, which the group's one bucket lookup
(4.7) maps to the same bucket, and live on the same group. Under a join on the
key, every output row is formed from rows of one group.

### 6.4 A closed query

A query is **closed** when every output row depends on the rows of one group
only, which lets a group compute its final rows. Let `K` be the key class. A
query is closed iff all hold:

- every aggregate is inside a grouping whose `GROUP BY` list contains a bare
  `Var` of `K` that is **not on the nullable side of an outer join** of the
  query (a `NULL` key produced by an outer join forms one group in the result
  but its rows come from several groups); with no `GROUP BY` and an aggregate,
  the query is not closed;
- a `DISTINCT` has a bare `K` variable (not nullable-side) in its select list;
  `DISTINCT ON` has one in its `DISTINCT ON` list;
- every window clause has a bare `K` variable (not nullable-side) in
  `PARTITION BY`;
- there is no `GROUP BY` without an aggregate unless it contains such a
  variable (grouping alone is a `DISTINCT`);
- a query with none of these (no aggregate, no `DISTINCT`, no window) is closed.

A closed query is correct to run as a whole on each group and to combine by
concatenation (or ordered merge). A query that is not closed can still use P2
(rows) and P3 (partial aggregate).

### 6.5 Reads that mix a distributed table with a local table

ADR 0022 rule 1 says that a `SELECT` that no faster path accepts is answered by
the scan, and that a statement the scan can run is never refused. The 2a code
refuses one read today, and this section answers it.

**What the code does** (`planner.c`, `planner_hook_body`; the message is at
line 1607). The statement contains a distributed table, or several, and a local
(ordinary, not distributed, not reference) table. Three cases:

1. The statement spans groups (a distributed table without a key restriction,
   or two keys that hash to different buckets): `can_distributed_scan` is true
   and `plan_with_scan` plans it with the scan. The local table is read locally,
   the distributed one by the scan. **Answered.**
2. Every distributed table is restricted to one key and the key's bucket is on
   this node's group: the statement is planned locally. **Answered.**
3. Every distributed table is restricted to one key and the key's bucket is on
   **another** group: the statement belongs to that group, and the routing code
   would deparse it whole and run it there. It cannot, because the local table
   does not exist on the other node, so the branch raises "statements that
   combine distributed tables of another shard group with local tables are not
   supported yet" (`a.has_nondist`). **Refused**, and `t/002_routing.pl`
   asserts the refusal. The same branch refuses a `SCROLL` cursor on a
   statement that belongs to another group ("scrollable cursors on statements
   of another shard group are not supported yet"), which is a refused read too.

So the message belongs to a narrower case than its wording says, and that case
is a read that the scan answers perfectly well: `SELECT * FROM kv, loc WHERE
kv.id = 5`, with the bucket of 5 on another group, is the scan of `kv` (with the
restriction shipped, and with the pruning of 4.5 asking only the owner of the
bucket) joined at the origin with `loc`.

**What 2b changes.** In that branch, before the error: if the statement is not a
write to a distributed table (`writes_hash_dist_walker` is false) and
`can_distributed_scan` is true, it returns `plan_with_scan(...)`. This covers a
`SELECT`, an `INSERT` or a `CREATE TABLE AS` whose target is a local table, and a
`SCROLL` cursor (`standard_planner` adds a `Material` node above a plan that does
not support backward scan, `planner.c:471`, and the custom scan does not set
`CUSTOMPATH_SUPPORT_BACKWARD_SCAN`). A statement that writes a distributed table
and also reads a local table stays refused with the existing message, because it
is a write and the row-locking and write rules of 2a (section 18) cover it.
`MERGE` keeps its own message. The statement is correct for the reason all 2a
scans are: every restriction is applied again at the origin.

Tests (`055_fragment_foundation`, with the comparison harness): the statement of
`t/002_routing.pl` becomes an equality against the single node, from each of the
three nodes, with the bucket of the key on the origin's group and on another
group; a join of a remote-key table with a local table and an aggregate over it;
`INSERT INTO loc2 SELECT ... FROM kv WHERE id = k` with the key on another
group; a `SCROLL` cursor on a remote-key statement fetched forward and backward;
and the same statements with `enable_pushdown = off` and with pruning disabled by
a restriction the planner cannot use, to show that pruning changes the cost and
not the answer.

## 7. Aggregate pushdown

### 7.1 Decision from the catalog

PostgreSQL decides if a split aggregation is possible in two places that this
design uses unchanged: `preprocess_aggref` (`prepagg.c`) sets
`root->hasNonPartialAggs` when an aggregate has `aggorder` or `aggdistinct`, or
no `aggcombinefn`; and `root->hasNonSerialAggs` when its transition type is
`internal` and it lacks `aggserialfn` or `aggdeserialfn` (and for
`array_agg` when the argument type has no `send`/`receive`).
`can_partial_agg` returns false for grouping sets, for a query with neither an
aggregate nor a `GROUP BY`, and when either flag is set. The result reaches the
hook as `extra->flags & GROUPING_CAN_PARTIAL_AGG`. **P3 is created only when
that flag is set**, plus the extension's own rules of 7.2.

The built-in aggregates, classified by running a script over
`src/include/catalog/pg_aggregate.dat` of this tree (the script is part of
the test `pushdown_agg_catalog`, which repeats the classification against
`pg_aggregate` at run time and compares it with an expected file, so a change in
a later PostgreSQL release shows up):

| Class | Catalog fact | Aggregates (built in) | Use |
|---|---|---|---|
| Partial-capable | `aggcombinefn` set; `aggtranstype = internal` implies `aggserialfn` and `aggdeserialfn` set | `any_value`, `array_agg`, `avg`, `bit_and`, `bit_or`, `bit_xor`, `bool_and`, `bool_or`, `corr`, `count`, `covar_pop`, `covar_samp`, `every`, `max`, `min`, `range_intersect_agg`, `regr_avgx`, `regr_avgy`, `regr_count`, `regr_intercept`, `regr_r2`, `regr_slope`, `regr_sxx`, `regr_sxy`, `regr_syy`, `stddev`, `stddev_pop`, `stddev_samp`, `string_agg`, `sum`, `var_pop`, `var_samp`, `variance` | P3 and P4 |
| No combine function | `aggcombinefn = 0` | `json_agg`, `json_agg_strict`, `jsonb_agg`, `jsonb_agg_strict`, `json_object_agg` and `jsonb_object_agg` (each with `_strict`, `_unique`, `_unique_strict`), `range_agg`, `xmlagg` | P4 only (closed) |
| Ordered-set and hypothetical-set | `aggkind` is `o` or `h` | `cume_dist`, `dense_rank`, `mode`, `percent_rank`, `percentile_cont`, `percentile_disc`, `rank` | P4 only |
| Internal state without serialization | `aggtranstype = internal` and no `aggserialfn` | none among the built-in aggregates (the script finds none); the planner flag handles a user-defined one | none |

A call is also excluded from P3 (and stays possible in P4 when closed) when it
has `DISTINCT`, an `ORDER BY` inside the call (`aggorder`; ordered-set
aggregates always have it), or when the whole query has grouping sets. `FILTER`
is allowed: the filter is part of the transition (it is in `AggTransInfo`) and
is deparsed into the fragment.

Aggregates of an allowed extension (5.5) are classified by the same planner
flags and meet the same rules. The aggregates of `kingres_sketch` have
`aggcombinefn`, `aggserialfn` and `aggdeserialfn` and a state of type
`internal`, so they are partial-capable: the group returns the serialized state
as `bytea` (which for these aggregates is the byte string of the value of
`hll`, `topn` or `tdigest`; the state of `approx_percentile` also holds the
fraction), the origin deserializes and combines, and the final
function runs at the origin. They are listed with the built-in aggregates in the
test `pushdown_agg_catalog`, which recomputes the classification from
`pg_aggregate` for every aggregate that `ext_object_ok` accepts.

### 7.2 Rules of the extension on top of the planner's

- the aggregate is built in (`aggfnoid < FirstGenbkiObjectId`, immutable by
  `func_ok`) **or an object of an allowed extension** (5.5, rule 2 of its
  object rules). Any other aggregate, even one with a combine function, is not
  split, because its transition type may not exist on the other nodes and its
  functions are not shippable (it forces the aggregation to the origin, as a
  non-shippable expression does, 5.4);
- the transition type is a built-in type or a type of an allowed extension, or
  `internal` with serialization (the partial column then has type `bytea`), so
  the partial state can cross as text;
- arguments, filter, group expressions and `HAVING` are shippable (5.1, 5.2);
- the group expressions are hashable or sortable at the origin (the planner
  checks this itself through `GROUPING_CAN_USE_HASH` and `_SORT`);
- every extension the query uses is named in the marker (`ext=`) and has the
  same version on the group (4.2 rule 3).

### 7.3 How a group produces partial states

No SQL expression returns the transition state of an aggregate, so the
fragment is the ordinary text `SELECT <group exprs>, <aggregate calls> FROM ...
WHERE ... GROUP BY ...` (no `HAVING`, `ORDER BY`, `LIMIT`, window or
`DISTINCT`), with `/* kingres:frag v=2 kind=partial place=<n> ... */` (4.2), and the group
turns its **plan** into a partial plan:

1. The planner hook of a session that is forwarded, or that runs the local
   fragment (nonce), recognizes `kind=partial`. It plans the statement with
   `standard_planner` inside a GUC nest level that sets
   `KS_PARTIAL_PLAN_GUCS` (2.3, D1), that is `max_parallel_workers_per_gather = 0`,
   `enable_partitionwise_aggregate = off` and, on 19, `enable_eager_aggregate = off`
   (so the top of the plan is one `Agg` node over the scan or join).
2. The post-processing, which is a few lines in a function `partialize_plan`:
   the top node must be an `Agg` with `aggsplit == AGGSPLIT_SIMPLE` and no
   `qual`; it is changed to `AGGSPLIT_INITIAL_SERIAL`, and every `Aggref` in its
   target list is replaced by a flat copy passed through
   `mark_partial_aggref(agg, AGGSPLIT_INITIAL_SERIAL)` (`planner.c`, exported in
   `optimizer/planner.h`). `nodeAgg.c` asserts `aggref->aggsplit ==
   aggstate->aggsplit` for every `Aggref` it initializes (`nodeAgg.c:3774`), so
   both must be changed together; the executor then skips the final function
   (`DO_AGGSPLIT_SKIPFINAL`) and serializes an `internal` state with the
   aggregate's `aggserialfn` (`nodeAgg.c:3806` to `3836`). The output column
   types become what `mark_partial_aggref` sets: the transition type, or
   `bytea` for `internal`.
3. Any other shape (the top is not an `Agg`, or has a `qual`, or is already
   split) raises an internal error that names the shape. It cannot happen with
   the settings of step 1 and the fragment's restricted grammar; the test
   plan has a test for each plan strategy (plain, sorted, hashed, with and
   without a `Sort` below, a constant group expression) so that a change in a
   later PostgreSQL release is noticed.

The local fragment goes through the same post-processing, so the origin's own
part has the same columns and types as the other groups'.

What was checked in the source for this step: `mark_partial_aggref` asserts
`agg->aggsplit == AGGSPLIT_SIMPLE` and a valid `aggtranstype` and sets
`aggtype` to the transition type, or to `bytea` for `internal` when the split
serializes. The `Aggref`s of the top `Agg` of a finished plan of the restricted
fragment are in that state: `preprocess_aggref` set `aggtranstype`, nothing
before this step changes `aggsplit` from `AGGSPLIT_SIMPLE`, and the
expression mutator of `setrefs.c` copies an `Aggref` with its fields. The
function is exported in `planner.h`, and its body is the same on 18.7 and 19. `nodeAgg.c` takes `aggstate->aggsplit` from the plan
node and asserts the same value in every `Aggref` it initializes, so the node
and its `Aggref`s are changed together. The result descriptor of a cursor or of
an SPI portal comes from the executor's own result type (`PortalStart`, from the
plan's target list), so the changed `aggtype` reaches the client as `bytea`. What
is not settled by reading, **[Needs verification]**: (a) that no node between the
top `Agg` and the client copies the pre-change type (the fragment's restricted
grammar and the settings of step 1 above make the top node the `Agg`, and a
`Sort` or `Result` above it is refused by step 3); (b) that the plan cache of an
SPI cursor does not recompute the result descriptor from the `Query` (the local
fragment). What settles both: a test on a build with assertions that partializes
the plan of each aggregate of the partial-capable row of 7.1, on every plan
strategy (plain, sorted with and without a `Sort`, hashed, a constant group
expression), through a `DECLARE` on a group and through the SPI cursor of the
local part, and compares the final result with the single node.

### 7.4 The path at the origin

In the hook for `UPPERREL_GROUP_AGG`, when the unit is eligible and
`extra->flags` has `GROUPING_CAN_PARTIAL_AGG`:

1. The **partial target** is built the way `make_partial_grouping_target`
   builds it (that function is static in `planner.c`; the extension repeats its
   few lines with the exported helpers `add_column_to_pathtarget`,
   `add_new_columns_to_pathtarget`, `pull_var_clause` and
   `set_pathtarget_cost_width`): the grouping columns of the grouped relation's
   target with their sort-group references, then every `Var` and `Aggref` used
   by the other target entries and by `HAVING`, with each `Aggref` marked by
   `mark_partial_aggref(..., AGGSPLIT_INITIAL_SERIAL)`.
2. A `CustomPath` "Kingres Fragment Scan" with that target returns the partial
   rows. Its fragment is `pg_get_querydef` of the copy of the query with the
   select list replaced by the **unmarked** expressions of the partial target
   (so that the group sees ordinary aggregates), `HAVING`, `ORDER BY`, window,
   `DISTINCT` and limit cleared, and `GROUP BY` kept with the target entries'
   sort-group references.
3. On top of it the extension builds the final aggregation with the planner's
   own function: `create_agg_path(root, grouped_rel, partial_path,
   grouped_rel->reltarget, strategy, AGGSPLIT_FINAL_DESERIAL,
   root->processed_groupClause, <having quals>, &agg_final_costs, numGroups)`,
   for `AGG_HASHED` when `GROUPING_CAN_USE_HASH`, for `AGG_SORTED` over an
   explicit `create_sort_path` when `GROUPING_CAN_USE_SORT`, and `AGG_PLAIN` when
   there is no `GROUP BY`. `agg_final_costs` comes from
   `get_agg_clause_costs(root, AGGSPLIT_FINAL_DESERIAL, ...)` (`optimizer/prep.h`)
   when `extra->partial_costs_set` is false. The number of groups is estimated
   with `estimate_num_groups`.
4. Plan creation then works as for any partial aggregation: `setrefs.c`
   (`convert_combining_aggrefs`) replaces each `Aggref` of the final node by a
   combining `Aggref` over a copy of the partial `Aggref` and finds that copy in
   the custom scan's target list by `equal()`; the scan's `custom_scan_tlist` is
   the partial target, so the match holds. EXPLAIN shows `Finalize ...Aggregate`
   over the custom scan (explain.c prints "Finalize" for `DO_AGGSPLIT_COMBINE`).
   Settled by reading `setrefs.c` (identical on 19): `set_plan_refs` handles the
   `Agg` node first (`convert_combining_aggrefs`, then `set_upper_references`,
   which looks each expression up in the child's **target list as it still is**,
   by `equal()`) and recurses into the child afterwards ("it is essential that
   we recurse into child plans AFTER we set subplan references in this plan's
   tlist and quals"). The child's target list is the partial target that `PlanCustomPath`
   received, so the partial `Aggref`s match. `set_customscan_references` then
   turns the same `Aggref`s into `INDEX_VAR` references into `custom_scan_tlist`,
   which is the arrangement of a `ForeignScan` with `scanrelid = 0` for an upper
   relation. What is left to a test is only that a `CustomScan` does it too
   (the plan of every aggregate of 7.1, `EXPLAIN` and a run: `060_pushdown_agg`).
5. Everything above the grouped relation (window functions, `DISTINCT`,
   `ORDER BY`, `LIMIT`) is planned by PostgreSQL as usual on top of the final
   aggregate: window functions above aggregates, `HAVING` (in the final node's
   `qual`), and sorts on aggregate results need no special case.

### 7.5 P4, the closed aggregate

When the query is closed (6.4) because its `GROUP BY` contains a key variable,
the path at `UPPERREL_GROUP_AGG` returns the **final** aggregate rows
(`Aggref`s are plain, `HAVING` is shipped): every aggregate in the table of 7.1
is allowed, including `DISTINCT`, `ORDER BY` inside the call, ordered-set,
hypothetical-set and the aggregates without a combine function, because all
rows of a group are on one group. The fragment is the raw query with `ORDER BY`,
`LIMIT` and window clauses cleared (the planner puts those above), and the
custom scan's `custom_scan_tlist` is the grouped relation's target. Aggregates
must still be built in or objects of an allowed extension (5.5), shippable (5.1), and order-insensitive in the sense
that every execution on a group gives the result a single node would (an
aggregate whose result depends on the input order, such as `array_agg` without
`ORDER BY`, depends on the order of the group's scan exactly as it does on one
node, which SQL leaves unspecified).

### 7.6 Correctness conditions for aggregates

Each is a statement a test can check; 16.3 maps them to files.

- **A1.** For every aggregate in the "partial-capable" row, over data in which
  rows are spread arbitrarily over groups, the final result equals the result
  of the same query on one node holding all the rows: exactly for integer,
  `numeric`, text, boolean, bit, `interval` (`sum`) and array results, and to
  the precision of the type for `float4`/`float8` results (A3).
- **A2.** Types whose transition state is `internal` (`sum(int8)`, `avg(int8)`,
  `sum(numeric)`, `avg(numeric)`, the `stddev` and `variance` family over
  `numeric`, `string_agg`, `array_agg`, `bit_*`...) cross as `bytea` produced
  by the aggregate's own `aggserialfn` and consumed by its `aggdeserialfn` at the
  origin. A node never interprets a state; it only transports it. `array_agg`
  over a type without `send`/`receive` is excluded by the planner flag
  (`agg_args_support_sendreceive`).
- **A3.** Floating-point `sum`, `avg`, `stddev`, `variance`, `corr`, `regr_*`
  add values in a different order from a single node (partial sums are added in
  the order the groups answer, which can change between runs), so the last
  bits can differ, and can differ between two runs of one query. PostgreSQL's own
  parallel aggregation has the same property. The result is correct within
  floating-point error; `kingres_shard.enable_partial_aggregation = off`
  restores the 2a plan, whose order is also not fixed but whose rows arrive in a
  repeatable order for a given plan. The tests compare float results after
  rounding to 9 significant digits and say so.
- **A4.** `count(DISTINCT x)`, `sum(DISTINCT x)` and every other `DISTINCT`
  aggregate are never split: with `GROUP BY <key>` they are pushed whole (P4),
  otherwise the rows of `x` come to the origin through P2 or P1.
- **A5.** `HAVING` is applied at the origin on the final aggregate
  (partial) or at the groups (closed). A `HAVING` that mentions an aggregate
  that is not in the select list is part of the partial target.
- **A6.** A window function above an aggregate is computed at the origin on the
  final rows (partial) or on the rows the groups return; if the window is in
  the same query level and partitioned by the key, the query may be closed and
  is computed at the groups (P5).
- **A7.** A scalar aggregate (no `GROUP BY`) over an empty set returns one row
  with the aggregate's value for no rows (`count` 0, `sum` `NULL`): each group
  returns one partial row holding the initial state (or none, for a pruned
  group), and the final aggregate combines whatever it receives, as it does in a
  parallel plan. A grouped aggregate over an empty set returns no row.
- **A8.** `GROUP BY` expressions that are not bare columns are computed at the
  groups and are part of the partial row; they must be shippable (5.1).
- **A9.** A query with grouping sets, a user-defined aggregate or an ordered-set
  aggregate that is not closed is not split and takes the 2a plan.
- **A10.** The aggregate's collation (`inputcollid`) is the default or `C`
  (5.1); `min(text)` combines under the same collation at the origin and at
  the groups.

## 8. Sort and limit

### 8.1 What is pushed

P5 (a whole closed query) is created at `UPPERREL_FINAL`. Its fragment is the
whole query: shippable `WHERE`, group, `HAVING`, window, `DISTINCT`, select list
and `ORDER BY`, and, when a limit is pushed, `LIMIT <n>` with `n =
count + offset` (and no `OFFSET`). The fragment's select list includes the
sort expressions as extra (junk) columns when they are not in the output, so
that the origin can merge on them.

The custom scan merges the per-group streams with a binary heap keyed by
`SortSupport` built with `PrepareSortSupportFromOrderingOp(sortop, ssup)` from
the query's `SortGroupClause` list: `sortop`, `nulls_first`, and the collation
of the sort expression (`exprCollation`). These are the same inputs that
`nodeSort` and `nodeMergeAppend` take from a `Sort` or `MergeAppend` plan node,
so the origin orders exactly as PostgreSQL would. The path's `pathkeys` are the
planner's `root->sort_pathkeys`, so the planner does not add a `Sort`; when
`limit_needed`, `create_limit_path` adds the `Limit` node, which applies `OFFSET`
and `LIMIT` to the merged stream.

The merge compares with `PrepareSortSupportFromOrderingOp` and
`ApplySortComparator` only. PostgreSQL 19 removes the inline comparators for
unsigned, signed and 32-bit datums from `sortsupport.h`; the merge does not use
them (2.3, D8). The merge keeps one row per group in its heap and reads the next
batch of a group when that group's head is consumed; the batches are owned as
in 4.4.

When the query has no `ORDER BY`, P5 returns the streams in arrival order and a
limit is pushed as `LIMIT count + offset` (any rows will do).

For queries that are not closed there is nothing to push at `UPPERREL_ORDERED` or
`UPPERREL_FINAL`: the rows that reach the sort are the output of the final
aggregate or of the join, and are sorted at the origin by the planner. P2 and P3
carry no `ORDER BY`.

### 8.2 Correctness conditions for ordered merge and limit

- **O1.** Collation: every sort expression has the default collation or `C`
  (5.1). A column with another collation, or a non-deterministic collation,
  makes P5 not created (the 2a plan sorts at the origin).
- **O2.** `ASC`/`DESC`/`USING <op>` and `NULLS FIRST`/`LAST` are taken from the
  `SortGroupClause` and sent in the fragment's `ORDER BY` by `pg_get_querydef`;
  the merge uses the same `sortop` and `nulls_first`. A test orders by every
  combination for `int`, `text`, `float` (with `NaN` and `-0`), `date`,
  `timestamptz`, `numeric`, `bool` and a `NULL`-heavy column.
- **O3.** Ties: rows that compare equal come out in an unspecified order, as
  on a single node. The merge breaks ties by the position of the group in a
  fixed order (the group id), so that a repeated query with unchanged data
  returns the same order unless a group answers more slowly (a merge waits for
  the next row of the group it needs, so arrival order does not matter).
- **O4.** Limit pushdown is valid only if every group returns its first
  `count + offset` rows of the *final* ordering of the query. It is therefore
  pushed only when the query is closed (so the group's rows are final rows) and
  only when no step after the group changes the row count or the order: no
  `DISTINCT` that the groups did not complete (6.4 closes it), no set-returning
  function in the select list, `LIMIT` without `WITH TIES`. For `FETCH FIRST ...
  WITH TIES` the merge is pushed and the limit is not (rows tied with the last
  may be in any group).
- **O5.** `OFFSET` is not pushed. A group returns `count + offset` rows; the
  `Limit` node of the origin discards the first `offset` merged rows.
  `LIMIT NULL` and `LIMIT ALL` push no limit. `count + offset` that overflows
  `int8` pushes no limit. A negative `count` or `offset` is the origin's error
  (its `Limit` node evaluates the expressions); the pushed value is computed at
  the start of the scan from the same expressions (5.2).
- **O6.** A cursor that is closed early closes its group cursors; a `LIMIT`
  query stops reading after the origin has what it needs, and the batch size
  bounds what was read ahead.
- **O7.** `LIMIT` without `ORDER BY` returns some `count` rows; which ones may
  differ from a single node, as it may between two runs on one node.

## 9. Colocated joins

### 9.1 What is pushed

P2 is created in `set_join_pathlist_hook` for the join relation that covers all
relations of the level. The fragment is the raw query's `FROM`/`WHERE` with a
select list of the columns the planner needs above the join (its
`joinrel->reltarget`); the planner then adds the rest of the plan above the
custom scan as for any scan. When the query is closed, P4 or P5 replaces P2 and
does more.

Permitted:

| Join | Condition |
|---|---|
| inner, any mix of distributed and reference tables | every distributed table in one key class (6.3). The key-equality conjuncts may be in `WHERE` or `ON`. |
| `D1 LEFT JOIN D2`, `D2 RIGHT JOIN D1`, `D1 FULL JOIN D2` (both distributed) | the key equality is in the `ON` clause of that join; every `ON` conjunct is shippable. |
| `D LEFT JOIN R` and `R RIGHT JOIN D` (`D` distributed, `R` reference) | every `ON` conjunct is shippable. The preserved side is distributed, so each of its rows is on one group, and `R` is whole there. |
| inner join of two reference tables inside the query | allowed; the pair is evaluated on every group, so it only counts when joined to a distributed table by an inner join or by `D LEFT JOIN`. |

Not pushed, with the reason:

| Join | Reason |
|---|---|
| `R LEFT JOIN D`, `D RIGHT JOIN R`, `R FULL JOIN D`, `R FULL JOIN R2` with `D` in the query | the preserved side is replicated: every group would emit the unmatched rows of `R`, so they would appear once per group, and a row of `R` that matches in one group is unmatched in the others. |
| semi join (`IN`, `EXISTS`) and anti join (`NOT EXISTS`, `NOT IN`) | they arrive as sub-links, which make the query not a unit (6.1). The conditions under which they would be correct are stated here for a later increment: the correlation contains a key equality (all matching rows are on one group); with a reference table as the outer side a semi join would repeat rows once per group, an anti join would emit the row once per group where it has no match. `postgres_fdw` ships semi joins and not anti joins (`foreign_join_ok`); neither is shipped here. They use the 2a scans. |
| lateral references and `LATERAL` | a parameterized path is never created, and a lateral reference means a sub-select or function in `FROM`, which is not a unit (6.1). |
| distributed tables of two colocation groups, or two key classes in one query | rows that join live on different groups; the join happens at the origin over the 2a scans. |
| a join on a non-key column, a key equality under another collation or operator, a key equated through an expression (`a.k = b.k + 0`, a cast that is a function call) | no proof that matching rows share a group. |
| a local table, a view, a subquery, a function, `VALUES` | not a unit. |

Each group's planner plans the join of the fragment itself, with its own
statistics and its own indexes; the origin does not choose a join order for the
pushed part.

The hook of P2 returns at once for a relation that is not the join of the whole
level, for a statement that is not a unit, and, on PostgreSQL 19, for a grouped
relation (`KS_IS_GROUPED_REL`, 2.3 D1): eager aggregation builds a copy of the
join relation that holds partial aggregates, with the same `relids`, and calls
`add_paths_to_joinrel` for it. The plan of a pushed join is correct under
eager aggregation because P2 is a path of the plain relation only; the
planner's own partial aggregate may still sit above a P1 scan.

### 9.2 Correctness conditions for joins

- **J1.** Every output row of a pushed join is formed from rows of one group, so
  the union over groups of the fragment results is the join result, with no row
  repeated and none missing (6.3).
- **J2.** Outer joins: the preserved side of every pushed outer join is
  distributed; a null-extended row is produced by the group that holds its
  preserved row.
- **J3.** A reference table is read from the group's own copy. All copies are
  equal (2a, section 3), so the result does not depend on the group.
- **J4.** A query with a non-pushable join is planned by 2a with the
  distributed scans of each table, so it has the 2a semantics.
- **J5.** The unmatched-row semantics of `FULL JOIN` between two distributed
  tables hold because the key equality in `ON` puts both sides' matching rows on
  one group.

## 10. Consistency, bucket moves, and plan caching

### 10.1 What snapshot each group sees

No global snapshot exists (phase 8), and nothing here creates one. What
changes:

- A group runs one fragment for the whole pushed part, in one statement, so all
  tables of a pushed join, and the rows of an aggregate, come from one snapshot
  of that group. Review finding S2-09 (one statement reading two tables of the
  *same node* in two different states, because each `DECLARE` of the 2a scan has
  its own snapshot in `READ COMMITTED`) does not happen for a pushed statement.
  Pushdown therefore makes the anomaly smaller, not larger.
- Between groups the skew of 2a remains: group 1 may be read before a distributed
  transaction commits and group 2 after it.
- In `REPEATABLE READ` the remote transaction takes its snapshot at its first
  statement and every later fragment reuses it; `SERIALIZABLE` is serializable
  per node.
- Read-your-writes: a fragment runs in the remote transaction that holds the
  transaction's earlier writes on that node, and the local fragment through SPI
  in the local transaction, so a transaction sees its own writes.

### 10.2 The placement check, and a bucket that is moving or has just moved

Every fragment, the 2a scan's fragment too, is checked by the procedure of 4.3
(the plan number, the probe before, the group's own `place=` check, the probe
after). A failure raises `KS001` and the statement is repeated by the client;
the origin does not reload its catalog inside the failing statement.

Argument that a statement whose fragments all passed reads every bucket
exactly once. A move (phase 4) commits in one distributed two-phase transaction
that detaches the source partitions, attaches the target tables, and changes the
placement of the colocation group on every node that takes part. A `DETACH
PARTITION` that is not `CONCURRENTLY` (and `CONCURRENTLY` cannot run inside a
transaction block, so the switch cannot use it) takes `ACCESS EXCLUSIVE` on the
parent (`AlterTableGetLockLevel` in `tablecmds.c`: `cmd_lockmode =
AccessExclusiveLock` unless `concurrent`).

1. The placement number and the partition identity of a node change only in the
   transaction of a switch on that node.
2. The probes read them from the catalog tables with the latest committed
   snapshot, and the first probe also makes the node's relcache entry of the
   parent as new as its answer. Equal numbers before and after the `DECLARE`
   therefore mean that no switch committed on the node between the two reads,
   so the cursor, which is planned in between, was planned with the partitions of
   the placement that the plan assumed, for every table of the fragment (each is
   probed). A bucket that left and came back in between changes the identity
   (review S4-01); the placement number alone would not see it.
3. After that, the cursor holds `ACCESS SHARE` on the parent and on the
   partitions it reads until the end of the remote transaction. A `DETACH`
   (the source) waits for that lock, so the source cannot lose the bucket, and
   cannot commit its part of the switch, while the statement is reading it. An
   `ATTACH` on the target does not conflict with the lock, but it is not in the
   plan of any cursor of this statement: the target's cursor was planned without
   the partition, and the bucket is read from the source, which is what the
   placement the origin used says.
4. A group that is not asked because of pruning owns no bucket the query can
   touch under the placement of the plan. If a move delivers a bucket to it
   during the statement, the statement reads that bucket from the source (asked,
   under the same placement, and unchanged between its probes), or fails at the
   source's probe. So pruned groups need no probe.
5. The origin's own part is checked like a remote node: the placement is
   recomputed from the catalog tables, and the identities of its own partitions
   of every table are read before and after the SPI cursor is opened (4.4).
6. A statement that finds a bucket moving (`state = 'moving'`, no switch
   committed) is not affected: the placement is the same until the switch.

The 2a scan has the same guarantee since 0.4, and it is what phase 4's tests
`026` to `032` ("readers during a move never see a row twice") exercise; they
must keep passing when the procedure moves into `placement_check.c`
(step 2). The orphan tables the old group keeps after a move are standalone
tables and are never read (a fragment names the parent, which has no such
partition); the standalone copies on the target before the switch are not
attached and are never read.

Costs, stated: (a) a transaction that has read a bucket through a fragment
holds the lock until it ends, so a long-open read transaction delays the
switch of that bucket (the switch times out after `move_switch_timeout` and goes
back to catching up, so the move retries; it can be starved by a transaction
that stays open); (b) a statement that overlaps the commit of a switch can fail
with `KS001` and must be retried by the client.

Bulk writes and moves: a batch `INSERT` to a bucket whose parent is locked by
the switch waits for the lock. The group plans the insert statement holding
`ROW EXCLUSIVE` on the parent, which a `DETACH` waits for, so once the insert is
planned the source partition stays attached until the transaction ends; an
`ATTACH` that commits in the meantime can make a row for the arriving bucket fail
with `no partition of relation ... found for row` (SQLSTATE 23514), which is an
error and never a row in the wrong bucket. The insert fragment's `place=` check
(11.5) catches a switch that committed before the statement was planned, with
`KS001`.

### 10.3 Standbys and a change of primary

**What 0.5 does** (ha-phase5.md part C, and the working tree). A standby owns no
buckets and is skipped by routing, catalog synchronization and reference-table
writes. `kingres.promote_node(node_id)` makes a promoted standby the primary of
its group and the former primary a standby in one atomic catalog change on every
node that can be reached, and raises the map version. Four properties matter
here: the placement number of a colocation group includes the node that is the
primary of each group (`catalog.c`), so a plan, a cached plan and a scan made
against the old primary have another number from then on; idle connections to a
node that is no longer a primary are dropped when the catalog is loaded
(`kingres_shard_conn_drop_stale`), and a connection that is in a transaction is
kept until the transaction ends; a node that the catalog does not call the
primary of its group has `local_group_id = -2`, owns no bucket, runs nothing as
local, and routes every statement to the primary; and the prepared transactions
of a failed-over group are resolved by the origin's decision log
(ha-phase5.md, part C).

**The rule.** A statement reads and writes each group through **one** node, the
primary that its plan was made with, or it fails with `KS001`. It never combines
rows or writes from two nodes of one group. Fragments are sent to the node that
`kingres_shard_group_primary` names after the plan number has been compared with
the cache (4.3 step 1), and nothing in the statement opens a connection to
another node of that group.

What happens when the primary of a group `g` changes at moment `M`, for each new
path (`old` is the former primary, `new` the promoted one):

| Path | `M` before the statement starts | `M` between the probes, or while the cursors are being declared | `M` after the second probe, while rows are fetched |
|---|---|---|---|
| Fragment scan (P2 to P5), and the 2a scan | If the origin's cache is already the new catalog and the plan is old: plan number differs, `KS001`. If the plan was made after the change: it goes to `new`. If the cache is old and `old` is alive and updated: the probe returns another number, and 4.2 rule 4 refuses the fragment, `KS001` | The second probe returns another number (the primary is in it), or the node refuses, `KS001`; nothing has been returned | The cursors on `old` read to the end from the snapshot they have: every row of `g` comes from `old` at one state, which a switchover makes complete (it waits for the standby, ha-phase5.md part B). If `old` dies, the next `FETCH` fails with a connection error, which `fragment_error` (4.6) reports as `KS001` because `g` has a standby. No row of `new` is mixed in: this statement never connects to it |
| Rescan of a fragment scan | Step 1 of 4.3 runs again: `KS001` if the cache changed | as the first row | as the first row |
| Row router (`INSERT`, `COPY`) | The batch statement carries `place=` and the group refuses if it is not the primary or its number differs: `KS001`, the statement aborts, every earlier batch is rolled back with the transaction | as before | A batch in flight when `old` fails is a connection error, reported as `KS001`; the transaction aborts and rolls back on every group. A batch that `old` accepted and a later failure: aborted with the transaction. A prepared transaction on `old` is resolved by recovery (part C) |
| Colocated insert (P6), fan-out `UPDATE`/`DELETE` | The plan number differs: `KS001` | The statement compares the placement of every target after it ran (`fanout.c`); the number includes the primary, so `KS001` with every change rolled back | The commit is the two-phase commit of 2a; a participant that is lost is resolved by recovery |
| A transaction that already used `old` and meets `new` in a later statement | `KS001` from the check "another node of the same group has an open remote transaction in this transaction" (4.4). The statement is not run | | |

A failure to reach a node that is not marked for failover (no standby is
registered) is the plain connection error (4.6).

**What is not covered.** A cut-off old primary that has not been fenced and has
not received the catalog change holds the same catalog as a stale origin, and the
numbers agree; the lease fencing of ha-phase5.md part B stops it, not this
phase. A hot standby that is the origin still reads its own partitions as its
group's (2a, README "Functions"): it reads from a standby, which is not a
primary, and its `local_group_id` is that of its primary; this is a 2a
limitation and is unchanged.

**Tests** (`064_pushdown_failover`, with the helpers of `t/048` to `t/054`:
`make_standby`, `register_standby`, `promote_standby`): for each path above, a
switchover and a hard failover with the fault points `frag_after_probe_before`,
`frag_after_declare`, `frag_after_probe_after`, `frag_first_fetch`,
`bulk_between_groups` and `bulk_before_last_flush`; a prepared statement planned
before the change and executed after it; a transaction that wrote through `old`
and then reads through `new`; and a kill of `old` in the middle of a fetch. After
each: the statement either returned the single-node answer or raised `KS001`, no
partial answer, the repeated statement succeeds, `pg_prepared_xacts` is empty
once recovery has run, and `check_cluster()` reports nothing but the old
primary.

### 10.4 Prepared statements and cached plans

- Fragment plans embed no parameter *value* at plan time except through
  pruning on constants; a key restriction by a parameter is evaluated when the
  scan starts (4.5). They therefore work as generic plans, unlike a routed
  statement (the 2a limitation stays for routed statements only).
- A cached plan of a pushdown path depends on the placement. The plan stores
  the placement number, and **the decision is made when the plan runs**, not
  when it was made: step 1 of 4.3 compares the origin's cache with the plan on
  every start and every rescan, so a plan that was valid when it was made and
  is not now fails with `KS001` even if an invalidation was missed. The plan's
  `relationOids` also contain every distributed table of the statement
  (`add_rte_to_flat_rtable` adds the OID of every `RTE_RELATION` of the query to
  `glob->relationOids` "even though the RTE might be unreferenced in the plan
  tree", so a custom scan with `scanrelid = 0` that replaces a join keeps them;
  the code is the same on 19), and every change of the catalog invalidates the
  relcache entry of every distributed table, which invalidates the cached plan.
  Both defences are needed: the first is the one that holds when a plan was
  cached before the table became a reference table, was undistributed or
  re-distributed (review S3-02 and phase 9), and the router and the fragment
  scan also check at start that the table is still distributed, in the same
  colocation group, and not a reference table (the message of `ds_start`:
  "no longer a distributed table").
- A plan that was cached for a statement that is a unit at one moment and not at
  another is not a problem: the unit decision is made at plan time and the
  plan is replaced on invalidation.

### 10.5 What the earlier reviews found, and how each mechanism avoids it

The reviews of 0.2, 0.2 (fixes), 0.3 and 0.4 are in `kingres/docs/reviews`. For
each finding whose class a mechanism of this note could reproduce, the table
says where it could recur and what the design does. The right-hand column is the
test that would catch a regression.

| Finding (class) | Where 2b is exposed | What the design does | Test |
|---|---|---|---|
| S4-01: an answer that is incomplete, without an error, when the placement changes during the start of a scan (a bucket moves away and back; the placement number is blind to it) | every fragment; the local part; the batch of an explicit destination | the probes of 4.3 carry the partition identity and read the catalog tables, not a cache; the first draft's "version `V` checked after planning" is replaced because it relied on invalidations that are not ordered with the commit; the local part is checked like a remote node; phase 10's epoch makes it simpler | `055`, `063`: the sequence of the review (move away, scan resumes after the move back) for a fragment on the origin's pair and on another pair of groups, with the fault points `frag_after_probe_before`, `frag_after_declare`, `frag_after_probe_after` |
| The same class in the local part: the 2a fan-out local part is planned by `pass_through` before `ensure_fresh_partitions` (the "argued, not forced" case of the 0.3 review) | the SPI part of every fragment | the nonce branch comes after the analysis and the freshness step (4.4), and step 2 moves the 2a fan-out local part to the same order | `055` with the delay of the 0.3 review's unconfirmed case |
| S2-08: a function marked `PARALLEL SAFE` that reads a distributed table answers from a worker's snapshot | the pushdown paths, the router, `COPY` | `parallel_safe = false` on every path and `consider_parallel = false` on base, join and upper relations; the hook raises in a worker instead of planning without pushdown; the executor node raises in a worker (4.4) | `067_pushdown_security`: a `PARALLEL SAFE` function that runs a pushdown query, called under `parallel_setup_cost = 0` |
| S3-01, S3-02: a write goes around a check through a function, a trigger, a cached plan or a generic plan | the router (a plan cached before the table changed kind), the local part (a function that runs the text of the local fragment), the source `SELECT` of an insert | decisions are made at execution (10.4); the nonce replaces the comparison of statement text (4.4); a trigger cannot exist on a distributed table and a trigger on a local target table runs its statements through the planner hook as statements of their own; the source `SELECT` of the router is an ordinary top-level statement planned by the full hook; reference-table triggers stay refused (S5-01) | `056`, `067`: prepare six times and force a generic plan, then `create_reference_table`/move; a function that runs the text of the local fragment inside the statement is still routed |
| S5-01: statements nested in a write that the extension runs as local are not checked | the SPI part, the router's SPI source | the nonce is valid only inside the SPI call of the active scan and never for another statement; the router's source runs with routing on | `067` |
| S2-03, S3-14 and the 2a note on `money`: text that means something else on another node (session settings, collations, `search_path`) | the deparse of whole queries, constants, the partial states, `RETURNING`, the batch text | the transmission modes around every deparse and every output of a value; `search_path = pg_catalog` on the remote session and every name qualified; only default and `C` collations; no `CoerceViaIO`; `money` is not shipped (5.1); extension types have a fixed text form; the 4 session profiles of the harness run every equivalence query, and the hostile-name and hostile-value test covers every statement shape | `017`, `066_hostile_values`, `055` (profiles) |
| S2-17 and the 19 port: results not freed, or freed twice | batches, probes, the router's in-flight results, `RETURNING` rows | one owner structure and one release function, results received in the scan's own context, no `PQclear` elsewhere (4.4, 2.3 D6); a router result is converted to a tuplestore row before the next call | a cancel at every wait of every path on a build with assertions and `MEMORY_CONTEXT_CHECKING`, on 18 and 19 |
| S4-03: a write into a bucket partition goes around the marker proof | the batch statements | the batch names the parent table, never a partition; the group's planner hook applies the existing refusal of a write into a bucket partition to a batch as to any statement (the marker relaxes only the proof that a bucket is local, 11.5); a superuser with routing off is the existing repair path | `056` |
| S4-04, S4-09 (lock and horizon cost of readers): a transaction that read a bucket holds the switch back | every fragment | stated as cost (a) of 10.2; fragments hold nothing beyond the 2a scan | `063` |
| S4-05: a node with the new library and old SQL objects, or two versions in one cluster | every new fragment kind | the probe carries the node's extension version; a different one is refused with a clear error; the rolling-upgrade instructions switch the paths off (14) | `055`, `upgrade` |
| S2-15 (views and owners), S2-04 (forged marker) | units, markers | `checkAsUser` refuses a unit; a marker is a comment and the group re-checks the whole statement | `067` |
| S3-06 (data and secrets in logs), in a new form | the router sends row values as parameters; a group with `log_min_duration_statement` or `log_statement = all` logs the parameters of a failing or slow batch (`log_parameter_max_length` defaults to -1: in full) | a batch is a statement like any other that logs its values; the README says that the batch size multiplies what is logged, and that `log_parameter_max_length` on each node bounds it; no secret, token or `node_conninfo` value is ever in a batch | `067`: a failing batch with the log setting at its default; the log has the values and no token |
| S2-05, S3-14 (order-dependent and libm-dependent results) | aggregates and `RETURNING` of a pushed read | A3 and 7.5: float results are not claimed to be repeatable to the last bit; `libm_dependent` functions stay unshipped; a write never depends on them | `060_pushdown_agg` |

## 11. Bulk writes: the row router

### 11.1 Statements and the plan

A multi-group `INSERT` into a distributed table, in any of these forms, is
planned by `planner_hook_body` as one plan whose top node is the **"Kingres Row
Router"** custom scan (like the "Kingres Remote Statement" the plan has no
`ModifyTable`):

- `INSERT ... VALUES (...), (...)` with more than one row, or one row whose key
  is not a constant (a default, a function call);
- `INSERT ... SELECT ...` (the source is any `SELECT`, which is planned by the
  planner hook as a statement of its own, so it uses the 2a scan and every
  pushdown path of this note);
- `COPY t FROM STDIN | 'file' | PROGRAM ...` (11.6).

A statement whose single row has a constant key is still routed to one group by
the existing code, and a single-group cluster is unchanged.

The router reads **source rows** one at a time and sends them to the group that
owns their bucket:

- For `INSERT`, the source is the rewritten statement turned into a `SELECT`
  (11.2) and deparsed under the transmission modes; the router runs it as an SPI
  cursor (`SPI_cursor_open`, read-only, fetching `kingres_shard.insert_batch_rows`
  rows at a time). It is planned by the planner hook like any statement, so a
  source over distributed tables is read through the scan or a pushdown path.
- For `COPY`, the source is `NextCopyFrom` (11.6).

The plan's `PlannedStmt` carries a one-entry range table (the target table)
and its `RTEPermissionInfo` with `ACL_INSERT` and the inserted columns, so the
executor checks the privilege at the origin (`ExecCheckPermissions`) before the
router starts, as it does for any insert; the groups check it again as the same
role.

The router plans from the placement of the plan (4.3 step 1) and finds the
bucket of every row with the one lookup of 4.7. When this node is not the
primary of a group (`local_group_id < 0`, 0.5), the router has no local group:
every batch is remote. A statement that the router would run and that carries
`ON CONFLICT DO UPDATE` or, on 19, `DO SELECT`, is refused (11.3); a colocated
statement that is shipped whole is not routed at all (11.7).

### 11.2 Which values are computed where

The rewriter has already put the default expression of every column the
statement does not give into the target list of the `INSERT`: `nextval()` for
`serial` and identity columns, constants, function calls. The source `SELECT`
the router runs has exactly those expressions as its select list, one output
column per column of the target that is not a stored generated column. The
consequence is the rule:

- **A default or identity value is computed once per row, at the origin, by
  the origin's own functions and sequences**, in every code path of this note,
  and sent as a value. A volatile default is therefore never repeated or
  diverging between groups. (The sequences are the origin node's, as for a
  routed single-row insert; the sequence behaviour of 2a, "values are
  node-local", is unchanged: a row inserted from another origin may draw from
  that node's sequence.)
- A column whose default is an immutable built-in expression (a constant) and
  that the statement did not name is **left out of the column list** of the
  remote statement, so the group applies the default itself and column-level
  `INSERT` privileges behave as on one node (a role with a privilege on some
  columns only can insert, as long as the other columns' defaults are constants).
  Columns whose default must be computed at the origin are sent, so the group
  checks the privilege on them too; a role without it gets the group's
  permission error. This is a documented difference.
- **Generated columns**, stored or virtual, are never sent; each group computes them when it stores the row or reads it (their
  expressions must be immutable, 2a notes).
- **Identity columns** declared `GENERATED ALWAYS` are sent with `OVERRIDING
  SYSTEM VALUE`, which needs no privilege beyond `INSERT`.
- A **trigger**: distributed tables have no user triggers (`create_distributed_table`
  refuses them, `CREATE TRIGGER` is refused), so there is no `BEFORE` or `AFTER`
  row trigger to run in the wrong place. Foreign-key triggers fire on the group
  that inserts, which holds the referenced row (a reference table is on every
  group; a colocated table's row is on the same group, the only FKs 2a allows).
- **`RETURNING`** is evaluated on the group that inserted the row. See 11.4.

### 11.3 The wire statement (one format)

All rows, for the local group and for the others, go as a batch of the same
statement; `COPY` is not used on the wire. Every column crosses as the text
form of the value under the transmission modes, in a `text[]` parameter per
column, and is cast back to its type on the group:

```
/* kingres:frag v=2 kind=insert place=<n> [ident=<n>] */
INSERT INTO "<schema>"."<table>" ("c1", "c2", ...) [OVERRIDING SYSTEM VALUE]
SELECT u.c1::<type1>, u.c2::<type2>, ...
FROM unnest($1::text[], $2::text[], ...) AS u(c1, c2, ...)
[ON CONFLICT DO NOTHING | ON CONFLICT (<inference>) [WHERE <pred>] DO NOTHING
 | ON CONFLICT ON CONSTRAINT "<name>" DO NOTHING]
[RETURNING <deparsed list>]
```

Why a `text[]` per column and not an array of the column's type: an array of
arrays of unequal length is not representable (a column of an array type), a
user-defined type needs an array type with the same name on every group, and
the text form is what every other statement of the extension already relies on.
`unnest` with several arguments in `FROM` is the SQL multi-argument form (it is
not one function call, so `FUNC_MAX_ARGS` does not limit the column count; the
protocol limit of 65535 parameters is far away). The `::<type>` is a cast from
`text` through the type's input function with the type name schema-qualified
(`format_type_be_qualified`); a typmod is applied by the `INSERT`'s assignment
cast, as on one node. A `NULL` element is a `NULL`. `array_out` quotes every
element that needs it, so a value with a comma, a brace, a quote, a backslash or
the text `NULL` crosses intact (tested, 16.4).

The text is built once per statement from catalog facts and the `Query`
(identifiers by `quote_identifier`, the target by `quote_qualified_identifier`
of the OIDs, `RETURNING` and an `ON CONFLICT` inference clause deparsed with
`deparse_expression` over a one-relation context, the constraint name from
`get_constraint_name`), then sent with the batch as parameters of an
extended-protocol call. Nothing a row contains is ever part of the text.

`ON CONFLICT DO UPDATE` is **refused** ("not supported yet; a colocated
INSERT ... SELECT is shipped whole and allows it") when the statement goes
through the router: applied per batch it would let a second row for the same key
update the row a previous batch inserted, where a single statement raises "ON
CONFLICT DO UPDATE command cannot affect row a second time". The reason is
batching, and it does not apply to a statement that a group runs whole; section
11.7 allows `DO UPDATE` there, with its conditions. `DO NOTHING` is allowed in
the router, since skipping a later duplicate is the same in either case. On
PostgreSQL 19 `ON CONFLICT DO SELECT` is refused by the router and by P6 (2.3
D4): its row-locking and `RETURNING` behaviour has not been analysed for rows
that arrive in several statements. `RETURNING` expressions and an inference
clause must be shippable (5.1); otherwise the statement is refused.

### 11.4 Buffering, flushing, atomicity, memory

- The router keeps one buffer per destination group: the rows' text values
  per column and a byte counter. A buffer is flushed when it holds
  `kingres_shard.insert_batch_rows` rows (default 1000) or
  `kingres_shard.insert_batch_bytes` bytes of text (default 1MB), and when the
  source is exhausted.
- At most one batch is in flight per remote group (`PQsendQueryParams`); before
  the next batch is sent to that group, the previous result is read. The
  local group's batch runs through `SPI_execute_with_args` synchronously. So the
  memory is bounded by `(2 * groups) * batch_bytes` (a buffer being filled and a
  batch in flight per group), the source cursor's batch, and the `RETURNING`
  rows not yet handed out (below). It does not depend on the number of
  rows.
- `RETURNING` rows arrive with each batch result. The router puts them in a
  tuplestore bounded by `work_mem` (it spills to disk) and returns them to the
  executor as the source loop goes on; the order of `RETURNING` rows is by
  group then by arrival within the group, which is not the input order. SQL
  does not promise the input order of `RETURNING` rows; a client that depends
  on it must use single-row inserts. The command tag count is the sum over
  groups of the rows inserted (`DO NOTHING` skips are not counted, as on one
  node).
- Every remote connection is marked a writer
  (`kingres_shard_note_remote_write`) before its first batch, so the commit
  protocol is the 2a one: two or more writers (the local node counts when its
  transaction has an id) commit with two-phase commit, a single writer commits
  directly. A subtransaction is mirrored by savepoints (`conn_subxact_callback`).
  An `ERROR` anywhere (a unique violation on a group, a constraint, a lost
  connection, a cancel, a `no partition` error, an interrupted source) aborts the
  transaction, which rolls back every group's work; rows of earlier batches
  that a group already applied are part of that group's transaction and go with
  it. There is no partial result.
- Unique-violation and constraint errors come from the group, with its
  `DETAIL` (`Key (id, k)=(5, 7) already exists`) and the context
  `remote node host:port`; the router adds the context line "bulk insert of
  <table>, batch <n> to group <g>". The position of the row inside the batch is
  not known, and the message does not claim one.
- `CHECK_FOR_INTERRUPTS` runs between source rows and the waits are
  interruptible; a failed `PQsendQuery` or a lost connection marks the
  connection broken as in 2a.
- Fault points (existing `kingres_shard.fault_injection` facility, named
  `bulk_<what>`): `bulk_before_first_batch`, `bulk_after_first_batch`,
  `bulk_between_groups`, `bulk_before_last_flush`, `bulk_before_commit`. The
  tests in section 16 use them with `:error`, `:kill` and `:wait`.

### 11.5 What a group does with an insert fragment

The forwarded session accepts `kind=insert` only for an `INSERT` into one
distributed table whose only other relation is the `unnest` function scan, with
no other table in the statement, `ON CONFLICT DO NOTHING` or nothing, and a
`RETURNING` list that passes the shippability walker. The node must be the
primary of its group (4.2 rule 4). After planning, the placement number of the
destination's colocation group computed from the catalog tables equals `place`
(a destination in the catalog), or the identity of the destination table's
partitions on this node equals `ident` (an explicit destination that is not in
the catalog, 11.9); otherwise `KS001`. The check is a fast failure for a stale
origin or a changed primary, and it is not what keeps a row out of a wrong
bucket.

The group does **not** prove that the rows belong to its buckets: PostgreSQL's
tuple routing does that when each row is inserted, and a row whose bucket is not
attached here fails with `no partition of relation ... found for row`. A row can
therefore never be stored in a wrong bucket whatever the origin believes. The
statement holds `ROW EXCLUSIVE` on the parent from planning to the end of the
transaction, which a `DETACH` of a partition waits for (10.2). The only effect of
the marker is that the 2a "provably local" rule, which would reject an insert
whose key it cannot see, is waived for this one statement shape.

### 11.6 `COPY FROM`

The `ProcessUtility` hook (`utility.c`) today refuses `COPY ... FROM` on a
distributed table on a cluster with more than one group (`T_CopyStmt` case).
It now handles it for a distributed table of a multi-group cluster; `COPY TO`
and `COPY (query) TO` are untouched, and a reference table is unchanged (the
2a refusal stays).

The handler does not call `DoCopy`, which would insert into the local
partitions. It uses `BeginCopyFrom`, `NextCopyFrom` and `EndCopyFrom`
(`commands/copy.h`; `file_fdw` uses the same functions) so that PostgreSQL parses
every format and option itself and evaluates the default expressions of omitted
columns (11.2). Because it bypasses `DoCopy`, it repeats `DoCopy`'s checks
(copy.c), each with a test:

- `pg_read_server_files` for a file source, `pg_execute_server_program` for
  `PROGRAM`;
- `ExecCheckPermissions` with an `RTEPermissionInfo` of `ACL_INSERT` and the
  columns of the statement;
- `PreventCommandIfReadOnly("COPY FROM")` and the read-only-transaction rule;
- row-level security: a distributed table has none, so the case does not arise,
  and the handler raises an error if `relrowsecurity` is set.

Options: the format options (`FORMAT`, `DELIMITER`, `NULL`, `HEADER`, `QUOTE`,
`ESCAPE`, `ENCODING`, `FORCE_NOT_NULL`, `FORCE_NULL`, `DEFAULT`) are passed to
`BeginCopyFrom`. `FREEZE`, `WHERE`, `ON_ERROR` and `REJECT_LIMIT` are refused
("not supported yet"): `WHERE` and the error handling are done by `CopyFrom`, not
by `NextCopyFrom`, and are not repeated here. Options are refused by name, so a value
that exists only on 19 (`ON_ERROR set_null`, `FORMAT json`, which the core refuses for
`COPY FROM`) needs no code; `HEADER` with a number of lines is passed on like `HEADER`
(2.3, D5).

For each row the handler gets `values[]` and `nulls[]`, takes the value of the
distribution column, computes the bucket with the one lookup of 4.7, through the batch
sender (11.9) (a `NULL`
key is bucket 0, which is what PostgreSQL's hash partitioning does; the existing
function states this), and feeds the router's per-group buffers (11.4). Error
context lines ("COPY t, line 3, column x") come from installing
`CopyFromErrorCallback` as an `ErrorContextCallback`, which `file_fdw` does
too. The number of rows is reported as the `COPY n` tag. `pg_stat_progress_copy`
is not updated (documented).

### 11.7 Colocated insert from select, shipped whole (P6)

When the target and every source table are in the same colocation group (or
reference tables), the whole statement is shipped as the 2a write fragment to
every group of the target (the `kingres_shard_make_fanout_plan` path with
`all_rows`): each group inserts rows it reads from its own buckets into its own
buckets, with no row passing through the origin. Conditions, all checked at
the origin:

- the statement is shippable as a whole: only shippable expressions (5.1,
  extension objects included), no volatile or sequence default for a column the
  statement does not give (otherwise the rows go through the router, so that a
  default is computed once at the origin, 11.2), `RETURNING` shippable;
- the select list gives the **target's distribution column** directly as the
  **distribution key variable of the key class** of the source (a bare `Var`,
  possibly under a `RelabelType`), so the row's target bucket is its source
  bucket and the target partition is on the same group;
- the source is a pushdown unit's `FROM`/`WHERE` (6.1 rules 2 to 5), with no
  aggregation that crosses groups (a closed query only, 6.4);
- the `ON CONFLICT` clause is absent, or `DO NOTHING`, or **`DO UPDATE`** under
  the conditions of the next paragraph. `DO SELECT` (19) is refused.

**`ON CONFLICT DO UPDATE`.** The first draft forbade it, for the reason that
applies to the router: batching (11.3). A statement that every group runs whole
is not batched, and each group's executor sees the whole statement, so the
reason does not apply. `DO UPDATE` is allowed when all of these hold:

1. the conflict target (the inference columns and predicate, or the constraint
   named by `ON CONSTRAINT`) resolves to a unique index of the target whose
   columns include the target's distribution column. Every unique index of a
   distributed table includes it, because `create_distributed_table` and the
   utility hook refuse any other; P6 re-checks it at plan time by reading the
   target's indexes (`indisunique` and `indkey`), so a table that a superuser
   altered by hand with routing off is not shipped;
2. the `SET` list does not assign the distribution column (the rule of 2a for
   `UPDATE`; PostgreSQL itself refuses a result tuple that would appear in
   another partition);
3. the `SET` expressions, the `WHERE` of the `DO UPDATE` and the inference
   predicate are shippable and may use the pseudo-relation `EXCLUDED` and the
   target's columns, nothing else.

Why it is correct. Two rows conflict only when they have equal keys of a unique
index, hence equal values of the distribution column under the equality of its
type (which the hash operator family respects, and the collation is
deterministic), hence the same bucket and the same group. A new row lands on the
group that read its source row (the second condition above), and the existing
row it conflicts with lives in the same bucket on the same group. So each group
sees exactly the conflicts that a single node would see for the rows of its
buckets and none across groups. "ON CONFLICT DO UPDATE command cannot affect row
a second time" arises when two proposed rows conflict with each other or with
the same existing row; both have the same key and therefore run in the same
group's statement, which raises it as a single node does, and the whole
statement fails and rolls back everywhere (one two-phase transaction). Which of
several source rows wins is as unspecified as on one node. The command tag and
`RETURNING` rows are the sums over the groups.

Tests (`056_bulk_insert`, P6 part): a counter upsert (`SET n = t.n + EXCLUDED.n`)
repeated so that every row conflicts; duplicates in the source (the same error as
on the single node, and nothing changed); `DO UPDATE ... WHERE`; a conflict
target by constraint name and by inference with a predicate; `RETURNING`; a
source with `GROUP BY` on the key; a `SET` of the distribution column (refused); a
unique index without the distribution column made with routing off (refused, not
shipped); each compared with the single node and with the router path
(`enable_colocated_insert_select = off`, which routes the same statement when it
has no `DO UPDATE` and refuses it when it has one).

A receiving group needs no proof: a row that did not land in a local
partition fails with `no partition ... found for row`. The group's own
`plan_forwarded` accepts the statement when it carries `KS_WRITE_MARKER`, is an
`INSERT`, and passes the shippability walker, which this phase extends so that
`relation_ok` also accepts the colocated source tables of an `INSERT`. The fan-out
compares the placement of every target after the statement ran (`fanout.c`,
`run_all`), which includes the primary of each group (10.3). All other
`INSERT ... SELECT` statements use the router.

### 11.8 Reference tables and ledger tables

- A reference table is not routed: writes to it are the 2a fan-out. The router
  refuses a statement that writes a reference table and reads a distributed
  one (unchanged, table in the scope section). A multi-row `INSERT` into a
  reference table is the existing fan-out of the statement.
- A **ledger table** (`kingres_ledger`) cannot be a distributed table or a
  reference table in the current designs: `ledger_enable` refuses partitioned
  tables, and `create_distributed_table` and `create_reference_table` refuse
  tables with user triggers, which a ledger table has (its capture trigger,
  `ledger-mvp.md`). So a bulk write never targets a ledger table through the
  router. The two cases that touch one are:
  - `INSERT INTO <ledger table> SELECT ... FROM <distributed table>`: the target
    is a local table, the source is read through the scan or a pushdown path,
    and the rows are inserted by the ordinary `ModifyTable` on this node, with
    the ledger's `AFTER INSERT` capture trigger firing as always. The
    append-only and updatable guarantees (`ledger-0.2.md`) are properties of
    that trigger and of the ledger's protection hook; nothing in 2b bypasses
    either. This statement is not a pushdown unit (its `SELECT` is the source of
    an `INSERT`), so it uses the 2a scan.
  - Should a later phase allow ledger tables to be distributed, the router
    keeps the guarantees by construction: every row reaches its group through an
    ordinary `INSERT ... SELECT FROM unnest(...)`, which runs the group's row
    triggers, never through `session_replication_role = replica`, `COPY ...
    FREEZE` or direct heap insertion.

### 11.9 The batch sender and its destination

The router of 11.1 to 11.6 is two parts: the source of rows (an SPI cursor or
`NextCopyFrom`) and the **batch sender** (`batch_sender.c/h`), which owns the
buffers, the wire statement of 11.3, the flushing and the placement check. The
sender does not read the catalog to decide where a row goes: it is given a
**destination**, and the router builds one from the catalog entry of the target
table. Phase 9 ([shard-phase9.md](shard-phase9.md), "Copy", and its
`internal_copy_routed`) uses the sender for a table that is not in the catalog (a
shadow table that is being built), and expects exactly this interface.

```
typedef enum KsDestKind
{
	KS_DEST_PLACEMENT,			/* each row goes to the group that owns its bucket */
	KS_DEST_EVERY_NODE,			/* each row goes to every active primary */
	KS_DEST_ONE_NODE			/* every row goes to one node */
} KsDestKind;

typedef struct KsDestination
{
	KsDestKind	kind;
	const char *schema;			/* the destination table, unquoted */
	const char *table;
	Oid			relid;			/* InvalidOid when the table is not in the catalog */
	const KsColocation *placement;	/* KS_DEST_PLACEMENT: key type, collation and the bucket layout (4.7) */
	int			key_attno;		/* position of the key column in the rows given to the sender */
	int			node_id;		/* KS_DEST_ONE_NODE */
	int64		expect_placement;	/* the placement number the caller decided with; 0: no check */
	bool		check_identity;	/* a table that is not in the catalog: check its partitions */
} KsDestination;

typedef struct KsBatchSpec
{
	List	   *columns;		/* names and types of the columns of the rows */
	int			conflict;		/* KS_CONFLICT_NONE | KS_CONFLICT_NOTHING (with the inference of 11.3) */
	bool		overriding_system_value;
	List	   *returning;		/* shippable expressions, or NIL */
} KsBatchSpec;

KsBatchSender *kingres_shard_batch_begin(const KsDestination *dest, const KsBatchSpec *spec);
void  kingres_shard_batch_add(KsBatchSender *s, const Datum *values, const bool *nulls);
void  kingres_shard_batch_flush(KsBatchSender *s);	/* waits for every batch in flight */
int64 kingres_shard_batch_finish(KsBatchSender *s, Tuplestorestate *returning_rows);
```

Rules:

- **`KS_DEST_PLACEMENT`.** For each row the sender hashes the key
  (`kingres_shard_hash_of_value`), finds the bucket with
  `kingres_shard_bucket_for_hash(dest->placement, hash)` (4.7), takes the group
  that the placement gives that bucket, and the node that is the primary of that
  group in the **current** catalog, resolved once at `begin` and not by name at
  flush. `placement` is the catalog's `KsColocation` for a routed `INSERT` or
  `COPY`, and a description that the caller built (the placement of the shadow
  table, with each bucket's modulus once phase 10 exists) for phase 9. The sender
  never assumes `hash % bucket_count`.
- **`KS_DEST_EVERY_NODE`.** Each row goes to every active primary, one batch
  statement per node, in node-id order (the lock-order rule of 2a). It is refused
  for a table that the catalog says is a reference table: those writes are the 2a
  fan-out, which takes the cluster-wide writer lock and checks that the copies
  are equal (review S2-06, S3-03). It is allowed for a table that is not in the
  catalog.
- **`KS_DEST_ONE_NODE`.** Every row goes to `node_id`, which must be an active
  primary.
- **The placement check of the destination.** For a destination in the catalog
  (`relid` valid) every batch statement carries `place=<expect_placement>`, and
  the group checks it after planning (11.5). For a table that is not in the
  catalog there is no placement number or epoch to compare, so with
  `check_identity` the sender reads, at `begin`, `internal_partition_identity` of
  the destination on each node it will write to, and every batch carries
  `ident=<that number>`; the group compares its own after planning. A switch
  cannot attach or detach a partition of a table that no move can touch (phase 9
  excludes moves while the shadow exists), so the identity detects a change that
  the caller did not expect. The identity probe needs a superuser or a node
  session (`require_node_session`), which the phase 9 sender is; an ordinary role
  can use a destination in the catalog only. With `expect_placement = 0` and no
  identity check the group relies on tuple routing alone, which is enough to
  keep a row out of a wrong bucket and not enough to detect a stale caller.
- The sender takes no decision about the transaction: it marks every connection a
  writer (`kingres_shard_note_remote_write`) before its first batch and leaves
  the commit to the caller, so a caller that uses it inside a 2a transaction gets
  two-phase commit when two or more nodes wrote. A caller that sends in chunks,
  one transaction per chunk (phase 9's copy), commits between `flush` calls.
- An error in any batch aborts the caller's transaction and raises the error of
  11.4 with the context "bulk insert of <table>, batch <n> to group <g>".

What 2b tests of it: the router is the first user and builds its destination
through this interface (`KS_DEST_PLACEMENT`, `relid` valid, the colocation of the
catalog), so the placement path, the `place=` check, the primary rule and the
errors are tested by `056` and `058`. The part for a table that is not in the
catalog (`relid` invalid, `check_identity`, an explicit `KsColocation`) has no
test in 2b because 2b adds no SQL object that could reach it; phase 9's
`internal_copy_routed` is its first caller and brings the test. This is stated so
that nobody takes the part for tested. **[Needs verification]** that the explicit
destination needs no change of the sender when phase 9 is built; if it does, this
section is updated by that phase.

## 12. EXPLAIN

Each custom scan names what it did. The layout of the properties is fixed by the
regression tests (`pushdown_plans`); the examples show the content.

Pruning applies to the 2a scan, whose output gains a line:

```
Custom Scan (Kingres Distributed Scan) on orders
  Remote groups: 2 of 3 (pruned by orders.customer_id = 42)
  Remote SQL: SELECT ... FROM public.orders AS ks_t WHERE (...)
```

P2, a pushed join:

```
Custom Scan (Kingres Fragment Scan)
  Pushed: join (orders, customers: key class customer_id; customers_ref reference)
  Groups: 3 of 3 (local + 2 remote)
  Remote SQL: SELECT ks_t1.id, ks_t2.name FROM public.orders AS ks_t1 JOIN public.customers AS ks_t2 ON (...) WHERE (...)
```

P3, a partial aggregate:

```
Finalize HashAggregate
  Group Key: ks_t1.region
  ->  Custom Scan (Kingres Fragment Scan)
        Pushed: aggregate (partial)
        Groups: 3 of 3 (local + 2 remote)
        Remote SQL: SELECT ks_t1.region, sum(ks_t1.amount), avg(ks_t1.amount) FROM public.orders AS ks_t1 GROUP BY ks_t1.region
```

P4/P5, a closed query with order and limit:

```
Limit
  ->  Custom Scan (Kingres Fragment Scan)
        Pushed: aggregate (by key), sort, limit 20
        Merge: ks_t1.customer_id ASC NULLS LAST (3 groups)
        Groups: 3 of 3 (local + 2 remote)
        Remote SQL: SELECT ... GROUP BY ks_t1.customer_id ORDER BY ks_t1.customer_id LIMIT 20
```

The row router:

```
Insert on orders (Kingres Row Router)
  Source SQL: SELECT ... FROM staging
  Groups: all (3), batch 1000 rows / 1024 kB
  Conflict: DO NOTHING
```

`EXPLAIN (VERBOSE)` on a 2a scan of a statement that was not pushed adds
`Pushdown: not used: <reason>` with the first failed rule of 6.1 or of 5.4. The
text of the remote SQL is flattened to one line as the 2a explain does. `EXPLAIN
ANALYZE` adds, per group, the rows returned and the time to the first batch.

## 13. Settings

Every path that this phase adds has a setting that switches it off, so that
the row-moving scan (ADR 0022) answers whatever a path does not.

| GUC | Default | Context | Meaning |
|---|---|---|---|
| `kingres_shard.enable_pushdown` | on | `USERSET` | Master switch for P2 to P6. Off: the 2a plan shapes (scan with per-table restrictions); pruning and the placement check stay on. |
| `kingres_shard.enable_pushdown_closed` | on | `USERSET` | P4 and P5 without order or limit (closed queries, 6.4). |
| `kingres_shard.enable_pushdown_join` | on | `USERSET` | P2 (colocated join rows, section 9). |
| `kingres_shard.enable_pushdown_sort` | on | `USERSET` | The ordered merge and the pushed limit (section 8). Off: a closed query is still pushed (if `enable_pushdown_closed`), without order. |
| `kingres_shard.enable_partial_aggregation` | on | `USERSET` | P3 only. Off: an aggregate that is not closed is aggregated at the origin over P2 or P1. P4 (exact) stays on. Turn off when floating-point results must be repeatable to the last bit (A3). |
| `kingres_shard.shippable_extensions` | `all` | `USERSET` | `all`, `none` or a list of names from the compiled-in list (5.5). Narrowing only. |
| `kingres_shard.enable_bulk_routing` | on | `USERSET` | Off: a multi-group `INSERT` is refused as in 2a, and the non-constant-key single-row insert too. |
| `kingres_shard.enable_copy_routing` | on | `USERSET` | Off: `COPY ... FROM` into a distributed table of a multi-group cluster is refused as in 2a. |
| `kingres_shard.enable_colocated_insert_select` | on | `USERSET` | P6 (11.7). Off: the statement goes through the router when it can (no `DO UPDATE`) and is refused when it cannot. |
| `kingres_shard.insert_batch_rows` | 1000 | `USERSET`, 1 to 100000 | Rows per batch per group. |
| `kingres_shard.insert_batch_bytes` | 1MB | `USERSET`, 16kB to 64MB | Text bytes per batch per group (the sum of the lengths of the values). |
| `kingres_shard.scan_fetch_size` | 1000 | `USERSET` (exists) | Rows per `FETCH` of every fragment, 2a scans included. |

They only choose among plans that give the same answer, or among statements
that are refused, so ordinary users may set them; they grant nothing.
`kingres_shard.enable_routing = off` (superuser) and
`enable_distributed_scan = off` still switch off everything they switched off
before, pushdown included (a pushdown path needs the scan machinery). The
new `enable_*` switches and `shippable_extensions` are listed in the `security`
regression test with the other GUCs (coding-standards, 1.3: the test lists `SHOW
ALL` by prefix).

Every GUC is defined in `_PG_init` with `MarkGUCPrefixReserved`, as the others.
Turning every new switch off gives exactly the 2a behaviour, apart from the new
pruning, the placement check of fragments and the read of a local table with a
distributed table of another group (6.5), which are corrections and have no
switch; if one of them is found wrong the fallback is `enable_distributed_scan =
off`.

## 14. Version and merge order

- Phase 4 is 0.3 and its fix round 0.4 (committed). Phase 5, the high-availability
  part, is 0.5 (`ha-phase5.md` part C; being written, `kingres_shard--0.4--0.5.sql`
  and `kingres_shard--0.5.sql` are in the working tree). Phase 2b is **0.6**, with
  `kingres_shard--0.5--0.6.sql`. The later phases are one higher than their
  documents say (architecture.md, "Versions of `kingres_shard`"): phase 9 is 0.7
  and phase 10 is 0.8. Six documents still carry the old numbers
  (`shard-phase9.md`, `shard-phase10.md`, `shard-phase11.md`,
  `analytics-phase12-queries.md`, `analytics-phase12-storage.md`, and
  `ha-phase5.md`, which calls part C "version 0.4"); this revision does not change
  them.
- 2b adds no table, view, function or column. The script contains the standard
  guard and a comment only. The version is raised because the nodes must run
  the same fragment protocol (`kind=query|partial|insert`, `place=`, `ident=`,
  `ext=`): `add_node` already refuses a node that runs another version of the
  extension (README, "Nodes must agree"), and the probe of 4.3 carries the node's
  installed version and refuses a different one.
- The code steps (section 17, steps 1 to 12) change only C files, tests and the
  README and **do not change `default_version`**, so they can be merged before
  or after phase 5 and in any order relative to it. The last step (the
  version bump with the upgrade script and the `upgrade` regression test) must
  merge after 0.5 is committed, because the upgrade chain is
  0.2 -> 0.3 -> 0.4 -> 0.5 -> 0.6 and `ALTER EXTENSION UPDATE` needs every link. If
  phase 5 is delayed, the bump waits; it must not be renumbered to 0.5, which
  belongs to phase 5. Test file names start at `t/055`, the next free number after
  the tests of 0.5; the integrator renumbers at merge if another phase took them.
- Rolling upgrade: nodes of 0.6 and older nodes cannot cooperate on the new
  fragments. The probe refuses an older node whose SQL objects are older (a
  clear error that names it); a node whose library is older and whose SQL objects
  were already updated is the case the probe cannot see, so the instruction is
  the 2a one and is part of the README: during a rolling upgrade set
  `kingres_shard.enable_pushdown = off`, `enable_bulk_routing = off` and
  `enable_copy_routing = off` on every node (they are per session or per
  server), and switch them on when the last node is upgraded. An older node that
  receives a new fragment does not recognize the marker, so its rule "run locally
  only if provably local" rejects the statement with `KS001`; the fragment has no
  key restriction that the old analysis can use, so this holds for every kind.
  **[Needs verification]** that no fragment can be proved local by the old rule
  (an old `analyze_walker` reads `=` and does not read `IN` lists or `OR`; read
  `restrict_from_conjunct` and test with a 0.5 build of the node).

## 15. Security

### 15.1 The forwarded-session model, unchanged

A session is forwarded only if it carries `kingres_shard.forward_token`: the
hex form of `HMAC-SHA-256(secret, "kingres-forward-v2" NUL role NUL database NUL
id of the target node)`, made from `kingres.cluster_secret` (README, "The
forwarded flag"; the first draft named the earlier `v1` form without the
database). The receiving backend checks it at the first statement with a
constant-time comparison; a session that claims the flag and cannot prove it
gets `FATAL` before any statement runs. The token binds the role, the database
and the target node. This phase adds statement forms to the forwarded session
and does not touch the token.

### 15.2 No new way to run as another role

- Every connection is opened as the current user (`GetUserId()`) and cached per
  user (`KsConn` key). No statement of this phase changes the role; no
  `SET ROLE`, `SET SESSION AUTHORIZATION` or `SECURITY DEFINER` function is
  sent. The local part of a fragment runs through SPI as the current user, without
  `SECURITY DEFINER`.
- A fragment cannot do more than the role could do by connecting to the group
  and typing the same text: it is a `SELECT` (or an `INSERT` into the
  distributed table, 11.5), re-checked by the group's walker (4.2), and
  privileges are checked on the group. The privilege on the origin's tables is
  checked at the origin by the executor too (the pushdown plan keeps the range
  table entries, with their `RTEPermissionInfo`, in the final range table, and
  `ExecCheckPermissions` runs on them).
- The row router is an `INSERT`: the origin checks `ACL_INSERT` on the
  columns named (`rte->perminfoindex`), the group checks it again. The router
  needs no privilege on `kingres.*` catalog tables.
- The `COPY` handler repeats the privileges `DoCopy` checks (11.6) and has a
  test for each.
- `kingres_shard.forwarded` remains `PGC_BACKEND` and token-protected. The
  fragment markers are comments and grant nothing.

### 15.3 Who controls each part of a statement text sent to another node

| Text | Built by | Table, column, function, operator, type names | Values | Other parts |
|---|---|---|---|---|
| scan, query, partial fragment (`DECLARE ... CURSOR FOR SELECT ...`) | `pg_get_querydef` and `deparse_expression` under the transmission modes | from catalog OIDs of the planned statement, qualified with their schema by the deparser (`search_path = pg_catalog`), identifiers quoted by `quote_identifier`: the creator of an object controls its name, and a name with a quote, a space, a newline or `*/` is quoted, not interpreted | constants in the statement are deparsed by ruleutils (`simple_quote_literal` doubles the quote; with `standard_conforming_strings = on` a backslash is literal); external parameters and the values computed by 5.2 travel as parameters of `PQsendQueryParams`, never in the text | the cursor name `ks_frag_<n>` is generated; the marker comment holds only a fixed word and decimal numbers; `LIMIT` is a bigint parameter |
| insert fragment (`INSERT ... SELECT FROM unnest($1::text[], ...)`) | the router, once per statement | as above; casts use the schema-qualified type name | every row value is a parameter; nothing a row contains is in the text | column order, the `OVERRIDING SYSTEM VALUE` keyword and `ON CONFLICT` are fixed text; the constraint name is quoted |
| the local part (SPI) | the same texts | as above | as above | the `/* kingres:local <nonce> */` comment, with a random nonce |
| `FETCH`, `CLOSE` | fixed format with the generated cursor name and a decimal | none | none | none |
| the `COPY` stream | not a statement | none | the client's data, parsed by `NextCopyFrom` at the origin; the groups receive values as parameters, not as a data stream | none |

The hostile-identifier and hostile-value test (section 16, `066_hostile_values`) exercises
every row of this table.

### 15.4 Other points

- A user-defined function runs only at the origin: through 5.2 (a value computed
  once) or as an expression the planner left above the pushed part. It never
  runs on another node as part of a fragment. **The one thing that runs on
  another node and is not built in** is an object of an extension on the
  compiled-in list (5.5): code of the extension's shared library, immutable, not
  `SECURITY DEFINER`, without settings, at a version that the group checks. A
  role that can create functions or a schema cannot add to the list, and the
  setting that touches it can only narrow it.
- The local part of a fragment runs through SPI as the current user, without
  `SECURITY DEFINER` and with the nonce of 4.4 valid only for that SPI call.
- A distributed transaction's remote connections still use the user's
  password-less or TLS credentials as in 2a; the README's advice to use TLS for
  node-to-node connections stands.
- `kingres_shard.node_conninfo` and the secret are unchanged. The values of a
  batch are parameters of the statement and are logged by a node that logs
  parameters, up to `log_parameter_max_length` (10.5); no token or secret is in
  a statement.

## 16. Tests

Rules from `testing.md` apply: tests are not tied to the developer's machine,
none is flaky, a bug fix starts with a test, and C changes are run on a build
with assertions enabled (several items above rely on `Assert`s of the
executor; the tests that free libpq results also run with
`MEMORY_CONTEXT_CHECKING`). **Every test of this phase runs on PostgreSQL 18 and
on PostgreSQL 19** (ADR 0023): the regression files with alternative expected
files only where the output differs, and the TAP files unchanged. `pg_regress`
tests run on one node with a fictitious second shard group in the catalog, as
`utility_guard` and `routing_decisions` do; they decide plans with `EXPLAIN` and
never execute a remote fragment. TAP tests use three nodes and the comparison
harness.

### 16.1 pg_regress (`contrib/kingres_shard/sql`, `expected`)

| File | What it proves |
|---|---|
| `pushdown_plans` | For a fictitious two-group catalog: every shape of sections 3, 8, 9 and 11 gets the plan of section 12; every refusal rule of 6.1 and 5.4 gives the 2a plan and the reason line of `EXPLAIN (VERBOSE)`; each new GUC switches its path off and only its path; pruning shows `Remote groups: n of m`; a statement with a local table is not a unit and gets the scan (6.5). |
| `pushdown_shippable` | The shippability matrix of 5.1: one statement per rule, each shipped or not as stated (`now() - interval` through 5.2 ships, `random()` does not, `CoerceViaIO` does not, `money` does not, a column with `COLLATE "C"` ships, a non-default collation does not, a user function does not, a `reg*` constant of a user table does not, `ON CONFLICT DO SELECT` does not on 19). Run under hostile session settings. With `kingres_sketch` installed: its immutable functions and aggregates ship, a user function with the same name does not, a function of a non-listed extension does not, `shippable_extensions = 'none'` stops it. |
| `pushdown_agg_catalog` | The classification of 7.1 recomputed from `pg_aggregate` at run time equals the expected file (the built-in aggregates and every aggregate that `ext_object_ok` accepts), and every partial-capable aggregate's plan has `Finalize` over the fragment scan, every other has not. |
| `pushdown_joins_plans` | The join matrix of 9.1: each row of both tables gives pushed or not pushed, with the reason; on 19 with `enable_eager_aggregate` on and off (2.3 D1). |
| `bulk_plans` | `EXPLAIN` of each insert form shows the router; refusals (`ON CONFLICT DO UPDATE` through the router, `DO SELECT`, reference-table writer that reads a distributed table, `COPY ... WHERE`, `FREEZE`, `ON_ERROR`, `FORMAT json`) give their message; privilege matrix of `COPY` checks (no `INSERT`, column privileges, file and program roles, read-only transaction). |
| `security` (extended) | Lists the new GUCs; the scan of `pg_proc` for functions without a pinned `search_path` still finds none (2b adds no function); `SHOW kingres_shard.enable_pushdown` and the others are visible; `shippable_extensions` accepts only the compiled-in names. |
| `upgrade` (extended) | `ALTER EXTENSION kingres_shard UPDATE` from 0.5 to 0.6 gives a catalog identical to a fresh 0.6, and the chain from 0.2. |

### 16.2 TAP (`contrib/kingres_shard/t`)

**The comparison harness.** `t/KingresShardCompare.pm` exists. It builds a
three-node cluster and a single node, runs a query on all four and compares the
output (`compare()`), repeats it under four session profiles (`DateStyle`,
`IntervalStyle`, `extra_float_digits`, `TimeZone`, `bytea_output`, `lc_*`;
`profiles => 0` skips them), and has `compare_dml()` for statements that count
rows. It has no notion of a plan, of two runs on the cluster compared with each
other, or of a result that is not exactly reproducible. This phase adds, in
step 1 of section 17, without changing what a test written for 2a does:

1. **Plan profiles**: `compare(..., plans => [...])` runs the query on every node
   under each list of settings (`scan`: `enable_pushdown = off`, the 2a plan;
   `pushed`: the defaults; `nopartial`: `enable_partial_aggregation = off`;
   `nopruning` where a test needs it), and requires equality of every profile
   with the single node **and with the `scan` profile**, so a pushed answer is
   compared with the scan's answer for the same statement, which is ADR 0022's
   rule 3.
2. **A path assertion**: `compare(..., path => 'Kingres Fragment Scan')` runs
   `EXPLAIN` under the `pushed` profile and requires the custom scan (or
   `path => 'Kingres Distributed Scan'` for the scan), so that a test cannot pass
   because nothing was pushed.
3. **Normalization**: a `float4`/`float8` result is rounded to 9 significant
   digits (A3), and the elements of an `array_agg`/`string_agg` result are sorted,
   before comparison; the hash of the whole normalized output is printed on a
   failure.
4. **The generator**: a seeded query generator (`KS_SEED`, printed in the test
   output so that a failure is reproducible) that builds queries from templates
   and runs them through the same `compare()`.
5. **Write helpers**: `copy_in($node, $table, $data, %options)` for `COPY FROM
   STDIN`, and `compare_tables($table)`, which compares the whole table on the
   cluster (by `COPY (SELECT ...) TO`) with the single node after a write. The
   existing `compare_dml()` runs the statement inside a `DO` block, which cannot
   contain `COPY` and plans the statement as a nested one; the router tests also
   run the statement at the top level.

Common to the equivalence tests: the data is generated by `generate_series` with
a fixed seed and includes `NULL` keys and values, duplicate rows, skewed keys
(one bucket holds half the rows), a group that owns no row of one table, an empty
table, very wide rows, text with the characters of 16.4, `float8` with `NaN`,
`-0` and infinities, `numeric` of many digits. The generator produces 500 queries
per run: filter, join (all join types of 9.1), group by with aggregates from 7.1,
`HAVING`, order by, limit and offset, and combinations; each test also keeps a
fixed list.

| File | What it proves |
|---|---|
| `055_fragment_foundation` | The bucket-lookup check (a search of `src/*.c` for `bucket_count` and for `%` applied to a hash outside the files of 4.7); the harness itself (a deliberately wrong path is caught); pruning (`IN` lists, `= ANY`, parameters, `OR`) with the plan and the result; the placement probes for the 2a scan and for fragments: the sequence of review S4-01 (move away, scan resumes after the move back) on the origin's pair of groups and on another pair, a placement change between the two probes, a node with another extension version; the nonce (a function that runs the text of the local fragment inside the statement is still routed; the 2a fan-out local part has the same order); reads that mix a local table with a distributed one (6.5), scroll cursors on them; results and memory: a cancel in every wait of the scan, with assertions and `MEMORY_CONTEXT_CHECKING`; the 2a statements of `048_hostile_values`. |
| `056_bulk_insert` | Each insert form of 11.1 with data hashing to all groups: multi-row `VALUES`, `INSERT ... SELECT` from a staging table, from a distributed table (router) and from a colocated one (P6), a default key, `serial` and identity (`ALWAYS` and `BY DEFAULT`) columns, a stored and a virtual generated column, an immutable and a volatile default, `ON CONFLICT DO NOTHING`, the refusal of `DO UPDATE` through the router and of `DO SELECT` on 19, P6 with `DO UPDATE` (11.7), `RETURNING` (rows and counts), a foreign key to a reference table and to a colocated table, savepoints, rollback; row counts and the table after equal the single node's (`compare_tables`) and equal the router's when P6 is switched off; every group's partitions hold exactly the rows of their buckets; a batch that names a bucket partition is refused; a cached plan after `create_reference_table` and after a move; memory stays bounded for 5 million rows with `insert_batch_rows = 100` (the test samples the backend's memory contexts through `pg_log_backend_memory_contexts` or a fixed upper bound on RSS). |
| `057_bulk_copy` | Text, CSV and binary `COPY FROM STDIN` and from a file, `HEADER`, `NULL`, `QUOTE`, `DEFAULT`, `FORCE_NULL`, malformed rows (the error names the line and column), a row with a `NULL` key, `COPY` into a table with a `serial` column, refusals (`FREEZE`, `WHERE`, `ON_ERROR`, `REJECT_LIMIT`, and on 19 `ON_ERROR set_null` and `FORMAT json`), the privilege checks of 11.6, `psql \copy`. |
| `058_bulk_faults` | With `kingres_shard.fault_injection` points `bulk_*` and the 2a points: an error before and after the first batch, between groups, before the last flush; a group `kill`ed (`pg_ctl stop -m immediate`) in the middle; the origin killed after the decision is logged and before `COMMIT PREPARED`; the target group out of connections; a `statement_timeout` and `pg_cancel_backend` in the middle. After each: `pg_prepared_xacts` is empty on every node once recovery has run, no node holds a row of the statement if it rolled back, every node holds its rows if it committed, `kingres.dtx_log` is consistent. |
| `059_pushdown_closed` | P4 and P5 without order or limit: closed queries (6.4) on every key shape, joins on the key, `DISTINCT` with the key, windows partitioned by the key; the result equals the single node and the `scan` profile; the plan is pushed; every refusal rule of 6.1 gives the scan and the same rows; the group refuses a forged fragment (the rules of 4.2). |
| `060_pushdown_agg` | Every aggregate of 7.1 on every type it takes (integer, `numeric`, float, text, `bool`, bit, `interval`, array, `date`, `timestamptz`), with `FILTER`, `HAVING`, several aggregates sharing a state, `GROUP BY` an expression, scalar aggregate over empty and non-empty tables (A7), `count(DISTINCT)` (A4: closed gives the pushed plan, open gives the 2a plan), ordered-set and hypothetical-set aggregates (open: 2a; closed: pushed), grouping sets (2a), window above aggregate, aggregates over a table with a dropped column; `money` columns stay on the scan; the partialize step on each plan strategy and on a build with assertions, through a group's `DECLARE` and through the origin's SPI cursor. |
| `061_pushdown_sort_limit` | O1 to O7: the ordering combinations of O2 (including `NaN`), ties, offset beyond the rows, `LIMIT 0`, `LIMIT NULL`, `WITH TIES`, a cursor fetched in parts and closed early (the group cursors are closed, `pg_cursors` on the groups is empty), a limit that is larger than the table, memory bounded with a small `work_mem` and a large result. |
| `062_pushdown_join` | The matrix of 9.1 on data with matching, non-matching and `NULL` keys: every permitted join equals the single node and the scan; every refused join gives the 2a plan and the same rows; a self join; a three-way join with a reference table; a join under a different collation; two key classes; on 19 a join under an aggregate with eager aggregation on (the plan has a pushed join and no pushed join under a grouped relation). |
| `063_pushdown_consistency` | 10.1, 10.2 and 10.4: a concurrent writer on the groups while a pushed aggregate and a pushed join run (every result is a state that existed on each group); the placement check (a change behind the origin's back is `KS001`); a bucket move running in a loop while the equivalence queries run on every node: no row twice, none missing, errors only `KS001` and the switch retry; a long read transaction holds back the switch and the timeout returns the move to catching up; prepared statements executed six times, with `force_generic_plan`, and after a move; a transaction that sees its own writes through a fragment. |
| `064_pushdown_failover` | 10.3, path by path (its tests are listed there). |
| `065_pushdown_extensions` | With `kingres_sketch` on every node: `approx_count_distinct`, `hll_add_agg`, `topn_add_agg`, `tdigest_agg` and `approx_percentile` pushed as partial aggregates and, when closed, whole; the results equal the scan's and, for the sketches whose merge is exact, the single node's; a node at another extension version (`KS001` with the detail, then success after the upgrade and with `shippable_extensions = 'none'`); a user function with the name of a sketch function is not shipped; a forged fragment with `ext` for a non-listed extension is refused. |
| `066_hostile_values` | Identifiers and values cross every deparser intact (16.4), for every statement shape. |
| `067_pushdown_security` | A client with `forwarded` and no token is refused for every fragment kind; a forged fragment of a valid session (a join of two tables of different groups, a user function, a sub-select with a wrong shape or without `rp`, a write, an unknown marker key, no `place`, a stale `place`) is refused by the group's walker with `KS001` and runs nothing; a demoted node refuses every fragment kind; privileges: a role without `SELECT` on one table of a join, without `INSERT`, with column-level privileges; a security-barrier view and a table with row-level security (not a unit); a view owned by another role; the `COPY` privileges; the local-fragment nonce cannot be guessed; a `PARALLEL SAFE` function that runs a pushdown statement fails in a worker; the log of a failing batch has the values and no token. |
| `068_pushdown_ledger` | The cross-extension test of 11.8. |
| `069_pushdown_equiv` | The generated equivalence of every path together: the result equivalence on the whole generated list, under the four session profiles of 2a and the plan profiles; `enable_pushdown = off` and `on` give equal results; the plans of a fixed list are checked to be pushed (so the test cannot pass by never pushing). It runs last in a step and is the test that a later phase extends. |

A cross-extension test `068_pushdown_ledger` loads `kingres_ledger` with
`kingres_shard`: `INSERT INTO <ledger table> SELECT ... FROM <distributed table>`
from every node keeps the ledger's guarantees (rows captured exactly once,
`ledger_verify` clean, an `UPDATE`/`DELETE` of an append-only table still
refused), and a `COPY` into a distributed table by a role that also owns ledger
tables does not touch them.

### 16.3 Tests per correctness condition

A1, A2, A3, A5, A7, A8, A10: `060`. A4, A6, A9: `060` and `pushdown_plans`.
O1 to O7: `061`. J1 to J5: `062`. Consistency 10.1, 10.2 and 10.4: `063`; 10.3:
`064`; 10.5 per row of its table. Section 11: `056` to `058`. Section 5:
`pushdown_shippable`, `065` and `017_settings_compare` run with the pushdown paths
on. Section 4.7: `055`. Section 6.5: `055`. Section 2.3: the tests named there.

### 16.4 Hostile identifiers and values

The test creates tables, columns, schemas, constraints, types and sequences
whose names are: `"a""b"`, `"a b"`, `"select"`, `"t\nnl"`, `"x*/ ; DROP TABLE y; /*"`,
a name of 63 bytes, a name with a 4-byte UTF-8 character, `"$user"`,
`"public"`, and mixed case; the same names for a role. It then runs every
statement shape (scan, join, partial aggregate, closed aggregate, ordered merge
with limit, router insert with `RETURNING` and `ON CONFLICT DO NOTHING`, `COPY`
with a column list) over them, and checks the result against the single node.
Values: strings containing `'`, `\`, `\\`, newline, tab, carriage return, `{`,
`}`, `,`, `"`, the text `NULL`, `\N`, an empty string, the 2-byte and 4-byte
UTF-8 characters, a string of 1 MB; `bytea` with all 256 byte values; `jsonb`
with nested quotes; arrays with `NULL` and with quoted elements; composite and
range values with quotes; enum labels with quotes; `timestamptz` before the
year 1 and `infinity`; `float8` `-0`, `NaN`, `1e308`; `numeric` with 1000
digits; `money`. Also: objects in `public` that shadow built-in names
(a function `public.sum`, an operator `public.=` on `text`, a type `public.text`)
must not change the result on any group (the text is fully qualified and the
remote `search_path` is `pg_catalog`); a `SET search_path` and `SET
standard_conforming_strings = off` in the session do not change the result.
For the router the same values cross as `text[]` elements; the test inserts each
value through every path and reads it back by `hashtext`/`md5` of the text form
on the group and at the origin.

## 17. Implementation order

Each step is a commit that can be merged alone and that leaves the extension
releasable. The rules for every step:

- **It builds and passes on PostgreSQL 18 and 19** (ADR 0023), with the
  existing tests unchanged, and on a build with assertions.
- **The row-moving scan answers whatever the step does not cover.** A new path
  is one more alternative chosen by cost, or one more form of write that was
  refused; a statement outside it gets exactly the plan it got before.
- **Each pushdown has a setting that switches it off** (section 13), and with
  all of them off the extension behaves as 2a. A step that is a correction
  (steps 1 and 2) has no switch and its fallback is `enable_distributed_scan =
  off`.
- **Its tests compare the new answer with the scan's answer for the same
  statements** (and with the single node), through the plan profiles of the
  harness (16.2). The harness exists in `t/KingresShardCompare.pm` and is
  extended in step 1; a test that passes because nothing was pushed is caught by
  the path assertion.
- It is sized for one engineer and one independent reviewer (about 1000 to 1500
  changed lines of C plus tests).

**Why this order.** The largest gain for the least risk comes first. Step 2
removes a refused read, which ADR 0022 does not allow, hardens the scan that
exists, and is needed by every later step. Steps 3 and 4 turn the refusal of
loading data across groups (multi-row `INSERT`, `INSERT ... SELECT`, `COPY`) into
a feature: the gain is a statement that could not be run at all, the risk is low
because no planner path is involved (an executor node, SPI and libpq, on the
commit protocol of 2a), and phase 9 cannot start without the batch sender. Step 5 adds
the colocated `INSERT ... SELECT`, which reuses the 2a fan-out. The pushdown
paths then come in the order of rising risk: whole closed queries (final rows,
nothing to merge or combine), partial aggregation (the largest reduction in rows
moved, with a rewrite of a finished plan on the groups), extension aggregates
(a list and a version check), the ordered merge and limit (a merge in the custom
scan), and last the join rows, which touch the join hook and, on 19, eager
aggregation.

| Step | Content | Files touched | Switch | Tests that must pass and be added |
|---|---|---|---|---|
| 1 | **Compatibility, bucket lookup, harness. No behaviour change.** The definitions of 2.3 in `pg_compat.h`; `kingres_shard_hash_of_value`, `kingres_shard_bucket_for_hash`, the bucket accessors and the signature change of `kingres_shard_try_bucket` and `kingres_shard_bucket` (4.7), every caller changed; the harness additions of 16.2 (plan profiles, path assertion, normalization, `copy_in`, `compare_tables`, the generator skeleton) | `pg_compat.h`, `hashing.c/h`, `catalog.h`, `planner.c`, `dist_scan.c`, `fanout.c`, `dist_table.c`, `t/KingresShardCompare.pm` | none | every existing regression and TAP test on 18 and 19 unchanged; `055` part A (the source check of 4.7, the harness self-test) |
| 2 | **The placement check, pruning, the local part, HA, local-table reads.** `placement_check.c/h`: the plan number, the probes with the identity and the extension version, `fragment_error` (4.6), the rule "no other node of the same group in this transaction" (4.4), used by `ds_start` and `fanout.c`; pruning from `baserestrictinfo` for the 2a scan (4.5); the nonce replacing `kingres_shard_in_fanout_local`, after `ensure_fresh_partitions` (4.4); `type_ok` excludes `money` (5.1); the reads of 6.5 and the `SCROLL` cursor are answered by the scan; the `t/002_routing.pl` assertion of the refusal becomes an equality | new `placement_check.c/h`; `dist_scan.c`, `fanout.c/h`, `planner.c`, `shippable.c`, `node_conn.c/h` (the group rule), `fault.c` (points) | none (a correction) | `013`, `014`, `026`, `031`, `032`, `049` and `002` unchanged or updated as said; `055` parts B to F: pruning, the S4-01 sequence for the scan, the nonce, local-table reads, cancels at every wait; `049` extended: a scan across `promote_node` |
| 3 | **The batch sender and the row router for `INSERT`**: the destination interface (11.9) with the router's use of it, the plan, the SPI source, the buffers, the wire statement, `RETURNING`, `ON CONFLICT DO NOTHING`, the insert fragment rule (11.5), the non-constant single-row key | new `batch_sender.c/h`, `row_router.c/h`; `planner.c`, `remote_scan.c` (shared helpers), `node_conn.c/h` (async send for batches), `fault.c` | `enable_bulk_routing` | `bulk_plans`, `056` (router part), `058` (insert part), `066` (router part), `064` (router part), `067` (batch and log part) |
| 4 | **`COPY FROM` through the router** (11.6) | `utility.c`, `row_router.c` | `enable_copy_routing` | `057`, `058` (copy part), the privilege matrix of `bulk_plans` |
| 5 | **The analysis module and the colocated `INSERT ... SELECT` (P6)**: `pushdown.c/h` (eligibility 6.1, key class 6.3, closed 6.4), the multi-relation mode of the shippability walker, `DO UPDATE` with its checks (11.7) | new `pushdown.c/h`; `shippable.c/h`, `planner.c`, `fanout.c` | `enable_colocated_insert_select` | `pushdown_shippable`, `pushdown_plans` (P6 part), `056` (P6 part) |
| 6 | **The fragment scan and closed queries (P4, P5 without order or limit)**: the refactor of `DsFrag` start, fetch, drain and stop out of `dist_scan.c` into `fragment_scan.c/h`; the local part through SPI; the marker kinds `query`; the `PushdownInfo` hand-over from `planner_hook_body`; the group-side rules of 4.2; EXPLAIN (section 12); the first measurement against the scan | new `fragment_scan.c/h`; `dist_scan.c`, `planner.c`, `pushdown.c`, `kingres_shard.c` (GUCs), `Makefile`, `meson.build` | `enable_pushdown`, `enable_pushdown_closed` | `059`, `063` (consistency part), `064` (scan part), `067` (forged fragments), `069` (the part that applies) |
| 7 | **Partial aggregation (P3)**: the partial target, `partialize_plan` on the groups and the local part with `KS_PARTIAL_PLAN_GUCS`, the final path with `create_agg_path`, the catalog test | `pushdown.c`, `planner.c` (`partialize_plan`, the `kind=partial` rule), `fragment_scan.c` | `enable_partial_aggregation` | `pushdown_agg_catalog`, `060`, `069`, on a build with assertions |
| 8 | **Objects of extensions (5.5)**: the compiled-in list, `ext_object_ok`, `ext=` in the marker and its check, the narrowing setting; `kingres_sketch` aggregates through P3 and P4 | `shippable.c/h`, `pushdown.c`, `planner.c`, `kingres_shard.c` | `shippable_extensions` | `pushdown_shippable` (extension part), `065`, `pushdown_agg_catalog` |
| 9 | **Ordered merge and limit (P5 with order, section 8)**: the `SortSupport` merge, `create_limit_path`, the limit parameter | `fragment_scan.c`, `pushdown.c` | `enable_pushdown_sort` | `061`, `069` |
| 10 | **Join rows (P2)**: `set_join_pathlist_hook`, `KS_IS_GROUPED_REL`, the join matrix | `pushdown.c`, `fragment_scan.c`, `planner.c` | `enable_pushdown_join` | `pushdown_joins_plans`, `062`, `069` |
| 11 | **Consistency and failover test completion and the README** rewrite of "Guarantees" and "Limitations" for pushdown, bulk writes and `KS001` (10.2, 10.3) | `README.md`, tests | none | `063`, `064` complete |
| 12 | **Security round**, the cross-extension ledger test, `EXPLAIN` polish, README | `README.md`, tests | none | `067`, `068`, `066` complete |
| 13 | **Version bump to 0.6** and the upgrade script; merges after 0.5 is committed | `kingres_shard.control`, `kingres_shard--0.5--0.6.sql`, `kingres_shard--0.6.sql`, `Makefile`, `meson.build`, `sql/upgrade.sql` | none | `upgrade` |

Steps 1 and 2 are the riskiest for review (they move code); step 1 has no new
behaviour and step 2 only the pruning, the check, the read of 6.5 and the `money`
rule, so the reviewer can compare them with the 2a tests. Steps 3 to 5 do not
depend on steps 6 to 10 and can be reviewed and released in either order with
them. Steps 6 to 10 each add one kind of path and can be reviewed against the
matching conditions of sections 6 to 9; step 7 needs step 6, step 9 needs step 6,
step 10 needs step 6, and step 8 needs step 7.

## 18. Out of scope, and what a user still sees refused after 2b

- Statements that are not pushdown units use the 2a scan and are correct but
  slow: any statement with a subquery, view, CTE, set operation, window function
  not partitioned by the key, a local table (answered by the scan, 6.5), a
  `LATERAL` item, a semi or anti join (`IN`, `EXISTS`), grouping sets, an
  aggregate that has no combine function and is not closed, a non-shippable
  expression, a `money` value, two colocation groups, a join not on the key, an
  `INSERT ... SELECT` source of its own (it is planned as a subquery).
- Refused as in 2a: `SELECT ... FOR UPDATE/SHARE` across groups, system columns
  of a distributed table, `UPDATE ... FROM`/`DELETE ... USING`/`MERGE` that read
  a distributed table and write another, `MERGE` on distributed tables of
  another group, a statement that writes a distributed table and reads a local
  one on another group, a reference-table write that reads a distributed table,
  an `UPDATE` of the distribution column, `COPY FROM` into a reference table on a
  cluster with several groups, a routed statement under `force_generic_plan`.
- New refusals, all clear errors: `INSERT ... ON CONFLICT DO UPDATE` whose rows
  go through the router (the colocated form is allowed, 11.7); `ON CONFLICT DO
  SELECT` (19) through the router and P6; `COPY ... FROM` with `FREEZE`, `WHERE`,
  `ON_ERROR`, `REJECT_LIMIT` on a distributed table of a multi-group cluster; a
  bulk `INSERT` whose `RETURNING` or inference clause is not shippable.
- Not provided: a global snapshot, parallel query on the groups' fragments, a
  statement-level retry after `KS001` for fragments, `RETURNING` in the input
  order of a multi-group bulk insert, `pg_stat_progress_copy` for a routed
  `COPY`, pushdown for a user-defined aggregate, a window function not
  partitioned by the key, `DISTINCT` pushed as a partial operation, distributed
  ledger tables, protection against an old primary that was cut off and not yet
  fenced (the lease of phase 5), and a test of the explicit destination for a
  table that is not in the catalog (it needs phase 9's caller, 11.9).
- A bucket move or a failover can still make a statement fail with `KS001`
  while the switch commits or the primary changes; the client retries.

## 19. Needs verification

Items of the first draft that this revision settled by reading the source are
marked "settled" with what settled them; they stay in the list only where a test
is still needed.

| # | Item | What would settle it |
|---|---|---|
| V1 | `partialize_plan` (7.3): the plan of the restricted fragment has an `Agg` on top under the settings of 7.3 for every strategy; the result descriptor of a cursor and of an SPI portal shows `bytea` for a serialized state. **Partly settled**: `mark_partial_aggref`, the `nodeAgg.c` assertion and the executor's source of the result type were read (identical on 19) | A test per strategy on an assertion build, through a group's `DECLARE` and the SPI cursor, comparing with the single node (`060`) |
| V2 | `set_customscan_references` and `convert_combining_aggrefs` accept a CustomScan whose `custom_scan_tlist` holds partial `Aggref`s (7.4). **Settled by reading** (`set_plan_refs` processes the `Agg` before its child; identical on 19) except that a `CustomScan` does it too | The plan of any partial aggregate, `EXPLAIN` and a run (`060`) |
| V3 | `pg_get_querydef` of the modified copy (select list replaced, clauses cleared, `GROUP BY` by sort-group reference) prints a valid statement for every shape of the tests (7.4, 4.1) | `pushdown_plans` prints each fragment; a round trip (parse the text, plan it) for each |
| V4 | A `CustomScan` with `scanrelid = 0` that replaces a join keeps the original range table entries and their OIDs in `relationOids` (10.4). **Settled by reading** `add_rte_to_flat_rtable` ("even though the RTE might be unreferenced in the plan tree"; identical on 19) | The prepare, move, execute test of `063` still runs, as a regression test |
| V5 | That `NextCopyFrom` evaluates the default expression (`nextval`) of an omitted `serial` or identity column, in the origin's session, for every row (11.2 relies on it), including for `GENERATED ALWAYS` identity columns, and that `BeginCopyFrom` accepts a partitioned table as `rel` (`file_fdw` passes a foreign table) | `057`: `COPY` into a table with each kind of default and identity column, comparing the values with the single node |
| V6 | The exact checks of `DoCopy` to repeat (11.6). **Settled by reading** `DoCopy` on 18.7 and 19: `pg_read_server_files`/`pg_execute_server_program`, `ExecCheckPermissions` with the inserted columns, `check_enable_rls` (error for `COPY FROM` under row-level security), `XactReadOnly && !rel->rd_islocaltemp` then `PreventCommandIfReadOnly`; 19 adds a system-column check in `WHERE`, which `COPY ... WHERE` refusal makes moot | The privilege matrix of `bulk_plans` fails if one is missed |
| V7 | `lc_time` and other `lc_*` settings that make an immutable built-in function differ between nodes (5.3) | The test described in 5.3 |
| V8 | A fragment cursor can be opened by the forwarded session as `DECLARE ... NO SCROLL CURSOR FOR <query with aggregate and join>` with the parameters of the extended protocol, as 2a does for a scan | `059`, `060` |
| V9 | The cursor's `ACCESS SHARE` lock on the parent stays until the end of the remote transaction and a `DETACH` waits for it (10.2 fact 3), for a fragment that reads through a cursor | A TAP test that holds a fragment's remote transaction open and shows `kingres.move_bucket` waiting on it (`063`) |
| V10 | The cost model: pushdown paths win against the 2a scan in the cases of the tests without hints | Tuning in steps 6 to 10 with `EXPLAIN`; the constants are in the regression expected files |
| V11 | `unnest` with several array arguments in `FROM` and a `text[]` cast per column has the throughput claimed for a batch of 1000 rows | A measurement in step 3 against a multi-row `VALUES` batch; if it is much slower the wire format changes behind the same sender interface, and this note is updated |
| V12 | Memory bound of the router for the stated settings (11.4) | `056`'s sampling of the memory contexts |
| V13 | That `pull_up_sublinks` and the other preprocessing really are the only reasons the planner's tree cannot be deparsed, and that no `Query` in a unit (no sub-links, no subqueries, 6.1) is changed by the rewriter in a way `pg_get_querydef` of the copy would not reproduce (for example a view expanded into a subquery, which 6.1 refuses) | `pushdown_plans`: every shape of the unit prints a fragment that parses and plans, and `pg_get_querydef` of the copy is compared with the statement as written for a list of statements |
| V14 | **Withdrawn.** The first draft's "the lock taken while planning processes invalidations, so the catalog is current after the lock". The `kingres_internal_placement_hash` comment and the 0.3 review show it is not ordered with the commit; 4.3 does not rely on it | the probes of 4.3 |
| V15 | `planOrigin = PLAN_STMT_UNKNOWN` in a hand-built `PlannedStmt` is harmless on 19 (2.3 D3). 0.4 builds such plans and passes its tests on 19 | the existing `remote_scan.c` plans under the tests of 19 |
| V16 | A portal opened by `SPI_cursor_open_with_args` survives `SPI_finish` and can be fetched in a later `SPI_connect` inside an executor node, with a function that uses SPI called between rows (4.4). `spi.c` copies the query string and the plan into the portal context | a test that calls such a function between rows and cancels at every wait |
| V17 | A fragment of an older node cannot be proved local by its rule (14) | read `restrict_from_conjunct` for `IN` lists and `OR`; a test with a node of 0.5 |
| V18 | The built-in `IMMUTABLE` functions do not run SQL (a nested statement in a fragment would meet the planner hook with the nonce of the local part) | a query over `pg_proc` for `IMMUTABLE` functions whose `prosrc` is in a list of SPI users, in `pushdown_shippable` |
| V19 | The explicit destination of 11.9 needs no change of the sender when phase 9 uses it | phase 9's test |
| V20 | `ha-phase5.md` part C and the code of 0.5 agree that a demoted node refuses forwarded statements with `KS001` and has no local group; read from the headers of `failover.c` and `049`, not from the whole code, because 0.5 is not finished | the 0.5 tests that exist (`049` case B) and `064` |

## Implementation notes and deviations

Added when phase 2b is implemented, in the form of the notes of the earlier
phases.

### Step 1 (compatibility, bucket lookup, harness)

Found when the design was checked against the code as it was on 2026-11-17,
after version 0.5 was finished; where they differ the code was followed.

1. **Test numbers (section 14, 16.2).** The highest TAP file is `097`, so the
   names `055` to `069` of this document are all taken by tests of 0.5. Step 1
   uses `098_foundation` (part A of the design's `055_fragment_foundation`) and
   `099_harness_selftest`; later tests start at `100`. Step 1 also adds one
   regression test that the design does not list, `bucket_lookup`.
2. **Build files.** The `Makefile` has no list of TAP tests (`t/*.pl` are
   run); only `meson.build` lists them. `REGRESS` is in both.
3. **Where buckets are walked (4.7).** `kingres_bucket_for_*` do not loop: they
   call `kingres_shard_bucket`. `catalog.c` loops over the buckets for the
   placement number, and `planner.c` read `bucket_group[bucket]` directly in
   two more places; all use the new accessors now. `bucket_count` is also read
   in `bucket_move.c` (the `MODULUS` of `internal_attach_bucket`), so the files
   that may read it are `catalog`, `hashing`, `dist_table` and `bucket_move`;
   `098_foundation` checks this. `fanout.c` and `dist_table.c` needed no change.
4. **Names in `pg_compat.h` (2.3).** The code already had
   `ks_onconflict_action_shippable(action)`; `KS_ONCONFLICT_IS_SELECT` stands
   next to it. `ks_result_adopt` returns the new pointer: on 19
   `libpqsrv_PGresultSetParent` returns a new wrapper and the old pointer must
   not be used. `KS_SET_PLAN_ORIGIN` takes `(pstmt, origin)`, with
   `KS_PLAN_ORIGIN_UNKNOWN`. The settings of a partial plan are
   `KS_PARTIAL_PLAN_GUCS`, pairs of name and value:
   `max_parallel_workers_per_gather = 0`, `enable_partitionwise_aggregate =
   off`, and on 19 `enable_eager_aggregate = off`.
5. **Plan profiles of the harness (16.2).** Section 13 has no setting for
   `nopruning`, so a test passes its own settings. A setting of a profile is
   applied only on a node that has it, so until step 6 the profile `scan` is
   the defaults (`missing_profile_gucs` lists what is absent).
   `compare_tables` takes options. `compare()` prints a NULL as `<NULL>`;
   before, a NULL and an empty string compared equal (no existing test
   depended on it).
6. **Section 14 and "Sources read".** `kingres_shard--0.5.sql` and the script
   from 0.4 are committed, `default_version` is `0.5`, and the tests of 0.5
   are `t/048` to `t/097`.
7. **Verified as the design says.** `enable_eager_aggregate` is on by default
   on 19 and `enable_partitionwise_aggregate` is off on both; on 19
   `PLAN_STMT_UNKNOWN` is 0; `PlannedStmt` has `subrtinfos`, `rowMarkRelids`,
   `elidedNodes` and `extension_state`; `OnConflictExpr.lockStrength` exists;
   the result wrapper is made in `CurrentMemoryContext`; `remote_scan.c`
   builds a `PlannedStmt` by hand; a node that is not the primary of its group
   has `local_group_id = -2`.

Of the compatibility definitions only `KS_PARTIAL_PLAN_GUCS` is exercised at
run time in this step (its names are checked against the server); the others
are compiled on both majors and first used by later steps.

### What steps 2 and 3 will find changed

- `type_ok` already excludes `money` (`shippable.c`, `t/055_ship_limits.pl`).
- `dist_scan.c` already has the fault points `scan_begin`, `scan_started`,
  `scan_after_placement`, `scan_after_declare` and `scan_after_check`, which
  overlap the `frag_*` points of this document; `fault.c` has no table of
  points, only the names at the call sites.
- `kingres_shard_ensure_fresh_partitions` and `check_partitions_in_snapshot`
  are in `bucket_move.c`, which the file list of step 2 omits.
- `node_conn.c` does much more than this document assumes: the scope of a
  control call, the protocol for a statement that is in flight or pending
  (`kingres_shard_conn_set_pending`, `kingres_shard_conn_quiesce`,
  `kingres_shard_conn_abandon_pending`, `kingres_shard_conn_drop_inflight`),
  the forced wait for a participant's synchronous standby,
  `kingres_shard_begin_write_xact` and `kingres_shard_note_remote_write`, the
  note of a lost session that makes `COMMIT` fail, and
  `kingres_shard_remote_extension_version`, which the version probe of step 2
  can reuse. The batch sender of step 3 and the drain of a fragment must
  register with the pending protocol, and a batch that writes must go through
  `kingres_shard_begin_write_xact` and `kingres_shard_note_remote_write`.

### Step 2 (the placement check, pruning, the local part, reads with a local table)

Where the code differs from sections 4, 5.1, 6.5 and 17:

- **Fault points keep the names `scan_*`.** The `frag_*` names belong to the
  fragment scan of step 6. The 2a scan has no counterpart for
  `frag_local_open` and `frag_first_fetch`: its local part is a child path of
  the planner and not an SPI cursor.
- **Pruning has files of its own**, `prune.c` and `prune.h`, which the file
  list of step 2 omits; later steps share them. `bucket_move.c` is not
  touched, `kingres_shard_ensure_fresh_partitions` is only called.
- **A pruned group needs no probe and no connection.** Two regression
  expectations changed for it: `bucket_lookup` shows "Remote groups: 0 of 1
  (pruned by id)", and in `routing_decisions` reads through an SQL function
  and a view on a local key, against a node that cannot be reached, answer
  where they failed with "could not connect".
- **The version in the probe** is compared with the version compiled into the
  library (`KINGRES_SHARD_EXT_VERSION`), as the error reports already do, and
  not with a read of `pg_extension` on the origin. A node of another version
  is an ordinary error (`55000`) that names both versions, not `KS001`.
- **The origin is recomputed from the catalog** (`placement_hash_now`) after
  the cursors are declared. The 2a scan compared the cache with itself. The
  check of the plan number (4.3 step 1) therefore only fails earlier: no test
  tells it from its absence, and a stale cached plan cannot be made on
  purpose.
- **The nonce** is 64 bits from `pg_strong_random`, written as a comment
  prefix of the SPI text of the local part and held only while that call
  runs. The planner looks for it after the analysis and after the loop that
  refreshes the partitions, before the branch of a write to a reference
  table; the branch of referential-integrity actions stays first with its own
  loop. A test shows that a forged comment does not make a statement local;
  nothing can inject a nested statement with the same text while the local
  part runs, so the ordering against a change of the partition list between
  the two plannings is argued from the source order and not tested.
- **The window after the cursors are declared** cannot be played with a real
  move: the cursors hold the parent in ACCESS SHARE and the detach waits for
  it (10.2). It stays covered by the fault points of `t/031`; the windows
  after the first probe are tested with a real move.
- **`type_ok` already excluded `money`** (the notes of step 1); step 2 added
  only the tests at the level of the scan.
- **Test files** are `t/105_fragment_foundation.pl` (the parts B to F that
  the design calls `055`) and `t/106_scan_failover.pl`, because the numbers 055
  to 069 were taken.

### What step 3 will find changed

- `placement_check.c` holds the plan number, the probes, the check of the
  origin, the rule of one node of a group per transaction
  (`kingres_shard_group_rule`) and `kingres_shard_fragment_error`. The batch
  sender and the row router call the group rule before they use a connection
  and send every error of a node through `kingres_shard_fragment_error`. The
  group rule is skipped inside a control call.
- A probe takes the relation ids of its tables and sorts them by OID.
- `kingres_shard_in_fanout_local` is gone. The local part of the router's SPI
  source is planned as a statement of the top level and needs no nonce; a
  batch that wants to be recognised as a local part uses the prefix of
  `fanout.c` (`KS_LOCAL_PART_PREFIX`, private to that file today).
- `prune.c` gives `kingres_shard_prune_terms` and `kingres_shard_prune_groups`
  to the fragment scan of step 6.
- The line of the scan in EXPLAIN has the pruned form when a group was left
  out.
- The fix round for the fifth review of the call sequences (S13) changes
  `node_conn.c` after this step: `RELEASE SAVEPOINT`, `ROLLBACK TO` and the
  `COMMIT` of a session that carried the work of a control call are bounded.

### Steps 3 and 4 (the batch sender, the row router for INSERT, COPY FROM), and the fixes for two reviews

Built together with the fixes for the review of steps 1 and 2
(`reviews/2026-11-18-kingres-shard-phase2b-steps-1-2.md`, P2B-01 to P2B-09)
and for the sixth review of the call sequences
(`reviews/2026-11-18-kingres-shard-0.5-sixth-sequences.md`, S14). What differs
from the text above:

**The local part is recognised by a flag, not by a mark in the text (replaces
the nonce of 4.4).** The review showed that the text of the statement that
SPI runs is part of the error context, so SQL code that the statement itself
runs (a CHECK function of the table, a default, a domain) could read the
nonce and plan nested statements without routing. The caller now arms a
one-shot flag right before the SPI call (the local part of the fan-out, and
the local batch of the router), the first call of the planner hook of that
call consumes it (SPI plans before it executes, so no nested statement can
come first), and it is disarmed when the call ends. The statement text carries
no mark. The guard against a write of a reference table by such a plan is
unchanged and stays in front.

**The marker of an insert fragment** is `/* kingres:insert-fragment place=<n>
[ident=<n>] */`, not `kingres:frag v=2 kind=insert`: step 2 did not build the
v=2 marker machinery. It is accepted only in a forwarded session. A statement
that carries a fragment marker and that no rule accepts is refused with
`0A000` ("this node does not accept the fragment that another node sent it"),
not with `KS001`, whose advice to repeat cannot help.

**The batch specification** carries the deparsed `ON CONFLICT` and `RETURNING`
texts with a tuple descriptor and a tuplestore, not a list of expressions;
`kingres_shard_batch_finish` returns the row count and
`kingres_shard_batch_end` was added. The key attribute is counted among the
columns that are sent. A caller of `add` has the transmission modes in force.

**The wire statement** uses an unqualified `unnest(...)` of several arguments
(only that form is recognised in FROM), writes type names with their type
modifier (without it `bpchar` reads as `character(1)` and cut values), and
casts every source column to the type of its column, so the router always
hashes a value of the type of the column. A column the statement does not name
whose default is a constant is left out; the key and identity columns are
always sent. The output function of every value runs in a short-lived context.

**A batch in flight is cancelled, not waited for, when the statement ends.**
The cancel is bounded by about a third of `node_timeout`; a node that does not
answer in that time loses its session and the transaction cannot commit.

**The placement check is per statement**: one probe before the first batch and
one after the last, never per batch. A bucket cannot move into or out of a
group the statement wrote while it runs: the statement holds ROW EXCLUSIVE on
the table of every group it wrote, and the switch of a move waits for it (the
move gives up after its attempts). So the case of 10.2, "a bucket that moves
into a group the statement wrote", does not disturb the statement, the moved
bucket carries the rows, and the probe after the last batch is a second line
that fails fast. The check of a statement reads only its own colocation group,
through the primary key of the bucket map.

**Refused, each with a reason and a test**: `ON CONFLICT DO UPDATE` (and `DO
SELECT` on 19); a `WITH` clause on the INSERT; an insert whose rewritten
statement has check options (a view WITH CHECK OPTION, a row-level security
policy: the groups do not know the view); a security barrier view or a view of
another owner in the source; `RETURNING` with OLD or NEW; an inference clause
with an explicit collation or operator class; `session_replication_role`
other than origin. A single row with a constant key is recognised only when
it folds to a constant. With `enable_bulk_routing` off the 2a refusals are
returned word for word.

**COPY FROM** is handled in the utility hook and does not call `DoCopy`; it
repeats the checks of `DoCopy`, verified against PostgreSQL 18 and 19 (the
roles for a server file and a program, the permission check with column
privileges, a read-only transaction, parallel mode), and refuses any table
with row-level security enabled, which is stricter. `FREEZE`, `WHERE`,
`ON_ERROR`, `REJECT_LIMIT` and `LOG_VERBOSITY` are refused by name; the formats
and the other options are passed to `BeginCopyFrom`. Rows are converted to
text once per chunk (at most 1000 rows or `insert_batch_bytes`). A forwarded
session, a reference table and a bucket partition still refuse COPY FROM, and
`pg_stat_progress_copy` is not updated.

**EXPLAIN** shows the target, the batch rows, the conflict action and the
source of a row router, not a remote statement.

**Fault points** beyond the five of the design: `bulk_before_probe`,
`bulk_before_local_batch`, `bulk_before_send`, `bulk_after_send`,
`bulk_before_wait`, `bulk_after_last_batch`, `bulk_before_recheck`.

**Tests** take the next free numbers: `t/112` (the 056 router part and the 058
insert part of the design), `t/113` (064 router part), `t/114` (066 router
part and 067 batch and log part), `t/115` (057 and 058 copy part), `t/116`
(064 moves and failover), `t/117` (memory), the regression test `bulk_plans`;
`t/118` holds the tests of the fixes for the review of steps 1 and 2, and
`t/119` the nested subtransactions of the sixth review of the call sequences.
The older tests that asserted a refusal these steps remove assert the new
behaviour and, with the switch off, the old refusal.

**From the reviews, in the scan and the connection code**: a restriction that
deparses with `COLLATE` is accepted by the receiving group when its collation
passes the collation rule; the count of range-table entries that detects an
expanded SQL-language function applies only when the statement calls one (a
set operation with an arm that contradicts itself was refused by it); the
terms of a pruned scan are evaluated in a context that is deleted at once and
the text of the cursor statement is freed at every start; `RELEASE SAVEPOINT`
is sent before the subtransaction commits, no callback of a subtransaction
raises an error, and a broken session is brought to a depth that every level
above it skips.

**Open after this round**: the names of nodes in the older messages (S14-07);
no test for the cut of the connect and of the first command to
`control_call_timeout` (the raw sessions of the control calls stay outside
that limit, as the README states); three test points of P2B-05 (a probe of two
tables in another order, a standby that is the only registered node of its
group, a stale cache at the origin).

### What step 5 will find changed

- `kingres_shard_plan_row_router` is called from the planner hook after the
  check of an UPDATE of the key and before the first reference is looked at;
  the colocated `INSERT ... SELECT` (P6) can use it as its fallback.
- The refusal of `DO UPDATE` says that a colocated `INSERT ... SELECT` is
  shipped whole and allows it; step 5 makes that true or changes the words,
  and the expected output of `bulk_plans` with it.
- For the local part of P6: `kingres_shard_local_part_take` and
  `kingres_shard_spi_local_part`. Also to reuse:
  `kingres_shard_conn_send_params`, `kingres_shard_conn_cancel_and_drop`,
  `kingres_shard_coloc_copy`, `KS_INSERT_MARKER_PREFIX`, `is_insert_fragment`.
- A fragment of a new kind needs its acceptance rule on the receiving side;
  what no rule accepts is `0A000`. The receiving side decides on text that was
  deparsed and parsed again, so a rule is tested by that round trip
  (`t/118` has the pattern).

### Version 0.6 was made before steps 5 to 12

Section 14 and row 13 of section 17 put the version bump last. It was made
after steps 3 and 4 instead: phase 9 was started beside the remaining steps of
this phase and builds its catalog on 0.6 (as 0.7; phase 10 is then 0.8 and
phase 11 is 0.9, each one higher than its own document says). No release
exists between the two points, so the order matters to no user.
`kingres_shard--0.5--0.6.sql` exists and `default_version` is 0.6, while the
later code steps of this phase still change only C files, tests and the
README.

One correction to section 14, which says that the script holds the standard
guard and a comment only: it also replaces
`kingres.internal_check_node_versions()`, because that function names the
required version in its body and in its message, and a fresh 0.6 would
otherwise refuse every bucket move. No SQL object is added, dropped or changed
in its signature; an installation upgraded from 0.1, 0.2, 0.3, 0.4 or 0.5 has
the same catalog as a fresh 0.6, which the `upgrade` regression test compares.

For a test run of both majors: the test of a cluster of two majors
(`t/046`) uses the extension as it is installed for the other major, so after
a change of the version both installations must hold the new version before
that test can pass.

### Step 5 (the analysis module and the colocated INSERT ... SELECT)

**The marker and the rule on the group.** The marker is
`/* kingres:insert-select-fragment place=<n> */`; the v=2 marker machinery
still does not exist. A group accepts the statement only in a forwarded
session and runs the same analysis function on the parse of the text it
received. A forged fragment is `0A000`, a stale `place` is `KS001`.

**The origin parses its own text.** After it deparsed the statement, the
origin parses and rewrites the text under the transmission modes and runs the
function of the group on it. A statement the origin ships is therefore never
refused by a group for its form; when the two disagree the statement goes to
the row router. This costs one more parse analysis for a colocated
`INSERT ... SELECT`.

**Execution is the fan-out of 2a.** The groups run one after another in the
order of the groups, not at once. The plan number is checked before the first
group runs, `place=` by each group after it planned, and the placement of
every group after the last one ran. A signature of every relation of the plan
(relation and colocation group) is taken at planning and checked again when
the plan starts. Errors of a group go through `kingres_shard_fragment_error`.
No group is pruned: every group of the target runs the fragment, also when a
restriction names one bucket.

**Columns the statement does not name.** The origin leaves a default out of
the text, and the group applies its own, only when the default has no
variable, is immutable and is shippable. A serial, identity, volatile or
stable default (`now()`) sends the statement to the row router, which computes
it once per row at the origin.

**Column order and literals.** The target list of the INSERT is put in the
order of the source when every column comes from a variable of the source. A
literal of the select list that has no type of its own (a quoted literal,
NULL) is accepted only when the order is the one of the source already.
Whole-row references are refused, except the one the parser adds for
`EXCLUDED`; the walker no longer walks that list, which made a `money` or enum
column anywhere in the target refuse `DO UPDATE`.

**Rule 6 of 5.1 was missing in the code and is there now, for every mode of
the walker.** A constant of a `reg*` type whose object is not built in (for
example `'t'::regclass`) is not shipped. A restriction of a 2a scan with such
a constant is now evaluated at the origin. This is a correction and has no
switch.

**The check of the unique indexes of the target (11.7, condition 1)** applies
to every statement of this path, not only to `DO UPDATE`, and compares the
collation of the distribution column in each unique index too. PostgreSQL
already refuses a unique index of a partitioned table that lacks the
partition key, so the check can only meet a catalog edited by hand.

**Refused beside the design's list, each going to the row router or to its
refusal**: a target with a trigger or a rule of a user; row-level security on
the target or on a source; a target that is a view, check options, or the
rights of another role; `DO SELECT` (on 19).

**Switches.** `enable_colocated_insert_select` does not depend on
`enable_bulk_routing`; `enable_pushdown` does not exist yet. A cached plan
keeps the path it was made with, as with every planner setting, and its
results stay right.

**A reload of the catalog cache during planning.** Parse analysis can reload
the cache and leave pointers into it dangling. Planning compares the
generation of the cache before and after and plans again when it changed, at
most three times.

**Tests** take `t/120` to `t/123` (the design's `056` P6 part, `058`, `064`)
and the regression tests `pushdown_shippable` and `pushdown_plans`. The
generator of `t/123` writes colocated statements, which must be shipped, and
almost colocated ones, which must not, and compares outcome, `RETURNING` rows,
error text and table with a single node.

**Open after this step.** Two branches of the move tests always took the
branch without an error (a move while the statement waits at the last check;
a prepared plan across a move), so their `KS001` branches are not reached by a
test.

### What step 6 will find changed

- `pushdown.h` gives, for one SELECT level and with no dependence on the
  INSERT case: `kingres_shard_pushdown_analyze` (the eligibility rules 2 to 6,
  the join rules of 9.1 with outer and FULL joins, the key class of 6.3; it
  does not check rule 1 (switches, level, session) nor rule 8, and the caller
  runs the walker on the whole statement), `kingres_shard_pushdown_is_key_var`
  (it looks through join aliases, grouping columns and relabels and refuses a
  key from the nullable side of an outer join),
  `kingres_shard_pushdown_closed` (6.4: aggregates, DISTINCT, DISTINCT ON,
  window partitions) and `kingres_shard_pushdown_relids`.
- The private list of a fan-out plan has two more members, the kind and the
  relation signatures.
- `fanout_targets()` in `planner.c` is shared by this path and the fan-out
  write.
- For this path "closed" follows from the rule that the key is bare, so only
  the reason line tests it; step 6 needs tests of its own for it.
- The reg-constant rule above changes what a 2a scan ships.

### Step 6 (the fragment scan and closed queries), with the fixes for the review of steps 3 and 4

**Where the path sits.** One path at the final upper relation, not at the grouping one: the fragment
returns exactly the final target of the query. The origin sorts and limits with its own Sort and
Limit over the scan; the groups get the query without ORDER BY and LIMIT, and an ORDER BY expression
that is not selected travels as an ordinary column (a DISTINCT query with such a column is not
shipped). Because ORDER BY and LIMIT stay at the origin, an ordered or limited query that the plan
of before answers cheaply is not shipped; the ordered merge is step 9.

**The rule "gain".** A closed query is shipped only when it has an aggregate, a GROUP BY or
DISTINCT, or two or more distributed tables. One table without grouping stays on the scan of 2a,
since the groups would return the same rows. `EXPLAIN VERBOSE` of the scan says why
("Pushdown: not used: ..."), when both switches are on.

**The marker and the rule of the group.** `/* kingres:query-fragment place=<n> */`, a plain comment;
the v=2 marker with `kind`, `ident` and `ext` does not exist. A group accepts the fragment only in a
forwarded session and runs the same function as the origin (analysis, "closed", no LIMIT, the
shippability walker) on the text it parsed again; otherwise `0A000`, and a stale `place` is `KS001`.
The origin parses its own deparsed text and runs that function before it ships.

**What is not relaxed.** Window functions are not shipped whatever their partition says (6.4 would
let a window partitioned by the key be closed); DISTINCT ON, float and order-sensitive aggregates,
stable functions and an ORDER BY inside an aggregate are refused by the walker as before.

**Cost.** `100 * remote groups + (input total - startup) / groups + rows * transfer * remote
fraction`; a tie goes to the scan. It is a first-order model: for a selective grouping by a unique
key and for DISTINCT on a unique key the fragment is chosen and is slower by 0.05 to 0.08 s on the
test data; for joins and grouped joins on the key it is 20 to 30 percent faster.

**Pruning** uses the restrictions of every distributed table of the statement and intersects the
groups they allow.

**The part of the origin's own group** runs through an SPI cursor planned under the one-shot mark;
a cursor plan that contains a scan of the extension is refused with `KS001`. The partitions of the
origin are identified before and after, and its placement is computed again.

**`EXPLAIN`** shows `Pushed`, `Groups` and `Remote SQL`; `EXPLAIN ANALYZE` adds the rows per group.
The time to the first batch (section 12) is not shown.

**A statement that ends while a FETCH is in flight** (an error, a cancel, the abort of a
subtransaction) asks the group to stop it, bounded; before, an abort behind a group that waited for
a lock took up to `node_timeout`.

**The move between the probes and the cursors** cannot be played with a real move, since the cursors
hold locks that the move waits for. Fault points exist at every gap (`frag_before_probe`,
`frag_after_probe_before`, `frag_after_declare`, `frag_local_open`, `frag_after_probe_after`,
`frag_first_fetch`, `frag_before_fetch`, `frag_wait_batch`, `frag_local_fetch`,
`frag_declared_g<n>`), and three checks of the start survive their mutants for that reason (the
plan's placement, the identity of the origin's partitions after the open, the probes after the
DECLAREs), with the check of the relation signatures.

**What moved.** The start, fetch, collect, drain and stop of a scan over the groups, the choice of
groups with pruning and the renumbering of parameters are in `fragment_scan.c` and serve both
scans; `dist_scan.c` keeps the path hook, the child plan, the conversion of rows and `EXPLAIN`.

**Switches.** `kingres_shard.enable_pushdown` and `enable_pushdown_closed`.

**The fixes for the review of steps 3 and 4.** P2C-01: the source of the row router is coerced with
the type modifier of the column, and the length coercion of an assignment is kept visible in the
deparsed text, so the origin hashes the value the table stores, and an over-length `varchar` or an
over-precision `numeric` fails with the message of a single node. P2C-02: the source cursor is
fetched in steps that follow the widest row seen (16 rows first, then the batch bytes divided by the
widest row, at least one). P2C-04 and P2C-07 in part: tests. P2C-03, 08, 09, 11: the README. The
defect of a routed `GENERATED ALWAYS` identity column could not be reproduced on this base; a test
(`t/130`) covers every case against a single node, and its mutant reproduces the reported error.

**Open after this step.** No test for P2C-05 (a function that replays the text of the local part)
and P2C-06 (S14-04); the reset of `xact_depth` in the broken-session branch is neither tested nor
removed; the refusal of COPY with row-level security has no test. A hazard that is older than this
step: the one-shot mark is consumed by the first planning; when SPI plans the local part of a
fan-out write or of a router batch a second time (an invalidated plan), the second plan is an
ordinary one and would read the other groups too; only the fragment scan guards against it. The
cost model is not calibrated. Only one seed of the generator `t/127` was run.

### What step 7 will find changed

- `KsFragSet` and `kingres_shard_fragset_*` in `fragment_scan.h` are the shared machinery; a scan of
  partial aggregates can use them and changes only the marker and the part of the origin's group.
- `kingres_shard_query_fragment_analyze` in `pushdown.c` is the rule of the group, to be extended
  with a kind for partial aggregates.
- The paths are added by `fs_upper_paths` on `create_upper_paths_hook`; `KsPushdownInfo` is the
  hand-over from the planner hook, and its `kind` drives `EXPLAIN`.
- The Makefile does not track header dependencies: after a change of a header, build clean.


### Round after step 6 (the mark of the local part, the rest of the review of steps 3 and 4, leftovers of step 6)

**The second planning of the local part (design 4.4, step 2 and the notes of steps 3 and 4).** It cannot happen on
the paths that use the one-shot mark: SPI_execute_with_args (the local part of a fan-out write, the router's local
batch) makes a one-shot plan, which SPI plans once in _SPI_execute_plan with no revalidation, and
SPI_cursor_open_with_args (the local cursor of the fragment scan) builds its plan once in GetCachedPlan; the validity
of a plan is checked only when a plan is reused.  A concurrent ANALYZE that commits between the planning and the start
was played with the fault point local_part_planned (t/131): the planner hook ran once per statement and every answer was
the single node's.  As a guard the second planning of the armed text before the execution starts is refused with KS001
(fanout.c, kingres_shard_local_part_take; the window is closed by ExecutorStart, so a statement that SQL code runs
during the execution, a CHECK function that replays the text, is planned and routed as before: t/132).  The mark is
armed only by the C code around the SPI call and disarmed when the call ends; nothing in the text or in a setting
carries it.  debug_discard_caches = 1 crashes the backend by stack overflow in the catalog loader of the extension
(each lock acquisition resets the caches and the loader runs queries): not usable as a test, not a wrong answer.

**S14-04.** The mutant "a failed ROLLBACK TO never dooms the transaction" survives t/119 because
discard_connection() notes a session that wrote for the user as well: the second path makes the first one redundant
for a session that wrote.  The mutant that always dooms is killed (a session that only read must commit).  The
reset of xact_depth in the broken-session branch of the abort callback was removed: the callback of the next level up
sees xact_depth above its level and closes the session.

**COPY and row-level security (row_router.c)** is reachable only after row-level security is turned on by hand with
routing off; it is tested that way (t/133).  **The step 5 path** with keys that have a type modifier (timestamp(0),
numeric(8,2), varchar(8), a wider source modifier than the target's) gives the twin's rows (t/133).

**EXPLAIN ANALYZE** of a fragment scan shows "Time to the first batch" per group and for this node's own part
(with TIMING on): the time from the first FETCH sent to the first batch read.

**place=0 (review of step 5, P2D-05).** A group now compares the placement number of a closed-query fragment and of a
colocated INSERT ... SELECT whatever its value, so place=0 is a stale placement (KS001), not "no check".  The only
legitimate 0 is the insert fragment of an explicit destination that is not in the catalog (it carries ident instead),
whose comparison stays as it was.

**reg* values (P2D-01).** The walker now decides a reg* type from the server (a built-in type whose input function is
named reg<...>in, looked through domains, arrays, ranges and the fields of a row), and refuses a parameter of such a type
and a constant of an array, range or row type that holds one.  A scalar constant keeps rule 6 of 5.1.  An
expression the planner folds to plain OIDs (a regclass compared with an oid, array of regclass cast to oid[]) is shipped as
numbers and means the same number everywhere, as it does on a single node; the hazard is only a value that is turned
into a name.  The row router and the colocated INSERT ... SELECT send values through the same walker.  Not done:
a test with objects whose OIDs differ between nodes for the router and the 2a scan.

**FROM ONLY (P2D-03).** Refused on every path with "FROM ONLY is not supported for distributed table": a distributed
table is one table whose rows are in the partitions of every group.  A reference table is an ordinary table on each node
and ONLY works.

**The cost of a closed query that reduces nothing.** A fragment scan over one distributed table whose result has 90 percent or
more of the rows the table gives after its restrictions is not offered (no gain: a grouping or DISTINCT by a unique key).
The tests t/124 to t/127 therefore use tables that hold every key twice (or a primary key of (id, copy)), and the cases that
return as many rows as they read have no path assertion.

### Step 7 (partial aggregation), with the rest of the review of step 5 and the crash under cache discards (2026-11-20)

**The crash under debug_discard_caches (answer to the question of the coordinator).**  It is the extension, not PostgreSQL.  A bare
server (initdb, no preload) and a server that only preloads kingres_shard and has the extension created run SELECT, CREATE TABLE and
the catalog functions under debug_discard_caches = 1 without a crash.  The crash needs a cluster: a SIGSEGV handler
(preloaded library, backtrace + addr2line on the build with -g) showed

  1. kingres.add_node -> require_primaries_can_commit (node_admin.c) walked `cat->nodes` and connected to each node in the loop; the
     connection takes locks, a lock processes invalidations, the catalog cache of the extension was loaded again and freed, and the
     loop read the freed list;
  2. an INSERT of several rows -> planner_hook_body -> kingres_shard_plan_row_router -> kingres_shard_placement_hash(dt->coloc):
     `dt` was taken from the cache by the analysis, kingres_shard_placement_hash() calls kingres_shard_catalog() first, the cache was
     invalid at that moment (a lock processed an invalidation), it was loaded again and the old context freed, `coloc` was freed memory.

The earlier note (stack overflow in the loader) was wrong: the address of the fault is 0 and the loader is guarded against re-entry.
The fix is in the extension (commit ca00f81bd5): a replaced cache is retired and freed at the end of the transaction, require_primaries_can_commit
copies, the planner analyses again when the generation changed.  t/135 runs a whole cluster under the setting on both majors.  With the
retire reverted (mutant) the test is killed.

**P2D-02.**  The fault action `waitinv` (fault.c) is new: wait for the file, then AcceptInvalidationMessages().  Without it the origin
does not see the catalog change that the test made while the statement waited (the lock it holds on the relations is already
granted, so nothing processes the invalidation) and the "cache is fresh, the plan is old" state that the defences exist for cannot be
produced.  The test changes the catalog of the origin by hand (bucket_map, dist_table); the groups agree with the plan.  Real moves
cannot be used: the switch of a move waits for the locks of the statement.

**P2D-01, 2a.**  A statement that names one key is forwarded to its group as the text the user wrote, so the group resolves the
names itself (reg* values are names there).  t/137 first expected the origin's OID there and failed for two of three origins;
the meaning on that path is the name, and it is what the README now says.  The review's criterion (one meaning on the paths that
evaluate at the origin) holds on the scan, the fragment scan, the router and the shipped INSERT ... SELECT.

**C, the no-gain rule.**  The first version compared two estimates that do not agree (rows of the table: all groups; rows of the grouping:
statistics of the local partitions).  t/138 (grouping by the primary key of 8000 rows) showed it shipped.  The rule now asks the
catalog for a unique index within the grouping columns.

**Mutant tables are only valid with a clean build.**  The first runs of the mutant scripts (mut.sh, mut19.sh) synced the mutated copy
with rsync -a over a work area that kept the objects of the previous mutant; the mtimes of the restored sources were older than the
objects, so make kept the mutated object of an earlier mutant and every later mutant inherited it (M07 to M10 of the section 5 table were
"killed" by the M06 mutant that came before them).  The scripts now run `clean build` for every mutant and run the test files one
at a time, stopping at the first failure; every table in the report is from the second run.

**D, partial aggregation (design 7), what differs from the text of the design.**
- The partial target and the text are made in one place, fs_partial_paths (the hook at UPPERREL_GROUP_AGG), from the planner's own
  grouping target; pushdown_prepare only decides eligibility (shape and aggregates) and keeps a copy of the query (`ship`).  The design
  has the select list "replaced by the unmarked expressions of the partial target"; so does this, and a grouping column that is not in the
  partial target stays in GROUP BY as a resjunk entry of the deparsed query.
- The aggregate rule of the group is kingres_shard_partial_fragment_analyze (pushdown analysis, shape, no HAVING/ORDER BY/LIMIT, no
  aggregate inside an expression, the shippability walker with allow_order_aggs).  It checks the catalog itself (combine function,
  serialization pair, normal kind, built in), not only the planner flag, because a forged fragment must not rely on the sender's flag.
- DISTINCT after the aggregation, a window in the same level and grouping sets are refused by the shape rule (the scan answers).  The
  design allows a window above an aggregate (A6) at the origin; it is not offered yet.
- A Group node at the top of the plan (a grouping with no aggregate, sorted) is accepted by partialize_plan and left as it is.
- Float and order-sensitive aggregates are accepted for the partial path only (A3).  The closed path still refuses them (step 6, unchanged).
- The classification test is part of t/139 (section C2), not a pg_regress file: the harness already compares on both majors and the list is the same on 18.7 and 19rc2.
- enable_pushdown_closed no longer disables the partial path (pushdown_prepare is called when either switch is on and the closed branch
  honours its own switch).  t/124 to t/127 run with enable_partial_aggregation = off: what they call "not shipped" is the closed rule.
- 2a path and reg*: see above.

**A kingres_sketch aggregate (step 8, not built).**  What it needs is listed in the README ("What an aggregate of an extension needs").

**Measurement after the no-gain rule asks for a unique key** (3 million rows, PostgreSQL 18.7,
three runs each): grouping by the key of 3 million rows 27.4 s by the scan, which is chosen;
of 30 000 rows 0.35 s against 0.34 s, the scan chosen; a join on the key with a filter 0.27 s by
the scan against 0.20 s as a fragment; a grouped join 1.95 s against 1.65 s. The loss of 25 percent
on a grouping by the primary key is gone.

**Mutants.** Of thirty mutants of the rules of section 5, five are killed by the tests of reads
(t/124 to t/127); of the rest the rules of inserts that a read cannot reach are killed by t/136
(M07, M09, M10, M27) and the regression tests (M04, M12), and two (M02, M20) are made redundant by
the walker of shippability for reads. Of eleven mutants of partial aggregation eight are killed and
three are equivalent (a check behind which another branch also refuses, a switch the hook checks
again, a sorted path that remains when the hashed one is removed).

**What the full run found after step 7.** `min()` and `max()` of an indexed column without
`GROUP BY` are answered by an index probe with no aggregation node; the partial plan accepts a
result with no left tree when every aggregate has no final function and a state of the result
type. A join of a table with itself was never shipped (the rule counted tables, not references).
The test harness writes `enable_partial_aggregation = off`, so that the older tests keep testing
the scan, the fragment scan and closed queries; t/139 turns it on.

### Steps 8 to 12 (objects of extensions, sort and limit, join rows, the README and the security round) (2026-11-20)

**Step 8, objects of extensions (5.5).** The compiled-in list (`kingres_sketch` 1.0), the object
rules and the setting `kingres_shard.shippable_extensions` (all, none or a list; it only narrows)
are as section 5.5 says. What differs:

- An object of an extension is shipped only by a closed query and by a partial aggregation. The 2a
  scan, the writes and the colocated `INSERT ... SELECT` never ship one, because their markers carry
  no version. A sketch aggregate in an `INSERT ... SELECT` is therefore run by the row router at the
  origin.
- The marker is `/* kingres:<kind>-fragment place=<n> ext=name:version[,...] */` for the kinds
  `query` and `partial`, not the `kingres:frag v=2 kind=...` of section 5.5; the markers of this
  implementation never had that form. The characters of the list are checked.
- A group checks the versions first: a different version, or an extension that the marker names and
  the group does not have, raises KS001 with both versions. A statement that uses an extension the
  marker does not name, or the reverse, raises 0A000. A group that does not have the extension at
  all fails earlier, when it parses the text ("function does not exist"), not with KS001; a hook
  between the marker and the parse would be needed for that.
- `self_test()` and the encoding are not compared here; section 5.5 leaves them to `add_node` and
  `check_cluster`.

**The survivors of the review of step 5.** t/152 reads with a FULL JOIN on and off the key, with a
reference table and with three tables, windows over closed groupings, sub-selects and views. The
mutants M18, M18b, M20, M02 and M03 survive it, each shielded by another rule that decides the same
statements (the count of key classes, the shippability walker, the analysis of the range table, the
type of the key columns). No statement separates them, so the rules stay as a second layer that is
not tested alone.

**Step 9, sort and limit (8.1, 8.2).**

- A closed query with `ORDER BY` has the groups sort; the fragment scan merges the sorted streams
  with one SortSupport per key, and no Sort is added above it (`EXPLAIN` shows the merge keys).
  Only a built-in B-tree operator with the default collation or "C" is pushed; otherwise this node
  sorts as before. A partial aggregation pushes no sort; it sorts above the final aggregate.
- The limit goes to the groups as `LIMIT count + offset` with no `OFFSET`, never with `WITH TIES`.
  A constant is folded first (the parser leaves the cast of a literal as a call). A parameter, or an
  expression of parameters and immutable or stable functions with no column, sub-select or volatile
  function, is computed when the scan starts and sent as one extra `bigint` parameter; the local part
  gets the same value. A NULL, a negative value or an overflow sends NULL, and the origin's own Limit
  node raises the single node's error, so no group raises it. A group accepts a limit only as one
  non-negative `bigint` constant or one `bigint` parameter.
- Ties are returned as this node's own part first, then the groups in catalog order, not by group id
  (O3); SQL does not order ties.
- **A difference from a single node:** a cursor declared with neither `SCROLL` nor `NO SCROLL` over a
  query whose groups sort cannot scan backward, because the merge cannot; `DECLARE ... SCROLL` can.
  The README names it, and t/155 tests it.
- Setting: `kingres_shard.enable_pushdown_sort` (on).

**Window and `DISTINCT` above a partial aggregation (7.6, A4 and A6).** The text of the groups has
neither (the named-window clause is cleared too, because the deparser prints it); the origin applies
them above the final aggregate. A group that is sent one still refuses it.

**Step 10, join rows (9.1).** A statement that is not closed as a whole (a window not partitioned
by the key, an ordered-set aggregate, `count(DISTINCT)`, `DISTINCT ON`) but whose tables join on
their keys gets the join of the whole level as a fragment of its own, through a chained
`set_join_pathlist_hook`. The text is the statement's `FROM` and `WHERE` with only the columns of
the join relation's target; it is itself a closed query, so a group needs no new rule. A closed
statement keeps its own path, a partial aggregation keeps its path, and the planner chooses by cost.
Setting: `kingres_shard.enable_pushdown_join` (on). Not done, as section 9 says: semi and anti joins,
lateral references, a join below the top of the statement.

**Step 11, the README.** Guarantees: pushdown gives the single node's answer and the scan is always
the fallback; a group enforces its own rules whoever sends the fragment. Limitations: a new item
names the differences that pushdown makes visible, among them the cursor above.

**Step 12, the security round** (t/156): forged fragments (crossed kinds, a wrong place, forged
text, a wrong forward token), privileges and row-level security on every path, `SECURITY DEFINER`,
`search_path`, hostile identifiers and values, and the version marker of an extension.

**Step 13, the version: correction.** This document says in several places (the header, the plan of
steps, the upgrade test and "Version 0.6 was made before steps 5 to 12") that the steps of phase 2b
end in version 0.6 with `kingres_shard--0.5--0.6.sql`. Version 0.6 was made, before step 5, and
phase 9 then took 0.7. Steps 6 to 12 add no SQL object, only settings of the library, so they ship
with the library of 0.7 and step 13 makes no version of its own. The four settings are in the
catalog regression test, and the upgrade test from 0.6 compares the upgraded catalog with a fresh 0.7.

**Tests and mutants.** t/151 (step 8), t/152 (the survivors), t/153 (step 9), t/154 (step 10), t/155
(the limit by parameter, window and `DISTINCT` above a partial aggregation, the cursor) and t/156
(step 12). The harness writes `enable_pushdown_join = off` beside `enable_partial_aggregation = off`,
so the older tests keep testing their own paths. Of the mutants of steps 8 to 12, each built clean,
all are killed except these equivalents: the "none" branch of `shippable_extensions` (the list parser
refuses the name anyway), the rule that an extension function must be immutable (`kingres_sketch` has
no other), the planner's collation check of the sort (the walker refuses that collation first), the
second parse of the text of join rows, the test of the whole level of join rows (an intermediate join
is never the cheaper path on the test data), and a negative limit sent as a number (the group would
raise the same error text as the origin).

KS001 for an extension that a group does not have at all, the tie-break by group id and the tests
063, 064, 066, 067 and 068 of the plan were left for step 13; they are described in its section below.

### The review of steps 6 to 12 and the fix for its critical finding (2026-11-20)

The review ([report](../reviews/2026-11-20-kingres-shard-phase2b-steps-6-12.md)) compared about
13 000 statements with a single node on both majors and found one critical finding, P2E-01: a
`HAVING` condition with no aggregate was applied by nobody. The planner moves such a conjunct (no
aggregate, no volatile function, no sub-select) from `HAVING` into `WHERE`, but the text of a partial
aggregation and of join rows is made from the statement as written, with its `HAVING` removed and
its `WHERE` unchanged. `SELECT c, count(*) FROM m GROUP BY c HAVING c < 3` returned every group; the
same flaw was found in join rows. Both paths are now refused for such a statement, and the scan
answers (`having_has_moved_conjunct` in `fragment_scan.c` applies the planner's rule, or a wider
one). What stays in `HAVING` (an aggregate, a volatile function, a sub-select) keeps the partial
path and is applied by this node's final aggregation. A closed query keeps its `HAVING` in its own
text and was right before. t/170 compares every form of `HAVING` under the three plan profiles on
every path, and the generator of t/127 has three such forms.

The alternative of putting the moved conjuncts into the shipped `WHERE` was not taken: the `HAVING`
of the statement is written in the variables of the grouping range table entry (18 and 19), which the
planner has flattened, and a text with them in `WHERE` would need `flatten_group_exprs` on both
majors. The cost is that `HAVING <grouping column> ...` is answered by the scan, a difference of speed
only; a user can write the condition in `WHERE`. The four mutants of the fix are killed by t/170.

The other findings, one commit each:

- **P2E-02, `TABLESAMPLE`.** The scan asked the groups for every row with no sampling. A sample
  cannot be taken correctly on the groups: a table spread over groups would be a different random
  draw from any single node's, and `REPEATABLE` would not repeat. `TABLESAMPLE` of a distributed table
  is now refused with 0A000 at any level of a statement (a sub-select, a CTE, the source of an
  `INSERT` or `DELETE`), with pushdown on or off and in a prepared statement. A reference table and a
  local table are sampled as on a single node. t/171; t/121 now expects the error.
- **P2E-03, the second profile of the tests.** The harness turned the partial aggregation and join
  rows off in the older tests. With `KS_ALL_PATHS=1` every node of every test has both on after the
  test's own settings. t/124 to t/127, t/135 to t/138, t/152 and t/153 pass in that profile on both
  majors, among them the faults and moves of t/125 and the discarded caches of t/135. Their "not
  shipped" assertions become "not a closed query" in that profile (`not_shipped_ok()`).
- **P2E-04, rules that no test separated.** t/172 forges fragments: a group refuses a negative
  `LIMIT`, an `OFFSET`, `WITH TIES`, a sub-select or NULL as the limit, and `DISTINCT` or
  `DISTINCT ON` in a partial fragment. The sum of count and offset that does not fit in a `bigint` is
  tested with custom and generic plans from every node. Its five mutants are killed.
- **P2E-05 to P2E-07, the README.** It names the `NOLOGIN` limit, the exceptions of the guarantee,
  a view of another owner refused with an error on every path, `min` and `max` of values that compare
  equal and print differently, and `TABLESAMPLE`.
- **P2E-10.** The ordered merge frees its arrays at a rescan.
- **P2E-08 and P2E-09** need no change. The cursor difference is in the README and in t/155. The
  rules shielded by another rule stay as a second layer.

### Step 13: the remaining tests, a missing extension, the order of ties and a crash (2026-11-20)

**The tests of section 16.2.** Their numbers 055 to 069 were taken by tests of 0.5, so each one is a
new `t/1nn` file or an addition to a test of steps 6 to 12. New in this step: t/157 (063 for the
partial aggregation, the ordered merge, join rows and an extension object: a writer on the groups, a
bucket that moves back and forth, a move between the plan and the run, a held switch, prepared
statements, a session's own writes); t/158 (064 for the same paths: a planned switchover at four
fault points with a prepared statement, a hard failover in the middle of a fetch, a write through the
old primary); t/159 (066: the hostile names of 16.4 for schema, table, column, type, sequence and
constraint over seven shapes of statement); t/160 (067: all seven kinds of fragment with no token,
with a wrong token and on a demoted node); t/161 (068: `kingres_ledger` with `kingres_shard`). The
source of an `INSERT ... SELECT` into a local table is planned as a subquery and read by the scan, not
by the pushdown paths (6.1, 18).

**A crash found by t/157.** A prepared statement with a sketch aggregate, executed several times on
the same cached session, crashed a group with SIGSEGV on both majors. The check of an extension's
aggregate ran from the analysis of the shape before the walk of the statement had reset the static list
of extensions used, so it read the list of the previous statement, whose memory was gone. It came in
with step 8. The check now starts its own empty list.

**KS001 for an extension that a group does not have.** A group cannot raise it: PostgreSQL analyses
the `DECLARE` of a fragment, and fails on the unknown function, before any hook of the extension, and
`emit_log_hook` cannot read the catalog in a transaction that is aborting. The node that sent the
fragment knows the extensions from the marker. When the `DECLARE` fails with `undefined_function` or
`undefined_object` and the marker names an extension, it raises KS001 ("node host:port does not have
an extension that the statement uses (kingres_sketch:1.0)") with the group's message as the detail
and `shippable_extensions = 'none'` as the escape. Another error, such as a division by zero, is not
changed. Known limit: another undefined function or type in a statement that also uses an extension
gets the same message, with the group's text as the detail.

**The order of ties (O3).** The streams of the merge are sorted by group id when it starts, so ties
come out in the same order from every node and on a repeat. Inside one group the order is the
group's own; a group that sorts with a top-N heap orders equal rows differently from a full sort.

**Not built, as section 18 says, and named in the README:** a join below the top of the statement
(6.1); semi and anti joins (9.1); grouping sets (6.1, 7.1, 7.6 A9); a `FULL JOIN` with a reference
table or without the key equality in its `ON` (9.1, 6.3; a `FULL JOIN` of two distributed tables on
the key is shipped); an aggregate without a combine function in a statement that is not closed (7.1,
7.6 A9); a limit with a column, a volatile function or a sub-select.

**A forwarded reference write without a term.** A demoted node runs a `reference-write` fragment
that carries no claim of a catalog term. A real sender always carries its term, and the demoted node
refuses an older one with KS002 (t/160). Refusing every such statement in the planner hook would break
the calls of the control plane to an old primary and the filling of a new node's reference tables.

**Tests and mutants.** t/157 to t/162. The eight mutants of this step (the conversion to KS001, the
sort of the streams, the crash line, a blind comparison of the merge, a demoted node that serves
fragments, a token that is not compared, a failed group that is not retryable, the local group read
twice) are killed. The tree with the fixes of the review of steps 6 to 12 merged passes on both
majors in both profiles.

**The review of step 13 (2026-11-20).** The review
([report](../reviews/2026-11-20-kingres-shard-phase2b-step-13.md)) found no wrong answer in 5120
generated statements per major before and after bucket moves, could not crash a group through the
fixed list by any path, and found no critical or high defect. Its three medium findings are being
fixed: a group that never had the extension raises `3F000` (its schema does not exist), which the
conversion to KS001 does not cover, while t/162 tested only a dropped extension, whose schema stays;
the test that another error is not converted fails at `FETCH`, not at `DECLARE`; and t/161 reads the
source through the scan, not the pushdown paths.
