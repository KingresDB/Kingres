<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026 RooyeKhat Media Co -->

# Review of the fixes for the review of the kingres_timepart 0.2 conversion (commit bc0a61c5b7)

Reviewed: commit bc0a61c5b7 on kingres-18 (fixes T13-01 to T13-14), against the review
`kingres/docs/reviews/2026-11-20-kingres-timepart-0.2.md` and the design section 10.4a of
`kingres/docs/design/analytics-phase12-storage.md`. Nothing was changed, committed or pushed.

## 1. Verdict

The main finding, T13-01 (the catalog read before the lock), is really fixed, and every refusal and carried property
that I attacked works on PostgreSQL 18.7. The tests of the fixes are good: of 25 mutants of the fixes, 23 are killed, one is
equivalent at the default settings and one shows a real gap in the tests (section 5).

New findings: no critical or high. One medium (T13F-01: a REVOKE that commits while the swap waits is undone silently).
Five low (T13F-02 to T13F-06), one note on tests (T13F-07) and one note decided by reading (T13F-08); the others were all reproduced.
None of them is a loss of a row.

## 2. What was run, and what was only read

Private copies of both installs, kingres_columnar and kingres_timepart built from `git archive kingres-18`, installed into the copies.

| Run | PostgreSQL 18.7 | PostgreSQL 19rc2 |
|---|---|---|
| pg_regress, 17 tests (setup, grid, create, policy, run, retention, names, hostile, security, history, privilege, forged, owner_code, convert, convert_more, drop, upgrade) | 17 of 17 pass | 17 of 17 pass |
| TAP, 9 files (001 to 009, with the new 009_convert_ddl_race) | pass | pass |
| T13-01 replayed by hand: a column added and filled and a unique index created on the partition while the conversion waits for its lock | fixed (see 3) | not run |
| same race with a new own trigger, a new NOT VALID check, row-level security enabled, a view created, the partition renamed and replaced by a table of the same name, renamed only, detached | all refused or failed as designed | not run |
| round trip heap -> columnar -> heap of a partition with trigger states R, A, D, storage parameters, statistics targets, index statistics, CLUSTER, an index in a tablespace | see 3 (T13-04, T13-05) | not run |
| refusals: NOT ENFORCED check, function with the row type as an argument, default partition above the limit, sub-partitioned default partition, hostile and multibyte names (29 two-byte letters) | run | not run |
| grants: grant with GRANT OPTION, column grant, owner's own revoke; REVOKE racing with the swap | run | not run |
| index DDL (rename, comment, SET) while a 1.5 million row partition is copied | run | not run |
| a partition dropped between the plan and the DO block (statement text replayed by hand) | run | not run |
| 25 mutants of `kingres_timepart--0.2.sql` (section 5) | run | not run |

Only read: the README text, the upgrade script (its conversion part has the same text as the full script, which the `upgrade` test also
asserts), the `timepart_run_action` path with the new `schema_changed` retry, the case of a publication of the schema (T13-03) beyond the
regression test that refuses it, T13-11 (documentation only).

## 3. The earlier findings

