<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
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
  measured cost: a cycle holds writers for at most that long (1 s at the default). (0.3 as
  reviewed: the locks are no longer queued for but tried for; see "Review of 0.3: the
  switch" below.)
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
  visible and its invalidation delivered). There was a window in which the catalog cache of a backend is loaded after a commit
  while its partition list is not yet current; it is closed and forced by `t/033_move_target_window.pl`
  (section "A routed statement on a target that is not the map group primary", below).
  On the map group primary, which commits the switch before the group
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
- *Moving a bucket away from a node a second time* is described in "Orphan tables of
  several moves" below.
- *Not implemented*: `REPLICA IDENTITY USING INDEX` tables and tables with identity
  columns raise "not supported yet" at the start of a move; a foreign key cycle between
  the group's tables too.

**A routed statement on a target that is not the map group primary** (the one case the
rule above could only argue; closed and forced in the fix round for review S3, item 2).

*The window.* A backend learns of a commit in two ways that do not meet. The extension's
catalog cache is rebuilt from the catalog tables with the latest committed snapshot, when
the invalidation of `kingres.map_version` has dropped it. The partition list of a table
(the relcache entry, which the planner prunes with) is rebuilt when the backend *processes
the invalidation messages of the switch*, and a backend processes messages only when it
starts a transaction, takes a lock it does not hold, or opens a catalog on a syscache miss;
a catalog snapshot that was taken before the commit also stays in use until then. A session
that has processed the messages sent before the switch (they drop the catalog cache) and has
nothing to process afterwards can therefore, after the switch has committed on its node, load the
catalog cache (new map: the bucket is here) and plan with a partition list that was built
before the attach (no partition for the bucket): partition pruning of `WHERE id = <key>`
finds no partition and the statement answers "no rows". A reader's `ACCESS SHARE` lock does
not wait for the attach (decided above, "Why not a lock on the target"), so nothing orders the
two. The same window opens for any backend between its last processing of messages and
its read of the cache, which includes the moment after a commit is visible and before its
messages are queued.

*Forced.* `t/033_move_target_window.pl` builds the state with a fault point, `route_before_catalog`
(in the planner hook, before the catalog cache is read), and a session that holds what it needs in an
explicit transaction: routing off while it reads the table and the catalog tables (so the cache is
not loaded and the locks of the catalog tables are held, which the cache load would otherwise
take, and take messages with them), then routing on and the statement, which stops at the fault
point after it has been parsed. The move then completes on every node, and the session is released.
Without the fix the statement answered `0` (the partition list had 2 partitions, the catalog 3)
in every run; with it, `1`.

*The fix.* After the planner hook has read the cache and found the distributed tables of the
statement, `kingres_shard_ensure_fresh_partitions()` (`src/bucket_move.c`) makes each table's
partition list at least as new as the cache: a backend remembers, for each table, the version of
the bucket map for which it last rebuilt the relcache entry, and when the version in the cache it
is about to plan with is another it drops the catalog snapshot and the relcache entry, which the
next use rebuilds from the catalog as it is now. *Why it is sufficient:* a switch changes the
partitions of a node and its placement, and counts the map version up, in one transaction on that
node; a cache that shows version V includes every switch committed up to V, so the committed state
from which the catalog snapshot taken *after* that read is built includes those switches' partitions,
and the rebuilt entry has them. The statement cannot be planned with a partition list older than the
cache version it plans with. A switch that commits after the cache was read does not matter: the
statement then follows the old placement, which the source still serves (it holds the parent
exclusively until its own commit), or gets `KS001`. The routed statement of a forwarded session
takes the same path. The distributed scan has its own check of the partition count.

