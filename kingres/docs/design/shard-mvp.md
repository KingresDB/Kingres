# kingres_shard: design of the sharding MVP (phase 1)

Status: accepted for implementation, 2026-11-14.
Scope: phase 1 of the roadmap in [architecture.md](../architecture.md). This
note turns the architecture into a concrete, buildable design and records the
choices made while doing so. Where it refines the architecture document, the
refinement is called out.

## Goals and non-goals

Phase 1 delivers an extension that can:

1. keep the cluster catalog (nodes, colocation groups, distributed tables, the
   bucket map and its version);
2. turn a table into a distributed table with a fixed number of hash buckets,
   placed across the registered shard groups;
3. route a statement that touches a single shard group to that group and
   return its result, from any node that has the catalog.

Not in phase 1 (they are phases 2, 4 and 5): statements that span several
groups, 2PC, reference tables, DDL propagation after table creation, bucket
movement and rebalance, failover handling, and the client router.

## Physical layout: a bucket is a hash partition

**Refinement of the architecture document.** The architecture says the bucket
function must agree with `PARTITION BY HASH (MODULUS N, REMAINDER r)`. The MVP
makes that the storage layout itself:

- A distributed table is, on every node, an ordinary PostgreSQL table
  partitioned `BY HASH (<distribution column>)`.
- Bucket `b` of the table is the partition
  `FOR VALUES WITH (MODULUS N, REMAINDER b)`, named `<table>_b<bbbb>`
  (zero-padded to four digits), in the same schema as the table.
- A node has the partitions of exactly the buckets its group owns. The parent
  table exists on every node; the partitions of buckets owned elsewhere do not
  exist locally.

Consequences, all of which we want:

- On a single node the result is a plain hash-partitioned table. Every
  PostgreSQL feature works on it with no routing code involved.
- PostgreSQL itself enforces the two rules the architecture requires: a
  primary key or unique constraint must include the distribution column, and a
  row can only be stored in the bucket its key hashes to.
- A statement forwarded to the owning group needs no rewriting. The same SQL
  text runs there against the same table name, and PostgreSQL's own partition
  pruning finds the bucket.
- A statement that arrives at a node which does not own the bucket fails with
  PostgreSQL's "no partition of relation ... found for row" (for writes) or
  returns no rows from a partition that is not there (for reads). The router
  must therefore never run a statement locally unless the map says the bucket
  is local; see "Stale maps" below.
- A bucket is one table, so phase 4 can move it with logical replication
  without row filters.

**Cost, and a changed default.** One table per bucket per distributed table
means the number of relations on a node is about
`tables x buckets / groups`, and on a single-node cluster it is
`tables x buckets`. With the 1024 buckets suggested in the architecture, 100
distributed tables on one node are more than 100,000 tables before indexes.
The MVP therefore sets the default to **128 buckets**
(`kingres_shard.default_bucket_count`, range 1 to 4096), which still gives a
rebalance granularity under 1% and matches the smaller of Couchbase's two
vBucket counts. The count remains fixed per colocation group at creation. This
default is a tunable, not an architectural change; it is recorded here and in
the work log so the project owner can overrule it.

## The bucket function

`bucket = compute_partition_hash_value(...) % N`, using the extended hash
support function of the distribution column's default hash operator class and
PostgreSQL's `HASH_PARTITION_SEED`. This is exactly what
`satisfies_hash_partition` computes for a single-column hash key, so routing
and storage cannot disagree. The implementation must call the exported
`compute_partition_hash_value()` from `partitioning/partbounds.h` rather than
reimplement it.

SQL-visible: `kingres.bucket_for(relation regclass, value anyelement) returns int`
and `kingres.bucket_for(colocation_id int, value anyelement) returns int`.
A regression test must check, for several types, that `bucket_for` equals the
partition PostgreSQL actually stores the row in.

Supported distribution column types in phase 1: any type with a default hash
operator class that has an extended hash function. Only a single distribution
column is supported.

## Catalog

All objects are in schema `kingres`, created by the extension script. The
catalog is per database (extensions are per database), as in Citus.

