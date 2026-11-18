<!-- SPDX-License-Identifier: AGPL-3.0-only -->
# Second independent review of the key and log libraries of kingresd

Reviewer: an independent reviewer. Date: 2026-11-18.

## Scope and method

Scope: `control-plane/internal/ledgerkeys/` and `control-plane/internal/ledgerlog/` after the first fix round, read against
the first review (K1-01 to K1-13), the package `doc.go` files, `docs/design/ledger-phase7.md` (sections 3, 4 and corrections
14, 17, 40 to 48, 55 to 59, 93, 94) and `kingres-verify` built from the export (a separate module).

Method: I read all non-test source of both packages and the parts of the verifier that decide key handling (`trust`,
`signednote`, `checkpoint`, `cli`). I worked only in private copies (Linux and Windows), wrote my own probe tests there
(an independent model of the key state machine, a naive Merkle tree of my own, a birthday search for key id collisions,
hook-driven crash matrices), and used one-line mutants of my own to test the tests. The export was not touched; all copies
were removed at the end. The probes are not part of the repository.

An incident during the review, reported to the coordinator and repeated here for the record: one of my mutants (K1-10a,
the check `size > MaxRequiredSize` in `Required` replaced by `false`) was run against the existing test
`TestRequiredIsBounded`, which calls `Required(1<<40)` and `Required(1<<62)`. With the bound removed the function builds a
list of size/256 keys (2^32 keys at 1<<40, 24 bytes each), so the test binary grew to 30.9 GB and the kernel killed it. That
allocation was caused by my mutant, not by production code: the shipped `Required` refuses every size above 2^28 with
`ErrTooLarge` before it allocates (measured on the shipped code: `Required(2^28)` lists 2,101,265 keys, takes 71 ms and
allocates 257 MB in total, because the slice grows by doubling; the list itself is about 50 MB). The size is linear in
the argument, so the 1<<40 case is 4096 times that. I did not reproduce it. Every later run of mine ran one test binary at
a time, under `timeout`, with a guard goroutine in the test binary that exits when the runtime has more than 2 GiB mapped,
and everything that was running when the machine was restarted was run again (below). No other exported function of the
packages allocated or looped in proportion to a size I gave it (see "What held").

## What I ran

