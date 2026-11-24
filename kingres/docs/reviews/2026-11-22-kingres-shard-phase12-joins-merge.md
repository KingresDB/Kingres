<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026 RooyeKhat Media Co -->

# Independent review: kingres_shard phase 12 (repartitioned join and MERGE), 2026-11-22

Branch shard-p12 at dd3e9f4564, base 28a28bf518 (kingres_shard 0.8).
Reviewer: independent, no part in writing the code.

## 1. Scope and method

Scope: contrib/kingres_shard, the sharding part of phase 12.

- Repartitioned join: src/repartition.c, the repartition parts of src/fragment_scan.c, EXPLAIN lines, the fault points
  rp_after_ship and rp_before_fragment, partial aggregation above the join, the broadcast side, repartition_max_bytes.
- MERGE on distributed tables: src/merge.c, the MERGE parts of src/fanout.c, src/planner.c and src/shippable.c, shapes M1, M3,
  M4, M5, M6.
- The design note analytics-phase12-queries.md, the README sections "The repartitioned join" and "MERGE on distributed
  tables", and the newest worklog entry (its list of deviations).

Method:

1. Read the code of every piece above, with the group-side analysis (kingres_shard_merge_fragment_analyze,
   kingres_shard_text_accepted, the shipped-shape check) as the safety net to check against.
2. Built the extension from the tip on PostgreSQL 18.7 and 19.0, each with its own private install, and ran the suites of the
   change: t/260 to t/267 and t/043 (all pass on both majors; t/260 passes on both).
3. Wrote probe TAP files (not part of the repository) that compare the cluster with a single node, using the harness of the
   repository (KingresShardCompare): about 60 join queries (shape holes, NULL keys, numeric/float/bpchar/int2/bigint keys, HAVING
   without aggregate, DISTINCT and ordered-set aggregates, FILTER, prepared statements under force_generic_plan, volatile
   functions, the size bound), about 45 MERGE statements (M1, M3, M4, M5, M6, RETURNING with OLD/NEW, self sources, hostile
   names, dropped columns, savepoints, triggers, row-level security on a source, privileges, user-partitioned targets with a
   row moving between partitions, views as targets, prepared statements with unused parameters, typed values under the four
   session profiles). All of it passes on 19.0; the join, the first MERGE file and the partition probes also pass on 18.7.
4. Ran 23 mutants of the rules that carry correctness (section 5), each against the tests that should catch it.

The worktree of the review was never modified; mutants were made in private copies of the source.

Result in one sentence: no wrong answer was found in the repartitioned join (including the 2b critical HAVING case); one silent
wrong answer was found in MERGE (P12M-01, a prepared statement that declares a parameter it does not use), and several rules that
carry correctness are guarded by one check only and have no test that fails when the check is removed (P12M-02, P12M-03).

## 2. Findings

Severity scale: critical = data corruption or privilege escalation reachable by ordinary use; high = wrong answer or write
reachable by ordinary use; medium = wrong answer reachable only in unusual use, or a security-relevant guard with no test and a
fragile arrangement; low = error where PostgreSQL succeeds, documentation, defence in depth, usability.

### P12M-01 (medium): MERGE with a prepared statement that declares an unused trailing parameter ships the arrays at the wrong parameter numbers

Where: src/merge.c:731-732 (nparams_ext = ps.maxid; first_param = ps.maxid + 1), against src/fanout.c:1245-1264 (arrays appended
after params->numParams values) and src/fanout.c:798 (run_local, the same with n = numParams).

The shipped text of M3 and M5 puts the arrays at $(max used parameter + 1) and up. The origin appends them after all
numParams parameters of the statement. When a statement declares more parameters than it uses (legal; PREPARE and the
extended protocol accept it) the arrays sit at the wrong numbers.

Failing statements (cluster of three nodes, table t distributed by id, loc a local table with x in 50..100, rf a reference table):

    PREPARE m3(int, int, int) AS MERGE INTO t USING loc ON t.id = loc.x AND loc.z > $1
        WHEN MATCHED THEN UPDATE SET n = $2 WHEN NOT MATCHED THEN INSERT (id, v, n) VALUES (loc.x, loc.y, $2);
    EXECUTE m3(1, 5, 99);
    -> ERROR: function pg_catalog.unnest(integer) does not exist       (a single node runs it)

    PREPARE m5(int, int) AS MERGE INTO rf USING loc ON rf.k = loc.x - 90 AND loc.z > $1
        WHEN MATCHED THEN UPDATE SET label = 'm5';
    EXECUTE m5(1, 99);
    -> the same error

