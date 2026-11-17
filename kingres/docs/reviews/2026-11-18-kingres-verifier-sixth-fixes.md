<!-- SPDX-License-Identifier: AGPL-3.0-only -->

# Independent review of the sixth fix round of the verifier (the fixes for L14)

Reviewer: an independent reviewer. Date: 2026-11-18.

## Scope and method

Reviewed: the Go module `verifier/` of kingres as exported read-only (the command, `internal/cli`,
`internal/statefile`, `internal/stdio`, `internal/signednote`, the README, and the design document section 7 with
its corrections). Read first: the review of the fifth round (L14-01 to L14-09). I worked only in copies of the
export (Linux and Windows); the export was not changed, and the two copies were checked to be identical to it
before and after the mutants (`diff -r`). The probe tests and scripts I wrote are not part of the repository and
were removed.

Standard applied (as in the earlier reviews): the verifier never prints VERIFIED or CONSISTENT, and never exits 0,
for something it did not verify; nobody without a trusted key can make a run pass that should fail, discard or
replace the held state, or flood the auditor; and every claim of the README and of a comment is kept by the code.

## What I ran

- The whole suite on Linux (WSL, Go 1.27.3): 16 packages ok. `gofmt -l` and `go vet ./...` clean.
- Windows 11 (Go 1.27.3): six runs of `internal/cli`, `internal/statefile`, `internal/safefile`, `internal/trust`
  together with the other jobs below (so the machine was loaded). Three runs passed fully (1, 3 and 4), run 2 and
  run 6 failed in `TestStateTwoRunsNeverMoveBackwards` (a run waited 10 s for the lock, the test counts status 2 as
  a failure), run 5 hit the 10 minute limit of `go test`, and run 6 also had the cleanup failure of section "The
  open point on Windows". All other tests passed in all runs.
- 22 one-line mutants of my own on the code of the round (21 on Linux, 1 on Windows), run against the author's suite.
- 11 probe tests of my own: weak anchor against a replay then a real fork; JSON on 8 failure paths; evidence names
  taken by a link and by a directory; a read-only directory; a limit on file size (a full disk); a stop in the
  extension of an evidence file; a failing flush of the directory; `consistency --old STATE` JSON; the read-only
  attribute on Windows; handle counts with the collector off (118 exit rows times 4 runs, plus 5 weak and conflict
  paths times 6 runs).
- Real processes on Linux with the built program: 400 runs killed with SIGKILL at 1 to 9 ms in the middle of a
  state replace, and 300 in the middle of an evidence write (each followed by an ordinary run); 7 runs of the
  built program under `KINGRES_VERIFY_NO_OUTPUT=1` with a shell and with Python.
- The adversary test of the author (`TestKeylessAdversaryNeverChangesTheOutcome`, 1,400 steps) with a counter of how
  many steps meet a weak state (45).
- Windows control experiments for the open point (plain file calls only, no code of the verifier): 1,000 + 400 +
  400 + 4 x 300 directories, and 800 child processes.
- No fuzzing was run (0 minutes of the 3 allowed).

## Fate of L14-01 to L14-09

| Finding | State | Evidence |
|---|---|---|
| L14-01 null device read-write refused | fixed | `stdio.go:48-50` (`Refused`), `main.go:28`. Mutants: env ignored, any value accepted: both killed by `TestNullStandardOutputGivenOnPurposeRunsWhenTheCallerSaysSo`. My runs: with `1<>/dev/null`, closed fds and Python `DEVNULL` on both streams the status is the real one (0, 1, 1, 1, 2); without the variable 2. The variable cannot turn a failure into 0 (see below) |
| L14-02 fork against a state on a revoked key | partly | wording, status 1 and one file per held state work (`cli_state.go:615-648`; mutants M06, M09, M11 killed). But the one file can be taken by a replayed genuine old checkpoint and a later real conflict is neither saved nor printed (L16-01) |
| L14-03 line of a removed key dropped | fixed | `signednote.go:451-515`. Mutants: loose lines dropped, count of loose lines, lines added not stored: all killed (`TestMergeStored...`, `TestStateKeepsTheLineOfAKeyThatLeftTheTrustFile`). Bounded: at most 32 lines, trust file at most 16 keys, loose lines dropped first. No way found to make two runs disagree |
| L14-04 window is the whole write | fixed | `statefile.go:284-310` (prepare, read, commit). Mutant "read before prepare" killed by `TestReplaceIfUnchangedLooksAfterTheTemporaryFileIsWritten`. The README figures are arithmetic-correct (see L16-09) |
| L14-05 reader of the state fails a run on Windows | fixed | `sync_windows.go:46-72`. Mutant `busy := false` killed by both Windows tests. Side effect: L16-06 |
| L14-06 guards without a test | fixed | the guard `Added == 0` is killed (mutant M17 by `TestEvidenceIsNotRewrittenWhenNothingIsAdded`); M29 and M30 are deleted in the code; the test for `consistency --old STATE` on a weak state exists |
| L14-07 weak anchor not in the JSON | partly | present on success and on a conflict; absent on every failure after the state was read that is not a conflict (L16-02) |
| L14-08 removal dead end | fixed | `removedKeysText` names the key ids; mutant killed by `TestLineOfAKeyThatLeftTheFileMakesNothingUsable`; README sentence limited |
| L14-09 evidence created, then written | fixed for the name, not guarded for the flush | `statefile.go:423-463`. The empty-file case is closed (mutant "in place" killed). The flush of the temporary file before the link is not guarded by any test (L16-07) |

