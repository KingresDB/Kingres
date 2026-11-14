<!-- SPDX-License-Identifier: PostgreSQL -->
# kingres_shard

`kingres_shard` is the sharding extension of KingresDB. It turns ordinary
tables into hash-distributed tables placed across several PostgreSQL
instances ("nodes", grouped into "shard groups"), and routes a statement that
touches one key to the group that owns it, from any node of the cluster.

This is phase 1, the sharding MVP. The design is in
[`kingres/docs/design/shard-mvp.md`](../../kingres/docs/design/shard-mvp.md)
(including its "Implementation notes and deviations" section); this file
describes what is implemented and how to use it.

## How it works in one page

- A **bucket** is a hash partition. A distributed table is, on every node, an
  ordinary table partitioned `BY HASH (<distribution column>)` with one
  partition `<table>_b<NNNN>` per bucket; a node has only the partitions of the
  buckets its shard group owns. The bucket of a key is
  `compute_partition_hash_value(key) % bucket_count`, the value PostgreSQL
  itself uses to store the row, so routing and storage cannot disagree.
- The **catalog** (`kingres.node`, `colocation`, `dist_table`, `bucket_map`,
  `map_version`, `local_node`) is authoritative on the primary of the *map
  group* (group 0, node 1) and is copied to every other node.
