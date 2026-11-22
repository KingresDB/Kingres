<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026 RooyeKhat Media Co -->

# Independent review of phase 11: the fixes of the first review, and steps 8 to 11

Date: 2026-11-21. Code reviewed: branch shard-p1011 at 583f86652e.
Fixes: 8f6323de74 and 0e9b05a3d2 (P11R-01 to P11R-15).
Steps: eeaceca8fe (8 and 10), 2990a7cb51 (11), 8a2814e82c and a5a36053bb (9).
Design: `kingres/docs/design/shard-phase11.md`, sections 6.4, 6.7, 7.9, 8.3, 11.1, 16, 18.
Paths below are relative to `contrib/kingres_shard/` unless they name `kingres_ledger`.

## 1. Verdict

**The fixes are good; steps 8, 10 and 9 are sound for data; step 11 is not closed.**

- All of P11R-01 to P11R-12, P11R-14 are fixed, and every one of the five probe scripts of the first review now
  gives the right answer. The critical one (P11R-01) is closed by a refusal, and the data survives the same scenario.
  P11R-09, P11R-13 and P11R-15 stay as documented limits and I accept that for 09 and 15 (section 3).
- Step 11 has two ways, both reproduced, to end with a ledger table that is written outside the rules of the ledger
  (UPDATE and DELETE of an append-only table succeed): `ledger_enable` run while the schema is being moved (P11S-01),
  and `distribute_schema(nsp, group)` of a schema with an empty ledger table to another group (P11S-02). Both are
  found afterwards by `check_cluster` (`ledger_on_non_owner`), but the ledger guarantee is broken until then, and in
  the first case the cleaning of the old copy then fails for good. Two high findings. Do not close phase 11 before
  they are fixed.
- Step 8 changes the meaning of a statement in one case that the design says must be refused: a shipped function that
  reads a local table reads the owner's copy (P11S-03, medium).
- Steps 9 and 10: I could not make any of them lose, duplicate or misplace a row, or read an empty shell. A scan
  error (not wrong data) in UNION ALL with an expression column (P11S-04) makes the fallback fail for some queries; it
  also fails for a hash table, so it is older than phase 11.
- The upgrade 0.7 to 0.8 gives the same catalog as a fresh 0.8 (875 normalized lines each, no difference).
- No SQL injection and no privilege hole found: the internal functions are not callable by an ordinary role, hostile
  schema, table and column names (quotes, dots, spaces, commas) work through SET SCHEMA, CREATE TABLE AS and the
  `search_path` that the owner receives (it travels as a bound parameter, every name goes through `quote_identifier`).

## 2. What I ran and what I only read

Ran (PG19 build from `git archive 583f86652e`, private install copy; the PG18 build was not run):

- build of kingres_sketch, kingres_columnar, kingres_timepart, kingres_ledger and kingres_shard;
- TAP t/239 to t/244 on the unmodified code: 406 tests, all pass;
- the six probe scripts of the first review (900 to 905), re-run against the fixed code (section 3);
- three new probe scripts of three nodes with logical replication (ledger during a move and ledger rules, steps 9 and 8
  and 10, privileges and hostile names): about 150 probes;
- the 0.7 to 0.8 upgrade against a fresh 0.8 (functions with body hash, config and ACL, relations with ACL and comment,
  columns, constraints, indexes, views, triggers, event triggers, extension membership, sequences, default ACL);
- 22 mutants (section 6).

Only read: the PG18 build; the standby and failover paths; the crash points of the two-phase DDL beyond the ones t/237
and t/238 force; a concurrent writer racing `ALTER TABLE ... SET SCHEMA` (P11S-08, not reproduced); the control plane
and the panel (outside this review).

## 3. The fixes of the first review (P11R-01 to P11R-15)

