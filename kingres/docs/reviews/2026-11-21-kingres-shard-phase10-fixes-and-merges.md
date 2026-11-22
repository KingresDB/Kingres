<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026 RooyeKhat Media Co -->

# Independent review: the fixes for the phase 10 review (P10R-01 to P10R-16) and the merges into shard-p1011

Date: 2026-11-21. The reviewer did not write the code.

Reviewed: commit 772aeb9ffa (the fixes), the merges c942d1ff83 (p10-fix into shard-p1011) and 4a1f824bd1 (kingres-18 at
e312c39271, the phase 9 review fixes, into shard-p1011), and the repair 583f86652e (a block of `src/dist_table.c` that the
textual merge had duplicated). The tree under test is 583f86652e. The review it answers is the one of 2026-11-21 on phase 10
(findings P10R-01 to P10R-16); the design section is "21. The fixes for the review of phase 10" in
`kingres/docs/design/shard-phase10.md`.

## Verdict

**The fixes are sound and the merges are correct. No critical, high or medium finding.**

The critical defect (P10R-01) is closed: the fence between the switch and the undo holds in every interleaving I could build,
and eight of nine mutants of the protocol and of the other fixes were killed by `t/205` and `t/206` (the ninth is an equivalent
mutant, see below). The fifteen other findings are fixed as described, with tests that kill their mutants. The merges lost
nothing and duplicated nothing; the generated `kingres_shard--0.8.sql` is byte-identical to what `mk-install-script.sh` makes.
Three low findings (a stale sentence, a sizing limit that is not documented, a restriction of the feature) are new.

## What was run and what was only read

Run (own copies of the PostgreSQL 18 and 19 installations, own build from `git archive 583f86652e`; zero-warning builds with
`-Werror` of kingres_sketch, kingres_columnar, kingres_timepart and kingres_shard on both majors):

- `kingres/dev/ext.sh kingres_shard regress` on 18 and on 19: passed (it includes `upgrade`, `plan_strategies`, `bucket_layout`).
- TAP on PostgreSQL 18: `t/204` to `t/207` (one run), then `t/189` to `t/198`, `t/200` to `t/203`, `t/236`, `t/237` (16 files, 1335
  subtests): all passed.
- TAP on PostgreSQL 19: `t/204` to `t/207`, `t/193`, `t/197`, `t/198`, `t/200` to `t/203`: all passed.
- `mk-install-script.sh` run on a copy of the tree: the output (16187 lines) equals the committed `kingres_shard--0.8.sql`.
- Nine mutants of the fence protocol, the writers' rights and the retry code, applied to the installed 0.8 script of PostgreSQL 18
  and run against the named test (table below).
- One probe of my own (800000 rows, 4 buckets, `move_switch_timeout` 2 s, verify `count`, `none` and `full`): see P10F-02.
- Merge analysis with git: line-level comparison of what each side changed against the merged tree for every file under
  `contrib/kingres_shard`, a conflict-marker check, and a check of `meson.build`.

Only read: the control plane and the panel (not in this tree), the failover and standby paths of a split (no test of mine),
`t/204` beyond the parts that the diff touches, the plan fuzz of the first review (not re-run; `plan_strategies` and `t/201`
pass).

## The fixes, one by one

