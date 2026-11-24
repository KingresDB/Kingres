<!-- SPDX-License-Identifier: AGPL-3.0-only -->
# kingres_shard and kingresd: design of phase 10 (splitting buckets, background rebalancing with strategies, draining a server)

Status: Proposed design, 2026-11-15; not implemented.

Builds on [shard-phase4.md](shard-phase4.md) (moving buckets, `rebalance_plan`;
extension version 0.3), [ha-phase5.md](ha-phase5.md) (standbys, leases, chained
placement), [control-plane-3.md](control-plane-3.md) (task engine, servers) and
the roadmap in [architecture.md](../architecture.md), section "Phases 9 to 12".
It runs under the standing rule of [ADR 0022](../adr/0022-the-complete-scan-stays.md):
the row-moving distributed scan stays the path that answers any `SELECT`; a split
or a move may make a statement fail with the retryable `KS001`, never answer wrongly.

Phase 10 adds three capabilities, each with its control in the web console:

1. splitting one bucket into several (the cure for a bucket that is too big or too
   hot, and the mechanism for isolating one tenant);
2. rebalancing in the background with a strategy ("by bucket count" or "by disk
   size"), a threshold, an automatic mode with safety rules, stop and resume,
   and reports;
3. draining a server so that it can be removed.

The extension stays small: it gets the split, a plan function that knows costs
and weights, and fixes for the holes that the review of version 0.3 found. The
orchestration of a whole rebalance, the automatic mode, the safety rules and the
drain are `kingresd`'s, as tasks.

## Sources read

Documents of this repository (committed versions for `contrib/kingres_shard`
and `kingres/control-plane`, which other people are editing):

- `contrib/kingres_shard/README.md` (all of it), `kingres_shard--0.3.sql` (catalog
  tables, `rebalance_plan`, `set_node_property`, `internal_move_tables`,
  `bucket_move`), at the last commit.
- [shard-phase4.md](shard-phase4.md) with its implementation notes;
  [shard-phase2b.md](shard-phase2b.md) (section 10, section 11.8);
  [ha-phase5.md](ha-phase5.md); [control-plane-3.md](control-plane-3.md);
  [control-plane-2.md](control-plane-2.md) (task rules);
  [ADR 0007](../adr/0007-fixed-hash-buckets.md),
  [ADR 0021](../adr/0021-failover-in-kingresd.md),
  [ADR 0022](../adr/0022-the-complete-scan-stays.md).
- The review [2026-11-15-kingres-shard-0.3.md](../reviews/2026-11-15-kingres-shard-0.3.md)
  (S4-01 to S4-10).
- `kingres/api/openapi.yaml` at the last commit (contract 0.6.0): the six rebalance
  operations and their schemas, `listBuckets`, `getBucketDistribution`,
  `updateServer`, `removeServer`, `ShardGroup`, `Server`, `ServerStatus`, the
  problem codes, `ClusterSettings`.
- `kingres/control-plane/README.md` (task engine, crash points, membership,
  "What rebalance and failover orchestration will need from this code"),
  `internal/task/task.go`, `internal/membership/remove.go` and `facts.go`,
  `internal/leader/leader.go`, at the last commit.
- `kingres/web/README.md` and the names of the files under `kingres/web/src`
  (pages `ShardsPage`, `RebalancePage`, `ServersPage`; `ServerActions`,
  `BucketGrid`, `RebalanceReportDialog`); [documentation.md](../dev/documentation.md).

PostgreSQL 18.7 in this tree (read, not run):

- `src/backend/partitioning/partbounds.c`: the hash bound rules
  (`check_new_partition_bound`, lines about 2925 to 3040: every modulus must be a
  factor of the next larger one; overlap is checked slot by slot in
  `boundinfo->indexes`), the hash-bound array construction (about 350 to 425),
  `satisfies_hash_partition` (about 4755).
- `src/backend/executor/execPartition.c` (about 1424 to 1436): a row goes to
  `boundinfo->indexes[rowHash % boundinfo->nindexes]`, where `nindexes` is the
  greatest modulus in use.
- `src/backend/commands/tablecmds.c` (about 4854 to 4863): `ATTACH PARTITION` takes
  SHARE UPDATE EXCLUSIVE on the parent, a plain `DETACH PARTITION` ACCESS EXCLUSIVE.
- `src/backend/parser/gram.y`, `tablecmds.c`, `doc/src/sgml/ref/alter_table.sgml`:
  no `SPLIT PARTITION` or `MERGE PARTITIONS` (searched; nothing found).
- `src/include/catalog/pg_proc.dat` (`satisfies_hash_partition`: variadic, not strict),
  `doc/src/sgml/ref/alter_table.sgml` (`ENABLE ALWAYS TRIGGER`).

Web sources (documentation only; Citus is AGPL and none of its source was read):

- Citus, cluster management and rebalancing:
  https://docs.citusdata.com/en/stable/admin_guide/cluster_management.html
  (background rebalance, sequential moves per colocation group, logical
  replication, replica identity requirement, tenant isolation by splitting).
- Citus, function reference:
  https://docs.citusdata.com/en/stable/develop/api_udf.html
  (`citus_rebalance_start` with `threshold`, `drain_only`, `rebalance_strategy`;
  `citus_drain_node`; `shard_transfer_mode` `auto`, `force_logical`,
  `block_writes`; strategy `by_shard_count` with constant cost 1; `by_disk_size`;
  `citus_rebalance_status`; `citus_rebalance_stop`). The page names both
  strategies as the default in different places; this design does not copy a default.
  It does not document `citus_split_shard_by_split_points`, so nothing is taken
  from it.
- Couchbase, rebalance: https://docs.couchbase.com/server/current/learn/clusters-and-availability/rebalance.html
  (rebalance after adding or removing nodes, stop and resume, concurrent
  vBucket moves per node from 1 to 64 with default 4, a JSON report per run,
  automatic retry up to 3 times, bucket rank from 0 to 1000).
- Couchbase, removal: https://docs.couchbase.com/server/current/learn/clusters-and-availability/removal.html
  (a node is marked for removal and its data stays until the rebalance has moved
  it; the replica count falls when too few nodes remain).
- PostgreSQL 19 status of `SPLIT PARTITION` / `MERGE PARTITIONS`:
  https://www.snowflake.com/en/blog/engineering/postgresql-19-release-delay-feature-reverts/
  (reported as reverted and moved to the next release). **[Needs verification]**
  against the PostgreSQL release notes; it does not matter for this design, which
  does not use the command.

## Goals

- Split one bucket of a colocation group into 2, 4, 8 or 16 buckets, online: writers
  of the colocation group's tables on the owning node wait only for the switch,
  readers only for the commit of the switch.
- Rebalance in the background as a resumable task: strategies `by_bucket_count`
  and `by_disk_size`, per-server weights, a threshold, concurrency limits, stop and
  resume, reports, and an automatic mode that is off by default and refuses to run
  unless the cluster is healthy.
- Drain a server in one operation: empty its shard group, relocate the standbys it
  hosts for other groups, and leave it removable by the existing `removeServer`.
- No new core patch. Close the holes of review S4-01, S4-02, S4-03, S4-04, S4-05
  before an automatic rebalancer depends on them (section "Holes of version 0.3
  that this phase must not inherit").
- Extend the six rebalance operations of contract 0.6.0 additively.

## Not in scope

- Merging buckets, and any change of the base bucket count of a colocation group.
  Changing the bucket count of a table by redistribution is phase 9
  ([shard-phase9.md](shard-phase9.md)); a group with split buckets is not
  re-bucketed in place (section "What this phase needs from phases 9 and 11").
- Moving the children of a split to other groups in the same operation. A split is
  in place; the children are moved afterwards by ordinary moves, by hand or by the
  rebalancer.
- User-written strategies (cost, capacity and allowed-node functions as in Citus).
  The two strategies are fixed code; their parameters (threshold, weights) are data.
- Moving schemas as units ([shard-phase11.md](shard-phase11.md)), time-partitioned
  and columnar tables ([analytics-phase12-storage.md](analytics-phase12-storage.md)),
  and changes of the query paths ([analytics-phase12-queries.md](analytics-phase12-queries.md)).
- Moving data that is not sharded: local tables, plain databases, the system
  database. A drain does not move them (section "Draining a server").
- A new global snapshot. The anomalies of [shard-phase2a.md](shard-phase2a.md)
  are unchanged.

## 1. The mechanism in PostgreSQL 18 terms

### 1.1 What a bucket is today, and what a split needs

A distributed table is a table `PARTITION BY HASH (<column>)` with one partition
`<table>_b<NNNN>` per bucket, created with `FOR VALUES WITH (MODULUS N, REMAINDER b)`.
A row belongs to the partition for which `hash % modulus = remainder`, where the hash
is `compute_partition_hash_value()` and the executor picks the partition through
`indexes[hash % nindexes]` (`execPartition.c`). Today every partition has modulus N
and `bucket = hash % N`.

PostgreSQL does not need one modulus. `check_new_partition_bound` accepts
partitions of different moduli in one table as long as every modulus is a factor of
the next larger one and the (modulus, remainder) pairs do not cover a common slot.
Example: a table with moduli 128 and 256 is valid if each 256-partition covers a slot
set that no 128-partition covers. A bucket `(m, r)` therefore splits into the two
buckets `(2m, r)` and `(2m, r + m)`, and into `k` buckets `(km, r + i*m)` for
`i = 0 .. k-1`: the children cover exactly the slots of the parent, every other
partition stays untouched, and the moduli of a group stay a chain of divisors when
`k` is a power of two (base N, N*2, N*4, ...). That is the answer to "one bucket or
the whole group": **one bucket is split and the modulus of the colocation group is
not changed.** Changing the modulus of every bucket would rewrite the whole table
and is phase 9's redistribution.

No core patch is needed. `ALTER TABLE ... SPLIT PARTITION` and `MERGE PARTITIONS`
do not exist in PostgreSQL 18.7 (searched in the grammar, `tablecmds.c` and the
reference page), and the command, where it exists in development versions, is for
range and list partitioning only. **[Needs verification]** that no development
version supports hash partitions; the design does not depend on it. A split is the
sequence "build the children as standalone tables, then in one transaction `DETACH`
the parent partition and `ATTACH` the children", which is the technique the bucket
move of phase 4 already uses. Fork policy holds: logic in the extension.

### 1.2 Why the rows cannot simply be re-labelled

The old partition holds the rows of both halves. Attaching it as `(2m, r)` would
need the rows of the other half removed first, and `ATTACH PARTITION` scans a
partition whose bounds are not implied by an existing validated `CHECK`. A copy
into two new tables is therefore needed, and it must be online. Logical replication
cannot do it on one node: a subscription writes into the table of the same
schema-qualified name as the published one, and on one node that is the published
table itself. The split therefore keeps the copy current by a row trigger on the old
partition that writes the same change into the child (section 2.3).

### 1.3 The bucket identity after a split

`bucket_map` is keyed by `(colocation_id, bucket)`. After a split the bucket
**number is the remainder**: the child `(2m, r)` keeps the number `r` of its parent
and the others get `r + i*m`. Two buckets that coexist never share a remainder
(two different moduli with equal remainder overlap on slot `r`), so the key stays
unique, the name `<table>_b<NNNN>` stays unique, and a group that was never split is
unchanged: bucket `b` has modulus N and `b = hash % N`. The number is no longer
below the bucket count. (`lpad(b::text, 4, '0')` does not produce a longer name for numbers of 10000 and
more, it cuts the number to four digits, and the C code names the partition with the full number: bucket numbers
are limited to 9999 and a split that would exceed it is refused, section 21.5.)

Routing resolves `hash` to the bucket that covers `hash % max_modulus` through an
array of `max_modulus` entries built from `bucket_map` (the same idea as
`boundinfo->indexes`). `max_modulus` is at most 65536
(`kingres_shard.max_bucket_modulus`, default 65536; 256 KB of array per colocation
group and backend, the same order as the array PostgreSQL itself keeps for the
partitioned table).

## 2. SQL interface

### 2.1 Version and upgrade

Phase 10 produces extension version **0.7** with the script
`kingres_shard--0.6--0.7.sql`, and the install script `kingres_shard--0.7.sql`.
The numbers follow the planned order of the roadmap (0.4 phase 5, 0.5 phase 2b, 0.6
phase 9); **[Needs verification]** against [shard-phase9.md](shard-phase9.md) and
the merge order; if phase 9 does not change the version, phase 10 is 0.6.
`ALTER EXTENSION kingres_shard UPDATE` must give the same objects as a fresh
install, compared by the `upgrade` regression test as in 0.3. The upgrade rewrites
nothing in user tables: every existing bucket gets `modulus = bucket_count`.

### 2.2 Catalog changes

| Object | Change | Reason |
|---|---|---|
| `kingres.bucket_map` | add `modulus int NOT NULL` (upgrade: the colocation group's `bucket_count`); `CHECK (modulus >= 1 AND bucket < modulus)`; state check gets `'splitting'` | a bucket is `(modulus, remainder = bucket)` |
| `kingres.colocation` | add `max_modulus int NOT NULL` (upgrade: `bucket_count`); `CHECK (max_modulus BETWEEN 1 AND 65536)`; add `placement_epoch bigint NOT NULL DEFAULT 0` | routing array size; the monotonic epoch of S4-01 (section 7.1). `bucket_count` keeps its meaning of the base modulus N that a new table asks for; the number of buckets is the number of rows of `bucket_map` |
| `kingres.node` | add `weight float8 NOT NULL DEFAULT 1 CHECK (weight > 0 AND weight <= 1000)` | capacity of the group whose primary this node is (section 4.3) |
| `kingres.bucket_move` | add `kind text NOT NULL DEFAULT 'move' CHECK (kind IN ('move','split'))`, `parts int`, `children jsonb`, `rows_done bigint`, `rows_total bigint`, `attempts int NOT NULL DEFAULT 0`; replace `bucket_move_groups_check` by `CHECK (kind = 'split' OR source_group <> target_group)` | one record, one unfinished-operation-per-bucket index, one resume and cleanup path for moves and splits. For a split `source_group = target_group` is the owning group |
| `kingres.buckets` (view) | add `modulus` | |
| `kingres.internal_catalog_snapshot` / `internal_apply_catalog` | carry the new columns; an absent key on apply means the default | rolling upgrade (section 7.5) |

Every place of the extension that uses `bucket_count` as a modulus must use the
bucket's own modulus or the routing array instead. From the committed sources:
`kingres_bucket_for_relation` and `_colocation`, the planner's key-to-bucket step,
`internal_table_ddl` (creates partitions), `create_distributed_table`, `add_node`
(creates the partitions a new node owns), `internal_attach_bucket(parent, bucket,
bucket_count)` and `internal_create_move_tables` / `internal_move_inspect` (the
parameter named `p_bucket_count` is in fact the modulus), `rebalance_plan`, and the
view `kingres.buckets`. **[Needs verification]** by a search of every use of
`bucket_count` in the working tree before the work starts; a use that is missed
produces a wrong bucket for a split group and no error, so each such function also
calls `kingres_shard_assert_bucket_layout(colocation_id)` in the test builds, which
fails when `bucket_map` does not cover every slot exactly once.

### 2.3 New and changed functions

```sql
-- Splits one bucket in place. Map group primary, superuser, READ COMMITTED.
-- Returns the id of the row in kingres.bucket_move (kind = 'split').
kingres.split_bucket(colocation_id int, bucket int,
                     parts int DEFAULT 2,
                     verify text DEFAULT 'count')   -- 'count' | 'none'
  RETURNS bigint

-- Unchanged signatures, now also handle kind = 'split':
kingres.cancel_bucket_move(move_id bigint)
kingres.resume_bucket_moves() RETURNS int
kingres.cleanup_moved_buckets(force bool DEFAULT false) RETURNS int
kingres.check_cluster(p_prepared_older_than interval DEFAULT interval '5 minutes')

-- Replaces kingres.set_node_property(int, bool) (dropped and recreated by the
-- upgrade script, grants repeated; calls with two positional arguments still work).
kingres.set_node_property(node_id int,
                          should_have_shards bool DEFAULT NULL,
                          weight float8 DEFAULT NULL)
  RETURNS void       -- NULL means "leave unchanged"; at least one must be given

-- The plan with strategies. New name, because adding defaulted arguments to
-- rebalance_plan(int) would make rebalance_plan() ambiguous.
kingres.plan_rebalance(
    strategy      text    DEFAULT 'by_bucket_count',  -- or 'by_disk_size'
    threshold     float8  DEFAULT NULL,   -- NULL: 0 for by_bucket_count, 0.1 for by_disk_size
    max_moves     int     DEFAULT NULL,
    drain_only    bool    DEFAULT false,
    extra_load    jsonb   DEFAULT NULL,   -- {"<group_id>": bytes}: load of other databases
    exclude_groups int[]  DEFAULT NULL,   -- groups to treat as not receiving (a what-if drain)
    colocation_ids int[]  DEFAULT NULL)   -- restrict to these groups
  RETURNS TABLE (seq int, colocation_id int, bucket int, source_group int,
                 target_group int, cost bigint, size_bytes bigint, reason text)
                                          -- reason: 'drain' or 'balance'

-- Where the groups stand, for the preview and the status:
kingres.group_load(strategy text DEFAULT 'by_bucket_count',
                   extra_load jsonb DEFAULT NULL,
                   exclude_groups int[] DEFAULT NULL)
  RETURNS TABLE (group_id int, receiving bool, weight float8, buckets int,
                 load bigint, utilization float8, deviation float8)

kingres.bucket_range(colocation_id int, bucket int)
  RETURNS TABLE (modulus int, remainder int)
```

`kingres.rebalance_plan(max_moves int DEFAULT NULL)` keeps its signature and its five
result columns; from 0.7 it is a wrapper that calls `plan_rebalance('by_bucket_count',
0, max_moves)` and returns the first five columns. Existing callers and the first
increment of the control plane (section 12) are not affected.

`kingres.bucket_for(...)` returns the number of the bucket that covers the key (the
remainder in that bucket's modulus). `kingres.internal_attach_bucket(parent, bucket,
modulus)` and `internal_create_move_tables(..., p_modulus, ...)` receive the bucket's
modulus. New internal functions: `internal_split_prepare`, `internal_split_backfill`,
`internal_split_reconcile`, `internal_switch_split` (C, the counterpart of
`internal_switch_bucket`), `internal_split_capture` (the trigger function, section
3.2), `internal_split_undo`.

### 2.4 Settings

| GUC | Default | Meaning |
|---|---|---|
| `kingres_shard.split_batch_rows` | 5000 | Rows per backfill batch (one transaction each) |
| `kingres_shard.split_switch_attempts` | 20 | Attempts of the switch before the operation fails with the blocker named; replaces the unbounded retry of S4-04. The same bound applies to `move_bucket` as `move_switch_attempts` |
| `kingres_shard.max_bucket_modulus` | 65536 | Upper bound of `max_modulus` of a colocation group (checked by `split_bucket`) |
| `kingres_shard.move_cleanup_delay` | 10min | Also the delay for the orphan of a split |

`move_switch_timeout`, `move_cleanup_delay` and the fault-injection setting of phase 4
apply to splits unchanged.

### 2.5 What check_cluster adds

Rows: `split_trigger` (a capture trigger on a partition without a live operation),
`split_child_table` (a child table without a live operation), `operation_stalled` (an
unfinished move or split whose live mark is free and whose `updated_at` is older than
`2 * node_timeout`; closes the silence of S4-02), `bucket_layout` (the buckets of a
colocation group do not cover each slot exactly once, or a partition's bounds differ
from its `bucket_map` row), `extension_version` (a node reports another version).

## 3. Splitting a bucket

### 3.1 Preconditions (checked in `preparing`, again before the switch)

- The caller is a superuser on the map group primary; every active node reports the
  same extension version (S4-05).
- The bucket exists, is `active`, has no unfinished operation, and no other split of
  the colocation group is unfinished (one split per colocation group at a time; moves
  of other buckets may run).
- `parts` is 2, 4, 8 or 16 and `max_modulus * parts / modulus_of_bucket` does not
  exceed `max_bucket_modulus`. The power of two keeps the moduli a chain of
  divisors (section 1.1).
- The owning group has an active primary. `wal_level = logical` is **not** needed.
- Every table of the colocation group has a primary key (a split matches rows of the
  children by key). `REPLICA IDENTITY FULL` without a primary key is refused for a
  split (it is accepted for a move); tables with identity columns are refused as
  in phase 4; so is a foreign-key cycle.
- No DDL on the colocation group while the split runs, as for a move
  (`kingres_shard_assert_no_move` covers both kinds).
- Disk: the extension cannot see free space. `kingresd` checks it before it starts
  the task (section 8.6); the extension fails the operation cleanly on an error
  during the copy, and the undo drops the children.

### 3.2 The algorithm

The unit is the bucket of every table of the colocation group, as for a move. The
orchestration is PL/pgSQL; each durable action runs through `internal_node_exec` in a
session of its own, as in phase 4. Let `T` be the old partition `P_b` of one table,
`(m, r)` its bounds, `k` the number of parts, and `C_i` the child `i` with bounds
`(k*m, r + i*m)`.

1. **preparing** (map group primary, committed at once). Check the preconditions,
   take the cluster lock on `kingres.map_version`, set the bucket to `splitting` on
   every node in one atomic catalog change, insert the row of `bucket_move`
   (`kind = 'split'`, `parts`, `children` = the planned `(bucket, modulus)` pairs,
   `rows_total` from `pg_class.reltuples` of the partitions). Routing is unchanged.
2. **copying: children and capture triggers** (owning node). For every table of the
   group, in foreign-key order (`internal_move_tables`):
   - create each `C_i` as a standalone table `<table>_s<id>_<bucket>` (`LIKE parent
     INCLUDING ALL`: columns, defaults, constraints, all indexes) with
     `CHECK (satisfies_hash_partition('<parent>'::regclass::oid, k*m, r + i*m, <col>))`,
     the check phase 4 uses so that the later `ATTACH` needs no scan;
   - create on `T` an `AFTER INSERT OR UPDATE OR DELETE` row trigger and an `AFTER
     TRUNCATE` statement trigger, both `ENABLE ALWAYS`, generated per table
     (`internal_split_capture`). The trigger applies the change to the right child in the
     **same transaction** as the user's write: an insert becomes `INSERT INTO C_j
     SELECT NEW.* ON CONFLICT (<key>) DO UPDATE`; an update deletes the old key from the
     child that held it and inserts the new row into the child that covers it; a delete
     removes the key; a truncate truncates every child. The child is chosen with
     `satisfies_hash_partition`. `ENABLE ALWAYS` makes the trigger fire under
     `session_replication_role = replica` as well, so that a session that switched
     triggers off does not bypass it (`alter_table.sgml`: triggers configured `ENABLE
     ALWAYS` fire in every mode). The row trigger sits on the leaf partition, so a
     direct write into `T` is captured too: S4-03 cannot happen here.
   Creating the trigger takes SHARE ROW EXCLUSIVE on `T` (writers of that partition
   wait for it, with a lock timeout and retry), and nothing else.
3. **copying: backfill** (owning node). Per table, in key order, batches of
   `split_batch_rows`, each in its own transaction:
   ```sql
   INSERT INTO C_j
   SELECT * FROM (SELECT * FROM T WHERE (<key>) > (<last key>)
                  AND satisfies_hash_partition(...C_j bounds...)
                  ORDER BY <key> LIMIT <n> FOR SHARE SKIP LOCKED) s
   ON CONFLICT DO NOTHING;
   ```
   **[Needs verification]** that `FOR SHARE SKIP LOCKED` is accepted in that subquery
   form on 18.7 (otherwise a `SELECT ... FOR SHARE SKIP LOCKED` into a temporary
   array, then the insert). The row lock is what makes the copy exact against
   concurrent writers: a row locked by a writer is skipped and left to the trigger
   (or to the convergence pass); a row the copy has locked cannot be deleted or
   updated until the batch commits, so the copy cannot resurrect a row that a
   concurrent delete removed, and a later change finds the copied row in the child and
   changes it there. `SKIP LOCKED` also keeps the copy from waiting for a writer that
   in turn waits for the copy's uncommitted child rows (the cycle that would otherwise
   exist between the batch and the trigger); **[Needs verification]** by the
   interleaving tests of section 10. Progress is `rows_done`.
4. **catching_up: convergence.** Repeat the backfill over the whole key range until a
   pass skips no locked row and inserts no row (at most
   `split_converge_passes = 10` passes, then the operation fails with
   "writers keep rows locked" and is undone). Duplicates are impossible (`ON CONFLICT`),
   losses are caught by the pass, and stale rows by the verification below.
5. **switching** (owning node and map group, one two-phase transaction; the same
   shape as phase 4's switch without a target node and without a marker):
   - lock the parents of the group's tables on the owning node in SHARE mode (writers
     of every bucket of those tables on that node wait; readers go on), each lock
     waited for at most `max(200 ms, move_switch_timeout / 10)`;
   - **verify** (`verify = 'count'`): for each table `count(*)` of `T` equals the sum
     of the counts of the children, and each child holds only rows its check admits
     (guaranteed by the constraint). A difference runs one more convergence pass under
     the lock (writers are excluded, so it is exact) and verifies again; a second
     difference fails the operation (`detail` names the table) and undoes it. With
     `verify = 'none'` this step is skipped; the trigger and the passes are the only
     proof. The verification is an index-only count per table, so the time that
     writers wait is proportional to the number of rows of the bucket's
     partitions; **[Needs verification]** by measurement, which goes into the
     implementation notes;
   - detach the partitions: the old `T` is detached and renamed to an orphan in schema
     `kingres` (`orphan_<id>_<n>`, its indexes renamed as phase 4 does), its capture
     triggers and its foreign keys are dropped (so the orphan never writes into a
     child again);
   - rename the child that keeps the number `r` to `<table>_b<NNNN>` (its name was
     temporary because the old partition had that name), and attach every child with
     `FOR VALUES WITH (MODULUS k*m, REMAINDER r + i*m)`; the validated check makes the
     attach scan-free; with foreign keys the attach validates them (phase 4's limit;
     the children get the same constraints, validated before the switch, so that the
     attach finds them: **[Needs verification]** that 18.7 reuses an equivalent
     existing constraint of the child and does not rescan);
   - in the same transaction on every node: replace the bucket's row of `bucket_map`
     by the `k` child rows (state `active`, same group), raise
     `colocation.max_modulus` to `max(max_modulus, k*m)`, `placement_epoch + 1`, raise
     the map version, set the operation to `cleaning`. On the map group primary, when
     it is the owner, its catalog change runs in the coordinator's own transaction (rule
     (1) of phase 4's notes);
   - the owning node holds ACCESS EXCLUSIVE on the parents from the detach until its
     `COMMIT PREPARED` has returned (rule (2) of phase 4's notes).
6. **cleaning, done.** The orphan `T` is dropped after `move_cleanup_delay` by
   `cleanup_moved_buckets()`, as for a move.

### 3.3 What is blocked, and for how long

| Who | Blocked | For |
|---|---|---|
| Writers of any bucket of the group's tables on the owning node | at the SHARE lock of the switch | from the lock to the commit: the verification, the detach and attach (metadata), and the two-phase commit round trip; at most `move_switch_timeout` per attempt |
| Readers of those tables on the owning node | at the detach (ACCESS EXCLUSIVE) | only from the detach to the commit of the owning node |
| Writers of the bucket during the copy | not blocked | each write also writes the child (about twice the write cost and WAL for that bucket) |
| Everyone else, other nodes | not blocked | a statement that overlaps the commit may get `KS001` |
| DDL on the colocation group | refused | from `preparing` to `cleaning` |

An attempt that cannot get its locks, or whose verification exceeds
`move_switch_timeout`, rolls back, releases the locks and tries again after a random
pause; after `split_switch_attempts` attempts the operation fails with the pid and
query of the blocking transaction in `detail` (from `pg_locks`). A long transaction
that has the group's tables open on the owning node holds the switch back exactly as
it does for a move; the failure is bounded, no longer endless (S4-04).

### 3.4 Crash, failover and lost-node behaviour

Every phase is recorded before the action it announces, and every action is repeatable.

| Interrupted in | State left | Recovery (`resume_bucket_moves`, run by the recovery worker and the control plane) |
|---|---|---|
| preparing | bucket `splitting` or `active`, record maybe missing | the bucket state and the record are one transaction; `check_cluster` reports `bucket_moving_without_move` otherwise |
| copying, catching_up (session dead) | children, triggers, partial copy | **undo**: drop the capture triggers first, then the children, set the bucket `active`, record `failed`. The order matters: a trigger whose child is gone would fail every write of the bucket. The operation is then restarted from the beginning by whoever drives it. Continuing a half-done backfill is possible (the copy is idempotent) but the worker must not run for hours, so it only undoes |
| owning node down | the same, on disk | writes to its buckets fail anyway; when the node is back, the undo runs there. `internal_node_exec` times out after `node_timeout`, so the operation fails instead of waiting |
| failover of the owning group (phase 5) | children and triggers are in the WAL and are on the promoted standby | the operation's session is gone; the undo runs on the new primary. Unlike a move there is no logical slot to lose |
| switching | prepared transaction | decided by the two-phase commit as in phase 4: the phase becomes `cleaning` in the same transaction as the bucket map; a `switching` row after a crash is a switch that did not commit; resume waits `2 x node_timeout` before it undoes |
| cleaning | orphan on the owner | `cleanup_moved_buckets()` after the delay; the worker calls it (S4-02) |

A map group failover between phases leaves `kingres.bucket_move` and the catalog
intact (they are in the replicated map group); the control plane re-reads them.

### 3.5 Interaction with other mechanisms

- **Other moves.** A bucket with an unfinished operation cannot be split or moved. A
  move of another bucket of the same colocation group may run during the split; the
  two switches both want locks on the same parents on the same node and resolve it by
  the lock timeout and retry. The rebalancer (section 6) never plans a bucket that is
  `splitting`, never starts a move on a colocation group that has a split in its
  switch window, and the split of a bucket waits for no move of its own bucket
  because that is refused.
- **Map version and `KS001`.** The switch counts the map version up on every node and
  `placement_epoch` up for the colocation group. The group of every key is the same
  before and after a split (the children stay on the owner), so a statement that
  routes with the old catalog still reaches the right group; what the owner does with
  it is decided by PostgreSQL's own pruning over the partitions it has. A distributed
  scan or fan-out write compares the epoch (section 7.1) and gets `KS001` if a split
  committed in its window. A cached local plan is invalidated by the detach through
  PostgreSQL's relation invalidation.
- **Two-phase commit.** The switch is a two-phase transaction; user two-phase
  transactions that hold the group's tables on the owner delay it (prepared
  transactions keep their locks) and are resolved by the recovery worker, which also
  recovers the switch itself.
- **Reference tables.** Not split, not moved; every node has them and a split does not
  change them.
- **Ledger tables of `kingres_ledger`.** A ledger table cannot be distributed in the
  current designs ([shard-phase2b.md](shard-phase2b.md), section 11.8), so a split
  never touches one. The copy writes the children directly, not through the parent,
  so no ledger capture trigger of a parent fires twice. **[Needs verification]** when
  ledger tables over distributed tables are designed (phase 7): a split copies stored
  values verbatim, which preserves row hashes, but the block structure of a ledger is
  not examined here.
- **Standbys and failover.** Children, triggers and the final catalog change are
  ordinary WAL and reach the standbys of the owning group by physical replication, with
  no step of their own. The children double the write volume of the bucket during the
  copy; the standby's disk needs the room too (section 8.6). Durability waits for the
  synchronous standby as for any commit, so the switch is slower while a standby lags
  (section 8.4 checks the lag).

### 3.6 After the split

The two buckets are ordinary buckets. They share one group; a rebalance moves them
apart when the strategy asks for it. `by_bucket_count` treats every bucket as equal, so
it does not value a split; `by_disk_size` does. A split bucket cannot be merged back in
this phase (limit, section 14).

## 4. The strategy model

### 4.1 Items and groups

The unit of the plan is the pair `(colocation_id, bucket)` of one database. A shard
group `g` is a receiver when its primary node is active and has `should_have_shards`;
every other group that owns buckets is a drain source. Buckets that are `moving` count
as already at their target (read from the unfinished `bucket_move` row), buckets that
are `splitting` stay where they are and are not planned. Groups without an active
primary are neither receivers nor sources; their buckets are not planned and a warning
`groupUnavailable` is reported.

### 4.2 Cost of a bucket

- `by_bucket_count`: `cost(b) = 1`. Every bucket counts the same, split or not.
- `by_disk_size`: `cost(b) = max(size(b), min_bucket_cost)` where `size(b)` is
  `pg_total_relation_size` (table, indexes, TOAST) of the bucket's partitions of every
  table of the colocation group, read from the owning group's primary, and
  `min_bucket_cost` is 1 MiB (so that empty buckets are not free and do not pile up on
  one server). When a size cannot be read (a group does not answer) the plan stops with
  an error that names the group instead of guessing; a preview reports it as a warning
  and shows `estimatedSizeBytes` as null.

### 4.3 Capacity and the ability to hold shards

`capacity(g) = weight(primary node of g)`, a positive number, default 1. The control
plane keeps the weight per **server** (`Server.weight`, section 9) and writes it to every
node row of that server with `set_node_property`, as it already does for
`should_have_shards`, so that a failover to a standby on another server brings that
server's weight. A group is allowed to hold shards when its primary has
`should_have_shards`. Which groups may hold shards is therefore decided per server
by one flag, and a drain is the flag set to false.

Disk is a second, separate condition, enforced by the control plane and not by the
plan: a move is not started when the target server's data volume (and the volumes of
the target group's standbys) would exceed the limit of section 8.4. The plan does not
use free space as capacity because free space is not known to the extension.

### 4.4 Load, utilization, threshold

For the receivers `R`, with `extra(g)` the load of other databases passed in
`extra_load` (zero when planning one database alone):

- `load(g) = sum of cost(b) for buckets b owned by g, over all colocation groups`;
- `utilization(g) = (load(g) + extra(g)) / weight(g)`;
- `ideal = (sum over all groups of load(g) + sum over R of extra(g)) / sum over R of weight(g)`,
  so a drained group's load is counted as work to be distributed;
- `deviation(g) = abs(utilization(g) - ideal) / ideal` for `g` in `R` (0 when `ideal` is 0).

`threshold` is the largest `deviation` that is accepted. Nothing moves while every
receiver is within the threshold and no drain source holds a bucket. The defaults are 0
for `by_bucket_count` (counts differ by at most one after a run) and 0.1 for
`by_disk_size` (sizes are noisy and a move of a large bucket is expensive). The
automatic mode has its own, larger default (section 8.2).

### 4.5 The plan

The plan is computed by simulation, deterministic for a given state:

1. While some drain source holds an active bucket, or some receiver has `deviation >
   threshold`:
   - **source** `s`: the drain source with the greatest load, or, when none holds a
     bucket, the receiver with the greatest utilization;
   - **target** `t`: the receiver other than `s` with the least utilization (ties: the
     lowest group id);
   - **bucket**: the buckets of `s` in order of decreasing cost (ties: the higher bucket
     number first, as phase 4 picks the highest numbered surplus buckets), the first one
     that for a drain source always qualifies, and for a receiver source satisfies
     `abs(u_s' - u_t') < abs(u_s - u_t)` with `u_s' = u_s - c/w_s` and `u_t' = u_t + c/w_t`;
   - if no bucket qualifies, remove `s` from the candidate sources for the rest of the
     simulation; stop when there is none left.
   - Record the move and apply it to the simulated loads.
2. Cap at the number of buckets (so the loop always ends).
3. `reason` is `drain` for a move off a drain source, `balance` otherwise. The plan is
   ordered by `seq`; the order is the order of the simulation, which fills the least
   loaded receiver first and alternates between colocation groups through the cost
   ranking. Moves of different colocation groups are independent in the plan and in the
   scheduler (section 6.3).

With equal weights, `by_bucket_count` and threshold 0 the counts per group at the end of
the plan equal those of phase 4's `rebalance_plan` (differing by at most one, the
remainder left where the groups already hold most) and the number of moves is the same;
the order of the moves may differ. A pg_regress test compares the two over generated
maps, so that the wrapper of section 2.3 is justified.

### 4.6 Recomputation

The plan is a preview and a guide, not a script. The scheduler asks for the next moves
again before it starts each one (section 6.3), with the current state: buckets already
moving count as moved, completed moves are in the map, `extra_load` is read again, and
a resumed run starts from the state it finds. This keeps the plan correct when sizes
change during a long run, when a server is added or its flag changes, or when a move
failed.

### 4.7 Worked examples

**Example 1, by_bucket_count, a server is added.** One colocation group of 128 buckets
on groups 1, 2 and 3 (43, 43, 42 buckets), all weight 1; group 4 is added empty. Ideal
= 128 / 4 = 32.

- Threshold 0: the plan moves from the most loaded group to group 4 until counts differ
  by at most one: 32 moves (surplus 11 + 11 + 10), final 32, 32, 32, 32.
- Threshold 0.1 (window 28.8 to 35.2): the simulation stops when every receiver is
  inside the window. After 29 moves the counts are 33, 33, 33, 29 and every deviation
  is at most 0.1, so the plan has **29 moves**. After 28 moves group 4 has 28, which is
  outside the window, so the 29th move is still needed.

**Example 2, by_disk_size, threshold 0.15.** Groups A, B, C (C new), weight 1, one
colocation group. Bucket sizes in GB: A holds 12, 8, 6, 4 (load 30); B holds 10, 5, 3, 2
(load 20); C is empty. Ideal = 50 / 3 = 16.67; window 14.17 to 19.17.

- Move 1: source A (30), target C (0), the largest bucket that shrinks the gap: 12 GB
  (new utilizations 18 and 12). Now A 18, B 20, C 12: B deviates 0.2 and C 0.28.
- Move 2: source B (20), target C (12). The 10 GB bucket would give 10 and 22 (gap 12,
  larger than 8) and does not qualify; the 5 GB bucket gives 15 and 17 (gap 2) and does.
  Now A 18 (0.08), B 15 (0.10), C 17 (0.02): all inside the window.
- The plan has **2 moves, 17 GB**. The same cluster with `by_bucket_count` and threshold
  0.15: A 4 buckets, B 4, C 0, ideal 2.67; two moves give 3, 3, 2; the next move (3 to 2)
  would not shrink the gap (1 to 1), so the plan stops with 2 moves although C deviates
  0.25, and it has moved the largest buckets by number, not by size.

**Example 3, a drain with weights.** 128 buckets on A, B, C (43, 43, 42), by_bucket_count.
Drain C. With weights A = 2, B = 1 the receivers' ideal is 128 / 3 = 42.67 buckets per
unit of weight. A starts at utilization 21.5 and B at 43. Every move goes to the least
utilized receiver, which stays A until it has 85 buckets (utilization 42.5, still below
B's 43) after the 42 buckets of C are placed. Result: A 85, B 43; B receives nothing.
With equal weights the 42 buckets alternate: A 64, B 64.

**Example 4, a split.** Colocation group 1, base 128, bucket 17 on group 2, 40 GB over
its tables, `parts = 2`. Children: bucket 17 with modulus 256 (remainder 17) and bucket
145 with modulus 256 (remainder 145); `max_modulus` becomes 256; the map has 129 rows.
Group 2 needs about 40 GB of extra disk (children) until the orphan is dropped, 10
minutes after the switch by default. Writers of the group's tables on group 2's primary
wait during the switch only. If the group is then rebalanced by disk size, the two 20 GB
children can go to different groups.

## 5. Rebalancing in the extension

The extension provides, and the control plane uses:

- `kingres.move_bucket` (unchanged signature), with the changes of section 7 (epoch,
  bounded switch attempts, partition locks, worker resume);
- `kingres.plan_rebalance` and `kingres.group_load` (read only, any node; they read
  `bucket_move` where it exists, so the control plane calls them on the map group
  primary);
- `kingres.set_node_property(node_id, should_have_shards, weight)`;
- the split.

Sizes are read by `plan_rebalance` only for `by_disk_size` and only as a superuser (as
`rebalance_plan` does today: other roles get null sizes).

## 6. Background rebalancing in `kingresd`

### 6.1 The task

A rebalance is one task of kind `rebalance`, subject type `cluster`, subject id
`cluster` (the code already counts unfinished tasks of kind `rebalance`: `facts.go`,
`servergroups.go`), so there is one rebalance at a time, cluster-wide, as the contract
says. Steps (names are shown to users):

1. `Check that the cluster can rebalance`: the safety rules of section 8 for a manual
   start (a refusal is a 409 before the task exists; the step repeats the check),
   extension version equal on every node, `wal_level`, replica identities (plan
   warnings), `check_cluster()` without rows of a kind that makes moving unsafe.
2. `Compute the plan`: per database with `kingres_shard`, call `plan_rebalance` on that
   database's map group primary with the options, passing `extra_load` (the other
   databases' load per group, from their `group_load`), and record the run in
   `kingres_cp.rebalance_run` and the planned moves in `kingres_cp.rebalance_move`
   (idempotent: a run row that exists is kept).
3. `Move buckets` (a long step, section 6.3).
4. `Write the report`: totals, outcome, and the audit event.

The engine's step contract is at-least-once; step 3 is idempotent because everything it
does is recorded in `rebalance_move` and in the extension's `bucket_move` before it
acts. No step is marked `NonIdempotent`.

**Engine change.** The state `paused` is declared in `internal/task/task.go` and counted
as active, and the contract promises it, but no code sets it (searched). The engine
gets a sentinel `task.ErrPause{Reason}` that a step returns: the engine records the task
`paused`, releases the lease and keeps the steps already done; `resume` sets it to
`pending`. `POST /rebalance` resumes a paused rebalance task. Also, `facts.go` and
`servergroups.go` count a paused rebalance as "running" and so refuse `PATCH /servers`
and `DELETE /servers/{id}` with `rebalance_in_progress`; a **stopped** rebalance must
not block changing the drain flag of a server (it is exactly what an operator does
between a stop and a resume), so only `pending` and `running` count for those refusals
and a resume recomputes the plan. (Section 13 lists this as a correction.)

### 6.2 Stop, resume, cancel

- `stopRebalance` sets `stop_requested` on the run. The scheduler starts no new move;
  moves in flight finish (a move cannot be interrupted safely after its switch started;
  before the switch the control plane could cancel it with `cancel_bucket_move`, but a
  graceful stop waits). When none is in flight the step returns `ErrPause`; the status
  is `stopped` with `stoppedBy: operator`. `GET /rebalance` shows it as `stopped`.
  A stop asked while moves copy large buckets can take as long as those moves; the
  status shows `stopping` meanwhile, as the contract has it. A second stop request with
  `force: true` (additive field) cancels moves that have not reached `switching`.
- `startRebalance` on a stopped run resumes it: same options unless new ones are given,
  the plan recomputed.
- `cancelTask` on the task ends the run as `failed` with code `cancelled`; moves in
  flight are cancelled the same way as a forced stop.

### 6.3 The scheduler (step 3)

State per run: the list of planned moves, the in-flight set, per-bucket attempt counts.
Loop, once a second:

1. Reap: for each in-flight move, read its `bucket_move` row on the map group primary.
   `done` or `cleaning`: the move counts as done and the size is recorded. `failed` or
   `cancelled`, or the connection that runs the move is gone and `internal_move_alive`
   is false: call `resume_bucket_moves()`, record the failure and the error text, add 1
   to the bucket's attempts.
2. Stop conditions: `stop_requested`, a safety rule that now fails (section 8.3 for an
   automatic run), or a cancelled task.
3. Start: while `in_flight < maxConcurrentMoves`, ask for the next candidates with
   `plan_rebalance(..., max_moves => 2 * maxConcurrentMoves)` (the in-flight moves are
   accounted by the plan) and take the first that satisfies the concurrency rules and the
   per-move safety rules:
   - `maxConcurrentMoves` moves in the cluster (default 4, from the cluster setting);
   - `maxMovesPerServer` moves that involve a server as source or target (default 2);
   - **no two moves of one colocation group share a source group or a target group**
     (their switches would lock the same parents on the same node; moves of one
     colocation group between disjoint pairs of groups run in parallel);
   - the bucket has fewer than `maxMoveAttempts` (default 3) failures in this run; a
     bucket that failed is retried after a backoff of 30 s times the number of failures
     and meanwhile other buckets are tried (S4-04 makes a blocked switch fail instead of
     retrying for ever);
   - the target server has disk room (section 8.4);
   - a move whose plan entry no longer reflects the state (the bucket is busy, the groups
     changed) is dropped and the next candidate is taken.
   Before the call the scheduler writes the `rebalance_move` row as `starting` in a
   transaction that checks the task's lease and epoch, so a leader that lost its lease
   cannot start moves it has not recorded; then it opens a dedicated connection to the
   map group primary and calls `SELECT kingres.move_bucket(c, b, g)`.
4. End: no in-flight move and no candidate (all done, skipped or failed): the step ends.
   Moves that were never started because of a rule are recorded `skipped` with the reason.
5. Progress: every second, update the task's percent (bytes moved over bytes planned for
   `by_disk_size`, moves over moves for `by_bucket_count`) and the per-move phase and
   percent in `rebalance_move` for `inFlightMoves`.

**Percent of a move.** The phase comes from `bucket_move.phase`; the percent in the copy
phase is the size of the target's standalone copy over the source size (both read with
`pg_total_relation_size` through the existing node-exec call); the other phases map to
fixed values (`preparing` 0, `catching_up` 90, `switching` 95, `cleaning` 100).
**[Needs verification]** that the size of the target's tables grows steadily during the
initial table copy of logical replication; if not, the percent is a phase estimate only.
For a split the percent is `rows_done / rows_total`.

### 6.4 Crash of the leader, and fencing

The task lease and epoch of the engine fence the control plane's own state: a store
accepts a write of a task only from the holder in the current epoch, and the system
database lives on the map group, so a leader that is not (or no longer) the primary of
the map group cannot write at all. A new leader claims the task after the lease expired
and re-enters step 3. It finds the `starting` and `running` rows, reads `bucket_move`,
and waits until each unfinished move's live mark is free (the dead leader's
connections end by the TCP user timeout of `node_timeout`) before it calls
`resume_bucket_moves()`; a move past the switch is counted done. A zombie leader that
froze after its lease check and then starts one move can, at worst, start a move that
the plan also wants: "one move per bucket" (the unique index) makes the second start
fail, harmless. This is the same fencing argument as
[ADR 0021](../adr/0021-failover-in-kingresd.md) and
[ADR 0018](../adr/0018-manager-leader-follows-the-map-group-primary.md); it adds no
lease of its own.

### 6.5 Reports

Tables in `kingres_cp` (a migration after `0001_init`; the number is the next free
one): `rebalance_run (task_id pk, trigger, strategy, threshold, options jsonb,
started_at, finished_at, outcome, stopped_by, stop_reason, bytes_planned,
bytes_moved)` and `rebalance_move (task_id, seq, database_name, colocation_id, bucket,
from_group, to_group, reason, state, attempts, op_id, size_bytes, started_at,
finished_at, phase_seconds jsonb, error)`. The report of the contract is read from
them; `id` equals the task id as the contract requires. Reports are kept for 180 days
or the last 200 runs (settings), whichever keeps more; a move row holds the text of its
last error cut at 500 bytes, like other peer errors.

## 7. Holes of version 0.3 that this phase must not inherit

The review of version 0.3 found defects that an automatic rebalancer would make likely.
The design answers each. **Increment 0** (section 12) is to confirm, in the tree at the
time, that each fix exists, and to make it if it does not; the rebalancer is not
enabled before it.

### 7.1 S4-01: the placement hash is blind to a bucket that moved away and back

The scan compares a hash of `(bucket, group)` before and after its `DECLARE`. A bucket
that moves S to T and back inside that window restores the hash and a bucket is
missed. Fix: `kingres.colocation.placement_epoch`, a counter that only the switch (of a
move or of a split) increments, in the same transaction that changes the node's
partitions. The value `kingres.internal_placement_hash` returns includes the epoch
(`hash(epoch, placement)`), and the origin's cached value includes it; a counter never
returns to an earlier value, so two switches inside the window always change the
number. The epoch also covers a split, whose children share a group and whose placement
hash would otherwise change in a way that a scan could not tell from a move. Tests:
the sequence of the review (move away, scan resumes after the move back) with the
existing fault points; the same with a split in the middle; a scan across two moves on
a *remote* pair of groups (the review's unconfirmed second item).

### 7.2 S4-02: the recovery worker does not resume or clean up

The worker on the map group primary calls `resume_bucket_moves()` and
`cleanup_moved_buckets()` after `recover_prepared_transactions()` (bounded, skipping
live operations; both functions already skip live ones). `check_cluster()` reports a
move or split that is not alive and is older than `2 * node_timeout`
(`operation_stalled`). The control plane also calls `resume_bucket_moves()` when it
finds a move whose connection it lost. Tests: kill the session of a move and of a split
and assert that the worker undoes them (and no slot, trigger or child is left); a
finished move is cleaned by the worker.

### 7.3 S4-03: a write into a bucket partition is not covered by the marker proof

For a split the capture trigger is on the leaf partition and fires for direct writes
(section 3.2). For a move, the switch locks, after the parents, **every bucket partition
of the move in SHARE mode** (a write to the partition conflicts), and the subscription is
not dropped until a marker committed after the last lock release has reached the target.
A direct DML on a bucket partition outside the extension's sessions is also refused when
the cluster has several groups (a check in the planner hook; **[Needs verification]**
that the hook sees a statement that names a partition, which is a utility check today for
DDL only). Test: the review's reproduction (subscription disabled to force lag, a
direct write held across the switch) must keep the row.

### 7.4 S4-04: the switch retries without bound and stalls writers and readers

Both `move_bucket` and `split_bucket` count attempts (`*_switch_attempts`, default 20),
back off with a growing pause (0.2 s, doubling to 5 s, with jitter), and fail with the
blocking transaction's pid and query in `detail`. The control plane treats that failure
as retryable for another bucket and the failed bucket as deferred (section 6.3). The
design keeps the documented cost (writers of all buckets of those tables on the node
wait for an attempt) and adds it to the user documentation of both operations, with the
reader stall at the detach.

### 7.5 S4-05: no version handshake

`move_bucket`, `split_bucket`, `plan_rebalance`, and the control plane's step 1 refuse
to run unless every active node reports the same extension version
(`internal_local_state()` carries it), with an error that names the node and both
versions. The new library functions called on a remote node that lacks them raise
"node X has kingres_shard <v>: run ALTER EXTENSION kingres_shard UPDATE" instead of the
undefined-function error. The upgrade order is documented: update the library everywhere
(it needs a restart), then `ALTER EXTENSION ... UPDATE` on every node, then sharded
statements and rebalancing are allowed again; the console shows the alert `versionMismatch`
that exists.

### 7.6 Smaller items of the review

S4-09 (a move pins the map group primary's xmin for its whole run): the long waits run in
the control sessions through `internal_node_exec`; the caller keeps one snapshot, and the
documentation says so; moving the wait out of the caller (return after recording, poll)
is deferred (section 14). S4-06 and S4-07 (helper functions and `bucket_move` readable by
every role): `bucket_move` is readable by superusers and by a `kingres_ops` role through
the control plane only; `internal_placement_hash` ignores an OID that is not a
distributed table. S4-08 (a racy test): the new tests start their interference at a fault
point, never at a phase name.

## 8. Automatic rebalancing and its safety rules

### 8.1 Where it runs

In the manager leader only: the `kingresd` of the server that holds the primary of the
map group (`internal/leader`). A background job evaluates every
`checkIntervalSeconds` (default 60). It starts a rebalance by creating the same task as
`startRebalance` with `createdBy = system` and `trigger = automatic`. The engine's
one-task-per-subject rule makes a second start by a second leader impossible; a job that
sees it is not the leader does nothing.

### 8.2 When it starts

Automatic mode is a cluster setting, **off by default** (as automatic failover is), and
the console explains the requirements when it is switched on. All of these must hold:

1. The setting is on, and the current time is inside `window` when one is set.
2. `plan_rebalance` with the automatic mode's strategy and threshold is not empty.
   The automatic threshold has its own setting, default 0.15, so that it does not
   churn; for `by_bucket_count` this is also the smallest deviation worth a run.
3. The imbalance has persisted: the condition of 2 has held for `settleSeconds`
   (default 300), and for `settleAfterTopologySeconds` (default 600) after the last
   topology event (a server added, removed, a flag or weight changed, a failover). The
   timers are derived from the database (`server.joined_at`, the audit and task times),
   not from memory, so a new leader does not restart them.
4. At least `minIntervalSeconds` (default 3600) have passed since the last automatic
   run ended, except that a new server in status `pendingRebalance` or a drain flag
   waives it.
5. No safety rule of 8.3 blocks.

### 8.3 When it must not start, and when it must pause

An automatic run does not start, and a running automatic run **pauses** (starts no new
move, lets the moves in flight finish, status `stopped` with `stoppedBy: safetyRule` and
`pausedReason`), when any of these is true. A manual run is subject to the rules marked
(M) and refuses to start with a 409; the others it reports as warnings.

| Rule | Condition | Problem code (manual start) |
|---|---|---|
| Failover (M) | a failover or switchover task is unfinished, or any shard group changed primary in the last `failoverSettleSeconds` (default 600), or the map group is failing over | `operation_in_progress`, `map_group_unavailable` |
| Group without a healthy standby (M) | `replicaCount > 0` and a group that is a source or a target of a move (automatic: any group with buckets) has `health != healthy` in `ShardGroup`, or its standby lags by more than `maxStandbyLagBytes` (default 64 MiB) | `group_degraded` |
| Group without a primary | any group with buckets is `unavailable` | `group_degraded` |
| Servers (M) | a server is `dead` or `suspect`; a `server.add`, `server.remove`, replica-count or restore-preferred-primaries task is unfinished | `operation_in_progress` |
| A failed move | an earlier move failed `maxMoveAttempts` times in this run or the last automatic run, and an operator has not acknowledged it (a manual start acknowledges it) | automatic only: suspended, alert `rebalanceFailed` |
| Disk on the target | after the move the data volume of the target server, or of any server that hosts a standby of the target group, would be above `maxDiskUsedFraction` (default 0.85), or less than `2 x` the bucket size would remain free | `insufficient_disk` (the move is skipped, not the run) |
| Cluster consistency (M) | `check_cluster()` reports `catalog_differs`, `bucket_layout`, `extension_version`, `operation_stalled`, `prepared_transaction`, or a node is unreachable | `conflict` with the rows in the problem detail |
| Prerequisites (M) | `wal_level` is not `logical` on a node of a move, a table without a replica identity | `unsupported_operation` naming the setting or the table |
| Another operation on the colocation group | a split is in its switch window | the move is deferred |

A paused automatic run resumes by itself, with a recomputed plan, when the rules hold
again for `settleSeconds`. A manual run that was stopped by a rule stays stopped until an
operator resumes it. Rules that depend on a clock (the settle times) use the database's
time, as the task engine's leases do.

### 8.4 Disk

`kingresd` reads the free and total bytes of each server's data volume and of the volumes
of its standby instances from the resource data it already gathers (the contract's
`ServerResourceSummary`; **[Needs verification]** that it carries free bytes of the data
directory's volume, and per instance). The bucket size is known from the plan. A move
adds the bucket's size to the target group's primary and to each of its standbys. On the
source side the bucket's space is freed only after `move_cleanup_delay`, when the orphan
is dropped, so a rebalance that is meant to relieve a full disk is slow to show it; the
run option `cleanupDelaySeconds` (additive; default the GUC) lets an operator shorten the
delay for one run by calling `cleanup_moved_buckets(true)` for that run's own moves only
after the moves' readers are known to have finished (the control plane waits for
`pg_stat_activity` on the source to show no transaction older than the switch). A move
also retains WAL on the source through its logical slot; the existing alert on retained
WAL applies.

### 8.5 Concurrency limits

The cluster setting `rebalance.maxConcurrentMoves` (existing, default 4, 1 to 64),
`maxMovesPerServer` (new, default 2) and the colocation-pair rule of section 6.3. Splits
are not part of the rebalance task and do not count against these limits; a split is its
own task (section 9.3) and at most one runs per colocation group.

### 8.6 Splits and the safety rules

A split is manual only. It checks the failover rule, the group-health rule for the
owning group, the consistency rule and, from the disk data, that the owning server and its
standbys have free room for `1.2 x` the bucket size plus the reserve of `maxDiskUsedFraction`.

## 9. Control plane and contract

### 9.1 Tasks and subjects

| Task kind | Subject | Steps |
|---|---|---|
| `rebalance` | `cluster` / `cluster` | section 6.1 |
| `bucket.split` | `bucket` / `<database>/<colocationId>/<bucket>` | `Check that the split can start`; `Split the bucket` (calls `split_bucket`, polls `bucket_move` for progress); `Wait for the old partitions to be dropped` (optional, until cleaning is done); `Write the record` |
| `server.drain` | `server` / `<serverId>` | section 11 |

`bucket.split` and `server.drain` use the engine's one-task-per-subject rule (the first
by bucket, the second by server); the cluster-wide rebalance lock (`rebalance_in_progress`)
is also taken by `server.drain` because it runs moves, and by `bucket.split` in the sense
that a split is refused while a rebalance has a move on the same bucket.

### 9.2 Additions to the six existing operations (additive, contract 0.7.0)

No operationId, path, required role or existing property changes. New optional
properties and enum values; clients must tolerate them (the contract already says so
for some enums; the panel's `tolerant()` helper does).

`RebalanceOptions` (request of `startRebalance` and `previewRebalancePlan`, also the
`options` of status and reports):

| Property | Type | Meaning |
|---|---|---|
| `strategy` | `byBucketCount` or `byDiskSize` | default: the cluster setting, initially `byBucketCount` |
| `threshold` | number 0 to 1 | default: the strategy's default |
| `drainOnly` | boolean | move only from servers that should not hold shards |
| `maxMovesPerServer` | integer 1 to 16 | |
| `acknowledgeFailures` | boolean | clears a suspension by failed moves (manual start only) |
| `cleanupDelaySeconds` | integer | per-run override of the orphan delay |

`RebalancePlanRequest` adds `excludeServerIds` (array of `ServerId`: plan as if these
servers should not hold shards, the preview of a drain). `PlannedMove` adds `cost` and
`reason` (`balance` or `drain`). `RebalancePlan` adds `strategy`, `threshold`,
`imbalance` (the largest deviation now) and `serverImpacts[]` gets `loadBefore` and
`loadAfter` (bytes or buckets, by strategy) next to the primary counts; `warnings`
carries `groupUnavailable`, `targetDiskLow` and the unsupported-table warnings.

`RebalanceStatus` adds `strategy`, `threshold`, `trigger` (`manual`, `automatic`,
`drain`), `stoppedBy` (`operator`, `safetyRule`, `moveFailed`, null while running),
`pausedReason` (text, null), and `automatic`: `{enabled, nextCheckAt,
lastEvaluation: {at, outcome: started | nothingToDo | blocked | settling, blockedBy[]}}`.
The `state` enum is unchanged: a stopped run, whether by an operator or a safety rule,
is `stopped`, distinguished by `stoppedBy`. `inFlightMoves[].phase` keeps the enum;
`BucketMove` adds `kind` (`move` or `split`).

`RebalanceReportSummary` adds `trigger`, `strategy`, `threshold`, `stoppedBy` and in
`totals` `movesSkipped`; `outcome` keeps `completed`, `stopped`, `failed`. The moves of
`RebalanceReport` add `reason`, `attempts`, `phaseSeconds` and, for `skipped`,
`skipReason` (`targetDiskLow`, `groupDegraded`, `bucketBusy`, `planChanged`,
`attemptsExhausted`).

`ClusterSettings.rebalance` and `ClusterSettingsUpdateRequest.rebalance` (role
`kingres_admin`, as now) add `strategy`, `threshold`, `maxMovesPerServer`,
`maxMoveAttempts`, `maxDiskUsedFraction`, `maxStandbyLagBytes`, and
`automatic: {enabled (default false), strategy, threshold (default 0.15),
checkIntervalSeconds, settleSeconds, settleAfterTopologySeconds, minIntervalSeconds,
failoverSettleSeconds, window: {from, to, timeZone} or null}`.

`RebalanceReason` adds `weightChanged`. `ServerUpdateRequest` adds `weight` (number
greater than 0, at most 1000; closed request, so it is a new known member), and `Server`
adds `weight`.

### 9.3 New operations

All under `/api/v1`, each with the usual shared responses.

| Operation | Method and path | Role | Answer |
|---|---|---|---|
| `previewBucketSplit` | `POST /databases/{databaseName}/colocation-groups/{colocationId}/buckets/{bucket}/split-preview` (body `{parts}`) | `kingres_readonly` | 200 `BucketSplitPreview`: children `(bucket, modulus, remainder)`, estimated size per child, the extra disk needed on the owning server and its standbys, the blockers found (busy bucket, no primary key table, group degraded, not enough disk) |
| `splitBucket` | `POST .../buckets/{bucket}/split` (body `{parts (2,4,8,16, default 2), verify (count, none)}`) | `kingres_admin` | 202 `Task` kind `bucket.split` |
| `drainServer` | `POST /servers/{serverId}/drain` (body `{removeWhenDone, maxConcurrentMoves, acceptFewerReplicas}`) | `kingres_ops` | 202 `Task` kind `server.drain` |
| `previewServerDrain` | `GET /servers/{serverId}/drain-preview` | `kingres_readonly` | 200 `DrainPreview`: moves, bytes, standbys to relocate and where, whether the replica count can be kept, blockers |

`splitBucket` needs `kingres_admin` and not `kingres_ops`: a split is permanent (no
merge) and changes the catalog layout; moves and drains are reversible by moving again.
Both a stopped and a running rebalance are compatible with a split of an idle bucket.

Additions to schemas: `ColocationGroup` adds `baseBucketCount`, `maxModulus` and
documents `bucketCount` as the current number of buckets (it already says "number of
buckets"); `Bucket` adds `modulus` and `remainder` (equal to `bucket`), and its `bucket`
description changes from "`hash(key) % bucketCount`" to "the remainder of the hash modulo
`modulus`"; `BucketState` adds `splitting`.

### 9.4 New problem codes

Added to the closed list of `Problem.code` and to the `Conflict` description:
`bucket_busy` (409: the bucket has a move or split), `group_degraded` (409: a group a
move or split would touch has no healthy standby, or has none at all, or is
unavailable), `insufficient_disk` (409), `no_receiving_server` (409: a drain or a flag
would leave no server that should hold shards), `replicas_cannot_be_kept` (409: a
drain would leave fewer servers than `replicaCount + 1`; `acceptFewerReplicas` overrides),
`split_unsupported` (422: a table without a primary key, an unsupported kind). The
existing `rebalance_in_progress`, `operation_in_progress`, `map_group_unavailable`,
`server_owns_buckets` and `server_hosts_map_group` keep their meaning.

### 9.5 Roles

`kingres_readonly`: status, previews, reports, bucket lists. `kingres_ops`: start, stop,
drain, flag and weight changes. `kingres_admin`: split, automatic mode and the rebalance
settings, because they change what the system does by itself and permanently.
`kingres_stats` sees none of these pages (it lands on the dashboard).

## 10. The unsupported cases and what each statement sees

- A single-key `SELECT`, `UPDATE`, `DELETE` or `INSERT` routed to the owner during a
  split: runs normally; writers wait during the switch only.
- A distributed scan in flight when a split commits: its cursor on the owner holds ACCESS
  SHARE on the parent, which delays the detach until the scan's transaction ends (so the
  scan sees the old partition entirely); one that opens after the commit sees the
  children; one that overlaps the commit window fails with `KS001` through the epoch
  check. No bucket is read twice or not at all, because the old partition and its
  children are never attached together.
- A fan-out write: compares the epoch after it ran, as in phase 4.
- A statement on a node whose catalog is stale (the split's catalog change reached it
  later): it routes to the same group; the owner prunes by its own partitions.
- After a later move of one child to another group, a statement from a stale node is
  routed to the old owner, which has no partition for the key and raises `KS001` (a
  forwarded session) or the tuple routing error (a direct insert), and the origin reloads
  the catalog and retries once, as in phase 4.

## 11. Draining a server

### 11.1 What a drain is

A drain makes a server removable: its shard group owns no bucket, no other group depends on
a standby it hosts, and the existing `removeServer` then succeeds (its checks, `holds()`
for primary and replica copies, `server_hosts_map_group`, `last_server`, stay as they
are). The console's model is Couchbase's: a node is marked, a rebalance moves the data
away while the cluster stays in service, and the node is removed afterwards. A drain
does **not** move data that is not sharded (section 11.5).

### 11.2 The two ways

- By hand, with the six rebalance operations and the flag (the contract's existing text):
  `PATCH /servers/{id}` `shouldHaveShards: false`, then `POST /rebalance` with
  `drainOnly: true` (or any rebalance, since the plan drains non-receivers first), then
  `DELETE /servers/{id}`.
- In one operation, `POST /servers/{id}/drain`, which does the same and adds the standby
  relocation and the checks. It is the button of the console.

### 11.3 The task `server.drain`

Input: the server id, `removeWhenDone`, `maxConcurrentMoves`, `acceptFewerReplicas`.
Steps:

1. `Check that the server can be drained`: refusals before the task exists, checked
   again here: 404 unknown server; 409 `last_server`; 409 `no_receiving_server` (no
   other server with the flag true and an active primary); 409 `replicas_cannot_be_kept`
   unless accepted: the servers that remain after the drain are fewer than `replicaCount
   + 1` (the chained rule places no two instances of a group on one server); 409
   `group_degraded` for a source or target group, the failover rule, the disk rule for
   the receivers (the whole drain size, not only the next move); 422 `node_state_invalid`
   for a `dead` server (fail it over first, as `removeServer` says); 409
   `operation_in_progress` for an unfinished task of this server.
2. `Stop new buckets from landing on the server`: `set_node_property(node, false)` on
   every node row hosted on the server, primary or standby (one atomic catalog change per
   database). `create_distributed_table` already spreads a new colocation group only over
   receivers.
3. `Move the buckets away`: a rebalance run (trigger `drain`, `drainOnly` true, strategy
   from the settings) under the same scheduler, sharing its reports. The run is the same
   task machinery inside this step: the report has the id of the drain task. The rebalance
   lock is held for the duration. If a failover moves the group's primary off this server
   meanwhile (a standby elsewhere promoted), the group is no longer on the server, which
   is correct, and the step re-reads the topology each loop.
4. `Hand over the map group` (only when the server hosts the primary of group 0): a
   graceful switchover of group 0 to a standby on another server (phase 5), so that the
   server is no longer the manager leader. The task continues after the leader changes
   (the engine's lease takeover, ADR 0018). **[Needs verification]** against the phase 5
   implementation: both that a graceful switchover of group 0 is an operation `kingresd`
   exposes, and that a task of the old leader survives it. If not available, `drainServer`
   answers 409 `server_hosts_map_group` for such a server at step 1 and the limit of
   section 14 stands. **Built, see section 23:** both hold, with a pause of the drain around the switchover.
5. `Relocate the standbys hosted here`: for each shard group `g` whose standby instance
   runs on this server (chained placement: this server hosts the standby of the group
   whose primary is on the previous server, and of the one before it when
   `replicaCount > 1`): recompute the ring placement over the servers that remain, create
   a new standby for `g` there with `pg_basebackup` and a physical slot, wait until it is
   streaming and in the group's synchronous list, then remove the old standby on this
   server. Make before break: the group never has fewer healthy standbys than before,
   except where `acceptFewerReplicas` was given and no place exists. This reuses the
   replica-count task's machinery of [ha-phase5.md](ha-phase5.md) ("Changing
   `replicaCount` is a task that adds or removes standbys group by group"); it is not
   written here again. A group whose standby cannot be placed because every remaining
   server already hosts an instance of it stays at fewer standbys, reported in the task
   detail and as `groupUnderReplicated`.
6. `Remove the group of this server`: the shard group of the server owns no bucket now;
   `kingres.remove_node` for its primary and its standbys on other servers, and the
   standbys' instances are deleted (they existed only to protect this group). Reads
   `holds()` again; any remaining bucket (a table created during the drain) sends the task
   back to step 3 once, then fails with `server_owns_buckets` and the databases named.
7. `Remove the server` (only if `removeWhenDone`): the existing `server.remove` task's
   steps, started as a child so that its own checks and certificate revocation run
   unchanged. Otherwise the server stays, status `draining` until it has no instances of
   shared groups, then `live` with `shouldHaveShards = false` and no buckets, ready for
   `DELETE`.

Cancel: `cancelTask` stops the moves as in 6.2 and leaves the flag false; the server
stays `draining`. **Return to service** is `PATCH shouldHaveShards: true` once no drain
task is running; a later rebalance then fills it like a new server.

### 11.4 Status of the server during and after

`ServerStatus` is derived as the control plane does today: `draining` is "should not
hold shards while holding buckets" (the README's rule), and stays so until the buckets
are gone; `decommissioning` is reserved for the running `server.remove` task. The
contract's text contradicts itself here: the status description says `decommissioning` is
"rebalance away from it is running" and the `updateServer` description says "`draining`
because of a removal, `decommissioning`". This design uses the README's rule and the
contract description of `ServerStatus` should be corrected in 0.7.0 (section 13).

### 11.5 What a drain does not move

- Local tables of ordinary databases and plain databases (the control plane creates
  every database on every server, but their data is per instance): `previewServerDrain`
  lists the databases in which the server's instances hold user tables outside
  `kingres.dist_table`, with sizes, and `drainServer` requires the request field
  `acknowledgeLocalData: true` when there are any. **[Recommendation]** exactly this,
  because deleting the instance deletes that data.
- The system database and the map group (step 4 and the limit of section 14).
- Reference tables: every node has all of them; nothing to move.
- Ledger tables are local tables and are in the previous item. A ledger on a removed
  server leaves with it; `previewServerDrain` names ledger tables (found through
  `kingres_ledger`'s catalog) so that the operator exports a digest first.
  **[Needs verification]** the catalog table to read.

### 11.6 Failure behaviour

A drain survives a crash of the leader (it is a task; steps 3 and 5 are idempotent by
construction: moves are recorded, standby creation checks what exists). A failed move
suspends the drain run like any run; the console shows the failure and "Retry". A server
that goes down during a drain: moves that involve it fail and are retried after it
returns or the group fails over; the drain does not continue past step 3 until no bucket
is on the server, so a dead source is never "completed" by removing it (`removeServer`
refuses a dead server anyway).

## 12. The web console

The console is a client of the contract only. Pages exist today: `RebalancePage`
(`/rebalance`), `ShardsPage`, `ServersPage` and `ServerDetailPage` with the
`ServerActions` dialogs. The architecture says "Shards page"; to avoid a second, parallel
screen the controls for strategy, threshold, automatic mode, progress and history stay on
the existing Rebalance page, reached from a status strip at the top of the Shards page
("Rebalance: idle, 3 servers, balanced" with a link) and from the Dashboard's existing
rebalance notice. The split is on the Shards page. The drain is on the Servers page and
the server's detail page. The mock API implements every new operation, tasks that
progress and then change the mock state, as it does for the six existing ones.

### 12.1 Shards page: split

- In the bucket detail panel (`data-testid="bucket-detail"`), for a user whose role
  allows `splitBucket`, the button **Split bucket**. Others see the bucket without it;
  a read-only user sees a disabled button with the text "Splitting buckets needs the
  administrator role" (the `GateButton` pattern).
- Dialog **Split bucket 17 of colocation group 1** (database shown): fields **Number of
  parts** (2, 4, 8, 16), **Verification before switching** (`Count rows` default, `None`
  with a warning text), read-only list of the resulting buckets from
  `previewBucketSplit` (number, modulus, estimated size), the disk needed on the owning
  server and its standbys, and a notice: "Writes to the tables of this colocation group on
  server X wait briefly while the split switches. Schema changes on these tables are
  refused until it finishes. A split cannot be undone." The confirm button reads **Split**.
  Blockers from the preview are shown as an error list and disable it.
- After submit the dialog closes, a task row appears in the panel with the phase and
  `rows_done / rows_total` as a progress bar (`role="progressbar"`, with an `aria-label`
  naming the bucket) and the bucket grid shows the bucket as `splitting` (a new state
  tone with an icon and text, not colour alone). When done the grid shows the children.
- Failure states: a failed task shows its error and "Nothing was changed: the copies were
  removed" (the undo's result); a task that cannot start shows the problem (`bucket_busy`,
  `group_degraded`, `insufficient_disk`, `split_unsupported`) with a plain explanation.

### 12.2 Rebalance page: strategy, threshold, automatic, progress, history

- **Options** card: **Strategy** (`By bucket count`, `By disk size`, with a one-line
  explanation each), **Threshold** (a percentage field with the strategy's default as
  placeholder, "Nothing moves while every server is within this share of the average"),
  **Concurrent moves**, **Moves per server**, a checkbox **Only move buckets off servers
  that should not hold shards** (`drainOnly`). **Preview plan** shows totals, the
  per-server before and after bars (buckets or bytes), the first moves and the
  warnings.
- **Automatic rebalance** card (visible to everyone; the switch is enabled for
  `kingres_admin`): **On / Off** switch (off by default), the strategy and threshold for
  it, **Wait before starting** (settle seconds), **Allowed window** (optional), and the
  last evaluation: "Checked 14:02: waiting, the imbalance has lasted 3 of 5 minutes",
  "Blocked: group 2 has no healthy standby", "Started 14:07". The explanation lists the
  safety rules of section 8.3 in plain words.
- **Status** card: the state with text and icon (`Idle`, `Running`, `Stopping`,
  `Stopped`, `Failed`; a stopped run says why: "Stopped by safety rule: a failover is in
  progress. It resumes by itself when the cluster is healthy" or "Stopped by an operator"),
  the progress bar, and the moves in flight with kind, phase and percent.
- Buttons: **Start rebalance**, **Resume**, **Stop** (confirmation: "Moves in flight
  will finish first"), **Preview plan**; `GateButton` disables each for roles that lack
  it with the reason in the tooltip text and in an `aria-describedby`.
- **History** table: the reports (`listRebalanceReports`) with trigger, strategy,
  outcome, moves done, skipped and failed, bytes, duration, and the existing report dialog
  with every move, its reason, attempts and skip reason.
- A read-only user sees every card and the history, no enabled action, and the automatic
  switch as a read-only value.

### 12.3 Servers page: drain, weight, remove

- A server row and the detail page get **Drain** next to the existing actions. The dialog
  **Drain server 10.0.0.12** loads `previewServerDrain` and shows: the buckets and bytes
  that will move and where to, the standbys that will be relocated and to which servers,
  whether the replica count can be kept, any local data that will be lost, and the
  blockers. Fields: **Remove the server when it is empty** (checkbox), **Concurrent
  moves**, **I accept fewer standbys** (shown only when needed), **I understand that data
  of ordinary databases on this server will be deleted** (shown only when the preview
  lists local data). The confirm button reads **Drain** (**Drain and remove** when the
  checkbox is set).
- While draining the row shows `Draining` with a progress bar (moves done over planned)
  and the Rebalance page shows the same run with trigger `drain`. **Return to service**
  appears for a server with the flag false and no running drain. Once the server holds no
  bucket and hosts no standby the row offers **Remove** (the existing dialog) and says
  "Empty: ready to remove".
- **Weight** is a field of the existing server edit dialog (`updateServer`): "Relative
  capacity, 1 is normal; 2 receives twice as much", used by both strategies.
- Failure states: a failed drain shows the task error and the step; the server stays
  `Draining` and the actions **Retry** (start the drain again), **Return to service**.
- Read-only users see the status and the preview button only.

### 12.4 Everywhere

All text is English, server and database names pass through `safeText`, no state is
conveyed by colour alone, every progress element has an accessible name, and the pages
work at 320 px (the existing browser layout tests).

## 13. Corrections to existing documents, contract and code that this design relies on

1. Contract: the `ServerStatus` text says `decommissioning` is "rebalance away from it is
   running"; `updateServer` says "draining because of a removal, decommissioning"; the
   control plane README defines `draining` as a flag false with buckets held. This design
   fixes the README's rule and asks for the contract text to follow (0.7.0).
2. Control plane: a paused task is counted as running by `facts.go` and
   `servergroups.go`, which would block changing the flag of a server during a stopped
   rebalance (section 6.1). The engine also has no way to pause (section 6.1).
3. Contract: `Bucket.bucket` is "`hash(key) % bucketCount`", true only for a group that was
   never split (section 9.3).
4. `shard-phase4.md` says the move blocks "only writers of this bucket" in its design text;
   its own implementation notes correct it (parents locked). The user documentation of
   splits and moves must say "writers of the group's tables on the node".
5. ADR 0007 says the hotspot inside one bucket cannot be split and the bucket count is
   fixed. A new ADR (0023, "Splitting a bucket by hash bounds of different moduli") records
   the change; ADR 0007 is not edited except for its status line pointing to it, as the
   documentation rules require.
6. `rebalance_plan` of version 0.3 counts buckets per colocation group independently.
   With several colocation groups that is correct for counts, but it ignores that load is
   the sum over groups (matters for `by_disk_size`); `plan_rebalance` sums them.
7. Phase 4 and the review: S4-01 to S4-05 above must be confirmed fixed (increment 0).
8. The contract's `removeServer` says a replica copy counts as "owning buckets"; with
   chained placement every server hosts a neighbour's standby, so no server is removable
   until the standbys are relocated. `server.drain` is the only way, which the contract's
   "run `POST /rebalance`, then remove it" does not say and which section 11 adds.

## 14. Tests

### 14.1 Extension

pg_regress (one node, no replication):

- `plan_rebalance`: both strategies on hand-built maps: the examples of section 4.7 with
  their exact move counts; weights; drain sources; threshold 0; `extra_load`;
  `exclude_groups`; a group without a primary; buckets that are `moving` or
  `splitting`; the ordering rule; termination on an unimprovable map; equality of the
  counts with phase 4's `rebalance_plan` over generated maps (a test of the wrapper).
- Bucket layout: after hand-made splits, `bucket_for` returns the covering bucket for
  every hash in a large range; `bucket_range`; the layout check rejects overlap and gaps.
- Upgrade 0.6 to 0.7 against a fresh install; `set_node_property` call forms.

TAP (three nodes unless stated), each with fault points at every phase boundary
(`split_after_begin`, `split_after_mark`, `split_alive_probe` (in the probe), `split_after_preparing`, `split_after_children`, `split_after_triggers`,
`split_mid_backfill` (after n batches), `split_after_backfill`, `split_before_switch`,
`split_switch_after_lock`, `_after_verify`, `_after_detach`, `_after_attach`,
`_after_local`, `split_after_switch`, `split_undo_after_triggers`,
`split_undo_after_children`):

- Split of a bucket with two colocated tables and a foreign key between them: every row
  exactly once from every node before and after; partitions and bounds as expected; a
  second split of a child; a split into 4; the layout check clean.
- Writers during a split (the model of phase 4's load test): inserts, updates, deletes and
  truncates of the bucket's keys and others, from all nodes, throughout; afterwards the
  table equals the model, including after the switch.
- Interleavings against the backfill: a delete of a row exactly between the batch's lock
  and its commit (must not resurrect), an update during a batch, an insert behind and
  ahead of the key cursor, a row locked by a long transaction (skipped, copied by a later
  pass), a writer that takes a row of the batch in the opposite order (the deadlock
  argument of 3.2 must hold: no deadlock, no abort of the writer).
- Bypass attempts: a direct write into the old partition; a write with
  `session_replication_role = replica`; `COPY` into the partition by a superuser; each
  must be present in the children.
- Switch against statements: a long reader and a long writer on the owner hold the
  switch back, the attempt bound fails the split with the blocker named, nothing is left;
  scans, fan-out writes and routed statements from every node in a loop through the
  switch (the harness of phase 2a): no wrong answer, `KS001` the only failure.
- S4-01 sequences: scan across a move away and back; across a split; across a split and
  a move of a child.
- Crash and loss: kill the operation's session in each phase; kill the owner, the map
  group primary, a third node; failover of the owning group in `copying` and in
  `catching_up` with `promote_node`; in each case after `resume_bucket_moves` the bucket
  is whole in exactly one layout, no trigger, child, slot or orphan is left except the
  orphan of a finished switch, and `check_cluster` is clean. The recovery worker alone
  (interval 2 s) does it with no manual call (S4-02).
- Direct write across a move's switch with a lagging subscription (S4-03).
- Moves and splits together: a move of a bucket while another bucket of the colocation
  group splits; a move of a bucket refused during its split.
- Preconditions: no primary key, `REPLICA IDENTITY FULL` without one, a mismatched node
  version, a second split of the group, DDL during a split; each fails cleanly and
  leaves nothing.

### 14.2 Control plane

Unit: the scheduler's choice of the next move under the concurrency rules (the pair
rule, per-server limit, attempts and backoff) with a fake extension; the safety-rule
evaluator for every row of the table in 8.3 with generated cluster states; the settle
timers across a simulated leader change; report assembly; the `ErrPause` engine path and
its crash points (the existing `FaultPoints` extended with `pause_requested` and
`pause_recorded`); the strategy defaults.

Integration (WSL, three or more `kingresd` processes, as phase 5):

- The six operations end to end with `by_bucket_count`: add a server, preview, start,
  progress, stop (moves in flight finish), resume, report; a second start gets 409
  `rebalance_in_progress`.
- `by_disk_size`: skewed buckets (a few large ones) produce the plan of example 2.
- Kill the leader's `kingresd` at each step boundary and in step 3 with moves in flight:
  the new leader finishes; no bucket ends on two groups or on none.
- A failed move (the target's `max_replication_slots` set to 0 for one bucket) is retried
  by the attempts rule, then the run is suspended with `moveFailed`.
- Automatic mode: off by default; switched on after a server is added, it starts after the
  settle time and not before; with a standby stopped it does not start, and a running
  automatic run pauses and resumes when the standby is back; with a failover task it does
  not start; a target volume above the limit skips the move.
- Drain: a three-server cluster with `replicaCount = 1`, a write load throughout; drain
  with removal; every bucket on the remaining servers, no acknowledged write lost, the
  standbys relocated before the old ones are dropped (the group never has fewer healthy
  standbys than before), the server is `uninitialized` and its certificates revoked;
  drain refused when only two servers remain and `replicaCount = 2`; drain of the
  map-group server with the handover step; cancel and return to service; local data
  acknowledgement.
- Split through the API: a task with progress, then the buckets list shows the children.
- Contract tests: the new operations and schemas against `openapi.yaml` 0.7.0; the
  existing six unchanged for a client of 0.6.0 (the old request bodies and responses
  validate).

### 14.3 Console

Vitest and Testing Library against the mock API: the split dialog (fields, preview,
blockers, role gating, task progress, failure state); the Rebalance page (options, preview
totals, start, stop, resume, the stopped-by-safety-rule text, automatic card for admin and
for read-only, history and report dialog); the Servers page (drain dialog with each
conditional field, progress in the row, return to service, remove after drain, weight);
a tolerance test with unknown enum values (`splitting`, a new `stoppedBy`); axe checks of
each new dialog in the browser tests; layout at 320 px. `npm run api:check` for the
generated types of contract 0.7.0.

## 15. Implementation order

Each increment is committable and testable alone, on the working tree at the time. The
extension is changed by one line of work at a time (architecture, "Phases 9 to 12").

0. **Prerequisites (extension).** Confirm or make the S4-01 epoch, S4-02 worker calls,
   S4-03 partition locks, S4-04 bounded attempts, S4-05 version check, and the TAP tests of
   section 14.1 for them. No new feature. Gate: those tests pass.
1. **The six rebalance operations with `by_bucket_count` (control plane, contract
   unchanged).** Engine `ErrPause` and the migration of `kingres_cp.rebalance_run` and
   `rebalance_move`; `getRebalanceStatus`, `startRebalance`, `previewRebalancePlan`,
   `stopRebalance`, `listRebalanceReports`, `getRebalanceReport` implemented with the
   existing `kingres.rebalance_plan(max_moves)` and `kingres.move_bucket`, the scheduler of
   section 6.3 with the concurrency rules and attempts, the manual safety rules marked (M),
   the fix of `facts.go` for paused tasks, and the replacement of the "not supported yet"
   answers. Options in this increment are the two the contract has today (`databases`,
   `maxConcurrentMoves`). Gate: the integration tests of 14.2 for the six operations,
   including the leader kill; the panel's existing Rebalance page works against the real
   server.
2. **`plan_rebalance`, weights, `by_disk_size`, threshold (extension 0.7 part 1).** The
   catalog column `node.weight`, `set_node_property` with three arguments, `plan_rebalance`,
   `group_load`, the wrapper, `kingres_shard.min_bucket_cost` handling; then the control
   plane uses them, adds the new `RebalanceOptions`, the cluster settings and `Server.weight`
   (contract 0.7.0 part 1). Gate: pg_regress of 14.1, integration for `by_disk_size`.
3. **Drain.** `previewServerDrain`, `drainServer`, the `server.drain` task with steps 1 to 3
   and 6 to 7 first (a cluster with `replicaCount = 0` and the case where standbys are not
   in the way), then step 5 (standby relocation) when phase 5's standby management is in
   the tree, then step 4 (map group handover). Gate: the drain integration tests.
4. **Automatic mode.** The job in the leader, the settle timers, the safety rules of 8.3
   as the automatic evaluator and the pause and resume of automatic runs, the alert
   `rebalanceFailed`, the settings. Gate: the automatic-mode integration tests.
5. **Split, extension.** The catalog changes of 2.2 (modulus, `max_modulus`, `bucket_move`
   columns), the audit of every use of `bucket_count`, `split_bucket`, the capture
   triggers, backfill, switch, undo, `check_cluster` rows, and a TAP test for each item of
   14.1. Gate: all of 14.1, then the comparison harness of phase 2a run with split groups.
6. **Split, control plane and contract.** `previewBucketSplit`, `splitBucket`, the
   `bucket.split` task, the schema additions and problem codes (contract 0.7.0 part 2).
7. **Console.** Rebalance page options, automatic card and history; Servers page drain and
   weight; Shards page split. Each can ship with the increment whose operations it uses
   (1 first, the page needs nothing new).
8. **Documents.** ADR 0023, README of the extension (the new interface, limits), the
   control plane README, the web README, and the work-log entries, as
   [documentation.md](../dev/documentation.md) requires.

## 16. Open questions, and the decisions taken

| Question | Decision |
|---|---|
| Split one bucket or change the group's modulus? | **[Recommendation]** one bucket, in place, powers of two; the whole-group change is phase 9. Taken above. |
| Online copy by trigger or by logical replication to the same node? | **[Recommendation]** trigger on the old partition with row-lock backfill: logical replication cannot write into a differently named table on the same node. A decoder plugin was rejected as too much code for the benefit. |
| Should the worker continue a half-done split instead of undoing it? | **[Recommendation]** undo; the worker must stay short, and a restart costs only the copy. Revisit if splits of very large buckets fail often. |
| Bucket numbering after a split. | **[Recommendation]** the remainder (section 1.3); keeps names, keys and unsplit groups unchanged. |
| Split role. | **[Recommendation]** `kingres_admin`, because it is permanent. |
| Children placed on other groups in the same call? | **[Recommendation]** no; split in place, then move. |
| `by_bucket_count` cost of a split bucket. | **[Recommendation]** 1; users who split for balance use `by_disk_size`. |
| User-defined strategies. | **[Recommendation]** not in this phase. |
| Default automatic threshold and timers. | **[Recommendation]** 0.15, 300 s, 600 s after a topology event, 3600 s between runs; to be tuned by measurement and recorded in the work log. |
| Parallel moves in one colocation group. | **[Recommendation]** allowed between disjoint pairs of groups, never on a shared source or target. |
| Does a stopped rebalance block flag changes? | **[Recommendation]** no (section 6.1). |
| Automatic mode drains a server whose flag was set by hand? | **[Recommendation]** yes: the plan drains non-receivers first, and the flag is the operator's request. |
| Should a split verify by checksum? | **[Recommendation]** `count` by default and `none`; a checksum mode is not offered because it needs the full scan under the lock. |
| `previewBucketSplit` as `GET`? | **[Recommendation]** `POST` with a body, like `previewRebalancePlan`; the contract's pattern for computed previews. |

## 17. What this design needs from the other phase documents

- [shard-phase9.md](shard-phase9.md): that a colocation group with split buckets can be
  the source of a redistribution into a new uniform group (`modulus` per bucket is read
  from `bucket_map`), that `create_distributed_table`'s "same bucket count" matching uses
  `colocation.bucket_count` (the base) and creates partitions with each bucket's modulus
  when joining a split group, that a partitioned-table distribution does not assume one
  modulus either, and the extension version it produces (section 2.1).
- [shard-phase11.md](shard-phase11.md): whether a schema placement unit moves with the same
  scheduler (a move of a schema would be one more kind of item in the plan with a
  cost, and the concurrency rule of 6.3 generalizes to "no two operations share a source
  or target group inside one unit"); and that a drain must also move schemas.
- [analytics-phase12-storage.md](analytics-phase12-storage.md): whether columnar or
  time-partitioned distributed tables are tables of a colocation group that the move and
  the split handle (a split of a bucket whose partitions are themselves partitioned is
  **not** designed here and is refused by `split_unsupported`).
- [analytics-phase12-queries.md](analytics-phase12-queries.md): a repartitioned join must
  take the bucket's modulus from `bucket_map`, not `bucket_count`.

## 18. Known limits after this phase

- A split cannot be undone: no merge. A group's buckets of different sizes are balanced by
  `by_disk_size` only.
- The base bucket count of a colocation group does not change in place, and a group with
  split buckets is re-bucketed only by phase 9's redistribution.
- Writers of every bucket of the group's tables on the owning node wait during the switch
  of a split or a move (and readers at the detach); the wait of a split grows with the
  number of rows of the bucket's partitions (the verification), and a long transaction on
  those tables delays the switch until it fails after `*_switch_attempts`.
- During a split or a move of a colocation group, schema changes on its tables are
  refused; a rebalance of many buckets of one group keeps them refused for most of its
  duration. The operator stops the rebalance for a schema change.
- The write cost of a bucket doubles during its split, and the owner and its standbys need
  room for a second copy of the bucket until the orphan is dropped, 10 minutes after the
  switch by default.
- The split needs a primary key on every table of the colocation group.
- A drain does not move data that is not sharded (local tables, plain databases, ledger
  tables, the system database); the server that hosts the map group primary is drained only
  if phase 5's group-0 switchover is available (section 11.3, step 4).
- `by_disk_size` uses sizes read when the plan is computed; sizes change, which is why the
  scheduler recomputes, but a run can end with deviation above the threshold. The
  automatic mode never chases a deviation that is below its own threshold.
- Free disk space is checked by the control plane before and between moves, not
  reserved: another writer can still fill the volume.
- The logical slot of a move is lost at a failover of its source (a move in flight
  restarts); a split has no such external state. **[Needs verification]** whether
  PostgreSQL 18's replication-slot synchronization to standbys could be used to carry
  slots over; this design does not rely on it.
- No global snapshot: a scan that spans a split's commit on two nodes can see the groups
  at different times, as in phase 2a; a statement that overlaps the commit fails with
  `KS001`, never answers wrongly (ADR 0022).
- `KS001` rates of splits and moves are measured only in test builds; production figures
  are an implementation task (section 15).

## 19. Needs verification (collected)

1. No development version of PostgreSQL supports `SPLIT PARTITION` for hash partitions,
   and the PostgreSQL 19 status (section 1.1).
2. Every use of `bucket_count` as a modulus in the working tree (section 2.2).
3. `FOR SHARE SKIP LOCKED` in the subquery of the backfill insert (3.2).
4. Deadlock-freedom of the backfill against the capture trigger, by the interleaving
   tests (3.2).
5. That an attach reuses an equivalent validated foreign key of the child and does not
   rescan (3.2).
6. The time of the under-lock verification by measurement (3.2).
7. The behaviour of ledger tables over distributed tables, when designed (3.5).
8. That the planner hook sees a direct write to a bucket partition (7.3).
9. That the size of the target's tables grows steadily during the initial table copy of a
   logical subscription (6.3).
10. That resource data carries the free bytes of each instance's data volume (8.4).
11. Phase 5: a graceful switchover of group 0 as an operation, and survival of a task
    across the leader change it causes (11.3 step 4).
12. The catalog table of `kingres_ledger` to name ledger tables in the drain preview (11.5).
13. The extension version number, against [shard-phase9.md](shard-phase9.md) (2.1).
14. Whether PostgreSQL 18's slot synchronization could carry a move's slot across a
    failover (section 18).

## 20. First increment as built: where it departs from this design

The first increment of `kingresd` (contract 0.9.0) implements the six existing rebalance operations with the strategy "by bucket
count" and nothing of the later increments (strategies and weights, automatic mode, draining a server, splitting a bucket). It is the
base the later increments extend. Departures from sections 6 and 9:

- **One move at a time.** The scheduler of section 6.3 runs `maxConcurrentMoves` moves; this increment runs one, and plans again
  (`kingres.rebalance_plan`, not `plan_rebalance`, which does not exist yet) before every move. `maxConcurrentMoves` is accepted,
  validated and recorded in the run, and has no effect yet.
- **Task kind and steps.** Kind `rebalance`, subject `cluster`/`cluster`, steps `Check that the cluster can rebalance`, `Compute the
  plan`, `Move buckets`, `Write the report`. The step list is fixed; the moves are rows of `kingres_cp.rebalance_move`, not steps.
- **Engine change (section 6.1).** `task.PauseError` / `task.Pause(reason)` and the store interface `PauseStore` (`Pause`, `Resume`)
  exist in the three stores; the state `paused` is now set. A paused task still holds the subject lock, so there is one rebalance
  per cluster including a stopped one; `Engine.Cancel` of a paused task fails it at once and runs the definition's `OnFail`. The
  second engine change is the step state `skipped` (`task.Skip`), which section 13 did not list.
- **Stop (section 6.2).** A stop does not wait for the move in flight. It asks `kingres.cancel_bucket_move` for it at every poll of
  the loop: a move before its switch is cancelled and undone by the extension (the report lists it as skipped, reason `stopped`); a
  move that is switching answers that it cannot be cancelled yet and is asked again until it has ended (the switch either
  completes or falls back to catching up); a move past the switch finishes. There is no `force` member yet.
- **Retry rule.** A move that fails is tried again after 30 s times its failures (`-rebalance-retry-backoff` sets the 30 s), up to
  three tries (the default of section 6.3); other buckets go on meanwhile. A bucket given up makes the run end `failed`; the report
  step writes the report first and then returns the error, so the task fails with a complete report. The alert `taskFailed` is raised
  with `subject.taskId`.
- **Resume after a restart or a change of leader.** A new claim of the task under a new epoch re-enters `Move buckets`, which first
  settles every move row left `running`: it finds the extension's record (`kingres.bucket_move`) and, while it is unfinished, calls
  `kingres.resume_bucket_moves()` until the record is `done`, `cleaning`, `failed` or `cancelled` (the extension never touches a move
  whose session is alive). Every write of the run is one transaction that proves the lease and its epoch (`FOR SHARE` on the task row),
  so a leader that lost its lease starts and records nothing.
- **Conditions** checked before the run and before each move: no unfinished `server.failover`, `server.add`, `server.remove` or
  `server.leave` task; `kingres.check_cluster()` without a finding of a kind that makes moving unsafe (the list is in
  `internal/rebalance/pg.go`); `wal_level = logical` on every primary (instances now get it from `kingresd`; an older instance
  needs a restart); a server that should hold shards. Per move: the lease state of the two groups as the leader knows it, a
  synchronous standby that is required and not in sync (the move is skipped: commits would wait), and the free disk of the target when
  the server reports it (none does yet, so the check is dormant). The problem codes are `rebalance_in_progress`,
  `operation_in_progress`, `failover_in_progress`, `version_mismatch`, `cluster_inconsistent`, `no_receiving_server`,
  `nothing_to_rebalance`; `group_degraded` and `insufficient_disk` are reserved.
- **Percent of a move** is an estimate by phase (5, 30, 85, 95, 100); the copy is not measured.
- **Reports** are `rebalance_run` and `rebalance_move` of migration 0008, with the placement of primary buckets per database and server
  before and after as JSON; there is no retention job yet.
- **Standbys.** A move changes the owner of the data, not where standbys are. A logical slot and a subscription exist on primaries that
  have standbys; the slot is not on the standby, so a failover during a move fails the move and the extension undoes it. Failover itself is
  not implemented in `kingresd`.

## 21. The fixes for the review of phase 10 (2026-11-21)

An independent review of the extension part of phase 10 (sections 3, 4, 7, 14 and 15 of this design, `contrib/kingres_shard`; the control plane and the console were not in
the reviewed tree) found one critical defect, two high ones, six medium ones and seven low ones (P10R-01 to P10R-16). This section says what each was, what was done and which
test and mutant stand for it. The files are `kingres_shard--0.7--0.8.sql` (the phase 10 part; `kingres_shard--0.8.sql` is generated from it by `mk-install-script.sh`),
`README.md` of the extension and the tests `t/201`, `t/204`, `t/205`, `t/206` and the regression test `plan_strategies`. Every mutant below was applied to the installed script of
PostgreSQL 18 and the named test failed; the unmutated script passes every test on 18 and 19.

### 21.1 P10R-01 (critical): the recovery undid a switch that then committed

**The defect.** The switch runs on the owning node. The backend there does not stop when the session that called it dies (it is in a lock wait or counting rows), so
`resume_bucket_splits()` could ask "are the partitions switched?", get "no" for a switch that was a moment from committing, undo (its first statement waited for the
switch's locks), and record the split as failed; the switch then committed. The map said modulus 8, the owner had two partitions of modulus 16, the old partitions stayed as orphans
for ever, and the next `move_bucket()` copied one child and left the other: rows unreachable, and a key that could be inserted twice.

**The fix** is a fence, in the way the phase 4 mover settles its switch by one record that both sides read, adapted to the fact that the decision here is on another node
than the work:

- the switch and the undo of one split take the same advisory transaction lock (`hashtext('kingres_split_switch')`, the split id) as their first act, so they never run together;
  whoever comes second waits for the first to end (the undo for at most `move_switch_timeout` plus a lock timeout, the switch for the lock timeout) and then looks at the partitions;
- `internal_split_undo()` looks at the bounds of the partitions after it holds the fence: switched means it undoes nothing and answers `switched`, so that the caller
  (`resume_bucket_splits()`, or the error path of `split_bucket()`) writes the catalog; else it undoes and answers `undone`. `resume_bucket_splits()` no longer asks first;
- the switch, once it holds the fence, returns at once when the partitions are switched already (a call that was retried after its answer was lost) and fails when its
  children are gone (an undo ran): a switch that arrives late can never commit after an undo;
- `move_bucket()` compares the bounds of the source's partitions with the bucket map (`internal_move_bounds()`) and refuses a bucket where they differ, with the difference in the
  message, as the second line of defence against any other way to the same state.

The design (14.1) lists fault points inside the switch: they now exist as `split_switch_after_lock` (after the locks of every table, once per table), `split_switch_after_verify`,
`split_switch_after_detach`, `split_switch_after_attach`, and the undo has `split_undo_after_lock`, `split_undo_after_triggers`, `split_undo_after_children`. They run on the owning node (the
setting of the session that runs the split is copied to its control sessions).

**Tests.** `t/205` A: for each of the four points inside the switch the coordinator is terminated while the owner waits at the point, `resume_bucket_splits()` is called, it must not
answer and not undo while the switch runs, and after the point is released the split is finished (not failed), the bucket and its twin have the new modulus in the map and on the
owner, every row is read from every node, a move of the bucket carries every row, and a duplicate key is refused from every node. B: the undo is stopped at each of its three points, and
once the owner's backend and once the session that runs the recovery is killed there: the undo is one transaction (nothing is half done), the next recovery completes it, nothing is left, a
later split of the bucket works. F: a move of a bucket whose partitions are not the map's is refused. **Mutants:** M01 (the undo does not take the fence) and M02 (the undo does not look
again after it) are killed by `t/205` A, M16 (no bounds check in the move) by `t/205` F.

### 21.2 P10R-02 (high): a writer that is not a superuser could not write during the copy

The capture trigger ran with the rights of whoever wrote the old partition, and the children were owned by the role that splits, so every write of an ordinary role (and of the
owner of the table) into the splitting bucket failed with "permission denied" for the whole copy. The children and the trigger functions now belong to the owner of the table (as the
bucket partitions do, and as the copy of a move makes them); the functions are `SECURITY DEFINER`, `search_path = pg_catalog, pg_temp` (the body schema-qualifies everything), revoked from
`PUBLIC`. Making the function run as the superuser would have let the owner of a table run a CHECK constraint or a default of his as a superuser on every write of the splitting bucket, so the
owner is the definer, and the owner of a table needs `USAGE` on its schema (the split is refused before anything is changed otherwise). **Test:** `t/206` A: a role with `INSERT, UPDATE, DELETE`, the
owner, from every node, insert, update, delete during the copy; a role without the privilege is still refused by the table it writes; the functions and the children belong to the owner, the new
partitions too. **Mutants:** M03 (no `SECURITY DEFINER`) and M03b (the children keep the splitting role as owner) are killed by it.

### 21.3 P10R-03 (high): the copy did not converge under inserts at random keys

The count of "rows held by writers" took a new snapshot after the batch, so every row inserted since with a key inside the batch's range counted as held, and with random keys every pass
found some: the split failed after the whole copy. The batch now finds the rows of its range that it did not take in the same statement (same snapshot: new rows are not in it), and looks
at those again in a second statement with a new snapshot; a row that is free is copied, one that is gone is dropped from the count, and only a row that exists and is still locked is held. A row
a writer inserts is in its child by the trigger already. While a writer holds rows the passes now pause (0.2 s times the pass, at most 2 s), because ten quick passes ended a split whose writer
needed a second to finish. **Tests:** `t/206` B (three sessions insert odd keys of the bucket at random, a few hundred a second, in a random order and for longer than the ten passes of a copy can last, while a bucket of
25000 rows is split: it completes, every row that was written is there in the order it was written, no key twice) and C (a transaction holds a row with `FOR UPDATE`; the record shows the held row, the split does not switch, and when the transaction ends the row is copied: verify `none`, so that
only the passes protect the row). **Mutants:** M04 (nothing is ever held) is killed by C, and M19 (the old batch function put back) by B: the split fails with "the copy does not converge", as it did in the reviewer's run.

### 21.4 P10R-04 (medium): a deadlock was not retried, and readers got it

`split_bucket()` retried only a lock timeout. It now retries `55P03`, `40P01` and the statement timeout of the switch, within `move_switch_attempts`, and takes the SQLSTATE of the remote
error (it is passed on) instead of matching the text. The switch sets `deadlock_timeout = 100 ms` (the move does the same), so that a deadlock with an application transaction is found by the
switch, which gives way, and not by a reader or a writer that waited longer. **Test:** `t/206` D: a writer holds `ROW EXCLUSIVE` on one table, the switch holds its lock on the first and waits at the
fault point, the writer asks for the first: the switch asks for the second and finds the deadlock. The writer gets its lock without an error, the owner's log says "deadlock detected", the split is
completed by the second attempt. **Mutants:** M05 (no retry of `40P01`) and M18 (the default deadlock timeout) are killed by D. What stays: a reader can still get `deadlock detected` when a
deadlock involves three parties (a reader, a writer and the switch) and another party's timer fires first; the design's promise that `KS001` is the only reader failure holds to the extent that this
does not occur, and the README says "retryable" for it as for a move.

### 21.5 P10R-05 (medium): bucket numbers of 10000 and above

`lpad` cuts: `lpad('12288', 4, '0')` is `1228`, so SQL named the partition of bucket 12288 `<table>_b1228` and the C code `<table>_b12288`; a split that made such a bucket failed after the full
copy, and two partitions could have the same name. The same cut was in the views `kingres.buckets` and `kingres.partitions`, in `partition_leaves` and in the regular expressions `_b[0-9]{4}$` of the
checks. Instead of changing every one of them, and the C code with them, a split whose new buckets would be numbered 10000 or more is refused before anything is recorded
(`program_limit_exceeded`, with the number). A group starts with at most 4096 buckets, so only a split can reach such numbers; the limit is in the README under "Limits". The
design text of section 1.3, which says that `lpad` "produces a longer name", is wrong and is corrected here: it does not. **Test:** `t/205` E (7 in 16, then 127 in 16, then 2047 in 16 is refused,
2047 in 2 is allowed). **Mutant:** M06 (no limit) is killed by it.

### 21.6 P10R-06 (medium): the live mark was not released after an error

The session advisory lock of a split stayed after an error after the switch or after a failed undo, so `resume_bucket_splits()` thought the split alive until the session ended (a pooled
session: never), and each failed split of a session leaked one lock. `split_bucket()` releases it on every way out. **Test:** `t/205` C (an error at `split_after_switch` in a session that stays
open: the mark is gone, the session holds no advisory lock, the recovery settles the split at once). **Mutant:** M07 is killed by it.

### 21.7 P10R-07 (medium): the plan stopped early with unequal weights

The target was always the receiver of the least utilization, and a source with no bucket that improved that one pair was given up for the rest of the plan. The plan now tries the receivers in
the order of their utilization and gives the source up only when none helps. With equal weights nothing changes (a bucket that helps the least utilized receiver helps the others as much or
less: the regression test that compares the plan with `rebalance_plan()` over generated maps passes unchanged). **Test:** `plan_strategies` (weights 1, 1, 3, 1 with 1, 1, 3, 2 buckets: one move
from group 3 to group 2, which the old plan did not make). **Mutant:** M08 (the first receiver only) is killed by it. The reviewer's fuzz (400 random clusters through
`internal_plan_simulate`) found 16 stuck plans; the fix is the loop the example exercises, the fuzz itself was not re-run (see 21.14).

### 21.8 P10R-08 (medium): the switch was not bounded by `move_switch_timeout`

The locks had a bound, the verification, the detach and the attach, which run with the writers waiting, had only the statement timeout of the call (six node timeouts plus the switch timeout).
The coordinator now sends the switch as one command of three statements in one transaction, the lock function, `SET LOCAL statement_timeout = move_switch_timeout` and the switch function. A
statement timeout inside a function does not apply to the running statement, so the bound is set between two statements. A switch that runs past it is cut, rolls back and is tried again
(21.4). **Test:** `t/205` D (`move_switch_timeout` 1 s, three attempts, a verification that does not end: the split gives up in seconds and says it did not finish; without the bound it waits for the
call's, a minute). **Mutant:** M09 (statement timeout 0) is killed by it.

### 21.9 The low findings

- **P10R-09** (a distributed schema counted as the floor in `by_disk_size`): `internal_plan_inputs()` now reads, on the owning node, the size of the schema's tables as the size of the schema's one
  bucket. `t/201` G (a schema of 17 MB raises the load of its group by its size, the plan does not move it). Mutant M12 killed.
- **P10R-10** (who may plan): the README sentence was wrong; `plan_rebalance()` and `group_load()` are for superusers (the plan reads through functions that are not granted to others). The sentence is
  corrected; `t/201` F now tests the role that has `SELECT` on `kingres.bucket_move` and `USAGE` on the schema (permission denied; `drain_status()` works). Mutant M14 (the helper granted to `PUBLIC`) killed.
- **P10R-11** (the time bound of `check_cluster()`): see 21.10.
- **P10R-12** (a false `bucket_layout` between the owner's switch and the catalog change): the partition check leaves out a colocation group with an unfinished split. `t/205` G holds a split at
  `split_after_switch` and reads `check_cluster()`: nothing. Mutant M10 killed.
- **P10R-13** (the waits of the undo): the undo has a lock timeout of its own (`max(200 ms, move_switch_timeout / 10)`, and the fence wait is bounded by the switch bound); a lock that is not got fails
  the undo and the recovery tries again. `t/206` E: a writer that keeps the switch out for all its attempts and then keeps the undo out: the split gives up in seconds, names the writer
  (`pid N: UPDATE ...`), leaves the record for the recovery and the recovery settles it when the writer has ended. Mutant M17 (no lock timeout in the undo) killed.
- **P10R-14** (what `verify 'full'` proves): its repair is now `ON CONFLICT (key) DO UPDATE SET` every other column, so a child row with other values is brought to the partition's values, as the README
  said. `t/204` C2 adds a child row with a wrong value. Mutant M11 killed. `'count'` still compares counts only, as the design says; a split with `'count'` trusts the triggers for content.
- **P10R-15** (departures from the design): see 21.11.
- **P10R-16** (tests): the promised cases that now exist are a foreign key between two colocated tables (`t/206`, split with `s2` referencing `s1`), a non-superuser writer, random-key inserts, a held row, a
  deadlock, a writer that keeps the switch out with the blocker named, kills inside the switch and the undo, and a bucket number above 9999. The mutant that survived the regression tests,
  the removal of the "arriving" condition of `drain_status().drained`, has a case in `plan_strategies` (a group that may not receive and owns nothing, with a move arriving, is not drained) and M15 is killed
  by it. What is still not tested is in 21.12.

### 21.10 P10R-11: the time bound of `check_cluster()`

The README said at most `2 x T` (`node_timeout`) for each node that does not answer. The reviewer measured, with `node_timeout = 3` and a stopped postmaster, 16.9 s for one node and 31.7 s for two:
10.05 s in the base check (the older code), 5.0 s in `internal_check_table_operations()`, which did not use the list of nodes that the base check found not answering (the layout and split
checks of phase 10 did, and are right), and the rest in the other steps. The claim was never true for that case: a node that does not accept a new connection is waited for by the connect, which
`node_connect_timeout` (C, 5 s by default) bounds and `node_timeout` does not; the base check connects to such a node twice and the table operations check once more.

Fixed: the table operations check takes the list and asks no node on it. Measured again (`t/207`, T = 3, C = 2, the tests' own cluster of four nodes): a node whose cached sessions do not answer costs T
(3.04 s; 6.03 s for two such nodes); a node whose postmaster is stopped costs 2 x C (4.09 s; 8.07 s for two; with the old table operations check 6.1 s and 12.1 s: mutant M13, killed by the test, whose bound
is 2 x C plus 1.5 s); a node that is down is refused at once (0.09 s). The README states these three cases instead of `2 x T`. The base check is the older code and is not restructured (its two connects to a
node could become one: left open, 21.12).

### 21.11 P10R-15: departures from the design, and what the control plane calls

The extension's departures stay (they are on purpose and in the README); this is what the part of the design that is not built (sections 5, 6, 9 and 15, the control plane) must use:

- **The record of a split is `kingres.bucket_split`, not a row of `bucket_move` with `kind = 'split'`.** `resume_bucket_moves()`, `cancel_bucket_move()` and `cleanup_moved_buckets()` do not see a split. A control
  plane that lost its connection to a split calls `resume_bucket_splits()` (it settles every split whose session is gone, and answers how many), stops one with `cancel_bucket_split(id)` (before its
  switch) and drops the orphans with `cleanup_bucket_splits([force])`; the recovery worker calls both sets. The split is started with `split_bucket(colocation_id, bucket, parts, verify)` on the map group
  primary, as a superuser, in a READ COMMITTED session that stays open for the whole split (the live mark is a session lock), and it returns the split id; progress is `kingres.bucket_split` (`phase`,
  `detail`, `rows_done`, `rows_total`).
- **The settings of the design for a split are constants:** `split_batch_rows` is 5000, `split_converge_passes` is 10, `max_bucket_modulus` is 65536 (checked by `split_bucket()`), and a split uses the
  attempts and bounds of the move (`move_switch_attempts`, `move_switch_timeout`). The control plane does not set them.
- **Names:** `check_cluster()` says `split_stalled` where the design says `operation_stalled`, and `extension_version_differs` where it says `extension_version`.
- **The epoch of a split** is raised by the catalog change after the switch, not in the transaction that changes the partitions (design 7.1); the partition-identity check of scans covers the gap (the
  children are new tables, so a scan that overlaps it fails with `KS001`).
- **Fault points** exist inside the switch and the undo (21.1) in addition to the phase boundaries of design 14.1.
- **Limits that the control plane should show:** bucket numbers up to 9999 (21.5); a split needs a primary key on every table; and the owner of every table of the group needs `USAGE` on its schema (21.2).

### 21.12 Not done, and left open

- A failover of the owning group during a split, a split with the S4-01 sequences, a split refused for a node of a mismatched version, and the comparison harness of phase 2a run with split groups
  (design 14.1, 15) have no test in this round.
- The plan fuzz of the reviewer (400 clusters) was not re-run after the fix; the regression test holds the example and the equal-weight comparison.
- `drain_status().drained` does not consider moves in `cleaning` (the old copy is still on the group); the control plane waits for the cleanup too.
- The base check of `check_cluster()` is the older code and keeps its own cost per node that does not accept a connection: two connects, 2 x `node_connect_timeout` (21.10).
- A reader can still get `deadlock detected` when a deadlock involves more than the reader and the switch (21.4).
- The undo that a failed `split_bucket()` runs can fail on a writer that holds the table; the record then waits for the recovery (the worker interval), and until then the triggers keep the children current.

### 21.13 The mutants

Each was applied by changing the installed install script of PostgreSQL 18 and running the named test, which failed; the script was restored afterwards.

| Mutant | Change | Killed by |
|---|---|---|
| M01 | the undo does not take the fence of the switch | `t/205` A |
| M02 | the undo does not look at the partitions again after the fence | `t/205` A |
| M03 | the capture trigger function without `SECURITY DEFINER` | `t/206` A |
| M03b | the children are owned by the role that splits | `t/206` A |
| M04 | rows held by writers are never counted | `t/206` C |
| M05 | no retry of a deadlock (`40P01`) in the switch | `t/206` D |
| M06 | no limit on bucket numbers | `t/205` E |
| M07 | the live mark is not released after an error after the switch | `t/205` C |
| M08 | the plan gives a source up at the first receiver | `plan_strategies` |
| M09 | no statement timeout between the two halves of the switch | `t/205` D |
| M10 | `check_cluster()` compares the partitions of a group with an unfinished split | `t/205` G |
| M11 | `verify 'full'` does not bring the values in line | `t/204` C2 |
| M12 | a distributed schema counts as the floor | `t/201` G |
| M13 | the table operations check asks nodes that did not answer | `t/207` |
| M14 | the plan helpers are granted to `PUBLIC` | `t/201` F |
| M15 | `drain_status().drained` ignores a move that is arriving | `plan_strategies` |
| M16 | a move does not compare the source's bounds with the bucket map | `t/205` F |
| M17 | the undo has no lock timeout | `t/206` E |
| M18 | the switch keeps the default deadlock timeout | `t/206` D |
| M19 | the old batch function (held rows counted with a new snapshot) | `t/206` B |

## 22. The control plane of increments 2 to 4 and the split as built (contract 0.15.0): decisions and departures

**Decision: a drain is a task of kind `rebalance`, not of kind `server.drain` (departs from sections 9.1 and 11.3).** `drainServer` submits the task `rebalance` (subject `cluster`/`cluster`) with the
options `trigger = drain`, `drainOnly`, the drained server, `removeWhenDone` and `acceptFewerReplicas`. Reason: everything the control plane already does for a rebalance is keyed by that kind and subject, and a drain must
have all of it: the database triggers that exclude server operations and failovers (migrations 0009 to 0016, `KX001`/`KX002`), the in-memory exclusion of the task store, the pause of a rebalance by a failover (`mapgroup.go`),
`rebalance_in_progress` for `PATCH /servers`, the counting in `facts.go` and the read model, one stop, resume and cancel path, the report with the id of the task, and the alert. A separate kind would need each of
those places changed and tested again, and the design wants the same exclusion ("the rebalance lock is held for the duration"). The cost: the task list shows `rebalance` for a drain (the status and the report say
`trigger: drain`), and a drain and a rebalance exclude each other, which they should. The extra steps of a drain are steps of the same task that a plain rebalance skips (`task.Skip`): the flag first, the wait until the
server owns no bucket (including moves in `cleaning`), and the removal, which is the `server.remove` task started after the drain task has ended (the two exclude each other); a manager that restarts in between leaves the
server drained and not removed.

**Not built in this round: steps 4 and 5 of section 11.3** (hand-over of the map group, relocation of the standbys hosted on the server). `drainServer` refuses a server that runs the map group primary
(`server_hosts_map_group`) and a drain that would leave fewer servers than `replicaCount + 1` (`replicas_cannot_be_kept`, `acceptFewerReplicas` overrides); `previewServerDrain` lists the groups whose standby runs on the server.

**Automatic mode (section 8).** The safety rules are the pure function `SafetyBlockers`; an automatic run is paused by `autoGuard` before each move (`stoppedBy: safetyRule`) and resumed by the job when the rules have held for
`settleSeconds`. The "last topology change" is the newest of `server.joined_at`, the end of a server task and `topology.changedAt`, which `updateServer` writes when it changes the flag or the weight. The disk rule uses the existing
per-move check (`Disk`); the rules `maxStandbyLagBytes` and the window are settings, the lag rule was not evaluated in this round and is since (section 23: the lag is read on the primary of each group).

**Split (section 9.3).** The preview cannot find a table without a primary key (the owner's tables are not readable from the map group primary); the extension refuses it when `split_bucket` starts and the task fails with
`split_unsupported`. The task does not wait for the orphans to be dropped (`cleanup_bucket_splits` is the recovery worker's). The size estimate comes from the extension's plan inputs (`internal_plan_inputs('by_disk_size', ...)`).

**Corrections to section 22 (same round).**

- *`Make the schema distributed` of phase 11 is not marked `NonIdempotent` (departs from shard-phase11.md 13.1).* The design marks the step so that an interrupted run fails with `step_interrupted` instead of calling
  `distribute_schema` twice. The step reads `kingres.schemas` before it calls the function and does nothing when the schema is distributed already, which is the design's own remedy for a retry ("its result is known by reading
  `kingres.dist_schema`"). The function is one two-phase transaction, so after an interruption the schema is either distributed or not, and the read says which. With the mark, a crash of the leader between the call and the record of
  the step would fail a task whose work is done and ask a person to look; without it the task finishes. A call that was cut while still running is refused by the extension as "already distributed" only if the transaction committed, which is the case the read finds first.
- *Step 5 of the drain (section 11.3, the standbys hosted on the server) is built; step 4 (the map group) was not built in this round and is built since (section 23).* Step 5 is the standby reconcile of phase 5 (`standby.Leader`) with the draining server taken out of its ring
  (the hook `Excluded`, fed by the cluster setting `drain.excludedServers`, which a return to service clears), then a wait until no `kingres.node` row of role standby is on the server. It has unit tests (`standby/exclusion_test.go`, `rebalance/drain_test.go`)
  and no cluster test yet. Step 4 (the map group) is the planned switchover of the map group of the failover branch
  (the old leader hands the bootstrap to a standby of group 0 on another server, `server.failover` graceful for group 0), after which the drain task continues on the new leader. Until step 4 exists `drainServer` refuses a server that
  runs the map group primary (`server_hosts_map_group`); a server whose standbys the others cannot take fails the relocation step with `replicas_cannot_be_kept` (or ends it with fewer standbys under `acceptFewerReplicas`).

**Cancelling a split, and its limit.** The extension looks at a request to cancel (`cancel_bucket_split(id)`) only during the backfill, before each batch of rows is copied; after the backfill it cannot stop the split, and it refuses the request while the split is switching.
The step `Split the bucket` is not interruptible by the engine (an interrupted step ends the task as cancelled whatever the extension does). The step itself watches for the request (every second), passes it on and says truthfully what came of it:
(a) the request reached the backfill: the extension undoes the split (the children and the triggers go, the bucket is `active` again) and the task **fails with the code `cancelled`** and the text "The split was cancelled before its switch and undone: the bucket is unchanged"; the bucket can be split again;
(b) the request came too late (the extension refused it, or accepted it and the split completed all the same): the task records in its detail "The cancel request came too late: the split had passed its backfill and goes on to its end", **goes on and ends `done`** (the step `Check the new buckets` runs), and the bucket list shows the children. A task is therefore never `cancelled` while the split completes.
The cancel guard of the kind (`splitCancellable`) accepts a cancel until the split step is done or the "too late" note is recorded; a cancel after that is answered 409 `task_not_cancellable`, which is the truthful answer. A split whose session is lost (a restart of `kingresd`) is not touched by a lost lease or a shutdown: the task is taken over, finds the record and settles it with
`resume_bucket_splits()`. The cluster test of the split shows both cases (the gate `split_after_triggers` for the undo, `split_after_backfill` for the late cancel, which ends done with the children in the bucket list).

## 23. Step 4 of the drain (the hand-over of the map group) and the standby-lag rule, as built (contract 0.27.0)

**Step 4 is built** (`internal/rebalance/handover.go`); it settles the **[Needs verification]** of section 11.3.

- *A graceful switchover of group 0 is an operation `kingresd` exposes:* `failoverServer` with `mode: graceful` and `shardGroupIds: [0]` on the server that runs the map group primary (failover round 2, contract 0.18.0). The drain calls the same
  service in the process (`failover.Service.Start`), without a target: the service chooses the standby that streams best.
- *A task of the old leader survives it:* the task rows are in the system database, which the new leader has as the standby that was promoted, so the task can be taken over there. What does not work is a drain that runs while the switchover
  starts: migration 0009 refuses a failover task while a rebalance is pending or running (`KX002`), and the drain is a rebalance (section 22). So the step does not run the switchover inside itself. **Decision:** the step writes a marker
  (cluster setting `drain.mapHandover`: task id, server, state), records the run as stopped by `safetyRule` and pauses the task (a paused rebalance does not exclude a failover); a goroutine waits for the pause, asks for the switchover and
  records `started` or `refused` with the problem; the job `RunDrainWatch` on the manager leader resumes the paused drain when the marker says so and no failover task is unfinished (the database refuses the resume while one is, which is the wait).
  The step runs again from its start: a server that no longer runs the primary of group 0 is done; one that still does fails the task (the switchover was refused, rolled back or given up) and the server stays `draining`. The marker carries an attempt (a random name, so no clock is compared between servers; its age is the clock of the database) and the states `requested`, `requesting` (written before the call), `started` and `refused`. A goroutine acts only while the marker carries its attempt, a young marker of the same task is not overwritten by a manual resume, the request is not made while any failover task is unfinished, and the watch also resumes a `requested` or `requesting` marker when a failover created after it has ended. The marker is cleared when the task ends, fails or is cancelled.
- *Refusal before a task exists:* `server_hosts_map_group` stays for a server that runs the map group primary when no other server runs an active standby of group 0 (or the build has no switchover). It does not read whether that standby is ready: the
  switchover refuses with its own code when it is not, and the drain then fails at step 4 with that code (after the buckets have moved, which is the cost of not duplicating the switchover's checks).
- *Departures from section 11.3:* the order of the steps in the task is the one of section 22 (move, wait until empty, hand over, relocate the standbys, report); the design lists the relocation of the standbys after the hand-over as well. The old
  leader's former instance of group 0 becomes a standby of group 0 on the drained server; step 5 relocates it like any other standby there. The pause is visible: `getRebalanceStatus` says `stopped` with `stoppedBy: safetyRule` while the
  switchover runs. A drain whose switchover is a failover task that waits for an operator stays paused until the operator ends that task.
- *The server dies during the hand-over:* the marker says `requesting`, the old leader is gone, an operator fails the map group over (hard), and the new leader's watch resumes the drain (a failover that began after the marker has ended). The step finds that the server no longer runs group 0 and is done; step 5 then sees the server dead (`ServerStatus`) and fails the task at once with `node_state_invalid` ("died during the drain; fail it over first, then remove it"), because the instances of a dead server can neither be removed nor rebuilt (before this check the step waited for its whole bound). `onFail` clears the marker and the rebalance lock is free. Tested in the cluster (`TestDrainWhoseServerDiesWhileTheMapGroupIsHandedOver`, with the fault point `drain:handover` that holds the request after the marker says `requesting`).
- *The states of a standby record* are the five of the CHECK of migration 0006: `creating`, `ready`, `rebuilding`, `removing` and `failed`; the code writes the first four and never `failed`. The former primary of group 0 is inserted by the failover (`AfterPromotion`) as `rebuilding`, diverged, needs rebuild, and becomes `ready` only when the reconcile has rebuilt it (`setState ready`), so the wait of step 5 for `creating` and `rebuilding` ends exactly when the instance is a standby; `removing` and `failed` cannot become ready and are not waited for. Not covered: two drains at once (a rebalance is one task at a time).

**The standby-lag rule is built** (`automatic.go`, `LaggingGroups`, `PG.StandbyLag`): the row "Group without a healthy standby" of section 8.3 also holds for a group whose standby is behind its primary by more than `maxStandbyLagBytes` (default 64 MiB).
The lag source is the primary of each group (`pg_stat_replication`: the largest `pg_wal_lsn_diff(pg_current_wal_lsn(), replay_lsn)` over the standbys that are connected), read through the map group's connection with the extension's node call, once
per group per evaluation and once per move (the guard). A lag of exactly the limit is allowed. **Inside a run** the guard (before each move) is less strict, because the run's own writes make the standbys lag: the lag has to be over **twice** the limit on **three** evaluations in a row before the run pauses; the resumption of a paused run and the start of a run use the plain limit. The groups are read at the same time, each bounded by 15 s. Only the physical standbys of the group count (the walsenders with a physical slot that are catching up or streaming); a base backup and a logical walsender are ignored; a standby with no replayed position yet counts as lagging; a standby fed by another standby (cascading) is not a sender of the primary and is not seen. A primary that cannot be asked counts as lagging (what cannot be shown to be in step is not trusted). A standby that is not connected does not appear in
`pg_stat_replication` and adds no lag; its absence is the business of the other rules (fewer standbys than the replica count, a server that is not live). The rule uses the name `groupDegraded` (no new name in `blockedBy`). **A manual start** (and the resumption of a stopped manual run) is refused too, before any task exists, with 409 `group_degraded` and the groups named, using the limit of a start; `RebalanceOptions.force` (0.27.0) lifts only this refusal for that start. A drain has its own refusals, the automatic mode its own rule, and the guard inside a run is the one above; none of them reads `force`. A start reads nothing when no standby is wanted. (`CheckMove` still skips a move whose group has no synchronous standby in sync.)
