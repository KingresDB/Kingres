<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->

# Independent review of phase 2b of kingres_shard, steps 3 and 4, and of the fixes for two reviews

## Scope and method

Reviewed: the round that adds the batch sender (`src/batch_sender.c`), the row router and the routed
`COPY FROM` (`src/row_router.c`), the one-shot mark of the local part (`src/fanout.c`, `src/planner.c`),
the insert-fragment acceptance rule on the receiving group (`src/planner.c`, `is_insert_fragment`), the
subtransaction callbacks and the cancel of a batch in flight (`src/node_conn.c`), the placement probes
(`src/placement_check.c`) and the fixes P2B-01 to P2B-09 and S14-01 to S14-08. The tree under review is the
committed tree with the round (version 0.6, which changes no behaviour); the diff against the tree before the
round was read file by file for the C sources (`fanout.c`, `planner.c`, `node_conn.c`, `utility.c`,
`placement_check.c`, `catalog.c`, `dist_scan.c`, `kingres_shard.c`) and `row_router.c` and `batch_sender.c`
were read completely. Design: section 11 completely, the notes "Steps 3 and 4 ... and the fixes for two reviews",
the rows of section 17, and the two earlier reports.

Method: I built the extension out of tree against assertion builds of PostgreSQL 18.7 and 19rc2, ran the whole suite once per major, and wrote my
own tests against the tree: a seeded generator against an ordinary twin node (reads of every group directly),
shape tests (self insert, volatile keys, CTE, functions, savepoints, parameters, joins and aggregates as
sources), fault runs on a live three-group cluster (killed group, killed origin backend, cancel, concurrent
overlapping statements) and measurements of memory. For the test-quality question I made one-line mutants
of the sources and ran the round's own tests against them.

Where I only read the code I say so. Every number below is from a run unless it says "read".

## What was run

| What | PostgreSQL 19rc2 | PostgreSQL 18.7 |
|---|---|---|
| Build, clean, `-Werror` (`EXT_WERROR=1`) | no warning | no warning |
| `pg_regress` (14 tests, including `bulk_plans` and `upgrade`) | 14 of 14 | 14 of 14 |
| TAP suite, 119 files | 119 files, 15145 tests, all pass, 4009 s | 119 files, 14516 tests, all pass, 4093 s |
| `t/046_two_majors.pl` | skipped by the suite (needs the other major named); run alone against the 18 install: 30 of 30 | ran in the suite against the 19 install: pass |
| Generator against the twin, types without a type modifier on the key | seeds 11, 12, 13: 3 x 515 statements, 0 differences | seeds 21, 22: 2 x 515 statements, 0 differences |
| Generator, seed 7, with `timestamp(0)`, `timestamptz(0)`, `numeric(8,2)` keys | 315 statements; P2C-01 | - |
| Shape test (`INSERT` shapes against the twin, 58 assertions) | 56 pass, 2 expected refusals (P2C-08) | same |
| Sources with joins, aggregates, windows, set operations, reference tables (12 statements, 37 assertions) | pass | not run |
| Mutants of my own | about 25 one-line mutants, about 100 runs of the round's test files against them (table below) | not run |

Version tests outside `src/pg_compat.h`: none (`grep` for `PG_VERSION_NUM` and `PG_MAJORVERSION` in `src/`
finds them only in `pg_compat.h`).

### The generator (question 1)

About 2900 statements in total (315 + 1545 on 19, 1030 on 18; the 15 primary-key and identity statements of
each run are included). Random origin among the three nodes. Shapes: multi-row `VALUES` with `DEFAULT`
keyword, column lists in another order, and `RETURNING`; `INSERT ... SELECT` from `generate_series` with a
constant or a `CASE` key; `INSERT ... SELECT` from a `VALUES` source typed `text`, `varchar(20)` or `char(20)`
and cast to the key type; `COPY FROM STDIN` through `psql` in text and in CSV (NULLs, backslash, tab, newline,
quotes); `ON CONFLICT DO NOTHING` (no target, inference column, `ON CONSTRAINT`) on a primary-key table with
`RETURNING` and with in-statement duplicates; `GENERATED ALWAYS` and `BY DEFAULT` identity with `OVERRIDING
SYSTEM VALUE`, `serial`; defaults that are constants and a default that is an expression; a stored generated
column; a `CHECK` that one row of the statement violates (5 percent of the statements); `insert_batch_rows` of
1, 2, 3, 7 and 1000 and `insert_batch_bytes` of 16 kB; random `TimeZone` and `DateStyle` per statement;
NULL keys with probability 0, 0.1 and 0.5; 1, 2, 3, 5, 12, 40 and 150 rows.

