<!-- SPDX-License-Identifier: AGPL-3.0-only -->
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
read all of it (about 4,000 lines outside the tests) and build it with one
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
| 2 | usage or input error: a flag is missing or wrong, a file cannot be read, or the trusted input (digest, columns file, trust file, block export when it cannot be parsed as an export) is malformed; nothing was verified |
| 3 | incomplete: nothing failed, but something that was asked for could not be checked with the inputs and flags given (today: a tile of a log is missing from the directory); `INCOMPLETE` is printed and the missing items are listed; `VERIFIED` is never printed. Only the commands of phase 7 (`checkpoint`, `consistency`) can end with 3; a failure always wins over an incomplete check |

A malformed **proof** is a failed verification (status 1, step `parse`): the
proof is the thing under test. A malformed **digest** is an input error
(status 2): the trusted anchor is unusable. `--json` prints one JSON object
(`command`, `verified`, `steps`, `failed_step`, `error`, `not_proved`, `info`, and for status 3
`incomplete` and `missing`)
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

### `checkpoint`, `consistency`, `trust` (phase 7, step 1)

These commands check signed checkpoints of tile logs, the format of
`kingres/docs/design/ledger-phase7.md` (sections 3, 4 and 7). They need a trust
file that **you** made from a key you obtained outside the cluster. They are
built from the public C2SP texts `signed-note`, `tlog-checkpoint` and
`tlog-tiles` and from RFC 6962 and RFC 9162, with Ed25519 only.

```
kingres-verify trust init --origin-prefix P (--public-key B64 | --public-key-file F) --fingerprint HEX [--name LABEL] [--out FILE|-]
kingres-verify trust show --trust trust.json [--json]
kingres-verify checkpoint --trust trust.json --log chain:<id>[:e<N>]|aggregate:<id> [--source DIR | --note FILE] [--min-size N] [--now TIME] [--json]
kingres-verify consistency --trust trust.json --log <as above> --old NOTEFILE|SIZE:ROOT [--new NOTEFILE] [--source DIR] [--now TIME] [--json]
```

**`trust init`** refuses to run without `--fingerprint` (the SHA-256 of the
public key in hex; upper case and colons are accepted) and refuses a key that
does not match it, so a mistyped or swapped key is noticed. It never overwrites
a file. **`trust show`** validates and prints a file. `trust follow` (rotation
events of the publication) is not built yet and exits with status 2.

**`checkpoint`** verifies one checkpoint note: the note is well formed, at
least one signature is by a key of the trust file, no signature of such a key
is wrong, the origin is the origin of the named log below the trusted prefix,
the size and the root are well formed (`--min-size` fails a smaller tree). It
reads `<source>/c/<id>/e<N>/checkpoint` (aggregate: `<source>/a/<id>/checkpoint`)
or `--note`. Without `:e<N>` the epoch is taken from the source only when it
holds exactly one epoch of that chain; with `--note` the epoch must be given.
It checks no tile. A key whose `not_before` is later than `--now` (default the
system clock, printed) is not used. `not_after` is reported, not enforced: a
checkpoint has no time, and the design says a retired key's checkpoints stay
valid (section 3.3).

**`consistency`** checks that the log at the new checkpoint (`--new`, default
the checkpoint in `--source`) extends the log at the old one (`--old`: a note
file, verified the same way, or `SIZE:ROOT` with the root in base64, taken as
you give it). It reads the proof hashes from the tiles under
`<source>/c/<id>/e<N>/tile/...` (full and partial tiles; a partial tile that
is missing is read from the full tile), and verifies them against both roots with
RFC 9162 section 2.1.4.2. A new size smaller than the old one fails (the log
went back). Missing tiles give status 3; a tile of the wrong size or with
wrong content gives status 1. A source is a directory (a tar file and an HTTP
source are not built yet).

Formats, with the specification they follow:

- **Signed note** (signed-note, "Format"): UTF-8 without control characters
  except newline; the text ends in a newline; the last blank line separates it
  from one or more lines `U+2014 SP name SP base64(4-byte key id || signature)`.
  Key names are non-empty without Unicode spaces or `+`. Base64 must be
  canonical (RFC 4648 3.5), which is checked by re-encoding. Two lines with the
  same name and key id are rejected (tlog-checkpoint). At most 32 signatures
  and 1 MiB. Key id = `SHA-256(name || 0x0A || 0x01 || public key)[:4]`.
  A line whose name and key id match no trusted key is ignored; a line that
  matches one must verify, or the whole note is rejected (the specification
  says SHOULD); no verified line means rejection.
- **Checkpoint** (tlog-checkpoint): at least three non-empty lines, origin,
  size as a canonical decimal below 2^62, root as canonical base64 of 32 bytes;
  further lines are extension lines, kept and reported but without meaning here.
  The origin follows tlog-cosignature: at most 255 bytes, no spaces, `+` or
  control characters. A size 0 checkpoint must carry the hash of the empty
  string. The key name of a log's signature is its origin
  (`<prefix>/c/<chain_id>/e<epoch>`, `<prefix>/a/<aggregate_id>`).
- **Tiles** (tlog-tiles): height 8, hashes of 32 bytes, a full tile is 8192
  bytes; path `tile/<L>/<N>[.p/<W>]` with L from 0 to 63 and N in three digit
  groups, every group but the last prefixed `x` (`x001/x234/067`); a partial
  tile has 1 to 255 hashes; the width at level l for size s is
  `floor(s / 256^l) mod 256`. Paths are parsed canonically (no leading zeros,
  no `..`); a tile of any other length is rejected. Entry bundles
  (`tile/entries/<N>`, big-endian `u16` length prefixes) can be parsed and are
  not used yet.
- **Trust file** (design 7.2): strict JSON; `format` 1, `origin_prefix`, `keys`
  (`name` label, `public_key` base64, `fingerprint` that must equal the
  SHA-256 of the key, `not_before`, `not_after` as RFC 3339 UTC or null),
  optional `tsa_roots` (PEM certificates, validated), `client_keys` (`key_id`
  must equal the first 16 bytes of the SHA-256 of the key), and `witnesses`,
  which must be empty. Unknown members are rejected.

Not built yet (later steps of the design): `trust follow` and rotation events
(`key-added`, `key-retired`, `key-revoked`), tar and HTTP sources, `proof
--bundle`, `aggregate`, `anchors`, `attestation`, `bundle`, `monitor`,
`release verify`. The text of this section is the contract of step 1; where
the design and the specification differ, the specification was followed (see
"Findings about the phase 7 design").

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
(format), `FuzzParseDigest`, `FuzzParseBlocks` (chain), `FuzzVerify` (proof), and for phase 7 `FuzzParse` (signednote, trust), `FuzzVerify` (checkpoint), `FuzzParseTilePath` and `FuzzParseTile` (tlog), `FuzzParseLogSpec` (cli). `scripts/fuzz.sh [seconds]` runs every target for a short bounded time; the seeds (the published vectors) also run in `go test ./...`.
Run one with `go test ./internal/proof -run '^$' -fuzz '^FuzzVerify$' -fuzztime 30s`.

## Layout

| Package | Purpose |
|---|---|
| `internal/merkle` | RFC 6962 hashes, tree root, RFC 9162 audit path and consistency proof verification |
| `internal/format` | every hash input, one function each, with the domain tag as a named constant and the README text quoted; strict hex |
| `internal/jsonx` | strict JSON (duplicate keys, invalid UTF-8, lone surrogates, plain integers in range) with error paths |
| `internal/chain` | digests, block headers, the block chain, digest sets |
| `internal/proof` | proof parsing and the verification steps |
| `internal/signednote` | signed notes (C2SP signed-note), Ed25519, key ids, verification against trusted keys |
| `internal/tlog` | tile paths, tile and entry bundle parsing, tree hashes read from tiles, inclusion and consistency proofs |
| `internal/checkpoint` | tlog-checkpoint parsing and verification of a signed checkpoint |
| `internal/trust` | the trust file: parsing, validation, writing, keys per log origin |
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

