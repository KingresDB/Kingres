<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Portions Copyright (c) 2026, RooyeKhat Media Co -->
# kingres_ledger

Tamper-evident ledger tables for KingresDB (a thin fork of PostgreSQL 18.7),
version 0.3. Version 0.1 (phase 6, first increment) has append-only tables;
version 0.2 adds updatable ledger tables with a history table, background
workers that build the blocks, and the records an operator needs (recorded
digests and verifications and a status view); version 0.3 (part A of the
extension work of phase 7) adds three read-only functions for tile
publication (see "Reading the chain for publication"). The designs are
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

The hash format is **version 1, unchanged by 0.2 and 0.3**: digests, proofs and block
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

Tests: seventeen pg_regress tests and seventeen TAP tests under `t/`.

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
| `ledger_preimage` | `ledger_block_preimage`: an independent layout of the 87 bytes, a pinned value, every stored block, refused arguments, privileges |
| `ledger_head` | `ledger_head` and `ledger_blocks_entries` in a database of their own: an empty ledger, one and five blocks, every bound of a page, malformed stored blocks (`XX001`), the removal of the end of the chain (with and without the rows that refer to it), recorded digests, the head's own hash and link, the chain identity on every call, a repeated height, a valid forged successor, privileges, `REVOKE ... FROM PUBLIC` on the catalog tables, nothing written |
| `ledger_stray` | a row above the head (L8-08) and a digest with another hash (L9-02): who can write one (nobody but a superuser who asks), the error of `ledger_head` that names the row and the table, `ledger_verify` naming the same row, that no SQL removes it, `ledger_remove_stray` step by step and the record it keeps, an empty chain with a stray digest |
| `ledger_manual` | what a superuser's hand leaves (L9-01 second half, L9-14): a valid block written by hand with a time in 2100 and no transaction, `ledger_head` that accepts it, `ledger_verify` that reports it (`block_time`, `block_empty`), the builder that refuses to build on it (`KL004`) with the transactions left pending, the limit that only a superuser lifts, the block that then carries the time, a transaction with no entry (`tx_empty`), a pending row deleted by hand (`entry_orphan`) and a member of `pg_write_all_data` that cannot |
| `ledger_guard` | the write guard (L9-01): the nine tables against `INSERT`, `ON CONFLICT`, `MERGE`, a view and a `SECURITY DEFINER` function of a member of `pg_write_all_data`, `DELETE`, `UPDATE` and `SELECT ... FOR UPDATE`, as that member, as a role with nothing, as a superuser and as a superuser with `allow_manual_write` on; every statement that is allowed is rolled back and `ledger_verify` finds nothing; where the setting can and cannot be set; that no function takes the content of a write; the honest flows with the setting off |
| `ledger_paths` | the invariant and the paths to it (L10-01 to L10-03, L10-13): a member of the owner role with `INHERIT` and no `SET` on every table and every internal function, a hand-made capture trigger and a hand-made registration, the superuser with no setting, the setting in the hands of a role that was given `SET ON PARAMETER` (also through a `SECURITY DEFINER` function of a superuser and after `SET ROLE`), every lock mode and every row lock on every table as `pg_maintain` and as a member of `pg_write_all_data`, `REINDEX` and `CLUSTER` in a transaction block, the marker lowered after the capture trigger, the builder, the digest, the recorded verification, `ledger_enable`, and after a failed statement in a subtransaction, in an exception block and in the capture trigger, the record that a write by hand leaves, the clauses of the digest check and the units of the clock limit |
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
| `011_upgrade_0_3` | `ALTER EXTENSION UPDATE` from 0.2 and from 0.1 to 0.3 with append-only and updatable tables, blocks, digests, proofs of `I` and `D` and a recorded verification: all still verify, the upgrade writes nothing, the result equals a direct installation of 0.3 (catalog fingerprint with the full definition of every function), `pg_dump` and restore |
| `012_publish_read` | the reads for publication: an independent recomputation with `Digest::SHA` (hashes, fields, links, the RFC 6962 root), tiles and a signed checkpoint checked by the Go verifier when `KINGRES_VERIFY` names it, concurrent calls under load, a standby, the bound of 10000 entries |
| `013_head_inflight` | `ledger_head` under load: writers, block builders and digest generators on a primary, readers on it and on a hot standby that replays (single statements, a loop in a `DO` block, `REPEATABLE READ`): no call may fail; then a planted row is refused on both, with the row named |
| `014_guard` | a member of `pg_write_all_data` attacks every table with 100 statements per table from its own sessions while writers, builders and digest generators run (not one statement succeeds, the ledger verifies, no block time of the attacker); `COPY FROM` into the nine tables as the member, as a superuser and with the setting on; a hot standby replays and verifies; a subscription that publishes a ledger table of another server is refused by the guard trigger, in the synchronisation of the table and in the apply worker |
| `015_recipe` | the statements of the README recipe, read out of the README and run literally for four kinds of damage (a transaction row above the head, a digest above the head, a digest with another hash, the head block removed): `ledger_head` answers, `ledger_verify` reports exactly the `operator_removal` findings, the rows are in `ledger_removed` and in the file, no trigger is switched off, a member still cannot write, the chain goes on |
| `017_guard_paths` | an owner of the extension that lost `SUPERUSER`; every way a setting can arrive (`PGOPTIONS`, `postgresql.conf`, `ALTER ROLE`, `ALTER DATABASE`, `ALTER SYSTEM`, a `SET` clause, `SET LOCAL`, `set_config`); a real subscription whose table synchronisation and apply workers write (inserts and deletes) the ledger tables of the subscriber, which only the guard trigger sees; the block builder against locks and advisory locks that other sessions hold; the honest path with the guard closed |
| `016_dump_upgrade` | `pg_dump` and a restore with the guard (a new empty ledger, nothing written by hand), and `pg_upgrade` of a cluster with a ledger (binary upgrade mode): the chain, the blocks and a digest of before verify, the builder and the capture work, the guard is on |

