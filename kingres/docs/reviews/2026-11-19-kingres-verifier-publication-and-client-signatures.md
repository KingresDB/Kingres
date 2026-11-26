<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
# Review of kingres-verify, phase 7 steps 7 and 18 (eighth review of the verifier)

## 1. Scope and method

Reviewed: the exported tree of the commit "a publication, the aggregate, bundles, a monitor and client signatures", the Go code of
`kingres/verifier` (all non-test files of the diff were read; the larger ones twice), its README, the design sections named in the brief
(formats, trust file and key events, aggregate leaf, bundles, exit statuses, client signatures, corrections 160 to 186), and
`contrib/kingres_ledger` for the client-signature rules. No file of the tree was changed. All my tests were written as separate test files in
a copy of the verifier (they use the tree's own test helpers, `world`, `newSigner`, `signText`, `makeChain`, which build whole publications
from the documents) or as standalone programs. Findings are numbered V8-nn; the extension has its own section (L23-nn).

Conventions: "run" means I executed it and report its output; "read" means I only read the code.

What I did not take over from the earlier reports: byte flips of the state file, the lock, the output layer (status 2 on unwritable output)
and the older commands were not re-examined except where this commit touched them.

## 2. What was run

| Run | Windows | Linux (Ubuntu 24.04, Go 1.27.3) |
|---|---|---|
| `go test -count=1 ./...` of the verifier | 27 of 27 packages ok (26 with tests, 1 without), 431 top-level Test and Fuzz functions listed | same: rc 0, all packages ok |
| `go vet ./...` | not run | clean |
| `go test -race -count=1 ./...` | not run | 0 data races (`grep -c "DATA RACE"` = 0); the only failures are the two known resident-memory tests (`TestPeakResidentSetOfACommandOnAHostileFile`, `TestPeakResidentSetOfAProofDoesNotGrowWithItsChain`) |
| every bit of every file of a whole bundle (events, proof, 3 chains, aggregate with a retired chain), 8 flips per byte | not run | 52,344 mutations of 19 files: every one fails except `bundle.json` (49 positions, an index that is not believed, by design) and 825 positions in the 3 event files, see V8-02 |
| Ed25519 differential: Go (`attest.Verify`) against OpenSSL 3.0.13 `EVP_DigestVerify` behind the extension's own C key check (`ed25519_check.c` compiled standalone) | not run | 708 signature vectors (good; mixed-order A and R with k mod 8 from 0 to 7; all 8 x 8 small-order A and R with S = 0, 1, 2; S+L, S=L, S with high bits; non-canonical R for y = 0..4, 18 and their S=1 forms; R with the sign bit on x = 0; R = identity legit) and 4,637 key vectors (all 8 torsion points with both sign bits, y = 0..39 and p+0..p+39 with both sign bits, 4,500 random and random-plus-torsion): 0 differences in either set |
| `edkey.Check` against my own big-integer reference (canonical, on curve, [8]P not the identity) | not run | fuzzed 60 s, 1,547,056 inputs, no difference |
| merkle property test against my own recursive RFC 9162 implementation | not run | 1,500 random trees (1 to 600 leaves): 30,656 positive checks (every leaf path and consistency proofs), 217,060 negative checks (wrong index, index >= size, truncated and extended paths, first hash dropped, node presented as leaf, swapped roots, extended consistency proofs): all as expected |
| fuzzing, 60 s each, `-parallel 2` (11 minutes in all) | not run | aggregate leaf, bundle index, event, tar source, trust file, signed note, tile, jsonx, state-from-note, plus two of mine (attestation and revocation columns, `edkey` against reference): no failure |
| tar attacks (PAX path, symlink, hard link, FIFO, duplicate, absolute, empty name, `a/../b`, `./.`, backslash, colon, GNU sparse, file and directory) through `bundle verify` | not run | all refused (status 2) with a message; none read or written outside |
| HTTP attacks against a server on 127.0.0.1 (302, 307 to `file:`, 206, a 3 GB gzip body, an endless body, notes with 100 Kbyte names), IPv6, `localhost.`, `127.1`, `0x7f.0.0.1`, `0.0.0.0`, `::ffff:127.0.0.1`, user-info, `%2e%2e`, `ftp:` | not run | all as designed: redirects status 2 and never followed, 206 refused, the gzip body and the endless body stop at the 64 KiB limit in 7 ms with 16 MB of memory, non-loopback http refused; `[::1]`, `LOCALHOST`, `[::ffff:127.0.0.1]` accepted (loopback) |
| 29 mutants of the new code | not run | table in section 5 |
| extension: three named mutants, TAP tests 032, 036, 037, 038, 039 | not run | section 4 |

