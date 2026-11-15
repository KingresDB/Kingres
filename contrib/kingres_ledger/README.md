<!-- SPDX-License-Identifier: PostgreSQL -->
<!-- Portions Copyright (c) 2026, RooyeKhat Media Co -->
# kingres_ledger

Tamper-evident ledger tables for KingresDB (a thin fork of PostgreSQL 18.7),
version 0.2. Version 0.1 (phase 6, first increment) has append-only tables;
version 0.2 adds updatable ledger tables with a history table, background
workers that build the blocks, and the records an operator needs (recorded
digests and verifications and a status view). The designs are
[`kingres/docs/design/ledger-mvp.md`](../../kingres/docs/design/ledger-mvp.md)
(0.1, including its implementation notes) and
[`kingres/docs/design/ledger-0.2.md`](../../kingres/docs/design/ledger-0.2.md)
(0.2, with its "Implementation notes and deviations"). The findings of the
independent review of version 0.1 and what was done about them are in
[`kingres/docs/reviews/2026-11-14-kingres-ledger-0.1.md`](../../kingres/docs/reviews/2026-11-14-kingres-ledger-0.1.md)
and in the notes at the end of the 0.1 design document.

A table put under ledger control with `kingres.ledger_enable` is either
**append-only** (the default: rows can only be inserted) or **updatable**
(`ledger_enable(table, 'updatable')`: rows can be inserted, updated and
deleted, and every old version stays provable). Every change is hashed and
recorded in a hash chain of blocks. A digest of the latest block, stored
outside the database, lets anyone check later that the table and the chain
have not been changed, and an inclusion proof shows that one row version is
part of the ledger.

The hash format is **version 1, unchanged by 0.2**: digests, proofs and block
hashes made by 0.1 still verify, and the independent verifier
(`kingres/verifier`) checks proofs of both operations: `I` (a row version was
inserted) and `D` (a row version was removed), where the caller chooses which one
it expects (`--op I|D|any`; the default accepts either and reports which one
was proven). What 0.2 adds that a verifier needs to know is the operation `D`
(an entry that records the removal of a row version) and the `op` member of a
proof; both are described in "Hash format version 1" below.

## What the ledger guarantees, and what it does not

The ledger makes tampering **detectable**, not impossible. Anyone who can
modify the database files, restore an old backup or replace the server binary
can change data; what they cannot do is change it without the change being
found by a verification against a digest that was stored outside the
database. **The digest stored outside the database is the root of trust.**
Nothing inside the database, including `ledger_verify`, can prove integrity on
its own: an attacker who rewrites the whole chain consistently passes
`ledger_verify`, and is caught only by `ledger_verify_digest` with a digest
exported earlier.

There is also a window: a committed transaction is covered by a digest only
once it has been placed in a block and a digest of that block has left the
database. Until then a privileged attacker could alter its ledger entries
undetected. The window is the block interval (the background worker builds a
block about every `kingres_ledger.block_interval`, 10 seconds by default) plus
the digest export interval. The digests that `ledger_generate_digest` records
in `kingres.ledger_digest` are **not** outside the database and are not a root
of trust.

**What an updatable table guarantees, and what it does not.** The ledger of an
updatable table proves that the rows the table holds now are exactly the row
versions that were entered and not yet removed (every `I` entry minus every
`D` entry, as multisets of row hashes), that every removed version is in the
history table with the transaction that removed it, and, against a digest kept
outside the database, that none of this was rewritten. It does **not** prove
what a version meant, who is allowed to change a row, or that a change was
authorised; the user name of a transaction is the name of the role that ran
the statement. It does not prevent a change (a privileged user can still run
`UPDATE`); it makes a change visible and the old version provable. The order of
the entries inside a transaction is the order in which the capture triggers
ran, which is the order in which statements and their triggers finished: for
nested changes (a trigger that updates the row again) an old version can be
entered after a newer one, which does not matter for the check of the rows, as
the check compares multisets.

The protections against switching the ledger off (below) stop mistakes and
ordinary privileged use. They are not a security boundary against someone who
edits the catalogs directly, switches event triggers off, removes the library
from the configuration, or changes files: the section "What the protections
cannot stop" lists every way that is known.

**Enabling the ledger on a table is permanent, and so is its mode.** There is
deliberately no function that switches it off, and an append-only table cannot
be made updatable or the reverse (`ledger_enable` on a ledger table with the
other mode is rejected with "not supported yet"). Once a table is under ledger
control:

- `DROP TABLE` of it is rejected, and so is `DROP SCHEMA ... CASCADE` of a
  schema that contains it, and `DROP OWNED BY` its owner;
- `DROP ROLE` of its owner fails until the table has been moved to another
  role with `REASSIGN OWNED BY old TO new` (`ALTER TABLE ... OWNER TO` works);
- rows of other tables cannot be deleted while an append-only ledger row
  refers to them through a foreign key (the ledger rows can neither be deleted
  nor changed, so `ON DELETE CASCADE` and `ON DELETE SET NULL` fail), and
  `TRUNCATE` of such a table fails; an updatable table does not have this
  problem: the actions change its rows and are captured;
- the history table of an updatable table is part of the ledger and stays too;
- `DROP EXTENSION kingres_ledger` is rejected while any ledger table exists;
- the entries of its rows stay in `kingres.ledger_entry` for ever, and the
  tables and their entries take disk space for ever.

Because of this, `ledger_enable` is not executable by PUBLIC: it is an
administrative decision (see "Privileges").

## Build and test

In the PostgreSQL tree (autoconf): `make -C contrib/kingres_ledger` and
`make -C contrib/kingres_ledger check` (needs
`--enable-tap-tests` for the TAP tests). With meson the extension is built
when `subdir('kingres_ledger')` is added to `contrib/meson.build` and
`kingres_ledger` to `contrib/Makefile` (done by the integrator).

Against an installed server, with PGXS:

```
make USE_PGXS=1 PG_CONFIG=/path/to/pg_config
make USE_PGXS=1 PG_CONFIG=/path/to/pg_config install
```

The developer helper `kingres/dev/ext.sh` does this in WSL2 and runs all
tests:

```
kingres/dev/ext.sh kingres_ledger all     # sync build install regress tap
```

Tests: eleven pg_regress tests and ten TAP tests under `t/`.

| Test | What it covers |
|---|---|
| `ledger_setup` | switches the block-building workers off for the pg_regress tests, which build blocks by hand and count them (the TAP tests cover the workers) |
| `ledger_updatable` | updatable tables: the history table, every statement path that changes a row (`ON CONFLICT DO UPDATE`, `MERGE`, foreign-key actions, triggers that change the row before and after the capture, `COPY`, `session_replication_role = replica`, views, prepared transactions, `TOAST`ed values), `ADD COLUMN`, rejection of conversion, the protection and privileges of the history table, proofs of `I` and `D` entries and the strict parsing of proofs, and every tamper case |
| `ledger_records` | `ledger_digest`, `ledger_verify_recorded`, `ledger_status`: contents, caps, privileges, protection |
| `ledger_review2` | one reproduction for each finding of the second review (L2-01 to L2-05, L2-08) that a single server can show: event triggers of a superuser at every point of an `ADD COLUMN`, the privileges of the history table through owner changes, `REASSIGN OWNED`, default privileges and tampering, what PUBLIC can read, savepoints and exception blocks around `ledger_enable`, `COPY ... FREEZE`, hostile session settings |
| `ledger_hash` | RFC 6962 vectors, the tags, the genesis hash, the canonical row encoding, independence of session settings |
| `ledger_capture` | `ledger_enable`, capture, rollback and subtransactions, blocks, digests |
| `ledger_verify` | clean verification, digests, proofs, and every tamper case |
| `ledger_protect` | every statement the hook rejects, and the ordinary DDL it allows |
| `ledger_format` | the format hardening, checked against an independent SQL implementation over the whole ledger: tags, chain identity, block time, table binding of proofs |
| `ledger_review` | one reproduction for each finding L-01 to L-12 of the review |
| `ledger_privs` | scan of `pg_proc`: fixed `search_path`, `SECURITY DEFINER` only where needed, qualified references; the default privileges; shadowing by temporary objects |
| `001_concurrent_blocks` | concurrent writers while blocks are built |
| `002_restart_standby` | restart, crash recovery, a streaming standby |
| `003_preload_and_hooks` | the preload requirement, hook chaining with `pg_stat_statements` |
| `004_races` | the rename race against the hook (a stress test), a foreign-table child, `ledger_enable` and snapshots, the block builder lock |
| `005_environments` | logical replication apply, hot standby, parallel query, an extension owner that is not a superuser |
| `006_guard_without_hook` | the event triggers alone: after the library was removed from `shared_preload_libraries` each way of switching the ledger off is still rejected |
| `007_upgrade` | `ALTER EXTENSION UPDATE` from 0.1 with tables, blocks, digests and proofs; the upgraded database has the objects, privileges and protection of a direct installation of 0.2 (compared through a fingerprint of the catalogs) |
| `008_updatable_concurrency` | concurrent `UPDATE`, `DELETE`, upsert, `MERGE`, locking, savepoints and rollbacks of the same rows (READ COMMITTED with re-evaluation of the row, and REPEATABLE READ with retries) while blocks are built; logical replication apply into an updatable table |
| `009_worker` | the block-building workers: bounded time, two databases, `block_interval = 0`, reload, restart, a prepared transaction, bounded blocks, a busy builder lock, a standby and promotion; every phase asserts the outcome (the state the workers report, and no failed or killed worker in the log), not the absence of one line |
| `010_review2` | the second review and the areas it did not examine: 24 hostile database settings that must not stop the worker, a failing worker (growing delay, `ledger_status`), a cancelled `ledger_verify_recorded`, the launcher (sweeps, memory, a worker that never finishes), `max_worker_processes` used up, logical replication into an updatable table (conflict, `TRUNCATE`), `pg_dump` and restore |

The regress tests need `shared_preload_libraries = 'kingres_ledger'` (the
in-tree `check` target and `kingres/dev/ext.sh` set it).