| Run | Result |
|---|---|
| `go vet`, both packages (Linux) | clean |
| Default tests of both packages, Linux, `KINGRES_VERIFY` set | 121 passing entries (top-level tests and subtests), 0 failures, 3 skips (`TestPutCheckpointGivesUpAfterTheDefaultRetry`, `TestLargerTreeAcrossTileLevels`, `TestMaxBatchBytesBoundary`: all long tests); ledgerlog 5.7 s, ledgerkeys about 14 s |
| Default tests, Windows (own Windows build of the verifier) | 117 passing, 0 failures, 6 skips (the three above, the two `os.SameFile` swap tests, the symlink test: no privilege). ledgerlog took 134 s, of which `TestPublicationAgainstTheVerifier` 114 s (many verifier processes); the doc says the default run is under 30 s |
| `KINGRES_LONG_TESTS=1`, ledgerlog, Linux | pass, 18.2 s, no skip; it adds the 65,539 and 66,000 leaf cases, long random sizes, the 520-leaf crash matrix, the 64 MiB batch, the 13-log publication, the 3 s give-up test |
| Random walk of 300 rounds x 400 steps (120,000 operations: 4,800 Add, 1,676 Activate, 445 Retire, 4,449 Revoke accepted, about 108,000 refused) over Add, Activate, Retire (AtMost, Unbounded, no decision), Revoke, with times that go forward, back, far ahead and with milliseconds, against my own model written from the doc | every accept and refuse equal to the model, every resulting key field equal, refused calls leave the input unchanged, `Validate` and `TrustEntries` accept every set the operations produced, never a third active key, never an overlap without two active keys, never an in-service set without an active key, two active keys can always be retired at a late time |
| Sweep against the built verifier: sizes 0, 1, 2, 255, 256, 257, 65535, 65536, 65537, 20011; uneven batches (1 to about 3000, one batch boundary at one third); my own naive recursive tree (nothing from the package) for the root; `Audit`; checkpoints: one key one line, overlap with two lines, retired `AtMost(n)` (accepted) and `AtMost(n-1)` (refused, status 1, key-size), retired `Unbounded`, revoked key with a note of two lines (accepted) and of the revoked key only (refused), re-signed note; `consistency --old SIZE:ROOT` with my naive root of the one-third size | all as predicted: 0 or 1 exactly where expected; the package root equals the naive root for every size; `Audit` passes |
| Trust files against `kingres-verify trust show`: `AtMost(2^62-1)`, `AtMost(1)`, `Unbounded`, revoked, next with `IncludeNext`, 16 keys with 191-byte labels of `U+2028 & < > " \ e-acute`, prefixes of 160 bytes and with UTF-8, activation in year 1969 and 10000 | all read, status 0, except activation in year 10000 (status 2, see K2-05) |
| Key id collision matrix with the built verifier (a pair found by birthday search under one origin): both active, retired+active, revoked+active, next excluded, next included, next-revoked; fresh `checkpoint` and held-note paths (`consistency --old`, `checkpoint --state`) | see K2-02 |
| Crash matrix: a 600-leaf append after a 5-leaf one, a `DirStore` hook error at each of the 32 hook steps (temp-written, before-publish, after-publish, every sync-dir), `RemoveTemporaryFiles`, `Open` (expects success or `ErrTilesAhead`), `Open` with `AllowTilesAhead`, same leaves again, naive root, `Audit` | all 32 recover |
| `ResignCheckpoint`: good + unknown forged line, good + forged line of a trusted key, signed only by an untrusted key, no trusted key, 32-line note, same signer twice, trusted key listed twice, new signer colliding with a trusted key | see K2-03 |
| `PutCheckpoint` failure paths (rename error, busy forever, checkpoint is a directory), on Linux with seams and on Windows with real handles and files | temporary files left: 0 in every path (except the deliberate hook crash); see K2-08 |
| Memory scans of the process after a plain flow and a sealed flow (raw seed, raw passphrase) | see K2-11 |
| Fuzz: 8 targets (key file, note, checkpoint, tile, frontier, tile path, aggregate entry, checkpoint text then parse), 20 s each, `-parallel=2`, 160 s in all | no crash, no failure |
| Mutants of my own: 45 valid ones (5 first versions did not compile and were rewritten and run again) | 38 killed (one of them, K1-10a, only by exhausting memory), 7 survived; list under K2-12 |

## The earlier findings

