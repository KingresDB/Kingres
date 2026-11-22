<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026 RooyeKhat Media Co -->

# Independent review: kingres_shard phase 10 (elasticity), extension part

Date: 2026-11-21. Reviewed tree: the combined phase 10 + 11 branch at a707948716 (phase 10 as written is
`0690871179..42c1dfef21`, merged by 2cbdfb90f4, plus 16d5975d26). The reviewer did not write the code.
Design: `kingres/docs/design/shard-phase10.md` (sections 3, 4, 7, 14, 15). Extension: `contrib/kingres_shard`.

The phase 10 control plane and panel work is not in this branch and is not reviewed. What the design expects of
them is noted at the end.

## Verdict

**Not ready to rely on.** The plan functions, the bucket layout, the weights and the upgrade are in good shape
(all regression and TAP tests of the phase pass on PostgreSQL 18 and 19, 0.8 equals 0.7 plus 0.7--0.8, the merge
resolution is correct). The split is not: one **critical** defect (a recovery decision that can leave a
bucket whose rows are unreachable and whose key can be inserted twice), two **high** defects (a split cannot
be written to by an ordinary role while it copies; a split cannot converge on a bucket that receives inserts
at random keys), and several medium ones. Every defect below marked "reproduced" was reproduced on a throw-away
cluster; the critical one on both majors.

## What was run and what was only read

Run (three-node clusters, own copies of the PostgreSQL 18 and 19 installations, own build of the sources at
a707948716, zero-warning build with -Werror on both majors):

- `kingres/dev/ext-majors.sh kingres_shard` steps sync, clean, build, install, regress: passed on 18 and 19.
- TAP `t/200` to `t/204` on 18 and on 19: all passed (441 and 443 subtests).
- Eight TAP scripts of my own (reproductions, load, timing) and one plan fuzz (400 random clusters through
  `internal_plan_simulate`), and eight SQL mutants of the plan, layout and drain code against the regression tests
  (one of them re-run against `t/201`).
- The generator `mk-install-script.sh` run on a copy: its output is byte-identical to the committed
  `kingres_shard--0.8.sql` (15201 lines).

Only read: the control-plane side of the design, the upgrade from 0.1 to 0.6 (older versions), the C code
outside the diff, the failover and standby paths of the split (no test of mine; see P10R-16), `by_disk_size`
sizes from remote nodes (read; `t/201` covers it and passes).

## Findings

| ID | Severity | Title | Reproduced |
|---|---|---|---|
| P10R-01 | critical | Recovery of a split whose session died during the switch can undo a switch that then commits: bucket map and partitions disagree, rows become unreachable after the next move, keys can be duplicated | yes (18 and 19) |
| P10R-02 | high | The capture trigger runs with the privileges of the writer: every write of an ordinary role into the splitting bucket fails for the whole copy | yes |
| P10R-03 | high | The copy cannot converge when the bucket receives inserts at random keys (the "skipped rows" count includes new rows); the split fails after the full copy | yes |
| P10R-04 | medium | A split switch that meets a deadlock is not retried; it fails and is undone, and readers get "deadlock detected" instead of KS001 | yes |
| P10R-05 | medium | Bucket numbers of 10000 and above: SQL names the partition with a truncated number, C with the full number; a split that makes such a bucket always fails | yes |
| P10R-06 | medium | The session-level advisory lock of a split is not released on an error after the switch or when the undo fails: the worker treats the split as alive | yes |
| P10R-07 | medium | `plan_rebalance` stops with a receiver far outside the threshold although a single improving move exists (weights) | yes |
| P10R-08 | medium | The switch is not bounded by `move_switch_timeout`: the verification runs under the writers' lock up to about six minutes per attempt, twenty attempts | read |
| P10R-09 | low | `by_disk_size` counts a distributed schema as the 1 MiB floor | read |
| P10R-10 | low | README says a role that can read `kingres.bucket_move` may plan; `plan_rebalance` and `group_load` fail for it | yes |
| P10R-11 | low | check_cluster time bound of the README (2 x T per node) does not hold: 5.6 T for one stopped node | yes |
| P10R-12 | low | `check_cluster()` reports `bucket_layout` during the normal gap between the owner's switch and the catalog change | yes (seen) |
| P10R-13 | low | `internal_split_undo` and the undo of a split wait for locks without a bound of their own | read |
| P10R-14 | low | Verification `full` does not make the children equal in content; `count` cannot see equal counts with different rows | read |
| P10R-15 | low | Departures from the design that the control plane will meet (separate record and functions, settings, fault points, names) | read |
| P10R-16 | low | Tests: promised cases that are missing, and a mutant the regression tests do not kill | yes |