## Installation

The library must be loaded at server start:

```
shared_preload_libraries = 'kingres_ledger'
```

then, in each database, `CREATE EXTENSION kingres_ledger;` as a superuser.
**The role that creates the extension owns its functions and tables and must
stay a superuser**: the functions that run as the owner create triggers on
tables of other roles and read every ledger table. `ledger_enable` refuses to
run when the owner is not a superuser. Without the library in
`shared_preload_libraries` the extension can be created, but `ledger_enable`
refuses to run (SQLSTATE `KL003`), because nothing would stop a privileged user
from switching the ledger off.

The extension lives in schema `kingres`. The control file says
`schema = kingres` and `relocatable = false`: `CREATE EXTENSION` creates the
schema when it does not exist and uses it when it does (for example because
`kingres_shard` is installed), and in both cases the schema is not a member
of this extension, so `DROP EXTENSION` never removes a schema another
extension also uses. All objects of the extension are named `ledger_*`.
`USAGE` on the schema is granted to `PUBLIC`.

Note: a role named `kingres` puts the schema `kingres` first in the default
`search_path` (through `"$user"`), so unqualified `CREATE TABLE` by that role
creates the table there; `ledger_enable` refuses tables in schema `kingres`.
Create user tables in another schema.

**Upgrade from 0.1.** `ALTER EXTENSION kingres_ledger UPDATE` (the script is
`kingres_ledger--0.1--0.2.sql`; load the new library first: restart the server,
or at least start new sessions after installing the files). It adds the column
`history_relid` to `ledger_table` and `queued_at` to `ledger_pending`, lets
`ledger_entry.op` be `D`, creates `ledger_digest` and `ledger_verification` and
the view `ledger_status`, and replaces the functions that changed. Existing
ledger tables, blocks, digests and proofs are not touched and still verify; a
digest made before the upgrade verifies after it; the result is the same set
of objects, privileges and protection as `CREATE EXTENSION kingres_ledger
VERSION '0.2'`. Things to know: `ledger_inclusion_proof(regclass, bytea)` is
kept (as a wrapper) so that existing grants keep working, and the new
three-argument form needs its own grant; transactions that were pending
before the upgrade have no `queued_at`; and the 0.2 library must be loaded
for the 0.2 SQL (a 0.2 library works with the 0.1 SQL, a 0.1 library does not
work with the 0.2 SQL).

The extension creates three event triggers (`kingres_ledger_guard_end`,
`..._drop`, `..._rewrite`), which run after every DDL command of the
database; see "Protection against being switched off". Their cost is a few
catalog queries per ledger table per DDL command.

## Using it

```sql
GRANT EXECUTE ON FUNCTION kingres.ledger_enable(regclass, text) TO table_admin;  -- once, by the administrator
SELECT kingres.ledger_enable('public.accounts');   -- existing rows are entered too
INSERT INTO accounts VALUES (...);                  -- captured by a trigger
SELECT kingres.ledger_build_block();               -- normally called on an interval
SELECT kingres.ledger_generate_digest();           -- store the result OUTSIDE the database
SELECT * FROM kingres.ledger_verify();             -- no rows when consistent
SELECT kingres.ledger_verify_digest('{...}');      -- digest exported earlier
SELECT kingres.ledger_inclusion_proof('accounts', kingres.ledger_row_hash(a.*))
  FROM accounts a WHERE id = 1;
SELECT kingres.ledger_verify_proof(proof, digest); -- uses only its two arguments
```

Write the row as `a.*`, not `a`: a bare `a` means a column named `a` if the
table has one. The extension itself never relies on a bare alias.
`ledger_enable` needs the READ COMMITTED isolation level, as the block builder
does.

### Updatable tables

```sql
SELECT kingres.ledger_enable('public.inventory', 'updatable');
UPDATE inventory SET qty = qty - 1 WHERE id = 7;     -- D of the old version, I of the new one
DELETE FROM inventory WHERE id = 8;                  -- D of the old version
SELECT * FROM inventory_ledger_history ORDER BY ledger_txid, ledger_seq;   -- every removed version
```

`ledger_enable(relation, 'updatable')` creates the **history table**
`<schema>.<table>_ledger_history` (the name must fit in 63 bytes and must not
exist; the table must not have columns named `ledger_txid`, `ledger_seq` or
`ledger_op`): the columns of the table (same names, types and modifiers, in
the same order, no constraints, defaults or identity), then `ledger_txid
bigint`, `ledger_seq int` and `ledger_op "char"`. The two triggers on the
table fire after every row change:

| Statement | Entries | History row |
|---|---|---|
| `INSERT` | `I`, the hash of the new row | none |
| `DELETE` | `D`, the hash of the old row | the old row |
| `UPDATE` | `D` of the old row, then `I` of the new row | the old row |

An `UPDATE` that changes no column still writes both entries (they have the
same row hash). `ledger_txid` and `ledger_seq` of a history row are those of
its `D` entry, `ledger_op` is `D`. `TRUNCATE` of an updatable table is
rejected. Existing rows are entered as `I` entries by `ledger_enable`.
Everything that changes a row of the table is captured, whatever statement
did it: `UPDATE`, `DELETE`, `INSERT ... ON CONFLICT DO UPDATE`, `MERGE`,
foreign-key actions (`ON DELETE CASCADE`, `SET NULL`, `ON UPDATE CASCADE`),
`COPY`, an updatable view over the table, the logical replication apply worker
(which runs with `session_replication_role = replica`; the triggers are
`ENABLE ALWAYS`), triggers on the table that change the row before the
capture (the capture sees the row that was stored) and triggers that update
the row again after it (every version is captured), and concurrent updates of
the same row (a row that READ COMMITTED re-evaluates after another transaction
changed it is captured as the version that was really replaced). `SELECT ...
FOR UPDATE` changes no row and writes nothing.

The history table belongs to the extension owner. **Who can read old row
versions.** Exactly these roles, and nobody else: the extension owner (a
superuser), and the owner of the ledger table, which includes every role that is
a member of the owner and inherits its privileges. The rule is kept by the
extension, not by a grant: `ledger_enable` sets the privileges explicitly (default
privileges of the extension owner, `ALTER DEFAULT PRIVILEGES`, do not apply to
it); when the owner of the table changes (`ALTER TABLE ... OWNER TO`, `REASSIGN
OWNED`) the former owner's `SELECT` is taken away and the new owner is granted
`SELECT` in the same transaction (the event trigger does it after every DDL
command, and the protection hook does it after `ALTER TABLE ... OWNER TO` and
`REASSIGN OWNED` also when event triggers are off); `GRANT` on a history table
is rejected, a `GRANT ... ON ALL TABLES IN SCHEMA` that reaches one is undone by
the event trigger at the end of the statement, and `ledger_verify` reports a
history table whose privileges are not the ones of the rule (`history_privileges`).
To give another role access, make it a member of the owner of the ledger table
(or read the history through a `SECURITY DEFINER` function of your own that
applies the filter you want). **The history table bypasses everything that
protects the ledger table itself**: the row security policies and the column
privileges of the table do not apply to it, so a role that reads the history can
read every column of every old version, including rows and columns that the
role could not read in the table. Nobody can write it through SQL: `UPDATE`,
`DELETE` and `TRUNCATE` are rejected by the blocking triggers like those of every
ledger table, `INSERT` by a third trigger (`kingres_ledger_block_insert`; only the
capture code inserts, and it does that through the table access method, which
fires no trigger).

`ALTER TABLE ... ADD COLUMN` on an updatable table is allowed under the rules
of 0.1 (nullable, no default, a type `ledger_enable` accepts) and is applied
to the history table in the same statement by the event trigger, through the
catalog API of the server and not as a statement (no event trigger, hook or
function of a user runs while it is done); the new
column is appended to the history table (after `ledger_op`), and the row hash
of a history row ignores the three ledger columns, so the order of the columns
of the table decides the hash. A column named `ledger_txid`, `ledger_seq` or
`ledger_op` is rejected. Every other change of the column layout stays
rejected.

**Verification of an updatable table** (`ledger_verify`), in addition to the
checks of 0.1: the multiset of the row hashes of the current rows equals the
multiset of `I` entries minus the multiset of `D` entries (`table_rows`);
every `D` entry has exactly one history row with the same `ledger_txid` and
`ledger_seq`, whose row hash recomputed from its columns
(`kingres.ledger_history_row_hash(h.*)`) is the row hash of the entry, and
every history row has its `D` entry and has `ledger_op = 'D'`
(`history_rows`); the history table, its triggers and its columns are as
`ledger_enable` made them (`history_missing`, `history_state`,
`history_trigger`, `history_columns`); and an append-only table has no `D`
entry (`entry_op`).

**Proofs.** `ledger_inclusion_proof(relation, row_hash)` proves the first `I`
entry with that row hash, as before; `ledger_inclusion_proof(relation,
row_hash, 'D')` proves the first `D` entry: that this version was removed in
that transaction. The proof carries `op`. `ledger_verify_proof(proof, digest)`
accepts either; the four-argument form (with the table id and the row hash)
accepts an `I` proof only; the five-argument form takes the operation as its
last argument.

