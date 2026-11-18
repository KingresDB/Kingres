<!-- SPDX-License-Identifier: AGPL-3.0-only -->
# Third independent review of the key and log libraries of kingresd

Reviewer: an independent reviewer. Date: 2026-11-18.

## Scope and method

Scope: `control-plane/internal/ledgerkeys/` and `control-plane/internal/ledgerlog/` after the second fix round, read against
the second review (K2-01 to K2-12), both `doc.go` files, `docs/design/ledger-phase7.md` (sections 3 and 4, corrections 14, 17,
40 to 48, 93, 94 and 101 to 103) and `kingres-verify` built from the export (a separate module).

Method: I read all non-test source of both packages and the parts of the tests that meet a bound. I worked only in private
copies (Linux and Windows), wrote my own probe tests there (an independent model of the key state machine written from the doc,
a naive recursive Merkle tree, a collision matrix run against the built verifier, a damage sweep over stored sets, crash and
flush probes), and ran one-line mutants of my own, one test binary at a time, under `timeout`, with a guard goroutine
(exit when the runtime has more than 2 GiB mapped) in every probe and `GOMEMLIMIT` set. Before running any mutant that widens
or removes a bound I read the test that meets it: the `Required` bound test uses the inner function with a small bound and
sizes that stay small, the limited-reader test grows a file by about 66 KiB and has an 8 MiB budget reader; none can allocate
more than about 50 MB. No run came near the limit. The export was not touched; all copies were removed at the end. The
probes are not part of the repository.

## What I ran