### P10R-01 (critical): recovery undoes a switch that is still running

Files: `contrib/kingres_shard/kingres_shard--0.7--0.8.sql`, `resume_bucket_splits()` (line 2066, the decision at 2085),
`internal_split_undo()` (1651), `internal_split_switch()` (1566), `split_bucket()` (1835).

The split is two steps: the owning node switches the partitions in one transaction (`internal_split_switch`), then
the map group changes the bucket map (`internal_split_finish`). Between them, and while the switch is running, the
only record is `bucket_split.phase = 'switching'`. `resume_bucket_splits()` (the worker, or a manual call) decides
as follows when the session of the split is gone: ask the owner `internal_split_state` ("are the partitions
switched?"); if "no", run `internal_split_undo` and mark the split `failed`.

The owner's backend does not stop when the session that called it dies: it goes on with the switch (a lock wait, or
the row counts of the verification, which are proportional to the bucket). So the answer "not switched" can be true
when asked and false a moment later. The undo then waits for the switch's SHARE locks (its first statement is
`DROP TRIGGER IF EXISTS` on the old partition), the switch commits, and the undo finds nothing to drop: the children
were renamed to `<table>_bNNNN` and attached, so `DROP TABLE IF EXISTS <table>_s<id>_<r>` matches nothing. The split
is recorded `failed`, the bucket `active` with the old modulus, and the owner has the new partitions.

Reproduction (`rev_c`, PostgreSQL 18 and 19): a reader transaction holds ACCESS SHARE on the table on the owner,
`split_bucket(C, 6, 2)` reaches the detach and waits, its session is terminated, `resume_bucket_splits()` is called
(it blocks in the undo), the reader commits. Result:

- record: `failed | the session of the split ended before it switched`; bucket map: bucket 6, modulus 8, `active`;
- owner: `s1_b0006` is `FOR VALUES WITH (modulus 16, remainder 6)` and `s1_b0014` is `(modulus 16, remainder 14)`;
  bucket 14 has no row in the bucket map; `check_cluster()` says `bucket_layout`;
- the old partitions stay as `kingres.orphan_s<id>_N` for ever (no `cleaning` phase, nobody drops them);
- `kingres.move_bucket(C, 6, 1)` then **succeeds**: it copies only `s1_b0006` (slots 6 of 16) to the target, which
  attaches it as modulus 8. Slot 14 rows stay on the old owner in `s1_b0014`, the catalog sends them to the target:
  `SELECT count(*) FROM s1 WHERE id = 18` returns 0 while the old owner holds the row, and `INSERT` of the same key
  succeeds (a duplicate of the primary key on two nodes). Ids 18, 25 and 27 were used in the run; `count(*)` over the
  whole table still says 2000, so a full scan does not show it.

The window is as long as the switch runs: the lock waits (`max(200 ms, move_switch_timeout / 10)` each) and the
verification (P10R-08). The worker's default interval makes it realistic: a control-plane restart or a dropped
connection during the switch of a large bucket is enough.

Why the tests miss it: `t/204` section E kills the session at `split_after_backfill` and `split_after_switch`, never
inside the switch, and the switch function has no fault point inside it (the design lists
`split_switch_after_lock`, `_after_verify`, `_after_detach`, `_after_attach`).

