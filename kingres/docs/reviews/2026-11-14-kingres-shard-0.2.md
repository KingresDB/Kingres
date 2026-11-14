<!-- SPDX-License-Identifier: PostgreSQL -->
# Adversarial review: contrib/kingres_shard 0.1 -> 0.2

Reviewer: independent (not the author). Reviewed state: the tree of
`contrib/kingres_shard` as of commit c283dbf6bc ("version 0.2 ... phase 2a"),
which is also the working tree I read (the 0.2 changes were committed while the
review ran; file modification times show nothing changed after I read them).

## Scope and method

Read completely: every file under `src/` (all `.c` and `.h`), both SQL scripts
(`kingres_shard--0.2.sql` in full, `kingres_shard--0.1--0.2.sql` against it),
`README.md` (guarantees, forwarded flag, limitations), `shard-phase2a.md` with
its implementation notes, the 0.1 review, and the test helpers
(`KingresShardTest.pm`, `fault.c`). I did not re-read the 18 TAP files line by
line; I used them to see what is claimed to be covered.

Ran: a copy of the installed 0.2 server under WSL2 (`~/kw-review-shard`, ports
57501-57505, 57511-57513, all removed afterwards). Experiments:

- a three-node cluster (one group per node) plus a plain single node holding
  the same data (tables with int, text, numeric, timestamptz, collate "C",
  array, enum, domain, composite, range, 100 kB values, a dropped column and a
  generated column; two colocated distributed tables, one reference table, one
  local table), and a battery of 96 queries run on all three cluster nodes and
  on the single node under five profiles (defaults; partitionwise join and
  aggregate on with forced parallel query; nested loop only; merge join only;
  hash join with 64 kB work_mem, partition pruning off, JIT on). Shapes:
  filters, grouping sets/rollup/cube, DISTINCT ON, window functions, left/full
  joins, correlated sub-selects and LATERAL (rescans), UNION/INTERSECT/EXCEPT,
  recursive and (not) materialized CTEs, row comparison, IS DISTINCT FROM,
  to_char/to_tsvector/regclass clauses, whole-row references, large values.
  Plus scripted sessions: scroll and WITH HOLD cursors with backward fetch,
  prepared statements under every `plan_cache_mode`, PL/pgSQL and SQL
  functions, views and security-barrier views, reading own writes, savepoints,
  errors in a transaction, EXPLAIN, LIMIT, `FOR UPDATE` (rejected cleanly),
  cross-group UPDATE/DELETE (counts and resulting rows), writes to local tables
  that read distributed ones;
- reference-table experiments (forwarded sessions, ctid, concurrent writers,
  add_node during a write, DDL defaults);
- crash experiments (`pg_ctl stop -m immediate`, `kingres_shard.fault_injection`
  waits), a 25 s pgbench load of two-node transactions against a recovery
  worker running every second;
- `node_conninfo` visibility as an ordinary role; `ALTER DATABASE ... SET`
  against the recovery worker;
- a rolling upgrade 0.1 -> 0.2 of a three-node cluster holding data (node 3,
  then 2, then 1), and a comparison of the resulting catalogs with a fresh 0.2
  node (tables, columns, constraints, indexes, function definitions, ACLs,
  triggers, comments, event triggers: identical, 683 lines each).

Results that held up: all 96 comparison queries return the same rows as the
single node under all five profiles (the one difference, `tableoid`, is a
clean documented error); the part_scheme/nparts trick survived partitionwise
settings, forced parallel query, UNION ALL members, LATERAL, nested loop with
the scan inside, merge join, spilling hash join, cursors, `LIMIT`, prepared
statements and PL/pgSQL.

Findings are most severe first. "Reproduced" means I observed it; "reasoned"
means I read the code and did not run it.

## Findings

### S2-01 (critical) Recovery decides with a stale snapshot and can roll back a transaction that committed

`recovery.c` 160-172 (`decision_logged`), 337-342 (loop), 543 (worker snapshot).

