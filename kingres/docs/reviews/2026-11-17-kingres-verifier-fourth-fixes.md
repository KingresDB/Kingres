<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->

# Review of the fourth fix round of `kingres/verifier` (the stand-alone verifier of the ledger)

Reviewer: an independent reviewer. Date: 2026-11-17.

## Scope and method

Reviewed: the fix round that answers L10-04 to L10-12 and L10-16 to L10-18 of
`2026-11-17-ledger-phase7-steps-1-2-third-fixes.md`, as committed (the code was exported with
`git archive` and worked on outside the repository; the previous state of the code was exported
the same way for comparisons and for the downgrade test). The three earlier reviews
(`2026-11-16-ledger-phase7-steps-1-2.md`, `...-fixes.md`, `2026-11-17-...-second-fixes.md`) were
used as the list of things that must stay fixed; the suite, which contains their tests, passes.

Standard applied: the verifier never prints VERIFIED or CONSISTENT, and never exits 0, for
something it did not verify; and nobody who holds no trusted key can bring the auditor's state into
a condition from which the auditor's protection is lost or from which no later run can go on.

What was done:

- the suite on Linux (WSL, Ubuntu 24.04, Go 1.27, Linux 6.18 WSL2 kernel) and on Windows 11;
- an independent model of the state with its own generator (4 keys, 7 standings, 3 clocks, 5 sizes,
  random signer subsets and junk lines, 400 sequences of 9 runs through the real command layer);
- attacks on the canonical form: subsets of verified lines, junk lines, a later revocation;
- the private path and the lock: a real run of every path form that could be tried by one account,
  and an in-process test of two lock holders;
- the proof reader: 82,000 single-byte mutants of four real proofs through both implementations,
  a file of 1 GB with one huge element, a named pipe, a device, `/dev/stdin`, a file named `-`;
- the output paths with a closed standard output, `/dev/full`, a closed pipe, both streams closed;
- the escape rule decoded by a decoder written from the README (200,000 random inputs);
- 24 cells of the exit-status table produced by hand on Linux;
- the claims "fails without the fix" for eight fixes (the code was changed in a copy and the named
  packages were run), and a mutation run of 26 hand-made mutants (24 applied) of the state code;
- `FuzzStateFromNote` for 30 minutes with `-parallel=2`;
- the downgrade: a state written by the new program read by the previous one and the other way round.

No second local account was available (`sudo -n` asks for a password), so everything that needs
another owner is simulated by the tests of the author or by file modes only; this is said again in
"Not covered". The tests and programs I wrote for this review are not part of the repository.

## Summary

| Severity | Count | Findings |
|---|---|---|
| critical (as defined for this review) | 1 | L12-01 |
| high | 1 | L12-02 |
| medium | 3 | L12-03, L12-04, L12-05 |
| low | 3 | L12-06, L12-07, L12-08 |
| note | 2 | L12-09, L12-10 |

No case was found in which VERIFIED or CONSISTENT was printed for something that was not
verified, no rollback or fork was accepted, and the streamed proof verification never gave a
verdict that the buffered one does not give. The critical finding is of the third kind of the definition
("a state that a party without a key made unusable or weaker"): a publisher without a key chooses
which of the genuinely verified signature lines a state keeps. It needs two cosigning keys and a
later revocation, and it is recovered by the documented rebase, so in practice it is a medium; it is
listed as critical because the round claims exactly the opposite ("never makes it accept less").

Baseline: Linux 282 top-level tests passed, 1 skipped, 0 failed (`go test -v ./...`, `--- PASS`
lines); Windows 251 passed, 15 skipped, 0 failed (255 and 16 with the four test files of this review
that were added there). These are the numbers of the integrator.

## Fate of L10-04 to L10-12 and L10-16 to L10-18