| Run | Result |
|---|---|
| `go vet`, both packages (Linux) | clean |
| Default tests, Linux, `KINGRES_VERIFY` and `KINGRES_REQUIRE_VERIFY=1` set | 184 passing entries (tests and subtests), 0 failures, 3 skips (`TestPutCheckpointGivesUpAfterTheDefaultRetry`, `TestLargerTreeAcrossTileLevels`, `TestMaxBatchBytesBoundary`, all long tests); ledgerkeys 1.4 s, ledgerlog 7.7 s |
| Default tests, Windows (own build of the verifier) | 179 passing, 0 failures, 7 skips (the three above, two `os.SameFile` swap tests, the "open off Windows" test, the symlink test); ledgerkeys 4.9 s, ledgerlog 19.7 s (the second review measured 134 s; the doc says under 30 s: now true) |
| Random walk against my own model of the key set: 400 rounds x 300 steps (120,000 operations: 6,400 Add, 2,184 Activate, 520 Retire, 5,935 Revoke accepted, about 102,000 refused) over Add, Activate (overlap 0, 1, 30, 365, 366, -1), Retire (AtMost small, AtMost 2^62-1, AtMost(0), Unbounded), Revoke (also compromised-since in the future); times that go forward, back by up to 400 days, ahead by up to 900 days, with milliseconds, zero, year 0, year 1 at one second, year 9999, year 10000 | every accept and refuse equal to the model, every resulting key field equal, every accepted state passes `Validate` and `TrustEntries`, never a third active key, a way to retire one of two active keys at the time both are in force, always a signer at the end of time while a key is active |
| Damage sweep: 7 valid sets (new, one active, active+next, two active, retired+active, two retired+active, with a revoked next, revoked active) x every key x 38 single-field damages (state flips, each time moved by 1 s, 1 day, 1000 days, set or cleared, MaxTreeSize, Reason, Label, Public) | see K3-10 |
| Times (probe): first key, a set idle two years, a next key that waited 366 and 367 days, restore from a backup, Add at 2099 then activation in 2098, activation 400 days before creation, rotation at a clock one hour back, Revoke clamp, zero time, years 0, 1, 9999, 10000, local time zones whose UTC year is on the other side of the range | see K3-03 to K3-06 |
| Key id collision matrix against the built verifier (a colliding pair A, B under one origin from the tests' own seeds, plus a clean active key C that signs every note; unchecked `Add` on purpose): A retired and B retired; both revoked; retired and revoked in both orders; A retired and B next with next listed and not listed; A revoked and B next listed and not listed; commands `checkpoint` (fresh path), `consistency --old NOTE` and `checkpoint --note --state` (held paths) | 8 rows; `CheckSetKeyIDs` agrees with the verifier in every row on "collision" and on `Fresh` / `Held` (see "What held") |
| `ResignCheckpoint` probes (revoked-only note, witness line, unknown line under the origin, 32-line note, no new signer) with the verifier | K3-01; the rest held |
| Flush probe: 100,000 leaves in 10 batches of 10,000 into a `DirStore` with flushes on, then the crash repair (`Open` with `AllowTilesAhead` from the empty frontier, same leaves again) | first write: 812 puts, 840 directory flushes, 31.8 s; repair: 812 puts, 2,467 directory flushes plus one file flush per put, 40.3 s, same root |
| `PutCheckpoint` onto a directory (Linux, Windows), onto a read-only file (Windows), onto a file held open (Windows); cancelled context on every store call and `Audit` (also cancelled after the fifth get); link error classification on Linux | all as documented, see the table |
| Sweep against the built verifier (Linux): sizes 0, 1, 2, 255, 256, 257, 511, 512, 513, 20011, 65535, 65536, 65537, written in random batches of 1 to 3,000 leaves; naive recursive tree of my own for the root; `Audit`; five trust variants (one key one line; two keys two lines; A retired `AtMost(size)` and a line of A only; A retired `Unbounded`; A revoked with a note of B and A); `checkpoint`; `consistency --old SIZE:ROOT` with my naive root of one third; `AtMost(size-1)` refused with status 1 | all as predicted, package root equals the naive root at every size, `Audit` passes |
| Trust files with times at the edges against `trust show`: activation at year 1 (one second, and with nanoseconds that `Add` truncates), 9999-12-31T23:59:59Z, 1969; year-1 keys with a 365 day overlap and `AtMost(2^62-1)` | all read, status 0 |
| Fuzz: `FuzzParseKeyFile`, `FuzzParseNote`, `FuzzCheckpointTextThenParse`, `FuzzParseFrontier`, `FuzzParseAggregateEntry`, `FuzzParseState`, 25 s each, `-parallel=2`, 150 s in all | no failure |
| Mutants of my own: 56 runs in all (K2 items plus the new points), 4 of them invalid (build failed or pattern not found) and run again in a better form | of the 52 valid runs 41 killed and 11 survived, listed in K3-07 and in the table of earlier findings |

## The earlier findings

| Id | State | Evidence and mutants |
|---|---|---|
| K2-01 repeated put returns without flushing | fixed | `dirstore.go:314-321` and `:353` call `flushExisting` (`:417`): file, directory, every ancestor to the root, the parent of a created root. Mutants: `return nil` in the first branch killed by `TestPutFlushesNewDirectoriesUpToTheRoot`; in the `ErrExist` branch by `TestPutTileExistsAtTheLink`; stop after the tile directory by `TestPutTileExistsAtTheLink`. The mutant that removes only the file flush (`syncFile`) survives: in practice equivalent (the data was flushed in `writeTemp` before the link; only the directory entry was ever in doubt) and not visible to a hook |
| K2-02 collisions with revoked and next keys | fixed | `CheckSetKeyIDs` (`keyid.go:104`) with `Fresh` and `Held`; `AddChecked` (`state.go:426`) checks every key, next included. Matrix against the verifier in all eight of my rows agrees (see "What held"). Mutants on the revoked rule, on `Held`, on `Fresh`, on `usable`, on `IncludeNext` in `AddChecked`: all five killed. New gaps: K3-02, K3-08 |
| K2-03 resign does not check a silent trusted key | fixed | `checkpoint.go:208` runs `CheckKeyIDs` over trusted and new signers; `:217` refuses a line of no trusted key. Mutants: the check removed, the untrusted-line check removed, `known = append(known, s.Public())` removed: all three killed. New gap: K3-01 |
| K2-04 future activation wedges the overlap | fixed for the case named, with a hole | `Retire` clamps to the activation (`state.go` about :609), `otherActive` needs "in force", `DueForRetirement`, `Retire`, `Revoke`, `CanSign` agree (`MaySign` checks `now.Before(ActivatedAt)` for the checkpoint, key-added and key-retired/revoked purposes). The model and my clock-back probe find no wedge. Mutants on the clamp, on `otherActive`, on both `MaySign` checks, on the 366-day guard (`+100 y`, `+1 d`): killed. The guard itself is easy to defeat: K3-03 |
| K2-05 zero time and year 10000 | fixed | `checkTime` (`state.go` about :189) in `Add`, `Activate`, `Retire`, `Revoke` (both times), overlap end checked, `Validate` refuses stored times. Mutants (zero, range, `Validate` range, overlap end, compromised-since, range widened to 10000): all six killed. Year 1 at one second and 9999-12-31T23:59:59Z are read by the verifier |
| K2-06 `Validate` accepts damaged sets | fixed for the shapes named; three mutants survive | exactly one `OverlapEnds` for two active keys, on a key, equal to the other key's activation plus whole days 1 to 365 (`state.go` about :364-381); fields that do not belong to a state refused. Random walk and 7 base sets: nothing legal refused, no damaged shape of the review accepted. Survivors: minimum of one day, `OverlapEnds` on a non-active key (redundant with an earlier check), retired-before-activated on a revoked key: K3-07 |
| K2-07 link error classification | fixed | `dirstore_unix.go:33`, `dirstore_windows.go:37`; `%w` on both paths (`dirstore.go:356`, `:360`). Probe: ENOSPC, EDQUOT, EIO, EMLINK, EACCES, EROFS are not "no hard links" and `errors.Is` finds the cause; EPERM, EXDEV, EOPNOTSUPP are. Mutants (always true, `%v` on each path, ENOSPC added): all four killed. EPERM is wider than the doc's reasoning: K3-09 |
| K2-08 Windows access denied is always busy | fixed | `checkpointTargetProblem` (`dirstore.go` about :640), called after the first denial (`:632`). Windows probe: a directory at the target gives the named error after 7 ms, no `ErrCheckpointBusy`; a read-only file the same after 8 ms with the old note whole; a file held open gives `ErrCheckpointBusy` after the 300 ms I set. Mutant: killed |
| K2-09 context ignored | fixed | every `DirStore` and `MemStore` call, `Audit`, the retry wait. Probe: every call returns the context error; `Audit` stops at the fifth get when the context is cancelled there. Mutants: the wait ignoring the context killed; the two `Audit` checks each survive alone (each covers the other; removing the `load` check and the leaf-tile check together was not run); `Append` without its per-tile check survives (`PutTile` checks anyway) |
| K2-10 smaller points | fixed | `required` counts first and allocates once (`tilepath.go:168`; capacity equals length, checked by `TestRequiredIsBounded` at 2^28 for 2,101,265 keys); doc corrected to "about 50 MB". Mutants: bound removed (small bound, safe), allocation by doubling, a limit of limit+1000 in `readAtMost`: all killed |
| K2-11 raw seed in memory | doc fixed, test still cannot see it | `doc.go` of ledgerkeys now names the raw seed and the library's copies. The mutant that removes `defer clear(seed)` in `parsePlain` still survives, although the doc says a test scans that path and finds none: K3-07 |
| K2-12 test quality | partly | killed now: `NewDirStore` parent flush, the limited readers (through `readHook`), the `crashed` flag, `known = append`; the `Required` test no longer touches unbounded sizes; the default retry test runs on a virtual clock. Still open: the constant-against-itself tests, the verifier-skip notice (K3-07) |

## New findings

### K3-01 (medium) ResignCheckpoint lends the signature of a good key to a note that rests only on a revoked key

`ledgerlog/checkpoint.go:185-230`, with the instruction of `doc.go` ("giving it as the trusted keys EVERY public key of the trust
file ... revoked ..."). `trusted` has two jobs that need opposite answers: the collision check wants every key of the file, and
the "does this note verify" check wants only keys that may vouch for it. The function has only `[]ed25519.PublicKey`, so it
cannot tell a usable key from a revoked one, and `VerifyNote` counts a line of a revoked key as a verified line.
Shown by probe and verifier: a checkpoint signed by K1 alone, K1 revoked in the trust file, K2 active. The verifier refuses the
note by itself (status 1, "no signature of a trusted key"). `ResignCheckpoint(note, [K1, K2], K2)` returns a note with both
lines and no error; the verifier accepts that note (status 0). A forged checkpoint made with a leaked, revoked key, planted in
the publication directory, becomes a genuine checkpoint of the cluster the next time the daemon re-signs the stored note at a
rotation. The same holds for a retired key beyond its `max_tree_size` (no standing, no bound is known to the function).
This contradicts design 3.3 (a revoked key's signature is rejected unless provably older than the compromise) at the one place
where the server itself endorses text. It needs write access to the store and a revoked key, so medium and not high.
A fix must give the function the standing of each key (for example a list of vouching keys apart from the list of keys for the
collision check) and refuse a note that no vouching key signed, with the size check against a retired key's bound, and a test
that runs the case above with the verifier.

### K3-02 (medium) AddChecked refuses every new key for an origin while the set holds an old colliding pair, and names the old pair

`ledgerkeys/state.go:426` and `add`, `keyid.go:104`. `add` calls `CheckSetKeyIDs` on the whole new set, so an old collision
between two other keys (found after the fact, or from an origin that did not exist when they were added; the doc says it
cannot be cured) makes `AddChecked` fail for a clean key with `ErrKeyIDCollision`. Probe: A and B collide, `AddChecked(C, origin)`
returns the collision of A and B (fingerprints of A and B, not C). Consequences: the checked path is closed for that origin
for ever, so the only way to rotate is the unchecked `Add`, which also stops checking every other origin; and the message
("check a new key with AddChecked ...") sends the operator to the function that failed. Related: `CheckSetKeyIDs` returns the
first pair it meets, so an old held-only pair (revoked) is reported before, and hides, a later pair that is also `Fresh`; its
`Fresh` flag is then a statement about one pair, not about the set.
A fix must check the new key against the other keys only (and report the pair that contains it), report every colliding pair
or at least the worst, and have a test with an old pair in the set.

### K3-03 (medium) The activation guard is removed by any recorded far time, and by one wrong reading at Add

`ledgerkeys/state.go` `latest` (about :469) and the guard in `Activate` (about :525). `latest()` counts creations,
activations, retirements and revocations. `Add`, `Retire` and `Revoke` accept any time of years 1 to 9999. Probe: a set with an
active key from 2026; `Add(B, 2099)` is accepted; after that `Activate(C, mid 2098)` is accepted, because the limit is now
2099 + 366 days. In the random walk a `Revoke` at 9999-12-31 was accepted, which leaves the guard without any effect for the
rest of the life of the set. And a clock that is wrong in the same way for `Add` and `Activate` is not seen at all (the doc says
so). The guard of K2-04 therefore protects only against a single wrong reading at `Activate`, and the first call of every
rotation, `Add`, is unguarded and itself leaves a creation time in 2099 that no call can repair (`CreatedAt` stays; the key can
be activated at the right time, since activation before creation is allowed, K3-05).
A fix cannot guard `Add` by the latest record (a set idle for two years must be able to add a key), so it must take what the
package does not have: a reference time from the caller (for example every transition takes a `Clock` or a `Reference`
that the caller reads from a source other than the one it passes as `now`), or the doc must say that the 366-day rule is a
last line and that the daemon must compare every `now` it passes with its own trusted clock and with the latest record before
it calls. The tests must show a record in the far future from `Add`, `Retire` and `Revoke`.

### K3-04 (low) A next key that is pre-announced for more than 366 days cannot be activated; the advice for it has an order problem

`state.go` `Activate`. `IncludeNext` exists so that an auditor can pin a next key before it signs; a rotation plan with a
notice longer than 366 days (nothing in the design forbids it; the set holds 16 keys and rotation is meant to be rare) is
refused at the activation, and so is a set restored from a backup whose next key is older. The doc says "add a fresh key and
revoke the stale one". Probe: with a set whose only key is a stale next key, `Add(fresh)` works, `Revoke(stale)` fails (no
other key active in force) until the fresh key is active; the stale key stays pinned by auditors and takes a place of the 16.
Not a defect of the guard; the doc should give the order (add fresh, activate fresh, then revoke stale) and say that the pinned
auditors need the new key, and the limit should be a stated policy (366 days) in the design, not only a number in code.

### K3-05 (low) Times in the record can be in an order no key ever had

`Activate` accepts a time 400 days before the key's creation (`Validate` accepts the result); `Revoke` accepts a
`compromisedSince` before the creation of the key (probe: revoked at its creation time, compromised since four days earlier)
and `Validate` accepts it. Neither can wedge the set, but a revocation record that says a key was compromised before it
existed is nonsense that an auditor reading the table will see. Also, `CompromisedSince` never reaches the trust file (no such
member), so for the verifier a revocation has no date; the design text (3.3) speaks of "provably older than compromised-since".
A fix must refuse `ActivatedAt` before `CreatedAt` in `Activate` and in `Validate`, and clamp or refuse `compromisedSince` before
`CreatedAt`; the second point is for the design.

### K3-06 (low) One active key and a clock that steps back by a second stops signing

`MaySign` for `PurposeCheckpoint` requires `now >= ActivatedAt` (`state.go` about :726), so a set with one active key,
activated a minute ago, returns `ErrNoActiveKey` from `Signers` if the next call carries a time one hour or one second
earlier (probe: one hour). The doc states that the caller must treat that as "do not sign". It is the right answer for the
design, but a step of a few seconds by a time service at the activation instant halts checkpoints of every log until the clock
catches up, with an alarm text ("ErrNoActiveKey ... damaged") that is also the text of real damage. The caller can tell the
two apart only by calling `Validate` (damage) as well. A fix could give `Signers` a small stated tolerance, or the doc should
tell the daemon to use a monotonic reference and to distinguish the two cases in its alarm.

### K3-07 (low) Test quality: mutants that survive and tests that compare a value with itself

Survivors of my mutants (default run, both packages):

| Mutant | Meaning |
|---|---|
| `MaxActivationLeadDays = 366` changed to 3660 | the boundary test computes its dates from the constant (`secondfix_test.go:247`), so it moves with the constant; a `+1 day` mutant of the limit is killed, the constant itself is not |
| `DueForRetirement` without `otherActive` | nothing tests that a key whose partner is not in force is not due |
| `Validate`: minimum overlap of one day removed | two active keys with an overlap that ends at their activation are accepted |
| `Validate`: `OverlapEnds` on a non-active key; retired-before-activated on a revoked key | the first is redundant with the active-count rule; the second is a missing test |
| `parsePlain` without `defer clear(seed)` | the doc says a test scans this path and finds nothing; the mutant that stops the clearing is not noticed, so the test cannot fail on the thing it names |
| `Append` without its per-tile context check; `Audit` without either of its two checks | each is covered by the other (and by `PutTile`); not a gap alone |
| `flushExisting` without the file flush | equivalent in practice (see K2-01) |

Also: `TestVerifierTestsRan` still passes without a verifier (it writes a notice to standard error). That is what the doc says
and is acceptable only if the integrator's script sets `KINGRES_REQUIRE_VERIFY=1`; the repository has no such script that I
could see, so a plain `go test ./...` of several packages passes with the whole verifier matrix skipped and nothing visible.
Ledgerkeys carries its own copy of the same notice test. No test needs the network; the default runs are 7.7 s and 1.4 s on
Linux and 19.7 s and 4.9 s on Windows.
A fix must kill the survivors that are not equivalent (constant, `DueForRetirement`, minimum overlap, seed clearing) and keep
`KINGRES_REQUIRE_VERIFY=1` in the run script of the integration.

### K3-08 (low) The unchecked Add still exists, and nothing tells a caller that used it

`ledgerkeys/state.go:413`. `Add` succeeds for a key that collides with a key of the set; `Validate`, `TrustEntries` and `TrustFile`
accept the set (probe: all eight matrix rows were built with `Add`; each trust file was written without error, and the verifier
refused the fresh path in two rows and the held path in six). The collision shows up at `SignNote` (only if both keys sign) or in
the verifier of an auditor. `TrustFile` knows no origin, so it cannot check. The package doc names `AddChecked` as the call for a
caller that publishes logs; the type of `Add` is the shorter and the first in the list.
A fix must make the checked form the default (rename the unchecked one to a name that says so, or make `TrustFile` take the
origins and refuse a collision), with a test that the daemon-facing path cannot reach `Add`.

### K3-09 (note) EPERM is classed as "no hard links"

`dirstore_unix.go:33`. link(2) gives EPERM also for an immutable or append-only target directory and for the
`protected_hardlinks` rule; the comment rules the second out for a temporary file the process just made, but not the first. A
publication directory that has the immutable attribute would be reported as a file system without hard links. Nothing is
published in either case, so the harm is the wrong advice. I could not test against FAT, exFAT or a network share; the Windows
codes (1, 17, 50, 120) are as the documentation of `CreateHardLink` lists, injected, not run against a real volume.

### K3-10 (note) What `Validate` cannot see: values that change the trust file

The sweep: no single-field damage of a valid set that changes a state, a flag or a presence is accepted (state flips, revoked or
retired fields, overlap on the wrong key, two active keys without exactly one marker: all refused). What is accepted and changes
the trust file the verifier reads is a change of a value with no second copy: `ActivatedAt` (not_before), `MaxTreeSize` of a
retired key (the bound; set to 0 it becomes unbounded), `Label`, `Public`, and the order key `CreatedAt`. The most
consequential is `MaxTreeSize`: a flipped or raised bound silently widens what a retired key may sign. This is inherent without a
copy, so the caller must keep one: the daemon should store the published trust file (or its hash) with each transition and
compare `TrustFile(...)` with it after every load. Also not in the trust file at all: `RetiredAt`, `RevokedAt`,
`CompromisedSince`, `OverlapEnds`, `Reason`.

### K3-11 (note) Cost of the repair after a crash, and a duty the doc does not state

The repair is not a denial of service: a replay of 100,000 leaves costs 812 puts, 2,467 directory flushes and 812 file flushes,
40 s on this disk against 32 s for the first write (the put reads the existing tile, flushes the file and up to four
directories). It scales with the round that was cut, and a round that large is the caller's choice.
The doc does not say that nothing in the package stops the caller from signing a checkpoint that is not the log's own: `SignCheckpoint`
signs any `Checkpoint` it is given (any size, any root), and `Open` does not look at the stored checkpoint, so "never sign a
smaller tree than the one published" and "never two roots for one size" are duties of the caller (see the list below). The
function `Log.Checkpoint()` exists; the doc should say that it is the only input to `SignCheckpoint`.

## The duties of the caller

This list is read from both `doc.go` files and from what I found. "Easy" means the API lets the caller forget it with no error.

| # | Duty | If forgotten | API |
|---|---|---|---|
| K1 | Persist the `Set` after every transition (`ledger_signing_key`); the package keeps nothing | key states roll back after a restart: a retired key signs again, a revoked key is active; a rolled back set is valid and `Validate` cannot see it | easy: pure functions, the old `Set` stays usable, no version number |
| K2 | Serialise transitions | two tasks start from one stored set, the second write erases the first transition (a lost revocation) | easy: no sequence number or compare-and-swap helper |
| K3 | Use a trusted clock, never a stored or user time, and compare it with the latest record; the package refuses zero, years outside 1 to 9999 and an activation more than 366 days after the latest record, nothing else (K3-03) | a key activated in a wrong time is not in force for up to a year (a single key: the daemon cannot sign), or not-before in the trust file sits ahead of the auditors' clocks | hard to avoid the first, easy the rest |
| K4 | Before a key is added: `AddChecked` with every origin that exists or will (every log of every epoch), never `Add`; recheck every key when a new origin (new epoch or prefix) appears with `CheckSetKeyIDs` | a collision stops every auditor's state file for that log for ever, also when one key is revoked (matrix: held path status 2); cannot be cured | easy: `Add` exists and nothing complains (K3-08); `AddChecked` is closed by an old pair (K3-02) |
| K5 | `Validate` after every load; an error is an alarm and no signing; `ErrNoActiveKey` from `Signers` is "do not sign", never an empty list | a damaged row is used as it is (`Signers`, `Retire` do not call `Validate`) | easy for `Validate` (optional); `Signers` errors on empty |
| K6 | Keep a copy of the published trust file (or hash) and compare after load (K3-10) | a changed bound or label is invisible | easy: not in the doc |
| K7 | Choose the `TreeBound` at `Retire`: at least the largest size of any log the key ever signed (all logs, all epochs), or `Unbounded` | too low: the verifier refuses the newest checkpoints a retired key signed, for everybody; unbounded: a leaked retired key can sign larger trees for ever | forced to decide (the zero value is refused), not to decide right |
| K8 | At each rotation re-sign the latest checkpoint of every log that no longer grows (`ResignCheckpoint`, `trusted` = every key of the file); check the note first rests on a key that may vouch (K3-01) | a log that stopped keeps only the old key's signature: fine while the old key is retired and bounded, unreadable if it is revoked | easy: nothing lists the logs to re-sign |
| K9 | During an overlap give `SignCheckpoint` every signer `Signers(now)` returns (two lines) | one line only: auditors who still hold the old trust file (or only the new) cannot verify it | easy: the signers are a variadic list |
| K10 | Build and sign the key-added, key-retired, key-revoked statements (design 3.2.4) with `CanSign` and log them; the package builds none | transitions with no audit trail; auditors who pinned one key never learn the new one | easy |
| K11 | Poll `DueForRetirement` and retire; alert on a key past its overlap | two keys sign for ever, a retired key is never retired | easy: nothing is automatic and nothing alerts |
| K12 | Publish the trust file entries; delete the key file of a retired or revoked key on every holder; back up the key files and the passphrase (the doc says nothing about backup) | auditors on an old trust file; a retired key that leaks later; a lost active key with no second active key means no signing | easy |
| K13 | Alert on: `Validate` failure, `ErrNoActiveKey`, a due retirement, `ErrTooManyKeys` and 14 or more keys, `ErrKeyIDCollision`, every revocation, `ErrUntrustedLine`, `ErrCheckpointBusy` for longer than a few tries, `ErrConflict`, `ErrTilesAhead` outside a restart, `ErrHardLinksRequired` | silent stops | easy: errors are typed but no alert hook |
| K14 | Never automatically: revoke, choose `Unbounded`, add a key with unchecked `Add`, activate a stale next key, re-sign a note that no vouching key signed, use `AllowTilesAhead` on a normal start, remove temporary files while a write is running, delete tiles after `ErrConflict`, take a time from outside | each of them is irreversible or hides a divergence | easy: every one is a plain call |
| L1 | One writer per log (the package takes no lock) | two writers: the second at another width succeeds silently, the checkpoint is last writer wins; the fork is found late | easy |
| L2 | Round order: `Append`, persist the frontier (with size and root, one atomic write), sign, `PutCheckpoint` | checkpoint ahead of the frontier after a crash; the frontier ahead of tiles is impossible, tiles ahead are repaired | easy: three independent calls |
| L3 | Crash repair: `Open` with `AllowTilesAhead`, append the same leaves again; other leaves give `ErrConflict`, which is a stop for the operator | rebuilding the round with another closing time or order leaves a log that cannot be extended | easy to get wrong: the leaves must be reproducible |
| L4 | `Open` with the stored frontier, size and root; for the first round the empty frontier | `ErrFrontierRoot` or a log that starts from nothing over existing tiles | moderate |
| L5 | Append once per checkpoint | no partial tile for the sizes in between: a reader of an earlier size cannot be served | easy |
| L6 | Sign only `Log.Checkpoint()`; never a smaller tree than the one published, never two roots for one size (K3-11) | a rollback or a fork signed by the server itself | easy: `SignCheckpoint` takes any `Checkpoint` |
| L7 | On `ErrConflict` stop the log and alert; never repair | an attacker or a bug makes the log diverge and the daemon goes on | easy to automate wrongly |
| L8 | Run `Audit` from time to time; the verifier reads no entry bundle | a damaged bundle is found by nobody; cost linear in the size | easy |
| L9 | Keep every partial tile of a size that has a checkpoint; there is no pruning | a reader of an old checkpoint finds no tile (status 3 for auditors, an alert on their side) | easy: nothing deletes, but nothing says "keep" at the backup or cleaning layer |
| L10 | A file system with hard links and honoured flushes; `NoSync` false in production (the zero value is right); on Windows the directory flush is missing | no hard links: `ErrHardLinksRequired` at the first put; a lying disk: lost tiles after power loss | moderate |
| L11 | A `Reader` for a request or a batch, then drop it (no eviction; a long-lived one holds up to a million tiles at 2^28 leaves) | slow memory growth in the server | easy |
| L12 | `RemoveTemporaryFiles` only at start, with no write running | it deletes the temporary file of a running put | easy |
| L13 | Pass a context and expect it to be honoured only at the start of a store call, at each tile of `Audit` and in the checkpoint retry | a hung file system call is not interrupted | by design |
| L14 | Check divergence with `CheckLink` and the leaves it reads (design 4.7) | the publisher does not monitor itself | easy |

## What held under attack

- The tree and the tiles: every size of the sweep, written in uneven batches, has the root of my naive tree; `Audit` passes;
  `checkpoint` and `consistency` accept with one line, two lines, a retired key with `AtMost(size)` and `Unbounded`, a revoked
  key beside a good one; `AtMost(size-1)` is refused with status 1.
- The key set against my model: 120,000 operations, forward, back and far ahead, zero and edge times: every accept and refusal,
  every field and every `Validate` and `TrustEntries` result equal. Backward activation, the clock step at a rotation, and the
  retirement of a key that is active and not in force leave the set with a signer and a legal way forward (`CanSign` of the
  not-yet-in-force key answers no and `CanSign` of the other answers yes; `Retire` records the activation time, and the trust
  file keeps the key's own `not_before`, so no checkpoint it really signed is described as signed before it was valid).
- `CheckSetKeyIDs` equals the verifier in the eight rows (retired+retired, revoked+revoked, retired+revoked both ways, next
  listed and not listed with a retired or a revoked partner): "collision" exactly when some path refuses, `Fresh` exactly when
  the fresh checkpoint is refused, `Held` for every listed pair (status 2 on both held paths even when both keys are revoked and
  the note is by a clean key).
- `ResignCheckpoint`: witness cosignatures under other names kept, a line under the log's name by an unknown key refused
  (`ErrUntrustedLine`), a note at the 32-line limit refused with an error, no new signer returns the same bytes, the check
  covers every trusted key (mutants killed).
- `PutTile`: a repeated put flushes as documented; link errors classified by cause on Linux and by code on Windows as the
  documentation lists them (injected); the cost of a repair is the cost of the first write (K3-11).
- `PutCheckpoint`: a directory or read-only target is named at once on Linux and Windows with no temporary file left; a held-open
  file gives the typed busy error after the retry.
- Contexts honoured as the doc says; `Required` counts first and allocates once; the limited readers read at most limit+1 bytes
  of a file that grows.
- Fuzz, 150 s over six parsers: no failure. Edge times (year 1, 9999) are read by `trust show`.

## Verdict

Fit to be wired into the daemon. Nothing I found makes the verifier accept damage, nothing breaks the crash procedure, every
K2 item is fixed (K2-11 only in its doc), and everything the packages wrote was accepted by the built verifier in every case I
tried. The new findings are of three kinds: one is a real hole at a trust boundary (K3-01, a revoked key's note can be made
genuine by `ResignCheckpoint`), two are design faults that will bite in operation (K3-02, a checked add that an old collision
closes for good; K3-03, a clock guard that one far time removes), and the rest are low or notes.

Another review round of the libraries is not needed before the wiring. I recommend that K3-01 and K3-02 are fixed in the
libraries before the daemon calls `ResignCheckpoint` and `AddChecked` (both are small and local), and that K3-03, K3-08 and the
duty list travel with the wiring: the daemon must hold the clock check and the collision check itself (duties K3 and K4), keep the
published trust file with each transition (K6), and sign only `Log.Checkpoint()` (L6). K3-04 to K3-07, K3-09 and K3-10 can follow
with the wiring.

## What I did not check

- Real power loss; the effect of the flush order on a real disk (hooks only). FAT, exFAT, network file systems and any file system
  without hard links; the Windows link codes against a real volume.
- Windows junctions, symbolic links (no privilege), ACLs, `\\?\` paths; macOS and the BSDs (the classification of ENOTSUP there).
- A remote `TileGetter`, an external signer, the race detector, concurrent use of `Log`, `Reader` and the stores.
- The verifier's `proof` and bundle commands; sizes above 66,000 leaves beyond my sweep (65,537) and the long tests.
- Fuzzing beyond 25 s per target and for the targets I did not name; timing channels; the Go runtime's own copies of keys.
- The combined removal of the two `Audit` context checks, the minimum-overlap and seed-clearing mutants beyond showing that they
  survive; the SQL and C parts of the extension and the design text outside the sections named.