```sql
-- One row per PostgreSQL instance that takes part in the cluster.
kingres.node (
  node_id            int primary key,
  group_id           int not null,          -- shard group; 0 is the map group
  host               text not null,
  port               int  not null,
  role               text not null check (role in ('primary','standby')),
  is_active          bool not null default true,
  should_have_shards bool not null default true,
  server_group       text,
  unique (host, port)
)

-- Tables with the same colocation_id have the same bucket count, the same
-- distribution column type and the same bucket placement, so equal keys are
-- always on the same group and joins on the key stay local.
kingres.colocation (
  colocation_id    int primary key,
  bucket_count     int not null check (bucket_count between 1 and 4096),
  dist_column_type regtype not null,
  dist_collation   oid not null default 0
)

kingres.dist_table (
  relid         regclass primary key,
  method        text not null check (method in ('hash')),  -- 'reference' comes in phase 2
  dist_column   name not null,
  colocation_id int not null references kingres.colocation
)

kingres.bucket_map (
  colocation_id int not null references kingres.colocation,
  bucket        int not null,
  group_id      int not null,
  state         text not null default 'active' check (state in ('active','moving','orphan')),
  primary key (colocation_id, bucket)
)

-- Single-row table: the version of the whole map.  Every change to node,
-- colocation, dist_table or bucket_map increments it (statement triggers).
kingres.map_version (version bigint not null)

-- Single-row, node-local table (never synchronized): which node this is.
kingres.local_node (node_id int not null)
```

`dist_table.relid` is a `regclass`, which stores an OID. OIDs differ between
nodes, so catalog synchronization must transfer the table by its
schema-qualified name and resolve it on the receiving node.

## Node registry and catalog synchronization

The node on which `kingres.init_cluster()` is run becomes the map group
primary (node 1, group 0). In phase 1 the map group also holds data, so a
single node is a working cluster.

- `kingres.init_cluster(host text, port int)`: registers the local node as
  node 1 / group 0 and sets `local_node`. Errors if already initialized.
- `kingres.add_node(host text, port int, group_id int default null, role text default 'primary') returns int`:
  only on the map group primary. Registers the node (a new group when
  `group_id` is null), connects to it, creates the extension there if needed,
  copies the whole catalog to it, and sets its `local_node`. New groups own no
  buckets until a rebalance (phase 4) or until a table is created afterwards.
- `kingres.remove_node(node_id int)`: only when the node's group owns no
  buckets.
- `kingres.sync_catalog()`: pushes the full catalog from the map group
  primary to every active node. Every catalog-changing function calls it
  before returning.

Phase 1 synchronization is deliberately simple: inside the caller's
transaction the function opens a transaction on each remote node, replaces
the remote catalog contents, and commits the remote transactions in a
pre-commit callback. This is not atomic across nodes; a crash between the
remote commits can leave a node with a stale catalog, which `sync_catalog()`
repairs. Phase 2 replaces it with 2PC. This limitation must be stated in the
extension's README.

Node-to-node connections use libpq with the node's host and port, the current
database name and the current user. Authentication is whatever
`pg_hba.conf` and the password file provide; credentials are never stored in
the catalog.

## Creating a distributed table

`kingres.create_distributed_table(relation regclass, dist_column name, colocate_with regclass default null, bucket_count int default null) returns void`

Only on the map group primary. Steps:

1. Check that `relation` is an ordinary, empty, non-partitioned table without
   inheritance children, and that `dist_column` exists and has a hashable
   type. (Converting a table that already holds data is a later phase.)
2. Choose the colocation group: the one of `colocate_with`, or an existing
   group with the same type and bucket count, or a new one. A new group
   assigns its buckets round-robin over the groups that have
   `should_have_shards`, in group order.
3. Replace the table by a hash-partitioned table of the same name, columns,
   defaults, constraints and indexes. Constraints that PostgreSQL does not
   allow on a partitioned table (a unique constraint that does not include
   the distribution column) are reported as an error that names the
   constraint.