| Finding | Fate | Evidence |
|---|---|---|
| L10-04 state refused for ever after a key is added | fixed for lines that nobody signed; a variant remains (L12-01) | junk lines are not stored; my model: 3,600 runs, held rule and new rule agree with the code; a publisher that drops lines still decides the signer set of the state |
| L10-05 publisher without a key stops the state from advancing | fixed | a line of a retired or unknown key is ignored on a held note, dropped on a new one; model run |
| L10-06 private check looks at one directory | fixed on Unix, with one divergence (L12-06) | `..`, `/proc/self/cwd`, a link to a 0777 directory, a relative path through the parent of a 0777 directory: all refused with status 2; `sl/../s` with a relative path is stored lexically |
| L10-07 proof cap | fixed for the size; the memory bound is not what the README says (L12-05) | 1.06 GB proof element: RSS 1.04 GB; the honest year-old proof is streamed |
| L10-08 status 0 when the verdict was not written | fixed for `/dev/full`, a failing write, a closed pipe; not fixed for a closed standard output (L12-04) | `>&-` gives status 0 and no output |
| L10-09 publisher without a key forces status 4 | fixed for added lines; a variant with dropped lines remains (L12-03) | 4 keys: 7 of 15 subsets end with status 4 |
| L10-10 clip before escaping | fixed (one cut); the README describes it wrongly (L12-07) | the cut is made before escaping, the README says after |
| L10-11 backslash escape in the README | fixed | a decoder written from the README read back 200,000 random inputs and the fixed hostile strings |
| L10-12 lock file not private | fixed for the file; the replacement is racy (L12-02) | two holders at once, found in two runs of up to 200 rounds each |
| L10-16 failed `trust init` leaves a file | fixed | bad fingerprint: status 2, no file |
| L10-17 `blocks` does not say whether a digest anchors | fixed | `anchored_to_digest` true with `--digest`, false with `--chain-id` |
| L10-18 nesting limit | fixed in the code (32 exactly); the test that guards it cannot fail (L12-08) | changing the constant to 33 leaves every test passing |

## The state

### The canonical form

`signednote.Canonical` writes the text of the note (the bytes that `Verify` checked, copied from the
parsed note, not re-built), a blank line and the lines of the counted keys sorted by name and key
id. I read the function: the text is `n.Text` byte for byte (the bytes `Verify` checked); my model and the tests of the author exercise it; the note
parses again; it verifies under any set that holds one of its keys. Extension lines are part of the text and are kept unchanged. Trailing blank lines (an empty line in a checkpoint)
and CRLF (a control character other than newline) are refused by `Parse` before the note reaches it,
so there is no canonical form of such a note.
Two trusted keys with one name and one key id and different public keys are refused as ambiguous
(`indexKeys`); in this program every key name is the log origin, so a collision of the four-byte key
id between two trust keys is the only way to reach that and it needs a trust file of the auditor.
A line of a key that the attacker controls and the trust file does not hold is ignored as unknown.

Held notes (`VerifyHeld`): a line that names a key of the file and does not verify is ignored with a
note; a held note with no verifying line is refused (status 2) in `checkpoint --state` and in
`consistency --old`, both of which use `verifyHeld`; a held note whose every verifying signer is
revoked or beyond its `max_tree_size` is refused with the advice to rebase. This is the same in
both commands. The trust file in this version has `revoked` and no `compromised_since`.

### What a publisher without a key can still decide

A note signed by two keys stays valid when one of the two lines is removed. The state keeps the lines
that were served and verified. A publisher that serves only the line of the key that the auditor is
about to retire or revoke makes the state hold only that key (L12-01). The same freedom, applied to
a rollback, gives a different evidence file for every subset (L12-03).

### Trust-file changes over time (my model)

