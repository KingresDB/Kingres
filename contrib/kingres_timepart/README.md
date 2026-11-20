<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Portions Copyright (c) 2026, RooyeKhat Media Co -->
# kingres_timepart

Time partitioning of ordinary (non-distributed) tables for KingresDB (a thin
fork of PostgreSQL 18.7), version 0.2. For a table partitioned by `RANGE` on a
time column it creates the partitions for a period ahead, converts the
partitions that have become older than a period to columnar storage
(`kingres_columnar`), drops or detaches the partitions that have become older
than a retention period, and keeps a policy per table and a history of runs.
The design is section 10 of
[`kingres/docs/design/analytics-phase12-storage.md`](../../kingres/docs/design/analytics-phase12-storage.md)
(increments T1 and T2); the departures from it are listed at the end of this
file.

The extension is written in SQL and PL/pgSQL. It has no C code, needs no
`shared_preload_libraries` entry, and starts no background process: a run
happens when something calls it (`psql`, `pg_cron`, or the scheduler of the
KingresDB control plane). The same install script works on PostgreSQL 18 and
19.

Conversion needs the extension `kingres_columnar` in the same database (and its
library in `shared_preload_libraries`, see its README); everything else works
without it. Not in this version: distributed tables (they need the shard
extension, phase 9), and the scheduler, the REST contract and the web console.

## Install

```sql
CREATE EXTENSION kingres_timepart;
```

The control file sets `schema = kingres` and `relocatable = false`. The schema
is created if it does not exist. Every object of the extension is in it.
`CREATE EXTENSION` needs a superuser, because it creates two event triggers.

EXECUTE on every function is revoked from `PUBLIC` (the pure helpers and the
functions that the views call excepted). An administrator grants it
to the roles that should use the functions that act on one's own tables. The
policy and history are readable by every role that has USAGE on the schema,
through views that show each role the rows of the tables it owns (judged by
`current_user`):

```sql
GRANT USAGE ON SCHEMA kingres TO app_owner;
GRANT EXECUTE ON FUNCTION
    kingres.create_time_partitions(regclass, interval, timestamptz, timestamptz, name, text),
    kingres.drop_old_time_partitions(regclass, timestamptz, boolean, text),
    kingres.timepart_set_policy(regclass, interval, interval, interval, text, interval, text[], jsonb, text, interval, boolean),
    kingres.timepart_unset_policy(regclass),
    kingres.timepart_plan(regclass, timestamptz),
    kingres.timepart_partitions(regclass),
    kingres.timepart_check(interval, timestamptz)
  TO app_owner;
GRANT EXECUTE ON PROCEDURE
    kingres.convert_partition(regclass, regclass, name, text[], jsonb),
    kingres.convert_old_partitions(regclass, timestamptz, name, text[], jsonb)
  TO app_owner;
```

## What a table must be

`timepart_set_policy` and `create_time_partitions` check, and name the table in
the error:

- it is a partitioned table (`relkind = 'p'`), not a temporary table;
- it is partitioned by `RANGE`, on one column, not an expression, of type
  `date`, `timestamp` or `timestamptz` (not a domain over them);
- the caller is the owner of the table, a role that has the owner's privileges
  (a member that inherits them), or a superuser;
- it has no `DEFAULT` partition (see "Default partition").

Partitions that already exist may have any bounds. A partition whose bounds are
not on the grid is not one the policy recognises as its own: it is reported as
`misaligned` (in the run's actions and by `timepart_check`) and is never
converted, dropped or detached by a run, however old it is. It stays until its
owner drops it, or until `drop_old_time_partitions` is called by hand, which
takes any partition.

## The grid of bounds

`partition_every` is one of:

| Interval | Cells |
|---|---|
| `N hours`, with `24 mod N = 0` | absolute lengths of N hours, counted from 2000-01-01 00:00 in the time zone of the policy; a daylight-saving change never changes their length |
| `N days` | calendar days of the zone, N at a time, counted from 2000-01-01 (a day of a change is 23 or 25 hours long) |
| `1 week` (or `7 days`) | calendar weeks of the zone, starting on Monday |
| `N months`, with `12 mod N = 0` | calendar months of the zone, starting on the first |
| `N years` | calendar years of the zone, starting on 1 January, N at a time counted from 2000 |

Anything else (`5 hours`, `1 day 1 hour`, `5 months`, a negative or zero
interval) is refused. At most 3660 days or 1000 years. `24 hours` is an
absolute 24 hours; `1 day` follows the calendar. `7 days` is the week: it starts
on Monday, not on 2000-01-01.

Every cell ends exactly where the next one starts (both come from the same
function), so cells never overlap and never leave a gap. Cell starts are local
midnights of the zone converted to instants; where the zone has no such local
time (a change at midnight) the cell starts at the first instant after the gap.
A zone that skipped a whole calendar day (Pacific/Apia on 2011-12-30,
Pacific/Fakaofo the same day, Pacific/Kwajalein on 1993-08-21,
Pacific/Kiritimati on 1994-12-31) has a cell for that day that would start and
end at one instant: the grid skips it, no empty partition is made and its name
is not used twice; the cells before and after it meet at one instant.
**Time zone.** The default is `UTC`, not the session's `TimeZone`: the bounds
do not depend on who runs the maintenance. The zone must be a name from
`pg_timezone_names`. For a `timestamptz` key the zone decides the bounds. For a
`timestamp` or `date` key the values are wall-clock values: the bounds are
computed on the wall clock without a daylight-saving change, and the zone is
used only to read "now" (`as_of`) as a wall-clock time. A `date` key accepts no
`hours` interval.

**Names.** A partition is named `<table>_p<suffix>`, in the schema of the table.
The suffix is `YYYYMMDDHH24` for hours (always in UTC, so that two cells with
the same local hour at a daylight-saving change get different names),
`YYYYMMDD` for days and weeks (the first day, in the zone), `YYYYMM` for months
and `YYYY` for years. The table's name is cut, character by character, until
the whole name is at most 63 bytes, with the suffix kept whole: a table named
with 63 characters gets partitions named from its first 53 for daily ones and 51
for hourly ones. Two tables whose names differ only in the cut part get the
same partition names; the second one's action fails with `name_conflict`, and
nothing is renamed or overwritten.

## SQL interface

### Tables and view

The data is in three base tables, `kingres.timepart_policy_data`,
`kingres.timepart_run_data` and `kingres.timepart_run_action_data`, owned by
the extension owner, which grant nothing to `PUBLIC` and have no row-level
security. They are dumped with the extension's configuration (`pg_dump`
carries the policies and the history; the sequence behind `run_id` too; the
lock table is not dumped). Roles use the views `kingres.timepart_policy`,
`kingres.timepart_run` and `kingres.timepart_run_action`, which are what the
rest of this document calls "the policy table" and "the history tables".
Every role with USAGE on the schema may read the views, and may write
`timepart_policy`, but a view shows it, and lets it change, only the rows of
tables it owns, judged by `current_user` (a superuser sees every row). See
"Security" for why views and not row-level security. A role that writes a
policy row directly gets the same checks as `timepart_set_policy`, and a
trigger pins what only the extension decides: `parent_oid`, `created_by`, `created_at`, and
`table_owner` (renewed only when the policy is written again by a role that
owns the table now). The history tables cannot be written by anyone but the
extension. `kingres.timepart_lock` has one row per table, is private to the
extension owner, and is what a run locks.

`kingres.timepart_policy` has one row per table, keyed by the table's schema and
name (the columns `table_owner` and `created_by` are the names of the roles
in the catalog, so a role that was renamed shows its new name; they cannot be
written):

| Column | Meaning |
|---|---|
| `schema_name`, `table_name` | the table (a hint `parent_oid` is kept and refreshed; the table is found by name) |
| `partition_every` | the width of a partition |
| `timezone` | the zone of the grid; default `UTC` |
| `create_ahead` | how far ahead of the run time partitions are created; default 7 days |
| `retention` | partitions that end at or before `as_of - retention` are retired; null keeps everything |
| `retention_action` | `drop` (default) or `detach` |
| `columnar_after`, `columnar_sort`, `columnar_options` | conversion to columnar storage: partitions that end at or before `as_of - columnar_after` are converted, ordered by `columnar_sort` (default: the partition column), with the options (`compression`, `compression_level`, `stripe_row_count`, `chunk_row_count`); see "Conversion" |
| `run_every` | the interval between runs; default 1 hour |
| `enabled` | disabled policies are not reported by `timepart_due` |
| `next_run_at` | set by `timepart_finish_run` |
| `table_owner` | the owner of the table when the policy was set; a run fails with `owner_changed` if the owner of the table is not that role (the role, not its name: see "Identity") |
| `created_by`, `created_at`, `updated_at` | who set the policy first, and when |

`kingres.timepart_run` has one row per run (`run_id`, the table's schema and
name, `table_oid`, `table_owner`, `as_of`, `trigger` `schedule`, `manual` or
`sql`, `state` `running`, `done`, `partial` or `failed`, `started_at`,
`finished_at`, `started_by`, `error`, and the `backend_pid` and `backend_start`
of the session that began the run). `table_oid` is the table the run was made
for, and is null once that table has been dropped; `table_owner` is the role
that owned the table when the run began (the role that may see the run, see
"Who sees the history"); `started_by` is the session user that began it. The
schema and the name of the table follow a rename of the table (not a rename of
its schema).
`kingres.timepart_run_action` has one row per action of a run (`run_id`, `seq`,
`kind` `create`, `convert`, `drop`, `detach` or `misaligned`, the partition's
schema and name, its bounds, `state` `done`, `failed` or `skipped`, `detail`,
`bytes_before` for drops and detaches, times). Runs are kept for 90 days after
the time of the newest and at most 1000 per table (`timepart_prune_runs`, called
at the end of every run).

### Identity: which table and which role a row means

A policy or a run names a table by its schema and name and a role by its name,
which is what a backup carries, and keeps the OIDs of both, which a rename does
not change and a backup does not carry: another cluster has other role OIDs,
another database other table OIDs, and an OID there can be the one of an
unrelated object. The rows of the policy and of the history therefore carry a
**stamp**: a random value made by `CREATE EXTENSION` (the function
`kingres.timepart__stamp()` returns it), which is different in every
installation and is never in a dump, because it is in a function and not in
the data.

