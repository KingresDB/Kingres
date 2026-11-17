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
- `digest-chain` compares digests only (at least two, from a directory or files you name); `blocks` checks exported headers only.
  Their output says what they do not prove.
- `checkpoint` without `--state` shows that a checkpoint is signed by a key you trust, not
  that it is the latest one (rollback), and reads no tile. With `--state` it also proves,
  from the tiles, that the checkpoint extends the one you recorded, and nothing else about
  the log. `consistency` reads only the tiles on its proof path. Neither shows that the
  entries behind the hashes are what the operator says. Status 3 is an alert: see "Exit
  statuses".

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
| 1 | a verification failed (the failed step is named), or the publication holds a file in a form it must not have (a symbolic link on the way, a directory, device or named pipe, a file over its size limit, a tile of the wrong size), or the thing under test cannot be parsed (a proof, a block export, a checkpoint note); the word `VERIFIED` is never printed |
| 2 | usage or input error: a flag is missing or wrong, a file or directory **you named** cannot be read (it is absent, not a regular file, too large) or is malformed (digest, columns file, trust file, state file), or no key of the trust file can be used at the time given, or **a checkpoint you hold** (`--state`, `--old FILE`) cannot serve as your anchor (no key of the trust file signed it, or only a revoked one, or one beyond its `max_tree_size`), or the state file stayed locked by another run for 10 seconds; nothing was verified |
| 3 | incomplete: nothing failed, but **the publication does not contain something that was asked for** (a checkpoint, a tile, the directory of a log); `INCOMPLETE` is printed and the missing items are listed; `VERIFIED` is never printed. **Treat 3 as an alert, never as "probably fine"**: a publisher must serve the tiles of every checkpoint it signed, so one that withholds exactly the tiles that would show a fork gets status 3 and not status 1, and a missing file is at best neglect. Only the commands of phase 7 (`checkpoint`, `consistency`) can end with 3; a failure always wins over an incomplete check |
| 4 | a fork or a rollback was found, which is what status 1 says for it, **and the conflicting checkpoint could not be saved** (every name for the evidence is taken by a file with other content, or the disk refuses it); `FAILED at step fork` (or `rollback`) is printed with a note that says `was NOT saved`. The finding stands and is as serious as a 1: act on the fork and save the note you were shown yourself. Only `checkpoint --state` can end with 4 |

A malformed **proof** or **block export** is a failed verification (status 1, step
`parse`): it is the thing under test. A malformed **digest** is an input error
(status 2): the trusted anchor is unusable. `--json` prints one JSON object
(`command`, `verified`, `steps`, `failed_step`, `error`, `notes`, `not_proved`, `info`, `problems`,
for status 3 `incomplete` and `missing`, and `invalid_utf8` when a string had bytes that
are not UTF-8) instead of text; the exit status is the same.

**Which command ends with which status.** The statuses of each command are the ones its exit
sites in the source can give; `TestEveryExitSiteHasARowAndAScenario` reads the sites from the
source (every call of a finish function, of the flag parser and of `exit`) and fails when one
has no row or when the scenario of a row does not reach it with its status, and
`TestReadmeTableOfStatusesIsTheTableOfTheCode` fails when this table differs from the rows.
Status 0 of a command that has none for a failure is a success; `trust` alone is a missing or
unknown subcommand.

| Command | Statuses it can end with |
|---|---|
| `proof` | 0, 1, 2 |
| `row-hash` | 0, 2 |
| `digest-chain` | 0, 1, 2 |
| `blocks` | 0, 1, 2 |
| `checkpoint` | 0, 1, 2, 3, 4 |
| `consistency` | 0, 1, 2, 3 |
| `trust init` | 0, 2 |
| `trust show` | 0, 2 |
| `trust` | 2 |

**Status 3 is not a pass.** It means that the publication did not contain something that was
asked for. A publisher that wants to hide a fork can withhold exactly the tile or the file
that would have shown it, and the run then ends with 3 and not with 1; it can never end with
0. Treat 3 like 1 when you decide what to do, and ask the publisher for the missing files.

Text and JSON are made from one structure: every `ok` step, `note`, `not proved`,
`missing` and `problems` line of the text is a member of the JSON (`steps`,
`notes`, `not_proved`, `missing`, `problems`) and the other way round; a test fails when a
command prints a line in only one of the two. `notes` is what the reader must know about
how the run was made (the clock that was used, a key that was left out and why, a state
file that was written); `not_proved` is what a success does not show. A failed result
carries its notes too.

**Text from the inputs is escaped, one to one.** A file name, a key of a JSON file, a label, a path
on the command line or an error text that quotes them can come from the publisher or the
database server. In the text output (and on the standard error, in both formats) every
such string is written by one function, and what it writes can be read back exactly:

- a backslash is written `\`, so that no input can write an escape or the cut marker;
- a control character (the C0 range, newline and tab included, and DEL) is written `\xNN`
  (a newline is `\x0a`, escape `\x1b`); a byte that is not part of valid UTF-8 is written `\xNN`
  too, with `NN` of 80 or more, which a valid character never gives;
- any other character that changes how text is drawn or split or that is not printable (the
  C1 range, the bidirectional marks, overrides and isolates, the line and paragraph separators,
  the byte order mark, zero width and other format characters) is written `\uNNNN`, or
  `\UNNNNNNNN` above U+FFFF, in lower case hex;
- a value of more than 2,048 bytes is cut at a character boundary and ends with
  `\...(cut: N of M bytes shown)`: `\.` is never written for any other input, so no input can
  print the marker, and M is the original length;
- everything else is written as it is.

To read it back: `\` is a backslash, `\xNN` the byte NN, `\uNNNN` and `\UNNNNNNNN` the
character, and the cut marker ends the text. A message that quotes a value between double
quotes was quoted by Go first (`%q`) and then escaped, so undo the escape first and then Go's
quoting. A real newline and the two characters backslash and `n` print differently
(`\x0a` and `\n`), and `TestUntrustedIsOneToOne` and a fuzz target test that two different
inputs never print the same and that the original comes back when it was not cut.
So no input can start a line (`VERIFIED:`, `FAILED at step`, `  ok  `, `  note:` ...) or
move the terminal, and `TestNoPrintOutsideTheOutputLayer` reads the source and fails when code
outside the output layer prints anything but literals or a value that went through the function.

The JSON output escapes the same characters: everything above that the text writes as an
escape is written in the JSON as a `\uXXXX` escape (a pair of them above U+FFFF), so a JSON
file shown on a terminal cannot move it either; `<`, `>` and `&` are written as they are.
A JSON string cannot carry bytes that are not UTF-8: each becomes U+FFFD and the object has
`"invalid_utf8": true` (a real U+FFFD in the input does not set it). The JSON gives the
strings as they are, without the doubled backslash: a program reads them, a person reads the text.

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
A directory is listed by name (a directory called `x[ab]` is that directory, never a
pattern) and is read through the same helper as a source: at most 1,024 entries, no link
followed. At most 1,024 digest files are read in all (named or in a directory), each at most
64 KiB (a digest is a few hundred bytes) and 16 MiB together; more is status 2.

**Names in a digest directory.** A name the directory listing returns is a name, whatever
it holds (a colon, a space, a leading dash, a backslash on Linux): it is never read as a
path, so only what cannot be one entry is refused (nothing, `.`, `..`, a slash, a NUL, more
than 255 bytes). The export command of this README (below) makes names without a colon
(`digest-20261114T172153Z.json`), which every file system holds, and a test runs the very text
of that command, as far as it can run without a database, and reads the directory it fills.

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

The export is read line by line and only one header is kept, so memory does not grow
with the file: a measured run over 1.2 million blocks (a 400 MB file) used 12 MB of
resident memory, where reading the whole file and all headers (the earlier version) used
1.15 GB (2.9 times the file). A line is at most 1 MiB and the file at most 1 GiB. The
first problem in file order is reported, so a later damage is found only if no earlier
one stops the run (a parse failure and a chain failure are both status 1, because the
export is the thing under test). A read error, a line over the limit or a line that is
cut in the middle after a good start is such a failure at the step `parse`, never "verified
up to the cut".

**What `blocks` establishes, and what it does not.** What it proves is that the blocks it
read are a chain: from the genesis hash of this chain id, dense heights, every hash
recomputed, times that do not go back, and with `--digest` a block at the digest's height
that the digest names. The output says how far that goes: `the N block headers read (heights
0 to H) form an unbroken chain from the genesis hash`, with the hash of block H, and in
`--json` `info.blocks`, `info.head_height`, `info.head_hash` and `info.complete_export_shown`
(always `false`). **It cannot tell a complete export from one that was cut**: the hash of a
block does not say whether another follows, so an export that ends at any block verifies the
same way. Without `--digest` the output says that; with one it says that the digest covers up
to its height and that blocks after it, or an end before the head of the ledger, are not
covered. An export that ends before the digest's height is a failure at the step `digest`.
To know that the export is the whole chain, compare block H with a digest you hold of height
H or above, or with the head the ledger reported when you made the export.

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
kingres-verify trust show --trust trust.json [--now TIME] [--json]
kingres-verify checkpoint --trust trust.json --log chain:<id>[:e<N>]|aggregate:<id> [--source DIR | --note FILE] [--min-size N] [--state FILE [--allow-shared-state-dir]] [--now TIME] [--json]
kingres-verify consistency --trust trust.json --log <as above> --old NOTEFILE|SIZE:ROOT [--new NOTEFILE] [--source DIR] [--now TIME] [--json]
```