I wrote the model without reading the author's property test. Inputs of a run: a trust file over
four keys, each absent, usable, retired (`not_after` in 2020), not yet valid (`not_before` in 2999),
revoked, limited (`max_tree_size` 400) or limited and retired; a clock of 2026, 2019 or 3000;
a note of size 255, 300, 400, 513 or 700 signed by a random subset of the four keys, with junk
lines (zero signatures) for some of the keys that are unusable or unknown. The model says: the new
note is judged first (no usable key: 2; no counted signer or none within its size limit: 1); then the
held state needs one signer that is not revoked and within its limit at the stored size (else 2);
then a smaller size is 1 (or 4), the same size is 0 and unchanged, a larger size is 0 and the state
becomes the counted signers of the new note. After every run the size of the state and the number
of signature lines stored are compared with the model.

400 sequences of 9 runs (3,600 runs) agree with the code on the exit status, the size in the state and
the number of signature lines; 894 runs refused a held state because the random trust file no longer
held a usable signer of it, and in each of them the state was unchanged. The state never went down
and no fork was accepted. Not in the model: a changed origin prefix (the code refuses with status 2,
the key name of a note is the origin, so a changed origin leaves no key that can verify; the advice
is to rebase), two epochs, the 16-key limit, a trust file replaced by an attacker with write access to
the auditor's directory (the README places this out of scope: the state and the trust file are the
auditor's own).

### Downgrade

A state written by the new program holds only verified lines; the previous program reads it
(checkpoint with `--state`: "same as the checkpoint in the state file", status 0; `consistency --old`
on the state: status 0). A state written by the previous program is read by the new one. No dead end
in either direction in the cases tried (one signer, no junk).

### Evidence

The evidence file name is `<state>.conflict-<size>-<root>` and its content is the canonical note, so
junk lines cannot change either (tested: 10 junk variants, one file). The two variants that remain
are in L12-03: the subset of verified lines, and one file per historic checkpoint that a publisher
replays (each replay of an older genuine checkpoint is a rollback and saves a file of at most a few
hundred bytes; the count is bounded by the checkpoints that were ever signed, 8 names each).

## The private path

Produced on Linux (WSL) with one account, in directories I made (modes 0700, 0711, 0777):

| Typed `--state` | Result |
|---|---|
| `s` in a 0777 directory | status 2, "can be written by group or others (mode 0777)" |
| `./s`, `/proc/self/cwd/s` in a 0777 directory | status 2, the same message |
| `../shared/x` from a 0700 directory, `sl/x` with `sl` a link to the 0777 directory | status 2 |
| `s`, `/proc/self/cwd/s2` in a 0700 directory | status 0 |
| `h/s` with `h` of mode 0711 | status 0 (accepted on purpose; others cannot create names there) |
| a hard link `hl` to the state in the same directory | status 0, unchanged; the lock is per directory |
| `sl/../s3` (relative, `..` after a link) | state written at `priv/s3` (lexical), not in the parent of the link target (L12-06) |

The lock: `Acquire` replaces a lock file that is not private (mode with group or other bits, or
another owner) by removing it and creating a new one. Two runs can both see the bad file; the second
removes the new private file that the first created and locks a third file. I ran four goroutines on
a directory that held a lock file of mode 0644, repeated up to 200 times: two holders were inside at the same
time, in the 11th round of the first run and in the 2nd round of the second (L12-02). The path walk itself: a component swapped between the walk and
the open needs write access to a component that the walk required to be owned by the auditor or root,
or to be sticky (then only its owner can rename it). I did not find a way around this with one account.
I could not run a swapper with another account; the swap loop of the task was not run
(see "Not covered").

## Proofs without a size limit

- Streamed against buffered: for four real proofs (`acc1`, `acc5`, `evt1`, `acc8`) every byte was
  xor-ed with 0x01, 0x20, 0xff and 0x0f, deleted, and preceded by a space, and five tails were appended
  (`x`, `{}`, two newlines, a NUL, a space and a newline): 82,292 mutants. The two implementations agree
  on the verdict and on the failing step in every one.
- Named pipe and device as `--proof`: status 2, "is not a regular file (it is a named pipe / a
  device)". `/dev/stdin` and `/proc/self/fd/0` with a regular file as standard input verify (they are
  regular files; the second open starts at the beginning). A file named `-` is read as a file.
