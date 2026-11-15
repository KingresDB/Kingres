<!-- SPDX-License-Identifier: AGPL-3.0-only -->
# kingres_shard

`kingres_shard` is the sharding extension of KingresDB. It turns ordinary
tables into hash-distributed tables placed across several PostgreSQL
instances ("nodes", grouped into "shard groups"), keeps small tables on every
node as reference tables, and makes every statement correct on a cluster with
several groups: a statement that touches one key goes to the group that owns
it, and a statement that spans groups is answered by reading the other groups.

This is version 0.5 (0.3 added moving buckets, phase 2a made every statement correct, 0.4 is the fix round of the reviews of 0.2 and 0.3, 0.5 is high availability: standby nodes, `promote_node`, prepared transactions across a failover, distributed deadlock detection and coordinated restore points; see "High availability" below). Phase 2b will make the common
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
  itself, and after the local commit the remote ones are committed. The local
  commit is always flushed before the first `COMMIT PREPARED` is sent, whatever
  `synchronous_commit` says: the extension forces it for these transactions
  (`ForceSyncCommit()`), and a session with `synchronous_commit` of `off` or
  `local` has it raised to `on` for such a transaction, so that the commit also
  waits for the synchronous standbys of the origin and the decision survives a
  failover (S3-15; `remote_write` and above are kept as they are). A participant that
  does not answer `COMMIT PREPARED` within `kingres_shard.node_timeout` is left
  to recovery with a warning and the later participants are not delayed. A crash at
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
  before it). It is worse than skew between groups: in `READ COMMITTED` every
  scan node of a statement (every `DECLARE` of the distributed scan) has its own
  snapshot in its remote transaction, so one statement that scans two
  distributed tables sees the *same node* in two different states, and a
  transaction that is atomic on one node looks torn. Reproduced: a local
  transaction on node 2 updates row 6 of `t1` and `t2` together; a reader on
  node 1 running `select (select sum(a) from t1), pg_sleep(4), (select sum(z)
  from t2)` got `t1` before and `t2` after the transaction (`1545 | 27733.33`
  where the states are `1545 | 26733.33` before and `2545 | 27733.33` after).
  The local buckets are read with the statement snapshot taken at the start,
  the remote ones with the snapshot of the later `DECLARE`. It cannot invent
  data that was never committed (every row read is committed), and it
  disappears when a global snapshot exists (a later phase). Read statements
  that need one state of several tables from one node should read them in one
  scan node (a join of colocated tables on the key is routed to one group).
  `SERIALIZABLE` is serializable per node only.
- **Distributed deadlocks are detected (0.5).** Writes that fan out to several
  nodes take their row locks in the same order on every node (nodes in id
  order), so two such statements do not deadlock each other. Two transactions
  that lock in opposite orders across nodes form a cycle that no single node
  sees: the recovery worker of the map group primary finds it (a pass every
  `kingres_shard.deadlock_check_interval` seconds), confirms it by reading the
  waits twice, and cancels the youngest transaction of the cycle with SQLSTATE
  `40P01` and the message "distributed deadlock detected". See "Distributed
  deadlock detection" below for what is guaranteed and what is not (waits that
  are not lock waits are not seen; set `lock_timeout` for them).
- **Failover.** A distributed transaction commits or aborts on every node also
  across a failover of a group that has a synchronous standby; with an
  asynchronous standby a transaction can be half committed, and the extension
  says so afterwards. See "Prepared transactions across a failover".

## Requirements and installation

- KingresDB (the extension needs `KINGRES_VERSION`, core patch KP-0001; it
  does not compile against stock PostgreSQL by design), built on **PostgreSQL 18 or
  19**. One source builds against both (ADR 0023): the differences between the majors
  are in `src/pg_compat.h`, the SQL scripts are the same, and the tests run on both.
- **One PostgreSQL major per cluster.** `add_node()` refuses a node whose major differs
  from the one of the node that adds it (`object_not_in_prerequisite_state`),
  `move_bucket()` refuses to move a bucket between two nodes of different majors, and
  `check_cluster()` reports a node of another major as `server_major_differs`. To go
  from 18 to 19 build a new cluster of 19 nodes and move the data; mixing majors in
  one cluster is not supported.
- On every node: `shared_preload_libraries = 'kingres_shard'` (the library
  installs hooks and a background worker; `CREATE EXTENSION` fails otherwise)
  and `max_prepared_transactions` > 0 (at least the number of concurrent
  multi-node transactions).
- Nodes connect to each other with libpq as the current user to the current
  database, so every node needs a role of that name, and `pg_hba.conf` (and the
  password file, if used) must let the nodes in. Credentials are never stored
  in the catalog. SSL options are set with `kingres_shard.node_conninfo`.