| Function | Purpose | Executable by |
|---|---|---|
| `kingres.ledger_enable(regclass, text default 'append_only')` | Put an ordinary table under ledger control, for good, in mode `append_only` or `updatable`. The caller must own the table. READ COMMITTED only. | granted roles |
| `kingres.ledger_row_hash(record) returns bytea` | Leaf hash of a row of a ledger table; the argument must be a row of the table itself, e.g. `ledger_row_hash(a.*) FROM accounts a` (not a sub-select). | PUBLIC |
| `kingres.ledger_build_block() returns bigint` | Close a block from the pending transactions; the new height, or NULL when nothing was pending. READ COMMITTED only. | granted roles |
| `kingres.ledger_generate_digest() returns jsonb` | Build a block if needed, **record** the digest in `kingres.ledger_digest` and return it: the digest of the latest block; NULL (and nothing recorded) when the ledger is empty. | granted roles |
| `kingres.ledger_verify(from_height bigint default 0, to_height bigint default null)` | One row `(check_name, object, detail)` per problem. Records nothing. | granted roles |
| `kingres.ledger_verify_recorded(from_height bigint default 0, to_height bigint default null) returns bigint` | Run `ledger_verify` and record the run in `kingres.ledger_verification`; returns its id. | granted roles |
| `kingres.ledger_verify_digest(jsonb) returns boolean` | Whether a digest matches the stored chain (the whole chain up to the digest height is recomputed). Never raises an error for a malformed digest. | granted roles |
| `kingres.ledger_inclusion_proof(regclass, bytea) returns jsonb` | Proof for the (first) `I` entry with this row hash. The row must already be in a block. | granted roles |
| `kingres.ledger_inclusion_proof(regclass, bytea, "char") returns jsonb` | The same for the entry of the given operation, `I` or `D`. | granted roles |
| `kingres.ledger_verify_proof(jsonb, jsonb) returns boolean` | Check a proof against a trusted digest of the same chain; reads no table; malformed input gives false. | PUBLIC |
| `kingres.ledger_verify_proof(jsonb, jsonb, int, bytea) returns boolean` | The same, and the proof must be for this table id and this row hash (the ones the caller computed from the row it holds), and must be an `I` proof. | PUBLIC |
| `kingres.ledger_verify_proof(jsonb, jsonb, int, bytea, "char") returns boolean` | The same for a proof of the given operation, `I` or `D`. | PUBLIC |
| `kingres.ledger_history_row_hash(record) returns bytea` | Row hash of a row of a history table (`SELECT kingres.ledger_history_row_hash(h.*) FROM t_ledger_history h`): the hash its `D` entry carries. | PUBLIC |
| `kingres.ledger_leaf_hash(bytea)`, `ledger_node_hash(bytea, bytea)`, `ledger_merkle_root(bytea)` (aggregate), `ledger_merkle_path(bytea[], int)`, `ledger_tx_leaf(...)`, `ledger_block_tx_leaf(...)`, `ledger_block_hash(...)`, `ledger_genesis_hash(bytea)`, `ledger_timestamptz_us(timestamptz)` | Primitives used by the SQL functions and useful for independent checks. | PUBLIC |
| `kingres.ledger_hook_active()` | True when the library was loaded through `shared_preload_libraries`. | PUBLIC |

Internal functions (`ledger_capture`, `ledger_record_row`, `ledger_lock_table`,
`ledger_history_sync`, `ledger_history_acl_sync`, `ledger_worker_state` (read by
the view `ledger_status`, whose readers need `EXECUTE` on it; see
"Privileges"), and the helpers `ledger_all_defects`,
`ledger_table_defects`, `ledger_history_defects`, `ledger_catalog_defects`,
`ledger_trigger_ok`, `ledger_type_problem`, `ledger_is_member`,
`ledger_guard`) are not part of the interface. The ones that write the ledger
are not executable by PUBLIC, and the protection hook rejects any attempt to
grant them.

**The records for operators.**

| Object | Contents |
|---|---|
| `kingres.ledger_digest` | one row per digest this database generated (`ledger_generate_digest`), also when nothing changed since the last one: `digest_id`, `generated_at`, `generated_by`, `height`, `block_hash`, `digest` (exactly what the function returned). PUBLIC can read it except the column `generated_by` (the name of an operator). **A convenience, not a root of trust**: the table is in the database, whoever can rewrite the chain can rewrite it, and `ledger_verify` does not compare it with the chain; to check a recorded digest call `ledger_verify_digest(digest)`; keep a copy outside the database. |
| `kingres.ledger_verification` | one row per `ledger_verify_recorded` run: `verification_id`, `started_at`, `finished_at`, `started_by`, `from_height`, `to_height` (the heights actually checked), `state` (`clean`, `problems`, `failed`; `running` is allowed by the table but never written: see below), `problem_count`, `blocks_checked` (blocks in the range), `rows_checked` (ledger entries of the transactions in those blocks), `problems` (a JSON array of `{check, object, detail}`, at most the first 100; `problem_count` is exact; NULL when clean). |
| `kingres.ledger_status` | one row: `append_only_tables`, `updatable_tables`, `latest_block_height`, `latest_block_hash`, `latest_block_closed_at`, `pending_transactions`, `oldest_pending_age` (the clock time since the oldest pending transaction wrote its first entry; NULL when nothing is pending or for a transaction that was queued by 0.1), `last_digest_id`, `last_digest_at`, `last_digest_height`, `last_verification_id`, `last_verification_at`, `last_verification_state`, `last_verification_problems`, and what the block-building worker knows about this database: `worker_state` (`ok`, `busy` when the builder lock was taken, `failed`, `no_extension`, `never_visited`, `not_running` when the library is not preloaded), `worker_consecutive_failures`, `worker_retry_after` (when the next attempt follows a failure) and `worker_last_error`. |

`ledger_verify_recorded` writes its row when the run is over. PostgreSQL has
no autonomous transaction, and the table is append-only, so a row in state
`running` that survives a failure of the calling transaction cannot exist. A
failure of `ledger_verify` itself (an error, not a finding) is caught: the run
is recorded with state `failed` and the error message, the caller gets a
warning and the id. A run that was **cancelled** (`statement_timeout`,
`pg_cancel_backend`, Ctrl-C) is recorded as `failed` too, with the SQLSTATE
`57014`, and the function then returns normally (it does not re-raise the
cancellation, which would roll the record back with it). What cannot be
recorded: a run that ends the session (`pg_terminate_backend`,
`transaction_timeout`, a crash, a lost connection) and a run whose calling
transaction rolls back. Both tables are append-only for every role and are
protected like the other ledger tables. `ledger_verification` is **not**
readable by PUBLIC (its problems name tables, row hashes and transaction ids, the
class of the finding L-09 of the first review); `ledger_digest` is readable
except its column `generated_by` because it holds what a digest holds,
block-level data; the view is not readable either (see "Privileges").

**Growth of the records.** Both tables are append-only for every role, so their
rows cannot be pruned, and every call of `ledger_generate_digest` (also when
nothing changed) and of `ledger_verify_recorded` adds a row (a digest row is
a few hundred bytes; a verification row holds at most the first 100 problems, a
few tens of kilobytes at worst). The decision: **these functions are for
administrators, and a role that is granted `EXECUTE` on them is trusted not to
call them in a loop**, like the holder of `ledger_enable`, which also writes
permanent rows. A rate limit inside the functions was not added because it
would bound the speed, not the total (a caller can call once per interval for
ever), and PostgreSQL has no quota for it; granting `EXECUTE` is the only
control, and it is kept off PUBLIC. A dashboard that only shows the state needs
`SELECT` on `ledger_status` (and `EXECUTE` on `ledger_worker_state`), not the
recording functions. Plan the disk for the rows of one digest and one
verification per interval of your schedule, for ever.

Settings:

| GUC | Default | Meaning |
|---|---|---|
| `kingres_ledger.max_block_transactions` | 100000 | A block is closed with at most this many transactions; the rest wait for the next block. Superuser setting (`PGC_SUSET`). |
| `kingres_ledger.block_interval` | 10s | How often the launcher looks for pending transactions; `0` switches the block-building workers off. Server setting (`PGC_SIGHUP`, `postgresql.conf` or `ALTER SYSTEM` and a reload; a database or role setting has no effect on the launcher). |
| `kingres_ledger.worker_timeout` | 60s | The longest one block-building worker may work on one database; it cancels itself after this time (an error, recorded as a failure, with a growing delay before the next attempt) and the launcher terminates it five seconds later if it does not react. A round of the launcher also stops starting workers when it has used this much time, and continues with the other databases in the next round. `0` means no limit. Server setting (`PGC_SIGHUP`). |

Errors raised by the extension: SQLSTATE `KL001` (modification of an
append-only table), `KL002` (statement rejected by the protection hook or by
the event trigger), `KL003` (library not preloaded).

## Privileges

The functions of the extension run as the role that created it (a superuser)
where they have to: this is the list, and nothing else uses `SECURITY
DEFINER`:

| Function | Why it runs as the owner |
|---|---|
| `ledger_capture` | writes the entry of every inserted row; ordinary roles have no privilege on the ledger tables |
| `ledger_record_row` | writes the entries of the rows that exist when a table is enabled |
| `ledger_enable` | creates triggers on a table that the owner does not own, and writes `ledger_table` (it checks that the calling role owns the table) |
| `ledger_build_block` | writes blocks and transactions |
| `ledger_generate_digest` | calls the block builder and writes `ledger_digest` |
| `ledger_verify` | reads every ledger table, every history table and every table under ledger control |
| `ledger_verify_recorded` | writes `ledger_verification` |
| `ledger_inclusion_proof` (both forms) | reads the entries and transactions, which the caller may not read; the two-argument form is a wrapper that exists so that grants made on it under 0.1 keep working |
| `ledger_guard` (event trigger) | must see the ledger tables whatever grants an administrator changed, so that DDL of every role keeps working; it also moves the privileges of the history tables (through `ledger_history_acl_sync`, which is not `SECURITY DEFINER` and is not executable by anybody else) |

All of them have `SET search_path = pg_catalog, pg_temp`, and every reference to
an object of the extension is qualified with `kingres.`. None of the SQL of the
extension contains a backslash or relies on a setting that a role or database can
change for its sessions (`standard_conforming_strings`, `DateStyle`, `TimeZone`,
`plpgsql.*`, `default_table_access_method` and so on): the test `ledger_review2`
runs every function with those settings changed, and `010_review2` attaches 24
such settings to a database and checks that the block-building worker is not
stopped by any of them. The test
`ledger_privs` scans `pg_proc` and fails when a function breaks one of these
rules. The only functions without a `search_path` are C primitives that compute
on their arguments and look nothing up (the list is in the test).

**Default privileges.**

