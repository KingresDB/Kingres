<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->

# Review of phase 2b, step 13 ("round 2b11")

## Scope and method

Reviewed: branch `shard-2b11`, head `e046ad1452`; the round is the commits 9e4d229286, e31f3c11d1 and b0e59ece25 (the diff of
`contrib/kingres_shard` over them), read together with `fragment_scan.c`, `shippable.c`, `pushdown.c`, `placement_check.c`, `node_conn.c`
and the new tests t/157 to t/162. The merge of the already reviewed fixes was not re-reviewed.

Method: a read of the diff and of every reader of the statics in `shippable.c`; the round's tests and the older suites on both majors;
one-line mutants of the code (one at a time, clean build, tests of the round plus the neighbours); and five own TAP tests (kept outside the
repository, in a private copy of the extension directory) that crash-hunt, compare against a single-node twin, compare nodes with each other
on ties, check the README's limits against the plans, and probe the KS001 conversion.

Environment: all builds and runs in WSL with private copies of the two installations (see "Process note" at the end). Results below are from
the private installs.

## What was run

| Run | PostgreSQL 18 | PostgreSQL 19 |
|---|---|---|
| t/157-162, t/124-127, t/151-156, t/170-172 (19 files, clean build with -Werror) | PASS, 16906 assertions | PASS, 16894 assertions |
| own t/190: 180 generated statements (sketch and plain aggregates, closed/partial/scalar, HAVING, ORDER BY, LIMIT, OFFSET) compared with a single node under the `pushed` and `scan` profiles, before and after 6 bucket moves, plus a crash hunt (8 session scripts) | PASS, 5120 | PASS, 5120 |
| own t/193: ties from every node, before and after bucket moves and after a fourth group is added, with a parameter limit and offset, repeated | PASS, 51 | PASS, 51 |
| own t/194: group without the extension (see P2F-01) | 5 of 7 fail, as the finding says | 5 of 7 fail |
| own t/192: README limits against the plans (26 statements) | 1 case wrong by my own test (see "README check"), 4 failures are a defect of my test (a view exists only on node 1) | same |

The first full runs were made against the shared installs and were redone against the private ones; the numbers above are the private ones
(the earlier runs had the same verdicts: 16924 and 16872 assertions).

## Answers to the questions

**Is the crash fix complete?** For the reported crash, yes. `ship_used_ext` and `ship_allow_ext` are read in three places: inside the walk
(after every entry point has reset both), in `kingres_shard_shippable_ext_used()` (called by `pushdown.c` right after a successful walk, lines
1097 and 1312), and in `kingres_shard_extension_aggregate_ok()` (the fixed one, which now starts its own empty list and puts the old pointer
back without reading it). All five walk entry points reset both statics before they walk. I found no reader that can run on a stale list.
A leftover `ship_allow_ext = true` after a walk does no harm because nothing but the walkers consults it. I tried to crash a group and the
coordinator with: a prepared statement executed 9 times (custom and forced generic plan), a parameter limit, interleaved sketch and plain
partial/closed statements in one session, a plan-time error and an execution error between sketch statements, a rolled-back transaction,
nested planning from a plpgsql function called per row, a DO loop, cursors, and DISCARD ALL. No node crashed on either major (logs checked
for signals and TRAP). The mutant that removes the fix (M6) is killed by t/157 as a crash; the mutant that removes the restore line (M7) is
equivalent today (nothing reads the list afterwards) and survives, which is fine but means the restore is untested.

**Does the KS001 conversion turn a different error into a misleading KS001, or hide one?** It is both too narrow and too wide. Too narrow: it
does not cover the main case, a node that never had the extension (P2F-01). Too wide: it converts by SQLSTATE only (42883 and 42704) and not
by cause (P2F-02), and the test that says it does not do this does not reach the code (P2F-03). I could not make a user trigger a wrong
conversion: `current_setting()` of a custom setting (which raises 42704 on a node that lacks the setting) is not shipped, built-in
functions are only immutable ones, and collations are limited to the default and C; so within the shipped subset the only 42883/42704 a
group raises at DECLARE is a missing extension object, a function dropped on one group, or a mixed-major cluster (refused by `add_node`).
A SQLSTATE `3F000` is not converted at all.

