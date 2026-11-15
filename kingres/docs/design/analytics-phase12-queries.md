<!-- SPDX-License-Identifier: PostgreSQL -->
# KingresDB: design of phase 12, part 1 (repartitioned joins, MERGE on distributed tables, approximate aggregates)

Status: Proposed design, 2026-11-15; not implemented.
Builds on [shard-phase2b.md](shard-phase2b.md) (pushdown, fragments, the row
router; extension version 0.5), on [shard-phase4.md](shard-phase4.md) (bucket
moves) and on [ADR 0022](../adr/0022-the-complete-scan-stays.md) (the row-moving
scan stays). Part 2 of phase 12, columnar storage and time partitioning with
retention, is [analytics-phase12-storage.md](analytics-phase12-storage.md). The
phases between them are [shard-phase9.md](shard-phase9.md),
[shard-phase10.md](shard-phase10.md) and [shard-phase11.md](shard-phase11.md);
this note does not depend on their content (section 15 lists what it needs from
them).

After phase 2b a `SELECT` that spans shard groups can push restrictions,
aggregates, sorts, limits and colocated joins to the groups, and bulk writes go
through a row router. Three things that real-time analytics needs are still
missing and are the subject of this note:

1. A join on columns other than the distribution column still moves every row of
   both tables to the originating node (the 2a scan) and joins there.
2. `MERGE` on a distributed table is refused.
3. `count(DISTINCT x)`, "most frequent values" and percentiles over large
   distributed tables need every row of `x` at the origin, and a rollup table
   cannot hold a summary of such a value.

Everything here is an optimization or a lifted refusal that sits next to the
unchanged scan. The standing rule of ADR 0022 applies to every path: a path is
chosen only when its conditions are proven; otherwise the statement is planned as
before (a `SELECT`: the scan; a write: the refusal that exists today); every
path has a setting that switches it off; every path has tests that compare its
answers with the answers of the scan (for the approximate aggregates, with the
answers of the exact aggregate within a stated bound).

How this note was checked. Claims about PostgreSQL are checked against the 18.7
source in this tree, with the file and function named. Claims about the
extension are checked against the committed `contrib/kingres_shard` (the working
tree is being edited by others, so only `HEAD` was read for that directory and
for `kingres/control-plane`). Claims about other projects and licences are
checked against the pages named in "Sources read". Anything not checked is
marked **[Needs verification]** with what would settle it, and collected in
section 19.

## Sources read

- Documents of this repository: `contrib/kingres_shard/README.md`,
  `shard-phase2b.md` (whole; this note reuses its fragments, markers,
  shippability rules, key classes, `PushdownInfo`, partial aggregation of its
  section 7, row router of its section 11 and `EXPLAIN` format of its section 12),
  `shard-phase10.md` (its catalog and split sections: per-bucket `modulus`,
  `colocation.max_modulus`, the routing array, `placement_epoch`),
  `shard-phase2a.md`, `shard-phase4.md` (the move and the placement check of
  "The race between a move and a statement"), `ha-phase5.md` (title and
  status only), `control-plane-3.md`, ADR 0004 (PostgreSQL licence, no Citus
  code), ADR 0022, `kingres/docs/dev/documentation.md`,
  `kingres/docs/dev/core-patches.md`, `kingres/web/README.md`,
  `kingres/control-plane/README.md` (task engine), `kingres/api/README.md` and
  `kingres/api/openapi.yaml` (contract 0.6.0: databases, tasks, problem codes,
  roles, `executeQuery`).
- Code at `HEAD`: `contrib/kingres_shard/src/planner.c` (the `MERGE`
  refusals), `dist_scan.c` (`ds_error_system_columns`, the cost of the scan),
  `kingres_shard.h` (SQLSTATEs `KS001`, `KS004`), `kingres_shard.control`,
  `meson.build`; `kingres/control-plane/internal/ops/databases.go` (task steps,
  `installExtensions`, `database_setting`), `internal/sysdb/migrations`.
- PostgreSQL 18.7 in this tree: `src/backend/access/transam/xact.c`
  (`PrepareTransaction`, the refusal after temporary objects, line 2630),
  `access/common/relation.c` (the flag is set when a temporary relation is
  opened), `optimizer/plan/planner.c` (`preprocess_rowmarks`, line 2622 and the
  `ROW_MARK_COPY` choice for a relation that is not a table, line 2741),
  `optimizer/prep/prepjointree.c` (`transform_MERGE_to_join`, line 191),
  `parser/parse_merge.c`, `executor/nodeModifyTable.c` (`MERGE` execution),
  `include/nodes/primnodes.h` (`MergeSupportFunc`), `include/nodes/pathnodes.h`
  (`CustomPath.custom_paths`), `include/nodes/plannodes.h`
  (`CustomScan.custom_plans`), `include/nodes/execnodes.h`
  (`CustomScanState.custom_ps`), `backend/optimizer/plan/createplan.c`
  (`create_customscan_plan`), `doc/src/sgml/ref/create_aggregate.sgml`
  (`COMBINEFUNC`, `SERIALFUNC`, `DESERIALFUNC`, `PARALLEL`).
- Web sources (read on 2026-11-15; licences read from the `LICENSE` files):
  - `postgresql-hll`, <https://github.com/citusdata/postgresql-hll>, licence
    Apache-2.0 (<https://raw.githubusercontent.com/citusdata/postgresql-hll/master/LICENSE>,
    first lines "Apache License, Version 2.0, January 2004"). The page lists
    PostgreSQL 9.4 to 18 as tested.
  - `postgresql-topn`, <https://github.com/citusdata/postgresql-topn>, licence
    **AGPL-3.0** (<https://raw.githubusercontent.com/citusdata/postgresql-topn/master/LICENSE>,
    first lines "GNU AFFERO GENERAL PUBLIC LICENSE, Version 3, 19 November 2007").
    The page lists PostgreSQL 9.6 to 17.
  - `tdigest`, <https://github.com/tvondra/tdigest>, licence PostgreSQL
    License (<https://raw.githubusercontent.com/tvondra/tdigest/master/LICENSE>,
    a copyright line of its author and the standard permission and disclaimer
    paragraphs). The page says the extension supports PostgreSQL 13 and newer
    and that its development branch installs version 2.0.0-dev.
  - Citus documentation, read for user-visible behaviour only (Citus is AGPL;
    none of its source code was read or is used):
    <https://docs.citusdata.com/en/stable/develop/reference_sql.html> (repartition
    joins, `citus.count_distinct_error_rate`),
    <https://docs.citusdata.com/en/stable/articles/hll_count_distinct.html>,
    <https://docs.citusdata.com/en/stable/develop/reference_dml.html>
    (`INSERT ... SELECT`, upserts as rollups), and, for the `MERGE` support
    matrix, the search result summaries of
    <https://docs.citusdata.com/en/latest/reference/sql_workarounds.html> and
    <https://docs.citusdata.com/en/v11.3/develop/reference_workarounds.html>
    (the matrix itself was not read in full: **[Needs verification]**).
- Papers (the algorithms of section 9 are written from them):
  - P. Flajolet, E. Fusy, O. Gandouet, F. Meunier, "HyperLogLog: the analysis of
    a near-optimal cardinality estimation algorithm", AofA 2007.
  - S. Heule, M. Nunkesser, A. Hall, "HyperLogLog in Practice: Algorithmic
    Engineering of a State of The Art Cardinality Estimation Algorithm", EDBT 2013.
  - O. Ertl, "New cardinality estimation algorithms for HyperLogLog sketches",
    arXiv:1702.01284, 2017.
  - A. Metwally, D. Agrawal, A. El Abbadi, "Efficient Computation of Frequent and
    Top-k Elements in Data Streams", ICDT 2005 (Space-Saving).
  - P. K. Agarwal, G. Cormode, Z. Huang, J. Phillips, Z. Wei, K. Yi, "Mergeable
    Summaries", PODS 2012 (merging of counter summaries).
  - T. Dunning, O. Ertl, "Computing Extremely Accurate Quantiles Using
    t-Digests", arXiv:1902.04023, 2019.

## Goals and scope

| # | Item | Section |
|---|---|---|
| 1 | Repartitioned joins: a join of distributed tables on columns that are not their (common) distribution column runs on the groups, by moving the rows of one or more tables to the group that owns the matching rows of the others | 4 to 6 |
| 2 | `MERGE` on distributed, reference and local tables, for the combinations that can be proven correct | 7 |
| 3 | Approximate aggregates (distinct count, top-n, percentiles) as sketch types in a new extension `kingres_sketch`, pushed down with the partial aggregation of 2b section 7, storable in rollup tables, enabled per database from the web console | 8 to 10 |
| 4 | An optional setting that makes `count(DISTINCT x)` approximate | 9.8 |
| 5 | Rollup tables maintained with `INSERT ... SELECT` and `MERGE` | 10 |
| 6 | The control-plane task, the REST additions and the console changes that enable item 3 | 12, 13 |

Not in scope:

- Columnar storage, time partitioning, retention, compression, and the policies
  that drop or compress old rollup partitions: part 2,
  [analytics-phase12-storage.md](analytics-phase12-storage.md).
- Any change to PostgreSQL's executor or planner source. No core patch is
  needed for the three features (section 2.2), apart from one line in a build
  file (section 2.3).
- A global snapshot, distributed deadlock detection, a statement-level retry of
  a `SELECT` after `KS001`: unchanged from 2b.
- Repartitioning with temporary tables on the groups, and group-to-group transfer
  of rows: compared and rejected in section 4.2.
- A repartitioned semi or anti join (`IN`, `EXISTS`): not a pushdown unit in 2b
  (sub-links) and not here.
- Approximate versions of window functions, of `percentile_cont ... WITHIN
  GROUP` (the exact ordered-set aggregate stays exact and is never rewritten),
  and of `count(DISTINCT a, b)` with several arguments.
- `INSERT ... ON CONFLICT DO UPDATE` through the router (2b refuses it). Section
  10 gives the two ways to maintain a rollup without it.

## 1. Decisions in one table

| Question | Decision |
|---|---|
| How is the moved side of a repartitioned join transported? | Through the origin: the rows are read by the ordinary 2a scan (a child plan of the new custom scan), hashed to their destination group at the origin, and sent to each group as **array parameters of the fragment** (`unnest($n::text[], ...)` in a sub-select, the wire format of the row router). Nothing is created on a group. |
| Temporary tables on the groups? | No. A transaction that has touched a temporary table cannot be prepared (`xact.c:2630`, "cannot PREPARE a transaction that has operated on temporary objects"), and the remote transactions of this extension are prepared whenever two nodes write. They also add catalog churn per statement and need an `ANALYZE`. Listed as a later increment for sides larger than the array bound (section 4.2). |
| Group-to-group transfer? | No. It needs a forwarded session to open connections, which widens the forwarded-session model (2b section 15) for a gain that only matters when the origin is the bottleneck. |
| Which join types? | Key-routed sides: every join type with a hash-compatible equality in the join condition. Broadcast sides: inner joins and joins where the broadcast side is not preserved. The existing rules of 2b section 9 for reference tables are the rules for replicated inputs. |
| How is it chosen? | By cost, among the other paths of the same planner run, inside the bounds `repartition_max_bytes` per group. A statement that does not qualify keeps the 2a plan. `EXPLAIN` names the path and the reason it was not used. |
| `MERGE` | Five proven shapes (colocated, routed source, reference target, broadcast source to a reference target, local target with a fenced source); every other shape stays refused with a message that names the missing condition. |
| Approximate aggregates | One new extension `kingres_sketch` (C, PostgreSQL License) with types `hll`, `topn`, `tdigest`, written from the papers. No code of `postgresql-topn` (AGPL-3.0) is read or used. `postgresql-hll` (Apache-2.0) and `tdigest` (PostgreSQL License) are not bundled either, for reasons of format control and one version per cluster, not of licence (section 9.1). |
| Pushdown of sketches | The partial aggregation of 2b section 7, with its rule "built-in aggregates only" widened to "built-in, or a member of an extension on a fixed list at a version the other nodes also run". |
| Who enables them | `kingres_admin`, per database, from the web console: a task that installs `kingres_sketch` on every server, verifies that the versions and the hash agree, and records the setting. |
| Rollups | `INSERT ... SELECT` that is colocated (2b P6, widened to allow `DO UPDATE` for a whole-statement push) or `MERGE` with a routed source. |

## 2. Mechanism in PostgreSQL 18 terms

### 2.1 Hooks and APIs used

Everything is built on the interfaces 2b already uses; this phase adds no new
hook.

| Need | Interface | Used for |
|---|---|---|
| Analysis of a statement before planning | `planner_hook` (`kingres_shard_planner_hook`), on the `Query` as the rewriter produced it | The repartition plan (`RepartPlan`) is part of `PushdownInfo`; the `MERGE` classification; the rewrite of `count(DISTINCT)`; the rewrite of the source of a `MERGE` into a local table. |
| A join of the whole level, aggregates, final stage | `set_join_pathlist_hook`, `create_upper_paths_hook` (2b 2.1) | A new path P7 (repartitioned join) next to P2 to P5; P3 to P5 use the repartitioned fragment when P7's plan is chosen. |
| Reading the shipped side | `CustomPath.custom_paths` / `CustomScan.custom_plans`: child paths and plans of a custom scan (`createplan.c:4304` builds `custom_plans` from `custom_paths`; `execnodes.h:2131` has `custom_ps` for the child states) | The shipped relation is read by its own ordinary path, the 2a scan, as a child of the new custom scan. All 2a semantics (restrictions shipped and rechecked, pruning, the placement check) are reused unchanged. `nodeCustom.c` does not mention `custom_ps`, so the provider starts the children itself in `BeginCustomScan` (**[Needs verification]**: read `ExecInitCustomScan` while implementing). |
| Running `MERGE` on a group | The write fragment of 2a (`kingres_shard_make_fanout_plan`) and the insert fragment of 2b 11.3 | The `MERGE` fragment kinds (section 7). |
| Splitting an aggregate | `AGGSPLIT_INITIAL_SERIAL` / `AGGSPLIT_FINAL_DESERIAL`, `mark_partial_aggref`, 2b 7.3 and 7.4 | Sketch states travel as `bytea` produced by the aggregate's `serialfunc`. |
| Sketch aggregates | `CREATE AGGREGATE ... COMBINEFUNC, SERIALFUNC, DESERIALFUNC, PARALLEL = SAFE` (`create_aggregate.sgml`) | A state of type `internal` takes part in partial aggregation only with a serialization pair (the documentation says so in the `SERIALFUNC` entry and in the parallel paragraph). |

### 2.2 Is a core patch needed?

