<!-- SPDX-License-Identifier: AGPL-3.0-only -->
# Review of the third fix round: contrib/kingres_sketch and
# contrib/kingres_timepart (phase 12)

Reviewer: an independent reviewer. Reviewed state: commit fe25792c14
(`kingres_timepart`) and commit 821c33be4b (`kingres_sketch`), the committed
tree at 821c33be4b for the two directories. Exported with `git archive` into
a private work area; the working tree was not used. Nothing in the
repository was changed except this file.

## Scope and method

Read the commit messages of fe25792c14 and 821c33be4b, the review they
answer (`2026-11-16-phase12-extensions-second-fixes.md`, K12S-01 to K12S-05)
and the two reviews before it. Read `kingres_timepart--0.1.sql` in full
(tables, views, the policy trigger, `timepart__exec_as`, the run functions,
`timepart_adopt`, the event triggers) and the relevant parts of
`kingres_sketch.c`, `hll.c`, `topn.c`, `tdigest.c`. Read both `README.md`
files for the security sections the commit messages point at.

Built and ran the existing suites once on each major first (18.7 and 19rc2,
read-only installations copied for this review, `EXT_WERROR=1`, at most
`-j2`, ports 59700-59739), with `kingres/dev/ext.sh <ext> sync clean build
install regress tap`, then wrote the experiments below against a private
server per major (ports in the same range, stopped at the end of each
session), as roles with only the rights the README gives, plus one
superuser session playing the maintenance driver.

Baseline runs (see "Numbers of the baseline runs"): both extensions build
without a compiler warning and pass their regression and TAP suites on
18.7 and 19rc2.

## Summary

| Severity | Count |
|---|---|
| Critical | 1 |
| High | 2 |
| Medium | 0 |
| Low | 0 |
| Note | 1 |

