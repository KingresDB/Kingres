<!-- SPDX-License-Identifier: AGPL-3.0-only -->
# kingres_shard: design of phase 9 (table lifecycle)

Status: Proposed design, 2026-11-15; not implemented.
Builds on [shard-phase2a.md](shard-phase2a.md) (0.2), [shard-phase4.md](shard-phase4.md) (0.3, bucket moves), [ha-phase5.md](ha-phase5.md) (0.4) and [shard-phase2b.md](shard-phase2b.md) (0.5, the row router), and produces extension version **0.6** with the upgrade script `kingres_shard--0.5--0.6.sql` (see "Version and upgrade"; the number moves if the order of phases changes). The REST contract changes from 0.6.0 to 0.7.0, additively.

Phase 9 of the roadmap in [architecture.md](../architecture.md) ("Phases 9 to 12") gives a table a life after `create_distributed_table`: it can be distributed while it holds rows, turned back into an ordinary table, given another distribution column, bucket count or colocation group, and it can be a table that the user partitioned by time. Every action has a task and a place in the web console.

How this note was checked. Claims about PostgreSQL are checked against the 18.7 source in this tree with the file and function named; a claim that was not checked is marked **[Needs verification]** and collected in the last section. Claims about the extension are checked against the committed versions of `contrib/kingres_shard` (README, `kingres_shard--0.3.sql`, `src/dist_table.c`); the working tree was being edited while this note was written and was not used. Claims about the control plane and the web console are checked against the committed `openapi.yaml` (0.6.0), `kingres/control-plane` (`internal/ops/tables.go`, README) and `kingres/web`.

## Sources read

