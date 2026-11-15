<!-- SPDX-License-Identifier: PostgreSQL -->
# Adversarial review of the fix round: contrib/kingres_shard 0.2 (S2-01 to S2-20)

Reviewer: independent (not the author, not the reviewer of the first round).
Commit reviewed: `f78f85220f` ("kingres_shard: fix the findings of the 0.2 review"),
exported with `git archive` into a private directory and built from there. The fix round
is the difference `c283dbf6bc..f78f85220f` under `contrib/kingres_shard` (44 files,
+3217 -134). Nothing in the working tree was read.

## Scope and method

Read: the first review (S2-01 to S2-20), `shard-phase2a.md` with the fix-round notes, the
extension README, and the whole diff of `src/` (`forward_auth.c`, `catalog.c`, `fanout.c`,
`node_conn.c`, `node_admin.c`, `planner.c`, `recovery.c`, `shippable.c`, `utility.c`,
`dist_scan.c`, `remote_scan.c`, `kingres_shard.c`), both SQL scripts and the test diffs.
Core code was consulted where an API mattered (`SyncRepRequested`,
`RecordTransactionCommit`).

Ran, against a private copy of the installation (extension rebuilt from the exported
commit; ports 57800-57999; everything removed afterwards):

- the extension's own 24 TAP files (4405 tests): all pass on the exported commit;
- **revert experiments**: in a second copy of the installation, 17 individual fixes were
  reverted in the source, the extension rebuilt, and the test that is supposed to guard
  the fix was run (results in the status table; every one fails without its fix);
- hand-built three-node clusters (one shard group per node) with a distributed table, a
  colocated one, several reference tables and ordinary roles (`bob` with write rights,
  `eve` with none, `reader` in `pg_read_all_data`), and about 30 scripted experiments
  (quoted with each finding);
- a comparison battery of 117 restriction shapes x 4 session profiles x 3 cluster nodes
  against a plain node holding the same data (timestamptz, timestamp, date, timetz,
  interval, float4/8, numeric, money, bytea, text with non-ASCII, jsonb, arrays, uuid,
  inet, bit, regclass, tsvector, "char"), plus 28 prepared-statement parameter cases
  under generic plans in 3 profiles;
- a crash stress test (below);
- SIGSTOP of a participant at the second phase of two-phase commit.

"Reproduced" means observed; "reasoned" means read from the code and not run. Guesses are
labelled as such.

## Status of the first review's findings