- What is hashed: every byte of both readings through `io.TeeReader`; a file that differs in any byte
  has another SHA-256, so equal hashes mean equal bytes. A test of the author fails when the comparison is removed
  (below). A swapper process was not run (the argument above is by the hash).
- Memory: a proof file of one chain element that holds 16,000 strings of 65,000 bytes (1,040,064,023
  bytes) ends with `FAILED at step parse` in 4.8 seconds and a peak resident set of 1,036,732 KiB;
  195 MB file: 199,420 KiB. The README says memory is "a constant plus a small multiple of the largest
  single value" and that the proof "does not depend on its size". The element is the largest single
  value, and the limit on an element is 16,384 values of 65,536 bytes, about 1 GiB (L12-05).
- Time: the check is linear; a file that does not end is read until the disk does (documented).

## Output

Linux, the built program, the fixture of the tests (a log of 300 entries, one key):

| Case | Status |
|---|---|
| `checkpoint` normal | 0 |
| standard output is `/dev/full`, text or `--json` | 2, one line on the standard error: "could not be written ... do not rely on this run" |
| both streams `/dev/full` | 2 |
| a failing verdict with `/dev/full` (a bad note, a missing directory) | 2 (the input error); a verdict of 1 keeps 1 by design |
| `version`, `--help`, `row-hash --help`, `trust show`, `trust init --help` to `/dev/full` | 2 |
| the reader of a pipe exits at once | 141 (killed by SIGPIPE; also when the shell ignores SIGPIPE), never 0 |
| the reader takes 5 bytes (`head -c 5`) | the output is cut by the reader (`VERIF`), the program ended with 0 because the whole output fit in the pipe: the program cannot know |
| standard output closed (`>&-`), text or `--json` | **0**, nothing printed anywhere (L12-04) |
| standard output and standard error both closed | **0** |
| `--state` run with `/dev/full` | 2 and the state was updated; the standard error says so ("the state file may have been updated"): documented |

The escape rule: a decoder written from the README (two backslashes, `\xNN`, `\uNNNN`, `\UNNNNNNNN`,
the cut marker ends the text) read back 200,000 random strings of 0 to 60 bytes with ASCII, controls,
backslashes, `x`, `u`, `U`, `.`, newline, lone continuation bytes and lead bytes, and strings of 2,000
to 2,200 bytes (read back as a prefix). A value made only of continuation bytes that is longer than
2,048 bytes prints as the cut marker with "0 of 3000 bytes shown", which is honest. No fake headline
can be started on a new line: a newline is `\x0a`. The cut is made before escaping, not after (L12-07).

The exit-status table: 24 cells were produced on Linux, each with its status as in the README:
proof 0, 1 (a byte of the proof changed), 2 (no `--digest`); row-hash 0, 2; digest-chain 0, 1 (two
chains), 2; blocks 0, 1 (a damaged export), 2; checkpoint 0, 1 (`--min-size`), 2 (no `--trust`),
4 (a rollback with all eight evidence names taken); consistency 0, 1 (a wrong old root), 2, 3 (no tiles
in the source); trust init 0, 2; trust show 0, 2; trust alone 2. Status 3 of `checkpoint` was not
produced: a missing source directory is status 2, by the README ("a directory you named").

## The tests of this round

- Property test and fuzz target: my own model (above) found no disagreement. `FuzzStateFromNote`
  ran 30 minutes with `-parallel=2`: 205,869 executions, 209 corpus entries, no failure (rc 0). The target checks that a run with status 0 leaves a state its own trust file reads back; it cannot see L12-01, which keeps that property (the state is readable under the trust file of the run that wrote it).