| Object | PUBLIC |
|---|---|
| `kingres.ledger_table`, `kingres.ledger_chain`, `kingres.ledger_blocks` | `SELECT` |
| `kingres.ledger_digest` | `SELECT` on every column except `generated_by` (block-level data only) |
| `kingres.ledger_entry`, `kingres.ledger_pending`, `kingres.ledger_tx`, `kingres.ledger_verification` | nothing |
| `kingres.ledger_status` (view) | nothing (it shows how many transactions wait, and the state of the worker) |
| the history table of an updatable table | nothing; `SELECT` for the owner of the ledger table only, kept by the extension (see "Updatable tables"); no grant is possible |
| `ledger_history_acl_sync`, `ledger_history_sync`, `ledger_worker_state`, `ledger_lock_table`, `ledger_record_row`, `ledger_capture` | nothing |
| the identity sequences of `ledger_digest` and `ledger_verification` | nothing |
| `ledger_enable`, `ledger_build_block`, `ledger_generate_digest`, `ledger_verify`, `ledger_verify_recorded`, `ledger_verify_digest`, `ledger_inclusion_proof` | nothing |
| `ledger_row_hash`, `ledger_history_row_hash`, `ledger_verify_proof`, the hash primitives, `ledger_hook_active` | `EXECUTE` |

The entries, the work queue and the transaction table are not readable by
PUBLIC because the row hashes in them let a reader confirm a guess about the
content of a row with little entropy, and because they say which role wrote
when. The block headers, the chain identity and the list of ledger tables only
describe the chain and are public (they are in every digest and proof anyway).
No role can write any ledger table through SQL; the protection hook rejects
any `GRANT` on them other than `SELECT`, and any `GRANT` on the internal
functions.

**Who may enable a table.** The administrator decides, because it is permanent:

```sql
GRANT EXECUTE ON FUNCTION kingres.ledger_enable(regclass, text) TO table_admin;
```

The role still has to own the table (or be a member of the owning role).

**The agent that builds blocks and exports digests:**

```sql
GRANT EXECUTE ON FUNCTION kingres.ledger_build_block(),
                          kingres.ledger_generate_digest(),
                          kingres.ledger_verify_recorded(bigint, bigint) TO ledger_agent;
GRANT SELECT ON kingres.ledger_status TO ledger_agent;      -- and the management panel's role
GRANT EXECUTE ON FUNCTION kingres.ledger_worker_state() TO ledger_agent;   -- the view calls it with the caller's rights
```

(The block-building workers need no grant: they run as the extension owner.)

**An auditor role** that verifies the ledger and builds proofs needs `EXECUTE`
only (the functions run as the owner):

```sql
GRANT EXECUTE ON FUNCTION kingres.ledger_verify(bigint, bigint),
                          kingres.ledger_verify_digest(jsonb),
                          kingres.ledger_inclusion_proof(regclass, bytea),
                          kingres.ledger_inclusion_proof(regclass, bytea, "char") TO auditor;
GRANT SELECT ON kingres.ledger_verification TO auditor;       -- to read the recorded runs and their problems
GRANT inventory_owner TO auditor;                              -- to read the old versions (see below)
```

The old versions of an updatable table can be read by the owner of the ledger
table only; a `GRANT SELECT` on the history table is rejected. An auditor who must
read them is made a member of that role (`GRANT owner_role TO auditor`, with
`INHERIT`, which is the default), and that gives the auditor everything the owner
has. If that is too much, write a `SECURITY DEFINER` function that returns what the
auditor may see.

A role that also wants to read the entries, the pending transactions and the
transactions itself needs, in addition:

```sql
GRANT SELECT ON kingres.ledger_entry, kingres.ledger_pending, kingres.ledger_tx TO auditor;
```

`ledger_digest` (without the column `generated_by`) and the block headers are
readable by everybody, so a role that only checks digests needs `EXECUTE` on
`ledger_verify_digest` and nothing else.

Whoever can execute `ledger_inclusion_proof` can learn the row hashes of the
table it is called for. The verification functions cost time proportional to
the size of the ledger, which is why they are not executable by PUBLIC.

## How it works

- `ledger_enable` locks the table first, by OID, in SHARE ROW EXCLUSIVE mode
  (the caller must own it), so concurrent writers wait and every row committed
  before the lock is seen; after that it works from the OID. It creates three
  triggers, all `ENABLE ALWAYS` so that `session_replication_role = replica`
  does not skip them: `kingres_ledger_capture` (AFTER INSERT, row level,
  written in C), `kingres_ledger_block_modify` (BEFORE UPDATE OR DELETE, row
  level) and `kingres_ledger_block_truncate` (BEFORE TRUNCATE); arms the
  capture trigger last; registers the table in `kingres.ledger_table` (this
  gives the stable `table_id` that is used inside hashes); and enters the rows
  the table already has in the enabling transaction. Stored and virtual
  generated columns are rejected: a virtual column has no stored value, and a
  stored one depends on an expression that a dependency cascade or a later
  `ALTER` could drop, which would drop the column and change every hash.
- The capture trigger appends a row to `kingres.ledger_entry` for every
  inserted row (transaction id of the top-level transaction as `xid8`, a
  sequence number inside the transaction, the row hash) and queues the
  transaction in `kingres.ledger_pending` when it writes its first entry. The
  per-transaction counter lives in backend memory, is reset by transaction
  callbacks, and the "queued" flag follows subtransactions: if the
  subtransaction that queued the transaction is rolled back, the next entry
  queues it again. Sequence numbers are unique and ordered but may have gaps.
- For an updatable table the same trigger function is an `AFTER INSERT OR UPDATE
  OR DELETE` row trigger (the blocking trigger for update and delete is not
  created; `TRUNCATE` is blocked). On `UPDATE` and `DELETE` it hashes the old
  tuple (`tg_trigtuple`) and writes the `D` entry, then writes the old tuple into
  the history table (matched by column name, same types, through the table
  access method: the exact old tuple, no copy through text), then, for
  `UPDATE`, hashes the new tuple (`tg_newtuple`) and writes the `I` entry. The
  tuples are those the executor queued for the trigger, fetched by tuple id, so
  they are the versions that were really stored, whatever happened to the row
  afterwards in the same statement. The trigger argument (the table id) is
  checked against the registry for `UPDATE` and `DELETE`.
- `ledger_build_block` takes a table lock on `kingres.ledger_chain` (EXCLUSIVE
  mode; only the extension owner can take it, so no other role can stop block
  building), reads the pending transactions (at most `max_block_transactions`,
  lowest transaction id first), computes each transaction's Merkle root,
  writes `ledger_tx` and `ledger_blocks`, and deletes the processed rows from
  `ledger_pending`. A transaction that commits after the builder's snapshot is
  picked up by the next block. Order inside a block is by transaction id, not
  by commit order.
- **The block-building workers** (`src/worker.c`). A launcher is registered at
  library load (it needs `shared_preload_libraries`; it starts when recovery has
  finished, so a standby has none). Every `kingres_ledger.block_interval` it
  starts, one at a time, a short-lived worker for each database it decides to
  look at; the worker connects to the database, runs as the extension owner,
  does nothing if the extension is not installed or nothing is pending, and
  otherwise calls `ledger_build_block()` once and exits. The settings that a
  database owner or a role owner attaches to the database or the role
  (`ALTER DATABASE ... SET`) are applied during the connection, before any code of
  the extension runs; the worker then overrides, with the highest priority and
  before its transaction starts, every setting that can make it fail or stall:
  `transaction_timeout`, `idle_in_transaction_session_timeout`,
  `idle_session_timeout`, `statement_timeout`, `default_transaction_isolation` (READ
  COMMITTED), `default_transaction_read_only`, `default_transaction_deferrable`,
  `transaction_read_only`, `search_path`, `session_replication_role`,
  `row_security`, `check_function_bodies`, `standard_conforming_strings`,
  `plpgsql.variable_conflict`, `plpgsql.extra_errors`, `plpgsql.extra_warnings`,
  `jit`, `debug_parallel_query` and `temp_tablespaces`, and a lock timeout of one
  second (or the interval, if shorter) for the builder lock: if another builder
  holds it, the worker logs a message, gives up and the next round tries again.
  The background worker API has no flag that skips the settings, so a setting
  that kills the session during the connection itself cannot be overridden; such
  a failure is handled like any other. **A worker that fails** logs one line,
  records the failure (state, error, count) in shared memory, which
  `kingres.ledger_status` shows, and is not started again for a delay that doubles
  with every failure (the interval, up to an hour); the first success clears it.
  Nothing logs at `FATAL` every interval. **The cost of a sweep**: the launcher
  remembers the outcome of each visit, so a database where a worker found no
  extension is left out of the next nine sweeps (without the extension nothing can
  be pending, and a database that gets the extension and a ledger transaction is
  marked dirty by the commit and visited at once), and a database in its delay
  is left out. What it cannot know without connecting is whether the extension is
  installed in a database and whether anything is pending there, so the first
  sweep after a start connects once to every database. **Fairness**: a worker is
  cancelled after `kingres_ledger.worker_timeout`, databases are visited in an
  order that continues where the last sweep stopped, and a round that has used
  `worker_timeout` leaves the other databases to the next round; one slow or
  failing database delays the others by at most about that time, and a database
  whose single block needs longer than `worker_timeout` is never finished by the
  worker (lower `max_block_transactions` for it). When `max_worker_processes` is
  used up (a launcher takes one slot, a worker another) no worker starts, the
  launcher says so in the log (at the first round and then every sixty rounds),
  transactions stay pending, `ledger_status` shows their number and age, and a call
  of `ledger_build_block()` or `ledger_generate_digest()` still builds the block. If more than
  `max_block_transactions` transactions were pending, the next round builds
  the next block. The launcher cannot look into a database, so a backend that
  commits a transaction with ledger entries puts its database into a small set
  in shared memory, and the launcher visits those databases only; it visits
  every database in its first round after the server starts or is promoted,
  and again every sixth round (a transaction that was prepared and is
  committed later, and an overflow of the set, are covered by that). A
  committed transaction is therefore in a block within about one interval, a
  committed prepared transaction within six. `block_interval = 0` (or a
  reload that sets it) switches the workers off; the launcher then idles. Every
  database needs `max_worker_processes` to have a free slot for the worker.
  The blocks are the same ones `ledger_build_block()` builds by hand; both can
  run together (they serialise on the builder lock).
