<!-- SPDX-License-Identifier: PostgreSQL -->
# Code review: contrib/kingres_shard (version 0.1)

Independent review against the specification in
`kingres/docs/design/shard-mvp.md` (including the author's "Implementation
notes and deviations") and the project rules in
`kingres/docs/dev/coding-standards.md`. The reviewer is not the author of
this code.

## Integrator's note: read this first

This review was produced in two passes that disagree, so the file below is
**not complete and is wrong in two places**. The reviewer's first report
(delivered as a message, cut off part-way) listed four findings that this
file does not contain, and this file's section "Examined and found sound"
declares two of them sound. The integrator re-ran the reproductions on
2026-11-14 on a throwaway two-node cluster; the results are below and take
precedence over anything later in this file. The fix work must cover the
findings in this note **and** findings 1 to 4 of the main list.

| ID | Severity | Finding | Integrator's check |
|---|---|---|---|
| S-A | Critical | **Any role becomes superuser through the `SECURITY DEFINER` event trigger.** `kingres.forget_dropped_tables()` has `SET search_path = pg_catalog, kingres`. `pg_temp` is not listed, so it is searched first for type and relation names; the unqualified cast `d.relid::oid` then resolves to a domain the caller created in their temporary schema, and the domain's `CHECK` runs the caller's function as the extension owner. The `sql_drop` trigger fires on every `DROP` in the database. | **Reproduced.** As an ordinary role, with one distributed table present: `create function pg_temp.evil(x anyelement) returns bool language plpgsql as $$ begin alter role alice superuser; return true; end $$; create domain pg_temp.oid as pg_catalog.oid check (pg_temp.evil(value)); create temp table zz(a int); drop table zz;` After it, `rolsuper` for alice is `true`. With no row in `kingres.dist_table` the cast is never evaluated and the exploit does nothing, which is why an empty-catalog test does not show it. |
| S-B | Critical | **The stale-map reload applies a catalog snapshot as the bootstrap superuser inside the user's session**, through `kingres.internal_apply_catalog()`, whose `search_path` has the same hole (type names such as `text` resolved from the caller's temporary schema). | Not re-run by the integrator. The reviewer's first report says it was reproduced (a `pg_temp.text` domain, then a routed `SELECT` after the map on two nodes disagreed). The same root cause as S-A is confirmed, so treat it as real. |
| S-C | High | **Session settings leak into the forwarded statement.** The statement is deparsed with the local session's output settings but parsed remotely with the remote session's. | **Reproduced.** With `SET datestyle = 'SQL, DMY'`, `insert into t values (<id>, '03/04/2026')` stores `2026-04-03` when the key is local and `2026-03-04` when it is forwarded. This is silent wrong data. The first report also reproduced a doubled backslash with `standard_conforming_strings = off`. This file's "found sound" entry for "Deparse correctness for literal round-tripping" is therefore wrong. |
| S-D | Unknown | A fifth finding in the first report began "Any PL" and was cut off. Its content is lost. | The fix work must look for it: the likely subject is a PL/pgSQL or SQL-language function (or any nested statement) that reads or writes a distributed table and is planned outside the routing checks. |

Main-list finding 1 (`kingres_shard.forwarded` is settable by any user) was
also re-checked: an ordinary role can run `SET kingres_shard.forwarded = on`.

Rules for every fix that comes out of this note: every function in the
install script gets `pg_temp` as the last entry of its `search_path` and
schema-qualified type names; `SECURITY DEFINER` is removed wherever it is not
strictly needed; and each finding gets a regression or TAP test that fails
before the fix.

## Scope and method

Read in full: every file under `contrib/kingres_shard/src/`
(`kingres_shard.c`, `catalog.c`/`.h`, `hashing.c`/`.h`, `node_conn.c`/`.h`,
`node_admin.c`/`.h`, `dist_table.c`/`.h`, `planner.c`/`.h`,
`remote_scan.c`/`.h`, `kingres_shard.h`), the install script
`kingres_shard--0.1.sql`, the `pg_regress` tests
(`sql/*.sql`, `expected/*.out`), and the TAP tests (`t/*.pl`). Cross-checked
against the PostgreSQL 18.7 tree at the repository root for every core API
used (planner hook contract, `pg_get_querydef`, `eval_const_expressions`,
relcache invalidation, `PQ*` libpq functions, `SPI_*`, subtransaction
callback ordering in `xact.c`).

Reproduction was done on a throwaway two-node cluster started under WSL2
(`Ubuntu-24.04`, user `kingres`) on ports 57001/57002, using the already
built `~/kingres-work/install/bin` binaries (no rebuild, no changes to
`~/kingres-work/ext` or `~/kingres-work/install`). The cluster and all
scratch files were removed after the review; no repository file other than
this report was modified.