| Id | Earlier severity | Verdict | Evidence |
|---|---|---|---|
| T13-01 statements built before the lock, a column, index or constraint added meanwhile is lost | high | Fixed | `timepart__convert_plan` (VOLATILE, SECURITY INVOKER) is called again by the DO block right after `LOCK TABLE`, and the block holds only OIDs. My replay of the review's scenario (session A: insert, then ADD COLUMN + UPDATE + CREATE UNIQUE INDEX, commit while the conversion waits): the new partition has 2001 rows and 2001 non-null values of the new column, and the unique index exists on the columnar partition. The same wait with a new own trigger, a new NOT VALID check, RLS enabled, or a view created gives the right refusal (`conversion_refused`), not a silent copy: so the refusals also read the catalog again. A partition renamed and replaced by another table: `schema_changed` (55P03), and the other table is untouched; renamed only: `schema_changed`; detached: `conversion_refused: ... is not a partition`. Mutant T1 (the plan read before the lock) is killed by t/009 (4 assertions). |
| T13-02 REPEATABLE READ reader sees the swapped partition empty | medium | Accepted as a documented limit | README "Snapshots older than the swap"; t/009 pins the behaviour. I agree with the design's reasoning (no wait for older snapshots is possible from SQL without two transactions). The README wording on pg_dump is right (a window exists between its snapshot and its table locks). Not re-run. |
| T13-03 a schema publication publishes the copy under the temporary name | medium | Fixed (refusal) | `timepart__convert_refusal`, heap target: a publication of all tables or of the partition's schema is refused. Mutant T10 (refusal off) killed by timepart_convert_more. Not run with a real slot. |
| T13-04 the enabled state of a cloned trigger is reset | medium | Fixed | Round trip heap -> columnar -> heap: tr1 R, tr2 A, tr3 D before, in the middle and after. Mutant T2 killed. |
| T13-05 storage parameters, statistics targets, CLUSTER marker dropped | medium | Fixed, with one gap | Carried in both directions: fillfactor, autovacuum_enabled, parallel_workers; column statistics target 500; n_distinct; statistics target 777 of an index expression; the CLUSTER marker; index storage parameters. Mutants T3, T4, T5, T25, T26 killed. The "TOAST table" part of the text has no code (the loop reads only the reloptions of the partition), but the only heap target is the way back from columnar, which has no TOAST table, so nothing is lost. Not carried: the tablespace of an index (T13F-03). |
| T13-06 NOT VALID becomes valid | low | Fixed (refusal), with a side effect | Refused with the name. Mutant T6 killed. A NOT ENFORCED check (PostgreSQL 18) is also not validated and is now refused with the NOT VALID message (T13F-04). |
| T13-07 grantor changed, owner's revoked privilege returns | low | Fixed, with a new window | A grant by another role is refused (table and column level); the owner's own revoke is carried (`REVOKE DELETE ... FROM owner` survived, `{o=arwDxtm/o,b=r*/o}` and the column grant `{c=w/o}` kept). Mutants T7a, T7b, T11, T23 killed. New: T13F-01. |
| T13-08 dependents that are not views found by DROP after the copy | low | Fixed | `CREATE FUNCTION f(x partition)` is refused before any lock with `used by function s.fdep(s.dp_p1)`. Mutant T8 killed. The policy, SQL-body and regclass-default cases were not run by me (same query). |
| T13-09 multibyte names: temporary index names collide | low | Fixed | A partition named with 29 Arabic letters and an index converts. Names are `kc_<md5>_<n>`. Mutant T12 killed. |
| T13-10 attach scans the default partition under ACCESS EXCLUSIVE | low | Fixed, with a bypass | A default partition of 872 kB is refused when `convert_default_max_mb` is 0, and the conversion works at 256. Mutant T9 killed. A default partition that is itself partitioned is not measured (T13F-05). |
| T13-11 files left after a crash | low | Documented | README with a query; not run. |
| T13-12 session_user check untested (M23) | low | Fixed | Mutant T17 (the check removed) killed by timepart_convert_more. |
| T13-13 test gaps (M15, M16, M18, M32, M34, M37, M04, M05, M40) | note | Mostly fixed | Killed by my mutants: replica identity (T21), constraint comments (T22), default privileges (T23), tablespace (T19), the cycle look of the swap (T18). Not mutated by me: M18 grid, M37 other access method. Two swap gaps remain (T13F-07). |
| T13-14 helpers executable by PUBLIC | note | Fixed in part | `timepart__special_table` answers only the owner of the parent or a superuser (a non-owner gets 42501; mutant T13 killed). It does not tie the second argument to the first (T13F-08). |

## 4. New findings

### T13F-01 (medium) A REVOKE or GRANT that commits during the swap wait is lost; the conversion succeeds

Where: `kingres_timepart--0.2.sql`, the DO block of `timepart__convert_stmts`: the privileges are read at line 3935 and replayed on the
new table; the swap asks for ACCESS EXCLUSIVE on the table at line 4002 (a wait of up to 500 ms) and drops the old partition at line 4009.

Scenario: GRANT and REVOKE take no lock on the table they name, so the SHARE ROW EXCLUSIVE lock of the conversion does not keep them out. A role that
revokes a privilege from the partition after the block read its ACL and before the old partition is dropped has its REVOKE undone: the new
partition has the privilege. The window is the swap wait, which a reader that holds the table makes up to half a second; it is also the
time of the detach and attach.

Reproduction (PostgreSQL 18.7, owner role): a partition with SELECT granted to role b; a reader holds ACCESS SHARE on the table; the conversion
starts and waits for the swap; the moment `pg_locks` shows the conversion's ungranted ACCESS EXCLUSIVE request, the owner runs
`REVOKE SELECT ON part FROM b` (it returns at once and commits); the reader ends 200 ms later and the conversion commits.
Result: `relacl` of the new partition is `{o=arwdDxtm/o,b=r/o}` and `has_table_privilege('b', part, 'select')` is true, with no error and no
warning. Reproduced.

