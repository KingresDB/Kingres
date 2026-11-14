<!-- SPDX-License-Identifier: PostgreSQL -->
# kingres_shard

`kingres_shard` is the sharding extension of KingresDB. It turns ordinary
tables into hash-distributed tables placed across several PostgreSQL
instances ("nodes", grouped into "shard groups"), keeps small tables on every
node as reference tables, and makes every statement correct on a cluster with
several groups: a statement that touches one key goes to the group that owns
it, and a statement that spans groups is answered by reading the other groups.

This is version 0.2 (phase 2a, "correct"). Phase 2b will make the common
statements fast. The designs are in
[`kingres/docs/design/shard-mvp.md`](../../kingres/docs/design/shard-mvp.md)
(phase 1) and
[`kingres/docs/design/shard-phase2a.md`](../../kingres/docs/design/shard-phase2a.md)
(this version, with its "Implementation notes and deviations"); this file
describes what is implemented and how to use it.

## How it works in one page

- A **bucket** is a hash partition. A distributed table is, on every node, an
  ordinary table partitioned `BY HASH (<distribution column>)` with one
  partition `<table>_b<NNNN>` per bucket; a node has only the partitions of the
  buckets its shard group owns. The bucket of a key is
  `compute_partition_hash_value(key) % bucket_count`, the value PostgreSQL
  itself uses to store the row, so routing and storage cannot disagree.
- A **reference table** is an ordinary table that every node holds in full.
  Reads are local; writes run on every node in one distributed transaction.
- The **catalog** (`kingres.node`, `colocation`, `dist_table`, `bucket_map`,
  `map_version`, `local_node`) is authoritative on the primary of the *map
  group* (group 0, node 1) and is copied to every other node. Changes to it are
  made on all nodes in one two-phase transaction. Two node-local tables are not
  copied: `kingres.dtx_log` (commit decisions) and `kingres.pending_node`.
