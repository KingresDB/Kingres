<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026 RooyeKhat Media Co -->

# Review of the fixes for the phase 2b step 13 review (kingres_shard, commit 3fea547ec5)

Reviewed: commit 3fea547ec5 on kingres-18, against the review
`kingres/docs/reviews/2026-11-20-kingres-shard-phase2b-step-13.md` (findings P2F-01 to P2F-07) and the design
section "The fixes for the review of step 13" in `kingres/docs/design/shard-phase2b.md`. Nothing was changed,
committed or pushed.

## 1. Verdict

All seven earlier findings are fixed in the sense the review asked for. The new code decides a missing extension by the
catalog of the node, as the review suggested, and I found no defect in it. New findings: none critical, high or medium;
two low (neither reproduced, both edge cases of the format or the design). Every new test I mutated fails without its fix,
except one mutant that is equivalent.

## 2. What was run, and what was only read

Run, on private copies of both PostgreSQL installs, a clean build with warnings as errors on PostgreSQL 18.7 and 19rc2,
the extension built from `git archive kingres-18`:

| Run | PostgreSQL 18.7 | PostgreSQL 19rc2 |
|---|---|---|
| clean build with -Werror, install | ok | ok |
| t/173, t/174, t/175, t/161 (4 files, 333 assertions) | pass | pass |
| t/158, t/160, t/162, t/157 (4 files, 461 assertions) | pass | pass |
| mutant S1: the KS001 for a missing extension never raised (`if (!found)` made false) | not run | killed: t/173 (48 assertions fail), t/162 (12) |
| mutant S2: the check of this node's own map after the DECLARE removed (`st->after_check`) | not run | killed: t/174 B (4 assertions) |
| mutant S3: the second probe of the groups (`kingres_shard_probe_read_after`) removed | not run | killed: t/174 A (4 assertions) |
| mutant S4: the name match in `probe_read_before` loosened from "name:" to a prefix | not run | survives; equivalent (section 5) |
| mutant S6: the column list of `copy_routed.c` unquoted | not run | killed: t/175 aborts at the distribution |

Only read: the README changes (both files), the meson test list, the diffs of t/158, t/160, t/161 and t/162, the part of
`forward_auth.c` that the t/160 refusals reach.

## 3. The earlier findings

| Id | Earlier severity | Verdict | Evidence |
|---|---|---|---|
| P2F-01 a node that never had the extension is not turned into KS001 | medium | Fixed | `placement_check.c`: the probe before the DECLARE returns the names and versions the node has (`pg_extension`), and `kingres_shard_probe_read_before` raises KS001 for a name that is missing. t/173 A (no schema), B (dropped, schema left; dropped, schema dropped too) pass on both majors; mutant S1 killed. |
| P2F-02 the conversion by SQLSTATE took any undefined function or object for a missing extension | low | Fixed | `declare_missing_extension` is removed; no SQLSTATE of a DECLARE is converted. t/173 D (a column renamed behind the statement) shows 42703 coming through, not KS001, for both kinds of fragment. |
| P2F-03 the "other error is not converted" test never reached the conversion | medium (test) | Fixed | t/173 D makes the DECLARE itself fail on a group (undefined column) while the marker names an extension, and asserts the original error. There is no conversion left to mutate, so the old mutants M1b and M2 are gone; S1 shows that the test set fails when the decision is removed. |
| P2F-04 t/161 did not run the pushdown paths under the ledger | medium (test, claim) | Fixed | t/161 A2 runs the statements inside functions (`RETURN QUERY`), so the partial, ordered-merge and closed fragments are shipped; it checks the log of a group for the marker (at least two sightings each) and one ledger entry per row. Passes on both majors. The header says what A and A2 each test. Not mutated by me (a ledger mutant needs ledger code, outside this commit). |
| P2F-05 weak or vacuous assertions (t/160, t/158) | low (tests) | Fixed | t/160: the forged write fragments now change what the checks see (`w = w + 1000`, `lbl = 'changed'`, the whole insert-select), refusals run outside a transaction block, the pattern no longer accepts `FATAL`. t/158: `ERROR` removed from the pattern. The refusal in these tests is a FATAL at connection (`forward_auth.c`), so the strengthening is belt and braces; both tests pass. |
| P2F-06 README sentence on ties | note | Fixed | README now says "among the groups" and "on unchanged data", and that the order inside a group is the group's own. |
| P2F-07 survivors M8, M9, M11 | note | Fixed | t/174 (A: a remote group's map after the DECLARE; B: this node's own map at the new fault point `frag_before_own_check`) kills S2 and S3; t/175 (a populated table with hostile column names) kills S6. |

## 4. New findings

### S13F-01 (low) An extension installed in another schema on a node is no longer reported as KS001

Where: `contrib/kingres_shard/src/placement_check.c`, `kingres_shard_probe_read_before` (the block "An extension that the
statement uses and the node does not have"); `fragment_scan.c` (removal of `declare_missing_extension`).

Scenario: a node has the extension installed, at the same version, in a schema other than the sender's (a relocatable
extension created with another schema). The probe finds the name and the version, so it passes; the group's parse then fails with
`3F000` or `42883`. Before the fix the SQLSTATE conversion turned the 42883 into KS001 (with a hint that did not fit);
now the raw error comes through, which is the stated aim ("no other failure of a DECLARE is taken for a missing
extension"). It is a consequence of the design choice, not a coding defect; the user gets the group's own message and nothing
says that the schema differs. Not reproduced (no node with the extension in another schema was built); decided by reading the
code. A line in the README would be enough.

### S13F-02 (low) The extension list is split on ':' and ',' with no guard on the names

Where: `placement_check.c`, `kingres_shard_probe_before_sql` and `kingres_shard_probe_read_before` (two copies of the same
split).

Scenario: an extension whose name contains a colon. The marker parser (`pushdown.c`, `kingres_shard_parse_query_marker`) accepts
both characters in the text, so the split is ambiguous: the name is cut at the first colon, the probe asks for a shorter name,
and a node that has the extension is reported as lacking it. Names with a colon are legal but not used, and a comma cannot
be listed in `shippable_extensions` (a comma list). Not reproduced. The duplicated parse also means a later change of the
format must touch both places.

## 5. Notes on the mutants and other checks

* S4 (prefix match) is equivalent: the probe's `IN (...)` list asks for exactly the names in the marker, so the answer holds
  only those names and a prefix cannot match another extension.
* The review's M7 (restore of the old list in `kingres_shard_extension_aggregate_ok`) is code of the earlier fix and outside this commit.
* Column positions: `kingres_shard_probe_read_before` reads the version at `ntables + nid` and the extension list at
  `ncols - 1`, for both values of `KS_PROBE_IDENTITY`; `probe_shape` checks the column count with the extra column. The
  batch sender passes NULL and gets the old shape. `dist_scan.c` and `fragment_scan.c` both clear `KsFragStart` with `memset`
  before use, so `st.ext` is NULL for the 2a scan.
* Error path: the new KS001 is raised in the same place and with the same cleanup as the version-mismatch error (the pending
  results of the other groups are drained by the connection callbacks); `PQclear(res)` is called before `ereport`, after the host and
  the names were copied.
* The fault point `frag_before_own_check` is built with `psprintf` only when a fault specification is set; no cost in production.
* An extension dropped on a group between the probe and the DECLARE gives the parser's own error once (documented; a repeat gives KS001).
* A prepared statement keeps its extension list in the plan, so a node that loses the extension later gives KS001 on the next
  run; I did not run a prepared statement across a drop.