## New findings

### L16-01 (medium) One evidence file for a weak state is taken by the first conflict, even a replay, and the next conflict is lost

Place: `cli_state.go:615-648` (`weakConflict`), `statefile.go:499-520` (`SaveWeakConflict`).
What is wrong: for a held state that rests only on keys that no longer count, the evidence is one file named from
the held state, and it holds the first conflicting note served, whatever it is. A party without any key can serve
an old genuine checkpoint once (a rollback, which is always possible) and take the slot. A later conflict, for
instance a real fork signed by the key that counts, is then not saved ("this run's was not saved") and, unlike the
strong path, its note is not printed either (no `note_base64`, in text or JSON). The run says status 1 and
"keep both files", so the auditor does not learn that the fork note exists only in the run's source. The same is
true when the save fails (a planted file): `weakConflict` never prints the note.
How shown: probe `TestProbeWeakSlotBurn` on the thief scenario of the author: serve size 300 (rollback, saved as the
one file), then serve a different log of size 900 signed by `b`: status 1, evidence files after: one, the note of
size 300; the output of the second run holds no note. The author's own test
`TestThiefStateKeepsOneEvidenceFileWhateverTheNumberOfRuns` asserts that the file is the size-300 note.
What a fix must achieve: a keyless replay must not be able to exclude a later conflicting note: for instance one
file per signed text of the served note with a small fixed bound per held state (a rollback and a fork are
different texts), or at least print the unsaved note (base64, and `info.state.note_base64`) whenever it was not
saved by this run. The bound must stay independent of anything a party without a key chooses.

### L16-02 (low) `anchor_weak` and `old_anchor_weak` are not always present, against two statements of the README

Place: `cli_state.go:367-393` (the missing tile, bad tile and input error exits of `applyState` set no
`info.state`), `cli_state.go:407-417` (read-back and changed-state failures), `cli_tlog.go:576-582` (the size
failure of `consistency` sets the member only for a weak state). README lines 755 and 1528 say that
`info.state.anchor_weak` is true or false for every run that read a state, and that `consistency --old STATE` has
`old_anchor_weak`.
How shown: probe `TestProbeWeakJSONPaths` (a state that rests on a revoked key, then damaged tiles: status 1; then
no tiles: status 3): `info` has `origin, root, signers, size` and no `state` member at all, weak or not.
`TestProbeOldWeakPresence`: `consistency --old STATE --json` with a smaller new checkpoint and a state that counts:
status 1, `info` is null. The JSON is one valid document on every failure path I tried (lock timeout, directory as
state, garbage state, read-only directory, full disk, status 4, evidence names taken): no case of invalid JSON.
What a fix must achieve: set the member on every exit after the held state was judged (or change the sentence and
the test to say which exits have it). Add a test that walks the failure exits.

### L16-03 (low) A weak state repaired by the same run is still called weak, and "not proved" says the held checkpoint was not shown genuine

Place: `cli_state.go:342-361` and `:402-404`.
What is wrong: a weak state meets the same size and root signed by a key that counts: the run adds that line and
writes the state (`action: signatures_added`), yet the JSON says `anchor_weak: true` with the revoked key, and
`not_proved` says "that the checkpoint the state file held was genuine: it rested only on keys the trust file no
longer counts". The same text was just verified under a counting key. The next run says not weak.
How shown: probe `TestProbeWeakSame` (output read in full).
What a fix must achieve: report the state as it is after the write (weak before, not weak after), and do not list a
non-proof that the run discharged.

