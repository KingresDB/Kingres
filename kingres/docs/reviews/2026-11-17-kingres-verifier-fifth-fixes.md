<!-- SPDX-License-Identifier: AGPL-3.0-only -->

# Review of the fifth fix round of `kingres/verifier` (the stand-alone verifier of the ledger)

Reviewer: an independent reviewer. Date: 2026-11-17.

## Scope and method

Reviewed: the fix round that answers L12-01 to L12-10 of `2026-11-17-kingres-verifier-fourth-fixes.md`, as
committed. The code of the round and of its parent were exported with `git archive` and worked on outside
the repository (Windows 11 with Go 1.27.3; WSL Ubuntu 24.04 with Go 1.27.3). The four earlier reviews
(`2026-11-16-ledger-phase7-steps-1-2.md`, `...-fixes.md`, `2026-11-17-ledger-phase7-steps-1-2-second-fixes.md`,
`...-third-fixes.md`) and the fourth were used as the list of things that must stay fixed; the suite, which
holds their tests, passes on both systems. I read the README contract and `ledger-phase7.md` at the branch head.

Standard applied: the verifier never prints VERIFIED or CONSISTENT, and never exits 0, for something it did
not verify; it never accepts a rollback or a fork; and nobody who holds no trusted key can bring the
auditor's state into a condition from which the protection is lost or from which no later run can go on, or
decide the exit status of a run by something no signature covers.

What was done (all of it was run; the programs and tests I wrote are not part of the repository):

- the whole suite on both systems (baseline below);
- the decision about revoked keys: a key thief, the old code (the parent of the round) and the new code side
  by side with one test file, and a check of every command that can take a new note;
- a stronger adversary beside a richer history of the trust file: 2,500 sequences of 8 steps on Windows and
  2,500 on Linux (20,000 steps each, three runs per step: 60,000 runs per system);
- real processes on the lock: 2, 8 and 32 concurrent runs advancing one state to different sizes, 2,150
  rounds on Linux and 108 on Windows, with a watcher that reads the state about once a millisecond and
  requires that it never goes down; a lock file that is a link, a FIFO, a directory, a dangling link, or has
  mode 0666; a state directory without write access; a state that is a link, a FIFO, a directory;
- a measurement of the window between the last read and the rename with a second writer that ignores the
  lock (3,000 attempts on Windows, 2,000 on Linux);
- the standard output in 12 forms on Linux and 4 on Windows;
- the limits on a proof element, derived from the format and checked against the fixtures;
- the escape rule against a decoder of my own (300,000 random inputs) and `..` after a link;
- 30 mutants of my own on the code the round touched, run against the author's suite only;
- ten minutes of `FuzzKeylessAdversary` with `-parallel=2`;
- Windows: the suite, the lock, the concurrency, the window, the adversary, a reader that holds the state.

## Summary

| Severity | Count | Findings |
|---|---|---|
| critical (as defined for this review) | 0 | |
| high | 0 | |
| medium | 2 | L14-01, L14-02 |
| low | 4 | L14-03, L14-04, L14-05, L14-06 |
| note | 3 | L14-07, L14-08, L14-09 |

No case was found in which VERIFIED or CONSISTENT was printed, or status 0 returned, for something that was
not verified; no rollback or fork was accepted; no path lets the signature of a revoked key count for a new
note (seven forms in four commands); in 40,000 steps of the stronger adversary the state never went down,
never held a text that was not published, never moved on a refusal, status 4 never appeared, and the honest
run after an adversary run always gave what the honest world gave. The lock held under every
real-process experiment.

The two medium findings are about the edges of the round, not about a wrong verdict. (L14-01) A status-only
caller that gives the null device read-write, which is what Python's `subprocess.DEVNULL` does, is refused
with status 2. (L14-02) When a state that rests on a revoked key meets the honest log (a stolen key signed a
fork before it was revoked), the run says "the log has two histories, both signed by a key of the trust
file", saves one evidence file for every run and does not say what to do; the rule before the round gave
status 2 with the rebase named.

Baseline: Linux 313 top-level tests passed, 4 skipped, 0 failed (16 packages ok); Windows 273 passed,
21 skipped, 0 failed (15 packages ok). These are the integrator's numbers and I measured the same.

## Fate of L12-01 to L12-10

