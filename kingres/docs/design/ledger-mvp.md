# kingres_ledger: design of the ledger MVP (phase 6, first increment)

Status: accepted for implementation, 2026-11-14.
Scope: the first increment of phase 6 of the roadmap in
[architecture.md](../architecture.md): append-only ledger tables, blocks,
digests, verification and inclusion proofs on a single PostgreSQL instance.
The ledger does not depend on sharding, so it is built in parallel with
phase 1.

Not in this increment: updatable ledger tables (history tables), client and
server signatures, external anchoring, the per-shard aggregate digest, a
custom WAL resource manager, and enforcement inside the PostgreSQL core.

## What the ledger guarantees, and what it does not

The ledger makes tampering **detectable**, not impossible. Anyone who can
modify the database files, restore an old backup or replace the server binary
can change data; what they cannot do is change it without the change being
found by a verification against a digest that was stored outside the
database. The digest stored outside is the root of trust. This must be stated
in the extension's README in these terms.

There is also a window: a committed transaction is covered by a digest only
once it has been placed in a block and a digest of that block has left the
database. Until then a privileged attacker could alter its ledger entries
undetected. The window is the block interval plus the digest export interval.

## Data model

Schema `kingres`. Every table below is append-only for every role, enforced
by triggers; the only writer is the extension's own code.

```sql
-- Tables under ledger control.  table_id is the stable identity used inside
-- hashes; a regclass OID is not stable across dump and restore.
kingres.ledger_table (
  table_id   int generated always as identity primary key,
  relid      regclass not null unique,
  mode       text not null check (mode in ('append_only')),
  enabled_at timestamptz not null default now()
)

-- The random identity of this chain, one row, written when the extension is
-- created; the prev_hash of block 0 is derived from it (see "Hashing").
kingres.ledger_chain (
  singleton  boolean primary key default true check (singleton),
  chain_id   bytea not null check (octet_length(chain_id) = 32),
  created_at timestamptz not null default now()
)

-- One row per row inserted into a ledger table.
kingres.ledger_entry (
  ledger_txid bigint  not null,   -- top-level transaction id (xid8) of the writer
  seq         int     not null,   -- order of the row inside its transaction, from 0
  table_id    int     not null references kingres.ledger_table,
  op          "char"  not null check (op in ('I')),
  row_hash    bytea   not null,   -- leaf hash of the row, 32 bytes
  primary key (ledger_txid, seq)
)

-- Transactions that have entries but are not in a block yet.  This is a work
-- queue, not part of the ledger: the block builder deletes from it.
kingres.ledger_pending (ledger_txid bigint primary key, user_name name not null)

-- One row per transaction, written by the block builder.
kingres.ledger_tx (
  ledger_txid  bigint primary key,
  block_height bigint not null,
  tx_index     int    not null,   -- position inside the block, from 0
  entry_count  int    not null,
  merkle_root  bytea  not null,   -- Merkle root of the transaction's entries
  user_name    name   not null,
  unique (block_height, tx_index)
)

kingres.ledger_blocks (
  height         bigint primary key,   -- from 0, no gaps
  hash_version   smallint not null,
  prev_hash      bytea not null,       -- SHA-256(0x14 || chain_id) for height 0
  tx_merkle_root bytea not null,       -- Merkle root of the block's transaction roots
  tx_count       int   not null,
  closed_at      timestamptz not null,
  block_hash     bytea not null
)
```

## Hashing

All hashes are SHA-256, computed with PostgreSQL's `pg_cryptohash` API. The
format is versioned by `hash_version` (this increment defines version 1) so a
later format can coexist with existing blocks. All integers in hash inputs
are big-endian. Nothing had been released when the format was hardened (see
"Notes of the fix round" at the end), so version 1 was changed in place.

**Domain tags.** Every kind of hashed data starts with its own one-byte tag,
so data of one kind can never be read as data of another:

| Tag | Value | First byte of |
|---|---|---|
| row | `0x10` | the canonical row encoding (the data of the row's leaf) |
| entry | `0x11` | the leaf data of an entry in a transaction tree |
| block transaction | `0x12` | the leaf data of a transaction in a block tree |
| block | `0x13` | the input of the block hash |
| chain | `0x14` | the input of the hash that gives the `prev_hash` of block 0 |

**Canonical row encoding.** Never hash the text form of a value: text output
depends on session settings such as `DateStyle` and `extra_float_digits`. The
encoding of a row is:

```
u8   tag 0x10
u16  hash_version
u32  table_id
u16  number of encoded columns
for each non-dropped column whose value is not NULL, in column order:
    u16  length of the column name, then the name in UTF-8
    u32  length of the value, then the value as produced by the type's
         binary send function
```

NULL columns are omitted, so adding a nullable column later does not change
the hash of existing rows. Types without a binary send function cannot be
used in a ledger table; `ledger_enable` rejects them.

**Merkle trees** follow RFC 6962: a leaf hash is `SHA-256(0x00 || data)`, an
inner node is `SHA-256(0x01 || left || right)`, and a tree of `n` leaves
splits at the largest power of two smaller than `n`. The tree of zero leaves
has root `SHA-256("")`.

- `row_hash` = leaf hash of the canonical row encoding.
- Transaction root = Merkle root over the transaction's `row_hash` values in
  `seq` order, each wrapped again as a leaf together with its `seq`, table id
  and operation: leaf data = `0x11 || u32 seq || u32 table_id || u8 op ||
  row_hash`.
- Block transaction root = Merkle root over the leaves
  `0x12 || u64 ledger_txid || u32 entry_count || merkle_root || u16 length of
  user_name || user_name in UTF-8`, in `tx_index` order (see "Implementation
  notes and deviations", item 12).
- `block_hash` = `SHA-256(0x13 || u16 hash_version || u64 height || prev_hash ||
  tx_merkle_root || u32 tx_count || i64 closed_at in microseconds since
  2000-01-01 UTC)`.

**Chain identity.** `kingres.ledger_chain` holds one row, written when the
extension is created: `chain_id`, 32 random bytes. The `prev_hash` of block 0
is `SHA-256(0x14 || chain_id)`, not 32 zero bytes. A digest and every
inclusion proof carry `chain_id`; the checks of digests and proofs reject
those of another chain, and `ledger_verify` checks that block 0 starts from
the stored identity. This keeps the digest or proof of one ledger from being
taken for another; databases that are copies of each other share the identity
until they diverge, and the identity is neither secret nor authenticated.

**Block time.** `closed_at` is the server's clock, and never earlier than the
`closed_at` of the previous block: the builder uses the previous value when
the clock went backwards, and `ledger_verify` and the digest and proof checks
reject a chain in which a block is earlier than its predecessor. The time is
evidence only relative to an external anchor.

A **digest** is the tuple (chain_id, height, block_hash) of the latest block,
returned as JSON together with the generation time and the database name. An
**inclusion proof** carries the chain identity and the table id of the entry
(which the entry leaf and the row encoding cover), so it is a proof for one
table; it cannot know the row, and a verifier has to recompute the row hash
from the row it holds.

## Capturing writes

`kingres.ledger_enable(relation regclass, mode text default 'append_only')`
registers the table and installs triggers on it, all created `ENABLE ALWAYS`
so that `session_replication_role = replica` does not skip them:

- an `AFTER INSERT ... FOR EACH ROW` trigger, written in C, that computes
  `row_hash`, appends the `ledger_entry` row with the next `seq` of the
  transaction, and on the transaction's first entry inserts into
  `ledger_pending`;
- `BEFORE UPDATE OR DELETE ... FOR EACH ROW` and `BEFORE TRUNCATE` triggers
  that raise `ERROR: table "<name>" is an append-only ledger table`
  (SQLSTATE `KL001`).

A table that already contains rows can be enabled; its existing rows are
entered into the ledger by the enabling transaction.

The per-transaction sequence counter lives in backend memory and is reset at
transaction end through a transaction callback. Subtransactions that roll
back must not leave gaps that break verification: `seq` values only need to
be unique and ordered inside the transaction, not dense.

## Protection against being switched off

The extension installs a `ProcessUtility` hook and must therefore be loaded
through `shared_preload_libraries`; `ledger_enable` refuses to run otherwise.
The hook rejects, for every role including superusers:

- `DROP TABLE`, `ALTER TABLE ... DISABLE TRIGGER`, `DROP COLUMN`,
  `ALTER COLUMN ... TYPE`, `RENAME COLUMN`, `DETACH/ATTACH PARTITION`,
  `SET SCHEMA`, and `DROP TRIGGER` on a ledger table or its ledger triggers;
- any DDL on the `kingres.ledger_*` tables;
- `DROP EXTENSION kingres_ledger` while any ledger table exists.

`ALTER TABLE ... ADD COLUMN` is allowed when the new column is nullable and
has no default. There is deliberately no function to disable the ledger on a
table.

This stops accidents and ordinary privileged users. It does not stop someone
who edits the catalog directly or removes the library from the configuration;
only verification against an external digest detects that.

## Building blocks

`kingres.ledger_build_block() returns bigint` takes a transaction-level
advisory lock so that only one builder runs, then:

1. reads the pending transactions that are visible to its snapshot;
2. orders them by `ledger_txid`;
3. computes each transaction's Merkle root from its entries;
4. writes the `ledger_tx` rows and the `ledger_blocks` row, and deletes the
   processed rows from `ledger_pending`.

It returns the new height, or NULL when nothing was pending. A transaction
that commits after the builder's snapshot is simply picked up by the next
block; no transaction can be skipped or included twice.

The order inside a block is by transaction id, which is not commit order.
Tamper evidence does not need commit order, only an order that is recorded
and reproducible.

There is no background worker in this increment. Blocks are built when
`ledger_generate_digest()` is called and when something calls
`ledger_build_block()`; in a deployed cluster the KingresDB agent calls it
on the interval configured for the database.

## Public functions

| Function | Purpose |
|---|---|
| `kingres.ledger_enable(regclass, text)` | Put a table under ledger control |
| `kingres.ledger_row_hash(record) returns bytea` | Leaf hash of a row of a ledger table, e.g. `SELECT kingres.ledger_row_hash(a) FROM accounts a` |
| `kingres.ledger_build_block() returns bigint` | Close a block from pending transactions |
| `kingres.ledger_generate_digest() returns jsonb` | Close a block if needed and return the digest of the latest block |
| `kingres.ledger_verify(from_height bigint default 0, to_height bigint default null) returns table (check_name text, object text, detail text)` | Recompute everything in the range; returns one row per problem, no rows when the ledger is consistent |
| `kingres.ledger_verify_digest(digest jsonb) returns boolean` | Whether a previously exported digest matches the stored chain |
| `kingres.ledger_inclusion_proof(relation regclass, row_hash bytea) returns jsonb` | Proof that a row is included: both Merkle paths and the block header |
| `kingres.ledger_verify_proof(proof jsonb, digest jsonb) returns boolean` | Check a proof using only its content and a trusted digest; reads no table |

`ledger_verify` must check, at least: block heights are dense; every
`prev_hash` and `block_hash`; every block transaction root; every transaction
root against its entries; every entry belongs to a transaction in a block or
to a pending transaction; and, for each ledger table, that the multiset of
row hashes of the table's current rows equals the multiset of its entries.
The last check is what detects a modified, deleted or smuggled-in row.

## Implementation split

- C: the insert trigger, canonical row encoding and `ledger_row_hash`, the
  Merkle primitives (a root aggregate and a path function), the block hash
  function, the `ProcessUtility` hook, and `ledger_verify_proof`.
- PL/pgSQL or SQL, using those primitives: `ledger_enable`, block building,
  digest generation, `ledger_verify`, `ledger_inclusion_proof`. Keeping the
  orchestration in SQL makes it short and easy to audit.

All functions that write ledger tables are `SECURITY DEFINER` with a fixed
`search_path`, owned by the extension owner; ordinary roles get no direct
privileges on the ledger tables beyond `SELECT`.

## Settings

| GUC | Default | Meaning |
|---|---|---|
| `kingres_ledger.max_block_transactions` | 100000 | A block is closed with at most this many transactions; the rest wait for the next block |

## Source layout

```
contrib/kingres_ledger/
  Makefile, meson.build, kingres_ledger.control, kingres_ledger--0.1.sql, README.md
  src/kingres_ledger.c   module load, GUCs, hook installation
  src/row_hash.c|h        canonical encoding and row hash
  src/merkle.c|h          RFC 6962 trees, root aggregate, path, proof check
  src/capture.c           the insert trigger and transaction state
  src/protect.c           the ProcessUtility hook
  sql/, expected/         pg_regress tests
  t/                      TAP tests (restart, standby)
```

## Tests required for this increment

- Hash test vectors: the RFC 6962 trees for 0, 1, 2, 3, 5 and 8 leaves
  computed independently in the test (with `sha256()` in SQL) and compared
  with the C implementation.
- Row hash is independent of `DateStyle`, `extra_float_digits`,
  `bytea_output` and `IntervalStyle`; adding a nullable column does not
  change existing row hashes.
- Insert, block building, digest, and a clean `ledger_verify`.
- Every protection rule: update, delete, truncate, each rejected DDL, and
  `session_replication_role = replica` not bypassing the triggers.
- Tamper detection, with the triggers bypassed the way an attacker with
  catalog access would: a modified row, a deleted row, an inserted row
  without an entry, a modified entry, a modified block; each must be reported
  by `ledger_verify`, and `ledger_verify_digest` must fail for a rewritten
  chain.
- Inclusion proof round trip, and rejection of a proof with one byte changed.
- Rolled-back transactions and subtransactions leave no entries.
- Concurrent writers while blocks are being built (TAP): every committed
  transaction ends up in exactly one block.
- The ledger survives a restart and is identical on a streaming standby (TAP).

## Implementation notes and deviations

Added 2026-11-14 with the implementation in `contrib/kingres_ledger/`. The
text above is unchanged. The README of the extension holds the exact hash
formats and the list of protections. Where this section says "deviation", the
implementation does something the specification does not say or does
differently; everything else in the specification is implemented as written.

### Deviations

1. **Client encoding while hashing.** The specification says the value is "as
   produced by the type's binary send function". The send functions of
   text-like types (`text`, `varchar`, `name`, `json`, `jsonb`, `xml`, enum
   labels, ...) convert from the database encoding to `client_encoding`, so
   the plain binary form depends on a session setting, which the specification
   forbids for the hash. The implementation sets the client encoding to UTF-8
   while the send functions run and restores it in a `PG_FINALLY` block. Hashes
   are therefore independent of `client_encoding` and, for convertible text, of
   the database encoding.
2. **Types that `ledger_enable` rejects.** The binary form of composite values
   and of arrays contains type OIDs, which are not stable across dump and
   restore, so `ledger_enable` rejects composite types, arrays whose element
   type is user-defined (OID of 16384 or more) and virtual generated columns (a
   whole-row value expands them, a stored tuple does not), in addition to types
   without a binary send function.
3. **What "ledger table" means to the hook.** A table is protected from the
   moment its `kingres_ledger_capture` trigger is `ENABLE ALWAYS`. `ledger_enable`
   creates the three triggers, arms the two blocking triggers and arms the
   capture trigger last, which is what lets its own `ALTER TABLE ... ENABLE ALWAYS
   TRIGGER` statements through the hook. The `kingres.ledger_*` tables are
   recognised as members of the extension.
4. **More rules than the specification lists**, all with SQLSTATE `KL002`:
   every form of `ALTER TABLE ... ENABLE/DISABLE TRIGGER` (an `ENABLE TRIGGER`
   would turn `ENABLE ALWAYS` into origin-only and let
   `session_replication_role = replica` skip the capture trigger), changing
   persistence, inheritance, rules or a generation expression, creating a table
   that inherits from a ledger table, replacing or renaming the ledger triggers,
   replacing or altering the functions of the extension, `ALTER EXTENSION ...
   ADD/DROP/SET SCHEMA`, renaming the schema `kingres`, `DROP SCHEMA` and `DROP
   OWNED` that would drop a ledger table, `DROP TYPE/DOMAIN/EXTENSION ... CASCADE`
   that would drop the column of a ledger table, and `ALTER TYPE ... RENAME VALUE`
   of an enum used by a ledger column (the label is part of the row hash).
5. **Inclusion proofs carry a chain of block headers.** A proof for a row in
   block `b` is verified against a digest of the latest block, which is later
   than `b`; the proof therefore lists the headers of the blocks after `b` up to
   the latest one (`chain`), and `ledger_verify_proof` follows the `prev_hash`
   links up to the digest height. A digest older than the proof's block cannot
   verify the proof. The proof also carries `entry_index` and `tx_index`
   (positions in the trees), because `seq` is not dense.
6. **Additional functions and checks.** C helpers `ledger_leaf_hash`,
   `ledger_node_hash`, `ledger_tx_leaf`, `ledger_block_tx_leaf`,
   `ledger_block_hash`, `ledger_merkle_path`, `ledger_timestamptz_us` and the
   aggregate `ledger_merkle_root`; `ledger_hook_active`, `ledger_outer_user` and
   `ledger_record_row` (internal). `ledger_verify` also checks malformed values,
   transactions that point to no block, transactions that are both pending and in
   a block, entries of an unknown table, and that the triggers of every ledger
   table and of the ledger tables themselves exist and are `ENABLE ALWAYS`.
   `proof.c` holds the proof check (the specification puts it in `merkle.c`).
7. **Privileges.** `ledger_build_block`, `ledger_generate_digest`,
   `ledger_verify` and `ledger_inclusion_proof` are not executable by PUBLIC (a
   role that could call `ledger_build_block` could close arbitrarily small
   blocks); the capture trigger function and `ledger_record_row` are revoked
   from PUBLIC so that nobody can attach them to a table of their own and forge
   entries. `ledger_enable` is callable by the owner of the table (checked
   against the role that called it, as seen outside the `SECURITY DEFINER`
   function). `ledger_verify` runs with `row_security = off`.
8. **Isolation level.** `ledger_build_block` requires READ COMMITTED: the
   builder must see the block written by the previous builder, which an older
   REPEATABLE READ snapshot would not.
9. **Schema creation.** The control file has `schema = kingres` and
   `relocatable = false`. `CREATE EXTENSION` creates the schema when it is
   missing and uses it when it exists, and the schema is never a member of the
   extension (so `DROP EXTENSION` does not take a schema that `kingres_shard`
   shares). `GRANT USAGE ON SCHEMA kingres TO PUBLIC` is part of the script.
10. **Table restrictions of `ledger_enable`.** Ordinary permanent tables outside
    the schemas `kingres`, `information_schema` and `pg_*`; no partitioned
    tables, no inheritance, no tables with rules. These raise "not supported yet"
    style errors (`0A000`).
11. **The extra index** `ledger_entry_row_hash_idx (table_id, row_hash)` supports
    `ledger_inclusion_proof`. Constraints and indexes have explicit names.

12. **The user name is part of the block transaction leaf.** The specification
    covered `ledger_txid`, `entry_count` and the transaction root only, which left
    `user_name` (who wrote the transaction) outside every hash. The leaf data is
    now `u64 ledger_txid || u32 entry_count || merkle_root || u16 length of
    user_name || user_name in UTF-8`, still hash version 1 (nothing had been
    released). The name is the one captured in `ledger_pending` when the
    transaction wrote its first entry: the current user of the statement as seen
    outside `SECURITY DEFINER` functions (after `SET ROLE` the role switched to).
    Inclusion proofs carry `user_name`, and `ledger_block_tx_leaf` takes it as a
    fourth argument. The name can still be altered while the transaction is
    pending, before its block exists.

### Review of the hash formats

No second-preimage or ambiguity flaw that requires a change of the format was
found. The points that were checked:

- Leaves and inner nodes of every tree are domain separated by the RFC 6962
  prefix bytes. The row encoding, the entry leaf data (42 bytes: tag, `u32 seq`,
  `u32 table_id`, `u8 op`, 32 bytes of row hash) and the block transaction leaf
  data (47 bytes plus the user name: tag, `u64 ledger_txid`, `u32 entry_count`,
  32 bytes of transaction root, `u16` name length) have different layouts and
  are never mixed in one tree (corrected in 0.2: the counts used to say 41 and
  44, from before the domain tags and the user name were added).
- The row encoding is unambiguous: every variable-length item is length
  prefixed, the number of columns is explicit, and the table identity and the
  hash version are inside. Omitting NULL columns is intended; two rows that differ
  only in a column that is NULL in one of them differ in the encoding, and the
  same row before and after `ADD COLUMN` has the same hash.
- An audit path does not authenticate the size of its tree. The sizes are
  authenticated one level up: the transaction leaf contains `entry_count`, and
  the block hash contains `tx_count`. `ledger_verify_proof` uses those values.
- The block hash input starts with the tag `0x13` (the fix round replaced the
  earlier remark that it started with the version `0x0001`) and is 87 bytes:
  `1 + 2 + 8 + 32 + 32 + 4 + 8`. It is not used inside a tree and has the same
  length as no leaf data.

One weakness found in the first review is closed in hash version 1 (item 12 of
the deviations). Weaknesses that remain:

- The user name is covered only from the moment the transaction is in a block;
  in `ledger_pending` it can be changed undetected, like everything else in the
  window before a block and its digest exist.
- `closed_at` is covered by the block hash but is the server clock, not an
  external time source.
- The order inside a block is by transaction id, as specified; commit order is
  not recorded.

### Known gaps in the protection hook

See the README, "What the hook cannot stop". In short: direct catalog edits, a
restart without the library, `DROP TYPE ... CASCADE` through a domain or composite
built on a column type, changes of the send format of an extension type, and
anything below the SQL layer.

## Notes of the fix round (2026-11-14, after the review of 0.1)

Added after the independent review `kingres/docs/reviews/2026-11-14-kingres-ledger-0.1.md`
(findings L-01 to L-12). The extension is still version 0.1 and nothing had been
released, so the install script and hash format version 1 were changed in place.
The "Hashing" section above now describes the hardened format; the README has the
tables, the exact inputs and the list of what the protections cannot stop. Where
this section contradicts the notes above it, this section is current.

### Hash format hardening (still format version 1)

- **Domain tags.** Row encoding `0x10`, transaction entry leaf `0x11`, block
  transaction leaf `0x12`, block hash input `0x13`, genesis hash input `0x14`
  (table in the README). This replaces the remark in "Review of the hash
  formats" that the block hash input starts with `0x0001`: it starts with `0x13`.
- **Chain identity.** New table `kingres.ledger_chain` (one row, 32 random
  bytes, written at `CREATE EXTENSION`). The `prev_hash` of block 0 is
  `SHA-256(0x14 || chain_id)`. Digests and proofs carry `chain_id`;
  `ledger_verify_digest` and `ledger_verify_proof` reject another chain;
  `ledger_verify` checks block 0 against the stored value. The identity is not
  put into the hash of every block: a proof of a later block checks it by
  equality with the digest, and the proof stays secure through the block hash of
  the digest.
- **Block time.** `closed_at` is never earlier than the previous block's (the
  builder clamps; `ledger_verify`, `ledger_verify_digest` and the proof chain walk
  check it).
- **Table identity in proofs.** The proof carries `table_id` (as before: the
  entry leaf and the row encoding cover it). New: the four-argument
  `ledger_verify_proof(proof, digest, expected_table_id, expected_row_hash)`,
  which rejects a proof for another table or row, and documentation that a verifier
  must recompute the row hash from the row it holds.
- Canonical integers in proofs: a JSON number with a fractional part, or out of
  range, is rejected, not rounded.

### What changed for each finding

- **L-01.** No query over a user table uses a bare alias any more: the whole row
  is `kl_r.*`. `ledger_verify` checks each table inside its own exception block; a
  table that cannot be checked is a `table_error` finding and the verification
  goes on.
- **L-02.** The hook rejects `CREATE FOREIGN TABLE` and `CREATE TABLE` with
  `INHERITS` or `PARTITION OF`, `ALTER TABLE INHERIT` and `ATTACH PARTITION` for
  a ledger table as parent or child; `ledger_verify` checks `pg_inherits`.
- **L-03.** The hook resolves a table name once, with the lock the statement will
  take (for the owner of the table), and decides on the OID. A second layer, new
  in this round and not in the original design: the event triggers
  `kingres_ledger_guard_*` (`ddl_command_end`, `sql_drop`, `table_rewrite`,
  `ENABLE ALWAYS`) check by OID what a statement did and roll it back.
- **L-04, L-09.** `ledger_enable` and every function that writes the ledger or
  scans the chain are not executable by PUBLIC; `ledger_entry`, `ledger_pending`
  and `ledger_tx` are not readable by PUBLIC. The README lists the grants of an
  administrator and of an auditor, and what enabling a table makes permanent.
- **L-05.** `ledger_enable` requires READ COMMITTED, locks the table by OID
  (`kingres.ledger_lock_table`, which checks ownership first) before reading
  anything, and registers the table last, after its triggers are armed.
- **L-06.** Functions and triggers of the extension are recognised by membership
  (`pg_depend`) and by C symbol (`prosrc`), in the hook and in `ledger_verify`.
  Renaming, altering, re-owning and granting on functions and event triggers of
  the extension is rejected; so is granting on a ledger catalog table anything but
  `SELECT`.
- **L-07.** The event trigger rejects any statement that drops a column of a ledger
  table, through any cascade. `ledger_enable` rejects stored generated columns
  as well as virtual ones (a stored one depends on an expression that a cascade
  or an `ALTER` can drop). `ADD COLUMN` passes the type checks of
  `ledger_enable` (`kingres.ledger_type_problem`, used by both).
- **L-08.** The block builder serialises on `LOCK TABLE kingres.ledger_chain IN
  EXCLUSIVE MODE`, which only the extension owner can take.
- **L-10.** `ledger_verify_digest` checks the range before converting and
  returns false for every input; an infinite `closed_at` in the chain no longer
  raises an error.
- **L-11, L-12.** See the README and `sql/ledger_review.sql`.

### Other findings of this round

- The owner of the extension must be a superuser: `ledger_enable` creates triggers
  on tables of other roles. It now says so, instead of failing on a permission
  error half way.
- `ledger_verify` reports a capture trigger on a table that is not registered
  (what a restored database looks like), and any child table.
- Logical replication into a ledger table works for inserts and stops with an
  error on a replicated UPDATE, DELETE or TRUNCATE; a hot standby verifies and
  builds proofs; a parallel query is safe because the functions that read tables
  are not marked parallel safe. These are tested (`t/005_environments.pl`).
- `REINDEX` of the ledger catalog tables changes nothing that the ledger covers.

### Known gaps in the protection

See the README, "What the protections cannot stop". The additions to the list
above: switching event triggers off (`event_triggers = off`), a cooperating
second role that moves a name under a statement that only the hook rejects
(`RENAME COLUMN`, `ALTER COLUMN TYPE` without rewrite; detected by
`ledger_verify`), and the replacement of a function body by catalog edits.

## Notes of version 0.2 (format documentation, hash format unchanged)

Added with version 0.2 after the independent verifier (`kingres/verifier`)
was written from the README alone and listed gaps in the description of format
version 1. The format did not change. The README of the extension is the
reference and now says, in the section "Hash format version 1": the export of
the block headers and where the chain identity comes from; that headers of
`chain` beyond the height of the digest are parsed but not used; the ranges of
the JSON integers, which members are required and how unknown and repeated
members are treated; the formats of `block_closed_at` and `generated_at` and
that an empty ledger has no digest; the edge cases of the row encoding
(dropped and added columns, an empty value against NULL, the encoding of
column names, limits); how to obtain the binary send bytes outside the server;
the checks a verifier must make that were only implied; the encoding of the
user name in a database that is not UTF-8 (the server's proof check converted
nothing before 0.2, which was wrong for such a database and is fixed); and the
rules that make a set of digests consistent. The operation `D` of version 0.2
(`ledger_entry.op`, byte `0x44`, and the `op` member of a proof) adds a value
to the entry leaf and changes no layout. The sizes above (entry leaf data 42
bytes, block transaction leaf data 47 bytes plus the name, block hash input
87 bytes) replace the stale ones in "Review of the hash formats".