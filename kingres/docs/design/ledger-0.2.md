<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
# kingres_ledger: design of version 0.2 (rest of phase 6)

Status: accepted for implementation, 2026-11-14.
Builds on [ledger-mvp.md](ledger-mvp.md) (extension version 0.1) and produces
version 0.2 with the upgrade script `kingres_ledger--0.1--0.2.sql`.

Version 0.1 covers append-only tables. Version 0.2 completes phase 6 of the
roadmap in [architecture.md](../architecture.md): updatable ledger tables,
blocks built automatically, and the records the management panel shows.

Not in 0.2 (phase 7): client and server signatures, external anchoring, the
aggregate digest over shard groups, and a standalone verifier (which lives in
`kingres/verifier`).

## 1. Updatable ledger tables

`kingres.ledger_enable(relation, 'updatable')` puts a table under ledger
control without freezing its rows. The table holds the current rows;
everything that ever happened to it stays provable.

### Model

An updatable ledger table has a **history table**,
`<schema>.<table>_ledger_history`, created by `ledger_enable`: the columns of
the table (same names and types, no constraints, no defaults, no identity)
plus `ledger_txid bigint`, `ledger_seq int` and `ledger_op "char"`. It is an
append-only ledger object: only the capture code writes it, and it is
protected by the hook and the event triggers like the other ledger tables.

Every change is recorded as ledger entries with one of two operations:

| Statement | Entries written | History row |
|---|---|---|
| `INSERT` | `I` with the hash of the new row | none |
| `DELETE` | `D` with the hash of the old row | the old row |
| `UPDATE` | `D` with the hash of the old row, then `I` with the hash of the new row | the old row |

`ledger_entry.op` therefore allows `I` and `D`. The row hash and the entry
leaf are unchanged (the entry leaf already contains the operation).
An `UPDATE` that changes no column still writes both entries; that is simpler
than deciding equality, and it is rare.

`TRUNCATE` stays rejected on an updatable ledger table.

### Verification

For an updatable table `ledger_verify` checks, in addition to the checks of
0.1:

- the multiset of hashes of the **current rows** equals the multiset of the
  table's `I` entries minus the multiset of its `D` entries;