| Finding | Fate | Evidence |
|---|---|---|
| L12-01 publisher chooses the key a state rests on | fixed; one dead end left, stated (L14-08) | a state made from one line of a key revoked later: status 0, state advances, `consistency --old STATE` passes; 40,000 adversary steps; removing every signing key from the trust file is status 2 with the rebase named, and the rebase works |
| L12-02 lock replaced, two holders | fixed for the lock; the write window is not what the README says (L14-04) | 2, 8, 32 processes: 2,150 rounds on Linux, 108 on Windows, final size always the maximum, watcher never saw the state go down; link, FIFO, directory, dangling link: status 2; mode 0666: kept, tightened to 0600; the author's tests kill four mutants of the lock |
| L12-03 subsets of lines are other notes, status 4 | fixed | every subset of 4 lines (author's test) and 5,000 sequences: one evidence file per signed text; status 4 never reached |
| L12-04 closed standard output is status 0 | fixed for `>&-`; the fix refuses a legitimate use (L14-01) | `>&-`: 2; `>&- 2>&-`: 2; `>/dev/null`: 0; `1<>/dev/null`: 2; Python `subprocess.DEVNULL`: 2 |
| L12-05 an element of 1 GiB | fixed | 4,096 bytes per element, 1 MiB outside the chain; an honest header is at most 455 bytes, an honest proof outside the chain about 12 KiB |
| L12-06 `..` after a link | fixed | `p/sl/../s3` with `sl` a link to `q/inner`: state is `q/s3`, as the system resolves it; relative `sl/../s4`: `q/s4` |
| L12-07 README says the cut is after the escape | fixed | README and code agree; my decoder read back 300,000 inputs one to one; output at most 4,992 bytes for a value of 2,600 |
| L12-08 guards no test catches | the two named are fixed; three new guards have no test (L14-06) | mutants of the nesting limit and of `readBack` are killed; M03, M06b, M24b, M29, M30 survive |
| L12-09 pipe closed early | documented | `| true`: 141; README says `head -c 5` leaves 0 |
| L12-10 state updated, verdict not written | documented, unchanged | `/dev/full`: 2 with the state updated, the standard error says so |

## The decision about revoked keys

Rule before: a held checkpoint all of whose signing keys were revoked (or beyond `max_tree_size`) was
refused: status 2, file kept, rebase named. Rule now: the identity of a checkpoint is its signed text; a held
checkpoint is usable while its text verifies under any key of the trust file; a state that rests only on
keys that no longer count stays the anchor (status 0, or 1 for a smaller size, a fork or a failed
extension) with a note, and the first checkpoint that extends it and is signed by a key that counts replaces
it. A plain note given as `--old FILE` keeps the strict rule.

### Scenarios (one test file on the new code and on the parent)

| Scenario | Old rule | New rule |
|---|---|---|
| S1 an honest checkpoint of size 300 signed only by `a`; `a` is revoked; the log grows to 900 signed by `b` | status 2 on every run until a rebase: the auditor is switched off from the day of the revocation | status 0, state moves to 900, a note says the held checkpoint rested on a revoked key and was replaced; `consistency --old STATE` passes. This is the case the round is for, and it is a real gain |
| S2 a thief holding `a` signs a forked log (size 700), the auditor stores it, `a` is revoked, the honest log (signed by `b`) is at 700, 900 or 300 | status 2 on all three, "keep the file, rebase with --min-size": the auditor is told what to do | status 1 "FAILED at step fork" (700 and 900) or "rollback" (300) on every run for ever, one evidence file per honest size, no rebase advice (L14-02). The rebase works under both: `--min-size 300` against the honest log gives status 0 and a state at 900 |
| S3 the revoked key's signature on a new note, in every command | not accepted | not accepted: `checkpoint`, `checkpoint --state`, `checkpoint --note FILE` with and without `--state`, `consistency --old STATE` (note from the log), `consistency --old STATE --new FILE`, `consistency --old FILE` (plain note: strict, status 2). Status 1 or 2 in all seven, the state unchanged byte for byte. Only `verifyNote` decides whether a note is new evidence, and it takes only the keys that count now |
| S4 a key limited to N signs N+1 | refused as new (`key-size`) | the same in every form; "one line among several" (limited key and a counting key sign 300): counted by the other, the limited line noted and kept; as a held checkpoint it is a weak anchor like a revoked one |
| S5 a forged later note after a forged held one | refused | refused: a forged later note needs a counting key; no consistency proof exists between a forged held checkpoint and a genuine later one, so a fork is reported |
| S6 a weak state meets a note of the same size and root | status 2 | (from the code, not run) status 0 "same as the state"; the lines of the new note are merged and the state is no longer weak; this passes only because the new note verified under a counting key |

### Judgement

"A held state only makes a run stricter" is correct for the decision of a run: given a new note that
verifies under a counting key, a held checkpoint can only add failures (rollback, fork, no proof of
extension). I found no input for which a weak state makes a run pass that the held text does not allow and
a counting key did not sign. The old rule had the opposite defect and it was real: a revocation switched the
auditor off for a state that was genuine, and a publisher without a key chose which state that was (L12-01).

Against a key thief the new rule is no worse in what matters (the auditor is bricked under both until it
rebases), and the rebase keeps what it can keep: a new state starts at the first checkpoint the auditor
trusts and catches a rollback from then on. The floor `--min-size N` is the auditor's own belief and nothing
authenticates it; what the rebase loses is the record between the poisoned state and the new one. That is
the same under the old rule, and now the poisoned state stays as evidence.

What is worse than before is how the brick is reported (L14-02). "Fork" is a statement about the operator;
it is printed against an honest log with the sentence "both are signed by a key of the trust file", which is
false for the held side; there is no pointer to the rebase; and every run saves a new evidence file (a log
that grows makes a new file at each cron run). The auditor cannot tell "my state was poisoned by a stolen
key" from "the log really forked" by the output, and the old output at least accused nobody.

What I would change, without reversing the decision: (1) when the held checkpoint is weak and the run ends in
a fork, a rollback or a failed extension, say so in the headline and in the failure line ("your state rests
only on revoked key X; if you did not expect a fork a stolen key may have signed it: rebase with ...") and
print the rebase command, as the status 2 message did; (2) do not write an evidence file for a conflict
against a weak state, or write at most one per held state; (3) put the weak flag and the key names in the
JSON; (4) correct the README sentence "keeping one that rests on a revoked key cannot make anything pass that
would fail without it": it is true against no state at all and false against the old rule (the new rule
gives status 0 where the old gave 2, correctly), so say "cannot make a run pass that the held text does not
allow". I accept the decision, on condition that (1) and (2) are done.

### The one dead end that is left

Removing from the trust file every key whose line is in the state gives status 2 with the rebase named in
`checkpoint --state` and in `consistency --old STATE` (the second says: "a note signed by a key you trust, or
SIZE:ROOT"). The message is right and the rebase works (the author's test, and by hand). What the auditor
loses: the floor is its own belief, and the record before the rebase. Who decides: the lines in the state are
those the publisher served on the first runs (later runs add lines), so a publisher without a key chooses
which removals brick the auditor. The README says to revoke and never to delete, so it needs an act against
the README; I rate it a note (L14-08), with the remark that the standard of this review is exactly "nobody
without a key". My adversary produced no such run in 40,000 steps (its trust files rarely remove the one key
that signed); the hand-made case is deterministic.

## The union of lines and the evidence

- **Accumulation as a lever.** A line that verifies under a key of the trust file but for which the key did
  not count at the time (not yet valid, retired, revoked, over its limit) is stored. A held checkpoint was
  already judged without the validity window, so a stored line of a "not yet valid" key makes the state
  "not weak" and changes a label and nothing else: the signature is genuine, and a held checkpoint decides
  nothing but failures. Not a finding. A line of a key removed from the trust file is not kept (L14-03).
- **Evidence extended in place** is `Replace` (temporary file, flush, rename, flush of the directory):
  atomic. Two runs in one directory cannot extend it at once (the lock is per directory); two states in two
  directories have different names. A file at the evidence name with other content takes the next of eight
  names; a party without write access to the directory cannot create one. Creating a new evidence file is
  not atomic (exclusive create, then write): a run that stops between the two leaves an empty file, which
  the next run skips; eight such stops, or write access to the auditor's directory, are needed for status 4
  (L14-09).
- **Can a party without a key create evidence files?** Yes, one for each genuine signed text it replays:
  each older checkpoint is a rollback and makes one file of a few hundred bytes; the same text with other
  lines is the same file. A text that was never signed cannot create one: m3 (another epoch), m4 (another
  log) and m5 (renamed keys) of my generator ran about 7,400 times on each system and no file appeared. The
  count is bounded by the number of checkpoints the log ever signed, which can be large; "no limit, since a
  limit would be a lever" is a reasoned decision. L14-02 adds the honest-log case.
- **A line is lost (L14-03).** The merge keeps only the lines that verify under the trust file now. After a
  key left the file, extending the evidence in place drops its line while the note says "the file now holds
  the lines of both"; the state rewrite does the same.

## The lock

Real processes on Linux (WSL ext4) and Windows; each experiment under two minutes.

| Experiment | Result |
|---|---|
| 2 processes, 1,700 rounds, random sizes, Linux | final size = maximum in every round; watcher saw no decrease; statuses 0 and 1 only |
| 8 processes, 550 rounds, Linux | the same |
| 32 processes, 162 rounds, Linux | the same |
| 2, 8, 32 processes, 108 rounds, Windows | the same |
| lock file is a link to a file (also to a 0644 file in another directory) | status 2 "is or passes through a symbolic link"; target untouched, mode unchanged |
| lock file is a FIFO | status 2 "not a regular file (it is a named pipe)", no hang |
| lock file is a directory / a dangling link | status 2 |
| existing lock file of mode 0666, owned by the auditor | kept, locked, tightened to 0600, status 0 |
| state directory 0500 | status 2 "cannot take the lock ... permission denied" |
| state is a FIFO / a directory / a link to a missing file | status 2, nothing created |
| Windows: lock is a directory; lock file held open by another handle | status 2 at once |
| a lock replaced between open and lock by a third party | only the author's hook test; not tried with real processes (see "Not reproduced") |
| crash between the re-read and the rename | only the author's step hook; not tried with a real kill |

**The window (L14-04).** The README says that what is left is "the few microseconds between that read and
the rename, for a writer that ignores the lock". The re-read is made before `Replace`, which then creates
the temporary file, writes it, flushes it and only then renames. A call takes 8 ms on Windows and 44 ms on
Linux (WSL). With a second writer that ignores the lock (temporary file and rename, no flush) starting at a
random time between 0 and twice the duration of the call: 1,312 of 3,000 attempts on Windows (44%) and 404
of 2,000 on Linux (20%) had the newer bytes replaced by the decision made on the older state. The number of
runs that refuse is near zero because the window starts at the re-read and lasts the whole write. The claim
is wrong by three orders of magnitude and it names the only protection against a writer without the lock.
The direction is simple: write and flush the temporary file first, then re-read and compare, then rename:
the window is then really the time between the re-read and the rename.

## The standard output

Linux, the built program, fixture of 300 entries:

| Case | Status |
|---|---|
| `>&-` | 2, one line on the standard error ("the standard output is closed ... for no output at all write >/dev/null") |
| `>&- 2>&-` | 2 |
| `>/dev/null`, `>>/dev/null` | 0 |
| `1<>/dev/null` | 2 (as the README says) |
| Python `subprocess.run(..., stdout=subprocess.DEVNULL)` | **2** (L14-01) |
| `>/dev/full` | 2, "could not be written" |
| closed pipe (`| true`) | 141 |
| stdout a socket | 0, the verdict arrives |
| `nohup prog </dev/null >/dev/null 2>&1` | 0 |
| Windows `> NUL`, a file | 0 |
| Windows: reader closed the pipe | 2 |

**L14-01 (medium).** Python's `subprocess.DEVNULL` opens the null device with O_RDWR, as do `daemon(3)`
and the usual double-fork recipes, so a supervisor that wants only the status gets "the standard output is
closed" and status 2 where it got 0 before. The README names only `1<>/dev/null`. Not verified here: what
`systemd` gives for `StandardOutput=null` (no systemd in the session; I believe write-only, which would be
accepted), cron, a tty, the Windows console that is gone. The fix is that the closed-descriptor guess cannot
tell the two cases; give the caller a way to say it (a flag or an environment variable that accepts a null
standard output), keep the refusal as default, and name Python and `daemon(3)` in the README.

## Limits

Format 1 has one hash form: 64 lower-case hex characters, `hash_version` fixed at 1.

- The largest honest element of `chain`: seven members, three hashes of 66 bytes with quotes, `height` at
  most 19 digits, `closed_at_us` at most 20 characters, `tx_count` at most 10. Compact 348 bytes; indented
  with four spaces per level and 12 spaces for the depth of a chain element: 455 bytes. The largest in the
  fixtures: 359. The limit 4,096 is nine times the largest honest element.
- Outside the chain: two paths of at most 64 hashes (4,610 bytes each, indented), one header, a few numbers,
  `relation` and `user_name` (at most 63 bytes each, 400 bytes at the worst with escapes). About 12 KiB; the
  largest in the fixtures 1,148 bytes. 1 MiB is eighty times that.
- An honest proof is never refused. A header with a member the format does not know is refused (unknown
  members are ignored in the proof, not in a header element): a later format needs a new limit.

## The adversary test

What the author's test (`TestKeylessAdversaryNeverChangesTheOutcome`, `FuzzKeylessAdversary`) asserts: with
the same tiles and the same trust file in both worlds, status, first line of the output, size of the state,
held text, names of evidence files and the verdict of `consistency --old STATE` are equal, a refusal moves
nothing. It does not compare the whole output, the time, or the content of evidence files; its adversary only
changes lines of the same text (drop, reorder, repeat, junk).

My generator (`TestR37Adversary`, my own file): 3 to 5 keys; every step the trust file changes (usable,
retired, not yet valid, revoked, limited to 400, limited to 299, removed); a log of non-decreasing sizes
(300 to 1,200); a random set of signers; world A runs the honest note; world B first runs an adversary note
and then the honest note. The adversary: m0 nothing, m1 lines dropped, reordered, repeated, junk lines for
keys, m2 an older genuine note, m3 a note of another epoch, m4 a note of another log, m5 genuine lines under
another key name, m6 a truncated tile, m7 a missing tile. Checked at every step: no status 4, no VERIFIED
with a non-zero status, a non-zero run leaves the state byte for byte unchanged, a status 0 for m3, m4, m5 is
a failure, no evidence file for m3 to m7, the state text is always a published one, neither state ever goes
down, B never behind A (except the removal dead end, which would be counted separately), and when A's honest
run gives 0 then B's honest run gives 0 and the sizes and held texts are equal.

Result: 2,500 sequences on Windows and 2,500 on Linux, 8 steps each: **0 failures**. Windows: A status 0
14,984, 1 4,566, 2 450; adversary run status 0 6,895, 1 12,411, 2 382, 3 312. Linux: A status 1 4,668,
2 406; adversary status 0 6,935, 1 12,414, 2 344, 3 307 (A status 0 14,926). The removal dead end was not
reached by chance. `FuzzKeylessAdversary` for 10 minutes with `-parallel=2`: 1,883 executions (each is a
multi-run world, so the rate is low), corpus 49, no failure, status 0.

## The tests of this round

Thirty mutants of my own, each in a clean copy, run against the author's suite with `-failfast` (my files
removed). Seven first versions did not build (an unused variable) and were redone. 25 killed, 5 survived.