- Extension: `contrib/kingres_shard/README.md` (whole file), `kingres_shard--0.3.sql` (catalog tables, `internal_check_distributable`, `internal_table_ddl`, `internal_check_referenceable`, the grants), `src/dist_table.c` (`kingres_create_distributed_table`, `choose_colocation`, `kingres_shard_copy_table_rows`, `kingres_create_reference_table`), all at the last commit.
- Designs: [shard-phase4.md](shard-phase4.md) (whole, with its implementation notes), [shard-phase2b.md](shard-phase2b.md) (sections 4, 10 and 11, and the goals table), [shard-phase2a.md](shard-phase2a.md) (the parts on DDL, sequences and reference tables), [ha-phase5.md](ha-phase5.md), [control-plane-3.md](control-plane-3.md), [ADR 0022](../adr/0022-the-complete-scan-stays.md), the roadmap section "Phases 9 to 12" of [architecture.md](../architecture.md), [documentation.md](../dev/documentation.md) and [coding-standards.md](../dev/coding-standards.md).
- Review: [reviews/2026-11-15-kingres-shard-0.3.md](../reviews/2026-11-15-kingres-shard-0.3.md). What it found and how this design avoids the same causes is in "What the review of 0.3 changes here".
- Contract and clients: `kingres/api/openapi.yaml` at 0.6.0 (`listTables`, `getTable`, `distributeTable`, `TableDetail`, `DistributeTableRequest`, `Task`, `TaskKind`, `TaskStep`, `ProblemCode`, `cancelTask`), `kingres/control-plane/README.md` ("Task engine: crash points and guarantees", decision 4 on quick tasks), `kingres/control-plane/internal/ops/tables.go` (`PrepareDistribute`, `stepDistribute`), `kingres/web/README.md`, `kingres/web/src/pages/DatabaseDetailPage.vue` and `src/components/DistributeTableDialog.vue`.
- PostgreSQL 18.7 in this tree: `src/backend/commands/tablecmds.c` (`AlterTableGetLockLevel`, the lock mode of `CREATE TABLE ... PARTITION OF` at `DefineRelation`, `ALTER TABLE ... ATTACH/DETACH PARTITION`, foreign key creation and `skip_validation`), `src/backend/commands/trigger.c` (`CreateTriggerFiringOn`: a row trigger on a partitioned table is created on every partition), `src/backend/commands/indexcmds.c` (the partition key rule for unique indexes), `src/backend/access/common/reloptions.c` (no storage parameters on a partitioned table), `src/backend/parser/parse_utilcmd.c` (`LIKE ... INCLUDING`), `src/backend/replication/logical/relation.c` and `tablesync.c` (how a subscription finds its target table and what the initial copy of a partitioned table reads), `src/include/catalog/pg_proc.dat` (`satisfies_hash_partition`).
- Citus documentation, read as documentation only (Citus is AGPL; no source of it was read or is used): [Citus utility functions](https://docs.citusdata.com/en/stable/develop/api_udf.html) (`create_distributed_table`, `undistribute_table` and its `cascade_via_foreign_keys`, `alter_distributed_table` with `distribution_column`, `shard_count`, `colocate_with`, `cascade_to_colocated`, the time partitioning functions), [Creating and modifying distributed objects (DDL)](https://docs.citusdata.com/en/stable/develop/reference_ddl.html) (a table that holds rows is copied, writes are blocked meanwhile, foreign keys, the distribution column type cannot be altered), the Citus 11.1 announcement [Citus 11.1 shards your Postgres tables without interruption](https://www.citusdata.com/blog/2022/09/19/citus-11-1-shards-postgres-tables-without-interruption/) and the Azure documentation of [the same functions](https://learn.microsoft.com/azure/cosmos-db/postgresql/reference-functions) (for `create_distributed_table_concurrently`: not inside a transaction block, one at a time, updates and deletes fail without a primary key or replica identity, a table that is referenced by or references a local table is refused, foreign keys to reference tables work). Those documents are the model for the user-visible behaviour; nothing else of Citus is used.

## Goals

1. **Distribute a table that already has rows**, in two ways: a *blocking* variant (one transaction, writers wait for the whole copy) and an *online* variant (writers keep working; writers wait only for the switch). Say which restrictions of `create_distributed_table` are lifted and which stay. Also for reference tables.
2. **Undistribute**: turn a distributed or a reference table back into an ordinary table, with the tables it is linked to by foreign keys.
3. **Alter**: change the distribution column, the bucket count or the colocation group of a distributed table (and of the tables colocated with it).
4. **Partitioned tables**: a table the user partitioned by range or list becomes a distributed table; its user partitions can be created, attached, detached and dropped as propagated DDL; say what a bucket move moves and what phase 12 can rely on.

Every goal has a SQL interface, a task of the control plane and an action in the web console.

### Not in scope

- Splitting a bucket in place (phase 10 mixes hash moduli in one table), rebalancing strategies and draining: phase 10 ([shard-phase10.md](shard-phase10.md)). Section "Relationship to bucket splitting" says what this note needs from it and why it does not wait for it.
- Distributed schemas (phase 11, [shard-phase11.md](shard-phase11.md)) and time-partitioning policies, retention and columnar storage (phase 12, [analytics-phase12-storage.md](analytics-phase12-storage.md), [analytics-phase12-queries.md](analytics-phase12-queries.md)). This note provides the partition operations those policies call; it adds no policy.
- Ledger tables as distributed tables (they stay refused, see section "Restrictions"), triggers, row-level security, exclusion constraints, inheritance, user sub-partitioning (more than one level of user partitions), hash partitioning chosen by the user, reference tables that are partitioned.
- Tables whose home is not the map group primary. `create_distributed_table` runs on the map group primary and needs the table there; that stays.
- Moving the data of a plain table between shard groups (it is only an ordinary table on its node).

## Decisions in one table

| # | Decision | Why |
|---|---|---|
| D1 | One engine, the **rebuild**, does distribute-with-rows, undistribute, and every change of a distributed table that moves rows: build a *shadow* table in the new layout, copy, catch up, switch in one two-phase transaction. | The same code, the same proof and the same crash rules for three features. A distributed table is a partitioned table, and PostgreSQL has no `ALTER TABLE ... PARTITION BY`, so every change of layout needs a new table. |
| D2 | The online catch-up uses **triggers and a key log**, not logical replication. | A subscription finds its target table by the schema and name of the published table (`relation.c`, `logicalrep_rel_open`: `RangeVarGetRelid(makeRangeVar(remoterel->nspname, remoterel->relname))`), so a shadow table that must live next to the original under another name cannot be the target of a replication of the original; and a rebuild would need `wal_level = logical`, slots and a restart. Phase 4's logical replication stays for moving buckets, where source and target are different nodes with equal names. |
| D3 | The online catch-up **re-reads the current row** of every logged key instead of replaying the change. | Order of replay does not matter, a lost or repeated log entry does not matter, and no row image is stored. The proof of completeness is a log that is empty under a lock that excludes writers. |
| D4 | The switch reuses phase 4's shape: lock that excludes writers, proof, one two-phase transaction on every node, the phase recorded in the same transaction as the catalog change, a delay before the old copy is dropped. It is **bounded in attempts and names its blocker**. | The 0.3 review (S4-04) found a switch that retried without limit and without saying why. |
| D5 | The blocking variant is **one transaction** (the caller's), with the same shadow-then-swap order; it keeps no durable operation record. | A failure rolls everything back by two-phase commit; nothing needs resuming. |
| D6 | A user-partitioned table is laid out as **hash on top, user partitions below**: `T` is `PARTITION BY HASH (dist column)`, each bucket partition `T_bNNNN` is itself `PARTITION BY RANGE|LIST (user key)`, and its leaves are `<partition>_bNNNN`. | The bucket partition stays the unit of placement, of a move, of the placement check and of every phase 2a/2b/4 path; nothing that counts or names bucket partitions changes. A user partition becomes a logical object (a row in `kingres.dist_partition`) with one leaf per bucket. |
| D7 | Triggers, row-level security, exclusion constraints, inheritance, statistics objects, ledger tables and foreign keys to or from ordinary tables **stay refused**; foreign keys to reference tables and to colocated tables on the distribution column, indexes, identity columns and sequences, privileges, comments and (last increment) storage parameters are **lifted**. | See "Restrictions that are lifted and restrictions that stay". |
| D8 | Changing the bucket count, the distribution column or the colocation group is a rebuild; changing only the colocation group between two groups with equal placement, or leaving a group (`colocate_with => 'none'`), is a **catalog change only**. Phase 9 does not need phase 10, but reads each bucket's own modulus. | Section "Relationship to bucket splitting". |
| D9 | No core patch. | Section "Core patches". |
| D10 | Bucket moves, other table operations and DDL on the colocation group exclude each other through one check in `kingres.bucket_move` preparation and in the preparation of a table operation. | Phase 4 already refuses DDL during a move; the reverse (a move during a rebuild) must be refused too, or the shadow would miss a bucket. |

## What the review of 0.3 changes here

| Finding | What phase 9 does |
|---|---|
| S4-01 (a bucket that moves away and back is invisible to the placement hash) | A rebuild creates a new table with a new OID and, when the colocation group changes, a **new colocation id that is never reused** (a sequence `kingres.colocation_id_seq`; today `choose_colocation` uses `max(colocation_id) + 1`, which reuses the number of a deleted highest group). `kingres.internal_placement_hash(colocation_id, parent)` must answer a reserved value when `parent` is not (or no longer) a table of that colocation group, so a scan that was planned with the old identity fails the comparison and raises `KS001`. Phase 10 adds the monotonic `colocation.placement_epoch` the review proposes, and phase 9 increments it in every swap and every catalog-only change (see "Relationship to bucket splitting"); phase 9 does not depend on it, because it never puts a table back under an identity it had. |
| S4-02 (the recovery worker never resumed moves) | `kingres.resume_table_operations()` and `kingres.cleanup_table_operations()` are called by the recovery worker after `recover_prepared_transactions()`, and a TAP test kills a session and waits for the worker (section "Tests"). `check_cluster()` reports a stalled operation whose session is gone. |
| S4-03 (a write straight into a bucket partition escapes the marker proof) | The source of an online rebuild is locked in `EXCLUSIVE` mode on the table itself and, for a distributed or partitioned source, on the parent **and** on every bucket partition and leaf the operation reads on that node. Writing into a partition directly conflicts there. The proof is a log that is empty after the lock, and the capture trigger is a row trigger on the parent, which PostgreSQL creates on every partition (`CreateTriggerFiringOn`: `partition_recurse` for a non-internal row trigger on a partitioned table), so a direct write to a partition is logged too. |
| S4-04 (the switch retried without limit and every attempt stalled everybody) | Locks are taken with `NOWAIT` in a polling loop first, then queued for at most `kingres_shard.rebuild_lock_timeout` (default 200 ms) so that a queued request cannot hold back other sessions for longer; at most `kingres_shard.rebuild_switch_max_attempts` attempts per call; the blockers (`pg_locks` holders with their pids and the start of their transaction) are written to the operation's `detail`; the control plane shows them and the task waits in the state `paused`. The documentation says that readers and writers of the table wait during an attempt and that a long transaction prevents the switch. |
| S4-05 (no version handshake) | Every function of this phase first checks that every active node reports extension 0.6 (`kingres.internal_require_version`), names the node that does not, and does nothing otherwise. |
| S4-06, S4-07 (helpers and records readable by everybody) | New helpers are revoked from `PUBLIC`; `kingres.table_operation` is readable by superusers only; the view `kingres.table_operations` has no error text and no node address and is readable by `pg_monitor`. |
| S4-09 (a move pins the xmin horizon of the map group primary for its whole length) | The online operations do not run as one statement. Each phase function returns when its phase ends, the long waits are loops of short transactions, and the control plane calls the phases one after the other. |

## Mechanism in PostgreSQL 18 terms

### What stays as it is

- A distributed table is on every node a table `PARTITION BY HASH (dist column)` whose partitions `<table>_b<NNNN>` are the buckets the node's group owns; the bucket of a key is `compute_partition_hash_value(key) % bucket_count` (README, "How it works in one page"). Phase 9 keeps this. Even a user-partitioned table has this shape at its first level (D6).
- The catalog tables of the README (`node`, `colocation`, `dist_table`, `bucket_map`, `map_version`) keep their meaning. Phase 9 adds columns and tables (section "Catalog").
- The planner hook, the distributed scan, the fragments and the row router are not changed. The scan reads every layout of this note (ADR 0022 rule 4): the local path is PostgreSQL's own plan over the table's partition tree, and the remote fragments name the parent table; both work for a bucket partition that is itself partitioned. A table that is being rebuilt is read as before, because the shadow is not in `kingres.dist_table` until the switch.

### Facilities used

| Need | Facility | Checked |
|---|---|---|
| Build the new table on every node | DDL through the existing node connections (`kingres_shard_begin_write_xact`, `kingres_shard_exec`) and through `kingres.internal_node_exec` for sessions that commit at once (phase 4) | README, `dist_table.c` |
| Copy rows to their owners | The batch sender of the row router of phase 2b (section 11.3, `kind=insert`, `unnest` of `text[]` parameters, version check), called with an **explicit description of the destination** (a placement, or "every node", or "one node") instead of the catalog, because the shadow is not registered | 2b design; the explicit destination is new |
| Capture changes of a table that stays in use | `AFTER INSERT OR UPDATE OR DELETE ... FOR EACH ROW` trigger on the source parent, which writes the key to a log table in the writer's transaction; `BEFORE TRUNCATE` trigger that raises an error | `trigger.c`: a non-internal row trigger on a partitioned table is created on every partition |
| Exclude writers, admit readers | `LOCK TABLE ... IN EXCLUSIVE MODE` (conflicts with `ROW EXCLUSIVE`, which every `INSERT`, `UPDATE`, `DELETE` and `COPY` holds, and with the `ROW EXCLUSIVE` held by a prepared transaction; allows `ACCESS SHARE`) | PostgreSQL lock conflict table, used the same way by phase 4 and `create_reference_table` |
| Swap | `ALTER TABLE ... RENAME` (needs `ACCESS EXCLUSIVE`), `ALTER INDEX ... RENAME`, `ALTER SEQUENCE ... OWNED BY`, in the transaction that is prepared on every node | `AlterTableGetLockLevel` |
| Atomic switch on all nodes and its recovery | The two-phase commit of phase 2a with `kingres.dtx_log`, `recover_prepared_transactions()` and the recovery worker | README |
| Make a user partition (a leaf) without blocking the bucket partition | `CREATE TABLE leaf (LIKE ... INCLUDING ALL)`, a `CHECK` that equals the bound, then `ALTER TABLE bucket_partition ATTACH PARTITION leaf FOR VALUES ...`. `ATTACH PARTITION` needs `SHARE UPDATE EXCLUSIVE` on the parent (`AlterTableGetLockLevel`, `AT_AttachPartition`), while `CREATE TABLE ... PARTITION OF` takes `ACCESS EXCLUSIVE` on the parent (`DefineRelation`: `parentLockmode`) | `tablecmds.c` lines near 872 and 4854 |
| Detach a leaf | Non-concurrent `DETACH PARTITION` needs `ACCESS EXCLUSIVE` on the parent (here: the bucket partition); `CONCURRENTLY` cannot run in a transaction block and cannot be prepared, so it is not used | `AlterTableGetLockLevel`, phase 4 notes |
| Unique indexes of a sub-partitioned table | PostgreSQL requires the partition key columns of every level in a unique index ("unique constraint on partitioned table must include all partitioning columns") | `indexcmds.c` |
| Storage parameters | Not allowed on a partitioned table ("cannot specify storage parameters for a partitioned table"), so a lifted restriction has to apply them to the leaves | `reloptions.c` |
| Row identity of the log | The primary key, or a unique index on `NOT NULL` columns, of the source | design |

### Core patches

None. Every facility above is an ordinary SQL command, a trigger or a hook that the extension already uses. If implementation finds that the batch sender cannot be driven with an explicit destination without touching core, that is recorded as a new patch id in [core-patches.md](../dev/core-patches.md); none is expected.

### Why the online variant does not use logical replication

Phase 4 moves a bucket with logical replication, and the instruction for this phase is to reuse that machinery where it fits. It fits the move because the source and the target are different nodes and the table has the same schema and name on both. A rebuild is different:

- The shadow of table `s.t` has to exist next to `s.t` while the application keeps using `s.t`, so it has another name. A subscription maps a published table to the local table of the same schema and name (`relation.c`: `RangeVarGetRelid(makeRangeVar(remoterel->nspname, remoterel->relname, -1), ...)`), and the initial copy runs `COPY` of the published name on the publisher (`tablesync.c`, `copy_table`). A loopback subscription of `s.t` would write into `s.t`. Put in another schema or database, the shadow still cannot receive the table under a different name.
- A subscription can only target nodes that already have a table of that name; the destination of a distribute is a set of nodes where `s.t` does not exist yet.
- It would make `wal_level = logical` (a restart of every node), a slot and a replication origin a precondition of a table operation.

What is reused from phase 4: the operation record with phases and a resume function, the lock that excludes writers while readers go on, the two-phase switch with the phase recorded in the same transaction as the catalog change, the delay before the old copy is dropped, the fault-point naming, the exclusion of DDL and moves, and `kingres.check_cluster()`. What is new is the capture by trigger and the proof by an empty log. A log written in the writer's own transaction is as exact as the marker row of phase 4 for the question both answer: "has every transaction that wrote before the lock been applied?". The argument is in "Proof of completeness".

### Relationship to bucket splitting

Phase 10 ([shard-phase10.md](shard-phase10.md), sections 1 and 2) splits **one bucket in place** into 2, 4, 8 or 16 children, so that a table can hold partitions of different hash moduli (PostgreSQL accepts this when each modulus divides the next larger one). A bucket is then a pair `(modulus, remainder)` with the remainder as its number; the catalog gains `bucket_map.modulus`, `colocation.max_modulus` and `colocation.placement_epoch`, and routing finds the bucket through an array indexed by `hash % max_modulus`. `colocation.bucket_count` keeps meaning the base modulus that a new table asks for. Changing the bucket count of a table by rebuild is a different operation: every row is read and written once, whatever the new count is. Phase 9 does not need phase 10 and does not wait for it; the two meet at these points, which this note follows:

- **A rebuild never assumes `hash % bucket_count`.** The bucket of a row is found with the routing array of the destination group; the placement handed to the batch sender is `bucket -> (group, modulus)` and `max_modulus`. A source group with split buckets is read partition by partition, which does not depend on moduli, and the copy of a source with `max_modulus = X` into a destination whose bucket count `M` is a multiple of `X` is local (below).
- **A new table that joins a colocation group with split buckets** (`create_distributed_table(..., colocate_with => T)` on a table with rows or without, a shadow of a rebuild into that group, `kingres.attach_partition` of a distributed table) creates each bucket partition with that bucket's own modulus, `FOR VALUES WITH (MODULUS m_b, REMAINDER b)` with `m_b` read from `bucket_map`; `create_distributed_table` matches an existing group by type, collation and `colocation.bucket_count` (the base) as today. `internal_table_ddl` and the code that creates the partitions of a table for a new node get this change in increment 9.1, because phase 10 is going to have the same list of functions to change (`kingres_shard_assert_bucket_layout(colocation_id)` of its test builds checks that every slot is covered exactly once, and the tests of this phase run with it on).
- **Changing to a uniform count.** A group whose buckets were split is made uniform again only by `alter_distributed_table(bucket_count => M)` with `cascade_to_colocated => true`: a rebuild of every table of the group into a new group of `M` buckets, all with modulus `M`. This is the "re-bucketing by redistribution" that phase 10 names as known limit.
- **Local copy.** When `M` is a multiple of the source's `max_modulus`, the new bucket `b'` covers the slots `h mod M == b'`, which lie inside exactly one old bucket (the one that covers slot `b' mod max_modulus`); the rebuild places `b'` on that bucket's group, so every node copies from its own old partitions to its own new ones and no row crosses the network. Otherwise rows of one old bucket go to several new buckets and a rebuild is the only way; phase 10 cannot do it.
- **Exclusion.** Phase 10 puts splits in `kingres.bucket_move` with `kind = 'split'`, one unfinished split per colocation group, and uses `kingres_shard_assert_no_move`. The claim of this phase (`table_operation_lock`) is checked by that function too, so a split refuses a group that an operation claims, and an operation refuses a group with an unfinished move or split or a bucket in `moving` or `splitting`. A rebuild that has claimed a group makes `move_bucket`, a split and the rebalance of the control plane refuse it.
- **The placement epoch.** Phase 10 replaces the placement-hash comparison by `colocation.placement_epoch`, which only a switch increments. Phase 9 increments it in every swap and every catalog-only change that touches a group (a table joins or leaves it, a group's rows are removed) and compares nothing else, so the review's S4-01 cannot happen through this phase either. Until phase 10 is merged, the reserved answer of `internal_placement_hash` described in the review table is what protects a scan.
- **Sizes.** A bucket partition that is itself partitioned (feature 4) reports `pg_total_relation_size` of its own storage only; `rebalance_plan().size_bytes`, `kingres.buckets` and the disk-size strategy must sum the leaves of `pg_partition_tree`. This is done in this phase (increment 9.5) because feature 4 creates the case.
- **Logical replication of a bucket that is partitioned** must publish through the root (`publish_via_partition_root = true`); the initial copy of a partitioned root reads the whole tree (`tablesync.c` builds `COPY (SELECT ... FROM <root>)` without `ONLY` for a partitioned root, and with `ONLY` for a plain table). Phase 4's move code is changed for this in increment 9.5.
- **Splitting a user-partitioned table's bucket.** Phase 10 refuses it (`split_unsupported`). For the layout of this phase that limit is **acceptable**: the size of a bucket of a time-partitioned table is bounded by the retention the user keeps (old partitions are dropped, which is what phase 12 automates), a split of a tree would copy every leaf and capture changes on every leaf, and the escape hatch exists and is complete: `alter_distributed_table(bucket_count => M, cascade_to_colocated => true)` rebuilds the table with more buckets. What would lift the limit later, all of it inside phase 10's mechanism: the children are built as partitioned trees with the parent's key, the leaf bounds are copied from the leaves of the old bucket (`kingres.partition_leaves` lists them), the copy and the capture trigger work per leaf (the trigger of this phase is already created on every leaf by `CreateTriggerFiringOn`), the `CHECK (satisfies_hash_partition(...))` of each child is put on the root of the child tree (inherited by its leaves, so `ATTACH PARTITION` finds it implied), and partitions created while the split runs are refused as DDL is. Its error should say "use `alter_distributed_table`" (the web console of phase 10 shows that hint).
- **What phase 9 asks of phase 10 in one list.** (1) The routing array and the per-bucket modulus are the one bucket function; `kingres.bucket_for` stays the public way to ask. (2) `kingres_shard_assert_no_move` covers `table_operation_lock`. (3) Sizes sum partition trees. (4) A split of a partitioned bucket stays refused with a hint. (5) `kingres.colocation.placement_epoch` is incremented by the switch of a table operation as well. (6) The version is 0.7 and the contract 0.8.0 when both phases land in this order (this note takes 0.6 and 0.7.0).

## Catalog and SQL interface

### Catalog changes (extension 0.6)

Synchronized tables (part of the catalog snapshot that `sync_catalog` copies; the jsonb of `internal_catalog_snapshot` and `internal_apply_catalog` gain the new columns and table):

```sql
ALTER TABLE kingres.dist_table
    ADD COLUMN partition_method text,   -- NULL, 'range' or 'list': how the user partitioned the table
    ADD COLUMN partition_key    text,   -- the key as pg_get_partkeydef gives it, without the method
    ADD CONSTRAINT dist_table_partition_check CHECK (
        (partition_method IS NULL AND partition_key IS NULL)
        OR (method = 'hash' AND partition_method IN ('range', 'list') AND partition_key IS NOT NULL));

-- The user partitions of a distributed table. A row stands for one leaf per bucket
-- (the leaf of bucket b is <partition_name>_b<bbbb> inside <parent>_b<bbbb>).
CREATE TABLE kingres.dist_partition (
    parent         regclass NOT NULL REFERENCES kingres.dist_table (relid),
    partition_name name     NOT NULL,   -- unqualified; in the schema of the parent
    bound          text     NOT NULL,   -- 'FOR VALUES FROM (...) TO (...)', 'FOR VALUES IN (...)' or 'DEFAULT'
    created_at     timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (parent, partition_name)
);

CREATE SEQUENCE kingres.colocation_id_seq;  -- never reused; 0.6 starts it above max(colocation_id)
```

`kingres.dist_table.relid` is a `regclass`, which follows a rename, so a swap by rename does not break the row; a rebuild still replaces the row's `relid` because the table has a new OID. Phase 12 may add columns for a time-partitioning policy to its own table; it must not change these.

Node-local tables on the map group primary, never synchronized (like `kingres.bucket_move`):

```sql
CREATE TABLE kingres.table_operation (
    op_id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    kind           text NOT NULL CHECK (kind IN ('distribute','distribute_reference','undistribute','alter')),
    mode           text NOT NULL CHECK (mode IN ('online')),     -- blocking runs have no record
    phase          text NOT NULL CHECK (phase IN ('preparing','shadow','copying','catching_up',
                                                  'switching','cleaning','done','failed','cancelled')),
    spec           jsonb NOT NULL,        -- what was asked, and what was decided (placement, names, set of tables)
    detail         text,                  -- last error, or what it waits for (the blockers)
    colocation_id  int,                   -- the colocation group(s) the operation locks; see item table
    attempts       int NOT NULL DEFAULT 0,
    cancel_requested bool NOT NULL DEFAULT false,
    cleanup_after  timestamptz,
    verified       text CHECK (verified IN ('ok','failed','skipped')),
    started_at     timestamptz NOT NULL,
    updated_at     timestamptz NOT NULL,
    finished_at    timestamptz
);

-- One row per table of the operation (a set when colocated tables are rebuilt together).
CREATE TABLE kingres.table_operation_item (
    op_id       bigint NOT NULL REFERENCES kingres.table_operation,
    ordinal     int    NOT NULL,
    relid       regclass NOT NULL,         -- the source (follows renames)
    shadow_name name,                      -- the shadow's name while it exists
    old_name    name,                      -- the retained old copy after the switch
    rows_copied bigint NOT NULL DEFAULT 0,
    rows_total  bigint,                    -- an estimate at the start (pg_class.reltuples), never a promise
    PRIMARY KEY (op_id, ordinal)
);
```

One operation per colocation group: a second node-local table `kingres.table_operation_lock (colocation_id int primary key, op_id bigint not null)` holds one row per colocation group that an unfinished operation uses, as source or as destination. `begin_table_operation` inserts all the rows it needs in one statement, so a set of groups is claimed atomically or not at all. This is the object that bucket moves, splits (phase 10) and table operations all check (decision D10): `kingres.internal_begin_move` must refuse a group that has a row here, and `begin_table_operation` must refuse a group that has an unfinished row in `kingres.bucket_move` or a bucket in state `moving`. A plain source that is not distributed yet claims no group until the destination group is chosen in `preparing`. The column `table_operation.colocation_id` records the destination group.

Capture structures exist only during an operation and are named after it: log table `kingres.tlog_<op_id>_<ordinal>` on each source node (columns `seq bigserial`, `op char`, the key columns of the source, and for an operation that changes the distribution column also the key columns of the old row), trigger `kingres_capture_<op_id>` on the source parent, and the shadow tables `kingres_shadow_<op_id>_<ordinal>` and, for partitioned shadows, `<final bucket partition name>` directly (a bucket partition name is `<table>_b<NNNN>` and cannot collide with a table that is not distributed yet; for a rebuild of an existing distributed table the shadow's partitions are named `kingres_shadow_<op_id>_<ordinal>_b<NNNN>` and renamed at the switch). `check_cluster()` knows these names (section "Check").

### Functions

All of them run on the map group primary, in a `READ COMMITTED` session, as a superuser (`EXECUTE` revoked from `PUBLIC`, `SET search_path = pg_catalog, kingres, pg_temp`). The functions with `_concurrently` cannot run inside a transaction block (`PreventInTransactionBlock`) because they commit in sessions of their own.

| Function | Meaning |
|---|---|
| `kingres.create_distributed_table(relation regclass, dist_column name, colocate_with regclass default null, bucket_count int default null) returns void` | **Changed**: the table may hold rows (blocking variant, one transaction) and may be a table the user partitioned by range or list. Same signature, so nothing that calls it breaks. |
| `kingres.create_distributed_table_concurrently(relation regclass, dist_column name, colocate_with regclass default null, bucket_count int default null) returns bigint` | **New**: the online variant. Returns the id of the operation when the table is distributed. |
| `kingres.create_reference_table(relation regclass) returns void` | **Changed**: foreign keys to other reference tables are lifted (section "Restrictions"). It already copies rows (blocking, writers wait). |
| `kingres.create_reference_table_concurrently(relation regclass) returns bigint` | **New**: the online variant, destination "every node". |
| `kingres.undistribute_table(relation regclass, cascade_via_foreign_keys bool default false, target_group int default null) returns void` | **New**: the blocking variant. `target_group` null means the map group. |
| `kingres.undistribute_table_concurrently(relation regclass, cascade_via_foreign_keys bool default false, target_group int default null) returns bigint` | **New**: the online variant. |
| `kingres.alter_distributed_table(relation regclass, distribution_column name default null, bucket_count int default null, colocate_with text default null, cascade_to_colocated bool default false) returns void` | **New**: the blocking variant. `colocate_with` is `null` (unchanged), `'default'` (the group chosen as `create_distributed_table` chooses it), `'none'` (a group of its own) or the name of a distributed table (a `regclass` in text form). |
| `kingres.alter_distributed_table_concurrently(...same arguments...) returns bigint` | **New**: the online variant. |
| `kingres.table_change_report(kind text, relation regclass, options jsonb default '{}') returns table (severity text, code text, object text, detail text)` | **New**, read-only, any role that may read the catalog may call it through the control plane's service role. `kind` is `distribute`, `distribute_reference`, `undistribute`, `alter`, `create_partition`, `attach_partition`, `detach_partition`, `drop_partition`. `severity` is `blocker`, `warning` or `note`; `code` is a stable word (`trigger`, `foreign_key_plain_table`, `dependent_view`, `unique_without_distribution_column`, `ledger_table`, `not_on_map_group`, `not_carried`, ...); `object` names the offending object; `detail` is a sentence. The table of codes is in the README of the extension. The report never stops at the first problem. The create/alter/undistribute functions call it and raise one error (SQLSTATE `0A000` or `55000`, class as today) that lists every blocker. |
| `kingres.begin_table_operation(kind text, relation regclass, options jsonb) returns bigint` | **New**, for the control plane and the `_concurrently` wrappers: runs the report, claims the colocation groups, records the operation (phase `preparing`) in a session of its own, returns the id. Idempotent for an operation that already exists for the same `options->>'request_id'`. |
| `kingres.table_operation_advance(op_id bigint, to_phase text, timeout_ms int default null) returns text` | **New**: runs the operation up to the end of `to_phase` (one of `shadow`, `copying`, `catching_up`, `switching`, `cleaning`) and returns the phase reached. Every phase can be called again after a failure or a crash (it checks what exists). `catching_up` returns when the backlog is under `kingres_shard.rebuild_drain_threshold` and has stayed there for one round; `switching` returns `switching` with `detail` set when the attempts of one call are used up (the caller calls again). Progress is in `kingres.table_operations`. |
| `kingres.cancel_table_operation(op_id bigint) returns void` | **New**: before the switch it undoes the operation (drops the capture trigger, the log tables, the shadow tables, releases the groups; the operation becomes `cancelled`); after the switch it does nothing and says so. |
| `kingres.resume_table_operations() returns int` | **New**: for every unfinished operation whose live mark (a session-level advisory lock held by the session that runs it, as in phase 4) is free: before `switching` continue or undo as `kingres.table_operation.cancel_requested` says; a `switching` row is decided by the two-phase commit (the phase becomes `cleaning` in the same transaction as the catalog change, so a `switching` row after recovery is a switch that did not commit; it waits `2 x node_timeout` like `resume_bucket_moves`). The recovery worker calls it every `kingres_shard.recovery_interval`, after the resolution of prepared transactions. |
| `kingres.cleanup_table_operations(force bool default false) returns int` | **New**: verifies and drops the old copies whose `cleanup_after` has passed (all with `force`); the recovery worker calls it. |
| `kingres.create_partition(parent regclass, partition name, bound text, if_not_exists bool default false) returns void` | **New**: a user partition of a distributed table. `bound` is `FOR VALUES FROM (...) TO (...)`, `FOR VALUES IN (...)` or `DEFAULT`; the function builds `CREATE TABLE ... PARTITION OF ... <bound>`, parses it with the raw parser and accepts it only if it is exactly one `CreateStmt` with a partition bound and no other clause (so `bound` cannot carry another statement). |
| `kingres.drop_partition(parent regclass, partition name, missing_ok bool default false) returns void` | **New**: drops the leaves and the row. |
| `kingres.detach_partition(parent regclass, partition name) returns regclass` | **New**: the partition becomes a distributed table of its own, named `partition`, in the colocation group of the parent. |
| `kingres.attach_partition(parent regclass, partition regclass, bound text) returns void` | **New**: `partition` is a distributed table colocated with `parent` (a metadata operation: its bucket partitions are attached as leaves) or an ordinary table on the map group (its rows are copied through the router and the table is consumed, as `create_distributed_table` consumes it). |
| `kingres.partition_leaves(parent regclass, partition name default null) returns table (node_id int, bucket int, leaf text)` | **New**, read-only, for phase 12: where the leaves of a user partition are (all of the table's leaves when `partition` is null). |
| `kingres.internal_require_version(version text) returns void`, `kingres.internal_copy_routed(...)`, `kingres.internal_table_digest(...)`, `kingres.internal_capture_trigger()` | **New** helpers; revoked from `PUBLIC`. `internal_capture_trigger()` is the trigger function and the only new `SECURITY DEFINER` function (section "Security"). |

Views (readable by `pg_monitor` and superusers): `kingres.table_operations` (op id, kind, mode, phase, tables, rows copied and estimated, backlog, attempts, whether verified, started, updated; no `detail` text and no node addresses; the control plane reads `detail` as the service role) and `kingres.partitions` (parent, partition, method, bound, is_default, leaf count, and the size of the leaves of this node). The existing `kingres.buckets` gets the size of the partition tree.

### Settings

| GUC | Default | Meaning |
|---|---|---|
| `kingres_shard.rebuild_batch_rows` | 10000 | Rows per chunk of the copy and of the replay. A chunk is one transaction on every group it writes (two-phase commit when it writes on two or more). |
| `kingres_shard.rebuild_batch_bytes` | 4MB | Upper bound of one chunk in text form. |
| `kingres_shard.rebuild_drain_threshold` | 1000 | Entries of the log under which the switch is attempted. |
| `kingres_shard.rebuild_lock_timeout` | 200ms | The longest a lock request of the switch is queued behind other sessions. |
| `kingres_shard.rebuild_switch_timeout` | 10s | The longest one switch attempt (lock, final replay, commit) lasts; at least `rebuild_lock_timeout`. |
| `kingres_shard.rebuild_switch_max_attempts` | 30 | Attempts in one call of `table_operation_advance(..., 'switching')`. |
| `kingres_shard.table_cleanup_delay` | 10min | How long the old copy is kept after a switch. 0 drops it after the check. |
| `kingres_shard.max_partition_leaves` | 20000 | `create_partition`, `create_distributed_table` of a partitioned table and every rebuild of one raise an error if the table would have more leaves (user partitions times buckets) than this. A table with 128 buckets and 120 monthly partitions has 15360 leaves, and PostgreSQL's planning cost grows with the partition count. |
| `kingres_shard.rebuild_verify` | on | The check of "Verification after the switch". |

Existing settings that apply: `kingres_shard.node_timeout`, `recovery_interval`, `node_conninfo`, `default_bucket_count`, `fault_injection`.

### Version and upgrade

`kingres_shard.control` default version becomes 0.6. `kingres_shard--0.5--0.6.sql` adds the columns, tables, sequence, functions, views and grants above, starts `kingres.colocation_id_seq` at `max(colocation_id) + 1`, and revokes the new helpers from `PUBLIC`. The `upgrade` regression test compares an upgraded installation with a fresh 0.6 one, as before. The library is loaded first (`shared_preload_libraries`), then `ALTER EXTENSION kingres_shard UPDATE` runs on every node; until it has run everywhere, the functions of this phase refuse to start (`internal_require_version`) and the older paths keep working, which avoids the outage the review found for 0.3 (S4-05) because the new code paths are entered only through the new functions. The catalog snapshot document gains a key `dist_partition` and two columns of `dist_table`; `internal_apply_catalog` accepts a document without them (a map group primary that is not upgraded yet).

## The rebuild engine

The three features that move rows share one engine. It takes a **source set** (one table, or the colocated tables that must change together) and a **destination layout** and produces, for each source table, a *shadow* table in the destination layout, then swaps shadow and source.

| Feature | Source | Destination layout |
|---|---|---|
| Distribute with rows | a plain table, or a user-partitioned table, on the map group primary | hash-partitioned by the new column over a colocation group's placement (user-partitioned: with the leaves below) |
| Distribute as reference table | a plain table on the map group primary | a plain table on every node |
| Undistribute | a distributed table (and, with cascade, the tables linked to it), or a reference table | a plain table on `target_group` |
| Alter | a distributed table (and, with cascade, its colocated tables) | hash-partitioned by the new column, with the new bucket count, in the new colocation group |

A distributed destination is described to the engine by a **placement**: `bucket -> (group, modulus)` with the `max_modulus` of the group. It is the placement of an existing colocation group, or, for a new group, the one `choose_colocation` makes (round-robin over the groups whose primary has `should_have_shards`), or, for a bucket-count change to a multiple, the one described in "Relationship to bucket splitting". The placement is decided in `preparing` and stored in `spec`; it never changes during the operation.

### Operation record and phases

An online operation is a row of `kingres.table_operation`; its `phase` is written before the action it announces, as in phase 4, and every action can be repeated.

1. **preparing.** In a session of its own (committed at once): `kingres.internal_require_version('0.6')` on every active node; the report of `table_change_report` (any blocker stops here with the full list); the claim of the colocation groups in `kingres.table_operation_lock` (a group with a moving bucket, an unfinished move, a split or another operation is refused: "colocation group N is busy: <what>"); the choice of the destination group and placement; the record. The cluster lock on `kingres.map_version` is **not** held during the operation, only in the switch. Nothing visible changes.
2. **shadow.** On every node that will hold rows, in sessions that commit at once (`internal_node_exec`): create the shadow parent and the partitions of the buckets the node's group owns in the destination placement, in the layout of "Shadow layout", without foreign keys. A shadow is not in `kingres.dist_table`, so no statement, scan or move sees it; it is an ordinary table that nothing refers to. Then, on each source node, create the log table and the capture triggers ("Capture").
3. **copying.** Copy every row of every source table to its place in the shadow ("Copy").
4. **catching_up.** Replay the log until the backlog is under `kingres_shard.rebuild_drain_threshold` ("Replay").
5. **switching.** Attempts, each bounded ("Switch"). The phase becomes `cleaning` in the same transaction that changes the catalog on the map group.
6. **cleaning.** Verify the old copies against the new tables ("Verification after the switch"), validate the foreign keys that the switch added `NOT VALID`, and after `kingres_shard.table_cleanup_delay` drop the old copies. The operation is `done`.

`failed` and `cancelled` are reached only before the switch committed; they leave nothing: the triggers, log tables, shadow tables and the claim are removed ("Crashes and lost nodes").

### Shadow layout

The shadow of a distributed destination is created from the source's definition by the same function that `create_distributed_table` uses today (`internal_table_ddl`: columns, defaults, identity, generated columns, collations, `NOT NULL`, check, unique and primary key constraints, indexes, owned sequences, owner), extended as follows.

- **Names.** The parent is `kingres_shadow_<op>_<n>` in the schema of the source. For a destination that is a *new* table (distribute) the bucket partitions get their final names `<table>_b<NNNN>` at once: the final name is free, because the table is not distributed yet. For a rebuild of an existing distributed table the shadow's partitions are `kingres_shadow_<op>_<n>_b<NNNN>` and are renamed at the switch. Indexes and constraints of the parent get the names PostgreSQL generates from the shadow's name and are renamed to the source's names in the switch, after the source's own have been renamed away (index and table names share one namespace per schema; phase 4 had the same collision for orphans and solved it the same way, by renaming the indexes to names that carry the operation id).
- **Partitions.** On each node only the buckets the node's group owns. A node that owns no bucket of the table gets the parent only, which is also what `create_distributed_table` does.
- **Foreign keys** are not created on a shadow. During the copy and the replay rows arrive in an order that violates a foreign key (a child before its parent, a parent delete before the delayed delete of the child), and the key's triggers would also make an unrelated `DELETE` on a reference table fail while the shadow lags. They are added in the switch, `NOT VALID` (cheap: no scan), and validated in `cleaning` under `SHARE UPDATE EXCLUSIVE`, which admits writers. `tablecmds.c` of 18.7 no longer contains the old error "cannot add NOT VALID foreign key on partitioned table" (a search for it finds nothing) and passes `skip_validation` to the partitions; **[Needs verification]** by a test that adds the key to a partitioned referencing table and validates it.
- **User-partitioned tables.** The bucket partitions are `PARTITION BY RANGE|LIST (<key>)` with the source's key, and each leaf `<partition>_b<NNNN>` is created with the bound of the source's partition `<partition>` (feature 4).
- **Sequences and identity.** The sequences owned by the source's columns stay the same objects: `OWNED BY` is moved to the shadow in the switch. On every other node the sequence is created `IF NOT EXISTS` as today. Values are node-local (phase 2a); after the copy, each node's sequence is advanced with `setval` to at least the largest value of its column in the table, so no node hands out a value that already exists as a *starting* point. Two nodes can still hand out the same value later, exactly as for a table distributed while empty; unique keys contain the distribution column anyway, and values that must differ across nodes should not come from a per-node sequence. Identity columns `GENERATED ALWAYS` are copied with `OVERRIDING SYSTEM VALUE`.
- **Ownership, privileges, comments.** The shadow gets the owner of the source, its table and column privileges (`OWNER TO`, `GRANT` and `REVOKE` generated from `relacl` and `attacl`) and its comments. They are applied in the switch, so no role sees a shadow.

### Capture (online only)

Before the copy reads anything, each source node gets, in a session that commits at once:

1. the **log table** `kingres.tlog_<op>_<n>(seq bigserial primary key, op "char", <key columns>)`. The key columns are the columns of the source's primary key, or of its first unique index over `NOT NULL` columns (by name); when the operation changes the distribution column, also that column's old value, so that a delete can be routed. A source without such a key cannot be rebuilt online: the report says `no_key_for_online` and only the blocking variant is available.
2. a **capture trigger** on the source parent: `AFTER INSERT OR UPDATE OR DELETE ... FOR EACH ROW EXECUTE FUNCTION kingres.internal_capture_trigger('<log table>')`. Both triggers are `ENABLE ALWAYS`, so that `session_replication_role = replica` does not bypass them (the split of phase 10 does the same). On a partitioned source PostgreSQL creates the row trigger on every partition (`CreateTriggerFiringOn`: `partition_recurse` for a non-internal row trigger on a partitioned table); a partition attached later is given the trigger by `ATTACH PARTITION` (**[Needs verification]**: the clone of row triggers at attach). The function writes the key of the old and of the new row in the writer's transaction, so the log row commits or rolls back with the change.
3. a **`BEFORE TRUNCATE` trigger** that raises "table X is being rebuilt; TRUNCATE is not allowed until it finishes". A truncate cannot be replayed by "read the current row of a key"; refusing it is simpler than a second code path. Other DDL on the source is refused by the DDL guard (the source is claimed by an operation).

Creating the triggers needs `SHARE ROW EXCLUSIVE` on the source, which conflicts with writers; it is taken with the rule of "Locks". The cost to writers while an operation runs is one extra insert into the log per changed row.

### Copy

- **A plain or user-partitioned source on the map group primary**: one stream. It reads the source in key order in chunks of `rebuild_batch_rows` rows (`WHERE key > last ORDER BY key LIMIT n`, one `READ COMMITTED` transaction per chunk) and sends each chunk to the destination with the batch sender: for a distributed destination each row goes to the group that owns its bucket in the placement (the bucket is computed by `kingres_shard_bucket`, as the router does); for a reference destination to every node; for a plain destination to the target node. The statement is the insert fragment of phase 2b (`INSERT ... SELECT ... FROM unnest($1::text[], ...) ON CONFLICT DO NOTHING`, with the version check against the **destination's** map version, which is current because the shadow is not in the catalog). Every chunk is one transaction that writes on one or more groups and commits with two-phase commit when it writes on two or more. The key of the last committed chunk is stored in `spec`, so a resumed copy continues after it; rows copied are counted in `table_operation_item.rows_copied`.
- **A distributed source** (undistribute, alter): every source node copies its **own** buckets, in parallel, through `internal_copy_routed`, which the map group primary starts on each node with `internal_node_exec` (one session per node, bounded by `node_timeout`, interruptible). A node reads `SELECT ... FROM ONLY <bucket partition>` (or the bucket partition tree) for each bucket it owns, in key order, and sends chunks to the destination as above. For a bucket-count change to a multiple, the destination bucket of every row of old bucket `b` is on the same group as `b`, so no row leaves the node.
- `ON CONFLICT DO NOTHING` makes a repeated chunk harmless. The copy does **not** apply the log. A row deleted after the copy read it leaves a ghost in the shadow, and a row updated after being read is stale there; both are repaired by the replay, which runs only after the copy has finished everywhere and is the only thing that deletes from the shadow. (Running the replay during the copy would let a stale chunk insert arrive after the replay of the delete that removed the row, and nothing would ever remove it.)
- Progress is `rows_copied` against `rows_total`, an estimate from `pg_class.reltuples`; it is shown as a percentage that is never 100 before the phase ends.

### Replay (online only)

The log is read in `seq` order, `rebuild_batch_rows` entries at a time. For each distinct key of the batch:

- read the **current** row of the source by key (on a distributed source: on the node that owns the log; a distributed table never moves a row between buckets by an `UPDATE`, which phase 2a refuses for the distribution column, and moves are excluded during an operation);
- if the row exists, make the destination equal to it: delete the key at the destination of the **old** row image (the log holds the old distribution value when the operation changes the column) and then insert the row with the insert fragment, without `ON CONFLICT`, at the destination of the current row;
- if no row exists, delete the key at the destination.

Then delete the batch's entries from the log, in the same transaction as the destination writes where one node holds both, otherwise after they have committed. Because the action is "make the destination equal to the current state of this key", entries can be processed in any order, twice, or after a crash, and the last pass over a key wins. One replay stream runs per log (a distributed source has one log per node, and a key is only ever logged on one node), so two replays never race on one key.

The backlog is the number of entries in the log tables; `catching_up` ends when it is under `rebuild_drain_threshold` after one full pass and has not grown over that pass. A source written faster than the replay reads keeps the operation in `catching_up`; `detail` says "the backlog does not shrink: N entries", and the operator lowers the load or cancels.

### Switch

One attempt, on the map group primary, in a session that stays alive for the attempt:

1. **Cluster lock** on `kingres.map_version` (`kingres_shard_lock_catalog`, the lock every catalog change takes). The claim guarantees that no move or other operation has the groups.
2. **Lock the sources on every source node** in `EXCLUSIVE` mode: the parent of every source table and, for a distributed or partitioned source, every bucket partition and leaf the operation reads on that node (review S4-03: a writer that goes straight into a partition conflicts there). `EXCLUSIVE` conflicts with the `ROW EXCLUSIVE` of every writer and of a prepared transaction and admits `ACCESS SHARE`: **writers of the source wait, readers go on**. Locks are taken in a fixed order (node id, then schema and name) with the rule of "Locks".
3. **Final replay.** The logs are replayed, by sessions of their own that commit, until a statement run **after** the locks were granted sees every log table empty. This is the proof ("Proof of completeness").
4. **The swap, one two-phase transaction** on the map group primary and every other active primary: on each source node, rename the source parent and its partitions to `kingres_old_<op>_<n>` and `kingres_old_<op>_<n>_<k>` (and its indexes and constraints to names that carry the operation id), `ALTER SEQUENCE ... OWNED BY NONE`, drop the capture triggers and the log tables; rename the shadow parent, partitions, indexes and constraints to the source's names; move sequence ownership; add the foreign keys `NOT VALID`; apply owner, privileges and comments; on the map group primary update the catalog (`dist_table`: the new `relid`, `colocation_id`, `dist_column`, `partition_*`; `colocation` and `bucket_map` for a new group; `dist_partition`; for undistribute the row is deleted; an old colocation group that has no table left is removed and its id is never reused) and raise the map version; set `table_operation.phase = 'cleaning'` and `cleanup_after`. The commit protocol is phase 4's: `PREPARE TRANSACTION` everywhere, the decision in `kingres.dtx_log`, `COMMIT PREPARED`.
5. **Release**: the locks go with the transactions.

An attempt that cannot get a lock in time, or whose final replay would take longer than `rebuild_switch_timeout`, rolls back, releases everything, records the blockers and the attempt count in `detail`, and sleeps a random 0.2 to 0.7 s; the phase returns to `catching_up`. After `rebuild_switch_max_attempts` attempts in one call, `table_operation_advance` returns with `detail` set and the caller (the control plane) waits and calls again. The operation never applies an attempt that lost its proof.

**What is blocked, and for how long.** From the first lock of an attempt to the commit of the swap: writers of the source tables on the source nodes (a plain or user-partitioned source: node 1; a distributed source: every group); readers of a source table during the swap itself, from the `RENAME` (`ACCESS EXCLUSIVE`) to the commit, which is the prepare and the commit of one transaction; nobody else. Statements on other tables are not touched. An attempt that fails to get its locks stalls others for at most `rebuild_lock_timeout`. The final replay is bounded by the drain threshold. These are design targets; the tests of "Tests" measure them and the implementation notes of the phase record the result. For the blocking variant the writers wait for the whole copy.

### Locks

Every lock the engine takes on a user table follows one rule, from the 0.3 review (S4-04): try `NOWAIT` first, in a loop of at most `rebuild_switch_timeout / 4` with a pause of 5 to 20 ms; if that does not succeed, queue **once** with `lock_timeout = rebuild_lock_timeout`. A queued request blocks every later request that conflicts with it, so the queue time is kept short, and a table that always has a writer is still reached by the second step. On failure the attempt is rolled back as a whole and the holders of the conflicting locks are read from `pg_locks` and `pg_stat_activity` (pid, role, `xact_start`; the query text only when the caller is a superuser) into `detail`.

### Proof of completeness

*Online.* Claim: at the commit of the swap, every committed change of every source table is in the shadow, and nothing else is.

1. The capture trigger exists before the copy's first read (`shadow` ends before `copying` starts) and is created under a lock that excludes concurrent writers, so no transaction is half inside it: for every transaction, all its changes to the table are logged or none.
2. Every row that exists when the copy reads it is copied; every row inserted, updated or deleted after the trigger exists has a log entry whose key identifies it.
3. The replay runs after the copy has finished on every node. For each logged key it sets the destination to the source's current state (upsert or delete), so any ghost or stale row of the copy that has a log entry is corrected. A ghost without a log entry cannot exist: a row that disappears from the source after being copied was deleted by a statement that logged it.
4. The switch's `EXCLUSIVE` lock is granted only after every earlier writer has committed or rolled back; later writers wait. The final replay then runs against a source that cannot change. When a statement started after the locks sees empty logs, every key that changed before the lock has been replayed after its last change (its entry was written after that change and consumed after it), and none can change now.
5. The swap runs in the same locked state, so the shadow renamed to the source's name is the state the final replay left.

What the proof does not cover: a change that bypasses the table's triggers, which `ENABLE ALWAYS` limits to `ALTER TABLE ... DISABLE TRIGGER` by the owner during the operation (the DDL guard refuses that statement on a claimed table, so the case needs a superuser who edits the catalog). The documentation says so, and the verification below detects it.

*Blocking.* The source is locked `EXCLUSIVE` before the first read and until the commit, so no write can be missed. The copy counts the rows it read and the sum of the rows each group acknowledged (the tags of the insert fragments); the transaction fails unless they are equal and equal to `count(*)` of the source taken under the lock. PostgreSQL's tuple routing rejects a row that does not belong to a bucket of the node, so a row cannot land in a wrong bucket.

### Verification after the switch

In `cleaning`, before the old copy is dropped, `kingres.internal_table_digest(table)` runs on every node over the old copy (frozen, and kept for this) and over the new table, each returning the row count and the sum of `hashtextextended(row_text, 0)` over all rows, where `row_text` is the whole-row text form under the transmission modes. The sums are compared (order does not matter). A difference sets `table_operation.verified = 'failed'`, **keeps the old copy**, is reported by `check_cluster()` as `table_verification_failed`, and ends the task with an error that names the table and the counts; the data is not rolled back (the switch has committed and writers have written to the new table), the old copy is the evidence. `kingres_shard.rebuild_verify = off` skips the check (`verified = 'skipped'`). The cost is one read of the old copy and one of the new table, outside any lock. **[Needs verification]** that the text form of a row is equal on two nodes for every column type (the nodes must agree on collation, which `add_node` enforces; `money` depends on `lc_monetary`, which the transmission modes do not fix: the digest function sets it to `C`; a test compares a `money` column).

### Crashes and lost nodes

All state is in `kingres.table_operation` (a committed row on the map group primary) and in objects whose names carry the operation id. Every action is idempotent: it checks what exists before it creates.

| Where it stops | What is left | What the recovery worker does |
|---|---|---|
| The session that runs a phase dies (before `switching`) | the record; shadows, triggers and logs as far as they were created | `resume_table_operations()`: when the live mark is free and `cancel_requested` is not set, it continues the phase (the control plane normally does that first); otherwise it undoes: drops the triggers, logs and shadows on every reachable node, releases the claim, marks `cancelled` (or `failed` with the reason). A node that cannot be reached keeps its leftovers; `check_cluster()` reports them as `shadow_table`, `log_table`, `capture_trigger` until the node is back and `resume` removes them. |
| A node that holds shadow partitions is lost before the switch | the same | The operation cannot continue; `resume` undoes it on the nodes that answer. If the node is replaced by a failover (phase 5), the promoted standby has the shadow objects (physical replication) and the operation continues when the node is reachable at its catalog address. |
| The map group primary fails over | the record, if it was committed before (the control plane's calls use the database's durability level, synchronous for a group with a synchronous standby) | The promoted node's recovery worker runs `resume`. Shadows whose operation id has no record are orphans: reported by `check_cluster()` and dropped by `resume`. |
| Crash during a copy chunk | the chunk's transaction is prepared on some groups | `recover_prepared_transactions()` decides it; the chunk is repeated; `ON CONFLICT DO NOTHING` makes this harmless. |
| Crash during a replay chunk | the same | Repeated: the replay sets state, not a delta. Log entries are deleted after the chunk commits. |
| Crash in `switching`, before `PREPARE` | locks released with the sessions; nothing changed | The phase is `switching`; `resume` waits `2 x node_timeout` after `updated_at` as phase 4 does, finds the catalog unchanged and sets the phase back to `catching_up`. |
| Crash after some nodes prepared | prepared transactions | `recover_prepared_transactions()` commits them if the decision is in `dtx_log` and rolls them back otherwise. The phase is `cleaning` if and only if the swap committed on the map group (the same transaction). `resume` reads the phase. |
| Crash in `cleaning` | old copies, perhaps foreign keys not yet validated | `resume` finishes: verifies, validates, drops after `cleanup_after`. |
| The recovery worker | n/a | The control plane calls `resume_table_operations()` when a step's database call failed, and the worker calls it every `recovery_interval`. A test kills a session and waits for the worker (S4-02). |

No space on a destination disk fails the chunk with PostgreSQL's error; the operation stays in `copying` with `detail` set; space can be freed and the phase called again, or the operation cancelled. Disk use of an online operation is the source plus the shadow (the same size plus indexes) plus the log; the control plane compares the estimate with the free space of every server that holds a destination (it knows server resources) and refuses with a problem that names the server; the extension cannot see free space and does not check.

### Interaction with the rest of the system

| With | Rule |
|---|---|
| **Bucket moves** | A move of a group that an operation claims is refused in `internal_begin_move`; an operation is refused while a move of one of its groups exists or a bucket is `moving`. The rebalance task of the control plane does not start moves of a claimed group. A move that was started first makes the operation fail to start with `operation_in_progress` (or `rebalance_in_progress` when the control plane's rebalance task is running). |
| **The map version and `KS001`** | The shadow changes no map; the swap raises the map version once on every node. A statement planned before the swap meets the new version in a fragment (phase 2b, section 10.2: `KS001`, the origin reloads and retries once when it can), or a placement hash that no longer names that table (the reserved value, see the table of review findings), or a different partition count. A statement that holds locks on the old tables holds the swap back (its lock request waits, bounded by the attempt timeout). After the swap the name resolves to the new table: a cached plan is rebuilt from its raw parse tree when the rename's invalidation arrives. |
| **Two-phase commit** | Chunks and the swap use phase 2a's protocol unchanged, including its fault points. A user-level prepared transaction that holds a lock on a source blocks the switch like any writer. |
| **Reference tables** | A reference table is a source of undistribute and a destination of distribute. A foreign key from a distributed table to a reference table is kept across every operation. The advisory writer locks of phase 2a (S2-06) are taken, in node order, for the final replay and the swap of a reference source, as `add_node` does. |
| **Ledger tables** (`kingres_ledger`) | A ledger table, and the history table of an updatable ledger table, are refused as a source of every operation of this phase (blocker `ledger_table`): their capture trigger and protection hook are the integrity mechanism, and a copy into a new table would start a new history. Ledger design notes say `ledger_enable` refuses partitioned tables and `create_distributed_table` refuses tables with triggers, so a distributed ledger table does not exist today. The contract text of `distributeTable` ("the table can be a ledger table at the same time") says otherwise; see the report of contradictions at the end. A table that is undistributed can then be put under the ledger with `enableTableLedger`. A ledger of a distributed table's changes is made with `INSERT ... SELECT` into a ledger table, which phase 2b allows. |
| **Standbys and failover (phase 5)** | Standbys receive shadows, logs, renames and drops by physical replication, so a promoted standby has what the old primary had committed. No slot or subscription is used, so nothing is lost by a failover (a 0.3 bucket move must be restarted after one). The control plane's free-space check includes the standbys of the destination groups, which hold a copy of the shadow. An operation in `switching` during a failover of a participating group is decided by the two-phase commit and the recovery across failover that phase 5 specifies. A failover of the map group moves the manager leader and the operation record with it. |
| **The scan (ADR 0022)** | Every layout of a destination is read by the existing scan, and the comparison tests of `t/013`-style run against each layout and against the old layout during an operation (section "Tests"). Nothing of this phase narrows the scan. |
| **`kingres.check_cluster()`** | New checks: `table_operation_stalled` (unfinished, live mark free, `updated_at` older than three `node_timeout`), `shadow_table`, `log_table`, `capture_trigger` (objects with the id of an operation that is `done`, `failed`, `cancelled` or missing), `old_table` (overdue by an hour), `table_verification_failed`, `dist_partition_mismatch` (a leaf missing on the owner of its bucket, or a leaf with no row). |

## Feature 1: distributing a table that already has rows

### Restrictions that are lifted and restrictions that stay

`create_distributed_table` and `create_reference_table` refuse a long list today (README). With rows the table is also moved by a rebuild, which constrains what can be carried. The decision for each:

| Restriction today | After phase 9 | Reason |
|---|---|---|
| The table must be empty | **Lifted**: blocking and online variants | The goal. |
| Ordinary, permanent, not partitioned | **Lifted for a table partitioned by range or list** (feature 4); stays for a hash-partitioned table, a partition, a temporary or unlogged table | The hash level belongs to the extension. |
| No inheritance | Stays | A child or parent would be left with a table that is no longer the same object. |
| **Foreign keys** from the table | **Lifted** when the target is a reference table, or a distributed table of the same colocation group and the key uses the distribution column on both sides (the rule of propagated `ADD CONSTRAINT` today). Recreated on the shadow in the switch, `NOT VALID`, validated afterwards. Stays refused when the target is an ordinary table (blocker `foreign_key_plain_table`), a distributed table of another group, or a key that does not use the distribution column | Phase 2a refuses a key from a distributed table to an ordinary table and from an ordinary table to a distributed table; those rules are not changed here. |
| Foreign keys **to** the table | Lifted for a referencing distributed table of the group the table joins, on the distribution column; stays refused when the referencing table is an ordinary table (`referenced_by_plain_table`), and from a reference table to a distributed table (the 2a rule) | Same rules. Citus documents the same ordering ("distribute the referenced table first"). A set of ordinary tables linked by keys: see below. |
| No user triggers | **Stays** | A trigger's function is not synchronized across nodes (roles, functions and types are not, phase 2a), the row router and shipped writes assume no row trigger on a node that did not receive the statement, and rows applied by a move or by this phase's copy fire no trigger. The report lists them; dropping them is the user's choice. |
| No row-level security | Stays | Policy functions are not synchronized. |
| No exclusion constraints | Stays | PostgreSQL does not allow them on a partitioned table. |
| **Storage parameters** (`fillfactor`, `autovacuum_*`) | **Lifted in the last increment** (9.9): applied to the leaves of the shadow (PostgreSQL does not allow them on a partitioned parent: `reloptions.c`, "cannot specify storage parameters for a partitioned table"), recorded in `kingres.dist_table.reloptions`, and copied from the source partition by phase 4's move code. Until then they stay refused | A rebuild that silently dropped them would change the table's behaviour. `LIKE ... INCLUDING ALL` does not copy them (`INCLUDING STORAGE` is the column-level setting; **[Needs verification]** in `parse_utilcmd.c`), so the move needs a change. |
| Unique and primary keys must contain the distribution column | **Stays** (PostgreSQL's rule) | For a user-partitioned table they must also contain the user partition key (`indexcmds.c`, "unique constraint on partitioned table must include all partitioning columns"). The report names each key and the columns that would fit; the web console lists them. |
| Type with a default hash operator class and extended hash function | Stays | The bucket is a hash. |
| **Indexes** | Carried: every index of the table (expression, partial, covering). Non-unique indexes are built on the shadow parent **after** the copy (bulk loading into an indexed table is slower); unique ones exist from the start, because `ON CONFLICT DO NOTHING` needs them. `CREATE INDEX CONCURRENTLY` stays refused as DDL | |
| **Identity columns and sequences** | **Lifted**, values node-local as for an empty table (see "Shadow layout"). A `serial` default may not be added to a distributed table by `ADD COLUMN` and stays so; existing ones are carried | Existing behaviour of the propagated `ADD COLUMN`. |
| Column defaults that call a function | Carried; the report probes that the function exists on every node (`to_regprocedure` through `internal_node_exec`) and raises `default_function_missing` with the node | Functions are not synchronized. |
| Privileges, comments, ownership | **Lifted**: carried (today they are not copied: "grant after creating") | A table in use has users; dropping their privileges would be a regression. |
| Statistics objects (`CREATE STATISTICS`) | Stays (warning `not_carried`) | Refused as DDL on a distributed table today. |
| Per-column storage, compression and statistics target, `CLUSTER ON`, replica identity | Not carried: warning `not_carried` | The shadow is built from the definition. A non-default tablespace is a blocker (`tablespace`): tablespaces are not synchronized. |
| Dependent objects: views, materialized views, rules, functions that use the table's row type, publications, policies | **Blocker** `dependent_object`, the object named | A swap by rename leaves these bound to the *old* table. Plain views can be recreated: the option `dependent_views => 'recreate'` of the control plane request (increment 9.8) captures `pg_get_viewdef`, owner, `reloptions`, grants and comments in dependency order, drops them in the switch and recreates them on the new table; anything else is refused. |
| Ledger table or history table | Stays (`ledger_table`) | See the interaction table. |
| The table must be on the map group primary | Stays (`not_on_map_group`) | `create_distributed_table` runs there and needs the table there; `homeShardGroupId` in the contract says which group holds it. |

**A set of ordinary tables linked by foreign keys** cannot be distributed one by one: distributing the parent needs the key from the child dropped, and the other way round. The extension does not do that dance (it would drop integrity checks for the length of a copy). The control plane offers it as **`distribute with related tables`** (increment 9.7): a task with a list of tables and a column for each; it captures the definitions of the keys, drops them, distributes the tables in dependency order, adds the keys back (`NOT VALID`, then `VALIDATE`), and on any failure re-adds the keys to whatever state the tables are in and reports. The dialog says that no key is enforced between the first drop and the last add, and that the blocking variant shortens that window. It is a control-plane feature because it is a sequence of ordinary statements that the user could run by hand.

### Blocking variant (`create_distributed_table` on a table with rows)

Everything runs in the caller's transaction on the map group primary; the remote work is in the transaction branches that the commit protocol prepares.

1. Cluster lock, `kingres_shard_require_map_group`, the report (any blocker raises one error listing all), the version handshake, `kingres_shard_assert_no_move`, the claim check of the group.
2. `LOCK TABLE <t> IN EXCLUSIVE MODE` (waits under the caller's `lock_timeout`; from now to the commit writers wait, readers do not).
3. Create the shadow on every node, in the branches that will be prepared (the same remote calls as today, with the shadow's names), and the owned sequences.
4. Copy through `internal_copy_routed(source, destination)` in this transaction; count the rows read and the rows acknowledged and compare both with `count(*)` taken under the lock; any difference raises an error and everything rolls back.
5. Build the non-unique indexes on the shadow parent.
6. Take `ACCESS EXCLUSIVE` on the source (readers now wait), do the swap of "Switch", step 4, register the table, sync the catalog, commit. The old copy is renamed and kept for `table_cleanup_delay`; a blocking operation has no record in `kingres.table_operation`, so the old copies are listed in a small node-local table `kingres.table_old_copy(relid regclass, dropped_after timestamptz)` that `cleanup_table_operations()` also reads (the online operations use the same table, so there is one cleanup path).

What is blocked: writers of the table from step 2 to the commit (the whole copy), readers from step 6 to the commit. Everything else keeps working. **When to use it:** a small table, or a table for which a window without writers is acceptable. The web console shows "writers wait while about N rows are copied", with N from the table's estimate, and gives no time (the rate depends on the cluster).

### Online variant (`create_distributed_table_concurrently`)

The engine with: source = the plain table on the map group primary, claim = the destination group, destination = the placement chosen in `preparing`. Specific points:

- **No bucket move is needed afterwards**: the shadow's buckets are placed where they belong from the start.
- It needs a primary key or a unique index over `NOT NULL` columns (the key of the log); otherwise `no_key_for_online` and only the blocking variant is offered.
- Writers keep working throughout; their statements run as statements on a plain table on node 1 until the swap and as distributed statements after it. A prepared statement or a cached plan re-plans on the swap's invalidation.
- `COPY` into the table fires the row triggers and is captured; `TRUNCATE` and `ALTER TABLE` are refused while the operation runs.

### Reference tables

`create_reference_table` already copies the rows (blocking: `EXCLUSIVE` on the table, one transaction); it changes only in the foreign key rule (a key from the table to another reference table is lifted: the keys are added on the nodes after the tables, as `add_node` does). `create_reference_table_concurrently` uses the engine with the destination "every node": the shadow is a plain table on every node, the copy goes to all nodes (one chunk, one two-phase transaction on all of them), the final replay and the swap hold the reference-table advisory locks of phase 2a. Before the swap the table is not a reference table, so no cluster-wide writer serialization is needed; after it, writers use the 2a protocol.

## Feature 2: turning a table back into an ordinary table

### What it means

`undistribute_table(relation, cascade_via_foreign_keys, target_group)` replaces a distributed or reference table by an ordinary table with the same name, columns, defaults, constraints, indexes, owner, privileges and comments on the primary of `target_group` (the map group when null), and removes the table from the catalog. The data must fit on that node; the control plane compares `sizeBytes` with the free space of the target server before it starts.

### Distributed table, blocking

Cluster lock, report, claim; `EXCLUSIVE` on the parent of the table on every node (writers wait on every group, readers go on); create the ordinary shadow on the target node; copy by `INSERT INTO shadow SELECT * FROM <table>` through the distributed scan on the target node, in the caller's transaction, counting; compare with `count(*)` under the lock; swap; commit. Writers wait for the whole copy.

### Distributed table, online

The engine with: source = the table on every group; capture on every group's parent; destination = the shadow on the target node. The copy runs on every source node (each reads its own buckets and sends to the target node); the replay runs per node and reads the current row of each logged key from the node's own partitions. The switch locks every group's parents and bucket partitions in `EXCLUSIVE` mode, replays to empty, and swaps in one two-phase transaction on all nodes:

- on the target node: rename the distributed parent and its bucket partitions to the retained names, rename the shadow to the table's name, add the foreign keys `NOT VALID`, apply owner, privileges and comments;
- on every other node: rename the parent and its bucket partitions to the retained names;
- on the map group: delete the `dist_table` row, drop the colocation group if no table remains in it, raise the map version, set the phase.

The retained old tables are no longer in the catalog, so their drop is an ordinary `DROP TABLE` (the `sql_drop` event trigger deletes catalog rows only for tables that are in `kingres.dist_table`; phase 4's notes record that the trigger used to count the map version up for a drop with no row, and was fixed). Dropping the partitions of a node is a loop of short transactions.

### Reference table

A reference table is complete on every node, so no rebuild is needed on the node that keeps it. `undistribute_table` of a reference table:

1. cluster lock, report, no group claim; take the advisory writer locks of phase 2a on every node (writers wait) and `EXCLUSIVE` on the table on every node;
2. verify the copies are equal: `internal_table_digest` on every node; a difference stops with the nodes named (copies can differ after a failed write path or manual edits; `target_group` says whose copy stays, and the report shows the difference);
3. in one two-phase transaction: on the target node remove the `dist_table` row (the table stays, with the same OID, so views, grants and keys that depend on it stay); on every other node drop the table; raise the map version.

Writers wait from step 1 to the commit; the time is proportional to the size of a table that is small by the definition of a reference table. A dependent view on another node makes that node's drop fail, so the report lists dependents on every node first. There is no `undistribute_table_concurrently` for a reference table: the blocking variant keeps readers going and writers wait for a short time. **[Recommendation]**

### Foreign keys

Keys connect the tables of the catalog in a graph: every `kingres.dist_table` row is a node and every foreign key whose both ends are in the catalog is an edge (reference to reference, distributed to reference, distributed to colocated distributed). The function computes the connected component of the table.

- `cascade_via_foreign_keys => false` (default): when the table has an edge, the report raises `foreign_key_linked` with the tables and keys: "undistribute them together with `cascade_via_foreign_keys => true`, or drop the keys". This is Citus's rule (its documentation: tables that have or are referenced by foreign keys cannot be undistributed unless `cascade_via_foreign_keys` is true), with one exception: a key **from a distributed table to a reference table** is kept (the reference table is on every node, including the target node, so the ordinary table can keep a key to it). A reference table that is referenced by distributed tables cannot be undistributed without them.
- `cascade_via_foreign_keys => true`: every table of the component is undistributed in one operation (one claim, one switch). They go to the same node, so `target_group` applies to all, and the keys between them are recreated as ordinary keys `NOT VALID` and validated in `cleaning`. The preview lists the component with sizes; undistributing 40 tables is one task.
- A key from an ordinary table to the table cannot exist (phase 2a refuses it).

### Locks, blocked time, recovery

As in the engine: blocking, writers of every table of the component wait for the copy; online, writers wait for one switch attempt. The table is distributed until the swap commits and ordinary after; `resume` decides by the phase. Statements in flight at the swap get `KS001` or read the retained copy, which is frozen (writers waited for the switch).

## Feature 3: changing the distribution column, the bucket count or the colocation group

`alter_distributed_table(relation, distribution_column, bucket_count, colocate_with, cascade_to_colocated)` and its `_concurrently` form change one or more properties of a distributed table. The options follow the meaning of the Citus function of the same name (its documentation: distribution column, shard count, `colocate_with` as `default`, `none` or a table, and `cascade_to_colocated` applying the change to the tables colocated with the table so that the colocation is kept; with `false` the table's colocation is broken). The argument names follow this project's vocabulary (`bucket_count`).

### The cases

| Request | What happens | Data moved |
|---|---|---|
| `colocate_with => 'none'`, nothing else | One atomic catalog change: a new colocation group (an id from `kingres.colocation_id_seq`) with a copy of the table's `bucket_map` rows; the table's `colocation_id` set to it. No table is touched. | none |
| `colocate_with => <table T2>` or `'default'`, T2's group has the same distribution type, collation and bucket count, and an **identical placement** | One atomic catalog change: the table's `colocation_id` becomes T2's. The old group is removed if empty. | none |
| The same with a different placement | Rebuild into the layout of T2's group. | the rows that are on another group than T2's placement gives them |
| `bucket_count => M` (M differs) | Rebuild into a new colocation group with M buckets; the placement is the old one when M is a multiple of the old `max_modulus` N (new bucket `b'` on the group of the old bucket that covers slot `b' mod max_modulus`: no row crosses the network), otherwise round-robin over eligible groups. | all rows, locally when M is a multiple of N |
| `distribution_column => c` | Rebuild. A new group if the type or collation of `c` differs from the old group's, otherwise a new group of its own with the old bucket count and placement. The table is not colocated with its old peers: colocation means that equal values of the key give equal buckets, which is no longer true for another column. | all rows |
| A combination | One rebuild. | as above |

The catalog-only cases take the cluster lock, check that no bucket of either group is `moving` and that no operation claims either, and are one two-phase catalog change. Both groups' tables must have no foreign key that the change would invalidate (see below).

### Which tables change

- `cascade_to_colocated => false`: the table leaves its group. It is refused when something depends on the colocation: a foreign key between the table and a table of its old group (blocker `foreign_key_colocation` naming the key); the user passes `cascade_to_colocated => true` or drops the key.
- `cascade_to_colocated => true`: **every table of the group is rebuilt in one operation**, with one switch. Colocation in the catalog is a declaration, and a statement that joins tables of different groups is still correct (it runs through the scan, ADR 0022), so the tables could be switched one at a time without wrong answers; but the foreign keys between them would have to be dropped in between. So the switch is one. The cost is that the final replay and the swap lock all tables of the group together, and that the disk needed is the whole group.
- A set changes its distribution column only when every table of it has a column of that name and the same type; otherwise the request is refused with the table named. A per-table list of columns is a later extension.

### Blocking and online

Both exist. The blocking one is the caller's transaction (writers of the set wait for the copies). The online one is the engine with capture on every group, as for undistribute, with the destination placement above. When the distribution column changes, the log also keeps the old value of the new column so that a delete can be routed, and the replay deletes the old image at its destination before inserting the new one; an `UPDATE` of the new distribution column is an ordinary update on the old table (it was not its distribution column) and may move the row to another destination bucket, which the replay handles because it acts on the key.

### What `alter` needs from other phases

- It does not need splitting: see "Relationship to bucket splitting".
- It needs from the row router (2b) the batch sender with an explicit destination and the version check against the destination's map version.
- It needs from phase 4 the exclusion (increment 9.1: the check of `table_operation_lock` in `internal_begin_move`) and nothing else.

## Feature 4: partitioned tables as distributed tables

### The layout, and why

Today a user-partitioned table is refused (README: "a partition ... is refused", `internal_check_distributable` refuses `relkind = 'p'`). The layout of a distributed partitioned table (decision D6) is **hash on top, the user's partitioning below**:

```
orders                      PARTITION BY HASH (customer_id)          every node: the parent
  orders_b0007              PARTITION BY RANGE (created_at)          only on the group that owns bucket 7
    orders_2026_11_b0007    FOR VALUES FROM ('2026-11-01') TO ('2026-12-01')
    orders_2026_12_b0007    FOR VALUES FROM ('2026-12-01') TO ('2027-01-01')
  orders_b0012              PARTITION BY RANGE (created_at)          ...
```

The user-visible partition `orders_2026_11` is a **logical object**: a row of `kingres.dist_partition` (`parent`, `partition_name`, `bound`) and, on each node, one leaf `<partition>_b<NNNN>` inside each bucket partition the node owns. The name of a leaf is `<partition name>` + `_b` + the bucket number, so the rule that a name has 6 characters of suffix is the same for leaves (a user partition name may have at most 57 bytes; the report says `name_too_long`).

Alternatives considered, and why this one:

| Layout | For | Against |
|---|---|---|
| **Hash on top, user partitions below (chosen)** | The bucket partition stays the unit of placement, of a move, of the placement and partition-count checks of the scan and fragments, of `kingres.buckets`, `check_cluster` and of every name pattern the extension already knows. A bucket move or a split moves one object per table of the group, however many time partitions it has. Logical replication of a bucket is one table (published through its root), so the number of table-sync workers does not grow with the number of time partitions. PostgreSQL prunes at both levels. | The user's partition is not a PostgreSQL object, so `SELECT * FROM orders_2026_11` fails and `\d+` shows buckets as children. The statements that name a partition (`ATTACH`, `DETACH`, `DROP`) are handled by the hook. |
| User partitions on top, hash below (each user partition is a hash-partitioned table) | The user's partition is a real table on every node; `DROP TABLE orders_2026_11` is one statement per node. | A bucket is spread over every user partition, so a bucket move must publish, create, attach and detach one table per user partition (hundreds), a table-sync worker each; a user partition created while a move runs is a new table inside the move; the partition-count check of the scan and of fragments (`rel->nparts`) no longer counts buckets; the placement of a bucket is no longer the property of one partition. Citus builds the layout from the other side for its shards, but its shards are separate catalog objects; this design has one catalog row per bucket and no row per leaf. |
| One distributed table per user partition (Citus's model: every partition is a distributed table colocated with the parent) | Fits "a partition is a table" completely. | A catalog row, a colocation claim and a bucket partition set per time partition: for monthly partitions of 128 buckets, 1536 bucket partitions a year, each with a map row or a shared one that must be kept equal; a move moves them all. It is the first layout again with more objects. |

The cost of the chosen layout is stated in the documentation: a user partition is addressed through its parent; to read one partition's rows, query the parent with the key condition (the planner prunes the leaves). **[Recommendation]** No compatibility views named after the partitions: they would be objects the hook has to propagate, rename and drop with the partition, and they hide that the partition is not a table.

**What a bucket move moves.** One bucket partition of every table of the colocation group, as today, and each bucket partition of a user-partitioned table is a tree: the move's standalone copy on the target is created as a partitioned table with the same key and with one leaf per user partition present on the source (the bounds are read from `pg_partition_tree` and `pg_get_expr(relpartbound)`), the `CHECK (satisfies_hash_partition(...))` of phase 4 is added to the root and is inherited by the leaves (so `ATTACH PARTITION` finds the partition constraint implied and does not scan), the publication of the bucket's root is created with `publish_via_partition_root = true`, and the attach and detach at the switch are those of the root. The cost of a move is proportional to the data of the tree, not to the number of leaves; the number of leaves bounds the creation of the copy. DDL on the group is refused while a move runs, as today, and that includes creating a user partition: a time-partitioning policy (phase 12) retries a creation that was refused by a move, and creates partitions ahead so that a refusal has time to be retried (see "What phase 12 can rely on"). **[Needs verification]** that `publish_via_partition_root` publication of a partitioned root works with phase 4's marker table, row filter and `pg_subscription_rel` checks, and that the replica identity of the root (a primary key) is what the publisher uses.

### Turning a partitioned table into a distributed one

`create_distributed_table` (and `_concurrently`) accepts a table with `relkind = 'p'` that is partitioned by RANGE or LIST, with **one level** of user partitions (a partition that is itself partitioned is a blocker `subpartitioned`), whose partitions are ordinary tables (no foreign table, blocker `foreign_partition`), with at most `kingres_shard.max_partition_leaves` leaves in the result (bucket count times partitions; the report states the number). Further conditions, each a report code:

- the unique and primary keys contain the distribution column **and** the columns of the user partition key (PostgreSQL requires the second already; the first is the rule of this extension);
- no partition has an index, check constraint, default or storage parameter of its own that its siblings and the parent do not have (`partition_local_object`): the leaves are created from the parent's definition and the parent's inherited objects, so a local object would be lost; a difference in tablespace is the `tablespace` blocker;
- a DEFAULT partition is accepted, with the warning `default_partition`: attaching a new partition scans the default one for rows that belong to the new bound, and with 128 buckets that is 128 scans on each node (**[Needs verification]** that attaching a partition while a default partition exists takes `ACCESS EXCLUSIVE` on the default partition and scans it; the PostgreSQL manual says so for `ATTACH`, and it is why the creation of a leaf uses `ATTACH` with a `CHECK`, not `PARTITION OF`);
- the distribution column may equal the partition key (a table distributed by its time column): accepted with the warning `distribution_column_is_partition_key` (rows of one time interval hash to buckets, and the hash spreads them, so it works; it does not colocate a tenant's rows).

The shadow of such a table is the hash parent, the bucket partitions with `PARTITION BY` of the source's key, and the leaves with the source partitions' bounds, created on the node that owns each bucket. The copy reads **each source user partition** in key order (for the partition, `WHERE key > last`) and sends rows to their owners; PostgreSQL's tuple routing at the owner puts a row in the leaf that its bucket partition's bounds select. A row that fits no leaf cannot exist (the source had no such row) and would fail loudly. The capture trigger is on the source parent and, by PostgreSQL's recursion, on every source partition, so a write directly into a source partition is captured (S4-03). The swap renames the source parent and the source partitions to retained names and renames the shadow; the source partitions are not reused.

### Creating, attaching, detaching and dropping user partitions

These are DDL on a distributed table and follow the rules of the README: they run on the map group primary (any other node refuses them with the hint that names it), reach every node in one two-phase transaction under the cluster lock, are refused while a bucket of the group is moving or an operation claims it, and are refused when the table has more than one group and the statement is not one of the forms below. Each has a function and the corresponding PostgreSQL statement, which the ProcessUtility hook turns into a call of the function (the statement's text is not shipped; the function builds its own statements from validated pieces).

| User statement | Function | What happens |
|---|---|---|
| `CREATE TABLE s PARTITION OF p FOR VALUES ...` (also `DEFAULT`) | `kingres.create_partition(p, 's', '<bound>')` | For every node and every bucket it owns: `CREATE TABLE <leaf> (LIKE <bucket partition> INCLUDING ALL)` as a standalone table, `ALTER TABLE <leaf> ADD CONSTRAINT ... CHECK (<bound as an expression>)` (**[Needs verification]** that PostgreSQL recognizes a constraint equal to the bound as implying it; the manual documents the recipe for range partitions), then `ALTER TABLE <bucket partition> ATTACH PARTITION <leaf> FOR VALUES ...` and drop the check. `ATTACH` takes `SHARE UPDATE EXCLUSIVE` on the bucket partition (readers and writers of the bucket go on), whereas `CREATE TABLE ... PARTITION OF` takes `ACCESS EXCLUSIVE` on the parent (`DefineRelation`), which would stop the bucket for the length of a two-phase commit; the extension prefers the first. A row is added to `kingres.dist_partition`. A sub-partitioning clause, storage parameters and a tablespace in the statement are refused; `USING <access method>` is accepted only for `heap` until phase 12. |
| `ALTER TABLE p ATTACH PARTITION t FOR VALUES ...` where `t` is a distributed table of the colocation group of `p` (same distribution column, bucket count and group) | `kingres.attach_partition(p, t, '<bound>')` | Metadata only: for each bucket, `t`'s bucket partition `t_bNNNN` is detached from `t` and attached to `p`'s bucket partition as the leaf `t_bNNNN` (its name is already the leaf name); the old parent `t` is dropped and its catalog row removed; `t` becomes a logical partition of `p`. Before the attach, a validated check constraint equal to the bound is added to each leaf (`NOT VALID`, then `VALIDATE`, which takes `SHARE UPDATE EXCLUSIVE` and lets writers go on) so that `ATTACH` does not scan under its lock; the cost of the validation is one scan of the leaf. Leaves must have the same columns, in the same order and types, as `p` (the attach check of PostgreSQL), and indexes that match `p`'s are reused, others are built. |
| the same, where `t` is an ordinary table on the map group primary | `kingres.attach_partition(p, t, '<bound>')` | Not by `ALTER TABLE`: the statement is refused with the hint "use `SELECT kingres.attach_partition(...)`", because the table's rows have to be copied and the table consumed (as `create_distributed_table` consumes its table), which is not what `ATTACH PARTITION` means on one node. The function creates the leaves empty, copies the rows through the router in the caller's transaction (blocking; writers of `t` wait; a bound check is applied by the leaf's own partition constraint), compares the counts, drops `t`, and records the partition. |
| `ALTER TABLE p DETACH PARTITION s` (without `CONCURRENTLY`; with it, refused) | `kingres.detach_partition(p, 's')` | For every node: create the hash parent `s` as `CREATE TABLE s (LIKE p INCLUDING ALL) PARTITION BY HASH (<dist column>)` (the key columns, indexes and constraints, owner and privileges of `p`; foreign keys of `p` are re-added as `NOT VALID` and validated), detach each leaf `s_bNNNN` from `p`'s bucket partition (`ACCESS EXCLUSIVE` on the bucket partition for the length of the transaction, so a bucket is unavailable on its node only for that time) and attach it to `s` as `FOR VALUES WITH (MODULUS m, REMAINDER b)`, where `m` is the bucket's modulus in `bucket_map` (a `CHECK (satisfies_hash_partition(...))` is put on the leaf before the detach so the attach does not scan). The catalog gets `s` as a distributed table of the same colocation group (a new `dist_table` row) and the `dist_partition` row is removed. The data does not move. |
| `DROP TABLE s` where `s` is a logical partition | `kingres.drop_partition(p, 's')` | Drops the leaves on every node and the row. Refused when `s` is referenced by a foreign key (it cannot be: keys to a partition are not allowed by PostgreSQL). |
| `TRUNCATE s` | the same hook path as `TRUNCATE` of a distributed table | Truncates the leaves. |
| `ALTER TABLE s ...`, `CREATE INDEX ... ON s`, `GRANT ... ON s` for a logical partition | refused | "`s` is a partition of the distributed table `p`; run the statement on `p`". The statement on `p` reaches the leaves by PostgreSQL's own inheritance of indexes, constraints and privileges where they apply. |

`CREATE INDEX` and `ALTER TABLE ... ADD CONSTRAINT` on `p` are propagated as today; PostgreSQL creates the matching index on every bucket partition and, below it, on every leaf (a partitioned index), including leaves created later (which get the parent's indexes at attach: `LIKE ... INCLUDING ALL` creates them and `ATTACH` matches them). A unique index on `p` must contain the distribution column and the user partition key, and the error of PostgreSQL for a missing partition key column ("unique constraint on partitioned table must include all partitioning columns") is turned by the hook into a message that names both.

**Locks and what is blocked.** Creating a leaf: `SHARE UPDATE EXCLUSIVE` on each bucket partition for the length of the transaction, so a concurrent `ATTACH` or `VACUUM` of that bucket waits and reads and writes do not; the `LIKE` copy and the `CHECK` are on the new standalone table, which nobody else can see. Detaching: `ACCESS EXCLUSIVE` on each bucket partition of the table for the length of the transaction (the two-phase commit). A partition operation takes the cluster lock on the catalog for its duration, which blocks other catalog changes (not data statements). With `lock_timeout` set by the control plane to 5 s, an operation that cannot get its locks fails and is retried by the caller; it never leaves a half-created partition because the transaction rolls back on every node.

**Crash and lost node.** One two-phase transaction: before it commits nothing is visible; after it, everything is. A node that is lost during the transaction has the prepared transaction resolved by recovery (phase 2a). `check_cluster()` reports `dist_partition_mismatch` if a leaf is missing on the owner of its bucket (a leaf that was attached by hand outside the extension, or a failure of recovery). A bucket that moves later takes its leaves with it, and the target creates its tree from the source's leaves, so a node never needs to know the list of partitions from the catalog.

**Interaction.** With a move in progress: refused (see above). With the map version: every partition operation raises it on every node, so a statement planned earlier meets `KS001` and is retried. With reference tables: foreign keys from a user-partitioned distributed table to a reference table are carried by the parent as for any distributed table. With ledger tables: refused as a source, and a ledger table cannot be partitioned. With standbys: physical replication carries the leaves; nothing else. With the scan: the planner of every node prunes by the user's key at the second level exactly as for a partitioned table on one node, and a remote fragment names the parent.

**Reference tables that are partitioned** are not supported (`create_reference_table` refuses them as today). **[Recommendation]** keep it: a partitioned reference table needs every node to run every partition operation as a reference-table write, and the case is rare.

### What phase 12's time-partitioning tools can rely on

[analytics-phase12-storage.md](analytics-phase12-storage.md) adds creation of partitions ahead of time, retention (dropping old ones) and storage format changes of old partitions. It can rely on:

1. **The catalog and the names.** `kingres.dist_partition(parent, partition_name, bound)` lists the logical partitions; `kingres.dist_table.partition_method` and `partition_key` say how the table is partitioned; leaf names are `<partition_name>_b<NNNN>` inside `<table>_b<NNNN>`; `kingres.partition_leaves(parent, partition)` lists `(node_id, bucket, leaf)` for the owner nodes. These are stable contract of this phase; a column may be added, none removed or renamed.
2. **The four operations as functions with fixed semantics**: `create_partition(parent, name, bound, if_not_exists)`, `drop_partition(parent, name, missing_ok)`, `detach_partition(parent, name)`, `attach_partition(parent, partition, bound)`, each atomic across the cluster, idempotent when `if_not_exists` or `missing_ok` says so, and each refusing (error `55006`-class "object in use", not a hang) when a move or operation holds the group, so that a policy runner can retry with backoff.
3. **Identical definitions.** All leaves of one logical partition have the same columns, constraints and indexes, inherited from the parent; the only difference phase 12 may introduce per leaf is the storage (access method) of **old** partitions, through a per-leaf operation that it implements with `partition_leaves` and that must be repeatable after a bucket move (a move copies the leaf with its storage by `LIKE`, and **the storage format of a leaf is not carried by `LIKE ... INCLUDING ALL`**, so phase 12 must either change the move's copy to recreate the access method or convert after a move; this is a requirement on phase 12's storage design, and the move code of increment 9.5 passes the access method of each leaf explicitly).
4. **A bound is a string** accepted by PostgreSQL's parser in the `FOR VALUES` position; the function does not interpret time. Computing "the next month" is the policy's job. `kingres.partitions` shows each partition's size on the local node for the policy to decide on.
5. **Partition creation is refused during a move or an operation**; a runner must create partitions with enough lead that a refusal can be retried before the data arrives, and rows that fit no partition fail with PostgreSQL's "no partition of relation found for row" (or go to a DEFAULT partition if the user made one).
6. **The number of leaves is bounded** by `kingres_shard.max_partition_leaves`, and the report tells how many leaves one more partition adds (the bucket count).
7. **Drop is cheap and exact**: `drop_partition` removes `bucket count / groups` tables on each node in one transaction; it does not touch the other partitions' data and is what retention should call.

## Security

- **Who may call what.** Every function of this phase is revoked from `PUBLIC` and, like the administration functions of 0.2, executable by superusers (or a role they grant execute to). The control plane calls them as its service role after checking the cluster role of the caller: `kingres_admin` for every action that changes a table, `kingres_readonly` and above for reading the report of a table (through the contract's read operations only; `previewTableChange` is an administrator's operation because it names blockers that may expose object names a read-only role cannot see). Nothing here lets a role do in SQL what it could not before: a role without superuser cannot call any of the functions, and a role that is not an administrator of the panel cannot start a task.
- **The caller's rights on the table.** The panel's administrators act as the service role on tables they may not own. The result keeps the table's owner, its table and column privileges, its comments and its policies of ownership (`OWNER TO`, `GRANT`/`REVOKE` from `relacl` and `attacl`), so a rebuild does not give anyone a privilege nor take one away, and the old copy keeps the old owner's rights for as long as it exists (a rename does not change privileges, so it stays readable by whoever could read the table, under a name that carries the operation id). **[Recommendation]** the retained copy is moved to the schema `kingres` (which only the owners of the extension use) and its privileges revoked from everyone but its owner, in the swap, as phase 4 does for orphans.
- **The capture function.** `kingres.internal_capture_trigger()` runs in the session of every writer of the source, so it needs to insert into a log table that the writer has no privilege on. It is `SECURITY DEFINER`, owned by the extension owner, with `SET search_path = pg_catalog, kingres, pg_temp`, takes the log table's name from the trigger's argument (a name the extension made) and **only inserts key columns of the row being changed**; it reads nothing else, has no dynamic SQL other than the insert built with `format('%I')`, and is not callable as a function (it returns `trigger`). It is the second `SECURITY DEFINER` function of the extension (after `forget_dropped_tables`); the `security` regression test is changed to list exactly these two, and a test checks that a role without privileges on the log table cannot read or write it, and that a writer on the source sees no error about it. A role that can write the source can append keys of rows it could write anyway; a forged entry (a key that does not exist) makes the replay delete a destination key for a source row that does not exist, which is correct.
- **Shipped statements.** The copy and replay statements are built from catalog facts with `quote_identifier` and parameters, as the router's insert fragment is; no row value is part of a statement text (2b, 11.3). They run in sessions that prove their origin with the forward token (phase 2a) as the role of the origin, which is the service role. The marker comment and token rules of 2b apply to the `kind=insert` fragment unchanged.
- **Bounds and names.** `create_partition`'s `bound` is parsed with the raw parser inside a fixed `CREATE TABLE` skeleton and accepted only if the parse tree is a single `CreateStmt` with a partition bound and nothing else; the bound is then deparsed from the parse tree, not from the input string, before it is sent to other nodes. Schema, table and partition names are always passed through `quote_identifier`.
- **Secrets and logs.** No credential is stored in a catalog table, a log table, `table_operation.spec` or a task row. Key values in log tables are row data, readable by superusers only (`kingres.tlog_*` have no privileges for anyone else) and dropped at the swap. The task `detail` never contains row values, only counts, names and blockers.
- **Exposure through the view.** `kingres.table_operations` omits `detail` (error text and addresses). The control plane reads the `detail` as the service role and the panel shows it to administrators and operators only.
- **Version handshake and mixed nodes.** `internal_require_version` makes a mixed-version cluster refuse these operations instead of misbehaving (review S4-05).

## Control plane (`kingresd`)

### Tasks and their named steps

A table operation is a task with named steps (the engine of [control-plane-3.md](control-plane-3.md) and the README's "Task engine": a step recorded `done` never runs again; a step that ran without its `done` runs again, so every step is idempotent; progress goes in `TaskStep.itemsDone/itemsTotal` and `detail`). The steps call the phase functions of the extension; the task never holds a database transaction across steps. Subject: `table`, id `database/schema.table` of the table the request named. A table operation claims its colocation group(s) in the extension; a second request for the same table or group answers 409 `operation_in_progress` (an unfinished rebalance task: `rebalance_in_progress`).

**`table.distribute`** (changed; the current task is the single step "create_distributed_table"). Online:

1. *Check the table* (`table_change_report`; blockers fail the task with 422 `table_not_eligible` at the HTTP call, before the task exists, when the check is done in the request; the step repeats the check because the table may have changed).
2. *Check free space* (estimate against each destination server's free space, standbys included; fails with the server named).
3. *Prepare* (`begin_table_operation`, `internal_require_version`).
4. *Create the new layout* (`table_operation_advance(..., 'shadow')`; `itemsTotal` = servers).
5. *Copy rows* (`... 'copying'`; `itemsDone/itemsTotal` = rows copied / estimated rows; `detail` names the server being read).
6. *Catch up with writers* (`... 'catching_up'`; `detail` = backlog).
7. *Switch* (`... 'switching'`; `detail` = attempt and blockers; when the call returns with the attempts used up the step sets the task `paused` with the blockers as `detail`, waits 60 s and calls again; the task is `paused` for as long as the blockers hold).
8. *Verify and clean up* (`... 'cleaning'`; `detail` = verification result; a failed verification fails the task with the code `verification_failed`, not retryable, the old copy kept).

Blocking: *Check the table*, *Check free space*, *Copy rows and switch* (one call of `create_distributed_table`; `detail` says "writers of the table wait during this step"), *Finish* (cleanup is by the worker). Reference: the same steps with `create_reference_table[_concurrently]`.

**`table.undistribute`**: *Check the table*, *Find linked tables* (the foreign key component; fails with the list when `cascadeViaForeignKeys` is false and the component has more than the table), *Check free space on the target server*, then the steps 3 to 8 above (blocking: one step), for the component as one operation. A reference table: *Check the table*, *Compare the copies* , *Remove the table from the other servers* (blocking: one two-phase transaction).

**`table.alter`**: *Check the table*, *Plan* (which tables are rebuilt, the destination group, the placement, how many rows move; for a catalog-only change this step is the last but one), *Check free space*, steps 3 to 8 for the set; or *Change the catalog* for the catalog-only cases.

**`table.partition`** (create, attach, detach, drop a user partition): *Check the request* (`table_change_report`), *Change the partitions on every server* (one call of the function; `itemsTotal` = servers), *Finish*. Quick, but a task for uniformity with decision 4 of the README ("tasks that finish quickly are still tasks").

**Cancel and failure.** `cancelTask` of an online table task before the switch calls `cancel_table_operation` and ends the task `failed` with code `cancelled` (the contract's rule); `Task.cancellable` is true until step 7 has committed and false afterwards (cancelling would not undo the swap). A task that fails before the swap runs `cancel_table_operation` as its compensation, so nothing is left; a task whose compensation could not reach a node is `failed` with a `detail` that names the node and says that the operation will be undone by the recovery worker when the node returns. A task killed with the process is resumed by the engine at the first step not `done`; a database call that fails because a node is down makes the step wait and report the server (control-plane-3, D4), not fail. `NonIdempotent` is not used: every step checks what exists.

**Serialization.** One table task per colocation group; the control plane checks `table_operation_lock` through `begin_table_operation` and maps the extension's error to 409. The rebalance task checks for an unfinished table task of a group before it starts a move of that group and skips the group with `detail`. Phase 10's scheduler must do the same (see "Relationship to bucket splitting").

### REST contract additions (0.7.0, additive)

Phase 10 also adds to the contract and names it 0.7.0; whichever phase is merged first takes 0.7.0 and the other 0.8.0. All additions are optional properties, new operations, new enum values and new problem codes, which the contract's rule ("clients must tolerate codes they do not know") allows. Operations are added under the tag `Tables`.

**Changed: `distributeTable`** (`PUT /databases/{databaseName}/schemas/{schemaName}/tables/{tableName}/distribution`, `kingres_admin`). Description: replace "The existing rows are moved to their buckets" by the two modes. `DistributeTableRequest` gains:

```yaml
bucketCount:      { type: integer, minimum: 1, maximum: 4096,
                    description: "Bucket count of a new colocation group; ignored with colocateWith." }
mode:             { type: string, enum: [auto, online, blocking], default: auto,
                    description: "auto: online when the table has a usable key and more than 100000 estimated rows, otherwise blocking. blocking: writers wait for the whole copy." }
relatedTables:    { type: array, items: { $ref: TableRef },
                    description: "Tables linked by foreign keys that are distributed together (see 'distribute with related tables'); each needs an entry in `columns`." }
dependentViews:   { type: string, enum: [refuse, recreate], default: refuse }
```

Responses: 202 `AcceptedTask` (kind `table.distribute`); 409 `operation_in_progress`, `rebalance_in_progress`; 422 `table_not_eligible` (with `blockers`); 503 `map_group_unavailable`.

**New operations**

| operationId | Method and path | Role | Body / query | Response |
|---|---|---|---|---|
| `previewTableChange` | `POST .../tables/{tableName}/distribution/preview` | `kingres_admin` | `TableChangeRequest` (below) | 200 `TableChangePreview` |
| `undistributeTable` | `DELETE .../tables/{tableName}/distribution` | `kingres_admin` | query `cascadeViaForeignKeys` (bool, default false), `targetGroupId` (int, default 0), `mode` (`auto`, `online`, `blocking`) | 202 `AcceptedTask`; 409 `operation_in_progress`, `rebalance_in_progress`; 422 `table_not_eligible` |
| `alterTableDistribution` | `PATCH .../tables/{tableName}/distribution` | `kingres_admin` | `AlterTableDistributionRequest` | 202 `AcceptedTask`; 409 and 422 as above |
| `listTablePartitions` | `GET .../tables/{tableName}/partitions` | `kingres_readonly` | query `limit`, `cursor` | 200 `TablePartitionPage` |
| `createTablePartition` | `POST .../tables/{tableName}/partitions` | `kingres_admin` | `CreateTablePartitionRequest` | 202 `AcceptedTask`; 409 `already_exists`, `operation_in_progress`; 422 |
| `attachTablePartition` | `POST .../tables/{tableName}/partitions/attach` | `kingres_admin` | `AttachTablePartitionRequest` | 202; 409, 422 |
| `detachTablePartition` | `POST .../tables/{tableName}/partitions/{partitionName}/detach` | `kingres_admin` | `DetachTablePartitionRequest` | 202; 404, 409 |
| `dropTablePartition` | `DELETE .../tables/{tableName}/partitions/{partitionName}` | `kingres_admin` | none | 202; 404, 409 |

(`...` is `/databases/{databaseName}/schemas/{schemaName}`; all list the shared responses 401, 403, 405, 422, 503, and 400 where a body or query exists. `partitionName` is a new path parameter.) That is 8 operations; the contract goes from 88 to 96 and the panel's contract test counts it from the spec.

**New and changed schemas**

```yaml
TableChangeRequest:            # previewTableChange
  required: [kind]
  properties:
    kind: { type: string, enum: [distribute, undistribute, alter, createPartition, attachPartition, detachPartition, dropPartition] }
    distribute:   { $ref: DistributeTableRequest }          # when kind = distribute
    undistribute: { type: object, properties: { cascadeViaForeignKeys: {type: boolean}, targetGroupId: {type: integer} } }
    alter:        { $ref: AlterTableDistributionRequest }
    partition:    { type: object, properties: { name: {type: string}, bound: {type: string}, table: {$ref: TableRef} } }
AlterTableDistributionRequest:
  properties:
    distributionColumn: { type: string }
    bucketCount:        { type: integer, minimum: 1, maximum: 4096 }
    colocateWith:       { oneOf: [ {$ref: TableRef}, {type: string, enum: [default, none]} ] }
    cascadeToColocated: { type: boolean, default: false }
    mode:               { type: string, enum: [auto, online, blocking], default: auto }
  minProperties: 1 (of distributionColumn, bucketCount, colocateWith)
TableChangePreview:
  required: [blockers, warnings, notes, affectedTables, estimate]
  properties:
    blockers:       { type: array, items: { $ref: TableBlocker } }
    warnings:       { type: array, items: { $ref: TableBlocker } }
    notes:          { type: array, items: { $ref: TableBlocker } }
    affectedTables: { type: array, items: { $ref: TableChangeTable } }   # schema, name, sizeBytes, estimatedRows, role: source|colocated|linked
    estimate:       { rows: int64|null, bytes: int64|null, rowsThatMove: int64|null, modeRecommended: online|blocking,
                      onlineAvailable: boolean, onlineUnavailableReason: string|null, writersBlocked: none|switchOnly|wholeCopy }
TableBlocker:  { required: [code, object, message], code: string, object: string, message: string }
TablePartition: { required: [name, method, bound, isDefault, leafCount, sizeBytes],
                  name, method: range|list, bound: string, isDefault: boolean, leafCount: integer, sizeBytes: int64|null }
Table (additive, optional): partitioning: { method: range|list, key: string, partitionCount: integer } | null
                            operation: { taskId: int64, kind: string, phase: string } | null   # the unfinished table task on it, if any
TableColumn (additive): nothing.
TaskKind (additive): table.undistribute, table.alter, table.partition
Problem (additive, optional): blockers: array of TableBlocker   # present for table_not_eligible
ProblemCode (additive, "Added in 0.7.0"): table_not_eligible (422)
```

`getTable` gets `partitioning` and `operation` through `Table`. The per-column `distributionBlockedReason` values are unchanged; the new reasons are carried by the `blockers` of the preview. `TableClass` is unchanged: a user-partitioned distributed table is `distributed`; `partitioning` says it is also partitioned. `TaskSubject.type` `table` already exists. The mapping of `kind` to subject in the control plane README ("Task kinds and subjects") gets three rows. `table_not_eligible` is 422 because the request is well formed and the table cannot take it; `operation_in_progress` and `rebalance_in_progress` already exist and cover the busy case; no other new code is needed.

## Web console

**Where.** The tables list of the database page (`DatabaseDetailPage.vue`) keeps its columns and gets a progress indicator on a row whose table has an unfinished operation (`operation` of the `Table`), and its row actions depend on the class:

| Class | Actions (administrators) |
|---|---|
| plain | Distribute table (existing, extended), Enable ledger (existing) |
| distributed | Change distribution, Undistribute |
| reference | Undistribute |
| distributed and partitioned | the above, and a link "Partitions" |

A new route `/databases/:db/tables/:schema/:table` (page `TablePage.vue`, `meta.operation = getTable`) shows the table: summary (class, distribution column, colocation group and its other tables, bucket count, size, rows), the operation in progress if any, and a tab **Partitions** for a partitioned distributed table (list from `listTablePartitions`; actions Create partition, Attach table, Detach, Drop). Phase 12 adds a tab "Time partitioning" to this page; nothing here assumes it. The Shards page shows, in the row of a colocation group, a badge "table operation" while a claim exists (from the table's `operation`).

**Dialogs** (all are `TaskActionDialog`s, as the existing distribute dialog; each fetches `previewTableChange` when it opens, shows a spinner and then the preview, and its confirm button is disabled while there is a blocker):

- *Distribute table* (changed): method (hash, reference); distribution column (the existing list from `getTable`, with the reason for columns that cannot be used; for a partitioned table the key columns the unique keys need are named); colocate with (the existing list); bucket count (collapsed "Advanced", disabled when colocating); **mode** with three radio choices, "Online (writers keep working; recommended for tables over 100,000 rows)", "Blocking (writers wait while the rows are copied; simpler)", and "Choose for me" (default), with the preview's `writersBlocked` text and, when the table has no usable key, the reason online is not available; a list "What will be carried" (foreign keys to reference tables, indexes, privileges, comments) and "What stops this" (the blockers, each with its object and message, including `foreign_key_plain_table` with the hint). The confirmation text says that the table is replaced by a new table with the same name and that the old one is kept for ten minutes.
- *Change distribution*: three sections that can be changed together (distribution column, bucket count, colocation: keep, new group, join the group of another table), a checkbox "Apply to the N colocated tables" (listing them with sizes; unchecked and impossible when a foreign key depends on the colocation, with the key named), mode, and the preview: tables rebuilt, rows that move, whether the change is catalog-only ("nothing is copied") and the free space check outcome per server. Type-to-confirm of the table name when data moves.
- *Undistribute*: target server (a select of servers with free space shown; default the map group's), a checkbox "Also undistribute the N linked tables" with the list from the preview (required when the table is linked, the preview says why), mode, and a warning in `tone-danger`: "The table will live on one server. Its data must fit there." Type-to-confirm.
- *Create partition*: name, bound (range: from and to; list: values; default), with the example for the table's key type; the preview states how many leaves it adds and whether the table is near `max_partition_leaves`. *Attach table*: choose a distributed table of the group or an ordinary table (with the warning that an ordinary table is consumed), bound. *Detach*: confirmation that the partition becomes a distributed table of its own. *Drop*: type-to-confirm the partition name, with its size.

**Progress and failure.** The dialog follows the task through the existing `TaskProgress` component: the named steps, the percentage of the copy, the `detail` of the running step (the backlog, the attempt and the blockers: "waiting for a transaction that holds the table: pid 4123, role app, running for 3 min"). A task in the state `paused` is shown with the blockers and a "Cancel" button (`cancelTask`, `kingres_ops` and above). A failed task shows the step, the message, whether it is retryable, and for `verification_failed` "The new table differs from the old copy by N rows. The old copy has been kept as `<name>`; contact an administrator" with the table named; for a failed check it shows the blockers list again. After a failure of an online operation, nothing is left behind and the dialog says so, or says which server will be cleaned when it returns. Accessibility and hostile names follow the existing rules (`SafeName` for every name from the database, `LiveRegions` for progress, no polling in a hidden tab, `?mock=hostile` data in the new tests).

**What a read-only user sees.** The tables list and the table page with all data, the partitions list, and the progress of an operation; the action buttons are absent or disabled with the reason, as `GateButton` does today for roles below the operation's `x-required-role` (the contract's role table is the source: `getTable`, `listTablePartitions` need `kingres_readonly`; every action needs `kingres_admin`; cancelling a task needs `kingres_ops`). A user with `kingres_stats` only has no Tables page, as today.

**Mock API.** `src/mocks/state.ts` gets operations on the mock cluster: a table operation takes a few seconds with the steps above, can be made to fail with the table name `failme`, to wait with blockers with `blockme`, and to fail verification with `diffme`; a partitioned table `events` exists in the `shop` database, with partitions; the mock handlers cover the eight new operations (the contract test fails otherwise) and the new problem code.

## Tests

### pg_regress (one node)

- `catalog`: the new columns and tables, and that `kingres.table_operation` and `tlog_*` are unreadable by `PUBLIC`.
- `distribute_rows`: the report for a table with each blocker and warning of the table in "Restrictions" (one statement per code, the error text of each blocker in one message); `create_distributed_table` on a table with rows on a one-node cluster (the copy, the counts, owner, privileges, comments, identity and sequence values, foreign keys to a reference table, unique and non-unique indexes), and that a failure at each step rolls back (fault points).
- `partitions`: `create_partition`, `attach_partition` of a distributed table, `detach_partition`, `drop_partition` and the PostgreSQL statements that map to them, on a one-node table; the bound parser (a bound with a semicolon, a comment, a second clause, a quote); unique keys that miss a key column; `kingres.partitions`.
- `security`: every function of the phase is revoked from `PUBLIC`; `internal_capture_trigger` is one of exactly two `SECURITY DEFINER` functions; every function pins `search_path`; a role without privileges on the log table writes the source without error.
- `upgrade`: 0.5 to 0.6 equals a fresh 0.6, including a table distributed before the upgrade and the colocation id sequence.
- `undistribute_ref`: undistribute of a reference table on one node (catalog only).

### TAP (several nodes), with fault points and interleavings

Fault points follow the naming of phase 4: `rebuild_after_preparing`, `rebuild_after_shadow`, `rebuild_after_capture`, `rebuild_copy_chunk`, `rebuild_after_copy`, `rebuild_replay_chunk`, `rebuild_before_switch`, `rebuild_switch_after_lock`, `rebuild_switch_after_final_replay`, `rebuild_switch_after_prepare`, `rebuild_switch_after_commit_map`, `rebuild_after_switch`, `rebuild_verify`, `rebuild_cleanup`, `rebuild_undo_after_triggers`, `rebuild_undo_after_shadows`, and `partition_after_leaves`, `partition_before_attach`, `partition_after_attach`. The `:kill`, `:error` and `:wait:<file>` forms and scopes of `fault.c` apply.

- `t/040_distribute_rows.pl`: three nodes; a table with rows, blocking and online; the rows equal a single-node model by the comparison harness (`KingresShardCompare.pm`) for counts, checksums and a list of queries, from every node; the map version rose on every node; owner, privileges, comments and sequences carried.
- `t/041_distribute_online_load.pl`: writers on all nodes' sessions on the source (inserts, updates, deletes, `COPY`) run throughout; afterwards the table equals the model, no statement failed except those the test expects and retries; the longest statement of the writers and the longest of readers are recorded and compared with the design targets ("What is blocked").
- `t/042_distribute_interleavings.pl`: forced interleavings with the fault points: a row inserted, updated or deleted between the copy's read and its insert; between the final replay and the swap (impossible while the lock holds: the writer must wait and the test shows it waits); a writer into a source partition directly during the switch (S4-03); a prepared user transaction holding the source; `session_replication_role = replica` in a writer (captured, because the trigger is `ENABLE ALWAYS`); a `TRUNCATE` (refused); a long reader (it delays the swap, the attempt gives up and the blockers appear in `detail`).
- `t/043_rebuild_faults.pl`: a crash of the map group primary, of a source node, of a destination node, a lost connection, at every fault point; after `resume_table_operations()` the table is whole in exactly one layout, nothing is left (`check_cluster()` clean), and a crash after the commit on the map group and before commit on the others is resolved by recovery. **The recovery worker, not a manual call, does the resuming in a variant of the test (S4-02).**
- `t/044_undistribute.pl`: a distributed table with rows to an ordinary table on each target group; reference tables; with and without `cascade_via_foreign_keys`, a component of three tables with keys between them and to a reference table; the refusals; the keys recreated and validated; views refused and named.
- `t/045_alter.pl`: distribution column change (the rows are in the right buckets from every node), bucket count 128 to 256 (no network bytes between nodes: the counters of the node connections show none), 128 to 100, colocation to `none` and back to a group with equal placement (catalog-only: no table is read), cascade to colocated with a foreign key between two tables, a group with split buckets (using a hand-built mixed-modulus layout when phase 10 is not merged) rebuilt to a uniform one.
- `t/046_partitioned.pl`: a table partitioned by range and one by list with rows in several partitions becomes distributed (blocking and online); every query shape of `t/013` runs against it through the scan and, when 2b is present, pushdown, compared with a single-node model; user partitions created, attached (a distributed table, an ordinary table), detached, dropped from each node (the non-map nodes refuse with the hint); a bucket move of a partitioned bucket under load, then a partition created after it; a rebuild of the partitioned table; `check_cluster()` clean at every point.
- `t/047_operation_exclusion.pl`: a move during an operation, an operation during a move, a split during an operation (when phase 10 is present), DDL during an operation: each refused with the right message, and the claim released after failure and after cancel; a scan that was planned before a swap fails with `KS001` and works when repeated; a prepared statement is re-planned.
- `t/048_failover_during_operation.pl` (needs phase 5's standbys): promotion of a destination's standby during `copying`, of a source's standby during `catching_up`, of the map group's standby during `switching`.
- `t/049_verification.pl`: a table changed behind the trigger's back (trigger disabled by a superuser editing the catalog) is detected by the digest and the old copy kept; a `money` column; `rebuild_verify = off`.
- `upgrade` TAP: a rolling upgrade where one node still has 0.5: every function of the phase refuses and names the node, and old paths keep working.

Every test that compares answers also runs the table through the scan with all faster paths switched off (ADR 0022 rule 3) and compares with the faster path when it exists.

### Control plane (Go)

- Unit tests of the request validation (`PrepareDistribute` and the new `Prepare*` functions): every field, the blockers mapped to 422 with `blockers`, the mode `auto` decision, the free-space check with fake server resources.
- Task-engine tests: each task kind killed at each step boundary with the crash points of `task.FaultPoints`, resumed by a second process, ending in one table (`TestTableTaskInterruptedAtEveryStep`, in the style of `TestJoinInterruptedAtEveryStepResumes`); a `paused` task that resumes when the blocker goes; cancel before and after the switch; a verification failure.
- Integration tests in WSL with three `kingresd` processes (as control-plane-3): distribute, alter and undistribute from the API on a table with rows and a write load; the same with a node stopped (the task waits and names it); the audit entries; the roles (`kingres_readonly` cannot, `kingres_ops` cannot start, can cancel).
- Contract tests: `openapi.yaml` validates; the new operations have roles, responses and examples; the README table of task kinds and subjects has the three rows (`readme_table_test.go`).

### Panel (Vitest and the mock API)

Following `kingres/web/README.md`: `tests/table-lifecycle.test.ts` (distribute dialog: blockers disable the button, the mode choice and its text, the related-tables list; change-distribution and undistribute dialogs with cascade and type-to-confirm; progress, paused with blockers, cancel, failure and verification-failure states); `tests/table-partitions.test.ts` (partitions list, create/attach/detach/drop, bound validation, hostile partition names); `tests/roles.test.ts` extended (actions hidden or disabled with a reason for `kingres_readonly` and `kingres_ops`); `tests/pages-a11y.test.ts` gets the table page; `tests/hostile.test.ts` gets the new dialogs; `tests/contract.test.ts` counts the 96 operations; `tests/contract-0-7.test.ts` (new problem code and enum values, optional properties tolerated when absent). `npm run test:browser` covers the layout of the new dialogs at 320 px and the content security policy for the new page. The end-to-end run (`npm run e2e`) gets a distribute-with-rows step and an undistribute step against the real server.

## Implementation order

Each increment can be committed and tested alone; the extension is changed by one line of work at a time (architecture.md), so these are in the order they are merged.

| # | Increment | Gate |
|---|---|---|
| 9.1 | Catalog and plumbing: the 0.6 install and upgrade scripts (columns, tables, sequence, views, the version handshake), `table_change_report` with all codes, the exclusion check in `internal_begin_move` and `kingres_shard_assert_no_move`, `internal_table_ddl` and the partition creation functions reading each bucket's modulus, `colocation_id_seq`. No behaviour change yet except the new report. | pg_regress `catalog`, `upgrade`, `security`, `distribute_rows` (report part) |
| 9.2 | The batch sender with an explicit destination (`internal_copy_routed`) and the blocking `create_distributed_table` with rows, owner/privilege/comment/sequence carry, foreign keys to reference and colocated tables, `create_reference_table` foreign key rule. | `t/040` blocking, pg_regress |
| 9.3 | The rebuild engine: operation record, shadow, capture, copy, replay, switch, cleaning, verification, resume, cancel, check_cluster rows; `create_distributed_table_concurrently` and the reference variant on top. | `t/040`-`t/043`, `t/047` |
| 9.4 | Undistribute (blocking and online, reference, cascade) and alter (catalog-only cases, then blocking, then online, cascade to colocated). | `t/044`, `t/045` |
| 9.5 | User-partitioned tables: the layout, `create_distributed_table` of a partitioned table, `dist_partition`, the four partition functions and hook mapping, `kingres.partitions`, `partition_leaves`, the changes to phase 4's move code (tree copy, publish through the root, leaf access method, sizes summed over `pg_partition_tree`). | `t/046`, a move test of a partitioned bucket |
| 9.6 | Control plane: `table.distribute` revised, `table.undistribute`, `table.alter`, `table.partition`, free-space check, `paused` handling, cancel; contract 0.7.0 (spec first, then code). | Go unit, task-crash and integration tests, contract tests |
| 9.7 | Web console: the dialogs, the table page, progress and failure states, mock API; `distribute with related tables` in the control plane and its dialog. | Vitest, browser tests, `e2e` |
| 9.8 | `dependent_views => 'recreate'` (plain views). | pg_regress and TAP with views and grants |
| 9.9 | Storage parameters carried (shadow leaves, `dist_table.reloptions`, copy in the move). | pg_regress, move test |
| 9.10 | README of the extension, the control plane README table of task kinds, the web README, worklog entry; the implementation notes and deviations of this document. | documentation review |

Increments 9.1 to 9.4 do not need the control plane; 9.6 needs 9.3 and 9.4; 9.5 can be merged before or after 9.4.

## Open questions

Each has a recommendation; the project owner is not asked.

1. **Is the online variant default?** **[Recommendation]** `mode = auto`: online when the table has a usable key and more than 100,000 estimated rows, blocking otherwise, and the dialog shows the choice. The threshold is a control plane setting.
2. **Retention of the old copy.** **[Recommendation]** 10 minutes (`table_cleanup_delay`), verification before the drop; `0` is allowed for operators short of disk. A longer default would double disk use for hours.
3. **Which node copies a plain source?** **[Recommendation]** the map group primary only; moving a plain table to the map group is the user's step (a `pg_dump` or `INSERT ... SELECT` from the node that holds it). The report says where the table is.
4. **Should an online operation be offered for a reference table's undistribute?** **[Recommendation]** no (above).
5. **Compatibility views named after partitions?** **[Recommendation]** no (above).
6. **Per-table distribution columns when a colocated set changes its column?** **[Recommendation]** not in phase 9; the set form needs the same column name and type in every table.
7. **What happens to a distributed table's sequences when a node is added later?** Unchanged from 2a (node-local); phase 9 does not make it worse. **[Recommendation]** a later phase gives sequences per-node ranges.
8. **Should `undistribute` refuse tables bigger than the target's free space automatically?** **[Recommendation]** the control plane refuses when its estimate says so (it knows free space); the extension does not.
9. **Which table operation wins a conflict with a rebalance?** **[Recommendation]** whichever claimed the group first; a rebalance that finds a claimed group skips its moves and records why, a table operation never waits for a move, it is refused.
10. **A time-partitioned table with more than 20000 leaves.** **[Recommendation]** refuse at `max_partition_leaves` (the default is a design target that the tests of 9.5 measure; planning cost of PostgreSQL grows with the partition count), and tell the user to lower the bucket count or the number of partitions kept.
11. **Distribute with related tables as a function of the extension?** **[Recommendation]** no, a control-plane task (above).
12. **Logical replication for the online variant when `wal_level = logical` is on?** **[Recommendation]** no: one mechanism, and the trigger log works with any `wal_level`.

## Known limits after this phase

- A table is replaced by a new table with a new OID. Views, materialized views, rules, publications, policies and functions that depend on the table's row type block the operation (plain views can be recreated from increment 9.8). Statistics objects are not carried.
- User triggers, row-level security, exclusion constraints, inheritance, ledger tables and foreign keys to or from ordinary tables stay refused. A set of ordinary tables linked by foreign keys is distributed by the control plane's dance, which leaves the keys unenforced for the length of the copies.
- The online variant needs a primary key or a unique index on `NOT NULL` columns, refuses `TRUNCATE` and DDL on the table while it runs, and costs the writers one extra insert per row change; its guarantee does not cover a change made with the capture trigger disabled by someone who edits the catalog (the digest check finds it).
- A switch blocks writers of the source tables (on a distributed source, on every group) and, during the rename, readers, for one attempt; a long transaction that holds the table delays the switch, and an operation can stay `paused` for as long as one does.
- The disk use of an online operation is about twice the table plus indexes (three times for a retained old copy during cleaning), on the destination servers and their standbys.
- Sequences are node-local, as before.
- Changing the bucket count or the distribution column moves every row; a change to a multiple of `max_modulus` moves no row between nodes, any other moves most of them.
- While an operation, a move or a split claims a colocation group, DDL on its tables, user-partition operations on them and other operations on the group are refused.
- A distributed partitioned table has hash on top and the user's partitions below; the partition is addressed through its parent; one level of user partitioning only; no hash partitioning by the user; no partitioned reference tables; phase 10 does not split a bucket of such a table (use `alter_distributed_table`); the number of leaves is bounded by `max_partition_leaves`.
- Moving a bucket of a partitioned table copies the tree, and creating a leaf is refused while a move runs.
- The verification digest compares counts and an order-independent hash of row text; it is a check against mistakes, not against an adversary.

## Needs verification

1. A `NOT VALID` foreign key can be added to a partitioned referencing table and later validated on PostgreSQL 18.7 (the old restriction message is not in `tablecmds.c`; no test run).
2. A row trigger on a partitioned table is cloned to a partition attached later (`CloneRowTriggersToPartition` at attach), so a partition attached during an operation is captured. Creation on existing partitions is checked (`CreateTriggerFiringOn`).
3. `ALTER TABLE ... ATTACH PARTITION` finds a validated check constraint equal to the bound as implying the partition constraint (range, list, and hash with `satisfies_hash_partition` on the root of a partitioned child), and does not scan.
4. Attaching a partition when a DEFAULT partition exists takes `ACCESS EXCLUSIVE` on the default partition and scans it.
5. `LIKE ... INCLUDING ALL` does not copy table storage parameters (`INCLUDING STORAGE` is the column-level one) and does not copy the access method.
6. Publishing a partitioned root with `publish_via_partition_root = true` works with phase 4's marker, row filter and ready-state checks, with the primary key of the root as replica identity, and the initial copy reads the whole tree (the publisher side is checked in `tablesync.c`; the subscriber side applies into a partitioned target through tuple routing and is not).
7. The text form of a whole row is equal on two nodes for every column type under the transmission modes (and `lc_monetary` = `C`), so the digest compares equal data as equal.
8. That the copy of this phase needs nothing registered in the catalog cache: its destination is explicit (the design does not rely on a table registered in the same transaction being visible to the planner hook).
9. Whether the hook that refuses a foreign key from an ordinary table to a distributed table also refuses one to a **reference** table (the design keeps a key from the new ordinary table to a reference table after undistribute: the README states the rule only for distributed tables).
10. Dropping a retained old partitioned table (not in `kingres.dist_table`) does not touch the catalog through `forget_dropped_tables`, as phase 4's notes describe for orphans.
11. The contract text of `distributeTable` ("the table can be a ledger table at the same time") against `ledger_enable`'s refusal of partitioned tables and `create_distributed_table`'s refusal of triggers.
12. The measured lock times and the stall figures of "What is blocked" (design targets, no measurement yet).
13. That the lock of `ATTACH PARTITION` on the attached table is `ACCESS EXCLUSIVE` (the parent's mode, `SHARE UPDATE EXCLUSIVE`, is read in `AlterTableGetLockLevel`; the attached table's is not read).
14. Whether a cached plan on a renamed table re-resolves the name (the plan cache re-analyzes the raw parse tree on invalidation; read in general, not traced in 18.7 source).

## Report of contradictions found

- `openapi.yaml` 0.6.0, `distributeTable`: "The existing rows are moved to their buckets; the table can be a ledger table at the same time", while `create_distributed_table` needs an empty table (README), `ledger_enable` refuses partitioned tables, and the control plane answers 422 for `method: reference`. This phase makes the first half true and keeps ledger tables out.
- README of the extension says "a foreign key from an ordinary table to a distributed table is refused" and that foreign keys to reference tables are allowed from distributed tables; it does not say anything about a key from an ordinary table to a reference table (needs verification 9).
- `choose_colocation` allocates `max(colocation_id) + 1`, which can reuse an id; the 0.3 review's S4-01 argument assumes placements never come back.
- `dist_table.c` says the remote work of `create_distributed_table` is committed in the pre-commit callback ("not atomically, see node_admin.c") while the README says it is one two-phase transaction.
- [shard-phase2b.md](shard-phase2b.md) states that a distributed table has no user triggers by construction; this phase keeps that and lists it as a reason for the refusal.

## Found while building

### Increments 9.1 and 9.2 (2026-11-19)

The extension version of this phase is 0.7 (the document says 0.6, which phase 2b took).

- **The blocking variant builds a shadow named after the table's oid** (`kingres_shadow_<oid>`) while
  readers read the old table, and drops the old table in the same transaction after the counts
  match. No copy of the old table is kept and no digest is compared; `table_old_copy` and the
  operation tables exist and are unused until 9.3, whose rebuilds need names per operation.
- **Foreign keys** are added validated inside the transaction. A key to a colocated distributed
  table can exist only with one shard group or with routing off; a key of a table to itself only on
  the distribution column.
- **The form of a refusal.** One blocker is its own sentence with the SQLSTATE of its code; several
  are "N problems prevent the change of table X" with one line per code in the detail and SQLSTATE
  `0A000`. Codes added to the report beside the design's: `not_available_yet`, `node_unreachable`,
  `role_missing`, `name_in_use`, `group_busy`, `no_hash_support`.
- **The handshake** asks the other nodes with twice the node timeout.
- **`create_distributed_table` sends eleven statements to a node** (seven before).
- **A colocation id is never used again**; ids are therefore not contiguous, which the control plane
  must not assume.
- **Sequences.** The owner of an owned serial sequence is now set on the other nodes; privileges on
  owned sequences are not carried yet.
- **`money`** crosses nodes under `lc_monetary = C` in the copy only; other paths are as before.
- **For range and list partitions** a check constraint proves the attach only when the column is
  `NOT NULL`.
- **The guard of DDL** fires only when the statement is propagated.

Of the section "Needs verification": items 1, 2, 3, 4, 5, 7, 8, 9, 11, 13 and 14 are settled by the
regression test `pg_facts` and the TAP tests; 6, 10 and 12 are not.

What the control plane must follow: the version 0.7, `kingres.table_change_report(kind, relation,
options)`, the views `kingres.table_operations` and `kingres.partitions`, the new error texts.

### The fixes for the review of 9.1 and 9.2 (2026-11-19)

- **P9A-01 (critical).** The copy cursor used the snapshot of the top statement, taken before the
  call waited for the lock on the table, so a change committed by a transaction that was open when
  the call began was not copied. The copy (distributed and reference) now pushes a fresh snapshot
  after the lock and pops it when the cursor is closed. `REPEATABLE READ` and `SERIALIZABLE` are
  refused with the reason (`55006`), since a transaction snapshot cannot be renewed. In the online
  engine the capture trigger is created under `SHARE ROW EXCLUSIVE`, which waits for every writer,
  and the switch replays under `EXCLUSIVE` with fresh statements. `t/150` races a writer that
  commits during the wait, for every kind of change, both variants and reference tables.
- **P9A-02 (high).** The utility hook takes `ROW EXCLUSIVE` on the table of a statement before it
  reads the catalog (ALTER TABLE, RENAME, SET SCHEMA, CREATE INDEX, TRIGGER, RULE or POLICY, COPY
  FROM, TRUNCATE, GRANT on tables, DROP TABLE), and resolves the name again after the wait. For a
  statement that propagates, the lock is given back before the catalog lock is taken (keeping it
  deadlocked with propagating DDL). **Remaining window:** a statement that touches a plain table and
  a distributed table together and propagates (a foreign key from the plain table to the
  distributed one) is not seen by a call on the plain table that starts in that gap.
- **P9A-03 to P9A-05, P9A-07.** New blocking codes of the report: `rule`, `invalid_index`,
  `check_not_valid`, `dependent_object` (also a column of another table of the row type),
  `default_sequence_not_owned`, `function_owner_not_superuser`, `enum_distribution_column`,
  `collation_missing`, `schema_privilege` (asked only when the owner is not a superuser),
  `name_in_use` (now also on the other nodes), and `row_level_security` also for
  `FORCE ROW LEVEL SECURITY` alone; attribute options are a warning (`not_carried`). P9A-05 is
  resolved by refusing, not by running the copy as the owner as the design proposed.
- **P9A-06.** The state of a serial sequence is raised to the largest value of the column and kept
  when it is higher (documented in the README of the extension).
- **The privileges of the new table** are those of the old one: `internal_reset_acl` takes back every
  grant that default privileges gave the shadow (the owner's own entry included), then the carried
  grants are applied. A default ACL is written out explicitly afterwards (the same privileges).
- 21 one-line mutants of the round, all killed by `t/150`. One test skips where the system has only
  the C and POSIX locales (a different `lc_monetary` between nodes); one check of the copy is an
  equivalent mutant (it guards a partition replaced between the probe and the insert, which nothing
  can do to a private shadow).

### Increment 9.3, the rebuild engine (2026-11-19)

`create_distributed_table_concurrently()` and its reference variant: a shadow is built and loaded
while writers go on, a capture trigger logs the keys they change, the log is replayed in chunks, and
the switch replays the rest and swaps the tables under a short lock. Where it differs from the
sections above:

- **Names.** The shadow parent is `kingres_shadow_<oid of the source>` (found again after a crash
  without a lookup; recorded in `table_operation_item.shadow_name`). The design's per-operation names
  are needed only for a rebuild of a table that is already distributed (9.4). The log table is
  `kingres.tlog_<op>_1(seq, op, k1 .. kn)` with the key columns in key order, because source
  columns may be called `seq` or `op`. The key is the primary key or the first unique index over
  `NOT NULL` columns without predicate or expression; it always contains the distribution column,
  so an `UPDATE` that changes the key logs the old key as a delete and the new one as an update.
- **The replay deletes in the transaction of its chunk**, on other nodes in the remote branch of
  that chunk's two-phase transaction, not in a session of its own (which could not see an earlier
  chunk's uncommitted insert of the same key). **The final replay runs in the transaction of the
  swap**, so the statement that sees the log empty is in the transaction that renames.
- **The verification after the switch is not exact while writers write.** The digest of a node
  also counts the rows newer than that node's swap transaction; if the digests differ and newer rows
  exist the result is `skipped` with the reason, otherwise `failed`. **A workload of deletes only in
  that window can give a false `failed`**; the old copy is then kept and nothing is lost. An exact
  check would need a digest under the lock, which the targets for blocked time rule out.
- **`done` does not wait for the old copy to be dropped.** The claim of the colocation group is
  released after the verification and the validation of the foreign keys; the old copy moves to the
  schema `kingres`, renamed per object, unreadable to all but its owner and without foreign keys,
  and `cleanup_table_operations()` (called by the recovery worker) drops it after
  `kingres_shard.table_cleanup_delay`. `check_cluster()` reports `old_table` when it is an hour
  overdue.
- **The reference variant keeps a shadow on every node**, the map group primary included.
- **The shadow gets all its indexes when it is made**, not the non-unique ones after the copy; a
  faster load is left for later.
- **`resume_table_operations()` undoes** an operation whose session is gone, that did not switch and
  that has not been written for twice the node timeout, and finishes one that switched. Continuing
  an operation is `kingres.table_operation_advance()` called by another session (the control plane);
  the recovery worker does not run a copy inside its pass. An unreachable node during an undo keeps
  its leftovers, listed in `spec -> 'cleanup_pending'` and cleaned when it answers; an empty
  colocation group may stay and is taken by the next table of its kind.
- **`kingres_shard.rebuild_batch_bytes` is not built**; a chunk is bounded by rows and the batch
  sender bounds what goes to a node.
- **The wrapper functions undo what fails** before they re-raise; the wrapper is one statement and
  holds its snapshot for the whole operation, the control plane's phase calls do not.
- **The caller's transaction must not hold locks on the source**: statements that read the source
  run in a session of their own, or the swap waits for ever. A later increment that adds a read of
  the source to the caller must do the same.
- **The switch attempt locks the row of the operation** for its length, so a cancel and a switch are
  serialized and a cancel never arrives in the middle of a swap.
- **`check_cluster()` is now a wrapper** over the old function (renamed
  `internal_check_cluster_base`, same privileges) and a part for operations: rows
  `table_operation_stalled`, `shadow_table`, `log_table`, `capture_trigger`, `old_table`,
  `table_verification_failed`.
- **DDL on the source of an operation is refused** with the id of the operation (the sessions of the
  operation are exempt); the guard reads the catalog as the bootstrap superuser and only for the
  statements it guards. A claimed group is skipped when `create_distributed_table` looks for a group
  of the same type, collation and bucket count.
- **Fault points** added for the swap (`rebuild_switch_before_swap`, `_after_local_swap`,
  `_after_remote_swap`, and the commit points scoped with `@tswitch`); those of partitions come with
  9.5.
- **Identity and owned sequences** keep their options on every node and their state at the origin;
  a value drawn by an explicit `nextval()` in the milliseconds between the read of the state and the
  rename would be given twice.
- **A defect of 9.2 found by the tests:** `kingres_shard_load_placement()` read its bucket array
  after it was freed (reachable only by a superuser calling an internal function); fixed.
- 22 one-line mutants of the proof of completeness and of the switch, all killed (`t/144` to
  `t/149`).

What 9.4 will find: the table DDL function knows only "a plain table becomes hash-partitioned"; a
distributed source needs a copy over each node's partitions, a log per node, the per-operation
shadow names, a placement kept in `spec`, a retirement that moves partitions, locks on every node
and bucket partition at the switch, and claims of several groups for an alter of colocated tables.

### The second review and its fixes (2026-11-20)

The second review ([report](../reviews/2026-11-20-kingres-shard-phase9-rebuild.md)) looked at the
rebuild engine and at the fixes for the first review. It found that P9A-01 holds and that P9A-05 was
only half fixed, and it found two high, four medium and four low defects and three notes. Each one was
reproduced by its test on the tree under review before it was fixed. The tests are t/180 to t/184.

- **P9B-01 (high), code of a role that is not a superuser ran as a superuser.** The check of P9A-05
  followed only the dependencies of the table's defaults, constraints and indexes on functions. A
  domain check or default, the function behind an operator, an operator class, a cast and a range or
  composite type were not followed. A role became superuser on every node. Now
  `kingres.internal_untrusted_functions(relation)` follows all of them, and the operator classes of the
  column types, to every user-defined function owned by a role that is not a superuser. The report
  makes each one a `function_owner_not_superuser` blocker that says how it was reached, so both calls
  and the reference variant refuse before they start. The section on rights asked for the code to run
  as its owner or be refused; it is refused, also for a `SECURITY DEFINER` function of such a role.
  Code of a superuser is trusted; what such a function calls by name in its body is not followed.
  The capture log stores the base type of a domain key, so no domain check runs in the trigger, which
  runs with the rights of the extension's owner.
- **P9B-02 (high) and P9B-08 (low), the capture.** The trigger took the key from `to_jsonb(NEW)`,
  which writes a float with the `extra_float_digits` of the writer's session: a writer with a value
  below 1 logged a key that does not exist, and its change was not replayed. A row with a value over
  256 MB could not be written. The trigger now reads the key columns from the row with no text between,
  and it runs with `extra_float_digits = 3`; the sessions of the engine are already forced to 3.
- **P9B-03 (medium), the replay and a unique index other than the key.** A value that moved from one
  row to another could put the two rows in chunks in the wrong order, and the replay failed on every
  retry. A chunk that raises `unique_violation` is now repeated with the whole log in one chunk, which
  cannot conflict with itself; in the final replay of a switch attempt the attempt is given up and
  tried again. Ordering the entries or deferring the index cannot be done for an index that is not
  deferrable.
- **P9B-04 (medium), the DDL guard.** It now also refuses `DROP TRIGGER` (the capture trigger by
  the table's owner), `DROP INDEX`, `ALTER INDEX`, `SET SCHEMA`, `CREATE RULE`, `CREATE POLICY`,
  `DROP RULE` and `DROP POLICY` while an operation runs.
- **P9B-05 (medium), a cancelled call.** `WHEN OTHERS` does not catch `query_canceled`, so a call
  cancelled while its session stayed left an operation that nothing could cancel. The handlers catch
  both now. The same tests found that a switch attempt that runs out of time is raised as
  `query_canceled` too, so "the switch committed although its answer was lost" was never seen after a
  timeout; the switch and the recovery call catch it now.
- **P9B-06 (medium), a reader's lock.** The lock of P9A-02 is taken only for a role that has the
  right the statement needs. A role with only `SELECT` can still make an administrator's call wait
  with a `GRANT` in an open transaction, because `GRANT` writes the table's row in `pg_class` even
  when it grants nothing; plain PostgreSQL waits the same way.
- **P9B-07 (low).** The README now states the bound of the capture's wait for writers.
- **P9B-09 (low).** The retained old copy loses column privileges too.
- **P9B-10 (low).** t/184 kills the six mutants that survived the review.
- **P9B-11 (note), the verification.** One write after the swap hides any loss from the comparison.
  An exact comparison would need a digest taken under the lock, which this design rules out, or the
  logged keys. The record now says why in `detail`, the call that ends the operation raises a WARNING
  with the reason, and the README says what to do.
- **P9B-12 and P9B-13 (notes).** The README now names the window of P9A-02, the double draw of a
  sequence, the disk and WAL cost of the online variant, and that a distributed serial column counts
  per node after distribution.

Of the mutants of the fixes, all are killed except two lines that set `extra_float_digits` in the copy
and the replay, which were removed because those sessions are already forced to 3.

### Increment 9.4, undistribute and alter (2026-11-20)

Four calls: `undistribute_table` and `alter_distributed_table` (blocking), and
`undistribute_table_concurrently` and `alter_distributed_table_concurrently` (online). Tests t/185 to
t/188. The only C change is in `rebuild.c` (an optional base name for shadow partitions and a helper
that reads one value from a node's branch); the planner and the pushdown code are not touched.

**The blocking calls.** The order is:

1. Checks: superuser, map group primary, version handshake, the cluster lock, then the report with
   two new blockers: an object such as a view that uses a table the call drops (`dependent_object`),
   and a group that a move uses.
2. The statements that rebuild the old table's definition (sequence states, owner, privileges, keys)
   are read before anything is dropped.
3. `EXCLUSIVE` on the table on every active primary in node order: writers, also those of other
   nodes, wait; readers go on.
4. Undistribute makes one ordinary shadow table on the target node. Alter renames the old partitions
   out of the way on every node and makes the shadow of the new layout with its final names. Then
   the rows are copied and the counts compared.
5. The swap runs with the extension's statement handling off in every node's branch: the sequences
   are released, the old tables are dropped, the shadows take the names, and the keys, sequence
   states, privileges and comments follow. Then the catalog changes, a group left without a table
   is deleted, and `sync_catalog()` runs.
6. The caller's transaction commits with the two-phase commit of 2a.

The whole call is one transaction, so no claim row is written: the cluster lock and the report's
refusal of a busy group keep others out. Deviations from the design:

- For a reference table, the copy of the target node stays and the copies are not compared first
  (a difference could only be reported, not repaired; the README says so).
- Foreign keys of the component are recreated valid in the same transaction. `NOT VALID` with a
  later validation belongs to the online form, whose switch must not scan under the locks.
- A changed distribution column always makes a new group. When the new bucket count is a multiple of
  the old one, each bucket keeps the node of its rows, but the rows still pass through the map group
  primary.
- Four fault points per call (after the lock, the copy, the drop and the swap) make the atomicity
  and the waiting of writers testable.

**The online calls.** One operation row on the map group primary and a mirror row on every other
node. The node-local functions of 9.3 (capture, copy step, replay, undo, DDL guard) read the row of
their own node and run unchanged on every node with routing off, so the lessons of the second review
(the guard on every node, exact keys in the capture, the same cancel and undo) hold here too. The
destination is either the shadow partitions of the new placement (alter) or one ordinary shadow
table on the target node (undistribute). The switch attempt takes the catalog lock, then `EXCLUSIVE`
on every node's table, replays every node's log and checks that every log is empty. It then takes
`ACCESS EXCLUSIVE` and swaps node by node, recording each node's swap transaction and old copy. The
verification compares the digests of all old copies with the new table, with the rule of 9.3 for
rows written after the swap. The cleanup drops the old copies on every node. The untrusted-code
check of the second review also applies to these calls.

Decisions of the online form:

- It takes one table. A table linked by a foreign key to another hash-distributed table, and the
  `cascade_*` options, need the blocking calls; a key to a reference table is carried over.
- A request that moves no rows returns NULL and starts no operation; the blocking call does that
  change.
- After an undistribute, a writer on another node whose statement comes after the swap gets
  `relation ... does not exist` and writes nothing.

**Mutants.** Of the blocking calls, two survive: the count check after the copy cannot fail,
because the copy already fails unless every row read was stored. Every mutant of the online calls is
killed. Two of them survived the first run and led to new tests: one that reads `pg_locks` on every
node at the three points of the switch, and one that loses a row from the old copy of a second node,
which the verification must report.

**Not done.** Several tables at once online, and the control plane and console (9.6).

### Increment 9.5, user-partitioned tables (2026-11-20)

A table partitioned by the user (RANGE or LIST) can be distributed: every leaf is distributed in the colocation of the parent, the four partition functions and the statements keep the tree the same on every node, the bucket move copies the tree, and queries read the tree. Notes from the build follow.

#### Answers to the two open questions (experiments on 18.7 and 19rc2, identical results)

1. `ATTACH PARTITION` with a CHECK equal to the bound.
   - A CHECK that is only the user's bound (`t >= .. AND t < ..`) does NOT make the attach skip the scan ("verifying table leaf"): the partition
     constraint of a leaf also contains the constraints of its ancestors (the hash condition of the bucket partition).
   - A CHECK that includes the hash condition (`satisfies_hash_partition('<root>'::regclass, m, r, key) AND <bound>`) IS recognised
     ("partition constraint for table ... is implied by existing constraints"), no scan.
   - Consequence for the design: the CHECK(satisfies_hash_partition ...) on the bucket partition (phase 4 puts it there) is inherited by a leaf made with
     `LIKE <bucket partition> INCLUDING ALL`; adding only the bound as a second CHECK then makes the leaf attach without a scan (measured: leaf3 and p_b1
     above). The bucket partition attached to the root with the hash CHECK on it is also implied (no scan).
   - A DEFAULT partition under the bucket partition is ALWAYS scanned by an attach, whatever checks the new leaf has ("verifying table p_b0_def"), and it is
     locked `ACCESS EXCLUSIVE` for the rest of the transaction. `ATTACH` locks the bucket partition `SHARE UPDATE EXCLUSIVE`; `CREATE TABLE ... PARTITION OF`
     locks it AND the default partition `ACCESS EXCLUSIVE`. So the design's choice (create a leaf by `ATTACH`) holds, and the `default_partition` warning is
     right: the cost is one scan of the default leaf per bucket and node, and the default leaf is blocked for the whole two-phase commit.
2. `publish_via_partition_root = true` on a partitioned root.
   - The initial copy reads the root (60 rows arrive, routed by the subscriber into its own leaves 36/24); update, delete and insert are applied through the root;
     the root with a primary key and replica identity `d` (default) is enough; a row filter on the partitioned root in such a publication is accepted.
   - The subscriber needs the partition tree (or at least a matching partitioned table) with the leaf that the row belongs to: a change for a bound
     with no leaf on the subscriber fails in the apply worker and is retried, so the move's target must create every leaf before the subscription starts and
     a leaf created on the source during a move is refused (the design already refuses DDL on a group that moves).
   - Not yet checked: phase 4's marker table and its `pg_subscription_rel` checks against a partitioned root (the table in `pg_subscription_rel` is the root's
     leaves or the root, depending on the version: to be read from bucket_move.c in step 5, with a test).

#### Step 2: blocking create_distributed_table of a RANGE or LIST table

- The report (`table_change_report`, kind distribute) replaces the blanket blocker with: `partitioned_table` (a partition on its own, HASH strategy, a
  partitioned reference table), `subpartitioned`, `foreign_partition`, `partition_local_object` (index, constraint, default or storage parameter of the
  partition that the table lacks), `tablespace`, `name_too_long`; warnings `default_partition` and `distribution_column_is_partition_key`; the online
  variant refuses with `not_available_yet`.
- `internal_partition_layout(relid)` returns the key text (`pg_get_partkeydef`) and one row per partition (bound without `FOR VALUES`, `DEFAULT` for the
  default partition); `dist_table.c` makes every bucket partition `PARTITION BY <method> (<key>)` with one leaf per user partition, here and on every node
  (`kingres_shard_append_partition_tree`), copies from the parent (tuple routing at the owner places each row), and `internal_set_partitioning` records
  `partition_method`, `partition_key` and the `dist_partition` rows. The swap drops the source parent and with it its partitions.
- New setting `kingres_shard.max_partition_leaves` (default 8192), checked in C once the bucket count is known.

#### Step 3: the four partition functions and the statements (lessons)

- Functions: `create_partition`, `drop_partition`, `detach_partition`, `attach_partition`, built on `internal_leaf_op` (node-local, per owned bucket),
  `internal_leaf_everywhere` (per node, routing off, bound validated: no `;`, `--`, `/*`, must start with FROM, IN, WITH or be DEFAULT) and
  `internal_partition_context` (superuser, map group, version, catalog lock, busy group check). Fault points `partition_after_lock` and
  `partition_after_leaves`.
- Decision: a leaf is made `LIKE <bucket partition> INCLUDING ALL` and attached; no equal CHECK is added, because the leaf is empty (the experiments say
  a CHECK would only matter for a populated table, and then it must include the hash condition).
- Decision: `attach_partition` takes a distributed table of the group only; the ordinary-table form of the design (copy through the router, consume the table)
  is not built: the caller creates the partition and inserts the rows. `detach_partition` and `attach_partition` refuse tables with foreign keys.
- The hook (`utility.c`, `map_partition_statement`): the raw tree of a partition bound holds expressions, not `PartitionRangeDatum`; `MINVALUE` and `MAXVALUE`
  are column references (found by the first run, which crashed the backend on a cast: fixed). Constants, casts of constants and negative numbers are
  written back; anything else is refused with the hint to call the function.
- A user-partitioned table cannot be undistributed or altered yet (`not_available_yet` in the report).
- Not done in step 3: the ordinary-table attach, `ALTER TABLE s`/`CREATE INDEX ON s` messages naming the parent (PostgreSQL's "does not exist" stands),
  foreign keys on detach and attach.

#### Step 4: partition_leaves and the check

- `kingres.partition_leaves(parent, partition_name)` from the catalog (node, bucket, leaf); `check_cluster()` reports `dist_partition_mismatch` for a leaf that
  the catalog lists and its owner node does not have (one query per node). t/192, 5 mutants killed.

#### Step 5: the bucket move

- The source's `internal_move_inspect` adds the tree (`internal_move_tree`: key, leaves with bound and access method); `internal_create_move_tables` makes the
  target copy `PARTITION BY` the key, one leaf per user partition with `USING <access method>` (so a converted leaf keeps its format; phase 12's requirement),
  owners of the leaves follow the table; the `ks_bucket_check` on the root is inherited by the leaves, so the attach at the switch scans nothing (the
  experiments above); the publication uses `publish_via_partition_root = true` when a table of the group is partitioned; `internal_detach_bucket` (C) renames the
  leaves and their indexes to names of the orphan and moves them to schema `kingres` so a bucket that returns finds the names free; `internal_drop_orphans`
  and `internal_drop_move_tables` take partitioned tables; the view `kingres.buckets` sums the leaf sizes over `pg_partition_tree`.
- Found by the first run of t/193: a writer on another node gets "the bucket map changed while the statement was running ... Repeat the statement" at
  the switch (existing behaviour, retryable); the test's writer repeats. The orphan listing of `check_cluster()` (`orphan_table`) does not include a partitioned
  orphan: the 0.6 function was not redefined for it; the cleanup drops it.
- Mutants: v01-v11 (10 run, v03 not testable: the test build has only the heap access method); v02 (publication without the root) survived the first run
  because the leaf names are the same on both sides: killed by a test that holds the move after the slot and reads `pubviaroot` and `pg_publication_tables`.
- Old move tests (t/025-037, t/074, t/083) pass on both majors with the changes.

#### Step 6: queries

- No change to planner.c, fragment_scan.c, shippable.c or pushdown.c was needed: with a user-partitioned table the plain-table paths give single-node answers
  from every node (t/194, 119 tests), closed aggregates are fragment scans, key plus interval prune to one bucket partition and one leaf (+ the default one).

#### Files changed outside tests, for the 2b reviewers

- src/utility.c (the hook mapping, a new static section before the hook function), src/dist_table.c / dist_table.h (`kingres_shard_append_partition_tree`, the layout in
  the creation), src/kingres_shard.c / .h (one setting), src/bucket_move.c (leaves in `internal_detach_bucket`), the SQL delta. Not touched: planner.c,
  fragment_scan.c, shippable.c, pushdown.c, rebuild.c, copy_routed.c.

#### Not done in 9.5

- `create_distributed_table_concurrently` of a partitioned table, undistribute and alter of a user-partitioned table (refused with `not_available_yet`).
- Attaching an ordinary table to a distributed partitioned table by a function (the design's copy-and-consume form); foreign keys on detach and attach.
- `ALTER TABLE s`/`CREATE INDEX ON s` of a logical partition name: PostgreSQL's "does not exist" stands.
- `orphan_table` of check_cluster for a partitioned orphan; the access-method mutant (needs a second access method).
- Not run: the whole TAP suite, t/046, the in-tree meson run; the cluster mutex was not taken.

### Increments 9.8, dependent views, and 9.9, storage parameters (2026-11-20)

#### 9.8: kingres_shard.dependent_views = 'recreate'

Design decisions
- The option is a session setting, not an argument: the public functions have fixed signatures and the design's option belongs to the control plane request
  (9.6 maps it to the setting). `refuse` (default) keeps today's behaviour; `recreate` drops and recreates. An online operation reads the setting once, in
  `begin_table_operation`, and keeps it in its spec (a session that finishes it later does not change what it does with the views); the options of the
  report carry it too, because the control sessions that check a request again do not have the caller's setting.
- `internal_view_capture(regclass[])` (node-local): the plain views that depend on the tables, transitively, in an order in which they can be created (a view after
  the views it uses), with definition (`pg_get_viewdef`), column names, owner, reloptions, privileges (`aclexplode`, with the grant option and PUBLIC),
  view and column comments. `blockers` lists what cannot be recreated: a materialized view, a rule other than the view's own, a trigger, privileges on a
  column, a function that uses the row type, a publication of the view, a dependency cycle.
- Where: blocking `create_distributed_table` (C, local node), `alter_distributed_table`, `undistribute_table`, the single-node switch of the online engine and
  `internal_switch_attempt_dist`. The views are captured on every node that holds the table (a view is local to the node where it was created), dropped in the
  transaction of the swap (last first) and recreated after it (first first). Recreating is inside the same transaction and the same locks as the swap.
- The report (`table_change_report`) with `recreate`: the views are not blockers; what cannot be recreated is a `dependent_object` blocker naming the node;
  for an undistribute a view on a node other than the one that keeps the table is a blocker ("the table will be on node N only"). Everything is refused before
  anything is made, and no operation is recorded.

Lessons of the build
- Reading a view (`pg_get_viewdef`, deparse) takes a lock on the tables it uses: capturing on another node after the switch took ACCESS EXCLUSIVE there
  waits forever. The capture of `internal_switch_attempt_dist` is made before the ACCESS EXCLUSIVE loop. The check of the report runs in a control session on every
  node (this one too): in the caller's transaction it left a lock on the source that the switch (another session) needed, and the operation deadlocked with its own caller.
- The first plan recreated views on every node on undistribute; the table is gone from the others, so it is now a refusal.
- The scan's rule that a distributed table read through a view of another owner needs `security_invoker` shows in the tests: the views over distributed tables are `security_invoker`.

Tests: t/195_dependent_views (default refuses; every operation, blocking and online; views on views (the view that sorts before the one it uses); a view on node 2;
privileges, owner, options, comments identical; the views are bound to the new table; refusals (materialized view on a table and on a view, trigger, column privilege, rule;
a view on node 3 of a distributed table; undistribute with a view on node 2); an error at four points of the switch and in the blocking alter after the drop; a reader of a view on
node 2 waits for the switch and reads through the new table). Mutants w01-w15, all killed (the order mutant needed the test with the alphabetically earlier view name).

#### 9.9: storage parameters carried

Design decisions
- `kingres.dist_table.reloptions text[]`, synchronized (the catalog snapshot carries it). A partitioned parent cannot have storage parameters, so they are on the
  bucket partitions; the record is what every later table gets.
- `internal_reloptions_of(regclass)`: the recorded ones of a distributed table, else the table's own; `internal_with_clause(text[])`.
- Applied: blocking create (C: `kingres_shard_append_partitions` with a WITH clause on every node), the shadow script of the rebuild engine (create and alter
  online), `internal_table_ddl` for a plain target (undistribute, reference tables), the bucket move (the source's bucket partition reloptions travel in
  `internal_move_inspect`, the target copy is made WITH them). Recorded: the C create, the C swap of the single-node engine, blocking alter, online alter.
- The report: `storage_parameters` is a note, not a blocker.
- Not done: the parameters of a partition of a user-partitioned table (still `partition_local_object`), and keeping the record in step with a later `ALTER TABLE ... SET`
  on a bucket partition.

Tests: t/196_storage_parameters. Mutants x01-x13.

#### Results and files

See the report. Files changed outside tests: src/dist_table.c, dist_table.h, kingres_shard.c, kingres_shard.h, rebuild.c, the SQL delta; planner and pushdown files untouched.
