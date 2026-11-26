<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
# Review of the second fix round: contrib/kingres_sketch and
# contrib/kingres_timepart (phase 12)

Reviewer: an independent reviewer. Reviewed state: commit f33498c9a5 for the
two directories (no later change: `git diff --stat f33498c9a5 HEAD --
contrib/kingres_sketch contrib/kingres_timepart` was empty). Exported with
`git archive` into a private work area; the working tree was not used.
Nothing in the repository was changed except this file.

## Scope and method

Read: the second review (`2026-11-15-phase12-extensions-fixes.md`), the
commit message, `kingres_timepart--0.1.sql` (tables, views, trigger,
event triggers, run functions, grants), both READMEs, and the memory
and `self_test()` parts of `kingres_sketch`.

Built and run on PostgreSQL 18.7 and 19rc2 (private copies of the
read-only installations, prefix fixed, `EXT_WERROR=1`, `-j2`), with
`kingres/dev/ext.sh <ext> sync clean build install regress tap`. Own
experiments ran on a private server per major (ports 59218 and 59219,
stopped at the end), as roles with only the rights the README gives.

| Experiment | Result |
|---|---|
| A role that owns nothing, and a role that owns another table, against the policy view: SELECT, UPDATE, DELETE, UPDATE moving a row to another table, UPDATE ... FROM, INSERT ... ON CONFLICT DO UPDATE (with and without RETURNING), MERGE (update, insert, delete), COPY both ways, TRUNCATE, CREATE RULE, CREATE TRIGGER, ALTER VIEW | no other owner's row read, changed, created or deleted; the base tables are "permission denied"; the view cannot be altered (18.7) |
| Barrier: a function in WHERE that reports each row, a function that divides by zero on a foreign row, EXPLAIN, `pg_stats` | no foreign row reached the function; the plan shows the view filter below the user's filter; `pg_stats` empty (18.7) |
| Identity: role membership with and without INHERIT, `SET ROLE`, `SET SESSION AUTHORIZATION`, a SECURITY DEFINER function of another role called from a superuser session | rows shown follow `current_user`; NOINHERIT member sees 0 rows, INHERIT member sees and may change the rows (18.7) |
| Table recreated under the same name by another role; a role renamed; ownership changed | see K12S-01, K12S-02 |
| `pg_dump` as superuser and as a role with only `pg_read_all_data`; of one schema, one table and schema `kingres`; restore of the second into a new database | see "What held" (18.7 and 19rc2) |
| `self_test()` on both majors, 7 session settings, 3 build optimisation settings | see K12R-03 in the table |
| `topn_add_agg` in a GROUP BY with `work_mem` 4 MB | see K12S-03 |

## Summary

| Severity | Count |
|---|---|
| Critical | 0 |
| High | 0 |
| Medium | 1 |
| Low | 3 |
| Note | 1 |

The new privilege boundary held against every direct attack I ran. The
findings are an information leak through the history views, a name-based
owner record, a memory figure that is correct per call but not per query, and
two documentation gaps.

## Fate of the second review's findings

| ID | Fate | Evidence |
|---|---|---|
| K12R-01 | fixed | `pg_dump` as a `pg_read_all_data` member exits 0 and its output equals the superuser's except the random `\restrict` token (18.7 and 19rc2); the restore as superuser gives the same policies, 7 actions, the sequence at 2 |
| K12R-02 | partly | the README figures are per call and match the test; a query with many groups multiplies them and `work_mem` has no effect (K12S-03); no limit exists in the extension |
| K12R-03 | fixed | `self_test()` is the same line on 18.7 and 19rc2 and under `extra_float_digits` -15 and 3, JIT on, `LATIN1` client encoding, `DateStyle`, `bytea_output`, `search_path`; it changed with `-O3 -march=native` (tdigest, tdigest_union) and with `-ffast-math` (tdigest_scale), unchanged at `-O0` |

## New findings

### K12S-01 (medium) The history of a dropped table is shown to whoever
### owns a table of the same name later

Where: `timepart__owns` / `timepart__visible` (the view condition) and the
event trigger `timepart__on_sql_drop`, which removes the policy but keeps the
history by design.

Steps (18.7 and 19rc2): `alice` owns partitioned `app.ta`, sets a policy,
a superuser runs `CALL kingres.timepart_run('app.ta', ...)` (run 1, seven
actions). `alice` drops `app.ta`. `bob` creates his own `app.ta`.

Observed: as `alice`, `select count(*) from kingres.timepart_run` is 0 (her
own history is gone from her view). As `bob`, `timepart_run` shows run 1
(state, `started_by`, error) and `timepart_run_action` shows `ta_p20261116`
and the other partition names and bounds of alice's table.

Expected: a role sees the history of tables it owned when the run was made,
or the history of a dropped table is visible only to superusers; a new table
with the same name is not the old table. The README says the history stays;
it does not say who sees it.

Direction: store the owner (or the table OID) in the run row at begin, and
show a row to the roles that own the table now and own the recorded owner.

### K12S-02 (low) The owner is recorded by name: renaming a role makes
### every policy of that role fail

Where: `table_owner name` in the policy, compared in `timepart_begin_run`
and `timepart_run_action`.

Steps (18.7 and 19rc2): `alice` sets a policy on `app.tc`;
`ALTER ROLE alice RENAME TO alice2`; `CALL kingres.timepart_run('app.tc',
...)`.

Observed: the run is recorded `failed`, error `42501: owner_changed: the
owner of app.tc is now "alice2", the policy was set for "alice"`; the policy
row still says `alice`. The same error ends every later run until the new
name sets the policy again.

Expected: a rename is not a change of owner; or the README says that a
rename needs the policy to be set again. `created_by` and `table_owner` also
name a role that no longer exists.

