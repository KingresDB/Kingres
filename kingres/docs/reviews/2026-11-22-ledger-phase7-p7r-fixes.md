<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026 RooyeKhat Media Co -->

# Independent review of the fixes P7R-01 to P7R-11 (commit d9f79e78d2)

## 1. Scope, verdict, method

Reviewed: commit `d9f79e78d2` (branch `p7r-fix`), parent `94b8c3d562`, against the report `2026-11-21-ledger-phase7-anchoring-and-verifier-fixes.md` (findings P7R-01 to P7R-11) and the design `ledger-phase7.md` (section 16 and the sections it cites).

Verdict:

- All eleven findings are fixed. Every fix that matters was mutated, and each mutant that should be killed is killed (section 4). Four guards are not isolated by a test (section 4, "survived"); none of them is a defect of the product.
- No critical and no high finding. I found no input that makes the verifier accept a tampered history or reject a genuine one because of this commit, and no path that puts a secret in a log, an error, an alert or a file with a loose mode.
- 1 medium finding (P7F-01: a credentials rotation that a holder missed is never given again, and the alert that says it will be is wiped within a minute). It is older than this commit (it comes from the P7A-03 job) but it sits in the code that P7R-08 and P7R-10 changed, and the text of the problem makes a promise that the code does not keep.
- 5 low findings and 1 note.

Method. A detached worktree of the commit. Go tests on Windows for `internal/ledgeranchor`, `internal/ledgerstate`, `cmd/kingresd` (control plane) and `./...` of the verifier. The PostgreSQL-backed tests and the tests that skip on Windows (the unwritable-directory test of the restore copy) through the integration script on a private PostgreSQL 18 install (the extensions of the parent commit, lane l5). The packaging test of the verifier (37 checks) and `repro-check.sh` for `linux/amd64` under WSL with the Go release that `go.mod` names. Mutants: one-line changes, each made in the worktree, run, and restored. Probes of my own (two, not left in any tree). Everything under `timeout`; the private install and the scratch work were deleted at the end, the worktree was removed without `--force`.

Baselines, all green: control plane three packages; verifier `go test ./...`; PostgreSQL tests `TestTheDatabaseStore*` and the three `ledgerstate` restore-copy tests; packaging test 37 passed, 0 failed; `repro-check.sh`: REPRODUCIBLE, and the binary is the same under the hostile Go environment.

## 2. Verdict for each finding

