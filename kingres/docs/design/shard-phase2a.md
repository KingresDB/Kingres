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

## Implementation notes and deviations

Added when `contrib/kingres_shard` version 0.2 was implemented, on PostgreSQL
18.7. Everything above is implemented as written except what is listed here;
each item says what differs and why. "The extension" is `kingres_shard`.

**What the specification got wrong about PostgreSQL 18, or left open**

- *The recovery rule of section 1 is not enough after a crash.* "The full
  transaction id in the gid is not running locally" assumes that an id that was
  handed out stays handed out. A backend that dies by `SIGKILL` after assigning an
  id but before its WAL reaches the disk leaves the id unused: after the restart
  the next transaction gets it again, and recovery then sees "that id is in
  progress" and leaves the prepared transaction alone (the recovery function's own
  transaction can be the one that got the id). The rule implemented: an id at or
  above the next id of the node cannot belong to a live backend; an id below it is
  "running" only if `TransactionIdIsInProgress()` is true *and*
  `BackendXidGetPid()` is the pid in the gid.
- *`max_prepared_transactions` defaults to 0*, so every node of every test needs it
  set (the TAP helpers and `kingres_shard.conf` do). The library warns at load when
  it is 0.
- *A partitioned table loses its paths after the hook.* For a query with one
  partitioned base relation, `apply_scanjoin_target_to_paths()` discards the paths
  of the partitioned relation and rebuilds them from the paths of its partitions,
  which would read the local buckets only. The distributed scan therefore makes the
  table stop looking partitioned (`rel->part_scheme = NULL`, `nparts = 0`) when it
  installs its path, and partitionwise join and aggregation are switched off while
  such a statement is planned. Members of a `UNION ALL` arrive as
  `RELOPT_OTHER_MEMBER_REL`; the hook handles them too.
- *The target of the path is not the target of the plan.* By the time
  `PlanCustomPath` runs the planner has replaced the path target (order, extra
  expressions); the scan tuple is therefore built from the target list of the child
  plan, and an expression the planner computes on the table (a `COALESCE` of
  columns) is evaluated here on the rows of the other groups from the columns
  fetched.
- *`DECLARE CURSOR` takes parameters in the extended protocol*, which lets one
  fragment text carry the external parameters of the statement.
- *PostgreSQL 18 has no deparser for `ALTER TABLE`*, so the original statement text
  is always what is sent (section 4 allowed that).
- *`COPY` of a partitioned table to `STDOUT` is refused by PostgreSQL itself*; the
  hook leaves `COPY ... TO` alone and refuses every `COPY ... FROM` on a
  distributed table (not only the rows for other groups: the router cannot see
  which rows those are).
- *The validation of a new foreign key* (a join of the partitions of the
  referencing table with the whole referenced table) is run by PostgreSQL inside
  the `ALTER TABLE` and must read the node's own buckets on every node. While a
  propagated DDL statement runs, the planner hook therefore does no routing
  (`kingres_shard_in_authorized_ddl()`). Foreign keys are accepted only between
  colocated tables on the distribution column and to reference tables, which is
  what makes that correct.

**Two-phase commit and recovery**

- *The cluster-wide lock* is the exclusive lock on `kingres.map_version` of phase 1
  (review finding 4), not `pg_advisory_xact_lock`: it also blocks a hand-made change
  of a catalog table and cannot collide with an application's advisory keys.
  `create_reference_table` and the propagated DDL take it too.
- *gid.* The format is as specified. When one transaction has two connections to one
  node (two roles after `SET ROLE`), the second gid gets a `_<n>` suffix.
- *`kingres.dtx_log` rows are removed only if they are older than the moment the
  prepared transactions of their node were listed* and are no longer prepared;
  otherwise a transaction that prepared and committed between the listing and the
  cleanup would lose its decision while still prepared and be rolled back by the
  next pass. Rows of nodes that are no longer registered are removed.
- *`kingres.pending_node` (a second node-local table, not in the spec).* The
  registration of a new node is part of the transaction that may abort or crash, so
  after a crash between prepare and commit of `add_node` the prepared transaction on
  the new node belongs to no registered node and recovery would never look there.
  `add_node` therefore records the address in this table, in a transaction of its
  own (through a connection to the node itself), before it touches the new node;
  recovery looks at those addresses too and forgets them once the node is registered
  or after an hour.
- *Order at commit.* Read-only remote participants are committed first, then the
  writers are prepared. A single writer is committed directly (no `dtx_log` row); a
  transaction whose only writer is the local node commits as before.
- *The decision row is inserted with the heap API* (`CatalogTupleInsert`) in the
  pre-commit callback, with a snapshot pushed for the unique-index check; the
  committing user needs no privilege on `dtx_log`.
