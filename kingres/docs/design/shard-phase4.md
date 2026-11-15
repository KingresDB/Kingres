# kingres_shard: design of phase 4 (moving buckets and rebalancing)

Status: accepted for implementation, 2026-11-14.
Builds on [shard-phase2a.md](shard-phase2a.md) (extension version 0.2) and
produces version 0.3, with the upgrade script `kingres_shard--0.2--0.3.sql`.

Phase 4 of the roadmap in [architecture.md](../architecture.md) lets data
move between shard groups while the cluster stays in use. The extension
provides two things: a primitive that moves one bucket, and a function that
computes which buckets to move. Driving a whole rebalance (ordering,
concurrency, progress, stop and resume) is the control plane's job, as a
task that calls the primitive.

Not in phase 4: splitting a bucket, changing the bucket count, moving a
bucket between tables of different colocation groups, and anything about
standbys (phase 5).

## What moves

The unit is one bucket of one colocation group: the partitions
`<table>_b<bbbb>` of **every** table in the colocation group move together,
so that equal keys stay on the same group and joins on the key stay local.
Reference tables do not move; every node has them.

## Requirements on the cluster

- `wal_level = logical` on every node, with enough `max_replication_slots`,
  `max_wal_senders` and `max_logical_replication_workers`. The move checks
  these on both nodes first and fails with a message that names the setting.
- Every distributed table has a replica identity that logical replication
  accepts. A primary key qualifies, and a distributed table's primary key
  always includes the distribution column. A distributed table without a
  primary key or `REPLICA IDENTITY` makes the move fail before it starts,
  naming the table.
- Node-to-node connections for replication use the same options as other
  node connections (`kingres_shard.node_conninfo`), and the role used for the
  subscription needs the replication privilege on the source.

## The move

`kingres.move_bucket(colocation_id int, bucket int, target_group int)
returns bigint` runs only on the map group primary, and returns the id of a
row in `kingres.bucket_move`, the record of the move. The function returns
when the move is complete or raises an error; a move in progress is visible
to other sessions through that table. One move per bucket at a time; a
second call for the same bucket fails. Moves of different buckets may run
at the same time in different sessions.

```sql
-- On the map group primary only; never synchronized to other nodes.
kingres.bucket_move (
  move_id        bigint generated always as identity primary key,
  colocation_id  int not null,
  bucket         int not null,
  source_group   int not null,
  target_group   int not null,
  phase          text not null check (phase in
                   ('preparing','copying','catching_up','switching',
                    'cleaning','done','failed','cancelled')),
  detail         text,           -- last error, or what it is waiting for
  slot_name      text,
  started_at     timestamptz not null,
  updated_at     timestamptz not null,
  finished_at    timestamptz
)
```

Phases:

1. **preparing.** Take the cluster lock briefly, check the preconditions, set
   the bucket's state to `moving` in `bucket_map` (an atomic catalog change),
   and record the move. Statements keep going to the source group: `moving`
   does not change routing.
2. **copying.** On the target group, create the bucket's partitions as
   **standalone tables**, with the same columns, constraints and indexes as
   the source partitions but **not attached** to their parents. An attached
   but half-filled partition would be read by a distributed scan on the
   target node and return some rows twice. On the source, create a
   publication of the bucket's partitions; on the target, create a
   subscription to it with the initial copy enabled.
3. **catching_up.** Wait until the initial copy of every table has finished
   and the subscription's replay position is within
   `kingres_shard.move_catchup_threshold` (default 1 MB of WAL) of the
   source's current position.
4. **switching.** The only step that blocks writers, and only writers of
   this bucket:
   - lock the bucket's partitions on the source in a mode that blocks
     writes and allows reads;
   - wait until the subscription has replayed everything up to the source's
     position after the lock was taken; if that takes longer than
     `kingres_shard.move_switch_timeout` (default 10 s), release the lock,
     return to **catching_up** and try again later, so that a stuck move
     never blocks writers indefinitely;
   - in one two-phase transaction across the map group, the source and the
     target: detach the partitions from their parents on the source (they
     stay as orphan tables), attach the tables as partitions on the target,
     set the bucket's group to the target and its state to `active`, and
     raise the map version on every node;
   - drop the subscription and the publication.
   A statement that was routed with the old map and arrives at the source
   after the switch finds no partition there and gets the stale-map error,
   which makes its origin reload the map and retry, as in phase 1. A
   transaction that already wrote the bucket on the source and is still open
   holds a lock that the switch waits for (bounded by the timeout above).