## Findings about the phase 7 design (step 1)

Checked against the public text of C2SP `signed-note`, `tlog-checkpoint`,
`tlog-cosignature` (for the definition of an origin), `tlog-tiles`, RFC 6962
section 2.1 and RFC 9162 sections 2.1.1 to 2.1.4. Where the design and a
specification differ, the specification was followed. The test vector of the
signed-note specification (example key and note) is used; the checkpoint
example of tlog-checkpoint has no published key, so only its text is parsed.

1. **Consistency path length.** Design 4.10 says "at most `2*ceil(log2 n)`
   hashes"; RFC 9162 2.1.4.1 says "bounded above by `ceil(log2(n)) + 1`". The
   verifier accepts at most 65 hashes.
2. **Extension lines.** Design 3.2.4 says a checkpoint has three non-empty
   lines and no extension lines; tlog-checkpoint allows optional non-empty
   extension lines after the third. The verifier accepts and reports them
   (the design's own checkpoints have none).
3. **Origin rules.** The design states key-name rules only. tlog-checkpoint
   takes the origin from tlog-cosignature: at most 255 bytes, no Unicode spaces,
   `+` or control characters. Enforced; the trust file limits `origin_prefix`
   to 160 bytes so that `/c/<64 hex>/e<digits>` fits.
4. **Key names.** Design 7.2 says a key "whose name is `origin` or
   `origin_prefix`" is tried, but the `name` of a trust file key (`"2026"`) is
   a label. Implemented: the note key name of every signature is the log origin
   (3.2.4), which must be the prefix or below it; each pinned public key is
   tried under that name. The cluster-level name `<prefix>` is accepted for the
   origin of a later step (events).
5. **Validity windows against rotation.** Design 7.4 applies `not_before` and
   `not_after` to `--now`, but 3.3 says a retired key's checkpoints stay valid
   for ever, and a checkpoint carries no time. `not_before` is applied to
   `--now`; `not_after` is reported, not enforced.
6. **Exit status 2 for the note.** Design 7.4 lists a malformed "checkpoint
   note given as the anchor of trust" under status 2. For `checkpoint` the note
   is the thing under test, so a malformed note is status 1 (step `parse`), like
   a malformed proof. For `consistency` the `--old` note is the anchor: unreadable
   or not a note is 2, a bad signature or origin is 1.
7. **Output order.** Design 7.4 says the final line is `VERIFIED`, `FAILED` or
   `INCOMPLETE`; the existing commands print the headline first and the steps
   after. The new commands do the same as the existing ones.
8. **Duplicate signature lines.** The signed-note text does not forbid two
   lines with the same name and key id; tlog-checkpoint does. Rejected for every
   note, as design 7.6 (3) lists.
9. **A forged line from a known key.** signed-note says SHOULD reject; the
   verifier always rejects. A line of an unknown key is ignored.
10. **Epoch.** Design 7.4 shows `:e<N>` as optional. Without it the epoch is
    taken from the source only if it holds exactly one epoch; a checkpoint file
    needs it given.
11. **Partial tiles.** Design 4.4 deletes a partial tile 24 hours after its
    full tile; tlog-tiles allows clients to fall back to the full tile, which
    the reader does (the first `W` hashes).
12. **Sizes are not authenticated by a proof.** A path that verifies for size
    `n` can also verify for `n + 1` for the same root (observed in the tests),
    so sizes are only meaningful through a signed checkpoint, which binds size
    and root. Every command takes sizes from checkpoints.
13. **Sources.** Design 4.6 and 7.4 let the verifier read a tar file or an HTTP
    base URL; step 1 reads a directory only.