| ID | Sev. then | Verdict | Evidence |
|---|---|---|---|
| P11R-01 move back truncates live rows | critical | **Fixed** | `move_bucket` now refuses a target group that is the source of a move of the same colocation group in `cleaning` ("still being cleaned by an earlier move"); re-run of the probe: the move back is refused, the 100 rows are there before and after the forced cleaning, both moves end `done`. A second probe (create a table while cleaning, write, move to a third group, clean) keeps 20 and 5 rows. The cleaning of a group that owns the schema again does nothing (second guard). Mutant K4 (guard removed) is killed by t/243. |
| P11R-02 names that are not plain lower case | high | **Fixed** | `internal_register_schema_tables` uses `to_regnamespace(quote_ident(...))`. Probe: `"Ten Ant.1"`, `"Odd""Name"`, `"MixedCase"` next to `mixedcase` all register on every node; the twin case reads the same rows from every node. Mutant K10 (old cast back) killed. |
| P11R-03 privileges of an empty schema | high | **Fixed** | ACL, comment and default privileges of an empty schema are on all three nodes (probe A: ACL, table ACL and `pg_default_acl` count equal on every node). |
| P11R-04 session built-ins in path S | medium | **Fixed** | `current_schema()` gives the session's schema (`s1`), `current_setting('app.tenant', true)` gives 42 and `application_name` gives `zzz` when written through a non-owner, like on the owner. |
| P11R-05 triggers and defaults with unqualified names | medium | **Fixed** | A trigger that inserts into `audit` works from a non-owner with `search_path = ra`, and fails the same way on both nodes without it. The router (COPY and INSERT ... SELECT, a5a36053bb) does the same, also with a path that contains `a, b` and `q'q` as schema names. Mutant K6 (the `set_config` of the path removed) is killed. |
| P11R-06 `ship_functions` | medium | **Fixed, one hole remains** | A function of a local schema is no longer shipped (result `7|1`); the owner runs a shipped statement under the caller's path. The remaining hole is P11S-03. |
| P11R-07 `setval(..., false)` | medium | **Fixed** | State `5000|f` is on the new owner and the first `nextval` returns 5000. Mutant K9 killed. |
| P11R-08 `REASSIGN OWNED`, extensions | medium | **Fixed** | `REASSIGN OWNED` of a role that owns a distributed schema is refused (owner unchanged on the three nodes); `CREATE EXTENSION ... SCHEMA` a distributed schema is refused and leaves nothing on any node. |
| P11R-09 standalone `nextval` during the lock phase of a switch | low | **Accepted as a documented limit** | Section 18 of the design and the README name it and say to use the sequence as a default. `nextval('s.q')` with a constant name is shipped to the owner (the counter is shared: 1 on the owner, then 2 from node 1), so the window is the lock phase of one switch only. |
| P11R-10 `UPDATE ... SET id = DEFAULT` | low | **Fixed (by refusal)** | Refused with "it sets an identity column to DEFAULT" from a non-owner, works on the owner. Mutant K11 is killed by t/243. |
| P11R-11 `check_function_bodies` | low | **Fixed** | A function that names a later object is created with the setting off (probe I, rc 0). |
| P11R-12 user operators in ANY, DISTINCT FROM, NULLIF | low | **Fixed** | Covered by t/243 (passes); not re-probed by me. |
| P11R-13 owner-side check narrower than design 6.2 | low | **Accepted for a plain statement; not acceptable with step 8** | Before step 8 the origin never sent a statement that names a local table, so it was hardening. With `ship_functions` the body of a function is the way to send one: see P11S-03. |
| P11R-14 `kingres.schemas.size_bytes` | low | **Fixed** | A role that is not a superuser sees an empty size. |
| P11R-15 `nextval(<expression>)` advances the local copy | low | **Accepted as a documented limit** | The README names the safe forms (constant regclass, the default). I reproduced the duplicate (1 from a non-owner while the owner is at 2) as before. It is a silent duplicate key for a user who writes `nextval(pg_get_serial_sequence(...))` from a non-owner; the limit is stated in the README text of the fixes and in design 18. A NOTICE from the planner for a non-constant `nextval` argument when a distributed schema exists would cost little; not required. |

## 4. Steps 8 to 11