| ID | Verdict | How checked |
|---|---|---|
| S2-01 | closed | Reverted the `GetLatestSnapshot()` push in `decision_logged` (`recovery.c`): `t/016` fails 2 tests (old-snapshot recovery, stress). Own crash stress (below): no participant lost, no committed transaction rolled back. The safety argument was re-derived (see "Examined and found sound"). |
| S2-02 | closed for local durability; the comment about standbys is wrong (S3-15) | Reverted `ForceSyncCommit()`: `t/016` fails test 13. Own stress with `synchronous_commit = off` and `kill -9` of the origin: every acknowledged commit present. |
| S2-03 | closed | Reverted the `CoerceViaIO` refusal: 12+ tests of `t/017` and `t/013` fail. Own battery (117 shapes x 4 profiles x 3 nodes) and 28 parameter cases: no row-set difference. The only differences: which row raises the first conversion error, and floating point sums (S3-14). |
| S2-04 | partly closed | A client that sets only `kingres_shard.forwarded=on`, or sets it with a wrong or placeholder token, gets `FATAL ... cannot prove it` (reproduced; reverting the check fails 12+ tests of `t/019`). But the property "a copy of a reference table is written only through the router" does not hold: a function called by a routed statement diverges the copies without any forged session (S3-01); the secret is readable by `pg_read_all_data` (S3-04) and appears in the log and in an error context in non-default configurations (S3-06); a node without a secret accepts the claim (S3-09). |
| S2-05 | closed for the listed constructs | Reverted the system-column refusal and the LIMIT/OFFSET refusal: `t/018` fails. Own battery of 25 reference writes (below): no divergence among the accepted ones. Reasoned residue: libm differences between heterogeneous nodes (S3-14). |
| S2-06 | partly closed | Reverted the advisory locking: `t/018` fails 6 tests. The single-statement "reads its own table" race is closed. But the lock is a plain advisory lock that any role can take (S3-03), it is table-wide so that a crashed origin blocks every writer (S3-05), and the lock set is the tables named in the statement only (S3-13). |
| S2-07 | closed | Reverted the `lock_all_reference_tables()` call in `add_node`: `t/020` fails 3 tests. Own run: a generic plan prepared before `add_node` and used after it reached all five nodes (no error, no loss). Next to it a new defect: `add_node` copies no privileges (S3-11). |
| S2-08 | closed | Replaced `IsParallelWorker()` by false in the three files: `t/020` fails 3 tests. |
| S2-09 | closed | README text corrected (read). |
| S2-10 | partly closed | `'now'`, `'today'` and the cast forms are refused, but `E'\tnow'` and `E'now\n'` are accepted and evaluated per node: reproduced, copies differ (S3-08). Reverting the fix fails 6 tests of `t/024`. |
| S2-11 | partly closed | (a) closed: removing `fix_worker_settings()` fails `t/022`. (b) the launcher deadline exists (read; not exercised by me); the `statement_timeout`/`lock_timeout` the worker is meant to set on its node connections is never applied: the server rejects the statement on every pass (S3-12). Other per-database settings that govern the worker are not forced (S3-16). |
| S2-12 | partly closed | `bounded_exec` bounds the wait, but the cancel it sends on timeout is the blocking `PQcancel`: with the participant's postmaster stopped the committing backend hung until the node resumed although `node_timeout` was 3 s (reproduced, S3-07). |
| S2-13 | closed | Reverted `change_partition_owners`: `t/021` fails 7 tests. Own run as `bob` after `OWNER TO bob`: ADD COLUMN, ADD CONSTRAINT, generated column, DROP COLUMN, TRUNCATE work; table, partitions and their indexes are owned by `bob` on all three nodes; `bob` can drop a colocated table he owns. |
| S2-14 | closed (residue trivial) | The depth counter became the prefix test `"SELECT fk."` in `kingres_shard_in_authorized_ddl`. Reverting to the depth test fails `t/023` (2 tests). The prefix is client-controlled text, so user code running during an authorized DDL can still get one of its own `SELECT fk. ...` statements planned without routing; such a statement only reads, and any function it calls plans its own statements with their own text. Reasoned. |
| S2-15 | closed by refusal | Removing the check fails `t/021` (5 tests). The refusal depends on where the key lives (a view over a local bucket works, over a remote one is refused) and refuses a superuser reading another role's view: documented behavior, not a defect. |
| S2-16 | closed | Changing `PGC_SIGHUP` back to `PGC_SUSET` fails `t/015` (5 tests). `ALTER ROLE/DATABASE ... SET` of the forwarded settings is refused ("cannot be set after connection start", reproduced). `sslpassword` is rejected. |
| S2-17 | closed | Removing the reset-callback registration fails `t/024` ("30 aborted scans of ~18 MB each leave the backend at +165616 kB"): the test does fail without the fix. |
| S2-18 | closed | Message names UPDATE/DELETE/MERGE ... FROM; README corrected. |
| S2-19 | closed | Removing `contain_volatile_functions` from the write test fails 2 tests of `t/024`; the single writer is committed after the readers (read). |
| S2-20 | mostly closed | `add_node` compares collation/ctype/provider/version and the extension version (test passes), the gid parser is strict (reverting is not tested by me because my patch did not compile; the test passes), `node_id = 0` rows are purged (read), partitionwise settings are documented, the pending-node clock changed (read). Not verified beyond reading: the hostile-gid items. |

Reverts run (all fail the named test without the fix): S2-01, -02, -03, -04, -05 (system
column and LIMIT separately), -06, -07, -08, -10, -11a, -13, -14, -15, -16, -17, -19.
Not reverted: S2-09, S2-11b, S2-12 (SIGSTOP experiment instead), S2-18, S2-20.

## New findings (most severe first)

### S3-01 (critical) A reference table is written on one node only when a function or trigger inside a routed statement writes it

`planner.c` 1184 (`if (a.has_ref && !kingres_shard_session_forwarded())`), 730-786
(`plan_forwarded`: a statement that names no distributed table is planned locally),
README "forwarded session ... never forwards again".