The file header says the `dtx_log` lookup happens "after [the in-progress test],
with a fresh snapshot". It does not. `decision_logged()` calls
`SPI_execute_with_args(..., read_only = true, ...)`, and for a read-only SPI
call PostgreSQL 18 uses the snapshot that was active on entry (spi.c,
`_SPI_execute_plan`, "snapshot == InvalidSnapshot, read_only = true: ... use
the entry-time ActiveSnapshot"). That is the statement snapshot of
`SELECT kingres.recover_prepared_transactions()`, or, in the worker, the one
`PushActiveSnapshot(GetTransactionSnapshot())` at the start of
`kingres_shard_recovery_worker_main` - taken before any node was contacted.
A transaction T that commits between that snapshot and the check has its
`dtx_log` row invisible to it. The order of tests in the loop is then:
`xid_possibly_running()` says "not running" (T has committed), the row lookup
says "no decision", so `ROLLBACK PREPARED` is sent to a participant of a
transaction that committed on the origin. T's own `COMMIT PREPARED` then fails
with a WARNING only, and the client already has COMMIT. Half-committed write.

Reproduced deterministically (the window is widened by making the statement
snapshot old: `SELECT pg_sleep(6), kingres.recover_prepared_transactions()` on
node 1; meanwhile a session with `kingres_shard.fault_injection =
'after_commit:wait:<file>'` runs `begin; insert into d values (1000,..);
insert into d values (1002,..); commit;` and stops after the local commit):

```
-- recovery output: |1            (one prepared transaction resolved)
-- T's session:  WARNING: could not commit prepared transaction
--   "kingres_1_607697_830_2" on node 127.0.0.1:57502; recovery will commit it
--   DETAIL: ERROR: prepared transaction with identifier ... does not exist
-- afterwards:  node 1 has key 1000, node 2 has no key 1002
```

Natural window: from the worker's snapshot to the check of the gid. In my
25 s pgbench run (16 clients, ~1850 transactions/s, worker every second) it was
not hit, because on one machine it is microseconds. It becomes seconds when the
worker pass is slow: each unreachable node costs `node_connect_timeout` (5 s) of
snapshot age, and a participant whose `COMMIT PREPARED` is delayed (the
postcommit call is sequential and unbounded, see S2-12) stays listed as
prepared while its transaction is already committed. That combination is an
ordinary failure (a partition or a slow node), so I rate it critical on the
consequence; the natural frequency is a guess.

Fix: run the lookup with a fresh snapshot taken after the in-progress test:
non-read-only SPI (a new snapshot per statement in READ COMMITTED), or
`PushActiveSnapshot(GetLatestSnapshot())` around the lookup. Add a test that
holds T after its local commit while a recovery that began earlier finishes.

### S2-02 (critical) Remote `COMMIT PREPARED` is sent before the local commit is durable (`synchronous_commit = off`)

`node_conn.c` `log_decision()` / `postcommit()` (1206-1245); no `ForceSyncCommit()`.

The design says the decision row "becomes durable exactly when the local commit
does". With `synchronous_commit = off` (session, role, database or server
setting) the commit record is still in WAL buffers when the `XACT_EVENT_COMMIT`
callback sends `COMMIT PREPARED` to the participants, and `COMMIT PREPARED` on a
participant is always flushed. If the origin then crashes, its commit and its
`dtx_log` row are gone but the participants have committed.

Reproduced (`wal_writer_delay = 10s`, then `set synchronous_commit = off; begin;
insert into d values (1004,'sc-local'); insert into d values (1005,'sc-remote');
commit;` and `pg_ctl stop -m immediate` on node 1 right after the COMMIT):

```
after crash of node1:
n1 local key 1004|0
node2 sees 1005|1
```

The client had received COMMIT. Recovery cannot repair this (no row, nothing
prepared). Fix: call `ForceSyncCommit()` (xact.h) in `log_decision()` when at
least one remote was prepared, so the commit record is flushed (and waits for
synchronous standbys) before the callback returns; document it.