Suggested fix: make the undo prove the state while it holds the switch's lock (take `SHARE` on the parents and old
partitions of every table first, with a bound, then ask `internal_split_state` again, and finish instead of undoing if
it says switched), or put the decision in a two-phase transaction as the design says, or make `internal_split_switch`
check on entry that its split is still `switching` and refuse otherwise. Add fault points inside the switch and a test
that kills the session there. A bucket whose partitions are not the map's must stop `move_bucket` (`internal_move_inspect`
should compare bounds with the map).

### P10R-02 (high): an ordinary role cannot write the bucket while it is copied

File: `kingres_shard--0.7--0.8.sql` line 1503 (`CREATE FUNCTION kingres.%I() ... LANGUAGE plpgsql SET search_path`) in
`internal_split_prepare()`; children created at 1491.

The capture trigger is not `SECURITY DEFINER` and the children are standalone tables owned by the role that runs the
split (`LIKE` copies no privileges). The trigger therefore inserts into the child with the privileges of whoever writes
the old partition. Reproduction (`rev_a`, X1): `GRANT ALL ON s1 TO app` on every node, `split_bucket(C, 5, 2)` stopped
after the backfill, then as `app`:

`ERROR: permission denied for table s1_s1_5` (context: `INSERT INTO public.s1_s1_5 ... ON CONFLICT (id) DO UPDATE`,
function `kingres.split_capture_1_1()`), on the owner and when routed from another node. The same holds for the owner of
the table. Only superusers can write a splitting bucket. The copy of a large bucket takes hours, so the keys of one bucket
are unwritable by the application for that long; no data is lost.

`t/204` writes only as the superuser (the only role it creates is the one that proves split_bucket needs a superuser).

Suggested fix: create the trigger function `SECURITY DEFINER` (owner: the superuser, `search_path` already pinned; the
body schema-qualifies everything) or grant the writing roles on the children; add a test with a role that has only
`INSERT, UPDATE, DELETE` on the table.

### P10R-03 (high): the copy does not converge under inserts at random keys

File: `kingres_shard--0.7--0.8.sql` lines 1556-1560 (`internal_split_batch`, the `v_skip` count) and 1940-1969 (the
passes in `split_bucket`).