| ID | Severity then | Verdict | What I checked |
|---|---|---|---|
| P10R-01 | critical | **Fixed** | Read the whole protocol (below). `t/205` A (the coordinator killed at each of four fault points inside the switch, the recovery running meanwhile), B (the undo cut at each of three points, by the death of the owner's backend and of the calling session), F and G pass on 18 and 19. Mutants m1, m2, m3, m5 killed. |
| P10R-02 | high | **Fixed** | The trigger function is `SECURITY DEFINER`, `search_path = pg_catalog, pg_temp`, owned by the table owner; the children are owned by the table owner; the owner's `USAGE` on the schema is checked first. `t/206` A passes (the owner and an ordinary role with INSERT, UPDATE and DELETE only, from every node). Mutant m6 (plain trigger function) killed. The function runs as the owner, never as the writer or the superuser, so it gives no escalation. |
| P10R-03 | high | **Fixed** | The held-row count is made in the batch's own statement and the rows it did not see are looked at again with a new snapshot; rows inserted after the snapshot are outside the range. `t/206` B (random keys, steady rate) passes on 18 and 19. |
| P10R-04 | medium | **Fixed** | 40P01, 55P03 and the statement timeout are retried by SQLSTATE; the switch runs with `deadlock_timeout` 100 ms so that it is the victim. `t/206` D passes; mutant m7 (40P01 not retried) killed. |
| P10R-05 | medium | **Fixed** (as a limit) | `split_bucket` refuses a split whose new bucket numbers reach 10000, before anything is recorded, with a message; `t/205` E passes. The wrong claim about `lpad` in the design is corrected. The limit is a real restriction (P10F-03). |
| P10R-06 | medium | **Fixed** | The live mark is released on every way out, including an error after the switch; `t/205` C (session kept open, the recovery settles the split, no advisory lock leaked) passes. Mutant m8 killed. |
| P10R-07 | medium | **Fixed** | `internal_plan_simulate` tries the receivers in the order of utilization (`g_try`) before it gives a source up; the bucket choice and the tie rules are unchanged; the loop still ends (a bucket moves at most once). `plan_strategies` passes on 18 and 19 with the new case. |
| P10R-08 | medium | **Fixed** (see P10F-02) | One command: fence and locks, `SET LOCAL statement_timeout = move_switch_timeout`, verification, detach, attach. In a simple-query string of several statements the timeout is per statement, so the SET takes effect for the second half. `t/205` D (a verification that does not end is cut, tried again, the split undone) passes; mutant m9 (timeout reset to 0) killed. |
| P10R-09 | low | **Fixed** | `internal_plan_inputs` adds the size of the tables of a distributed schema as bucket 0 of its group; `t/201` G passes. |
| P10R-10 | low | **Fixed** (text) | The README now says superusers only; `t/201` F checks the refusal and that `drain_status()` still works for the role. |
| P10R-11 | low | **Fixed** | `internal_check_table_operations(p_skip int[])` skips the nodes that the base check found not answering; the README states the measured bounds. `t/207` passes (measured on 18: 3.0 s and 6.0 s for stuck sessions, 4.1 s and 8.1 s for a stopped postmaster, with one and two nodes). |
| P10R-12 | low | **Fixed** | `internal_check_layout_partitions` skips a colocation group with an unfinished split; `t/205` G passes. The effect is that real layout damage in a group with a running split is hidden until the split ends; a split that stalls is reported as `split_stalled`. Acceptable. |
| P10R-13 | low | **Fixed** | The undo sets a `lock_timeout` for its own locks (`max(200 ms, move_switch_timeout / 10)`); a lock not got fails the undo and the recovery retries; `t/205` B. |
| P10R-14 | low | **Fixed** (with a cost, P10F-02) | `full` repairs key by key, values included (`ON CONFLICT DO UPDATE`); `t/204` has a child row with other values. |
| P10R-15 | low | **Fixed** (documented) | README "What the control plane calls" and the design section 21 list the departures (separate record and functions, `split_stalled`, `extension_version_differs`). |
| P10R-16 | low | **Fixed** (mostly) | New `t/205`, `t/206`, `t/207`, the `drain_status` case, the P10R-09 and P10R-10 cases. Still missing from the design's list: S4-01 sequences with a split, failover of the owning group during a split, a mismatched node version for `split_bucket`. They are not new defects. |

### P10R-01 in detail: the lock protocol

The switch is sent to the owner as one command: `internal_split_switch_lock(...)`, then `SET LOCAL statement_timeout`, then
`internal_split_switch(...)`. All three run in one implicit transaction. The undo is `internal_split_undo(...)`, one transaction too.

- Both take `pg_advisory_xact_lock(hashtext('kingres_split_switch'), split_id)` as their first act, before they look at any
  partition. Because it is a transaction lock, the fence is held from the first statement to the commit or rollback of the switch;
  a switch and an undo of one split can never overlap.
- If the undo is second: it waits (`move_switch_timeout` plus a lock timeout), then asks `internal_split_state` under the fence.
  A committed switch is seen (the first table's bucket partition has the new modulus) and the undo answers `switched` without
  changing anything; both callers (`resume_bucket_splits()` and the error path of `split_bucket()`) then write the catalog. A
  switch that rolled back leaves the old partitions and the undo goes on. A timeout fails the undo; nothing was changed and the
  next recovery pass tries again.
- If the switch is second (the undo ran first): `internal_split_switch_lock` finds the children gone and raises, so the switch
  cannot commit after an undo; with verification `none` the rename would fail as well.
- A retried switch whose first answer was lost finds `switched` and returns at once.
- The old failure (the recovery asking first, without the fence) is gone: `resume_bucket_splits()` no longer asks, it calls the
  fenced undo directly. The unfenced `internal_split_state` that the coordinator calls after a failed switch call is only used to
  skip the undo; if it answers "not" while a late switch is still running, the coordinator either retries (the retry waits for the
  fence) or undoes (the undo waits for the fence). I found no interleaving that ends in a committed switch with a `failed` record.
  The twelve fault-point combinations of `t/205` A and B pass.
- `move_bucket()` compares the source's partition bounds with the bucket map (`internal_move_bounds`) and refuses with the
  difference; mutant m5 (the check made inert) is killed by `t/205` F.
- Fault points inside the switch (`split_switch_after_lock`, `_after_verify`, `_after_detach`, `_after_attach`) and inside the
  undo (`split_undo_after_lock`, `_after_triggers`, `_after_children`) exist and are used by the tests.

### Mutants

All were applied to the installed `kingres_shard--0.8.sql` of a private PostgreSQL 18 copy (restored after each run); the unmutated
script passes every named test.

| Mutant | Test | Result |
|---|---|---|
| m1: the undo does not take the fence | `t/205` | killed (the recovery undoes a switch that commits: "has settled the split", "the bucket has the children's modulus" and the row checks fail) |
| m2: the switch does not take the fence | `t/205` | killed |
| m3: the undo never answers `switched` | `t/205` | killed |
| m4: the switch does not check that its children exist | `t/205` | survived; equivalent: the verification count, or the rename, fails on a missing child anyway, only the message differs |
| m5: `move_bucket` does not check the bounds | `t/205` | killed |
| m6: the trigger function is not `SECURITY DEFINER` | `t/206` | killed |
| m7: a deadlock (40P01) is not retried | `t/206` | killed |
| m8: the live mark is not released after the switch | `t/205` | killed |
| m9: the statement timeout of the switch is reset to 0 | `t/205` | killed |

## The merges

### What the merges did

shard-p1011 already contained the phase 9 content (increments 9.5, 9.8 and 9.9) as the integration commit 0690871179, which has
the same extension sources as kingres-18's 8a9843c475. The merge 4a1f824bd1 therefore had to bring in only the changes between
8a9843c475 and e312c39271: the P9C and P9F fixes. For `contrib/kingres_shard` those are `README.md`,
`kingres_shard--0.6--0.7.sql`, `kingres_shard--0.7.sql`, `meson.build`, `src/copy_routed.c`, `src/utility.c`, `t/197` and `t/198`.
(The generated `kingres_shard--0.8.sql` follows from the 0.7 changes.)

### Checks and results

- **Per-file comparison.** For each of those files I compared the lines that the P9 delta adds and removes
  (8a9843c475 to e312c39271) with the lines that the merge added and removed against shard-p1011's own parent
  (6a9b701239 to 583f86652e). `kingres_shard--0.6--0.7.sql`, `kingres_shard--0.7.sql`, `meson.build`, `src/copy_routed.c` and
  `src/utility.c`: identical sets, nothing lost, nothing added. `README.md`: the delta is complete; the only extra lines are the
  "installs version 0.7" and chain sentences, see P10F-01.
- **`src/dist_table.c`.** The P9 delta does not touch this file. The textual merge 4a1f824bd1 added 37 lines (the block that
  creates the leaves of a user-partitioned table, a second time); 583f86652e removes exactly that block, and the file in 583f86652e
  is byte-identical to the one in 6a9b701239 (the parent on the shard-p1011 side). Correct.
- **`src/utility.c`, `src/copy_routed.c`.** Equal to the P9 delta applied to shard-p1011's version (same added and removed lines,
  no repeated hunk). Both build without warnings on 18 and 19.
- **`kingres_shard--0.7--0.8.sql` and the P9 functions.** The merge does not change this file. I checked that none of the twenty
  functions and views that the P9 delta changes in `kingres_shard--0.7.sql` is redefined in `kingres_shard--0.7--0.8.sql` (a later
  `CREATE OR REPLACE` would silently undo a fix on 0.8). There is none. `internal_group_busy` covers a bucket that is not
  `active` (so also `splitting`), so the P9 busy check and the P10 split exclude each other.
- **`kingres_shard--0.8.sql`.** Running `mk-install-script.sh` on a copy of the tree gives a file that `cmp` finds identical to the
  committed one (16187 lines). It carries the P9 delta once (the same 269 changed lines as `0.7.sql`).
- **`check_cluster`.** Defined once per layer: the 0.7 definitions (the rename to `internal_check_cluster_base` and the P9 wrapper)
  and a single 0.8 `CREATE OR REPLACE`, which calls `internal_check_layout_partitions`, `internal_check_splits`,
  `internal_check_table_operations(p_skip)` and `internal_check_partitions()` once each. `internal_check_partitions()` (changed by
  P9C) is still called. No function is defined twice. `check_cluster()` finds nothing in the TAP tests that assert it.
- **`meson.build`.** `t/` holds 213 files; the four helper modules (`KingresShardCompare.pm`, `KingresShardTest.pm`, `KsProxy.pm`,
  `KsSweep.pm`) are not tests, and every `t/*.pl` is listed exactly once: a comparison of the file list against the list in
  `meson.build` is empty, and there is no duplicate entry.
- **No conflict markers** in the merged tree (checked over the shard extension and the documents it touches).
- **Other trees** (kingres_columnar, kingres_timepart, verifier, packaging, docs, `.github`, top-level `README.md`): equal to
  kingres-18 at e312c39271, except for files that only shard-p1011 has changed (for example
  `contrib/kingres_ledger/src/kingres_ledger.c`).
- **Behaviour.** The merged tree builds and passes the P9 suites (`t/189` to `t/198`, among them `t/193`, the partitioned move,
  which goes through the new bounds check of a move) and the P10 suites, so the interplay of the P9 move code and the P10 checks is
  covered.

## New findings

| ID | Severity | File and line | Reproduced |
|---|---|---|---|
| P10F-01 | low | `contrib/kingres_shard/README.md` line 208 | yes (read of the tree) |
| P10F-02 | low | `contrib/kingres_shard/kingres_shard--0.7--0.8.sql`, `internal_split_switch`, the verification loop (about lines 1830 to 1862) | no |
| P10F-03 | low | `contrib/kingres_shard/kingres_shard--0.7--0.8.sql`, `split_bucket`, the 10000 check (about line 2171) | n/a (by design, tested) |

### P10F-01 (low): the README says that version 0.7 is installed

README line 208 says "`CREATE EXTENSION kingres_shard` installs version 0.7 (the `default_version` of the control file)" and the
next lines name the chain as ending in 0.7. The control file says `default_version = '0.8'` and the README itself says "This is
version 0.8" on line 11. The sentence said 0.6 on the shard-p1011 side and 0.7 on the kingres-18 side, so both were stale; the
merge took the kingres-18 text and the result is stale in the same way. Scenario: an operator who reads the Installation section
expects 0.7. Fix: write 0.8 and the chain 0.1 to 0.8 (the `upgrade` test already covers 0.7 to 0.8).

### P10F-02 (low): a bucket whose rows take longer to count than `move_switch_timeout` can never be split

The fix for P10R-08 bounds the whole second half of the switch by `move_switch_timeout` (default 10 s). The verification (the
`count(*)` of the old partition and of the children, twice at most) and, for `verify 'full'`, an upsert of every row of the bucket
run inside that bound. `DO UPDATE` rewrites every row even when its values are equal, so `full` also doubles the bucket's dead
tuples. A bucket whose verification takes longer than the limit makes every attempt fail with "did not finish in N attempts", and
the split is undone after the whole copy (which can take hours). The README does not say that the setting must be raised for a big
bucket or that `full` suits small buckets only. Scenario: a bucket of tens of gigabytes, default settings: the copy finishes, the
switch is cut at 10 s in each attempt, the split is undone.

Reproduction attempt: a bucket of 200000 rows with `move_switch_timeout` 2 s: `count`, `none` and `full` all finished (the
verification took well under 2 s), so I did not reproduce the failure; the cut itself is shown by `t/205` D with a fault point.
Suggested fix: document the sizing (the count of a bucket must fit in `move_switch_timeout`), skip equal rows in the `full` upsert
(`WHERE` the columns are `IS DISTINCT FROM` the excluded ones), and consider a pre-count outside the locks that refuses a split
whose verification cannot fit.

### P10F-03 (low): the limit of 9999 leaves little room to split a group of 4096 buckets

`split_bucket` refuses a split whose new remainders would reach 10000. A group of 4096 buckets can be split in 2 (remainders up to
8191), but a bucket of that result (modulus 8192) cannot be split again (remainders up to 16383), and a split in 4 or more is
refused for most buckets. The limit is in the README ("Limits") and tested (`t/205` E); it is the price of the naming of the
partitions (`lpad` cuts). The lasting fix is one naming function, used by the SQL and the C code, that prints longer numbers in
full. This is a restriction of the feature, not a defect of the fix.

## What I found no defect in

- The advisory lock key: `hashtext('kingres_split_switch')` with the split id (cast to int4) is distinct from the live-mark key
  `hashtext('kingres_split')`; the 32-bit cast of the split id is safe for any realistic number of splits.
- The coordinator's error path: an error with `v_stage` of 3 or more releases the live mark and re-raises, and the worker finishes
  the split from the owner's state; an error before that undoes, and an undo that answers `switched` finishes the split instead of
  recording it as failed.
- The capture trigger function: run as the owner, fixed `search_path`, revoked from PUBLIC; it cannot be used to run code as a
  superuser (a superuser who writes the table fires it as the table owner).
- `internal_plan_simulate`: the retry over the receivers cannot loop (the `g_try` array and the one-move-per-bucket rule bound it).
- The 0.8 install script against the upgrade path: the `upgrade` regression test passes on 18 and 19.
