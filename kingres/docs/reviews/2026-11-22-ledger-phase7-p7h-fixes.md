<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
# Review of the P7H fixes (commit f2b351c426), 2026-11-22

## Scope

Commit f2b351c426 on branch p7f-fix: fixes P7H-01 to P7H-06 of the review ledger-phase7-p7g-fixes, section 17.2 of the phase 7 design.
Files read: creds.go, credsync.go, rest.go, service.go, p7h_test.go, the wiring in cmd/kingresd/ledger_anchor.go, release.sh.
The Go tests of ledgeranchor, ledgerstate, ledgerround, internalapi and cmd/kingresd pass on Windows at the commit.
No finding is critical or high. One medium and two low findings (P7I-01 to P7I-03) and two notes.

## Verdicts

- P7H-01 (plain file above generation 0, jump bound): fixed for the cases it names. Install refuses a plain file above 0 on a sealing server, Digest demotes a planted plain file, Install and judgeCopies bound the jump to 64, the floor ignores far holders, exhaustion names the way out. Holes: P7I-01 (first fetch bypasses the bound), P7I-02 (a plain leader can never reach a sealing holder), P7I-03 (the absolute bound blocks a holder that is really far behind).
- P7H-02 (stale note cleared): fixed. Cleared in the gate (ensureCurrent) and in the job when no newer holder is found and the holders were read. A pass that could not read the holders leaves it. Correct.
- P7H-03 (24-hour alert, 503 on partial view): fixed as designed. The alert is raised in both paths and expires. The 503 applies only when the row already had credentials. The holder list is read from kingres.node (group 0, is_active), so a removed server is not listed and does not block; a server that is down but still active does block until it returns or is removed, which the design accepts (note N1).
- P7H-04 (per-target locks): correct, no deadlock found. Lock order is always service rotation lock, then store lock (Rotate), and the store never calls the service. Install takes only the store lock; Digest takes none, so Rotate and Install do not re-enter. The service map mutex is released before the target mutex is taken. The rotation lock is held across the network calls (Holders, Distribute), bounded by the peer client timeout and the request context, so a slow holder delays other changes of the same target only, for at most that time (note N2).
- P7H-05 (release.sh): fixed, the portable form is used and the packaging test checks it.
- P7H-06 (30-minute bound pinned): fixed. Mutants on the comparison and on the constant are killed.

## New findings

### P7I-01 (low) the first file is not bounded

creds.go Install, the check `if cur, cerr := s.Digest(id); cerr == nil` (about line 715). On a server with no file for the target (a new holder, a replaced disk) Digest fails, so neither the jump bound nor the order check runs. The same holds when the local file cannot be decoded.
Scenario (run as a probe): a sealed file of generation 2^40-1 installed into an empty sealed store succeeds; on a server that does not seal, a plain file of that generation also installs. The server then sits at the top of the range: one more rotation reaches the bound and then ErrGenerationExhausted, and every other holder refuses its files by the jump rule. This needs a damaged or foreign file on a holder, so the likelihood is low, but the P7H-01 claim "no jump is taken" is not true for the first file.
Fix: when there is no local file, bound the incoming generation (for a first file, at most MaxGenerationJump above 0, or relative to the generations the leader sees in the holder report), and refuse a plain file above 0 on every server. Document the choice.

### P7I-02 (medium) a mixed cluster (some servers seal, some do not) cannot distribute above generation 0

creds.go Install (ErrPlainFile) and Digest demotion. The leader hands over its raw file. A leader without a passphrase file holds plain files of generation 1 and above after any rotation; a sealing holder refuses every one of them (probe: plain generation 3 into a sealed store gives ErrPlainFile). Before the fix the holder re-sealed it. During a rollout of the passphrase file, or in a cluster where one server was left without it, the credentials of a target can never reach the sealing holders, the alert is only "could not be given to every holder", and the cause is not named. The same applies to a server that has just got a passphrase file: its existing plain files above 0 count as generation 0 in Digest, so it believes it is behind and, sealing, cannot take the plain file from the others either.
Fix: name the cause in the alert ("holder X seals its files and this server does not"); on start-up of a server that seals, re-seal its own plain files (keeping the generation, since the file is its own) so Digest does not demote them; or document that all holders must seal or none must.

### P7I-03 (low) the absolute jump bound of 64 blocks a holder that is really far behind, and the floor ignores holders when the leader has no file

creds.go Install, credsync.go generationFloor.
Scenario A: a holder was down (or restored from an old backup) while the cluster rotated more than 64 times. The leader hands over its file: Install returns ErrGenerationJump for it, forever. The only way out is to remove the file on that holder; the alert for this case is the generic distribution failure, not the text written for the "jump" class (which is shown only on the other side of the comparison).
Scenario B: the leader has no file (disk replaced) and an operator changes the credentials. generationFloor uses own = 0, so holders at generation 100 are ignored (above 0+64), the leader writes generation 1, the holders refuse it, and the next job pass reports the holders as a "jump" and tells the operator to remove the file on the holders, which are the healthy copies. Recoverable but misleading.
Fix: in B, fetch first (fetchMissing) or refuse the change with a clear message when the leader has no file but holders do; in A, make the refusal text name the remedy, and consider an explicit operator override instead of a fixed 64.

## Notes (no action required)

- N1: the 503 refusal is as documented. A server that is down but still active in kingres.node blocks changes of existing credentials until it returns or is removed from the cluster. A removed server (is_active false) is not listed. Consider an operator override for an emergency (a revoked key cannot be rotated meanwhile).
- N2: the rotation lock is held across Holders and Distribute. Bounded by the peer client timeout, so no deadlock, but a second change of the same target waits for the first. Acceptable.
- The way out is reachable: removing the file `anchor/<id>.json` on the holder works with the daemon running or not, and on the leader the exhaustion message names it. When the leader itself is poisoned below the bound (not exhausted), holders refuse its file by the jump rule and only the generic distribution alert is shown; the documented "a rotation here always works" is true locally but the holders still refuse it (same cause as P7I-03 A).

## Mutants run (all in ledgeranchor, restored after each run)

| Id | Change | Result |
|----|--------|--------|
| M1 | plain-file refusal in Install removed | killed (2 tests) |
| M2 | jump check in Install removed | killed |
| M3 | Digest demotion of a planted plain file removed | killed (2 tests) |
| M4 | exhausted-generation check removed | killed |
| M5 | store lock in Rotate removed | killed (concurrent rotations) |
| M6 | jump class in judgeCopies removed | killed |
| M7 | jump bound in the floor removed | killed |
| M8a | clearing of the wait in the gate removed | killed (2 tests) |
| M8b | clearing of the wait in the job removed | killed |
| M9 | alert of a replaced file made a no-op | killed (gate and job) |
| M10 | refusal of a partial change removed | killed (2 tests) |
| M11 | refusal on unreached holders removed (error only kept) | killed |
| M12 | service rotation lock removed | killed |
| M14 | stale limit comparison changed from >= to > | killed |
| M15 | stale limit changed from 30 to 60 minutes | killed |

Not run: the release.sh form (checked by the packaging shell test, which needs bash and was not run here). A probe test (first fetch with a high generation, plain generation 3 into a sealing store) confirmed P7I-01 and P7I-02 and was deleted afterwards. The worktree was removed and the main tree was not touched.