Killed (the first failing test): union not written (`TestLinesOfOneCheckpointAccumulate`); held-strict
(`TestHeldNoteAndKeyStates`); merge without signature check (`TestVerify`); merge allowing duplicates
(`TestTextAndJSONCarryTheSameLines`); lock owner check removed
(`TestLockFileOfAnotherOwnerIsRefusedAndKept`); name check after the lock removed
(`TestLockIsOnTheFileTheNameLeadsToAfterTheLock`); `AcquireChecked` second look removed
(`TestAcquireCheckedLooksAgain`); lock tightening removed (`TestLockExcludesRealProcessesOnALooseLockFile`);
`..` fix undone (`TestDotDotAfterALinkMeansTheParentOfTheTarget`); `ReplaceIfUnchanged` replaced by `Replace`
and its re-read ignored (`TestReplaceIfUnchangedWritesOnlyOverWhatWasRead`); null-device check off in the
package and in `main` (`TestClosedStandardOutputIsStatus2`); element limit and outside limit removed
(`TestAnElementOfManyStringsIsRefusedBeforeItIsHeld`, `TestWhatIsOutsideTheStreamIsBounded`); exclusive
create of evidence and the eight names (`TestConflictEvidenceClaims`); `readBack` bytes
(`TestReadBackRefusesWhatALaterRunWouldNotRead`); rollback and fork off (`TestStateRollbackAndSameSizeFork`);
revoked and limit counted in a held note; lenient rule of `consistency --old`; weak note in "not proved" of
the state. So at least eight fixes of the round (the union, the lenient rule, the lock owner, the lock name,
the re-read, the standard output, the element limits, `..`) each have a test that fails without them.

