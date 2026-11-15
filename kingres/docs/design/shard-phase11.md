<!-- SPDX-License-Identifier: PostgreSQL -->
# kingres_shard: design of phase 11 (distributed schemas: schema-based sharding)

Status: Proposed design, 2026-11-15; not implemented.
Builds on [shard-phase4.md](shard-phase4.md) (bucket moves, extension 0.3),
[ha-phase5.md](ha-phase5.md) (0.4), [shard-phase2b.md](shard-phase2b.md) (0.5),
[shard-phase9.md](shard-phase9.md) (table lifecycle) and
[shard-phase10.md](shard-phase10.md) (elasticity). It produces the next
extension version after phase 10's: **0.8** (see section 5.6), with the upgrade
script `kingres_shard--0.7--0.8.sql`.

Phase 11 of the roadmap in [architecture.md](../architecture.md) ("Phases 9 to
12") adds schema-based sharding. A **distributed schema** is a schema whose
tables all live in one shard group: one tenant or one microservice per schema.
Schemas are placed and moved as units, and a database can be set so that new
schemas are distributed by default. The setting and the placement are
configurable from the web console. Today the extension knows two table kinds:
hash-distributed tables (row-based sharding: a table is split into buckets by a
distribution column) and reference tables (held in full by every node). A table
of a distributed schema is a third kind.

How this note was checked. Claims about PostgreSQL are checked against the 18.7
source or documentation in this tree, with the file or function named. Claims
about the extension are checked against the committed versions of
`contrib/kingres_shard` (`git show HEAD:`; other work changes the working tree)
and its README. Anything not checked is marked **[Needs verification]**, and all
such items are collected in section 19. Items that are decisions of this note,
not facts, are marked **[Recommendation]** and are listed with their reasons in
section 17.

## 1. Sources read

- Project documents: `contrib/kingres_shard/README.md` (committed version, the
  whole file), `shard-mvp.md`, `shard-phase2a.md`, `shard-phase4.md` (including
  its implementation notes), `shard-phase2b.md` (sections 4, 6, 9, 10, 11.6, 11.8,
  13, 14), `ha-phase5.md` (Part C), `ledger-mvp.md` (protection and data model),
  `control-plane-3.md`, the README of `kingres/api` ("Long-running operations and
  tasks", "Task kinds and subjects"), `kingres/web/README.md`,
  [adr/0022-the-complete-scan-stays.md](../adr/0022-the-complete-scan-stays.md),
  [reviews/2026-11-15-kingres-shard-0.3.md](../reviews/2026-11-15-kingres-shard-0.3.md),
  [dev/documentation.md](../dev/documentation.md), and the roadmap section of
  `architecture.md`.
- Code at the last commit (`git show HEAD:`): `src/planner.c` (`planner_hook_body`,
  `analyze_query`), `src/utility.c` (the `ProcessUtility` hook, `statement_text`,
  the propagation loop, `kingres_shard_in_authorized_ddl`), `kingres_shard--0.3.sql`
  (catalog tables, views, function list), `kingres_shard.control`,
  `src/recovery.c` (searched for calls of `resume_bucket_moves` and
  `cleanup_moved_buckets`: none at that commit), `src/catalog.c` (searched for a
  placement counter: none at that commit).
- Contract and panel: `kingres/api/openapi.yaml` at 0.6.0 (`/databases`,
  `/databases/{databaseName}`, `/database-create-options`, tables, colocation
  groups, buckets, bucket distribution, rebalance, tasks, `ProblemCode`,
  `TaskKind`, `TaskSubject`, `Table`, `TableClass`, `ShardingSettings`), the
  control-plane README (task engine: steps, `NonIdempotent`, leases), and the
  panel sources `kingres/web/src/pages` and `src/components` (names only).
- PostgreSQL 18.7 in this tree: `src/backend/commands/schemacmds.c`
  (`CreateSchemaCommand` runs the elements of `CREATE SCHEMA` through
  `ProcessUtility` with `PROCESS_UTILITY_SUBCOMMAND`),
  `src/backend/catalog/pg_namespace.c` (`NamespaceCreate` calls
  `InvokeObjectPostCreateHook`), `src/backend/commands/tablecmds.c`
  (`AlterTableNamespace`), `src/backend/utils/adt/ruleutils.c`
  (`generate_relation_name` qualifies a relation unless `RelationIsVisible`),
  `src/backend/parser/gram.y` (`TABLES IN SCHEMA` publications),
  `doc/src/sgml/logical-replication.sgml` (sequence data is not replicated;
  apply runs with `session_replication_role = replica`, so triggers do not fire
  on the subscriber; the apply process fires row triggers only when enabled).
- Web, Citus (documentation only; Citus is AGPL, no source was read and none is
  used):
  - https://docs.citusdata.com/en/stable/get_started/concepts.html : "Schema-based
    sharding" ("the schema becomes the logical shard within the database"; needs no
    query changes; suits microservices) and "Type 5: Schema Tables" (distributed
    schemas are linked to individual colocation groups; tables created in them
    become colocated distributed tables without a shard key).
  - https://docs.citusdata.com/en/stable/develop/api_udf.html : `citus_schema_distribute(schemaname)`
    (converts a regular schema; the schema is assigned to and moved to a node
    automatically), `citus_schema_undistribute(schemaname)` (back to a regular
    schema; tables and data move back to the coordinator),
    `citus_schema_move(schema_id, target_node_name, target_node_port,
    shard_transfer_mode)` with the modes `auto`, `force_logical`, `block_writes`
    (logical replication, or blocking writes for tables without a replica
    identity).
  - https://docs.citusdata.com/en/stable/develop/reference_workarounds.html ("Schema-based
    Sharding SQL Compatibility"): foreign keys across distributed schemas are not
    supported; joins across distributed schemas are subject to the cross-node SQL
    limitations; creating a distributed schema and its tables in one SQL
    statement is not supported.
  - Not found in the pages I could fetch: the setting `citus.enable_schema_based_sharding`
    and the `citus_schemas` view. I know of them from memory only and use them
    here as names to compare with, not as facts: **[Needs verification]**.

## 2. Goals

1. A tenant or a microservice gets a schema. Every table, sequence, index, view,
   function and trigger of the schema is in one shard group, so that every
   statement that stays inside the schema runs on one node, with the whole
   feature set of PostgreSQL (any unique constraint, any foreign key inside the
   schema, any trigger, any query shape), because nothing has to be split.
2. A schema is placed on a group when it is created, and moved to another group
   as one unit, online, with the phase 4 machinery wherever it fits.
3. A database can be set to shard by row, by schema or by both, and, when schemas
   are allowed, to distribute every new schema by default.
4. An existing schema, with tables and data, can be made distributed, and a
   distributed schema can be made local again.
5. Every statement that touches a distributed schema gets its right answer.
   A `SELECT` that is not run whole on the owning group is answered through the
   row-moving distributed scan ([ADR 0022](../adr/0022-the-complete-scan-stays.md)),
   never refused for being unknown.
6. No core patch. The behaviour is the same as the behaviour of the tables of a
   non-sharded database, apart from the documented limits (section 18).

## 3. What is not in scope

- Splitting a schema across groups, or sharding the tables of a distributed
  schema by row (a table of a distributed schema is on one group).
- Joins that repartition data between distributed schemas (phase 12). Statements
  that touch two distributed schemas on different groups use the distributed scan.
- Foreign keys between distributed schemas, or from a distributed schema to a
  row-distributed or local table, and the reverse (refused, section 7.8).
- Row-level security, rules, materialized views, foreign tables, publications and
  event triggers inside a distributed schema (refused in this phase, section 7.12).
- DDL entered on a node that is not the map group primary (the existing rule of
  phase 2a stays; a later phase may forward it).
- Moving a schema that holds a `kingres_ledger` table (section 11).
- Time partitioning and columnar storage (phase 12), and anything about the
  change of the number of buckets (phase 10): a distributed schema has no buckets
  of its own to split.

## 4. Mechanism in PostgreSQL 18 terms

### 4.1 Hooks

No new hook type is needed. All of these exist in the extension today.

| Hook | Use in phase 11 |
|---|---|
| `planner_hook` (`planner.c`) | Classifies every statement by the schemas of the relations it reads and writes (section 6), routes a closed statement whole to the owning group, plans the rest through the distributed scan. |
| `ProcessUtility_hook` (`utility.c`) | Propagates DDL on objects of a distributed schema to every node in one two-phase transaction (section 7); handles `CREATE SCHEMA`, `ALTER SCHEMA`, `DROP SCHEMA`, `ALTER ... SET SCHEMA`, `COPY`, `TRUNCATE`; refuses what is out of scope. The elements of `CREATE SCHEMA ... CREATE TABLE ...` arrive as separate calls with `context == PROCESS_UTILITY_SUBCOMMAND` (`schemacmds.c`), which the hook skips today (`utility.c`: `context != PROCESS_UTILITY_SUBCOMMAND`); phase 11 refuses the elements where the schema is distributed (section 7.1). |
| `sql_drop` event trigger (`kingres.forget_dropped_tables`) | Removes the catalog rows of a dropped schema table and of a dropped distributed schema. It is the existing `SECURITY DEFINER` function; it gains the new tables and keeps the rule that it acts only while the hook authorized the drop. |
| Executor and custom scans | `Kingres Remote Statement` (a whole statement for another group) and `Kingres Distributed Scan` (a read of one table from one other group) exist; the scan gets a "single source" form for a table of a distributed schema (section 6.6). |
| Background worker | The recovery worker also resumes and cleans up schema moves (section 9.8). |

`object_access_hook` (`OAT_POST_CREATE` for a namespace, called from
`NamespaceCreate`) is **not** used: the hook runs inside the creating
transaction before the statement has finished, it knows nothing about the
statement text that has to be propagated, and a catalog change made there cannot
reach other nodes. The decision to distribute a new schema is taken in the
`ProcessUtility` hook, which has the statement text (section 7.1).

### 4.2 Core patches

None. The fork policy ([ADR 0001](../adr/0001-thin-fork-with-extensions.md)) is
to keep logic in extensions; every mechanism above is a hook that the extension
already uses. KP-0001 (`KINGRES_VERSION`) is unchanged. If the work finds a
need for a patch (for example a hook point that does not exist), it is proposed
in `kingres/docs/dev/core-patches.md` first, not made here.

### 4.3 Catalog

The new catalog objects are in schema `kingres`, are copied to every node as the
other catalog tables are, and are changed on every node in one two-phase
transaction (section 4.4 for how, because the existing full snapshot is too
expensive for a change per tenant).

## 5. The catalog model and the SQL interface

### 5.1 How a distributed schema maps onto the existing concepts

The existing concepts and what each is for:

- **colocation group**: a set of tables with the same bucket count, distribution
  type and placement;
- **bucket**: one hash partition of every table of a group; **`bucket_map`**: which
  shard group owns which bucket, with a state (`active`, `moving`, `orphan`);
- **map version**: one counter that every catalog change raises;
- **bucket move** (`kingres.bucket_move`, `move_bucket`): the record and the
  procedure that move one bucket of a group, with phases `preparing`, `copying`,
  `catching_up`, `switching`, `cleaning`, `done`, `failed`, `cancelled`.

**Decision: a distributed schema is a colocation group of a new kind, with one
bucket.** Concretely:

- `kingres.colocation` gets `kind text` (`'hash'`, the default, or `'schema'`).
  A group of kind `schema` has `bucket_count = 1`, no distribution column type
  (`dist_column_type` becomes nullable; a check ties it to `kind`) and exactly one
  row in `kingres.bucket_map`: bucket 0, the owning group and the state.
- `kingres.dist_table` gets the method `'schema'`: no distribution column, a
  `colocation_id` that is the schema's group. Every table (relkind `r` or `p`,
  including the partitions of a partitioned table) of a distributed schema has
  a row. Views, sequences and indexes have none.
- `kingres.dist_schema` (new) has one row per distributed schema: the schema
  (`regnamespace`, a node-local OID like `dist_table.relid`; the catalog snapshot
  carries the name), its `colocation_id` (unique), `created_at`. It is how the
  planner, the hook and the views know that a schema is distributed, and how a
  table that is created in it later is recognised.
- A move of a schema is a record in `kingres.bucket_move` with the schema's
  `colocation_id` and `bucket = 0`. The columns, the phases, `move_marker`, the
  moving-state rule, the live-move mark, `resume_bucket_moves`, `cancel_bucket_move`
  and `check_cluster` are unchanged.

What this reuses, so that nothing is built twice:

- the placement record and its states, the map version, the map group as the
  source of truth, the cluster lock and the rule "no DDL on a group while it is
  moving" (`kingres_shard_assert_no_move`);
- the record, phases, undo, resume, cancel and cleanup of a move, and the
  progress that the control plane already shows;
- `kingres.plan_rebalance`, its scheduler and `kingres.set_node_property` of
  [shard-phase10.md](shard-phase10.md): a schema group's one bucket is one more item
  in the same plan (section 9.9);
- `bucket_map.modulus`, `colocation.max_modulus`, `colocation.placement_epoch`,
  `bucket_move.kind` and its progress columns, which phase 10 adds (section 5.2);
- the control plane's bucket and colocation-group views (a schema is listed with
  `kind = schema`).

**Why a new object is still needed, and why `move_bucket` cannot be reused
unchanged.** The bucket move of phase 4 works on hash partitions: it creates
standalone tables that are not attached, copies, then in one two-phase
transaction detaches the partitions on the source and attaches the standalone
tables on the target. A table of a distributed schema is an ordinary table, not
a partition, for these reasons:

1. A partitioned table cannot carry what a tenant's schema normally has: a unique
   constraint or primary key that does not contain the partition key, an exclusion
   constraint, a foreign key from another table to a unique column that is not the
   key, some trigger forms. `create_distributed_table` refuses all of these today
   (README: "Every unique constraint or unique index must include the distribution
   column", no triggers, no exclusion constraints). A schema-based design that
   refused them would lose its point.
2. A tenant table has no natural distribution column.
3. Partitioned parents with one partition per table would double the number of
   relations, and every `ALTER TABLE` would have to be mirrored on the partition.

So a table of a distributed schema is an **ordinary table on every node**: the
node that owns the schema holds its rows, every other node holds an empty copy
(the "shell"). The shell makes the parser, the planner, views, functions and
foreign keys work on every node without any change to name resolution: a
statement refers to the table by its schema-qualified name everywhere. Nothing
reads a shell: the planner routes each statement to the owner (section 6), and
the plan checks of section 6.8 turn a plan that would read a shell into an error
instead of an empty answer.

This has two consequences for the move. There is nothing to attach or detach,
so the switch has **no `ACCESS EXCLUSIVE` step and no readers are blocked**;
and the copy on the target goes **into the existing empty shells**, in place.
The orchestration (phases, undo, resume) is the same; the unit-specific steps are
new functions (section 9). `move_bucket(colocation_id, 0, target_group)` on a
colocation group of kind `schema` dispatches to the schema mover, so existing
callers (the rebalancer, the control plane) need no special case.

### 5.2 Catalog changes (0.8)

```sql
ALTER TABLE kingres.colocation
  ADD COLUMN kind text NOT NULL DEFAULT 'hash',
  ADD CONSTRAINT colocation_kind_check CHECK (kind IN ('hash','schema')),
  ALTER COLUMN dist_column_type DROP NOT NULL,
  ADD CONSTRAINT colocation_kind_shape_check CHECK (
    (kind = 'hash'   AND dist_column_type IS NOT NULL) OR
    (kind = 'schema' AND dist_column_type IS NULL AND bucket_count = 1));

-- dist_table: method gains 'schema'; the shape check gains
--   OR (method = 'schema' AND dist_column IS NULL AND colocation_id IS NOT NULL)

CREATE TABLE kingres.dist_schema (
  nsp           regnamespace NOT NULL,   -- node-local OID; the snapshot carries the name
  colocation_id int          NOT NULL UNIQUE REFERENCES kingres.colocation,
  created_at    timestamptz  NOT NULL DEFAULT now(),
  PRIMARY KEY (nsp)
);

-- One row: the database's sharding policy (section 10).
CREATE TABLE kingres.sharding_settings (
  mode                     text NOT NULL DEFAULT 'row'
                           CHECK (mode IN ('row','schema','both')),
  distribute_new_schemas   bool NOT NULL DEFAULT false,
  CHECK (distribute_new_schemas = false OR mode IN ('schema','both'))
);
CREATE UNIQUE INDEX sharding_settings_single_row_idx ON kingres.sharding_settings ((true));
INSERT INTO kingres.sharding_settings DEFAULT VALUES;
```

A database created before phase 11 has mode `row` and no schema group: it is
unchanged. `ALTER EXTENSION kingres_shard UPDATE` creates the same objects as a
fresh install; the `upgrade` regression test compares the two (as for 0.3).

**What phase 10 adds and phase 11 builds on.** [shard-phase10.md](shard-phase10.md)
section 2.2 adds `bucket_map.modulus`, `colocation.max_modulus`,
`colocation.placement_epoch` (a counter that only a switch raises, replacing the
placement-hash comparison of 0.3: review finding S4-01), `node.weight`, and
`bucket_move.kind` (`'move'` or `'split'`) with `parts`, `children`, `rows_done`,
`rows_total` and `attempts`. Phase 11 does not duplicate any of them:

- a schema group's single bucket has `modulus = 1` and `max_modulus = 1` (the shape
  check of `kind = 'schema'` also requires both); it can never be split
  (`split_bucket` refuses a group of kind `schema` with `split_unsupported`);
- every switch of a schema move raises `placement_epoch` of the schema's group, and
  every check of section 6 and section 9.4 uses it;
- a schema move is a `bucket_move` row of `kind = 'move'` with `source_group <>
  target_group`; progress for the console is bytes of the target's tables over
  bytes of the source's tables (the method phase 10 uses for the percent of a
  move), not `rows_done`.

Phase 11 depends on phase 10's catalog (0.7) being installed; it is written
against the committed text of that note. **[Needs verification]** against the code
when phase 10 is implemented.

### 5.3 Catalog changes are deltas, not snapshots

`create_distributed_table` calls `sync_catalog`, which copies the whole catalog
to every node. A distributed schema is meant for many tenants: a catalog of
10,000 schemas with 20 tables each is 200,000 `dist_table` rows, and a snapshot
on every `CREATE TABLE` would make the cost of N tables grow with N squared.
Phase 11 therefore changes the catalog on every node by **replaying one small
node-local function in the same two-phase transaction as the DDL**:

- `kingres.internal_register_schema(nsp name, colocation_id int, group_id int)`
- `kingres.internal_register_schema_table(relid regclass)` (the schema's
  colocation group is found from the table's namespace)
- `kingres.internal_unregister_schema(nsp name)`, `kingres.internal_unregister_schema_table(relid)`
- `kingres.internal_set_schema_group(colocation_id int, group_id int, state text)`
  (used by a move)

Each one changes this node's own catalog rows (resolving names to this node's
OIDs) and raises `map_version` by one. The map group primary picks the
colocation id and the group once and passes them, so every node gets the same
rows. Because every node raises the version by the same amount in the same
transaction, the versions stay equal across nodes, which `check_cluster` compares
for the catalog (README: "the version counter is not compared" today; for
schema deltas the count of rows and the epoch are compared, section 9.10).
`sync_catalog()` remains the repair tool and includes the new tables, so a node
that missed a delta is brought back by it. The catalog cache that every backend
holds is invalidated by the existing `map_version` trigger; its memory and load
time at the sizes above are not known: **[Needs verification]** (measure at
2,000 and 10,000 schemas, section 15).

### 5.4 SQL interface

All functions are in schema `kingres`, are `EXECUTE`-revoked from `PUBLIC`,
carry `SET search_path = pg_catalog, kingres, pg_temp`, and are superuser-only
unless stated, like every administration function of the extension (README,
"Security notes"; the `security` regression test checks it).

| Function | Meaning |
|---|---|
| `kingres.distribute_schema(nsp regnamespace, group_id int default null) returns int` | Map group primary only. Makes the local schema `nsp` a distributed schema and returns its `colocation_id`. See section 8.1. |
| `kingres.undistribute_schema(nsp regnamespace) returns void` | Map group primary only. Makes a distributed schema local again, on the node that owns it. See section 8.2. |
| `kingres.move_schema(nsp regnamespace, target_group int) returns bigint` | Map group primary only. Moves the schema; returns the id of its row in `kingres.bucket_move`. Runs the same procedure as `move_bucket` for a schema group. See section 9. |
| `kingres.move_bucket(colocation_id int, bucket int, target_group int) returns bigint` | Changed: for a group of kind `schema` (only `bucket = 0` is valid) it calls the schema mover; for kind `hash` it is unchanged. |
| `kingres.set_sharding_mode(mode text, distribute_new_schemas bool default null) returns void` | Map group primary only. Sets `mode` (`'row'`, `'schema'`, `'both'`) and, when given, the default for new schemas. See section 10. |
| `kingres.schema_inventory(nsp regnamespace) returns table (object_kind text, object_name text, supported bool, reason text)` | Any role that may read the catalog of the schema; reads only. Lists every object of the schema with whether the schema can be distributed with it, and why not. Used by the control plane's pre-check and by `distribute_schema` itself. |
| `kingres.alter_distributed_schema(nsp regnamespace, ship_functions bool default null) returns void` | Map group primary only; changes a flag of a distributed schema (`dist_schema.ship_functions bool not null default false`, added in the step of section 16 that implements section 6.4; the column is part of the 0.8 catalog from the start). |
| `kingres.internal_schema_structure(nsp regnamespace) returns text[]` | Internal. The statements, in creation order, that recreate the structure of the schema (no data) on another node. Section 8.1. |
| `kingres.internal_register_*`, `internal_unregister_*`, `internal_set_schema_group` | Internal, section 5.3. |

Changed behaviour of existing functions:

- `create_distributed_table`, `create_reference_table`: refused for a table in a
  distributed schema ("a distributed schema holds only schema tables", with a hint
  to use another schema), and refused when the database mode is `schema`
  (`create_reference_table` stays allowed in mode `schema`: a reference table is
  what a tenant's foreign keys point to; see section 10).
- `add_node`: creates, on the new node, every distributed schema and its
  structure, in batches (section 9.12).
- `remove_node`: refuses a group that owns a distributed schema, as it refuses a
  group that owns buckets.
- `plan_rebalance`, `check_cluster`, `cleanup_moved_buckets`,
  `resume_bucket_moves`, `cancel_bucket_move`: extended (sections 9.8 to 9.10).

Views:

- `kingres.schemas` (new): one row per **distributed** schema:
  `schema_name`, `owner` (role), `colocation_id`, `group_id`, `node_id` (the
  primary that holds it, null when the group has none), `state` (`active`,
  `moving`), `table_count`, `size_bytes` (null where the group is not this node
  and the caller is not a superuser, as `plan_rebalance` sizes are; see section 9.9),
  `moving_to_group_id`. It is the analogue of the Citus view that lists distributed
  schemas **[Needs verification]** of the name and columns there.
- `kingres.buckets` (changed): rows for schema tables show `bucket = 0`,
  `partition_name = NULL` and the owner group; `kingres.dist_table` rows of method
  `schema` are visible there. A reader that lists "tables with their buckets" sees
  the schema tables with one bucket.
- `kingres.nodes`: unchanged.

### 5.5 Settings

| GUC | Default | Context | Meaning |
|---|---|---|---|
| `kingres_shard.enable_schema_routing` | on | `USERSET` | Off: a statement on a schema table is never shipped whole; it is planned with the distributed scan (single-source form). Used to compare the two paths, as ADR 0022 requires of every faster path. |
| `kingres_shard.distribute_new_schemas` | `default` | `SUSET` | `default` follows the database setting in `kingres.sharding_settings`; `on` or `off` overrides it for the session of a superuser (for example to create a local helper schema in a database that distributes every new schema). Ordinary roles cannot change it. |
| `kingres_shard.schema_placement_group` | -1 | `SUSET` | Group that receives the next schema this session creates; -1 chooses the group (section 9.1). |
| `kingres_shard.max_distributed_schemas` | 10000 | `SIGHUP` | Upper bound on the number of distributed schemas of a database. The number is a guess, not a measured limit (section 5.3); the creation of another schema fails with a clear message. |
| `kingres_shard.move_schema_cleanup_delay` | follows `move_cleanup_delay` | `SIGHUP` | How long the old copy of a moved schema stays before it is emptied. Same default as the bucket move (10 minutes). |
| `kingres_shard.add_node_batch_objects` | 2000 | `SIGHUP` | Objects created per transaction on a new node (section 9.12). |

The three phase 4 settings (`move_catchup_threshold`, `move_switch_timeout`,
`move_cleanup_delay`) apply to schema moves unchanged. Every GUC is defined with
`MarkGUCPrefixReserved`. The new switches only choose among plans that give the
same answer, or restrict what an administrator may do, and grant nothing.

### 5.6 Extension version and upgrade

Phase 4 is 0.3, phase 5 is 0.4, phase 2b is 0.5, phase 9 is expected to be 0.6 and
phase 10 is 0.7 ([shard-phase10.md](shard-phase10.md), section 2.1). Phase 11 is
**0.8** (`kingres_shard--0.7--0.8.sql`, install script `kingres_shard--0.8.sql`).
If a phase before it does not change the version, the number moves down with it;
it must never be renumbered into another phase's. The script contains the catalog
changes of section 5.2, the new functions, the replaced `move_bucket`,
`plan_rebalance`, `check_cluster`, `resume_bucket_moves`, `cleanup_moved_buckets`,
`kingres.forget_dropped_tables` and the snapshot functions, and the `REVOKE`/`GRANT`
lines. Nodes must run the same version: `add_node` already refuses a node of
another version, and a schema statement carries the map version and the
placement epoch of the plan, so a node of an older version answers with `KS001`,
never with a wrong answer (the argument of 2b section 14). Rolling upgrade: set
`kingres_shard.enable_schema_routing = off` until every node runs 0.8; schemas
cannot be distributed before that anyway (`distribute_schema` checks the version
of every node, which answers review finding S4-05 for this phase).

## 6. Routing

### 6.1 The classification

For every `SELECT`, `INSERT`, `UPDATE`, `DELETE` and `MERGE` the planner hook
walks the whole `Query` (the range tables of subqueries, CTEs and sub-links
included; views are already expanded by the rewriter, so the walk sees the
tables under them) and puts every relation into one class:

| Class | Which relations |
|---|---|
| **S(g)** schema table | a table in `kingres.dist_table` with method `schema`; `g` is its colocation group |
| **H** | a hash-distributed table |
| **R** | a reference table |
| **L** | everything else: an ordinary table, a temporary table, a table of a system catalog or of `information_schema`, a foreign table |

A sequence of a distributed schema named in a call of `nextval`, `currval` or
`setval` with a constant `regclass` argument counts as S(g) (section 6.4). The
decision for the statement:

| The statement has | Action |
|---|---|
| S(g) for one group `g` (or several groups that all have the same owner at plan time, section 6.3), R, and nothing else | **Path S: run the whole statement on the owner** (section 6.2). |
| S(g) and R and L (a local table, a catalog) | `SELECT`: the distributed scan in its single-source form (6.6): the schema table is read from its owner, the rest at the origin. `INSERT ... SELECT` into L: the same. A write to a schema table that also reads L, `UPDATE ... FROM` L, `MERGE` with L: refused with a message that names the local table (as the 2a refusal of `UPDATE ... FROM` a distributed table: PostgreSQL asks for the row identifier of a table it does not modify). |
| S(g1) and S(g2) with different owners | `SELECT`: the scan, one source per schema table. A write that reads the other schema: `INSERT` into S(g1) `SELECT` from S(g2) uses the row router of 2b with one destination group (section 6.7); other writes are refused with a message that names both schemas. |
| S and H | `SELECT`: the scan (each table through its own path). `INSERT INTO S SELECT FROM H`: the router with one destination; `INSERT INTO H SELECT FROM S`: the 2b router with the ordinary rule for H; a write that reads the other kind and cannot be run in one place is refused. |
| S and a write to a reference table | refused (the 2a rule: a statement that writes a reference table and reads a distributed one is not supported yet); run them as two statements. |

Every row of this table that says "the scan" is the standing rule of
[ADR 0022](../adr/0022-the-complete-scan-stays.md) at work: a `SELECT` is never
refused because the planner does not recognise its shape. The refusals are all
for writes, where no correct way to run the statement exists yet, and each
refusal is the same refusal that applies to hash-distributed tables today.

### 6.2 Path S: the whole statement goes to the owner

A statement of class "S(g) plus R only" is a closed query in the sense of 2b
section 6.4, with no condition on its shape. Nothing has to be merged, so
subqueries, CTEs (including data-modifying ones), window functions, aggregates,
set operations, `FOR UPDATE`, `ON CONFLICT`, `RETURNING`, `MERGE`, joins of any
kind and `LATERAL` are all allowed: they run in the owner's own planner and
executor, with its statistics and indexes. This is the path that makes a
tenant's schema behave like a database of its own.

Conditions, all of which must hold, otherwise the statement goes to the scan
(or is refused if it is a write that the scan cannot run):

1. The classification above gives S and R only, and `enable_schema_routing` is on.
2. Every function, operator and cast in the statement that is not built in is
   either absent, or the schema has `ship_functions` set (section 6.4).
3. The statement is not planned in a parallel worker, and a cursor on it is not
   scrollable (a scrollable cursor on a remote statement is refused today; the
   scan supports rescan, so such a `SELECT` uses the scan).
4. It does not write a reference table (the line of the table above).

Mechanism. It is the mechanism of a statement routed by one key: the origin
deparses the analysed query with `pg_get_querydef` under the transmission modes
(`search_path = pg_catalog`, ISO dates, shortest-exact floats, hex `bytea`, and
the others of the README) and sends it in a forwarded session to the primary of
the owning group, as `Kingres Remote Statement`. When the owner is this node's
group the statement is planned by PostgreSQL as it is, after the check of 6.8.
The difference from a routed statement is that the decision does not depend on a
key, so a **generic plan can be routed** (the README limitation "Generic plans
cannot route a statement restricted by a parameter" does not apply: the owner is
known at plan time). How parameters are sent to the owner is what the existing
remote statement already does for parameters that are not routing keys: **[Needs
verification]** (read `remote_scan.c` at the implementation step).

What the owner checks (`plan_forwarded`, extended): every schema table of the
statement belongs to a colocation group whose placement gives bucket 0 to this
group, in the `active` state; every reference table is fine; any other relation
raises `KS001` (a statement that reached a node that does not own the schema is
a stale map). The origin treats `KS001` as before: it reloads the catalog and
repeats once, when no earlier statement of the transaction ran on that node.

### 6.3 Several schemas that happen to be on one group

A statement that touches S(g1) and S(g2) where the two schemas have the same
owner at plan time can be run whole on that owner and is then closed. It is
correct only while both stay there, so the plan carries the placement epoch of
each group and the owner checks both (the same check as for one group). A move of
one of them between planning and execution gives `KS001`; the origin retries once.
This is a performance path only; with `enable_schema_routing = off`, or when the
owners differ, such a statement is the scan. **[Recommendation]**: implement it
in a later increment (section 16, step 7), after path S for one schema is stable,
because it multiplies the cases of the race tests.

### 6.4 Names, `search_path`, functions, sequences

**Names.** Name resolution happens once, in parse analysis on the origin, with
the session's `search_path`. The deparsed text names every relation that is not in
`pg_catalog` with its schema (`generate_relation_name` qualifies a relation
unless `RelationIsVisible`, checked in `ruleutils.c`; with
`search_path = pg_catalog` nothing outside `pg_catalog` is visible). The
remote session runs with `search_path = pg_catalog` and resolves nothing by the
caller's path. Because the schema and its tables exist under the same names on
every node, the text means the same everywhere. Qualified names, unqualified names
resolved through `search_path`, and names resolved through `"$user"` therefore
all work, and the result does not depend on the search path of any other node.
The existing note stays: if the server user has a name that is also a schema
name, unqualified names resolve to that schema through `"$user"`.

**Functions.** A function body is not deparsed: it runs on the node that
executes the call, with the `search_path` of that session unless the function
sets its own (`SET search_path` in its definition). On the owner the session is
the forwarded one, whose path is `pg_catalog`, so a function body that uses
unqualified table names would fail there. This is why path S requires that the
statement has no function that is not built in. A statement that calls one is
planned at the origin: the function runs at the origin, and every statement
inside it is planned and routed like any other (one round trip to the owner per
inner statement, always correct). This is slower and never wrong.

Schemas for which that cost matters (a microservice whose logic is stored
procedures) can set `ship_functions` (`kingres.alter_distributed_schema(nsp,
ship_functions => true)`, section 16 step 8). Statements that call functions
**of that schema** (and built-in ones) are then shipped whole; the owner's forwarded
session runs the statement with `SET LOCAL search_path` set to the path of the
calling session at the origin, checked to contain only schemas that exist on the
owner and not `pg_temp`. This does not widen anyone's rights: the owner session
has the calling role, and the role could set the same path on a direct
connection. A function of another schema (a local one, which exists on one node)
is never shipped. **[Recommendation]**: default off, separate increment.

A function body that touches a table of **another** distributed schema, or a local
table, cannot run in a shipped statement (the forwarded session never forwards
again); with `ship_functions` the owner raises `KS001` with a detail that names
the function, and the statement is refused rather than answered wrongly. This is
the one place where a statement that touches a distributed schema can fail
because of its shape; it exists only for schemas that opted in, and the default
path answers it.

**Sequences.** Each node has its own copy of every sequence of a distributed
schema, and only the owner's copy is used. A `nextval('s1.t_id_seq')` with a
constant `regclass` argument, including the one in a column default, is part of a
statement that is shipped to the owner. A bare `SELECT nextval('s1.seq')` has no
table, and the classification routes it to the owner because the sequence counts
as S(g). A call whose argument is not a constant (`nextval(some_expression)`)
cannot be classified and runs on the node that executes it, which advances that
node's copy: **a documented limit** (section 18). `lastval()` and `currval()` read
session state of the remote session; use `RETURNING`. **[Needs verification]**:
whether the cached connection of a backend keeps its `currval` state between
statements of different transactions.

### 6.5 Transactions

A multi-statement transaction is the existing machinery: the origin keeps one
remote connection per group for the transaction, statements for the owner go
through it, and the commit protocol is the two-phase commit of phase 2a when
more than one node wrote (README, "Guarantees"). A transaction whose writes are
all on one owner and none on the origin has one writer. Whether the extension
then skips the prepare is how it works today; this note relies on that and does
not change it. **[Needs verification]** (read the commit path of `node_conn.c`).
Consequences that follow from the existing design and are stated so nobody
expects more:

- a transaction that touches schema A (owner g1) and schema B (owner g2) is a
  two-group transaction and commits atomically by two-phase commit, with the
  documented absence of a global snapshot;
- the statements of a transaction that stay inside one schema all see one
  snapshot of the owner (they run in one remote transaction); read-your-writes
  holds;
- a savepoint, `ROLLBACK TO`, advisory locks and `SET LOCAL` act on the remote
  transaction as they do for a routed statement today; session-level state
  (`SET`, temporary tables, prepared statements of the origin session, session
  advisory locks, `LISTEN`) does not exist on the owner, which is the existing
  limit of forwarded statements;
- the placement check (the epoch of section 5.2) is made when the first
  statement of the transaction reaches the owner. A move of the schema that
  switches while the transaction is open waits for it: the transaction holds
  `ACCESS SHARE` or `ROW EXCLUSIVE` on the tables, and the switch's lock request
  queues behind it with a bounded wait (section 9.4). So a transaction that has
  already run on the owner finishes there, and the move waits.

### 6.6 The distributed scan for a table of a distributed schema

The scan of phase 2a reads the local partitions of a hash table plus one cursor
per other group that owns buckets. For a table S(g):

- if the owner is this node's group, the table is read by the planner's own
  paths on the local table (no custom scan, no network);
- otherwise the table is read by one `Kingres Distributed Scan` with **no local
  part** and one fragment, `DECLARE ... CURSOR FOR SELECT <columns needed> FROM
  <table> WHERE <shippable restrictions>`, to the owner. The local copy is never
  read. Restrictions are shipped and rechecked as in 2a. The placement check
  (before and after the `DECLARE`, with the epoch) is the check that exists for
  other groups' buckets, with one bucket.

This is the path that answers everything path S does not take (ADR 0022, rule 4:
a new table kind is readable through the scan before any pushdown exists for it).
It is implemented first (section 16, step 3) and path S is the faster path chosen
on top of it. A comparison test runs the same statements with
`enable_schema_routing` on and off and compares the answers, as the rule requires.

### 6.7 Writes that read another schema or kind

`INSERT INTO s1.t SELECT ... FROM <something not in s1>` reads through the scan
(or any pushdown of 2b) and delivers the rows to the owner of `s1` through the row
router of 2b section 11, with one destination group instead of a bucket function.
Until that router exists, this form is refused with "not supported yet" and the
hint to copy through the application. `CREATE TABLE ... AS` and `SELECT INTO` into
a distributed schema are executed as the creation of the empty table (section 7.3)
followed by this `INSERT ... SELECT`. **[Recommendation]**: an increment that
follows 2b (step 9).

### 6.8 The defence against reading a shell

The one new way to get a silently wrong answer is a plan that reads the empty
copy of a table on a node that does not own it. Three defences:

1. The existing end-of-planning check (`check_plan_dist_rtes`) is extended: after
   a statement has been planned in a session that is not forwarded, every
   `RTE_RELATION` that is a schema table must be under a Kingres node or have this
   group as owner. A violation is `ERROR: internal error: a plan reads the local
   copy of "s1.t", which this node does not own` with SQLSTATE `XX000`, never an
   empty answer. This is a test (section 15) that tries the paths that might skip
   the hook.
2. A forwarded session accepts a statement only if the owner check of 6.2 passes.
3. Paths that do not use the planner hook are handled in the utility hook:
   `COPY s1.t TO` (the relation form does not go through the planner; README says
   it was not examined for hash tables) is refused when the owner is not this
   group, with the hint to use `COPY (SELECT * FROM s1.t) TO`; `COPY ... FROM`
   into a schema table is refused unless the owner is this group, until the
   router of 2b section 11.6 gets its single-destination form (step 9);
   `TRUNCATE` is propagated (section 7.4); `VACUUM`, `ANALYZE`, `REINDEX`, `LOCK`
   and `EXPLAIN` stay local to the node they run on, as the README says for hash
   tables (a `VACUUM` of a shell is harmless; `autovacuum` maintains the owner).
   `kingres_shard.enable_routing = off` (superuser) switches the checks off, as
   it does for everything.

**The same defence applies to `COPY t TO` of a hash table, which the README lists
as not examined.** It reads local partitions only, which is a silently partial
answer on a cluster with several groups. This note reports it as a finding for
phase 2b / README, not something that phase 11 fixes for hash tables; for schema
tables the refusal above covers it. **[Needs verification]** that `COPY t TO` of a
hash table bypasses the planner hook (read `DoCopy` and `utility.c` at the
implementation step).

### 6.9 Prepared statements and cached plans

A plan of path S depends on the owner. The plan lists every table of the
statement in its relation OIDs, and the existing `map_version` trigger
invalidates the relcache entry of every distributed table on every catalog
change; for schema tables the trigger does the same, so a cached plan made
before a move is replaced. The plan also stores the map version and the
placement epoch, which the owner checks (a missed invalidation gives `KS001`, not
a wrong answer). **[Needs verification]** the point that the 2b note also leaves
open: that a `CustomScan` with `scanrelid = 0` keeps the original range-table
entries in `relationOids`; the test "prepare, move, execute again" decides it.

## 7. DDL

### 7.1 The rule

On a cluster with more than one group, a utility statement whose object is in a
distributed schema (or that creates, drops, renames or changes the owner of a
distributed schema) is handled by the `ProcessUtility` hook:

- It must be entered on the **map group primary** (the existing rule). On another
  node it is refused with the hint that names the map group primary.
- It is run locally, then propagated to every other active primary in the same
  two-phase transaction, as text as written with the session's `search_path` and
  literal settings (README, "DDL on distributed and reference tables"), under the
  cluster lock on `kingres.map_version`.
- The catalog delta of section 5.3 is applied on every node in the same
  transaction.
- A statement on a schema that is being moved is refused (`kingres_shard_assert_no_move`
  of the schema's group): the in-place copy on the target would not see it.
- After a propagated `CREATE`/`ALTER TABLE` the map group primary asks every node
  for the **structure fingerprint** of the table (an MD5 over column names, types,
  type modifiers, collations, nullability, default expressions, constraint
  definitions and index definitions, sorted) and aborts the transaction if the
  fingerprints differ. This is the guard against divergence through name
  resolution (a different schema of the same name earlier in the path on a node, a
  function that exists on one node only). **[Recommendation]**.

Statement kinds. "Propagated" means to every node. "Refused" means an error with
the reason and a hint, and nothing is left behind.

| Statement | Behaviour |
|---|---|
| `CREATE SCHEMA` | section 7.2 |
| `ALTER SCHEMA ... RENAME TO` | Propagated. The catalog needs no change, because `dist_schema.nsp` is an OID (`regnamespace`) that follows a rename on each node; the snapshot carries names and is rebuilt when it is next made. `kingres.schemas` shows the new name. |
| `ALTER SCHEMA ... OWNER TO` | Propagated. The role must exist on every node (the existing limit). PostgreSQL's own checks (membership of the new owner) run on the first node and fail the whole transaction there. |
| `DROP SCHEMA` | section 7.3 |
| `CREATE TABLE` (also `LIKE`, `PARTITION OF`, `INHERITS`) | Propagated; a row is registered on every node; section 7.4. |
| `CREATE TABLE ... AS`, `SELECT INTO` | Creation of the empty table (propagated), then `INSERT ... SELECT` (section 6.7). |
| `ALTER TABLE` (all subcommands that are allowed on a local table) | Propagated. Subcommands that rewrite the table run on every node, which is cheap on the shells. A `SET SCHEMA` is section 7.9. |
| `CREATE/DROP INDEX`, also `UNIQUE`, partial, expression | Propagated. `CONCURRENTLY` is refused (it cannot run in the two-phase transaction). Every index exists on every node, and is maintained on the owner. |
| `CREATE/ALTER/DROP SEQUENCE`; `ALTER SEQUENCE ... RESTART` | Propagated, except `RESTART` and `setval`, which would give the shells' copies a value that nothing uses: they are run on the owner only (a statement for the owner). |
| `CREATE/ALTER/DROP VIEW` (ordinary and `WITH (security_invoker)`) | Propagated. The view text must be creatable on every node: it may refer to schema tables of the same schema, reference tables and built-in objects. A view that refers to a local table fails on the nodes that lack it, and the transaction rolls back. |
| `CREATE/ALTER/DROP FUNCTION`, `PROCEDURE` | Propagated, to every node, so that statements that call the function can be planned anywhere (6.4). A function that refers to a local object fails the same way. |
| `CREATE/DROP TRIGGER` | Propagated; the trigger fires on the owner, where the rows are written (the shipped statement runs there). The trigger function must exist on every node. |
| `CREATE/ALTER/DROP TYPE`, `DOMAIN` | Propagated for enum, composite and domain types in the schema. A type of another schema is used as it is (it must exist on every node, the existing limit). |
| `COMMENT`, `SECURITY LABEL` | Propagated. |
| `GRANT`, `REVOKE` (on the schema, on its objects, `ALL ... IN SCHEMA`), `ALTER DEFAULT PRIVILEGES IN SCHEMA` | Propagated. `GRANT ... ON ALL TABLES IN SCHEMA s1` is allowed for a distributed schema because every table of the schema is a schema table, which the README refusal ("a `GRANT ... ON ALL TABLES IN SCHEMA` that reaches a distributed table is refused") does not apply to; the refusal stays for a local schema that holds a hash table. |
| `TRUNCATE` | Propagated to every node (the shells are truncated too, which is harmless and also cleans stray rows). Cascading to a table of another schema is refused. |
| `CREATE EXTENSION`, `ALTER EXTENSION` | Out of the hook. An extension that creates a schema is never distributed (`creating_extension`). |
| `CREATE MATERIALIZED VIEW`, `CREATE RULE`, `CREATE POLICY`, `ENABLE ROW LEVEL SECURITY`, `CREATE STATISTICS`, `CREATE FOREIGN TABLE`, `CREATE PUBLICATION`/`SUBSCRIPTION`, `CREATE EVENT TRIGGER` | Refused in a distributed schema (section 7.12). |

### 7.2 `CREATE SCHEMA`

`CREATE SCHEMA [IF NOT EXISTS] name [AUTHORIZATION role] [elements]`.

1. If the database does not distribute new schemas (the effective value of
   `distribute_new_schemas` is false, section 10), the statement is not touched
   and the schema is an ordinary local schema.
2. Otherwise, and only for a role that the hook runs as normal (any role that may
   `CREATE` in the database), the hook requires the map group primary, refuses
   the elements (`CREATE SCHEMA s CREATE TABLE ...` is refused in a database
   that distributes new schemas, with the hint "create the schema, then its
   tables"; Citus refuses the same combination), takes the cluster lock, runs
   the statement locally, and chooses the group (section 9.1) and a new
   colocation id.
3. It propagates the statement text to every other node, then calls
   `internal_register_schema` on every node, all in one two-phase transaction.
   A name that exists on any node (for example a local schema of that name on
   another node) fails the transaction on that node and nothing is created.
4. The schema name is read from the created namespace, not from the text, so
   `CREATE SCHEMA AUTHORIZATION role` (a schema named like the role) is handled.

`IF NOT EXISTS` on an existing schema is a no-op notice and changes nothing.
Schemas whose names start with `pg_`, the schema `kingres`, `public`,
`information_schema` and schemas created by an extension are never distributed
by the default. `public` can be distributed explicitly only if it holds no hash,
reference or local table that has to stay; in practice the answer is no, because
the reference tables of a database are usually there (section 8.1 lists the
preconditions).

### 7.3 `DROP SCHEMA`

`DROP SCHEMA s1 [CASCADE]` on a distributed schema is propagated like any DDL.
`CASCADE` drops the schema's tables on every node, and the `sql_drop` event
trigger removes the schema's rows (`dist_table`, `dist_schema`, `colocation`,
`bucket_map`) on each node, because it runs on each node as the statement
arrives. A drop that cascades outside the schema (a view in another schema that
uses a table of this one) is stopped by the trigger as for hash tables today.
`DROP SCHEMA a, b` that mixes a distributed schema with another is refused.
A drop of a schema that is being moved is refused ("the schema is being
moved"; cancel the move first). A schema that holds a ledger table cannot be
dropped (the ledger's own protection refuses it, section 11).

### 7.4 `CREATE TABLE` in a distributed schema, and what a table may have

The table is created on every node as an ordinary table and registered. The
table of a distributed schema has no restriction that a hash table has, apart
from these:

- **Foreign keys** (section 7.8).
- **Inheritance and partitioning.** A partitioned table in the schema, its
  partitions and `PARTITION OF` are allowed, if every partition is in the same
  distributed schema (a partition in another schema is refused). Each partition
  is registered as a schema table. `INHERITS` of a table of the same schema is
  allowed.
- **Triggers** run on the owner. The trigger function must exist on every node,
  and **when the schema is moved, triggers do not fire for the copied rows**:
  logical replication applies changes with `session_replication_role = replica`
  (`logical-replication.sgml`, "triggers and rules will not fire on a
  subscriber"). That is the wanted behaviour for a copy.
- **Identity and serial columns** use sequences that live in the schema (7.1).
- **Temporary and unlogged tables.** A temporary table is never in a distributed
  schema. An unlogged table is allowed, but is not replicated to standbys and
  is not copied by logical replication: a schema that has one cannot be moved
  (the pre-check names it).
- **Storage parameters, `TABLESPACE`, `WITH`**: allowed; tablespaces must exist on
  every node (the existing limit for roles and types).

### 7.5 Sequences and defaults across a move

Sequence data is not replicated by logical replication
(`logical-replication.sgml`, "Sequence data is not replicated"). The switch of a
move therefore copies the state of every sequence of the schema from the source to
the target inside the switch (section 9.4, step 7), so that the first
`nextval` on the new owner is greater than anything the old one handed out.

### 7.6 Views, functions and types

They live on every node (7.1) and are used where the statement is executed: a
view is expanded by the rewriter on the origin, so a view in any schema over
tables of a distributed schema works from every node; a view of a distributed
schema over a local table is refused at creation. Types defined in a distributed
schema are created on every node. **[Needs verification]** that a column of a
user-defined enum type of a distributed schema is carried correctly by the scan
fragment (the type's OID differs between nodes; the scan transmits text).

### 7.7 Indexes, constraints, and the `ALTER TABLE` family

All propagated. An index that PostgreSQL builds on the owner builds on the
shells too; they are empty, so the cost is negligible. An `ALTER TABLE ... ADD
COLUMN` with a volatile default is allowed (it is evaluated on every node but
only the owner has rows; the shells have none). The row values on the owner are
then those of the owner's evaluation, which is what a single-node database does.

### 7.8 Foreign keys

| From | To | Allowed |
|---|---|---|
| a table of schema S | a table of the same schema S | yes |
| a table of S | a reference table | yes (as for a hash table today: the reference table exists on the owner) |
| a table of S | a table of another distributed schema, a hash table, a local table | no |
| a hash, reference or local table | a table of S | no (README: "A foreign key from an ordinary table to a distributed table is refused"; Citus refuses the cross-schema case too) |

A reference table's write runs on every node; the check on the owner, where the
rows are, fails the whole write if it would break a key, and the two-phase
transaction rolls back everywhere. The foreign key from a schema table to a
reference table is part of the structure that `add_node` and a move copy.

### 7.9 `ALTER TABLE ... SET SCHEMA`

`AlterTableNamespace` (`tablecmds.c`) moves a table, its indexes, owned sequences
and constraints to another schema. A move across a distributed-schema boundary
changes where the rows are. Phase 11 handles the cases where no row has to move
and refuses the others, with the function that does move rows named in the hint:

| From | To | Behaviour |
|---|---|---|
| local schema | distributed schema S | The table is created on every other node (structure), registered, and its rows must be on the owner of S. If the table is empty, or the node that holds it is in the owner's group, nothing moves. Otherwise the statement is refused with the hint to copy through the router (`INSERT ... SELECT`) or to use phase 9's primitive. **[Recommendation]**: refuse in the first increment (step 10), allow after phase 9 provides the primitive (section 8.3). |
| distributed schema S | local schema | The table stays on the owner (its data does not move). The other nodes drop their shells; the table becomes a local table on the owner's primary. Refused if the table has foreign keys to or from other tables of S that would be left behind. |
| distributed S1 | distributed S2 | Allowed if S1 and S2 have the same owner (nothing moves, the table gets the group of S2 in the catalog) and no foreign key crosses the boundary. Otherwise refused. |
| a sequence, view, function or type | across the boundary | Refused ("recreate it"). An owned sequence cannot be moved alone anyway (PostgreSQL). |

### 7.10 Two-phase commit, and what a failure leaves

The propagated DDL uses the commit protocol of phase 2a unchanged: the map group
primary is the origin, the other nodes are participants, the decision is written
to `dtx_log` by the committing transaction, and the recovery worker commits or
rolls back what a crash left. For a statement that creates a table the unit is
the whole of: the text on every node, the registration delta on every node, the
fingerprint check. A node that is down fails the DDL (the transaction cannot
prepare), as for hash tables today.

### 7.11 Cost

Every DDL statement on a distributed schema is a two-phase transaction on every
node. A `CREATE TABLE` costs about the round trips of one `PREPARE` and one
`COMMIT PREPARED` to each node, plus the fingerprint question. A tenant that
creates 50 tables on creation therefore waits for 50 such transactions. The
number is not measured: **[Needs verification]** (measure at the step that
implements it; if it is too slow, a `CREATE SCHEMA` with its tables can be
batched by a function that takes a script, a later increment).

### 7.12 What is refused in a distributed schema in this phase

Materialized views (their content would live on the owner only and `REFRESH` is a
utility statement that would need routing; a later increment can add it), rules,
row-level security and policies, extended statistics objects, foreign tables,
publications, subscriptions, event triggers, `CREATE TABLE ... TABLESPACE` of a
tablespace that does not exist on every node, and `CREATE INDEX CONCURRENTLY`.
Each is refused with `<statement> is not supported in a distributed schema yet`
and `kingres.schema_inventory` lists the same reason for an existing object.

## 8. Converting

### 8.1 `distribute_schema`

`SELECT kingres.distribute_schema('tenant_7');` or with a group:
`kingres.distribute_schema('tenant_7', 2)`.

The central idea: **an existing schema is distributed in place**. The schema, with
its data, is on one node today. That node becomes the owner (its group is the
owner group) and nothing moves; every other node receives the structure, as empty
shells. This is a metadata operation, short, whatever the data size. Moving the
schema afterwards to another group is the ordinary move (section 9). This differs
from distributing a table with rows (phase 9), which has to move rows to buckets;
the cost difference is the reason schema-based sharding is the cheaper way to
shard an existing application.

Preconditions (the function checks all of them first, with the first failure
named; `schema_inventory` returns the same list for the control plane):

1. The function runs on the map group primary, as a superuser, in a
   `READ COMMITTED` session, and every active node runs this version of the
   extension.
2. The schema exists on the map group primary and the database mode is `schema`
   or `both`. The data's node is the map group's: a schema that exists on another
   node only is not supported here (**[Recommendation]**: run it on the map group
   primary; a later increment can take the node as a parameter).
3. The schema holds only supported objects: ordinary and partitioned tables with
   their partitions, indexes, constraints, sequences, views, functions and
   procedures, triggers, enum, composite and domain types, comments and grants.
   Anything else (section 7.12) makes it `supported = false` with the object and
   the reason. The schema holds no hash table, no reference table and no
   `kingres_ledger` table (section 11), is not a system schema, and is not already
   distributed.
4. No foreign key from or to a table of another schema except a reference table,
   and no inheritance or partition relationship across the schema's border.
5. A view or function of the schema that depends on an object outside the schema
   that does not exist on every node (a local table) is `supported = false`.
6. The schema has no temporary or unlogged table, and no table that has the
   `kingres_ledger` capture trigger.
7. `group_id` is null (then it is the group of the map group primary, the data's
   home) or the schema holds no row in any table (then any group that should
   have shards is allowed and the local tables become shells). If the schema has
   data and `group_id` names another group, the function refuses with the hint to
   distribute in place and then move the schema.

Algorithm.

1. Check the preconditions. Take the cluster lock on `kingres.map_version`
   (exclusive, until the end of the transaction), as every catalog-changing
   function does.
2. Take `ACCESS EXCLUSIVE` on the tables of the schema, with `lock_timeout` of
   `move_switch_timeout / 10` (at least 200 ms), polling `LOCK ... NOWAIT` so that a
   queued request does not stall other sessions (review finding S4-04); at most
   a bounded number of attempts, then fail with the pid of a blocker. The lock
   keeps the structure from changing while the script is produced; it does not
   copy or scan data.
3. Produce the structure with `internal_schema_structure(nsp)`: an ordered list of
   statements built from `pg_get_*def` functions and catalog reads
   (`pg_get_viewdef`, `pg_get_functiondef`, `pg_get_indexdef`,
   `pg_get_constraintdef`, `pg_get_triggerdef`, `pg_get_expr`, the existing
   `internal_table_ddl` for columns, defaults, constraints and owned sequences
   of a table) in dependency order (types, sequences, tables, indexes and
   constraints, functions, views, triggers, comments, grants, owner). The kinds
   it cannot produce are the unsupported kinds of precondition 3; this is the
   reason for that list. **[Recommendation]**: do not use `pg_dump` in the
   extension; the control plane may use it later for a wider coverage (section 17,
   question 9).
4. Create the colocation group (kind `schema`, one bucket on the owner group),
   `dist_schema` and `dist_table` rows locally.
5. In one two-phase transaction, on every other node: create the schema (with its
   owner) and run the structure statements, then register (`internal_register_schema`,
   `internal_register_schema_table` for each table). Each node runs the script in a
   forwarded, authorized-DDL session with `kingres_shard.enable_routing`-style
   protection (the statements are not propagated again). Fingerprint check of every
   table on every node.
6. Commit. The owner node's tables are untouched; nothing was copied.

The transaction holds the exclusive locks on the schema's tables for the duration
of steps 3 to 6, which is bounded by the time to create the structure on the
other nodes, not by the data size. It blocks readers and writers of those tables
for that time. **[Needs verification]** (measure the lock time for a schema of 50
tables on three nodes; if it is too long, the structure can be created on the
other nodes **before** the lock, in a first transaction that is invisible
because the schema is not yet registered, and only the registration and a
fingerprint re-check run under the lock: this is the intended form, the above
is its worst case).

Failure. A crash before the prepare: nothing happened, the function can be run
again. A crash after the prepares: the recovery worker decides by `dtx_log` (a
commit decision is only written by the committed origin). A node that is down:
the function fails before the prepare. The statement is idempotent in the
sense that a schema that already exists on a node (left by an earlier crash
before the first commit) is removed by the recovery of the prepared transaction;
a half-created schema is never visible because creation is in the prepared
transaction.

### 8.2 `undistribute_schema`

`SELECT kingres.undistribute_schema('tenant_7');`

The reverse, again in place: the owner keeps the data. The schema becomes an
ordinary schema that exists on the owner group's primary only; every other node
drops its shells.

Preconditions: the schema is distributed and not moving; its tables on every
node other than the owner are empty (the function counts: a stray row in a shell
is reported with the table and node and the function stops; `check_cluster`
reports the same); no foreign key from a reference table is affected (there is
none); the schema holds no `kingres_ledger` table (section 11); no other
distributed schema depends on it (none can: no cross-schema foreign key).

Algorithm: lock the tables on every node; in one two-phase transaction on every
node other than the owner drop the schema (`DROP SCHEMA ... CASCADE`, run as an
authorized internal statement so that it is not propagated and passes the
drop guard), and on every node including the owner unregister the schema, its
tables and its colocation group. The owner keeps the schema and its objects
untouched. The data never moves.

**The schema is then local to the owner's primary.** A client that reaches it
through another node no longer sees it. This is a consequence of what a local table
is in the current design (a table lives on the node where it is created) and the
web console says so before the user confirms (section 14). A user who wants it
local on the map group, or on another node, moves the schema first
(`move_schema`) and then undistributes, which the control plane offers as one
task with two steps ("Move to group G, then make local").

**What I need from phase 9.** Phase 9 designs distributing tables that have rows
and undistributing a table. Phase 11's operations differ in cost (the schema
operations move no rows) but must agree with phase 9 on: (a) where an
undistributed table ends up (this note assumes: on the node where the user runs
the function for a table, and on the owner for a schema), (b) the name of the
primitive that copies the rows of a local table to a given group (used by
section 7.9, local to distributed `SET SCHEMA` with rows, and by `INSERT ... SELECT`
into a schema), and (c) the lock and failure protocol of its row copy. If phase 9
chooses another home for an undistributed table, section 8.2 changes its default
and nothing else does. **[Needs verification]** against `shard-phase9.md`.

### 8.3 Single tables in and out

A single table may enter a distributed schema through `ALTER TABLE ... SET SCHEMA`
(7.9) and leave it the same way. A table with rows moves only when phase 9's
primitive exists. Until then, only empty tables, or tables on the owner group's
node, move.

## 9. Placement and moving

### 9.1 Where a new schema goes

A new distributed schema, by default or by `distribute_schema` of an empty
schema without a group, is placed on the shard group that **has the fewest
distributed schemas per unit of weight** among the groups whose primary has `should_have_shards`
and is active (a group that is draining is never chosen). A tie goes to the
group with the lowest load under the strategy of phase 10 (cost 1 per bucket, or
bytes), then to the lowest group id. The count is divided by the group's `weight`
(`node.weight`, phase 10), so a group of twice the capacity receives twice the
schemas.
The choice is made on the map group primary under the cluster lock, so two
concurrent `CREATE SCHEMA` statements see each other's schemas (the count includes
a schema created by the same transaction). `kingres_shard.schema_placement_group`
(superuser) overrides it for a session; `kingres.distribute_schema(nsp, group_id)`
names the group for the explicit form. **[Recommendation]** that count is the
rule, not size: when a schema is created it has no size, and the rebalancer
(section 9.9) corrects the distribution by size afterwards.

### 9.2 What a move is

`move_schema(nsp, target_group)` is `move_bucket(colocation_id, 0, target_group)`
for the schema's colocation group. The record, the phases and their names, the
preconditions that are about nodes and settings, the undo and the resume are
those of phase 4, **plus** the fixes that review S4 asks for, which the schema
mover is built to include from the start (section 9.11).

Requirements on the cluster are those of phase 4 (`wal_level = logical` on every
node, replication slots and workers, `pg_hba.conf`, a superuser role, the
`node_conninfo` of the target) and these about the schema:

- every table of the schema has a primary key or `REPLICA IDENTITY FULL`
  (`USING INDEX` and identity columns that phase 4 refuses are refused here too);
  a table without either is named, and nothing is started. The control plane
  offers "set `REPLICA IDENTITY FULL` on these tables" as a separate, propagated
  `ALTER TABLE` before the move, because it is a change of the user's table.
- the schema has no unlogged table, no table that holds a `kingres_ledger`
  capture trigger, and no foreign key cycle among its tables that phase 4 does not
  support (the schema's foreign keys are inside it, so a cycle is possible; a
  cycle of two tables is supported by the initial copy only if the target applies
  with `session_replication_role = replica`, which logical replication does for
  the apply, and foreign keys are not checked there: **[Needs verification]**;
  phase 4 refuses a cycle, and this note keeps that refusal until the test shows
  otherwise);
- the target's shells exist, are empty, and have no row (`SELECT EXISTS` on each
  table with routing off in the target's control session).

### 9.3 Algorithm

Phases are recorded in `kingres.bucket_move.phase` before the action they
announce, as in phase 4.

1. **preparing.** Checks (above). In a session of its own (committed at once):
   the cluster lock is taken briefly, the move is recorded, the schema's bucket is
   set to `moving` in `bucket_map` on every node (a delta,
   `internal_set_schema_group(..., 'moving')`). Routing is unchanged: statements
   still go to the source. DDL on the schema is refused from now until the move
   ends.
2. **copying.** On the source: a publication `ks_move_<id>` of the schema's tables
   (an explicit `FOR TABLE` list of the leaf tables and ordinary tables, plus
   `kingres.move_marker` with the row filter on the move id; **not**
   `FOR TABLES IN SCHEMA`, which would include a table created later and does not
   combine with a row filter in the same way) and a logical slot. On the target: a
   subscription with `copy_data = true` and `synchronous_commit = on` into the
   **existing empty tables**. The subscription's tables are copied by the table
   sync workers; the shells are not attached to anything, so no scan can read a
   half-filled copy (section 5.1, "nothing reads a shell").
3. **catching_up.** Until every table of the subscription is `r` in
   `pg_subscription_rel` and the lag is within `move_catchup_threshold` or the
   marker has made the round trip (the exact proof of phase 4).
4. **switching.** The only step that blocks anything, and only writers of this
   schema. See 9.4.
5. **cleaning.** The subscription, the slot and the publication are dropped at
   once; after `move_schema_cleanup_delay` the old copy on the source is emptied
   (`TRUNCATE` of every table of the schema in one statement, with a bounded lock
   wait and a retry by the recovery worker; an authorized internal statement that
   is not propagated). The move is `done` when the source shells are empty.
6. **done.**

### 9.4 What the switch does, locks, and how long

1. On the **source**, the tables of the schema (the tables themselves; there is no
   parent) are locked in `SHARE` mode: **writers of the schema's tables on that
   node wait; readers go on**. The lock is taken by `LOCK ... NOWAIT` polled in a
   short loop with a total bound of `move_switch_timeout / 10`, not by a queued
   request: a queued `SHARE` request makes every later writer wait (review S4-04).
   On failure the attempt gives up, the move returns to `catching_up`, and the
   next attempt starts after a random pause. The number of attempts is **bounded**
   (`move_switch_attempts`, default 20; then the move is `failed` with the pid of
   the blocking transaction in `detail`), which is the fix that S4-04 asks for.
2. The marker row is committed on the source and waited for on the target (all
   writers before the lock have committed before the lock was granted, so the
   marker arrives after their changes).
3. Every table is locked, not only through its parent: a direct write to a
   partition of a partitioned table is a write to a table of the schema, taken by
   the same `SHARE` lock on every relation of the move's table list, partitions
   included (review S4-03: the phase 4 lock covered the parents only).
4. After the marker, **a second marker is committed after the locks and waited
   for**, and the subscription is not dropped until a marker committed after the
   last lock release has arrived (S4-03's second request), so no change made by a
   transaction that held a conflicting lock is lost to a lagging apply.
5. The state of every sequence of the schema is read on the source (the
   `last_value` and `is_called` in the sequence relation, which includes values
   that a session has cached: **[Needs verification]**) and set on the target with
   `setval`, in the switch's transaction on the target.
6. The two-phase transaction: on the map group primary, the source and the target
   (and so on every node, by the version rule of phase 4) set the schema's bucket
   to the target group and `active`, raise the placement epoch and the map version.
   Nothing is attached or detached; there is no `ACCESS EXCLUSIVE` and readers are
   never blocked.
7. The locks are released at the end of the two-phase transaction on the source
   (`COMMIT PREPARED`). A writer that had been waiting for the lock gets it only now; it
   has locked the table in the analyser before the planner hook looks at the
   catalog, and `LockRelationOid` has processed the invalidations that the commit
   sent, so the hook reads the new owner and raises `KS001` (phase 4's argument for
   routed statements, which holds here for the same reason: the lock is taken before
   the check). **[Needs verification]** that the check order holds for statements
   that are planned from a cached plan; the test of section 15 forces it.

How long. The writers of the schema wait for one attempt (normally tens of
milliseconds: the marker round trip and the prepare), at most `move_switch_timeout`
(10 s by default) per attempt. Writers of **other** schemas on the same node are
not affected, which is the improvement over a bucket move (where the lock is on
the parents of tables that other buckets share). A long transaction that wrote
the schema holds the attempts back; the bound on attempts makes that a visible
failure rather than a permanent stall.

### 9.5 Crash and lost-node behaviour

| Step | Crash or loss | Result |
|---|---|---|
| preparing | session or map group primary dies | `resume_bucket_moves` undoes: the bucket goes back to `active`, the move is `failed`. |
| copying | session dies | resume: drop subscription, slot, publication; **truncate the target's shells** (they were empty by the precondition and nothing else writes them); bucket `active`; `failed`. |
| copying | target node down | the subscription cannot make progress; the move stays `copying` with `detail` naming the target; after `2 x node_timeout` of no progress resume undoes what it can reach and records what it could not (the shells and subscription on the target stay until the node returns; `check_cluster` reports `stray_schema_rows` and `subscription`). |
| copying | source node down | the schema is unavailable anyway; the move waits; after a failover (phase 5) the slot is gone (a slot is not on the standby by default **[Needs verification]**), so resume undoes the move. |
| catching_up | any of the above | the same as copying. |
| switching, before the prepare | session dies | undone as copying. The locks of an unprepared transaction vanish with it. |
| switching, after the prepares | any node dies | decided by the two-phase commit, as phase 4: the phase becomes `cleaning` in the same transaction as the catalog change, so a `switching` row after a crash is a switch that did not commit. `resume` waits `2 x node_timeout` before undoing a `switching` move. |
| cleaning | source down | the truncate waits; the move stays `cleaning`; `check_cluster` reports the stale rows after the delay. |
| cleaning | a long reader holds the source tables | the truncate's lock wait is bounded; the recovery worker retries. |

A failover of the **target** group during `copying` or `catching_up` aborts the
move (the subscription's apply worker and its state are not on the new primary in
a form this note relies on: **[Needs verification]**); the schema stays on the
source. A failover during `cleaning` does not matter: the new primary has the
data (physical replication carried the logical apply's changes).

### 9.6 Interaction with bucket moves, the map version, and `KS001`

- Moves of different colocation groups (a schema and a hash bucket, or two
  schemas) run at the same time in different sessions, within the limits of
  replication slots and workers; a second move of the same schema fails.
- The map version rises on every catalog delta. A scan or a fan-out write
  compares the placement epoch, not the version (phase 4: the version counts
  every catalog change and a scan would fail on each).
- `KS001` is retryable and is the answer to a statement that reaches a node that
  no longer owns the schema, or that finds the placement changed. The origin
  reloads the map and repeats once, when no earlier statement of the transaction
  ran on that node. A statement that cannot be repeated (an earlier statement ran
  there) fails to the client with the retry hint, as for hash buckets.
- A move of schema A does not block DDL on schema B: the lock "no DDL on a group
  while it moves" is per colocation group.

### 9.7 Interaction with reference tables

Reference tables do not move and are on every node. A foreign key from a schema
table to a reference table is checked on the owner, where the rows are. The
reference-table writer lock (the advisory locks of the README) is separate from
the switch's locks. A reference-table write that is in flight while the switch
commits is unaffected (it writes every node).

### 9.8 The recovery worker

The recovery worker, after it has resolved prepared transactions, calls
`resume_bucket_moves()` and `cleanup_moved_buckets()` (review S4-02 found that
at the last commit it calls neither: `src/recovery.c` has no call, and README
and design say it does). Phase 11 depends on this fix: a move session that dies
leaves a slot that holds WAL and a schema that is refused DDL. If the fix has not
been merged when phase 11 starts, step 1 of section 16 is that fix, and
`check_cluster` gains the row of S4-02 for "a move in progress whose live mark is
free and whose `updated_at` is older than a few `node_timeout`".
`cleanup_moved_buckets()` also handles the stale rows of a schema's old copy
(`force = true` empties them at once, for erasure).

### 9.9 The rebalancer and the drain (phase 10)

**A distributed schema is one more item in phase 10's plan and scheduler; nothing
separate is needed.** [shard-phase10.md](shard-phase10.md) defines the unit of the
plan as the pair `(colocation_id, bucket)`, the load of a group as the sum of the
costs of the buckets it owns "over all colocation groups", and
`plan_rebalance(strategy, threshold, max_moves, drain_only, extra_load,
exclude_groups, colocation_ids)` with columns `(seq, colocation_id, bucket,
source_group, target_group, cost, size_bytes, reason)`. A schema is the item
`(colocation_id of the schema, 0)`. It therefore takes part in the greedy
simulation, in drains (`drain_only`), in `exclude_groups`, in `colocation_ids`
filters, in `extra_load`, in per-group `weight`, and in the scheduler's loop that
recomputes the plan before each move, with no change to the plan's signature,
columns or loop. The scheduler calls `move_bucket(colocation_id, 0, target)` for
it, which dispatches to the schema mover (section 5.4). Phase 4's per-colocation
rule (a group of one bucket never moves) does not apply to `plan_rebalance`,
because its balance is over group loads, not per colocation group.

Cost of a schema:

- `by_bucket_count`: cost 1, as for every bucket ("every bucket counts the same").
  This is deliberately crude: a tenant is one item, however large. A database whose
  schemas differ much in size, or that mixes row buckets and schemas, should use
  `by_disk_size`. **[Recommendation]**: no separate weight for a schema under this
  strategy; a setting that gives a schema another count would make the counts of
  phase 10's worked examples depend on a number nobody can derive.
- `by_disk_size`: `max(size, min_bucket_cost)`, where `size` is the sum of
  `pg_total_relation_size` of the schema's tables on the owner (tables, indexes,
  TOAST), read from the owner's primary as for a bucket. A schema larger than the
  ideal load of a group is placed alone on the least utilized receiver; the plan
  never splits it, and the rule of the plan that a bucket qualifies only if it
  reduces the difference of utilization keeps it from bouncing between two groups.

What the scheduler must additionally know (the concurrency rules of its section
6.3 are stated for the parents of a colocation group's tables):

- "No two moves of one colocation group share a source or target group" holds
  trivially for a schema (one bucket). A schema move locks only the schema's own
  tables (9.4), so two schema moves, or a schema move and a bucket move, that share
  a group do not lock the same objects. The per-server limits (`maxMovesPerServer`,
  `maxConcurrentMoves`) apply to schema moves as to bucket moves; they matter more
  here because each move holds a replication slot and one table sync worker per
  table (bounded by `max_sync_workers_per_subscription`). **[Recommendation]**:
  count a schema move as one move for these limits and let that setting bound the
  copy.
- The safety and disk rules of phase 10 (its sections 8.3 and 8.4) use the schema's
  size as the size of the move.
- A schema that cannot move (a ledger pin, an unlogged table, a table without a
  replica identity, an unsupported object) fails its preconditions; the scheduler's
  rule that a plan entry that no longer reflects the state is dropped, and its
  per-item failure count, apply, and the report shows the reason. A **pinned schema
  is not an item** of the plan: `plan_rebalance` leaves it out of the units and
  counts its size as fixed load of its group (it cannot be balanced away), as it
  treats a bucket that is `moving`. The plan is thereby not stuck on it.
- `plan_rebalance` and `group_load` need no new column. `group_load.buckets` counts
  schema groups too; the number of schemas per group is read from `kingres.schemas`.
- Splits: a schema group has one bucket that cannot be split; the automatic mode
  never plans a split of it and the console does not offer one.

**Drain.** `server.drain` (phase 10, section 11.3) also moves the distributed
schemas placed on the server's group. It needs no new step, because its step 3,
"Move the buckets away", is a rebalance run with `drain_only`, and the plan of such
a run holds every item of every colocation group that the draining group owns,
schemas included, with `reason = 'drain'`. The additions to the other steps:

1. Step 1 (check): `previewServerDrain` lists the distributed schemas on the group
   with their sizes, and the check refuses with `409 schema_pinned` when a pinned
   schema (ledger table) is on it, naming the schema, because the drain could not
   complete; the operator must decide about that data first. The disk rule for the
   receivers counts the schemas' sizes in the drain size. The other move
   preconditions of 9.2 (unlogged tables, a table without a replica identity,
   unsupported objects) are reported by the preview as blockers per schema, so the
   drain does not start and then stall.
2. Step 2 (stop new buckets landing): `set_node_property(node, false)` also stops
   new schemas from being placed on the group, because the placement rule of 9.1
   considers only receivers.
3. Step 3: schema moves run under the same scheduler, in parallel with bucket moves
   within its concurrency rules. For tables without a replica identity the preview
   offers the same option as `moveSchema` (the control plane sets `REPLICA IDENTITY
   FULL` first).
4. Step 6 (read `holds()` again): a group that still owns a bucket **or a
   distributed schema** sends the task back to step 3 once (a schema whose move
   failed, or one created by a session with a stale map), then fails with
   `server_owns_buckets` naming the databases and the schemas. `kingres.remove_node`
   refuses a group that owns a schema (section 5.4), which is the guard under that
   check.
5. Local (undistributed) schemas on the server are local data in the sense of phase
   10's section 11.5 (tables outside `kingres.dist_table`): the preview lists them
   and `acknowledgeLocalData` is required, because deleting the instance deletes
   them. A distributed schema is not local data after its move; the drain waits for
   the `cleaning` phase of the last move before it removes the group, and the old
   copy on the drained group is emptied by the cleanup of the move.

**[Needs verification]** against the code of phase 10: that `plan_rebalance` can read
the cost and size of a schema group (its size query is described per bucket
partition `<table>_b<NNNN>`, which a schema table does not have), and that
`group_load` and the report tables accept a bucket that has no partitions.

### 9.10 `check_cluster` additions

On the map group primary, per active primary node: `schema_missing` (a
distributed schema that the node lacks), `schema_table_missing`,
`schema_structure_differs` (fingerprint differs from the map group's),
`stray_schema_rows` (a non-owner table holds rows, outside a move's cleaning
window), `schema_catalog_differs` (rows of `dist_schema`/`dist_table` of method
`schema` differ from the map group's, by count and epoch), and for moves the
existing `replication_slot`, `publication`, `subscription`, `bucket_moving_without_move`,
`move_without_moving_bucket`, which apply to a schema group because its bucket
is in `bucket_map`. `check_cluster` stays a structural check; it does not compare
row counts of a copy with its source (review S4-10) and says so.

### 9.11 What the schema mover does differently from the bucket mover, because of the review

| Finding of review 0.3 | Cause | What phase 11 does |
|---|---|---|
| S4-01 (critical): the placement hash cannot see a bucket that moves away and back | the number is a function of the placement alone | uses the monotonic placement epoch (section 5.2) in every check; test with the sequence of the review |
| S4-02 (high): the recovery worker never resumes or cleans | the worker's main loop does not call them | section 9.8; a test kills a move session and waits for the worker |
| S4-03 (high): a write straight into a partition is not covered by the marker | the lock is on the parent only; no second marker | lock every relation of the move, partitions included, and a second marker after the locks (9.4 steps 3 and 4) |
| S4-04 (medium): the switch retries without limit and queued locks stall readers and writers | queued lock requests, no attempt limit | `NOWAIT` polling, bounded attempts, the blocking pid in `detail` (9.4 step 1) |
| S4-05 (medium): no version handshake | scans call functions that an older node lacks | `distribute_schema` and `move_schema` refuse unless every active node reports this version; the error names the node |
| S4-06 (low): helper functions open to every role | `internal_placement_hash` is callable by all | the new helpers are revoked from `PUBLIC`; the placement function ignores an OID that is not a distributed table of a group |
| S4-07 (low): `bucket_move` readable by everyone | grant to `PUBLIC` | the control plane reads moves through a view that shows `phase` and a cleaned `detail`; grant only to the monitoring role |
| S4-08: a test that races | started a long transaction before the phase it assumed | the test waits for the target phase by `detail`, not by name alone |
| S4-09 (low): the move pins the xmin horizon | one long PL/pgSQL call | the schema move returns after recording the move; the waits run in the control sessions and the caller polls (the control plane already polls) |
| Orphan table names (fix round item 1) | a table moved twice before cleanup | no orphan tables here: the old copy stays in the table itself; a second move back to a group whose old copy is not yet cleaned is refused with the hint to run `cleanup_moved_buckets(true)` |

### 9.12 `add_node` and new nodes

`add_node` creates on the new node every distributed schema and its structure
(section 8.1 step 3's statements), registers them, and gives the node no schema
(placement is by group; a new group has no schemas until the rebalancer or a
move gives it some). The work is done in **batches** of
`kingres_shard.add_node_batch_objects` objects, each in its own transaction on the
new node, which is not yet active and therefore not visible; the final activation is
the existing two-phase transaction. A batch that was done is skipped when the call
is repeated. This avoids one transaction that creates 100,000 relations and would
exceed `max_locks_per_transaction`. While `add_node` runs it holds the cluster
lock and so DDL on schemas waits: with thousands of schemas this can be long
(**[Needs verification]**: measure). An existing standby is not affected: it
receives all of this through physical replication from its primary (ha-phase5).

## 10. The database-level setting

`kingres.sharding_settings` holds `mode` and `distribute_new_schemas`.

| Mode | `create_distributed_table` | `create_reference_table` | `distribute_schema` | New schemas |
|---|---|---|---|---|
| `row` (default, and every existing database) | yes | yes | refused | local; `distribute_new_schemas` must be false |
| `schema` | refused | yes | yes | distributed if `distribute_new_schemas`, else local |
| `both` | yes | yes | yes | distributed if `distribute_new_schemas`, else local |

Reference tables are allowed in `schema` mode because every tenant needs shared
lookup tables, and a foreign key from a tenant table to a reference table is the
way to express it. A reference table is in a local schema, not in a distributed
one (7.1, 8.1 precondition 3).

`kingres.set_sharding_mode(mode, distribute_new_schemas)` changes it, atomically
on every node, and returns an error naming what is in the way:

- `row` to `both`, `schema` to `both`: always allowed.
- `both` to `row`: refused while any distributed schema exists.
- `both` to `schema`: refused while any hash table exists.
- `row` to `schema` and `schema` to `row`: refused while the other kind exists.
- `distribute_new_schemas = true` requires mode `schema` or `both`.

The setting is **per database** (the extension is installed per database), and
for a database whose sharding is turned on later from the control plane it
starts as `row`. **[Recommendation]** defaults: the create-database dialog offers
`row` as before (nothing changes for anyone who does not choose), and when the
user picks `schema` the box "Distribute new schemas automatically" is checked;
for `both` it is unchecked. This keeps a database that mixes kinds from
distributing a schema that someone meant to be local.

The effective value of the default for a session is `kingres_shard.distribute_new_schemas`
(section 5.5): `default` means the catalog value; a superuser may override it for
a session. A role other than a superuser cannot opt a schema out, because a
schema that escapes distribution would exist on one node only.

## 11. Ledger tables, standbys and failover

### 11.1 Ledger tables in a distributed schema

State of `kingres_ledger` (ledger-mvp.md): the ledger's entries, blocks and
registered tables are rows in tables of schema `kingres` on the node where the
writes happen; the table's capture trigger and the protection hook are
node-local; the protection hook refuses `DROP TABLE`, `SET SCHEMA`, dropping the
trigger and columns, for every role. The per-shard aggregate digest is phase 7.

Decision. A ledger table **may** be in a distributed schema, and then lives
entirely on the owner:

- `kingres.ledger_enable('s1.t')` is run **on the owner's primary** (the control
  plane runs it there). It registers the table on that node only; the shells on
  other nodes are not ledger tables. Rows written by shipped statements fire the
  capture trigger on the owner.
- A ledger table cannot be `DROP`ped or moved by `SET SCHEMA` (the protection
  hook), so a distributed schema with a ledger table **cannot be dropped, moved,
  or undistributed**: the ledger's chain is in tables of schema `kingres` on the
  node, which no schema operation moves, and a move would leave the chain behind.
  The pre-check of `distribute_schema`, `move_schema` and `undistribute_schema`
  asks `to_regclass('kingres.ledger_table')` on the owner and refuses with
  `schema_pinned` when it holds a table of the schema. The message says what
  would have to exist for it to move (the per-shard ledger of phase 7).
- A schema that holds ledger tables cannot be distributed **with the data being
  ledger** if the ledger would need to move; it can be distributed in place (8.1,
  the data stays where it is, and the ledger stays with it) with the pin: the
  precondition 6 of 8.1 is relaxed to "a ledger table is allowed, and the schema is
  pinned". **[Recommendation]**.
- `kingres.ledger_enable` run on a node that is not the owner would register a
  shell. To prevent it, `kingres_ledger` gets one small check (a later step,
  section 16 step 11): if `kingres_shard` is installed and the table is in a
  distributed schema owned by another group, `ledger_enable` fails and names the
  owner. Until then the control plane is the only caller and runs it on the
  owner, and `check_cluster` reports `ledger_on_non_owner`.
- The verification and digest of the ledger are per node, as today; the owner's
  digests cover the tables of the schemas it holds. The per-shard aggregate digest
  of phase 7 is what will tie them.
- Contradiction found in the documents (section 19): the contract's `TableClass`
  text says a table can be `distributed` and `ledger` at once, while `shard-phase2b.md`
  11.8 says a ledger table cannot be a hash-distributed or reference table
  in the current designs. For a table of a distributed schema the answer is yes,
  with the pin; for a hash table it stays no.

**[Needs verification]** that `ledger_enable` accepts a table that has an owned
sequence and foreign keys inside the schema, that it is idempotent, and the name
`kingres.ledger_table` used by the pin check.

### 11.2 Standbys and failover

A standby of a group (phase 5) is a physical replica of the group's primary. It
receives the schema's tables, its shells, the catalog rows and the data of a move
through WAL. Consequences:

- Placement is by group. A failover of the owning group's primary (phase 5
  `promote_node`) changes the primary node of the group; the schema's colocation
  group, bucket and epoch are unchanged. Statements follow the group's new
  primary. A statement in flight at the failover fails with the connection or with
  `KS001` after the map-version change and is retried by the client.
- A hot standby routes like its primary (README, limitation "Functions": a hot
  standby routes and scans like its primary but cannot reload a stale bucket map);
  for a schema statement that means a statement on a standby of a non-owner group
  is shipped to the owner's **primary**, and a statement on the owner's standby
  reads the standby's replica of the data (read-only). A write on a standby is the
  standby's own error. **[Needs verification]** (read the routing of a standby at
  the implementation step).
- A move whose source or target group fails over is undone before the switch
  (9.5). A move after the switch is decided by two-phase commit, whose recovery
  rule across a failover is the one of phase 5 (the origin group, the timeline).
- The standbys of the target group receive the in-place copy through physical
  replication of the target primary's WAL. Their bytes are therefore not
  counted separately by the move; the progress and the lag of the target group's
  standbys are what phase 5's replication view shows.
- A lost node: statements for the owner of a schema on a node that is down fail
  (no group primary). Statements on other schemas are unaffected, which is the
  isolation that schema-based sharding is for.

## 12. Security

- **Who may do what.**
  - Create a schema: any role that may `CREATE` in the database. If the database
    distributes new schemas, the creation also registers it. The registration is
    done by the hook with the extension's own privileges (the bootstrap superuser
    through the existing reload mechanism and the `internal_*` functions), not by
    the caller; the caller gets only what `CREATE SCHEMA` gives anyone. A role
    cannot register a schema it did not create, because the registration is in the
    same statement that creates it.
  - Create, alter, drop objects of a distributed schema: the same privileges as in
    a local schema, checked by PostgreSQL on the first node and again on every
    node as the statement arrives (the session on each node is the same role).
  - `distribute_schema`, `undistribute_schema`, `move_schema`, `move_bucket`,
    `set_sharding_mode`, `kingres.alter_distributed_schema`: superuser only
    (`EXECUTE` revoked from `PUBLIC`); the control plane runs them with its service
    role after checking the REST role (section 13).
  - Drop a distributed schema: the schema owner or a superuser, as for any schema;
    the `sql_drop` trigger lets the catalog rows go only while the hook authorized
    the drop (`kingres.internal_ddl_authorized()`).
- **Nothing may give a caller a right it does not have.**
  - A statement shipped to the owner runs in a forwarded session as the **same
    role** and is checked there against that node's ACLs, which are the same as the
    origin's because every `GRANT` on the schema is propagated. A role that has no
    right on the schema is refused on the owner, not only at the origin. A divergent
    grant (made with routing off on one node) is decided by the owner.
  - The forwarded-session proof (the token of `forward_auth.c`) is unchanged: a
    client cannot claim to be forwarded. The marker comment grants nothing.
  - Views: a statement that needs the owner through a view owned by another role
    (not `security_invoker`) is refused, as for hash tables (`kingres_shard_refuse_view_owner`),
    because the fragment would run with the session role's rights, not the view
    owner's.
  - `ship_functions` (6.4) sets a `search_path` on the owner for a statement; it
    is the caller's own path, validated not to contain `pg_temp` or a schema
    that is missing on the owner, so it cannot make the owner resolve a name to
    an object the caller could not make it resolve to on a direct connection.
  - A function of a distributed schema runs with the rights it has everywhere
    (`SECURITY DEFINER` functions run as their owner on the owner node, with the
    function's own `SET search_path`, which the README already requires for the
    extension's functions and which a user function should have).
  - The `COPY` refusals (6.8) close the paths that would read an empty shell
    silently; the shells hold no data that another role could see.
  - A superuser with `enable_routing = off` bypasses all of this, as everywhere.
- **Resource abuse.** A role that may create schemas can create thousands; each is
  a catalog row set on every node and a two-phase transaction.
  `kingres_shard.max_distributed_schemas` bounds it. A database owner who does not
  want this keeps `distribute_new_schemas` off and distributes tenants explicitly.
- **Names.** Every identifier in the dynamic SQL of the new functions goes through
  `%I`/`quote_ident`/`regnamespace`, as in phase 4; the hostile-identifier tests of
  2b section 16.4 are repeated for schema names (quotes, dots, a semicolon,
  Unicode, 63-byte names, a name equal to a role).
- **Information.** `kingres.schemas` shows names, owners and sizes of distributed
  schemas to every role that can read the catalog, as `pg_namespace` does for
  names; the sizes are `NULL` for non-superusers off the owner node, which is the
  existing rule for the sizes of `plan_rebalance`. The error texts of a failed move
  in `bucket_move.detail` are not readable by `PUBLIC` (S4-07).
- **Replication credentials.** Unchanged: the subscription uses the target's
  `node_conninfo` and the server user's password file; nothing is stored in the
  catalog.

## 13. Control plane

### 13.1 Tasks and named steps

New task kinds, each with `subject.type = schema` and `subject.id =
database/schema` (the form of the table subject). Steps are idempotent; a step that
cannot be repeated is `NonIdempotent` and the engine fails the task with
`step_interrupted` if it was running when the process stopped (control-plane
README, "task engine").

`schema.distribute`:

1. `Check the schema` (calls `schema_inventory`; fails with the blockers).
2. `Set REPLICA IDENTITY FULL` (only if the request asked for it; one `ALTER TABLE`
   per table, propagated; skipped otherwise). Not needed to distribute, only to move.
3. `Make the schema distributed` (`distribute_schema`; **NonIdempotent**: its
   result is known by reading `kingres.dist_schema` on a retry, so the step
   first checks that and records `done`).
4. `Verify on every server` (`check_cluster` rows for the schema).

`schema.move` (and the same steps as part of a `rebalance` task's moves):

1. `Check the move` (preconditions, replica identity, cluster settings, nodes'
   version, no ledger pin).
2. `Copy the data` (calls `move_schema`; polls `kingres.bucket_move`;
   `itemsDone` and `itemsTotal` are bytes copied and total, from the subscription
   status and sizes; the step's `detail` shows the phase and what it waits for).
3. `Switch` (visible only through the phase; the task shows "Switching: writers
   of this schema wait").
4. `Clean up the old copy` (waits for `done`; the user may stop waiting: the task
   is `done` at `cleaning` with a note, since the schema is already served by the
   target; **[Recommendation]**, so a 10-minute delay does not hold a task open).

`schema.undistribute`: `Check` (inventory and pins), `Move first` (only when the
request names a target group and it differs; a nested `schema.move`),
`Make the schema local` (`undistribute_schema`), `Verify on every server`.

Cancellation: `POST /tasks/{taskId}/cancel` on a `schema.move` calls
`cancel_bucket_move` while the phase is before `switching`; after that the task
reports `task_not_cancellable`. A cancelled or failed move leaves the schema
`active` on the source (the undo of 9.5).

A rebalance (`rebalance` task) that issues schema moves uses the same steps for
each move and reports them in `inFlightMoves` with `kind: schema` (13.2).

### 13.2 REST contract additions (additive; contract 0.X.0, one minor release for phase 11)

The contract's own rules apply: base path `/api/v1`, camelCase, `x-required-role`
on every operation, RFC 9457 problems, cursor pagination, `202` with a `Task`,
every operation lists 401, 403, 422, 503 (and 400 when it has a body or query,
405 and 409 where they apply). Clients must tolerate codes and enum values they do
not know (`ProblemCode` text). No existing property is removed or made required.

Paths and operations:

| Path | operationId | Role | Notes |
|---|---|---|---|
| `GET /databases/{databaseName}/schemas` | `listSchemas` | `kingres_readonly` | cursor pagination; query `distributed` (bool), `shardGroupId`, `state`; items are `SchemaSummary`. A database without sharding lists its schemas with `distribution: null`. |
| `GET /databases/{databaseName}/schemas/{schemaName}` | `getSchema` | `kingres_readonly` | `SchemaDetail`: the summary, `distributionBlockers` (the output of `schema_inventory` with `supported = false`), `pinned` (reason or null), `moveInProgress` (task id or null). |
| `PUT /databases/{databaseName}/schemas/{schemaName}/distribution` | `distributeSchema` | `kingres_admin` | body `DistributeSchemaRequest { shardGroupId?: int32 or null }`; returns `202` task `schema.distribute`. |
| `DELETE /databases/{databaseName}/schemas/{schemaName}/distribution` | `undistributeSchema` | `kingres_admin` | query `shardGroupId?` (move first); `202` task `schema.undistribute`. |
| `POST /databases/{databaseName}/schemas/{schemaName}/moves` | `moveSchema` | `kingres_ops` | body `MoveSchemaRequest { targetShardGroupId: int32, setReplicaIdentityFull?: bool }`; `202` task `schema.move`. Needs `kingres_ops`, like `startRebalance`. |

Schemas (new): `SchemaSummary { name, owner, distribution: SchemaDistribution | null,
tableCount, sizeBytes | null, estimatedRows | null }`; `SchemaDistribution { state:
active | moving, colocationId, shardGroupId, serverId | null, movingToShardGroupId |
null }`; `SchemaDetail` (summary plus `distributionBlockers: [{objectKind, objectName,
reason}]`, `pinned: {reason} | null`, `moveInProgress: int64 | null`);
`SchemaPage { items, nextCursor }`.

Changes to existing schemas (all additive):

- `ShardingCreateSettings`: new optional `mode` (`row | schema | both`, default
  `row`) and `distributeNewSchemas` (bool, default false; true only with `schema`
  or `both`). `bucketCount` stays but is optional when `mode` is `schema`
  (relaxing `required` on a request is compatible; a client that still sends it is
  accepted and it is ignored).
- `ShardingSettings` (response): `mode`, `distributeNewSchemas`; `bucketCount`
  is null in mode `schema` (already nullable).
- `DatabaseUpdateRequest.sharding`: `required: [enabled]` is relaxed to
  `minProperties: 1` and gains `mode`, `distributeNewSchemas`. Changing the mode
  is synchronous (`200`); `enabled` still cannot be turned off.
- `DatabaseCreateOptions`: `shardingModes` (the three values) and their default.
- `Database.stats`: `distributedSchemaCount`.
- `TableDistribution.method`: `schema` is added; `column` null, `bucketCount` 1.
  `Table.homeShardGroupId`: for a schema table it is the owner's group (the
  description said "null for distributed"; a client already handles a non-null
  value for a plain table).
- `ColocationGroup`: `kind` (`hash | schema`) and `schemaName` (null for hash);
  `listColocationGroups` gains a query `kind`, and the panel's Shards page asks
  for `kind=hash` so that thousands of one-bucket groups do not swamp it.
- `RebalanceStatus.inFlightMoves[]` items: `kind`, `schemaName`.
- `BucketDistribution.servers[]`: `primarySchemas`.
- `TaskKind`: `schema.distribute`, `schema.move`, `schema.undistribute`.
  `TaskSubject.type`: `schema`. The README table "Task kinds and subjects" gets
  the three rows.
- `ProblemCode`:
  - `sharding_mode_in_use` (409): `updateDatabase` changing the mode while the other
    kind exists;
  - `schema_not_distributable` (422): `distributeSchema`, with `errors[]` as the
    blockers (`ValidationIssue.field` = object, `code` = reason);
  - `schema_pinned` (409): a schema with a ledger table cannot move or become local, and `drainServer` is refused for a server whose group holds one;
  - `schema_move_in_progress` (409): DDL-affecting operations on a schema that is
    moving, and a second move; the generic `operation_in_progress` stays for tasks;
  - `schema_already_distributed` / the opposite are `409 conflict`.
  `rebalance_in_progress` is returned by `moveSchema` while a rebalance runs, as
  for any bucket-level operation.

Every new operation is added to `kingres/api/README.md` (operation counts, task
table) and to the panel's generated types (`npm run api:generate`; `api:check`
fails the test run when they differ).

## 14. Web console

The panel is a client of the contract only. Components and files named below are
those that exist (`CreateDatabaseDialog.vue`, `EditDatabaseDialog.vue`,
`DatabaseDetailPage.vue`, `DistributeTableDialog.vue`, `TaskActionDialog.vue`,
`TypeToConfirm.vue`, `GateButton.vue`, `ShardsPage.vue`, `RebalancePage.vue`,
`TasksPage.vue`, `SafeName.vue`, the in-memory mock under `src/mocks`).

### 14.1 Creating a database, and database settings

The create-database dialog (`CreateDatabaseDialog.vue`) has today the group
"Sharding" with a checkbox and a bucket count. Phase 11 changes it to:

- "Sharding" checkbox (as now).
- When on: a choice "Shard by": **Rows** (the default), **Schemas**, **Both**, each with
  one sentence: "Rows: a table is split into buckets by a column. Schemas: each
  schema (a tenant or a service) lives on one server group, and every table in it
  with it. Both: choose per table or per schema."
- "Bucket count" is shown only for Rows and Both.
- "Distribute new schemas automatically": a checkbox shown for Schemas (checked
  by default) and Both (unchecked), with the sentence "Every schema created in
  this database is placed on a server group and kept there. Tables in it can use
  any constraint or query. Schemas created by an extension, `public` and system
  schemas are not distributed."

The database settings (`EditDatabaseDialog.vue`) show the same fields for a
database that has sharding on: the mode can be changed (with the error of the
section 10 table shown as the problem notice: "This database has distributed
schemas, so it cannot be set to Rows only"; the problem code is
`sharding_mode_in_use`), and the default for new schemas can be toggled; the bucket
count stays read-only. For a database with sharding off the existing "turn on
sharding" action gains the mode choice.

### 14.2 The Schemas list

A "Schemas" section on the database detail page (`DatabaseDetailPage.vue`), after
"Tables", using `listSchemas`. One row per schema, columns: **Name** (through
`SafeName`, because names can be hostile), **Owner**, **Distribution** (a badge:
"Distributed" or "Local"; "Moving to group 3" with a link to the task while a move runs),
**Server group** and server, **Tables**, **Size**, with "n/a" (never 0) when the
size is null. Actions per row:

- **Distribute schema** (for a local schema when the mode allows): a dialog that
  runs `getSchema` first and lists the **blockers** in a table ("tenant_7.orders_mv:
  materialized views are not supported in distributed schemas yet"); the
  confirm button is disabled while blockers exist and says why. Optional "Place on
  server group" select (default: automatic, in place). The sentence: "The data stays
  where it is. Other servers receive empty tables. You can move the schema
  afterwards." Starts `distributeSchema` and shows the task progress
  (`TaskActionDialog`).
- **Move schema**: a dialog with the target group (groups that should have
  shards, the current one excluded) and the table of tables that lack a primary
  key or replica identity, with the checkbox "Set REPLICA IDENTITY FULL on these
  tables" (required to continue if the list is not empty). The warning: "Writers of
  this schema wait for a moment while the move switches over (normally well under
  a second). Other schemas are not affected." Starts `moveSchema`.
- **Make local** (for a distributed schema): a type-the-name confirmation
  (`TypeToConfirm`), with the sentence about where the schema ends up ("on server
  group N; clients connected to other servers will no longer see it") and an
  optional "Move to group G first".

Progress and failure states: a task row with the step names of 13.1 and bytes
copied; a `schema.move` that failed shows the failed step, the error, and
"The schema is still on its original group and in use. You can start the move
again." A blocker that appears during the task (a ledger pin) is shown as the
problem notice with the code `schema_pinned`. A moving schema's other actions are
disabled with the reason. The mock layer (`src/mocks`) gets the new operations,
with schemas that are `moving` and a schema `failme` that fails, the same way
`createDatabase` and the table distribute do.

A summary on the **Shards** page: "Distributed schemas per server group" (a count
and size per group) beside the bucket distribution, and the in-flight moves list
shows schema moves with the schema name. The **Rebalance** page's plan preview
lists schema units with their size, and the strategy options of phase 10 apply.

### 14.3 What a read-only user sees

`kingres_readonly` sees the Schemas list, the distribution, the blockers, the
tasks and their progress. The action buttons are `GateButton`s: they are shown
disabled with the tooltip that names the role needed (`kingres_ops` for Move,
`kingres_admin` for Distribute and Make local), as the other operations are.
`kingres_stats` has no access to the page. The mode and default controls in the
database settings are read-only text for roles that cannot call `updateDatabase`.

## 15. Tests

### 15.1 pg_regress (single node; `contrib/kingres_shard/sql`, `expected`)

- `schema_catalog`: the catalog objects, checks (shape of `kind`, `method`),
  `kingres.schemas` on hand-built rows, `schema_inventory` for every object kind,
  `set_sharding_mode` transitions (every cell of the table in section 10),
  `max_distributed_schemas`.
- `schema_security`: every new function is superuser-only, has `search_path` set
  (the existing check), `kingres.schemas` and the placement function with an
  unprivileged role, `PUBLIC` has no `EXECUTE` on the `internal_*` functions.
- `schema_utility_guard`: DDL on a distributed schema on a cluster with a
  fictitious second group (the existing technique): refused off the map group
  primary, refused for each kind of section 7.12, `CREATE SCHEMA` with elements
  refused when distributing, `CREATE INDEX CONCURRENTLY` refused, `SET SCHEMA` cases.
- `schema_routing_decisions`: `EXPLAIN` of the classification table of 6.1 for each
  row, with the owner local and not local, under `enable_schema_routing` on and off,
  checking that an unrouted statement is `Kingres Distributed Scan` and never a local
  scan of a shell (the defence of 6.8).
- `upgrade`: 0.7 to 0.8 against a fresh install, as for 0.3.

### 15.2 TAP, three nodes unless stated (`t/110` to `t/119` for this phase; the integrator renumbers at merge)

- `110_schema_ddl`: create a schema (default on and explicit), create tables,
  indexes, sequences, views, functions, triggers, enum types, partitioned tables
  and a foreign key inside the schema and to a reference table; every node has the
  structure and the same fingerprint; drop, rename, change owner, grants; a failure
  of a node during each step with fault points (`schema_ddl_before_prepare`,
  `_after_first_prepare`, `_after_register`, `_after_fingerprint`) leaves no
  half-created schema after recovery.
- `111_schema_routing`: a statement of every shape (join, subquery, CTE incl.
  data-modifying, window, aggregate, `FOR UPDATE`, `ON CONFLICT`, `MERGE`, `RETURNING`,
  a generic plan with parameters) run from each of the three nodes, compared with
  the same statement on a single node (the comparison harness of 013), and with
  `enable_schema_routing` off (the scan); a transaction of several statements with
  a rollback to savepoint; `search_path` variations including a path that resolves
  through `"$user"`, an unqualified name and a hostile path (`pg_temp` first).
- `112_schema_mixed`: a schema and a hash table, two schemas on different groups,
  a schema and a local table, a schema and a reference table; every `SELECT` is
  answered correctly (the ADR 0022 rule: none refused); the refused writes give the
  documented messages; the plan check of 6.8 on forced bypasses.
- `113_schema_functions_sequences`: sequence values from `nextval` through a column
  default and bare, from every node, never duplicated; a function call statement
  runs at the origin and its inner statements route; with `ship_functions` the
  statement is shipped and the `KS001` for a body that touches another schema.
- `114_schema_move`: a move with rows, sequences, a foreign key to a reference
  table, a partitioned table, a trigger (not fired for the copied rows), under a
  write load from all three nodes (the model of 025/027: afterwards the data equals
  the model, no row lost or doubled); writers of **another** schema on the same
  nodes are not delayed beyond a bound; readers are never blocked; a long
  transaction holds the switch back, the attempts are bounded and the failure
  reports the blocker.
- `115_schema_move_faults`: fault injection at every phase boundary and inside the
  switch (crash of the map group primary, source, target; loss of connection),
  then `resume_bucket_moves`: the schema is whole on exactly one group, no slot,
  publication, subscription or stray rows; **the recovery worker alone** resolves a
  killed session (S4-02); the sequence of review S4-01 (move away and back inside
  one scan's `DECLARE`) returns the right row count; a direct write to a partition
  during the switch is not lost (S4-03); a long writer and a long reader do not
  stall other schemas (S4-04).
- `116_schema_convert`: `distribute_schema` of a schema with data (nothing moves,
  other nodes have empty tables, a statement from every node answers), of an empty
  schema with a group, with each unsupported object (refused, named); a failure
  during the structure creation; `undistribute_schema` and the stray-row refusal;
  the round trip distribute, move, undistribute, and the data equal at the end.
- `117_schema_mode_default`: the mode matrix of section 10, the default for new
  schemas and the override, a role that is not a superuser cannot change them,
  `max_distributed_schemas`.
- `118_schema_ledger_standby`: a ledger table in a distributed schema (enabled on
  the owner), writes from another node, the pin refusals, `check_cluster`
  `ledger_on_non_owner`; failover of the owner group (phase 5 harness) with schema
  statements in flight; a move whose source fails over is undone.
- `119_schema_scale`: 2,000 schemas of 5 tables created from the map group primary,
  the time per `CREATE SCHEMA`, per `CREATE TABLE`, the catalog cache load time and
  size in a backend, `add_node` with the batches; the numbers are reported, not
  asserted (they decide the question of 5.3 and 7.11).

Every test that waits for a state waits for the state itself, not for a sleep
(review S4-08). Races are run in a loop and the number of runs is reported.

### 15.3 Control plane (Go)

Integration tests under `internal/clustertest` and `cmd/kingresd`, with real
instances (the existing three-process harness): `distributeSchema` for a schema of
a three-server cluster (task steps in order, `check_cluster` clean); `moveSchema`
with a writer running (progress monotone, final state, no lost row); the task
resumed after `kingresd` is killed at each step (a `NonIdempotent` step is not run
twice; `step_interrupted` where it applies); `undistributeSchema`; `PATCH
/databases/{name}` mode changes and `sharding_mode_in_use`; role checks (`kingres_readonly`
gets 403 on each mutating operation, `kingres_ops` cannot distribute); a rebalance that
includes schema moves; a hostile schema name through every operation; the contract
tests that compare the served responses with `openapi.yaml`.

### 15.4 Panel (Vitest and Testing Library; browser tests)

Against the mock API: the create dialog's three modes and the conditional fields; the
settings dialog error for `sharding_mode_in_use`; the Schemas list (columns, null
sizes shown as "n/a", moving badge, hostile names through `SafeName`); the three
dialogs (blockers disable the button with a reason; the replica-identity choice is
required when tables lack a key; type-the-name confirmation); the task states
(running, failed, with the failed step and the "still in use" text); what each role
sees; `api:check` for the generated types. Browser tests (`test:browser`): layout
at 1366, 683 and 320 px for the new section and dialogs, keyboard operation, axe.

## 16. Implementation order

Each step is committable and testable alone; the extension is changed by one line
of work at a time (architecture.md). Steps 1 to 6 are the extension; the version
bump and the upgrade script are in step 2 and the later steps add no version.

1. **Prerequisites that are not phase 11's own** (merged before): phase 10's catalog
   (0.7: `placement_epoch`, `modulus`, `bucket_move.kind`, `node.weight`) and the
   recovery worker's call of `resume_bucket_moves` and `cleanup_moved_buckets`
   (review S4-02; phase 10 section 7.2 plans it), with their tests.
2. **Catalog and views.** Section 5.2, the delta functions of 5.3, `kingres.schemas`,
   `schema_inventory` (read-only), the upgrade script, the `schema_catalog` and
   `upgrade` tests. No behaviour change: nothing yet creates a schema group.
3. **Single-source scan and the shell defence.** 6.6 and 6.8 for a schema table that is
   registered by hand in the test (a fictitious group): reads through the scan, the
   plan check, the `COPY TO` refusals. This is the step that makes ADR 0022's rule hold
   first.
4. **DDL on a distributed schema** (7.1 to 7.8, 7.12) with `distribute_schema` of an
   empty schema and `CREATE SCHEMA` by default, the fingerprint, `set_sharding_mode`,
   and `max_distributed_schemas`. Tests 110 and 117.
5. **Path S** (6.2, 6.5, 6.9): statements shipped whole, forwarded-session owner
   check, `enable_schema_routing`. Tests 111 and 112.
6. **`move_schema`** (9.2 to 9.8) with the review fixes, `check_cluster` additions,
   the schema items in `plan_rebalance` and the drain (9.9; code of phase 10, changed
   together with it). Tests 114 and 115.
7. **Conversion with data** (`distribute_schema` of a schema with data, in place;
   `undistribute_schema`) and several schemas on one group (6.3). Test 116.
8. **`ship_functions`** (6.4). Test 113 completed.
9. **Writes that read another schema or kind** through the router of 2b, `CREATE TABLE
   AS` into a schema, `COPY FROM` into a schema table (6.7, 6.8 item 3), after phase 2b
   and phase 9 provide their parts.
10. **Single tables in and out** (`SET SCHEMA` with rows, 7.9, 8.3), after phase 9.
11. **Ledger** (11.1): the pin, `ledger_on_non_owner`, and the small check in
    `kingres_ledger`. Test 118.
12. **Scale work** if the measurements of test 119 ask for it (batching of DDL
    scripts, `add_node` batches).
13. **Control plane**: the contract additions, the operations, the task kinds, the
    Go tests. Can start after step 4 with the mock of the extension's functions.
14. **Panel**: the create dialog and settings (after the contract of step 13), the
    Schemas list and dialogs, mocks and tests.
15. **Documentation**: the README of the extension (a section "Distributed
    schemas", the limits), `kingres/api/README.md`, `kingres/web/README.md`, the work
    log entry.

## 17. Open questions, each decided

1. **Where does the schema live in the catalog: a new colocation kind or a separate
   object?** **[Recommendation]** a colocation group of kind `schema` with one
   bucket (section 5.1): it reuses placement, map version, moves, the rebalance
   plan's output and the control plane's views; a separate object would duplicate
   them. Cost: `colocation` and `dist_table` change, and the hash-specific code must
   check `kind`.
2. **Ordinary tables with shells, or partitioned tables with one bucket?**
   **[Recommendation]** ordinary tables (5.1): partitioned tables cannot carry a
   tenant's constraints.
3. **Default placement rule.** **[Recommendation]** fewest schemas per unit of weight, tie by lowest
   load, then lowest group id (9.1); size-based correction is the
   rebalancer's work.
4. **Default mode for a new database.** **[Recommendation]** `row`, as today, and
   "distribute new schemas" checked only when `schema` is chosen (10).
5. **A ledger table in a distributed schema.** **[Recommendation]** allowed, on
   the owner, with the schema pinned (11.1); a move needs phase 7.
6. **Materialized views, rules, row-level security.** **[Recommendation]** refused in
   this phase (7.12): each needs routing of a utility statement or an identity of the
   caller on the owner that is not settled.
7. **`CREATE SCHEMA` with elements.** **[Recommendation]** refused when the schema is
   distributed (7.2); simple, matches the documented Citus behaviour, and
   removes the sub-statement ordering problem.
8. **DDL off the map group primary.** **[Recommendation]** keep the rule of phase 2a
   (refuse with the hint). A tenant onboarding tool connects to the map group
   server; forwarding DDL from other nodes is a later phase, because it changes how
   every DDL statement is entered, not only schema DDL.
9. **Structure generator: in-extension or `pg_dump`.** **[Recommendation]**
   in-extension (8.1) for the supported kinds, refusing the others by name; `pg_dump`
   by the control plane is a possible later widening, but it makes the extension
   function depend on text from outside and widens what a superuser can inject into
   other nodes, so it is not the first choice.
10. **Materialized views during a move.** Not a question: refused (7.12).
11. **Replica identity for tables without a key.** **[Recommendation]** the move
    refuses and names the tables; the console offers `REPLICA IDENTITY FULL` as a
    separate propagated change. The extension never changes a user's table as a
    side effect of a move.
12. **Statements that touch two schemas of one group.** **[Recommendation]** scan by
    default; shipped whole in step 7, with both placement checks (6.3).
13. **Functions.** **[Recommendation]** statements that call user functions run at
    the origin by default; `ship_functions` per schema is an opt-in (6.4).
14. **The limit on the number of schemas.** **[Recommendation]** 10,000, a guess
    until test 119 gives numbers (5.5).
15. **When is a task `done`.** **[Recommendation]** a move task is done when the
    schema is served by the target (at `cleaning`), with the cleanup continuing
    in the background (13.1).

## 18. Known limits after this phase

- DDL on a distributed schema runs on the map group primary only. Clients that
  create tenants connect there (the control plane does).
- A table of a distributed schema is on one group; a tenant cannot be larger than
  one group's disk. There is no way to split a schema (phase 10 splits buckets of
  hash tables only).
- A distributed schema is reachable from every node (statements are routed to its
  owner); a schema that was made local again exists on one node only.
- Statements that touch two distributed schemas on different groups, a schema and a
  hash table, or a schema and a local table are answered by the distributed scan:
  correct, and slow for large inputs. There is no repartitioning (phase 12). The
  consistency note of phase 2a applies: no global snapshot across groups.
- `INSERT ... SELECT`, `CREATE TABLE AS` and `COPY FROM` into a schema table that
  read from elsewhere are refused until steps 9 and 10.
- User-defined functions run at the origin unless the schema opted in; a function
  that touches another schema from inside a shipped statement raises `KS001`.
- A `nextval` call with a non-constant argument runs where the statement runs;
  `lastval()` and `currval()` are not reliable across a routed statement.
- Materialized views, rules, row-level security, policies, statistics objects,
  foreign tables, publications and event triggers are refused in a distributed
  schema.
- Unlogged tables cannot be in a schema that is moved.
- A schema with a ledger table cannot be moved, made local or dropped.
- The writers of a schema wait during the switch of its move (an attempt, normally
  tens of milliseconds, at most `move_switch_timeout`); the old copy stays until the
  cleanup delay and takes disk space on the source; a move back to a group whose old
  copy is not yet emptied is refused.
- The number of distributed schemas per database is bounded by a configured guess;
  every DDL statement is a two-phase transaction on every node; `add_node` takes time
  proportional to the number of objects.
- `VACUUM`, `ANALYZE`, `REINDEX`, `CLUSTER` of a schema table act on the node they
  run on (the owner's copy is maintained by autovacuum).
- Roles, tablespaces and extensions are not synchronized (the existing limit): a
  role used as a schema owner must exist on every node.
- Local helper schemas in a database that distributes by default need a superuser to
  create them (`SET kingres_shard.distribute_new_schemas = off`).

## 19. Needs verification

1. That phase 10's catalog (`placement_epoch`, `modulus`, `max_modulus`, `bucket_move.kind`, `node.weight`) is implemented as its note describes (5.2).
2. Whether `shard-phase9.md` gives the version number assumed in 5.6 (0.6), the home of
   an undistributed table (8.2) and the row-copy primitive (8.3).
3. The names `citus.enable_schema_based_sharding` and `citus_schemas` and the exact
   columns of the latter (not found in the pages that could be read).
4. How a remote statement already passes parameters, so that a generic plan can be
   routed (6.2).
5. Whether a transaction with one remote writer and no local writes commits without
   a prepare (6.5).
6. Whether the cached connection of a backend keeps `currval` state between
   transactions (6.4).
7. That `COPY t TO` of a hash table bypasses the planner hook and reads local
   partitions only, and that `COPY (SELECT * FROM t) TO` is the correct redirection
   (6.8).
8. That a `CustomScan` with `scanrelid = 0` keeps the relation OIDs for plan
   invalidation (6.9).
9. That a column of an enum type of a distributed schema is carried correctly by the
   scan fragment (7.6).
10. Time and lock hold of `CREATE TABLE`, `CREATE SCHEMA` and `distribute_schema` for
    realistic schemas, and the size and load time of the catalog cache at 2,000 and
    10,000 schemas (5.3, 7.11, 8.1, 9.12).
11. That the sequence relation's `last_value` includes values cached by sessions, so
    copying it to the target never repeats a handed-out value (9.4 step 5).
12. That a replication slot is not present on a standby after failover, and what
    happens to a subscription on a target that fails over (9.5, 11.2).
13. That the check order of 9.4 step 7 (lock, then catalog check) holds for
    statements planned from a cached plan.
14. That logical-replication apply copies a cycle of foreign keys between two tables
    into empty tables (9.2); until shown, the phase 4 refusal stays.
15. Routing of a statement on a hot standby of a non-owner group and of the owner
    group (11.2).
16. That `ledger_enable` works on a table with an owned sequence and foreign keys,
    is idempotent, and that `kingres.ledger_table` is the name to test for the pin
    (11.1); and whether the database `ledger.mode` of the contract applies to new
    tables automatically (it would then reach tables of a distributed schema on
    every node).

Findings in existing documents, code and contract, for their owners (not changed
here):

- At the last commit the recovery worker calls neither `resume_bucket_moves` nor
  `cleanup_moved_buckets` (`src/recovery.c`), contrary to the README of the
  extension and `shard-phase4.md`; and there is no placement counter in
  `src/catalog.c` (review S4-01, S4-02 are open at that commit).
- The contract's `TableClass` description allows a distributed ledger table; the
  designs (`shard-phase2b.md` 11.8, `ledger-mvp.md`) say a ledger table cannot be
  distributed or a reference table today.
- The README of the extension says `COPY t TO` of a distributed table was not
  examined; it probably returns local partitions only (6.8).
- `Table.homeShardGroupId` is documented as null for distributed tables; this phase
  gives it a value for schema tables.
- `DatabaseUpdateRequest.sharding` requires `enabled`, so the mode cannot be changed
  without it; section 13.2 relaxes that.
- The README says DDL goes through the map group primary while phase 5 says every
  server accepts reads and writes: a tenant-creation client that connects to another
  server gets a refusal for DDL (section 18).
- Phase 4's "Orphan tables" handling and `check_cluster` mention "No rows means
  consistent", but `check_cluster` does not compare row contents (S4-10); this note
  repeats the wording that it is structural.