- Memory test: the bound that the author is said to have loosened is the blocks case in
  `memory_test.go`: it was `maxBlocksSize / 64` and is `16 << 20`; the numbers are equal (the constant
  was 1 GiB), so it is not looser. `TestPeakResidentSetOfAProofDoesNotGrowWithItsChain` fails above 16
  MiB over a small input and above 8 MiB of growth, which are real bounds, but the hostile shapes of the
  memory tests do not include many large strings in one element (L12-05).
- "Fails without the fix" (the code was changed in a copy and the named packages were run):

  | Fix removed in a copy | Result |
  |---|---|
  | the canonical form keeps every line that was served | killed: `TestCanonicalDropsWhatNoKeyVerified`, `TestStateKeepsOnlyTheLinesThatVerified`, `TestRetiredKeyLineDoesNotStopTheState`, `TestEvidenceIsOneFileWhateverTheUnverifiedLines`, `TestStateSurvivesEveryChangeOfTheTrustFile` |
  | a held note with a wrong line is refused as before | killed: `TestParseDetailsAndMessages`, `TestCanonicalDropsWhatNoKeyVerified`, `TestStateOfTheEarlierVersionIsStillRead` |
  | a failed write of the verdict does not change the status | killed: `TestEveryExitSiteHasARowAndAScenario`, `TestFailedWriteOfTheVerdictIsNeverStatus0`, `TestVerdictToDevFull` |
  | no second look at the name after the lock | killed: `TestAcquireCheckedLooksAgain` |
  | no comparison of the hashes of the two readings of a proof | killed: `TestStreamedProofWithOtherBytesAndTheSameCountIsRefused`, `TestProofThatChangesBetweenTheTwoReadingsIsRefused` |
  | a lock file that is not private is kept | killed: `TestLockFileIsPrivateAndReplaced` |
  | the nesting limit is 33 | **survives** (L12-08) |
  | the read-back of the state before it is written | **survives** (L12-08) |

- A mutation run of my own on `signednote`, `statefile` and the state part of `cli`: 26 hand-made
  mutants; 24 applied. The survivors are classified below.

Of the 24 state mutants that applied (the other two of the 26 patterns did not apply and were not
counted) and the eight fix removals above, two mutants survived and two fix removals survived:

- `buf.Len() > maxStateFile` replaced by `false` in `encodeState`: equivalent. A note that passed `Parse`
  is at most 64 KiB, its JSON form at most twice that plus a few hundred bytes, less than the 144 KiB of
  the limit, so the check cannot fire; it is a guard of the invariant and a test cannot reach it.
- `st.Mode().Perm()&0o077 == 0` replaced by `&0o007 == 0` in `openLock`: **not equivalent**. A lock
  file of mode 0660 or 0640 (group access) is then kept, while the README says no access for group or
  others. The test of the lock uses modes with other bits only. A gap of the tests (low): add a case of
  0640.
- the nesting constant and the read-back guard: above (L12-08).

Twenty-one of the 24 mutants were killed by a test that names the property (for example
`TestStateRollbackAndSameSizeFork` for the removal of the fork case, `TestConflictNamesAndTheContentTest`
for the exclusive create and the number of names, `TestStateRecordStrictness` for the duplicate member, the
empty file and the size of the record, `TestCheckPrivate...` for owners, modes and the number of links); one
more was killed by a build or panic with no test name (the ambiguous key index). I did not take twenty
survivors of the author's list, which is not in the repository; I made my own mutants of the state code,
and the two that survived are classified above.

## Windows