| ID | Verdict | Evidence |
|---|---|---|
| P7R-01 binary compared with the signed sums | **Fixed** | `TestReleaseVerifyRefusesARightNamedBinaryWithOtherBytes` reaches the comparison (right name, one byte changed, appended, empty; renamed with `--name`; mixed genuine and changed). Mutant R06 (comparison off): 2 tests fail. |
| P7R-02 common height believed against the run's own evidence | **Fixed** | `blockNotInLog` compares the leaf with the signed log of epoch e+1 (the leaf is the same expression as in `cli_proofleg.go`). Mutants V2 (path check off) and V5 (the aggregate leg without `--accept-epoch-closed`): killed. Probe: with an epoch 2 that really shares heights 0 to 3, a proof of block 3 verifies (the block is found in epoch 2 first, so the genuine case is not rejected). See P7F-03 for a branch that cannot be reached. |
| P7R-03 late worker of one term ends a later term | **Fixed** | `takeFatal(term)` drops the error of another term. Mutant T1 (drop removed): 2 tests fail. Residual: P7F-05. |
| P7R-04 tests that did not isolate their guard | **Fixed** | Killed now: M03b (second worker), M05c and M06b2 (PostgreSQL SQL), M14 (job not in `Jobs()`), M15 (file of an existing target), M36b, M36bp, M36c, M36cu, M36d, M36du, M36s, M36r (each form of the redactor), M23 (copy before restore, only when run on Linux as a non-root user), M04b (object key of the WORM evidence). |
| P7R-05 epoch guards E06, E09, E14 | **Fixed** | Mutants E06, E09, E14: each killed by one new test. |
| P7R-06 build not isolated from `go env -w` | **Fixed** | `build-one.sh` exports `GOENV=off` and unsets the variables. Mutant S1 (old script): `repro-check.sh` ends with "differs under a hostile Go environment" and status 1. With the fix: REPRODUCIBLE. What the check covers: one target (the first of the list, linux/amd64 by default), exported variables; the hostile environment file is ignored by design because `GOENV=off` is set after it. |
| P7R-07 a release says which release it is | **Fixed** | `RELEASE` is in the signed sums; `release verify --binary RELEASE` shows it; the "newest release" limit is in "not proved"; `release.sh` refuses a dirty tree, a missing tag, a HEAD that is not the tag. Mutants S2 (dirty check off), S3 (HEAD vs tag off), S4 (untracked files not listed): each fails 2 of the 37 package checks. Residual: P7F-04. |
| P7R-08 holder not reached is not holder without file | **Fixed** | `FetchFirst` tells the two cases; the 404 text is compared with `FetchNoneMessage`; the internal API passes `CallError` messages through unchanged (`answerError`, `peerText` keeps a 50-character text), so the wire form matches. Mutants F1 (unreached never set), F2 (message not compared), F3 (`fetchMissing` never reports unreached), F4 (workTarget ignores unreached), F5 (work ignores unreached): all killed. Residual: P7F-02. |
| P7R-09 verifier reads restore-acknowledged | **Fixed** | The strict parser accepts exactly the publisher's nine lines (checked against `announceRestore`; the reason is printable ASCII, at most 200, as the publisher's `AcceptToken` guarantees). Notes in aggregate, bundle verify, monitor; remark in the failures. Mutants P2, P4, P10 (notes), P1b (rollback remark), P7b (hash check), P8 (generation from 1), P9 (extra line refused): killed. Survived: P1, P5 (see P7F-03). |
| P7R-10 orphan removal races a target being created | **Fixed** | Second read of the targets after the holders were asked. Mutants O1 (second list not used) and O3 (whole block off): killed. Survived: O2 (read error keeps the removal), see P7F-03. |
| P7R-11 copy before restore | **Fixed** | Temporary file, sync, rename, never over an earlier copy. Mutants K1 (existing copy not looked at), K3 (copy truncated), K2 (write error ignored): killed. The directory and file syncs cannot be tested and were only read. |

## 3. New findings

Severities: critical 0, high 0, medium 1, low 5, note 1.

### P7F-01 (medium): a rotation that a holder missed is never given again; the alert is wiped and its text is false

Place: `kingres/control-plane/internal/ledgeranchor/credsync.go` lines 105 to 123 (`lacking`), 156 to 158 (`s.credProblems = problems`); `rest.go` `putCreds` (the text "They are given again every minute").

Scenario. A target has a credentials file on three servers. The operator sends new credentials (a rotation after a leak). The leader writes the new file and calls `Distribute`; holder h2 is down, so `putCreds` stores the problem "could not be given to every holder ... They are given again every minute". Holder h2 still has the old file, so `credentials/list` does not report it as lacking: `jobCredentials` gives files only to holders that lack one, and gives nothing here. At the end of its first pass the job replaces `credProblems` with its own map, so the alert is gone within 60 s. The old, rotated-out file stays on h2 until the next change. After a failover to h2 the new leader anchors with the old credentials: the records fail (or, if the old key is not yet revoked, anchor with it), and the operator has no alert that says why.

Reproduced: yes, with a probe test (target, `Distribute` failing, `UpdateTarget` with new credentials, then `jobCredentials` with a holder report that lists nothing as lacking): the problem list is empty after the job and `Distribute` was not called again.

A fix must achieve: the report compares more than presence (a digest or revision of the file, or the target revision that the file was written for), the job gives a file to a holder whose copy differs, and the problem of a failed distribution is not wiped by a pass that did not give the file again.

### P7F-02 (low): a failed install of a fetched file is taken for "every holder has none"

Place: `credsync.go` lines 75 to 79 (`fetchMissing`, `return false, false` after a failed `Install`), `jobs.go` `workTarget`.

A holder gave the file, but `Creds.Install` failed here (disk full, an I/O error, a temporary permission problem). `fetchMissing` returns `(false, false)`, so `workTarget` keeps the no-credentials error and fails the records of the target for good (`target_unbuildable`, class permanent), the same defect that P7R-08 removed for unreached holders. The job's text says "no other holder could give it: enter the credentials again", which is not what happened. Not run (needs a failing disk); read only.

