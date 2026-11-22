<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026 RooyeKhat Media Co -->

# Independent review of the fixes of the anchoring wiring (P7A-01 to P7A-17), of verifier step 21 (epochs, completion of bundle verify, anchors in bundle verify, the four fixes of the eighth review) and of verifier step 23 (the release)

## 1. Scope, verdict, method

Commits reviewed, each against the review or design step it answers:

| Commit | What | Answers |
|---|---|---|
| `e0c1face24` | the fixes P7A-01 to P7A-17, corrections 230 to 245, ADR 0025 | `docs/reviews/2026-11-20-kingresd-anchoring-wiring-and-publication-fixes.md` |
| `aacdd0968b` | verifier step 21 | design `ledger-phase7.md` section 10 (step 21) and section 15, notes 1 to 9; the four medium findings V8-01 to V8-04 of `docs/reviews/2026-11-19-kingres-verifier-publication-and-client-signatures.md` |
| `e7d7a34814` | verifier step 23, the release | design section 10 (step 23) and section 15, "Step 23" |

A note on the branches. `e0c1face24` is contained in the branches `cp-int`, `fo-r2wip` and `web-15`. The branch `cp-push` holds the same patch under the id `d9aa31a283` (the two patch ids are equal), and the clean worktree of `cp-push` that was named for this review is four commits older than that and does not hold the fix at all. I therefore reviewed `git archive` copies of `e0c1face24`, `aacdd0968b` and `e7d7a34814`.

**Verdict.**

- No critical and no high finding. I found no input that makes the verifier accept a wrong history that it should refuse, and no path by which kingresd anchors or publishes unsigned or wrong state because of these commits.
- All 17 findings P7A-01 to P7A-17 and the four medium findings V8-01 to V8-04 are really fixed: each fix was exercised, and for each I found a one-line mutant of the fix that its test kills (the exceptions, below, are guards that are redundant with another guard or equivalent mutants, and a few guards whose tests do not isolate them).
- There are 2 medium findings, both about the strength of what the commits claim: the one that matters most is that no Go test fails if `release verify` stops comparing the hash of a binary with the signed sums (P7R-01). The second (P7R-02) is a verifier note that states something its own run has just shown to be false.
- 8 low findings and 1 note.

**Method.** Sources: `git archive` of the three commits into a private directory (nothing under a shared area; a private `cp -a` copy of a PostgreSQL 19 install for the tests that need a server). Everything was run under `timeout`, in at most 4 parallel Go processes, with the Go build cache in the private directory. Section 2 lists what was run and what was only read.

## 2. What was run, and what was only read

Run:

- `go build ./...`, `go vet` (the packages of the commits) and the unit tests of `internal/ledgeranchor`, `internal/anchor/...`, `internal/ledgerstate`, `internal/ledgerround`, `internal/ledgersign`, `internal/internalapi`, `cmd/kingresd`: all pass. The same three ledger packages also under the race detector (without a server): pass.
- The tests of `internal/ledgeranchor` that need PostgreSQL (`TestClaimedRecordsWithAFutureCheckDoNotStarvePendingOnesInPostgreSQL`, `TestTheWholeWayOfACheckpointOnTheDatabaseStore`, `TestTheDatabaseStoreKeepsTheRulesOfTheStore` and the rest of the package) with `KINGRES_PG_BIN` pointing at a private copy of a PostgreSQL 19 install. The SQL of `Due`, `Apply`, `Failures` and `Enqueue` is therefore run, not only read.
- The whole verifier module: `go build`, `go vet`, `go test ./...` on the head of step 23: pass (the `internal/cli` package takes about 160 s).
- Fuzzers, 90 s each: `FuzzParseSums` (release) and `FuzzEpochClosedEvent` (events): no finding.
- 92 mutants that built (one-line changes, each in a clean copy of the tree, each followed by a build and the unit packages): 62 for the control plane, 30 for the verifier. Section 5 lists them. Some did not compile and were redone; the table counts only those that built.
- Seven probes of my own (not left in any tree): the SQL guard of the revision of a target on PostgreSQL, the PostgreSQL `Failures` and `Retry` of an obsolete record, the stale error of a deposed worker, the forms of a secret in the redactor, the common height in a bundle, the `restore-acknowledged` event read by the verifier, the effect of a `go env` file on the reproducible build.
- `scripts/repro-check.sh` for `linux/amd64` and `windows/amd64` (two copies of the source at different depths, empty caches): REPRODUCIBLE. `kingres/packaging/test/test-verifier-package.sh`: 23 checks, all pass (it builds a real .deb from a small staged tree).