**Orphan tables of several moves** (item 1 of the fix round). A bucket that leaves the same node
twice before `cleanup_moved_buckets()` has dropped the first orphan failed with "relation
<table>_b<nnnn>_pkey already exists in schema kingres": the detach moved the table to schema
`kingres`, where table and index names share one namespace, and its indexes kept their names.
Decision: the detach renames every index of the partition (a constraint that an index implements
follows the index) to `<orphan>_i<k>` before the table moves, so the orphan's objects carry the move
id and cannot meet those of another orphan. The alternatives were rejected: dropping the earlier
orphan when the next move of the bucket starts would take away the delay `move_cleanup_delay`
guarantees to statements that still read it; one schema per move needs a schema to create, check,
drop and protect. The orphan's table and its indexes keep the one-to-one relation of name to move
that `cleanup_moved_buckets()`, `check_cluster()` and the orphan listing already use (they look at
tables only). `t/025_move_bucket.pl` moves a bucket away, back and away again with no cleanup
between, and `t/032_move_scan_stress.pl` no longer cleans up after every move.

**Tests that failed under load** (item 4 of the fix round; each was run 30 times alone on the
machine with as many busy loops as it has processors, before and after; the figures are in the
report of that round).

- `t/027_move_under_load.pl`, "the move went back to catching up when the switch timed out":
  *the test was wrong.* It started the transaction that holds the switch back when it saw the
  phase `catching_up`, but that phase begins with "waiting for the initial copy". The copy of
  each table creates a slot of its own on the source, and a slot waits for every transaction
  that is running when it is created, so a transaction that starts during the copy holds the
  copy back, the move never reaches the switch, and "switch attempt gave up" never appears. On
  an idle machine the copy of three small tables is finished before the test's next statement
  starts; under load it is not. The move now stops by itself at the end of the catch-up (the
  fault point `move_after_catchup`), the transaction starts there, and the move is released. The
  assertions are unchanged.
- `t/028_move_faults.pl`, the source node dies during the copy: *the product was wrong.* The loop
  that waits for the initial copy spoke to the target only. With the source gone the copy cannot
  finish and the apply and table-copy workers keep failing to connect, which is not counted as
  an error of the subscription, so `move_bucket` waited forever (the test waited for it and
  hung); on an idle machine the copy is done before the node stops, and the next call that
  reaches the source (the lag of the slot) fails the move. The waiting loop now asks the source
  something at every round while the copy is incomplete, so a source that cannot be reached
  fails the move within `node_timeout` and the undo and `resume_bucket_moves()` take over as
  for any other failure.

## Review of 0.3: the recovery worker, versions, privileges (S4-02, S4-05 to S4-07, S4-09, S4-10)

**The worker runs the upkeep of moves (S4-02).** The documentation said the recovery
worker resumes interrupted moves and drops orphan tables; the code did not. The worker
of the map group primary now, after it has resolved the prepared transactions and
committed that transaction, runs `kingres.resume_bucket_moves()` and then
`kingres.cleanup_moved_buckets()`, each in a transaction of its own, a failure of one
logged and not stopping the other (`run_move_step()` in `recovery.c`). Nothing new
is needed to make this safe: both functions already skip a move whose session holds
the live mark (`internal_move_alive`), take the record `FOR UPDATE SKIP LOCKED`,
bound every call to another node by `node_timeout`, and the launcher terminates a
worker that has not finished after `5 x node_timeout`; every step they take is
repeatable, so a terminated pass leaves a state the next pass continues from. A
retried move cannot meet the old one: a unique index allows one unfinished move per
bucket, and the undo changes only the bucket state `moving` of its own source group.
One gap was real: `internal_begin_move` commits the record (phase *preparing*) in a
session of its own and the move's session takes the live mark a moment later, so for
that moment a live move looks abandoned. `resume_bucket_moves()` therefore leaves a
*preparing* move alone for `2 x node_timeout`, as it already did for *switching*. A
worker on a node whose SQL objects lack `kingres.bucket_move` (library newer than
the SQL) does not call the functions.

`check_cluster()` gets `stalled_move`: a move before *done* that no session runs
(`internal_move_alive` is false) and that has not been touched for `2 x (node_timeout +
recovery_interval)` (a *cleaning* move counts from its cleanup time), the time in
which the worker, or a person when the worker is off, has certainly had a pass. The
overdue-orphan threshold is the same time instead of the former fixed hour.

