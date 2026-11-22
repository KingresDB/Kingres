<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026 RooyeKhat Media Co -->

# Independent review of phase 11 (distributed schemas), increments 1 to 6

Date: 2026-11-21. Code reviewed: the combined phase 10 + 11 branch at a707948716
(phase 11 increments 1 to 4: 270e70de95 to 8ea64259a4; merge 2cbdfb90f4; increments
5 and 6: 16d5975d26 to a707948716). Design: `kingres/docs/design/shard-phase11.md`
(sections 5 to 9, 12, 15, 18 read). All paths below are relative to
`contrib/kingres_shard/`.

## 1. Verdict

**Not ready to close the phase.** One critical finding (silent loss of live rows
after a schema is moved and moved back), two high findings (schema names that are
not plain lower case break or silently skip registration; the empty-schema
conversion drops privileges and default privileges), and several medium findings
where path S or a move changes the meaning of a statement or of a counter.

What is good: the defence against reading an empty copy held up. I could not make
any plan, COPY, forwarded statement, prepared statement, inlined SQL function,
partition child, cursor or `ONLY` read return the rows of a shell. The existing
tests are real: six mutants of the central defences (end-of-plan check, COPY
refusal, sequence carry-over at the switch, owner check in a forwarded session,
refusal of statements over two owners, refusal of views owned by another role)
were each killed by t/231 to t/239. The 0.7 to 0.8 upgrade produces the same
catalog objects as a fresh 0.8 (737 normalized lines each, no difference).

## 2. How this was done

Ran (PG19 build of the branch, private install, never the shared ones):

- build of the four extensions; TAP t/230 to t/239 (981 tests, all pass) as the baseline;
- five new probe TAP scripts of my own (three-node clusters, logical replication
  where moves were involved) that print the result of each probe; every finding
  marked "reproduced" below comes from one of them;
- a comparison of the catalog (functions with body hash, config and ACL, tables,
  columns, constraints, indexes, views, triggers, event triggers, comments, grants,
  sequences, extension membership) of a database upgraded 0.7 to 0.8 against a
  fresh 0.8;
- six mutants (listed in section 5).

Only read, not run: the PG18 build (the extension is one source; I ran PG19 only),
the planner/standby paths, the 2PC crash points other than those t/237 and t/238
already force, and the check_cluster bound (16d5975d26) beyond reading its diff.

## 3. Findings

### P11R-01 (critical) An old move's cleaning truncates the live data after a move back

File: `kingres_shard--0.7--0.8.sql` (and the same text in 0.8), `internal_continue_cleaning`
(line 3503, the call of `internal_schema_shells(..., false)` at about line 3548),
`internal_schema_shells` (line 3368, the TRUNCATE at 3393), and the checks in
`move_bucket` (line 298, schema branch at about 395 to 440).

Cleaning of a schema move empties the old copy by `TRUNCATE` of every table of the
colocation group on the source group, by name. Nothing stops a second move from
making that same group the owner again while the first move is still in the
`cleaning` phase (default delay: `move_cleanup_delay` = 600 s). The only check at
the start of a move is "the shells on the target are empty" (`internal_schema_shells(c, true)`)
and "no move is in preparing, copying, catching_up or switching". A schema whose tables
were all empty when it was first moved passes both checks on the way back.

Failure scenario (reproduced, three nodes):

1. `distribute_schema('cl', 0)` on a schema with two tables and no rows.
2. `move_schema('cl', 1)`: move 1, phase `cleaning`.
3. `INSERT INTO cl.t SELECT g, 'r' FROM generate_series(1, 100) g` (goes to group 1).
4. `move_schema('cl', 0)`: succeeds, move 2, group 0 owns the schema again. `count(*)` = 100.
5. `cleanup_moved_buckets(true)` (or the background worker once the delay has passed)
   finishes move 1: `TRUNCATE` of the tables on group 0, which is now the owner.
6. `count(*)` on every node = 0. The 100 rows are gone, with no error. Both moves
   are `done`.

A rebalance that moves new, still empty tenant schemas back and forth makes this
reachable without any unusual step. Hash buckets are not affected because their old
partitions are dropped by the name of the move, not emptied by group.

