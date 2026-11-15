<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Portions Copyright (c) 2026, RooyeKhat Media Co -->
# kingres_timepart

Time partitioning of ordinary (non-distributed) tables for KingresDB (a thin
fork of PostgreSQL 18.7), version 0.1. For a table partitioned by `RANGE` on a
time column it creates the partitions for a period ahead, drops or detaches the
partitions that have become older than a retention period, and keeps a policy
per table and a history of runs. The design is section 10 of
[`kingres/docs/design/analytics-phase12-storage.md`](../../kingres/docs/design/analytics-phase12-storage.md)
(increment T1); the departures from it are listed at the end of this file.

The extension is written in SQL and PL/pgSQL. It has no C code, needs no
`shared_preload_libraries` entry, and starts no background process: a run
happens when something calls it (`psql`, `pg_cron`, or the scheduler of the
KingresDB control plane). The same install script works on PostgreSQL 18 and
19.

Not in this version: distributed tables (they need the shard extension, phase
9), conversion of old partitions to columnar storage (it needs
`kingres_columnar`, which does not exist yet), and the scheduler, the REST
contract and the web console. The policy fields `columnar_after`,
`columnar_sort` and `columnar_options` and the procedures `convert_partition`
and `convert_old_partitions` are in the interface already; see "Conversion".

## Install

```sql
CREATE EXTENSION kingres_timepart;
```

The control file sets `schema = kingres` and `relocatable = false`. The schema
is created if it does not exist. Every object of the extension is in it.
`CREATE EXTENSION` needs a superuser, because it creates two event triggers.

EXECUTE on every function is revoked from `PUBLIC` (the pure helpers and the
functions that row-level security calls excepted). An administrator grants it
to the roles that should use the functions that act on one's own tables. The
policy and history tables are readable by every role that has USAGE on the
schema, each role seeing the rows of the tables it owns (row-level security,
judged by `current_user`):

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

The tables are owned by the extension owner and are dumped with the extension's
configuration (`pg_dump` carries the policies and the history; the sequence
behind `run_id` too; the lock table is not dumped). Every role with USAGE on
the schema may read the policy and history tables and may write the policy
table, but row-level security shows it, and lets it change, only the rows of
tables it owns, judged by `current_user`. A role that writes a policy row
directly gets the same checks as `timepart_set_policy`, and a trigger pins what
only the extension decides: `parent_oid`, `created_by`, `created_at`, and
`table_owner` (renewed only when the policy is written again by a role that
owns the table now). The history tables cannot be written by anyone but the
extension. `kingres.timepart_lock` has one row per table, is private to the
extension owner, and is what a run locks.

`kingres.timepart_policy` has one row per table, keyed by the table's schema and
name:

| Column | Meaning |
|---|---|
| `schema_name`, `table_name` | the table (a hint `parent_oid` is kept and refreshed; the table is found by name) |
| `partition_every` | the width of a partition |
| `timezone` | the zone of the grid; default `UTC` |
| `create_ahead` | how far ahead of the run time partitions are created; default 7 days |
| `retention` | partitions that end at or before `as_of - retention` are retired; null keeps everything |
| `retention_action` | `drop` (default) or `detach` |
| `columnar_after`, `columnar_sort`, `columnar_options` | conversion to columnar storage; see "Conversion" |
| `run_every` | the interval between runs; default 1 hour |
| `enabled` | disabled policies are not reported by `timepart_due` |
| `next_run_at` | set by `timepart_finish_run` |
| `table_owner` | the owner of the table when the policy was set; a run fails with `owner_changed` if it differs |
| `created_by`, `created_at`, `updated_at` | who set the policy first, and when |