5. **cleaning.** After `kingres_shard.move_cleanup_delay` (default 10
   minutes; long enough for statements that were reading the old partitions
   to finish), drop the orphan tables on the source. The recovery worker does
   this; `kingres.cleanup_moved_buckets()` does it on request.
6. **done.**

### Failure and resumption

Every phase change is recorded before the next action, and every action is
written so that it can be repeated. `kingres.resume_bucket_moves()` (called
by the recovery worker and on request) continues or undoes each unfinished
move:

- a move that had not reached **switching** is undone: drop the
  subscription, the slot and the publication, drop the standalone tables on
  the target, set the bucket back to `active` on the source, and mark the
  move `failed` with the reason (it can simply be started again);
- a move interrupted during **switching** is decided by the two-phase
  commit: either the switch transaction committed, and the move continues
  with cleaning, or it did not, and the move is undone as above.

`kingres.cancel_bucket_move(move_id)` requests the same undo for a move that
has not reached **switching**.

A replication slot left behind by a failed move would retain WAL on the
source forever. Undo therefore always drops the slot, and
`kingres.bucket_move` records its name so that a slot whose move row says
`failed`, `cancelled` or `done` can be found and reported by
`kingres.check_cluster()` (below).

## Planning a rebalance

`kingres.rebalance_plan(max_moves int default null) returns table
(colocation_id int, bucket int, source_group int, target_group int,
size_bytes bigint)`: the moves that would even out the number of buckets per
group, for every colocation group, in the order they should run.

Rules:

- only groups whose primary has `should_have_shards` receive buckets; a
  group without it is drained completely (this is how a server is emptied
  before it is removed);
- the target count per group is the bucket count divided by the number of
  receiving groups, with the remainder spread one each;
- the plan moves the fewest buckets that reach those counts, and never moves
  a bucket between two groups that both stay within their target;
- colocation groups are independent, but the plan interleaves them so that a
  receiving group fills evenly;
- `size_bytes` is the current size of the bucket's partitions on the source
  (null when it cannot be read), for progress display, not for the choice.

The function only reads; it changes nothing and can run on any node.

`kingres.set_node_property(node_id int, should_have_shards bool)` changes
the flag (an atomic catalog change).

## Checking a cluster

`kingres.check_cluster() returns table (check_name text, node_id int,
object text, detail text)`: one row per problem, no rows when everything is
consistent. It compares, on every node, the catalog with the map group's,
the partitions that exist with the bucket map (a bucket's partition missing
on its owner, or present and attached on a node that does not own it), the
orphan tables, replication slots and subscriptions with the move records,
and prepared transactions older than a threshold. The control plane shows
its output and raises alerts from it.

## Settings added

| GUC | Default | Meaning |
|---|---|---|
| `kingres_shard.move_catchup_threshold` | 1MB | How close the copy must be before the switch is attempted |
| `kingres_shard.move_switch_timeout` | 10s | Longest time writers of the bucket are blocked by one switch attempt |
| `kingres_shard.move_cleanup_delay` | 10min | How long orphan tables are kept after a move |

## Tests required

Single node (pg_regress): `rebalance_plan` for hand-built maps (even, one
empty group, a draining group, several colocation groups, remainders,
`max_moves`), checked against the rules above; `check_cluster` on a clean
catalog; the upgrade from 0.2.

Multi-node (TAP, three nodes unless stated):

- A move of a bucket with rows in two colocated tables and a foreign key
  between them: before and after, every row is returned exactly once from
  every node, the partitions are attached on the target and gone from the
  source after cleanup, and the map version rose on every node.
- Writers during a move: a workload of inserts, updates and deletes on keys
  of the moving bucket and of other buckets, from all three nodes, runs
  throughout; afterwards the table equals a model kept by the test, no
  statement failed except with errors the test expects and retries, and no
  row is duplicated or lost.
- A long transaction that wrote the bucket holds the switch back, the
  timeout returns the move to catching up, and it completes after the
  transaction ends.
- Readers during a move never see a row twice (the comparison harness of
  phase 2a, run in a loop on all nodes while moves run).
- Fault injection at every phase boundary and inside the switch (crash of
  the map group primary, of the source, of the target; connection loss):
  after `resume_bucket_moves` the bucket is whole on exactly one group, no
  slot, publication, subscription or stray table is left, and
  `check_cluster` is clean.
