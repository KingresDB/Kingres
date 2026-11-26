<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
# Independent review of phase 2b of kingres_shard, steps 1 and 2

Reviewer: an independent reviewer. Date: 2026-11-18.

## Scope and method

Reviewed state: the read-only export of `contrib/kingres_shard` (commit 316bf4ea7e), `kingres/docs/design`
(`shard-phase2b.md`: sections 1 to 6, 10, 17 and the notes "Step 1", "Step 2", "What step 3 will find changed"),
`kingres/docs/adr` (0022, 0023), `kingres/docs/reviews` and `kingres/dev/ext.sh`. Nothing in the export was changed.
Work was done on private copies: a copy of each installation (18.7 with assertions, 19rc2 with assertions), a copy of the
export, a copy of `ext.sh` that puts the throw-away server's socket and port in my own area, and a copy of the TAP
framework module with its port range narrowed to 59500-59599. Every long command ran under `timeout` and `nice -n 10`
and under a watchdog that kills the process group of the command when the summed resident size of its processes passes
2 GB (an address-space `ulimit` was tried first and breaks the servers). Each test cluster is three nodes and, for the
fuzz, one ordinary twin node; their memory is bounded by the default settings (the watchdog never fired). A three-node cluster is three servers, four with the twin, as in the project's own tests; I never ran two clusters or two suites at once.
The work was sequential. The area was removed at the end and no process of mine is left.

I read `prune.c`, `placement_check.c`, `fanout.c` (nonce), the changed parts of `planner.c`, `dist_scan.c`, `catalog.c`
and `hashing.c`, `pg_compat.h`, `shippable.c` where a fragment is accepted, and the new tests `t/105`, `t/106`
(`t/098` and `t/099` for step 1).

What I wrote and ran:

* A query generator against a twin (`fuzz`): a cluster of three groups with 12 buckets and, next to it, an ordinary node
  with the same data. For each distribution-column type there is a table `t_x`, a colocated partner `u_x` (half of the
  rows), and an ordinary table `loc` on every node. Each generated statement is run on the twin and from each of the
  three nodes of the cluster, and the multisets of rows are compared (errors are compared as errors). 15 statement
  shapes, 22 weighted slots (single table with an ordered result, aggregates with `count(DISTINCT)`, `UNION ALL`, `UNION`, `EXCEPT`, a CTE
  that is materialized, not materialized and default, joins `JOIN`/`LEFT`/`RIGHT`/`FULL` with the restriction in `ON` or
  in `WHERE`, a join with the local table, a left join from the local table, a window function, `GROUP BY ... HAVING`,
  `ORDER BY ... LIMIT ... OFFSET`, `NOT EXISTS`, a derived table) over predicates built from 29 kinds of atom (`=`, `<>`, `<`,
  `<=`, `>`, `>=`, `IN` with 1 to 80 values, `= ANY (ARRAY[...])`, `= ANY` of a typed array, `<> ALL`, `= ALL`,
  `BETWEEN`, `IS [NOT] NULL`, `IS [NOT] DISTINCT FROM`, a cast of the key to text, row comparison `(k, v) = (...)` and
  `IN ((...), (...))`, a scalar sub-select, `IN (SELECT ...)`, a correlated `EXISTS`, `CASE`, `coalesce`, `nullif`,
  `NOT`, a contradiction `k = a AND k = b`, an OR of `IN` lists), combined with `AND`, `OR`, `NOT` to depth 3. Constants
  are present keys, absent keys, NULL, and values of other types (`int2`/`int8`/`numeric`/`float8` against integers,
  `name`/`varchar`/`bpchar`/`char(3)` against text, `date`/`timestamp`/`timestamptz` against each other, `cidr` against
  `inet`, `interval` spelled in other units, `-0` and `NaN`, an explicit `COLLATE`). Prepared statements (`PREPARE`
  with `$1`, `= ANY ($1)`, `$1 OR $2`, `IN ($1,$2,$3)`, parameters with `AND`/`OR`/`<>`) run 8 times each under
  `force_generic_plan`, `force_custom_plan` and `auto`.
* Types: `int2 int4 int8 numeric float8 text varchar bpchar name "char" bytea uuid bool date time timetz timestamp
  timestamptz interval inet macaddr pg_lsn oid jsonb int4[] enum`, a domain over `int`, a domain over `text`, `text
  COLLATE "C"`, `text` with a deterministic ICU collation and with a non-deterministic ICU collation (case-insensitive).
  `bit varying` cannot be a distribution column; `int4range` failed in my own load step and was not tested.
