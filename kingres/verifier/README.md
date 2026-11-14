<!-- SPDX-License-Identifier: PostgreSQL -->
<!-- Portions Copyright (c) 2026, RooyeKhat Media Co -->
# kingres-verify

`kingres-verify` is a standalone command-line verifier for the tamper-evident
ledger of KingresDB (`contrib/kingres_ledger`). It checks ledger digests,
inclusion proofs and exported block headers **without trusting the database**:
an auditor runs it on their own machine with files exported earlier. It is an
independent second implementation of hash format version 1, written from the
documentation (`contrib/kingres_ledger/README.md` and
`kingres/docs/design/ledger-mvp.md`) alone, and it is checked against fixtures
exported from a real server (see "Cross-check against the server").

It is standard library Go only, no third-party modules, so that an auditor can
read all of it (about 2,000 lines outside the tests) and build it with one
command.

## What it can and cannot prove

It verifies **what is in the files you give it**, nothing else.

- The **digest must have been obtained and kept outside the database's
  control** (a different machine, an append-only store, a notary, a printout).
  A digest that was read from the database you are checking proves nothing: an
  attacker who rewrites the whole chain also rewrites the digest. Everything
  `kingres-verify` says is relative to the digest you hold.
- A proof shows that **an entry with this `row_hash` and `table_id` is in the
  ledger**. It cannot know the row. To learn that a row **you hold** is in the
  ledger, **recompute its row hash from data you hold** (`row-hash`) and give it
  to `proof --row-hash` together with `--table-id`. Without those two flags the
  tool says so in its output.
- It does not prove that a row was not later changed *in the table* (the ledger
  is append-only; compare current table contents with the hashes yourself), that
  no other rows exist, or anything about transactions that are not yet in a
  block covered by a digest (the window described in the ledger README).
- Block times are the database server's clock; they are evidence only relative
  to an external anchor of a digest with a trusted timestamp.
- `digest-chain` compares digests only; `blocks` checks exported headers only.
  Their output says what they do not prove.

## Installation

```
cd kingres/verifier
go build -o kingres-verify ./cmd/kingres-verify      # Linux, macOS
go build -o kingres-verify.exe ./cmd/kingres-verify  # Windows
```

Go 1.27.3 or later. `go test ./...` needs no database (the fixtures are
committed). `go.mod` has no `require` line: only the standard library is used.

## Exit statuses

| Status | Meaning |
|---|---|
| 0 | everything that was asked for was verified (`VERIFIED` or `CONSISTENT` is printed) |
| 1 | a verification failed (the failed step is named); the word `VERIFIED` is never printed |
| 2 | usage or input error: a flag is missing or wrong, a file cannot be read, or the trusted input (digest, columns file, block export when it cannot be parsed as an export) is malformed; nothing was verified |

A malformed **proof** is a failed verification (status 1, step `parse`): the
proof is the thing under test. A malformed **digest** is an input error
(status 2): the trusted anchor is unusable. `--json` prints one JSON object
(`command`, `verified`, `steps`, `failed_step`, `error`, `not_proved`, `info`)
instead of text; the exit status is the same.

## Subcommands

### `proof`

```
kingres-verify proof --proof proof.json --digest digest.json [--table-id N] [--row-hash HEX] [--op I|D|any] [--json]
```

Runs the verification steps of the ledger README on the proof against the
trusted digest: the chain id (step `chain-id`; `binding` first when
`--table-id`/`--row-hash` are given), the entry leaf and its audit path to the
transaction root (`entry-path`), the transaction leaf and its audit path to the
block's transaction root (`tx-path`), the block hash recomputed from the header
(`block-hash`, and for block 0 the genesis `prev_hash`), then the chain of later
headers up to the digest height with linkage, recomputed hashes and
non-decreasing block times, ending at the digest's block hash (`chain-walk`). If
the digest carries `block_closed_at` it must equal the block time of the digest's
block (`digest-time`). A digest older than the proof's block fails at
`chain-walk`. Numbers must be plain JSON integers in range; hex must be lower case.

