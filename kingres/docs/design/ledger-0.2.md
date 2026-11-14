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
