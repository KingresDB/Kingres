<!-- SPDX-License-Identifier: AGPL-3.0-only -->
# kingres_shard

`kingres_shard` is the sharding extension of KingresDB. It turns ordinary
tables into hash-distributed tables placed across several PostgreSQL
instances ("nodes", grouped into "shard groups"), keeps small tables on every
node as reference tables, and makes every statement correct on a cluster with
several groups: a statement that touches one key goes to the group that owns
it, and a statement that spans groups is answered by reading the other groups.

This is version 0.8 (0.8 is distributed schemas, phase 11, and elasticity, phase 10; 0.7 is the table lifecycle of phase 9 on top of 0.6; 0.6 is the version of phase 2b; 0.3 added moving buckets, phase 2a made every statement correct, 0.4 is the fix round of the reviews of 0.2 and 0.3, 0.5 is high availability: standby nodes, `promote_node`, prepared transactions across a failover, distributed deadlock detection and coordinated restore points; see "High availability" below; 0.6 is phase 2b and adds no SQL object, see "Installation" for why the version is raised). Phase 2b makes the common
statements fast. The designs are in
[`kingres/docs/design/shard-mvp.md`](../../kingres/docs/design/shard-mvp.md)
(phase 1) and
[`kingres/docs/design/shard-phase2a.md`](../../kingres/docs/design/shard-phase2a.md)
(this version, with its "Implementation notes and deviations"); this file
describes what is implemented and how to use it.

Version 0.7 (phase 9, [`shard-phase9.md`](../../kingres/docs/design/shard-phase9.md),
increments 9.1 and 9.2) starts the table lifecycle: `create_distributed_table` and
`create_reference_table` accept a table that already holds rows and carry its owner,
privileges, comments, sequences and foreign keys; `kingres.table_change_report()` says what
stands in the way of a change of a table before it is tried; and the catalog holds what the
rest of the phase needs (see "The table lifecycle (version 0.7)"). Increment 9.4 adds the blocking
`undistribute_table` and `alter_distributed_table`. The online variants of those two and user partitions are
the next increments and are not in this version.

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
  - an `INSERT ... SELECT` whose target and sources are colocated and joined on the
    distribution key is shipped whole to every group, which reads and writes its own
    buckets (no row passes through this node); `ON CONFLICT DO UPDATE` is allowed
    there ("Colocated INSERT ... SELECT" below);
  - any other `INSERT` whose rows belong to several groups (a multi-row `VALUES`,
    an `INSERT ... SELECT` that is not colocated, a single row whose key is
    computed) and `COPY ... FROM` are run by the row router, which sends each row
    to the group that owns its bucket in batches ("Bulk writes through the row
    router" below);
  - anything else (an `UPDATE` or `DELETE` that cannot be shipped, an insert that the
    router does not take) is refused with a "not supported yet" error that says what
    to change.
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
- **Pushdown gives the single node's answer, and the scan is always the fallback.** A read that spans shard
  groups may be run by the groups (a closed query, a partial aggregation, the rows of a join, a sorted stream
  with a limit, the functions of a listed extension: see the sections of phase 2b below). Every such path
  returns the rows the single node returns, in the order the statement asks for (the same set of rows, the
  same order where `ORDER BY` fixes one, the same errors for a negative or `NULL` `LIMIT`), with the exceptions
  that "Differences from a single node that pushdown makes visible" lists: a floating point aggregate, an
  aggregate over values that compare equal and print differently, a cursor that cannot scan backward, `SET ROLE`
  to a role that cannot log in, and a `KS001` that the client repeats. A statement that
  no path accepts, or whose pushed path costs more, runs through the distributed scan as before, and every
  switch (`enable_pushdown`, `enable_pushdown_closed`, `enable_partial_aggregation`, `enable_pushdown_sort`,
  `enable_pushdown_join`, `shippable_extensions = 'none'`) turns a path off without changing an answer.
  What is **not** promised: a global snapshot (see above: the groups of a pushed statement read their own
  snapshots, so the skew above applies to a pushed statement as well), and the last digits of a floating
  point aggregate or the order of the elements of `array_agg` and `string_agg` without `ORDER BY` (the
  states are combined in the order the groups answer, as in a parallel plan).
- **A group does what it is allowed to do, whoever asks.** A fragment is a text with a marker
  (`kingres:query-fragment`, `kingres:partial-fragment`) that a group parses and checks against its own
  rules again; a text that is forged, or has a marker the statement does not fit, is refused with `0A000`,
  and a placement or an extension version that differs from the sender's is `KS001`. A fragment runs with the
  rights of the session role that this node opened on the group (never with the rights of the node), a
  statement over a table with row-level security is not shipped (the scan answers), a statement that reads a view
  of another owner is refused with an error (on every path: the view's owner has no rights on another node), a
  function of a user is never shipped, and the session of a group runs with `search_path = pg_catalog`. See "Security
  notes".
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
  `check_cluster()` reports a node of another major as `server_major_differs`. A node
  that is upgraded in place to another major and restarted (`pg_upgrade`) is still
  registered; the catalog records no major, so a node learns what another node runs
  where it connects to it (review finding S6-09): **a connection from a node to a node
  of another major is refused**, with `node host:port runs PostgreSQL 19, this node runs
  PostgreSQL 18`, and with it every statement that needs that node: a distributed scan
  that has buckets there, a routed or fanned-out write, a write to a reference table,
  DDL, the two-phase commit and its recovery on a new connection. Statements that need
  only the nodes of the same major keep working, and `check_cluster()` (which asks over
  control sessions that are not refused) names the node. The library of a node is built
  for one major, so a node cannot disagree with itself. **How an operator upgrades a
  cluster from 18 to 19 is not designed yet.** What is certain: do not leave nodes of two
  majors running side by side except for the short time of an upgrade of the whole
  cluster that has stopped the traffic; to go from 18 to 19, build a new cluster of 19 nodes and move the
  data (the supported way). A rolling in-place
  upgrade, a move between the majors and a standby of another major (impossible with
  physical replication) are not supported.
- On every node: `shared_preload_libraries = 'kingres_shard'` (the library
  installs hooks and a background worker; `CREATE EXTENSION` fails otherwise)
  and `max_prepared_transactions` > 0 (at least the number of concurrent
  multi-node transactions).
- Nodes connect to each other with libpq as the current user to the current
  database, so every node needs a role of that name, and `pg_hba.conf` (and the
  password file, if used) must let the nodes in. Credentials are never stored
  in the catalog. SSL options are set with `kingres_shard.node_conninfo`.
- `CREATE EXTENSION kingres_shard` installs version 0.6;
  `ALTER EXTENSION kingres_shard UPDATE` upgrades a 0.1, 0.2, 0.3, 0.4 or 0.5 installation
  (the chain is 0.1 -> 0.2 -> 0.3 -> 0.4 -> 0.5 -> 0.6, and the catalog is identical to a
  fresh 0.6 one; the `upgrade` regression test compares the two). Transactions that 0.4
  prepared and that are pending at the upgrade are resolved in the old identifier format
  (see "Prepared transactions across a failover").
- **Version 0.6 adds no table, view, function or column.** `kingres_shard--0.5--0.6.sql` holds
  one statement, which makes the check of the node versions before a bucket move ask for 0.6
  (the function exists already; only its text names the version). The version is raised
  because the nodes of a cluster must run the same protocol between them (the fragments that
  phase 2b sends), and `add_node`, the placement check and `move_bucket()` refuse a node that
  runs another version.
- **Rolling upgrade from 0.5 to 0.6.** Nodes of 0.6 and nodes of 0.5 cannot cooperate on the
  new fragments. Until the last node is upgraded, set `kingres_shard.enable_pushdown`,
  `kingres_shard.enable_bulk_routing` and `kingres_shard.enable_copy_routing` to `off` on
  every node (per session or in `postgresql.conf`), and set them back to `on` when the last
  node is upgraded. Of these, `enable_bulk_routing` and `enable_copy_routing` exist today;
  `enable_pushdown` comes with the step of phase 2b that adds pushdown, and until then there
  is nothing to switch off for it. Then upgrade as described under "Upgrade order" below.
- **Changes made to the 0.5 scripts in place.** 0.5 is not released, so its install script and its
  `0.4--0.5` script change as the reviews find things, and the version stays 0.5 (0.6 does not repair such a database either). A database that
  installed an earlier build of 0.5 has no update path to the later one (`ALTER EXTENSION ... UPDATE`
  does nothing for the same version). What changed in place, and what such a database needs:
  `kingres.pending_node.node_id` (added for `remove_node`, S11-05). Without it `add_node`,
  `remove_node` and the recovery pass fail with `42703`, "the table kingres.pending_node has no column
  node_id", and the hint is the statement to run on that node: `ALTER TABLE kingres.pending_node ADD
  COLUMN node_id pg_catalog.int4;` (`t/104`). Nothing else of 0.5 needs a statement; the library
  changes (the SQLSTATE that a node gives when it refuses a proof, S12-05) need the library of the
  new build on every node, which the upgrade order above already requires.
- **The schema `kingres` is shared** with `kingres_columnar`, `kingres_ledger` and
  `kingres_timepart` (their control files say `schema = kingres`, which makes the schema a
  member of the extension that creates it). The five KingresDB extensions can be created
  and dropped in one database in any order: the install script creates the schema only
  when it is missing and takes it out of the extension, so it exists afterwards whoever
  made it, `DROP EXTENSION kingres_shard` leaves it (empty if nothing else uses it; drop it
  by hand then), the objects of `kingres_shard` are members of the extension as before,
  and `pg_dump` and restore work (the dump has `CREATE SCHEMA kingres` and the
  `CREATE EXTENSION` lines; `t/060_shared_schema.pl` runs every order, with the other
  extensions that are installed). If another extension made the schema it is a member of
  that one: dropping that extension is its own business.
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
  extension version 0.6, and names the node that does not. An older library does not
  accept the session of a newer one (the proof of a node-to-node session differs between
  versions), which looks like a lost connection; `check_cluster()` and `move_bucket()`
  ask for the version over a session that claims nothing when that happens, so they name
  both versions (`extension_version_differs`, "has kingres_shard 0.4, a bucket move needs
  0.6") and do not call the node unreachable (review finding S6-06). A node that cannot
  be asked either is `node_unreachable`.

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
  `bucket_for`, `bucket_lookup` (the bucket of boundary values, NULL and values of every
  supported key type, under every collation and for a value of another width than the
  column, and the bucket in the plan for a constant; its expected file was recorded
  before the bucket lookup was moved into one place, so it fails if any bucket moves),
  `distribute`, `routing_single`; `security`: the search_path and
  `SECURITY DEFINER` scan of every function, the temporary-schema attack, the
  routing switches; `utility_guard`: utility statements on a cluster with a
  second, fictitious shard group; `routing_decisions`: collations, functions
  and other nested contexts, forwarded sessions, decided with `EXPLAIN`;
  `reference`: reference tables on one node; `upgrade`: `ALTER EXTENSION ...
  UPDATE` from 0.1, 0.2, 0.3, 0.4, 0.5, 0.6 and 0.7 against a fresh 0.8 install; the same on both majors:
  the script of 0.3 names a column of `pg_stat_subscription_stats` that PostgreSQL 19
  renamed, so on 19 it creates `kingres.internal_move_subscription_status()` with its
  body check off and `ALTER EXTENSION ... UPDATE` to 0.4 replaces it with a body that
  reads the counter on both majors; 0.3 itself is not usable on 19). The scripts put the
  session's `check_function_bodies` back after that one function (S6-07). The snapshot
  that the test compares covers columns (with defaults, identity, generation, collation,
  storage and privileges), constraints, indexes, views and their definitions, relations
  with owners and options, sequences with their parameters, functions with a digest of
  their bodies, owners and dependencies, triggers, event triggers with tags and owner,
  policies, comments, privileges of the schema, the dumped configuration and the members.
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
  Fix round for the review of the high availability in the extension (S7): `061_sync_decision` (the
  second phase is not sent when the wait for the synchronous standby was cancelled, terminated or cut
  by a shutdown), `062_participant_sync` (the participant's `PREPARE`, `COMMIT PREPARED` and recovery wait
  for its standby whatever its `synchronous_commit`), `063_catalog_term` (an old map primary that comes
  back, two promotions in a row, an unreachable primary that comes back, the map group from a minority
  and a majority), `064_promote_resume` (the map group primary killed at every fault point of
  `promote_node`), `065_deadlock_identity` (forged labels and a flood of them, a malformed label, the
  mark of a victim), `066_restore_cancel` (the barrier after a timeout, a cancel, a termination),
  `067_gid_tag` (forged identifiers, the old forms, the length), `068_participant_failover` (a
  participant that lost its part), `069_inflight_promotion` (a cursor and its rescan, `COPY` and a transaction across a
  promotion).
  Fix round for the second review of 0.5 (S8): `070_participant_durability` (every way in which a
  participant's wait for its standby can end early, the bound of a stall, a smart shutdown, the
  second phase and recovery), `071_promote_rules` (the list of standbys that a promoted node
  inherits, the majority of the other groups, one to four groups, the refusals), `072_rotate_and_applied`
  (rotation with a transaction in doubt, the cleanup of `dtx_applied`), `073_sequences` (planned
  switchover, the way back, removing a dead standby, choosing among standbys),
  `074_move_tablesync_hold` (a bucket move whose table-sync worker is held: cancelled while held, target restarted, slot lost, target down).
  Fix round for the review of the port to 18 and 19 (S6): `056_ri_distributed` (foreign keys
  from distributed tables to reference tables, every action and every combination, during
  a move), `057_scan_errors` (errors caught by savepoints and `EXCEPTION` clauses during a
  scan, cursors, thousands of repetitions), `058_review_s6` (inheritance of a reference
  table, `session_replication_role`, the version handshake, `check_function_bodies`,
  a connection to a node of another major), `059_restore_node` (every step of "Restoring a
  node"), `060_shared_schema` (every order of creating and dropping the extensions that share
  the schema `kingres`, and a dump and restore).
  Foundation of phase 2b (no change of behaviour): `098_foundation` (nothing but
  `hashing.c` computes a bucket, and nothing but `pg_compat.h` tests the version of
  PostgreSQL; the check is tested on snippets it must refuse; the settings listed for a
  partial fragment exist on the server) and `099_harness_selftest` (every helper of the
  comparison harness is shown to fail when it should).
  Fix round for the fourth review of the call sequences (S12): `100_sweep_calls` and
  `101_sweep_promote` (every control call against a node behind a forwarder that goes silent after
  every chunk of its sessions, from the first to the last; every answer is judged by the standard of
  a control call; `KsSweep.pm` is the judge), `102_slow_nodes` and `103_slow_promote` (every call
  against one, two and four nodes that answer every statement at 0.5 and 0.9 x `node_timeout`, with
  the bound of the README as a formula, and a large table copied), `104_s12_fixes` (the column that
  was added to 0.5 in place, the two refusals of a proof told apart by their SQLSTATE, `25001`, a
  lock wait on a node with and without a `lock_timeout` of the role). `KsProxy.pm` is the forwarder
  (`swallow_after`, `delay_turn`, `chunk_log`, `turn_log`).
  Bulk writes of phase 2b (steps 3 and 4; the design's `056`, `057`, `058`, `064`, `066` and `067`
  are taken by tests of 0.5, so the parts are named here): the regression test `bulk_plans` (the plans,
  the switches, the settings, every refusal, the privilege matrix of `INSERT` and `COPY FROM`),
  `112_row_router` (the router part of 056 and the insert part of 058: every form of `INSERT`, compared
  with a single node and with the same rows inserted one statement at a time; `RETURNING`, `ON
  CONFLICT DO NOTHING`, defaults, identity and generated columns, constraints, every type under every
  session profile, savepoints), `113_router_faults` (the router part of 064: an error and a cancel at every
  point and wait of the sender, a node that does not read and one that does not answer, a group that dies,
  an origin that dies before and after its commit), `114_router_security` (the router part of 066 and the
  batch and log part of 067: the privileges of the caller on every node, a marker is a comment, no value
  in a statement text or an error, the number of batches, hostile names), `115_copy_routing` (057 and the
  copy part of 058: every format and option of `COPY FROM`), `116_router_moves` (a bucket that moves,
  and a group that changes its primary, during a bulk write) and `117_bulk_memory` (the memory of a load
  of millions of rows is a constant: peak private memory of the origin and of the groups' sessions).
  `118_review_steps_1_2` (the fixes of the review of steps 1 and 2: the one-shot mark of a local part cannot be
  read by SQL code, the memory of thousands of rescans, restrictions that deparse with `COLLATE`, set operations with
  an arm that contradicts itself, inequalities never prune, forged fragments are refused with `0A000`) and
  `119_nested_subxacts` (the fixes of the sixth review of the call sequences: two nested subtransactions with the
  connection closed or answered with an error at every chunk, a failed `RELEASE` that PL/pgSQL catches, a read-only
  session that does not answer `ROLLBACK TO`, the memory of thousands of failed `RELEASE`s).
  The online variants (increment 9.3): the regression test `online_refusals` (every refusal before anything is
  recorded, with its message, and the privileges of the new functions); `144_online_distribute` (three nodes: a
  table with rows, an owner, privileges, comments, a serial column, an identity column, a key to a reference table and
  indexes, distributed in one call and phase by phase; the digests, the sequences, the old copy; a reference table),
  `145_online_load` (four writers with inserts, updates, deletes and `COPY` throughout; the table equals a model),
  `146_online_interleavings` (changes between the read of a chunk and its insert, a writer that waits for the lock of the
  switch, a prepared transaction and a long reader that delay it, `session_replication_role = replica`, `COPY`, a role with no
  privilege on the log), `147_online_faults` (an error at every point before and after the switch, the session killed, the
  map group primary crashed inside every step and inside the commit of the swap, a node lost during the copy: the table
  is whole in exactly one layout and nothing is left), `148_online_cancel_exclusion` (cancel in every phase, resume by
  another session, a move against an operation, two operations, a prepared statement across the swap, the version
  handshake), `149_online_verification` (a change behind the trigger, money and other types, verification off, a write
  after the switch, a reference table).
  Table lifecycle (version 0.7, increments 9.1 and 9.2; the design's `t/040` and the numbers after it are
  taken by older tests, so the names are `140` and up): the regression test `distribute_rows` (the report, one
  statement per code, and a creation with rows on one node compared object by object with the source), the
  steps from 0.6 in `upgrade`, the new objects in `security` and `catalog`; `140_distribute_rows` (the blocking
  `t/040`: three nodes, every row in the group that owns its bucket and nowhere else, owner, privileges, comments,
  sequences and keys the same on every node, writers wait and readers do not, an error at every step and every send and
  wait of the sender, a cancel, a node that is down, the origin killed before and after its commit),
  `141_distribute_memory` (a million and three million rows: the same peak memory), `143_distribute_insel` (a prepared colocated INSERT ... SELECT across the swap of its source, and one that waits for the lock of a table that is being distributed), `142_distribute_ledger` (a
  ledger table is refused; it needs `kingres_ledger` built into the same installation, for example
  `make -C contrib/kingres_ledger USE_PGXS=1 PG_CONFIG=... install`, and both libraries preloaded, which the test does).
  `180_untrusted_code` .. `184_engine_rules` (the fixes of the second review of phase 9: code of untrusted roles through domains,
  operators, range types and hash functions; float, domain and very large keys in the capture, a unique value that changes hands in the
  replay and the verification that says it was not done; the guard against `DROP TRIGGER`, `SET SCHEMA` and the like and the lock of a
  `GRANT`; a cancelled call whose session stays; a column privilege on the old copy, a repeated copy chunk, the row lock of the switch,
  the worker and a live or recent operation, the log name of the capture trigger, a lost answer of the switch),
  `187_online_undistribute` and `188_online_alter` (increment 9.4, part two: the online variants for a distributed table, a writer on another node at every point), `185_undistribute` and `186_alter` (increment 9.4: a table with every kind of object undistributed and compared with its definition before,
  a reference table, keys with and without cascade, another target group, the refusals, an error at every step, a writer that waits;
  the catalog-only cases of alter, the rebuilds with their placement, cascade over a group, the refusals and the faults),
  `150_distribute_races` (the fix round of the review of 9.1 and 9.2: a writer that commits while the call waits for the lock, for an
  update, a delete, an insert, a reference table and the online engine, with the values compared; statements queued behind the call;
  rules, row types, `NOT VALID` checks, unowned sequences, collations, schema privileges, enum keys, function owners, invalid indexes, forced
  row-level security, default privileges and the other refusals before anything changes).
  `t/KingresShardTest.pm` builds clusters (and standbys); `t/KingresShardCompare.pm` is the
  comparison harness: it loads the same data into a three-node cluster and a
  single node and compares query results (a NULL is shown as `<NULL>`, so it never equals
  an empty string), and repeats every query under four
  non-default session profiles (`DateStyle`, `IntervalStyle`,
  `extra_float_digits`, `TimeZone`, `bytea_output`, `lc_*`; `profiles => 0`
  switches that off for one query). Later phases add queries to the list in
  `013_dist_scan.pl`. For the pushdown paths of phase 2b it also runs a query under plan
  profiles (`plans => ['scan', 'pushed', 'nopartial']`: every result must equal the single
  node's and the row-moving scan's), asserts the plan (`path => 'Kingres Fragment Scan'`, so
  a test cannot pass because nothing was pushed), normalizes floats and the elements of
  aggregated arrays and strings (`normalize`), loads data with `COPY FROM STDIN`
  (`copy_in`), compares a whole table after a write (`compare_tables`) and generates
  queries from templates with a seed that is printed (`KS_SEED`).

## Quick start

```sql
-- on the first node (it becomes the map group primary, node 1)
CREATE EXTENSION kingres_shard;
SELECT kingres.init_cluster('host1', 5432);       -- the address the other nodes use

-- add nodes (each new primary is a new shard group)
SELECT kingres.add_node('host2', 5432);
SELECT kingres.add_node('host3', 5432);

-- distribute a table (empty or not), and keep a small one everywhere
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
| `kingres.promote_node(node_id int) returns jsonb` | Superuser. Makes a promoted standby the primary of its group and the former primary a standby, in one two-phase transaction; raises the map version and the catalog term; resumable; skips the nodes it cannot reach and lists them. Refuses, changing nothing, when the node that commits cannot get its synchronous standbys to confirm. Answers `{"status": "promoted"|"completed"|"already", "term", "unreached"}`. On the map group primary, for group 0 on the promoted node itself. See "High availability". |
| `kingres.catalog_status` (view) | The catalog term of this node, whether it has been superseded as the map group primary, and its map version. For superusers; exists on every node. |
| `kingres.promote_map_group_without_majority(node_id int, acknowledgement text) returns jsonb` | Superuser, on the promoted standby of the map group. The operator's way out when the majority rule refuses for ever; needs the exact acknowledgement text (README, failover of the map group); the catalog term jumps by 1000. |
| `kingres.standby_name_matches(standby_name text) returns bool` | Superuser, on the primary of a group: would an element of `synchronous_standby_names` with this name match a streaming standby of this primary? Decided as PostgreSQL decides: names compared without regard to case, `*` matches every standby, the name may be written as in the list (double quotes). The check before a name goes into the list. |
| `kingres.switchover_mark() returns pg_lsn` | Superuser, on the old primary of a planned switchover, after the writes stopped: writes and flushes a WAL record and returns its position. |
| `kingres.switchover_replayed(mark_lsn pg_lsn) returns bool` | Superuser, on the new primary: has this node replayed up to the mark? **Only that**: it says nothing about what the old primary wrote after the mark, so a `t` is not "nothing is lost". |
| `kingres.switchover_verify(shutdown_checkpoint pg_lsn, cluster_state text) returns bool` | Superuser, on the new primary, asked **before** `pg_promote()` (after it the answer can turn into `KS006` once NEW has recycled the segment of the record, sequence 5): the proof that a planned switchover loses nothing. Total: for every `pg_lsn` it returns `t`/`f` or raises `KS006` (a position below the first segment is refused like any position that no record can start at, `t/094`). The arguments are "Latest checkpoint location" and "Database cluster state" of `pg_controldata` of the stopped old primary. `t` only when the state is exactly `shut down` and this node's own WAL holds a shutdown checkpoint at the position, replayed as the last record; `f`: not replayed yet; error `KS006`: it can never be proven (treat the promotion as a failover). See sequence 5. |
| `kingres.create_restore_point(name text) returns table(node_id, group_id, restore_lsn)` | Map group primary, superuser. A restore point on every primary with no distributed transaction half way through its commit. |
| `kingres.standby_status() returns table(...)` | Map group primary, superuser. The standbys with their replication state as their primary sees it. |
| `kingres.detect_distributed_deadlocks() returns int` | Map group primary, superuser. One pass of the distributed deadlock detector (the recovery worker runs it); returns the number of transactions cancelled. |
| `kingres.sync_catalog()` | Push the catalog to every active primary node, atomically. |
| `kingres.rotate_secret()` | Map group primary, superuser. Replaces the secret behind the tokens of node-to-node sessions on every active primary, atomically. Refuses while a distributed transaction of the cluster is prepared on any node (its tag would stop verifying). Sessions another node opens while the commit is in flight may be refused once; repeat the statement. A node that dies between the PREPARE and the COMMIT PREPARED of the rotation is reached by the node that finishes it with the secret that the rotation replaced (every node keeps it as "previous" and never accepts a session with it); recovery commits the rotation by itself and drops the previous secret in the first pass of a node's recovery (the worker, or `recover_prepared_transactions()`, on that node) that reaches every primary with the current secret and needs no session made with the previous one: the pass that commits a stranded node's rotation needs it and keeps it, the next pass drops it (S9-03, `t/076` asserts both). |
| `kingres.create_distributed_table(relation regclass, dist_column name, colocate_with regclass default null, bucket_count int default null)` | Map group primary only; superuser. The table may hold rows. See "The table lifecycle (version 0.7)". |
| `kingres.create_reference_table(relation regclass)` | Map group primary only; superuser. Makes an ordinary table a reference table: creates it on every node, copies its rows, and carries its owner, privileges, comments and its foreign keys to other reference tables. Writers are blocked while it copies. |
| `kingres.undistribute_table(relation regclass, cascade_via_foreign_keys bool default false, target_group int default null) returns void` | Map group primary only; superuser. Turns a distributed or a reference table back into an ordinary table on the primary of `target_group` (default: the map group), with the tables that foreign keys link to it when `cascade_via_foreign_keys` is true. One transaction, writers wait; see "Undistribute and alter (increment 9.4)". |
| `kingres.alter_distributed_table(relation regclass, distribution_column name default null, bucket_count int default null, colocate_with text default null, cascade_to_colocated bool default false) returns void` | Map group primary only; superuser. Changes the distribution column, the bucket count or the colocation group (`'default'`, `'none'` or the name of a distributed table) of a distributed table, and of the tables of its group with `cascade_to_colocated`. One transaction, writers wait; see "Undistribute and alter (increment 9.4)". |
| `kingres.table_change_report(kind text, relation regclass, options jsonb default '{}') returns table (severity text, code text, object text, detail text)` | Superuser, any node that has the catalog. What prevents a change of a table (`blocker`), what the change would not carry (`warning`) and what is good to know (`note`), without changing anything; it never stops at the first problem. `kind` is `distribute`, `distribute_reference`, `undistribute`, `alter`, `create_partition`, `attach_partition`, `detach_partition` or `drop_partition`. The codes are listed under "The table lifecycle (version 0.7)". |
| `kingres.recover_prepared_transactions() returns int` | Commits or rolls back the prepared transactions this node originated, on every active primary (itself included); returns how many it resolved. Superuser. Runs in a `READ COMMITTED` transaction; not on a standby. |
| `kingres.bucket_for(relation regclass, value anyelement) returns int` | The bucket of a key of a distributed table. |
| `kingres.bucket_for(colocation_id int, value anyelement) returns int` | The bucket of a key in a colocation group. |

Views: `kingres.nodes` (`kingres.node` plus `is_local`) and
`kingres.buckets` (table, bucket, partition name, group, owning node, state,
and the partition size where the partition exists on this node).

`create_distributed_table` takes an ordinary, permanent, table (or one partitioned by RANGE or LIST: see "User-partitioned tables")
that may hold rows, without inheritance, triggers, rules, row-level security (enabled, forced or with a
policy) or exclusion constraints, on a type with a default hash operator
class and extended hash function. Every unique constraint or unique index must
include the distribution column; the error names the offending object. The
table is recreated as a hash-partitioned table with the same columns,
defaults, identity and generated columns, `NOT NULL`, check, unique and foreign key
constraints and indexes, the sequences owned by its columns (each node's state is raised to the largest value of the
column on that node, and the state of this node is kept when it is higher), its
owner, its table and column privileges and its comments, and its rows are
moved to the groups that own their buckets. The colocation group is the one of
`colocate_with`, else an existing group with the same type, collation and
bucket count, else a new group whose buckets are spread round-robin over the
groups that should hold shards. Statistics objects, per-column storage, compression
and statistics targets, the attribute options of a column, `CLUSTER ON` and the replica identity are not carried (the
report says so as warnings). The privileges of the new table are exactly those of the old one: the default privileges of
the schema (`ALTER DEFAULT PRIVILEGES`) are not added to it. Everything that prevents the call is listed in one error
(or by `table_change_report()`); see the next section.

`create_reference_table` takes the same kind of table (a unique constraint needs no
distribution column). Its rows, owner, privileges and comments are carried to the other
nodes, and so are its foreign keys to other reference tables.

## The table lifecycle (version 0.7)

This section is phase 9 of [`shard-phase9.md`](../../kingres/docs/design/shard-phase9.md),
increments 9.1, 9.2 and 9.3 (what the catalog and the report are, the blocking
`create_distributed_table` of a table with rows, and the rebuild engine with
`create_distributed_table_concurrently` and `create_reference_table_concurrently`). The design
calls its version "0.6"; the phase is 0.7 because phase 2b is 0.6 (a version with no object
change). Increment 9.4 adds `undistribute_table` and `alter_distributed_table` (blocking). Not in this version: their online
variants, user partitions of distributed tables, the control plane and the console. `table_change_report` already
reports for those kinds what it can know today (see the codes).

### What a creation with rows does

One transaction, the caller's, on the map group primary:

1. The cluster lock, the check that this node is the map group primary, the **version
   handshake** (`kingres.internal_require_version('0.7')`: every active primary must report
   0.7, the first that does not is named), the report (any blocker raises one error that lists
   all of them), the choice of the colocation group (a new group takes its id from
   `kingres.colocation_id_seq`, so a number is never used twice, also after a rollback), and the
   check that no bucket of the group is being moved and no table operation claims it.
2. `LOCK TABLE ... IN EXCLUSIVE MODE` on the source. From here to the commit **writers of the table wait,
   readers do not**; everything else goes on.
3. The new table is made on every node **under the name `kingres_shadow_<oid>`** (its constraints and
   indexes carry the oid too), with the partitions of the buckets the node's group owns (named `<table>_b<NNNN>`
   at once, with the modulus of the bucket). It is not in the catalog of any node, so no statement, scan or
   move sees it. The old table is still the table.
4. The rows are copied with the batch sender of the row router (`copy_routed.c`) driven with an explicit
   destination (a placement, because the shadow is not registered): the source is read through a cursor in
   chunks and every row goes to the group that owns its bucket, in batches of one `INSERT` that carry the
   identity of the destination's partitions. Rows read, rows the groups acknowledged and `count(*)` of the
   source taken under the lock must be equal, or the call fails.
5. From the moment the old table is dropped to the commit **readers of the table wait** too (`DROP TABLE` takes
   `ACCESS EXCLUSIVE`): on this node the old table is dropped and the shadow takes its name (constraints, indexes,
   identity sequences and the sequences that columns own are renamed or moved), the foreign keys are added, the
   sequences are advanced (each node's to at least the largest value of its own rows, and this node's to the state
   the source had), and the owner, privileges and comments are applied; on every other node the shadow takes its
   name and gets the same. The table is registered and the catalog is pushed to every node.
6. The commit is the two-phase commit of phase 2a: `PREPARE TRANSACTION` on every node that holds a part, the
   decision in `kingres.dtx_log`, `COMMIT PREPARED`. A failure of any node or of this node at any point before the
   decision rolls everything back (the old table is as it was, with its rows, and no node has a shadow, a
   partition or a prepared transaction after recovery); a crash after the decision is finished by
   `recover_prepared_transactions()` or the recovery worker, and the table is whole.

Fault points of the creation (`kingres_shard.fault_injection`): `distribute_after_lock`,
`distribute_after_shadow`, `copy_before_read`, `copy_after_copy`, `distribute_before_swap`,
`distribute_after_swap`, and the points of the batch sender (`bulk_*`) and of the commit protocol.

Memory does not depend on the number of rows: the cursor chunk is `insert_batch_rows` rows, the sender
keeps at most two buffers per group (`insert_batch_rows` rows or `insert_batch_bytes` bytes each), and a group
runs one batch at a time (`t/141` distributes 1,000,000 and 3,000,000 rows with 4 MB of shared buffers: the
peak resident size of the backend is the same to the megabyte).

### Transactions that run at the same time

The call reads the rows after the lock that stops the writers is granted, with a snapshot taken then: what a transaction
committed while the call waited for the lock (an update, a delete, an insert) is in the new table. For that the call
runs only in a `READ COMMITTED` transaction and is refused in `REPEATABLE READ` and `SERIALIZABLE`, whose snapshot is older
than the lock. A statement that was started before the call and waits for the table (`ALTER TABLE`, `RENAME`, `CREATE INDEX`,
`TRUNCATE`, `GRANT`, `COPY ... FROM`, `DROP TABLE` and the others the hook looks at) first waits for the lock and decides
afterwards, so it finds the table as the call left it and goes to every node. The statement takes a `ROW EXCLUSIVE` lock
on the table for this, which every writer holds anyway. Only a role that has the right the statement needs takes it (the owner; `INSERT` for
`COPY ... FROM`; `TRUNCATE`; `TRIGGER` for `CREATE TRIGGER`; a grant option for `GRANT`), so a `GRANT` of a role that may only read
leaves no lock behind. The online variants take the copy from a snapshot after the capture trigger exists. The trigger is made under
a lock that excludes writers: the call tries without waiting for a quarter of `rebuild_switch_timeout` (2.5 s by default), then queues once for
`rebuild_lock_timeout`, and fails with the holders named, and is undone, when a writer (or a prepared transaction) is still in the table. A
table that always has a transaction open for longer than that cannot be rebuilt online with the call; the phase functions can be called
again.

### What is carried and what stays refused

| | |
|---|---|
| Columns, defaults, identity (also `GENERATED ALWAYS`: copied with `OVERRIDING SYSTEM VALUE`), generated columns, collations, `NOT NULL` (with its constraint name), check, unique and primary key constraints, every index (expression, partial, covering) | carried |
| Sequences owned by columns | carried: the same object on this node, created with the same options and the same owner on the others; the state is kept on this node when it is above the largest value of the column, otherwise raised to that value (`is_called` true), and on the other nodes raised to the largest value of the node's own rows. So the next value of a serial column can be higher than it was when the sequence was behind its column. Privileges on the sequences are not carried. **A sequence is per node, as before:** after the distribution the nodes start from different numbers (the largest value of each node's own rows), so a serial or identity column hands out the same value on two nodes, and the table's primary key (which contains the distribution column) is what keeps the rows apart; a value that repeats is harmless only when the repeated value goes to a different distribution key. Do not rely on a distributed serial column for a value that must be unique across the table |
| Owner, table privileges, column privileges (with grantors and grant options), comments on the table, columns, constraints and indexes | carried; the roles must exist on every node (blocker `role_missing`) |
| Foreign keys **to a reference table** | carried and validated in the same transaction |
| Foreign keys **to a distributed table** of the group the table joins, on the distribution column of both | carried (such a key exists on an ordinary table only while the cluster has one shard group, or when it was made with `enable_routing = off`: with several groups the hook refuses it at creation) |
| A key from the table to itself | carried when it is on the distribution column on both sides; refused otherwise |
| Foreign keys to an ordinary table, to a distributed table of another group, or without the distribution column; foreign keys from an ordinary table to the table | refused (`foreign_key_plain_table`, `foreign_key_other_group`, `foreign_key_not_distribution_column`, `referenced_by_plain_table`) |
| Triggers, rules, row-level security (also `FORCE ROW LEVEL SECURITY` alone), exclusion constraints, invalid indexes, `CHECK` constraints that are `NOT VALID`, inheritance, partitioned tables and partitions (user partitions are increment 9.5), temporary and unlogged tables, tablespaces, ledger tables and the history table of one, a table on a node that is not the map group primary | refused |
| Views, materialized views, publications and functions that use the table's row type, and a column of another table that has the row type | refused (`dependent_object`): the table is replaced by a new one, and a rename would leave them on the old |
| A column default that calls `nextval()` of a sequence that no column of the table owns; a collation that another node does not have; an owner who has `CREATE` on the schema here but not on another node; an enum as the distribution column (the hash of an enum value depends on the OID of the value, which differs between the nodes); a relation of the shadow's name on another node; any function that the copy would reach and that belongs to a role who is not a superuser: one a `CHECK`, default, generated column or index names, and also one behind a domain of a column (its checks and its default), an operator or operator class of an index, a cast, a range or composite type (the copy runs it with the rights of the caller on every node, and the code of a table's owner must not run as a superuser) | refused before anything is done (`default_sequence_not_owned`, `collation_missing`, `schema_privilege`, `enum_distribution_column`, `name_in_use`, `function_owner_not_superuser`) |
| Statistics objects, per-column storage, compression and statistics target, attribute options, `CLUSTER ON`, replica identity | not carried (warning `not_carried`) |
| `lc_monetary` | the values of a `money` column are converted under `lc_monetary = 'C'` on both ends of the copy |