**Insert and removal proofs.** A proof is for one ledger entry, and an entry has an
operation: `I` (a version of a row was inserted) or `D` (a version of a row was
removed, by an `UPDATE` or a `DELETE` of an updatable table). They are different
statements: "this version was inserted" does not say that it is still there, and
"this version was removed" does not say who made the row. The verifier accepts
proofs of either operation, because the entry leaf covers the operation byte and a
proof whose `op` was changed fails at `entry-path`. `--op I` or `--op D` makes the
expected operation the caller's explicit choice: a proof of the other operation
fails at step `binding`. The default, `--op any`, accepts either and **reports
which one was proven** (in the text output as `operation D: ...`, in `--json` as
`info.op` and `info.proves`). Use `--op` whenever you mean one of the two. Any
other value of `op` in the proof is a parse failure.

```
$ kingres-verify proof --proof proofs/acc5.json --digest digests/d5.json \
    --table-id 1 --row-hash 56700cbe26b660e69f01339a5629217dde30b035480f68f9c7873caca086d856 --op I
VERIFIED: the proof is valid against the digest at height 9.
  ok  binding      proof is for the expected table, row and operation
  ...
$ echo $?
0
```

### `row-hash`

```
kingres-verify row-hash --table-id N --columns columns.json [--encoding] [--json]
```

Computes `row_hash = leaf_hash(canonical row encoding)` and prints it as 64
lower case hex characters (`--encoding` also prints the encoding). The table id
is the `table_id` of the proof (or `SELECT table_id FROM kingres.ledger_table WHERE relid = 'app.accounts'::regclass`).

**Columns file.** A JSON array with one object per **non-NULL** column, **in
column order** (attribute order of the table, dropped columns omitted). Each
object has exactly two members:

- `name`: the column name, a JSON string (UTF-8; it is encoded as UTF-8 bytes);
- `send_hex`: the bytes of the value's **binary send form** (the output of the
  type's `typsend` function, without the protocol's length header), as lower
  case hex. A zero-length value (an empty string, an empty `bytea`) is `""`; a
  NULL column is **left out of the array**, it is not an empty string.

```json
[
  {"name": "id",    "send_hex": "00000001"},
  {"name": "owner", "send_hex": "616c696365"},
  {"name": "note",  "send_hex": ""}
]
```

Unknown members, duplicate keys, upper case or odd-length hex are rejected
(status 2). Text-like send functions convert to the client encoding, so
obtain the bytes with `SET client_encoding = 'UTF8'`. For example, for
`app.accounts(id int, owner text, note text)`:

```sql
SET client_encoding = 'UTF8';
SELECT encode(int4send(id), 'hex'), encode(textsend(owner), 'hex'), encode(textsend(note), 'hex')
FROM app.accounts WHERE id = 1;
```

`scripts/gen-fixtures.sh` contains a generic helper (`app.fx_cols`) that builds
the whole file for any table from `pg_type.typsend`. An auditor who does not
hold the database can build the file from their own source data, using the send
format of each type (for example `int4send` is 4 bytes big-endian, `textsend` is
the UTF-8 bytes, `boolsend` is one byte, `timestamptzsend` is the 8-byte
microseconds since 2000-01-01 UTC).

### `digest-chain`

```
kingres-verify digest-chain --digests DIR|FILE [--digests FILE ...] [FILE ...] [--json]
```

Takes several digests of the same chain exported at different times (a
directory stands for its `*.json` files) and checks that they are mutually
consistent as far as digests alone allow: the same `chain_id`; sorted by height,
block times do not go back; equal heights have equal block hashes (and equal
`block_closed_at`); and, when every digest has `generated_at`, heights do not
decrease in generation order (otherwise the database went back to an earlier
state). At least two digests are required. Exit status 1 lists every problem.

It does **not** prove that the blocks between the digests are intact, that any
row is in the ledger, or that any digest is authentic; `generated_at` is not
authenticated. Use `proof` or `blocks` for that.

### `blocks`

```
kingres-verify blocks --blocks blocks.jsonl (--digest digest.json | --chain-id HEX) [--json]
```

