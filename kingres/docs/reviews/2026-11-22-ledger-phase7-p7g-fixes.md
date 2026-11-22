# Independent review of the fixes P7G-01 to P7G-06 (branch p7f-fix)

## 1. Scope, method, verdict

Reviewed: commits 3ff1480865, f03ec43aa6, 7cdfabfc54, a0c5901b29, ac5dde27f6, 7ab7ab5beb and 706ca342c0 on top of f13ae261cf, against the 2026-11-22 ledger-phase7 P7F-fixes review (findings P7G-01 to P7G-06) and section 17.1 of the ledger phase 7 design. I read the full diff (creds.go, credsync.go, agent.go, jobs.go, rest.go, service.go, the verifier restore rule, `release verify` exit status, release.sh, the packaging test) and the new tests.

Run on Windows: Go tests of control-plane ledgeranchor, ledgerstate, ledgerround, internalapi, cmd/kingresd, and the verifier `go test ./...` (cli and events with -count=1). All pass. 31 mutants and 2 probe tests (sections 5 and 6). No PostgreSQL-backed run (the change touches no SQL); this is a limit of the review.

Verdict: the six findings are fixed in the sense that the reported scenarios no longer happen, and the main guards are isolated by tests. No critical and no high finding. New findings: 3 medium, 3 low (section 4).

## 2. Verdict for each finding

| ID | Verdict | Evidence |
|---|---|---|
| P7G-01 verifier refuses the second restore event | Fixed | Rule is now "same generation and same replaced sum is a repetition". Mutants V1 (generation only), V2b (no rule), V3 (sum only) killed. A restore event is used only for notes and remarks (`cli_restore.go`, the only users of `Walk.Restores`); no path turns a failed comparison into a pass. See section 3. |
| P7G-02 old file with new mtime replaces the current one | Fixed for the stated cases (restore from backup, clock skew), with limits P7H-01, P7H-03, P7H-04 | No time is used any more. Install refuses lower and rival files (A1, A2b, A24 killed). |
| P7G-03 unbounded time reported by a holder | Fixed | There is no time in the call. A generation is bounded (2^40), but see P7H-01: the bound is itself an attack point. |
| P7G-04 classes of local failures | Fixed | `ErrPassphraseFile` is a server failure in `Install`; unopenable is final at once (A12, A20, A21b killed); other failures wait 30 minutes. Limits: P7H-02 (the wait clock is not reset), P7H-06 (bound not pinned by a test). |
| P7G-05 digest neither salted nor keyed | Fixed, and not vacuous | HMAC-SHA256 keyed with the stamp. Mutants A4 (constant key), A6 (no content), A18 (empty digests equal) killed. See section 3. |
| P7G-06 small defects | Fixed | Gate pruning (A13 killed), exit status 5 for a dry-run set (V4, V5, V6, V7 killed). Survivors: A14, A9, A10 (P7H-06). release.sh: P7H-05. |

## 3. Answers to the focus questions

**New file format, authentication.** A sealed file carries generation and stamp inside the AEAD plaintext; `openBody` opens it before `parseBody`, so the sealed form is authenticated, and an attacker without the passphrase cannot raise the number of a sealed file. Replaying an older sealed file gives the older number (refused by Install when a newer one is present, and a holder that serves it is told it is older). However, a sealed server accepts a plain file ("a plain one is accepted whether or not this server seals", `DecodeFile`/`openBody`), and in a plain file the generation is plain text. So the claim "both are authenticated" holds for sealed files only; anyone who can write one file into a holder's key directory (or send `credentials/install`, which needs the cluster certificate) can write a plain file with any generation up to 2^40 and credentials of their choosing, and it wins everywhere (probe H1: accepted by a sealed store, generation 1099511627776). The leader then takes it from the holder and distributes it. Rotation afterwards is impossible (Encode refuses generation above 2^40, `Rotate` returns the generic "cannot be read or opened"). Before this change the same write could be corrected by entering the credentials again; now it cannot. See P7H-01.

**Rotation, "one above the highest".** Two servers rotating at once, or across a leader change, give equal generations with different stamps. Both stay; nobody gives its file to the other (judgeCopies `conflict`, Install refuses a rival: A2b, A24 killed). Anchoring is not blocked: the gate only waits for a higher generation, and each server anchors with its own file. The cost is that servers anchor with different credentials, and the alert is non critical. The operator action is to enter the credentials again on the leader: `generationFloor` asks reachable holders and takes the highest, so the new file is above both rivals and every reachable holder takes it (A7, A8, A25 killed). Two gaps: (a) a holder that is not reachable at that moment is not in the floor, so a rotation can land on the same number as, or below, that holder's file (P7H-03); (b) two simultaneous rotations on the same server are not serialized (P7H-04).

