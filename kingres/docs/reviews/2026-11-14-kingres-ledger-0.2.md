<!-- SPDX-License-Identifier: AGPL-3.0-only -->

# Review of kingres_ledger 0.2 (updatable tables, workers, records, upgrade)

Reviewer: independent, adversarial. Date: 2026-11-14.
Tree reviewed: `contrib/kingres_ledger/` as of the uncommitted working tree, which
was committed as `bdf029f334` while the review was running (no content change
was seen: `git status` of the directory became clean, and the installed
`kingres_ledger--0.2.sql`, `--0.1--0.2.sql`, `--0.1.sql` and `.control` are
byte-identical to the tree).

## Summary

No way was found to change, add or remove a current row or a past version of an
updatable ledger table so that `ledger_verify` reports clean and an earlier
digest still verifies (finding area 1). Every DML path that was tested is
captured correctly (area 2), the history write path is sound including TOAST
(area 3), the upgrade produces exactly the objects of a fresh install (area 7),
and the proof parser never raised an error in 432 mutated proofs (area 8).

The defects found are in the protection of the new machinery and in the worker:

| ID | Severity | Title |
|---|---|---|
| L2-01 | High | The `kl_internal_ddl` window switches the protection hook off for every statement that runs inside it, so a superuser's event trigger can grant, re-own and replace ledger objects with plain SQL |
| L2-02 | Medium | After `ALTER TABLE ... OWNER TO` / `REASSIGN OWNED` the former owner keeps `SELECT` on the history table and the new owner gets none |
| L2-03 | Medium | The database owner (not a superuser) can stop the automatic block building with per-database settings the worker does not override |
| L2-04 | Medium-low | `ledger_verification.problems` is readable by PUBLIC and carries row hashes, table names and transaction ids (the class of L-09 of the 0.1 review) |
| L2-05 | Low | Per-transaction caches survive a subtransaction rollback of `ledger_enable`; later DML of the table fails for the rest of the transaction |
| L2-06 | Low | Launcher: list leak per round, one worker per database every sixth round, one slow database delays all others |
| L2-07 | Low | The standalone verifier rejects `D` proofs that the server issues and accepts |
| L2-08 | Low | Records of operators: unbounded permanent growth by any `EXECUTE` holder; a cancelled run leaves no record; weak test assertions |

## Scope and method

Read completely: `src/capture.c`, `src/kingres_ledger.c`, `src/worker.c`,
`src/row_hash.c` (diff and the new functions), `src/proof.c` (diff and the
parsing helpers), `src/protect.c` (diff, `kl_check_add_column`, classification),
`kingres_ledger--0.2.sql` (lines 120-2148), the head of
`kingres_ledger--0.1--0.2.sql`, `README.md` (updatable sections, limits),
`t/009_worker.pl`, the design note `ledger-0.2.md` including its notes, and the
0.1 review (L-01 to L-12). Not read line by line: `src/merkle.c` (unchanged),
`src/protect.c` outside the diff, the 0.1 parts of the SQL scripts, the other TAP
tests and the regression `.sql` files.

Run: a copy of the installation at `~/kw-review-ledger/install` (the original
was not touched), a throwaway cluster on port 57401 with
`shared_preload_libraries = 'kingres_ledger'`, a streaming standby on 57402. No
rebuild, no reinstall. Everything was removed at the end. Experiments:

- capture: BEFORE trigger returning NULL and modifying NEW, INSTEAD OF trigger on
  a view, `ON CONFLICT DO UPDATE` on a partial unique index, `MERGE ... RETURNING`
  with update, delete and insert actions, foreign keys in both directions
  (`SET DEFAULT`, `CASCADE` update and delete into updatable children),
  `UPDATE ... FROM`, `DELETE ... USING`, data-modifying CTE, `WHERE CURRENT OF`,
  tid scan, savepoints and rollback to savepoint, SERIALIZABLE and REPEATABLE
  READ, 200000-row update and 100000-row delete (verify clean);