- The **planner hook** looks at each statement. Without distributed tables, or
  on a cluster with one shard group, nothing changes. Otherwise:
  - every distributed table restricted to one key (`dist_column = constant or
    parameter` in the top-level `AND` of the `WHERE` clause or of an inner
    join's `ON`, or the key of a single-row `INSERT`), all keys in one colocation
    group and one bucket: the statement belongs to the group that owns the
    bucket. If that is this node's group, PostgreSQL's planner runs; otherwise
    the statement is deparsed and run there through a custom scan ("Kingres
    Remote Statement");
  - a `SELECT` that spans groups is planned by PostgreSQL as if all the data were
    here, with each distributed table read by a "Kingres Distributed Scan" (the
    local buckets plus one fragment per other group, fetched in batches, all
    restrictions rechecked here), so every query shape is correct;
  - an `UPDATE` or `DELETE` of one distributed table that spans groups and can be
    shipped whole is run on every group that owns buckets of the table;
  - a write to a reference table is run on every node;
  - anything else (a multi-row `INSERT` or `INSERT ... SELECT` whose rows belong
    to several groups, an `UPDATE` or `DELETE` that cannot be shipped) is
    refused with a "not supported yet" error that says what to change.
- A forwarded statement runs in a session that the origin opened with
  `kingres_shard.forwarded = on`. Such a session never forwards again and does
  not take the origin's word for the placement either: it runs a statement
  locally only if every bucket the statement can touch is provably on this
  node, and raises `KS001` otherwise, except for the two statement forms that
  the extension itself sends to every group and marks with a comment (a scan
  fragment and the group's part of a fanned-out write; see "The forwarded
  flag"). The originating node treats `KS001` as a stale bucket map: it reloads
  the catalog from the map group and retries once.
- The text of a statement sent to another node means the same there whatever
  the session settings of either node are: it is deparsed with ISO dates,
  postgres-style intervals, shortest-exact floats, standard-conforming strings,
  hex `bytea` and `search_path = pg_catalog`; the remote session is opened with
  the same settings (and again `SET LOCAL` in every remote transaction) and with
  `client_encoding` of the local database; its `TimeZone` follows the `TimeZone`
  of the local session, statement by statement. DDL text is sent as written,
  with the `search_path` and literal settings of the session.

## Guarantees

- **Atomic commit across nodes (two-phase commit).** A transaction that writes
  on two or more nodes (each remote connection that ran a writing statement,
  and the local node when its transaction has an id) commits with two-phase
  commit: every remote writer is prepared (`PREPARE TRANSACTION`), the decision
  is written to the node-local `kingres.dtx_log` by the committing transaction
  itself, and after the local commit the remote ones are committed. A crash at
  any point is resolved by `kingres.recover_prepared_transactions()` and by the
  recovery background worker: a prepared transaction whose origin did not commit
  is rolled back, one whose decision is in `dtx_log` is committed. Every node
  needs `max_prepared_transactions` greater than zero (the library warns when it
  loads with zero; a prepare that fails for that reason says so).
- **Atomic catalog changes and DDL.** `sync_catalog`, `add_node`,
  `remove_node`, `create_distributed_table`, `create_reference_table` and the
  propagated DDL change all nodes in one such transaction, under one
  cluster-wide lock on `kingres.map_version`.
- **Atomicity is not a global snapshot.** Two-phase commit makes a transaction
  commit on all nodes or on none, but each node commits at its own moment. A
  reader on another node can see the effects of a transaction on one group
  before it sees them on another (for example a `SELECT` through the
  distributed scan that reads group 1 just after the commit and group 2 just
  before it), and the distributed scan reads each group in the snapshot of that
  group's transaction. This is a *read skew* anomaly; it cannot create data
  that was never committed, and it disappears when a global snapshot exists
  (a later phase). `SERIALIZABLE` is serializable per node only.
- **No distributed deadlock detection.** Writes that fan out to several nodes
  take their row locks in the same order on every node (nodes in id order), so
  two such statements do not deadlock each other; two transactions that lock in
  opposite orders across nodes wait for each other until a lock timeout
  (distributed deadlock detection is a later phase). Set `lock_timeout`.

## Requirements and installation

- KingresDB (the extension needs `KINGRES_VERSION`, core patch KP-0001; it
  does not compile against stock PostgreSQL by design).
- On every node: `shared_preload_libraries = 'kingres_shard'` (the library
  installs hooks and a background worker; `CREATE EXTENSION` fails otherwise)
  and `max_prepared_transactions` > 0 (at least the number of concurrent
  multi-node transactions).
- Nodes connect to each other with libpq as the current user to the current
  database, so every node needs a role of that name, and `pg_hba.conf` (and the
  password file, if used) must let the nodes in. Credentials are never stored
  in the catalog. SSL options are set with `kingres_shard.node_conninfo`.
- `CREATE EXTENSION kingres_shard` installs version 0.2;
  `ALTER EXTENSION kingres_shard UPDATE` upgrades a 0.1 installation (the
  catalog is identical to a fresh 0.2 one; the `upgrade` regression test
  compares the two). `shared_preload_libraries` must hold the new library first.

## Building and testing

Out of tree against an installed server (PGXS):

```
make USE_PGXS=1 PG_CONFIG=/path/to/pg_config
make USE_PGXS=1 PG_CONFIG=/path/to/pg_config install
```

In the source tree the extension builds with the tree's own build system
(`contrib/kingres_shard/meson.build` and `Makefile` list the same sources,
data files and tests).

Tests, using the development helper from the repository root (WSL2):

```
kingres/dev/ext.sh kingres_shard all          # sync build install regress tap
PROVE_TESTS=t/013_dist_scan.pl kingres/dev/ext.sh kingres_shard tap
```

- `sql/` and `expected/`: pg_regress tests on one node (`version`, `catalog`,
  `bucket_for`, `distribute`, `routing_single`; `security`: the search_path and
  `SECURITY DEFINER` scan of every function, the temporary-schema attack, the
  routing switches; `utility_guard`: utility statements on a cluster with a
  second, fictitious shard group; `routing_decisions`: collations, functions
  and other nested contexts, forwarded sessions, decided with `EXPLAIN`;
  `reference`: reference tables on one node; `upgrade`: `ALTER EXTENSION ...
  UPDATE` from 0.1 against a fresh 0.2 install).
- `t/`: TAP tests with several nodes. Phase 1: `001_cluster`, `002_routing`,
  `003_hooks`, `004_security`, `005_session_settings`,
  `006_ddl_and_concurrency`, `007_nested_contexts`. Phase 2a: `008_twophase`
  (commit, rollback, prepare failure, every crash window with fault
  injection), `009_recovery_worker` (the same through the background worker, a
  reload, a standby), `010_atomic_catalog`, `011_reference`,
  `012_ddl_propagation`, `013_dist_scan` (the comparison with a single node),
  `014_dist_write` (the same for `UPDATE` and `DELETE`), `015_node_conninfo`.
  `t/KingresShardTest.pm` builds clusters; `t/KingresShardCompare.pm` is the
  comparison harness: it loads the same data into a three-node cluster and a
  single node and compares query results. Later phases add queries to the list
  in `013_dist_scan.pl`.

## Quick start

```sql
-- on the first node (it becomes the map group primary, node 1)
CREATE EXTENSION kingres_shard;
SELECT kingres.init_cluster('host1', 5432);       -- the address the other nodes use

-- add nodes (each new primary is a new shard group)
SELECT kingres.add_node('host2', 5432);
SELECT kingres.add_node('host3', 5432);

-- distribute an empty table, and keep a small one everywhere
CREATE TABLE users (id int, name text, PRIMARY KEY (id));
SELECT kingres.create_distributed_table('users', 'id');
CREATE TABLE countries (code text PRIMARY KEY, name text);
SELECT kingres.create_reference_table('countries');

-- from any node
INSERT INTO users VALUES (42, 'ada');
SELECT name FROM users WHERE id = 42;
SELECT count(*) FROM users;                       -- reads every group
EXPLAIN SELECT name FROM users WHERE id = 42;     -- shows where it runs

-- DDL goes through the map group primary (node 1) and reaches every node
ALTER TABLE users ADD COLUMN email text;
CREATE INDEX users_name_idx ON users (name);
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
| `kingres.add_node(host text, port int, group_id int default null, role text default 'primary') returns int` | Map group primary only. Registers a node (a new group when `group_id` is null), creates the extension there, creates the distributed and reference tables (with the rows of the reference tables and the foreign keys), sets its `local_node`, and copies the catalog, all in one two-phase transaction. A `standby` is only registered. |
| `kingres.remove_node(node_id int)` | Unregister a node whose group owns no buckets; its catalog is cleared if reachable. |
| `kingres.sync_catalog()` | Push the catalog to every active primary node, atomically. |
| `kingres.create_distributed_table(relation regclass, dist_column name, colocate_with regclass default null, bucket_count int default null)` | Map group primary only. See below. |
| `kingres.create_reference_table(relation regclass)` | Map group primary only. Makes an ordinary table a reference table: creates it on every node and copies its rows. Writers are blocked while it copies. |
| `kingres.recover_prepared_transactions() returns int` | Commits or rolls back the prepared transactions this node originated, on every active primary (itself included); returns how many it resolved. Superuser. Runs in a `READ COMMITTED` transaction; not on a standby. |
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
are not copied (grant after creating; the grant is propagated).

`create_reference_table` takes the same kind of table (ordinary, permanent,
without inheritance, foreign keys, triggers, row-level security, storage
parameters or exclusion constraints; a unique constraint needs no distribution
column). Add foreign keys afterwards with `ALTER TABLE`.

## Settings

| GUC | Default | Meaning |
|---|---|---|
| `kingres_shard.default_bucket_count` | 128 | Bucket count of a new colocation group (1 to 4096). |
| `kingres_shard.enable_routing` | on | When off, the planner and utility hooks do nothing. Superusers only (`PGC_SUSET`): with routing off a `SELECT` reads the local partitions only and a `TRUNCATE` clears this node only, which an administrator who repeats a statement on every node by hand wants and an ordinary session must not be able to switch on. |
| `kingres_shard.enable_distributed_scan` | on | When off, a `SELECT` that spans shard groups raises the phase 1 error instead of reading the other groups (to forbid slow cross-group queries). |
| `kingres_shard.scan_fetch_size` | 1000 | Rows requested from another group at a time by the distributed scan. |
| `kingres_shard.recovery_interval` | 30s | Seconds between runs of the recovery worker; 0 disables it. `sighup`. |
| `kingres_shard.node_conninfo` | empty | Connection options appended to every node-to-node connection, see below. Superuser only. |
| `kingres_shard.node_connect_timeout` | 5s | Connection timeout between nodes. |
| `kingres_shard.forwarded` | off | Set by the router on the sessions it opens; fixed at connection time (`PGC_BACKEND`), so `SET`, `set_config()` and `RESET` in a running session fail. See "The forwarded flag" below. |
| `kingres_shard.fault_injection` | empty | Developer setting, superuser only: fails the commit protocol at a named point (`<point>:kill`, `:error`, `:wait:<file>`; points `before_prepare`, `after_first_prepare`, `after_prepare`, `after_log`, `after_commit`, `between_commit_prepared`). Used by the tests; it adds no capability a superuser does not have. |

### Node connection options

`kingres_shard.node_conninfo` is a libpq connection string appended to every
node-to-node connection (including those of the recovery worker): use it for
`sslmode`, `sslcert`, `sslkey`, `sslrootcert`, `sslcrl`, `gssencmode` and the
like. It is validated when it is set: the keywords the extension controls
(`host`, `hostaddr`, `port`, `dbname`, `user`, `password`, `options`,
`service`, `client_encoding`, `application_name`, `connect_timeout`,
`replication`) and strings that are not connection strings are rejected, and
the messages never repeat the value (an invalid value is replaced by a
placeholder, because the generic message of the server would print it). Only
superusers can set or read it. The text of a connection error has the values of
`sslcert`, `sslkey`, `sslrootcert`, `sslcrl`, `sslcrldir`, `sslpassword`,
`sslkeylogfile` and `passfile` replaced by `<hidden>`. A value in
`postgresql.conf` is logged by the server in full when the file is reloaded (the
generic "parameter changed" line); keep secrets out of the value itself and use
files that only the server can read. Passwords come from the password file, not
from this setting. A change applies to connections opened afterwards; a cached
connection keeps the options it was opened with.

## The forwarded flag

`kingres_shard.forwarded` marks a session that another node opened to run one
statement for a bucket of this node. It used to be an ordinary setting that any
client could `SET`, and a forwarded session ran every statement locally, so a
client could turn "refused because it spans shard groups" into a silently
partial answer (review finding 1). Now (1) a forwarded session runs a statement
only if every bucket it can touch is provably local, and raises `KS001`
otherwise; and (2) it is `PGC_BACKEND`: it can be given only in the connection
options (`options=-c kingres_shard.forwarded=on`), as the router does, and
cannot be toggled in a running session. The setting that does switch checking
off is `kingres_shard.enable_routing`, which is superuser-only.

Phase 2a adds three uses of a forwarded session, each limited:

- a **scan fragment** (`/* kingres:scan-fragment */ DECLARE ... CURSOR FOR SELECT
  <columns> FROM <table> WHERE <shippable restrictions>`): a plain read of one
  distributed table, run on the buckets of this node;
- the group's part of a **fanned-out `UPDATE` or `DELETE`** (marked
  `/* kingres:write-fragment */`): shippable (built-in immutable functions, one
  distributed table, reference tables) and run on the buckets of this node;
- **propagated DDL** (no marker): the supported statements on distributed and
  reference tables, which are not refused on a node that is not the map group
  primary when they arrive in a forwarded session.

Anyone can open such a session. What they can do in it is limited to statements
the extension itself sends, checked against their own privileges; they cannot
obtain rows they could not read, but they can see the rows of this node only
(scan fragment), write this node's rows only (write fragment) or change a table
on this node only (DDL), which are inconsistencies the owner of a table can
also create with `enable_routing = off` as a superuser. A statement without a
marker that is not provably local is still `KS001`.

## Security notes

- Every function of the extension has `SET search_path = pg_catalog, kingres,
  pg_temp` (the temporary schema last), so nothing a user creates in a schema of
  their own or in their temporary schema is ever resolved by name.
  `pg_regress` test `security` fails if a function lacks it.
- The only `SECURITY DEFINER` function is the `sql_drop` event trigger
  `kingres.forget_dropped_tables()`: whoever may drop a table has no privilege
  on the catalog tables, but the catalog row must go with the table. It
  qualifies every type, cast and operator with `pg_catalog` and takes no
  arguments. It lets a drop of a distributed table through only while the
  utility hook runs the statement itself (`kingres.internal_ddl_authorized()`).
- When a node finds its bucket map stale it reloads the catalog from the map
  group as the bootstrap superuser (the catalog tables belong to the extension
  owner). That reload calls `kingres.internal_apply_catalog()` by OID, parses
  no statement and resolves no name through the session's `search_path`;
  the function pins its own `search_path` and qualifies every name it uses.
- Catalog-changing functions hold an exclusive lock on `kingres.map_version`
  until the end of their transaction, and the schema allows at most one primary
  per shard group.
- Everything sent to another node is deparsed under the transmission modes
  (data), or sent with the session's own settings re-applied for the duration of
  the statement (DDL); the commit protocol writes `dtx_log` with the heap API
  (the user needs no privilege on it); recovery runs as the role that calls it
  (superuser) or as the bootstrap superuser in the worker.
- `dtx_log` and `pending_node` are readable by their owner only.

## DDL on distributed and reference tables

On a cluster with more than one shard group, a utility statement that names a
distributed or reference table is handled by the ProcessUtility hook:

- **Propagated** (run on the map group primary, then on every other node in the
  same two-phase transaction, with the statement text as written and the
  session's `search_path` and literal settings): `ALTER TABLE` with `ADD
  COLUMN` (a constant default only; no `serial`, no identity), `DROP COLUMN`
  (not the distribution column, not `CASCADE`), `ALTER COLUMN ... SET/DROP
  DEFAULT`, `SET/DROP NOT NULL`, `ADD CONSTRAINT` (check, unique or primary key
  that includes the distribution column, foreign key from a distributed table to
  a reference table or to a colocated distributed table on the distribution
  column, or between reference tables; no exclusion, no `USING INDEX`), `DROP
  CONSTRAINT`, `VALIDATE CONSTRAINT`, `OWNER TO`; `RENAME COLUMN` (not the
  distribution column) and `RENAME TO` (the bucket partitions are renamed on
  every node); `CREATE INDEX` and `DROP INDEX` (not `CONCURRENTLY`); `DROP
  TABLE`; `TRUNCATE`; `GRANT` and `REVOKE` on named tables; `COMMENT` on the
  table and its columns.
- **Local** (they act on one node by nature): `VACUUM`, `ANALYZE`, `REINDEX`,
  `LOCK`, `EXPLAIN`.
- **Refused** with `<statement> is not supported on distributed tables yet`:
  everything else, among them `ALTER COLUMN ... TYPE`, `SET SCHEMA`, `ATTACH`
  and `DETACH PARTITION`, triggers, rules, policies, statistics objects,
  `CLUSTER`, `CREATE INDEX CONCURRENTLY`, `COPY ... FROM`, row-level security,
  storage parameters, `ALTER INDEX`.
- A statement that mixes distributed and other tables (`DROP TABLE a, b`,
  `TRUNCATE a, b`), a query string with several statements, and a `GRANT ... ON
  ALL TABLES IN SCHEMA` that reaches a distributed table are refused.
- On a node that is not the map group primary the supported statements are
  refused with a hint that names the map group primary. A foreign key from an
  ordinary table to a distributed table is refused.
- A bucket partition is not a table of the user: renaming, dropping, attaching,
  detaching, creating one, or changing the distribution column, is refused
  whatever the number of groups.
- Drops that cascade (`DROP SCHEMA ... CASCADE`, `DROP TYPE ... CASCADE`) are
  stopped by the `sql_drop` event trigger.
- Roles, schemas created for tables, and user-defined types are not
  synchronized: `ALTER TABLE ... OWNER TO r` and `GRANT ... TO r` fail (and are
  rolled back everywhere) if `r` does not exist on every node.
- A superuser who repeats a statement on every node by hand sets
  `kingres_shard.enable_routing = off` for it. With one shard group every
  statement runs locally as before.

## Reference tables

Reads are local on every node. A write (`INSERT`, `UPDATE`, `DELETE`, also with
`ON CONFLICT` and `RETURNING`) runs on every active primary node, node after
node in node-id order, in the transaction's two-phase commit; the rows returned
and the count are those of the local copy, and a statement that affects a
different number of rows on two nodes (the copies differ) fails. Because every
node must produce the same rows, a write is accepted only if every expression in
it is a built-in immutable function or operator on built-in types: `now()`,
`random()`, `nextval()` (a `serial` default) and user-defined functions are
rejected ("cannot be run on every node"), and it may read only reference tables.
A statement that writes a reference table and reads a distributed table is not
supported yet. A foreign key from a distributed table to a reference table is
allowed (a new node receives it); the reverse is refused. `COPY ... FROM` into a
reference table is refused on a cluster with several groups.

## The distributed scan

A `SELECT` that spans groups (and a write to a local table that reads such a
table) is planned by PostgreSQL with every distributed table replaced by one
path, `Custom Scan (Kingres Distributed Scan)`: the best local plan of the
table's partitions, plus one cursor per other group that owns buckets,
`DECLARE ... FOR SELECT <columns needed> FROM <table> WHERE <shippable
restrictions>`, read in batches of `kingres_shard.scan_fetch_size` rows with
the next batch of every group requested before the current one is consumed.
Restrictions are shipped only if built in, immutable, on built-in types and the
default or `C` collation, columns of that table, constants and external
parameters; every restriction is applied again here to the rows fetched. The
scan is not parallel-aware and supports rescan (it runs the remote queries
again). Partitionwise join and aggregation are switched off while such a
statement is planned. `EXPLAIN` shows the number of remote groups and the
remote SQL. Not supported yet: row locking (`FOR UPDATE`) on a statement that
spans groups, system columns of a distributed table (`ctid`, `tableoid`).

## UPDATE and DELETE that span shard groups

An `UPDATE` or `DELETE` that is not restricted to one bucket runs on every group
that owns buckets of the table (in group order, the local one through SPI), in
one two-phase transaction, and the counts and `RETURNING` rows are added, if the
statement can be shipped whole: one distributed table (as the target only, no
subquery or join on it or on another distributed table), reference tables
allowed, only built-in immutable functions and operators. An `UPDATE` that
assigns the distribution column is refused. Otherwise it fails with `this
UPDATE/DELETE spans shard groups and cannot be shipped`.

## Limitations in phase 2a

Read these first; each is a reviewed boundary, not an accident.

- **No global snapshot** (see Guarantees). The distributed scan is slow for
  large tables by design: it moves rows, it pushes down restrictions only
  (aggregates, sorts, limits and colocated joins are phase 2b).
- **Writes that span groups.** A multi-row `INSERT` or `INSERT ... SELECT` whose
  rows belong to several groups, `COPY ... FROM`, `MERGE` and an `UPDATE` or
  `DELETE` that cannot be shipped are refused. A write that uses a function
  that is not built in cannot be shipped.
- **Functions.** A statement inside a PL/pgSQL function, a `DO` block, an
  `EXECUTE`, a trigger, a rule or a cursor is routed like any other statement;
  a SQL-language function expanded into the statement is read through the
  distributed scan. A statement for another shard group cannot be forwarded from
  a parallel worker (mark such functions `PARALLEL RESTRICTED`). A hot standby
  routes and scans like its primary but cannot reload a stale bucket map.
- **Collations.** A comparison restricts a statement to one bucket only if it is
  made under the collation the table was distributed with (and that collation
  is deterministic); a comparison under another collation is evaluated here by
  the distributed scan (it is not shipped). The nodes must agree on the
  default collation and its library version: shipped comparisons of the default
  collation rely on it.
- **Session settings.** `search_path` of the remote session is `pg_catalog`
  (as `postgres_fdw` does) for data statements; a function without its own `SET
  search_path` that runs on the remote node resolves unqualified names there.
  Types whose text form depends on `lc_monetary` or `lc_numeric` (`money`) are
  not portable between sessions with different settings.
- **Generic plans** cannot route a statement restricted by a parameter. With
  `plan_cache_mode = auto` the plan cache keeps using custom plans for such
  statements; `force_generic_plan` fails with a clear error. The distributed
  scan does not depend on parameter values and works with generic plans.
- **Stale maps.** The stale-map retry (`KS001`) happens once, and only when the
  remote transaction has run no earlier statement. The distributed scan asks the
  groups the node's current map names; buckets do not move in this phase.
- **Standby of a shard group.** A standby is registered, not used for reads of
  other nodes; failover handling is a later phase.
- Roles, schemas created for tables, and user-defined types must already be
  compatible on every node; they are not synchronized. Privileges are copied
  only by propagated `GRANT`.
- A reference-table write to a group that is down fails (the write needs every
  node); reads are unaffected.
- Cached connections are per backend and are replaced when found closed; there
  is no pool shared between backends.
- `dist_collation` is an OID; non-default, non-deterministic collations are
  not portable between nodes.
