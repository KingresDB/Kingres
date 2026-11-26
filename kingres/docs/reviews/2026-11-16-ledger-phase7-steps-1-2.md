<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->

# Review of ledger phase 7, steps 1 and 2 (verifier core, kingres_ledger 0.3 part A)

Reviewer: independent, adversarial. Date: 2026-11-16.
Reviewed: commit `8ceed6b88b` (step 1, `kingres/verifier`: signed notes,
checkpoints, tile logs, consistency proofs, trust file, commands `checkpoint`,
`consistency`, `trust`) and commit `86e15fcae3` (step 2, `contrib/kingres_ledger`
0.3 part A: `ledger_block_preimage`, `ledger_head`, `ledger_blocks_entries`).
Both ids were checked with `git cat-file -e`. `git diff --stat HEAD` for the two
directories was empty, so the committed state is the working state.

## Summary

No way was found to make the verifier say "verified" for a rewritten, forked,
truncated-below-an-old-checkpoint or untrusted-key history, to crash it, or to
make it use unbounded memory; and no way was found to read the chain through the
new functions as a role without the grant, or to make a damaged chain come
back as a plausible answer from a range that contains the damage. The proof and
note code agrees with independent implementations on every input tried (about
105,000 mutated consistency proofs, 300,000 mutated notes, every tree size up to
130, sizes up to 2^62 - 1, and a tree with a level 2 tile).

The defects are at the edges: what the tools say they prove, special files in
a source directory, and one case in which the README promises more than the
function does.

| ID | Severity | Title |
|---|---|---|
| L7-01 | Medium | A named pipe (or any blocking file) in a source directory hangs the verifier for ever |
| L7-02 | Medium | A key marked `not_after` keeps verifying with exit 0, and `--json` drops the only warning |
| L7-03 | Medium | A stale but genuine checkpoint is "VERIFIED"; the list of what is not proved omits freshness |
| L7-04 | Medium | README: "never a silently shortened answer" is false for the tail of the chain, and what is not detected is not stated |
| L7-05 | Low | Symbolic links inside a source directory are followed |
| L7-06 | Low | Output and help claim more than was checked (tile content, "read from the tiles") |
| L7-07 | Low | Exit status of a missing checkpoint is 2, of a missing tile 3; `trust show` accepts a file with no key |
| L7-08 | Low | `ledger_head` returns unchecked values; the chain identity is checked only at block 0; the block before a range is trusted by its stored hash |
| L7-09 | Note | `ledger_blocks_entries` runs as the caller; `EXECUTE` hides nothing that `SELECT` does not give; `REVOKE ... FROM PUBLIC` on the ledger catalog tables fails |
| L7-10 | Note | A publisher can turn every failure into exit 3 by withholding tiles |

Count: critical 0, high 0, medium 4, low 4, note 2 (10 findings).

Baselines and the numbers of the runs are at the end ("Baseline runs").

## Scope and method

