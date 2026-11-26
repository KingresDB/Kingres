<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
# Independent review of the key and log libraries of kingresd

Reviewer: an independent reviewer. Date: 2026-11-18.

## Scope and method

Scope: `control-plane/internal/ledgerkeys/` (key file, key set and state machine, Signer, trust file) and
`control-plane/internal/ledgerlog/` (RFC 9162 tree in the C2SP tlog-tiles layout, frontier, Log, MemStore and
DirStore, Reader and Audit, signed-note checkpoints, leaf formats), read against the design
(`docs/design/ledger-phase7.md` sections 4, 5.2 and corrections 14, 17, 40 to 48, 55 to 59) and
against `kingres-verify` built from the export.

Method: I read all non-test source of both packages in full, then worked only in private copies of
`control-plane/` and `verifier/` (Linux and Windows), writing my own probe tests there. The export was not
touched. Both copies were removed at the end. The probe tests are not part of the repository.

## What I ran

| Run | Result |
|---|---|
| `go vet` on both packages (Linux) | clean |
| Default tests of both packages, Linux, with `KINGRES_VERIFY` set | pass: 87 top-level tests (85 pass, 2 skipped as long tests), 117 test entries counting subtests; 0.6 s and 3.3 s |
| Same, Windows (with a Windows build of the verifier) | pass, including the publication-vs-verifier tests |
| `KINGRES_LONG_TESTS=1`, ledgerlog, Linux | pass in 4.6 s, no skip, 270 MB peak |
| Own naive recursive Merkle tree (no package function used) against `Log`, `Reader`, `Audit`, `VerifyInclusion`, `VerifyConsistency`: 24 sizes from 1 to 1025 incl. 255/256/257/511/512/513, uneven batches, every sampled old size against every new size, root, audit path and consistency path compared hash by hash | all equal |
| Publication directories written by `DirStore`, uneven batches, sizes 0, 1, 2, 255, 256, 257, 511, 512, 513, 65535, 65536, 65537, one or two signature lines, key 1 retired and key 2 active, trust file from `TrustFile`; `kingres-verify checkpoint` on each, `consistency --old` from a one-third checkpoint signed by the old key; `Audit` on each (Linux and Windows) | all verified, status 0 |
| Single-bit damage (two masks) of every byte of the checkpoint of a 1-leaf and a 257-leaf log, run through `kingres-verify checkpoint` | every flip refused except flips inside the name or key id of one of two signature lines (170 of 980, 171 of 976), where the other line still verifies; that is the signed-note rule (unknown line ignored), not a defect. A flip in any signature byte, in the text, or in the dash or spacing is refused |
| Damage of 3 bytes of every file of a 257- and a 513-leaf publication, `Audit(final size)` | all found, except the older partial tiles and bundles (widths 85, 171, 23) that the final size does not read; expected |
| Crash matrix with `MemStore.BeforePut`: 7 appends (batches 1, 5, 250, 1, 300, 600, 3), a crash at put 0..11 of each, `Open` (expects `ErrTilesAhead` or success), `Open` with `AllowTilesAhead`, same leaves again, reopen after each round, `Audit` | all recover to the naive root |
| Crash at each of the `DirStore` hook steps (`temp-written`, `before-publish`, `after-publish`) on the first 6 occurrences, `RemoveTemporaryFiles`, recovery, `Audit` (Linux and Windows) | all recover |
| Delete each tile of a 65,843-leaf store, `Open` | right-edge tile gives `ErrFrontierAhead`; other tiles are not looked at, as documented |
| Store failing on call 1..39 of GetTile or Widths during `Open`, and on put 1..7 during `Append` | errors only; state unchanged after a failed `Append`; retry gives the right root |
| Two logs on one directory, `DirStore` against symlink, directory in place of a file, linked directory, oversize file, checkpoint as directory or link | covered below |
| Key file: every single-bit flip and every truncation of a sealed and of a plain file (478 cases), 11 hostile iteration values, wrong passphrase | no panic; none opens; all damaged sealed files refused or fail to open |
| `%v %+v %#v %s %x %d` of a struct holding `File`, `Passphrase`, `*FileSigner`, `FileSigner` | exported fields print a marker; see K1-08 for what stays in memory |
| Fuzz targets (10: key file, key state, note, checkpoint, tile, bundle, block entry, aggregate entry, frontier, tile path), 12 s each, `-parallel=2`, about 126 s in all | no crash |
| Hand-made mutants, 42 applied (2 more did not apply), default tests | 30 killed, 12 survived (see K1-12) |

