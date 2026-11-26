<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
# kingres_shard: review of the fixes for the step 12 review (P12R-01 to P12R-07)

## Scope and method

Scope: commit 651dd867e6 (the fixes) and the tip of m18, 28a28bf518. The tip changes only t/246 (one trailing blank); it has no
other effect on contrib/kingres_shard.

Method: reading of the diff and of the code around it; builds and runs on PostgreSQL 18 in a private copy of the install
(PostgreSQL 19 was not run; the header of both majors declares `RelationGetPrimaryKeyIndex(Relation, bool)` with two arguments, so the
call compiles on both). Runs: t/247, t/249, t/250, t/251 (all pass unmutated, 55 tests), the whole pg_regress suite (23 of 23 pass,
including `catalog` with the context `superuser` and `upgrade`), mutants for each fix, and one TAP probe for the missing-primary-key
path. The rest of the TAP suite was not run.

## Verdicts

| Id | Verdict | Evidence |
|---|---|---|
| P12R-01 (verify 'full' on columns without equality) | Fixed | t/249 fails with the old comparison (3 of 7 fail, `operator does not exist: json = json`). The new comparison is sound, see below. |
| P12R-02 (stale generated 0.8 script after a merge) | Fixed | t/250 fails when the 0.7--0.8 script is changed and 0.8.sql is not, and when 0.7.sql alone is changed. It does not pass vacuously. |
| P12R-03 (cached primary key OID) | Fixed, with a new defect (P12F-01) | t/247 section D fails with a cached OID. REINDEX CONCURRENTLY, rename and drop/re-create of the key are handled because the index comes from the open relation. |
| P12R-04 (add_node with no schema left) | Fixed | t/251 A fails with the early return restored (tests 7 and 8). |
| P12R-05 (missing role) | Partly fixed | t/251 B passes, and fails on its hint test when the role rule is removed. The rule is a message match (P12F-02), and the role pre-check order from the report was not done. |
| P12R-06 (deleted rows not seen) | Documented only | README states the limit; no code change, as the report allowed. |
| P12R-07 (sha256, quadratic cost, SUSET) | Fixed for sha256 and SUSET; quadratic cost left as documented | Hash entry is 72 bytes for a 64-character digest plus NUL; the fingerprint is compared as a string, with no fixed buffer. `catalog_incremental` is PGC_SUSET, regress `catalog` shows context `superuser`. |

## Checks of the points asked for

verify 'full' as text. The comparison `(c.a::text COLLATE "C", ...) IS DISTINCT FROM (EXCLUDED.a::text COLLATE "C", ...)` runs in one
statement, so both sides are printed by the same session with the same TimeZone, DateStyle, IntervalStyle, bytea_output and
extra_float_digits. A different TimeZone on two nodes cannot matter. The statement is run through the internal connection, which sets
`extra_float_digits = 3` (KS_FIXED_SETTINGS_SQL), so float text is the shortest exact form and not lossy; I confirmed that with
extra_float_digits = 0 the text of 0.1+0.2 is `0.3` (lossy), so the function relies on that setting and does not set it itself
(see P12F-03). Float -0 against 0 prints `-0` and `0`: a row is rewritten (a harmless extra write, and the values are then bit-equal).
NaN prints the same. numeric 1.0 against 1.00 differ in text: a rewrite, which keeps the scale (stricter than the equality
operator, correct for a copy). Arrays, domains and composites print through their element types. NULL is handled by IS DISTINCT FROM
on the row. A nondeterministic collation cannot call two strings equal because the cast result is forced to "C". A single non-key
column gives `(x) IS DISTINCT FROM (y)`, which is a scalar comparison and works. I found no false "equal" and no wrong rewrite.

Primary key from the open table. `RelationGetPrimaryKeyIndex` reads the index list of the relcache entry, which every change of the
indexes of the table invalidates (REINDEX CONCURRENTLY, DROP and ADD of the constraint, ALTER INDEX RENAME), and `table_open` accepts
the messages first. A renamed index is found (t/247 D). The scan key is the first column, which every re-created key shares.

add_node cleaning. The clean path runs only when `add_node_progress` exists on the node, after the check that the node has no
`local_node` and no `dist_table` row (so a node that belongs to a cluster is refused before anything is dropped). It drops only the
schemas that the progress table names, and the DROP and the DELETE of the progress row are one simple-query message, hence one
implicit transaction. A crash in the middle leaves either both or neither, and the next call repeats it. I found no way for it to
delete a schema the call did not make, other than a user who creates a schema of the same name as a progress row on a node that is
not yet in a cluster (the same assumption as before the fix for the non-empty list).

catalog_incremental. The variable is defined in `_PG_init` through `shared_preload_libraries`, so the context change takes effect
at once. Values in postgresql.conf and ALTER SYSTEM are still read (they are applied by the postmaster and on reload, whatever
the context of the variable). A role-level or database-level setting is applied at login for a superuser; for an ordinary role it is
ignored, which is the intent. The test helper in t/KingresShardTest.pm sets it in the conf file, which still works.

t/250. It runs the real generator into a temporary file and compares byte for byte. Mutants: a changed 0.7--0.8 script with an
unchanged 0.8.sql fails; a changed 0.7.sql fails. The only way to pass without comparing is the `skip_all` when
`mk-install-script.sh` is missing; in the repository it is present.

## New findings

### P12F-01 (medium): a missing primary key of a catalog table blocks every statement, including the repair

File: `contrib/kingres_shard/src/catalog.c` lines 1124-1131 (`lazy_fetch`, the new `ereport(ERROR)`).