Findings are listed most severe first. Each gives the file and line(s), the
defect, a concrete reproduction or the reasoning when it was not run, and a
suggested fix.

## Findings

### 1. CRITICAL — `kingres_shard.forwarded` lets any ordinary client disable the cross-group safety net and get silently wrong results

**File:** `contrib/kingres_shard/src/kingres_shard.c`, lines 83-89 (the
`DefineCustomBoolVariable` call for `kingres_shard.forwarded`, declared
`PGC_USERSET`); consumed throughout `contrib/kingres_shard/src/planner.c`
(e.g. lines 442, 465-466, 471-472, 481-482, 490-492, 503-509).

**What is wrong.** `kingres_shard.forwarded` means "this session is itself
a forwarded one; the origin node already proved the statement is confined to
one bucket, so this node must not re-refuse it" (see the design note "With
`kingres_shard.forwarded` on, a statement that cannot be decided is run
locally"). The flag is read in `kingres_shard_planner_hook` to turn every
"statements that span shard groups are not supported yet" refusal, every
colocation-group mismatch, and every "bucket hashes differently" mismatch
into a silent `pass_through()` to the standard planner instead of an error.

The GUC is `PGC_USERSET` with no additional gate: it is set by
`node_conn.c`'s `connect_to_node()` via connection `options` for a *real*
forwarded session, but nothing stops an ordinary, unprivileged client of the
*same* node from issuing `SET kingres_shard.forwarded = on` in its own
interactive session. Once set, the single safety net that stops a
multi-bucket statement from silently touching only the local shard group's
physical partitions is gone, for arbitrary SQL the user types themselves —
not just for genuinely forwarded statements. This is exactly the attack
surface item 3 of the task ("whether an unprivileged user can set
`kingres_shard.forwarded` ... to bypass routing") warns about, and it does.

The project's own TAP test documents the *intended* one-hop behavior without
ever checking that an un-forwarded client cannot reach it the same way:
`t/002_routing.pl:473-475`:
```
($rc, $out, $err) = run_sql($n2,
    "SET kingres_shard.forwarded = on;\nSELECT count(*) >= 0 FROM public.kv;");
is($err, '', 'a forwarded session does not refuse statements that span groups');
```
This only asserts "no error"; it never checks the *value* of `count(*)`, so
it is exactly the kind of test from priority item 6 that "would pass even if
the feature were broken" — and it is.

**Reproduction (observed).** Two-node cluster, group 0 = node1 (local map
group primary), group 1 = node2; table `t(id bigint primary key, v text)`,
`bucket_count=>8`, buckets round-robin over `[0,1]`. Inserted one row whose
key hashes to a local bucket (`id=1`, bucket 0, group 0) and one row whose
key hashes to the remote group (`id=3`, bucket 1, group 1); both succeed
individually through normal single-key routing.

```
-- ordinary session, no special privilege, routing fully on:
postgres=# SELECT * FROM t;
ERROR:  statements that span shard groups are not supported yet

postgres=# SET kingres_shard.forwarded = on;
SET
postgres=# SELECT * FROM t;
 id |      v
----+--------------
  1 | local-bucket
(1 row)
```

The row with `id=3` (on the remote group) is silently missing from the
result; the statement reports success with no warning. The same path
applies to any statement the planner cannot prove single-bucket — an
unqualified `UPDATE t SET ...` or `DELETE FROM t` would likewise silently
touch only the local group's partitions while reporting full success.

**Fix.** Do not use a client-settable GUC as the trust boundary for "this
statement was already decided by an origin." At minimum make the GUC
superuser-only (`PGC_SUSET`) and non-resettable mid-session
(`PGC_BACKEND`, settable only via connection `options`, which is how
`node_conn.c` already sets it) so an ordinary login role cannot flip it.
Better: do not trust a GUC at all for this decision; carry the "already
routed" fact some other way that an ordinary SQL session cannot forge (for
example, a value that is only ever consulted from the physical connection
state established by `connect_to_node()`, not from a `SET`-table GUC).

### 2. CRITICAL — routing hashes under the column's stored collation, not the comparison's actual collation: a collation-mismatched predicate is silently sent to the wrong shard

**File:** `contrib/kingres_shard/src/planner.c`, `restrict_from_conjunct()`
(lines 233-272) and `restrict_to_value()` (lines 188-226, specifically line
191 `coloc->collation` and the call at lines 218-221).