The escalation of privilege through the table owner's code during DDL
(the finding of this round's own commit, fe25792c14) is fixed: the
`SECURITY DEFINER` frame in the temporary schema refuses `SET ROLE`,
`RESET ROLE` and `SET SESSION AUTHORIZATION`, confirmed by reproducing the
test `timepart_privilege` and by hand-driving `timepart__exec_as`. Two
problems remain in the same area and a new one, unrelated to any earlier
review, defeats the memory limit that K12S-03 added:

1. `kingres_sketch.state_memory_limit` is enforced with a ceiling that an
   ordinary role may not exceed by `SET`, but `ALTER ROLE <itself> SET`
   is not checked at all and lets any login role give itself an
   arbitrary ceiling, without superuser rights (K12T-01, critical).
2. The table owner's code, run inside the `SECURITY DEFINER` frame, can
   leave objects of its own choosing in the session's temporary schema;
   nothing in `timepart__exec_as` removes them, and they outlive the
   action, the run and the maintenance call, so they are still there
   for the superuser's own later, unrelated statements in that session
   (K12T-02, high).
3. `timepart_adopt()` re-derives the OIDs of a restored row only when its
   stamp differs from this installation's; a row whose stamp already
   equals this installation's (which a crafted dump can set, since the
   policy trigger does not touch an already non-null `table_owner_oid`
   that a superuser's restore inserts) is treated as native without any
   check that its `parent_oid` and its recorded owner agree with who
   really owns that table now, so a crafted dump can attach a policy
   with a short, destructive retention to a table the dump's source
   never owned (K12T-03, high).

## Fate of K12S-01 to K12S-05 and of the escalation the round found

| ID | Fate | Evidence | Failed on parent? |
|---|---|---|---|
| Escalation (owner's DDL code leaves `SET ROLE` via `RESET ROLE`) | fixed | `timepart_privilege` reproduced: `RESET ROLE`, `set_config('role', ...)`, `SET SESSION AUTHORIZATION` all refused in the `pg_temp` `SECURITY DEFINER` frame; `escapes = 0` | Not run here; commit states author ran it, nobody else has |
| K12S-01 (dropped table's history shown to a later owner of the name) | fixed | dropped `alice`'s table, let `bob` reuse the name: `bob`'s views show only runs owned by `bob`; `alice`'s run stays visible only to `alice` | Not reproduced here; matches the second review's finding |
| K12S-02 (owner identity: OID vs. name, rename, real change) | fixed | renaming the owning role mid-run does not fail the run; changing the table's real owner fails the next action with `owner_changed` | Not reproduced here |
| K12S-03 (one state grows unbounded, `work_mem` has no effect) | partly | the limit stops one state and a `GROUP BY` of many (`sketch_memory`: SQLSTATE 54000, counter back to 0); but a role raises its own ceiling past the administrator's with `ALTER ROLE <itself> SET` (K12T-01) | The commit's mutant (read before library load) reproduced: first two runs fail that way with `KINGRES_PG` set, `EXT_WERROR=1` |
| K12S-04 (`pg_dump` messages; `DROP`/`CREATE EXTENSION`) | fixed | the README's quoted `pg_dump` messages match 18.7 and 19rc2; `DROP`/`CREATE EXTENSION` and `CREATE EXTENSION` over pre-made objects behave as described | Not reproduced here |
| K12S-05 (`self_test()` compared by nothing) | fixed | README states plainly that nothing compares it yet; no code claims otherwise | N/A (documentation only) |

## New findings

### K12T-01 (critical) `ALTER ROLE <itself> SET
### kingres_sketch.state_memory_limit` is not checked, so an ordinary role
### raises its own memory ceiling without superuser rights

Where: `ks_state_limit_check()` in `contrib/kingres_sketch/src/
kingres_sketch.c`. The hook exempts every `GucSource` below `PGC_S_CLIENT`,
with the comment that these are "the per-database and per-role settings
(which only an administrator can store for another role)". That is true of
a role storing the setting for *another* role, but PostgreSQL lets any role
run `ALTER ROLE CURRENT_ROLE SET ...` for itself without `CREATEROLE` or
superuser rights (the permission check in `AlterRoleSet` is skipped when the
target role is the caller), and the value then applies at the start of a
later session of that role with the same low-privilege source
(`PGC_S_USER`/`PGC_S_DATABASE_USER`), which the hook never judges.

Steps (18.7; the hook has no major-specific code, not reproduced separately
on 19rc2):

```sql
-- as an ordinary login role, not a superuser, not CREATEROLE:
ALTER ROLE nosuper SET kingres_sketch.state_memory_limit = '2GB';
```
then, in a fresh session logged in as `nosuper`:
```sql
SELECT kingres_sketch.state_bytes();  -- loads the library
SHOW kingres_sketch.state_memory_limit;
```

Observed: `ALTER ROLE` succeeds with no error for the unprivileged role
altering itself; `SHOW` in the new session reports `2GB`, twice the
compiled-in administrator default of 1GB, and no superuser or
administrator action was involved at any point.

Expected: the commit message states "a role that is not a superuser may not
set it above the value the administrator gave it (configuration file,
`ALTER DATABASE`, `ALTER ROLE`)"; a role raising its own ceiling by altering
its own role is exactly the case the check hook is meant to stop, and the
existing regression test `sketch_memory.sql` only exercises the
administrator path (a superuser running `ALTER ROLE ks_mem_user SET ...`
for another role), never a role altering its own row in
`pg_db_role_setting`. The single line in that test that does run as the
unprivileged role under `SET ROLE` (`ALTER ROLE ks_mem_user SET
kingres_sketch.state_memory_limit = '2GB'` while `SET ROLE ks_mem_user` is
active) is allowed to succeed with no check on its effect in a later
session, so the gap is already present in the committed test output and was
not noticed.

Severity: critical. The limit exists specifically to stop one backend's
aggregate states from growing unboundedly (the review this round answers
reproduced 19 MB growing to 742 MB); any login role that can call one
function of the extension can raise its own ceiling up to `MAX_KILOBYTES`
(about 2 TB) without any administrator action, which removes the memory
protection entirely for that role and lets it push its own backend's
resident memory far past what the administrator intended, up to the point
of exhausting the machine (a denial of service reachable by an ordinary
role, which the limit exists to prevent).

Suggested direction: in the check hook, judge an `ALTER ROLE`/`ALTER
DATABASE` setting against the ceiling too, unless the role performing the
`ALTER` has `CREATEROLE`/superuser rights (the actual administrator path),
not merely by the `GucSource` of the value when it is later applied; or
require the hook to compare against a ceiling fixed at a time before the
role could have altered it itself (for example the configuration file or
command-line value only, via `GetConfigOptionByName` with a source
restricted to `PGC_S_FILE` and below).

### K12T-02 (high) The table owner's code, inside the `SECURITY DEFINER`
### frame, can leave objects in the session's temporary schema that outlive
### the action, the run, and the maintenance call

Where: `kingres.timepart__exec_as` in `kingres_timepart--0.1.sql`. The
function creates `pg_temp.timepart_exec`, gives it to the table owner, calls
it, and drops it; the owner's statements run inside that function's body
(`EXECUTE s` for each statement in the array). Nothing stops a statement
from creating a further object in `pg_temp` under a name of the owner's
choosing; `timepart__exec_as` drops only the one function it made, and no
other part of the maintenance (`timepart_run_action`, `timepart_run_phase`,
`timepart_run`) inspects or clears the session's temporary schema.

Steps (18.7):

```sql
-- the owner's code, reached through an index expression evaluated at
-- ATTACH PARTITION, as in the committed test timepart_privilege:
CREATE OR REPLACE FUNCTION tpp.f_index(x int) RETURNS int
LANGUAGE plpgsql IMMUTABLE AS $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_class
                  WHERE relname = 'pg_roles'
                    AND relnamespace = pg_my_temp_schema()) THEN
    EXECUTE 'CREATE VIEW pg_temp.pg_roles AS
             SELECT * FROM pg_catalog.pg_roles WHERE public.probe()';
  END IF;
  RETURN x;
END $$;
-- public.probe() is owned by the superuser and merely reports who is
-- running it; the owner was granted EXECUTE on it only.
```
A superuser then runs the maintenance (`CALL kingres.timepart_run(...)`)
for the owner's table, and afterwards, in the same session, runs an
unrelated query of its own:
```sql
SELECT count(*) FROM pg_roles;
```

Observed: the view `pg_temp.pg_roles`, owned by the table owner, is created
during the action and is still present in the session's temporary schema
after `timepart_run` returns (`SELECT relname FROM pg_class WHERE
relnamespace = pg_my_temp_schema()` shows it; it is not dropped by
`timepart__exec_as`, which drops only `timepart_exec`). The superuser's own
later, unqualified `SELECT count(*) FROM pg_roles` is then silently answered
by that view rather than by `pg_catalog.pg_roles` (confirmed by the notices
`public.probe()` raises, which fire once per row of the real `pg_roles`):
the temporary schema is searched before any schema named in `search_path`,
so a name the owner's code chose intercepts the superuser's own later work
in that session, with no relation to the maintenance call that created it.

Expected: the README states that the frame leaves "no other session sees
it (it is not committed), and no object of the extension is owned by a
table owner", which is true and was confirmed, but says nothing about
objects the owner's code creates that are not part of the extension; the
review this round explicitly asks whether anything the owner's code leaves
behind is used "after the maintenance when the superuser goes on working in
that session". It is: the object is not transactional (temporary relations
survive `COMMIT`, which `timepart_run_phase` does between actions, and
survive past the end of the call), it is owned by the table owner (not
dropped automatically when the owning role still exists), and it persists
for the rest of the superuser's session.

Severity: high. No escalation of privilege occurs (the intercepted view's
own `WHERE` clause still runs with the caller's identity, confirmed by the
notices showing the superuser as `current_user`), but a table owner who is
never trusted with anything beyond their own table can silently redirect
or hide the result of the superuser's own, unrelated queries for the rest
of a session that runs maintenance for many tables and owners in a row (a
plausible deployment, since `timepart_run_phase` is designed to be called
repeatedly from one driver session): a wrong answer to a query the
superuser never associated with the owner's table, for as long as that
session lasts.

Suggested direction: drop every relation, routine and type the action
leaves behind in the session's temporary schema (besides the one the
frame itself made), by name, as part of `timepart__exec_as`, before
returning; or run each action's DDL in a backend of its own (a new
connection) rather than in the single long-lived driver session, so a
temporary schema does not survive from one action to the next or past the
call.