Direction: compare the owner by OID (store `table_owner_oid`) and show the
name from the catalog.

### K12S-03 (low) `topn` memory is per call in the README, per group in a
### query, and `work_mem` does not bound it

Where: `contrib/kingres_sketch/README.md`, section "topn" and the limits
table (K12R-02's documentation fix).

Steps (18.7): `set work_mem='4MB'; set max_parallel_workers_per_gather=0;
set enable_sort=off;` then `select count(*) from (select grp,
topn_add_agg(repeat('x',900)||i::text, 100000) from (select i, i%300 as grp
from generate_series(1,600000) i) s group by grp) q;` (plan: HashAggregate).

Observed: the backend peak (`VmHWM`) went from 23 MB (idle) to 659 MB. Each
group's state grows after the group is admitted, so the hash table's limit
does not stop it. The figure scales with groups times the retained items
(any role can make millions of rows with `generate_series`); with 100000
groups of 100000 distinct items the state is 100000 times the README's
single-value figure. Parallel workers multiply it again.

Expected: the README states that the figures are per aggregate state, that a
query has one state per group and per worker, and that `work_mem` does not
limit them; or the extension limits the total (a GUC for the total bytes of
counters). Ordinary aggregates such as `array_agg` behave alike, so this is a
documentation point more than a defect, but K12R-02 asked for a bound and the
extension has none.

Direction: add the sentence and a per-backend limit setting that raises an
error, with a test.

### K12S-04 (low) The README names the wrong error for a backup role that
### cannot read the tables

Where: README "Dump, restore, rename, drop" (says the refusal is "permission
denied for table timepart_policy_data").

Steps (18.7 and 19rc2): `pg_dump -U bob -d t2` where `bob` is a table owner
without `pg_read_all_data`.

Observed: exit 1, `failed to get data for sequence "timepart_run_data_run_id_seq";
user may lack SELECT privilege`.

Expected: the README matches (the sequence is read first) or says "permission
denied for a table or the sequence".

### K12S-05 (note) Nothing calls `self_test()` outside the regression setup

Where: the design (`analytics-phase12-queries.md`, step 3 of the task list at
1570, and `shard-phase2b.md` at 970) says the control plane compares
`self_test()` on every server and fails with `sketch_hash_mismatch`. A search of
`contrib` and `kingres` finds no caller: the check does not exist yet.
The line itself is sound (see K12R-03): two builds with different
floating-point contraction differ in it, so the comparison would catch them,
but only if something runs it. The README says the line "must be equal on every
node" and does not say that no component compares it yet.

## What held under attack

- Reading, changing, moving or deleting another owner's policy through every
  path of the starting list: SELECT, UPDATE (also changing the table the
  row names, with and without FROM), DELETE, INSERT ... ON CONFLICT DO UPDATE,
  MERGE, COPY, TRUNCATE, rules, triggers, ALTER VIEW: refused or no row.
- The base tables: "permission denied" for every role; `relacl` shows only the
  owner.
- The trigger pins `created_by`, `table_owner` and `parent_oid`: an INSERT
  that names `table_owner = postgres` and `created_by = zed` for another
  table was refused by the owner check, and for an own table the values were
  the caller's.
- A function in WHERE or a division by zero on a foreign row never ran
  before the view condition (barrier), under EXPLAIN too.
- `current_user` semantics: `SET ROLE`, `SET SESSION AUTHORIZATION`, a member
  with and without INHERIT, a SECURITY DEFINER function of another role called
  from a superuser session.
- Table renamed or moved: the policy follows; dropped: the policy goes.
- `pg_dump` of one schema or one table carries no policy; `-n kingres` carries
  the three tables and the sequence; the restore round trip (18.7).
- 19rc2 gave the same results as 18.7 for every experiment I repeated (history,
  rename, dump and restore).

## Not reproduced

- The in-place edit of the 0.1 script: the commit changes
  `kingres_timepart--0.1.sql` (199 lines) and the control file keeps
  `default_version = '0.1'`, while the README says a released script is not
  edited. A database created from the earlier text has tables with row-level
  security and no `ALTER EXTENSION UPDATE` path. I read this from the diff
  against commit 8ceed6b88b; I did not install the old script. No release has
  been made, so it may be intended.
- A policy left stale by a restore (the OID hint) attached to an unrelated
  table that is later renamed (the event trigger changes a row only if the
  named table is gone): read, not run.
- Statistics leak through a planner estimate function of a user operator.
- A role that changes the owner back and forth with a matching name.

## Not covered

- Whether the code that a table owner supplies (index expressions) can leave
  its role while a superuser session runs the maintenance: I did not test it.
- `ALTER EXTENSION UPDATE` (no script exists), `DROP EXTENSION` then
  `CREATE EXTENSION` with policies, an installation with user objects under
  the new `*_data` names, a dropped owner, a reused table OID.
- Crafted `recv` or text input of the largest values for memory, parallel
  workers each building a large state, and a union of the largest values on
  19rc2 (the README figures were not remeasured).
- `self_test()` under other locales (only `C`, `C.utf8` and `POSIX` exist
  here), other compilers, big-endian machines.
- The timing of lock retries, Windows, valgrind, a fresh sanitizer fuzz run.

## Baseline runs

1. `kingres_sketch`, 18.7: 7 pg_regress files pass; TAP 7 files, 348
   subtests, pass (`Result: PASS`).
2. `kingres_timepart`, 18.7: 10 pg_regress files pass; TAP 3 files, 134
   subtests, pass.
3. `kingres_sketch`, 19rc2: 7 files pass; TAP 7 files, 348 subtests, pass.
4. `kingres_timepart`, 19rc2: 10 files pass; TAP 3 files, 134 subtests, pass.

All four with `EXT_WERROR=1` and no compiler warning.