Key types (26): `int2`, `int4`, `int8`, `numeric`, `numeric(8,2)`, `float8` (NaN, infinities, -0), `text`
(commas, braces, `NULL`, quotes, backslash, tab, newline, UTF-8), `varchar(8)`, `char(5)` (trailing spaces),
`uuid` (upper and lower case), `date` (infinity), `timestamp`, `timestamp(0)`, `timestamptz`, `timestamptz(0)`,
`time`, `interval` (`1 mon` and `30 days` hash alike), `bool`, `bytea`, an enum, a domain over `int` and one over
`text` (both with `CHECK`), `int[]`, `text[]`, `jsonb`, `inet`.

After every statement: every group is read directly (routing off), every row must be on the group that the
bucket map gives for `bucket_for(table, key)`, and the sorted rows of the three groups must equal the twin's
rows; the outcome (success or error) of the statement must equal the twin's, and `RETURNING` output must equal
the twin's. Tables are truncated every 40 statements. Not in the generator: triggers (a distributed table
refuses them by design), row-level security (refused), virtual generated columns, encodings other than UTF-8.

Shape counts of the five clean runs (2575 statements): 814 multi-row `VALUES`, 272 `VALUES` with a column
list in another order, 278 with `RETURNING`, 285 `INSERT ... SELECT` from `generate_series`, 275 from a typed
source, 276 `COPY` text, 300 `COPY` CSV, 75 primary-key and identity shapes. Both sides failed in 80 of them
(the injected `CHECK`, unique violations, source casts that fail the same way on both).

## The fate of P2B-01 to P2B-09

- **P2B-01 held, with a gap in the tests (P2C-05).** The mark is a file-static boolean and a pointer set by
  `local_part_arm` right before `SPI_execute_with_args` in both callers (`fanout.c` local part, `kingres_shard_spi_local_part`
  for the router's local batch), consumed at the entry of the planner hook before anything else runs
  (`planner.c`, `kingres_shard_planner_hook`), and cleared by `PG_FINALLY` in both callers, so it cannot stay armed
  after an error. Nothing about it is in the statement text or in a GUC, so SQL code cannot arm it. The nested
  statements of the review are routed (t/118 passes on both majors). I also ran the replay variant of the attack:
  a `CHECK` function reads `PG_EXCEPTION_CONTEXT` (which still contains the exact text of the local part,
  `/* kingres:write-fragment */ UPDATE ...`), and executes that text itself inside the statement: on the
  shipped code it affects 40 rows (routed); with the mark not cleared at consumption (one-line mutant) it
  affects 14 (the local part only). Both the shipped behaviour and the difference are real, and no test of the
  round notices the mutant. The local batch of the router uses the same two functions; I found no difference.
  Not reproduced: a `CHECK` or `DEFAULT` that names a user function cannot be given to `create_distributed_table`
  on my cluster (the groups receive the call without its schema and answer `function f(integer) does not
  exist`), so for the nested-planning cases I rely on t/118, which adds the constraint after distribution.
- **P2B-02 held (run).** `WHERE name COLLATE "C" = 'a'` returns 20, `COLLATE "POSIX" < 'b'` returns 60, as on one node.
- **P2B-03 held (run).** `UNION`, `UNION ALL` and `EXCEPT` with an arm that cannot match are answered.
- **P2B-04 held (t/118 passes on both majors; code read).** The pruning terms live in a context deleted at once and the
  statement text of a declared cursor is freed. I did not measure it beyond t/118.
- **P2B-05 as stated.** The three test points that the round says are open are open; I did not re-check the
  others beyond the suite.
- **P2B-06 held (code read).** `kingres_shard_placement_hash_now` now scans `bucket_map` through its primary key
  for the one colocation group (`catalog.c`). Not measured.
- **P2B-07 held (run).** `SELECT ... FROM t_c, loc_c WHERE t_c.k = 3 AND loc_c.k = t_c.k FOR UPDATE` (key of another
  group) says "a local table cannot be read in a statement that locks rows of a distributed table of another
  shard group"; a statement without a key that spans groups keeps the old message.
- **P2B-08 held (code read).** `probe_number` parses without raising; no run.
- **P2B-09 held.** README, section on the version of the peers, says the compiled-in version is compared.

## The fate of S14-01 to S14-08

- **S14-01 and S14-05 held.** t/119 passes on both majors (75 s). Mutants: not sending `RELEASE` at the pre-commit event
  (the pre-commit branch made to fall into the abort branch) is killed; not resetting `xact_depth` in the broken-session
  branch of the abort callback survives t/119 (74 s). I read the next level's branch (`xact_depth > curlevel` closes the
  session and notes it when it held work), which does the same repair, so I believe the survivor is redundant, not a
  hole; I did not construct a case in which it matters. The abort and commit callbacks contain no `ereport(ERROR)` on
  any path I read (warnings only); the one `PG_TRY` is in the pre-commit branch, where an error is legal.