### S2-03 (critical) Pushed-down scan restrictions are evaluated under different session settings than the local semantics: rows of other groups silently missing

`shippable.c` 242-253 (`T_CoerceViaIO` accepted for any built-in types),
`dist_scan.c` `ds_plan_custom_path`, `node_conn.c` (remote session fixed to
`DateStyle='ISO, MDY'`, `extra_float_digits=3`, hex bytea, ...).

A clause such as `z::text = '...'`, `(id::text || '/01/2024')::date = ...`, or
`d::text like ...` contains a `CoerceViaIO` between built-in types; it is
judged shippable and sent. The remote session evaluates it under the fixed
transmission settings; the local executor (which owns the semantics of the
statement) evaluates it under the user's session settings. When they differ the
other groups return the wrong subset and the local recheck cannot add rows
back. Same class for `interval::text` (IntervalStyle), `bytea::text`
(bytea_output), `float::text` (extra_float_digits), date/time input parsing
(DateStyle). Reproduced with `set datestyle = 'SQL, DMY'; set extra_float_digits = 0;`:

```
select id from t1 where id < 13 and (id::text || '/01/2024')::date = '2024-01-05';
  single node: 5        cluster, node 1 and node 2: (no rows)   -- key 5 is on node 3
select id from t2 where z::text = '0.333333333333333';
  single node: 1        cluster, node 1 and node 2: (no rows)
select id, to_char(d,'FMDay') from t1 where d::text like '%01/01/2024%' and id < 30;
  different row sets
```

This is wrong results with no error, reachable by any user who has set one of
these (very common: `DateStyle`, `extra_float_digits` in drivers). The
comparison test never sets a non-default session setting. This is the "session
settings leaking" class of the 0.1 review (S-C) reopened for the scan.
Fix: do not ship `CoerceViaIO` (and `FuncExpr`s such as `to_char` on these
types) when either type is float, timestamp/date/time/timestamptz, interval,
bytea, money or any type whose text form depends on a setting; or make the
fragment run under the session's own output settings. Add the case to
`t/013_dist_scan.pl` with `SET DateStyle`/`extra_float_digits`.

### S2-04 (critical) Any role with INSERT/UPDATE/DELETE on a reference table can make the copies differ

`planner.c` 1095 (`if (a.has_ref && !kingres_shard_forwarded)`), `kingres_shard.c`
(`forwarded` is `PGC_BACKEND`, settable in connection options by any client).

A forwarded session never fans a reference-table write out, so a client that
connects with `options=-c kingres_shard.forwarded=on` writes the local copy
only. The README ("The forwarded flag") argues that the flag grants no right
the user lacks and that the resulting inconsistency is one "the owner of a
table can also create with enable_routing = off as a superuser". That is not
accurate for reference tables: the user needs only the write privilege the
application already grants, not ownership or superuser. Reproduced as role
`bob` (INSERT/SELECT/UPDATE/DELETE on `r`, no ownership):

```
PGOPTIONS='-c kingres_shard.forwarded=on' psql -p <node2> -U bob
  insert into r values (99,99);                  -- INSERT 0 1
node 1: 0 rows with id 99   node 2: 1 row   node 3: 0 rows
```

Later ordinary writes of key 99 from another node fail with a duplicate-key
error on one node (after having changed the others in the same 2PC, then
aborted), and reads return different answers depending on the node. The same
flag on a distributed table is limited by the bucket check (I verified
`insert ... values (1000)` on the wrong node gives KS001, multi-row insert and
COPY are refused), so the damage is confined to reference tables and to
schema changes by owners (DDL without propagation, documented).