**What is wrong.** `restrict_from_conjunct()` accepts an `OpExpr` as
"dist_column = key" once it is a `Var`/`Const` (or `Var`/`Param`) pair whose
operator is a member of the column's hash operator family
(`op_in_opfamily`, lines 264-268). Family membership is independent of
collation. The function never compares the clause's actual collation
(`op->inputcollid`, or `exprCollation()` of the clause) against
`coloc->collation` — the collation that was fixed when the table was
distributed and that the *physical* bucket map was built from
(`hashing.c:kingres_shard_try_bucket`, which always hashes with
`coloc->collation`, never with the collation attached to the comparison).

Consequently, a `WHERE`/`ON` clause that compares the distribution column
under an explicit `COLLATE` different from the column's own collation (or,
by the same mechanism, through any expression that attaches a different
collation to the comparison) gets a bucket number computed from the
*wrong* collation's hash, so the statement is forwarded to — or executed
locally restricted to — the wrong shard. The query silently returns fewer
rows than the correct cluster-wide answer, with no error.

**Reproduction (observed).** Non-deterministic ICU collation `ci`
(`und-u-ks-level2`, case-insensitive) created on both nodes. Table
`w(name text primary key, v int)` distributed on `name` with the table's
*default* (deterministic) collation — i.e. `coloc->collation` is the
database default, not `ci`. `bucket_for(name)` under the default collation
gives `'a' -> bucket 6` (group 0, local to node1) and `'A' -> bucket 3`
(group 1, node2) — confirmed with `kingres.bucket_for()` directly. One row
`('a', 1)` inserted; its physical bucket (6) is local to node1.

```
node1=# SELECT * FROM w WHERE name = 'a';
 name | v
------+---
 a    | 1

node1=# SELECT * FROM w WHERE name = 'A' COLLATE ci;   -- should match 'a' case-insensitively
(0 rows)                                                -- WRONG: silently empty

node1=# SET kingres_shard.enable_routing = off;        -- disable routing entirely
node1=# SELECT * FROM w WHERE name = 'A' COLLATE ci;
 name | v
------+---
 a    | 1                                               -- correct once routing is bypassed
```

With routing on, the planner computed the bucket for key `'A'` using the
table's default (case-sensitive) collation — bucket 3, group 1 — and
forwarded the statement to node2, which physically holds no row matching
`name = 'a'` at all, so it correctly (from its own point of view) returns
zero rows. The row that case-insensitively matches lives in bucket 6 on
node1 and is never even scanned. Disabling routing (and thus running a
plain local scan across every locally-present partition) immediately
produces the correct answer, confirming the routing decision — not the
storage or query execution — is at fault.

