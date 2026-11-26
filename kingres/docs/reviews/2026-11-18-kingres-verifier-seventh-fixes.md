<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->

# Independent review of the seventh fix round of the verifier (the fixes for L16)

Reviewer: an independent reviewer. Date: 2026-11-18.

## Scope and method

Reviewed: the Go module `verifier/` of kingres as exported read-only (`internal/statefile`, the state and
consistency code of `internal/cli`, `internal/testdir`, the Windows rename, the README, design section 7 with
corrections 55 to 59, 69 to 73 and 85 to 87). Read first: the review of the sixth round (L16-01 to L16-10 and its
section on the Windows cleanup failure). I worked only in copies (Linux and Windows) and did not change the
export. The probes, scripts and mutants I wrote are not part of the repository and were removed at the end.

Standard applied: the verifier never prints VERIFIED or CONSISTENT, and never exits 0, for something it did not
verify; nobody without a trusted key can make a run pass that should fail, move or replace the held state, hide a
conflict from the auditor, or flood the auditor; every claim of the README and of a comment is kept by the code.

## What I ran

- Linux (Go 1.27.3): the whole suite, 17 packages ok (`internal/cli` 60.7 s); `gofmt -l` empty and `go vet ./...`
  clean.
- Windows 11 (Go 1.27.3, with the Git `usr/bin` first on the PATH): the whole suite, 16 packages ok;
  `internal/stdio` has no test files on Windows (`internal/cli` 184 s). One run; no test failed and no cleanup
  failure appeared.
- 16 one-line mutants of my own on Linux, one test binary at a time (M01 to M15 and two variants; two first
  attempts did not build or did not apply and were redone): see the table below.
- 5 probe tests in the cli package: a randomized sequence of 80 served notes against a weak state, each run in text
  and in JSON (the invariants "the served note is in a file or printed in full", "printed in both formats or in
  neither", at most two evidence files); five kinds of planted object at the evidence name (directory, link, other
  text, same text with a line of a key that does not verify, empty file); a state named like a temporary file and
  look-alike files in the directory; the repair of a weak state (same checkpoint with a counting line, a rollback
  before and after, a state that becomes weak again); `old_anchor_weak` in JSON on eight exits of
  `consistency --old STATE`.
- Windows with real files and a real second process (a child that opens the state without share-delete and with
  share-delete, for 2 s and for 6 s), the read-only attribute and a directory at the name; a probe of the helper
  `internal/testdir` that leaks an open file.
- Real processes on Linux with the built program: 150 runs killed with SIGKILL at 1 to 9 ms in the middle of a state
  replace and 150 in the middle of a weak evidence write, each followed by an ordinary run; a sample of 19 forged or
  damaged inputs on `checkpoint`, `proof`, `row-hash` and `digest-chain`.
- No fuzzing was run (0 of the 3 minutes allowed). No probe could use more than 2 GB (a guard in the sequence
  probe; the others are small).

## Fate of L16-01 to L16-10

| Finding | State | Evidence (file:line, mutant) |
|---|---|---|
| L16-01 replay takes the one weak evidence file | fixed | `statefile.go:559-608` (two names by kind, `SaveWeakConflict`), `cli_state.go:687-732` (`weakConflict`, `printNote` on every case that is not saved). M01 (one kind for all): killed by 4 tests; M02 (no print when the slot is taken): killed by 3. Sequence probe (80 random served sizes, text and JSON): every conflicting note was in a file or printed in full, printed in both formats or in neither, at most two files |
| L16-02 `anchor_weak` missing on some exits | fixed | `cli_state.go:262-269` (`judged`, set at 326), `cli_tlog.go:538-545` and `cli_out.go:44-49` (`keepInfo`, `inputInfo`). M11b (no info on failure after judged): killed by `TestAnchorWeakIsOnEveryExitAfterTheStateWasJudged`. Probe: `old_anchor_weak` is present on all 8 exits of `consistency --old STATE` I tried (statuses 0, 1, 2, 3), JSON valid on each |
| L16-03 repaired state still weak | fixed | `cli_state.go:414-424`, `:454-456`. M07 (report the held state): killed by 6 tests; M10 (no repaired flag): killed by 2. Probe: same checkpoint plus a counting line gives `signatures_added`, weak false, repaired true; the next run says not weak; a later rollback takes the strong path (`state.conflict-250-...`); a state revoked again is weak again |
| L16-04 leftover not removed | fixed, one edge (L18-01) | `statefile.go:148-203`, called under the lock at `cli_state.go:290`, `statefile.go:242`, `:420`, `:594`. M04 (no regular-file check), M05 (no owner check), M08 (no digit check), M12 (any `.tmp`), M13 (not removed in `Prepare`): all killed. Probe: a link (also dangling), a directory, upper case, a suffix, an empty counter are left alone. Every call site is under the directory lock and in the directory of the state |
| L16-05 failed flush reported wrongly | fixed | `statefile.go:205-214`, `:313-316`, `:509-511`; `cli_state.go:438-445` (state, status 2, `directory_flushed`), `:525-537` (strong, status 4, `evidence_flushed`), `:700-705` (weak, status 1). M06 (plain error after the rename) and M14 (plain error after the link): killed in `statefile`, not in `cli` (its tests use hooks) |
| L16-06 read-only blamed on another program | fixed on Windows | `sync_windows.go:45-103`. Real files: a read-only target is refused after 20 ms with the right words, a directory at the name after 20 ms, a holder that does not share delete for 2 s: the run waited and succeeded; for 6 s: status-2 message after 3 s, state unchanged, no temporary file left. See L18-03 for the wording |
| L16-07 flush untested, load tests, adversary test | fixed | M03 (no `Sync` before the link): killed by `TestEvidenceTemporaryFileIsFlushedBeforeItIsLinked` and `TestEvidenceIsNotLinkedWhenItCannotBeFlushed`. `TestStateTwoRunsNeverMoveBackwards` keeps its assertion (M15, no lock: killed by it and by 4 others). The deterministic adversary test kills M01 and M07 |
| L16-08 design document behind the code | fixed | corrections 69 to 73 and 85 to 87 are in `ledger-phase7.md`; 56, 58 and 69 point to 85 and 70 |
| L16-09 percentages | fixed | README states counts, one machine, one rival, and the interval for one event; no rate is claimed |
| L16-10 rotation makes a state weak | fixed (stated) | README section "A key rotation makes a state weak without anyone's act" and correction 86, with the step to take before revoking |
| Windows cleanup failure | mitigated, not settled | `internal/testdir`; see L18-05 |