## 3. Findings

No finding makes the verifier print VERIFIED for a checkpoint, tile, proof or signature that is wrong. The cryptographic and structural
paths (note, checkpoint, tiles, inclusion and consistency proofs, aggregate leaf, member-to-chain binding, attestation signature) held under
every attack I made. The findings are about what the tool leaves out of a "VERIFIED" for evidence that carries its own warning, and about
two bounds.

### V8-01 (medium) A verified key-revoked event for the signing key changes nothing in `checkpoint`, `aggregate`, `proof`, `bundle verify` and `monitor`

Place: `cli/cli_pub.go:194` (`events()` is read only where a chain-retired event is needed), `cli/cli_bundle.go:347`, `cli/cli_monitor.go:347`;
only `trust follow` (`cli/cli_trustfollow.go:99-110`) turns a key event into a trust file.

Steps (run): publication of two chains and an aggregate signed by key K1; events 000000 key-added K1, 000001 key-added K2 (signed by K1 and K2),
000002 key-revoked K1 (signed by K2, compromised-since earlier than the checkpoints). Trust file pins K1 only.
`aggregate`: status 0, "VERIFIED: ... is signed by a trusted key". `checkpoint`: status 0. `bundle verify`: status 0, with the line
"ok events 3 event(s) read and verified under the trust file, 0 chain(s) retired". `monitor --once`: status 0, "ok poll 1 events 3 event(s)
read, none differs". No line mentions that a verified event of the same publication revokes the key whose signature was just accepted.
`trust follow` on the same input writes K1 as revoked and says so.

Why it matters: an auditor who runs the commands the README lists for the usual check gets VERIFIED on checkpoints signed by a key that the
publication itself, through events the program verified, calls compromised. The README says to start from a file you pinned yourself and
describes `trust follow`, but nowhere says that the other commands ignore key events; `bundle verify` and `monitor` print "events read and
verified" and so suggest the opposite.

A fix must achieve: a command that reads and verifies the events must not stay silent about a verified key-revoked (or key-retired, with a
size below the checkpoint) event that concerns a key it just accepted a signature from; at least a note and a distinct status, or a refusal,
and the README must say what each command does with key events.

### V8-02 (medium) `bundle verify` counts unauthorised events as "read and verified" and never lists the skipped ones

Place: `cli/cli_bundle.go:347-349` prints `w.Count` events as verified; the tolerant walk (`events/events.go:576`, `skip`) records the events no
authorised key signed in `w.Skipped`, and only `monitor` (`cli/cli_monitor.go:378`) prints them.

Steps (run): in the bundle above change one character of the key name of the signature line of `events/000000` ("ledger.example.com/kingres"
to "dedger.example.com/kingres"). `bundle verify`: status 0, VERIFIED, "ok events 3 event(s) read and verified under the trust file".
In the bit-flip run 825 positions of the three event files behave this way (name part of the signature line). A key-added event that is
skipped this way leaves its key unknown to the walk, and a chain-retired event that is skipped makes a member's `retired` flag unexplained, yet
the line says "verified".

Why it matters: the bundle line is the only statement about events, and it is false in its plain reading ("3 verified") for a skipped event;
an auditor of a bundle cannot tell a tampered or forged event from a good one.

A fix must achieve: the number of events that no authorised key signed is part of the output of `bundle verify` (and of `aggregate` when it
reads events), the word "verified" is used only for events that were verified, and a skipped event that is needed (for a retired member) is
already a failure today and stays one.

### V8-03 (medium) `bundle fetch` without `--trust` writes without bound, driven by an unsigned checkpoint

Place: `cli/cli_bundle.go:626-687` (`fetchTiles`), `cli/cli_bundle.go:461-482` (`get`); the size comes from the unverified note at
`cli/cli_bundle.go:588-597`.