The README says that the private-path check is not made on Windows, and says what to do instead
("a folder only you and the administrators can write, and in none of whose parents others can create
or rename entries"). That is true to the code (`CheckPrivate` returns nil). The lock on Windows is
`LockFileEx` on a file opened without following a link; there is no replacement of the lock file, so the
race of L12-03 does not exist there, but the README sentence "the lock file is checked for its owner
and mode and replaced when it is not private" is a statement about Unix only. The suite passes
(251 and 15 skipped by the integrator; I ran it once: 255 passed and 16 skipped with my four added
files, 0 failed). My state experiments (the drop of lines, the evidence names for 3, 4 and 5 keys, the
escape round trip and the 82,000 mutants) give the same results on Windows. Not run on Windows:
a junction or symbolic link at the lock name, closed standard output.

## Findings

### L12-01 Critical (by the definition of this review): a publisher without a key chooses the signer set that the state keeps

Steps (Linux and Windows, the test code of this review; the construction is by hand below):

1. A trust file with two keys, k1 and k2, both usable, origin prefix P.
2. A log of 700 entries with its tiles. The genuine checkpoint of size 300 is signed by k1 and k2:
   text `P/c/<chain>/e1`, `300`, root, then two lines "em dash, space, origin, space,
   base64(key id, signature)" (the key id is the first four bytes of SHA-256 of origin, 0x0a, 0x01 and the
   public key).
3. The publisher serves the same note without the line of k1. `checkpoint --state S` verifies it
   (one trusted signature) and stores the canonical note with the line of k2 only. Exit status 0.
4. The auditor revokes k2 in the trust file (`"revoked": true`) and keeps k1. The publisher serves the
   genuine checkpoint of size 700 signed by k1.
5. `checkpoint --state S` with the new trust file: status 2, "cannot be used as your earlier checkpoint:
   key "k2" ... is revoked in the trust file ... Deleting it would give up the protection ...".
   With the note served whole in step 3, the same step 5 ends with status 0 and "extends the checkpoint of
   size 300 in the state file".

Observed: the state of the first case is unusable until the auditor rebases; the second case works.
Expected: by the README ("Nobody who holds no trusted key can put a byte into the state ... never makes it
accept less"), what a publisher without a key serves must not decide whether a later trust file can read
the state. Place: `signednote.Canonical` is called with the signers of this run only (`canonicalOf`,
`applyState`); on an unchanged checkpoint nothing is merged (`write = false`). The same works with a key
that is later limited by `max_tree_size`. Not a rollback or a fork, and the auditor can go on by the
documented rebase with `--min-size`, which is why the weight in practice is lower.
Direction: when the served note has the same text as the stored one, merge the verified lines of both
(and rewrite the state, which is a change of signatures and not of size); and say in the README that a
state holds the signatures that were served to it. A stronger rule is to keep every line that verifies
against any key of the trust file, counted or not (a retired or limited key included), so that
what is stored depends on the key set and not on the counted set, though it still depends on what was served.

### L12-02 High: two runs can hold the lock at the same time when the lock file is not private

Steps: make the directory of the state (mode 0700) and a file `.kingres-state.lock` in it with mode
0644 (a lock kept from a time the directory was shared, the case the README describes). Start four
runs of `checkpoint --state` at once (I called `statefile.Acquire` from four goroutines in a test and
counted holders: two were inside at once in the 11th round of one run and in the 2nd of another). Cause: `openLock` opens
the file, sees that it is not private, closes it and removes the name; a second run that opened the old
file before the first removed it removes, after its own check, the new private file that the first created, and
creates a third; each locks a different file. Nothing checks after `flock` that the locked file is the one
that the name leads to.
Observed: two holders of one state. A holder that read size 300 before the other wrote 700 can write
500 after it, so the state can go backwards: the protection that the lock is for is lost, with no attacker.
Expected: one holder (README "What the lock does and does not protect").
Place: `internal/statefile/sync_unix.go`, `openLock`. Direction: never remove a lock file; use a private
temporary name and `rename` over the old name only when no run holds it, or fail with status 2 and say to
remove it by hand; after `flock`, compare `fstat` of the descriptor with `stat` of the name and retry when
they differ. Severity high rather than critical because it needs a lock file that is not private at the start.

### L12-03 Medium: a publisher without a key can force status 4 (and 8 evidence files) when a note carries four or more signatures

Steps: a trust file with four keys; a state at size 700 from a note signed by all four; the log of 700 entries;
serve, one after the other, the genuine checkpoint of size 300 signed by each non-empty subset of the four
keys (15 subsets, no key needed: lines are only removed).
Observed (Linux and Windows): 3 keys: seven runs, status 1, 7 evidence files. 4 keys: eight runs of status 1
and seven of **status 4** ("every name for the evidence is taken"), 8 evidence files. 5 keys: 8 of status 1 and
23 of status 4. Evidence is claimed as not saved, although the same fork was seen eight times. The
note is printed in base64 so nothing is lost.
Expected: the README says that "the same size and root with ten different sets of junk lines is one evidence
file, status 1 and not status 4"; junk was fixed, subsets of the verified lines were not.
Place: `statefile.SaveConflict` (`maxConflictNames`, content compared byte for byte), `canonicalOf`.
Direction: name the evidence by the hash of its canonical bytes, and keep one file per (size, root) that is
extended with the signatures of the same text (or compare the text and merge the lines), so that the
subset is not a different file; bound the number of evidence files in the directory.

### L12-04 Medium: a closed standard output gives status 0 and nothing printed

Steps: `kingres-verify checkpoint --trust T --log L --source D >&-` (also `--json`, also with the standard
error closed). Observed: status 0 and nothing on any stream; the Go runtime opens `/dev/null` for a closed
descriptor at start, so the write succeeds. With `--state` the state is updated as well.
Expected: README, "Exit statuses": "the result could not be written to the standard output (a closed
descriptor, ...)" is status 2 "in text and in `--json`, on every command". The verdict was true, nothing
was reported to the person.
Place: `cli.go` (`errWriter`), `cmd/kingres-verify`. Direction: the runtime hides the closed descriptor, so it cannot be seen
from `main`; either change the README to say that a closed standard output is not detected (a caller
must give a real file or a pipe), or detect it before the runtime does (not possible in pure Go
without a wrapper), and test the sentence that the README makes.