**The HMAC key.** The stamp is in the file, which is copied unchanged by Install to every holder, so all copies of one rotation have the same key and the same digest. A holder compares its own copy with another's by comparing digests: both were made with the stamp the two files share, so equal files give equal digests; the digest never needs the other holder's key. The stamp does not leave the server in `credentials/list` (CopyInfo has id, generation and digest only; confirmed in code), it is not logged, and the digest cannot be used to test guesses of a secret without the stamp. It is not vacuous: killing the key (A4), the content (A6) or the empty-equals-empty rule (A18) is detected. Two rotations with the same credentials and the same generation have different stamps, so they count as rivals (conservative, harmless). Note that the keyed digest covers a tampered body only when the stamp is unchanged; changing the stamp makes it a rival. A file with the same generation, the same stamp and different members gives a different digest and is a conflict: correct. The `hmac.Equal` comparison is cosmetic (the digest is not an authenticator; equivalent mutant, no test can kill it).

**The 30 minute bound.** `CredStaleLimit` is 30 minutes counted from the first failure of a target (`credStaleSince`); `staleFailure` is `>=`. The records then fail with class permanent and code `credentials_stale`; the alert text switches from "asked again every minute" to "not tried again until Retry". The texts are right for the cases tested. Problems: the start of the wait is never reset unless a take succeeds (P7H-02); the test advances 50 minutes, so the limit is not pinned (mutants 60 minutes and `>` survive: P7H-06). "Retry" is the existing operator retry of failed records; the cause must be fixed first, otherwise the next record fails at once again because the note is still old.

**Upgrade.** Plain old files (`{"credentials":{...}}`) decode as generation 0 without a stamp (the new body parser takes the same prefix and an absent generation as 0; mutants A15, A16, A19 killed). Sealed old files (members only inside the seal) fall to the "earlier version" branch and are also generation 0. Two generation-0 files are current to each other, and the first rotation after the upgrade makes every old copy behind. Install of a gen-0 file over a gen-0 file writes it (no rival check by design). Mixed clusters during upgrade: old servers cannot read the new form (documented). One more observation: a server that is old reports no generation, which the leader reads as generation 0 and "stale"; it then redelivers the new form every pass until the old server is upgraded (a warning, not a failure). I did not run a mixed-cluster test.

**The verifier rule.** A repeated restore event cannot hide a fork. Restore events are used only in `restoreNotes` and `restoreRemark`, which add text to failures and notes; the verdict comes from the checkpoint, consistency and leaf checks. VERIFIED stays impossible for a tampered log with any number of restore events (no code path turns a failure into a pass). What the new rule changes: an authorised key can now sign any number of events with the same generation and different replaced sums, and the verifier will list each as verified. That is the operator's signed statement, not a proof; the notes already say that. The rule also accepts a generation that goes backwards or skips, as before. The duplicate check is quadratic in the number of restore events (negligible).

**release.sh.** The assignment `[ "${dry}" -eq 0 ] || accept=(--accept-dry-run)` is safe under `set -e` (the right side of `||`). `"${accept[@]}"` on an empty array under `set -u` fails with "unbound variable" on bash before 4.4 (4.3, and 3.2 on macOS). The script has `set -euo pipefail` at line 43 and the empty array is the real-release path, so a real release on such a bash would stop at that line with a misleading message ("the note that was just made does not verify" does not appear; the shell aborts first). WSL bash 5.2 does not show this. See P7H-05 for the portable form. `go run` itself turns a non-zero exit of the program into status 1, which is harmless here because the script only tests for success. The packaging test checks 5 without the flag and 0 with it, and the unit test also checks `--json` (status 5) and the flag.

## 4. New findings

### P7H-01 (medium): a plain file with a chosen generation is accepted by a sealed server and cannot be rotated away

Place: `creds.go` `openBody` (plain prefix branch), `parseBody` (`cb.Generation > MaxCredGeneration` is the only bound), `Install` (lines of the generation comparison), `Rotate`/`Encode` (`gen > MaxCredGeneration` gives ErrCredentials).

