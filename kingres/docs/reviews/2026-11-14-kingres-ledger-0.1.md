<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
# Independent security and correctness review: kingres_ledger 0.1

Date: 2026-11-14. Reviewer: independent reviewer (not the author).

## Scope and method

Scope: `contrib/kingres_ledger/` (about 2,700 lines of C in `src/`, the
install script `kingres_ledger--0.1.sql`, pg_regress tests in `sql/` and
`expected/`, TAP tests in `t/`, the README) against the specification
`kingres/docs/design/ledger-mvp.md`, including its "Implementation notes and
deviations" section.

Read completely: `src/capture.c`, `row_hash.c`, `merkle.c`, `proof.c`,
`protect.c`, `kingres_ledger.c`, the headers, the full SQL install script,
the README and the specification notes. The test sources were only skimmed
(`sql/ledger_protect.sql` and grep over the others); the TAP tests were not
read in detail.

Reproduction: a throwaway server (PostgreSQL 18.7, KingresDB build, WSL2
Ubuntu-24.04) on port 57101 with `shared_preload_libraries =
'kingres_ledger'`, in a scratch directory outside the repository. Scripts ran
through psql and pgbench as the bootstrap superuser and as ordinary roles. The
scratch server and directory were removed afterwards. No repository file was
changed during the review. Each finding says "Reproduced" (observed on the
server) or "Reasoned" (from code reading only).

Severity scale: critical = undetected tampering, forged proof, privilege
escalation, or crash reachable by an ordinary user, or (here) permanent loss
of verification by an ordinary user; high; medium; low.

Summary: no way was found to change or remove a ledger row, entry,
transaction or block and still pass `ledger_verify` with a previously exported
digest. Findings L-01 to L-12 below are defects in availability of
verification, hook coverage, privileges, and documentation.

## Findings

### L-01 Critical: any table owner can permanently break ledger_verify for the whole database

- Where: `kingres_ledger--0.1.sql`, `ledger_verify`, the `table_rows` query
  (lines 833-845): `SELECT kingres.ledger_row_hash(x) ... FROM ONLY %I.%I x`.
  `ledger_enable` has the same pattern at lines 429-431 with alias `t`.
- What is wrong: `x` is a bare identifier. When the table has a column named
  `x`, the column wins over the row alias, so `ledger_row_hash(x)` is resolved
  against the column type and fails.
- Reproduction (Reproduced): as an unprivileged role that owns a schema:
  `create table bobs.evil (x int, y text); select
  kingres.ledger_enable('bobs.evil');` then, as superuser, `select * from
  kingres.ledger_verify();`.
- Observed: `ERROR: function kingres.ledger_row_hash(integer) does not
  exist` (QUERY shows the table_rows statement). The whole function aborts, so
  no problem is reported for any table. The table cannot be dropped because the
  hook refuses it, so verification stays broken. The same failure was seen
  with a column `x` of type xml.
- Fix: do not reference the whole row by a bare alias that a column can
  shadow. Use a subquery with a generated alias that cannot collide, or
  `ROW(x.*)`-style qualified construction, or reject such column names in
  `ledger_enable`. Make a failure in one table's check non-fatal for the
  others. Add tests with columns named `x` and `t`.

### L-02 High: rows can be added to a ledger table through a foreign-table child and verify stays clean

- Where: `src/protect.c`, `kl_check_statement` (lines 734-754) handles
  `T_CreateStmt` (inheritance parents) but not `T_CreateForeignTableStmt`;
  `ledger_verify` compares with `FROM ONLY`.
- What is wrong: `CREATE FOREIGN TABLE ... INHERITS (ledger_table)` is
  accepted. Queries on the ledger table then return rows that have no ledger
  entry, and verify cannot see them.
- Reproduction (Reproduced): as the table owner (superuser in the test), with
  `file_fdw`: `create foreign table app.forged () inherits (app.led) server fs
  options (filename '/tmp/forged.csv', format 'csv');` where the file holds
  `999,forged`.
- Observed: `select * from app.led` returns (1,real) and (999,forged);
  `ledger_verify()` returns 0 rows; `ledger_generate_digest()` returns the same
  digest as before.