- A history row with the stamp of this installation was written here; its OIDs are
  its identity. A role that is renamed is the same role (`ALTER ROLE ... RENAME`
  changes nothing: no run fails with `owner_changed`, no run is hidden), a table
  that is renamed or moved to another schema is the same table, a role that was
  dropped is gone for good, and a table or role made later with the same name
  is another one. (OIDs wrap around after 2^32 objects; a role dropped and a
  role made after the wrap with the same OID would be taken for each other:
  that is not defended against.)
- A history row with another stamp was restored from a backup; its OIDs mean
  nothing, and its names are its identity: a role is the role of that name, a run's
  table is the table of that schema and name that the recorded owner owns. The
  next `timepart_due` or `timepart_begin_run` for the table (or
  `kingres.timepart_adopt()`) gives such rows the OIDs of this cluster and the
  stamp of this installation, so a role that is renamed after that is followed.
  A row whose owner role does not exist in this cluster stays as it is: only a
  superuser sees it.
- **A policy row is not trusted by its stamp.** The stamp is readable by every
  role (it is in the source of a function) and a row is data, so a stamp in a row
  proves nothing: a policy row shaped by hand, with the real schema, table and
  owner of a table and the stamp of this installation, made the maintenance act
  on a table whose owner never set a policy (review K12T-03; on the earlier code
  it dropped the ten old partitions of the victim). The maintenance acts only on
  a policy that has a **seal**: a row of `kingres.timepart_policy_seal` with a
  fingerprint of the fields that decide what is done (the intervals, the time
  zone, the retention and its action, the columnar fields, `enabled`, the
  owner). The seal is written by the interface only: `timepart_set_policy`, a
  write by the owner through the view `timepart_policy` after its checks, and
  `timepart_adopt`. It is not in the dump (the table is not registered with
  `pg_extension_config_dump`), so a restore starts without seals; it goes with
  the policy (`DELETE`, `DROP TABLE`, `TRUNCATE`) and moves with a rename. A
  field of the policy changed by hand (`UPDATE` of the table by a superuser)
  breaks the seal; the schedule and the OID hint do not. A policy without a
  seal is inert: `timepart_due` does not list it, `timepart_begin_run` records a
  failed run with `policy_unverified`, `timepart_run_action` does nothing, and
  `timepart_check` reports `policy_unverified`.
- **Adoption** (`timepart_adopt(own_stamp boolean default false)`, superuser,
  explicit: nothing calls it for policies) seals a policy row only if the table
  of that name exists and is partitioned, the role the row names as owner is the
  role that owns the table now, the values pass the checks of
  `timepart_set_policy`, and the row does not carry the stamp of this
  installation (a restore brings the stamp of the installation that made the
  backup; a row with ours that is not sealed was not written by the interface,
  and is refused with a `WARNING` that says to set the policy again, unless the
  caller passes `true` and says that it knows the row). It says `NOTICE: policy
  of s.t adopted: ...` for every row it adopts and `WARNING` for every one it
  refuses; read them after a restore.