- Cancel before the switch.
- A full rebalance after adding a third node to a two-node cluster, driven
  by calling `rebalance_plan` and `move_bucket` in a loop: the counts end up
  as the rules say; then draining a node with `should_have_shards = false`
  and removing it.
- Preconditions: `wal_level` not logical, a table without a replica
  identity, a second move of the same bucket; each fails cleanly and leaves
  nothing behind.

## Implementation notes and deviations

Added when `contrib/kingres_shard` version 0.3 was implemented, on PostgreSQL
18.7. Everything above is implemented as written except what is listed here; each
item says what differs and why.

**Structure.** The orchestration of a move (the phases, the waits, the undo, the
resumption, the plan, the check) is PL/pgSQL in the install script; what needs C is in
`src/bucket_move.c`: `kingres.internal_node_exec()` (SQL on a node in a session of its
own, outside the caller's transaction, bounded and interruptible), the switch, the
attach and detach of a bucket partition, the connection string of the subscription,
and the mark of a live move. A move needs sessions that commit at once (its record,
`CREATE SUBSCRIPTION`, the slot) and waits on other nodes, which a function in one
transaction cannot do, so `move_bucket` reaches the map group primary itself through
`internal_node_exec` for everything that must be durable. Extra columns of
`kingres.bucket_move` (not in the design): `cancel_requested`, `cleanup_after`,
`replication_dropped`. Extra tables: `kingres.move_marker`. Extra functions: the
`internal_*` ones, `check_cluster(older_than interval)` (the argument is new).

**What the design got wrong about PostgreSQL 18, or left open**

- *Detach needs the parent exclusively.* "Lock the bucket's partitions in a mode that
  blocks writes and allows reads, then detach" deadlocks: a writer that is queued for
  the partition holds the parent in ROW EXCLUSIVE, and `ALTER TABLE ... DETACH
  PARTITION` (not `CONCURRENTLY`, which cannot be prepared) needs ACCESS EXCLUSIVE on
  the parent; the detach would wait for writers that wait for the lock held by the
  session that detaches. The switch locks the **parents** of the group's tables on the
  source in SHARE mode (writers wait, readers go on) and upgrades at the detach.
  Consequence: the writers of **every** bucket of those tables on the source node wait
  for the length of the attempt, not only the writers of the moving bucket.
- *The attach of a table with a foreign key locks the referenced table SHARE ROW
  EXCLUSIVE.* The switch takes that lock on the target first; the writers of the
  group's tables on the target wait too.
- *The two node locks form a cycle nobody sees.* A writer that comes from the source
  node and goes to the target holds a lock on its own node while it waits for the other
  (and the reverse); the switch holds one node and waits for the other. Each lock of
  the switch is therefore waited for for at most `max(200 ms, move_switch_timeout/10)`
  (a lock timeout, a retryable failure), and the move retries after a random pause. The
  measured cost: a cycle holds writers for at most that long (1 s at the default).
- *The slot of a move waits for every running transaction of the source* (the
  snapshot of `CREATE_REPLICATION_SLOT` must be consistent), and the session that runs
  the move must not be one of them: it holds no row lock and no transaction id while it
  waits. The mark of a live move is a session-level lock on a tag of its own (an
  advisory-lock tag whose fourth field the SQL advisory functions never use), not a row
  lock on `bucket_move`.
- *`confirmed_flush_lsn` is not a replay position.* It advances with transactions that
  touch published tables or with a keepalive after half of `wal_sender_timeout`. The
  switch point is proved by a **marker row** instead (README, "How it knows"); the
  design's "within the threshold of the source's current position" is checked with the
  slot lag as a quick test and the marker round trip as the proof.
- *`CREATE SUBSCRIPTION` with `create_slot = false` can run in a function*; only
  `create_slot = true` and `DROP SUBSCRIPTION` of a subscription that has a slot cannot.
  The slot is made first (`pg_create_logical_replication_slot`), then the subscription
  uses it (`copy_data = true`); dropping goes through a plain statement, falling back
  to `slot_name = NONE` when the publisher cannot be reached.
- *The subscription is created with `synchronous_commit = on`*: the default `off` of an
  apply worker would let a crash of the target lose applied changes after the
  subscription (the only way to replay them) is gone.
- *`ATTACH PARTITION` scans the partition unless an equal check exists.* The copy gets
  `CHECK (satisfies_hash_partition(...))` first, which PostgreSQL recognizes as implied
  ("partition constraint ... is implied by existing constraints"), so the attach of a
  table without foreign keys needs no scan under the lock; the check is dropped after.
  With a foreign key the attach validates it (a scan of the copy under the lock).
- *A detached partition keeps its foreign keys* (to the parent and to every partition
  of the referenced table), and the referenced table would look for rows in the
  orphan. The detach drops them; the orphan is moved to schema `kingres` as
  `orphan_<move>_<n>`.
- *Dropping a table fires the drop trigger, which deleted from `kingres.dist_table`
  with no row to delete and so counted the map version up on that node alone*; map
  versions are now compared across nodes (below), so the trigger deletes only when a
  distributed table is dropped.

**The race between a move and a statement that reads or writes several groups**
(reported by the designer of phase 2b; the first version of the rule below had a hole,
found by `t/031_move_scan_interleavings.pl`, fixed here).

*The cause of the wrong answers.* The switch is one two-phase transaction: the map
group primary commits first, then `COMMIT PREPARED` reaches the other nodes one after
the other. A distributed scan opens a cursor on each group, then asks each group for
its map version and compares it with its own. ATTACH PARTITION takes only SHARE UPDATE
EXCLUSIVE on the parent, so on the **target** a reader's lock does not wait for it.
Interleaving that returned 211 rows of 240 (reproduced on demand): the origin node
(not the source) has committed the switch and plans with the new map; the source has
committed (the bucket is detached; a cursor opened there waits for the detach lock, so
it opens only after the source's commit); the target has the attach prepared and not
committed. The scan's cursor on the target opens now, **without the bucket** (the
attach is not visible), and stays open while the cursor on the source waits for the
source's commit; the target commits meanwhile; the version check, made after the
cursors are open, reads the new version on every node and passes. Nobody returned the
bucket. (A node's version being behind the plan is not the cause: the comparison is
`!=`. What the check cannot see is that the *cursor* was opened before the version it
later reads.) The same hole existed for a routed read to the target when the catalog
cache was reloaded while the partition list was stale, and, when the map group primary
is the source or the target, the version and the partitions of that node are changed
by two transactions (its own, and the control session that detaches or attaches), so a
version could be seen before its partitions.

*The rule.* (1) A node's bucket partitions of a table and its placement of the
colocation group (which group holds each bucket) change only in the transaction of the
switch on that node, and on the map group primary, when it is the source or the target,
that part runs in the coordinator's own transaction (it used to run in a session of the
coordinator to its own node, which committed before the coordinator did: the node
showed the new catalog with the old partitions for a moment, and a statement of that
node, a scan or a routed read, lost the bucket). (2) The source holds ACCESS EXCLUSIVE
on the parents of the group's tables from the detach until its COMMIT PREPARED has
returned, so a statement that opens the tables there either got its lock before (the
switch cannot prepare, so it cannot commit anywhere, until the statement's transaction
ends) or waits and sees the node after the commit. The target has no such lock (see
"Why not a lock on the target" below). (3) A scan asks every remote node for the
placement of the colocation group **before** it opens its cursor there and **again
after** (`kingres.internal_placement_hash(colocation_id, parent)`: one number over
`(bucket, group)`, read from `kingres.bucket_map` with the latest snapshot, never from
a cache; the first call also drops this backend's relcache entry of the parent when
the placement is not the one it saw last, because a commit becomes visible a moment
before its invalidation messages arrive), and compares both answers and its own
(from its cache, which the plan used) with the placement in its plan. It also compares
the number of partitions the table has on its own node, read from `pg_inherits` with the
latest snapshot, with the number the planner saw when it expanded the table (`rel->nparts`,
the one number that belongs to the partitions of the child path: reading the partition list
of the relcache again a moment later, in the hook, gave a number that an invalidation
processed by a lock in between could make newer than the child path, which lost the bucket
in 3 of the first 72 runs of `t/032`). A fan-out write compares the plan's
placement before it runs, and the placement of every target after it ran. Any
difference raises `KS001` before the first row (scan) or rolls the statement back
(write). *Why it is sufficient:* the placement of a node changes together with its
partitions (1), so equal numbers before and after the cursor's `DECLARE` mean that no
switch committed there in between and the cursor was planned with the partitions of that
placement; what happens later cannot change what the cursor reads: on the source the
parent lock holds the detach back, on the target a later attach adds a partition the
plan does not have, and a plan whose placement does not give the bucket to the target
reads it from the source. If all placements equal the plan's, each node serves exactly
the buckets the plan gives it, so every bucket is read once, by its owner. A group that
owns nothing in the plan cannot hold a bucket either: its source is a participant, and
reports the plan's placement only while it still has the bucket, which it keeps (parent
lock) until the scan's transaction ends. The placement replaces the map version in the
comparison: the version counts every catalog change (a move's `moving` state, another
table, another colocation group) and a scan would fail on each.