The old table is dropped in the same transaction after the counts have matched; the design keeps it for
`table_cleanup_delay` (increment 9.3 does, for the online variants). For a blocking creation the transaction
is the guarantee: until it commits nothing changed, and after it the old copy would only be a second copy of
rows that writers may have changed since.

### The codes of `table_change_report`

A code is a stable word; `object` names what is wrong, `detail` is a sentence (the text of the error when it is the only
blocker). Severity `blocker`: the call is refused. `warning`: carried differently or not at all. `note`: good to know.

| Code | Kinds | Meaning |
|---|---|---|
| `not_on_map_group` | all | this node is not the map group primary, or the cluster is not initialized |
| `already_distributed` | distribute, distribute_reference | the table is distributed or a reference table already |
| `not_ordinary_table` | distribute, distribute_reference | a view, a sequence, a foreign table, ... |
| `partitioned_table` | distribute, distribute_reference | a partition on its own, a table partitioned by HASH, or a partitioned reference table |
| `subpartitioned`, `foreign_partition`, `partition_local_object`, `tablespace` | distribute | a partition that is itself partitioned or is not an ordinary table; a partition with an index, a constraint, a default or a storage parameter of its own, or in another tablespace |
| `default_partition`, `distribution_column_is_partition_key` (warnings) | distribute | the table has a default partition (attaching a partition scans it in every bucket on every node); the distribution column is the partition key |
| `temporary_or_unlogged`, `inheritance`, `row_level_security`, `exclusion_constraint`, `trigger`, `rule`, `invalid_index`, `check_not_valid` | distribute, distribute_reference | the table has what stays refused (one row per constraint and per trigger) |
| `tablespace` | distribute, distribute_reference | the table or one of its indexes is not in the default tablespace |
| `ledger_table` | distribute, distribute_reference | a `kingres_ledger` table or the history table of one |
| `dependent_object` | distribute, distribute_reference | a view, materialized view, publication or function that uses the table, or a column of another table with its row type |
| `default_sequence_not_owned`, `function_owner_not_superuser` | distribute, distribute_reference | a default calls `nextval()` of a sequence that no column of the table owns; code of a role who is not a superuser is reached by the copy through a constraint, default, index, domain, operator, operator class, cast, range or composite type (the report says through what) |
| `enum_distribution_column`, `collation_missing`, `schema_privilege` | distribute | the hash of the column is not the same on every node; another node lacks a collation of a column or `CREATE` on the schema for the owner |
| `foreign_key_plain_table`, `foreign_key_other_group`, `foreign_key_not_distribution_column`, `referenced_by_plain_table`, `referenced_by_distributed_table` | distribute, distribute_reference | the foreign keys that stay refused |
| `column_missing`, `no_hash_support`, `unique_without_distribution_column`, `name_too_long`, `name_in_use` | distribute | the distribution column is not usable, a unique key lacks it, the name leaves no room for `_bNNNN`, or a relation of a bucket partition's name or of the shadow's name exists, on this node or on another |
| `no_key_for_online` | distribute with `mode = 'online'` | a primary key or a unique index over `NOT NULL` columns is needed (for increment 9.3) |
| `group_busy` | distribute, undistribute, alter | the colocation group has an unfinished move or a table operation |
| `role_missing`, `default_function_missing`, `node_unreachable` | distribute, distribute_reference | another node lacks a role of the table's owner or privileges, or a function of a column default; or it cannot be asked |
| `not_carried` (warning), `rows` (note) | distribute, distribute_reference | what is not carried; the estimate of the rows that writers wait for |
| `not_distributed`, `foreign_key_linked`, `target_group_invalid`, `group_busy`, `dependent_object` | undistribute | the table is not distributed or a reference table; a foreign key links it to another table of the catalog (undistribute them together with `cascade_via_foreign_keys`, or drop the key; a key from a distributed table to a reference table is kept); the target group has no active primary; a move or an operation uses the group; a view (or another object) uses a table that the call drops |
| `not_distributed`, `bucket_count_invalid`, `colocate_with_not_distributed`, `foreign_key_colocation`, `unique_without_distribution_column`, `column_missing`, `no_hash_support`, `group_busy` | alter | the table is not distributed; the bucket count is out of range; the table to colocate with is not distributed; a key depends on the colocation of two tables (pass `cascade_to_colocated`); a unique index lacks the new distribution column; the column does not exist or cannot be hashed; a move or an operation uses the group |
| `not_distributed`, `name_too_long`, `not_available_yet` | create_partition, attach_partition, detach_partition, drop_partition | user partitions are not in this version |

`options` is what the call would be given: `column`, `colocate_with` (a table name, schema-qualified unless it is in `pg_catalog`
or `kingres`: the report runs with a fixed `search_path`), `bucket_count`, `mode` for a distribution;
`cascade_via_foreign_keys` and `target_group` for undistribute; `distribution_column`, `bucket_count`, `colocate_with` and
`cascade_to_colocated` for alter; `partition` for the partition kinds.

### Undistribute and alter (increment 9.4)

`undistribute_table(relation, cascade_via_foreign_keys, target_group)` and `alter_distributed_table(relation, distribution_column,
bucket_count, colocate_with, cascade_to_colocated)` are the blocking variants of design/shard-phase9.md, "Feature 2" and "Feature 3". Each
is one transaction, the caller's, and the commit is the two-phase commit of phase 2a: an error, a cancel or a failure of a node at any
step leaves every table as it was (fault points `undistribute_after_lock`, `_after_copy`, `_after_drop`, `_after_swap` and the same
four for `alter_distributed_table`). The report (kinds `undistribute` and `alter`) runs first and one error lists every blocker.
Writers of the tables wait from the first lock to the commit (`EXCLUSIVE` on the table on every node); readers go on until the old
table is dropped, which is the last step before the commit. Both functions run on the map group primary and refuse elsewhere.

**Undistribute.** A distributed table is copied through the distributed scan into an ordinary table on the primary of `target_group`
(the map group when null; the data must fit there), the distributed table is dropped on every node, and the new table takes its name,
constraints, indexes, identity and owned sequences (their state is raised to the largest value of the rows), owner, privileges and
comments; the table leaves the catalog and a colocation group without a table is removed (its id is never used again). A reference
table stays on the target node (the same table, so views, grants and keys that depend on it stay) and is dropped on the others;
its copies are not compared, the copy of the target node is the one that stays. A foreign key from a distributed table to a reference
table is kept and the reference table stays a reference table; a key that links two tables of the catalog needs
`cascade_via_foreign_keys => true`, which takes every table of the connected component (distributed and reference) in one transaction and
recreates the keys on the ordinary tables. A view or anything else that depends on a table stops the call (blocker `dependent_object`).

**Alter.** `colocate_with => 'none'` (the table leaves its group for a group of its own with the same placement) and
`colocate_with => <table>` or `'default'` when the other group has the same type, collation, bucket count and placement change the
catalog only: no table is touched. Anything else is a rebuild: another bucket count, distribution column or placement. The new tables are
made on every node in a new colocation group (or the group of `colocate_with`), with the partitions named as a new table would name them (the
old partitions and their indexes are renamed out of the way and dropped with the old table); the rows are read through the distributed scan
and sent to their new buckets; the old tables are dropped and the new ones take their names; a group left without a table is removed. When the
new bucket count is a multiple of the old one, new bucket `b` is placed on the group of old bucket `b mod old count`. Without
`cascade_to_colocated` only the table changes and it leaves its group (refused with `foreign_key_colocation` when a key depends on the
colocation); with it every table of the group changes together (the distribution column must exist, with the same type, in every one).

**Limits of this increment.** All rows of a rebuild pass through the session of the map group primary (a distributed scan there, the batch
sender to the destinations): the cost is the size of the table, not what it moves between nodes. Writers wait for the whole copy; the online
variants follow below. The state of the sequences of the nodes that lose the table is dropped with it; a serial or identity column of an undistributed table
continues above its rows and above the state of the map group's sequence. `t/185` and `t/186`.

### The online variants for a distributed table (increment 9.4, part two)