Fix: the origin must authenticate the forwarded role: the marker has to be
something a client cannot supply (for example a per-cluster secret checked by a
`PGC_SU_BACKEND` setting that only a superuser connection may set, with the
node-to-node connections made as a superuser-held proxy or via a signed token),
or reference-table writes in a session whose `forwarded` flag was not set by
the router must be refused. At minimum, make `forwarded` `PGC_SU_BACKEND` and
have the router connect as a role that is allowed to set it, and say in the
README that otherwise reference tables are only as consistent as their writers.

### S2-05 (critical) A reference-table write may use expressions that depend on physical layout: copies diverge silently

`shippable.c` `ship_walker` (statement mode lets system-column `Var`s through:
the `varattno <= 0` test is only made in clause mode, lines 161-170;
aggregates, window functions, `LIMIT` without `ORDER BY` and `FROM` joins of
reference tables are all accepted), `fanout.c` (equal row counts are the only
agreement check).

Reproduced: after deleting a row and running `VACUUM r` on node 2 only
(a local, allowed command), the physical row addresses of later inserts differ
between nodes. Then, as an ordinary user with UPDATE privilege:

```
update r set v = -1 where ctid = '(0,3)';      -- UPDATE 1 on every node, no error
node 1: id 50 updated   node 2: id 51 updated   node 3: id 50 updated
```