## Summary

The arithmetic and the formats are right. Everything I made the packages write was accepted by
`kingres-verify` on Linux and on Windows, and every integrity-relevant damage was refused by the verifier or
found by `Audit`. The crash procedure of the doc works at every put of every append I tried. The key file parser
is strict and bounded (the 6,000,000 iteration cap costs 0.7 s here), nonce and salt are fresh on every write.

I found no critical or high defect. Two medium findings: a panic in `Reader` on a short tile from any
`TileGetter`, and a way for `Retire` to produce a trust file that the verifier refuses for every command. The
rest are low: a few unchecked invariants of the key state machine, durability and Windows details of
`DirStore`, a doc claim about zeroed secrets that is not true for the plain key file, and test gaps.

## Findings

### K1-01 (medium) Reader panics on a short full tile

`ledgerlog/reader.go:64`. When the partial tile asked for is `ErrNotFound`, `Reader.get` takes the first
`Width*32` bytes of the full tile with `fdata[:k.Width*HashSize]` without checking `len(fdata)`.
`MemStore` and `DirStore` validate length on read, so today it cannot trigger, but `Reader` takes any
`TileGetter` (the design plans a remote one for mirrors and monitors), and the claim of the doc is that parsers
are total.
Shown by probe (`TestRev2HostileGetter`): a getter that returns 31 bytes for the full tile and not-found for the
partial one makes `Reader.Root` panic with `slice bounds out of range [:320] with capacity 31`.
A fix must check `len(fdata) == TileWidth*HashSize` (or call `ParseTile` on the full tile) before slicing, and a
test must use a getter that lies.

### K1-02 (medium) Retire and Validate accept a max tree size that the verifier refuses

`ledgerkeys/state.go:332` (`Retire`) and `:240` (`Validate`). `maxTreeSize` is any `uint64`. The verifier reads
`max_tree_size` as an integer from 1 to 2^62-1. Shown (`TestRev3Retire`, then `kingres-verify trust show`):
`Retire(fp, now, 1<<62)` succeeds, `TrustFile` writes it, and the verifier ends with status 2: `max_tree_size:
4611686018427387904 is out of range`. Status 2 on the trust file means no checkpoint of the cluster can be
verified by anyone who takes the new file. The same holds for a `Key` loaded from storage with such a value,
because `Validate` does not look at the range.
A fix must refuse a `MaxTreeSize` of 2^62 or more in `Retire` and in `Validate` (and say 1 to 2^62-1), with a test
that writes the trust file and parses it with the verifier.

### K1-03 (low) DirStore.PutTile can replace a tile through its fallback

`ledgerlog/dirstore.go:214-222`. When `os.Link` fails with anything other than "exists" (a file system without hard
links, a permission error on the link call), the code re-reads the target and then calls `os.Rename(tmp, target)`.
Rename replaces. Between the read and the rename a second writer (or a stale process, see K1-13) can publish a
different tile, which is then silently overwritten, against "a published tile is never replaced". I did not force
a link failure (no portable way); this is by reading.
A fix must not use rename for a tile: use exclusive create (`O_EXCL`) of the final name followed by write and flush, or
fail closed with a clear error when hard links are not available.

### K1-04 (low) PutCheckpoint fails on Windows while a reader holds the file

`ledgerlog/dirstore.go:352-358`. The rename is retried 8 times with 20 ms sleeps (160 ms) only on Windows.
Shown on Windows (`TestRev2CheckpointWhileOpen`): with the checkpoint open by another handle opened with
`os.Open`, `PutCheckpoint` returns `rename ...: Access is denied` after the retries, leaving the old checkpoint.
(Linux: succeeds in 11 ms.) A static file server or `kingres-verify` that holds the file for a second makes a
round fail on Windows. The failure is clean (temp file removed, old note intact), so the caller can retry; the doc
should say that `PutCheckpoint` can fail for a reason that a retry cures, or the retry window should be longer and
configurable. A fix must define the behaviour (error type that a caller can test, documented retry) and test it.