The regress tests need `shared_preload_libraries = 'kingres_ledger'` (the
in-tree `check` target and `kingres/dev/ext.sh` set it).

The optional verifier check of `012_publish_read` needs a built
`kingres-verify` (see `kingres/verifier`) and the `openssl` command; run it
with `KINGRES_VERIFY=/path/to/kingres-verify`. Without the variable those
subtests are skipped.

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

**Upgrade from 0.2.** `ALTER EXTENSION kingres_ledger UPDATE TO '0.3'` (the
script is `kingres_ledger--0.2--0.3.sql`; load the new library first: restart
the server). It creates three functions, adds the check `digest_block` to
`ledger_verify` (a recorded digest that names a block that does not exist),
and changes nothing else: no table, block, digest, proof or ledger entry is
touched or written, and every verification that passed before still passes.
A 0.1 database reaches 0.3 with one `ALTER EXTENSION kingres_ledger UPDATE`
through `kingres_ledger--0.1--0.3.sql`, which is the two upgrade scripts in one:
the protection hook trusts only the first script of a command, and the step to
0.3 replaces `ledger_verify`. The result is the
same set of objects, privileges and protection as `CREATE EXTENSION
kingres_ledger VERSION '0.3'` (`t/011_upgrade_0_3.pl` compares them,
including the definition of every function). The 0.3 SQL needs the 0.3 library,
and the 0.3 library needs the 0.3 SQL to write the ledger: with the SQL of 0.1
or 0.2 the library refuses what the old functions write and says to run `ALTER
EXTENSION kingres_ledger UPDATE` (see "Who can write the ledger tables"); the
capture trigger keeps working, the block builder does not until the update.

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
| `kingres.ledger_block_preimage(hash_version smallint, height bigint, prev_hash bytea, tx_merkle_root bytea, tx_count int, closed_at timestamptz) returns bytea` | **0.3.** The 87 bytes that `ledger_block_hash` hashes (the same code lays them out), with the same argument checks. `IMMUTABLE`, `STRICT`. | PUBLIC |
| `kingres.ledger_head() returns table (chain_id bytea, height bigint, block_hash bytea, closed_at_us bigint)` | **0.3.** The latest block without building one and without recording a digest; no row for an empty ledger. The head is checked before it is returned (its hash recomputed, its link to the block before, the chain identity against block 0) and so is the end of the chain (no transaction row and no recorded digest names a block above it); an error `XX001` otherwise, also when the chain identity is missing. See "Reading the chain for publication" for what that does and does not catch. `STABLE`, `SECURITY DEFINER`. | granted roles (the service role of `kingresd`) |
| `kingres.ledger_blocks_entries(from_height bigint, max_count int) returns table (height bigint, entry bytea)` | **0.3.** The block preimages from `from_height`, in height order, at most `min(max_count, 10000)` of them. Every block returned is checked first: hash version 1, lengths, no gap and no repeat, the link to the block before (the genesis hash for block 0; for a range that starts later, the block before it is read and its hash recomputed), the chain identity against block 0, and `SHA-256(entry) = block_hash`; the first failure is an error `XX001` and no row is returned. It does not see blocks removed from the end: use `ledger_head`. `STABLE`, `SECURITY DEFINER` (it reads `ledger_blocks` and `ledger_chain` only). | granted roles |
| `kingres.ledger_hook_active()` | True when the library was loaded through `shared_preload_libraries`. | PUBLIC |