### K12T-03 (high) A policy row whose stamp is forged to this
### installation's own stamp skips `timepart_adopt()`'s identity check and
### can attach a destructive policy to a table its source never owned

Where: `kingres.timepart_adopt()` and the policy trigger
`kingres.timepart__policy_trg()` in `kingres_timepart--0.1.sql`. The trigger's
branch for a superuser (the path `pg_dump`/`pg_restore` data loading takes)
only fills `table_owner_oid`/`table_owner` when they are `NULL`; it never
re-derives them, nor `parent_oid` or `stamp`, from the row's actual
`schema_name`/`table_name` when the inserted row already carries values
(which a `COPY` of dumped data does). `timepart_adopt()` only touches rows
whose `stamp` differs from `kingres.timepart__stamp()`; for the policy
table, the parts of `WHERE` it adds when it does run do not check that the
table it resolves `parent_oid` to is actually owned by the role named in
`table_owner`.

Steps (18.7), simulating a crafted "restored" row by inserting directly
into the base table as a superuser (equivalent to what `COPY` during a
restore of a hand-edited dump file does; the trigger's superuser branch
treats both the same way, since it does not distinguish a dump restore
from any other superuser insert):

```sql
-- a real table, never touched by the extension, owned by b1
CREATE TABLE victim.v (ts timestamptz NOT NULL) PARTITION BY RANGE (ts);
ALTER TABLE victim.v OWNER TO b1;
-- partitions already exist (made honestly through create_time_partitions)

-- the crafted row: schema/table name of a real table, the real current
-- owner's name and OID, and this installation's own stamp
INSERT INTO kingres.timepart_policy_data
  (schema_name, table_name, parent_oid, partition_every, timezone,
   create_ahead, retention, retention_action, run_every, enabled,
   stamp, table_owner, table_owner_oid, created_by, created_by_oid,
   created_at, updated_at, next_run_at)
SELECT 'victim', 'v', 'victim.v'::regclass::oid, interval '1 day', 'UTC',
   interval '1 day', interval '1 second', 'drop', interval '5 minutes',
   true, kingres.timepart__stamp(), 'b1', 'b1'::regrole::oid,
   'b1', 'b1'::regrole::oid, now(), now(), now();

CALL kingres.timepart_run('victim.v', '2026-05-05 UTC');
```