`kingres.timepart_run` has one row per run (`run_id`, table, `as_of`, `trigger`
`schedule`, `manual` or `sql`, `state` `running`, `done`, `partial` or `failed`,
`started_at`, `finished_at`, `started_by`, `error`, and the `backend_pid` and
`backend_start` of the session that began the run).
`kingres.timepart_run_action` has one row per action of a run (`run_id`, `seq`,
`kind` `create`, `convert`, `drop`, `detach` or `misaligned`, the partition's
schema and name, its bounds, `state` `done`, `failed` or `skipped`, `detail`,
`bytes_before` for drops and detaches, times). Runs are kept for 90 days after
the time of the newest and at most 1000 per table (`timepart_prune_runs`, called
at the end of every run).

`kingres.timepart_policies` is the policy with the state of the latest run of
the table.

### Functions and procedures

Callable by the owner of the table (when EXECUTE has been granted):

| Signature | Meaning |
|---|---|
| `kingres.create_time_partitions(rel regclass, partition_every interval, end_at timestamptz, start_from timestamptz default now(), access_method name default null, timezone text default 'UTC') returns integer` | Create every missing partition from the one that holds `start_from` to the one that holds `end_at`, inclusive. Returns the number created. All or nothing (one transaction). `access_method` null means the access method of the parent. |
| `kingres.drop_old_time_partitions(rel regclass, older_than timestamptz, detach_only boolean default false, timezone text default 'UTC') returns integer` | Drop (or only detach) the partitions whose upper bound is at or before `older_than`. Returns the number. |
| `kingres.timepart_set_policy(rel regclass, partition_every interval, create_ahead interval default '7 days', retention interval default null, retention_action text default 'drop', columnar_after interval default null, columnar_sort text[] default null, columnar_options jsonb default null, timezone text default 'UTC', run_every interval default '1 hour', enabled boolean default true) returns void` | Create or replace the policy of a table. Does not create partitions; the next run does. `retention` must be at least one `partition_every`; `columnar_after` must not be longer than `retention`. |
| `kingres.timepart_unset_policy(rel regclass) returns void` | Remove the policy. The history stays. |
| `kingres.timepart_plan(rel regclass, as_of timestamptz default now()) returns table (seq integer, kind text, partition_name name, range_from timestamptz, range_to timestamptz, reason text)` | The actions a run would take at `as_of`, without taking any. A function of the catalog and `as_of`. |
| `kingres.timepart_partitions(rel regclass) returns table (partition regclass, range_from timestamptz, range_to timestamptz, access_method name, size_bytes bigint, row_estimate bigint, compression_ratio float8, attached boolean)` | Partitions with bounds, size and format; also the partitions that a `detach` retention detached and kept, with `attached = false`. Needs ownership or `SELECT`. |
| `kingres.timepart_check(min_ahead interval default null, as_of timestamptz default now()) returns table (schema_name name, table_name name, problem text, detail text)` | `ahead_low` (the last partition ends before `as_of + min_ahead`, default `create_ahead / 2`), `last_run_failed` (the last finished run is `failed` or `partial`), `retire_blocked` (that run could not drop or detach the partitions named, for example because a foreign key references them or a view depends on them), `policy_orphan` (superusers only: the table is gone or no longer suitable), `default_partition_present`, `misaligned` (partitions the policy does not recognise, which a run never retires), `lock_timeout` (the partitions could not be read within the bounded wait) and `check_failed` (any other error while checking one table). A role sees the tables it owns. |

Superuser only (the scheduler and `psql` of an administrator):