Internal functions (`ledger_capture`, `ledger_register_table`, `ledger_lock_table`,
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
| `kingres_ledger.max_clock_skew` | 1h | How far the previous block may be ahead of the clock of the server before `ledger_build_block` refuses to build (error `KL004`); `0` means no limit. Superuser setting (see "Who can write the ledger tables", "The time of a block") |
| `kingres_ledger.allow_manual_write` | off | A superuser may write the ledger tables by hand in this session. Superuser setting that can be turned on only with `SET` in a session (see "Who can write the ledger tables") |

Errors raised by the extension: SQLSTATE `KL001` (modification of an
append-only table), `KL002` (statement rejected by the protection hook or by
the event trigger), `KL003` (library not preloaded), `KL004` (the previous block
is closed ahead of the clock), `KL005` (the SQL of the extension is older than the
library: `cannot write to table "...": the SQL of extension "kingres_ledger" is
version X, and this library writes the ledger only through the functions of
version Y`, with the hint `Run ALTER EXTENSION kingres_ledger UPDATE.`). Any other
write that the guard refuses is `42501`.

## Privileges

The functions of the extension run as the role that created it (a superuser)
where they have to: this is the list, and nothing else uses `SECURITY
DEFINER`:

| Function | Why it runs as the owner |
|---|---|
| `ledger_capture` | writes the entry of every inserted row; ordinary roles have no privilege on the ledger tables |
| `ledger_register_table` | registers a table that `ledger_enable` set up and writes the entries of the rows it already holds; computes both from the catalogs and the table (see "Who can write the ledger tables") |
| `ledger_enable` | creates triggers on a table that the owner does not own, and writes `ledger_table` (it checks that the calling role owns the table) |
| `ledger_build_block` | writes blocks and transactions; a C function that computes everything it writes |
| `ledger_generate_digest` | calls the block builder and writes `ledger_digest` |
| `ledger_verify` | reads every ledger table, every history table and every table under ledger control |
| `ledger_remove_stray` | removes rows that stop `ledger_head` and keeps a copy in `ledger_removed`; a superuser, with `allow_manual_write` on, and no other role (see "A row above the head") |
| `ledger_verify_recorded` | writes `ledger_verification` |
| `ledger_inclusion_proof` (both forms) | reads the entries and transactions, which the caller may not read; the two-argument form is a wrapper that exists so that grants made on it under 0.1 keep working |
| `ledger_head` | one statement snapshot of the chain identity, the latest block, and the newest transaction row and recorded digest it is compared with; defined as the owner so that the answer does not depend on what the caller may read of the ledger tables (it returns a hash and a count, no row data) |
| `ledger_blocks_entries` | reads `ledger_blocks` and `ledger_chain` only, as the owner, so that `EXECUTE` is all a service role needs and an operator may withdraw PUBLIC's `SELECT` on those tables (see "Reading the chain for publication") |
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
| `kingres.ledger_table`, `kingres.ledger_chain`, `kingres.ledger_blocks` | `SELECT` (an operator may revoke it from PUBLIC: see "Reading the chain for publication") |
| `kingres.ledger_digest` | `SELECT` on every column except `generated_by` (block-level data only) |
| `kingres.ledger_entry`, `kingres.ledger_pending`, `kingres.ledger_tx`, `kingres.ledger_verification` | nothing |
| `kingres.ledger_status` (view) | nothing (it shows how many transactions wait, and the state of the worker) |
| the history table of an updatable table | nothing; `SELECT` for the owner of the ledger table only, kept by the extension (see "Updatable tables"); no grant is possible |
| `ledger_history_acl_sync`, `ledger_history_sync`, `ledger_worker_state`, `ledger_lock_table`, `ledger_register_table`, `ledger_capture` | nothing |
| the identity sequences of `ledger_digest` and `ledger_verification` | nothing |
| `ledger_enable`, `ledger_build_block`, `ledger_generate_digest`, `ledger_verify`, `ledger_verify_recorded`, `ledger_verify_digest`, `ledger_inclusion_proof` | nothing |
| `ledger_row_hash`, `ledger_history_row_hash`, `ledger_verify_proof`, the hash primitives, `ledger_hook_active` | `EXECUTE` |

The entries, the work queue and the transaction table are not readable by
PUBLIC because the row hashes in them let a reader confirm a guess about the
content of a row with little entropy, and because they say which role wrote
when. The block headers, the chain identity and the list of ledger tables only
describe the chain and are public (they are in every digest and proof anyway).
No role can change or remove a row of a ledger table through SQL, and no role
is granted a privilege on one: the protection hook rejects any `GRANT` on them
other than `SELECT`, and any `GRANT` on the internal functions. A new row can
still be inserted by a superuser, by the owner, and by every member of
`pg_write_all_data`, which bypasses table privileges (see "What the
protections cannot stop"); do not give that role to anyone you would not trust
with the ledger.

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

## Reading the chain for publication

Version 0.3 (part A) adds what the control plane needs to publish the blocks
as a tile log (design `kingres/docs/design/ledger-phase7.md`, sections 4 and
8.1): the leaf of the log is the block hash preimage, and the log is built
from pages of preimages. The three functions read; none builds a block,
records a digest or writes any table, and the hash format is unchanged.

- **Snapshot.** The functions are `STABLE`: they read the snapshot of the
  statement that calls them, so `ledger_head` and `ledger_blocks_entries` in
  one statement (or one `REPEATABLE READ` transaction) see the same blocks.
  Separate statements in `READ COMMITTED` see the head they find then; the
  head only grows. They take no lock that blocks writers or the block
  builder, and a block becomes visible whole and in height order (the
  builder holds an exclusive lock on `ledger_chain` until it commits), so a
  page never has a gap that is not damage.
- **Standby.** They work on a hot standby and read what has been replayed;
  the answer is the same as the primary's at that replay point. They run in
  a read-only transaction.
- **Privileges.** `ledger_block_preimage` is a pure computation and is
  executable by PUBLIC, like `ledger_block_hash`. `ledger_head` and
  `ledger_blocks_entries` are not executable by PUBLIC: they expose hashes,
  counts and close times of blocks, no row data, but the publication is an
  operator's choice. `GRANT EXECUTE` to the service role of `kingresd`;
  nothing is granted at installation. Both functions run with the rights of
  the extension owner and read only `ledger_blocks`, `ledger_chain` and, for
  `ledger_head`, the newest rows of `ledger_tx` and `ledger_digest`, so a role
  with only that grant can call them and cannot read `ledger_entry`,
  `ledger_tx` or `ledger_pending`.
- **Withdrawing PUBLIC's read access.** `ledger_table`, `ledger_chain`,
  `ledger_blocks` (and the columns of `ledger_digest`) are readable by
  PUBLIC after installation because they only describe the chain. An
  operator who does not want that can `REVOKE SELECT ON ... FROM PUBLIC`
  (also `REVOKE ALL ON ALL TABLES IN SCHEMA kingres FROM PUBLIC`); it works,
  and the roles that were granted `EXECUTE` on the two functions keep
  working, because the functions read as the owner. (The statement used to fail
  with `role "public" does not exist`: the protection hook asked for the OID
  of every grantee to compare it with the owner, and PUBLIC has none. Revoking
  the privileges of the owner is still refused.) After the
  revoke, `SELECT` on the tables, and with it the export of the headers for
  `kingres-verify blocks`, needs a grant to the role that exports. The
  function `ledger_block_preimage` stays executable by PUBLIC: it computes on
  its arguments and reads nothing.
- **Paging.** `ledger_blocks_entries(from_height, max_count)` returns the
  blocks with `height >= from_height` in height order, at most
  `min(max_count, 10000)`. An empty chain, a `from_height` past the head and
  `max_count = 0` give no rows; a negative argument is an error (`22023`);
  a NULL argument gives no rows. To read on, ask from the last height plus
  one. `closed_at_us` and the last eight bytes of an entry are microseconds
  since 2000-01-01 00:00:00 UTC, as in the digests.
- **What is detected, and what is not.** The two functions check the chain
  they read. They do not make a database that can be written to trustworthy
  on their own: a signature, a record kept elsewhere, or the next call of
  `ledger_verify` is what does.

  Detected (error `XX001`, never a silently shortened or altered answer
  within what is read):

  | Damage | Where it is found |
  |---|---|
  | a changed field (`hash_version`, `prev_hash`, `tx_merkle_root`, `tx_count`, `closed_at`, `block_hash`) of a block that is read | `ledger_blocks_entries`; `ledger_head` for the head |
  | a block with a wrong link or a wrong hash | both, within what is read |
  | a gap, a repeated height (the message names the repeated block), a missing block 0, a missing chain identity | `ledger_blocks_entries`; `ledger_head` for block 0, the head and the block before it |
  | a block before the range that was changed with its stored hash left alone | `ledger_blocks_entries` from `from_height > 0`: that block is read and its hash recomputed, not taken as stored |
  | a changed chain identity | both, on every call that returns a row (block 0 must link to the genesis hash of the identity) |
  | the end of the chain removed from `ledger_blocks` and from nothing else | `ledger_head`: a transaction row (`ledger_tx.block_height`) or the newest recorded digest (`ledger_digest`) names a block above the head, and the newest digest must also name the block that is there; the error names the row and the table, and `ledger_verify` reports every such row (`tx_block`, `digest_block`); see "A row above the head" below |

  Not detected:

  - Blocks removed from the end together with the rows that refer to them.
    The transaction rows and the digest rows are in the same database and
    can be removed by whoever can remove blocks: a shorter chain that is
    valid is answered as the chain. `ledger_head` only catches a removal
    that left those records behind (an accident or a careless removal; no
    honest write leaves such a row, see "A row above the head"). `ledger_blocks_entries` reads `ledger_blocks`
    alone and never sees the end of the chain: a range past the head is
    "nothing new", which is also what a truncated chain looks like.
  - A fully valid successor written by someone who can write the table: a
    block with the right link and the right hash is a block. Nothing in the
    database can tell it from one the builder wrote; `ledger_verify` reports
    it (it checks the blocks against the transactions), a digest kept
    outside the database shows it.
  - Anything in blocks that are not read: a call reads its range, the block
    before it, block 0 and, for the head, the newest records. The other
    blocks are not looked at.

  What a publisher must therefore do: keep its own record of what it
  published (the size and root of the last checkpoint, the head it saw); at
  every round call `ledger_head` first and compare it with that record (the
  head never goes back, and for an equal height the hash is the same); read
  again the last published block (`from_height = published_size - 1`) and
  check that its entry is the one published, then read on; and never sign
  more than it has read. A head that went back, or a published block that
  changed, is an alarm, not a retry.

### A row above the head

`ledger_head` refuses (error `XX001`) when a row of `ledger_tx`, or the newest
row of `ledger_digest`, names a block above the head, or when the chain has no
block and such a row exists. The message says what was found and where:

```
the head is block 2, but the row of transaction 900000001 in kingres.ledger_tx names block 77: the end of the chain was removed, or the row was written by hand
the head is block 2, but digest 2 in kingres.ledger_digest names block 88: the end of the chain was removed, or the row was written by hand
the chain has no block, but the row of transaction 900000001 in kingres.ledger_tx names block 77
```

`ledger_verify` reports the same row, for every such row and not only the
newest: `tx_block` (`tx 900000001`, "transaction refers to block 77, which
does not exist") and `digest_block` (`digest 2`, "digest refers to block 88,
which does not exist"). `ledger_blocks_entries` does not read these rows and
keeps answering; a publisher that calls `ledger_head` first stops.

*What it means.* A block row and the rows that refer to it are written by one
transaction (the block builder, `ledger_generate_digest`), and a statement sees
them all or none of them, on the primary and on a standby that replays the
commit. No honest write leaves a row above the head, and none was seen under
load (`t/013_head_inflight.pl`: writers, builders and digest generators on a
primary, readers on it and on a hot standby that was replaying, in single
statements, in a loop inside a `DO` block and in `REPEATABLE READ`; no call
failed). So the row is either what is left of a block that was removed from
`ledger_blocks`, or it was written by hand.

*Who can write one.* A superuser who asks for it, and nobody else. Since the
write guard (see "Who can write the ledger tables"), a row of a ledger table
exists only because the code of the extension wrote it, or because a superuser
turned `kingres_ledger.allow_manual_write` on in a session and wrote it by hand;
a member of `pg_write_all_data`, which bypasses table privileges, is refused
like every other role (`cannot write to table "ledger_tx": only the code of the
ledger writes its tables`). That keeps the recovery below an act of a superuser
and not of whoever planted the row. A row that was planted before the guard
existed, or by a superuser, or a restore that wrote rows by hand, is exactly as
dangerous as it was: the guard cannot see the past.

Other things that stop `ledger_head` for good, with the same way out:

- **A recorded digest with another hash than its block** (L9-02): `ledger_head`
  compares the newest digest row with the block it names and refuses when they
  differ (`block 0 is not the block that digest 3 in kingres.ledger_digest
  names`). `ledger_verify` now checks every recorded digest, whatever the range:
  `digest_block` (the block does not exist), `digest_hash` (the block has
  another hash) and `digest_json` (the digest document does not say what the
  row says), each with `digest <id>` as the object. A rewritten chain makes
  every earlier recorded digest a `digest_hash` finding (an attacker has to
  remove them too, which needs the same setting).
- **A removed head block**: the block row alone was removed from
  `ledger_blocks`, which leaves its transaction rows (`ledger_tx`) and the digest
  of it behind, both above the head.

*What to check first, before anything is removed.*

1. `SELECT * FROM kingres.ledger_verify();` and read every finding, not only
   this one: a gap, a bad link or a bad hash next to the stray row means that
   the chain itself was changed.
2. Compare block B that the message names with what was published or kept
   outside the database: the size of the last checkpoint of the publication, a
   digest you stored elsewhere. If B is not above all of them, the block
   existed and was published or digested, and it was removed. That is damage,
   not a stray row: do not remove the row yet. Restore the database from a
   backup or a point in time that has block B (the blocks come back with their
   rows), or accept the divergence in the publication (see below).
3. Find out who wrote the row: the server log (`log_statement`, an audit
   extension) and the members of `pg_write_all_data` (`SELECT roleid::regrole,
   member::regrole FROM pg_auth_members WHERE roleid = 'pg_write_all_data'::regrole`).

*How to remove it.* A superuser, in one session, with the setting on. The
function `kingres.ledger_remove_stray(reason)` does the work: it takes the
builder lock, copies every transaction row above the head and every recorded
digest that names a missing block or another hash into `kingres.ledger_removed`
(as they were, with who, when and why), queues the transactions of the removed
rows again when they have entries (so that no entry is left without a block and
the next block is built from them), and deletes the rows. No trigger is
switched off and no other session is affected: the setting is the only thing
that opens the tables, and only for the session that turned it on. The
statements below write the rows to a file first, because the copy in the
database is part of the database and an auditor must hold one that is not.

<!-- recipe: remove-stray -->
```sql
\pset tuples_only on
\pset format unaligned
\o stray-rows.json
SELECT jsonb_pretty(jsonb_build_object(
    'transactions', (SELECT coalesce(jsonb_agg(to_jsonb(t)), '[]')
                       FROM kingres.ledger_tx t
                      WHERE t.block_height > coalesce((SELECT max(b.height) FROM kingres.ledger_blocks b), -1)),
    'digests', (SELECT coalesce(jsonb_agg(to_jsonb(d)), '[]')
                  FROM kingres.ledger_digest d
                 WHERE NOT EXISTS (SELECT 1 FROM kingres.ledger_blocks b
                                    WHERE b.height = d.height AND b.block_hash = d.block_hash))));
\o
SET kingres_ledger.allow_manual_write = on;
SELECT kingres.ledger_remove_stray('rows above the head found by ledger_head');
RESET kingres_ledger.allow_manual_write;
SELECT * FROM kingres.ledger_head();
SELECT check_name, object FROM kingres.ledger_verify();
```

What `ledger_verify` then reports is exactly this and nothing else: one
`operator_removal` finding for every row that was removed (`object` is
`removal <id>`, the detail says which table, by whom, when and why), for as long
as the record exists. There is no `entry_orphan` and no `tx_block`: the entries of
a removed transaction are queued again and built into the next block (a row that
was written by hand has no entries and is not queued). The removed rows can
be read with `SELECT * FROM kingres.ledger_removed` (only the extension owner,
a superuser, and roles that were granted `SELECT` on it); `row_data` is the row
as `to_jsonb` shows it. **Tell the auditor** what was removed: the file
`stray-rows.json` and the number of the removal records. The record is covered
by no hash and by no digest (a block does not contain it; adding it to a block
would change the hash format), so a digest taken later does not prove that it was
not edited; what proves a removal is the file and the report kept outside the
database.

*A pending row that was deleted by hand* (a superuser with the setting on; a
member of `pg_write_all_data` cannot) means that the entries of that transaction
belong to no block and to no pending transaction: the rows exist in the user table
and are no longer covered by any block. `ledger_verify` reports them as
`entry_orphan`, for good (tested in `ledger_manual`); the role name of the
transaction is not recoverable, so the extension offers no way to queue it
again. Treat it like a removed block: say so to the auditor.

*What it means for the publication.* Removing rows above the head changes no
block. If the rows were a stray row or a stray digest, nothing that was
published is touched, and the publisher goes on from the head it had. If the
rows are what a **removed block** left behind, the block itself is gone and the
recipe does not bring it back: the next block that is built is another block
with the same transactions, with another time and another hash, at the same
height. A publisher or an auditor that holds a checkpoint beyond the removed
block (a tile log that was signed at a larger size, a digest of that block) will
see **a fork**: the head went back, and the block at that height is not the
published one. That is correct, and it must stay visible: it is what happened.
Do not sign over it; start a new publication state, say what was removed and
why, and keep the old state file. Deleting the rows without the record would
only have removed the evidence, which is why step 2 comes first and the record
is kept.



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
## Who can write the ledger tables

**The rule, in one sentence.** *No statement that a role can issue writes a ledger
table, whatever the role inherits or owns, except (a) through the public functions
of the extension doing what they are documented to do, and (b) a superuser acting
as itself with `kingres_ledger.allow_manual_write` set in the session.*

A row of a table of the ledger (the nine `kingres.ledger_*` tables) exists only
because the code of the extension wrote it, or because a superuser asked to write
it by hand, as an explicit act (which leaves a record, see below). Nobody who is not a
superuser changes, removes, adds to or forges history without `ledger_verify`
reporting it, and no honest sequence brings the ledger into a state that it
refuses. A member of `pg_write_all_data` bypasses table privileges, so before
the guard it could insert a block, a transaction row, a digest or a pending
row; with a block whose time it chose it made every later honest block carry
that time (finding L9-01). It cannot any more. A member of `pg_maintain`, a
member of `pg_read_all_data`, the owner of an application table and a role with
`CREATE` on a schema are not superusers and cannot either.

**The guard has two parts, in `src/guard.c`.**

1. `ExecutorCheckPerms_hook` refuses every statement that inserts, updates or
   deletes a row of those tables, before any row is built, with `cannot write
   to table "<name>": only the code of the ledger writes its tables` (SQLSTATE
   `42501`; an `UPDATE` or `DELETE` of the append-only tables says `table "<name>"
   is an append-only ledger table`, `KL001`, as it always did). The hook has the
   same signature on PostgreSQL 18 and 19. It is called from
   `ExecCheckPermissions` (`src/backend/executor/execMain.c`) for every plan
   that `ExecutorStart` starts, for `COPY FROM` (`DoCopy` in `copy.c`), for the
   plans that SPI runs, and, on 19, for the foreign key check that does not go
   through the executor (`ri_CheckPermissions` in `ri_triggers.c`); the call
   comes after the privilege check and does not depend on its result, so it
   happens for a superuser too. That covers `INSERT`, `INSERT ... ON CONFLICT`,
   `MERGE`, writes through an updatable view, through a rule, from a trigger,
   from a `SECURITY DEFINER` function, from a foreign key action, and `COPY`.
   `SELECT ... FOR UPDATE` (and the foreign key checks) set the same bit as an
   `UPDATE` privilege but update no column: it changes no row, but a row lock
   that a role holds until its transaction ends makes the builder (its `DELETE` from
   the queue) and the capture triggers (the foreign key checks) wait, so on the
   nine tables it is refused like a write (`cannot lock rows of table`); the foreign
   key checks of the extension's own writes run with the marker raised. Partitions and inheritance children cannot be
   ledger tables (the protection refuses them).
2. A `BEFORE INSERT` row trigger on every table (`kingres_ledger_guard_write`;
   on `ledger_pending` also `BEFORE DELETE`), `ENABLE ALWAYS`, for the writers
   that never call `ExecCheckPermissions`: the logical replication apply worker
   and the table synchronisation worker check privileges themselves
   (`TargetPrivilegesCheck` in `worker.c`) and write through
   `ExecSimpleRelationInsert`, which fires triggers. (The review proposed the
   hook alone and said that it sees the apply worker; on both majors it does not.)

**What is not seen.** `TRUNCATE` and `ALTER TABLE` are utility statements
(`TRUNCATE` is stopped by its trigger, `ALTER TABLE` by the `ProcessUtility`
hook); large objects are not rows of these tables; C code of another extension
that writes the heap directly is not an executor statement and is out of scope;
file-level and catalog-level changes are in "What the protections cannot stop".
**What stops the block builder or the capture triggers.** The capture triggers take
`ROW EXCLUSIVE` on the queue, the entries and the registry, and the builder takes
`ROW EXCLUSIVE` on the transaction rows, the blocks and the queue and reads the
others; since L10-03 it takes no lock of any table: builders and
`ledger_remove_stray` take turns on a lock on the extension's own object with a
sub-object number that no SQL statement uses (not a table lock, which
`pg_maintain` and the members of `pg_write_all_data` can request, and not an
advisory lock, which every role can). Whatever conflicts with the writes is
refused to every role that is not a superuser: `LOCK TABLE` of a ledger catalog
table in any mode above `ACCESS SHARE` (finding L9-14 and L10-03), a row lock
(`SELECT ... FOR UPDATE`, `NO KEY UPDATE`, `SHARE`, `KEY SHARE`) on any of the
nine tables, even for a superuser unless it turned the setting on (the row lock
of a `FOR UPDATE` on the queue would make the builder wait for its `DELETE`), and
`CLUSTER` and `REINDEX` (not `CONCURRENTLY`) of a ledger catalog table in a
transaction block or in a function, where the lock would be held until the
transaction ends; as a statement of their own they hold it for the time the
statement takes. A single maintenance statement on a large table (`VACUUM FULL`,
`CLUSTER`, `REINDEX`) therefore still holds its lock for as long as it runs: that
is the price of granting `MAINTAIN`, not a way to keep it. `VACUUM`, `ANALYZE` and
`REINDEX CONCURRENTLY` conflict with no write. A superuser can stop the server
anyway.

**How the extension's own writes are told apart, and why no other path exists.** By
a marker that only the extension's C code raises, never SQL: a backend-local depth
counter, raised and lowered (in a `PG_FINALLY` block) around the statements that
the library itself runs, and zeroed at the end of every transaction. The capture
code (`capture.c`) raises it around its two `INSERT`s (`ledger_pending`,
`ledger_entry`), and `write.c` raises it around the fixed statements that
`ledger_build_block`, `ledger_generate_digest`, `ledger_verify_recorded` and
`ledger_register_table` run. The design rule since the fourth review (L10-01,
L10-02): **no SQL-callable function takes the content of a write as an argument.**
The earlier function `ledger_internal_write(op, VARIADIC args)` took a statement
number and the values, and its only protection was its ACL; an ACL is inherited
(a member of the owner role with `INHERIT` and no `SET`, an owner that was made a
non-superuser, any superuser) and `REVOKE` does not stop a superuser, so it was
a way to write a block of the caller's choice. It is gone, and so is
`ledger_record_row(table_id, row)` (kept only as a refusal for the SQL of 0.1 and
0.2, see "Upgrades"). What writes is a C entry point that reads the ledger and
computes every value:

- `ledger_build_block` reads the queue, the entries and the head, and computes the
  transaction roots, the time of the block (with the clock rule below), the hash
  and the end of the queue;
- `ledger_generate_digest` builds a block if one is pending and records the digest
  of the head in one statement;
- `ledger_verify_recorded` runs `ledger_verify` and records its counts and its
  first 100 problems (an error is caught in a subtransaction and recorded as
  `failed`);
- `ledger_register_table` (the last step of `ledger_enable`) takes the identity from
  the capture trigger of the table (a value that the registry's sequence issued),
  the mode from the events of that trigger, the history table from the catalogs,
  refuses a table that `ledger_table_defects` finds anything wrong with, and
  enters the rows of the table; it refuses a table that is already registered and
  a table that the caller does not own;
- the capture trigger enters the hash of the row that was inserted, under the
  identity that the registry has for that table, which it checks on insert as on
  update and delete (a role that may create the trigger on a table of its own
  cannot record entries under another table's identity).

A superuser can declare the C symbols as functions of its own, and finds that each
one computes what it writes. The statements are fixed text with numbered
parameters, run with a fixed `search_path`; no argument is text that is run.

*Every function that the install script creates.* Pure functions that read
nothing (`ledger_leaf_hash`, `ledger_node_hash`, `ledger_merkle_*`, `ledger_tx_leaf`,
`ledger_block_tx_leaf`, `ledger_block_hash`, `ledger_genesis_hash`,
`ledger_row_hash`, `ledger_history_row_hash`, `ledger_timestamptz_us`,
`ledger_verify_proof`, `ledger_block_preimage`) and read-only functions
(`ledger_hook_active`, `ledger_binary_upgrade`, `ledger_outer_user`,
`ledger_worker_state`, `ledger_is_member`, `ledger_type_problem`,
`ledger_trigger_ok`, `ledger_table_defects`, `ledger_history_defects`,
`ledger_catalog_defects`, `ledger_all_defects`, `ledger_verify`,
`ledger_verify_digest`, `ledger_inclusion_proof` (both), `ledger_head`,
`ledger_blocks_entries`) write nothing. `ledger_lock_table` locks a table that the
caller owns. `ledger_history_sync` and `ledger_history_acl_sync` change a table
through the catalog API with none of the checks of a statement and are refused
unless the current user is a superuser (a member of the owner role without
`SET` is not; it could have changed any table). `ledger_capture` and
`ledger_block_modify` are trigger functions and refuse any other call;
`ledger_guard` is an event trigger function. `ledger_build_block`,
`ledger_generate_digest`, `ledger_verify_recorded`, `ledger_enable` and
`ledger_register_table` are the writers above (`ledger_enable` checks by hand that
the caller owns the table); `ledger_remove_stray` is for a superuser with the
setting on and refuses everybody else. Every `SECURITY DEFINER` function has
`SET search_path = pg_catalog, pg_temp` (the test `ledger_privs` keeps that
list). The catalog is the same for a new installation and for both updates
(`t/011_upgrade_0_3.pl` compares them, now also column privileges, initial
privileges, default privileges, owners, `proconfig` and the enabled state of
the triggers).

*What a role that holds everything but the attribute can do* (tested in
`ledger_paths` and `017_guard_paths`): call the public functions, which do what
they are documented to do; nothing else. It cannot insert, it cannot register a
table it does not own or that was not set up, it cannot call the two internal
functions, and `SET kingres_ledger.allow_manual_write = on` gets it nothing,
because the hook asks that both the current user and the user outside any
`SECURITY DEFINER` function are superusers.

*Where user code could run while the marker is raised, and what stops it.* The
marker is raised around a single `SPI` statement and nothing else. The tables
that statement writes have no trigger but the blocking ones (they raise errors),
no policy, no rule and no default or `CHECK` expression other than constants,
`now()` and `clock_timestamp()`; their indexes have no expression; their column
types are built in; the foreign key checks are system triggers. The protection
hook refuses `CREATE TRIGGER`, `CREATE RULE`, `CREATE POLICY` and `CREATE INDEX`
on them, and the event triggers report a table that has any. The functions the
statements call are `pg_catalog`'s and the extension's, named with the schema,
under `search_path = pg_catalog, pg_temp`, so no operator or function that a role
created is found. Column defaults, `CHECK` constraints, index expressions, type
input functions and row-level security of **user** tables are not involved: the
user's row is read by the capture trigger before the marker is raised and is
hashed in C, and `ledger_register_table` reads the existing rows with a cursor and
hashes them in C. Statement-level event triggers fire on DDL, not on these
statements, and only a superuser can create one.

**A superuser who writes by hand.** A superuser's direct `INSERT` is refused
too, unless the superuser turned on `kingres_ledger.allow_manual_write` in the
session (`SET kingres_ledger.allow_manual_write = on`; `RESET` turns it off). It
is `PGC_SUSET`, off, and a check hook refuses `on` from any source but a `SET`
of a session: not in `postgresql.conf`, not with `ALTER SYSTEM`, `ALTER ROLE`,
`ALTER DATABASE`, not as a connection option or an environment variable. It
counts only when both the current user and the session user are superusers: a
`SET ROLE` to somebody else, a `SECURITY DEFINER` function of a role that is not
a superuser, or a role that was given the right to set the parameter with `GRANT
SET ON PARAMETER` get nothing from it. With it on, `INSERT` works on every ledger
table, and `DELETE` works on `ledger_tx`, `ledger_digest` and `ledger_pending`
(the rows that `ledger_remove_stray` takes away); `UPDATE` and `TRUNCATE` stay
refused by their triggers, as always. **A write by hand leaves a record.** The guard trigger of the seven tables that
hold the chain (not the queue and not the record itself) writes, in the same
transaction as the insert, a row in `kingres.ledger_removed` with the action
`inserted`: who, when, the table and the row as JSON. `ledger_verify` reports one
finding `manual_write` for every table that has such a record, with the number of
rows, for as long as the record exists, and the record is rolled back with the
write. It is not part of any hash, so hash format 1 is unchanged. It is not a
proof: a superuser can still switch triggers off through the catalogs, and
deletes by hand are recorded only when `ledger_remove_stray` makes them.
Writing by hand is how a ledger is
damaged on purpose; `ledger_verify` reports what it leaves, and the break-glass
recipe of this README ("A row above the head") is the one use that is documented.

**The statements of the extension's own script** (`CREATE EXTENSION`, `ALTER
EXTENSION UPDATE`) are allowed to write while that script runs (it writes the
identity of the chain). In pg_upgrade's binary upgrade mode (a server that only
pg_upgrade reaches) the `ProcessUtility` hook and the event triggers do nothing: the
extension is created empty and its members are added one by one, and the first
event trigger would find the others missing (`kingres.ledger_binary_upgrade()` says
whether the server is in that mode). The ledger tables arrive as files, not as rows.

**History that exists already.** The guard cannot detect rows that a member of
`pg_write_all_data` wrote before it existed. What an auditor can still check:
the digests and the checkpoints that were kept outside the database (a forged
block that was never published is not in them, and every block after it differs
from what was published); `ledger_verify`, which reports a transaction with no
entry (`tx_empty`), a block with no transaction (`block_empty`), a block closed
in the future (`block_time`), a block time that goes back, a transaction or
digest row above the head, a digest whose hash is not its block's, and entries
and rows that do not match; and the `user_name` of the transactions against the
audit log of the server for the time of the block. A forged block that has the
right link, hash and root and a plausible time, and whose transactions have
entries that match rows, cannot be told from a real one inside the database.

**The time of a block.** A block time is the clock of the server when the
builder runs, and never earlier than the block before (the verifier and
`ledger_verify` require it). One block with a time far ahead would therefore
give its time to every later block, silently and for ever (a forged block, or an
honest server whose clock was set to the year 2100 for a minute). The rule, which
changes no hash: **the builder refuses to build a block when the previous block
was closed more than `kingres_ledger.max_clock_skew` ahead of its own clock**
(default one hour, at most about 24 days, `PGC_SUSET`; error `KL004`; nothing is
lost, the transactions stay pending), and `ledger_verify` reports the first block that was closed later
than the time of the verification by more than that (`block_time`, with the
number of blocks that carry a later time), whatever the setting is. A superuser
who has looked at the clock and decided to go on sets
`kingres_ledger.max_clock_skew = 0` (no limit); the finding stays until real time
passes the block, which is intended: the times are wrong and the ledger says so.
A clock that is set back a little (less than the limit, a standby promoted with a
clock that is behind) still gives the time of the previous block.

**Upgrades.** The guard is in the library; the functions that write are in the
SQL of version 0.3. A database whose SQL is 0.1 or 0.2, with the 0.3 library, is
**refused with the statement to run** and is never half protected: the old
functions write with plain statements, which the library refuses (`cannot write
to table "ledger_blocks": the SQL of extension "kingres_ledger" is version 0.2,
and this library writes the ledger only through the functions of version 0.3`,
hint `Run ALTER EXTENSION kingres_ledger UPDATE.`, SQLSTATE `KL005`, which no other
refusal of the guard has; the control plane checks the versions before it calls,
the code is for other callers). The function `ledger_record_row` of 0.1 and 0.2,
which took a table_id and a row, is still in the library as a refusal: it works
only for a superuser with the setting on (the old `ledger_enable` calls it) and
the update drops it. The capture trigger is part
of the library and keeps working; the block builder and the workers fail until
the update, and the transactions stay pending. The update is `ALTER EXTENSION
kingres_ledger UPDATE`, from 0.1 (directly) or from 0.2; the three ways to 0.3
(a new installation, from 0.2, from 0.1) give the same catalog, which
`t/011_upgrade_0_3.pl` compares. A superuser who must build a block before the
update sets the manual-write setting for that session.

**Dump, restore and replication.** `pg_dump` does not dump the ledger tables
(they are not configuration tables of the extension), so a restore writes
nothing into them but the identity of the new chain, which `CREATE EXTENSION`
writes: a restored database has a new, empty ledger, it does not verify as the
old one, and `ledger_verify` names the capture trigger of every table that is not
registered. Take physical backups, or use `pg_upgrade`, which carries the files
(tested in `016_dump_upgrade`). There is no supported way to load a ledger with
`COPY`: the new chain's row cannot be replaced (its triggers refuse `UPDATE` and
`DELETE`). **Logical replication** into a ledger table of the extension is
refused by the guard trigger for every role (a superuser's subscription too: the
apply worker cannot turn the setting on), which is right: a subscriber's chain
has its own identity. Replicating a **user** ledger table works (the capture
trigger writes the entries with the marker raised, in the apply worker). A
**standby** replays the write-ahead log, which does not go through the executor,
so the rows of the primary appear on it and the guard sees nothing (tested in
`002_restart_standby`, `009_worker`, `012_publish_read`, `013_head_inflight` and
`014_guard`).

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

- Rows that a superuser writes: with `kingres_ledger.allow_manual_write` on a
  superuser can insert into every ledger table (and delete transaction rows,
  digests and pending rows); without it nobody can, a member of
  `pg_write_all_data` included (see "Who can write the ledger tables"). What such
  rows do to the chain is found by `ledger_verify` and, for the head, by
  `ledger_head` (see "A row above the head"); a block with the right link, the
  right hash and a plausible time, whose transactions have entries that match
  rows, is not found (see "What is detected, and what is not"). Rows that a
  member of `pg_write_all_data` wrote **before** the guard existed are not
  detected by it either; see "History that exists already" there.
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
  database has the triggers of its tables but a new, empty ledger; inserts into
  a restored ledger table fail (the entry refers to a table id that is not
  registered), and `ledger_verify` reports the capture trigger of an unregistered
  table. Use physical backups, standbys and `pg_upgrade` (tested).
- The clock: block times are the server's clock, kept from going backwards; a
  clock that jumped ahead gives its time to the blocks after it, which
  `ledger_verify` reports and the builder refuses to extend (see "Who can write
  the ledger tables", "The time of a block").
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
7. **The write guard** (`src/guard.c`): the list of what the executor hook sees
   (check it against the callers of `ExecCheckPermissions` of the major you run, and
   against any new way to write a relation that does not call it, as the apply
   worker did not), the marker around the statements of the library (every other
   `SPI` call of the library must be a read), the one statement list of `write.c`
   (no function may be added that takes the content of a write), and the two places
   that decide "allowed" (`kl_write_allowed`, the check hook of `allow_manual_write`).
8. **Not run at all**: `EXEC_BACKEND` (Windows) builds, a standby that
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
