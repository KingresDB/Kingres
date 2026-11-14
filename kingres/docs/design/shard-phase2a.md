# kingres_shard: design of phase 2a (atomic writes, reference tables, DDL, multi-group statements)

Status: accepted for implementation, 2026-11-14.
Builds on [shard-mvp.md](shard-mvp.md) (phase 1, extension version 0.1) and
produces extension version 0.2, with an upgrade script
`kingres_shard--0.1--0.2.sql`.

Phase 2 of the roadmap in [architecture.md](../architecture.md) is split in
two. Phase 2a makes every statement **correct** on a multi-group cluster.
Phase 2b makes the common ones **fast** (aggregate, sort and limit pushdown,
colocated join pushdown, row routing for bulk loads).

## Goals of phase 2a

1. A transaction that writes on more than one node commits atomically (2PC),
   and a transaction left prepared by a crash is resolved automatically.
2. Catalog synchronization and cross-node DDL are atomic.
3. Reference tables.
4. DDL on distributed and reference tables is propagated, or rejected; never
   silently applied to one node.
5. Any `SELECT` works when it spans shard groups, by an always-correct scan
   that fetches the rows of the other groups.
6. `UPDATE` and `DELETE` that span shard groups work when they can be shipped
   whole.

Not in phase 2a: aggregate/sort/limit pushdown and colocated join pushdown
(phase 2b); multi-row `INSERT`, `INSERT ... SELECT` and `COPY FROM` whose
rows belong to several groups (phase 2b; until then they raise the existing
"not supported yet" error instead of misplacing rows); a global snapshot
(phase 8); distributed deadlock detection (phase 5).

## 1. Two-phase commit

### When it is used

At pre-commit, count the participants that **wrote**: each remote node whose
connection executed a writing statement in this transaction, plus the local
node if the local transaction has an assigned transaction id and wrote
anything.

- Zero or one writer: as in phase 1. Remote transactions are committed in the
  pre-commit callback; read-only remote transactions are always committed
  this way.
- Two or more writers: 2PC.

The phase 1 errors "a transaction may write on at most one remote group" and
"may not write both locally and remotely" are removed.

### Protocol

1. **Prepare.** In the pre-commit callback, on every remote writer:
   `PREPARE TRANSACTION '<gid>'`. If any prepare fails, the local transaction
   aborts, and the abort callback rolls back what was prepared
   (`ROLLBACK PREPARED`) and what was not (`ROLLBACK`).
2. **Decide.** Still in pre-commit, insert one row per prepared remote into
   the node-local table `kingres.dtx_log (gid text primary key, node_id int
   not null, prepared_at timestamptz not null)`. The row is written by the
   local transaction itself, so it becomes durable exactly when the local
   commit does. The presence of the row *is* the commit decision.
3. **Commit.** After the local commit (the `XACT_EVENT_COMMIT` callback):
   `COMMIT PREPARED '<gid>'` on every remote. Errors here cannot abort
   anything; they are logged as warnings and left to recovery. The rows in
   `dtx_log` are removed later by recovery, not in this callback, because the
   callback cannot run a transaction.

`gid` format: `kingres_<origin node id>_<origin backend pid>_<local xid as
unsigned 64-bit full transaction id>_<target node id>`. It is unique per
target, and it names the origin so that recovery knows which node owns the
decision.

A local transaction that used 2PC must have a transaction id even if it wrote
nothing locally before step 2; inserting into `dtx_log` assigns one.

`max_prepared_transactions` must be greater than zero on every node. The
extension checks the local value when it is loaded and logs a warning if it
is zero; a prepare that fails remotely for that reason is reported with a
hint naming the setting.

A session may not use `PREPARE TRANSACTION` itself on a transaction that has
remote participants; this is rejected with a clear error (it already is in
phase 1; keep the check and test it).

### Recovery

`kingres.recover_prepared_transactions() returns int` (number resolved) runs
on a node and resolves the prepared transactions **this node originated**:

for each active primary node (including itself), list `pg_prepared_xacts`
entries whose gid has this node's id as origin; for each:

- a matching row exists in the local `dtx_log`: `COMMIT PREPARED`;
- no row, and the origin backend's transaction is no longer in progress
  (the full transaction id in the gid is not running locally):
  `ROLLBACK PREPARED`;
- no row, and that transaction is still in progress: leave it; it is between
  prepare and commit.

Then delete from `dtx_log` every row whose gid is no longer prepared on its
target node. A target that cannot be reached is skipped and retried later.

A **background worker** runs this automatically. A launcher is registered at
library load; every `kingres_shard.recovery_interval` (default 30s, 0
disables) it starts, for each database in which the extension is installed, a
short-lived worker that calls the function. The worker does nothing on a
standby.