4. Create the parent on every other node, and on every node create the
   partitions of the buckets its group owns.
5. Record the table in `dist_table` and synchronize the catalog.

`kingres.undistribute_table(relation regclass)` is not part of phase 1.

## Routing

Phase 1 routes whole statements; it does not rewrite SQL.

**Decision in the planner hook.** For each statement the hook finds the
distributed tables it references.

- No distributed table: the standard planner.
- All referenced distributed tables are in one colocation group, and each of
  them is restricted, in the top-level `AND` of the `WHERE` clause (or by the
  `VALUES` row of a single-row `INSERT`), by `dist_column = <constant or
  parameter>` with values that hash to the same bucket: the statement belongs
  to the group that owns the bucket.
  - The owner is the local group: the standard planner.
  - The owner is another group: a plan with a single `CustomScan`
    ("Kingres Remote Statement") that forwards the statement.
- Otherwise, and the cluster has a single group: the standard planner (every
  bucket is local).
- Otherwise: `ERROR: statements that span shard groups are not supported yet`
  with a hint naming the distribution column. This is the phase 1 boundary;
  phase 2 lifts it.

Parameter values are available at planning time only for custom plans. When a
parameter's value is not known, the hook must not cache a routing decision in
a generic plan; it forces a custom plan for statements that reference
distributed tables.

**Execution.** The custom scan deparses the statement with
`pg_get_querydef()`, sends it with its parameters to the primary of the owning
group over a cached libpq connection, and returns the result rows converted
with the input functions of the target-list types. For `INSERT`, `UPDATE` and
`DELETE` without `RETURNING` it reports the affected row count.

**Loop prevention.** A forwarded statement runs in a session that has
`kingres_shard.forwarded = on`. In such a session the hook never forwards; if
the bucket is not local it raises `ERROR: bucket <b> of <table> is not on this
node (stale map version <v>)` with SQLSTATE class `KS` (code `KS001`).

**Stale maps.** When the originating node receives `KS001`, it reloads the
catalog from the map group primary and retries the statement once.

**Transactions.** A remote connection joins the local transaction: `BEGIN` on
first use with the local isolation level, `COMMIT` in the pre-commit callback,
`ROLLBACK` on abort, and matching savepoints for subtransactions. In phase 1 a
transaction may write on at most one remote group and may not write both
locally and remotely; a second writer raises a clear "not supported yet"
error. Reads from several groups in one transaction are allowed.

## Catalog cache

The planner hook runs for every statement, so catalog lookups must be cheap.
Each backend keeps the distributed-table list and the bucket map in memory and
invalidates it through relcache invalidation callbacks on the catalog tables.
A statement that references no relation in the cache must pay no more than a
hash lookup per range-table entry.

## Settings

| GUC | Default | Meaning |
|---|---|---|
| `kingres_shard.default_bucket_count` | 128 | Bucket count of a new colocation group |
| `kingres_shard.enable_routing` | on | When off, the planner hook does nothing (for debugging and upgrades) |
| `kingres_shard.forwarded` | off | Set by the router on forwarded sessions; not for users |
| `kingres_shard.node_connect_timeout` | 5s | Connection timeout between nodes |

The extension must be in `shared_preload_libraries`, because it installs
planner and transaction hooks.

## Introspection

- `kingres.version() returns text`: the KingresDB version (`KINGRES_VERSION`).
- View `kingres.buckets`: table, bucket, partition name, group, owning node,
  state, and on the owning node the partition size.
- View `kingres.nodes`: `kingres.node` plus whether the row is the local node.
- `EXPLAIN` on a forwarded statement shows the custom scan, the target node
  and the remote SQL.

## Source layout

```
contrib/kingres_shard/
  Makefile, meson.build, kingres_shard.control, kingres_shard--0.1.sql, README.md
  src/kingres_shard.c      module load, GUCs, hook installation
  src/catalog.c|h           catalog access and the per-backend cache
  src/hashing.c|h           bucket function
  src/node_conn.c|h         libpq connection cache and remote transactions
  src/node_admin.c          init_cluster, add_node, remove_node, sync_catalog
  src/dist_table.c          create_distributed_table
  src/planner.c|h           routing decision in the planner hook
  src/remote_scan.c|h       the custom scan
  sql/, expected/           pg_regress tests (single node)
  t/                        TAP tests (multi-node)
```