Read completely: all of `kingres/verifier/internal/signednote`, `checkpoint`,
`tlog` (tile.go, tree.go), `trust`, `merkle/consistency.go`, the new part of
`internal/cli` (cli_tlog.go and the diff of cli.go), `scripts/fuzz.sh`, the
README of the verifier ("checkpoint, consistency, trust", "Findings about the
phase 7 design"), `kingres/docs/design/ledger-phase7.md` sections 4, 8.1 and
14, `kingres_ledger--0.2--0.3.sql`, the 0.3 parts of `kingres_ledger--0.3.sql`
(grants, the three functions), the diff of `src/merkle.c` and `merkle.h`, the
README section "Reading the chain for publication", and the form of the
reviews of the ledger 0.1 and 0.2. The specifications were fetched and read in
full: C2SP signed-note, tlog-checkpoint, tlog-tiles, and tlog-cosignature (for
the definition of an origin), and RFC 9162 (2.1.3 and 2.1.4).
Not read line by line: `src/protect.c`, the tests of the authors, the 0.1
and 0.2 parts of the SQL scripts.

Run, all in exports and copies, nothing in the repository was touched:

- an export of the two directories plus `kingres/dev` and `kingres/docs` with
  `git archive`; for PostgreSQL, private copies of the 18.7 and 19rc2
  installations (the originals were not written), data directories under
  `~/kw-review18/work`, ports 59318, 59319 and 59328 (a standby), all stopped
  at the end;
- the verifier built for Windows and for Linux; the Linux binary named by
  `KINGRES_VERIFY` for the extension tests;
- independent code written for this review (kept in the review work area, not
  in the repository): a recursive RFC 6962 / RFC 9162 implementation of roots,
  inclusion and consistency proofs; a tile and signed-checkpoint writer that
  uses only the standard library and does not share code with the verifier; a
  differential test of the note parser against `golang.org/x/mod/sumdb/note`;
  SQL scripts for damage, arguments, privileges, dump and restore,
  concurrency, a standby and the catalog comparison.

Experiments on the verifier:

- notes: 300,000 mutated notes (bit flips, deleted and inserted bytes
  including CR, NUL, NEL, `=`, `+`, a missing or doubled final newline;
  duplicate lines; unknown signers; a known name with the wrong signature)
  compared with `x/mod/sumdb/note`: in no case did the verifier accept what
  that implementation rejects (it rejects more: duplicate lines, non-canonical
  base64, control characters); the text it returns was always the same;
  a line with the trusted name and key id and the signature of another key
  rejects the note;
- proofs: for every `first <= second <= 130` the canonical proof is accepted;
  105,343 mutations (a hash dropped at either end, a hash flipped, two swapped,
  one added at either end, the old root appended) were all rejected; an old
  root or a new root of another tree, and a fork that changes the last leaf of
  the old tree, were rejected for every pair; sizes 0, equal, and first larger
  than second behave as the code comments say; with the reader and tile files
  of sizes up to 70,000 (random firsts) and of 16,975,111 leaves
  (`2^24 + 3 * 65536 + 5 * 256 + 7`, which has a level 2 tile) the proof equals
  the reference proof and verifies (a mutated hash is rejected); sizes `2^62 - 1`, `2^61` and
  `2^61 + 12345` over a synthetic tile source need 29 to 36 tile reads and a
  proof of at most 62 hashes; 2^62 is refused;
- command line, with a log written by the independent writer (700, 11,030
  entries, a log with the real entries of this extension): `checkpoint` and
  `consistency` with the right key, an untrusted key (seed other than the
  pinned one), a checkpoint of another chain under the same path, a log that
  was forked at an old leaf (same key), forked after the old size, truncated
  to a size below the old checkpoint, one leaf changed, one leaf deleted, two
  leaves swapped, a tile missing, short, long, flipped; `--old SIZE:ROOT`
  right and wrong; `--min-size`; trust files with a duplicate member, a
  duplicate key, trailing data, a byte order mark, a float, a time zone
  offset, upper case fingerprint, an unknown member, 1,000,000 nested `[`;
- Linux file types as a source: a FIFO as a tile, a FIFO as the checkpoint, a
  symbolic link to `/etc/passwd` and to `/dev/zero`, a 4 GiB sparse tile and a
  4 GiB sparse checkpoint;
- fuzzing: the six new targets of `scripts/fuzz.sh`, 180 s each, `-parallel 2`.

Experiments on the extension (both majors unless said):

- a chain built through 0.1 (blocks before and after each update), through
  0.2 to 0.3, and fresh: every block's SHA-256 of `ledger_block_preimage`
  equals `block_hash` and `ledger_block_hash`; the preimage is 87 bytes; every
  `ledger_blocks_entries` row hashes to its block; the catalog of the three
  databases (every function with owner, security, volatility, parallel safety,
  strictness, `proconfig`, ACL, language, cost, return type, MD5 of the
  source and the comment; all relations with owner, ACL, options, comment;
  columns, constraints, indexes, triggers, event triggers, types, schema,
  extension membership, `pg_init_privs`) is identical, 1,907 lines, for
  0.1 to 0.3, 0.2 to 0.3 and fresh 0.3, on 18 and on 19; a deliberate
  `GRANT` on one copy made the comparison differ (the comparison is not blind);
- damage with the capture and block triggers disabled in `pg_trigger` (as the
  README's own tests do), on an 8-block chain: `hash_version`, one byte of
  `prev_hash`, of `tx_merkle_root`, `tx_count` (plus 1 and -1), `closed_at`
  (plus one microsecond, infinity), one byte of `block_hash`, a short
  `block_hash`, a renumbered height, a deleted block in the middle, the last
  block (see L7-04), blocks 6 and 7, the first, a swap of two heights, a block appended
  with a correct hash and a wrong link, with a right link and a wrong hash,
  fully valid, a changed chain identity, a deleted chain identity, a
  duplicated height (primary key made non-unique in `pg_index`);
- arguments: `(0,5)`, `(0,0)`, `(-1,5)`, `(0,-1)`, NULL in either place,
  `from = 2^63 - 1`, `max_count = 2^31 - 1`, `from = head`, `head + 1`, a
  `smallint` second argument, and the cap with 11,030 blocks
  (`(0, 20000)` returns 10,000 rows, `(10000, 2^31 - 1)` returns 1,030);
- privileges: a role without a grant and a role with `EXECUTE` on the two
  read functions, on a fresh 0.3, on 0.1 to 0.3 and on 0.2 to 0.3, and after
  `pg_dump` and restore (`proacl` and behaviour);
- concurrency: a `REPEATABLE READ` reader holding its snapshot for 6 s while a
  writer and the block builder ran (0.04 s, nothing blocked, the reader kept
  its head); a reader while a transaction holds the builder's locks (0.12 s);
  a 20 s loop of 516 builds against 575 reads with no error; a hot standby
  that follows the primary, calls both functions in a read-only transaction
  and sees the new head 2 s after a build.

## Findings

### L7-01 Medium: a named pipe in a source directory hangs the verifier

Place: `kingres/verifier/internal/tlog/tree.go`, `FSSource.read` (opens with
`os.DirFS` and reads with no check of the file type); `internal/cli/cli.go`,
`readLimited` (checkpoint, `--note`, `--old`, `--trust`).

The threat model says whoever controls the input must not be able to hang the
verifier. Only the size of a regular file is bounded (the 4 GiB sparse tile and
checkpoint were refused at once, 7 MB of memory). A FIFO is not a regular file:
`open` blocks until a writer appears.

Steps (Linux, `kv-linux` is the Linux build of commit `8ceed6b88b`, the log was
written by the independent writer):

```
mkfifo w/s/c/<id>/e1/tile/1/000.p/2          # replaces a tile the proof needs
timeout 20 kingres-verify consistency --trust trust.json \
    --log chain:<id>:e1 --source w/s --old old300.cp ; echo rc=$?
mkfifo w/s/c/<id>/e1/checkpoint
timeout 10 kingres-verify checkpoint --trust trust.json \
    --log chain:<id>:e1 --source w/s ; echo rc=$?
```

Observed: no output, `rc=124` (killed by `timeout`) for both. A directory made
by `tar -x` of an attacker's archive can contain a FIFO (the archive source is
step 7, but a directory the auditor unpacked is a source today).
Expected: refuse a source file that is not a regular file (status 2 or 1) and
never block.
Direction: `Stat` after `Open` (or `O_NONBLOCK` then `Stat`) and require
`Mode().IsRegular()`; for `os.DirFS` use `Lstat` first. Same helper for
`readLimited`.

### L7-02 Medium: `not_after` is reported, not enforced, and the report is lost in `--json`

Place: `internal/trust/trust.go`, `SignerKeys` (the key is added whether or not
`not_after` has passed; the warning goes to `notes`); `internal/cli/cli.go`,
`jsonOutput` has no field for notes; `internal/cli/cli_tlog.go`, `runTrustShow`.

The design correction 5 says it plainly and the README repeats it ("`not_after`
is reported, not enforced"). The consequence is that the one field in the trust
file that looks like a way to retire a key does not do it, and a user who
writes a revoked or leaked key's end date into `not_after` gets the opposite of
what the name says. The only warning is a `note:` line in the text output.

Steps (Windows or Linux, the trust file has the pinned key with
`"not_after":"2020-01-01T00:00:00Z"`, the checkpoint is correctly signed by
that key):

```
kingres-verify checkpoint --trust t_na.json --log chain:<id>:e1 --source pub
kingres-verify checkpoint --trust t_na.json --log chain:<id>:e1 --source pub --json
kingres-verify trust show --trust t_na.json
```

Observed: text output `VERIFIED: ...`, exit 0, with the line `note: key "k"
(34750f98bd59fcfc) is marked not_after 2020-01-01T00:00:00Z: its signatures
count only if the statement was made before that time, which a checkpoint
alone does not show`. With `--json`: `"verified": true`, exit 0, and no trace of
the note (the same for the `not_before` note "was not used" and for the line
that says the system clock was used). `trust show` prints `not after 2020-...`
without saying that it has no effect.
Expected: a retired key that cannot be told apart from a current one by the
data in the checkpoint should not look "verified" without a visible condition
in every output format; the trust file should have a way to say "do not accept
this key" that is enforced.
Direction: (1) add `notes` (or `warnings`) to the JSON output of every command;
(2) in `trust show`, say that `not_after` is informational; (3) consider a
separate member (for example `revoked: true` or `distrust_after_size`) that is
enforced, and a flag such as `--strict-validity` that fails on an expired key.

### L7-03 Medium: a stale checkpoint is "VERIFIED" and the list of limits does not say so

Place: `internal/cli/cli_tlog.go`, `runCheckpoint`, the `notProved` list.

The listed limits are: tiles not checked, the time is the signer's claim, a
split view, the trust file. Not listed: that the checkpoint may be an old one.
A publisher (or anyone between the auditor and the publisher) that serves an
older genuine checkpoint hides every block after it, which is a truncated
history, and `checkpoint` accepts it.

Steps: log of 700 entries, an older signed checkpoint of the same log at size
300 (`old300.cp`):

```
kingres-verify checkpoint --trust trust.json --log chain:<id>:e1 \
    --note old300.cp
```

Observed: `VERIFIED: the checkpoint of <origin> at size 300 is signed by a
trusted key.` exit 0, while the log has 700 entries. Expected by the README
text: nothing wrong is claimed, but a user reading "VERIFIED" and the four
"not proved" lines is not told that rollback is not checked. `--min-size`
exists and `consistency` against the last checkpoint the auditor kept does fail
("the log went back", reproduced with a log truncated to 4,000 against an old
checkpoint at 5,000).
Direction: add "that this is the latest checkpoint: an older genuine one is
accepted; keep the last checkpoint you verified and use consistency, or use
--min-size" to the `notProved` list of `checkpoint`, and make the README
section say that `checkpoint` alone gives no protection against rollback.

### L7-04 Medium: README overclaims on a shortened chain and does not say what is not detected

Place: `contrib/kingres_ledger/README.md`, section "Reading the chain for
publication", bullet "Damage": "... is an error `XX001`, never a silently
shortened answer, so a publisher that signs what it reads never signs a broken
chain."; the comment of `ledger_blocks_entries` in `kingres_ledger--0.3.sql`
("a range past the head ... gives no rows").

Steps (8 blocks, triggers disabled as in `ledger_head.sql`):

```
DELETE FROM kingres.ledger_blocks WHERE height = 7;     -- the head
SELECT count(*), max(height) FROM kingres.ledger_blocks_entries(0, 100);
SELECT height FROM kingres.ledger_head();
SELECT count(*) FROM kingres.ledger_verify();
```

Observed: 7 rows, last height 6; `ledger_head` height 6; no error from either.
`ledger_verify` does report one problem (`tx_block ... transaction refers to
block 7, which does not exist`), because the transaction rows of the deleted
block are still there. Deleting blocks 6 and 7 gives 6 rows and head 5, again
no error (the same on 18 and 19). The two new functions read only
`ledger_blocks`, so a shortened chain is not detectable by them: the publisher
can only detect it by comparing the head with what it has published (design 4.7
(3)), and `from_height` past the head gives an empty answer that looks like
"nothing new".
Expected: the README states the limit. It states the opposite for "a missing
block", which is true only for a gap in the range read.
Direction: rewrite the bullet: a gap, a changed field, a wrong link or hash
inside the pages that are read is an error; removing the last blocks is not
detected by these functions (`ledger_verify` and the digests held outside the
database detect it); the publisher must compare `ledger_head` with its own
record of what it published. Add the other limits of L7-08.

### L7-05 Low: symbolic links inside a source directory are followed

Place: `tlog.FSSource` over `os.DirFS` (documented by Go as following links).

Steps: replace the tile the proof needs by a link to `/etc/passwd` or
`/dev/zero`; run `consistency` as in L7-01.
Observed: `FAILED at step tiles: tile is malformed: tile/1/000.p/2 has 65
bytes, expected 64`, exit 1 (the read is bounded, `/dev/zero` is cut at
size + 1). The verifier reads a file outside the directory it was pointed at
and reports its length (and, for a missing target, exit 3). No write, no
content is echoed, so this is an oracle of existence and size only.
Direction: `Lstat` and refuse links (or resolve and require the target inside
the source), together with the regular-file check of L7-01.

### L7-06 Low: output and help claim more than was checked

Place: `cli_tlog.go`, `runConsistency` (help text, the step "tiles", the step
"consistency"); `README.md` of the verifier ("a tile of the wrong size or with
wrong content gives status 1").

1. The reader reads only the hashes on the proof path. In a log of 700 entries
   the level 1 tile `tile/1/000.p/2` has two hashes, and the second is not on
   the path of the proof from 300. Flipping one byte of it (offset 40):
   `VERIFIED`, exit 0; flipping a byte of the first hash (offset 5):
   `FAILED at step consistency`, exit 1. A tile "with wrong content" is not
   always status 1. The tile directory is not checked as a whole (only
   `not proved: ... this checks the tree hashes only` says something near).
2. When the sizes are equal, or the old size is 0, no tile is read, yet the
   output says `ok tiles proof of 0 hash(es) read from the tiles of size 700`
   and `ok consistency RFC 9162 2.1.4.2: both roots follow from the proof`. With
   the whole `tile/` directory deleted and equal sizes the result is still
   `VERIFIED` with those lines. The claim is true only in the sense that an
   empty proof is enough.
Direction: say "only the tiles needed for the proof are read" in the help and
README; print "no tiles needed" for the empty proof.

### L7-07 Low: status mapping and the empty trust file

- A source without `checkpoint` (a path that does not exist) is status 2 (input
  error), while a missing tile is status 3 (could not check). Both mean "the
  publication did not contain what was asked for". Reproduced with an empty
  `--source`: `open empty\c\<id>\e1\checkpoint: ... cannot find the path`, exit 2.
- `trust show` of `{"format":1,"origin_prefix":"a.com/x","keys":[]}` prints
  "trust file ... is valid" and exits 0; every verifying command then refuses it
  ("has no keys: nothing can be verified", exit 2).
Direction: decide the rule (an absent publication object is 3, or all are 2),
and make `trust show` warn or fail on an empty key list.

### L7-08 Low: what `ledger_head` and `ledger_blocks_entries` do not check

Place: `kingres_ledger--0.3.sql`, `ledger_head`, `ledger_blocks_entries`.

Reproduced (8 blocks, triggers disabled):

- `ledger_head` returns the stored `block_hash` and the stored `chain_id` with no
  check. Appending a block 8 whose `prev_hash` is right and `block_hash` is wrong:
  `ledger_head` answers height 8 and the wrong hash with no error, while
  `ledger_blocks_entries(7, ...)` raises `XX001 block 8 does not match its stored
  block hash`. A publisher that trusts the head and compares it with its own
  record would notice, one that only uses it as an upper bound would not.
- The chain identity is compared with the genesis hash only when
  `from_height = 0`. With the identity replaced, `entries(0,..)` raises
  `XX001` and `entries(3,..)` returns 5 rows; `ledger_head` returns the new
  identity.
- For `from_height > 0` the link into the range uses the stored `block_hash`
  of the block before the range, which is not recomputed. With `tx_count` of
  block 4 changed (its stored hash and block 5's `prev_hash` untouched),
  `entries(5, 100)` returns 3 rows without error; `entries(4, 1)` raises
  `XX001 block 4 does not match its stored block hash`.
- Message of a duplicated height (primary key made non-unique, block 4 copied):
  `XX001 block 5 is missing (found block 4)`. It is an error, but the text
  points at the wrong block.
- A fully valid successor written by someone who can write `ledger_blocks`
  (a block 8 with a correct hash and link) is returned as valid. It cannot be
  otherwise without a signature; the design relies on the digests and
  anchors outside the database. `ledger_verify` reported 1 problem.
Expected: the README says these. Direction: a sentence for each, and say that a
publisher that reads incrementally should re-read the last published block
(`from_height = published_size - 1`) on every round, as design 4.7 (1) already
asks.

### L7-09 Note: the extension's access model

- `ledger_blocks_entries` is `SECURITY INVOKER`; `ledger_head` is `SECURITY
  DEFINER`. Today `ledger_blocks`, `ledger_chain` and `ledger_table` are
  `SELECT` to PUBLIC, so a role that cannot execute the two functions can read
  the same data and compute the same preimages with the PUBLIC function
  `ledger_block_preimage`. The README says so correctly ("the publication is an
  operator's choice"). `EXECUTE` is a convenience gate, not confidentiality.
- A role without the grant gets `permission denied for function ledger_head`
  and the same text shape for the entries function; the message does not depend
  on the chain. No timing or error difference was found that depends on the
  data.
- Hardening is not possible with the statement the README implies:
  `REVOKE SELECT ON kingres.ledger_blocks FROM PUBLIC` fails with `ERROR: role
  "public" does not exist` (and so for `FROM public`); `GRANT SELECT ... TO
  PUBLIC` is accepted. The behaviour comes from the protection hook of 0.1 and
  0.2 (`kl_check_catalog_grant`), not from the reviewed commits. If it were
  possible, `ledger_blocks_entries` would stop working for the service role
  (it needs `SELECT` on `ledger_blocks` and `ledger_chain`), while
  `ledger_head` would keep working.
- After `pg_dump` and restore the three functions keep their ACLs
  (`GRANT ALL ... TO l7_svc` in the dump; `proacl` after the restore equals
  `{postgres=X/postgres,l7_svc=X/postgres}`), the restored ledger has no blocks
  (the documented limit) and `ledger_head` returns no row.

### L7-10 Note: exit status 3 and a hostile publisher

A publisher that has forked its log can withhold the tiles that would show it:
the verifier then ends with status 3 (INCOMPLETE), not 1 (reproduced with the
needed tile deleted, level 1 and level 0 partial tiles). It never ends with 0.
`tlog-tiles` says a log MUST serve the partial tiles for a checkpoint size, so
a missing tile is a violation by the log, and the "nothing failed" wording in
the output is generous. Scripts must treat every non-zero status as an alert.
The README says "Missing tiles give status 3"; a sentence that 3 must not be
read as "probably fine" would help.

## The thirteen corrections of section 14 of the design

Checked against the texts fetched during the review.

| # | Correction | Verdict |
|---|---|---|
| 1 | Consistency proof length: `ceil(log2 n) + 1`, not `2*ceil(log2 n)`; at most 65 accepted | Correction right: RFC 9162 2.1.4.1 says "bounded above by ceil(log2(n)) + 1". 65 is looser than the bound (63 for n < 2^62) but harmless: the algorithm itself enforces the exact length (a hash added or dropped at either end was rejected in all 105,343 mutations). |
| 2 | Extension lines accepted and reported | Correct: tlog-checkpoint allows non-empty lines after the third and says they are covered by Ed25519 log signatures. |
| 3 | Origin follows tlog-cosignature (255 bytes, no space, `+`, control character); prefix at most 160 | Correct: tlog-cosignature says logs are identified by strings "following the above construction" (the cosigner name rules). 160 + `/c/<64>/e<9 digits>` = 239 fits. |
| 4 | Note key name is the origin; `name` in the trust file is a label | Correct for what the specifications say: tlog-checkpoint, Ed25519 log signatures, "The key name SHOULD be the log origin". Only a SHOULD, so the verifier is stricter than the specification (a checkpoint signed under another name is "no signature of a trusted key"); that is the right choice for a log the auditor pins, and will need a witness policy later. |
| 5 | `not_before` enforced, `not_after` reported not enforced | Right that a checkpoint carries no time, so `not_after` cannot be applied to it, and nothing in the specifications contradicts it. The original design could not be implemented. But the result is a field that looks like a control and is not: see L7-02. |
| 6 | Malformed note under test is status 1; an unreadable `--old` note is status 2, a bad signature or origin of it is 1 | Reasonable and matches the code; consistent with `proof` for a malformed proof. Checked: malformed `--old` file gives 2, `--old` signed by an untrusted key gives 1. |
| 7 | Headline first | Neutral; consistent with the existing commands. |
| 8 | Two lines with the same name and key id rejected for every note | Correct for checkpoints (tlog-checkpoint: MUST NOT); signed-note does not require it for other notes; stricter, harmless. |
| 9 | A bad signature from a known key always rejects the note | Correct: signed-note says known-key signatures MUST be verified and the note SHOULD be rejected on failure. Reproduced with the trusted name and key id and the signature of another key. |
| 10 | Epoch in `--log` | A usability choice, safe: with more than one epoch directory the command refuses (reproduced, status 2). |
| 11 | Fall back to the full tile when a partial tile is missing | Correct: tlog-tiles lets a log delete a partial tile once the full tile exists and says clients MAY fetch the full tile. Reproduced (partial removed, full tile present: verified). |
| 12 | A proof that verifies for a size can verify for a neighbouring size with the same root | Correct and important; reproduced exhaustively: a path for `(first, second)` also verifies as `(first, second')` for sizes with the same shape (for example (1,3) as (1,4), (4,6) as (4,8)). Sizes must come from a signed checkpoint; every command does this. |
| 13 | Directory source only in step 1 | Fact; and it is the reason for L7-01 and L7-05 to be fixed before the archive and HTTP sources are added. |

## Examined and found sound

Signed notes and checkpoints:

- 300,000 mutated notes against `x/mod/sumdb/note`: never accepted where it
  rejects; the signed text is identical when both accept.
- A line with a trusted name and key id and a signature of another key rejects
  the note; unknown name or key id is ignored; a note whose only valid
  signature is by an untrusted key is "no signature of a trusted key"; two
  trusted keys with the same name and id and different keys are detected.
- Control characters other than newline, invalid UTF-8, CR, NUL, a missing
  final newline, a missing blank line, a doubled final blank line,
  non-canonical base64: refused (differential test). More than 32 signature
  lines, a signature shorter than the key id, a key name with a space or `+`:
  refused by the code and the authors' unit tests (read, not re-run by hand).
  The text of the note is parsed only after a signature verified; the origin is
  compared with the one the verifier builds, not with the one in the signature
  name.
- Checkpoint sizes with a sign, leading zeros, whitespace, 20 digits, 2^62,
  an empty line inside the text, a root of 31 or 33 bytes, size 0 with a wrong
  root: refused (unit tests and 3 minutes of fuzzing per target).
- A signed checkpoint of one chain presented under the path of another is
  rejected (the key name is the origin of the requested log).

Tiles and proofs:

- Path encoding `x001/x234/067` for large indexes, partial widths 1 to 255, a
  width of 256 as a full tile, no `..`, no absolute path, no leading zeros
  (fuzzed 8.6 million inputs); level and index outside the tree are errors.
- A tile that is 1 byte short or 1 byte long (63 and 65 bytes for a two-hash
  tile): status 1 with the size in the message; a tile of 4 GiB (sparse):
  bounded read.
- Exhaustive comparison of proofs and roots with an independent implementation
  (see "Scope and method"); equal sizes, first 0, first larger, an extra or
  missing hash, a proof for another tree: all as the code says.
- Memory and time: a 2^62 - 1 tree needs 29 tile reads; no recursion deeper than
  64; the reader caches tiles of at most 8 KiB.
- Trust file: duplicate member, duplicate key or fingerprint, trailing data,
  byte order mark, `1.0`, offset times, upper case fingerprint, unknown member,
  a 3 MB file, 1,000,000 nested brackets (nesting refused at 32): all refused
  with status 2 (a fingerprint that does not match the key by the unit tests). `trust init` refuses
  without `--fingerprint`, with a wrong one, and it never overwrites (created
  with `O_EXCL`, mode 0644); it refuses an existing file with a clear message.
- Output: errors that carry input are quoted with `%q` and clipped; no
  attacker string reached the terminal raw in any experiment; the headline of
  a verdict is built from the verifier's own strings.
- The three commands are separate from the older ones, which kept their
  behaviour (the suite of the older packages passes; `go vet` and `gofmt` are
  clean).

Extension:

- Layout of the 87 bytes against `ledger_block_hash` for every block of three
  chains (0.1 to 0.3, 0.2 to 0.3, fresh 0.3) on 18.7 and 19rc2; arguments
  refused as `ledger_block_hash` refuses them (version 0, 2, negative height or
  count, 31 or 33 bytes, infinite time); NULL gives NULL; hash format 1 is
  unchanged.
- Upgrade scripts and the fresh script give identical catalogs (see above), and
  the upgrade wrote nothing (the block, digest and entry counts are unchanged).
- Damage inside the read range is always an error: every field of block 4 changed
  one by one, a missing block, a renumbered block, a swapped pair, an appended
  block with a wrong link or a wrong hash, a duplicated height, a missing
  chain identity, the first block deleted. None returned rows (the function
  materialises its result, so a late error discards earlier rows; checked with
  `from 0` and `from 3`).
- Arguments: negative is `22023`, NULL and zero give no rows, `2^63 - 1` and a
  start past the head give no rows, 10,000 is the cap, `max_count = 2^31 - 1`
  works.
- `search_path` is fixed to `pg_catalog, pg_temp` on both read functions and
  every object name is qualified; the `SECURITY DEFINER` function reads only
  the two tables it needs.
- No lock that blocks: readers take `AccessShareLock`; a reader with an open
  snapshot did not delay a writer or the builder; the builder holding its
  locks did not delay a reader; 20 s of concurrent building and reading gave
  no error and no gap; on a hot standby the functions work and follow the
  primary.
- Privileges after a fresh install, after both upgrades and after a restore:
  `ledger_block_preimage` PUBLIC, the other two only the granted roles.

End to end (written by the independent writer, verified by the verifier):

- The 11,030 real entries of this extension were written as tiles and signed
  checkpoints at 5,000 and 11,030; `checkpoint` and `consistency` verified
  them. Failing as expected: a changed byte in entry 1,234 or 4,999 (status 1,
  step consistency), a deleted entry 2,000, two swapped entries, a log
  truncated to 4,000 (status 1, "the log went back"), a log forked at 4,999
  and regrown to 7,000, another key (status 1), another chain's origin. A
  change at entry 5,000 or 10,600 (after the old size) is a valid extension and
  is accepted, as it must be.
- With what the three functions return, a publisher has: the identity and the
  head (`ledger_head`), the leaves (`ledger_blocks_entries`), and the layout
  (`ledger_block_preimage`), which is what design 4.5 and 8.1 list. Two
  things are missing for the publisher and are not in the three functions:
  nothing returns the chain identity when there are no blocks (it can be read
  from `ledger_chain`, readable by PUBLIC), and nothing says "the head went back"
  (L7-04, the publisher compares).

## Not reproduced

- A way to get "verified" from a note with no valid trusted signature: none
  found (the differential test, the unit tests, 3 minutes of fuzzing per target).
- A key id collision between a trusted and an untrusted key with the same
  name: finding a 32-bit collision is cheap, but the result is a bad
  signature of a known key, so the note is rejected; checked by constructing
  the line, not by searching for a real collision.
- A remote way to make the verifier use unbounded memory: every read is bounded
  by a limit (1 MiB for notes and trust files, 8 KiB plus one byte for tiles).
- Terminal escape or a forged verdict line in the output: not found; a trust
  file may contain U+009B or U+202E in a label (`validLabel` allows them) and
  they are printed with `%q` in `trust show` and `%s` in the output of
  `trust init`; the trust file and its labels are the auditor's own input, so
  this was not rated.
- A torn read on the primary while blocks are built (a gap that is not
  damage): none seen in 575 reads over 516 builds; the argument in the README
  (the builder holds an exclusive lock on `ledger_chain` until commit and the
  rows of one block are visible at once) was not tested at a deeper level
  (for example with `pg_dump` snapshots or logical decoding).

## Not covered

- Key rotation events, anchors, the aggregate, attestations, bundles, archive and
  HTTP sources and `trust follow`: not built in these steps.
- Windows behaviour of special files (reserved device names, alternate data
  streams) in a source directory; only Linux file types were tried.
- `go test -race` (needs cgo, not available in the environment) and a fuzz run
  longer than 3 minutes per target.
- The C code of 0.3 other than by tests: `merkle.c` was read (the diff only),
  not run under a memory checker; the build used the PostgreSQL installations
  with assertions and `-Werror`.
- Performance of `ledger_blocks_entries` on a chain of millions of blocks; the
  10,000 cap and the plan were not measured beyond 11,030 blocks.
- The behaviour on a standby that lags (the answer is "what has been replayed",
  as the README says) and promotion; logical replication.
- The TAP tests of the authors were run, not read.

## Baseline runs

- Verifier, Windows, Go 1.27.3: `go test ./...` in `kingres/verifier`: 12
  packages with tests ok, 103 top-level tests, none failing (the number in the
  commit message); `go vet ./...` and `gofmt -l` clean. Linux build with
  `CGO_ENABLED=0`.
- Fuzz, 180 s per target, `GOMAXPROCS=2 -parallel 2`, no failure: signednote
  `FuzzParse` 3.3 million executions; checkpoint `FuzzVerify` 4.1 million;
  tlog `FuzzParseTilePath` 8.6 million; tlog `FuzzParseTile` 8.5 million; trust
  `FuzzParse` 2.1 million; cli `FuzzParseLogSpec` 3.1 million. (The author ran
  8 s each.)
- Extension, PostgreSQL 18.7 with assertions, out of tree with
  `kingres/dev/ext.sh kingres_ledger all`, `EXT_WERROR=1`, `-j2`, `KINGRES_VERIFY`
  set to the Linux build: 13 of 13 regression tests, 12 TAP files, 375 tests,
  all passed, no compiler warning.
- Extension, PostgreSQL 19rc2, same command: 13 of 13 regression tests, 12 TAP files, 374 tests, all passed (one test fewer than on 18, as the commit message also reports).
- Independent tests written for this review, all passing: consistency proofs
  of every pair up to 130 (105,343 mutations rejected, 0 accepted); readers
  and proofs for sizes up to 70,000 and 16.97 million leaves; sizes up to
  2^62 - 1; the note differential test (300,000 cases); the damage matrix
  (about 30 cases) on 18 and 19 with the same results; the argument matrix; the
  end-to-end cases above.
