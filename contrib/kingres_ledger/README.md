<!-- SPDX-License-Identifier: PostgreSQL -->
<!-- Portions Copyright (c) 2026, RooyeKhat Media Co -->
# kingres_ledger

Tamper-evident ledger tables for KingresDB (a thin fork of PostgreSQL 18.7).
This is the first increment of phase 6 of the roadmap; the design is in
[`kingres/docs/design/ledger-mvp.md`](../../kingres/docs/design/ledger-mvp.md),
including its section "Implementation notes and deviations". The findings of
the independent review of version 0.1 and what was done about them are in
[`kingres/docs/reviews/2026-11-14-kingres-ledger-0.1.md`](../../kingres/docs/reviews/2026-11-14-kingres-ledger-0.1.md)
and in the notes at the end of the design document.

A table put under ledger control with `kingres.ledger_enable` becomes
append-only. Every inserted row is hashed and recorded in a hash chain of
blocks. A digest of the latest block, stored outside the database, lets anyone
check later that the table and the chain have not been changed, and an
inclusion proof shows that one row is part of the ledger.

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
undetected. The window is the block interval plus the digest export interval.

The protections against switching the ledger off (below) stop mistakes and
ordinary privileged use. They are not a security boundary against someone who
edits the catalogs directly, switches event triggers off, removes the library
from the configuration, or changes files: the section "What the protections
cannot stop" lists every way that is known.

**Enabling the ledger on a table is permanent.** There is deliberately no
function that switches it off. Once a table is under ledger control:

- `DROP TABLE` of it is rejected, and so is `DROP SCHEMA ... CASCADE` of a
  schema that contains it, and `DROP OWNED BY` its owner;
- `DROP ROLE` of its owner fails until the table has been moved to another
  role with `REASSIGN OWNED BY old TO new` (`ALTER TABLE ... OWNER TO` works);
- rows of other tables cannot be deleted while a ledger row refers to them
  through a foreign key (the ledger rows can neither be deleted nor changed,
  so `ON DELETE CASCADE` and `ON DELETE SET NULL` fail), and `TRUNCATE` of
  such a table fails;
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

Tests: seven pg_regress tests and six TAP tests under `t/`.

| Test | What it covers |
|---|---|
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

| Function | Purpose | Executable by |
|---|---|---|
| `kingres.ledger_enable(regclass, text default 'append_only')` | Put an ordinary table under ledger control, for good. The caller must own the table. READ COMMITTED only. | granted roles |
| `kingres.ledger_row_hash(record) returns bytea` | Leaf hash of a row of a ledger table; the argument must be a row of the table itself, e.g. `ledger_row_hash(a.*) FROM accounts a` (not a sub-select). | PUBLIC |
| `kingres.ledger_build_block() returns bigint` | Close a block from the pending transactions; the new height, or NULL when nothing was pending. READ COMMITTED only. | granted roles |
| `kingres.ledger_generate_digest() returns jsonb` | Build a block if needed and return the digest of the latest block; NULL when the ledger is empty. | granted roles |
| `kingres.ledger_verify(from_height bigint default 0, to_height bigint default null)` | One row `(check_name, object, detail)` per problem. | granted roles |
| `kingres.ledger_verify_digest(jsonb) returns boolean` | Whether a digest matches the stored chain (the whole chain up to the digest height is recomputed). Never raises an error for a malformed digest. | granted roles |
| `kingres.ledger_inclusion_proof(regclass, bytea) returns jsonb` | Proof for the (first) entry with this row hash. The row must already be in a block. | granted roles |
| `kingres.ledger_verify_proof(jsonb, jsonb) returns boolean` | Check a proof against a trusted digest of the same chain; reads no table; malformed input gives false. | PUBLIC |
| `kingres.ledger_verify_proof(jsonb, jsonb, int, bytea) returns boolean` | The same, and the proof must be for this table id and this row hash (the ones the caller computed from the row it holds). | PUBLIC |
| `kingres.ledger_leaf_hash(bytea)`, `ledger_node_hash(bytea, bytea)`, `ledger_merkle_root(bytea)` (aggregate), `ledger_merkle_path(bytea[], int)`, `ledger_tx_leaf(...)`, `ledger_block_tx_leaf(...)`, `ledger_block_hash(...)`, `ledger_genesis_hash(bytea)`, `ledger_timestamptz_us(timestamptz)` | Primitives used by the SQL functions and useful for independent checks. | PUBLIC |
| `kingres.ledger_hook_active()` | True when the library was loaded through `shared_preload_libraries`. | PUBLIC |

Internal functions (`ledger_capture`, `ledger_record_row`, `ledger_lock_table`,
and the helpers `ledger_all_defects`, `ledger_table_defects`,
`ledger_catalog_defects`, `ledger_trigger_ok`, `ledger_type_problem`,
`ledger_is_member`, `ledger_guard`) are not part of the interface. The ones that
write the ledger are not executable by PUBLIC, and the protection hook rejects
any attempt to grant them.