Takes an export of the block headers, one JSON object per line, and recomputes
every block hash from the genesis hash `SHA-256(0x14 || chain_id)`: the file
must start at height 0, heights must be dense, each `prev_hash` must be the
previous block's hash, and block times must not go back. With `--digest` it also
checks that the chain reaches the digest (block at the digest height has the
digest's hash and `block_closed_at`). The chain id comes from the digest or from
`--chain-id` (the export has no chain id; without either, status 2).

Each line has the header fields: `height`, `hash_version`, `prev_hash`,
`tx_merkle_root`, `tx_count`, `closed_at_us` (microseconds since 2000-01-01 UTC,
an integer), `block_hash` (hex lower case). Export:

```
psql -X -At -c "SELECT json_build_object('height', height, 'hash_version', hash_version,
    'prev_hash', encode(prev_hash, 'hex'), 'tx_merkle_root', encode(tx_merkle_root, 'hex'),
    'tx_count', tx_count, 'closed_at_us', kingres.ledger_timestamptz_us(closed_at),
    'block_hash', encode(block_hash, 'hex'))
  FROM kingres.ledger_blocks ORDER BY height" > blocks.jsonl
```

`kingres.ledger_blocks` is readable by PUBLIC. This verifies the headers only: it
says nothing about transactions or rows.

### `version`, `--help`

`kingres-verify version`, `kingres-verify --help`, `kingres-verify <command> --help`
(all exit with status 0).

## Exporting from a database

Run these where the database is, then copy the files to the auditor's machine
(the digest must go to a place the database's operators cannot change):

```
# digest (builds a block first if transactions are pending); store it OUTSIDE the database
psql -X -At -c "SELECT kingres.ledger_generate_digest()" > digest-$(date -u +%FT%TZ).json

# inclusion proof for one row (the row hash is computed by the server here;
# the auditor recomputes it with row-hash from data they hold)
psql -X -At -c "SELECT kingres.ledger_inclusion_proof('app.accounts', kingres.ledger_row_hash(a.*)) FROM app.accounts a WHERE id = 1" > proof.json
```

`ledger_generate_digest` and `ledger_inclusion_proof` need `EXECUTE` granted
(see the ledger README, "Privileges"). The server's OS user is called `kingres`,
like the extension's schema, so set `search_path` explicitly or qualify tables.
A proof made at time T contains the headers up to the latest block at T; verify
it against a digest of block T or later.

## Cross-check against the server

`scripts/gen-fixtures.sh` starts a throwaway server from the installed binaries
in WSL2 (new directory under `$KINGRES_TEST_WORK/verifier-test/`, default `~/kw-verifier`, a free port from
57300 to 57399, `shared_preload_libraries = 'kingres_ledger'`), creates two
ledger tables with many column types and a third, updatable table (integers, numeric, text with non-ASCII
characters and a non-ASCII column name, timestamptz, timestamp, date, bytea,
boolean, uuid, float, jsonb, interval, inet, an array, NULL columns, an empty
string, an added nullable column), inserts rows over several transactions (one
by another role, one rolled back) and ten blocks (the updatable table gets inserts, updates of the same row
in two transactions, an update that changes several columns and a delete), and
writes `testdata/fixtures/`: nine digests at different heights, one digest of a
second, separately created database, the proofs of 15 rows and of 4 current rows
of the updatable table (`I`) and of 4 removed versions of rows, each as the `D`
proof of its removal and as the `I` proof of its entry, the column descriptions of
all of them in binary send form (for a removed version, the columns of its row in
the history table), the block headers and a manifest of the server's own answers.
Then it stops and removes the server. Regenerate with:

```
wsl.exe -d Ubuntu-24.04 -u kingres -- bash "/mnt/c/<path to the repository>/kingres/verifier/scripts/gen-fixtures.sh"
```

`go test ./internal/crosscheck/` then shows that every server proof verifies
against its digest and every later digest, fails against earlier digests and
against the other database's digest, that every `D` proof verifies and says that it is a `D` proof, that a `D` proof presented as `I` (by `--op I`, or by changing its `op` member) fails and the reverse, that every recomputed row hash equals the
server's `ledger_row_hash`, that the recomputed block chain equals the server's
digests and the headers inside the proofs, and (in `internal/proof`) that every
single-field and every single-byte mutation of a fixture proof is rejected.

Fuzz targets (kept): `FuzzParse` (jsonx, rowfile), `FuzzDecodeHex`, `FuzzEncodeRow`
(format), `FuzzParseDigest`, `FuzzParseBlocks` (chain), `FuzzVerify` (proof).
Run one with `go test ./internal/proof -run '^$' -fuzz '^FuzzVerify$' -fuzztime 30s`.

## Layout

| Package | Purpose |
|---|---|
| `internal/merkle` | RFC 6962 hashes, tree root, RFC 9162 audit path verification |
| `internal/format` | every hash input, one function each, with the domain tag as a named constant and the README text quoted; strict hex |
| `internal/jsonx` | strict JSON (duplicate keys, invalid UTF-8, lone surrogates, plain integers in range) with error paths |
| `internal/chain` | digests, block headers, the block chain, digest sets |
| `internal/proof` | proof parsing and the verification steps |
| `internal/rowfile` | the columns file of `row-hash` |
| `internal/cli` | the command line (`cmd/kingres-verify` is a thin `main`) |

## Decisions where the documentation is silent

These readings were made and are enforced; each is a finding below.

- Required proof members: all of the README example except `relation`
  (informational, optional). Unknown members are ignored in proofs and digests
  (but rejected in the columns file). Duplicate keys are always rejected.
- Required digest members: `chain_id`, `hash_version` (must be 1), `height`,
  `block_hash`. `database`, `block_closed_at`, `generated_at` are optional but
  checked when present (`block_closed_at` must equal the block time of the digest's block).
- Integer ranges: `table_id`, `seq`, `entry_index`, `entry_count`, `tx_index` and
  `tx_count` are PostgreSQL `int` (0 to 2147483647; counts at least 1);
  `ledger_txid` and `height` are `bigint` (0 to 9223372036854775807);
  `closed_at_us` is any `int64`; `hash_version` and `format` must be 1.
- Headers in `chain` after the digest height are parsed (strictly) but not
  verified, because a proof made after the digest legitimately has them.
- `op` must be the string `"I"` (byte `0x49`, an entry that inserted a row version) or `"D"`
  (byte `0x44`, an entry that removed one); any other string is rejected. A proof
  that says `D` but whose entry is an `I` (or the reverse) fails at `entry-path`.

## Findings about the format documentation

The ledger README and the design note were enough to write a working verifier:
the first run of the cross-check against the real server matched on every row
hash, every block hash and every proof, and **no source of the extension was read**
(the cross-check resolved nothing that needed it). The findings below are
ambiguities, omissions and one stale passage, most important first. Each gives
the wording to add.

1. **The block header export is not documented, and a header export does not
   carry the chain id.** The README documents the header fields only inside the
   proof (`block`, `chain`), never as an export of `kingres.ledger_blocks`, and
   the column names appear only in the design note. A verifier that recomputes
   the chain from the genesis hash needs the chain id, which is not in
   `ledger_blocks`. Add to the README, section "Chain identity": *"To check the
   whole chain independently, export `kingres.ledger_chain.chain_id` (it is also
   in every digest and proof) and the headers: `SELECT height, hash_version,
   prev_hash, tx_merkle_root, tx_count, kingres.ledger_timestamptz_us(closed_at)
   AS closed_at_us, block_hash FROM kingres.ledger_blocks ORDER BY height`. The
   chain is valid when block 0 has `prev_hash = SHA256(0x14 || chain_id)`, every
   block hash is recomputed as in "Block hash", every `prev_hash` equals the hash
   of the previous block, heights are 0, 1, 2, ... without gaps and
   `closed_at_us` never decreases."*

2. **Step 4 does not say what to do with `chain` elements beyond the digest
   height, or with a non-empty `chain` when `block.height == H`.** A proof made
   after the digest was taken has headers beyond `H`. Add to step 4: *"Elements of
   `chain` with a height greater than `H` are not used and need not be checked;
   a proof is valid against any digest whose height is at least the proof's block
   height and at most the height of the last element of `chain`. When
   `block.height == H`, `chain` is ignored."* (Consequence, implemented: a
   mutation of an element beyond `H` is not detected by that digest.)

3. **JSON integer ranges are not given.** "A number with a fractional part, or
   outside the range, is rejected" does not say which range, and the hash input
   widths (u32, u64) are wider than the database types. Add: *"`table_id`, `seq`,
   `entry_index`, `entry_count`, `tx_index`, `tx_count` are 32-bit signed
   integers (0 to 2147483647; `entry_count` and `tx_count` at least 1);
   `ledger_txid` and `height` are 64-bit signed integers (0 to
   9223372036854775807); `closed_at_us` is a 64-bit signed integer. A number
   written with a fraction (`1.0`), an exponent (`1e2`), a leading zero, a plus
   sign, or `-0` is rejected."* The same strictness should be stated for digests
   (the README only says it for proofs).

4. **Which proof and digest members are required, and whether unknown or
   duplicate members are allowed, is not stated.** Add: *"All members shown in
   the example are required, except `relation`, which is informational. A digest
   must contain `chain_id`, `hash_version`, `height` and `block_hash`; `database`,
   `block_closed_at` and `generated_at` are informational. A verifier must reject
   an object that has the same key twice. Unknown members are ignored. `format`
   and `hash_version` must be 1."* The README says `op` is `"I"` in the proof JSON
   and `0x49` in the leaf; add *"the JSON value `"I"` is the byte `0x49`"* to
   "Transaction tree".

5. **Digest timestamps: format and meaning.** `block_closed_at` and
   `generated_at` are shown only by example. Add: *"`block_closed_at` is the
   `closed_at` of the digest's block and `generated_at` the server time when the
   digest was made, both RFC 3339 timestamps in UTC with microsecond precision.
   `block_closed_at` must equal `closed_at_us` of block `height`. `generated_at`
   and `database` are not authenticated."* Also say that `ledger_generate_digest()`
   returns NULL for an empty ledger, so there is no digest (and no proof) for it.

6. **Row encoding: what "column order" and "name" mean in edge cases.** The
   README says "attribute order (attnum ascending)" in one place and "column
   order" in the design note. Add: *"Columns are taken in `attnum` order; dropped
   columns are skipped and do not renumber anything; a column added later comes
   last. The name is the column's name as stored, converted to UTF-8 in the same
   way as text values. A value of zero length (an empty string, an empty `bytea`)
   is not NULL and is encoded with `u32 length = 0`. At most 65535 columns and
   names of at most 65535 bytes are encoded."* (Both hold in the cross-check: an
   empty `bytea` and an empty `text` are in the fixtures.)

7. **How to obtain the send bytes of a row outside the server.** The README says
   "compute the row hash from that row" but gives no per-type list or procedure.
   Add a paragraph: *"The bytes are those of `typsend`: for example `int4send`
   is 4 bytes big-endian, `boolsend` one byte, `textsend` the UTF-8 bytes,
   `timestamptzsend` the int64 microseconds since 2000-01-01 UTC, `numeric_send`
   the PostgreSQL binary numeric. To obtain them in SQL: `SET client_encoding =
   'UTF8'; SELECT encode(<type>send(col), 'hex')`. Types whose send form depends
   on the server version are listed in `ledger_type_problem`."*

8. **Verification step list omits some checks that an implementation needs.**
   Add to the numbered steps: *"Every header (the proof's `block` and each
   element of `chain` that is used) must have `hash_version` 1. The tree size
   must be at least 1 and the index smaller than the size. An audit path longer
   than 64 hashes is invalid. `user_name` is at most 255 bytes of valid UTF-8."*
   The README says the sizes are authenticated by the transaction leaf and the
   block hash but never says a verifier must reject an index outside the tree.
   (Observed in this implementation's tests: an audit path built for a tree of
   `n` leaves also verifies as a path in a tree of `n+1` leaves for many indexes,
   which is why the authenticated sizes matter.)