Silent wrong answer, when the unused trailing parameter is a text[]:

    PREPARE m3t(int, text[]) AS MERGE INTO t USING loc ON t.id = loc.x WHEN MATCHED THEN UPDATE SET n = $1;
    EXECUTE m3t(777, ARRAY['1','2']);

The single node updates the rows whose id is in loc.x (50..100). The cluster reports success and updates rows 1 and 2: the
group reads the caller's array as the shipped column of the source (verified: table contents differ from the single node on all
three nodes). Trigger conditions are unusual (an unused parameter of an array-of-text type), but the result is a wrong write with
no error, and a client that declares more parameters than it uses gets an error on a valid statement. M1, M4 and the
repartitioned join are not affected (the join renumbers its parameters; verified with unused trailing parameters).

Suggested fix: send exactly the first nparams_ext parameters followed by the arrays (truncate the parameter list to
FO_MI_FIRST_PARAM - 1 values in run_all and run_local; FO_MI_FIRST_PARAM is already in mergeinfo and unused), or renumber the
parameters as kingres_shard_renumber_params does for the join. Add a test with an unused trailing parameter of type int and of
type text[].

### P12M-02 (medium): guards that keep MERGE through a view, a row-level-security target or a CHECK OPTION view off the groups have no failing test, and the plain-view case is held by accident

Where: src/merge.c:239 (withCheckOptions), 243 (securityQuals and relrowsecurity), 246 (checkAsUser); src/merge.c:336
(merge_as_select drops q->jointree->quals); src/merge.c:520-523 (M3 refuses jointree->quals only by its text "USING is a join").

Mutants m26 (checkAsUser), m27 (target row-level security, both copies) and m29 (WITH CHECK OPTION) all survive t/263, t/264 and
t/265. Removing the securityQuals/relrowsecurity guard together with the other two gives wrong writes:
MERGE into a security_barrier view v2 of the distributed table t (`WHERE n < 3`), USING a colocated table (M1) or a VALUES list
(M3), changes or deletes rows of t that the view does not show, while the single node changes only the visible rows (verified:
table contents differ on every node). The deparsed text cannot carry the view's filter: a MERGE has no WHERE clause, and
securityQuals are never deparsed. So these guards are the only thing standing between a view-target MERGE and silent changes of
rows outside the view; no test fails when they go.

For a plain (not security_barrier) view the filter lands in q->jointree->quals. The code refuses it only by accident:
M1 is refused because the view's own range table entry (relkind v) makes the pushdown analysis say "not colocated", and M3 because
its test `q->jointree->quals != NULL` produces the message "USING is a join or several relations", which is wrong text for the
case (the user sees a message about USING for a MERGE into a view). merge_as_select() builds its SELECT with jointree quals
NULL, so the analysis itself would not see the view's filter.

On the pristine tip every view case is refused (verified: plain, security_barrier, WITH CHECK OPTION, view of another owner, with
a distributed or a VALUES source). Behaviour is correct today; the arrangement is fragile and untested.

Suggested fix: refuse explicitly and early in merge_common() when any range table entry of the statement is a view
(relkind RELKIND_VIEW) with a message "MERGE through a view is not supported"; keep the three guards; add t/263 and t/264 cases
for a plain view, a security_barrier view, a WITH CHECK OPTION view, a view of another owner and an RLS-enabled target (a
non-owner role with a policy), each for M1 and M3 and for M4 (reference target).

### P12M-03 (low): a source row whose group is not among the planned targets is dropped silently; the final placement recheck is the only backstop and is untested

Where: src/merge.c:1332-1333 (`if (share == NULL) continue;`), src/fanout.c:1335 (final recheck).