| Step | Commit | Result |
|---|---|---|
| 8 `ship_functions` | eeaceca8fe | Works as designed for functions of the schema and for built-ins; functions of other schemas are planned at the origin; the path of the session reaches the owner; a function of the schema that reads another distributed schema is refused by the owner. **One gap: a function that reads a local table runs on the owner's copy without error (P11S-03).** Privileges of `alter_distributed_schema`, `move_schema`, `distribute_schema`, `undistribute_schema` and every internal function are refused to an ordinary role. |
| 10 single tables in and out | eeaceca8fe | Into a schema (empty table, or this node holds the schema and the rows), out of a schema (owner keeps table and rows, shells are dropped), between two schemas of one owner, and a table with serial, quoted names and a view on it all work, and `check_cluster` stays quiet. Refused as designed: rows on a non-owner (message names the groups), a foreign key across the border, a ledger table (by the ledger), a trigger. A schema moved after a table came in by `SET SCHEMA` copies the table and the cleaning is right. No row lost or left in a shell in any probe. Gaps: P11S-08 (a window), P11S-06 (a test does not cover one branch). |
| 11 ledger tables | 2990a7cb51 | The normal path works (ledger on the owner, writes from other nodes, router writes and COPY from other nodes are captured on the owner, truncate, drop, trigger change refused, pin on move and undistribute, `ledger_on_non_owner`). **Two ways around the pin: P11S-01 and P11S-02.** A user trigger named like the ledger's is silently not propagated (P11S-07). |
| 9 writes that read other schemas or kinds | 8a2814e82c, a5a36053bb | INSERT ... SELECT from a local table, a hash table, a table of another owner, the same table, a union, a join with a local table; COPY FROM PROGRAM; CREATE TABLE AS and SELECT INTO (with a role that is not a superuser: the table is owned by that role on every node; inside a transaction with rollback or with writes after; from a view; from a shell; hostile column names): rows only on the owner, defaults and sequences are the owner's, nothing in a shell, a constant `nextval` in the select list is refused, not guessed. Gaps: P11S-04, P11S-05, P11S-06 (a test does not cover the path reset). |
| fixes 8f6323de74, 0e9b05a3d2 | | See section 3. |

## 5. New findings

### P11S-01 (high) `ledger_enable` during `move_schema`: the moved ledger table loses its ledger and can be changed

File: `kingres_shard--0.8.sql` (and the same text in 0.7--0.8): `move_bucket` at 11864, the only
check of the pin is `internal_schema_precheck` at the start of a move (call at about line 11996), which calls
`internal_schema_pinned` (16020). `src/kingres_ledger.c`, `kl_check_distributed_owner` (255) checks only that the
node is the owner at the time of the call.

Scenario (reproduced, three nodes): schema `lg` on node 2 with a table of 10 rows; `move_schema('lg', 2)` is held
after the catch-up (`move_after_catchup:wait`); on node 2, `kingres.ledger_enable('lg.t')` succeeds (the schema is
not pinned yet; nothing locks the tables during the copy); the move then completes. Result: the ledger table is
registered and has its three triggers on node 2, which is no longer the owner; the new owner (node 3) holds the rows
(11, one written during the move) with no trigger. Then, through any node: `UPDATE lg.t SET v = 'tampered' WHERE id = 1`
succeeds, `DELETE FROM lg.t WHERE id = 2` succeeds, new rows are not in the ledger (11 entries on node 2 stay 11).
`check_cluster` reports `ledger_on_non_owner:2:lg.t`, but the cleaning of the old copy now fails for good
(`could not clean up bucket move 1: table "t" is an append-only ledger table`), so the move never reaches `done`.

Fix direction: take the same decision at the switch: have the switch (on the source, under its lock) call
`internal_schema_pinned` again and abort the move if it is set; and make `ledger_enable` refuse a table of a schema
whose state is not `active` or that is the colocation group of a move that is not finished.

### P11S-02 (high) `distribute_schema(nsp, other_group)` takes an empty ledger table to a group without its ledger

File: `kingres_shard--0.8.sql`, `distribute_schema` (14797): the rule that the data decides the group is applied only
when a table holds rows (line 14917, "holds rows on shard group %, not on group %"); step 11 removed the refusal of
ledger tables from `schema_inventory` (about line 13957).