- history write path: 640 kB compressed (lz4) text, 160 kB `EXTERNAL` text and
  100 kB bytea columns, then update, delete, `VACUUM (FREEZE)`, read the history
  (data intact, own TOAST relation, no pointer to the source's TOAST table);
- ADD COLUMN with many types in one statement (numeric typmod, varchar with
  collation, domain, enum from another schema, timestamp(3), array, bit varying),
  `IF NOT EXISTS`, combined with `SET (fillfactor)`, domains with a default
  (direct and domain over domain: rejected), a domain with a check (rejected by
  the rewrite guard even on an empty table), `event_triggers = off` and its repair;
- 25 attacks on the history table and 25 on the catalog tables and the status
  view, schema and owner statements (see "Examined and found sound");
- tamper cases performed by switching the triggers off in `pg_trigger` and
  reconnecting (the relcache must be fresh): current row changed, old version
  put back as a current row, history row changed, history row removed;
- 2PC: prepare two transactions that update and delete, crash the server
  (`stop -m immediate`), commit one and roll back the other after recovery,
  build, verify; standby: replay of history rows, `ledger_verify` on the standby,
  promotion in the middle of a transaction of the old primary;
- proofs: `I` and `D` proofs, 4- and 5-argument forms, flipped `op`, and a
  mutation fuzz of one `D` proof (delete or replace every member and every array
  element with 15 hostile values: 432 cases);
- LATIN1 database with the role `Jos` followed by the two bytes `C3 A9` (stored
  in a UTF-8 cluster), digest and `I` and `D` proofs, checked by the server and by
  `kingres-verify` built with Go 1.27 (see area 8);
- upgrade: 0.1 database with two tables, 50 rows, blocks, a digest and pending
  transactions, `ALTER EXTENSION ... UPDATE`, then a catalog fingerprint
  (columns with defaults, storage, identity, ACLs, comments, constraints,
  triggers with arguments and state, functions with source hash, owner, ACL,
  config, volatility, event triggers with tags and owner, sequences and their
  ownership, extension membership of every object, GUC definitions) compared with
  a fresh 0.2 install: no difference; an old session kept running across the
  upgrade; a 0.1 digest verifies after it;
- worker: block building in two databases, a database without the extension,
  database-level settings of a non-superuser database owner, 60 extra databases
  for the launcher's memory.

Method note: direct edits of `pg_trigger` need a new backend before the relcache
sees them. A first attempt in one session produced "tampering" that was captured
as legitimate changes; the numbers above are from the reconnecting version.

## Findings

### L2-01 High: the `kl_internal_ddl` window disables the protection hook for everything that runs inside it

Files: `src/kingres_ledger.c:238-253` (`kingres_ledger_history_sync`),
`src/protect.c:1131` (`if (kl_internal_ddl) return;` at the head of
`kl_check_statement`).

What is wrong. The flag is global and boolean. While it is true, the hook lets
every utility statement of the backend through, not only the one `ALTER TABLE
<history> ADD COLUMN` that `history_sync` runs. Anything that executes while the
`SPI_execute` of that statement is in progress runs unprotected, and event
triggers do: the nested `ALTER TABLE` fires the `ddl_command_start` and
`ddl_command_end` triggers of the database (alphabetically before
`kingres_ledger_guard_end`), as the superuser that owns the extension (the guard
is `SECURITY DEFINER`). The window is entered by every `ADD COLUMN` on an updatable
table, by any role that owns one.

Result: a superuser, without editing a catalog and without switching event
triggers off, can do everything the hook exists to stop: grant `INSERT` on
`kingres.ledger_entry`, change the owner of a ledger catalog table, replace the
body of `kingres.ledger_verify`, `ledger_hook_active` or any other function of the
extension. In 0.1 the same statements are rejected for the superuser (the README
says the protections stop a superuser who does not edit catalogs). The 0.2
README does not say that this got worse. The event-trigger guard catches only
what it looks for (drops, rewrites, triggers, the defects list); privileges,
ownership and function bodies are protected by the hook only.

Reproduced. As the superuser, outside the window the statements fail:

```
grant insert on kingres.ledger_entry to alice;
ERROR:  cannot grant any privilege other than SELECT on table "ledger_entry": it is part of the ledger
alter table kingres.ledger_entry owner to alice;
ERROR:  cannot alter table "ledger_entry": it is part of the ledger
create or replace function kingres.ledger_hook_active() ... ;
ERROR:  cannot replace function "ledger_hook_active": it belongs to the ledger
```

Then, with one ordinary event trigger that acts on the second firing of
`ALTER TABLE` in the statement (the first is the user's statement, the second is
the nested one; an event trigger of type `ddl_command_end` named `aaa_evil` runs
before the guard, so the guard sees a consistent state afterwards):

```
create function public.evil() returns event_trigger language plpgsql as $$
declare n text := coalesce(current_setting('evil.n', true), '');
begin
  if tg_tag = 'ALTER TABLE' then
    if n = '' then perform set_config('evil.n', '1', true);
    elsif n = '1' then
      perform set_config('evil.n', '2', true);
      execute 'grant insert on kingres.ledger_entry to alice';
      execute 'alter table kingres.ledger_entry owner to alice';
      execute 'create or replace function kingres.ledger_hook_active() returns boolean language sql as $f$ select true $f$';
    end if;
  end if;
end $$;
create event trigger aaa_evil on ddl_command_end execute function public.evil();
alter table alice.acc add column extra9 text;   -- alice.acc is an updatable ledger table
```

Observed afterwards: `has_table_privilege('alice','kingres.ledger_entry','insert')`
is `t`, the owner of `kingres.ledger_entry` is `alice`, and `ledger_hook_active`
has language `sql` with body ` select true ` (a `ddl_command_start` trigger fails
instead: the nested guard sees a history table that has not been extended yet and
raises, which is why the trigger has to run at the end). A plain audit
event trigger that builds statements from `current_query()` or identifiers is
enough for the same result by accident or by injection.

Fix. Replace the boolean by a narrow permission and do not use SPI: call
`AlterTable` (or `ProcessUtility` with a prepared `AlterTableStmt`) directly from
`history_sync`, so that no other statement or event trigger runs inside; if SPI
is kept, store the history relation OID and require, in `kl_check_statement`, that
the statement is an `AlterTableStmt` on exactly that relation whose commands are
only `AT_AddColumn`, and clear the flag the first time it matches (so that a
nested statement is checked normally). Add a test with a superuser event trigger
at both `ddl_command_start` and `ddl_command_end`.

### L2-02 Medium: the former owner keeps reading the history after an ownership change; the new owner cannot

File: `kingres_ledger--0.2.sql:1200-1203` (the only `GRANT SELECT` on the history
table, made once in `ledger_enable`); README line 86 says `ALTER TABLE ... OWNER
TO` works.

What is wrong. The history table holds the old versions of every row and
protects them with its own privileges (the README says so). The `SELECT` that
`ledger_enable` grants goes to the owner at the time of enabling. Nothing moves
it when the table changes owner. The hook allows `ALTER TABLE ... OWNER TO` and
`REASSIGN OWNED` on a ledger table.

Reproduced:

```
-- alice owns al.own (updatable); the superuser enables it, then:
alter table al.own owner to bob;
set role bob;   insert into al.own values (1,'bob-secret-1'); update al.own set secret='bob-secret-2';
select has_table_privilege('alice','al.own_ledger_history','select'),
       has_table_privilege('bob','al.own_ledger_history','select');   --  t | f
set role alice; select secret from al.own_ledger_history;             --  bob-secret-1
```

The former owner keeps reading every later old version, including data that
belongs to the new owner, and bypasses the table's row security and column
privileges (which is how the history is documented). The new owner cannot read
the history of their own table. The same follows from default privileges: a
`pg_default_acl` entry of the extension owner for the schema (an administrator's
convenience setting) is applied to the history table by `CREATE TABLE` and gives
PUBLIC or others `SELECT`; the README sentence "nobody else gets anything" is
then not true (not tested, reasoned from how `CREATE TABLE` applies default
privileges).

Fix. Either reject `OWNER TO` / `REASSIGN OWNED` of an updatable ledger table (the
hook already classifies the table), or move the grant in the event trigger on
`ddl_command_end` (revoke from the old owner, grant to the new one), and have
`ledger_enable` and the event trigger `REVOKE ALL` on the history table from
everybody first so that default privileges do not apply. Say it in the README.

### L2-03 Medium: the worker honours database-level settings that a database owner can set

File: `src/worker.c:408-416` (only `default_transaction_isolation`, `lock_timeout`
and `statement_timeout` are overridden).

What is wrong. `BackgroundWorkerInitializeConnectionByOid` applies the settings
made with `ALTER DATABASE ... SET`. The owner of a database, who need not be a
superuser, can set `transaction_timeout`, `default_transaction_read_only`,
`idle_in_transaction_session_timeout` and others. The worker overrides three
settings only.

Reproduced (role `dbo` owns database `w1`, interval 1 s):

```
alter database w1 set transaction_timeout = '1ms';        -- as dbo
insert into a values (6);                                    -- pending
```

The transaction stays in `ledger_pending` for as long as the setting stands (two
rounds of 3.5 s observed; it was built about 3 s after `alter database w1 reset
transaction_timeout`). Every round the log gets `FATAL:  terminating connection
due to transaction timeout` with the context `ledger_build_block()` and a
`background worker ... exited with exit code 1`. With
`default_transaction_read_only = on` the worker fails the same way with `cannot
execute INSERT in a read-only transaction`. The README says the window before a
block exists is bounded by the worker; a database owner can make it unbounded
while the worker keeps failing and logging, and the launcher keeps marking the
database dirty. Explicit calls of `ledger_generate_digest` still work, so this is
not tampering, but it defeats the stated purpose.

Fix. In the worker, force the safety settings with `PGC_S_OVERRIDE` before the
transaction starts: `transaction_timeout=0`, `idle_in_transaction_session_timeout=0`,
`default_transaction_read_only=off`, `search_path = pg_catalog`, and consider
`default_transaction_deferrable=off`. Log a failing database at a bounded rate.

### L2-04 Medium-low: `ledger_verification` is readable by PUBLIC and publishes row hashes and table names

Files: `kingres_ledger--0.2.sql:197-199` (grant to PUBLIC), `:2061-2078`
(`problems` stores `check_name`, `object`, `detail` of `ledger_verify` verbatim).

What is wrong. The 0.1 model (review L-09) keeps `ledger_entry` unreadable by
PUBLIC because a row hash of a low-entropy row can be confirmed by guessing the
row. The `table_rows` problems of `ledger_verify` are of the form `row hash <64
hex>: n row(s) in the table, m entry(ies) in the ledger`, `history_rows` problems
name transaction ids, sequence numbers and the table; the `object` is the schema
qualified table name, also for tables the reader may not see. A recorded run is
stored in a table every role can read. The specification asks for PUBLIC
readability of this table, but it did not consider the content.

Reproduced. After tampering with a history table, `ledger_verify_recorded()` run
by the superuser; `set role bob; select problems from kingres.ledger_verification`
returned the detail strings with the schema-qualified name of the table and the
transaction ids; the same code path writes the `table_rows` text with the row
hash (seen in the output of `ledger_verify` itself).

Fix. Store only counts and check names for PUBLIC (or put `problems` in a column
that is not granted to PUBLIC, or make the table readable only by roles that may
read `ledger_entry`); the rest of the table (state, counts, times) can stay public.

### L2-05 Low: caches survive a subtransaction rollback of `ledger_enable`

Files: `src/capture.c:98` (caches are reset only at top-level commit, abort and
prepare), `src/row_hash.c:383` (`kl_lookup_history`), the `kl_cached_relid` cache
of 0.1.

Reproduced:

```
begin; create table spt(id int);
savepoint s1; select kingres.ledger_enable('spt','updatable'); insert into spt values (1); update spt set id=2; rollback to s1;
select kingres.ledger_enable('spt','updatable');
insert into spt values (5);
update spt set id=6;
ERROR:  ledger trigger of table "spt" has table id 37, the ledger has 36 ...
```

The cached relid-to-table_id and history mapping of the rolled back registration
is used by the second one. It fails closed (the error stops the statement), so no
entry is wrong, but the transaction cannot use the table. Fix: also reset the
caches in the subtransaction callback on `SUBXACT_EVENT_ABORT_SUB`, or key the
caches by the registry row's xmin.

### L2-06 Low: launcher costs and a leak

File: `src/worker.c:300-335`.

1. `kl_connectable_databases()` allocates its list in the launcher's long-lived
   context; when the round is not a sweep (`targets` is built separately from the
   dirty OIDs) the list `all` is never freed. Observed: the launcher grew from
   10092 kB to 10292 kB of RSS over 150 busy rounds with 65 databases (about 1
   kB per round), so about 100-500 kB per day at the default interval; larger
   with more databases and a shorter interval. Free `all` in both branches.
2. Every sixth round the launcher starts one worker (a full backend start and
   connection) for every connectable database, with or without the extension. A
   cluster with a thousand databases pays a thousand connections per minute at
   the default interval. A database that cannot start a worker is retried each
   round.
3. Workers run one after another and the launcher waits for each. One database
   with a long block build (up to `max_block_transactions` = 100000
   transactions) delays all others by that time. This is the documented design,
   but the README does not say that.

### L2-07 Low: the independent verifier cannot check `D` proofs

`kingres/verifier/internal/proof/proof.go:114` rejects `op` other than `I`
(`FAILED at step parse: $.op: is "D", expected "I"`), while the server issues `D`
proofs (`ledger_inclusion_proof(rel, hash, 'D')`) and the README tells users that
a `D` proof proves removal. Reproduced with `kingres-verify proof` on a LATIN1
database's `D` proof (an `I` proof from the same database verified, all steps ok).
Either teach the verifier `D` or say in the README that `D` proofs can only be
checked by the server's `ledger_verify_proof`, which is a function of the database
being verified.

### L2-08 Low: records for operators; tests

1. `ledger_digest` and `ledger_verification` are append-only and cannot be pruned.
   Every call of `ledger_generate_digest` (also when nothing changed) and of
   `ledger_verify_recorded` adds a row, up to 100 problems of JSONB each. Any
   role that was granted `EXECUTE` (for example an auditor or a dashboard) can
   grow them without limit and nobody can remove the rows. Consider recording a
   digest only when the height changed and a retention rule for verification
   rows.
2. `ledger_verify_recorded` under `statement_timeout` is cancelled and no record
   exists (reproduced: `ERROR: canceling statement due to statement timeout`,
   row count unchanged), while `lock_timeout` is recorded as `failed`. A run that
   an operator cancels leaves no trace; the README says every run is recorded.
3. `t/009_worker.pl` checks the logs with `unlike($log, qr/kingres_ledger.*ERROR|ERROR.*kingres_ledger/)`.
   The regex works on one line; an `ERROR` of a failing worker does not name the
   extension (the message of L2-03 does not), so the assertion would pass if
   workers failed. The same holds for the standby assertions. The `failed` state
   of `ledger_verification` is untested (the author says so).
4. The document `ledger-0.2.md` has no "Where a reviewer should attack first"
   section, and neither has the README; the task text refers to one.

## Examined and found sound

- **Verification arithmetic.** Tested by tamper: a changed current row (reported
  as two `table_rows` lines: hash with 2 rows and 0 entries and hash with 0 rows
  and 2 entries), an old version put back as a current row (`table_rows`), a
  changed history row (`history_rows`: "does not hash to the row hash of its D
  entry", 3 findings), removed history rows ("D entry ... has no history row").
  By reading: duplicates are counted (`count(*)` against `sum(I=+1, D=-1)`), an
  `I` and a `D` that cancel cancel against both sides, a row updated back to an
  earlier value is a plain count, the row hash includes the `table_id`, so equal
  rows of two tables differ; the pairing is `(ledger_txid, ledger_seq)` against
  `D` entries of the same table only, `ledger_entry` has a key on that pair; a
  history row with a NULL key or a second row for a key is reported; `ledger_op` is
  checked; a `D` entry of an append-only table is reported (`entry_op`).
- **What is not detected** is what the README says: removal of a version from
  history and entries together is caught by the transaction root only after a block
  exists, and a consistent rewrite of entries, blocks and history only by an
  external digest. The README states both plainly (limits list, "Updatable
  tables").
- **Capture paths**: all listed in the summary, plus 2PC with crash recovery,
  standby replay, promotion with a transaction in flight on the old primary.
  `UPDATE ... SET x = x` writes `D` and `I`. Concurrent updates were not run
  here (the TAP test 008 covers them).
- **History write path**: external, compressed (lz4) and plain values; the history
  keeps its own TOAST copies (checked after `VACUUM (FREEZE)` and deletion of the
  source rows); column matching by name, the 63-byte name rule, the added
  columns; WAL logged (the standby has the rows); no index, trigger or rule is
  involved.
- **Protection of new objects**: 25 statements against the history table (index,
  rule, trigger, RLS, replica identity, options, owner, rename, set schema,
  default, not null, constraint, unlogged, tablespace, inherit, storage, statistics,
  drop column, child, partition, truncate, delete, update, insert, copy, grant other
  than `SELECT`, policy, disable trigger, drop trigger, drop table) were all
  rejected. Against the status view and the catalog tables: alter, rename, set
  schema, options, rules, triggers, revoke from owner, add or drop column, drop
  `NOT NULL`, rename, drop constraint, sequence owner, `GENERATED BY DEFAULT`:
  rejected. Allowed and harmless: `VACUUM FULL` of the history table (contents
  preserved), `COMMENT`, `CREATE STATISTICS` on it, `CREATE PUBLICATION` over it,
  `GRANT SELECT` on the history table or the status view, and
  `ALTER SEQUENCE ... RESTART` and `setval` on the identity sequences of the
  records tables (superuser only; they can only create gaps or make
  `ledger_generate_digest` fail on a duplicate key). Drops of schema,
  `DROP OWNED`, `DROP EXTENSION`, rename of schema `kingres`: rejected.
- **Privileges**: `search_path` is fixed on every function that runs SQL;
  `SECURITY DEFINER` on `ledger_capture`, `ledger_record_row`, `ledger_enable`,
  `ledger_build_block`, `ledger_generate_digest`, `ledger_verify`,
  `ledger_inclusion_proof` (both forms), `ledger_verify_recorded`, `ledger_guard`;
  PUBLIC cannot execute any of them except `ledger_guard` and the pure helpers;
  `ledger_status` is not readable by PUBLIC; the history table has no grant other
  than the owner's.
- **Upgrade**: no difference between an upgraded database and a fresh install in
  the fingerprint described in the method, including column positions, defaults,
  identity, ACLs, comments, trigger arguments and state, function source hash and
  attributes, event triggers, sequence ownership, extension membership, GUC
  definitions. Digests made under 0.1 verify after it. A session that was running
  while the upgrade waited continued (its inserts are in the ledger).
- **Proof and digest parsing**: no error in 432 mutations; an `I` proof under the
  `D` form and the converse, and a flipped `op`, return false. LATIN1 database with
  the role name `Jos` + bytes `C3 A9`: the user name in the proof is those five characters read as LATIN1 (the bytes `4a 6f 73 c3 83 c2 a9` in UTF-8), the
  server and `kingres-verify` (Go) both verify the `I` proof (steps entry-path,
  tx-path, block-hash, chain-walk, digest-time ok). The bytes hashed are the UTF-8
  conversion of the database encoding, as `ledger_block_tx_leaf` does. The
  conversion in `proof.c` uses `pg_do_encoding_conversion`, which can raise an error
  for a name that is not convertible; a role whose name is not valid in the database
  encoding cannot log in, so no such transaction exists (an EUC_JP database with a
  role named with bytes invalid in EUC_JP: the connection itself failed).
- **Worker**: builds in two databases, ignores a database without the extension, is
  switched off by `block_interval = 0`, survives restart and standby promotion
  (observed), logs and retries on a busy builder lock, runs with the identity of
  the extension owner and calls only fully qualified objects; no user-defined code
  runs in it (hashing of column types uses send functions of base types that only a
  superuser can create; `ledger_verify` uses `row_security = off`).
- **Event-trigger propagation**: a statement with several `ADD COLUMN`, combined
  with `SET (...)`, `IF NOT EXISTS`, arrays, enums, domains, typmods; with
  `event_triggers = off` the table stops accepting updates (capture refuses) and
  the next DDL on the table repairs the history table (the guard adds every missing
  column), which the README does not say but is better than described.
- **Performance**: 200000-row update in 8.0 s (insert 3.9 s), 100000-row delete in
  2.1 s, verify of the result in 5.1 s.

## Not examined

- Logical replication apply and its conflict paths, publications that include a
  ledger table, `COPY FREEZE`, Windows or `EXEC_BACKEND` builds, `pg_upgrade`,
  dump and restore.
- Exhaustion of `max_worker_processes` (read only: the launcher logs and retries),
  behaviour with a thousand databases, `EXEC_BACKEND` shared memory.
- The regression and TAP suites were not run (no rebuild allowed); the concurrent
  update tests of `t/008_updatable_concurrency.pl` were read for intent only.
- Race between the hook's name resolution for `ADD COLUMN` of a domain and a
  concurrent `ALTER DOMAIN ... SET DEFAULT` by the same owner (the hook looks the
  type up before the statement; a window of microseconds; not demonstrated).
- A kill of the server between the history insert and the commit was covered by
  the immediate-stop experiment with prepared transactions only; WAL of an
  unprepared in-flight update was not inspected with `pg_waldump`.
- Recorded `failed` state from a real failure other than `lock_timeout`.