Only read:

- The cluster tests (`kingres/control-plane/scripts/run-integration.ps1` was not run, as instructed). The cluster tests named by the commits (a follower that forwards a hostile body, a database that is dropped, the failover tests) were not read in detail either; what they would show about failover and credentials (P7A-03, P7R-08) I could only reason about.
- `cmd/kingresd/ledger_anchor.go`: the closures `fetch`, `holderReport`, `Distribute`, `Undistribute` (tested only through fakes in `internal/ledgeranchor`).
- `kingres/packaging/test/test-deb.sh` (needs Docker, and the added checks of `systemctl show` and `/proc/.../limits`), the builds for `linux/arm64` and `darwin/arm64` (only the two targets above were built and compared), the signing with a real release key (none exists), `internal/anchor/*` libraries (not re-read; taken as reviewed five times), the design and README texts (read for what they promise, not proofread).
- No closed-epoch publication made by a real cluster exists (design 15, note 8): the epoch tests build their fixtures from documents. I could not close that gap.

## 3. The findings P7A-01 to P7A-17

Legend: "killed" means a one-line mutant of the fix makes a named test fail. "Survived" means no test notices. All mutants are in section 5.

| ID | Verdict | Evidence |
|---|---|---|
| P7A-01 high: due list cut at 100 before state and time | **Fixed.** `Due` now takes the set (pending, or submitted and claimed) and selects state and due time in the query, then applies the limit per target and set (row_number over the target). Same rule in the memory store. | PostgreSQL and memory tests run and pass. Mutants killed: due time removed from the SQL (M01a), `row_number` bound removed (M01c), per target bound removed in the memory store (M01b). |
| P7A-02 medium: no way back after a real restore | **Fixed, with the cost stated.** `accept-restore` file with a confirmation that depends on the record and the database, generation of the record, the old record kept as `published.json.before-restore-N`, the signed event `restore-acknowledged`, a higher generation wins the merge, a lower never takes the floor back, the design text of 9.4 corrected, ADR 0025. The tests cover the whole sequence (accept, event first, restart, another holder). | Killed: merge by generation (M20, M21), confirmation (M25, M30), event (M28, M29), pull from the other holders (M31), `settle` (M22). Survived: the guard "the copy of the old record must be written before anything changes" (M23, P7R-04). The verifier reads the event as an unknown type and says nothing of it (P7R-09). |
| P7A-03 medium: credentials handed over once | **Fixed.** Job `ledger.anchor.credentials` (60 s): fetches the files this server lacks, gives holders the files they lack, removes the files of deleted targets, keeps the rest as an alert (`target:<id>|credentials`); the worker asks the holders before it gives up; an unbuildable target fails its record at once with the cause. | Killed: fetch in the worker (M13), fetch in the job (M13b), distribution (M16), orphans (M15b), alert (M16b), permanent class (M12, M12b). Survived: the job is not in `Jobs()` (M14) and the guard that keeps the file of an existing target (M15), both P7R-04. Behaviour questions: P7R-08, P7R-10. |
| P7A-04 medium: a hanging target holds the pass | **Fixed.** One worker per target and set, `PassWait` of 20 s, a worker that is still running is left alone by the next passes. | Without the wait the pass takes 2.0 s and the test fails (M03a, redone correctly; my first form of the mutant was equivalent). Survived: the guard against a second worker for the same target (M03b); the bound of the test (at most one more request after 150 ms) is too loose to see it (P7R-04). A new defect of the same code: P7R-03. |
| P7A-05 medium: evidence not bound to the record | **Fixed.** `Binds` for the three kinds, checked in `succeed` before `ApplyReceipt`; a mismatch fails the record for good (`evidence_mismatch`). | Killed: the gate (M04a), the SHA-256 of the note (M04c), the token imprint (M04d), the OpenTimestamps digest (M04e). Survived: the key tail of the object (M04b) and the length (M04c2): both redundant behind the SHA-256 of the note (P7R-04). |
| P7A-06 medium: one wrong answer retires the real chain | **Fixed.** The same new chain id must answer for `MissingFor` without a single other answer, an alert from the first answer, the group reads as unread meanwhile; a returning real answer ends the wait. | Killed: M27 (confirmation removed), M34 (the end of the wait). |
| P7A-07 low: result applied to a changed target | **Fixed on both stores.** `Apply` carries the revision and drops the result when the target changed, was switched off or deleted. | Memory store: killed (M05a). PostgreSQL: the guard of the SQL has no test (M05c survived); I wrote one (right revision, wrong revision, disabled target, revision 0) and the SQL is correct. P7R-04. |
| P7A-08 low: old failure keeps the alert | **Fixed.** A failure with a newer anchored record of the same (target, log) is not listed; Retry of an obsolete record is refused (409). | Killed on the memory store (M06a, M06c). The PostgreSQL `Failures` clause (`NOT EXISTS` a newer anchored record) has no test (M06b2 survived); my probe on the server shows the SQL is right (P7R-04). |
| P7A-09 low: evidence file for any state | **Fixed.** `json` only for an anchored record, with the time of the anchoring. | Killed: the time (M07b). The state test alone is redundant with the `AnchoredAt` test (M07 is equivalent). |
| P7A-10 low: redactor | **Fixed.** URL-escaped, hexadecimal and four base64 forms, the Basic pair of a TSA as a secret of its own, members of at least 4 characters. | Killed: the pair (M37), the minimum (M38). Survived: the URL-escaped (M36b), lower case hexadecimal (M36c) and raw URL base64 (M36d) forms: the test compares against a text that never holds the full escaped form, so those three are not isolated (P7R-04). My own probe confirms the code removes them. |
| P7A-11 low: record write failure is silent | **Fixed.** `ErrNotWritten`, `dirty`, `settle` before every `Allow`, `AllowEvent`, `Check`; the round stops with a critical alert; an event whose record write fails is a task error; a file larger than the maximum is not written. | Killed: M22, M24, M26, M33, M35. Survived: the `return` of the error in `ensureCheckpoint` (M32): equivalent, because the next `Allow` refuses while the file is behind. |
| P7A-12 low: `%!w(<nil>)` | **Fixed.** Three texts, none names a path or content. | Killed (M12c). |
| P7A-13 low: concurrent enqueue | **Fixed.** The unique violation returns the record of the winner. | Killed (M18, M18b): the PostgreSQL test of a second enqueue of the same checkpoint fails without the handling of the unique violation. |
| P7A-14 low: limits of the record file | **Fixed.** 32 MiB, `save` refuses a larger record, `Open` and `save` use the same bound, directory sync after the rename (not testable here). | Killed (M24). |
| P7A-15 note | Decisions written down (correction 244). | Read. |
| P7A-16 note | Written down (correction 236: the clock is in memory, the safe direction). | Read. |
| P7A-17 note: tests for the surviving mutants | The tests exist (names listed in the design, correction 245). | Killed when mutated again: sealed iterations (B19), claim of a record (B28), evidence of another database (B15), behind warning (B08). B21 survived, equivalent behind the anchored test. |