### L12-05 Medium: one chain element can make the verifier allocate about 1 GiB

Steps: write a file `{"chain":[{"x":[` followed by 16,000 strings of 65,000 `A`, each followed by a comma,
then `"z"]}]}` (1,040,064,023 bytes), and run `proof --proof F --digest D` with any digest.
Observed: `FAILED at step parse: $.chain[0]: missing field "height"`, status 1, 4.8 seconds, peak resident
set 1,036,732 KiB (a file of 195 MB: 199,420 KiB). Expected: README "Memory is a constant plus a small
multiple of the largest single value (one string, one streamed element)" and the hostile-shape table (13 MiB):
the table has no shape of many long strings in one element, and an element may be 16,384 values of 64 KiB.
The memory is bounded by a constant (about 1 GiB) but is the size of the element, and an auditor on a small
machine is killed with no verdict. The same holds for the part of the proof outside `chain` (16,384 values).
Place: `jsonx.DefaultLimits` (`MaxValues`, `MaxString`), `proof.ParseStream`. Direction: a limit on the bytes of
one streamed element (a header is about 400 bytes: 64 KiB is generous), and a test with this shape.

### L12-06 Low: a relative path with `..` after a link is resolved lexically

`--state sl/../s3` from a directory that holds a link `sl` to another directory: the state is stored at
`./s3` (observed), not in the parent of the link target; `kingres-verify` and the shell then disagree on
where the file is. With the absolute spelling the walk follows the link and the parent of the target is
used. README: "a `..` after a link means the parent of the target". Both the check and the use agree (no
bypass was found), so the effect is a state in an unexpected place. Place: `statefile.Resolve` and
`CheckPrivate` (the relative path is joined with the working directory, which cleans `..`). Direction:
make the path absolute without cleaning, or refuse `..` in `--state`.

### L12-07 Low: the README says the cut is after escaping; it is before