A fix must achieve: an install error other than a file that does not decode is the retryable class, like an unreached holder.

### P7F-03 (low): guards and branches without a test, and one branch that cannot be reached

Place and mutants:

- `credsync.go` line 144, `orphans = nil` when the second read of the targets fails (mutant O2 survives): without it the removal goes on with the stale list, which is the race P7R-10 closes.
- `cli_aggregate.go` line 317 (the same-size fork failure of `aggregate --previous` names the restore event; mutant P1 survives; the rollback line, P1b, is tested).
- `cli_restore.go` `forkRemark` (the event is named only for a fork, not for a damaged tile; mutant P5 survives).
- `cli_bundle_epoch.go`, the branch that prints "and it is leaf N of the signed log of epoch e+1 in the bundle as well": it cannot be reached. `proofEpochs` tries the newest epoch first, so a block that is also in epoch e+1 is found there and never reaches the check; a block that reaches it has already failed in e+1, so `blockNotInLog` always fails. The failing branch is correct and tested; the success note is dead code and untested. No wrong verdict follows (my probe verifies a genuine shared prefix).
- The test of the unwritable directory (M23, mutant K2) skips on Windows and as root, so the guard is covered only by Linux runs of the package.
- Mutant P11 (the note of `checkpoint` and `consistency` of the aggregate log, `cli_keyevents.go` line 121) was not run: my first edit did not apply to that file.

### P7F-04 (low): the verifier does not mark a signed set whose RELEASE says "commit: unknown"

Place: `kingres/verifier/internal/cli/cli_release.go` lines 150 to 158, `scripts/release.sh` (`--dry-run`).

`release.sh --dry-run` signs the files with the real release key from a dirty or untagged tree and records `commit: unknown`. The script says so in capitals, but a set that leaves the releaser's machine is indistinguishable from a release for an auditor except by reading the commit field: `release verify --binary RELEASE` prints "commit unknown" as an ordinary `ok` line and ends VERIFIED. Not run beyond reading and the package test, which covers the script side.

A fix must achieve: a finding or a "not proved" line when the signed commit or date is `unknown` ("this set was not made from a clean tagged checkout; it is not a release").

### P7F-05 (low): a stale error of an old term can hide the error of the new term

Place: `jobs.go` lines 178 to 186 (`setFatal`: first error wins, whatever its term).

A late worker of term 1 stores `ErrDeposed` after the pass of term 2 has started (the pass cleared the slot at its start). A worker of term 2 that then meets `ErrDeposed` (term 3 exists) finds the slot taken and stores nothing. At the end of the pass `takeFatal(2)` drops the term 1 error, so the pass of term 2 returns nil although its own worker saw the deposition. It heals at the next write of a worker, which is refused again; cost: one pass late. Not run; reasoned from the code.

A fix must achieve: `setFatal` replaces an error of another term, or keeps one slot per term.

### P7F-06 (low): a strictly parsed restore-acknowledged event ends the whole run for every command

Place: `kingres/verifier/internal/events/restore.go` (`restoreAcknowledged` returns a plain error), `events.go` `Follow`.

Before this commit the type was counted as "other" and never refused. Now any deviation in an event signed by an authorised key (a field a later publisher adds, a number above 2^40) ends the walk with an error, and with it `trust follow`, `aggregate`, `bundle verify` and `monitor` of that publication. The publisher of this commit writes exactly the strict form, and the strictness matches the other events, so this is a compatibility cost and not a wrong verdict. A fix must achieve: a decision written down (the design says "ends the run"), and a version marker in the event if a later field is expected.

### P7F-07 (note): the event of a second restore may be suppressed when the record starts again at generation 0

Place: `kingres/control-plane/internal/ledgerround/restore.go` `announceRestore` (subject `generation:<n>`), `ledgersign/publish.go` `AppendEvent` (an existing event of the same subject is returned, not written again).