Steps (run): a server on 127.0.0.1 serves a checkpoint note of size 1,000,000,000,000 signed by a random key, and answers every tile and entry
bundle request with 8 KiB or 20 KB of zeros (stopped after 3,000 requests by my server). `bundle fetch --source http://127.0.0.1:PORT --out
DIR --log chain:ID:e1` (no `--trust`) wrote 3,001 files, 24,576,351 bytes, and went on asking until the server said 404 (status 3). There is no
limit on the number of files, on the total bytes, or on the size from an unverified note (the 60 s limit is per request). A hostile server can
fill the auditor's disk.

Why it matters: the verifier's own rule is that hostile input must not exhaust resources; the other readers (archive, HTTP read, state) all
have limits.

A fix must achieve: without a verified checkpoint, a ceiling on the size, files and bytes written (or refuse to fetch tiles without
`--trust`), and with `--trust` a ceiling that the auditor can set and that is reported when reached.

### V8-04 (medium) A member flagged diverged and retired (0x06) without a chain-retired event ends with status 3, not 1

Place: `cli/cli_aggregate.go:131-143`: the `case m.Retired()` branch calls `checkRetired`, which returns "incomplete" and ends the loop before
the diverged flag is recorded (`cli/cli_aggregate.go:140`).

Steps (run): aggregate of one round, one member with flags 0x06, chain log honest. No events: status 3 "INCOMPLETE". Event 000000 key-added
only: status 3. An event 000001 chain-retired signed by an unauthorised key: status 1 (step chain-retired), not step diverged. With a matching
authorised event: status 1, step diverged. Flags 0x04, 0x05 and 0x07 give status 1 at step diverged in every case.

Why it matters: the current publisher writes 0x06 for a diverged chain that was retired (design correction 176), and correction 162 and the
README say that diverged is status 1 so that a script is not misled and that a failure wins over an incomplete check. Here the
publisher's own "this chain was rewritten" is reported as "could not check" whenever the retirement event is absent or not yet served.

A fix must achieve: the diverged flag is judged before, and independently of, the retired-event check, so that 0x06 is a failure at step
diverged whatever events are served.

### V8-05 (low) Flags stale and retired together: stale is dropped and the status is 0

Place: `cli/cli_aggregate.go:132-139` (`switch { case m.Retired(): ... case m.Stale(): ... }`).

Steps (run): one member, flags 0x01: status 3. Flags 0x03 with a matching authorised chain-retired event: status 0, VERIFIED, no stale note.

Why it matters: correction 162 says stale is status 3; with the retired flag the head is also confirmed by the event, so the loss is small, but the
combination is not mentioned in the README or the design.

A fix must achieve: either the stale flag is still reported (status 3 or a note), or the README says that a retired member confirmed by an event
is not stale.

### V8-06 (low, read) `bundle fetch` replaces what it already holds without looking

Place: `cli/cli_bundle.go:461-482`.

`get` always replaces `checkpoint` and events of the output directory by rename, with no comparison with the file it replaces; for tiles it
skips a file that has the expected size without reading it (`skipIfSize`). A mirror directory that holds an earlier, signed checkpoint loses it
when a server shows a smaller or different one (a rollback or fork leaves no evidence), and without `--trust` a stale or altered tile of the right
size is kept and reported as "FETCHED" (the output does say nothing was verified).

A fix must achieve: before replacing a checkpoint or event the program compares it with the one it holds and refuses or keeps both when the new
one does not extend the old, and a tile that is kept without being read is not counted as fetched.

### V8-07 (low, run) A missing or emptied `monitor.json` is taken for a first run

Place: `cli/cli_monitor.go:300-317`.

Steps (run): after one poll the state directory holds `monitor.json` and the log state file. Replace `monitor.json` by `{"format":1}`: status 0
(the decoder keeps the pre-made empty maps, so the "incomplete" check never fires); a deleted file is read as new (`ErrNotExist`). Event digests,
audited positions and the list of retired chains are forgotten with no word, while the log state file remains and is checked. Other damage
(empty, garbage, truncated, future format, 5 MB of spaces) is refused with status 2, as is a damaged log state file.

A fix must achieve: a state directory that holds log states but no valid monitor record is reported, and an emptied record is told from a
first run.

### V8-08 (low, read) The attestation row's own entry is not tied to the transaction, and an insert is not required