`untrusted` cuts the raw value at 2,048 bytes and then escapes: 3,000 bytes of 0x01 print as 8,227 bytes,
so one value can fill a screen (4 times its cut length). The README and the commit message say the cut is
made after escaping. The rule for a decoder holds either way. Place: `internal/cli/text.go`.
Direction: say so, or cut the escaped text at an escape boundary.

### L12-08 Low: guards and tests that cannot fail

(a) `TestNestingLimitIsExactlyTheDocumentedOne` compares with the constant `MaxDepth`; setting the
constant to 33 passes every test, while the README says 32. (b) `readBack` is a guard that "cannot be
reached" by construction; removing the call passes every test (the test that names it constructs the file
by hand and calls `readBack`). (c) the lock test uses modes with other bits only: a lock file of mode 0640 is kept (mutant `&0o007`), against the README (no access for group). Direction: write 32 as a literal in the test; add a 0640 case; either test the guard through a
note that only it can stop, or delete it and say so.

### L12-09 Note: the exit code of a verdict that was written to a pipe that closed

A reader that closes a pipe early kills the program with SIGPIPE (status 141 as the shell sees it) even
when SIGPIPE is ignored; a reader that takes only the first bytes gives status 0 and a cut line. This is
as good as a program can do, and it is the caller's cut: say it in the README next to L10-08.

### L12-10 Note: the status of `--state` after a failed write of the verdict

A run that updates the state and then cannot write the verdict ends with status 2 and the state is
updated; the standard error says so. Consistent with the README. It may be better to write the verdict
before the state moves (then a failed write could leave the state alone).

## What held under attack

- The streamed and the buffered verification agree on 82,292 mutants.
- A named pipe and a device as `--proof` are refused.
- The held rule and the new rule of notes are as the model says in 3,600 runs; the state never goes down
  and a fork is never accepted; a held note with no verifying line and one with only a revoked signer are
  refused with status 2 in `--state` and `--old` alike.
- The canonical form copies the verified text unchanged; junk lines (any number, any order) change neither
  the state nor the evidence.
- `/proc/self/cwd`, `..`, a link to a 0777 directory and a 0777 holder are refused; 0711 is accepted and does
  not allow a name to be created.
- The downgrade in both directions.
- `trust init` leaves no file on a failure; `blocks` says whether a digest anchors the chain.
- The escape rule decodes from the README.

## Not reproduced

- A rollback or a fork accepted through the private path with one account: not reproduced; the
  second look after the lock refuses a name that leads somewhere else (the test of the author fails without it).
- A swapped component between the walk and the open: the components that the walk accepts cannot be
  renamed by another user; not run with another account.
- A dead end of the state by the added lines (the earlier findings): not reproduced after the round.
- `ErrAmbiguousTrustedKey` with a four-byte key id collision: not tried (needs a collision of SHA-256
  prefixes of two keys; it needs a trust file of the auditor).

## Not covered

- No second local account (`sudo -n` needs a password): owners, a mount owned by another user, a bind
  mount, a file system without owners were not run; the 0711 holder was run with one account only.
- The swap loop against the private path and against the proof file was not run.
- Fuzz of 30 minutes was run once on Linux; not on Windows.
- A mutation run of the whole command layer; 26 mutants of the state code were made by hand, 24 applied.
- Proofs of 2^31 and 2^32 headers and a file of 10 GB: not run (disk); the time and memory were measured on
  the file of 1 GB only, and the analysis for larger files is by the code (linear time, memory bound by an element).
- Numbers and strings at the limits of the streaming reader, duplicate members and depth 32 and 33 were
  not run by me; the tests of the author exist and pass.
- Windows: junctions, closed standard output and a real concurrent run.
- Two epochs and the 16-key limit in the model.

## Baseline numbers

| System | Passed | Skipped | Failed |
|---|---|---|---|
| Linux (WSL, Ubuntu 24.04, Go 1.27) | 282 | 1 (`TestWriteBigProofForMeasurement`) | 0 |
| Windows 11 (Go 1.27) | 251 (255 with four files of this review) | 15 (16) | 0 |