No. The rewrite of a statement tree, the custom scans, the aggregates and the
types are all reachable from an extension. The fork policy ("write an extension
first", `core-patches.md` rule 1) is kept.

### 2.3 The one build-file change

`contrib/meson.build` lists the contrib directories that are built (KP-0003 added
`kingres_ledger` and `kingres_shard`). `kingres_sketch` needs one more
`subdir('kingres_sketch')` line there. This is a change to a file that exists in
upstream PostgreSQL, so it needs its own register row in
[core-patches.md](../dev/core-patches.md): **KP-0004** (the next free number; the
register has KP-0001 to KP-0003), purpose "builds `kingres_sketch` with the
tree", reason "the list of contrib modules is a core build file", test "the meson
suite `kingres_sketch`", upstream status "local only". The register's row is
added in the same commit as the line.

## 3. Common machinery added to 2b

### 3.1 The shipped side: a sub-select over `unnest`

A relation of the query that is not read where its rows are stored is **shipped**.
In the fragment text it is replaced by a sub-select with the same alias and the
same column names, so that every `Var`, join clause, `GROUP BY` and select-list
expression of the text stays valid:

```
(SELECT u.c1::pg_catalog.int8 AS id, u.c2::pg_catalog.text AS name, NULL::pg_catalog.date AS d3
   FROM pg_catalog.unnest($4::pg_catalog.text[], $5::pg_catalog.text[]) AS u(c1, c2)) AS ks_t2
```

- The text is made by replacing the `RTE_RELATION` of the copy `pd->raw` (2b 4.1)
  by an `RTE_SUBQUERY` at the same range table index, then deparsing the copy with
  `pg_get_querydef` under the transmission modes. The sub-select produces every
  attribute number that any `Var` of the copy uses, in the attribute order of the
  table; attributes that no `Var` uses are `NULL::<type>` constants, so
  attribute numbers stay valid. **[Needs verification]** (2b V3 and V13 apply):
  that the deparse prints this shape for every case of the tests.
- The arrays are `text[]` parameters numbered after the statement's external
  parameters and the values of 2b 5.2, one array per shipped column, built from
  the text form of the values under the transmission modes. This is the wire
  format of the row router (2b 11.3), including its reason: the text form is
  what every other statement of the extension relies on, and `array_out` quotes
  anything that needs it. A `NULL` element is a `NULL`.
- Only columns whose type is built in (2b 5.1 rule 2) are shipped. A column of an
  extension type (a `kingres_sketch` type) may be **carried** as a value when the
  type is on the allowed list (3.3), because the text form of those types is
  fixed by this design and the type exists on every node; it may not be the
  argument of an expression evaluated on a group unless the function is
  allowed (3.3).

### 3.2 Markers and what a group checks

The marker of 2b 4.2 gets optional keys:

```
/* kingres:frag v=2 kind=<query|partial|insert|merge> map=<V> rp=<n> ext=kingres_sketch/<version> */
```

`rp=<n>` is the number of shipped sides in the statement (0 or absent: none).
`ext=` is present when the statement uses an object of an allowed extension
(3.3). The group's checks of 2b 4.2 are extended, and a statement that fails any
of them raises `KS001` with a detail that names the rule:

- rule 1 ("no sub-selects"): a sub-select is accepted only if it has exactly the
  shape of 3.1 (the only range table entry is `pg_catalog.unnest` of
  `text[]` parameters, the target list is casts of its columns and `NULL`
  constants, no `WHERE`, no other clause), and the count of such sub-selects
  equals `rp`;
- rule 2: every other relation is a distributed table of one colocation group
  or a reference table (unchanged); a statement whose every relation is shipped or
  replicated is refused (there must be a distributed table to be read);
- rule 3: the shippability walker applies to the whole statement, including the
  allowed extension functions (3.3);
- rule 4: the placement check of phase 4, as for every fragment (section 6.4);
- new rule 5: `ext=` names an extension of the fixed list whose installed version
  on this node equals the version in the marker. A mismatch raises `KS001` with
  the detail "extension kingres_sketch is at version X here and Y at the
  origin". This is not retryable by repeating the statement; the detail says so
  and the control plane checks versions (section 12).

Markers stay inert comments: a session is forwarded only with the token of
`forward_auth.c`, and the receiving planner hook re-checks the statement instead
of trusting the marker (2b 4.2).

### 3.3 Widening the shippability rule for allowed extensions

2b 5.1 rule 1 requires functions and operators to be built in
(`oid < FirstGenbkiObjectId`), and 2b 7.2 requires the aggregate to be built in.
This phase adds an **allowed extension list** compiled into `kingres_shard`:
today `kingres_sketch` only. An object (function, aggregate, type, cast,
operator) is allowed when it is a member of an allowed extension
(`getExtensionOfObject`, `catalog/pg_depend.h`, then the extension's name) **and**
the extension's installed version is one of the versions that the compiled-in
list accepts (the versions that share one state format). The conditions that
make a function of such an extension shippable are the same as for a built-in
one: it is `IMMUTABLE` (aggregates: partial-capable per 2b 7.1), the types of
its arguments and result are built in or allowed, and its result does not depend
on the session (this is a property that the extension's code and tests must
have: no function of `kingres_sketch` reads a setting that is not fixed by the
transmission modes; section 9.2).

Nothing in `kingres_shard` calls an extension function it finds by name except
the planner rewrite of 9.8, which looks up `kingres_sketch.approx_count_distinct`
by a fixed qualified name and does nothing when it is not installed.

Why not "any immutable function of any extension": a user-defined or third-party
function is not known to exist, at the same version and with the same meaning,
on the other nodes (2b 5.1 gives the same reason for built-ins only). The list
is code and is reviewed. **[Recommendation]** keep the list in code in this
phase; a catalog table that the administrator fills would let any user with the
privilege to create an extension widen what runs on other nodes.

## 4. Repartitioned joins

### 4.1 What a repartitioned join is

Phase 2b pushes a join only if all distributed tables of the query are in one
key class (2b 6.3): equated on their distribution columns, same colocation
group. A join `orders o JOIN events e ON e.user_id = o.user_id` where `orders` is
distributed by `customer_id` and `events` by `event_id` has no such class. Today
it is planned by the 2a scan: both tables are read through the origin, whole.

A repartitioned join keeps the larger relations where they are, ships the others
to the groups that need them, and runs the join, and everything 2b can push
above it, on the groups. Each relation of the statement gets one **role**:

| Role | Meaning | Rows of the relation on a group `g` |
|---|---|---|
| **Anchored** (A) | read where stored; the relations of one key class (2b 6.3) are anchored together | its own buckets |
| **Routed** (M, "moved by key") | shipped, each row to exactly one group, chosen by hashing the join column | the rows whose join key maps to `g` |
| **Broadcast** (B) | shipped, every row to every group | all rows |
| **Replicated** (R) | a reference table, already on every group | all rows (its local copy) |

The join key of a routed relation is a column of it that belongs to the **key
component** of the statement (4.3): it is equated, through inner-join equalities
or the `ON` equality of an outer join, with the distribution column of the
anchored class, or with the key of another routed relation of the same
component.

Variants, all produced by the same analysis (4.3):

- **Single repartition.** One anchored class, the other distributed relations
  routed (or broadcast) to it. The anchored tables do not move; routed rows go to
  the group that owns, in the anchored class's colocation group, the bucket of
  their key.
- **Dual repartition.** No relation is anchored (no key class of two or more
  tables, or the planner finds it cheaper): every distributed relation of a key
  component is routed by its join column to a synthetic placement (4.4).
- **Broadcast join.** One side (small) is sent to every group and joined with the
  anchored side by any condition, including conditions that are not equalities.

### 4.2 The mechanisms compared

| | Broadcast of a small side | Redistribution through temporary tables on the groups | Redistribution through the origin (**chosen**) | Group-to-group transfer |
|---|---|---|---|---|
| What happens | The small side is sent to every group | A temporary table `ks_rp_<n>` is created on each group and filled; the fragment names it | Rows go group to origin to group; the group gets them as parameters of the fragment | Each group sends its rows of the moved side to the destination groups directly |
| Join types | Any where the broadcast side is not preserved | All | All | All |
| Join condition | Any | Equality on the key | Equality on the key (broadcast: any) | Equality on the key |
| Bytes moved | `size x (1 + G)` for a distributed small side (one read, `G` sends) | `2 x size` | `2 x size` (broadcast: `size x (1 + G)`) | `size` (no hop through the origin) |
| Memory | Per group: the whole side | Per group: its share, spills to disk | Origin: tuplestore per destination (`work_mem`, then disk); group: its share as a parameter array, bounded by `repartition_max_bytes` | Per group: its share, plus buffers |
| Transaction | The remote transaction is unchanged | **Cannot be prepared** after a temporary table is touched: `PrepareTransaction` raises "cannot PREPARE a transaction that has operated on temporary objects" (`xact.c:2630`); the flag is set when a temporary relation is opened (`relation.c:73`, `:123`). A group that wrote earlier in the transaction and then used a temporary table would fail at commit | The remote transaction is unchanged; read-your-writes holds, and a group that wrote earlier can run the fragment | Needs a connection group to group, which a forwarded session never opens |
| Snapshot | One snapshot per fragment | Statement snapshots of the creating statements; extra round trips | The shipped side is read at the origin's reading time of the child scan; the fragments see their groups at their own statement or transaction snapshot (4.8) | As above |
| Planner statistics on the group | None for the unnest; the row count is known from the parameter | None until `ANALYZE`; a second statement per group | The row count is known (the array length of the custom plan's parameter); the column statistics of the shipped side are not (the planner uses defaults) | None |
| Catalog churn | None | A `pg_class`/`pg_attribute` row set per statement per group | None | None |
| New trust | None | A fragment kind that creates objects | None | The group must hold the cluster secret and act as an origin |

Decision: **redistribution through the origin, with the moved rows carried as
array parameters**, for both key-routed and broadcast sides. Reasons, in order:
it is the only mechanism that does not widen the forwarded-session model and does
not block two-phase commit, and it reuses, unchanged, two pieces that 2b builds
and tests (the 2a scan as the reader, the wire format of the row router as the
transport). Its cost is that every moved row crosses the origin twice and that
the group's share of a moved side must fit in `repartition_max_bytes` (default
64 MB, section 11), so two large tables with no usable key class still use the
2a scan.

Rejected for this phase, with what would reopen the question:

- *Temporary tables.* Would lift the size bound. A variant that avoids the
  prepare problem is a dedicated read-only session per (user, node) that holds the
  temporary tables and runs the fragment; it would not see the main remote
  transaction's earlier writes, so it could be used only when the transaction has
  written nothing on any node. Recorded as a later increment (section 16); the
  size bound is the price of not having it.
- *Group-to-group.* A forwarded session "never forwards again" (README, "How it
  works in one page"). Reopen if a measurement shows the origin's network link is
  the limit for a workload that the bound admits.
- *Inline `VALUES` instead of array parameters.* The text of the statement would
  contain row values, which the rule of 2b 15.3 forbids ("nothing a row contains
  is ever part of the text").

### 4.3 Eligibility: the key component and the roles

The analysis extends 2b 6.1 and 6.3 and runs in `planner_hook_body` on a
statement that 2b classifies as a candidate for pushdown. A statement is a
**repartition candidate** when 2b 6.1 rules 1, 2, 3, 6 and 7 hold (top-level
`SELECT`, no sub-links, CTEs, sub-selects, set operations, row locking; tables
only; distributed tables and reference tables, at least one distributed; no row
security; no grouping sets) and rule 4 (one key class) does **not** hold, or holds
and the planner still finds a repartitioned plan cheaper (rare; allowed). All the
conjuncts of the statement must be shippable (5.1 of 2b), with no exception for
conjuncts left at the origin (that exception of 2b 5.4 is not available here).

**Join equalities.** An equality conjunct `x = y` is a *key equality* when both
sides are bare `Var`s (a `RelabelType` looked through), their types are the same,
the operator is the equality operator of a hash operator family (so a hash
function exists with the property that equal values hash equal), the input
collation is the default collation or `C` (deterministic), and the conjunct is in
the `WHERE` clause or in the `ON` clause of an inner join, or in the `ON` clause
of the outer join whose two arms contain the two variables, and **neither
variable is on the nullable side of an outer join that is below the conjunct**
(a stricter reading than 2b 6.3, which is needed because the join here is
formed after rows have been moved: a null-extended value is not a value that
was hashed). **[Recommendation]** types are the same type (`int4 = int8` is not a
key equality in this phase); cross-type equalities are listed in section 16. The
hash function used is the one the operator family gives for that type
(`get_opfamily_proc(..., HASHEXTENDED_PROC)`), the same function PostgreSQL uses
for hash partitioning of that type, so that routed rows hash as the rows of an
anchored table of the same key type do.

**Key component.** The connected components of the graph whose nodes are the
`Var`s that occur in key equalities and whose edges are the key equalities. A
component that contains the distribution column of an anchored class is the
*anchor component*. A relation can be routed by any of its columns that belongs
to a component.

**Role assignment.** Each candidate plan is an assignment of roles to the relations
of the query. The planner enumerates, for each distributed key class `K` of 2b
6.3 (a class may be one table), the plan "anchor `K`, route or broadcast every
other distributed relation, replicate the reference tables", and the dual plan for
each component, and keeps those that pass these rules (a relation with no
suitable column or an unsuitable join type makes the assignment invalid):

1. **Anchored relations** are exactly the members of one key class `K`.
2. A distributed relation outside `K` is **routed** if one of its columns is in the
   anchor component of `K`, or, in the dual plan, in the component that is being
   used. If it has no such column it can only be **broadcast**.
3. A relation may be **broadcast** only if for every outer join in the statement
   the broadcast relation is not on the preserved side of a join whose other side
   contains a partitioned (anchored or routed) relation. This is the rule of 2b
   9.1 for reference tables ("`R LEFT JOIN D` is not pushed"): a preserved
   replicated side would be emitted once per group. An inner join with a broadcast
   side may use any condition; an outer join whose broadcast side is the nullable
   side may also use any condition.
4. A relation may be **routed** in every join position: its rows go to one group,
   so a preserved routed relation is emitted once. A routed relation whose key is
   `NULL` cannot match; the origin sends such a row to the group of the bucket that covers
   hash slot 0 (the group that holds `NULL` keys for the anchored class: a `NULL`
   key hashes to 0, as 2b 11.6 says), exactly once, if the relation is preserved by an
   outer join, and drops it at the origin if it is not (inner join, or the
   nullable side of an outer join), because it can never be emitted.
5. For every outer join that contains a routed or anchored relation on both
   arms, the key equality of 6.3 must be in that join's `ON` clause (J5 of 2b).
6. If the statement has no anchored relation (dual plan), every distributed
   relation must be routed; none is broadcast unless it is small enough to be
   replicated by rule 3, and at least one relation is routed.

The result is a list `RepartPlan` = for each shipped relation: its range table
index, role, key column (routed), destination scheme (4.4), and the child path
that reads it. It is stored in `PushdownInfo` (2b 4.1).

**Closed queries (2b 6.4).** The definition of "closed" is applied with the
*partition key component*: a `GROUP BY` or `PARTITION BY` that contains a bare
`Var` of the anchor component (any relation's column in it, not only the
anchored table's distribution column) and not on the nullable side of an outer
join. For a routed relation the column's rows have been placed by the same hash,
so its group has all rows of one value. P4 and P5 may therefore sit on top of
P7. A statement with a broadcast relation and `GROUP BY` the key of the anchored
class is closed by the same rule (the broadcast relation does not change which
group holds a key's rows).

### 4.4 Where a routed row goes

The destination of a routed row is a function of the key value and of a
**placement scheme**:

- *Anchored scheme (single repartition).* The scheme is the colocation group of
  the anchored class: the key's hash is resolved through the **catalog's bucket
  lookup** (the routing array over `colocation.max_modulus` built from
  `bucket_map`, [shard-phase10.md](shard-phase10.md) sections 1 and 2.2), so that
  each bucket's own `modulus` is used when a bucket has been split in place, and
  `group` is the group that the placement the planner used gives that bucket. The
  expression `hash % bucket_count` is never used. This is the same lookup as the
  row router (2b 11.6, which phase 10 changes in the same way). The shipped rows are therefore exactly the
  rows that a table of that colocation group, distributed by this column, would
  hold on each group. This is what makes the join local on the group.
- *Synthetic scheme (dual repartition).* `group = groups[hash(key) % n]`, where
  `groups` is the list of the group ids that own at least one bucket of any table
  of the statement, sorted ascending, `n` its length, and `hash` the extended hash
  function of the key's operator family with seed 0, taken as `uint64`. Every
  routed relation of the statement uses the same list and the same function, so
  rows with equal keys meet. The list is part of the plan and is checked at
  execution (4.8).
- *Broadcast.* every group of the statement's group set (4.8).

The group set of the statement is the set of groups that own a bucket of an
anchored table (after pruning, 2b 4.5), or, in a dual plan, the list above. A
routed row whose destination is not in the group set cannot exist in a single
repartition (the destination is a group that owns the bucket, so it is in the
colocation group's set); pruning of the anchored side (`k = 5`) shrinks the set
and the routed rows are filtered by the same bucket test, so rows that belong to
a pruned bucket are never sent.

### 4.5 The fragment, the custom scan, and what the origin does

The path P7 "Kingres Fragment Scan" with `Pushed: join (... repartitioned)` has
child paths: for each shipped relation, the cheapest ordinary path of the
relation (`rel->cheapest_total_path`), which is the 2a distributed scan with all
its restrictions, pruning and rechecks. Its fragment is `pd->raw` with the
shipped relations replaced as in 3.1. At execution:

1. `BeginCustomScan` starts the children. For each shipped relation, the child
   returns rows (only the columns the text uses, plus the key column), and the
   scan reads it **to the end before any fragment is started**: rows are placed
   in one tuplestore per destination group (`tuplestore_begin_heap` with the
   share of `work_mem` described in 4.6) by the destination function of 4.4, or in
   every group's store for a broadcast side. This is a blocking phase; it is
   stopped on an interrupt, and every cursor of the child is closed at the end or
   on error as `ds_stop` does today.
2. The text form of the rows is made with the transmission modes and built into one
   `text[]` literal per column per destination group. The size of each group's
   literals is counted, and the bound of 4.6 is applied before any fragment is
   sent.
3. The fragments are started on all groups concurrently, as in 2b 4.4: remote
   groups by `PQsendQueryParams` of the `DECLARE` with the external parameters, the
   2b 5.2 values and the arrays; the origin's own part through SPI with
   `SPI_cursor_open_with_args(..., read_only = true)` and the same parameters.
4. Results are read in batches of `scan_fetch_size` rows per group (2b 4.4); the
   aggregate and sort variants P3, P4, P5 above the fragment behave as in 2b.
5. A rescan restarts the fragments with the same arrays (they are kept in the
   scan's memory context until the end of the statement) and does not read the
   children again. This costs memory for the duration of the statement; a plan
   that would rescan (a nested loop above it) is possible only for paths
   without parameters, which is all that 2b creates.

### 4.6 Memory and size bounds

Let `G` be the number of groups in the group set, `S` the total text size of
the shipped rows (key-routed: each row once; broadcast: each row `G` times).

- **At the origin.** The tuplestores hold at most `work_mem` in memory in total
  (the memory is divided evenly among the stores at start and spills to temporary
  files beyond that); the `text[]` literals are then built group by group and
  sent, so at most `G` literals of at most `repartition_max_bytes` exist at the
  same time, plus libpq's own copy of each (a `PQsendQueryParams` call copies
  into the output buffer). The origin's peak is bounded by
  `work_mem + 2 x G x repartition_max_bytes` plus the fetch batches of the
  children. This is not independent of the data size: it is bounded by the
  setting, and the statement fails before sending when the bound is exceeded.
- **At a group.** The parameter value and its detoasted array exist in the
  backend's memory for the whole fragment (a parameter is not spilled): about
  `2 x` the group's share. The join above is bounded by that group's `work_mem`
  as any join is.
- **The bound.** `kingres_shard.repartition_max_bytes` (default 64 MB, 1 MB to
  512 MB; 512 MB is under the 1 GB limit of one allocation in PostgreSQL's memory
  contexts, which a single `text` literal cannot exceed; **[Needs verification]**
  `MaxAllocSize` applies to the libpq output buffer and to the `text[]` datum at
  the group, so the largest safe literal is below 1 GB, and the limit of the
  protocol message length) applies to the literals of one group of one shipped
  relation, summed over the relation's columns.
- **Plan-time bound.** The planner estimates the bytes per group from
  `rel->rows`, `rel->reltarget->width` and `G` (key-routed: `S / G`; broadcast:
  `S`; times 1.5 for the text form), and does not create P7 when the estimate
  exceeds the bound. `EXPLAIN (VERBOSE)` says so (4.9).
- **Run-time bound.** An estimate can be wrong (skew: one key holds half the
  rows and its group gets half the side). When the actual size of a group's
  literals exceeds the bound, **before the first fragment is sent**, the
  statement fails with `54000` (`program_limit_exceeded`), "repartitioned join:
  the share of group N of <table> is X bytes, above kingres_shard.repartition_max_bytes
  (Y); the plan estimated Z", with the hint "set kingres_shard.enable_repartition_join
  = off to read the tables through the scan, or raise the setting". This is the
  one place in this phase where a `SELECT` raises an error instead of falling
  back to the scan, because the statement has no way to change its plan once the
  executor runs; ADR 0022 point 2 covers conditions that cannot be *proven* at
  plan time, and this is the same kind of limit as `statement_timeout`.
  **[Recommendation]** keep it, and tell users in the README that the setting
  that switches the path off is the remedy. A later increment can make the scan
  node able to run the 2a path when the bound fails (it holds the child paths
  that read the same tables), see section 16.

### 4.7 How the planner decides, by cost

P7 is a `CustomPath` added with `add_path` on the join relation of the whole
level (and, for P3 to P5, on the upper relations), next to P1, P2 and the others.
The planner takes the cheapest by `total_cost` (and startup cost for
cursors, as always). The cost of P7 uses the constants of the 2a scan
(`dist_scan.c`: a remote row costs `cpu_tuple_cost * 2 + 0.02`, a remote group
`100` of startup) and adds the cost of the second hop:

```
fetch(r)   = rows(r) * (2 * cpu_tuple_cost + 0.02) + 100 * groups_read(r)   -- the 2a cost of r
send(r)    = rows(r) * copies(r) * (cpu_tuple_cost + 0.02) + 100 * G        -- build, hash, transmit
             copies(r) = 1 for a routed relation, G for a broadcast one
group_work = cost of the cheapest local join path of the fragment, divided by G
             (the planner's own estimate on the origin's statistics, as 2b 4.4)
return     = rows(result) * (2 * cpu_tuple_cost + 0.02)                     -- the fragment's rows come back
cost(P7)   = sum over shipped r of (fetch(r) + send(r)) + group_work + return
```

The anchored relations contribute only through `group_work`: they are not moved.
`rows(r)` and `width(r)` are the planner's estimates after the restrictions of
`r`; the shipped sides therefore get cheaper with selective filters, which is the
common case in analytics. For P3 (partial aggregate) `return` is
`groups(result) x G` partial rows, so P7 with aggregates on top wins when the
result is much smaller than the inputs, which is the situation in which the 2a
scan loses most. When the result is as large as the inputs, `return + fetch + send`
exceeds the scan's cost and the scan stays. The constants are the same for all
paths and are tuned by test (2b V10): they are kept in the expected files of
`pushdown_plans`, and the tuning is a step of the implementation order.

A statement planned from a generic plan (a prepared statement) uses the
estimates without the parameter values; the plan stores the role assignment, and
the run-time bound of 4.6 protects it. Cached plans depend on the map (2b 10.4).

### 4.8 Correctness conditions (the repartitioned join)

- **R1 (co-location).** Every output row of the fragment on group `g` is formed
  from rows that, under the key equalities of the statement, are the rows with
  equal key values; those rows are all on `g`: anchored rows because the bucket of
  their key maps to `g` in the placement the plan used, routed rows because the
  same hash and placement sent them to `g`, broadcast and replicated rows because
  they are on every group. Hence the union over groups of the fragment results
  is the join result with no row repeated or missing (J1 of 2b).
- **R2 (exactly once).** A routed row is sent to exactly one group, a broadcast row
  to every group of the set, a `NULL`-key row of a preserved routed relation to
  one group, and a `NULL`-key row of any other routed relation to none (it can
  match nothing). A row of a routed relation that is read twice by the child
  (the child is a scan with the placement check, which guarantees each bucket is
  read once, 2b 10.2) is therefore sent once.
- **R3 (outer joins).** Rule 3 and 4 of 4.3 give: the preserved side of every
  outer join is partitioned (anchored or routed) or both sides are replicated; a
  null-extended row is produced by the group that holds its preserved row. A
  `FULL JOIN` of two routed relations is correct because both sides' unmatched
  rows live on the group of their key (J5).
- **R4 (the same hash).** The hash function and the equality operator belong to
  the same operator family, the key types are identical, and the collation is
  deterministic and the default or `C`. Two values that the equality calls equal
  hash equal. For a `float8` key `-0` and `0` are equal and the type's hash
  function treats them alike (**[Needs verification]**: `hashfloat8extended`
  normalizes `-0`; a test with `-0`, `NaN`, infinities settles it).
- **R5 (values survive the trip).** Every shipped column is of a built-in type
  and crosses as text under the transmission modes, then is cast back by the
  input function of the same type on the group. This is the router's rule
  (2b 11.3); the hostile-value test of 2b 16.4 runs over this path.
- **R6 (all expressions shippable).** Every expression of the fragment passes 2b
  5.1 and 5.2; there are no conjuncts left at the origin.
- **R7 (the size bound).** The literal of one group of one shipped relation is
  at most `repartition_max_bytes`, checked before the first fragment is sent
  (4.6).
- **R8 (bucket moves).** See 4.10.
- **R9 (snapshot).** See 4.9a below.
- **R10 (equivalence).** The answer of a statement planned with P7 equals the
  answer of the same statement planned with the 2a scan, up to the tolerances of
  2b A3 (float sums) and the unspecified order of rows. Tested in `060` (section
  14).

### 4.9 Snapshot and transaction behaviour

**4.9a Snapshot.** Nothing here creates a global snapshot.

- The shipped relation is read by its child scan when the custom scan starts, in
  the origin's transaction and in the remote transactions of the groups that hold
  it, each at its statement snapshot (`READ COMMITTED`) or transaction snapshot
  (`REPEATABLE READ`, `SERIALIZABLE`, where the remote transaction takes its
  snapshot at its first statement and every later statement reuses it, 2b 10.1).
- The anchored side is read by the fragment, one statement per group, later.
- In `REPEATABLE READ` every group is seen in one state by both reads. In
  `READ COMMITTED` a group that holds both a shipped table and an anchored table
  is read in two statements and can be seen in two states: this is the anomaly
  S2-09 of the 2a README ("one statement that scans two distributed tables sees
  the same node in two different states"). A repartitioned join is therefore as
  consistent as the 2a scan, and weaker than the pushed join P2, which reads all
  its tables in one statement per group. The README says so, and **[Recommendation]**
  the planner does not prefer P7 over P2: P2 exists only when one key class
  covers the statement, and then P7 is not created unless the planner finds it
  cheaper, which it cannot be (P7 includes all the cost P2 has plus shipping).
- Between groups the skew of 2a remains.
- Read-your-writes: the child scan and the fragments run in the same remote
  transactions that hold the transaction's earlier writes, the origin's part
  through SPI in the local transaction, so a transaction sees its own writes.
- Two-phase commit: nothing is created on a group, no connection is marked a
  writer by this path, so the commit protocol is unchanged. A group that wrote
  earlier in the transaction can run the fragment (this is the reason for 4.2).
  The SPI use at the origin does not create temporary objects.

**4.9b Errors.** An error on a group is an `ERROR` at the origin and aborts the
transaction (2b 4.6). A cancel closes the child cursors and discards the batch in
flight. A statement that is cancelled during the shipping phase has sent
nothing to any group.

### 4.10 A bucket that moves during the statement

Two placements are involved: the placement of the anchored class's colocation
group (it decides the destination of routed rows and which buckets the fragment
reads) and the placement of the colocation groups of the tables read by the
children (the scan's own check, 2b 10.2, implemented in phase 4 as the
comparison of the placement before and after the cursor opens, README
"What a move blocks"; phase 10 replaces the placement hash by the monotonic
`colocation.placement_epoch` (section 7.1 of [shard-phase10.md](shard-phase10.md)),
because a bucket that moves away and back restores a hash but not an epoch). This
note says "the placement check" and means whichever of the two the committed code
has; the argument below needs only that the check fails when a switch or a split
of the colocation group committed between planning and the fragment.

- The destination map used for routing is the placement the plan was made with
  (`P_plan`, from the origin's catalog cache at planning time). The fragment on
  each group runs the phase 4 check against `P_plan` (the group's own placement
  before and after its `DECLARE` equals `P_plan`) and raises `KS001` otherwise,
  before any row is returned. If all groups pass, every group serves exactly the
  buckets `P_plan` gives it, and every routed row was sent to the group that,
  under `P_plan`, owns its key's bucket: R1 holds.
- A move that commits between the shipping and the fragment makes a group's
  placement differ from `P_plan`: `KS001`, no rows, the origin reloads its catalog
  (as for any `KS001`), and the client repeats the statement.
- A move that commits during the read of a shipped relation is handled by the
  child scan's own check: `KS001` from the child, before the first row is
  returned from the custom scan.
- A **split** (phase 10) changes the placement of the colocation group without
  changing any bucket's group: the children stay on the parent's group, so the
  destination of a key is the same group, but the partitions a group has are not.
  The epoch increases at the split's switch, so a plan made before it fails the
  check, which is required: the partition list the fragment reads and the lookup
  array the routing used must come from one epoch.
- The dual scheme (4.4) does not depend on a colocation placement, only on the
  group list; a change of the set of groups (`add_node`, `remove_node`) changes
  the catalog's map version, which the fragment's marker carries as `map=V` and
  which 2b 4.3 checks after planning (**[Needs verification]**: 2b 10.2 uses the
  map version `V`; the README of phase 4 says the scan compares a placement hash
  (phase 10: an epoch) because the version counts every catalog change. Which of the two the dual scheme
  uses must be settled when 2b step 1 is written; this note needs one rule that
  fails a statement planned with a different set of groups, and either of them
  does it).
- Liveness is as in phase 4: a read transaction that holds the tables on the
  source group holds the switch back, and the switch gives up and retries.
  A repartitioned join holds its remote transactions as long as the origin's
  transaction lives, as every scan does.

### 4.11 Reference tables, ledger tables, standbys

- Reference tables are replicated relations (R). A reference table is also a legal
  shipped relation? No: it is never shipped; it is already everywhere. A
  reference table that is the only relation on the preserved side of an outer
  join with a partitioned relation keeps the refusal of 2b 9.1.
- A ledger table cannot be distributed or a reference table (2b 11.8); the
  statement can read one only as a local table, which is not a pushdown unit. A
  repartitioned join never touches a ledger table.
- On a hot standby that is the origin, the statement plans and scans as on its
  primary (README, "Functions"); P7 needs no write and no catalog reload, so it
  works. A standby of a group is not used for the fragments (2b 10.3).

### 4.12 `EXPLAIN`

```
Custom Scan (Kingres Fragment Scan)
  Pushed: join (orders, events, users_ref: repartitioned)
  Repartition: events by user_id -> placement of orders (colocation 1, 128 buckets), est 1200000 rows, 96 MB; orders anchored; users_ref replicated
  Groups: 3 of 3 (local + 2 remote)
  Remote SQL: SELECT ks_t1.id, ks_t2.kind FROM public.orders AS ks_t1 JOIN (SELECT u.c1::pg_catalog.int8 AS user_id, ... FROM pg_catalog.unnest($1::pg_catalog.text[], ...) AS u(c1, ...)) AS ks_t2 ON (ks_t2.user_id = ks_t1.user_id)
  ->  Custom Scan (Kingres Distributed Scan) on events ks_t2
        Remote groups: 3 of 3
        Remote SQL: SELECT user_id, kind FROM public.events AS ks_t WHERE (...)
```

Routed: `by <column> -> placement of <anchor> (...)` or `-> synthetic placement (n groups)`;
broadcast: `Repartition: dim_regions broadcast (to 3 groups), est 4000 rows, 0.3 MB`.
`EXPLAIN (VERBOSE)` on a statement that did not get P7 adds one line:
`Repartition: not used: <reason>`, with the first failed rule: "no key equality
joins events to a column of the anchored class", "a join is not an equality on
a hash-compatible type", "the preserved side of the join with users_ref would be
replicated", "estimated 800 MB per group exceeds kingres_shard.repartition_max_bytes",
"cost: the scan is cheaper (X against Y)", or "switched off by
kingres_shard.enable_repartition_join". `EXPLAIN ANALYZE` adds per shipped
relation the rows read, the rows sent per group and the bytes sent per group.
Layouts are fixed by the regression test `repartition_plans`.

## 5. Settings of the repartitioned join

| GUC | Default | Context | Meaning |
|---|---|---|---|
| `kingres_shard.enable_repartition_join` | on | `USERSET` | Switches P7 (routed and broadcast sides) off. Off: the 2b plan shapes (P1 to P5). Requires `enable_pushdown`. |
| `kingres_shard.repartition_max_bytes` | 64MB | `USERSET`, 1MB to 512MB | The most text bytes of one shipped relation for one group (4.6). |

Both only choose among plans that give the same answer, or fail a statement that
would not have been planned with them, so ordinary users may set them; they grant
nothing. They are listed in the `security` regression test with the other GUCs
and defined with `MarkGUCPrefixReserved`, as 2b 13 requires.

## 6. What a repartitioned join does not do

- A statement with a sub-select, view, CTE, set operation, window function not
  partitioned by the anchor component, `LATERAL`, semi or anti join, or a local
  table is not a pushdown unit (2b 6.1): the 2a scan.
- Two distributed relations of different colocation groups that are joined on
  their distribution columns are *two key classes*; P7 treats each class as a
  candidate anchor and routes the other. This is the common case of a join of
  two colocation groups and is covered.
- A join on an expression (`a.x + 1 = b.y`), on a different type, or on a
  non-deterministic collation: no key equality, so no routed side; a small side
  can still be broadcast when the join type allows it.
- A broadcast of a relation whose size estimate exceeds the bound: not created.
- A group that owns no bucket of the anchored class is not asked (2b 4.5).

## 7. `MERGE` on distributed tables

### 7.1 What PostgreSQL does, and why `MERGE` is refused today

`transform_MERGE_to_join` (`prepjointree.c:191`) turns `MERGE INTO t USING s ON c`
into a join of `t` and `s` on `c`: inner when only `WHEN MATCHED` actions exist,
left when `NOT MATCHED BY SOURCE` exists, right for `NOT MATCHED [BY TARGET]`, full
for both, and the actions are executed by `ModifyTable` per joined row. The
statement has one target; the actions are `UPDATE`, `DELETE`, `INSERT` and `DO
NOTHING`, with conditions, and `RETURNING` with `merge_action()`
(`MergeSupportFunc`, `primnodes.h:642`). Two rules matter here:

- A target row that matches several source rows is an error ("MERGE command
  cannot affect row a second time"); this is checked per statement, so a
  distributed execution must give all the source rows of one target row to **one
  statement**.
- A relation of the statement that is not the target gets a row mark. For
  `UPDATE`, `DELETE` and `MERGE`, a relation that is a table gets
  `ROW_MARK_REFERENCE`, which needs its `ctid`, and a relation that is not a table
  (a sub-select, a function, `VALUES`) gets `ROW_MARK_COPY`, a copy of the whole
  row (`planner.c:2741`, "If it's not a table at all, use ROW_MARK_COPY"). The
  distributed scan cannot give a `ctid` of a row on another node, so 2a refuses
  with "UPDATE, DELETE and MERGE that read distributed table ... (FROM, USING) are
  not supported" (`dist_scan.c`, `ds_error_system_columns`). Reference-table
  targets are refused by `plan_reference_write` ("MERGE into a reference table is
  not supported yet") and a `MERGE` whose single target bucket lives on another
  group by `planner_hook_body` ("MERGE on distributed tables of another shard
  group is not supported yet"). `query_ok` in `shippable.c` returns false for a
  `MERGE`.

### 7.2 The matrix

| Target | Source | Shape | Path | Decision |
|---|---|---|---|---|
| distributed | distributed tables of the target's key class (joined to the target by a key equality in `ON`), and reference tables; or reference tables only | plain tables and joins, no sub-select | **M1** colocated: the whole statement runs on every group of the target | In |
| distributed | anything else: another colocation group, a join on a non-key column, a local table, a sub-select, `VALUES`, a function, an aggregate | `ON` has `target.dist_col = <expr of the source>` | **M3** routed source: the origin evaluates the source, sends each row to the owner of `hash(expr)`, each group runs the `MERGE` over its share | In |
| distributed | as above, `ON` has no such equality | | none | Refused: "the ON clause of a MERGE into a distributed table must equate its distribution column with a column or expression of the source" |
| reference | reference tables or constants only, shippable | | **M4** the 2a reference-table write: run on every node in node-id order, in the two-phase transaction, with the advisory locks and the equal-row-count check | In |
| reference | distributed, local, sub-select, `VALUES` | | **M5** the origin evaluates the source once and sends the rows to every node, each node runs the same `MERGE` | In, last increment (section 14) |
| local | local only | | PostgreSQL | Unchanged (not a distributed statement) |
| local | distributed or reference tables | | **M6** the planner hook fences each distributed source relation (7.7) so that it is read by the scan, with a copy row mark | In |
| distributed target on another group, single bucket (`ON` restricted to one key) | any | | the routed statement of 2a | In, a consequence of M1/M3 being general: the statement is one group's M1 |

Refused in every case, with the reason:

- `UPDATE` of the distribution column in a `WHEN MATCHED` action (the row would
  have to move between buckets; refused for `UPDATE` today).
- An `INSERT` action whose distribution-column value is not provably the key the
  statement was routed or colocated by (7.3 rule 4 and 7.4 rule 5): the row
  would belong to another group.
- An `INSERT` action into a target that has a column with a volatile default,
  an identity column or a `serial` column that the action does not give a value
  for, and a stored generated column is fine (each group computes it). The group
  would draw the value from its own sequence (2a: sequence values are
  node-local). 2b computes defaults at the origin once per row, which is possible
  only when the origin knows which rows are inserted, and in a `MERGE` the group
  decides that. The error says "give the column a value in the INSERT list, or
  use a value that is not volatile".
- Any expression that is not shippable (2b 5.1, 5.2) in the `ON` clause, a
  `WHEN` condition, a `SET` or `VALUES` expression or `RETURNING`: it would run on
  a group. (A stable function without column arguments is evaluated once at the
  origin and shipped as a parameter, 2b 5.2.)
- `MERGE` into a table with user triggers: distributed tables have none
  (2b 11.2); a reference table that has one is refused already; a **local**
  target keeps its triggers (M6), including the capture trigger of a ledger table.
- `MERGE` on a hot standby (read-only).
- `MERGE` inside a parallel worker (as every extension statement, S2-08).

### 7.3 M1: colocated `MERGE`

Conditions, all checked at the origin by the analysis of 6.1/6.3 of 2b extended
to a write (the target is a member of the key class; the sources are the other
members and reference tables):

1. the target and every distributed source relation are in one key class, and
   the `ON` clause contains, among its top-level `AND` conjuncts, a key equality
   (4.3) between the target's distribution column and the key variable of the
   source class; or the source has only reference tables;
2. no sub-select, CTE or function in `USING`: plain tables and joins of plain
   tables (a sub-select source goes to M3, which is correct and slower);
3. every expression is shippable (2b 5.1, 5.2, 3.3);
4. each `INSERT` action's distribution-column value is a bare `Var` of the key
   class, equal to the source side of the key equality in `ON`; a statement whose
   source has **only reference tables** has no `WHEN NOT MATCHED [BY TARGET]
   INSERT` action (an inserted row's group would be decided by the key value, but
   every group would insert it);
5. no `UPDATE` assigns the distribution column; no refused target feature (7.2).

Execution: the statement is deparsed as written, qualified, under the transmission
modes, with `/* kingres:frag v=2 kind=merge map=V */`, and sent to **every group
that owns a bucket of the target** (after pruning by a key restriction in `ON`),
including groups where the source share is empty, in node-id order, in the
transaction's two-phase commit; the local group runs it through SPI (`kind=merge`
local part with the nonce, 2b 4.4). Each group runs a plain `MERGE` on its buckets:
the planner on the group joins its target partitions and its source partitions,
and a group's `MERGE` sees only matches within the group, which by condition 1 are
all matches. The command tag count is the sum of the groups' counts; `RETURNING`
rows are collected as the router does (2b 11.4), in the order of groups. The
group's walker accepts `kind=merge` only for a `MERGE` whose target is a
distributed table and whose other relations are distributed tables of the same
colocation group, reference tables, or none; without sub-selects; with
shippable expressions; it checks the placement as always (3.2).

A group that owns no bucket of the target is not asked. A group that owns buckets
and has no matching source rows still runs the statement, because `WHEN NOT
MATCHED BY SOURCE` acts on target rows with no source row: **the origin never
skips a group that owns target buckets when the statement has such an action.**
(The Citus documentation records a gap of this kind for a repartitioned source
whose share of a shard is empty; **[Needs verification]** that this reading of
that page is right. It is stated here as a requirement of this design either
way.)

### 7.4 M3: routed source

Conditions, in addition to 7.2's refusals:

1. the `ON` clause has a top-level conjunct `t.k = e` where `t.k` is the target's
   distribution column (a bare `Var`), `e` is an expression of the source relations
   only, the operator is a hash-family equality of the key's type, the collation is
   deterministic and default or `C`, and `e` is immutable and shippable. Other
   conjuncts of `ON` may refine the match;
2. every `WHEN ... AND <cond>`, `SET`, `VALUES`, `ON` and `RETURNING` expression is
   shippable;
3. every `INSERT` action sets the distribution column to an expression that is
   `equal()` to `e` (after looking through `RelabelType`), so that the inserted
   row's bucket is the bucket of the source row's route; `WHEN NOT MATCHED BY
   SOURCE` has no `INSERT`;
4. no `UPDATE` assigns the distribution column; no volatile or identity default
   is needed by an `INSERT` (7.2).

Algorithm:

1. **Read the source.** The origin plans and runs the source as a `SELECT` of its
   own (`SELECT <columns of the source that the statement uses>, e AS ks_route
   FROM <USING clause>`, `WHERE` none), through the ordinary planner hook, so it
   uses the 2a scan, every pushdown path of 2b and P7. The source may be a local
   table, a reference table, a sub-select, `VALUES`, an aggregate (a rollup),
   anything a `SELECT` is.
2. **Route.** Each row goes to the group that owns the bucket of `ks_route` in the
   target's colocation group, found by the catalog's bucket lookup with each
   bucket's own modulus (never `hash % bucket_count`; the row router's function,
   2b 11.6 as phase 10 changes it). A row whose `ks_route` is `NULL` cannot
   match; it is routed to the group of the bucket that covers hash slot 0 if
   there is a `WHEN NOT MATCHED [BY TARGET] INSERT` action (the inserted row has a
   `NULL` distribution value, which hashes to slot 0 as PostgreSQL's hash
   partitioning does) and dropped otherwise. Rows are kept per destination in
   tuplestores (4.6's rule: `work_mem` shared, then disk).
3. **Run per group.** Each group of the target that owns buckets (all of them if
   the statement has a `NOT MATCHED BY SOURCE` action, otherwise only those whose
   share is not empty) gets the statement
   `MERGE INTO <target> [AS a] USING (SELECT c1::t1 AS c1, ... FROM unnest($1::text[], ...) AS u(c1, ...)) AS <source alias> ON <ON> WHEN ...`
   with the `USING` clause replaced as in 3.1 and everything else deparsed from the
   statement, marker `kind=merge rp=1`. **One statement per group**, because
   "cannot affect row a second time" must be raised exactly as on one node.
4. Counts and `RETURNING` rows are combined as in M1.

Correctness: a target row `t` on group `g` can match only source rows with
`e = t.k`; those rows hash to `bucket(t.k)`, so they were sent to `g` (1 to 2);
therefore the group's `MERGE` sees all of `t`'s possible matches and no other
group needs them. A source row with no match anywhere reaches exactly one group
(its route), is `NOT MATCHED BY TARGET` there, and is inserted there; by condition
3 the inserted row's bucket is on that group, so tuple routing accepts it. A target
row with no source row is `NOT MATCHED BY SOURCE` on its own group. The
duplicate-match error is raised by a group when two source rows match one target
row, and then the transaction aborts as it would on one node. The order of
evaluating rows, and which of two competing `UPDATE`s wins in a concurrent
transaction, are as unspecified as on one node.

Memory and bounds: the group's share must fit in
`kingres_shard.merge_max_source_bytes` (default 64 MB, 1 MB to 512 MB, the same
reason as 4.6); exceeded before the first group runs: `54000`, with the hint to
split the source by key range (a `MERGE` per range) or raise the setting. There
is no fallback for a write: the statement could not run before this phase either.

### 7.5 M4 and M5: a reference-table target

A reference table is on every node and a write runs on every node, in node-id
order, in one two-phase transaction, with the advisory locks that serialize
writers cluster-wide (README, "Reference tables", S2-06). The statement is
accepted by the rule that governs every statement run on several nodes
(`shippable.c`, `ship_walker` and `query_ok`, extended to the `MERGE` node kinds:
the join condition, the actions and their conditions, the `SET` and `VALUES`
lists, `RETURNING`).

- **M4.** The statement reads only reference tables, `VALUES` and constants. The
  2a rules apply unchanged (built-in immutable functions, no `LIMIT`, no
  order-dependent aggregate...). The row counts of the nodes must be equal (the
  existing check), `RETURNING` rows are those of the local copy.
- **M5.** The source is distributed, local, or a sub-select over them. The origin
  evaluates the source once, in its transaction, takes the writer locks, and runs on
  every node the statement with the `USING` clause replaced by the sub-select of
  3.1 over the same arrays, so that every copy sees the same source rows. The
  expressions of the statement obey the 2a rule (no volatile function). The copies
  stay equal because the `MERGE` is deterministic given the same source rows
  and the same target contents, the target copies being equal before the
  statement (2a, section 3). The equal-row-count check stays. `merge_max_source_bytes`
  applies to the whole source (every node gets all of it).

2b lists "a reference-table write that reads a distributed table" as out of
scope because it needs the cluster-wide writer serialization and a per-statement
equality check of the copies. Both already exist for reference-table writes (2a);
M5 adds only the transport of the source, which is the 4.5 mechanism. Because it
is the most delicate of the five shapes (a failure to keep the copies equal is
silent damage), it is the last increment of the `MERGE` work and has its own
fault tests (section 14).

### 7.6 M6: a local target with a distributed or reference source

PostgreSQL asks for `ctid` of a source relation that is a table (7.1). The
planner hook therefore rewrites the **query copy** before planning: each
distributed table in `USING` (not the target) becomes a sub-select
`(SELECT <all attributes in order> FROM <table> OFFSET 0)` with the same alias
and column names (attributes the statement does not use are `NULL::type`), at
the same range table index. A sub-select with `OFFSET 0` is not pulled up
(`is_simple_subquery` refuses a sub-select with `limitOffset`, **[Needs
verification]**), so it stays a sub-select range table entry, and
`preprocess_rowmarks` gives it `ROW_MARK_COPY`. The sub-select is then planned
by 2b as a statement of its own level: the 2a scan with the pruning and shipped
restrictions that apply, and all conjuncts of `ON` stay in `ON` (moving them into
the fence would change the `NOT MATCHED` semantics). The fence also blocks the
pushing of restrictions from outside into the sub-select (a qual is not pushed
into a sub-select with `LIMIT`/`OFFSET`, `subquery_is_pushdown_safe`,
**[Needs verification]**), which is the cost of the method: the whole source is
read through the scan, restricted only by restrictions written inside the user's
own sub-select. A reference table needs no fence (it is local), so a local target
with a reference source is planned by PostgreSQL unchanged.

The local target's triggers run as always, which includes the capture trigger of
a **ledger table**: `MERGE INTO <ledger table> USING <distributed table>` is
executed by `ModifyTable` on this node and the ledger's guarantees are those of
`ledger-0.2.md`, not touched here. It is not a pushdown unit and not routed.

### 7.7 Transaction, snapshot, bucket moves, standbys

- **Writers.** Every group that runs a `MERGE` fragment is a writer
  (`kingres_shard_note_remote_write`); two or more writers commit with two-phase
  commit (2a). The statement is atomic: an error on any group aborts everything.
  Nothing is created on a group, so the prepare is possible.
- **Snapshot.** Each group's `MERGE` reads its target and its source share in
  its own statement snapshot. In M3 the source is read at the origin before the
  groups run, so the source may be older than the target as seen by the group; in
  `READ COMMITTED` the `MERGE` re-evaluates a concurrently updated target row
  (the usual `MERGE` behaviour), but a source row that changed after the origin read
  it is not re-read. This is stated in the README: the source is a snapshot of
  the statement's start.
- **Bucket moves.** A `MERGE` fragment is a write fragment: it carries `map=V`
  and the group compares its placement before running (the write rule of
  phase 4, "a fan-out write compares the placement of every target after the
  statement ran"). A difference raises `KS001` and rolls the statement back with
  every change. The statement can be repeated. M3's routing uses the placement of
  the target's colocation group in the plan.
- **Reference tables and ledger tables.** 7.5 and 7.6.
- **Standbys.** Refused (read-only).

### 7.8 Correctness conditions for `MERGE`

- **G1.** The result of a `MERGE` by M1, M3, M4, M5 or M6 equals the result of
  the same statement on one node holding all rows: the same rows in the target
  afterwards, the same command tag counts, and, as a set, the same `RETURNING`
  rows, except for the unspecified choices that PostgreSQL leaves open (which of
  two rows is inserted first, the order of `RETURNING` rows).
- **G2.** Every group that owns buckets of the target and could act (7.3, 7.4) runs
  the statement.
- **G3.** A statement outside the accepted shapes raises the error of the matching
  row of 7.2 before any group has run anything.
- **G4.** A statement that fails on a group leaves no change anywhere.

## 8. Approximate aggregates: overview

The sketches are a new extension, `kingres_sketch` (section 9). Pushing them
down needs the partial-aggregation machinery of 2b section 7 and the widening of
3.3. Section 10 covers stored sketches and rollups, section 12 the control plane.

## 9. The extension `kingres_sketch`

### 9.1 Licences, and what is bundled

| Component | Licence (source) | Decision | Reason |
|---|---|---|---|
| HyperLogLog, `postgresql-hll` | Apache-2.0 (<https://github.com/citusdata/postgresql-hll>, `LICENSE`) | **Not bundled; written new** from Flajolet et al. 2007, Heule et al. 2013 and Ertl 2017 | Apache-2.0 is permissive and compatible with shipping inside a PostgreSQL-License product, but its files would carry another licence and a notice obligation inside a tree where every file has `SPDX-License-Identifier: PostgreSQL`; more important, the pushdown needs the three types in one extension with one version that the control plane can compare across servers and one format-version byte. The upstream is a separate extension with its own versions and storage format. |
| Top-n, `postgresql-topn` | **AGPL-3.0** (<https://github.com/citusdata/postgresql-topn>, `LICENSE`) | **Written from scratch** from Metwally et al. 2005 and Agarwal et al. 2012. The source of `postgresql-topn` is not read | AGPL code must not be used or copied (ADR 0004). Only its published purpose (a frequent-items sketch with a union aggregate) is known from its page. |
| t-digest, `tdigest` | PostgreSQL License (<https://github.com/tvondra/tdigest>, `LICENSE`) | **Not bundled; written new** from Dunning and Ertl 2019 | The licence would allow copying with its notice. Not copied because its development branch is 2.0.0-dev (the page says so) with a changing state format, and for the same reason as hll: one extension, one version, one format header, one serialization policy and one validation policy for untrusted bytes. Its published SQL names are not reused (9.2 uses a schema). |

One extension means that "same version on every node" is a single check, that
the allowed list of 3.3 has one entry, and that a state format change is one
version. The cost is that the code is ours to write, review and test; the code
is about 2500 lines of C (a tenth of it the three types' input and output
functions) and its tests are in 14.3. **[Needs verification]** patent status of
HyperLogLog++ and of the t-digest algorithm: the sources read do not say; legal
review before a public release, as ADR 0004 asks for the trademark question.

### 9.2 Packaging, names, versions

- Extension `kingres_sketch`, directory `contrib/kingres_sketch`, files
  `kingres_sketch.control` (`default_version = '1.0'`, `module_pathname =
  '$libdir/kingres_sketch'`, `relocatable = false`, `schema = kingres_sketch`),
  `kingres_sketch--1.0.sql`, later `kingres_sketch--1.0--1.1.sql`, `meson.build`,
  `Makefile`, `README.md`, `sql/`, `expected/`, `t/`. Every source file starts
  with `SPDX-License-Identifier: PostgreSQL` and the RooyeKhat Media Co copyright
  line, as the other extensions.
- It is a plain library of functions: **no `shared_preload_libraries` entry, no
  hook, no background worker, no catalog table**. Installing it needs no restart
  and no `max_prepared_transactions`.
- The schema `kingres_sketch` is fixed (non-relocatable). Types and functions
  are not in `public`, so the extension never shadows or conflicts with an
  installed `hll`, `topn` or `tdigest`. Users add `kingres_sketch` to
  `search_path` or qualify names. Every function has `SET search_path =
  pg_catalog, pg_temp`... **[Recommendation]** `SECURITY INVOKER`, `SET
  search_path = pg_catalog, kingres_sketch, pg_temp` as the other extensions do
  (README "Security notes"), and a regression test `security` that fails if a
  function lacks it.
- Versions and the state format. A sketch begins with a header: one byte magic,
  one byte **format version** (1 in extension 1.0), one byte kind. A reader that
  meets a format version it does not know raises an error (`22023`, "unsupported
  sketch format N"); it never reads the bytes as another format. Extension
  versions that keep format 1 are listed in `kingres_shard`'s allowed list as
  accepted together (3.3); a new format means a new list entry and a note in the
  upgrade script.
- Every function is `IMMUTABLE` where it is a pure function of its arguments,
  `PARALLEL SAFE`, `STRICT` where NULL in gives NULL out. No function reads a GUC.
  The aggregate functions are `PARALLEL SAFE`.
- **Canonical bytes.** A sketch value is a function of its content: two sketches
  with the same registers (hll), the same counters (topn) or the same centroids
  (tdigest) have identical bytes. This matters for equality, for a ledger table
  that holds a sketch column (the ledger hashes row content), and for tests.
- **Untrusted bytes.** A value can arrive from `bytea`, a type input function, a
  `COPY`, a partial state from another node. Every reader validates before it
  uses a length or an index: header, kind, precision range, payload length,
  sorted and in-range sparse entries, counter lists with counts that are
  consistent, centroid counts, weights and ordering. Invalid input is an error
  (`22P02` for the input function, `22023` otherwise), never a crash or a read
  past the value. There is a fuzz test (14.3).
- Hash. The 64-bit hash of an element is the extended hash function of the
  type's default hash operator class (`hash_extended_proc` of the type cache) with
  seed 0, finished with a fixed 64-bit mixing function (so that the low and
  the high bits are well distributed). It is defined for the types that have a
  hash operator class; a type without one raises "type X has no hash function".
  For text-like types it hashes the bytes in the database encoding, so
  `kingres_sketch.self_test()` is compared across nodes (12.3) to catch nodes that
  differ in encoding or byte order. **[Needs verification]** that the extended
  hash functions of the built-in types are independent of byte order and
  platform for the types allowed in a pushdown (the partition hash of the same
  functions already has to be, for `add_node` of nodes on different machines);
  `self_test()` is the check.

### 9.3 Type `kingres_sketch.hll`

**Algorithm.** A sketch has precision `p` (4 to 18, default 14) and `m = 2^p`
registers. An element's 64-bit hash `h` selects register `j = h >> (64 - p)` and
the rank `r = 1 + (the number of leading zeros of the remaining 64 - p bits)`
(at most `64 - p + 1`); the register keeps the maximum rank seen (Flajolet et
al. 2007). The estimate is Ertl's improved estimator (arXiv:1702.01284): from the
histogram `C_k` of register values, `n = alpha_inf * m^2 / (m * sigma(C_0 / m) +
sum_{k=1..q} C_k * 2^-k + m * tau(1 - C_{q+1} / m) * 2^-q)` with `q = 64 - p`,
`alpha_inf = 1 / (2 ln 2)` and the series `sigma`, `tau` of that paper. It needs no
bias tables and no switch between linear counting and the raw estimate
(**[Needs verification]**: the formulas are written from memory of the paper's
Algorithm 6 and must be checked against it before coding; the test in 14.3
checks the estimate against exact counts, which would reveal an error). The
original HyperLogLog++ tables of Heule et al. are not used, which also avoids
reproducing a published table.

**Encodings.** `EMPTY` (header only); `SPARSE` (a sorted array of 32-bit entries
`(j << 6) | rank` of the non-zero registers, used while it is smaller than the
dense form); `DENSE` (`m` registers of 6 bits). A sketch is converted from sparse
to dense when the sparse form would be larger than the dense one, and **never
converted back** (so that bytes are canonical for a given state: the encoding is
the one that the register set implies, sparse if and only if its size is smaller
than dense). The estimate is a function of the registers only, whatever the
encoding. Sizes: empty 4 bytes; dense `0.75 x 2^p` bytes plus the header (p = 14:
12 KB; p = 18: 192 KB).

**Merge.** The union of two sketches of the same precision is the register-wise
maximum. It is commutative, associative and idempotent, so a union of partial
sketches built on any partition of the rows, in any order and with any number of
duplicates across the partitions, equals the sketch built from all rows by one
backend. Precision mismatch: error `22023` ("cannot merge sketches of precision
12 and 14; use hll_fold"). `hll_fold(hll, p2)` reduces to a lower precision by
taking the maximum over the registers that map to the same lower register with the
rank adjusted (Ertl); it is the only way to combine different precisions.

**Accuracy.** The relative standard error of the estimate is about `1.04 /
sqrt(m)`: 0.81% for p = 14, 1.63% for p = 12, 0.2% for p = 18 (Flajolet et al.;
**[Needs verification]** that the constant 1.04 holds for Ertl's estimator, which
the paper presents as at least as accurate). `count(DISTINCT)` rewrite maps the
requested error to a precision: `p = ceil(2 * log2(1.04 / error))`, capped to
4..18 (error 0.01 gives p = 14, 0.005 gives p = 16).

**SQL.**

```
CREATE TYPE kingres_sketch.hll;        -- varlena; input/output: '\x' + hex of the bytes; send/receive: the bytes
hll_empty(precision int DEFAULT 14) RETURNS hll                      -- IMMUTABLE STRICT PARALLEL SAFE
hll_hash(anyelement) RETURNS bigint                                  -- the 64-bit hash of 9.2
hll_add(hll, anyelement) RETURNS hll
hll_add_hash(hll, bigint) RETURNS hll
hll_union(hll, hll) RETURNS hll
hll_fold(hll, precision int) RETURNS hll
hll_cardinality(hll) RETURNS bigint                                   -- rounded estimate
hll_precision(hll) RETURNS int
hll_is_empty(hll) RETURNS boolean
CREATE CAST (hll AS bytea) WITH FUNCTION ... ;  CREATE CAST (bytea AS hll) WITH FUNCTION ... ;   -- explicit, validating
AGGREGATE hll_add_agg(anyelement) RETURNS hll                        -- precision 14
AGGREGATE hll_add_agg(anyelement, precision int) RETURNS hll         -- precision constant for the aggregate call
AGGREGATE hll_union_agg(hll) RETURNS hll
AGGREGATE approx_count_distinct(anyelement) RETURNS bigint           -- hll_cardinality(hll_add_agg(x)), precision 14
AGGREGATE approx_count_distinct(anyelement, precision int) RETURNS bigint
```

`NULL` elements are ignored by every aggregate. An aggregate over no rows returns
an empty sketch (`hll_add_agg`, `hll_union_agg`) or `0` (`approx_count_distinct`),
as `count` does: the final function is not strict and builds the empty sketch when
its state is null. A second argument that is not constant is read from the first
row; a row with another value raises `22023`.

### 9.4 Type `kingres_sketch.topn`

**Algorithm.** The Space-Saving summary of Metwally et al. 2005, with `k`
counters (`capacity`, default 1000, 1 to 100000). Each counter is `(item, count,
error)`. Adding an item that has a counter increments it. Adding an item with no
counter, while fewer than `k` exist, creates `(item, 1, 0)`. When all `k` counters
are in use, the counter with the smallest count `c_min` is taken over: its item
is replaced, its `error` becomes `c_min`, and its count becomes `c_min + 1`.
After `N` insertions, every counter's `count` is at least the true frequency of its
item and at most that plus its `error`, and `error <= N / k`; every item with
true frequency above `N / k` has a counter (Metwally et al. 2005).

**Merge.** The merge of Agarwal et al. 2012 for counter summaries: for two
summaries `S1`, `S2` of capacity `k`, take the union of the items; an item missing
from a summary that is full is given that summary's minimum count (and the same
value as additional error); keep the `k` items with the largest counts. The
guarantee of a merged summary over `N = N1 + N2` insertions is the same
`count - error <= true <= count` and `error <= N / k` (**[Needs verification]**:
the paper's theorem is for the Misra-Gries summary and carries to Space-Saving by
the isomorphism of the two summaries; the exact statement and the constant of the
merge must be read before coding; the property tests of 14.3 check the
invariants after random sequences of adds and merges, which would show a
violation). The merge is **not** associative or commutative in its exact result:
two merge orders can keep different low-count items. Only the guarantees hold.
Both operands must have the same capacity (`22023` otherwise).

**Items.** An item is a string. `topn_add` and `topn_add_agg` take `anyelement` of
the types whose canonical text does not depend on the session: `int2`, `int4`,
`int8`, `numeric`, `text`, `varchar`, `bpchar`, `name`, `uuid`, `bool` (the
canonical text is the type's output function, which is independent of session
settings for these types: **[Needs verification]** `numeric_out` and `uuid_out`).
Floating-point, date, time and range types are refused ("topn does not take type X;
cast to text with an explicit format"), because their text depends on
`extra_float_digits` or `DateStyle`. An item longer than 1024 bytes is an error
(`54000`), so the state is bounded: at most `k x (1024 + 24)` bytes in memory.
The state in memory is a hash table keyed by item plus a min-heap of counts;
serialized it is a list sorted by `(count desc, item)`, so bytes are canonical.

**SQL.**

```
CREATE TYPE kingres_sketch.topn;
topn_empty(capacity int DEFAULT 1000) RETURNS topn
topn_add(topn, anyelement) RETURNS topn
topn_union(topn, topn) RETURNS topn
topn_list(topn, n int DEFAULT NULL) RETURNS TABLE (item text, frequency bigint, max_error bigint)   -- by frequency desc; all counters if n is NULL
topn_total(topn) RETURNS bigint          -- N, the number of items added
topn_capacity(topn) RETURNS int
AGGREGATE topn_add_agg(anyelement) RETURNS topn                      -- capacity 1000
AGGREGATE topn_add_agg(anyelement, capacity int) RETURNS topn
AGGREGATE topn_union_agg(topn) RETURNS topn
```

`frequency` is the counter's count (an upper bound), `max_error` its error; the
true frequency is between `frequency - max_error` and `frequency`.

### 9.5 Type `kingres_sketch.tdigest`

**Algorithm.** The merging t-digest of Dunning and Ertl 2019: a sorted list of
centroids `(mean, weight)`, with a buffer of unmerged points that is merged into
the list when it is full, using the scale function `k_1` (arcsine) so that
centroids near the tails are small and near the median large; `compression`
`delta` (default 100, 10 to 10000) bounds the number of centroids (about
`delta` for `k_1`, **[Needs verification]** exact bound in the paper). The digest
keeps the exact `min`, `max` and total weight. `tdigest_quantile(d, q)` interpolates
between centroid means, using the exact min and max for the extremes; the
inverse, `tdigest_rank(d, x)`, is the estimated fraction of weight at or below
`x`. Merge: concatenate the centroids of both digests and run the merging pass.

**Accuracy.** The rank error is smallest near 0 and 1 and largest near the median
(Dunning and Ertl). The numbers the tests assert are measured on the test data and
fixed in the expected files, not claimed as theory: at `delta = 100` the rank
error of `quantile(q)` is expected below `0.01` for every `q` and below `0.002`
for `q <= 0.01` or `q >= 0.99` on the test distributions (uniform, normal,
log-normal, a mixture with heavy ties); the exact thresholds are set by the first
measurement and reviewed (14.3). **A merge is not exact and not order independent**:
the result of merging partial digests of the groups differs, in the last digits and
within the bound, from the result of a digest built by one backend over all the
rows. The scan path (a single digest built at the origin) and the pushdown path
therefore differ slightly; tests compare both with the exact quantile within the
bound, not with each other for equality (14.3). `NaN` is rejected (`22023`),
infinities are rejected; `NULL` is ignored.

**SQL.**

```
CREATE TYPE kingres_sketch.tdigest;
tdigest_empty(compression int DEFAULT 100) RETURNS tdigest
tdigest_add(tdigest, x double precision, weight bigint DEFAULT 1) RETURNS tdigest
tdigest_union(tdigest, tdigest) RETURNS tdigest
tdigest_quantile(tdigest, q double precision) RETURNS double precision           -- q in [0, 1]; NULL for an empty digest
tdigest_quantile(tdigest, q double precision[]) RETURNS double precision[]
tdigest_rank(tdigest, x double precision) RETURNS double precision
tdigest_count(tdigest) RETURNS bigint
tdigest_min(tdigest) RETURNS double precision
tdigest_max(tdigest) RETURNS double precision
tdigest_compression(tdigest) RETURNS int
AGGREGATE tdigest_agg(x double precision) RETURNS tdigest                         -- compression 100
AGGREGATE tdigest_agg(x double precision, compression int) RETURNS tdigest
AGGREGATE tdigest_weighted_agg(x double precision, weight bigint, compression int) RETURNS tdigest
AGGREGATE tdigest_union_agg(tdigest) RETURNS tdigest
AGGREGATE approx_percentile(q double precision, x double precision) RETURNS double precision   -- compression 100
AGGREGATE approx_percentile(q double precision, x double precision, compression int) RETURNS double precision
```

For `approx_percentile` the fraction `q` is read from the first row of the group
(it must be a constant of the call; another value in a later row raises `22023`)
and is part of the state. Other numeric types are cast to `float8` by the user.
It is a plain aggregate, not an ordered-set aggregate: `percentile_cont(0.5)
WITHIN GROUP (ORDER BY x)` stays exact and is never rewritten.

### 9.6 The aggregates as aggregates

Every aggregate has a transition state of type `internal`, a `finalfunc`, and:

| Aggregate | `stype` | `COMBINEFUNC` | `SERIALFUNC` / `DESERIALFUNC` | `FINALFUNC` | `PARALLEL` |
|---|---|---|---|---|---|
| `hll_add_agg`, `hll_union_agg` | `internal` | `hll_agg_combine(internal, internal)` | `hll_agg_serial(internal) -> bytea` / `hll_agg_deserial(bytea, internal) -> internal` | `hll_agg_final(internal) -> hll`, not strict | `SAFE` |
| `approx_count_distinct` | `internal` | the same | the same | `hll_agg_final_card(internal) -> bigint` | `SAFE` |
| `topn_add_agg`, `topn_union_agg` | `internal` | `topn_agg_combine` | `topn_agg_serial` / `topn_agg_deserial` | `topn_agg_final -> topn` | `SAFE` |
| `tdigest_agg`, `tdigest_weighted_agg`, `tdigest_union_agg` | `internal` | `tdigest_agg_combine` | `tdigest_agg_serial` / `tdigest_agg_deserial` | `tdigest_agg_final -> tdigest` | `SAFE` |
| `approx_percentile` | `internal` | `tdigest_agg_combine` (the fraction travels in the state) | as above | `tdigest_agg_final_pct -> float8` | `SAFE` |

`CREATE AGGREGATE` options as in `create_aggregate.sgml` (`COMBINEFUNC`,
`SERIALFUNC`, `DESERIALFUNC`, `PARALLEL = SAFE`), plus `SSPACE` set to the
typical state size (12288 for hll at p = 14, 16384 for topn, 8192 for tdigest) so
the planner's estimate of the hashed aggregate's memory is right. The transition
functions call `AggCheckCallContext` and keep the state in the aggregate's
memory context. The serialized state is the same bytes as the type's value (so a
partial result and a stored sketch have one format); `DESERIALFUNC` validates (9.2).
`NULL` input values are skipped by the transition function (it is not strict
because the first call must create the state).

`aggcombinefn`, `aggserialfn` and `aggdeserialfn` set and an `internal` state
with both serialization functions are exactly what `preprocess_aggref` needs to
leave `hasNonPartialAggs` and `hasNonSerialAggs` false (2b 7.1); `DISTINCT` and
`ORDER BY` inside a call of these aggregates are not partial-capable and stay at
the origin (same as built-ins, 2b 7.1).

### 9.7 Pushdown of sketch aggregates

2b's P3 (partial aggregate) and P4 (closed aggregate) apply as they are written,
with two changes.

1. **The aggregate rule of 2b 7.2** ("the aggregate is built in") becomes: "the
   aggregate is built in, or it is a member of an allowed extension (3.3) whose
   state format the origin and the group share (the `ext=` marker of 3.2)".
   Its transition type is `internal` with serialization, so the partial column has
   type `bytea` as for `sum(int8)`.
2. **What the group does** is 2b 7.3 unchanged: the group plans the fragment with
   the partial settings, `partialize_plan` re-marks its `Agg` as `AGGSPLIT_INITIAL_
   SERIAL`, runs the transition functions over its rows, and returns the
   serialized state (`SERIALFUNC`) instead of calling the final function. The
   origin's final aggregate deserializes each state (`DESERIALFUNC`), combines
   them (`COMBINEFUNC`) and calls `FINALFUNC`. The 2b V1 and V2 verifications
   apply.

What each piece needs:

| Piece | Needed | Why |
|---|---|---|
| `COMBINEFUNC` | yes | The origin combines states of several groups. Without it `hasNonPartialAggs` is set and P3 is not created. |
| `SERIALFUNC`, `DESERIALFUNC` | yes (state is `internal`) | A state must cross as `bytea`. Without them `hasNonSerialAggs` is set. |
| `PARALLEL SAFE` on the aggregate and its functions | yes for parallel aggregation of local tables and for `can_partial_agg` to be usable inside a worker; the 2b fragments themselves run with `max_parallel_workers_per_gather = 0` | Without it a plain `GROUP BY` over a local table never parallelizes the aggregate. |
| `IMMUTABLE` on the transition, combine and final functions | the function walker of 2b 5.1 requires it | A volatile function is not shipped. |
| Types built in or allowed | the arguments are built-in types (`anyelement` resolved to a built-in type; for topn a type of 9.4); the result type `hll`, `topn`, `tdigest` is allowed (3.3) because the final function runs at the origin | Rule 2 of 5.1. |
| The `ext=` marker and version agreement | yes | 3.2 rule 5. |

An aggregate call that has a non-allowed type argument (a user-defined type) is
not shipped: that statement is aggregated at the origin over P2 or P1, as for
any non-shippable aggregate (2b 5.4).

Size of the partial result: one state per group per grouping value, at most
12 KB (hll p = 14), 1000 counters plus items (topn), 2 x 100 centroids (tdigest)
each. A high-cardinality `GROUP BY` multiplies this: the planner's estimate uses
`SSPACE`, and `enable_partial_aggregation = off` or a smaller precision
helps; the README says so.

### 9.8 Making `count(DISTINCT x)` approximate

Setting `kingres_shard.count_distinct_mode = 'approximate'` (default `'exact'`,
`USERSET`; values `exact`, `approximate`) and `kingres_shard.count_distinct_error_rate`
(`real`, default 0.01, range 0.0005 to 0.1; maps to the precision of 9.3, so
0.01 gives p = 14 and 0.005 gives p = 16) make the planner hook rewrite, in a
`Query` that reads a distributed table:

- every `count(DISTINCT e)` with **one** argument of a type that has a hash
  operator class and is built in, with or without `FILTER`, no `ORDER BY`, not as a
  window function (PostgreSQL does not allow `DISTINCT` there),
- into `kingres_sketch.approx_count_distinct(e, p)`, the same `Aggref` with the
  function replaced, `aggdistinct` cleared, a constant argument `p` added (its
  `aggargtypes` and `aggtype = int8` updated).

It is done on the `Query` before 2b's analysis, in every query level of the
statement that has a distributed table in its own range table (not in a level
that has none, so a local-only sub-select stays exact). A type without a hash
function, `count(DISTINCT a, b)`, and `count(DISTINCT e)` over a local table only
are left alone. If `kingres_sketch` is not installed, or its version is not on
the allowed list, the setting does nothing and issues one warning per session
("count_distinct_mode is approximate but kingres_sketch is not installed in this
database"). Because the rewritten aggregate is partial-capable, the statement now
takes P3 and the groups return sketches instead of rows (2b A4 says `count(DISTINCT)`
is never split because it is not partial-capable; the rewrite is what makes it so).

Who may set it: the setting changes results, so it is **not** a default on
installing the extension. The administrator turns it on for a database in the web
console (12.1); the task runs `ALTER DATABASE ... SET
kingres_shard.count_distinct_mode = 'approximate'` and the error rate. Any
session may override it for itself (`SET`, `USERSET`): that affects only that
session's statements. A view or a function that was defined while the setting
was on is planned each time with the setting of the session that runs it.
`EXPLAIN` of a rewritten statement shows `approx_count_distinct(...)` in the
aggregate and the line `Rewritten: count(DISTINCT x) -> approximate (p = 14)`.
**[Recommendation]** `exact` stays the default for every database that did not
enable it, and the statement result carries no marker (the type is `bigint`); the
console says what the setting does.

**[Needs verification]** that the `Aggref` fields can be rewritten in the planner
hook as described (the fields are those the parser fills; `preprocess_aggref`
fills `aggtranstype` later). A test with an assertion build settles it.

## 10. Stored sketches and rollup tables

A column of type `kingres_sketch.hll`, `topn` or `tdigest` is an ordinary
column. A rollup table keeps one sketch per group of rows:

```
CREATE TABLE page_views_hourly (
    customer_id bigint NOT NULL, hour timestamptz NOT NULL,
    views bigint NOT NULL, visitors kingres_sketch.hll NOT NULL,
    top_pages kingres_sketch.topn NOT NULL, latency kingres_sketch.tdigest NOT NULL,
    PRIMARY KEY (customer_id, hour));
SELECT kingres.create_distributed_table('page_views_hourly', 'customer_id', colocate_with => 'events');
```

- **The type must exist on every node** before the table is distributed
  (types are not synchronized, README "Limitations"); `create_distributed_table`
  fails otherwise with the usual "type does not exist" and the extension's
  installation is the control-plane task of 12.1.
- **Reading a column through the 2a scan.** A column of a sketch type travels in the
  text form of the type (hex of the bytes; the input function validates it), like any
  user-defined type. **[Needs verification]** that the 2a scan, whose restrictions
  are limited to built-in types, still reads columns of other types (the scan
  fetches columns, and only restrictions are subject to the type rule); a test
  settles it, and if it does not, the scan gets the rule "columns of an allowed
  extension's types may be fetched".
- **Reading with pushdown.** `SELECT customer_id, hll_cardinality(hll_union_agg(visitors)) FROM
  page_views_hourly WHERE hour >= ... GROUP BY customer_id` is a closed aggregate
  (P4: the group key contains the distribution column), computed at the groups;
  the same without `GROUP BY customer_id` is P3: each group combines its rows'
  sketches and returns one state.
- **Equality and indexes.** A sketch has no default btree or hash operator class.
  It is not usable in a distribution column, a unique key or a `GROUP BY`.
  The bytes are canonical (9.2); `=` between two sketches is not defined (use
  `hll_cardinality`).
- **Ledger tables.** A ledger table cannot be distributed (2b 11.8). A local
  ledger table may hold sketch columns; the ledger hashes the row content as it
  does any user-defined type, and canonical bytes make the hash repeatable.
  **[Needs verification]** how `kingres_ledger` hashes a column of a type that has
  no built-in canonical form (by output function or by `send`); both are
  deterministic for these types.

### 10.1 Maintaining a rollup

Three ways, in order of preference. All are one statement in one transaction
(atomic across groups by two-phase commit).

1. **Colocated rebuild of a window (P6, widened).** The rollup and the source
   table are in one colocation group and the aggregation is closed by the key:

   ```
   INSERT INTO page_views_hourly
   SELECT customer_id, date_trunc('hour', ts), count(*), hll_add_agg(visitor_id),
          topn_add_agg(page_id), tdigest_agg(latency_ms)
   FROM events WHERE ts >= $1 AND ts < $2 GROUP BY 1, 2
   ON CONFLICT (customer_id, hour) DO UPDATE
     SET views = EXCLUDED.views, visitors = EXCLUDED.visitors,
         top_pages = EXCLUDED.top_pages, latency = EXCLUDED.latency;
   ```

   2b 11.7 (P6) requires "no `ON CONFLICT DO UPDATE`". The reason in 2b is the
   router's: applied per batch, a second row for one key would update the row the
   previous batch inserted. A P6 statement is not batched: each group runs the
   whole statement once over its own rows, so the check "cannot affect row a
   second time" works exactly as on one node. **This note proposes widening P6 to
   allow `ON CONFLICT DO UPDATE` when the statement is shipped whole** (the 2b
   conditions of 11.7 hold and the `SET` expressions and the inference
   clause are shippable, 3.3 included). The router path of 2b 11.4 keeps refusing it.
   This is a change to 2b's section 11.7 and must be agreed when 2b step 9 is
   written (section 15).
2. **A `MERGE` with a routed source (M3)**, when the rollup is not colocated with
   the source (for example it is distributed by another column or the source is a
   local staging table):

   ```
   MERGE INTO page_views_hourly t
   USING (SELECT customer_id, date_trunc('hour', ts) AS hour, count(*) AS views,
                 hll_add_agg(visitor_id) AS visitors
          FROM events WHERE ts >= $1 AND ts < $2 GROUP BY 1, 2) s
     ON t.customer_id = s.customer_id AND t.hour = s.hour
   WHEN MATCHED THEN UPDATE SET views = s.views, visitors = s.visitors
   WHEN NOT MATCHED THEN INSERT (customer_id, hour, views, visitors)
        VALUES (s.customer_id, s.hour, s.views, s.visitors);
   ```

   The source is aggregated on the groups (P3/P4 with sketch pushdown); only the
   aggregated rows (one per customer and hour) reach the origin and are routed. The
   rows carry the sketch as text (3.1: a column of an allowed type is carried).
3. **Incremental union.** When new rows are added and the rollup holds the older
   rows, the update is a union: `SET visitors = kingres_sketch.hll_union(t.visitors,
   s.visitors)`. The function is allowed (3.3), so it is shippable in a `MERGE`
   fragment. **A union is idempotent for `hll` only.** Union of the same window twice
   leaves an `hll` unchanged and **double counts** a `topn` (frequencies add) and a
   `tdigest` (weights add). A rollup job must either rebuild a full window with
   `SET col = s.col` (way 1 or 2) or add each source row to the rollup once. The
   README of `kingres_sketch` has a table of the three types and this rule.

Row router, `INSERT ... SELECT ... ON CONFLICT DO NOTHING` and plain inserts into
the rollup work as in 2b (the router carries sketch values as text). The router
refuses `DO UPDATE` as in 2b, which is why ways 1 and 2 exist. Retention of old
rollup rows and their partitions by time is part 2
([analytics-phase12-storage.md](analytics-phase12-storage.md)).

## 11. Settings, all of them

| GUC | Default | Context | Meaning |
|---|---|---|---|
| `kingres_shard.enable_repartition_join` | on | `USERSET` | 4.7. |
| `kingres_shard.repartition_max_bytes` | 64MB | `USERSET`, 1MB to 512MB | 4.6. |
| `kingres_shard.enable_distributed_merge` | on | `USERSET` | Off: every shape of 7.2 is refused as in 2a (a message that says the switch is off). |
| `kingres_shard.merge_max_source_bytes` | 64MB | `USERSET`, 1MB to 512MB | 7.4, 7.5. |
| `kingres_shard.enable_sketch_pushdown` | on | `USERSET` | Off: sketch aggregates are not shipped and are aggregated at the origin (2b 5.4); the statement answers the same, slower (hll exactly the same bytes). Requires `enable_partial_aggregation`. |
| `kingres_shard.count_distinct_mode` | `exact` | `USERSET` | 9.8. |
| `kingres_shard.count_distinct_error_rate` | 0.01 | `USERSET`, 0.0005 to 0.1 | 9.8. |

`kingres_sketch` has no setting. Settings that only choose among plans that give
the same answer, or that fail instead of planning, grant nothing. The one setting
that changes results is `count_distinct_mode`, and it is documented as such.
They are listed in the `security` regression test, defined with
`MarkGUCPrefixReserved`, and `enable_pushdown = off` or `enable_distributed_scan
= off` switch off everything here (the same rule as 2b 13).

## 12. Control plane and REST contract (additive)

### 12.1 What the administrator does

The web console's database page gets a section "Approximate aggregates"
(section 13). Enabling it for a database:

1. installs `kingres_sketch` in that database on every server (the extension
   is not preloaded, so no restart);
2. checks that every server runs the same extension version and the same hash;
3. records the setting and applies the database defaults
   (`kingres_shard.count_distinct_mode`, the error rate).

Turning it off sets `count_distinct_mode = 'exact'` and
`kingres_shard.enable_sketch_pushdown = off` for the database and records it. It
**never drops the extension** (a table may have sketch columns and a dropped type
would fail the drop or destroy data); the console says so. Turning it back on
repeats the steps and is idempotent.

### 12.2 The task and its steps

New task kind `database.approximateAggregates`, subject type `database`, subject
id the database name (the same subject as `database.update`). Steps, in the
naming style of the existing tasks (sentences, one step per server where the
action is per server, `perServer` of `ops/databases.go`):

| # | Step | What it does | Idempotent |
|---|---|---|---|
| 1 | `Check the servers` | For every server that is a member of the cluster: reachable, the database exists, and `pg_available_extensions` lists `kingres_sketch` with the same `default_version` on all of them. Fails with `extension_unavailable` (a server lacks the package) or `extension_version_mismatch` (two default versions). | yes |
| 2 | `Install the extension on server <id>` (one step per server) | `CREATE EXTENSION IF NOT EXISTS kingres_sketch`; if installed at an older version, `ALTER EXTENSION kingres_sketch UPDATE`. Runs as the service superuser connection, as `installExtensions` does. | yes |
| 3 | `Verify versions and hashes` | On every server: `extversion` equal to the expected version; `kingres_sketch.self_test()` equal on all servers (9.2). Fails with `extension_version_mismatch` or `sketch_hash_mismatch` and names the servers. | yes |
| 4 | `Apply the database settings` | `ALTER DATABASE <db> SET kingres_shard.count_distinct_mode = '<mode>'`, `... count_distinct_error_rate`, `... enable_sketch_pushdown` on every server (a database setting is stored per server in `pg_db_role_setting`; the existing durability setting is applied the same way, `setDurability`). | yes |
| 5 | `Record the settings` | Upserts the row of the database in `kingres_cp.database_setting` (new columns, 12.3). | yes |

No step is `NonIdempotent`. The task is cancellable before step 2 and between steps;
a cancelled or failed task leaves the cluster in a state the same task can resume
(an installed extension is harmless). Progress is the step list with
`itemsDone`/`itemsTotal` on the per-server steps. The task runs on the leader as
other cluster-wide tasks do (`control-plane-3.md` D6); a conflicting second
request (another `database.*` task on the same database) is 409
`operation_in_progress`.

### 12.3 Storage in the control plane

`kingres_cp.database_setting` (migration number: the next free in
`internal/sysdb/migrations`) gets three columns, all defaulted so that older rows
read as "off":

```
approx_enabled          boolean NOT NULL DEFAULT false
approx_count_distinct   text    NOT NULL DEFAULT 'exact' CHECK (approx_count_distinct IN ('exact','approximate'))
approx_error_rate       real    CHECK (approx_error_rate BETWEEN 0.0005 AND 0.1)
```

The state of each server (installed version) is read live from the servers, not
stored.

### 12.4 REST contract (additive, a minor revision)

The contract revision is the next minor (**[Recommendation]** the revision that
carries these additions is 0.7.0 if no other phase takes it first; clients must
tolerate unknown codes and properties already, per the `ProblemCode` text).

Paths:

```
GET /databases/{databaseName}/approximate-aggregates
  operationId: getApproximateAggregates
  tags: [Databases]
  x-required-role: kingres_readonly
  200: ApproximateAggregates ; 401 403 404 405 422 503 as every operation

PUT /databases/{databaseName}/approximate-aggregates
  operationId: configureApproximateAggregates
  tags: [Databases]
  x-required-role: kingres_admin
  requestBody: ApproximateAggregatesRequest
  202: AcceptedTask (Task kind database.approximateAggregates)
  400 401 403 404 405 409 422 503
  409: conflict | operation_in_progress | extension_version_mismatch
  422: unsupported_operation (sharding off is NOT a condition: the extension works without sharding) | extension_unavailable
```

Schemas:

```
ApproximateAggregates:
  required: [enabled, extensionVersion, countDistinct, errorRate, servers]
  enabled: boolean                                  # the recorded setting
  extensionVersion: string | null                   # the version every server runs; null if none or not equal
  countDistinct: enum [exact, approximate]
  errorRate: number | null                          # null when countDistinct is exact
  servers: array of { serverId: string, installedVersion: string | null,
                      state: enum [ok, notInstalled, versionMismatch, unreachable] }
ApproximateAggregatesRequest:
  required: [enabled]
  enabled: boolean
  countDistinct: enum [exact, approximate]          # default exact; approximate needs enabled=true
  errorRate: number, 0.0005 to 0.1, default 0.01    # only with countDistinct approximate
```

A request with `enabled: false` and `countDistinct: approximate` is a 400
`validation_failed` with the field issue. The `Database` response gains one
optional property `approximateAggregates: { enabled, countDistinct }` (not in
`required`, so existing clients are unaffected); `listDatabases` carries it too.
The audit action is the operation id; `AuditAction` gains
`configureApproximateAggregates`. `TaskKind` gains `database.approximateAggregates`;
the table "Task kinds and subjects" of `kingres/api/README.md` gets one row.
`ProblemCode` gains: `extension_unavailable` (422: a server's package lacks the
extension or the database lacks it; step 1), `extension_version_mismatch` (409:
steps 1 and 3), `sketch_hash_mismatch` (409: step 3, servers that disagree on the
hash of the known-answer values). The `components/responses/*` references are the
existing ones.

Per the rule of `documentation.md` ("`kingres/api/openapi.yaml`: an endpoint
changes (before the code does)"), the contract file is changed first, then the
generated types of the panel (`npm run api:generate`) and the Go contract tables
(`internal/contract/operations_gen.go`).

### 12.5 Who may do what

- Enable or disable: `kingres_admin`. A role is **not** granted by the console
  the right to use the functions: after the extension is installed, `EXECUTE` on
  its functions is the default `PUBLIC` privilege of any function, and the use of
  a sketch needs only `SELECT` on the data it summarizes, so a sketch cannot show
  rows a role could not read. **[Recommendation]** the install step revokes
  `EXECUTE` from `PUBLIC` on the functions and grants it to the roles that can
  already use the database's tables? That is hard to maintain and gives little:
  keep `PUBLIC` and document it; a role that must be kept out of the sketch
  functions revokes them by hand.
- Read the state: `kingres_readonly` and above. `kingres_stats` cannot
  (`getApproximateAggregates` is `kingres_readonly`).
- Use of the sketch functions in statements: any role with `SELECT` on the tables,
  through the query workbench or any client; the workbench runs as the signed-in
  role.

## 13. Web console

Pages and components follow `kingres/web/README.md` ("Adding a page",
"Conventions").

- **Database page (`DatabaseDetailPage.vue`)**: a new card "Approximate
  aggregates" next to the ledger card. It shows, from `getApproximateAggregates`:
  the state (off or on), the extension version, the `count(DISTINCT)` mode and
  error rate, and a table of servers with `installedVersion` and `state`
  (icon plus text, `SafeName` for server ids). Text under the title says in plain
  words: "Adds the sketch types hll, topn and tdigest to this database on every
  server. Approximate results are estimates with a stated error. The exact
  count(DISTINCT) is the default and changes only if you choose the approximate
  mode below."
- **Action**: a button "Configure" that opens `ApproximateAggregatesDialog.vue`
  (modelled on `EnableLedgerDialog.vue` and `TaskActionDialog`): a switch
  "Enable on this database", a select "count(DISTINCT)" (Exact, Approximate) and a
  field "Error rate (percent)" (0.05 to 10, default 1), a statement of the
  consequence ("approximate counts can differ from exact counts by the error rate;
  turning this off never removes the types") and the usual confirmation. The submit
  is `configureApproximateAggregates`; the dialog follows the returned task with
  `useTask` (the five steps, `aria-live` summary), invalidates the database queries
  and the card, and shows `extension_unavailable` and `extension_version_mismatch`
  with their own explanations through `describeProblem` (`Record<ProblemCode, ...>`
  makes the compiler require the new codes).
- **Roles**: the button is a `GateButton op="configureApproximateAggregates"`, so an
  operator or a read-only user sees it disabled with "Requires the kingres_admin
  cluster role". A `kingres_readonly` user sees the whole card read-only. A
  `kingres_stats` user does not reach the page. A server that does not know the
  operation (an older server) answers `unsupported_operation`, shown as calm
  information as for any such case.
- **Workbench (`WorkbenchPage.vue`)**: it runs `EXPLAIN` as any statement
  through `executeQuery` (the result is rows of text). The panel shows a plan
  result (first column `QUERY PLAN`) in a monospaced block and a one-line summary
  above it, derived by matching the text lines of 4.12, 12 of 2b and 7.2:
  "Path: repartitioned join" / "colocated join" / "partial aggregate" / "scan";
  "Not used: <reason>" taken from the `Pushdown:` and `Repartition:` lines;
  "Rewritten: count(DISTINCT) -> approximate". The summary is a convenience
  derived from text, never parsed for any decision; when no line matches it is
  absent. A short help text next to the workbench lists the sketch functions
  when the database has the extension (from `getApproximateAggregates.enabled`). An
  `EXPLAIN ANALYZE` of a statement that changes data runs it (as on any
  PostgreSQL), and the existing workbench note about statements that change data
  covers it. **[Recommendation]** add one sentence to that note for `MERGE`.
- **Task list and notices**: `database.approximateAggregates` gets a label in
  `kind-labels` (tests `kind-labels.test.ts` and `contract.test.ts` require it).
- **Mock API** (`src/mocks`): handlers for both operations, a state with per-server
  versions, switches for `extension_unavailable` and `extension_version_mismatch`;
  never more permissive than the contract.

## 14. Tests

Rules of `testing.md` and 2b 16 apply (no dependence on a developer's machine, no
flaky test, assertion build for C changes, a bug fix starts with a test). Test
file numbers continue after 2b's `040` to `050`; the integrator renumbers.

### 14.1 `kingres_shard` pg_regress

| File | What it proves |
|---|---|
| `repartition_plans` | Fictitious two-group catalog: every role assignment of 4.3 gets the `EXPLAIN` of 4.12; every refusal rule (no key equality, different type, non-default collation, an expression join, a preserved broadcast side, an estimate above the bound, cost) gives the 2a plan and the reason line; the two GUCs switch only their path; a join on non-key columns of two tables of one colocation group is repartitioned or not as the cost says. |
| `merge_plans` | Each row of the matrix 7.2: `EXPLAIN` of the accepted shapes shows M1, M3, M4, M5, M6; each refusal gives the message of its row before anything runs; `enable_distributed_merge = off` gives the 2a messages. |
| `sketch_shippable` | The rule 3.3: functions of `kingres_sketch` ship when the extension is installed at an allowed version and do not when it is not; a look-alike function of the same name in `public` does not ship; a function of a user extension does not; `count_distinct_mode = approximate` rewrites exactly the statements of 9.8 and no other. |
| `security` (extended) | The new GUCs; no function without a pinned `search_path` in either extension. |

### 14.2 `kingres_shard` TAP (three nodes and the comparison harness)

The harness `KingresShardCompare.pm` gains plan profiles `repartition off`,
`merge off`, `sketch pushdown off` next to those of 2b, and the data of 2b 16.2
(NULL keys, skew with one bucket holding half the rows, an empty table, a group
that owns no row of a table, text with the hostile characters, floats with `NaN`
and `-0`) plus: a column with many duplicates of the join key (fan-out), keys of
type `int4`, `int8`, `text`, `uuid`, `date`, `numeric`, `float8`, and a join whose
key has no match at all.

| File | What it proves |
|---|---|
| `060_repartition_equiv` | For a generated list (seeded, `KS_SEED` printed) of two-, three- and four-table joins on non-key columns, with every join type of 4.3, broadcast and routed sides, reference tables, aggregates, `HAVING`, order and limit: the result with P7 equals the result with `enable_repartition_join = off` (the scan) and the single node. The plans of a fixed list are checked to be repartitioned, so the test cannot pass by never choosing it. Float results are rounded to 9 digits, `array_agg` compared sorted. |
| `061_repartition_edges` | `NULL` keys on the preserved and on the other side; a key with 10 000 duplicates; skew above `repartition_max_bytes` (the run-time error of 4.6, `54000`, raised before any row); a bound that is exactly met; an empty shipped side; a group set pruned by a restriction; a dual plan with three groups; the hostile values of 2b 16.4 through the shipped path (every character, `bytea`, `jsonb`, arrays, ranges, `money`, `numeric` of 1000 digits, `timestamptz` before the year 1); a column dropped from the table; read-your-writes after an `INSERT` in the same transaction; `REPEATABLE READ`; a cursor fetched in parts and closed early; a prepared statement six times and with `force_generic_plan`; a transaction that writes on two groups then runs a repartitioned join and commits (two-phase commit works, no temporary object exists: `pg_prepared_xacts` after `PREPARE` of the remote side). |
| `062_repartition_moves` | After a `split_bucket` (phase 10) of the anchored group and of the shipped table's group, repartitioned joins, `MERGE` routing and rollups still agree with the single node (children with mixed moduli; a split that commits between shipping and fragments gives `KS001`). A bucket move of the anchored colocation group (phase 4) runs in a loop while 3 nodes run repartitioned joins and the same statements with the option off: every answer is correct, or `KS001`; none returns a row twice or misses one (the check of 4.10); the same with a move of the colocation group of the shipped table; a move that commits between the shipping and the fragments (fault points `rp_after_ship`, `rp_before_fragment`, named in the style of 2b, `:wait`); a long read transaction that holds the switch back. |
| `063_merge_colocated` | M1: every action type, `WHEN` conditions, `RETURNING merge_action()`, `NOT MATCHED BY SOURCE` with a group whose source share is empty (the requirement of 7.3: the unmatched target rows of that group are still handled), a target and source of one colocation group, a reference source, a statement restricted by a key in `ON`, a source with no rows. Row counts, the target afterwards and the `RETURNING` set equal the single node's. |
| `064_merge_routed` | M3: sources that are a local table, a reference table, a sub-select with an aggregate, `VALUES`, another colocation group, a distributed table on a non-key join; `NULL` keys; a source row matching two target rows (allowed) and two source rows matching one target row (the `cannot affect row a second time` error on the group, whole statement rolled back); a source above `merge_max_source_bytes`; refusals of 7.2 each with its message; a rollup refresh of 10.1. |
| `065_merge_reference_local` | M4, M5 (copies equal on all nodes after, row counts, a failure on one node rolls back everywhere, a concurrent writer is serialized by the advisory lock), M6 (a local target with a distributed source; a local ledger table as the target: the ledger's capture trigger fires once per row, `ledger_verify` clean). |
| `066_merge_faults` | The fault points `merge_before_first_group`, `merge_between_groups`, `merge_before_commit` with `:error`, `:kill`, `:wait`; a group killed in the middle; the origin killed after the decision is logged; after each: `pg_prepared_xacts` empty after recovery, every node's target equal to the before-state or to the after-state, never a mix. A bucket move during a `MERGE` gives `KS001` and no change. |
| `067_sketch_pushdown` | Every sketch aggregate over data spread over the groups, scalar and grouped, `FILTER`, `HAVING`, closed (`GROUP BY` the key) and open: P3/P4 plans are chosen when expected; **hll results are identical to the scan's** (same sketch bytes, `hll_add_agg` on the pushdown path and at the origin); topn and tdigest answers are within the bounds of 14.3 of the exact answer and of the scan's; `enable_sketch_pushdown = off` gives the scan plan; version mismatch (`ext=` marker) gives `KS001` with the detail; `count_distinct_mode = approximate` rewrites and its answer is within the bound of the exact `count(DISTINCT)`; partialize on each plan strategy on an assertion build. |
| `068_rollup` | The refreshes of 10.1: P6 with `DO UPDATE` (the widened rule), `MERGE` with a routed source, incremental `hll_union`; re-running a window leaves `hll` unchanged and shows the double counting of `topn` and `tdigest` unions (the documented rule); the rollup read back through P3/P4. |
| `069_sketch_security` | A forged fragment of a valid session that names a function of another extension, a look-alike in `public`, or `kingres_sketch` at a different version, is refused with `KS001` and runs nothing; a forged `repartition` sub-select of the wrong shape is refused; privileges: a role without `SELECT` on the table cannot learn anything through a sketch of it; a role without `INSERT` on the rollup cannot `MERGE`. |

### 14.3 `kingres_sketch` tests

pg_regress (`sql/`, `expected/`): types' input and output, every function and
aggregate with edge arguments (empty, `NULL`, precision 4 and 18, capacity 1 and
100000, compression 10 and 10000, long items, `NaN`), the errors, canonical bytes
(the same content built in two orders has the same bytes: hll always; topn and
tdigest compared after a rebuild from the same bytes), `send`/`receive` round trip,
upgrade script from 1.0 (once there is a 1.1), `security`, and `self_test()`.

TAP (single node and the three-node harness):

- **Accuracy, with stated bounds and fixed seeds.** Because the hash is fixed, a
  test is deterministic: it is not a statistical test that can flake.
  - *hll:* for cardinalities 0, 1, 10, 100, 1000, 10^4, 10^5, 10^6 and 200 seeded
    data sets each, at `p` = 10, 12, 14, 16: the relative error is at most 4 x
    `1.04 / sqrt(2^p)` or an absolute error of 2, whichever is larger. Union of
    sketches of a random partition (2 to 8 parts, with overlaps) equals the
    sketch of the whole data, byte for byte. Sparse and dense forms give the same
    estimate for the same registers. A run at p = 14 over 10^6 values on the
    three-node cluster through P3 equals the single-node sketch.
  - *topn:* on Zipf-distributed streams (exponent 0.8, 1.0, 1.2) of 10^6 items
    over 10^5 distinct, with capacity 100, 1000: for every returned item
    `frequency - max_error <= true <= frequency` and `max_error <= N / k`; every
    item with true frequency above `N / k` is present; after random merges of
    2 to 16 partial summaries the same invariants hold. The top 10 by
    `frequency` equals the true top 10 for the 1.2 exponent at capacity 1000.
  - *tdigest:* on uniform, normal, log-normal and tie-heavy data of 10^6 values,
    `delta` = 50, 100, 500: the rank error of `quantile(q)` for q in {0.001, 0.01,
    0.1, 0.5, 0.9, 0.99, 0.999} is below the thresholds written in the expected
    file, set from the first measured values with a margin of 2 and reviewed (the
    values quoted in 9.5 are the starting thresholds); the digest has at most the
    documented number of centroids; `min`, `max` and `count` are exact; merging
    16 digests meets the same thresholds.
- **Fuzz.** Random bytes, truncated values, flipped bits in valid values and
  out-of-range lengths go through the input function, `receive`, the `bytea`
  casts and `*_agg_deserial`: every one is an error or a valid value; on an
  assertion build and under `valgrind` or the address sanitizer if available
  (**[Needs verification]** the project's sanitizer setup).
- **Parallel.** A parallel aggregate over a local table (forced with
  `parallel_setup_cost = 0`) equals the serial result for hll (identical) and is
  within the bounds for the others, which proves `COMBINEFUNC`, `SERIALFUNC` and
  `DESERIALFUNC` independently of `kingres_shard`.
- **Memory.** The state of each aggregate stays within the figures of 9.7 for the
  parameters' limits.
- **Optional cross-check.** If `postgresql-hll` or `tdigest` is installed in the
  test environment, the tests compare estimates with theirs within the
  combined bounds. They are never required and never bundled.

### 14.4 Control plane and panel tests

- Go (`internal/ops`, `internal/httpapi`, `cmd/kingresd/*_e2e_test.go`): unit tests
  of `PrepareApproximateAggregates` (validation of the request, the 400 for
  `enabled: false` with `approximate`); the task definition lists the five steps
  (and one `Install` step per server); `TestApproxInterruptedAtEveryStepResumes`
  in the style of `TestJoinInterruptedAtEveryStepResumes` (SIGKILL at each step,
  the second process finishes, the extension is installed once, the setting is
  recorded once); a cluster test (`internal/clustertest`) with three servers: enable,
  every server has the extension at one version, `ALTER DATABASE ... SET` is in
  `pg_db_role_setting` on every server, a server stopped makes step 1 fail with the
  retryable error and the task resumes when the server is back; a server with the
  extension at another version fails with `extension_version_mismatch` and changes
  nothing; disable leaves the extension installed; a read-only role gets 403 on the
  `PUT` and 200 on the `GET`; a `kingres_stats` role gets 403 on both; the audit
  log records the operation; the contract tests (`contract_test.go`) see the new
  operations, codes and the kind.
- Panel (Vitest with the mock API, `tests/`): `approximate-aggregates-0-7.test.ts`:
  the card for each state, the dialog validation, the task steps shown, each new
  problem code shown with its own text, the disabled button with the reason for an
  operator and a read-only user, no card for `kingres_stats`, hostile server ids
  through `SafeName`; `pages-a11y.test.ts` includes the new card and dialog (axe);
  `kind-labels.test.ts` and `contract.test.ts` cover the kind, the operations and
  the generated files; the workbench summary test feeds the `EXPLAIN` texts of 4.12
  and 12 of 2b and checks the one-line summary, and checks that a text with no
  recognized line gives no summary. A real-browser layout test covers the dialog at
  a narrow width.

## 15. Implementation order

Each increment is mergeable and testable alone, leaves the cluster correct if the
next never lands (a new path is chosen by cost or by an explicit setting, among
correct paths, and has a switch), and is sized for one engineer and one reviewer.
Steps 1 to 5 of the repartition work need 2b steps 1 to 5 (fragment scan, pushdown
analysis, closed queries, partial aggregation) and 2b step 7 for the transport
format. The `kingres_sketch` steps do not depend on `kingres_shard` at all until
step S4.

| Step | Content | Tests |
|---|---|---|
| S1 | `kingres_sketch` skeleton: control, build files, KP-0004, the header and canonical-bytes machinery, validation, `self_test()`; type `hll` with all functions and aggregates, parallel aggregate | the `kingres_sketch` regression, the hll accuracy, fuzz and parallel tests |
| S2 | `topn` | its accuracy and fuzz tests |
| S3 | `tdigest` and `approx_percentile` | its accuracy and fuzz tests |
| S4 | In `kingres_shard`: the allowed-extension rule (3.3), the marker keys (3.2), sketch pushdown through 2b P3/P4 (needs 2b step 5), `enable_sketch_pushdown` | `sketch_shippable`, `067_sketch_pushdown` (without the rewrite), `069` |
| S5 | `count_distinct_mode` rewrite (9.8) | `sketch_shippable`, `067` rewrite part |
| C1 | Control plane: migration, task, `Check the servers` to `Record the settings`, the operations in `openapi.yaml` (first), the audit action | Go tests of 14.4 |
| W1 | Web console: the card, the dialog, the mock, the workbench summary | panel tests |
| J1 | Repartitioned join, single repartition with a routed side and the 2a scan child, no broadcast: the analysis (4.3), `RepartPlan`, the shipped sub-select (3.1), the tuplestores and the bound, P7 at the join level, `EXPLAIN` | `repartition_plans`, `060` (inner and outer, routed), `061` |
| J2 | P3/P4/P5 above P7 (closed with the anchor component), broadcast sides, dual repartition, the cost tuning | `060` complete |
| J3 | Bucket-move tests and fault points of the repartition | `062` |
| R1 | Widen 2b P6 to `DO UPDATE` for a whole-statement push (agree with 2b step 9 first); rollup tests | `068` (way 1) |
| M1 | `MERGE` M1 (colocated) and the refusals of 7.2 with their messages | `merge_plans`, `063` |
| M2 | M3 (routed source) | `064`, `068` (way 2, 3) |
| M3 | M6 (local target with a fenced source) and the ledger case | `065` (M6 part) |
| M4 | M4 (reference target, reference sources) | `065` (M4 part) |
| M5 | M5 (reference target, broadcast source) and `066` | `065` (M5 part), `066` |
| V | Version bump of `kingres_shard` to the next free version after 2b and phases 9 to 11 (this note's changes add no SQL object: the upgrade script is a guard and a comment, as in 2b section 14), `kingres_sketch` 1.0 in the packaging, README updates (the "Limitations" of the 2a text about `MERGE`), `core-patches.md` row | `upgrade`, package test |

Steps S1 to S3 can be done in parallel with J1 and M1 by other engineers. The
`kingres_shard` version has to move because the fragment protocol gains markers
(3.2); a node that does not know them answers `KS001` (not a wrong answer), so a
rolling upgrade sets `enable_repartition_join`, `enable_distributed_merge` and
`enable_sketch_pushdown` off until the last node runs the new version (as 2b 14
says for its settings).

## 16. Known limits after this phase

- A repartitioned join moves each shipped row twice through the origin. The
  origin's network link and CPU bound the throughput; a moved side larger than
  `repartition_max_bytes` per group is not repartitioned (the scan answers) or, if
  the estimate was wrong, raises `54000` (4.6). Skew can trigger it.
- No repartition by temporary tables, no group-to-group transfer, no inline
  `VALUES` for tiny sides, no cross-type key equality (`int4 = int8`), no
  repartitioned semi or anti join, no repartition of a sub-select or a view, no
  statistics for the shipped side on the group (the planner uses defaults for its
  columns).
- `READ COMMITTED` can show a group in two states for a statement that reads a
  shipped table and an anchored table on the same group (4.9a).
- A `KS001` during a repartitioned join or a `MERGE` must be retried by the client.
- `MERGE`: no `INSERT` into a table that needs a volatile, identity or
  `serial` value that the statement does not give; the distribution column cannot
  be updated; a source larger than `merge_max_source_bytes` per group is refused;
  the source of M3 is read at the start of the statement; no `MERGE` on a
  standby; the `ON` clause must equate the distribution column with a source
  expression.
- Sketches: estimates, not exact values; `topn` and `tdigest` merges are
  order dependent, so two runs of one query on changing data can return slightly
  different (within-bound) answers; a union of `topn` or `tdigest` is not
  idempotent; floating-point, date and time items are not accepted by `topn`;
  `count(DISTINCT)` with several arguments is not rewritten; the `GROUP BY` with
  a very large number of groups multiplies the partial state size; no pushdown for
  an aggregate over a user-defined type; sketch types have no ordering or equality;
  a database that drops the extension loses its sketch columns (the console never
  drops it).
- A rollup job must not re-add a window into `topn` or `tdigest` with a union (10.1).
- A third-party extension's functions are never shipped (3.3).

## 17. Open questions, with a recommendation each

| # | Question | **[Recommendation]** and decision |
|---|---|---|
| Q1 | Transport of shipped rows: temporary tables or array parameters? | Array parameters in this phase (4.2). Revisit with a read-only helper session when the bound becomes the complaint. |
| Q2 | Should a runtime overflow of `repartition_max_bytes` fall back to the scan? | No in this phase: an error before any row, with the switch as the remedy (4.6). The scan node holds the child paths, so a later increment can run the scan instead; keep the interface (the child paths are in the custom scan) so that it is possible. |
| Q3 | Cross-type key equalities (`int4 = int8`)? | No in this phase (same type). Add with a test of hash compatibility across the integer family. |
| Q4 | Should P7 be created when a P2 (colocated) plan exists? | Yes, but it cannot win by cost; kept so that a mistaken colocation proof shows as a difference in the equivalence test rather than hiding. |
| Q5 | `MERGE` M5 (reference target with a non-reference source): in this phase? | Yes, as the last `MERGE` increment, with its own fault tests; if it is cut for time the matrix row stays refused. |
| Q6 | Volatile or identity defaults in `MERGE ... INSERT` | Refuse (7.2). The alternative (the group's own sequence) gives values that collide in meaning across groups. |
| Q7 | Bundle `postgresql-hll` or `tdigest` rather than write them? | No (9.1). One extension, one version, one format. Licence is not the obstacle for those two; it is for `postgresql-topn`. |
| Q8 | Is `EXECUTE` on sketch functions restricted to a role? | No. PUBLIC, documented (12.5). |
| Q9 | `count_distinct_mode` default and scope | `exact`; per database by the console; any session may override for itself (9.8). |
| Q10 | Widening 2b P6 to `ON CONFLICT DO UPDATE` | Propose it (10.1) and agree when 2b step 9 is written; if refused, rollups use `MERGE` (way 2), which needs no change to 2b. |
| Q11 | The allowed extension list: code or catalog | Code (3.3). |
| Q12 | Which placement check do the new fragments use: the map version `V` of 2b 10.2, the placement hash of phase 4, or the epoch of phase 10? | The placement check of the colocation group a statement reads, with `colocation.placement_epoch` once phase 10 lands (it replaces the hash), because it fails only on a switch or split of that group. 2b 10.2 and its step 1 should say so (section 18). |
| Q15 | How do rows get their group: `hash % bucket_count`? | Never. The catalog's bucket lookup with each bucket's own modulus (phase 10). Tests split a bucket and run every routed path (`062`, `064`, `068`). |
| Q13 | Name of the contract revision | Next minor (12.4). |
| Q14 | Do the console's switch and the error rate apply to all existing views and functions? | They apply per session at planning time (9.8); the dialog says so. |

## 18. Contradictions found in existing documents and code

- **Phase 10 (bucket split).** `hash % bucket_count` is not valid for a split
  group. The routing of this note (4.4, 7.4, rollups) uses the bucket lookup and
  the epoch, and the wording of 2b 11.6 (`kingres_shard_bucket`) must follow.
- **2b section 10.2 and README (phase 4).** 2b says every fragment "carries the
  map version `V` ... equality is required"; the committed README ("The map
  version rule") and the phase 4 notes say the scan compares the **placement
  hash** before and after opening its cursor, and that it "replaces the map
  version in the comparison" because the version counts every catalog change. 2b's
  step 1 still lists "the map version in fragments and its check after
  planning". This note uses the placement check for placed data (4.10, 7.7) and
  needs 2b step 1 to be written against what phase 4 implemented.
- **2b 11.7 (P6) forbids `ON CONFLICT DO UPDATE`** for a statement that is shipped
  whole, although the reason it gives (a second row updating the row of an earlier
  batch) does not apply to a statement that is not batched. 10.1 proposes the change.
- **2b 5.1 rule 1 and 7.2** say "built in" for functions and aggregates. Nothing
  can be pushed that is in an extension, so 3.3 widens the rule; 2b's 6.1 rule
  that a sub-select makes a statement not a unit also needs the exception of 3.1
  for the shipped sub-select, in the group's walker.
- **2b "What phase 2a deferred", `MERGE` that reads a distributed table: Out.**
  Section 7 puts the cases back in. Its row "A reference-table write that reads a
  distributed table: Out" is lifted by M5.
- **README, "Limitations in phase 2a"**: "`MERGE` ... are refused" and "UPDATE ...
  FROM, DELETE ... USING and MERGE that read a distributed table and write another
  table are refused". After the `MERGE` increments those lines change; `UPDATE ...
  FROM` and `DELETE ... USING` stay refused (7.6's fence is for `MERGE` only; it could
  be applied to them in a later phase).
- **`openapi.yaml` description of `updateClusterSettings`** says first enable of
  a feature needs a restart "because the extensions load through
  `shared_preload_libraries`". `kingres_sketch` does not, so enabling it needs no
  restart and is not a "feature flag" of `ClusterSettings.features`.
- **`documentation.md`** says the architecture document is the source of truth for
  design and only its owner edits it; this note is a design file next to the
  others and the architecture document is not changed by it.

## 19. Needs verification

| # | Item | What would settle it |
|---|---|---|
| N1 | A `CustomScan` that has child plans: who initializes `custom_ps` (4.5, section 2.1) | Read `ExecInitCustomScan` and `ExecEndCustomScan`; write the scan with a test that runs a child. |
| N2 | `pg_get_querydef` of the copy with an `RTE_RELATION` replaced by an `RTE_SUBQUERY` over `unnest` prints a valid statement for every case, including attributes that are `NULL::type` placeholders (3.1) | `repartition_plans` prints each fragment; each is parsed and planned. (2b V3, V13 are the same risk.) |
| N3 | Largest safe size of one `text[]` parameter literal: `MaxAllocSize` at the origin (libpq buffer) and at the group, and the protocol message length (4.6) | A test that ships 500 MB for one group; read `pqPutMsgEnd` limits and `palloc`'s. |
| N4 | `hashfloat8extended` treats `-0` and `0` alike, and the hash of the integer, text and `uuid` types is independent of byte order and platform (R4, 9.2) | The tests with `-0`, `NaN`; `self_test()` on a big-endian machine if one is available, or reading the hash functions. |
| N5 | The form of Ertl's estimator (the `sigma` and `tau` series and `alpha_inf`) and the standard-error constant 1.04 for it (9.3) | Read arXiv:1702.01284 before coding; the accuracy test. |
| N6 | The merge guarantee for Space-Saving summaries (9.4): the theorem in Agarwal et al. is for Misra-Gries | Read the paper; the property tests of 14.3. |
| N7 | The exact centroid bound of the merging t-digest with `k_1` (9.5) | Read arXiv:1902.04023; the test of the centroid count. |
| N8 | The patent position of HyperLogLog++ and t-digest (9.1) | Legal review. |
| N9 | `numeric_out`, `uuid_out`, `int*out`, `textout`, `boolout` do not depend on session settings (9.4) | Read the output functions; a test under hostile settings. |
| N10 | The `MERGE` row of the Citus matrix and the statement about an empty share of a shard (7.2, 7.3): only search summaries were read | Read the Citus pages named in "Sources read" in full, documentation only. |
| N11 | `is_simple_subquery` and `subquery_is_pushdown_safe` treat a sub-select with `OFFSET 0` as in 7.6; `preprocess_rowmarks` then gives it `ROW_MARK_COPY` (7.6) | Read the two functions; `merge_plans` with an assertion build; `065`. |
| N12 | The 2a scan can fetch a column of a type that is not built in (10) | A test with a `kingres_sketch` column in `013`'s style. |
| N13 | How `kingres_ledger` hashes a column of an extension type (10) | Read `kingres_ledger` code; a test. |
| N14 | Rewrite of `Aggref` in the planner hook (9.8) | An assertion-build test. |
| N15 | `ExecInitCustomScan`'s handling of `custom_plans` with parameters and rescan: the saved arrays survive `ExecReScan` (4.5) | `061`'s rescan case. |
| N16 | Which of the map version or the placement hash fails a statement planned with a different set of groups in a dual plan (4.10) | Decide with 2b step 1; `062`. |
| N17 | The sanitizer or `valgrind` setup for the fuzz test (14.3) | `testing.md` and the dev environment. |

## 20. What this note needs from the other phase documents

- **2b:** the corrections of section 18 (placement check, P6 with `DO UPDATE`, the
  allowed-extension rule in 5.1 and 7.2, the sub-select exception of 6.1 for the
  shipped side); the final names of the fault points, `PushdownInfo` and
  `pd->raw`; the kind and marker syntax (3.2) if it changes.
- **Phases 9 to 11 ([shard-phase9.md](shard-phase9.md),
  [shard-phase10.md](shard-phase10.md), [shard-phase11.md](shard-phase11.md)):**
  the version numbers of `kingres_shard` they take, any new SQLSTATE in class `KS`
  (this note uses only `KS001` and the standard `54000`), any new problem code or
  task kind in the REST contract (this note adds three codes and one kind), any
  new `kingres_cp.database_setting` columns and the migration number, and any
  change to the forwarded-session model or to the fragment marker.
- **Part 2 ([analytics-phase12-storage.md](analytics-phase12-storage.md)):** that
  columnar and time-partitioned distributed tables are readable through the scan
  before pushdown (ADR 0022 point 4); whether a time-partitioned rollup table
  changes 10.1's `ON CONFLICT` clause or the key class; whether a columnar table can
  be the shipped side of 3.1 (it is read by its own scan path, which is a child
  path, so it should be); and the retention of rollup partitions.