Scenario (reproduced on PostgreSQL 18, three nodes, one distributed schema): on the map node run
`ALTER TABLE kingres.dist_schema DROP CONSTRAINT dist_schema_pkey`. The drop succeeds. Then
`ALTER TABLE kingres.dist_schema ADD CONSTRAINT dist_schema_pkey PRIMARY KEY (nsp)` fails with `the table "dist_schema" of the
extension has no primary key` and the hint "Restore the primary key". So does `CREATE UNIQUE INDEX` on it, also with
`kingres_shard.enable_schema_routing = off`. The same happens for `dist_table`; there a plain `CREATE TABLE public.loc (id int)` in the
same session also fails. The statement that the hint asks for cannot be run, because the hook looks up the schema of the statement
through the same lazy lookup. Before the fix the answer was silently wrong ("local table"); now the cluster cannot be repaired from SQL
(the way out is a restart without the preload library). t/247 D hides this because it drops the key inside a transaction that it rolls
back, and it only runs a SELECT.

Fix: when the open relation has no primary key, do not raise an error; scan the heap with no key index
(`systable_beginscan(rel, InvalidOid, false, snap, 1, &skey)` is correct and only slower), and raise a WARNING once per backend, or
raise the error only in the planner path and never in the utility hook. Add to t/247 D: drop the key (committed), run
`ALTER TABLE ... ADD PRIMARY KEY` and check that it succeeds and that a statement on a schema table answers.

### P12F-02 (low): the role rule matches the English text of the message

File: `contrib/kingres_shard/src/node_admin.c` lines 842-846 (`stage_error_is_dependency`).

`strstr(msg, "role \"")` depends on the language of the messages of the new node. With `lc_messages` set to another language
(a translation that does not keep the word and the quote) the test is false, the missing role is again taken as a dependency, and the
schema is deferred to the last transaction: the old behaviour of P12R-05 (late failure after every batch, without the hint). The
pattern also matches other messages that contain `role "` (for example a table named that way), which is only a theoretical
misclassification.

Fix: decide on the node instead of on the text: run `SELECT 1 FROM pg_roles WHERE rolname = ...` for the roles of the schema
structure before the staging (the check of ACL roles that the earlier report suggested), or send `SET lc_messages = 'C'` on the
staging session, which makes the match stable.

### P12F-03 (low): `internal_split_switch` takes the float text from the caller's session settings

File: `contrib/kingres_shard/kingres_shard--0.7--0.8.sql` line 1842 (and `kingres_shard--0.8.sql`), the comparison of `v_dl` and `v_dr`.

The text of a float4, float8 or a geometric type depends on `extra_float_digits`. The function does not set it; it is right today only
because the call goes through the internal connection, which sets `extra_float_digits = 3`. A call of the function by a
superuser in a session that has `extra_float_digits = 0` (a common setting for old drivers), or a future change of the call path, would
make two floats that differ in the last bits compare as equal, and 'full' would miss the difference. Fix: add
`SET extra_float_digits = 3` to the function definition (the fingerprint functions of the same script already do this), and a case
in t/249 with a float8 column and two values that differ in the 17th digit.

## Mutants

| Mutant | Result |
|---|---|
| Both scripts: old unconditional-equality comparison restored (`DO UPDATE ... WHERE (c.cols) IS DISTINCT FROM (EXCLUDED.cols)`) | t/249 fails (tests 1, 2 and 6, `operator does not exist: json = json`); t/250 passes (it compares generated with committed, both mutated) |
| Only the 0.7--0.8 script mutated, 0.8.sql unchanged | t/250 fails on the byte comparison |
| A comment line appended to kingres_shard--0.7.sql only | t/250 fails |
| `lazy_fetch` keeps the first primary key OID for the process | t/247 fails (D: the write after the REINDEX, and the missing-key message) |
| `if (names == NIL) return 0;` restored in `stage_schemas_on_node` | t/251 fails (A: schemas and progress rows left on the node) |
| role rule removed from `stage_error_is_dependency` | t/251 fails (B: the hint to repeat the call) |

The probe test for P12F-01 was a temporary file in the worktree and was removed. All mutants were reverted, the worktree is removed,
and the private install and work area are deleted.

## Fixes of P12F-01 to P12F-03

- P12F-01: `lazy_fetch` no longer raises an error when the open table has no primary key; it scans the heap with no index (the answer is the same, only slower) and
  raises a WARNING once for a backend, with the statement that restores the key as the hint (the warning is raised again if the key came back and went again).
  The cache holds answers and not the way they were found, so it is not touched.  `t/247` section E drops the key of `dist_schema` and of `dist_table` and
  commits, checks that a schema table answers, that a write is routed, that an unrelated `CREATE TABLE` and `CREATE UNIQUE INDEX` run, repairs with
  `ALTER TABLE ... ADD PRIMARY KEY` and checks that routing works.  Mutant (the error restored): section D (two tests) and section E fail.
- P12F-02: the roles that the distributed schemas name (owners and grantees of the schemas, their relations, functions, types and default privileges) are looked up
  on the new node with a query on `pg_roles` before any schema is made there (`stage_check_roles`); the error names the roles and the hint says to create them and to repeat
  the call.  As a second line, the staging session sets `lc_messages = 'C'` and the text rule reads the primary message of the node, which must begin with `role "`.
  `t/251` B covers the pre-check (a function owner, a default-privilege grantee, and that no schema is made).  Mutant (pre-check removed): three tests of `t/251` fail.
  The second line is not covered by a test: no role can reach it any more once the pre-check has run, and the test machine has no translated messages.
- P12F-03: `internal_split_switch` has `SET extra_float_digits = 3`; `kingres_shard--0.8.sql` was regenerated.  `t/249` checks `proconfig` of the function.  Mutant
  (the setting removed from both scripts): `t/249` fails.