**Is the order by group id stable and correct?** Yes in everything I tried. t/193 and t/190 on both majors: ties on a non-unique key
come out identically from the three nodes (the own part is no longer first), a repeated statement gives the same order, with a LIMIT, with a
LIMIT and OFFSET, with `LIMIT $1 OFFSET $2` (custom and generic), after four bucket moves, and after a fourth group (the new group id sorts
where its id says). The unlimited tie order is non-decreasing in group id. Against the single node, ordered statements with a total order
(key as last sort column) are identical in 180 generated statements per major, before and after 6 moves; no query returned other rows than
the single node. Residual remark (P2F-06): the claim "the same order when the statement is repeated" also depends on the order inside one
group, which is the group's own (a parallel plan in a group can reorder equal rows between two runs); the README sentence does not say so.

**Do the new tests fail when the property breaks?** Mostly yes; see the mutant table. Survivors of the round's tests: the sqlstate filter
of the conversion (both M1b and M2), the quoting of the column list in `copy_routed.c` (M11), the count + offset of a parameter limit
(M16: killed only by the older t/155 and t/172), and the two re-checks after the DECLARE (M8, M9: survive t/157, t/125, t/139, t/158, t/153,
t/154, t/124, t/126, t/136, t/151, t/152).

**Generated statements against a single node, both majors:** done (t/190), no difference found, 5120 assertions per major, 0 failures.

**README claims and known limits:** checked against the plans of 26 statements; they hold (see below), with two corrections, P2F-01 (the
README promises KS001 for a node without the extension) and P2F-04 (t/161 and its header promise the pushdown paths under a ledger).

## Findings

### P2F-01 (medium): a node that never had the extension is not turned into KS001; the README says it is

`declare_missing_extension()` converts only SQLSTATE 42883 (undefined_function) and 42704 (undefined_object). A group that does not have the
extension at all has no schema `kingres_sketch`, and PostgreSQL then reports `3F000 schema "kingres_sketch" does not exist` for a qualified
function or type name. The user gets that raw error, not the KS001 with the node, the version and the hint. The conversion works only in
the situation t/162 builds: the extension was installed and then dropped (`DROP EXTENSION` leaves the schema behind, so the group says 42883).
Dropping the schema too gives 3F000 again.

Reproduction (own t/194, both majors): three nodes, extension on nodes 1 and 2 only, then
`SELECT g, kingres_sketch.approx_count_distinct(c) FROM public.s GROUP BY g ORDER BY 1` (and the closed variant with `GROUP BY id ... LIMIT 5`)
from node 1: SQLSTATE 3F000. After `CREATE EXTENSION` and `DROP EXTENSION` on node 3: KS001. After also `DROP SCHEMA kingres_sketch`: 3F000.
A direct `SELECT kingres_sketch.approx_count_distinct(1)` on node 3 gives 3F000 before and 42883 after the drop.
The README (section on extension objects, "A group that does not have the extension at all ...") and the note of the round say the
conversion covers this case; t/162 passes because it creates and drops the extension.

Suggested fix: do not guess from the error. The probe that already runs on every group before the DECLARE (`probe_read_before`) can also ask
`pg_extension` for the names and versions in the marker and raise the KS001 itself, which covers a missing schema, a missing function and a
version difference in one place and cannot mislabel another error. At the least add 3F000 to the converted codes. Make t/162 a node that
never had the extension, not one that had it dropped.

### P2F-02 (low): the conversion is by SQLSTATE, so any undefined function or object in a statement that names an extension is reported as a missing extension

With a marker that has `ext=`, every 42883 or 42704 at DECLARE becomes "does not have an extension that the statement uses". The note of the
round lists one case (a user function dropped on one group). The hint ("a repeat does not help until the extension is installed ...") would
then send the user in the wrong direction. I could not produce another case from user input (see the answers above), so this is low. It is
fixed by the same suggestion as P2F-01 (decide by the catalog, not by the SQLSTATE).