Survived: M03 (`Merge` checks that the texts are equal: the callers check first, so unreachable and
untested); M29 (the guard `bytes.Equal(held.note.Text, served.Text)` before the merge: `Merge` fails on
other text and the failure is swallowed, so equivalent); M30 (origin compared in `readBack`: the byte
comparison covers it); M06b (the evidence file is rewritten even when nothing was added: no test sees an
unneeded rewrite); M24b (the "old checkpoint was genuine" line in `not proved` of `consistency`: no test).
The author's claim "each test fails without its fix" holds for the eight above.

## Windows

The suite passes (273 and 21 skipped). Experiments: 108 rounds of 2, 8 and 32 concurrent runs, final size
the maximum, no decrease; lock directory and held lock file: status 2; the window above; the adversary
(2,500 sequences); `> NUL`, a file and a closed pipe. One new behaviour (L14-05): a program that has the
state open without sharing delete (a backup or scanner; `FileStream` with `FileShare.Read`) makes the
rename fail: "the checkpoint verified but --state ... could not be updated ... Access is denied", status 2,
state unchanged; the next run after the reader closed gave 0. Safe, and no retry.

## New findings

### L14-01 (medium) A status-only caller that gives the null device read-write is refused

Steps (Linux): `python3 -c "import subprocess,sys; r=subprocess.run(['./kingres-verify','checkpoint','--trust','T','--log','L','--source','S'], stdout=subprocess.DEVNULL, stderr=subprocess.PIPE); print(r.returncode, r.stderr[:100])"`.
Observed: status 2, "the standard output is closed (it is the null device opened for reading and writing...)".
Expected: the caller asked for no output (Python opens `/dev/null` O_RDWR; so do `daemon(3)` and the
double-fork recipes), the run should go on, or at least the README should name these callers.
Place: `internal/stdio/stdio_unix.go` (`O_ACCMODE == O_RDWR`), `cmd/kingres-verify/main.go`, README "The
standard output". Direction: an explicit way to accept it (flag or environment variable), refusal by
default; name the callers in the README. A closed descriptor and an O_RDWR null device cannot be told apart
after the runtime has replaced it, so no check of the flags can do it.