Setting:

| GUC | Default | Meaning |
|---|---|---|
| `kingres_ledger.max_block_transactions` | 100000 | A block is closed with at most this many transactions; the rest wait for the next block. Superuser setting (`PGC_SUSET`). |

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
| `ledger_build_block`, `ledger_generate_digest` | write blocks and transactions |
| `ledger_verify` | reads every ledger table and every table under ledger control |
| `ledger_inclusion_proof` | reads the entries and transactions, which the caller may not read |
| `ledger_guard` (event trigger) | must see the ledger tables whatever grants an administrator changed, so that DDL of every role keeps working |

All of them have `SET search_path = pg_catalog, pg_temp`, and every reference to
an object of the extension is qualified with `kingres.`. The test
`ledger_privs` scans `pg_proc` and fails when a function breaks one of these
rules. The only functions without a `search_path` are C primitives that compute
on their arguments and look nothing up (the list is in the test).

**Default privileges.**

| Object | PUBLIC |
|---|---|
| `kingres.ledger_table`, `kingres.ledger_chain`, `kingres.ledger_blocks` | `SELECT` |
| `kingres.ledger_entry`, `kingres.ledger_pending`, `kingres.ledger_tx` | nothing |
| `ledger_enable`, `ledger_build_block`, `ledger_generate_digest`, `ledger_verify`, `ledger_verify_digest`, `ledger_inclusion_proof` | nothing |
| `ledger_row_hash`, `ledger_verify_proof`, the hash primitives, `ledger_hook_active` | `EXECUTE` |

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
                          kingres.ledger_generate_digest() TO ledger_agent;
```

**An auditor role** that verifies the ledger and builds proofs needs `EXECUTE`
only (the functions run as the owner):

```sql
GRANT EXECUTE ON FUNCTION kingres.ledger_verify(bigint, bigint),
                          kingres.ledger_verify_digest(jsonb),
                          kingres.ledger_inclusion_proof(regclass, bytea) TO auditor;
```

A role that also wants to read the entries, the pending transactions and the
transactions itself needs, in addition:

```sql
GRANT SELECT ON kingres.ledger_entry, kingres.ledger_pending, kingres.ledger_tx TO auditor;
```

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
- `ledger_build_block` takes a table lock on `kingres.ledger_chain` (EXCLUSIVE
  mode; only the extension owner can take it, so no other role can stop block
  building), reads the pending transactions (at most `max_block_transactions`,
  lowest transaction id first), computes each transaction's Merkle root,
  writes `ledger_tx` and `ledger_blocks`, and deletes the processed rows from
  `ledger_pending`. A transaction that commits after the builder's snapshot is
  picked up by the next block. Order inside a block is by transaction id, not
  by commit order.
- There is no background worker in this increment; blocks are built when
  `ledger_generate_digest()` or `ledger_build_block()` is called.
- Logical replication into a ledger table works for inserts: the apply worker
  runs with `session_replication_role = replica`, the `ENABLE ALWAYS` triggers
  fire, and the rows are captured as the owner of the subscription. A replicated
  `UPDATE`, `DELETE` or `TRUNCATE` is rejected by the append-only triggers, so
  the apply worker stops with an error until the subscription is changed.
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

`ledger_enable` rejects columns whose binary form is not stable or does not
exist: types without a binary send function, composite types and arrays of
composite types (the binary form contains type OIDs), arrays whose element type
is user-defined (`array_send` writes the element type OID) or itself not
supported, and generated columns. `ADD COLUMN` on a ledger table passes the same
checks (the event trigger applies them).

**Transaction tree.** The leaves of a transaction are its entries in `seq`
order: `leaf_hash(0x11 || u32 seq || u32 table_id || u8 op || row_hash)` with
`op = 0x49` ('I'). `merkle_root` of the transaction is `MTH` over these leaf
hashes (a transaction row in `ledger_tx` also stores `entry_count`).

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

**Chain identity.** `kingres.ledger_chain` has one row, written when the
extension is created: `chain_id`, 32 random bytes (`SHA-256` of two random
UUIDs). The `prev_hash` of block 0 is `SHA256(0x14 || chain_id)` instead of 32
zero bytes. `ledger_verify` checks that block 0 starts from the stored
`chain_id`; the digest and every proof carry `chain_id`, and
`ledger_verify_digest` and `ledger_verify_proof` reject a digest or proof of
another chain. The table is protected like the other ledger tables.

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

where `closed_at` is the timestamp in microseconds since 2000-01-01 00:00:00
UTC (the internal value of `timestamptz`; `kingres.ledger_timestamptz_us`
returns it), and `prev_hash` is `SHA256(0x14 || chain_id)` for height 0.

**Block time.** `closed_at` is the clock of the database server when the
block was built. It never goes back: the builder uses the `closed_at` of the
previous block when the clock is behind it, `ledger_verify` and
`ledger_verify_digest` check that no block is earlier than the one before, and
a proof's chain of headers is checked the same way. A clock that was set far
ahead keeps every later block at that time until real time passes it. The time
is evidence only relative to an external anchor (a digest that is stored with
a timestamp from a source you trust); inside the database it is the word of the
server.

**Digest.** A JSON object; `chain_id`, `height` and `block_hash` (lower case hex
of the bytes) are what verifiers use:

```json
{"database": "...", "chain_id": "3f1c...", "hash_version": 1, "height": 12,
 "block_hash": "e3b0...", "block_closed_at": "2026-11-14T10:20:30.123456Z",
 "generated_at": "2026-11-14T10:21:00.000000Z"}