In kingres_shard_merge_collect the group of a row is found from the colocation entry read at that moment; if the bucket map was
refreshed between the start check and the collect (the SPI cursor opens and may accept invalidations) and a bucket now belongs
to a group that was not a target when planned, the row is skipped without error. The final recheck (theirs != origin_placement)
turns this into KS001, but mutant m09 (that recheck removed) survives t/266 and t/262: nothing drives a move across the window
after the last group has run. In the repartitioned join the same line is correct (a pruned group's rows are not needed).

Suggested fix: in merge_collect, make `share == NULL` an error with code KS001 (stale map), as a defence that does not depend on
the later check; add a test that moves a bucket (to a group with no bucket of the colocation group) at the fault point
merge_before_first_group and checks the statement fails and changes nothing.

### P12M-04 (low): the MERGE fault points do not exist for M4 and M5

Where: src/fanout.c:1226 and 1305 (FO_IS_MERGE is true for FO_KIND_MERGE and FO_KIND_MERGE_ROUTED only; M4 and M5 run as
FO_KIND_WRITE).

merge_before_first_group, merge_between_groups and merge_before_commit never fire for a MERGE into a reference table, so the
atomicity of M4/M5 across nodes (an error or a kill after some nodes ran) is covered only by the general reference-write tests,
not by the MERGE test file t/266. Suggested fix: fire the three points for reference MERGE too (pass the kind or a flag) and
extend t/266.

### P12M-05 (low): documentation of MERGE is stale in two places of the README

- README.md line 1331 (table of reference-table behaviour): "`MERGE`, `COPY ... FROM` | refused". A MERGE into a reference table
  runs since phase 12 (M4, M5); only COPY FROM is refused there.
- README.md lines 2250-2256 ("Writes that span groups": MERGE is refused; "UPDATE ... FROM, DELETE ... USING and MERGE that read a
  distributed table and write another table are refused"). A MERGE into a distributed table, a reference table, or a local table
  that reads distributed tables (M6) runs since phase 12.

The new section (line 1533 on) is accurate and honest about snapshots and concurrency. Fix: edit the two stale passages.

### P12M-06 (low): the refusal for a target column with a stable default does not name the default

A MERGE (M1 or M3) with an INSERT action that does not list a column whose DEFAULT is a stable function (now(), current_user,
current_setting()) is refused with "it uses a function that is not built in and immutable" (or "it has an expression of kind 40
that cannot be shipped" for current_user). The refusal is correct (each group would compute its own value) but the message
does not say that the default of the column is the cause, and the README list of refusals mentions volatile values but not stable
defaults. Suggested fix: name the column and say "give it a value in the INSERT list", as the volatile case does; mention stable
defaults in the README list.

### P12J-01 (low): repartition_max_bytes can be set above what the transport can carry

Where: src/repartition.c:92 (`65536, 1024, MAX_KILOBYTES`), against src/merge.c:80 (merge_max_source_bytes is capped at
524288 kB).

The shipped rows of a group are one text array in a StringInfo and a psprintf("{%s}") copy; both fail at 1 GB. A setting above
about 1 GB turns the friendly bound error (54000, hint to raise or switch off) into "invalid memory alloc request size" at an
unpredictable row. No wrong answer. Suggested fix: cap the setting at 512MB like merge_max_source_bytes, or check the buffer
against MaxAllocSize in repart_collect.

### P12J-02 (low): the choice of the table that may move under a LEFT JOIN is guarded twice and tested once

Where: src/repartition.c:550 (only the nullable table is a candidate).

Mutant m01 (preserved tables become candidates) survives t/260 and t/267 and my probes. Cause: the group-side text analysis
refuses the moved preserved side ("the preserved side of an outer join is replicated"), so the planner never offers the path.
This is sound defence in depth, but then no test exercises the rule at src/repartition.c:550 and the rule is not needed for
correctness. Either keep it as an optimisation and say so in the comment, or add a forged-fragment test (t/261) that sends a
fragment with a moved preserved side directly to a group and asserts the refusal, so the group-side rule is pinned.

### P12J-03 (info): any HAVING conjunct without an aggregate switches the repartitioned join off completely

Where: src/fragment_scan.c:1467, 1726, 1944 (having_has_moved_conjunct). Correct (the mutant that removes the check is killed by
t/260 and by my probes: the HAVING condition is lost and rows are wrong), but conservative: `HAVING cu.region > 2` on a
repartitioned join falls back to the row-moving scan, and EXPLAIN says "not used: its HAVING has a condition without an
aggregate". The same query with the condition written in WHERE is repartitioned. A cheaper design would copy the condition into
WHERE of the shipped text. Not a defect.

## 3. Verified correct

Repartitioned join:

- NULL keys: a NULL key of the moved table joins nothing and is dropped (inner and nullable side of a LEFT JOIN); NULL keys of
  the preserved side stay and are null-extended; a broadcast side keeps them. Probed with 25 percent NULL keys and a LEFT JOIN
  whose WHERE names the preserved side or the nullable side (the second is refused with a reason).
- Key typing: int2/int4/int8 against each other (including a bigint value beyond the int range), numeric with different scales,
  float8 with NaN, -0 and infinity, bpchar (refused for shipping, falls back), numeric against int by a cast (broadcast), text
  keys with multibyte values; all equal to the single node under both plan profiles.
- LEFT JOIN with a broadcast side, with a routed side, a reference table as the preserved side, ON clauses that equate two
  preserved tables (the case that would lose cross-group pairs is refused), ON conditions on the preserved table (decide the
  NULLs, stay in the text), constant false and NULL ON clauses.
- Partial aggregation above the join: HAVING without aggregate (refused, correct), HAVING false and NULL without GROUP BY,
  HAVING with aggregate and with OR, DISTINCT aggregates, ordered-set aggregates (percentile_disc, mode), array_agg and
  string_agg with ORDER BY, FILTER, float sums, DISTINCT, DISTINCT ON, grouping sets (not pushed), window functions.
- Parameters and prepared statements: generic and custom plans with parameters in the moved and the staying filters and in the
  join expression; an unused trailing parameter (correct for the join; see P12M-01 for MERGE).
- Volatile functions: in the ON clause, in the WHERE on each side, in the ON of a broadcast LEFT JOIN; the evaluated-once cases
  (a conjunct that names only the moved table) are evaluated by its own scan once per row.
- Typed values under the four session profiles of the harness (DateStyle, IntervalStyle, bytea_output, TimeZone,
  extra_float_digits): equal to the single node, for a MERGE and for a repartitioned join, on all nodes.
- Bucket moves between ship and fragment: t/262 passes on both majors; the rule "placement of the groups is checked again by the
  group, KS001, never a wrong answer" holds on the code path I read (placement check of the group against the marker, after
  planning the fragment).
- repartition_max_bytes: a bound below the need gives an error, never a wrong answer (mutant m33 is killed by t/260).
- Security: the moved table is read by the origin as the user (permissions, row-level security refuse the path); the groups run
  the fragment in the forwarded session; the shipped data is passed as parameters, never interpolated; names are quoted.

MERGE:

- M1, M3, M4, M5, M6 results equal the single node for the shapes I tried, on 19.0 (and on 18.7 for the first set):
  UPDATE, DELETE, INSERT, DO NOTHING, NOT MATCHED BY SOURCE with an empty source and a partial source, RETURNING with
  merge_action(), with OLD and NEW, in M1, M3, M4 and M5, self sources (the target as USING, a sub-select over the target),
  hostile names (quotes in table, alias and column names; columns called u, c1, ks_c1, ks_route), a dropped column in the source,
  a partitioned target with a row that moves between user partitions, an INSERT through the partitions, EXPLAIN ANALYZE of M1 and
  M3, savepoints with a rollback inside, prepared statements with parameters in ON, in WHEN conditions, in SET and in the
  source sub-select under force_generic_plan.
- NULL route key: a NULL key goes to the group where the row router puts a NULL key (verified by reading each group with routing
  off: the router's row and the two MERGE rows are on the same group).
- Atomicity: t/266 passes on both majors; an error on any group leaves nothing changed (t/263 section C).
- Triggers and the ledger in M6: row-level trigger of the local target fires once per row, with the same log as on a single node
  (my probe) and t/265 section D for the ledger.
- Row-level security on a source (M1, M3, M6), privileges on the source and on the target, as an ordinary role: same status and
  same table contents as the single node; a role without the privilege is refused by the origin.
- Views as targets: refused on the pristine tip in every case I tried (see P12M-02 for how).
- Volatile and stable values: sequences, identity columns without a value, random(), now() in SET, current_user, current_setting
  are refused with a message (not silently shipped).
- Concurrency: the README states honestly that the source is read once at the start, each group reads its target in its own
  snapshot and READ COMMITTED re-evaluation is as on one node. I found no case where the cluster is weaker than that text. (Not
  tested here: two concurrent distributed writers; the per-group semantics are those of the server on each group.)
- Table kind change between plan and run (the gap the writer names): not exercised by a test of mine. By the code: M1 and M3
  take a relations signature at plan time and check it at the start of the run (FO_IS_DIST_WRITE); M4 and M5 run without that
  check but every node analyses the received text itself and refuses a target that is not a reference table; M6 relies on the
  distributed scan's own checks and on relation invalidation. All three end in an error, not a wrong answer, as far as I can
  see, but only M1 and M3 are protected by an explicit check at the executor.
- Comparison with 19.0: the suites and my probes pass on 19.0 and show no difference from 18.7 (RETURNING OLD/NEW, NOT MATCHED BY
  SOURCE, cross-partition UPDATE, the OFFSET 0 fence of M6). I did not find a 19.0 behaviour that the code assumes differently.

Deviations of the worklog:

- Marker `kingres:merge-fragment place=N rp=K` instead of `kingres:frag v=2 kind=merge`: sound. It is recognised in the list of
  markers that a non-forwarded session may not use (planner.c), parsed strictly, and checked against the count of shipped
  sub-selects; mixed versions are excluded by the existing rule that nodes must agree on the extension version.
- M3 and M5 need one relation as the source: sound (stated in the README, refused with a message, workaround given).
- A key restriction in ON does not prune M1: sound (always correct, only slower).
- A NULL route key hashed like the row router: verified, see above.
- Not built (preserved moved side of a LEFT JOIN, dual repartition, closed fragment above the join, extension objects in the
  text): each is refused with a reason and the complete scan answers; checked by t/260 section C and my probes.

## 4. Test quality

Strong: t/260 (3130 checks), t/263 to t/267 run each statement on the three nodes and a single node, compare tables and
RETURNING rows, and assert the path from EXPLAIN. The killed mutants (section 5) show the core rules are pinned.

Gaps (each is a surviving mutant or a probe that found something):

- No test for a prepared MERGE with a declared but unused parameter (P12M-01).
- No test for MERGE into a view, a target with row-level security, or a CHECK OPTION view (P12M-02): m26, m27, m29 survive.
- No test that moves a bucket after the last group ran (m09 survives) or between the plan check and the collect (P12M-03).
- No fault-point test for M4 and M5 (P12M-04).
- The preserved-side rule of the join is not pinned by a test (P12J-02).
- t/260 compares repartitioned queries with `profiles => 0`; session-profile equivalence of the typed values is tested for the
  shipped columns only through t/267 if at all. My probe (all four profiles, MERGE and join) passes, so the code is right; a
  permanent test would keep it so.

## 5. Mutants

Each mutant was applied to a private copy, built clean on 19.0 and run against the listed tests. "Killed" means a test failed.

| id | change | tests run | result |
|---|---|---|---|
| m01 | repartition.c:550 allow a preserved table of a LEFT JOIN to be a candidate | t/260, t/267, probes | survived: group-side text analysis refuses it (P12J-02) |
| m02 | repartition.c:480 drop the refusal of a WHERE that names only the nullable side | t/260 | killed (10 checks) |
| m03 | fragment_scan.c:1467, 1726, 1944 drop the HAVING-moved-conjunct check | t/260, t/267, probe | killed (t/260: 10; probe: 51) |
| m04 | fragment_scan.c:1361 drop the pruning rule of a self join | t/260, t/262, t/267 | killed (4 checks) |
| m33 | repartition.c:970 drop the repartition_max_bytes check | t/260 | killed (4 checks) |
| m05 | fanout.c:1214 skip a group with no source row even when the statement has NOT MATCHED BY SOURCE | t/263, t/264 | killed (30 checks) |
| m06 | merge.c:250 drop the unique-index-carries-key check | t/263, t/264 | killed |
| m07 | merge.c:248 and 1439 allow a target with a trigger or a rule | t/263, t/264, t/265 | killed |
| m08 | merge.c:505 accept any INSERT key in M3 | t/264 | killed (32 checks) |
| m09 | fanout.c:1335 drop the placement recheck after the groups ran | t/266, t/262 | survived (P12M-03) |
| m10 | merge.c:1741 drop the OFFSET 0 on a sub-select of M6 | t/265 | killed |
| m11 | planner.c:1532 drop the marker count check of a repartitioned fragment | t/261, t/267 | killed (7 checks) |
| m12 | merge.c:265 allow an UPDATE of the distribution column | t/263, t/264 | killed |
| m13 | merge.c:460 drop the route collation equality (the determinism test stays) | t/264 | survived: equivalent for deterministic collations |
| m14 | merge.c:973 drop the shipped-sub-select count check | t/263, t/264 | survived: the shape checks refuse the same texts |
| m23 | merge.c:365 drop the INSERT key check of M1 and M3 | t/263, t/264 | killed |
| m24 | merge.c:275 drop the volatile-value check | t/263, t/264 | killed |
| m26 | merge.c:246 drop the view-owner check | t/263, t/264, t/265, probe | survived (P12M-02) |
| m27 | merge.c:243 and 1437 drop the target row-level security check | t/263, t/264, t/265, probe | survived; with m26 and m29 together, wrong writes through a security_barrier view (P12M-02) |
| m29 | merge.c:239 and 1435 drop the CHECK OPTION check | t/263, t/264, t/265 | survived (P12M-02) |
| m34 | merge.c:1349 drop the merge_max_source_bytes check | t/264, t/265 | killed (24 checks) |
| m37 | merge.c:304 NOT MATCHED BY SOURCE no longer makes the analysed join a LEFT JOIN | t/263, t/264, t/265 | survived: I found no statement whose answer depends on it (the colocation analysis needs the key equality either way) |
| m38 | merge.c:237 drop the sub-select check | t/263, t/264 | survived: shippable.c refuses the same expressions with a similar message |

Nine survivors: m01, m13, m14, m37, m38 are equivalent or defence in depth; m09 and m26/m27/m29 are test gaps (P12M-03,
P12M-02).

## 6. What was not done

- Two concurrent distributed writers on the same rows (MERGE against MERGE, MERGE against UPDATE): relied on the per-group
  server semantics and the README text.
- A kill or a crash between the groups (t/266 does this; I ran it, I did not extend it).
- A plan-to-run change of the kind of a table with a second session (reasoned from the code, section 3).
- Performance and cost choices.

## 7. Fixes

Branch p12-fix (from shard-p12 dd3e9f4564).  One commit for each finding, each with a test that fails without it (the mutant that undoes the
fix is killed by that test, on 19.0; the new files also pass on 18.7).  The tests are t/268 (new), t/266 and t/261.

| id | what changed | commit | the test that fails without it |
|---|---|---|---|
| P12M-01 | `run_all` sends the first `FO_MI_FIRST_PARAM - 1` parameters followed by the arrays, on the local part and on the remote nodes (a parameter that is declared and unused no longer shifts the arrays) | c18a9330f8 | t/268 section A: unused leading, middle and trailing parameters of nine types for M1, M3, M4, M5 and M6 under custom and generic plans; mutant m01 (all declared parameters sent): 239 assertions |
| P12M-02 | `mg_target_guards()` (CHECK OPTION, securityQuals/relrowsecurity, view owner, and a new explicit refusal of a view among the range table entries: "MERGE through a view is not supported") is asked on the statement as written by `merge_common` (M1, M3, the groups), the reference analysis (M4), `kingres_shard_merge_build_ship` (M3, M5) and, first of all, `kingres_shard_merge_classify_ref` (so that a refusal of M4 can no longer be followed by an M5 text that has lost the filter); `merge_as_select` returns NULL for a statement written through a view | cfeb8b40ee | t/268 section B (plain, security_barrier, CHECK OPTION and other-owner views, a target with row-level security as a subject role and as the owner; M1, M3, M4; UPDATE, DELETE, INSERT); mutants m26, m27, m29, m30, m38 killed |
| P12M-06 | the refusal for a column with a stable default names the column and says "give it a value in the INSERT list" (`mg_stable_default_insert`) | af13728459 | t/268 section C (ts, who, cfg; M1, M3, M4, M5; and the same statements with values); mutant m33 killed |
| P12J-01 | `repartition_max_bytes` is capped at 512MB (1MB minimum) | 0826a469f0 | t/268 section D ('1GB' and 524289 kB refused, '512MB' works and a repartitioned join runs under it); mutant m34 killed |
| P12M-04 | the plan of a MERGE into a reference table has its own kind (`FO_KIND_MERGE_REF`) and the three merge fault points fire for it | 5d5f163eeb | t/266 section E (M4 and M5: an error at every point, a cancel at the merge points, a node that dies, an origin that dies); mutant m32 killed (61 assertions) |
| P12M-03 | `kingres_shard_merge_collect` raises KS001 for a source row whose group is not a planned target (it dropped the row); new fault point `merge_before_collect` | cfc92818e5 | t/266 section F (a bucket moves to an empty group at `merge_before_collect` and at `merge_before_first_group`); mutants m31 and m09 killed |
| P12J-02 | comment of the preserved-side candidate rule says it is an optimisation; the group-side rule is pinned by forged fragments | 0d5a88b41b | t/261 section G (moved table on the preserved side of a LEFT JOIN and of a RIGHT JOIN refused, controls run); mutant m35 killed |
| P12M-05 | the two stale README passages are corrected, and the README follows the limits that changed (views, stable defaults, unused parameters, fault points, the cap) | 4ff708a732 | documentation only |
| P12J-03 | information only: nothing done | | |

What the fixing found beyond the review:

- **A security_barrier view over a reference table was not refused.**  `kingres_shard_merge_classify_ref` tries M4 and then M5 on the same statement.  M4
  refused the view (its filter is in securityQuals), but M5 built its text from the statement, and the deparsed text has no WHERE clause and no
  securityQuals, so M5 accepted it and every node changed the rows that the view hides (in the test the statement failed only because the view did not
  exist on the other nodes).  The review had tested a plain view over a reference table, which the filter in the jointree refused by accident, and a
  security_barrier view only over distributed tables, where `merge_common` refuses it before M3.  This is the case that the guards now stop for every shape
  (P12M-02, mutant m38).
- **M5 locks every table of the statement on every node, a local source included.**  `reference_lock_names` takes the writer lock of every relation of the
  statement (`collect_relids_walker`), so an M5 whose source is a local table that exists on one node only fails at the first other node with "reference table
  ... does not exist on this node".  The tests of t/265 create the source on every node and so do not see it.  Not changed here (it is not a wrong answer and
  is outside the list of this review); t/266 section E creates the source on every node.  A fix would take the lock names from the reference tables of
  `m.relids` only.
- **t/266 section F, the group side**: the origin's own group does not check the placement that the statement started with (the local part is planned by
  `pass_through`), so the final placement recheck is the only check for a statement that runs on the origin's group alone; the test shows it
  (mutant m09).

Mutants of the fixes (the review's own m09, m26, m27 and m29 keep their numbers; the others are numbered for the fixes; all on 19.0):

| mutant | change | result |
|---|---|---|
| m01 (of the fixes; not the review's m01) | all declared parameters are sent again (P12M-01) | killed by t/268 (239 assertions) |
| m09 (review) | the final placement recheck of `run_all` removed | killed by t/266 section F (3 assertions) |
| m26 (review) | the view-owner guard removed | killed by t/268 section B (9) |
| m27 (review) | the row-level security guard removed | killed by t/268 section B (39) |
| m29 (review) | the CHECK OPTION guard removed | killed by t/268 section B (6) |
| m30 | the explicit view refusal removed | killed by t/268 section B (12) |
| m31 | a source row of a group that is not planned is dropped again | killed by t/266 section F (1) |
| m32 | the merge fault points do not fire for a reference MERGE | killed by t/266 section E (61) |
| m33 | the stable-default check removed | killed by t/268 section C (12) |
| m34 | `repartition_max_bytes` maximum back to MAX_KILOBYTES | killed by t/268 section D (3) |
| m35 | the group's rule "the preserved side of an outer join is replicated" removed (what the review's m01 relied on) | killed by t/261 (4: the earlier case F and the new section G) |
| m36 | the view test of `merge_as_select` removed | survives: `merge_common` refuses a view before `merge_as_select` is reached (second layer) |
| m38 | the guards removed from `classify_ref` and `build_ship` (the state before the fix) | killed by t/268 section B (3: the security_barrier view over a reference table) |
| m39 | only the early call of the guards in `classify_ref` removed | survives: `build_ship` and the reference analysis refuse the same statements (second layer) |