- Logical replication into a ledger table works: the apply worker runs with
  `session_replication_role = replica`, the `ENABLE ALWAYS` triggers fire, and
  the rows are captured as the owner of the subscription. For an append-only
  table a replicated `UPDATE`, `DELETE` or `TRUNCATE` is rejected by the
  blocking triggers, so the apply worker stops with an error until the
  subscription is changed; for an updatable table they are captured like local
  changes (`TRUNCATE` is rejected: the apply worker then fails and retries until
  the subscription is disabled, as for any statement that the ledger refuses).
  A change that the apply worker skips because the row is missing on the
  subscriber (an update or delete of a row that is not there) writes no entry.
  A publication can name a ledger table and its history table. Tested in
  `010_review2`.
- `COPY ... FREEZE` works for a ledger table in both orders (loaded frozen and then
  enabled in the same transaction, or enabled and then loaded frozen): the rows are
  captured by the trigger or entered by `ledger_enable`, and the ledger verifies.
- A hot standby can verify the ledger, check digests and build and check
  proofs; it cannot enable tables or build blocks (it is read-only).

## Hash format version 1

All hashes are SHA-256 (the `pg_cryptohash` API). All integers in hash inputs
are unsigned and big-endian unless stated. `||` is concatenation. The format is
selected by `hash_version` (1); a later format must not change how existing
hashes are computed. Nothing has been released, so version 1 was changed in
place by the hardening described here.

**Merkle trees (RFC 6962).**
`leaf_hash(data) = SHA256(0x00 || data)`;
`node_hash(l, r) = SHA256(0x01 || l || r)`.
For `n` leaf hashes `h[0..n-1]`: `MTH([]) = SHA256("")` (the empty string, so
`e3b0c442...b855`), `MTH([h0]) = h0`, and for `n > 1`, with `k` the largest
power of two smaller than `n`, `MTH = node_hash(MTH(h[0..k-1]), MTH(h[k..n-1]))`.
Audit paths and their verification are those of RFC 6962 section 2.1.1 and
RFC 9162 section 2.1.3.2 (the verification algorithm that takes the leaf index
and the tree size). The SQL function `ledger_leaf_hash` is the plain RFC 6962
leaf hash of any data; the extension's own hashes use the tags below.

**Domain tags.** Every kind of hashed data starts with its own one-byte tag, so
that data of one kind can never be read as data of another:

| Tag | Value | Where it is the first byte |
|---|---|---|
| row | `0x10` | the canonical row encoding, the data of the row's leaf |
| entry | `0x11` | the leaf data of an entry in a transaction tree |
| block transaction | `0x12` | the leaf data of a transaction in a block tree |
| block | `0x13` | the input of the block hash |
| chain | `0x14` | the input of the hash that gives the `prev_hash` of block 0 |

The values avoid `0x00` and `0x01`, the RFC 6962 prefixes.

