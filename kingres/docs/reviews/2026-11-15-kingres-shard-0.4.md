<!-- SPDX-License-Identifier: AGPL-3.0-only -->
# Adversarial review: contrib/kingres_shard 0.4 (fix round for the reviews of 0.3 and of the 0.2 fixes)

Reviewer: independent (not the author of the code, not the reviewer of the earlier
rounds). Reviewed state: commit 37293a7e77 ("kingres_shard 0.4: fix round for the
reviews of 0.2 and 0.3"), exported with `git archive` into a private area
(`~/kw-review5`); the working tree was not used for any result. The ten findings
S4-01 to S4-10 of `2026-11-15-kingres-shard-0.3.md` and the eighteen findings
S3-01 to S3-18 of `2026-11-14-kingres-shard-0.2-fixes.md` are answered below; new
findings are numbered S5-nn.

## Scope and method

Read: both earlier reviews in full; the commit message; the 0.3-to-0.4 diff of
`dist_scan.c`, `catalog.c`, `planner.c`, `fanout.c`, `forward_auth.c`, `recovery.c`,
`bucket_move.c`, `utility.c`, the relevant parts of `node_conn.c`, `kingres_shard.c`
and `kingres_shard--0.4.sql` (`resume_bucket_moves`, `cancel_bucket_move`,
`cleanup_moved_buckets`, `internal_check_node_versions`, the privilege lines of the
move tables and views); the README chapters on the forwarded flag, security notes,
reference tables and moving buckets; the test helper and the headers of the new
tests. I did not read `shippable.c`, `node_admin.c` (the `add_node` ACL copy) or the
0.3-to-0.4 upgrade script line by line.

Built and ran (PostgreSQL 18.7, a private copy of an existing installation, the
extension built from the exported commit with `kingres/dev/ext.sh`, TAP servers on
ports chosen by the test framework; the machine was shared, load average 5 to 9;
everything under `nice`, `-j4`):

- **Whole suite, once**: 12 pg_regress tests and 44 TAP files, 5764 subtests, no
  failure (660 s). This includes `upgrade` and `catalog`.
- **`t/031_move_scan_interleavings.pl` with `KS_INTERLEAVINGS=full`** (the authors
  did not run it): 854 subtests, all passed, 319 s.
- **`t/027` five times in a row, `t/028` three times**, one at a time (an
  attempt to run six TAP files at once failed in `initdb` of one of them, three times
  for the same reason, and showed nothing about the code; see "Not covered"): `t/027`
  passed 5 of 5 (plus the baseline run), `t/028` passed 3 of 3 (309 subtests each,
  plus the baseline run).
- **Revert experiments** for the three fixes the commit says were never run against
  old code (second copy of the installation, source edited, rebuilt, `t/036` run):
  S4-05 (both the SQL version check and the C error text disabled): subtests 25, 26
  and 29 to 32 fail; S4-06 (`require_node_session` made a no-op) and S4-07
  (`GRANT SELECT ON kingres.bucket_move TO PUBLIC` added): subtests 34 to 41 and 44, 45 fail
  (the same run shows 25 and 26 failing again because my build reused an object file
  built with the S4-05 edit; I count those two from the first run only). So the tests
  can fail; they do guard the fixes. (The first run, with all four edits at once,
  aborted after subtest 32 and did not reach the S4-06 and S4-07 part, which is why
  they were run again without the S4-05 edits.)
- **About 25 scripted experiments of my own** on three-node clusters (one shard group
  per node, helpers from `KingresShardTest.pm`, fault points from `fault.c`), described
  with each finding. Among them: the S4-01, S4-03 and S4-02 reproductions of the
  earlier review; concurrent moves; give-up of a move; reference-table writes
  through functions, triggers, cached plans, `EXPLAIN ANALYZE`, `COPY`, `MERGE`,
  data-modifying CTEs, `ON CONFLICT`, identity and serial columns, foreign-key
  cascades in both directions and `TRUNCATE ... CASCADE`; 24 spellings of `ADD COLUMN ... DEFAULT`
  for date, time and object-name types; the secret read as seven roles, in logs at
  `debug5` with every parameter-logging setting at `-1`, in `pg_stat_statements`,
  in `pg_dump` output; `rotate_secret()` with a move paused at three points and with
  a lock timeout on one node; `SIGSTOP` of a participant during a commit;
  a bare crash of the origin after `PREPARE`; `add_node` with privileges;
  a cluster whose nodes run the 0.3 binary and the 0.4 binary at the same time (a
  second installation built from the 0.3 commit); a cluster with the 0.4 binary
  and 0.3 SQL objects on some nodes; the forged forwarded flag with and without a
  secret on the node.

"Reproduced" below means I observed it; "by reading only" means I did not run it.
All clusters and data directories were removed or left under `~/kw-review5`; no
other area of the machine was written.

## Status of the earlier findings

Verdicts: fixed / partly / not fixed. "Original" means the reproduction of the
earlier review (or the nearest equivalent that the current code allows) was run
against 0.4.

| ID | Verdict | How checked |
|---|---|---|
| S4-01 | fixed | Original: first move of bucket 0 held in the switch (`move_switch_after_detach:wait`), scan from another node held after its DECLARE (`scan_declared_g<S>:wait`), bucket moved back, scan released: it fails with KS001 ("the bucket map changed while the statement was reading it"), the table is 30 rows on every node (0.3 answered 16). Variants: two moves of different buckets between the placement check and the cursor (in `t/031`, full matrix passes). Not tried: a scan whose origin is the source (covered by the local check in `check_local_partitions`, by reading and by `t/031` source origins). |
| S4-02 | fixed | Original: move session terminated at `move_after_subscription` with `recovery_interval = 2`: the worker marked it `failed` ("interrupted; undone by kingres.resume_bucket_moves()"), slot, subscription, publication and the `moving` state were gone on all nodes within the polling window, and the bucket could be moved again. Finished moves: orphan tables gone after `move_cleanup_delay = 1`. One caveat found while building the test: a fault point that waits on a file keeps a control session of the move alive, and the worker leaves the move alone while it does (correct). |
| S4-03 | fixed | Original with the lag forced (`move_switch_after_attach:wait`, subscription disabled on the target): as superuser with `enable_routing = off` the direct `INSERT` into the partition now waits for the switch's lock and fails with "relation ... does not exist" after the rename; no acknowledged write is lost, the table has no row from it anywhere. As an ordinary role (routing on) the statement is refused at planning ("cannot write a bucket partition of a distributed table directly"); `COPY partition FROM` is refused too. |
| S4-04 | fixed | A reader that has held the tables for more than `lock_ms`: the move gave up after `move_switch_attempts = 2` in 0.5 s with a message that names the blocker pid; no slot, subscription, publication or standalone table was left on any of three nodes (compared the full relation list of each node before and after); the bucket stayed `active`. The stall of other sessions during an attempt: not measured by me; `t/035` passes. Two concurrent moves between the same two groups, and two in opposite directions, both completed (0.3 s). |
| S4-05 | fixed | 0.4 library with 0.3 SQL objects on nodes 2 and 3: scans, `COPY (SELECT ...)`, say "node ... runs an older version of the kingres_shard SQL objects ... Run ALTER EXTENSION kingres_shard UPDATE"; `move_bucket` says "node 2 (...) has kingres_shard 0.3, a bucket move needs 0.4 on every node". Fan-out `UPDATE` and point reads still work (they only need helpers that 0.3 has), which is harmless. Revert experiment above. See S5-02 for the case that the libraries differ. |
| S4-06 | partly | `internal_map_version()` and `internal_placement_hash(...)` refuse an unprivileged role; a role that may read the table may ask for its hash. `internal_partition_identity(regclass)`, new in 0.4, has no such check (S5-03). |
| S4-07 | fixed | `kingres.bucket_move` is revoked from PUBLIC, `kingres.bucket_move_status` (no `detail`, no slot name) is readable; an ordinary role reads the view and is refused the table. Revert experiment above. |
| S4-08 | fixed | `t/027` five times in a row under a loaded machine, passed every time (plus the baseline run). |
| S4-09 | not fixed | Documented as such in the commit. Reproduced that it is still so: `backend_xmin` of the session that runs the move is non-null during catch-up. |
| S4-10 | partly | The orphans now go away on their own (worker), which bounds the time the old copy stays readable to `move_cleanup_delay` plus one worker interval. `check_cluster()` still compares structure for distributed tables; it now compares row count and digest for reference tables, and README says so. |
| S3-01 | fixed | Original (function that writes a reference table, called by a statement routed to another node: `INSERT INTO d VALUES (k, wr(k))`, `SELECT wr(k) FROM d WHERE id = k`, `UPDATE d SET v = wr(k)`, `DELETE ... AND wr(k) > 0`): all four refused on the remote node with "cannot be written from a statement that is run for another node"; nothing written on any node; the same function called for a local key writes all three copies. A hand-made trigger on the reference table itself is a different path and is not covered: S5-01. |
| S3-02 | fixed | Original: generic plan `UPDATE` prepared before `create_reference_table()`, executed three times after it: all three nodes show the same value. Also a PL/pgSQL function with a plan cached before the table became a reference table: same. |
| S3-03 | fixed | The advisory-lock keys of 0.2 (`pg_advisory_lock(1263751750, hashtext(...))`) taken by an unprivileged role no longer delay writers; `internal_lock_reference_table` is refused to ordinary roles. A role that holds INSERT on the table and keeps a transaction open still blocks the other writers for `node_timeout` (it holds the table's rows and the lock): inherent. |
| S3-04 | fixed | Role in `pg_read_all_data`: `SELECT ... FROM kingres.cluster_secret` returns 0 rows; `pg_monitor`, `pg_read_server_files`, `pg_read_all_stats`, a `REPLICATION` role, a `CREATEDB` role, a plain role: permission denied. `pg_dump` as the `pg_read_all_data` role and as superuser (data only) does not contain the secret. A superuser, a replication slot reader and anyone who reads the files still can: documented. |
| S3-05 | fixed (as intended) | Origin killed after `PREPARE` (`after_prepare:kill`): a writer of the same reference table on another node fails after exactly `node_timeout` (3.0 s) with "canceling statement due to lock timeout" and the context "while waiting for the writer lock of reference table ... (possibly a prepared transaction that waits for its origin)". The prepared transaction still holds the lock until recovery resolves it: by design. |
| S3-06 | fixed | `log_parameter_max_length_on_error = -1`, `log_parameter_max_length = -1`, `log_statement = 'all'`, `log_min_duration_statement = 0`, `log_min_messages = debug5` on all three nodes during `init_cluster`, `add_node`, `rotate_secret` (also with a failing `internal_apply_secret`: lock timeout forced on node 2): neither the old nor the new secret appears in any server log, the error text or the context; `pg_stat_statements` shows `internal_apply_secret($1)` only; `pg_stat_activity` showed nothing. |
| S3-07 | fixed | `SIGSTOP` of a participant's postmaster and children after the local commit, `node_timeout = 3`: the committing session returned after 4.0 s (0.2: 28 s) with the "recovery will commit it" warning; the participant resolved the prepared transaction after `SIGCONT`. See S5-04 for a misleading second warning. |
| S3-08 | fixed | 24 spellings of `ADD COLUMN ... DEFAULT`: `E'\tnow'`, `E'now\n'`, `E' \r\n now '`, `E'\x0bnow'`, `'NOW'`, `'  tOdAy  '`, `'now'::timestamptz`, `'Now'::text::timestamptz`, `'tomorrow'`, `'allballs'`, `'2020-01-01 yesterday'`, `now()`, `current_timestamp`, `(select now())`, `coalesce(null,'now')`, a `CASE`: all refused. `'infinity'` and `'epoch'` (the same on every node) and a `text` column with the default `'now'` are accepted, and the copies are equal. |
| S3-09 | fixed | Secret table emptied on node 2: a forged `forwarded = on` as an ordinary role is refused ("cannot prove it"), also with a wrong token; a superuser's claim is accepted (documented); statements of an ordinary role that need node 2 fail with the hint to run `sync_catalog()`; after `sync_catalog()` they work. `check_cluster()` returned no row for the node without a secret (the commit says this was not done). |
| S3-10 | fixed | `INSERT ... SELECT` into a table with a column default, column list in the same and in another order, multi-row `VALUES`: accepted, copies equal (defaults sent as values). |
| S3-11 | fixed | `add_node` of a fourth node: `relacl` of the reference table equal to node 1's, an ordinary role with INSERT can write through the new node and through node 1, all four copies equal. |
| S3-12 | fixed | `recovery_interval = 1` for the whole run of a reference-table workload: no "cannot insert multiple commands" line in any node log. |
| S3-13 | fixed | Foreign keys between reference tables (`ON UPDATE CASCADE`, `ON DELETE CASCADE`), from a distributed child to a reference parent (`ON DELETE CASCADE`), `TRUNCATE ... CASCADE` from the map group primary and refusal from another node: copies equal, distributed children removed on the right nodes, no error. The concurrency of two writers whose lock sets overlap through a key was not run (by reading, the sorted union is taken). |
| S3-14 | partly | `exp(f)` and `f ^ 2` are refused for a reference write; `sqrt`, `*`, `/`, `round`, `ceil`, `trunc` and `pi()` are shipped and gave equal copies. A floating-point `sum`/`avg` over a distributed scan still depends on the origin (documented in the README, not changed). |
| S3-15 | fixed (by reading) | `synchronous_commit` is raised to `on` for a transaction that prepared remote participants (`node_conn.c`). Not run with a synchronous standby. |
| S3-16 | fixed (by reading) | The launcher passes `node_timeout` and `node_connect_timeout` to the worker in `bgw_extra`; the worker forces them at override level. `t/039` passes. |
| S3-17 | partly | The token is now bound to the database name (`kingres-forward-v2`), and `rotate_secret()` exists (atomic: with a lock timeout forced on node 2 the call failed and all three nodes kept the same secret; moves paused at `move_after_catchup`, `move_before_switch` and the copy phase completed after a rotation). Replay of a token seen on the wire without TLS remains, as documented. The rotation is a two-phase commit that applies on the map group primary first: connections opened by a statement in the short window between the first and the last commit may be refused; not run. |
| S3-18 | fixed | `INSERT INTO <view of a reference table>` now says "it writes through a view; write the table itself". |

## New findings

| ID | Severity | Summary |
|---|---|---|
| S5-01 | medium | Statements run by a trigger (or function) that fires on a reference table itself are planned without routing: a read of a distributed table returns the node's own buckets, a volatile function is not checked, and the copies diverge with no error |
| S5-02 | low | With nodes of two versions the diagnostics mislead: "cannot prove it" without a word about versions, and `check_cluster()` reports `reference_table_differs` ("table missing", digest 0:0) for copies that are equal |
| S5-03 | low | `internal_partition_identity(regclass)` is executable by every role for every table, without a privilege check |
| S5-04 | low | A warning that a prepared transaction could not be committed names a participant that had committed |
| S5-05 | note | A cancel that `cancel_bucket_move()` reports as "asked to stop" can be overridden by the move, which completes |
| S5-06 | note | Smaller points: wording of the refusal of `ADD COLUMN ... DEFAULT`; no instruction for restoring a node from a logical dump; `check_cluster()` is silent about a node without a secret (known, "not done" in the commit) |

### S5-01 (medium) Nested statements of a reference-table write are not routed or checked

Where: `planner.c` (`planner_hook_body`: `kingres_shard_in_refwrite_nested()` makes
every statement planned while a reference-table write runs on this node a plain
local statement), `fanout.c` (`refwrite_local_depth`, `run_local`, and the same
test for the fragment that a remote node runs). The intent is right for the
actions of foreign keys, which must act on this node's copy only. It is wrong for
a trigger or function that the write fires: its statements are planned as if the
node were alone, and are not shippability-checked, so the guarantee "a write to a
reference table reaches every copy ... or is refused" (README, table of paths)
does not extend to what the write does on each node.

Reproduction (three nodes, node 1 the map group primary; the trigger can only be
made by hand, as the README tells a superuser to do, because `CREATE TRIGGER` on a
reference table is refused otherwise):

```
-- node 1
CREATE TABLE public.d (id int PRIMARY KEY, v int);
SELECT kingres.create_distributed_table('public.d', 'id', bucket_count => 6);
INSERT INTO public.d SELECT g, g FROM generate_series(1, 30) g;  -- one INSERT per row
CREATE TABLE public.r (id int PRIMARY KEY, seen int);
SELECT kingres.create_reference_table('public.r');
-- every node, as superuser
CREATE FUNCTION public.cnt() RETURNS trigger LANGUAGE plpgsql AS
  $$ BEGIN NEW.seen := (SELECT count(*) FROM public.d); RETURN NEW; END $$;
SET kingres_shard.enable_routing = off;
CREATE TRIGGER t BEFORE INSERT ON public.r FOR EACH ROW EXECUTE FUNCTION public.cnt();
-- node 1, routing on
SELECT count(*) FROM public.d;           -- 30
INSERT INTO public.r VALUES (1, 0);      -- INSERT 0 1
-- every node
SELECT * FROM public.r;
```

Observed: `1|11` on nodes 1 and 3, `1|8` on node 2. No error. The same with a
trigger that sets a column to `clock_timestamp()`: three different values.
`check_cluster()` reports `reference_table_differs` for node 2 only (node 3's
digest equals node 1's by chance). What should happen: the write is refused, or the
statements of the trigger are routed like any other (the count is 30 on every
node), or the README says that triggers on reference tables must be deterministic and
node-local and that a read of a distributed table in one is wrong. Suggested fix: in
`kingres_shard_in_refwrite_nested()` pass through only the statements that touch
nothing but reference tables and the table of the foreign-key action; plan the rest
normally (so a read of a distributed table is a distributed scan, or fails with a
reason); document the rule for triggers on reference tables.

### S5-02 (low) Diagnostics when the nodes run two versions

(a) Nodes 2 and 3 run the 0.3 binary, node 1 the 0.4 binary with `ALTER EXTENSION
... UPDATE` run. Every statement that needs another node fails, from either side:

```
ERROR:  FATAL:  this session claims to have been opened by another node of the cluster but cannot prove it
server closed the connection unexpectedly ...
CONTEXT:  remote node ...
```

Nothing is written wrongly (a reference write fails before any copy changes; a
point write for a local key works; `move_bucket` fails with "cannot be asked for
its kingres_shard version"), which is what the README promises. But the message
suggests an intrusion and says nothing about versions: the token context changed
from `v1` to `v2` in 0.4. Suggested: the 0.4 side, on this refusal from a node, asks
(on a connection of a superuser, which both versions accept) for the extension
version and says so; or keep accepting `v1` for one release.

(b) In both skewed states (0.3 binary on 2 and 3, or 0.4 binary with 0.3 SQL
objects on 2 and 3), `SELECT * FROM kingres.check_cluster()` on node 1 returns

```
reference_table_differs|2|public.r|rows:digest of this copy are table missing, of the map group primary's 0:0; the copies of a reference table must be equal
```

for a reference table whose copies are equal (the first state also adds
`node_unreachable` rows, which are right). A report of divergence is the strongest
statement the tool makes; it should not be made for a node the function could not
ask. Suggested: report that node as unreachable or version-skewed and skip the
comparison.

### S5-03 (low) `internal_partition_identity` is open

`kingres_shard--0.4.sql` (the function has no `REVOKE`; `bucket_move.c`
`kingres_internal_partition_identity` calls no `require_node_session`). Reproduced:

```
-- as a role with no privilege on public.d
SELECT kingres.internal_partition_identity('public.d'::regclass);   -- a bigint
SELECT kingres.internal_partition_identity('pg_class'::regclass);   -- 0
```

It is a sum of mixed OIDs of the partitions, so it leaks little, but it answers for any
table and changes with every bucket move, which is what S4-06 asked to close for its
two siblings. Fix: call `require_node_session(parent)` first.

### S5-04 (low) A warning that names a participant that did commit

`SIGSTOP` of node 3 after the local commit of a reference-table write, `node_timeout = 3`:
the session returns after 4.0 s with two warnings, `could not commit prepared
transaction "..._3" on node ...:<node 3>` (true) and `... "..._2" on node ...:<node 2>;
recovery will commit it` (not true: node 2 had no prepared transaction left and showed
the row when the session returned). Reproduction: `after_commit:wait:<file>` on the
origin, `SIGSTOP` the postmaster and its children of node 3, remove the file, read
`pg_prepared_xacts` and the table on node 2 at once. By reading, the answers are
awaited one after the other against one deadline, and the answer of the healthy node
is taken as missing when the stalled one used up the time. The data are right; the
message makes an operator look for a transaction that is not there. Suggested: read
every answer that has arrived before treating the deadline as expired.

### S5-05 (note) A cancel can lose to the switch

`cancel_bucket_move()` sets `cancel_requested`, which the move reads only in its
catch-up loop (`kingres_shard--0.4.sql`, near line 2284). With the move held by the
fault point `move_after_catchup:wait:<file>`, `SELECT kingres.cancel_bucket_move(<id>)`
prints `NOTICE: bucket move N has been asked to stop; it is still catching_up`; when
the file is removed the move goes on to the switch and completes (`done`, the
bucket on the target). Without a fault point the window is the time between the end
of the loop and the change to `switching` (milliseconds), so this is a note; a
cancel that is accepted and then not honoured should still be an error or be
re-read before the switch starts.

### S5-06 (note) Smaller points

- `ALTER TABLE ... ADD COLUMN ... DEFAULT 'now'` on a reference table is refused with
  the heading "ALTER TABLE is not supported on distributed tables yet" followed by the
  right DETAIL; the heading says more than is true.
- `pg_dump` of one node holds that node's partitions (`COPY public.d_b0001 ...`) and
  nothing of the cluster catalog; a restore into a database that has the extension
  but no catalog produces plain tables, and a restore into a node that has the
  catalog meets the refusal of `COPY` into partitions (superuser repair with
  `kingres_shard.enable_routing = off` works; I checked that this superuser may
  delete from and insert into a bucket partition). The README has no instruction for
  restoring a node (it says only that the secret is not dumped).
- `check_cluster()` does not report a node that has no secret (the commit says so);
  ordinary roles then fail on cross-node statements with the hint to run
  `sync_catalog()`, a state the tool could name.

## Examined and found sound

- Original reproductions of S4-01, S4-02, S4-03, S3-01, S3-02 and the secret group
  S3-04, S3-06, S3-09 against 0.4: the wrong outcome of the earlier review does not
  occur (details in the table).
- **Reference-table writes by path.** Refused with a reason and nothing written:
  `COPY ... FROM` (also into a bucket partition), `MERGE`, a data-modifying CTE,
  `DELETE ... WHERE id IN (SELECT ...)` (parameter computed during execution),
  identity and serial defaults, volatile defaults, `ADD COLUMN` with a serial,
  identity, `gen_random_uuid()` or generated-by-`now()` column. Accepted and equal
  on all copies: `ON CONFLICT DO UPDATE ... RETURNING`, `ON CONFLICT DO NOTHING`, a
  scalar sub-select of the same table, `INSERT ... SELECT` from
  `generate_series`, a read-only CTE, `EXPLAIN ANALYZE` of a write (runs the write on
  every copy), a prepared statement, an explicit `OVERRIDING SYSTEM VALUE` insert,
  a stored generated column, `ALTER COLUMN SET DEFAULT random()` followed by a
  constant default (inserts omitting the column are refused while the volatile
  default is in force). A `CHECK (v > random())` constraint is accepted on a
  reference table (a write that passes on one node and not on another fails and
  rolls back; it cannot diverge).
- **Unprivileged role against the writer lock.** Advisory locks do nothing; the
  helper that takes the object lock is refused; a bounded, explained error for
  a writer behind a prepared transaction. Order of acquisition (node id, then table
  name, linked tables included) is the same for every writer; DDL propagates in the
  same node order, so I found no cycle (reasoned, not stressed).
- **Secret.** Not readable by any of seven roles; absent from logs at `debug5` with
  every parameter-logging setting at `-1`, from `pg_stat_statements`,
  `pg_stat_activity` (during the calls I could catch) and `pg_dump`; the forged
  claim is refused with no token, a wrong token, and on a node without a secret
  (ordinary role); `rotate_secret()` is atomic and does not break a move that is
  paused around it. `internal_forward_token` is refused to an ordinary role.
- **Moves.** Give-up leaves nothing on any node; worker resumption; two concurrent
  moves of different buckets in the same or opposite directions; `cancel_bucket_move()`
  during catch-up leaves no relation behind; the move after a give-up and after a
  worker resumption works; an autovacuum worker holding SHARE UPDATE EXCLUSIVE on
  the partition of the moving bucket did not starve the move (it was cancelled
  by the move and the move completed in 1.3 s; I had expected the non-queueing
  lock to stop that happening, and it did not reproduce).
- **Direct partition writes.** A superuser is not locked out of repair
  (`enable_routing = off`); ordinary roles are refused for `INSERT`, `UPDATE`,
  `DELETE`, `COPY ... FROM` into a partition, and `COPY ... TO` of a partition reads
  it as it is.
- **COPY TO of distributed tables** in text, csv with header, binary, with a column list,
  `FORCE_QUOTE`, to a program, from the map group primary and from another node:
  30 of 30 rows each time.
- **Upgrade.** 0.3 to 0.4 on a live three-node cluster (data, a reference table,
  a distributed table, the 0.3 binary then the 0.4 binary node by node):
  after the last `ALTER EXTENSION ... UPDATE`, scans, an ordinary role's
  reference write from another node and a move all work, `check_cluster()` is empty. The
  regress test `upgrade` (0.1, 0.2 and 0.3 to 0.4 equal to a fresh 0.4) passes.

## Not reproduced (suspected, with what would confirm)

- **A scan that fails with KS001 for ever under a steady stream of moves.** By reading,
  each move changes the identity of two nodes only once; a scan that overlaps a move
  fails and a repeat is free of it unless a move starts inside every repeat. I did
  not run a continuous back-and-forth against a continuous scan beyond what `t/032`
  does (it passes). To confirm: moves of one bucket back and forth every 100 ms
  while one client repeats a full scan; count the share of KS001.
- **Rotation window.** `rotate_secret()` commits on the map group primary and then on
  the others; a node-to-node connection opened between the first and the last
  `COMMIT PREPARED` with a token made from the new secret toward a node that still
  has the old one should be refused. To confirm: fault point `between_commit_prepared`
  inside the rotation, a scan started in the window.
- **S3-15** (synchronous standby): the code raises the setting; not run with a
  standby.
- **Hot standby or a promoted standby as origin of a scan or as map group
  primary** for the recovery worker's move upkeep (`pg_is_in_recovery()` returns
  early; by reading).

## Not covered

- Real locales and ICU, heterogeneous machines, TLS between nodes.
- A live upgrade from 0.1 or 0.2 on a running cluster: building such a cluster
  needs the 0.1 and 0.2 libraries, which I did not build (the 0.4 library cannot
  `add_node` on 0.1 SQL objects: "the table kingres.pending_node does not exist");
  only the catalog-equality regress test and the 0.3 path above.
- Parallel plans over distributed tables (refused by design since 0.2) and hot-standby
  reads.
- Logical decoding of the secret by a `REPLICATION` role (documented), WAL contents.
- A `pg_restore` of a node into a running cluster with routing on (see S5-06).
- A lock-order stress of reference-table writers against moves and DDL (reasoned,
  not stressed); the stall of writers during a move attempt as a latency
  measurement (`t/035` passes).
- Six TAP files started at the same instant failed in `initdb` three times on this
  machine (once in a run of `t/031` that was started alone while another test
  started its servers); every retry on its own passed. I did not find the cause
  (not a defect I can attribute to the extension: it happens before the extension is
  loaded). If it recurs in an integration run it will read as a failure of the
  suite.

## Summary of the verdicts

S4-01 fixed, S4-02 fixed, S4-03 fixed, S4-04 fixed, S4-05 fixed, S4-06 partly,
S4-07 fixed, S4-08 fixed, S4-09 not fixed (documented), S4-10 partly; S3-01 fixed,
S3-02 fixed, S3-03 fixed, S3-04 fixed, S3-05 fixed, S3-06 fixed, S3-07 fixed,
S3-08 fixed, S3-09 fixed, S3-10 fixed, S3-11 fixed, S3-12 fixed, S3-13 fixed,
S3-14 partly, S3-15 fixed (by reading), S3-16 fixed (by reading), S3-17 partly,
S3-18 fixed.
