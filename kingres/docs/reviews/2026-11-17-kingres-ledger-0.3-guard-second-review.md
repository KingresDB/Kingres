<!-- SPDX-License-Identifier: AGPL-3.0-only -->

# kingres_ledger 0.3, part A: independent review of the fixes for the fourth review (guard, second round)

Reviewer: an independent reviewer. Subject: the fix round that answers
`2026-11-17-ledger-phase7-steps-1-2-third-fixes.md` (findings L10-01, L10-02,
L10-03, L10-13, L10-14, L10-15, L10-19), applied to the extension
`contrib/kingres_ledger`. The three earlier reviews (`2026-11-16-ledger-phase7-steps-1-2.md`,
`2026-11-16-ledger-phase7-steps-1-2-fixes.md`,
`2026-11-17-ledger-phase7-steps-1-2-second-fixes.md`) were read; nothing they
found was seen to come back. The verifier (`kingres-verify`) is outside this
review and was used only as a read-only tool for the TAP tests that want one.

## 1. Scope and method

What was reviewed: the committed tree of the fix round (extension, `kingres/dev`,
`kingres/docs`) and, for comparison, the tree of its parent. Nothing in the
repository was changed except this file. All work ran on a shared Linux machine
(WSL) against two assertion-enabled installations: PostgreSQL 18.7 and 19rc2.
Every attack below was run on 19rc2; the ones that matter were repeated on 18.7
and are marked.