## New findings

### L18-01 (low) `RemoveLeftovers` removes the held state itself when the state is named like a temporary file

Place: `statefile.go:183-203`, called before the state is read at `cli_state.go:290`.
What is wrong: the pattern `.kingres-state-<12 hex>-<digits>.tmp` is matched against every file in the directory
and the state is not excluded. A state whose own name has that form (the auditor's choice, so no party without a key
can cause it) is a regular file owned by the user, so it is deleted under the lock before it is read.
How shown: probe: the thief state of size 700 moved to the name `.kingres-state-0123456789ab-7.tmp`, the honest log
served at size 300 (a rollback): status 0, `VERIFIED ... size 300`, the held state gone and recreated at 300. With
any other name the same run gives status 1.
What a fix must achieve: never remove the file the run was told to use (compare with the resolved state path, and
with the lock and evidence names), with a test.

### L18-02 (low, documentation) README line 808 has a literal backslash-n

`README.md:808` reads `are not a held checkpoint,\nhave none)`: a typing error in the sentence about the members of
`consistency --old`. Fix the text.

### L18-03 (low, Windows wording) "without sharing delete" is more than the code can know

Place: `sync_windows.go:99` and the README sentences around 703 to 710 and 1668.
How shown: a real child process held the state open with `FileShare.ReadWrite | Delete` (it does share delete): the
rename was refused with code 5 for as long as the handle was open, and the message said "another program may have the
file open without sharing delete". So any open handle gives code 5, and the words are wrong for a holder that shares
delete. The README names the access control list as the only limit. For a directory at the name the advice "clear the
attribute or name another file" is also off (it names an attribute).
What a fix must achieve: say "another program may have the file open", give the directory its own words, and add the
sharing case to the stated limit.

### L18-04 (low, tests) The handle tests do not walk the weak path, the evidence merge or the leftover removal

Place: `handles_linux_test.go`, `handles_windows_test.go`: the runs are a create, an advance, `consistency --old`
and a rollback against a state that counts.
How shown: mutant M09 (one `os.Open` that is never closed, at the top of `SaveWeakConflict`): the whole `cli` suite
passes. The same leak in `SaveConflictMerging` (M09b) is killed by `TestStateRunsLeaveNoDescriptorOpen` with the
collector off, so the test works where it looks.
What a fix must achieve: add the weak conflict (new, the same text again, a planted slot), the extension of an
evidence file (`mergeInto`) and a run that removes a leftover to the loop of both handle tests.

### L18-05 (note) `testdir.New` can hide a leaked file handle on Windows when other tests allocate

Place: `internal/testdir/testdir.go:34-60`.
How shown: on Windows a test that opens a file in its directory and drops the reference fails its cleanup after 10 s
when the process is quiet (the removal keeps failing). In the same process with a goroutine that allocates, the
collector's finalizer closed the handle and the test passed in 0.05 s. This is a property of the runtime that the
old two-second retry of `t.TempDir` also had; the 10 s retry makes the window longer. It does not weaken the handle
tests (they count handles with the collector off and caught M09b), but the cleanup of a test is not a leak detector.
On Linux `New` makes no check at all (a removal does not fail for an open file).
What a fix must achieve: nothing urgent; say in the package comment that a leak is for the handle tests to find, and
widen them (L18-04).