*Why not a lock on the target.* A first fix made the switch also take ACCESS
EXCLUSIVE on the target's parents until the commit (readers then wait for it, as on the
source). It is correct, and it starves the move: a scan opens its cursors on all nodes at
once, so one that holds the target and waits for the source (which the switch holds)
makes a cycle with the switch, which waits for the target; the lock timeout breaks it
and the attempt fails, and with scans running back to back (`t/026`) nearly every
attempt does. The source never had that cycle because only one node was locked against
readers. The placement check on the target costs nothing for the switch.

Liveness: `KS001` only when the placement of the scan's colocation group changed (a switch
committed on a node it reads); a scan that opens the tables on the source while the switch
is committing waits for the commit (milliseconds); a scan or write that holds the tables
on the source holds the switch back (it gives up after its lock timeout and tries again).
*Measured* (out of tree, one machine, `cassert` build). `t/026_move_vs_scan.pl` alone:
the first version of the rule failed 3 runs of 10 (a scan returned 211, 216 or 223 rows of
240; 0.8% of the scans, 1852 in 232361, ended with `KS001`); the final one 0 of 10, 0.5% of
the scans with `KS001` (960 in 186797). `t/031_move_scan_interleavings.pl` (forced
interleavings, default form: every ordered pair of groups, every order of the commits, a
statement from each of the three nodes in each state), on the code as found plus the fault
points: of 1715 statements 206 returned a wrong answer and 220 failed with a foreign key
violation or a deadlock instead of `KS001` (the catalog reload); on the final code 0 and 0
in 1235 statements (50 moves). The full form (`KS_INTERLEAVINGS=full`: every order for every
kind, four pause points of the scan, five inside the switch): 0 and 0 in 4939 statements
(152 moves, 844 test points). `t/032_move_scan_stress.pl`, four copies at once (6 reader and
writer processes on 3 nodes each, 24 moves each): code as found, 9 of 12 runs failed, 6 of
them with wrong answers (`KS001` about 0.22% of the statements); final code 0 wrong answers
and 0 other failures in 128 runs (5.6 million statements), `KS001` about 0.19%. `KS001` is
the same order as before because a scan that overlaps the commit of a switch must still
fail; the other changes of the catalog no longer fail it.
For each path:

- *Distributed scan*: (3) on every remote group, and the cached placement of this node
  after the local locks (the executor holds them). The catalog reload that the first
  version made on a mismatch is removed: it ran in the transaction that the error
  ends, and deadlocked with the switch.
- *Fan-out write (`UPDATE`/`DELETE` that span groups)*: (3) on every target after the
  statement ran on all of them, and the placement of the plan is compared before it runs
  (a cached or generic plan can be older than the catalog). The writer holds ROW
  EXCLUSIVE on the parents, which the switch's SHARE (source) and SHARE ROW EXCLUSIVE
  (target) wait for, so a switch cannot commit between the fragments of a statement
  that has already written; one that straddles it is rolled back by the error.
- *Single-group routed statements*: the author's argument ("a node's partitions and
  catalog change in one transaction, and the statement locks the parent before the
  planner checks the catalog") was wrong for the map group primary: its part of the
  switch ran in a second transaction, so a statement of that node planned with the new
  catalog and read the parent before the attach was visible, and found no row (forced in
  `t/031`, kind `point`, 50 wrong answers before the fix; the same for a routed write to
  the bucket, which waits for the target's SHARE ROW EXCLUSIVE and is not affected).
  With (1) the argument holds for every node but one case it cannot cover: on a target
  that is not the map group primary, a reader's lock does not wait for the attach, and
  the node's own catalog says whether the bucket is its (`KS001` until the commit is
  visible and its invalidation delivered). There remains a window of microseconds in
  which the catalog cache of a backend is loaded after a commit while its partition
  list is not yet current; it needs a cache invalidated for another reason at that
  moment, it cannot be forced by a test, and a lock that closes it was rejected (above).
  This is the one place where the rule is argued and not proved by a forced
  interleaving.  On the map group primary, which commits the switch before the group
  that gave the bucket up, a statement planned just before the commit can find that
  group already changed: the stale-map retry also runs there (`remote_scan.c`), once.
  The reload of that retry now takes SHARE ROW EXCLUSIVE on `kingres.map_version` before
  it applies the map group's document: it waits for a catalog change that is in flight
  on the node (its prepared transaction holds ROW EXCLUSIVE there) instead of writing
  over the rows of it, which failed with a foreign key violation or a deadlock where the
  caller expects `KS001`.