- `CREATE EXTENSION kingres_shard` installs version 0.5;
  `ALTER EXTENSION kingres_shard UPDATE` upgrades a 0.1, 0.2, 0.3 or 0.4 installation (the
  catalog is identical to a fresh 0.5 one; the `upgrade` regression test
  compares the two). Transactions that 0.4 prepared and that are pending at the upgrade
  are resolved in the old identifier format (see "Prepared transactions across a
  failover").
- A group's primary needs `wal_level` of `replica` or higher to have standbys and for
  `create_restore_point()`. `shared_preload_libraries` must hold the new library first.
- **Upgrade order** (review finding S4-05). A node whose library is new and whose
  SQL objects are not yet updated cannot answer the helper functions that the
  other nodes call, and the library says so ("node ... runs an older version of
  the kingres_shard SQL objects") instead of "function does not exist". Distributed
  scans and fan-out writes of the whole cluster fail from the moment a node
  restarts with the new library until every node has run `ALTER EXTENSION
  kingres_shard UPDATE`; single-key reads and writes keep working. So: install the
  new files on every node, restart them one after the other, run `ALTER EXTENSION
  ... UPDATE` on every node at once and soon after, and do not start a bucket move
  before it is done: `move_bucket()` refuses unless every active primary reports
  extension version 0.5, and names the node that does not.

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
  UPDATE` from 0.1, 0.2, 0.3 and 0.4 against a fresh 0.5 install; the same on both majors:
  the script of 0.3 names a column of `pg_stat_subscription_stats` that PostgreSQL 19
  renamed, so on 19 it creates `kingres.internal_move_subscription_status()` with its
  body check off and `ALTER EXTENSION ... UPDATE` to 0.4 replaces it with a body that
  reads the counter on both majors; 0.3 itself is not usable on 19).
- `t/`: TAP tests with several nodes. Phase 1: `001_cluster`, `002_routing`,
  `003_hooks`, `004_security`, `005_session_settings`,
  `006_ddl_and_concurrency`, `007_nested_contexts`. Phase 2a: `008_twophase`
  (commit, rollback, prepare failure, every crash window with fault
  injection), `009_recovery_worker` (the same through the background worker, a
  reload, a standby), `010_atomic_catalog`, `011_reference`,
  `012_ddl_propagation`, `013_dist_scan` (the comparison with a single node),
  `014_dist_write` (the same for `UPDATE` and `DELETE`), `015_node_conninfo`.
  Fix round for the review of 2026-11-14: `016_recovery_ordering` (recovery
  with an old snapshot, recovery against concurrent commits, durability under
  `synchronous_commit = off`), `017_settings_compare` (restrictions under
  non-default session settings), `018_reference_writes` (layout-dependent
  statements, concurrent writers), `019_forwarded_auth` (forged forwarded
  sessions, the token and the secret), `020_parallel_and_add_node`,
  `021_owner_and_views`, `022_timeouts_and_worker`, `023_scoped_bypass`,
  `024_low_findings`.
  Phase 4 (moving buckets): `025_move_bucket`, `026_move_vs_scan`,
  `027_move_under_load`, `028_move_faults`, `029_check_cluster`, `030_rebalance`,
  `031_move_scan_interleavings`, `032_move_scan_stress`.
  Fix round for the reviews of 0.2 and 0.3 (version 0.4): `033_move_target_window`
  (the routed statement on a target that is not the map group primary),
  `034_move_direct_write` (a write straight into a bucket partition during the
  switch), `035_move_switch_stall` (the switch does not queue, bounded retries),
  `036_move_worker` (the recovery worker resumes and cleans up, version handshake,
  grants), `037_move_transactions` (COPY, user two-phase transactions, long
  REPEATABLE READ and cursors across a switch), `038_secret_storage` (who can read
  the cluster secret, rotation, logs), `039_node_timeouts` (the cancel that does not
  block, worker settings), `040_ref_dml` (defaults, libm, views, date defaults),
  `041_copy_and_utility`, `042_add_node_acl`, `043_reference_paths` (every path that
  can write a reference table, the divergence check), `044_reference_lock` (the
  writer lock nobody can take, foreign key lock sets).
  One PostgreSQL major per cluster: `045_server_major` (`add_node()` refuses a node of
  another major, tested with a fault setting that makes this node report another
  major, on any single installation) and `046_two_majors` (real nodes of 18 and 19:
  `add_node()`, `check_cluster()`, `move_bucket()`; it skips, with the reason in its
  plan, unless the installation of the other major, named by `KINGRES_PG18` or
  `KINGRES_PG19` as in `kingres/dev/ext-majors.sh`, has this build of the extension).
  High availability (version 0.5), each with real streaming standbys: `048_standby_nodes`
  (registration, what a standby is skipped by, a standby that is down or does not stream,
  `check_cluster`, removal), `049_promote_node` (a dead primary, a switchover with a scan and a
  fan-out write in flight, a client of the old primary, the map group, the refusals),
  `050_failover_twophase` (the origin of a two-phase transaction stopped at every point of the
  protocol, with a synchronous and with an asynchronous standby), `051_gid_formats` (both
  identifier formats, before and after a failover), `052_distributed_deadlock` (cycles across two
  and three groups, a chain, a local deadlock, the decision with made-up edges),
  `055_ship_limits` (money and ON CONFLICT actions are not shipped), `053_restore_point` (the protocol, the refusals, and every server restored to the point under a
  multi-node write load), `054_failover_move` (a failover during the copy, the catch-up and the
  switch of a bucket move).
  `t/KingresShardTest.pm` builds clusters (and standbys); `t/KingresShardCompare.pm` is the
  comparison harness: it loads the same data into a three-node cluster and a
  single node and compares query results, and repeats every query under four
  non-default session profiles (`DateStyle`, `IntervalStyle`,
  `extra_float_digits`, `TimeZone`, `bytea_output`, `lc_*`; `profiles => 0`
  switches that off for one query). Later phases add queries to the list in
  `013_dist_scan.pl`.

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
| `kingres.add_node(host text, port int, group_id int default null, role text default 'primary') returns int` | Map group primary only. Registers a node (a new group when `group_id` is null), creates the extension there, creates the distributed and reference tables (with the rows of the reference tables and the foreign keys), sets its `local_node`, and copies the catalog, all in one two-phase transaction. A `standby` is only registered (it must name the group of its primary; when it can be reached it must be in recovery, of the same major and a copy of that primary). |
| `kingres.remove_node(node_id int)` | Unregister a node whose group owns no buckets (a standby: whatever its group owns; a primary that has standbys: not before them); its catalog is cleared if reachable. |
| `kingres.promote_node(node_id int)` | Superuser. Makes a promoted standby the primary of its group and the former primary a standby, in one two-phase transaction; raises the map version. On the map group primary, for group 0 on the promoted node itself. See "High availability". |
| `kingres.create_restore_point(name text) returns table(node_id, group_id, restore_lsn)` | Map group primary, superuser. A restore point on every primary with no distributed transaction half way through its commit. |
| `kingres.standby_status() returns table(...)` | Map group primary, superuser. The standbys with their replication state as their primary sees it. |
| `kingres.detect_distributed_deadlocks() returns int` | Map group primary, superuser. One pass of the distributed deadlock detector (the recovery worker runs it); returns the number of transactions cancelled. |
| `kingres.sync_catalog()` | Push the catalog to every active primary node, atomically. |
| `kingres.rotate_secret()` | Map group primary, superuser. Replaces the secret behind the tokens of node-to-node sessions on every active primary, atomically. Sessions another node opens while the commit is in flight may be refused once; repeat the statement. |
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

## Restoring a node from a logical dump

`pg_dump` of a node holds that node's partitions (`COPY public.d_b0001 ...`) and the
reference tables in full, and nothing of the cluster catalog or the secret. What works, in this order
(review finding S5-06):

1. Restore the schema and data into a database that has **no** `kingres_shard`: the partitions come
   back as plain tables and the distributed tables as plain partitioned tables. Run
   `CREATE EXTENSION kingres_shard` on the node only if it is to be a new node.
2. To put a node back into a cluster, restore into an empty database, then run
   `SELECT kingres.add_node(...)` from the map group primary for it (the node must not belong to a
   cluster); `add_node()` creates the tables, copies the reference tables and installs the catalog
   and the secret. Restored data of a distributed table is not read by the cluster: load it through
   the cluster (`COPY (SELECT ...)` out, `INSERT` in, or a restore on a single node that is then
   distributed with `create_distributed_table`).
3. A restore into a node that already has the catalog meets the refusal of `COPY` into bucket
   partitions, and of `CREATE TABLE` of a table that is distributed. A superuser can repair one
   node by hand with `SET kingres_shard.enable_routing = off`, which this check does not need to
   ask about: the partitions of the node can be written, and nothing else on the cluster changes.
   Afterwards run `kingres.check_cluster()`; a node that lost its secret shows as `secret_missing`
   and gets it back with `kingres.sync_catalog()`.

## Settings

| GUC | Default | Meaning |
|---|---|---|
| `kingres_shard.default_bucket_count` | 128 | Bucket count of a new colocation group (1 to 4096). |
| `kingres_shard.enable_routing` | on | When off, the planner and utility hooks do nothing. Superusers only (`PGC_SUSET`): with routing off a `SELECT` reads the local partitions only and a `TRUNCATE` clears this node only, which an administrator who repeats a statement on every node by hand wants and an ordinary session must not be able to switch on. |
| `kingres_shard.enable_distributed_scan` | on | When off, a `SELECT` that spans shard groups raises the phase 1 error instead of reading the other groups (to forbid slow cross-group queries). |
| `kingres_shard.scan_fetch_size` | 1000 | Rows requested from another group at a time by the distributed scan. |
| `kingres_shard.recovery_interval` | 30s | Seconds between runs of the recovery worker; 0 disables it. `sighup`. |
| `kingres_shard.deadlock_check_interval` | 2s | Seconds between passes of the distributed deadlock detector; 0 disables it. `sighup`. |
| `kingres_shard.deadlock_confirm_delay` | 200ms | Time between the two readings of the wait-for graph that confirm a cycle. `sighup`. |
| `kingres_shard.node_conninfo` | empty | Connection options appended to every node-to-node connection, see below. Server configuration only (`sighup`: `postgresql.conf` or `ALTER SYSTEM`), never per role or per database. |
| `kingres_shard.node_connect_timeout` | 5s | Connection timeout between nodes. |
| `kingres_shard.node_timeout` | 60s | The longest the phases that cannot be left waiting may wait for a node (`COMMIT PREPARED`, the cleanup after an abort, savepoint rollbacks, the recovery worker), and, as `tcp_user_timeout` and keepalives, the time after which a connection to a node that does not answer at all is dead. The cancel request sent when a wait runs out is bounded too (a third of this value, at most `node_connect_timeout`, at least a second), so a node whose postmaster is stopped delays the committing session by `node_timeout` plus that, not until it resumes (S3-07). The recovery worker takes the value of the server configuration, whatever `ALTER DATABASE ... SET` says (S3-16); so for `node_connect_timeout`. |
| `kingres_shard.forwarded` | off | Set by the router on the sessions it opens, together with `kingres_shard.forward_token`; fixed at connection time (`PGC_BACKEND`). A client that sets it without the token is refused. See "The forwarded flag" below. |
| `kingres_shard.forward_token` | hidden | The proof of a forwarded session; never shown. |
| `kingres_shard.fault_injection` | empty | Developer setting, superuser only: fails the commit protocol at a named point (`<point>:kill`, `:error`, `:wait:<file>`; points `before_prepare`, `after_first_prepare`, `after_prepare`, `after_log`, `after_commit`, `between_commit_prepared`; 0.5 adds `commit_waits_for_barrier`, `promote_before_change`, `promote_after_first_push`, `promote_after_push`, `restore_barrier_up` and `restore_all_quiet`). Used by the tests; it adds no capability a superuser does not have. |

### Node connection options

`kingres_shard.node_conninfo` is a libpq connection string appended to every
node-to-node connection (including those of the recovery worker): use it for
`sslmode`, `sslcert`, `sslkey`, `sslrootcert`, `sslcrl`, `gssencmode` and the
like. It is validated when it is set: the keywords the extension controls
(`host`, `hostaddr`, `port`, `dbname`, `user`, `password`, `options`,
`service`, `client_encoding`, `application_name`, `connect_timeout`,
`replication`) and strings that are not connection strings are rejected, and
the messages never repeat the value (an invalid value is replaced by a
placeholder, because the generic message of the server would print it), and so
is `sslpassword` (a password belongs in a file the server can read, not in a
setting). Only superusers can read it, and it can be set only in the server
configuration (`postgresql.conf`, `ALTER SYSTEM`): a value set with `ALTER ROLE
... SET` or `ALTER DATABASE ... SET` would be stored in `pg_db_role_setting`,
which every role can read, and a per-role value would let a role redirect the
node-to-node connections (review finding S2-16). The text of a connection error has the values of
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

**Not anyone can open such a session** (review finding S2-04; 0.2 as first
committed said the opposite and was wrong: a role with the write privilege on a
reference table could connect with the flag on and write one copy). A session
may be treated as forwarded, and its statement text as a scan or write
fragment, only if it proves it comes from a node of the cluster:

- `init_cluster()` creates a random secret (32 bytes from `gen_random_uuid()`)
  in `kingres.cluster_secret`, a table on which no role has any privilege and
  which has row-level security enabled and forced with no policy, so that roles
  that bypass table privileges (`pg_read_all_data`, a role with `SELECT` given
  by mistake, the owner of the database) see no row (S3-04); it is copied to a
  new node by `add_node()` and to every node by `sync_catalog()`, is never part
  of the catalog snapshot that every user can read, and is read only by the
  extension's C code with the heap API. The statement that installs it on a node
  tells the node not to log its argument or show it in an error (both
  `log_parameter_max_length` and `log_parameter_max_length_on_error` are 0 for
  it, S3-06). What this does not cover, honestly: a superuser reads the table;
  so does whoever can read the files of the node or its WAL (a role with
  `pg_read_server_files`, `REPLICATION` (`pg_basebackup`, logical decoding), a
  backup, the operating system account). Those can read every row of every
  table anyway; treat them as superusers. `pg_dump` does not dump the table.
  `kingres.rotate_secret()` (map group primary, superuser) replaces the secret on
  every node in one two-phase transaction and makes every token made from the old
  one useless; run it when a node, a backup or a disk left your control.
- A node that opens a node-to-node connection passes, in the connection
  options, `kingres_shard.forward_token`: the hex form of
  `HMAC-SHA-256(secret, "kingres-forward-v2" NUL role NUL database NUL id of the
  target node)`. A token seen for one role, one database or one node is useless
  for another: a copy of the database made under another name
  (`CREATE DATABASE ... TEMPLATE`) cannot make tokens for the original (S3-17). A
  copy under the same name has the secret, and is ended by the rotation.
- The receiving backend checks the token at the first statement of the session
  (the catalog cannot be read while the connection options are processed), with
  a constant-time comparison. A session that claims the flag and cannot prove
  it gets `FATAL: this session claims to have been opened by another node of
  the cluster but cannot prove it` before it runs anything. Markers in the text
  of an ordinary session are inert comments.
- The token and the secret never appear in `SHOW`, `pg_settings`, error
  messages, the log or `EXPLAIN`: the setting rewrites its own value to
  `<hidden>` before it is stored. Node-to-node connections should use TLS
  (`kingres_shard.node_conninfo`), because a token seen on the wire can be
  replayed for the same role and node. Without TLS, whoever can read the traffic between two
  nodes can open sessions that the node takes for sessions of the cluster, as that role,
  until `kingres.rotate_secret()` is run: the exposure of a cluster whose nodes talk in the
  clear (S3-17).
- A node that is not part of a cluster (no node id) holds no secret and accepts
  the claim: there is nothing to protect there, and `add_node()` talks to a node
  in that state. A node that belongs to a cluster and holds no secret (one that
  was upgraded from 0.1 and has not yet run `SELECT kingres.sync_catalog()` on
  the map group primary, or whose secret was deleted) fails closed (S3-09): it
  accepts a claim only from a superuser, which is what `add_node()` and
  `sync_catalog()` connect as, and refuses everybody else with a hint to run
  `sync_catalog()`. The statements of ordinary roles that need such a node
  therefore fail until the secret is installed. After a rolling upgrade, run
  `sync_catalog()` once on the map group primary: it makes the secret if the
  node has none and copies it to every node.

With the proof in place an ordinary role cannot write one copy of a reference
table, run DDL without propagation, or write a bucket on one node only: the
flag is refused before any statement runs. A statement without a marker that
is not provably local in a session that did prove it is still `KS001`. What a
holder of a valid token can do (a role that is given a copy of the token, or
that can read the traffic) is what a forwarded session can do: see this node's
rows through a scan fragment, write this node's rows through a write fragment
or change a table on this node only, within its own privileges.

A database that holds the objects of the first committed 0.2 (before the
secret existed) has no `kingres.cluster_secret`; since 0.2 is unreleased it is
repaired by dropping and creating the extension on a test cluster, or by
running the `CREATE TABLE kingres.cluster_secret ...` and
`kingres.internal_ensure_secret()` statements of `kingres_shard--0.2.sql` once
on each node and then `sync_catalog()` on the map group primary.

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
  COLUMN` (a constant default only; no `serial`, no identity; a default written
  as a string for a date, time or object-name column (`regclass`, `regtype`, ...)
  is refused unless it is numeric ISO text such as `'2024-01-02 03:04:05+00'` or
  one of `epoch`, `infinity`, `-infinity`, because `'now'`, `'today'` and a table
  name mean another value on every node; domains, arrays, ranges and composite
  types over such types count too), `DROP COLUMN`
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

- `INSERT ... SELECT` and multi-row `INSERT` into a table with column defaults
  work: the defaults (this node's, which are the same constants everywhere) are
  sent as values, not looked up again on every node (S3-10). An identity or
  `serial` column is refused, as any `nextval()`.
- Functions that use the C library of the node for a floating point result (`exp`,
  `ln`, `log`, `power` and `^`, the trigonometric, hyperbolic and gamma functions,
  `cbrt`) are not shipped (S3-14): another libm can differ in the last bit and make
  the copies differ. The same functions in a restriction of a distributed scan are
  evaluated by the node that reads. `sqrt`, the basic operators and everything on
  `numeric` are exact and are shipped. A floating point `sum` or `avg` over a
  distributed table adds the groups in an order that depends on the node that runs
  the query, so the last digits can differ between origins: that is a *read*, and it is
  not refused. A *write* to a reference table that reads a distributed table (as
  `INSERT INTO ref SELECT sum(f) FROM dist`) is refused whatever the aggregate, and a
  floating point aggregate is not shippable (see below), so no copy of a reference table
  can depend on the order of the rows of the origin (S3-14).
- A write through a view of a reference (or distributed) table is refused with "it
  writes through a view; write the table itself" (S3-18): the rewritten statement
  does not stand alone, and `WITH CHECK OPTION` and the rights of the view's owner
  cannot be sent. A view that is only read works.
- `add_node()` gives the new node the owners and privileges of the tables, of the
  sequences of their columns and of their schemas, with column privileges and grant
  options (S3-11); the grantor is the owner. It checks first that every role that
  owns something or holds a privilege exists on the new node and names the missing
  ones (roles are not copied).
- `COPY <distributed table> TO` is run as `COPY (SELECT <columns> FROM <table>) TO`
  with the same options, so it returns the rows of every group (the core refuses
  `COPY` of a partitioned table with a hint to use the query form; a core that
  accepted it would return this node's buckets only). A reference table is local and
  whole; a bucket partition named directly is read as it is, the rows of that bucket
  on this node. `COPY ... FROM` stays refused.

**The rule for every statement that is run on several nodes** (a reference-table
write, and the `UPDATE` and `DELETE` that span shard groups; one place in the
code: `src/shippable.c`, `ship_walker` and `query_ok`). The copies must be equal
whatever the physical layout of each node is, and a restriction must mean the
same on every node whatever the session settings are. So the statement is
refused ("cannot be run on every node" / "cannot be shipped") when it has:

- a function, operator, cast or type input/output function that is not built in
  and immutable in the catalog's sense (as `postgres_fdw` requires), which
  includes every conversion through the text form of a value (`x::text`,
  `'...'::date` of a column: they depend on `DateStyle`, `IntervalStyle`,
  `extra_float_digits`, `bytea_output`, `TimeZone`);
- a system column (`ctid`, `xmin`, `tableoid`, ...), `LIMIT` or `OFFSET` (the
  rows picked depend on the physical order; a total `ORDER BY` is not
  recognized), `DISTINCT ON`, `TABLESAMPLE`, a window function, an aggregate
  whose result depends on the order of its input (`array_agg`, `string_agg`,
  `json_agg`, `xmlagg`, `any_value`, any aggregate of `float4` or `float8`);
- an `UPDATE ... FROM` (or `DELETE ... USING ... RETURNING`) of a relation
  that is not matched through every column of one of its unique indexes, because
  which of several matching rows supplies the values is arbitrary.

**A reference table is written on every copy or not at all** (review findings
S3-01, S3-02, S3-13). Whatever path a write takes, it either runs on every node
or fails before it changes anything; silently writing one copy never happens.
The decision for each path (tested in `t/043_reference_paths.pl`):

| Path | Decision |
|---|---|
| a statement that names the table | run on every node |
| `INSERT ... SELECT`, `UPDATE ... FROM`, `ON CONFLICT`, `RETURNING`, prepared statements, custom and generic plans | run on every node (the statement must be shippable, see above) |
| a function (any language), a `DO` block or dynamic SQL called by a statement that runs on this node | each write it makes is a statement of its own and runs on every node |
| the same function called by a statement that is routed to another shard group (`INSERT INTO d VALUES (6, f(6))`, `SELECT f(6) FROM d WHERE id = 6`) | **refused**: `reference table "t" cannot be written from a statement that is run for another node`; nothing is changed. Call the function in a statement of its own |
| a trigger on an ordinary table | its write runs on every node, as a function's does; a trigger on a bucket partition (made by hand) fires on the other node and is refused as above |
| a trigger on the reference table itself | **refused**, `CREATE TRIGGER` and `CREATE CONSTRAINT TRIGGER`, also by a superuser with `enable_routing = off` (S5-01): what a trigger reads or computes on each node (a count of a distributed table, a clock, a sequence) cannot be shown to give every copy the same row. The supported way is to put the logic in the statement that writes the table, or in a trigger on a distributed table; the foreign keys of a reference table are not affected, and their actions are the only statements that run on this node's copy alone while a write to a reference table runs. Any other statement that such a write makes run (a function it calls) is planned, routed and checked like a statement of its own |
| a rule on an ordinary table whose action writes the table | **refused** ("cannot be run on every node": the rule's action reads the rule's table) |
| a data-modifying `WITH` | **refused** ("a write to a reference table inside a WITH clause or a subquery is not supported yet") |
| `MERGE`, `COPY ... FROM` | **refused** |
| `TRUNCATE` | propagated, from the map group primary only |
| the actions of foreign keys between reference tables (`ON DELETE CASCADE`, `SET NULL`, `ON UPDATE CASCADE`) | every node does its own part of the cascade once; the writer locks cover every reference table reachable through foreign keys |
| a plan made before the table became a reference table, or before the cluster had several nodes (a prepared statement, a PL/pgSQL function, a driver's statement) | `create_reference_table()` and every catalog change invalidate the plans of the tables, so the plan is made again; as a backstop the executor refuses a write to a reference table that a plan would make on this node only (`reference table "t" cannot be written by this plan ...`, repeat the statement) |
| a session of a superuser with `kingres_shard.enable_routing = off` | allowed, by hand, on that node only: the superuser repeats it on every node |

The check that makes this true is not in the planner hook, because a cached plan
and a function that runs on another node never see the hook: it is made when a
plan that modifies a table starts to run (an executor hook), so no path goes
around it. A session that another node opened for its fragment of a write may
modify a reference table only when the statement text begins with the marker the
fanning-out node adds; anything else it runs, such as a function that a routed
statement calls, is refused.

`kingres.check_cluster()` compares the copies: for every reference table the row
count and an order-independent digest of the rows (a sum of 64-bit hashes of each
row's text under fixed session settings) on every active primary against the map
group primary's, and reports `reference_table_differs` for a node whose copy is
another one. It reads every row of every reference table on every node, so it is a
check for maintenance windows; a write that is in flight shows as a difference
once, and a difference is reported only if it is still there after two more looks.

**Writers are serialized cluster-wide** (review findings S2-06, S3-03). Before it
runs, a reference-table write takes, on every active primary node in node-id
order, a lock for each reference table the statement uses or can reach through
foreign keys (tables in name order), held until the distributed transaction ends.
Reads are never blocked. A write that reads the table it writes, or another
reference table, therefore sees the same rows on every node, and the fixed order
makes such writers wait for each other without a deadlock. It costs concurrency
(reference-table writers do not run concurrently, even on different tables that
one statement does not combine) and availability (a node that is down blocks them,
as before). The lock is a heavyweight lock on an object of a class of its own (the
OID of the table on that node), not an advisory lock and not `LOCK TABLE`: advisory
locks live in a key space every role shares, so any role could hold the key of a
table and stop all its writers (S3-03); `LOCK TABLE ... SHARE ROW EXCLUSIVE` needs
the `UPDATE`, `DELETE` or `TRUNCATE` privilege, which a role that may only `INSERT`
does not have. No SQL a role can run reaches a lock of that class: the function
that takes it for another node (`kingres.internal_lock_reference_table`) answers
superusers and sessions that proved they come from a node of the cluster. The wait
is bounded by the session's `lock_timeout` and by `kingres_shard.node_timeout`
(whichever is shorter), with an error that names the table. A prepared transaction
of a crashed origin keeps the lock until it is resolved (`recover_prepared_transactions()`
or the recovery worker), so writers of that table fail with that error meanwhile;
`check_cluster()` lists the prepared transactions. `add_node()` takes
the same locks on the existing nodes while the new node's copies are made, and
a writer that waited for it finds the set of nodes changed and fails with
`KS001`-class "the set of nodes changed while a write to a reference table
waited" (repeat the statement): no write is lost on the new node (S2-07).

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
- **UPDATE ... FROM, DELETE ... USING and MERGE that read a distributed table
  and write another table** are refused ("UPDATE, DELETE and MERGE that read
  distributed table ... are not supported"): PostgreSQL asks for the row
  identifier of every table it does not modify. Only `INSERT ... SELECT` and
  `CREATE TABLE AS` can write an ordinary table from a distributed one (S2-18).
- **Nodes must agree.** `add_node()` refuses a node whose default collation,
  `LC_CTYPE`, collation provider or collation library version differs from the
  map group primary's, or that runs another version of the extension; a
  different database encoding is a warning (statements are converted, but
  comparisons of text outside ASCII can differ).
- **Two-phase gids.** The prepared transactions the extension makes are named
  `kingres_<origin>_<pid>_<xid>_<target>[_<n>]`; recovery touches only names of
  exactly that shape. Anyone who can connect and prepare a transaction can still
  name one after a live transaction of the origin (both numbers are visible in
  `pg_locks`), which makes recovery leave it alone while that transaction lives,
  or make the real prepare fail: the same exposure as any prepared transaction.
- **Planning side effects.** On a cluster with several shard groups, every
  statement of every session that does not name a distributed table is planned
  with the distributed scan armed, and `enable_partitionwise_join` and
  `enable_partitionwise_aggregate` are off while it is planned (S2-20): the scan
  path replaces a partitioned table's paths. Restrictions of a scan are shipped
  in the order the planner gives them, not in the security-level order of
  `order_qual_clauses` (a static function of the core); the rows of the other
  groups are filtered by the same restrictions again here.
- **Functions.** A statement inside a PL/pgSQL function, a `DO` block, an
  `EXECUTE`, a trigger, a rule or a cursor is routed like any other statement;
  a SQL-language function expanded into the statement is read through the
  distributed scan. A distributed scan, a statement for another shard group and a
  fanned-out write never run in a parallel worker (review finding S2-08): the
  scan path marks the table not parallel-safe, and a worker that reaches the
  extension's planner or executor code raises an error that says to mark the
  function `PARALLEL RESTRICTED` (a `PARALLEL SAFE` function that reads a
  distributed table fails in a worker rather than answering from a snapshot of
  its own). A hot standby
  routes and scans like its primary but cannot reload a stale bucket map.
- **Collations.** A comparison restricts a statement to one bucket only if it is
  made under the collation the table was distributed with (and that collation
  is deterministic); a comparison under another collation is evaluated here by
  the distributed scan (it is not shipped). The nodes must agree on the
  default collation and its library version: shipped comparisons of the default
  collation rely on it.
- **Views.** PostgreSQL checks the rights on the table of a view with the owner
  of the view (unless the view is `security_invoker`). A fragment run on another
  node uses the rights of the session role, so a statement that needs another
  shard group through a view owned by another role is refused with a clear
  message (S2-15); it is not run with the wrong rights. The router does not see
  a key restriction behind a view, so `WHERE id = 5` on such a view is refused
  as well. Create the view `WITH (security_invoker = true)`, or read as the
  owner.
- **Ownership.** `ALTER TABLE ... OWNER TO` on a distributed table is applied to
  the bucket partitions and their indexes on every node (S2-13).
- **Session settings.** `search_path` of the remote session is `pg_catalog`
  (as `postgres_fdw` does) for data statements; a function without its own `SET
  search_path` that runs on the remote node resolves unqualified names there.
  Types whose text form depends on `lc_monetary` or `lc_numeric` (`money`) are
  not portable between sessions with different settings: a value or restriction of
  type `money` (or an array of it) is never shipped to another node, so an `UPDATE`
  or `DELETE` that spans groups, or a reference-table write, that uses one is
  refused. `ON CONFLICT` actions other than `DO NOTHING` and `DO UPDATE` (PostgreSQL
  19's `DO SELECT`) are refused until they are designed.
- **Generic plans** cannot route a statement restricted by a parameter. With
  `plan_cache_mode = auto` the plan cache keeps using custom plans for such
  statements; `force_generic_plan` fails with a clear error. The distributed
  scan does not depend on parameter values and works with generic plans.
- **Stale maps.** The stale-map retry (`KS001`) happens once, and only when the
  remote transaction has run no earlier statement. The distributed scan asks the
  groups the node's current map names. Buckets move (version 0.3) and a primary can
  be replaced (0.5): a statement planned against the map that a switch or
  `promote_node` replaced fails with `KS001` and is repeated by the client, and a
  node that is no longer the primary of its group answers no fragment of any kind.
- **Standby of a shard group.** A standby is registered, owns nothing and is not used
  for reads of other nodes; failover is described under "High availability". A read
  on a standby itself (hot standby) is routed as on its primary, so the part of a
  distributed scan that belongs to the standby's own group is read from the standby
  and lags by the replication delay; use the primaries for reads that must not lag.
- Roles, schemas created for tables, and user-defined types must already be
  compatible on every node; they are not synchronized. Privileges are copied
  by propagated `GRANT`, and to a new node by `add_node()` (see "Reference
  tables"); `REASSIGN OWNED` and `ALTER DEFAULT PRIVILEGES` are not propagated.
- A reference-table write to a group that is down fails (the write needs every
  node); reads are unaffected.
- Cached connections are per backend and are replaced when found closed; there
  is no pool shared between backends.
- `dist_collation` is an OID; non-default, non-deterministic collations are
  not portable between nodes.

## Moving buckets and rebalancing (version 0.3, phase 4)

Design: [`kingres/docs/design/shard-phase4.md`](../../kingres/docs/design/shard-phase4.md)
(with its "Implementation notes and deviations"). The extension has a primitive
that moves one bucket and a function that plans which buckets to move; driving a
whole rebalance (order, concurrency, progress, stop and resume) is the control
plane's job.

### What a move does

`SELECT kingres.move_bucket(colocation_id, bucket, target_group)` runs on the map
group primary, as a superuser, in a READ COMMITTED session, and returns the id of
its row in `kingres.bucket_move` when the bucket is on the target (or raises an
error and leaves nothing behind). The unit is one bucket of one colocation group:
the partitions of **every** table of the group move together. Reference tables do
not move. The phases, recorded in `kingres.bucket_move.phase` before the action
they announce:

1. **preparing**: the preconditions are checked on both nodes, then (in a session
   of its own, so that it is committed at once) the cluster lock is taken, the
   move is recorded and the bucket is set to `moving` on every node (atomically).
   Routing is unchanged.
2. **copying**: on the target, standalone tables (`<table>_b<bbbb>`: the columns,
   defaults, constraints and indexes of the parent, and a check that is the
   partition constraint) that are **not** attached to anything, so a scan never
   sees a half-filled copy. On the source a publication of the bucket's partitions
   and a logical slot; on the target a subscription (`synchronous_commit = on`)
   with the initial copy.
3. **catching_up**: until every table of the subscription is ready and the lag is
   within `kingres_shard.move_catchup_threshold` or a marker row has made the
   round trip (next section).
4. **switching**: one two-phase transaction across the map group, the source and
   the target (see "What a move blocks").
5. **cleaning**: the subscription, slot and publication are dropped at once; after
   `kingres_shard.move_cleanup_delay` the orphan tables of the source
   (`kingres.orphan_<move>_<n>`) are dropped by `kingres.cleanup_moved_buckets()`
   (the recovery worker calls it) and the move is `done`.

Moves of different buckets may run at the same time in different sessions; a
second move of the same bucket fails ("already being moved").

### How it knows the copy is complete and replayed

- *The initial copy is complete* when `pg_subscription_rel` on the target has a row
  for every published table (the bucket's partitions and the marker table) and every
  row has `srsubstate = 'r'` (ready: the table sync worker has copied it and the apply
  worker has taken it over). `pg_stat_subscription_stats` is watched for apply and
  sync errors, which fail the move.
- *The subscriber has replayed up to the switch point* when a **marker row**,
  inserted on the source in its own committed transaction after the bucket's tables
  are locked against writes, has arrived on the target (`kingres.move_marker`, in the
  publication with a row filter on the move id). Logical replication applies
  transactions in the commit order of the source, and every transaction that wrote the
  bucket before the lock has committed before the lock is granted, so the marker
  arrives after all of them. This is exact where the slot position
  (`pg_replication_slots.confirmed_flush_lsn`) is not: that position advances only
  with transactions that touch published tables, or with a keepalive after half of
  `wal_sender_timeout`, so on a busy source it can lag for reasons that have nothing
  to do with the bucket. The slot lag is used only as a quick test in *catching_up*.

### What a move blocks, and for how long

Only the switch blocks, and only for as long as the attempt lasts (normally tens of
milliseconds; the measurement is in the design note):

- On the **source** the tables of the colocation group (their parents) are locked in
  SHARE mode: **writers of those tables on that node wait, readers go on**; for the
  last step (the detach) the lock becomes ACCESS EXCLUSIVE until the two-phase commit
  ends. The lock is on the parents, not only on the bucket's partitions: PostgreSQL's
  DETACH needs the parent exclusively, and a writer that is queued for the bucket's
  partition holds its parent. So **writers of every bucket of those tables on the
  source node wait**, not only writers of the moving bucket.
- On the **target** the tables are locked in SHARE ROW EXCLUSIVE (attaching a table
  that has a foreign key needs it): writers of those tables on the target wait too,
  readers do not.
- Locks are **tried for, never queued for** (review finding S4-04). A request that
  waits for a lock makes every later statement that needs a conflicting lock on the
  same table wait behind it, so the switch does not wait: it first looks, holding
  nothing, for a transaction that has been open for longer than an attempt tries and
  holds a table of the group in the way (it will not be gone in time), then takes each set with `LOCK ... NOWAIT`, trying again for at
  most a tenth of `kingres_shard.move_switch_timeout` (default 10 s, so 1 s; at least
  200 ms), and takes the ACCESS EXCLUSIVE locks of the detach the same way before the
  `DETACH` itself. A set that cannot be had abandons the attempt: everything is
  released, the move goes back to *catching_up* and tries again after a pause that
  doubles from 0.2 s to 10 s. A transaction that keeps a table of the colocation group
  open (a long writer of any bucket of the tables, or, for the last step, a long
  reader) therefore delays the move and costs the application nothing; after
  `kingres_shard.move_switch_attempts` attempts (default 20, a few minutes at the longest pauses) the
  move fails and its `detail` names the sessions in the way (`held by pid 123`), the
  bucket goes back to `active`, and the move can be started again. The whole attempt
  takes at most `move_switch_timeout`.
- **A write straight into a bucket partition** (`INSERT INTO orders_b0002 ...`) takes
  no lock on the parent and is not a write of the distributed table: it is refused
  ("cannot write a bucket partition of a distributed table directly"), for superusers
  too. A superuser who has switched `kingres_shard.enable_routing` off (to repeat a
  statement on every node by hand) can still write one; the switch covers that case
  as well, because it also locks the partitions of the moving bucket in SHARE mode, so
  a transaction that wrote one before the switch holds it back until it ends, and
  none can start before the commit of the switch.
- A statement routed with the old map that reaches the source after the switch gets
  the stale-map error `KS001`; its origin reloads the map and repeats it (when it can:
  when no earlier statement of the transaction ran on that node).
- **The map version rule** (the placement rule; `src/dist_scan.c`, `ds_start`, and
  `src/fanout.c`, `run_all`). A node's bucket partitions and its placement of the
  colocation group (which group holds each bucket) change only in the switch's
  transaction on that node (on the map group primary, when it is the source or the
  target, that part runs in the coordinator's own transaction); the source also holds
  ACCESS EXCLUSIVE on the group's parents from the detach to the end of its commit, so
  a statement that opens the tables there waits for it. A scan asks every remote node
  for two numbers, read from the catalog tables and not from a cache: the placement of
  its colocation group (`kingres.internal_placement_hash(colocation_id, parent)`) and
  the **identity of the partitions** the node has of the table
  (`kingres.internal_partition_identity(parent)`, a sum over the OIDs of the attached
  partitions), **before** it opens its cursor there and **again after**. The placement
  of every node must be the one its own plan was made with, and the identity of every
  node must be what it was before the cursor was opened; so must the identity of the
  scan's own node, against the partitions its local plan reads. The identity is what
  sees a bucket that left a node and came back in between (S4-01): the placement is
  the same then, but the partition that came back is another table. A fan-out write
  compares the placement of every target after the statement ran. A difference raises
  `KS001` ("the bucket map changed while the statement was reading it") before the
  first row is returned (a scan) or with every change rolled back (a write), and the
  statement can simply be repeated. When nothing differs, every node serves exactly the
  buckets the placement gives it, so each bucket is read or written once, by its
  owner. A statement fails with `KS001` only when a switch committed on a node it reads
  between planning and the check; other catalog changes (another table, another
  colocation group, the `moving` state of a move) do not fail it. Routed single-group
  statements need no comparison: they run on the one node that serves them, and that
  node checks that it owns the bucket (`KS001` otherwise); a reference-table statement
  never sees a move. The argument is in the design note.
- While a move of a colocation group is unfinished (before *cleaning*), **DDL on the
  tables of the group and the creation of a table in it are refused** ("a bucket of
  colocation group N is being moved"): the standalone copy would not see the
  statement. `remove_node()` refuses a group that takes part in a move.
- A move holds one snapshot on the map group primary for as long as the function runs
  (it is one statement); for hours-long copies that delays vacuum there, like any
  long statement. The slot holds WAL on the source until the move ends.

### Requirements on the cluster

- `wal_level = logical` on **every** node; `max_replication_slots`, `max_wal_senders`
  and `max_logical_replication_workers` with room for the slot and the table sync
  workers of each move that runs at the same time (the move checks and names the
  setting); on PostgreSQL 18 also `max_active_replication_origins`.
- Every table of the colocation group has a primary key or `REPLICA IDENTITY FULL`
  (`REPLICA IDENTITY USING INDEX` and identity columns are refused as "not supported
  yet"). A table without one is named in the error and nothing is started.
- Source and target run the same PostgreSQL major; a move between nodes of different
  majors is refused before it starts.
- The role that runs the move is a superuser. The subscription connects with the
  `kingres_shard.node_conninfo` of the **target** (sslmode and key files) and the
  password file of the server's OS user; no password is stored in the catalog.
  `pg_hba.conf` of the source must let the target in for that role, as a logical
  replication connection to the database.
- No cycle of foreign keys between the tables of the group, and no DDL meanwhile.

### Functions and settings

| Function | |
|---|---|
| `kingres.move_bucket(colocation_id, bucket, target_group) returns bigint` | the move |
| `kingres.rebalance_plan(max_moves default null)` | `(colocation_id, bucket, source_group, target_group, size_bytes)`: the moves that even out the buckets per group, in the order to run them; any node, reads only (sizes come from the source group when the caller is a superuser) |
| `kingres.set_node_property(node_id, should_have_shards)` | change the flag (atomic); a group whose primary has it false is drained by the plan |
| `kingres.resume_bucket_moves() returns int` | undo the moves interrupted before the switch, finish those after it; the recovery worker calls it |
| `kingres.cancel_bucket_move(move_id)` | stop and undo a move that has not reached *switching*; it says that the move was stopped, or fails with "passed the point where it can be stopped"; a move that was asked to stop never enters its switch (S5-05) |
| `kingres.cleanup_moved_buckets(force default false) returns int` | drop the orphan tables whose delay is over (all with `force`) |
| `kingres.check_cluster(older_than default '5 minutes')` | one row per inconsistency |

Settings: `kingres_shard.move_catchup_threshold` (1MB), `kingres_shard.move_switch_timeout`
(10s), `kingres_shard.move_switch_attempts` (20: how many times a move tries to switch before it fails, see "What a move blocks"), `kingres_shard.move_cleanup_delay` (10min).

The plan: only groups whose primary has `should_have_shards` receive buckets; the
target count is `bucket_count / receivers`, the remainder one each to the receivers
that already hold the most; only the surplus of a group moves, and only to groups
below their target (the fewest moves); a bucket that is itself moving is left out;
colocation groups are independent and interleaved so that a receiving group fills
evenly.

### When a move fails: what an operator does

A failure before the switch undoes itself (subscription, slot, publication and the
standalone tables are dropped, the bucket goes back to `active`, the move is `failed`
with the reason in `detail`) and the move can be started again. If the session or a
node died, or the undo itself could not reach a node, the move stays in its phase:
run `SELECT kingres.resume_bucket_moves()` on the map group primary. The recovery
worker of the map group primary does it, and calls `cleanup_moved_buckets()` after it,
every `kingres_shard.recovery_interval` once it has resolved the prepared transactions
(review finding S4-02; with the interval at 0 nobody does, and `check_cluster()`
reports a `stalled_move`). A move in *preparing* is recorded a moment before its
session takes the live mark, so it too is left alone for `2 x node_timeout`. A move in *switching* is decided by the two-phase commit: the phase
becomes *cleaning* in the same transaction as the bucket map, so a *switching* row
after a crash is a switch that did not commit. `resume` waits `2 x
kingres_shard.node_timeout` after the last update before it undoes such a move, so
that a switch that is still committing is not undone. A move that is alive (its
session holds a lock that nothing else takes) is never touched by resume. Watch
`kingres.bucket_move_status` (everybody may read it: ids, groups, `phase`, times) and,
as a superuser or a role that was granted `SELECT` on it, `kingres.bucket_move` (it
also holds `detail`, the last error with the text of remote errors and node
addresses, and the slot and subscription names; review finding S4-07);
`cancel_bucket_move()` stops a running move.

The helper functions `kingres.internal_placement_hash()` and
`kingres.internal_map_version()` are called by the sessions of other nodes, which
run as the role of the user whose statement it is; they answer superusers, sessions
that proved they come from a node of the cluster, and a role that may `SELECT` the
distributed table the call names (review finding S4-06). An ordinary role gets
"permission denied" and cannot make a backend forget cache entries.

### What check_cluster reports

On the map group primary, superuser: per active primary node `catalog_differs` (the
version counter is not compared), `partition_missing` (a bucket's partition absent
on its owner), `partition_misplaced` (attached where the group does not own the
bucket), `stray_bucket_table` (a table with the name of a bucket partition that is not
attached), `orphan_table` (no record, a move that is not cleaning, or overdue by
`2 x (node_timeout + recovery_interval)` after its cleanup time), `stalled_move` (a
move in a phase before *done* that no session runs and that nobody has touched for that
long, or a *cleaning* move that is overdue by it: the worker would resolve it, or
`resume_bucket_moves()` / `cleanup_moved_buckets()` by hand), `replication_slot`, `publication`, `subscription`, `replication_origin`
(leftovers of a move that is not running), `prepared_transaction` (older than the
argument), `node_unreachable`; and on the map group `bucket_map_incomplete`,
`group_without_primary`, `bucket_moving_without_move`, `move_without_moving_bucket`; per
active primary also `extension_version_differs` (the node runs another version of the
extension: nothing else is compared with it, so a copy of a reference table that could not be
read is not reported as one that differs), `secret_missing` (the node holds no cluster secret),
`server_major_differs` (the node runs another PostgreSQL major than the map
group primary); and for every reference table `reference_table_differs` (a node whose copy has other rows).
No rows means consistent, in this sense: it is a **structural** check (catalogs, which
partitions exist where, what a move left behind). It never compares the rows, indexes,
constraints or replica identity of a partition with its parent, so a copy that diverged
in content does not show up; after a move compare counts or checksums of the bucket's
tables yourself if you need that assurance. (Reference-table copies are compared by
their rows; see "Reference tables".) The old copy of a moved bucket stays readable in
`kingres.orphan_<move>_<n>` (the extension's schema, with the rights of the table's
owner) until `move_cleanup_delay` has passed and the worker or
`cleanup_moved_buckets()` dropped it; a deletion that must be final at once (erasure)
needs `cleanup_moved_buckets(true)` after the move (review finding S4-10).

### Limits

- One bucket per call; no splitting, no change of the bucket count, no move between
  colocation groups. A bucket move whose source primary is replaced
  fails and is undone (the replication slot of the move does not survive a
  failover), unless its switch had been decided, in which case recovery finishes it.
  When the target primary is replaced, the subscription of the move is on its
  promoted standby too and carries on, so the move may finish or be undone; in every
  case each row is there exactly once (`t/054_failover_move.pl`).
- During the switch writers of the group's tables wait on the source and the target
  node (not only those of the bucket), and a switch has to wait for transactions that
  hold the tables: long transactions delay moves. Attaching a copy of a table with a
  foreign key checks the key by scanning the copy under the lock, so the switch of a
  large bucket of such tables takes longer.
- The slot of a move waits at its creation for the transactions that are running on
  the source (bounded by 5 x `node_timeout`).
- A scan or a fan-out write that meets a switch in the middle fails with `KS001`
  instead of answering; the client repeats it.  A statement that opens the tables on
  the source while the switch is committing there waits for the commit
  (milliseconds); one whose transaction is open on the source holds the switch back
  until it ends (the switch gives up after its lock timeout and tries again), so a
  long transaction that read a table of the group delays its moves.
- Orphan tables are kept for `move_cleanup_delay` and take disk space on the source.
- `move_bucket()` is one statement of the session that calls it, so its snapshot, and
  with it the xmin horizon of the map group primary, is held back for the whole move
  (review finding S4-09): vacuum there cannot remove rows that became dead after the
  move began. The tables of the map group primary that change are the catalog tables,
  which are small; a long copy therefore costs little, but the tables of an application
  that lives on the map group primary bloat for as long as a move lasts. Moves are
  expected to last minutes. (Splitting the move into recorded steps that a poller
  drives would release the snapshot; it is not done in 0.3.)
- Transactions that stay open across a switch (covered by `t/037`): a transaction
  that holds a lock on the tables of the group on the source (a writer, a prepared
  user transaction, a REPEATABLE READ or SERIALIZABLE reader, a cursor of a scan, and a
  transaction on any node that has scanned the table, whose remote transactions stay
  open until it ends) delays the switch; a move that cannot get past it gives up with
  the pid of the blocker and moves nothing. A REPEATABLE READ or SERIALIZABLE
  transaction on the node that a bucket moves *to*, which has read only rows of its own
  buckets and so holds nothing on the source, does not hold the move back, and its next
  read of the table would not see the rows of the arrived bucket (they were written
  after its snapshot): it fails with a serialization failure ("a bucket of table ...
  moved to this node after the snapshot of the transaction was taken") and can be
  retried. `COPY (SELECT ...) TO` reads like any `SELECT`; `COPY ... FROM` into a
  distributed table is refused, during a move as always.

## High availability (version 0.5, phase 5)

Version 0.5 is the part of high availability that lives in the extension
([`kingres/docs/design/ha-phase5.md`](../../kingres/docs/design/ha-phase5.md),
part C; the design calls it 0.4, a number that a fix round used first). The
control plane creates the standbys (`pg_basebackup`, streaming replication),
decides when a primary is gone, fences it and promotes a standby; the
extension is what makes the cluster's catalog, routing and two-phase commit
follow. The functions below are for the control plane and for an
administrator; all of them are for superusers (`REVOKE ... FROM PUBLIC`, and
`promote_node`, `create_restore_point`, `standby_status` check it again).

### Standby nodes

`kingres.add_node(host, port, group_id, 'standby')` on the map group primary
registers a standby of an existing group. A standby

- **owns no buckets**: the buckets belong to groups and are reached through the
  primary of the group; a standby is skipped by routing, by the catalog
  synchronization (it gets the catalog by physical replication, as it gets
  everything), by writes to reference tables, by the fan-out of `UPDATE` and
  `DELETE`, by the distributed scan, by recovery of prepared transactions and by
  bucket moves;
- **is skipped by the version and major checks that connect to nodes**
  (`internal_check_node_versions`, the checks of `add_node`): a physical copy
  has the PostgreSQL major, the extension version and the catalog of its primary
  by construction. What can differ is its *library* (a standby that was not
  upgraded with its primary), and that matters at promotion, so
  `promote_node` checks the library of the node it promotes;
- **may be absent or not streaming** without harm to the primaries: nothing on a
  primary ever connects to a standby, and `synchronous_standby_names` is the
  control plane's business. A standby that cannot be reached at registration is
  registered with a warning;
- is checked when it can be reached: it must be in recovery, run the major of this
  node and be a copy of the primary of its group (same system identifier). A
  node of another cluster, or one that is not in recovery, is refused.

`kingres.remove_node(node_id)` removes a standby (also of a group that owns
buckets). A primary that still has standbys cannot be removed before them.

`kingres.standby_status()` (map group primary) returns one row per standby with
its **replication state as its primary sees it** (`streaming`, `catchup`, ...,
`not connected`), the synchronous state (`async`, `sync`, `potential`), the
replay position and lag in bytes, and `problem` and `detail` when something is
wrong. The standby is matched to a walsender of its primary by the name of its
replication slot, else by its `application_name` (that of `primary_conninfo`,
else its `cluster_name`, else `walreceiver`). `kingres.check_cluster()` includes
the rows with a problem: `standby_unreachable`, `standby_not_streaming`,
`standby_not_in_recovery`, `standby_without_primary`.

A node's `local_node` row says which node it is. A standby is a physical copy
and holds the row of its primary; `promote_node` sets it. The token that proves
a node-to-node session is made for that id, so a session to a standby is made
for the id of its group's primary.

A node that the catalog does not call the primary of its group (the old primary
after `promote_node`) is in **no group** for routing: it runs no statement of a
distributed table as local, forwards the statements of its clients to the
primary of the group, reads every group (not its own stale partitions) in a
distributed scan, and refuses a forwarded session with the retryable `KS001`.
It does not stop an application that writes ordinary tables on it: it must be
fenced (stopped, or rebuilt as a standby) by the control plane, which is what
the design does before it promotes.

### `kingres.promote_node(node_id int)`

Run by the control plane **after** the standby was promoted in PostgreSQL
(`pg_promote`). It refuses unless the node is out of recovery and runs the same
extension version and library. In one two-phase transaction on every node that
can be reached:

1. the node row of the promoted standby becomes `primary` and the former
   primary's `standby`; the statement triggers raise the map version;
2. the promoted node's `local_node` is set to its own id;
3. every node that applies the catalog drops its idle cached sessions to nodes
   that are no longer primaries; the old primary, if it is reachable, gets the
   catalog too (if not, a warning says it keeps the old one and must not accept
   writes).

**In-flight statements.** The *placement* of a bucket, which a distributed scan
and a fan-out write compare between the node that plans them and every node
they use, is now the group *and the node that is the primary of the group*. A
scan that was planned against the old primary fails with `KS001`
("the bucket map changed ..."), and so does a write that fans out; the client
repeats the statement and it runs against the new primary. A statement routed to
one group that reaches the old primary finds it demoted and gets `KS001` as
well, and the node that sent it reloads its catalog and repeats it. A distributed
transaction that had already done work on the old primary is not rescued: its
commit fails or, if the old primary is gone, its prepared transaction is resolved
by recovery as below.

**Calling it.** On the map group primary: `SELECT kingres.promote_node(<node id of
the standby>)`. A repeated call for a node that is already the primary only
raises a notice; the outcome is read from `kingres.node` (the role of the node)
and from the absence of an error (the function is one transaction: it either
happened or it did not).

**The map group.** The catalog can only be written on the map group primary, and
after its failover the only node that can write it is the promoted standby, while
the catalogs of the other nodes still name the dead map primary. So for a standby
of group 0 the function runs **on the promoted node itself**. It refuses unless the
node is out of recovery, is a copy of the old map primary (its `local_node` is
that node's), and listens on the port of the row it is about to become (the
function cannot ask the operating system more than that: the control plane must
call it on the instance it promoted). It writes its catalog, then pushes it, with
the cluster secret it holds from replication, to every other node it can reach;
those nodes learn the new map group primary from the catalog they receive. Until
then a statement that needs the map group fails (it is not answered wrongly). A
node that could not be reached keeps naming the old map primary and is repaired by
`kingres.sync_catalog()` on the new one.

### Prepared transactions across a failover

The identifier of a two-phase transaction is now
`kingres_g<origin group>_<timeline>_<pid>_<xid>_<target node>`, not
`kingres_<origin node>_...`: it names the origin **group**, so the node that became
the primary of the group resolves what the old primary started, and the timeline
the origin was on. The decision log `kingres.dtx_log` is an ordinary table: the
promoted standby has it, up to what was replicated.

Recovery on a node resolves the transactions of its group (it does nothing on a
node that the catalog does not call a primary). For a transaction of an *earlier
timeline* it takes "the origin transaction is no longer in progress" as given (the
backend that ran it was on the primary that is gone, and its transaction id may
have been used again on the new timeline), and goes straight to the decision log,
looked up with a fresh snapshot, the ordering rule of the 0.2 fix round: a decision
row means commit, no row means rollback. A rollback of this kind is written to
`kingres.dtx_incident` and shown by `check_cluster()` as
`prepared_rolled_back_after_failover` (kept 30 days).

**Upgrade.** A transaction that 0.4 prepared and that is still pending when a node
is upgraded has the old identifier. Both formats are resolved: the old one names a
node, and the group is the group of that node's row, which a failover leaves in the
catalog (as a standby). `t/051_gid_formats.pl` covers both, and a failover with an
old identifier.

**What is guaranteed, and what is not.**

- *With a synchronous standby* (`synchronous_standby_names` of the origin's group
  names a standby that is streaming; ANY 1 is enough if the promoted standby is
  the most advanced of them, as the control plane chooses): the commit of the origin
  is acknowledged by the standby before the first `COMMIT PREPARED` is sent
  (the extension raises `synchronous_commit` to `on` for these transactions), and
  `PREPARE TRANSACTION` of a participant is acknowledged by the participant's own
  synchronous standby, when its group has one, before the origin decides. So after a
  failover of the origin's group the outcome of a distributed transaction is the same
  on every node: committed where the decision reached the promoted node, rolled back
  everywhere otherwise (tested at every point of the protocol, `t/050`); after a
  failover of a participant's group a prepared transaction is on the promoted node,
  and is committed or rolled back by the origin's decision as usual.
- *With an asynchronous standby*, or when the group runs without its synchronous
  standby (the control plane removes it from `synchronous_standby_names` after a
  grace period, so that the group keeps accepting writes): the primary can commit
  and tell some participants to commit before the commit record has reached the
  standby. If that primary is lost, the promoted standby has **no decision** for the
  transaction, the participants that were still prepared are rolled back, and the
  ones that had committed stay committed: **a half-committed transaction**.
  Likewise the local part of that transaction is lost with the standby's missing WAL
  while the participants keep theirs. The extension cannot prevent this (it is the
  loss that asynchronous replication allows) and cannot always see it: an operator
  sees a `WARNING` in the log of the promoted node, a row in `kingres.dtx_incident`
  and `prepared_rolled_back_after_failover` in `check_cluster()` for each prepared
  transaction that was rolled back for lack of a decision; the rows name the gid, so
  that the other participants can be inspected (`t/050`, last scenario). A
  transaction whose participants had *all* already committed, and whose decision was
  lost with the old primary, leaves no trace: the participants committed, the
  origin's part is gone.
- A participant whose *own* primary fails over after it prepared, with an
  asynchronous standby that did not receive the `PREPARE`, loses its part of a
  transaction that the origin then commits. This is not detected either.
- Writes acknowledged to a client by a primary whose standby did not have them are
  lost as in any asynchronous replication; nothing about two-phase commit changes
  that.

### Distributed deadlock detection

The recovery worker of the map group primary runs one pass every
`kingres_shard.deadlock_check_interval` seconds (default 2; 0 disables): it
calls `kingres.detect_distributed_deadlocks()`.

- Every remote session that a transaction opens is labelled, for the length of
  its remote transaction, with the application name
  `ks-dtx:<origin node>:<origin backend pid>:<start of the origin transaction>`;
  any other backend is its own origin. `kingres.internal_wait_edges()` reports,
  on each primary, the waits older than `deadlock_timeout` as edges
  "transaction A waits for transaction B" (from `pg_locks` and
  `pg_blocking_pids`, with the start of the wait).
- The pass collects the edges of every primary; if they contain a cycle it waits
  `kingres_shard.deadlock_confirm_delay` (200 ms) and collects again.
- **No false positive:** the edges were read at different moments on different
  nodes, so a cycle in one reading may never have existed. A cycle is acted on only
  if every one of its edges is in both readings as *the very same wait*: the same
  backend, in the wait that began at the same time, blocked by the same transaction.
  A wait is one uninterrupted period, so each such edge existed during the whole
  interval between the two readings, and all of them at the same time: a
  deadlock, which nothing resolves by itself. A cycle whose edges come from one
  node only is PostgreSQL's (it resolves it at `deadlock_timeout`); the detector
  acts only on cycles with edges of two or more nodes. What the argument does not
  cover: a transaction that releases a lock and takes it again between the readings
  (an advisory lock, a rolled-back savepoint) while the waiter stays queued behind
  it; and the instant between the check and the cancel on the node. The victim is
  cancelled only if its backend is still in the very wait that was confirmed.
- The victim is the **youngest transaction of the cycle** (the latest start). Its
  wait in the cycle is cancelled where it is, on its origin node or on the node
  whose lock it waits for (the error goes back to the origin). It receives
  **SQLSTATE `40P01`** (`deadlock_detected`), message `distributed deadlock
  detected`: class 40 is "transaction rollback", which drivers and ORMs already
  retry, and it is the code PostgreSQL gives for a local deadlock, which is what
  this is to the client. The error is made from a query cancel by `emit_log_hook`
  of the library; it needs `log_min_messages` to be `error` or lower (the default
  is `warning`); with a higher setting the victim still ends, with the plain "canceling
  statement due to user request" (57014).
- Not covered: waits that are not lock waits (a transaction waiting for another to
  finish a `COPY`, advisory locks held across nodes by sessions that do not belong to
  one distributed transaction), and cycles through a prepared transaction (they end
  by themselves).

### `kingres.create_restore_point(name text)`

Map group primary, superuser. Returns `(node_id, group_id, restore_lsn)` for every
primary.

1. It raises a **barrier** on every primary: a transaction that is about to
   prepare its remote writers waits there (up to the time the barrier expires;
   the barrier expires by itself after `2 x node_timeout + 10` seconds, so a
   coordinator that died cannot stop commits for ever).
2. It waits until the number of distributed transactions that had passed the
   barrier (between their first `PREPARE TRANSACTION` and the end of their second
   phase) is zero on every node, for at most `node_timeout` seconds; once it is zero
   on a node it stays zero while the barrier is up.
3. It calls `pg_create_restore_point(name)` on every primary.
4. It lowers the barrier and checks that it was up the whole time on every node (if
   it was not, the restore point is not consistent and the function says so).

**Restoring every server to that name** (`recovery_target_name = name`) gives a
state in which no distributed transaction is half committed: every transaction
either finished its second phase on every node before the restore point of that
node, or started after the restore point of every node. (Tested with a restore of
three servers made under a multi-node write load: no transaction is prepared, every
transaction has all its rows or none, and later transactions are missing; `t/053`.)

What it does not give:

- **Standbys.** Only primaries get a restore point. A standby is rebuilt from the
  restored primary; a standby that streams from the restored server continues
  from its own, later position and has to be recreated.
- **A bucket move in progress.** The restore point does not wait for a move. A
  restore to it brings back the move as it was at that moment (a record of the
  move in `kingres.bucket_move`, the replication slot of the move does not exist
  any more); run `kingres.resume_bucket_moves()` (the recovery worker does) which undoes
  it or finishes it. A switch is itself a two-phase transaction and is either before
  or after the restore point on every node.
- **Sequences.** A sequence advances outside transactions and its state is in the
  WAL at the moment of the restore point on each node: the values handed out after
  that point are handed out again after the restore. `nextval` values are not
  protected (as in any PostgreSQL point-in-time recovery).
- **Time.** The restore points of the nodes are made at slightly different moments
  (one after the other, with no distributed commit between them); statements that
  touch one node only and commit between the first and the last restore point
  are in the restored state of one node and not of another, which is not a
  half-committed transaction. A node with `wal_level = minimal` refuses
  (`pg_create_restore_point` needs `replica` or `logical`).
- Transactions whose second phase failed (a participant not reached: "recovery will
  commit it") are over for the barrier; their prepared transactions are in the
  restored state and are resolved by `kingres.recover_prepared_transactions()` after
  the restore, in the way their decision log says.

### What the control plane calls

Registering a standby (after it streams): on the map group primary,
`SELECT kingres.add_node('<host>', <port>, <group_id>, 'standby')`; the result is
the node id (stored by the control plane). Removing it:
`SELECT kingres.remove_node(<node id>)`.

Failover of an ordinary group *g* (the old primary is fenced, the standby
promoted in PostgreSQL and out of recovery):

1. on the map group primary: `SELECT kingres.promote_node(<standby node id>)`;
   success is "no error"; the roles in `kingres.node` show the result;
2. on the new primary of the group: `SET kingres_shard.node_timeout = 3; SELECT
   kingres.recover_prepared_transactions()` (the worker does it every
   `recovery_interval` too), which resolves what the old primary left prepared;
3. on the map group primary: `SELECT * FROM kingres.check_cluster()` is expected
   to show only the old primary as `standby_unreachable` (until it is rebuilt) and
   possibly `prepared_rolled_back_after_failover` (see above).

Failover of the map group: promote the standby of group 0 in PostgreSQL, then **on
that node**: `SELECT kingres.promote_node(<its node id>)`; then
`SELECT kingres.recover_prepared_transactions()` and `SELECT kingres.sync_catalog()`
on it (the second one repairs a node that could not be reached in the first). The
nodes that were not reachable keep naming the old map primary until then.

The old primary, when its server returns, is rebuilt as a standby of the new
primary (`pg_rewind` or a new base backup) before it accepts a connection; it stays
registered as the standby that it has become.