After each batch the function runs a second statement, `count(*)` of the rows of the partition whose key is between the
cursor and the last key, and reports `skipped = count - n` (rows "held by writers"). That count has a new snapshot, so
every row inserted by a writer after the batch's snapshot with a key inside the batch's key range is counted as held. A
pass is accepted only when it copies nothing **and** skips nothing; ten passes fail the split ("writers keep rows ...
locked, the copy does not converge") after the whole copy, which then has to be undone.

Reproduction (`rev_g`): 200000 rows with even keys, three sessions inserting odd keys of the bucket in random order
(about 750 rows per second into the bucket), `split_bucket(C, 5, 2)` on a 25000-row bucket: it fails after 1.5 s
with that message and the record says `failed`; the writers saw no error and no row was locked. With keys that only
grow (a serial key) the inserts land behind the end and do not count, which is why `t/204` (keys `10000 * round + ...`)
passes. Random keys (UUID, hashes) are the common case for a hot bucket.

Suggested fix: count held rows from the locking statement itself (for example `FOR SHARE SKIP LOCKED` in one statement
and `FOR SHARE` NOWAIT probing, or count rows whose `xmax` is a live locker) or compare only rows that were visible at
the batch's snapshot; decide convergence on `inserted = 0` of a pass plus the verification under the lock, which the
switch already does.

### P10R-04 (medium): a deadlock fails the split and the readers

File: `kingres_shard--0.7--0.8.sql` lines 1995 and 2010 (only `lock timeout` is retried; anything else is re-raised),
against `move_bucket` line 575 (`'55P03', '40P01', 'KS004'` are retried).

With a foreign key between two colocated tables, the switch detaches the referencing table's partition first and then the
referenced one (1630-1636) while a reader that joins `s1` and `s2` locks them in query order. `rev_b` (three splits, six
readers on three nodes): one split ended with `ERROR: deadlock detected` raised at line 2010 (undone after its full copy),
and four reader statements got `deadlock detected` as well; the others got the retryable "bucket map changed" (KS001).
No wrong answer was seen. The design (section 14.1) promises KS001 as the only reader failure, and the README of
the phase says so for moves.

Suggested fix: treat SQLSTATE 40P01 like a lock timeout in the switch loop (the attempts exist for it); take the locks of
the switch in one fixed order that matches the planner's (parent tables in `oid` order) before the detach.

### P10R-05 (medium): bucket numbers of 10000 and above

Files: `kingres_shard--0.7.sql` line 1275 (`internal_bucket_part_name`), `kingres_shard--0.7--0.8.sql` lines 144 and 2698
(`kingres.buckets`), against `src/bucket_move.c` line 1569 and `src/dist_table.c` lines 265, 327, 335 (`"%s_b%04d"`).

The design (section 1.3) says `lpad(b::text, 4, '0')` "produces a longer name for numbers of 10000 and more". It does
not: `lpad` truncates. `lpad('12288', 4, '0')` is `1228`; the C code gives `_b12288`. A split that makes a bucket of
that number renames the child with the SQL name and attaches it with the C name: `rev_h` (a group of 4 buckets, splits of
bucket 0 and 60 in 16 parts, then bucket 1020 in 16 parts) failed with
`relation "public.s1_b10236" does not exist` after the full copy and was undone cleanly. So every split that would make
a remainder of 10000 or more fails, although `max_bucket_modulus` is documented as 65536, and the failure message does
not say why. In a group of 4096 buckets a split in 4 parts is enough. If the rename ever succeeded the name could also
collide with the real partition of bucket 1228. `kingres.buckets` (used by `plan_inputs` and `move_progress` for sizes)
builds the same truncated name.

Suggested fix: one function for the name that never truncates (pad with zeros up to four digits and print longer numbers in full, for
example `CASE WHEN b < 10000 THEN lpad(b::text, 4, '0') ELSE b::text END`), used by the SQL functions, the views and the C code; or refuse the
split with a clear message when a remainder would reach 10000. Add a regression test for a layout with a bucket of 10000 and more.

### P10R-06 (medium): the live mark of a split is not released on a failure

File: `kingres_shard--0.7--0.8.sql` lines 1917 (`internal_split_lock`), 2021-2024 (`RAISE` for `v_stage >= 3` with no
unlock), 2026-2030 (undo failure swallowed, then `RAISE` with no unlock), 2033 (the only unlock). `move_bucket` calls
`internal_move_unlock` before its `RAISE`.

`rev_a` X2: an error at `split_after_switch` (any failure of `internal_split_finish`, for example a node that does
not take the catalog) with the session kept open: `bucket_split.phase` stays `switching`, the owner has the new
partitions, `check_cluster()` says `bucket_layout`, and `resume_bucket_splits()` returns 0 because the advisory lock
makes the split look alive. It resolves (returns 1, phase `cleaning`) only when the session ends. A pooled or long
control session keeps the split unresolved for as long as it lives. Each failed split in a session also leaks one
advisory lock.

Suggested fix: unlock on every path out of the function (as `move_bucket` does), including the error path.

### P10R-07 (medium): the plan gives up early when weights differ

File: `kingres_shard--0.7--0.8.sql`, `internal_plan_simulate`, target choice (line 1129) and bucket choice (1146-1165):
the target is always the least utilized receiver, and a source with no bucket that improves that one pair is dropped
for the rest of the plan (`g_cand[s] := false`).

Reproduction (SQL, no cluster needed): groups 0..3 with weights 1, 1, 3, 1 and 1, 1, 3, 2 buckets (7 buckets, one
colocation group). `group_load()` shows group 3 at utilization 2.0 against an ideal of 1.167 (deviation 0.71);
`plan_rebalance('by_bucket_count', 0)` returns **no move**. Moving one bucket from group 3 to group 2 (weight 3) is
strictly improving (utilizations 1, 1, 1.33, 1; worst deviation 0.14), but the least utilized receiver is group 0 and
that pair does not improve. The fuzz (400 random clusters, weights 0.5 to 3, both strategies) found 16 plans that end
with a receiver outside the threshold while an improving single move existed (the other invariants held: no bucket
twice, sources and targets valid, drain complete, termination). With equal weights the algorithm equals the phase 4
plan, as the regression test shows.

The control plane will report "balanced" for such a cluster. Suggested fix: when no bucket improves the pair with the
least utilized target, try the other receivers in order of utilization before dropping the source.

### P10R-08 (medium): the switch of a split is not bounded by `move_switch_timeout`

File: `kingres_shard--0.7--0.8.sql` line 1980: the switch is called with the timeout `v_sw_ms + v_node_ms * 6`.

Design 3.3 says writers wait "at most `move_switch_timeout` per attempt". The SHARE locks are taken with a bound
(`max(200 ms, move_switch_timeout / 10)`) but the verification (`count(*)` of the old partition and the children, and
under `full` a pass of deletes and inserts), the detach and the attach (which scans a referencing table's rows against
its foreign key) run under them with no bound but the statement timeout: 10 s + 6 x 60 s = 370 s by default. For a big bucket
writers of every bucket of the table wait that long per attempt, up to twenty attempts. Not reproduced (it needs a bucket
that takes minutes to count). Suggested fix: set `statement_timeout` inside the switch function to
`move_switch_timeout` after the locks are taken, as the move does, and measure the verification as the design asks.

### P10R-09 (low): a distributed schema counts as the floor in `by_disk_size`

`internal_plan_inputs` (line 890) takes sizes from `kingres.buckets.partition_size`. The phase 11 version of that view
(line 2681) returns NULL for a table of method `schema`, so the bucket of a schema group costs `min_bucket_cost` (1 MiB)
however large the schema is. A group that holds a 500 GB schema looks as light as an empty one, and a by-size rebalance
fills it. Not reproduced. Fix: read `pg_total_relation_size` of the schema's tables (`kingres.schemas` has the size) into
the size of the bucket.

### P10R-10 (low): who may plan

README line 2472 says `plan_rebalance` may be run by a superuser or "a role that may read `kingres.bucket_move`". With
`GRANT SELECT ON kingres.bucket_move` and `USAGE ON SCHEMA kingres`, `plan_rebalance()` fails with permission denied for
`internal_plan_inputs` (which is revoked from PUBLIC and is not SECURITY DEFINER); `drain_status()` works. `t/201` F only
tests a role without the grant. Fix the sentence, or make the plan functions SECURITY DEFINER with the superuser
check for `by_disk_size`.

### P10R-11 (low): the documented time bound of check_cluster

README "Bounds that apply to every call" (line 3189): at most 2 x T for each node that does not answer. Measured
(`kingres_shard.node_timeout = 3`, postmaster of the node stopped): 16.9 s with one stopped node and 31.7 s with two
(documented: 6 s and 12 s). Per stopped node: 10.05 s in the base check and 5.0 s in `internal_check_table_operations`,
which does not use the skip list. The phase 10 additions are correct: `internal_check_layout_partitions` and
`internal_check_splits` cost 5.0 s per node when asked, and the commit 16d5975d26 makes `check_cluster` skip the nodes the
base check found unreachable, so they add nothing. The slack comes from the older code (the 2 T claim was not true
before), but the table still states it and `t/086` does not check the number. Fix the text or the code.

### P10R-12 (low): a false `bucket_layout` between the two steps

Between the commit of the owner's switch and `internal_split_finish` (normally milliseconds, longer with P10R-06 or an
unreachable node: `finish` calls `sync_catalog`, which fails while any node is down, and the split stays `switching`
until it can), `check_cluster()` says `bucket_layout` (partition bounds differ from the map). A control plane that
requires a clean `check_cluster()` before it moves will refuse for that time, and a human will read it as damage. Suggest
that the check skips a colocation group with an unfinished split.

