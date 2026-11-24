# Independent review: kingres_shard 0.9, storage part of phase 12 (branch p12-storage, tip fca6e4820d)

Date: 2026-11-22. Compared with p12f-fix (f72ac87392). Reviewer: independent, read-only (no commit, no push).

## 1. Scope and method

Reviewed (source read in full unless stated):
- src/count_distinct.c and .h, the planner hook call in planner.c, shippable.c (extension_usable), node_conn.c (prepare_one change),
  dist_table.c / rebuild.c (replica identity, access method of leaves), kingres_ledger.c (kl_check_storage), the 0.8--0.9 upgrade
  script (all of it: handshake, am clause, table_ddl, partition_layout, table_change_report wrapper, move_inspect, split_inspect,
  raise_blockers, ledger_distributed, check_ledger, create_move_tables), mk-install-script.sh, t/250, t/270 to t/274,
  README sections "Columnar storage" and "Approximate count(DISTINCT x)", and the design sections 9.1 to 9.7 of the storage design.
- The generated kingres_shard--0.9.sql was checked through t/250 (regeneration) and the pg_regress "upgrade" test (catalog of an
  upgrade equals the catalog of a fresh install), not by reading its 17000 lines.

Executed:
- Full kingres_shard suite (pg_regress and TAP) on the final tip, on PostgreSQL 18 and on PostgreSQL 19.0, in private installs,
  both runs side by side.
- Full kingres_ledger TAP suite on both majors.
- kingres_columnar TAP files that touch decoding or publications (012, 034, 039, 061, 064) on both majors. No columnar TAP file
  runs a bucket move; the move is covered only by kingres_shard t/274.
- Eight extra probe tests of my own (not committed, not part of the tip), described in section 4.
- Twelve mutants (section 6).

## 2. Full-suite counts

| Suite | PostgreSQL 18 | PostgreSQL 19.0 |
|---|---|---|
| kingres_shard pg_regress | 23 of 23 passed | 23 of 23 passed |
| kingres_shard TAP | 223 files, 48471 tests, all successful (6137 s) | 223 files, 48938 tests, all successful (6082 s) |
| kingres_ledger TAP | 39 files, 2587 tests, all successful | 39 files, 2587 tests, all successful |
| kingres_columnar TAP (012, 034, 039, 061, 064) | 5 files, 202 tests, all successful | 5 files, 205 tests, all successful |

No test failed on either major. The two shard runs were taken from the tip as one suite each, with the three late commits included.

## 3. Findings

### P12S-01 (high): with count_distinct_mode = approximate every statement of a role without USAGE on schema kingres_sketch fails

- Where: src/count_distinct.c:231-236 (get_extension_oid, then LookupFuncName with a schema-qualified name), called from
  src/planner.c:3488 for every planned statement before any aggregate is looked at.
- Failing scenario (tested, PG18): two nodes, kingres_sketch installed, role appu has SELECT on a distributed table and no
  other grant.
  `SET ROLE appu; SET kingres_shard.count_distinct_mode = 'approximate'; SELECT 1;`
  gives `ERROR: permission denied for schema kingres_sketch`. So does `SELECT count(*) FROM public.ev`, and a count(DISTINCT)
  on a local table. With USAGE granted on the schema but EXECUTE revoked on the aggregate, a count(DISTINCT) fails with
  `permission denied for aggregate approx_count_distinct`.
- Why it matters: LookupFuncName checks USAGE on the schema, and the executor checks EXECUTE on the aggregate; neither error is a
  fallback. The README says the console turns the setting on with ALTER DATABASE ... SET, so one console action locks every
  ordinary role without that grant out of the whole database (superusers are unaffected). The design says a missing sketch means
  "stays exact with one warning"; a missing privilege is not handled the same way. The lookup (extension oid, extension version,
  function lookup) also runs for every statement while the setting is on, even for statements that contain no count(DISTINCT).
- Suggested fix: first walk the tree and return when no eligible Aggref exists (no lookup at all). Resolve the function with
  missing_ok and check the privileges explicitly (object_aclcheck on the schema with ACL_USAGE and pg_proc_aclcheck with
  ACL_EXECUTE for GetUserId()); on failure keep the statement exact and warn once. Cache the resolved oid per catalog
  invalidation.