- Fix: in the hook, reject `CreateForeignTableStmt` whose `base.inhRelations`
  names a ledger table. As defense in depth, have `ledger_verify` report any
  ledger table with `relhassubclass` or entries in `pg_inherits`.

### L-03 High: the hook can be bypassed by a rename race, by anyone who owns the ledger table

- Where: `src/protect.c`, `kl_rv_relid` (lines 212-218) resolves the name with
  `NoLock`; the real statement resolves it again later.
- What is wrong: time-of-check/time-of-use on the table name. The README says
  the race needs "another privileged session"; in fact the table owner needs
  no superuser rights, only a second table they own.
- Reproduction (Reproduced): schema `rc` with tables `h` (harmless) and `l`
  (ledger). Session A in a loop: `do $$ begin execute 'alter table rc.t
  disable trigger user'; exception when others then null; end $$;` (3
  clients). Session B loop of transactions renaming so that `rc.t`
  alternates between `h` and `l`.
- Observed: after 60 seconds all three triggers on `rc.l` had `tgenabled =
  'D'`. After that `UPDATE` and unlogged inserts succeeded. `ledger_verify`
  then reported the three triggers and the row mismatches (so the tampering was
  detected, not hidden). The same race applies to `DROP TABLE` (Reasoned).
- Fix: resolve names with a lock and a relid-pinning callback
  (`RangeVarGetRelidExtended`), or enforce the rules where the action happens
  rather than by pre-resolving names. Correct the README wording.

### L-04 High: ledger_enable is executable by PUBLIC; any table owner can make a table permanently undroppable

- Where: `kingres_ledger--0.1.sql`, `ledger_enable` (lines 291-433) is
  `SECURITY DEFINER` and is never revoked from PUBLIC (the REVOKE statements
  cover other functions only).
- What is wrong: an ordinary role can irreversibly put any table it owns under
  ledger control. Administrators then cannot drop the table, its schema, the
  owner role or the extension, and the owner can add unbounded permanent
  `ledger_entry` rows.
- Reproduction (Reproduced): as role `bob`: `create table bobs.mine (id int
  primary key, note text); select kingres.ledger_enable('bobs.mine');` then as
  superuser: `drop table bobs.mine;`, `drop schema bobs cascade;`, `drop owned
  by bob;`, `drop role bob;`, `drop extension kingres_ledger;`.
