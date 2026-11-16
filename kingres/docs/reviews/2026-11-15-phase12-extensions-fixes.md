<!-- SPDX-License-Identifier: AGPL-3.0-only -->
# Adversarial review of the fixes: contrib/kingres_sketch and
# contrib/kingres_timepart (phase 12)

Reviewer: independent (not the author of the code or of the first
review). Reviewed state: `HEAD` of the two directories, which carries
commit e6ee42f265 (`kingres_sketch`, fixes for K12-01 to K12-06) and
commit 873cba63ce (`kingres_timepart`, fixes for K12-07 to K12-17), on
top of the code the first review read at commit 5fc2a70fad. Exported
with `git archive` into a private work area; the working tree, which
other people were editing, was not used for any result. Nothing in the
repository was changed except this file.

## Scope and method

Read completely: the diffs of the two fix commits against
5fc2a70fad, the current `src/*.c`, `*.sql` and `README.md` of both
extensions, and the first review
(`kingres/docs/reviews/2026-11-15-phase12-extensions.md`). Compared
behaviour against both READMEs and against
`kingres/docs/design/analytics-phase12-queries.md` and
`analytics-phase12-storage.md`.

Built and run, on PostgreSQL 18.7 and PostgreSQL 19rc2 (read-only
installations copied into a private work area so nothing was installed
into the shared read-only installations): `kingres/dev/ext.sh sync
clean build install regress tap`, both extensions, both majors, with
`EXT_WERROR=1`. All four runs built without warning and passed every
test:

| Suite | 18.7 | 19rc2 |
|---|---|---|
| `kingres_sketch` pg_regress (7 files) | pass | pass |
| `kingres_sketch` TAP (7 files, 339 subtests) | pass | pass |
| `kingres_timepart` pg_regress (10 files) | pass | pass |
| `kingres_timepart` TAP (3 files, 105 subtests) | pass | pass |

These four runs are the baseline referred to below as "baseline run
1-4" (sketch 18, sketch 19, timepart 18, timepart 19, in that order).

Own experiments (on 18.7 unless stated), run by hand through `psql`
against a disposable server of my own (port in the 59100-59199 range,
`fsync=off`, stopped at the end):