### P12S-02 (medium): a cached plan keeps the rewrite after the setting changes, in both directions

- Where: src/planner.c:3486-3488 and src/count_distinct.c:221. The rewrite is done at plan time, and the setting is a plain GUC
  that does not invalidate the plan cache.
- Failing scenario (tested, PG18):
  ```
  PREPARE pq AS SELECT count(DISTINCT u) FROM public.ev;     -- 50000 distinct values
  SET kingres_shard.count_distinct_mode = 'approximate';
  EXECUTE pq;  (7 times: 49446 each, the sixth and later use the generic plan)
  SET kingres_shard.count_distinct_mode = 'exact';
  EXECUTE pq;                                    -- 49446, an exact query answered approximately
  ```
  Same with a plpgsql function that contains the statement (third call after SET exact still returns 49446). A view or a
  statement outside a cache (the same text sent again) gives 50000 correctly, so only cached plans are affected.
  The reverse also holds: a generic plan made under exact ignores a later SET approximate.
- Why it matters: the task requirement is that an exact query is never changed silently; here a session that set exact (or a
  database default that was switched back) gets approximate numbers from prepared statements, function bodies and ORMs that
  prepare.
- Suggested fix: add an assign hook to both settings that invalidates cached plans (CacheInvalidateRelcacheAll, or a call to
  the plan cache reset), or make the plan carry the mode and have the hook refuse to reuse it, for example by marking the
  rewritten query with a dependency that is invalidated on SET. Add a test with PREPARE, plan_cache_mode = force_generic_plan and
  both directions.

### P12S-03 (medium): columnar options are silently reset to defaults by create_distributed_table, undistribute_table and bucket moves

- Where: the options of a columnar table live in its metapage (columnar_set_options); the shard code recreates the table
  through internal_table_ddl / internal_create_move_tables and carries only the access method and reloptions (dist_table.c:298,
  0.8--0.9.sql internal_table_ddl and internal_create_move_tables). The README documents the loss for moves only.
- Failing scenario (tested, PG18):
  ```
  CREATE TABLE oc (id int, v text) USING columnar;
  SELECT kingres.columnar_set_options('oc', compression => 'none', stripe_row_count => 20000, chunk_row_count => 5000);
  SELECT kingres.create_distributed_table('oc', 'id', bucket_count => 4);
  ```
  Every bucket partition reports `zstd / 150000` instead of `none / 20000`. After a move the new copy again has defaults; after
  undistribute_table the table has `zstd | 3 | 150000 | 10000`.
- Impact: no row is lost, but a table that was set to no compression (or a small stripe size for freshness) changes its storage
  size and read behaviour without a message. The report row `columnar_storage` says nothing about it.
- Suggested fix: read columnar_options of the source and apply them to every bucket partition after creation, in the copy tables
  of a move and in the undistribute result; or at least add a `columnar_options_reset` note row to table_change_report and a
  sentence to the README for create/undistribute/alter, as for moves.

### P12S-04 (low): the prepare-time confirmation is skipped for any participant that answers "undefined function", not only for a node being added

- Where: src/node_conn.c:2948-2950. The new condition `ERRCODE_UNDEFINED_FUNCTION` is added to `schema_missing`, whose comment
  says "a node that gets the extension in this very transaction (add_node) ... is not part of the cluster yet".
- Scenario (by reading; not executed): a participant of an existing cluster whose installed SQL lacks internal_sync_confirm (a
  node that did not run ALTER EXTENSION UPDATE, or one where the function was dropped) answers the confirmation with
  undefined_function after PREPARE. The coordinator now treats that as "nothing to confirm" and goes on to commit, so the
  guarantee that the prepared record is durable on the synchronous standby is dropped without any message. Before the change
  only a missing schema was accepted.
- Mitigation present: the version handshake (internal_require_version, claims) refuses most mismatched nodes earlier.
- Suggested fix: carry a flag on the connection (set by add_node for the target) and accept the sqlstate only when the flag is
  set; or confirm with a query that does not depend on the function (for example a check of pg_prepared_xacts plus a standby
  lag test) for the other participants.

### P12S-05 (low): move_inspect checks only the first non-heap relation of a bucket tree