**`trust init`** refuses to run without `--fingerprint` (the SHA-256 of the
public key in hex; upper case and colons are accepted) and refuses a key that
does not match it, so a mistyped or swapped key is noticed. It never overwrites
a file. `trust follow` (rotation events of the publication) is not built yet and
exits with status 2.

**`trust show [--now TIME]`** validates a file and prints, for each key, how it
stands at `--now` (default the system clock): `usable`, `not yet valid`,
`expired` or `revoked`, and a line for each validity member the key has that
says that it is enforced and how (below). A file that is well formed but in which
no key can be used at that time is reported as such and ends with status 2,
because every verifying command would refuse it; a file with no key at all is
refused when it is read. The time-stamp authority roots and client keys of the
file are validated and counted, and the output says that no command uses them yet.

**Validity of a key** (design 14, correction 14). A checkpoint carries no time that
can be trusted, so the verifier can enforce what the data proves (a tree size) and
what the auditor says (the time given with `--now`), and nothing else:

| Member | Effect |
|---|---|
| `not_before`, `not_after` | enforced on `--now` (default the system clock, printed in the output with its fraction of a second), inclusive at both ends (a time equal to `not_after` is still inside, one nanosecond later is not): a key outside its window is left out, with a note that says why; if no key is left the command ends with status 2 and lists each key and why. After `not_after` a retired key's checkpoints therefore stop verifying as **new** checkpoints unless you give an earlier `--now`, which is your statement and is printed. **A checkpoint you already hold** (`--state`, `--old FILE`) is not subject to the window: see "Checkpoints you hold". The window is measured on the clock of the machine you run on: a machine whose clock was set back still accepts a key whose `not_after` has passed, so for a key that leaked use `revoked`, which does not depend on the clock. This fails closed: a member that only reported would look like a control and not be one. `--now` must be an RFC 3339 time with a point as the decimal mark; an empty `--now` (an unset shell variable) is an error, never the clock |
| `revoked` (true or false) | true: the key is never used for a new checkpoint, whatever `--now` says, and its signature on a checkpoint you hold is not accepted either. A note signed by a revoked key and by a valid one is accepted through the valid key, and the output says that a signature of the revoked key was on it and was not counted |
| `max_tree_size` (an integer from 1) | a checkpoint counts as signed by this key only if its tree size is at most this: the way to retire a key at a known size so that what it signed stays valid and what it signs later (a leaked key) is refused, step `key-size`. It does not stop a leaked key from signing a different history at a smaller size: only a checkpoint or consistency proof you already hold catches that. The limit is applied to a checkpoint you hold too. `trust show` cannot know the size of your log, so a key it reports as usable may still be refused for a larger checkpoint. `revoked` and `max_tree_size` together are refused |

**`checkpoint`** verifies one checkpoint note: the note is well formed, at
least one signature is by a key of the trust file that may be used now, no
signature of such a key is wrong, the origin is the origin of the named log below
the trusted prefix, the tree size is within what the trust file allows for the
signing key, and the size and the root are well formed (`--min-size` fails a
smaller tree). It reads `<source>/c/<id>/e<N>/checkpoint` (aggregate:
`<source>/a/<id>/checkpoint`) or `--note`. Without `:e<N>` the epoch is taken
from the source only when it holds exactly one epoch of that chain; with `--note`
the epoch must be given. It checks no tile.