Place: `cli/cli_attestation.go:121-139`.

The row of the attestation is proven with `Expect{TableID, RowHash}` only: its operation may be D, and its own transaction id is not compared with the
`ledger_txid` column or with the entry's. The checks that bind it are the column values (txid and seq equal to the entry's), the same block
(`cli_attestation.go:132`), and the signature, whose 136 bytes cover chain, table, operation, row hash, key id and nonce, but not the transaction.
Two inserts of the same row content in one chain share row hash, table and operation, so one signature fits both; an operator who can insert
into the attestation table (a role that bypasses the extension) can show a signature for a second, identical insert. The README says only
that the client "approved the server's claim".

A fix must achieve: require the attestation row's entry to be an insert in the transaction its column names, and say in the README that the
signature is over content and not over a transaction.

### V8-09 (low, run) `aggregate` verifies a retired chain whose own signed log goes on, and says nothing

Place: `cli/cli_aggregate.go:248-266` (`checkRetired`), versus `cli/cli_monitor.go` (rule `retired-continues`).

Steps (run): a chain of 5 blocks, a round in which the member has height 2 and flag retired, and a verified chain-retired event for height 2.
`aggregate`: status 0, "ok chain-retired ... names the head of the member", and the program has just read the signed checkpoint of that chain
(size 5). `monitor` reports "retired-continues" with status 1 on the same data.

A fix must achieve: `aggregate` (and the member check of `bundle verify`) uses the chain checkpoint it already holds: a retired member whose signed
log is longer than final-height plus one is a failure at step chain-retired.

### V8-10 (note, read) `proof --log-leg aggregate` does not use the stale information of the member

`cli/cli_proofleg.go:185` calls `verifyAggregate` and discards `rep.incomplete`; a member flagged stale gives status 0 for the aggregate leg. The
statement proved (the block is in the chain log of the size the member names) stays true, but `aggregate` gives status 3 for the same member.

### V8-11 (note, run) Events after a gap are ignored without a word

A directory source with `events/000000` to `000002` and `000004` (no `000003`): `trust follow` status 0, "3 event(s) read and verified". The README
says a withheld event cannot be told from the end; for a directory or tar source the numbers could be listed and a later file noticed.

### V8-12 (note, run) Consistency from size 0 accepts any second root

`merkle.VerifyConsistency(0, n, empty root, any root, nil)` returns nil. This is the mathematical truth for an empty log and no caller can reach it
with a signed first checkpoint of size 0 other than through `--old 0:ROOT`; I mention it because a script comparing "consistent" for a held size-0
state proves nothing.

### Not reproduced or not found

- Soundness (question 1): no mutation of any byte of the checkpoint, tile or entry files, truncation, swap, wrong origin, wrong epoch directory, chain id swap,
  height beyond the signed size, hash flip of the head block, extra unknown signature, duplicate signature line, CR LF, missing final newline, text after
  the signatures, non-canonical base64, empty or huge signature name, plus sign or leading zero in the size, NUL in the text, blank line before the signature: all
  refused except the extra unknown signature, which is accepted by design when a trusted signature is also present (run, 21 note variants).
- Trust chain (question 2, run): a key that adds itself, add without the new key's signature, a key revoking itself, a key retiring itself, an event signed
  by a retired key after its retirement, an event signed by a revoked key after its revocation (all refused in `trust follow`; skipped and then "not authorised"
  in `aggregate`). Which keys vouch for a size: the file's keys that are usable at `--now`, with `max_tree_size` applied by `LimitFor`; a key revoked in the file
  is never used (read, and mutant M03 and the tests killed it). A key-added event of a pinned key that only a revoked pinned key signed is accepted and adds nothing
  (run, harmless).