## 4. The four findings of the eighth review (step 21)

The step 21 commit does not change the fixes of V8-01 to V8-04; they were already in the code and the project README was stale. What it adds is the re-run, a test for the ceiling of files at the write of `bundle fetch`, and the README correction. I mutated each fix again.

| ID | Verdict | Evidence |
|---|---|---|
| V8-01 key events change nothing outside `trust follow` | **Fixed** in all commands. | The revocation (`eff.Revoke`) removed: killed by `TestV801AKeyRevokedByAVerifiedEventVouchesForNothingInAnyCommand`, `TestV801TheOutputNamesTheKeysThatVouched`, `TestAnchorsKeyEventsStillApply`. The size bound of a retired key (`eff.LimitTo`) removed: killed by `TestV801ARetiredKeyVouchesOnlyUpToTheBoundOfTheEvent`. |
| V8-02 unauthorised events counted as verified | **Fixed.** | Count of the not verified events set to 0: killed by `TestV802AnEventNoAuthorisedKeySignedIsNeverCountedAsVerified` and the byte-mutation test of `bundle verify`. |
| V8-03 `bundle fetch` without a bound | **Fixed, and the new test is real.** | The check at the write (`f.files+1 > f.maxFiles`) removed: killed by `TestV803TheFileCeilingHoldsAtTheWriteAsWell` (the test that step 21 adds, and it fails exactly there). The check before the first request removed: killed by `TestV803AnUnsignedCheckpointOfAHugeSizeDoesNotDriveWrites`. |
| V8-04 member flagged 0x06 without an event | **Fixed.** | The `diverged` list not filled: killed by `TestV804TheFlagsOfAMemberAreJudgedTogether`, `TestAggregateRefusesADivergedMemberAndReportsAStaleOne`, `TestEpochClosedNeedsAVerifiedEvent`. |