- *Reference tables*: every node has all of them and they do not move; a join of a
  distributed table and a reference table reads the reference table on the origin, and
  its distributed side is the scan above (forced in `t/031`, kind `join_ref`).
- *Joins of two distributed tables, subplans, rescans, cursors, EXPLAIN ANALYZE*: each
  scan node runs (3) when it starts, and its lock on the parent of the source pins the switch
  for the rest of the transaction; a rescan opens new cursors and checks again; a cursor
  fetched in several round trips keeps its locks. Parallel workers are refused.
- *COPY*: `COPY (SELECT ...) TO` is a scan. `COPY ... FROM` into a distributed table of
  several groups is refused (utility.c); `COPY t TO` of the relation itself does not go through the planner hook and was not
  examined here (it is not changed by a move any more than before).

**Other deviations**

- *Writers are blocked on both nodes, and every bucket of the tables.* See above. The
  design said "only writers of this bucket". Measured in `t/027_move_under_load.pl`
  (three writers on three nodes, four moves under load, every statement started as its
  own `psql`, so each time includes its start): a move takes about 0.2 s from the start to
  the end of `move_bucket` when the switch meets no lock cycle, and about 1.6 s when one
  attempt ran into a cycle and gave up after the lock timeout (1 s at the default); the
  longest statement of the moving bucket and of the other buckets was 1.0 s (that lock
  timeout) in the runs of `t/027_move_under_load.pl` (it prints them), otherwise tens of milliseconds.
  The switch itself (the time the source tables are locked) is not recorded separately.
- *DDL during a move is refused* (the design did not say). The standalone copy is not
  attached, so a propagated statement would miss it.
- *A move to a group whose primary has `should_have_shards = false` is refused.*
- *`kingres.cleanup_moved_buckets(force bool default false)`* and
  *`kingres.check_cluster(older_than interval default '5 minutes')`* have an argument
  each. `check_cluster` runs on the map group primary and is for superusers (it asks
  every node); it does not compare the version counter.
- *`remove_node()` refuses a group that takes part in an unfinished move, including one
  that is cleaning* (its orphan tables must be dropped first: `cleanup_moved_buckets(true)`).
- *Resume waits `2 x node_timeout`* before it undoes a *switching* move, so that a
  switch that is still committing in its session is not undone.
- *Fault injection*: `kingres_shard.fault_injection` takes `kill:<n>` (let n hits of the
  point pass), the setting of the session that runs a move is copied to the control
  sessions it opens, and a `wait:<file>` point creates `<file>.reached`.  A point can carry a scope, `after_commit@switch:wait:<file>`:
  it fires only in a session that has run the switch (`kingres_shard_fault_set_scope`), so
  that the commit of the switch can be stopped and not that of the earlier steps of the
  move, which are two-phase transactions too.  Points added for `t/031`: `scan_begin`,
  `scan_started`, `scan_declared_g<group>`, `scan_after_declare`, `scan_after_check`,
  `fanout_after_run`. The points of a
  move are `move_after_preparing`, `move_after_target_tables`, `move_after_publication`,
  `move_after_slot`, `move_after_subscription`, `move_after_catchup`, `move_before_switch`,
  `move_switch_after_lock`, `_after_marker`, `_after_attach`, `_after_detach`,
  `_after_local`, `move_attach`, `move_detach`, `move_after_switch`,
  `move_after_drop_replication`, `move_undo_after_subscription`, `move_undo_after_tables`,
  `move_cleanup_after_orphans`, and the points of the commit protocol.
- *Found while testing the rule, not fixed*: moving a bucket away from a node a second time
  before `cleanup_moved_buckets()` has dropped the first orphan fails with "relation
  <table>_b<nnnn>_pkey already exists in schema kingres": the detach moves the table to
  schema `kingres`, and its indexes keep their names there.  `t/032` cleans up after
  every move for this reason.
- *Not implemented*: `REPLICA IDENTITY USING INDEX` tables and tables with identity
  columns raise "not supported yet" at the start of a move; a foreign key cycle between
  the group's tables too.