- every `D` entry has exactly one history row with the same `ledger_txid`,
  `ledger_seq` and row hash (recomputed from the history row's columns with
  the table's `table_id`), and every history row has its `D` entry;
- no entry is `D` for an append-only table.

An inclusion proof for a row version works as before for `I` entries. A
proof for a `D` entry proves that the version was removed in that
transaction; `ledger_inclusion_proof` takes an optional `op` argument
(default `I`) to select which entry of a row hash to prove, and the proof
carries `op`.

### Schema changes

The history table must follow the table. `ALTER TABLE ... ADD COLUMN` on an
updatable ledger table is allowed under the same rules as in 0.1 and is
applied to the history table by the extension in the same transaction
(through the `ddl_command_end` event trigger). Every other change of column
layout stays rejected.

### Converting

Changing an append-only ledger table to updatable or the reverse is not
supported in 0.2 and is rejected. `ledger_enable` on a table that already has
rows enters them as `I` entries, as in 0.1.

## 2. Blocks built automatically

A background worker builds blocks, so that the window before a block exists
is bounded without relying on an external scheduler.

- A launcher is registered at library load. Every
  `kingres_ledger.block_interval` (default `10s`; `0` disables the worker) it
  starts, for each database in which the extension is installed and which has
  pending ledger transactions, a short-lived worker that calls
  `kingres.ledger_build_block()` once and exits.
- The worker runs as the extension owner, does nothing on a standby, and
  never blocks on the builder lock for long (it gives up and retries at the
  next interval).
- `kingres_ledger.max_block_transactions` still bounds a block.

## 3. Records for operators

Two tables, both append-only for every role and protected like the other
ledger tables, readable by roles that may read `ledger_blocks`.

```sql
-- Every digest this database has generated.  It is a convenience record,
-- NOT a root of trust: a digest proves something only when a copy was kept
-- outside the database.
kingres.ledger_digest (
  digest_id     bigint generated always as identity primary key,
  generated_at  timestamptz not null,
  generated_by  name not null,
  height        bigint not null,
  block_hash    bytea  not null,
  digest        jsonb  not null     -- exactly what ledger_generate_digest returned
)

-- Every verification run and its outcome.
kingres.ledger_verification (
  verification_id bigint generated always as identity primary key,
  started_at    timestamptz not null,
  finished_at   timestamptz,
  started_by    name not null,
  from_height   bigint not null,
  to_height     bigint,
  state         text not null check (state in ('running','clean','problems','failed')),
  problem_count int,
  blocks_checked bigint,
  rows_checked  bigint,
  problems      jsonb                -- the rows ledger_verify returned, capped
)
```

- `ledger_generate_digest()` records its result in `ledger_digest`.
- New function `kingres.ledger_verify_recorded(from_height bigint default 0,
  to_height bigint default null) returns bigint`: runs `ledger_verify`,
  records the run in `ledger_verification` (state `running` first, in an
  autonomous way that survives a failure of the verification itself where
  PostgreSQL allows it; otherwise the row is written at the end and a
  failure is reported to the caller), and returns the verification id.
  `ledger_verify` itself is unchanged and records nothing.
- New view `kingres.ledger_status`: one row with the mode summary (number of
  append-only and updatable tables), the latest block (height, hash, closed
  at), the number of pending transactions and the age of the oldest, the
  last digest, and the last finished verification with its state.

## 4. Settings added

| GUC | Default | Meaning |
|---|---|---|
| `kingres_ledger.block_interval` | 10s | How often the worker looks for pending transactions; 0 disables it |

## Tests required

- Updatable tables: insert, update, delete, update of an unchanged row,
  several changes to one row in one transaction, rollback and savepoints;
  history rows and entries after each; a clean `ledger_verify`.
- Tamper cases for updatable tables, performed the way an attacker with
  catalog access would: a current row changed without entries, a current row
  removed, a history row changed, a history row removed, a `D` entry removed
  together with its history row, an old version put back as the current row.
  Each must be reported.
- Proof of an `I` entry and of a `D` entry; rejection when `op` is altered.
- Every protection rule for the history table and the two new tables;
  `ADD COLUMN` propagation; rejection of the mode conversion.
- The background worker (TAP): a committed transaction is in a block within a
  bounded time without any call; it does nothing on a standby; it survives a
  restart; `block_interval = 0` disables it; two databases are both served.
- `ledger_digest`, `ledger_verify_recorded` and `ledger_status`.
- `ALTER EXTENSION kingres_ledger UPDATE` from 0.1 with existing append-only
  ledger tables, blocks and digests: everything still verifies, and digests
  generated before the upgrade still verify after it.
- The hash format is unchanged: the format tests of 0.1 pass unmodified.

## Implementation notes and deviations

Added with the implementation in `contrib/kingres_ledger/` (version 0.2). The
text above is unchanged. The README of the extension has the exact behaviour,
the privilege model and the complete list of what the protections cannot stop.
Where this section says "deviation" the implementation does something the
specification does not say or does differently.

### Deviations and additions

1. **The history table's owner and privileges.** The history table is created
   by `ledger_enable` and belongs to the extension owner; the owner of the
   ledger table gets `SELECT` on it and nobody else gets anything (the
   history holds data that the table's own privileges and row security would
   protect). The specification only says the capture code writes it.
2. **A third blocking trigger on the history table**,
   `kingres_ledger_block_insert` (`BEFORE INSERT`), rejects every SQL insert.
   The capture code inserts through the table access method, which fires no
   trigger, so this closes SQL inserts of forged history rows for every role
   without costing the capture path anything. The specification lists UPDATE,
   DELETE and TRUNCATE as blocked; INSERT is blocked in addition.
3. **Registry and queue columns.** `ledger_table` gains `history_relid`
   (regclass, NULL for append-only tables; a check ties it to the mode) and
   `ledger_pending` gains `queued_at` (the time of the first entry of the
   transaction). The latter is needed for "the age of the oldest" pending
   transaction in `ledger_status`, which the specification asks for but the
   0.1 queue could not answer. It is not covered by any hash; it is NULL for
   transactions queued by 0.1.
4. **`ledger_verification` never has state `running`.** The table allows it,
   as specified, but PostgreSQL has no autonomous transaction and the table is
   append-only, so a `running` row cannot be written and later finished. The
   run is written when it ends, as the specification permits ("otherwise the
   row is written at the end"); an error inside `ledger_verify` is caught and
   recorded as `failed`, with a warning; an error of the arguments (an invalid
   range) raises before anything is recorded. `rows_checked` is the number of
   ledger entries of the blocks in the range; the problems are capped at 100
   (the count is exact).
5. **`ledger_status` is not readable by PUBLIC** (the number of pending
   transactions says how active the database is); the specification says
   nothing about it. `ledger_digest` and `ledger_verification` are readable by
   PUBLIC as specified. `ledger_verify` does not compare recorded digests with
   the chain (that would make a consistent rewrite of the blocks detectable
   only if the attacker forgot the table, and changes the 0.1 tests that show
   exactly such a rewrite passing); `ledger_verify_digest(digest)` is how a
   recorded digest is checked.
6. **`ledger_inclusion_proof` is an overload, not one function with a default.**
   The three-argument form `(relation, row_hash, op "char")` is new; the
   two-argument form stays as a `SECURITY DEFINER` SQL wrapper of it with op
   `I`, so that grants made on the two-argument function under 0.1 survive the
   upgrade (replacing it by a function with another signature would drop them
   silently). The wrapper is one more function in the exact set that the
   test `ledger_privs` asserts as `SECURITY DEFINER`, with that reason.
7. **`ledger_verify_proof` gained a five-argument form** that takes the
   expected operation; the four-argument form of 0.1 now accepts an `I` proof
   only (the operation `D` did not exist in 0.1, so every proof it ever
   accepted was an `I` proof). The check also became stricter in ways the
   format documentation needed (see "Format documentation" below).
8. **The workers.** One launcher at library load (started when recovery has
   finished, restarted after 10 seconds if it dies) and one short-lived worker
   per database, run one after another. Because a launcher cannot look into a
   database, a backend that commits a transaction with ledger entries records
   its database in a small set in shared memory (an additional shared memory
   segment of a few hundred bytes), and the launcher visits those databases;
   it visits every database in its first round after the start or promotion
   of the server and every sixth round (covering a lost set, an overflow, and a
   prepared transaction committed later). This implements "for each database
   in which the extension is installed and which has pending ledger
   transactions" without starting a process for every database every
   interval. The worker's lock timeout is one second or the interval if
   shorter. `kingres_ledger.block_interval` is a `PGC_SIGHUP` setting in
   milliseconds, as a server setting only (a database or role setting is not
   seen by the launcher).
9. **History columns after `ADD COLUMN`.** A column added to the table is
   appended to the history table, after `ledger_op`; the rows of the history
   table are matched to the table by column name, and the row hash of a history
   row encodes the columns other than the three ledger ones in their order,
   which equals the order of the table. The follow-up `ALTER TABLE` on the
   history table runs only after an `ALTER TABLE` command that names a
   registered updatable table, through the event trigger and a flag set by one
   internal function.
10. **Names.** A table with a column named `ledger_txid`, `ledger_seq` or
    `ledger_op` cannot be made updatable (the names would clash in the history
    table), the names are rejected by `ADD COLUMN`, and a table whose history
    name (`<table>_ledger_history`) would be longer than 63 bytes or already
    exists is rejected.
11. **Event trigger and hook additions.** The status view and the history
    table are recognised by the hook (`DROP`, `CREATE OR REPLACE`, rename,
    `SET SCHEMA`, grants other than `SELECT`); the event triggers reject drops
    and rewrites of history tables and check their state (`history_missing`,
    `history_state`, `history_trigger`, `history_columns`).
12. **Existing tests changed.** `ledger_capture` called `ledger_enable(...,
    'updatable')` to show "not supported yet"; it now uses an invalid mode
    (`'bogus'`) for the same check. The expected output of `ledger_hash` lists
    the new GUC `kingres_ledger.block_interval` (the only change there, no
    hash or format line) and shows the value `0` that the new first test
    `ledger_setup` sets; the expected output of `ledger_privs` lists the new
    functions, tables and the changed `SECURITY DEFINER` set. The TAP test
    `002_restart_standby` sets `kingres_ledger.block_interval = 0` because it
    checks that a committed transaction is still queued after crash recovery,
    which the workers would otherwise have emptied. No assertion was removed or
    weakened.

### Format documentation

The independent verifier found twelve gaps in the description of format
version 1 (`kingres/verifier/README.md`). All are resolved in the README of the
extension; the format itself did not change. Three behaviours of the server's
own `ledger_verify_proof` turned out to be defects and were fixed, with tests
in `ledger_updatable`: it hashed the user name of a proof as it stood in the
JSON (database encoding) instead of in UTF-8 as the block builder does (different
bytes for a non-ASCII name in a database that is not UTF-8); it did not check
that `format` is 1; it accepted any one-character `op`; it treated a malformed
`block_hash` of a header in `chain` as an absent one; and it ignored the
headers of `chain` beyond the digest height completely (they are now parsed
strictly, like the verifier does, but not used). The rest of the proposals
describe what the code already did.

### Known gaps

- A test of a non-ASCII user name in a database that is not UTF-8 is not part
  of the suite (it needs a role whose name is valid in two encodings); the
  conversion is the one `ledger_block_tx_leaf` uses.
- The state `failed` of `ledger_verification` is written by code that only a
  failure inside `ledger_verify` reaches; no test makes `ledger_verify` fail.
- The launcher has not been tested with `EXEC_BACKEND` (Windows); the shared
  memory pointer is not re-attached there.

## Notes of the second review fixes (findings L2-01 to L2-08)

The review `kingres/docs/reviews/2026-11-14-kingres-ledger-0.2.md` found eight
defects. Version 0.2 was not released, so the fixes are in `kingres_ledger--0.2.sql`
and `kingres_ledger--0.1--0.2.sql` (no new version); the hash format did not
change. What was done, with the decisions the review left open.

**L2-01, the `kl_internal_ddl` window.** The flag is gone. `ledger_history_sync`
makes the `ADD COLUMN` of the history table with `AlterTableInternal` (the catalog
API), on the history table whose OID it was given and with the subcommands it built
itself, with the collection of commands for event triggers inhibited; nothing is
run as a statement, so no event trigger of a user, no function and no default
expression runs while it works, and the hook has nothing to be told. The
privileges of the history table are changed the same way (`ExecuteGrantStmt`).

Every state of the form "we are inside our own code" that was examined:

| State | Where | Verdict |
|---|---|---|
| `kl_internal_ddl` (global boolean, hook skipped every statement) | `kingres_ledger.c`, `protect.c` | removed (L2-01) |
| `creating_extension && CurrentExtensionObject == extoid` (hook skipped every statement while the install or upgrade script of the extension ran, including statements of event triggers fired by the script) | `protect.c` | scoped: the hook remembers the query string of the first statement of the script (the text `execute_sql_string` passes for each of its statements) and trusts only statements with that very string; a statement run by an event trigger, a function or SPI has a text of its own and is checked fully. The state is forgotten as soon as no script of this extension runs. Limit: one step of upgrade per command (a chain of scripts would need the rule extended; there is one step) |
| `SetUserIdAndSecContext(extowner)` in the worker | `worker.c` | not a relaxation of a check; the worker only calls `kingres.ledger_build_block()` (qualified) with the settings overridden; the launcher is not connected to a database |
| `SetUserIdAndSecContext(extowner)` in the hook after `REASSIGN OWNED` | `history.c` | new; runs only `kl_sync_history_acl` (fixed catalog operations on the history tables of the registry); the user id is restored in `PG_FINALLY` |
| `SECURITY DEFINER` functions of the extension | SQL | each runs fixed statements with a fixed `search_path`; the function that runs on behalf of a user (`ledger_enable`) checks the ownership by hand; no flag |
| `ENABLE ALWAYS` triggers and `kl_preloaded` | | not relaxations (`kl_preloaded` only refuses `ledger_enable`) |
| `ledger_guard` and `ledger_all_defects` | SQL | read-only checks; the guard now also calls the ACL sync, which only brings the privileges to the rule |
| `kl_rv_relid`: no lock for a role that does not own the table | `protect.c` | an optimisation that was reviewed in 0.1 (L-03); it does not skip a check |

**L2-02, privileges of the history table.** The rule: the extension owner and the
owner of the ledger table read it, nobody else. `ledger_enable` sets the ACL
explicitly (default privileges are revoked); the event trigger repairs every
history table after every DDL command (also what `GRANT ... ON ALL TABLES IN
SCHEMA` did), the hook repairs after `ALTER TABLE ... OWNER TO` and `REASSIGN
OWNED` (which fire no event trigger), and a `GRANT` on a history table is rejected.
`ledger_verify` and the guard report `history_privileges`. Decision: no grant
to other roles (the previous documentation said "grant `SELECT` yourself");
membership in the owner role is the way, documented with the bypass of row
security and column privileges.

**L2-03, settings of the database.** `BackgroundWorkerInitializeConnectionByOid`
has no flag that skips `pg_db_role_setting`; the worker overrides the list in
`kl_harden_settings` with `PGC_S_OVERRIDE` before its transaction (the list is in
the README). The worker is also cancelled by its own `USER_TIMEOUT` timer after
`kingres_ledger.worker_timeout` (a GUC added by this fix: a fix needs a limit that
can be tested), reports its outcome in shared memory (`ledger_worker_state`,
shown by `ledger_status`), is not started again for a delay that doubles with
every failure up to an hour, and logs one line per failure at `LOG`.
`synchronous_commit` and `work_mem` are not overridden. The SQL of the extension
has no backslash (a literal `'\x00'` and a `LIKE` with a backslash escape were
replaced) and no dependence on session settings; `ledger_review2` runs every
function under hostile session settings.

**L2-04, readability.** `ledger_verification` is not readable by PUBLIC;
`ledger_digest` is readable except `generated_by`. The view `ledger_status` calls
`ledger_worker_state`, and PostgreSQL checks `EXECUTE` of a function in a view for
the caller, so a reader of the view needs `EXECUTE` on it too (documented in
"Privileges"). Existing tests: `ledger_records` grants that and the read of the
verifications now needs a grant; `ledger_updatable` expected that a `GRANT SELECT`
on a history table works, and it is now rejected (expected output changed);
`ledger_hash` lists the new GUC; `ledger_privs` lists the new functions and the
columns. No assertion was removed.

**L2-05.** The caches are dropped by the subtransaction abort callback.

**L2-06.** Launcher: the per-round list is freed (round memory context) and so is
the worker handle; sweeps skip databases where no extension was found (nine of
ten sweeps) and databases in their failure delay; order continues after the last
visited database; a round stops at the worker timeout and its remainder goes to
the next round; a worker has a timeout and is terminated by the launcher five
seconds after it; a missing
worker slot is logged once and then every sixty rounds.

**L2-07.** The verifier accepts `I` and `D` proofs, `--op I|D|any`, fixtures from an
updatable table.

**L2-08.** Records: decision "administrators only" (see README, "Growth of the
records"). A cancelled `ledger_verify_recorded` is recorded as `failed`. The
assertions of `009_worker` are on outcomes. The README has "Where a reviewer
should attack first".

**Areas the review did not examine**, established by test (`ledger_review2`,
`010_review2`): `COPY ... FREEZE` works in both orders; logical replication apply
into an updatable table works, a missing row is skipped, a replicated `TRUNCATE`
fails the apply worker until the subscription is disabled; `max_worker_processes`
used up leaves transactions pending (`ledger_status` shows them), logs once, and
a call of `ledger_generate_digest` builds the block; `pg_dump` and restore give a
database with the rows and an empty ledger (documented limit, not a defect).
