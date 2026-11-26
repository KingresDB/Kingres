<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
# Review of the fourth fix round: contrib/kingres_timepart and
# contrib/kingres_sketch (phase 12)

Reviewer: an independent reviewer. Reviewed state: commit 89d79e7b84
(`kingres_timepart`) and commit d1ef3851ee (`kingres_sketch`), the committed
tree at d1ef3851ee for the two directories. Exported with `git archive` into
a private work area; the working tree was not used. Nothing in the
repository was changed except this file.

## Scope and method

Read the commit messages of 89d79e7b84 and d1ef3851ee in full, the review
they answer (`2026-11-16-phase12-extensions-third-fixes.md`, K12T-01 to
K12T-03) and the three reviews before it. Read `kingres_timepart--0.1.sql`
in full (tables, the seal trigger, the policy trigger, the driver and
`exec_as`/`exec_direct` machinery, `timepart_adopt`, the views, the event
triggers) and the memory-accounting and GUC parts of `kingres_sketch.c`,
together with the relevant parts of `hll.c`, `topn.c`, `tdigest.c` and
`sketch_hash.c`. Read both `README.md` files for the sections the commit
messages point at.

Built and ran the existing suites once on each major first (18.7 and
19rc2, read-only installations copied for this review, `EXT_WERROR=1`, at
most `-j2`, ports 59700-59732), with `kingres/dev/ext.sh <ext> sync clean
build install regress tap`, then threw away the test work areas and wrote
the experiments below against private servers of my own (same port range,
stopped at the end of each session), as roles with only the rights the
README gives, plus one superuser session playing the maintenance driver or
the administrator.

Baseline runs (see "Numbers of the baseline runs"): both extensions build
without a compiler warning and pass their regression and TAP suites on
18.7 and 19rc2.

## Summary

| Severity | Count |
|---|---|
| Critical | 0 |
| High | 0 |
| Medium | 0 |
| Low | 1 |
| Note | 1 |