| Experiment | What it covers |
|---|---|
| Hash equality and refusal, about 35 types, domains, collations, oid-bearing containers | K12-01, re-verification and new angles |
| Parallel vs. serial `hll_add_agg` on enum, `int[]`, `int4range` columns, 200000 rows, 4 workers | K12-01 under real parallelism (the first review's fuzzer could not reach this) |
| `tdigest_quantile`/`tdigest_rank` at the double limits, denormals, ties | K12-02 |
| NULL precision/capacity/compression in both row orders, four aggregates; `approx_percentile` NULL-fraction rows in both orders | K12-03 |
| `topn` item folding: `bpchar`, `numeric`, domains over them, `NaN`/`Infinity`/`-0`, empty string, embedded control bytes, multi-byte characters at the 1024-byte boundary, `numeric(5,-2)` | K12-04 |
| Negative-zero injection into a stored `tdigest` via `set_byte` | K12-05 |
| `topn_union` of two 100000-item, 1020-byte-item values | K12-06 (functional only, see below) |
| Malformed binary values: 8 byte-level mutations of a real `tdigest`, 5 of a real `hll`, 5 of a real `topn`, fed through `pg_input_is_valid` | crash/over-acceptance under attack, beyond the first review's fuzzer |
| Advisory-lock replacement: listing of `kingres` functions and ACL of `timepart_lock` | K12-07 |
| `search_path` hijack of `timepart_run` with a look-alike `unnest` in a schema listed first | K12-08 |
| Deputy-privilege attack: a `SECURITY DEFINER` function owned by an unprivileged role, called from a superuser session, calling `timepart_set_policy` on a table it does not own | K12-09 |
| An `IMMUTABLE` index-expression function that sleeps, with a short per-action `statement_timeout` | K12-10 |
| `Pacific/Apia` 2011-12-30 (the skipped calendar day) through `create_time_partitions` | K12-11 |
| A hand-made, off-grid partition with a row, through `timepart_run` | K12-12 |
| A detached-partition name reused by an unrelated table of another role | K12-13 |
| A table with a foreign key referencing a partition, through two runs | K12-15 |
| Direct `INSERT`/`UPDATE` of `kingres.timepart_policy` by a non-owner and by the owner, bypassing `timepart_set_policy` | the row-level-security replacement for K12-09 |
| A table and partition name built to look like a SQL injection (`tab'); drop table app.fkp; --`), through `timepart_set_policy` and `timepart_run` | the `timepart_hostile` class of attack, independently |
| A sub-partitioned child (list-partitioned under a range-partitioned parent), through creation and retention | not in the first review's "not covered" list |
| `pg_dump` of a database with `kingres_timepart` installed, run as a non-superuser role with and without `BYPASSRLS`/`pg_read_all_data` | new: row-level security and backup roles |

Not run by me: a fresh memory-safety fuzzer under AddressSanitizer
(the first review's covered the validators and C entry points
exhaustively for the unchanged code; I re-ran the shipped TAP fuzz
suite instead, which is part of the baseline above), Windows, a
big-endian machine, logical replication with a subscriber, the
interplay with `kingres_shard` or `kingres_columnar`,
`pg_upgrade` itself.

## Summary

| Severity | Count |
|---|---|
| Critical | 0 |
| High | 1 |
| Medium | 0 |
| Low | 1 |
| Note | 1 |

Every finding of the first review that was marked for a fix in this
round was fixed, and I found no case of a fix breaking something that
worked (see the table below and "What held under attack"). The new
findings are operational, not correctness bugs: one about backup roles
under the new row-level security, one about a documentation gap in the
`topn` memory figure, one note about the `tdigest` README's remaining
claim about platform determinism.

## Fate of the first review's findings

| ID | Fate | Evidence |
|---|---|---|
| K12-01 | fixed | enum hashed by label (`red` after a rename gives a new hash, as expected); `oid`, `regclass`, `oidvector` and containers of them refused, including inside a composite type and an array; parallel and serial `hll_add_agg` give identical bytes on an enum, `int[]` and `int4range` column (200000 rows, 4 workers) |
| K12-02 | fixed | `tdigest_quantile`/`tdigest_rank` on a digest of `-1.7e308` and `1.7e308` give `-1.7e+308`, `0`, `1.7e+308` at q = 0, 0.5, 1 (no `Infinity`/`NaN`); denormal values (`4.9e-324`) handled without error |
| K12-03 | fixed | `hll_precision(hll_add_agg(x,p))` errors `22004` in both row orders, and so do `topn_add_agg`, `tdigest_agg`, `approx_count_distinct`; `approx_percentile`'s NULL-fraction-row-ignored rule holds in both orders |
| K12-04 | fixed | `'a'::char(3)` and `'a'::char(5)` fold to one item; `1.5` and `1.50::numeric` fold to one item; a nondeterministic-collation string is refused `0A000` |
| K12-05 | fixed | a `tdigest` value with the bit pattern of `-0.0` forced into its minimum is refused on cast with "minimum or maximum is not valid" |
| K12-06 | fixed (functional check only) | a union of two 100000-item, 1020-byte topn values completed; the exact 4.4x figure was not independently remeasured (RSS measurement infrastructure was not set up in this round) |
| K12-07 | fixed | no `kingres` function or operator names an advisory lock any more; `kingres.timepart_lock` is `REVOKE ALL FROM PUBLIC` and an ordinary role gets "permission denied" reading or writing it |
| K12-08 | fixed | `timepart_run` with `search_path = hijack, public, pg_catalog` (a schema holding a look-alike `unnest` that raises a warning if called) ran to completion without the look-alike ever firing |
| K12-09 | fixed | a `SECURITY DEFINER` function owned by an unprivileged role, called from a superuser session, trying to call `timepart_set_policy` on a table it does not own, is refused "permission denied for schema app"; the policy is unchanged |
| K12-10 | fixed | a 2-second `IMMUTABLE` index function under a 500ms `statement_timeout` on `timepart_run_action` gives a `failed` action with detail `statement_timeout: ...` and the run ends `partial`, not a hang or a crash |
| K12-11 | fixed | `Pacific/Apia` 2011-12-30 (the skipped calendar day) creates 6 partitions with no gap, no overlap and no duplicate name across the skip |
| K12-12 | fixed | a hand-made, off-grid partition holding a row survives a run at a time that would have retired it; it is reported `misaligned`, `skipped`, "left alone", and `timepart_check` lists it |
| K12-13 | fixed | `timepart_partitions` no longer lists a detached-name partition that the caller cannot read (the name reused by another role's unrelated table is invisible to the policy's owner) |
| K12-14 | fixed (docs) | the README's lock table and prose now describe the `SHARE ROW EXCLUSIVE` lock on referenced tables |
| K12-15 | fixed | a foreign key referencing a partition makes its run `partial` with `retire_blocked` named in `timepart_check`; the referenced partition and its row are kept |
| K12-16 | fixed (docs) | the README documents that `DROP EXTENSION` leaves the schema `kingres` |
| K12-17 | fixed (docs) | the README documents the `search_path` caveat for a role named `kingres` |

## New findings

### K12R-01 (high) A non-superuser backup role without BYPASSRLS fails
### the whole database dump, not just this table

Where: `kingres_timepart--0.1.sql`, the row-level-security policies on
`kingres.timepart_policy` (`FOR ALL TO PUBLIC USING
(kingres.timepart__owns(...))`); README "Security", "Policy and
history tables" (documents the row-level-security behaviour for
queries, not for `pg_dump`); README "Install" (grants `EXECUTE` to
`app_owner`, says nothing about the role that runs backups).

