<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->

# Review of kingres_shard phase 9: the online rebuild engine and the fixes for the first review

Reviewer: independent (did not write the code). Tree under review: commit 00bf7ef2f5, extension version 0.7.
Nothing in the tree was changed; all builds, experiments and mutants ran in private copies.

## Scope and method

Read: `kingres/docs/design/shard-phase9.md` (all of it, the dated sections at the end twice), the phase 9 parts of
`contrib/kingres_shard/README.md`, the first review (for format and for what was already found), the 0.7 SQL
(`kingres_shard--0.7.sql`: capture, replay, switch, retire, clean, undo, advance, cancel, resume, wrappers, report),
`src/rebuild.c`, the P9A-01 code in `src/dist_table.c` and `src/copy_routed.c`, and the P9A-02 hook code in `src/utility.c`.

Method, in this order:

1. Build the tree clean with `-Werror` on both majors into private copies of the two installs, and run the phase 9
   tests (t/140 to t/150) and the pg_regress suite on both.
2. Read the engine against the six questions; write throw-away TAP scripts (private, not part of the tree) for every
   suspicion, and run them. A suspicion is a finding only when a script showed it.
3. Run 27 one-line mutants of capture, replay, switch, undo, resume and the P9A-01 fix on PostgreSQL 18, with the
   tests t/144 to t/150 as the judge.

What I did not take on trust: each fix of P9A-01..07 was attacked by a script (below), not accepted from the design
text. Findings are numbered P9B-01 and on; none repeats a finding of the first review that is fixed and holds.

## What was run (per major)

All TAP runs used ports 58500 to 58599 only (the harness copy in the private install was changed to that range),
temporary files under the private work directory, one cluster at a time, every command under `timeout`. The private
work directory stayed under 600 MB. No process of other jobs was touched; at the end no process of mine was left.

| What | PostgreSQL 18.7 (assertions) | PostgreSQL 19rc2 |
|---|---|---|
| Clean build with `-Werror` of kingres_shard, kingres_ledger, kingres_sketch | 0 warnings, built | 0 warnings, built |
| t/140 to t/150 (11 files), one run each | pass, 1621 tests, 209 s | pass, 1621 tests, 207 s |
| pg_regress of kingres_shard (20 tests, including `distribute_rows`, `sequence_options`, `online_refusals`, `security`, `utility_guard`, `upgrade`, `pg_facts`), one run each | 20 of 20 | 20 of 20 |
| My experiment scripts (X1 to X22, L, L2, L3, M, SEQ below) | each run 1 to 3 times (the first runs of X1 and X5 had an order mistake of mine; the numbers below are from the corrected runs) | X1, X2, X5, X5b, X6, X6b run once; same results as on 18 |
| 27 mutants, t/144 to t/150 each | one run each (see Mutants) | not run |

No test of the tree failed in any run, and none needed a repeat for a port or timing failure.

## Answers to the questions

### 1. Is P9A-01 really fixed?

For the blocking variant and the reference variant: yes, as far as I could break it. The mechanism holds in code: the
call takes `EXCLUSIVE` on the table first (`dist_table.c`, the lock at the start of both functions), every later SPI
statement of the call is not read-only (so it takes a fresh snapshot), the copy cursor pushes a fresh snapshot
(`copy_routed.c`, `dist_table.c`), `REPEATABLE READ` and `SERIALIZABLE` are refused, and the count that the copy must
equal is taken under the lock, so a mismatch is an error and not a silent loss. The five mutants that undo it (no
`READ COMMITTED` check, `RowExclusiveLock` instead of `ExclusiveLock` for distributed and for reference tables, a stale
snapshot for the distributed copy and for the reference copy) were all killed by t/150 (M23 to M27).

For the online variant: no committed change was lost in the cases I ran, except through the capture defects found
(P9B-02 for float keys, P9B-04 for a dropped trigger). What I tried, and how often:

