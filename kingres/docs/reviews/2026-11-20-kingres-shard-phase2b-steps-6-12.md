<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->

# Review of kingres_shard phase 2b, steps 6 to 12: the read pushdown

## Scope and method

Reviewed: the read pushdown of phase 2b as it stands at commit 268a81d93f, all of it on by default: the closed query and the
fragment scan (step 6), the partial aggregation (7), the objects of extensions (8), the ordered merge and the pushed LIMIT,
including a LIMIT computed when the scan starts and a window or DISTINCT above a partial aggregation (9), the join rows (10), and
the README and the security round (11, 12). Code read: `pushdown.c` (the shape rules, the fragment analyses, the markers),
the planning half of `planner.c` (`pushdown_prepare`, `closed_sort_limit`, `limit_evaluable`, `kingres_shard_text_accepted`),
`fragment_scan.c` (the paths of the closed query, the partial aggregation and the join, `fs_start`, the ordered merge, rescan),
`shippable.c` (the type, collation, extension and function rules), the GUC defaults in `kingres_shard.c`, and the README sections of
steps 6 to 12 with the new Guarantees and Limitations bullets. Design notes read: the writer's notes of rounds 2b9 and 2b10 and the
reviews of steps 3 and 4 (the review of step 5 is not in this tree; I could not compare against it).