- Where: kingres_shard--0.8--0.9.sql:636-638 (`ORDER BY lc.relname LIMIT 1`) in internal_move_inspect.
- Scenario (reasoned; no second table access method is installed, so not runnable here): a user-partitioned bucket with a
  columnar leaf that sorts first and a leaf of another non-heap access method that sorts later passes the check
  (`amname <> 'columnar'` is evaluated for the first row only) and is then copied USING the first leaf's rules; the other leaf
  gets its access method from the leaves list but its changes are not known to be decodable.
- Suggested fix: raise for every relation whose access method is neither heap nor columnar (drop LIMIT 1, test each row).

### P12S-06 (low): two guards of the count_distinct rewrite are not covered by any test (mutants survive)

- Mutant m1 (src/count_distinct.c: remove the exclusion of oid, oidvector and the reg types) passes t/273 unchanged.
  Baseline behaviour is correct (count(DISTINCT oid_col), regclass and oidvector stay exact: tested, result 10, 1, 1), but
  a regression would make those statements fail with the sketch's 0A000 refusal or count object identifiers in a way that
  differs between nodes.
- Mutant m5 (planner.c: apply the rewrite only when the cluster has more than one shard group) passes t/273, which always uses
  two groups, although the comment says the answer must not depend on the number of groups.
- Suggested fix: add count(DISTINCT oid), regclass, oidvector and a one-group cluster to t/273.

### Notes without a finding id

- The error rate documented as a relative standard error is one sigma, so about one result in three lies outside the bound;
  observed 1.11 percent on 50000 distinct values with p = 14 (bound 0.01). The README wording is right, but a reader may take it
  for a limit. A sentence "about 2 in 3 results are within the rate" would help.
- The warning "count_distinct_mode is approximate but kingres_sketch is not installed" is once per backend (static flag), which
  is the same as once per session.

## 4. What was verified as correct

Data correctness of bucket moves with columnar (probe tests p1 and p7 on PG18, plus t/274 and the full suite on both majors):
- p1: columnar table, 4 buckets, 2 nodes, wal_level logical, logical_decoding_work_mem 64 kB. Four writers ran for the whole
  period through the router: multi-row inserts of 7, 40, 300 and 2500 rows per statement, transactions with a savepoint that is
  rolled back, transactions that are rolled back whole. Meanwhile twelve bucket moves (every bucket three times, so including
  the move back), cleanup after each, columnar_compact calls on source buckets, and four moves cancelled by statement_timeout
  followed by resume_bucket_moves and cleanup. Result: the exact expected number of rows (about 390000), count(DISTINCT id)
  equal to count(*), the sum of ids equal to the expected sum, no id of a rolled-back statement or savepoint present, the same
  numbers from both nodes, columnar_check empty on both nodes, check_cluster empty.
- p7: the same with a user-partitioned table (a columnar leaf and a heap leaf, three writers, rolled-back transactions, twelve
  moves): all counts exact, no duplicate, no phantom rolled-back rows.
- Read of kc_write.c: the logical rows are written by the transaction that writes the stripe, after the pages and the catalog row,
  never for the copies that compaction makes (maintenance flag), so compaction during a move does not double rows.
- The gate by version works: with kingres_columnar 0.1 on the source, move_bucket refuses in "preparing" with SQLSTATE 0A000
  and the hint, nothing is created (no slot, no subscription). With 0.2 on the source and 0.1 on the target the move works
  (the target only applies inserts). Mutant m8 shows the gate is tested.
- REPLICA IDENTITY FULL is set for non-heap leaves and bucket partitions on create, rebuild (alter_distributed_table) and in the
  copy of a move (mutants m6, m7, m12 are caught).
- add_node onto a node without the access method is refused with the node's own error and leaves nothing behind (tested:
  node count stays 2, check_cluster empty); onto a node with kingres_columnar it is covered by t/270 and t/271 (mutant m9 is caught).

count_distinct rewrite (probe tests p2, p4, p6 on PG18, t/273 on both majors):
- Applies only with the setting on, to a level that has a hash-distributed table in its own range table; a local table, a reference
  table, a level without a distributed table, count(DISTINCT a, b), ORDER BY inside the aggregate, arrays, rows and user types
  stay exact. DISTINCT of other aggregates (string_agg) is untouched.