The router sends a routed write to another node as a forwarded session. In a forwarded
session a write to a reference table is never fanned out, by design (the origin does it).
But a function, trigger or rule that the routed statement itself calls runs on the
remote node, and its statements are planned there, in the forwarded session, so a write
to a reference table made by user code goes to that node's copy only. The authentication
of S2-04 does not help: the session is genuine. The same function called for a key
owned by the origin node is fanned out. Reproduced as an ordinary role:

```
-- on every node (functions are not propagated):
create function public.wr(k int) returns text language plpgsql volatile as
  $$ begin insert into public.logr values (k, 'wr'); return 'w'; end $$;
-- logr is a reference table; key 6 belongs to node 2, key 3 to node 1
bob@node1:  insert into d values (6, wr(6));     -- routed to node 2: INSERT 0 1
bob@node1:  insert into d values (3, wr(3));     -- local bucket: fanned out
select * from logr order by k;
  node1: 3|wr          node2: 3|wr, 6|wr          node3: 3|wr
```

No error; the copies differ from then on and later writes of key 6 fail with a duplicate
key on node 2 only. The same happens for a trigger on a bucket partition (triggers on
distributed tables are refused by the DDL hook, but the README tells a superuser to
create objects by hand with `enable_routing = off`), and for any `SELECT wr(6) ...`
routed to another node. This is the consequence S2-04 was rated critical for, reached
without forging anything.