## 5. Mutants

Control plane (each: build, then the unit packages of the file; PostgreSQL tests with a private server where the SQL is concerned).

| Id | Change | Result |
|---|---|---|
| M01a | PostgreSQL `Due`: due time removed | killed |
| M01b | memory `Due`: per target bound removed | killed |
| M01c | PostgreSQL `Due`: `row_number` bound removed | killed |
| M03a | `pass`: no wait bound (first form equivalent, redone with a channel that never fires) | killed (2.0 s) |
| M03b | `beginTarget`: no guard | **survived** |
| M04a | `succeed`: `Binds` not called | killed |
| M04b | `bindsWORM`: key not compared | **survived** |
| M04c | `bindsWORM`: SHA-256 and length not compared | killed |
| M04c2 | `bindsWORM`: length not compared | **survived** (redundant) |
| M04d | `bindsTSA`: imprint not compared | killed |
| M04e | `bindsOTS`: digest not compared | killed |
| M05a | memory `Apply`: revision not compared | killed |
| M05b | memory `Apply`: enabled not checked | survived (equivalent: a switch changes the revision) |
| M05c | PostgreSQL `Apply`: revision guard `true OR` | **survived** (SQL correct by my probe) |
| M05d | PostgreSQL `Apply`: enabled not checked | killed |
| M06a | memory `Failures`: newer record not looked at | killed |
| M06c | `Retry`: newer record not looked at | killed |
| M07 | `Evidence`: state not checked | survived (equivalent) |
| M07b | `Evidence`: `created_at` is `UpdatedAt` | killed |
| M12, M12b | the unbuildable target is retryable / another code | killed |
| M12c | `credFileProblem`: mode case removed | killed |
| M13 | worker does not ask the holders | killed |
| M13b | job does not fetch | killed |
| M14 | job not in `Jobs()` | **survived** |
| M15 | orphans: file of an existing target also removed | **survived** |
| M15b | no removal of orphans | killed |
| M16, M16b | no distribution, no alert | killed |
| M17 | credentials problem is never critical | killed |
| M18, M18b | PostgreSQL `Enqueue`: unique violation not handled | killed |
| M06b2 | PostgreSQL `Failures`: the `NOT EXISTS` of a newer anchored record removed | **survived** (SQL right by my probe) |
| M20, M21 | `Merge`: generation rules | killed |
| M22 | `settle` never writes | killed |
| M23 | the copy of the old record: write error ignored | **survived** |
| M24 | `save`: size not checked | killed |
| M25 | `Confirmation`: database not in the hash | killed |
| M26 | `persist`: `dirty` not set | killed |
| M27, M34 | one wrong answer retires / wait never ended | killed |
| M28, M29, M30, M31 | event name, announcement skipped, token not compared, pull of a higher generation | killed |
| M32 | `ensureCheckpoint`: error not returned | survived (equivalent) |
| M33 | `ErrNotWritten` not an alert | killed |
| M35 | `mutate`: write error of the record ignored | killed |
| M36 | std base64 form removed | survived (the raw form removes it up to the padding) |
| M36b, M36c, M36d | URL-escaped, lower case hexadecimal, raw URL base64 forms removed | **survived** |
| M37, M38 | Basic pair; minimum length of a member | killed |
| B08, B15, B19, B28 | mutants of the earlier review | killed |
| B21 | evidence of a record with none | survived (equivalent) |