- **What a hostile dump can and cannot cause.** It cannot make anything act by
  itself: the policy rows of a dump are inert until a superuser calls
  `timepart_adopt()`; it cannot attach a policy to a table whose owner is not the
  role it names, to a table that does not exist, to a role that does not exist
  or to a superuser who does not own the table; it cannot get values past the
  checks; the seal cannot be in a dump. It can, once the superuser adopts what
  the dump brought, give a table a policy of the dump's choosing if the dump
  names the table and its real owner (that is indistinguishable from a backup
  in which that owner's table had that policy): with the shortest retention
  that the checks allow, the next run drops the old partitions. So read what
  `timepart_adopt()` says, and `SELECT * FROM kingres.timepart_policy` before it
  (a superuser sees the rows), when the dump is not your own. It can replace
  the policy of a table by a row of another content (a restore over live rows),
  which then is inert: the table is not maintained until its policy is set
  again. A superuser who restores arbitrary SQL has lost anyway.
  History rows that a dump brings are only information: a run row names an
  owner and is shown to that role (a forged one can show a role a run that did
  not happen, and cannot make anything act); a role that does not exist or a
  superuser is seen by superusers only; a row that says `running` for a backend
  that is not there is marked `interrupted` by the next run (tested in
  `timepart_forged`).
- The event trigger that follows a rename or drops the policy of a dropped
  table acts only on rows with the stamp of this installation, so the OID in a
  restored policy, which may be the OID of an unrelated table, cannot make
  that table's rename move the policy.

### Who sees the history

The rule: a run belongs to the table it was made for and to the role that owned
that table when the run began. A role sees a run when it is that role, or a
member of it that inherits its privileges (`pg_has_role(..., 'USAGE')`), or a
superuser. `timepart_run_action` follows its run, and so does every function that
reads the history (`timepart_partitions`, `timepart_check`, `timepart_policies`).

| Case | Who sees the earlier runs |
|---|---|
| the table is dropped | the owner it had (and nobody else but superusers); `table_oid` is null |
| another role, or the same one, makes a table of the same name | the new table has no history; the old runs stay with the old table and its owner; the list of detached partitions of `timepart_partitions` is the table's own |
| the table or its schema is renamed or moved | the owner, as before (the schema name in the rows follows the table's rename, not a rename of the schema) |
| a role is renamed | the same role |
| `ALTER TABLE ... OWNER TO` | the runs made while the old owner owned the table stay with the old owner; the new owner sees only the runs after the change, starting with the run that fails with `owner_changed` |
| the owner role is dropped | only superusers, also when a role of the same name is made later |
| a dump and restore, by a superuser or by a role with `pg_read_all_data` | the same as before the dump (see "Identity") |

The history of a table is never moved to the new owner by an `OWNER TO`: it
records who was responsible when it was made, and a new owner does not learn
what the old one saw.

`kingres.timepart_policies` is the policy with the state of the latest run of
the table.

### Functions and procedures

Callable by the owner of the table (when EXECUTE has been granted):

| Signature | Meaning |
|---|---|
| `kingres.create_time_partitions(rel regclass, partition_every interval, end_at timestamptz, start_from timestamptz default now(), access_method name default null, timezone text default 'UTC') returns integer` | Create every missing partition from the one that holds `start_from` to the one that holds `end_at`, inclusive. Returns the number created. All or nothing (one transaction). `access_method` null means the access method of the parent. |
| `kingres.drop_old_time_partitions(rel regclass, older_than timestamptz, detach_only boolean default false, timezone text default 'UTC') returns integer` | Drop (or only detach) the partitions whose upper bound is at or before `older_than`. Returns the number. |
| `kingres.timepart_set_policy(rel regclass, partition_every interval, create_ahead interval default '7 days', retention interval default null, retention_action text default 'drop', columnar_after interval default null, columnar_sort text[] default null, columnar_options jsonb default null, timezone text default 'UTC', run_every interval default '1 hour', enabled boolean default true) returns void` | Create or replace the policy of a table. Does not create partitions; the next run does. `retention` must be at least one `partition_every`; `columnar_after` must not be longer than `retention`; `columnar_options` is a JSON object with the keys `compression` (`none`, `pglz`, `lz4`, `zstd`, `auto`), `compression_level` (1 to 19), `stripe_row_count` (1000 to 10000000) and `chunk_row_count` (100 to 100000), the last three whole numbers; any other key is an error. |
| `kingres.timepart_unset_policy(rel regclass) returns void` | Remove the policy. The history stays. |
| `kingres.timepart_plan(rel regclass, as_of timestamptz default now()) returns table (seq integer, kind text, partition_name name, range_from timestamptz, range_to timestamptz, reason text)` | The actions a run would take at `as_of`, without taking any. A function of the catalog and `as_of`. |
| `kingres.timepart_partitions(rel regclass) returns table (partition regclass, range_from timestamptz, range_to timestamptz, access_method name, size_bytes bigint, row_estimate bigint, compression_ratio float8, attached boolean)` | Partitions with bounds, size and format; also the partitions that a `detach` retention detached and kept, with `attached = false`. Needs ownership or `SELECT`. |
| `kingres.timepart_check(min_ahead interval default null, as_of timestamptz default now()) returns table (schema_name name, table_name name, problem text, detail text)` | `ahead_low` (the last partition ends before `as_of + min_ahead`, default `create_ahead / 2`), `last_run_failed` (the last finished run is `failed` or `partial`), `retire_blocked` (that run could not drop or detach the partitions named, for example because a foreign key references them or a view depends on them), `convert_blocked` (that run could not convert the partitions named: refused, failed on a lock, or `kingres_columnar` is not installed; the detail is the reason), `policy_orphan` (superusers only: the table is gone or no longer suitable), `default_partition_present`, `misaligned` (partitions the policy does not recognise, which a run never retires), `lock_timeout` (the partitions could not be read within the bounded wait) and `check_failed` (any other error while checking one table). A role sees the tables it owns. |

Superuser only (the scheduler and `psql` of an administrator):

| Signature | Meaning |
|---|---|
| `CALL kingres.timepart_run(rel regclass, as_of timestamptz default now(), phases text[] default '{create,convert,retire}', INOUT run_id bigint default null)` | `timepart_begin_run`, the phases in the order create, convert, retire, and `timepart_finish_run`. The id of the run comes back in `run_id`. It commits between actions, so it cannot run inside a transaction block. |
| `kingres.timepart_begin_run(rel regclass, as_of timestamptz default now(), trigger text default 'sql') returns bigint` | Record a run in state `running` and claim the table: a row lock (`FOR UPDATE NOWAIT`) on the table's row of `kingres.timepart_lock`, which serialises concurrent begins, and the run row, which records the backend (pid and start time). Raises `55P03` when a run of the table is `running` and its backend is alive. A `running` run whose backend is gone is marked `failed` with error `interrupted`. A run that cannot start (the owner changed, the table is no longer suitable) is recorded as `failed` at once. |
| `kingres.timepart_run_action(run_id bigint, phase text) returns boolean` | Plan again from the catalog at the run's `as_of`, execute the next action of the phase that this run has not tried, record it, and return true; false when nothing is left. One transaction of the caller, with a `NOWAIT` row lock on the table's lock row, so two steps of a table never overlap. See "Time limit of an action". |
| `CALL kingres.timepart_run_phase(run_id bigint, phase text)` | `timepart_run_action` in a loop with a `COMMIT` after each call. Idempotent: it re-plans from the catalog at the run's `as_of` and does what is left. |
| `kingres.timepart_finish_run(run_id bigint) returns void` | Final state, `next_run_at`, pruning. Calling it again does nothing more. |
| `kingres.timepart_action_timeout() returns integer` | The `statement_timeout`, in milliseconds, that a driver sets before it calls `timepart_run_action` (setting `kingres_timepart.action_timeout_ms`, default 300000). |
| `kingres.timepart_due(as_of_time timestamptz default now()) returns table (schema_name name, table_name name, as_of timestamptz)` | Enabled, sealed policies whose `next_run_at` is at or before the time, oldest first: all of them for a superuser (after adopting the history of a restore), the caller's own tables for another role. |
| `kingres.timepart_prune_runs(schema_name name, table_name name, as_of timestamptz default now()) returns bigint` | Delete old runs of a table; returns the number. |
| `kingres.timepart_adopt(own_stamp boolean default false) returns bigint` | Superuser. Adopt the policies (after the checks of the catalog, see "Identity") and the history that a backup carried here. |
| `kingres.timepart_grant_driver(role name)`, `kingres.timepart_revoke_driver(role name)` | Superuser. Give a role, or take from it, the EXECUTE to drive the maintenance of its own tables from its own session. |

`timepart_set_policy`, `timepart_unset_policy`, `timepart_plan`, `timepart_partitions` and
`timepart_check` are `SECURITY INVOKER`: they judge `current_user`. The functions that need
`GRANT EXECUTE` are the ones listed under "Install".

The conversion procedures, callable by the owner of the table (when EXECUTE has
been granted, see "Install") and by a superuser. Both commit, so neither can run
inside a transaction block (the error is `invalid transaction termination`):

| Signature | Meaning |
|---|---|
| `CALL kingres.convert_partition(parent regclass, part regclass, access_method name default 'columnar', sort_by text[] default null, options jsonb default null)` | Replace the leaf partition `part` of `parent` by a copy stored with `access_method` (`columnar`, or `heap` to convert back), in one transaction; see "Conversion". `sort_by` names the columns the copy is ordered by (default for columnar: the partition column; for heap: the order of the scan); `options` are the options of `kingres.columnar_set_options`, as in the policy (columnar only). |
| `CALL kingres.convert_old_partitions(parent regclass, older_than timestamptz, access_method name default 'columnar', sort_by text[] default null, options jsonb default null)` | `convert_partition` for every partition whose upper bound is at or before `older_than` and whose access method differs, oldest first, one transaction each. The first one that is refused or fails ends the call with its error; those before it stay converted. A `timestamp` or `date` key is read as UTC wall-clock time, as in `drop_old_time_partitions` with its default zone. |

Without `kingres_columnar` both raise an error that names it (a conversion to
heap needs it too: only a columnar partition can be converted to heap).

The helpers `timepart_interval_parts`, `timepart_cell_start`, `timepart_cell_of`,
`timepart_part_name`, `timepart_create_one`, `timepart_retire_one` and the
functions whose names start with `timepart__` are internal. The first four are
pure; the other two execute the DDL with the caller's own rights.

## What a run does, step by step

`CALL kingres.timepart_run('t', as_of)` does this. "Now" is `as_of`; nothing in
the run reads the clock except the default of that argument.

1. **Begin.** Lock the table's row of `kingres.timepart_lock` without waiting
   (`55P03` if another begin holds it). If a run of the table is `running` and
   its backend is still alive, raise `55P03`. Mark the other runs of the table
   that are still `running` as `failed` with error `interrupted`. Check that the table
   is still suitable and that its owner is the one recorded in the policy
   (`owner_changed` otherwise); on failure record the run as `failed`. Insert
   the run row in state `running` and commit.
2. **Plan, per action.** Every action re-plans from the catalog (see below) and
   takes the first action of the phase that this run has not tried yet. Each
   action is its own transaction. This is why the phases are idempotent: after
   an interruption, or when a phase is repeated, the plan has only what is left.
3. **Create phase.**
   - A partition whose bounds are not on the grid is recorded as `misaligned`,
     state `skipped`, and left alone: it is not the policy's own, and the
     retire and convert phases skip it too.
   - The cells to create: from the cell of the first existing partition (or of
     `as_of` when the table has none) to the last cell whose start is before
     `as_of + create_ahead`; the cell of `as_of` is always included. A cell that
     overlaps an existing partition (the same bounds under another name, a wider
     partition, a partition made by hand with other bounds) is not created.
     Cells that end at or before `as_of - retention` are not created, since the
     retire phase would drop them at once. At most 10000 cells in a plan.
   - For each cell: `CREATE TABLE <schema>.<name> (LIKE <parent> INCLUDING
     DEFAULTS INCLUDING CONSTRAINTS INCLUDING INDEXES INCLUDING STORAGE
     INCLUDING COMMENTS INCLUDING GENERATED INCLUDING COMPRESSION) [USING
     <access method of the parent>]`, then `ALTER TABLE <parent> ATTACH PARTITION
     ... FOR VALUES FROM (...) TO (...)`, in one transaction, as the owner of the
     table (`SET LOCAL ROLE`). If a relation of that name exists, the action fails
     with `name_conflict`. Indexes and foreign keys of the parent are matched or
     cloned by the attach; triggers are cloned.
4. **Convert phase.** The partitions of the grid whose upper bound is at or
   before `as_of - columnar_after` and that are not about to be retired in
   the same run, oldest first, one conversion per action; see "Conversion".
   A conversion that was done is recorded as `converted to columnar` with the
   rows, the size before and after (`bytes_before`, `bytes_after`) and the
   time it took.
5. **Retire phase.** Partitions whose upper bound is at or before
   `as_of - retention` (the default partition, a partition with an unbounded
   upper bound and a partition with bounds off the grid are never retired): `ALTER TABLE <parent> DETACH PARTITION <p>`
   and, for `drop`, `DROP TABLE <p>`, in one transaction, as the owner. A
   `detach` keeps the table, with its rows and its name. `bytes_before` records
   the size of what was dropped. A partition that spans the boundary stays until
   its upper bound passes it, so data may be kept up to one `partition_every`
   longer than `retention`. Nothing deletes rows.
6. **Finish.** State `done`, `partial` when an action failed, `failed` when the
   run itself failed. `next_run_at = as_of + run_every`, or `as_of + least(run_every,
   5 minutes)` when an action failed on a lock timeout. Prune old runs.

An action that fails (a name taken, a lock timeout, a view that depends on the
partition to drop, a foreign key that references the partition to drop, a role
that lacks the privilege, a statement timeout) is recorded as `failed` with
`SQLSTATE: message` as its detail, does not stop the run and is not tried again
in the same run; the next run tries again. A partition that a foreign key
references is never retired while a referencing row exists: every run of the
table is `partial` until then, and `timepart_check` names the partition
(`retire_blocked`).

## Locks

| Step | Lock on the parent | Waits |
|---|---|---|
| create, `LIKE` | `ACCESS SHARE` on the parent | bounded |
| create, `ATTACH PARTITION` | `SHARE UPDATE EXCLUSIVE` on the parent; `ACCESS EXCLUSIVE` on the new table | bounded |
| create, `ATTACH PARTITION` of a table with foreign keys | `SHARE ROW EXCLUSIVE` on every table the parent references (PostgreSQL's rule), which conflicts with every `INSERT`, `UPDATE` and `DELETE` of that table | bounded, and writers of the referenced table queue behind it while it waits |
| retire, `DETACH PARTITION` and `DROP` | `ACCESS EXCLUSIVE` on the parent and on the partition | bounded |

Creation does not block readers or writers of the parent: `CREATE TABLE ...
PARTITION OF` would take `ACCESS EXCLUSIVE` on the parent, `LIKE` plus `ATTACH`
does not. It does take `SHARE ROW EXCLUSIVE` on the tables that the parent has
a foreign key to, and that blocks their writers for as long as the attach waits
or works; for a table with foreign keys keep `kingres_timepart.create_lock_timeout_ms`
low (the default is 5 seconds, with five attempts). Two sessions that create
partitions of one table block each other; the claim of a run prevents it. A retirement waits for running statements on
the parent and, while it waits, queues new ones behind it, for at most the
timeout of the attempt.

A conversion takes the locks of its own table, see "Conversion".

Reading the bounds of the partitions takes `ACCESS SHARE` on them (the deparse of
a partition bound opens the relation), so a transaction that holds `ACCESS EXCLUSIVE` on
the table makes a run fail as a whole after the lock timeout: the run is `failed`, its
error starts with `55P03`, nothing is changed, and the next run is due within 5
minutes. A lock that conflicts only with the attach or the detach (for example
`SHARE UPDATE EXCLUSIVE`, held by a concurrent `VACUUM` or `ANALYZE` of the parent) fails
the create actions one by one, with retries, and the run is `partial`.

`timepart_plan`, `timepart_partitions`, `timepart_check` and `timepart_begin_run` bound their reads the same way.

All waits are `lock_timeout`s, set by the extension for the statements of an
action and put back afterwards. They are driven by settings, which a test or an
operator can set per session, per role, per database or in `postgresql.conf`
(they are plain integers):

| Setting | Default | Range | Meaning |
|---|---|---|---|
| `kingres_timepart.create_lock_timeout_ms` | 5000 | 1 to 60000 | `lock_timeout` of the statements that create a partition |
| `kingres_timepart.retire_lock_timeout_ms` | 2000 | 1 to 60000 | `lock_timeout` of the statements that retire one |
| `kingres_timepart.lock_retries` | 5 | 1 to 20 | attempts per action in a run (not in the functions called by hand, which make one) |
| `kingres_timepart.retry_sleep_min_ms` | 200 | 0 to 60000 | the sleep between attempts is a random time between these two |
| `kingres_timepart.retry_sleep_max_ms` | 2000 | 0 to 60000 | |
| `kingres_timepart.action_timeout_ms` | 300000 | 1 to 86400000 | the `statement_timeout` a driver sets for one action (see "Time limit of an action") |

A value out of range is an error, not a silent clamp. After the last attempt the
action is recorded as `failed` with detail `lock_timeout: ... (attempt N of N)`.
`DETACH PARTITION ... CONCURRENTLY` is not used: it cannot run inside a
function or procedure (checked by the test `timepart_retention`).

## Security

- **Identity.** Every function judges `current_user`, the role whose
  privileges PostgreSQL applies, never the session user. Code of a role that
  runs in a session of a superuser (a `SECURITY DEFINER` function that the role
  owns) is judged as that role. (Code that runs with the caller's own
  identity, such as an ordinary function or a view that a superuser calls, runs
  as the superuser: that is PostgreSQL's rule, not something an extension can
  change.) `SECURITY DEFINER` is used by the two event
  trigger functions, the functions that write the history and the seal (see
  below), and `timepart__special_table`, which reads the private catalogs of
  `kingres_ledger` and `kingres_shard` for the conversion and returns a word;
  none of them takes a decision about a caller except the ones that check it
  as described below. `timepart__special_table` answers only for a table of the
  login role (`session_user`): a superuser, or a role that has the privileges of
  the table's owner; any other role gets `permission denied`, so the function
  cannot be used to ask the private catalogs about a table that is not
  yours. The other helpers that `PUBLIC` may execute read `pg_catalog` only.
- **Policy and history tables.** The base tables (`*_data`) grant nothing to
  `PUBLIC` and have no row-level security; roles reach them through
  `security_barrier` views owned by the extension owner, whose condition
  (`timepart__visible`) shows a role the rows of the tables it owns, by
  `current_user`. PostgreSQL checks the privileges on the base table against the
  owner of the view, while a function in the view's condition runs as the caller,
  so `current_user` is the role whose privileges apply, also inside a
  `SECURITY DEFINER` function of another role (tested). The views are not
  `security_invoker` (the caller has no right on the base tables).
  `security_barrier` keeps a function that the caller puts in a `WHERE` clause
  from seeing a row before the condition has hidden it (tested with a function
  that reports what it sees), and `WITH CASCADED CHECK OPTION` on the policy
  view refuses a write that would make a row the view does not show. A role can
  write the policy row of its own table directly (through the view), and then a trigger applies the same checks as
  `timepart_set_policy` and pins `parent_oid`, `created_by`, `created_at` and
  `table_owner`, so a role cannot make a policy for a table it does not own or
  put another table's OID in the hint. A superuser (the extension's own code, a
  restore) is trusted. The history views are read-only for roles.
- **Why views and not row-level security.** `pg_dump` runs with
  `row_security = off`, and PostgreSQL allows that only to a superuser, to the
  owner of the table and to a role with `BYPASSRLS`. A role that reads every
  table through `pg_read_all_data` is none of them, so with row-level security
  on the policy table the dump of the whole database by a least-privilege backup
  role stops with "query would be affected by row-level security policy". The
  base tables have no row-level security, so that role reads them as it reads
  any table. What the backup role may do is stated in "Dump, restore, rename,
  drop".
- **DDL on user tables** (`create_time_partitions`, `drop_old_time_partitions`,
  `timepart_create_one`, `timepart_retire_one`) is `SECURITY INVOKER`: PostgreSQL
  checks every statement against the caller. They also check ownership first,
  for a clear error.
- **A run executes the DDL as the owner of the table, where the owner's own
  code cannot leave that role.** The new tables are the owner's, and the owner
  needs `CREATE` on the schema. The statements are built by the extension, as
  the extension owner, before the switch; the owner role needs no right on the
  `kingres` schema. The switch is not `SET ROLE`, and the reason is the
  finding of the review of the second fix round (the test `timepart_privilege`): DDL makes
  PostgreSQL run code that the table owner wrote. An index whose expression
  calls a function with a constant argument is evaluated again when a partition
  is attached, in an ordinary context, and a default, a check constraint, a
  generated column or a trigger can call the owner's code when rows are
  written. A run is made by a superuser. After `SET ROLE owner` in that session
  the owner's function ran as the owner but could say `RESET ROLE`, or call
  `set_config('role', 'none', false)` from a function declared `IMMUTABLE`
  that calls a `VOLATILE` one (the declaration is not checked), and the rest
  of the statement ran as the superuser: an escalation of privilege (seen on
  PostgreSQL 18 and 19). PostgreSQL's own maintenance commands (`REINDEX`,
  `CLUSTER`, `VACUUM`, `REFRESH MATERIALIZED VIEW`) run such code with the
  table owner's user id and a security-restricted flag that no SQL can set. The
  one thing SQL has is the frame of a `SECURITY DEFINER` function: inside it
  `SET ROLE`, `RESET ROLE`, `set_config('role', ...)` and `SET SESSION
  AUTHORIZATION` are refused (`cannot set parameter "role" within
  security-definer function`). That is all the frame of a `SECURITY DEFINER`
  function (`timepart__exec_as`) does, and it is **not isolation**: the owner's
  code runs inside the superuser's session and transaction, and was tested to
  leave behind, there, an object in the temporary schema (a view named like a
  catalog view, which the superuser's next `SELECT FROM pg_roles` runs the
  owner's function through, as the superuser: review K12T-02), a cursor `WITH
  HOLD` (its query runs at commit as the superuser), a prepared statement, a
  setting changed with `set_config(..., false)` (for example `statement_timeout`),
  a session level advisory lock and a `LISTEN`. (A deferred trigger fires as the
  owner, on both majors.) No cleanup from SQL is complete: a `LISTEN` pending or a
  cached plan cannot be seen, and a setting of the owner cannot be told from the
  superuser's own. So there are two ways, and the extension offers no third:
  - **The owner drives its own table, from its own session** (isolated: its
    code runs there, as it, with its own rights, and what it leaves is in its
    own session). `kingres.timepart_grant_driver('role')` gives a role the
    EXECUTE it needs (`timepart_revoke_driver` takes it back); the role then
    calls `CALL kingres.timepart_run('s.t')`, or `timepart_due`,
    `timepart_begin_run`, `timepart_run_phase`/`timepart_run_action` and
    `timepart_finish_run` step by step, for the tables it owns. The functions
    that write the history for it are `SECURITY DEFINER` and judge the caller by
    `session_user` and by the role the invoking function runs as, which must both
    be a superuser or the owner of the table; a role that was switched to with
    `SET ROLE` in a superuser's session is judged as the role it set, and its
    action is refused (it is a superuser's session). Such a driver sees in
    `timepart_due` the policies of its own tables only and changes nothing.
    The history of a table that its owner drives is only as true as the owner:
    it can write the history of its own table.
  - **A superuser drives it in its own session**, which runs the owner's code
    there. That is refused for a table whose owner is not a superuser (a
    superuser's own code is trusted as the session's), with a run that fails
    `owner_code_not_trusted`, unless the session says
    `SET kingres_timepart.trust_table_owners = on` (or the server does, in
    `postgresql.conf`), which means: the owners of the tables are trusted as
    the superuser is. With it, everything listed above is possible, so use it
    only for owners that you trust, and drive the rest as themselves or with
    one connection per owner that is that owner. (`dblink` from inside needs a
    password that the server asks for, `dblink_connect_u` and `COPY ...
    PROGRAM` need rights that the role does not have; tested in
    `004_owner_and_races.pl`.) The frame uses a name of its own every time, so
    a leftover function of the owner in the temporary schema cannot stop later
    actions of other tables; before, one named `timepart_exec` did (any table's
    maintenance in that session failed with "already exists").
  `create_time_partitions`, `drop_old_time_partitions`, `timepart_create_one` and
  `timepart_retire_one` are `SECURITY INVOKER` and run the DDL as whoever calls
  them: a superuser who calls them for another role's table runs that role's
  code as the superuser, exactly as a superuser's `ALTER TABLE ... ATTACH
  PARTITION` does; that is PostgreSQL's rule, and the reason `timepart_run`,
  not these functions, is what a scheduler calls.
- **A run does execute user code, as the owner, and with no limit of its own.**
  The owner's code that runs is the code described above (an index expression
  at an attach; a default, a generated column or a trigger when rows are
  written, which a run does not do; the check constraints and defaults are
  copied, not run). It runs as the owner and cannot leave that role. Nothing in
  the extension limits how long it runs: a table owner who can make a function
  run for as long as it likes can keep a driver's session busy: see "Time limit
  of an action", and do not run the maintenance of tables of untrusted owners
  in a session that other work waits for. Event triggers that a superuser created
  in the database do run.
- **Search path.** Every function has `SET search_path = pg_catalog, kingres,
  pg_temp`. The two procedures that commit cannot carry a `SET` clause; their
  first statement, and the first statement after every `COMMIT`, sets the path
  of the transaction to `pg_catalog, pg_temp`, every function they call is
  schema-qualified and the one operator they use is spelled
  `OPERATOR(pg_catalog.=)`. This holds for any search path of the caller,
  including one that lists a writable schema first; the test `timepart_hostile`
  attacks every function and procedure with a schema that holds a look-alike of
  every function the bodies call.
- **Claims.** A run is claimed by a row lock on `kingres.timepart_lock` and by
  its run row (with the pid and start time of its backend); no other role can
  take that lock, so no role can stop the runs of a table by holding a lock whose
  key it can compute. (A role that owns a table can still hold a lock on its own
  policy row; a run that has to refresh the OID hint of that row then fails on
  the lock timeout, and `timepart_finish_run` leaves the policy due when it
  cannot update `next_run_at`; this slows down the maintenance of that role's
  own table only.)
- **Ownership of a partition is its own:** `ALTER TABLE parent OWNER TO` does not
  change the partitions. After an owner change the policy must be set again by
  the new owner, and the old partitions, owned by the old owner, cannot be
  dropped by the new one until their owner is changed too.
- **Who may call what:** `timepart_adopt`, `timepart_prune_runs`,
  `timepart_grant_driver` and `timepart_revoke_driver` are the superuser's
  alone. `timepart_run`, `timepart_run_phase`, `timepart_run_action`,
  `timepart_begin_run`, `timepart_finish_run` and `timepart_due` are the
  superuser's and, for the tables it owns, the owner's (see above); EXECUTE on
  all of them is revoked from `PUBLIC`. `timepart_due` is `VOLATILE` because a
  superuser's call adopts the history rows of a restore; any other caller's call
  reads only and lists the caller's own sealed policies.
- **The views.** The policy and history views are `security_barrier`; a
  function that a role puts in a `WHERE` clause over them does not see a row
  of another role's table (tested for the three views with a function that
  records what it is given, in `timepart_security`).
- **Events that race a run.** A rename, a drop or a change of owner of a table
  between two actions, or while one waits for the table, ends in a consistent
  state (tested in `004_owner_and_races.pl`): a rename moves the policy, its
  seal and the history and the run goes on by the new name; a drop removes the
  policy and its seal, the history stays without a table and the run fails; a
  change of owner fails the next action with `owner_changed`. The steps of an
  action take only a key share lock on the run row, so the event trigger of the
  DDL is not held up by a step that waits for the table (it was: a deadlock).
  Which of the two comes first is for the lock manager.
- **Quoting.** Every identifier goes into dynamic SQL with `format('%I')` and
  every literal with `%L`. Names of partitions are generated and always quoted.
  The test `timepart_names` runs tables, schemas, columns and a role with
  quotes, semicolons, comments, dots, upper case, newlines and 63-byte names
  through every statement.
- **Names of detached partitions** in `timepart_partitions` are shown only
  when the caller can read the table that now has that name and only to the
  owner of the table.

## Time limit of an action

A timer cannot be started from inside a running statement: `SET LOCAL
statement_timeout` in a function, or after a `COMMIT` in a procedure, does not
bound the statement that is running (checked on PostgreSQL 18). So `timepart_run`
and `timepart_run_phase`, which are one statement each, are bounded only by the
`statement_timeout` of the statement that calls them, and only the first action
that overruns it fails. A driver that wants a limit for every action issues each
action as a statement of its own after `SET statement_timeout =
kingres.timepart_action_timeout()` (default 5 minutes):

```sql
SET statement_timeout = <kingres.timepart_action_timeout()>;
SELECT kingres.timepart_run_action(<run_id>, 'create');   -- repeat until false
```

When the timeout strikes while an action runs, the action's sub-transaction is
rolled back (no partition is left half made), the action is recorded as `failed`
with detail `statement_timeout: ...`, the run goes on and ends `partial`, and the
next run tries again. A cancel by an operator is not turned into a failed action:
only a timeout is. The extension does not set the timeout itself; the scheduler of
the control plane does.

## Default partition

A table with a `DEFAULT` partition is refused by `timepart_set_policy` and
`create_time_partitions`: `ATTACH PARTITION` next to a default partition scans
the default partition under a strong lock to check that it holds no row of the
new range, and `DETACH ... CONCURRENTLY` is refused. If a default partition is
added after the policy was set, a run still tries: where the default partition
holds no row of a range to create, the attach succeeds; where it holds such a
row, PostgreSQL refuses the attach (`updated partition constraint for default
partition ... would be violated by some row`), that action is recorded as
`failed`, the other partitions are created, the run ends `partial`, the run does
not loop, and the next run tries again. `timepart_check` reports
`default_partition_present`. Move the rows out of the default partition (or
create the partitions earlier with a larger `create_ahead`) to clear it.

## Conversion

A partition is converted by copying it into a new table of the other access
method and swapping the new table in, in one transaction (design 10.4). It is
done by hand (`convert_partition`, `convert_old_partitions`) or by the policy:
when `columnar_after` is set, the convert phase of a run converts, oldest
first, every partition of the grid that ends at or before `as_of -
columnar_after`, with the policy's `columnar_sort` and `columnar_options`
(a partition that is also old enough to be retired is only retired). Converting
back is the same call with `access_method => 'heap'`; it is how a columnar
partition, which accepts no `UPDATE` or `DELETE`, is changed: convert it to
heap, change it, convert it again.

**What the new partition is.** The same table: the name, the bounds, the
columns and their defaults, comments and storage settings, the `CHECK` and `NOT
NULL` constraints (all validated: see "What is refused"), the stored generated
columns (recomputed), the owner, the table comment, the tablespace, the
replica identity `FULL` or `NOTHING`, all indexes with their names and
comments (the comments of primary key and unique constraints too), and the
following, which `CREATE TABLE ... (LIKE ...)` does not carry and the
conversion sets itself:

* the privileges on the table and on its columns, with `GRANT OPTION`, and the
  privileges that the owner took from itself (`REVOKE UPDATE ON t FROM
  owner` stays in force). Every privilege must have been granted by the owner:
  a partition with a grant that another role made with its `GRANT OPTION` is
  refused, because the new table would have the owner as the grantor and
  `REVOKE ... CASCADE` would no longer reach it. Default privileges of the
  schema (`ALTER DEFAULT PRIVILEGES`) are revoked from the new table;
* the enabled state of the triggers that the partition has from the table
  (`DISABLE TRIGGER`, `ENABLE REPLICA TRIGGER`, `ENABLE ALWAYS TRIGGER`): the
  attach clones them as enabled, and the old state is put back;
* the storage parameters of the table and of its TOAST table (`fillfactor`,
  `autovacuum_enabled`, `parallel_workers`, ...), set after the load and the
  indexes: they apply to what is written next, not to the pages of the copy. A
  parameter that the target access method does not accept is a `WARNING` and
  the others are carried. A columnar table has no TOAST table, so the TOAST
  parameters are not carried to it, and not back;
* the statistics target and the options (`n_distinct`) of each column, the
  statistics target of an index expression, and the index that the table is
  clustered on (`CLUSTER ON`, the marker only);
* Triggers of the parent are cloned by the attach, as for any partition.

The options (`columnar_options`) are set before the first row is
written; without them the new table takes the `kingres_columnar.*` settings of
the session that converts. Columnar options live in the table's metapage, so a
partition that is converted back and forth keeps none of them.

**The steps**, all in one transaction (a failure anywhere leaves the old
partition as it was, and no trace of the new one):

1. The checks (see "What is refused"), before any lock on the partition: a
   partition that cannot be converted is refused without waiting for anything.
2. `LOCK TABLE <partition> IN SHARE ROW EXCLUSIVE MODE`: the writers of the
   partition wait, its readers go on. (The design says `SHARE`; this mode has
   the same effect on readers and writers and also keeps out a plain `CREATE
   INDEX`, which `SHARE` lets in, and a second conversion.) **Everything the
   conversion reads from the catalog is read again now**, with the lock held:
   the checks, the columns, the indexes, the constraints, the name and the
   bound. The lock can take a while to be granted (a session that holds a lock
   on the partition may add a column, create an index, drop a column or rename
   the partition before it commits), and a statement built before the wait would
   copy the table as it was. What was read before the lock is only used to
   refuse early. If the partition was renamed or dropped while the conversion
   waited, the call fails with `schema_changed` (SQLSTATE `55P03`, retryable;
   for a policy run the action is `failed` and the next run is due within five
   minutes) and changes nothing.
3. `CREATE TABLE <partition>__kc (LIKE <partition> ...) USING <method>` with a
   `CHECK` of the bound in its definition, so that the attach below scans
   nothing; the options; the comment, replica identity, the statistics targets
   and options of the columns.
4. `INSERT INTO <partition>__kc (<every stored column>) SELECT ... FROM
   <partition> ORDER BY <sort_by>`: the columns are listed one by one, no
   default is evaluated, and it reads with a plain scan.
4b. The indexes: every index of the old partition (the parent's, and the
   partition's own), made on the new table **after the load** with a plain
   `CREATE INDEX`, or `ALTER TABLE ... ADD CONSTRAINT` for a primary key and a
   unique constraint, from the definition the server gives
   (`pg_get_indexdef`, `pg_get_constraintdef`), under temporary names; never
   `CONCURRENTLY`, never `CLUSTER`. No index entry is carried over: the rows have
   new numbers and the index is built from them. The attach in step 7 then finds
   an equal index for each index of the parent and builds nothing under the lock
   on the parent. After the old partition is dropped the indexes and
   constraints get its names back, with their comments. A unique index that
   finds a duplicate while it is built fails the conversion with the server's
   message (`could not create unique index ...`, with the key), and the old
   partition stays. Converting back to heap does the same. The temporary names
   are `kc_<12 characters of md5 of the partition name>_<n>`, so they are short
   and cannot collide whatever the name of the partition is. The storage
   parameters of the table are set after the indexes.
5. The check that the copy is whole: both tables have the same number of
   rows, or the transaction fails with `verification_failed`. (The design also
   compares the rows of `columnar_info`; a columnar table writes its stripes
   when the transaction commits, so they cannot be read before. After the
   commit the run records them: `N rows` in the action's detail.)
6. The privileges of the old partition on the new one.
7. The swap: `ACCESS EXCLUSIVE` on the parent, waited for at most 500 ms (or
   half of `deadlock_timeout` if that is less), then `DETACH PARTITION`,
   `ATTACH PARTITION`, `DROP TABLE` of the old partition, `RENAME` of the new
   one to the old name, drop of the helper `CHECK`, then the enabled state of
   the cloned triggers, the clustering index and the statistics targets of the
   indexes. A statement of the parent that runs at the swap sees the old
   partition or the new one, never neither and never both; **a transaction with
   an older snapshot is the exception**, see "Snapshots older than the swap".
8. The commit; then, in a transaction of its own, the result is written to the
   history (`bytes_after`, rows, time) and the new partition is analyzed
   (`ANALYZE`; a failure of it is a warning). A `VACUUM` of the new partition is
   not done here: PostgreSQL does not allow `VACUUM` inside a function or a
   procedure (`VACUUM cannot be executed from a function`), so a driver that wants
   one (the scheduler of the control plane) issues it as a statement of its own
   after the conversion, and autovacuum does it otherwise.

**Snapshots older than the swap.** The new partition is filled by the
conversion's own transaction, so its rows are newer than any snapshot taken
before the swap commits. A `REPEATABLE READ` or `SERIALIZABLE` transaction (or
a session that exported a snapshot) whose snapshot is older than the swap, and
that reads the table for the first time after the swap, **finds the converted
partition empty**: the catalog it reads is the current one, its rows are
those of the old snapshot, and the new rows are not in it. This is the
rule PostgreSQL states for `TRUNCATE` and for the table-rewriting forms of
`ALTER TABLE` ("not MVCC-safe", chapter "Concurrency Control"); no swap of one
table for another done in a transaction can give a better one. What is not
affected: a transaction that has already read the table (it holds `ACCESS
SHARE` on it, so the swap gives way: it waits at most 500 ms and fails with
`swap_lock_timeout`, see below), any statement at `READ COMMITTED` (its snapshot is
taken after it has the locks it needs, so a statement that waits for the swap
sees the new partition whole), and `pg_dump` once it has locked the table
(it locks every table within seconds of taking its snapshot). So a long
`REPEATABLE READ` or `SERIALIZABLE` transaction, a `pg_dump` that has not yet
locked the table, and a session that holds an exported snapshot must not
span a conversion of a partition they have not touched. The extension cannot
see such a snapshot (`pg_stat_activity` shows `backend_xmin`, not the
isolation level, and shows nothing about other roles' sessions to an owner),
and does not try to guess: `t/009_convert_ddl_race.pl` keeps the behaviour
under test, so that a change in it is noticed and this text is changed.

**Locks and how long a session waits.**

| Who | Waits for | How long |
|---|---|---|
| a reader of the partition or of the parent | nothing during the copy | never, except for the swap |
| a reader of the parent | the swap: `ACCESS EXCLUSIVE` on the parent is granted when the statements running now have ended; the swap itself takes a fraction of a second | for the length of the readers' statements that were running when the swap asked, at most 500 ms of the conversion's waiting; the readers that arrive after the request queue behind it for those 500 ms at most |
| a writer that names the partition (`INSERT INTO ev_p1`) | the copy (`ROW EXCLUSIVE` conflicts with `SHARE ROW EXCLUSIVE`) | as long as the copy: proportional to the size of the partition. After the commit it goes on against the new partition (PostgreSQL looks the name up again after a lock wait). The writer's own `lock_timeout` or `statement_timeout` applies |
| a writer through the parent (`INSERT INTO ev`) of a row of the partition | the copy, **and the conversion gives up**: the writer holds the parent in `ROW EXCLUSIVE` while it waits, so the swap cannot get its lock | until the swap finds it (it checks the lock table before it asks, and gives up at once with `swap_lock_timeout`), the conversion is rolled back, the writer proceeds against the old partition and its row is kept, and the next attempt converts it |
| the conversion | the partition lock: `kingres_timepart.create_lock_timeout_ms` (default 5 s); the parent: at most 500 ms | a lock that is not granted ends the attempt: for a policy run the action is `failed`, with `lock_timeout: ...` or `swap_lock_timeout: ...`, the run is `partial`, and the next run is due within five minutes. A conversion is tried once per run, not retried: its copy is the expensive part |

So a conversion runs best on a partition nobody writes any more, which is what
`columnar_after` means. A writer that arrives during the copy never loses a
row and is never made to fail by the conversion: it is the conversion that
gives way. With writers or long transactions on the parent at every attempt a
conversion does not get through; `timepart_check` reports `convert_blocked`
with the reason. Two conversions of one table do not overlap (the claim of a
run, and the lock on the partition for hand calls).

**What happens to the rows written during a conversion.** A row that a writer
commits before the conversion takes the lock is copied. A writer that names the
partition after that waits, and its row goes into the new partition. A writer
that goes through the parent makes the conversion give way, as above, and its
row goes into the old partition and is copied by the next attempt. The check of
the row counts, made under the lock, is what makes sure that no row of the old
partition is dropped uncopied. A prepared transaction (`PREPARE TRANSACTION`)
that holds a row of the partition keeps its lock until it is ended: the
conversion times out on the partition lock (a row of the partition) or on the
parent (a read of it), changes nothing, and works after `COMMIT PREPARED` or
`ROLLBACK PREPARED` (`t/008_convert_prepared.pl`).

**What is refused** (`conversion_refused: <reason>`, SQLSTATE `0A000`, naming
the object; for a policy run the action is `skipped` with the same text, the
run is not failed, and `timepart_check` reports `convert_blocked`). The
conversion drops the old partition, so whatever would be lost with it, or
cannot be on the new one, is a reason; nothing of the user's is dropped
silently:

* the target is not `columnar` or `heap` (another table access method, or a
  name that is none);
* the partition is not a partition of the table, is itself partitioned, is the
  default partition, is already stored with the target access method, is not
  permanent (temporary, unlogged), has no column that can be copied, or is owned
  by another role than the table;
* the transaction isolation is not `READ COMMITTED` (under a snapshot older
  than the lock, a row committed in between would be dropped uncopied);
* the table or the partition belongs to a ledger table (`kingres_ledger`) or to
  a distributed table (`kingres_shard`; distributed tables are for a later
  phase);
* the partition has a trigger of its own (a row-level or statement-level
  trigger that is not cloned from the parent), a rule, row-level security or a
  policy, extended statistics, an identity column, a sequence it owns, is in a
  publication, is used by a view or materialized view, or is referenced by a
  foreign key;
* the partition has a `CHECK` or `NOT NULL` constraint that is not validated
  (`NOT VALID`): the new table would have it validated, and a row that
  violates it would fail the copy, in every run; validate it or drop it;
* a privilege on the partition or on a column of it was granted by a role that
  is not the owner (see "What the new partition is");
* another object depends on the partition or on its row type: a function with
  the row type as an argument or a result (also as an array), an SQL function
  whose body reads it, a policy of another table that reads it, a default value
  that names it. `DROP TABLE` of the old partition would fail on the first of
  them, after the whole copy, in every run; the refusal names it
  (`pg_describe_object`), drop or change it first;
* the table has a default partition larger than
  `kingres_timepart.convert_default_max_mb` (default 256 MB, 0 to 1048576):
  `ATTACH PARTITION` scans the default partition under `ACCESS EXCLUSIVE` on the
  table to check that it holds no row of the new range, which stalls every
  reader and writer of the table for about 50 ms per million rows, and
  `lock_timeout` does not bound a scan. A policy table has no default partition
  (see "Default partition"); this is for a conversion by hand;
* for `heap`, a publication of the partition's schema or of all tables: it would
  take in the new table the moment it is created and publish every row of the
  copy under the table's temporary name (for `columnar` every publication that
  would include the partition is refused, as below);
* for `columnar`, what that access method refuses (`kingres_columnar` README,
  "What is refused" and "B-tree indexes"):
  * **an index that is not a B-tree with immediate checking**, on the table
    (its partitioned index) or on the partition: another access method (hash,
    GiST, GIN, BRIN, SP-GiST), an exclusion constraint, a deferrable primary key
    or unique constraint, an invalid index; the error names the index and the
    reason. A B-tree index of any other kind is carried over: plain,
    multi-column, expression, partial, `INCLUDE`, `DESC`, `NULLS FIRST`,
    unique (also `NULLS NOT DISTINCT`), primary key and unique constraint (they
    are per partition: the partition key is in the key of the parent's);
  * **an extension still at 0.1**: the new table would be made by
    `kingres_columnar` 0.1, of file format 2, which cannot have an index, so a
    table with indexes is refused until `ALTER EXTENSION kingres_columnar UPDATE`
    (a table the new extension makes is format 3); a table without indexes is
    converted either way;
  * a foreign key on or to the partition or the table, an `AFTER ROW` trigger
    on the partition or the table, a publication that would include the
    partition (`FOR ALL TABLES`, a schema, the table);
* a replica identity that uses an index (any target);
* a sort column that is not a column of the partition, an empty `sort_by`,
  options that are not valid (the keys and ranges of `columnar_options`
  above), or options with `heap`;
* a name conflict: a table named `<partition>__kc` exists (`name_conflict`).

A refused index is not dropped: drop it yourself when you decide that its loss
is right, or leave the partition in heap storage.

**What a columnar partition does with them.** `INSERT ... ON CONFLICT` into a
columnar partition that has an index is refused by `kingres_columnar` itself
(`INSERT ... ON CONFLICT is not supported on columnar tables`): the speculative
insert needs a row lock and a take-back that a columnar row cannot give. A
plain `INSERT` that makes a duplicate of a unique key fails as on a heap table.
`UPDATE` and `DELETE` are refused as before. Index scans and ordered index scans
work, bitmap and index-only scans do not (`kingres_columnar` README).

**Who may convert.** The owner of the table (it must own the partition too),
in its own session, or a superuser. The owner needs `USAGE` on the schema
`kingres`, as for every function of the extension: the block that does the
conversion calls `timepart__convert_plan` as the owner, after the lock. The conversion copies rows through the
owner's own `CHECK` constraints and generated columns, and drops and creates
tables as the owner, so it is run like every other action of a run (see
"Security", "A run executes the DDL as the owner"): a superuser's session runs
the owner's code only for an owner that is a superuser, or when the session has
`kingres_timepart.trust_table_owners = on`; otherwise the call fails with
`owner_code_not_trusted`, and the owner converts its own partitions. A role
that is a member of the owner role (even one that inherits) is refused: the
owner's statements would run in its session.

**Crash behaviour.** The whole conversion is one transaction. A crash (or
`pg_terminate_backend`, a cancel, `statement_timeout`) before the commit leaves
the old partition as it was, whole, and no `__kc` table: the new table was made
in the same transaction. A crash after the commit leaves the complete
columnar partition. The run row stays `running`; the next `timepart_begin_run`
marks it `interrupted`, and a rerun converts what is left (the tests
`t/006_convert_crash.pl`: a crash before each of the nine statements of a
conversion, in the middle of the copy, just after the commit, and in the
middle of a run of the policy). The history records an action in the same
transaction as the swap, so it is true after a crash: an action that is
`done` was converted, and a conversion that was cut off has no action.

**Files after a crash.** The catalog is as before, but the files of the
unfinished new table are not removed: PostgreSQL does not remove the files
of a relation that a transaction in flight at a crash had created (they
belong to no `pg_class` row). A `kill -9` in the middle of a columnar to heap
copy of 2.5 million rows left a file of 187 MB; a heap to columnar conversion
writes its data at the commit, so it leaves less. A cancel, an error, a
timeout and `pg_terminate_backend` leave nothing. The files are found by name:
a file of the database directory whose number is no `pg_class.relfilenode`.
As a superuser (it lists a directory), after a crash:

```
SELECT f AS orphan
  FROM pg_ls_dir('base/' || (SELECT oid FROM pg_database WHERE datname = current_database())) f
 WHERE f ~ '^[0-9]+$'
   AND f::oid NOT IN (SELECT pg_relation_filenode(c.oid) FROM pg_class c
                       WHERE pg_relation_filenode(c.oid) IS NOT NULL);
```

and, after looking at it (a relation that a session creates or rewrites
now is in `pg_class` already, so a file that this lists after a quiet restart
belongs to no relation), remove each file named `<number>` and
`<number>.<n>` and its `_fsm` and `_vm` forks by hand, with the server stopped.
Nothing in the extension can do it.

**Standbys.** The conversion is ordinary WAL (the DDL of PostgreSQL and the
records of `kingres_columnar`), so a hot standby sees the old partition or
the new one, never neither or both, and the same rows after replay
(`t/007_convert_standby.pl`, which also converts on a promoted standby).

**Cost.** The copy reads at the speed of the heap scan and writes at the speed
of compression; the new table exists next to the old one until the swap, so the
tablespace needs room for both. On a 1 million row partition of 60 byte rows
the copy takes about 1.2 seconds on the test machine (not a benchmark).

## Dump, restore, rename, drop

Policies and history are in `pg_dump` output of the database (the three base
tables and the sequence of `run_id` are extension configuration).

**Who may dump and restore.** A dump needs a role that can read the base tables:
a superuser, or any role that can read every table of the database, such as a
member of `pg_read_all_data` (no `BYPASSRLS` is needed, and no row-level
security is in the way). A database owner that is not a member of
`pg_read_all_data` is refused like any role that does not own the tables; add
it to the role or use another. `pg_dump` reads the sequence behind `run_id` before it reads the tables, so a
dump of the whole database by such a role stops with `pg_dump: error: failed to
get data for sequence "timepart_run_data_run_id_seq"; user may lack SELECT
privilege on the sequence or the sequence may have been concurrently dropped`
(exit status 1); only a dump of a table alone (`-t kingres.timepart_policy_data`)
reaches the table and says `permission denied for table timepart_policy_data`.
Both are what a role gets that cannot read the extension's tables (the messages
are from a run on PostgreSQL 18). A dump by a superuser and a dump by a member of `pg_read_all_data` are
identical. The restore needs a superuser, as `CREATE EXTENSION kingres_timepart`
does, and the restore writes the policies as they were (the trigger that pins
`created_by` and the other columns trusts a superuser only); a database owner
that is not a superuser is refused. (Tests: `003_dump_restore.pl`.) After a restore the
OIDs in a policy and in the history are stale, and the rows carry the stamp of
another installation (see "Identity"): the table is found by name, the role by
name, and the next `timepart_begin_run` replaces the OIDs and the stamp. A run that was `running` in the dump is marked `interrupted` by
the next begin on its table, because its backend (pid and start time) is not alive.

An event trigger on `ALTER TABLE` moves the policy (and the name in the runs) when
the table is renamed or moved to another schema; it changes a policy only when
the policy has the stamp of this installation and the table named in it no
longer exists, so a stale hint, which can be the OID of another table, cannot
move a policy by mistake. An event trigger on `DROP` removes the policy of a
dropped table (the history stays, with the owner the table had, and no longer
belongs to a table). A rename of the schema is not followed: the policy becomes an orphan,
reported by `timepart_check`, and has to be set again.

`DROP EXTENSION kingres_timepart` removes the event triggers, the functions and
procedures, the views, and the tables with the policies and the whole history
(without a warning: dump them first if the history matters). A later `CREATE
EXTENSION` is a new installation: no policy, no run, the numbering of runs
starts again, and it has a stamp of its own (the test `timepart_drop`).
`CREATE EXTENSION` in a database where something of the user's already has the
name of an object of the extension in the schema `kingres` (a table
`timepart_policy_data`, a view `timepart_run`, a function with the signature of
one of ours, a sequence, an index or a type of those names) ends in an error that
names it, and changes nothing: the extension is not created, no other object of
ours is left, and what the user made is as it was. A function of the user's with
another signature is an overload and stays (tested). The partitions and
tables of the users stay, as ordinary tables, and so do detached partitions. It
leaves the schema `kingres`: `CREATE EXTENSION` created the schema because the
control file asks for it, but the schema is not a member of the extension (the
extension depends on the schema, not the other way), so that the extensions
that share it do not remove it from each other. Drop it by hand when no other
extension uses it. The test `timepart_drop` shows it.

**A role named like the schema.** The default `search_path` is `"$user",
public`. A role named `kingres` (the name `initdb` gives to a superuser when the
server runs under an operating system user of that name) makes the schema of the
extension the first schema of its path, so an unqualified `CREATE TABLE` creates
the table in `kingres`, and such tables make a later `DROP SCHEMA kingres` harder.
Nothing of the extension is affected (everything it does is qualified); give
tables a schema, or set `search_path` for that role.

## Upgrade path

Version 0.1 was the first version, 0.2 adds the conversion: `ALTER EXTENSION
kingres_timepart UPDATE` runs `kingres_timepart--0.1--0.2.sql`, and a new
installation runs `kingres_timepart--0.2.sql`; the regression test `upgrade`
shows that the two give the same catalog (columns, constraints, functions with
the digest of their bodies and their privileges, comments, event triggers,
members, dumped configuration), that the policies, the history and the
identity of the installation (`timepart__stamp()`) survive, and that a role
that was given `timepart_grant_driver` before has the two new functions. The
tables and views are unchanged. 0.2 replaces eleven functions and procedures by
`CREATE OR REPLACE` with the signature they had (so their privileges stay),
adds eight, drops `timepart__no_columnar` (it was executable by superusers
only; `timepart__need_columnar` replaces it for every caller), and changes one
visible behaviour: `columnar_options` of a policy is checked when the policy is
set (only the four keys of `columnar_set_options`; 0.1 accepted any JSON object).
A policy of 0.1 that holds another key must be set again. A released script is
not edited: `kingres_timepart--0.1.sql` is as it was.

Version 0.2 itself has not been released (no beta, no tag), so the review of
the conversion (`kingres/docs/reviews/2026-11-20-kingres-timepart-0.2.md`) was
fixed in `kingres_timepart--0.2.sql` and in `kingres_timepart--0.1--0.2.sql`,
which still give the same catalog; there is no `0.2--0.3` script. The fixes add
one function (`timepart__convert_plan`), change `timepart__convert_refusal`,
`timepart__convert_stmts`, `timepart__special_table` and two lines of
`timepart_run_action` and `timepart__finish_run` (the retry of `schema_changed`),
and add one setting (`kingres_timepart.convert_default_max_mb`). A database
that has installed 0.2 from a development build before that needs `DROP
EXTENSION` and `CREATE EXTENSION` (the policies are in the extension's own
tables: dump them first) or the `CREATE OR REPLACE` of those functions from the
script; once a version is released, a change to its script is a new version
with an upgrade script.

## Limits

- One time key column; no expression, no multi-column key, no `LIST` or `HASH`.
- At most 10000 partitions created by one call or one plan (`too many
  partitions in one call`); a run is bounded the same way.
- Years 1 to 9999 only.
- Retention drops whole partitions, never rows; a partition older than
  `retention` that spans the boundary stays until its upper bound passes it.
- A run creates only the cells from the first existing partition on, and not
  the cells older than the retention boundary; a hand-made gap stays a gap.
- A run cannot be started inside a transaction block.
- Every action re-plans from the catalog, so a run of N actions reads the bounds of the
  partitions N times; with thousands of partitions and thousands of actions in one run
  this is slow (at most 10000 actions per plan). A daily run has one or two.
- One step of a run per table at a time (a `NOWAIT` row lock on the table's lock
  row); a run is alive while the backend that began it is: a scheduler that begins
  a run on one connection and closes it must expect the run to be marked
  `interrupted` by the next begin of the table, and its steps then return false.
- Partitions are owned by the role that created them; the owner of the table
  needs `CREATE` on the schema of the table.
- A schema rename orphans the policies of its tables.
- A conversion needs a partition that has no hash, GiST, GIN, BRIN or SP-GiST
  index, exclusion constraint, deferrable unique key, foreign key, row trigger,
  rule, view or publication of the kinds listed in "Conversion", and the owner
  of the table owns the partition. A table with distributed partitions is not
  converted. A partition with indexes needs about twice the time of the copy
  alone (the indexes are built from the new rows), and room for the indexes of
  both tables until the swap.
- A conversion is tried once per run and not retried; a partition that is
  written through the parent at every attempt is not converted (`timepart_check`
  says `convert_blocked`).
- A `REPEATABLE READ` or `SERIALIZABLE` snapshot that is older than the swap and
  has not read the table yet finds the converted partition empty (see "Snapshots
  older than the swap"); the extension cannot see such a snapshot and does not
  wait for it.
- The new table is made by `CREATE TABLE ... (LIKE ...)` and filled by the
  conversion, so the TOAST parameters do not go to a columnar partition, columnar
  options do not come back from one, and the grantor of a privilege must be the
  owner (see "What the new partition is").
- A partition that a foreign key references is not retired while a referencing
  row exists.
- A partition the policy does not recognise (bounds off the grid) is never
  retired by a run.
- A table owner can keep a driver's session busy with a slow function in an index
  expression (see "Security" and "Time limit of an action").

## Departures from the design

The design is `kingres/docs/design/analytics-phase12-storage.md`, section 4.4 and
10. What differs, and why:

1. `CREATE TABLE ... (LIKE ...)` and `ATTACH PARTITION` as designed, but no
   helper `CHECK` constraint: the new table is empty, so the attach scans
   nothing, and the constraint would only have to be dropped again.
2. `timepart_run` has a trailing `INOUT run_id bigint` that returns the id of
   the run. `timepart_run` and `timepart_run_phase` are superuser only (the
   design lists `timepart_run_phase` as superuser only and does not say for
   `timepart_run`) and carry no `SET search_path` clause, because a procedure
   with a `SET` clause cannot `COMMIT`; they set the path of each transaction
   themselves instead. The single step is a public function,
   `timepart_run_action(run_id, phase)`, and `timepart_action_timeout()` gives
   the `statement_timeout` for a driver that issues each action as a statement.
3. `timepart_due` calls its input `as_of_time`: PostgreSQL does not allow an
   input and an output column with the same name (`as_of`).
4. `timepart_check` has a second argument `as_of` (default `now()`), so that it
   too is under the control of a test's clock, and a problem `misaligned`.
   `drop_old_time_partitions` has a fourth argument `timezone` like
   `create_time_partitions`.
5. `timepart_run_action.kind` also allows `misaligned` (design 10.2: the run
   reports a misaligned partition).
6. The policy has a column `table_owner`, the owner at the time the policy was
   set, for the `owner_changed` rule (design 12, item V20): the simple form, the
   owner at run time must be the one recorded.
   The policy and history tables are not private to the extension owner as the
   design says: views and a trigger let a role read and write the
   rows of the tables it owns (the data is in the tables `timepart_policy_data`,
   `timepart_run_data` and `timepart_run_action_data`, the views carry the
   names the design gives the tables), which makes `timepart_set_policy` and the other
   owner-facing functions `SECURITY INVOKER` (the design had `SECURITY DEFINER`
   functions that decide the caller; that cannot be done correctly, because
   inside such a function the caller's `current_user` is not available).
   The claim of a run is a row lock on a private table and the backend recorded
   in the run row, not an advisory lock (any role can take an advisory lock whose
   key it can compute).
7. `next_run_at` is `as_of + run_every`, not `now() + run_every`, so that a run
   with a controlled clock is reproducible; for a scheduled run `as_of` is
   `now()`. After a lock timeout it is `as_of + least(run_every, 5 minutes)`.
8. Cells older than the retention boundary are not created when the run catches
   up (the design creates every cell from the first existing partition, which
   would create partitions only to drop them in the same run).
9. A run whose action hits a default partition that already holds rows is
   `partial`, not refused up front; the refusal up front is for `set_policy`
   and `create_time_partitions`.
10. Hour partitions are named in UTC, and `timestamp` and `date` keys use the
    wall clock without daylight saving (the design says "in the policy's zone"
    for names and does not treat naive keys).
11. The retries of a lock timeout are made by the run (`kingres_timepart.lock_retries`
    attempts); `create_time_partitions` and `drop_old_time_partitions` called by
    hand make one attempt and raise the lock error. Creation is retried too, not
    only retirement.
12. The DDL statements of an action are built by `timepart__create_stmts` and
    `timepart__retire_stmts` and executed by the run under `SET LOCAL ROLE`
    to the owner, so that the owner needs no right on the extension's schema.
13. A partition the policy does not recognise (bounds off the grid) is never
    converted, dropped or detached by a run; the design only said the run
    reports it and creates around it.
14. A zone that skipped a calendar day has that cell left out of the grid.
15. Items needing verification settled: V14 (DETACH CONCURRENTLY inside a
    procedure fails: tested), V15 (daylight-saving bounds: tested over Berlin,
    New York, Lord Howe, Havana and Kolkata), V20 (owner rule: see 6).
16. Conversion (increment T2), each where it differs from design 10.4:
    - The partition is locked `SHARE ROW EXCLUSIVE`, not `SHARE`: the same for
      readers and writers, and a plain `CREATE INDEX` or a second conversion
      cannot slip in during the copy.
    - A writer through the parent does not wait for the copy and then go on
      against the new partition, as the design says: it holds the parent while
      it waits, so the swap cannot lock the parent. The 500 ms wait of the
      swap breaks that cycle in the design, but the writer's own
      `deadlock_timeout` timer started when it began to wait, long before the
      swap asked, and a writer that had waited a second or more was made the
      victim of the deadlock detector in the test (`t/005`). The swap now looks
      in the lock table before it asks, and gives up at once, with
      `swap_lock_timeout`, when a session that holds the parent waits for the
      partition; the 500 ms wait stays for the other cases (a reader, a prepared
      transaction). A writer that names the partition behaves as the design
      says.
    - The row counts of `columnar_info` are not compared in the transaction (the
      stripes are written at commit); they are read after it and written to the
      history. The check of the counts of the two tables is made, under the lock.
    - A conversion is tried once in a run (the design retries a lock timeout up
      to five times; for a copy that is the expensive part), and records
      `swap_lock_timeout` or `lock_timeout` as the detail of a failed action; the
      next run is due within five minutes in both cases.
    - A refusal in a run is a `skipped` action, not a `failed` one, and the run
      stays `done`; `timepart_check` reports it as `convert_blocked`.
    - The refusals are those of the design and also: a partition that has a
      trigger of its own of any kind (not only a row-level one), extended
      statistics, an identity column, a sequence it owns, a different owner than
      the table, a transaction isolation level other than `READ COMMITTED`, and
      options with a target other than columnar; the refusal for functions
      outside `pg_catalog` in a `CHECK`, a generated column or a default is
      replaced by the rule of every action of a run: the owner's code runs in a
      superuser's session only for an owner that is a superuser, or when the
      session trusts the owners (`kingres_timepart.trust_table_owners`), else
      the owner converts its own partitions.
    - `convert_old_partitions` stops at the first partition that is refused or
      fails (a procedure that commits cannot catch an error and go on); the
      policy path records each partition and goes on.
    - The policy's `columnar_options` is checked when the policy is set: the keys
      of `kingres.columnar_set_options` and their ranges.
17. Indexes (the round after T2, `kingres_columnar` 0.2): a conversion carries
    every B-tree index with immediate checking; see "Conversion". The indexes of
    the new table are built after the load with plain `CREATE INDEX` (a
    constraint with `ADD CONSTRAINT`), under temporary names, before the swap, so
    that the attach finds them; a `VACUUM` of the new partition is not made by
    the extension (not allowed in a function).

## Tests

`sql/` and `expected/` hold the pg_regress tests (`timepart_setup`,
`timepart_grid`, `timepart_create`, `timepart_policy`, `timepart_run`,
`timepart_retention`, `timepart_names`, `timepart_hostile`, `timepart_security`, `timepart_history` (who
sees the history, renamed roles and tables, owner changes, dropped roles),
`timepart_privilege` (the code of a table owner while a superuser runs the
maintenance), `timepart_forged` (policy and history rows shaped by hand, the seal), `timepart_owner_code` (the refusal, what the acknowledgement gives away, the owner's own session), `timepart_convert` (conversion to columnar and back, every kind of index and every refusal with its message, a duplicate found while an index is built, a copy that loses rows, who may convert, hostile names and search paths, the policy path), `timepart_convert_more` (what a conversion keeps: the enabled state of cloned
triggers, storage parameters, statistics targets, the clustering index, replica
identity, constraint comments, a revoked privilege of the owner, no default
privilege of the schema; what it refuses before the copy: a constraint that is
not validated, a grant made by another role, an object that depends on the
partition or its row type, a large default partition, a publication of the
schema; a name of multibyte letters with two indexes; another table access
method; a partition off the grid in the convert phase of a run; a superuser
that switched to the owner; the helper that knows the ledger and distributed
tables), `timepart_drop` (`DROP EXTENSION` and `CREATE EXTENSION`, also where
the user has objects of the same names), and `upgrade` (0.1 to 0.2 against a new installation of 0.2)).
`timepart_convert` needs `kingres_columnar` in the installation and preloaded
(`kingres_timepart.conf` does it; for a build outside the tree, install
`kingres_columnar` 0.2 first). Every one passes an `as_of` time; none reads the clock. `t/` holds the TAP tests:
`001_locks.pl` (a long transaction holding the parent, readers and writers,
one run per table, a role that holds advisory locks, a foreign key), `002_interrupt.pl` (a backend killed and the server
crashed in the middle of a run, then repeated), `003_dump_restore.pl`
(`pg_dump` and restore by a superuser, a role with only `pg_read_all_data` and a
database owner, then a run; a restore by a role that is not a superuser is
refused; the extension in a database whose schema `kingres` exists already; who sees
what before and after a restore, also into another cluster where the role OIDs
differ and a role is renamed afterwards), `004_owner_and_races.pl` (a server
without the acknowledgement setting; a rename, a change of owner and a drop of
the table while an action waits for it; forged rows restored with `COPY`;
`dblink` and `COPY ... PROGRAM` from the owner's code), and for the
conversion `005_convert_concurrent.pl` (a writer that names the partition, a
writer through the parent, by hand and by the policy, a long reader, readers
across the swap, a cancel by `statement_timeout`; each time the count and a
checksum of every row against the rows that were inserted),
`006_convert_crash.pl` (a crash before each statement of a conversion, in the
middle of the copy, just after the commit, and in the middle of a policy run),
`007_convert_standby.pl` (a reader on a hot standby across the swap, the same
rows and the same access method after replay, a promotion followed by a
conversion), `008_convert_prepared.pl` (a prepared transaction that holds a
row, a row through the parent, or a read, also across a restart and through
the policy), `009_convert_ddl_race.pl` (a column added and filled and a unique
index created, a column dropped, and the partition renamed, each while the
conversion waits for its lock; the tablespace; the swap that gives way after the
short wait and says why, to a reader and to a writer; the limit of the
`REPEATABLE READ` snapshot).

Build and test against a KingresDB installation, out of tree (install
`kingres_columnar` into the same installation first, with
`kingres/dev/ext.sh kingres_columnar build install`):

```
KINGRES_WORK=<work area> kingres/dev/ext.sh kingres_timepart all
```