9. **Stale numbers in the design note.** "Review of the hash formats" says "the
   transaction leaf (41 bytes) and the block leaf (44 bytes)". With the tag
   byte the entry leaf data is 42 bytes, and the block transaction leaf is
   `1 + 8 + 4 + 32 + 2 + n = 47 + n` bytes. The section above the "fix round"
   notes still contradicts them. Replace with *"the entry leaf data (42 bytes)
   and the block transaction leaf data (47 bytes plus the user name)"*. The block
   hash input is 87 bytes (`1 + 2 + 8 + 32 + 32 + 4 + 8`); stating it would help
   implementers check their buffer.

10. **`user_name` and database encoding.** The README says the name is UTF-8 but
    `name` values are stored in the database encoding. Add: *"For a database whose
    encoding is not UTF-8 the name is converted to UTF-8 (like text values); for
    `SQL_ASCII` the stored bytes are used."* Also state that names longer than
    255 bytes cannot occur (`name` is at most 63 bytes) so that no truncation rule is needed.

11. **Time ordering across a digest set.** The README defines "never goes back"
    for blocks only. Add: *"Digests of one chain taken at different times satisfy:
    same `chain_id`; if `height` is equal then `block_hash` is equal; if `height`
    is larger then `block_closed_at` is not smaller."* (This is what
    `digest-chain` checks.)

12. **Minor.** The README example shows `"closed_at_us": 816000000000000` for a
    block whose `block_closed_at` would be 2025; use consistent example values so
    that an implementer can test the conversion. The README never says that proof
    JSON is produced by `jsonb`, so key order and spacing (`"op": "I"` with a
    space) are not significant; say so: *"Key order and white space are not
    significant."*