Fix direction: refuse a move whose target group is the source of a move of the same
colocation group that is not `done`/`failed`/`cancelled` (including `cleaning`), or make
the cleaning of move N a no-op when the group is the current owner or the target of a
later move. Add a test: move, write, move back before the cleanup, force the cleanup, count.

### P11R-02 (high) Schema names that are not plain lower case: CREATE TABLE fails, or the table is silently not registered

File: `kingres_shard--0.7--0.8.sql`, `internal_register_schema_tables` lines 3005 and 3011
(`p_nsp::pg_catalog.regnamespace` on a `name`); called from `schema_ddl.c`
`kingres_shard_schema_sync_tables_local` and from `distribute_schema` (lines 3213, 3224).
`internal_register_schema` and `internal_unregister_schema` do it right
(`to_regnamespace(quote_ident(p_nsp))`).

A cast of a `name` to `regnamespace` parses the text as an identifier (case folding,
dots, quotes). Three outcomes, all reproduced with a database that distributes new schemas:

- `CREATE SCHEMA "Ten Ant.1"` works, then `CREATE TABLE "Ten Ant.1".t (...)` fails with
  "invalid name syntax": no table can ever be created in that schema.
- `CREATE SCHEMA "Odd""Name"` works, then `CREATE TABLE` fails with
  `schema "odd"name" does not exist`; `distribute_schema('"Odd""Name"'::regnamespace)` fails the same way.
- A schema with upper case letters, when a schema of the folded name also exists
  (`CREATE SCHEMA tw; CREATE SCHEMA "TW";`): `CREATE TABLE "TW".t` succeeds on every
  node, but the table is **not registered** (the function looked at schema `tw`, which
  has no table). `kingres.dist_table` has no row for it. Reproduced split brain: an
  INSERT on node 1 and an INSERT on node 2 each go to the local copy; a read on node 1
  returns "from n1", on node 2 "from n2", on node 3 nothing. Without a lower-case twin the
  statement fails (error, rolled back), so the silent form needs the twin; the failing form
  needs only an upper case letter, a space, a dot or a quote.

Design section 12 promises the hostile-identifier tests of 2b for schema names; no test
covers upper case or dotted names in a distributed schema.

Fix: use `to_regnamespace(quote_ident(p_nsp))` (or pass the OID) in both places; add tests
with `"Mixed"`, `"a.b"`, `"a b"`, `"q""uote"` and a lower-case twin.

### P11R-03 (high) Conversion of an empty schema loses the schema's privileges and comment; default privileges are never carried

File: `kingres_shard--0.7--0.8.sql`, `distribute_schema` (line 3065): `v_stmts` is empty when
the schema holds no object (`v_has` false), so the other nodes get only
`CREATE SCHEMA %I AUTHORIZATION %I`. With objects, `internal_schema_structure` carries the
schema ACL and comment (line 3658 on) but no `pg_default_acl` row in either case.

Reproduced: `CREATE SCHEMA e1; GRANT USAGE ON SCHEMA e1 TO app; ALTER DEFAULT PRIVILEGES IN
SCHEMA e1 GRANT SELECT ON TABLES TO app;` then `distribute_schema('e1', 1)` and
`CREATE TABLE e1.t (...)`. Node 1: schema ACL `{...,app=U/...}`, table ACL has `app=r`.
Nodes 2 and 3: schema ACL empty, no default ACL, table ACL empty. A `SET ROLE app; SELECT
count(*) FROM e1.t` is refused with "permission denied for schema e1" from the owner
(node 2) even through node 1, and on node 2 directly, while the catalog of the map node
says it is granted. `check_cluster()` is quiet.

The same gap widens rights in the other direction: `ALTER DEFAULT PRIVILEGES ... REVOKE
EXECUTE ON FUNCTIONS FROM PUBLIC` made before the conversion does not exist on the other
nodes, so functions created later are PUBLIC-executable there (the owner node decides).
Section 12 says every GRANT is propagated; a grant made before the conversion is not.

Fix: carry schema ACL, comment and the default ACLs of the schema (and, for a non-empty
schema, the default ACLs too) in the structure, whatever `v_has` says; include nspacl in the
fingerprint-like comparison or in `check_cluster`.

### P11R-04 (medium) Path S evaluates session-dependent built-ins on the owner

File: `planner.c`, `paths_walker` (2801) and `plan_schema_shell` (2913): only user-defined
functions stop path S; built-ins that read the session are shipped as text and evaluated
by the forwarded session on the owner.