P7R-11 itself says that a record that was lost and made again from the database starts at generation 0 and reaches the same generation number. If the first event of that generation is in the database that the record was made from, the next acceptance finds it, writes nothing, and the second cut is never announced, although the copy of the old record is kept under a suffix. Not run; the restored database may or may not hold the old event, so this depends on the order of events. A fix must achieve: a subject that includes the sum of the state replaced (published-state-sha256), so that two cuts of one number have two events.

## 4. Mutants

Control plane (build, then the unit packages; PostgreSQL SQL and the Linux-only tests through the integration script).

| Id | Change | Result |
|---|---|---|
| T1 | `takeFatal`: the term is not compared | killed (2) |
| F1 | `FetchFirst`: an unreached holder is not counted | killed (1) |
| F2 | `fetchAnswerOf`: the 404 text is not compared | killed (1) |
| F3 | `fetchMissing`: unreached always false | killed (2) |
| F4 | `workTarget`: unreached ignored | killed (1) |
| F5 | `work`: credsUnreached branch removed | killed (2) |
| O1 | orphans: the second list of targets not used | killed (1) |
| O2 | orphans: not cleared when the second read fails | **survived** |
| O3 | orphans: the whole second-read block off | killed (1) |
| K1 | restore copy: an existing copy is not looked at | killed (1) |
| K2 | restore copy: write error ignored (PostgreSQL install, Linux, non-root) | killed; survives on Windows because the test skips there |
| K3 | restore copy: half the bytes written | killed (2) |
| M05c | PostgreSQL `Apply`: revision guard `true OR` | killed |
| M06b2 | PostgreSQL `Failures`: the newer-anchor clause off | killed |
| M36b, M36bp, M36c, M36cu, M36d, M36du, M36s, M36r | each of the eight extra forms of a secret removed from the redactor | all killed |
| M03b | `beginTarget`: no guard | killed |
| M14 | credentials job not in `Jobs()` | killed |
| M15 | orphans: file of an existing target also removed | killed |
| M04b | `bindsWORM`: object key not compared | killed (3) |

Verifier.

| Id | Change | Result |
|---|---|---|
| R06 | `release verify`: SHA-256 of the binary not compared | killed (2) |
| V2 | `blockNotInLog`: the path is not verified | killed (1) |
| V5 | aggregate leg of a proof: no `--accept-epoch-closed` | killed (1) |
| E06 | frozen log not compared with the event | killed (1) |
| E09 | a closed log that continues not refused | killed (1) |
| E14 | the note of an unused acceptance changed | killed (1) |
| P1 | `aggregate --previous`: same-size fork without the restore remark | **survived** |
| P1b | the rollback line without the remark | killed (1) |
| P2 | `aggregate`: restore notes dropped | killed (1) |
| P4 | `bundle verify`: restore notes dropped | killed (1) |
| P5 | `forkRemark`: step not tested | **survived** |
| P7b | `database-state-sha256` not checked | killed (1) |
| P8 | generation from 1 not enforced | killed (1) |
| P9 | an extra line of the event not refused | killed (1) |
| P10 | `bundle verify` notes dropped (second form) | killed (1) |
| P3, P7, P11 | not run properly (a build error, a wrong package, an edit that did not apply); P7 is equivalent in the events package alone and killed through the command tests (P7b) | not counted |

Scripts (WSL, packaging test of 37 checks, and `repro-check.sh`).

| Id | Change | Result |
|---|---|---|
| S1 | `build-one.sh`: back to the old environment handling | killed by `repro-check.sh` (hostile build differs) |
| S2 | `release.sh`: a dirty tree is not a problem | killed (2 checks fail) |
| S3 | `release.sh`: HEAD need not be the tag | killed (2 checks fail) |
| S4 | `release.sh`: untracked files not listed | killed (2 checks fail) |

## 5. What was only read

- The cluster tests of failover and the real network path of the credentials fetch (a holder that is down); the wire form of the 404 was checked in the code of the internal API, not on a cluster.
- The directory and file `fsync` of the restore copy (untestable here).
- The builds for `linux/arm64`, `darwin/arm64` and `windows/amd64` (only `linux/amd64` was compared by the reproduction check and the package test).
- The fixtures of the epoch tests are built from documents, as in the earlier review; no closed epoch of a real cluster exists.