**Versions (S4-05).** Two cheap defences, no new catalog. `kingres.move_bucket()` and
`internal_begin_move()` call `internal_check_node_versions()`: every active primary
is asked for its `pg_extension` version over the ordinary node connection (a query that
needs nothing of the extension, so a node with older SQL answers it), and a node that
reports anything but 0.3, or cannot be asked, stops the move before anything is
recorded, with its id and address. A distributed scan or fan-out write that reaches a
node whose helper functions are missing gets "node ... runs an older version of the
kingres_shard SQL objects", raised where the library turns a remote error into a local
one (`kingres_shard_report_error`), instead of the remote "function does not exist".
A rolling upgrade is still a window with failing cross-group statements; the order to
follow is in the README. The check compares with the literal 0.3: a later version
changes it with its script.

**Who may call the helpers (S4-06).** They are called from the sessions that other
nodes open, which run as the role of the user whose statement it is, so execution
cannot be limited to superusers, and they must not distinguish tables of which the
caller has no knowledge. `internal_placement_hash()` and `internal_map_version()` now
refuse unless the caller is a superuser, a session that proved it was opened by a
node (the same proof as for the forwarded flag), or, for the placement of a table, a role with
`SELECT` on that distributed table; any other relation is refused before anything is
invalidated, so a role can no longer make a backend rebuild cache entries of relations
it names.

**Move records (S4-07).** `kingres.bucket_move` is no longer readable by PUBLIC; the
view `kingres.bucket_move_status` (ids, groups, phase, times) is. The view is an
ordinary view of the extension owner's, so its readers need no right on the table.

**The xmin horizon (S4-09): documented, not changed.** `move_bucket()` is a function,
so the statement that calls it, and its snapshot, last as long as the move; a function
cannot end its caller's transaction or snapshot. Moving the long waits into a
procedure (which may commit) or into steps driven by a poller changes the interface
of the one function the control plane calls, for a cost (a delayed vacuum on the map
group primary, whose only changing tables are the small catalog tables) that is
documented in the README.

**check_cluster is structural (S4-10).** It stays so: comparing the content of
buckets means reading them, which a check that runs on a cluster in operation must
not do by default. The README says what it does and does not compare, and that the
old copy of a moved bucket stays readable until the cleanup.

**Transactions across a switch.** `t/037_move_transactions.pl` holds, across a switch,
a prepared user transaction, REPEATABLE READ and SERIALIZABLE readers on the source and
the target, and a cursor on a third node, and runs COPY and INSERT ... SELECT during a
move. What it found: everything that holds a lock on the group's tables on the source
(a writer, a prepared transaction, a reader, a cursor, and also a transaction on any
other node that scanned the table, because its remote transactions stay open) holds the
switch back and the move gives up cleanly; the data stays whole in every case. One
case lost rows: a REPEATABLE READ or SERIALIZABLE transaction on the *target* that has
read only its own buckets holds nothing on the source, so the switch completes, and the
rows of the arrived bucket, which the copy wrote after the transaction's snapshot, are
invisible to it: its next read of the table returned 50 of 60 rows with no error
(reproduced before the fix). The fix is in `kingres_shard_ensure_fresh_partitions()`,
which the planner calls for every distributed table of a statement: in a transaction that
uses one snapshot for all its statements it counts the partitions of the table that the
snapshot sees (the `pg_inherits` rows) and the partitions of the latest state, and when
the latest has more raises a serialization failure that says the transaction can be
retried. It is conservative (a transaction whose snapshot was taken after the copy but
before the attach is refused although it would have seen the rows) and costs two index
scans of `pg_inherits` per distributed table per statement, only in such transactions.
The reverse direction (a bucket that left this node after the snapshot) is not affected
by the snapshot rule: its rows are read from the new owner with the statement snapshot
of that node, as every remote read is (no global snapshot, README "Guarantees").
## Review of 0.3: the switch (S4-01, S4-03, S4-04)