Reproduced from a node that does not own the schema (the same statement on the owner
gives the origin's value):

- `SET search_path = s1, public; INSERT INTO t (v) VALUES (current_schema())` stores `pg_catalog`.
- `SET app.tenant = '42'; INSERT INTO s1.t (v) VALUES (current_setting('app.tenant', true))`
  stores NULL (42 on the owner).
- `SET application_name = 'zzz'; ... current_setting('application_name')` stores `kingres_shard`.

For a write there is no other path, so the data is silently different depending on the
node the client is connected to. For a SELECT with `enable_schema_routing` off the target
list is evaluated at the origin by the scan, so the answer changes with the setting, which
ADR 0022 says it must not.

Fix: refuse path S (use the scan, or refuse a write with a reason) when the statement calls
a built-in that is not safe to move: `current_schema`, `current_schemas`, `current_setting`,
`pg_backend_pid`, `inet_client_*`, `pg_trigger_depth` and the like (a list by OID), or
replace them by constants at the origin before the deparse.

### P11R-05 (medium) Triggers, defaults and checks that call functions run with search_path = pg_catalog on the owner

File: `planner.c`, `paths_walker` sets `user_function` only from the statement text;
design 6.4 says path S requires "no function that is not built in" for exactly this reason.

Reproduced: a trigger function in the schema that does `INSERT INTO audit(msg) ...`
(unqualified, the usual style, works for a session with `search_path = s1`). An INSERT on
the owner with `SET search_path = s1` succeeds and fires the trigger. The same INSERT sent
from node 1 through path S fails with `relation "audit" does not exist` from the owner.
The write has no fallback, so a tenant's application works when it is connected to the
owner and fails elsewhere. It is an error, not a wrong answer. The statement walker cannot
see triggers; the schema's `ship_functions` flag does not matter.

Fix: either have the owner run the forwarded statement with the caller's search_path when
the schema has triggers or function-bearing defaults (validated as in 6.4), or document the
rule that functions of a distributed schema must set their own `search_path`, and have
`schema_inventory`/the DDL hook warn for a function without a `SET search_path`.

### P11R-06 (medium) `ship_functions` does not do what section 6.4 describes

File: `planner.c` 2801 to 2900 (`ship_functions` is read only in `paths_note_group`);
nothing sets `search_path` on the owner.

Reproduced with `kingres.alter_distributed_schema('s1', ship_functions => true)`:

- A function of the schema with an unqualified table name in its body, called from a
  statement shipped by node 1, fails on the owner (`relation "audit" does not exist`), as
  it would without the flag; the design says the owner session runs with the caller's
  `search_path`.
- `SELECT public.lf(), count(*) FROM s1.u` with `lf` a function of a local schema is
  shipped (`user_function` is set for any `funcid >= FirstNormalObjectId`, whatever its
  schema) and fails with "function public.lf() does not exist"; section 6.4 says a function
  of another schema is never shipped.

Still an error, not a wrong answer, but the feature is not what the README/design promise.

### P11R-07 (medium) A sequence that was only set with `setval(..., false)` restarts at its start after a move

File: `kingres_shard--0.7--0.8.sql` line 3330 (`internal_schema_sequence_state` keeps only rows
where `pg_sequence_last_value` is not null) and `src/bucket_move.c` 1450 to 1470.

`pg_sequence_last_value` is NULL while `is_called` is false, which is the state after
`setval(seq, n, false)`. Those sequences are not carried. Reproduced: `SELECT setval('q.sq',
5000, false)` on the owner (state `5000|f`), `move_schema('q', 1)`; the new owner reports
`1|f` and the first `nextval` returns 1, below the floor the administrator set and likely
colliding with existing rows. `ALTER SEQUENCE ... RESTART` is not affected (it is propagated
to every node).

Fix: carry `last_value` and `is_called` from `pg_sequence` state (`SELECT last_value, is_called
FROM seq` under the lock) and `setval(seq, last_value, is_called)`.

### P11R-08 (medium) `REASSIGN OWNED` is not propagated; owners diverge between nodes

File: `schema_ddl.c` `kingres_shard_schema_ddl_interesting` (367) and `classify`: the list
has no `T_ReassignOwnedStmt`, `T_AlterTableMoveAllStmt`, `T_CreateExtensionStmt`/`AlterExtensionStmt`
(an extension created `SCHEMA s` puts objects in a distributed schema on one node), and so
on. The file header says unknown statements run on this node only.

Reproduced: `REASSIGN OWNED BY tenantowner TO CURRENT_USER` on node 1 changes the owner
of a distributed schema there; nodes 2 and 3 keep `tenantowner`. `check_cluster()` is quiet.
The owner of the schema/tables decides CREATE and ALTER rights on the node that owns the
rows. (`DROP OWNED BY` is safe: the drop guard refuses it with a clear error.)

Fix: classify `REASSIGN OWNED` (it names roles, not schemas: refuse it, or run it on every
node, when any distributed schema or table is owned by the old role); refuse the other
statements that name a distributed schema indirectly; make `check_cluster` compare schema and
table owners and ACLs.

### P11R-09 (low) A standalone `nextval` can run on the old owner after the sequence state was read

File: `src/bucket_move.c` 1448 to 1470 (state read through a control connection, applied on the
target in the switch transaction). Read only, not reproduced.

The switch holds SHARE on the tables, which blocks writers that take a table lock before
evaluating the default. `SELECT nextval('s.seq')` (no table) is not blocked, so a value
handed out on the source between the read and the commit of the switch is lower than or
equal to a value the target hands out later. The window is the lock phase of the switch.
Mitigation: read the state after the last writer is out (it is), and also `LOCK` the
sequences, or take the state again inside the final transaction with the source sequence
locked against `nextval` (for instance `ALTER SEQUENCE ... OWNER TO` of the same owner takes
the lock the `nextval` needs).

### P11R-10 (low) `UPDATE ... SET id = DEFAULT` on a GENERATED ALWAYS identity column fails when shipped

File: `planner.c` 629 to 665. The fix of increment 6 adds `OVERRIDING SYSTEM VALUE` for INSERT
only. Reproduced: `UPDATE s1.t SET id = DEFAULT WHERE id = 1` from a non-owner fails with
"column "id" can only be updated to DEFAULT" (the rewriter replaced DEFAULT by `nextval(...)`
and the deparse prints the expression); on the owner it works.

### P11R-11 (low) `check_function_bodies = off` is not propagated by DDL

File: `src/node_conn.c` `kingres_shard_exec_ddl` (3647) sets search_path and the literal
settings only. Reproduced: with `SET check_function_bodies = off`, `CREATE FUNCTION s1.later()
... AS 'SELECT count(*)::int FROM s1.notyet'` is accepted on the origin and rejected by the
other nodes (`relation "s1.notyet" does not exist`); the whole statement is rolled back. A
restore of a dump (which sets the flag) into a distributed schema fails for any function
that names a later object. The conversion path sets the flag itself (`internal_schema_structure`),
so only ordinary DDL is affected.

### P11R-12 (low) User operators in `x = ANY(...)`, `IS DISTINCT FROM` and `NULLIF` are not seen by path S

File: `planner.c` 2885 (`IsA(node, OpExpr)` only; the other walker at 1053 handles
`DistinctExpr` and `NullIfExpr`, and neither handles `ScalarArrayOpExpr`). Read only.
A user-defined operator in those forms makes a statement look function-free, so it is shipped
even when the schema does not ship functions.

### P11R-13 (low) The owner-side check of path S is narrower than design 6.2

File: `planner.c` 2747 and the tail of `plan_forwarded` (about 1580 to 1640). The design says
every relation other than the schema's tables and reference tables raises KS001 on the owner;
the code raises KS001 only for schema tables that another group holds. A forwarded statement
that names a local table runs on it. The origin never sends such a statement, and the session
proves it is forwarded, so this is hardening, not a hole.

### P11R-14 (low) `kingres.schemas` is readable by PUBLIC and shows sizes

File: `kingres_shard--0.7--0.8.sql` 2651 to 2680 and 2958. The view lists every distributed
schema with owner, table count and, on the owner node, `pg_total_relation_size` of its tables,
for any role (the view runs with its owner's rights). Design section 12 says the sizes are NULL
for non-superusers. Read only (my probe ran on a node that no longer owned the schemas, so the
column was NULL there).

### P11R-15 (low, documented limit) `nextval` with a non-constant argument advances the local copy

Section 18 documents it; I reproduced it because `nextval(pg_get_serial_sequence('s1.u','id'))`
is a common idiom: from a node that does not own the schema it returns 1 (the shell's counter)
while the owner's counter is at 2, a duplicate. The shell's counter is overwritten at the next
move, but until then two nodes hand out the same number. A warning from the planner when a
`nextval`/`setval` argument names a sequence of a distributed schema indirectly is not possible,
but the README should name `pg_get_serial_sequence` and `DEFAULT` as the safe forms.

## 4. Design steps 2 to 7

| Step | Increment | What was reviewed | Result |
|---|---|---|---|
| 2 catalog, delta functions, `kingres.schemas`, `schema_inventory`, `set_sharding_mode` | 1 | catalog load, `internal_register_*`, apply/snapshot of the catalog, inventory | Upgrade equals fresh install. `register_schema_tables` mis-resolves names (P11R-02). `kingres.schemas` as in P11R-14. Inventory lists operators, collations and text search objects only through the separate refusal in `distribute_schema`; fine. |
| 3 single-source scan, shell defence | 2 | `schema_find`, `plan_schema_shell`, `check_plan_schema_shells`, COPY refusal, `dist_scan.c` | Strong. No path reads a shell (inline SQL function, partition child, `ONLY`, tablesample, cursor, EXPLAIN ANALYZE, prepared generic plans). Mutants of the end-of-plan check and COPY refusal are killed. |
| 4 DDL on a distributed schema, empty `distribute_schema`, 2PC structure statements, fingerprint | 3 | `schema_ddl.c`, `utility.c` hook, `kingres_shard_exec_ddl`, `distribute_schema` | Atomic and verified by fingerprint. Gaps: P11R-02, P11R-03, P11R-08, P11R-11. Fingerprint does not include ACLs or owners (part of P11R-03). |
| 5 path S, `enable_schema_routing` | 4 | `paths_walker`, `deparse_for_remote`, owner check, retry | Correct for table-only statements and compared with the scan on and off. Meaning changes: P11R-04, P11R-05, P11R-06, P11R-10, P11R-12, P11R-13. |
| 6 `move_schema` | 5 | `bucket_move.c` switch, `internal_undo_move`, `internal_continue_cleaning`, sequences | Rows written during the move are safe (t/237 and my reading). Data loss after a move back: P11R-01. Sequences: P11R-07, P11R-09. Undo and the failed-before-switch case look right. |
| 7 conversion with data in place, `undistribute_schema`, several schemas on one group | 6 | `distribute_schema` with objects, `internal_schema_structure`, `undistribute_schema`, same-owner statements | Locks, owner group and refusal of another group while rows exist are right; the role-missing case rolls back everywhere. `undistribute_schema` keeps every row on the owner (reproduced, 50 rows). Not carried: P11R-03. Same-owner statements work and are tested; the different-owner refusal is covered (mutant killed). |

## 5. Mutants run

Each applied alone to a private copy, with a clean rebuild, and run against t/230, 231, 233,
235, 237, 238, 239.

| Mutant | Killed by |
|---|---|
| end-of-plan check (`check_plan_schema_shells`) removed | t/231 (three assertions) |
| `refuse_copy_of_schema_shell` made a no-op | t/231 (ten assertions) |
| sequence `setval` on the target at the switch removed | t/237 (sequence goes on after the move) |
| owner-side `schema_forwarded_check` removed | t/231 and t/235 (stale map, wrong node) |
| statement over two owners no longer refused for path S | t/235 (stops at the first cross-owner join) |
| view-owner refusal removed from path S | a test of the 233 to 239 group ("the same read from another node is refused with the reason") |

No mutant survived. The tests that are missing are the ones behind the findings above:
move back before cleanup (P11R-01), non-lower-case names (P11R-02), grants made before a
conversion (P11R-03), session built-ins in a write (P11R-04), triggers through path S
(P11R-05), `setval(..., false)` before a move (P11R-07), `REASSIGN OWNED` (P11R-08).

## 6. Summary by severity

- Critical: P11R-01.
- High: P11R-02, P11R-03.
- Medium: P11R-04, P11R-05, P11R-06, P11R-07, P11R-08.
- Low: P11R-09 to P11R-15.