### P2F-03 (medium, test): the "other error is not converted" test never reaches the conversion; the sqlstate filter is untested

t/162 A checks that a division by zero is not reported as a missing extension, but the division happens in the first FETCH, not in the DECLARE,
so `declare_missing_extension()` is never called for it. Mutant M1b (convert every SQLSTATE) and mutant M2 (drop the 42704 branch) both pass
the whole of t/162. Reproduce: in `fragment_scan.c` change the filter to `if (sqlstate == 0) return;` and run t/162.
Suggested fix: a statement whose DECLARE fails for another reason while the marker names an extension, for example a forged or stale
fragment text that names a missing column, a type that does not exist, or a failed `SET`; assert the original SQLSTATE comes through. Also a
test for the 42704 path (a type of the extension).

### P2F-04 (medium, test and claim): t/161 does not run the pushdown paths under the ledger

The INSERT ... SELECT into a ledger table reads its source through the 2a `Kingres Distributed Scan` (plans printed in own t/191 on a local
target: `Insert on loc -> HashAggregate -> Custom Scan (Kingres Distributed Scan) ... Remote SQL: SELECT g FROM public.s`), for the partial
aggregation, the ordered merge with a limit, the closed aggregate and the plain scan alike. t/161 asserts the path only on the bare SELECT, and the
notes of the round say so, but the header of t/161, the claim of design test 068 in the round, and the test names ("INSERT INTO ledger SELECT <partial aggregation>")
read as if the Fragment Scan were under the ledger. What is tested is the ledger with the 2a scan. The Fragment Scan is not tested with the ledger at all.
Suggested fix: use a source that is planned at top level and still feeds the ledger table, for example `INSERT INTO led SELECT * FROM f()` with a plpgsql
`RETURN QUERY` that runs the pushed statement, or a COPY (SELECT) into a function; or state in the header that the pushdown paths are not under test.

### P2F-05 (low, tests): weak or vacuous assertions

- t/160: the "nothing changed" checks run after statements that were all wrapped in `BEGIN; ...; ROLLBACK`, so they would pass even if a forged write had run.
  The refusals themselves are asserted by the exit code, which is fine. The "no token" message check accepts `FATAL`.
  The insert-fragment kind has no control run with the proof, so its refusal cannot be tied to the token.
- t/158 C: the read after the old primary's write is accepted when stderr matches `ERROR` (any error).
- t/158 A: both outcomes (retryable error or the right answer) pass; that is the intended property, but if the fault gate never produced a
  change in flight the test would still pass. The `reached` file is asserted, which covers most of it.
- t/157 gates a move at `frag_before_probe` only; the re-checks after the DECLARE are not what it separates (M8, M9 survive).
Suggested fix: compare counts without the enclosing rollback (run the forged statement outside a transaction block in the control tests that use a
rolled-back table), and tighten the regexes.

### P2F-06 (note): README sentence on ties

"Rows that compare equal come out in the same order whichever node runs the statement, and in the same order when the statement is repeated."
Held in every case I ran (t/193, t/190, t/162). The order of equal rows inside one group is the group's own (top-N heapsort with a limit, a parallel
plan in the group), so the second half is a statement about unchanged data and a deterministic group plan. Suggest adding "among the groups".

### P2F-07 (note): survivors of the claim "a bucket moves between the plan and the run" (M8, M9, M11)