**Rollback, and what `--state` and `consistency --old` are for.** `checkpoint` shows that a
checkpoint is signed, **not that it is the latest**: an older genuine checkpoint verifies, so a
publisher that serves one hides every later entry. Without `--state` the output says so ("not
proved: that this is the latest checkpoint ... (rollback)"). There are three ways to close it:

- `--min-size N`: fail a tree smaller than the size you already saw.
- `checkpoint --state FILE`: the way to **follow** a log over time. The state file is your
  record of the last checkpoint this command proved. The invariant is that **the state only ever
  moves from a checkpoint to one that this very run proved to be an extension of it, for the same
  bytes whose signature it verified** (the note is read once and used for both), so status 0
  with `--state` means exactly that. A larger checkpoint is therefore proved in the same
  command: it reads the proof tiles from `--source` (RFC 9162 2.1.4.2) and only then replaces
  the state (atomically: a temporary file in the same directory, flushed, renamed over the state,
  the directory flushed; on Windows the rename is written through). The first run creates the
  state and says that nothing earlier was known. The same checkpoint again changes nothing and
  needs no tile. A smaller size, the same size with another root, or a path that does not lead to
  the stored root is **status 1**: the state is left untouched, the conflicting note is saved
  next to it as `<state>.conflict-<size>-<root in hex>` (see "The evidence of a fork"), and the output names both files: two notes signed by the same key for
  trees that are not one history are the proof of misbehaviour, so **keep both**. A smaller size
  is reported as `rollback` and may be an old genuine checkpoint or another history; this run does
  not tell which. If the tiles are missing the result is **status 3** and the state stays; if a tile
  is present and wrong (the tiles do not lead to the signed new root) it is status 1 and the state
  stays. `--source` must therefore be a source that holds the tiles; without them `--state` cannot
  advance.
- `consistency --old FILE|SIZE:ROOT`: a **read-only** proof for a checkpoint you hold by hand (a
  note you saved, a state file, or a size and root from elsewhere). It writes nothing, and it does not
  update a state file: it is the check for an old note that is not your state, or for
  a second opinion. The earlier advice to run `consistency --old STATE` and then `checkpoint --state
  STATE` is withdrawn: they were two reads of the publication, a publisher could show an honest
  checkpoint to the first and a fork to the second, and the second replaced the evidence of the
  first. `checkpoint --state` does both in one run.

**The state file** is a small JSON record (`format`, `log` the origin, `size`, `root`,
`verified_at`, `verified_by` the fingerprints of the keys, `note` the signed note). The facts are
for the reader, and they must agree with the note (see "What a state file must be"); the decision
is the note's: it still has to carry a valid signature of a key of the trust file that is not
revoked (a person who can write the file can also delete it).
It may be a link (the state is replaced where the link points, the link stays a link; a link to
nothing is refused). A new state file has mode 0600, a replaced one keeps its mode. The temporary file has a short
name that does not depend on the length of the state's name. The state file is locked with `flock`
(Linux, macOS) or `LockFileEx` (Windows) on `.kingres-state.lock` in its directory for the whole read,
check, proof and replace; another run waits up to 10 seconds and then ends with status 2 (nothing
changed), so two runs never move the state backwards. A reader that does not take the lock (`consistency --old STATE`) can on
Windows meet a sharing error while the file is replaced; run it again. A crash leaves the old state or the new
one, whole, and at worst a temporary file that the next run removes (tested by stopping after each
step, on both platforms). Old state files that are a plain signed note are still read.

**What a state file must be, and what the verifier never writes.** The invariant is that the
verifier never writes a state it would not read back, and every limit on what is read is a limit
on what is written, by the same code. A note is bounded where it is parsed (see "Limits"): at most
64 KiB, 32 lines of text, 32 signatures and 1,024 bytes a line, and at most 16 extension lines
in a checkpoint, so a signed checkpoint that exceeds them (a validly signed note with an
extension line of 700,000 characters made a state of 4.2 MB that no later run could read) is
refused **before anything is stored**, as a failed `parse` (status 1), with the state and the
folder untouched. The record built from a note is at most twice its size plus a few hundred bytes
(`<`, `>` and `&` are not rewritten), and the limit on reading a state is that size plus 16 KiB
(144 KiB), the same constant as on writing. Before a state is stored it is **read back** through
the functions the next run uses (the same decoder, the same trust file, the same checks of
"Checkpoints you hold") and must give the checkpoint that was verified: a note that is good as
a new checkpoint but that a later run would refuse as a held one (a wrong signature of a key
that is revoked now is ignored for a new note and refused for a held one) is not stored; the
run ends with status 2, says `cannot be recorded`, and the state is unchanged. The decoder of
a state is strict: one object, every member exactly once (`format`, `log`, `size`, `root`,
`verified_at`, `verified_by`, `note`), no unknown member, no text after the record (white space
is allowed), format 1, the right types, a `verified_at` that is a time, at most 32 fingerprints,
and the `log`, `size` and `root` of the record must be those of the note it holds (the note
decides, a record that says otherwise was damaged or edited). Anything else is status 2 and the
file is never changed or removed. **An empty state file is not "no state"**: a state that lost
its content (a disk that kept the name and not the data after a power loss) must not make the next
run record a first checkpoint without any check against a rollback. It is status 2 and says
so; if you made the file yourself with `touch` it holds nothing to lose (remove it or give
another name); if it ever held a state, restore your copy, or start a new record with
`checkpoint --state NEWFILE --min-size N`.

**Where the state may be.** The state is your protection against a rollback, so it must be in a
directory that only **you** can write and that you own. On Linux and macOS the verifier checks
the directory (and the state file when it exists): when it belongs to another user or group or
others can write it (mode `022` bits), `checkpoint --state` is **refused** with status 2 and nothing
is created, and `--allow-shared-state-dir` accepts it, with a note in the output. A user who can
write the directory can replace or delete the state, plant a link at the name of the lock or of
the evidence, or hold the lock for ever. On Windows the access control list of the folder is not
read: keep the state in a folder only you can write. A new state file, its lock file and the files
of evidence have mode 0600 (a replaced state keeps its mode). The lock file and the evidence
are opened **without following a link** and must be regular files (`internal/safefile`, `O_NOFOLLOW`
on Unix, the reparse point flag on Windows): a link at the name of the lock is status 2 and nothing
is created where it points.

**What the lock does and does not protect.** The lock is `.kingres-state.lock` in the directory of
the state (not named after the file), taken with `flock` or `LockFileEx` for the whole read, check,
proof and replace. It excludes every other run of this program that uses a state in that
directory, whatever name it gives the file (a hard link in the same directory, two spellings of
one name on a system that ignores case): the second waits up to 10 seconds and then ends with
status 2. It does **not** cover two names of one file in different directories (a hard link from
another directory: a run replaces the name it was given, which does not change the other link),
a program that does not take it, or a user who can write the directory (see above): use one name
for a state.

**The evidence of a fork.** When `--state` finds a fork or a rollback the conflicting note is saved
beside the state as `<state>.conflict-<size>-<the whole root in 64 hex>` (the state's name is
shortened above 100 bytes), created exclusively and never through a link. When a file is already
at that name its content is compared: the same note is "already saved" (an earlier run saved it),
**another content is never called saved**: the next name (`-2`, `-3` and so on, 8 in all) is used. If
none can be used or the disk refuses, the output says `was NOT saved`, `--json` has
`info.state.evidence_saved` false and `conflict_file` null, the finding is printed as for a fork,
and the status is **4**, which is not 0 and is not the 1 of the fork itself: act on the fork and
save the note you were shown yourself.

**Checkpoints you hold** (`--state`, `--old FILE`; design correction 17). A checkpoint you kept is
a statement about the past, so it is judged differently from a checkpoint under test, and the same
way in both commands: its signature must verify against a key of the trust file and no signature of
a key of the file may be wrong; a **revoked** key's signature is not accepted; a key limited by
`max_tree_size` counts only up to its size; the **validity window is not applied** (a note signed by a
key since retired by `not_after`, or not yet valid, stays usable, and a note says so). Retiring a key
by `not_after` therefore does not make you discard your state. Anything else (not signed by a key you
trust, malformed, another log, a revoked key, beyond a key's limit) is **status 2** in both commands,
never 1: the thing you hold cannot serve as your anchor. When the cause is a revoked key, a size beyond
a key's limit or a changed trust file, the message does **not** say to delete the state (with no
state a rollback is no longer noticed). It says to keep the file, obtain a checkpoint of the log
signed by a key you trust from a source you trust, and run `checkpoint --state NEWFILE --min-size N`
with N the size you believe the log had (the old note says what it claims, which nothing
authenticates), and to use NEWFILE from then on. What trusting the record costs: whoever can write your
state file can also delete it or put an older genuine checkpoint in it, which weakens the protection
the same way; they cannot make a revoked key's note acceptable.
A key that leaked should be marked `revoked`; `not_after` is only as good as the clock.

**`consistency`** checks that the log at the new checkpoint (`--new`, default
the checkpoint in `--source`) extends the log at the old one (`--old`: a note
file or a state file, verified as "Checkpoints you hold" says, or `SIZE:ROOT` with the root
in base64, taken as you give it and printed as not authenticated). It writes nothing. It reads the
proof hashes from the tiles under
`<source>/c/<id>/e<N>/tile/...` (full and partial tiles; a partial tile that
is missing is read from the full tile), and verifies them against both roots with
RFC 9162 section 2.1.4.2. **Only the tiles on the proof path are read** (the
output says how many): a tile that is damaged but not on that path is not noticed,
and the output lists it as not proved. When the sizes are equal, or the old size is 0,
the proof is empty and no tile is read; the output says "no tile was needed". A new
size smaller than the old one fails (the log went back). A tile that is absent gives
status 3; a tile that is present and wrong gives status 1.

**Sources and files.** A source is a directory (a tar file and an HTTP source are
not built yet). Every file the verifier reads goes through one helper
(`internal/safefile`): it opens without blocking (a named pipe cannot hang it),
requires a regular file after the open, and bounds the size before and while
reading. In a source directory **no symbolic link or Windows reparse point is
followed, in any component of a path below the directory**, and a path cannot leave
the directory (`os.Root`, Go 1.24 and later; the module needs Go 1.27.3 and the
packaging build uses it with `GOTOOLCHAIN=local`). Links were refused instead of
resolved inside the directory because a publication written by the publisher's
tools has no link, so a link is an accident or an attempt to alias one tile as
another or to reach outside, and a rule that allowed some would have to explain
which. A directory you pointed `--source` at may itself be a link. A file you
name on the command line (`--trust`, `--note`, `--old`, `--new`, `--state`, the
files of the older commands) may be a link, since you chose it, but must be a
regular file after resolution, never a pipe, device, socket or directory. On
Windows the device names (`NUL`, `CON`, `COM1`, ...), the device namespace
(`\\.\`) and alternate data streams are refused by name. A directory of digests
or a directory of epochs is read at most 1,024 entries.

**What is status 2, 3 or 1** (one rule for every command, tested site by site in
`TestEveryExitSiteHasARowAndAScenario` and, for `checkpoint` and `consistency`, in
`TestExitStatusFollowsTheRule`):

| Status | When | Examples |
|---|---|---|
| 2 | what **you** named or hold cannot be used | a missing or malformed `--trust`, `--source`, `--note`, `--old`, `--state`; a held checkpoint no key of the trust file can vouch for (also a bad signature or another log); no key usable at `--now`; `--now` empty or malformed; two epochs of the chain in the source and no `:e<N>` in `--log` (you must name one); the lock of the state; a file of the publication that exists but that your account cannot read (permission denied: it is your copy, so it is not the publisher's absence) |
| 3 | what the publication **should hold and does not** | no checkpoint file, no tile, a chain directory that holds no epoch directory (with or without `:e<N>`), no chain directory |
| 1 | what the publication holds in a form it must not have, or the thing **under test** fails | a link, a pipe, a directory, a file over its limit, a tile of the wrong size; `--note`, `--new` or the checkpoint of the source that is malformed, forged, of another log or beyond a key's limit; a wrong root; a fork or a rollback with `--state` (4 when its evidence could not be saved) |

A held checkpoint is status 2 and the thing under test status 1 because the first is your anchor
(a wrong one is your problem) and the second is the claim being checked.

Formats, with the specification they follow:

- **Signed note** (signed-note, "Format"): UTF-8 without control characters
  except newline; the text ends in a newline; the last blank line separates it
  from one or more lines `U+2014 SP name SP base64(4-byte key id || signature)`.
  Key names are non-empty without Unicode spaces or `+`. Base64 must be
  canonical (RFC 4648 3.5), which is checked by re-encoding. Two lines with the
  same name and key id are rejected (tlog-checkpoint). At most 32 signatures,
  32 lines of text, 1,024 bytes a line and 64 KiB in all. Key id = `SHA-256(name || 0x0A || 0x01 || public key)[:4]`.
  A line whose name and key id match no trusted key is ignored; a line that
  matches one must verify, or the whole note is rejected (the specification
  says SHOULD); no verified line means rejection.
- **Checkpoint** (tlog-checkpoint): at least three non-empty lines, origin,
  size as a canonical decimal below 2^62, root as canonical base64 of 32 bytes;
  further lines are extension lines (at most 16), kept and reported but without meaning here.
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
  SHA-256 of the key, `not_before`, `not_after` as RFC 3339 UTC or null, `revoked` true or
  false, `max_tree_size` an integer from 1 or null; at least one key is required),
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

## Limits and memory

The files the verifier reads come from a database server, a publisher or a directory somebody
else filled, and a note can be signed by a key of your trust file and still be hostile. What one
input can make the program allocate, store or print is therefore bounded by constants, never by
the shape of the input. **Memory is a constant plus a small multiple of the largest single value
(one string, one streamed element), never a multiple of the input.**

| What | Limit |
|---|---|
| A proof, a columns file | 16 MiB |
| A digest file | 64 KiB each, 1,024 files and 16 MiB together for `digest-chain` |
| A trust file | 1 MiB |
| A signed note (a checkpoint, `--old`, `--new`) | 64 KiB, 32 lines of text, 32 signature lines, 1,024 bytes a line; a checkpoint has at most 16 extension lines |
| A state file | 144 KiB (twice the largest note and 16 KiB for the rest) |
| A block export | 1 GiB, 1 MiB a line |
| A source directory, a digest directory | 1,024 entries |
| JSON, every file | nesting 32; 16,384 values in a document (a proof counts the elements of `chain` one by one, not together); 1,024 members in an object; 256 bytes in a key; 64 KiB in a string as written (a `send_hex` of a columns file may be 16 MiB); 64 characters in a number; 65,536 characters of white space in a row |
| A proof's `chain` | read element by element; the headers kept are about 100 bytes each and a header is at least 230 bytes of the file |

What the JSON reader does about each: a document that exceeds a limit is refused at the first
value that does (`more than 16384 values`, `a string is longer than 65536 bytes`, `nesting deeper
than 32 levels`), so eight million zeros in a 16 MiB file are not turned into eight million
nodes (the first version held 2.0 to 2.4 GiB for such a file: a tree of values, each with the
text of its path); the path of a value is built only when an error says it. A file is read into one
buffer of its size (not by doubling); the white space between two tokens is bounded because the
decoder keeps it until the next token.

Measured on Linux, peak resident set of a child process (VmHWM; a run of `version` is 6 MiB),
the worst of nine hostile shapes (zeros in an array, a nesting of 16 million, a million members, a
million equal keys, one string of the whole file, a file of white space or of newlines, and a file
of `{}` lines) for each command that reads a file, at the largest size the command accepts and at 16 MiB:

| Command and file | Peak resident set |
|---|---|
| `proof --proof` (16 MiB) | 29 MiB |
| `proof --proof` with a chain of 49,618 valid headers (15 MiB) | 50 MiB |
| `proof --digest`, `digest-chain` (64 KiB; 16 MiB is refused unread, 6 MiB) | 14 MiB |
| `row-hash --columns` (16 MiB, the worst case is one string of the whole file) | 72 MiB |
| `blocks --blocks` (16 MiB of lines) | 8 MiB |
| `blocks --digest`, `checkpoint --note`, `--trust`, `consistency --old`, `trust show` | at most 15 MiB |
| `digest-chain` over a directory of four files of 16 MiB (the review's case, 3.1 GiB before) | 6 MiB (the first is refused by its size, unread) |

`TestPeakResidentSetOfACommandOnAHostileFile` (Linux; it runs the test binary as a child and
reads its `VmHWM`) fails above the size of the file times five plus 8 MiB over the baseline, and
`TestAllocationDoesNotGrowWithTheShapeOfTheInput` (every system, in the process of the test)
measures the peak of the heap with the collector made eager and fails above the same bound. The
first one is skipped on Windows, which has no `VmHWM`; the second runs there.

## Exporting from a database

Run these where the database is, then copy the files to the auditor's machine
(the digest must go to a place the database's operators cannot change):

```
# digest (builds a block first if transactions are pending); store it OUTSIDE the database
psql -X -At -c "SELECT kingres.ledger_generate_digest()" > digest-$(date -u +%Y%m%dT%H%M%SZ).json

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
(format), `FuzzParseDigest`, `FuzzParseBlocks` (chain), `FuzzVerify` (proof), and for phase 7 `FuzzParse` (signednote, trust), `FuzzVerify` (checkpoint), `FuzzParseTilePath` and `FuzzParseTile` (tlog), `FuzzParseLogSpec` (cli), `FuzzCleanRel` (safefile: the relative paths the source directory accepts). `scripts/fuzz.sh [seconds]` runs every target for a short bounded time; the seeds (the published vectors) also run in `go test ./...`.
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
| `internal/safefile` | the one way every file is opened: never blocking, regular files only, bounded size, no link below a source directory |
| `internal/statefile` | the state file: lock, atomic replace with a crash test, link resolution, the place for conflicting evidence |
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

## Findings of the review of step 1 and what was changed

The independent review of steps 1 and 2 (`kingres/docs/reviews/2026-11-16-ledger-phase7-steps-1-2.md`)
found these in the verifier; each has a test that failed before the change.

- **L7-01, L7-05** (a named pipe hangs the verifier; links are followed): one file helper
  for everything the verifier opens (`internal/safefile`) and the rule above for sources.
- **L7-02** (`not_after` reported only, notes missing from `--json`): the validity members
  of a key are enforced and `trust show` says how; text and JSON are made from one structure.
- **L7-03** (an older genuine checkpoint is VERIFIED): the "not proved" list names rollback
  and the three remedies; `checkpoint --state`.
- **L7-06** (output claimed more than was checked): the tile line says how many tiles were
  read and that no other was; an empty proof says that no tile was needed; the help and this
  README say that only the proof path is read.
- **L7-07** (status 2 for a missing checkpoint, 3 for a missing tile; `trust show` accepted a
  file with no key): one rule for what the publication does not contain (status 3), and a trust
  file with no usable key is refused by every command including `trust show`.
- **L7-10** (status 3 and a hostile publisher): the output and "Exit statuses" say that 3 is
  an alert.
- Found in the pass over the older commands: `digest-chain` over an empty directory printed
  `CONSISTENT: 0 digests agree`; `blocks` without `--digest` said "the block chain is consistent"
  without saying that a chain rewritten from the genesis block verifies too; the text output of
  `proof` and `blocks` called the "not proved" items "note" while `--json` called them
  `not_proved`; a failed `proof` printed none of the steps that had passed (the JSON did); the text of `proof` had two lines (the
  entry and the operation) that were not in `--json` as lines (they are steps now); every
  command opened its files with `os.Open`.

## Findings of the review of the fixes (L8) and what was changed

The second review (`kingres/docs/reviews/2026-11-16-ledger-phase7-steps-1-2-fixes.md`) found
these in the verifier; the design has the decisions as corrections 17 to 19.

- **L8-01** (`--state` accepted a fork of a larger size and replaced the evidence): `checkpoint
  --state` proves the extension from the tiles in the same run and for the same bytes, keeps
  the state and saves the conflicting note on a fork or rollback; `consistency --old` is the
  read-only proof; the two-command order is withdrawn.
- **L8-02** (input text reached the output raw): one function renders every string, see "Text
  from the inputs is escaped".
- **L8-03** (a state or `--old` note signed by a key retired since): a checkpoint you hold is
  judged by signature, revocation and size limit, not by the window; one status (2) in both
  commands; the advice never says to delete the state.
- **L8-04, L8-07** (two runs lose the higher state; a link replaced; a long name failed; mode
  0600; no directory flush): lock, write through a link, short helper names, kept mode, a flush
  of the directory (a write-through rename on Windows), a crash test at every step.
- **L8-05** (`digest-chain` caps and glob characters): listed by name through the safe helper,
  1,024 files and 64 MiB.
- **L8-06** (three exit statuses): a chain directory with no epoch is 3; a held note is 2 in both
  commands; a publication file your account cannot read stays 2, documented in the table above.
- **L8-09** (key windows): clock and `revoked` said in the table, the time is printed with its
  fraction, the boundary is documented, an empty or comma `--now` is an error, a revoked signer
  on a note is named, `trust show` says what it cannot know.
- **L8-11**: `blocks` streams; `row-hash --json` no longer says `"verified": true`; the message
  for a regular file given as a source no longer says "special file".

## Findings of the review of the second fixes (L9) and what was changed

The third review (`kingres/docs/reviews/2026-11-17-ledger-phase7-steps-1-2-second-fixes.md`)
found these in the verifier; each fix has a test that fails without it (shown by removing the
fix in a copy of the tree).

- **L9-04** (a validly signed note with a long extension line made a state of 4.2 MB that no later
  run could read): the note is bounded where it is parsed, the record is bounded by the same
  constant on writing and on reading, and a state is read back through the reader's own functions
  before it is stored ("What a state file must be"); a property test and a fuzz target write
  every state they can and read it back.
- **L9-05** (2 GiB for a 16 MiB JSON file): caps on depth, values, members, key and string
  length, white space, and the elements of a proof's chain streamed ("Limits and memory"), with a
  measurement of the peak memory of every command that reads a file.
- **L9-06**: a directory of digests is read by the names its listing gives; the export command
  makes names without a colon.
- **L9-07**: evidence is claimed saved only when a file with its content is there; status 4 when
  it could not be saved.
- **L9-08, L9-12**: the lock is opened without following a link, is one per directory, the
  directory of the state must be private (Unix), and the README says what the lock does not cover.
- **L9-09**: the table of statuses is derived from the source and tested; `blocks` is status 1 for
  an export that cannot be parsed.
- **L9-10**: the escaping is one to one, the cut is marked in a way no input can print, and the JSON
  escapes what the text escapes.
- **L9-11**: tests for the mutants that survived (the JSON `extends_state`, the strictness of the
  state, a stored size of 1, a read error in `blocks`, the printed time, the order of the flushes
  of the state), and a statement of what `blocks` does and does not show about an export that ends.
- **L9-13**: the failure seen on Windows (a test directory that cannot be removed at the end of
  a test, "The directory is not empty") was reproduced, about once in 150 runs of a state
  command and about once in three runs of the whole suite on a machine with a file scanner, and
  it is not the program: no handle of the program is left open after a run (a test counts them on
  Windows, before and after a collection, over 60 runs of three commands), and the directory can
  be removed some 12 ms later by the same process without anything being closed, so the holder is
  outside it (a scanner opens a new file for a moment). Nothing was added to retry it. The memory
  tests write smaller files on Windows so that they give such a scanner less to read. The tests of
  the state package are named `TestStateFile...`, so the filter `-run 'State|HeldNote'` runs them.
- **L9-15**: an empty state file is status 2 with advice that keeps the protection.

Changes of interface that a caller can see: exit status 4; the flag `--allow-shared-state-dir`;
`blocks` output (the headline, one more "not proved" line and `info.blocks`, `info.head_height`,
`info.head_hash`, `info.complete_export_shown`); `info.extends_state` and
`info.state.extension_proved` are `false` (not null) for a first use; `info.state.evidence_saved`;
`"invalid_utf8"`; the state file name `<state>.conflict-<size>-<64 hex>`; the lock file
`.kingres-state.lock`; a new state file has mode 0600; notes over 64 KiB, with a line over 1,024
bytes or with more than 16 extension lines are refused; digests over 64 KiB are refused;
escapes in the text output (`\` for a backslash, `\x0a` for a newline, a cut marker).