### P10R-13 (low): waits of the undo

`internal_split_undo` (1651-1676) takes SHARE ROW EXCLUSIVE and ACCESS EXCLUSIVE locks (`DROP TRIGGER`, `DROP TABLE`) on the
old partition with no `lock_timeout`; the only bound is the caller's statement timeout (`6 x node_timeout`). While it waits
behind a long transaction every new writer of that partition queues behind it. The prepare sets a lock timeout; the undo
should too, and retry.

### P10R-14 (low): what the verification proves

`verify = 'count'` compares counts only (the design says so). `verify = 'full'` (not in the design) is documented as
"bring the children in line key by key, both ways", but its repair is `INSERT ... ON CONFLICT DO NOTHING`
(`internal_split_switch`, 1612-1621): a child row that exists with other values is kept. The capture trigger upserts, so
no path was found that creates such a row, but `full` is then weaker than its description. Use `DO UPDATE` or say so.

### P10R-15 (low): departures from the design that the control plane will meet

The README of the extension lists most of them; collected here because section 15 of the design builds on the other form:

- the record is `kingres.bucket_split`, not `bucket_move.kind = 'split'`; `resume_bucket_moves()`, `cancel_bucket_move()`
  and `cleanup_moved_buckets()` do not handle splits (`resume_bucket_splits()`, `cancel_bucket_split()`,
  `cleanup_bucket_splits()` do; the recovery worker calls both sets). The control plane that "calls
  `resume_bucket_moves()` when it finds a move whose connection it lost" will not settle a split.