Equal counts (1 and 1) defeat the "copies of a reference table differ" check.
Other statements with the same property and no setting needed:
`insert into r2 select ... from r limit 1` (no ORDER BY), `update ... from r1`
with several matches, `array_agg`/`string_agg` without ORDER BY, `row_number()
over ()`, `tableoid`/`xmin`. Fix: in statement mode reject system columns,
`LIMIT/OFFSET` without a total order, window functions, ordered-set and
order-sensitive aggregates, and joins that can match several rows per target
row; or make the statement deterministic by evaluating it once on the origin and
shipping the resulting rows (the design's own alternative).

### S2-06 (critical) A reference-table write that reads a reference table runs on each node under a different snapshot: concurrent writers make the copies differ

`fanout.c` `run_all` (statement run node after node, each with its own
snapshot), `shippable.c` (sub-selects on reference tables are accepted).

Reproduced: node 1, `update r set v = (select count(*) from r s) where id = 2
and (select count(*) from generate_series(1,40000000)) > 0;` (slow on node 1) while
a second session inserts a row into `r` and commits during that time:

```
node 1: v = 3     node 2: v = 4     node 3: v = 4
```

No error, equal counts. Any `INSERT/UPDATE ... SELECT` that reads a reference
table has this property whenever another writer commits during the statement
(the other writer's `COMMIT PREPARED` reaches the nodes at different moments).
Fix: forbid reads of the written reference table (and of other reference
tables whose writers can run concurrently) inside a reference write, or take a
table-level lock on every node before the first statement runs, or evaluate on
the origin and ship rows (as in S2-05).

### S2-07 (high) add_node during traffic loses reference-table writes on the new node

`node_admin.c` `create_tables_on_node` / `kingres_add_node`
(no lock on the reference tables; the new node is not in other nodes' catalogs
until the transaction commits and the catalog reaches them).

`add_node` copies the rows of every reference table to the new node and holds
only the catalog lock. Writers on other nodes fan out to the nodes of their own
catalog, which does not yet contain the new node. Reproduced with
`set kingres_shard.fault_injection = 'after_prepare:wait:<file>'` in the
session that runs `add_node('127.0.0.1', 57505)`, an `insert into r values
(888,8)` on node 2 during the wait, then release:

```
node 1: 1  node 2: 1  node 3: 1  new node: 0
```

The window is the whole transaction plus the time until each node applies the
new catalog. Fix: take `SHARE ROW EXCLUSIVE` (or `EXCLUSIVE`) on every
reference table on every node before copying, and make reference writes check
the map version (or take the catalog lock in a share mode) so that a write
planned with the old node list cannot commit after the registration.

### S2-08 (high) Parallel-safe functions that read a distributed table give wrong answers in workers

`dist_scan.c` 320-324 (the path is parallel unsafe but the relation keeps
`consider_parallel`), `planner.c` (the forward path refuses `IsInParallelMode()`,
the scan path and `plan_with_scan` do not).

A function declared `PARALLEL SAFE` that runs `select count(*) from t2 where ...`
is planned inside the worker, where the distributed scan opens the worker's
own remote connection and remote transaction: it does not see the leader's
uncommitted writes and has its own snapshot. Reproduced: inside a transaction
that inserted three rows, `sum(fpar(0))` serial = 150 (3 per call), while the
parallel run returned calls with `min = 1`, `max = 3` (workers missing the
remote rows written by the leader). Fix: refuse in `ds_set_rel_pathlist` (or at
executor start) when `IsParallelWorker()`/`IsInParallelMode()`, as the forward
path does; README tells users to mark functions restricted only for forwarding.

### S2-09 (medium) The README understates the read anomaly

README "Guarantees" 88-96 describes read skew between groups ("reads each group
in the snapshot of that group's transaction"). It is worse: every scan node
(every `DECLARE`) of a statement has its own snapshot in the READ COMMITTED
remote transaction, so one statement that scans two distributed tables sees
different states **of the same node**, and a transaction that is atomic on one
node looks torn. Reproduced (node 2 local transaction updating `t1` and `t2`
row 6 together; reader on node 1: `select (select sum(a) from t1), pg_sleep(4),
(select sum(z) from t2)`):

```
before: sum(a) 1545, sum(z) 26733.33
reader: 1545 | 27733.33   (t1 old, t2 new)    after: 2545 | 27733.33
```

(Also the local buckets use the statement snapshot taken at the start while
remote ones use the snapshot of the later `DECLARE`.) It cannot invent
uncommitted data, so "never committed" in the README is right, but "one group
before another" should read "one scan before another, even on one node".
Fix: say so; for the REPEATABLE READ case consider `SET TRANSACTION SNAPSHOT`
import later; for READ COMMITTED declare all cursors of a statement up front.

### S2-10 (medium) `ADD COLUMN ... DEFAULT 'now'` passes the "constant default" check and differs per node

`utility.c` 355-362 (`A_Const` or `TypeCast` of `A_Const`).

`alter table r2 add column ts timestamptz default 'now'` on a reference table is
accepted (`default now()` is refused). PostgreSQL evaluates the special input
string at ALTER time on each node, so existing rows get different values:

```
57501|1|2026-11-14 20:47:49.972544+03:30
57502|1|2026-11-14 20:47:49.975782+03:30
57503|1|2026-11-14 20:47:49.981689+03:30
```

Same for `'today'`, `'tomorrow'`, `'yesterday'`, `'epoch'` is fine, `'infinity'`
fine. Fix: after transformation, require a `Const` whose type does not accept
these strings, or reject datetime-typed columns with a string default.

### S2-11 (medium) A per-database setting can switch the recovery worker off; one hung node stalls recovery for all databases

`recovery.c` 524-559 (`recovery_worker_main`), 462-500 (`launch_workers`).

(a) The worker connects with the database's default settings applied.
`ALTER DATABASE x SET default_transaction_isolation = 'repeatable read'`
(allowed for the database owner, a USERSET setting) makes every pass fail with
"must run in a READ COMMITTED transaction". Reproduced as superuser: the log
shows the error and `exited with exit code 1` every second, recovery never
runs. `statement_timeout = '1ms'`, `default_transaction_read_only = on` and
`role` give the same. Fix: set `default_transaction_isolation`,
`default_transaction_read_only`, `statement_timeout`, `lock_timeout`,
`idle_in_transaction_session_timeout` and `role` explicitly in the worker
(`set_config_option(..., PGC_S_OVERRIDE)`), as already done for `search_path`.

(b) The worker talks to the nodes with unbounded waits (`libpqsrv_get_result_last`
with no statement timeout; `PQexec` for `CLOSE`/rollback elsewhere). The launcher
runs the workers one after another and `WaitForBackgroundWorkerShutdown`s each,
so one node that accepts the connection and then stops answering (a partition
after connect: no reply, no reset; reasoned, not run) blocks that worker, and
therefore recovery of every other database and the next pass. Fix: a
`statement_timeout`/`tcp_user_timeout` on the node connections of the worker,
and a per-database deadline in the launcher (terminate the worker).

### S2-12 (medium) The post-commit phase is unbounded and uninterruptible

`node_conn.c` 1239 (`PQexec` for `COMMIT PREPARED` in `XACT_EVENT_COMMIT`),
also `abort_connection` (972, 988) and the subtransaction callback (1364).
A participant that stops answering after PREPARE holds the committing backend
(already committed locally) in a blocking `PQexec` that ignores cancel and
`statement_timeout`, while its prepared transaction holds locks; because the
participants are committed one after another, the later ones wait behind it
(and see S2-01). The author notes the limit. Fix: bounded wait (`tcp_user_timeout`,
a monotonic deadline using `libpqsrv_get_result` with a latch timeout), then give
up with a WARNING and leave it to recovery.

### S2-13 (medium) The owner of a distributed table cannot run any supported DDL after `OWNER TO`

`utility.c` (`AT_ChangeOwner` is propagated), PostgreSQL does not recurse
`OWNER TO` to partitions. After `alter table d3 owner to bob` (propagated, all
nodes), `bob`'s `alter table d3 add column zz int` fails with `must be owner of
table d3_b0000` (partitions stay owned by the creator, and renaming or altering a
partition is refused as structural). Only a superuser can then change the table,
so the "ordinary role owns a table and runs the supported DDL" path of the
specification does not work; as a side effect it also made me unable to test
user code running during DDL (S2-14) as an ordinary role. Fix: propagate and
apply `OWNER TO` to the bucket partitions (and indexes) too, on every node.

### S2-14 (medium, reasoned) User code runs with routing and the drop guard switched off while an authorized DDL statement executes

`utility.c` `ddl_authorized_depth` (incremented around the whole
`standard_ProcessUtility`), `planner.c` 1064-1070 (`kingres_shard_in_authorized_ddl()`
returns `pass_through`), `kingres.internal_ddl_authorized()` in the drop guard.
While `ALTER TABLE ... ADD CONSTRAINT CHECK (f(x))`, `CREATE INDEX` on an
expression, `ADD COLUMN ... GENERATED`, validation of a foreign key etc. run,
any function they call (user-defined, with the caller's rights) plans its own
statements with no routing: writes to reference tables are not fanned out,
reads of distributed tables are local only, and `DROP TABLE` of a distributed
table passes the event-trigger guard. This is the "bypass flag can be set while
user code runs" class. I could not turn it into an ordinary-role exploit because
of S2-13 and because create_distributed_table needs rights on the catalog, but a
role that is granted ownership of such a table by a superuser (by hand on the
partitions) can use it as a second route to S2-04. Fix: do not use a process-wide
depth counter; make the bypass specific to the statements PostgreSQL itself
generates for the DDL (the FK validation join), e.g. by recognising the query
string/`queryEnv`, or run the validation only through SPI with the flag set and
cleared around it.

### S2-15 (medium) Views over distributed tables do not work as an access barrier

`node_conn.c` `kingres_shard_get_conn` (connection per `GetUserId()`), the scan
and the forward path. A view owned by a privileged role and granted to `bob`
(`create view vsec as select id, a from t1; grant select on vsec to bob;`, bob
has no right on `t1`) works on a single node because PostgreSQL checks the
underlying table as the view owner. On the cluster:

```
bob: select count(*) from vsec                -> ERROR: permission denied for table t1  CONTEXT: remote node ...:57502
bob: select count(*) from vsec where id = 6   -> same
```

The local check passes (view owner), the remote fragment runs as `bob`.
Not a leak (errors, never extra rows), but a correctness/compatibility bug of
the "same rights on every node" claim. Fix: open the remote connection as the
`checkAsUser` of the range-table entry (or refuse with a clear message).

### S2-16 (medium) `node_conninfo` secrets are readable through role/database settings

`kingres_shard.c` (`PGC_SUSET`, `GUC_SUPERUSER_ONLY`). `SHOW`, `pg_settings` and
`current_setting` are blocked for ordinary roles (verified), but a value set with
`ALTER ROLE ... SET` or `ALTER DATABASE ... SET` is stored in `pg_db_role_setting`,
which every role can read (`pg_roles.rolconfig` too):

```
bob: select setrole::regrole, setconfig from pg_db_role_setting
  bob | {"kingres_shard.node_conninfo=sslpassword=topsecret sslkey=/k/key.pem"}
```

`sslpassword` is an accepted keyword. The README tells the administrator to put
the setting in `postgresql.conf`, but nothing stops the other route. Fix: make the
setting `PGC_SIGHUP` (file or `ALTER SYSTEM` only) and drop `sslpassword` from the
accepted keywords, or document the exposure.

### S2-17 (low/medium) Cancelled or failed scans leak libpq results; write-fragment and scan errors

`dist_scan.c` 744, 888: `DsFrag.res` (a `PGresult` of up to `scan_fetch_size`
rows, default 1000, maximum 1,000,000) is freed only by `ds_stop` through
`ds_end_scan`; an error in the executor never calls `ExecEndNode`, so each
aborted distributed scan leaks its current batches (libpq malloc memory, not
palloc) for the life of the backend, which matters for pooled sessions with
large rows (reasoned, not measured). Fix: register a memory-context reset
callback on the scan state that clears `res`.

### S2-18 (low) Statements that read distributed tables and write a local table: UPDATE ... FROM, DELETE ... USING, MERGE fail with a misleading message

`update lw set lab = d.b from t1 d where d.id = lw.k` fails with "system columns
of distributed table "t1" are not supported in a statement that spans shard
groups" because PostgreSQL asks for `ctid` of every non-target relation of an
UPDATE/DELETE (row mark). It is a clean error, and the implementation notes list
"a write to an ordinary table that reads a distributed table" as supported
(true for INSERT ... SELECT and CTAS only). Fix: README/limitations, and a
message that says "UPDATE/DELETE ... FROM/USING a distributed table".

### S2-19 (low) Non-atomic commit of a "read-only" remote participant that wrote through a function

`remote_scan.c` `is_write` is `commandType != CMD_SELECT || rowMarks || hasModifyingCTE`;
`node_conn.c` `precommit` commits non-writer participants before preparing the
writers. `select wr(v) from d where id = 1002` (routed SELECT calling a volatile
function that writes on the remote group) is not marked as a write, is committed
first and outside 2PC. Reasoned. Also in the no-2PC loop (`precommit` 1126-1142)
the single writer may be committed before a read-only participant whose COMMIT
then fails, so the client gets an error for a transaction whose write is
durable. Fix: treat any routed statement with a volatile function (or any call
to a non-built-in function) as a write; commit writers last.

### S2-20 (low) Other items

- No check that nodes share encoding, default collation and library version
  (`add_node` never compares them); shipped comparisons assume it. A new node with
  another collation silently returns different restrictions. Add a comparison of
  `datcollate/datctype/datlocprovider/encoding` in `add_node`.
- `pending_node` is purged by the origin's clock, and `dtx_log` rows with
  `node_id = 0` (target not registered when the gid was made) are never purged.
- `plan_with_scan` turns off `enable_partitionwise_join/aggregate` for every
  statement of every session on a multi-group cluster (also statements on
  unrelated partitioned tables), not only those that read a distributed table.
  Undocumented behavior change.
- `add_node` against a node still at 0.1 SQL fails with `relation
  "kingres.pending_node" does not exist` (no hint to upgrade); the hint exists for
  `dtx_log`.
- Hostile `PREPARE TRANSACTION 'kingres_<origin>_<pid>_<fxid>_<n>'` with the pid
  and xid of a live transaction of the origin (both visible in `pg_locks`) makes
  recovery leave it alone as "in progress" for as long as that transaction lives;
  a gid equal to a future real gid makes that transaction's PREPARE fail. Both need
  the right to connect and `max_prepared_transactions` slots; same exposure as any
  prepared transaction, noted for completeness. A user's own two-phase gids that
  start with `kingres_<node>_` are rolled back by recovery as superuser.
- `ds_plan_custom_path` passes `extract_actual_clauses(clauses)` without
  `order_qual_clauses`, so the security-level order of restrictions is not applied
  to rows of other groups. I tried a security-barrier view with a leaky function
  (cost 1e-7) and saw no leak (view quals came first in the list), so this is a
  hardening note, not a finding.

## Examined and found sound

- Distributed scan result equality (96 queries x 5 settings profiles x 3 nodes,
  see Scope), cursors incl. `WITH HOLD` and backward fetch, prepared statements
  under every `plan_cache_mode`, PL/pgSQL and SQL functions, security-barrier
  view, LIMIT early termination, reading own writes and writes before reads in
  the same transaction (local and remote), savepoints with rollback to
  savepoint, errors in the middle of a transaction, `FOR UPDATE` rejected cleanly,
  system columns rejected cleanly, parallel plans for statements that are planned in
  the leader.
- Cross-group UPDATE/DELETE: counts and resulting rows equal to the single node;
  the non-shippable shapes (sub-select on the target, `random()`, `now()`,
  `UPDATE` of the key, data-modifying CTE) fail with clear errors.
- Forged markers on a non-map node (bob): scan-fragment read returns this node's
  rows he may read; write-fragment `delete` affects only this node's rows he may
  delete; an insert for a bucket of another group is KS001; multi-row insert and
  `COPY FROM` are refused; `UPDATE d SET id = ...` to another group's key is
  refused or affects 0 rows. No privilege is gained beyond S2-04.
- Two-phase commit: `COMMIT AND CHAIN`, savepoints inside a multi-node
  transaction, user `PREPARE TRANSACTION` (rejected cleanly), `pg_prepared_xacts`
  empty afterwards on all nodes; kill of the origin after the local commit
  followed by recovery commits (author's tests, plus my fault-injection run).
- Upgrade 0.1 -> 0.2, node 3 then 2 then 1, with data: statements keep working,
  a multi-group transaction on a node still at 0.1 SQL fails with the hint "Run
  ALTER EXTENSION kingres_shard UPDATE", nothing is left prepared, and the final
  catalog is identical to a fresh 0.2 catalog.
- `node_conninfo`: `SHOW`, `pg_settings`, `current_setting`, `pg_file_settings`
  denied to an ordinary role; invalid or forbidden keywords are rejected without
  echoing the value (placeholder `<hidden>`).
- Extension loaded but not created in the database (`emptydb`): ordinary DDL/DML
  and drop work, no errors from the hooks.

## Not examined

Non-default database encodings and ICU providers; JIT behavior beyond `jit=on,
jit_above_cost=0` passing the battery (I did not confirm the build has LLVM);
hot standby and promotion of the recovery worker, failover of an origin (the
`dtx_log` is a normal table and replicates, gids embed the node id; async
replication lag would lose the decision, same as S2-02); `remove_node`;
row-level security on distributed tables (creation is refused; I only read the
checks); extension scripts running DDL (`creating_extension` bypass); statement
cancel at every protocol step (I read `abort_connection`; I did not cancel
during PREPARE); xid wraparound and pid reuse in `xid_possibly_running` (read:
the full-xid window test and the pid comparison look right); memory growth of
very large results; behavior on a black-holed node (reasoned only).