What happens: the fix for K12-09 replaced the `SECURITY DEFINER`
functions with ordinary ones and protects
`kingres.timepart_policy` with row-level security judged by
`current_user`. `pg_dump` must read every row of an extension's
configuration table regardless of ownership, which it does by asking
PostgreSQL to turn row-level security off for the dump
(`SET row_security = off`, done internally). PostgreSQL grants that
only to the table's owner, to a role with the `BYPASSRLS` attribute,
or to a superuser. A role that can read every table of the database
(for example one granted the built-in role `pg_read_all_data`, a
common and recommended way to give a backup account read access
without superuser) does **not** get `BYPASSRLS` and is not the
table's owner (the table is owned by the extension, here superuser
`kingres`). `pg_dump` run by such a role does not silently skip the
table: it aborts the entire dump with a nonzero exit code, so a
database that has `kingres_timepart` installed cannot be backed up at
all by a conventional least-privilege backup role, even though that
role can read every other table in the database. Before the K12-09
fix the table was reachable from any `SECURITY DEFINER` context and
had no row-level security, so this problem did not exist; the fix for
one finding introduced an operational regression that affects every
installation, not an attacker-chosen one.

Reproduction (PostgreSQL 18.7, database `t2` with `kingres_timepart`
installed; role `vown` is an ordinary login role, not the table
owner, not a superuser):

    GRANT pg_read_all_data TO vown;
    -- as a shell user that can authenticate as vown
    pg_dump -h ... -U vown -d t2 -f /tmp/full.sql

Observed:

    pg_dump: error: query failed: ERROR:  query would be affected by
      row-level security policy for table "timepart_policy"
    pg_dump: detail: Query was: COPY kingres.timepart_policy (...) TO
      stdout;

exit code 1, no dump file produced. The same happens with `-t
kingres.timepart_policy` alone. Expected: either the dump should
succeed (because `pg_read_all_data` is meant to let a role read
everything), or the requirement that a backup role needs `BYPASSRLS`
or superuser should be stated next to the "Install" grants, so an
operator sets it up before the first backup rather than discovering it
when a scheduled dump starts failing.

Suggested direction: document, next to the `GRANT EXECUTE` block in
"Install", that the role used for `pg_dump`/`pg_basebackup`-style
logical backups of a database that has this extension must be a
superuser, the owner of `kingres.timepart_policy`, or hold
`BYPASSRLS`; or add that grant to the extension's own install
instructions as a named step. Add a TAP test that runs `pg_dump` as a
role with `pg_read_all_data` and checks the exit code, alongside the
existing `003_dump_restore.pl` (which runs dump and restore as the
superuser and would not have caught this).

### K12R-02 (low) The `topn` memory figure in the README is still the
### pre-fix number in one place

