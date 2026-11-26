<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
# Sharding inside PostgreSQL: implementation-level notes for KingresDB (as of Nov 2026)

Method note: roughly 15 tool calls. Several fetches failed (ECONNRESET / socket hang up): Citus 2017 2PC blog, Citus 14.0 release page, PG partitioning docs, YugabyteDB transactions docs. Items marked "[background knowledge, unverified in this session]" come from the author's prior knowledge of the Citus/PG codebases. They have no URL, so the report writer should treat them as lower confidence and verify them against the cited source tree before relying on them.

## 1. Metadata and shard mapping (Citus catalogs, Couchbase vBuckets, hash-bucket design on PG)

### Takeaway
Citus keeps the shard map in five coordinator catalog tables (partition, shard, placement, node, colocation) and copies it to workers through metadata sync. Couchbase uses a fixed 1024-slot CRC32 vBucket map that is pushed to clients. For KingresDB, the simplest design is a fixed, high number of hash buckets, each stored as a normal table or partition, plus a bucket-to-node map. Citus effectively does this with hash-token ranges.

### Cited Findings
- `pg_dist_partition` lists distributed tables. Its columns are `logicalrelid`, `partmethod` (hash/reference/etc.), `partkey`, `colocationid` and `repmodel`. — [Citus metadata tables](https://docs.citusdata.com/en/stable/develop/api_metadata.html)
- `pg_dist_shard` has one row per shard (`logicalrelid`, `shardid`, `shardstorage`, `shardminvalue`/`shardmaxvalue`). The min/max are the hash-token range used for pruning. — [Citus metadata tables](https://docs.citusdata.com/en/stable/develop/api_metadata.html)
- `pg_dist_placement` has `placementid`, `shardid`, `shardstate` and `groupid`. It joins to `pg_dist_node` on `groupid`, so placements point at node groups, not hosts. This indirection is what lets a failover change a host without touching placements. — [Citus metadata tables](https://docs.citusdata.com/en/stable/develop/api_metadata.html)
- `pg_dist_node` has `nodeid`, `groupid`, `nodename`, `nodeport`, `isactive`, `noderole` (primary/secondary) and `shouldhaveshards`. Setting `shouldhaveshards=false` drains the node on rebalance. — [Citus metadata tables](https://docs.citusdata.com/en/stable/develop/api_metadata.html)
- `pg_dist_colocation` has `colocationid`, `shardcount`, `distributioncolumntype` and `distributioncolumncollation`. Tables in one colocation group have the same shard count and distribution-column type, and their shards with equal hash ranges live on the same node. — [Citus metadata tables](https://docs.citusdata.com/en/stable/develop/api_metadata.html)
- Metadata is synced to new nodes when they are added. By default this runs in one transaction, which can run out of memory on large clusters. `citus.metadata_sync_mode` offers a non-transactional alternative. — [Citus cluster management](https://docs.citusdata.com/en/stable/admin_guide/cluster_management.html)
- The maintenance daemon (a background worker) syncs node metadata after `citus_update_node`. — [Citus distributed README](https://raw.githubusercontent.com/citusdata/citus/main/src/backend/distributed/README.md)
- Couchbase stores each bucket as 1024 vBuckets (64 on macOS). Each replica gets its own set of 1024, so two replicas give 3072 in total. — [Couchbase 7.2 vBuckets](https://docs.couchbase.com/server/7.2/learn/buckets-memory-and-storage/vbuckets.html), [6.0](https://docs.couchbase.com/server/6.0/learn/buckets-memory-and-storage/vbuckets.html)
- Couchbase applies CRC32 to the key to get the vBucket number. The client looks the vBucket up in the vBucket map and talks to that node directly. The Cluster Manager updates the map and sends it to all participants. — [Couchbase 7.2 vBuckets](https://docs.couchbase.com/server/7.2/learn/buckets-memory-and-storage/vbuckets.html)
- A Couchbase JIRA ticket notes that CRC32-to-range modulo is clean for powers of two such as 1024 but not for other numbers. A forum post reports data is within single-digit percent of even across vBuckets. — [MB-19520](https://issues.couchbase.com/browse/MB-19520), [forum](https://www.couchbase.com/forums/t/uniform-distribution-of-data/30164)

### Inferences
- A fixed vBucket count is a "virtual bucket" scheme. Rebalancing only moves whole buckets, never rehashes keys. Citus's default of 32 hash shards (default value [background knowledge, unverified]) is the same idea with a smaller N. KingresDB should pick N (for example 1024 or more) up front, because changing N means a full resplit.
- Implementation on PG:
  - Table: `kg_bucket_map(bucket int primary key, group_id int, state)`.
  - Bucket function: `bucket = hash(key) % N`, using PG's own `hashint4extended`/`hashtextextended` with a fixed seed. [background knowledge: PG hash partitioning uses the `*extended` hash opclass support functions with seed 8816678312871386365 and combines columns via `hash_combine64`; verify in `src/backend/partitioning/partbounds.c` and `src/include/partitioning/partdefs.h`.]
  - The same function can be reused on workers via a local `PARTITION BY HASH (MODULUS N, REMAINDER r)` table. Matching PG's hash makes `COPY`/pruning on a worker consistent with the coordinator's routing.
  - Alternative that Citus uses: shards are plain tables named `tbl_<shardid>` with the 32-bit hash space split into N contiguous ranges [background knowledge].
- Consistent hashing (ring) gives smooth node add/remove but needs vnode bookkeeping and gives uneven ranges. Range sharding supports range scans and splits but suffers hotspots. Fixed buckets are the simplest for rebalancing and replica placement.

### Gaps
- Fetch of the PG partitioning docs failed, so the hash-partition function details are uncited.
- I did not read the Citus `colocation_utils.c` or `metadata_sync.c` source, so how metadata sync is triggered (the `hasmetadata` and `metadatasynced` flags on `pg_dist_node`) is uncited.

## 2. Query path (hooks, planner tiers, executor, pruning)

### Takeaway
Citus is a pure extension. It uses `planner_hook` to produce a CustomScan plan, runs the adaptive executor inside that CustomScan, uses the utility hook for DDL, and runs a maintenance background worker. Planning is tiered, from a fast-path router that skips the PG planner up to a rule-based multi-shard optimizer.

### Cited Findings
- Hooks and components: `planner_hook`; an `ExecutorRun` hook to run subplans; CustomScan for distributed plans; xact callbacks at pre-commit, post-commit and abort; a utility hook for DDL and COPY on Citus tables; a maintenance-daemon background worker (deadlock detection, 2PC recovery, cleanup); UDFs for metadata changes. — [Citus distributed README](https://raw.githubusercontent.com/citusdata/citus/main/src/backend/distributed/README.md)
- Planner tiers, in order:
  1. Fast path router: `FastPathPlanner()`/`FastPathRouterQuery()`. Single shard on a distribution-key equality, and it skips `standard_planner()`. Deferred pruning is supported for prepared statements.
  2. Router: `PlanRouterQuery()`, `TargetShardIntervalsForRestrictInfo()`. Colocated queries pruned to one shard, using restrictions from `standard_planner()`.
  3. Recursive planning: `RecursivelyPlanSubqueryWalker()`, `ExecuteSubPlans()`. Non-pushdownable subqueries are materialized as intermediate results.
  4. Pushdown: `RestrictionEquivalenceForPartitionKeys()`, `ShouldUseSubqueryPushDown()`. Multi-shard queries joined on distribution keys.
  5. Logical planner/optimizer: `MultiLogicalPlanCreate()`, `MultiLogicalPlanOptimize()`, `MultiNodeTree()`. Rules in `multi_logical_optimizer.c`.
  6. Combine planner: plans the coordinator-side query over worker results, via a CustomPath injected through `set_rel_pathlist_hook`.
  — [Citus distributed README](https://raw.githubusercontent.com/citusdata/citus/main/src/backend/distributed/README.md); the paths `src/backend/distributed/planner/README.md` and `multi_logical_optimizer.c` are named there
- Executor: `BeginCustomScan` does function evaluation, deferred pruning and placement lookup. `ExecCustomScan` runs the adaptive executor over remote, local and repartition tasks. INSERT..SELECT and MERGE have their own CustomScans (`CreateDistributedInsertSelectPlan`, `CreateMergePlan`). Citus steers PG away from generic plans for multi-shard queries (`DissuadePlannerFromUsingPlan()`). — [Citus distributed README](https://raw.githubusercontent.com/citusdata/citus/main/src/backend/distributed/README.md)
- Single-shard routing reads the distribution-column value, finds the shard in metadata, and rewrites the SQL to target the shard table. Workers plan and run fragments with the normal PG planner and executor. The coordinator merges the results. — [Citus query processing](https://docs.citusdata.com/en/stable/develop/reference_processing.html)
- Subqueries or CTEs that cannot be pushed down (for example, one with LIMIT) run separately. The results are pushed to workers, where they are read with `read_intermediate_result` in a "Distributed Subplan". — [Citus query processing](https://docs.citusdata.com/en/stable/develop/reference_processing.html)
- The adaptive executor opens connections per shard as needed. — [Citus query processing](https://docs.citusdata.com/en/stable/develop/reference_processing.html)
- `citus_stat_activity` adds `global_pid` and `is_worker_query` to `pg_stat_activity`, and `citus_lock_waits` shows cluster-wide blocking. — [Citus metadata tables](https://docs.citusdata.com/en/stable/develop/api_metadata.html)
- Current versions: Citus 14.0 (Feb 2026) adds PG18 support. The 14.x line covers PG 16/17/18 and the 13.x line covers PG 14-17. Citus moved to a PostgreSQL-style Major.Minor cadence in 14.1. — [Citus 14.0](https://www.citusdata.com/updates/v14-0/), [Citus 14.0 blog](https://www.citusdata.com/blog/2026/02/17/distribute-postgresql-18-with-citus-14/), [14.1](https://www.citusdata.com/updates/v14-1/), [14.2](https://www.citusdata.com/updates/v14-2/). Search snippets report conflicting dates for some releases.

### Inferences
- MVP: the router tier only (single-key lookups and writes), implemented as `planner_hook` + CustomScan. Add multi-shard pushdown for colocated joins and aggregates next. Skip the rule-based multi-shard optimizer until needed.
- [Background knowledge, unverified] Adaptive executor details (slow-start connection ramp, `citus.max_adaptive_executor_pool_size`, `citus.max_shared_pool_size`, one connection per worker reused within a transaction so read-your-writes holds) were not confirmed this session.
- FDW alternative: `postgres_fdw` plus partitioning gives pushdown of scans, joins and aggregates with far less code, but it has no shard-aware pruning beyond partition pruning, no parallel async across many shards by default, and it is weak on distributed DML and 2PC. It suits a prototype only.

### Gaps
- The adaptive executor's connection-pool algorithm and the repartition-join mechanism are uncited (page and README were truncated).
- The README fetch covered only characters 0-100000 of 237813. The 2PC, deadlock and rebalance internals sections were not read.

## 3. Distributed transactions, deadlocks, global snapshots

### Takeaway
Citus gives atomic commit via 2PC plus background recovery, but not global snapshot isolation. Fixing visibility requires a global timestamp or CSN scheme. The pgsql-hackers "Global snapshots" patches (Clock-SI) never landed as far as the sources show. The simplest correct design for a new project is per-shard transactions plus 2PC, with shard-key transactions routed to one shard and no cross-shard snapshot promise at first.

### Cited Findings
- Citus says cross-node transactions are atomic, durable and consistent but lack full distributed snapshot isolation. Writes to reference tables and replicated placements use 2PC with aggressive locking. The maintenance daemon does 2PC recovery and distributed deadlock detection. — [Citus distributed README](https://raw.githubusercontent.com/citusdata/citus/main/src/backend/distributed/README.md)
- PG `PREPARE TRANSACTION` is the XA-style first phase. After it, only `COMMIT PREPARED` or `ROLLBACK PREPARED` are valid. Short-lived prepared transactions live in shared memory and WAL, and ones that span a checkpoint go to `pg_twophase`. They are visible in `pg_prepared_xacts`. — [PG two-phase docs](https://www.postgresql.org/docs/current/two-phase.html)
- `max_prepared_transactions` defaults to 0, so it must be raised and the server restarted. Prepared transactions hold locks, and an orphan blocks VACUUM and risks wraparound. GIDs are limited to 200 bytes. — [PG two-phase docs](https://www.postgresql.org/docs/current/two-phase.html) (the fetch tool marked some of these as "additional details not on this page"; they match PG documentation elsewhere but verify)
- Patroni sets `max_prepared_transactions` to `2*max_connections` if unset when running Citus. — [Patroni Citus](https://patroni.readthedocs.io/en/latest/citus.html)
- The pgsql-hackers "Global snapshots" thread was started on 2018-05-01 by Stas Kelvich. It proposed Clock-SI with CSN-based visibility, used via SQL functions or `postgres_fdw`. Reviewers Masahiko Sawada and Amit Kapila joined in 2020. Fujii Masao argued that Clock-SI alone is not enough. A separate proposal introduced `postgres_fdw.use_twophase`. — [Search results for the thread](https://www.postgresql.org/message-id/87pnx0fgiw.fsf%40ars-thinkpad), [thread index](https://postgrespro.ru/list/thread-id/2385436), [Sawada message](https://www.postgresql.org/message-id/CA%2Bfd4k6YTa25VxpsqvpvqZmsCErT5T8%3DMYNNDXt7Wn0DsJvSrg%40mail.gmail.com). I found no evidence it was committed.

### Inferences
- [Background knowledge, unverified] Citus names prepared transactions `citus_<groupid>_<pid>_<xid>_<connectionnumber>` and records them in `pg_dist_transaction` in the same local transaction as the coordinator's commit decision. Recovery then reconciles `pg_prepared_xacts` on each worker with those records: if a record exists, commit the prepared transaction; otherwise roll it back. Verify in `src/backend/distributed/transaction/transaction_recovery.c`.
- [Background knowledge, unverified] Distributed deadlock detection builds a wait-for graph from each node's lock waits (`citus_internal`/`dump_local_wait_edges`, in `distributed_deadlock_detection.c` and `lock_graph.c`). It uses a `global_pid`/transaction-number id to unify backends across nodes, finds cycles, and cancels the youngest backend.
- Design options, in order of complexity: (a) 2PC only, with Read Committed per shard and no global snapshot (the Citus model). (b) A GTM/timestamp oracle (Postgres-XL style, with a SPOF and a latency cost per transaction). (c) CSN/Clock-SI or HLC-based snapshots, which need core patches to snapshot and visibility code and have no mainline PG support. The simplest correct start is (a), with documented anomalies (cross-shard reads can see a partially committed distributed transaction). Single-shard transactions are fully correct.

### Gaps
- The Citus 2PC blog (2017) fetch failed, so the specific naming and recovery algorithm is unconfirmed.
- The YugabyteDB HLC/read-restart and Clock-SI/HLC paper details were not retrieved. I have no cited facts on Postgres-XL's GTM.

## 4. Rebalancing and online shard moves

### Takeaway
Citus moves shards with logical replication (copy, catch up, short write lock, metadata flip) and runs the moves as background tasks. Couchbase's analogous mechanism is a DCP-based vBucket takeover.

### Cited Findings
- `citus_rebalance_start()` redistributes shards, for example after adding a node. Non-blocking reads and writes during rebalance have been supported since community Citus 11.0. — [Citus cluster management](https://docs.citusdata.com/en/stable/admin_guide/cluster_management.html)
- Moves run sequentially by default. `citus.max_background_task_executors_per_node` (default 1) allows parallel moves, but shards in one colocation group still move sequentially. — [Citus cluster management](https://docs.citusdata.com/en/stable/admin_guide/cluster_management.html)
- `citus_move_shard_placement()` also moves colocated shards. Data is copied with PG logical replication (source = publisher, target = subscriber). The write lock is brief and covers only the metadata update during promotion of the subscriber. — [Citus cluster management](https://docs.citusdata.com/en/stable/admin_guide/cluster_management.html)
- Tables need a replica identity (a primary key or a unique index that includes the distribution column). The docs advise against `REPLICA IDENTITY FULL`. — [Citus cluster management](https://docs.citusdata.com/en/stable/admin_guide/cluster_management.html)
- `isolate_tenant_to_new_shard()` splits a hash range to give one tenant its own shard. The `CASCADE` option applies it to colocated tables. — [Citus cluster management](https://docs.citusdata.com/en/stable/admin_guide/cluster_management.html)
- Couchbase: the Cluster Manager updates and redistributes the vBucket map. — [Couchbase 7.2 vBuckets](https://docs.couchbase.com/server/7.2/learn/buckets-memory-and-storage/vbuckets.html)

### Inferences
- Implementation recipe [background knowledge on the generic mechanism]:
  1. Create the target shard and schema.
  2. Create a publication on the source and a subscription on the target (`copy_data=true`, with a replication slot created first so the snapshot is consistent).
  3. Wait for the lag to reach about zero.
  4. Take a write lock on the source shard.
  5. Drain the last WAL.
  6. Update the placement metadata and sync it to nodes.
  7. Drop the old shard, deferred through a cleanup record, since failures can leave orphaned shards.
- Pitfall: logical replication does not replicate DDL, sequences or TRUNCATE the same way. Hence the replica-identity requirement.
- A KingresDB bucket map (section 1) makes "move bucket" the unit of work: one task row per move plus a background worker with retries.

### Gaps
- `shard_transfer_mode` options (`auto`/`force_logical`/`block_writes`) were not covered by the fetched page.
- Couchbase DCP and takeover details were not retrieved.
- Citus `citus_split_shard_by_split_points` internals were not read.

## 5. Replication and HA per shard

### Takeaway
Treat each shard group as an independent Patroni cluster. Placements point to groups, so a promotion only needs `pg_dist_node` updated. Patroni already automates this for Citus.

### Cited Findings
- In Patroni's Citus mode, each Patroni cluster sets `citus.group` (coordinator = 0, workers 1, 2, 3...) with the same scope and database. Each group gets its own DCS subtree. — [Patroni Citus](https://patroni.readthedocs.io/en/latest/citus.html)
- Patroni auto-configures `synchronous_mode` to quorum if unset, adds `citus` to `shared_preload_libraries`, sets `max_prepared_transactions`, and creates the Citus extension. Superuser credentials must be identical across nodes. — [Patroni Citus](https://patroni.readthedocs.io/en/latest/citus.html)
- The coordinator primary discovers worker primaries and registers them with `citus_add_node()`. Patroni keeps `pg_dist_node` current across failovers and switchovers. Since Patroni 4.0.0, secondaries without `noloadbalance` are also registered, and apps read from them with `citus.use_secondary_nodes`. — [Patroni Citus](https://patroni.readthedocs.io/en/latest/citus.html)
- On a worker switchover, the old primary asks the coordinator to update the node. This pauses application traffic to that group on the coordinator until the new primary is ready, and then the coordinator commits and resumes. — [Patroni Citus](https://patroni.readthedocs.io/en/latest/citus.html)
- Citus docs: DDL runs only through the first coordinator. A second coordinator can load-balance client connections, with `shouldhaveshards=false`. — [Citus cluster management](https://docs.citusdata.com/en/stable/admin_guide/cluster_management.html)
- Metadata is synced to workers, and the Citus docs describe a metadata-synced worker as able to take queries. — see the metadata sync note in section 1. The cluster-management page did not describe "query from any node" explicitly, so this is partially unconfirmed.

### Inferences
- The coordinator is a SPOF unless it is HA via Patroni, and it is a throughput bottleneck for DDL. Mitigation: sync metadata to all nodes (Citus `hasmetadata`) so any node can plan queries, while DDL and metadata changes still go through one node (or a Raft-replicated catalog).
- [Background knowledge] Quorum commit via `synchronous_standby_names = 'ANY 1 (...)'` avoids stalls and prevents loss of acknowledged commits on failover. Fencing is done by Patroni's DCS leader key plus the watchdog or `pg_rewind` for the old primary.
- In-doubt prepared transactions survive a failover only if the promoted replica had streamed the PREPARE record. With sync replication it did. 2PC recovery then handles them.
- Raft inside PG (pgraft-style extensions) is an option for catalog replication but I found no verified source on maturity.

### Gaps
- No sources on pg_auto_failover, pgraft maturity, or YugabyteDB's per-tablet Raft.

## 6. DDL, sequences, constraints, reference tables, backups

### Takeaway
Citus propagates DDL through the utility hook and has reference tables (replicated on every node) for joins. Uniqueness and foreign keys must include the distribution column. I have little cited evidence beyond this in this session.

### Cited Findings
- The utility hook handles DDL and COPY for Citus tables, and UDFs manipulate metadata. — [Citus distributed README](https://raw.githubusercontent.com/citusdata/citus/main/src/backend/distributed/README.md)
- Writes to reference tables use 2PC. — [Citus distributed README](https://raw.githubusercontent.com/citusdata/citus/main/src/backend/distributed/README.md)
- DDL runs only through the first coordinator in the documented model. — [Citus cluster management](https://docs.citusdata.com/en/stable/admin_guide/cluster_management.html)
- Rebalancing requires a primary key or unique index that includes the distribution column. — [Citus cluster management](https://docs.citusdata.com/en/stable/admin_guide/cluster_management.html)

### Inferences
- [Background knowledge, unverified] Cross-shard unique constraints are impossible without a global index, so enforce uniqueness only when the unique key includes the distribution column. Foreign keys are only allowed between colocated tables on the distribution column, or to reference tables. Global IDs: use `bigint` sequences with per-node ranges, Snowflake-style IDs, or UUIDv7. Citus bakes a node-id prefix into sequences on workers when metadata-synced. Backups: take a consistent cluster-wide restore point with `pg_create_restore_point()` on every node after blocking distributed commits (Citus's `citus_create_restore_point`, which takes locks to block 2PC commits). Verify in `src/backend/distributed/operations/citus_create_restore_point.c`.
- DDL propagation recipe: in `ProcessUtility_hook`, resolve the relation, deparse the command (or pass the original query string), run it locally and on each worker's shard tables inside one 2PC transaction.

### Gaps
- None of the DDL, sequence, FK or restore-point behavior was verified against a fetched source this session.

## 7. Monitoring primitives

### Takeaway
Most needed views already exist in Citus and PG. A panel can be built from them.

### Cited Findings
- `citus_shards` has one row per placement with `table_name`, `shardid`, `nodename`, `nodeport`, `shard_size` and `citus_table_type`. It is useful for spotting size imbalance. — [Citus metadata tables](https://docs.citusdata.com/en/stable/develop/api_metadata.html)
- `citus_stat_statements` is the per-query statistics view on the coordinator (`queryid`, `executor`, `partition_key`, `calls`). Its stats are not replicated and do not survive crash or failover. It is capped by `pg_stat_statements.max`. — [Citus metadata tables](https://docs.citusdata.com/en/stable/develop/api_metadata.html)
- `citus_stat_activity` and `citus_dist_stat_activity` (distributed queries only) give cluster-wide activity, and `citus_lock_waits` gives cluster-wide blocking. — [Citus metadata tables](https://docs.citusdata.com/en/stable/develop/api_metadata.html)
- Background tasks drive rebalance, so progress can be read from the task tables. — [Citus cluster management](https://docs.citusdata.com/en/stable/admin_guide/cluster_management.html) (the specific progress view names, `citus_rebalance_status` and `get_rebalance_progress`, are [background knowledge, unverified])
- `pg_prepared_xacts` is the place to monitor in-doubt 2PC. — [PG two-phase docs](https://www.postgresql.org/docs/current/two-phase.html)

### Inferences
- Add `pg_stat_replication` (`replay_lag`, `write_lag`, `flush_lag`) per group, `pg_replication_slots` (to catch stuck logical-replication slots during moves), `pg_stat_database` and bucket-level size from `pg_total_relation_size` per shard.

### Gaps
- Node health and replication-lag views were not fetched. They are standard PG views and are listed here from background knowledge.

## 8. Effort estimates and incremental build order

### Takeaway
I found no citable statements from builders about effort in person-years. The cited facts only show the scale of the Citus codebase's feature list and how many years the global-snapshot patches have been in discussion.

### Cited Findings
- The Global snapshots patches have been under discussion on pgsql-hackers since 2018, with review in 2020 and no sign of commit in the sources found. — [thread index](https://postgrespro.ru/list/thread-id/2385436)
- Citus has needed a stream of releases to track each PG major: 14.0 (Feb 2026) for PG18, with 13.x stuck at PG17. — [Citus 14.2](https://www.citusdata.com/updates/v14-2/)
- Rebalancing without write blocking arrived in community Citus only with 11.0. — [Citus cluster management](https://docs.citusdata.com/en/stable/admin_guide/cluster_management.html)

### Inferences
- Suggested build order (author's judgment):
  1. Extension skeleton: `shared_preload_libraries`, catalogs, `create_distributed_table` with N hash buckets, and a node registry.
  2. Router/fast-path planner as a CustomScan, with a connection pool to workers.
  3. DML and DDL propagation through the utility hook, with 2PC and a recovery worker.
  4. Pushdown of multi-shard scans and aggregates, and reference tables.
  5. Logical-replication-based bucket moves, a rebalancer, and task tables.
  6. Per-group HA via Patroni, plus metadata sync to all nodes.
  7. Distributed deadlock detection, restore points and monitoring views.
  8. Only then consider a global snapshot (CSN/HLC) with core patches.
- Maintenance burden: because Citus tracks each PG major, a fork with core patches takes on the cost of rebasing every year. Keep core patches minimal (ideally none for MVP).

### Gaps
- No primary-source quotes on effort from Marco Slot, Onder Kalaci or the Postgres-XL/YugabyteDB teams were retrieved. The Citus SIGMOD 2021 paper was not fetched.