Scenario. A server seals its files. Someone with write access to one holder's key directory (a backup operator, a compromised side process) or with the cluster certificate (`credentials/install`) places `KINGRES-ANCHOR-1\n{"credentials":{...},"generation":1099511627776,"stamp":"<32 hex>"}\n`. It decodes, is the highest generation, and wins: the leader takes it from the holder (takeNewer) and gives it to all holders, which re-seal it. The credentials are now the attacker's (anchoring fails; the real credentials are gone from the cluster), and every attempt to enter the credentials again fails with a generic error, because the next generation 2^40+1 cannot be encoded. Probe H1 confirms: Install accepted, then Rotate returned "the credentials file cannot be read or opened". The only recovery is to delete the target and make a new one. Before the P7G change a re-entry fixed this. The authentication of the generation (design 17.1: "inside the sealed text, so both are authenticated") does not hold when the plain form is accepted.

Fix: (a) a server that seals refuses a plain file in `Install` and in `Digest` for the purpose of generation ordering (treat it as generation 0 with no stamp), or accept plain only as generation 0; (b) bound the jump: refuse an incoming generation above the own plus a small constant, or at least refuse reaching MaxCredGeneration; (c) make the operator re-entry able to go past a poisoned file (an explicit "reset" that writes generation = max seen + 1 and raises an alert if above a sane bound); (d) give the failure of Rotate at the bound its own message.

### P7H-02 (medium): the start of the 30 minute wait is not reset when the cause goes away

Place: `credsync.go` `staleFailure`, `staleCleared`, `ensureCurrent` (lines 274-281), `jobCredentials` (lines 349-371).

`credStaleSince[id]` is deleted only after a successful take, or when the target is gone. If the condition ends in another way (the holder with the newer file is deleted or reset, the operator enters the credentials again at a generation above it, the holder is rolled back), the note stays. Any later, unrelated stale episode (hours or days after) is judged against the old start and is final at its first failure: the records fail for good at once, with no 30 minute wait. Probe H2: an episode, then agreement of the holders, two hours later a new newer holder whose file cannot be fetched: the record is `failed`/`credentials_stale` at the first pass. Mutant A14 (the job never calls staleCleared) survives, which shows the clearing is not tested for the job path.

Fix: call `staleCleared(id)` whenever the judging finds no newer holder (in `ensureCurrent` after a successful holder report, and in the job), not only after a take. Add a test of two separate episodes.

### P7H-03 (medium): a rotation while a holder with a higher or equal generation is unreached is reverted or splits the cluster, with no operator-visible alert for the revert

Place: `credsync.go` `generationFloor` (a failed or partial report gives a lower floor), `rest.go` `putCreds`, `jobCredentials` take-newer branch.

Scenario. The old leader L1 rotated to generation 5 and went down before it handed the file out. The new leader L2 holds 3 and the reachable holders hold 3. The operator enters new credentials on L2 (for example after a key leak): floor 3, new generation 4. L1 comes back with generation 5 (the older, possibly leaked credentials). L2 sees a higher generation, takes L1's file (`takeNewer`) and gives it to everyone; the operator's rotation is undone. The only trace is a Warn log; there is no alert for a take-newer (the alert exists only when taking fails). If L1 held exactly 4, the result is a rival at generation 4 with an alert, which is correct. The design says a holder with a higher generation "refuses the file, which is an alert", true only when that holder is reachable at distribution time.

Fix: a rotation should raise a non-critical alert (or refuse, with a message that names the holders) when the report of holders is partial or failed (`Unreached` non-empty, or `Holders` returned an error), and a take-newer should raise an alert once ("a rotation was replaced by a file of a holder that was away"), so the operator can see it. A quorum for rotation is the stronger fix.

### P7H-04 (low): concurrent rotations on one server are not serialized

Place: `creds.go` `Rotate` (read of own generation, then write, no lock), `rest.go` `putCreds`.

Two REST requests that change the credentials of one target at the same time (a retry from a script, two operators) both read generation N and write generation N+1 with different stamps. The last local write wins; holders receive both and the second Install is refused as a rival (409 older_generation), and the leader's file may be either. The result is a conflict (rival) alert that the operator has to resolve by entering the credentials again, a third time. No time or corruption beyond that. Fix: a per-target mutex around `Rotate` plus `Distribute` in the service, or an atomic compare-and-swap on the generation in `write`.

### P7H-05 (low): `"${accept[@]}"` fails under `set -u` on bash before 4.4