- Aggregate leaf (question 3, run): flags 0..7, 8 and 0x80; out-of-order, duplicate and member-count mismatches are covered by the tree's tests and by mutants M14 to M16.
  Members were bound to the right chain log in every swap I tried (another epoch directory: `no signature of a trusted key`; another chain's root: `member-root`).
- Sources (question 6): see section 2. `bundle fetch` writes only below `--out` through `os.Root` with names made of numbers (read); I found no way out of the directory.
- Output (question 8): hostile notes (control characters, bidi marks, invalid UTF-8, 3,000 characters) in `bundle.json` are refused at parse time and never reach the terminal;
  `--json` output stayed valid JSON in the cases I tried. I did not sweep every command for hostile names.

## 4. The ledger extension (short check)

Build: a private copy of the 19rc2 build, `kingres/dev/ext.sh` with `EXT_SRC` pointing to a copy of the extension source per mutant, `PROVE_TESTS` set
to `t/032`, `t/036`, `t/037`, `t/038`, `t/039`, ports restricted to 58400 to 58499 by editing my private copy of the Perl harness. Baseline (unmutated): 746 tests, all pass.

| Mutant | One line | Result |
|---|---|---|
| m4 | `capture.c:204` reset of the table cache at the abort of a subtransaction removed | killed by `t/036` (33 of 245 fail: 36-37, 40-41, 44-45, ...); `t/037` passes (all 250 under the mutant) |
| m5a | `sign.c:819` date of the switch later by 1000000 | killed by `t/036` (4 of 250: 191, 192, 199, 250) |
| m9 | `ed25519_check.c:409` a refused key is remembered as acceptable (`if (1)`) | killed by `t/038` (9 of 84: 67-70, 73-76, 81) |

Cache bound (run, standalone harness around the same C file): 60,000 keys (25,750 acceptable, 34,250 refused, with small-order keys mixed in), each judged twice
through `kl_ed25519_key_defect_cached`: 0 disagreements with the plain judgment, no refused key ever returned as acceptable, resident memory 1,568 KB
for the whole process (the cache is 8,192 static slots; it empties at 4,096 acceptable keys). I did not run the 4,096-key session through SQL.

L23-01 (note): the fix round's new test `t/037` (savepoints, written for m4) does not kill m4; `t/036` kills it because its cases include rolled-back subtransactions. No change
needed, but the test that was meant to guard that line does not.

No other finding in the extension.

## 5. Mutants of the new code (29; each run against the packages named, the first failing test is shown)

| Mutant | Change | Result |
|---|---|---|
| M01 | signature check removed in the first note verification (`signednote.go:262`) | killed: `TestSpecVector` |
| M02 | origin comparison of the checkpoint removed (`checkpoint.go:168`) | killed: `TestVerify` |
| M03 | `max_tree_size` of a key ignored (`cli_tlog.go:319`) | killed: `TestKeyLimitedToATreeSize` |
| M04 | partial tile of an older size: hashes not compared (`cli_audit.go:285`) | killed: `TestBundleVerifyRejectsEveryByteOfEveryFile` |
| M04b | partial entry bundle of an older size: entries not compared | killed: same test |
| M05 | member root not compared with the chain log's root | killed: `TestAggregateRefusesAMemberWhoseRootIsNotTheChainLogs` |
| M05b | member root not shown a prefix of the signed chain log (`VerifyConsistency` of the member) | **survives** the whole suite (356 s). Redundant: the audit path of the head block (the last leaf of the prefix) already covers every left subtree of the prefix, so a tile set that is not the signed history fails at `inclusion` first. I could not build an input that only this check catches |
| M06 | retired member: event's final height and block hash not compared | killed: `TestAggregateRetiredMemberNeedsAnAuthorisedEvent` |
| M07 | revocation rule `<=` to `<` (same block) | killed: `TestTheRevocationRuleIsByBlockHeightAndTheSameBlockCountsAsAfter` |
| M08 | small-order test of `edkey` removed | killed: `TestRefusedKeysWithTheReasonOfTheExtension` |
| M09 | redirects followed | killed: `TestHTTPStatusesAndLimits` |
| M10 | HTTP read limit not enforced | killed: `TestHTTPStatusesAndLimits` |
| M11 | key-retired signed only by the key itself allowed | killed: `TestTrustFollowRefusesWhatNoAuthorisedKeySigned` |
| M12 | key-added without the new key's own signature allowed | killed: same test |
| M13 | a key revoked by an earlier event still authorises | killed: `TestTrustFollowAKeyThatWasRevokedVouchesForNothingAfterIt` |
| M14 | aggregate members out of order accepted | killed: `TestRefusals` |
| M15 | undefined flag bits accepted | killed: `TestRefusals` |
| M16 | aggregate seq equal to the leaf index not checked | killed: `TestAggregateRefusesALeafWhoseSequenceIsNotItsIndex` |
| M17 | tar: repeated name accepted | killed: `TestTarRefusesWhatAPublicationNeverHolds` |
| M18 | tar: name of a regular file not cleaned | killed: same test |
| M19 | attestation row's transaction and entry not compared with the entry's | killed: `TestAttestationRefusesWhatTheSignatureDoesNotCover` |
| M20 | attestation row not required to be in the entry's block (`cli_attestation.go:132`) | **survives** (177 s). Not redundant: the column check does not constrain where the attestation row sits. A test is missing (an attestation row proof in another block of the same chain) |
| M21 | unpinned client key not refused at the lookup | killed: `TestAttestationRefusesAKeyThatIsNotPinnedAndAWrongExpectedKey` |
| M22 | diverged member not a failure | killed: `TestAggregateRefusesADivergedMemberAndReportsAStaleOne` |
| M23 | monitor: a changed event text not reported | killed: `TestMonitorReportsAnEventNumberSeenTwiceWithDifferentText` |
| M24 | inclusion path too short not refused (`merkle.go:112`) | **survives** (201 s). Redundant by the root comparison that follows: a short path cannot reach the signed root; it is a defence in depth against second-preimage shapes. My property test (truncated paths) passes only with the check or the root comparison present, so no test of the tree fails if it goes |
| M25 | proof leg: block above the signed size not refused | killed: `TestBundleVerifyChecksProofsOnBothLegsAndRefusesATamperedOne` |
| M26 | bundle index size not compared with the signed size | killed: `TestBundleVerifyRefusesALyingIndexAndAMissingOne` |
| M27 | key-added: fingerprint line not compared | killed: `TestTrustFollowRefusesWhatNoAuthorisedKeySigned` |

(M04b and M05b first did not compile; I rebuilt them with `&& false`. 26 of 29 are killed, 3 survive: M05b and M24 are redundant, M20 is a missing test.)

## 6. Documentation (question 12)

The README is exact about what it does not prove for the logs ("no anchor is checked", "no revocation shown in the evidence given" is never turned into "none exists", the statuses table matches the code through
`TestReadmeTableOfStatusesIsTheTableOfTheCode`). Gaps:

- It does not say that `checkpoint`, `aggregate`, `proof`, `bundle verify` and `monitor` do not apply key events (V8-01) while the last two print "events read and verified".
- It says (aggregate) that stale is status 3, without the retired and stale combination (V8-05); it says a diverged member is a failure, which is not true for 0x06 without an event (V8-04).
- Under `bundle fetch` it says "Names are made from numbers only", true, but not that nothing bounds what an unsigned checkpoint makes it write (V8-03) or that it replaces what it holds (V8-06).
- The attestation section does not say that the signature does not cover the transaction (V8-08), nor that a copy of the database, which shares the chain identity (the extension README), is told apart from the
  original only by the signed chain log of the publication the auditor was given.
- Newer publisher features (`logs.json`, anchors): the verifier does not read `logs.json`; `bundle verify` searches the layout for logs and verifies the ones it finds, `bundle fetch` needs `--log`. Anchors listed in a bundle give status 3, flag 0x06 is decoded as retired and diverged. Nothing it does not understand is accepted as good (run for the flags; read for the rest).

## 7. What I did not examine

- Two monitors on one state directory at once, and the kill-at-random loop of a few hundred polls (question 7): not run. Only corrupt state files and the monitor record were tested (V8-07).
- Windows-only matters (case collisions, reserved names such as NUL and CON as directory entries, alternate data streams in a directory source): the tar path with such names was run on Linux; a directory source on Windows was not.
- Symbolic links, FIFOs and devices inside a directory source: not run in this review (the tree has `cli_fifo_unix_test.go`, which passes).
- A single hostile file of 1 GB and a publication of hundreds of thousands of entries (question 10): not built; the limits were read (`MaxArchiveFile` 1 GiB read through a limit reader, 64 KiB events, per-file limits in the fetcher).
- HTTP/2, a slow body up to the 60 s limit, TLS peers: not run.
- The README's long history sections (L8 to L18) and the older commands.
- JSON validity of every command for hostile input; terminal escapes in the commands other than `bundle verify`.
- The extension beyond the three mutants and the cache: not reviewed.
- Windows `go vet`, and the race run on Windows.