| Case | Result |
|---|---|
| Transactions open at the start (update, delete, delete and insert, insert; reference table; online) | t/150 passes; mutants M23 to M27 killed. Not repeated by me. |
| Prepared transaction open at the start of the online call | The call fails after about 3 s with "could not lock table ... in SHARE ROW EXCLUSIVE mode" and is undone; the prepared transaction's rows were all there afterwards (1 run). See P9B-07. |
| Prepared transaction open at the switch | t/146 case C passes. |
| Subtransactions (`SAVEPOINT`, `ROLLBACK TO`) | Model comparison X22: a rolled-back subtransaction left no trace. |
| `INSERT ... ON CONFLICT DO UPDATE` and `DO NOTHING`, `MERGE`, `UPDATE` that changes the primary key, `UPDATE` that changes the distribution column, `DELETE`, insert then delete in one transaction, a 2 MB value, a prepared transaction committed before the switch, 101 inserted rows | X22 (1 run): the live table and a twin table that received the same statements were equal in both directions (0 rows differ of 3063), `verified = ok`. The serial column `ser` was not compared. |
| `COPY` | t/145 and t/146 case F pass. My own `COPY` in X22 failed on a quoting mistake of mine and was not repeated. |
| `TRUNCATE` | Refused while an online operation runs (X4b), as designed. In the blocking call a queued `TRUNCATE` is decided after the lock (t/150, passes). |
| Triggers on the source | The report refuses user triggers (t/140). Not tried by me. |
| Sequences and identity | See P9B-12 and the SEQ run below. |
| TOASTed and large values | 96 MB value written during capture: fine. A value over 256 MB: the write fails (P9B-08). |
| Long-running writer (8 s) at the start of the online call | The call fails after 3 s and is undone (X20, 1 run). See P9B-07. |
| Writers in `READ COMMITTED` across the switch: an `UPDATE`, a `DELETE`, and a transaction that read first, all queued behind the switch | X19 (1 run): all three applied to the new table (upd 1, deleted 1, upd2 1, 49 rows). |
| Float keys changed by a writer with `extra_float_digits < 1` | Lost (P9B-02). |
| Capture trigger dropped by the table owner | Lost (P9B-04). |

The narrow window of P9A-02 (a statement that touches a plain table and a distributed table together and propagates,
meeting a call that starts on the plain table in the gap between the release of the table lock and the catalog lock):
I read it, did not reproduce it, and did not find a way for a client to hit it that is not a race between a DDL
statement and an administrator's call on the same table. Reasoning, not an experiment:

- It needs a statement that propagates and names a plain table. The only such statement in the design is a foreign key
  from the plain table to a distributed table, which the hook refuses when the cluster has several shard groups, so
  the window exists in a cluster with one shard group.
- The gap is the time between `release_waited_tables()` and `kingres_shard_lock_catalog()` in one backend: a few
  statements of C code, no I/O.
- The effect is on a DDL statement, not on rows: the statement keeps its decision made before the call changed the
  table, so it runs locally against a table that has become partitioned and is propagated against a catalog that now
  lists it.
- I rate it low. The README does not mention it at all (P9B-12).

### 2. Is the online engine correct?