## Tests required for phase 1

Single node (pg_regress): catalog creation and constraints; `bucket_for`
against real partition placement for int4, int8, text, uuid; distributing a
table with a primary key that includes the key, and the error for one that
does not; full SQL (join, aggregate, update, delete, transaction) on a
distributed table on one node; `kingres.buckets`.

Multi-node (TAP, three nodes): `add_node` and catalog equality on all nodes;
bucket placement after `create_distributed_table`; insert and select by key
from every node for keys owned by every group; `EXPLAIN` shows forwarding;
update and delete by key; prepared statements with parameters; a transaction
that reads from two groups; the "span shard groups" error; rollback of a
forwarded write; the stale-map retry.

## Implementation notes and deviations

Added when `contrib/kingres_shard` (milestones 1 to 3) was implemented, on
PostgreSQL 18.7. Everything above is implemented as written except what is
listed here. Each item says what differs and why.

**Corrections to what the design assumed about PostgreSQL 18**

- *Generic plans cannot be turned into custom plans by a hook.* "It forces a
  custom plan" is not possible from `planner_hook`: the plan cache decides.
  What the extension does instead: when a routing decision depends on a
  parameter whose value is not known (the planner is called with no bound
  parameter values, i.e. building a generic plan), the hook returns a
  one-node plan that raises a clear error if it is ever started and whose cost
  is 1e30. With `plan_cache_mode = auto` the plan cache therefore always
  prefers a custom plan (the first five executions are custom anyway, and the
  generic plan built for the cost comparison never wins);
  `plan_cache_mode = force_generic_plan` fails with a hint. SQL-language
  functions, whose bodies are planned without parameter values, hit this rule
  for statements on distributed tables.
- *Relcache callbacks do not fire on DML.* The catalog cache cannot be
  invalidated "through relcache invalidation callbacks on the catalog tables"
  by ordinary row changes. The `map_version` statement trigger, which runs
  after every change of the four synchronized tables, calls
  `CacheInvalidateRelcacheByRelid()` for `kingres.map_version` and for every
  distributed table; the backends' relcache callback drops the cache on the
  first, and cached plans of distributed tables are invalidated by the others.
- *The executor does not count the rows of `INSERT`/`UPDATE`/`DELETE ...
  RETURNING` for a plan that has no `ModifyTable`.* `ExecutePlan` counts rows
  only for `SELECT`; the custom scan counts them itself (`es_processed`).
- *The operator of `bigint_column = 5` is `int84eq`*, not the same-type
  equality operator. The routing decision accepts any equality operator of the
  hash operator family of the column and hashes the constant with the hash
  function the family has for the constant's own type. Parameter values and
  constants are first simplified with `eval_const_expressions()` (so `5::bigint`
  and `$1` work). The `excluded` pseudo-relation of `ON CONFLICT DO UPDATE` is a
  second range table entry of the target table and is skipped.
- A role whose name equals a schema name captures unqualified names through
  `"$user"`; the regression and TAP tests pin `search_path = public`.

**Catalog and installation**

