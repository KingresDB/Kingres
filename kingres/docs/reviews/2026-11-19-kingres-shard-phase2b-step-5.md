<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->

# Review of kingres_shard phase 2b, step 5: the analysis module and the colocated INSERT ... SELECT

Date of the review: 2026-11-19.  Reviewer: independent; the reviewer did not write the change.

## 1. Scope and method

Reviewed: the exported tree (extension version 0.6, `contrib/kingres_shard`), the diff of step 5 (`src/pushdown.c`,
`src/pushdown.h`, the new walker mode in `src/shippable.c`, the fan-out kind in `src/fanout.c`, the planner changes in
`src/planner.c`, `src/catalog.c`, the README, the tests `t/120` to `t/123` and the regression tests
`pushdown_shippable` and `pushdown_plans`), and the design notes (sections 5, 6, 9.1, 11.7, "Step 5" and "What step 6 will
find changed").  Findings P2C-01 to P2C-12 of the previous review are open and are not repeated.  One of my own probes
met P2C-01 again (a `timestamp(3)` key with sub-precision input, router path); it is named where it occurred and not counted.

Method: everything below was run on private copies of the two assertion builds (PostgreSQL 18.7 and 19rc2, both with
`USE_ASSERT_CHECKING`), into which the tree under review was built with `-Werror` (clean on both majors) and installed;
the default version of the extension is 0.6 on both copies.  Clusters were three nodes plus, for comparisons, one
ordinary single node, always one cluster at a time (see the process note at the end of this section).  Ports 58300 to 58399
only, temporary files only in the reviewer's own area.  The TAP harness of the private installs was changed to
draw its ports from that range (the harness file of the private copy, not the tree).

* A generator of my own (`gen`, not `t/123`): seeded random statements over families of three colocated tables
  (source, other, target) per key type (`int`, `bigint`, `text`, `varchar(10)`, `char(5)`, `timestamp(0)`,
  `numeric(8,2)`, `date`, a domain over `int`), a reference table, a local table, volatile, stable and immutable user functions, about
  135 statement shapes (joins of every kind on and off the key, aggregates, DISTINCT and DISTINCT ON, windows, set operations,
  sub-selects, CTEs, LIMIT/OFFSET/FETCH, FOR UPDATE, TABLESAMPLE, every `ON CONFLICT` form, `RETURNING`, self inserts,
  casts, constants of `reg*` types, time and random functions, system columns) combined with random `WHERE` clauses, random
  target column lists, random session profiles (`TimeZone`, `DateStyle`, `IntervalStyle`, `search_path`, `extra_float_digits`,
  `bytea_output`), and run as plain statements, prepared statements, generic plans, `DO` blocks and transactions with a
  savepoint.  Before each statement the target (and a self-insert target) is reset on the cluster and the twin; after it the
  harness compares the outcome, the error (SQLSTATE and text with partition names normalised), the printed
  `RETURNING` rows and the whole target table (read directly on every group with routing off, against the twin).  Each
  statement is also planned with `EXPLAIN` to learn whether it was shipped ("Pushed:") or routed.
* A second runner (`spec`) with 64 fixed cases per major: hostile text keys and literals, collations, NULL keys, two
  schemas with same-named tables and search_path changes, quoted identifiers, enums, `timestamp(3)` into `timestamp(0)`, key
  joins that are almost colocated (offset, inequality, OR, transitive through a reference table), outer joins with extra
  conditions, HAVING, DISTINCT ON, OVERRIDING SYSTEM/USER VALUE, literals in the select list with permuted column lists,
  `ON CONFLICT` forms.
* Hand-written probes (shell and SQL) on a persistent cluster for questions 2 to 8, run on 18.7 and, where stated, on 19rc2.
* Thirty one-line mutants of the new code, each built (with `-Werror`; eight needed `-Werror` off because the mutation
  leaves an unused variable or function) and run against the regression tests and `t/120` to `t/123`.
* The complete suite (pg_regress and every TAP file, one at a time) once per major.

Process note, in the interest of accuracy: (a) the first start of the full suite on 18 was stopped after two minutes by me, to
do the probes first; (b) once, for about two minutes, a second three-node cluster was up while a generator run was going
(an error of mine in ordering commands; no port outside the range, no failure came from it); (c) three of my probe scripts
had defects of their own (a shell function that gave the forwarding settings to the wrong command, so that "forged
fragment" probes first ran as ordinary sessions; multi-statement strings that the product refuses; a context-id column
that 18 does not have); they were fixed and re-run, and only the corrected runs are reported.

## 2. What was run

| Run | PostgreSQL 18.7 | PostgreSQL 19rc2 |
| --- | --- | --- |
| Build with `-Werror`, `clean` first | clean | clean |
| `grep PG_VERSION_NUM / PG_MAJORVERSION` outside `src/pg_compat.h` | no hit | (same source) |
| Generator, final version, 3 seeds x 600 statements | seeds 31, 32, 33: 1800 statements; 399 shipped, 1225 routed, 287 refused by the router (`0A000`, mostly `ON CONFLICT DO UPDATE` that the router does not support), 1420 equal to the twin, 53 equal-after-refusal, 40 differences, **0 differences among the shipped** | seeds 41, 42, 43: 1800 statements; 400 shipped, 1238 routed, 316 refused, 1406 equal, 37 equal-after-refusal, 41 differences, **0 differences among the shipped** |
| Generator, earlier versions of the same program | seeds 1, 11, 12, 13, 14: 2300 statements; one shipped difference (seed 1, `FROM ONLY`, P2D-03) | seeds 21, 22: 1200 statements; none |
| Fixed cases (`spec`) | 64 cases, 27 shipped, 0 shipped differences, 5 other differences (see 3) | the same 64, the same result |
| `INSERT ... SELECT` through forged fragments, roles, column privileges, `SET ROLE`, `SET SESSION AUTHORIZATION` | run | run (security part and forged fragments) |
| Faults, kills, timeouts, savepoints, 2PC recovery | run | - |
| Prepared plans across catalog changes made by hand on every node | run | - |
| Bucket moves before, between and after the groups | run | - |
| Memory, 2000 executions x 6 variants | run | - |
| DDL churn on distributed tables while planning (75 s, two origins) | run | - |
| `debug_discard_caches = 1` | run | - |
| Mutants (30) | run | - |
| Regression tests (pg_regress) | 16 of 16 passed | 16 of 16 passed |
| TAP files | 123 files, 20167 tests, all passed (`t/046_two_majors` ran with the 19rc2 copy; 4135 s) | files 001 to 109: 12694 tests passed; `t/110` stopped twice with "pg_ctl start failed" because the TCP port drawn for a node was taken by another run on the machine ("could not bind IPv4 address ... Address already in use" in the node's log; 359 subtests had passed in the first try); `t/110` to `t/123` run again alone: 14 files, 8113 tests, all passed (the re-run counts 110 once) |

In total about 7100 generated statements and 128 fixed cases were compared with a single node.

The differences counted above were all inspected by their statement text.  Every one of the 81 differences of the final seeds is a
statement that was not shipped; the ones I traced to their cause (all of seed 31 and a sample of every other seed) are:
`ON CONFLICT DO NOTHING` or `OFFSET` without `ORDER BY` over duplicates, where "which row wins" is not defined; `random()`
and `now()` values; the text of an object identifier (`'f'::regproc::oid::text`), which is a different number on a different
cluster; `current_schema()`, which is `kingres` on the cluster nodes in this environment and `public` on the twin; and a
state drift of my own harness in the earlier versions.  None is a wrong answer of the product.

## 3. Answers to the questions

1. **Right rows.**  No shipped statement gave a different result from the single node, in 3600 final statements and 128
   fixed cases over both majors, with the profiles named above, with prepared and generic plans, in `DO` blocks and in
   transactions with `ROLLBACK TO`.  The one shipped difference of the early versions is the `ONLY` case (P2D-03).  What is
   shipped matches what the design says (joins on the key, left and right joins with the key from the preserved side,
   reference tables, aggregates and DISTINCT by the key, every `ON CONFLICT` form, `RETURNING`, a self insert, hostile text
   keys and constants, quoted names, same-named tables in two schemas, a `search_path` change, `OVERRIDING SYSTEM VALUE`).
   What must not be shipped was not: a join on a key plus an offset, an inequality, an `OR`, a join made transitive through
   a reference table, a local table, `LIMIT`, windows, set operations, sub-selects, CTEs, `FOR UPDATE`, `TABLESAMPLE`,
   volatile and stable functions, `now()`, `current_user`, `current_setting`, a key that is cast or computed, a key from the
   nullable side, a literal in the select list (every case of it, in every column order, is routed).  Statements that the
   design says may be shipped but were not: any conversion of a non-text value to text (`g::text`, `n::text`,
   `length(v)::text`, `sum(n)::text`) is refused as "it converts a value through its text form, which depends on session
   settings", also for integers (P2D-06); `concat()` and `to_char()` are refused as not built in and immutable; `string_agg`
   is refused; a `FULL JOIN` is never shipped (no key of it is non-nullable, P2D-06).  Key types with modifiers
   (`timestamp(0)`, `numeric(8,2)`, `varchar(10)`, `char(5)`), a domain key, NULL keys (no unique index), and collations
   (a `"C"` key colocated with a `"C"` table; a table with the default collation cannot be colocated with it, the product
   refuses at `create_distributed_table`) gave equal results.  My enum and `timestamp(3)` fixed cases were inconclusive
   because of my own setup (the `timestamp(3)` one meets P2C-01; a manual enum copy was right).
2. **The decision "shippable".**  I could not make the origin ship something a single node answers differently by hiding a
   function: a view is a sub-select and is not shipped; a user function (inlined or not), a user operator, a
   `SECURITY DEFINER` function, a sequence or `random()` in a default, `now()`, `current_user`, `session_user`,
   `current_setting`, `current_schema`, `concat` are all routed (the volatile and stable defaults are computed once per row
   at the origin: 30 distinct identity values, one `now()` value, 30 distinct `random()` values).  `txid_current()`,
   `inet_client_addr()`, `currval` and an index expression of the conflict target were not run; by reading they are stable or volatile
   and the walker accepts only built-in immutable functions (read).  Privileges are checked by the group as the calling role:
   a role with `SELECT (k)` on the source and `INSERT (k, v)` on the target may run `INSERT (k) SELECT k` and is refused
   (`permission denied for table s1`) when the select list or `WHERE` names `v`, when the target list names a column it may not
   write, and for a join with a table it may not read; the same under `SET ROLE` and `SET SESSION AUTHORIZATION`; a view of
   another owner and row-level security on the source are refused by the router.  No privilege was missed.  (The group's error
   text carries the fragment, with its internal `place=` number, to an ordinary user; harmless, noted.)
3. **The fragment on the receiving group.**  A session that is not forwarded cannot run it: `SET kingres_shard.forwarded` and
   `forward_token` cannot be set after connection start, and a marker typed by an ordinary user, a superuser, inside `DO`/`EXECUTE`
   is only a comment.  In a forwarded session (token from `internal_forward_token`): a stale, garbage, huge (`99999999999999999999`)
   or negative `place` is `KS001` (stale map version); a missing or unparsable `place`, an UPDATE, a DELETE, a SELECT, a VALUES
   insert, a set operation, a sub-select, a key computed (`k + 1`), a `WITH`, behind the marker is `0A000` "does not accept the fragment";
   the marker hidden behind another comment or after another statement is not recognised and the statement is judged as an
   ordinary forwarded statement ("cannot prove that the statement touches only buckets of this node"); a second statement
   after the marker (`INSERT ...; DELETE ...`) fails and the whole string is rolled back (checked on 19: 0 rows left).  The acceptance
   rule is the function the origin runs, on the parse of the received text (read, and the mutant M06 shows the group-side call is
   tested).  One weakness: `place=0` skips the comparison (P2D-05).  `dblink` from a superuser with forged settings is refused at connection, and without them the marker is a comment (P2D-07).
4. **Placement changes.**  A bucket move started while the statement waits before the first group, between groups and before
   the final recheck: the move waits for the statement (its switch cannot get the lock), the statement ends with all 60 rows once,
   in the groups that owned them when it started, and the move then completes (3 of 3 runs; rows 60, nothing prepared).  A generic
   plan prepared while a group owned no bucket, then the bucket moved back to that group, then executed: the plan is replanned
   (the move invalidates it) and the statement inserts 60 rows, none lost, none doubled.  Catalog changes made by hand on every
   node (ALTER of a distributed table is refused by the product, so the changes were made with routing off on each node) between
   `PREPARE` and `EXECUTE` of a generic plan: an added column with a default, a volatile default, a stable default, a trigger
   added and dropped, row-level security with a check on the target, and row-level security on the source for a role: the cached
   plan noticed every one (rows changed from 10 to 3 for the role under the policy; the trigger text appeared and disappeared; the volatile default gave 10
   distinct values).  The two `KS001` branches that the design says are not reached by the tests could not be reached
   either: the move cannot overtake a statement that holds the tables, and a prepared plan across a move is replanned.  Mutants
   M07, M09 and M10 (the three placement/relation checks) survive for that reason (section 5).
5. **Transactions and faults.**  A unique violation on the last group (after the others had inserted) leaves nothing on any
   group, nothing prepared, and the session usable; the same for a `CHECK` violation and a `statement_timeout` of 1.5 s while a group was
   blocked by a lock.  A SIGKILL of the origin backend while the statement waited between two groups left no row and no prepared
   transaction on the surviving nodes.  A kill of the origin after the first PREPARE left one prepared transaction on one group;
   `recover_prepared_transactions()` rolled it back (0 rows).  A kill after the origin's local commit left prepared
   transactions on two groups; the recovery committed them (120 of 120 rows).  A transaction with two savepoints and a `ROLLBACK TO`
   gave 30, 60 and 60 rows as a single node would, and a failing second statement rolled the transaction back to the last good
   state.  A stopped node was not run (the step's own `t/122` does it; see the suite totals).
6. **The `reg*` rule.**  Scalar constants: built-in objects ship (`'pg_class'::regclass`, `'int4'::regtype`, `'public'::regnamespace`);
   user objects (`'public.s1'::regclass`, `'rcheck'::regrole`, `to_regclass(...)`) are evaluated at the origin and the statement is
   routed, also in `DO UPDATE SET` and in scan restrictions, where the origin evaluates them after the groups return rows.  The rule has
   gaps: a `regclass` **parameter** and a `regclass[]` constant compared as such are shipped as names that each group resolves to its
   own OID (P2D-01).  On 19, `regdatabase` is a new type that the list does not know (run: P2D-01).
7. **Reload of the catalog cache during planning.**  The fourth pass is an error (`the catalog of the cluster changed again and
   again while the statement was planned`, with the hint "Repeat the statement"), not a fallback and not a dangling pointer;
   I found no stale pointer by reading the code.  Under real DDL churn (828 create/distribute/drop cycles in 75 s against two
   origins that planned 200 statements per round, shipped and routed) there was no error, no assertion failure and no crash.  With
   `debug_discard_caches = 1` every routed `INSERT ... SELECT` fails deterministically with that error, while a shipped one works
   (P2D-04).
8. **Memory.**  2000 executions of a shipped statement in one session: the sum of all memory contexts was 3.03 MB after
   500 statements and 3.03 MB after 2000 (ad hoc), 3.07 MB and 3.07 MB (prepared, generic); `TopMemoryContext` itself stays at
   107624 bytes and `CacheMemoryContext` at 1.36 MB.  2000 plain `EXPLAIN` (shipped and routed): flat after the first.  2000
   failing statements: flat.  No growth.
9. **Both majors.**  No `PG_VERSION_NUM` or `PG_MAJORVERSION` outside `src/pg_compat.h`; `-Werror` builds are clean on both; the full
   suite totals are in the table of section 2.  `ON CONFLICT DO SELECT` on 19 is refused with "this ON CONFLICT action is not
   supported for an INSERT that goes to several shard groups" (the README says "not shipped"; the statement is not run at all,
   P2D-06).
10. **Test quality.**  Section 5.
11. **Documentation.**  The README says what is shipped, the setting, what EXPLAIN shows and the fallback, and matches the behaviour
    except for the points of P2D-06.
12. **`pushdown.h` as a base.**  Section 6.

## 4. Findings

Count: 0 critical, 0 high, 2 medium, 4 low, 2 notes.

| Id | Severity | Title |
| --- | --- | --- |
| P2D-01 | medium | A `reg*` parameter, an array constant of a `reg*` type, and on 19 a `regdatabase` constant are shipped as names that each group resolves to its own object; the answer differs from the router and from the scan |
| P2D-02 | medium (test quality) | The defences that the design lists for a move or a missed invalidation, and several analysis rules, survive their mutants; two rules cannot be tested through an INSERT |
| P2D-03 | low | `FROM ONLY` a distributed table: the analysis does not look at `inh`; three paths give three answers |
| P2D-04 | low | With `debug_discard_caches = 1` every routed `INSERT ... SELECT` fails after three replans |
| P2D-05 | low | `place=0` switches the placement comparison off on the group |
| P2D-06 | low (documentation) | The README promises or implies what the code never does, and leaves out three refusals a user meets |
| P2D-07 | note | `dblink` with the forwarding settings and a view as the target (see the result of the probes) |
| P2D-08 | note | Where `pushdown.h` is right only because the caller is an INSERT |

### P2D-01 (medium) A `reg*` parameter and an array constant of a `reg*` type are shipped as names that each group resolves to its own object

Place: `src/shippable.c:93-111` (`is_reg_type` names the scalar types only), `src/shippable.c:524-526` (the rule tests a `Const` of
a listed scalar type), `src/shippable.c:530` (`T_Param` is accepted whatever its type).

Steps (18.7, three nodes; `public.s1(k int primary key, v text, n int, g int)` and `public.d1` colocated, 30 rows; the OID of `public.s1`
is 16722 on node 1 and 16714 on nodes 2 and 3; the row `k = 1` has `g = 16722`, a stored OID):

```
-- a plain constant: evaluated at the origin, routed
INSERT INTO public.d1 (k, v) SELECT k, v FROM public.s1 WHERE g::oid::regclass = 'public.s1'::regclass;   -- INSERT 0 1
-- a parameter of type regclass
PREPARE q(regclass) AS INSERT INTO public.d1 (k, v) SELECT k, v FROM public.s1 WHERE g::oid::regclass = $1;
EXPLAIN (COSTS OFF) EXECUTE q('public.s1');
   Custom Scan (Kingres Fan-out Write)   Pushed: colocated INSERT ... SELECT ...
   Remote SQL: ... WHERE ((((s1.g)::oid)::regclass)::oid = ($1)::oid)
EXECUTE q('public.s1');                                             -- INSERT 0 0   (shipped)
SET kingres_shard.enable_colocated_insert_select = off;
PREPARE q2(regclass) AS ... (the same statement);  EXECUTE q2('public.s1');   -- INSERT 0 1   (row router)
PREPARE q3(regclass) AS SELECT count(*) FROM public.s1 WHERE g::oid::regclass = $1;  EXECUTE q3('public.s1');  -- 1  (scan)
-- an array constant
INSERT INTO public.d1 (k, v) SELECT k, v FROM public.s1 WHERE ARRAY[g::oid::regclass] = '{public.s1}'::regclass[];
   -- shipped, Remote SQL has '{public.s1}'::regclass[]; INSERT 0 0   (the constant version above gives 1)
```

Why it matters: rule 6 of 5.1 exists so that the meaning of a reference to a catalog object is the same wherever the
statement is evaluated.  The rule is implemented for a scalar `Const` only.  A parameter of a `reg*` type is sent in its text form and the groups
resolve the name to their own OID; so is an array constant, whose element type is not in the list.  The same statement then gives a different
answer shipped (0), routed (1) and by the scan (1).  The effect needs a column that holds raw OIDs (for a column of a `reg*` type, names are the
consistent meaning and the shipped answer is right, the origin-evaluated one is the odd one); it is a difference of path, which is the criterion
of this review.  On 19 `regdatabase` is a new `reg*` type that `is_reg_type` does not know (run: with a database `rdb1` that exists only on node 1, `INSERT ... WHERE n::oid::regdatabase = 'rdb1'::regdatabase` is shipped (Remote SQL has `('rdb1'::regdatabase)::oid`) and fails with `ERROR: database "rdb1" does not exist` from the groups, while the same statement with the switch off returns `INSERT 0 0` and the scan evaluates the constant at the origin (`'16873'::oid`)).

A fix must achieve: one meaning for a value of a `reg*` type (name, or OID of the origin) that is the same on every path (shipped, routed,
scanned), for scalars, arrays, parameters and fields of rows, and for every `reg*` type of the major in use (the list must come from the
catalog of types of the server, not a hand-written list), with a test for a parameter and for an array on each path.

### P2D-02 (medium, test quality) The defences against a move and a missed invalidation, and several analysis rules, survive their mutants

Place: `src/fanout.c:1041`, `src/fanout.c:1148`, `src/fanout.c:602`, `src/planner.c:889`, `src/pushdown.c:319,391,428,553,579,588,777,907,914,940,978`,
`src/shippable.c:497-500`.

Steps: section 5.  Of 30 one-line mutants, 16 survive `t/120` to `t/123` and the two regression tests.  Four are defences that the design note itself says no test reaches (the plan-start placement check M07, the recheck after the last group M09, the relation signature at plan start M10, and the origin's parse of its own text M27).  Four are rules that an INSERT cannot violate because its key must be a bare key variable (`closed()`: aggregate without GROUP BY M24, DISTINCT M21, window partition M20; and the FULL JOIN key rule M18).  One is dead code in this mode (the whole-row exception for `EXCLUDED`, M16).  Three are redundant second layers (sub-link M02, variables of the SET list M28, view of another owner on a source M29).  Three are checks for which I could build no distinguishing statement or that no test exercises (M15, M25, M26).  The last, M30 (the target written with the rights of another role), has a security meaning but is redundant in effect: the router refuses the same statement (P2D-07).

Why it matters: the step's own note says "closed follows from the rule that the key is bare, so only the reason line tests it" and that step 6 needs
tests of its own; the survivors show how many rules of `pushdown.c` are untested when step 6 lifts the bare-key restriction.  The three
placement/relation defences are the ones that protect against lost or doubled rows after a move, and no test can show that they work.

A fix must achieve: each rule of `pushdown.c` and each defence of `fanout.c` is killed by a test that can reach it (through a test
hook that lets a move or an invalidation overtake the statement, and through statements of a kind that is not an INSERT), or is
removed with the reason written where it was.

### P2D-03 (low) `FROM ONLY` a distributed table: the analysis does not look at `inh`; three paths give three answers

Place: `src/pushdown.c:417-420` (`rte->tablesample` and `relkind` are tested; `rte->inh` is not).

Steps (18.7; `s1` has 30 rows over three groups, `d1` empty):

```
SELECT count(*) FROM public.s1;                          -- 30
SELECT count(*) FROM ONLY public.s1;                     -- 19   (the rows of the other groups; the local parent is empty)
INSERT INTO public.d1 (k, v) SELECT k, v FROM ONLY public.s1;        -- shipped: Remote SQL has "FROM ONLY public.s1"; 0 rows
SET kingres_shard.enable_colocated_insert_select = off;  INSERT ... FROM ONLY public.s1;   -- 0 rows (router)
-- on a single node (a table that is not partitioned) the same INSERT copies all rows
```

(The generator met it first as a shipped statement that differed from the twin; seed 1, `d_bigint`.)  The shipped and the routed answers agree (0,
which is what `ONLY` on a partitioned parent means in PostgreSQL); the distributed scan, which is older, returns the rows of the remote groups
only.  The analysis of a later read pushdown would inherit the shipped meaning and the scan would keep its own.

A fix must achieve: `ONLY` on a distributed table has one defined meaning on every path (an error is a valid one), and
`kingres_shard_pushdown_analyze()` refuses or handles `rte->inh == false` explicitly.

### P2D-04 (low) With `debug_discard_caches = 1` every routed `INSERT ... SELECT` fails after three replans

Place: `src/planner.c:2085-2115` (the generation compare and the `replans >= 3` branch).

Steps (18.7 assertion build, any distributed `s1`, `d1` colocated):

```
SET debug_discard_caches = 1;
INSERT INTO public.d1 (k, v) SELECT s.k, s.v FROM public.s1 s JOIN public.o1 o ON o.k = s.k ON CONFLICT DO NOTHING;   -- shipped: INSERT 0 0
INSERT INTO public.d1 (k, v) SELECT s.k + 0, s.v FROM public.s1 s JOIN public.o1 o ON o.k = s.k ON CONFLICT DO NOTHING;
ERROR:  the catalog of the cluster changed again and again while the statement was planned
HINT:  Repeat the statement.
```

With `client_min_messages = debug1` the line "INSERT ... SELECT is not shipped whole: ..." appears four times: the whole planner hook, including the analysis, is
run four times.  The fourth pass is an error, as designed; there is no dangling pointer.  The consequence is that a build that discards caches at every
opportunity (`CLOBBER_CACHE_ALWAYS` style testing, `debug_discard_caches`) can never plan a routed `INSERT ... SELECT` into a distributed table, while a
shipped one works, so such a run cannot cover the router.  Under real DDL churn (828 create/distribute/drop cycles in 75 s, two origins planning in a loop)
I saw no error.

A fix must achieve: the decision "replan" depends on a change that matters to the pointers the code holds (not on every reload), or the bound
is documented with the setting that defeats it and the test suites that use that setting know it.

### P2D-05 (low) `place=0` switches the placement comparison off on the group

Place: `src/planner.c:1131-1132` (`if (place != 0 && kingres_shard_placement_hash_now(...) != place)`), `src/fanout.c:489` (`place != 0 ? place : ...`).

Steps (forwarded session on a group; `internal_forward_token` of the group's own node):

```
/* kingres:insert-select-fragment place=0 */ INSERT INTO public.d1 (k, v) SELECT k, v FROM public.s1     -- runs (a duplicate key error in my run: it reached the executor)
/* kingres:insert-select-fragment place=1 */ INSERT ...                                                  -- ERROR: the bucket map of this node is not the one the statement was shipped with
```

Why it matters: zero is used as "no number" and also may be the value of a placement hash; a fragment of zero is accepted without the
comparison that is the second line of defence.  Only a holder of the cluster secret can send it, and the origin never sends zero unless the hash is
zero (probability 2^-64 per placement), so the effect is small.  A huge `place` saturates (`strtoll` without `errno`) and is a mismatch, which is right.

A fix must achieve: a fragment always carries a number that is compared, and the absence of one is a refusal, not a bypass.

### P2D-06 (low, documentation) What the README implies or leaves out

Place: README section "Colocated `INSERT ... SELECT`", `src/shippable.c` (text conversion), `src/planner.c:1125`.

* "a `FULL JOIN` is shipped only between two distributed sides": a `FULL JOIN` is never shipped for an INSERT (both sides are nullable,
  so no key of it is a legal row key); in 5 shapes x about 25 runs per major, none was shipped.  The sentence reads as if some are.
* "every expression is built in and immutable": a conversion of any non-text value to text (`g::text`, `n::text`, `length(v)::text`, `sum(n)::text`)
  is refused with "it converts a value through its text form, which depends on session settings", also for integers, whose text form depends on no setting.
  `concat()`, `to_char()` and `string_agg()` are refused too.  The README does not say that casts to text are routed.
* "`DO SELECT` (PostgreSQL 19) is not shipped": the statement is not run at all; the router refuses it with "this ON CONFLICT action is not
  supported for an INSERT that goes to several shard groups" (19rc2, checked), so the user cannot run it on the cluster.
* "the groups run it" is true but the groups run one after another (design note), so a shipped statement over N groups takes the sum of the
  groups' times, not the longest; the README says "sent to every group".

A fix must achieve: the README states each of these as a limit.

### P2D-07 (note) Forwarding settings through `dblink`, and the target through a view

Run on 18.7 (and the dblink part also by reading the receiving side): `internal_forward_token` is not executable by an ordinary role (`permission denied for
function internal_forward_token`).  `dblink` from a superuser with `options='-c kingres_shard.forwarded=on -c kingres_shard.forward_token=00ff'`, and with
`forwarded=on` and no token, is refused at connection ("FATAL: this session claims to have been opened by another node of the cluster but cannot prove it").
`dblink` without the settings and a text that starts with the marker is a plain statement (the marker is a comment; `SELECT count(*)` returned 30, a distributed scan).
`postgres_fdw` was not exercised beyond its being the same kind of ordinary client.  An insert through a view of another owner is refused with
"reads "d1" through a view owned by role "vo"" (also with the mutant M30, so the router's own refusal covers it).  No defect.


### P2D-08 (note) Where `pushdown.h` is right only because the caller is an INSERT

See section 6.


## 5. Mutants

Each mutant is a one-line change of the new code, built into the 18.7 copy (clean build; `-Werror` on, off for the eight marked "w" whose change leaves an unused
variable or function), then run against the regression tests (`pushdown_shippable`, `pushdown_plans` and the rest of the 16) and the TAP files `t/120`,
`t/121`, `t/122`, `t/123` in that order, stopping at the first that fails.  The original code was rebuilt and reinstalled afterwards.

| Mutant | Place (tree) | Change | Result | Verdict for a survivor |
| --- | --- | --- | --- | --- |
| M01 | pushdown.c:422 | drop the row-level security test of a source | killed by t/121 | |
| M02 | pushdown.c:391 | drop the sub-link rule of the analysis | survives | redundant: the walker refuses `T_SubLink` and every sub-select that is not the statement's own source |
| M03 | pushdown.c:181 | drop the operator-family test of the join clause (`op_in_opfamily`) | killed by t/123 | |
| M04 | pushdown.c:816 | drop the call of `closed()` | killed by regress `pushdown_plans` only | no TAP test kills it |
| M05 | pushdown.c:487 | drop the key-class count (`nroots != 1`) | killed by regress `pushdown_plans` only | no TAP test kills it |
| M06 | planner.c:1125 | the group accepts every marker statement without the analysis | killed by t/121 | |
| M07 | fanout.c:1041 | drop the plan-start placement check (`placement_check_plan`) | survives | redundant while the group compares `place=`; a test needs a hook that lets a move overtake a plan |
| M08 | planner.c:1132 | drop the group's comparison of `place=` | killed by t/121 | |
| M09 w | fanout.c:1148 | drop the placement recheck after the last group | survives | test missing: a move cannot overtake the statement (it waits for the locks), which is why the design note lists the branch |
| M10 w | fanout.c:602 | drop the relation signature check at plan start | survives | test missing: no test misses an invalidation |
| M11 w | pushdown.c:960 | drop the unique-index check of the target | killed by t/120 | |
| M12 | shippable.c:525 | drop the `reg*` rule (OID >= FirstGenbkiObjectId) | killed by regress `pushdown_shippable` | |
| M13 | pushdown.c:853 | drop the volatile-default rule | killed by regress `pushdown_plans` | |
| M14 | pushdown.c:855 | drop the shippable-default rule | killed by t/121 | |
| M15 | pushdown.c:907 | drop the "untyped literal" condition for a variable-free column | survives | redundant in practice: the parser leaves a variable-free named column only for an untyped literal; I found no statement that separates them |
| M16 | shippable.c:497-500 | drop the whole-row exception for `EXCLUDED` | survives | dead code in this mode: the new `OnConflictExpr` case does not walk the list that holds the whole-row entry |
| M17 | pushdown.c:514 | drop the "nullable side" test of a key | killed by regress `pushdown_plans` | |
| M18 | pushdown.c:319 | drop the FULL JOIN "no key equality" rule | survives | not reachable through an INSERT (both keys of a FULL JOIN are nullable); needs a test of a read |
| M19 | pushdown.c:330 | drop the "preserved side is replicated" rule | killed by t/123 | |
| M20 w | pushdown.c:588 | drop the window partition test | survives | redundant for an INSERT (the walker refuses window functions); needs a test of a read |
| M21 | pushdown.c:579 | drop the DISTINCT test | survives | not reachable through an INSERT (the bare key is in the output, so DISTINCT covers it) |
| M22 w | pushdown.c:958 | drop the trigger/rule test of the target | killed by t/121 | |
| M23 w | pushdown.c:975 | drop "DO UPDATE assigns the distribution column" | killed by regress `pushdown_plans` | |
| M24 | pushdown.c:553 | drop the "aggregate without GROUP BY" rule | survives | not reachable through an INSERT (no key in the output) |
| M25 | pushdown.c:940 | drop the "key is a Var" test (the cast to `Var` that follows then reads a non-Var) | survives | test missing and undefined behaviour in the mutant; the next test refuses by luck |
| M26 w | pushdown.c:914 | drop "columns named in another order next to a literal" | survives | redundant in effect: with the mutant `INSERT INTO d (v, k) SELECT 'lit', s.k FROM s` is still routed (a later position check refuses it) and gives the same rows as the single node; the rule is reached (DEBUG1 names it) but has no test of its own |
| M27 w | planner.c:889 | drop the origin's parse of its own text | survives | test missing: no statement makes deparse and parse change the decision |
| M28 | pushdown.c:978 | drop the "variables of the SET list" test | survives | redundant: the parser admits only the target and `EXCLUDED` there |
| M29 | pushdown.c:428 | drop the view-of-another-owner test of a source | survives | redundant: a view is a sub-select and is refused earlier |
| M30 | pushdown.c:777 | drop the rights-of-another-role test of the target | survives | redundant in effect: with the mutant an insert through a view of another owner is still refused by the router, with its own message (P2D-07) |

14 killed (six of them, M04, M05, M12, M13, M17 and M23, by the regression tests and by no TAP file), 16 survive.


## 6. `pushdown.h` as a base for the read pushdown

Read only (no read pushdown exists yet), against `src/pushdown.c` and `src/pushdown.h`.  The three functions are independent of the INSERT in their
signatures and in most of their code: the join-tree walk (inner, left, right, full, union-find over key variables, the nullable marks), the key test, and
the rules on aggregation.  The places where the analysis is right only because the caller is an INSERT are these.

1. `kingres_shard_pushdown_closed()` tests `q->hasAggs && q->groupClause == NIL` for "aggregate without GROUP BY".  A query with `HAVING` and neither
   an aggregate nor `GROUP BY` (`SELECT 1 FROM s HAVING true`) is an implicit one-group aggregation, `hasAggs` is false, and the function says "closed".
   Run on every group and added up it gives one row per group.  An INSERT cannot reach this: any column it names must then be a bare key, which the
   parser refuses outside a group.  A SELECT can.  Same family: `LIMIT`, `OFFSET`, `FETCH ... WITH TIES`, `ORDER BY` with a limit, `TABLESAMPLE` (tested), `FOR UPDATE` (tested)
   are not looked at by `analyze()` or `closed()`; the INSERT's walker rejects `LIMIT` and the like, so the module relies on its caller for them; the header
   says so for "rule 1" but not for these.
2. `kingres_shard_pushdown_closed()` accepts a grouping that contains a key of the class and a DISTINCT that covers one; for a SELECT whose output does
   not include the key, a `DISTINCT` of the key plus other columns is closed (each group is separate) which is right, but `DISTINCT ON` is accepted by the same test
   (`distinctClause` holds the `DISTINCT ON` list): `DISTINCT ON (k) ... ORDER BY k, x` is closed, `DISTINCT ON (n, k)` is closed, but the sort and the
   `ORDER BY` that gives the "first" row of each set then has to be shipped by the caller; the INSERT never ships it (README: no `DISTINCT ON`).
3. `kingres_shard_pushdown_is_key_var()` and `_key_rti()` resolve an expression against `pd->query`.  An INSERT hands them an expression of the source
   level; a caller that hands one of an outer level (a column of a sub-select it wraps) gets a wrong or a zero answer without an error.  The header says "of the output
   of the analysed level".
4. `kingres_shard_pushdown_analyze()` requires a distributed table (`ndist > 0`) and refuses a level that reads only reference tables, which is right for
   an INSERT (the target decides where the rows go) and wrong for a read that wants to run a reference-only sub-level on one node.
5. `rte->inh` is not read (P2D-03): `FROM ONLY` a distributed table is analysed like the table.
6. The outer-join rules (`J2`, `J5`, the key-from-the-nullable-side rule) are right in my reading (the preserved side must keep a distributed table, a FULL JOIN
   needs both sides distributed and a key equality, an equality in an outer `ON` joins only the two arms), but through an INSERT only the left/right
   join with a non-null key is reachable, so the FULL JOIN rule M18 and the replicated-preserved-side rule are tested only by the generator (M19 is killed, M18 not).
   Step 6 needs its own tests of every rule that the bare-key condition hides (M18, M20, M21, M24).
7. `KsPdRel` and `KsPushdown` copy what they need out of the catalog cache (good: the cache can reload), and `kingres_shard_pushdown_relids()` returns OIDs
   only, so a caller that keeps a `KsPushdown` across a call that locks (parse analysis) is safe.  `KsPushdown.query` points into the caller's tree; the INSERT code
   documents that.

The module is a sound base for the join and key logic; before step 6 relies on `closed()` the HAVING case, `ONLY`, and the limit/ordering family must be
decided inside the module or written into the header as the caller's duty, and the rules named in 6 need tests that an INSERT cannot give.


## 7. What I did not examine

* A group that is stopped (SIGSTOP or a stopped node) while it runs the fragment, a failover between the plan and the run, and a group with a
  standby ("reported as KS001 when it has a standby"): only the step's own `t/122` covers them; I did not run my own.
* Two-phase commit with a node stopped between prepare and commit: I killed the origin at three points (before the first PREPARE completed, after the first
  PREPARE, after the local commit) and recovered; I did not stop a participant.
* `txid_current()`, `inet_client_addr()`, `currval`, a volatile or stable function in an index expression of the conflict target, a volatile domain constraint on the
  target (a domain must exist on every node, and my attempt to create one failed on the remote side), a user function as a default (same), a security barrier view as
  a source (a view is refused as a sub-select; judged by reading), `SECURITY DEFINER` as a source of rights (a user function is not shippable: routed).
* Sources that are partitioned or inherited, and a target that is a partition: the product has no such table.
* `dblink` and `postgres_fdw` as origins of a forged fragment: see P2D-07 for what was run.
* The 19-only behaviour of the other new reg types beyond `regdatabase`; arrays of reg types in a `RETURNING` or `DO UPDATE` list.
* Performance: the shipped statement against the router on large tables, the sum-of-groups time of the sequential run.
* The `place=` interplay with a `CREATE` of a new group between plan and run beyond what `t/122` and my generic-plan probes show.
* The behaviour of the walker for a collation that is neither the default nor `C` on a source column (the product refuses to colocate a different collation,
  so the case could not be built with a colocated source).
* `kingres_shard.enable_routing = off` sessions (a superuser's own risk).
* The upgrade path (`kingres_shard--0.5--0.6.sql`) beyond what the regression test `upgrade` shows.