Capture and replay order. The replay does not depend on order: for each logged key it sets the destination to the
current row of the source, and entries are deleted by their own `seq`, so an entry whose transaction commits late is
taken by a later chunk. I found no loss from ordering. I found three problems in what the key can express and in
what the replay needs: P9B-02 (the key is written through `to_jsonb(NEW) ->> key`, which loses digits for float keys
under a session setting), P9B-03 (a chunk boundary can make the replay violate a unique index that is not the key) and
P9B-04 (the trigger can be dropped by its table's owner).

A replay that falls behind: the catching-up phase reports "the backlog does not shrink" and the switch attempt gives up
at `rebuild_switch_timeout`, releasing its locks. I read this; I did not drive a workload faster than the replay.
t/145 uses four writers only.

The switch under load: writers that queue behind the lock go on against the new table (X19); readers are not blocked
until the rename; a prepared transaction or a long reader makes the attempt give up with the holder named (t/146 C and
D). The wait for the lock in the first attempt phase is the NOWAIT loop of a quarter of `rebuild_switch_timeout`
(2.5 s by default) during which nothing is queued, then one queued wait of `rebuild_lock_timeout`. Passing.

Undo at every phase and crashes: t/147 and t/148 pass on both majors. Mutants that break the undo (no release of the
claim, no drop of the shadow, no drop of the triggers; M13, M14, M15) and the cancel handling (M17, M22) are killed.
What I added: the cancelled wrapper whose session stays (P9B-05), the undo after the session is ended (confirmed: the
worker undid the operation 11 s after the session ended with `node_timeout = 3 s`, that is after `2 x node_timeout`),
and an unreachable-node case is covered by t/147, not by me.

Two calls at once: a blocking `create_distributed_table`, a second online call and `create_reference_table` on the
source of a running online operation are all refused with blockers (the capture trigger, `name_in_use` for the
shadow's partitions); `DROP TABLE` is refused by the guard (X14, 1 run). t/148 covers the group claim.

A DDL change during the call: see P9B-04 (guard gaps). `ALTER TABLE ... DISABLE TRIGGER ALL` is refused (X5). `GRANT` is
accepted and is carried (the new table has the added privilege). `COMMENT` was not run.

Disk-full-like failures: not simulated (see What I did not examine).

### 3. Do P9A-02 to P9A-07 hold?

- P9A-02 (queued statements): the data effect holds (t/150, and mutants M24/M25 killed). The fix has a side effect:
  P9B-06 (a role that has only `SELECT` can hold `ROW EXCLUSIVE` on a table through a harmless `GRANT`). The remaining
  window is in the design and not in the README (P9B-12).
- P9A-03, P9A-04 (new report codes): pg_regress `distribute_rows`, `online_refusals` and t/150 pass on both majors. I did
  not write a test per code.
- P9A-05 (code of the owner run as superuser): resolved only for functions that the table's defaults, constraints and
  indexes reference directly. **Not resolved for a domain whose CHECK calls the owner's function, nor for an operator
  behind an index expression**: P9B-01.
- P9A-06 (sequence state): holds as the README states it (SEQ run). The state of a node's sequence is the largest value
  of that node's own rows, so on a distributed table the nodes start from different numbers (P9B-13).
- P9A-07 (warnings): attribute options are a warning (`not_carried`); regress passes.

### 4. Security

A role without superuser cannot call any function of phase 9 (16 functions tried, including the internal ones: all
"permission denied"), cannot read `kingres.table_operations` or the log table, and cannot create a trigger with
`internal_capture_trigger` on a table of an operation (X17). The capture trigger is `SECURITY DEFINER`, pins its
`search_path`, checks the log name against `^kingres\.tlog_[0-9]+_[0-9]+$` (that check is not exercised by any test:
mutant M20 survived) and inserts only key columns.

Two ways to cross a boundary that I found: P9B-01 (a non-superuser's function runs as superuser during the copy, and
the role became superuser on all three nodes) and P9B-06 (a lock a role could not take before). A third, smaller one:
P9B-09 (column privileges stay on the retained old copy).

### 5. Mutants

See the table below: 27 mutants, 18 killed, 9 survived (3 equivalent or performance-only, 6 point at untested rules).

### 6. Is the README honest about the window, the refusals and the cost?

Partly.

- The window of P9A-02 is not in the README at all.
- "whose creation waits for every writer" (README, "Transactions that run at the same time") is not what happens: the
  capture waits about `rebuild_switch_timeout / 4 + rebuild_lock_timeout` (about 2.7 s by default) and then fails
  (P9B-07).
- The README lists the statements the DDL guard refuses (ALTER TABLE, RENAME, DROP, CREATE INDEX, CREATE TRIGGER,
  TRUNCATE) and says the capture can only be bypassed by a superuser who edits the catalog; `DROP TRIGGER` by the owner
  is another way (P9B-04).
- The refusals are listed in detail and match the tests (that part is good), and the `REPEATABLE READ` refusal is stated.
- The cost to writers (one insert per change) is stated. The disk use (about twice the table with indexes on the
  destination servers, three times with the retained copy) is in the design's "Known limits" and not in the README; the
  WAL cost is in neither. A write of a row with a value over 256 MB fails during the capture (P9B-08), and the README does
  not say so.
- The sequence double-draw (design, "Identity and owned sequences") is not in the README.

## Findings

Count: high 2, medium 4, low 4, note 3. No critical.

### P9B-01 (high): the call runs a non-superuser's code as superuser through a domain or an operator; P9A-05 is only half fixed

- File and function: `contrib/kingres_shard/kingres_shard--0.7.sql`, `kingres.table_change_report()` (the block that names `function_owner_not_superuser`, about line 6551); the copy runs in `src/copy_routed.c` / `src/dist_table.c` as the caller.
- Steps (PostgreSQL 18 and 19, 3 nodes, the roles and the objects created on every node):
  1. As superuser create role `evil6` (no superuser), give it `CREATE` on `public`.
  2. Create `public.chk6(int)` (plpgsql, no `SECURITY DEFINER`) owned by `evil6`: `IF current_setting('is_superuser') = 'on' THEN EXECUTE 'ALTER ROLE evil6 SUPERUSER'; END IF; RETURN true;`.
  3. `CREATE DOMAIN public.d6 AS int CHECK (public.chk6(VALUE))`, owned by `evil6`; `CREATE TABLE public.t6 (id public.d6 PRIMARY KEY, v text)` with 10 rows, owned by `evil6`.
  4. As superuser: `SELECT * FROM kingres.table_change_report('distribute', 'public.t6', '{"column":"id"}')`, then `SELECT kingres.create_distributed_table('public.t6', 'id')`.
- Expected: a blocker `function_owner_not_superuser` (or another refusal), as for a function used directly by a CHECK, default or index.
- Actual: the report is empty; the call succeeded; `rolsuper` of `evil6` was `f` before and `t` on all three nodes after (1 run on 18, 1 run on 19).
- A second path, same cause: an operator owned by `evil6` used in an index expression (`CREATE INDEX ... ((a ### a))`). The report shows only a note; during the copy the function ran with `is_superuser = on` (it tried `ALTER ROLE` and was stopped only by "not allowed in a non-volatile function", which an attacker avoids by calling a volatile helper function owned by the same role; I did not build that). 1 run on each major.
- Cause: the check follows `pg_depend` rows from the table's defaults, constraints and indexes to `pg_proc` only. It does not follow a domain's constraints and defaults (`pg_constraint.contypid`, `pg_type.typdefaultbin`), the function behind an operator (`pg_operator.oprcode`), casts, or a function called by one of those functions. The capture trigger has the same exposure in a smaller form: it casts the key to the key column's type inside a `SECURITY DEFINER` function, so a domain over a key column runs its CHECK as the extension owner for every writer (not tried).
- Precondition: an administrator (the control plane acting for a panel administrator) distributes a table owned by an untrusted role. That is the case P9A-05 was about.

### P9B-02 (high): the capture trigger loses precision of a float key; the change is not replayed

- File and function: `kingres--0.7.sql`, `kingres.internal_capture_trigger()` (the `($1 ->> key)::type` expressions built in `internal_op_capture`).
- Steps (18 and 19, 3 nodes):
  1. `CREATE TABLE fx (k float8 PRIMARY KEY, v text)`; insert `(0.1::float8 + 0.2::float8, 'orig')`, `(1.5,'x')`, `(0.1::float8 + 0.7::float8, 'orig2')`.
  2. `begin_table_operation('distribute', 'public.fx', '{"column":"k","bucket_count":12}')`; `table_operation_advance(op, 'copying')` (the copy is done).
  3. In another session: `SET extra_float_digits = 0; UPDATE fx SET v = 'changed' WHERE k = 0.1::float8 + 0.2::float8; DELETE FROM fx WHERE k = 0.1::float8 + 0.7::float8;`.
  4. `table_operation_advance(op, 'cleaning')`.
- Expected: the new table has `(0.30000000000000004, 'changed')` and no 0.7999999999999999 row.
- Actual: the log holds `U|0.3` and `D|0.8` (digits cut by `float8out` under the writer's setting); the replay looked up keys that do not exist. The new table has `(0.30000000000000004, 'orig')` and the deleted row `(0.7999999999999999, 'orig2')` is back. The verification said `failed` in this run. The default `extra_float_digits` is 1 (shortest exact output), so only a session that lowers it is affected; some clients and drivers do.
- Why it matters more than "detected": the verification reports `skipped`, not `failed`, as soon as one row anywhere was written after the swap (`internal_op_clean`: a difference with `newer > 0` is `skipped`). Under real load the loss is therefore silent.
- Not tried: float4, interval keys with a non-default `IntervalStyle` (the trigger casts back inside the writer's session, so it should round trip), `money`.

### P9B-03 (medium): the replay can fail with a unique violation on an index other than the key, and then cannot progress

- File and function: `kingres--0.7.sql`, `kingres.internal_replay_chunk()` (a chunk deletes and re-inserts the keys of its own entries only).
- Steps (18; 19 reached the same state):
  1. `CREATE TABLE ux (tenant int NOT NULL, id int NOT NULL, email text, v text, PRIMARY KEY (tenant,id), UNIQUE (tenant,email))`; rows `(1,1,'a')`, `(1,2,'b')`.
  2. Begin the operation, advance to the end of `copying`.
  3. Then: `UPDATE ux SET v='y' WHERE id=2;` `UPDATE ux SET email='z' WHERE id=1;` `UPDATE ux SET email='a' WHERE id=2;` (three log entries: B, A, B).
  4. `SET kingres_shard.rebuild_batch_rows = 1; table_operation_advance(op, 'catching_up')`.
- Expected: the replay finishes.
- Actual: `duplicate key value violates unique constraint "ux_b0008_tenant_email_key"`, `Key (tenant, email)=(1, a) already exists`. The chunk for key B inserts B with email `a` while the shadow still holds A with email `a`; A's entry is in the next chunk. The same chunk order recurs on every retry, and the final replay in the switch has the same defect. The operation stays in `catching_up` until it is cancelled (cancel worked, 1 run). With the default batch of 10000 entries it needs a backlog over 10000 entries and values that move between rows (rotating slugs or usernames), which a loaded table can have.
- Loss: none (the call fails and is undone by the wrapper); the online call is unavailable for such a table under such a workload.

### P9B-04 (medium): the DDL guard has gaps; `DROP TRIGGER` of the capture trigger loses writes, silently once anything is written after the swap

- File and function: `src/utility.c`, `refuse_ddl_on_claimed_table()` (covers `ALTER TABLE`, `RENAME`, `CREATE INDEX`, `CREATE TRIGGER`, `TRUNCATE` and `DROP TABLE` only: `DropStmt` of other kinds and `AlterObjectSchemaStmt` are not looked at).
- Steps for `DROP TRIGGER` (18, as the table's owner, a non-superuser; 18 and 19 as superuser):
  1. Online operation on a table owned by `ownr`, advanced to the end of `copying`.
  2. As `ownr`: `DROP TRIGGER kingres_capture_<op> ON public.dt2;` then `INSERT` a row, `UPDATE` a row, `DELETE` a row.
  3. Advance to `switching`, write one more row to the new table, advance to `cleaning`.
- Expected: the `DROP TRIGGER` is refused like `ALTER TABLE ... DISABLE TRIGGER` is ("is being rebuilt by table operation N").
- Actual: accepted. The new table has 21 rows without the inserted row, without the update, with the deleted row; the operation ended `done` with `verified = skipped`; `check_cluster()` returned no row. The old copy (kept for `table_cleanup_delay`) holds the right rows. Without the "one more write" the verification said `failed` and the call raised an error, but the switch had already committed and nothing is rolled back (1 run each way; 1 run on 19 for the `failed` case).
- The design and the README say this needs "a superuser who edits the catalog". It needs the table's owner and one statement.
- Second gap, `ALTER TABLE ... SET SCHEMA` on the source (18 and 19): accepted; the next switch attempt fails with `relation "other5.kingres_shadow_N" does not exist`, the operation sits in `switching` and cannot finish; the wrapper undoes it. Low impact, confusing error.
- Not tried: `DROP INDEX` of a unique key index, `CREATE RULE`, `CREATE POLICY` (all outside the guard's list).

### P9B-05 (medium): a cancelled wrapper whose session stays leaves an operation that nobody can cancel

- File and function: `kingres--0.7.sql`, `create_distributed_table_concurrently()` and `table_operation_advance()` (`EXCEPTION WHEN OTHERS` does not catch `query_canceled`, so neither the wrapper's undo nor `internal_op_unlock` runs); `src/rebuild.c`, `kingres_internal_op_lock()` (a session-level lock).
- Steps (18, 3 nodes, 1.5 million rows so that the copy takes a few seconds):
  1. In a psql session: `SET statement_timeout = 1500; SELECT kingres.create_distributed_table_concurrently('public.cx','id');` The statement is cancelled. Keep the session open.
  2. From a second session: `kingres.internal_op_alive(op)` is `t`; the capture trigger and the log are still there; `TRUNCATE public.cx` is refused ("is being rebuilt").
  3. `kingres.resume_table_operations()` returns 0 (the live mark is held). `kingres.cancel_table_operation(op)` answers only with NOTICE "was asked to stop; the session that runs it has not yet noticed", and the phase stays `copying` (checked after a 9 s wait).
  4. End the idle session (`pg_terminate_backend`): `internal_op_alive` is `f`; `resume_table_operations()` returned 0 at 5 s and 1 at 11 s (`node_timeout = 3`), and the operation became `failed`, "interrupted; undone by kingres.resume_table_operations()".
- Expected (README: "The two wrapper functions undo what fails"; cancel "undoes an operation that has not switched"): the operation is undone at the cancel, or at least `cancel_table_operation` works from another session.
- Actual: until the client closes the session (or an administrator ends it), every writer of the table pays the capture and the table cannot be changed. A client that stays connected after Ctrl-C (psql, a pooler) does exactly that. Default grace after the session ends: `2 x node_timeout` = 120 s.

### P9B-06 (medium): a role with only `SELECT` can hold `ROW EXCLUSIVE` on a table and block administrators' calls

- File and function: `src/utility.c`, `wait_for_table_operation()` (P9A-02; the lock is taken for any role that has any table privilege, before the statement's own permission check, and a statement on a plain table keeps it to the end of the transaction).
- Steps (18): role `ro_l` has `SELECT` on `public.lt`. In a session: `SET ROLE ro_l; BEGIN; GRANT SELECT ON public.lt TO PUBLIC;` (a warning "no privileges were granted"; stay in the transaction).
- Observed: `pg_locks` shows `RowExclusiveLock` held by that backend on `lt`. While it is held: `CREATE INDEX` by the owner fails after `lock_timeout`; `kingres.create_distributed_table('public.lt','id')` waits until `statement_timeout` cancels it; `create_distributed_table_concurrently` fails with "could not lock table ... in SHARE ROW EXCLUSIVE mode, Held by: pid ...". After `ROLLBACK` the call succeeds. (1 run.) `REVOKE` behaves the same; `ALTER TABLE`, `DROP`, `CREATE INDEX`, `RENAME`, `CREATE TRIGGER` and `TRUNCATE` by that role left no lock (they fail before the lock in the cases I ran).
- The same role cannot take that lock with `LOCK TABLE ... IN ROW EXCLUSIVE MODE` ("permission denied"), and the README (section on reference-table write locks) says this class of lock is kept away from roles for that reason (S3-03). I did not run the same `GRANT` on a server without the extension, so "the extension adds it" rests on the code and on the denied `LOCK TABLE`.
- Effect: a denial of service on administrators' table operations by any role that can read the table.

### P9B-07 (low): the online capture waits about 2.7 s for writers, then fails; the README says it waits for every writer

- File and function: `kingres--0.7.sql`, `kingres.internal_lock_table()` (NOWAIT loop of `rebuild_switch_timeout / 4`, then one queued wait of `rebuild_lock_timeout`) called by `internal_op_capture()`.
- Steps (18): a session runs `BEGIN; UPDATE public.lw ...; SELECT pg_sleep(8); COMMIT;`; one second later `SELECT kingres.create_distributed_table_concurrently('public.lw','id')`.
- Expected (README: "the capture trigger ... whose creation waits for every writer"): the call waits, or the README states the bound.
- Actual: after 3 s "could not lock table public.lw in SHARE ROW EXCLUSIVE mode ... Held by: pid ..." (with the holder named, which is good); the whole call is undone, shadows included. Same with a prepared transaction that holds the table (1 run each). A table that always has a writer longer than 2.7 s cannot be rebuilt online with this call; the phase function `table_operation_advance` is not retried by the engine in this phase (the control plane may).

### P9B-08 (low): while an online operation runs, a write of a row with a value over 256 MB fails

- File and function: `kingres.internal_capture_trigger()` (`to_jsonb(NEW)` of the whole row).
- Steps (18): during capture `INSERT INTO bx VALUES (5001, repeat(md5('y'), 9000000))` (288 MB). A 96 MB value was written without trouble (1 run).
- Actual: "string too long to represent as jsonb string, jsonb strings cannot exceed 268435455 bytes", from the trigger. A `text` or `bytea` value can be up to 1 GB, so a statement that works without the operation fails with it. The row is also serialized for every change, so memory and time grow with the row size.

### P9B-09 (low): column privileges stay on the retained old copy

- File and function: `kingres--0.7.sql`, `kingres.internal_retire_table()` (revokes table-level grantees found in `relacl` only; `attacl` is not looked at).
- Steps (18): role `col9` has `SELECT (secret)` on a table; distribute it online; `has_column_privilege('col9', 'kingres.kingres_old_N', 'secret', 'SELECT')` is `t`, and `has_schema_privilege('col9','kingres','USAGE')` is `t`.
- Expected (README: the old copy is "readable by its owner only"): not readable by `col9`.
- Actual: readable for `table_cleanup_delay` (10 minutes by default), also after an administrator revokes the privilege on the new table (1 run).

### P9B-10 (low): rules that no test exercises (surviving mutants)

Six survivors point at rules that are in the code and not in any test: M09 (copy chunks are `ON CONFLICT DO NOTHING`), M10 (the switch attempt locks the operation row, so cancel and switch are serialized), M11 and M12 (the worker leaves a live operation alone, and one written a moment ago), M20 (the capture trigger checks its log name), M21 (an answer lost after the switch committed is a switch). See Mutants. M09 also means that a repeated copy chunk after a crash, claimed harmless in the design, is not tested.

### P9B-11 (note): the verification cannot see a loss once anything is written after the swap

`internal_op_clean` turns every difference into `skipped` when the new table has a row newer than the swap. One write anywhere hides any amount of loss (P9B-02, P9B-04 under load). The README states the rule ("exact for a table nobody wrote since the swap"); the consequence for the claim "the verification detects the rest" is not stated. An exact check would compare a digest taken under the lock or compare the logged keys (the cost the design rules out); a cheaper improvement is to compare the count of the old copy with the new table's count minus the rows newer than the swap.

### P9B-12 (note): the README leaves out three things the design says

- The remaining window of P9A-02 (see answer 1).
- The sequence double-draw: "a value drawn by an explicit `nextval()` in the milliseconds between the read of the state and the rename would be given twice" (design only).
- The disk and WAL cost of the online variant (design: about twice the table with indexes, three times with the retained copy; WAL not stated).

### P9B-13 (note): after distribution the sequences of a table are per-node maxima

SEQ run (18, 3 nodes, 60 rows): a serial column whose sequence was at 1000 stays at 1000 on the map node and goes to 55 and 60 on the others; a sequence at 5 over a column with maximum 60 went to 59, 55 and 60 (node 1 got 59 because its own rows end there); an identity column distributed online went to 60, 55 and 60. A following `INSERT` produced 60, 61 and 62 for the three inserts I made through the three nodes, and id 60 already existed on another node. That is what the README says ("raised to the largest value of the node's own rows") and it holds for P9A-06; the consequence, values of a serial column that repeat across nodes, is only harmless when the primary key includes the distribution column and the repeated value goes to another tenant.

## Mutants

27 one-line mutants, each built clean with `-Werror` into a private copy of the PostgreSQL 18 install and judged by t/144 to t/150 (one run each, `PG_TEST_TIMEOUT_DEFAULT = 45`, 330 s cap per mutant; a cap that fires is a hang and counted as killed). PostgreSQL 19 was not run for the mutants. The SQL mutants were applied to `kingres_shard--0.7.sql` only (the file a new install uses; `0.6--0.7.sql` was not mutated).

| # | Mutant | Result |
|---|---|---|
| M01 | capture: `SHARE ROW EXCLUSIVE` to `ROW EXCLUSIVE` in `internal_op_capture` | survived; equivalent: `CREATE TRIGGER` takes `SHARE ROW EXCLUSIVE` itself, so the `LOCK TABLE` is redundant |
| M02 | switch: `EXCLUSIVE` to `ROW EXCLUSIVE` on the source | killed (t/146) |
| M03 | capture: an update does not log the old key | killed (t/146) |
| M04 | capture: trigger without `DELETE` | killed (t/144, t/146) |
| M05 | capture: trigger not `ENABLE ALWAYS` | killed (t/146) |
| M06 | replay: the delete of the key at the destination never matches | killed (t/144, t/145, t/146) |
| M07 | replay: entries are not removed from the log | killed (hang, cap) |
| M08 | switch: the final replay is skipped | killed (t/145, t/146) |
| M09 | copy chunk: `ON CONFLICT DO NOTHING` off | **survived** |
| M10 | switch attempt: no `FOR UPDATE` on the operation row | **survived** |
| M11 | resume: ignores the live mark | **survived** |
| M12 | resume: ignores "written a moment ago" | **survived** |
| M13 | undo: the claim is not released | killed (t/147, t/148) |
| M14 | undo: the local shadow is not dropped | killed (t/147, t/148) |
| M15 | undo: the triggers are not dropped | killed (t/147, t/148) |
| M16 | retire: old copy granted to `PUBLIC` instead of revoked | killed (t/144) |
| M17 | advance: ignores `cancel_requested` | killed (hang, cap) |
| M18 | advance: does not wait for the backlog before switching | survived; performance only (the switch replays the rest under the lock) |
| M19 | switch: "log not empty after the final replay" check off | survived; equivalent while the replay is correct |
| M20 | capture trigger: log name check off | **survived** |
| M21 | advance: "the switch committed although the answer was lost" off | **survived** |
| M22 | cancel after the switch: the notice branch off | killed (t/148) |
| M23 | blocking: `READ COMMITTED` requirement off | killed (t/150) |
| M24 | blocking: `ExclusiveLock` to `RowExclusiveLock` | killed (t/150) |
| M25 | reference: `ExclusiveLock` to `RowExclusiveLock` | killed (t/150) |
| M26 | distributed copy: stale (active) snapshot | killed (t/150) |
| M27 | reference copy: stale (active) snapshot | killed (t/150) |

Killed 18, survived 9: three are equivalent or performance only (M01, M18, M19) and six are real gaps (M09, M10, M11, M12, M20, M21; P9B-10). All five mutants of the P9A-01 fix (M23 to M27) are killed, and so are the capture and replay mutants that change what is copied (M02 to M08).

## What I did not examine

- Disk-full failures, a full WAL disk, and an out-of-memory backend: not simulated.
- A node lost at every phase: I relied on t/147 and t/148 (passed on both majors) and added only the session-kept case (P9B-05). I did not kill or stop a node myself, and I did not test a standby promotion during an operation (phase 5's part).
- A replay that cannot keep up with a heavy write rate (no workload above four writers, no measured blocked time; the design's blocked-time targets are unmeasured by me).
- Mutants on PostgreSQL 19, mutants of `0.6--0.7.sql`, and mutants of the C code of the switch (`rebuild.c`) and of the sender.
- Key types other than float and the ones in t/149: float4, interval with another `IntervalStyle`, `money`, domains and enums as keys, and nondeterministic collations on a key (the replay compares with the column's `=` against an untyped array value).
- `DROP INDEX` of a unique key index, `CREATE RULE`, `CREATE POLICY`, `COMMENT` during an operation; the capture trigger's exposure to a domain key (P9B-01, second form).
- `COPY` as a racing writer in t/150's blocking race (t/145 and t/146 cover `COPY` for the online variant); a prepared transaction open at the start of the blocking call.
- The control plane (`kingresd`) steps for phase 9 and the web console; the first review's findings P9A-08 to P9A-10 beyond what t/150 checks; the P9A-02 window (reasoned, not reproduced).
- The comparison of the sequences' serial column in the model test X22, and `GRANT` on sequences.
- Whether `GRANT` takes a table lock on a server without the extension (P9B-06 rests on the code and on the denied `LOCK TABLE`).