### L14-02 (medium) A fork against a state that rests on a revoked key accuses the log, gives no way out and saves a file every run

Steps: Go test file `TestR37Thief` (helpers of the package): key `a` signs a forked log of 700 entries, state
made with a trust file holding `a`; trust file now `a` revoked, `b` usable; the honest log is signed by `b`
at 700, 900 and 300; then a run with the old code.
Observed (new code): status 1 on every run: "FAILED at step fork: two trusted checkpoints of this log have
the same size 700 and different roots"; at 900: "both are signed by a key of the trust file, so the log has
two histories"; at 300 "rollback". The weak note is among the notes, there is no rebase advice, and three
evidence files (700, 900, 300) were saved, one for each honest run. Old code: status 2 on all three with
"keep the file ... run checkpoint --state NEWFILE --min-size N".
Expected: a revoked key cannot "be trusted" as the held side; the output should name the held key as
revoked, say that the state may have been signed by a stolen key, and print the rebase command; no evidence
file per run.
Place: `cli_state.go` (`conflict`, the failure texts in `applyState`), `rebaseAdvice` unused for weak states.
Direction: as in "The decision about revoked keys".

### L14-03 (low) Extending an evidence file or the state in place drops the line of a key that left the trust file

Steps: `TestR37EvidenceLineOfRemovedKey`: a rollback served with the lines of `a` and `b`, the evidence holds
both; `a` is removed from the trust file; the same rollback is served with the line of `c`.
Observed: status 1, "the file now holds the lines of both"; the file holds the lines of `b` and `c`; `a`'s
line (what proves that `a` signed the old text) is gone. The state does the same on a `signatures_added`
write (`TestR37StateLineOfRemovedKey`).
Expected: lines of one text only accumulate; a line that does not verify now for lack of a key should be
kept, not dropped. Place: `signednote.Merge` (drops unknown keys) and `conflict`/`applyState`. Direction:
merge the old file's lines as they are (they are verified text-bound lines), verify only the new ones.

