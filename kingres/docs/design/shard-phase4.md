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