This is exactly the hazard flagged by the task ("the key compared under a
different collation ... than the hash-compatible operator"), and no test in
`sql/`, `expected/`, or `t/` exercises a `COLLATE` override on the
distribution column (`grep -i collate` over those directories matches
nothing).

**Fix.** In `restrict_from_conjunct()`, reject (fall through to "not
restricted") any equality clause whose collation (`op->inputcollid`, taken
via `exprCollation((Node *) clause)` or the operator's own `inputcollid`)
does not match `coloc->collation`, instead of accepting any operator merely
because it belongs to the right hash opfamily.

### 3. HIGH — `TRUNCATE` on a distributed table only empties the local buckets, with no error or warning, because utility statements never reach the routing hook

**File:** no `ProcessUtility_hook` exists anywhere in
`contrib/kingres_shard/src/` (confirmed: `grep -rn ProcessUtility src/`
returns nothing); `kingres_shard_planner_hook` in `planner.c` only ever
sees `Query` nodes (`parse->commandType` checked against `CMD_SELECT`,
`CMD_INSERT`, `CMD_UPDATE`, `CMD_DELETE`, `CMD_MERGE` at lines 432-435), and
`TRUNCATE` is a `TruncateStmt` utility statement that bypasses the planner
entirely.

**What is wrong.** Because nothing in the extension inspects utility
statements, `TRUNCATE <distributed table>` is never refused, never routed,
and never forwarded. PostgreSQL's own executor simply truncates whatever
partitions are physically present on the local node (the buckets of the
local shard group) and reports success. Buckets that live on other shard
groups are completely untouched, and the user is given no indication that
only part of the logical table was cleared.

**Reproduction (observed).** Table `t` as above, with `id=5` on a remote
bucket (group 1) holding row `('five', ...)` and no colliding local row.

```
node1=# SELECT * FROM t WHERE id = 5;
 id |   v
----+-------
  5 | final
node1=# TRUNCATE t;
TRUNCATE TABLE
node1=# SELECT * FROM t WHERE id = 5;
 id |   v
----+-------
  5 | final        -- the row is still there: TRUNCATE never reached it
```

`TRUNCATE TABLE` reports unqualified success while the logical table is far
from empty. This is not listed among the documented phase-1 limitations in
`shard-mvp.md`'s "Implementation notes and deviations" (which discusses DDL
propagation of `DROP`/`ALTER`, not `TRUNCATE`'s data-clearing semantics), so
it is an undocumented, user-reachable correctness gap, not a recorded
deviation. No test in `sql/`/`t/` issues `TRUNCATE` on a distributed table.

**Fix.** Either install a `ProcessUtility_hook` that refuses (or
cluster-wide forwards) `TRUNCATE` on a distributed table the same way the
planner hook refuses other cross-group statements, or document this
limitation explicitly and have `create_distributed_table` revoke `TRUNCATE`
on distributed tables until it is supported, so the failure mode is an
error instead of silent partial data loss of apparent effect.

### 4. MEDIUM (reasoned, not reproduced under true concurrency) — `kingres.add_node()` has no locking against a concurrent call, so two primaries can be registered for the same shard group

**File:** `contrib/kingres_shard/src/node_admin.c`,
`kingres_add_node()` (lines 242-390), specifically the primary-uniqueness
check (lines 312-321) and the `node_id`/`group_id` allocation (lines 296-336);
`kingres_shard--0.1.sql` lines 20-34 (`kingres.node`'s constraints: a
primary key on `node_id` and a unique key on `(host, port)`, but no
constraint of any kind on "at most one `role = 'primary'` row per
`group_id`").

**What is wrong.** `kingres_shard_require_map_group()` only checks that
the *caller* is the map-group primary; it takes no lock (no
`LOCK TABLE`, no advisory lock) over the whole add/remove/sync operation.
`kingres_add_node()` picks `group_id` (when not given) via
`SELECT coalesce(max(group_id),0)+1 FROM kingres.node` and checks for an
existing primary of a given group via a plain
`SELECT node_id FROM kingres.node WHERE group_id = $1 AND role = 'primary'`
— both under default `READ COMMITTED` visibility and no row lock. Two
concurrent `kingres.add_node(..., group_id => 5, role => 'primary')` calls
(or two concurrent calls that both omit `group_id` and so both compute the
same "next" group) can each see "no primary yet for this group" and both
proceed to `INSERT`. Nothing in the schema prevents two `role = 'primary'`
rows for the same `group_id` (only `node_pkey` and
`node_host_port_key` exist), so both inserts succeed, leaving the catalog
with two primaries for one shard group. `kingres_shard_group_primary()`
(`catalog.c`) then returns whichever one its linear scan hits first —
undefined, and not necessarily the same node from every backend's own
cache, since each backend rebuilds its cache independently.

This was not reproduced with genuinely concurrent sessions in this review
(doing so reliably needs two real concurrent `psql` clients racing on the
same transaction boundary, which the available single-threaded scratch
setup did not exercise); it is reported as a reasoned defect from reading
the schema and the code path, and is flagged explicitly by priority item 5
of the task ("concurrent `add_node`/... from two sessions — is there a
lock?").

**Fix.** Add a partial unique index,
`CREATE UNIQUE INDEX ON kingres.node (group_id) WHERE role = 'primary'`,
so the second concurrent insert fails cleanly with a constraint violation
instead of silently succeeding; additionally take an explicit lock (e.g.
`LOCK TABLE kingres.node IN SHARE ROW EXCLUSIVE MODE`, or a
`pg_advisory_xact_lock`) for the whole duration of `add_node`/`remove_node`/
`sync_catalog`/`create_distributed_table` so that id allocation itself is
also race-free.

## Examined and found sound

- **Deparse correctness for literal round-tripping.** `DateStyle`,
  `extra_float_digits`, and `standard_conforming_strings` differences
  between sessions were tested directly (date literal parsed under
  `DateStyle='SQL, DMY'`, a full-precision float, and a backslash-escaped
  string under `standard_conforming_strings = off`); in every case the
  value survives the round trip through `pg_get_querydef()`/remote
  execution correctly, because the literal is folded to a `Const` at parse
  time on the origin (using the origin's session settings) before being
  deparsed, and `node_conn.c` fixes `DateStyle`, `IntervalStyle`, and
  `extra_float_digits` on every remote connection. No bug found here.
- **SQL injection in the deparsed statement.** The remote SQL text comes
  from `pg_get_querydef()` under `search_path = pg_catalog`
  (`planner.c:deparse_for_remote`), which fully schema-qualifies every
  name, so a hostile object in `public` on the remote node cannot be
  resolved by an unqualified reference. Administrative statements built by
  string concatenation (`dist_table.c`, `node_admin.c`) use
  `quote_identifier()` for identifiers and bind parameters
  (`SPI_execute_with_args`/`kingres_shard_exec_params`) for values; no
  unescaped concatenation of untrusted values into SQL text was found.
- **`PGresult` lifetime.** Every `PGresult` obtained in `node_conn.c` and
  `remote_scan.c` is either consumed and `PQclear`'d on every path
  (including inside `kingres_shard_report_error`, which always clears
  before raising) or is cleared within a `PG_FINALLY` (`store_result()` in
  `remote_scan.c`). No leak was found by inspection on the error paths
  exercised (statement-timeout cancellation mid-`PQgetResult`, and rollback
  of an unfinished remote transaction).
- **Transaction/savepoint mirroring.** `conn_xact_callback`/
  `conn_subxact_callback` in `node_conn.c` were exercised with nested
  savepoints, a `ROLLBACK TO SAVEPOINT`, a `RELEASE SAVEPOINT`, and a
  `plpgsql` `EXCEPTION` block catching a unique-violation from a forwarded
  insert; behavior matched expectations (the remote savepoint is rolled
  back and released in step with the local one). Statement-timeout
  cancellation while waiting on a remote query
  (`libpqsrv_cancel`/`discard_connection` in `abort_connection`) correctly
  discards the connection rather than hanging or leaving stale state.
- **Privilege checks on a forwarded statement.** A revoked `SELECT`
  privilege on the distributed table is enforced on the node that actually
  executes the remote statement (connections are opened "as the current
  user", never with elevated privilege) — a non-superuser without `SELECT`
  got "permission denied for table t" both locally and when the statement
  was forwarded.
- **`kingres.internal_*` function privileges and `SECURITY DEFINER`
  search_path.** All catalog-mutating SQL/PL/pgSQL functions pin
  `SET search_path = pg_catalog, kingres` and the administrative ones have
  `EXECUTE` revoked from `PUBLIC` (`kingres_shard--0.1.sql` lines 616-637);
  `forget_dropped_tables()` (the only `SECURITY DEFINER` function) also
  pins its search path. No search_path hijack of these functions was found.
- **Stale-map retry.** Forcing one node's local bucket map to disagree with
  the map group's authoritative copy and then issuing a statement that
  hits the stale bucket correctly produced `KS001`, and (when the origin
  itself is not the map group) correctly triggered a catalog reload and a
  one-shot retry through SPI, consistent with the design note "it does not
  apply when the origin is the map group primary."
- **General catalog cache lifetime discipline.** `catalog_load()` builds
  the new cache entirely in a fresh `MemoryContext` under `PG_TRY`/
  `PG_CATCH`, swapping in the new context and deleting the old one only
  after a successful build, with `ks.loading` preventing reentrant
  recursion into the planner hook while the catalog itself is being loaded
  from heap scans. This looked correct by inspection; no reentrancy or
  half-built-state bug was found.
- **ON CONFLICT's `excluded` pseudo-relation** is correctly excluded from
  routing consideration (`planner.c` lines 294-300), matching the design
  note.

## Not examined

- **Two-phase commit / multi-group transaction paths** are explicitly out
  of scope for phase 1 per the specification and were not reviewed beyond
  confirming that the extension does refuse them (`PREPARE TRANSACTION`
  after remote work, and local-after-remote writes, both raise clear
  errors as documented).
- **True concurrent-client races** (two real, simultaneously-running
  `psql` sessions hitting the same race window) were not exercised; only
  single-session, sequential reasoning and code reading were used for
  finding 4. A multi-process concurrency test (e.g. two TAP background
  processes issuing `add_node` at once) would be needed to turn that
  finding into a hard reproduction.
- **Parallel workers, background workers, and hot-standby/recovery
  behavior of the hooks** were read but not run under those execution
  modes (no parallel worker or standby was started in this review).
- **`pg_dump`/`pg_upgrade` interaction and `CREATE EXTENSION` ordering**
  beyond the `creating_extension` guard in the planner hook were not
  exercised end-to-end (no actual `pg_dump`/`pg_upgrade` run against a
  populated cluster).
- **Non-ASCII/multi-byte encoding edge cases** in the deparsed literal
  round trip (only ASCII and a Latin collation test were tried).
- Full line-by-line review of `t/001_cluster.pl` and `t/003_hooks.pl` was
  not completed to the same depth as `t/002_routing.pl`; only targeted
  `grep`s were used to check for `TRUNCATE`/`COLLATE` coverage.