- Nested levels (sub-select, CTE, sub-link, scalar sub-select, view), HAVING, ROLLUP, FILTER (kept), count(DISTINCT x) inside
  sum(...) OVER (), count(DISTINCT e) with an expression argument, an empty input and a NULL-only input all give the right
  shape and a value within the expected error of the exact one. count(DISTINCT x) OVER () is rejected by the parser before the
  hook, so the rewrite never sees it.
- Types tested at 200000 rows (exact versus approximate): int, text, float8 with 0 and -0, numeric, bpchar, jsonb, inet, bool,
  name, "char" (all within 1.2 percent at p = 14); text under a nondeterministic ICU collation (100 distinct values both ways,
  because the sketch hashes with the aggregate's collation); oid, regclass, oidvector, tid stay exact; xid and cid have no
  ordering operator, so the exact query is already refused.
- The precision formula is monotone and its bounds are right (0.01 gives 14, 0.1 gives 7, 0.0005 caps at 18); the setting range
  check works; mutant m4 (ceil to floor) is caught.
- The function is resolved by qualified name and its membership in the extension is checked, so another function with the same
  name in another schema or one created by a user is never called; the setting is USERSET and never on by default; with
  shippable_extensions = none or without the extension the statement stays exact with one warning.
- The rewrite copies the Aggref, clears DISTINCT and the sort-group reference, keeps FILTER and the input collation, sets the
  result type bigint and adds the precision as a constant; the planned aggregate is a partial aggregation (plan shows partial
  states from each group).

Upgrade 0.8 to 0.9:
- t/250 regenerates kingres_shard--0.9.sql from the 0.8 script and the 0.8--0.9 script and compares byte for byte, so a change to
  either script without regeneration fails; it is meaningful as a freshness check. Mutants m8 and m11 (changed in both files
  consistently) are caught by the behavioural tests, as intended.
- The pg_regress "upgrade" test compares a snapshot of the catalog (columns, defaults, function definitions, privileges, comments)
  of a direct install with the result of ALTER EXTENSION UPDATE from 0.1 to 0.8 and it passes on both majors.

Version handshake:
- A cluster with a node at SQL 0.8 and a library at 0.9: add_node refuses a node of another version with the hint to run
  ALTER EXTENSION UPDATE; create_distributed_table refuses ("a table operation needs 0.9 on every node"); move_bucket refuses
  ("a bucket move needs 0.9 on every node") through internal_check_node_versions; check_cluster reports
  extension_version_differs for the node; after ALTER EXTENSION UPDATE on the node the cluster works. A mixed cluster that has
  distributed tables already was not built in the probe, so reads and writes across 0.8 and 0.9 nodes of a live distributed
  table were not exercised beyond what the existing tests do.

Ledger and refusals:
- ledger_enable refuses a columnar table and a partitioned table with a columnar parent (message, detail and hint present);
  SET ACCESS METHOD on a ledger table is rejected by the rewrite guard.
- create_distributed_table and create_reference_table on a ledger table raise KS016 with the list of blockers in the detail;
  the online variants are refused earlier by the columnar_online blocker for columnar tables. Mutants m10 and m11 are caught.
- Security: the new internal functions (internal_am_clause, internal_ledger_distributed, the renamed base report) are revoked from
  PUBLIC; none is SECURITY DEFINER; every function sets search_path and qualifies pg_catalog operators; identifiers are built
  with %I and values with %L; table_change_report requires the internal helper that needs superuser for remote probes and
  swallows only connection errors.

## 5. Not verified

- A live mixed-version cluster with distributed tables (see handshake note).
- Behaviour with a third-party access method other than columnar (none exists in the tree).
- The 17000-line generated install script was not read; it is covered by t/250 and the upgrade snapshot test.

## 6. Mutants run

Each mutant was built into a private copy of the PostgreSQL 18 installation and run against the named tests; the source was
restored from the tip for the next one.

| Id | Mutation | Tests | Result |
|---|---|---|---|
| m1 | count_distinct.c: drop the oid and oidvector exclusion | t/273 | SURVIVED (P12S-06) |
| m2 | count_distinct.c: do not skip aggregates with ORDER BY | t/273 | killed (2 tests) |
| m3 | count_distinct.c: rewrite reference tables too | t/273 | killed (3 tests) |
| m4 | count_distinct.c: precision uses floor instead of ceil | t/273 | killed (many tests) |
| m5 | planner.c: rewrite only when more than one shard group | t/273 | SURVIVED (P12S-06) |
| m6 | rebuild.c: no REPLICA IDENTITY FULL in the shadow script | t/270, t/271, t/274 | killed |
| m7 | dist_table.c: no REPLICA IDENTITY FULL for leaves | t/274, t/270 | killed |
| m8 | 0.8--0.9.sql and 0.9.sql: version gate of columnar 0.2 lowered to 0.0 | t/250, t/271, t/274 | killed |
| m9 | node_conn.c: undefined_function no longer accepted at prepare | t/270, t/271, t/059 | killed (add_node with columnar fails) |
| m10 | kingres_ledger.c: remove kl_check_storage | kingres_ledger t/040 | killed |
| m11 | both SQL files: rename the check_cluster check name for ledger_distributed_table | t/272, t/250 | killed |
| m12 | both SQL files: internal_am_clause never names the access method | t/270, t/274 | killed |

Two of twelve mutants survived; both are test gaps, not defects of the code.

## 7. Cleanup

The worktree was removed with git worktree remove (no force), the private installs and work areas were deleted, no process of the
review was left running, nothing was committed or pushed.

## Fixes

Made on branch p12-storage after the review, each with a test that fails without it (a one-line mutant of each fix is killed by the named test).

| Finding | Fix | Test (mutants killed) |
|---|---|---|
| P12S-01 | `count_distinct.c` looks the sketch up only when the first rewritable `count(DISTINCT)` of a distributed level is met (no lookup, no warning for any other statement); it checks USAGE on the schema of the extension and EXECUTE on `approx_count_distinct` for the current role, and on failure keeps the statement exact with one warning per session | `t/273`: a role without USAGE runs `SELECT 1`, `count(*)` and `count(DISTINCT)` (exact, one warning), with USAGE it gets the sketch, without EXECUTE it is exact again, a statement without a candidate raises no warning (mutants: no USAGE check, no EXECUTE check, eager lookup) |
| P12S-02 | assign hooks on `count_distinct_mode` and `count_distinct_error_rate` call `ResetPlanCache()` when the value changes (a rollback of a `SET` calls the hook again, so both directions are covered) | `t/273`: PREPARE with `force_generic_plan` through exact, approximate, exact; `EXPLAIN EXECUTE`; a plpgsql function; the error rate (mutants: each hook) |
| P12S-03 | `kingres.internal_columnar_args()` reads the options of a columnar table (for a partitioned table those of its first columnar leaf); they are applied with `columnar_set_options` to the bucket partitions of `create_distributed_table` and of the rebuilds, to the plain table of `undistribute_table`, to the leaves of a user-partitioned table (`internal_partition_layout` has an `opts` column) and to the copy of a move (the answer of `internal_move_inspect` and `internal_move_tree` carry them) | `t/270` (create, alter, undistribute, a leaf), `t/274` (a move of a bucket partition and of a leaf) (mutants: the helper, create, rebuild, leaf, plain table, move bucket, move leaf) |
| P12S-04 | `KsConn.new_node`, set by `add_node` for the node it adds and reset at the end of the transaction; `prepare_one` accepts `undefined_function` after the PREPARE only when it is set | new `t/275`: a participant whose `internal_sync_confirm` is gone fails the transaction, the prepared part is rolled back (mutant: the flag removed) |
| P12S-05 | `internal_move_inspect` first looks for any relation of the tree that is neither heap nor columnar (no `LIMIT 1` over all non-heap relations) and refuses naming it, then checks the version for the columnar ones | `t/271` with a second table access method (`heap2` over the heap handler): a bucket with a columnar leaf sorting first and a `heap2` leaf is refused naming the `heap2` leaf (mutant: the test for other access methods) |
| P12S-06 | tests only | `t/273`: `count(DISTINCT o)`, `rc`, `ov` of `oid`, `regclass` and `oidvector` stay exact (plan and answer), and a one-group cluster rewrites too (they kill m1 and m5) |

Limits that remain: the options of a rebuilt or undistributed distributed table are those of its first columnar bucket partition by name on the node that runs the call; a node that holds none gives the defaults.