### L18-06 (note) The bound on strong evidence is the log's history, chosen by a party without a key

Place: README near 476 to 481, `statefile.go:418-451`.
A keyless publisher can serve, one per run, every old genuine checkpoint of the log as a rollback against a state
that counts; each makes a new file, and a log that signs often has many. The weak path now has a fixed bound of two;
the strong one has none, and the README says so and gives the reason (a limit would force status 4). The sentence
"not by anything a party without a key chooses" is true of the bound and false of the count: the party chooses which of
the log's checkpoints to replay. The runs are the auditor's own and each file is a few hundred bytes. Not a
regression, and I do not ask to change the decision, only to word it as it is.

### L18-07 (note) The fallback for a file system that cannot link does not flush the directory

Place: `statefile.go:515-527` (`createEvidenceInPlace`). On a file system that cannot make a hard link, an evidence
file is created and flushed, and the run says saved with no flush of the directory and no `evidence_flushed` member.
Only that file system reaches it; the README describes the path and its empty-file risk.

## What held under attack

- Two evidence files for a weak state: in an 80-step random sequence over sizes 100 to 900 (rollbacks, forks at the
  same size, larger forks), each served note was in a file or printed whole, in text and JSON alike, and the two
  names were the only files.
- Names taken by planted objects: a directory, a link to a file (its target untouched), a file with other text and an
  empty file at the fork name: status 1, the note printed, nothing overwritten, no new name. A planted file with the
  same signed text and a line of a key that does not verify is read as the same checkpoint and extended with the
  genuine line (the line that does not verify is dropped); it needs write access to the private directory.
- Old single-name weak files from the earlier version are neither read nor removed; the weak names, the strong names
  and their `-2` suffixes cannot collide by construction.
- A state that becomes weak, counting and weak again: the weak names depend on the held checkpoint, which only a
  trusted note changes, so two files per held checkpoint.
- Repair: in every case I made (same checkpoint plus a counting line, an extension, a rollback in between) the next
  run reports the state as not weak and conflicts then take the strong path.
- JSON: one valid document on every path I ran, with `anchor_weak` or `old_anchor_weak` present on each.
- Leftover removal: always under the directory lock, always in the directory of the state, never a link or a
  directory, strict pattern. A swap to a link between the stat and the remove removes the name only.
- A failed flush: the three statuses as documented (2, 4, 1); mutants killed in `statefile`.
- Windows with a real second process: waits and succeeds when the holder lets go within 3 s, fails with the state
  unchanged and no temporary file when it does not; read-only and directory named at once.
- Core: 150 kills in a state replace (139 old, 11 new, 0 torn; 128 left a temporary file; every following run
  succeeded and removed it) and 150 kills in a weak evidence write (0 empty files; 130 left a temporary file; every
  following run gave status 1, one weak file, no temporary file left). Forged signature, root, size, truncated note,
  altered proof and digest, damaged and missing tile, garbage state, bad and no trust file: statuses 1, 1, 1, 1, 1,
  1, 1, 3, 2, 2, 2, with no VERIFIED on a non-zero status. With the lock removed (M15) five tests fail, among them
  the real-process test.
- Tests that became weaker: none found. `TestStateTwoRunsNeverMoveBackwards` counts the designed status 2 only after
  a full wait and keeps `got >= told` and `got == 320` when nobody gave up; `childRunWithin` takes the smallest of
  three peaks, which a command that grows with its input still fails. I did not mutate the memory and pipe tests.

## Verdict

All ten findings of the sixth review are fixed, and the mutants of my own that touch them are killed by tests that
are about the change; the one that survives (a leak in the weak path) is a gap in the handle tests, not in the code.
I found no case of VERIFIED or status 0 for something not verified except the self-inflicted one of L18-01 (a state
named like a temporary file), no way for a party without a key to move or replace the held state, to hide a
conflicting note from the run that saw it, or to make more than two weak evidence files, no torn state in 300 kills,
and no invalid JSON. What remains is one low code defect (L18-01), wording (L18-02, L18-03, L18-06) and test
coverage (L18-04). I recommend one more small round for L18-01 and L18-04, and then to stop.

## What I did not check

- A file of another user on Linux (only the author's test with a stand-in owner, which M05 kills); no root.
- An access control list that denies the rename on Windows (only the stated limit); a scanner holding the file.
- Power loss (the order of flushes was read and tested through the hooks, not observed); a failing flush of the
  directory in a real process.
- A real second writer that ignores the lock (the README counts were not repeated); systemd and `daemon(3)` for the
  null device; macOS and the BSDs.
- The memory and pipe tests by mutation; `proof`, `blocks`, `digest-chain` and `row-hash` beyond the sample above.
- Fuzzing (none run), repeated Windows full runs (one only), and which process held an entry in the old Windows
  cleanup failure (it did not appear in my run).