**Canonical row encoding** (the data of the row's leaf):

```
u8   tag 0x10
u16  hash_version (1)
u32  table_id
u16  number of encoded columns (non-dropped columns whose value is not NULL)
for each such column in attribute order (attnum ascending):
    u16  length in bytes of the column name, then the name in UTF-8
    u32  length in bytes of the value, then the value
```

The value is the output of the type's binary send function (`typsend`),
without the length header that the frontend/backend protocol adds. Text is
never used, so `DateStyle`, `extra_float_digits`, `bytea_output`,
`IntervalStyle` and `TimeZone` have no influence. The send functions of
text-like types (`text`, `varchar`, `bpchar`, `name`, `json`, `jsonb`, `xml`,
enum labels and others) convert to the client encoding; the extension
therefore switches the client encoding to UTF-8 while it runs the send
functions (and restores it afterwards), so the bytes are UTF-8 whatever the
database encoding and `client_encoding` are. (For a `SQL_ASCII` database no
conversion takes place and the bytes are the stored bytes.) NULL columns are
omitted, so adding a nullable column later does not change the hash of
existing rows. `row_hash = leaf_hash(encoding)`.

*Edge cases of the encoding.* Columns are taken in `attnum` order, that is, in
the order in which they were created; a dropped column is skipped (the
`attnum` of the others does not change), and a column added later comes last,
whatever the names. A value of zero length (an empty string, an empty `bytea`,
an empty array) is **not** NULL: it is encoded with `u32 length = 0` and counts
in the number of encoded columns. A NULL column is not encoded at all and does
not count. The column name is the name stored in the catalog, converted from
the database encoding to UTF-8 like text values are (for a `SQL_ASCII`
database the stored bytes); its length is a `u16` in bytes (the name is at
most 63 bytes in the database encoding, so at most 252 in UTF-8, and a table
has at most 1600 columns, so no limit of the encoding is ever reached; the
implementation raises an error for a name longer than 65535 bytes). There is no
padding and no alignment anywhere.

*The value bytes.* They are the bytes that the type's binary send function
(`typsend`) produces, without the length word of the frontend/backend
protocol. For example: `int4send` is 4 bytes, big-endian; `int8send` 8;
`boolsend` 1 byte (`0` or `1`); `float8send` the 8 bytes of the IEEE double,
big-endian (`NaN` and `-0` as they are); `textsend` and `varcharsend` the
UTF-8 bytes of the text; `bpcharsend` the UTF-8 bytes including the padding;
`byteasend` the bytes; `timestamptzsend` and `timestampsend` the `int64`
microseconds since 2000-01-01 00:00:00 (UTC for `timestamptz`); `date_send`
the `int32` days since 2000-01-01; `numeric_send` the PostgreSQL binary numeric
(`ndigits`, `weight`, `sign`, `dscale`, and the base-10000 digits, each a
`u16`); `uuid_send` the 16 bytes; `jsonb_send` a version byte `1` followed by
the text of the document in the canonical `jsonb` output form; `array_send`
the array header with the element type OID (so arrays of types with an OID of
16384 or more are rejected) and the elements, each with a `u32` length (-1
for NULL). Outside the server, in SQL on any server: `SET client_encoding =
'UTF8'; SELECT encode(int4send(42), 'hex'), encode(textsend('abc'), 'hex')`;
the send function of any other type is named in `pg_type.typsend`. The text
send functions convert to `client_encoding`, which is why the extension forces
UTF-8 and why you must set it to get the same bytes. Types whose binary form
is not stable, or that have none, are rejected by `ledger_enable`
(`kingres.ledger_type_problem(oid)` says why); a type whose binary form
changes between server versions would change the hashes of existing rows, and
nothing prevents the server from being upgraded to such a version (see
"What the protections cannot stop").

`ledger_enable` rejects columns whose binary form is not stable or does not
exist: types without a binary send function, composite types and arrays of
composite types (the binary form contains type OIDs), arrays whose element type
is user-defined (`array_send` writes the element type OID) or itself not
supported, and generated columns. `ADD COLUMN` on a ledger table passes the same
checks (the event trigger applies them).

**Transaction tree.** The leaves of a transaction are its entries in `seq`
order: `leaf_hash(0x11 || u32 seq || u32 table_id || u8 op || row_hash)` with
`op = 0x49` (ASCII `I`, an inserted row version) or `op = 0x44` (ASCII `D`,
**new in 0.2**: a row version that was removed by an `UPDATE` or a `DELETE`;
`row_hash` is then the hash of the version that was removed, computed exactly
like that of an inserted row). The JSON value `"I"` of the `op` member of a
proof is the byte `0x49`, `"D"` is `0x44`. The entry leaf data is 42 bytes
(`1 + 4 + 4 + 1 + 32`). `merkle_root` of the transaction is `MTH` over these
leaf hashes (a transaction row in `ledger_tx` also stores `entry_count`). The
format of the leaf, the tags, the trees, the block hash and the chain
identity are exactly those of 0.1; only the set of values `op` may have grew.
An `UPDATE` writes `D` (old version) then `I` (new version) in consecutive
`seq` positions of the same transaction (for nested changes by triggers the
order of the entries is the order in which the capture trigger ran); a
verifier of the tree does not need to know the meaning of `op`.

**What the entries mean for a table** (not part of the hash format, but what
`ledger_verify` checks): for every row hash `h` of a table, the number of rows
of the table with hash `h` equals the number of `I` entries with `h` minus the
number of `D` entries with `h`. For an updatable table each `D` entry has a
history row with the same `ledger_txid` and `ledger_seq` whose columns hash to
`h` (`ledger_history_row_hash`, which encodes the history row as the table's
row: the columns other than `ledger_txid`, `ledger_seq` and `ledger_op`, in
order, with the `table_id` of the ledger table).

**Block tree.** The leaves of a block are its transactions in `tx_index` order
(ascending `ledger_txid`, `tx_index` from 0):
`leaf_hash(0x12 || u64 ledger_txid || u32 entry_count || merkle_root || u16 n || user_name)`
where `user_name` is the name of the user who wrote the transaction in UTF-8
and `n` is its length in bytes (at most 255; the fixed-width fields come first
and the name is length prefixed, so the encoding is unambiguous).
`tx_merkle_root = MTH` over these leaf hashes. The name is therefore covered by
the block's transaction root, the block hash and every digest.

**Which user.** The name is the value stored in `ledger_pending.user_name` when
the transaction wrote its first entry (copied unchanged to `ledger_tx` by the
block builder). It is the current user of the statement that inserted the row,
as seen outside any `SECURITY DEFINER` function: after `SET ROLE` it is the role
switched to, not the session user, and it is never the extension owner that the
capture trigger runs as. The trigger function and `ledger_enable` run as the
owner, so the outer user is the only identity that still describes the caller.
Only the first entry of a transaction records the name; a later `SET ROLE` in the
same transaction does not change it. The name is captured before the block
exists, see the limitations.

*Encoding of the name.* `name` values are stored in the database encoding. In
the hash the name is **UTF-8**: for a database whose encoding is not UTF-8 it
is converted from the database encoding like text values are (for `SQL_ASCII`
the stored bytes are used). A role name has at most 63 bytes in the database
encoding, at most 252 in UTF-8, so the limit of 255 bytes that the proof
check applies never truncates anything and no truncation rule exists. (The
proof check of 0.1 hashed the name of a proof as it stood in the JSON, that
is, in the database encoding, which is the same bytes only for UTF-8 and ASCII
names; 0.2 converts it like the block builder does.)

**Chain identity.** `kingres.ledger_chain` has one row, written when the
extension is created: `chain_id`, 32 random bytes (`SHA-256` of two random
UUIDs). The `prev_hash` of block 0 is `SHA256(0x14 || chain_id)` instead of 32
zero bytes. `ledger_verify` checks that block 0 starts from the stored
`chain_id`; the digest and every proof carry `chain_id`, and
`ledger_verify_digest` and `ledger_verify_proof` reject a digest or proof of
another chain. The table is protected like the other ledger tables.

**Exporting the chain for an independent check.** `ledger_blocks` is readable
by PUBLIC; the block headers and the chain identity are
`SELECT chain_id FROM kingres.ledger_chain` (the chain identity is also in
every digest and every proof) and

```sql
SELECT height, hash_version, prev_hash, tx_merkle_root, tx_count,
       kingres.ledger_timestamptz_us(closed_at) AS closed_at_us, block_hash
FROM kingres.ledger_blocks ORDER BY height;
```

(`ledger_blocks` itself has `closed_at timestamptz`, not `closed_at_us`; the
function returns the `int8` microseconds since 2000-01-01 UTC that the block
hash covers.) These are the same members a proof carries in `block` and in the
elements of `chain`. The chain is valid when block 0 has `prev_hash =
SHA256(0x14 || chain_id)`, every block hash is recomputed as in "Block hash"
and equals `block_hash`, every `prev_hash` equals the hash of the previous
block, the heights are 0, 1, 2, ... without gaps, `hash_version` is 1 in every
block and `closed_at_us` never decreases. Each block's transactions
(`ledger_tx`, not readable by PUBLIC) and entries (`ledger_entry`) are then
checked against `tx_merkle_root` as in "Block tree" and "Transaction tree".

What this does: a digest or proof of one ledger is not accepted as belonging
to another, and two ledgers that were created independently have different
identities with overwhelming probability, so a chain cannot be taken for a
different one. What it does not do: databases that are copies of each other (a
`pg_basebackup`, a restore of the same backup, `CREATE DATABASE ... TEMPLATE`)
share the identity until they diverge, and their digests are accepted by each
other as long as the blocks agree; the identity is not secret, and nothing
authenticates who created it; the digest fields `database` and the proof field
`relation` are informational and not authenticated. For a proof of a block
after block 0 the identity is not inside any hash that the proof contains
(only block 0 hashes it): the check is that proof and digest name the same
chain, and the security of the proof comes from the block hash of the digest.

**Block hash.**

```
block_hash = SHA256(0x13 || u16 hash_version || u64 height || prev_hash
                    || tx_merkle_root || u32 tx_count || i64 closed_at)
```

The input is 87 bytes (`1 + 2 + 8 + 32 + 32 + 4 + 8`). `closed_at` is the
timestamp in microseconds since 2000-01-01 00:00:00 UTC (the internal value of
`timestamptz`; `kingres.ledger_timestamptz_us` returns it, and the proof
carries it as `closed_at_us`), and `prev_hash` is `SHA256(0x14 || chain_id)`
for height 0. The block transaction leaf data is `1 + 8 + 4 + 32 + 2 + n` =
47 + `n` bytes for a user name of `n` bytes.

**Block time.** `closed_at` is the clock of the database server when the
block was built. It never goes back: the builder uses the `closed_at` of the
previous block when the clock is behind it, `ledger_verify` and
`ledger_verify_digest` check that no block is earlier than the one before, and
a proof's chain of headers is checked the same way. A clock that was set far
ahead keeps every later block at that time until real time passes it. The time
is evidence only relative to an external anchor (a digest that is stored with
a timestamp from a source you trust); inside the database it is the word of the
server.

**JSON.** Digests and proofs are JSON documents (the server holds and
produces them as `jsonb`, so the order of the members and the white space are
not significant: `jsonb` sorts the members and normalises the spacing, and a
verifier must not depend on either). Hex strings are lower case and exactly
64 digits for a 32-byte value (upper case digits are accepted by the server;
a verifier may be strict). Rules that apply to every number of a digest or a
proof:

- A number must be an integer in the range given below. A number that is not
  an integer in value (`1.5`) is rejected; one that is out of range is
  rejected. The server function receives `jsonb`, which has already parsed the
  number: leading zeros and a plus sign are not JSON and fail when the text is
  turned into `jsonb`, and a number that is integral in value is accepted
  whatever its spelling (`1.0`, `1e2` and `-0` are read as `1`, `100` and `0`).
  The server writes plain decimal integers only; an independent verifier that
  works on the text may, and should, reject any other spelling.
- Ranges: `table_id`, `seq`, `entry_index`, `tx_index`, `entry_count` and
  `tx_count` are 32-bit signed integers (0 to 2147483647), with `entry_count`
  at least 1, `entry_index < entry_count` and `tx_index < block.tx_count`
  (so `block.tx_count` is at least 1); `ledger_txid` and `height` (also the
  height of a digest) are 64-bit signed integers (0 to 9223372036854775807);
  `closed_at_us` is a 64-bit signed integer; `hash_version` and `format` must
  be exactly 1. The `tx_count` of an element of `chain` may be 0 to
  2147483647.
- Members: **every member shown in the example is required**, except
  `relation` (informational) and `block_hash` in the elements of `chain`
  (optional; when present it must be a well-formed hash and is checked against
  the recomputed one; a malformed value is an error, not an absent member).
  Unknown members are ignored. A member of the wrong JSON type is an error.
  The server cannot see a repeated member (`jsonb` keeps the last value of a
  key); an independent verifier that reads the text must reject an object that
  has the same key twice.

**Digest.** A JSON object; `chain_id`, `height` and `block_hash` (lower case hex
of the bytes) are what `ledger_verify_digest` and `ledger_verify_proof` read
and require; the others are informational and not authenticated:

```json
{"database": "shop", "chain_id": "3f1c...", "hash_version": 1, "height": 8,
 "block_hash": "e3b0...", "block_closed_at": "2026-11-14T10:20:31.000000Z",
 "generated_at": "2026-11-14T10:21:00.000000Z"}
```

`block_closed_at` is the `closed_at` of block `height` and `generated_at` the
server time when the digest was made, both RFC 3339 timestamps in UTC with six
digits of microseconds and a `Z`; `block_closed_at` equals `closed_at_us` of
that block (here 847966831000000), and a verifier that holds the block header
may check it (the server does not look at it). `generated_at`, `database`
and `hash_version` are not read by the server's checks; generated digests
always have `hash_version` 1, and a verifier should require it. An empty
ledger has no block, so `ledger_generate_digest()` returns NULL for it: there
is no digest, and no proof, until the first block exists.

*Several digests of one chain*, taken at different times, are consistent when:
they have the same `chain_id`; two digests with the same `height` have the same
`block_hash`; and of two digests with different heights the one with the larger
height has a `block_closed_at` that is not smaller. `ledger_verify_digest`
checks one digest against the stored chain; the comparison of a set of stored
digests is the job of whoever stores them.

**Inclusion proof.** A JSON object (the rules above apply):

```json
{"format": 1, "chain_id": "3f1c...", "relation": "public.t", "table_id": 3,
 "ledger_txid": 1234, "seq": 1, "op": "I", "row_hash": "<hex>",
 "entry_index": 1, "entry_count": 5, "entry_path": ["<hex>", ...],
 "tx_root": "<hex>", "user_name": "alice", "tx_index": 2, "tx_path": ["<hex>", ...],
 "block": {"height": 7, "hash_version": 1, "prev_hash": "<hex>",
           "tx_merkle_root": "<hex>", "tx_count": 4,
           "closed_at_us": 847966830123456, "block_hash": "<hex>"},
 "chain": [{"height": 8, "hash_version": 1, "prev_hash": "<hex>",
            "tx_merkle_root": "<hex>", "tx_count": 1,
            "closed_at_us": 847966831000000, "block_hash": "<hex>"}, ...]}
```

`op` is `"I"` or `"D"` (a one-character string; any other value is
rejected); a proof of a `D` entry proves that the row version with that
`row_hash` was removed in that transaction. `entry_index` is the position of
the entry among the entries of its transaction in `seq` order (not the value
of `seq`, which may have gaps); `tx_index` is the position of the transaction
in its block; `chain` lists the headers of the blocks after the proof's block
up to the latest block at the time the proof was made. `relation` is
informational. `table_id` is a local number that is part of the row encoding
and of the entry leaf, so the proof binds the table: a proof for a row of one
table does not verify as a proof for the same bytes in another table.
`user_name` is the UTF-8 name described under "Which user".

**What a proof proves, and what the verifier must do.** A proof says that an
entry with this `row_hash` and this `table_id` is in the ledger. It cannot know
the row. To learn that a row you hold is in the ledger, **recompute the row
hash from that row** (encode it as above with the `table_id` of the proof, or
call `ledger_row_hash` on a server that has the table) and compare it with the
`row_hash` of the proof; the four-argument `ledger_verify_proof` does that
comparison for the caller, and the five-argument form also fixes the
operation. An independent verifier, given a trusted digest `(C, H, D)` (chain
identity, height, block hash):

0. checks that the proof is well formed (the rules under "JSON"), that
   `format` is 1, that `op` is `I` or `D` (and the one it expects), that the
   `chain_id` of the proof is `C`, and, for the row it holds, that `row_hash`
   is the hash it computed and `table_id` is the table it means;
1. computes `e = leaf_hash(0x11 || u32 seq || u32 table_id || u8 op || row_hash)`
   and checks that the audit path `entry_path` leads from `e`, at index
   `entry_index` in a tree of `entry_count` leaves, to `tx_root`; the path has
   at most 64 hashes, the tree has at least one leaf and the index is smaller
   than the size;
2. computes `t = leaf_hash(0x12 || u64 ledger_txid || u32 entry_count || tx_root ||
   u16 n || user_name)` (`user_name` from the proof, in UTF-8, at most 255
   bytes, valid UTF-8, `n` its length) and checks that `tx_path` leads from
   `t`, at index `tx_index` in a tree of `block.tx_count` leaves, to
   `block.tx_merkle_root` (the same bounds);
3. recomputes the block hash from the header fields of `block` (`hash_version`
   must be 1; the hash must equal `block.block_hash`; for height 0 the
   `prev_hash` must be `SHA256(0x14 || C)`);
4. if `block.height == H`, checks that this hash equals `D` (and ignores
   `chain`); if `H` is greater, walks `chain`: the element for height `h + 1`
   must have `hash_version` 1, `prev_hash` equal to the hash of the block at
   `h`, a `closed_at_us` not smaller than that of the block at `h`, and its own
   hash is recomputed the same way (and must equal its `block_hash` when it has
   one), until height `H`, whose recomputed hash must equal `D`; if `H` is
   smaller than `block.height`, or `chain` ends before `H`, the check fails.
   **Elements of `chain` with a height greater than `H` are not used**: they
   are parsed under the same rules (a malformed one makes the proof invalid)
   but their links and hashes are not checked, because a proof made after the
   digest legitimately has them and they say nothing about the digest. So a
   proof is valid against any digest whose height is at least the proof's block
   height and at most the height of the last element of `chain`, and a change
   to an element beyond `H` is not detected by the digest of height `H`.

The tree sizes (`entry_count`, `block.tx_count`) are authenticated by the
transaction leaf and by the block hash respectively; an audit path alone does
not authenticate the size of its tree (a path built for a tree of `n` leaves
also verifies at some indexes of a tree of `n + 1` leaves, which is why the
sizes come from the authenticated values and the index must be checked against
them). A proof for a row that is in a block older than the digest needs the
chain of headers in between, so its size is linear in the number of blocks
between them.
## Protection against being switched off

Two layers enforce the rules, both for every role including superusers, both
with SQLSTATE `KL002`.

**The `ProcessUtility` hook** (installed only when the library is preloaded)
looks at a statement before it runs. On a table under ledger control it rejects
`DROP TABLE`, `ALTER TABLE ... DISABLE/ENABLE [REPLICA | ALWAYS] TRIGGER` (every
form), `DROP COLUMN`, `ALTER COLUMN ... TYPE`, `ALTER COLUMN ... SET EXPRESSION`,
`RENAME COLUMN`, `ATTACH/DETACH PARTITION`, `SET SCHEMA`, `DROP TRIGGER` and
`ALTER TRIGGER ... RENAME` and `CREATE [OR REPLACE] TRIGGER` of the ledger
triggers, `SET LOGGED/UNLOGGED`, `INHERIT`, `CREATE RULE`, and `ADD COLUMN`
unless the new column is nullable, has no default and is not generated or
identity. A ledger table can have no children of any kind and cannot be a child:
`CREATE TABLE` and `CREATE FOREIGN TABLE` with `INHERITS` or `PARTITION OF`,
`ALTER TABLE ... INHERIT` and `ATTACH PARTITION` are rejected (`LIKE` is fine).
On the `kingres.ledger_*` tables it rejects every ALTER, RENAME, CREATE INDEX,
CREATE TRIGGER, CREATE RULE, CREATE POLICY, DROP and SET SCHEMA, and every
`GRANT` of anything but `SELECT`. It rejects renaming, replacing, altering,
re-owning and granting on the functions and event triggers of the extension
(they are recognised by membership of the extension and by their C symbol, never
by their name; internal functions never get a grant), `ALTER EXTENSION ...
ADD/DROP/SET SCHEMA`, `ALTER SCHEMA kingres RENAME`, `DROP EXTENSION
kingres_ledger` while any ledger table exists, `DROP SCHEMA` and `DROP OWNED`
that would drop a ledger table, `DROP TYPE/DOMAIN ... CASCADE` and `DROP
EXTENSION ... CASCADE` that would drop a column of a ledger table, and `ALTER TYPE
... RENAME VALUE` of an enum that a ledger column uses (the label is part of the
row hash). The hook does nothing in a database where the extension is not
installed. It always calls the previous hook.

**No "inside our own code" switch.** The hook has no global flag, setting or
state that turns a check off while the extension works. The audit of every such
state is in `kingres/docs/design/ledger-0.2.md` ("Notes of the second review
fixes"); the result: the extension's own change of a history table (`ADD
COLUMN`, the privileges) is made through the catalog API, not as a statement, so no
statement is run in a privileged window and no event trigger of a user fires in
it, and the one remaining exception is the **statements of the extension's own
install or upgrade script**, identified by the text of the script (the query
string that `CREATE EXTENSION` and `ALTER EXTENSION UPDATE` pass for each of
its statements, which a statement of an event trigger or a function never has),
and only while that script runs. A statement that an event trigger, a function
or a default expression runs meanwhile goes through every check. A chain of
upgrade scripts in one command would need that rule extended (only one step
exists).

A table is "under ledger control" for the hook when it is registered in
`kingres.ledger_table` or its capture trigger is `ENABLE ALWAYS` (`ledger_enable`
arms that trigger before it registers the table).

**The history table** of an updatable table is part of the ledger: the hook
treats it like a table of the ledger catalog (registered as `history_relid` in
`kingres.ledger_table`; it is not a member of the extension, so the registry is
what identifies it). Every `ALTER TABLE`, rename, `SET SCHEMA`, `DROP`, index,
trigger, rule or policy on it is rejected, and so is every `GRANT` (its privileges
follow the owner of the ledger table; the extension's own `ADD COLUMN` that
follows an `ADD COLUMN` of the table is made through the catalog API and does
not pass the hook). Dropping its columns or the table, or rewriting it, is rejected by the
event triggers too, and so is any state of it that is not what `ledger_enable`
made (a missing trigger, a constraint, an index, a column that does not belong,
different column types or order). The updatable table itself is protected like
an append-only one, with the capture trigger of the other shape (`AFTER INSERT
OR UPDATE OR DELETE`), no blocking trigger for update and delete, and the
names `ledger_txid`, `ledger_seq` and `ledger_op` forbidden for columns.

**Names are resolved once, with the lock.** A statement resolves the names in it
again after the hook has looked at them. If the hook only looked, a concurrent
rename could move the name to another table in between, and the statement would
change a ledger table that the hook had not checked (this was possible for the
owner of two tables, without any superuser right: finding L-03). For the
statements that name a table, the hook therefore resolves the name once, takes
the lock that the statement is going to take, and decides on the OID; while the
lock is held the name cannot move. A role that does not own the table takes no
lock (so it cannot make anybody wait), and a second role that cooperates can
still move a name under such a statement; the next layer covers that.

**The event triggers** (`ddl_command_end`, `sql_drop`, `table_rewrite`; all
`ENABLE ALWAYS`) enforce the rules at the point of action, by OID, from what a
statement did, and roll it back with an error when it dropped a ledger table, a
column of a ledger table or of the ledger catalog (whatever dependency path led
there, `DROP ... CASCADE` included), when ALTER TABLE rewrites a ledger table,
and, after any DDL command, when a ledger table or the ledger catalog is no
longer what `ledger_enable` made it: a trigger missing, disabled, of another
shape or with another function; a child or parent table; a partition; rules; an
unlogged table; a generated column; a column of a type that `ledger_enable`
rejects (so `ADD COLUMN` of a composite or an array of a user type fails here);
row security or policies on a ledger catalog table; a missing event trigger.
This does not depend on any name resolution before the statement runs.

`ledger_verify` runs the same checks on the state of the database, so a bypass
that did get through is reported, and it checks `pg_inherits` for every ledger
table and reports any child. A table that cannot be checked (a hostile or
broken one) is reported as a finding for that table and the verification goes on
with the others.

### What the protections cannot stop

This is the complete list that is known. Each item is either detected after the
fact by `ledger_verify` (and always by a digest taken before it), or outside what
the database can see.

- Direct modification of the system catalogs (for example `UPDATE pg_trigger`
  by a superuser), and anything done at file level (a tool that edits heap
  pages, such as `pg_surgery`), or to a copy of the data directory.
- Switching event triggers off: a superuser can run `SET event_triggers = off`
  (or set it in the configuration, or for a role or database), which removes
  the point-of-action layer; the hook stays. Disabling an event trigger of the
  extension is rejected by the hook and reported by `ledger_verify`.
- Removing `kingres_ledger` from `shared_preload_libraries` and restarting:
  from then on the triggers still fire and the event triggers still guard
  (they are SQL), but the hook is gone. A server that was started without the
  library is detected by `ledger_verify` only as far as it changes ledger
  state.
- The bodies of the functions. The event triggers check that the trigger
  functions are still the C functions of the extension (membership and C
  symbol), but nothing compares the SQL and PL/pgSQL functions (`ledger_verify`,
  `ledger_verify_digest`, ...) with what the script installed. `CREATE OR
  REPLACE` of them is rejected by the hook, by schema and name prefix; a
  replacement by a catalog edit is not seen. A verification that was replaced
  tells nothing: run the verification of a copy, with an independent verifier,
  against a digest that was stored outside.
- Changes that only the hook rejects and the event triggers do not see:
  `RENAME COLUMN` and `ALTER COLUMN TYPE` that does not rewrite the table, when
  a second, cooperating role moves a name under the statement (see above), or
  when the hook is not there. The new column name or a changed binary form
  changes the row hashes, and `ledger_verify` reports every row. Also not seen by
  the event triggers: a nullable column added to a ledger catalog table (the
  functions name the columns they use, so this changes nothing the ledger
  covers).
- A table that was renamed, or replaced: the ledger follows a table by OID, not
  by name. A ledger table can be renamed (that is allowed) and a new table
  created under the old name; the ledger keeps verifying, while applications
  read the new, unledgered table. Compare the name of `ledger_table.relid`
  with what applications use.
- The window before a transaction is in a block and a digest of that block has
  left the database; `ledger_pending` is not covered by any hash.
- Changes of the binary send format of a type that a ledger column uses (for
  example by upgrading the extension that provides the type) are not
  prevented: the existing row hashes then no longer match and `ledger_verify`
  reports every row of the table.
- Dropping the database, restoring an older backup, a copy of the data
  directory, or a replacement of the server binary. Only verification against an
  external digest detects these. A restored or cloned database has the same
  chain identity as the original until they diverge.
- Dump and restore (`pg_dump`): the ledger tables are not dumped. A restored
  database has the triggers of its tables but an empty ledger; inserts into a
  restored ledger table fail (the entry refers to a table id that is not
  registered), and `ledger_verify` reports the capture trigger of an unregistered
  table. Use physical backups and standbys.
- The clock: block times are the server's clock, kept from going backwards.
- **Updatable tables.** Everything that changes a row through SQL fires the
  capture trigger, which is `ENABLE ALWAYS`; the ways around it are the ones
  above (catalog edits, event triggers off for the `ADD COLUMN` follow-up,
  file level) and these, each detected by `ledger_verify` after the fact:
  a current row changed, removed or put back (an old version as the current row)
  without entries; a history row changed, removed, duplicated, invented or
  with another operation; a `D` entry removed together with its history row
  (the transaction root of the block no longer matches, and the table no longer
  equals its entries); an entry turned from `I` into `D` or back. Against a
  digest taken before the change all of these are detected, including a
  consistent rewrite of the entries, the blocks and the history table. What the
  verification cannot tell is whether a legitimate-looking change was
  authorised.
- **With `event_triggers = off`** an `ALTER TABLE ... ADD COLUMN` on an updatable
  table does not extend its history table. The next `UPDATE` or `DELETE` of the
  table then fails (the capture function refuses a history table that lacks a
  column), so nothing is recorded wrongly, and `ledger_verify` reports
  `history_columns`; inserts, which write no history row, continue.
- **The workers** can be switched off (`kingres_ledger.block_interval = 0`, a
  failed launcher, `max_worker_processes` exhausted, a standby), or kept from
  finishing by what the database settings cannot reach but a superuser can (a
  broken chain row, a lock held on `ledger_chain`, a synchronous standby that
  never answers, a block that needs longer than `kingres_ledger.worker_timeout`):
  blocks are then built only when something calls `ledger_build_block()`, and the
  window before a transaction is in a block grows. Settings that a database owner
  or a role owner attaches to the database or the role do not stop the worker
  (see "How it works"), except one that kills the session during the connection
  itself, which the background worker API does not allow to be skipped; that
  failure is retried with a growing delay and shown in `ledger_status`. `kingres.ledger_status` shows the
  number of pending transactions and the age of the oldest; nothing raises an
  alarm by itself. `ledger_pending.queued_at` (like the rest of the work queue) is
  not covered by any hash.
- **The records for operators** (`ledger_digest`, `ledger_verification`) are in
  the database: whoever rewrites the chain can write fitting records. A
  `clean` verification run proves nothing about the past unless the digest it
  was checked against was kept outside; the `ledger_verify` function and its
  recorded runs are only as honest as the functions (see "The bodies of the
  functions"). The view `ledger_status` is not checked by `ledger_verify`
  (the hook rejects replacing or dropping it).
- **Reading**: the history table holds the old versions of rows, protected by
  its privileges only: the extension owner (a superuser) and the owner of the
  ledger table (and the roles that inherit from it) can read it, nobody else; row
  security and column privileges of the table do not apply to it, so those roles
  read every column of every old version. A superuser, or a role that is made a
  member of the owner, reads it too.
- **Dump and restore** (`pg_dump`, then a restore into a new database): the dump
  contains the tables, the history tables with their rows and the triggers, and
  the restore works without error, because the data is loaded before the
  triggers are created. The restored database has the rows but an **empty ledger**
  (the `kingres.ledger_*` tables are not dumped): `ledger_verify` reports the
  capture trigger of every table that is not registered, and an insert into a
  restored ledger table fails on the entry's foreign key. The restored copy is
  not a ledger. Use physical backups and standbys (tested in `010_review2`).
- **Reviewer-visible limit of the history privileges**: they follow the owner of the
  ledger table at the end of every DDL command and after `ALTER TABLE ... OWNER TO`
  and `REASSIGN OWNED`; with event triggers off and an ownership change that is
  neither of those (a catalog edit), the privileges are wrong until the next DDL
  command, and `ledger_verify` reports it as `history_privileges`.
- Anything that reads: the ledger protects the integrity of the data, not its
  confidentiality, and the row hashes of an entry reveal a row with little
  entropy to somebody who may read them (hence the privileges).

## Where a reviewer should attack first

Written from what is known of this code after two reviews; ordered by how little
it has been looked at, not by how likely it is to be wrong.

1. **The hook's list of statements** (`kl_check_statement` in `src/protect.c`).
   It inspects the statement types it lists and lets every other type through. A
   utility statement of PostgreSQL that is not listed and that changes a ledger
   table or the ledger catalog is not stopped by the hook (the event triggers are
   the second line, and only for what they look at: drops, rewrites and the state
   checked by `ledger_all_defects`). Read the list against `src/include/nodes/parsenodes.h`
   and against what the event trigger can see afterwards.
2. **The identity of the extension's own script** (`kl_note_script`,
   `kl_is_own_script`). It trusts the statements whose query string is the text of
   the script that `execute_sql_string` passes. That is a property of the server
   code that is not documented as an interface; check it against
   `src/backend/commands/extension.c`, and check that nothing a script statement
   triggers can hand a statement the same string.
3. **The history table's privileges** (`src/history.c`). They are changed through
   `ExecuteGrantStmt` with a statement built in C, run as the extension owner, from
   the event trigger (every DDL command, every updatable table) and from the hook
   after `ALTER TABLE ... OWNER TO` and `REASSIGN OWNED`. Look at roles that hold
   the privilege with grant option from somebody else, at column privileges, at a
   change of owner that neither the hook nor the event trigger sees (`event_triggers
   = off` and a catalog edit), and at the cost of the check on a database with many
   updatable tables.
4. **The history write path and `ADD COLUMN`** (`kl_insert_history`, `ledger_history_sync`).
   The history row is written through the table access method; the history table is
   changed through `AlterTableInternal` with the collection of commands for event
   triggers switched off. A race between a concurrent `UPDATE` and an `ADD COLUMN`
   is excluded by the lock `ALTER TABLE` takes on the table, not by a test of its
   own.
5. **The worker's settings** (`kl_harden_settings`, `src/worker.c`). The list was
   made by reading `guc_tables.c`; a setting that is missing from it can still stop
   or subvert a worker. `synchronous_commit` and `work_mem` are deliberately left
   alone. The failure state is in shared memory with a spinlock and is written from
   an exit callback and from an `emit_log_hook` that is installed in the worker
   process only.
6. **Cancellation is swallowed on purpose** in `ledger_verify_recorded` (it records
   a cancelled run and returns normally). A caller that sends several statements
   in one request and cancels the first will see the others run.
7. **Not run at all**: `EXEC_BACKEND` (Windows) builds, `pg_upgrade`, a standby that
   is promoted while a worker is running a failure delay, and concurrent `ALTER
   TABLE ... OWNER TO` of the same table by two sessions.

## Limitations of this version

- No client or server signatures, no external anchoring, no per-shard
  aggregate digest (phase 7). No conversion between the modes of a table, no
  partitioned or inheriting tables, `TRUNCATE` is rejected for both modes.
- A history table's name is `<table>_ledger_history` and must fit in 63 bytes.
  An `UPDATE` that changes nothing writes two entries and a history row.
  The history table has no indexes (the capture path writes it directly, and
  the protection rejects an index on it), so a lookup by key scans it; copy
  the rows somewhere else if you need to query the history often.
- The capture code writes the history row through the table access method,
  not through the executor: no trigger, rule, constraint or index of the
  history table is involved (there must be none), and a value stored out of
  line in the table is stored in the TOAST table of the history table.
- `ledger_verify_recorded` cannot write a `running` record (see above), and the
  `rows_checked` of a run is the number of entries of the blocks in its range.
- `user_name` is covered by the hashes only once the transaction is in a block.
  In `ledger_pending`, before `ledger_build_block` has run, it can be changed
  without detection, like everything else in the window before a block and its
  digest exist (a test shows this: the changed name is then hashed into the
  block). After that, changing `ledger_tx.user_name` is reported by
  `ledger_verify`. The name says which role ran the statement; it is not a
  signature.
- Tables must be ordinary permanent tables in a user schema without generated
  columns; partitioned tables, tables that take part in inheritance, tables with
  rules, temporary and unlogged tables are rejected.
- The extension owner must be a superuser, and `ledger_enable` needs READ
  COMMITTED and a server that is not in recovery.
- The row hash depends on the database encoding only through the UTF-8
  conversion of text values; it assumes that conversion is lossless.
- `ledger_verify` reads every row of every ledger table and every entry; it
  is meant for scheduled use, not for every transaction. Each of its checks is
  one statement (one snapshot); run it in a REPEATABLE READ transaction for a
  single snapshot of the whole verification.
- `ledger_build_block` and `ledger_enable` need the READ COMMITTED isolation
  level.
- Every DDL command runs the checks of the event trigger over all ledger tables.
- Proof size grows with the number of blocks between the proof's block and the
  digest (the headers are listed one by one).
- EXEC_BACKEND builds (Windows) were not tested: the library is detected as
  preloaded only in the postmaster.