```

**Inclusion proof.** A JSON object (hex is lower case; numbers are JSON
integers: a number with a fractional part, or outside the range, is rejected):

```json
{"format": 1, "chain_id": "3f1c...", "relation": "public.t", "table_id": 3,
 "ledger_txid": 1234, "seq": 1, "op": "I", "row_hash": "<hex>",
 "entry_index": 1, "entry_count": 5, "entry_path": ["<hex>", ...],
 "tx_root": "<hex>", "user_name": "alice", "tx_index": 2, "tx_path": ["<hex>", ...],
 "block": {"height": 7, "hash_version": 1, "prev_hash": "<hex>",
           "tx_merkle_root": "<hex>", "tx_count": 4,
           "closed_at_us": 816000000000000, "block_hash": "<hex>"},
 "chain": [{"height": 8, "hash_version": 1, "prev_hash": "<hex>",
            "tx_merkle_root": "<hex>", "tx_count": 1,
            "closed_at_us": 816000000000001, "block_hash": "<hex>"}, ...]}
```

`entry_index` is the position of the entry among the entries of its
transaction in `seq` order (not the value of `seq`, which may have gaps);
`tx_index` is the position of the transaction in its block; `chain` lists the
headers of the blocks after the proof's block up to the latest block at the
time the proof was made. `relation` is informational. `table_id` is a local
number that is part of the row encoding and of the entry leaf, so the proof
binds the table: a proof for a row of one table does not verify as a proof for
the same bytes in another table.

**What a proof proves, and what the verifier must do.** A proof says that an
entry with this `row_hash` and this `table_id` is in the ledger. It cannot know
the row. To learn that a row you hold is in the ledger, **recompute the row
hash from that row** (encode it as above with the `table_id` of the proof, or
call `ledger_row_hash` on a server that has the table) and compare it with the
`row_hash` of the proof; the four-argument `ledger_verify_proof` does that
comparison for the caller. An independent verifier, given a trusted digest
`(C, H, D)` (chain identity, height, block hash):

0. checks that the `chain_id` of the proof is `C`, and, for the row it holds,
   that `row_hash` is the hash it computed and `table_id` is the table it
   means;
1. computes `e = leaf_hash(0x11 || u32 seq || u32 table_id || u8 op || row_hash)`
   and checks that the audit path `entry_path` leads from `e`, at index
   `entry_index` in a tree of `entry_count` leaves, to `tx_root`;
2. computes `t = leaf_hash(0x12 || u64 ledger_txid || u32 entry_count || tx_root ||
   u16 n || user_name)` (`user_name` from the proof, UTF-8, `n` its length) and
   checks that `tx_path` leads from `t`, at index `tx_index` in a tree of
   `block.tx_count` leaves, to `block.tx_merkle_root`;
3. recomputes the block hash from the header fields of `block` (it must equal
   `block.block_hash`; for height 0 the `prev_hash` must be `SHA256(0x14 || C)`);
4. if `block.height == H`, checks that this hash equals `D`; if `H` is greater,
   walks `chain`: the element for height `h + 1` must have `prev_hash` equal to
   the hash of the block at `h`, a `closed_at_us` not smaller than that of the
   block at `h`, and its own hash is recomputed the same way, until height `H`,
   whose recomputed hash must equal `D`; if `H` is smaller than `block.height`
   the digest is too old to prove anything about this block and the check
   fails.

The tree sizes (`entry_count`, `block.tx_count`) are authenticated by the
transaction leaf and by the block hash respectively; an audit path alone does
not authenticate the size of its tree. A proof for a row that is in a block
older than the digest needs the chain of headers in between, so its size is
linear in the number of blocks between them.

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
installed and nothing while the extension's own install script runs. It always
calls the previous hook.

A table is "under ledger control" for the hook when it is registered in
`kingres.ledger_table` or its capture trigger is `ENABLE ALWAYS` (`ledger_enable`
arms that trigger before it registers the table).

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
- Anything that reads: the ledger protects the integrity of the data, not its
  confidentiality, and the row hashes of an entry reveal a row with little
  entropy to somebody who may read them (hence the privileges).

## Limitations of this increment

- Only `append_only` mode; no updatable tables, no client or server
  signatures, no external anchoring, no per-shard aggregate digest.
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