- The **planner hook** looks at each statement. Without distributed tables, or
  on a cluster with one shard group, nothing changes. Otherwise every
  distributed table of the statement must be restricted to one key
  (`dist_column = constant or parameter` in the top-level `AND` of the `WHERE`
  clause or of an inner join's `ON`, or the key of a single-row `INSERT`), all
  keys must be in one colocation group and in one bucket. If the owner is this
  node's group, PostgreSQL's own planner runs; if it is another group, the
  statement is deparsed and run there through a custom scan ("Kingres Remote
  Statement"). Anything else is refused with
  `statements that span shard groups are not supported yet`.
- A forwarded statement runs in a session that the origin opened with
  `kingres_shard.forwarded = on`. Such a session never forwards again and does
  not take the origin's word for the placement either: it runs a statement
  locally only if every bucket the statement can touch is provably on this
  node, and raises `KS001` otherwise (a bucket of another group, a table that
  is not restricted to one bucket, a function body the planner expanded into
  the statement). The originating node treats `KS001` as a stale bucket map: it
  reloads the catalog from the map group and retries once.
- The text of a forwarded statement means the same on the remote node whatever
  the session settings of either node are: it is deparsed with ISO dates,
  postgres-style intervals, shortest-exact floats, standard-conforming strings,
  hex `bytea` and `search_path = pg_catalog`; the remote session is opened with
  the same settings (and again `SET LOCAL` in every remote transaction) and with
  `client_encoding` of the local database; its `TimeZone` follows the `TimeZone`
  of the local session, statement by statement.

## Requirements and installation

- KingresDB (the extension needs `KINGRES_VERSION`, core patch KP-0001; it
  does not compile against stock PostgreSQL by design).
- The extension must be in `shared_preload_libraries` on every node
  (`shared_preload_libraries = 'kingres_shard'`); `CREATE EXTENSION` fails
  otherwise.
- Nodes connect to each other with libpq as the current user to the current
  database, so every node needs a role of that name, and `pg_hba.conf` (and the
  password file, if used) must let the nodes in. Credentials are never stored
  in the catalog.

## Building and testing

Out of tree against an installed server (PGXS):

```
make USE_PGXS=1 PG_CONFIG=/path/to/pg_config
make USE_PGXS=1 PG_CONFIG=/path/to/pg_config install
```

In the source tree the extension builds with the tree's own build system
(`contrib/kingres_shard/meson.build` and `Makefile` list the same sources,
data files and tests; the integrator adds the directory to `contrib/`).

Tests, using the development helper from the repository root (WSL2):

```
kingres/dev/ext.sh kingres_shard all          # sync build install regress tap
PROVE_TESTS=t/002_routing.pl kingres/dev/ext.sh kingres_shard tap
```

- `sql/` and `expected/`: pg_regress tests on one node (`version`, `catalog`,
  `bucket_for`, `distribute`, `routing_single`; `security`: the search_path and
  `SECURITY DEFINER` scan of every function, the temporary-schema attack, the
  routing switches; `utility_guard`: utility statements on a cluster with a
  second, fictitious shard group; `routing_decisions`: collations, functions
  and other nested contexts, forwarded sessions, decided with `EXPLAIN` and
  with refusals, no second server needed).
- `t/`: TAP tests with several nodes (`001_cluster.pl`: node registry,
  catalog equality, bucket placement; `002_routing.pl`: routing, EXPLAIN,
  prepared statements, transactions, limits, stale-map retry;
  `003_hooks.pl`: coexistence with `pg_stat_statements` and `auto_explain`;
  `004_security.pl`: the stale-map reload under a hostile temporary schema,
  forwarded sessions; `005_session_settings.pl`: forwarded statements under
  non-default session settings and another encoding;
  `006_ddl_and_concurrency.pl`: utility statements on a real cluster, the
  cluster lock, two `add_node` calls at once; `007_nested_contexts.pl`:
  functions, `DO`, triggers, `CREATE TABLE AS`, `COPY`, prepared statements,
  parallel workers, a hot standby).

## Quick start

```sql
-- on the first node (it becomes the map group primary, node 1)
CREATE EXTENSION kingres_shard;
SELECT kingres.init_cluster('host1', 5432);       -- the address the other nodes use

-- add nodes (each new primary is a new shard group)
SELECT kingres.add_node('host2', 5432);
SELECT kingres.add_node('host3', 5432);

-- distribute an empty table
CREATE TABLE users (id int, name text, PRIMARY KEY (id));
SELECT kingres.create_distributed_table('users', 'id');

-- from any node
INSERT INTO users VALUES (42, 'ada');
SELECT name FROM users WHERE id = 42;
EXPLAIN SELECT name FROM users WHERE id = 42;     -- shows where it runs
```

The administration functions and the internal helpers can be executed by
superusers only (`EXECUTE` is revoked from `PUBLIC`). If the server user has a
name that is also a schema name, unqualified table names resolve to that
schema through `"$user"`; qualify names or fix `search_path`.

## SQL interface

| Function | Meaning |
|---|---|
| `kingres.version() returns text` | The KingresDB product version. |
| `kingres.init_cluster(host text, port int)` | Register this node as node 1 of group 0. |
| `kingres.add_node(host text, port int, group_id int default null, role text default 'primary') returns int` | Map group primary only. Registers a node (a new group when `group_id` is null), creates the extension there, creates the parents of all existing distributed tables, sets its `local_node`, and copies the catalog. A `standby` is only registered. |
| `kingres.remove_node(node_id int)` | Unregister a node whose group owns no buckets; its catalog is cleared if reachable. |
| `kingres.sync_catalog()` | Push the catalog to every active primary node. |
| `kingres.create_distributed_table(relation regclass, dist_column name, colocate_with regclass default null, bucket_count int default null)` | Map group primary only. See below. |
| `kingres.bucket_for(relation regclass, value anyelement) returns int` | The bucket of a key of a distributed table. |
| `kingres.bucket_for(colocation_id int, value anyelement) returns int` | The bucket of a key in a colocation group. |

Views: `kingres.nodes` (`kingres.node` plus `is_local`) and
`kingres.buckets` (table, bucket, partition name, group, owning node, state,
and the partition size where the partition exists on this node).

`create_distributed_table` requires an empty, ordinary, non-partitioned table
without inheritance, foreign keys, triggers, row-level security, storage
parameters or exclusion constraints, on a type with a default hash operator
class and extended hash function. Every unique constraint or unique index must
include the distribution column; the error names the offending object. The
table is recreated as a hash-partitioned table with the same columns,
defaults, `NOT NULL`, check and unique constraints and indexes, the sequences
owned by its columns, and its owner. The colocation group is the one of
`colocate_with`, else an existing group with the same type, collation and
bucket count, else a new group whose buckets are spread round-robin over the
groups that should hold shards. Privileges, comments and statistics objects
are not copied.

## Settings

| GUC | Default | Meaning |
|---|---|---|
| `kingres_shard.default_bucket_count` | 128 | Bucket count of a new colocation group (1 to 4096). |
| `kingres_shard.enable_routing` | on | When off, the planner and utility hooks do nothing. Superusers only (`PGC_SUSET`): with routing off a `SELECT` reads the local partitions only and a `TRUNCATE` clears this node only, which an administrator who repeats a statement on every node by hand wants and an ordinary session must not be able to switch on. |
| `kingres_shard.forwarded` | off | Set by the router on the sessions it opens; fixed at connection time (`PGC_BACKEND`), so `SET`, `set_config()` and `RESET` in a running session fail. See "The forwarded flag" below. |
| `kingres_shard.node_connect_timeout` | 5s | Connection timeout between nodes. |

## The forwarded flag

`kingres_shard.forwarded` marks a session that another node opened to run one
statement for a bucket of this node. It used to be an ordinary setting that any
client could `SET`, and a forwarded session ran every statement locally, so a
client could turn "refused because it spans shard groups" into a silently
partial answer (review finding 1). Now (1) a forwarded session runs a statement
only if every bucket it can touch is provably local, and raises `KS001`
otherwise, so the flag can only make a statement fail, never make a wrong
answer silent, whoever sets it; and (2) it is `PGC_BACKEND`: it can be given
only in the connection options (`options=-c kingres_shard.forwarded=on`), as
the router does, and cannot be toggled in a running session (an application
that wants a stricter session can still connect with it on). The one setting
that does switch checking off is `kingres_shard.enable_routing`, which is
superuser-only.

## Security notes

- Every function of the extension has `SET search_path = pg_catalog, kingres,
  pg_temp` (the temporary schema last), so nothing a user creates in a schema of
  their own or in their temporary schema is ever resolved by name.
  `pg_regress` test `security` fails if a function lacks it.
- The only `SECURITY DEFINER` function is the `sql_drop` event trigger
  `kingres.forget_dropped_tables()`: whoever may drop a table has no privilege
  on the catalog tables, but the catalog row must go with the table. It
  qualifies every type, cast and operator with `pg_catalog` and takes no
  arguments.
- When a node finds its bucket map stale it reloads the catalog from the map
  group as the bootstrap superuser (the catalog tables belong to the extension
  owner). That reload calls `kingres.internal_apply_catalog()` by OID, parses
  no statement and resolves no name through the session's `search_path`;
  the function pins its own `search_path` and qualifies every name it uses.
- Catalog-changing functions (`init_cluster`, `add_node`, `remove_node`,
  `sync_catalog`, `create_distributed_table`) hold an exclusive lock on
  `kingres.map_version` until the end of their transaction, and the schema
  allows at most one primary per shard group.

## Utility statements

On a cluster with more than one shard group, a statement that would change a
distributed table on this node only is refused with `<statement> is not
supported on distributed tables yet`: `TRUNCATE`, `ALTER TABLE` (and `ALTER
INDEX`), `DROP TABLE`, `DROP INDEX`, `DROP SCHEMA` of a schema that holds one,
`CREATE INDEX`, `COPY ... FROM`, `CREATE TRIGGER`, `CREATE RULE`, `CREATE
POLICY`, `CREATE STATISTICS`, renaming or moving, attaching or creating a
partition or an inheritance child. Drops that cascade (`DROP SCHEMA ...
CASCADE`, `DROP TYPE ... CASCADE`) are stopped by the `sql_drop` event
trigger. Allowed: `VACUUM`, `ANALYZE`, `REINDEX`, `CLUSTER`, `LOCK`,
`EXPLAIN`, `GRANT`, `REVOKE`, `COMMENT` (privileges and comments are not
copied between nodes; repeat them on every node), and statements on other
tables. A superuser who repeats a statement on every node by hand sets
`kingres_shard.enable_routing = off` for it. Propagating DDL is a later phase.
With one shard group every statement works as before, except dropping,
renaming or retyping the distribution column and attaching, detaching,
dropping or creating a bucket partition, which are refused whatever the number
of groups.

## Limitations in phase 1

Read these first; each is a reviewed boundary, not an accident.

- **Catalog synchronization is not atomic.** Remote catalog changes are made
  in remote transactions that are committed one after the other in the
  pre-commit callback of the local transaction. A crash or error between two
  remote commits can leave a node with a stale catalog; `kingres.sync_catalog()`
  repairs it. Phase 2 replaces this with two-phase commit. The same holds for
  the remote DDL of `create_distributed_table` and `add_node`.
- **No statements that span shard groups.** A statement needs a key for every
  distributed table; no scatter-gather, no cross-group joins, no multi-row
  `INSERT`, no `INSERT ... SELECT`. A `WHERE id IN (...)` or `OR` of several
  keys is refused unless all keys are in one bucket.
- **Writes**: a transaction may write on at most one remote shard group and may
  not write both locally and remotely; the second writer gets a
  "not supported yet" error. Reads from several groups in one transaction are
  allowed (there is no global snapshot). `PREPARE TRANSACTION` after remote
  work is refused.
- **No DDL propagation** after table creation: on a cluster with several
  shard groups `ALTER TABLE`, `CREATE INDEX`, `TRUNCATE`, `DROP TABLE`, `COPY
  FROM` and the like are refused (see "Utility statements"). There is no `COPY`
  routing, no reference tables, no bucket movement or rebalance, no failover
  handling.
- **Functions.** A statement inside a PL/pgSQL function, a `DO` block, an
  `EXECUTE`, a trigger, a rule or a cursor is routed like any other statement.
  A SQL-language function that the planner expands into the calling statement
  (a set-returning function with a one-statement body, for example) reads its
  tables after the routing decision was made, so a statement that reaches a
  distributed table through one is refused ("through a SQL-language function
  are not supported yet"); write the function in PL/pgSQL. A statement for
  another shard group cannot be forwarded from a parallel worker (mark such
  functions `PARALLEL RESTRICTED`). A hot standby routes reads like its primary
  but cannot reload a stale bucket map.
- **Collations.** A comparison restricts a statement to one bucket only if it is
  made under the collation the table was distributed with (and that collation
  is deterministic); `WHERE name = 'x' COLLATE "C"` on a column of the default
  collation is "not restricted to one key" and is refused.
- **Session settings.** `search_path` of the remote session is `pg_catalog`
  (as `postgres_fdw` does): a function without its own `SET search_path` that
  runs on the remote node resolves unqualified names there. Types whose text
  form depends on `lc_monetary` or `lc_numeric` (`money`) are not portable
  between sessions with different settings, and functions such as `now()` are
  evaluated on the node that runs the statement.
- **Statements that combine a remote distributed table with local tables**,
  `MERGE`, and scrollable cursors on remote statements are refused.
- **Generic plans** cannot route. With `plan_cache_mode = auto` the plan cache
  keeps using custom plans for such statements (the generic plan gets a huge
  cost); `force_generic_plan` fails with a clear error. A SQL-language function
  whose body uses a distributed table with parameters hits the same rule.
- **Stale-map retry** happens once, and only when the remote transaction has
  run no earlier statement (so it can be rolled back to just before). Inside a
  longer transaction the `KS001` error reaches the client. A node whose map
  wrongly says that the bucket is local cannot notice it by itself. The map
  group primary cannot repair a stale remote node by retrying; run
  `kingres.sync_catalog()`.
- Dropping a distributed table (possible with one shard group, or by hand with
  routing off) removes its catalog row on that node only.
- Roles, schemas created for tables, and user-defined types must already be
  compatible on every node; they are not synchronized.
- Cached connections are per backend and are replaced when found closed; there
  is no pool shared between backends.
- `dist_collation` is an OID; non-default, non-deterministic collations are
  not portable between nodes.