| Id | State | Evidence |
|---|---|---|
| K1-01 Reader panics on a short full tile | fixed | `reader.go:64-70` checks `len(fdata) != TileWidth*HashSize` and returns `ErrTileMalformed`; mutant removing the check killed by `TestReaderIsTotalForAnyGetter` |
| K1-02 max tree size above 2^62-1 | fixed | `state.go` `MaxTrustTreeSize`, `TreeBound.check`, `Validate`; mutants on `check` and on `Validate` each killed by `TestRetireBoundRange`; `AtMost(2^62-1)` accepted by the verifier. Residual: times (K2-05) |
| K1-03 tile replaced through rename fallback | fixed | `PutTile` has no rename; a link failure other than "exists" gives `ErrHardLinksRequired`, nothing published, no temporary file; mutant that renames on link failure killed by `TestPutTileFailsClosedWithoutHardLinks`. "Exists" with a vanished target gives an error, "exists" with other content `ErrConflict` (my probe). The classification is too wide (K2-07) |
| K1-04 PutCheckpoint on Windows with an open handle | fixed, with a defect in the classification | typed `ErrCheckpointBusy`, configurable `CheckpointRetry`, default 3 s; real-handle test passes on Windows; mutants killed. K2-08 |
| K1-05 new directories not flushed | partly | the order is right (hook shows `tile/0/x001/234.p`, `tile/0/x001`, `tile/0`, `tile`, `.`), my crash matrix recovers at each step. But a put of a tile that exists already returns without any flush (K2-01); the flush of the parents in `NewDirStore` is untested (mutant survives) |
| K1-06 Checkpoint.Text vs Parse | fixed | newline refused in `Text`; mutant killed by `TestCheckpointTextRefusesNewlinesAndTooManyLines` |
| K1-07 key id collision | partly | typed error with both fingerprints in `SignNote`, `AddSignature`, `VerifyNote`, `CheckKeyIDs`; verifier agrees for two usable keys. Misses: collision with a trusted key that has no line in the note (K2-03); revoked and next keys (K2-02) |
| K1-08 secret zeroing, plain key file | partly | the plain file is parsed from and written into byte slices; the base64 text of the seed is gone from memory (scan, with a positive control). The raw seed is still in memory (K2-11); the doc sentence about where a secret cannot be zeroed does not name it; mutant removing `defer clear(seed)` in `parsePlain` survives |
| K1-09 key set invariants | fixed in the points named, new gaps below | (a) `otherActive` looks at the activation time; (b) `settle` clears stale overlaps; (c) `Validate` rule and `Signers` error; (d) `TreeBound` with no default; (e) "When the set is full" in the doc. Mutants for each killed. See K2-04 and K2-06 |
| K1-10 unbounded work | fixed | `Required` bounded (2^28), `SameFile` after open in `readRegular`, `LoadFrontierFile` and `readPrivateFile`, limited readers. The mutants for `SameFile` are killed; those for the limited readers survive (K2-12) |
| K1-11 re-sign does not verify | partly | `ResignCheckpoint` takes the trusted keys and verifies first; forged trusted line, untrusted-only note and empty trust are refused (probe). Residual: K2-03 |
| K1-12 test gaps | fixed for those named | the mutants of K1-12 (`checkAhead` tolerance, next tile only, level 0 only, tile path overflow guard, `noLinks`, `MaySign` activation time, `Retire` before activation, `SameFile` in `readPrivateFile`) are all killed now |
| K1-13 behaviour worth stating | unchanged, documented | still no lock file; the doc states it |

## New findings

### K2-01 (medium) A repeated put of an existing tile returns nil without flushing anything