| Signature | Meaning |
|---|---|
| `CALL kingres.timepart_run(rel regclass, as_of timestamptz default now(), phases text[] default '{create,convert,retire}', INOUT run_id bigint default null)` | `timepart_begin_run`, the phases in the order create, convert, retire, and `timepart_finish_run`. The id of the run comes back in `run_id`. It commits between actions, so it cannot run inside a transaction block. |
| `kingres.timepart_begin_run(rel regclass, as_of timestamptz default now(), trigger text default 'sql') returns bigint` | Record a run in state `running` and claim the table: a row lock (`FOR UPDATE NOWAIT`) on the table's row of `kingres.timepart_lock`, which serialises concurrent begins, and the run row, which records the backend (pid and start time). Raises `55P03` when a run of the table is `running` and its backend is alive. A `running` run whose backend is gone is marked `failed` with error `interrupted`. A run that cannot start (the owner changed, the table is no longer suitable) is recorded as `failed` at once. |
| `kingres.timepart_run_action(run_id bigint, phase text) returns boolean` | Plan again from the catalog at the run's `as_of`, execute the next action of the phase that this run has not tried, record it, and return true; false when nothing is left. One transaction of the caller, with a `NOWAIT` row lock on the table's lock row, so two steps of a table never overlap. See "Time limit of an action". |
| `CALL kingres.timepart_run_phase(run_id bigint, phase text)` | `timepart_run_action` in a loop with a `COMMIT` after each call. Idempotent: it re-plans from the catalog at the run's `as_of` and does what is left. |
| `kingres.timepart_finish_run(run_id bigint) returns void` | Final state, `next_run_at`, pruning. Calling it again does nothing more. |
| `kingres.timepart_action_timeout() returns integer` | The `statement_timeout`, in milliseconds, that a driver sets before it calls `timepart_run_action` (setting `kingres_timepart.action_timeout_ms`, default 300000). |
| `kingres.timepart_due(as_of_time timestamptz default now()) returns table (schema_name name, table_name name, as_of timestamptz)` | Enabled policies whose `next_run_at` is at or before the time, oldest first. |
| `kingres.timepart_prune_runs(schema_name name, table_name name, as_of timestamptz default now()) returns bigint` | Delete old runs of a table; returns the number. |

`timepart_set_policy`, `timepart_unset_policy`, `timepart_plan`, `timepart_partitions` and
`timepart_check` are `SECURITY INVOKER`: they judge `current_user`. The functions that need
`GRANT EXECUTE` are the ones listed under "Install".

The conversion procedures `kingres.convert_partition(parent regclass, part
regclass, access_method name default 'columnar', sort_by text[] default null,
options jsonb default null)` and `kingres.convert_old_partitions(parent
regclass, older_than timestamptz, access_method name default 'columnar',
sort_by text[] default null, options jsonb default null)` raise an error that
names the missing extension `kingres_columnar`.

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
4. **Convert phase.** See "Conversion".
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
  trigger functions only; they take no decision about a caller.
- **Policy and history tables.** Row-level security shows a role the rows of
  the tables it owns, by `current_user`. A role can write the policy row of its
  own table directly, and then a trigger applies the same checks as
  `timepart_set_policy` and pins `parent_oid`, `created_by`, `created_at` and
  `table_owner`, so a role cannot make a policy for a table it does not own or
  put another table's OID in the hint. A superuser (the extension's own code, a
  restore) is trusted. The history tables are read-only for roles.
- **DDL on user tables** (`create_time_partitions`, `drop_old_time_partitions`,
  `timepart_create_one`, `timepart_retire_one`) is `SECURITY INVOKER`: PostgreSQL
  checks every statement against the caller. They also check ownership first,
  for a clear error.
- **A run executes the DDL as the owner of the table** (`SET LOCAL ROLE`), so
  the new tables are the owner's, and the owner needs `CREATE` on the schema.
  The statements are built before the switch; the owner role needs no right on
  the `kingres` schema.