`kingres.undistribute_table_concurrently(relation, cascade_via_foreign_keys, target_group)` and
`kingres.alter_distributed_table_concurrently(relation, distribution_column, bucket_count, colocate_with, cascade_to_colocated)` run the
engine of the online variants above with a distributed table as the source: writers on every node go on while the rows are copied.
Map group primary only; superuser; the same refusals as for the blocking variants, with these additions: the table needs a primary key or a
unique index over NOT NULL columns (the capture names a row by it); it must have no foreign key to or from another hash-distributed table
(a key to a reference table is carried over; tables linked to each other, `cascade_via_foreign_keys` and `cascade_to_colocated => true` are the
blocking variants' work: the error says so); a request that moves no rows (a change of the catalog only) is the blocking function's and the
online call returns no operation. Without `cascade_to_colocated` the table leaves its group alone, as in the blocking variant.

How it works. The operation has a row on the map group primary and a mirror row on every other node, so that the node-local functions of the
engine run unchanged on each node with the local table. On every node holding partitions: a capture trigger (`ENABLE ALWAYS`) and a log with the
exact key of every changed row; a copy of every partition into the destination (alter: shadow partitions `kingres_shadow_<oid>_bNNNN` of the new
group, each bucket on the node where its rows already are when the bucket count is a multiple of the old one; undistribute: one shadow table on
`target_node`); a replay per node that reads the current row for every logged key. The switch is one transaction: the catalog lock, the locks of the table
on every node, a last replay on every node, the swap, and on every node the retirement of the old partitions (they are renamed, not dropped).
A table that exists on one node only stays the same for a writer on that node. Undo (an error, a cancel, a kill of the call) and the verification
(a digest of the new table against the old copies, with the rule on rows written after the swap) work per node.

Bounded waits (the same settings as the single-node variants). The capture triggers are taken with `NOWAIT` in a loop for
`kingres_shard.rebuild_switch_timeout / 4`, then queued for `kingres_shard.rebuild_lock_timeout`; the locks of a switch attempt are queued for
at most `rebuild_lock_timeout` on each node and an attempt that fails is retried until `rebuild_switch_timeout`; every call to another node is
bounded by `kingres_shard.node_timeout`. The untrusted-code rule applies before any copy: the call is refused with `function_owner_not_superuser`
when a default, a check, an index expression, a trigger or a generated column of the table calls a function of a role who is not a superuser.
The DDL guard of the single-node variants is on every node (a mirror row is what the guard reads): `ALTER TABLE`, `DROP TRIGGER`, `RENAME`,
`CREATE INDEX` and `TRUNCATE` on the table are refused on every node while the operation runs.

After the switch. For `undistribute` the table is an ordinary table on `target_node` and is gone from the others: a statement of a writer on another node that comes after the
switch fails with `relation ... does not exist` and writes nothing; one that came before the switch is in the new table. The old copies stay on
every node for `kingres_shard.table_cleanup_delay`; `kingres.cleanup_table_operations()` drops them on every node. An error after the switch
(`rebuild_verify`, `rebuild_cleanup`) leaves the operation in `cleaning`; another call to `table_operation_advance` finishes it.
Fault points: the ones of the single-node variants at every phase, and `rebuild_switch_after_local_swap`, `rebuild_switch_after_remote_swap`,
`rebuild_switch_after_prepare` in the switch. Tests: `t/187_online_undistribute` and `t/188_online_alter` (a table of every kind of object
while it is written from another node, a writer racing each of eleven points, an error at each point, refusals, the guard on every node,
cancel, float keys written with `extra_float_digits = 0`).

### User-partitioned tables (increment 9.5)

`create_distributed_table` takes a table that the user partitioned by RANGE or LIST (one level; ordinary partitions with nothing of their own: no
index, constraint, default or storage parameter that the table lacks; at most `kingres_shard.max_partition_leaves` leaves, the bucket count times the
number of partitions; the blocking variant only, the online variants refuse it with `not_available_yet`). The layout is hash on top and the user's
partitioning below: every node has the parent; a bucket partition `<table>_bNNNN` exists on the group that owns it and is itself
`PARTITION BY <method> (<key>)`; inside it a leaf `<partition>_bNNNN` per user partition, with the bound of the partition (`DEFAULT` too). A user
partition is a row of `kingres.dist_partition` (`parent`, `partition_name`, `bound`), not a table: `SELECT * FROM <partition>` does not work, query the
table with the key condition (the planner prunes both levels). `kingres.dist_table.partition_method` and `partition_key` say how the table is
partitioned; `kingres.partitions` lists the partitions with the size of their leaves on this node. The rows are copied from the parent and routed by
PostgreSQL's own tuple routing at the owner; a row that fits no partition cannot exist in the source and would fail loudly. Writers of the table wait for
the copy, as for any blocking creation. `t/189_partitioned_distribute` (a RANGE table with a default partition, a LIST table, the refusals, an error at
every fault point of the creation).

**Creating, dropping, detaching and attaching partitions.** Four functions (map group primary, superuser; one two-phase transaction on every
node under the catalog lock; refused with `object_in_use` while a move or a table operation uses the group, so that a caller can retry):

| Function | What happens |
|---|---|
| `kingres.create_partition(parent regclass, partition_name text, bound text, if_not_exists bool default false)` | One leaf `<partition_name>_bNNNN` per bucket, made like the bucket partition (`LIKE ... INCLUDING ALL`, same owner) and attached with the bound, which is the text that follows `FOR VALUES` (`FROM (...) TO (...)`, `IN (...)`) or `DEFAULT`; a bound with `;`, `--` or `/*` is refused. `ATTACH` locks each bucket partition `SHARE UPDATE EXCLUSIVE`: readers and writers go on; a default partition is scanned and locked `ACCESS EXCLUSIVE` until the commit, so writers of its rows wait. |
| `kingres.drop_partition(parent regclass, partition_name text, missing_ok bool default false)` | The leaves go on every node, and the row. |
| `kingres.detach_partition(parent regclass, partition_name text)` | The partition becomes a distributed table of the same group and distribution column (a hash parent `<partition_name>` made like the table, the leaves moved under it); no row moves. Refused when the table has foreign keys. |
| `kingres.attach_partition(parent regclass, partition regclass, bound text)` | A distributed table of the same group, on the same distribution column, in the schema of the table, without keys, becomes a partition: its bucket partitions become leaves; no row moves. An ordinary table is not attached by this function: create the partition and insert its rows. |

The statements are mapped by the ProcessUtility hook (on the map group primary, routing on): `CREATE TABLE s PARTITION OF p FOR VALUES ...` calls
`create_partition` (constants, casts of constants, `MINVALUE`, `MAXVALUE` and `DEFAULT` are written back; any other expression is refused with the hint
to call the function; sub-partitioning, storage parameters, a tablespace, an access method and columns are refused), `ALTER TABLE p ATTACH PARTITION t`
calls `attach_partition` (an ordinary table is refused), `ALTER TABLE p DETACH PARTITION s` calls `detach_partition` (`CONCURRENTLY` is refused) and
`DROP TABLE s` of a name that is a partition calls `drop_partition` (a drop of several names, with `CASCADE`, is left to PostgreSQL, which refuses
it because the name is no table). `ALTER TABLE s`, `CREATE INDEX ON s` and `TRUNCATE s` of a partition name fail with PostgreSQL's "does not exist":
run them on the table. A user-partitioned table cannot be undistributed or altered in this version (`not_available_yet`). Fault points:
`partition_after_lock`, `partition_after_leaves`. Tests: `t/190_partition_functions` (every function, the refusals, an error at each fault point, a
writer on another node racing a held `create_partition`, a busy group) and `t/191_partition_statements` (the statements).

`kingres.partition_leaves(parent regclass, partition_name text)` returns `(node_id, bucket, leaf)` for every leaf of a partition, from the catalog; `check_cluster()`
reports `dist_partition_mismatch` (node, leaf) for a leaf that the catalog lists and the node that owns its bucket does not have. `t/192_partition_catalog`.

**Moving a bucket of a user-partitioned table.** The bucket partition `<table>_bNNNN` is a tree, and the move moves the tree: the copy on the target is
made with the same key and one leaf per user partition, each with its bound, its owner and its access method (read from the source, so a leaf that was
converted keeps its format); the check that makes the attach at the switch free of a scan is on the root and is inherited by the leaves; the publication
of the source names the bucket partition and publishes through it (`publish_via_partition_root`, only when a table of the group is user-partitioned),
so the number of table-sync workers does not grow with the number of leaves; the switch attaches and detaches the root as before. The orphan that the
source keeps is the detached tree: its leaves and their indexes get names of the orphan and go to schema `kingres` with it, so that a bucket that comes
back finds the names `<partition>_bNNNN` free, and dropping the orphan drops them. `kingres.buckets.partition_size` is the size of all the leaves. A
partition that is created while a move runs is refused by the guard of the group. `check_cluster()` does not list an orphan that is a partitioned table
(`orphan_table`); the cleanup drops it. `t/193_partitioned_move` (the move, the tree and its bounds, the orphan with its leaves, the move back at once, a
writer on another node during the move, an error at three points, the publication through the root).

**Queries.** The planner prunes both levels (the bucket partition by the key, the leaf by the interval); the pushdown, the fragment scan and the
distributed scan take a user-partitioned table as they take a plain one, and the answers are those of the same table on one node, from every node and
after every kind of change (`t/194_partitioned_queries`). No change of the planner or of the pushdown was needed.

### Dependent views (increment 9.8)

A table operation replaces the table by a new one, and a view is bound to a table by its OID, so by default a view that uses the table stops the
operation (`dependent_object`, the view named). With `kingres_shard.dependent_views = 'recreate'` (a session setting; the online operations keep the value
they were started with) the plain views that use the table, and the views that use those, are recreated: in the same transaction (the same locks) as the
swap they are dropped, last first, and after the swap they are made again on the new table, first first, with the same definition (`pg_get_viewdef`), column
names, owner, options (`security_barrier`, `security_invoker`, `check_option`), privileges (with the grant option, and `PUBLIC`) and comments (view and
columns). It applies to `create_distributed_table`, `alter_distributed_table`, `undistribute_table` and the three online operations. A view that exists on
another node than the map group primary (a view is local to the node where it was created) is recreated there; a distributed table is checked on every node.
Refused, before anything is made (the report says so with `dependent_object`, and names the node): a materialized view that uses the table or a view; a view with a
trigger, with a rule of its own, with privileges on a column, one that a function uses the row type of, or that a publication publishes; and, for an
undistribute, a view on a node other than the one that keeps the table (the table will not be there). The default `refuse` is unchanged. A view over a
distributed table that is owned by another role than the session role is refused by the rule of the scan unless it is `security_invoker`; the recreated view has
the owner and options it had. `t/195_dependent_views` (the default, every operation, views on views, a view on another node, privileges, owner, options, comments,
the refusals, an error at four points of the switch, a reader on another node that waits for the switch).

### Storage parameters (increment 9.9)

A table with storage parameters (`fillfactor`, `autovacuum_*`, `toast.*`, ...) is distributed, made a reference table, rebuilt and moved with them (they
were refused before; the report now says `note: storage_parameters`). PostgreSQL does not allow them on a partitioned table, so a distributed table has them
on its bucket partitions, and `kingres.dist_table.reloptions` (synchronized to every node) records them: the bucket partitions of the table that
`create_distributed_table` (blocking and online) makes, of the table that `alter_distributed_table` (blocking and online) makes, and the ordinary table of
`undistribute_table` (blocking and online) are made `WITH` them, and a bucket move makes its copy on the target `WITH` the parameters of the bucket partition on the
source (`LIKE ... INCLUDING ALL` does not copy them). A reference table keeps its own. Not carried: the parameters of a partition of a user-partitioned table
(`partition_local_object` refuses them as before), and a later `ALTER TABLE ... SET (...)` of a bucket partition (the record is the one of the last rebuild).
`t/196_storage_parameters` (every operation, every node, the move, a table without parameters, the refusal for a partition).

### What 0.7 adds to the catalog

`kingres.dist_table.partition_method` and `partition_key` (empty in this version), `kingres.dist_partition` (synchronized
with the rest; nothing writes it yet), the sequence `kingres.colocation_id_seq` (a colocation id is never used twice; the
update starts it at the highest id, and a node that applies a catalog keeps it above the ids it sees), and four node-local
tables, readable by superusers only: `table_operation`, `table_operation_item`, `table_operation_lock` and `table_old_copy`
(written by the online variants of increment 9.3; `internal_begin_move` and the DDL guard already refuse a colocation group
that has a row in `table_operation_lock`). The views `kingres.table_operations` and `kingres.partitions` are readable by
`pg_monitor` and show no error text and no node address. `kingres.internal_copy_routed()` copies the rows of a table to a
placement, to every node or to one node for the rebuild engine to come; `kingres.internal_require_version()` is the
handshake. All new functions are revoked from `PUBLIC`; one is `SECURITY DEFINER`:
`kingres.internal_capture_trigger()` of the online variants (below), the second of the extension after
`forget_dropped_tables`.

### Upgrading

Load the new library on every node, then run `ALTER EXTENSION kingres_shard UPDATE` on every node. Until a node has been
updated, `create_distributed_table` and `create_reference_table` refuse to run with "the kingres_shard objects of this
database are older than the library", and the handshake of the new functions names the node that is behind; the other paths
keep working, and a node can still be added. `internal_apply_catalog` accepts a catalog document without the new keys (a map
group primary that is not updated yet).

### The online variants (increment 9.3)

`kingres.create_distributed_table_concurrently(relation, dist_column, colocate_with, bucket_count) returns bigint` and
`kingres.create_reference_table_concurrently(relation) returns bigint` do what their blocking namesakes do while **writers
keep writing**. They cannot run inside a transaction block (they commit in sessions of their own), need a primary key or a
unique index over NOT NULL columns (the report says `no_key_for_online` otherwise), and return the id of the operation
(`kingres.table_operations`). Superusers, on the map group primary, in a `READ COMMITTED` session. Everything stays in 0.7.

The work is a **record** (`kingres.table_operation`, node-local, with its phase and `spec`) and phases that can be called one
by one, which is what the control plane does:

| Phase | What happens | Function |
|---|---|---|
| `preparing` | `kingres.begin_table_operation(kind, relation, options)`: the report (every blocker, `no_key_for_online` too), the version handshake, the colocation group (a new one is made and announced), its claim in `kingres.table_operation_lock` (a group with an operation, an unfinished move or a bucket in motion is refused), the record. `kind` is `distribute` or `distribute_reference`; `options` has `column`, `colocate_with` (the OID as text), `bucket_count`, and optionally `request_id` (a second call with it returns the same operation). Committed at once. | `begin_table_operation` |
| `shadow` | On every node, in a session of its own, the shadow of the table (`kingres_shadow_<oid>`, partitions of the node's buckets under their final names, the same statements as the blocking variant); then on the source, under `SHARE ROW EXCLUSIVE`, the **log table** `kingres.tlog_<op>_1` and the **capture trigger** `AFTER INSERT OR UPDATE OR DELETE ... ENABLE ALWAYS` (so `session_replication_role = replica` does not bypass it) and a `BEFORE TRUNCATE` trigger that refuses. | `table_operation_advance(op, 'shadow')` |
| `copying` | The source is read in key order in chunks of `kingres_shard.rebuild_batch_rows` (one transaction each, with the last key recorded in the same transaction, so a copy resumes after it) and sent with the batch sender to the group of each bucket, `ON CONFLICT DO NOTHING`. | `table_operation_advance(op, 'copying')` |
| `catching_up` | The log is replayed in chunks: for each key it names, the key is deleted at the destination and the row, if it exists, is inserted (the destination is made equal to the **current** row, so the order and the number of times do not matter); the entries are removed in the same transaction. It ends when the backlog is under `kingres_shard.rebuild_drain_threshold`. | `table_operation_advance(op, 'catching_up')` |
| `switching` | One bounded attempt in one transaction: the catalog lock, `EXCLUSIVE` on the source (**writers wait, readers go on**; a lock is tried without waiting for a quarter of `rebuild_switch_timeout`, then queued once for `rebuild_lock_timeout`), the replay of what is left, the check that the log is empty, then `ACCESS EXCLUSIVE` and the swap, on every node in the two-phase commit of 2a: the old table moves to the schema `kingres` (`kingres_old_<oid>`, readable by its owner only, its keys dropped, its indexes renamed), the shadow takes its name, sequences, owner, privileges and comments, the foreign keys are added `NOT VALID`, the table is recorded and the catalog pushed. An attempt that cannot get a lock in time (a long reader, a prepared transaction: the detail names the holders) or whose replay takes too long leaves everything as it was; after `rebuild_switch_max_attempts` attempts the call returns with `detail` set and can be called again. | `table_operation_advance(op, 'switching')` |
| `cleaning` | The old copy is compared with the new table (see below), the foreign keys are validated, the claim is released, the operation is `done`. The old copy is dropped after `kingres_shard.table_cleanup_delay`. | `table_operation_advance(op, 'cleaning')`, `cleanup_table_operations()` |

`kingres.table_operation_advance(op_id, to_phase, timeout_ms)` runs the operation up to the end of `to_phase` and returns the
phase it is in; any phase can be called again after a failure or a crash, by another session too; the session that runs it holds a live
mark (an advisory lock) and a second session that tries gets "is being run by another session". **Cancel**:
`kingres.cancel_table_operation(op)` undoes an operation that has not switched (triggers, log, shadows, claim, a colocation group the
operation made); a session that runs it notices at the end of its chunk and undoes it itself; after the switch it only says so. **Resume**:
`kingres.resume_table_operations()` (the recovery worker calls it every `recovery_interval`, after it resolved the prepared transactions) undoes
the operations whose session is gone and that did not switch (not written for `2 x node_timeout`), finishes those that switched, and cleans
again an undone operation that could not reach a node (`spec -> 'cleanup_pending'`). The two wrapper functions undo what fails, a cancel of the call (Ctrl-C, a statement timeout, `pg_cancel_backend`) included, whether or not the client stays connected: the live mark of the operation is released and the undo runs before the cancel is reported.

**The proof that nothing is missed.** The capture trigger exists, on a source that no writer is half inside (the trigger is made under a lock
that excludes writers), before the copy reads anything. Every row that exists is copied; every change after has a log entry, written in the
writer's own transaction, so it commits or rolls back with the change. The replay sets the destination to the current state of each logged key.
The switch takes the lock that waits for every earlier writer and stops later ones, then replays again **after the lock is granted** and
checks that a statement that runs after it sees an empty log: no writer can add to it now, so the shadow is the table. The swap runs in the
same locked state. What this does not cover is a change that bypasses the triggers (`ALTER TABLE ... DISABLE TRIGGER` by a superuser who edits
the catalog); the DDL guard refuses that statement on the table of an operation, and the verification detects the rest.

**The verification** (`kingres_shard.rebuild_verify`, on): in `cleaning`, `kingres.internal_table_digest()` returns the number of rows and
the sum of a hash of every row in text form (under fixed `DateStyle`, `IntervalStyle`, `extra_float_digits`, `bytea_output`, `lc_monetary`
and `TimeZone`) for the old copy and for the new table on every node. A difference keeps the old copy, sets `verified = 'failed'`, makes
the call fail and is reported by `check_cluster()` (`table_verification_failed`). A table that was **written to after the swap** cannot be
compared (its rows have changed): the digest also counts the rows whose `xmin` is newer than the swap of that node, and a difference with
newer rows is `verified = 'skipped'` with the reason in `detail`, and the call that ends the operation raises a `WARNING` with it (also when
the tables differ: the warning then says so). So the check is exact for a table nobody wrote since the swap, and **one write after the swap
hides any loss**: a table that is written to constantly is never verified by this check, and the capture is then the only guard.
Compare the retained old copy with the new table yourself in that case (the warning says so); the old copy is kept for
`table_cleanup_delay`. A workload that only deletes in that window can give a false `failed`, which only keeps the old copy.
The capture stores the keys with all their digits (it reads the key columns of the row, not a text of the row; the sessions of the engine
run with `extra_float_digits = 3` like every control session) and in the base type of a
domain, never as a jsonb of the whole row, so a float key, a domain key or a value over 256 MB does not change what is replayed or what a
writer may write. A replay chunk that fails on a unique index other than the key (a unique value that moved from one row to another between
two chunks) is repeated with the whole backlog.

**What else changes.** The DDL guard refuses `ALTER TABLE` (also `SET SCHEMA` and `DISABLE TRIGGER`), `RENAME`, `DROP TABLE`, `DROP INDEX`, `DROP TRIGGER`,
`DROP RULE`, `DROP POLICY`, `ALTER INDEX`, `CREATE INDEX`, `CREATE TRIGGER`, `CREATE RULE`, `CREATE POLICY` and `TRUNCATE` on the source
of a running operation ("is being rebuilt by table operation N"). A bucket move of a claimed group is refused; so is a second operation on
the group or the table. `create_distributed_table` does not choose a claimed group for a new table. `check_cluster()` also reports
`table_operation_stalled`, `shadow_table`, `log_table`, `capture_trigger` (objects of an operation that is not running), `old_table` (an
old copy overdue by an hour) and `table_verification_failed`.

**Cost to writers.** One more insert into the log per changed row while the operation runs (the table is a plain table on the map group
primary until the swap), and a wait from the first lock of the switch to its commit, bounded by the drain threshold and
`rebuild_switch_timeout`. `t/145` measures a reader during the operation (under a second) and checks the table against a model of four
writers (inserts, updates, deletes, `COPY`).

**Disk and WAL.** While an operation runs the destination servers hold a second copy of the table with its indexes (about twice the
size of the table and its indexes in total), and after the switch the retained old copy stays on the map group primary for
`table_cleanup_delay` (ten minutes by default): up to three times the size at the peak. Every row is written once more to the WAL of its
destination (and of the destination's standbys), every change during the operation is one more log row on the map group primary, and
the verification reads the old copy and the new table once. A table whose size does not fit that room twice cannot be rebuilt online.

**What the online variant does not promise.**

- A value drawn by an explicit `nextval()` of an identity or owned sequence in the few milliseconds between the read of the sequence state
  and its rename in the swap (writers cannot insert meanwhile, a call of `nextval()` alone can) is given twice (the design says so too).
- The remaining window of the statements that wait for a table that is being distributed ("Transactions that run at the same time"):
  a statement that names a plain table and a distributed table together and propagates (a foreign key from the plain table to the
  distributed one, possible only on a cluster with one shard group) lets go of the lock of the plain table before it takes the lock of
  the catalog. A call that starts on the plain table in those few instructions is not seen by the decision of that statement, which
  then runs on a table that has become partitioned. It needs a DDL statement and an administrator's call on one table at the same
  moment; the effect is on that statement, not on rows.

Fault points: `rebuild_after_preparing`, `rebuild_after_shadow`, `rebuild_after_capture`, `rebuild_copy_chunk`, `rebuild_after_copy`,
`rebuild_replay_chunk`, `rebuild_before_switch`, `rebuild_switch_after_lock`, `rebuild_switch_after_final_replay`,
`rebuild_switch_before_swap`, `rebuild_switch_after_local_swap`, `rebuild_switch_after_remote_swap`, `rebuild_switch_after_prepare`,
`rebuild_after_switch`, `rebuild_verify`, `rebuild_cleanup`, `rebuild_undo_after_triggers`, `rebuild_undo_after_shadows`; the points of the
commit protocol can be scoped to the swap with `@tswitch` (`after_prepare@tswitch:kill`).

Settings: `kingres_shard.rebuild_batch_rows`, `rebuild_drain_threshold`, `rebuild_lock_timeout`, `rebuild_switch_timeout`,
`rebuild_switch_max_attempts`, `table_cleanup_delay` and `rebuild_verify` (see "Settings").

## Restoring a node from a logical dump

`pg_dump` of a node holds that node's partitions (`COPY public.d_b0001 ...`) and the
reference tables in full, the line `CREATE EXTENSION IF NOT EXISTS kingres_shard`, and
nothing of the cluster catalog or the secret (the catalog tables are not dumped). It also
holds the tables `kingres.orphan_*` that finished bucket moves left on the node (they are
not members of the extension): leave them out with `-T 'kingres.orphan_*'` unless the old
copies are wanted. Every step below is run by `t/059_restore_node.pl` (review findings
S5-06 and S6-08). The restored server needs the library in `shared_preload_libraries` and
the files of the extension, because the dump creates the extension.

1. **A dump restored elsewhere** (inspection, extraction of data, a copy for a test): restore it
   into an empty database of a server that has the extension installed. The dump creates the
   extension there (without a catalog), the partitions come back as plain tables, the
   distributed tables as partitioned tables with only the partitions of that node, and
   the reference tables in full. Nothing of this belongs to a cluster.
2. **A node that lost its database, put back in place**: create an empty database with the same
   name on the server that has the node's old address (the catalog knows nodes by host
   and port), restore the dump into it, then run `SELECT kingres.sync_catalog()` on the map group
   primary. The restored node has the extension but no catalog, no cluster secret and no
   idea which node it is; `sync_catalog()` gives it the catalog and the secret, and its node id
   when it has none (a node that knows its id is not touched). Then `check_cluster()` finds nothing
   and the node's rows are back in the cluster. The restored data is as old as the dump:
   rows written to the node's buckets after it are lost, and what changed in the reference
   tables meanwhile is a difference that `check_cluster()` reports; a physical backup is the
   way to get a node back as it was. A dump of a node that took part in a bucket move
   after the dump was made does not fit the cluster any more (the buckets are elsewhere).
3. **The partitions of a node that is in a cluster, repaired from a dump**: the refusal of `COPY`
   into bucket partitions and of `CREATE TABLE` of a distributed table stands, so a superuser sets
   `kingres_shard.enable_routing = off` in the session of the restore (put `SET
   kingres_shard.enable_routing = off;` at the top of a plain-format dump). Dump only the partitions
   (`pg_dump --data-only --disable-triggers -t 'public.d_b*'`), not the reference tables, which
   are whole on every node and would collide; empty the damaged partitions first. The partitions
   of the node can be written then and nothing else changes. Afterwards run
   `kingres.check_cluster()`; a node that lost its secret shows as `secret_missing` and gets it
   back with `kingres.sync_catalog()`.
4. **Not supported: `add_node()` of a node that holds the tables of the cluster already.** It creates
   the tables itself and fails with `relation "d" already exists`; registering nothing. To add
   a new node, run `add_node()` on an empty database and load the data through the cluster
   (`COPY (SELECT ...)` out, `INSERT` in, or a restore on a single node that is then
   distributed with `create_distributed_table`).

## Settings

| GUC | Default | Meaning |
|---|---|---|
| `kingres_shard.default_bucket_count` | 128 | Bucket count of a new colocation group (1 to 4096). |
| `kingres_shard.enable_routing` | on | When off, the planner and utility hooks do nothing. Superusers only (`PGC_SUSET`): with routing off a `SELECT` reads the local partitions only and a `TRUNCATE` clears this node only, which an administrator who repeats a statement on every node by hand wants and an ordinary session must not be able to switch on. |
| `kingres_shard.enable_distributed_scan` | on | When off, a `SELECT` that spans shard groups raises the phase 1 error instead of reading the other groups (to forbid slow cross-group queries). |
| `kingres_shard.scan_fetch_size` | 1000 | Rows requested from another group at a time by the distributed scan. |
| `kingres_shard.enable_pushdown` | on | Master switch of the pushdown paths of a read (step 6: closed queries). Off: a read that spans shard groups is answered by the row-moving distributed scan, as before. `USERSET`; it chooses among plans that give the same answer, and grants nothing. |
| `kingres_shard.enable_pushdown_closed` | on | Off: a closed query (see "Closed queries, run whole by every group") is not shipped (the partial aggregation has its own switch); the scan answers. `USERSET`. |
| `kingres_shard.enable_partial_aggregation` | on | Off: an aggregate or a grouping that is not closed is not run in two steps (see "Partial aggregation"); the scan answers. `USERSET`. |
| `kingres_shard.enable_pushdown_sort` | on | Off: a closed query is not sorted or limited by the groups; they send the rows unordered and this node sorts and limits them (see "Sort and limit of a closed query"). `USERSET`. |
| `kingres_shard.enable_pushdown_join` | on | Off: the join of colocated tables is not run by the groups as a fragment of its own when the statement is not closed (see "Join rows"); the rows of each table come to this node. `USERSET`. |
| `kingres_shard.shippable_extensions` | all | all, none, or a list of names from the list compiled into the library (see "Objects of extensions"): which extensions' objects may be shipped to the groups. A narrowing only. `USERSET`. |
| `kingres_shard.enable_bulk_routing` | on | Off: an `INSERT` that goes to several shard groups is refused as in 0.5 (a multi-row `VALUES`, `INSERT ... SELECT`, a single row whose key is not a constant). A single row with a constant key never needed the router and is not affected. `USERSET`; it grants nothing. |
| `kingres_shard.enable_colocated_insert_select` | on | Off: a colocated `INSERT ... SELECT` is not shipped whole: it goes through the row router when it can (no `ON CONFLICT DO UPDATE`) and is refused when it cannot. `USERSET`; it chooses among statements that give the same answer, and grants nothing. A cached plan keeps the path it was made with (as for every planner setting). |
| `kingres_shard.enable_copy_routing` | on | Off: `COPY ... FROM` into a distributed table of a cluster with several groups is refused as in 0.5. `USERSET`. |
| `kingres_shard.insert_batch_rows` | 1000 | Rows per batch that the row router sends to one group (1 to 100000). |
| `kingres_shard.insert_batch_bytes` | 1MB | Text bytes per batch per group, the sum of the lengths of the values (16kB to 64MB). The memory of a bulk write is about two batches per group, whatever the number of rows. |
| `kingres_shard.dependent_views` | refuse | `refuse` or `recreate`: what a table operation does with the plain views that use the table (see "Dependent views"). `USERSET`. |
| `kingres_shard.max_partition_leaves` | 8192 | Most leaves (buckets times user partitions) one distributed table may have; a user-partitioned table over the limit is refused. `USERSET`. |
| `kingres_shard.rebuild_batch_rows` | 10000 | Rows per chunk of the copy and of the replay of an online table operation (one transaction on every group it writes). `USERSET`. |
| `kingres_shard.rebuild_drain_threshold` | 1000 | Entries of the capture log under which the switch is attempted. `USERSET`. |
| `kingres_shard.rebuild_lock_timeout` | 200ms | The longest a lock request of the switch is queued behind other sessions. `USERSET`. |
| `kingres_shard.rebuild_switch_timeout` | 10s | The longest one switch attempt lasts (locks, final replay, swap). `USERSET`. |
| `kingres_shard.rebuild_switch_max_attempts` | 30 | Switch attempts in one call of `table_operation_advance(.., 'switching')`. `USERSET`. |
| `kingres_shard.table_cleanup_delay` | 10min | How long the old copy of a replaced table is kept. 0 drops it after the check. `USERSET`. |
| `kingres_shard.rebuild_verify` | on | Compare the old copy with the new table in the cleaning phase. `USERSET`. |
| `kingres_shard.recovery_interval` | 30s | Seconds between runs of the recovery worker; 0 disables it. `sighup`. |
| `kingres_shard.deadlock_check_interval` | 2s | Seconds between passes of the distributed deadlock detector; 0 disables it. `sighup`. |
| `kingres_shard.deadlock_confirm_delay` | 200ms | Time between the two readings of the wait-for graph that confirm a cycle. `sighup`. |
| `kingres_shard.decision_retention` | 300s | How long the commit decisions of a transaction that is over are kept when the participant's group has a standby, so that the origin can find a participant that lost its part after a failover. The time counts from the moment the participant's synchronous standbys have confirmed everything it flushed (S9-01): until then the decisions are kept without limit and `check_cluster()` reports `decision_unconfirmed`. A prepared transaction with no decision that is older than this is rolled back and recorded (`rolled_back_after_retention`). The participants' marks in `kingres.dtx_applied` are kept as long as this and a minute; set it the same on every node. `sighup`. |
| `kingres_shard.node_conninfo` | empty | Connection options appended to every node-to-node connection, see below. Server configuration only (`sighup`: `postgresql.conf` or `ALTER SYSTEM`), never per role or per database. |
| `kingres_shard.node_connect_timeout` | 5s | Connection timeout between nodes. |
| `kingres_shard.control_call_timeout` | 0 (no limit) | An opt-in bound for the **whole** of a control-plane call (S13-02): a call that is still working after that many seconds ends with `KS015`, which says that the call took too long as a whole and **nothing about any node**. Settable by the caller (`SET`). It is read when the call starts, checked between the statements the call sends to nodes, and every wait is cut to what is left; with the default nothing changes for anyone. It does not cover the commit of the transaction that follows the call, and a statement that is being cut off can take the cancel time (see "Bounds that apply to every call") |
| `kingres_shard.node_timeout` | 60s | The longest the phases that cannot be left waiting may wait for a node (`COMMIT PREPARED`, the cleanup after an abort, savepoint rollbacks and releases, the recovery worker), the longest any statement of a control-plane call (`promote_node`, `add_node`, `remove_node`, `sync_catalog`, `rotate_secret`, `recover_prepared_transactions`, `create_distributed_table`) waits for a node, and the longest such a call waits for another call that changes the catalog (then `KS008`); the node's own lock wait in such a call ends at three quarters of it (README, "Bounds that apply to every call"). And, as `tcp_user_timeout` and keepalives, the time after which a connection to a node that does not answer at all is dead. The cancel request sent when a wait runs out is bounded too (a third of this value, at most `node_connect_timeout`, at least a second), so a node whose postmaster is stopped delays the committing session by `node_timeout` plus that, not until it resumes (S3-07). The recovery worker takes the value of the server configuration, whatever `ALTER DATABASE ... SET` says (S3-16); so for `node_connect_timeout`. |
| `kingres_shard.forwarded` | off | Set by the router on the sessions it opens, together with `kingres_shard.forward_token`; fixed at connection time (`PGC_BACKEND`). A client that sets it without the token is refused. See "The forwarded flag" below. |
| `kingres_shard.forward_token` | hidden | The proof of a forwarded session; never shown. |
| `kingres_shard.fault_injection` | empty | Developer setting, superuser only: fails the commit protocol at a named point (`<point>:kill`, `:error`, `:wait:<file>`, `:waitinv:<file>` (the same, and then the backend takes the invalidations that committed meanwhile: a test changes the catalog of the node behind the statement); points `before_prepare`, `after_first_prepare`, `after_prepare`, `after_log`, `after_commit`, `between_commit_prepared`; 0.5 adds `commit_waits_for_barrier`, `promote_before_change`, `promote_after_first_push`, `promote_after_push`, `restore_barrier_up`, `restore_all_quiet` and `recovery_before_resolve` (recovery has decided and is about to send COMMIT PREPARED or ROLLBACK PREPARED); `server_major:value:<n>` and `peer_major:value:<n>` make this node, or the nodes it connects to, report another PostgreSQL major to the checks that keep one major per cluster). Used by the tests; it adds no capability a superuser does not have. |

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
  `CLUSTER`, `CREATE INDEX CONCURRENTLY`, `COPY ... FROM` into a reference table or a bucket partition (into a distributed table it is routed, see "Bulk writes through the row router"), row-level security,
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
supported yet. Which foreign keys exist, and what they do, is under "Foreign keys that
point at a reference table" below. `COPY ... FROM` into a reference table is refused on a
cluster with several groups.

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
  on this node.  `COPY ... FROM` into a distributed table is run by the row router (section "Bulk writes through the row router"); into a reference table or a bucket partition it stays refused.

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
| a trigger on the reference table itself | **refused**, `CREATE TRIGGER` and `CREATE CONSTRAINT TRIGGER`, also by a superuser with `enable_routing = off` (S5-01): what a trigger reads or computes on each node (a count of a distributed table, a clock, a sequence) cannot be shown to give every copy the same row. The supported way is to put the logic in the statement that writes the table, or in a trigger on a distributed table; the foreign keys of a reference table are not affected. The statements of foreign keys (the checks and the actions) are the only ones that run on this node's rows alone while a write to a reference table runs; they are recognised by the security restriction that `ri_triggers.c` sets while it plans and runs them, not by their text (S6-01, see "Foreign keys that point at a reference table"). Any other statement that such a write makes run (a function it calls) is planned, routed and checked like a statement of its own |
| inheritance and partitions of a reference table | **refused**: `CREATE TABLE ... INHERITS (r)`, `CREATE TABLE ... PARTITION OF r`, `ALTER TABLE ... INHERIT r`, `ALTER TABLE r INHERIT ...`, `ATTACH PARTITION` in both directions, also with routing off (S6-03). A child of a reference table would take part in every write to the parent, with its triggers and rows, on each node alone. `create_reference_table()` already refused a table that takes part in inheritance |
| a write while `session_replication_role` is not `origin` | **refused** with a message that names the setting (S6-04): the role switches off triggers and foreign key actions in this session only, and is not sent to the other nodes. The same for a write to a distributed table on a cluster of several shard groups, whose rows would be checked by foreign keys in some groups and not in others. A superuser who needs the role repeats the statement on each node by hand with `kingres_shard.enable_routing = off`. One shard group: no restriction |
| a stored generated column whose expression calls a function that is declared `IMMUTABLE` and is not | **the extension trusts the declaration**, as PostgreSQL does for an index (S6-05): every node computes the column itself with that function. A function that is declared immutable and reads something of its node (a port, a setting, a table of the node) gives a different value in each copy; `check_cluster()` finds the copies that differ (`reference_table_differs`). Declare such a function `STABLE` or `VOLATILE`, and it is refused in a write |
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

### Foreign keys that point at a reference table

*Which keys can be made.* Checked when the constraint is created (`ALTER TABLE ... ADD
CONSTRAINT`, `ADD COLUMN ... REFERENCES`, `CREATE TABLE`) on a cluster of several shard
groups (with one group everything is local and PostgreSQL's own rules apply):

| from | to | |
|---|---|---|
| a distributed table | a reference table | allowed, every action |
| a reference table | a reference table (or itself) | allowed, every action |
| a distributed table | a colocated distributed table, on a key that holds the distribution column of both | allowed |
| a reference table | a distributed table | **refused** at creation ("A foreign key to a distributed table is possible only from a colocated distributed table") |
| a distributed table | a distributed table of another colocation group, or on a key without the distribution column, or a key that is not named | **refused** at creation |
| any table | a bucket partition | **refused** at creation |

Nothing that cannot be enforced is accepted: the combinations that are not in the table
as allowed are refused when the constraint is created, with a message that says why.

*What a key from a distributed table to a reference table does* (review finding S6-01).
The rows of the distributed table are in every shard group, and a write to a reference
table runs on every node, so every node fires the triggers of the key on its own
buckets, and together they see every row once. This is what one PostgreSQL server does,
in the same distributed transaction:

- `NO ACTION` and `RESTRICT`: a `DELETE` or key `UPDATE` of a referenced row is refused
  when a referencing row exists in **any** group (the error comes from the node that has
  the row; the whole statement and transaction fail, nothing is written on any node). A
  row that nobody references is deleted or changed, whichever node starts the statement.
- `CASCADE`, `SET NULL`, `SET DEFAULT`, on delete and on update: the rows of **every**
  group are deleted or changed, each node doing its own.
- A constraint that is `DEFERRABLE INITIALLY DEFERRED`: each node checks its rows when
  it prepares, so a violation is an error at `COMMIT`; the rows may be fixed in the
  same transaction, in any order. `SET CONSTRAINTS ... IMMEDIATE` is a statement of
  this node only: the other nodes keep checking at commit, which is later, not never.
- A reference table that references itself, a reference table referenced by a
  reference table and a distributed table at once, keys of several columns (`MATCH
  SIMPLE` and `MATCH FULL`), tables of two colocation groups that reference one
  reference table, and a chain reference table, distributed table, colocated
  distributed table: all of these act as above (`t/056_ri_distributed.pl` tests every
  one, with every action).
- During a bucket move: the check and the action see the rows of the bucket where
  they are attached (the source until the switch, the target after it); a cascade
  while the copy runs reaches the target through the replication.
- The writer locks of a reference table cover the reference tables reachable through
  keys, as before.

*How the statements of a key are told from a statement of a user.* Not by their text: for
a partitioned referencing table (every distributed table) PostgreSQL writes
`DELETE FROM "s"."t" WHERE ...` without `ONLY`, and a user can write any text. The planner
asks whether the backend is inside the security restriction `SECURITY_NOFORCE_RLS`, which
`ri_triggers.c` sets around the planning and the running of each of its statements (and
the fast path of PostgreSQL 19) and nothing else sets; no SQL statement, function or
`SET` of a user can set it. The statement must also have the shape of what the triggers
run (one table, a restriction by parameters, no join, subquery, aggregate or set
operation). Such a statement is planned for this node's rows. What remains is code that
PostgreSQL itself runs inside a key's statement (the default expression of a `SET
DEFAULT` action, the check constraints of the referencing table, the equality operator
of a key type): a function of that kind that runs a statement of the shape above gets
that one statement planned for this node's rows. It needs the right to define the
function, and a write to a reference table is still guarded by the executor check, so it
cannot make the copies differ.

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

Since step 2 of phase 2b the scan has these properties as well (design:
`kingres/docs/design/shard-phase2b.md`, sections 4.3 to 4.6 and 6.5):

- **Pruning.** A group that cannot own a key the statement names is not asked:
  `dist_col = <constant or parameter>`, `dist_col IN (...)`, `dist_col = ANY
  (<array or parameter>)` and an `OR` of such clauses, with an equality operator
  of the column's hash family under the collation the table was distributed with.
  The groups are chosen when the scan starts, so a generic plan prunes by the
  values of its parameters. `EXPLAIN` shows `Remote groups: 1 of 2 (pruned by
  id)` when a group was left out; any other clause prunes nothing, which is
  always correct.
- **One placement check for every statement** (`src/placement_check.c`): the
  placement number of the plan against the cache, the probe of every node before
  and after the cursors are declared (placement, identity of the node's
  partitions, version of the extension), and the origin's own placement and
  partitions. A node that runs another version of `kingres_shard` is refused
  with an error that says so (not `KS001`: repeating does not help until the
  upgrade is finished). The 2a fan-out write uses the same functions.
- **One node of a group per transaction.** A transaction that has read or
  written a group through one node and then needs another node of the same
  group (the old primary after `promote_node`, then the new one) fails with
  `KS001`; it never combines two states of a group.
- **A failed group.** A node that fails with a connection error, a shutdown or a
  read-only refusal is reported as `KS001` ("a failover of its shard group may be
  in progress") when its group has a registered standby, or the node is no longer
  its group's primary in the cache; otherwise it is the plain connection error,
  which a repeat would only meet again.
- **Reads that mix a distributed table with a local table**, and `SCROLL`
  cursors, are answered by the scan also when every distributed table is
  restricted to a key of another group (they were refused). A statement that
  writes a distributed table and reads a local one is still refused. With
  `kingres_shard.enable_distributed_scan = off` the refusal is back; that switch
  is the fallback of this step.
- **The local part of a fan-out write** (an `UPDATE`, `DELETE` or reference-table
  write run on every group) and the local batch of the row router are planned for
  this node only when the code that runs them has armed a one-shot flag of this
  backend right before the SPI call (it is consumed by the first call of the
  planner hook that follows, and cleared when the call ends), and only after the
  partition lists are as new as the catalog. Nothing in the statement text says
  that it is a local part, so no error context, `current_query` or log line carries
  it, and a function that the local part calls (a `CHECK`, a default, a trigger of
  a local table) cannot plan a statement of its own without routing: such a
  statement is routed like any other.
- **What a peer is compared with.** The version of the extension that a node
  answers in the placement probe is compared with the version compiled into this
  node's library, not with the version installed in its catalog: a node whose
  library is new and whose `ALTER EXTENSION UPDATE` has not run, and a node updated
  in SQL that has not been restarted, are refused by their peers with "node X runs
  kingres_shard A, this node runs B" (A and B are library versions).
- **The subtransactions of the node sessions** (review S14-01, S14-05).  The `RELEASE SAVEPOINT` of a
  committing subtransaction is sent at `SUBXACT_EVENT_PRE_COMMIT_SUB`, where an error is legal (as
  `postgres_fdw` does), so a failure aborts a subtransaction that is still in progress; the callbacks of the
  commit and of the abort of a subtransaction raise no error: a session that broke at a deeper level is repaired
  (its depth is lowered, or it is closed) and the transaction is refused at its commit or prepare when the
  session held work that is lost (`wrote`, a snapshot isolation level, work of a control call).  A session
  that only read in READ COMMITTED and did not answer `ROLLBACK TO SAVEPOINT` is closed without refusing the
  transaction.
- **A fragment that a group does not accept** (a statement that carries the marker of
  a scan, write or insert fragment and has no shape that the group runs) is refused
  with `0A000`, not with `KS001`: the origin and the group decide on the same tree,
  so repeating cannot help.

## UPDATE and DELETE that span shard groups

An `UPDATE` or `DELETE` that is not restricted to one bucket runs on every group
that owns buckets of the table (in group order, the local one through SPI), in
one two-phase transaction, and the counts and `RETURNING` rows are added, if the
statement can be shipped whole: one distributed table (as the target only, no
subquery or join on it or on another distributed table), reference tables
allowed, only built-in immutable functions and operators. An `UPDATE` that
assigns the distribution column is refused. Otherwise it fails with `this
UPDATE/DELETE spans shard groups and cannot be shipped`.

## Bulk writes through the row router (phase 2b, steps 3 and 4)

An `INSERT` into a hash-distributed table whose rows can belong to several groups, and `COPY ...
FROM` into one, are run by the **row router**, the "Kingres Row Router" custom scan (no
`ModifyTable` is in the plan). Switches: `kingres_shard.enable_bulk_routing` (`INSERT`) and
`kingres_shard.enable_copy_routing` (`COPY`); with a switch off the statement is refused exactly as
before.

**What is routed.** A multi-row `VALUES`, `INSERT ... SELECT` (the source is any `SELECT`: it is read
through the distributed scan like any query, and may read the table itself or a local table), and a single
row whose key is not a constant (a function that is not immutable, a default). A single row with a
constant key is forwarded to its group or run locally, as before; a reference table, a table with one
group and a local target are not touched.

**How.** The rewritten statement is turned into a `SELECT` that has one column for every column the
statement gives a value to; the router runs it as a read-only SPI cursor, fetching
`insert_batch_rows` rows at a time. A default, a `serial` and an identity value are computed once per
row, **at the node that runs the statement**, by its functions and sequences, and are sent as values
(sequences are node-local as before: a row inserted from another node draws from that node's
sequence). A column that the statement does not name and whose default is a constant is left out of
the column list, so the group applies its own default, and a role with the privilege on some columns only
can insert (a documented difference: the privilege on a column whose default is computed at the origin is
needed). Generated columns are never sent. A `GENERATED ALWAYS` identity column is sent with
`OVERRIDING SYSTEM VALUE`. The rows go to the group that owns their bucket in **batches of one statement**,
`INSERT ... SELECT u.c::type, ... FROM unnest($1::text[], ...) AS u(...)`, with the text of every value in
an array parameter (nothing a row holds is ever part of the statement text; the text is built from quoted
identifiers). The batch of the origin's own group runs through SPI.

**Atomicity.** The transaction is the one of 2a: every node written is a writer, two or more writers commit
with two-phase commit, a failure of any group at any point (a constraint, a unique violation, a lost
connection, a cancel) fails the statement and leaves nothing, after recovery too. The context of an error
names the batch (`bulk insert of <table>, batch <n> to group <g>`); the position of the row in the batch is not
known. There is no partial result.

**Memory.** At most one batch is in flight per remote group, and the previous answer is read before the
next batch is sent, so the memory is about two batches (`insert_batch_bytes`) per group plus the batch of
the source cursor and the `RETURNING` rows not yet handed out (a tuplestore: `work_mem`, then disk),

The batch of the source cursor is bounded in bytes as well: it is fetched in steps that follow the widest row seen
so far, about `insert_batch_bytes` plus one row, down to one row at a time (`t/128_router_review_round.pl`).  One row
is never split, and one very large value costs several times its size: the output string, the array literal and the
message at the origin, and on the group the parameter, the array, the `unnest` row and the stored row (measured:
about 5 times at the origin and 8 times at the receiving group for a value of 150 MB), so a value near the limit of
1 GB of a column needs memory of that order on the group, and the statement then fails with an ordinary out of memory
error.  The parameters of a batch follow the logging settings of each group (`log_statement`,
`log_min_duration_statement`, `log_parameter_max_length`): a group that logs binds logs the values of a batch, up to
`insert_batch_bytes`; the origin never logs the data of a `COPY`, and `pg_stat_activity.query` of a group shows the
marker and the statement, never a value.  The wait for the answer to a batch has no bound of its own besides a cancel,
`statement_timeout` and the keepalives; the locks of the statement stay on every group it wrote until then.

**Refused with a message that names another cause.**  A `RETURNING` list that cannot be run by the groups (a cast of an
integer to `text`, a whole-row reference, a system column, a sub-select, `random()`, `now()`) is refused with "the
RETURNING list ... cannot be run by the shard groups" (an I/O cast of an immutable type is not shipped); a
data-modifying CTE around a multi-group insert, `WITH i AS (INSERT INTO t SELECT ... RETURNING id) SELECT ...`, is
refused with the 2a message "statements that span shard groups are not supported yet".  Both fail closed.
whatever the number of rows (`t/117_bulk_memory.pl`).

**Waits.** The send is non-blocking and a wait for room in the socket is a latch wait, the answer is read
interruptibly: a cancel or a statement timeout ends the statement at once, also while a node does not read or
does not answer. The session of a batch in flight that the error leaves behind is cancelled and its end read
for at most the time of a cancel request (a third of `node_timeout`, at most `node_connect_timeout`); a node
that does not answer even then loses the session and the transaction cannot commit.

**Buckets that move and groups that change their primary.** Every batch statement carries the placement
number the router decided with and the group compares it with its own; before its first batch the router asks
each group for its placement and the identity of its partitions, and after the last batch asks again (the
rule of 10.2): a bucket that moved between the plan and the end fails the statement with the retryable
`KS001`, nothing stored; a statement that has rows in a group holds `ROW EXCLUSIVE` on its table there, which
the switch of a move waits for, so the moved bucket carries them. A row can never be stored in a wrong bucket
whatever the router believes: the tuple routing of the group refuses a row whose bucket is not attached
there. A group that is no longer a primary answers `KS001`. `PREPARE`d statements and generic plans work
(the router's routing is decided per row).

**`RETURNING`.** Evaluated by the group that inserted the row, arrives with each batch; the order is by group,
then arrival, not the input order (single-row inserts if the order matters). The list must be built from
columns of the table and built-in immutable expressions (`OLD` and `NEW` are refused).

**`ON CONFLICT DO NOTHING`** (also with an inference clause or `ON CONSTRAINT`) is supported; the count skips
the conflicts as on one node. `DO UPDATE` and, on 19, `DO SELECT` are refused (a second row for one key could
touch the row of an earlier batch). An inference clause with an explicit collation or operator class is refused.

**`COPY FROM`.** Does what `DoCopy` does (the privileges of the role, `pg_read_server_files` and
`pg_execute_server_program`, the read-only transaction, row-level security refused) and parses every format
and option itself (`FORMAT text|csv|binary`, `DELIMITER`, `NULL`, `HEADER`, `QUOTE`, `ESCAPE`, `ENCODING`,
`FORCE_NOT_NULL`, `FORCE_NULL`, `DEFAULT`, column lists, files and programs of the node that runs the
statement) with `BeginCopyFrom` and `NextCopyFrom`. **Refused**: `FREEZE`, `WHERE`, `ON_ERROR`,
`REJECT_LIMIT` and `LOG_VERBOSITY` (the core carries them out in `CopyFrom`), a reference table and a
bucket partition named directly. The error context names the line and the column. `pg_stat_progress_copy`
is not updated.

**Still refused for `INSERT`** (through the router; a colocated `INSERT ... SELECT` is shipped whole and
allows `DO UPDATE`, see the next section): `ON CONFLICT DO UPDATE`, a `WITH` clause on the statement (put it inside the
`SELECT`), `RETURNING` with `OLD`/`NEW` or an expression that is not shippable, an insert that reads a
security barrier view or a view of another owner, an insert through a view `WITH CHECK OPTION` (the check
would be lost), `session_replication_role` other than `origin`, a parallel
worker; with a reference table as the target the fan-out of 2a is used.

**Privileges.** Those of the caller on every node: the executor checks `INSERT` (and the columns) at the
origin before the router starts, and the groups check them again as the same role (nothing is run as a
superuser or as the extension). The statement of a batch is accepted by a group as such only in a session that
another node of the cluster opened (the forwarded flag of the README section "The forwarded flag"); in any
other session the same text is an ordinary statement and is routed like one.

**New SQLSTATEs and messages.** None. `KS001` is raised for the changes of placement named above; the
refusals use `0A000`. The messages of the refused forms are listed in `sql/bulk_plans.sql`.

## Colocated `INSERT ... SELECT`, shipped whole (phase 2b, step 5)

When the target of an `INSERT ... SELECT` and every distributed table its `SELECT` reads are in one
colocation group, and the `SELECT` is joined and restricted so that each row of its result is made on the
group that owns it, the whole statement is sent to every group of the target and run there, inside the
distributed transaction of the session: each group reads its own buckets and writes its own buckets, no row
passes through this node, and the count and the `RETURNING` rows are the sums over the groups. The plan is the
"Kingres Fan-out Write" custom scan with a line `Pushed: colocated INSERT ... SELECT`, and its `Remote SQL` is
the text that the groups run, starting with the comment `/* kingres:insert-select-fragment place=<n> */`
(`place` is the placement number of the colocation group, which a group compares with its own after planning).
The switch is `kingres_shard.enable_colocated_insert_select`.

**What qualifies.** All of these (the analysis is `src/pushdown.c`, shared with the later pushdown paths):

- the target is a distributed table and every distributed table of the `SELECT` is of its colocation group;
  reference tables may be joined anywhere an inner join, or the nullable side of an outer join, allows;
- the distributed tables of the `SELECT` are joined by equalities of their distribution keys (in `WHERE`,
  in an inner `ON`, or in the `ON` of the outer join whose two sides they are on): one key class;
  an outer join keeps its preserved side distributed, and a `FULL JOIN` is shipped only between two
  distributed sides;
- the distribution column of the target is given directly by a key of that class (not an expression of it, and not
  a key of a table on the nullable side of an outer join): the row is then made, and stored, on the same group;
- the `SELECT` is closed: an aggregate, `DISTINCT`, or grouping is by a key of the class (a `SELECT` without
  `GROUP BY` but with an aggregate is not shipped); no `LIMIT`, `OFFSET`, `DISTINCT ON`, window function,
  sub-select, `WITH`, set operation, `FOR UPDATE`, view, function or `VALUES` in `FROM`;
- every expression is built in and immutable, on built-in types, with the default or the `C` collation, as in
  "Which statements are shipped" above; a column that the statement does not name must have a default
  that is the same everywhere (a constant: the group applies it), and a serial, identity, volatile or stable
  (`now()`) default is computed once, at this node, by the row router; the OID of a table or function that is
  not built in (`'t'::regclass`) is not shipped;
- the target has no trigger or rule of a user, no row-level security and no `WITH CHECK OPTION`; the sources have
  no row-level security and are not read with the rights of another role.

A statement that does not qualify takes the row router (or its refusals) exactly as before, with the same
rows and errors; `SET client_min_messages = debug1` says why a statement was not shipped.

**`ON CONFLICT`.** None, `DO NOTHING`, and `DO UPDATE` (with an inference clause or `ON CONSTRAINT`), under
the rules of design 11.7: the `SET` list does not assign the distribution column; the `SET` and `WHERE`
expressions are shippable and use only the target and `EXCLUDED`; every unique index of the target has the
distribution column among its key columns (PostgreSQL already requires it of a partitioned table). Two
rows conflict only if their keys are equal, so on the same group: the count, the `RETURNING` rows and the
error "ON CONFLICT DO UPDATE command cannot affect row a second time" are those of one node, and the whole
statement fails and rolls back everywhere. `DO SELECT` (PostgreSQL 19) is not shipped.

**Consistency.** The groups commit together by the two-phase commit of the fan-out. A bucket that moves, or a group
that changes its primary, between the plan and the end of the statement is `KS001` with every change rolled back
(the plan number is compared before the first group runs, `place=` by each group after planning, and the
placement of every group after the last one ran; a move of a bucket of a group that already ran waits for the
transaction). A cached or generic plan checks at its start that its relations are still what they were
(distributed in the same group, or reference). Each group runs the statement under one snapshot of its own,
so a source read twice by the same statement is one state of that group; between groups the skew of the scan
remains. A group that fails is reported as the remote error, or as `KS001` when it has a standby (a failover may
be in progress).

**What an operator sees.** `EXPLAIN` shows the line `Pushed:` and the text of the groups. The setting is
`kingres_shard.enable_colocated_insert_select`; fault points `insel_before_group`, `insel_after_group` and
`insel_before_recheck` exist for the tests. The groups run it in a session that this node opened, and refuse
the same text in any other session as an ordinary statement; a forged fragment is `0A000`, a stale
placement `KS001`. During a rolling upgrade set the switch off on every node (the groups must run the same
version, which the placement probes of the scans check).

## Closed queries, run whole by every group (phase 2b, step 6)

A `SELECT` that spans shard groups is normally answered by the distributed scan, which moves rows to the node
that runs the statement.  A **closed** query is one whose every output row depends on the rows of one group only:
a `GROUP BY` that contains the distribution key (or a key that is equal to it through a join on the keys), a
`DISTINCT` that has the key in its select list, a join of colocated tables on their keys with a reference table
or without.  Each group computes its own final rows from its own buckets and only those rows cross the network
(the "Kingres Fragment Scan" of `EXPLAIN`); the node that runs the statement runs its own group's part through
SPI, concatenates, and applies `OFFSET` and `LIMIT` itself and, when the groups do not sort for it (see "Sort and limit of a closed query"), `ORDER BY`.  The planner takes this
path only when its cost is lower than the scan's; a statement outside it gets the plan it got before.

What is sent is the statement with its parameters (`$1`...), after a check that the groups would accept its text
(the origin parses and rewrites the text it deparsed and runs the group's own rule on it).  The marker is the
comment `/* kingres:query-fragment place=<n> */`; a group refuses a statement that no rule accepts with `0A000`
and a stale placement with `KS001`.  Reading is checked as the scan is (the placement of the colocation group
before and after the cursors are opened, the partitions of every table, one node per group in a transaction).

Not shipped as a closed query (the scan answers, or the partial aggregation below does for an aggregate or a grouping): a grouping or `DISTINCT` that does not contain the key, an aggregate with no
`GROUP BY`, grouping sets, `DISTINCT ON`, aggregates whose result depends on the order of the
rows (`array_agg`, `string_agg`, floating point sums), a function that is not built in and immutable (`now()`),
a conversion through the text form of a value, a sub-select, a CTE, a view, a local table, a table of another
colocation group, a key from the nullable side of an outer join, row-level security, a
statement that locks rows, and a statement that reads one table and neither groups nor removes duplicates (nothing
to gain).  `EXPLAIN (VERBOSE)` of the scan such a statement gets says why (`Pushdown: not used: ...`).  A statement that reads a view of another
owner is not answered by the scan either: it is refused with an error, on every path ("Views" in the limitations of phase 2a).

`EXPLAIN` of a shipped statement shows `Pushed:`, `Groups: 3 of 3 (local + 2 remote)` (with `; pruned by <column>`
when a restriction on the key left groups out), and `Remote SQL:`; `EXPLAIN ANALYZE` adds `Rows from groups:`.

**What is never shipped, and what is refused.**  A `FULL JOIN` is shipped whole only between distributed tables whose key equality is in its `ON` clause; one with a
reference table, or without that equality, is not (the scan answers).  In a
colocated `INSERT ... SELECT` every cast that is not text to text goes through the row router, not through the groups; the
groups run one after another, not at once.  `ON CONFLICT DO SELECT` (19) is refused, not merely "not shipped".  A
parameter, an array, a range or a row of a `reg*` type is not shipped (each group would resolve the name to its own
object).  `FROM ONLY` a distributed table is refused on every path ("FROM ONLY is not supported for distributed table"), and so is `TABLESAMPLE` of one
("TABLESAMPLE is not supported for distributed table"): a sample of a table that is spread over groups is not one sample of the table; a reference
table and a local table are sampled as on a single node, and a `random()` condition samples a distributed table.
A closed query that returns about as many rows as it reads (a grouping or `DISTINCT` by a unique key of one table) is
left to the scan: the groups would return the same rows.  `EXPLAIN ANALYZE` of a fragment scan shows the time to the
first batch of each group.
The test is not the planner's estimate of the number of groups (it comes from the rows of this node): a grouping or `DISTINCT`
whose columns contain every column of a unique index of the table (immediate, valid, no predicate, no expression) is
left to the scan whatever the statistics say.  `DEBUG1` shows the two numbers it compared.

**What a value of a `reg*` type means.**  On the paths that evaluate at this node (the scan, a fragment scan that is not
shipped, the router, a colocated `INSERT ... SELECT` that is not shipped) it is the value this node computes: the OID
of this node's object.  A statement that names one key goes to its group as the text the user wrote (the 2a path,
unchanged) and that group resolves the names in it itself: a raw OID that one node computed and stored in a column is
not an object of another node, and such a column is not portable between nodes.  t/137 uses a table whose OID differs on
every node and runs every node as the origin.

Switches: `kingres_shard.enable_pushdown` and `kingres_shard.enable_pushdown_closed`.  Fault points of the tests:
`frag_before_probe`, `frag_after_probe_before`, `frag_after_declare`, `frag_local_open`, `frag_after_probe_after`,
`frag_first_fetch`, `frag_before_fetch`, `frag_wait_batch`, `frag_local_fetch`, `frag_declared_g<n>`; the windows of the defences against a catalog that changed after the plan (t/136): `insel_before_rels_check`, `insel_before_start_check`, `insel_before_text_parse`, `query_before_text_parse`.  A statement
that is cancelled, or fails, while a group runs its part asks the group to stop it (bounded), so a group that is
blocked by a lock does not hold the end of the statement for `node_timeout`.

**Under `debug_discard_caches`.**  A node runs under the setting (every cache is discarded at every lock and every
catalog access) as it runs without it.  The cache of the catalog that was replaced is kept until the end of the
transaction, so that the pointers a statement holds stay valid (a node, a distributed table, a colocation group), and the
planner makes its analysis again with the new cache when the cache changed under it.  `add_node` and the check of the
synchronous standbys of the other primaries copy what they need before they connect.  t/135 runs a cluster under the setting.

## Join rows (phase 2b, step 10)

A statement that is not closed as a whole (a window function that is not partitioned by the key, an ordered-set aggregate,
`count(DISTINCT ...)`, `DISTINCT ON`, a `FULL JOIN` with a window above it) but whose distributed tables are joined on their
distribution keys (with reference tables if it likes) has a join that every group can compute on its own rows.  The join of the
whole level is then a fragment of its own: the groups return the rows of the join, with the columns the rest of the plan needs
(`EXPLAIN` shows `Pushed: join rows (on key)` and `Remote SQL: SELECT s1.id, s2.c FROM (public.s1 JOIN public.s2 ON ...) WHERE ...`), and
this node plans what is above it (the window, the aggregate, the sort) as for any scan, so only the joined rows cross, not the rows of
each table.  The text is a closed query (a join on the key and no aggregate), so the group's rule for the fragment of a closed query
accepts it; nothing new can be asked of a group.  Permitted: an inner join of any mix of distributed and reference tables
in one key class, a left, right or full join between distributed tables with the key equality in the `ON` clause of that join, and a
distributed table left joined to a reference table.  Not pushed: a reference table on the preserved side of an outer join (every group would emit
its unmatched rows), a join on a non-key column, two colocation groups or two key classes, a sub-select, a view, a local table.  The planner
chooses by cost between this, the partial aggregation, the closed query and the scans; a closed statement keeps its own path.  The
hook runs for the join relation of the whole level only, once, and never for a grouped relation (19, eager aggregation).  Switch:
`kingres_shard.enable_pushdown_join` (on).


## Sort and limit of a closed query (phase 2b, step 9)

The groups sort the rows of a closed query and this node merges the sorted streams.  What is sent is the query with its `ORDER BY`
(the select list holds every sort expression: an entry that only orders is sent as a column) and, when the limit is pushed, `LIMIT <count +
offset>` with no `OFFSET`; every group returns its first rows of the final order, the `Kingres Fragment Scan` merges the streams with
one `SortSupport` per key (the ordering operator, the direction, `NULLS FIRST` or `LAST` and the collation of the key, as the planner
takes them for a `Sort`), so there is no `Sort` above it, and the `Limit` of this node applies the offset and the limit to the merged
stream.  A tie goes to the stream of the group with the lower group id (this node's own part has the id of its own group), so rows that compare equal come out in the same order among the groups whichever node runs the statement, and in the same order when the statement is repeated on unchanged data.  The order of equal rows inside one group is that group's own (it can differ between a plan with a limit and one without).  `EXPLAIN` shows
`Merge keys: <n>` and the text with `ORDER BY` and `LIMIT`.  A cursor that is closed early closes the cursors of the groups; a group that
answers late holds the merge up for as long as it has to.

Pushed only when this node can order exactly as the groups do: the ordering operator of every key is a built-in btree operator and
the collation of the key is the default or `C`; otherwise the groups send the rows unordered and this node sorts them, as before.  The
limit is pushed only with a sort that is pushed (or none).  Constants that fit in `bigint` are summed here (`LIMIT 7 OFFSET 20` sends
`LIMIT 27`).  A parameter, or an expression of parameters and of immutable and stable functions (`LIMIT $1 OFFSET $2`, `LIMIT
current_setting('x')::int`), is computed by this node when the scan starts, so it follows the values of each execution of a prepared
statement and of a generic plan; count + offset is sent as one more `bigint` parameter (`LIMIT $n`).  `NULL`, a negative number and a sum
that does not fit send no limit (`NULL`): this node's own `Limit` node then raises the single node's error for a negative count or
offset, so the error is never one that a group made.  `LIMIT ALL`, `OFFSET` alone, `FETCH FIRST ... WITH TIES`, and an expression with
a column, a volatile function or a sub-select send no limit (the merge still holds).  A group accepts a fragment with a limit only when
it is one constant or one `bigint` parameter, with no `OFFSET`.  An aggregate or grouping that is not closed has nothing
to push: its rows are sorted after the final aggregation.  Switch: `kingres_shard.enable_pushdown_sort` (on).


## Partial aggregation (phase 2b, step 7)

An aggregate or a grouping that is not closed (it does not contain a distribution key: `SELECT count(*) FROM t`, `SELECT a,
sum(b) FROM t GROUP BY a`, the same over a join of tables of one colocation group on their keys, or with a reference table)
is run in two steps.  Every group aggregates its own rows and returns one row per group of rows, with the transition
state of each aggregate (a state of type `internal` is returned serialized, as `bytea`, by the aggregate's own
serialization function); the node that runs the statement combines the states with the planner's own final aggregation
(`Finalize HashAggregate`, `Finalize GroupAggregate` or `Finalize Aggregate` in `EXPLAIN`, over the `Kingres Fragment
Scan`), applies `HAVING`, runs the final functions, and everything above (`ORDER BY`, `LIMIT`, a window function over
the result, `DISTINCT` and `DISTINCT ON` of the result) is planned by PostgreSQL as usual: the text a group receives has neither a window
nor `DISTINCT`, and a group that is sent one refuses it.  The marker of the text a group receives is
`/* kingres:partial-fragment place=<n> */`; a group refuses what its rule does not accept (a statement with `HAVING`,
`ORDER BY`, `LIMIT`, `DISTINCT`, a window, an aggregate that cannot be split, an aggregate inside an expression) with
`0A000`, and a stale placement with `KS001`, as for a closed query.  The placement check, the cursors, the local part of
this node, pruning, cancel and failover are those of the fragment scan.

What is split: the built-in aggregates that have a combine function and, for an internal state, a serialization pair
(`any_value`, `array_agg`, `avg`, `bit_and`, `bit_or`, `bit_xor`, `bool_and`, `bool_or`, `corr`, `count`, `covar_pop`,
`covar_samp`, `every`, `max`, `min`, `range_intersect_agg`, the `regr_*` family, `stddev`, `stddev_pop`, `stddev_samp`,
`string_agg`, `sum`, `var_pop`, `var_samp`, `variance`), with `FILTER`, over any type that is shipped.  The test t/139
recomputes this list from `pg_aggregate` and fails when a release changes it.  Not split (the scan answers, the answer is the
same): `DISTINCT` or `ORDER BY` inside the call, ordered-set and hypothetical-set aggregates, `json_agg`, `jsonb_agg`,
the `*_object_agg` family, `range_agg`, `xmlagg` (no combine function), grouping sets, a column of type `money`, an aggregate of an extension that is not on the compiled-in list, a table of another colocation group, a local table, a sub-select.

**Floating point and order.**  The partial states are combined in the order the groups answer, as in a parallel plan.  A
floating point `sum`, `avg`, `stddev`, `variance`, `corr` or `regr_*` can therefore differ from the single node's in the
last digits, and between two runs of one query; `array_agg` and `string_agg` without `ORDER BY` give the elements in
an order that SQL leaves open.  (The closed query of step 6 never ships such an aggregate: it is run by the scan.)
`kingres_shard.enable_partial_aggregation = off` restores the scan.  The tests compare a float to nine digits and sort
the elements of an array.

Switch: `kingres_shard.enable_partial_aggregation` (on; `enable_pushdown` is the master switch).  `DEBUG1` says why a
statement is not split.

## Objects of extensions (phase 2b, step 8)

The functions, operators, types and aggregates of an extension are shipped to the groups when the extension is on a list that is
compiled into `kingres_shard` (today `kingres_sketch`, version `1.0`), when the version installed on the node is one the list
accepts, and when the object meets the rules below.  The `kingres_sketch` aggregates (`approx_count_distinct`, `hll_add_agg`,
`hll_union_agg`, `topn_add_agg`, `tdigest_agg`, `approx_percentile`, ...) then run as partial aggregations and, when the query is
closed, whole; a column of a sketch type crosses as its text form (`\x` and the hex digits).  The rules (`ext_object_ok`):
a function or aggregate is written in C or `internal`, is `IMMUTABLE`, is not `SECURITY DEFINER`, has no `SET` clause, and has
argument and result types that are built in or of the list; a type is a base type of the extension whose input, output, `send` and
`receive` functions are the extension's; an operator is a member whose function meets the rule for functions.  A function of a user,
whatever its label, is not shipped: functions are not synchronized between the nodes.

The marker names the extensions the statement uses with their versions, `/* kingres:partial-fragment place=<n>
ext=kingres_sketch:1.0 */` (the same for `kingres:query-fragment`).  A group compares them with its own catalog before it
analyses the text: another version, or none, is `KS001` with the message "extension kingres_sketch is at version 1.0 here and 0.9
at the node that sent the statement", and the hint says a repeat does not help before the upgrade is finished on every node.  A
statement that uses an extension its marker does not name, or a marker that names one the statement does not use, is `0A000`.  A
group that does not have the extension at all would fail when it parses the text ("function ... does not exist" when the extension was
dropped and its schema is left, "schema ... does not exist" when it never had it); PostgreSQL has no hook between the parser and the analysis
of the `DECLARE`, so the sender does not wait for that error: the probe that it asks of every group before the `DECLARE` also asks the catalog
of the group which of the extensions named in the marker it has, and a group that has none of that name gives the `KS001` of a version difference
("node host:port does not have an extension that the statement uses (kingres_sketch:1.0)", the detail says it is not installed there).  The
decision is the group's catalog, not the error of a parse, so no other error is taken for a missing extension.  This is the
one place where a shipped statement can fail for a reason the scan would not have; the escape is
`SET kingres_shard.shippable_extensions = 'none'`, with which the node that runs the statement reads the rows and runs the
aggregate itself.

`kingres_shard.shippable_extensions` (`USERSET`, default `all`) is `all`, `none` or a comma-separated list of names from the
compiled-in list; another name is an error when it is set.  It is read at the node that runs the statement and only narrows what the
code allows, so any user may set it.  Comparing `kingres_sketch.self_test()` and the encoding between nodes belongs to `add_node` and
`check_cluster` (phase 12); this step checks the version only.  Objects of an extension are allowed in the fragment of a closed
query and of a partial aggregation only; the scan, the writes and the colocated `INSERT ... SELECT` never ship them.


## Distributed schemas (phase 11, version 0.8)

Design: [`shard-phase11.md`](../../kingres/docs/design/shard-phase11.md).  A **distributed schema** is a schema whose tables all live
in one shard group (one tenant, or one microservice, per schema).  The design is built in increments; this is what exists so far.

**Increment 1: the catalog, the views and the inventory.**  No statement is routed by schema and no schema is made distributed by the
product yet: these are the objects the later increments use, tested with rows made by hand.

- `kingres.colocation.kind` is `hash` (the buckets of hash-distributed tables) or `schema` (one bucket: the one group that holds a distributed
  schema; `dist_column_type` is then null and `bucket_count` is 1).  `kingres.dist_table.method` gains `schema` (a table of a distributed
  schema: no distribution column, the colocation group of the schema).
- `kingres.dist_schema`: one row per distributed schema (`nsp` as a node-local `regnamespace`, the colocation group, `ship_functions`).
  The catalog snapshot carries it by schema name, so a node that is given a snapshot must have the schema (an error names it).
- `kingres.sharding_settings`: one row, the `mode` of the database (`row`, the default and the mode of every database before 0.8; `schema`;
  `both`) and `distribute_new_schemas`.  `kingres.set_sharding_mode(mode, distribute_new_schemas)` changes it atomically (map group primary,
  superuser): `row` to `both` and `schema` to `both` always; `both` or `schema` to `row` while a distributed schema exists, and `both` or `row`
  to `schema` while a hash-distributed table exists, are refused with the schemas or tables named; `distribute_new_schemas` needs `schema` or `both`.
- The delta functions (node-local, superuser, the same text on every node in one transaction in the later increments):
  `internal_register_schema`, `internal_register_schema_table`, `internal_unregister_schema_table`, `internal_unregister_schema`,
  `internal_set_schema_group`.  Dropping a registered schema forgets its rows (`kingres.forget_dropped_tables`).
- `kingres.schemas`: one row per distributed schema with its owner, group and node, state, number of tables, size (known on the node of the owner
  group only) and the target group of a running move.  `kingres.buckets` shows a schema table with bucket 0 and no partition.
- `kingres.schema_inventory(schema)`: every object of the schema with `supported` and the reason when it is not (a hash or reference
  table, an unlogged table, a materialized view, a rule, row-level security, extended statistics, a publication, an aggregate, a range type, a foreign key or an
  inheritance across the border of the schema, a view that reads a table of another schema that is not a reference table, a `kingres_ledger` table, and a
  system or already distributed schema).  It reads the catalog only and any role may call it.

The SQL of phase 11 is a marked section ("PHASE 11") at the end of `kingres_shard--0.7--0.8.sql` and `kingres_shard--0.8.sql`.  The version handshake
of `internal_check_node_versions()` asks for 0.8.

**Increment 2: reading a table of a distributed schema that another group holds, and the defence against reading the empty copy.**  The tables
of a distributed schema exist on every node; the rows are on the one node (shard group) that holds the schema, the owner, and the copy on every
other node is an empty "shell".  The catalog cache keeps these tables apart from the hash-distributed ones (`schema_tables`), so no code written
for hash tables sees them.  Still no statement is routed whole and no schema is made distributed by the product: the rows of `dist_schema`
and `dist_table` are made by hand with the delta functions, as the tests do.

- A `SELECT` (or a read inside any statement) that names a shell is planned with the distributed scan in its single-source form: no local part,
  one fragment `SELECT <columns> FROM <table> AS ks_t WHERE <shippable restrictions>` to the owner, the restrictions applied again here, the
  placement checked before and after the cursor is declared.  Joins with other schemas, with local tables, with hash and reference tables,
  subqueries, CTEs, aggregates, windows, parameters and generic plans are planned by PostgreSQL above the scan as for any scan.  On the
  owner the table is read by the planner of PostgreSQL alone.
- A write whose target is a shell is refused ("is not supported from shard group N yet", with the node to run it on in the hint); so are
  `COPY` of the relation (`COPY t TO` would answer with no rows, `COPY t FROM` would write the empty copy), `FOR UPDATE`, and a write of a hash-distributed table
  that reads a shell.  `COPY (SELECT ...) TO` works, and a read of a shell into a local table (`CREATE TABLE AS`, `INSERT ... SELECT`) works.  The same limit as for a hash
  table holds: an `UPDATE` or `DELETE` that reads a shell in `FROM`, `USING` or a semi join needs the row identifier of the table it does not modify and is refused.
- The end-of-planning check: after a statement has been planned, no scan of the plan may read a shell (a plan that does is `ERROR: internal error: a plan reads the local copy of
  "s.t", which this node does not own`, SQLSTATE `XX000`).  The paths the routing cannot see, such as a SQL function that the planner expands into the statement
  when `kingres_shard.enable_distributed_scan` is off, end here and never in an empty answer.
- A session that another node opened (a forwarded session) that names a table of a distributed schema which this group does not hold gets the retryable
  error `KS001` ("this node does not hold table ...").

**Increment 3: DDL on a distributed schema, `distribute_schema` of an empty schema, `CREATE SCHEMA` that distributes.**

- `kingres.distribute_schema(nsp regnamespace, group_id int default null) returns int` (superuser, map group primary, read committed, mode `schema`
  or `both`): the schema must exist and hold no object; it is created on every other node with its owner and registered everywhere in the same
  two-phase transaction.  The group is the one with the fewest distributed schemas among the groups whose primary is active and should have shards (the
  lowest group on a tie), the one named, or the one of `kingres_shard.schema_placement_group` (superuser, session).  A system schema, `public`, the schema of an
  extension and a schema that is already distributed are refused; a schema that holds objects is refused until the conversion of a later increment.
- A statement whose object is in a distributed schema is run on the map group primary and then on every other node in the same two-phase transaction, as
  the DDL on a hash-distributed table is: `CREATE`/`ALTER`/`DROP TABLE` (also partitioned tables and `PARTITION OF`), indexes (`CONCURRENTLY` is
  refused), views, sequences, functions and procedures, enum, composite and domain types, `COMMENT`, `GRANT`/`REVOKE` (also `ON ALL TABLES IN SCHEMA`
  and `ALTER DEFAULT PRIVILEGES IN SCHEMA`), triggers, `TRUNCATE`, renames and `OWNER TO`.  Every table the statement makes is registered as a schema table on every
  node (`kingres.internal_schema_delta`, a node-local step that runs with the rights of the bootstrap superuser; the caller is an ordinary role).  After a
  `CREATE`, `ALTER` or `CREATE INDEX` on a table the map group primary asks every node for the structure of the table in the same transaction
  (`kingres.internal_table_structure`, `internal_table_fingerprint`) and the whole statement is rolled back when one differs.
- Refused in a distributed schema, with `<statement> is not supported in a distributed schema yet`: `CREATE MATERIALIZED VIEW`, `CREATE TABLE AS`, `SELECT INTO`, rules,
  policies and row-level security, extended statistics, foreign tables, range types, aggregates and operators and the other `CREATE` forms of `DefineStmt`,
  `TRUNCATE ... CASCADE`, `ALTER ... SET SCHEMA` into or out of a distributed schema, a statement that names a distributed schema and another schema, a foreign key from a table of a
  distributed schema to anything but the same schema or a reference table, a foreign key from outside to a table of a distributed schema, and a partition or
  parent in another schema.  A statement that is none of these kinds is run on this node only, as before.
- `CREATE SCHEMA` in a database that distributes new schemas (`kingres.set_sharding_mode(mode, true)`, or `kingres_shard.distribute_new_schemas = on` for the session of a
  superuser; an ordinary role cannot opt out) creates the schema on every node and registers it, on the map group primary.  `CREATE SCHEMA ... CREATE TABLE ...`
  (elements) is refused there.  The limit `kingres_shard.max_distributed_schemas` (default 10000, a guess) bounds the number of schemas.
- `create_distributed_table` is refused in the mode `schema` and for a table of a distributed schema; `create_reference_table` is refused for a table of a distributed schema and
  stays allowed in the mode `schema`.

## Limitations in phase 2a

Read these first; each is a reviewed boundary, not an accident.

- **No global snapshot** (see Guarantees). The distributed scan is slow for
  large tables by design: it moves rows. Since phase 2b the planner can run a closed query, an
  aggregate, a sorted stream with a limit and a join of colocated tables in the groups so that only their
  results cross (the sections of phase 2b above); what none of those paths accepts is answered by the
  scan, which moves the rows of every table involved. `EXPLAIN (VERBOSE)` says why a statement was not pushed.
- **Differences from a single node that pushdown makes visible.**
  - A cursor with neither `SCROLL` nor `NO SCROLL` over a pushed ordered statement (the merge of the sorted
    streams) cannot scan backward: `FETCH BACKWARD` fails with "cursor can only scan forward". On a single node
    the same cursor can, when the plan happens to allow it. Declare the cursor `SCROLL` (it then reads the
    rows through a `Material` node and the pushed path stays) or accept forward only.
  - A pushed statement can fail with `KS001` (a placement or an extension version that changed or that differs
    between nodes) where the scan would have waited or answered; the client repeats it (for a version difference,
    after the upgrade has reached every node).
  - The groups run a pushed statement with the settings the extension sends and `search_path = pg_catalog`;
    a function that reads other settings is not shipped (only built-in immutable functions and the listed
    extension objects are).
  - A floating point aggregate can differ in the last digits from the single node's and between runs, and
    the elements of an `array_agg` or `string_agg` without `ORDER BY` come in an order SQL leaves open.  The same holds
    for `min` and `max` of values that compare equal and print differently (`numeric` 57 and 57.00): any of them is
    returned, and which one changes between runs (a single node leaves it open too).
  - **`SET ROLE` to a role that cannot log in.**  The groups are reached as the current role (`current_user`), so after
    `SET ROLE` to a role with `NOLOGIN` every read that spans groups fails ("role ... is not permitted to log in") where a
    single node answers.  The scan has the same limit.  It leaks nothing (a role with no privilege is denied on both); use a role
    that can log in, or connect as the role.
  - A `LIMIT` that is a parameter or an expression of parameters and stable functions is computed when the
    scan starts; an expression with a column, a volatile function or a sub-select is not pushed (the merge
    still holds, and this node's `Limit` node applies it).
  - Not pushed at all, answered by the scan: grouping sets, a `FULL JOIN` with a reference table or without the key equality in its `ON` clause, aggregates without a
    combine function when the query is not closed (`json_agg`, `jsonb_agg`, `*_object_agg`, `range_agg`, `xmlagg`), `DISTINCT` or
    `ORDER BY` inside an aggregate call, statements with sub-selects, CTEs, views, local tables, tables of
    two colocation groups, a table with row-level security, and a statement that locks rows.
- **Writes that span groups.** `MERGE` and an `UPDATE` or `DELETE` that cannot be
  shipped are refused. A write that uses a function that is not built in cannot be
  shipped. (A multi-row `INSERT`, `INSERT ... SELECT` and `COPY ... FROM` into a
  distributed table are no longer refused: see "Bulk writes through the row router",
  which lists what is still refused for them.)
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
- **Settings of the session that change what a write does** (review finding S6-04). A
  statement that runs on another node runs in a session that this node opened, with the
  settings below and the defaults of that node for everything else. Every setting that
  can change the rows a write produces, and what is done about it:

  | setting | on the other node | what the extension does |
  |---|---|---|
  | `DateStyle`, `IntervalStyle`, `extra_float_digits`, `standard_conforming_strings`, `bytea_output` | forced | the text of a statement and of its parameters is written under fixed values, and the remote session has the same; for DDL the session's values are sent with the statement |
  | `search_path` | `pg_catalog` for data statements; the session's for DDL | a function without its own `SET search_path` that runs there resolves unqualified names in `pg_catalog` only (it fails, it does not find another object) |
  | `TimeZone` | carried, statement by statement | |
  | `transaction_isolation`, `transaction_read_only` | carried at `BEGIN` | `default_transaction_read_only` of the other node does not apply (a read only transaction is asked for explicitly), but a node whose role or database is set to read only fails a write with its own error: nothing is written silently |
  | `session_replication_role` | **not carried** | a write to a reference table, and any write to a distributed table on a cluster of several shard groups, is **refused** while the role is `replica` or `local` (the origin would skip triggers and foreign key actions that the other nodes run); use `origin`, or repeat the statement on each node by hand with `kingres_shard.enable_routing = off` |
  | `row_security` | not carried | harmless: row-level security cannot be enabled on a distributed or reference table (it is refused), so there is nothing to bypass or to apply |
  | `check_function_bodies` | not carried | harmless: it matters when a function is created, which is DDL and runs under the setting of the session on every node |
  | `default_toast_compression`, `gin_pending_list_limit`, `fastupdate`-style index parameters, `vacuum_*`, `work_mem` and the planner settings | not carried | harmless: they change how a value is stored or how fast a statement is, not which value; a column may be compressed differently on two nodes |
  | `statement_timeout`, `lock_timeout`, `idle_in_transaction_session_timeout` | not carried | each node applies its own; a timeout is an error, never a different result (the writer lock of a reference table has its own bound, `node_timeout`) |
  | `transform_null_equals`, `array_nulls`, `xmloption`, `lc_monetary` and the other settings that change how text is read | not carried | the statements and their parameters are sent already parsed into text that does not depend on them; DDL is sent as written, so a `CHECK` that depends on one of these is read under the other node's value: do not use them in the DDL of a distributed table; `money` is never shipped (see above) |
  | `synchronous_commit` | not carried | the commit of a participant follows the node's own setting; the decision of the origin is flushed whatever it says |

  A generated column or a function that is declared `IMMUTABLE` and is not: see the paths
  table of "Reference tables" (the extension trusts the declaration, as PostgreSQL does for an
  index, and `check_cluster()` finds the copies that differ).
- **An error that ends a distributed scan, caught by the session** (review finding
  S6-02). A scan asks every group for its next batch before it needs it. If an error
  ends the statement while a batch is in flight (a division by zero in the target
  list, a failing restriction on a group, a cancel, `statement_timeout`) and the error is
  caught by a savepoint (`ROLLBACK TO SAVEPOINT`) or an `EXCEPTION` clause, the
  transaction goes on: the answer in flight is read and dropped, the remote savepoint is
  rolled back, the cursors the scan declared go with it, and the next statement uses the
  connection as before. A cursor that was declared before the savepoint keeps its place; one
  declared inside it is closed. The connection never keeps a pointer to a scan that is gone:
  the scan lets go when its memory goes (a reset callback of its memory context), and the
  abort of a subtransaction reads the answer through the scan while it still exists.
  `t/057_scan_errors.pl` runs these sequences, thousands of times in one backend.
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
group primary); and for every reference table `reference_table_differs` (a node whose copy has other rows);
and for the online table operations (9.3) `table_operation_stalled`, `shadow_table`, `log_table`, `capture_trigger`,
`old_table` and `table_verification_failed` (see "The online variants").
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
- A target that stops (a crash) in the instant between its table-sync worker's drop of its
  slot on the source and the record that the table is done restarts a worker that asks for a
  slot that is gone (seen on PostgreSQL 19; the subscription's error counters do not count it).
  A move that finds a table in the state "finished copy" whose slot is missing on the source, for
  `2 x node_timeout` seconds (at least 10), fails with "lost its replication slot" and is undone;
  repeat it (`t/074`, `t/028`).
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
  distributed table is routed by the row router, which fails with the retryable `KS001` when a bucket moved between its plan and its first batch and otherwise makes the switch of a move wait for it (section "Bulk writes through the row router").

## High availability (version 0.5, phase 5)

Version 0.5 is the part of high availability that lives in the extension
([`kingres/docs/design/ha-phase5.md`](../../kingres/docs/design/ha-phase5.md),
part C; the design calls it 0.4, a number that a fix round used first). The
control plane creates the standbys (`pg_basebackup`, streaming replication),
decides when a primary is gone, **fences** it and promotes a standby in
PostgreSQL; the extension is what makes the cluster's catalog, routing and
two-phase commit follow. The functions below are for the control plane and for an
administrator; all of them are for superusers (`REVOKE ... FROM PUBLIC`, and
`promote_node`, `create_restore_point`, `standby_status` and the helpers check it
again). This section is the contract: every sentence of it is either a guarantee
that a test shows (the test is named), or says what is not guaranteed.

### What is guaranteed, and the configuration it needs

Configuration that the guarantees below assume, per node: `max_prepared_transactions`
above zero on every node (two-phase commit); a PostgreSQL major and an extension
version that are the same on every node; for the "no loss" guarantees, a
**synchronous standby in every group that must not lose a commit**:
`synchronous_standby_names = 'ANY 1 (<standby>, ...)'` on its primary (the standby that
the control plane promotes must be one of those that can have acknowledged
everything). `synchronous_commit` of any session, role or database may be anything:
for the work of a distributed transaction the extension raises it to at least `on` on
every node that takes part (see 11). A group whose primary has no synchronous standby
(or whose standby the control plane removed from `synchronous_standby_names` after a
grace period) gets the weaker guarantees of 12.

| # | Guarantee | Needs | Test |
|---|---|---|---|
| 1 | A standby owns no buckets and is skipped by routing, catalog synchronization, writes to reference tables, the fan-out, scans and bucket moves. Recovery looks at a standby only when it is out of recovery (a standby that `promote_node` is promoting, see 7) | - | `t/048`, `t/054` |
| 2 | A standby may be absent or not streaming without harm to the primaries. One that cannot be reached at registration is registered with a warning. An absent standby costs a recovery pass one failed connection attempt (at most `kingres_shard.node_connect_timeout`) | - | `t/048` |
| 3 | A standby is checked when it can be reached: in recovery, same major, a copy of its group's primary (same system identifier) | - | `t/048` |
| 4 | `check_cluster()` and `standby_status()` report a standby that is unreachable, not streaming, not in recovery | - | `t/048` |
| 5 | A node that the catalog does not call the primary of its group is in no group for routing: it forwards its clients' statements, refuses forwarded fragments with `KS001`, reads every group | - | `t/049`, `t/063` |
| 6 | It does not stop an application that writes ordinary tables on a demoted node: the control plane must fence it | fencing | `t/049` |
| 7 | `promote_node` runs on every node that can be reached, in one two-phase transaction, **resumably**, and says what it did (below) | - | `t/049`, `t/063`, `t/064` |
| 8 | A statement in flight across a promotion answers completely and correctly, or fails with an error that can be retried; it never answers with a part. A scan or fan-out planned against the old primary fails with `KS001`; a cursor and its rescan behave the same; `COPY (SELECT ...) TO` reads every row; `COPY FROM` into a distributed table is routed (a primary that changed between its plan and its first batch fails it with `KS001`) | - | `t/049`, `t/054`, `t/069` |
| 9 | The map group is promoted from the promoted node only when it reaches the primary of a **strict majority of the groups other than the map group** (a cluster with no other group has no electorate; a tie is not a majority), so two sides of a partition cannot both promote it; **no catalog change of any kind is accepted from a node that is not the map primary by such a claim** (S9-02): a standby of the map group that was promoted in PostgreSQL still holds the old primary's catalog and node id, and until `promote_node` has run on it with the majority it refuses `promote_node` of an ordinary group, `add_node`, `remove_node`, `sync_catalog`, `rotate_secret`, `set_node_property`, `move_bucket`, `create_distributed_table` and DDL on distributed tables ("this node was promoted in PostgreSQL after the catalog named it the map group primary"); a group counts as reached only when its primary answers now and proves it is one (out of recovery, knowing itself by its id); a node that missed a promotion cannot override the new catalog (catalog term, below) and learns it from the first node it talks to. The rule does not stop the old primary from taking writes: that is fencing | fencing | `t/063`, `t/064`, `t/071`, `t/077` |
| 10 | The identifier of a two-phase transaction names the origin group, the timeline and a keyed tag; recovery touches only the identifiers whose tag verifies; the older forms are resolved if prepared before the node came to 0.5; `rotate_secret()` refuses while a transaction with a valid tag is prepared on any node; **a node that holds a rotation prepared can always be reached by the node that can finish it** (S9-03) | cluster secret | `t/050`, `t/051`, `t/067`, `t/072`, `t/076` |
| 11 | **With a synchronous standby the outcome of a distributed transaction is atomic across a failover of the origin's group or of a participant's group.** The second phase is sent only after the origin's commit record is known to be acknowledged as the configuration requires. A participant's `PREPARE` counts as done only when the participant **confirms** that its record is durable by its own rule (`internal_sync_confirm`, a result of the same batch, not a warning): otherwise the origin aborts and the client gets `KS005`, nothing committed. A participant's `COMMIT PREPARED` is asked the same; an unconfirmed one leaves the transaction committed everywhere, the client gets a `WARNING`, and **the decision is kept until the participant's standbys have confirmed everything the participant flushed, with no time limit** (S9-01; `decision_retention` counts only from then on) so that recovery commits the part again whenever the participant fails over to a standby that lacks it. Recovery asks the same of the `COMMIT PREPARED` it sends. All of it is acknowledged by the participant's own synchronous standby whatever its `synchronous_commit` is. The weaker case that PostgreSQL itself has: a list of standbys that is emptied during a wait releases the wait, and then nothing is required | synchronous standby | `t/050`, `t/061`, `t/062`, `t/070` |
| 12 | **Without one** a failover can lose a committed transaction's part, on the origin or on a participant; the extension notices and records it (`kingres.dtx_incident`, `check_cluster()`): `prepared_rolled_back_after_failover` and `participant_lost_committed_transaction` | `decision_retention` for the second | `t/050`, `t/068`, `t/075` |
| 13 | Distributed deadlock detection finds a cycle across nodes and cancels the youngest transaction; **no false positive**, and no label a client can set is believed: the identity of a remote session comes from the node that opened it, kept in shared memory by a session that proved to be a node session | - | `t/052`, `t/065` |
| 14 | The victim gets SQLSTATE `40P01`, `distributed deadlock detected`; a victim mark that did not lead to a cancel of its wait never changes another error | - | `t/052`, `t/065` |
| 15 | `create_restore_point`: barrier, wait for second phases, restore points on every primary; the barrier is lowered on every way out of the function (cancel, timeout, termination of the backend, error, loss of a node connection) and a node drops the barrier of a session that ended | - | `t/053`, `t/066` |
| 16 | All of these functions are for superusers; the node-to-node helpers check a proven node session or a superuser (`internal_dtx_applied` and `internal_sync_confirm` are executable by every role, because a participant runs as the role of the user, and refuse all but a node session and a superuser; `internal_sync_probe` is for superusers) | - | `t/065`, `t/067`, `t/068` |

### Standby nodes

`kingres.add_node(host, port, group_id, 'standby')` on the map group primary
registers a standby of an existing group. A standby

- **owns no buckets**: the buckets belong to groups and are reached through the
  primary of the group; a standby is skipped by routing, by the catalog
  synchronization (it gets the catalog by physical replication, as it gets
  everything), by writes to reference tables, by the fan-out of `UPDATE` and
  `DELETE`, by the distributed scan and by bucket moves. Recovery of prepared
  transactions skips it too, **until it is out of recovery**: the standby that
  `promote_node` is promoting holds the prepared catalog change of a promotion that
  stopped half way, and recovery finishes or rolls it back (guarantee 7);
- **is skipped by the version and major checks that connect to nodes**: a physical
  copy has the PostgreSQL major, the extension version and the catalog of its
  primary by construction. What can differ is its *library* (a standby that was not
  upgraded with its primary), and that matters at promotion, so `promote_node`
  checks the library of the node it promotes;
- **may be absent or not streaming** without harm to the primaries:
  `synchronous_standby_names` is the control plane's business;
- is checked when it can be reached: it must be in recovery, run the major of this
  node and be a copy of the primary of its group (same system identifier).

`kingres.remove_node(node_id)` removes a standby (also of a group that owns
buckets). A primary that still has standbys cannot be removed before them.

`kingres.standby_status()` (map group primary) returns one row per standby with its
**replication state as its primary sees it** (`streaming`, `catchup`, ..., `not
connected`), the synchronous state (`async`, `sync`, `potential`), the replay position
and lag in bytes, and `problem` and `detail` when something is wrong.
`kingres.check_cluster()` includes the rows with a problem: `standby_unreachable`,
`standby_not_streaming`, `standby_not_in_recovery`, `standby_without_primary`.

A node's `local_node` row says which node it is. A standby holds the row of its
primary; `promote_node` sets it. The token that proves a node-to-node session is made
for that id; a node that holds a promotion prepared and does not yet know itself by its
new id is tried with the other ids of its group.

A node that the catalog does not call the primary of its group (the old primary after
`promote_node`) is in **no group** for routing: it runs no statement of a distributed
table as local, forwards the statements of its clients to the primary of the group,
reads every group in a distributed scan, and refuses a forwarded session with the
retryable `KS001`. It does not stop an application that writes ordinary tables on it:
it must be fenced (stopped, or rebuilt as a standby) by the control plane.

### The catalog term

`kingres.map_version.term` is a number in the catalog, raised by **every
`promote_node`**. It is carried by every catalog push (the `internal_apply_catalog`
snapshot) and by every remote transaction a node opens (`kingres_shard.forward_term`
and `kingres_shard.forward_origin`, set with `SET LOCAL` by the node session; a plain
client cannot set them).

- A node **refuses a catalog push** (SQLSTATE `KS002`) whose term is older than its
  own, and **refuses a statement of a session** whose sender's term is older (`KS002`;
  once per transaction). The refusal says which term the refusing node holds.
- The sender that is refused learns it, in a background worker of its own, as the
  superuser: a node that is the **map group primary according to its own catalog** has
  been **superseded**: `kingres.catalog_status` shows `superseded = true` and
  `superseded_by_term`; it refuses every catalog change (`add_node`, `sync_catalog`,
  `create_distributed_table`, `promote_node`'s non-map form, the detector, ...) with
  "this node has been superseded as the map group primary" until it is rebuilt as a
  standby of the current map primary. Any other node had only missed a promotion: it
  fetches the newer catalog from the node that refused it (or from the other
  primaries) before it does anything else.
- A node that receives a statement whose sender holds a *newer* term brings itself up
  to date first, the same way, and refuses the statement with `KS001` if it cannot.
- `kingres.catalog_status` (term, superseded, map version) exists on every node, hot standbys included, and is for superusers.

What the term does not do: it cannot stop a node that is not told and does not meet a
node that knows (two old nodes talking only to each other, one of them the old primary
of a group that was promoted while it was unreachable). Fencing the old primary of a
group is the control plane's job (guarantee 6); the term closes the rest.

### `kingres.promote_node(node_id int) returns jsonb`

Run by the control plane **after** the old primary was fenced and the standby was promoted in
PostgreSQL (`pg_promote`); the preconditions are in "What the control plane calls". It refuses
unless the node is out of recovery, runs the same extension version and library, **and can
commit**. The answer is `{"status": ..., "term": <catalog term>, "final": <bool>,
"unreached": [<node ids>], "blocked_by": [<prepared transactions in the way>]}`. `final` is `true` only for `already`: the answer is made before the
commit of the call, and a commit that waits for the synchronous standbys of the node that runs it can
be ended by a cancel, a termination or a shutdown with a warning and an ordinary result, leaving the
change prepared on the other nodes (the case found in review: S8-02). `promoted` and `completed`
therefore say "this call did the work", `"final": false` says "not yet known to be done", and
**the caller repeats the call until it answers `already` (`"final": true`)**: a repeated call finishes
what a deferred commit left (it answers `completed`) and then answers `already`:

| `status` | meaning |
|---|---|
| `promoted` | the node was a standby in the catalog; it is the primary of its group now, as far as this call knows (`final` is false) |
| `completed` | the catalog already called it the primary but the promotion was not carried through (a catalog change was left prepared, or a node did not hold the catalog); this call finished it (`final` is false) |
| `already` | nothing was left to do (a notice says so); `final` is true |

`unreached` lists the nodes that were **not told**, as the call sees them now, **every time it
is called, the repeated call included** (S8-05): the primaries that cannot be reached and
the other nodes of the promoted group that cannot be reached (the old primary among them). A
node of the group that can be reached and is out of recovery (an old primary that is running, another
promoted standby) is told, and a repeated call tells it again if it holds an older term.

`blocked_by` names, node by node, the prepared transactions that hold a lock on the catalog's version row that a catalog change conflicts with (`{"node": N, "prepared": [{"gid": ..., "class": "cluster" | "foreign"}]}`) and that a repeated call found in the way (S9-04): `cluster` ones are resolved by recovery; a `foreign` one (a prepared transaction that is not the cluster's, made by a superuser) is for the operator (only a superuser can take a conflicting lock), and the caller stops repeating after two identical answers (README sequence 2). A lock of `ACCESS SHARE`, the only one an ordinary role can take, blocks nobody and is not counted.

**The old primary is not waited for (S9-05).** When OLD can be reached it takes part in the call like every other primary, and its `PREPARE` and `COMMIT PREPARED` do not wait for its synchronous standbys and are not asked about them: it is about to stop being a primary, its list names the node that was just promoted, and its confirmation is not what makes the promotion durable. The decision rows of such a participant are marked and are not looked at for a lost part (S9-06). `t/079`.

**The node must be able to commit (S8-02).** A promoted standby has the
`synchronous_standby_names` of the primary it was copied from. A list that names standbys that
are not connected to it makes every commit wait for ever, and `promote_node` used to hang, or
to end with its second phase deferred and the answer `promoted`. Before it changes anything the
call writes a WAL record on the node that runs it (and asks the node that is to be promoted to
do the same, for an ordinary group) and waits at most `min(node_timeout, 5) - 1` seconds for the
standbys to confirm it; when they do not it refuses ("node N cannot commit: its synchronous
standbys do not confirm" / "this node cannot commit ..."), with the list in the detail and what to
change in the hint, and nothing is changed or left prepared (`t/071` A and B). What the extension
does about the inherited list: nothing, it never edits the configuration of a node; the control
plane sets it (precondition 3). A participant of the call (a primary of another group) whose
standbys do not confirm makes the call fail with `KS005` before its decision (`t/070`).

In one two-phase transaction:

1. the node row of the promoted standby becomes `primary` and the former primary's
   `standby`; the **catalog term is raised** (above every term the call sees);
2. the promoted node's `local_node` is set to its own id;
3. every node that applies the catalog drops its idle cached sessions to nodes that
   are no longer primaries.

**Which nodes must be reachable.** For an ordinary group (the call runs on the map
group primary): the promoted node. Every other node is told when it can be reached; one
that cannot (including the former primary) is skipped with a warning and listed in
`unreached`; it takes the new catalog from `sync_catalog()` or learns it by itself the
first time it talks to a node that knows (the term). Two groups that failed together
are promoted one after the other. For the **map group** (the call runs on the promoted
node, which refuses unless it is out of recovery, is a copy of the old map primary and
listens on the port of its row) the rule is the **majority of the other groups** (S8-04):
the electorate is the groups other than the map group (the map group is the one being
replaced, and it has a node on each side of a partition: the old primary on one, the
promoted standby on the other, so counting it would give both sides a majority); the
promoted node must reach the primary of a **strict majority** of them. Two sides of a
partition cannot both reach more than half of the same electorate, so they cannot both
promote the map group; an even electorate that splits in two halves lets neither side
promote (a tie is not a majority); a cluster whose only group is the map group has no
electorate and its standby is promoted by the call alone; a cluster of two groups needs the
other group to be reachable. Otherwise the call refuses ("only k of the n other shard
groups can be reached from this node; the map group cannot be promoted from a minority of
them") and changes nothing. The rule is about who may *promote*; it does not stop the old
primary, or a side that does not promote, from serving its own group's data (guarantee 6): fencing
does, before the call. Tested by stopping the nodes of the other side, for three and four groups, two
standbys of the map group on the two sides, one and two groups (`t/063`, `t/071`).

**Without a majority (S9-10).** `kingres.promote_map_group_without_majority(node_id, acknowledgement)` is the operator's way out when the rule refuses for ever; see README sequence 3. It is `promote_node` for the map group without the electorate and with a catalog term 1000 above the highest seen. `t/081`.

**Resumable.** A failure after the decision of the two-phase transaction (the map group
primary stopped, a `COMMIT PREPARED` failed) leaves the change prepared on the promoted
node. Recovery of the origin finishes it (the worker, or `recover_prepared_transactions()`
on the node that called `promote_node`; it looks at the promoted node although the
catalog does not yet call it a primary); and **calling `promote_node` again for the same
node** finishes whatever is left (resolving the prepared change, pushing the catalog to
the nodes that miss it, setting `local_node`) and answers `completed`; when nothing was
done before the decision, the repeated call promotes (`promoted`). It is safe to call
any number of times. Tested with a kill of the origin at every fault point of the
function and of the commit protocol (`t/064`).

**In-flight statements.** The *placement* of a bucket, which a distributed scan and a
fan-out write compare between the node that plans them and every node they use, is the
group *and the node that is the primary of the group*. A scan or fan-out planned
against the old primary fails with `KS001` ("the bucket map changed ..."); the client
repeats the statement. A statement routed to one group that reaches the old primary
finds it demoted and gets `KS001`, and the node that sent it reloads its catalog and
repeats it. A **cursor** that is open keeps the sessions it opened and answers from the
snapshot it started with (completely) or fails with an error that can be retried; a
rescan after the promotion likewise; `COPY (SELECT ...) TO` after it reads every row from the new primary (`COPY FROM` into a distributed table is run by the row router: a group that changed its primary in the middle of it fails it with `KS001`, nothing stored); a transaction
that had already done work on the old primary is not rescued: it fails, or, if the old
primary is gone, its prepared transaction is resolved by recovery as below (`t/049`,
`t/054`, `t/069`).

### Prepared transactions across a failover

The identifier of a two-phase transaction is
`kingres_h<origin group>_<timeline>_<pid>_<xid>_<target node>[_<n>]_<tag>`. It names the
origin **group**, so the node that became the primary of the group resolves what the old
primary started, the **timeline** the origin was on, and carries a **tag**: 16 hex digits of
an HMAC of the rest of the name with the cluster secret. Any role can `PREPARE TRANSACTION`
under any name; recovery therefore touches a name only when its tag verifies. A name of
that form with a wrong tag is left alone and `check_cluster()` reports it, once per name,
as `foreign_prepared_transaction`; it never produces a failover incident. The longest name
the extension makes (a three-digit group, a ten-digit timeline, pid and node, a
twenty-digit transaction number) is under 100 bytes; PostgreSQL's limit is 200.
`kingres.rotate_secret()` changes the tags, so it **refuses while a transaction with a valid
tag is prepared on any node** ("the cluster secret cannot be rotated while distributed
transactions are in doubt"; the detail says which nodes): resolve them first with
`recover_prepared_transactions()`. A name whose tag does not verify does not count, so a role
cannot stop a rotation by preparing one. A transaction that prepares in the instant between the
check and the commit of the rotation is not stopped; if one is stranded that way,
`check_cluster()` says that a foreign name can be one that was prepared around a rotation, and
how to resolve it by hand (`COMMIT PREPARED` when its gid has a row in `kingres.dtx_log` on the
origin, `ROLLBACK PREPARED` when it has none) (`t/072`).

**Upgrade.** The names that 0.4 and the first 0.5 made carry no tag. They are resolved
**when they were prepared before this node came to 0.5**: the moment is
`kingres.dtx_epoch.old_format_until` (the time of the upgrade); a name of an older form
prepared after it is not the extension's and is left alone. (Before the upgrade a role
could prepare such a name; that is the 0.4 situation.) `t/051` and `t/067`.

The decision log `kingres.dtx_log` is an ordinary table: the promoted standby has it, up
to what was replicated. Recovery on a node resolves the transactions of its group (it
does nothing on a node that the catalog does not call a primary). For a transaction of an
*earlier timeline* it takes "the origin transaction is no longer in progress" as given
and goes to the decision log, looked up with a fresh snapshot: a decision row means
commit, no row means rollback. A rollback of this kind is written to `kingres.dtx_incident`
and shown by `check_cluster()` as `prepared_rolled_back_after_failover` (kept 30 days).

**The decision is durable before it is acted on.** The origin writes the decision as a row
of `kingres.dtx_log` in its own transaction, which commits locally, and the commit waits
for the synchronous standbys as PostgreSQL does. PostgreSQL ends that wait with only a
warning when the backend is cancelled or terminated (or the server is shut down): the
commit has happened locally and nothing says whether the standby has it. The extension
therefore asks, after the commit, whether everything up to the commit record has been
confirmed by the standbys that `synchronous_standby_names` requires (the position
`WalSndCtl->lsn[mode]` that the walsenders release waiters with, read under
`SyncRepLock`; the same test `SyncRepWaitForLSN` makes, with the same rules for a standby
that is not defined, for the first moments after a start and for `remote_apply`). If it
is confirmed (the usual case, and every case without a synchronous standby), `COMMIT
PREPARED` is sent. If not, the session **does not send it**: the participants stay
prepared, the client gets a `WARNING` ("the commit ... was not confirmed by the
synchronous standby; the second phase was not sent ... the outcome is decided by
recovery") in addition to PostgreSQL's own, and the transaction's outcome is decided by
recovery: the recovery worker (and `recover_prepared_transactions()`) acts on a decision
row only after everything flushed on the node so far has been confirmed by the standbys
(it waits for that for at most `min(node_timeout, 5)` seconds per pass and tries again at
the next); and after a failover it acts on what the promoted node has, which is the rule
above. On PostgreSQL 18 and 19 the code is the same (`WalSndCtl`, `SyncRepLock` and the
two status bits are the same in both). Tested with `pg_cancel_backend`,
`pg_terminate_backend` and a fast shutdown of the origin during the wait, on both majors
(`t/061`).

**The participant's side.** In the remote transaction that a node opens for two-phase
work, `synchronous_commit` is raised to at least `on` (`remote_apply` is kept) for the
`PREPARE TRANSACTION`, and on the same connection for the `COMMIT PREPARED` or `ROLLBACK
PREPARED`, and for the commands of recovery, whatever the session, role or database
setting of the participant is. The next remote transaction on the connection resets it.
(`t/062`.)

**The participant says whether its record is durable (S8-01).** PostgreSQL ends the wait of
`PREPARE TRANSACTION` and of `COMMIT PREPARED` for a cancel, a termination or a shutdown
with a warning and an answer of success; the origin does not see the warning. So the batch
that prepares a participant ends with `SELECT kingres.internal_sync_confirm(<ms>)`: *is
everything this node has flushed so far confirmed by its synchronous standbys as its own
configuration requires?* It takes the flush position once, then waits at most
`min(node_timeout, 5)` seconds for the standbys to confirm it (a busy participant's standby
needs a few milliseconds to catch up with the writes of other sessions), and answers `true`
or `false` in a result of the batch. An empty list of standbys, or a node without walsenders,
requires nothing and answers `true`, as a commit does. What the origin does:

| when | participant's answer | origin |
|---|---|---|
| PREPARE, before the decision | `true` | goes on |
| PREPARE, before the decision | `false`, an error (the wait was cancelled again), no answer, or the node did not answer within `node_timeout` | the prepare failed: the transaction aborts, the client gets an error with SQLSTATE `KS005` ("did not confirm that its part of the transaction is durable"), nothing is committed anywhere; the prepared transactions are rolled back (`ROLLBACK PREPARED`, bounded by `node_timeout`, else recovery finds no decision row and rolls them back) |
| COMMIT PREPARED, after the decision | `true` | done |
| COMMIT PREPARED, after the decision | `false` or no answer | the transaction stays committed everywhere (it cannot be undone), the client gets a `WARNING` that names the node, and the decision row is **kept until the participant's standbys confirm** (S9-01): if the participant fails over to a standby that lacks the commit, its part comes back prepared and recovery commits it again, however long after the commit that is |
| COMMIT PREPARED sent by recovery | `false` | `recover_prepared_transactions()` and the worker log it, count the transaction as resolved and keep its decision row until the participant's standbys confirm |

A node that gets the extension in the very transaction that is prepared (`add_node`) has no
schema to ask after the PREPARE; it is not in the cluster yet and the question is skipped for it.
Tested for the cancel and the termination of the participant's backend, its fast shutdown, the
emptied list, the cancel of the second phase and recovery's, with a failover of the participant afterwards
(`t/070`, both majors).

**What bounds a stall (S8-08).** A participant whose standby is down makes the origin's commit
wait, and PostgreSQL has no timeout for a commit that waits for a standby (`statement_timeout`
does not apply to it). The extension bounds the waits that it starts: the wait for a participant's
`PREPARE` and for its answer is bounded by `kingres_shard.node_timeout`; when it runs out the origin
cancels the participant's backend (which ends the wait with a warning), the participant answers
"no" after at most `min(node_timeout, 5)` seconds, and the origin aborts with `KS005`: the client
waits at most about `2 x node_timeout + 5` seconds, holds row locks for that long, and gets an
error that says which node and what bounded it. The second phase is bounded by `node_timeout` per
participant (then "recovery will commit it"), and so is the `COMMIT PREPARED` or `ROLLBACK PREPARED` that recovery sends, also from `recover_prepared_transactions()` in a session of a user (S9: it used to wait for ever for a standby that is away; after `node_timeout` it is cancelled, the participant has completed it locally, and its standby is asked afterwards). After the cancel of a `PREPARE` the origin waits for the participant's own answer, `min(node_timeout, 5)` seconds, before it gives up. The state left: nothing committed anywhere, a
participant's prepared transaction that is rolled back by the origin or, when it cannot be reached, by
recovery. The client's own cancel on the origin has always been safe and ends the wait at once (the
transaction aborts before the decision, or is left to recovery after it). The operator's way out of a
stall: start the standby, or take its name out of the participant's `synchronous_standby_names`
(`ALTER SYSTEM SET ...; SELECT pg_reload_conf()`: the waiting backends are released at once), or
wait for `node_timeout`. **Cancelling the participant's backend is safe now**: the commit fails, it no longer
succeeds without the part. A commit that writes in one group only, or an origin that waits for its own
standby, has PostgreSQL's wait: cancel it (the second phase is then deferred to recovery, `t/061`).
A **smart** shutdown of a node whose commit waits for its standby does not end the wait: the
postmaster that is shutting down refuses the standby's connection, so the standby cannot come
back to confirm; use a fast shutdown (`t/070` 8).

**What is guaranteed, and what is not.**

- *With a synchronous standby* in the origin's group and in the participants' groups: the
  commit of the origin is acknowledged by its standby before the first `COMMIT PREPARED`
  (or the second phase is deferred to recovery), and the `PREPARE` of a participant is
  acknowledged by the participant's own standby before the origin decides, so after a
  failover of any of the groups the outcome is the same on every node: committed where
  the decision reached the promoted node, rolled back everywhere otherwise (tested at
  every point of the protocol, `t/050`, and with the commit wait ended early, `t/061`).
  Writes of a participant's group that were acknowledged by its standby survive its
  failover.
- *With an asynchronous standby*, or when the group runs without its synchronous standby:
  the primary can commit and tell participants to commit before the commit record has
  reached the standby. If that primary is lost, the promoted standby has **no decision**,
  the participants that were still prepared are rolled back, and the ones that had
  committed stay committed: **a half-committed transaction**. The extension cannot prevent
  it (it is the loss that asynchronous replication allows); it **records** it: a `WARNING`
  in the log of the promoted node, a row in `kingres.dtx_incident` and
  `prepared_rolled_back_after_failover` in `check_cluster()` for each prepared transaction
  rolled back for lack of a decision (`t/050`, last scenario). A transaction whose
  participants had *all* already committed, and whose decision was lost with the old
  primary, leaves no trace: the origin's part is gone.
- A participant whose *own* primary fails over with an asynchronous standby that did not
  receive its part loses that part of a transaction that the origin committed. The
  extension **notices**: every participant writes the identifier into its own
  `kingres.dtx_applied` in the transaction it prepares (so the row exists exactly when the
  part is committed and replicates with it); the origin keeps its decision rows for
  `kingres_shard.decision_retention` seconds (default 300) after the transaction when the
  participant's group has a standby; when the primary of that group has changed, the
  origin's next recovery pass asks the new primary for the marks of those decisions and
  records every transaction without one in `kingres.dtx_incident` as `participant_lost`;
  `check_cluster()` shows it as `participant_lost_committed_transaction` (`t/068`). The
  look is made once per group failover and costs nothing before it. A loss that happens
  after the retention is over is not found.
- **A decision is never forgotten while a participant might still hold the transaction
  prepared (S9-01).** The retention above starts only when the participant's synchronous
  standbys have confirmed everything it flushed, the commit of its part included (the origin
  asks the participant after it has listed its prepared transactions; `kingres.dtx_log.durable`
  records the answer). Until then the decision rows of that participant are kept without limit
  and `check_cluster()` reports `decision_unconfirmed` (node, since when, how many). What grows
  without bound while a participant's standby never comes back is those rows (a few dozen
  bytes per distributed transaction that touched it) and, in its group's WAL, nothing more; the
  operator ends it by starting the standby, or by taking its name out of the participant's
  `synchronous_standby_names` (then the participant requires nothing, the rows are released and
  the retention applies as in the weaker case), or by `remove_node` of the standby. A prepared
  transaction that recovery cannot match to a decision is rolled back as presumed abort only
  while the origin could not have decided: when it is older than `decision_retention` (a decision
  of it might have been forgotten, which only a participant that required no confirmation allows),
  the rollback is recorded as an incident `rolled_back_after_retention` and shown by
  `check_cluster()` as `prepared_rolled_back_after_failover`, so that it is never silent
  (`t/075`). The same rule holds when the origin itself fails over, the retention setting is
  changed, or every node is restarted: the state is in `kingres.dtx_log`, which replicates, not
  in memory.
- Writes acknowledged to a client by a primary whose standby did not have them are lost
  as in any asynchronous replication.

### Distributed deadlock detection

The recovery worker of the map group primary runs one pass every
`kingres_shard.deadlock_check_interval` seconds (default 2; 0 disables): it calls
`kingres.detect_distributed_deadlocks()`.

- **Identity.** A remote session that a node opens for a distributed transaction says
  whose transaction it works for with `SET LOCAL kingres_shard.forward_dtx =
  '<origin node>:<origin backend pid>:<start of the origin transaction>'` in the first
  command of its remote transaction. A session that **proved to be a node session**
  (the token derived from the cluster secret, forward_auth) has it recorded in shared
  memory (one slot per backend, bounded by `max_connections` and the auxiliary
  processes) and removed with the transaction; any other session cannot set it
  (`kingres_shard.forward_dtx` is refused to it) and every other backend is its own
  origin, with the node, pid and transaction start that `pg_stat_activity` says.
  `application_name` is **not looked at**: a client that sets one that looks like a label
  gets a label of its own. `kingres.internal_dtx_identity(pid)` (superusers) reads a slot.
  (`t/065`.)
- `kingres.internal_wait_edges()` reports, on each primary, the waits older than
  `deadlock_timeout` as edges "transaction A waits for transaction B" (from `pg_locks` and
  `pg_blocking_pids`, with the start of the wait). Collected edges are validated before
  they are used: an entry that is malformed is ignored and logged once per pass and node,
  and never stops the pass; a cycle can contain only labels that parse.
- The pass collects the edges of every primary; if they contain a cycle it waits
  `kingres_shard.deadlock_confirm_delay` (200 ms) and collects again.
- **No false positive:** the edges were read at different moments on different nodes, so a
  cycle in one reading may never have existed. A cycle is acted on only if every one of
  its edges is in both readings as *the very same wait*: the same backend, in the wait that
  began at the same time, blocked by the same transaction. A wait is one uninterrupted
  period, so each such edge existed during the whole interval between the two readings,
  and all of them at the same time: a deadlock, which nothing resolves by itself. A cycle
  whose edges come from one node only is PostgreSQL's; the detector acts only on cycles
  with edges of two or more nodes. What the argument does not cover: a transaction that
  releases a lock and takes it again between the readings while the waiter stays queued
  behind it; and the instant between the check and the cancel on the node.
- The victim is the **youngest transaction of the cycle**. Its wait is cancelled where it
  is (on its origin node or on the node whose lock it waits for; the error goes back to the
  origin). It receives **SQLSTATE `40P01`**, message `distributed deadlock detected`: class
  40 is "transaction rollback", which drivers and ORMs already retry. The error is made
  from a query cancel by `emit_log_hook` of the library, for a backend that carries a mark
  made for **the very wait that was cancelled** (the start of the wait must lie in the
  statement the error ends; the detector removes a mark that the victim's error did not
  consume). The hook runs for every error that is reported to the log, which at the
  default `log_min_messages` (`warning`) is every error; it is skipped only when
  `log_min_messages` is above `error` (`fatal`, `panic`): then the victim still ends, with
  the plain "canceling statement due to user request" (57014).
- Not covered: waits that are not lock waits, advisory locks held across nodes by sessions
  that do not belong to one distributed transaction, and cycles through a prepared
  transaction (they end by themselves).

### `kingres.create_restore_point(name text)`

Map group primary, superuser. Returns `(node_id, group_id, restore_lsn)` for every
primary.

1. It raises a **barrier** on every primary: a transaction that is about to prepare its
   remote writers waits there. The barrier expires by itself after `2 x node_timeout + 10`
   seconds.
2. It waits until the number of distributed transactions that had passed the barrier
   (between their first `PREPARE TRANSACTION` and the end of their second phase) is zero on
   every node, for at most `node_timeout` seconds.
3. It calls `pg_create_restore_point(name)` on every primary.
4. It lowers the barrier and checks that it was up the whole time on every node.

**The barrier falls with its caller.** The session of a node that raised the barrier owns
it: when that session ends (the caller was terminated, the connection was lost, the
caller's server stopped) the node lowers the barrier at once. When the caller's
transaction aborts for any reason (a cancel, a `statement_timeout`, an error) the caller
lowers the barriers it still owes over the control sessions it holds (an abort callback);
a late lowering never lowers a newer barrier (the generation must match). The timer
remains the last resort. (`t/066`.)

**A restored map primary must claim the map group again (S9-02):** a restore ends on a new timeline, which is what tells the catalog that a node was promoted without it, so after the restore run `SELECT kingres.promote_node(<the map primary's node id>)` on the map primary (it needs the majority of the other groups, which are restored too; answers `completed` or `promoted`, repeat until `already`; `t/053`).

**Restoring every server to that name** (`recovery_target_name = name`) gives a state in
which no distributed transaction is half committed (tested with a restore of three servers
made under a multi-node write load: `t/053`).

What it does not give:

- **Standbys.** Only primaries get a restore point. A standby is rebuilt from the restored
  primary.
- **A bucket move in progress.** A restore to it brings back the move as it was at that
  moment; `kingres.resume_bucket_moves()` (the recovery worker does) undoes or finishes it.
- **Sequences.** The values handed out after that point are handed out again after the
  restore.
- **Time.** The restore points of the nodes are made at slightly different moments;
  statements that touch one node only and commit between the first and the last restore
  point are in the restored state of one node and not of another, which is not a
  half-committed transaction. A node with `wal_level = minimal` refuses.
- Transactions whose second phase failed or was deferred ("recovery will commit it") are
  over for the barrier; their prepared transactions are in the restored state and are
  resolved by `kingres.recover_prepared_transactions()` after the restore.

### What the control plane calls

Everything below is a single SQL call on a named node, and every call is safe to repeat.
Where a sequence says "answers", the list is complete for what a test has produced: a call can
end in one of those ways and in no other except a connection that breaks or a timeout of the
caller, which is treated as "the call did not finish, repeat it" (see sequence 9). Every answer
named here is produced by a test; the test is named (`t/073`, `t/082`, `t/084`, `t/086` and `t/091`
drive the sequences as written). The names used: **MAP** is the primary of the map group, **OLD** the
primary of the group that failed, **NEW** the standby that is promoted, **OTHERS** the other standbys of
that group.

**Matching an answer.** The text of an error is as stable as the table that lists it, and every
refusal that a caller reacts to has an SQLSTATE of its own as well (next section): match on the
SQLSTATE first, on the text when the SQLSTATE is shared. A `WARNING` or a `NOTICE` on the session is
information (a node that was not reached, a promotion that was already done), never a failure: a
caller that treats text on stderr as a failure stops a call that went well. **The standard of
every call: it never returns success for work that was not done on a node without saying so** (a
`WARNING` that names the node and what was left, as the tables below promise, or an error), and a
call that fails leaves the state that the table for that answer names. `t/100` and `t/101` hold every
call to it, with a node that goes silent at every point of the call. A `WARNING` that names a node
carries the SQLSTATE of the reason when there is one (`KS007` for a node that stopped answering), so
a program can tell it from a warning that only informs.

#### SQLSTATEs of the answers

The extension's own class is `KS`. A caller reacts to a class of situation, not to a sentence, so the
codes follow the reaction and not the sentence: several texts share a code when the caller does the
same thing for each.

| SQLSTATE | Meaning | What the caller does | Texts (unchanged) | Test |
|---|---|---|---|---|
| `KS001` | stale bucket map | retry (the statement was routed on an old map; a failover of a group of a read is reported the same way) | "the bucket map changed after the statement was planned", "this transaction already uses node ... of shard group ...", "node ... failed while a distributed statement was running" and the like | `t/002`, `t/026`, `t/105`, `t/106` |
| `KS002` | the sender's catalog is superseded (a newer catalog term exists) | stop sending from that node; `sync_catalog()` brings it up to date | "the catalog of the sending node (term T) has been superseded by term U" (the text is produced by `t/063`; its SQLSTATE is not asserted there) | `t/063` |
| `KS004` | a bucket move gave up waiting for its switch | the move goes back to catching up by itself | "the copy of bucket B did not replay up to the switch point within N ms" and the like | `t/026`, `t/028` |
| `KS005` | a participant could not get its own standby to confirm that its part is durable | fix that node's list or standby, repeat; nothing was committed | "did not confirm that its part of the transaction is durable on its synchronous standby" | `t/070`, `t/084` |
| `KS006` | a planned switchover cannot be proven loss-free | treat the promotion as a failover (sequence 5) | "the switchover cannot be proven loss-free: ..." | `t/085` |
| `KS007` | a node accepted the session and **one statement of the call was not answered** within `node_timeout`. Nothing else produces it: a node that answers every statement in time (however late, however many statements the call sends) never ends a call with `KS007` (S12-03); a lock wait that lasts `node_timeout` is a statement not answered. **The wait of the proof itself for a lock is one of them** (S13-03): the node checks the proof of a session when it reads the session's first statement, before the `SET lock_timeout` of that statement takes effect, so a session of the node that holds a lock the proof needs (for example `ACCESS EXCLUSIVE` on `kingres.cluster_secret`) ends the call with `KS007` after `T`, not with `55P03`, unless the role or the database of the node has a `lock_timeout` of its own shorter than `T` | do not repeat against that node: probe it (`SELECT 1` over a new session, `pg_stat_activity` and `pg_locks` on it), fence or restart it (sequence 9). A call that sends a thousand statements to a slow node takes a thousand times its answer, which is the bound of the call below, not a stuck node | "node N (H:P) did not answer within T seconds"; for the node to promote: "node N (H:P), which is to become the primary of group G, did not answer within T seconds". As a `WARNING` (the call goes on) it has the same SQLSTATE: `remove_node` of a node that stopped answering, `recover_prepared_transactions()` for a node it skipped | `t/086`, `t/096`, `t/095`, `t/100`, `t/101`, `t/102`, `t/104` |
| `KS008` | another call that changes the catalog is running on this node and did not end within `node_timeout` | nothing was changed; repeat later (one controller at a time) | "another call that changes the catalog is running on this node, and did not end within T seconds" | `t/086` |
| `KS009` | the node cannot commit: its synchronous standbys do not confirm | fix the list or the standby, repeat; nothing was changed | "node N (H:P) cannot commit: its synchronous standbys do not confirm" (and the same for "this node") | `t/071`, `t/081`, `t/084`, `t/091` |
| `KS010` | the call went to the wrong node: not the map group primary, a superseded one, a node that is not the standby of the map group, a node row that names another port than the server listens on, a standby that PostgreSQL promoted and the catalog has not recognised | send it where the message and the hint say (sequences 2 and 3) | "this function can only run on the map group primary", "this node was promoted in PostgreSQL after the catalog named it the map group primary ...", "this node is not a copy of the primary of group 0 ...", "node N is registered on port P, this server listens on port Q", "this node has been superseded as the map group primary" | `t/049`, `t/077`, `t/082`, `t/084`, `t/091` |
| `KS011` | the node to promote is still in recovery | promote it in PostgreSQL first, wait, repeat | "node N (H:P) is still in recovery", "this node is still in recovery" | `t/071` G, `t/082`, `t/091` |
| `KS012` | promoting would not be safe: a node holds a newer catalog term, or only a minority of the groups can be reached | not a retry: a decision of the operator (sequence 3) | "node N holds catalog term T, newer than ...", "only k of the n other shard groups can be reached ..." | `t/071` C, `t/077`, `t/084` C |
| `KS013` | distributed transactions are in doubt | run recovery, wait until `check_cluster()` shows none, repeat | "the cluster secret cannot be rotated while distributed transactions are in doubt" | `t/072` |
| `KS014` | two nodes hold cluster secrets that do not match, and the error does not say which of the two is behind: the node that refused (a standby that missed a rotation after the previous secret was no longer kept, asked about or to be promoted) **or the node that asked** (a map standby that missed a rotation and runs `promote_node` itself: the proof it sends is made with a secret that every other node has dropped), or a rotation was refused because a standby has not confirmed the last one. The first is decided by a fact the node reports (S12-05): the node refuses the proof of the session with **its own SQLSTATE `KS014`**, which it gives only when the proof does not match its secret; a node that belongs to a cluster but holds no secret refuses an ordinary role with `28000` and the detail "holds no cluster secret", which is **not** `KS014` (run `sync_catalog()` on MAP). The words of the message are the same for both and are never matched | **not a retry**: for the refused rotation, start the standby and let it replay, run `recover_prepared_transactions()` until it has confirmed, rotate again (or `remove_node` it); for a promotion, **first tell the two apart, then rebuild the one that is behind**: ask a third node of the cluster (a session from it to each of the two, or the same call from it); the caller that every other node refuses is the stale one, the node that only one node refuses is the one that is behind. Rebuild the stale one from a node that has the current secret (`remove_node`, `add_node`) or promote another standby of the group; never rebuild a healthy node because it was the first to refuse | "node N does not accept the session of this node: the two hold cluster secrets that do not match, and either may be the one that is behind" (the hint names both possibilities and says how to tell them apart), "the cluster secret cannot be rotated again yet: standby N has not confirmed the last rotation" | `t/095`, `t/101` (the rotation refused after a failover), `t/104` (the two refusals of a proof told apart) |
| `55P03` | the node's own lock wait ended: the first statement of a session of a call runs with a `lock_timeout` of three quarters of `T` (the statement itself, not the check of the session's proof, which runs before that setting takes effect: a wait of the proof for a lock is `KS007` unless the role or database of the node has a shorter `lock_timeout`, S13-03), and every statement does when the administrator gave the node's database or role a shorter `lock_timeout` (`ALTER ROLE ... SET`, `ALTER DATABASE ... SET`): another session holds a lock that the call needs. **A lock wait on a node without a `lock_timeout` of its own is not this answer**: it lasts `T` and is `KS007` | repeat when the lock is gone (look at `pg_locks` on the node named) | "lock not available on node N (H:P): ..." | `t/086` (the first statement), `t/104` (the role's `lock_timeout`) |
| `KS015` | the call took longer than `kingres_shard.control_call_timeout` **as a whole** (S13-02): the setting is `0` (no limit) unless the caller sets it. **It says nothing about any node**: every statement that was answered was answered in time, or the wait that was cut off was cut off by what was left of the call's time. It must not be read as "fence the node" | nothing was kept (the state is the one of any call that failed); repeat the call with a longer `control_call_timeout` or none, and look at the nodes that were slow only if it keeps happening | "the call took longer than kingres_shard.control_call_timeout (N seconds) as a whole"; the detail names the node the call was waiting for, when there was one, and says that nothing says it is dead or slow | `t/107` |
| `25001` | `recover_prepared_transactions()` and `promote_node()` (which runs a recovery pass first) in a `REPEATABLE READ` or `SERIALIZABLE` transaction: the call reads what other sessions committed meanwhile, which that snapshot does not show. A role whose `default_transaction_isolation` is one of them gets this for every call | run it in a `READ COMMITTED` transaction: `BEGIN ISOLATION LEVEL READ COMMITTED; SELECT kingres.promote_node(N); COMMIT;`, or give the role of the control plane `default_transaction_isolation = 'read committed'`. Nothing was changed | "recover_prepared_transactions() must run in a READ COMMITTED transaction" | `t/104` (recovery), `t/101` (`promote_node`) |
| `40P01` | distributed deadlock victim | retry the transaction | "distributed deadlock detected" | `t/052`, `t/065` |
| `55000` | the rest: a mistake of whoever calls (a node that is not a standby, a node of another major or extension version, the library not loaded, "not initialized"); no control plane reacts to them automatically | fix the input | (many) | `t/045`, `t/046`, `t/048`, `t/091` |
| class `08` | a connection could not be made or broke | repeat; if it keeps, the node is down | "could not connect to node H:P" | `t/084`, `t/091` |

Why these and not one per sentence: `KS009`, `KS011`, `KS012` and `KS013` each ask a different thing of
the control plane (fix a list, wait for PostgreSQL, stop and ask a human, run recovery), while the
refusals that all say "this is the wrong node" ask the same (call another node), so they share
`KS010`. `KS007` and `55P03` are told apart because the first must never be repeated against the
same node and the second may be, once the lock is gone. The remaining `55000` answers are errors of
the input, which a human reads. The `KS003` slot is not used; `KS015` is the next free code of the series.

#### Bounds that apply to every call

**Scope of the deadline (S11-01).** The deadline exists for the control-plane calls and only for them:
it is a value that a call sets on entry and puts back on every way out (also an error that a PL/pgSQL
block catches), so nothing of it is left in the session, the transaction or a node session after the
call. The statements it bounds are the ones that `add_node`, `remove_node`, `sync_catalog`,
`rotate_secret`, `promote_node`, `recover_prepared_transactions`, `create_distributed_table` and
`create_reference_table` send to a node themselves, including the copy of the existing rows by the
two `create_*_table` calls (a node that does not answer in `T` seconds ends that call with `KS007` and
the transaction rolls back; nothing is lost). It **never** applies to the statements of a user: the
planner's remote statements, scans and cursors, fan-out, `COPY`, DDL forwarded for a user, a query of
a user after a control call in the same transaction or in a later one, in a savepoint that was rolled
back, in a function with a `SET` clause (`t/093`): such a statement waits for a node as long as the
node takes: a call sets no `lock_timeout` in a remote transaction (it did until the round of S12, and
gave it back with a `RESET` at the end of the call, which could not be answered by a node that had
stopped, closed the session in the middle of the call's own work, and let the commit skip it: S12-01).
**A statement that was not carried out is an error that aborts the transaction, never a warning**; and
a node session that held the writes of the transaction is never closed without the transaction
knowing: the commit (and `PREPARE TRANSACTION`) then fails with an error that names the node
(`t/093`). **A call sends nothing to a node after its last piece of work**, and a session that holds
the call's own work is closed in these places only: the deadline of a statement (an error ends the
call), `remove_node`'s own decision to let a stuck node go (a `WARNING`), and the end of the
transaction (commit or abort, where every failure is a `WARNING` that names the node and what
recovery will do); `t/100` and `t/101` run a call against a node that goes silent after every chunk
it sends, from the first to the last, and judge each answer by that rule.

**What waits for how long at the end of a session's work (S13-01).** The statements a call sends while it
runs wait `T` each (below). The statements that the extension sends to a node **after** the call has been
left, when a subtransaction ends and when the transaction ends, used to wait for ever whenever the control
depth was back at zero: a call inside `SAVEPOINT` (psql with `ON_ERROR_ROLLBACK`, a driver's autosave, a
PL/pgSQL block with an `EXCEPTION` clause, a retry loop of `promote_node`) hung the backend at `RELEASE
SAVEPOINT` with the catalog lock held, so every other call that changes the catalog was refused with `KS008`,
until another session cancelled the backend. **`statement_timeout` does not cover the end of a statement**: the
commit of a subtransaction or of a transaction runs after the timer of the statement is disarmed, and a client
that has gone away does not end the wait either; only `node_timeout` bounds these statements, and only the
cancel of another session (`pg_cancel_backend`) ends one that is not bounded. A session of a node **carried
the work of a control call** when a statement of a control call was sent through it in the transaction; it
**carried only statements of a user** otherwise. Exactly:

| statement the extension sends | session that carried the work of a call | session that carried only statements of a user |
|---|---|---|
| `RELEASE SAVEPOINT` (a subtransaction commits) | at most `T`; then an **error** (`KS007`, or the node's own error) that names the node by its id and address; the session is closed, and the transaction can no longer commit (the commit raises `08006` that names the node): the sessions that were released before it keep the work of a subtransaction that the error rolls back | the same |
| `ROLLBACK TO SAVEPOINT`, and the `RELEASE` that follows it (a subtransaction aborts) | at most `T`; an abort cannot raise an error, so a `WARNING` (`KS007`, or `08006` for a session that broke) names the node, the session is closed (the node rolls the transaction back with it), and the commit or `PREPARE TRANSACTION` of the transaction fails with `08006` that names the node | the same |
| `COMMIT TRANSACTION` of a session that only read, or is the only writer (a transaction that needs no two-phase commit) | at most `T`; then `KS007` that names the node and says that whether it committed is not known; the session is closed and the transaction aborts. No call of the extension makes such a session today (its calls write on the origin, so their remote writers are prepared, not committed): the bound is for the day one does | **no bound**: the node may take long for honest reasons (a deferred constraint); as before, only a cancel from another session ends the wait |
| `PREPARE TRANSACTION` | `T`, the cancel, and the answer to its question (the table below) | the same: it is bounded for every session |
| `COMMIT PREPARED`, `ROLLBACK PREPARED` | `T`; then a `WARNING` and recovery (below) | the same |
| `BEGIN`, `SAVEPOINT` and every other statement of the call | `T` each, while the call runs | not sent by a call: a statement of a user has no bound |

`t/107` runs the first row and the user's `COMMIT` (a forwarder that is silent at that statement),
`t/109` and `t/110` run every call inside `SAVEPOINT` / `RELEASE`, inside a PL/pgSQL block with an `EXCEPTION`
clause and inside a transaction block that the caller commits.

Every statement that a call sends to a node waits for its answer at most `kingres_shard.node_timeout`
(60 s by default), **and nothing else limits a call that is working unless the caller asks for it** (S12-03; `kingres_shard.control_call_timeout`, below). A node whose every
answer comes within `T` never fails a call, however many statements the call sends it: the budget of
`2 x T + 2` seconds per node and call (S11-07) ended calls against a node that answered every
statement at 0.5 x `T` or 0.9 x `T` with `KS007`, which says "do not repeat against that node: fence
it", and a control plane would have fenced a working node. So the bound of a call is a formula, not
a budget: **`s x T` for each node, where `s` is the number of statements the call sends to that node**
(the table below counts them). `KS007` means one statement was not answered within `T`, and nothing
else: a program tells a slow node from a silent one by that code, and a slow call is a call that
succeeds late; a control plane sets its own client timeout from the formula (`s x T` for the nodes it
expects to be slow, plus the work on the nodes that answer) and does not treat a call that is merely
long as a node that is stuck. If a node does not answer a statement at all, the call ends at that
statement (an error, or a `WARNING` for a node the call can do without: `remove_node`, recovery,
`promote_node`'s other nodes) after `T` and the time to cancel the statement on the node (at most
`min(node_connect_timeout, T / 3)` s, at least 1 s); the node is **not asked again in the same call**,
and a node that could not be connected to is not connected to again in the same call. The connection
to a node is bounded by `node_connect_timeout` once per call and node; **a name that does not
resolve is not bounded** (libpq resolves before its timer starts): register nodes by address. A
statement that is slow on a healthy node (a trigger, a very large batch of a copy) is a statement
not answered when it takes more than `T`: raise `kingres_shard.node_timeout` for such a call. A node
that stops answering **between** two statements is found at the next one. The wait for a statement
is measured with the monotonic clock (a step of the system clock does not shorten it) and an answer
that is already in the socket when the time is up counts as an answer (a stalled backend does not
call a node silent that has answered). **What a call that is working costs while it works (S13-02).** The bound is per statement, so a call that is merely slow is not ended: its whole time is the number of statements times `T` for each slow node (the table below), and for all of that time the origin holds the catalog lock (every other call that changes the catalog is refused with `KS008` after `T`, and bucket moves and DDL that take the catalog lock wait) and every participant holds its remote transaction open and not yet prepared (catalog rows, new tables). At the default `T` of 60 s a node that answers every statement at 0.9 x `T` gives `sync_catalog` about 11 minutes and `promote_node` about 18. A control plane that cannot accept that sets **`kingres_shard.control_call_timeout`** (`0`, no limit, by default; `SET` by the caller): the call then ends with `KS015` after that many seconds, checked between statements, with each wait cut to what is left, plus the time to cancel the statement that was cut (at most `min(node_connect_timeout, T / 3)` s, at least 1 s). `KS015` says that the call took too long as a whole and **nothing about a node**; the state is the one of a failed call (nothing kept), and the control plane repeats with a longer limit or none, it does not fence a node because of it. The limit counts the statements the call sends, not the commit of the transaction that follows (that is bounded as the table below says). The other way out is to cancel the call from another session (`pg_cancel_backend`). `t/107` makes a node answer every statement at 0.9 x `T`: the call succeeds without the setting, fails with `KS015` with it, leaves the state of a failed call, and succeeds when repeated without it.

`T` below is
`node_timeout`; **tolerance of the tests: 4 s** for a loaded machine.

| what | bound | test |
|---|---|---|
| every call below, against a node that answers every statement late but within `T` (0.5 x and 0.9 x `T` are measured, with one, two and four such nodes) | **it succeeds, with no `WARNING` and no `KS007`, nothing prepared, the work done on every node, in at most `s x T` seconds for each slow node** (`s` below) plus the work on the nodes that answer (a fraction of a second) | `t/102` (every call but `promote_node`), `t/103` (`promote_node`) |
| `add_node`, `remove_node`, `sync_catalog`, `rotate_secret`, `recover_prepared_transactions`, `create_distributed_table`, `create_reference_table`, against a node that **stops answering** (silent after any chunk of any of its sessions, from the first to the last) | at most **`2 x T + 8 s`** from the first statement that is not answered, **plus `node_connect_timeout` once for connecting to it**: `T` and the cancel for a statement; at a `PREPARE TRANSACTION`, `T`, the cancel, and `max(T, min(T, 5) + 2)` more for the answer to its question, then the abort; a node that answers but whose standbys do not confirm: `min(T, 5) - 1` s per node, then `KS009` | `t/100` (every call, every position), `t/096` (the bound with the tolerance), `t/086` (a lock, a stopped backend), `t/091` (`KS009`) |
| `promote_node` | at most **`3 x T` for each node that does not answer**; a node that does not answer is listed in `"unreached"` with a `WARNING` that names it, unless it is the node to promote, which refuses the call with `KS007` (whether it accepts the session and does not answer its first, its second or its third question: `t/096`), or a node that stops answering after the promotion has begun on it, which ends the call with `KS007` and changes nothing (repeat it after the node answers: `promoted`); a participant that stops answering during the commit: `2 x T + 8 s` as above | `t/086`, `t/096`, `t/101` (every position, for a participant and for the node to promote), `t/103` |
| `check_cluster()` | at most **`2 x T` for each node that does not answer**; the node gets a row `node_unreachable` and the other nodes are still checked: the call does not fail as a whole | `t/086` |
| a call behind another call that changes the catalog | `T`, then `KS008` | `t/086` |
| client timeout of the control plane for any of these calls | **without `control_call_timeout` there is no bound for a call that is working**: the formula above (`s x T` for each slow node) plus the bound for a silent node for each node that may be stuck, plus 10 s; its expiry means "did not finish" and is **not** proof of a local reason: a call that works through a slow node can run that long with the catalog lock held, so cancel it from another session (`pg_cancel_backend`) before starting another. With `n` nodes of which `k` may be stuck: `k x (3 x T + 8) + 10 s` is always enough for the silent ones. With `control_call_timeout = L` the call ends in at most `L` plus the time to cancel one statement | `t/086` (the bounds), `t/082` (a cancelled call), `t/107` (`control_call_timeout`) |
| a call, with `kingres_shard.control_call_timeout` set | at most that many seconds plus the cancel of one statement, then `KS015` (S13-02). **Cut to what is left (review S14-02):** every statement of the call's sessions, the connect of a new session (its `connect_timeout`) and the first command of a new session. **Outside it, each with its own bound:** the raw sessions of `require_primaries_can_commit` (the start of `add_node`, `remove_node`, `rotate_secret`, `sync_catalog`), `ask_node` and the sessions of `promote_node` (`failover.c`), each bounded by `node_timeout` or `node_connect_timeout` per node, one after the other: with `k` silent nodes the worst case is `L + k x node_timeout` for these, not `L`. When the time runs out while a node is being waited for, `KS015` names that node (and says nothing is known against it) | `t/107` |
| the repeat loop of `promote_node` | at most **10 calls**, 2 s apart, ending at `already`. If `"blocked_by"` names the same transaction in two answers in a row, stop repeating: see the table of sequence 2. *Not produced by a test:* the number 10 and the 2 s are a recommendation; the tests loop at most 6 times (`t/078`) and end at `already` within 3 calls (`t/071`, `t/073`) | |
| a refused call (the node cannot commit) | the probe waits `min(T, 5) - 1` seconds **per node**, so a cluster with several nodes in that state is several waits in a row; then `KS009`, changing nothing. A commit that waits for a standby: `2 x T + 5` seconds, then `KS005` | `t/091`, `t/070`, `t/084` |
| the second phase of a distributed commit | `T` per participant, then "recovery will commit it" (`t/076` produces the warning; the bound itself is not asserted) | |
| waiting for the worker | `kingres_shard.recovery_interval` per pass (`t/009`); call `recover_prepared_transactions()` instead of waiting | |

**How many statements `s` a call sends to one node**, as the forwarder of `t/102` and `t/103`
counts them (the answers it holds back: the first statement of every session, the statements of the
call, the `PREPARE TRANSACTION` batch, the commit; the cluster of the tests has four nodes besides
MAP and no table unless the call makes it). `t/102` and `t/103` print the count of every cell and
assert the bound with it:

| call | `s` for a node | what makes it grow |
|---|---|---|
| `sync_catalog` | 12 | |
| `rotate_secret` | 11 | |
| `create_distributed_table` | 11 | the version handshake, the questions of the report about roles and functions, the shadow and its partitions, the swap and the final statements (0.7; it was 7 before) |
| `create_reference_table` | 11 + about one per batch of the copy (a batch is at most 20000 values: a table of 60000 rows of two columns is 16 statements in the test) | the rows and columns of the table |
| `recover_prepared_transactions` | 4 + 2 per prepared transaction of the node (6 for one) | the transactions in doubt |
| `add_node`, the new node | 15 | one statement per table of the cluster, one per foreign key and per object whose privileges are copied, one per batch of the copy of a reference table |
| `add_node`, a node already in the cluster | 8 | |
| `remove_node`, the removed node | 8 | |
| `promote_node`, the node to promote | 20 | |
| `promote_node`, a participant | 10 | |

**What the sweeps cover, and what the default run does not (S13-05).** `t/100` and `t/101` hold every call to the standard of the call with a node that goes **silent** after every chunk of its sessions, the call in autocommit, for every position, as before; the bound they assert is the one of the table above (`2 x T + 8 s` plus `node_connect_timeout`), not a wider one. `t/108` and `t/110` run the other three ways a node can fail at the n-th chunk of a session, which `t/KsProxy.pm` makes: **the connection is closed** after chunk n, **the node's answer to chunk n is lost** (the node did the work, the origin never hears of it), **the node answers chunk n with an error**; `t/109` runs the call inside `SAVEPOINT` / `RELEASE`, inside a PL/pgSQL block with an `EXCEPTION` clause, and inside a transaction block that the caller commits (`t/110` does the same for `promote_node`, which needs a new cluster for every position: its default run takes the savepoint variant only, `KINGRES_LONG_TESTS=1` adds the other two); `t/111` makes two nodes fail at two positions of one `rotate_secret`. The judge checks that a `WARNING` names the node that was left, and the state it reads for `sync_catalog` covers every catalog table the call writes (`node`, `colocation`, `dist_table`, `bucket_map`, the map version); it waits for a condition (no session of a node is still working for the call) and not a fixed time before it reads the state. **The default run does not run every position of those new scenarios**: it runs a sample (the first position, the last, one in each third of the call, and the chunks around the first and the last `RELEASE SAVEPOINT`, `COMMIT TRANSACTION`, `PREPARE TRANSACTION` and `COMMIT PREPARED` of the sessions); `t/111` runs a few positions per node by default. Every position of every scenario runs with `KINGRES_LONG_TESTS=1`, as do the cells of `t/102` and `t/103` below. A statement that spans several chunks (a large copy) is cut by the error mode into a protocol error on the node, which ends as a closed connection.

**Which cells are in the default run.** `t/102` and `t/103` together take a few minutes by default: every call
with **one** slow node at **0.9 x T** (the case of the finding: a healthy slow node never gets `KS007`), and the
copy of 60000 rows once. The cells with two and four slow nodes and the cells at 0.5 x T run only with
`KINGRES_LONG_TESTS=1` in the environment (about 22 minutes for `t/102` and 6 for `t/103` per major); a skipped
cell is announced by a `note` that names the variable. The measured times of the long run (T = 2 s) were, for
one / two / four slow nodes: `sync_catalog` 13.7 / 24.7 / 46.7 s at 0.5 x T and 23.4 / 43.1 / 84.5 s at 0.9 x T;
`rotate_secret` 12.7 / 21.1 / 44.4 and 19.9 / 41.2 / 75.5; `create_distributed_table` 9.5 / 13.2 / 27.0 and
12.8 / 25.2 / 46.9; `create_reference_table` 9.9 / 15.2 / 30.9 and 14.6 / 28.9 / 55.7;
`recover_prepared_transactions` 6.0 / 13.7 / 24.0 and 12.5 / 21.6 / 46.4; `add_node` 15.1 / 23.8 / 37.8 and
28.9 / 41.5 / 68.4; `remove_node` 8.1 / 16.8 / 30.8 and 14.5 / 28.8 / 55.8; `promote_node` 21.7 / 30.9 / 50.2
and 37.6 / 55.4 / 87.8; the copy of 60000 rows 13.8 (0.5) and 25.9 (0.9) with one node. Every one is within
`s x T` summed over the slow nodes. Only the one-node cells at 0.9 x T are in the default run.

So the time of `create_reference_table` for a node that answers every statement at 0.9 x `T` is
`(7 + batches) x 0.9 x T`, and a control plane that copies a large table sets its client timeout
from that, not from a fixed number. A statement that is slow on its own (a batch of the copy that
takes more than `T` on the node, a trigger) is a statement not answered: raise `node_timeout` for
that call.

**What a deadline leaves behind.** Every statement that a call sends before its `PREPARE
TRANSACTION` is sent inside a remote transaction that has not been prepared; the deadline closes the
session, which ends that transaction on the node, and the call fails, so the local transaction rolls
back: nothing is prepared and the catalog is unchanged anywhere (`t/086` counts the prepared
transactions and the nodes, and repeats each call). Per phase:

| phase of the call when a node does not answer | what is left | repeating the call |
|---|---|---|
| connecting, the node's first command, the first statement on the node | nothing | converges (`t/086`) |
| a statement of the catalog change on a participant, before its `PREPARE` | nothing: its remote transaction is rolled back with its session | converges (`t/086`) |
| the `PREPARE TRANSACTION` of a participant | what the origin prepared is rolled back by the origin; what could not be, recovery rolls back (`KS005`, "could not prepare the transaction on node ...") | converges (`t/070`, `t/089`) |
| the origin's commit and the second phase (`COMMIT PREPARED`) | the decision stands; the participants that were not told hold the change prepared until recovery commits it | `completed`, then `already` (`t/064`, `t/089`) |
| `promote_node`, a node other than the one to promote | the promotion is made on the nodes that answered; the node is in `"unreached"` and keeps its old catalog | `completed` finishes it when the node answers (`t/086`) |
| `promote_node`, the node to promote | nothing: `KS007` | converges |
| `recover_prepared_transactions()` and the worker | the node is skipped with a `WARNING` that names it (a `LOG` line in the worker); what it holds prepared is resolved by the next pass | converges |
| `check_cluster()` | a row `node_unreachable` for the node | n/a |
| `remove_node` of a node that does not answer, or that stops answering at any statement of the removal before its `PREPARE` (S12-04) | the node is removed from the catalog and its own catalog is not cleared (a `WARNING` names it, with `KS007` as its SQLSTATE for a node that stopped); the session to it was closed, so nothing of the call is left on it | n/a (`t/100`) |
| the end of a call (S12-01) | nothing: a call sends no statement to a node after its last piece of work, so there is nothing at the end that a silent node could fail or lose; the work of the call commits with the transaction, and a node that stops answering between the call and the commit fails the commit at its `PREPARE` (`08006`, nothing changed) or, after the decision, is a `WARNING` "recovery will commit it" | converges (`t/100`, `t/101`) |

#### Preconditions of every failover and switchover

1. **Fencing (the control plane's job, before anything else).** OLD must not be able to
   accept writes from anybody before NEW is promoted: stop it (`pg_ctl stop -m fast`, or
   `-m immediate` when it hangs), or power it off, or cut every client off at the
   network. (For a **planned switchover** the stop must be a clean one: `-m immediate` is a
   failover, not a switchover, because nothing then proves that NEW has what OLD wrote; sequence 5.)
   The extension does not fence. `promote_node` tells OLD about the new
   catalog when it can reach it, and from then on OLD routes the statements of
   distributed tables to NEW and refuses forwarded ones; that is a courtesy, not a
   fence: OLD still accepts writes of ordinary (not distributed) tables, and a client
   that is connected to it and writes rows of its own group before it learns is
   acknowledged. If OLD is not reached (it is listed in `"unreached"`) it goes on taking
   writes that nobody else has; **they are acknowledged to the client and then gone** when
   OLD is rebuilt as a standby of NEW, with no record of them anywhere. What the extension
   offers to detect it afterwards: the node is in `"unreached"` of the answer;
   `check_cluster()` shows it as `standby_not_in_recovery` ("registered as a standby and is
   not in recovery") for as long as it runs as a primary, and `catalog_status` on it shows
   the term it holds (a node that was not told holds the old one). Nothing shows which writes
   it took; before rebuilding it, compare what it holds with `pg_rewind --dry-run` or keep its
   data directory for an audit (`t/049`, `t/063` show the listing in `unreached` and the term; *not produced by a test:* the loss of the writes of an unfenced OLD, which is the review's observation).
2. **Which standby to promote.** The one that has the most WAL. Read the position of every
   standby of the group: `SELECT pg_last_wal_receive_lsn()` on each (the flushed position of what
   it has received; `pg_last_wal_replay_lsn()` is smaller or equal), and promote the standby
   with the highest value (`pg_wal_lsn_diff`). With `synchronous_standby_names = 'ANY k
   (a, b, c)'` a commit was acknowledged by any k of them, so the standby that has the
   commit is not necessarily a given one: read **at least n - k + 1** standbys of the list (all of them
   when you can) and take the highest; a standby that you cannot read must be assumed to hold
   the commits that the others lack. With one synchronous standby it is that standby, if it was
   streaming when the primary was lost. Without a synchronous standby a failover loses what the
   standby had not received (guarantee 12). `t/073` (C) reads the positions.
3. **The synchronous standby list, on NEW and on the others.** A standby is a copy of its primary and
   has the primary's `synchronous_standby_names`, which names standbys that are not connected
   to NEW (typically itself): every commit on NEW would wait for ever. Before `promote_node`
   (right after `pg_promote()` at the latest) set on NEW the list of the standbys that will
   follow it, or an empty string while there are none, and reload (`ALTER SYSTEM SET
   synchronous_standby_names = ''; SELECT pg_reload_conf();`). `promote_node` checks that
   NEW can commit and refuses, changing nothing, when it cannot (`KS009`). Names in the list are
   the `application_name` of the standbys: put `application_name=<name>` in each
   standby's `primary_conninfo` and **check the name before it goes into a list** (sequence 1, `kingres.standby_name_matches`, `t/084`, `t/088`).
   The lists of the other groups' primaries are not touched by a promotion. OLD's own list may
   keep naming NEW: OLD is not waited for (sequence 4).
4. **The settings that bound the calls.** `kingres_shard.node_timeout` (default 60 s) bounds
   every wait on another node that must not last for ever, and every statement of a control-plane
   call (Bounds above); `kingres_shard.node_connect_timeout`
   (default 5 s) the connection to a node; `kingres_shard.decision_retention` (default 300 s,
   the same on every node) how long a commit decision is kept once its participant's standbys
   have confirmed it (`t/075`). *Not produced by a test:* the default values.
5. **Where each call runs.** `promote_node` for an ordinary group, `add_node`, `remove_node`,
   `sync_catalog`, `standby_status`, `check_cluster`, `create_restore_point`: on MAP.
   `promote_node` for the map group, and `switchover_verify` of a map group switchover: on the
   promoted node itself (NEW). `switchover_mark`: on OLD. `recover_prepared_transactions`: on the
   node that originated the transactions (in practice on MAP and on NEW). A node that was
   promoted in PostgreSQL and is not yet the map primary in the catalog refuses every catalog
   change (guarantee 9, `KS010`).
6. **One controller at a time.** Calls that change the catalog (`add_node`, `remove_node`,
   `promote_node`, `sync_catalog`, `rotate_secret`, `create_distributed_table`, `create_reference_table`,
   `move_bucket`) run one at a time on the map group primary. A second caller waits for the first
   for at most `node_timeout` and then gets `KS008` having changed nothing (`t/086`); two
   controllers that call at once therefore do no damage, but the one that is refused must not read
   the refusal as "the cluster is broken": it repeats later. Run one controller, or give them a
   lease.

#### Postconditions (every sequence ends with these)

- On MAP (on NEW for the map group): `SELECT node_id, role FROM kingres.node ORDER BY 1`
  shows NEW as `primary` and OLD as `standby`; the same query on every other primary gives
  the same rows, and `SELECT term, superseded FROM kingres.catalog_status` gives the
  same term on every node that was reached (nodes in `"unreached"` keep the old term until
  they are told by `sync_catalog()` or learn it).
- `SELECT count(*) FROM pg_prepared_xacts WHERE gid LIKE 'kingres\_h%'` is 0 on every primary,
  after `SELECT kingres.recover_prepared_transactions()` on NEW and on MAP if it is not.
- `SELECT * FROM kingres.check_cluster()` on MAP shows only what the sequence says it may
  (`standby_unreachable` for OLD until it is rebuilt; `decision_unconfirmed` while a
  participant's standby is away). The promotion's own transactions leave no incident (S9-06).
- `SELECT * FROM kingres.standby_status()` on MAP: every standby `streaming`, `problem` empty,
  `sync_state` as the list says, **except OLD after a failover with an unreachable OLD**: it shows
  `problem = standby_unreachable` until it is rebuilt as a standby of NEW (sequence 6); the
  condition holds for it only after that sequence (`t/091`, `t/090`).
- **The group is healthy again** when NEW is the primary of the group on every node, the
  group's standbys stream from NEW and are named in NEW's `synchronous_standby_names`, `unreached`
  is empty or every node in it has been fenced or rebuilt, and `check_cluster()` shows nothing
  for the group.
- The previous cluster secret exists only between a rotation and the first recovery pass that
  reached every primary **and every standby of the catalog** with the new one; that pass tells every
  primary to forget it (`t/087`). A standby that was down, or that had not replayed the rotation, keeps
  the previous secret alive: it can then still be reached and promoted (`promote_node`, `t/095`), and
  a **second rotation is refused** (`KS014`) while a standby has not confirmed the last one. Until the
  pass, a name tagged with the old secret still counts as the cluster's. A node that holds a secret that
  nobody has (it missed two rotations, or the previous secret was dropped by hand) is refused by
  `promote_node` with `KS014` and the way out: rebuild it from a node that has the current secret.

#### 1. Registering a standby

Precondition: the standby streams from its primary (`pg_basebackup -R`, `primary_conninfo`
with `application_name=<name>`) and is in recovery.

1. MAP: `SELECT kingres.add_node('<host>', <port>, <group_id>, 'standby')`.

   | answer | SQLSTATE | action |
   |---|---|---|
   | the node id (an integer) | | store it |
   | `WARNING:  standby H:P could not be reached; it is registered anyway` | | the standby was registered without being checked (guarantee 2, `t/084`); repeat step 3 until it is clean |
   | error "this function can only run on the map group primary" | `KS010` | wrong node (`t/082`, `t/091`) |
   | error "is not in recovery, so it is not a standby" (`t/048`), "is not a copy of the primary of group N" (`t/048`, `t/082`) | `55000` | nothing was registered; fix the standby |
   | error "group N has no primary node" | `42704` | a standby for a group that does not exist; nothing was registered (`t/091`) |
   | error "group N already has a primary (node M)" | `42710` | a node given the role `primary` for a group that has one: it is a standby, say `'standby'` (`t/091`) |
   | error "invalid node role "R"" | `22023` | the role is `primary` or `standby` (`t/091`) |
   | error "could not connect to node H:P" followed by the libpq text | `08001` | a primary node that cannot be reached cannot be registered (a standby can, with the warning above); fix the address (`t/091`) |
   | error "kingres_shard must be loaded via shared_preload_libraries" (its context names an internal function; the first line is what to act on) | `55000` | a primary node that has no library loaded; add it to `shared_preload_libraries` and restart that node (`t/091`) |
   | a major version error | `55000` | `t/046` produces the refusal of a node of another major by `add_node` (the text is that test's) |
   | error "node ... is already registered as node N" | `42710` | it is registered; use N (`t/082`) |
   | error "node N cannot commit: its synchronous standbys do not confirm" | `KS009` | the list of the primary of some group names a standby that is not there (S9-08): fix that list first; nothing was changed (`t/084`, `t/091`; the wait is bounded, see Bounds) |
   | error "node N (H:P) did not answer within T seconds ..." | `KS007` | a node of the cluster accepts sessions and does not answer: the call is refused having changed nothing; look at that node (`t/086`) |
   | error "lock not available on node N (H:P) ..." | `55P03` | another session holds a lock on a catalog table of that node; repeat when it is gone (`t/086`) |
   | error "another call that changes the catalog is running on this node ..." | `KS008` | repeat later; one controller at a time (`t/086`) |

2. **Check the name before it goes into the list.** On the primary of the group:
   `SELECT kingres.standby_name_matches('<name>')` must answer `t`: a streaming standby of this
   primary carries that `application_name` and the group has a registered standby. The decision is
   PostgreSQL's: names are compared **without regard to case** and the element `*` matches every
   standby; the name may be written as in the list, in double quotes (`"My-Standby"`). `f` for a
   typo, for a standby that is not streaming and for a group without a standby (`t/084`, `t/088`,
   which puts each form into the list and compares the answer with the `sync_state` that the primary
   then reports). It does not say which registered node the walsender serves: `pg_stat_replication`
   does not. A name that matches nobody
   makes every commit that writes in the group wait for ever (PostgreSQL's wait has no timeout) and a
   commit that also writes in another group fail with `KS005`; it is released the moment the list is
   corrected. Then add the name to `synchronous_standby_names` (`ANY 1 (<name>, ...)`) and reload.
3. MAP: `SELECT * FROM kingres.standby_status()`: the row of the node shows `streaming`, `sync_state`
   `quorum` or `sync`, `problem` empty. `async` means that the name does not match its
   `application_name`: put the previous list back at once. (`check_cluster()` shows nothing for an
   `async` standby that no list names; that is by design.) (`t/048`, `t/082`)

#### 2. Failover of an ordinary group (OLD is dead, fenced; NEW is a standby of the group)

Preconditions: 1, 2 and 3 above. NEW has been promoted with `pg_promote()` and accepts connections.

1. NEW: set `synchronous_standby_names` to the standbys that will follow (or `''`), reload.
2. MAP: `SELECT kingres.promote_node(<NEW node id>)`, repeated at most 10 times until `already`.
   The answer is `{"status": ..., "term": T, "final": bool, "unreached": [ids], "blocked_by": [...]}`.

   | answer | SQLSTATE | what the caller does |
   |---|---|---|
   | `promoted`, `final` false | | repeat the call (it is cheap) until `already`, then go to 3 (`t/071` A) |
   | `promoted` or `completed` with `"unreached": [ids]` | | the same; the listed nodes were not told: fence them if they are primaries (OLD is one), and run `sync_catalog()` on MAP when they are back (`t/049`, `t/063`). The session also gets a `WARNING` per node, which says why: "the former primary, node N (H:P), cannot be reached and keeps its old catalog" for OLD, "node N (H:P) cannot be reached and was not given the new catalog" for another node, and, for a node that accepts sessions and does not answer, its `KS007` text (`t/091`, `t/086`); a caller that treats text on stderr as failure must not |
   | `completed`, `final` false | | an earlier call or a crash had left it half done and this call finished it (`t/064`); repeat |
   | `already`, `final` true | | done; go to 3. `unreached` lists the nodes of the group that cannot be reached now, OLD among them if it is down (`t/064`, `t/071`). The session gets `NOTICE: node N is already the primary of group G`; the same answer comes for the id of any node that is the primary of its group already, the map group's own id included (`t/091`) |
   | any status with `blocked_by` not empty | | a prepared transaction that holds a lock on the catalog's version row on that node is in the way. `"class": "cluster"`: recovery resolves it, repeat. `"class": "foreign"` (somebody's own prepared transaction): stop after two identical answers; the operator ends it (`ROLLBACK PREPARED '<gid>'` on that node if it is theirs and unwanted) and the next repeat answers `already`. An ordinary role can only hold ACCESS SHARE on the table (it may read it), which blocks nothing and is not counted (S9-04, `t/078`) |
   | error "node N (H:P) cannot commit: its synchronous standbys do not confirm" | `KS009` | NEW's list names standbys that are not connected: fix that node's list, repeat. Nothing was changed (`t/071` A) |
   | error "this node cannot commit: its synchronous standbys do not confirm" | `KS009` | the same for MAP's own list (`t/071` B) |
   | error "node N (H:P) is still in recovery" | `KS011` | `pg_promote()` has not finished; wait, repeat (`t/071` G, `t/082`, `t/091`) |
   | error "could not connect to node N (H:P), which is to become the primary of group G" | `08001` | NEW does not accept connections; repeat when it does (`t/084` B) |
   | error "node N does not accept the session of this node: the two hold cluster secrets that do not match, and either may be the one that is behind" | `KS014` | NEW missed a rotation of the cluster secret and the previous one is no longer kept, **or the caller did** (when the call runs on a map standby): **not a retry**; ask a third node first (the caller that every other node refuses is the stale one), then rebuild the stale one from a node that has the current secret, or promote another standby of the group (`t/095`) |
   | error "node N (H:P) did not answer within T seconds ..." | `KS007` | NEW accepts sessions and does not answer: do not repeat; look at it (`t/086` produces it for a participant and for `add_node`; `t/096` for NEW itself, silent after its 2nd, 4th and 6th message: the text then names the node and says "which is to become the primary of group G") |
   | error "node N does not exist" | `42704` | wrong id (`t/082`) |
   | `NULL` (no error) | | the argument was NULL; the function is strict (`t/082`) |
   | error "kingres_shard: ... did not confirm that its part of the transaction is durable" | `KS005` | a participant of the call (a primary of another group) cannot get its own standby to confirm, after `2 x node_timeout + 5` s; nothing was committed; fix that node's list or standby and repeat. `t/084` B and `t/070` produce KS005 with that text for a distributed commit; *not produced by a test:* KS005 out of `promote_node` itself (it probes first, so the refusal above comes before it) |
   | error "could not prepare the transaction on node N (H:P): the node did not answer within kingres_shard.node_timeout (N s), nor in the time it gets after the command was cancelled" | class `08` | a participant of the call does not answer even after its wait was cancelled (the node is hung, not just without its standby); nothing was committed, and recovery rolls back what it may have prepared; repeat when the node answers. *Not produced by a test* (it needs a participant that stays silent after a cancel in the middle of the `PREPARE`, which a test can only make with a stopped process at that moment); `t/086` produces the answers for a node that does not answer before the `PREPARE` |
   | error "could not prepare ... on node H:P: <text>" with any other text | the participant's | the participant's own error (for example prepared transactions disabled) or "terminating connection" when its backend was terminated (`t/070`); the text after the colon is never empty |
   | error "this function can only run on the map group primary" | `KS010` | wrong node (`t/049`, `t/082`, `t/091`) |
   | error "this node was promoted in PostgreSQL after the catalog named it the map group primary ..." | `KS010` | the call went to the promoted standby of the map group before its own `promote_node`: do the map-group sequence first (S9-02, `t/077`) |
   | error "another call that changes the catalog is running on this node ..." | `KS008` | repeat later (`t/086`) |
   | any other error, or the connection breaks, or the client times out | | the call did not finish: repeat it on the same node (sequence 9) |

3. Re-point the other standbys of the group (OTHERS), if any: on each, set `primary_conninfo` to NEW
   (with its `application_name`), reload or restart. A standby that is behind NEW follows it
   over the timeline switch; one that is ahead of NEW cannot (rebuild it with `pg_rewind` or
   `pg_basebackup`, sequence 6); that is why NEW was chosen by position.
4. NEW: when OTHERS stream, set `synchronous_standby_names = 'ANY 1 (<their names>)'`, reload.
5. MAP: postconditions. Run `SELECT kingres.recover_prepared_transactions()` on NEW (the worker does
   it every `recovery_interval` too) and look at `check_cluster()`: `standby_unreachable` for OLD until
   it is rebuilt, and `prepared_rolled_back_after_failover` or `participant_lost_committed_transaction`
   rows, each of which names a transaction to look at (guarantee 12) (`t/050`, `t/068`).
6. OLD: see sequence 6.
7. **Before the next `rotate_secret`.** After the failover the old primary OLD and the other standbys
   of its group (OTHERS that were not re-pointed to NEW) are still standbys of the catalog. The
   previous secret is kept after a rotation until a recovery pass has reached every primary **and every
   standby of the catalog**, and a second rotation is refused (`KS014`, "standby N has not confirmed the
   last rotation", naming the standby) while it is kept and a standby cannot be reached with the
   current secret: OLD is dead, and a standby that still streams from OLD replays nothing. They block
   the rotation until each is dealt with, one of two ways: re-point it to NEW (step 3), wait until it
   streams, and run `recover_prepared_transactions()` on MAP until `check_cluster()` is clean; or take it
   out of the catalog with `remove_node(<its id>)` (sequence 8: a dead node is removed with a
   `WARNING`; OLD is removed this way when it will not be rebuilt as a standby, sequence 6). Then rotate
   again (`t/101` runs the refusal and both ways out).

#### 3. Failover of the map group (OLD map primary is dead, fenced; NEW is its standby)

Preconditions: 1, 2, 3, and a majority (below). NEW has been promoted with `pg_promote()`.

1. NEW: set `synchronous_standby_names`, reload.
2. **On NEW itself**: `SELECT kingres.promote_node(<NEW node id>)`, repeated at most 10 times until `already`.
   NEW must reach the primary of a **strict majority of the groups other than the map group**, and a
   group counts only when its primary answers now and says it is one. Until this call has been
   accepted NEW changes no catalog (guarantee 9); in particular it does not promote any ordinary
   group.

   | answer | SQLSTATE | what the caller does |
   |---|---|---|
   | `promoted` / `completed` (`final` false), then `already` (`final` true); `unreached` as above | | repeat until `already`, then go to 3. The session gets the same `WARNING` for OLD as in sequence 2 (`t/091` produces it for an ordinary group; the code path is the same) |
   | error "only k of the n other shard groups can be reached from this node; the map group cannot be promoted from a minority of them" | `KS012` | nothing was changed. Bring unreachable nodes back, or promote from a node that reaches more than half of the other groups. If the other side is really gone for good, see "When the majority will never come back" below (`t/063` D, `t/071` C, `t/077`) |
   | error "node N holds catalog term T, newer than the T0 of this node" | `KS012` | another node has promoted the map group already: this node must not; rebuild it as a standby of that one (`t/084` C) |
   | error "this node cannot commit: its synchronous standbys do not confirm" | `KS009` | the list of NEW (it runs the commit); fix and repeat (`t/071` B) |
   | error "cannot acquire lock mode ExclusiveLock on database objects while recovery is in progress" (PostgreSQL's) | `55000` | the call went to the standby before `pg_promote()` (`t/084` C); the extension's own "this node is still in recovery" (`KS011`) is behind that lock and is *not produced by a test* |
   | error "this node is not a copy of the primary of group 0 and cannot become it" | `KS010` | the call went to a node that is not the standby of the map group (`t/084` C) |
   | error "node N is registered on port P, this server listens on port Q" | `KS010` | **two causes**: the call went to a node other than NEW (for example `promote_node(<the map standby's id>)` sent to MAP, or to another node) while NEW is in recovery or elsewhere, in which case the hint says "Run kingres.promote_node() on the standby that was promoted"; or the node row of NEW really names another port than the server listens on. Check where the call went before repairing the catalog; the repair is for the second cause only (`t/084` C for the second, `t/091` for the first) |
   | errors that name `KS005`, a broken connection or a timeout | | repeat on NEW |

3. NEW: `SELECT kingres.sync_catalog()` once the nodes in `"unreached"` are back; `SELECT
   kingres.recover_prepared_transactions()`. Then the failover of any ordinary group that failed with it
   (sequence 2, **from NEW**, which is MAP now).
4. OTHERS and the lists as in sequence 2 (steps 3 and 4). The clients of the cluster connect to any
   node; the connection string of MAP that the control plane stores changes to NEW.
5. If the old map primary comes back unfenced it refuses every catalog change from then on and `SELECT *
   FROM kingres.catalog_status` on it shows `superseded = true`: rebuild it as a standby (`t/063` A).

**When the majority will never come back.** A cluster of two groups, or one that lost half or more
of the other groups together with the map group, cannot promote its map group by the rule, and the
rule is the only protection against two sides each promoting one. There is a way out that is
deliberately awkward: on NEW, after every other node of the cluster is fenced (stopped or cut off
for good; the operator is the electorate), `SELECT kingres.promote_map_group_without_majority(<NEW
node id>, 'I have fenced every other node of the cluster and accept that the cluster can split')`.
It is `promote_node` without the electorate and with a catalog term 1000 above the highest one
seen, so that no node that missed it, and no claim made before it, can override it; it does not
fence anything. If a node of the lost side is alive and comes back with a promotion of its own, the
higher term wins wherever the two meet and what the other wrote into the catalog is lost
(`t/081`). Then continue with step 3.

#### 4. Failover when OLD is reachable but must be demoted

OLD answers on the network (a partition on the control plane's side, a stuck disk, a decision to
move) and the control plane promotes NEW anyway. Fence it first (precondition 1): **stop OLD**. If it
cannot be stopped, cut it off from the clients and from the other nodes at the network, and still
treat it as unreached afterwards. If it can be reached by the cluster and stays up, the call is the
same as sequence 2, with nothing to prepare on OLD: OLD is a participant of the call and is **not
waited for** (S9-05): its `PREPARE` and `COMMIT PREPARED` do not wait for its standbys, it is not
asked to confirm them, and its `synchronous_standby_names` may keep naming NEW, which no longer
streams from it. Its node row becomes `standby`, it keeps its data, it forwards the statements of
distributed tables to NEW, refuses forwarded fragments with the retryable `KS001`, and is in no group
for routing; `unreached` does not list it. The promotion's own transactions leave no
`participant_lost_committed_transaction` (S9-06). That is still not a fence (guarantee 6): stop OLD
right after the call, and rebuild it before it comes back (`t/049` B, `t/079`).

#### 5. Planned switchover without loss (OLD is healthy and is to hand over to NEW)

Precondition: NEW streams from OLD and is named in OLD's `synchronous_standby_names`, or is caught up
(step 5 proves it, as a step that can fail).

**What the two checks promise.** `kingres.switchover_replayed('<M>')` says only that NEW has replayed
up to the mark `M`; it says **nothing about what OLD wrote after the mark**, and a `t` from it is not
"nothing is lost". `kingres.switchover_verify('<X>', '<state>')` is the proof: it answers `t` only
when the control-file state of the stopped OLD is exactly `shut down` (the caller reads it from
`pg_controldata`: only a clean shutdown writes that state, and then the checkpoint that the file
names is the last record OLD wrote) **and** NEW's own WAL holds a shutdown checkpoint at `X`, replayed
as the very last record (read by the function from the WAL of NEW, not believed from the argument; after
`pg_promote()` the record must be the point where NEW left the timeline). `f` means "not replayed yet"
(ask again). An error with `KS006` means that it can never be proven: the state is not `shut down`, `X`
is no record start on NEW, or is not a shutdown checkpoint (an ordinary checkpoint, a position of another
node or of another timeline), or NEW has replayed WAL beyond it.

1. Stop the writes of the application to the group (the control plane stops sending them, or sets
   `default_transaction_read_only` for the role and terminates the sessions). Wait for distributed
   transactions in flight to end: `SELECT count(*) FROM pg_prepared_xacts WHERE gid LIKE
   'kingres\_h%'` is 0 on every primary.
2. OLD: `SELECT kingres.switchover_mark()` writes a record into the WAL, flushes it and returns its
   position **M** (keep it). Every commit that was acknowledged before it is below M. On NEW,
   `SELECT kingres.switchover_replayed('<M>')` becomes `t` once NEW has it (poll a few seconds); if it does not, NEW is
   not caught up: do not go on (`t/080`).
3. Make OLD **unable to start again** until the end of this sequence (S11-08): take its unit out of the
   care of the supervisor, mask it, or remove its start script; the proof cannot see a restart. Then
   stop it **cleanly**: `pg_ctl stop -m fast` (the shutdown sends the WAL that it has to its standbys before it
   ends). A **smart** shutdown does the same once its sessions are gone, but a session that waits for
   its standby is not helped (the node that is shutting down refuses the standby's connection); use fast
   (`t/070` 8). **`-m immediate`, a crash, `kill -9` and a power loss are not a clean stop**: whatever OLD
   wrote after the mark may be missing on NEW, and nothing shows it. If OLD hangs and must be stopped that
   way, the promotion is a **failover** (sequence 2, guarantee 12), and the rows written after the mark
   are the loss. *Without the stop* (OLD stays up, writes stopped): go on to step 6 and treat OLD as in
   sequence 4 afterwards.
4. Read, from `LC_ALL=C pg_controldata <OLD's data directory>` (the words are translated otherwise, and the
   proof then answers `KS006` for every switchover), the lines **"Database cluster state"**,
   **"Latest checkpoint location"** (**X**), **"Latest checkpoint's REDO location"**, **"Time of latest
   checkpoint"**, **"Database system identifier"** and **"Latest checkpoint's TimeLineID"**, and check
   that `postmaster.pid` is absent from OLD's data directory. The state must be exactly `shut down`. For every other
   state (`in production`, `in crash recovery`, `shutting down`, anything) do not call the proof as a
   formality: it refuses with `KS006`, and the situation is a failover.
5. **The proof, which can fail.** On NEW, **before `pg_promote()`** (the same standby that will be
   promoted; after the promotion NEW's end-of-recovery checkpoint can remove the segment that holds the
   record, and the proof then turns into `KS006` although the switchover was provable: on 19 about a second
   after the promotion in the review's run; keep `wal_keep_size` at one segment or more, or ask before):
   `SELECT kingres.switchover_verify('<X>', '<state>')`, with the state text as `pg_controldata` printed it.
   **What the caller must guarantee** (the function cannot see it): between the read of the control file
   in step 4 and `pg_promote()` OLD is not started, not even once (a clean restart and stop of OLD while NEW
   was disconnected writes a new shutdown checkpoint, and the old position still proves `t` on a NEW that
   has not followed it: the review lost 10 acknowledged rows that way). So read the control file **again
   immediately before `pg_promote()`** and promote only when every line of step 4 is unchanged (the
   position, the state, the checkpoint's REDO location and time, the system identifier, the timeline), and
   `postmaster.pid` is still absent. The function refuses a position that NEW has replayed beyond; what stays
   the caller's duty is the window in which NEW is disconnected and OLD is started (not tested beyond the
   review's reproduction).

   | answer | SQLSTATE | what the caller does | test |
   |---|---|---|---|
   | `t` | | proven: NEW has every WAL record OLD wrote; go to 6 | `t/080`, `t/085` |
   | `f` | | NEW has not replayed it **yet**; poll for a few seconds while it may still be receiving. If it stays `f`, **do not promote**: NEW is missing what OLD wrote (it was disconnected, or it is not the standby that was caught up). Start OLD again, let NEW catch up, and repeat from step 1; if OLD cannot be started the missing rows are lost, and promoting is a failover with loss | `t/080`, `t/085` |
   | error "the switchover cannot be proven loss-free: the old primary's database cluster state is ..." | `KS006` | the stop was not clean: failover (sequence 2) | `t/085` |
   | error "... no WAL record starts at this position on this node ...", "... is not a shutdown checkpoint", "... replayed WAL beyond ...", "... not on the history of this node's timeline" | `KS006` | the position is not OLD's shutdown checkpoint on NEW's WAL (another node, another timeline, an ordinary checkpoint, or NEW is not the standby of OLD): treat as a failover | `t/085` |
   | error "... both required" | `KS006` | the state or the position was not given (NULL) | `t/085` |

   What each way of stopping OLD answers (`t/085`; rows are written up to the stop and counted on NEW after
   `pg_promote()`; the answer is `t` only when no acknowledged row is missing):

   | way OLD stops | `Database cluster state` | the proof | acknowledged rows missing on NEW |
   |---|---|---|---|
   | `-m fast`, NEW streaming | `shut down` | `t` | none |
   | `-m smart` | `shut down` | `t` | none |
   | `-m immediate`, NEW caught up | `in production` | `KS006` | none, but nothing proves it |
   | `-m immediate`, NEW behind (the review's reproduction) | `in production` | `KS006` | the rows after what NEW had |
   | crash or `kill -9` of the postmaster | `in production` | `KS006` | possibly |
   | `-m immediate` during a checkpoint (a power loss) | `in production` | `KS006` | possibly |
   | `-m fast`, NEW disconnected | `shut down` | `f` for ever | the rows after the disconnection |
   | `-m fast`, `synchronous_standby_names` emptied and NEW disconnected | `shut down` | `f` for ever | the rows after the list was emptied |
   | `-m fast`, two standbys, one disconnected | `shut down` | `f` on the lagging one, `t` on the other | only on the lagging one |
   | `-m fast`, a cascading standby | `shut down` | `t` when it has replayed the record, `f` when it fell behind | only if it fell behind |
   | any state, with `X` of another node, an ordinary checkpoint of OLD, `0/1`, or a position of NEW's own timeline | | `KS006` (or `f` for a position beyond NEW's WAL) | |

   The check passes on the map group's NEW as well, run on NEW. It says nothing about a node other than
   the one it runs on: run it on the standby that you promote.
6. NEW: `pg_promote()`, then set the list (step 1 of sequence 2).
7. MAP: `SELECT kingres.promote_node(<NEW node id>)` as in sequence 2 (**for the map group, on NEW itself**, as
   in sequence 3): `promoted`, `unreached` is `[<OLD>]` for the stopped OLD (`t/073` A), then `already`.
8. OLD becomes a standby of NEW (sequence 6). Nothing is lost: NEW had all the WAL of OLD (`t/073` A, `t/080`, `t/085`).

#### 6. Re-adding an old primary as a standby

Precondition: OLD is stopped, and NEW is the primary of the group in the catalog.

1. **After a clean stop** (planned switchover): create `standby.signal`, set `primary_conninfo` to NEW
   with `application_name=<name>` and start it; it follows the timeline switch (`t/073` A).
2. **After a crash, or a failover with an unreachable OLD**, OLD may have WAL that NEW does not have, so
   it cannot simply follow NEW. Two ways, in this order:
   1. `pg_rewind --target-pgdata=<OLD's directory> --source-server='host=<NEW> port=<port> dbname=postgres'`.
      It needs: data checksums (the default of `initdb` since PostgreSQL 18) or `wal_log_hints = on` on OLD;
      OLD's own crash recovery first (pg_rewind runs it in single-user mode when OLD was not shut down
      cleanly, or start OLD alone and stop it cleanly yourself); and **the WAL of OLD from the last checkpoint
      before the divergence still present** in its `pg_wal` (`wal_keep_size` large enough, or a
      `restore_command`). When it is not, pg_rewind stops with "could not find previous WAL record at ..."
      (seen after a crash with the small WAL settings of a test cluster); that is the case for the fallback.
      pg_rewind also **copies the configuration files of NEW over those of OLD** (`postgresql.conf`,
      `postgresql.auto.conf`, `pg_hba.conf`: they live in the data directory): keep OLD's own and put them
      back before starting it (they hold its port and socket directory; started with NEW's it collides with NEW).
      Then `standby.signal` and `primary_conninfo` as in step 1 (`t/090` A).
   2. **The fallback: a fresh base backup** of NEW (`pg_basebackup -R` into an empty directory), started as a
      standby (`t/090` B). It is always possible; it costs the copy.
3. **The catalog.** The node row of OLD is already `standby` (the promotion made it so); when it comes back on
   the **same host and port** nothing is called. A standby that comes back on **another address** (a rebuilt
   machine, another port) is **a new node**: take OLD's name out of NEW's `synchronous_standby_names` if it is
   there, `SELECT kingres.remove_node(<OLD's id>)` on MAP, then register the new address (sequence 1). Until
   then `check_cluster()` shows the old row as `standby_unreachable` (`t/090` B).
4. MAP: `standby_status()` shows it `streaming`; `catalog_status` on OLD shows the term of NEW and
   `superseded = false` (it exists on a hot standby too, `t/073` A, `t/063`, `t/090`).
5. NEW: check the name (sequence 1, step 2) and add it to `synchronous_standby_names` if it is to be synchronous.

#### 7. Restoring the preferred primary (failing back)

The preferred node P was OLD, is now a standby of the group (sequence 6) and streams. It is a
planned switchover in the other direction (sequence 5, P as NEW), **with the same proof**: stop the
current primary with `-m fast`, read its `Database cluster state` and `Latest checkpoint location`, and
call `switchover_verify` on P; every defect of an unclean stop applies here as in sequence 5 (`t/085` runs
the way there and the way back, P being a standby of the new primary on timeline 2 and promoted on
timeline 3). The term goes up by one (`"term": 2` after one earlier promotion); `unreached` lists the
stopped NEW (`t/073` A). Then re-add NEW as a standby (sequence 6). For the map group the call is on P itself.

#### 8. Removing a dead standby

1. On the primary of its group: take its name out of `synchronous_standby_names` (or replace the list by
   the standbys that exist, or `''`), reload. **Do this first.** While the name is in the list, every commit
   that writes in the group waits for a standby that is gone: for the origin of a distributed transaction
   that touches the group the wait ends after `2 x node_timeout + 5` seconds with `KS005`, "did not confirm
   that its part of the transaction is durable" (`t/073` B); for a transaction that writes only in that group it
   is PostgreSQL's wait with no end.
2. MAP: `SELECT kingres.remove_node(<standby node id>)`: it returns nothing.

   | answer | SQLSTATE | action |
   |---|---|---|
   | nothing | | done |
   | error "node N cannot commit: its synchronous standbys do not confirm" | `KS009` | step 1 was not done. The wrong order is refused after at most `min(node_timeout, 5) - 1` seconds **per node** that cannot commit (not "at once"), and by every call that commits on every primary: `remove_node`, `add_node`, `rotate_secret` and `sync_catalog` (S9-08, `t/081`, `t/091`): do step 1, repeat |
   | error "cannot remove node N: shard group G still has the standby node M" | `55006` | a primary that still has standbys cannot be removed before them (`t/048`) |
   | error "node N does not exist" | `42704` | wrong id or already removed (`t/073` B) |
   | `WARNING: could not reach node N (H:P); its catalog was not cleared`, with the SQLSTATE of the reason (`KS007` for a node that stopped answering, `55P03`, or a connection error) and its text in the detail | | the node was removed from the catalog; it does not answer, or stopped answering after it had accepted the session, at whatever statement of the removal (S12-04), so its own copy of the catalog was not cleared: **a stuck node is removed by calling `remove_node` while it is stuck**, with this warning and nothing else; there is no `force` form. The warning is the whole answer: check afterwards (`SELECT count(*) FROM kingres.node` on the removed node) if the copy matters, or reset it when it is back (`SELECT kingres.internal_reset_node()` as a superuser on that node). **The removed node still holds its catalog and the cluster secret, so the nodes of the cluster still accept its sessions until the secret is rotated** (S13-06): run `kingres.rotate_secret()` after the removal (it no longer includes the node, so the old secret is useless to it), and treat the node as trusted until then; the warning says so in its hint (`t/086`, `t/100`: every position of the silence, `t/107`: the text) |
   | error "could not prepare the transaction on node N (H:P) ..." (`08006`) | | the node answered every statement of the removal and then stopped before its `PREPARE TRANSACTION` was answered: nothing was changed anywhere, the node is still in the catalog (`t/100`). Repeat: a node that is stuck now does not answer the first statement, and the repeat ends with the warning above |

   A `remove_node` that was **killed after its commit** can leave the removed node's part of the change
   prepared on it (with its locks on the node's catalog tables). The removal writes the node's address and
   old id into `kingres.pending_node` first, and the recovery of MAP (the worker, or
   `recover_prepared_transactions()`) visits it and commits the part, so the repeat of
   `remove_node` and one recovery pass are enough (`t/089`: the origin killed at each of the six points,
   and the removed node killed while the origin waits). If MAP is lost before it, look on the removed node
   for `SELECT gid FROM pg_prepared_xacts WHERE gid LIKE 'kingres\_h%'` and decide each with the origin's
   `kingres.dtx_log` (or `COMMIT PREPARED` it, the removal having committed).

3. MAP: `standby_status()` has no row for it and `check_cluster()` no `standby_*` row (`t/073` B).

#### 9. A call that did not finish

A call that returns an error not listed above, whose connection broke, or that the
control plane gave up on (its own timeout; a cancel is as good as a crash here) leaves the cluster in one of two
states, and the same call settles both: before the decision of its two-phase transaction nothing
was changed and nothing is left prepared (the repeated call answers `promoted`); after it the change
is committed on MAP and may be left prepared on other nodes (the repeated call answers `completed`, and the
worker's recovery pass does the same by itself within `recovery_interval`). Call it again, on the same
node, until it answers `already` (`"final": true`), at most 10 times: `promoted` and `completed` are made before the
call's commit, and a commit whose wait for the standbys of the node that runs it was cancelled
leaves the change prepared on the other nodes, with a `WARNING` on the session that says so (the
extension refuses beforehand when it can see that the standbys do not confirm; the rest is a race, and
`final` is what makes it harmless). A client that cancels the call is a client that gave up: the same repeat
settles it (`t/064`, `t/082`). The same holds for `rotate_secret` and `add_node` killed at every point of
the commit protocol (`t/089`: the origin killed at each of the six points, recovery, the repeated call; every node
ends with the same secret or the same catalog, nothing prepared, `check_cluster()` empty) and for a bucket
move (`t/028`). After ten calls without `already`, read `blocked_by`, `unreached` and the log
of MAP, and stop.

**A call that does not return.** The calls of this chapter do not wait for ever at a statement: every statement
that a call sends waits at most `T` (Bounds above), and so does every statement that the extension sends when
the call's subtransaction or transaction ends (S13-01). **But a call that is working is bounded per statement
only**: its whole time is the number of statements times `T` for each slow node, with the catalog lock held on
MAP and open remote transactions on the participants, unless `kingres_shard.control_call_timeout` is set
(`KS015`). A call that is still running after the formula and 10 s has a **local** reason (MAP itself is
stuck) or is working through a slow node: look at `pg_stat_activity` on MAP, and cancel the backend
(`pg_cancel_backend`) before starting another call. A call that ends with `KS007`, or
with `55P03`, names the node: that is the answer, not a reason to repeat. Probe the node with a
read-only call of your own, with your own deadline (`SELECT 1` over a new session, and a query of
`pg_stat_activity` and `pg_locks` on it); the node that does not answer is the problem, to be fenced
or restarted. Repeating `promote_node` or `sync_catalog` against a node that does not answer repeats
the same wait. If your own client timeout expires **twice in a row** for the same call, stop the loop
and probe every primary the same way. A call that was **refused with `KS008`** did not run: another
call that changes the catalog was running; wait and repeat (one controller at a time, precondition 6).

#### Detecting damage afterwards

`check_cluster()` on MAP is the one report: `standby_unreachable`, `standby_not_streaming`,
`standby_not_in_recovery` (an old primary that runs as a primary and is registered as a standby), `prepared_transaction`
(a transaction prepared for longer than the argument says), `foreign_prepared_transaction`,
`prepared_rolled_back_after_failover`, `participant_lost_committed_transaction`, and the checks of
moves and copies. A row of `kingres.dtx_incident` is kept 30 days.

`decision_unconfirmed` that **lasts** means that a participant's standby has not confirmed a commit,
so the commit decisions of that participant are kept without limit (a few dozen bytes per distributed
transaction, in `kingres.dtx_log`) instead of for `decision_retention`: alert on it, and bring the
standby back or take it out of the list (sequence 8). A node that does not answer `check_cluster()`
shows as `node_unreachable`, with the text of what it was asked (`KS007`); the other nodes are still
checked.