Where: `contrib/kingres_sketch/README.md`, the paragraph right after
the capacity/limits table (the sentence beginning "`topn_union` of two
such values was 436 MB, 4.4 times the value") is new and correct, but
the "Limits" table a few lines above it and the "Untrusted input and
limits" table still say only "largest value: ... topn about 104 MB"
without a working-memory figure, and a reader who stops at the table
(as the first review's K12-06 finding did) has no indication that a
call needs several times that much. The commit message of the fix
states the new multiplier (4.4x) but I could not independently
remeasure it in this round (no RSS-sampling harness was set up); the
functional test (a union of two 100000-item, 1020-byte topn values)
completed without error, which is consistent with the claim but does
not confirm the exact figure.

Expected: the limits table itself (or a footnote on it) should carry
the working-memory multiplier, not only the paragraph below it, since
an operator who right-sizes a container from the "largest value" row
alone will now under-provision by a factor the commit itself reports
as about 4.4, down from about 9-10 but still not 1. Suggested
direction: add a "working memory" column or footnote to the limits
table pointing at the explanation below it, and re-measure and record
the exact multiplier the way K12-06 did, as part of closing this
finding rather than leaving the number only in the commit message.

### K12R-03 (note) The t-digest union's platform dependency is
### unchanged and still only partly covered by `self_test()`

Where: `contrib/kingres_sketch/README.md`, "tdigest" ("the decisions
of the merging pass use the C library's `asin`, so two platforms can
differ in the last digits of a centroid for the same input");
`kingres_sketch_self_test` (`src/kingres_sketch.c`) does not include a
`tdigest` value, only `hll`.

What happens: this is pre-existing behaviour, not touched by the fix
round, and the README already discloses it; I list it because the
first review's `self_test()` coverage table does not mention tdigest
at all, and a node pair that differs only in `libm`'s `asin` rounding
(a realistic cross-platform or cross-glibc-version case for a stored,
unioned rollup) would not be caught by `self_test()` before the two
nodes exchange partially-merged digests that are bit-for-bit
different, only statistically close. This is not a correctness bug
(the README already says "not order independent... can differ in the
last digits"); it is a gap in the one line meant to catch exactly this
kind of mismatch between nodes. Suggested direction: add a `tdigest`
value (built once, deterministically, from a fixed small input) to
`self_test()`'s output, so that an `asin` or floating-point
discrepancy between nodes shows up the same way an `hll` or hash
discrepancy would.

## What held under attack

- Enum, array, range, multirange, composite/record and domain hashing
  (K12-01): about 35 equal-value pairs across types, nested
  combinations, NULL elements inside containers, multidimensional
  arrays with nonstandard lower bounds, and `oid`/`regclass`/
  `oidvector` refusal including inside a composite type and an array:
  all behaved as the README now describes. Parallel and serial
  aggregation gave byte-identical results on enum, array and range
  columns.
- Malformed binary sketch values: 18 byte-level mutations of real
  `hll`, `topn` and `tdigest` values, fed through `pg_input_is_valid`,
  were all rejected cleanly; none crashed the backend and none was
  wrongly accepted.
- The `SECURITY INVOKER`/row-level-security replacement for the
  `SECURITY DEFINER` identity bug (K12-09): a non-owner cannot insert a
  policy row for a table it has no rights on (refused at the catalog
  lookup, "permission denied for schema ..."), cannot update another
  role's existing policy row (the `UPDATE` matches zero rows under
  row-level security), and the owner's own direct write still goes
  through `timepart__validate_policy` (a retention shorter than
  `partition_every` is refused even when written directly, bypassing
  `timepart_set_policy`) and still has `parent_oid`, `created_by` and
  `table_owner` pinned by the trigger.
- A hostile table and partition name
  (`"tab'); drop table app.fkp; --"`) went through
  `timepart_set_policy`, `timepart_run`, `create_time_partitions` and
  `timepart_part_name`'s 63-byte truncation without any sign of
  injection; the unrelated table named in the payload was untouched.
- A sub-partitioned child (a `timepart`-managed partition later turned
  into a two-level partitioned table by hand, with a row in a
  grandchild): retention dropped it as a whole in one `DROP TABLE`,
  which PostgreSQL itself cascades to the grandchildren; this matches
  "retention drops whole partitions" and is not a defect of the
  extension.
- `timepart_run_action`'s per-action `statement_timeout` handling
  (K12-10's documented driver pattern): a cancelled action is recorded
  as `failed` with a `statement_timeout` detail, the run is `partial`,
  and the claim on the table's lock row is released normally on the
  next call.

## Not reproduced

- The exact 4.4x memory multiplier of the K12-06 fix (functional
  behaviour confirmed, the number itself not independently
  remeasured; see K12R-02).
- A real cross-platform `asin` discrepancy in a unioned `tdigest`
  (K12R-03 is argued from the README's own disclosure and the gap in
  `self_test()`, not from an observed mismatch; only one platform was
  available).

## Not covered

- A fresh memory-safety fuzz run under AddressSanitizer/UBSan of the
  changed C code (`sketch_hash.c` is new); the baseline TAP fuzz suite
  passed, but it is not the same depth of coverage the first review's
  standalone fuzzer gave the pre-fix code.
- Windows, meson in-tree builds, a big-endian machine, valgrind.
- `pg_upgrade` itself; only dump and restore across 18.7 and 19rc2
  were covered, and only implicitly through the existing TAP suite
  (`007_portable_sketches.pl`, `003_dump_restore.pl`), not with a new
  scenario of my own in this round.
- Logical replication with a real subscriber, the interplay with
  `kingres_shard` or `kingres_columnar`, hash joins/indexes on a `hll`
  expression.
- A restore into a database with another encoding or default
  collation.
- Load/stress timing of the lock-retry and lock-timeout settings under
  real contention (the shipped `001_locks.pl`/`002_interrupt.pl` TAP
  tests passed as part of the baseline; no new timing experiment of my
  own was added given the shared, loaded machine).

## Baseline runs

1. `kingres_sketch`, PostgreSQL 18.7: 7 pg_regress files, 7 TAP files
   (339 subtests), pass.
2. `kingres_sketch`, PostgreSQL 19rc2: same, pass.
3. `kingres_timepart`, PostgreSQL 18.7: 10 pg_regress files, 3 TAP
   files (105 subtests), pass.
4. `kingres_timepart`, PostgreSQL 19rc2: same, pass.