- M8 (the own part's check after the DECLARE, `st->after_check`) and M9 (`kingres_shard_probe_read_after`) survive all of t/157, t/125, t/139, t/158,
  t/153, t/154, t/124, t/126, t/136, t/151, t/152. Earlier-phase tests that I did not run (phase 4) may kill them. These checks are code of earlier steps, but
  the round states that it tests the property.
- M11 (the column list of `copy_routed.c`, used by `create_distributed_table` of a table with rows) is not hit by t/159 with hostile column names
  (the tables are distributed empty).
Suggested fix: a move that commits between the first probe and the DECLARE (gate `frag_after_probe_before`) with `move_bucket`, and a populated table with
hostile column names passed through `create_distributed_table`.

## Mutant table

One-line mutants, one at a time, clean build, on PostgreSQL 18 (private install); M3, M6, M15 rerun on the private install with the same verdict.

| id | mutant | tests run | verdict |
|---|---|---|---|
| M1b | conversion for every SQLSTATE (`sqlstate == 0` as the filter) | t/162 | SURVIVED (P2F-03) |
| M2 | only 42883, no 42704 | t/162 | SURVIVED (P2F-03) |
| M3 | merge streams sorted in descending group id | t/162 | killed (3 assertions) |
| M4 | own part has group id -1 | t/162 | killed (4) |
| M5 | no sort of the streams | t/162 | killed (4) |
| M6 | crash fix removed (`ship_used_ext = NIL` line) | t/157 | killed (a group crashes) |
| M7 | restore of the old list removed | t/157, t/151, t/162 | survived, equivalent today |
| M8 / M8b | `after_check` of the own part removed | t/157, t/125; and t/139, t/158, t/153, t/154, t/124, t/126, t/136, t/151, t/152 | SURVIVED (P2F-07) |
| M9 / M9b | `probe_read_after` removed | the same | SURVIVED (P2F-07) |
| M10 | unquoted column list in the router batch (`batch_sender.c`) | t/159 | killed (setup dies) |
| M11 | unquoted column list in `copy_routed.c` | t/159 | SURVIVED (P2F-07) |
| M12 | unquoted qualified name in the batch sender | t/159 | killed |
| M15b | no KS001 for a failed group with a standby (`failover_possible` off) | t/158 | killed (4) |
| M16 | parameter limit sends count without the offset | t/157, t/153, t/162 | SURVIVED; killed by t/155 (18) and t/172 (6) |

(M1 and M15 as first written did not compile under -Werror; M1b and M15b are the corrected ones.)

## README check

Of the 26 statements of own t/192 (default settings with partial aggregation, ordered merge and join rows on), the plans agree with the README's
list of shipped and not-shipped shapes: shipped are the FULL JOIN of two distributed tables on the key, the closed LEFT JOIN, the sketch partial
aggregate; not shipped (answered by the 2a scan, plan `Kingres Distributed Scan`) are FULL JOIN with a reference table, FULL JOIN without the key
equality, GROUPING SETS and ROLLUP, `json_agg`/`jsonb_agg` when not closed, `count(DISTINCT)`, `ORDER BY` inside an aggregate, sub-selects, EXISTS,
NOT IN, a CTE, a view, a local table, two colocation groups, `= ANY(ARRAY(subselect))`, a window not by the key, UNION and LATERAL. My case
"json_agg closed" was not shipped because I put an `ORDER BY` inside the aggregate, which the README also lists as not shipped; a closed
`json_agg` without it was not run. Answers of every statement matched the single node except the view (a defect of my test). The README
sentence about a node that does not have the extension is the one that does not hold (P2F-01).

## What I did not examine

- A pushed statement with the group lacking the extension while the group is a standby or is failing over (only the demoted primary of t/160).
- The join-rows fragments with the conversion (they carry no `ext=`, so they never convert).
- Closed `json_agg` without `ORDER BY` in the aggregate; the other aggregates without a combine function.
- t/157 section by section beyond the mutants; t/159's 2450 assertions beyond the three mutants; t/161 beyond P2F-04.
- Phase 4 tests (bucket move) against M8 and M9; the control plane (not run, as instructed); the meson build.
- Groups removed (not only added); concurrency of two merges on one node.
- The `KS_ALL_PATHS=1` profile of the round's tests.

## Process note

My first runs installed the extension into the shared PostgreSQL installations (`ext-majors.sh install`); on the coordinator's instruction I stopped,
made private copies, and reran every result above against them. The shared installs were left for the coordinator to reinstall. Earlier in the
work I also copied helper scripts named `r13-*.sh` into my home directory, which may have replaced files of the same names there. The worktree and
the build trees are removed at the end.