### K1-05 (low) New directories are not flushed

`ledgerlog/dirstore.go:234` (`mkdirs`) and `:278` (`syncDir`). After `PutTile` only the directory that holds the
tile is flushed. A directory that `MkdirAll` just created (`tile/0/x001`, `tile/0/000.p`, `tile/entries`, the first
tile of a level) is a new entry in its parent, and the parent is not flushed. After a power loss the tile file can
be there and its directory entry gone while the caller has already persisted a frontier that needs it: the next
`Open` gives `ErrFrontierAhead`, which the doc says must not be extended. Detected, not silent, but it is an
unforced loss. Not tested (needs a power-cut model). A fix must flush the parent of every directory it created, up
to the root, and say so in the doc.

### K1-06 (low) Checkpoint.Text can write a text that ParseCheckpoint refuses

`ledgerlog/checkpoint.go:50-72`. The cap of 16 extension lines is applied to the number of strings, but a string
may contain a newline. Shown (`TestRev2CheckpointTextRoundtrip`): 10 extensions of `"a\nb"` give a text of 23 lines
that `Text` accepts and `ParseCheckpoint` refuses (`20 extension lines, expected at most 16`). The doc says
`Text` "refuses a checkpoint that ParseCheckpoint would refuse". `kingresd` writes none today, so it is a latent
error. A fix must refuse a newline inside an extension in `Text`, and a test must check `Text` followed by `Parse` for hostile extensions.

### K1-07 (low) Two keys with the same key id under one origin cannot sign together

`ledgerlog/note.go:117, 181, 252`. The note refuses a second line with the same name and 32-bit key id, and
`AddSignature` refuses a key whose id equals an earlier line, without comparing the public keys. The key id is
only 32 bits and its name is the origin of the log, so two keys of a set can collide for some log origins. I found
two seeds that collide under one origin after 79,697 random keys (birthday, as expected). `SignCheckpoint` with the
pair gives `the note is already signed by this key`, and a note written by hand with both lines is refused by
`Marshal`. Signed-note allows such a note (a reader tries every line with the id). For a set of at most 16 keys
and about 10^4 logs the odds of this for some log are roughly 10^-4, which makes a rotation overlap fail for one
log with an error that blames the wrong thing. It is a refusal, never a wrong signature. A fix must at least name
the cause (a key id collision, with the two fingerprints) and the doc must say what the operator does (activate
another key); comparing public keys instead of ids would let both lines in if the verifier takes them, which I
did not check.

### K1-08 (low) The doc says secrets are zeroed except in the signing library; the plain key file path keeps copies

`ledgerkeys/keyfile.go:209` (`marshalPlain`), `:340` (`parseObject`), doc.go "What it guarantees". Encode of a plain
file builds the base64 of the seed with `EncodeToString` and a string; `Parse` takes the JSON through
`encoding/json`, which makes the seed text a `string` (`member.str`). Strings cannot be zeroed, so the base64 text of
the seed stays on the heap until the collector reuses it. Shown by scanning the process memory after `Decode` and
`Close` of a plain file with its input zeroed: several copies of the base64 text remain (my own probe holds some,
so I do not give an exact number; `TestRev3SeedInMemory`, `TestRev5RawSecrets`). For the sealed file the passphrase
and the raw seed look zeroed apart from what the crypto library keeps (I could not separate those from my own
copies). The plain file is on disk in clear text anyway, so the practical impact is small, but the doc says
"the one place a secret cannot be zeroed", which is not true. A fix is either to state the plain-file path as a
second place, or to parse the plain form by hand from a byte slice that can be zeroed.

### K1-09 (low) Key set invariants that are not checked