Method: I built the extension out of tree, with `-Werror` after `make clean`, into private copies of the PostgreSQL 18.7 assertion
install and of the 19rc2 install (the TAP harness of the private copies draws ports from 58400 to 58499 only; temporary files stay in a
private work area; nothing shared was touched). I ran the shipped tests of the steps on both majors. I then wrote my own tests
against an ordinary twin node (the harness `KingresShardCompare`: the cluster's answer from each of the three nodes is compared with
the single node's, under the plan profiles "pushed" and "scan", with float normalisation), probed a live three-group cluster by hand
(`psql`, roles, prepared statements, cursors, killed group sessions, memory counters), and made one-line mutants of the rules of steps
7 to 10 and of the limit-by-parameter code.

Where I only read code I say so. Every number is from a run unless it says "read". All my clusters ran with the code defaults
(`enable_partial_aggregation` and `enable_pushdown_join` on; I set them on explicitly in the test configuration, which is the boot
value in `kingres_shard.c`), because the harness `make_node` writes both as `off` unless a test asks otherwise (see P2E-03).

Limits of this machine: the database locale is `C.UTF-8` and the only collations are `C`, `C.utf8` and `POSIX` (no ICU, no
`en_US`); the build has no XML support. Questions about non-C collations cannot be answered by a run here.

## What was run

| What | PostgreSQL 19rc2 | PostgreSQL 18.7 |
|---|---|---|
| Build, clean, `-Werror` | no warning | no warning |
| `pg_regress` (20 tests) | 20 of 20 | 20 of 20 |
| Shipped TAP tests of the steps: t/124 to 127, 135 to 139, 151 to 156 (15 files) | all 15 pass (with my two files: 17 files, 28729 assertions, 444 s) | all 15 pass (first run: 13 files, 14984 assertions; t/151 and t/156 died because the sketch extension was not in my private install; after I installed it they pass, 1043 assertions) |
| t/127 (seeded generator), seeds 101 to 105 | not run | 5 seeds pass (4350, 4324, 4340, 4330, 4362 assertions; two of them were run once while I stopped clusters by mistake, they failed, and were rerun alone: pass) |
| My t900: about 70 aggregates x 4 shapes (by non-key, whole, empty input, by expression with HAVING on an aggregate), unordered aggregates, closed aggregates with limit, 35 other shapes; pushed, scan, single | 9352 assertions pass | 9352 assertions pass |
| My t901: reference tables on either side of outer joins, DISTINCT / DISTINCT ON, windows, set operations, sub-selects, CTEs, LATERAL, WITH TIES, LIMIT / OFFSET of constants, parameters and stable functions in three plan cache modes, cursors, EXPLAIN ANALYZE in four formats on three nodes, functions in restrictions | 3376 assertions, 61 fail | 3376 assertions, 61 fail (same 61) |
| My t902: the four session profiles (DateStyle, IntervalStyle, TimeZone, bytea_output, ...) over temporal, float, array and jsonb reads on every path | not run | 440 assertions pass |
| My t903: 28 forged fragments sent to a group with a valid forward token (24 must be refused, 4 are accepted baselines), and 10 joins of three or four tables | not run | 332 assertions pass |
| t/125 and t/135 with partial aggregation and join rows switched on (my copies t905, t904) | not run | both pass (28 s, 142 s) |
| Hand probes on a kept cluster | not run | see "Answers" |
| Mutants of my own | not run | 33 one-line mutants, plus 11 re-runs against my tests (table in "Mutants") |

Of the 61 failures of t901 (identical on both majors): 9 are P2E-01; 3 are the documented cursor difference (P2E-08); the other 49 are
ties that my test compared as ordered (`ORDER BY` with ties and `OFFSET`, `FETCH FIRST ... WITH TIES` with the tied rows in another order,
`LIMIT` without `ORDER BY`, min/max of equal numerics of different scale, P2E-07). I checked the tied-row sets by hand on the cluster
for `WITH TIES`: they are equal to the single node's.

## Answers to the questions

**1. Does any shipped read give a different answer from a single node?** Yes, one: P2E-01 (critical), a `HAVING` clause that has no
aggregate is lost by the partial aggregation. Found while checking `HAVING` in a window above a grouping; reproduced on both majors.
One more on the fallback, not on a pushed path: P2E-02 (`TABLESAMPLE`). Everything else I compared gave the single node's answer:
NULLs in every type and in sort keys; float specials (NaN, -0, +-Infinity) in sums, groups, sorts and joins; numeric with different
scales (a `min` or `max` of equal values of different scale returns either scale, which a single node also does: P2E-07); `DISTINCT`
and `DISTINCT ON` by key and not; windows partitioned and not partitioned by the key, and above a partial aggregation; `GROUP BY`
expressions, alias, position, constant; empty inputs and a group with no rows (`count`, `sum`, `avg`, `array_agg`, `string_agg`,
`bool_and` over no row: one row, same values); sub-selects, CTEs, set operations, `IN`, `EXISTS`, `NOT EXISTS`, `ALL`, `LATERAL`,
correlated sub-plans (none of these is pushed; the answers are the single node's); reference tables on the preserved side of
`LEFT`, `RIGHT` and `FULL` joins and `count(*)` over them (not shipped, answers equal); `LIMIT` and `OFFSET` as constants, `1 + 2`,
`2.5`, `ALL`, `NULL`, `0`, past the end, `9223372036854775807` with an offset of 1 and of 9223372036854775807, as parameters of types
bigint, int, numeric, text (`length($1)`), expressions of parameters and of stable functions (`current_setting(...)::int`), `NULL`
and negative parameters (same error text as the single node), correlated `LIMIT o.w % 4` in a sub-plan; `FETCH FIRST n ROWS WITH
TIES`; prepared statements under `auto`, `force_custom_plan` and `force_generic_plan`, each executed twice; `EXPLAIN ANALYZE` with
VERBOSE and BUFFERS in TEXT, JSON, XML and YAML on all three nodes (all succeed); the four session profiles. The cursor difference is
documented and confirmed (P2E-08).

**2. Partial aggregation.** The rule `kingres_shard_aggregate_partial_ok` is a second layer: the planner's own
`GROUPING_CAN_PARTIAL_AGG` refuses the same aggregates first (mutants M02 to M05 survive for that reason; they accept an `ORDER BY`
inside the call, an ordered-set aggregate, a missing combine function, a missing serialization pair, and the answer stays the single
node's because the planner then makes no partial path). I compared `avg`, `sum`, `stddev`, `variance` of int2, int4, int8, numeric,
numeric(10,2), float4, float8, interval; `count(*)`, `count(col)`, `count(DISTINCT)`; `min` and `max` of text, text under `C`, int,
bigint, float, numeric, date, timestamptz, interval, int[]; `bool_and`, `bool_or`, `every`, `bit_and`, `bit_or`, `bit_xor`;
`corr`, the `regr_*` family, `covar_*`; `array_agg` and `string_agg` with and without `ORDER BY`; `json_agg`, `jsonb_agg`,
`jsonb_object_agg`, `json_object_agg`, `range_agg`, `mode`, `percentile_cont`; with `FILTER`; over empty input, by non-key, by
expression. All equal (floats to nine digits; the unordered ones by length or sorted). `sum(int8)` over values of 9e18 gave the
single node's numeric sum. A `sum(int4)` overflow of the combined state needs more than 2^32 rows; not run. `json_agg` and the
like are refused (no combine function), as the README says. Nothing is accepted that must not be, except that the shape rule
and the group's rule accept a statement for which the planner then declines; and one rule is wrong in what it removes:
P2E-01 (the group's text has no HAVING and the final aggregation lacks the quals the planner moved to WHERE).

**3. The ordered merge.** `merge_cmp` uses one `SortSupport` per key built from the sort operator, collation and `NULLS FIRST`; the
groups and this node's own part (SPI) sort with the same `ORDER BY` text. I checked every combination of ASC, DESC, NULLS FIRST,
NULLS LAST and the default over int, bigint, text, float (NaN, -0, infinities), numeric, bool, date, timestamptz, a column of NULLs,
several keys, a junk sort column, `DISTINCT` with `ORDER BY`, a join ordered, sorts on aggregates, and `COLLATE "C"`, `"POSIX"`,
`USING >`, `USING ~<~`, `upper(t)`; ties across groups (the tied rows are the same set). Mutants that damage the merge (M25 stops
at the first key that sorts higher, M26 loses `NULLS FIRST`, M20 pushes the limit of a `WITH TIES`) are killed by t/124 and t/153.
A group's stream cannot arrive unsorted, because the sort is part of the group's text and a text that the group would not accept is
refused. Collation "C" against the default cannot be separated on this machine (both are code point order); M18, which removes the
collation test, survives and I cannot judge it (see What I did not examine).

**4. Security.** I found no bypass. Row-level security: refused on every path (t/156 C passes); a table with the flag set is not
pushed. Column privileges: a role with `SELECT (id, c)` only gets `GROUP BY c` and `GROUP BY id` and is refused `sum(i)`, a filter on
`i` and `max(id), count(i)` on the cluster exactly as on the single node. `SECURITY DEFINER`: t/156 D passes; a function owned by a
role runs its pushed query as its owner. A view owned by another role: the single node answers; the cluster refuses with the clear
error "distributed table "m" is read through a view owned by role ..." for the closed, partial and join paths, and a
`security_invoker` view without privilege on the table is denied alike (README line "a view owned by another role is refused";
see P2E-06 for how the README words it elsewhere). `SET ROLE`: dropping to a role with no privilege is denied on the cluster as on
the single node (no bypass); going to a role that cannot log in fails (P2E-05). Forged fragments: 24 forged texts sent to a group
with a valid token (t903; 4 more are accepted baselines) are refused with `0A000` or `KS00x`: negative `LIMIT`, `OFFSET`, `WITH TIES`, `LIMIT NULL`, a `LIMIT`
sub-select, a partial text with a window, `DISTINCT`, `HAVING`, `ORDER BY`, `LIMIT`, `count(DISTINCT)`, `array_agg(... ORDER BY)`, an
ordered-set aggregate, `ROLLUP`, a set-returning function, an expression over an aggregate, no aggregate; a closed text with a window
not partitioned by the key, no key, `random()`, `now()`, a local table, `FOR UPDATE`. The accepted baselines (a closed text with a
limit, one with a bigint limit, a partial text) are accepted. The version marker: M30b (the extension version always accepted) is
killed by t/151. Search path, hostile identifiers and values: t/156 E and F pass on both majors; the group session runs with
`search_path = pg_catalog` (read in `kingres_shard_set_transmission_modes`).

**5. Robustness.** t/125 (faults, killed groups, bucket moves during a scan) and t/135 (`debug_discard_caches = 1`) pass on both
majors, and also with partial aggregation and join rows switched on (my copies t905 and t904 on 18; the shipped versions run with
both off, P2E-03). I terminated the backends of one group between two batches of a cursor over a merged closed scan
(`scan_fetch_size = 3`): the next fetch fails with "terminating connection due to administrator command" and the node named; no row
is silently missing. The same on a partial aggregation was not informative (the sort above the partial scan has read all rows
before the first fetch). Memory: three loops of 3000 executions in one transaction (a closed query with a limit that changes each
time, a partial aggregation with a changing filter, a join with a window): the sum of `pg_backend_memory_contexts` grew by about 9
to 10 kB over the second 3000 executions of each; no leak seen. Statement cancel of a pushed statement and bucket moves were not run
beyond the shipped tests.

**6. Mutants.** 33 mutants, table below: 10 killed by the shipped tests, 21 survive them, 2 failed to build (replaced by two variants).
Of the 21 survivors my tests kill 5 (M08, M12, M13, M14, M31). The 16 that survive everything are shielded by another layer (10),
equivalent (4) or not testable on this machine (2).

**7. The README.** It is honest about the snapshot skew, the cursor difference, the floating-point order, the extension that is absent
on a group, the refusals, and that `KS001` can appear where the scan would have waited. It overstates in three places: the sentence
"Every such path returns the rows the single node returns" is false while P2E-01 stands (and the fallback it names has P2E-02);
the text says in three places that a statement over "a view of another owner is not shipped" and that "the scan answers", while
the statement is refused with an error (it says so once, far below, in the limitations); and the new Limitations bullet about
differences lists the cursor and `KS001` but not the `SET ROLE` case (P2E-05).

## Findings

### P2E-01 (critical) A `HAVING` clause that contains no aggregate is dropped by the partial aggregation: the statement returns rows the single node does not, or fails

Reproduce (default settings; a distributed table `m(id int, c int, bits int, PRIMARY KEY (id, c))` on three groups, 300 ids with
c = 1 to 4 each, so 1200 rows, 300 per value of `c`):

```
SELECT c, count(*) FROM public.m GROUP BY c HAVING c < 3 ORDER BY c;
```

Expected (single node): 2 rows, `1 | 300` and `2 | 300`. Actual on every node, on both majors: 4 rows, `c` = 1 to 4. `EXPLAIN (VERBOSE)`
shows `Pushed: partial aggregate` and `Remote SQL: SELECT c, count(*) FROM public.m GROUP BY c`: the text has neither the HAVING nor a
WHERE. More forms, all wrong:

- `SELECT count(*) FROM public.m GROUP BY c HAVING c < 3` returns 4 rows instead of 2.
- `SELECT c + 1, count(*) FROM public.m GROUP BY c HAVING c + 1 < 4 ORDER BY 1` returns the rows for 2 to 5, not for 2 and 3.
- `SELECT c, bits % 2 AS p, count(*) FROM public.m GROUP BY c, bits % 2 HAVING c = 2 ORDER BY 1, 2`: expected `2 | 0 | 300`; actual
  `2 | 0 | 900` and `1 | 1 | 300` (the planner took `c` out of the grouping because the HAVING makes it a constant, and the
  constant is not in the text, so the final aggregation merges all values of `c` and prints one of them).
- `SELECT bits % 2, count(*) FROM public.m GROUP BY bits % 2 HAVING bits % 2 = 0` fails with `column "m.bits" must appear in the GROUP BY clause or be used in an aggregate function`; the single node answers.
- `PREPARE h(int) AS SELECT c, count(*) FROM public.m GROUP BY c HAVING c < $1 ORDER BY c; EXECUTE h(3)` returns 4 rows.
- `SELECT id, sum(count(*)) OVER (ORDER BY id) FROM public.m GROUP BY id HAVING id < 6` (a window above, so not closed) returns all 300 ids.

With `SET kingres_shard.enable_partial_aggregation = off` every one of them is right. A closed query (`GROUP BY id HAVING id < 4`) is right,
because the closed text keeps its HAVING. HAVING with an aggregate (`HAVING count(*) > 5`) and `HAVING true` are right.

Cause (read, and confirmed by the three forms above): `pushdown_prepare` copies the statement into `pi->ship` before planning. In
`fs_partial_paths` (`fragment_scan.c`) the text is built from that copy with `ship->havingQual = NULL` (the HAVING is meant to be
applied by this node), and the final aggregation applies `extra->havingQual`. But the planner's `subquery_planner` has by then moved
every HAVING clause that has no aggregate into the WHERE clause of `root->parse`, so `extra->havingQual` no longer holds them and the
text never had them: neither side applies the clause. The shape check `kingres_shard_partial_shape_ok` and the group's
`kingres_shard_partial_fragment_analyze` do not look at it.

Files and functions: `contrib/kingres_shard/src/fragment_scan.c`, `fs_partial_paths` (the lines `ship->havingQual = NULL` and the
construction of `ptarget` from `extra->havingQual`); `contrib/kingres_shard/src/planner.c`, `pushdown_prepare` (the copy before
planning).

Why no test saw it: every `HAVING` in t/139, t/151 and the generator t/127 (slot `@having`) is on an aggregate (t/139 lines 88 and 89,
t/127 line 74).

### P2E-02 (high; the fallback scan of phase 2a, not a pushed path) `TABLESAMPLE` on a distributed table returns the unsampled rows of every remote group

Reproduce (same table `m`):

```
SELECT count(*) FROM public.m TABLESAMPLE BERNOULLI (0);
SELECT count(*) FROM public.m TABLESAMPLE BERNOULLI (50) REPEATABLE (1);
```

Expected: 0 on a single node for the first, about 600 for the second. Actual: 792 for the first (only the local partitions are
sampled; every row of the remote groups comes back) and 1050 for the second (7/8 of the rows). The result is the same with
`kingres_shard.enable_pushdown = off`. The distributed scan asks the groups for `SELECT 1 FROM public.m AS ks_t`, with no sampling. The
pushdown refuses the statement with the reason "it samples a table at random" and so sends it to this scan, which ADR 0022 makes the
fallback of every refusal and which the README guarantees to be the single node's answer. This is older than steps 6 to 12 and I did not
read the scan's code; I report it because the refusal text of step 6 hands such a statement to it, and because `TABLESAMPLE` is
listed with the refused shapes only for writes.

File: the distributed scan (`contrib/kingres_shard/src/dist_scan.c` and the text it sends); not located further.

### P2E-03 (medium, test quality) The test harness switches off two of the on-by-default paths in every node it makes, so the fault, forged-fragment, generator and cache-discard tests of the steps never ran them

`make_node` in `t/KingresShardTest.pm` writes `kingres_shard.enable_partial_aggregation = off` and
`kingres_shard.enable_pushdown_join = off` into every node; a test turns them on only through `extra =>`. t/124 to 127 even write the
`off` again explicitly; t/135 (`debug_discard_caches`), t/136 to 138, t/152 and t/153 take the harness default. So the faults and bucket
moves (t/125), the generator (t/127), the forged fragments (t/126) and the discard-caches run (t/135) test the closed path only; the
partial aggregation and the join rows are tested for answers (t/139, t/151, t/154, t/155, t/156) but not under faults or discarded
caches. I ran t/125 and t/135 with both switched on (t905, t904 on 18): both pass, so this is a gap in what is checked, not a bug I
found. The generator has no slot for `HAVING` without an aggregate, which is how P2E-01 survived.

### P2E-04 (medium, test quality) Rules of steps 7 and 9 that no shipped test separates from their absence

Mutants M08, M12, M13, M14 and M31 survive every shipped test and are killed by tests I wrote (table in "Mutants"): a group that
accepts `DISTINCT` in a partial fragment, a negative constant `LIMIT`, an `OFFSET`, or `WITH TIES` in a closed fragment (the README
states these rules: "a group accepts a fragment with a limit only when it is one constant or one bigint parameter, with no OFFSET";
t/126 and t/156 forge fragments but none of these shapes), and the runtime sum of count and offset of a parameter limit without
the overflow test (a `LIMIT $1 OFFSET $2` with 9223372036854775807 and 1; the shipped t/155 has no overflow case, my t901 has 4 and
M31 gives 9 more failures than the baseline). The rules that survive for good reasons (shielded or equivalent) are listed under
P2E-09.

### P2E-05 (low) After `SET ROLE` to a role that cannot log in, every read that spans groups fails

Reproduce: `CREATE ROLE rc NOLOGIN; GRANT rc TO ra; GRANT SELECT ON public.m TO rc;` on every node; as `ra`: `SET ROLE rc; SELECT c,
count(*) FROM public.m GROUP BY c;`. Expected: the 4 rows (a single node answers). Actual: `could not connect to node ... FATAL: role
"rc" is not permitted to log in`. The groups are reached as `current_user`. The same with `enable_pushdown = off`, so the scan has the
limit too. It is not a leak (dropping to a role with no privilege is denied alike on both), and the README does not name it.

### P2E-06 (low) README wording

(a) The Guarantees bullet "Every such path returns the rows the single node returns" is not true while P2E-01 stands. (b) "a statement
over a table with row-level security or a view of another owner is not shipped" (Guarantees, the closed-query section and the new
Limitations bullet) and "the scan answers": for a view of another owner the statement is refused with an error on the closed, partial
and join paths and on the scan; the one line that says so is in the limitations of phase 2a, far from these. (c) `SET ROLE` (P2E-05)
is not in the list of differences.

### P2E-07 (note) `min` and `max` of equal numerics of different scale

`SELECT c, max(n), min(n) FROM m GROUP BY c` where `n` holds `57`, `57.00` and `57.000` returns any of them, and which one changes
between two runs of the same plan on the same cluster; a single node also leaves it open (it returns the first it meets). Reported
because my first comparison flagged it. Not a defect.

### P2E-08 (note) A default cursor over a pushed ordered statement cannot scan backward

Confirmed as the README says: with neither `SCROLL` nor `NO SCROLL`, `FETCH BACKWARD`, `FETCH PRIOR` and `FETCH LAST` on `SELECT id,
max(i) mi FROM public.m GROUP BY id ORDER BY mi DESC NULLS LAST, id LIMIT 20` and on `SELECT c, sum(i) FROM public.m GROUP BY c ORDER BY
c` fail with "cursor can only scan forward"; with `SCROLL` they answer as the single node does; `WITH HOLD` works. (3 failures in my t901.)

### P2E-09 (note) Rules that cannot be tested because another layer decides first, or that need a locale this machine lacks

Survivors of every test I have, by reason. Shielded: M02 to M05 (`kingres_shard_aggregate_partial_ok`: `ORDER BY` inside a call,
ordered-set kind, no combine function, no serialization pair; the planner's `GROUPING_CAN_PARTIAL_AGG` decides first), M06 and M09
(grouping sets and a set-returning function in the shape rule; `pushdown_analyze` and `fs_partial_paths` refuse first), M07 (a window in
a partial fragment: the shippability walker refuses a window function first), M16 and M17 (a negative count, or an overflow of count and
offset, pushed at plan time: the origin parses and analyses its own text with the group's rule and falls back to the scan), M23b (the
limit evaluable test always true: a sub-select is refused earlier by `hasSubLinks`). Equivalent: M15 (a `LIMIT` that is a parameter
other than an external one cannot appear in a received text), M21 (a volatile limit is evaluated once at the start either way), M22 (a
column in `LIMIT` is a parse error), M28 (a join fragment for a sub-join: answers equal, only cost differs). Not testable here: M18 (the
collation rule of the sort: needs a collation that differs from code point order) and M19 (the "built-in ordering operator" rule: needs
a user-defined ordering operator). The writer's notes already name M02 to M05 as shielded for other rules; these four are the same.

### P2E-10 (note, read only, not run) The ordered merge allocates its arrays again at every rescan

`fs_next_row_merged` allocates `msrc`, and `vals` and `nulls` of each source, in the executor's context when the merge starts;
`fs_stop` (called by `fs_rescan`) deletes the per-source row contexts but not those arrays. A rescan of a merged scan (a scrollable
cursor that goes back, or the scan as the inner side of a nested loop) therefore leaves a few hundred bytes behind each time. I did not
try to reach a case where the scan is rescanned often; the pushdown is applied at the top level of a statement only, which makes it
rare.

## Mutants

Each mutant is one changed line (or condition) of a copy of the tree, built clean with `-Werror` on 18.7 and run against the files
named in its group: A (partial aggregation and extension rules): t/139, 151, 152, 155; B (limit, sort, merge): t/124, 153, 155; C
(joins): t/154, 152, 124, 138; D (my tests): t901 and t903 for the survivors. A mutant is killed when a test fails. The shipped tests
passed unmutated on both majors.

| Id | File, rule changed | Shipped tests | My tests |
|---|---|---|---|
| M01 | pushdown.c: a partial aggregate accepts `DISTINCT` inside the call | KILLED (t/139) | |
| M02 | pushdown.c: accepts `ORDER BY` inside the call | survived | shielded |
| M03 | pushdown.c: accepts ordered-set and hypothetical aggregates | survived | shielded |
| M04 | pushdown.c: accepts an aggregate with no combine function | survived | shielded |
| M05 | pushdown.c: accepts an internal state with no serialization | survived | shielded |
| M06 | pushdown.c: shape rule accepts grouping sets | survived | shielded |
| M07 | pushdown.c: a group accepts a window in a partial fragment | survived | survived (shielded by the walker) |
| M08 | pushdown.c: a group accepts `DISTINCT` in a partial fragment | survived | KILLED (t903) |
| M09 | pushdown.c: shape rule accepts a set-returning function | survived | shielded |
| M10 | pushdown.c: a group accepts an aggregate inside an expression | KILLED (t/139) | |
| M11 | pushdown.c: a group accepts `HAVING` in a partial fragment | KILLED (t/139) | |
| M12 | pushdown.c: a group accepts a negative constant `LIMIT` | survived | KILLED (t903) |
| M13 | pushdown.c: a group accepts `OFFSET` in a closed fragment | survived | KILLED (t903) |
| M14 | pushdown.c: a group accepts `WITH TIES` | survived | KILLED (t903) |
| M15 | pushdown.c: a group accepts a non-external parameter as `LIMIT` | survived | survived (equivalent) |
| M16 | planner.c: a negative count or offset is pushed | survived | survived (shielded) |
| M17 | planner.c: no overflow test of count + offset at plan time | survived | survived (shielded) |
| M18 | planner.c: collation test of the pushed sort removed | survived | survived (not testable here) |
| M19 | planner.c: built-in test of the sort operator removed | survived | survived (not testable here) |
| M20 | planner.c: the limit of `WITH TIES` is pushed | KILLED (t/153) | |
| M21 | planner.c: a volatile function allowed in a limit expression | survived | survived (equivalent) |
| M22 | planner.c: a column allowed in a limit expression | survived | survived (equivalent) |
| M23 | planner.c: the evaluable test removed | does not build (unused function) | |
| M23b | planner.c: the evaluable test always true | survived | survived (shielded) |
| M24 | fragment_scan.c: a `NULL` count sends `LIMIT 0` | KILLED (t/155) | |
| M25 | fragment_scan.c: the merge compare stops at the first key that sorts higher | KILLED (t/124, 153, 155) | |
| M26 | planner.c: `NULLS FIRST` lost in the merge keys | KILLED (t/153) | |
| M27 | pushdown.c: a reference table on the preserved side of an outer join is accepted | KILLED (t/154) | |
| M28 | fragment_scan.c: a join fragment for any sub-join | survived | survived (equivalent: same answers) |
| M29 | shippable.c: `money` accepted | KILLED (t/139) | |
| M30 | shippable.c: the extension version test removed | does not build (unused variable) | |
| M30b | shippable.c: any installed extension version accepted | KILLED (t/151) | |
| M31 | fragment_scan.c: no overflow test of count + offset when the scan starts | survived | KILLED (t901: 70 failures against 61) |

Totals: 33 mutants; shipped tests kill 10 (M01, M10, M11, M20, M24, M25, M26, M27, M29, M30b), 21 survive them (M02 to M09, M12 to M19,
M21, M22, M23b, M28, M31), 2 do not build. My tests kill 5 of the 21. The 16 left: shielded 10 (M02 to M07, M09, M16, M17, M23b),
equivalent 4 (M15, M21, M22, M28), not testable here 2 (M18, M19). A first run of a mutant stream that overlapped another cluster was
not used; all counted runs were alone.

## What I did not examine

- Collations other than the code point order: no ICU and no `en_US` here. The claim that "C" and the default sort alike on the groups
  and on this node, and the whole class "a collation that is not shipped", were read, not run (M18 survives).
- Mixed-major clusters (a group on 18 and one on 19) and the state formats of partial aggregates across majors.
- The whole 119-file suite: I ran the files of the steps and my own, not the files of earlier phases.
- A statement cancel and a statement timeout during a pushed scan, and a failover during a scan, beyond what t/125 does.
- A `sum(int4)` that overflows the combined state (more than 2^32 rows).
- The code of the distributed scan (`dist_scan.c`) behind P2E-02.
- The objects of `kingres_sketch` beyond what t/151 and t/156 G test; XML aggregates (the build has no XML).
- Rescan memory (P2E-10) and memory of a very long-running cursor.
- The upgrade script and the version question of step 13; no `.sql` file changed in these steps and I did not rerun the upgrade test
  beyond the `pg_regress` `upgrade` test, which passes on both majors.