- The schema `kingres` is created by the install script (not by the control
  file's `schema` key) so that it is a member of the extension and is dropped
  with it. `map_version` and `local_node` get a unique index on `(true)` to
  make them single-row tables; the catalog tables have a few extra `CHECK`
  constraints (`group_id >= 0`, port range, `bucket >= 0`).
- An event trigger on `sql_drop` removes a dropped table from `dist_table`, so
  the catalog never holds a dangling OID. This is local to the node where the
  table was dropped (DDL is not propagated).
- Distributed tables are written into the synchronized snapshot as
  `(schema, name)`; collation OIDs and type names (`regtype`) are transferred
  as they are, so non-built-in types and non-default collations are not yet
  portable between nodes.
- Administration functions and internal helpers have `EXECUTE` revoked from
  `PUBLIC`; `kingres.internal_catalog_snapshot()` stays public because a node
  that finds its map stale reads it from the map group as an ordinary user. The
  reload of a stale catalog applies the snapshot as the bootstrap superuser.
- `kingres_shard.forwarded` is visible in `SHOW ALL` (the regression test
  lists the GUCs).

**Node registry**

- `add_node` also creates, on a new primary, the parents of all existing
  distributed tables (without partitions): the catalog sync resolves tables by
  name, so they must exist before the catalog is applied. A node that already
  has a `local_node` row or distributed tables is refused. `remove_node`
  clears the removed node's catalog when it can be reached (a warning
  otherwise). A standby is only registered; its catalog arrives by physical
  replication and it is not a sync target.
- `init_cluster` returns void.

**`create_distributed_table`**

- Besides the checks of the design it refuses tables with foreign keys (in
  either direction), user triggers, row-level security, storage parameters,
  exclusion constraints, inheritance and persistence other than permanent, with
  an error naming the object. A unique constraint or unique index without the
  distribution column is refused by the extension itself, with the constraint's
  name, before PostgreSQL's own check would name only the table. Sequences owned
  by columns are detached before the old table is dropped and attached to the
  new one (created on other nodes if missing; values are node-local). The owner
  is preserved; privileges, comments and statistics objects are not.

**Routing**

- *Statement shapes.* The key may come from the top-level `AND` of the `WHERE`
  clause, from the `ON` clause of inner joins, or from the single row of an
  `INSERT`. A table in a statement that mixes distributed tables of several
  colocation groups is refused. `MERGE`, statements that mix a remote
  distributed table with local tables, and scrollable cursors on remote
  statements are refused with "not supported yet". With `kingres_shard.forwarded`
  on, a statement that cannot be decided is run locally (the origin already
  decided).
- *Deparse.* The remote SQL comes from `pg_get_querydef()` run with
  `search_path = pg_catalog`, so every name is schema-qualified; parameters are
  sent in text form with their OID when it is built in and unspecified
  otherwise. Remote sessions get fixed `DateStyle`, `IntervalStyle` and
  `extra_float_digits` through connection options.
- *Stale-map retry.* The retry re-runs the deparsed statement through SPI after
  reloading the catalog, so it is planned with the new map and may run locally
  or be forwarded elsewhere. It is possible only when the remote transaction
  has run no earlier statement, because the remote side can then be rolled back
  to just before; otherwise `KS001` reaches the client. It does not apply when
  the origin is the map group primary (whose catalog is the reference).
- *Write limits* are enforced when the forwarded write starts and by an
  executor hook for local writes after a remote write. A reload of the catalog
  after a stale map does not count as a local write.
- *Commit.* Remote transactions commit in the pre-commit callback, in cache
  order, before the local commit; this is as non-atomic as the catalog sync.
  `PREPARE TRANSACTION` after remote work is refused.

**Fix round after the independent review of 2026-11-14**
(`kingres/docs/reviews/2026-11-14-kingres-shard-0.1.md`, findings S-A to S-D
and 1 to 4; every item below changes or replaces something stated above)

- *Security (S-A, S-B).* Every function of the install script, the C ones too,
  has `SET search_path = pg_catalog, kingres, pg_temp`; the one `SECURITY
  DEFINER` function, the `sql_drop` event trigger, qualifies every type, cast and
  operator with `pg_catalog` and only forgets tables whose table object itself
  (not a column) was dropped. The reload of a stale catalog no longer runs an SPI
  statement as the bootstrap superuser: it looks the function up and parses the
  document while it still has the rights of the user, then calls
  `kingres.internal_apply_catalog()` by OID, so no name is resolved through the
  session's `search_path` or temporary schema while privileges are elevated. A
  pg_regress test scans `pg_proc` for functions of the extension without a pinned
  `search_path` ending in `pg_temp` and for `SECURITY DEFINER` functions outside an
  allow-list. This replaces "forget_dropped_tables() (the only SECURITY DEFINER
  function) also pins its search path" of the review's "found sound" list.