`ledgerkeys/state.go:319, 332, 356, 383, 188`.
(a) `Retire` and `Revoke` test `otherActive` without looking at its activation time. Probe: A active, B activated
at t0+48 h, `Retire(A, t0+1h)` succeeds and `Signers(t0+1h)` is empty until B's time comes (clock stepped back or an
activation in the future). A fix must require the other key to have `ActivatedAt` at or before `now`.
(b) `Retire` does not compare `now` to the other key's overlap, and `Revoke` of the younger active key leaves the older
key with its `OverlapEnds` (stale; harmless because `DueForRetirement` needs another active key, but wrong state
to store).
(c) `Validate` accepts a set with no active key (all next, retired, or revoked); a loaded set that is damaged that way
is not noticed until signing is asked. `Signers` returns an empty slice, not an error; every caller must check.
(d) `Retire(fp, now, 0)` leaves the retired key in the trust file with no `max_tree_size` and no `not_after`, so a retired
key that leaks can sign any larger checkpoint for ever (the verifier cannot tell). This follows correction 14
and the doc ("max_tree_size is written when the caller gives one"), but the default should be loud:
the console and the daemon must always pass the maximum size or the operator must be told that 0 means unbounded.
(e) No way to remove a retired key, so after 16 keys no rotation is possible. The limit of 16 is the verifier's,
so removing a key also breaks old checkpoints; the doc should say which procedure applies (a new trust file
generation, re-signing every log, or a new cluster prefix).

### K1-10 (low) Unbounded work on a size or a file read by name

`ledgerlog/tilepath.go:154` (`Required`) builds a slice of size/256 keys plus the levels above; with a size near
2^62 it asks for about 2^54 entries (memory exhaustion). It is only called with trusted sizes today, but it is
exported and the doc says every input is limited. `ledgerlog/dirstore.go:428` (`LoadFrontierFile`) checks the size with
`Lstat` and then `os.ReadFile`, so a file that grows between the two calls is read whole; `readRegular`
(`dirstore.go:116`) does `Lstat` and then `Open` without `os.SameFile` (the key file reader does check it), so a
file swapped for a link between the two calls is followed. These need an attacker who can write the publication
directory, who can anyway damage tiles; the checks are cheap. A fix must cap `Required` (size below a limit that
fits memory, or an iterator), read the frontier through a limited reader, and compare `SameFile` after the open.

### K1-11 (note) Re-signing does not verify what it keeps

`ledgerlog/checkpoint.go:149-165`. `ResignCheckpoint` parses the note and adds signatures, but does not verify the
existing lines. If the stored note was damaged or forged, the new key's valid signature is attached to forged text.
The doc puts verification on the caller. A short sentence in the doc, or a variant that takes the trusted keys and
verifies first, would remove the trap.

### K1-12 (note) Test gaps (surviving mutants)

Mutants that the default tests do not kill:
- `Open` tolerance for a tile one entry ahead (`checkAhead`, `w <= have+1` instead of `w <= have`): no test crashes
  after a one-entry append.
- `checkAhead` looking only at tile `cur` and not `cur+1`, and only at level 0 and not the entry bundles: no test
  crashes with only the next tile or only a bundle written.
- the overflow guard in `ParseTilePath` (`n > (^0-v)/1000`).
- the link check of `noLinks` for a linked directory on the way (a file symlink is caught by the regular-file check,
  so the mutant lives): no test puts a symlinked `tile/0`.
- `os.SameFile` in `readPrivateFile`.
- `MaySign` refusing a key before its activation time, and `Retire` refusing a time before activation.
Equivalent or defence in depth (not counted as gaps): the size limit in `readRegular` (a tile is checked again),
the member counter in `parseObject`, the length test in `VerifyingSigner`, and the self-check of `FileSigner.Sign`
(needs fault injection).
Test quality otherwise is good: the tests compare against the verifier, against fixtures of a real server
(blocks.jsonl), and against an independent reference tree; the default run needs no network and finishes in
under 5 s, and the long tests (`KINGRES_LONG_TESTS=1`) add the 65,539 and 66,000 leaf cases, the 64 MiB batch and
the 13-log publication, all of which passed here in 4.6 s.

### K1-13 (note) Behaviour that is documented but worth stating