All three items of the third review (K12T-01 to K12T-03) are fixed, and
every attempt made in this round to walk around the new protections
failed. One low-severity usability gap is reported (K12U-01): the
documented recovery path for a role's own `kingres_sketch.state_memory_
limit` before the library is loaded in a session needs the role to be
able to call a function of the extension first, which needs a grant the
README's own install example does not mention, so an operator who follows
only the "Install" section's grants cannot use the path the memory
section describes.

## Fate of K12T-01 to K12T-03

| ID | Fate | Evidence | Seen fail on earlier code? |
|---|---|---|---|
| K12T-01 (`kingres_sketch`, any role raises its own ceiling with `ALTER ROLE <itself> SET`) | fixed | the ceiling is now a second, `PGC_SUSET` setting (`kingres_sketch.state_memory_ceiling`); `ALTER ROLE sknosuper SET kingres_sketch.state_memory_ceiling = '2GB'` run as `sknosuper` itself gives `permission denied to set parameter`, on 18.7; the role's own `state_memory_limit` can still be lowered (`ALTER ROLE sknosuper SET kingres_sketch.state_memory_limit = '2GB'` succeeds for itself, as designed) | Not run against the parent of 89d79e7b84/d1ef3851ee in this round; the commit's own account of reproducing it (`ALTER ROLE <itself> SET` on the state before this fix) was not repeated independently here |
| K12T-02 (`kingres_timepart`, the owner's code leaves an object in `pg_temp` that answers the superuser's later, unrelated queries) | fixed | the superuser path now needs `kingres_timepart.trust_table_owners = on` and is refused otherwise (`owner_code_not_trusted`); the owner-driven path (`timepart_grant_driver`) keeps the owner's code in the owner's own session, where nothing it leaves behind can be read by the superuser's session; `SET ROLE <owner>` in the superuser's session is judged by `session_user`, not `current_user`, so it still cannot reach the `direct` or `frame` execution mode (verified directly, see "What held under attack") | Not run against the parent of 89d79e7b84 in this round |
| K12T-03 (`kingres_timepart`, a policy row whose stamp equals this installation's own bypasses the owner-identity check) | fixed | the crafted row of the review (real schema/table/owner, this installation's stamp, a destructive retention) is not sealed, `timepart_begin_run` fails it with `policy_unverified`, and zero actions run; direct `INSERT` into `kingres.timepart_policy_data` never calls `timepart__mark_seal`, so a superuser's hand-written row stays unsealed until an explicit `timepart_adopt()` call, which itself refuses a row that already carries this installation's stamp unless called as `timepart_adopt(true)` | Not run against the parent of 89d79e7b84 in this round |

None of the three was independently reproduced against the parent commit
in this round (the budget went to attacking the fixed code instead, per
the instruction to go beyond what the last round covered); the commit
messages' own accounts of reproducing them are the only record for the
parent code, as in the previous round.

## New findings

### K12U-01 (low) The documented recovery for a role's own
### `kingres_sketch.state_memory_limit`, before the library is loaded in a
### session, needs a grant the "Install" example does not give

Where: `contrib/kingres_sketch/README.md`, the paragraph beginning "*A
library that is not preloaded*", and the "Install" section's example
`GRANT`.

The README says: "before the library is loaded in a session even the
role's own limit cannot be stored (call a function of the extension
first, then `ALTER ROLE`)." A role that was only given the grants the
"Install" section shows as the administrator's example (`GRANT EXECUTE
ON FUNCTION hll_add_agg(...), ... TO app_role`, and similar for `topn`
and `tdigest`) has no `USAGE` on the extension's schema and no `EXECUTE`
on `state_bytes()` or `state_limit()`, the two functions the README
names for exactly this purpose ("for tests and monitoring"). Calling
either, as such a role, before the library has been loaded in that
session, fails with "permission denied for schema kingres_sketch", not
with the `unrecognized configuration parameter` the README describes for
an unloaded session, and the role is stuck: it cannot load the library
through a function it may call (the aggregate functions are granted, but
calling one only to load the library and then issuing `ALTER ROLE` in
the same session works for the aggregate functions too, so this is a
narrow gap, not a dead end) and cannot call `state_bytes()`/`state_
limit()` either until an administrator additionally grants `USAGE` on
the schema and `EXECUTE` on those two functions.

Steps (18.7), as a role with only the extension's own install-example
grants on `hll_add_agg`, `USAGE` on the extension's schema not granted:

```sql
-- as the role, a fresh connection, before any call in this session
SELECT kingres_sketch.state_bytes();
```
Observed: `ERROR: permission denied for schema kingres_sketch`.

Expected: either the README's "Install" example grants `USAGE` on the
schema and `EXECUTE` on `state_bytes()`/`state_limit()` alongside the
aggregate functions, or the README's recovery sentence names an
aggregate function (which the example does grant) as the one to call
first, rather than implying any function of the extension will do.

Severity: low. It is a documentation/install-example gap, not a security
weakness: a role can still load the library and lower its own limit by
calling any aggregate function it has been granted (confirmed: `SELECT
kingres_sketch.hll_add_agg(1)` loads the library and a following `ALTER
ROLE ... SET kingres_sketch.state_memory_limit` then succeeds for that
role). The role that cannot reach `state_bytes()`/`state_limit()` can
monitor nothing of its own account, which is a usability loss, not an
escalation.

Suggested direction: add `GRANT USAGE ON SCHEMA kingres_sketch` and
`GRANT EXECUTE ON FUNCTION kingres_sketch.state_bytes(), kingres_sketch.
state_limit()` to the "Install" example's `GRANT` block, or change the
recovery sentence to name a function the example already grants.

## What held under attack

**Mechanism 1 (`kingres_timepart`): who may run maintenance for whose
table.**

- An ordinary role with no driver grant cannot call `timepart_run`,
  `timepart_run_action`, `timepart_begin_run` or `timepart_finish_run`
  for any table, its own included (`permission denied for procedure/
  function`), confirmed on 18.7; `timepart_grant_driver('ownr')` by a
  superuser is needed first, and only then can `ownr` drive its own
  table (`CALL kingres.timepart_run('tpo.t')` as `ownr`, directly,
  succeeds and the actions run as `ownr` with `o_mode = 'direct'`,
  confirmed by the committed `timepart_privilege`/`timepart_owner_code`
  suites passing and by a direct call made here).
- A superuser's `SET ROLE ownr` before `CALL kingres.timepart_run(...)`
  for `ownr`'s own, driver-granted table does start a run (`require_
  driver` passes, since `session_user` is the superuser and the role the
  invoking function runs as is the table's owner), but the action itself
  then fails cleanly with `owner_code_not_trusted`, because `session_user
  <> current_user` makes `p_caller_plain` false (no `direct` mode) and
  `current_user` is not a superuser (no `frame` mode either): confirmed
  on 18.7, run recorded `failed`, zero rows in `timepart_run_action` for
  that run. This matches the README's claim that a role switched to with
  `SET ROLE` in a superuser's session is judged as the role it set, in a
  superuser's session, and is refused.
- Without `kingres_timepart.trust_table_owners = on`, a superuser's own
  `CALL kingres.timepart_run(...)` for a table owned by an ordinary,
  non-superuser role is refused outright with `owner_code_not_trusted`
  before any DDL runs: confirmed on 18.7 and 19rc2.
- `kingres_timepart.trust_table_owners` is a setting of a pure-SQL
  extension (no C code, no `shared_preload_libraries` entry), so it is
  forever an unregistered placeholder to every backend of every session
  unless something in that session sets it. PostgreSQL's own rule for an
  unregistered placeholder is that `ALTER ROLE ... SET`, `ALTER DATABASE
  ... SET` and `ALTER ROLE ... IN DATABASE ... SET` all require superuser
  rights, regardless of whose row is being written and regardless of
  self-targeting. Tried and refused with `permission denied to set
  parameter "kingres_timepart.trust_table_owners"` on 18.7 and 19rc2:
  - an ordinary role (`ownr`) doing `ALTER ROLE ownr SET ... = on` for
    itself;
  - the owner of the database the superuser later connects to (`ALTER
    DATABASE tp SET ... = on`, with the database first given to `dbo`
    by `ALTER DATABASE tp OWNER TO dbo`, confirmed `dbo` really owns
    `tp` via `pg_database.datdba`, then connected as `dbo` and refused).
  `ALTER ROLE <role> IN DATABASE <db> SET ...` by the superuser itself,
  for `ownr`, succeeds (an ordinary administrative act, not an attack,
  since only a superuser or `CREATEROLE` can target another role's
  settings at all; a non-superuser's own attempt at that form was not
  separately tried, since PostgreSQL already needs the same privilege
  that the two forms above lack). This is the opposite of K12T-01's
  cause for `kingres_sketch`: there the setting had a C-registered,
  `PGC_USERSET` identity once the library loaded, which is exactly what
  let a role store it for itself; `trust_table_owners` has no C
  registration at all, so it never acquires that identity and the
  superuser-only placeholder rule applies to it unconditionally.
- A forged policy row placed directly in the base table (schema, table
  and owner all genuine, the installation's own stamp, a one-second
  retention), the scenario K12T-03 described: `timepart__is_sealed`
  reports `false` immediately after the `INSERT` (the trigger that
  writes the seal fires only through the interface, and a raw `INSERT`
  as superuser does not ask for one); `CALL kingres.timepart_run(...)`
  for that table fails at once with `policy_unverified`, before any
  catalog-changing statement; zero rows appear in `timepart_run_action`
  for that run. `timepart_adopt()` (no argument, i.e. `own_stamp =
  false`) also refuses the row with a `WARNING` ("carries the stamp of
  this installation but was not written by it") and does not seal it.
  Confirmed on 18.7.
- `timepart_adopt(true)` does adopt and seal a row of this kind once its
  values pass `timepart__validate_policy` and its recorded owner matches
  the table's real, current owner — exactly the documented, explicit
  exception ("call `timepart_adopt(true)` if the row is known to come
  from this installation"), not a new way around the check; the
  README's own "What a hostile dump can and cannot cause" already states
  this residual risk for an operator who adopts an unexamined, forged
  row.

**Mechanism 2 (`kingres_timepart`): the seal.** Covered directly by the
K12T-03 fate above (a forged, unsealed row does nothing) and by the
existing `timepart_forged` suite (14+ cases, read but not independently
rerun against new attacks this round beyond the one above, given the
time spent on mechanism 1 and mechanism 3). No additional seal-specific
attack (fingerprint collision, dump-format carrying the seal table,
`pg_upgrade`, a rename dance) was attempted independently in this round;
see "Not covered".

**Mechanism 3 (`kingres_sketch`): the ceiling and the limit.**

- `ALTER ROLE <itself> SET kingres_sketch.state_memory_ceiling`, the
  exact K12T-01 attack, is refused with `permission denied to set
  parameter` for an ordinary role altering itself, confirmed on 18.7
  (the GUC is now `PGC_SUSET`, defined in `_PG_init`); the same role's
  own `state_memory_limit` is settable by `ALTER ROLE <itself> SET` once
  the library has been loaded in that session (by calling a granted
  aggregate function first), confirmed on 18.7, matching the design.
- A non-superuser's connection with `PGOPTIONS='-c kingres_sketch.
  state_memory_ceiling=2GB'` and the library **not** preloaded: the
  connection succeeds, the attempted value is refused with a `WARNING`
  (`permission denied to set parameter`, then the library's own warning
  that the administrator's configured value was lost), and the session's
  effective limit is capped at the documented failsafe of 1 MB
  (`state_limit()` returns `1048576`); confirmed on 18.7, matching the
  commit message and the README word for word.
- The same `PGOPTIONS` attempt with the library **preloaded**
  (`shared_preload_libraries = 'kingres_sketch'`): the connection itself
  is refused at startup with `FATAL: permission denied to set parameter
  "kingres_sketch.state_memory_ceiling"`, confirmed on 18.7; this is the
  stronger guarantee the README recommends preloading for.
- The memory limit itself: a `GROUP BY` of 5 groups of an `hll` sketch
  under a 2 MB limit completes and `state_bytes()` returns to 0
  afterward; the same aggregate with 50 groups under a 100 kB limit
  fails with `SQLSTATE 54000` ("aggregate states of kingres_sketch hold
  131072 bytes ... over the limit of 102400 bytes") and `state_bytes()`
  returns to exactly 0 immediately after the error; confirmed on 18.7.
- Timing and memory of one million groups, and mutant-style checks of
  the accounting under errors inside the accounting code, were not
  independently repeated this round; the commit message's own figures
  (measured on 18 only, as it states) were not re-measured here; see
  "Not covered".

## Not reproduced

- K12T-01, K12T-02 and K12T-03 against the parent commits (89d79e7b84~1
  and d1ef3851ee~1): the commits' own accounts of reproducing them were
  not independently repeated in this round; the budget went to new
  attacks on the fixed code instead, which is explicitly what this
  round's instructions called for after the last round's own
  independent reproduction of the prior findings.
- A role with `CREATEROLE` (but not superuser) attempting `ALTER ROLE
  <some other, non-superuser role> SET kingres_timepart.trust_table_
  owners = on` or the equivalent for `kingres_sketch.state_memory_
  ceiling`: PostgreSQL's own rule already requires the unregistered-
  placeholder or `PGC_SUSET` permission respectively regardless of
  `CREATEROLE`, so this was reasoned from the GUC rules rather than
  separately executed.
- `GRANT SET ON PARAMETER kingres_sketch.state_memory_ceiling TO role`,
  the administrator's documented delegation path: not exercised: this
  round trusted the README's own table of outcomes for it, which is
  consistent with the code (`DefineCustomIntVariable(..., PGC_SUSET,
  ...)`, which PostgreSQL's `GRANT SET ON PARAMETER` is built to honor).
- Parallel workers and a ceiling that was "lost" in the leader (the
  1 MB failsafe): whether a worker spawned from such a leader inherits
  the capped effective limit or independently reaches a higher one in
  its own `_PG_init`, was asked by the review this round answers but not
  set up and measured here; this is the same item the third round could
  not reach either.
- The one-million-group timing and memory figures of the commit message,
  and the memory figures of the largest `topn` values, were not
  independently re-measured in this round.
- Mutant-style injection of an error inside the accounting code or the
  reset callback itself (rather than inside the aggregate transition
  function, which the existing suites already cover) was not attempted.
- `kingres_timepart`'s seal mechanism beyond the one forged-row scenario
  above: fingerprint collisions under different session settings
  (`IntervalStyle`, `DateStyle`, `TimeZone`, locale), whether a dump made
  with `--inserts`/`--column-inserts`/`-a`/`--section`/`--binary-upgrade`
  carries the seal table (the README states it is not registered with
  `pg_extension_config_dump`, which was read but not independently
  dumped and inspected this round), `pg_upgrade`, and a rename/drop-and-
  recreate dance around a sealed table name.
- The owner-driven path's own misbehavior: an owner that lies in its own
  history, starves other owners' runs through the claim row or an
  advisory lock, or fills the history tables without bound, and
  `timepart_due` for a non-superuser beyond the one case confirmed (an
  ordinary role with no driver grant cannot reach it at all through the
  functions this round tried).
- `kingres_sketch`'s accounting under `GROUPING SETS` with hashing and
  sorting mixed, ordered-set and hypothetical-set aggregates, rescans of
  a subplan, and states passed between contexts by a transition function
  called outside an aggregate through direct SQL calls: read in the
  source, not independently exercised this round beyond what the
  existing `t/008_memory.pl` and `sketch_memory` tests already cover
  (which the commit message lists as including sorted/`DISTINCT`
  aggregates, `GROUPING SETS`, windows and parallel queries).

## Not covered

- Any attack requiring `dblink`, `postgres_fdw`, or `COPY ... PROGRAM`
  from inside the `SECURITY DEFINER` frame or the owner-driven session:
  neither was available to a non-superuser role in this review's test
  setup, as in the prior round.
- A full re-read of `kingres_sketch.README.md` against the code beyond
  the memory section (point for point for the rest of the document).
- A full re-run of every TAP and regression file of both extensions
  under a mutant of the fixed code (only the existing suites' own pass/
  fail on the committed tree was checked; no mutant of 89d79e7b84 or
  d1ef3851ee was built).
- `security_barrier`/leak-proofness of the history views beyond what the
  second review already exercised and this review did not repeat.
- `pg_upgrade` end to end for either extension.

## Numbers of the baseline runs

- `kingres_timepart`, 18.7, in tree: 14 of 14 regression tests pass
  ("All 14 tests passed"); TAP: 4 files, 216 tests, all pass
  (194 wallclock seconds).
- `kingres_timepart`, 19rc2, out of tree: 14 of 14 regression tests
  pass; TAP: 4 files, 216 tests, all pass (173 wallclock seconds).
- `kingres_sketch`, 18.7, out of tree: 8 of 8 regression tests pass;
  TAP: 9 files, 559 tests, all pass (211 wallclock seconds).
- `kingres_sketch`, 19rc2, out of tree: 8 of 8 regression tests pass;
  TAP: 9 files, 559 tests, all pass (172 wallclock seconds).
- No transient infrastructure failure was seen in this round's baseline
  runs (the third round saw one `initdb` failure under load on 19rc2,
  unrelated to the extensions, which did not recur here).