- *Session settings (S-C).* Deparse, the output of parameters and the local
  re-execution of a deparsed statement (stale-map retry) run under
  `kingres_shard_set_transmission_modes()` (the technique of `postgres_fdw`,
  plus `standard_conforming_strings = on` and `bytea_output = hex`). Remote
  sessions get the same settings as connection options and again as `SET LOCAL`
  at the start of each remote transaction, `client_encoding` of the local
  database, and the `TimeZone` of the local session, re-sent when it changes
  between statements. Remote `search_path` is `pg_catalog`. This replaces the
  earlier "Remote sessions get fixed `DateStyle`, `IntervalStyle` and
  `extra_float_digits` through connection options", which left the statement
  text dependent on the local session.
- *Nested statements (S-D).* The routing decision is made on the query tree
  before planning, so a SQL-language function that the planner expands into the
  statement (a set-returning function with a one-statement body) was read from
  the local partitions only, unrestricted. After planning, the plan is checked for
  distributed tables that the analysis did not count; such a statement is
  refused ("through a SQL-language function are not supported yet"). Other
  nested contexts (PL/pgSQL, `DO`, `EXECUTE`, triggers, rules, views, cursors,
  `CREATE TABLE AS`, `COPY (query) TO`, `EXPLAIN ANALYZE`, prepared statements
  under each `plan_cache_mode`) plan their statements one by one through the
  hook and were already routed or refused. Found on the way: a routed statement
  inside a PL/pgSQL function failed ("could not determine data type of
  parameter") because PL/pgSQL offers every variable as a parameter, unused ones
  without a type; unused parameters are now sent as NULL text. A parallel worker
  could not plan a statement on a distributed table at all (the catalog cache
  took `GetLatestSnapshot()`), and no statement is forwarded from a parallel
  worker now. A hot standby routes reads but cannot reload a stale map.
- *`kingres_shard.forwarded` (finding 1).* A forwarded session runs a statement
  locally only if every bucket it can touch is provably local (a table
  restricted to one bucket needs that bucket to be local, an unrestricted one
  needs the whole colocation group to be local), else `KS001`. The setting is
  `PGC_BACKEND`: given in the connection options of a forwarded session, not
  changeable in a running one. `kingres_shard.enable_routing` is `PGC_SUSET`.
  This replaces "With `kingres_shard.forwarded` on, a statement that cannot be
  decided is run locally (the origin already decided)".
- *Collations (finding 2).* An equality restricts a statement to one bucket only
  if its input collation is the collation the table was distributed with, and is
  deterministic.
- *Utility statements (finding 3).* A ProcessUtility hook (`utility.c`) refuses,
  on a cluster with more than one shard group, `TRUNCATE`, `ALTER TABLE`/`INDEX`,
  `DROP TABLE`/`INDEX`/`SCHEMA`, `CREATE INDEX`, `COPY FROM`, `CREATE TRIGGER`/
  `RULE`/`POLICY`/`STATISTICS`, renames, `SET SCHEMA` and partition or
  inheritance children on distributed tables; it allows `VACUUM`, `ANALYZE`,
  `REINDEX`, `CLUSTER`, `LOCK`, `GRANT`/`REVOKE` and `COMMENT` (to be repeated on
  every node), and what routing off (superuser) switches off. Whatever the number
  of groups, dropping, renaming or retyping the distribution column and attaching,
  detaching, dropping or creating a bucket partition are refused. The event
  trigger refuses drops that cascade to a distributed table. Dropping a column of
  a distributed table no longer makes the table forget that it is distributed (it
  used to, because the event trigger treated the dropped column as the table).
- *`add_node` and the other catalog changes (finding 4).* Each takes an exclusive
  lock on `kingres.map_version` until the end of its transaction, before it
  reads anything it decides on; `kingres.node` has a unique index on `group_id`
  where `role = 'primary'`.