* Mutants of my own (one line each) against `t/105`, `t/106`, `t/098`, `t/099`; a stress with moves; a nonce probe with
  a non-superuser; a rescan memory probe; cost probes; and the full TAP suite on both majors.

One honest note on my own tool: the first version of the fuzz compared the output of a failed `psql` start with itself,
so its first ~28 000 comparisons were meaningless; I found it because no statement had ever returned an error. Those
numbers are discarded. Everything below comes from the corrected harness (checked by a mutant that it kills, see P2B-05).


## What I ran

| Major | What | Count | Result |
|---|---|---|---|
| 18.7 | build, clean, `EXT_WERROR=1`, `KINGRES_JOBS=2` | 21 files | no warning |
| 19rc2 | the same | 21 files | no warning |
| 18.7 | `make installcheck` (pg_regress) | 13 tests | all pass |
| 19rc2 | the same | 13 tests | all pass |
| 18.7 | `t/098 t/099 t/105 t/106` | 1226 tests | all pass |
| 19rc2 | the same | 1227 tests | all pass |
| 18.7 | full TAP suite `t/*.pl` | 106 files, 10140 tests, 2575 s | all pass (`t/046` skips: needs a second major) |
| 19rc2 | full TAP suite `t/*.pl` | 106 files, 10693 tests, 2594 s | 105 files pass; `t/102_slow_nodes.pl` failed 6 of 64 (load average 12 to 15 from other work on the machine; see P2B-05); rerun alone: 64 of 64 pass |
| 18.7 | fuzz, seeds 11, 21, 41, 51 | 12 445 units, 37 335 node-executions | 0 wrong results |
| 19rc2 | fuzz, seed 31 | 10 150 units, 30 450 node-executions | 0 wrong results |

A "unit" is one generated statement or one prepared-statement session (8 EXECUTEs). Together: 22 595 units, 67 785
runs from a node, about 18 800 plain statements and about 3 800 prepared sessions (about 30 000 EXECUTEs), 29 types,
all three origins. Differences between the cluster and the twin: 0 where the twin answers with rows and the cluster
answers with other rows; 63 cases where both fail with different messages (the twin: `integer out of range`,
`collation "en_US" ... does not exist`; the cluster: `a generic plan cannot route ...`); about 2 000 cases where the twin
answers and the cluster refuses (details in P2B-02, P2B-03 and the known refusal below). In a sample of 1 080 EXPLAINs
(seeds 41 and 51) 166 showed `(pruned by ...)`, so pruning was exercised in about 15 percent of the statements.

The "known refusal" is the one the design lists as unchanged: `a generic plan cannot route a statement on distributed
tables` for a prepared statement under `force_generic_plan` with `k = $1` or `k = $1 AND ...`. It is about half of
the refusals and I do not count it.

Moves while reading: a pgbench-like loop of 2 readers and 1 mover (50 moves of buckets between the three groups
while generic and custom plans of `= ANY ($1)`, `IN ($1,$2,$3)`, `$1 OR $2` and an unrestricted `count(*)` ran, two
sessions, 6 000 statements each): 0 wrong counts, 17 `KS001` (bucket map changed), no hang. A first version of this
check reported wrong counts; they were all the previous value of a psql variable after a failed statement (my
harness), and the check was fixed to reset the variable.

## Findings

Summary: 0 critical, 1 high, 3 medium, 3 low, 2 notes. No wrong answer was found.

| ID | Severity | Summary |
|---|---|---|
| P2B-01 | high | The nonce of the local part is readable by SQL code that the local statement runs (a `CHECK` function of the table, a domain, a default); with it a role that is not a superuser plans nested statements without routing, which `kingres_shard.enable_routing` reserves to superusers |
| P2B-02 | medium | A scan whose restriction deparses with `COLLATE` (or a `name`/`text` mixed list) is accepted by the origin and refused by every other group with the retryable `KS001`; ADR 0022 says the scan answers every read |
| P2B-03 | medium | `UNION` and `INTERSECT` with an arm that contradicts itself on the key are shipped whole to one group, which refuses them with `KS001` ("expanded a SQL-language function") |
| P2B-04 | medium | The pruning of a rescanned scan allocates in the query context at every start and never frees: 210 KB per rescan for a 10 000-element array (345 MB after 1 500 rescans), linear in both |
| P2B-05 | low | Tests that cannot fail: 13 of 25 one-line mutants survive `t/105`/`t/106`; among them the operator rule of pruning, the lifetime of the nonce, the origin recompute, the probe order |
| P2B-06 | low | A read pruned to the origin's own group costs 3 to 4 times the local plan; the origin recompute scans all of `bucket_map` (every colocation group) at each start |
| P2B-07 | low | `FOR UPDATE` in a read that mixes a remote key with a local table says "spans shard groups" and advises to restrict to one key, which the statement already is |
| P2B-08 | note | `pg_strtoint64` on a node's probe answer is called before `PQclear`: a leak or an unclear error from a node that answers non-numeric text |
| P2B-09 | note | The extension version of a peer is compared with the compiled-in constant, not with the installed version of this node |