- **S14-02: read only.** The deadline arithmetic in the connect and the first command is as the finding asks. No test, as
  stated in the brief.
- **S14-03 held.** t/119 asserts that `TopMemoryContext` grows by under 24 kB over 2000 more failed `RELEASE`s; passes. The
  labels are now made in the short-lived context and only the final text in the long-lived one (read).
- **S14-04 fixed in the code, not tested (P2C-06).** A mutant that always notes the failure survives t/118 and t/119.
- **S14-06 and S14-08: not examined beyond the suite.**
- **S14-07: open, as stated.**

## New findings

### P2C-01 (medium) A multi-row `INSERT`, an `INSERT ... SELECT` or an `ON CONFLICT` insert fails when the key column has a type modifier that changes the stored value

Place: `src/row_router.c:434` and `:452` (`coerce_to_target_type(..., -1, ...)` and `makeRelabelType(..., -1, ...)`; the
comment at `:446-451` says the select list is cast to the column's type but gives no modifier), with the hash taken at
`src/batch_sender.c:869-871` on the value as the source produced it, and the modifier applied only by the assignment cast of
the batch statement on the group (`batch_sender.c:213-220` writes `u.k::<type>` with `FORMAT_TYPE_TYPEMOD_GIVEN` and modifier -1).

Steps (19rc2, three groups, run from node 1):

```
CREATE TABLE ts0 (k timestamp(0), v text);
SELECT kingres.create_distributed_table('ts0', 'k', bucket_count => 12);
INSERT INTO ts0 VALUES ('2024-03-10 10:30:00.7', 'a');      -- four single rows, keys .7 s: all four succeed
INSERT INTO ts0 VALUES ('2024-03-10 11:30:00.7','b'), ('2024-03-10 11:30:01.7','b'),
                       ('2024-03-10 11:30:02.7','b'), ('2024-03-10 11:30:03.7','b');
ERROR:  no partition of relation "ts0" found for row
DETAIL:  Partition key of the failing row contains (k) = (2024-03-10 11:30:03).
CONTEXT:  remote node ...:59556
bulk insert of kingres.ts0, batch 1 to group 1
INSERT INTO ts0 SELECT '2024-03-10 12:30:00'::timestamp + (g || ' sec')::interval + interval '0.7 sec', 'c'
  FROM generate_series(1, 20) g;                             -- ERROR as above
CREATE TABLE n82 (k numeric(8,2), v text);                    -- distributed on k
INSERT INTO n82 SELECT g + 0.005, 'x' FROM generate_series(1, 12) g;
ERROR:  no partition of relation "n82" found for row    DETAIL: ... (k) = (2.01)
INSERT INTO n82 VALUES (1.234,'a'), (2.345,'a'), (3.456,'a'), (4.567,'a');   -- ERROR, (k) = (1.23)
```

The same statements succeed on a single node and the single-row form succeeds on the cluster. The origin hashes the
unrounded value (`.7`), sends it to the group of that bucket, the group rounds it (`01`), and hashes the rounded value to
another bucket; a row is accepted only when that group also owns the other bucket, so a statement with several
fractional keys nearly always fails and a single row with a constant key is accepted by the 2a path. In the generator
the statements with a fractional or over-scale key failed in this way; every difference printed in the first 40 lines of the
seed 7 run was on `timestamp(0)`, `timestamptz(0)` or `numeric(8,2)`. The failure is closed (nothing is written, nothing
stays prepared, the table equals what it was) but the error names "no partition" for valid data, and a type with a modifier
is an ordinary key. No wrong row was seen: a row that is accepted is in the partition of its stored value, which that group
owns. `COPY` was not seen to fail (the input function applies the modifier before the router hashes), but I ran it for a
few statements only.

A fix must achieve: the key (and every column) reaches the router in the value the table will store, that is, the source
select list is coerced with the column's `atttypmod` (length coercion included), so the hash is taken on the stored value;
the same inserts as above succeed and place every row where the single-row path does; an over-length `varchar(n)` or
over-precision `numeric(p,s)` fails with the single node's message. A test of the generator's kind on `timestamp(0)`,
`timestamptz(0)`, `numeric(p,s)`, `char(n)` and `time(0)` keys.

### P2C-02 (medium) The batch of the source cursor is counted in rows, not in bytes: wide rows break the stated memory bound

Place: `src/row_router.c:747` (`SPI_cursor_fetch(portal, true, kingres_shard_insert_batch_rows)`); the statement of the
bound in `README.md:529` ("The memory of a bulk write is about two batches per group, whatever the number of rows") and
`:1065` ("plus the batch of the source cursor"); `t/117_bulk_memory.pl` tests narrow rows only.

Steps (19rc2): `INSERT INTO wide SELECT g, repeat('x', 400000) FROM generate_series(1, 3000) g` with the default
`insert_batch_rows = 1000`. Peak resident memory (`VmHWM`) of the origin backend 443 MB; of the two other groups' sessions
39 MB each. For comparison 3 000 000 narrow rows (id and a 200-byte text) through the same path: 170 MB at the origin,
about 165 MB at each group's session; 3 000 000 rows by `COPY` from a file: 170 MB and 164 MB; `RETURNING` of 1 000 000
rows: 172 MB (the tuplestore spills). The cursor fetch materialises up to 1000 rows whatever they weigh, so the origin needs
`insert_batch_rows x row width`: 2 MB rows need about 2 GB, 4 MB rows about 4 GB, with no setting that is expressed in bytes
(`insert_batch_bytes` only bounds the buffers that are sent). `COPY` is not affected (its chunk is bounded in bytes).

A fix must achieve: the memory taken by the fetch is bounded by about `insert_batch_bytes` plus one row, for example by
fetching in steps whose size follows the width of the rows fetched so far (down to one row), and a test with rows of a few
hundred kilobytes that asserts a peak, next to the narrow ones in t/117; or the README says the bound is `insert_batch_rows`
times the row width at the origin.

### P2C-03 (low) One very large value costs five to eight times its size

Place: the path of one value: the output string, the array literal (`append_element`, `batch_sender.c:282`), the message,
and on the group the parameter text, the array, the `unnest` row and the stored row.

Steps: `INSERT INTO mem2 (id, v) VALUES (-1, repeat('y', 150000000)), (-2, 'z')`: peak of the origin 754 MB (5x), of the
receiving group's session 1188 MB (8x). With the 1 GB that a value may have, a group would need an order of 8 GB, and the
statement then fails with an out-of-memory error (a clean error) or is killed, depending on the machine. The README's bound
does not mention it. I measured 150 MB only; the larger figures are an extrapolation.

A fix must achieve: either the multiplier is stated in the README next to the bound, or a row larger than a threshold is sent
in a form that does not copy it several times (for example a batch of one row with the value as its own parameter in the
column's type, binary format).

### P2C-04 (medium, test quality) A routed row with a NULL distribution key is not tested by the round

Mutant: `batch_sender.c:871`, `nulls[s->key_attno]` replaced by `false` in the call that hashes the key. It survives t/112
and t/115 (both complete and pass; t/112 runs 320 assertions). The same mutant is killed by the generator at once (a node
stopped during the first statements with a `text` key; I did not diagnose it, a dereference of the uninitialised datum is
the likely cause). The design says a NULL key goes to bucket 0 and
it works on the shipped code (the generator has NULL keys in about half of its statements, for 26 types, on both majors),
so this is a gap in the tests, not in the code.

A fix must achieve: a test with NULL keys in `INSERT ... VALUES`, `INSERT ... SELECT` and `COPY`, for a fixed-width and a
varlena key type, compared with the twin, with every group read directly.

### P2C-05 (low, test quality) The "consumed by the first call" property of the one-shot mark has no test

Mutant: `fanout.c:174`, the statement `local_part_armed = false;` in `kingres_shard_local_part_take` removed (the mark then
lasts until the end of the SPI call and a nested statement with the same text takes it). It survives t/002, t/014, t/105, t/107,
t/112, t/113 and t/118. Demonstrated: with the mutant, a `CHECK` function that executes the text of the local part (read from
`PG_EXCEPTION_CONTEXT`, which holds it) affects 14 rows (this node's) where the shipped code affects 40; it is the effect of
`enable_routing = off` that P2B-01 was about, reachable by SQL code, for a nested statement whose text equals the outer
local part. The shipped code is right; nothing would notice a regression.

A fix must achieve: a test whose function replays the exact text of the local part of a fan-out `UPDATE` and of a router
batch from the error context, and requires the routed result (all rows).

### P2C-06 (low, test quality) S14-04 has no test

Mutant: `node_conn.c:3477`, `if (lost_work)` replaced by `if (0)` (the failed `ROLLBACK TO` always dooms the transaction as
before the fix, that is, it is the fix that is removed). It survives t/118 and t/119. The second mutant of part C that
survives (`node_conn.c:3601`, the reset of `xact_depth` in the broken-session branch) is probably redundant (see S14-01).

A fix must achieve: a test with a read-only user session on a node that does not answer `ROLLBACK TO SAVEPOINT`: the
transaction commits (S14-04), and the same with a session that wrote: it does not.

### P2C-07 (low, test quality) Several refusals and second layers survive every test of the round

All of 112 to 118 pass against each of these mutants: `row_router.c:722` (the check of the plan's placement at the start
of the run; the probes make it redundant, I believe), `:970` (`PreventCommandIfReadOnly` of COPY), `:618` (the read-only
check of the router), `:183` (a view of another owner in the source is refused), `:173` (a security barrier view is refused),
`:964` (COPY and row-level security; a table with row-level security cannot be distributed, so this one is unreachable),
`:1021` (the `OVERRIDING SYSTEM VALUE` flag for a COPY into a table with a `GENERATED ALWAYS` identity column). The last one
is a real gap: COPY into such a table works on the shipped code (checked by hand, 6 rows, identity values filled), and
nothing in t/115 loads one. For the read-only refusals the groups' sessions are read-only too, so a statement is still
refused (with another message); I did not run the mutant to see which message. For `:183` and `:173` the unrefused statement
reads the base table with the invoker's rights, so I expect a permission error instead of the refusal, not a leak; not run.

A fix must achieve: one test for each refusal, asserting its message.

### P2C-08 (low) Refusals that a user will meet

(a) `RETURNING k::text` (an integer to text cast) is refused with "the RETURNING list ... cannot be run by the shard groups"
(whole-row `RETURNING t`, system columns, a sub-select, `random()` and `now()` are refused too, by design). A cast to
`text` of an immutable type is a common thing to write. (b) A data-modifying CTE with a multi-group insert,
`WITH i AS (INSERT INTO t SELECT ... RETURNING id) SELECT count(*) FROM i`, is refused with the 2a message "statements that
span shard groups are not supported yet", which the round's notes do not list (they list a `WITH` clause on the `INSERT`).
Both fail closed. I did not check whether the README names them.

A fix must achieve: the message names the cause, or the README lists them; ideally an I/O cast of an immutable type is
shippable in `RETURNING`.

### P2C-09 (note) The rows of a COPY reach the log of every group

With `log_statement = all` (or a statement over `log_min_duration_statement`) each group logs the batch statement with
`DETAIL: Parameters: $1 = '{...}'` (all values, up to `insert_batch_bytes`), as PostgreSQL does for any bind with parameters;
the origin never logs the data of a COPY. On an error the parameters are not logged (default `log_parameter_max_length_on_error`).
`pg_stat_activity.query` on a group shows `/* kingres:insert-fragment place=<number> */ INSERT INTO ... unnest($1::...)`: the
marker, the placement number and the statement, never a value. README could say that the parameters of the batches follow
the group's logging settings.

### P2C-10 (note) Overlapping bulk statements from different origins deadlock across groups; the detector resolves it

Three origins each running 20 statements `INSERT INTO cc1 SELECT g, ... FROM generate_series(r*30, r*30+300) g ON CONFLICT DO
NOTHING` on overlapping keys: with the detector off (the harness default for the tests, `deadlock_check_interval = 0`) the
statements waited for more than six minutes with the sessions on `transactionid` in the groups; with the detector on
(1 s) three statements ended with "distributed deadlock detected", the others committed, the table held 871 of the 871
distinct keys, nothing was prepared, no row was on a wrong group. This is the behaviour of 0.5 and of the design; none of
t/112 to t/119 runs concurrent overlapping bulk statements with the detector on (I did not search the older tests).

### P2C-11 (note) A statement that waits for a frozen group that still answers the kernel waits until a cancel

The wait for the answer to a batch (`collect`, `libpqsrv_get_result_last`) has no time bound of its own; a cancel,
`statement_timeout` and the death of the postmaster end it; a dead host is cut by the keepalives and `tcp_user_timeout`.
t/113 part C tests the cancel with a stopped group. This is the S13 rule as stated; it is only worth saying that the locks
of the statement stay on every group it wrote until then.

### P2C-12 (note) Identity and serial values of rows from different origins collide

Known (the design keeps the node-local sequences of 2a): three runs of the generator (seeds 11 to 13) inserted into a table
with a `GENERATED ALWAYS AS IDENTITY`, a `BY DEFAULT` identity and a `serial` column from random origins: 41 to 131
duplicate values per column across the three groups, no NULL. Not new.

## Mutants of my own (question 10)

Each row is one line changed in the source; "killed" names the test that failed first. Tests listed as passed all passed.

| Mutant | Result |
|---|---|
| bucket of a row: `hash + 1` in `batch_sender.c:875` | killed, t/112 |
| cast of the source column: `TYPEMOD_GIVEN` dropped (`batch_sender.c:220`) | killed, t/112 |
| NULL key hashed as a value (`batch_sender.c:871`) | survives t/112, t/115; killed by the generator, a node stopped (P2C-04) |
| command tag count (`row_router.c:801`) | killed, t/112 |
| probe after the last batch skipped (`batch_sender.c:940`) | killed, t/116 |
| probe before the first batch skipped (`batch_sender.c:674`) | killed, t/116 |
| the writer is not marked, no two-phase path (`batch_sender.c:664`) | killed, t/113 |
| batch in flight not cancelled at the end of the statement (`batch_sender.c:575, 636`) | killed, t/113 |
| COPY permission check removed (`row_router.c:986`) | killed, t/114 |
| COPY file privilege check removed (`row_router.c:940`) | killed, t/114 |
| COPY `FREEZE` not refused (`row_router.c:955`) | killed, t/115 |
| `DO UPDATE` not refused (`row_router.c:262`) | killed, t/112 |
| check options not refused (`row_router.c:361`) | killed, t/112 (passes t/114) |
| source relabel removed (`row_router.c:452`) | killed, t/112 |
| one-shot mark not cleared at consumption (`fanout.c:174`) | survives 002, 014, 105, 107, 112, 113, 118 (P2C-05) |
| `RELEASE` not sent at the pre-commit event (`node_conn.c:3537`) | killed, t/119 |
| `xact_depth` not reset for a broken session (`node_conn.c:3601`) | survives t/119 (probably redundant) |
| S14-04 `lost_work` (`node_conn.c:3477`) | survives t/118, t/119 (P2C-06) |
| plan placement check, read-only checks, view owner, security barrier, RLS of COPY, identity override of COPY | survive 112 to 118 (P2C-07) |

Verdicts that depend on timing: t/113 part C sleeps 1.5 s before it stops the group and asserts the cancel ends within 30 s;
t/116 sleeps 4 s at three places to let a move reach a state (a race, not a bound: under load it could pass without the
window it means to test, though not fail); t/117 compares peak memory of two runs (bounded by 64 MB). The suite ran once
per major under load and nothing failed, so I did not see timing make a verdict.

## What held under attack

- Rows in the right groups: 2900 statements, 26 key types, NULL keys, casts from `text`, `varchar(20)` and `char(20)` sources,
  every session `TimeZone` and `DateStyle` tried, constants and expressions as keys; volatile keys (`random()`,
  `gen_random_uuid()`, `clock_timestamp()`, `nextval()`, 3 x 400 rows each from every origin): every row on the group that owns
  its bucket, none lost or doubled. Sources with joins (also with a reference table), aggregates, windows, `ORDER BY ... LIMIT`,
  `DISTINCT`, `UNION ALL`, `EXCEPT`, sub-selects, and a self insert doubled three times with batch sizes 1, 7, 1000 (400, 800,
  1600 rows as the twin), and from every node: all equal to the twin.
- Empty sources (`INSERT`, `COPY` of nothing, `COPY ... FROM PROGRAM 'true'`), one row with a computed key from every node, all
  rows to one group, a table whose only column is the key, `DEFAULT VALUES`, hostile identifiers (schema and table with a quote,
  a space and a `$1` column, `select`, `user`, mixed case, a column named `u`), 150 columns: all as the twin.
- Prepared statements: a generic plan with parameters of two types from each node; a cached generic plan run before and six times
  after a bucket move: no error, rows complete.
- All or nothing: a `CHECK` violated by one row (5 percent of 2900 statements, all groups, batches of 1 to 1000): nothing of
  the statement anywhere. A group killed (immediate stop) in the middle of an `INSERT ... SELECT` of 4 million rows (batch 655)
  and of a `COPY` from a pipe of 30 million lines: error, counts unchanged, nothing prepared, next statement works. The backend
  of the origin killed with `SIGKILL` in the middle of 20 million rows: the other two groups unchanged (30 and 38 rows), nothing
  prepared; the origin restarted (the harness runs with `restart_after_crash = off`) and the next statements ran. `pg_cancel_backend`
  in the middle of an `INSERT ... SELECT` and of a `COPY` from a program, in a transaction with an earlier insert: error, the
  earlier insert rolled back too. A `COPY` with a bad line after 3000 good ones (batches of 100): nothing stored, error context
  "COPY cp1, line 3001, column k". A function with an `EXCEPTION` block that rolls back a 60-row bulk insert and inserts again,
  from each node; nested savepoints with `ROLLBACK TO` and `RELEASE`: only the rows of the surviving levels, nothing prepared.
- Bucket moves and failover during a bulk write: t/116 passes on both majors (parts A to G); I did not add runs of my own.
- Memory: 3 million narrow rows by `INSERT ... SELECT` in 2.5 s: 170 MB at the origin; by COPY: 170 MB; `RETURNING` 1 million rows:
  172 MB (the figures are `VmHWM` of the session, shared buffers included). Wide rows and one huge row: P2C-02, P2C-03.
- COPY against `DoCopy`: I read `DoCopy` of the PostgreSQL 18 based tree (the roles for a file and a program, the permission check with the columns
  of the statement, the read-only and parallel checks, the row-level security refusal, which is stricter here) and the handler
  repeats each. I did not read the 19 source; on 19 the behaviours I ran are the same. Run: `FREEZE`, `WHERE`, `ON_ERROR`
  (`ignore` and `set_null`), `LOG_VERBOSITY`, `REJECT_LIMIT` refused by name with the reason; `FORMAT json` refused by the core;
  `HEADER match` accepted and a wrong header refused with the core's message; `HEADER 2`, `DEFAULT 'x'`, `FORCE_NULL`, `NULL`,
  binary format by file (500 rows), `FROM PROGRAM` as superuser, an omitted key column with a volatile default, a generated column
  named in the list (refused as in PostgreSQL), a read-only transaction ("cannot execute COPY FROM in a read-only transaction"),
  COPY and `ROLLBACK TO` of a savepoint: all right. Nothing silently ignored. COPY through a driver: not run.
- Privileges and security: a role that may not insert is refused at the origin; a `SECURITY DEFINER` function that inserts 30 rows
  across the groups runs as its owner on every group (rows stored; the caller has no privilege itself). A user session that sends
  the text of an insert fragment (marker, `place=0`, the `unnest` shape, parameters by `\bind`) to a group node gets an ordinary
  statement: 20 rows, distributed to all three groups (5 on that node); the marker has no effect outside a forwarded session.
  A view with `WITH CHECK OPTION` and a view over a view with `CASCADED CHECK OPTION` are refused for a multi-row insert and
  an ordinary updatable view works; `CREATE RULE`, `CREATE POLICY` and `ALTER TABLE ... ENABLE ROW LEVEL SECURITY` are refused on
  a distributed table, and a table with row-level security cannot be distributed. Forged markers in a forwarded session (needs the
  secret) and a wrong `place`: not run (the wrong-placement case is t/116 and t/118).
- Concurrency with the detector on: P2C-10.
- EXPLAIN: `EXPLAIN (ANALYZE)` of a bulk insert runs it once (30 rows, one tag), plain `EXPLAIN` runs nothing.

## Verdict

The round does what it says on every path I ran. I found no wrong, lost or doubled row, no row on a group that does not own
it, and no transaction committed on part of the groups, in about 2900 generated statements and the fault runs above, on both
majors. Both majors build without a warning and pass the whole suite. The two fixes that I rate as worth doing before the next
step are P2C-01 (a key with a type modifier makes valid multi-row inserts fail with a misleading error; the failure is closed) and
P2C-02 (the memory bound of the source side holds only for narrow rows). P2C-04 to P2C-07 are test gaps that I could close
by running mutants: the NULL key, the one-shot consumption, S14-04, COPY into a `GENERATED ALWAYS` table and the refusals have no
test that fails when they break.

Can the sequences track (S13, S14) be closed? For the code, yes: S13 and S14-01, -03, -05 hold under the whole suite and the
mutant that removes the pre-commit `RELEASE` is killed; S14-02 I only read. I would close it with two conditions: a test for
S14-04 (P2C-06) and a decision on the surviving reset of `xact_depth` (a test, or removing the line if the next level's repair
is meant to be the one that counts). S14-07 stays open as stated.

## What was not checked

- S14-02 by a run (the cut of the connect and of the first command), S14-06 and S14-08 beyond the suite.
- The `DoCopy` of PostgreSQL 19 by reading; `COPY FROM STDIN` through a driver; `HEADER` with a number on 19 beyond what the
  suite does; encodings other than UTF-8 and a conversion between the origin and a group.
- Forged markers in a forwarded session with the secret, a wrong `place` sent by hand.
- A move that starts before a statement and a primary that changes, other than t/116; the probe after the last batch other
  than by the mutant that removes it.
- A value larger than 150 MB; rows larger than 400 kB through the cursor in numbers that would need more than 2 GB.
- Triggers on the groups (distributed tables refuse them), virtual generated columns, row-level security (refused),
  tables with a `TOAST`ed column of several megabytes in a sustained load, a cluster of more than three groups, a group with a
  standby that is the only node of its group.
- The mutants that survive were not examined one by one for equivalence beyond what each entry says.
