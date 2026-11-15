<!-- SPDX-License-Identifier: AGPL-3.0-only -->
# Adversarial review: contrib/kingres_shard 0.3 (moving buckets, rebalance plan, cluster check)

Reviewer: independent (not the author). Reviewed state: commit 1d4b815f2d
("kingres_shard 0.3: moving buckets between shard groups, rebalance plan,
cluster check"), exported with `git archive` into a private directory; the
working tree, which another engineer was editing during the review, was not
used for any result. Line numbers below are those of that commit. The eighteen
findings S3-01 to S3-18 of the older review are not repeated.

## Scope and method

Read: `src/bucket_move.c` completely; the diffs of 0.3 in `dist_scan.c`,
`fanout.c`, `remote_scan.c`, `utility.c`, `catalog.c`, `node_admin.c`,
`node_conn.c`, `recovery.c`, `kingres_shard.c`, `dist_table.c`; all new SQL in
`kingres_shard--0.2--0.3.sql` (tables, the C wrappers, `move_bucket` and every
`internal_*` helper, `resume_bucket_moves`, `cancel_bucket_move`,
`cleanup_moved_buckets`, `check_cluster`, `rebalance_plan`,
`set_node_property`, the grants); `README.md` (move chapter, limits),
`shard-phase4.md` including its implementation notes, and the fault points in
`fault.c`. I did not read tests 001-024 line by line, nor `KingresShardTest.pm`
beyond its helpers; 025-032 I read for what they cover (their fault lists) and
ran.

Built: the whole tree out of tree under WSL2 (`debugoptimized`, `cassert`,
TAP enabled) and ran the whole extension suite once (`meson test --suite
setup --suite kingres_shard`, 36 tests, 6 in parallel on a machine whose load
average was 20-25 because of other work): 35 passed, `t/027_move_under_load.pl`
failed (S4-08); run alone afterwards it passed (1 of 1). The pg_regress
`upgrade` and `catalog` tests passed, so 0.2 -> 0.3 gives the same objects as a
fresh 0.3 (the one place I compared myself is in S4-05).

Ran by hand (four private three-node clusters on one machine, one shard group
per node, `wal_level = logical`, fault points from `fault.c`, all stopped and
deleted afterwards). The role name of my sessions equals the extension schema
name, so my test tables were created in schema `kingres`; I do not believe it
changes any result below but I did not repeat them in `public`.

| Experiment | Runs |
|---|---|
| Scan overlapped by two moves of one bucket (S4-01) | 2, wrong answer 2 of 2 |
| Direct write to a bucket partition during the switch (S4-03) | 2 (no loss without lag, loss with lag) |
| Writer stall by an unrelated long writer (S4-04) | 2 (first one blocked earlier by the slot, see text) |
| Reader stall by a long reader (S4-04) | 1 |
| Recovery worker at 2 s with a finished and a killed move (S4-02) | 1 (+ one `resume_bucket_moves()`) |
| Version mix 0.2 SQL / 0.3 library on one to three nodes (S4-05) | 1 |
| DDL of 12 kinds from each of 3 nodes during `copying` (36 statements) | 1 |
| Privilege probe of 14 functions/tables as an unprivileged role | 1 |
| Table with array, jsonb, tsvector, bytea, numeric, generated column, 96 kB TOAST values, gin/expression/partial/covering/unique indexes, quoted/unicode/dotted schema and table names, FK between colocated tables: move, cleanup, move back | 5 |
| Prepared statements (generic plans) on a third node across a move | 1 |

Not covered (honestly): COPY in either direction during a move; user-level
two-phase transactions across the switch; long REPEATABLE READ/SERIALIZABLE
transactions; cursors fetched across the switch; INSERT ... SELECT; crash of
each participant at each step (I relied on 028/031 for that and looked only for
what they do not list); two moves running at once beyond what 025-027 do;
`rebalance_plan` with a group whose primary is inactive; mixed binaries (I had
one binary only); moves of very large buckets; disk exhaustion by a leaked slot
(I only observed the slot staying); the "argued, not forced" case of the commit
message (see "Unconfirmed").

"Reproduced" below means I observed it; "by reading only" means I did not run it.

## Findings

| ID | Severity | Summary |
|---|---|---|
| S4-01 | critical | Placement hash cannot see a bucket that moved away and back: a distributed scan returns too few rows, with no error (reproduced 2 of 2) |
| S4-02 | high | The recovery worker never calls `resume_bucket_moves()` or `cleanup_moved_buckets()`, contrary to README and design; an interrupted move keeps its slot, its `moving` bucket and its DDL block for ever; `check_cluster()` does not report it (reproduced) |
| S4-03 | high | A write straight into a bucket partition is not covered by the marker proof: an acknowledged row is lost if the replay lags the end of the switch (reproduced with the lag forced) |
| S4-04 | medium | A switch that cannot get its locks retries without limit, and every attempt blocks all writers (and, at the detach, all new readers) of the group's tables on the source node for up to a second; a long transaction on any bucket of those tables causes it (reproduced) |
| S4-05 | medium | No version handshake: with 0.3 binaries and 0.2 SQL on any node every distributed scan and fan-out write of the cluster fails with "function does not exist"; `move_bucket` checks the version of no node (reproduced; second half by reading) |
| S4-06 | low | `internal_placement_hash` and `internal_map_version` are executable by every role, and the first one invalidates a relcache entry of any relation the caller names (reproduced) |
| S4-07 | low | `kingres.bucket_move` is readable by every role and stores error texts, node addresses and slot names (reproduced) |
| S4-08 | low | `t/027_move_under_load.pl` of this commit is racy: failed in a loaded full run |
| S4-09 | low | The session that runs a move holds its snapshot, and so the xmin horizon of the map group primary, for the whole move (reproduced) |
| S4-10 | note | Rows of a moved bucket stay readable in `kingres.orphan_*` for `move_cleanup_delay`; `check_cluster()` checks structure, never content |

### S4-01 (critical) The placement hash does not see a bucket that moves away and back during a scan's start

Where: `src/dist_scan.c` 902 (`origin_placement`), 948-1099 (the "before" call
`internal_placement_hash(id, parent)`, the DECLAREs, the "after" call and the
comparison), `src/catalog.c` `kingres_shard_placement_hash*` (the hash is a sum
of a mix of `(bucket, group)` pairs).

What happens: the rule of the commit message ("equal numbers before and after
the cursor's DECLARE mean no switch committed in between") is true only if
placements never come back to an earlier value. The hash is a function of the
placement alone. If a bucket moves S -> T and T -> S, both commits fall between
the "before" and "after" calls, and the cursor on S is declared while the
bucket is detached (it waited for the first switch's lock and was planned after
its commit), the second move re-attaches the bucket on S (its attach needs only
SHARE UPDATE EXCLUSIVE on the parent, which a cursor's ACCESS SHARE does not
conflict with), and the "after" calls then return the original placement. The
check passes and the scan silently returns every row except those of the bucket.
No node of the plan owns T in the original placement, so no other cursor reads
the bucket. Nothing is returned wrongly twice; rows are just missing, and there
is no error. The window is as long as the scan's DECLARE round trip plus any
wait for a lock; it needs two complete moves of the same bucket inside it, so
natural probability is small, but a scan that waits behind the switch's lock on
S is exactly a long window, and an automatic rebalancer (the stated consumer of
`rebalance_plan`) is a source of back-and-forth moves.

Reproduction (two runs, 2 of 2 wrong; default settings, three nodes, one group
each; the fault points are the existing ones). A table `t3` with
`bucket_count => 2` and 30 rows, bucket 0 on group S (14 rows), bucket 1 on
group O, third group T owning nothing in this colocation group (create the table
while `set_node_property(<node of T>, false)`, then set it back to true):

1. Session M (map group primary): `SET kingres_shard.fault_injection =
   'move_switch_after_detach:wait:<flag-a>'; SELECT kingres.move_bucket(2, 0, T);`
   (the move stops inside the switch while S holds ACCESS EXCLUSIVE; wait for
   `<flag-a>.reached`).
2. Session Q on node O: `SET kingres_shard.fault_injection =
   'scan_declared_g<S>:wait:<flag-b>'; SELECT count(*) FROM t3;` The scan asks S
   for the placement (original), sends DECLARE, which waits for S's lock.
3. `rm <flag-a>`: the first move commits everywhere; the DECLARE on S completes
   on the detached bucket; the scan stops at `scan_declared_g<S>` (wait for
   `<flag-b>.reached`).
4. On the map group primary: `SELECT kingres.cleanup_moved_buckets(true);
   SELECT kingres.move_bucket(2, 0, S);` (the bucket comes back).
5. `rm <flag-b>`: the scan continues, both placement checks equal the original.

Observed: `scan result|16`, while `SELECT count(*) FROM t3` on each of the three
nodes returns 30 (14 rows of bucket 0 missing). Second run: 16 again.

Fan-out writes are not affected by the same sequence: the statement holds ROW
EXCLUSIVE on the parent of every node it has written, which the second move's
SHARE ROW EXCLUSIVE on its target waits for (by reading). The hole is the
reader's lack of any lock on the target.

Suggested fix: make the compared number monotonic: add a counter column to
`bucket_map` (or `colocation`) that only the switch increments, and put it into
the hash and into the origin's cached value; then every switch changes the
number, whatever the placement. Add a test to 031 that does the sequence above
(it needs only the existing fault points). Alternatively take SHARE UPDATE
EXCLUSIVE conflict into account by having the cursor hold a lock that the
attach waits for, but the epoch is cheaper and keeps the cost the author
measured.

### S4-02 (high) The recovery worker does not resume or clean up moves; check_cluster() does not notice

Where: `src/recovery.c` 706-725 (`kingres_shard_recovery_worker_main` runs
`kingres_shard_recover_prepared()` and nothing else); `README.md` 650, 754 and
775-776 ("the recovery worker calls it", "does it every recovery_interval");
`shard-phase4.md` 107 and 114-115. `grep` finds no call of
`resume_bucket_moves` or `cleanup_moved_buckets` in any `.c` file. Every test
that needs them calls them by hand (028 `resume_all`, 025/030 explicit cleanup),
so no test could notice.

What happens: a move whose session died (connection lost, backend terminated,
map group primary restarted) stays `preparing`/`copying`/`catching_up`; its
logical slot, subscription and publication stay; the bucket stays `moving`; DDL
on the whole colocation group is refused ("a bucket of colocation group N is
being moved") and `remove_node` of both groups is refused; the bucket cannot be
moved again; the slot retains WAL on the source for as long as the target does
not consume it (if the target is down, without limit: the source's disk fills).
Finished moves stay `cleaning` and their orphan tables stay, so every node keeps
a full second copy of every moved bucket. `check_cluster()` is silent in both
states: a move row in progress counts as explaining its slot and its `moving`
bucket (`kingres_shard--0.2--0.3.sql` 1575 ff.) even when nobody runs it.
Documentation tells the operator the opposite.

Reproduction: cluster with `kingres_shard.recovery_interval = 2` and
`kingres_shard.move_cleanup_delay = 1`.
(a) `SELECT kingres.move_bucket(1, 0, 1);` then wait 25 s: `SELECT phase,
replication_dropped FROM kingres.bucket_move` gives `cleaning|t` and the source
still has `kingres.orphan_1_1`. (b) Run `SET
kingres_shard.fault_injection='move_after_subscription:wait:<flag>'; SELECT
kingres.move_bucket(1, 1, T)`, and when `<flag>.reached` exists run `SELECT
pg_terminate_backend(<that backend>)`. After 45 s: the move is `copying`, the
slot `ks_move_2` is `active = t` on the source, the bucket is `moving`,
`ALTER TABLE ... ADD COLUMN` fails with "is being moved", and
`SELECT * FROM kingres.check_cluster()` returns no row. One manual
`SELECT kingres.resume_bucket_moves()` undoes it (move `failed`, no slot,
publication or subscription left): the operator step works, the automation does
not exist.

Suggested fix: have the worker on the map group primary call
`resume_bucket_moves()` and `cleanup_moved_buckets()` after
`recover_prepared_transactions()` (in the same bounded session; both skip live
moves already), or correct the three documents and add the call to the
operator's checklist. Add to `check_cluster()` a row for a move in progress
whose live-lock (`internal_move_alive`) is free and whose `updated_at` is older
than a few `node_timeout`, and a test that kills a move session and waits for
the worker.

### S4-03 (high) The marker proof does not cover a write into a bucket partition

Where: `src/bucket_move.c` 895-960 (what the switch locks and proves),
`kingres_shard--0.2--0.3.sql` 634 (publication). The switch locks the *parents*
of the colocated tables in SHARE mode (908), commits a marker on the source
(917-923), waits until it is on the target (931), attaches, and only then
detaches (968), which needs ACCESS EXCLUSIVE on the partition. A transaction
that writes the partition directly (`INSERT INTO orders_b0002 ...`, a table owner
can; nothing in the planner hook forbids it, only DDL on partitions is refused,
`utility.c` 192-260) takes ROW EXCLUSIVE on the partition only; the SHARE lock on
the parent does not wait for it, so the marker is committed and seen on the
target while this transaction is still open. The detach then waits for it (up to
`lock_ms`, 1 s by default), the transaction commits, the detach and the whole
switch commit, and `internal_drop_move_replication` drops the subscription at
once. The row of that transaction is in the orphan table and on the target only
if the apply worker replayed it in the few milliseconds between its commit and
the drop. Replay is not ordered before the switch: no wait for it exists after
the marker.

Reproduction (the lag is forced, otherwise replication usually wins the race and
I saw the row arrive, 1 of 2): move bucket 2 of a 6-bucket table with
`SET kingres_shard.fault_injection = 'move_switch_after_attach:wait:<flag>'`;
when `<flag>.reached` exists: on the target `ALTER SUBSCRIPTION ks_move_<id>
DISABLE` (stands for an apply worker that is behind), then on the source
`BEGIN; INSERT INTO orders_b0002 VALUES (2000, 'direct'); SELECT pg_sleep(0.8);
COMMIT;` in the background, then `rm <flag>`. Observed: the insert returns
`INSERT 0 1`, the move completes, `SELECT count(*) FROM orders WHERE id = 2000`
returns 0 from all three nodes; `SELECT id FROM kingres.orphan_2_1` on the old
source returns 2000 and 2 (an acknowledged write lost to the orphan, which is
dropped ten minutes later).

Suggested fix: after the parent locks also lock every bucket partition of every
table of the move in SHARE mode (a write to a partition conflicts there), or
forbid DML on a bucket partition outside the extension's own sessions; and after
the detach, wait for a second marker (committed after the detach) before the
commit, or at least do not drop the subscription before a marker committed after
the last lock release has arrived. Add a test of 028's kind with a direct write.

### S4-04 (medium) The switch retries without limit and every attempt stalls writers, and at the detach readers

Where: `src/bucket_move.c` 884 (`lock_ms = Max(200, switch_ms / 10)`), 895-910
(locks), `kingres_shard--0.2--0.3.sql` 1243-1330 (`<<attempts>>` loop: no counter
limit, a fixed random pause of 0.2-0.7 s).

What happens: the switch queues for SHARE on the source's parents (and SHARE ROW
EXCLUSIVE on the target's) with a lock timeout of one tenth of the attempt, and
then queues for ACCESS EXCLUSIVE at the detach. A transaction that holds ROW
EXCLUSIVE on one of those parents *for any bucket* (a writer that wrote another
bucket, left open) makes the SHARE request time out; a transaction that merely
read the table (ACCESS SHARE, e.g. a report) makes the detach request time out.
Either way the attempt fails with `55P03`, is classified as retriable, and the
loop starts again, for ever. While a request is queued, every new writer
(SHARE request) or every new reader (ACCESS EXCLUSIVE request) of all tables of
the colocation group on that node queues behind it. The move never completes
while such a transaction exists, and the application is slowed all that time.
README says only that writers of the bucket wait and readers go on; the design
note concedes writers of all buckets wait, but not that readers stall or that
there is no bound.

Reproduction: (1) long writer: bucket on source group G; stop the move at
`move_after_catchup:wait:<flag>` with `SET kingres_shard.move_switch_timeout =
'5s'`; when reached, in another session on the source node `BEGIN; INSERT INTO
orders VALUES (<key of another bucket of G>, 'long'); SELECT pg_sleep(30);
COMMIT;`, then `rm <flag>`. A loop of 120 single-row INSERTs (any key) on the
source node: 25 of 120 took 0.36-0.54 s, 12 s of the 30 s lost; the move record
read `switch attempt 27 gave up: canceling statement due to lock timeout` and
completed only after the long transaction ended. With the default 10 s the stall
per attempt is 1 s. (2) long reader: same, the other session being `BEGIN;
SELECT count(*) FROM orders; SELECT pg_sleep(25); COMMIT;` on the source node:
16 of 60 `SELECT count(*)` took up to 1.03 s; the move finished after the reader.
(My first attempt at (1) started the long writer during the copy; the slot
creation then waited for it (documented), the move stayed in `copying` and the
writers were not affected.)

Suggested fix: bound the move (a number of attempts or a total time, then
`failed` with a clear reason and the pid of the blocker from `pg_locks`); do not
queue: acquire with `LOCK ... NOWAIT` in a short polling loop so that no
pending request blocks other sessions, and give the switch priority only after
the blockers are gone; back off exponentially; state in README that readers and
writers of all buckets are affected and that a long transaction prevents the
move.

### S4-05 (medium) No version handshake; mixed 0.2/0.3 clusters fail with an undefined function

Where: `src/dist_scan.c` 948 and `src/fanout.c` (calls of
`kingres.internal_placement_hash`), `move_bucket` (checks `internal_move_inspect`
on the source and target only).

Reproduced: a three-node cluster installed at 0.2 (`CREATE EXTENSION kingres_shard
VERSION '0.2'` on each node, library of 0.3 loaded). After `ALTER EXTENSION
kingres_shard UPDATE` on node 1 only: `SELECT count(*) FROM orders` fails on all
three nodes with `ERROR: function kingres.internal_placement_hash(integer,
regclass) does not exist`; so does every join with a reference table and every
cross-group `UPDATE` (`internal_placement_hash(integer)`); single-key reads and
inserts work. `move_bucket` fails with `function kingres.internal_move_inspect(...)
does not exist` (`CONTEXT: remote node ...`), `check_cluster` reports the 0.2
nodes as `node_unreachable`. After node 3, node 2 can scan (its peers are
upgraded) while the upgraded nodes cannot until node 2 is done. After all three
are upgraded everything works, and a move succeeds (1 of 1). So no data is
damaged, but a rolling upgrade is a cluster-wide read/fan-out-write outage from
the first library restart until the last `ALTER EXTENSION`, which the README
("`shared_preload_libraries` must hold the new library first") does not say, and
the error does not name the cause. An *old binary* on a third node during a
move (by reading only, I had no 0.2 binary) would run scans with the map-version
rule that the 0.3 design says can miss a bucket, because `move_bucket` verifies
nothing about nodes other than the two it uses.

Suggested fix: make the 0.3 library raise a specific error ("node X has
kingres_shard 0.2: run ALTER EXTENSION ... UPDATE") when the remote function is
missing, or skip the check when the remote lacks it and no move is in progress;
make `move_bucket` and `internal_begin_move` refuse unless every active node
reports extension version 0.3; document the upgrade order.

### S4-06 (low) Helper functions open to every role

`internal_placement_hash(int, regclass)` and `internal_map_version()` are not in
the `REVOKE` list (`kingres_shard--0.2--0.3.sql` 1799-1840; 1851-1870): an
unprivileged role calls both (reproduced: `3901490574131563253`, `12`). They need
to be callable by sessions of other nodes, which run as the same user, so the
grant is intended, but the first one also calls `RelationCacheInvalidateEntry()`
and `InvalidateCatalogSnapshot()` for any OID the caller passes (`bucket_move.c`
about 520-540): a role can force relcache rebuilds in its own backend
(performance only) and learn the placement of any colocation group (no table
privilege needed). Suggested: check that the caller is a node-to-node session or
the table's reader (`pg_class_aclcheck`), and ignore an OID that is not a
distributed table.

### S4-07 (low) `kingres.bucket_move` is readable by everyone

`GRANT SELECT ON kingres.bucket_move TO PUBLIC` (1842). The table holds
`detail` (the last error, which carries remote error text and, through
`CONTEXT`-less messages, node `host:port`), `slot_name` and `sub_oid`. Reproduced
as an ordinary role: all rows are returned. Suggested: grant to a monitoring
role, or expose a view with `phase` and a sanitized `detail`.

### S4-08 (low) Test 027 of this commit is racy

`t/027_move_under_load.pl` 194-199 starts the transaction that must hold the
switch back as soon as the phase `catching_up` is visible, which is before the
copy ends; under load the move had switched before the transaction began (the
transaction's UPDATE was routed to the target group) and tests 12 and 13 failed
in my full parallel run (`cleaning` where `catching_up|switching` was expected);
run alone it passed. The test cannot tell a real failure to wait for a long
transaction from this race, which weakens the evidence for "a long transaction
holds the switch back". (The working tree already has a different version of
the test; I did not look at it.)

### S4-09 (low) A move pins the xmin horizon of the map group primary

`move_bucket` is one PL/pgSQL call and runs in the snapshot of the calling
statement for the whole move (`backend_xmin` non-null in `pg_stat_activity`,
reproduced during `catching_up`). For a move of hours, vacuum on the map group
primary cannot remove dead rows younger than the call,. Suggested:
document, or run the long waits in the control sessions only and finish the
caller's statement early (return after recording the move, poll).

### S4-10 (note) Orphan data and what check_cluster compares

The old copy of a moved bucket stays in `kingres.orphan_<move>_<n>` (the
extension's schema) for `move_cleanup_delay` (10 minutes), with the table
owner's rights, and for ever when S4-02 applies; a deletion that must be
immediate (erasure) is not final until `cleanup_moved_buckets(true)` has run.
`check_cluster()` compares catalogs, partition names and leftovers; it never
compares row counts, indexes, constraints or replica identity of a partition
with the parent, so a copy that diverged would not show up; it is a structural
check and should be described so.

## Unconfirmed (with what would confirm)

- **The case "argued, not forced"** (a routed statement on a target that is not
  the map group primary, catalog cache and partition list refreshed apart): I
  did not force it and found no new argument against it. It needs a backend
  whose extension cache is reloaded after the target's commit is visible and
  before the invalidation of the parent is sent. To confirm: add a delay in
  `CommitTransaction` between `ProcArrayEndTransaction` and `AtEOXact_Inval`
  that a GUC switches on, run `COMMIT PREPARED` of the switch on the target with
  it, and read the moved bucket by key from a long-lived session on the target
  that was invalidated for another reason a moment before (a `set_node_property`
  call does that). I did not build such a patch.
- **Another source of S4-01-like skew:** `kingres_shard_attached_partitions_now`
  counts partitions only on the origin; a back-and-forth on a *remote* group is
  protected only by the hash. Same confirmation as S4-01 with the bucket moved on
  a different pair of groups.
- **Bucket with foreign keys: attach time.** The attach validates the foreign
  keys under the switch's locks (design notes). With 1.5 million rows in two
  colocated tables the switch still finished within `move_switch_timeout =
  600ms`, so I could not make validation exceed the timeout; a bucket of tens of
  millions of rows should. To confirm: that size and watch for repeated
  `statement timeout` attempts (they are classified as retriable and loop, S4-04).
- **`rebalance_plan` for a group without an active primary** would propose moves
  that `move_bucket` refuses ("has no active primary"); I could not make a group
  inactive without editing the catalog. To confirm: after a failover of phase 5.
- **Disk fill by a leaked slot** (S4-02): I saw the slot stay active with 60 kB
  retained; filling needs the target down and writes on the source. To confirm:
  stop the target after killing the move session and write to the source.
- **COPY, user two-phase transactions, long RR/SERIALIZABLE and cursors across
  the switch**: not run; by reading the switch waits for any transaction that
  holds a lock on the source parents and cannot commit past a reader's ACCESS
  SHARE, so I expect them to be protected on the source, with S4-01's gap for
  scans.
- **Subscription conninfo in logs:** `CREATE SUBSCRIPTION ... CONNECTION '...'`
  is executed from PL/pgSQL; an error in it prints the statement (with
  `node_conninfo` values such as `sslkey` paths, not secrets) in `CONTEXT`.
  Not checked in a server log.

## Examined and found sound

- DDL during a move: 12 statement kinds (ADD COLUMN, CREATE INDEX, TRUNCATE,
  create_distributed_table colocated, DROP TABLE, COMMENT, GRANT, RENAME, SET
  DEFAULT, and the unsupported ones) from each of the three nodes during
  `copying`: refused by `kingres_shard_assert_no_move` on the map group primary
  or by the existing "must run on the map group primary" rule; nothing slipped
  through (`utility.c` 1207, `dist_table.c` 404).
- Privileges: as an unprivileged role `move_bucket`, `resume_bucket_moves`,
  `cleanup_moved_buckets`, `check_cluster`, `set_node_property`,
  `internal_node_exec`, `internal_subscription_conninfo` and `move_marker` are
  refused; `rebalance_plan` runs and returns no sizes (superuser only). No new
  function is SECURITY DEFINER; every `search_path` ends in `pg_temp` and the
  dynamic SQL uses `%I`/`%L`/`quote_identifier` (read in full; no injection
  found). `set_node_property` on a non-map node fails after taking and releasing
  the lock only.
- The marker proof against writers that go through the parent: the SHARE lock on
  the parents (ROW EXCLUSIVE and prepared transactions conflict) is granted only
  after earlier writers end, and the marker is committed after the grant. Row
  locks (`FOR UPDATE`, ROW SHARE) conflict with the ACCESS EXCLUSIVE of the
  detach, so they hold it back. (The exception is S4-03.)
- Undo after a killed move (`resume_bucket_moves()`): subscription, slot,
  publication, markers and standalone tables removed, bucket `active`, no
  leftover on any of three nodes.
- Contents of moved buckets: arrays, jsonb, tsvector, bytea, numeric, a stored
  generated column (recomputed on the target), 96 kB TOAST values, gin,
  expression, partial, covering and unique indexes, a table named with quotes,
  dots, a semicolon and a unicode schema: count and an md5 over all rows equal
  from all nodes before and after the move; second move away after cleanup works;
  `check_cluster()` empty.
- A move with a prepared statement kept in a generic plan on a third node:
  executions before and after the move all answered, no persistent KS001.
- Upgrade 0.2 -> 0.3 object by object: the pg_regress `upgrade` test passes
  (the upgrade script is the install script's suffix).
- Overflow in bucket arithmetic: bucket counts are at most 4096; all
  millisecond products of `node_timeout` (at most 3600 s) fit in 32 bits.
- Concurrency of the move with itself: unique index on unfinished moves, the
  advisory "alive" mark and `FOR UPDATE SKIP LOCKED` in undo/cleaning behave as
  designed (read; 025-028 cover them).
- Tests 025 to 032 other than 027 all passed in my full run (031 in 459 s,
  282 subtests; 032 in 100 s).