`ledgerlog/dirstore.go` `PutTile`: the first lines (`if done, err := same(); done || err != nil { return err }`) and the
`fs.ErrExist` branch return nil when the tile is there and equal, before `d.flush` is reached. After a crash between the
link and the flushes (hook `after-publish`) the tile and the directories it created are in the page cache but not
necessarily on disk; the doc (`doc.go` "DirStore: what is on disk", and the comment of `DirStore`) says "a put that
returned nil is whole on disk with every directory it created" and that after a crash "a second put of the same tile
finishes it". The retry is exactly what the crash procedure of the doc prescribes (append the same leaves again), and it
now returns nil without making anything durable; the caller then persists the frontier that needs the tile.
Shown by probe: put with a hook that fails at `after-publish`, then the same put with a recording hook: err nil, steps
`[]` (no `sync-dir`). The existing tests assert it (`TestPutFlushesNewDirectoriesUpToTheRoot`: "a repeated put writes
nothing and flushes nothing"), and `TestCrashAtEachFlushOfAPut` only checks that the second put returns nil and the
tile reads back, which the page cache serves.
A fix must make a put that returns nil durable also when the tile existed: flush the directory of the tile and every
ancestor up to the root (the depth is at most four) when the tile is found equal, and a test must show the `sync-dir`
steps on the repeated put.

### K2-02 (medium) Key id collisions: revoked and next keys count for held notes, and revoking is no cure

`ledgerkeys/doc.go` ("Key id collisions"), `ledgerlog/doc.go`, `ledgerkeys/keyid.go:36`. The doc tells the operator to call
`CheckKeyIDs` with "the keys that may sign at once and the retired keys the trust file lists", says the verifier refuses
a log "while its trust file lists both keys as usable", and offers revoking the retired key as the cure for an
active/retired collision. I built the verifier and ran a colliding pair A, B under one origin:

| trust file | fresh `checkpoint` (usable keys only) | held note (`consistency --old`, `checkpoint --state`) |
|---|---|---|
| A active, B active | refused (status 1, "two trusted keys have the same name and key id") | refused (status 2) |
| A retired, B active | refused | refused |
| A revoked, B active, note by B | accepted (revoked keys are left out) | refused (status 2) |
| A revoked, B active, note by A | refused (A's line has B's id and does not verify) | refused |
| B next, not listed | accepted for a note by A | accepted |
| B next, `IncludeNext` | refused | refused |

The held-note path (`trust.AllKeys`) takes every key of the file whatever its standing. A key is never removed from
the trust file, so after a collision with a revoked key every auditor who keeps a state file for that log (or passes
`--old`) gets status 2 for ever, and the log can no longer be audited incrementally. Revoking does not cure this. So (1)
the keys to check before a key is activated are all keys of the trust file, revoked ones and, with `IncludeNext`, next
ones included, not "retired keys"; (2) the doc statements above are wrong for held notes; (3) a collision with a
revoked key is a real collision for part of the verifier, so `CheckKeyIDs` on such a set is not a false report.
A fix must give the caller a function that takes the set (all states, as `TrustEntries` writes them) and an origin and
applies the verifier's rule for both paths, correct the doc, and say that no later transition cures a collision with a
revoked key (the key to avoid must be refused before it is added).
No false report: the same key twice, a retired key listed once, and a key that appears once are never reported.

### K2-03 (medium) ResignCheckpoint does not check a new signer against trusted keys that have no line in the note

`ledgerlog/note.go:addSignature` compares the new key id only with the lines already in the note; `ResignCheckpoint`
(`checkpoint.go`) passes the trusted keys only to `VerifyNote` (collisions among them) and as `known` (for the name in
the error). Shown (probe and verifier): trusted A (signed the note) and T (no line); new signer S whose key id equals
T's under the origin; `ResignCheckpoint(note, [A, T], S)` returns a note with the lines of A and S and no error; the
verifier with a trust file that lists A, T and S refuses it (status 1, ambiguous trusted key). The doc says the function
refuses "one whose key id collides with another key's".
Related trap, by reading and probe: a forged line under a key id that the caller did not put in `trusted` is kept
(the note then has 3 lines), and if that key is usable in the trust file the verifier refuses the whole note.
A fix must run `CheckKeyIDs(origin, trusted..., more...)` before it signs, and the doc must say that `trusted` has to be
every key of the trust file (K2-02), with a test that uses a silent trusted key.

### K2-04 (medium) A key activated with a time in the future wedges the overlap

`ledgerkeys/state.go` `Activate`, `DueForRetirement`, `Retire`. The package has no clock, so `Activate` accepts any
time. Probe: A is the only key and is activated with a clock error (2099); B is added and activated now. Result:
`Signers(now)` is B only (good), A gets `OverlapEnds` = now + 30 days, after 60 days `DueForRetirement` returns A, but
`Retire(A)` fails ("the key is retired before it was activated"), `Retire(B)` fails (A is not in force),
`Activate(C)` fails (`ErrTooManyActive`). The only exit is `Revoke(A)`, which marks a key that never signed as
compromised for ever and writes it into the trust file as revoked. `CanSign(A, PurposeKeyRetired, B, now)` says nil for
the key that `Retire` refuses to count. The same clock step back makes `Signers` order the keys by activation time while
`OverlapEnds` sits on the other key.
A fix must make `DueForRetirement`, `Retire` and `CanSign` agree, and give a way out that does not revoke (for example
allow retiring a key that is not yet in force at its own activation time, or refuse an activation later than the
latest time already in the set plus a stated tolerance and say so), with a test of the sequence above.

### K2-05 (medium) Add and Activate accept a time that no later call can repair

`state.go` `Add` and `Activate` call `norm(now)` and nothing else. (1) A zero `time.Time` (what an unset database column
gives): `Add` returns a set with `CreatedAt` zero and no error; `Validate` then refuses the set for ever
("has no creation time") and `TrustEntries` with it, and nothing removes a key, so the whole key set is dead and one of the
16 places is used. `Activate` with a zero time gives the same for `ActivatedAt`. (2) A time of year 10000 or later:
`Activate` and `Validate` accept, `TrustFile` writes `"not_before": "10000-01-01T00:00:00Z"`, and the verifier refuses the
file (`trust show` status 2, "not an RFC 3339 time"), so no checkpoint of the cluster verifies. Shown by probe and verifier.
A fix must refuse a zero time and a time outside years 1 to 9999 in `Add`, `Activate`, `Retire` and `Revoke`, and
`Validate` must refuse such a stored key.

### K2-06 (low) Validate accepts damaged sets of keys that it should refuse

`state.go:Validate`. The new rule about a set in service with no active key works in both directions I could test:
no legal set was refused (120,000 random operations, every intermediate set accepted) and the sets with no active key
after a key left `next` are refused. But damaged sets are accepted: two active keys with no `OverlapEnds` (never due for
retirement, two keys sign for ever); two active keys that both have an `OverlapEnds`; the overlap on the younger key;
an `OverlapEnds` before the activation; a retired key with `CompromisedSince`; a revoked key whose `RevokedAt` is before its
creation; and every key flipped back to `next` with its times cleared (looks like a new set; cannot be told apart, so
the doc should say so). A fix must require, for two active keys, exactly one `OverlapEnds`, on the older key and after
the activation of the younger, and refuse the fields that do not belong to a state.

### K2-07 (low) ErrHardLinksRequired covers every link failure and hides the cause

`dirstore.go` `PutTile`: any link error but "exists" becomes `ErrHardLinksRequired` with the cause formatted with `%v`.
Probe: `ENOSPC`, `EDQUOT`, `EIO` and `EMLINK` all give `ErrHardLinksRequired` ("needs a file system with hard links"),
`errors.Is(err, ENOSPC)` is false. A full disk then sends the operator to move the directory to another file system.
A fix must classify only the errors that mean no hard links (`EPERM`, `ENOTSUP`, `EXDEV`, `ENOSYS`, and on Windows
`ERROR_INVALID_FUNCTION` and `ERROR_NOT_SUPPORTED`) as `ErrHardLinksRequired`, and wrap the cause with `%w` in all cases.

### K2-08 (low) Windows: access denied is always "busy"

`dirstore_windows.go` treats `ERROR_ACCESS_DENIED` as a blocked rename. Shown on Windows (real files): a checkpoint that
is read-only, or a directory named `checkpoint`, gives `ErrCheckpointBusy` ("held open by another program; a retry may
succeed") after the whole retry time (3 s by default), every time, for a condition that does not go away. The clean
failure (old file whole, no temporary file) holds.
A fix must check for a directory or read-only attribute at the target before waiting, or return a different error
after the first denial when the target is not a regular writable file.

### K2-09 (low) Context is ignored by the stores and by Audit

`PutTile`, `PutCheckpoint`, `GetTile` of `DirStore` and `MemStore` do not look at `ctx` (probe: a cancelled context gives
nil), `Audit` does not check it in its loop (probe: `Audit` with a cancelled context on `MemStore` returns nil after
reading everything), and `PutCheckpoint` can wait 3 s with no way to stop it. `Audit` accepts every size below 2^62 and
its time is linear in the size (stated in the doc); `Required` stops at 2^28 while a log can grow beyond it.
A fix must check `ctx.Err()` in the `Audit` loop at least once per tile, in the retry wait, and in the store calls, or
the doc must say that `ctx` is only passed to the getter.

### K2-10 (note) Smaller points

- `Reader` keeps every tile it parsed in `cache` with no eviction (probe: 41 entries after 1,429 proofs over 10,000
  leaves; for a tree of 2^28 leaves a long-lived `Reader` can hold 1 M tiles of 8 KiB). Fine for a `Reader` per request.
- `Required(2^28)` allocates 257 MB in all (the doc says "some 50 MB", which is the list).
- `Retire` takes one `AtMost` for all logs of a key; the doc says so.
- The ErrNoOtherSigner message for `Retire(B)` when A is not in force is long but right.

### K2-11 (note) Raw seed copies stay in memory; the doc sentence is incomplete

`ledgerkeys/doc.go` "Where a secret cannot be zeroed" lists "the expanded private key and the scalars" of
`crypto/ed25519`. Probe (scan of the process memory after everything was closed and destroyed): the raw 32-byte seed is
found once after a plain flow and four times after a sealed flow that signed four times (one copy from
`NewKeyFromSeed`, one per `Sign`); the raw passphrase is found 0 times; the base64 text of the seed 0 times. The doc
should name the seed itself (it is what `NewKeyFromSeed` and `Sign` copy); the shipped test scans only the base64 text
and allows three tries, so it cannot see this, and the mutant that removes `defer clear(seed)` in `parsePlain` survives.
Errors and `%v` never show a seed or passphrase: `Parse` errors carry no content (only a clipped member name of an
encrypted file), the redaction methods cover every verb, and the fuzz run found nothing.

### K2-12 (note) Test quality

Surviving mutants (the test suite does not notice): `NewDirStore` not flushing the parents of the directories it made
(`K1-05b`); `parsePlain` not clearing the raw seed (`K1-08b`); the limited reader in `LoadFrontierFile` and in
`readRegular` replaced by an unlimited one (two mutants; the size is checked by `Lstat` first, and no test makes the file
grow in between); a put that crashed leaving its temporary file now removes it (`crashed` flag; no test counts the files
after a hook crash); `known = append(known, s.Public())` in `ResignCheckpoint` removed; the length test at the start of
`parsePlain` (probably equivalent, the later decode refuses the same input).
`TestRequiredIsBounded` calls `Required` with 1<<40 and 1<<62: with the bound deleted it allocates until the machine dies
(see the incident above); the test should run the unbounded sizes only after a guard, or pass a smaller limit to an inner
function. `TestPutCheckpointDefaultRetryIsThreeSeconds` first compares the constant with its own literal (cannot fail
usefully) and then waits 2.2 s in the default run. Skips: the verifier tests skip with only a message visible under
`-v` when `KINGRES_VERIFY` is not set (in a plain `go test` they are silent; the coordinator's run script must always set it);
the long tests skip with a message naming `KINGRES_LONG_TESTS=1` (justified); on Windows the two `os.SameFile` swap tests
skip with a reason that is stated and true as far as I could tell, and the symlink test skips without the privilege
(justified, not covered on Windows at all). On Linux nothing else skips. The default run needs no network; on Windows it
took 134 s here, against "under 30 s" in the doc (114 s in one test).

## What held under attack

- The tree, the tiles and the proofs against my own naive tree at the sizes listed and in uneven batches; everything the
  packages wrote was accepted by the verifier, with one key or two lines, retired with `AtMost(n)` and `Unbounded`,
  revoked, and re-signed, and `AtMost(n-1)` was refused with the right step.
- The key state machine against my independent model for 120,000 operations including backward and far-future times:
  every accept and every refusal equal, input never changed by a refused call, no third active key, no stale overlap, no
  in-service set without an active key, always a way to retire one of two active keys at a late time (except K2-04, which
  needs a future activation recorded in the set), 17th key refused.
- `Retire` with `AtMost` or `Unbounded`: the zero `TreeBound` is refused, 0 and 2^62 are refused, 2^62-1 is accepted by the
  verifier, an `AtMost` below the size of a checkpoint makes the verifier refuse it.
- `Signers` returns an error for an empty list; `Validate` refuses a set that has been in service with no active key and
  accepts a new set of next keys and the empty set.
- `CheckKeyIDs` and `VerifyNote` agree with the verifier for two usable keys (active, retired, next with `IncludeNext`),
  report no collision for the same key twice, and `SignCheckpoint` with both colliding keys is refused with both
  fingerprints.
- `PutTile`: no rename path; link errors leave no temporary file and no tile; "exists" with a vanished target is an
  error; "exists" with other content is `ErrConflict`. Flush order is deepest first up to the root and a crash at each of the
  32 steps of a 600-leaf append recovers (`Open` says `ErrTilesAhead` or succeeds, then the same leaves give the naive
  root and `Audit` passes).
- `PutCheckpoint`: no temporary file after a rename error, a busy failure, a directory in place of the file; the typed error
  works with a real open handle on Windows and the old note stays whole.
- `ResignCheckpoint`: a forged line of a trusted key is refused, a note signed only by untrusted keys is refused, an empty trust
  list is refused, the 32-line limit is an error and not a truncation, the same key twice is refused, a trusted key listed
  twice is not a collision, a collision between two trusted keys is reported.
- Bounds: `Required` above 2^28 refused for 2^28+1, 2^40, 2^62-1, 2^62 and 2^64-1 before any allocation; no other exported
  function I called with a size near 2^62 or with parsed input allocated or looped in proportion to it (`Reader`,
  `ParseFrontier`, `VerifyConsistency`, `RootFromInclusion`, the parsers under fuzzing). `Audit` is linear by its doc.
- The plain key file strict prefix rule: `KINGRES-KEY-1\n{"seed":"` followed by exactly 44 base64 characters and `"}\n`.
  It is the canonical form of the previous version, so it refuses nothing the previous version wrote; it refuses files an
  operator edited (CRLF, a missing final newline, a BOM, spaces), as the previous version did, and says why without echoing
  content.
- Fuzzing 160 s over eight parsers: no panic, no allocation spike, no parse-then-write mismatch.

## Verdict

Fit to be wired, with three conditions that touch what the wiring will depend on. Nothing I found makes the verifier
accept damage or breaks the crash procedure, and everything the packages write is accepted by the built verifier. But
before the daemon calls these packages: (1) fix K2-01, because the documented repair after a crash returns nil without
making the tile durable, which is the one guarantee the doc states about durability; (2) fix K2-02 and K2-03 or at least
change the doc and the wiring to check every key of the trust file, revoked ones included, against every log origin before a
key is added, because a collision with a revoked key cannot be cured afterwards and blocks every auditor's state file;
(3) fix K2-04 and K2-05 or make the daemon refuse any time that does not come from a trusted clock and a recent value,
because a zero time or a future activation puts the key set into a state that only a revocation or hand-edited rows can
leave. K2-06 to K2-12 can follow with the wiring.

## What I did not check

- Real power loss and the state of a disk after it; K2-01 is shown with the hooks only. Network file systems, FAT, file
  systems without hard links (the link failure is injected through the seam).
- Windows junctions, symbolic links (no privilege), ACLs, `\\?\` paths; macOS.
- A remote `TileGetter`, external signers, the race detector, concurrent `Log` and `Reader` use.
- The sizes above 65,537 with uneven batches against the verifier beyond 66,000 leaves (the long tests do 66,000);
  the verifier's `proof` and bundle commands.
- Fuzzing beyond 20 s per target; timing channels; the Go runtime's own copies of keys beyond the scans above.
- The mutant for `Required` without its bound was killed only by exhausting the memory of the machine (K1-10a); I did not
  re-run it.
- Design text outside the sections named in the task, and the SQL and C parts of the extension.