Fix: read the ACLs of the old partition (and its columns) again after the lock on the table is held, just before the detach, or compare
`relacl` and the column ACLs once more at that point and fail with a retryable error when they differ. A GRANT lost in the same way
is less harmful, a REVOKE lost is a privilege that an administrator thought was gone.

### T13F-02 (low) Index DDL that commits during the copy is lost silently

Where: the plan reads the indexes under the lock (fixed T13-01), but `ALTER INDEX ... RENAME`, `COMMENT ON INDEX` and `ALTER INDEX ... SET (...)`
do not lock the table, so they run while the copy runs and apply to the old index, which the swap drops.

Reproduction (PostgreSQL 18.7): a partition of 1.5 million rows with two indexes; start the conversion (4.8 s); 1.5 s later run
`ALTER INDEX ix_num RENAME TO ix_num_renamed`, `COMMENT ON INDEX ix_kind IS 'late comment'`, `ALTER INDEX ix_kind SET (fillfactor = 50)`:
each returns at once with success. After the conversion the indexes are `ix_num` and `ix_kind`, with no comment and no option.
Reproduced. Smaller than T13-01 (names, comments and options, no data or uniqueness), but it is the same class and silent.

Fix: the same as T13F-01 in spirit: compare the index names, comments and options once more under the lock of the table before the detach and
fail with `schema_changed` when they differ; or document the limit next to the privileges.

### T13F-03 (low) The tablespace of an index is not carried

Where: `timepart__convert_plan`, index statements built from `pg_get_indexdef(i.indexrelid)` (line 3640), which has no TABLESPACE clause (the
definition of a primary key or unique constraint does carry it).

Reproduction: `CREATE INDEX q_ts ON p (kind) TABLESPACE ts1`; convert to columnar and back: the index is in the default tablespace
afterwards (before: ts1). Reproduced. The tablespace of the table itself is carried (t/009).

Fix: add `TABLESPACE` to the statement from `pg_class.reltablespace` of the index (or `ALTER INDEX ... SET TABLESPACE` after the rename), or refuse.

### T13F-04 (low) A NOT ENFORCED check constraint is refused as "NOT VALID"; before the fix it was carried

Where: `timepart__convert_refusal`, line 3326: `NOT k.convalidated AND k.contype IN ('c', 'n')`.

Scenario (PostgreSQL 18): `ALTER TABLE p ADD CONSTRAINT c CHECK (num > 0) NOT ENFORCED`; the conversion is refused with
"has the constraint "c" that is not validated (NOT VALID): the new table would have it validated, and a row that violates it would fail the
copy". A NOT ENFORCED constraint has `convalidated = false` too, but the sentence is wrong for it (nothing is checked, no row can fail), and
the original review noted that NOT ENFORCED checks were carried correctly, so the fix turned a working case into a refusal and the policy
path records it "skipped" forever. Reproduced on 18.7.

Fix: test `conenforced` (PostgreSQL 18 and later; the compat is by the catalog column, so the test would be dynamic SQL or a check of the
server version) and keep carrying NOT ENFORCED constraints, or give them their own message.

### T13F-05 (low) A default partition that is itself partitioned is not measured

Where: line 3389: `pg_relation_size(d.oid) > ...`. For a partitioned default partition `pg_relation_size` is 0, so the limit of
`convert_default_max_mb` is never reached, and for a columnar default partition the main fork may not show the data either.

Reproduction: a default partition partitioned by HASH with one leaf of 20000 rows (892 KB); `convert_default_max_mb = 0`; the conversion of
another partition goes through (the same table with an ordinary default partition is refused). Reproduced. The effect is the stall that T13-10
described, for the large default partitions that use sub-partitioning.

Fix: sum `pg_total_relation_size` (or the sizes of the leaves, through `pg_partition_tree`) of the default partition.

### T13F-06 (low) A partition dropped between the plan and the lock gives a syntax error, not `schema_changed`

Where: the first statement of the DO block (line 3807): `'LOCK TABLE ' || v_part_oid::regclass::text`. For an OID that no longer exists the
cast gives the digits, and `LOCK TABLE 17112` is a syntax error; the handler only catches `undefined_table`.

Reproduction: take the statement text of `timepart__convert_stmts`, drop the partition, run the DO: `ERROR: syntax error at or near "17112"`.
Reproduced (by replay; the real window is the milliseconds between the early plan and the start of the block, a rename during the wait gives
the intended `schema_changed`). Under the policy it is recorded as an ordinary failure, not as the retryable `schema_changed`.