**What a scan's two reads must prove (S4-01).** The scan of 0.3 compared the placement of
the colocation group (a sum of a mix of every `(bucket, group)` pair) before and after the
`DECLARE`. What it has to prove is "no switch of this colocation group committed on that
node between the two reads", and the placement is a function of the state, not a count of
switches: a bucket that moves S to T and back to S gives the same placement before and
after, while the cursor on S was declared, and planned, while the bucket was detached (its
`DECLARE` waited behind the first switch's lock), and the second switch re-attaches the
bucket with a lock (SHARE UPDATE EXCLUSIVE on the parent) that the cursor's ACCESS SHARE
does not conflict with. Reproduced at the tree (`t/031`, the away-and-back block): the scan
answered 14 rows of 30 with no error.

The compared value is now a second number next to the placement: the **identity of the set
of partitions** the node has of the table (`kingres.internal_partition_identity(parent)`,
`catalog.c`: the sum of a mixed value per OID of the attached partitions, from the latest
committed state of `pg_inherits`). Every switch either detaches a partition from the node
(the old table, which then becomes an orphan under another name) or attaches one (a new
standalone table the move built), so the set changes at each switch, and the table that
comes back is a different relation with a different OID: the identity cannot return to an
earlier value unless the server hands out one OID twice while a single statement starts,
which needs 2^32 relation creations. That is "monotonic" in the only sense the check needs,
and it needs no counter in the catalog (a counter would be a catalog change and an upgrade
script; none is needed). The scan asks for placement and identity in one statement before
the cursors and in one after them and compares: the placement against the placement its
plan was made with, the identity against the identity of the same node before. The local
part of the scan carries, in its plan, the identity of the partitions the planner expanded
(taken from the planner's partition directory, which is what the child path is made of,
not from the relcache, which can be rebuilt after the expansion), and compares it with the
catalog at the start of the scan and again after the remote checks: this replaces the
count of partitions of 0.3, which a detach and an attach cancel out. Remote groups were
covered only by the placement before; each node's own identity covers them now, which is
the second case the reviewer listed ("a back-and-forth on a remote group").

The fan-out write needs no identity: it holds ROW EXCLUSIVE on the parent of every node
it has written, and the second move's attach on its target needs SHARE ROW EXCLUSIVE
there, so a back-and-forth cannot complete under the statement; the placement is compared
at the end as before. Tests: `t/031` (away and back inside the start of a scan, with a
third group that owns nothing so that no cursor of the scan holds a lock where the bucket
goes back; two moves of different buckets between the placement check and the cursors,
which uses the new fault point `scan_after_placement`; the second passes with the old code
too, the placement differing, and is kept as a guard of the sum).

**What a write into a partition does to the proof (S4-03).** The proof that the copy has
everything is a marker row committed on the source after the switch's locks are granted.
It covers every transaction that wrote the bucket before, provided the lock waits for
them. A write straight into the partition (a table owner can do it) takes ROW EXCLUSIVE
on the partition and, to check the partition constraint, ACCESS SHARE on the parent;
neither conflicts with SHARE on the parent. Observed: the switch queued at the detach (its
parent lock waited for the writer's ACCESS SHARE), the writer committed, the switch
committed and dropped the subscription at once, and the row, never replayed, was left in
the orphan table (`t/034`, with the replication held back by disabling the subscription
while the writer commits; confirmed by running the test against a build without the
partition locks: the row is lost).

Two measures, the second covering the gaps of the first:

1. The switch locks the partitions of the moving bucket in SHARE mode, with the parents,
   before it commits the marker. ROW EXCLUSIVE on the partition conflicts with SHARE, so
   every transaction that wrote the partition (through the parent or not, `COPY`
   included) has ended when the lock is granted, and none can start until the switch
   ends: a writer that arrives later waits for the lock, and when it gets it the
   partition has been renamed away, so its statement fails with "relation does not
   exist" instead of being acknowledged into the orphan. This is the barrier the reviewer
   asked for, placed before the marker instead of after the detach. The alternative, a
   second marker committed after the detach, does not work in this design: the attach on
   the target holds ACCESS EXCLUSIVE on the attached table until the switch commits, so
   the apply worker cannot write it and the second marker could not arrive before the
   commit. The barrier precedes the marker and the marker is replayed after everything
   committed before it, so "replayed up to the end of the switch" holds for every
   transaction that can ever have written the partition: nothing is committed on the
   partition between the marker and the commit.
2. The planner refuses a write that names a bucket partition (`planner.c`,
   `refuse_bucket_partition_write`: the result relation of the statement, or of a
   data-modifying `WITH`, is a partition of a hash-distributed table). It holds for
   superusers. It does not hold for a superuser who has switched
   `kingres_shard.enable_routing` off (to repeat a statement on every node by hand), for
   `COPY` into a partition, or for the replication workers of a move; the lock of measure
   1 covers all of them. An executor hook would close the first gaps too, at the price of
   a hook on every statement for cases the lock already covers; not done.

**Queueing for locks (S4-04).** The switch took its locks by waiting for them with a lock
timeout. While a request waits, every later request that conflicts with it waits behind
it (PostgreSQL grants in queue order): the SHARE request of the source stalled every new
writer of any bucket of the group's tables, and the ACCESS EXCLUSIVE request of the
detach every new reader. A transaction that held a table open (a long writer of another
bucket, a report) made every attempt queue for the whole lock timeout (one tenth of
`move_switch_timeout`, one second by default), and the attempts had no end. Reproduced by
the reviewer (25 of 120 inserts delayed, 12 s lost in 30 s) and by `t/035` against a
build that queues again: 29 and 27 samples of the lock table showed a waiting request.

Decision: no request of the switch ever waits.

- `kingres.internal_try_lock(sql, hold)` runs a `LOCK TABLE ... NOWAIT` in a
  subtransaction and returns false when a lock is not available; the subtransaction gives
  back what it had got. The C code retries for at most `lock_ms` (a tenth of the switch
  timeout, at least 200 ms) with a growing pause, and raises `lock_not_available` when the
  budget is spent, naming the sessions that hold a conflicting lock (asked from a session
  of its own: `pg_locks` of the node; the switch's own pid is left out).
- The attempt begins with a **probe**, holding nothing: is a transaction that has been open
  for longer than the attempt will try (`lock_ms`) holding a table of the move on the source
  or the target in a mode that conflicts with the lock the attempt needs (the exclusive one
  on the source, SHARE ROW EXCLUSIVE on the target)? Such a transaction will not be gone in
  time, and it is found before a single lock is taken, so the attempt costs the application
  nothing. A short statement is no reason to give up (a probe that asked for the locks with
  NOWAIT would fail whenever any reader overlaps the instant, which starved moves under a
  continuous scan load in `t/026`); the lock phases below get past it by trying for
  `lock_ms`. The locks are then taken in the
  old order (target, then source SHARE with the partitions). The ACCESS EXCLUSIVE locks of
  the detach are taken by try-locking, after the attach and before the `DETACH`
  statement, which then finds them held and does not queue. A failure after the probe
  holds the writers back for at most `lock_ms`.
- A failed attempt rolls back and the move backs off: 0.2 s, 0.4 s, ... up to 10 s, and
  fails after `kingres_shard.move_switch_attempts` attempts (default 20) with the last
  reason, which carries the pids, in `kingres.bucket_move.detail`; the bucket goes back to
  `active` and the move can be started again. Under a continuous, overlapping stream of
  writers on the source no instant may exist without a conflicting lock, and the move then
  cannot get its locks: it fails after its attempts instead of delaying the application
  for ever. A queued request would have drained the writers, at the price of stalling
  them; the trade is deliberate and stated in the README: the application comes first.