The transaction-id test must be safe against transaction id wraparound (use
full transaction ids) and against a restarted origin (after a restart no old
backend transaction is in progress, so an undecided prepared transaction is
rolled back, which is correct because its `dtx_log` row would have survived
if the decision had been made).

### What 2PC does not give

Atomicity, not a global snapshot: a concurrent reader on another node can see
the transaction's effects on one group before the other. This is the
documented phase 1 anomaly and stays until phase 8. The README must say so.

## 2. Atomic catalog changes and DDL

`sync_catalog()`, `add_node`, `remove_node`, `create_distributed_table` and
everything in sections 3 and 4 use the same 2PC path: all remote changes are
prepared before the local commit. A crash can no longer leave one node with a
different catalog; the phase 1 limitation is removed from the README.

All catalog-changing functions take one cluster-wide lock on the map group
primary (`pg_advisory_xact_lock` on a fixed key) so that two of them cannot
interleave.

## 3. Reference tables

`kingres.create_reference_table(relation regclass) returns void`, only on
the map group primary. A reference table has `method = 'reference'` in
`kingres.dist_table`, no distribution column and no colocation group
(`dist_column` and `colocation_id` become nullable for this method; adjust
the constraints in the upgrade script). The table must be an ordinary table;
its current rows are copied to every other node as part of the call.

- **Reads** are always local: every node has the full table.
- **Writes** (`INSERT`, `UPDATE`, `DELETE`, `TRUNCATE`) are executed on every
  active primary node, in one 2PC transaction: the statement runs locally and
  is forwarded to all the others. A write that uses a volatile function or a
  sequence default would produce different rows on different nodes; such
  statements are rejected with a clear error unless the value can be computed
  once on the originating node and shipped as a constant (out of scope here:
  reject).
- **Routing with reference tables.** A statement that joins distributed
  tables with reference tables is routed exactly as if the reference tables
  were not there, because every group has them.
- A new node receives the reference tables (schema and rows) in `add_node`.
- A foreign key from a distributed table to a reference table is allowed. A
  foreign key from a reference table to a distributed table is rejected.

## 4. DDL propagation

A `ProcessUtility` hook (chained) intercepts utility statements that name a
distributed or reference table. On the map group primary the statement is
executed locally and then on every other node, in the same 2PC transaction.
On any other node it is rejected with a hint to run it on the map group
primary (forwarding DDL to the map group is phase 5).

Supported in phase 2a:

- `ALTER TABLE`: `ADD COLUMN`, `DROP COLUMN` (not the distribution column),
  `ALTER COLUMN ... SET/DROP DEFAULT`, `SET/DROP NOT NULL`, `ADD CONSTRAINT`
  (check, unique or primary key that includes the distribution column,
  foreign key to a colocated table on the distribution column or to a
  reference table), `DROP CONSTRAINT`, `RENAME COLUMN`, `RENAME TO`,
  `OWNER TO`.
- `CREATE INDEX` and `DROP INDEX` (not `CONCURRENTLY`).
- `DROP TABLE` (the table is removed from the catalog on every node).
- `TRUNCATE`.
- `GRANT` and `REVOKE` on the table.

For a distributed table the statement is applied to the parent; PostgreSQL
applies it to the bucket partitions that exist on each node.

Everything else that names a distributed or reference table
(`ALTER COLUMN TYPE`, `SET SCHEMA`, `ATTACH/DETACH PARTITION`, triggers,
rules, row-level security policies, `CLUSTER`, `CREATE INDEX CONCURRENTLY`,
`COPY ... FROM` with rows for other groups, and so on) is **rejected** with
`ERROR: <statement> is not supported on distributed tables yet`. Maintenance
commands that are safe to run per node (`VACUUM`, `ANALYZE`, `REINDEX`) are
allowed and stay local.

The statement text sent to the other nodes is deparsed from the parse tree
where PostgreSQL offers a deparser and otherwise taken from the original
query string, with `search_path` set on the remote session to the local
session's value so that names resolve identically. A multi-statement query
string is never forwarded as text; that case is rejected.

## 5. Statements that span shard groups: the distributed scan

The always-correct fallback for `SELECT`. When the planner hook's routing
decision is "spans groups" for a `SELECT` (instead of raising the phase 1
error), planning continues with the standard planner, and a
`set_rel_pathlist_hook` replaces the paths of every distributed table in the
query by one custom path, **"Kingres Distributed Scan"**:

- Its child is the best local path for the table (the local buckets).
- At execution it also sends one query to the primary of every *other* group
  that owns buckets of the table: `SELECT <columns needed> FROM <table>
  WHERE <pushed-down restrictions>`, and returns the local rows and the
  remote rows as one stream. PostgreSQL then joins, aggregates, sorts and
  limits on top of it as usual, so every query shape gives a correct answer.