- the settings `split_batch_rows` (5000), `split_switch_attempts` (the move's), `max_bucket_modulus` (65536) and
  `split_converge_passes` (10) are constants in the function.
- the `check_cluster` names are `split_stalled` (design: `operation_stalled`) and `extension_version_differs`
  (design: `extension_version`).
- the epoch of a split is raised in `internal_split_finish`, after the switch, and not in the transaction that changes
  the partitions (design 7.1); the partition-identity check of scans covers the gap (children are new tables).
- no fault point exists inside the switch or the undo (design 14.1 lists eight).

### P10R-16 (low): tests

Against the list in design 14.1, `t/204` does not have: two colocated tables with a foreign key (the one
split with a foreign key that I ran, in `rev_b`, worked); a long reader or writer that holds the switch back until the
attempts run out with the blocker named; scans, fan-out writes and routed statements in a loop through the switch (no wrong
answer, only KS001): `t/204` has no reader at all during a switch; a writer that takes rows in the opposite order, a delete
exactly between the lock and the commit of a batch, a row locked by a long transaction; a non-superuser writer (P10R-02);
random-key inserts (P10R-03); a mismatched node version for `split_bucket`; the S4-01 sequences with a split; failover
of the owning group; a kill inside the switch (P10R-01); a bucket number of 10000 (P10R-05). Mutants: of eight mutants of
the plan, layout, drain and weight code, six were killed by the regression tests; the removal of the `min_bucket_cost`
floor survived them and was killed by `t/201` (so the floor is tested by TAP only); the removal of the "arriving"
condition of `drain_status().drained` survived the regression tests (I did not run a TAP test against it: `t/202` and
`t/203` do not read `drain_status`, and the only other reader is `t/236`, a row count).

## Checks that found no defect

- **0.8 install script and the upgrade.** The generator's output equals the committed `kingres_shard--0.8.sql`. The `upgrade`
  test compares columns (with position), constraints, indexes, function bodies by digest, owners, ACLs, triggers,
  views, comments and members between a fresh 0.8 and each older path and passes. `ALTER TABLE ... DISABLE TRIGGER` around
  the two backfill UPDATEs is restored. The layout defaults trigger covers rows of a 0.7 catalog document
  (`max_modulus`, `modulus`, `weight`, and `kind` from phase 11); the check constraints on `bucket_map.state` and
  `modulus` hold.
- **Merge resolution.** `internal_check_node_versions` asks for 0.8 and keeps the unclaimed fallback; `kingres.buckets`
  is defined twice (P10, then P11) and the second keeps `modulus` as the last column; `internal_split_allowed` is a stub in
  P10 and the P11 definition replaces it (refuses a schema group); `rebalance_plan` and `internal_plan_inputs` exclude
  schema groups from moving and count them in the load; the catalog snapshot carries `max_modulus`, `placement_epoch`,
  `modulus`, `weight` through `to_jsonb`, and `internal_apply_catalog` inserts them by name; `check_cluster` is defined once
  and calls the layout, split, table-operation and partition checks, skipping nodes the base check found unreachable.
- **Layout and routing.** `kingres_shard_coloc_add` rejects a modulus that does not divide `max_modulus`, a remainder at
  or above the modulus, and overlapping slots; a gap is an error that names the slot. Slot lookup, placement hash with the
  epoch (`kingres_shard_placement_hash` and `_now` are equal sums), `copy_routed`, `create_distributed_table` and
  `internal_new_colocation` follow the layout. A move of a split child and of a bucket during another bucket's split ran
  clean in `rev_e` (in both orders).
- **Concurrent moves and splits.** One split per colocation group is held by the partial unique index and the explicit check;
  a split of one bucket with a move of another bucket and the reverse leave the data and `check_cluster()` clean; a bucket
  in `moving` or `splitting` is refused by the other operation; DDL and `create_distributed_table` on a group with a split
  are refused.
- **SQL injection and search_path.** Every dynamic statement of the split uses `%I` and `%L` or an integer; the
  functions pin `search_path`; `internal_ddl` is superuser only and revoked from PUBLIC; the split and plan helpers are
  revoked from PUBLIC. `set_node_property` (3 arguments) has no superuser check of its own, exactly like the 2-argument
  function it replaces; it depends on the revoked EXECUTE.
- **Readers through splits.** In `rev_b` (six readers, four splits with a foreign key) no reader got a wrong answer; the
  failures were KS001 and the deadlocks of P10R-04.
- **Plan.** The fuzz found no duplicate bucket, no move from a group that does not own the bucket, no move to a non
  receiver, no incomplete drain, and the loop always ended. The cost floor, `exclude_groups`, `extra_load`, the weight cap
  and the improvement test are killed by the regression tests or `t/201`.

## Table: what the design promises, and what was found

Design section 15 (implementation order) as far as the extension goes.

| Design item | In this branch | Finding |
|---|---|---|
| 0. Prerequisites S4-01 to S4-05 (epoch, worker calls, partition locks, bounded attempts, version check) | present before P10; the epoch of a move is raised in `internal_switch_bucket`, the worker calls both sets of functions, `internal_check_node_versions` asks 0.8 | the epoch of a split comes after the switch (P10R-15); no S4-01 test with a split (P10R-16) |
| 1. Six rebalance operations (control plane) | not in this branch | not reviewed |
| 2. `plan_rebalance`, `group_load`, `set_node_property`, weights, `by_disk_size`, threshold, wrapper | P10.3 | stops early with weights (P10R-07); schema floor (P10R-09); who may plan (P10R-10) |
| 2. `rebalance_plan` wrapper over `plan_rebalance` | not a wrapper: the old function is kept as it was, with `bucket_count` replaced by the bucket count of the map; a regression test compares both over generated maps | none (documented) |
| 3. Drain | `drain_status()` (P10.4); the drain itself is control plane | `drained` ignores moves in `cleaning` (the old copy is still on the group); the "arriving" condition is not tested (P10R-16) |
| 4. Automatic mode | not in this branch | not reviewed |
| 5. Split, extension: catalog (`modulus`, `max_modulus`, `placement_epoch`), audit of `bucket_count`, `split_bucket`, capture triggers, backfill, switch, undo, `check_cluster` rows | P10.1, P10.5 | P10R-01 to P10R-08, P10R-12 to P10R-15 |
| 5. Gate "all of 14.1, then the comparison harness of phase 2a run with split groups" | `t/204` only; no harness run | P10R-16 |
| 6. Split, control plane and contract | not in this branch | not reviewed |
| 7. Console | not in this branch | not reviewed |
| 8. Documents (README, ADR) | README sections written for P10.1, P10.3, P10.4, P10.5 | wrong sentence on who may plan (P10R-10); time bound (P10R-11); `lpad` claim in the design (P10R-05) |

Design section 14.1 tests (extension).

| Promised test | Present | Notes |
|---|---|---|
| `plan_rebalance`: examples of 4.7, weights, drain, threshold 0, `extra_load`, `exclude_groups`, group without primary, moving/splitting buckets, ordering, termination, equality with `rebalance_plan` | `sql/plan_strategies.sql` | strong; mutants killed; the unequal-weight stuck case is not covered (P10R-07) |
| Layout: `bucket_for` over a range, `bucket_range`, layout check rejects overlap and gap | `sql/bucket_layout.sql`, `t/200` | none; no layout with a bucket of 10000 or more (P10R-05) |
| Upgrade 0.6 to 0.7 against fresh | `sql/upgrade.sql` (0.7 to 0.8 and older) | passes; strong |
| Split with two tables and a foreign key; second split of a child; split in 4; layout check | A of `t/204` (no foreign key) | no foreign key in `t/204` |
| Writers during a split (model comparison) | B of `t/204` | inserts only at growing keys; superuser only (P10R-02, P10R-03) |
| Interleavings against the backfill (delete between lock and commit, update during a batch, long-held row, opposite order, deadlock) | none directly | the writers in B are a load, not interleavings |
| Bypass: direct write, `session_replication_role = replica`, COPY | C of `t/204` | present and sound |
| Switch against statements; long reader and writer; attempt bound names the blocker | none in `t/204` | `t/035` covers the move |
| S4-01 sequences with a split | none | P10R-16 |
| Crash and loss: kill in each phase; owner, map group, third node; failover with `promote_node`; worker alone | E of `t/204`: kill before the switch and after it | nothing inside the switch (P10R-01); no failover test |
| Direct write across a move's switch with a lagging subscription (S4-03) | `t/034` | older |
| Moves and splits together | D and F of `t/204`, `t/200` D | `rev_e` confirmed both orders |
| Preconditions: no primary key, user-partitioned, bucket not active, second split of a group, DDL, parts, a mismatched version | D of `t/204` | no mismatched version for a split |
| Fault points at every phase boundary | `split_after_preparing`, `_after_triggers`, `_mid_backfill`, `_after_backfill`, `_before_switch`, `_after_switch`, `_after_local` | none inside the switch or the undo |

## What the design expects of the part that is not in this branch

Nothing here was reviewed. For the control plane: it must not rely on `resume_bucket_moves()` for splits (P10R-15), it
reads `check_cluster()` as a safety rule (P10R-12 gives false findings in a normal gap and P10R-06 a long one), it
should read `plan_rebalance` as a guide that may stop early with weights (P10R-07), it should add the node's `weight` per
server as the design says (the column and the three-argument function exist), and it should not start a split without
the checks of 3.1 that the extension cannot make: free disk of the owner and its standbys for the children, the doubled
write volume while the copy runs, and a role that the application writes with (P10R-02 until fixed).