### L16-04 (low) A crash in the extension of an evidence file leaves a temporary file that no later run removes

Place: `statefile.go:389` (`Replace(p, merged)` calls `Prepare`, whose `removeLeftovers` removes only the temporary
files of the name it is given), README "a temporary file that the next run removes".
How shown: probe `TestProbeEvidenceExtensionLeftover` (stop after the write in the extension): the temporary file
`.kingres-state-<tag of the evidence name>-*.tmp` stays through a replace of the state, a new evidence file, and a
second run on the same evidence with nothing to add. It is removed only when the same evidence file is extended
again. Bounded (one per evidence file and crash) and harmless to every decision, so low.
What a fix must achieve: remove every `.kingres-state-*.tmp` and `.kingres-evidence-*.tmp` of the directory that
belongs to a name of this program under the lock, or change the sentence.

### L16-05 (low) A failed flush of the directory after a successful rename or link reports the wrong thing

Place: `statefile.go:263` (`Commit` returns the error of `SyncDir` after the rename), `statefile.go:463`
(`createEvidence` returns it after the link).
How shown: probe `TestProbeSyncDirAfterRename` with a failing `SyncDir`: `ReplaceIfUnchanged` returns an error while
the state is already `v2`, so the run says "could not be updated, so later runs do not know it", which is false;
`SaveConflict` returns an error while the evidence file exists, so the run says "was NOT saved" and gives status 4
for evidence that is on disk. Both are the safe direction (a failure is shown, nothing is lost) but the text states
something untrue and the next run will find the state moved.
What a fix must achieve: say what is true ("renamed, but the directory could not be flushed: a power loss may bring
the old file back") and keep the status.

### L16-06 (low, Windows) Any access-denied on the rename is called "another program may have the file open" after 3 seconds

Place: `sync_windows.go:57-71`.
How shown: probe `TestProbeWinReadOnlyState`: the state has the read-only attribute; the run waits 3.1 s and ends
with status 2 and a message that blames another program; the state is unchanged and no temporary file is left.
The wait is also in the same range as what the machine does by itself (section below: the longest delay of an outside
holder I measured was 2.2 s), so a loaded machine can end a verified run with status 2.
What a fix must achieve: test the attribute first and say "the state is read-only"; keep the retry for sharing and
lock violations only.

### L16-07 (low, tests) The flush of the evidence temporary file is guarded by no test; two tests fail under load

- Mutants M12, M12b, M12c (no `Sync` of the evidence temporary file before the link) survive in the statefile
  package and in the evidence tests of `internal/cli` (the first run of M12 was "killed" only by
  `TestStateTwoRunsNeverMoveBackwards`, a load failure). L14-09 claims "written and flushed to a temporary file";
  the order test `TestReplaceIfUnchangedReadsBetweenTheFlushAndTheRename` records the calls of the state only.
  Fix: record the calls of `createEvidence` in the same way and require sync before link.
- `TestStateTwoRunsNeverMoveBackwards` (in process, 8 goroutines) fails on Windows when the machine is loaded
  because a waiter reaches the 10 s limit (the Windows lock polls every 20 ms and is not fair) and the test counts
  status 2 as a failure; the README says status 2 after 10 s is the designed answer. Fix: count status 2 only as
  failure when the wait was short, or give the test a long `LockWait`.
- `TestPeakResidentSetOfACommandOnAHostileFile` and `TestNamedPipesNeverHang` failed in mutant runs while the
  machine was busy and passed 8 of 8 alone: load-sensitive, not wrong.
- The adversary test compares the first output line in both worlds while the README says that a publisher without a
  key decides whether a state is weak, which changes the wording. It passes because the generator meets a weak
  state in only 45 of 1,400 steps and the two worlds mostly agree; it does not exercise the case where they differ.

### L16-08 (note) The design document was not brought to the code of this round

`docs/design/ledger-phase7.md` ends at correction 68; the corrections 69 to 73 named in the task are not in the
export. Correction 58 still says that a null device opened for reading and writing is refused on every command (no
variable). Correction 56 says the state holds "every line that verifies" and the evidence is "one file per signed
text": both are now wrong (stored lines of removed keys are kept; the weak case is one file per held state).
Correction 55 does not carry the weak wording or the one-file rule.

### L16-09 (note) The percentages of the README are what they are, with limits that are not stated

The figures are arithmetic-correct (265/1,200 = 22.1, 1/1,200 = 0.08, 1,346/3,000 = 44.9, 189/3,000 = 6.3) and the
README names the rival model (a writer without the lock that starts uniformly in 0 to twice the call and does not
flush). Two limits: 0.08 percent is one event, so it is "1 in 1,200" (the 95 percent interval is roughly 0.002 to
0.5 percent), and "What is left: 0.08 percent on Linux" reads as a rate; and both figures are for one machine and
one file system (WSL2 ext4, NTFS) with that rival. I did not repeat the measurement.

### L16-10 (note) A weak state is an ordinary result of a key rotation, so the soft wording is the normal wording after one

Place: `cli_state.go:579-587`.
The README says that a publisher without a key decides whether a state is weak (by which lines it serves). That is
true, but a rotation does it without the publisher: when the state was made while only the old key signed, and the
auditor then adds the new key and revokes the old one, every later conflict against that state is worded "may come
from your state and not from the log" and the advice is to rebase, which adopts the log as served. This is the
cost of accepting the decision of the fourth round, not a defect of the round; with L16-01 it means that the first
conflict after a rotation is the only one kept. I did not find a way for a keyless party to make a run pass or to
make the verifier replace the held state through this (replacement needs a note that verifies under a counting key
and a proof of extension).

## Answers to the questions of the task

Weak anchor. No way found for a party without a key to make a status 0 that should fail, to make the verifier discard
or replace a held state, or to flood the auditor: a weak state is replaced only by a note signed by a key that
counts and proved to extend it; weak conflicts make at most one file per held state (the strong path is still one
per signed text, as before). The wording can be had against an honest state in the way of L16-10. The one defect is
L16-01.

`KINGRES_VERIFY_NO_OUTPUT`. It is read in one place (`stdio.Refused`), only decides whether to run, and never touches
a status. Seven runs of the built program with the variable and a null or closed standard output gave 0 for the
advance, 1 for a rollback (also with both streams closed, with `--json`, and from Python with `DEVNULL` on both
streams) and 2 for a missing source; the evidence file of the rollback was written. It cannot turn a failure into
status 0. What is lost under it is the text, as documented (including the note of a status 4). The tests of the
author prove only status 2 survives under the variable; they would not catch a mutant that maps 1 or 3 to 0, but no
such code exists.

State replacement and evidence. Flushing and reading again before the rename: correct and tested. Crash at each
step: 400 kills of a state replace left the state whole (399 old, 1 new, 0 torn), 335 of them left a temporary file,
and the next ordinary run succeeded and removed it in all 400; 300 kills in an evidence write left no empty
evidence file and no leftover after the next run. Exceptions: L16-04, L16-05. Two runs at once: covered by the
lock, the author's real-process tests passed. A symbolic link and a directory at an evidence name: the next name
was used, the target of the link was untouched. A directory or garbage in place of the state: status 2. A
read-only directory with an existing lock file: status 2, nothing left. A limit on file size (a full disk) on the
state: status 2, state unchanged, no temporary file; on the evidence: status 4, no temporary file. Windows retry:
3.1 s, see L16-06.

Stored lines of removed keys. Cannot grow without bound (see L14-03 row) and the lines of a state cannot make two runs
disagree: they are ignored by every decision, kept in canonical order, and a line whose key returns is verified again.

## The open point on Windows: "TempDir RemoveAll cleanup: ... The directory is not empty"

Result: the cause is outside the process under test. I reproduced the same error with a program that uses no code of
the verifier and holds no handle, and I found no open handle in the verifier. What I have:

1. A natural failure in my own runs: run 6 of `internal/cli` failed in `TestSpecialFilesInASource` (0.03 s) with
   `unlinkat ...\e1: The directory is not empty`. `testing` retries a removal for 2 s on this error, so something
   kept the entry for more than 2 s.
2. Reproduction without the verifier (a test that creates a directory with `e1/tile/0/000`, writes and rewrites a
   `checkpoint` file, makes and removes a directory with that name, closes everything, then calls `os.RemoveAll`):
   10 of 400 directories took more than 20 ms to remove and one 2.18 s, with the same message, with 100 byte files
   and 5 of 400 with 1 MiB files. With a pause of 300 ms between the last write and the removal, 53 of 300
   directories were slow (longest 0.69 s); with no pause, 1 of 300. A directory with no file at all: 1 of 300
   (0.30 s). In these programs nothing is open when `RemoveAll` runs, so the holder is another process or a driver.
3. The machine runs a third-party endpoint security product and a search indexer; the built-in antivirus is off.
   Such components open new files for a scan, and when they open with delete sharing a file that the test deletes stays "delete
   pending" and the parent cannot be removed, which gives exactly "The directory is not empty" (an inference: I
   could not see the holder). The Restart
   Manager reported no holder for the entry (a delete-pending file has no open path to report), so I cannot name
   the process; this is the part I did not establish.