- *The recovery function* refuses to run in a transaction that has operated on
  remote nodes, in a `REPEATABLE READ` or `SERIALIZABLE` transaction (it needs a
  fresh snapshot for the `dtx_log` lookup after the in-progress test) and on a
  standby. `EXECUTE` is revoked from `PUBLIC`.
- *The workers.* One launcher (restarted after 5 s) and one short-lived worker per
  connectable database, started and awaited one after the other. The workers run
  as the bootstrap superuser with a pinned `search_path`, so that role's name must
  exist on every node (as for `add_node`).
- *Fault injection* is the superuser-only developer setting
  `kingres_shard.fault_injection`, as the task allowed.
- `kingres_shard.recovery_interval` is `sighup` (0 disables). The regression
  configuration sets it to 0 so that the worker cannot hold a database that
  `pg_regress` wants to drop.

**Reference tables**

- A write is accepted only if every expression is a built-in *immutable* function or
  operator on built-in types (not merely non-volatile): `now()` and every other
  `STABLE` function differ between nodes and are rejected with the volatile ones. It
  may read only reference tables.
- The statement runs node after node in node-id order (the local node through SPI
  in its turn), so concurrent writers take row locks in the same order on every
  node. The rows and the count returned are those of the local copy; a statement
  that affects a different number of rows on two nodes raises an error ("the copies
  of a reference table differ"), which aborts the transaction.
- A statement that writes a reference table and reads a distributed table is "not
  supported yet".
- `create_reference_table` copies the rows with multi-row `INSERT`s with text-form
  parameters under the transmission modes (generated columns are left to be
  computed, identity values are kept).

**DDL propagation**

- The list of section 4 is implemented, plus `VALIDATE CONSTRAINT` (needed for `NOT
  VALID` constraints) and `COMMENT ON TABLE` / `COLUMN` (a comment would otherwise
  exist on one node only). `ADD COLUMN` accepts a constant default only and no
  `serial` or identity (they would create a different sequence per node); generated
  columns are allowed (their expression must be immutable). `DROP COLUMN ...
  CASCADE` is refused.
- `CLUSTER` on a distributed or reference table is now refused (the spec lists it
  among the rejected statements; phase 1 allowed it).
- *The forwarded flag is also the "this DDL was propagated" marker*, and any client
  can give it: a table owner who connects to a non-map node with
  `kingres_shard.forwarded=on` can run the supported statements there without
  propagation. Privilege checks are unchanged, and the owner could do the same with
  `enable_routing = off` as a superuser; the README says so. A stronger marker needs
  an authenticated channel between nodes (a later phase).
- The text of a DDL statement is sent after `set_config(..., is_local)` of
  `search_path`, `DateStyle`, `IntervalStyle`, `standard_conforming_strings`,
  `bytea_output` and `extra_float_digits` of the session; the fixed settings are put
  back afterwards. A query string with several statements is refused.
- A foreign key from an ordinary table to a distributed table is refused (it cannot
  be enforced).

**Distributed scan**

- The remote fragment is `DECLARE ks_scan_n NO SCROLL CURSOR FOR SELECT <columns>
  FROM <table> AS ks_t WHERE <shippable clauses>`, read with `FETCH FORWARD
  kingres_shard.scan_fetch_size` (a new setting, default 1000). It starts with the
  comment `/* kingres:scan-fragment */`: a forwarded session runs a plain read of one
  distributed table without the bucket check only with that marker, so a routed
  statement of the same shape sent to a node that no longer owns the bucket is
  still `KS001`. A client can write the comment too; what it gets is the rows of
  this node of a table it may read.
- No bucket pruning: every group that owns a bucket of the table is asked, even if
  a restriction fixes the key of that table (range table indexes change when the
  planner pulls up subqueries, so the key analysis of the routing hook cannot be
  matched to the plan). The restriction is shipped, so the other groups return
  nothing.
- Row locking, system columns and `PlaceHolderVar`s computed on the table are "not
  supported yet". A `SELECT` of a SQL-language function that the planner expands is
  now read through the scan instead of being refused.
- A write to an ordinary table that reads a distributed table (`INSERT INTO local
  SELECT ... FROM dist`, `CREATE TABLE AS`) uses the scan as well.
- The scan asks the groups of the node's current bucket map; nothing checks the
  remote map version (buckets do not move in this phase).

**UPDATE and DELETE**

- "No sub-selects on distributed tables" is implemented as: the distributed table
  appears once, as the target; any other reference to it (a subquery, a self join)
  is refused, because group-by-group execution would see only the rows of one group.
- An `UPDATE` that assigns the distribution column is refused always (also when it
  would be routed to one bucket); the hint is DELETE and INSERT.
- Each group's part carries `/* kingres:write-fragment */`; a forwarded session runs
  such a statement on its own buckets if it is shippable.

**Node connection options (not in the spec)**

The forbidden keywords are `host`, `hostaddr`, `port`, `dbname`, `user`, `password`,
`options`, `service`, `client_encoding`, `application_name`, `connect_timeout` and
`replication` (the last six because the extension controls them or they would change
the kind of connection). The check hook replaces a rejected value by a placeholder so
that the generic server message does not print it; connection errors have the values
of `sslcert`, `sslkey`, `sslrootcert`, `sslcrl`, `sslcrldir`, `sslpassword`,
`sslkeylogfile` and `passfile` replaced by `<hidden>`. The server logs a value of
`postgresql.conf` in full when the file is reloaded; the README says so. `sslmode`
and the other SSL options apply to TCP connections only (libpq ignores them on Unix
sockets), which the test accounts for.

## Fix round for the review of 2026-11-14 (S2-01 to S2-20), implementation notes

Written during the fix round of `reviews/2026-11-14-kingres-shard-0.2.md`. These notes
override the text above where they differ.

**Recovery (S2-01, S2-02).** The invariant: a prepared transaction is rolled back only if
(a) the origin transaction is established as no longer in progress, and then (b) a lookup of
its decision row with a snapshot taken after (a) finds nothing. `recovery.c` has one
function, `origin_decision()`, that performs (a) then (b); (b) pushes
`GetLatestSnapshot()` around its SPI call, because a read-only SPI call uses the snapshot
that was active when the statement (or the worker's transaction) began. A stress test runs
two recoveries at once against concurrent multi-node commits (it lost 128 participants
before the fix, far more often than the review's estimate). The other reads of the path
were examined: the listing of prepared transactions needs no snapshot ordering (a
transaction prepared after it is simply not listed; the purge removes only rows older than
the listing); two recoveries at once decide the same way from the same facts, and a
`COMMIT PREPARED` or `ROLLBACK PREPARED` of a transaction the other run resolved fails
with "does not exist", which is ignored; the decision row of a gid is deleted only after
its prepared transaction is gone. `log_decision()` calls `ForceSyncCommit()` so the commit
that carries the decision rows is flushed before any `COMMIT PREPARED`.

**Pushdown (S2-03, S2-05).** `shippable.c` no longer ships `CoerceViaIO`; the walker refuses
system columns, `LIMIT`/`OFFSET`, `DISTINCT ON`, `TABLESAMPLE`, window functions,
order-sensitive aggregates and an `UPDATE ... FROM` not matched through a unique key, in
one place (`query_ok`, `ship_walker`, `agg_order_sensitive`, `matches_at_most_one`). The
harness `KingresShardCompare.pm` repeats every `compare()` under four session profiles.

**Reference writes (S2-06, S2-07).** Writers serialize with advisory transaction locks
`(KS_REFWRITE_LOCK_CLASS, hashtext(qualified name))`, taken on every node in node-id order
and table-name order (LOCK TABLE ... SHARE ROW EXCLUSIVE would need UPDATE/DELETE/TRUNCATE).
After the locks are held the statement checks that the node set is unchanged;
`add_node()` takes the same locks first.

**Forwarded sessions (S2-04).** `forward_auth.c`: `kingres.cluster_secret`, the token
`HMAC-SHA-256(secret, "kingres-forward-v1" NUL role NUL target node id)` sent as the
connection option `kingres_shard.forward_token` (rewritten to `<hidden>` by its check hook),
verified at the first statement with catalog snapshots only (a transaction snapshot taken
there would break `SET TRANSACTION ISOLATION LEVEL`). `add_node()` tells the connection layer
the id the new node will get. A node without a secret accepts the claim (bootstrap and
rolling upgrade); `sync_catalog()` makes and distributes the secret.

**Parallel (S2-08).** Routed statements for local buckets still plan and run in workers
(they only read local partitions); the scan path sets `consider_parallel = false` on the
relation and every path that sends work to another node refuses in a worker.

**Other fixes.** S2-11/12: `kingres_shard.node_timeout`, `kingres_shard_bounded_exec()` for
every wait that cannot throw, a per-database deadline in the launcher, fixed worker settings.
S2-13: `change_partition_owners()`. S2-14: three separate counters in `utility.c`; only the
foreign-key validation join (`SELECT fk.`) is planned without routing. S2-15: refused, not
emulated (`kingres_shard_refuse_view_owner`). S2-16: `PGC_SIGHUP`, no `sslpassword`.