Observed: the run proceeds normally (`state = 'done'`); `timepart_run_action`
shows `create` actions executed against `victim.v`, which never had a
policy set by its owner through `timepart_set_policy` or through the policy
view at all. The forged row is never visited by `timepart_adopt()` because
its `stamp` already equals `kingres.timepart__stamp()`, so none of the
reconciliation the commit describes ("rows restored from a backup carry
another installation stamp and are identified by names until
timepart_adopt() gives them the OIDs of this cluster") ever runs on it, and
nothing else re-checks that `parent_oid` and `table_owner`/`table_owner_oid`
genuinely belong together. With a short enough `retention` the same row
would go on to drop the owner's existing partitions on a later action,
which this experiment did not let run far enough to observe directly (see
"Not reproduced").

Expected: the commit's own description of the identity model implies that
a row's effect on a live table is tied to the role that genuinely owns
that table; a row's `stamp` matching this installation by construction
(rather than by having actually been adopted into it) should not be enough
to bypass any check. At minimum, `timepart_run_action`'s own
`owner_changed` check (`v_info.r_owner_oid IS DISTINCT FROM
v_pol.table_owner_oid`) is the only thing standing between a forged row
and the table it names, and it passes trivially once the forger copies the
table's real, current owner OID into the row, which is public information
(`relowner` of any table the forger can see with `\d+` or `pg_class`).

Severity: high. This requires the ability to place a row directly into the
base table (as this review simulated) or into a dump file that a superuser
or an equivalently privileged restore role later loads unexamined into the
target installation; the README states "a superuser (the extension's own
code, a restore) is trusted", so this does not cross a boundary the
extension claims to defend on the superuser's own account. It matters all
the same because the party supplying the dump content (the one who shaped
the row) need not be a superuser or ever have owned `victim.v`: an ordinary
role that controls what is in a dump file handed to an operator for restore
can cause a superuser's later, routine restore to attach a destructive
policy to an unrelated table, with no further action by the operator
beyond the restore and running the maintenance as documented. Marked high
rather than critical because it depends on the dump's content being
attacker-shaped rather than an honest `pg_dump` of the attacker's own
objects, which the review explicitly asked to test as a hostile case.