The invariant that was attacked is the one of the README ("Who can write the
ledger tables"): no statement that a role can issue writes a ledger table,
whatever the role inherits or owns, except (a) through the public functions of
the extension doing what they are documented to do, and (b) a superuser acting
as itself with `kingres_ledger.allow_manual_write` set in the session. The
roles used: a plain owner of an application table with EXECUTE on `ledger_enable`;
a member of the owner role with INHERIT and no SET (the case the author's own
tests use); a member of `pg_maintain`; a member of `pg_write_all_data`; a role
that was granted EXECUTE on one builder; the owner of the extension as a
superuser; a superuser with and without the setting.

Method: (1) the baseline suites on both majors; (2) a read of `write.c`,
`capture.c`, `guard.c`, `history.c`, the relevant part of `protect.c`, the SQL
script and the README; (3) hand-written attack scripts, each reproduced with
the statements and the output recorded; (4) a catalog comparison of the three
installation paths with a dump of every catalog row that belongs to the
extension (functions with ACL, owner, `proconfig`, source hash and symbol;
relations, column ACLs, triggers with enabled state and arguments, constraints,
indexes, event triggers, initial and default privileges, dependencies,
comments), 2029 lines per database; (5) `pg_upgrade` 18.7 to 19rc2, `pg_dump`
and restore; (6) 23 mutants of the source run against the new tests; (7) CPU
time of the capture path against the parent.

The author's list of 30 mutants is not in the repository, so it could not be
sampled; the mutants of section 5 are the reviewer's own, written from the
checks that the commit message and the README claim.

## 2. Summary

Count by severity (new findings of this review):

| severity | count | findings |
|---|---|---|
| critical | 0 | |
| high | 2 | L11-01, L11-02 |
| medium | 2 | L11-03, L11-04 |
| low | 1 | L11-05 |
| note | 4 | N1 to N4 |

No row of a ledger table was written, and no chain verified, by a role or a way
outside the invariant. The writers that "compute every value they write" held
under every input a role can influence (section 4). The two high findings are not
writes: they are ways to stop the ledger (L11-01, a lock that the refusal of
L10-03 does not see) and to remove it (L11-02, a cascade that no hook of the
extension sees). Both existed before this round (shown on the parent) and both
are reachable by roles the README names as non-superusers.

The most serious, in two lines each:

- L11-01 (high): `LOCK TABLE` on a view over a ledger table is not refused. A member of
  `pg_maintain` or `pg_write_all_data` takes ACCESS EXCLUSIVE or SHARE ROW
  EXCLUSIVE on `ledger_blocks`, `ledger_pending` and the others until its
  transaction ends; the block builder and every capture trigger wait. L10-03 is fixed
  only for the direct statement.
- L11-02 (high): `DROP EXTENSION plpgsql CASCADE` by a member of the owner role
  (INHERIT, no SET; not a superuser) drops `kingres_ledger` with its tables,
  its event triggers and the capture triggers of every user ledger table, in one
  statement the hook does not see; the application tables are then freely
  updatable.

## 3. Fate of L10-01, L10-02, L10-03, L10-13, L10-14, L10-15, L10-19

| id | fate | evidence |
|---|---|---|
| L10-01 (high, a non-superuser forges a block through the internal writer) | fixed | `ledger_internal_write` and `ledger_record_row(int, record)` are gone from a new installation and from both updates (catalog dump). A member of the owner role with INHERIT and no SET calls `ledger_build_block`, `ledger_generate_digest`, `ledger_verify_recorded` and gets what they document; every INSERT or DELETE on the nine tables, `ledger_remove_stray`, `ledger_history_sync` on arbitrary tables and the setting give nothing (`ledger_paths`, repeated by hand in section 4.1, 19rc2 and 18.7). |
| L10-02 (low, superuser writes without the explicit act) | fixed | A superuser without the setting is refused for INSERT, COPY, `session_replication_role = replica`, `ALTER TABLE ... DISABLE TRIGGER` (refused even for the superuser). With the setting, INSERT and COPY leave a `ledger_removed` row with action `inserted` and `ledger_verify` reports `manual_write` (section 4.4). |
| L10-03 (medium, a weak lock on `ledger_chain` stops the builder) | partly fixed | Weak locks no longer stop the builder: ROW SHARE and SHARE on `ledger_chain` held by a `pg_maintain` member, taken through a view, the builder builds. A strong lock is still possible because the refusal looks at the name of the relation in the `LOCK` statement and not at what it reaches: L11-01. A transaction-lifetime lock on the extension object is also held by any grantee of a builder: L11-03. |
| L10-13 (medium, surviving mutants) | mostly fixed | 22 mutants of the reviewer's own (plus an equivalent control): 18 killed, 4 survive (section 5); of the survivors one is a real gap (the ownership refusal of `ledger_register_table`, L11-04), the others are redundant or unreachable checks. |
| L10-14 (low, honest paths under an open guard) | fixed | 140 lines `SET kingres_ledger.allow_manual_write = on` are gone from five files (verify 54 to 22, head 64 to 32, updatable 16 to 5, records 5 to 1, format 6 to 3). The expected files have the same number of ERROR lines as before in all but one file (`ledger_guard`: two lines of the removed internal writer gone, one lock refusal added); no test of a hand-written row turned into an early error. |
| L10-15 (low, the fingerprint ignores column privileges) | fixed | A reviewer's own dump (2029 lines, ACLs, column ACLs, `proconfig`, owners, trigger states, dependencies, comments, initial and default privileges) is identical for a new installation, 0.2 to 0.3 and 0.1 to 0.3, on 18.7 and 19rc2, and after `pg_upgrade` (section 6). |
| L10-19 (note, one test fewer on 19) | fixed | Both majors run 17 regression tests and 17 TAP files with 612 tests each (baseline, section 11). |

## 4. The guard

### 4.1 The writers: can a caller make a writer compute what the caller wants

The inputs of `ledger_build_block`, `ledger_generate_digest`,
`ledger_verify_recorded` and `ledger_remove_stray` are the ledger itself, the
clock, the caller's outer user name and (verify, remove_stray) a height range or
a reason. Nothing a role can set changes a value that is written:

- Session settings. Ten settings that change a text form, among them the client
  encoding, were varied (`datestyle`, `timezone`, `extra_float_digits`, `bytea_output`, `intervalstyle`,
  `xmloption`, `lc_monetary`, `client_encoding`, `standard_conforming_strings`,
  `default_text_search_config`) while inserting the same row, with 27 column types
  (text, bytea, timestamptz, timestamp, date, float8, float4, numeric, interval, jsonb,
  json, xml, money, inet, tsvector, enum, range, domain, int array, text array, time,
  timetz, uuid, bit varying, "char", name): the three entries have the same row
  hash (19rc2). The row hash uses the binary send functions and a client
  encoding that the extension forces to UTF-8, which is why.
- Names resolved inside the fixed statements. Under `search_path = pg_temp, public`
  a role created temporary functions named `count`, `jsonb_build_object`,
  `clock_timestamp`, `row_number`, `encode`, `array_agg`, `max`, `coalesce`, `sum`, an
  operator, types named `int8` and `bytea`, and temporary tables named
  `ledger_blocks` and `ledger_tx`; `ledger_build_block`, `ledger_generate_digest` and
  `ledger_verify_recorded` gave the documented results and a clean verification
  (functions and operators are never looked up in `pg_temp`; the statements
  qualify every type and table).
- The catalogs the writer reads. `ledger_register_table` refused, with the
  documented errors, a table with a generated column (stored and virtual), a
  composite column, a partition, an inheritance child and parent, a table with a
  rule, an unlogged table, a temporary table, a partitioned table, a system table,
  a table with an armed capture trigger and no other trigger. It accepted a table
  with a `regclass` column, which `ledger_enable` accepts too.
- Who can call it. The caller must own the table by the outer user. A member of
  the owner role with INHERIT and no SET can hand-build everything
  `ledger_enable` builds (the three triggers, ENABLE ALWAYS, the capture trigger last,
  with an identity taken from `nextval` of the registry's sequence, all in one
  transaction) and call `ledger_register_table` on a table it owns: the table is
  registered, its two existing rows are entered, `ledger_verify` is clean
  (19rc2 and 18.7). That is the documented equivalence ("a call that is not made by
  `ledger_enable` can only register a table that is exactly what `ledger_enable`
  would have made"); it is not a forgery, and the entries are those of the rows
  the table already holds, as in `ledger_enable`. A failed attempt (a missing
  trigger) leaves a table with an armed capture trigger and no registry row
  that nobody but a superuser can drop, write or re-enable (N2).
- Forged preconditions. An identity that the sequence never issued, a table the
  caller does not own, an already registered table, a table that is half made,
  a system table: all refused by `ledger_paths`. Repeated by hand: the defects of
  13 table shapes (section above), a missing trigger, a view (no trigger can be
  created on it). Not repeated: an identity never issued, a system table. A race
  of two registrations was not driven. The one precondition whose refusal is not
  tested is ownership (L11-04).
- `ledger_record_row`, the symbol kept for the 0.1 and 0.2 SQL. Called from a
  SECURITY DEFINER function of a second superuser that PUBLIC may execute, by a
  role that was granted the right to set the parameter and turned it on: refused
  (the outer user is not a superuser). Called by that superuser without the
  setting: refused. Called through a SECURITY INVOKER function of a
  non-superuser: not reachable (the GRANT is refused). What is checked is
  `GetUserId()` and `GetOuterUserId()`; the second is the session's role after
  `SET ROLE`, not the owner of the function. Background workers, parallel
  workers, apply workers and event triggers were not driven through it (section 10).
- The marker. The only statements that run while it is raised are the fixed
  `SPI` statements of `kl_run` and the two inserts of the capture code, against
  tables that have no trigger but the blocking ones, no policy, rule, default or
  index expression; user code of user tables runs before it is raised (the row is
  hashed in C) and the cursor of `ledger_register_table` reads the table without
  evaluating anything of it. No way for user code to run inside was found.
  Mutant M09 (the marker lowered only on success) is killed by `ledger_paths`.
- The setting. `GRANT SET ON PARAMETER`, `SET ROLE`, a SECURITY DEFINER function
  of a superuser, a function with a SET clause (refused when created):
  nothing is written (`ledger_paths`; the first three repeated).

### 4.2 Registration and the registry check on insert

The capture trigger now compares its identity with the registry on insert as on
update and delete. A table that was dumped and restored (the capture trigger is
restored, the registry is not) refuses every insert with `table "..." is not under
ledger control`, cannot be re-enabled (`ledger_enable` fails on the trigger that
exists) and cannot be dropped (N2, documented in the README). A table of a 0.1 or
0.2 database is in the registry with the identity its trigger carries: after
`ALTER EXTENSION UPDATE` (also after `pg_upgrade`) inserts, updates and deletes
into append-only and updatable tables of 0.1 and 0.2 capture, the builder builds,
`ledger_verify` has no finding (section 6). Mutant M01 (insert check without the
comparison) is killed.

### 4.3 The marker, `kl_run`, the lock of the builder

`ledger_build_block` waits on `LockDatabaseObject(extension, oid, 0x4B4C,
ExclusiveLock)`. No SQL statement can request that tag: advisory locks use another
lock type, `COMMENT ON EXTENSION`, `ALTER EXTENSION` and the `DROP` family lock
sub-object 0. The tests of `017` hold ACCESS SHARE to ACCESS EXCLUSIVE on
`ledger_chain` and advisory locks and the builder builds. What can still stop it:
L11-01 (strong locks through a view) and L11-03 (the lock is held to the end of the
caller's transaction, so any role that may call a builder can keep it).

### 4.4 The record of a manual write

A superuser with the setting wrote by INSERT and by `COPY ... (FORMAT csv)` into
`ledger_digest`, by INSERT into `ledger_blocks` with `session_replication_role =
replica`: each leaves one `ledger_removed` row with action `inserted`, rolled back
with the write, and `ledger_verify` reports `manual_write` (and the effect of the
row: `digest_hash`, `digest_json`). Nobody can delete or change the record
(UPDATE, DELETE and TRUNCATE are refused, also for the superuser; the guard
trigger cannot be disabled even by a superuser: `ALTER TABLE` is refused). The
two tables where a manual write leaves no record are the queue and `ledger_removed`
itself (by design, in the code); what they do shows up as `tx_empty` and
`operator_removal` findings (N3). The function that writes the record takes the row
being inserted by the same superuser: no path.

### 4.5 The locks and what the refusals break

Honest operations after the round (superuser, `pg_maintain` member, 19rc2): `pg_dump`
(ACCESS SHARE), `VACUUM`, `VACUUM FULL`, `ANALYZE`, `REINDEX CONCURRENTLY`,
`CLUSTER` and `REINDEX` as statements of their own, `LOCK ... ACCESS SHARE` work; `LOCK`
above ACCESS SHARE, a row lock, `CLUSTER` and `REINDEX` inside a transaction block
are refused for a non-superuser; a superuser may do all. Not run: `pg_repack`,
base backup, logical decoding, `ALTER EXTENSION UPDATE` under load (section 10).
The author's extra finding holds: `ledger_history_sync` and
`ledger_history_acl_sync` refuse a non-superuser; the event trigger calls them as
the extension owner, so `ALTER TABLE ... ADD COLUMN`, `OWNER TO` and
`REASSIGN OWNED` by the (non-superuser) owner of an updatable table work, and the
history table follows (checked: add column, owner change to another role, a
column added after the change, a clean verification).

## 5. The tests of this round

Baseline: both majors pass (section 11). `sql/ledger_paths.sql` and
`t/017_guard_paths.pl` establish their preconditions for the cases they claim,
with the exceptions of L11-04 (ownership of the table in `ledger_register_table`)
and of L11-01 (no case reaches a ledger table through a view). The case
"inheriting member, register a table it does not own" registers the table `docs`,
which is registered already: the refusal in the expected output is `42710 table
"docs" is already under ledger control`, so the test passes with or without the
ownership check.

Mutants (own work; source of the copy changed, built, then all 17 regression
tests and 10 TAP files `001 003 006 007 011 013 014 015 016 017`; M04 with `001 004 014
017`):

| mutant | change | killed by |
|---|---|---|
| M01 | capture: no comparison of the identity on insert | `ledger_paths` |
| M02 | `ledger_remove_stray`: superuser test on the current instead of the outer user | `ledger_paths` |
| M03 | the manual-write record is never written | 12 regression tests, `013`, `015`, `017` |
| M04 | the builder lock in ROW EXCLUSIVE instead of EXCLUSIVE (a weaker mode) | `001` and `014` only (no regression test; run with `004` too: not killed by it) |
| M05 | `kl_check_owner` tests the current user | `ledger_capture`, `ledger_review` |
| M06 | `ledger_register_table`: no "identity was issued" check | `ledger_paths` |
| M07 | `ledger_register_table`: no ownership check | **survives** (L11-04) |
| M08 | the verification record written without the marker | 4 regression tests |
| M09 | the marker lowered only after success, not after an error | `ledger_paths` |
| M10 | `LOCK` refusal only above SHARE | `ledger_guard`, `ledger_paths` |
| M11 | `ledger_history_sync` accepts a non-superuser | `ledger_paths`, `017` |
| M12 | row locks are not refused | `ledger_guard`, `ledger_paths` |
| M13 | the setting counts for a superuser whose outer user is not | `ledger_paths` |
| M14 | `max_clock_skew` in wrong units | `ledger_paths` |
| M15 | `ledger_register_table`: defects not checked | `ledger_paths` |
| M16 | `ledger_register_table`: no persistence check | **survives**: redundant with `ledger_table_defects` |
| M17 | `ledger_register_table`: no `CommandCounterIncrement` before the cursor | **survives**: `SPI` advances the counter after the registration insert |
| M18 | `ledger_verify_recorded` records the current instead of the outer user | `ledger_records` |
| M19 | the guard trigger lets the apply worker delete from the queue | `017` only |
| M20 | capture: no refusal of UPDATE or DELETE on a table without history | **survives**: unreachable, the blocking trigger refuses first |
| M21 | the builder runs in any isolation level | `ledger_capture` |
| M22 | the manual-write record also when the marker is raised | `ledger_head` |
| M23 | a harmless change (control) | none, as it should be |

22 real mutants: 18 killed, 4 survive (M07, M16, M17, M20). M19 and M22 are
killed by one test file only, and the weaker lock mode (M04) by no regression
test and by two TAP files only, which is a thin margin.

Removing the 140 `SET` lines did not make a test of a hand-written row pass for
the wrong reason: the lines are in front of honest calls (`ledger_build_block`,
`ledger_generate_digest`, `ledger_verify_recorded`, `ledger_enable`), and the ERROR lines of the expected
files are unchanged in number (section 3, L10-14).

## 6. Update paths, pg_upgrade, dump and restore

Three databases per major: a new installation, a 0.2 database with data
(append-only and updatable tables, two blocks, a digest, a recorded verification,
a pending transaction) updated to 0.3, and a 0.1 database with data updated to
0.3. The 2029-line catalog dump of each (section 1) is identical for the three
paths on 19rc2 and on 18.7. After the update every old table captures
(insert, update and delete on 0.2 tables, insert on the 0.1 table, also a row inserted
before the update), the builder builds, `ledger_verify` has no finding, a recorded
verification can be made. A digest, an I proof and a D proof made at 0.2 verify
at 0.3 (18.7 and 19rc2).

`pg_upgrade` of an 18.7 cluster holding four ledger databases (0.1, 0.2 updated,
a fresh one and one with updatable tables, blocks, digests) to 19rc2: exit 0; for
each database the number of entries, blocks and the hash of the head are the
same before and after; digests and proofs taken before the upgrade verify after
it; inserts into old tables capture, the builder builds, `ledger_verify` has
no finding; the catalog of the upgraded 0.2 and 0.1 databases equals that of the
fresh one; the guard is on (`ledger_hook_active` true, an INSERT into
`ledger_pending` and `ALTER TABLE ... DISABLE TRIGGER` refused). This is the first run
of the cross-major `pg_upgrade` the previous review did not make.

`pg_dump` (plain and custom) and restore into a new database, on 19rc2: restores
without error; the restored database has a new empty ledger; `ledger_verify` names
the capture trigger of every table that is not registered; the tables then cannot
be written (N2, as documented); `ledger_enable` on a new table works.

## 7. Cost

CPU time of the backend (user plus system, read from `/proc`; the machine was
shared and wall time varied by a factor of three), the parent against the round,
runs interleaved, a ledger table against an ordinary table in the same session.

| workload | parent | this round | note |
|---|---|---|---|
| 20000 single-row autocommit inserts, one ledger table, 4 runs (ticks of 10 ms, median) | 525 | 523 | no difference |
| `INSERT ... SELECT` of 1M rows, one ledger table, 3 runs (median) | 2735 | 2643 | no difference (the ordinary table: 66 to 96) |
| 600k rows into one ledger table, 6 runs (median) | 1475 | 1638 | +11%, inside the spread of 1380 to 1768 |
| 1M rows into a ledger table whose row trigger inserts a row into a second ledger table (2M entries, alternating), 3 runs (median) | 6215 | 8416 | +35% |
| 300k rows alternating into two ledger tables (600k entries), 6 runs (median) | 1711 | 2137 | +25%; higher in all 6 pairs |

The one-entry cache serves a run of rows of one table. When rows of two tables
alternate it misses on every entry and the lookup costs about 7 microseconds
(SPI, one index probe). The capture path was already about 27 microseconds per
entry on this machine; the lookup adds about a quarter to that case and nothing
measurable to the others. A small per-transaction hash would remove it; it is not
raised as a finding because the cost is bounded and the case is unusual.

## 8. New findings

### L11-01 (high): `LOCK TABLE` through a view takes any lock on a ledger table, which stops the builder and every capture

Steps (19rc2 and 18.7; a database with the extension, a role `mnt` that is a
member of `pg_maintain`, a role `own` that owns an updatable ledger table):

```
-- as mnt
create view v_blocks  as select * from kingres.ledger_blocks;
create view v_pend    as select * from kingres.ledger_pending;
begin;
lock table kingres.ledger_blocks in access exclusive mode;   -- ERROR: cannot lock with a mode that blocks readers or writers ...
lock table v_blocks in access exclusive mode;                -- LOCK TABLE
lock table v_pend  in share row exclusive mode;              -- LOCK TABLE
select pg_sleep(12);
-- meanwhile, as the superuser:   set lock_timeout='3s'; select kingres.ledger_build_block();
--   ERROR: canceling statement due to lock timeout   (waiting for a lock on ledger_blocks)
-- meanwhile, as own:             set lock_timeout='3s'; insert into acc values (51,1);
--   ERROR: canceling statement due to lock timeout   (INSERT INTO kingres.ledger_pending ...)
```

`pg_locks` shows AccessExclusiveLock on `kingres.ledger_blocks` and
ShareRowExclusiveLock on `kingres.ledger_pending`, granted to the `mnt` session,
until it ends its transaction. A member of `pg_write_all_data` gets ROW EXCLUSIVE
and SHARE ROW EXCLUSIVE the same way (a view of a view as well). The direct
statement is refused; the view is not, because the check in `kl_check_statement`
(`T_LockStmt`) classifies the relation named in the statement and `LOCK` on a view
locks the tables under it with the privileges of the view's owner. The same bypass
exists in the parent (shown), so L10-03 was fixed for the direct statement only.

Expected: the refusal that README describes ("a lock above ACCESS SHARE on a ledger
catalog table is refused to every role that is not a superuser") for every way to
reach the table. Effect: every transaction that writes a ledger table of the
application waits (the capture inserts into the queue), the worker cannot build,
`ledger_verify` waits; nothing is lost and nothing is forged, the ledger stops for
as long as the transaction is held.

Place: `src/protect.c`, `kl_check_statement`, case `T_LockStmt`. Direction: resolve views
(walk `pg_rewrite` dependencies of a view, recursively) before classifying, or
refuse `LOCK` on any view that depends on a ledger table; add the case to
`ledger_paths`.

### L11-02 (high): `DROP EXTENSION plpgsql CASCADE` removes the ledger, and no protection sees it

Steps (19rc2 and 18.7, also on the parent): the role `carol` is a member of the
owner role with INHERIT TRUE, SET FALSE (not a superuser, cannot `SET ROLE`).

```
-- as postgres: create extension kingres_ledger; create table t(id int);
--              select kingres.ledger_enable('t'); insert into t values (1);
-- as carol:
drop extension kingres_ledger;      -- ERROR: cannot drop extension "kingres_ledger" while ledger tables exist
drop extension plpgsql cascade;     -- NOTICE: drop cascades to 4 other objects
                                    --   drop cascades to extension kingres_ledger
                                    --   drop cascades to trigger kingres_ledger_capture on table t ...
select count(*) from pg_extension where extname = 'kingres_ledger';   -- 0
update t set id = 2;  delete from t;                                  -- UPDATE 1, DELETE 1
```

The ledger functions written in PL/pgSQL (the event trigger `ledger_guard`,
`ledger_enable`, `ledger_verify`, ...) depend on the language; with CASCADE the
drop reaches their extension, which is dropped with the nine tables, all blocks and
digests, the event triggers and the capture triggers of every user table. The
hook of the extension handles `DROP EXTENSION kingres_ledger` and `DROP SCHEMA
kingres`, and the `sql_drop` event trigger is itself dropped by the statement.
The same works for `DROP LANGUAGE plpgsql CASCADE`. A role that inherits the
ownership of the bootstrap extension needs no other privilege. README says the
ledger "can only be switched off outside the database" and lists the known
exceptions; this one is not listed. (`DROP DATABASE` by the same role is listed as
outside, which is the same power; the difference is that this one is silent and
that the protection against dropping the extension is advertised.)

Expected: refusal while ledger tables exist, whatever the path. Place: the
extension has no `object_access_hook`; `src/protect.c` handles statements, not
cascades. Direction: an `object_access_hook` that refuses `OAT_DROP` of the
extension `kingres_ledger` (and of its schema and tables) while ledger tables
exist, which covers every cascade; or write `ledger_guard` and the other
event-trigger functions in SQL or C so that the extension does not depend on
`plpgsql`; list the superuser's catalog route in the README either way.

### L11-03 (medium): a role that may call a builder can stop the block builder for as long as it keeps a transaction open

The lock of `kl_lock_builders` is a transaction lock. A role with only EXECUTE on
`ledger_generate_digest` (what an auditor would be given):

```
-- as aud: begin; select kingres.ledger_generate_digest() is not null; select pg_sleep(12); commit
-- meanwhile, as the superuser: set lock_timeout='3s'; select kingres.ledger_build_block();
--   ERROR: canceling statement due to lock timeout
--   CONTEXT: waiting for ExclusiveLock on object ... of class 3079 of database ...
-- pg_locks: locktype object, classid pg_extension, objsubid 19276 (0x4B4C), ExclusiveLock, granted, aud
```

README and the comment of the SQL script say that no role can request the lock "but
through this code"; true, and enough: the worker's transactions are refused by
`lock_timeout` and retried with a growing delay for as long as `aud` stays in the
transaction, with no entry in `ledger_status` that names the holder. Expected:
at least a statement of it in the README ("Privileges": a role that may call a
builder can delay block building by holding a transaction; set
`idle_in_transaction_session_timeout` or `transaction_timeout` for it), better a
digest function that does not take the lock when nothing is pending. Place:
`src/write.c`, `kl_lock_builders` and `kingres_ledger_generate_digest`.

### L11-04 (medium, test): the ownership refusal of `ledger_register_table` has no test that can fail

Mutant M07 removes `kl_check_owner(relid)` from `kingres_ledger_register_table`: all
17 regression tests and the 10 TAP files pass. The only case ("inheriting member,
register a table it does not own") registers `docs`, which is registered, so it is
refused as `42710 table "docs" is already under ledger control` whatever the owner
check does. The README states "it refuses ... a table that the caller does not own".
Direction: a case that registers an unregistered, fully hand-made table owned by
another role (the shape of the hand-made registration in section 4.1), expecting
`42501`.

### L11-05 (low): a member of the owner role (INHERIT, no SET) can change the schema ACL, the owner of the schema and the registry's sequence

Tried as that member on 19rc2: `REVOKE USAGE ON SCHEMA kingres FROM PUBLIC` (ran; from then on
`ledger_enable`, `ledger_digest` and every other `kingres.` object gave `permission
denied for schema kingres` to the application roles; captures kept working),
`ALTER SCHEMA kingres OWNER TO carol` (ran), `ALTER SEQUENCE
kingres.ledger_table_table_id_seq RESTART WITH 1` and `setval` (ran; the next
`ledger_enable` failed with `duplicate key value violates unique constraint
"ledger_table_pkey"` and succeeded only when the sequence had passed the ids in
use), `CREATE FUNCTION`, `CREATE OPERATOR`, `CREATE TYPE`, `CREATE TABLE` in schema
`kingres` (ran, none of them is found by the fixed statements), `ALTER FUNCTION ...
DEPENDS ON EXTENSION` and `ALTER TRIGGER ... DEPENDS ON EXTENSION` (ran; the
trigger form followed by `DROP EXTENSION` of that other extension is rolled back by
the event trigger, which sees the capture trigger missing). Everything else that
was tried on tables, indexes, functions, event triggers, the extension and the
schema was refused. No write to a ledger table; availability only. Place:
`src/protect.c` (no case for `GRANT ... ON SCHEMA`, sequences,
`T_AlterObjectDependsStmt`, `ALTER SCHEMA ... OWNER`).

### Notes

- N1. The hand-built registration of section 4.1 by a member of the owner role works and
  is equivalent to `ledger_enable` (also entering pre-existing rows). The
  README says so; the member could not have run `ledger_enable` without the
  administrator's GRANT, but it inherits the EXECUTE of the owner.
- N2. A table that has an armed capture trigger and no registry row (after a restore,
  or after a failed hand-made registration) cannot be written, re-enabled or
  dropped by anybody but a superuser through the catalogs; the README documents the
  restore case, not that the tables cannot be dropped.
- N3. A manual insert into `ledger_pending` or `ledger_removed` leaves no `inserted`
  record (by design); a pending row for a transaction without entries makes the next block
  carry a transaction with no entry and `ledger_verify` reports `tx_empty`, a forged
  `ledger_removed` row is reported as `operator_removal`.
- N4. `kl_check_owner` tests the outer user: a SECURITY DEFINER wrapper owned by a table's
  owner that calls `ledger_enable` fails for every caller that is not the owner
  (read from the code, not run). Intended, but it deserves one sentence in the README.

## 9. What held under attack

- No SQL-callable function reaches a write with content of the caller's choice; the
  four writers, `ledger_register_table` and `ledger_remove_stray` computed the same
  values under every setting, `search_path` and temporary object tried.
- The marker was never raised for user code; nothing is left raised after an error.
- `record_row`, the setting through `SET ROLE`, a SECURITY DEFINER function of another
  superuser and a role with the right to set the parameter: refused.
- Superuser without the setting: no INSERT, COPY, no trigger switched off, also
  with `session_replication_role = replica`.
- Manual writes: recorded in the trigger; the record cannot be deleted.
- Row hash: independent of the session text settings for 27 column types.
- Every `ALTER TABLE`, `ALTER INDEX`, rename, move, owner change, `DROP`, `CREATE
  TRIGGER`, `CREATE INDEX`, `CREATE POLICY`, `TRUNCATE`, `REASSIGN OWNED`, `DROP OWNED`,
  `ALTER EXTENSION` of a ledger table, index, function, event trigger or the
  extension, tried as the inheriting member: refused.
- The update paths, `pg_upgrade` across majors, dump and restore: section 6.
- The honest `ADD COLUMN` on an updatable table by its non-superuser owner.

## 10. Not reproduced, and not covered

Not reproduced (suspected, tried, no effect):

- A user trigger that updates the inserted row before the capture trigger on an
  updatable table: reasoned only (entries `D(old)`, `I(new)`, `I(old)` in an order that
  differs from the order of the statements, with equal counts); not run, and an
  owner can do the same with ordinary statements.
- A `pg_temp` object that captures a name of the fixed statements: none found.
- A pending-queue flood or a `LISTEN`/`NOTIFY` storm: not run.

Not covered:

- The behaviour of `kl_manual_write_ok` in a background worker, a parallel worker,
  the logical replication apply worker and inside `ALTER EXTENSION UPDATE`: read, not driven
  (the author's `017` drives the apply and table-sync workers; it passed).
- A prepared transaction that holds entries; two registrations of the same table in
  parallel; `ledger_remove_stray` concurrent with the builder and a crash in its middle
  (the author states they are not tested).
- `pg_repack`-like tools, a base backup, logical decoding of the ledger tables.
- The in-tree build on 18.7 (21 of 21 in the commit message): the out-of-tree
  suites were run instead.
- The verifier.
- A hand-made updatable registration (history table owned by the member).

## 11. Baseline

| major | regression tests | TAP files | TAP tests |
|---|---|---|---|
| 19rc2 (out of tree, `-Werror`) | 17 of 17 | 17 of 17 | 612 |
| 18.7 (out of tree, `-Werror`) | 17 of 17 | 17 of 17 | 612 |

Both on the committed tree, with the library in `shared_preload_libraries`. The
machine was shared (load average between 5 and 38 during the review); no test failed.