Fix: look the OID up in `pg_class` before the LOCK and raise `schema_changed` when the row is gone (or also catch the syntax error).

### T13F-07 (note, tests) Two swap rules still survive the tests

* Mutant T14 (the check of the lock actually held, in the DO block, line 3815, which catches "the name is another table now") survives pg_regress and
  all of t/009: t/009 renames the partition and does not put a table in its place. I reproduced the case by hand
  (rename the partition, create a table of the old name, commit, while the conversion waits) and the real code gives `schema_changed`; no test
  fails if the check is removed.
* Mutant T16 (the cap `least(500, ...)` of the swap wait at line 3765 raised to 60000) survives t/009 because at the default
  `deadlock_timeout` of 1 s the other bound, half of it, is also 500 ms. A test with `deadlock_timeout = 10s` would pin the cap.

### T13F-08 (note, read only) `timepart__special_table` checks the parent but not the second OID

Where: lines 3149 to 3182. The caller must own the parent (or be a superuser), but `p_part` is used as given: an owner of any table can pass
the OID of another role's table as `p_part` and learn whether it belongs to a ledger or a distributed table, which T13-14 wanted to keep
private. I confirmed that an owner gets NULL for any OID without an error (no relation between the two OIDs is checked); I did not
install kingres_ledger or kingres_shard to see the positive answer. The information is one word about tables whose OIDs are guessable and is not
secret in any strong sense, so the severity stays a note.

Fix: require that `p_part` is a partition of `p_parent` (`pg_inherits`) before answering.

## 5. Mutants of the fixes

One-line edits of the installed full script, one at a time, each against a fresh cluster; pg_regress of setup, convert and convert_more
for the rules, TAP 009 for the race and the swap; PostgreSQL 18.7 only.

| Id | Mutant | Result |
|---|---|---|
| T1 | the plan read before the lock, not after | killed (t/009, 4 assertions) |
| T2 | trigger states not put back | killed (convert_more) |
| T3 | storage parameters not carried | killed (convert_more) |
| T4 | column statistics targets not carried | killed (convert_more) |
| T5 | CLUSTER marker not carried | killed (convert_more) |
| T6 | NOT VALID constraint not refused | killed (convert_more) |
| T7a, T7b | grant by another role not refused (table, column) | killed (convert_more) |
| T8 | dependents not refused | killed (convert_more) |
| T9 | default partition size not checked | killed (convert_more) |
| T10 | publication of the schema not refused (heap) | killed (convert_more) |
| T11 | owner's own revoke not carried | killed (convert_more) |
| T12 | temporary index names all the same | killed (timepart_convert and convert_more) |
| T13 | special_table asked by anyone | killed (convert_more) |
| T14 | lock actually held not verified (name replaced by another table) | SURVIVES (T13F-07), regress and t/009 |
| T16 | swap cap 500 ms raised to 60 s | SURVIVES, equivalent at the default `deadlock_timeout` (T13F-07) |
| T17 | session_user = current_user check removed | killed (convert_more) |
| T18 | the cycle look of the swap removed | killed (t/009, 2 assertions) |
| T19 | tablespace not carried | killed (t/009, 2 assertions) |
| T21 | replica identity FULL not carried | killed (convert_more) |
| T22 | constraint comments not carried | killed (convert_more) |
| T23 | default privileges not revoked from the new table | killed (convert_more) |
| T25 | column options (n_distinct) not carried | killed (convert_more) |
| T26 | index statistics targets not carried | killed (convert_more) |
| T27 | schema_changed renamed (the run's retry rule) | killed (t/009, 2 assertions) |

## 6. Other checks that held

* Injection: the DO block is made of OIDs and quoted literals (`%L`, `quote_ident`, `quote_literal`); option names and values of storage parameters
  and column options go through `quote_ident` and `quote_literal`; a `$tpl$` in a name is harmless because the block is passed as a literal.
* search_path: the block runs under `pg_catalog, pg_temp` (the function's own SET), every called object is qualified. The new
  `timepart__convert_plan` is SECURITY INVOKER and checks `require_owner(current_user)` first, so a stranger that runs it gets "must be owner".
* Frame mode (a superuser converting a table of another owner, `trust_table_owners = on`) works with the new code: the owner (`y`, no explicit grant on
  the schema `kingres`, which PUBLIC has by default) converted; the extra requirement of USAGE on the schema `kingres` in the README only matters if
  an administrator has revoked it from PUBLIC. `timepart__special_table` judges `session_user`, so the superuser's session passes.
* The upgrade script and the full script have the same text of the conversion (the `upgrade` test passes on both majors).