Suggested direction: when `timepart_adopt()` (or the policy trigger, on
insert by a superuser) resolves `parent_oid` for a policy row, require that
the resolved table's current `relowner` matches a role of the recorded
`table_owner` name, not merely that the schema/table name matches; treat a
`stamp` equal to this installation's, on a row whose `parent_oid` does not
match such a table, the same as an unmatched stamp (re-run the adoption
logic) rather than trusting it as already native.

## What held under attack

- The escalation this round's own commit fixes: `SET ROLE`, `RESET ROLE`,
  `set_config('role', ...)` from an `IMMUTABLE`-declared function, `SET
  SESSION AUTHORIZATION`, tried from an index expression, a check
  constraint, a default, a generated column and both a row and a
  statement trigger, all refused inside the `pg_temp` `SECURITY DEFINER`
  frame on 18.7; every logged statement ran as the table owner, none as
  the superuser.
- `timepart__exec_as` called directly for a role that does not exist is
  refused (`42704`); called for a role whose statements fail (no `CREATE`
  on the target schema) rolls the subtransaction, and its frame function,
  back, leaving no `pg_temp.timepart_exec` behind and recording the
  action as failed.
- A pre-existing `pg_temp.timepart_exec` function (made in an earlier
  action by the owner's own code, with an extra default argument so its
  signature differs) is not silently reused: the next action's
  `CREATE FUNCTION pg_temp.timepart_exec(text[])` fails
  ("already exists with same argument types" for an identical signature,
  "is not unique" for an overload at the call site), and the action is
  recorded as failed rather than running against the wrong function.
- `timepart__require_superuser()` still refuses `timepart_run_action`,
  `timepart_finish_run` and `timepart_adopt()` to a non-superuser.
- Role renamed between two actions of the same run: the run continues
  without `owner_changed`; the table's real owner actually changed by
  `ALTER TABLE ... OWNER TO`: the next action fails with `owner_changed`
  as documented.
- The sketch memory limit itself, once in effect and not raised by
  `ALTER ROLE <itself>` (K12T-01): a single state over the limit, a
  `GROUP BY` of many small states together over the limit, the union
  (combine) path, a savepoint rolled back, and a cursor closed all bring
  `state_bytes()` back to exactly zero; a query under the limit still
  runs normally under a low one.

## Not reproduced

- K12T-03 taken to the point of an actual `DROP` of the victim's
  partitions by the forged policy's retention: the experiment confirmed
  the forged row is adopted as native and that `create` actions run
  against the unrelated table, but was not run far enough past the
  retention window to watch a `retire`/`drop` action remove the victim's
  own partitions; the `owner_changed` check and the retire code path give
  no reason to expect it would stop, but this was not observed directly.
- Whether an attacker who is not already able to insert into the base
  table or shape dump bytes directly (for example, a role restricted to
  an honest `pg_dump` of only its own objects through the views) can
  reach K12T-03 by any route that does not require controlling the dump
  file's bytes or a direct insert as superuser.