4. In the process: no handle left open by any exit path. With the collector off (so that no finalizer closes a
   leaked file), the handle count did not grow over 4 runs of each of 118 exit-site scenarios, nor over 6 runs of
   weak conflict, strong conflict, `consistency --old` on a weak state, a missing tile, and `--json` (131 handles
   before and after). The author's handle test calls `runtime.GC()` and sleeps, which would hide a handle that only
   a finalizer closes; that is why I repeated it without the collector. Reading the code: every `Open` has a
   `Close` on every path in `safefile`, `statefile` and `cli`; the one place that holds a directory for the whole
   command (`os.Root` of `--source`) is closed by `defer in.close()`.
5. Child processes are not the cause: 200 directories after 4 child processes each (800 processes) needed no retry.
6. Note: `os.Root` on Windows opens with delete sharing, so a handle of this program that was left open would
   produce the same message; item 4 is the evidence that none is.

The failure is rare per directory and shows up in large packages because there are thousands of directories. It
does not change a verdict of the program. What would settle the holder: a cleanup hook that lists the entries and
asks the Restart Manager (or the system's handle listing) at the failure, and a run with the temp directory
excluded from the scanner. What the tests should do meanwhile: remove test directories with a long retry (10 s)
on Windows through one helper (only `scratchDir` in the Windows rename test does today) or put the temp directory
where the scanner is told to leave it.

## Test quality, claims

- Mutants: 22. Killed by a test that is about the change: 18 (M01, M02, M03, M04, M05, M06, M07, M08, M09, M10, M11,
  M13, M15, M16, M17, M20, M22, and the Windows rename mutant). Not killed: the three evidence-flush mutants
  (L16-07). One mutant of mine (the name of the weak file with a suffix) is not meaningful in the package it was
  run in. Five of the first runs were "killed" only by a load failure or did not build and were redone (M06, M07,
  M09, M11, M12).
- Tests that pass for the wrong reason: none found in the round beyond L16-07. The test of the closing of standard
  output with the variable only proves status 2 survives, which is enough for the code as written.
- Claims not kept: the two README sentences of L16-02, the sentence "a temporary file that the next run removes"
  (L16-04), "could not be updated, so later runs do not know it" after a failed directory flush (L16-05), the design
  corrections of L16-08.

## Verdict

The sixth round fixes L14-01, L14-03, L14-04, L14-05, L14-06, L14-08 and L14-09 with tests that fail without the fix,
except the flush of the evidence file. L14-02 and L14-07 are fixed for the cases the author tested and not on their
edges. I found no case of VERIFIED or status 0 for something not verified, no way for a party without a key to move
or replace a held state, no torn state in 700 kills, and no invalid JSON. The one finding that matters is L16-01:
the new rule of one file lets a replay of an old genuine checkpoint decide which conflict is kept, and drops the note
of the next one without printing it. Fix that and the missing JSON members; the rest is wording, one test, and the
design document.

## What I did not check

- The Windows behaviour of the lock and the rename with a real second program holding the state (only the author's
  test and my read-only attribute probe).
- systemd `StandardOutput=null` and `daemon(3)` (the README states a belief for systemd; I did not run either).
- macOS and the BSDs (the code is built for them by tag only).
- Power loss (no crash of the machine; the flush order was read and tested through the hooks, not observed).
- A real second writer that ignores the lock (the README figures were not repeated).
- Fuzzing (none run), the other commands (`proof`, `blocks`, `digest-chain`, `row-hash`) beyond the exit rows, and
  the console and control-plane parts of the repository.
- Which process holds the entry in the Windows cleanup failure (the Restart Manager gave no name).
- A state file or an evidence file made by a version older than the fifth round.
