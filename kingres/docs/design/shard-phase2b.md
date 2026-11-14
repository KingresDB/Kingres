<!-- SPDX-License-Identifier: PostgreSQL -->
# kingres_shard: design of phase 2b (pushdown, ordered merge, colocated joins, row routing)

Status: draft for review, 2026-11-14.
Builds on [shard-phase2a.md](shard-phase2a.md) (extension version 0.2), on
[shard-phase4.md](shard-phase4.md) (0.3, bucket moves) and on
[ha-phase5.md](ha-phase5.md) (0.4), and produces extension version **0.5** with
the upgrade script `kingres_shard--0.4--0.5.sql` (see "Version and merge
order").

Phase 2a made every statement correct or refused. Phase 2b makes the common
ones fast and lifts the refusals that remain on writes. It adds no catalog
table and no SQL object; it changes how statements are planned and executed.

How this note was checked. Claims about PostgreSQL are checked against the
18.7 source in this tree, with the file and function named. Claims about the
extension are checked against the code under `contrib/kingres_shard/src`. Where
the working tree differs from the last commit (the bucket-move work of phase 4
is in progress there), the files this note relied on are named in "Sources
read". Anything not checked is marked **[Needs verification]** with what would
settle it, and collected at the end.

## Sources read

- Documents: `shard-mvp.md`, `shard-phase2a.md` (including its implementation
  notes and the fix round), `shard-phase4.md`, `ha-phase5.md`, `ledger-mvp.md`
  and `ledger-0.2.md` (what a ledger table is), the reviews of `kingres_shard`
  0.1 and 0.2, `coding-standards.md`, `testing.md`, `documentation.md`, and the
  README of the extension.
- Extension, identical in the working tree and at the last commit:
  `src/planner.c`, `dist_scan.c`, `remote_scan.c`, `shippable.c`, `fanout.c`,
  `catalog.c/h`, `hashing.c/h`, `node_conn.h` and the parts of `node_conn.c`
  that begin a remote transaction and set the transmission modes.
- Extension, different in the working tree (phase 4 in progress): `utility.c`
  (read for the `COPY` rule), `kingres_shard.c`, `kingres_shard.h`,
  `node_conn.c/h`, `fault.c`, `node_admin.c`, `dist_table.c`, `Makefile`,
  `meson.build`, `kingres_shard.control` (default version 0.3), and the new
  `bucket_move.c/h`, `kingres_shard--0.3.sql`, `kingres_shard--0.2--0.3.sql`.
  From these only the names of the new GUCs, the description of the switch in
  the header of `bucket_move.c`, the fault-point naming (`move_<what>`) and the
  control file were used. Nothing in `planner.c`, `dist_scan.c`, `remote_scan.c`,
  `shippable.c`, `fanout.c` or `catalog.c` differs from the commit.
- PostgreSQL 18.7: `src/backend/optimizer/plan/planner.c`
  (`grouping_planner`, `create_ordinary_grouping_paths`,
  `create_partial_grouping_paths`, `make_partial_grouping_target`,
  `mark_partial_aggref`, `can_partial_agg`), `optimizer/prep/prepagg.c`
  (`preprocess_aggref`), `optimizer/plan/setrefs.c`
  (`convert_combining_aggrefs`, `add_rte_to_flat_rtable`,
  `set_customscan_references`), `optimizer/path/allpaths.c` and `joinpath.c`
  (the hook call sites), `executor/nodeAgg.c`, `executor/spi.c`
  (`SPI_cursor_open_internal`), `tcop/pquery.c` (`PortalStart`), `commands/copy.c`
  (`DoCopy`), `include/commands/copy.h`, `include/nodes/pathnodes.h`,
  `include/optimizer/*.h`, `commands/explain.c`, and
  `src/include/catalog/pg_aggregate.dat`.
- `contrib/postgres_fdw`: `postgres_fdw.c` (`foreign_join_ok`,
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
| The scan does not check the remote map version because "buckets do not move in this phase" (2a notes) | In, required | Phase 4 moves buckets; section 10 gives the check and the argument. |
| A global snapshot (2a "Not in phase 2a") | Out | Phase 8. Section 10 says what pushdown does and does not change. |
| Distributed deadlock detection (2a) | Out | Phase 5. |
| Row locking (`FOR UPDATE`, `FOR SHARE`) on a statement that spans groups (2a section 5) | Out | Needs a row identifier valid across nodes. Such a statement is refused as today. |
| System columns and `PlaceHolderVar`s computed on a distributed table (2a notes) | Out | Unchanged refusals. |
| `UPDATE ... FROM`, `DELETE ... USING`, `MERGE` that read a distributed table and write another table (review S2-18) | Out | Needs a row identifier of the read table; unchanged. |
| A reference-table write that reads a distributed table, or uses a volatile value shipped as a constant (2a section 3 "out of scope here: reject") | Out | The router could compute such rows once at the origin and ship them to every node, but then the whole stream needs the cluster-wide writer serialization of review S2-06 and a per-batch equality check of the copies. That is a separate increment. Still refused. |
| Views over distributed tables whose owner differs from the session role (S2-15) | Out | Unchanged. A view, a subquery, a CTE or a set operation is not a pushdown unit in 2b (section 6), so it uses the 2a scan. |
| Parallel query inside the origin's part of a scan, and parallel plans on the other groups | Out | Fragments run through cursors, which cannot be parallel; section 4.4. |
| Statement-level retry after a stale map for fragments (2a retries only a routed statement on a fresh remote transaction) | Out | A fragment that meets a changed map fails with `KS001`; the client retries. Section 10. |
| `enable_partitionwise_join`/`_aggregate` being switched off for statements planned with the scan (S2-20) | Out | Unchanged. |
| Generic plans cannot route a statement restricted by a parameter | Out | Unchanged. Fragment plans do work as generic plans (section 10.4). |

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

## 2. Mechanism in PostgreSQL 18 terms

### 2.1 Hooks used

| Need | Hook or API, with the place in the 18.7 source | Why this one |
|---|---|---|
| The routing decision and the analysis of whether a statement is a pushdown unit | `planner_hook` (already installed, `kingres_shard_planner_hook`) | It is the only point where the `Query` is still in the shape the rewriter produced. After `subquery_planner` has pulled sub-links up, they have become semi and anti joins and outer joins may have been reduced, and the deparser has no text for such a tree (`get_from_clause_item` in `ruleutils.c` raises "unrecognized join type" for anything but the four SQL join types, line 12709), so the text of a fragment is always built from a copy of the `Query` taken here. |
| Base relation: pruning and the 2a scan | `set_rel_pathlist_hook` (`allpaths.c:542`, already used) | Unchanged role. |
| Join of distributed and reference tables | `set_join_pathlist_hook` (`joinpath.c:345`, signature `(root, joinrel, outerrel, innerrel, jointype, extra)`) | Called for every pair of inputs of every join relation; used only for the join relation that covers every relation of the query level (`bms_equal(joinrel->relids, root->all_query_rels)`), so the cost of the hook is a bitmap comparison for all other pairs. |
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

## 3. Overview of the new paths

| Path | Stage | What each group returns | What the origin does |
|---|---|---|---|
| P1 scan (2a) | base relation | rows of one table that satisfy the shippable restrictions | joins, aggregates, sorts, limits |
| P2 join rows | the join relation of the whole level | rows of the join of all tables of the level | everything above the join |
| P3 partial aggregate | `UPPERREL_GROUP_AGG` | one row per group key, with the serialized transition state of each aggregate | final aggregation (`Finalize`), then everything above |
| P4 closed aggregate | `UPPERREL_GROUP_AGG` | final aggregate rows (the group key contains the distribution key) | concatenates, then everything above |
| P5 whole query | `UPPERREL_FINAL` | the final rows of the whole query, sorted if it has an `ORDER BY`, at most `limit+offset` rows | merges, applies `OFFSET` and `LIMIT` |

P1 exists today. P2 to P5 are new and are called "pushdown paths". All of them
are created only when the analysis of section 6 says the statement is a
pushdown unit. A query that is a pushdown unit still gets P1 (it costs nothing
to keep) and the planner chooses by cost.

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
`/* kingres:frag v=2 kind=<query|partial|insert> map=<V> */`, where `V` is the
map version of the origin's plan. A comment is inert text; a client that writes
one gets nothing it could not get without it, because a session is treated as
forwarded only if it carries the token of `forward_auth.c` (review S2-04), and
the receiving planner hook re-checks the statement instead of trusting the
marker.

For `kind=query` and `kind=partial` the forwarded session accepts the statement
only if all of these hold, and raises `KS001` (with a detail naming the
failed rule) otherwise:

1. it is a `SELECT` without sub-selects, CTEs, row locking, set operations or
   data-modifying parts;
2. every relation is a distributed table of one colocation group or a
   reference table, with at least one distributed table, and no relation has
   row-level security or a `checkAsUser` other than the session role;
3. the whole statement passes the same shippability walker (section 5) that the
   origin applied;
4. after the statement has been planned (see 4.3), the map version of this node
   equals `V`.

What the marker relaxes is only this: the 2a rule "run a statement locally only
if every bucket it can touch is provably local" (`plan_forwarded`) would reject
a query that reads a whole distributed table. A fragment *means* "answer from
the buckets of this node", so it is accepted without a bucket proof. It does
not relax privileges (the session is the role of the origin's session), it does
not allow anything that is not shippable, and it never writes. The `insert`
kind is described in section 11.

### 4.3 The version check comes after planning

The fragment's `V` is compared with the node's map version **after** the
fragment has been planned, not before. Planning a statement on a partitioned
table takes `AccessShareLock` on every partition that can be read, and taking a
lock processes pending invalidations (`LockRelationOid` calls
`AcceptInvalidationMessages`), so the catalog cache that
`kingres_shard_catalog()` returns is current as of the moment the locks were
taken. Section 10.2 uses this to show that a bucket move cannot commit while a
fragment that read the source bucket is in flight.

### 4.4 How the origin runs fragments ("Kingres Fragment Scan")

One custom scan runs all the fragments of a path:

- The set of groups is computed when the scan starts (`BeginCustomScan`), from
  the pruning rules of 4.5, against the origin's bucket map.
- The remote groups are started first: for each, `begin_xact`, then
  `PQsendQueryParams` of the `DECLARE`, so that the groups plan and start
  concurrently (this is what `ds_start` does today). The origin's own part runs
  after, as an SPI cursor (below). Rows are then read in batches of
  `kingres_shard.scan_fetch_size` rows per group (existing setting), the next
  batch of a group being requested as soon as the current one is used up
  (`frag_send_fetch`).
- **The origin's own part is the same text run through SPI**, as an SPI
  cursor opened with `SPI_cursor_open_with_args(..., read_only = true)`. For a
  read-only cursor `SPI_cursor_open_internal` uses `GetActiveSnapshot()`
  (spi.c, "if (read_only) snapshot = GetActiveSnapshot()"), that is the
  snapshot of the statement that is running, so the local part sees what the
  other local scans of the statement see. The text is prefixed with
  `/* kingres:local <nonce> */`, where `<nonce>` is a 64-bit random number made
  when the scan starts and held in a static variable only while that scan's SPI
  call is active; `planner_hook_body` plans a statement with that nonce without
  routing and without the scan (it is the local part, by construction). This
  replaces the 2a mechanism of `kingres_shard_in_fanout_local()`
  (`fanout.c`, `planner.c`), which recognizes the local part by comparing the
  whole statement text with the text being run: any nested statement with the
  same text, run by a function that the statement calls, would also be planned
  without routing. A nonce cannot be guessed by such a function, and `fanout.c`
  switches to it in step 1. Running the local part through SPI means it uses the
  planner and executor on its own buckets exactly as a remote group does, and no
  local join or aggregate path has to be built by hand.
- The local rows come back as `Datum`s; remote rows go through the input
  functions of the column types, as in 2a. Both fill the same scan tuple.
- **Memory is bounded** by one batch per group (plus the SPI batch): nothing is
  materialized. A cursor on a group is closed when the scan ends early
  (`LIMIT` above, a closed portal), as `ds_stop` does today.
- Rescan restarts the fragments with the new parameter values; the node
  supports no parameterized path (the pushdown paths are created only for paths
  without `param_info`).
- It is not parallel-aware, and the paths set `parallel_safe = false` and the
  relation `consider_parallel = false`, as 2a does (review S2-08); a parallel
  worker that reaches the node raises the same error as in 2a.

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
per group; the tests of step 2 measure that against the 2a scan so that the
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

The bucket of a constant is `kingres_shard_try_bucket`. When nothing prunes,
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

## 5. Shippability

The rule is the one `shippable.c` implements and the 0.1 and 0.2 reviews
shaped; this section states it in one place and says what 2b adds.

### 5.1 The rule for an expression that depends on data

An expression that contains a `Var` of a distributed or reference table may be
evaluated on another node only if **every** node of it satisfies all of:

1. functions and operators are built in (`oid < FirstGenbkiObjectId`, which
   `postgres_fdw` also uses as its `is_builtin`) and `IMMUTABLE`. Stable
   functions are not shipped: their result may depend on session state that
   differs between nodes (`TimeZone`, `lc_*`, `search_path`, `now()`);
2. every type of a column, a constant, a parameter, a result or a transition
   state is built in (`type < FirstGenbkiObjectId`), so that its text form and
   its meaning are the same on every node. OIDs between 10000 and 16383 are
   assigned by `genbki` and the `initdb` scripts and are not used;
3. the collation of every comparison, ordering or grouping is the default
   collation or `C` (`collation_ok`). The default collation is the same on
   every node because `add_node` refuses a node whose default collation,
   `LC_CTYPE` or provider version differs (README, "Nodes must agree"), and
   the default collation of a database is deterministic. A column or an
   expression with another collation, in particular a non-deterministic ICU
   collation, makes the clause local;
4. no conversion goes through the text form of a value (`CoerceViaIO`):
   `DateStyle`, `IntervalStyle`, `extra_float_digits`, `bytea_output` and
   `TimeZone` change it (review S2-03);
5. no system column; no `tableoid`, `ctid`, `xmin` (the values name one node's
   layout);
6. no `Const` of a `reg*` type whose value is an OID of a user object (the
   OID is local); built-in objects are fine;
7. only the node kinds `ship_walker` lists. Anything else (`SQLValueFunction`
   such as `current_user`, `FieldSelect`, `RowExpr`, `SubPlan`, `XmlExpr`,
   `JsonExpr`, ...) is not shipped.

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
| `lc_messages`, `lc_time`, `lc_monetary`, `lc_numeric` | Not controlled. Results of `to_char` with locale-dependent patterns (`TM` prefix) and of `money` depend on them; the functions are marked immutable by PostgreSQL, so they ship. **[Needs verification]** whether the remote fixed settings should also pin `lc_time`; settle it with a test that sets `lc_time` differently on two nodes and compares `to_char(d, 'TMDay')`. Until then the README says the nodes must have the same `lc_*` settings, as it already says for `lc_monetary` and `lc_numeric`. |
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

## 6. The pushdown unit: analysis in the planner hook

The analysis runs in `planner_hook_body` on a statement that the existing
decision has classified as "spans groups" and that is a `SELECT` (including a
cursor, `CREATE TABLE AS` and `EXPLAIN` of one: the planner hook receives the
`Query` of the `SELECT`). It produces `PushdownInfo` or a refusal reason; the
reason is shown by `EXPLAIN (VERBOSE)` on the 2a scan (section 12).

### 6.1 Eligibility

A query is a pushdown unit when all of these hold:

1. it is a top-level `SELECT` (`root->query_level == 1`), on a cluster with
   more than one group, in a session that is not a forwarded session, not in a
   parallel worker, with `kingres_shard.enable_pushdown`,
   `enable_distributed_scan` and `enable_routing` on;
2. it has no `WITH`, no set operation, no sub-links, no `FOR UPDATE`/`SHARE`,
   no set-returning function in the target list, and every range table entry is
   a plain table (`RTE_RELATION`), a join (`RTE_JOIN`) or the group entry
   (`RTE_GROUP`); a subquery, a view, a function, a `VALUES` list, a CTE
   reference or a table sample makes it not a unit;
3. every table is a distributed table of one colocation group or a reference
   table, at least one is distributed, and no ordinary local table is present
   (a local table does not exist on the other groups);
4. every distributed table is connected to the others by distribution-key
   equalities that make one key class (6.3);
5. the join types are those of section 9.1;
6. no range table entry has row-level security or a different `checkAsUser`
   (5.3);
7. grouping sets are not used (they are not partial-capable either; a query
   with grouping sets stays on the 2a scan).

A statement that fails any rule is planned exactly as in 2a. A statement
inside a subquery, a CTE, a view, a function or the source of an `INSERT` is
planned at its own level (`query_level > 1`) and is not a unit in 2b; it uses
the 2a scan with the new pruning and the new map-version check.

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

Why this is exact: tables of one colocation group have the same bucket count,
the same distribution type and collation and the same placement, so two rows
whose keys are equal under a hash-compatible deterministic equality hash to the
same bucket and live on the same group. Under a join on the key, every output
row is formed from rows of one group.

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

### 7.2 Rules of the extension on top of the planner's

- the aggregate is built in (`aggfnoid < FirstGenbkiObjectId`, immutable by
  `func_ok`): a user-defined aggregate, even with a combine function, is not
  split, because its transition type may not exist on the other nodes and its
  functions are not shippable (it forces the aggregation to the origin, as a
  non-shippable expression does, 5.4);
- the transition type is a built-in type, or `internal` with serialization (the
  partial column then has type `bytea`), so the partial state can cross as text;
- arguments, filter, group expressions and `HAVING` are shippable (5.1, 5.2);
- the group expressions are hashable or sortable at the origin (the planner
  checks this itself through `GROUPING_CAN_USE_HASH` and `_SORT`).

### 7.3 How a group produces partial states

No SQL expression returns the transition state of an aggregate, so the
fragment is the ordinary text `SELECT <group exprs>, <aggregate calls> FROM ...
WHERE ... GROUP BY ...` (no `HAVING`, `ORDER BY`, `LIMIT`, window or
`DISTINCT`), with `/* kingres:frag v=2 kind=partial map=V */`, and the group
turns its **plan** into a partial plan:

1. The planner hook of a session that is forwarded, or that runs the local
   fragment (nonce), recognizes `kind=partial`. It plans the statement with
   `standard_planner` inside a GUC nest level that sets
   `max_parallel_workers_per_gather = 0` and
   `enable_partitionwise_aggregate = off` (so the top of the plan is one `Agg`
   node over the scan or join).
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
part has the same columns and types as the other groups'. **[Needs
verification]** that the `Aggref` nodes of a finished plan can be re-marked this
way: `mark_partial_aggref` asserts `agg->aggsplit == AGGSPLIT_SIMPLE` and a
valid `aggtranstype`, which `preprocess_aggref` sets and `setrefs.c` keeps.
What settles it: a test on an assertion-enabled build that partializes the plan
of each aggregate in the "partial-capable" row of 7.1 and compares the final
result with the single-node result.

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
   **[Needs verification]** that `set_customscan_references` leaves the partial
   `Aggref`s of `custom_scan_tlist` and of the node's target list matching in
   the way `set_upper_references` needs; a ForeignScan with `scanrelid = 0` for
   an upper relation is the same arrangement and works, a test settles it for a
   CustomScan.
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
must still be built in and shippable (5.1), and order-insensitive in the sense
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

### 10.2 The map version, and a bucket that is moving or has just moved

Every fragment (the 2a scan fragment too) carries the map version `V` of the
origin's plan and is checked on the group after planning (4.3). Equality is
required; a mismatch raises `KS001` and the origin reloads its catalog (the
`kingres_shard_reload_catalog` of 2a) before the error reaches the client, so
the next statement is planned with the new map.

Argument that a statement whose fragments all passed reads every bucket exactly
once. A move (phase 4) commits in one distributed two-phase transaction that
detaches the source partitions and attaches the target tables, and raises the
map version on every node. A `DETACH PARTITION` that is not `CONCURRENTLY` (and
`CONCURRENTLY` cannot run inside a transaction block, so the switch cannot use
it) takes `ACCESS EXCLUSIVE` (`AlterTableGetLockLevel` in `tablecmds.c`:
`cmd_lockmode = AccessExclusiveLock` unless `concurrent`).

1. A fragment on the source group holds `ACCESS SHARE` on the partitions it
   reads from planning until the end of the origin's transaction (the remote
   transaction lives until then). The `DETACH` waits for that lock, so the move
   cannot prepare, and so cannot commit anywhere (its `ATTACH` on the target is
   not visible until commit), while such a fragment is in flight. Reading the
   bucket on the source is therefore consistent with the bucket still being
   unattached on the target.
2. If the move commits before the fragment is planned on the source, planning
   takes the lock after the commit, `AcceptInvalidationMessages` runs, and the
   version check (after planning) sees `V + 1`: `KS001`, no rows.
3. If the fragment on the target is planned before the move commits, the bucket
   is not attached there and it returns nothing for it; the source returns the
   bucket's rows (by 1). If it is planned after the commit, the check fails.
4. A group that is not asked because of pruning would be asked only if the
   origin's map is stale; then the group the origin asks is the old owner, which
   fails the check.

The 2a scan has the same guarantee once it carries the check; this is what
phase 4's test "readers during a move never see a row twice" relies on, and
that test must run with this check present. The orphan tables the old group
keeps after a move are standalone tables and are never read (a fragment names
the parent, which has no such partition); the standalone copies on the target
before the switch are not attached and are never read.

Costs, stated: (a) a transaction that has read a bucket through a fragment
holds the lock until it ends, so a long-open read transaction delays the
switch of that bucket (the switch times out after `move_switch_timeout` and goes
back to catching up, so the move retries; it can be starved by a transaction
that stays open); (b) a statement that is planned during a move's commit window
can fail with `KS001` and must be retried by the client.

Bulk writes and moves: a batch `INSERT` to a bucket whose partitions are locked
by the switch waits for the lock; when it proceeds after the commit, the
partition is gone and PostgreSQL's own tuple routing raises `no partition of
relation ... found for row` (SQLSTATE 23514). The statement fails as a whole; it
never places a row in the wrong bucket. The insert fragment also carries the
version check (section 11.5), which catches a stale map before the first row.

### 10.3 Standbys and failover (phase 5)

Fragments go to the primary of each group, as every 2a statement does. A
failover changes the map version (`promote_node`), so a statement planned
before it fails the version check or the connection and is retried by the
client.

### 10.4 Prepared statements and cached plans

- Fragment plans embed no parameter *value* at plan time except through
  pruning on constants; a key restriction by a parameter is evaluated when the
  scan starts (4.5). They therefore work as generic plans, unlike a routed
  statement (the 2a limitation stays for routed statements only).
- A cached plan of a pushdown path depends on the map: the plan's
  `relationOids` contain every distributed table of the statement (the
  planner's flat range table adds `RTE_RELATION` OIDs to `glob->relationOids`
  in `add_rte_to_flat_rtable`; the entries of the original `Query` stay in the
  final range table with a scan relation of 0, as for a foreign join), and the
  `map_version` trigger invalidates the relcache entry of every distributed
  table on every change, which invalidates the cached plan. **[Needs
  verification]** that a `CustomScan` with `scanrelid = 0` replacing a join keeps
  the original range table entries; a test prepares the statement, moves a
  bucket, executes it again and checks that a new plan is made.
- The plan stores `V`; the check of 10.2 catches a plan that was valid when it
  was made and is not now, even if invalidation was missed.

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
- **Stored generated columns** are never sent; each group computes them (their
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
/* kingres:frag v=2 kind=insert map=<V> */
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

`ON CONFLICT DO UPDATE` is **refused** ("not supported yet"): applied per batch
it would let a second row for the same key update the row a previous batch
inserted, where a single statement raises "ON CONFLICT DO UPDATE command cannot
affect row a second time". `DO NOTHING` is allowed, since skipping a later
duplicate is the same in either case. `RETURNING` expressions and an inference
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
`RETURNING` list that passes the shippability walker. It compares `V` with its
map version after planning (4.3). It does **not** prove that the rows belong to
its buckets: PostgreSQL's tuple routing does that when each row is inserted,
and a row whose bucket is not attached here fails with `no partition of
relation ... found for row`. A row can therefore never be stored in a wrong
bucket whatever the origin believes. The only effect of the marker is that the
2a "provably local" rule, which would reject an insert whose key it cannot see,
is waived for this one statement shape.

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
by `NextCopyFrom`, and are not repeated here.

For each row the handler gets `values[]` and `nulls[]`, takes the value of the
distribution column, computes the bucket with `kingres_shard_bucket` (a `NULL`
key is bucket 0, which is what PostgreSQL's hash partitioning does; the existing
function states this), and feeds the router's per-group buffers (11.4). Error
context lines ("COPY t, line 3, column x") come from installing
`CopyFromErrorCallback` as an `ErrorContextCallback`, which `file_fdw` does
too. The number of rows is reported as the `COPY n` tag. `pg_stat_progress_copy`
is not updated (documented).

### 11.7 Colocated `INSERT ... SELECT` without moving rows (P6)

When the target and every source table are in the same colocation group (or
reference tables), the whole statement is shipped as the 2a write fragment to
every group of the target (the `kingres_shard_make_fanout_plan` path with
`all_rows`): each group inserts rows it reads from its own buckets into its own
buckets, with no row passing through the origin. Conditions, all checked at
the origin:

- the statement is shippable as a whole: only built-in immutable functions, no
  volatile or sequence default for a column the statement does not give
  (otherwise the rows go through the router, so that a default is computed once
  at the origin, 11.2), no `ON CONFLICT DO UPDATE`, `RETURNING` shippable;
- the select list gives the **target's distribution column** directly as the
  **distribution key variable of the key class** of the source (a bare `Var`,
  possibly under a `RelabelType`), so the row's target bucket is its source
  bucket and the target partition is on the same group;
- the source is a pushdown unit's `FROM`/`WHERE` (6.1 rules 2 to 5), with no
  aggregation that crosses groups (a closed query only).

A receiving group needs no proof: a row that did not land in a local
partition fails with `no partition ... found for row`. The group's own
`plan_forwarded` accepts the statement when it carries `KS_WRITE_MARKER`, is an
`INSERT`, and passes the shippability walker, which this phase extends so that
`relation_ok` also accepts the colocated source tables of an `INSERT`. All
other `INSERT ... SELECT` statements use the router.

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

| GUC | Default | Context | Meaning |
|---|---|---|---|
| `kingres_shard.enable_pushdown` | on | `USERSET` | Master switch for P2 to P6. Off: the 2a plan shapes (scan with per-table restrictions); pruning and the map-version check stay on. |
| `kingres_shard.enable_partial_aggregation` | on | `USERSET` | P3 only. Off: an aggregate that is not closed is aggregated at the origin over P2 or P1. P4 (exact) stays on. Turn off when floating-point results must be repeatable to the last bit (A3). |
| `kingres_shard.enable_bulk_routing` | on | `USERSET` | Off: a multi-group `INSERT`/`COPY` is refused as in 2a, and the non-constant-key single-row insert too. |
| `kingres_shard.insert_batch_rows` | 1000 | `USERSET`, 1 to 100000 | Rows per batch per group. |
| `kingres_shard.insert_batch_bytes` | 1MB | `USERSET`, 16kB to 64MB | Text bytes per batch per group (the sum of the lengths of the values). |
| `kingres_shard.scan_fetch_size` | 1000 | `USERSET` (exists) | Rows per `FETCH` of every fragment, 2a scans included. |

They only choose among plans that give the same answer, or among statements
that are refused, so ordinary users may set them; they grant nothing.
`kingres_shard.enable_routing = off` (superuser) and
`enable_distributed_scan = off` still switch off everything they switched off
before, pushdown included (a pushdown path needs the scan machinery). The three
`enable_*` switches are listed in the `security` regression test with the other
GUCs (coding-standards, 1.3: the test lists `SHOW ALL` by prefix).

Every GUC is defined in `_PG_init` with `MarkGUCPrefixReserved`, as the others.
Turning every new switch off gives exactly the 2a behaviour, apart from the new
pruning and the map-version check, which are corrections and have no switch; if
one of them is found wrong the fallback is `enable_distributed_scan = off`.

## 14. Version and merge order

- Phase 4 is 0.3 (`kingres_shard--0.3.sql` and `--0.2--0.3.sql` exist in the
  working tree). Phase 5 plans 0.4 (ha-phase5.md, Part C). Phase 2b is **0.5**,
  with `kingres_shard--0.4--0.5.sql`.
- 2b adds no table, view, function or column. The script contains the standard
  guard and a comment only. The version is raised because the nodes must run
  the same fragment protocol (`kind=query|partial|insert`, `map=V`): `add_node`
  already refuses a node that runs another version of the extension (README,
  "Nodes must agree"), and the version check is what enforces the protocol.
- The code steps (section 17, steps 1 to 10) change only C files, tests and the
  README and **do not change `default_version`**, so they can be merged before
  or after phases 4 and 5 and in any order relative to them. The last step (the
  version bump with the upgrade script and the `upgrade` regression test) must
  merge after 0.4 exists, because the upgrade chain is
  0.2 -> 0.3 -> 0.4 -> 0.5 and `ALTER EXTENSION UPDATE` needs every link. If
  phase 5 is delayed, the bump waits; it must not be renumbered to 0.4 or to
  0.3, which belong to the other phases. Test file names use the range
  `t/040` to `t/049` so that they do not collide with the files phases 4 and 5
  add (this assumes those use lower numbers; the integrator renumbers at merge).
- Rolling upgrade: nodes of 0.5 and older nodes cannot cooperate on the new
  fragments. A node of an older version answers a new fragment with `KS001` (it
  does not recognize the marker, so its rule "run locally only if provably
  local" rejects the statement), never with a wrong answer. During a rolling
  upgrade set `kingres_shard.enable_pushdown = off` and `enable_bulk_routing =
  off` on every node (they are per session or per server), and switch them on
  when the last node is upgraded. The README says so.

## 15. Security

### 15.1 The forwarded-session model, unchanged

A session is forwarded only if it carries `kingres_shard.forward_token`: the
hex form of `HMAC-SHA-256(secret, "kingres-forward-v1" NUL role NUL id of the
target node)`, made from `kingres.cluster_secret`. The receiving backend checks
it at the first statement with a constant-time comparison; a session that claims
the flag and cannot prove it gets `FATAL` before any statement runs (README,
"The forwarded flag"). The token binds the role and the target node. This phase
adds statement forms to the forwarded session and does not touch the token.

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

The hostile-identifier and hostile-value test (section 16, `048`) exercises
every row of this table.

### 15.4 Other points

- A user-defined function runs only at the origin: through 5.2 (a value computed
  once) or as an expression the planner left above the pushed part. It never
  runs on another node as part of a fragment.
- A distributed transaction's remote connections still use the user's
  password-less or TLS credentials as in 2a; the README's advice to use TLS for
  node-to-node connections stands.
- `kingres_shard.node_conninfo` and the secret are unchanged.

## 16. Tests

Rules from `testing.md` apply: tests are not tied to the developer's machine,
none is flaky, a bug fix starts with a test, and C changes are run on a build
with assertions enabled (several items above rely on `Assert`s of the
executor). `pg_regress` tests run on one node with a fictitious second shard
group in the catalog, as `utility_guard` and `routing_decisions` do; they decide
plans with `EXPLAIN` and never execute a remote fragment. TAP tests use three
nodes and the comparison harness.

### 16.1 pg_regress (`contrib/kingres_shard/sql`, `expected`)

| File | What it proves |
|---|---|
| `pushdown_plans` | For a fictitious two-group catalog: every shape of sections 3, 8, 9 and 11 gets the plan of section 12; every refusal rule of 6.1 and 5.4 gives the 2a plan and the reason line of `EXPLAIN (VERBOSE)`; each new GUC switches its path off and only its path; pruning shows `Remote groups: n of m`. |
| `pushdown_shippable` | The shippability matrix of 5.1: one statement per rule, each shipped or not as stated (`now() - interval` through 5.2 ships, `random()` does not, `CoerceViaIO` does not, a column with `COLLATE "C"` ships, a non-default collation does not, a user function does not, a `reg*` constant of a user table does not). Run under hostile session settings. |
| `pushdown_agg_catalog` | The classification of 7.1 recomputed from `pg_aggregate` at run time equals the expected file, and every partial-capable aggregate's plan has `Finalize` over the fragment scan, every other has not. |
| `pushdown_joins_plans` | The join matrix of 9.1: each row of both tables gives pushed or not pushed, with the reason. |
| `bulk_plans` | `EXPLAIN` of each insert form shows the router; refusals (`ON CONFLICT DO UPDATE`, reference-table writer that reads a distributed table, `COPY ... WHERE`) give their message; privilege matrix of `COPY` checks (no `INSERT`, column privileges, file and program roles, read-only transaction). |
| `security` (extended) | Lists the new GUCs; the scan of `pg_proc` for functions without a pinned `search_path` still finds none (2b adds no function); `SHOW kingres_shard.enable_pushdown` and the others are visible. |
| `upgrade` (extended) | `ALTER EXTENSION kingres_shard UPDATE` from 0.4 to 0.5 gives a catalog identical to a fresh 0.5. |

### 16.2 TAP (`contrib/kingres_shard/t`)

Common to the equivalence tests: the harness `KingresShardCompare.pm` gains
`compare()` **plan profiles** next to its four session profiles: pushdown on (the
default), `enable_pushdown = off`, `enable_partial_aggregation = off`, and a run
on the single node. Every query result must be identical in all of them. The
data is generated by `generate_series` with a fixed seed and includes: `NULL`
keys and values, duplicate rows, skewed keys (one bucket holds half the rows), a
group that owns no row of one table, an empty table, very wide rows, text with
the characters of 16.4, `float8` with `NaN`, `-0` and infinities, `numeric` of
many digits. A query generator (seeded from the environment variable
`KS_SEED`, printed in the test output so that a failure is reproducible)
produces 500 queries per run from templates: filter, join (all join types of
9.1), group by with aggregates from 7.1, `HAVING`, order by, limit and offset,
and combinations; the test also keeps a fixed list. Float results are rounded to
9 significant digits before comparison (A3); `array_agg` and `string_agg` results
are compared after sorting their elements.

| File | What it proves |
|---|---|
| `040_pushdown_equiv` | The result equivalence above, on the whole generated list, under the four session profiles of 2a; `enable_pushdown = off` and `on` give equal results; the plans of a fixed list are checked to be pushed (so the test cannot pass by never pushing). |
| `041_pushdown_agg` | Every aggregate of 7.1 on every type it takes (integer, `numeric`, float, text, `bool`, bit, `interval`, array, `date`, `timestamptz`), with `FILTER`, `HAVING`, several aggregates sharing a state, `GROUP BY` an expression, scalar aggregate over empty and non-empty tables (A7), `count(DISTINCT)` (A4: closed gives the pushed plan, open gives the 2a plan), ordered-set and hypothetical-set aggregates (open: 2a; closed: pushed), grouping sets (2a), window above aggregate, aggregates over a table with a dropped column; the partialize step on each plan strategy and on a build with assertions. |
| `042_pushdown_join` | The matrix of 9.1 on data with matching, non-matching and `NULL` keys: every permitted join equals the single node; every refused join gives the 2a plan and the same rows; a self join; a three-way join with a reference table; a join under a different collation; two key classes. |
| `043_pushdown_sort_limit` | O1 to O7: the ordering combinations of O2 (including `NaN`), ties, offset beyond the rows, `LIMIT 0`, `LIMIT NULL`, `WITH TIES`, a cursor fetched in parts and closed early (the group cursors are closed, `pg_cursors` on the groups is empty), a limit that is larger than the table, memory bounded with a small `work_mem` and a large result. |
| `044_pushdown_consistency` | 10.1 and 10.2: a concurrent writer on the groups while a pushed aggregate and a pushed join run (every result is a state that existed on each group); the version check (`KS001` when a group's catalog is changed behind the origin's back with a catalog change, and the reload); with phase 4 present, a bucket move running in a loop while the equivalence queries run on every node: no row twice, none missing, errors only `KS001` and the switch retry; a long read transaction holds back the switch and the timeout returns the move to catching up; prepared statements executed six times, with `force_generic_plan`, and after a move (10.4); a transaction that sees its own writes through a fragment. |
| `045_bulk_insert` | Each insert form of 11.1 with data hashing to all groups: multi-row `VALUES`, `INSERT ... SELECT` from a staging table, from a distributed table (router) and from a colocated one (P6), a default key, `serial` and identity (`ALWAYS` and `BY DEFAULT`) columns, a stored generated column, an immutable and a volatile default, `ON CONFLICT DO NOTHING`, `RETURNING` (rows and counts), a foreign key to a reference table and to a colocated table, savepoints, rollback; row counts and the table after equal the single node's; every group's partitions hold exactly the rows of their buckets; memory stays bounded for 5 million rows with `insert_batch_rows = 100` (the test samples the backend's memory contexts through `pg_log_backend_memory_contexts` or a fixed upper bound on RSS). |
| `046_bulk_copy` | Text, CSV and binary `COPY FROM STDIN` and from a file, `HEADER`, `NULL`, `QUOTE`, `DEFAULT`, `FORCE_NULL`, malformed rows (the error names the line and column), a row with a `NULL` key, `COPY` into a table with a `serial` column, refusals (`FREEZE`, `WHERE`, `ON_ERROR`), the privilege checks of 11.6, `psql \copy`. |
| `047_bulk_faults` | With `kingres_shard.fault_injection` points `bulk_*` and the 2a points: an error before and after the first batch, between groups, before the last flush; a group `kill`ed (`pg_ctl stop -m immediate`) in the middle; the origin killed after the decision is logged and before `COMMIT PREPARED`; the target group out of connections; a `statement_timeout` and `pg_cancel_backend` in the middle. After each: `pg_prepared_xacts` is empty on every node once recovery has run, no node holds a row of the statement if it rolled back, every node holds its rows if it committed, `kingres.dtx_log` is consistent. |
| `048_hostile_values` | Identifiers and values cross every deparser intact (see 16.4). |
| `049_pushdown_security` | A client with `forwarded` and no token is refused for every fragment kind; a forged fragment of a valid session (a join of two tables of different groups, a user function, a sub-select, a write) is refused by the group's walker with `KS001` and runs nothing; privileges: a role without `SELECT` on one table of a join, without `INSERT`, with column-level privileges; a security-barrier view and a table with row-level security (not a unit); a view owned by another role; the `COPY` privileges; the local-fragment nonce cannot be guessed (a function that runs the text of the local fragment inside the statement is still routed). |

A cross-extension test `050_pushdown_ledger` loads `kingres_ledger` with
`kingres_shard`: `INSERT INTO <ledger table> SELECT ... FROM <distributed table>`
from every node keeps the ledger's guarantees (rows captured exactly once,
`ledger_verify` clean, an `UPDATE`/`DELETE` of an append-only table still
refused), and a `COPY` into a distributed table by a role that also owns ledger
tables does not touch them.

### 16.3 Tests per correctness condition

A1, A2, A3, A5, A7, A8, A10: `041`. A4, A6, A9: `041` and `pushdown_plans`.
O1 to O7: `043`. J1 to J5: `042`. Consistency 10.1 to 10.4: `044`. Section 11:
`045` to `047`. Section 5: `pushdown_shippable` and `017_settings_compare` run
with the pushdown paths on.

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

Each step is mergeable and testable alone, leaves the cluster correct if the
next step never lands (every new path is chosen by cost among correct paths and
switched off by its GUC), and is sized for one engineer and one independent
reviewer (about 1000 to 1500 changed lines of C plus tests).

| Step | Content | Files touched | Tests that must pass and be added |
|---|---|---|---|
| 1 | Foundation. The map version in fragments and its check after planning; pruning from `baserestrictinfo` and from key expressions at scan start, for the 2a scan; `kingres_shard_in_local_fragment` with the nonce, replacing the text comparison in `fanout.c` and `planner.c`; origin-evaluated parameters (5.2) for the 2a scan's restrictions; multi-relation `ship_walker` mode (`clause_shippable` with a set of allowed range table indexes); the hostile-value test over the 2a statements; the harness plan profiles | `shippable.c/h`, `dist_scan.c`, `planner.c`, `fanout.c/h`, `kingres_shard.h`, `node_conn.c/h` (only the nonce state if any), `t/KingresShardCompare.pm` | `pushdown_shippable`, `pushdown_plans` (pruning part), `013_dist_scan` additions, `048_hostile_values` (2a part), `044` (the check part), the 2a tests unchanged |
| 2 | The fragment scan: refactor `DsFrag` and its start, fetch, drain and stop code out of `dist_scan.c` into `fragment_scan.c/h`; the local part through SPI; the `Kingres Fragment Scan` custom scan with unordered merge; the analysis (`pushdown.c/h`: eligibility 6.1, key class 6.3, closed 6.4) and the `PushdownInfo` hand-over from `planner_hook_body`; P2 in `set_join_pathlist_hook`; EXPLAIN | new `fragment_scan.c/h`, `pushdown.c/h`; `dist_scan.c`, `planner.c`, `kingres_shard.c` (GUC), `Makefile`, `meson.build` | `pushdown_joins_plans`, `042_pushdown_join`, `040_pushdown_equiv` (joins only) |
| 3 | P5 without order and limit and P4 (closed query): the `UPPERREL_FINAL` and `UPPERREL_GROUP_AGG` hooks for the closed case | `pushdown.c`, `fragment_scan.c` | `040`, `041` (closed aggregates), `pushdown_plans` |
| 4 | Ordered merge and limit pushdown (8): `SortSupport` merge in the fragment scan, `create_limit_path`, the limit parameter | `fragment_scan.c`, `pushdown.c` | `043_pushdown_sort_limit` |
| 5 | P3, partial aggregation: the partial target, `partialize_plan` on the groups and the local part, the final path with `create_agg_path`, the catalog test | `pushdown.c`, `planner.c` (`partialize_plan`, the `kind=partial` rule) , `fragment_scan.c` | `pushdown_agg_catalog`, `041` complete, `040` complete, on a build with assertions |
| 6 | Consistency tests with phase 4 (a test-only step if phase 4 has landed; otherwise the part that does not need it) and the README rewrite of "Guarantees" and "Limitations" for pushdown | `README.md`, `t/044` | `044_pushdown_consistency` |
| 7 | The row router for `INSERT`: the plan, the SPI source, the batch buffers, the wire statement, `RETURNING`, `ON CONFLICT DO NOTHING`, the insert fragment rule, the non-constant single-row key | new `row_router.c/h`; `planner.c`, `remote_scan.c` (shared helpers), `node_conn.c/h` (async send for batches), `fault.c` (points) | `bulk_plans`, `045_bulk_insert`, `047_bulk_faults` (insert part), `048` (router part) |
| 8 | `COPY FROM` through the router | `utility.c`, `row_router.c` | `046_bulk_copy`, `047` (copy part), the privilege matrix of `bulk_plans` |
| 9 | Colocated `INSERT ... SELECT` (P6) | `shippable.c`, `planner.c`, `fanout.c` | `045` (P6 part), `pushdown_plans` |
| 10 | Security review round, the cross-extension ledger test, EXPLAIN polish, README | `README.md`, tests | `049_pushdown_security`, `050_pushdown_ledger` |
| 11 | Version bump to 0.5 and the upgrade script; merges after 0.4 | `kingres_shard.control`, `kingres_shard--0.4--0.5.sql`, `kingres_shard--0.5.sql`, `Makefile`, `meson.build`, `sql/upgrade.sql` | `upgrade` |

Steps 1 and 2 are the riskiest for review (they move code); they contain no new
behaviour except pruning and the check, so the reviewer can compare them with
the 2a tests. Steps 3 to 5 each add one kind of path and can be reviewed against
the matching conditions of sections 6 to 9. Steps 7 to 9 are independent of
steps 3 to 5.

## 18. Out of scope, and what a user still sees refused after 2b

- Statements that are not pushdown units use the 2a scan and are correct but
  slow: any statement with a subquery, view, CTE, set operation, window function
  not partitioned by the key, a local table, a `LATERAL` item, a semi or anti
  join (`IN`, `EXISTS`), grouping sets, an aggregate that has no combine function
  and is not closed, a non-shippable expression, two colocation groups, a join
  not on the key, an `INSERT ... SELECT` source of its own (it is planned as a
  subquery).
- Refused as in 2a: `SELECT ... FOR UPDATE/SHARE` across groups, system columns
  of a distributed table, `UPDATE ... FROM`/`DELETE ... USING`/`MERGE` that read
  a distributed table and write another, `MERGE` on distributed tables of
  another group, a reference-table write that reads a distributed table, an
  `UPDATE` of the distribution column, `COPY FROM` into a reference table on a
  cluster with several groups, a routed statement under `force_generic_plan`.
- New refusals, all clear errors: `INSERT ... ON CONFLICT DO UPDATE` whose rows
  span groups; `COPY ... FROM` with `FREEZE`, `WHERE`, `ON_ERROR`, `REJECT_LIMIT`
  on a distributed table of a multi-group cluster; a bulk `INSERT` whose
  `RETURNING` or inference clause is not shippable.
- Not provided: a global snapshot, distributed deadlock detection, parallel
  query on the groups' fragments, a statement-level retry after `KS001` for
  fragments, `RETURNING` in the input order of a multi-group bulk insert,
  `pg_stat_progress_copy` for a routed `COPY`, pushdown for a user-defined
  aggregate, a window function not partitioned by the key, `DISTINCT` pushed as
  a partial operation, and distributed ledger tables.
- Bucket moves can still make a statement fail with `KS001` while the switch
  commits; the client retries.

## 19. Needs verification

| # | Item | What would settle it |
|---|---|---|
| V1 | `partialize_plan` (7.3): the plan of the restricted fragment has an `Agg` on top under the settings of 7.3 for every strategy, and the `Aggref`s of a finished plan can be re-marked | A test per strategy on an assertion build comparing with the single node (`041`); read of `setrefs.c` while implementing. |
| V2 | `set_customscan_references` and `convert_combining_aggrefs` accept a CustomScan whose `custom_scan_tlist` holds partial `Aggref`s (7.4) | The plan of any partial aggregate, `EXPLAIN` and a run; a ForeignScan with `scanrelid = 0` is the known-good arrangement. |
| V3 | `pg_get_querydef` of the modified copy (select list replaced, clauses cleared, `GROUP BY` by sort-group reference) prints a valid statement for every shape of the tests (7.4, 4.1) | The regression test `pushdown_plans` prints each fragment; a round trip (parse the text, plan it) for each. |
| V4 | A `CustomScan` with `scanrelid = 0` that replaces a join keeps the original range table entries and their OIDs in `relationOids`, so a cached plan is invalidated by the map change (10.4) | The prepare, move, execute test of `044`; `add_rte_to_flat_rtable` (`setrefs.c:542`, the `relationOids` append at 586) is the code to read. |
| V5 | That `NextCopyFrom` evaluates the default expression (`nextval`) of an omitted `serial` or identity column, in the origin's session, for every row (11.2 relies on it), including for `GENERATED ALWAYS` identity columns | `046`: `COPY` into a table with each kind of default and identity column, comparing the values with the single node. |
| V6 | The exact checks of `DoCopy` to repeat (11.6) are complete in 18.7 | Read `DoCopy` in `commands/copy.c` line by line while implementing; the privilege matrix of `bulk_plans` fails if one is missed. |
| V7 | `lc_time` and other `lc_*` settings that make an immutable built-in function differ between nodes (5.3) | The test described in 5.3. |
| V8 | A fragment cursor can be opened by the forwarded session as `DECLARE ... NO SCROLL CURSOR FOR <query with aggregate and join>` with the parameters of the extended protocol, as 2a does for a scan (it works for the 2a scan; the same for these shapes) | `042`, `041`. |
| V9 | The planner takes `AccessShareLock` on all partitions it can read before `plan_forwarded`'s check, and the lock stays until the end of the remote transaction, for a fragment that reads through a cursor (4.3, 10.2) | A TAP test that holds a fragment's remote transaction open and shows `kingres.move_bucket` waiting on it (needs phase 4). |
| V10 | The cost model: pushdown paths win against the 2a scan in the cases of the tests without hints | Tuning in step 2 to 5 with `EXPLAIN`; the constants are in the regression expected files. |
| V11 | `unnest` with several array arguments in `FROM` and a `text[]` cast per column has the throughput claimed for a batch of 1000 rows | A measurement in step 7 against a multi-row `VALUES` batch; if it is much slower the wire format changes behind the same router interface, and this note is updated. |
| V12 | Memory bound of the router for the stated settings (11.4) | `045`'s sampling of the memory contexts. |
| V13 | That `pull_up_sublinks` and the other preprocessing really are the only reasons the planner's tree cannot be deparsed, and that no `Query` in a unit (no sub-links, no subqueries, 6.1) is changed by the rewriter in a way `pg_get_querydef` of the copy would not reproduce (for example a view expanded into a subquery, which 6.1 refuses) | `pushdown_plans`: every shape of the unit prints a fragment that parses and plans, and `pg_get_querydef` of the copy is compared with the statement as written for a list of statements. |
| V14 | `LockRelationOid` processes invalidations so that the catalog cache is current after the lock of a partition (4.3). `lmgr.c` calls `AcceptInvalidationMessages()` in `LockRelationOid` and `LockRelation`, and the extension's cache is invalidated by relcache callbacks, but the ordering with the extension's own cache reload was not traced | A test that commits a catalog change between the lock and the check (fault point `frag_after_plan` in the group) and shows `KS001`. |

## Implementation notes and deviations

Added when phase 2b is implemented, in the form of the notes of the earlier
phases.