Scenario (reproduced): on node 1, `CREATE SCHEMA lz; CREATE TABLE lz.t (...); SELECT kingres.ledger_enable('lz.t');`
then `SELECT kingres.distribute_schema('lz', 1)` succeeds (owner group 1). The ledger and the triggers stay on node 1;
the rows go to node 2, which has a plain table. An `INSERT` through node 1 is not captured (0 entries on node 1), and
the table can be updated and deleted without limit. `check_cluster` says `ledger_on_non_owner:1:lz.t`.

Fix direction: when the schema holds a registered ledger table, `distribute_schema` takes only the group of the node
that holds the ledger (refuse an explicit other group, with the reason), whether or not the table has rows.

### P11S-03 (medium) A shipped function reads the owner's copy of a local table (design 6.4 says KS001)

File: `src/planner.c` `paths_walker` / `plan_forwarded` (about 2776 to 2900 and the tail of `plan_forwarded`); design
6.4 ("A function body that touches ... a local table ... the owner raises KS001"). This is P11R-13 made reachable by
step 8.

Scenario (reproduced): `kingres.alter_distributed_schema('ra', ship_functions => true)`; a function `ra.cnt_loc()`
that does `SELECT count(*) FROM public.loc`, where `public.loc` is a table that exists on every node with different
rows (5 rows on node 1, 2 on node 3). `SELECT ra.cnt_loc(), count(*) FROM ra.t` returns 5 and the owner's tag from
node 1 and from node 3 alike; run alone on node 3 the function would count 2. The answer depends on whether the
schema opted in. Reads only (a write has the same effect: a row written from node 3 gets the owner's tag).

Fix: raise KS001 in the owner session for any relation that is not a table of a distributed schema held by this
group, a reference table or a catalog (the check that the design describes), or refuse the shipment when a function
of the statement reads a table outside the schema (`pg_depend` of the function is not enough for dynamic SQL, so
the owner-side check is the one to have).

### P11S-04 (medium) The distributed scan fails for a UNION ALL with an expression column; the fallback does not always answer

File: `src/dist_scan.c`, line 531 (the check "column N is needed above the scan but the local scan does
not return it"). Not in phase 11's own code: the same error is given for a hash-distributed table.

Scenario (reproduced, also with `enable_schema_routing` off, on a schema table and on a hash table):
`SELECT id + 1, v FROM ra.t UNION ALL SELECT id + 2000, tag FROM public.loc` from a node that does not own `ra`
fails with `distributed scan of "t": column 1 is needed above the scan but the local scan does not return it`; the same
with a subquery and `ORDER BY`. The statement works on the owner and works without the expression. An INSERT ...
SELECT of that shape through the router fails the same way. It is an error, not a wrong answer, but ADR 0022 makes the
scan the fallback for every query. Not reported by the first review because it is not specific to schemas.

### P11S-05 (low) `CREATE TABLE ... AS`: `IF NOT EXISTS` and `AS EXECUTE`

File: `src/utility.c` `schema_ctas` (about 190 to 255). Reproduced:
`CREATE TABLE IF NOT EXISTS ra.c1 AS SELECT 2` on an existing table raises `relation "c1" already exists` (PostgreSQL
says NOTICE and skips); `CREATE TABLE ra.c5 AS EXECUTE pp` (prepared statement, with or without a parameter) fails with
`unexpected utility statement type` (the table creation runs first, and is rolled back with the error). No data
problem; document it or refuse the two forms up front, with the reason.

### P11S-06 (low, test gap) Mutants that survive

- K13: remove `kingres_shard_schema_table_op_local("assert_empty", ...)` in `src/utility.c` (the origin node's own shell
  of a table that leaves its schema, before it is dropped): t/239 to t/244 all pass. A shell that holds a row (written
  with routing off) would be dropped with it. The remote copy of the same check (K7) is killed.
- K16b: replace `SET LOCAL search_path = pg_catalog` in `kingres_shard_batch_finish` (`src/batch_sender.c`) by a
  no-op: all of t/239 to t/244 pass. The reset keeps the path of a router batch from reaching later statements of the
  same transaction on the same connection; nothing tests it. I could not build a case that changes a result with this
  mutant (names in a shipped statement are qualified), so the severity is low.
- The header of t/244 promises a test "the owner moves while a statement is planned"; the file has no `move_schema`.

### P11S-07 (low) A user trigger named `kingres_ledger_*` is silently local

File: `src/schema_ddl.c` (the `CreateTrigStmt` case, `strncmp(t->trigname, "kingres_ledger_", 15)`) and the
fingerprint and structure functions (`tgname NOT LIKE 'kingres!_ledger!_%'`).
Reproduced: `CREATE TRIGGER kingres_ledger_mine BEFORE INSERT ON ld.u ...` on a plain table of a distributed schema
is created on the node where it is run only (1 trigger, 0 on the others), never fires for the rows that the owner
receives, and the fingerprint and `check_cluster` do not see the difference. Fix: skip propagation only when the
table is a registered ledger table, or refuse the prefix for other tables.

### P11S-08 (low, not reproduced) `SET SCHEMA` into a schema checks the rows before it locks the table

File: `kingres_shard--0.8.sql` `internal_set_schema_check` (15765, the `SELECT EXISTS (SELECT 1 FROM ONLY ...)`) is run
by `set_schema_table` (`src/schema_ddl.c`) with no lock on the table; the lock comes when the statement itself runs.
A row committed by another session in between stays in a shell of a node that does not own the schema, where no read
sees it. The window is small. Fix: take the lock the statement needs (ACCESS EXCLUSIVE) before asking.

## 6. Mutants

Each applied alone to a private copy, rebuilt, and run against the named files (t/239 to t/244 and t/237 as marked).
"Killed" means at least one assertion or the test itself failed.

| Mutant | Result |
|---|---|
| K1 ledger: the owner check in `ledger_lock_table` removed | killed (10 assertions, t/242) |
| K2 the ledger triggers are propagated by the DDL hook again | killed (t/242 stops early) |
| K3 `internal_schema_pinned` always NULL | killed (11, t/242) |
| K4 the P11R-01 refusal removed | killed (3, t/243) |
| K5 the router sends columns the statement did not name | killed (t/244 stops early) |
| K6 the owner gets no `search_path` in a router batch | killed (t/243) |
| K7 `internal_schema_assert_empty` never raises | killed (5, t/241) |
| K8 the rows check of `internal_set_schema_check` removed | killed (6, t/241) |
| K9 only `is_called` sequences carried (P11R-07) | killed (1, t/243) |
| K10 the name cast of P11R-02 back | killed (t/243) |
| K11b the refusal of `SET id = DEFAULT` removed | killed (1, t/243) |
| K12 the pin check in `undistribute_schema` removed | killed (5, t/242) |
| K13 the origin's own `assert_empty` for a shell that leaves | **survives** |
| K14 `check_cluster` without the ledger check | killed (1, t/242) |
| K15 `COPY` sends columns that were not named | killed (1, t/244) |
| K16b the reset `SET LOCAL search_path` after a router batch | **survives** |
| K19 `ENABLE ALWAYS TRIGGER` of a ledger trigger propagated | killed (t/242) |
| K20 `COPY FROM` into a shell refused again | killed (13, t/244) |
| K23 the fingerprint counts the ledger triggers | killed (2, t/242) |
| K24 the structure copy carries the ledger triggers | killed (2, t/242) |
| K25 `COPY` demands the distribution column of a schema table | killed (11, t/244) |

The first runs of K11 and K16 did not compile (an unused variable); their results were discarded and the mutants were
redone as K11b and K16b.

## 7. Where the existing tests do not reach

No test covers any of: `ledger_enable` while a move is running (P11S-01); `distribute_schema` with an explicit group
of a schema that has an empty ledger table (P11S-02); a function of a schema with `ship_functions` that reads a local
table (P11S-03); UNION ALL with an expression column from a non-owner (P11S-04); `CREATE TABLE IF NOT EXISTS ... AS`
and `AS EXECUTE` (P11S-05).

## 8. Summary by severity

- Critical: none.
- High: P11S-01, P11S-02.
- Medium: P11S-03, P11S-04.
- Low: P11S-05, P11S-06, P11S-07, P11S-08.