- Observed: KL002 on the first, second, third and fifth ("cannot drop table
  "mine": it is a ledger table"); `DROP ROLE` fails with "owner of table
  bobs.mine". A table with an FK to a ledger table also cannot be truncated or
  have rows deleted.
- Fix: revoke EXECUTE from PUBLIC and grant it to an administrative role (or
  require superuser). Document quota and disk implications of ledger tables.

### L-05 Medium-high: ledger_enable under REPEATABLE READ or SERIALIZABLE silently misses committed rows

- Where: `kingres_ledger--0.1.sql`, lines 403-431: the `LOCK TABLE` waits, but
  the later `SELECT ... FROM ONLY` still uses the snapshot taken earlier in the
  transaction.
- What is wrong: rows committed by another session after the snapshot but
  before the lock are in the table but are never entered into the ledger.
- Reproduction (Reproduced): session A: `begin isolation level repeatable read;
  select count(*) from s8.t;`. Session B: `insert into s8.t values (2);`
  (committed). Session A: `select kingres.ledger_enable('s8.t'); commit;`.
- Observed: `ledger_verify` then permanently reports `table_rows | s8.t | row
  hash b956...: 1 row(s) in the table, 0 entry(ies) in the ledger`. The row
  cannot be repaired.
- Fix: refuse unless `transaction_isolation = 'read committed'` (as
  `ledger_build_block` does), and lock the table before reading catalog
  information and rows; use the OID rather than re-resolving names.

### L-06 Medium: hook bypass by renaming an extension function

- Where: `src/protect.c`, `kl_check_rename` (line 438: `stmt->relation == NULL`
  returns without checks); `kl_is_ledger_function` (lines 124-137) and the
  trigger tests in `ledger_verify` (SQL lines 821-830 and 863-871) identify
  functions by name.
- What is wrong: renaming `kingres.ledger_capture` makes the hook stop
  classifying every ledger table as protected. A fake function with the old name
  could also satisfy verify's name-based trigger check.
- Reproduction (Reproduced), as superuser: `alter function
  kingres.ledger_capture() rename to ledger_capture_x; drop table app.led;
  alter function kingres.ledger_capture_x() rename to ledger_capture;`.
- Observed: the `DROP TABLE` succeeded. Afterwards `ledger_verify` reports
  `table_missing | table_id 1 | the table with oid ... no longer exists`
  (detected after the fact). The README and design claim rejection "for every
  role including superusers" is therefore not true.
- Fix: reject RENAME of extension member functions; identify functions in the
  hook and in `ledger_verify` by extension membership and OID.

### L-07 Medium: a ledger column can be dropped through a dependency cascade

- Where: `src/protect.c`, `kl_check_drop` (lines 524-640) checks `DROP TYPE`,
  `DOMAIN` and `EXTENSION ... CASCADE` against direct column types only.
- Reproduction (Reproduced): `create function app.dbl(int) ...; create table
  app.led (id int, g int generated always as (app.dbl(id)) stored, v text);`
  enable, then `drop function app.dbl(int) cascade;`.
- Observed: column `g` was dropped without error; `ledger_verify` then reports
  every row as a `table_rows` mismatch (detected, but the ledger is permanently
  marked inconsistent).
- Fix: reject DROP ... CASCADE whose dependency closure reaches an attribute of
  a ledger table (walk `pg_depend`), or reject generated columns in
  `ledger_enable`.

### L-08 Medium: denial of service on block building by any role

- Where: `kingres_ledger--0.1.sql`, `ledger_build_block`, line 465:
  `pg_advisory_xact_lock(hashtextextended('kingres_ledger.build_block', 0))`.
  The key is derivable and advisory locks are available to every role.
  `ledger_verify_digest` is also executable by PUBLIC and scans the whole chain.
- Reproduction (Reproduced): any role: `begin; select
  pg_advisory_lock(hashtextextended('kingres_ledger.build_block',0));` and keep
  the session open. Meanwhile `ledger_build_block()` by the administrator blocks
  until statement_timeout ("canceling statement due to statement timeout" at
  line 17 of the function).
- Fix: serialize through a lock on a catalog row (for example `SELECT ... FOR
  UPDATE` on a private row) or a lock only privileged roles can take; restrict
  or cost-limit `ledger_verify_digest` (the CPU cost is Reasoned).

### L-09 Medium: ledger tables are readable by PUBLIC and the row hashes are unsalted

- Where: `kingres_ledger--0.1.sql`, lines 99-104 grant SELECT on every ledger
  table to PUBLIC.
- What is wrong: any role can read all row hashes, table ids, transaction ids
  and user names (`ledger_pending.user_name`, `ledger_tx.user_name`). The row
  encoding is public and unsalted, so a role can confirm guesses about
  low-entropy rows in tables it cannot read (the guess-confirm step is
  Reasoned). Access to the metadata was Reproduced as role `carol`.
- Fix: restrict SELECT on the entry, pending and tx tables to administrative
  roles, or add a per-database secret salt to the row hash, or document it.

### L-10 Medium (documentation and robustness): digest and proof checks

- `ledger_verify_digest` raises an error instead of returning false when the
  digest height is 9223372036854775807 (`dheight + 1` overflow, SQL line 609).
  Reproduced: `ERROR: bigint out of range`.
- The proof field `relation` and the digest field `database` are not
  authenticated; `table_id` is a local number. The README verifier steps 1-4 do
  not say that a verifier must recompute `row_hash` from the actual row to learn
  that the row is in the ledger. Databases cloned from each other accept each
  other's digests (Reasoned).
- Fix: validate the height range before the arithmetic; document the above or
  bind a database identifier into the chain.

### L-11 Low: hardening and smaller defects

- Leaf kinds share the leaf prefix: the row hash, the entry leaf and the block
  transaction leaf are all `SHA-256(0x00 || data)` with no kind tag. They
  differ in length and structure and no practical collision was found
  (Reasoned). The block hash has no prefix at all and its input starts with the
  bytes 00 01 (`merkle.c`, line 392). A distinct prefix or tag would remove the
  theoretical concern.
- `ledger_verify_proof` accepts non-integer JSON numbers by rounding
  (`kl_json_int`, `proof.c` lines 114-125): `entry_index` plus 0.4 was accepted
  for all 194 test proofs (Reproduced). Not exploitable, but not canonical.
- `ADD COLUMN` after `ledger_enable` allows types that `ledger_enable` rejects:
  a composite type, an array of a user type, or `aclitem`. Inserts of
  non-NULL values then either fail ("no binary output function available for
  type aclitem", Reproduced) or hash type OIDs (Reasoned).
- A ledger table can be renamed and a new table created under the old name;
  the ledger keeps verifying while applications read an unledgered table. The
  README does not say so.
- `pg_dump` and restore are unsupported and documented as such; a restored
  database keeps the triggers but has an empty `ledger_table`, so later inserts
  fail on the foreign key (Reasoned).
- `ledger_enable` checks and reads the catalog before taking its lock and uses
  names from that read in later statements (Reasoned race; the effect is on the
  caller's own tables).

### L-12 Low: test gaps

- No test covers: `ledger_enable` by an unprivileged role; columns named `x` or
  `t`; `CREATE FOREIGN TABLE ... INHERITS`; renaming an extension function; the
  concurrent rename race; `ledger_enable` under REPEATABLE READ with concurrent
  inserts; cascade-dropping a function used by a generated column; the
  advisory-lock denial of service.
- `pg_temp.try` in `sql/ledger_protect.sql` only prints notices, so a statement
  that is wrongly accepted appears only as a diff in the expected file.

## Examined and found sound

- RFC 6962 Merkle code: roots equal an independent Python implementation for
  n = 0..70 including the empty tree; the audit-path verifier follows RFC 9162
  section 2.1.3.2; 194 entries in transactions of 1..40 entries verified, and
  altered `entry_count`, `tx_index` and `user_name` were rejected. Index
  arithmetic is safe for the reachable sizes (Reasoned).
- Proof JSON handling: scalar `block`, object `chain`, bad `entry_path`,
  1e400 numbers, wrong `op`, over-long `user_name`, wrong digest height or
  hash, an older digest, `[]` and `null` inputs all returned false with no
  error or crash.
- pg_temp operator or function hijack of the SECURITY DEFINER functions: tried
  an `=(oid, regclass)` operator in `pg_temp` against `ledger_enable`; it was
  not used (the search path code ignores the temp schema for operators and
  functions).
- Capture: COPY, MERGE, `INSERT ... ON CONFLICT DO NOTHING`, subtransaction
  rollback and release, prepared transactions, stored generated columns,
  compressed and large text and `VACUUM FULL` all kept the ledger consistent.
  `TRUNCATE ... CASCADE` through a foreign key, cascading deletes and
  `ON CONFLICT DO UPDATE` were refused. The hook saw nested DDL in `DO` blocks.
- Row hash and client encoding: identical hashes under client_encoding SJIS,
  LATIN1 and SQL_ASCII in a UTF8 database, and a LATIN1 database verified
  clean; the client encoding was restored after calls and after a rolled-back
  savepoint. The domain-default checks of `ADD COLUMN`, including a domain over
  a domain, work.
- Tampering with row data, entries or blocks after a hook bypass: the
  comparison of table rows with `ledger_entry` and the chain checks detected
  every attempt; catalog tables stay protected by extension membership even
  when the user-table hook is defeated (partly tested, partly Reasoned).
- Block builder: advisory lock plus READ COMMITTED means no transaction is
  skipped or included twice (Reasoned); entries and the pending row commit
  atomically with the row, so a backend dying between them cannot leave an
  orphan.

## Not examined

- The TAP tests (`t/`): standby, crash recovery, `pg_stat_statements` hook
  chaining.
- Logical replication apply, hot standby behaviour and parallel workers.
- `pg_dump` and `pg_upgrade` behaviour, beyond reasoning.
- Performance of `ledger_verify` on a large ledger.
- Transaction ID epoch handling beyond reading the `xid8` use.
- `CREATE INDEX CONCURRENTLY` and `REINDEX` on the ledger catalog tables.
- A non-superuser extension owner.