- K12T-02 with a leftover object named to shadow something the
  extension's own functions call unqualified (rather than something the
  superuser's own later interactive query calls unqualified): every
  function body in `kingres_timepart--0.1.sql` schema-qualifies its own
  calls and sets `search_path = pg_catalog, kingres, pg_temp`, and the
  committed test `timepart_hostile` already attacks this; no route to
  reach the extension's own internals through a planted `pg_temp` object
  was found, only the superuser's own unrelated later work in the same
  session.
- Whether `ALTER DATABASE ... SET kingres_sketch.state_memory_limit` by a
  non-superuser database owner (rather than a role altering itself) is
  checked or not; not tried for lack of time, though the same
  `GucSource` reasoning that produces K12T-01 for `ALTER ROLE` suggests
  the same gap, since a database owner can likewise be the target of
  their own setting.
- Parallel workers and the memory limit together with K12T-01 (whether a
  worker spawned by a backend that raised its own ceiling also runs with
  the raised value, which the code comment implies since a worker's
  values are exempted from the check by `IsParallelWorker()`).

## Not covered

- Point (a) of the sketch memory review item: whether many states each
  just under one growth step, or memory of parallel workers, non-aggregate
  functions (`topn_add`, casts, sorting or hashing a sketch value, window
  or ordered-set aggregates, `GROUPING SETS`, states serialised and read
  back) let a role exceed the limit by materially more than one growth
  step. Only the cases the committed test `sketch_memory` and `t/
  008_memory.pl` already cover were reproduced.
- Point (d), the memory and time cost of one context per aggregate state
  compared with the parent of 821c33be4b, on a `GROUP BY` with a million
  small groups, on either major.
- The event triggers racing a run with a concurrent `DROP`/`RENAME`/
  owner change of the table, or OID reuse of a dropped role or table
  reused by a new one of the same name, during the window of a run.
- `timepart_due` being `VOLATILE` and calling `timepart_adopt()` as a side
  effect of what looks like a read: who may call it today (its
  definition was read, but no experiment was run against it) and what a
  caller who is not the maintenance driver could trigger by calling it.
- `security_barrier`/leak-proofness of the history views under
  `pg_has_role` conditions, beyond what the second review already
  exercised.
- Deferred constraint triggers, `ON COMMIT` actions of temporary tables,
  event triggers of the owner's own making, `LISTEN/NOTIFY`, a cursor
  `WITH HOLD`, a prepared or cached plan, and `pg_advisory_lock` as
  further means for the owner's code to act outside the `SECURITY
  DEFINER` frame; only the leftover-object route (K12T-02) was tried and
  found to work.
- `dblink`/`postgres_fdw` loop-backs and `COPY ... PROGRAM` from inside
  the frame (neither extension nor OS-level program execution was
  available to the owner role in this review's test setup).
- Whether `kingres_sketch`'s README and code agree point for point beyond
  the memory section; only the memory section was compared in depth.

## Numbers of the baseline runs

- `kingres_timepart`, 18.7, in tree: 12 of 12 regression tests pass
  ("All 12 tests passed"); TAP: 3 files, 167 tests, all pass.
- `kingres_timepart`, 19rc2, out of tree: TAP `t/001_locks.pl` passes in
  the first combined run; a later combined run hit a transient `initdb`
  failure under `t/002_interrupt.pl` on a heavily loaded machine
  (unrelated to the extension: `pg_control` and base files went missing
  under a concurrent job), so `t/002_interrupt.pl` and
  `t/003_dump_restore.pl` were rerun alone and both passed (2 files, 114
  tests); the failure did not repeat and is not reported as a finding.
- `kingres_sketch`, 18.7, out of tree: 8 regression test files pass;
  TAP: 8 files, 377 tests, all pass.
- `kingres_sketch`, 19rc2, out of tree: TAP: 8 files, 377 tests, all pass.
- The parent-commit mutant the sketch commit message describes was
  reproduced: with `EXT_WERROR=1` and `KINGRES_PG` set (the out-of-tree
  script, which preloads the library), the first two runs of the
  committed `sketch_memory` test read
  `kingres_sketch.state_memory_limit` before any function of the library
  had been called in that backend and failed exactly as the commit
  message describes; the committed test (which calls a function first)
  passes.