### L14-04 (low) The README says the write window is "a few microseconds"; it is the whole temporary write

Steps: `TestR37Window` (statefile package): a second writer (temporary file, rename, no flush) starts at a
random time in [0, 2 x the call); count the runs in which `ReplaceIfUnchanged` returned nil and the
intruder's bytes were gone. Observed: a call takes 8.1 ms on Windows and 43.7 ms on Linux/WSL; 1,312 of
3,000 (Windows) and 404 of 2,000 (Linux) were overwritten. Expected: a window of microseconds as stated.
Place: `statefile.go`, `ReplaceIfUnchanged` calls `Replace`, which creates, writes and flushes after the
re-read. Direction: split `Replace` into prepare (write and flush the temporary file) and commit (rename),
re-read between them; correct the README to what is measured.

### L14-05 (low, Windows) A reader of the state makes a verified run fail

Steps: seed a state; open it with `FileStream(path,'Open','Read','ReadWrite')`; run `checkpoint --state` to
a larger size. Observed: status 2, "could not be updated ... rename ...: Access is denied"; after closing
the reader the same command gives 0. Expected: a retry of the rename for a short time, or the README names
it. Safe, because the state is unchanged. Place: `statefile` Windows rename (`MoveFileEx`).

### L14-06 (low) Three guards of the round have no test