Place: `kingres/verifier/scripts/release.sh`, the call of `release verify` (the new `accept=()` and its expansion).

On bash 4.3 and earlier (macOS /bin/bash 3.2, old CentOS) an empty array expands as an unbound variable under `set -u`, so a real (non dry-run) release stops with "accept[@]: unbound variable" at the verification step, after the files were made. The WSL bash used here is 5.2, so the tests do not show it. Portable one-line forms:

- keep the array and expand it as `${accept[@]+"${accept[@]}"}`, or
- use a plain variable: `accept=; [ "${dry}" -eq 0 ] || accept=--accept-dry-run` and `${accept:+"${accept}"}` in the call, which also needs no array.

### P7H-06 (low): the 30 minute bound and the job-path clearing are not pinned by tests; stale test gaps

Place: `p7g_test.go` `TestALeaderThatMissedARotationAndCannotTakeTheNewFileDoesNotAnchorAndGivesUpAfterTheLimit` (advances `CredStaleLimit + 20 minutes`), `credsync.go` `staleFailure`.

Mutants `CredStaleLimit = 60 minutes` (A10), `>` instead of `>=` (A9) and "job never calls staleCleared" (A14) survive. A bound of 30 minutes is claimed in the design and in the alert text, but any limit between 1 and 50 minutes passes. Fix: a test that advances to just under the limit (still waiting) and exactly the limit (final), and a job-path test for the clearing (see P7H-02).

Minor, not numbered: sealed files do not bind the target id into the authenticated data (a sealed file of target A put in the slot of target B opens there; the digest is keyed by id, so the copy is still judged correctly when compared, but a swap of files between two targets is not detected by the sealing); this needs write access to the key directory.

## 5. Mutants

Credentials and sync (ledgeranchor package tests, all other tests unchanged):

| Id | Change | Result |
|---|---|---|
| A1 | Install: lower generation allowed | killed (2) |
| A2b | Install: rival of the same generation allowed (A2 did not compile) | killed (1) |
| A3 | judgeCopies: same generation and other digest counted as current | killed (2) |
| A4 | digest key constant instead of the stamp | killed (1) |
| A6 | digest without the members | killed (2) |
| A7 | Rotate ignores the floor | killed (1) |
| A8 | Rotate ignores the own generation | killed (6) |
| A9 | staleFailure `>` instead of `>=` | SURVIVED |
| A10 | CredStaleLimit 60 minutes | SURVIVED |
| A11b | CredStaleLimit effectively 0 (A11 did not compile) | killed (2) |
| A12 | unopenable newer file not final | killed (1) |
| A13 | gate: no staleCleared after a take | killed (1) |
| A14 | job: no staleCleared after a take | SURVIVED |
| A15 | parser accepts generation 0 with a stamp | killed (1) |
| A16 | parser: no stamp validation | killed (1) |
| A17 | parser: no generation bound | killed (1) |
| A18 | sameDigest: empty digests equal | killed (1) |
| A19 | judge: empty digest counted as conflict | killed (1) |
| A20 | gate ignores final | killed (2) |
| A21b | work: final stale not turned into permanent (A21 did not compile) | killed (2) |
| A22 | agent: 409 older_generation dropped | killed (1) |
| A24 | Install: equal-generation guard off | killed (1) |
| A25 | generationFloor ignores holders | killed (2) |
| A26 | judge: picks the lowest newer holder | killed (1) |
| A5, A23 | digest without the generation (pattern did not apply); a no-op | not counted |

Verifier (cli tests for restore and release verify):

| Id | Change | Result |
|---|---|---|
| V1 | duplicate rule: generation only | killed |
| V2b | duplicate rule off (V2 did not compile) | killed |
| V3 | duplicate rule: replaced sum only | killed |
| V4 | exit status 5 not set | killed |
| V5 | `--accept-dry-run` ignored | killed |
| V6 | `--accept-dry-run` inverted | killed |
| V7 | JSON path returns 0 | killed |

## 6. Probe tests (not kept in the repository)

- H1: a plain file with generation 2^40 and chosen members: Install by a sealed store succeeded; Rotate afterwards failed (P7H-01).
- H2: two stale episodes two hours apart with agreement between them: the first failure of the second episode is final (P7H-02).

## 7. Hygiene

All mutants were restored (the worktree was clean when it was removed with `git worktree remove`, no force). Nothing was committed or pushed. No PostgreSQL install was made, so there is nothing to delete.