### P2B-01 (high) The nonce of the local part can be read by SQL code, and with it a non-superuser turns routing off

Place: `src/fanout.c:93` (`local_nonce`), `96` (prefix), `167-175` (`kingres_shard_local_part_nonce_ok`),
`537-600` (`run_local`: the text is `/* kingres:local <nonce> */ <statement>` and goes through `SPI_execute_with_args`),
`src/planner.c:1516` (`if (kingres_shard_local_part_nonce_ok(query_string)) return pass_through(...)`),
`src/kingres_shard.c:112-118` (`kingres_shard.enable_routing`, `PGC_SUSET`), design 4.4 ("A nonce cannot be guessed by
such a function").

What it protects against holds (guessing: 64 bits from `pg_strong_random`; replay after the statement: see below). What
the design did not consider is that the text of the statement that SPI runs is part of the error context, and the
context stack of a PL/pgSQL function that is called from inside that statement contains it.

Steps (18.7; the run on 19 was not repeated). Cluster of three groups; superuser creates `public.mine2 (id int primary
key, v int)`, distributes it, inserts 40 rows, `ALTER TABLE mine2 OWNER TO lowpriv`; `lowpriv` is `NOSUPERUSER` and
`create_distributed_table` is refused for it (`permission denied for function create_distributed_table`), so it only
owns a table the administrator gave it. On each node `lowpriv` creates, in its own schema rights:

```
CREATE FUNCTION public.lp_steal(x int) RETURNS boolean LANGUAGE plpgsql AS $f$
DECLARE ctx text; nonce text; seen bigint; plain bigint;
BEGIN
  BEGIN PERFORM 1/0; EXCEPTION WHEN OTHERS THEN GET STACKED DIAGNOSTICS ctx = PG_EXCEPTION_CONTEXT; END;
  nonce := substring(ctx from 'kingres:local ([0-9a-f]{16})');
  IF nonce IS NOT NULL THEN
    EXECUTE '/* kingres:local ' || nonce || ' */ SELECT count(*) FROM public.mine2' INTO seen;
    EXECUTE 'SELECT count(*) FROM public.mine2' INTO plain;
    RAISE NOTICE 'lowpriv: nonce=% nested-with-nonce-sees=% nested-plain-sees=%', nonce, seen, plain;
  END IF;
  RETURN true;
END $f$
```

then, as `lowpriv` on node 1: `ALTER TABLE public.mine2 ADD CONSTRAINT c CHECK (public.lp_steal(v))` (propagated), and
`UPDATE public.mine2 SET v = v + 1 WHERE v > 0`. Output (14 rows of the origin's group):

```
NOTICE:  lowpriv: nonce=b4496564b5e985be nested-with-nonce-sees=14 nested-plain-sees=40
UPDATE 40
```

The nested statement with the comment is planned for this node only (14 rows); the same statement without it is routed
(40). The same with a superuser gave the same output, so the role is not what makes it work; the table owner's right
to add a `CHECK` (or a domain, a default, a generated column, an index expression) that runs a function is.
Nested writes: `INSERT ... VALUES (k, 0)` with the nonce for 31 keys succeeded for the 9 keys whose bucket is on the
origin and failed with `no partition of relation "mine2" found for row` for the others, so I produced no placement
violation. A nested `UPDATE` of a reference table with the nonce is stopped by a second guard
(`reference table "rf" cannot be written by this plan, which would change this node's copy only`); that guard is the
only thing between the nonce and a silent divergence of a reference table, and the nonce branch sits before the
reference-write branch in `planner.c`.
Replay after the statement is over fails (property "held only while that call runs" holds): the nonce captured into a
session setting and used in a comment of a later `SELECT count(*) FROM kv` gave 60 (routed), and neither `EXPLAIN` of
the fan-out write (`Remote SQL: /* kingres:write-fragment */ UPDATE ...`) nor `pg_stat_activity` shows it. It does
reach the client in the context line of an error of the local part, and the server log.

Why it matters: it gives a non-superuser the effect of `enable_routing = off` for the nested statements of a local
part, a switch the extension keeps for superusers, and in steps 3 and 6 the same prefix is meant for the local part of
a batch and of a fragment (design 4.4, "What steps 2 and 3 will find changed": `KS_LOCAL_PART_PREFIX` "for a batch that
wants to be recognised"), where a planned-as-fragment statement would also skip the shippability check and apply
`partialize_plan`.
Mutants F2 (nonce not restored after the call) and F3 (only part of the nonce compared) survive `t/105`: no test
reads the nonce or uses it after the call.

A fix must achieve: the information that makes a statement "the local part" must not be in text that SQL code can see
(error context, `current_query`, `pg_stat_statements`, logs). One way: a one-shot flag set by `run_local` and consumed by
the first call of the planner hook after `SPI_execute_with_args` starts (SPI plans before it executes, so no nested
statement can come first), combined with a check of the statement text; or bind to the portal or the `PlannedStmt` made
by that call. A test that runs a function with a `PG_EXCEPTION_CONTEXT` reader inside the local part and requires that
the nested statement is routed (sees 40), and one that uses a captured value after the call.

### P2B-02 (medium) The origin ships a restriction that the receiving group refuses (`COLLATE`)

Place: `src/planner.c:1283-1332` (`is_scan_fragment`, which calls `kingres_shard_clause_shippable` on the re-parsed
text), `src/shippable.c` (no case for `CollateExpr`, so `ship_walker` refuses it; `collation_ok` at 92 only looks at
collation OIDs of nodes it knows), `src/planner.c:873-942` (`plan_forwarded` falls back to the routing analysis and raises
at 935 or at 929), `src/planner.c:778` and `830` (the messages).

Steps (18.7, three nodes, `tx (k text primary key, v int)` distributed, 12 buckets, run from node 1 and node 2; an
ordinary table answers them, and the fuzz found the same shapes):

```
SELECT count(*) FROM tx WHERE k = 'a' COLLATE "C";              -> ERROR: cannot prove that the statement touches only buckets of this node
SELECT count(*) FROM tx WHERE k LIKE 'a%' COLLATE "C";          -> the same
SELECT count(*) FROM tx WHERE k IN ('a'::name, 'b');            -> the same
SELECT k FROM tx WHERE k = coalesce('a '::char(3), 'ab'::name); -> ERROR: bucket 2 of public.tx is not on this node (stale map version 15)
SELECT count(*) FROM tx WHERE k COLLATE "C" = 'a';              -> 1 (works)
```

The node's log shows what it received: `DECLARE ks_scan_4 NO SCROLL CURSOR FOR SELECT k FROM public.tx AS ks_t WHERE
((k = ANY ('{a,b}'::text[] COLLATE "C")))` followed by the error. The origin decides on the planner's tree (a constant
that carries a collation), deparses it with `COLLATE`, and the receiver parses a `CollateExpr` that its walker refuses.
The error is `KS001`, whose hint says "repeat the statement"; repeating never helps. Contradicts ADR 0022 ("a statement
the scan can run is never refused"). In the fuzz it is the largest class of real refusals: 384 of 737 (18.7, seed 11),
41 of 183 (seed 21), 161 of 221 (seed 51), mostly text-like keys with a `name`, `bpchar` or `COLLATE` constant.
I cannot say whether the step 2 code made it worse: the export has no history, the walker and `is_scan_fragment` are
2a code, and I did not have the code before step 2.

A fix must achieve: the origin and the receiver decide on the same tree: either the walker accepts a `CollateExpr`
whose collation passes `collation_ok`, or the origin refuses to ship what its own walker, run on the deparsed and
re-parsed text, would refuse; a refused fragment is not `KS001` unless a retry can succeed. A test that deparses and
re-parses every restriction of a generated list and compares the two decisions (the design repeats this rule on the
receiving side for steps 5 to 8, so this class will multiply).

### P2B-03 (medium) A set operation with a self-contradicting arm is refused

Place: `src/planner.c:784-840` (`check_plan_dist_rtes`, the "SQL-language function" error), the statement analysis of
`analyze_query` (`planner.c:346-414`) that routes the whole statement to one bucket.

Steps (18.7; from node 1 and node 2; the right answers are 1 row, 0 rows, 1 row, 1 row). `kv (id int primary key, ...)`, 12 buckets:

```
SELECT id FROM kv WHERE id = 1 AND id = 2 UNION     SELECT id FROM kv WHERE id = 1;   -> ERROR: cannot prove ... The planner expanded a SQL-language function that reads distributed table public.kv.
SELECT id FROM kv WHERE id = 1 AND id = 2 INTERSECT SELECT id FROM kv WHERE id = 1;   -> the same
SELECT id FROM kv WHERE id = 1 AND id = 2 UNION ALL SELECT id FROM kv WHERE id = 1;   -> 1
SELECT id FROM kv WHERE id = 1 AND id = 2 UNION     SELECT id FROM kv WHERE id = 3;   -> 3
```

The error comes from the other node (`CONTEXT: remote node ...`, with the whole statement printed): the origin shipped the
statement whole to one group, the receiver counted more distributed range-table entries in the plan than its analysis
saw. I did not find why `UNION ALL` passes and `UNION` does not. It is `KS001` again.
Not shown to be new in step 2.

A fix must achieve: a statement an ordinary node answers is answered or refused with a message that is true; the count
`check_plan_dist_rtes` compares must be the same on both sides; a test with a contradicting arm in every set operation.

### P2B-04 (medium) The pruning memory of a rescan is never freed until the statement ends

Place: `src/dist_scan.c:1077-1079` (`MemoryContextSwitchTo(estate->es_query_cxt); s->frags = remote_groups_of(...)`),
`src/prune.c:303-310` (`DatumGetArrayTypeP`, `deconstruct_array`: the element and null arrays, copies of a detoasted array,
and the list of groups are allocated in the current context), design 4.4 ("Rescan restarts the fragments").

Steps (18.7): `SET enable_hashjoin = off; enable_mergejoin = off; enable_material = off;` then
`SELECT count(*) FROM (SELECT g FROM generate_series(1,1500) g) s LEFT JOIN kv ON kv.id = ANY (ARRAY[1,...,10000])`:
the plan is `Nested Loop Left Join` with the scan as the inner side, which is rescanned once per outer row. Resident
size of the backend, sampled once a second: 29 580 kB at 1 s, 56 488 at 3 s, ... 338 864 kB at 48 s, 345 268 kB at the
end: about 210 kB per rescan, linear in the array and in the number of rescans; nothing is freed before the end of the
statement. With a 200 000-element array and 5 000 rescans the same code needs about 20 GB; I did not run that (the
rule of this review forbids it) and I argue it from the linearity.

A fix must achieve: the evaluation of the terms runs in a short-lived context that is reset at every start (copy only
the list of groups out), and a test that rescans a scan with a large `IN` list thousands of times and checks that the
backend's memory stays flat (`pg_backend_memory_contexts`).

### P2B-05 (low) Tests that cannot fail

Mutants (one line each, built into a separate copy of the 18.7 installation, run against `t/105` and `t/106`, plus
`t/098` and `t/099` for the last one; 26 tried, 25 valid):

| Mutant | Result |
|---|---|
| P2 OR: an arm that names no key is skipped | killed (`t/105`, 1 test) |
| P4 group of the bucket off by one | killed (86) |
| P5 `*pruned = false` | killed (12) |
| C1 version check removed | killed (2) |
| C2 identity compare removed | killed (2) |
| C7 group rule removed | killed (`t/106`, 3) |
| F1 nonce check without the nonce | killed (1) |
| N1, N2 the two halves of the 6.5 condition | killed (109, 8) |
| N3 the write guard of `can_distributed_scan` removed | killed (the file died at test 37) |
| D1 runtime pruning off | killed (12) |
| H1 `% (bucket_count - 1)` | killed |
| P1 collation compare removed | survives; equivalent while the second half of the condition (non-deterministic) stays |
| P3 AND intersects with union | survives; only less pruning, always correct |
| P7 equality-only operator check removed (`k < 5`, `k <> 5` would prune as equalities: wrong answers) | **survives `t/105` and `t/106`**; killed by `t/099` (17 failures) and by my fuzz (113 differences) |
| P8 `= ALL` allowed | survives; equivalent (a subset of `= ANY`) |
| P9 the `varno` test of a key column removed | survives; equivalent (a base restriction names one relation) |
| C3 probe-before compare of the placement removed | survives; redundant with the probe after |
| C4 origin recompute after the DECLAREs removed | survives (the notes of step 2 say so) |
| C5 plan-number check removed | survives (the notes say so) |
| C6 probe tables sorted in the wrong order | survives: no caller passes two tables yet |
| C8 `failover_possible` ignores a node that is not primary | survives |
| F2 nonce not restored after the call | survives (P2B-01) |
| F3 only a prefix of the nonce compared | survives |
| D2 the first of two origin-partition checks removed | survives (the second covers it) |

So 12 killed, 13 survive; of the survivors 4 are equivalent or redundant (P1, P3, P8, P9 by argument), 4 are admitted
in the notes or redundant (C3, C4, C5, D2), and 5 are real holes: P7, C6, C8, F2, F3. The P7 hole is the important one:
the rule "the operator must be an equality of the hash family" is the single line that keeps `k < 5` from pruning, and
only the harness self-test (by accident) and my generator catch its removal. The order of the nonce check against the
partition refresh (design: "after") is tested by a grep of `planner.c` for the two function names (`t/105` D), which no
mutant that moves the code behaviourally can be told from. The fuzz is a good detector: it kills P7 (113 of 1 575 runs
differ) and is blind to the equivalent mutants, as it should be.

Timing: the new tests wait for fault files (event, 30 s bound) and use one `statement_timeout = 400` against a statement
that needs about 45 s, so their verdict does not depend on timing. `t/102_slow_nodes.pl` (not new) does: on 19rc2 it failed
6 of 64 at load average 12 to 15 (`KS007 ... did not answer within 3 seconds` for a node slowed to 0.9 x the timeout)
and passed alone.

A fix must achieve: a one-line test per hole: an inequality on the key that must not prune (`t/105` has no inequality),
a nonce read and replayed, a probe of two tables in a different OID order, a standby that is the only registered node
of its group, an origin whose cache is stale while the catalog tables are new.

### P2B-06 (low) What a pruned read costs

Place: `src/dist_scan.c:1065` and `1199`, `src/catalog.c:778` (`kingres_shard_placement_hash_now` opens `bucket_map` and
the node table, `AccessShareLock`, and scans every row of every colocation group), `737` (`kingres_shard_placement_hash`
walks all buckets of the group, at planning and at every start).

Measured (18.7, one client, `pgbench`, ms per statement, key of the origin's own group): the local plan `id = k` takes
0.14 / 0.16 / 0.23 with 12 / 1 024 / 4 096 buckets; `id IN (k1, k2)`, which keeps the scan with 0 remote groups
(`Remote groups: 0 of 2 (pruned by id)`), 0.36 / 0.62 / 0.83; with a remote key `IN (remote, local)` 2.9 (1 024
buckets). After adding 8 colocation groups of about 1 000 buckets (8 084 rows in `bucket_map`) the pruned read of the
12-bucket table went from 0.19 to 0.36 ms (the local plan moved from 0.15 to 0.27 in the same minutes, so part of this
is noise). So the cost of a read grows with the total size of the bucket map, not with the table's, and the origin
recompute is paid even when no remote group is asked. It is not a cliff for one statement; a row router that calls
the check per batch (step 3) will be.

A fix must achieve: the check of a statement reads only its own colocation group (an index or the placement epoch of
phase 10), and a pruned scan with no remote group and an unchanged cache reads nothing.

### P2B-07 (low) `FOR UPDATE` on a read that mixes a remote key with a local table: the message and the hint are wrong

Place: `src/planner.c:1224-1237` (`can_distributed_scan` raises for `rowMarks`, now reached from the 6.5 branch at 1667).

`SELECT * FROM kv, loc WHERE kv.id = <key of another group> AND loc.id = kv.id FOR UPDATE` raises `row-level locking on a
statement that spans shard groups is not supported yet`, hint `Restrict every distributed table of the statement to one
key, or remove FOR UPDATE`. The statement does not span groups and is restricted to one key; the old message
("statements that combine distributed tables of another shard group with local tables") was nearer the truth.

A fix must achieve: a message that says "a local table cannot be read in a statement that locks rows of another group".

### P2B-08 (note) A probe answer is parsed before the result is freed

`src/placement_check.c:207`, `214`, `243`, `246`: `pg_strtoint64(PQgetvalue(...))` can raise on text that is not a
number, before `PQclear(res)`. On 18 the result is then lost (malloc), on 19 it is freed with its context. A kingres
node never answers non-numeric text; only a node that is not a kingres node would. Read only, not run: I found no way
to make a kingres node answer it.

### P2B-09 (note) The version compared is the compiled-in one

`src/placement_check.c:192`: `strcmp(version_of_peer, KINGRES_SHARD_EXT_VERSION)`. A node whose library is new but whose
`ALTER EXTENSION UPDATE` has not run refuses every peer, and a node updated in SQL but not restarted is refused by every
peer. That matches "finish the upgrade" but the message names "this node runs X" with the library's number; say so in the
README.

## Answers to the questions of the brief

1. **Wrong answers.** None found: 22 595 units, 67 785 runs against a twin on both majors, with pruning engaged in about
   15 percent of the plans; moves of 50 buckets while prepared statements with generic and custom plans read. Every
   operator and expression shape in the list was generated, except: a volatile function in the key position (it is
   never pruned), `ROW` comparison with a non-hashable type, and a `SELECT ... FROM` function that reads the table (not
   generated). What is not covered by the twin: a statement the cluster refuses (P2B-02, P2B-03) is counted as a
   refusal, not as a right answer. Collations: `C`, `POSIX`, ICU deterministic and non-deterministic were generated; the
   non-deterministic case is never pruned (second half of `equality_ok`), which is correct.
2. **The placement check.** A move or a change of primary between the first probe and the end of the start fails with
   `KS001` (t/105 C, `t/106`, and the 50-move stress). A cached generic plan across a move: 0 wrong results in 12 000
   executions; across `DROP TABLE` and re-creation as a table of another type, with another bucket count, or as a
   reference table: the plan is replaced (the answers were 6, 3 and 1, all right); a dropped table: `relation does not
   exist`. Accepting tables not placed together: `kingres_shard_probe_before_sql` supports several tables but no caller
   passes more than one, so that form is untested (C6). A group that does not hold the bucket after a stale map: caught
   by the probe of the asked node, because each node compares its own placement number of the whole colocation group,
   and an unasked group cannot hold a bucket the (stale) map gives to an asked one; I argued this from the code and
   from the stress, not from a dedicated test. Refusing what is right: any move in the colocation group changes the
   number every node compares, so a point read that a move does not concern can still fail with `KS001`; I read this,
   did not isolate it. A node whose extension library and installed version differ (P2B-09).
3. **The nonce.** See P2B-01: not guessable, not replayable after the call, but readable from inside the statement; a
   non-superuser gets the planner bypass for nested statements. The role does not get another role's rights: the local
   part runs through SPI as the session role.
4. **Design 6.5.** Against a twin and from each node: a join of a remote key with a local table, `CREATE TABLE AS`,
   `INSERT ... SELECT` into a local table, a `SCROLL` cursor with `FETCH BACKWARD` and `ABSOLUTE`, `COPY (SELECT ...)`,
   a volatile function in the statement: all right. `UPDATE loc ... FROM kv`, `MERGE`, `DELETE ... USING`, a write to a
   distributed table that reads a local table: refused as before. Privileges: without `SELECT` on the distributed
   table the statement fails at the origin (`permission denied for table kv`) and, with the grant on the distributed
   table only, on the local table; `REVOKE` on the local table at the origin only denies; a view owned by another role
   is refused (`is read through a view owned by role ...`). The local table is read with the session role's rights at the
   origin, the remote rows with the forwarded session. Snapshots: the remote part runs in `READ COMMITTED` remote
   transactions (the log shows `BEGIN ISOLATION LEVEL READ COMMITTED`), so under `REPEATABLE READ` the remote rows are
   not repeatable; that is design 10.1 and phase 8, but the 6.5 reads (refused before) are now exposed to it. The only wrong message is P2B-07.
5. **Unchanged behaviour.** `EXPLAIN` of an unpruned scan has the line `Remote groups: N`, of a pruned one `Remote groups:
   1 of 2 (pruned by id)`; two regression expectations changed, as the notes say. Performance: P2B-06. Locking: the
   probe and the recompute take `AccessShareLock`; no deadlock or stall in the 50-move stress. I could not compare with
   the code before step 2 (no history).
6. **Both majors.** `-Werror` clean build on both; regress 13 of 13 on both; the whole suite on both (the one failure is
   a timing test under load, passing alone). `PG_VERSION_NUM` appears only in `src/pg_compat.h` (the files outside it
   have none). `hash % bucket_count` appears only in `hashing.c:98`. The unused helpers of `pg_compat.h`
   (`ks_result_adopt`, `ks_window_ignore_nulls`, `KS_SET_PLAN_ORIGIN`) compile but are not exercised.
7. **Memory and crashes.** No crash on either assertion build in the fuzz, the stress and the whole suite; a 200 000-value `IN` (1.2 s, 13 MB for
   the client), 2 000 `OR` arms (0.06 s), nesting depth 300. P2B-04 (memory growth). The nonce is restored in
   `PG_FINALLY` (`fanout.c:585`); a failure of `SPI_connect` or `psprintf` before the `PG_TRY` leaves nothing set. A fuzz of
   text from another node: only the probe answer parses text (P2B-08); I did not fuzz it.
8. **Test quality.** P2B-05.
9. **What step 3 and later will trip over.**
   * The nonce channel (P2B-01) is the one the batch sender and the fragment's local part are meant to share.
   * The receiver re-decides on re-parsed text (P2B-02): every deparse asymmetry becomes a refused query in the
     fragment kinds of steps 5 to 8; a round-trip test belongs in step 3, before the walker is extended.
   * Per-batch cost: `kingres_shard_placement_hash_now` scans the whole `bucket_map`, and `kingres_shard_placement_hash` is
     O(buckets), so a router that checks per batch pays for every colocation group (P2B-06).
   * `kingres_shard_try_bucket` per row: for a value of another type than the column the cross-type path calls
     `get_opfamily_proc` and `fmgr_info` at every call; the same-type path uses the cached finfo. The router should hold
     a per-column cache. `kingres_shard_bucket` raises where `try_bucket` returns false (a `bpchar` into a `text` key:
     `cannot compute a bucket for a value of type character`), so a router needs the `try` form and its own message.
   * The probe orders tables by OID and reads the answer by position (`placement_check.c:63`, `100`); no test has two
     tables (C6).
   * `kingres_shard_group_rule` is called by the scan; the router and the fan-out write must call it too, and it is
     skipped inside a control call.
   * Pruning allocates in the query context (P2B-04); a router that prunes per row would multiply it.

## What held under attack

* Pruning: every operator and shape listed, 29 types including cross-type constants (`int2` against `int8`,
  `timestamp` against `timestamptz`, `name` against `text`, `-0` against `0`, `1.0` against `1.00`, `'a '::bpchar`
  against `'a'`, intervals in other units, `cidr` against `inet`), domains, `COLLATE "C"` and ICU collations, NULL and
  `NULL` in arrays, 200 000 values, parameters of generic and custom plans: no wrong result.
* The one bucket lookup: same bucket for `name`, `varchar` and `text` values of the same string (checked with
  `kingres.bucket_for`); a row inserted through a `name` constant is found by a `text` predicate.
* The placement check against 50 moves with generic plans, a cached plan across a dropped and re-created table.
* The nonce against forgery (`/* kingres:local 0000000000000000 */`, `... 1 ...`, the dead nonce of a finished
  statement): all planned as ordinary statements.
* Privileges of the 6.5 read.

## Verdict

Steps 1 and 2 do what they say on the inputs I could build: I found no wrong answer, no crash, no data in the wrong
place, and the two majors agree. They are not ready to be built on without three changes: the nonce must stop being
readable (P2B-01, high, and steps 3 and 6 extend its use), the pruning memory must be freed (P2B-04), and the tests must
cover the lines that keep pruning correct (P2B-05, above all the operator rule). P2B-02 and P2B-03 are refusals of reads
that a single node answers, with the wrong advice ("repeat"); I could not show that step 2 introduced them, and they
belong to the scan, but ADR 0022 says there should be none.

## Not checked

* The code before step 2: nothing in the export says what behaviour changed; every "unchanged" above is by reading and by
  comparing with the design.
* A standby that became primary, a stale map on one node, a cut-off old primary: only through `t/106` and the mutants
  C7 and C8; I wrote no case of my own.
* Two or more tables in one probe (no caller), the `kingres_shard_probe_placement` helper, `KS_PROBE_IDENTITY = 0`.
* A parallel worker running a pruned scan; `ON CONFLICT`, `COPY`, the batch sender (step 3 and later).
* Whether `UNION` with a contradicting arm (P2B-03) fails in the code before step 2, and why `UNION ALL` does not.
* A fuzz of the text of the probe answer, a rogue node, and the 19rc2 runs of the nonce probe, the rescan probe and the
  cost probes (18.7 only).
* `int4range` as a distribution column (my load step failed), `bit varying` (refused by the extension).
* The fuzz is single-session per origin; no concurrent writers during pruned reads except the bucket mover.
* The non-superuser nonce case needs an owned distributed table; I did not test a domain `CHECK`, a column default or a
  generated column as the carrier, only a table `CHECK` (the carrier does not matter to the planner hook).