M03, M06b, M24b, M29, M30 (above) survive. Worth a test: the evidence file is not rewritten when there is
nothing to add (M06b), `consistency --old STATE` on a weak state lists the "not genuine" line (M24b), the
text check in `Merge` is reached by a test of its own (M03). M29 and M30 are redundant guards: delete or test.

### L14-07 (note) The weak anchor is not in the JSON

`checkpoint --state --json` on a weak state gives status 0, `extends_state: true`, and the weak fact only in
`notes` and `not_proved`. A machine reader cannot see it. Direction: `info.state.anchor_weak` with the key
names.

### L14-08 (note) The removal dead end is chosen by the publisher

As in "The one dead end that is left": a publisher without a key chooses, by serving one line on the first
run, which key removals make the state unreadable (status 2, rebase named and working). It needs an act
against the README ("revoke, never delete"). Direction: when a key is removed from a trust file, `trust` or
`checkpoint` could warn that a state holds only that key; the README sentence "never makes a stored state
unreadable" should be limited to "except by removing a key".

### L14-09 (note) A new evidence file is created, then written

A stop between create and write leaves an empty file that takes a name (of eight). A directory writer or
eight crashes are needed for status 4. Direction: write to a temporary file and link it in.

## What held under attack

- Every path that decides a new note uses the keys that count now; the seven forms tried gave status 1 or 2
  and left the state byte for byte unchanged.
- The lock: 2,150 + 108 rounds of real processes with a watcher; link, FIFO, directory, dangling link,
  mode 0666 file; the state directory 0500; no run moved the state down.
- The union: a state made from any subset of the lines of a checkpoint advances, and 40,000 steps with
  another log, another epoch, renamed keys, an older genuine note, damaged and missing tiles never produced
  a difference that was not a refusal that moves nothing.
- The escape rule is one to one and bounded; `..` after a link follows the system; the proof limits are far
  above honest proofs and below hostile ones.
- The rebase works from a poisoned state and from a state with no signing key left.

## Not reproduced

- A third party replacing the lock file between open and lock, and a crash between the re-read and the
  rename with a real kill: only the author's hook tests exist; I had no safe way to time them.
- Status 4 from a keyless party: not reachable by any run I made.
- `systemd` `StandardOutput=null`, cron, tty: not run.
- A key thief who also controls the publisher after the revocation: needs a counting key, so not possible.

## Not covered

A second local account (the lock file of another owner is tested only by the author's simulation); a
read-only file system (a directory of mode 0500 was used); macOS and the BSDs; the Windows console that is
gone and a closed Windows handle; mutation testing on Windows; the committed ledger extension; the whole
exit-status table of the README (only the cells touched by the round); memory figures of the proof reader
(only the limits); the fuzz target for more than ten minutes; the filling of a full disk.

Note on my environment: before I set a private temporary directory, one killed Linux run left two
directories in the system temporary area; I removed them.