- **A run does execute user code, as the owner, and with no limit of its own.**
  `CREATE TABLE ... (LIKE ... INCLUDING INDEXES)` creates the parent's indexes
  on every new partition, and an index whose expression calls a function with
  a constant argument makes PostgreSQL evaluate that function at once: an
  `IMMUTABLE` function of the owner's (a declaration, not a check) runs once
  per partition created, in the session that runs the maintenance, which is
  usually a superuser's. It runs as the owner (`current_user` is the owner, and
  `RESET ROLE` is refused in a non-volatile function), but nothing in the
  extension limits how long it runs; column defaults, `CHECK` constraints,
  generated columns and triggers are copied, not run. A detach or a drop fires
  no trigger. An attach next to a default partition scans it with the built-in
  comparison operators of the key. Event triggers that a superuser created in
  the database do run. A table owner who can make a function run for as long as
  it likes can keep a driver's session busy: see "Time limit of an action", and
  do not run the maintenance of tables of untrusted owners in a session that
  other work waits for.
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
- **Superuser only:** `timepart_run`, `timepart_run_phase`, `timepart_run_action`,
  `timepart_begin_run`, `timepart_finish_run`, `timepart_due` and
  `timepart_prune_runs`, and EXECUTE on them is revoked from `PUBLIC`.
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

`columnar_after` is stored and validated (it must not be longer than
`retention`). In the convert phase, when partitions older than `columnar_after`
exist, the run records one action of kind `convert`, state `skipped`, with the
detail `conversion needs kingres_columnar, which is not installed; N partition(s)
older than columnar_after were not converted`, and goes on; the run is not
failed. When `kingres_columnar` is installed, the detail says the conversion is
not implemented in this version. `timepart_plan` lists the partitions that
would be converted as kind `convert`. `convert_partition` and
`convert_old_partitions` raise an error that names the missing extension.

## Dump, restore, rename, drop

Policies and history are in `pg_dump` output of the database (the three tables
and the sequence of `run_id` are extension configuration). After a restore the
OID in a policy is stale; the table is found by name and the hint is refreshed
by the next run. A run that was `running` in the dump is marked `interrupted` by
the next begin on its table, because its backend (pid and start time) is not alive.

An event trigger on `ALTER TABLE` moves the policy when the table is renamed or
moved to another schema; it changes a policy only when the table named in the
policy no longer exists, so a stale hint cannot move a policy by mistake. An
event trigger on `DROP` removes the policy of a dropped table (the history
stays). A rename of the schema is not followed: the policy becomes an orphan,
reported by `timepart_check`, and has to be set again.

`DROP EXTENSION kingres_timepart` removes the event triggers, the functions and
procedures, the view, and the tables with the policies and the whole history
(without a warning: dump them first if the history matters). The partitions and
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

Version 0.1 is the first version. Its install script `kingres_timepart--0.1.sql`
creates the three tables, the view, the functions and procedures, the two event
triggers, the grants and the comments, and registers the tables and the
sequence for dumping. A later version adds `kingres_timepart--0.1--0.2.sql`,
which alters the tables in place (`ALTER TABLE ... ADD COLUMN`, new constraints
by `ALTER TABLE ... ADD CONSTRAINT`), uses `CREATE OR REPLACE FUNCTION` for
changed functions, and never rewrites the rows of the history; the control
file's `default_version` is raised and the new install script `--0.2.sql` is the
whole of the new state. A released script is not edited.

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
   design says: row-level security and a trigger let a role read and write the
   rows of the tables it owns, which makes `timepart_set_policy` and the other
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

## Tests

`sql/` and `expected/` hold the pg_regress tests (`timepart_setup`,
`timepart_grid`, `timepart_create`, `timepart_policy`, `timepart_run`,
`timepart_retention`, `timepart_names`, `timepart_hostile`, `timepart_security`, `timepart_drop`).
Every one passes an `as_of` time; none reads the clock. `t/` holds the TAP tests:
`001_locks.pl` (a long transaction holding the parent, readers and writers,
one run per table, a role that holds advisory locks, a foreign key), `002_interrupt.pl` (a backend killed and the server
crashed in the middle of a run, then repeated) and `003_dump_restore.pl`
(`pg_dump` and restore, then a run).

Build and test against a KingresDB installation, out of tree:

```
KINGRES_WORK=<work area> kingres/dev/ext.sh kingres_timepart all
```