- Two writers on one directory: a second `Log` that appends different leaves of the same width gets `ErrConflict`; of
  another width it succeeds silently and the first writer then conflicts at its next append of that width
  (`TestRev2TwoLogs`). Detection is late but certain. The "one writer per log" rule is the only protection and
  the daemon must hold a lock (an advisory lock file in the directory would be cheap).
- A partial tile left by a crashed append that is never re-appended with the same leaves makes the log stop with
  `ErrConflict` at that width for ever. This is the divergence rule of 4.7 and the doc says so, but an operator who
  restores the database from a backup older than the tiles hits it.
- `Audit(size)` reads only the tiles of that size; older partial tiles (kept for older checkpoints) are checked only
  by `Audit` at their own size.
- A log appended in one batch has no partial tile for earlier sizes, so a reader at an earlier size cannot be served
  (the doc says: one append per checkpoint).
- Windows: key files are written without any ACL change (mode 0600 is not applied); symlink behaviour of `DirStore`
  could not be tested without the privilege; junctions were not tested.

## What held under attack

- Tree arithmetic, proofs and the tile layout: identical to a naive recursive implementation for 24 sizes and every
  sampled pair of sizes; RFC 9162 2.1.3.2 and 2.1.4.2 verification matches line by line; sizes near 2^62 and 2^64 give
  errors, never panics or wrong accepts (`RootFromInclusion`, `VerifyConsistency`, `Reader` near the limit, path index overflow).
- Everything written was accepted by `kingres-verify` on both systems, including a rotation (retired key 1, key 2, a
  note with two lines), size 0 and the sizes at tile boundaries; the trust file for one key is byte for byte what
  `kingres-verify trust init` writes, and the verifier reads a label with `&<>` (the package writes it unescaped).
- Crash and recovery: every put of every append, every DirStore step, store failures on any call; `Append` leaves memory
  unchanged on failure; identical tiles are not an error; different ones are `ErrConflict`; retrying with another
  batching of the same leaves works (`TestRev4RetryDifferentBatching`).
- DirStore refused: a symlink file, a linked directory (get, put and widths), a directory in place of a tile or of the
  checkpoint, a file over the size limit, a checkpoint that is a link.
- Key file: strict canonical parse, 478 damage cases and 11 iteration counts without a panic; a huge count is refused
  before any work; the largest allowed count costs 0.7 s; fresh salt and nonce per write; AAD covers magic, kdf,
  iterations and salt; wrong passphrase and altered file give the same error; the destroyed passphrase never turns a
  sealed write into a plain one.
- State machine: never a third active key, retire and revoke need another active key, a retired or revoked key cannot
  sign, 17th key refused, retired key written without `not_after`, revoked written with `revoked` and without
  `max_tree_size`.
- Key id and key name rules match signed-note (SHA-256 of name, newline, 0x01, key; no space, no plus).

## Verdict

The two libraries do what the doc says for every case I could build against the verifier, and I found nothing that
makes the verifier accept damage or that breaks the crash procedure. Fix K1-01 and K1-02 before anything calls these
packages; the low findings can be done with the wiring step (K1-03 and K1-05 are the ones that touch the guarantee
"a published tile is never replaced and is durable").

## What I did not check

- Real power loss and file system behaviour after it (K1-05 is by reading); behaviour on network file systems, FAT, or a
  file system without hard links (the fallback of K1-03 was not run).
- Windows symlinks, junctions, ACLs and `\\?\` paths; macOS.
- A remote or HTTP `TileGetter`; external signers (program, HSM, KMS) behind `Signer`.
- Concurrent use of one `Log` and `Reader` under the race detector; cancellation of a context in the middle of a large
  `Append` beyond the single-call tests.
- Whether the verifier accepts two signature lines with the same name and key id (K1-07); `consistency --old` against
  every size pair (only a one-third checkpoint was used); `proof` and bundle commands of the verifier.
- Fuzzing beyond about 12 seconds per target; no corpus was reused from the repository.
- Timing side channels in the key code, the PBKDF2 implementation against crypto/pbkdf2 (the package test does it; I
  did not repeat it), and the contents of the Go runtime's own copies of keys.
- Design text outside the sections named in the task, and the SQL or C parts of the extension.