Fix: mark the fan-out statements of a reference write (as `KS_WRITE_MARKER` marks the
other fragments) and make a forwarded session refuse a reference-table write that does
not carry the marker ("a reference table cannot be written from a statement that is
run for another node"); the same refusal for a nested write in a trigger. Add a test with
a function that writes a reference table, called by a routed statement.

### S3-02 (critical) A cached plan of a write keeps writing one copy after the table becomes a reference table

`create_reference_table` (`dist_table.c`) does not invalidate plans that mention the
table; the planner hook is not consulted again for a cached plan.

A prepared statement (or a PL/pgSQL function, or a driver's server-side prepared
statement) planned while `tt` was an ordinary table is a plain local plan. After an
administrator makes `tt` a reference table the plan stays valid and every later
execution writes the local copy only. Reproduced:

```
bob:   set plan_cache_mode = force_generic_plan;
       prepare u as update public.tt set v = v + 1 where id = 1;  execute u;
admin: select kingres.create_reference_table('public.tt');
bob:   execute u; execute u;
node1: v = 3      node2: v = 1      node3: v = 1      -- no error
```

Distributed tables are not affected (the generic plan is refused or replanned: tested).
Reads are not affected. The statement cannot be told from an ordinary one by the user,
and long-lived pooled sessions will hold such plans.

Fix: `create_reference_table` (and the code that removes a reference table or changes the
node set) must call `CacheInvalidateRelcacheByRelid()` for the table on every node (a
no-op ALTER TABLE does the same), and the reference-write path must be checked at
execution time (a plan for a table that is a reference table now and was not at planning
time must be refused).

### S3-03 (high) Any role can block every write to a reference table, cluster-wide

`fanout.c` 466 (`kingres_shard_refwrite_lock_sql`), `kingres_shard.h` 55
(`KS_REFWRITE_LOCK_CLASS 0x4B535246`).

The writer lock is `pg_advisory_xact_lock(0x4B535246, hashtext('<schema>.<table>'))`,
in the key space every role shares; `pg_advisory_lock` is executable by PUBLIC. A role
with no privilege on the table needs only the (public) constant and the table name.
Reproduced:

```
eve@node2:  select pg_advisory_lock(1263751750, hashtext('public.r2'));   -- keeps the session open
bob@node1:  set statement_timeout = '3s'; insert into r2 (id, v) values (900, 1);
ERROR:  canceling statement due to statement timeout      (after 3.0 s)
```

Reads continue. `add_node` takes the same lock for every reference table while it holds
the catalog lock on `map_version` (reasoned: it then waits, with the catalog lock held, and
every other catalog-changing function waits behind it). Two different table names can
collide in `hashtext` (32 bit) and then serialize needlessly (harmless).

Fix: take a lock that no SQL function exposes: from a C function on each node, a
`LOCKTAG_OBJECT` lock with the extension's own class id, or `SELECT ... FOR UPDATE` of the
table's row in `kingres.dist_table` through the heap API. Document `lock_timeout`.

### S3-04 (high) The cluster secret is readable by `pg_read_all_data` (and by backup and replication roles)

`kingres_shard--0.2.sql` (`kingres.cluster_secret`, "no role has any privilege"),
`catalog.c` 199.

`pg_read_all_data` bypasses table privileges. Reproduced as a role that is only a member of
`pg_read_all_data`:

```
reader@node1:  select encode(secret,'hex') from kingres.cluster_secret;
 fec9af2b2c6e4693a5e9b9c600ca7ae535c7fba310ed4b57994c780c8e77b7d7
```

With it the role makes the token for itself (HMAC over its own name and a node id; I used
`internal_forward_token` as superuser to get the same bytes) and opens a forwarded session:

```
PGOPTIONS='-c kingres_shard.forwarded=on -c kingres_shard.forward_token=<token>' psql -U reader -p <node2>
  insert into r values (991, 1);      -- INSERT 0 1; node 2 has 7 rows, nodes 1 and 3 have 5
```

So for every role that can read all data (reporting and backup roles are commonly given
`pg_read_all_data`) and holds a write privilege on a reference table, S2-04 is open. The
same holds for a `REPLICATION` role (logical decoding of the `INSERT` / `internal_apply_secret`
traffic, `pg_basebackup`), and a base backup or `CREATE DATABASE ... TEMPLATE` copy carries
the secret and the node ids (S3-17). `pg_dump` does not dump the table (checked).

Fix (tested): `ALTER TABLE kingres.cluster_secret ENABLE ROW LEVEL SECURITY` with no policy;
as `reader` the table then returns 0 rows (reproduced) while the C code, which reads the heap
directly, is unaffected. Better: keep the secret out of SQL-visible storage.

### S3-05 (medium) A crashed origin leaves prepared transactions that hold the table-wide lock

`fanout.c` 482 (`lock_reference_tables`, held to the end of the distributed transaction).

The advisory lock is taken in the participants' transactions, and a prepared transaction
keeps its locks. If the origin dies after PREPARE (fault point `after_prepare:kill`, or a
real crash), every writer of that reference table, even of other rows, waits on every
node until recovery runs on the origin; if the origin stays down, until someone runs
`ROLLBACK PREPARED` by hand. Before the fix only the rows written were locked.
Reproduced:

```
node1: set kingres_shard.fault_injection='after_prepare:kill'; insert into r (id,v) values (8000,1);
node2/3: pg_prepared_xacts = 1 each
node2:   set statement_timeout='5s'; insert into r (id,v) values (8001,1);
ERROR:  canceling statement due to statement timeout
CONTEXT: SQL statement "SELECT pg_catalog.pg_advisory_xact_lock(1263751750, pg_catalog.hashtext('public.r'))"
-- another reference table (r2): immediate
-- after restarting node 1 and calling recover_prepared_transactions(): released, copies equal
```

Rated medium because it needs a failure and recovers by itself; the blast radius is new.
Fix: document it; consider `lock_timeout` on the lock statement with a clear message, and
let the recovery worker run on a restarted origin promptly (it does).

### S3-06 (medium) The secret reaches the client error context and the server logs when `log_parameter_max_length_on_error` is non-zero

`node_admin.c` 217 (`push_secret`): only `log_parameter_max_length` is set to 0.

The README says the token and secret "never appear in ... error messages, the log". The
secret is sent as a statement parameter; if `internal_apply_secret` fails (lock timeout,
statement timeout from a role setting, deadlock, a trigger, out of memory) and
`log_parameter_max_length_on_error` is not 0 (a documented debugging aid that sites do
enable), the error context of the failing statement contains it. Reproduced
(`log_parameter_max_length_on_error = -1`, a trigger raising an error on the table of node 2):

```
ERROR:  boom
CONTEXT:  ... SQL function "internal_apply_secret" statement 2
unnamed portal with parameters: $1 = 'fec9af2b2c6e4693a5e9b9c600ca7ae535c7fba310ed4b57994c780c8e77b7d7'
remote node 127.0.0.1:57951
```

The same line was in the log of node 2 and of the origin (the origin relays the remote
context). The log with `log_statement = 'all'` and `log_min_duration_statement = 0` and
`log_parameter_max_length = -1` did not contain the secret or a token in a normal run
(checked on all three nodes at `debug2`).

Fix: `SET LOCAL log_parameter_max_length_on_error = 0` next to the other one, or
do not send the secret as a parameter of a statement that can fail inside the
extension's own error context (send it with `set_config`/a prepared `COPY`-less path).

### S3-07 (medium) The bounded wait is not bounded: the cancel on timeout can block forever

`node_conn.c` 803 (`PQcancel_best_effort`), 825 (`kingres_shard_bounded_exec`).

On a timeout `bounded_exec` calls the legacy `PQcancel()`, which connects to the node and
then waits for the postmaster to close the connection, with no timeout of its own other
than the socket's `tcp_user_timeout`. If the participant's postmaster does not answer (a
stopped or stuck process, a frozen machine whose kernel still accepts the connection) the
committing backend, which has already committed locally, blocks, and it does not react to
cancel. Reproduced with `kingres_shard.node_timeout = 3`:

```
session: begin; insert (key of node 1); insert (key of node 2); commit;   -- paused at after_commit
SIGSTOP the postmaster and all processes of node 2; release the pause
... nothing for 25 s (until SIGCONT) ...
WARNING:  could not commit prepared transaction "kingres_1_1954258_866_2" on node ...57951; recovery will commit it
DETAIL:  no result
psql returned after 28 s
```

(The same call is used by `abort_connection` and the savepoint callback.) The recovery
worker is not protected by this either: it is stuck in the same call and the launcher's
`WaitForBackgroundWorkerShutdown` after `TerminateBackgroundWorker` waits for it (reasoned
from the code, not run).

Fix: use `PQcancelCreate()/PQcancelStart()` (nonblocking, PG 17+) polled with the same
deadline, or skip the cancel and close the socket (the participant resolves it from the
prepared state anyway: a cancelled `COMMIT PREPARED` does nothing useful).

### S3-08 (medium) S2-10 is only partly fixed: whitespace in front of or behind the special string

`utility.c` 313-351 (`is_special_datetime_string` skips only the space character).

`E'\tnow'` and `E'now\n'` are accepted by `ALTER TABLE ... ADD COLUMN ... DEFAULT` on a
reference table and PostgreSQL accepts them as "now". Reproduced:

```
alter table r2 add column ts2 timestamptz default E'\tnow';     -- accepted
alter table r2 add column ts4 timestamptz default E'now\n';     -- accepted
select count(distinct ts2), min(ts2) from r2:
  node1: 22:44:30.958922   node2: 22:44:30.961985   node3: 22:44:30.965770
md5 of the whole table r2:  node1 78bb60c3...   node2 ee287687...   node3 8c042d03...
```

Fix: do not recognize the string by text. After parse analysis a default of a date/time
type that comes from a string constant is evaluated on the map group primary and shipped
as a literal; or reject every string-literal default for date, time, timestamp, timestamptz
and timetz columns.

### S3-09 (medium) A node without a secret accepts the claim: reachable states

`forward_auth.c` 176-177 (`kingres_shard_cluster_secret` false -> `return true`).

Documented by the author, but "fail open" is reachable by an ordinary user in these
ordinary states: every node of a cluster upgraded from 0.1 except the map group primary,
until someone runs `sync_catalog()` (the upgrade script creates the secret only where
`local_node` says "map group primary"); a node whose `kingres.cluster_secret` is empty for
any reason (restored from a dump, `internal_reset_node`); a node being added before the
secret is pushed (that window is inside the 2PC and the node is not yet visible: not
exploitable). Reproduced on a working cluster by emptying the table on node 2 and using
the forged flag as `bob`:

```
PGOPTIONS='-c kingres_shard.forwarded=on' psql -U bob -p <node2> -c "insert into r values (990,1)"
n1: 5 rows   n2: 6 rows   n3: 5 rows
```

(The forged session could not read distributed data: "cannot prove that the statement
touches only buckets of this node".) Fail closed once the node belongs to a cluster:
no secret and a `kingres.local_node` row means "refuse the claim"; and have `sync_catalog()`
/ the upgrade script report nodes without a secret (a warning at each accepted claim at least).

### S3-10 (medium) `INSERT ... SELECT` into a reference table that has a column default always fails

`planner.c` 432-450 (`deparse_for_remote`), reached from `plan_reference_write`.

The query handed to the hook is rewritten: the defaults of the columns that were not named
are in its target list. The deparser prints the column list with them and the sub-select
without. Reproduced (a table with one column `d int default 7`; source empty or not):

```
insert into r4 (id, v) select id + 10, v from r5;
ERROR:  INSERT has more target columns than expressions
LINE 1:  INSERT INTO public.r4 (id, v, d)  SELECT (r5.id + 10), ...
```

The first-round statement-mode battery used tables without defaults. Not caused by the fix
round (the code is untouched), found now. Fix: when the statement is an INSERT ... SELECT,
print the original column list (the columns whose target entry is a Var of the sub-select)
and leave the defaults to the remote parse, or add the default expressions to the sub-select
target list.

### S3-11 (medium) `add_node` does not copy privileges or ownership; non-owners cannot write on the new node

`node_admin.c` `create_tables_on_node`.

After `add_node` the new node has the tables and rows but not the ACLs. Reproduced:
`relacl` of `r2` is `{kingres=arwdDxtm/kingres,bob=arwdDxtm/kingres}` on node 1 and empty on
the new node; the owner of the distributed table is the bootstrap superuser. A generic plan
by `bob` after the add: `ERROR: permission denied for table r2 / CONTEXT: remote node ...:57953`.
The README says "Privileges are copied only by propagated GRANT", so this is documented,
but it breaks every write of an ordinary role to a reference table, and every routed statement
that hits the new node, atomically and without any hint. Fix: copy `relacl`/owner in
`add_node`, or refuse to finish while ACLs differ and say which `GRANT`s to repeat.

### S3-12 (medium) The worker's node timeouts are never applied, and every pass logs an error

`recovery.c` ~387 (`SET statement_timeout = %d; SET lock_timeout = %d` sent with
`kingres_shard_exec_params`, which uses `PQsendQueryParams`, one statement only).

The server answers `ERROR: cannot insert multiple commands into a prepared statement`;
the caller passes `throw_on_error = false` and drops the result. The intended bound on the
worker's remote commands (S2-11b) is therefore never in force, and each recovery pass of
each worker logs the error on every node (316, 341 and 345 lines in two minutes on a
three-node cluster at `recovery_interval = 1`). The tests pass because the launcher
deadline and the swallowed error hide it. Fix: send the two settings as one
`SELECT set_config(...), set_config(...)` or as two statements, check the result, and add a
test that reads `SHOW statement_timeout` from the worker's connection.

### S3-13 (low, reasoned) The lock set is the tables named in the statement; cascades and nested writes are not serialized

`planner.c` `reference_lock_names` collects the relations of the query tree only.

A write to a reference table `p` that cascades (`ON DELETE CASCADE/SET NULL`, `ON UPDATE
CASCADE`) into a reference table `c`, and a concurrent writer of `c` alone, hold different
locks and can interleave differently on different nodes. Most such races end in an error
(row counts differ) and a rollback; I did not find a silent one (guess). Fix: add every
table reachable by a foreign key from the target to the lock set.

### S3-14 (low) Floating point: results differ by origin, and libm results may differ between nodes

`shippable.c` accepts `sqrt`, `exp`, `ln`, `power`, trigonometric functions as "built-in
immutable". On heterogeneous nodes (another CPU architecture, another libm) their result can
differ in the last bit, and `update r set f = exp(f)` then makes the copies differ silently
(guess; not run: one machine). Reproduced a related user-visible effect: `select sum(f),
avg(f) from t1` over the distributed scan returns `610.0000018300001` from node 1 and
`610.0000018300002` from node 2 (the order in which groups are added depends on the origin);
the author's README treats this as inherent. Fix: document it; add the libm-dependent
functions to the refused list for reference writes.

### S3-15 (low) `ForceSyncCommit()` does not make the commit wait for synchronous standbys when `synchronous_commit` is `off` or `local`

`node_conn.c` 1241 (comment: "This also makes the commit wait for synchronous standbys").

`SyncRepRequested()` is `max_wal_senders > 0 && synchronous_commit > LOCAL_FLUSH`
(`syncrep.h`), and `RecordTransactionCommit` calls `SyncRepWaitForLSN` under that test, so
with `off`/`local` the commit record is flushed locally only while the participants'
`COMMIT PREPARED` goes out. A failover to a standby of the origin then loses the decision.
Reasoned from the core source. Fix: correct the comment and document that a failover of an
origin that must not lose decisions needs `synchronous_commit` of `remote_write` or higher
(a guess at the cheapest remedy: raise the setting for a transaction that prepared remotes,
at the start of `precommit`, before the commit record is written).

### S3-16 (low, reasoned) `kingres_shard.node_timeout` and `node_connect_timeout` are not forced in the worker

`recovery.c` 650 (`fix_worker_settings`). Both are `PGC_USERSET`; the owner of a database
can `ALTER DATABASE x SET kingres_shard.node_timeout = 1`, which the worker takes
(`statement_timeout`, keepalives, launcher deadline = 5 x timeout) and with which every
pass of a slow cluster may be cut short. Not run. Fix: add them to the forced list.

### S3-17 (low, reasoned) Tokens are static and bound only to role and node id

A token is valid forever for (role, node id) and is not bound to the database name, to the
cluster, or to a time. A base backup or a database copy (`CREATE DATABASE ... TEMPLATE`) has
the same secret and the same node ids, so tokens made from the copy are accepted by the
original nodes. Without TLS (the default) a token seen on the wire can be replayed by anyone
who can also log in as that role. The comparison is constant time and the check hook does hide the
token (`SHOW`/`pg_settings`/`ALTER ROLE` verified). Fix: bind the token to the system
identifier of the cluster and database name, and have `init_cluster` fail unless TLS or
a pg_hba entry is configured (or document it as a requirement).

### S3-18 (low) Writes through an updatable view of a reference table are refused with a misleading reason

`insert into rv values (7002, 1)` with `rv` a plain view of reference table `r` fails with
"it reads a table that is not a reference table": the view's own range-table entry is
what `relation_ok` rejects. Reproduced. Not a risk; a correct statement fails with a wrong
message. Fix: look through `RTE_RELATION` entries of `relkind` view that carry no
`rellockmode` use, or say "a view".

## Examined and found sound

- **Recovery (S2-01/S2-02), re-derived.** The decision is "commit iff the row is visible
  after the origin transaction is known to be over". (a) `xid_possibly_running` first, then
  (b) the row lookup with `GetLatestSnapshot()` pushed: the commit record is written, the
  clog set, and only then the xid leaves the proc array, so a snapshot taken after "not in
  progress" sees the row of a committed transaction (and a rollback needs both "not running"
  and "no row"); two recoveries decide from the same facts and a double resolution fails
  harmlessly with "does not exist"; the purge removes a row only if it is older than the
  listing, not in `still_prepared` and its prepared transaction is gone from the node, and
  `prepared_at` is the time the row was written, after every PREPARE, so a late prepared
  transaction cannot lose its row. Xid reuse after a crash: an id at or above the next xid is
  "not running" (the transaction never reached the WAL, so it cannot have committed), an id
  taken by another backend is told apart by the pid; pid reuse only delays resolution. A
  `COMMIT PREPARED` that times out and is delivered later is the same decision as recovery's
  (commit is sent only after the local commit; a rollback only when the local transaction
  aborted before commit).
- **Crash stress (clean third run, `synchronous_commit = off`).** 16 clients each committing a
  transaction of three inserts in three groups through a random origin, two loops calling
  `recover_prepared_transactions()` and the background workers every second, and a crasher
  that, every 4-9 s, alternately ran `pg_ctl stop -m immediate` and `kill -9` of the postmaster
  and its children on a random node (16 crashes in 120 s): 11562 acknowledged commits, 11564
  committed transactions (2 not acknowledged: the crash hit the client), 0 tags with other
  than 3 rows, 0 acknowledged commits missing, nothing left prepared on any node after the
  recoveries. Earlier runs of 90 s (with and without `synchronous_commit = off`, `stop -m
  immediate` only) gave the same result.
- **Forwarded-session check placement.** The planner hook and the ProcessUtility hook
  both call `kingres_shard_session_forwarded()` before anything else; nothing else reads the
  raw flag (`grep`). Fast-path calls, Parse/Describe, LISTEN, SET/RESET/DISCARD, login event
  triggers and startup `options` do nothing that depends on the flag until a statement is
  planned or a utility statement is executed, and both fail the session first. `ALTER ROLE ...
  SET kingres_shard.forwarded` / `forward_token` is refused ("cannot be set after connection
  start"). `SHOW`, `pg_settings` and `current_setting` of the token are denied to an ordinary
  role and show `<hidden>` otherwise. The HMAC input is unambiguous (NUL after the role);
  the comparison does not stop at the first different byte; a placeholder or missing token
  fails the check. With `log_statement = 'all'`, `log_min_duration_statement = 0`,
  `log_connections = 'all'` and `log_min_messages = 'debug2'` on all nodes the secret and
  tokens were absent from the logs (apart from S3-06). `pg_dump` by a `pg_read_all_data` role
  does not dump the table.
- **Reference writes.** Accepted and consistent on all nodes: ON CONFLICT DO UPDATE with
  `excluded`, RETURNING, INSERT ... SELECT with ORDER BY (no LIMIT), a scalar sub-select of the
  same table, `avg`/`sum` of integers, a timestamptz literal with offset, `upper`/`lower` of
  non-ASCII constants, `md5`, `to_hex`, a DO block, a data-modifying CTE (refused cleanly:
  "parameter computed during execution"). Refused with the intended reason: `ctid`/`tableoid`,
  LIMIT/OFFSET, DISTINCT ON, window functions, `array_agg`/`string_agg`/float aggregates,
  `random()`, `now()`, `nextval()`, `pg_backend_pid()`, `txid_current()`, `format`,
  `to_char`, `array_to_string`, `pg_typeof(...)::text`, any `x::text`, FOR SHARE, a catalog
  table in the FROM list, MERGE, COPY (refused with the standard hint), TRUNCATE from a node
  other than the map group primary. A generic plan prepared before `add_node` and run after
  it reaches every node. Deadlock between advisory locks: the lock order (node id, then name)
  is the same everywhere, so two writers cycle only on one node, where the deadlock detector
  sees it (reasoned); across nodes it is the documented absence of a distributed detector.
- **Shippability.** 117 restriction shapes (casts through text, `to_char`, `date_trunc` with and
  without zone, `extract`, `at time zone`, `to_tsvector`/`to_tsquery` with and without a
  configuration, `LIKE`/`ILIKE`/regex on non-ASCII text, collations, regclass columns and
  constants, arrays, jsonb, money, interval comparisons, row comparisons, `= ANY`, `CASE`,
  `coalesce`, `string_to_array`, `hashtext`) x 4 session profiles (DateStyle SQL/German/
  Postgres, IntervalStyle all four, `extra_float_digits` -3..1, bytea escape, TimeZone
  New_York/Tehran/UTC, `standard_conforming_strings` off, search_path, lc_monetary/lc_numeric C)
  x 3 origin nodes: identical row sets to the plain node. 28 parameter cases (date, timestamptz,
  float, interval, bytea, money, timetz, arrays, jsonb, regclass) under `force_generic_plan` in 3
  profiles: identical. Only `C`/`C.utf8`/`POSIX` locales exist on the machine, so `lc_monetary`
  with a real locale was not exercised (not examined).
- **Owner and views (S2-13/S2-15).** After `OWNER TO` the new owner runs the supported DDL;
  the view refusal triggers only when the remote work needs another group.
- **Parallel (S2-08).** A `PARALLEL SAFE` function that reads a distributed table, a routed
  statement and a fan-out all refuse in a worker; reverting fails the test.
- **Leak (S2-17).** The test for it fails without the fix.
- **add_node vs writers (S2-07).** Passes with and without prepared statements; the new node
  receives writes made during the add.
- **S2-04 versus ordinary operation.** The origin's own statements are forwarded with a valid token
  for `GetUserId()`: the role of a SECURITY DEFINER function is the definer, as before.

## Not examined

- Real (non-C) locales and ICU providers, `lc_monetary` with a currency locale, database
  encodings that differ between nodes (the new warning only).
- Heterogeneous machines for the libm question (S3-14 is a guess).
- The launcher deadline (`5 x node_timeout`) under a node that stalls after connect; the
  stall in `PQcancel` (S3-07) was shown, the combination with the worker only read.
- Logical decoding of the secret by a `REPLICATION` role (needs `wal_level = logical`; read,
  not run), hot-standby promotion of an origin, `remove_node` with writers in flight, xid
  wraparound in `xid_possibly_running` (read: the 2^31 window and the pid test look right).
- A real network partition (black hole); only a stopped process was used.
- DDL with a role that is not a superuser creating the extension objects; row-level security
  on distributed tables (creation refused); `pg_stat_statements` and core dumps (not installed
  / out of scope).
- A distributed deadlock between ordinary row locks and the new advisory locks across nodes
  (reasoned to be the documented limitation; not run).