- **Pushed-down restrictions.** A restriction clause is sent to the other
  groups only when it is safe: it references only this table's columns,
  constants and external parameters, and uses only built-in immutable
  functions and operators on built-in types with default collations. It is
  deparsed with PostgreSQL's `deparse_expression`. Clauses that are not
  pushed down are evaluated locally on the fetched rows; pushed-down clauses
  are evaluated locally too (as a recheck), which is always correct.
- **Bucket pruning.** If the restrictions fix the distribution key, phase 1
  routing has already handled the statement. Otherwise all groups are asked.
- Remote queries of one scan run concurrently (libpq asynchronous mode),
  with rows fetched in batches (single-row or chunked mode) so that a large
  table does not have to fit in memory.
- The scan supports rescan (it re-runs the remote queries) and is not
  parallel-aware.
- Each fragment runs in the remote transaction of the local transaction, as
  in phase 1.

Row-level locking clauses (`FOR UPDATE` and friends) on a statement that
spans groups are rejected in phase 2a.

`EXPLAIN` shows the node, the number of remote groups and the remote SQL.

A setting `kingres_shard.enable_distributed_scan` (default on) turns the
fallback off, in which case the phase 1 error is raised; operators who want
to forbid slow cross-group queries can use it.

## 6. `UPDATE` and `DELETE` that span shard groups

When an `UPDATE` or `DELETE` on a distributed table is not restricted to one
bucket, it is executed on every group that owns buckets of the table, in one
2PC transaction, and the affected-row counts are added, **if** the whole
statement can be shipped: it references only that table (and optionally
reference tables), has no sub-selects on distributed tables, no `RETURNING`
of volatile expressions, uses only built-in immutable functions and
operators (the test of section 5, applied to every expression in the
statement), and does not change the distribution column. With `RETURNING`,
the rows of all groups are returned.

Anything else raises `ERROR: this UPDATE/DELETE spans shard groups and
cannot be shipped` with a hint to restrict it by the distribution column.

An `UPDATE` that changes the distribution column is always rejected (it
would have to move the row between groups).

## Catalog changes in version 0.2

- `kingres.dist_table.method` allows `'reference'`; `dist_column` and
  `colocation_id` are nullable when the method is `reference` (a check
  constraint ties the three together).
- New node-local table `kingres.dtx_log`.
- New functions: `create_reference_table`, `recover_prepared_transactions`.
- New settings: `kingres_shard.recovery_interval`,
  `kingres_shard.enable_distributed_scan`.

The upgrade script must bring a 0.1 catalog to 0.2 without losing data, and a
regression test must exercise `ALTER EXTENSION kingres_shard UPDATE`.

## Tests required

Single node (pg_regress): reference table creation and DML; each supported
DDL statement and a sample of rejected ones; the upgrade from 0.1.

Multi-node (TAP, three nodes unless stated):

- 2PC: a transaction writing on two and three nodes commits everywhere;
  rollback leaves nothing prepared; a prepare failure on one node rolls back
  all; `pg_prepared_xacts` is empty afterwards on all nodes.
- Recovery, with fault injection by stopping a node or killing the origin
  backend at each stage: after prepare and before the local commit (must roll
  back everywhere); after the local commit and before `COMMIT PREPARED` (must
  commit everywhere); target node down during commit and back later; origin
  node restarted. Both through the SQL function and through the background
  worker.
- Atomic catalog change: a failure during `create_distributed_table` leaves
  no node with the table or the catalog rows.
- Reference tables: rows equal on all nodes after writes from any node;
  rollback; rejection of volatile writes; join of a distributed table with a
  reference table routed to one group; a new node receives the tables.
- DDL: each supported statement visible on all nodes and in all bucket
  partitions; rejection on a non-map node; rejected statements leave nothing
  behind.
- Distributed scan: the result of a set of queries (filter without key,
  join of two distributed tables on a non-key column, join with a plain local
  table, aggregate with `GROUP BY`, `ORDER BY ... LIMIT`, sub-select, CTE,
  window function, set operation, a correlated sub-select, a prepared
  statement run six times so that a generic plan is considered) is identical
  to the result of the same query on a single-node cluster holding the same
  data. This comparison test is the main correctness guard; add queries to it
  freely.
- Cross-group `UPDATE`/`DELETE`: counts and resulting rows match the
  single-node cluster; the non-shippable cases are rejected.
- A large-result test (enough rows that a remote fragment cannot be held in
  memory at once with a small `work_mem`) to confirm batched fetching.