Verifier:

| Id | Change | Result |
|---|---|---|
| V801a, V801b, V802, V803a, V803b, V804 | the four findings | killed (section 4) |
| E01 | `aggregate`: refused closed epochs not a failure | killed |
| E02 | event size and root not compared with the member | killed |
| E03 | a log of a later epoch needs no event | killed |
| E04 | `bundle verify`: an unaccepted closed epoch not refused | killed |
| E05 | `consistency` across epochs not a failure | killed |
| E06 | `epochContinuity`: event not compared with the frozen log | **survived** |
| E08 | `bundle verify`: event not compared with the closed log | killed |
| E09 | `closedMember`: a closed log that continues | **survived** |
| E10 | `--require-anchors` count not enforced | killed |
| E11, E12 | evidence digest, evidence origin and size not compared | killed |
| E13, E17 | a second event for one epoch, a final size of 0 | killed |
| E14 | the note for an acceptance that nothing used | **survived** |
| E15 | the aggregate leg not skipped across epochs | killed |
| E16 | anchors that could not be checked not counted | killed |
| R01, R02 | note text not compared with the sums; note signature not checked | killed |
| R03, R04, R08 | sums format: duplicate, name, upper case hex | killed |
| R05 | key string with another key name accepted | survived (a note under another name does not verify anyway) |
| R06 | `release verify`: the SHA-256 of the binary not compared | **survived** (P7R-01) |
| R07 | `release verify`: a name that is not in the sums not refused | survived (covered by R06's comparison, and the other way round) |

Both fuzzers ran 90 s: `FuzzParseSums` 5.2 million executions, `FuzzEpochClosedEvent` 3.3 million, no failure.

## 6. New findings

Severities: critical 0, high 0, medium 2, low 8, note 1.

### P7R-01 (medium): no Go test fails if `release verify` stops comparing a binary with the signed sums

Place: `kingres/verifier/internal/cli/cli_release_test.go` line 102 and 103 (`TestReleaseVerifyFailures`), `kingres/verifier/internal/cli/cli_release.go` line 145.

The case named "binary changed" gives the tampered content the file name `<name>-x`, so the run ends at "not in the signed sums" before any hash is computed; the case "binary not listed" is the same condition. No test gives a binary the right name and other content. Removing the comparison at line 145 (R06) leaves every test green, and so does removing the name lookup (R07) (each is covered only by the other). Reproduced: yes, with the mutant of line 145 (`if false && !bytes.Equal(...)`): all three `TestReleaseVerify*` pass.

The product is correct: `test-verifier-package.sh` (not a Go test, not run by `go test ./...`) makes a changed binary of the right name and gets status 1, and I ran it (23 checks pass). Why it matters: this comparison is the single reason to run `release verify`; a later edit that breaks it passes the module's tests and the release script (`release.sh` verifies the note with the first binary, which matches).

A fix must achieve: a case with the right name and one changed byte (status 1, step `binary`, the two hashes in the message), and one with the right content under a renamed file that is refused without `--name`.

### P7R-02 (medium): a bundle note says a block is "in the history that goes on" in the run that has just shown it is not

Place: `kingres/verifier/internal/cli/cli_bundle_epoch.go` line 151 (the loop over `found`), `cli_epoch.go` line 220 (`commonRemark`), `cli_proofleg.go` line 176.

Scenario: an `epoch-closed` event that states a common height (the publisher writes `unknown` today, so this needs a later writer, or a publisher that states a false value; the format allows it). A proof for a block at height 3, whose hash is in the closed epoch 1 and not in epoch 2, in a bundle that holds both logs. `proofEpochs` tries epoch 2 first and the inclusion proof fails there (it is a different history), then finds the block in epoch 1. `auditEpochs` then sees that the height 3 is at or below the stated common height 3 and prints "by that statement it is in the history that goes on as well", without the finding that is otherwise needed. With `--accept-epoch-closed 0` the run ends VERIFIED with that note. Without the flag it still fails, because the event itself is refused, so the false note only appears after an acceptance.

Reproduced: yes (a variant of the closed-epoch fixture of the tests with `common-height: 3` and a proof of block 3 of epoch 1; output "VERIFIED" and the note above, next to "the aggregate leg was not made").

Why it matters: the event is a statement of the publisher, signed, and the verifier has the data to test it (the block at that height of the log of epoch 2 is in the bundle). It believes the claim against its own evidence, and the note reduces the size of the cut that the auditor accepted.

A fix must achieve: when the event states a common height and the block is at or below it, compare the block (its hash) with the entry of that height in the log of the next epoch when that log is in the bundle; a difference is a failure at step `epoch-closed` that says the event and the logs disagree, whatever is accepted (the same rule as for the final size and root, which is tested).

### P7R-03 (low): an error of a late worker of one term ends the first pass of a later term

Place: `kingres/control-plane/internal/ledgeranchor/jobs.go` lines 96, 179 to 191 (`takeFatal`, `setFatal`), `kingres/control-plane/internal/scheduler/scheduler.go` `loop`.

`pass` now returns before its workers end, so a worker that meets `ErrDeposed` after its pass returned stores it in `s.fatal`. The error carries no term. `takeFatal` returns it at the start of the next pass, whatever term that pass belongs to. A server that was leader, lost the leadership while a worker was in a call, and later leads again: the first pass of the submit or upgrade job returns `ErrDeposed`, the scheduler reads that as "this term is over" and ends the new term at once (a wait of one margin and a new term).

Reproduced: yes at the mechanism: `setFatal(ErrDeposed)` then one pass returns `ErrDeposed` and submits nothing (record still pending). Not run: a real change of leader. Cost: a flap of the leadership of about one margin, once; no wrong state.

A fix must achieve: the error is kept with the term of the worker and dropped, not returned, when the pass belongs to another term.

### P7R-04 (low): tests of the fixes that do not isolate the guard they are named for

Place: `p7a_test.go` of `ledgeranchor`, `ledgerstate`, `ledgerround` (names below).

Mutants of the fixes that no test notices (section 5): the guard that a second worker is not started for a target that has one (M03b; `TestAHungTargetDoesNotHoldTheOtherTargetsOfAPass` allows one more request in 150 ms, which a second worker makes in any case); the SQL guard of the revision on PostgreSQL (M05c; every PostgreSQL call passes revision 0; my probe shows the SQL is right); the SQL clause of `Failures` that hides an old failure (M06b2; my probe shows it is right); the job `ledger.anchor.credentials` in `Jobs()` (M14: a pass is called directly, so a job that is never scheduled passes all tests); the file of an existing target that a holder has and the leader has not must not be removed (M15); the URL-escaped, hexadecimal and raw URL base64 forms of the redactor (M36b to M36d: the texts of the test do not hold the full forms they list as forbidden); the guard that the copy of the old record must be written before the record is replaced (M23); the object key of the evidence of a WORM record (M04b).

Reproduced: yes, each by its mutant. Nothing here is a defect of the product that I could show; each is a place where a later edit passes the tests.

### P7R-05 (low): two guards of the epoch findings and one note have no test

Place: `kingres/verifier/internal/cli/cli_epoch.go` (`epochContinuity`: the frozen log must have the size and root of the event, mutant E06; `closedMember`: a closed log that is longer than the event says, mutant E09; `unusedNotes`: an acceptance that nothing used is reported, mutant E14).

Without the first two, a run that was given `--accept-epoch-closed` accepts a frozen log whose size is not the one the event signed; without the third, a typo in the number of the event passes without a word (the design says it must not). Reproduced: yes, by the mutants; the code as it stands has all three checks. A test for each needs the fixture of the closed epoch with a longer frozen log and with a wrong number.

### P7R-06 (low): the reproducible build is not isolated from `go env -w` settings

Place: `kingres/verifier/scripts/build-one.sh` lines 27 and 28.

The script unsets `GOEXPERIMENT` and the micro-architecture variables and sets `GOFLAGS`, but it does not set `GOENV=off` and does not unset `GOFIPS140`. A machine whose Go environment file (`go env -w`) sets `GOEXPERIMENT` or `GOFIPS140` builds other bytes from the same source. Reproduced: yes. With the environment file giving `GOEXPERIMENT=nogreenteagc`, `GOFIPS140=v1.0.0` or `GOEXPERIMENT=nosizespecializedmalloc`, the SHA-256 of the `linux/amd64` build is `ee9bc80e...`, `68071b6b...`, `d0fabe41...`, against `05e2247e...` without. The README says that the same source and Go release give the same bytes on any machine.

Effect: the safe direction for an auditor (a rebuild that does not match makes him look), but a releaser with such a setting signs files that nobody reproduces, and `repro-check.sh` does not notice because both of its builds read the same file. A fix must achieve: `GOENV=off` and `unset GOFIPS140 GODEBUG` in `build-one.sh`, and a case for it in `repro-check.sh`.

### P7R-07 (low): a signed release carries no version, no date and no commit, and the script does not look at the tree

Place: `kingres/verifier/internal/cli/cli_release.go` lines 153 to 160 (the "not proved" list), `kingres/verifier/scripts/release.sh`.

The signed text is the sums file; the file names carry no version (`kingres-verify-linux-amd64`). An older release, with a known fault, verifies as VERIFIED for ever, and the "not proved" list does not say so (the list of `bundle verify` has the equivalent line: "an older genuine bundle verifies too"). `release.sh` builds the tree it runs in and does not check that it is clean, that it is the tag, or record the commit; the README only advises a clean checkout. Not run beyond reading. A fix must achieve: the line "that this is the newest release" in the output, a version line or version in the names inside the signed text, and a check of a clean tree at the commit named by the version in `release.sh`.

### P7R-08 (low): a leader that cannot reach the other holders fails the records of a target for good

Place: `kingres/control-plane/cmd/kingresd/ledger_anchor.go` (`fetch`, `continue` on any error), `kingres/control-plane/internal/ledgeranchor/jobs.go` lines 202 and 264.

`fetch` answers `ErrNoCredentials` both when every holder answered "I have none" and when none could be reached. After a failover in which the old leader (the only holder that has a file) is down, the new leader fails the records of that target with the code `target_unbuildable`, class permanent, and a critical alert; before the fix they were retryable. When the old leader returns, the job fetches the file, but the failed records stay failed until a Retry or until a newer checkpoint is anchored (P7A-08 then hides the alert). Read only (needs a cluster). A fix must achieve: the permanent class only when every holder was reached and none had the file; otherwise the retryable class, as before.

### P7R-09 (low): the verifier does not read `restore-acknowledged`; the design says it shows the fork

Place: `kingres/verifier/internal/events/events.go` (unknown types are counted, not interpreted), `kingres/docs/design/ledger-phase7.md` 9.4 ("What a verifier concludes").

The event that ADR 0025 and 9.4 call "the public statement that the history was cut" is parsed (I made one in the format of the publisher and `aggregate` reads the source without complaint) but no command says anything about it: not `aggregate`, not `bundle verify`, not `monitor`. A bundle of a publication after an accepted restore verifies with no trace of the cut, where a closed epoch of a chain gets a note in the same case. An auditor who holds an older aggregate checkpoint gets a fork or a smaller size from `--previous` or `monitor`, with no mention of the event. No wrong history is accepted (the fork is still detected by anyone who holds an old checkpoint); the explanation promised by the design is missing. A fix must achieve: a type known to the verifier, a note in `aggregate`, `bundle verify` and `monitor`, and the event named in the failure of a fork against a held checkpoint.

### P7R-10 (low): the job of the credentials can remove the file of a target that is being created

Place: `kingres/control-plane/internal/ledgeranchor/credsync.go` lines 121 to 135, `rest.go` `CreateTarget` (row first, file after).

`jobCredentials` reads the targets, then asks the holders which files they have. A target created between the two reads (the row, then the file on the leader and on the holders) has a file on every holder that the job takes for an orphan and removes from the holders (the leader keeps its file). The next pass of the job gives it back (60 s). In the window a failover would find a target without a file. Not reproduced (needs two interleaved calls); read only.

### P7R-11 (note): the notes about the ordering and the record of a restore

- `announceRestore` runs at the start of a round, but `checkPublished` also runs inside the key tasks, and a task can append an event after an acceptance and before the round that announces it. The ADR says "before anything else is signed"; the order of the events can differ. The record is consistent either way.
- `Merge` at the same generation takes the note `Restore` from a peer when this copy has none, so a cleared note comes back from a stale peer. This is harmless: the event is keyed by `generation:<n>` and `AppendEvent` finds the existing one.
- `Accept` writes `published.json.before-restore-<generation>` with a plain `WriteFile` (not synced, not atomic); a lost record that is regenerated from the database starts again at generation 0 and would overwrite an old copy.

## 7. Findings that are not defects

- The `e0c1face24` tests pass with the PostgreSQL store (the review of the wiring could not run it). `Due` per target with `row_number` gives the right sets on the server.
- No secret reached a log, alert or REST answer in the new paths I read: the fetch call answers the file in base64 (sealed when the server seals), the cleared buffers, `errText` on every error that is stored.
- The sums parser and the epoch event parser held under 90 s of fuzzing each (5.2 million and 3.3 million executions).

## 8. What the verifier accepts now that it did not (step 21, step 23)

- `bundle verify` of a database with a closed epoch: status 1 at `epoch-closed` unless the event is accepted by number; a log of epoch e + 1 needs a verified event for epoch e; an event that disagrees with the log it closes is a failure whatever is accepted (tested, mutants E02, E08 killed).
- `--require-anchors N` now means "at least N anchors verified" (an anchor that cannot be checked is then a note); without it everything the bundle lists must verify or the run ends with status 3. Tested (E10, E16 killed).
- `release verify`: the signature, the text and the format are tested (R01 to R04, R08 killed); the comparison of a binary is not (P7R-01).
- The reproducible build gives the same bytes at two paths with empty caches for the two targets I built, and the same as the packaged binary (the package test compares them byte for byte).
